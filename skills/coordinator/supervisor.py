"""Coordinator supervisor loop — event-driven reconcile + sparse stuck-check.

The classic `tick()` runs once per skill invocation: NB-flock the
coordinator.lock, do one reconcile/drain/dispatch pass, exit. That works
for the dispatch-and-die pattern but stops watching workers the moment
the coord agent's turn ends. Issue #79 surfaces the actual operator pain:
workers go silent and nothing notices.

The supervisor wraps the existing tick body in a long-lived loop that
keeps the coordinator.lock for its entire duration. Cheap signals
(state.json mtime stat) drive it; expensive operations (gh API calls,
tasks.md mutations) only fire when something actually changed. A sparse
stuck-check (default every 10 polls = 5 min on the default 30 s
interval) catches workers that died silently — workers that aren't
writing state.json at all.

Recovery ladder for stuck workers (all conditions must hold):

   ┌──────────────┐  no progress  ┌────────┐  no progress   ┌──────────┐
   │ (working OK) │ ────────────▶ │ nudge  │ ─────────────▶ │ escalate │
   └──────────────┘  ≥3 polls     │ inbox  │  one full      │ task →   │
                                  └────────┘  stuck-cycle   │ blocked  │
                                                            └──────────┘
                                                                  │
                                                       no progress │
                                                       one cycle   ▼
                                                            ┌──────────┐
                                                            │ phase →  │
                                                            │ blocked  │
                                                            │ (stop    │
                                                            │ polling) │
                                                            └──────────┘

Configuration is env-only — there is no per-project supervisor config,
because the operator's pain is "workers go idle and the coord doesn't
notice", and the same defaults work for every project. Setting any
threshold to 0 disables that behavior; setting POLL_INTERVAL_S=0 falls
back to the legacy single-tick behavior so tests + crashed-coord
recovery aren't dependent on the supervisor running.
"""
from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
import time
from dataclasses import dataclass, field
from pathlib import Path
from typing import Callable, Iterable


# ---------- env-overridable knobs ----------


def _env_int(name: str, default: int) -> int:
    """Return int(os.environ[name]) clamped to ≥0, or default on parse error."""
    raw = os.environ.get(name, "")
    if raw == "":
        return default
    try:
        n = int(raw)
    except ValueError:
        return default
    if n < 0:
        return default
    return n


def env_poll_interval_s() -> int:
    """30 s default. 0 disables the supervisor (single tick fallback)."""
    return _env_int("FLEET_COORD_POLL_INTERVAL_S", 30)


def env_stuck_check_every() -> int:
    """Run stuck-check every Nth poll. 10 default = 5 min on 30 s polls.
    0 disables stuck-checking entirely (cheap mtime polling continues)."""
    return _env_int("FLEET_COORD_STUCK_CHECK_EVERY", 10)


def env_stuck_threshold_s() -> int:
    """Heartbeat-stale threshold. Default 180 s = 3 min."""
    return _env_int("FLEET_COORD_STUCK_THRESHOLD_S", 180)


def env_stuck_polls() -> int:
    """Consecutive stuck-check passes required before recovery fires.
    Default 3 — guards against transient idle (one stuck-check pass
    sees a brief pause; we want sustained inactivity)."""
    return _env_int("FLEET_COORD_STUCK_POLLS", 3)


def env_nudge_cooldown_s() -> int:
    """Min gap between nudges to the same worker. Default 120 s."""
    return _env_int("FLEET_COORD_NUDGE_COOLDOWN_S", 120)


def env_poll_max_s() -> int:
    """Hard cap on the supervisor's wall-clock duration. Default 4 h.
    Past this the loop exits cleanly so the coord agent's turn ends."""
    return _env_int("FLEET_COORD_POLL_MAX_S", 14400)


def env_idle_ttl_h() -> int:
    """Hours of inactivity before an idle agent record is auto-archived
    via `fleet rm <id>` (dashboard-accumulation-f-4421 Sub-fix C).

    Default 24, range 1..720 (one month). 0 disables the sweep entirely
    — matches the supervisor's existing zero-disables convention. Out-of-
    range values (negative, > 720) fall back to the default rather than
    silently allowing an unbounded TTL that would never archive.
    """
    raw = os.environ.get("FLEET_COORD_IDLE_TTL_H", "")
    if raw == "":
        return 24
    try:
        n = int(raw)
    except ValueError:
        return 24
    if n == 0:
        return 0
    if n < 0 or n > 720:
        return 24
    return n


# Phases we treat as terminal — workers in these phases never get
# stuck-checked. Mirrors workers.isTerminalPhase + the parse.py task
# statuses used by the reconcile path. We keep one local constant rather
# than importing because supervisor.py runs inside the same skill: the
# canonical enum lives in internal/workers/workers.go and parse.py;
# this constant only needs to track them at human-edit speed.
TERMINAL_WORKER_PHASES = frozenset({"done", "blocked", "failed"})


# Fallback cadence for periodic_full_reconcile when stuck_check_every=0
# disables the recovery ladder. PR/CI sweeps for in-review tasks must
# keep running even with the ladder off — otherwise an operator who
# tunes off recovery (e.g., for a long manual-review session) silently
# loses CI-merge-driven status flips. 10 polls = 5 min on the default
# 30 s base; matches the recovery cadence default for parity.
_PERIODIC_RECONCILE_FALLBACK_EVERY = 10


# ---------- data classes ----------


