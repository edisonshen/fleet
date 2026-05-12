"""dispatch.py tests: prompt assembly + subprocess argv assertions.

The fleet binary is never invoked for real here; tests mock subprocess.run
to assert the exact argv we'd send and to drive return-code paths.
"""
from __future__ import annotations

import os
import subprocess
from unittest.mock import patch

import pytest

import dispatch
import parse


def _make_task(slug: str = "fix-thing-aaaa") -> parse.Task:
    return parse.Task(
        slug=slug,
        status="ready",
        priority="P1",
        spec="Fix the thing.",
        acceptance="Thing is fixed.",
        notes="",
    )


def test_build_worker_prompt_contains_required_sections() -> None:
    t = _make_task()
    out = dispatch.build_worker_prompt(
        t, project="fleet",
        standards_md="# Standards\n\n## Testing\n- TDD only.\n",
        learnings_text="WHEN  AUTHOR  TAG  TASK  BODY\n2026-05-06T10:00:00Z  agent:abcdef01  testing  -  use t.TempDir\n",
    )
    # Required structural sections.
    assert f"You are a Fleet worker for task: {t.slug}" in out
    assert "Project: fleet" in out
    assert "## Task" in out
    assert "Fix the thing." in out
    assert "## Acceptance" in out
    assert "Thing is fixed." in out
    assert "## Standards (the bar — non-negotiable)" in out
    assert "TDD only." in out
    assert "## Relevant prior learnings" in out
    assert "use t.TempDir" in out
    # Workflow tail mentions every required phase.
    for phase in ("branch", "tdd-red", "tdd-green", "tdd-refactor",
                  "review-claude", "review-codex", "push", "done"):
        assert f"--phase {phase}" in out
    # Branch derivation.
    assert f"git checkout -b worker/{t.slug}" in out


def test_build_worker_prompt_omits_learnings_section_when_empty() -> None:
    t = _make_task()
    out = dispatch.build_worker_prompt(
        t, project="fleet",
        standards_md="# Standards",
        learnings_text="",
    )
    assert "## Relevant prior learnings" not in out


def test_build_worker_prompt_omits_learnings_section_on_no_learnings_message() -> None:
    """`fleet learnings list` emits 'no learnings (run ...)' on empty."""
    t = _make_task()
    out = dispatch.build_worker_prompt(
        t, project="fleet",
        standards_md="# Standards",
        learnings_text="WHEN  AUTHOR  TAG  TASK  BODY\nno learnings (run `fleet learnings add` to record one)",
    )
    assert "## Relevant prior learnings" not in out


def test_build_worker_prompt_truncates_long_learning_rows() -> None:
    t = _make_task()
    long_body = "x" * 5000
    out = dispatch.build_worker_prompt(
        t, project="fleet",
        standards_md="# Standards",
        learnings_text=f"WHEN  AUTHOR  TAG  TASK  BODY\n2026-05-06T10:00:00Z  agent:abcdef01  testing  -  {long_body}\n",
    )
    # Truncation marker present; raw 5000 chars are not.
    assert "…" in out
    assert "x" * 5000 not in out


def test_build_worker_prompt_oversized_raises() -> None:
    t = _make_task()
    huge_standards = "# Standards\n\n" + ("x" * (20 * 1024))
    with pytest.raises(dispatch.PromptTooLargeError):
        dispatch.build_worker_prompt(
            t, project="fleet",
            standards_md=huge_standards,
            learnings_text="",
        )


def test_build_worker_prompt_handles_empty_spec_and_acceptance() -> None:
    """Operator may add a task with no spec yet — prompt still renders."""
    t = parse.Task(slug="empty-spec-aaaa", status="ready", priority="P2")
    out = dispatch.build_worker_prompt(
        t, project="fleet",
        standards_md="# Standards",
        learnings_text="",
    )
    assert "spec pending" in out
    assert "acceptance pending" in out


def test_build_worker_prompt_custom_branch_and_workers_dir() -> None:
    t = _make_task()
    out = dispatch.build_worker_prompt(
        t, project="fleet",
        standards_md="# Standards",
        learnings_text="",
        branch="feature/custom",
        workers_dir="/tmp/custom",
    )
    assert "Branch: feature/custom" in out
    assert "git checkout -b feature/custom" in out
    assert "State file:  /tmp/custom/state.json" in out


