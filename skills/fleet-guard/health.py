"""Agent health: read context_pct from the transcript, write the agent record.

The skill is one writer of ~/.fleet/agents/<id>.json among many. Its ownership
partition lives in SKILL.md: the skill owns context/blocked/inbox/handoff_type
flags and last_activity_ts; everything else (PID, session, engine, role, mode,
task_id, project, cwd, command, spawned_at, handoff_number, last_handoff_path,
review_round) is owned by `fleet dispatch` / `fleet handoff` and MUST be
preserved on every fire.

Atomic write contract: write to <path>.tmp on the same filesystem, then
os.replace, which is atomic on POSIX. No reader ever sees a torn JSON.

Failure contract: never raise. The host agent's turn must not be blocked by a
skill bug. Caller (main.py) catches everything and logs to stderr.
"""
from __future__ import annotations

import json
import os
import sys
import tempfile
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

# Mirrors spike/stop-hook.py:CONTEXT_LIMITS. Keep in sync. Future operator-
# overridable via ~/.fleet/config.yaml:context_limits (TODOS F11) — not yet
# wired. Unknown models leave context_pct=None rather than guess a limit.
CONTEXT_LIMITS: dict[str, int] = {
    "claude-opus-4-7":   1_000_000,
    "claude-opus-4-6":     200_000,
    "claude-opus-4-5":     200_000,
    "claude-sonnet-4-6":   200_000,
    "claude-sonnet-4-5":   200_000,
    "claude-haiku-4-5":    200_000,
}

# Schema version for ~/.fleet/agents/<id>.json. Mirrors
# internal/agent.SchemaVersion. Bumped only when the on-disk shape changes
# incompatibly.
SCHEMA_VERSION = 1

# Skill-owned record fields. Every other field on disk is preserved verbatim.
# Keep this set narrow — adding here requires a paired SKILL.md update.
OWNED_FIELDS: frozenset[str] = frozenset({
    "context_pct",
    "context_source",
    "last_activity_ts",
    "blocked",
    "blocked_reason",
    "blocked_since",
    "needs_input",
    "has_pending_question",
    "inbox_pending",
    "handoff_type",
    "handoff_type_at",
})

YELLOW_THRESHOLD = 50.0
RED_THRESHOLD = 70.0


def fleet_home() -> Path:
    """Resolve ~/.fleet, honoring FLEET_HOME for sandboxed tests / CI."""
    override = os.environ.get("FLEET_HOME")
    if override:
        return Path(override)
    return Path.home() / ".fleet"


def agent_record_path(agent_id: str) -> Path:
    return fleet_home() / "agents" / f"{agent_id}.json"


def read_context_pct(payload: dict[str, Any]) -> tuple[float | None, str | None]:
    """Walk the transcript JSONL referenced by payload['transcript_path'] and
    compute context_pct from the most-recent message.usage.

    Returns (context_pct, model_name). Either may be None — the skill records
    None rather than guessing when data is missing or the model is unknown.

    Mirrors spike/stop-hook.py logic so the skill and the spike compute the
    same number against the same transcript. Output tokens are excluded
    (they don't carry into the next turn's context).
    """
    transcript_path = payload.get("transcript_path") or ""
    if not transcript_path:
        return (None, None)
    tp = Path(transcript_path)
    if not tp.exists():
        return (None, None)

    last_usage: dict[str, Any] | None = None
    last_model = ""
    try:
        with tp.open("r", encoding="utf-8", errors="replace") as f:
            for line in f:
                line = line.strip()
                if not line:
                    continue
                try:
                    obj = json.loads(line)
                except Exception:
                    continue
                msg = obj.get("message") or {}
                if obj.get("type") == "assistant":
                    m = msg.get("model")
                    if isinstance(m, str) and m:
                        last_model = m
                u = obj.get("usage") or msg.get("usage")
                if isinstance(u, dict):
                    last_usage = u
    except Exception:
        return (None, None)

    if not last_usage:
        return (None, last_model or None)

    in_t = int(last_usage.get("input_tokens", 0) or 0)
    cr_t = int(last_usage.get("cache_read_input_tokens", 0) or 0)
    cc_t = int(last_usage.get("cache_creation_input_tokens", 0) or 0)
    total = in_t + cr_t + cc_t

    model = last_model or None
    if model not in CONTEXT_LIMITS:
        return (None, model)
    pct = round(total * 100.0 / CONTEXT_LIMITS[model], 2)
    return (pct, model)


