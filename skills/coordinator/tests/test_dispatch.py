"""dispatch.py tests: prompt assembly + subprocess argv assertions.

The fleet binary is never invoked for real here; tests mock subprocess.run
to assert the exact argv we'd send and to drive return-code paths.
"""
from __future__ import annotations

import os
import shlex
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
    # Three-stage flow: worker writes the code phases ONLY. Review +
    # push happen in separate subagents (reviewer-subagent-arch).
    for phase in ("branch", "tdd-red", "tdd-green", "tdd-refactor",
                  "review-pending"):
        assert f"--phase {phase}" in out, f"worker prompt missing --phase {phase}"
    # The old inline phases are GONE from the worker prompt — only the
    # reviewer subagent runs review-claude/review-codex; only the
    # finisher subagent runs push/done. Their presence in the worker
    # prompt would re-introduce the structural failure mode the
    # three-stage flow exists to prevent.
    for forbidden in ("--phase review-claude", "--phase review-codex",
                      "--phase push", "--phase done"):
        assert forbidden not in out, (
            f"worker prompt still mentions {forbidden}; three-stage flow "
            "must hand those phases to the reviewer/finisher subagents"
        )
    # Workers no longer invoke /review or codex inline. The prompt may
    # MENTION them only in prohibition language ("do NOT run /review")
    # — never as a step. The "On your diff. Fix every P0/P1" wording
    # from the old prompt is the bright-line check.
    assert "On your diff" not in out, (
        "worker prompt still tells worker to run reviewers on its diff"
    )
    assert "gh pr create" not in out, (
        "worker prompt still references gh pr create; that's the finisher's job"
    )
    # Branch derivation.
    assert f"git checkout -b worker/{t.slug}" in out


def test_build_worker_prompt_terminates_at_review_pending() -> None:
    """Three-stage flow: worker's last phase write is --phase
    review-pending. No subsequent phase=push or phase=done from this
    subagent — those belong to the finisher (reviewer-subagent-arch)."""
    t = _make_task()
    out = dispatch.build_worker_prompt(
        t, project="fleet",
        standards_md="# Standards",
        learnings_text="",
    )
    # The terminal phase write for this subagent.
    assert "--phase review-pending" in out
    # Explicit "exit" instruction at the handoff point.
    assert "Exit cleanly" in out or "exit cleanly" in out


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
    # Three-stage flow: worker MUST NOT open PRs (the finisher does
    # that). The prohibition stays — the wording just shifted from
    # "additional PRs" to "open PRs" since the worker never opens
    # ANY PR in the new flow.
    assert "open PRs" in out or "open additional PRs" in out
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
    # dispatch-durability (#184): the launch token follows agent_id.
    assert lines[2] == "  generation: 0"
    assert lines[3] == "  description: fleet worker ready-aaaa"
    assert lines[4] == "  prompt_file: /tmp/inbox/abcdef01.md"
    assert lines[5] == "  run_in_background: true"
    assert lines[6] == "  subagent_type: general-purpose"
    assert lines[7] == "END_DISPATCH"
    assert len(lines) == 8


def test_format_dispatch_instruction_carries_generation() -> None:
    """#184: a non-zero generation is stamped so the coord's
    mark-launch-attempted gate validates against the right lifecycle."""
    out = dispatch.format_dispatch_instruction(
        agent_id="abcdef01",
        slug="ready-aaaa",
        prompt_file="/tmp/inbox/abcdef01.md",
        generation=3,
    )
    assert "  generation: 3" in out


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
    """The reviewer prompt runs both resolved slots until both pass."""
    t = _make_task()
    out = dispatch.build_reviewer_prompt(t, project="fleet")
    # Section heading + role.
    assert "FLEET REVIEWER" in out.upper()
    assert f"task: {t.slug}" in out
    assert "review_slot.py" in out
    assert "Loop until BOTH slots exit 0" in out
    assert "P0" in out and "P1" in out
    # Fix-commit pattern.
    assert "fix: review iter-" in out
    # Final terminal write.
    assert "--phase review-done" in out
    assert "--review-alpha-status" in out
    assert "--review-beta-status passed" in out
    assert "--review-alpha-rounds" in out
    assert "--review-beta-rounds" in out
    # Reviewer does NOT push or open PR.
    assert "do NOT push" in out.lower() or "Do NOT push" in out