def test_build_worker_prompt_worktree_pre_created_skips_branch_create() -> None:
    """Codex iter-1 [P1] regress: in cap > 1 mode the coord ran
    `git worktree add -b <branch>` already; the prompt must NOT tell
    the worker to `git checkout -b <branch>` (it would fatal "branch
    already exists"). Verify mode."""
    t = _make_task()
    out = dispatch.build_worker_prompt(
        t, project="fleet",
        standards_md="# Standards",
        learnings_text="",
        branch="worker/alpha-1234",
        worktree_pre_created=True,
    )
    # The branch-create command MUST NOT appear under cap>1 mode.
    assert "git checkout -b worker/alpha-1234" not in out
    # The worker is told to verify the prepared worktree branch.
    assert "git rev-parse --abbrev-ref HEAD" in out
    assert "worker/alpha-1234" in out


def test_build_worker_prompt_default_keeps_branch_create() -> None:
    """Single-worker mode (the default) MUST emit the original
    `git checkout -b <branch>` step — worktree-mode is the override,
    not the new default. Byte-identical regression guard."""
    t = _make_task()
    out = dispatch.build_worker_prompt(
        t, project="fleet",
        standards_md="# Standards",
        learnings_text="",
    )
    assert "git checkout -b worker/" in out
    assert "git rev-parse --abbrev-ref HEAD" not in out


def test_build_worker_prompt_contains_post_completion_contract() -> None:
    """Subagent lifecycle hardening: every dispatched prompt carries a
    'Post-completion contract' section telling the worker that emitting
    the §7 return block ends the dispatch. Without this language,
    workers have been observed opening bonus PRs / amending branches /
    expanding scope (CLAUDE.md §8 violation). The contract pins the
    boundary explicitly so the regression case can't recur from
    ambiguous prompt language."""
    t = _make_task()
    out = dispatch.build_worker_prompt(
        t, project="fleet",
        standards_md="# Standards",
        learnings_text="",
    )
    # Heading appears so the worker can find the section by scrolling.
    assert "Post-completion contract" in out
    # Specific constraints — the wording matters because subagents
    # parse the prompt for "may NOT" directives.
    assert "may NOT" in out
    assert "open additional PRs" in out
    # Pointer to the right channel for adjacent fixes the worker
    # noticed but should not act on.
    assert "fleet tasks add" in out
    assert "P3" in out


# ---------- mint_agent_id (issue #84 Phase A) ----------


def test_mint_agent_id_returns_8hex() -> None:
    """mint_agent_id replaces the `fleet dispatch` stdout-parse path.

    Skill mints the token itself before emitting the DISPATCH block so
    the same id can be recorded in tasks.md (note "dispatched as agent
    <id>") and in the supervisor slug→agent_id map BEFORE the coord
    agent invokes the Agent tool. If the coord crashes between emit
    and Agent call, the next tick still has the breadcrumb."""
    out = dispatch.mint_agent_id()
    assert dispatch._AGENT_ID_FULL_RE.fullmatch(out), (
        f"mint_agent_id must return 8-hex, got {out!r}"
    )


def test_mint_agent_id_returns_distinct_tokens() -> None:
    """Two calls back-to-back must not collide. secrets.token_hex(4)
    has 32 bits of entropy — birthday collision after ~65k workers,
    which overflows project lifetimes."""
    seen = {dispatch.mint_agent_id() for _ in range(100)}
    assert len(seen) == 100, "mint_agent_id collision in 100 draws"


# ---------- format_dispatch_instruction (issue #84 Phase A) ----------


def test_format_dispatch_instruction_shape() -> None:
    """The DISPATCH block is the contract between the Python skill and
    the coord agent (Claude). SKILL.md's "Worker dispatch protocol"
    section pins the same format — drift here breaks the coord's
    parser-by-reasoning."""
    out = dispatch.format_dispatch_instruction(
        agent_id="abcdef01",
        slug="ready-aaaa",
        prompt_file="/tmp/inbox/abcdef01.md",
    )
    lines = out.splitlines()
    assert lines[0] == "DISPATCH: ready-aaaa"
    # Block fields must appear in order with exact spacing.
    assert lines[1] == "  agent_id: abcdef01"
    assert lines[2] == "  description: fleet worker ready-aaaa"
    assert lines[3] == "  prompt_file: /tmp/inbox/abcdef01.md"
    assert lines[4] == "  run_in_background: true"
    assert lines[5] == "  subagent_type: general-purpose"
    assert lines[6] == "END_DISPATCH"
    assert len(lines) == 7


