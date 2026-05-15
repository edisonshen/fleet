"""Coordinator reaper — bounded kill window for completed/aborted workers.

DESIGN-subagent-lifecycle.md invariant 5: when the coord judges a
subagent's response as `complete` (or error-abort), the coord's reaper
kills it within a bounded window. The trigger is the coord's judgment;
never the worker's self-reported status alone.

Algorithm per probe:

  judgment = judge_completion(rec)
  ┌── complete    → send `/exit` to pane; wait grace; if alive, `fleet rm`
  │                 (kills session + archives record). If STILL alive
  │                 after rm: `kill -9` the pid, [P0] log line.
  ├── error-abort → same kill flow + record "reaper_redispatch_pending"
  │                 so the next dispatch tick picks up a replacement.
  ├── continue    → no-op (worker is mid-task)
  └── pending     → coord has not yet judged; do nothing.

Status flip ordering: the status flip from in-progress to
{in-review,todo,blocked} that happens in `_apply_reconcile` is the
*completion* of the reaper's lane handover — but the *kill* must happen
before the operator-visible terminal status is finalized. We therefore
expose a `reaper_complete(slug)` predicate the loop uses to gate
"is it safe to flip the task to terminal yet?" and a
`reaper_record_complete(slug)` helper the loop calls AFTER the apply
step. The kill ITSELF happens inside `reap_one()`; the loop only sees
True/False on whether the lane is clear.

Reaper state lives in `coord-state.json` under "reaper" as a sub-dict
keyed by slug. Each entry tracks:
  - kill_directive_ts   (float UTC seconds; 0 means not yet directed)
  - hard_killed         (bool; True if we had to SIGKILL — operator alert)
  - judgment            (last judgment string; informational)

The reaper is intentionally idempotent: calling reap_one twice on the
same slug within one grace window is a no-op (the second call sees
kill_directive_ts already set and respects the grace window). After
the kill succeeds the entry is dropped — re-dispatch on the same slug
starts with a fresh ledger.

This module is loop.py-agnostic except for two callables the caller
supplies (run_fleet_rm, send_exit_to_pane). Both are injected via
function arguments so tests can stub them; production wires them to
subprocess.run shells.
"""
from __future__ import annotations

import json
import os
import signal
import subprocess
import sys
import tempfile
import time
from dataclasses import dataclass, field
from pathlib import Path
from typing import Callable


# ---------- env knobs ----------


def _env_int(name: str, default: int) -> int:
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


def env_grace_window_s() -> int:
    """Grace seconds between `/exit` and forcible kill. Default 10."""
    return _env_int("FLEET_COORD_REAPER_GRACE_S", 10)


def env_reaper_disabled() -> bool:
    """When set to "1", the reaper is entirely inert (judge → no-op).

    Honored as an emergency-disable knob: an operator who hits a
    pathological reap loop can flip this on, restart the coord, and
    fall back to the v0.2 manual `fleet rm` workflow until the issue
    is debugged. The coord still runs the supervisor / dispatch /
    reconcile paths; only the reaper helpers short-circuit.
    """
    return os.environ.get("FLEET_COORD_REAPER_DISABLED", "") == "1"


# ---------- judgment ----------


# Judgment string constants. Plain strings so coord-state.json round-trips
# cleanly without needing a custom enum encoder.
JUDGE_COMPLETE = "complete"
JUDGE_ERROR_ABORT = "error-abort"
JUDGE_CONTINUE = "continue"
JUDGE_PENDING = "pending"
JUDGE_BLOCKED = "blocked"


# Phases the worker writes into state.json. Pinned here so the judgment
# table stays in sync with `internal/workers/workers.go`. The constants
# also live in supervisor.TERMINAL_WORKER_PHASES for the supervisor-side
# stuck-check — they're owned by Go, mirrored here at human-edit speed.
_TERMINAL_PHASES = frozenset({"done", "blocked", "failed"})