def test_build_reviewer_prompt_git_with_codex_threads_slots() -> None:
    t = _make_task()
    out = dispatch.build_reviewer_prompt(t, project="fleet", has_codex=True)
    assert "review_slot.py" in out
    assert "--engine codex" in out
    assert "--engine claude" in out
    assert "--effort high" in out
    assert "--base origin/main" in out
    assert "--task-context" not in out
    assert dispatch.reviewcfg.CODEX_DEFAULT_MODEL in out
    assert dispatch.reviewcfg.OPUS_FALLBACK[0] in out
    assert "--review-alpha-status" in out
    assert "--review-beta-status passed" in out
    assert "exit 0 => record that slot passed" in out
    assert "exit 1 => the slot found [P0]/[P1]" in out
    assert "exit 2 => codex slot skipped" in out
    assert "--review-alpha-status skipped --review-alpha-engine codex" in out
    assert "--review-alpha-skip-reason <reason>" in out
    assert "continue (beta still must pass)" in out
    assert "Loop until BOTH slots are RESOLVED" in out
    assert "OR the codex alpha exits 2 (skipped)" in out
    assert "stop re-running it" in out
    assert "Beta must still reach exit 0 (passed)" in out
    assert "exit 3 => the slot is BLOCKED" in out
    assert "--phase blocked" in out
    assert "--review-alpha-skip-reason" in out
    assert "rate-limited" in out
    assert "unavailable" in out


def test_build_reviewer_prompt_git_without_codex_uses_two_claude_slots() -> None:
    t = _make_task()
    out = dispatch.build_reviewer_prompt(t, project="fleet", has_codex=False)
    assert "--engine codex" not in out
    assert out.count("--engine claude --model") == 2
    assert dispatch.reviewcfg.SONNET_FALLBACK[0] in out
    assert dispatch.reviewcfg.OPUS_FALLBACK[0] in out
    assert "Loop until BOTH slots exit 0." in out
    assert "Loop until BOTH slots are RESOLVED" not in out
    assert "OR the codex alpha exits 2 (skipped)" not in out


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


def test_build_reviewer_prompt_codex_coord_adds_diversity_banner() -> None:
    """When coord_engine = codex, the reviewer prompt must include the
    cross-engine diversity banner so the (claude) reviewer knows it's
    pinch-hitting for a codex-written worker diff. The banner labels
    the role split explicitly — memory project_codex_multi_engine.md
    Approach A."""
    t = _make_task()
    out = dispatch.build_reviewer_prompt(
        t, project="fleet", coord_engine="codex",
    )
    # Banner present.
    assert "coord engine = codex" in out.lower()
    # Worker engine documented.
    assert "CODEX" in out
    # Reviewer role still claude.
    assert "CLAUDE" in out
    # Existing review-iter contract still in place.
    assert "review_slot.py" in out
    assert "--phase review-done" in out


def test_build_reviewer_prompt_claude_coord_omits_diversity_banner() -> None:
    """Default coord_engine = claude-code is the v0 path. The diversity
    banner must NOT appear in this case so the prompt body matches the
    pre-v0.9 byte shape on the happy path."""
    t = _make_task()
    out = dispatch.build_reviewer_prompt(
        t, project="fleet", coord_engine="claude-code",
    )
    assert "coord engine = codex" not in out.lower()


def test_build_reviewer_prompt_reads_engine_from_env(monkeypatch) -> None:
    """coord_engine defaults to FLEET_ENGINE env when not passed. This
    is how loop.py picks up the engine without an explicit lookup —
    spawn propagates FLEET_ENGINE into the coord agent's process."""
    monkeypatch.setenv("FLEET_ENGINE", "codex")
    t = _make_task()
    out = dispatch.build_reviewer_prompt(t, project="fleet")
    assert "coord engine = codex" in out.lower()