@dataclass
class WorkerSupervisorState:
    """Per-slug supervisor bookkeeping. Persisted as a sub-dict in
    coord-state.json under "supervisor.<slug>"."""

    slug: str
    last_phase: str = ""
    consecutive_stuck_polls: int = 0
    nudged_at: float = 0.0
    escalated_at: float = 0.0

    def to_dict(self) -> dict:
        return {
            "last_phase": self.last_phase,
            "consecutive_stuck_polls": self.consecutive_stuck_polls,
            "nudged_at": self.nudged_at,
            "escalated_at": self.escalated_at,
        }

    @classmethod
    def from_dict(cls, slug: str, d: dict) -> "WorkerSupervisorState":
        return cls(
            slug=slug,
            last_phase=str(d.get("last_phase", "") or ""),
            consecutive_stuck_polls=int(d.get("consecutive_stuck_polls", 0) or 0),
            nudged_at=float(d.get("nudged_at", 0.0) or 0.0),
            escalated_at=float(d.get("escalated_at", 0.0) or 0.0),
        )


@dataclass
class SupervisorConfig:
    """Runtime knobs for the supervisor loop. All env-resolved at the
    boundary; tests pass explicit values. Zeroes have specific
    meanings (see field comments)."""

    poll_interval_s: int = 30          # 0 → single-tick fallback
    stuck_check_every: int = 10         # 0 → no stuck-checks (cheap polling only)
    stuck_threshold_s: int = 180        # 0 → never flag stuck (heartbeat unbounded)
    stuck_polls: int = 3                # 0 → fire recovery on first stuck pass
    nudge_cooldown_s: int = 120
    poll_max_s: int = 14400             # hard wall-clock cap

    @classmethod
    def from_env(cls) -> "SupervisorConfig":
        return cls(
            poll_interval_s=env_poll_interval_s(),
            stuck_check_every=env_stuck_check_every(),
            stuck_threshold_s=env_stuck_threshold_s(),
            stuck_polls=env_stuck_polls(),
            nudge_cooldown_s=env_nudge_cooldown_s(),
            poll_max_s=env_poll_max_s(),
        )


# ---------- helpers ----------


def safe_mtime(path: Path | str) -> float:
    """Return path's mtime in seconds since epoch, or 0.0 on any error.

    The mtime poll is the supervisor's hot path; it must NEVER raise.
    Missing files (worker hasn't written state.json yet) and OS errors
    return the same sentinel 0.0 — callers compare against the
    previously seen mtime, so 0.0 vs 0.0 reads as "no change".
    """
    try:
        return os.stat(str(path)).st_mtime
    except OSError:
        return 0.0


def tmux_session_alive(session: str, *, timeout_s: float = 5.0) -> bool:
    """Return True iff `tmux has-session -t <session>` exits 0.

    Used by the stuck-idle gate: if the agent's tmux session is gone,
    nudging it is pointless (no one to deliver the inbox to). Failure
    modes (tmux not on PATH, hung server) return False — the
    supervisor falls through to NOT flagging the worker as stuck-idle,
    which is the conservative choice (we can't tell, so don't escalate).

    Empty session name returns False — there's nothing to probe.
    """
    if not session:
        return False
    try:
        proc = subprocess.run(
            ["tmux", "has-session", "-t", session],
            capture_output=True, text=True,
            timeout=timeout_s, check=False,
        )
    except (FileNotFoundError, subprocess.TimeoutExpired, OSError):
        return False
    return proc.returncode == 0


# tmux session naming convention from internal/tmux/tmux.go: every
# worker session is `fleet-<agent_id>`. This matches the dispatch
# fixture in tests and the production code path.
def session_name_for_agent(agent_id: str) -> str:
    if not agent_id:
        return ""
    return f"fleet-{agent_id}"


def _parse_rfc3339(updated_at: str) -> float | None:
    """Parse Go's time.Time JSON (RFC3339 with optional fractional s)
    to a Unix timestamp. Returns None on parse failure."""
    if not updated_at:
        return None
    from datetime import datetime, timezone
    try:
        ts = datetime.fromisoformat(updated_at.replace("Z", "+00:00"))
    except ValueError:
        return None
    if ts.tzinfo is None:
        ts = ts.replace(tzinfo=timezone.utc)
    return ts.timestamp()


def is_stuck_idle(
    state: dict | None,
    sup: WorkerSupervisorState,
    *,
    cfg: SupervisorConfig,
    session_alive: bool,
    now_unix: float,
) -> bool:
    """Decide whether a worker has met all four stuck-idle conditions:

      1. Phase is non-terminal.
      2. last_heartbeat (state.json.updated_at) older than the threshold.
      3. consecutive_stuck_polls (post-increment) ≥ cfg.stuck_polls.
      4. The agent's tmux session is alive.

    The function ASSUMES the caller has already incremented (or reset)
    sup.consecutive_stuck_polls before calling. Separating the gate
    from the counter mutation keeps the testable contract narrow:
    given a state, am I stuck?

    state == None means "no state.json on disk" — we can't make a
    judgement (the worker is brand-new or its dir was archived
    between polls), so we return False to skip recovery.
    """
    if state is None:
        return False
    phase = str(state.get("phase", "") or "")
    if phase in TERMINAL_WORKER_PHASES:
        return False
    if cfg.stuck_threshold_s <= 0:
        return False
    updated_at = state.get("updated_at", "")
    last_heartbeat = _parse_rfc3339(str(updated_at) if updated_at else "")
    if last_heartbeat is None:
        # No parseable heartbeat — treat as not-stuck (the worker just
        # started and hasn't published yet).
        return False
    if now_unix - last_heartbeat <= cfg.stuck_threshold_s:
        return False
    if sup.consecutive_stuck_polls < cfg.stuck_polls:
        return False
    if not session_alive:
        return False
    return True


