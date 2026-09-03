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
from pathlib import Path
from typing import Any

import health

EDIT_TOOLS = frozenset({"Edit", "Write", "MultiEdit", "NotebookEdit"})

DOCS_DIR_NAMES = frozenset({"docs", "doc"})

# Bash patterns a coordinator has no business running. Every pattern is
# matched only in command position (start of line / after `;`, `&&`, `|`,
# `$(`, backtick, optional `sudo`/`env`/`time`), so `fleet rm <agent>` is
# not mistaken for `rm` and `rg -n 'go test' .` is not mistaken for a test
# run. Word-bounded so `go test` matches but `go-testing-helper` does not.
_CMD_POS = (r"(?:^|[;&|(`]\s*|\n\s*)"
            r"(?:(?:sudo|env|time|nice|xargs)(?:\s+-\S+)*\s+)*(?:\w+=\S*\s+)*")

# Shell metacharacters inside a quoted string are data, not structure:
# `rg -n 'pytest|npm test'` must not put `npm test` in command position.
_QUOTED = re.compile(r"'[^']*'|\"(?:\\.|[^\"\\])*\"")
_META_IN_QUOTES = re.compile(r"[;&|()`<>\n]")


def _neutralize_quotes(command: str) -> str:
    return _QUOTED.sub(lambda m: _META_IN_QUOTES.sub(" ", m.group(0)), command)


_BASH_DENY: tuple[tuple[str, re.Pattern[str]], ...] = (
    ("test runner", re.compile(
        _CMD_POS + r"(go\s+test|pytest|python3?\s+-m\s+(pytest|unittest)|"
        r"npm\s+(run\s+)?test|yarn\s+test|pnpm\s+test|cargo\s+test|"
        r"make\s+(test|check)|bun\s+test)\b")),
    ("git mutation", re.compile(
        _CMD_POS + r"git\s+(?:-[cC]\s+\S+\s+|--\S+\s+)*(add|commit|push|rebase|merge|cherry-pick|"
        r"stash|reset|restore|revert|apply|am|rm|mv|checkout|switch|clean|"
        r"worktree|filter-branch|update-ref|symbolic-ref)\b")),
    ("in-place edit", re.compile(_CMD_POS + r"(sed|perl)\s+(-[a-zA-Z]*i|--in-place)")),
    ("formatter write", re.compile(
        _CMD_POS + r"(gofmt|goimports|gofumpt)\s+[^|;&\n]*-[wl]*w\b|"
        + _CMD_POS + r"(black|isort|ruff\s+format|ruff\s+check[^|;&\n]*--fix|"
        r"prettier[^|;&\n]*--write|eslint[^|;&\n]*--fix|cargo\s+fmt|rustfmt)\b")),
    ("file write", re.compile(
        _CMD_POS + r"(tee|patch|rm|mv|cp|truncate|touch|install|ln|rmdir|"
        r"dd|rsync|chmod|chown|unzip|tar)(?:\s|$)")),
    ("scripted write", re.compile(
        _CMD_POS + r"(python3?|node|ruby|perl)\s+-[ec]\s")),
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
    return [health.fleet_home().resolve()]


def _is_docs_path(raw: str) -> bool:
    """True for paths a coord may write: anything under a docs/ folder or
    under FLEET_HOME. Judged on the fully resolved path (symlinks followed,
    `..` collapsed), so `docs/../main.go` or a `docs/link -> ../src` escape
    is still a source write. The docs check requires a real directory
    component, not just a filename prefix."""
    if not raw:
        return False
    p = Path(raw).expanduser()
    try:
        resolved = p.resolve()
    except Exception:
        resolved = Path(os.path.normpath(str(p)))
    if any(resolved.is_relative_to(root) for root in _scratch_roots()):
        return True
    return any(part in DOCS_DIR_NAMES for part in resolved.parts[:-1])


def _bash_offense(command: str) -> str | None:
    command = _neutralize_quotes(command)
    for label, pat in _BASH_DENY:
        if pat.search(command):
            return label
    for m in _REDIRECT.finditer(command):
        if not _is_docs_path(m.group(1)):
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
