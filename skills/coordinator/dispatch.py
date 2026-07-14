"""Worker prompt assembly + Agent-tool DISPATCH instruction emitter (issue #84).

The coordinator decides what to dispatch (loop.py); this module owns
the mechanics of HOW to dispatch:

  1. build_worker_prompt(task, standards_md, learnings_md) — assemble
     the self-contained prompt the worker receives on its first turn.
  2. mint_agent_id() — generate an 8-hex token used as the worker's
     fleet agent_id (matches the token shape `fleet dispatch` used to
     return).
  3. format_dispatch_instruction(...) — render a human + machine
     readable DISPATCH block. The Python skill cannot call Claude's
     Agent tool directly (it's a Claude tool, not a Python API); the
     skill prints the DISPATCH block to stdout and the coord agent
     (Claude session running /coordinator) reads it and invokes the
     Agent tool with the listed parameters. SKILL.md's "Worker
     dispatch protocol" section pins this contract for Claude.
  4. write_worker_inbox(agent_id, prompt) — drop the prompt into the
     worker's inbox so the coord can pass it via Agent's `prompt`
     parameter (the coord Reads the file then hands the body to
     Agent). The file path lives in the DISPATCH block.

All mutations route through the fleet CLI — Go remains the
authoritative writer for tasks.md and agent records. parse.py is
read-only inside the skill.

Phase A scope: dispatch mechanism only. Phase B (deferred) covers
[a] task-attach replacement, lifecycle/handoff for Agent subagents.
Phase C (deferred) covers TUI subagent_id surfacing.
"""
from __future__ import annotations

import json
import os
import re
import secrets
import subprocess
import tempfile
from dataclasses import dataclass
from datetime import datetime, timezone

import parse
import reviewcfg


# Hard cap on rendered prompt size (ENG §6.5: ≤4KB rendered). Soft cap
# at 8KB so we have headroom for unusually long Spec bodies — exceeding
# this is a sign the operator over-specified the task and the loop will
# refuse to dispatch.
_PROMPT_HARD_CAP_BYTES = 16 * 1024
# Per-learning truncation cap (ENG §6.5: 500 chars × 5).
_LEARNING_BODY_CAP = 500
_MAX_LEARNINGS_INLINED = 5


@dataclass
class DispatchResult:
    """Result of one dispatch attempt.

    agent_id is the 8-hex worker ID printed by `fleet dispatch` on
    success. Empty when error is non-empty.
    """

    agent_id: str = ""
    branch: str = ""
    error: str = ""


def project_is_git(project: str, *, fleet_home: str | None = None) -> bool:
    """Return True when ~/.fleet/projects/<project>/meta.json reports git mode.

    Mirrors `projects.Meta.GitMode()` on the Go side: an absent file (or
    absent is_git field) defaults to True so legacy projects pre-dating
    the non-git feature keep behaving as git-backed projects. Read
    errors (malformed JSON, permission denied) also default to True —
    git-mode is strictly more conservative for the dispatch path.

    The fleet_home arg lets tests point this at a sandbox directory
    instead of ~/.fleet.
    """
    home = fleet_home or os.environ.get("FLEET_HOME") or os.path.expanduser("~/.fleet")
    path = os.path.join(home, "projects", project, "meta.json")
    try:
        with open(path, "r", encoding="utf-8") as fh:
            data = json.load(fh)
    except (FileNotFoundError, OSError, json.JSONDecodeError):
        return True
    val = data.get("is_git")
    if val is None:
        return True
    return bool(val)


def worktree_preflight_ok(
    computed_path: str,
    expected_branch: str,
    *,
    timeout_s: float = 15.0,
) -> tuple[bool, str]:
    """DESIGN §4.2 dispatch-side preflight, keyed on the COMPUTED path.

    Before `git worktree add`, a `status=ready` task whose persisted
    `worktree=` is EMPTY can still have a LEAKED tree on disk at the
    deterministic `worker/<slug>` path (a prior attempt's dirty-parked or
    branch-mismatch tree that was kept). create_worktree treats a
    registered path collision as success WITHOUT a cleanliness/branch
    check, so dispatching would silently hand the worker that stale tree.

    This preflight refuses dispatch when a tree at computed_path is
    WRONG-BRANCH or DIRTY:

      (ok=True,  "")        — no tree at computed_path (fresh dispatch),
                              OR a tree that is on expected_branch AND
                              clean (a resumable idempotent-create tree).
      (ok=False, "<why>")   — a tree exists at computed_path that is on a
                              different branch, has a detached/unknown
                              HEAD, is dirty, or whose cleanliness can't
                              be determined. Caller REFUSES dispatch +
                              surfaces the reason.

    Keyed on the COMPUTED path (not the persisted worktree=, which is ""
    on a leaked-dir row). Pure read — never mutates. Never raises.
    """
    if not computed_path or not expected_branch:
        # Nothing to check (non-git / no path resolved) — let the normal
        # create path proceed; it has its own collision handling.
        return (True, "")
    if not os.path.exists(computed_path):
        return (True, "")
    # A tree is present. Verify branch identity first.
    try:
        proc = subprocess.run(
            ["git", "-C", computed_path, "rev-parse", "--abbrev-ref", "HEAD"],
            capture_output=True, text=True, timeout=timeout_s, check=False,
        )
    except (FileNotFoundError, subprocess.TimeoutExpired) as exc:
        return (False, f"preflight: branch probe failed at {computed_path}: {exc}")
    if proc.returncode != 0:
        stderr = (proc.stderr or proc.stdout or "").strip()
        return (False, f"preflight: not a git worktree at {computed_path}: {stderr}")
    actual_branch = (proc.stdout or "").strip()
    if actual_branch != expected_branch:
        return (
            False,
            f"preflight: tree at {computed_path} is on branch "
            f"{actual_branch!r} != expected {expected_branch!r} — refusing "
            f"dispatch (a prior attempt's tree was left behind; resolve it)",
        )
    # Right branch — now require it to be clean.
    try:
        sproc = subprocess.run(
            ["git", "-C", computed_path, "status", "--porcelain"],
            capture_output=True, text=True, timeout=timeout_s, check=False,
        )
    except (FileNotFoundError, subprocess.TimeoutExpired) as exc:
        return (False, f"preflight: status probe failed at {computed_path}: {exc}")
    if sproc.returncode != 0:
        stderr = (sproc.stderr or sproc.stdout or "").strip()
        return (False, f"preflight: status --porcelain failed at {computed_path}: {stderr}")
    if (sproc.stdout or "").strip():
        return (
            False,
            f"preflight: tree at {computed_path} ({expected_branch}) has "
            f"uncommitted changes — refusing dispatch (resolve / commit first)",
        )
    return (True, "")


def build_worker_prompt(
    task: parse.Task,
    project: str,
    standards_md: str,
    learnings_text: str,
    *,
    branch: str | None = None,
    workers_dir: str | None = None,
    worktree_pre_created: bool = False,
    is_git: bool = True,
    dispatch_generation: int = 0,
) -> str:
    """Assemble the worker's first-turn prompt.

    standards_md: the merged result from `fleet standards show --merged`
                  (already rendered as one markdown blob).
    learnings_text: the raw output of `fleet learnings list --limit=20`.
                  Inlined as-is up to the per-entry cap; the worker sees
                  the same table the operator does.
    branch: defaults to "worker/<slug>".
    workers_dir: defaults to "~/.fleet/projects/<project>/workers/<slug>".
    worktree_pre_created: True when the coord already created the
                  worker's worktree via `git worktree add -b <branch>`
                  (cap > 1 mode). The branch + worktree both already
                  exist on disk, so step 1 of the workflow is a verify
                  rather than a `git checkout -b`. False (default) keeps
                  single-worker mode byte-identical to v0.2.0 — worker
                  runs `git checkout -b <branch>` itself.

    The format mirrors ENG §6.5 — self-contained, no inheritance from
    parent context. Safe to truncate at the hard cap; operator is on
    the hook for keeping standards/learnings/specs within budget.
    """
    if branch is None:
        branch = f"worker/{task.slug}"
    if workers_dir is None:
        workers_dir = f"~/.fleet/projects/{project}/workers/{task.slug}"

    spec = task.spec.strip() or "(spec pending — operator should populate ### Spec)"
    acceptance = task.acceptance.strip() or "(acceptance pending — operator should populate ### Acceptance)"
    standards = (standards_md or "").strip() or "(no standards configured)"
    learnings_section = _select_learnings(learnings_text)

    lines: list[str] = [
        f"You are a Fleet worker for task: {task.slug}",
        f"Project: {project}",
        f"Branch: {branch}",
        "",
        "You are running as a Fleet-dispatched Claude session. The operator",
        "is NOT watching this terminal — communicate progress via",
        "`fleet workers update <slug> --phase <p>` after every phase",
        "boundary. Exit cleanly (Ctrl-D / /exit) once you reach phase=done",
        "or phase=blocked; the coordinator polls workers/<slug>/state.json",
        "to know when to advance the task.",
        "",
        f"State file:  {workers_dir}/state.json",
        f"Output log:  {workers_dir}/output.log",
        "             (anything you echo to stdout/stderr goes here)",
        "",
        "## Task",
        "",
        spec,
        "",
        "## Acceptance",
        "",
        acceptance,
        "",
        "## Standards (the bar — non-negotiable)",
        "",
        standards,
        "",
    ]
    if learnings_section:
        lines.extend([
            "## Relevant prior learnings",
            "",
            learnings_section,
            "",
        ])
    # All `fleet workers update` invocations include --project so they
    # land in the right project tree even when cwd's basename differs
    # from the project name (codex iter-3 [P2]: without an explicit
    # --project, the CLI's cwd-default project resolution would write
    # heartbeats into a phantom ~/.fleet/projects/<wrong>/workers/...
    # tree the coordinator never reads).
    proj_flag = f"--project {project}"
    # Fold the coord-owned per-slug fence token (DESIGN §2.2) into the
    # shared flag string so EVERY `fleet workers update` the worker runs
    # carries --dispatch-generation <gen> and is CAS'd against the task
    # row. A stale prior attempt that wakes and runs these commands is
    # rejected; the current attempt's gen matches the task-row authority
    # _apply_dispatch persisted. gen==0 (legacy / un-migrated dispatch)
    # omits the flag, keeping the ungated path.
    if int(dispatch_generation) > 0:
        proj_flag = f"{proj_flag} --dispatch-generation {int(dispatch_generation)}"
    # Step 1 differs by mode: cap=1 the worker creates the branch
    # itself; cap>1 the coord already ran `git worktree add -b <branch>`
    # so the worker's cwd is already the worktree on the right branch.
    # Doing `git checkout -b` again would fatal "branch already exists"
    # and stall every parallel worker on its first git step (codex
    # iter-1 [P1]).
    # Non-git mode skips branch creation entirely — workers edit the
    # project directory directly, no commits.
    if not is_git:
        step1 = (
            "1. Non-git project — there is no branch to create. Edit the "
            "project directory in place. No commits."
        )
    elif worktree_pre_created:
        step1 = (
            f"1. Confirm you are on the prepared worktree on branch {branch} "
            f"(coord ran `git worktree add -b {branch}`). "
            "Run `git rev-parse --abbrev-ref HEAD` to verify."
        )
    else:
        step1 = f"1. git checkout -b {branch}"
    if is_git:
        tdd_red_line = "2a. Write the failing test. git commit."
        tdd_green_line = "2b. Write the minimal impl. Test passes. git commit."
        tdd_refactor_line = "2c. Refactor without changing test behavior. git commit."
        step3_line = (
            "3. Commits are landed locally on the worker branch. Exit cleanly\n"
            "   (Ctrl-D / /exit). The coord polls state.json on the next tick,\n"
            "   sees phase=review-pending, and dispatches the reviewer subagent."
        )
    else:
        # Non-git: same phase machine, no commits. Workers leave a clean
        # local diff in the project directory and exit; reviewer reads
        # the diff from `git status`-equivalent (or just file mtime if
        # truly non-git). The phase=review-pending transition is the
        # same handoff signal.
        tdd_red_line = "2a. Write the failing test (file in place, no commit)."
        tdd_green_line = "2b. Write the minimal impl. Test passes (file in place, no commit)."
        tdd_refactor_line = "2c. Refactor without changing test behavior (file in place, no commit)."
        step3_line = (
            "3. Files are landed in place. Exit cleanly (Ctrl-D / /exit). The\n"
            "   coord polls state.json on the next tick, sees phase=review-pending,\n"
            "   and dispatches the reviewer subagent."
        )
    workflow_header = (
        "## Required workflow (three-stage flow — reviewer-subagent-arch)"
        if is_git
        else "## Required workflow (three-stage flow — non-git project)"
    )
    workflow_intro = (
        [
            "You write CODE + TESTS, commit them locally, and EXIT at",
            "phase=review-pending. A separate reviewer subagent (dispatched",
            "by the coord on the next tick) runs /review + codex against your",
            "branch. A separate finisher subagent pushes + opens the PR.",
        ]
        if is_git
        else [
            "You write CODE + TESTS directly in the project directory (no",
            "branches, no commits — this is a non-git project) and EXIT at",
            "phase=review-pending. A reviewer subagent runs /review on your",
            "local diff. A finisher subagent marks the task done with a",
            "diff summary. There is NO branch, NO push, NO PR.",
        ]
    )
    workflow_prohibit = (
        [
            "You do NOT run /review yourself. You do NOT run /codex review",
            "yourself. You do NOT push the branch. You do NOT open the PR.",
            "Doing any of those is a contract violation — the reviewer is the",
            "dedicated reviewer for a reason (structural enforcement that",
            "/review actually ran).",
        ]
        if is_git
        else [
            "You do NOT run /review yourself. You do NOT run /codex review",
            "yourself. Doing either is a contract violation — the reviewer",
            "is the dedicated reviewer for a reason (structural enforcement",
            "that /review actually ran). Non-git also means no branch / no",
            "push / no PR — there is no such thing in this project.",
        ]
    )
    lines.append(workflow_header)
    lines.append("")
    lines.extend(workflow_intro)
    lines.append("")
    lines.extend(workflow_prohibit)
    lines.append("")
    lines.extend([
        f"  fleet workers update {task.slug} {proj_flag} --phase branch",
        step1,
        "",
        f"  fleet workers update {task.slug} {proj_flag} --phase tdd-red",
        tdd_red_line,
        "",
        f"  fleet workers update {task.slug} {proj_flag} --phase tdd-green",
        tdd_green_line,
        "",
        f"  fleet workers update {task.slug} {proj_flag} --phase tdd-refactor",
        tdd_refactor_line,
        "",
        f"  fleet workers update {task.slug} {proj_flag} --phase review-pending",
        step3_line,
        "",
        "## Constraints",
        "",
        "- Stay on this task. File incidental bugs (max 3/session, honor system):",
        f"    fleet tasks add --project {project} --spawned-by {task.slug} --priority P3 \\",
        "      --slug <short> \"<one-line spec>\"",
        "  Operator must promote before dispatch.",
        "- Do NOT edit tasks.md or standards.md directly.",
        "- Stuck or genuinely confused:",
        f"    fleet workers update {task.slug} {proj_flag} --phase blocked --reason \"<one line>\"",
        "  Then exit 0. Coord raises to operator.",
        "",
        # Post-completion contract — agent-tool subagents have no kill
        # signal from the parent; their lifecycle ends only when they
        # return. A subagent that finishes its §7 contract and KEEPS
        # working (opening bonus PRs, amending the branch, expanding
        # scope) is a CLAUDE.md §8 violation. PR #124 (closed) was the
        # motivating case — a README rewrite subagent added a 9th
        # feature bullet not in the brief after returning its §7 block.
        # The post-completion contract makes the boundary explicit so
        # a worker that "noticed something to fix" routes it through
        # `fleet tasks add` (P3 backlog) instead of acting on it.
        "## Post-completion contract",
        "",
        "After you flip --phase review-pending and exit, your work for this",
        "dispatch is COMPLETE. You may NOT:",
        "  - open PRs (the finisher does that)",
        "  - run /review or /codex review (the reviewer does that)",
        "  - file additional bugs / tasks unless explicitly invited",
        "  - amend, push, or rebase any branch",
        "  - take ANY further action on this codebase",
        "",
        "If during the work you noticed valid follow-up ideas, do NOT do",
        "that work yourself. The original task spec is the closed scope.",
        "File a P3 ticket via",
        f"    fleet tasks add --project {project} --spawned-by {task.slug} \\",
        "      --priority P3 --slug <short> \"<one-liner>\"",
        "so the operator triages it. Bonus content violates CLAUDE.md §8.",
        "",
        "Specific past violation: a subagent dispatched to write a README",
        "with N feature bullets opened an unauthorized PR adding bullet N+1",
        "after returning its §7 contract. That PR was closed. Do not repeat",
        "the pattern.",
        "",
    ])
    if is_git:
        lines.extend([
            "You have: gh, git, full repo at <cwd>. /review and /codex review",
            "are reserved for the reviewer subagent — do NOT invoke them here.",
            "NO interactive chat — operator can't reply mid-flight. Communicate via",
            "`fleet workers update`, which mutates state.json atomically.",
        ])
    else:
        lines.extend([
            "You have: the project directory at <cwd> (NOT a git repo). /review",
            "is reserved for the reviewer subagent — do NOT invoke it here. Do",
            "not attempt `git` commands; this project has no .git entry.",
            "NO interactive chat — operator can't reply mid-flight. Communicate via",
            "`fleet workers update`, which mutates state.json atomically.",
        ])
    out = "\n".join(lines)
    if len(out.encode("utf-8")) > _PROMPT_HARD_CAP_BYTES:
        # Fail fast rather than truncate silently — silent truncation
        # could cut off the workflow section, leaving workers with
        # half-instructions. Caller (loop.py) raises to operator with
        # this error inlined as the task's blocked_reason.
        raise PromptTooLargeError(
            f"worker prompt for {task.slug} is "
            f"{len(out.encode('utf-8'))}B (cap {_PROMPT_HARD_CAP_BYTES}B); "
            "shrink standards / learnings / spec",
        )
    return out