# ---------- recovery actions ----------


_NUDGE_BODY = (
    "[OPERATOR] You appear idle. Reply with current state, or run "
    "/coordinator-helper to surface a question."
)


def nudge_worker(
    agent_id: str,
    *,
    fleet_home: Path | str,
) -> str:
    """Drop the canonical nudge body into ~/.fleet/inbox/<agent_id>.md.

    Returns the path written on success, "" on any error. Fail-soft:
    a missing inbox dir, a bad agent_id, or an I/O error must NOT
    crash the supervisor — the next stuck-check pass will retry.

    tmp + rename so a partial write never lands in fleet-guard's
    SessionStart hook half-formed (matches dispatch.write_worker_inbox).
    """
    if not _is_agent_id(agent_id):
        return ""
    home = Path(fleet_home)
    inbox_dir = home / "inbox"
    try:
        inbox_dir.mkdir(parents=True, exist_ok=True)
    except OSError:
        return ""
    target = inbox_dir / f"{agent_id}.md"
    try:
        fd, tmp = tempfile.mkstemp(prefix=f"{agent_id}.tmp.", dir=str(inbox_dir))
    except OSError:
        return ""
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as fh:
            fh.write(_NUDGE_BODY + "\n")
            fh.flush()
            os.fsync(fh.fileno())
        os.replace(tmp, target)
    except OSError:
        try:
            os.unlink(tmp)
        except FileNotFoundError:
            pass
        return ""
    return str(target)


# Agent IDs are 8 lowercase hex chars (mirrors agent.NewID + the regex
# in dispatch.py). We re-validate here so a malformed mapping in
# coord-state.json can't make us write to a junk inbox path.
def _is_agent_id(s: str) -> bool:
    if not s or len(s) != 8:
        return False
    return all(c in "0123456789abcdef" for c in s)


# ---------- structured log lines (operator-visible in tmux pane) ----------


def emit(line: str, *, stream=None) -> None:
    """Print a structured log line. Defaults to stdout; tests inject a
    StringIO. Newline appended if missing. The supervisor prefixes
    every line with `[coord]` so operators tail-grepping the coord's
    tmux pane can isolate supervisor output from worker output."""
    out = stream if stream is not None else sys.stdout
    if not line.endswith("\n"):
        line = line + "\n"
    try:
        out.write(line)
        out.flush()
    except OSError:
        # Closed pipe (operator detached the tmux pane mid-emit).
        # Swallow — supervisor must not crash on log failure.
        pass


# ---------- supervisor state (sub-dict in coord-state.json) ----------


_SUPERVISOR_KEY = "supervisor"
_AGENT_IDS_KEY = "worker_agent_ids"
_SUBAGENT_IDS_KEY = "worker_subagent_ids"

# Claude Agent-tool subagent IDs are opaque strings produced by the host
# Claude Code session. Phase C only needs them as display tokens — we
# truncate to the first 8 chars on the TUI — so we don't pin a strict
# shape here. Defensive bounds keep a malformed coord-state from blowing
# up the supervisor: empty / over-long / non-printable rejected.
_SUBAGENT_ID_MAX_LEN = 128


def _is_subagent_id(s: str) -> bool:
    """Loose validation for an Agent-tool subagent_id token. The host
    Claude doesn't document the exact shape, so we accept any non-empty
    printable string under the length cap. Whitespace is rejected (the
    coord agent strips before passing the token in).

    Defense-in-depth: shell metacharacters (`;`, `|`, `&`, `$`, backtick,
    `(`, `)`, `<`, `>`, `\\`, single+double quotes) are rejected. The
    coord agent passes this id into a Bash tool invocation (per
    SKILL.md's Worker dispatch protocol step 3); rejecting the
    metacharacter set blocks shell-injection-via-LLM-output even when
    the operator's Bash invocation forgets to quote the value. Host
    Claude's Agent-tool subagent_id format in practice is alphanumeric
    with hyphens / underscores, so this filter never trips on a
    legitimate id."""
    if not isinstance(s, str):
        return False
    if not s or len(s) > _SUBAGENT_ID_MAX_LEN:
        return False
    # Shell-metacharacter blocklist. We use a blocklist (not allowlist)
    # because the host Claude could in principle return any printable
    # subagent_id format and we don't want to over-fit. The blocklist
    # covers the operator-visible shell injection vectors.
    _SHELL_META = "$`;|&<>()\\\"'"
    for ch in s:
        if ch.isspace():
            return False
        if not ch.isprintable():
            return False
        if ch in _SHELL_META:
            return False
    return True


def load_supervisor_states(coord_state: dict) -> dict[str, WorkerSupervisorState]:
    """Pull the per-slug supervisor sub-dict out of coord-state. Returns
    an empty dict on any malformed shape — supervisor state is
    rebuilt cheaply on the next pass."""
    raw = coord_state.get(_SUPERVISOR_KEY, {})
    if not isinstance(raw, dict):
        return {}
    out: dict[str, WorkerSupervisorState] = {}
    for slug, val in raw.items():
        if not isinstance(slug, str) or not isinstance(val, dict):
            continue
        out[slug] = WorkerSupervisorState.from_dict(slug, val)
    return out