def test_format_dispatch_instruction_custom_description() -> None:
    out = dispatch.format_dispatch_instruction(
        agent_id="abcdef01",
        slug="ready-aaaa",
        prompt_file="/tmp/inbox/abcdef01.md",
        description="custom worker label",
    )
    assert "  description: custom worker label" in out
    # Default isn't substituted when an explicit description is set.
    assert "fleet worker ready-aaaa" not in out


def test_format_dispatch_instruction_rejects_invalid_agent_id() -> None:
    """A malformed agent_id in the stream the coord parses would let
    arbitrary content slip into the Agent tool's parameters. Reject
    at format time so the skill never emits one."""
    with pytest.raises(ValueError):
        dispatch.format_dispatch_instruction(
            agent_id="not-hex",
            slug="ready-aaaa",
            prompt_file="/tmp/inbox/x.md",
        )
    with pytest.raises(ValueError):
        dispatch.format_dispatch_instruction(
            agent_id="abcdef01extra",
            slug="ready-aaaa",
            prompt_file="/tmp/inbox/x.md",
        )


def test_format_dispatch_instruction_rejects_empty_slug_or_path() -> None:
    with pytest.raises(ValueError):
        dispatch.format_dispatch_instruction(
            agent_id="abcdef01", slug="", prompt_file="/x",
        )
    with pytest.raises(ValueError):
        dispatch.format_dispatch_instruction(
            agent_id="abcdef01", slug="ready-aaaa", prompt_file="",
        )


# ---------- dispatch.py no longer shells out for worker dispatch ----------


def test_dispatch_module_no_longer_exposes_dispatch_worker() -> None:
    """Issue #84 Phase A: dispatch_worker + _extract_agent_id were
    removed. Anything that imports them must be updated. Pin the
    surface so a stray re-add doesn't silently regress the
    no-subprocess invariant."""
    assert not hasattr(dispatch, "dispatch_worker"), (
        "dispatch_worker was removed — workers spawn as Agent-tool "
        "subagents now (issue #84 Phase A); restore would re-introduce "
        "the `fleet dispatch` subprocess call."
    )
    assert not hasattr(dispatch, "_extract_agent_id"), (
        "_extract_agent_id was removed (was used to parse `fleet "
        "dispatch` stdout). Skill mints its own agent_ids now."
    )


# ---------- inbox stub ----------


def test_write_worker_inbox_atomic_and_under_fleet_home(tmp_path) -> None:
    target = dispatch.write_worker_inbox(
        "abcdef01", "hello worker\n", fleet_home=str(tmp_path),
    )
    expected = tmp_path / "inbox" / "abcdef01.md"
    assert target == str(expected)
    assert expected.read_text() == "hello worker\n"
    # No tmp files left behind.
    assert not [p for p in (tmp_path / "inbox").iterdir() if ".tmp." in p.name]


def test_write_worker_inbox_appends_trailing_newline(tmp_path) -> None:
    target = dispatch.write_worker_inbox(
        "abcdef01", "no newline", fleet_home=str(tmp_path),
    )
    assert open(target).read().endswith("\n")


def test_write_worker_inbox_rejects_invalid_id(tmp_path) -> None:
    with pytest.raises(ValueError):
        dispatch.write_worker_inbox(
            "not-hex", "x", fleet_home=str(tmp_path),
        )
    with pytest.raises(ValueError):
        dispatch.write_worker_inbox(
            "abcdef01extra", "x", fleet_home=str(tmp_path),
        )


def test_write_worker_inbox_uses_env_fleet_home(monkeypatch, tmp_path) -> None:
    monkeypatch.setenv("FLEET_HOME", str(tmp_path))
    target = dispatch.write_worker_inbox("abcdef01", "via env\n")
    assert target.startswith(str(tmp_path))
    assert os.path.exists(target)


# ---------- fetch_standards / fetch_learnings ----------


def test_fetch_standards_returns_stdout_on_zero_exit() -> None:
    fake = subprocess.CompletedProcess(
        args=[], returncode=0, stdout="# Standards\n\n## Testing\n", stderr="",
    )
    with patch.object(dispatch.subprocess, "run", return_value=fake):
        out = dispatch.fetch_standards("fleet")
    assert "# Standards" in out


def test_fetch_standards_returns_empty_on_error() -> None:
    fake = subprocess.CompletedProcess(args=[], returncode=2, stdout="", stderr="boom")
    with patch.object(dispatch.subprocess, "run", return_value=fake):
        assert dispatch.fetch_standards("fleet") == ""