class PromptTooLargeError(Exception):
    """Worker prompt exceeded the hard cap. Operator must shrink inputs."""


# Canonical engine identifiers used by build_reviewer_prompt /
# build_finisher_prompt / build_worker_prompt to derive a coord-engine-
# aware banner. Mirrors internal/enginecfg.EngineClaudeCode /
# EngineCodex. Kept as plain string literals so the skill has no
# dependency on the Go side at import time.
ENGINE_CLAUDE_CODE = "claude-code"
ENGINE_CODEX = "codex"


def build_reviewer_prompt(
    task: parse.Task,
    project: str,
    *,
    branch: str | None = None,
    workers_dir: str | None = None,
    worktree: str | None = None,
    is_git: bool = True,
    coord_engine: str | None = None,
    dispatch_generation: int = 0,
    has_codex: bool = False,
    resolution: reviewcfg.Resolution | None = None,
) -> str:
    """Assemble the reviewer subagent's first-turn prompt.

    Three-stage flow (reviewer-subagent-arch): after the worker writes
    phase=review-pending and exits, the coord dispatches THIS subagent
    against the same worker dir + branch. The reviewer runs the resolved
    alpha/beta slots via review_slot.py, fixes P0/P1 findings, and records
    terminal slot-named status before flipping phase=review-done. The
    finisher subagent picks up from there.

    Contract this prompt enforces (matches CLAUDE.md §4):
      - Each round runs BOTH slots through review_slot.py.
      - exit 0 records that slot passed; exit 1 means P0/P1 findings,
        so the reviewer fixes and reruns that slot; exit 2 means a git
        codex alpha slot was skipped because review_slot.py printed
        rate-limited|unavailable on stdout; exit 3 blocks the worker and
        does not flip review-done.
      - Per-iteration fixes are committed (`fix: review iter-N — <line>`)
        on the worker's branch. No squashing.
      - Final action: `fleet workers update <slug> --phase review-done`
        with terminal --review-alpha-* and --review-beta-* flags. Then
        exit. The reviewer does NOT push or open the PR.

    coord_engine: the engine the coord session was launched with
                  (claude-code | codex). Defaults to FLEET_ENGINE env or
                  claude-code. APPROACH A (memory project_codex_multi_
                  engine.md): regardless of coord_engine, the reviewer
                  subagent ALWAYS runs claude. When the coord is codex,
                  the worker + finisher subagents also run codex; the
                  reviewer pinch-hits as claude for cross-engine
                  diversity (different model, different blind spots).
                  The prompt body is identical for both cases — it's
                  always written for a claude orchestrator running the
                  resolved review slots against the worker diff — but a
                  banner up top documents the diversity setup so the
                  reviewer subagent understands the role split.
    worktree:     absolute path to the worker's pre-created git worktree
                  (cap > 1 mode), or None for in-place (cap=1) dispatch.
                  When set, step 1 becomes `cd <worktree>` +
                  `git rev-parse --abbrev-ref HEAD` verify instead of
                  `git checkout {branch}`. The worker ran in this
                  worktree, so the branch is already checked out THERE —
                  a bare `git checkout {branch}` in the main repo fatals
                  "branch already used by worktree" and the review never
                  runs (dispatch-reviewer-finish-9316). Ignored in
                  non-git mode (no branches to check out).
    """
    if branch is None:
        branch = f"worker/{task.slug}"
    if workers_dir is None:
        workers_dir = f"~/.fleet/projects/{project}/workers/{task.slug}"
    if coord_engine is None:
        coord_engine = os.environ.get("FLEET_ENGINE", "") or ENGINE_CLAUDE_CODE
    if resolution is None:
        resolution = reviewcfg.resolve_slots(
            has_codex=has_codex,
            is_git=is_git,
            unavailable=set(),
        )
    alpha = resolution.alpha
    beta = resolution.beta

    proj_flag = f"--project {project}"
    # Handoffs INHERIT the dispatching attempt's gen (DESIGN §3) — no
    # increment. Fold it into the shared flag so the reviewer's
    # `fleet workers update --phase review-done` write is CAS'd under
    # the SAME generation as the worker that handed off.
    if int(dispatch_generation) > 0:
        proj_flag = f"{proj_flag} --dispatch-generation {int(dispatch_generation)}"
    base_branch = "main"  # finisher opens the PR against main; reviewer doesn't push.

    # Header + role intro. Non-git differs only in the "what handoff
    # follows" sentence and the absence of branch language.
    header_branch_line = (
        f"Branch to review: {branch}"
        if is_git
        else f"Project mode: non-git (no branches, no commits, no PR)"
    )
    handoff_summary = (
        "the worker's diff, commit any fixes, and flip the phase to review-done\n"
        "so the finisher can push + open the PR."
        if is_git
        else "the worker's in-place file diff, fix any [P0]/[P1] findings directly,\n"
        "and flip the phase to review-done so the finisher can mark the task done."
    )
    state_block = [
        f"State file:  {workers_dir}/state.json",
    ]
    if is_git:
        state_block.append(f"Branch:      {branch} (already on disk, already has the worker's commits)")
        state_block.append(f"Base for review: origin/{base_branch}")
    else:
        state_block.append("Project dir: non-git; worker edited files in place. There is no")
        state_block.append("             branch, no `origin/`, no commits — just the working tree.")

    # Engine-diversity banner (Approach A): when coord = codex, the
    # worker was codex; the reviewer is the cross-engine second opinion
    # (claude). The resolved slots provide the review engines/models.
    # Either way, the reviewer subagent process
    # itself is always claude — that's the structural decision Approach
    # A locks in for the v0.9 MVP. Banner applies to both git and
    # non-git modes — codex coord can run against either project type.
    if coord_engine == ENGINE_CODEX:
        engine_banner = [
            "Cross-engine review diversity (coord engine = codex):",
            "  The worker subagent that wrote the diff was running CODEX.",
            "  You are running CLAUDE as the second-opinion reviewer —",
            "  same role split the operator gets when coord is claude and",
            "  the resolved slots provide the cross-engine view, just",
            "  reversed. Treat the worker's commits as you would any other diff.",
            "",
        ]
    else:
        engine_banner = []

    lines: list[str] = [
        f"You are a Fleet REVIEWER subagent for task: {task.slug}",
        f"Project: {project}",
        header_branch_line,
        "",
        *engine_banner,
        "You are running as a Fleet-dispatched Claude session. The previous",
        "subagent (the worker) wrote the implementation + tests and exited at",
        "phase=review-pending. Your job is to run the two review slots on",
        handoff_summary,
        "",
    ]
    lines.extend(state_block)
    lines.append("")
    lines.append("## Required workflow")
    lines.append("")

    # Step 1 differs by mode (dispatch-reviewer-finish-9316): when the
    # worker ran in a pre-created worktree, the branch is checked out
    # THERE. `git checkout {branch}` in the main repo would fatal
    # "branch already used by worktree" — so cd into the worktree and
    # verify instead. In-place dispatch keeps the original checkout.
    if worktree:
        step1_lines = [
            f"1. `cd {worktree}` — the worker ran in this pre-created worktree, so",
            f"   branch {branch} is already checked out there with the worker's",
            "   commits. Do NOT `git checkout` in the main repo (it fatals \"branch",
            "   already used by worktree\"). Verify with",
            f"   `git rev-parse --abbrev-ref HEAD` (must equal {branch}); you append",
            "   review fixes on top from inside the worktree.",
        ]
    else:
        step1_lines = [
            f"1. `git checkout {branch}` — make sure you're on the worker's branch.",
            "   The worker's commits are already there; you append review fixes on",
            "   top.",
        ]
    base_arg = f" --base origin/{base_branch}" if is_git else ""
    alpha_cmd = (
        f"python3 ~/.claude/skills/coordinator/review_slot.py "
        f"--engine {alpha.engine} --model {alpha.model} --effort high{base_arg}"
    )
    beta_cmd = (
        f"python3 ~/.claude/skills/coordinator/review_slot.py "
        f"--engine {beta.engine} --model {beta.model} --effort high{base_arg}"
    )
    if is_git and alpha.engine == "codex":
        alpha_status_arg = "--review-alpha-status {passed|skipped}"
        alpha_skip_note = (
            "     If alpha is skipped only because codex is rate-limited or unavailable, "
            "add `--review-alpha-skip-reason rate-limited|unavailable`."
        )
        alpha_exit2_note = (
            "   - exit 2 => codex slot skipped (helper prints reason on stdout: "
            "rate-limited|unavailable); record that slot as "
            "`--review-alpha-status skipped --review-alpha-engine codex "
            "--review-alpha-skip-reason <reason>` and continue (beta still must pass)."
        )
        loop_termination_line = (
            "   Loop until BOTH slots are RESOLVED: each slot exits 0 (passed), "
            "OR the codex alpha exits 2 (skipped) — record it as "
            "`--review-alpha-status skipped` and stop re-running it (do not keep "
            "retrying a rate-limited codex). Beta must still reach exit 0 (passed)."
        )
    elif resolution.single_claude_only:
        alpha_status_arg = "--review-alpha-status single-claude-degraded"
        alpha_skip_note = (
            "     Because only one distinct Claude model is available, record alpha "
            "as `single-claude-degraded` after the shared Claude slot passes."
        )
        alpha_exit2_note = ""
        loop_termination_line = "   Loop until BOTH slots exit 0."
    else:
        alpha_status_arg = "--review-alpha-status passed"
        alpha_skip_note = "     Alpha is a Claude slot and must pass; do not skip it."
        alpha_exit2_note = ""
        loop_termination_line = "   Loop until BOTH slots exit 0."
    if is_git:
        fix_instruction = (
            "fix all [P0]/[P1] findings, add regression tests, "
            "`git commit -m \"fix: review iter-N — <one line>\"`, then re-run that slot"
        )
        initial_step = step1_lines
        review_target = "the worker's diff against origin/main"
    else:
        fix_instruction = (
            "fix all [P0]/[P1] findings directly in the files, then re-run that slot"
        )
        initial_step = [
            "1. The worker edited project files in place. Inspect the current",
            "   working tree to understand what changed.",
        ]
        review_target = "the worker's in-place file diff"
    lines.extend([
        *initial_step,
        "",
        f"2. Two-slot review loop for {review_target}:",
        "   Run BOTH slots each round through review_slot.py. Do not invoke",
        "   `/review` as a bare Skill call and do not run engine-specific",
        "   review commands directly; the helper owns engine details.",
        f"   - alpha ({alpha.engine}/{alpha.model}): `{alpha_cmd}`",
        f"   - beta ({beta.engine}/{beta.model}): `{beta_cmd}`",
        "   - exit 0 => record that slot passed.",
        f"   - exit 1 => the slot found [P0]/[P1]; {fix_instruction}.",
        *([alpha_exit2_note] if alpha_exit2_note else []),
        "   - exit 3 => the slot is BLOCKED. Do NOT flip review-done. Run:",
        f"     `fleet workers update {task.slug} {proj_flag} --phase blocked \\",
        "       --reason \"review slot <alpha|beta> blocked: <one line>\"`",
        "     and exit.",
        f"   - Mid-loop phase nudge after a fix: `fleet workers update {task.slug} {proj_flag} \\",
        "       --phase review-claude --review-alpha-status iterating`",
        loop_termination_line,
        "",
        "3. Final terminal write (the load-bearing call):",
        "",
        f"   `fleet workers update {task.slug} {proj_flag} --phase review-done \\",
        f"     {alpha_status_arg} --review-alpha-engine {alpha.engine} \\",
        f"     --review-alpha-model {alpha.model} --review-alpha-rounds <N> \\",
        f"     --review-beta-status passed --review-beta-engine {beta.engine} \\",
        f"     --review-beta-model {beta.model} --review-beta-rounds <M>`",
        alpha_skip_note,
        "     Beta is the Claude anchor and must be recorded as passed.",
        "",
        "4. Exit cleanly (Ctrl-D / /exit) once you wrote --phase review-done.",
        "   The coord polls state.json on the next tick and dispatches the",
        "   finisher subagent. You do NOT push or open the PR.",
        "",
        "## Hard prohibitions",
        "",
        "- Do NOT push the branch. Do NOT `gh pr create`. Do NOT amend.",
        "- Do NOT invoke `/review` as a bare Skill call for the loop.",
        "- Do NOT run engine-specific review commands directly; use review_slot.py.",
        "- Do NOT alter the worker's commits except via review-iter fix commits",
        "  on top in git mode.",
        "",
        "## §7 return contract (terse)",
        "",
        "When done, return: `review iterations: alpha=N beta=M; final alpha=<status>; final beta=passed`.",
        "",
        "You have: review_slot.py, gh/git when this is a git project, and the full project at cwd.",
        "NO interactive chat — operator can't reply mid-flight.",
    ])
    out = "\n".join(lines)
    if len(out.encode("utf-8")) > _PROMPT_HARD_CAP_BYTES:
        raise PromptTooLargeError(
            f"reviewer prompt for {task.slug} is "
            f"{len(out.encode('utf-8'))}B (cap {_PROMPT_HARD_CAP_BYTES}B)",
        )
    return out


