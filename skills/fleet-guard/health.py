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


def reconcile_pid(agent_id: str) -> bool:
    """Drift recovery for the P0 spawn-pid bug (2026-05-13).

    spawn.Spawn (Go side) records the real engine pid via the resolver
    that walks the tmux pane child tree. But the pid can drift if claude
    internally execs — e.g. on /remote-control reconnect, on auto-update
    paths — leaving agent.Record.PID pointing at a process that no
    longer exists.

    The fleet-guard Stop hook fires INSIDE the claude process on every
    assistant turn, so its own ancestor chain is guaranteed to contain
    the live claude pid (it's our parent's parent's parent... up the
    fork chain to the tmux-wrapper shell). When the recorded pid is
    dead, we walk our own ancestry until we find a process whose argv
    matches the agent's tmux session marker (fleet-coord-<id>) OR the
    engine binary name, and write that pid back.

    Returns True when no action was needed (recorded pid is alive) OR
    when reconciliation succeeded. False when the record is missing,
    the resolver found no live successor, or the rewrite failed.

    This widens OWNED_FIELDS to include "pid" specifically for this
    self-heal path. The skill-vs-dispatch ownership partition in
    SKILL.md still holds: dispatch is authoritative for the INITIAL
    pid (set in internal/spawn/spawn.go after the pane-tree resolver
    converges). The skill only writes pid as a corrective when the
    recorded pid is observably dead while the host claude is observably
    alive (we wouldn't be running this hook otherwise). Without this
    widening, the bug fix in Go doesn't survive a single claude exec.
    """
    record = read_record(agent_id)
    if record is None:
        return False
    recorded = record.get("pid", 0)
    try:
        recorded_int = int(recorded)
    except (TypeError, ValueError):
        recorded_int = 0
    # Codex iter-3 hardening (2026-05-14): a live recorded pid is not
    # sufficient to skip reconciliation. The Go-side spawn resolver
    # falls back to the pane pid (the wrapper shell `sh -c "claude
    # ..."`) on timeout. The shell stays alive for the whole session,
    # so a naive _pid_alive check would let the wrong-but-live pid
    # persist forever. Re-resolve when the recorded pid points at a
    # shell argv OR (for coord-spawn agents only) its argv does NOT
    # contain the disambiguator. The resolver itself is idempotent —
    # if no better pid is found we keep the recorded pid via the
    # _resolve_self_pid returning None branch.
    #
    # Codex iter-6 (2026-05-14): the disambiguator probe is scoped
    # to coord-spawn agents (task_id starts with "coord-"). Plain
    # workers and handoff replacements don't get the
    # `fleet-coord-<id>` injection, so checking for the needle on
    # every Stop hook would force a ps -ax scan on every turn AND
    # could reconcile a healthy worker pid to nothing.
    is_coord_spawn = str(record.get("task_id", "")).startswith("coord-")
    if recorded_int > 0 and _pid_alive(recorded_int):
        if not _recorded_pid_looks_like_wrapper(
            recorded_int, agent_id, check_disambiguator=is_coord_spawn,
        ):
            return True
    new_pid = _resolve_self_pid(agent_id)
    if new_pid is None or new_pid <= 0:
        return False
    # If resolver returned the same pid we already have, no-op write.
    if new_pid == recorded_int:
        return True
    record["pid"] = new_pid
    record["last_activity_ts"] = now_rfc3339()
    return _atomic_write_json(agent_record_path(agent_id), record)


def _recorded_pid_looks_like_wrapper(
    pid: int, agent_id: str, check_disambiguator: bool = True,
) -> bool:
    """Return True when the recorded pid's argv suggests the resolver
    earlier latched onto a wrapper shell rather than the real engine.
    Triggers reconciliation re-resolution.

    Heuristics (best-effort — return False on any probe failure so we
    don't churn the record on a transient ps blip):
      - argv first token is a known POSIX shell command name → wrapper
      - check_disambiguator=True AND agent_id non-empty AND
        `fleet-coord-<agent_id>` is NOT in the argv → wrapper /
        pid-recycled-into-something-else

    check_disambiguator MUST be False for non-coord-spawn agents
    (plain workers, handoff replacements). Only coord-spawn dispatches
    inject `fleet-coord-<id>` into the engine's argv, so checking the
    needle on a worker would force every Stop hook through the slow
    ps -ax resolve path AND could repeatedly try to reconcile a
    healthy worker pid that simply doesn't carry the token.
    """
    if pid <= 0:
        return False
    try:
        import subprocess
        out = subprocess.run(
            ["ps", "-o", "args=", "-p", str(pid)],
            capture_output=True, text=True, check=False, timeout=5,
        ).stdout
    except (FileNotFoundError, subprocess.TimeoutExpired, OSError):
        return False
    argv = out.strip()
    if not argv:
        # ps returned nothing — the process died between _pid_alive
        # and now, or argv is unreadable. Either way, return False so
        # the caller takes the no-op branch (will probably re-fire on
        # the next Stop hook with a different verdict).
        return False
    if _is_shell_argv(argv):
        return True
    if check_disambiguator and agent_id:
        needle = f"fleet-coord-{agent_id}"
        if needle not in argv:
            # Recorded pid is non-shell but doesn't carry our coord
            # disambiguator either. Could be pid recycle into an
            # unrelated process. Reconcile.
            return True
    return False