def judge_completion(
    *,
    task_status: str,
    worker_state: dict | None,
    is_git: bool,
) -> str:
    """Decide whether the coord considers this worker reapable.

    Inputs:
      task_status   — current tasks.md status (in-progress / in-review / ...)
      worker_state  — parsed workers/<slug>/state.json (or None if missing)
      is_git        — whether the project is git-mode (drives pr_url meaning)

    Returns one of the JUDGE_* constants. The mapping mirrors the
    reconcile decision tree in loop._reconcile_inflight so judgments
    don't drift from the existing transition logic:

      worker phase=done with pr_url    → complete    (worker shipped a PR)
      worker phase=done non-git        → complete    (non-git finisher succeeded)
      worker phase=failed              → error-abort (worker failed; replace)
      worker phase=blocked             → blocked     (operator decides; do not reap)
      worker phase=review-pending      → continue    (handoff to reviewer next)
      worker phase=review-done         → continue    (handoff to finisher next)
      any non-terminal phase           → continue    (worker still running)
      state.json missing               → pending     (can't judge yet)

    The judgment is INDEPENDENT of tmux session aliveness; tmux is the
    "is it safe to skip kill?" gate in `reap_one`. Splitting the two
    keeps the judgment testable (pure function of inputs) and the kill
    branch focused on side-effects.
    """
    if worker_state is None:
        return JUDGE_PENDING
    phase = str(worker_state.get("phase", "") or "")
    if not phase:
        return JUDGE_PENDING
    if phase == "failed":
        return JUDGE_ERROR_ABORT
    if phase == "blocked":
        # Operator-decision lifecycle per spec invariant 4: do not
        # auto-reap. The supervisor's stuck-alert path surfaces the
        # blocked state; reaping is gated on operator unblock.
        return JUDGE_BLOCKED
    if phase == "done":
        pr_url = str(worker_state.get("pr_url", "") or "")
        if is_git and not pr_url:
            # phase=done without a PR is the spec's "worker reported done
            # but didn't honor the contract" case. Treat as pending so
            # the existing reconcile path's "worker died without PR"
            # branch fires instead — that path requeues the task without
            # reaper involvement.
            return JUDGE_PENDING
        return JUDGE_COMPLETE
    if phase in ("review-pending", "review-done"):
        # Three-stage flow handoff phases — the next subagent picks up
        # via _dispatch_review_handoffs. NOT a reap target.
        return JUDGE_CONTINUE
    if phase in _TERMINAL_PHASES:
        # Defensive: any future terminal phase falls through to complete.
        # Adding a new terminal without wiring it explicitly here would
        # otherwise silently strand workers.
        return JUDGE_COMPLETE
    return JUDGE_CONTINUE


# ---------- reaper state in coord-state.json ----------


_REAPER_KEY = "reaper"
_REDISPATCH_PENDING_KEY = "reaper_redispatch_pending"


@dataclass
class ReaperEntry:
    """One slug's reaper bookkeeping. Persisted as a sub-dict in
    coord-state.json under reaper.<slug>."""

    slug: str
    kill_directive_ts: float = 0.0
    hard_killed: bool = False
    judgment: str = JUDGE_PENDING

    def to_dict(self) -> dict:
        return {
            "kill_directive_ts": self.kill_directive_ts,
            "hard_killed": self.hard_killed,
            "judgment": self.judgment,
        }

    @classmethod
    def from_dict(cls, slug: str, d: dict) -> "ReaperEntry":
        return cls(
            slug=slug,
            kill_directive_ts=float(d.get("kill_directive_ts", 0.0) or 0.0),
            hard_killed=bool(d.get("hard_killed", False)),
            judgment=str(d.get("judgment", JUDGE_PENDING) or JUDGE_PENDING),
        )