def build_finisher_prompt(
    task: parse.Task,
    project: str,
    *,
    branch: str | None = None,
    workers_dir: str | None = None,
    worktree: str | None = None,
    is_git: bool = True,
    dispatch_generation: int = 0,
) -> str:
    """Assemble the finisher subagent's first-turn prompt.

    Three-stage flow: after the reviewer writes phase=review-done +
    terminal review_*_status, the coord dispatches THIS subagent. The
    finisher is purely mechanical:

      1. `git push origin <branch>` (or --force-with-lease if a fix
         landed on a remote-existing branch from a prior attempt).
      2. `gh pr create --base main --head <branch> --title ... --body
         ...` with the standard PR body shape (scope summary +
         reviewer iteration counts + test plan).
      3. `fleet workers update <slug> --phase done --pr-url <url>`.
      4. Exit.

    The finisher does NOT run /review or codex (the reviewer already
    did). The finisher does NOT amend commits or rebase. Any failure
    on push or PR creation flips phase=blocked with the failure as
    --reason. Re-dispatching a failed finisher is the operator's call
    (or a future autosystem; v0 leaves it manual).

    worktree: absolute path to the worker's pre-created git worktree
              (cap > 1 mode), or None for in-place (cap=1) dispatch.
              When set, the prompt opens with a `cd <worktree>` step so
              push + `gh pr create` run from the checkout that holds the
              worker + reviewer commits — the main repo's HEAD is on a
              different branch (dispatch-reviewer-finish-9316). Ignored
              in non-git mode.
    """
    if branch is None:
        branch = f"worker/{task.slug}"
    if workers_dir is None:
        workers_dir = f"~/.fleet/projects/{project}/workers/{task.slug}"

    proj_flag = f"--project {project}"
    # Handoffs INHERIT the dispatching attempt's gen (DESIGN §3) — no
    # increment. The finisher's `fleet workers update --phase done` is
    # CAS'd under the same generation as the worker + reviewer.
    if int(dispatch_generation) > 0:
        proj_flag = f"{proj_flag} --dispatch-generation {int(dispatch_generation)}"

    # Push step. When the worker ran in a pre-created worktree, the cd is
    # folded into step 1 (push + PR must run from the worktree, which
    # holds the worker + reviewer commits; the main repo HEAD is on a
    # different branch). Folding it into step 1 keeps the downstream
    # step numbers (2..5) byte-identical to the in-place path
    # (dispatch-reviewer-finish-9316).
    if worktree:
        push_step = [
            f"1. `cd {worktree}` — the worker ran in this pre-created worktree;",
            f"   branch {branch} (worker + reviewer commits) is checked out THERE,",
            "   so push + PR must run from inside it, NOT the main repo (whose",
            "   HEAD is on a different branch). Then:",
            f"   `git push -u origin {branch}` — fresh push. If origin already has",
            "   a stale prior attempt's tip, use `git push --force-with-lease",
            f"   origin {branch}` (your branch is the only writer; --force-with-",
            "   lease is safe). Never plain `--force`.",
            "",
        ]
    else:
        push_step = [
            f"1. `git push -u origin {branch}` — fresh push. If origin already has",
            "   a stale prior attempt's tip, use `git push --force-with-lease",
            f"   origin {branch}` (your branch is the only writer; --force-with-",
            "   lease is safe). Never plain `--force`.",
            "",
        ]

    if is_git:
        lines: list[str] = [
            f"You are a Fleet FINISHER subagent for task: {task.slug}",
            f"Project: {project}",
            f"Branch to push: {branch}",
            "",
            "You are running as a Fleet-dispatched Claude session. The reviewer",
            "subagent ran the alpha/beta review slots on the worker's diff, recorded",
            "terminal review_alpha_* + review_beta_* fields, and flipped the phase to",
            "review-done. Your job is mechanical: push, open the PR, update the",
            "worker's phase to done. NO code changes. NO review iteration.",
            "",
            f"State file:  {workers_dir}/state.json",
            f"Branch:      {branch} (already has worker + reviewer commits, ready to push)",
            f"Base for PR: main",
            "",
            "## Required workflow",
            "",
            *push_step,
            "2. Read state.json to extract reviewer counts for the PR body:",
            f"   - `cat {workers_dir}/state.json | jq -r '.review_alpha_status, .review_alpha_engine, .review_alpha_model, .review_alpha_rounds, .review_alpha_skip_reason, .review_beta_status, .review_beta_engine, .review_beta_model, .review_beta_rounds'`",
            "",
            f"3. `gh pr create --base main --head {branch} --title '<commit-1 message>' \\",
            "     --body \"$(cat <<'EOF'",
            "## Summary",
            "<1-3 bullets from the worker's commits>",
            "",
            "## Review",
            "- alpha (<engine>/<model>): passed|skipped:<reason>|single-claude-degraded (rounds: <N>)",
            "- beta (claude/<model>): passed (rounds: <M>)",
            "",
            "## Test plan",
            "- [ ] CI green",
            "- [ ] verify locally",
            "EOF",
            "     )\"`",
            "",
            f"4. Capture the PR URL. Then:",
            f"   `fleet workers update {task.slug} {proj_flag} --phase done --pr-url <url> --exit 0`",
            "   If this terminal write is REJECTED by the review gate, do NOT loop",
            "   or retry. Run:",
            f"   `fleet workers update {task.slug} {proj_flag} --phase blocked \\",
            "     --reason \"finisher: review gate rejected — <one-line err>\"`",
            "   and exit.",
            "",
            "5. Exit cleanly.",
            "",
            "## On failure",
            "",
            "If `git push` or `gh pr create` errors, do NOT retry blindly. Flip",
            f"to blocked with the error inlined:",
            f"   `fleet workers update {task.slug} {proj_flag} --phase blocked \\",
            "     --reason \"finisher: <one-line error>\"`",
            "Then exit. The coord raises BLOCKED to the operator.",
            "",
            "## Hard prohibitions",
            "",
            "- Do NOT run /review or codex. The reviewer already did.",
            "- Do NOT amend, rebase, or alter commits.",
            "- Do NOT skip the `fleet workers update --phase done` call. The phase",
            "  is the canonical completion signal; without it the coord can't",
            "  transition the task to in-review.",
            "- Do NOT plain --force-push. --force-with-lease only.",
            "",
            "## §7 return contract (terse)",
            "",
            "Return: `PR URL: <url>; final phase=done`.",
            "",
            "You have: gh, git, full repo at the worker's cwd. NO interactive chat.",
        ]
    else:
        # Non-git finisher: no push, no PR. The deliverable is the
        # local diff already on disk. Worker writes phase=done WITHOUT
        # a pr_url — workers.WriteState relaxes the pr_url precondition
        # for non-git projects. The finisher's job is to:
        #   1. Capture a short diff summary (file list + line counts)
        #   2. Write phase=done
        #   3. Exit
        lines = [
            f"You are a Fleet FINISHER subagent for task: {task.slug} (non-git project)",
            f"Project: {project}",
            "Mode: non-git (no branches, no commits, no push, no PR)",
            "",
            "You are running as a Fleet-dispatched Claude session. The reviewer",
            "subagent ran both Claude review slots on the worker's in-place diff,",
            "recorded review_alpha_status=passed + review_beta_status=passed,",
            "and flipped the phase to review-done. Your job is purely mechanical:",
            "summarize the local diff, mark the task done, exit. NO code changes.",
            "NO review iteration. NO push or PR — there is no git here.",
            "",
            f"State file:  {workers_dir}/state.json",
            "Project dir: the worker edited files in place at <cwd>.",
            "",
            "## Required workflow",
            "",
            "1. Build a short diff summary by listing the files the worker touched",
            "   and a one-line note per file (≤200 chars total). Use whatever lives",
            "   in the cwd; do NOT attempt `git diff` (no .git).",
            "",
            "2. Read state.json to confirm the reviewer's terminal status:",
            f"   - `cat {workers_dir}/state.json | jq -r '.review_alpha_status, .review_alpha_engine, .review_alpha_model, .review_alpha_rounds, .review_beta_status, .review_beta_engine, .review_beta_model, .review_beta_rounds'`",
            "   - Expected: review_alpha_status=passed and review_beta_status=passed.",
            "",
            "3. Write the terminal phase update (no --pr-url; non-git workers ship",
            "   without a PR URL):",
            "",
            f"   `fleet workers update {task.slug} {proj_flag} --phase done --exit 0`",
            "   If this terminal write is REJECTED by the review gate, do NOT loop",
            "   or retry. Run:",
            f"   `fleet workers update {task.slug} {proj_flag} --phase blocked \\",
            "     --reason \"finisher: review gate rejected — <one-line err>\"`",
            "   and exit.",
            "",
            "   The workers CLI accepts phase=done without --pr-url ONLY when the",
            "   project's meta.json declares is_git=false. If the CLI rejects the",
            "   update with ErrPhaseRequiresPR, the meta.json is mis-declared as",
            "   git — flip to blocked with that error inlined and exit.",
            "",
            f"4. Append a one-line note to tasks.md so the operator sees the diff",
            "   summary in `fleet tasks show`:",
            f"   `fleet tasks note {task.slug} --project {project} \"finisher: <diff summary>\"`",
            "",
            "5. Exit cleanly.",
            "",
            "## On failure",
            "",
            "If the `fleet workers update --phase done` call errors, do NOT retry",
            "blindly. Flip to blocked with the error inlined:",
            f"   `fleet workers update {task.slug} {proj_flag} --phase blocked \\",
            "     --reason \"finisher: <one-line error>\"`",
            "Then exit. The coord raises BLOCKED to the operator.",
            "",
            "## Hard prohibitions",
            "",
            "- Do NOT `git init` or otherwise convert the project. The operator",
            "  registered it as non-git on purpose.",
            "- Do NOT run /review or codex. The reviewer already did.",
            "- Do NOT push, open PRs, or invoke gh — there is no remote.",
            "- Do NOT skip the `fleet workers update --phase done` call.",
            "",
            "## §7 return contract (terse)",
            "",
            "Return: `non-git project; final phase=done; diff: <one-line summary>`.",
            "",
            "You have: the project directory at the worker's cwd. NO interactive chat.",
        ]
    out = "\n".join(lines)
    if len(out.encode("utf-8")) > _PROMPT_HARD_CAP_BYTES:
        raise PromptTooLargeError(
            f"finisher prompt for {task.slug} is "
            f"{len(out.encode('utf-8'))}B (cap {_PROMPT_HARD_CAP_BYTES}B)",
        )
    return out


