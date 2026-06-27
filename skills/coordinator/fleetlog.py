"""Fleet local debug-log emitter (Python side), byte-compatible with the
Go internal/fleetlog package. See docs/DESIGN-fleet-debug-logs.md.

The coordinator tick (loop.py, a Python process) is the biggest log
producer, so it gets its own emitter that writes the SAME JSONL envelope
as Go to per-process files under the SAME directory — so one `jq` reads
Go + Python lines uniformly.

Invariants mirrored from the Go side:
  - Per-process file: fleet-<date>-<comp>-<pid>-<pid_start>.jsonl. The
    pid_start suffix makes same-day PID reuse a distinct file.
  - Raw fd, single syscall per record: os.open(O_WRONLY|O_APPEND|O_CREAT)
    + os.write(line_bytes). NOT open(...,"a") — buffered text I/O would
    fragment a record across writes.
  - Best-effort: log() NEVER raises; a logging failure must never break a
    tick.
  - XDG-aware dir(): $XDG_STATE_HOME/fleet/logs if set, else
    _resolve_home()/logs — matching Go's fleetlog.Dir() so the two
    languages always write to the same directory.
"""
from __future__ import annotations

import json
import os
import secrets
import threading
import time
from datetime import datetime, timezone

# Closed event vocabulary (mirror of Go fleetlog.Types). log() does not
# reject unknown types (best-effort), but callers stay within this set.
TYPES = frozenset({
    "coord.start", "coord.handoff", "coord.resume", "coord.tick",
    "decision", "dispatch.worker", "worker.start", "tool.call",
    "tool.result", "model.call", "state.transition", "pr.opened",
    "pr.status", "task.completed", "worker.failed", "cli.start",
    "cli.finish", "error", "cleanup",
})

COMP_COORD = "coord"
COMP_WORKER = "worker"
COMP_CLI = "cli"

# Per-field string cap in `data` (size bound, not a secret control). Mirror
# of Go fleetlog.dataCap + elision marker.
_DATA_CAP = 2048
_ELISION = "...[truncated]"

# Per-process identity, captured once at import. _PID_START fingerprints
# this process so a reused PID (a later process) writes a distinct file —
# the Python analogue of Go's pidStart.
_PID = os.getpid()
_PID_START = time.time_ns()
_PROC_TOKEN = secrets.token_hex(4)

_seq_lock = threading.Lock()
_seq = 0


def _reinit_after_fork() -> None:
    """A forked child inherits the parent's _PID/_PID_START/token, which
    would make it write the PARENT's per-process file — breaking the
    per-process isolation invariant. Re-fingerprint the child so it owns a
    distinct fleet-<date>-<comp>-<pid>-<pid_start>.jsonl."""
    global _PID, _PID_START, _PROC_TOKEN, _seq, _seq_lock
    _PID = os.getpid()
    _PID_START = time.time_ns()
    _PROC_TOKEN = secrets.token_hex(4)
    _seq = 0
    _seq_lock = threading.Lock()  # the inherited lock may be held; replace it


try:
    os.register_at_fork(after_in_child=_reinit_after_fork)
except AttributeError:  # pragma: no cover - register_at_fork is POSIX-only
    pass

# Marker file guarding the once/day retention prune.
_LAST_PRUNE = ".last-prune"
# 72h retention window (mirror of the Go PruneOlderThan(72h) default).
_RETENTION_S = 72 * 3600
# Throttle: re-scan for prunable files at most once per ~day.
_PRUNE_THROTTLE_S = 24 * 3600


def _resolve_home() -> str:
    """Mirror loop._resolve_home's FLEET_HOME / ~/.fleet resolution WITHOUT
    importing loop (avoids an import cycle: loop imports fleetlog)."""
    env = os.environ.get("FLEET_HOME")
    if env:
        return env
    return os.path.expanduser("~/.fleet")


def dir() -> str:
    """Log directory. XDG-aware to match Go fleetlog.Dir():
    $XDG_STATE_HOME/fleet/logs if set, else _resolve_home()/logs."""
    xdg = os.environ.get("XDG_STATE_HOME")
    if xdg:
        return os.path.join(xdg, "fleet", "logs")
    return os.path.join(_resolve_home(), "logs")


def _filename(comp: str, now: datetime) -> str:
    return (
        f"fleet-{now.strftime('%Y-%m-%d')}-{comp}-{_PID}-{_PID_START}.jsonl"
    )