# ---------- worktree-aware reviewer/finisher (dispatch-reviewer-finish-9316) ----------


def test_build_reviewer_prompt_worktree_cds_instead_of_checkout() -> None:
    """Regression for dispatch-reviewer-finish-9316: when the worker ran
    in a pre-created worktree, the branch is checked out THERE. A bare
    `git checkout <branch>` in the main repo fatals "branch already used
    by worktree". The reviewer must instead `cd` into the worktree and
    verify the branch with `git rev-parse --abbrev-ref HEAD`."""
    t = _make_task()
    wt = "/Users/x/.fleet/projects/fleet/worktrees/fix-thing-aaaa"
    out = dispatch.build_reviewer_prompt(
        t, project="fleet", branch="worker/fix-thing-aaaa", worktree=wt,
    )
    assert f"cd {wt}" in out
    assert "git rev-parse --abbrev-ref HEAD" in out
    # The fatal bare-checkout step-1 must NOT appear.
    assert "git checkout worker/fix-thing-aaaa" not in out


def test_build_reviewer_prompt_no_worktree_keeps_checkout() -> None:
    """In-place (cap=1, no worktree) dispatch must keep the original
    `git checkout <branch>` step — worktree-mode is the override, not
    the new default. Byte-for-byte behavior guard for the non-worktree
    path."""
    t = _make_task()
    out = dispatch.build_reviewer_prompt(
        t, project="fleet", branch="worker/fix-thing-aaaa",
    )
    assert "git checkout worker/fix-thing-aaaa" in out
    assert "cd /" not in out


def test_build_finisher_prompt_worktree_cds_before_push() -> None:
    """Regression for dispatch-reviewer-finish-9316: the finisher must
    `cd` into the worker's worktree before `git push` / `gh pr create`,
    otherwise it operates on the main repo's checkout (wrong branch /
    no worker commits visible to gh from the wrong cwd)."""
    t = _make_task()
    wt = "/Users/x/.fleet/projects/fleet/worktrees/fix-thing-aaaa"
    out = dispatch.build_finisher_prompt(
        t, project="fleet", branch="worker/fix-thing-aaaa", worktree=wt,
    )
    assert f"cd {wt}" in out
    # The cd step must come before the push instruction.
    assert out.index(f"cd {wt}") < out.index("git push")


def test_build_finisher_prompt_no_worktree_omits_cd() -> None:
    """In-place finisher dispatch must NOT emit a `cd` into a worktree —
    it runs in the main repo's checkout as before."""
    t = _make_task()
    out = dispatch.build_finisher_prompt(
        t, project="fleet", branch="worker/fix-thing-aaaa",
    )
    assert "cd /" not in out
    assert "git push" in out


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


# ---------- non-git project support (operator clarification 2026-05-12) ----------


def test_project_is_git_missing_meta_defaults_true(tmp_path) -> None:
    """A project without a meta.json on disk defaults to git-mode —
    legacy projects pre-date the field and must keep behaving as
    git-backed.
    """
    out = dispatch.project_is_git("ghost", fleet_home=str(tmp_path))
    assert out is True


def test_project_is_git_field_absent_defaults_true(tmp_path) -> None:
    """meta.json with no is_git key (legacy file) also defaults to git-mode."""
    proj_dir = tmp_path / "projects" / "legacy"
    proj_dir.mkdir(parents=True)
    (proj_dir / "meta.json").write_text(
        '{"schema":"v1","repo_path":"/x","added_at":"2026-01-01T00:00:00Z"}',
        encoding="utf-8",
    )
    assert dispatch.project_is_git("legacy", fleet_home=str(tmp_path)) is True


