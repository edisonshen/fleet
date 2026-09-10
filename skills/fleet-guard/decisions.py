"""Conversation decision capture — the Stop-hook half of Key Decisions.

A coordinator's material decisions mostly happen in conversation with the
operator ("defer the cache refactor, do auth first"), and nobody types
`fleet checkpoint decision` afterwards. The Stop hook already runs after
every coord turn with the transcript path in hand, so this module mines
the exchange automatically:

    transcript.jsonl                     coord-state.json
    ----------------                     ----------------
    user: "<operator prompt>"     --->   session_decisions += {
    assistant: ...tool calls...            "text": "operator: <prompt>
    assistant: "<final reply>"                      -> coord: <reply>",
    (Stop fires)                           "coord_id": ..., "ts": ...}

Capture runs only when the shell is stamped FLEET_ROLE=coord (spawn sets it
on every coord); the agent-record `is_coord` fallback coordguard uses for
its write-guard is deliberately NOT honoured here, so a hook fired from an
unstamped shell can never write another coord's decision buffer.

Only OPERATOR turns are captured — a human prompt, or an `[OPERATOR]`
inbox delivery — never the coord's own tool-result churn or fleet-guard's
own injections (HANDOFF REQUESTED, [FLEET] nags). Operator turns are rare
and are exactly where conversation-only decisions live, so the buffer stays
compact. Both halves are truncated so one exchange is one readable line.

The write goes through `fleet checkpoint decision` (coordinator.lock,
load-mutate-save, coord_id stamp, dedupe) rather than touching
coord-state.json from Python. A cursor file remembers the last captured
turn so a multi-Stop turn (hook block + continue) records the exchange
once. Every path is best-effort: a capture fault must never break the
Stop hook.
"""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import sys
from pathlib import Path

import health

OPERATOR_PREFIX = "[OPERATOR]"

# User-role transcript entries that are NOT the operator talking: fleet-guard
# / coord-guard injections, Claude Code's own command echoes, interrupts.
_SKIP_PREFIXES = (
    "HANDOFF REQUESTED",
    "[FLEET]",
    "[fleet coord-guard]",
    "<command-",
    "<local-command",
    "<system-reminder",
    "[Request interrupted",
)

MAX_OPERATOR_CHARS = 240
MAX_COORD_CHARS = 240
_CHECKPOINT_TIMEOUT_S = 10.0


def cursor_path(agent_id: str) -> Path:
    return health.fleet_home() / "coord-decisions" / f"{agent_id}.cursor"


def _flatten(text: str, limit: int) -> str:
    flat = " ".join(str(text).split())
    if len(flat) > limit:
        flat = flat[: limit - 1].rstrip() + "…"
    return flat


def _text_blocks(content) -> str:
    """Join the text blocks of a message content field (str or block list).
    tool_use / tool_result blocks contribute nothing."""
    if isinstance(content, str):
        return content
    if not isinstance(content, list):
        return ""
    parts: list[str] = []
    for block in content:
        if isinstance(block, dict) and block.get("type") == "text":
            t = block.get("text")
            if isinstance(t, str) and t.strip():
                parts.append(t)
    return "\n".join(parts)


def operator_text(obj: dict) -> str | None:
    """Return the operator's text for a `user` transcript entry, or None when
    the entry is not the operator speaking (tool results, meta entries,
    fleet-guard injections, command echoes)."""
    if obj.get("type") != "user" or obj.get("isMeta"):
        return None
    msg = obj.get("message") or {}
    content = msg.get("content") if isinstance(msg, dict) else None
    if content is None:
        content = obj.get("content")
    if isinstance(content, list) and any(
        isinstance(b, dict) and b.get("type") == "tool_result" for b in content
    ):
        return None
    text = _text_blocks(content).strip()
    if not text:
        return None
    if text.startswith(OPERATOR_PREFIX):
        text = text[len(OPERATOR_PREFIX):].strip()
        return text or None
    for prefix in _SKIP_PREFIXES:
        if text.startswith(prefix):
            return None
    return text


