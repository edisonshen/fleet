"""Operator inbox — relay one-shot messages from `fleet message <id>` into
the host agent's next turn.

The TUI / `fleet message` command writes ~/.fleet/inbox/<id>.md. On the next
Stop hook fire (or SessionStart), the skill reads that file, injects its body
prefixed with `[OPERATOR]`, and archives the file so the same message isn't
delivered twice.

Three callers in main.py — read_pending, deliver, archive — kept separate so
the orchestrator decides the ordering. Folding them into a single consume()
would hide the failure mode where archive fails after we've already returned
the body to stdout (the worst case: one duplicate delivery on the next fire,
which is acceptable; the alternative — retracting the injection — isn't).
"""
from __future__ import annotations

import os
from datetime import datetime, timezone
from pathlib import Path

import health


def inbox_path(agent_id: str) -> Path:
    return health.fleet_home() / "inbox" / f"{agent_id}.md"


def archive_dir() -> Path:
    return health.fleet_home() / "inbox" / "archive"


def read_pending(agent_id: str) -> str | None:
    """Read the pending inbox message body, or None if no file is waiting.
    Never raises — a transient I/O hiccup just means we deliver next fire."""
    path = inbox_path(agent_id)
    try:
        return path.read_text(encoding="utf-8")
    except FileNotFoundError:
        return None
    except Exception:
        return None


def deliver(content: str) -> str:
    """Wrap an inbox body in the canonical operator-message marker. The
    agent recognizes the `[OPERATOR]` prefix as out-of-band context to
    incorporate, distinct from skill-driven `HANDOFF REQUESTED` nudges."""
    return f"[OPERATOR] {content.rstrip()}"


def archive(agent_id: str) -> bool:
    """Move ~/.fleet/inbox/<id>.md → ~/.fleet/inbox/archive/<id>-<UTC stamp>.md.

    The timestamp suffix prevents a collision on rapid resends (operator
    sends a second message before the agent's next fire). Returns True on
    success, False if the source file doesn't exist or the rename fails.
    """
    src = inbox_path(agent_id)
    if not src.exists():
        return False
    stamp = datetime.now(timezone.utc).strftime("%Y%m%d-%H%M%SZ")
    dst_dir = archive_dir()
    try:
        dst_dir.mkdir(parents=True, exist_ok=True)
        dst = dst_dir / f"{agent_id}-{stamp}.md"
        # Defend against same-second double-archive — append a random suffix
        # if the timestamped path is already taken. The body of two same-
        # second archives is preserved in two distinct files; the operator
        # gets the full audit trail.
        if dst.exists():
            dst = dst_dir / f"{agent_id}-{stamp}-{os.urandom(2).hex()}.md"
        os.replace(src, dst)
        return True
    except Exception:
        return False