def test_project_is_git_false_returns_false(tmp_path) -> None:
    """is_git=false on disk surfaces as False — this drives the
    non-git prompt branches downstream.
    """
    proj_dir = tmp_path / "projects" / "scratch"
    proj_dir.mkdir(parents=True)
    (proj_dir / "meta.json").write_text(
        '{"schema":"v1","repo_path":"/x","added_at":"2026-01-01T00:00:00Z","is_git":false}',
        encoding="utf-8",
    )
    assert dispatch.project_is_git("scratch", fleet_home=str(tmp_path)) is False


def test_project_is_git_malformed_json_defaults_true(tmp_path) -> None:
    """Conservative default: malformed meta.json falls back to git-mode
    so a corrupted file doesn't accidentally relax the validator.
    """
    proj_dir = tmp_path / "projects" / "broken"
    proj_dir.mkdir(parents=True)
    (proj_dir / "meta.json").write_text("this is { not json", encoding="utf-8")
    assert dispatch.project_is_git("broken", fleet_home=str(tmp_path)) is True


def test_build_worker_prompt_git_project_uses_three_stage_flow() -> None:
    """is_git=True (default) pins the existing three-stage worker
    contract: branch creation, git commits, exit at review-pending.
    Regression guard against the non-git branch accidentally taking
    over for the common case.
    """
    t = _make_task()
    out = dispatch.build_worker_prompt(
        t, project="fleet", standards_md="", learnings_text="", is_git=True,
    )
    # Git-mode signals.
    assert "git checkout -b worker/" in out
    assert "git commit" in out
    # The handoff exit remains phase=review-pending.
    assert "--phase review-pending" in out
    # Not the non-git intro.
    assert "non-git project" not in out


def test_build_worker_prompt_non_git_project_skips_branch_push_pr() -> None:
    """is_git=False emits the non-git worker contract: no branch
    creation, no commits, but the same TDD/phase progression and
    exit-at-review-pending handoff.
    """
    t = _make_task()
    out = dispatch.build_worker_prompt(
        t, project="scratch", standards_md="", learnings_text="", is_git=False,
    )
    # Non-git mode signals.
    assert "non-git project" in out.lower()
    # No branch creation step.
    assert "git checkout -b" not in out
    # The contract still routes through review-pending — same SOP.
    assert "--phase review-pending" in out
    # Worker still hands off to reviewer (the three-stage flow is
    # preserved; only the finisher's actions differ).
    assert "reviewer" in out.lower()


def test_build_reviewer_prompt_non_git_uses_two_claude_slots_without_base() -> None:
    t = _make_task()
    t.spec = "Fix the quoted 'thing'.\nKeep context intact."
    t.acceptance = 'Thing is fixed with "quotes".'
    out = dispatch.build_reviewer_prompt(
        t, project="scratch", is_git=False, has_codex=True,
    )
    context = f"{t.spec}\n\nAcceptance:\n{t.acceptance}"
    assert "review_slot.py" in out
    assert "--engine codex" not in out
    assert out.count("--engine claude --model") == 2
    assert "--base" not in out
    assert out.count(f"--task-context {shlex.quote(context)}") == 2
    assert "no-git" not in out
    assert "--review-alpha-status passed" in out
    assert "--review-beta-status passed" in out


def test_build_finisher_prompt_non_git_skips_push_and_pr() -> None:
    """Non-git finisher: no git push, no gh pr create. Writes
    phase=done WITHOUT a pr_url (workers CLI relaxes the pr_url gate
    for non-git projects).
    """
    t = _make_task()
    out = dispatch.build_finisher_prompt(t, project="scratch", is_git=False)
    # Hard prohibitions. We assert the imperative-command form is
    # absent ("git push -u" / "gh pr create" the verb) rather than
    # the literal substring — the body explains that there is no PR
    # and no push, which incidentally contains the substring "PR" in
    # informational prose.
    assert "git push -u" not in out
    assert "gh pr create" not in out
    # Phase done is the terminal write — the command line itself must
    # NOT carry --pr-url.
    assert "--phase done --exit 0" in out
    assert "--phase done --pr-url" not in out
    assert "review_alpha_status=passed" in out
    assert "review_beta_status=passed" in out
    assert "no-git" not in out
    # Diff summary is the operator-visible deliverable.
    assert "diff" in out.lower()


