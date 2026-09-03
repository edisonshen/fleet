"""Coordinator delegation guard — PreToolUse hook half.

A coordinator session (FLEET_ROLE=coord) must discuss, plan, file tasks
and dispatch; it must never implement inline. SKILL.md says so in prose,
but prose drifts under long contexts and post-handoff resumes. This
module is the mechanical backstop: on every PreToolUse fire in a coord
session it denies

  * Edit / Write / MultiEdit / NotebookEdit outside an approved docs
    location (any path with a `docs` component, or under FLEET_HOME);
  * Bash commands that run test suites, mutate git history, or write
    to files (sed -i, redirects, tee, patch, ...).

Agent-tool subagents (workers / reviewers / finishers) run inside the
coord's process and inherit its env, so FLEET_ROLE alone can't tell
them apart. Claude Code stamps `agent_id` into hook payloads that fire
inside a subagent; its presence exempts the call.

Every denial is appended to FLEET_HOME/coord-violations/<agent_id>.jsonl
so the Stop hook can nag once per turn and operators can grep how often
a coord tried to do a worker's job.

Escape hatch: FLEET_COORD_GUARD=off disables the deny (violations are
still logged).
"""
from __future__ import annotations

import json
import os
import re
import sys
import tempfile
from pathlib import Path
from typing import Any

import health

EDIT_TOOLS = frozenset({"Edit", "Write", "MultiEdit", "NotebookEdit"})

DOCS_DIR_NAMES = frozenset({"docs", "doc"})

# Bash patterns a coordinator has no business running. Anchored on word
# boundaries so `go test` matches but `go-testing-helper` does not. File
# writers are matched only in command position (start of line / after
# `;`, `&&`, `|`, `$(`) so `fleet rm <agent>` is not mistaken for `rm`.
_CMD_POS = r"(?:^|[;&|(]\s*|\n\s*)(?:sudo\s+)?"
_BASH_DENY: tuple[tuple[str, re.Pattern[str]], ...] = (
    ("test runner", re.compile(
        r"\b(go\s+test|pytest|python3?\s+-m\s+pytest|npm\s+(run\s+)?test|"
        r"yarn\s+test|pnpm\s+test|cargo\s+test|make\s+test|bun\s+test)\b")),
    ("git mutation", re.compile(
        _CMD_POS + r"git\b[^|;&\n]*\b(add|commit|push|rebase|merge|cherry-pick|"
        r"stash|reset|restore|revert|apply|am|rm|mv)\b")),
    ("in-place edit", re.compile(_CMD_POS + r"(sed|perl)\s+(-[a-zA-Z]*i|--in-place)")),
    ("file write", re.compile(_CMD_POS + r"(tee|patch|rm|mv|cp|truncate)\s")),
)

# `>` redirect that targets a real file (not /dev/null, not fd dup).
_REDIRECT = re.compile(r"(?<![0-9&<>])>{1,2}\s*(?!&|/dev/null)([^\s;|&]+)")

DENY_REASON = (
    "[fleet coord-guard] Coordinators delegate. This session is a "
    "coordinator (FLEET_ROLE=coord): it discusses, saves plan docs under "
    "docs/, files tasks with `fleet tasks add`, and dispatches Agent "
    "subagents. It never edits source, runs tests, or mutates git inline. "
    "File a task and let a worker do this: {what}"
)


ROLE_REMINDER = (
    "[FLEET] You are the coordinator for this project. Discuss, plan, save "
    "docs under docs/, file tasks, dispatch and shepherd workers. NEVER edit "
    "source, run test suites, or mutate git yourself — the coord-guard hook "
    "denies those tool calls. Implementation belongs to Agent subagents."
)


def is_coord_session() -> bool:
    role = os.environ.get("FLEET_ROLE", "").strip().lower()
    if role:
        return role == "coord"
    agent_id = os.environ.get("FLEET_AGENT_ID", "").strip()
    if not agent_id:
        return False
    rec = health.read_record(agent_id)
    return bool(rec and rec.get("is_coord"))


def guard_disabled() -> bool:
    return os.environ.get("FLEET_COORD_GUARD", "").strip().lower() in {
        "off", "0", "false", "no"}


def violations_path(agent_id: str) -> Path:
    return health.fleet_home() / "coord-violations" / f"{agent_id}.jsonl"