def _pid_alive(pid: int) -> bool:
    """Liveness probe via kill(pid, 0). True when the pid exists (errno
    EPERM also counts as alive — the process is there, we just can't
    signal it). False on ESRCH (no such process) or any other error.

    Mirrors internal/spawn/dispatch_recovery.go:pidAlive on the Go side
    so the two-language pair agree on what 'alive' means."""
    if pid <= 0:
        return False
    try:
        os.kill(pid, 0)
        return True
    except PermissionError:
        # EPERM: process exists, we don't have signal permission. Alive.
        return True
    except (OSError, ProcessLookupError):
        # ESRCH or other transport error. Treat as dead — the worst
        # case is we trigger reconciliation when we didn't need to.
        return False


def _resolve_self_pid(agent_id: str) -> int | None:
    """Walk this process's ancestor chain looking for a claude (or wrapped
    engine) process whose argv matches the agent's tmux session marker.

    Returns the pid of the matching ancestor, or None if no match.

    Match strategy:
      1. Argv contains 'fleet-coord-<agent_id>' — the strongest signal
         (coord-spawn dispatches inject this via --remote-control).
      2. Argv command is 'claude' (or codex etc.) — the engine binary.
      3. Fallback to None — we can't safely guess.

    The fleet-guard hook runs inside the claude process. claude → tmux
    pane shell → tmux server is the typical ancestry; we want the
    deepest 'claude' ancestor. We walk up via ppid until we hit pid 1
    or a process whose argv we can't read.

    Failure modes (returns None):
      - ps not on PATH (vanishingly rare; the skill needs ps for
        liveness probes anyway).
      - No ancestor matches (the hook is somehow running outside a
        claude session — drift not recoverable).
      - ps output unparseable.
    """
    needle = f"fleet-coord-{agent_id}" if agent_id else ""
    # Build pid → (ppid, args) map via one ps call.
    procs: dict[int, tuple[int, str]] = {}
    try:
        import subprocess
        out = subprocess.run(
            ["ps", "-o", "pid,ppid,args", "-ax"],
            capture_output=True, text=True, check=False, timeout=5,
        ).stdout
    except (FileNotFoundError, subprocess.TimeoutExpired, OSError):
        return None
    for line in out.splitlines()[1:]:
        line = line.lstrip()
        if not line:
            continue
        parts = line.split(None, 2)
        if len(parts) < 3:
            continue
        try:
            pid = int(parts[0])
            ppid = int(parts[1])
        except ValueError:
            continue
        procs[pid] = (ppid, parts[2])

    # Walk ancestry from our own pid upward. Bounded by max depth so a
    # corrupt ps output (cycle) can't loop forever.
    #
    # Match rules (codex iter-3 hardening, 2026-05-14):
    #   - Disambiguator match short-circuits ONLY when the matched
    #     entry is NOT a shell argv. A `sh -c "claude --remote-control
    #     fleet-coord-X ..."` wrapper carries the disambiguator
    #     transitively; matching on that shell would return the
    #     wrapper pid and let dead claudes show up as alive (the
    #     wrapper outlives the engine). Mirrors Go-side resolver
    #     pidresolver.go:findEngineDescendant.
    #   - Engine-name match still uses the "walk further if there is
    #     a deeper disambiguator hit" heuristic, with the same shell
    #     skip applied.
    cur = os.getpid()
    for _ in range(256):
        info = procs.get(cur)
        if info is None:
            break
        _, args = info
        if needle and needle in args and not _is_shell_argv(args):
            return cur
        if _argv_command_is(args, "claude") or _argv_command_is(args, "codex"):
            # Engine match — return ONLY if we haven't been able to
            # match the disambiguator on a non-shell ancestor yet.
            # Walk one more step in case a deeper engine ancestor
            # carries the disambiguator (and is not a shell).
            best = cur
            cur = info[0]
            while True:
                info2 = procs.get(cur)
                if info2 is None:
                    break
                _, args2 = info2
                if needle and needle in args2 and not _is_shell_argv(args2):
                    return cur
                cur = info2[0]
                if cur <= 1:
                    break
            return best
        cur = info[0]
        if cur <= 1:
            break
    return None


def _argv_command_is(argv: str, name: str) -> bool:
    """Match the first whitespace-separated token of argv against name,
    stripping a leading path. Mirrors argvCommandIs in Go-side
    pidresolver.go."""
    s = argv.lstrip()
    space = s.find(" ")
    if space < 0:
        first = s
    else:
        first = s[:space]
    slash = first.rfind("/")
    if slash >= 0:
        first = first[slash + 1:]
    return first == name


def _is_shell_argv(argv: str) -> bool:
    """Report whether argv looks like a POSIX shell wrapper (`sh`,
    `bash`, `zsh`, etc.). Mirrors Go-side isShellArgv in
    pidresolver.go. Used to skip wrapper-shell ancestors that carry
    the disambiguator transitively (e.g. `sh -c "claude --remote-
    control fleet-coord-X ..."`) so reconcile_pid doesn't rewrite the
    record to point at a wrapper shell that can outlive the engine.
    """
    s = argv.lstrip()
    space = s.find(" ")
    if space < 0:
        first = s
    else:
        first = s[:space]
    slash = first.rfind("/")
    if slash >= 0:
        first = first[slash + 1:]
    return first in {"sh", "bash", "zsh", "dash", "ksh", "fish"}


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
