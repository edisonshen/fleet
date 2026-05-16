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


def env_poll_base_interval_s() -> int:
    """Default poll cadence per subagent (invariant 4, "5 seconds per
    subagent" target). Used by the adaptive driver when the loop is
    spending time on a single probe; the legacy single-rate driver
    keeps `env_poll_interval_s()` for back-compat.

    Codex iter-5 [P2]: returns 0 when FLEET_COORD_POLL_BASE_INTERVAL_S
    is UNSET, so the legacy poll_interval_s wins for deployments that
    only tuned the old knob. An operator who explicitly sets the new
    knob (even to its default 5) gets the adaptive driver; an operator
    who only sets FLEET_COORD_POLL_INTERVAL_S=60 keeps polling every
    60 s as before. Setting to 0 explicitly also disables adaptive
    cadence — same shape as the other supervisor knobs.
    """
    raw = os.environ.get("FLEET_COORD_POLL_BASE_INTERVAL_S", "")
    if raw == "":
        return 0
    try:
        n = int(raw)
    except ValueError:
        return 0
    if n < 0:
        return 0
    return n


def env_poll_backoff_interval_s() -> int:
    """Dilated poll cadence per subagent after stability window (spec:
    30 s). Tests pin smaller values."""
    return _env_int("FLEET_COORD_POLL_BACKOFF_INTERVAL_S", 30)