# ----------------------------------------------------------------------
# PR-watch auto-fix subagent prompts (DESIGN-coord-pr-watch-durable §5.1c
# / §5). The coordinator tick dispatches these when a watched PR goes
# STALE/BEHIND/DIRTY (rebase) or CI-fails / CHANGES_REQUESTED (fix). Both
# are general-purpose Agent subagents that run fleet-guard (so the tick can
# read their agent record for the §6 lease outcome: blocked flag /
# liveness). They MUST set `blocked` (via the BLOCKED contract) on any
# guard abort so the lease latches blocked rather than silently re-firing.
# ----------------------------------------------------------------------


def _standards_section(standards_md: str) -> str:
    s = (standards_md or "").strip()
    return s or "(no standards configured)"


def _bound_note(action) -> str:
    """A one-line note telling the fixer which attempt of the bounded
    remediation series this is (DESIGN-pr-watch-autoremediate §2.1) — so the
    subagent knows it is attempt K of N on this signature / dispatch M of
    max_series, and that exceeding the bound escalates to the operator. Empty
    when the action carries no bound metadata (back-compat)."""
    a = getattr(action, "attempt", 0) or 0
    ma = getattr(action, "max_attempts", 0) or 0
    s = getattr(action, "series", 0) or 0
    ms = getattr(action, "max_series", 0) or 0
    if not (a and ma):
        return ""
    return (
        f"This is bounded remediation attempt {a}/{ma} on this failure "
        f"signature (dispatch {s}/{ms} since the last real progress). The "
        f"coordinator will ESCALATE to the operator once a bound is hit — do "
        f"NOT churn: if you cannot make real progress (shrink the failing "
        f"set / resolve the conflict), set yourself BLOCKED rather than "
        f"pushing a no-progress change."
    )


def _agent_record_path(agent_id: str) -> str:
    """The agent-record path a PR-watch subagent must write its remediation
    outcome into, with the minted agent_id substituted when known (codex P1:
    a register:false subagent has no other way to learn it)."""
    return (f"~/.fleet/agents/{agent_id}.json" if agent_id
            else "~/.fleet/agents/<your-agent-id>.json")


def build_rebase_prompt(action, *, standards_md: str = "", agent_id: str = "") -> str:
    """Assemble the rebase subagent's first-turn prompt (§5.1c).

    `action` is a pr_watch.ActionDispatch carrying the PR number, url,
    branch, base, the watch-snapshot head_sha, and the fresh base_sha to
    rebase onto. The guard rules (verify head unchanged, isolated worktree,
    full gates, re-check OPEN, force-with-lease only, abort+blocked on any
    conflict, NEVER auto-resolve) are spelled out verbatim — a wrong rebase
    destroys live work, so the prompt is the load-bearing guard alongside
    the §6 lease.
    """
    pr = action.pr_number
    branch = action.branch or f"worker/pr-{pr}"
    base = action.base or "main"
    lines = [
        f"You are a Fleet PR-watch REBASE subagent for PR #{pr} ({action.pr_url}).",
        "",
        "The coordinator detected this PR is the uniquely-eligible next-to-merge",
        f"PR but its head no longer contains the current {base} tip (stale under",
        "strict branch protection). Rebase it cleanly onto the fresh base.",
        "",
        *( [_bound_note(action), ""] if _bound_note(action) else [] ),
        "## Read first",
        "- your engine's GLOBAL Subagent Dispatch Contract in your engine's",
        "  config dir: claude -> CLAUDE.md under ~/.claude; codex -> AGENTS.md",
        "  under your codex config dir (e.g. ~/.codex or ~/.Codex — read",
        "  whichever your engine actually uses; the dir name's case is",
        "  host-dependent).",
        "- the project's CLAUDE.md AND/OR AGENTS.md (whichever exists) + memory.",
        "",
        "## HARD GUARDS — a wrong rebase destroys live work. Follow EXACTLY.",
        f"1. `git fetch origin {base}` ONCE. Capture the fresh base SHA.",
        f"   Expected fresh base to rebase onto: {action.base_sha or '(fetch + use origin/' + base + ')'}.",
        f"2. Work where {branch} is ALREADY checked out — do NOT `git worktree",
        f"   add` it (an in-review task keeps its worker worktree with {branch}",
        "   checked out, so a second add fails 'already checked out'). Resolve",
        "   the existing checkout and cd into it:",
        f"     `git worktree list --porcelain | grep -B2 'branch refs/heads/{branch}'`",
        "   cd into that worktree's path. ONLY if no worktree has it checked",
        f"   out, create one: `git worktree add /tmp/fleet-rebase-{pr} {branch}`",
        "   (and remove it at the end). Never operate in the coord's main",
        "   checkout.",
        f"3. VERIFY the head is UNCHANGED before touching anything: the branch",
        f"   {branch} head MUST still equal {action.head_sha}. If it moved,",
        "   ABORT immediately (the branch changed under you) — do nothing,",
        "   report BLOCKED, exit. Do not force anything onto a moved head.",
        f"4. Re-check PR #{pr} is still OPEN (`gh pr view {pr} --json state`).",
        "   If MERGED/CLOSED, clean up the worktree and exit (nothing to do).",
        f"5. Rebase onto the fresh base: `git rebase {action.base_sha or 'origin/' + base}`.",
        "   On ANY conflict: `git rebase --abort` (NEVER auto-resolve — no",
        "   --theirs/--ours, no half-resolved commits). Then DO NOT block.",
        "   You MUST write the conflict outcome into your FLEET AGENT RECORD",
        f"   ({_agent_record_path(agent_id)}) — the coordinator reads",
        "   ONLY the agent record to advance the ladder; a WIP-note-only",
        "   report is INVISIBLE and would strand the PR. Set, in the record:",
        '     "remediation_outcome": "rebase_conflicted_needs_rederive",',
        '     "conflicted_paths": [<the conflicted file paths>]',
        "   Collect the paths with `git diff --name-only --diff-filter=U`",
        "   BEFORE the abort (or from the rebase output). Then EXIT cleanly",
        "   (do NOT set yourself blocked). The coordinator's next pass",
        "   advances the ladder to a RE-DERIVE step for an unapproved PR (or",
        "   escalates a human-approved one) — a conflict here is NONTERMINAL,",
        "   not a dead end. The conflicted-path list keys the next step's",
        "   signature, so it must be in the record.",
        "6. Run the FULL project gates (a textually-clean rebase can still",
        "   break behavior): `go build ./... && go test -race -count=1 ./...`,",
        "   `gofmt -l .`, `golangci-lint run ./...`, `python3 -m pytest skills/ -q`.",
        "   On any gate failure: do NOT push. Exit clean WITHOUT pushing",
        "   (gates_failed) — the watch re-dispatches, bounded; reserve BLOCKED",
        "   for a definitive no-go you can prove.",
        "7. Run /codex review then /review until clean (CLAUDE.md §4) — EVEN",
        "   for a clean rebase: a textually-clean rebase can still change",
        "   behavior (a silent semantic conflict the merge didn't flag), so",
        "   NO push that changes the branch tip is exempt from the reviewers.",
        f"8. Re-check PR #{pr} is STILL OPEN, then push with a LEASE only:",
        f"   `git push --force-with-lease={branch}:{action.head_sha} origin {branch}`.",
        "   NEVER plain --force. If the lease push is rejected (a human merged",
        "   or pushed mid-rebase), that is a CLEAN loss — abort, do not retry,",
        "   report BLOCKED, exit. No data is destroyed.",
        "9. Clean up: if YOU created a /tmp/fleet-rebase worktree, remove it",
        "   (`git worktree remove`). Do NOT remove the task's own worker",
        "   worktree. Cleanup is the LAST step, on success AND failure paths.",
        "",
        "## On BLOCKED",
        "Set yourself blocked (so the coordinator's lease latches blocked and",
        "raises to the operator — one attempt per head/base, never silently",
        "re-fired). Preserve a WIP note describing the conflict / guard abort.",
        "",
        "## Standards",
        _standards_section(standards_md),
        "",
        "Do NOT open or merge PRs. Do NOT change the PR base. Do NOT touch any",
        "PR other than this one. Rebase + force-with-lease push only.",
    ]
    out = "\n".join(lines)
    if len(out.encode("utf-8")) > _PROMPT_HARD_CAP_BYTES:
        raise PromptTooLargeError(
            f"rebase prompt for PR #{pr} is "
            f"{len(out.encode('utf-8'))}B (cap {_PROMPT_HARD_CAP_BYTES}B)",
        )
    return out