def save_supervisor_states(
    coord_state: dict,
    states: dict[str, WorkerSupervisorState],
) -> None:
    """Write the supervisor sub-dict back into coord-state in place."""
    coord_state[_SUPERVISOR_KEY] = {
        slug: s.to_dict() for slug, s in states.items()
    }


def load_agent_id_map(coord_state: dict) -> dict[str, str]:
    """Pull the slug → agent_id mapping written on dispatch."""
    raw = coord_state.get(_AGENT_IDS_KEY, {})
    if not isinstance(raw, dict):
        return {}
    out: dict[str, str] = {}
    for k, v in raw.items():
        if isinstance(k, str) and isinstance(v, str) and _is_agent_id(v):
            out[k] = v
    return out


def remember_agent_id(coord_state: dict, slug: str, agent_id: str) -> None:
    """Persist a slug → agent_id mapping inside coord-state.

    Called from loop._apply_dispatch on a successful dispatch so the
    supervisor's nudge path can address the worker's inbox without
    re-parsing tasks.md notes.
    """
    if not slug or not _is_agent_id(agent_id):
        return
    raw = coord_state.get(_AGENT_IDS_KEY, {})
    if not isinstance(raw, dict):
        raw = {}
    raw[slug] = agent_id
    coord_state[_AGENT_IDS_KEY] = raw


def forget_agent_id(coord_state: dict, slug: str) -> None:
    """Drop the mapping when a task transitions to terminal."""
    raw = coord_state.get(_AGENT_IDS_KEY, {})
    if isinstance(raw, dict) and slug in raw:
        del raw[slug]
        coord_state[_AGENT_IDS_KEY] = raw
    # Drop the parallel subagent_id mapping at the same beat — the
    # supervisor never wants to render a stale subagent chip on a
    # terminal task. Keys diverge only when a coord forgets to call
    # register_subagent (silent miss → empty chip), never when forgetting.
    forget_subagent_id(coord_state, slug)


def load_subagent_id_map(coord_state: dict) -> dict[str, str]:
    """Pull the slug → Claude Agent-tool subagent_id mapping written by
    the coord agent immediately after each Agent tool dispatch. Used by
    the TUI to render `· <8-char>` cross-reference chips on worker rows
    so the operator can correlate a fleet row with Claude's "N local
    agents" indicator (issue #94).

    Returns an empty dict on any malformed shape — the chip simply
    disappears from the row, no crash.
    """
    raw = coord_state.get(_SUBAGENT_IDS_KEY, {})
    if not isinstance(raw, dict):
        return {}
    out: dict[str, str] = {}
    for k, v in raw.items():
        if isinstance(k, str) and _is_subagent_id(v):
            out[k] = v
    return out


def remember_subagent_id(coord_state: dict, slug: str, subagent_id: str) -> None:
    """Persist a slug → subagent_id mapping inside coord-state.

    Called from the `register_subagent` helper after the coord agent
    captures the Agent tool's response. Silently no-ops on bad input
    (caller has already done its own validation; this is the last
    line of defense before disk).
    """
    if not slug or not _is_subagent_id(subagent_id):
        return
    raw = coord_state.get(_SUBAGENT_IDS_KEY, {})
    if not isinstance(raw, dict):
        raw = {}
    raw[slug] = subagent_id
    coord_state[_SUBAGENT_IDS_KEY] = raw


def forget_subagent_id(coord_state: dict, slug: str) -> None:
    """Drop the subagent_id mapping when a task transitions to terminal.
    Idempotent. Coord skill calls this implicitly via forget_agent_id —
    direct calls are reserved for tests + the register_subagent CLI's
    `--clear` mode (future)."""
    raw = coord_state.get(_SUBAGENT_IDS_KEY, {})
    if isinstance(raw, dict) and slug in raw:
        del raw[slug]
        coord_state[_SUBAGENT_IDS_KEY] = raw


# ---------- the loop driver ----------


@dataclass
class SupervisorResult:
    """Aggregate stats from one supervisor session — useful for tests
    and the JSON summary printed by main()."""

    iterations: int = 0
    reconcile_calls: int = 0
    stuck_check_passes: int = 0
    nudges_sent: int = 0
    escalations: int = 0
    blocks: int = 0
    # dashboard-accumulation-f-4421 Sub-fix C: count of agent records
    # auto-archived during this supervisor session via `fleet rm <id>`.
    idle_archives: int = 0
    exit_reason: str = ""
    errors: list[str] = field(default_factory=list)


@dataclass
class WorkerProbe:
    """Identifies a worker the supervisor should keep watching during
    one supervisor session. Built from tasks.md after the initial tick
    and refreshed on every reconcile pass.

    `live_worker`: True iff a worker subprocess is expected to be writing
    state.json. Tasks at status=in-progress have a live worker; tasks at
    status=in-review do NOT — the worker exited after opening the PR
    and the only thing left to advance the task is `gh pr` CI checks.
    Stuck-detection only applies to live workers (no point nudging an
    inbox owned by an exited agent); the periodic_full_reconcile hook
    covers in-review tasks separately.
    """

    slug: str
    state_path: Path
    agent_id: str = ""
    tmux_session: str = ""
    live_worker: bool = True


