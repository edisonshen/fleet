"""macOS memorystatus / jetsam observer for fleet-guard.

Background: when the kernel kills a claude process under memory pressure
(jetsam / memorystatus), fleet today has zero diagnostic record. The
operator sees a dead tmux session with no log, no incident file, no
explanation. This module gives fleet-guard a parser + incident writer
so an OOM kill can be turned into a structured incident in
~/.fleet/incidents/<agent_id>-<ts>.json.

Scope of this module:
  - parse_jetsam_line(line) — extract (pid, reason) from one macOS
    log line emitted by `log stream` (or saved log output)
  - write_incident(agent_id, pid, reason, project, role, command,
    last_activity_ts, when) — write an incident JSON file with the
    enough context for the TUI badge to render

The actual live `log stream` tail is NOT wired here — it requires
either a background daemon or a periodic hook scrape, both outside the
scope of the v0.1 spawn-pid bundle. A follow-up task (P3) wires the
tail to fleet-guard's _on_session_start or to a separate runner. The
parser + writer + TUI badge land now so the wiring is one PR away.

Failure contract: like the rest of fleet-guard, this module never
raises. Parse failures return None; writer I/O failures return False.
A jetsam-observer crash must not block the host turn.
"""
from __future__ import annotations

import json
import os
import re
import tempfile
from pathlib import Path
from typing import Any

# Match `pid N` followed by a reason. Two common macOS log shapes:
#
#   memorystatus_thread: idle: killing pid 8306 [highwater]
#   jetsam_kill_proc: killed pid 8306 reason: vm-thrashing
#
# We accept either a bracketed reason ("[highwater]") OR a "reason: <token>"
# tail. The pid is the load-bearing extraction; the reason is informational
# and falls back to "unknown" when parse fails.
_PID_RE = re.compile(r"\bpid\s+(\d+)\b", re.IGNORECASE)
_BRACKET_REASON_RE = re.compile(r"\[([a-zA-Z0-9_\-]+)\]")
_TAIL_REASON_RE = re.compile(r"reason[:\s]+([a-zA-Z0-9_\-]+)", re.IGNORECASE)

# Signal-strings that identify a jetsam / memorystatus kill line. We
# match on any of these to avoid eating regular memory-pressure
# advisories (which carry a pid too but are not kills).
_KILL_MARKERS = (
    "killing pid",
    "jetsam_kill",
    "killed pid",
    "memorystatus_kill",
)


def parse_jetsam_line(line: str) -> tuple[int, str] | None:
    """Parse one macOS log line. Returns (pid, reason) on a jetsam-kill
    match; None otherwise (advisory line, non-kill memorystatus event,
    or unrelated log entry).

    Reason normalization: bracketed reasons ([highwater], [vm-thrashing])
    take precedence over tail "reason: <token>" since macOS tends to
    emit the bracket form on the canonical jetsam_thread path. When
    neither is present, returns reason="unknown" so the incident JSON
    still has a field.
    """
    if not isinstance(line, str):
        return None
    lower = line.lower()
    if not any(marker in lower for marker in _KILL_MARKERS):
        return None
    pid_m = _PID_RE.search(line)
    if not pid_m:
        return None
    try:
        pid = int(pid_m.group(1))
    except ValueError:
        return None
    if pid <= 0:
        return None
    reason = "unknown"
    # The bracketed-reason marker appears AFTER the pid in the kernel's
    # canonical "killing pid N [reason]" form. macOS log lines also
    # carry a "kernel[0]" prefix bracket near the start, so we must
    # search only the post-pid tail to avoid latching onto that PID
    # facet. The pid_m.end() index points one past the last char of
    # the matched 'pid N' substring; the bracket search runs on the
    # remainder of the line.
    tail_start = pid_m.end()
    tail_segment = line[tail_start:]
    br = _BRACKET_REASON_RE.search(tail_segment)
    if br:
        reason = br.group(1)
    else:
        tail = _TAIL_REASON_RE.search(tail_segment)
        if tail:
            reason = tail.group(1)
    return pid, reason


def incidents_dir() -> Path:
    """Resolve ~/.fleet/incidents/, honoring FLEET_HOME for tests."""
    override = os.environ.get("FLEET_HOME")
    if override:
        base = Path(override)
    else:
        base = Path.home() / ".fleet"
    return base / "incidents"


def write_incident(
    agent_id: str,
    pid: int,
    reason: str,
    when: str,
    project: str = "",
    role: str = "",
    command: list[str] | None = None,
    last_activity_ts: str = "",
) -> bool:
    """Write a structured incident record to
    ~/.fleet/incidents/<agent_id>-<when-sanitized>.json.

    `when` is an RFC3339 timestamp string (caller passes
    health.now_rfc3339() or the kernel-emit time from the log line).
    We sanitize it for the filename (':' → '-') so it round-trips
    through filesystem semantics that disallow colons on some shares.

    Returns True on success, False on any I/O error. Best-effort —
    fleet-guard's failure contract.

    The JSON shape is what the TUI dashboard scan reads:
      {
        "schema_version": 1,
        "agent_id": "...",
        "pid": N,
        "reason": "highwater" | "vm-thrashing" | "fork-fail" | ...,
        "killed_at": "<RFC3339>",
        "project": "...",
        "role": "...",
        "command": ["sh", "-c", ...],
        "last_activity_ts": "..."
      }
    """
    body: dict[str, Any] = {
        "schema_version": 1,
        "agent_id": agent_id,
        "pid": pid,
        "reason": reason,
        "killed_at": when,
        "project": project,
        "role": role,
        "command": command or [],
        "last_activity_ts": last_activity_ts,
    }
    dir_ = incidents_dir()
    try:
        dir_.mkdir(parents=True, exist_ok=True)
        # Filename: <agent_id>-<sanitized-when>.json. Replace ':' with
        # '-' so Windows-shared filesystems don't reject the name —
        # macOS allows ':' but cross-host tooling shouldn't break.
        safe_when = when.replace(":", "-")
        path = dir_ / f"{agent_id}-{safe_when}.json"
        # Atomic via tempfile + os.replace (mirrors health._atomic_write_json).
        with tempfile.NamedTemporaryFile(
            mode="w",
            encoding="utf-8",
            dir=dir_,
            prefix=f".{path.name}.",
            suffix=".tmp",
            delete=False,
        ) as tf:
            json.dump(body, tf, indent=2)
            tf.write("\n")
            tf.flush()
            os.fsync(tf.fileno())
            tmp_path = tf.name
        os.replace(tmp_path, path)
        return True
    except Exception:
        try:
            os.unlink(tmp_path)  # type: ignore[name-defined]
        except Exception:
            pass
        return False


def list_incidents_for_project(project: str) -> list[dict[str, Any]]:
    """Return every parsed incident record whose project field matches.
    Used by the TUI dashboard scan (Go side reads the same directory
    directly). Provided here for symmetry + skill-side debugging.

    Returns an empty list when the directory is missing, when no
    incidents match, or on read errors (best-effort).
    """
    out: list[dict[str, Any]] = []
    dir_ = incidents_dir()
    if not dir_.exists():
        return out
    try:
        entries = list(dir_.iterdir())
    except OSError:
        return out
    for entry in entries:
        if entry.suffix != ".json":
            continue
        try:
            data = json.loads(entry.read_text(encoding="utf-8"))
        except (OSError, ValueError):
            continue
        if not isinstance(data, dict):
            continue
        if data.get("project", "") != project:
            continue
        out.append(data)
    return out