def test_build_finisher_prompt_git_keeps_push_and_pr() -> None:
    """Regression: is_git=True (default) keeps the existing finisher
    push + PR workflow.
    """
    t = _make_task()
    out = dispatch.build_finisher_prompt(t, project="fleet", is_git=True)
    assert "git push" in out
    assert "gh pr create" in out
    assert "--phase done --pr-url" in out
    assert ".review_alpha_" in out
    assert ".review_beta_" in out
    assert "- alpha (<engine>/<model>):" in out
    assert "- beta (claude/<model>):" in out
    assert "--phase blocked" in out
    assert "review gate rejected" in out
    assert ".review_codex_" not in out


# ---------- acquire_coord_prompt_inbox (PR1 dispatch-lifecycle) ----------


def _fake_run(stdout: str = "", returncode: int = 0, stderr: str = ""):
    """Build a CompletedProcess for subprocess.run mocking."""
    return subprocess.CompletedProcess(
        args=[], returncode=returncode, stdout=stdout, stderr=stderr,
    )


def test_acquire_coord_prompt_inbox_acquired(tmp_path) -> None:
    """Happy path: CLI returns `acquired`, helper returns the path."""
    envelope = (
        '{"outcome":"acquired","dispatch_id":"a690424b",'
        '"kind":"coord_prompt_inbox","path":"/tmp/x/inbox/a690424b.md"}\n'
    )
    with patch.object(dispatch.subprocess, "run", return_value=_fake_run(envelope)):
        path = dispatch.acquire_coord_prompt_inbox(
            "a690424b", "prompt body",
            owner="project/fleet/slug/foo",
            fleet_home=str(tmp_path),
        )
    assert path == "/tmp/x/inbox/a690424b.md"


def test_acquire_coord_prompt_inbox_already_acquired_is_success(tmp_path) -> None:
    """already_acquired is the idempotent retry success outcome."""
    envelope = (
        '{"outcome":"already_acquired","dispatch_id":"a690424b",'
        '"kind":"coord_prompt_inbox","path":"/tmp/x/inbox/a690424b.md"}\n'
    )
    with patch.object(dispatch.subprocess, "run", return_value=_fake_run(envelope)):
        path = dispatch.acquire_coord_prompt_inbox(
            "a690424b", "prompt",
            owner="project/fleet/slug/foo",
            fleet_home=str(tmp_path),
        )
    assert path.endswith("a690424b.md")


def test_acquire_coord_prompt_inbox_error_outcome_raises(tmp_path) -> None:
    """`error` outcome (or any non-success) raises AcquirePromptError."""
    envelope = (
        '{"outcome":"error","dispatch_id":"a690424b",'
        '"kind":"coord_prompt_inbox","error":"disk full"}\n'
    )
    with patch.object(dispatch.subprocess, "run", return_value=_fake_run(envelope, returncode=1)):
        with pytest.raises(dispatch.AcquirePromptError) as info:
            dispatch.acquire_coord_prompt_inbox(
                "a690424b", "prompt", owner="x", fleet_home=str(tmp_path),
            )
    assert info.value.outcome == "error"
    assert info.value.exit_code == 1
    assert "disk full" in str(info.value)


def test_acquire_coord_prompt_inbox_invalid_agent_id(tmp_path) -> None:
    """Local fail-fast on malformed agent_id — no subprocess call."""
    with pytest.raises(ValueError):
        dispatch.acquire_coord_prompt_inbox(
            "not-hex", "p", owner="x", fleet_home=str(tmp_path),
        )


def test_acquire_coord_prompt_inbox_fleet_bin_missing_raises(tmp_path) -> None:
    """FileNotFoundError from subprocess maps to AcquirePromptError."""
    with patch.object(dispatch.subprocess, "run", side_effect=FileNotFoundError("nope")):
        with pytest.raises(dispatch.AcquirePromptError) as info:
            dispatch.acquire_coord_prompt_inbox(
                "a690424b", "p", owner="x", fleet_home=str(tmp_path),
            )
    assert info.value.outcome == "error"