def build_worker_probes(
    *,
    project: str,
    home: Path,
    tasks: Iterable,
    agent_id_map: dict[str, str],
) -> list[WorkerProbe]:
    """Return one WorkerProbe per non-terminal in-flight task.

    `tasks` is parse.File.tasks; we accept any iterable of objects
    with `.slug` and `.status` so tests can pass simple dataclasses.
    """
    probes: list[WorkerProbe] = []
    for t in tasks:
        if t.status not in ("in-progress", "in-review"):
            continue
        slug = t.slug
        state_path = home / "projects" / project / "workers" / slug / "state.json"
        agent_id = agent_id_map.get(slug, "")
        live_worker = (t.status == "in-progress")
        probes.append(WorkerProbe(
            slug=slug,
            state_path=state_path,
            agent_id=agent_id,
            tmux_session=session_name_for_agent(agent_id),
            live_worker=live_worker,
        ))
    return probes


def has_active_workers(probes: list[WorkerProbe]) -> bool:
    """True iff at least one probe is non-terminal. Cheap: the caller
    refreshes the probe list whenever it re-reads tasks.md."""
    return len(probes) > 0


def run_supervisor(
    *,
    cfg: SupervisorConfig,
    project: str,
    home: Path,
    fleet_bin: str,
    sleep_fn: Callable[[float], None] = time.sleep,
    now_fn: Callable[[], float] = time.time,
    refresh_probes: Callable[[], list[WorkerProbe]],
    reconcile_one: Callable[[WorkerProbe], None],
    write_state: Callable[[], None],
    periodic_full_reconcile: Callable[[], None] | None = None,
    log_stream=None,
) -> SupervisorResult:
    """Drive the smart polling loop. Returns aggregate stats.

    Caller (loop._tick_locked) holds the coordinator.lock for the entire
    duration. Caller is also responsible for the FIRST tick (initial
    reconcile/drain/dispatch) before invoking the supervisor — the
    supervisor only watches what's already in flight.

    Hooks:
      refresh_probes  — re-read tasks.md, return current probe list.
                        Called once per outer iteration so a worker
                        that finished mid-loop drops out cleanly.
      reconcile_one   — run reconcile against ONE probe (a worker
                        whose state.json mtime advanced).
      write_state     — persist coord-state.json after a real event.

    The supervisor mutates per-slug supervisor bookkeeping (nudge
    timestamps, stuck-poll counters) inside coord-state.json via the
    write_state hook. It does NOT mutate tasks.md directly — escalation
    + block actions go through fleet CLI calls inside helpers below.
    """
    res = SupervisorResult()
    if cfg.poll_interval_s <= 0:
        # Disabled — the legacy single-tick path already ran in
        # _tick_locked; nothing to do.
        res.exit_reason = "supervisor-disabled"
        return res

    started_at = now_fn()
    last_mtimes: dict[str, float] = {}
    poll_count = 0

    probes = refresh_probes()
    for p in probes:
        last_mtimes[p.slug] = safe_mtime(p.state_path)

    if not has_active_workers(probes):
        res.exit_reason = "no-active-workers"
        emit("[coord] supervisor loop exiting: no active workers", stream=log_stream)
        return res

    emit(
        f"[coord] supervisor loop starting: {len(probes)} active workers, "
        f"poll={cfg.poll_interval_s}s stuck_every={cfg.stuck_check_every} "
        f"threshold={cfg.stuck_threshold_s}s",
        stream=log_stream,
    )

    while True:
        if now_fn() - started_at >= cfg.poll_max_s:
            res.exit_reason = "max-duration"
            emit("[coord] supervisor loop exiting: max duration reached", stream=log_stream)
            break

        sleep_fn(float(cfg.poll_interval_s))
        poll_count += 1
        res.iterations += 1

        # Re-read probes every iteration so a finished worker (whose
        # task transitioned to done/blocked between polls) drops out.
        # Cheap: parse.read on tasks.md is microseconds.
        probes = refresh_probes()
        if not has_active_workers(probes):
            res.exit_reason = "all-terminal"
            emit("[coord] supervisor loop exiting: all workers terminal", stream=log_stream)
            break

        # Track newly-appeared probes so they get baselined; drop
        # mtimes for slugs that left the in-flight set.
        cur_slugs = {p.slug for p in probes}
        for slug in list(last_mtimes.keys()):
            if slug not in cur_slugs:
                del last_mtimes[slug]
        for p in probes:
            last_mtimes.setdefault(p.slug, safe_mtime(p.state_path))

        # Mtime change detection — cheap stat, no API calls.
        changed: list[WorkerProbe] = []
        for p in probes:
            cur = safe_mtime(p.state_path)
            prev = last_mtimes.get(p.slug, 0.0)
            if cur > prev:
                changed.append(p)
                last_mtimes[p.slug] = cur

        if changed:
            for p in changed:
                try:
                    reconcile_one(p)
                    res.reconcile_calls += 1
                except Exception as exc:  # noqa: BLE001 — fail-soft
                    res.errors.append(f"reconcile {p.slug}: {exc}")
            try:
                write_state()
            except Exception as exc:  # noqa: BLE001
                res.errors.append(f"write_state after reconcile: {exc}")

        # Periodic full reconcile + sparse stuck-check. Decoupled
        # cadences: periodic_full_reconcile runs at every
        # `stuck_check_every` poll (or fallback _PERIODIC_RECONCILE_FALLBACK
        # when stuck_check_every=0 disables the ladder); stuck-check
        # ladder runs only when stuck_check_every > 0. codex iter-2 [P2]:
        # without the decoupling, an operator who set stuck_check_every=0
        # to disable the recovery ladder ALSO killed the in-review PR/CI
        # sweep — leaving merged-but-not-reconciled tasks frozen in
        # in-review until the supervisor exited.
        reconcile_cadence = (
            cfg.stuck_check_every if cfg.stuck_check_every > 0
            else _PERIODIC_RECONCILE_FALLBACK_EVERY
        )
        run_periodic = (
            periodic_full_reconcile is not None
            and reconcile_cadence > 0
            and poll_count % reconcile_cadence == 0
        )
        run_stuck = (
            cfg.stuck_check_every > 0
            and poll_count % cfg.stuck_check_every == 0
        )
        if run_periodic or run_stuck:
            # Periodic full reconcile FIRST. Running before stuck-check
            # means a worker that just transitioned to in-review (and
            # would otherwise be stuck-flagged on the next pass) drops
            # out of the live-worker probe set BEFORE we evaluate it.
            if run_periodic:
                try:
                    periodic_full_reconcile()
                except Exception as exc:  # noqa: BLE001
                    res.errors.append(f"periodic-reconcile: {exc}")
            if run_stuck:
                res.stuck_check_passes += 1
                try:
                    stuck_summary = _run_stuck_check_pass(
                        probes=probes,
                        project=project,
                        home=home,
                        fleet_bin=fleet_bin,
                        cfg=cfg,
                        now_unix=now_fn(),
                        log_stream=log_stream,
                    )
                    res.nudges_sent += stuck_summary.nudges
                    res.escalations += stuck_summary.escalations
                    res.blocks += stuck_summary.blocks
                except Exception as exc:  # noqa: BLE001
                    res.errors.append(f"stuck-check: {exc}")
                # dashboard-accumulation-f-4421 Sub-fix C: agent
                # auto-archive runs alongside the stuck-check pass at the
                # same cadence. Best-effort; failures log to stderr and
                # never abort the supervisor.
                try:
                    archived = _run_idle_agent_archive_pass(
                        project=project,
                        home=home,
                        fleet_bin=fleet_bin,
                        now_unix=now_fn(),
                        log_stream=log_stream,
                    )
                    res.idle_archives += archived
                except Exception as exc:  # noqa: BLE001
                    res.errors.append(f"idle-archive: {exc}")
            try:
                write_state()
            except Exception as exc:  # noqa: BLE001
                res.errors.append(f"write_state after stuck-check: {exc}")

    return res