def _cap_data(data):
    if not data:
        return None
    out = {}
    for k, v in data.items():
        if isinstance(v, str):
            # Python str slicing is by characters (Unicode), so no UTF-8
            # boundary issue — this correctly truncates at a character boundary.
            if len(v) > _DATA_CAP:
                out[k] = v[:_DATA_CAP] + _ELISION
            else:
                out[k] = v
        else:
            # Non-string values (lists, dicts, ints…): check JSON size.
            # If small, keep the typed value; if large, store a size hint
            # so the cap is visible and the line size stays bounded.
            try:
                b = json.dumps(v, separators=(",", ":"), ensure_ascii=False)
                if len(b.encode("utf-8")) > _DATA_CAP:
                    out[k] = f"<capped: {len(b.encode('utf-8'))} bytes>"
                else:
                    out[k] = v
            except (TypeError, ValueError):
                out[k] = v
    return out


def _next_seq() -> int:
    global _seq
    with _seq_lock:
        _seq += 1
        return _seq


def log(comp, evt, lvl="info", *, proj="", agent="", gen=0, session="",
        dispatch_id="", slug="", pr=0, caused_by="", msg="", data=None):
    """Append one JSONL event. Best-effort: returns the event id (for
    caused_by chaining) and NEVER raises. The envelope key order mirrors
    Go's struct order; absent correlation keys are omitted so equivalent
    events share a key set."""
    seq = _next_seq()
    eid = f"{_PROC_TOKEN}-{seq}"
    try:
        now = datetime.now(timezone.utc)
        env = {
            "ts": now.strftime("%Y-%m-%dT%H:%M:%S.%f") + "Z",
            "seq": seq,
            "type": evt,
            "lvl": lvl or "info",
            "comp": comp,
            "pid": _PID,
        }
        # omitempty-equivalent: only emit set correlation keys.
        if proj:
            env["proj"] = proj
        if agent:
            env["agent"] = agent
        if gen:
            env["gen"] = gen
        if session:
            env["session"] = session
        if dispatch_id:
            env["dispatch_id"] = dispatch_id
        if slug:
            env["slug"] = slug
        if pr:
            env["pr"] = pr
        if caused_by:
            env["caused_by"] = caused_by
        env["id"] = eid
        if msg:
            env["msg"] = msg
        capped = _cap_data(data)
        if capped:
            env["data"] = capped

        line = json.dumps(
            env, separators=(",", ":"), ensure_ascii=False,
        ).encode("utf-8") + b"\n"

        d = dir()
        try:
            os.makedirs(d, exist_ok=True)
        except OSError:
            pass
        path = os.path.join(d, _filename(comp, now))
        fd = os.open(path, os.O_WRONLY | os.O_APPEND | os.O_CREAT, 0o644)
        try:
            os.write(fd, line)
        finally:
            os.close(fd)
    except Exception:
        # Fire-and-forget: a logging failure must never break a tick.
        return eid
    return eid


def prune_older_than(max_age_s: float = _RETENTION_S) -> None:
    """Delete logs/fleet-*.jsonl whose filename date is older than
    max_age_s (day-granular: a file dated exactly max_age_s ago is kept).
    Best-effort; mirrors Go fleetlog.PruneOlderThan."""
    d = dir()
    try:
        names = os.listdir(d)
    except OSError:
        return
    now = datetime.now(timezone.utc)
    today_midnight = now.replace(hour=0, minute=0, second=0, microsecond=0)
    cutoff = today_midnight.timestamp() - max_age_s
    for name in names:
        if not (name.startswith("fleet-") and name.endswith(".jsonl")):
            continue
        # fleet-<YYYY-MM-DD>-...
        parts = name.split("-", 4)
        if len(parts) < 4:
            continue
        date_str = f"{parts[1]}-{parts[2]}-{parts[3]}"
        try:
            d_dt = datetime.strptime(date_str, "%Y-%m-%d").replace(
                tzinfo=timezone.utc)
        except ValueError:
            continue
        if d_dt.timestamp() < cutoff:
            try:
                os.remove(os.path.join(d, name))
            except OSError:
                pass


def maybe_prune_daily(max_age_s: float = _RETENTION_S,
                      throttle_s: float = _PRUNE_THROTTLE_S) -> bool:
    """Run prune_older_than at most once per throttle_s, guarded by the
    mtime of logs/.last-prune. Returns True if a prune ran this call.
    Best-effort: NEVER raises. Called from the coord tick (the regularly
    running, log-producing process) — no `fleet gc` dependency."""
    try:
        d = dir()
        marker = os.path.join(d, _LAST_PRUNE)
        now = time.time()
        try:
            last = os.stat(marker).st_mtime
            if now - last < throttle_s:
                return False  # throttled: skip the readdir entirely
        except OSError:
            pass  # no marker yet -> prune now
        prune_older_than(max_age_s)
        try:
            os.makedirs(d, exist_ok=True)
            # Touch the marker (create or update mtime).
            with open(marker, "a"):
                os.utime(marker, None)
        except OSError:
            pass
        return True
    except Exception:
        return False