def last_exchange(transcript_path: str) -> tuple[str, str, str] | None:
    """Walk the transcript and return (turn_key, operator_text, coord_reply)
    for the LAST operator turn, where coord_reply is the final assistant
    text block emitted after it (the conclusion, not the running
    narration). None when the transcript has no operator turn."""
    if not transcript_path:
        return None
    tp = Path(transcript_path)
    if not tp.exists():
        return None
    key = ""
    op = ""
    reply = ""
    try:
        with tp.open("r", encoding="utf-8", errors="replace") as f:
            for lineno, line in enumerate(f):
                line = line.strip()
                if not line:
                    continue
                try:
                    obj = json.loads(line)
                except Exception:
                    continue
                if not isinstance(obj, dict):
                    continue
                text = operator_text(obj)
                if text is not None:
                    key = str(obj.get("uuid") or obj.get("timestamp") or lineno)
                    op = text
                    reply = ""
                    continue
                if op and obj.get("type") == "assistant":
                    msg = obj.get("message") or {}
                    t = _text_blocks(msg.get("content") if isinstance(msg, dict) else None).strip()
                    if t:
                        reply = t
    except Exception:
        return None
    if not op:
        return None
    return key, op, reply


def decision_line(op: str, reply: str) -> str:
    line = f"operator: {_flatten(op, MAX_OPERATOR_CHARS)}"
    if reply.strip():
        line += f" → coord: {_flatten(reply, MAX_COORD_CHARS)}"
    return line


def _read_cursor(agent_id: str) -> str:
    try:
        return cursor_path(agent_id).read_text(encoding="utf-8").strip()
    except Exception:
        return ""


def _write_cursor(agent_id: str, key: str) -> None:
    path = cursor_path(agent_id)
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_suffix(".tmp")
    tmp.write_text(key, encoding="utf-8")
    os.replace(tmp, path)


def _fleet_binary() -> str | None:
    fleet_bin = os.environ.get("FLEET_BIN")
    if not fleet_bin or not os.access(fleet_bin, os.X_OK):
        fleet_bin = shutil.which("fleet")
    return fleet_bin or None


def record(line: str) -> bool:
    """Shell `fleet checkpoint decision <line>` for the coord's project.
    Returns True when the CLI accepted it."""
    fleet_bin = _fleet_binary()
    if not fleet_bin:
        print("fleet-guard: decisions: fleet binary not found", file=sys.stderr)
        return False
    cmd = [fleet_bin, "checkpoint", "decision"]
    project = os.environ.get("FLEET_PROJECT", "").strip()
    if project:
        cmd += ["--project", project]
    cmd.append(line)
    try:
        proc = subprocess.run(
            cmd, capture_output=True, text=True,
            timeout=_CHECKPOINT_TIMEOUT_S, check=False,
        )
    except (OSError, subprocess.TimeoutExpired) as exc:
        print(f"fleet-guard: decisions: {exc}", file=sys.stderr)
        return False
    if proc.returncode != 0:
        msg = (proc.stderr or proc.stdout or "").strip()
        print(f"fleet-guard: decisions: checkpoint failed: {msg}", file=sys.stderr)
        return False
    return True


def role_is_coord() -> bool:
    return os.environ.get("FLEET_ROLE", "").strip().lower() == "coord"


def capture(payload: dict, agent_id: str) -> str | None:
    """Stop-hook entry: record the latest operator↔coord exchange once.
    Explicit FLEET_ROLE=coord shells only. Returns the recorded line, or
    None when nothing new was captured."""
    if not role_is_coord():
        return None
    found = last_exchange(str(payload.get("transcript_path") or ""))
    if found is None:
        return None
    key, op, reply = found
    if key and key == _read_cursor(agent_id):
        return None
    line = decision_line(op, reply)
    if not record(line):
        return None
    try:
        _write_cursor(agent_id, key)
    except Exception as exc:
        print(f"fleet-guard: decisions: cursor write failed: {exc}", file=sys.stderr)
    return line