def _scratch_roots() -> list[Path]:
    roots = [health.fleet_home(), Path(tempfile.gettempdir())]
    return [r.resolve() for r in roots]


def _is_docs_path(raw: str) -> bool:
    """True for paths a coord may write: anything under a docs/ folder,
    FLEET_HOME, or the system temp dir."""
    if not raw:
        return False
    p = Path(raw).expanduser()
    try:
        resolved = p.resolve()
        if any(resolved.is_relative_to(root) for root in _scratch_roots()):
            return True
    except Exception:
        pass
    return any(part in DOCS_DIR_NAMES for part in p.parts)


def _bash_offense(command: str) -> str | None:
    for label, pat in _BASH_DENY:
        if pat.search(command):
            return label
    m = _REDIRECT.search(command)
    if m and not _is_docs_path(m.group(1)):
        return "file write (redirect)"
    return None


def classify(payload: dict[str, Any]) -> str | None:
    """Return a short description of the offense, or None if allowed."""
    if payload.get("agent_id"):
        return None
    tool = payload.get("tool_name", "")
    tool_input = payload.get("tool_input") or {}
    if not isinstance(tool_input, dict):
        tool_input = {}
    if tool in EDIT_TOOLS:
        path = str(tool_input.get("file_path") or tool_input.get("notebook_path") or "")
        if _is_docs_path(path):
            return None
        return f"{tool} {path or '<unknown path>'}"
    if tool == "Bash":
        command = str(tool_input.get("command") or "")
        label = _bash_offense(command)
        if label is None:
            return None
        snippet = command.strip().splitlines()[0][:120] if command.strip() else ""
        return f"Bash ({label}): {snippet}"
    return None


def record_violation(agent_id: str, what: str, tool: str, denied: bool) -> None:
    path = violations_path(agent_id)
    try:
        path.parent.mkdir(parents=True, exist_ok=True)
        with path.open("a", encoding="utf-8") as f:
            f.write(json.dumps({
                "ts": health.now_rfc3339(),
                "tool": tool,
                "what": what,
                "denied": denied,
            }) + "\n")
    except Exception as exc:
        print(f"fleet-guard: coord-violation log failed: {exc}", file=sys.stderr)


def count_violations(agent_id: str) -> int:
    try:
        with violations_path(agent_id).open("r", encoding="utf-8") as f:
            return sum(1 for line in f if line.strip())
    except FileNotFoundError:
        return 0
    except Exception:
        return 0


def on_pre_tool_use(payload: dict[str, Any], agent_id: str) -> dict[str, Any] | None:
    """PreToolUse handler. Returns the hook JSON output to print, or None
    to stay silent (allow)."""
    if not is_coord_session():
        return None
    what = classify(payload)
    if what is None:
        return None
    denied = not guard_disabled()
    record_violation(agent_id, what, str(payload.get("tool_name", "")), denied)
    if not denied:
        return None
    return {
        "hookSpecificOutput": {
            "hookEventName": "PreToolUse",
            "permissionDecision": "deny",
            "permissionDecisionReason": DENY_REASON.format(what=what),
        }
    }


def _seen_path(agent_id: str) -> Path:
    return violations_path(agent_id).with_suffix(".seen")


def _read_seen(agent_id: str) -> int:
    try:
        return int(_seen_path(agent_id).read_text(encoding="utf-8").strip() or 0)
    except Exception:
        return 0


def stop_nag(agent_id: str) -> str | None:
    """Stop-hook half: if violations accrued since the last Stop, return
    a one-line reminder to inject; else None. Advances the seen marker so
    each violation is nagged about exactly once."""
    total = count_violations(agent_id)
    seen_before = _read_seen(agent_id)
    if total <= seen_before:
        return None
    try:
        path = _seen_path(agent_id)
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(str(total), encoding="utf-8")
    except Exception as exc:
        print(f"fleet-guard: coord-violation seen-marker failed: {exc}", file=sys.stderr)
    return (
        f"[FLEET] coord-guard blocked {total - seen_before} attempt(s) this "
        "turn to edit source / run tests / mutate git. You are the "
        "coordinator: do not retry inline. File the work as a task "
        "(`fleet tasks add`), link a TASK-PLAN doc, promote, and let the "
        "tick dispatch a worker."
    )