def env_poll_stability_window_s() -> int:
    """Time a subagent's state must be stable before the per-probe
    cadence dilates from base to backoff. Spec default 5 min = 300 s."""
    return _env_int("FLEET_COORD_POLL_STABILITY_WINDOW_S", 300)


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
    # Invariant-4 adaptive cadence (DESIGN-subagent-lifecycle.md §4).
    #   poll_base_interval_s     — per-probe poll cadence default (5 s).
    #   poll_backoff_interval_s  — dilated cadence after stability (30 s).
    #   poll_stability_window_s  — seconds of no state-change before dilation.
    # poll_base_interval_s=0 falls back to the legacy single-rate driver
    # (poll_interval_s above) so legacy tests + operators stay byte-
    # identical to v0.2.x supervisor behavior.
    poll_base_interval_s: int = 5
    poll_backoff_interval_s: int = 30
    poll_stability_window_s: int = 300

    @classmethod
    def from_env(cls) -> "SupervisorConfig":
        return cls(
            poll_interval_s=env_poll_interval_s(),
            stuck_check_every=env_stuck_check_every(),
            stuck_threshold_s=env_stuck_threshold_s(),
            stuck_polls=env_stuck_polls(),
            nudge_cooldown_s=env_nudge_cooldown_s(),
            poll_max_s=env_poll_max_s(),
            poll_base_interval_s=env_poll_base_interval_s(),
            poll_backoff_interval_s=env_poll_backoff_interval_s(),
            poll_stability_window_s=env_poll_stability_window_s(),
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
# Codex iter-4 [P1] (dispatch-lifecycle PR1 follow-up): pending acquire
# IDs are dispatch IDs minted on a tick where `fleet claims acquire-
# prompt` FAILED. The next tick must retry with the SAME id so the
# AcquireCoordPromptInbox idempotent / recovery path can heal a half-
# written journal/inbox. Without this, every acquire-prompt failure
# orphans a journal that nothing in PR1 reclaims.
_PENDING_ACQUIRE_IDS_KEY = "pending_acquire_agent_ids"
# Codex iter-9 [P1]: pending release IDs are agent_ids whose
# `fleet claims release` returned an `error` outcome (transient
# fault). They live keyed by slug but value is a LIST because a single
# slug can accumulate multiple unreleased ids over its lifecycle —
# e.g., a handoff that overwrites worker_agent_ids with the new
# subagent's id while the prior subagent's release retry is still
# pending. The next sweep / reconcile retries every entry; success
# (or absent/not_owned/already_released) removes the id from the
# list; a slug with an empty list is dropped from the map.
_PENDING_RELEASE_IDS_KEY = "pending_release_agent_ids"

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
    # Codex iter-4 [P1]: also clear any pending-acquire entry. A slug
    # transitioning to terminal (todo / done / blocked) by any path
    # means the previous attempt is conclusively over — any orphaned
    # pending agent_id should not bleed into the NEXT dispatch attempt.
    forget_pending_acquire_agent_id(coord_state, slug)


def load_pending_acquire_agent_id_map(coord_state: dict) -> dict[str, str]:
    """Return the slug → pending-acquire agent_id map.

    A non-empty entry for slug S means a prior tick minted that
    agent_id and called `fleet claims acquire-prompt` for it but the
    CLI returned an error outcome. The journal/inbox may be half-
    written. The next dispatch attempt for S must reuse the SAME id
    so AcquireCoordPromptInbox's recovery branch (live-claim-with-
    missing-file) can complete the half-written acquire instead of
    orphaning a journal.
    """
    raw = coord_state.get(_PENDING_ACQUIRE_IDS_KEY, {})
    if not isinstance(raw, dict):
        return {}
    out: dict[str, str] = {}
    for k, v in raw.items():
        if isinstance(k, str) and isinstance(v, str) and _is_agent_id(v):
            out[k] = v
    return out


def remember_pending_acquire_agent_id(
    coord_state: dict, slug: str, agent_id: str,
) -> None:
    """Persist a slug → pending-acquire agent_id so the next tick
    retries with the same id and can hit the recovery path."""
    if not slug or not _is_agent_id(agent_id):
        return
    raw = coord_state.get(_PENDING_ACQUIRE_IDS_KEY, {})
    if not isinstance(raw, dict):
        raw = {}
    raw[slug] = agent_id
    coord_state[_PENDING_ACQUIRE_IDS_KEY] = raw


def forget_pending_acquire_agent_id(coord_state: dict, slug: str) -> None:
    """Drop the pending-acquire mapping on success or terminal."""
    raw = coord_state.get(_PENDING_ACQUIRE_IDS_KEY, {})
    if isinstance(raw, dict) and slug in raw:
        del raw[slug]
        coord_state[_PENDING_ACQUIRE_IDS_KEY] = raw


def load_pending_release_agent_ids(
    coord_state: dict,
) -> dict[str, list[str]]:
    """Return slug → list[agent_id] of releases that need retry.

    Codex iter-9 [P1]: when `fleet claims release` returns `error`
    during handoff, the new dispatch overwrites worker_agent_ids
    with the new subagent's id. The prior id is captured here so
    sweep / reconcile can retry the release without losing the
    only handle.
    """
    raw = coord_state.get(_PENDING_RELEASE_IDS_KEY, {})
    if not isinstance(raw, dict):
        return {}
    out: dict[str, list[str]] = {}
    for k, v in raw.items():
        if not isinstance(k, str) or not isinstance(v, list):
            continue
        ids = [x for x in v if isinstance(x, str) and _is_agent_id(x)]
        if ids:
            out[k] = ids
    return out


def remember_pending_release_agent_id(
    coord_state: dict, slug: str, agent_id: str,
) -> None:
    """Append an agent_id whose release failed transiently to the
    per-slug retry list. Deduped per slug."""
    if not slug or not _is_agent_id(agent_id):
        return
    raw = coord_state.get(_PENDING_RELEASE_IDS_KEY, {})
    if not isinstance(raw, dict):
        raw = {}
    existing = raw.get(slug, [])
    if not isinstance(existing, list):
        existing = []
    if agent_id not in existing:
        existing.append(agent_id)
    raw[slug] = existing
    coord_state[_PENDING_RELEASE_IDS_KEY] = raw


def forget_pending_release_agent_id(
    coord_state: dict, slug: str, agent_id: str,
) -> None:
    """Drop one agent_id from a slug's retry list. If the list goes
    empty, drop the slug key entirely."""
    raw = coord_state.get(_PENDING_RELEASE_IDS_KEY, {})
    if not isinstance(raw, dict):
        return
    existing = raw.get(slug, [])
    if not isinstance(existing, list):
        return
    if agent_id in existing:
        existing.remove(agent_id)
    if existing:
        raw[slug] = existing
    else:
        raw.pop(slug, None)
    coord_state[_PENDING_RELEASE_IDS_KEY] = raw


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


# ---------- adaptive cadence ----------


def compute_next_sleep_s(
    *,
    cfg: SupervisorConfig,
    probes: list[WorkerProbe],
    last_change_ts: dict[str, float],
    now_unix: float,
) -> float:
    """Return the sleep interval for the next supervisor iteration.

    Each probe has a per-slug "stable since" timestamp tracked in
    `last_change_ts`. A probe stable for > poll_stability_window_s
    dilates to poll_backoff_interval_s; otherwise it polls at
    poll_base_interval_s.

    The loop sleeps for `min(per_probe_cadence)`, so a single newly-
    active subagent reverts the loop to base cadence (the responsiveness
    requirement: the spec says "default 5 s per subagent" — the loop
    must visit each probe within that window).

    poll_base_interval_s <= 0 disables adaptive cadence; we fall back to
    cfg.poll_interval_s (legacy single-rate). This keeps existing tests
    that pin poll_base_interval_s=0 byte-identical to the v0.2.x driver.
    """
    if cfg.poll_base_interval_s <= 0:
        return float(cfg.poll_interval_s)
    if not probes:
        # Nothing to poll — fall back to base cadence so the outer loop
        # can re-evaluate has_active_workers in bounded time.
        return float(cfg.poll_base_interval_s)
    base = float(cfg.poll_base_interval_s)
    backoff = float(max(cfg.poll_backoff_interval_s, cfg.poll_base_interval_s))
    window = float(cfg.poll_stability_window_s)
    min_interval = backoff
    for p in probes:
        last_change = last_change_ts.get(p.slug, now_unix)
        age = now_unix - last_change
        if age <= window:
            return base  # at least one probe is hot — full speed
        min_interval = min(min_interval, backoff)
    return min_interval


# ---------- force-tick (inbox / queue event) ----------


def has_pending_inbox_events(
    coord_id: str,
    *,
    fleet_home: Path,
    last_seen_archive: str,
    direct_inbox_mtime_baseline: float = 0.0,
) -> bool:
    """Return True iff there's at least one inbox or queue event the
    coord hasn't processed yet.

    Two surfaces, both cheap fs scans (no parse, no shell):
      1. ~/.fleet/inbox/<coord_id>.md — operator → coord message AND
         the dropbox where `emit_stuck_alert` appends [STUCK] lines.
         Codex iter-1 [P1]: this file is NOT consumed by the coord
         skill — fleet-guard's SessionStart hook eats it on the next
         coord-agent turn. During a long supervisor session the file
         persists, so checking existence alone would wedge
         `force_tick_check` into "always True", spinning the supervisor
         at 0-second sleeps. We compare the current mtime against the
         supervisor's start-of-session baseline (`direct_inbox_mtime
         _baseline`); only an mtime advance counts as an event.
         baseline=0.0 (file absent at supervisor start) means "any
         current mtime triggers an event".
      2. ~/.fleet/inbox/archive/<coord_id>-*.md — worker → coord archive
         (the supervisor's force-tick wakes the loop when a worker's
         response lands during a long backoff sleep). We compare archive
         filenames against `last_seen_archive` (the watermark loop.py
         persists). Any name > watermark counts.

    The check is intentionally narrow: a true event triggers a forced
    immediate poll on the next iteration; false positives just shorten
    one sleep, so we err toward "skip force-tick" on any I/O hiccup.
    """
    if not coord_id:
        return False
    # Direct inbox file — check mtime advanced past the supervisor's
    # baseline. existence-alone would over-trigger because fleet-guard
    # (not us) clears the file; the file can persist for the lifetime
    # of the supervisor session if no coord-agent turn fires.
    direct = fleet_home / "inbox" / f"{coord_id}.md"
    try:
        st = direct.stat()
        if st.st_mtime > direct_inbox_mtime_baseline:
            return True
    except OSError:
        # File missing or unreadable — no event from the direct inbox.
        pass
    # Archive sweep for worker responses.
    archive = fleet_home / "inbox" / "archive"
    try:
        entries = archive.iterdir() if archive.is_dir() else iter(())
    except OSError:
        return False
    prefix = coord_id + "-"
    for entry in entries:
        try:
            name = entry.name
        except OSError:
            continue
        if not name.startswith(prefix):
            continue
        if last_seen_archive and name <= last_seen_archive:
            continue
        return True
    return False


def emit_stuck_alert(
    coord_id: str,
    slug: str,
    *,
    fleet_home: Path,
    detail: str,
    post_write_mtime: list[float] | None = None,
) -> str:
    """Drop a [STUCK] inbox alert at ~/.fleet/inbox/<coord_id>.md.

    Invariant 4 explicitly says "surface to operator (TUI badge, optional
    inbox alert), NOT auto-killed". This is the inbox-alert surface.
    The supervisor's existing recovery ladder (nudge → escalate → block)
    is a SEPARATE behavior that legacy operators rely on; toggling it
    off would regress recovery. The two surfaces coexist:
      - stuck-alert (this fn): inbox drop so TUI/operator sees the flag.
      - recovery ladder: actions that touch worker inboxes / tasks.md.

    Returns the path written on success, "" on any error.

    Codex iter-18 [P1]: caller can pass `post_write_mtime` (a list)
    into which we append the file's mtime IMMEDIATELY AFTER our
    write. The supervisor uses the highest such mtime as the baseline
    for the operator-message-exit check, so an operator write that
    ALSO landed during the same stuck-check pass (with a different
    mtime) still triggers the exit.

    The write is append-only via O_APPEND so we don't clobber a coord
    inbox that's already populated with an operator message — fleet-
    guard's coord-side hook reads the file end-to-end.
    """
    if not coord_id or not slug:
        return ""
    inbox_dir = fleet_home / "inbox"
    try:
        inbox_dir.mkdir(parents=True, exist_ok=True)
    except OSError:
        return ""
    target = inbox_dir / f"{coord_id}.md"
    line = f"[STUCK] worker {slug}: {detail}\n"
    try:
        with open(target, "a", encoding="utf-8") as fh:
            fh.write(line)
            fh.flush()
            os.fsync(fh.fileno())
    except OSError:
        return ""
    if post_write_mtime is not None:
        try:
            post_write_mtime.append(target.stat().st_mtime)
        except OSError:
            pass
    return str(target)


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
    force_tick_check: Callable[[], bool] | None = None,
    force_tick_dispatch: Callable[[], None] | None = None,
    reaper_hook: Callable[[list[WorkerProbe]], list[str] | None] | None = None,
    coord_id: str = "",
    direct_inbox_session_baseline: float | None = None,
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
    # Codex iter-11 [P1] + iter-21 [P3]: snapshot the direct-inbox
    # mtime at session start so the "operator wrote to inbox → exit"
    # gate fires only when the file actually changes during
    # supervision. The caller (loop._run_supervisor) may pass an
    # already-captured baseline — that's the SAME snapshot the
    # force_tick_check_hook uses, eliminating the two-read race
    # codex iter-21 [P3] flagged.
    if direct_inbox_session_baseline is None:
        direct_inbox_session_baseline = 0.0
        if coord_id:
            try:
                direct_inbox_session_baseline = (
                    (home / "inbox" / f"{coord_id}.md").stat().st_mtime
                )
            except OSError:
                direct_inbox_session_baseline = 0.0
    last_mtimes: dict[str, float] = {}
    # Per-slug "last observed state change" timestamp. Drives the
    # invariant-4 adaptive cadence: probes stable for >
    # poll_stability_window_s dilate from base to backoff cadence.
    # Seeded to `started_at` so the loop polls at base cadence until
    # each probe has accumulated stability_window worth of silence.
    last_change_ts: dict[str, float] = {}
    poll_count = 0
    # Codex iter-6 [P2]: track wall-clock cadences for stuck-check and
    # periodic-reconcile so the adaptive poll driver doesn't make them
    # 6× more aggressive. Legacy (poll_base_interval_s=0) keeps the old
    # poll-count behavior because cadence wall-clock = poll_count *
    # poll_interval_s is exact in that case.
    last_stuck_check_unix = now_fn()
    last_periodic_reconcile_unix = now_fn()

    probes = refresh_probes()
    for p in probes:
        last_mtimes[p.slug] = safe_mtime(p.state_path)
        last_change_ts[p.slug] = started_at

    if not has_active_workers(probes):
        res.exit_reason = "no-active-workers"
        emit("[coord] supervisor loop exiting: no active workers", stream=log_stream)
        return res

    emit(
        f"[coord] supervisor loop starting: {len(probes)} active workers, "
        f"poll={cfg.poll_interval_s}s base={cfg.poll_base_interval_s}s "
        f"backoff={cfg.poll_backoff_interval_s}s "
        f"stuck_every={cfg.stuck_check_every} "
        f"threshold={cfg.stuck_threshold_s}s",
        stream=log_stream,
    )

    while True:
        if now_fn() - started_at >= cfg.poll_max_s:
            res.exit_reason = "max-duration"
            emit("[coord] supervisor loop exiting: max duration reached", stream=log_stream)
            break

        # Invariant 4: compute the next sleep adaptively. Probes still
        # in their stability window force base cadence; otherwise back
        # off. force_tick_check is the inbox/queue event surface — if
        # an event is queued we sleep 0 (effectively "skip the wait")
        # and call force_tick_dispatch so a NEW_TASK / drain event
        # actually drains + dispatches under cap (codex iter-3 [P2]).
        forced = False
        if force_tick_check is not None:
            try:
                forced = bool(force_tick_check())
            except Exception as exc:  # noqa: BLE001
                res.errors.append(f"force_tick_check: {exc}")
                forced = False
        if forced:
            # Codex iter-19 [P2]: enforce a small floor on consecutive
            # forced wakes so a chronic force-tick condition (e.g.,
            # tasks.md parse error preventing watermark advance)
            # doesn't peg the supervisor at 0-second polls. The
            # 100-ms floor is large enough to keep CPU usage sane
            # but small enough that genuine fast-wake handling still
            # responds quickly.
            sleep_s = 0.1
        else:
            sleep_s = compute_next_sleep_s(
                cfg=cfg,
                probes=probes,
                last_change_ts=last_change_ts,
                now_unix=now_fn(),
            )
        if sleep_s > 0:
            sleep_fn(sleep_s)
        poll_count += 1
        res.iterations += 1

        # Codex iter-10 [P2]: when the operator wrote to the direct
        # coord inbox (~/.fleet/inbox/<coord>.md), exit the supervisor
        # so the next Claude-agent turn fires fleet-guard's
        # SessionStart hook and surfaces the message. The coord skill
        # itself doesn't consume that inbox surface (fleet-guard
        # does), so staying in the supervisor would silence operator
        # messages indefinitely.
        #
        # Codex iter-11 [P1]: gate on mtime ADVANCE relative to the
        # supervisor's start-of-session snapshot, not just file
        # existence. A pre-existing inbox file (stale operator message,
        # or a [STUCK] line dropped by emit_stuck_alert itself) would
        # otherwise terminate the supervisor on every forced wake —
        # including archive-driven NEW_TASK / TASK_DONE_PR wakes that
        # have nothing to do with operator messages.
        if forced and coord_id:
            direct = home / "inbox" / f"{coord_id}.md"
            try:
                cur_mtime = direct.stat().st_mtime
                if cur_mtime > direct_inbox_session_baseline:
                    res.exit_reason = "operator-inbox-message"
                    emit(
                        f"[coord] supervisor loop exiting: operator wrote "
                        f"to inbox {direct.name} (fleet-guard will deliver "
                        "on next turn)",
                        stream=log_stream,
                    )
                    break
            except OSError:
                pass

        # On forced wake, run the dispatch hook so a NEW_TASK arriving
        # mid-supervisor session is picked up under spare cap. Pre-sleep
        # tick order: the iteration body below still runs (reconcile +
        # reaper + stuck-check), so this is purely an additional
        # dispatch surface for the forced path.
        if forced and force_tick_dispatch is not None:
            try:
                force_tick_dispatch()
            except Exception as exc:  # noqa: BLE001
                res.errors.append(f"force_tick_dispatch: {exc}")

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
        for slug in list(last_change_ts.keys()):
            if slug not in cur_slugs:
                del last_change_ts[slug]
        for p in probes:
            last_mtimes.setdefault(p.slug, safe_mtime(p.state_path))
            last_change_ts.setdefault(p.slug, now_fn())

        # Mtime change detection — cheap stat, no API calls. Computed
        # FIRST so the change list is fixed before either the reaper or
        # the reconcile pass mutates state.json (the reaper writes
        # nothing to state.json; the reconcile path may delete the
        # worker dir, which would flip mtime AFTER reconcile fired —
        # we don't want that to count as a "changed" tick).
        changed: list[WorkerProbe] = []
        for p in probes:
            cur = safe_mtime(p.state_path)
            prev = last_mtimes.get(p.slug, 0.0)
            if cur > prev:
                changed.append(p)
                last_mtimes[p.slug] = cur
                last_change_ts[p.slug] = now_fn()

        # Invariant 5 (reaper) — runs BEFORE reconcile_one (codex iter-1
        # [P1]). A worker whose state.json mtime just advanced to
        # phase=done MUST get its /exit + kill-cycle started before
        # reconcile flips the task status and forgets the agent_id
        # mapping. Otherwise reconcile clears worker_agent_ids and the
        # subsequent reaper pass has no tmux session to address — the
        # tmux session leaks as an orphan (the exact failure shape
        # invariant 5 exists to prevent). Hook owns its own state
        # persistence; failures are non-fatal.
        #
        # Codex iter-3 [P2]: reaper_hook may return a list of slugs
        # whose kill cycle COMPLETED this iteration. Those slugs need
        # an immediate reconcile pass — their state.json mtime probably
        # hasn't moved (the worker exited cleanly and state.json was
        # stable for several polls), so without an explicit re-add to
        # `changed` the status flip would wait for the periodic full
        # reconcile (5+ minutes default). For cap=1 projects this
        # blocks the next dispatch.
        reaped_now: list[str] = []
        if reaper_hook is not None:
            try:
                ret = reaper_hook(probes)
                if isinstance(ret, list):
                    reaped_now = [str(s) for s in ret if isinstance(s, str)]
            except Exception as exc:  # noqa: BLE001
                res.errors.append(f"reaper_hook: {exc}")
        # Add the just-reaped slugs to `changed` so reconcile re-runs
        # against them this iteration (their lane is now clear, the
        # deferred status flip can proceed).
        if reaped_now:
            slug_to_probe = {p.slug: p for p in probes}
            for slug in reaped_now:
                p = slug_to_probe.get(slug)
                if p is None:
                    continue
                if p not in changed:
                    changed.append(p)

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
        # Codex iter-6 [P2] + iter-8 [P2]: cadence by WALL-CLOCK time
        # for BOTH drivers. Adaptive mode varies per-iteration sleep
        # (5/30 s); legacy mode is now perturbed by forced-wake zero-
        # sleep iterations (the codex iter-8 [P2] finding). Using
        # elapsed seconds against the operator-facing target
        # (poll_interval_s × stuck_check_every) gives "every 5 min by
        # default" semantics in every mode and shields the recovery
        # ladder from inbox/archive event bursts.
        stuck_target_s = float(cfg.poll_interval_s * cfg.stuck_check_every)
        periodic_target_s = float(cfg.poll_interval_s * reconcile_cadence)
        now_clock = now_fn()
        run_periodic = (
            periodic_full_reconcile is not None
            and reconcile_cadence > 0
            and periodic_target_s > 0
            and (now_clock - last_periodic_reconcile_unix) >= periodic_target_s
        )
        run_stuck = (
            cfg.stuck_check_every > 0
            and stuck_target_s > 0
            and (now_clock - last_stuck_check_unix) >= stuck_target_s
        )
        if run_periodic or run_stuck:
            # Periodic full reconcile FIRST. Running before stuck-check
            # means a worker that just transitioned to in-review (and
            # would otherwise be stuck-flagged on the next pass) drops
            # out of the live-worker probe set BEFORE we evaluate it.
            if run_periodic:
                last_periodic_reconcile_unix = now_clock
                try:
                    periodic_full_reconcile()
                except Exception as exc:  # noqa: BLE001
                    res.errors.append(f"periodic-reconcile: {exc}")
            if run_stuck:
                last_stuck_check_unix = now_clock
                res.stuck_check_passes += 1
                # Codex iter-12 [P1] + iter-18 [P1]: capture the
                # per-write mtimes of every [STUCK] alert this pass
                # emits, so we can swallow ONLY our own writes when
                # bumping direct_inbox_session_baseline. An operator
                # write that races our alert in the same pass would
                # have a DIFFERENT mtime — using that as baseline
                # would silence the operator message.
                stuck_alert_mtimes: list[float] = []
                try:
                    stuck_summary = _run_stuck_check_pass(
                        probes=probes,
                        project=project,
                        home=home,
                        fleet_bin=fleet_bin,
                        cfg=cfg,
                        now_unix=now_fn(),
                        coord_id=coord_id,
                        log_stream=log_stream,
                        stuck_alert_mtimes=stuck_alert_mtimes,
                    )
                    res.nudges_sent += stuck_summary.nudges
                    res.escalations += stuck_summary.escalations
                    res.blocks += stuck_summary.blocks
                except Exception as exc:  # noqa: BLE001
                    res.errors.append(f"stuck-check: {exc}")
                    stuck_summary = _StuckPassResult()
                # Codex iter-12 [P1]: if the stuck-check pass wrote
                # to the coord inbox (a [STUCK] alert), advance the
                # session baseline so the next forced wake doesn't
                # treat the alert as an operator message.
                #
                # Codex iter-18 [P1]: advance the baseline ONLY to
                # the highest mtime we recorded immediately after our
                # OWN writes. An operator write that races our pass
                # has a different (typically later) mtime — it stays
                # > baseline and triggers the operator-inbox-exit on
                # the next forced wake. Without per-write capture,
                # taking the file's final mtime would baseline-over
                # any concurrent operator write and silence the
                # operator message for the rest of the session.
                if coord_id and stuck_alert_mtimes:
                    new_baseline = max(stuck_alert_mtimes)
                    if new_baseline > direct_inbox_session_baseline:
                        direct_inbox_session_baseline = new_baseline
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
    # Codex iter-13 [P2]: count of [STUCK] inbox alerts emitted to
    # the coord inbox this pass. Used by run_supervisor to know
    # whether to swallow the post-pass mtime bump on the session
    # baseline (only swallow when we actually wrote — otherwise
    # an operator's concurrent write would be lost).
    stuck_alerts: int = 0


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
        # Multi-engine MVP (memory project_codex_multi_engine.md):
        # non-claude-code engines (currently just "codex") don't run
        # Claude Code hooks, so fleet-guard never updates their
        # last_activity_ts. A codex agent that the operator dispatched
        # an hour ago and is actively running would look "idle since
        # spawn" to this sweep — and idle-TTL auto-archive would `fleet
        # rm` it out from under the operator. Skip non-claude-code
        # engines until a sibling skill writes heartbeats for them.
        # Empty engine field defaults to claude-code (legacy records).
        engine = str(rec.get("engine", "") or "claude-code")
        if engine != "claude-code":
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
    coord_id: str = "",
    stuck_alert_mtimes: list[float] | None = None,
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
            # Codex iter-5 [P2]: emit the stuck alert FIRST, BEFORE the
            # nudge attempt. The invariant-4 alert surface must fire
            # even when the worker has no agent_id mapping (lost during
            # a coord restart, never registered) or when the nudge
            # inbox write fails transiently — those are exactly the
            # cases where the operator most needs the alert.
            # Codex iter-6 [P3]: use the threaded coord_id; fall back
            # to the env var only when the caller didn't supply one
            # (legacy CLI invocation path).
            alert_coord_id = coord_id or os.environ.get(
                "FLEET_AGENT_ID", "",
            ) or ""
            if alert_coord_id:
                wrote = emit_stuck_alert(
                    alert_coord_id, p.slug, fleet_home=home,
                    detail=(
                        f"phase={cur_phase or '?'} idle since "
                        f"{_fmt_ts(now_unix)}"
                    ),
                    post_write_mtime=stuck_alert_mtimes,
                )
                if wrote:
                    out.stuck_alerts += 1
            if agent_id and nudge_worker(agent_id, fleet_home=home):
                sup.nudged_at = now_unix
                out.nudges += 1
                emit(
                    f"[coord] worker {p.slug} stuck since {_fmt_ts(now_unix)}; nudging",
                    stream=log_stream,
                )
            # Codex iter-7 [P2]: if the nudge was NOT actually delivered
            # (no agent_id, or nudge_worker returned ""), do NOT advance
            # nudged_at — otherwise the next pass cooldown would mature
            # and escalate the worker to operator-blocked without any
            # recovery message having reached it. Leave nudged_at=0 so
            # the loop retries on the next stuck-check pass; the
            # stuck-alert above keeps surfacing the situation. The
            # cooldown-of-cooldowns concern (alert spam) is bounded by
            # FLEET_COORD_STUCK_CHECK_EVERY (default 5 min cadence).
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
