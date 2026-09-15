"""dispatch.py tests: prompt assembly + subprocess argv assertions.

The fleet binary is never invoked for real here; tests mock subprocess.run
to assert the exact argv we'd send and to drive return-code paths.
"""
from __future__ import annotations

import os
import re
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
        standards_md="# Standards\n\n## Testing\n- Scenario-first only.\n",
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
    assert "Scenario-first only." in out
    assert "## Relevant prior learnings" in out
    assert "use t.TempDir" in out
    # Three-stage flow: worker writes the code phases ONLY. Review +
    # push happen in separate subagents (reviewer-subagent-arch).
    for phase in ("branch", "spec-repro", "spec-encode", "verify",
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


@pytest.mark.parametrize("is_git", [True, False], ids=["git", "non-git"])
def test_build_worker_prompt_fits_cap_with_repo_standards(is_git: bool) -> None:
    """S11: the repo's own templates/standards.md + Fleet's project
    standards block (the ```markdown block in docs/TESTING.md, as a
    project override merges it) + a realistic ~2KB spec must render under
    _PROMPT_HARD_CAP_BYTES, or every real dispatch on this project raises
    PromptTooLargeError (task marked blocked)."""
    here = os.path.dirname(os.path.abspath(__file__))
    repo = here
    while repo != "/" and not os.path.exists(os.path.join(repo, "go.mod")):
        repo = os.path.dirname(repo)
    with open(os.path.join(repo, "templates", "standards.md"), encoding="utf-8") as f:
        standards = f.read()
    with open(os.path.join(repo, "docs", "TESTING.md"), encoding="utf-8") as f:
        testing_md = f.read()
    m = re.search(r"```markdown\n(## Sandbox\n.*?)```", testing_md, re.S)
    assert m, "docs/TESTING.md must open with Fleet's ```markdown ## Sandbox block"
    fleet_block = m.group(1)
    assert len(fleet_block.encode()) >= 800
    standards = standards + "\n" + fleet_block
    spec = ("WHEN the operator presses [a] on a project row THE TUI SHALL "
            "dispatch a coord and flash `dispatched <id>` in the status bar. ") * 12
    acceptance = ("- pane shows `fake claude: ready`\n"
                  "- agents/<id>.json has project=demo\n") * 8
    assert len((spec + acceptance).encode()) >= 2000
    t = parse.Task(slug="fix-thing-aaaa", status="ready", priority="P1",
                   spec=spec, acceptance=acceptance, notes="")
    out = dispatch.build_worker_prompt(
        t, project="fleet", standards_md=standards,
        learnings_text="", branch="worker/fix-thing-aaaa",
        workers_dir="~/.fleet/projects/fleet/workers/fix-thing-aaaa",
        is_git=is_git,
    )
    assert len(out.encode()) <= dispatch._PROMPT_HARD_CAP_BYTES


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
    assert "review_slot.py --both" in out
    assert "--alpha-engine codex" in out
    assert "--beta-engine claude" in out
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
    assert '"alpha": {"exit": N' in out
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
    assert "--alpha-engine codex" not in out
    assert "--alpha-engine claude" in out
    assert "--beta-engine claude" in out
    assert dispatch.reviewcfg.SONNET_FALLBACK[0] in out
    assert dispatch.reviewcfg.OPUS_FALLBACK[0] in out
    assert "Loop until BOTH slots exit 0." in out
    assert "Loop until BOTH slots are RESOLVED" not in out
    assert "OR the codex alpha exits 2 (skipped)" not in out


def test_build_reviewer_prompt_threads_review_effort() -> None:
    t = _make_task()
    out = dispatch.build_reviewer_prompt(
        t, project="fleet", has_codex=True, review_effort="medium",
    )
    assert "--effort medium" in out
    assert "--effort high" not in out


def test_build_reviewer_prompt_git_reruns_both_slots_after_fix() -> None:
    t = _make_task()
    out = dispatch.build_reviewer_prompt(t, project="fleet", has_codex=False)
    lower = out.lower()
    assert "re-run both slots from scratch" in lower
    assert "a fix changes the reviewed code" in lower
    assert "any earlier slot pass is stale" in lower
    assert "re-run that slot" not in lower
    assert "same final code" in lower
    assert "no fix commit applied after either slot's passing run" in lower
    assert "if any fix lands after a slot passed" in lower


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


def test_build_finisher_prompt_git_writes_push_phase_before_done() -> None:
    """Git finisher must trigger the review gate at phase=push before
    pushing remote code, then write phase=done only after the PR exists."""
    t = _make_task()
    out = dispatch.build_finisher_prompt(t, project="fleet", is_git=True)
    push_update = f"fleet workers update {t.slug} --project fleet --phase push"
    done_update = f"fleet workers update {t.slug} --project fleet --phase done --pr-url"

    assert push_update in out
    assert done_update in out
    assert out.index(push_update) < out.index("git push -u")
    assert out.index(push_update) < out.index(done_update)


def test_build_finisher_prompt_git_blocks_on_push_gate_rejection() -> None:
    """A phase=push review-gate rejection must block before push/PR."""
    t = _make_task()
    out = dispatch.build_finisher_prompt(t, project="fleet", is_git=True)
    assert "If this phase=push write is REJECTED by the review gate" in out
    assert "do NOT" in out and "push or open the PR" in out
    assert "finisher: review gate rejected at phase=push" in out


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
    creation, no commits, but the same spec-repro → encode → verify phase progression and
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


# ---------- verify gate ----------


VERIFY_GATE_LINE = (
    "If the review-pending write is rejected by the verify gate, your gates "
    "or counts are wrong; fix and re-record — never `--phase blocked` to bypass."
)


@pytest.mark.parametrize("is_git", [True, False])
def test_build_worker_prompt_verify_gate_rejection_line_precedes_handoff(is_git: bool) -> None:
    """The worker prompt carries exactly one verify-gate rejection line,
    after `--phase verify` and before the review-pending handoff, in both
    git and non-git modes."""
    t = _make_task()
    out = dispatch.build_worker_prompt(
        t, project="fleet", standards_md="", learnings_text="", is_git=is_git,
    )
    assert out.count(VERIFY_GATE_LINE) == 1
    proj = "--project fleet"
    assert out.index("--phase verify") < out.index(VERIFY_GATE_LINE)
    assert out.index(VERIFY_GATE_LINE) < out.index(
        f"fleet workers update {t.slug} {proj} --phase review-pending"
    )


@pytest.mark.parametrize("is_git", [True, False])
def test_build_finisher_prompt_unaffected_by_verify_gate(is_git: bool) -> None:
    """The finisher prompt never writes review-pending and does not carry
    the worker's verify-gate rejection line."""
    t = _make_task()
    out = dispatch.build_finisher_prompt(t, project="fleet", is_git=is_git)
    assert "--phase review-pending" not in out
    assert VERIFY_GATE_LINE not in out
    assert "verify gate" not in out.lower()
    if is_git:
        assert f"fleet workers update {t.slug} --project fleet --phase push" in out
    else:
        assert "--phase push" not in out


def test_build_reviewer_prompt_non_git_uses_two_claude_slots_without_base() -> None:
    t = _make_task()
    t.spec = "Fix the quoted 'thing'.\nKeep context intact."
    t.acceptance = 'Thing is fixed with "quotes".'
    out = dispatch.build_reviewer_prompt(
        t, project="scratch", is_git=False, has_codex=True,
    )
    assert "review_slot.py --both" in out
    assert "--alpha-engine codex" not in out
    assert "--alpha-engine claude" in out
    assert "--beta-engine claude" in out
    assert "--base" not in out
    # Spec + acceptance ride along in --task-context on the --both call (the
    # contract lens is appended after them; S3 pins that part).
    contexts = _task_contexts(out)
    assert len(contexts) == 1
    for ctx in contexts:
        assert ctx.startswith(f"{t.spec}\n\nAcceptance:\n{t.acceptance}")
    assert "no-git" not in out
    assert "--review-alpha-status passed" in out
    assert "--review-beta-status passed" in out


def test_build_reviewer_prompt_non_git_reruns_both_slots_after_fix() -> None:
    t = _make_task()
    out = dispatch.build_reviewer_prompt(t, project="scratch", is_git=False)
    lower = out.lower()
    assert "re-run both slots from scratch" in lower
    assert "a fix changes the reviewed code" in lower
    assert "any earlier slot pass is stale" in lower
    assert "re-run that slot" not in lower
    assert "same final code" in lower
    assert "if any fix lands after a slot passed" in lower


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
    assert "--phase push" not in out
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


# ---------- Scenario contract (scenario-first testing, PR-2) ----------
#
# S1/S3/S4 pin the prompt text each subagent sees — the prompt IS the
# boundary between the coord and the worker. S5 executes the finisher's
# embedded gate snippet with PATH shims. S6 runs the built binary.


def _task_contexts(prompt: str) -> list[str]:
    """The decoded `--task-context` argument of every review_slot line."""
    out = []
    marker = "--task-context "
    pos = prompt.find(marker)
    while pos != -1:
        lexer = shlex.shlex(prompt[pos + len(marker):], posix=True)
        lexer.whitespace_split = True
        out.append(lexer.get_token())
        pos = prompt.find(marker, pos + len(marker))
    return out


# Non-Fleet project standards. Fleet dispatches workers into arbitrary
# projects, so the prompt text is pinned against a project that is NOT
# Fleet: a remote preview sandbox gated by `npm test` (S1/S3) and a
# project with no runnable instance at all (S9). Anything Fleet-specific
# that shows up in the rendered prompt is a leak from dispatch.py.
_STANDARDS_REMOTE = (
    "# Standards\n\n## Sandbox\ntier: remote\nup: bin/preview up\n"
    "down: bin/preview down\nisolation: namespace\nobserve: bin/preview logs\n"
    "credentials: PREVIEW_TOKEN\nnotes:\n\n## Gates\n- npm test\n"
    "baseline: git worktree add /tmp/base origin/main && cd /tmp/base && npm test\n"
)
_STANDARDS_NONE = (
    "# Standards\n\n## Sandbox\ntier: none\nup:\ndown:\nisolation: namespace\n"
    "observe:\ncredentials:\nnotes:\n\n## Gates\n\nbaseline:\n"
)
_S1_REQUIRED = (
    "--phase spec-repro", "--phase spec-encode", "--phase verify",
    "Read `## Sandbox` and `## Gates` in the standards above",
    "Export\n    FLEET_WORKER_SLUG=", 'eval "$(<up>)"', "<observe>", "<down>",
    "obtain the artifact the row names",
    "verification.md", "Evidence — before", "A row above the tier (e2e with tier=none)",
    "Do not stand in a mock for a boundary `## Sandbox` can run",
    "project's own test framework", "it MUST fail for the reason",
    "Run EVERY\n    line of `## Gates`, in order", "`baseline:` command on untouched main",
    "## Baseline", "do NOT flip\n    review-pending", "standards: no ## Gates",
    "--phase verify \\\n        --scenarios-total M --scenarios-verified N --gates-status passed",
)
# Fleet-the-project nouns that must never reach a worker in another
# project (acceptance 3 of the task plan + the rev 1 TDD ladder).
_FLEET_NOUNS = (
    "go build", "$FLEET_DEV_BIN", "FLEET_HOME", "FLEET_TMUX_SOCKET", "go test",
    "pytest", "docs/TESTING.md", "coorde2e", ".github/workflows/ci.yml",
    "lint-test-isolation", "-tags=integration", "tdd-", "Write the failing test",
    "verify locally",
)


def _outside_standards(out: str) -> str:
    """The worker prompt minus the inlined standards block — the standards
    are the project's own words and may say anything."""
    head, _, rest = out.partition("## Standards (the bar — non-negotiable)")
    _, _, tail = rest.partition("\n## Required workflow")
    return head + "\n## Required workflow" + tail


def _assert_s1_prompt(out: str, *, is_git: bool) -> None:
    for needle in _S1_REQUIRED:
        assert needle in out, f"S1: worker prompt missing {needle!r}"
    generic = _outside_standards(out)
    for needle in _FLEET_NOUNS:
        assert needle not in generic, f"S1: worker prompt leaks Fleet noun {needle!r}"
    # The scenario phases run in order, after branch and before the
    # handoff, and the flags update lands before review-pending.
    order = ["--phase branch", "--phase spec-repro", "--phase spec-encode",
             "--phase verify", "--gates-status passed", "--phase review-pending"]
    idx = [out.index(s) for s in order]
    assert idx == sorted(idx), f"S1: phase order wrong: {order} at {idx}"
    if is_git:
        assert "git commit" in out
    else:
        assert "git commit" not in out


@pytest.mark.parametrize("is_git", [True, False], ids=["git", "non-git"])
def test_s1_worker_prompt_is_scenario_first_and_project_agnostic(is_git: bool) -> None:
    """S1: rendered for a NON-Fleet project (tier remote, `npm test`), the
    worker prompt carries 2a REPRODUCE / 2b ENCODE / 2c IMPLEMENT + VERIFY
    driven only by `## Sandbox` / `## Gates`, the verification.md path +
    sections and the three flags — and no Fleet noun, no TDD ladder."""
    t = _make_task()
    out = dispatch.build_worker_prompt(
        t, project="shop", standards_md=_STANDARDS_REMOTE, learnings_text="",
        is_git=is_git,
    )
    _assert_s1_prompt(out, is_git=is_git)
    for header in ("## Scenario contract", "## Evidence — before",
                   "## Evidence — after", "## Gates", "## Baseline",
                   "## Unit tests"):
        assert header in out, f"S1: verification.md section {header!r} missing"
    assert "~/.fleet/projects/shop/workers/fix-thing-aaaa/verification.md" in out
    assert "FLEET_WORKER_SLUG=fix-thing-aaaa" in out
    # The project's own sandbox/gates reach the worker through the
    # standards block, not through dispatch.py.
    assert "up: bin/preview up" in out and "- npm test" in out
    assert "npm test" not in _outside_standards(out)


@pytest.mark.parametrize("is_git", [True, False], ids=["git", "non-git"])
def test_s9_tier_none_renders_same_prompt_text(is_git: bool) -> None:
    """S9: a project with no runnable instance (tier none, empty gates)
    gets byte-for-byte the same instructions as the remote project — the
    only difference is the inlined standards. Nothing in dispatch.py
    branches on the tier."""
    t = _make_task()
    remote = dispatch.build_worker_prompt(
        t, project="shop", standards_md=_STANDARDS_REMOTE, learnings_text="",
        is_git=is_git,
    )
    none = dispatch.build_worker_prompt(
        t, project="shop", standards_md=_STANDARDS_NONE, learnings_text="",
        is_git=is_git,
    )
    assert remote != none
    assert _outside_standards(remote) == _outside_standards(none)
    _assert_s1_prompt(none, is_git=is_git)
    assert "tier: none" in none


@pytest.mark.parametrize("is_git", [True, False], ids=["git", "non-git"])
def test_s3_reviewer_prompt_carries_contract_lens(is_git: bool) -> None:
    """S3: the reviewer prompt carries the Scenario-contract lens (row at
    its level, observable outcome, evidence-before, tier/max-level and
    mock-for-a-runnable-boundary are P1) and the re-verify-after-fix rule
    over EVERY `## Gates` line — no Fleet gate list; non-git threads the
    lens via --task-context."""
    t = _make_task()
    out = dispatch.build_reviewer_prompt(t, project="shop", is_git=is_git)
    for needle in ("Scenario contract", "observable outcome", "re-run",
                   "## Review re-verification", "Evidence — before", "[P1]",
                   "verification.md", "e2e/integ/replay/unit", "max level",
                   "tier: local/remote → e2e, none → replay",
                   "mock standing in for a boundary `## Sandbox` can run",
                   "EVERY line of `## Gates` from\n   `fleet standards show --merged --project shop`"):
        assert needle in out, f"S3: reviewer prompt missing {needle!r}"
    for needle in _FLEET_NOUNS:
        assert needle not in out, f"S3: reviewer prompt leaks Fleet noun {needle!r}"
    # Re-verification is appended BEFORE the terminal review-done write.
    assert out.index("## Review re-verification") < out.index("--phase review-done")
    contexts = _task_contexts(out)
    if is_git:
        assert contexts == []
    else:
        assert len(contexts) == 1
        for ctx in contexts:
            assert ctx.startswith(f"{t.spec}\n\nAcceptance:\n{t.acceptance}")
            assert "Scenario contract lens" in ctx
            assert "observable outcome" in ctx
            assert "`## Sandbox` max level" in ctx
            assert "mock for a boundary `## Sandbox` can run" in ctx


@pytest.mark.parametrize("is_git", [True, False], ids=["git", "non-git"])
def test_s4_finisher_prompt_ships_verification_not_placeholder(is_git: bool) -> None:
    """S4: the finisher reads verification.md into `## Verification`
    (PR body / done note) and never emits the old checklist placeholder."""
    t = _make_task()
    out = dispatch.build_finisher_prompt(t, project="fleet", is_git=is_git)
    for needle in ("verification.md", "## Verification",
                   "finisher: no verification evidence (~/.fleet/projects/fleet/"
                   "workers/fix-thing-aaaa/verification.md)",
                   "section_nonempty '## Gates'",
                   "section_nonempty '## Evidence — after'"):
        assert needle in out, f"S4: finisher prompt missing {needle!r}"
    for needle in ("- [ ] CI green", "- [ ] verify locally", "## Test plan"):
        assert needle not in out, f"S4: finisher prompt still carries {needle!r}"
    if is_git:
        assert '## Verification\n$(cat "$V")' in out
    else:
        assert "fleet tasks note" in out
        assert "## Verification" in out.split("fleet tasks note", 1)[1]


_S5_GATE_RE = re.compile(r"```sh\n(.*?)```", re.S)


def _s5_run_finisher_gate(tmp_path, verification: str | None) -> tuple[int, list[str]]:
    """Stand up a sandbox FLEET_HOME with a worker at review-done, drop
    `verification` at its verification.md (None = no file), then run the
    finisher prompt's step-3 gate snippet followed by the step-4 PR
    command with `fleet`/`gh`/`git` shimmed onto PATH. Returns the
    script's exit code and the shims' call log."""
    fleet_home = tmp_path / "home"
    workers_dir = fleet_home / "projects" / "fleet" / "workers" / "fix-thing-aaaa"
    workers_dir.mkdir(parents=True)
    (workers_dir / "state.json").write_text('{"phase":"review-done"}\n')
    if verification is not None:
        (workers_dir / "verification.md").write_text(verification)
    shims = tmp_path / "bin"
    shims.mkdir()
    log = tmp_path / "calls.log"
    for tool in ("fleet", "gh", "git"):
        shim = shims / tool
        shim.write_text(f'#!/bin/sh\necho "{tool} $*" >> "{log}"\n')
        shim.chmod(0o755)
    t = _make_task()
    prompt = dispatch.build_finisher_prompt(
        t, project="fleet", is_git=True, workers_dir=str(workers_dir),
    )
    m = _S5_GATE_RE.search(prompt)
    assert m, "S5: finisher prompt embeds no ```sh gate snippet"
    script = m.group(1) + "\ngh pr create --base main --head worker/fix-thing-aaaa\n"
    env = dict(os.environ, PATH=f"{shims}:{os.environ['PATH']}", FLEET_HOME=str(fleet_home))
    proc = subprocess.run(
        ["bash", "-c", script], env=env, capture_output=True, text=True, check=False,
    )
    calls = log.read_text().splitlines() if log.exists() else []
    return proc.returncode, calls


_S5_VERIFICATION_EMPTY_GATES = (
    "## Scenario contract\n| S1 | x |\n\n"
    "## Evidence — before\nsaw the bug\n\n"
    "## Evidence — after\nPASS\n\n"
    "## Gates\n\n"
    "## Baseline\n\n## Unit tests\ntest_x\n"
)
_S5_VERIFICATION_FULL = _S5_VERIFICATION_EMPTY_GATES.replace(
    "## Gates\n\n", "## Gates\ngo build ./... ok\n\n",
)


@pytest.mark.parametrize(
    "verification",
    [_S5_VERIFICATION_EMPTY_GATES, None,
     _S5_VERIFICATION_FULL.replace("## Evidence — after\nPASS\n", "## Evidence — after\n")],
    ids=["empty-gates", "missing-file", "empty-evidence-after"],
)
def test_s5_finisher_gate_blocks_without_evidence(tmp_path, verification) -> None:
    """S5: running the finisher's own gate against a worker whose
    verification.md is missing or has an empty `## Gates` /
    `## Evidence — after` writes phase=blocked with the canonical reason
    and never reaches `gh pr create`."""
    rc, calls = _s5_run_finisher_gate(tmp_path, verification)
    assert rc == 0, f"S5: gate must exit 0 after blocking, got {rc}: {calls}"
    path = tmp_path / "home/projects/fleet/workers/fix-thing-aaaa/verification.md"
    assert calls == [
        "fleet workers update fix-thing-aaaa --project fleet --phase blocked "
        f"--reason finisher: no verification evidence ({path})",
    ], f"S5: unexpected shim calls: {calls}"


def test_s5_finisher_gate_passes_with_evidence(tmp_path) -> None:
    """S5 (control): a filled `## Gates` + `## Evidence — after` lets the
    same snippet fall through to the PR step."""
    rc, calls = _s5_run_finisher_gate(tmp_path, _S5_VERIFICATION_FULL)
    assert rc == 0
    assert calls == ["gh pr create --base main --head worker/fix-thing-aaaa"], calls


def test_s6_built_binary_merged_standards_are_scenario_first(tmp_path) -> None:
    """S6: `fleet standards show --merged` from a freshly built binary in a
    sandbox HOME prints the scenario-first `## Testing` section and no TDD
    text. Built binary, not the template file: the embedded copy is what
    every worker's prompt gets."""
    fleet_bin = _build_fleet_bin(tmp_path)
    if not fleet_bin:
        pytest.skip("could not build fleet binary; skipping S6 e2e")
    home = tmp_path / "home"
    home.mkdir()
    (home / ".fleet" / "projects" / "demo").mkdir(parents=True)
    env = dict(os.environ, HOME=str(home))
    env.pop("FLEET_HOME", None)
    init = subprocess.run(
        [fleet_bin, "init", "--parallelism", "2"],
        env=env, capture_output=True, text=True, check=False,
    )
    assert init.returncode == 0, init.stderr
    show = subprocess.run(
        [fleet_bin, "standards", "show", "--project", "demo", "--merged"],
        env=env, capture_output=True, text=True, check=False,
    )
    assert show.returncode == 0, show.stderr
    assert "## Testing" in show.stdout
    assert "Scenario-first, not test-first." in show.stdout
    assert "Test at the boundary where the user/operator would see it" in show.stdout
    assert "TDD required" not in show.stdout
    assert "tdd" not in show.stdout.lower()
    for noun in _FLEET_NOUNS:
        assert noun not in show.stdout, f"S6: global standards carry Fleet noun {noun!r}"


def test_s10_built_binary_project_sandbox_wins_over_global(tmp_path) -> None:
    """S10 (pytest half; the Go half is cmd/fleet/init_test.go +
    standards_test.go): `fleet init` seeds a global `## Sandbox` with
    `tier: none` and an empty `## Gates`; a project override with
    `tier: remote` + `npm test` wins in `standards show --merged`."""
    fleet_bin = _build_fleet_bin(tmp_path)
    if not fleet_bin:
        pytest.skip("could not build fleet binary; skipping S10 e2e")
    home = tmp_path / "home"
    home.mkdir()
    (home / ".fleet" / "projects" / "shop").mkdir(parents=True)
    env = dict(os.environ, HOME=str(home))
    env.pop("FLEET_HOME", None)
    init = subprocess.run(
        [fleet_bin, "init", "--parallelism", "2"],
        env=env, capture_output=True, text=True, check=False,
    )
    assert init.returncode == 0, init.stderr
    glob = subprocess.run(
        [fleet_bin, "standards", "show", "--global"],
        env=env, capture_output=True, text=True, check=False,
    )
    assert glob.returncode == 0, glob.stderr
    assert "## Sandbox" in glob.stdout and "## Gates" in glob.stdout
    assert re.search(r"^tier: none", glob.stdout, re.M), glob.stdout
    gates = glob.stdout.split("## Gates", 1)[1].split("\n## ", 1)[0]
    assert not re.search(r"^- ", gates, re.M), f"S10: seeded ## Gates not empty: {gates}"
    assert "fleet standards edit --project <p>" in glob.stdout

    (home / ".fleet" / "projects" / "shop" / "standards.md").write_text(_STANDARDS_REMOTE)
    merged = subprocess.run(
        [fleet_bin, "standards", "show", "--project", "shop", "--merged"],
        env=env, capture_output=True, text=True, check=False,
    )
    assert merged.returncode == 0, merged.stderr
    assert re.search(r"^tier: remote", merged.stdout, re.M), merged.stdout
    assert not re.search(r"^tier: none", merged.stdout, re.M), merged.stdout
    assert "- npm test" in merged.stdout
    assert merged.stdout.count("\n## Sandbox\n") == 1
    assert merged.stdout.count("\n## Gates\n") == 1