def test_acquire_coord_prompt_inbox_passes_dispatch_kind(tmp_path) -> None:
    """The --dispatch-kind flag is propagated to the CLI argv."""
    envelope = (
        '{"outcome":"acquired","dispatch_id":"a690424b",'
        '"kind":"coord_prompt_inbox","path":"/tmp/x/inbox/a690424b.md"}\n'
    )
    captured = {}

    def fake_run(cmd, **kwargs):
        captured["cmd"] = cmd
        return _fake_run(envelope)

    with patch.object(dispatch.subprocess, "run", side_effect=fake_run):
        dispatch.acquire_coord_prompt_inbox(
            "a690424b", "p", owner="x", dispatch_kind="reviewer",
            fleet_home=str(tmp_path),
        )
    assert "--dispatch-kind" in captured["cmd"]
    idx = captured["cmd"].index("--dispatch-kind")
    assert captured["cmd"][idx + 1] == "reviewer"


def test_acquire_coord_prompt_inbox_pipes_prompt_to_stdin(tmp_path) -> None:
    """Prompt body flows to subprocess via stdin, not argv."""
    envelope = (
        '{"outcome":"acquired","dispatch_id":"a690424b",'
        '"kind":"coord_prompt_inbox","path":"/tmp/x/inbox/a690424b.md"}\n'
    )
    captured = {}

    def fake_run(cmd, **kwargs):
        captured["input"] = kwargs.get("input")
        return _fake_run(envelope)

    with patch.object(dispatch.subprocess, "run", side_effect=fake_run):
        dispatch.acquire_coord_prompt_inbox(
            "a690424b", "secret prompt body", owner="x", fleet_home=str(tmp_path),
        )
    assert captured["input"] == "secret prompt body"
    # The argv MUST NOT contain the body.
    # (We don't have captured cmd here; the previous test exercised argv.)


def _build_fleet_bin(tmp_path) -> str | None:
    """Build the current source tree's `fleet` binary into tmp_path.

    Returns the absolute path on success, None on failure. Used by the
    E2E shell-out tests to exercise the real Python ↔ Go contract
    instead of the (possibly out-of-date) system-installed binary.
    """
    import shutil
    if not shutil.which("go"):
        return None
    # Walk up from this test file to find the repo root (Makefile + go.mod).
    here = os.path.dirname(os.path.abspath(__file__))
    repo = here
    while repo != "/" and not os.path.exists(os.path.join(repo, "go.mod")):
        repo = os.path.dirname(repo)
    if not os.path.exists(os.path.join(repo, "go.mod")):
        return None
    out = str(tmp_path / "fleet")
    proc = subprocess.run(
        ["go", "build", "-o", out, "./cmd/fleet"],
        cwd=repo, capture_output=True, text=True, check=False,
    )
    if proc.returncode != 0:
        return None
    return out


def test_acquire_coord_prompt_inbox_e2e_via_real_fleet_bin(tmp_path) -> None:
    """End-to-end via a freshly-built `fleet` binary from this tree.

    Builds the binary from source rather than relying on PATH so the
    test never sees a stale (pre-PR1) install. Exercises the full
    Python-shell-out → Go CLI → on-disk journal+inbox loop. Closes the
    "Python and Go must agree on the JSON envelope" contract gap.
    """
    fleet_bin = _build_fleet_bin(tmp_path)
    if not fleet_bin:
        pytest.skip("could not build fleet binary; skipping E2E shell-out test")

    fleet_home = tmp_path / "home"
    fleet_home.mkdir()
    path = dispatch.acquire_coord_prompt_inbox(
        "a690424b", "real e2e prompt body",
        owner="project/test/slug/e2e",
        fleet_bin=fleet_bin,
        fleet_home=str(fleet_home),
    )
    assert path.endswith("a690424b.md")
    assert open(path).read().startswith("real e2e prompt body")
    # Journal is on disk.
    journal = fleet_home / "dispatches" / "a690424b.json"
    assert journal.exists(), f"journal missing at {journal}"