# ---------- stuck-check helpers ----------


@dataclass
class _StuckPassResult:
    nudges: int = 0
    escalations: int = 0
    blocks: int = 0


def _run_idle_agent_archive_pass(
    *,
    project: str,
    home: Path,
    fleet_bin: str,
    now_unix: float,
    log_stream=None,
) -> int:
    """Scan ~/.fleet/agents/*.json and `fleet rm <id>` records that have
    been idle longer than FLEET_COORD_IDLE_TTL_H hours.

    A record is auto-archivable when ALL hold:
      - r.project == this supervisor's project (scope: own project only;
        every project's coord runs its own sweep against its own agents).
      - r.last_activity_ts is older than (now - TTL hours).
      - r.needs_input == False (asking agents need operator attention).
      - r.blocked == False (blocked agents need operator triage).

    Returns the count of `fleet rm` shells we invoked. Failures log to
    stderr but never abort the sweep — the next pass retries. TTL = 0
    disables the sweep entirely (no scan, returns 0).

    The TTL is read FRESH on every call rather than cached, so an
    operator toggling FLEET_COORD_IDLE_TTL_H between supervisor sessions
    doesn't need a coord restart. The supervisor.run_supervisor caller
    is responsible for guarding the cadence (only invoked on the
    stuck-check cadence — sub-second cost on a small agent dir but
    not free).
    """
    ttl_h = env_idle_ttl_h()
    if ttl_h == 0:
        return 0
    cutoff_unix = now_unix - (ttl_h * 3600)

    agents_dir = home / "agents"
    try:
        entries = list(agents_dir.iterdir())
    except OSError:
        # No ~/.fleet/agents/ dir at all (fresh install, test bootstrap
        # didn't create it). Nothing to sweep.
        return 0

    archived = 0
    for entry in entries:
        if not entry.is_file() or entry.suffix != ".json":
            continue
        try:
            with open(entry, "r", encoding="utf-8") as fp:
                rec = json.load(fp)
        except (OSError, json.JSONDecodeError):
            # Malformed / unreadable record — skip. The TUI tolerates
            # the same; nothing we should crash on.
            continue
        if not isinstance(rec, dict):
            continue
        # Scope to this project. Every project's coord runs its own
        # sweep; a sibling project's stale agent is that coord's
        # responsibility, not ours.
        rec_project = str(rec.get("project", "") or "")
        if rec_project != project:
            continue
        # Asking + blocked agents NEVER auto-archive — they need
        # operator attention.
        if bool(rec.get("needs_input", False)):
            continue
        if bool(rec.get("blocked", False)):
            continue
        last_ts = _parse_rfc3339(str(rec.get("last_activity_ts", "") or ""))
        if last_ts is None:
            # No parseable heartbeat — skip (defensive: brand-new agent
            # before its first Stop-hook fires, or a legacy record).
            continue
        if last_ts > cutoff_unix:
            # Fresh enough — keep.
            continue
        agent_id = str(rec.get("id", "") or "")
        if not _is_agent_id(agent_id):
            # Don't shell out with an invalid id (defensive: a hand-
            # edited record).
            continue
        # Best-effort `fleet rm <id>`. agent.go's archive flow tolerates
        # tmux session absence and pending-handoff queue absence; if
        # the agent has a pending handoff, rm refuses and the next
        # supervisor pass retries.
        try:
            proc = subprocess.run(
                [fleet_bin, "rm", agent_id],
                capture_output=True, text=True,
                timeout=30.0, check=False,
            )
        except (FileNotFoundError, subprocess.TimeoutExpired, OSError) as exc:
            emit(
                f"[coord] idle-archive: fleet rm {agent_id} failed: {exc}",
                stream=log_stream,
            )
            continue
        if proc.returncode != 0:
            # Non-zero exit (e.g., pending handoff blocks rm). Log and
            # move on — next pass retries naturally.
            err = (proc.stderr or proc.stdout or "").strip()
            emit(
                f"[coord] idle-archive: fleet rm {agent_id} "
                f"exit={proc.returncode}: {err}",
                stream=log_stream,
            )
            continue
        emit(
            f"[coord] idle-archive: archived {agent_id} "
            f"(idle > {ttl_h}h, project={project})",
            stream=log_stream,
        )
        archived += 1
    return archived