def threshold(context_pct: float | None) -> str:
    """Classify a context_pct into 'green' | 'yellow' | 'red' | 'unknown'."""
    if context_pct is None:
        return "unknown"
    if context_pct >= RED_THRESHOLD:
        return "red"
    if context_pct >= YELLOW_THRESHOLD:
        return "yellow"
    return "green"


def now_rfc3339() -> str:
    """UTC RFC 3339 with seconds precision and trailing Z. Matches the format
    spike/stop-hook.py uses; Go's time.Time accepts it on read."""
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def read_record(agent_id: str) -> dict[str, Any] | None:
    """Load the agent record from disk. Returns None if the file is missing
    or unparseable. Never raises — the skill must not block the host turn on
    a transient I/O hiccup. Callers that need a real record (handoff.py)
    treat None as 'no auto-action this fire' rather than escalating."""
    path = agent_record_path(agent_id)
    try:
        with path.open("r", encoding="utf-8") as f:
            record = json.load(f)
    except FileNotFoundError:
        return None
    except Exception:
        return None
    if not isinstance(record, dict):
        return None
    return record


def update_record(agent_id: str, **fields: Any) -> bool:
    """Read-modify-write the agent record, touching only OWNED_FIELDS plus
    last_activity_ts (set automatically each fire).

    Returns True on success, False on any failure (missing record, IO error,
    parse error). Caller decides whether to log; the skill itself never
    raises.

    Behavior:
    - If the record file does not exist, returns False without creating one.
      The skill is not authoritative for record creation — `fleet dispatch`
      is. A missing record means the agent is not under Fleet supervision
      (e.g. operator launched Claude directly), so we no-op silently.
    - Unknown fields passed in are dropped to avoid letting callers smuggle
      writes to dispatch-owned columns through this function.
    - Field order is preserved by reading the existing JSON into Python's
      insertion-ordered dict and writing it back without sort_keys.
    """
    path = agent_record_path(agent_id)
    try:
        with path.open("r", encoding="utf-8") as f:
            record = json.load(f)
    except FileNotFoundError:
        return False
    except Exception:
        return False
    if not isinstance(record, dict):
        return False

    for key, value in fields.items():
        if key not in OWNED_FIELDS:
            continue
        record[key] = value
    record["last_activity_ts"] = now_rfc3339()

    return _atomic_write_json(path, record)


def _atomic_write_json(path: Path, record: dict[str, Any]) -> bool:
    """Write record to path via tempfile + os.replace. Returns False on any
    I/O failure (caller logs). The temp file lives in the same directory as
    the destination so os.replace stays a same-filesystem rename."""
    try:
        path.parent.mkdir(parents=True, exist_ok=True)
        # NamedTemporaryFile with delete=False so we can os.replace it.
        # dir=path.parent guarantees same-fs rename.
        with tempfile.NamedTemporaryFile(
            mode="w",
            encoding="utf-8",
            dir=path.parent,
            prefix=f".{path.name}.",
            suffix=".tmp",
            delete=False,
        ) as tf:
            json.dump(record, tf, indent=2)
            tf.write("\n")
            tf.flush()
            os.fsync(tf.fileno())
            tmp_path = tf.name
        os.replace(tmp_path, path)
        return True
    except Exception:
        # Best-effort cleanup of the temp file if rename failed.
        try:
            os.unlink(tmp_path)  # type: ignore[name-defined]
        except Exception:
            pass
        return False