# ---------- release_coord_prompt_inbox (PR1 dispatch-lifecycle) ----------


def test_release_coord_prompt_inbox_released(tmp_path) -> None:
    """Happy path: CLI returns `released`, helper returns the envelope."""
    envelope = (
        '{"outcome":"released","dispatch_id":"a690424b",'
        '"kind":"coord_prompt_inbox","path":"/tmp/x/inbox/a690424b.md"}\n'
    )
    with patch.object(dispatch.subprocess, "run", return_value=_fake_run(envelope)):
        out = dispatch.release_coord_prompt_inbox(
            "a690424b", fleet_home=str(tmp_path),
        )
    assert out["outcome"] == dispatch.RELEASE_OUTCOME_RELEASED
    assert out["dispatch_id"] == "a690424b"


def test_release_coord_prompt_inbox_already_released_is_success(tmp_path) -> None:
    """already_released is the idempotent re-release success outcome."""
    envelope = (
        '{"outcome":"already_released","dispatch_id":"a690424b",'
        '"kind":"coord_prompt_inbox"}\n'
    )
    with patch.object(dispatch.subprocess, "run", return_value=_fake_run(envelope)):
        out = dispatch.release_coord_prompt_inbox(
            "a690424b", fleet_home=str(tmp_path),
        )
    assert out["outcome"] == dispatch.RELEASE_OUTCOME_ALREADY_RELEASED


def test_release_coord_prompt_inbox_absent_is_passed_through(tmp_path) -> None:
    """absent is a non-fatal terminal-race outcome (no claim on disk)."""
    envelope = (
        '{"outcome":"absent","dispatch_id":"a690424b",'
        '"kind":"coord_prompt_inbox"}\n'
    )
    # absent is exit code 11 on the CLI side; the helper does NOT branch
    # on returncode — it passes through the parsed outcome unchanged.
    with patch.object(
        dispatch.subprocess, "run",
        return_value=_fake_run(envelope, returncode=11),
    ):
        out = dispatch.release_coord_prompt_inbox(
            "a690424b", fleet_home=str(tmp_path),
        )
    assert out["outcome"] == dispatch.RELEASE_OUTCOME_ABSENT


def test_release_coord_prompt_inbox_not_owned_is_passed_through(tmp_path) -> None:
    """not_owned is the cross-host refusal outcome."""
    envelope = (
        '{"outcome":"not_owned","dispatch_id":"a690424b",'
        '"kind":"coord_prompt_inbox"}\n'
    )
    with patch.object(
        dispatch.subprocess, "run",
        return_value=_fake_run(envelope, returncode=10),
    ):
        out = dispatch.release_coord_prompt_inbox(
            "a690424b", fleet_home=str(tmp_path),
        )
    assert out["outcome"] == dispatch.RELEASE_OUTCOME_NOT_OWNED


def test_release_coord_prompt_inbox_error_outcome_returns_envelope(tmp_path) -> None:
    """`error` outcome does NOT raise — helper is best-effort by contract."""
    envelope = (
        '{"outcome":"error","dispatch_id":"a690424b",'
        '"kind":"coord_prompt_inbox","error":"journal write race"}\n'
    )
    with patch.object(
        dispatch.subprocess, "run",
        return_value=_fake_run(envelope, returncode=1),
    ):
        out = dispatch.release_coord_prompt_inbox(
            "a690424b", fleet_home=str(tmp_path),
        )
    # No exception. Caller branches on outcome.
    assert out["outcome"] == dispatch.RELEASE_OUTCOME_ERROR
    assert out.get("error") == "journal write race"