def load_reaper_entries(coord_state: dict) -> dict[str, ReaperEntry]:
    """Pull reaper sub-dict from coord-state. Empty on malformed shape."""
    raw = coord_state.get(_REAPER_KEY, {})
    if not isinstance(raw, dict):
        return {}
    out: dict[str, ReaperEntry] = {}
    for slug, val in raw.items():
        if not isinstance(slug, str) or not isinstance(val, dict):
            continue
        out[slug] = ReaperEntry.from_dict(slug, val)
    return out


def save_reaper_entries(
    coord_state: dict, entries: dict[str, ReaperEntry],
) -> None:
    """Write reaper sub-dict back into coord-state in place."""
    coord_state[_REAPER_KEY] = {
        slug: e.to_dict() for slug, e in entries.items()
    }


def forget_reaper_entry(coord_state: dict, slug: str) -> None:
    """Drop the entry for a slug — called after archive succeeds or when
    a task transitions back to todo (re-dispatch resets the ledger).
    Idempotent: a missing slug is a no-op."""
    raw = coord_state.get(_REAPER_KEY, {})
    if isinstance(raw, dict) and slug in raw:
        del raw[slug]
        coord_state[_REAPER_KEY] = raw


def load_redispatch_pending(coord_state: dict) -> set[str]:
    """Slugs flagged by the reaper for re-dispatch (error-abort path).
    The dispatch tick consumes + clears these so a one-shot replacement
    fires on the next tick."""
    raw = coord_state.get(_REDISPATCH_PENDING_KEY, [])
    if not isinstance(raw, list):
        return set()
    return {str(x) for x in raw if isinstance(x, str)}


def add_redispatch_pending(coord_state: dict, slug: str) -> None:
    raw = coord_state.get(_REDISPATCH_PENDING_KEY, [])
    if not isinstance(raw, list):
        raw = []
    if slug and slug not in raw:
        raw.append(slug)
    coord_state[_REDISPATCH_PENDING_KEY] = raw


def clear_redispatch_pending(coord_state: dict, slug: str) -> None:
    raw = coord_state.get(_REDISPATCH_PENDING_KEY, [])
    if not isinstance(raw, list):
        return
    coord_state[_REDISPATCH_PENDING_KEY] = [
        x for x in raw if isinstance(x, str) and x != slug
    ]


# ---------- kill primitives ----------


def _stderr_log(line: str, *, stream=None) -> None:
    """Emit a [P0] / informational line to stderr (or injected sink).

    The reaper's hard-kill branch is the only spec-mandated [P0] log
    line — invariant 5 says "Operator must see 'had to SIGKILL subagent
    X — investigate.'" Stderr lands in the coord's tmux pane and the
    operator's TUI tail, so it's the right surface for these alerts.
    """
    out = stream if stream is not None else sys.stderr
    if not line.endswith("\n"):
        line = line + "\n"
    try:
        out.write(line)
        out.flush()
    except OSError:
        pass


def send_exit_directive(
    tmux_session: str,
    *,
    timeout_s: float = 5.0,
) -> bool:
    """Send the worker the `/exit` directive via `tmux send-keys`.

    Returns True if the send-keys subprocess exited 0, else False. The
    /exit is a CLAUDE CLI built-in that triggers a clean Claude session
    exit; the worker's WIP write + state.json flush happens during this
    grace window (≤ 10 s default).

    Failure modes returning False (caller treats as "kill directive not
    delivered"; reap_one falls back to the harder kill path):
      - empty tmux_session
      - tmux not on PATH
      - subprocess timeout
      - non-zero exit (session doesn't exist, malformed key)
    """
    if not tmux_session:
        return False
    try:
        proc = subprocess.run(
            ["tmux", "send-keys", "-t", tmux_session, "/exit", "Enter"],
            capture_output=True, text=True,
            timeout=timeout_s, check=False,
        )
    except (FileNotFoundError, subprocess.TimeoutExpired, OSError):
        return False
    return proc.returncode == 0


