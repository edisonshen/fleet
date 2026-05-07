"""Worker prompt assembly + fleet-dispatch caller.

The coordinator decides what to dispatch (loop.py); this module owns
the mechanics of HOW to dispatch:

  1. build_worker_prompt(task, standards_md, learnings_md) — assemble
     the self-contained prompt the worker receives on its first turn.
  2. dispatch_worker(...) — invoke `fleet dispatch <task-id> --project
     <p> --cwd <wt> --command <claude command>`; capture the new
     agent ID off stdout.
  3. write_worker_inbox(agent_id, prompt) — drop the prompt into the
     worker's inbox so fleet-guard's SessionStart injects it.

Subprocess-only (matches fleet-guard discipline). All paths through
the fleet binary — Go remains the authoritative writer for tasks.md
and agent records. parse.py is read-only inside the skill.
"""
from __future__ import annotations

import json
import os
import re
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


def build_worker_prompt(
    task: parse.Task,
    project: str,
    standards_md: str,
    learnings_text: str,
    *,
    branch: str | None = None,
    workers_dir: str | None = None,
) -> str:
    """Assemble the worker's first-turn prompt.

    standards_md: the merged result from `fleet standards show --merged`
                  (already rendered as one markdown blob).
    learnings_text: the raw output of `fleet learnings list --limit=20`.
                  Inlined as-is up to the per-entry cap; the worker sees
                  the same table the operator does.
    branch: defaults to "worker/<slug>".
    workers_dir: defaults to "~/.fleet/projects/<project>/workers/<slug>".

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
    lines.extend([
        "## Required workflow",
        "",
        f"  fleet workers update {task.slug} {proj_flag} --phase branch",
        f"1. git checkout -b {branch}",
        "",
        f"  fleet workers update {task.slug} {proj_flag} --phase tdd-red",
        "2a. Write the failing test. git commit.",
        "",
        f"  fleet workers update {task.slug} {proj_flag} --phase tdd-green",
        "2b. Write the minimal impl. Test passes. git commit.",
        "",
        f"  fleet workers update {task.slug} {proj_flag} --phase tdd-refactor",
        "2c. Refactor without changing test behavior. git commit.",
        "",
        f"  fleet workers update {task.slug} {proj_flag} --phase review-claude",
        "3a. /review on your diff. Fix every P0/P1.",
        "",
        f"  fleet workers update {task.slug} {proj_flag} --phase review-codex",
        "3b. /codex review on your diff. Fix every P0/P1.",
        "",
        f"  fleet workers update {task.slug} {proj_flag} --phase push",
        "4. gh pr create. Capture the PR URL.",
        "",
        f"  fleet workers update {task.slug} {proj_flag} --phase done --pr-url <url>",
        "5. Done. Coord sees phase=done, archives this dir, transitions task to in-review.",
        "6. Optional:",
        f"   fleet learnings add --project {project} --tag <topic> \\",
        f"     --task {task.slug} \"<one paragraph>\"",
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
        "You have: /review, /codex review, gh, git, full repo at <cwd>.",
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


def dispatch_worker(
    task: parse.Task,
    project: str,
    cwd: str,
    *,
    fleet_bin: str = "fleet",
    extra_args: list[str] | None = None,
    timeout_s: float = 30.0,
) -> DispatchResult:
    """Invoke `fleet dispatch <task-id> --project <p> --cwd <cwd>`.

    Returns DispatchResult with agent_id parsed off the first 8-hex
    token in stdout. error is non-empty on failure (subprocess error,
    non-zero exit, or unparseable stdout). Caller writes nothing if
    error is set — the dispatch never started, so no inbox stub.
    """
    cmd = [fleet_bin, "dispatch", task.slug, "--project", project, "--cwd", cwd]
    if extra_args:
        cmd.extend(extra_args)
    try:
        proc = subprocess.run(
            cmd, capture_output=True, text=True, timeout=timeout_s,
            check=False,
        )
    except FileNotFoundError as exc:
        return DispatchResult(error=f"fleet binary not found: {exc}")
    except subprocess.TimeoutExpired as exc:
        return DispatchResult(error=f"fleet dispatch timed out after {timeout_s}s: {exc}")
    if proc.returncode != 0:
        # stderr first, stdout fallback. Trimmed because cobra can
        # emit a long usage block on a flag error.
        msg = (proc.stderr or proc.stdout or "").strip().splitlines()
        return DispatchResult(error="fleet dispatch failed: " + (msg[0] if msg else f"exit {proc.returncode}"))
    agent_id = _extract_agent_id(proc.stdout)
    if not agent_id:
        return DispatchResult(error=f"could not parse agent ID from `fleet dispatch` stdout: {proc.stdout!r}")
    return DispatchResult(
        agent_id=agent_id,
        branch=f"worker/{task.slug}",
    )


# Agent IDs are 8 hex chars (agent.NewID). Two patterns, in order:
#   1. Strict: `agent <id>` — fleet dispatch's canonical output shape
#      ("agent <id> dispatched on tmux session ...").
#   2. Fallback: any standalone 8-hex token, after stripping the bytes
#      that look like SHAs / tmux paths to reduce false-positive risk.
# We try strict first; if it doesn't match, fall back to the looser one
# but only if there's exactly one 8-hex token in the trimmed text.
_AGENT_ID_STRICT_RE = re.compile(r"\bagent\s+([0-9a-f]{8})\b")
_AGENT_ID_LOOSE_RE = re.compile(r"\b([0-9a-f]{8})\b")
# Strictly validate inputs to write_worker_inbox.
_AGENT_ID_FULL_RE = re.compile(r"^[0-9a-f]{8}$")


def _extract_agent_id(text: str) -> str:
    if not text:
        return ""
    # Prefer the keyword-anchored form. Drift-tolerant fallback only
    # fires when there's exactly one 8-hex match across the whole
    # string — a path like /tmp/abcdef01/foo would otherwise win.
    strict = _AGENT_ID_STRICT_RE.search(text)
    if strict:
        return strict.group(1)
    matches = _AGENT_ID_LOOSE_RE.findall(text)
    if len(matches) == 1:
        return matches[0]
    return ""


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