def test_fetch_learnings_passes_limit_arg() -> None:
    fake = subprocess.CompletedProcess(args=[], returncode=0, stdout="", stderr="")
    with patch.object(dispatch.subprocess, "run", return_value=fake) as m:
        dispatch.fetch_learnings("fleet", limit=5)
    args = m.call_args[0][0]
    assert "--limit" in args
    assert "5" in args
    assert "--project" in args
    assert "fleet" in args


def test_fetch_learnings_swallows_missing_binary() -> None:
    with patch.object(
        dispatch.subprocess, "run", side_effect=FileNotFoundError("no fleet"),
    ):
        assert dispatch.fetch_learnings("fleet") == ""


# ---------- build_reviewer_prompt (reviewer-subagent-arch) ----------


def test_build_reviewer_prompt_contains_review_iter_loop() -> None:
    """The reviewer subagent's prompt MUST instruct it to iterate
    /review until two consecutive clean passes. This is the structural
    invariant — without explicit "loop until clean" wording, a
    reviewer might run /review once, see [P0] findings, fix one, and
    declare done."""
    t = _make_task()
    out = dispatch.build_reviewer_prompt(t, project="fleet")
    # Section heading + role.
    assert "FLEET REVIEWER" in out.upper()
    assert f"task: {t.slug}" in out
    # /review iteration loop language.
    assert "/review" in out
    assert "TWO consecutive" in out or "two consecutive" in out
    assert "P0" in out and "P1" in out
    # Fix-commit pattern.
    assert "fix: review iter-" in out
    # Final terminal write.
    assert "--phase review-done" in out
    assert "--review-claude-status passed" in out
    assert "--review-claude-rounds" in out
    # /review is never skippable.
    assert "MANDATORY" in out or "mandatory" in out
    # Reviewer does NOT push or open PR.
    assert "do NOT push" in out.lower() or "Do NOT push" in out


def test_build_reviewer_prompt_documents_codex_skip_rate_limit() -> None:
    """Codex review's rate-limit handling is the documented
    operational escape hatch. The prompt must spell out: try once,
    wait 60s, retry, on persistent rate-limit MARK SKIPPED with
    --review-codex-skip-reason rate-limited. Anything else is rejected
    by the workers CLI."""
    t = _make_task()
    out = dispatch.build_reviewer_prompt(t, project="fleet")
    assert "codex" in out.lower()
    assert "rate-limit" in out.lower() or "rate limit" in out.lower()
    assert "60s" in out or "60 s" in out or "60 second" in out.lower()
    assert "--review-codex-skip-reason" in out
    assert "rate-limited" in out
    # Allowlist note: only rate-limited|unavailable are valid.
    assert "unavailable" in out


def test_build_reviewer_prompt_does_not_push_or_open_pr() -> None:
    """The reviewer hands the PR-opening job to the finisher. Its
    prompt must NOT mention `gh pr create` as a step it should run —
    that's a CLAUDE.md §7a "exit-before-push" gotcha if the reviewer
    thinks it should also push."""
    t = _make_task()
    out = dispatch.build_reviewer_prompt(t, project="fleet")
    # Hard prohibition.
    assert "Do NOT push" in out
    assert "Do NOT `gh pr create`" in out or "Do NOT gh pr create" in out


# ---------- build_finisher_prompt (reviewer-subagent-arch) ----------


def test_build_finisher_prompt_documents_push_and_pr() -> None:
    """The finisher's job is mechanical: push, open PR, mark done.
    The prompt must instruct exactly those steps with no review loop."""
    t = _make_task()
    out = dispatch.build_finisher_prompt(t, project="fleet")
    assert "FINISHER" in out.upper()
    assert "git push" in out
    assert "gh pr create" in out
    assert "--phase done --pr-url" in out
    # Hard prohibition: finisher does NOT run /review or codex.
    assert "Do NOT run /review" in out or "do NOT run /review" in out.lower()


def test_build_finisher_prompt_force_with_lease_only() -> None:
    """A finisher that --force-pushes can blow away co-located changes.
    The prompt must pin --force-with-lease as the only force variant
    allowed, and document plain --force as prohibited."""
    t = _make_task()
    out = dispatch.build_finisher_prompt(t, project="fleet")
    assert "--force-with-lease" in out
    # Explicitly prohibits plain force.
    assert "plain --force" in out.lower() or "NOT plain --force" in out


def test_build_finisher_prompt_blocks_on_push_failure() -> None:
    """The finisher cannot retry blindly on a push/PR creation
    failure. It must flip to phase=blocked + raise to operator."""
    t = _make_task()
    out = dispatch.build_finisher_prompt(t, project="fleet")
    assert "--phase blocked" in out
    assert "--reason" in out