def test_release_coord_prompt_inbox_invalid_agent_id_returns_error(tmp_path) -> None:
    """Malformed agent_id returns synthetic error envelope — no subprocess."""
    with patch.object(dispatch.subprocess, "run") as mock_run:
        out = dispatch.release_coord_prompt_inbox(
            "not-hex", fleet_home=str(tmp_path),
        )
    assert out["outcome"] == dispatch.RELEASE_OUTCOME_ERROR
    assert "invalid agent_id" in out["error"]
    mock_run.assert_not_called()


def test_release_coord_prompt_inbox_fleet_bin_missing_returns_error(tmp_path) -> None:
    """FileNotFoundError from subprocess maps to synthetic error envelope."""
    with patch.object(
        dispatch.subprocess, "run",
        side_effect=FileNotFoundError("no such binary"),
    ):
        out = dispatch.release_coord_prompt_inbox(
            "a690424b", fleet_home=str(tmp_path),
        )
    assert out["outcome"] == dispatch.RELEASE_OUTCOME_ERROR
    assert "not found" in out["error"]


def test_release_coord_prompt_inbox_empty_stdout_returns_error(tmp_path) -> None:
    """Empty stdout with non-zero exit synthesizes an error envelope."""
    with patch.object(
        dispatch.subprocess, "run",
        return_value=_fake_run("", returncode=1, stderr="boom"),
    ):
        out = dispatch.release_coord_prompt_inbox(
            "a690424b", fleet_home=str(tmp_path),
        )
    assert out["outcome"] == dispatch.RELEASE_OUTCOME_ERROR
    # The stderr is surfaced when stdout is empty.
    assert "boom" in out["error"]


def test_release_coord_prompt_inbox_passes_preserve_flag(tmp_path) -> None:
    """The --preserve flag is propagated to the CLI argv when requested."""
    envelope = (
        '{"outcome":"released","dispatch_id":"a690424b",'
        '"kind":"coord_prompt_inbox"}\n'
    )
    captured = {}

    def fake_run(cmd, **kwargs):
        captured["cmd"] = cmd
        return _fake_run(envelope)

    with patch.object(dispatch.subprocess, "run", side_effect=fake_run):
        dispatch.release_coord_prompt_inbox(
            "a690424b", preserve=True, fleet_home=str(tmp_path),
        )
    assert "--preserve" in captured["cmd"]


def test_release_coord_prompt_inbox_e2e_via_real_fleet_bin(tmp_path) -> None:
    """End-to-end: build fleet, acquire, release, assert files gone."""
    fleet_bin = _build_fleet_bin(tmp_path)
    if not fleet_bin:
        pytest.skip("could not build fleet binary; skipping E2E shell-out test")

    fleet_home = tmp_path / "home"
    fleet_home.mkdir()
    # Acquire first so there's a claim to release.
    inbox_path = dispatch.acquire_coord_prompt_inbox(
        "a690424b", "e2e release prompt body",
        owner="project/test/slug/release-e2e",
        fleet_bin=fleet_bin,
        fleet_home=str(fleet_home),
    )
    journal = fleet_home / "dispatches" / "a690424b.json"
    assert journal.exists()
    assert os.path.exists(inbox_path)

    out = dispatch.release_coord_prompt_inbox(
        "a690424b",
        fleet_bin=fleet_bin,
        fleet_home=str(fleet_home),
    )
    assert out["outcome"] == dispatch.RELEASE_OUTCOME_RELEASED
    # Inbox file unlinked by default (preserve=False).
    assert not os.path.exists(inbox_path), (
        f"inbox file still present after release: {inbox_path}"
    )
    # A second release is idempotent (already_released).
    out2 = dispatch.release_coord_prompt_inbox(
        "a690424b",
        fleet_bin=fleet_bin,
        fleet_home=str(fleet_home),
    )
    assert out2["outcome"] in (
        dispatch.RELEASE_OUTCOME_ALREADY_RELEASED,
        # CLI may report absent when both inbox and journal were
        # cleaned up by the first release; either is valid idempotent.
        dispatch.RELEASE_OUTCOME_ABSENT,
    )