def run_fleet_rm(
    fleet_bin: str,
    agent_id: str,
    *,
    timeout_s: float = 30.0,
) -> tuple[bool, str]:
    """Shell `fleet rm <id>` which kills the tmux session + archives
    the agent record (PR #146's SafeKillAndArchive equivalent).

    Returns (ok, stderr). ok=True on exit-0. A non-zero exit (e.g.,
    pending handoff blocks rm) returns False so reap_one falls through
    to the hard-kill branch.
    """
    if not agent_id:
        return False, "no agent_id"
    try:
        proc = subprocess.run(
            [fleet_bin, "rm", agent_id],
            capture_output=True, text=True,
            timeout=timeout_s, check=False,
        )
    except (FileNotFoundError, subprocess.TimeoutExpired, OSError) as exc:
        return False, str(exc)
    if proc.returncode == 0:
        return True, ""
    return False, (proc.stderr or proc.stdout or "").strip()


def hard_kill_pid(pid: int) -> bool:
    """SIGKILL the pid. Returns True if signal delivered (or pid was
    already gone). False on permission error / signal failure.

    Invariant-5 escape hatch: only called when the grace window has
    expired, `fleet rm` failed, and the tmux session is STILL alive on
    re-probe. Logs loudly at the caller — operator must see
    "had to SIGKILL".
    """
    if pid <= 0:
        return False
    try:
        os.kill(pid, signal.SIGKILL)
    except ProcessLookupError:
        # Already dead — treat as success.
        return True
    except (PermissionError, OSError):
        return False
    return True


# ---------- the reaper ----------


@dataclass
class ReapDecision:
    """Result of one reap_one() call.

    state ∈ {pending, directed, killed, hard-killed, no-op, error}.
      pending     — judgment was pending/continue; nothing happened.
      directed    — sent /exit (first time), still within grace window.
      killed      — within grace expired; fleet rm succeeded; entry dropped.
      hard-killed — fleet rm failed but session was dead OR pid SIGKILL'd.
      no-op       — slug not active (already reaped) OR reaper disabled.
      error       — judgment was complete/abort but kill could not proceed
                    (no agent_id, no tmux session, etc.). Surface to caller.
    """

    slug: str
    judgment: str = JUDGE_PENDING
    state: str = "pending"
    detail: str = ""
    hard_killed: bool = False


@dataclass
class ReapInputs:
    """Inputs the caller assembles per probe before calling reap_one.

    The caller knows tasks.md + state.json + tmux session names; the
    reaper consumes the read-only snapshot it built. Keeping this dumb
    means tests can construct it directly without monkeypatching the
    whole reading layer.
    """

    slug: str
    agent_id: str
    tmux_session: str
    worker_state: dict | None
    task_status: str
    pid: int = 0
    is_git: bool = True