def build_fix_prompt(action, *, standards_md: str = "", agent_id: str = "") -> str:
    """Assemble the fix subagent's first-turn prompt (§5 fix dispatch).

    Covers two events: EVENT_CI_FAILED (re-run / repair failing checks) and
    EVENT_CHANGES_REQUESTED (apply mechanical review fixes; raise on
    substantive/design asks — the substantive guard lives here because the
    review body is only readable from inside the subagent).
    """
    pr = action.pr_number
    branch = action.branch or f"worker/pr-{pr}"
    is_review = action.event == "changes-requested"
    lines = [
        f"You are a Fleet PR-watch FIX subagent for PR #{pr} ({action.pr_url}).",
        "",
        *( [_bound_note(action), ""] if _bound_note(action) else [] ),
        "## Read first",
        "- your engine's GLOBAL Subagent Dispatch Contract in your engine's",
        "  config dir: claude -> CLAUDE.md under ~/.claude; codex -> AGENTS.md",
        "  under your codex config dir (e.g. ~/.codex or ~/.Codex — read",
        "  whichever your engine actually uses; the dir name's case is",
        "  host-dependent).",
        "- the project's CLAUDE.md AND/OR AGENTS.md (whichever exists) + memory.",
        "",
        "## Guards",
        f"1. Work where {branch} is ALREADY checked out — do NOT `git worktree",
        f"   add` it (the in-review task keeps its worker worktree with {branch}",
        "   checked out; a second add fails 'already checked out'). Find it via",
        f"   `git worktree list --porcelain | grep -B2 'branch refs/heads/{branch}'`",
        "   and cd in; only create a fresh worktree if NONE has it checked out",
        "   (and remove that one at the end). Never use the coord's main",
        f"   checkout. Verify the head still equals {action.head_sha} before",
        "   pushing (if it moved, a newer attempt is in flight -> BLOCKED, exit).",
        f"2. Re-check PR #{pr} is still OPEN before and after your fix.",
        f"3. Push with `git push --force-with-lease={branch}:{action.head_sha}`",
        "   ONLY — never plain --force. A rejected lease (human pushed/merged",
        "   mid-flight) is a clean loss: abort, report BLOCKED, exit.",
        "4. Cleanup is the LAST step (success or fail): remove ONLY a /tmp",
        "   worktree YOU created — never the task's own worker worktree.",
        "",
    ]
    if is_review:
        lines += [
            "## Task — review CHANGES_REQUESTED",
            f"Read the review on PR #{pr} (`gh pr view {pr} --comments`).",
            "- MECHANICAL asks (typo, style, lint, rename, formatting): apply",
            "  them, re-run gates, push with the lease.",
            "- SUBSTANTIVE / design asks (API change, behavior change, anything",
            "  needing a judgement call): do NOT guess. Set yourself BLOCKED,",
            "  raise-hand with a summary, and exit. The operator decides.",
        ]
    else:
        lines += [
            "## Task — CI FAILURE",
            f"Fetch the failing checks (`gh pr checks {pr}` + the failing job",
            "logs) and fix the ROOT CAUSE (not a flake-retry unless the log",
            "proves a flake). Add/repair a regression test for the failure.",
            "If the failure is infra/flake only (not your diff) and a re-run is",
            "the right move, say so explicitly; if you cannot fix it, set",
            "yourself BLOCKED + raise-hand rather than pushing a guess.",
        ]
    lines += [
        "",
        "## Gates (mandatory before any push)",
        "`go build ./... && go test -race -count=1 ./...`, `gofmt -l .`,",
        "`golangci-lint run ./...`, `python3 -m pytest skills/ -q`.",
        "Run /codex review then /review until clean (CLAUDE.md §4).",
        "",
        "## BLOCKED vs clean-exit (DESIGN-pr-watch-autoremediate §2.4)",
        "Two distinct exits — pick the right one:",
        "- BLOCKED = 'I definitively cannot do this safely' (a substantive /",
        "  design review ask, a failure needing a product decision). Signal it",
        f"  by writing `\"blocked\": true` into your agent record",
        f"  ({_agent_record_path(agent_id)}) — that is the channel the",
        "  coordinator reads. It latches after ONE attempt + raises to the",
        "  operator immediately — it does NOT ride out the retry bound. Use it",
        "  only when you can PROVE it's hopeless.",
        "- Clean exit WITHOUT push = 'this attempt didn't land but isn't",
        "  definitively hopeless' (gates still red, fix didn't take). No",
        "  blocked latch; the watch re-dispatches, bounded by the retry +",
        "  series counters. Exit clean (do NOT block) in this case.",
        "",
        "## Standards",
        _standards_section(standards_md),
        "",
        "Do NOT open new PRs. Do NOT change the PR base. Do NOT touch any PR",
        "other than this one.",
    ]
    out = "\n".join(lines)
    if len(out.encode("utf-8")) > _PROMPT_HARD_CAP_BYTES:
        raise PromptTooLargeError(
            f"fix prompt for PR #{pr} is "
            f"{len(out.encode('utf-8'))}B (cap {_PROMPT_HARD_CAP_BYTES}B)",
        )
    return out


def build_rederive_prompt(action, *, standards_md: str = "", agent_id: str = "") -> str:
    """Assemble the RE-DERIVE subagent's first-turn prompt (DESIGN-pr-watch-
    autoremediate §2.2/§2.3). Reached ONLY for an UNAPPROVED DIRTY PR whose
    clean rebase conflicted. The subagent regenerates the change from the
    backing task's spec on a fresh base and force-with-lease-replaces the
    branch — it does NOT hand-edit conflict markers.

    Hard safety rails (a wrong re-derive ships code the operator didn't
    approve): re-fetch review state and REFUSE the push if a human APPROVED
    review now exists (TOCTOU); spec-availability gate (escalate if the spec
    is gone); full gates + codex/review before push; --force-with-lease only.
    """
    pr = action.pr_number
    branch = action.branch or f"worker/pr-{pr}"
    base = action.base or "main"
    slugs = list(getattr(action, "task_slugs", ()) or ())
    conflicted = list(getattr(action, "conflicted_paths", ()) or ())
    slug_hint = ", ".join(slugs) if slugs else "(no backing task slug recorded)"
    conflict_hint = ", ".join(conflicted) if conflicted else "(paths not recorded)"
    lines = [
        f"You are a Fleet PR-watch RE-DERIVE subagent for PR #{pr} ({action.pr_url}).",
        "",
        "A clean rebase of this PR onto fresh main hit a MERGE CONFLICT. The",
        "coordinator confirmed this PR carries NO human approval, so the policy",
        "is to RE-DERIVE: regenerate the change from the backing task's spec on",
        "a clean base — NOT to textually merge conflict markers.",
        "",
        *( [_bound_note(action), ""] if _bound_note(action) else [] ),
        "## Read first",
        "- your engine's GLOBAL Subagent Dispatch Contract in your engine's",
        "  config dir (claude -> ~/.claude/CLAUDE.md; codex -> AGENTS.md).",
        "- the project's CLAUDE.md AND/OR AGENTS.md (whichever exists) + memory.",
        "- the backing task spec(s) for EVERY slug below (run `fleet tasks",
        f"  show <slug>` for EACH of: {slug_hint}). This PR may be backed by",
        "  MULTIPLE tasks — you MUST reconstruct ALL of their changes; a",
        "  re-derive that regenerates only one task's spec force-pushes a",
        "  branch that silently drops the other tasks' work."
        if slugs else
        "- the backing task spec (NONE recorded — see the SPEC GATE below).",
        "",
        f"Backing task slug(s): {slug_hint}",
        f"Conflicted paths from the failed rebase: {conflict_hint}",
        "",
        "## How you signal BLOCKED",
        "Whenever a step below says 'report BLOCKED', do it by writing",
        f"`\"blocked\": true` into your agent record",
        f"({_agent_record_path(agent_id)}) — that is the ONLY channel the",
        "coordinator reads (this is a register:false dispatch). A WIP-note-only",
        "report is invisible.",
        "",
        "## SPEC GATE (DESIGN §2.2 — re-derive needs the spec)",
        "Re-derive requires the original task spec(s) to still be",
        "reconstructable (each backing task row's Spec/Acceptance in",
        "tasks.md). If ANY backing task is terminal/archived and its spec is",
        "GONE, re-derive is NOT possible: set yourself BLOCKED + raise-hand and",
        "exit. Do NOT guess the spec. The regenerated branch must express the",
        "union of ALL backing tasks' changes.",
        "",
        "## HARD GUARDS — a wrong re-derive ships unapproved code. Follow EXACTLY.",
        f"1. `git fetch origin {base}` ONCE. Capture the fresh base SHA",
        f"   (expected: {action.base_sha or 'origin/' + base}).",
        f"2. VERIFY the head is UNCHANGED: {branch} head MUST still equal",
        f"   {action.head_sha}. If it moved, ABORT (newer attempt / human",
        "   pushed) — report BLOCKED, exit.",
        f"3. Re-check PR #{pr} is still OPEN. If MERGED/CLOSED, clean up + exit.",
        "4. APPROVAL RE-CHECK (TOCTOU, §2.3): re-fetch review state",
        f"   (`gh pr view {pr} --json reviews,latestReviews`). If ANY non-bot",
        "   review is now APPROVED (a human approved while you ran), REFUSE the",
        "   re-derive: report BLOCKED + raise-hand, push NOTHING. The reviewed",
        "   diff must never be clobbered.",
        "5. Work in a FRESH isolated worktree off origin/" + base + ":",
        f"   `git worktree add /tmp/fleet-rederive-{pr} -b {branch}-rederive "
        f"origin/{base}` (or reset {branch} to origin/{base} in an isolated",
        "   checkout). NEVER the coord's main checkout. Regenerate the change",
        "   from the spec — re-express it on the clean base; do NOT merge or",
        "   rebase the stale commits, and do NOT hand-edit conflict markers",
        "   (no --theirs/--ours, no half-resolved commits).",
        "6. Run the FULL project gates: `go build ./... && go test -race",
        "   -count=1 ./...`, `gofmt -l .`, `golangci-lint run ./...`,",
        "   `python3 -m pytest skills/ -q`. On any gate failure: push NOTHING,",
        "   exit clean (gates_failed) — the watch re-dispatches, bounded.",
        "7. Run /codex review then /review until clean (CLAUDE.md §4) — a",
        "   regenerated diff goes through the same review discipline as a fresh",
        "   worker PR. If the divergence of intent is genuinely AMBIGUOUS (you",
        "   cannot produce a behavior-equivalent change that passes gates),",
        "   report BLOCKED with a summary — fleet GUESSES NOTHING on semantics.",
        "8. RE-CHECK head unchanged + PR OPEN + still no human APPROVED, THEN",
        f"   replace the branch: `git push --force-with-lease={branch}:"
        f"{action.head_sha} origin HEAD:{branch}`. NEVER plain --force. A",
        "   rejected lease (human pushed/merged mid-flight) is a CLEAN loss —",
        "   abort, report BLOCKED, exit. No data destroyed.",
        "9. SYNC THE LOCAL WORKER BRANCH to the pushed head. You worked on",
        f"   {branch}-rederive, so the task's worker worktree still has",
        f"   {branch} checked out at the OLD conflicted head. A later PR-watch",
        f"   fix/rebase finds that local {branch}, verifies it against the",
        "   GitHub head, and would BLOCK on the stale local state. So after a",
        f"   successful push, force the local {branch} to the new head — e.g.",
        "   in the worker worktree that has it checked out: `git fetch origin",
        f"   {branch} && git reset --hard origin/{branch}` (or `git branch -f",
        f"   {branch} <new-head>` if it is not checked out anywhere). The",
        "   local branch MUST end equal to the pushed head.",
        f"10. Clean up: remove the /tmp/fleet-rederive-{pr} worktree AND the",
        f"   temporary {branch}-rederive branch (`git worktree remove ...` then",
        f"   `git branch -D {branch}-rederive`). Cleanup is the LAST step, on",
        "   success AND failure paths.",
        "",
        "## Standards",
        _standards_section(standards_md),
        "",
        "Do NOT open or merge PRs. Do NOT change the PR base. Do NOT touch any",
        "PR other than this one. Re-derive + force-with-lease push only.",
    ]
    out = "\n".join(lines)
    if len(out.encode("utf-8")) > _PROMPT_HARD_CAP_BYTES:
        raise PromptTooLargeError(
            f"rederive prompt for PR #{pr} is "
            f"{len(out.encode('utf-8'))}B (cap {_PROMPT_HARD_CAP_BYTES}B)",
        )
    return out


def _select_learnings(raw: str) -> str:
    """Trim raw `fleet learnings list` output to the inline budget.

    `fleet learnings list` emits a tabwriter-aligned table with a header
    row. Drop the header, take up to N body rows, truncate each row to
    the per-entry cap. Returns "" when no entries — caller omits the
    section entirely (matches ENG §6.5).
    """
    raw = (raw or "").strip()
    if not raw:
        return ""
    lines = raw.splitlines()
    # Drop header + any "no learnings" placeholder.
    if not lines:
        return ""
    if lines[0].lstrip().startswith("WHEN"):
        lines = lines[1:]
    if not lines or "no learnings" in lines[0].lower():
        return ""
    out: list[str] = []
    for ln in lines[:_MAX_LEARNINGS_INLINED]:
        if len(ln) > _LEARNING_BODY_CAP:
            ln = ln[: _LEARNING_BODY_CAP - 1] + "…"
        out.append(ln)
    return "\n".join(out)


