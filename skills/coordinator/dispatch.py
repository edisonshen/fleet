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

import parse


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
    is_git: bool = True,
    coord_engine: str | None = None,
) -> str:
    """Assemble the reviewer subagent's first-turn prompt.

    Three-stage flow (reviewer-subagent-arch): after the worker writes
    phase=review-pending and exits, the coord dispatches THIS subagent
    against the same worker dir + branch. The reviewer iterates /review
    + codex against the worker's diff and records terminal review
    status before flipping phase=review-done. The finisher subagent
    (build_finisher_prompt) picks up from there.

    Contract this prompt enforces (matches CLAUDE.md §4):
      - /review (gstack skill) is MANDATORY — never skippable. Loop
        until two consecutive clean passes.
      - codex review is single-attempt with one 60s retry. On
        persistent rate-limit, record review_codex_status=skipped
        with --review-codex-skip-reason rate-limited. ANY other skip
        reason is rejected by the workers CLI.
      - Per-iteration fixes are committed (`fix: review iter-N — <line>`)
        on the worker's branch. No squashing.
      - Final action: `fleet workers update <slug> --phase review-done`
        with terminal --review-claude-status and --review-codex-status
        flags. Then exit. The reviewer does NOT push the branch and
        does NOT open the PR — the finisher subagent handles those.

    coord_engine: the engine the coord session was launched with
                  (claude-code | codex). Defaults to FLEET_ENGINE env or
                  claude-code. APPROACH A (memory project_codex_multi_
                  engine.md): regardless of coord_engine, the reviewer
                  subagent ALWAYS runs claude. When the coord is codex,
                  the worker + finisher subagents also run codex; the
                  reviewer pinch-hits as claude for cross-engine
                  diversity (different model, different blind spots).
                  The prompt body is identical for both cases — it's
                  always written for a claude reviewer running
                  /review + codex against the worker diff — but a
                  banner up top documents the diversity setup so the
                  reviewer subagent understands the role split.
    """
    if branch is None:
        branch = f"worker/{task.slug}"
    if workers_dir is None:
        workers_dir = f"~/.fleet/projects/{project}/workers/{task.slug}"
    if coord_engine is None:
        coord_engine = os.environ.get("FLEET_ENGINE", "") or ENGINE_CLAUDE_CODE

    proj_flag = f"--project {project}"
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
    # (claude). When coord = claude (the default), worker + reviewer
    # are both claude; the codex review step inside the loop covers the
    # cross-engine angle. Either way, the reviewer subagent process
    # itself is always claude — that's the structural decision Approach
    # A locks in for the v0.9 MVP. Banner applies to both git and
    # non-git modes — codex coord can run against either project type.
    if coord_engine == ENGINE_CODEX:
        engine_banner = [
            "Cross-engine review diversity (coord engine = codex):",
            "  The worker subagent that wrote the diff was running CODEX.",
            "  You are running CLAUDE as the second-opinion reviewer —",
            "  same role split the operator gets when coord is claude and",
            "  the inline `codex review` step provides the cross-engine",
            "  view, just reversed. Treat the worker's commits as you",
            "  would any other diff and rely on /review + codex review.",
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
        "phase=review-pending. Your job is to run /review and codex review on",
        handoff_summary,
        "",
    ]
    lines.extend(state_block)
    lines.append("")
    lines.append("## Required workflow")
    lines.append("")

    if is_git:
        lines.extend([
            f"1. `git checkout {branch}` — make sure you're on the worker's branch.",
            "   The worker's commits are already there; you append review fixes on",
            "   top.",
            "",
            f"2. /review iteration loop (gstack skill, MANDATORY — never skippable):",
            "   - Invoke `/review` via the Skill tool (literal name `review`).",
            "   - On any [P0] or [P1] finding: fix it, write a regression test,",
            f"     `git commit -m \"fix: review iter-N — <one line>\"`, then re-run",
            "     `/review`.",
            "   - Loop until `/review` returns clean in TWO consecutive runs (the",
            "     second confirms the last fix didn't introduce a regression).",
            "   - Track the rounds count — you'll pass it as --review-claude-rounds.",
            f"   - Update phase mid-loop: `fleet workers update {task.slug} {proj_flag} \\",
            "       --phase review-claude --review-claude-status iterating`",
            "     (idempotent; helps the operator see live progress).",
            "",
            f"3. codex review (single attempt + ONE 60s retry on rate-limit):",
            "   - Run `codex review --base origin/" + base_branch + " -c 'model_reasoning_effort=\"high\"' --enable web_search_cached < /dev/null 2>/tmp/codex-err.txt`.",
            "   - On [P0]/[P1] findings: fix, commit `fix: codex iter-N — <line>`,",
            "     re-run codex until clean in TWO consecutive runs.",
            "   - On rate-limit (stderr matches `usage limit` / `rate limit` /",
            "     `too many requests` / `out of token` / `quota`): wait 60s, retry",
            "     ONCE. If still rate-limited, MARK codex SKIPPED with reason",
            "     `rate-limited`. If the codex binary is missing or unreachable,",
            "     MARK SKIPPED with reason `unavailable`. NO other skip reasons are",
            "     allowed — the workers CLI rejects them at the flag layer.",
            f"   - Phase nudge: `fleet workers update {task.slug} {proj_flag} \\",
            "       --phase review-codex --review-codex-status iterating`",
            "",
            "4. Final terminal write (the load-bearing call):",
            "",
            f"   `fleet workers update {task.slug} {proj_flag} --phase review-done \\",
            "     --review-claude-status passed --review-claude-rounds <N> \\",
            "     --review-codex-status {passed|skipped} --review-codex-rounds <M>` \\",
            "    [`--review-codex-skip-reason rate-limited` only when codex SKIPPED]",
            "",
            "   The workers CLI rejects anything except passed/skipped here for",
            "   codex, and skipped+reason must be in {rate-limited, unavailable, no-git}.",
            "   /review can NEVER be skipped (the CLI rejects --review-claude-",
            "   status=skipped). If you cannot get /review to pass, do NOT flip",
            "   phase=review-done — instead:",
            f"     `fleet workers update {task.slug} {proj_flag} --phase blocked \\",
            "       --reason \"/review iter-N blocked: <one line>\"`",
            "   and exit. The coord raises the BLOCKED state to the operator.",
            "",
            "5. Exit cleanly (Ctrl-D / /exit) once you wrote --phase review-done.",
            "   The coord polls state.json on the next tick, sees phase=review-done,",
            "   and dispatches the finisher subagent. You do NOT push or open the",
            "   PR — that is the finisher's job.",
            "",
            "## Hard prohibitions",
            "",
            "- Do NOT push the branch. Do NOT `gh pr create`. Do NOT amend.",
            "- Do NOT skip /review (mandatory reviewer).",
            "- Do NOT broaden codex skip reasons beyond {rate-limited, unavailable, no-git}",
            "  — the workers CLI will reject the update and your phase=review-done",
            "  flip won't land.",
            "- Do NOT alter the worker's commits except via review-iter fix commits",
            "  on top.",
            "",
            "## §7 return contract (terse)",
            "",
            "When done, return a short message: `review iterations: claude=N codex=M;",
            "final claude=passed; final codex={passed|skipped:rate-limited|skipped:unavailable|skipped:no-git}`.",
            "",
            "You have: /review, /codex review, gh, git, full repo at the worker's cwd.",
            "NO interactive chat — operator can't reply mid-flight.",
        ])
    else:
        # Non-git reviewer: /review still mandatory; codex MUST be
        # skipped with reason=no-git because `codex review --base main`
        # needs a git diff. Fixes go directly on top of the worker's
        # in-place edits (no commits).
        lines.extend([
            "1. The worker edited project files in place (non-git project).",
            "   Inspect the current working tree to understand what changed.",
            "",
            f"2. /review iteration loop (gstack skill, MANDATORY — never skippable):",
            "   - Invoke `/review` via the Skill tool (literal name `review`).",
            "   - On any [P0] or [P1] finding: fix it directly in the file. There",
            "     is no commit — non-git projects have no version control.",
            "   - Loop until `/review` returns clean in TWO consecutive runs.",
            "   - Track the rounds count — you'll pass it as --review-claude-rounds.",
            f"   - Update phase mid-loop: `fleet workers update {task.slug} {proj_flag} \\",
            "       --phase review-claude --review-claude-status iterating`",
            "",
            "3. codex review — SKIP for non-git projects. `codex review --base main`",
            "   needs a git diff which this project doesn't have. The workers CLI",
            "   allowlists `no-git` as a skip reason precisely for this case. Set",
            "   `--review-codex-status skipped --review-codex-skip-reason no-git`",
            "   in the final terminal write. Do NOT attempt to run codex.",
            "",
            "4. Final terminal write (the load-bearing call):",
            "",
            f"   `fleet workers update {task.slug} {proj_flag} --phase review-done \\",
            "     --review-claude-status passed --review-claude-rounds <N> \\",
            "     --review-codex-status skipped --review-codex-skip-reason no-git`",
            "",
            "   /review can NEVER be skipped. If you cannot get /review to pass:",
            f"     `fleet workers update {task.slug} {proj_flag} --phase blocked \\",
            "       --reason \"/review iter-N blocked: <one line>\"`",
            "   and exit. The coord raises the BLOCKED state to the operator.",
            "",
            "5. Exit cleanly (Ctrl-D / /exit) once you wrote --phase review-done.",
            "   The coord polls state.json on the next tick and dispatches the",
            "   finisher subagent, which marks the task done with a diff summary.",
            "",
            "## Hard prohibitions",
            "",
            "- Do NOT `git init` or convert the project to git mode. The operator",
            "  registered this as non-git on purpose.",
            "- Do NOT skip /review (mandatory reviewer).",
            "- Do NOT use a codex skip reason other than `no-git` for this dispatch.",
            "",
            "## §7 return contract (terse)",
            "",
            "When done, return: `review iterations: claude=N codex=0;",
            "final claude=passed; final codex=skipped:no-git`.",
            "",
            "You have: /review, full project directory at the worker's cwd (no git).",
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
    is_git: bool = True,
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
    """
    if branch is None:
        branch = f"worker/{task.slug}"
    if workers_dir is None:
        workers_dir = f"~/.fleet/projects/{project}/workers/{task.slug}"

    proj_flag = f"--project {project}"

    if is_git:
        lines: list[str] = [
            f"You are a Fleet FINISHER subagent for task: {task.slug}",
            f"Project: {project}",
            f"Branch to push: {branch}",
            "",
            "You are running as a Fleet-dispatched Claude session. The reviewer",
            "subagent ran /review + codex on the worker's diff, recorded terminal",
            "review_claude_status + review_codex_status, and flipped the phase to",
            "review-done. Your job is mechanical: push, open the PR, update the",
            "worker's phase to done. NO code changes. NO review iteration.",
            "",
            f"State file:  {workers_dir}/state.json",
            f"Branch:      {branch} (already has worker + reviewer commits, ready to push)",
            f"Base for PR: main",
            "",
            "## Required workflow",
            "",
            f"1. `git push -u origin {branch}` — fresh push. If origin already has",
            "   a stale prior attempt's tip, use `git push --force-with-lease",
            f"   origin {branch}` (your branch is the only writer; --force-with-",
            "   lease is safe). Never plain `--force`.",
            "",
            "2. Read state.json to extract reviewer counts for the PR body:",
            f"   - `cat {workers_dir}/state.json | jq -r '.review_claude_rounds, .review_codex_rounds, .review_codex_status, .review_codex_skip_reason'`",
            "",
            f"3. `gh pr create --base main --head {branch} --title '<commit-1 message>' \\",
            "     --body \"$(cat <<'EOF'",
            "## Summary",
            "<1-3 bullets from the worker's commits>",
            "",
            "## Review",
            "- /review: passed (claude rounds: <N>)",
            "- codex review: <passed (codex rounds: <M>) | SKIPPED: <reason> at <UTC ts>>",
            "",
            "## Test plan",
            "- [ ] CI green",
            "- [ ] verify locally",
            "EOF",
            "     )\"`",
            "",
            f"4. Capture the PR URL. Then:",
            f"   `fleet workers update {task.slug} {proj_flag} --phase done --pr-url <url> --exit 0`",
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
            "subagent ran /review on the worker's in-place diff, recorded terminal",
            "review_claude_status=passed + review_codex_status=skipped (reason: no-git),",
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
            f"   - `cat {workers_dir}/state.json | jq -r '.review_claude_rounds, .review_codex_status, .review_codex_skip_reason'`",
            "   - Expected: review_codex_status=skipped, review_codex_skip_reason=no-git.",
            "",
            "3. Write the terminal phase update (no --pr-url; non-git workers ship",
            "   without a PR URL):",
            "",
            f"   `fleet workers update {task.slug} {proj_flag} --phase done --exit 0`",
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
          description: <short>
          prompt_file: <abs path>
          run_in_background: true
          subagent_type: general-purpose
        END_DISPATCH

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
    return "\n".join([
        f"DISPATCH: {slug}",
        f"  agent_id: {agent_id}",
        f"  description: {desc}",
        f"  prompt_file: {prompt_file}",
        "  run_in_background: true",
        "  subagent_type: general-purpose",
        "END_DISPATCH",
    ])


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