def reap_one(
    inp: ReapInputs,
    *,
    entry: ReaperEntry,
    fleet_bin: str,
    now_unix: float,
    grace_window_s: int,
    session_alive_fn: Callable[[str], bool],
    send_exit_fn: Callable[[str], bool] | None = None,
    fleet_rm_fn: Callable[[str, str], tuple[bool, str]] | None = None,
    hard_kill_fn: Callable[[int], bool] | None = None,
    log_stream=None,
) -> ReapDecision:
    """Process one probe against the reaper invariant.

    See module docstring for the algorithm. The `entry` argument is
    mutated in place — caller writes the updated entries dict back to
    coord-state.json after the loop completes.

    Hooks (function arguments) make the kill primitives swappable so
    tests can drive the full algorithm deterministically:
      session_alive_fn  — supervisor.tmux_session_alive in production.
      send_exit_fn      — send_exit_directive in production.
      fleet_rm_fn       — run_fleet_rm in production.
      hard_kill_fn      — hard_kill_pid in production.

    All four are pure-effect; the algorithm logic lives in this function.
    Defaults are resolved at call time via module-level lookups so
    monkeypatch on `reaper.send_exit_directive` (etc.) is honored even
    when the caller passes the default sentinel.
    """
    # Resolve defaults at call-time so a `monkeypatch.setattr(reaper,
    # "run_fleet_rm", ...)` properly redirects from this function. Bound
    # default arguments would freeze the lookup at module-load and bypass
    # the patch — that's the test-isolation regression we trade against.
    if send_exit_fn is None:
        send_exit_fn = send_exit_directive
    if fleet_rm_fn is None:
        fleet_rm_fn = run_fleet_rm
    if hard_kill_fn is None:
        hard_kill_fn = hard_kill_pid
    decision = ReapDecision(slug=inp.slug)

    if env_reaper_disabled():
        decision.state = "no-op"
        decision.detail = "reaper disabled (FLEET_COORD_REAPER_DISABLED=1)"
        return decision

    judgment = judge_completion(
        task_status=inp.task_status,
        worker_state=inp.worker_state,
        is_git=inp.is_git,
    )
    decision.judgment = judgment
    entry.judgment = judgment

    if judgment in (JUDGE_PENDING, JUDGE_CONTINUE, JUDGE_BLOCKED):
        # Not reapable. Leave the entry's kill_directive_ts as-is — if
        # the worker bounced through `done` → `running` (recovery) the
        # next reap pass will re-evaluate cleanly.
        decision.state = "pending"
        return decision

    # judgment ∈ {complete, error-abort} → reap.

    if not inp.agent_id and not inp.tmux_session:
        # No tmux address (legacy/test-fixture/operator-driven worker).
        # The reaper has nothing to kill: treat as already-reaped so the
        # reconcile path can flip status without blocking. Probing
        # tmux for a `fleet-` session keyed on an empty id would just
        # answer "not alive" anyway, so the kill primitive is a no-op.
        decision.state = "killed"
        decision.detail = "no agent_id/tmux_session (nothing to kill)"
        return decision

    # 1. Send /exit if we haven't yet. The send is best-effort: a failed
    #    send-keys still records kill_directive_ts so we move into the
    #    grace timer and the harder kill paths kick in on the next pass.
    if entry.kill_directive_ts <= 0.0:
        send_exit_fn(inp.tmux_session)
        entry.kill_directive_ts = now_unix
        decision.state = "directed"
        return decision

    # 2. Grace window check.
    elapsed = now_unix - entry.kill_directive_ts
    if elapsed < grace_window_s:
        decision.state = "directed"
        decision.detail = f"within grace ({elapsed:.1f}s/{grace_window_s}s)"
        return decision

    # 3. Grace expired — probe tmux liveness.
    alive = session_alive_fn(inp.tmux_session) if inp.tmux_session else False

    if not alive:
        # Worker exited cleanly during grace. Best-effort archive via
        # `fleet rm`; failure is non-fatal (the operator's manual rm
        # path remains).
        ok, err = fleet_rm_fn(fleet_bin, inp.agent_id)
        decision.state = "killed"
        decision.detail = "" if ok else f"fleet rm noop (already gone? {err})"
        return decision

    # 4. Still alive after grace. Try `fleet rm` first — it includes
    #    a clean kill-session + archive.
    ok, err = fleet_rm_fn(fleet_bin, inp.agent_id)
    if ok:
        decision.state = "killed"
        return decision

    # 5. fleet rm failed. Re-probe; if the session is gone we're fine
    #    (rm raced with an orphan reaper).
    alive2 = session_alive_fn(inp.tmux_session) if inp.tmux_session else False
    if not alive2:
        decision.state = "killed"
        decision.detail = f"fleet rm err but session gone: {err}"
        return decision

    # 6. Hard-kill via SIGKILL. Last resort.
    if inp.pid > 0:
        hk_ok = hard_kill_fn(inp.pid)
        if hk_ok:
            entry.hard_killed = True
            decision.hard_killed = True
            decision.state = "hard-killed"
            decision.detail = (
                f"fleet rm failed ({err}); SIGKILL'd pid {inp.pid}"
            )
            _stderr_log(
                f"[P0] reaper: hard-killed worker {inp.slug} "
                f"(agent={inp.agent_id} pid={inp.pid}) — fleet rm failed: {err}",
                stream=log_stream,
            )
            return decision
    decision.state = "error"
    decision.detail = (
        f"fleet rm failed ({err}); no pid available for SIGKILL"
    )
    _stderr_log(
        f"[P0] reaper: kill failure for worker {inp.slug} "
        f"(agent={inp.agent_id}) — manual intervention required: {err}",
        stream=log_stream,
    )
    return decision