def mint_agent_id() -> str:
    """Generate a fresh 8-hex token for a worker subagent's agent_id.

    Phase A (issue #84) replaced the `fleet dispatch` subprocess —
    which printed the agent_id on stdout — with the Agent tool path.
    The skill now mints the agent_id itself before emitting the
    DISPATCH instruction so:

      1. The token is in tasks.md (`note "dispatched as agent <id>"`)
         and supervisor's slug→agent_id map BEFORE the coord agent
         ever calls the Agent tool. If the coord crashes between
         emit + Agent call, the next tick still has the breadcrumb.
      2. The inbox file (~/.fleet/inbox/<agent_id>.md) is the
         worker's first-turn prompt source — without a stable
         agent_id at emit time we'd have to defer the inbox write
         to a later coord turn, splitting the dispatch path in two.

    The token is `secrets.token_hex(4)` — 4 random bytes, hex-encoded
    to 8 chars. Matches Go's agent.NewID alphabet (lowercase hex) and
    cardinality (32 bits → 4.3B unique workers per coord, which
    overflows project lifetimes).
    """
    return secrets.token_hex(4)


# Strictly validate inputs to write_worker_inbox + format_dispatch_instruction.
_AGENT_ID_FULL_RE = re.compile(r"^[0-9a-f]{8}$")


def format_dispatch_instruction(
    *,
    agent_id: str,
    slug: str,
    prompt_file: str,
    description: str | None = None,
    generation: int = 0,
    register: bool = True,
) -> str:
    """Render the DISPATCH block the coord agent (Claude) will act on.

    Phase A — the Python skill cannot invoke Claude's Agent tool
    directly. Instead, /coordinator emits structured DISPATCH blocks
    on stdout and SKILL.md's "Worker dispatch protocol" section
    instructs the coord agent to invoke `Agent(...)` for each block
    on its NEXT assistant turn (one Agent call per block).

    Block format:

        DISPATCH: <slug>
          agent_id: <8hex>
          generation: <int>
          description: <short>
          prompt_file: <abs path>
          run_in_background: true
          subagent_type: general-purpose
        END_DISPATCH

    `generation` is the launch token (dispatch-durability #184). Before
    invoking the Agent the coord runs `fleet claims mark-launch-attempted
    <agent_id> <generation>`; the flip lands only if the on-disk journal
    is still at this generation — a stale re-emitted block (older
    lifecycle) carries an old gen and predicate-fails, so it cannot
    double-launch a later lifecycle. See SKILL.md "Worker dispatch
    protocol" step 2.

    description defaults to "fleet worker <slug>". prompt_file is
    the path the coord must Read; the body is then passed verbatim
    as Agent's `prompt` parameter. agent_id MUST be 8 hex chars
    (validated here so a malformed token can't make it into the
    stream the coord parses).

    Returns the raw block as a string. Caller (loop.py) bundles
    blocks together — one per dispatched task — and prints them
    BEFORE the JSON tick summary so the coord sees them as
    distinct, parseable content.
    """
    if not _AGENT_ID_FULL_RE.fullmatch(agent_id):
        raise ValueError(f"invalid agent_id {agent_id!r}: expected 8 hex chars")
    if not slug:
        raise ValueError("slug must be non-empty")
    if not prompt_file:
        raise ValueError("prompt_file must be non-empty")
    desc = (description or f"fleet worker {slug}").strip()
    lines = [
        f"DISPATCH: {slug}",
        f"  agent_id: {agent_id}",
        f"  generation: {int(generation)}",
        f"  description: {desc}",
        f"  prompt_file: {prompt_file}",
        "  run_in_background: true",
        "  subagent_type: general-purpose",
    ]
    # `register: false` marks a dispatch whose agent_id is NOT a tasks.md
    # worker slug (PR-watch auto-fix/rebase — slug is a synthetic
    # `pr-fix-<n>`/`pr-rebase-<n>` label). The coord MUST skip the
    # register_subagent.py step for it (codex iter-17 [P2]): that step
    # writes worker_subagent_ids[slug] + acks via worker_agent_ids[slug],
    # which would pollute worker state with a non-worker label and never
    # actually ack the journal. The mark-launch-attempted gate still runs
    # (the journal exists); only the post-launch worker registration is
    # skipped. Omitted (== register: true) for normal worker dispatches.
    if not register:
        lines.append("  register: false")
    lines.append("END_DISPATCH")
    return "\n".join(lines)


def write_worker_inbox(agent_id: str, prompt: str, *, fleet_home: str | None = None) -> str:
    """Drop the worker's first-turn prompt into ~/.fleet/inbox/<id>.md.

    fleet-guard's SessionStart hook reads this file and injects its
    content as `[OPERATOR] <body>` into the worker's first turn. The
    write goes through tmp + rename so a partial write never blocks
    SessionStart. Returns the inbox path on success.

    The agent_id arg must already be validated (8-hex). Caller asserts
    via DispatchResult.agent_id which came from `fleet dispatch`.
    """
    if not _AGENT_ID_FULL_RE.fullmatch(agent_id):
        raise ValueError(f"invalid agent_id {agent_id!r}: expected 8 hex chars")
    home = fleet_home or os.environ.get("FLEET_HOME") or os.path.expanduser("~/.fleet")
    inbox_dir = os.path.join(home, "inbox")
    os.makedirs(inbox_dir, exist_ok=True)
    target = os.path.join(inbox_dir, f"{agent_id}.md")
    fd, tmp = tempfile.mkstemp(prefix=f"{agent_id}.tmp.", dir=inbox_dir)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as fh:
            fh.write(prompt)
            if not prompt.endswith("\n"):
                fh.write("\n")
            fh.flush()
            os.fsync(fh.fileno())
        os.replace(tmp, target)
    except Exception:
        try:
            os.unlink(tmp)
        except FileNotFoundError:
            pass
        raise
    return target


# ----------------------------------------------------------------------
# Rolling coord checkpoint
# ----------------------------------------------------------------------
#
# The coord writes ~/.fleet/projects/<p>/coord-checkpoint.md every N
# ticks (N = FLEET_COORD_CHECKPOINT_EVERY, default 5). The checkpoint
# bounds the recovery window between fleet-guard's 50%/70% context
# handoffs: a coord that dies mid-handoff is resumable from the most
# recent checkpoint (~2.5min stale on defaults) rather than the last
# handoff doc (potentially hours stale).
#
# Schema is markdown + YAML-ish frontmatter so internal/handoff/synth.go
# can read a few key:value pairs without a full YAML parser. The four
# body sections match the handoff doc shape verbatim (Active Subagents
# uses the same 7-field key="value" rows as handoff.go's
# renderActiveSubagents) so synth.go lifts the rows into a synthetic
# handoff doc without translating.
#
# The writer reuses the same tmp + fsync + os.replace atomic-publish
# dance as write_worker_inbox above — a torn write would corrupt the
# recovery path that needs the file most.
#
#   tick loop ──every N ticks──▶ write_coord_checkpoint()
#                                      │ tmp+fsync+os.replace
#                                      ▼
#                          coord-checkpoint.md
#                                      │ coord dies; successor dispatch
#                                      ▼
#       synth.go SynthesizeRecoveryWithLastHandoff() prefers it if newer
#
# Env knobs:
#   FLEET_COORD_CHECKPOINT_EVERY      (int >= 0, default 5; 0 disables)
#   FLEET_COORD_CHECKPOINT_DECISIONS  (int >= 0, default 10)

_CHECKPOINT_DEFAULT_EVERY = 5
_CHECKPOINT_DEFAULT_DECISIONS = 10

# Body placeholders — kept byte-identical with handoff.go's
# ActiveSubagentsNonePlaceholder / OpenPRsNonePlaceholder and the
# seedCheckpoint fixture in internal/handoff/synth_test.go. synth.go
# slices the section bodies by these literal strings.
_CHECKPOINT_ACTIVE_PLACEHOLDER = "_(none)_"
_CHECKPOINT_OPEN_PRS_PLACEHOLDER = "_(no open PRs)_"
_CHECKPOINT_DECISIONS_PLACEHOLDER = "_(no recent decisions)_"
# Slice 2: byte-identical to synth.go's parseCheckpointCompletions
# short-circuit constant so an empty Completed (recent) buffer round-trips
# to nil (not a literal-bullet artifact). Mirrors the decisions placeholder.
_CHECKPOINT_COMPLETIONS_PLACEHOLDER = "_(no recent completions)_"
_CHECKPOINT_DRAFTED_PLACEHOLDER = "_(empty — populated in Phase 2)_"


def resolve_checkpoint_every() -> int:
    """Read FLEET_COORD_CHECKPOINT_EVERY; default 5 on unset / invalid.

    Accepts non-negative ints. Negative values (operator misconfig) and
    non-int strings fall back to the default rather than silently
    flipping to "checkpoint every tick" (which would amplify the
    misconfig into disk pressure). 0 is valid: it disables checkpointing
    entirely (see should_checkpoint).
    """
    raw = os.environ.get("FLEET_COORD_CHECKPOINT_EVERY")
    if raw is None:
        return _CHECKPOINT_DEFAULT_EVERY
    try:
        n = int(raw)
    except (TypeError, ValueError):
        return _CHECKPOINT_DEFAULT_EVERY
    if n < 0:
        return _CHECKPOINT_DEFAULT_EVERY
    return n


def resolve_checkpoint_decisions() -> int:
    """Read FLEET_COORD_CHECKPOINT_DECISIONS; default 10 on unset/invalid.

    Caps the recent_decisions buffer so a long-running coord doesn't
    grow the checkpoint past readable size.
    """
    raw = os.environ.get("FLEET_COORD_CHECKPOINT_DECISIONS")
    if raw is None:
        return _CHECKPOINT_DEFAULT_DECISIONS
    try:
        n = int(raw)
    except (TypeError, ValueError):
        return _CHECKPOINT_DEFAULT_DECISIONS
    if n < 0:
        return _CHECKPOINT_DEFAULT_DECISIONS
    return n


def should_checkpoint(tick_count: int, every: int) -> bool:
    """Return True when this tick should write a checkpoint.

    Gate: tick_count > 0 AND every > 0 AND tick_count % every == 0.

    tick_count=0 is the pre-first-tick state — no checkpoint until the
    coord has actually run a tick. every=0 disables checkpointing
    entirely (operator override via FLEET_COORD_CHECKPOINT_EVERY=0).
    """
    if tick_count <= 0:
        return False
    if every <= 0:
        return False
    return tick_count % every == 0


def record_checkpoint_decision(state: dict, line: str) -> None:
    """Append `line` to state["recent_decisions"], capped to the
    FLEET_COORD_CHECKPOINT_DECISIONS limit. Mutates `state` in place.

    Tolerates a state dict that has never carried a recent_decisions key
    (fresh coord first tick) or one whose value is corrupt (non-list).
    Blank / whitespace-only entries are dropped; embedded newlines are
    flattened to spaces so the bullet-per-line markdown contract holds.
    """
    if line is None:
        return
    flat = str(line).replace("\r", "\n").replace("\n", " ").strip()
    if not flat:
        return
    cap = resolve_checkpoint_decisions()
    raw = state.get("recent_decisions")
    if not isinstance(raw, list):
        raw = []
    raw.append(flat)
    if cap > 0 and len(raw) > cap:
        raw = raw[-cap:]
    state["recent_decisions"] = raw


# _SESSION_TASKS_MAX caps coord-state.json:session_tasks — the auto Next
# Steps buffer (promoted/dispatched slugs). The coord TICK is the SOLE
# writer of session_tasks (codex iter-11 [P1]: a Go CLI write would spoof
# the coord-state.json-mtime heartbeat coordStateFresh reads); the Go side
# is read-only (internal/handoff collectors).
_SESSION_TASKS_MAX = 30