def _run_stuck_check_pass(
    *,
    probes: list[WorkerProbe],
    project: str,
    home: Path,
    fleet_bin: str,
    cfg: SupervisorConfig,
    now_unix: float,
    log_stream,
) -> _StuckPassResult:
    """One pass over every probe: load supervisor state, classify, act.

    Reads + writes coord-state.json directly (atomic via tmp+rename)
    rather than threading the dict through the supervisor's signature.
    Each pass is one read + one write — the cost is amortized over
    the stuck-check period (default 5 min).
    """
    out = _StuckPassResult()
    coord_state_path = home / "projects" / project / "coord-state.json"
    coord_state = _read_coord_state(coord_state_path)
    sup_states = load_supervisor_states(coord_state)
    agent_ids = load_agent_id_map(coord_state)

    # Lazy import to avoid the supervisor depending on loop at import
    # time (loop imports supervisor for the run_supervisor entry).
    from loop import _read_worker_state  # type: ignore[attr-defined]

    for p in probes:
        # codex iter-1 [P1]: in-review tasks have no live worker — the
        # subprocess exited after opening the PR. Skip stuck-detection
        # entirely. periodic_full_reconcile (called separately) handles
        # the gh pr CI re-check that moves the task to done.
        if not p.live_worker:
            continue
        st = _read_worker_state(project, p.slug, home=home)
        # Honor agent IDs from coord-state if probe didn't carry one
        # (probe is rebuilt from tasks.md every loop pass; agent_id
        # in coord-state is the durable source).
        agent_id = p.agent_id or agent_ids.get(p.slug, "")
        sup = sup_states.get(p.slug) or WorkerSupervisorState(slug=p.slug)
        sup_states[p.slug] = sup

        cur_phase = str(st.get("phase", "") or "") if st else ""
        # Phase change resets the FULL recovery ladder — the worker is
        # making progress (a transition counts as activity even if
        # heartbeat is stale). codex iter-2 [P1]: leaving nudged_at /
        # escalated_at populated meant a recovered worker that idled
        # later jumped straight to escalate/block on the next stall,
        # skipping the operator-friendly nudge.
        if cur_phase and cur_phase != sup.last_phase:
            sup.consecutive_stuck_polls = 0
            sup.last_phase = cur_phase
            sup.nudged_at = 0.0
            sup.escalated_at = 0.0

        # Pre-classify: would the worker BE stuck if we incremented the
        # counter? The classifier requires sup.consecutive_stuck_polls
        # ≥ cfg.stuck_polls, so we only increment when conditions 1-3
        # hold. Otherwise we leave the counter as-is.
        meets_pre_conditions = _meets_pre_conditions(
            st, cfg=cfg, now_unix=now_unix,
        )
        if meets_pre_conditions:
            sup.consecutive_stuck_polls += 1
        else:
            # Recently progressing or terminal — reset the FULL recovery
            # ladder. codex iter-2 [P1]: a worker that recovered (fresh
            # heartbeat) but kept its old nudged_at/escalated_at would
            # jump straight to escalate/block on its next stall instead
            # of starting at "nudge" again. Treat any pre-condition
            # failure (heartbeat caught up, terminal phase) as the
            # equivalent of a phase change for ladder-reset purposes.
            sup.consecutive_stuck_polls = 0
            sup.nudged_at = 0.0
            sup.escalated_at = 0.0
            continue

        session_alive = tmux_session_alive(p.tmux_session)
        if not is_stuck_idle(
            st, sup, cfg=cfg, session_alive=session_alive, now_unix=now_unix,
        ):
            continue

        # Recovery ladder. We use the timestamp markers to walk:
        #   no nudged_at      → nudge
        #   nudged_at set, no escalated_at, cooldown elapsed → escalate
        #   escalated_at set, cooldown elapsed               → block
        if sup.nudged_at <= 0.0:
            if agent_id and nudge_worker(agent_id, fleet_home=home):
                sup.nudged_at = now_unix
                out.nudges += 1
                emit(
                    f"[coord] worker {p.slug} stuck since {_fmt_ts(now_unix)}; nudging",
                    stream=log_stream,
                )
        elif sup.escalated_at <= 0.0:
            # Need at least one full cooldown after the nudge before
            # escalating, so the worker has a chance to respond.
            if (now_unix - sup.nudged_at) >= cfg.nudge_cooldown_s:
                if _escalate_to_operator(
                    project=project, slug=p.slug, fleet_bin=fleet_bin,
                    reason=(
                        f"Worker stuck-idle since {_fmt_ts(sup.nudged_at)}; "
                        "nudged but no response. Operator review needed."
                    ),
                ):
                    sup.escalated_at = now_unix
                    out.escalations += 1
                    emit(
                        f"[coord] worker {p.slug} still stuck post-nudge; "
                        "escalating to operator (task blocked)",
                        stream=log_stream,
                    )
        else:
            if (now_unix - sup.escalated_at) >= cfg.nudge_cooldown_s:
                if _mark_worker_blocked(
                    project=project, slug=p.slug, fleet_bin=fleet_bin,
                    reason="stuck-idle: persistent inactivity after escalation",
                ):
                    out.blocks += 1
                    emit(
                        f"[coord] worker {p.slug} persistently stuck; "
                        "marking blocked",
                        stream=log_stream,
                    )

    save_supervisor_states(coord_state, sup_states)
    _write_coord_state(coord_state_path, coord_state)
    return out