# ---------- helpers used by loop.py integration ----------


def reaper_lane_clear(
    coord_state: dict,
    slug: str,
) -> bool:
    """True iff the reaper has finished its kill cycle for `slug`.

    "Finished" means there is NO open reaper entry for slug. The lane is
    clear in three cases:
      - we never opened an entry (judgment was non-reapable)
      - we opened one and successfully reaped (entry deleted)
      - we explicitly forgot the entry (re-dispatch flow)

    A slug with an open entry whose kill_directive_ts is set but the
    kill hasn't completed yet returns False — callers (the reconcile
    apply step) defer terminal-status flips while False.
    """
    raw = coord_state.get(_REAPER_KEY, {})
    if not isinstance(raw, dict):
        return True
    return slug not in raw


def reap_probes(
    *,
    probes_inputs: list[ReapInputs],
    coord_state: dict,
    fleet_bin: str,
    now_unix: float,
    grace_window_s: int,
    session_alive_fn: Callable[[str], bool],
    send_exit_fn: Callable[[str], bool] | None = None,
    fleet_rm_fn: Callable[[str, str], tuple[bool, str]] | None = None,
    hard_kill_fn: Callable[[int], bool] | None = None,
    log_stream=None,
) -> list[ReapDecision]:
    """Run reap_one against every probe input. Mutates `coord_state` in
    place: opens / updates / drops reaper entries based on the decisions.

    Returns the list of decisions, one per input. Caller is responsible
    for persisting coord_state (atomic write via loop._save_coord_state).
    """
    entries = load_reaper_entries(coord_state)
    decisions: list[ReapDecision] = []
    for inp in probes_inputs:
        entry = entries.get(inp.slug) or ReaperEntry(slug=inp.slug)
        decision = reap_one(
            inp, entry=entry, fleet_bin=fleet_bin, now_unix=now_unix,
            grace_window_s=grace_window_s,
            session_alive_fn=session_alive_fn,
            send_exit_fn=send_exit_fn,
            fleet_rm_fn=fleet_rm_fn,
            hard_kill_fn=hard_kill_fn,
            log_stream=log_stream,
        )
        decisions.append(decision)
        if decision.state in ("directed",):
            # Keep the entry around: grace window is still running.
            entries[inp.slug] = entry
        elif decision.state in ("killed", "hard-killed"):
            # Successful reap. Drop the entry; if this was an error-abort
            # judgment, mark the slug for re-dispatch on the next tick.
            if entry.slug in entries:
                del entries[entry.slug]
            else:
                # not present — but ensure dict reflects deletion
                pass
            if decision.judgment == JUDGE_ERROR_ABORT:
                add_redispatch_pending(coord_state, inp.slug)
        elif decision.state in ("pending", "no-op"):
            # Nothing to persist for non-reapable judgments. If we had
            # an entry from a prior run (e.g., worker recovered from
            # phase=done back to running), drop it so the ledger
            # doesn't grow.
            if inp.slug in entries and decision.judgment in (
                JUDGE_PENDING, JUDGE_CONTINUE, JUDGE_BLOCKED,
            ):
                del entries[inp.slug]
        elif decision.state == "error":
            # Kill could not proceed — keep the entry so a later pass
            # retries once the operator has fixed the gap (agent_id
            # appears, tmux session re-spawned, etc.).
            entries[inp.slug] = entry
    save_reaper_entries(coord_state, entries)
    return decisions