def record_session_task(state: dict, slug: str, coord_id: str) -> None:
    """Append {slug, coord_id, ts} to state["session_tasks"] — the auto Next
    Steps buffer the handoff renders SESSION-SCOPED. Mutates `state` in place.

    Dedupe by slug: a slug promoted then dispatched appears ONCE, its
    coord_id/ts refreshed to the latest action and the entry moved to the
    tail. Capped to _SESSION_TASKS_MAX (newest kept). A blank slug is dropped.
    Tolerates a state dict that has never carried session_tasks or one whose
    value is corrupt (non-list).

    The entry JSON keys ({"slug","coord_id","ts"}) match the Go READER's
    struct tags (internal/handoff collect.go sessionTask) so the tick's
    writes round-trip through CollectNextSteps / CollectOpenQuestions.
    """
    if slug is None:
        return
    slug = str(slug).strip()
    if not slug:
        return
    raw = state.get("session_tasks")
    if not isinstance(raw, list):
        raw = []
    # Drop any prior entry for this slug so coord_id/ts refresh AND the entry
    # moves to the tail (newest).
    kept = [
        e for e in raw
        if not (isinstance(e, dict) and e.get("slug") == slug)
    ]
    kept.append({
        "slug": slug,
        "coord_id": coord_id or "",
        "ts": datetime.now(timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z"),
    })
    if len(kept) > _SESSION_TASKS_MAX:
        kept = kept[-_SESSION_TASKS_MAX:]
    state["session_tasks"] = kept


def record_checkpoint_completion(state: dict, line: str) -> None:
    """Append `line` to state["recent_completions"], capped to the
    FLEET_COORD_CHECKPOINT_DECISIONS limit (shared cap). Mutates `state`
    in place.

    Clone of record_checkpoint_decision: same cap / flatten / tolerance
    discipline. Both buffers render in the handoff doc: recent_decisions
    → "Key Decisions" (fed by loop.py's tick auto-producer AND the agent's
    out-of-band `fleet checkpoint decision`; the Go CLI is a second
    writer), recent_completions → "Completed". THIS buffer is wired in
    loop.py to two TRUE completion deltas — the reconcile done-transition
    and the PR-merged flip. dispatch / worker_failed are EXCLUDED: a start or
    a requeue is not a completion (it would tell the successor that
    in-flight/failed work is done). See DESIGN-handoff-manual-doc-
    enrichment.md Slice 2 (a).
    """
    if line is None:
        return
    flat = str(line).replace("\r", "\n").replace("\n", " ").strip()
    if not flat:
        return
    cap = resolve_checkpoint_decisions()
    raw = state.get("recent_completions")
    if not isinstance(raw, list):
        raw = []
    raw.append(flat)
    if cap > 0 and len(raw) > cap:
        raw = raw[-cap:]
    state["recent_completions"] = raw


def write_coord_checkpoint(
    *,
    project_dir,
    coord_id: str,
    project: str,
    state: dict,
    active_subagents: list[dict],
    open_prs: list[dict],
    now: datetime | None = None,
) -> str:
    """Atomically publish coord-checkpoint.md under project_dir.

    Returns the absolute path written (project_dir/coord-checkpoint.md).

    state: the coord-state.json dict. tick_count + recent_decisions +
        recent_completions are read out for the frontmatter + the Recent
        decisions / Completed (recent) sections.
    active_subagents: list of dicts with keys task, branch, phase,
        status, pr_url, agent_id, subagent_id. Rendered with the same
        7-field key="value" shape as handoff.go's renderActiveSubagents
        so synth.go copies the lines verbatim.
    open_prs: list of dicts with keys number, title, head, url. Rendered
        as `- #N title — head — url` bullets, matching handoff.go.
    now: override timestamp for deterministic tests; defaults to UTC now.

    Atomic write: tmp + fsync + os.replace in the same directory as the
    target (mirrors write_worker_inbox). A crash mid-write leaves the
    prior coord-checkpoint.md (if any) intact and no stray .tmp.* litter.
    """
    target_dir = os.fspath(project_dir)
    os.makedirs(target_dir, exist_ok=True)
    target = os.path.join(target_dir, "coord-checkpoint.md")

    if now is None:
        now = datetime.now(timezone.utc)
    # RFC3339 with Z (UTC) suffix — match the handoff doc style so the
    # synth.go time.Parse(time.RFC3339, ...) accepts it.
    updated_at = now.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")

    raw_tick = state.get("tick_count", 0)
    try:
        tick_count = int(raw_tick)
    except (TypeError, ValueError):
        tick_count = 0

    raw_decisions = state.get("recent_decisions")
    if isinstance(raw_decisions, list):
        decisions = [str(d) for d in raw_decisions if isinstance(d, str) and d.strip()]
    else:
        decisions = []

    raw_completions = state.get("recent_completions")
    if isinstance(raw_completions, list):
        completions = [str(c) for c in raw_completions if isinstance(c, str) and c.strip()]
    else:
        completions = []

    body = _render_checkpoint(
        coord_id=coord_id,
        project=project,
        updated_at=updated_at,
        tick_count=tick_count,
        active_subagents=active_subagents,
        open_prs=open_prs,
        decisions=decisions,
        completions=completions,
    )

    fd, tmp = tempfile.mkstemp(prefix="coord-checkpoint.md.tmp.", dir=target_dir)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as fh:
            fh.write(body)
            fh.flush()
            os.fsync(fh.fileno())
        os.replace(tmp, target)
    except Exception:
        try:
            os.unlink(tmp)
        except FileNotFoundError:
            pass
        raise
    return target


def _render_checkpoint(
    *,
    coord_id: str,
    project: str,
    updated_at: str,
    tick_count: int,
    active_subagents: list[dict],
    open_prs: list[dict],
    decisions: list[str],
    completions: list[str],
) -> str:
    parts: list[str] = []
    parts.append("---\n")
    parts.append("schema: v1\n")
    parts.append(f'coord_id: "{coord_id}"\n')
    parts.append(f'project: "{project}"\n')
    parts.append(f'updated_at: "{updated_at}"\n')
    parts.append(f"tick_count: {tick_count}\n")
    parts.append("---\n\n")

    parts.append("### Active Subagents\n")
    parts.append(_render_checkpoint_active(active_subagents))
    parts.append("\n\n")

    parts.append("### Open PRs\n")
    parts.append(_render_checkpoint_open_prs(open_prs))
    parts.append("\n\n")

    parts.append("### Recent decisions\n")
    parts.append(_render_checkpoint_decisions(decisions))
    parts.append("\n\n")

    # Slice 2: Completed (recent) sits AFTER Recent decisions and BEFORE
    # Drafted but unfiled tasks (test_write_emits_sections_in_order pins
    # the order; synth.go lifts this section → doc.Completed).
    parts.append("### Completed (recent)\n")
    parts.append(_render_checkpoint_completions(completions))
    parts.append("\n\n")

    parts.append("### Drafted but unfiled tasks\n")
    parts.append(_CHECKPOINT_DRAFTED_PLACEHOLDER + "\n")

    return "".join(parts)


def _render_checkpoint_active(subs: list[dict]) -> str:
    if not subs:
        return _CHECKPOINT_ACTIVE_PLACEHOLDER
    lines: list[str] = []
    for s in subs:
        # Mirrors handoff.go:renderActiveSubagents — 7 fields, each value
        # Go-strconv.Quote-escaped so synth.go's strconv.Unquote round-
        # trips it.
        line = (
            f'- task="{_checkpoint_q(s.get("task"))}" '
            f'branch="{_checkpoint_q(s.get("branch"))}" '
            f'phase="{_checkpoint_q(s.get("phase"))}" '
            f'status="{_checkpoint_q(s.get("status"))}" '
            f'pr_url="{_checkpoint_q(s.get("pr_url"))}" '
            f'agent_id="{_checkpoint_q(s.get("agent_id"))}" '
            f'subagent_id="{_checkpoint_q(s.get("subagent_id"))}"'
        )
        lines.append(line)
    return "\n".join(lines)


def _render_checkpoint_open_prs(prs: list[dict]) -> str:
    if not prs:
        return _CHECKPOINT_OPEN_PRS_PLACEHOLDER
    lines: list[str] = []
    for p in prs:
        number = p.get("number", 0)
        title = _checkpoint_flatten(p.get("title"))
        head = _checkpoint_flatten(p.get("head"))
        url = _checkpoint_flatten(p.get("url"))
        lines.append(f"- #{number} {title} — {head} — {url}")
    return "\n".join(lines)


def _render_checkpoint_decisions(decisions: list[str]) -> str:
    if not decisions:
        return _CHECKPOINT_DECISIONS_PLACEHOLDER
    return "\n".join(f"- {d}" for d in decisions)


def _render_checkpoint_completions(completions: list[str]) -> str:
    if not completions:
        return _CHECKPOINT_COMPLETIONS_PLACEHOLDER
    return "\n".join(f"- {c}" for c in completions)


def _checkpoint_q(v) -> str:
    """Escape a value for a `key="..."` field, matching Go strconv.Quote
    precedence (backslash first, then double-quote). None / non-string
    falls back to "" so the field renders as `key=""`.
    """
    if v is None:
        return ""
    s = str(v)
    s = s.replace("\\", "\\\\")
    s = s.replace('"', '\\"')
    s = s.replace("\n", "\\n")
    s = s.replace("\r", "\\r")
    return s


def _checkpoint_flatten(v) -> str:
    if v is None:
        return ""
    return str(v).replace("\n", " ").replace("\r", " ")


class AcquirePromptError(RuntimeError):
    """Raised when `fleet claims acquire-prompt` returns a non-success
    outcome (anything other than `acquired` or `already_acquired`).

    DESIGN-dispatch-lifecycle.md §"Acquire is Go-side; Python shells
    out via fleet claims" — the Python side has no way to mutate the
    journal atomically, so claim acquisition MUST shell out to the Go
    binary. This exception type lets loop.py distinguish "transient
    Go-side failure (rerun next tick)" from "malformed local state
    (don't retry blindly)".

    Attributes:
        outcome: the JSON `outcome` field from the CLI response, or
            "error" when no JSON envelope was returned (e.g., binary
            missing).
        exit_code: the CLI exit code (0 for success outcomes; 10/11/12/1
            for failure shapes per the stable exit-code table).
        message: human-readable error text (CLI's `error` field, or
            stderr fallback).
    """

    def __init__(self, outcome: str, exit_code: int, message: str) -> None:
        super().__init__(message)
        self.outcome = outcome
        self.exit_code = exit_code
        self.message = message


def acquire_coord_prompt_inbox(
    agent_id: str,
    prompt: str,
    *,
    owner: str,
    dispatch_kind: str = "worker",
    host_id: str | None = None,
    tmux_socket: str | None = None,
    fleet_bin: str = "fleet",
    fleet_home: str | None = None,
    timeout_s: float = 10.0,
) -> str:
    """Acquire a coord_prompt_inbox Delivery claim via `fleet claims`.

    Replaces the direct call to `write_worker_inbox` for the two
    loop.py call sites that produce coord_prompt_inbox files (worker
    dispatch + reviewer/finisher handoff). DESIGN-dispatch-lifecycle.md
    PR1 migrates ONLY these two call sites; the helper `write_worker_inbox`
    itself stays in PR1 because handoff_resume.py:366 still uses it
    (retires in PR2 via the Rewrite controller op).

    Flow:
      1. Mint dispatch_id == agent_id (the load-bearing invariant).
      2. Shell out: `fleet claims acquire-prompt <agent_id> --owner=<o>
         --host-id=<h> [--tmux-socket=<s>] --dispatch-kind=<k>` with
         the prompt piped on stdin.
      3. Parse the JSON envelope from stdout.
      4. On `acquired` or `already_acquired`: return the inbox path
         (`response["path"]`).
      5. On anything else: raise AcquirePromptError carrying the
         outcome + exit code + message so loop.py can decide whether
         to defer the slug to the next tick.

    Args mirror the controller's surface area:
      agent_id:      8-hex (validated locally to fail-fast before subprocess).
      prompt:        the file body.
      owner:         the task slug or owner label recorded in the journal.
      dispatch_kind: "worker" | "reviewer" | "finisher".
      host_id:       defaults to socket.gethostname().
      tmux_socket:   carried for forward-compat (PR2 uses it for
                     same-host different-socket discrimination).
      fleet_bin:     override the `fleet` binary path (tests).
      fleet_home:    override FLEET_HOME (tests).
      timeout_s:     subprocess timeout.

    Returns the absolute inbox path. Raises AcquirePromptError on
    non-success.
    """
    if not _AGENT_ID_FULL_RE.fullmatch(agent_id):
        raise ValueError(f"invalid agent_id {agent_id!r}: expected 8 hex chars")

    cmd = [fleet_bin, "claims", "acquire-prompt", agent_id, "--owner", owner,
           "--dispatch-kind", dispatch_kind]
    if host_id:
        cmd += ["--host-id", host_id]
    if tmux_socket:
        cmd += ["--tmux-socket", tmux_socket]

    env = os.environ.copy()
    if fleet_home:
        env["FLEET_HOME"] = fleet_home

    try:
        proc = subprocess.run(
            cmd,
            input=prompt,
            capture_output=True,
            text=True,
            timeout=timeout_s,
            check=False,
            env=env,
        )
    except FileNotFoundError as exc:
        raise AcquirePromptError(
            "error", 1,
            f"fleet binary not found: {exc}",
        ) from exc
    except subprocess.TimeoutExpired as exc:
        raise AcquirePromptError(
            "error", 1,
            f"fleet claims acquire-prompt timed out after {timeout_s}s",
        ) from exc

    response = _parse_claims_response(proc.stdout)
    outcome = response.get("outcome", "")
    if outcome in {"acquired", "already_acquired"}:
        path = response.get("path", "")
        if not path:
            raise AcquirePromptError(
                outcome, proc.returncode,
                "claims response missing 'path' field",
            )
        return path
    # Failure outcomes (or `error`, or empty stdout).
    message = response.get("error") or (proc.stderr or "").strip() or (
        f"fleet claims acquire-prompt returned outcome={outcome!r} "
        f"exit={proc.returncode}"
    )
    raise AcquirePromptError(outcome or "error", proc.returncode, message)


# Stable outcome strings returned by `fleet claims release` (see
# cmd/fleet/claims.go + internal/dispatch). `released` and
# `already_released` are both success (idempotent); `absent` /
# `not_owned` are non-fatal terminal-race outcomes; everything else
# is an `error` shape the caller logs and otherwise ignores. The
# release helper never raises — terminal cleanup must not block task
# progression on a transient `fleet` failure.
RELEASE_OUTCOME_RELEASED = "released"
RELEASE_OUTCOME_ALREADY_RELEASED = "already_released"
RELEASE_OUTCOME_ABSENT = "absent"
RELEASE_OUTCOME_NOT_OWNED = "not_owned"
RELEASE_OUTCOME_ERROR = "error"


def release_coord_prompt_inbox(
    agent_id: str,
    *,
    host_id: str | None = None,
    preserve: bool = False,
    fleet_bin: str = "fleet",
    fleet_home: str | None = None,
    timeout_s: float = 10.0,
) -> dict:
    """Release a coord_prompt_inbox Delivery claim via `fleet claims release`.

    Symmetric to `acquire_coord_prompt_inbox`. DESIGN-dispatch-lifecycle.md
    PR1 promises "Terminal-transition reclaim releases the inbox"; this
    helper wires the Python coord half. Best-effort by design: never
    raises. The returned dict carries the parsed CLI envelope with an
    `outcome` field; callers (loop.py wrappers) log non-success
    outcomes but do NOT abort the terminal transition. Rationale: the
    inbox/journal leak is a defense-in-depth problem the PR4 sweeper
    also catches; a terminal status flip blocked by a `fleet claims`
    failure would be far worse than a stale inbox file.

    Flow:
      1. Validate agent_id locally. Malformed → return synthetic error
         envelope (no subprocess).
      2. Shell out: `fleet claims release <agent_id> --kind=coord_prompt_inbox
         [--host-id=<h>] [--preserve]`.
      3. Parse the JSON envelope from stdout.
      4. Return the dict. Caller branches on `outcome`.

    Outcomes the helper passes through:
      - released         → claim was live, file unlinked (or archived
                            when --preserve), journal updated to released.
      - already_released → claim was already released (idempotent
                            success, e.g., supervisor + primary tick
                            race).
      - absent           → no claim/journal on disk (e.g., a worker that
                            never actually dispatched, or the file was
                            manually removed). Non-fatal.
      - not_owned        → cross-host refusal. Non-fatal: another host's
                            coord owns the claim; we don't touch it.
      - error            → CLI failed (binary missing, write race). The
                            caller logs and continues; PR4 sweeper will
                            reconcile any leaked file.

    Args:
      agent_id:   8-hex dispatch_id (== worker agent_id).
      host_id:    optional override; defaults to fleet's HostID() on
                  the Go side when omitted.
      preserve:   when True, archive the inbox instead of unlinking
                  (overrides the value recorded at acquire-time).
      fleet_bin:  binary path (tests).
      fleet_home: FLEET_HOME override (tests).
      timeout_s:  subprocess timeout.

    Returns the envelope dict. Always contains an `outcome` key; a
    synthetic `{"outcome": "error", "error": "<msg>"}` is returned
    for local failures (bad agent_id, FileNotFoundError, timeout)
    rather than raising.
    """
    if not _AGENT_ID_FULL_RE.fullmatch(agent_id):
        return {
            "outcome": RELEASE_OUTCOME_ERROR,
            "error": f"invalid agent_id {agent_id!r}: expected 8 hex chars",
        }

    cmd = [fleet_bin, "claims", "release", agent_id,
           "--kind", "coord_prompt_inbox"]
    if host_id:
        cmd += ["--host-id", host_id]
    if preserve:
        cmd += ["--preserve"]

    env = os.environ.copy()
    if fleet_home:
        env["FLEET_HOME"] = fleet_home

    try:
        proc = subprocess.run(
            cmd,
            capture_output=True,
            text=True,
            timeout=timeout_s,
            check=False,
            env=env,
        )
    except FileNotFoundError as exc:
        return {
            "outcome": RELEASE_OUTCOME_ERROR,
            "error": f"fleet binary not found: {exc}",
        }
    except subprocess.TimeoutExpired:
        return {
            "outcome": RELEASE_OUTCOME_ERROR,
            "error": f"fleet claims release timed out after {timeout_s}s",
        }

    response = _parse_claims_response(proc.stdout)
    if not response:
        # Empty / unparseable stdout — synthesize the error envelope so
        # callers always have an outcome to branch on.
        stderr = (proc.stderr or "").strip()
        return {
            "outcome": RELEASE_OUTCOME_ERROR,
            "error": (
                stderr
                or f"fleet claims release returned no envelope (exit={proc.returncode})"
            ),
        }
    # Pass through whatever outcome the CLI emitted, even unknown ones —
    # the caller's `outcome in {...}` check is the routing decision.
    return response


# ============================================================
# dispatch-durability (#184) launch-state helpers
# ============================================================
#
# Python shells out to `fleet claims <subcmd>` — the journal is Go-owned
# (only Go takes the per-id flock; a Python tmp+rename would race the Go
# writers). These helpers parse the JSON envelope and return the
# `outcome` (+ `generation` where present). They never write the journal
# directly. See durability.go for the underlying state machine.


def mark_launch_attempted(
    agent_id: str,
    generation: int,
    *,
    fleet_bin: str = "fleet",
    fleet_home: str | None = None,
    timeout_s: float = 10.0,
) -> str:
    """Run `fleet claims mark-launch-attempted <id> <gen>` — the tri-state
    CAS the coord runs IMMEDIATELY before invoking the host Agent.

    Returns the outcome string the coord MUST branch on:
      - "ok"             → flip landed; LAUNCH the Agent.
      - "predicate_fail" → not pending / stale gen; SKIP, do NOT launch.
      - "contention"     → flock deadline; TRANSIENT; retry SAME block
                           next tick; NEVER treat as skip.
      - "error"          → unexpected (binary missing / bad input); the
                           coord logs + retries next tick (fail-safe:
                           treated like contention by the caller, NOT a
                           skip).
    """
    out = _run_claims_simple(
        [fleet_bin, "claims", "mark-launch-attempted", agent_id, str(generation)],
        fleet_home=fleet_home, timeout_s=timeout_s,
    )
    return out.get("outcome", "error")


def mark_acked(
    agent_id: str,
    *,
    fleet_bin: str = "fleet",
    fleet_home: str | None = None,
    timeout_s: float = 10.0,
) -> str:
    """Run `fleet claims mark-acked <id>` — best-effort flip
    launch_attempted → acked. Returns the outcome ("acked" / "contention"
    / "error"). Never raises; a failed ack just leaves the entry at
    launch_attempted (residual-crash repair handles a never-acked
    launch)."""
    out = _run_claims_simple(
        [fleet_bin, "claims", "mark-acked", agent_id],
        fleet_home=fleet_home, timeout_s=timeout_s,
    )
    return out.get("outcome", "error")


def reserve_replay(
    agent_id: str,
    *,
    cap: int = 5,
    fleet_bin: str = "fleet",
    fleet_home: str | None = None,
    timeout_s: float = 10.0,
) -> dict:
    """Run `fleet claims reserve-replay <id> --cap N` — the tick-entry
    replay primitive. Increments the durable replay counter under the
    flock BEFORE the block reaches output.

    Returns a dict {"outcome": ..., "generation": int|None}:
      - "reserved"    → re-emit the block stamped with `generation`.
      - "capped"      → cap hit; journal flipped to ExecBlocked (caller
                        escalates off-channel).
      - "not_pending" → not ExecPending; do NOT replay.
      - "absent"      → no journal; nothing to replay.
      - "contention"  → flock deadline; retry next tick.
      - "error"       → unexpected; caller logs + skips this id this tick.
    """
    out = _run_claims_simple(
        [fleet_bin, "claims", "reserve-replay", agent_id, "--cap", str(cap)],
        fleet_home=fleet_home, timeout_s=timeout_s,
    )
    return {
        "outcome": out.get("outcome", "error"),
        "generation": out.get("generation"),
    }


def reset_for_relaunch(
    agent_id: str,
    prompt: str,
    *,
    fleet_bin: str = "fleet",
    fleet_home: str | None = None,
    timeout_s: float = 10.0,
) -> dict:
    """Run `fleet claims reset-for-relaunch <id>` with the new prompt
    piped on stdin — re-arm an EXISTING dispatch (handoff_resume).

    Under one flock the Go side rewrites the inbox + resets the entry to
    a fresh ExecPending with a bumped generation. Returns
    {"outcome": ..., "generation": int|None, "path": str}:
      - "reset"      → entry re-armed; emit the resume block stamped with
                       the NEW `generation`.
      - "absent"     → no journal; caller should acquire fresh instead.
      - "contention" → flock deadline; retry next tick.
      - "error"      → unexpected.
    """
    out = _run_claims_input(
        [fleet_bin, "claims", "reset-for-relaunch", agent_id],
        prompt, fleet_home=fleet_home, timeout_s=timeout_s,
    )
    return {
        "outcome": out.get("outcome", "error"),
        "generation": out.get("generation"),
        "path": out.get("path", ""),
    }


def _run_claims_simple(
    cmd: list[str],
    *,
    fleet_home: str | None,
    timeout_s: float,
) -> dict:
    """Run a `fleet claims` subcommand (no stdin) and return the parsed
    JSON envelope. On any subprocess failure return {"outcome": "error"}
    — these helpers are best-effort; the coord retries on the next tick.
    """
    env = os.environ.copy()
    if fleet_home:
        env["FLEET_HOME"] = fleet_home
    try:
        proc = subprocess.run(
            cmd, capture_output=True, text=True, timeout=timeout_s,
            check=False, env=env,
        )
    except (FileNotFoundError, subprocess.TimeoutExpired):
        return {"outcome": "error"}
    parsed = _parse_claims_response(proc.stdout)
    return parsed or {"outcome": "error"}


def _run_claims_input(
    cmd: list[str],
    stdin_body: str,
    *,
    fleet_home: str | None,
    timeout_s: float,
) -> dict:
    """Like _run_claims_simple but pipes stdin_body on stdin (for
    reset-for-relaunch, which reads the new prompt from stdin)."""
    env = os.environ.copy()
    if fleet_home:
        env["FLEET_HOME"] = fleet_home
    try:
        proc = subprocess.run(
            cmd, input=stdin_body, capture_output=True, text=True,
            timeout=timeout_s, check=False, env=env,
        )
    except (FileNotFoundError, subprocess.TimeoutExpired):
        return {"outcome": "error"}
    parsed = _parse_claims_response(proc.stdout)
    return parsed or {"outcome": "error"}


def _parse_claims_response(stdout: str) -> dict:
    """Parse the last JSON envelope from `fleet claims` stdout.

    The CLI emits one JSON object per call. Defensive: if the CLI ever
    interleaves log noise (it doesn't today), we still pick the last
    well-formed JSON object so the envelope contract is robust to
    that future state.
    """
    if not stdout:
        return {}
    last_line = stdout.strip().splitlines()[-1] if stdout.strip() else ""
    try:
        parsed = json.loads(last_line)
    except json.JSONDecodeError:
        return {}
    return parsed if isinstance(parsed, dict) else {}


def fetch_standards(project: str, *, fleet_bin: str = "fleet", timeout_s: float = 10.0) -> str:
    """Run `fleet standards show --merged` and return stdout.

    Empty result (no standards configured) returns "". Errors return ""
    too — the worker prompt assembly tolerates missing standards by
    inlining a placeholder. Coord logs the error via loop.py.
    """
    return _run_capture(
        [fleet_bin, "standards", "show", "--merged", "--project", project],
        timeout_s=timeout_s,
    )


def fetch_learnings(
    project: str,
    *,
    limit: int = 20,
    fleet_bin: str = "fleet",
    timeout_s: float = 10.0,
) -> str:
    """Run `fleet learnings list --limit=N` and return stdout."""
    return _run_capture(
        [
            fleet_bin, "learnings", "list",
            "--project", project,
            "--limit", str(limit),
        ],
        timeout_s=timeout_s,
    )


def _run_capture(cmd: list[str], *, timeout_s: float) -> str:
    """Run cmd; return stdout on exit-0, "" otherwise.

    Errors swallowed deliberately — caller (loop.py) tolerates absent
    standards/learnings and inlines placeholders. Real failures surface
    on the actual `fleet dispatch` call which is non-recoverable.
    """
    try:
        proc = subprocess.run(
            cmd, capture_output=True, text=True, timeout=timeout_s,
            check=False,
        )
    except (FileNotFoundError, subprocess.TimeoutExpired):
        return ""
    if proc.returncode != 0:
        return ""
    return proc.stdout or ""