def _meets_pre_conditions(
    state: dict | None, *, cfg: SupervisorConfig, now_unix: float,
) -> bool:
    """Pre-conditions 1-2 of the stuck-idle test: non-terminal phase +
    stale heartbeat. Used to decide whether to increment
    consecutive_stuck_polls. Condition 3 (counter ≥ threshold) and
    condition 4 (tmux alive) gate the actual recovery action.
    """
    if state is None:
        return False
    phase = str(state.get("phase", "") or "")
    if phase in TERMINAL_WORKER_PHASES:
        return False
    if cfg.stuck_threshold_s <= 0:
        return False
    last_heartbeat = _parse_rfc3339(str(state.get("updated_at", "") or ""))
    if last_heartbeat is None:
        return False
    return (now_unix - last_heartbeat) > cfg.stuck_threshold_s


def _escalate_to_operator(
    *, project: str, slug: str, fleet_bin: str, reason: str,
) -> bool:
    """Mark the task as blocked and append a stuck-idle note.

    Returns True on success, False on any subprocess failure. Failure
    just logs — the next stuck-check pass will retry.

    Schema constraint: tasks.md does NOT have an `asking` status
    (internal/tasks.go enforces todo/ready/in-progress/in-review/done/
    blocked/abandoned). We use `blocked` because PR #76's attention
    drill-in surfaces blocked tasks as ⚠ red on the dashboard — exactly
    the operator-facing surface the design called "asking" for.
    """
    try:
        subprocess.run(
            [
                fleet_bin, "tasks", "set",
                "--project", project, slug, "status=blocked",
            ],
            capture_output=True, text=True, timeout=30.0, check=True,
        )
    except (subprocess.SubprocessError, OSError):
        return False
    try:
        subprocess.run(
            [
                fleet_bin, "tasks", "note",
                "--project", project, slug,
                f"STUCK_IDLE_ESCALATED: {reason}",
            ],
            capture_output=True, text=True, timeout=30.0, check=True,
        )
    except (subprocess.SubprocessError, OSError):
        # Status flip is durable; missing the note is graceful
        # degradation — still return True so the supervisor records
        # the escalation timestamp.
        pass
    return True


def _mark_worker_blocked(
    *, project: str, slug: str, fleet_bin: str, reason: str,
) -> bool:
    """`fleet workers update --phase blocked --reason ...`. Returns
    True on success, False on subprocess failure."""
    try:
        subprocess.run(
            [
                fleet_bin, "workers", "update",
                "--project", project, slug,
                "--phase", "blocked",
                "--reason", reason,
            ],
            capture_output=True, text=True, timeout=30.0, check=True,
        )
    except (subprocess.SubprocessError, OSError):
        return False
    return True


def _fmt_ts(unix: float) -> str:
    """Format a UNIX timestamp as RFC3339 UTC for log lines + tasks.md
    notes. Falls back to the raw float on parse error so we never
    break logging."""
    from datetime import datetime, timezone
    try:
        return datetime.fromtimestamp(unix, tz=timezone.utc).strftime(
            "%Y-%m-%dT%H:%M:%SZ"
        )
    except (OSError, ValueError, OverflowError):
        return str(unix)


def _read_coord_state(path: Path) -> dict:
    """Load coord-state.json. Returns {} on any error — caller handles
    the "first run" case by writing an empty state."""
    try:
        with open(path, "r", encoding="utf-8") as fh:
            data = json.load(fh)
        if isinstance(data, dict):
            return data
    except (FileNotFoundError, json.JSONDecodeError, OSError):
        pass
    return {}


def _write_coord_state(path: Path, state: dict) -> None:
    """Atomic publish (tmp + rename + fsync). Mirrors loop._save_coord_state.
    Kept local so supervisor doesn't depend on loop's private API."""
    parent = path.parent
    try:
        parent.mkdir(parents=True, exist_ok=True)
    except OSError:
        return
    try:
        fd, tmp = tempfile.mkstemp(prefix=path.name + ".tmp.", dir=str(parent))
    except OSError:
        return
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as fh:
            json.dump(state, fh, indent=2, sort_keys=True)
            fh.flush()
            os.fsync(fh.fileno())
        os.replace(tmp, path)
    except OSError:
        try:
            os.unlink(tmp)
        except FileNotFoundError:
            pass
        return
