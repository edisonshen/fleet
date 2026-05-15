"""Coordinator tick driver — one Stop-hook invocation, one tick.

This module is the `tick()` entry point. The skill is invoked by Claude
Code on a timer (or by the operator via `fleet message <coord_id>`);
each invocation runs `tick()` and exits. Restart = resume from disk
state. There is no daemon, no asyncio, no thread pool.

Algorithm (ENG §6 / §5.1):

  1. NB-flock coordinator.lock; on EWOULDBLOCK exit cleanly.
  2. Parse tasks.md (read-only via parse.py) + load standards/learnings
     via the fleet CLI (cached).
  3. Reconcile in-flight workers: for each task in {in-progress,
     in-review}, check the worker_pid is alive; if not, examine pr_url
     + CI to decide the next status.
  4. Drain inbox archive: for each new sentinel-bearing archive file,
     mutate the matching task (TASK_DONE_PR, BLOCKED_QUESTION,
     WORKER_FAILED, NEW_TASK).
  5. Dispatch ready tasks under cap: filter by status + deps, sort by
     priority, skip on file-overlap conflict, shell out to `fleet
     dispatch`, drop the worker prompt into ~/.fleet/inbox/<id>.md.
  6. Write back: every mutation goes through the fleet CLI (`fleet
     tasks set/note`). parse.py is read-only inside the skill.

The dispatch path is via the CLI, not Python writes. Go remains the
authoritative writer for tasks.md, agent records, and learnings.
"""
from __future__ import annotations

import errno
import fcntl
import json
import os
import re
import subprocess
import time
from dataclasses import dataclass, field
from pathlib import Path
from typing import Iterable

import conflict
import dispatch as dispatch_mod
import parse
import reaper as reaper_mod
import remote_control
import supervisor as supervisor_mod
import worktree as worktree_mod


# Defaults tuned for the v0.2 single-worker mode (PLAN §"Coordinator
# skill"). cap=1 serializes everything; cap > 1 enables worktree-mode
# parallel dispatch (one git worktree per worker; v0.2.x).
DEFAULT_CAP = 1
# Filename under projects/<name>/ holding the per-project parallelism
# config. Schema: {"parallelism": <int>}. Missing or unparseable file →
# DEFAULT_CAP. Single tunable for now — v0.3 may grow more knobs.
COORD_CONFIG_FILE = "coord-config.json"
# Hard cap on archive files scanned per tick — prevents a runaway tick
# when the inbox archive grows unbounded across coord crashes. Files
# beyond this just wait for the next tick.
_ARCHIVE_SCAN_CAP = 200

# How long after a subagent's archived_at the audit keeps probing its
# branch for bonus PRs. Past this window the audit considers the
# subagent's lane closed: a PR fired weeks later is operator-cleanup
# noise, not a live drift signal. Bounds the per-tick gh-shell-out
# cost (~0.5–1s per record × number of records still inside the
# window). 14d is generous for a project that ships weekly; tune via
# FLEET_AUDIT_FRESHNESS_DAYS if needed.
#
# Resolved per-call (not module-cached) so a test or operator can
# `os.environ` it mid-process without re-importing the module.
def _audit_freshness_seconds() -> int:
    raw = os.environ.get("FLEET_AUDIT_FRESHNESS_DAYS", "14")
    try:
        days = int(raw)
    except ValueError:
        days = 14
    if days < 1:
        days = 1
    return days * 86400


# Sentinel grammar (ENG §5.3). Worker reports use these to communicate
# state changes back to the coord through the inbox archive.
_SENTINEL_PATTERNS = {
    "task_done_pr": re.compile(
        r"^TASK_DONE_PR\s*=?\s*(?P<slug>[a-z0-9._-]+)\s+(?P<url>\S+)\s*$",
    ),
    "blocked_question": re.compile(
        r"^BLOCKED_QUESTION\s*=?\s*(?P<slug>[a-z0-9._-]+)\s+(?P<text>.+)$",
    ),
    "worker_failed": re.compile(
        r"^WORKER_FAILED\s*=?\s*(?P<slug>[a-z0-9._-]+)\s+(?P<reason>.+)$",
    ),
    "new_task": re.compile(
        r"^NEW_TASK\s*=?\s*(?P<slug>[a-z0-9._-]+)\s*$",
    ),
}


@dataclass
class TickResult:
    """Structured result of one tick(). Useful for tests + logs.

    skipped=True with reason="lock-busy" means another tick is in
    flight; the coord exits cleanly. Counters are zero when skipped.

    dispatch_instructions carries the rendered DISPATCH blocks (issue
    #84 Phase A) that the coord agent (Claude) is expected to act on
    by invoking the Agent tool once per block on its NEXT assistant
    turn. SKILL.md's "Worker dispatch protocol" section pins the
    contract.
    """

    skipped: bool = False
    reason: str = ""
    parsed_tasks: int = 0
    reconciled: int = 0
    drained: int = 0
    dispatched: int = 0
    raised: int = 0
    errors: list[str] = field(default_factory=list)
    dispatch_instructions: list[str] = field(default_factory=list)


def tick(
    project: str,
    *,
    coord_id: str = "",
    cwd: str | None = None,
    cap: int = DEFAULT_CAP,
    fleet_home: str | None = None,
    fleet_bin: str = "fleet",
    now_unix: float | None = None,
) -> TickResult:
    """Run one coordinator tick for project. Returns a structured result.

    coord_id: this coord's 8-hex agent ID (defaults to FLEET_AGENT_ID
              env var). Used to scope inbox-archive scanning to this
              coord's archive files only.
    cwd:      where to spawn workers (cap=1 mode). Defaults to current
              working directory.
    cap:      max parallel in-progress workers. Default 1.
    fleet_home: override ~/.fleet for tests.
    fleet_bin:  override `fleet` binary path for tests.
    now_unix:   override time.time() for deterministic tests.
    """
    home = _resolve_home(fleet_home)
    coord_id = coord_id or os.environ.get("FLEET_AGENT_ID", "")
    cwd = cwd or os.getcwd()
    if now_unix is None:
        now_unix = time.time()
    result = TickResult()

    # 1. NB-flock coordinator.lock (PLAN §6 lock acquisition).
    project_dir = home / "projects" / project
    locks_dir = project_dir / ".locks"
    locks_dir.mkdir(parents=True, exist_ok=True)
    lock_path = locks_dir / "coordinator.lock"
    lock_fd = _try_lock(lock_path, holder_id=coord_id)
    if lock_fd is None:
        result.skipped = True
        result.reason = "lock-busy"
        return result
    # Load per-project parallelism config (cap>1 → worktree mode).
    # Caller-provided cap overrides only when it differs from
    # DEFAULT_CAP — that way tests can pin cap=2 explicitly while
    # production deployments rely on the on-disk config.
    if cap == DEFAULT_CAP:
        configured = _load_parallelism(project_dir)
        if configured > 0:
            cap = configured
    try:
        return _tick_locked(
            result, project, project_dir, coord_id, cwd, cap,
            home, fleet_bin, now_unix,
        )
    finally:
        try:
            fcntl.flock(lock_fd, fcntl.LOCK_UN)
        finally:
            os.close(lock_fd)


def _tick_locked(
    result: TickResult,
    project: str,
    project_dir: Path,
    coord_id: str,
    cwd: str,
    cap: int,
    home: Path,
    fleet_bin: str,
    now_unix: float,
) -> TickResult:
    """Body of the tick once we hold the coord lock."""
    # 1.1. Auto-inject /remote-control on first tick per coord (issue #56).
    # Fresh coordinator agents need to attach to `claude remote-control`
    # so the operator's mobile / claude.ai pairing follows the agent.
    # Handoff replacements get this via internal/handoff.FirstAction;
    # this call covers the FRESH path (first dispatch, no prior doc).
    # Idempotent + fail-soft: bootstrap_remote_control short-circuits
    # on the per-coord marker file, and any I/O failure is logged + the
    # tick continues. NEVER blocks the coord; matches fleet-guard
    # discipline.
    #
    # Returns a status string (see remote_control module docstring).
    # Non-OK / non-skipped-marker results land in BOOTSTRAP_LOG inside
    # the function itself. We additionally surface FAILED_* on the
    # tick's errors list so the operator (via `fleet status` or
    # equivalent) sees a breadcrumb when bootstrap can't make progress.
    try:
        status = remote_control.bootstrap_remote_control(
            project, coord_id, fleet_home=home,
        )
        if status in (
            remote_control.STATUS_FAILED_SEED,
            remote_control.STATUS_FAILED_MARKER,
        ):
            result.errors.append(
                f"remote-control bootstrap: {status} "
                f"(see {remote_control.BOOTSTRAP_LOG} for details)"
            )
    except Exception as exc:
        # bootstrap_remote_control already wraps each side-effect in
        # try/except, but a programming error in this module shouldn't
        # take down the tick. Caller's TickResult.errors records it.
        result.errors.append(f"remote-control bootstrap: {exc}")

    # 1.5. Orphan-worktree cleanup. A coord that crashed mid-tick can
    # leave a worktree directory + its git registry entry behind; the
    # next dispatch then trips on "already exists" and the task is
    # un-redispatchable. `git worktree prune` is idempotent and only
    # drops registry entries whose dirs are missing — safe to call
    # every tick. Gated on cap > 1 because cap=1 mode never creates
    # worktrees (byte-identical-to-v0.2.0 invariant); the call would
    # be a no-op there anyway, but skipping it removes one source of
    # behavior drift on the regression-safe path.
    if cap > 1:
        worktree_mod.prune_worktrees(cwd)

    # 2. Read tasks.md (read-only — coord doesn't write tasks.md
    # directly; mutations go via `fleet tasks set/note`).
    tasks_path = project_dir / "tasks.md"
    try:
        f = parse.read(str(tasks_path))
    except (parse.ParseError, parse.SchemaTooNewError, parse.DuplicateSlugError) as exc:
        # ENG §9.3: refuse to tick on parse error. The skill surfaces
        # this to the operator via blocked_reason on the agent record;
        # we just record it in the result and exit cleanly.
        result.errors.append(f"tasks.md parse error: {exc}")
        result.reason = "parse-error"
        result.skipped = True
        return result
    result.parsed_tasks = len(f.tasks)

    # Load coord-state up front — the supervisor mod's slug→agent_id
    # map and supervisor bookkeeping live in the same file as the
    # legacy archive-scan watermark, and the reconcile path needs to
    # forget mappings on worker-clear transitions. Previously this was
    # loaded between reconcile and drain; the supervisor merge moved
    # it earlier.
    state_path = project_dir / "coord-state.json"
    state = _load_coord_state(state_path)

    # 2.5. Reaper pass (DESIGN invariant 5). Runs BEFORE reconcile so a
    # worker whose state.json reports phase=done has its tmux session
    # killed + record archived BEFORE _apply_reconcile flips status to
    # in-review/done. Reaper mutates `state` in place (kill_directive_ts
    # tracking + redispatch_pending flagging). Decisions are surfaced
    # as raises/errors so the operator sees the hard-kill events.
    try:
        reap_decisions = _reap_inflight(
            f.tasks, project=project, home=home, fleet_bin=fleet_bin,
            coord_state=state, now_unix=now_unix,
        )
    except Exception as exc:  # noqa: BLE001
        result.errors.append(f"reaper: {exc}")
        reap_decisions = []
    for dec in reap_decisions:
        if dec.state == "hard-killed":
            result.errors.append(
                f"[P0] reaper hard-killed {dec.slug}: {dec.detail}"
            )
            result.raised += 1
        elif dec.state == "error":
            result.errors.append(
                f"reaper error {dec.slug}: {dec.detail}"
            )

    # 3. Reconcile in-flight workers. Reaper-gated: a worker whose
    # reaper lane is still open (kill_directive sent, grace not yet
    # expired) defers its status flip — _apply_reconcile is suppressed
    # for that action this tick.
    reconciled = _reconcile_inflight(f.tasks, project, fleet_bin, home=home)
    pre_reconcile_tasks_by_slug = {t.slug: t for t in f.tasks}
    reconcile_repo = cwd if cap > 1 else ""
    reconcile_tasks_by_slug = pre_reconcile_tasks_by_slug if cap > 1 else None
    for action in reconciled:
        # Invariant 5 gate: if this action would clear the worker
        # (terminal-ish transition) but the reaper hasn't completed
        # its kill cycle yet, defer the apply. The next tick re-runs
        # reconcile and re-evaluates — once the reaper finishes the
        # archive, this action proceeds.
        if action.clear_worker and not _reaper_lane_clear_for(state, action.slug):
            continue
        try:
            _apply_reconcile(
                action, project, fleet_bin,
                repo=reconcile_repo,
                tasks_by_slug=reconcile_tasks_by_slug,
                home=home,
                full_tasks_by_slug=pre_reconcile_tasks_by_slug,
            )
            result.reconciled += 1
            if action.raised_to_user:
                result.raised += 1
            # Drop slug → agent_id mapping when the worker is gone (any
            # transition that cleared worker_pid). Keeps the map size
            # bounded across long-running supervisor sessions.
            if action.clear_worker:
                supervisor_mod.forget_agent_id(state, action.slug)
                _clear_review_handoff_state(state, action.slug)
                # The reaper entry for this slug was already deleted by
                # reap_probes when the kill succeeded; clear redispatch
                # marker too so a re-dispatch tick doesn't loop forever.
                reaper_mod.clear_redispatch_pending(state, action.slug)
        except Exception as exc:
            result.errors.append(f"reconcile {action.slug}: {exc}")

    # 3.5. Defense-in-depth sweep for done tasks whose worker dir
    # still exists on disk. The reconcile transitions above already
    # fire `fleet workers delete` on the in-review→done flip (and the
    # in-progress→in-review flip when state.json reports phase=done).
    # This sweep handles the leftover cases:
    #   - operator-driven `fleet tasks set status=done` (no skill mediation),
    #   - v0.1-coord-era transitions that pre-date issue #101's
    #     delete_worker_dir wiring,
    #   - any path where a task reached status=done while a stale worker
    #     dir lingered.
    # Skip on tasks already done from a prior tick: existence-check
    # the worker dir first; if missing, no CLI invocation. This keeps
    # the per-tick cost on a clean project at one stat() per done task.
    try:
        swept = _sweep_done_worker_dirs(
            f.tasks, project, fleet_bin, home=home,
        )
        result.reconciled += swept
    except Exception as exc:  # noqa: BLE001
        result.errors.append(f"sweep done worker dirs: {exc}")

    # 4. Drain inbox archive sentinels.
    tasks_by_slug = {t.slug: t for t in f.tasks}
    drained, last_seen = _drain_archive(
        home / "inbox" / "archive",
        coord_id=coord_id,
        since=state.get("last_archive_scan_ts", ""),
        tasks_by_slug=tasks_by_slug,
    )
    # Worktree cleanup needs a base repo + the pre-mutation snapshot of
    # tasks.md (so we can read t.worktree before the sentinel clears
    # it). Single-worker mode keeps tasks_by_slug empty so the cleanup
    # path is a no-op — preserving v0.2.0 byte-identical behavior.
    sentinel_repo = cwd if cap > 1 else ""
    sentinel_tasks_by_slug = tasks_by_slug if cap > 1 else None
    for action in drained:
        try:
            _apply_sentinel(
                action, project, fleet_bin,
                repo=sentinel_repo,
                tasks_by_slug=sentinel_tasks_by_slug,
                home=home,
                full_tasks_by_slug=tasks_by_slug,
            )
            result.drained += 1
            if action.raised_to_user:
                result.raised += 1
            # Worker is leaving the in-flight set on TASK_DONE_PR
            # (status → in-review with closed worker), WORKER_FAILED
            # (status → todo, worker cleared), and BLOCKED_QUESTION
            # (status → blocked). Forget the mapping in all three so
            # the supervisor doesn't keep nudging an inbox owned by
            # a defunct agent record.
            if action.kind in ("task_done_pr", "worker_failed", "blocked_question"):
                supervisor_mod.forget_agent_id(state, action.slug)
                _clear_review_handoff_state(state, action.slug)
        except Exception as exc:
            result.errors.append(f"sentinel {action.slug}: {exc}")
    if last_seen:
        state["last_archive_scan_ts"] = last_seen

    # 5. Dispatch ready tasks under cap.
    # Re-read tasks.md after reconcile/drain so the dispatch-side filter
    # sees the latest in-progress count (mutations went through the
    # fleet CLI — they're durable on disk, we reload).
    try:
        f = parse.read(str(tasks_path))
    except Exception as exc:
        result.errors.append(f"tasks.md re-read failed: {exc}")
        return result
    # 5a. Review handoffs (reviewer-subagent-arch). Detect in-flight
    # tasks whose state.json reports phase=review-pending (→ dispatch
    # reviewer) or phase=review-done (→ dispatch finisher) and emit
    # the DISPATCH blocks. Each handoff counts toward result.dispatched
    # so the TUI surfaces an N-agents bump on the next tick.
    handoffs = _dispatch_review_handoffs(
        tasks=f.tasks,
        project=project,
        fleet_bin=fleet_bin,
        fleet_home=str(home),
        home=home,
        coord_state=state,
    )
    for action in handoffs:
        if action.error:
            result.errors.append(f"handoff {action.slug}: {action.error}")
            continue
        try:
            _apply_dispatch_handoff(action, project, fleet_bin)
            if action.agent_id:
                supervisor_mod.remember_agent_id(state, action.slug, action.agent_id)
                _record_review_handoff_dispatched(
                    state, action.slug, action.handoff_phase,
                )
            if action.dispatch_instruction:
                result.dispatch_instructions.append(action.dispatch_instruction)
            result.dispatched += 1
        except Exception as exc:
            result.errors.append(f"handoff apply {action.slug}: {exc}")
    dispatched = _dispatch_ready(
        tasks=f.tasks,
        project=project,
        cwd=cwd,
        cap=cap,
        fleet_bin=fleet_bin,
        fleet_home=str(home),
    )
    for action in dispatched:
        try:
            _apply_dispatch(action, project, fleet_bin)
            # Persist slug → agent_id so the supervisor loop can address
            # the worker's inbox for nudges without re-parsing tasks.md.
            # Notes accumulate over re-dispatches; coord-state's mapping
            # is a single durable lookup.
            if action.agent_id:
                supervisor_mod.remember_agent_id(state, action.slug, action.agent_id)
            # Issue #84 Phase A: surface the DISPATCH block so the
            # coord agent (Claude) can invoke the Agent tool. We
            # collect AFTER _apply_dispatch — the status flip /
            # state.json bootstrap MUST land on disk before the
            # coord spawns the subagent, so a worker that races to
            # `fleet workers update` finds an in-progress task.
            if action.dispatch_instruction:
                result.dispatch_instructions.append(action.dispatch_instruction)
            result.dispatched += 1
        except Exception as exc:
            result.errors.append(f"dispatch {action.slug}: {exc}")

    # Subagent-lifecycle audit (PR #124 motivating case): walk every
    # subagents/<slug>.json record, probe the worker branch for PRs
    # opened AFTER archived_at, and append flagged entries to the
    # record's post_archive_artifacts list. Surfaces in the TUI as a
    # ⚠ badge on the affected project row. Best-effort — any gh/IO
    # failure is swallowed inside _audit_archived_subagents so a
    # tick never fails on the audit.
    try:
        _audit_archived_subagents(project, home)
    except Exception as exc:  # noqa: BLE001
        result.errors.append(f"subagent audit: {exc}")

    # Heartbeat: rewrite coord-state.json on EVERY tick, even when nothing
    # was drained or dispatched. The Variant A dashboard reads this file's
    # mtime as the per-tick liveness signal — gating the write on
    # last_seen (issue #50) made dispatch-only ticks invisible to the TUI
    # and surfaced as `○ idle · auto-stopped` while the coord was actually
    # working. tmp+rename is cheap and idempotent on identical state, so
    # the unconditional refresh is correct.
    _save_coord_state(state_path, state)

    # Auto-archive when tasks.md grows past the threshold. Runs at end
    # of every tick, after dispatch + heartbeat, before lock release
    # (we still hold the coord lock; archive's own state-lock layers
    # cleanly on top — different paths). Keeps the operator-facing
    # tasks.md trim without manual `fleet tasks archive` ceremony.
    try:
        archived = _maybe_auto_archive(tasks_path, project, fleet_bin)
        if archived:
            result.errors.extend(archived.errors)
    except Exception as exc:  # noqa: BLE001
        # Archive should never fail a tick — log + continue. Operators
        # see the breadcrumb via `fleet status`/blocked_reason.
        result.errors.append(f"auto-archive: {exc}")

    # 6. Supervisor loop (issue #79). After the initial reconcile + drain
    # + dispatch pass we keep the lock and watch in-flight workers. Cheap
    # mtime polling drives event-based reconciliation; a sparse stuck-
    # check pass catches workers that died silently. The legacy
    # single-tick behavior is preserved when poll_interval=0 (or no
    # in-flight tasks) — the supervisor returns immediately.
    sup_cfg = supervisor_mod.SupervisorConfig.from_env()
    if sup_cfg.poll_interval_s > 0:
        _run_supervisor(
            cfg=sup_cfg,
            project=project,
            project_dir=project_dir,
            tasks_path=tasks_path,
            cwd=cwd,
            cap=cap,
            home=home,
            fleet_bin=fleet_bin,
            state_path=state_path,
            result=result,
        )
    return result


def _run_supervisor(
    *,
    cfg,
    project: str,
    project_dir: Path,
    tasks_path: Path,
    cwd: str,
    cap: int,
    home: Path,
    fleet_bin: str,
    state_path: Path,
    result: TickResult,
) -> None:
    """Drive the supervisor loop. Hooks defined here so the supervisor
    module stays free of loop.py's mutation surface.

    The supervisor reads coord-state.json on every stuck-check pass
    (its own internal write), so the local `state` dict here is rebuilt
    on each reconcile-on-mtime-change call from disk. The loop is the
    only writer of supervisor.* + worker_agent_ids inside this skill
    (besides the initial dispatch path).
    """
    # Build initial probes from tasks.md.
    try:
        initial = parse.read(str(tasks_path))
    except Exception as exc:  # noqa: BLE001
        result.errors.append(f"supervisor: tasks.md re-read failed: {exc}")
        return

    # Pull the agent_id map fresh from disk — we just wrote it.
    coord_state_now = _load_coord_state(state_path)
    agent_ids = supervisor_mod.load_agent_id_map(coord_state_now)

    if not _supervisor_has_inflight(initial.tasks):
        # Nothing to watch — the dispatch path scheduled nothing or
        # everything is already terminal. Skip cleanly.
        return

    def refresh_probes():
        try:
            f2 = parse.read(str(tasks_path))
        except Exception:
            return []
        # Reload agent_id map on every refresh so a fresh dispatch
        # mid-loop becomes addressable for nudges.
        cs = _load_coord_state(state_path)
        amap = supervisor_mod.load_agent_id_map(cs)
        return supervisor_mod.build_worker_probes(
            project=project, home=home, tasks=f2.tasks, agent_id_map=amap,
        )

    def _reconcile_slugs(slugs):
        """Run reconcile against the given slugs and apply actions.

        Centralizes the read-tasks → reconcile → apply path used by
        both the mtime-change hook (one slug) and the periodic full
        sweep (all in-flight). Idempotent — running on slugs whose
        worker is alive is a no-op.

        Returns True if any slot was freed (action.new_status in the
        terminal set) so the caller can re-run dispatch to backfill.
        """
        try:
            f3 = parse.read(str(tasks_path))
        except Exception as exc:  # noqa: BLE001
            result.errors.append(f"supervisor reconcile read: {exc}")
            return False
        scoped = [t for t in f3.tasks if t.slug in set(slugs)]
        if not scoped:
            return False
        actions = _reconcile_inflight(
            scoped, project, fleet_bin, home=home,
        )
        cs = _load_coord_state(state_path)
        slot_freed = False
        full_map = {t.slug: t for t in f3.tasks}
        for action in actions:
            try:
                _apply_reconcile(
                    action, project, fleet_bin,
                    repo=(cwd if cap > 1 else ""),
                    tasks_by_slug=(full_map if cap > 1 else None),
                    home=home,
                    full_tasks_by_slug=full_map,
                )
                if action.clear_worker:
                    supervisor_mod.forget_agent_id(cs, action.slug)
                    # Three-stage flow (reviewer-subagent-arch): clear
                    # the review-handoff dispatched markers in sync
                    # with the agent_id forget, mirroring the same
                    # cleanup in the primary tick path. Without this,
                    # supervisor-driven terminal transitions leave
                    # stale handoff keys that block future re-dispatches
                    # for the same slug.
                    _clear_review_handoff_state(cs, action.slug)
                # codex iter-1 [P1]: a worker leaving the in-flight
                # set frees a dispatch slot. Without re-running
                # _dispatch_ready, with cap=1 the next ready task
                # waits hours for the supervisor to exit. Mark the
                # transition so the outer caller dispatches.
                if action.new_status in (
                    "todo", "done", "in-review", "blocked",
                ):
                    slot_freed = True
            except Exception as exc:  # noqa: BLE001
                result.errors.append(
                    f"supervisor reconcile-apply {action.slug}: {exc}"
                )
        _save_coord_state(state_path, cs)
        return slot_freed

    def _maybe_dispatch_after_reconcile():
        """Re-run _dispatch_ready under the same lock when a slot freed.

        codex iter-1 [P1] regress: the supervisor's per-worker reconcile
        flipped a finished worker to in-review/done/todo but the next
        ready task waited until supervisor exited because dispatch only
        ran inside the FIRST tick. Re-running here keeps `cap` saturated
        across the entire supervisor session.
        """
        try:
            f4 = parse.read(str(tasks_path))
        except Exception as exc:  # noqa: BLE001
            result.errors.append(f"supervisor dispatch read: {exc}")
            return
        new_dispatched = _dispatch_ready(
            tasks=f4.tasks,
            project=project,
            cwd=cwd,
            cap=cap,
            fleet_bin=fleet_bin,
            fleet_home=str(home),
        )
        cs = _load_coord_state(state_path)
        for action in new_dispatched:
            try:
                _apply_dispatch(action, project, fleet_bin)
                if action.agent_id:
                    supervisor_mod.remember_agent_id(cs, action.slug, action.agent_id)
                # Issue #84 Phase A: supervisor-loop dispatches still
                # need to publish the DISPATCH block so the coord
                # agent can spawn the Agent subagent. Without this,
                # the supervisor's mid-tick re-dispatch would write
                # the inbox file but never surface the spawn
                # instruction — task would sit in-progress with no
                # actual worker.
                if action.dispatch_instruction:
                    result.dispatch_instructions.append(action.dispatch_instruction)
            except Exception as exc:  # noqa: BLE001
                result.errors.append(f"supervisor dispatch {action.slug}: {exc}")
        _save_coord_state(state_path, cs)

    def reconcile_one(probe):
        # Reconcile one slug whose state.json mtime advanced. The full
        # _reconcile_inflight path is idempotent and matches what the
        # primary tick does, so we just hand it a one-element scope.
        if _reconcile_slugs([probe.slug]):
            _maybe_dispatch_after_reconcile()

    def periodic_full_reconcile():
        """Re-reconcile EVERY in-flight task (not just mtime-changed ones).

        codex iter-1 [P1] regress: in-review tasks have no live worker,
        so their state.json mtime never advances and the mtime-driven
        hook never fires. Without this periodic sweep, the supervisor
        would sit on the lock for hours while a PR's CI flips green
        but tasks.md never advances to done. The sweep runs at the
        same cadence as stuck-check (default every 5 min) and is
        gated behind the same FLEET_COORD_STUCK_CHECK_EVERY knob;
        cost is bounded by len(in-flight) gh API calls per cycle.
        """
        try:
            f3 = parse.read(str(tasks_path))
        except Exception:
            return
        slugs = [
            t.slug for t in f3.tasks
            if t.status in ("in-progress", "in-review")
        ]
        if not slugs:
            return
        if _reconcile_slugs(slugs):
            _maybe_dispatch_after_reconcile()

    def write_state_hook():
        # Heartbeat publish. Re-load → no-op-write so any concurrent
        # supervisor stuck-check write isn't clobbered.
        cs = _load_coord_state(state_path)
        _save_coord_state(state_path, cs)

    # Invariant 4 force-tick: short-circuit the next sleep when there's
    # a pending inbox event for this coord. Cheap fs scan; the supervisor
    # consults this on every iteration before computing the sleep budget.
    def force_tick_check_hook() -> bool:
        cs = _load_coord_state(state_path)
        watermark = str(cs.get("last_archive_scan_ts", "") or "")
        return supervisor_mod.has_pending_inbox_events(
            coord_id=os.environ.get("FLEET_AGENT_ID", "") or "",
            fleet_home=home,
            last_seen_archive=watermark,
        )

    # Invariant 5 reaper hook. Runs every iteration so a worker that
    # writes phase=done mid-supervisor session is reaped within the
    # base poll cadence (5 s default) rather than waiting for the
    # next stuck-check pass (5 min default).
    def reaper_hook_supervisor(probes):
        try:
            f5 = parse.read(str(tasks_path))
        except Exception as exc:  # noqa: BLE001
            result.errors.append(f"reaper hook tasks-read: {exc}")
            return
        cs = _load_coord_state(state_path)
        decisions = _reap_inflight(
            f5.tasks, project=project, home=home, fleet_bin=fleet_bin,
            coord_state=cs, now_unix=time.time(),
        )
        for dec in decisions:
            if dec.state == "hard-killed":
                result.errors.append(
                    f"[P0] reaper hard-killed {dec.slug}: {dec.detail}"
                )
                result.raised += 1
            elif dec.state == "error":
                result.errors.append(
                    f"reaper error {dec.slug}: {dec.detail}"
                )
        _save_coord_state(state_path, cs)

    sup_result = supervisor_mod.run_supervisor(
        cfg=cfg,
        project=project,
        home=home,
        fleet_bin=fleet_bin,
        refresh_probes=refresh_probes,
        reconcile_one=reconcile_one,
        write_state=write_state_hook,
        periodic_full_reconcile=periodic_full_reconcile,
        force_tick_check=force_tick_check_hook,
        reaper_hook=reaper_hook_supervisor,
    )
    # Surface supervisor stats as auxiliary tick result fields. We
    # don't mutate TickResult's primary counters because they describe
    # the FIRST tick (the reconcile/drain/dispatch pass before the
    # supervisor entered).
    if sup_result.errors:
        result.errors.extend(sup_result.errors)


def _supervisor_has_inflight(tasks) -> bool:
    """True if any task is in-flight (in-progress / in-review)."""
    for t in tasks:
        if t.status in ("in-progress", "in-review"):
            return True
    return False


# ---------- reaper integration (invariant 5) ----------
#
# The reaper judges each in-flight worker on every supervisor poll and on
# the primary tick path. When the judgment is `complete` or `error-abort`
# it sends /exit, waits the grace window, falls through to `fleet rm`,
# and last-resort `kill -9`s the pid.
#
# Status-flip ordering: the existing _reconcile_inflight path computes
# `new_status` and `clear_worker` from state.json. We layer the reaper
# under it: the reaper runs first to make sure the tmux session is gone
# (or recorded as in-progress kill); then _apply_reconcile's status flip
# is the operator-visible commit point. If the reaper still has an open
# entry for a slug, _apply_reconcile defers the status flip — matches the
# DESIGN doc: "Before the reaper succeeds, status stays in its prior
# non-terminal state — so the operator never sees 'task done' with a
# still-live worker."


def _build_reap_inputs(
    tasks: list[parse.Task],
    *,
    project: str,
    home: Path,
    agent_id_map: dict[str, str],
    is_git: bool,
) -> list[reaper_mod.ReapInputs]:
    """Build ReapInputs from the in-flight task list.

    Probes every task at status=in-progress or in-review. In-review
    workers have already exited (the PR is open and we're polling CI);
    the reaper just confirms the tmux session is gone — the judgment
    will normally be `complete` (phase=done with pr_url) so we run the
    archive path even when the subprocess exited cleanly without
    a `/exit`.
    """
    out: list[reaper_mod.ReapInputs] = []
    for t in tasks:
        if t.status not in ("in-progress", "in-review"):
            continue
        agent_id = agent_id_map.get(t.slug, "")
        tmux_session = (
            supervisor_mod.session_name_for_agent(agent_id) if agent_id else ""
        )
        worker_state = _read_worker_state(project, t.slug, home=home)
        pid = 0
        if worker_state is not None:
            raw_pid = worker_state.get("pid", 0) or worker_state.get("worker_pid", 0)
            try:
                pid = int(raw_pid)
            except (TypeError, ValueError):
                pid = 0
        out.append(reaper_mod.ReapInputs(
            slug=t.slug,
            agent_id=agent_id,
            tmux_session=tmux_session,
            worker_state=worker_state,
            task_status=t.status,
            pid=pid,
            is_git=is_git,
        ))
    return out


def _reap_inflight(
    tasks: list[parse.Task],
    *,
    project: str,
    home: Path,
    fleet_bin: str,
    coord_state: dict,
    now_unix: float,
) -> list[reaper_mod.ReapDecision]:
    """Run the reaper against every in-flight task. Mutates coord_state
    in place; caller persists via _save_coord_state.

    Returns the decisions list (one per probe). The caller is expected
    to surface stderr-grade [P0] events and translate decisions into
    tick-result error / raise text where relevant.
    """
    if not tasks:
        return []
    agent_id_map = supervisor_mod.load_agent_id_map(coord_state)
    is_git = dispatch_mod.project_is_git(project, fleet_home=str(home))
    inputs = _build_reap_inputs(
        tasks, project=project, home=home,
        agent_id_map=agent_id_map, is_git=is_git,
    )
    if not inputs:
        return []
    grace = reaper_mod.env_grace_window_s()
    decisions = reaper_mod.reap_probes(
        probes_inputs=inputs,
        coord_state=coord_state,
        fleet_bin=fleet_bin,
        now_unix=now_unix,
        grace_window_s=grace,
        session_alive_fn=supervisor_mod.tmux_session_alive,
    )
    return decisions


def _reaper_lane_clear_for(coord_state: dict, slug: str) -> bool:
    """Wrapper so loop.py callers don't import reaper_mod directly when
    only this gate is needed. Returns True iff the reaper has no open
    kill cycle for `slug`."""
    return reaper_mod.reaper_lane_clear(coord_state, slug)


# ---------- lock helpers ----------


def _try_lock(path: Path, *, holder_id: str = "") -> int | None:
    """Acquire LOCK_EX | LOCK_NB on path. Returns the open fd or None.

    Caller is responsible for unlocking + closing on the success path.
    Re-acquiring across hook fires is the documented pattern (ENG §4.3
    "Coordinator-lock lifecycle") — short-lived Python procs release
    the flock automatically on exit.

    holder_id (optional): when non-empty, write `<holder_id>\\n` into
    the lock-file body after acquiring the flock. The Go-side dashboard
    reader (internal/tui/dashboard.go:readCoordHolder) consumes this to
    distinguish "this project's coord agent" from "loose v0.1 agents"
    on the LEFT column of the v0.2 ops console (issue #55).

    Lock-body publication is invariant-safe because:
      - we hold LOCK_EX | LOCK_NB; only this process can write the file,
      - the write is bounded (holder_id is an 8-hex-char agent ID + \\n),
      - kernel-side flock(2) does NOT touch the file body, so a stale
        body persists after the holder exits without re-acquiring; the
        Go reader gates "is this body current?" on coord-state.json's
        mtime within coordActiveWindow (5 min) instead of trusting the
        body alone.
    """
    try:
        fd = os.open(str(path), os.O_RDWR | os.O_CREAT, 0o644)
    except OSError:
        return None
    try:
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except OSError as exc:
        os.close(fd)
        if exc.errno in (errno.EWOULDBLOCK, errno.EAGAIN):
            return None
        raise
    # Best-effort write of the holder ID into the lock body. Truncate
    # first so a previous holder's longer ID can't leave trailing bytes.
    # On any I/O error we keep the lock and let the Go reader fall
    # through to the "no holder" branch — the freshness gate on
    # coord-state.json's mtime is the load-bearing safeguard, body
    # publication is purely an aid.
    if holder_id:
        try:
            os.lseek(fd, 0, os.SEEK_SET)
            os.ftruncate(fd, 0)
            os.write(fd, (holder_id + "\n").encode("ascii"))
            os.fsync(fd)
        except OSError:
            pass
    return fd


def _resolve_home(fleet_home: str | None) -> Path:
    if fleet_home:
        return Path(fleet_home)
    env = os.environ.get("FLEET_HOME")
    if env:
        return Path(env)
    return Path(os.path.expanduser("~/.fleet"))


# ---------- coord state ----------


def _load_coord_state(path: Path) -> dict:
    try:
        with open(path, "r", encoding="utf-8") as fh:
            data = json.load(fh)
        if isinstance(data, dict):
            return data
    except (FileNotFoundError, json.JSONDecodeError):
        pass
    return {}


def _load_parallelism(project_dir: Path) -> int:
    """Read coord-config.json's `parallelism` field. Defaults to 0
    (caller falls through to DEFAULT_CAP).

    Schema: `{"parallelism": <int>}`. Out-of-range values (<1 or >50)
    are clamped to the legal window — coord misconfig should never
    crash the tick. v0.2.x ships parallelism only; future fields will
    live alongside without breaking the loader.
    """
    cfg_path = project_dir / COORD_CONFIG_FILE
    try:
        with open(cfg_path, "r", encoding="utf-8") as fh:
            data = json.load(fh)
    except (FileNotFoundError, json.JSONDecodeError, OSError):
        return 0
    if not isinstance(data, dict):
        return 0
    raw = data.get("parallelism")
    if not isinstance(raw, int) or isinstance(raw, bool):
        return 0
    # Clamp to 1..50; >50 is operator error (50 concurrent workers
    # would saturate fsnotify + tmux long before disk fills).
    if raw < 1:
        return 0
    if raw > 50:
        return 50
    return raw


def _save_coord_state(path: Path, state: dict) -> None:
    """Atomic publish of coord-state.json (tmp + rename + fsync)."""
    _atomic_write_json(path, state)


def _load_review_handoff_state(home: Path, project: str) -> set[str]:
    """Read coord-state.json:review_handoffs and return the set of
    (slug:phase) keys for already-dispatched review handoffs this
    coord has fired.

    The set is read-only here; mutation routes through
    _record_review_handoff_dispatched (which is called from
    _apply_dispatch_handoff). Empty set on missing / malformed file —
    a coord that lost its state file just re-dispatches at most one
    duplicate per (slug, phase), which is recoverable: the second
    reviewer sees the first reviewer's commits and a clean diff to
    review, runs through the loop quickly (two consecutive clean
    passes), and writes phase=review-done a second time. The
    finisher then dispatches as normal.
    """
    state_path = home / "projects" / project / "coord-state.json"
    state = _load_coord_state(state_path)
    raw = state.get("review_handoffs_dispatched")
    if not isinstance(raw, list):
        return set()
    return {str(x) for x in raw if isinstance(x, str)}


def _record_review_handoff_dispatched(
    state: dict, slug: str, phase: str,
) -> None:
    """Append `<slug>:<phase>` to coord-state.review_handoffs_dispatched.

    Called from _apply_dispatch_handoff after the inbox file + DISPATCH
    block have been emitted. The state dict is the same one tick()
    will persist at the end via _save_coord_state — we mutate in
    place. Idempotent: a duplicate key is added without dedup (the
    list grows briefly, but _load_review_handoff_state coerces to a
    set on read, and the entries are cleared on terminal phases via
    _clear_review_handoff_state below).
    """
    raw = state.get("review_handoffs_dispatched")
    if not isinstance(raw, list):
        raw = []
    key = f"{slug}:{phase}"
    if key not in raw:
        raw.append(key)
    state["review_handoffs_dispatched"] = raw


def _clear_review_handoff_state(state: dict, slug: str) -> None:
    """Drop ALL review-handoff entries for the given slug.

    Called when a slug reaches a terminal state (done | blocked |
    failed | todo-after-CI-red etc.) — the worker dir is gone or about
    to be, and any re-dispatch on the same slug starts fresh. Without
    this cleanup the dispatched-list would grow unboundedly across a
    project's lifetime.
    """
    raw = state.get("review_handoffs_dispatched")
    if not isinstance(raw, list):
        return
    prefix = slug + ":"
    state["review_handoffs_dispatched"] = [
        x for x in raw if isinstance(x, str) and not x.startswith(prefix)
    ]


def _atomic_write_json(path: Path, data: dict) -> None:
    """tmp + fsync + rename publish of a JSON file. Caller-owned schema.

    Used by _save_coord_state and the subagent-lifecycle archive paths
    (_write_subagent_archive_record, _audit_archived_subagents). The
    invariant: a crash mid-write never leaves a half-finished JSON at
    `path`. tempfile.mkstemp uses the SAME directory as the target so
    `os.replace` is atomic on POSIX (cross-filesystem moves would not be).
    """
    import tempfile
    parent = path.parent
    parent.mkdir(parents=True, exist_ok=True)
    fd, tmp = tempfile.mkstemp(prefix=path.name + ".tmp.", dir=str(parent))
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as fh:
            json.dump(data, fh, indent=2, sort_keys=True)
            fh.flush()
            os.fsync(fh.fileno())
        os.replace(tmp, path)
    except Exception:
        try:
            os.unlink(tmp)
        except FileNotFoundError:
            pass
        raise


# ---------- reconcile ----------


def _is_worker_alive(t: parse.Task, project: str, home: Path | None = None) -> bool:
    """Return True if the worker behind task t is still working.

    Two signals, OR'd:
      1. worker_pid > 0 AND that PID responds to kill(0). Cheap;
         catches the in-tick window where the coord just dispatched.
      2. workers/<slug>/state.json exists AND its phase is non-terminal
         (not done|blocked|failed) AND updated_at is fresh (within
         _WORKER_STATE_FRESH_S). This is the canonical signal once
         the worker subprocess starts calling `fleet workers update`:
         the OS PID stored on tasks.md is unreliable across coord
         ticks (the coord tick is short-lived) but state.json
         freshness is owned by the live worker subprocess. Without
         this fallback, every coord-dispatched task gets requeued
         the next tick (codex full-stack [P1]).
    """
    if t.worker_pid > 0 and _pid_alive(t.worker_pid):
        return True
    return _worker_state_fresh(project, t.slug, home=home)


# How long a worker's state.json may go without an update before the
# coord treats it as dead (and consults pr_url + CI to decide the next
# status). Longer than the longest expected phase boundary (`/codex
# review` can take several minutes) but shorter than a wedged-process
# wait that would block the queue forever.
_WORKER_STATE_FRESH_S = 15 * 60


def _read_worker_state(
    project: str, slug: str, *, home: Path | None = None,
) -> dict | None:
    """Load workers/<slug>/state.json. Returns None on any read/parse
    failure so callers fall through to their default behavior.

    home: project-tree root (~/.fleet by default). Tests pass an
    isolated tmp_path so the host's real ~/.fleet/projects/ doesn't
    bleed into the test's worker-state visibility.
    """
    if home is None:
        home = Path(os.environ.get("FLEET_HOME") or os.path.expanduser("~/.fleet"))
    state_path = home / "projects" / project / "workers" / slug / "state.json"
    try:
        raw = state_path.read_text(encoding="utf-8")
    except (FileNotFoundError, NotADirectoryError, PermissionError):
        return None
    try:
        return json.loads(raw)
    except json.JSONDecodeError:
        return None


def _worker_state_fresh(
    project: str, slug: str, *, home: Path | None = None,
) -> bool:
    """Read workers/<slug>/state.json and return True iff the worker
    is still publishing progress.

    Returns False on any error (missing file, parse error, terminal
    phase, stale heartbeat) — the caller falls through to the
    pr_url + CI decision tree, matching the v0.2 design's
    "if we can't tell, treat as dead" stance.
    """
    st = _read_worker_state(project, slug, home=home)
    if st is None:
        return False
    phase = st.get("phase", "")
    if phase in ("done", "blocked", "failed"):
        return False
    updated_at = st.get("updated_at", "")
    if not updated_at:
        # Bootstrapped state (just dispatched, no first tick yet).
        # Treat as alive so the worker has a chance to run.
        return True
    try:
        # Workers serialize updated_at via Go time.Time JSON, which is
        # RFC3339 with nanos. Python's fromisoformat handles RFC3339
        # since 3.11; for older interpreters we fall back to a regex
        # strip of the trailing fractional seconds.
        from datetime import datetime, timezone
        ts = datetime.fromisoformat(updated_at.replace("Z", "+00:00"))
        if ts.tzinfo is None:
            ts = ts.replace(tzinfo=timezone.utc)
        age_s = (datetime.now(tz=timezone.utc) - ts).total_seconds()
    except ValueError:
        return False
    return age_s <= _WORKER_STATE_FRESH_S


def _worker_terminal_state(
    project: str, slug: str, *, home: Path | None = None,
) -> tuple[str, str, str] | None:
    """Read workers/<slug>/state.json and, if the worker is in a
    terminal phase, return (phase, pr_url, blocked_reason).

    Returns None when state.json is missing/unparseable or the phase
    is non-terminal — caller falls through to its existing decision
    path (pr_url + CI). This is how reconcile transcribes worker-side
    "I'm done" / "I'm blocked" signals into tasks.md when the worker
    process exited cleanly between ticks (codex full-stack iter-2
    [P1]: without this, a phase=done worker that exited got
    classified as "died without PR" and the task was requeued).
    """
    st = _read_worker_state(project, slug, home=home)
    if st is None:
        return None
    phase = st.get("phase", "")
    if phase not in ("done", "blocked", "failed"):
        return None
    return (
        phase,
        str(st.get("pr_url", "") or ""),
        str(st.get("blocked_reason", "") or ""),
    )


@dataclass
class _ReconcileAction:
    slug: str
    new_status: str | None = None
    clear_worker: bool = False
    note: str = ""
    raised_to_user: bool = False
    raise_text: str = ""
    set_pr_url: str = ""  # populated when a phase=done worker shipped
                          # a PR (codex full-stack iter-2 [P1]:
                          # state.json is the only signal until the
                          # inbox sentinel path lands; reconcile must
                          # transcribe it).
    clear_pr_url: bool = False  # set on requeue paths whose retry will
                                # open a NEW PR (CI red, worker failed).
                                # Without clearing, the stale URL stays
                                # attached and the next reconcile keeps
                                # polling the dead PR (codex iter-3
                                # [P2]).
    delete_worker_dir: bool = False  # issue #101 lifecycle hygiene:
                                     # set when the worker reached a
                                     # terminal phase (done|failed). The
                                     # apply step rm-rf's the worker dir
                                     # AFTER persisting set_pr_url +
                                     # status onto tasks.md, so the
                                     # operator-visible PR URL outlives
                                     # the dir cleanup.


# Subagent-lifecycle archive record path. One file per worker that
# reached phase=done; the post-completion audit re-reads these every
# tick to detect bonus-PR scope drift (CLAUDE.md §8 violation; see
# PR #124 as the motivating case).
#
# Schema (JSON):
#   {
#     "slug": "<task-slug>",
#     "subagent_id": "<8hex>",          # may be ""
#     "branch": "worker/<slug>",
#     "archived_at": "<RFC3339Z>",
#     "expected_pr_url": "<url>",
#     "post_archive_artifacts": [
#       {"pr_number": N, "pr_url": "...", "opened_at": "...",
#        "action": "flag-for-operator"},
#       ...
#     ]
#   }
def _subagent_record_path(home: Path, project: str, slug: str) -> Path:
    return home / "projects" / project / "subagents" / f"{slug}.json"


def _write_subagent_archive_record(
    home: Path,
    project: str,
    slug: str,
    *,
    expected_pr_url: str,
    branch: str = "",
    subagent_id: str = "",
    now_unix: float | None = None,
) -> None:
    """Persist a fresh archive receipt at projects/<p>/subagents/<slug>.json.

    Called from _apply_reconcile when a phase=done worker is being
    cleared (delete_worker_dir + new_status=in-review). archived_at is
    "now" in RFC3339Z; the post-archive audit treats any PR with
    createdAt > archived_at on this worker's branch as scope drift.

    Idempotent: if the record already exists with a populated
    post_archive_artifacts list, the existing list is preserved so a
    re-archive (e.g. a re-dispatched worker that lands again) doesn't
    wipe the audit trail.
    """
    import datetime as _dt
    rec_path = _subagent_record_path(home, project, slug)
    rec_path.parent.mkdir(parents=True, exist_ok=True)

    if now_unix is None:
        archived_at = _dt.datetime.now(tz=_dt.timezone.utc)
    else:
        archived_at = _dt.datetime.fromtimestamp(now_unix, tz=_dt.timezone.utc)
    archived_iso = archived_at.isoformat().replace("+00:00", "Z")

    # Preserve any prior artifacts list across a re-archive (e.g. a CI-red
    # retry that opens a NEW PR and shipping a second time). The operator
    # may have already inspected a flag; nuking the list would erase the
    # audit history.
    prior_artifacts: list = []
    try:
        existing = json.loads(rec_path.read_text(encoding="utf-8"))
        if isinstance(existing, dict):
            raw = existing.get("post_archive_artifacts", [])
            if isinstance(raw, list):
                prior_artifacts = raw
    except (FileNotFoundError, json.JSONDecodeError, OSError):
        pass

    if not branch:
        branch = f"worker/{slug}"

    record = {
        "slug": slug,
        "subagent_id": subagent_id,
        "branch": branch,
        "archived_at": archived_iso,
        "expected_pr_url": expected_pr_url,
        "post_archive_artifacts": prior_artifacts,
    }
    _atomic_write_json(rec_path, record)


def _probe_branch_prs(branch: str) -> list[dict]:
    """Return PRs whose head ref matches `branch`. Each entry has at
    least {"number", "url", "createdAt"} keys.

    Production wires this to `gh pr list --head <branch> --state all
    --json number,url,createdAt`. Empty list on any failure — the audit
    treats absent-data as "no drift" rather than blocking the tick.

    Override via `monkeypatch.setattr(loop, '_probe_branch_prs', ...)`
    in tests; same pattern as _gh_pr_checks. Branch matching means we
    pick up bonus PRs the subagent opened off its OWN worker branch
    (the most common drift). A subagent that opened a PR off main is
    out of scope for this probe — the gh repo-level audit would catch
    those (deferred).
    """
    if not branch:
        return []
    cmd = [
        "gh", "pr", "list",
        "--head", branch,
        "--state", "all",
        "--json", "number,url,createdAt",
    ]
    data, err = _gh_run_json(cmd, timeout_s=10.0)
    if err or not isinstance(data, list):
        return []
    out: list[dict] = []
    for entry in data:
        if isinstance(entry, dict):
            out.append({
                "number": entry.get("number"),
                "url": entry.get("url", ""),
                "createdAt": entry.get("createdAt", ""),
            })
    return out


def _audit_archived_subagents(
    project: str,
    home: Path,
) -> int:
    """Walk projects/<p>/subagents/*.json. For each archived record,
    probe the worker branch for PRs opened AFTER archived_at and append
    any to post_archive_artifacts.

    Returns the count of archived records that got a NEW flag this tick
    (informational; the tick result doesn't propagate this yet — the
    operator-facing signal is the dashboard badge on the project row).

    Idempotent: a flag already present in post_archive_artifacts
    (matched by pr_number) is NOT re-appended. A subagent record with
    no archived_at is skipped (defensive; shouldn't happen post-write
    but a malformed file shouldn't crash the tick).

    Cost cap: each scanned record costs one `gh pr list` shell-out
    (~0.5–1s). To prevent the audit from ballooning a tick's runtime
    once a project has months of archived subagents, we skip records
    whose archived_at is older than _audit_freshness_seconds(). A
    bonus PR that fires weeks after the subagent finished is
    operator-cleanup noise, not a live drift signal — the audit
    window is closed for those.
    """
    from datetime import datetime, timezone
    now_dt = datetime.now(tz=timezone.utc)
    freshness_s = _audit_freshness_seconds()
    sub_dir = home / "projects" / project / "subagents"
    if not sub_dir.is_dir():
        return 0
    flagged_now = 0
    try:
        names = sorted(p.name for p in sub_dir.iterdir() if p.name.endswith(".json"))
    except OSError:
        return 0
    for name in names:
        rec_path = sub_dir / name
        try:
            rec = json.loads(rec_path.read_text(encoding="utf-8"))
        except (FileNotFoundError, json.JSONDecodeError, OSError):
            continue
        if not isinstance(rec, dict):
            continue
        archived_at_s = str(rec.get("archived_at", "") or "")
        if not archived_at_s:
            continue
        try:
            archived_at = datetime.fromisoformat(archived_at_s.replace("Z", "+00:00"))
        except ValueError:
            continue
        # Audit-window guard — see _audit_freshness_seconds() docstring.
        if (now_dt - archived_at).total_seconds() > freshness_s:
            continue
        branch = str(rec.get("branch", "") or "")
        if not branch:
            continue
        existing = rec.get("post_archive_artifacts", [])
        if not isinstance(existing, list):
            existing = []
        seen_numbers = {
            a.get("pr_number")
            for a in existing
            if isinstance(a, dict) and a.get("pr_number") is not None
        }
        # Also exclude the original PR — its createdAt is well before
        # archived_at and we filter on opened_at > archived_at anyway,
        # but pinning by pr_url adds a defense against gh API
        # timezone-tagging quirks at the second boundary.
        expected_url = str(rec.get("expected_pr_url", "") or "")
        prs = _probe_branch_prs(branch)
        new_artifacts: list[dict] = []
        for pr in prs:
            num = pr.get("number")
            url = pr.get("url", "")
            created_s = str(pr.get("createdAt", "") or "")
            if num is None or not created_s:
                continue
            if num in seen_numbers:
                continue
            if url and url == expected_url:
                continue
            try:
                created_at = datetime.fromisoformat(
                    created_s.replace("Z", "+00:00"),
                )
            except ValueError:
                continue
            if created_at <= archived_at:
                continue
            new_artifacts.append({
                "pr_number": num,
                "pr_url": url,
                "opened_at": created_s,
                "action": "flag-for-operator",
            })
        if not new_artifacts:
            continue
        existing.extend(new_artifacts)
        rec["post_archive_artifacts"] = existing
        # Atomic publish — a crash mid-write (gh timeout, OOM) must NOT
        # leave a half-written JSON; the audit re-reads the record on
        # next tick and must always see a parseable file.
        try:
            _atomic_write_json(rec_path, rec)
        except OSError:
            continue
        flagged_now += 1
    return flagged_now


def _reconcile_inflight(
    tasks: list[parse.Task],
    project: str,
    fleet_bin: str,
    *,
    home: Path | None = None,
) -> list[_ReconcileAction]:
    """For each in-flight task, check the worker is alive; otherwise
    decide the next status from state.json's terminal phase, then
    pr_url + CI.

    Returns a list of _ReconcileAction; caller applies via the fleet CLI.
    """
    actions: list[_ReconcileAction] = []
    # One project-mode lookup per reconcile pass. Non-git projects'
    # finishers write phase=done WITHOUT a pr_url; the reconcile path
    # needs to recognize that as success rather than the legacy
    # "worker died without PR" failure (codex iter-1 [P1]).
    fleet_home_str = str(home) if home is not None else None
    is_git = dispatch_mod.project_is_git(project, fleet_home=fleet_home_str)
    for t in tasks:
        if t.status not in ("in-progress", "in-review"):
            continue
        if _is_worker_alive(t, project, home=home):
            continue
        # Worker is gone. Before falling through to pr_url + CI, check
        # whether state.json reports a terminal phase. v0.2 workers
        # only signal completion via `fleet workers update --phase done
        # --pr-url X` (which writes state.json) or `--phase blocked
        # --reason X`. Without copying that signal back to tasks.md
        # here, the reconcile path classifies a cleanly-done worker
        # as "died without PR" and silently requeues the task to todo
        # (codex full-stack iter-2 [P1]).
        #
        # Critically: the terminal-state branch only fires for tasks
        # currently at status=in-progress. Once the task transitions
        # to in-review, the existing pr_url + CI path owns the
        # decision tree (codex iter-3 [P1] — without this guard, a
        # stale state.json with phase=done kept re-flipping the task
        # to in-review every tick and short-circuited the CI checks
        # that finish the merge → done lifecycle).
        if t.status == "in-progress":
            # Three-stage flow handoff phases: phase=review-pending
            # (worker → reviewer) and phase=review-done (reviewer →
            # finisher). The previous subagent exited cleanly to make
            # way for the next; this is NOT a "worker died" failure.
            # The handoff dispatch path (_dispatch_review_handoffs)
            # spawns the next subagent. Skip the reconcile decision
            # tree entirely so we don't transcribe the worker's
            # mid-pipeline exit as a requeue-to-todo.
            mid_phase = _read_worker_state(project, t.slug, home=home)
            if mid_phase is not None and mid_phase.get("phase", "") in (
                "review-pending", "review-done",
            ):
                continue
            terminal = _worker_terminal_state(project, t.slug, home=home)
            if terminal is not None:
                phase, pr_url, blocked_reason = terminal
                # Non-git: finisher's terminal write is phase=done WITHOUT
                # a pr_url. Treat that as TerminalSuccess and flip the
                # task to status=done directly (skip the in-review →
                # CI-poll dance — there is no PR to poll). Codex iter-1
                # [P1] regression — the legacy `phase == "done" and pr_url`
                # branch fell through to "worker died without PR" and
                # requeued every successful non-git task to todo.
                if not is_git and phase == "done":
                    actions.append(_ReconcileAction(
                        slug=t.slug, new_status="done",
                        clear_worker=True,
                        note="non-git worker phase=done (no PR)",
                        raised_to_user=True,
                        raise_text=f"non-git worker shipped {t.slug}",
                        delete_worker_dir=True,
                    ))
                    continue
                if phase == "done" and pr_url:
                    # Worker shipped — flip to in-review with the PR
                    # URL so the next tick's pr_url branch runs gh
                    # checks against the new PR.
                    action = _ReconcileAction(
                        slug=t.slug, new_status="in-review",
                        clear_worker=True,
                        note=f"worker phase=done, PR {pr_url}",
                        raised_to_user=True,
                        raise_text=f"worker shipped {t.slug}: {pr_url}",
                    )
                    # Always overwrite tasks.md.pr_url with the
                    # fresh state.json value: a re-dispatched worker
                    # opens a NEW PR after a CI-red retry, and
                    # leaving the stale PR URL would have the next
                    # reconcile poll the wrong PR (codex iter-3 [P2]).
                    action.set_pr_url = pr_url
                    # Issue #101 lifecycle hygiene: worker reached
                    # TerminalSuccess. set_pr_url runs first in
                    # _apply_reconcile so the operator-visible PR
                    # URL outlives the dir cleanup.
                    action.delete_worker_dir = True
                    actions.append(action)
                    continue
                if phase == "blocked" and blocked_reason:
                    # Lifecycle Waiting — operator may unblock the
                    # task; the dir's blocked_reason is still useful
                    # context. KEEP the dir (no delete_worker_dir).
                    actions.append(_ReconcileAction(
                        slug=t.slug, new_status="blocked",
                        clear_worker=True,
                        note=f"worker blocked: {blocked_reason}",
                        raised_to_user=True,
                        raise_text=f"{t.slug} blocked: {blocked_reason}",
                    ))
                    continue
                if phase == "failed":
                    # Lifecycle TerminalFailure — delete the dir.
                    actions.append(_ReconcileAction(
                        slug=t.slug, new_status="todo", clear_worker=True,
                        clear_pr_url=True,
                        note="worker failed",
                        raised_to_user=True,
                        raise_text=f"{t.slug} worker failed",
                        delete_worker_dir=True,
                    ))
                    continue
                # phase=done without pr_url, or phase=blocked
                # without reason — fall through to pr_url + CI; the
                # worker didn't honor the contract.
        if t.pr_url:
            ci = _gh_pr_checks(t.pr_url)
            if ci.all_green and ci.merged:
                # Task done; any lingering worker dir (likely
                # phase=done from earlier) is deletable.
                actions.append(_ReconcileAction(
                    slug=t.slug, new_status="done", clear_worker=True,
                    delete_worker_dir=True,
                ))
            elif ci.all_green and not ci.merged:
                # Worker exited done; tasks.md already at in-review.
                # Dir is no-longer-needed read-only state from the
                # worker — delete to keep the tree neat.
                actions.append(_ReconcileAction(
                    slug=t.slug, new_status="in-review",
                    raised_to_user=True,
                    raise_text=f"CI green for {t.slug}, ready to merge",
                    delete_worker_dir=True,
                ))
            elif not ci.mergeable:
                # Rebase needed — keep the existing pr_url; the
                # operator (or a re-dispatch) will rebase the SAME
                # branch onto main, so the PR URL is still the right
                # poll target. Worker that opened the PR is gone;
                # the dir on disk is junk now → delete.
                actions.append(_ReconcileAction(
                    slug=t.slug, new_status="todo", clear_worker=True,
                    note="rebase needed",
                    delete_worker_dir=True,
                ))
            elif ci.failed:
                # CI red — clear pr_url so a re-dispatched worker's
                # new PR (different branch / different number) becomes
                # the next poll target. Without clear_pr_url, the
                # stale failed PR URL stays attached and reconcile
                # in the next cycle re-polls the dead PR forever
                # (codex iter-3 [P2]).
                actions.append(_ReconcileAction(
                    slug=t.slug, new_status="todo", clear_worker=True,
                    clear_pr_url=True,
                    note=f"CI red {t.pr_url}",
                    raised_to_user=True,
                    raise_text=f"CI red for {t.slug}",
                    delete_worker_dir=True,
                ))
            else:
                # CI pending — leave as-is until next tick.
                continue
        else:
            # Worker died without PR. Dir might exist with stale
            # state.json or might be missing entirely; delete is
            # idempotent on ENOENT, so it's safe to fire.
            actions.append(_ReconcileAction(
                slug=t.slug, new_status="todo", clear_worker=True,
                note="worker died without PR",
                delete_worker_dir=True,
            ))
    return actions


def _pid_alive(pid: int) -> bool:
    """Return True if pid is alive. kill -0 — sends no signal, just
    error-reports if the PID isn't ours / doesn't exist."""
    if pid <= 0:
        return False
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return False
    except PermissionError:
        # PID exists but is owned by a different user — alive enough
        # for our purposes (it's still draining on the system).
        return True
    return True


@dataclass
class _CIResult:
    all_green: bool = False
    merged: bool = False
    mergeable: bool = True
    failed: bool = False
    pending: bool = False
    error: str = ""


def _gh_pr_checks(pr_url: str, timeout_s: float = 15.0) -> _CIResult:
    """Run `gh pr checks` AND `gh pr view` for full reconcile signal.

    ENG §9.4 distinguishes four post-worker-death states:
      1. all green + merged                 → status=done
      2. all green + not merged             → status=in-review (raise)
      3. not mergeable (conflicts)          → status=todo (rebase)
      4. failed                             → status=todo (CI red, raise)

    The four states require BOTH `gh pr checks` (per-check state +
    conclusion) and `gh pr view` (PR-level state + mergeable signal).
    Querying only one collapses cases 1↔2 and 3↔4. We run both and
    synthesize the result; either failing leaves _CIResult.error set
    so the caller treats the task as 'unknown' and skips this tick.
    """
    checks_data, err = _gh_run_json(
        ["gh", "pr", "checks", pr_url, "--json", "state,conclusion"],
        timeout_s=timeout_s,
    )
    if err:
        return _CIResult(error=err)
    view_data, err = _gh_run_json(
        ["gh", "pr", "view", pr_url, "--json", "state,mergeable"],
        timeout_s=timeout_s,
    )
    if err:
        # PR view failed but checks succeeded — leave the task with
        # error set so the caller skips. Otherwise we'd misroute on a
        # missing mergeable signal.
        return _CIResult(error=err)

    # PR-level state: "OPEN", "CLOSED", "MERGED" per gh schema.
    pr_state = ""
    mergeable = True
    if isinstance(view_data, dict):
        pr_state = (view_data.get("state") or "").upper()
        # `gh pr view` mergeable values: MERGEABLE, CONFLICTING, UNKNOWN.
        # Treat UNKNOWN as mergeable=True (don't trigger rebase on a
        # transient gh side answer); CONFLICTING is the only failure.
        mergeable_str = (view_data.get("mergeable") or "").upper()
        mergeable = mergeable_str != "CONFLICTING"
    merged = pr_state == "MERGED"

    if not isinstance(checks_data, list):
        checks_data = []
    if not checks_data:
        # No checks configured on the repo — treat as all green.
        return _CIResult(
            all_green=True, merged=merged, mergeable=mergeable,
        )
    all_green = True
    failed = False
    pending = False
    for check in checks_data:
        if not isinstance(check, dict):
            continue
        state = (check.get("state") or "").upper()
        conclusion = (check.get("conclusion") or "").upper()
        if state in ("PENDING", "QUEUED", "IN_PROGRESS") or conclusion == "":
            pending = True
            all_green = False
        elif conclusion == "SUCCESS":
            continue
        else:
            failed = True
            all_green = False
    return _CIResult(
        all_green=all_green and not pending,
        merged=merged,
        mergeable=mergeable,
        failed=failed,
        pending=pending,
    )


def _gh_run_json(cmd: list[str], *, timeout_s: float):
    """Run a gh subcommand expected to return JSON. Returns (data, err).

    err is "" on success, else a one-line error message. Caller routes
    err through _CIResult.error so the reconcile loop skips this tick.
    """
    try:
        proc = subprocess.run(
            cmd, capture_output=True, text=True, timeout=timeout_s,
            check=False,
        )
    except (FileNotFoundError, subprocess.TimeoutExpired) as exc:
        return None, str(exc)
    if proc.returncode != 0:
        return None, (proc.stderr or proc.stdout or "").strip()
    try:
        return json.loads(proc.stdout or "null"), ""
    except json.JSONDecodeError as exc:
        return None, f"json decode: {exc}"


def _apply_reconcile(
    action: _ReconcileAction,
    project: str,
    fleet_bin: str,
    *,
    repo: str = "",
    tasks_by_slug: dict[str, parse.Task] | None = None,
    home: Path | None = None,
    full_tasks_by_slug: dict[str, parse.Task] | None = None,
) -> None:
    """Apply an _ReconcileAction via the fleet CLI.

    Each `fleet tasks` invocation passes `--project <project>` explicitly
    so the CLI's cwd-default project resolution (`tui.ProjectTag(cwd)`)
    can't drift away from the coord's project. Without this guard a
    coord whose cwd's parent-basename sanitizes differently from its
    own project name would mutate a sibling project's tasks.md — a
    silent corruption that's invisible until the wrong project's task
    list shows surprise edits.

    `repo` + `tasks_by_slug` non-empty in worktree-mode (cap>1): on
    a terminal transition (status=done from CI-merged, or status=todo
    after worker died) we remove the per-slug worktree. Same best-
    effort semantics as the sentinel-side cleanup — failures stay out
    of the reconcile result.
    """
    # Order is load-bearing for the lifecycle hygiene contract (issue
    # #101): the PR URL MUST land on tasks.md BEFORE the worker dir is
    # rm-rf'd. set_pr_url runs ahead of delete_worker_dir; otherwise a
    # crash between them would lose the operator-visible PR link.
    if action.new_status:
        _run_fleet([fleet_bin, "tasks", "set", "--project", project, action.slug, f"status={action.new_status}"])
    if action.clear_pr_url:
        _run_fleet([fleet_bin, "tasks", "set", "--project", project, action.slug, "pr_url="])
    if action.set_pr_url:
        _run_fleet([fleet_bin, "tasks", "set", "--project", project, action.slug, f"pr_url={action.set_pr_url}"])
    if action.clear_worker:
        _run_fleet([fleet_bin, "tasks", "set", "--project", project, action.slug, "worker_pid=0"])
    if action.note:
        _run_fleet([fleet_bin, "tasks", "note", "--project", project, action.slug, action.note])
    # Worktree cleanup on terminal transitions (cap>1 only). Done +
    # todo-without-PR are the two states where the working tree is no
    # longer needed: the PR is merged (done) or the worker bailed before
    # opening a PR (todo). Other transitions (in-review, blocked) keep
    # the worktree because a re-dispatch may want to resume on the same
    # checkout.
    if action.new_status in ("done", "todo"):
        _maybe_remove_worktree(action.slug, repo, tasks_by_slug, fleet_bin, project)
    # Issue #101 lifecycle hygiene: rm-rf workers/<slug>/ on terminal
    # transitions (done/failed). Delete is idempotent on missing dir
    # so a coord-then-TUI race is safe — first mover wins. Best-effort:
    # any failure logs to stderr and does NOT roll back the tasks.md
    # mutations above, matching the worktree-cleanup discipline.
    if action.delete_worker_dir:
        _maybe_delete_worker_dir(action.slug, fleet_bin, project)
    # Subagent-lifecycle archive receipt — only on the phase=done +
    # PR-shipped path. set_pr_url is the signal that this was a
    # TerminalSuccess (the only branch in _reconcile_inflight that
    # populates set_pr_url). phase=failed / phase=blocked / rebase /
    # CI-red paths all skip this — those subagents didn't reach §7
    # contract emit, so the post-archive audit window doesn't open.
    #
    # Best-effort: a write failure is logged to stderr and does NOT
    # roll back the tasks.md mutations above. The audit signal is a
    # nice-to-have; correctness lives in tasks.md.
    if action.set_pr_url and home is not None:
        try:
            branch = ""
            # full_tasks_by_slug is the unfiltered pre-reconcile map
            # (cap=1 and cap>1 both have it). tasks_by_slug is the
            # worktree-cleanup-gated variant which is None under cap=1.
            # The archive write only needs branch lookup; falling back
            # through both keeps branch-resolution working in either
            # mode.
            lookup = full_tasks_by_slug or tasks_by_slug
            if lookup is not None:
                tk = lookup.get(action.slug)
                if tk is not None and tk.branch:
                    branch = tk.branch
            _write_subagent_archive_record(
                home, project, action.slug,
                expected_pr_url=action.set_pr_url,
                branch=branch,
            )
        except Exception as exc:  # noqa: BLE001
            import sys
            print(
                f"coord: subagent archive write failed for {action.slug}: {exc}",
                file=sys.stderr,
            )
    # Note: action.raised_to_user is informational; the operator sees
    # the raise via the agent record's needs_input + the appended note.
    # The skill doesn't fan out a separate inbox message — the next
    # `fleet status` shows the asking row.


# ---------- drain inbox archive ----------


@dataclass
class _SentinelAction:
    slug: str
    kind: str  # "task_done_pr" | "blocked_question" | "worker_failed" | "new_task"
    payload: str = ""
    raised_to_user: bool = False
    raise_text: str = ""
    # delete_worker_dir is set in _apply_sentinel based on `kind`
    # (task_done_pr / worker_failed → True; blocked_question /
    # new_task → False). It's not part of the parsed sentinel grammar
    # — the field lives on the action so the apply path can drive
    # the lifecycle cleanup uniformly with _ReconcileAction.


def _drain_archive(
    archive_dir: Path,
    *,
    coord_id: str,
    since: str,
    tasks_by_slug: dict[str, parse.Task],
) -> tuple[list[_SentinelAction], str]:
    """Scan inbox/archive/ for files newer than `since` matching this coord.

    Files are named `<coord_id>-<UTCstamp>.md`. We sort lex (which is
    chronological for the stamp format `YYYYMMDD-HHMMSSZ...`) and walk
    until we hit our cap. `since` is the last-seen archive filename;
    files <= since are skipped (we already processed them).

    Returns (actions, new_since). Caller persists new_since to coord-state.
    """
    if not archive_dir.is_dir():
        return [], since
    if not coord_id:
        return [], since
    actions: list[_SentinelAction] = []
    last_seen = since
    try:
        files = sorted(p.name for p in archive_dir.iterdir())
    except OSError:
        return [], since
    scanned = 0
    for name in files:
        if scanned >= _ARCHIVE_SCAN_CAP:
            break
        if not name.startswith(coord_id + "-"):
            continue
        if since and name <= since:
            continue
        scanned += 1
        last_seen = name
        try:
            body = (archive_dir / name).read_text(encoding="utf-8")
        except OSError:
            continue
        # ENG §5.3 / §6.3 contract: "each file has a known schema:
        # one sentinel per file." Apply only the FIRST recognized
        # sentinel; subsequent sentinel-shaped lines in the same file
        # are operator narrative or accidental drift and must not
        # produce a second mutation. Two sentinels for the same task
        # in one file would otherwise double-apply (e.g. status set
        # twice, two `note` calls); two sentinels for DIFFERENT tasks
        # would silently mutate both off the same delivery, breaking
        # the slug-keyed isolation guarantee.
        for line in body.splitlines():
            sentinel = _parse_sentinel(line)
            if sentinel is None:
                continue
            if sentinel.slug not in tasks_by_slug:
                # Slug-mismatch logging path (ENG §6.4): coord ignores
                # this file. The watermark already advanced to `name`
                # so we don't re-scan; sticking to one sentinel per
                # file means we don't keep walking past it.
                break
            actions.append(sentinel)
            break
    return actions, last_seen


def _parse_sentinel(line: str) -> _SentinelAction | None:
    """Parse one line from an archive file into a sentinel action.

    Returns None for lines that don't match any sentinel pattern (most
    of an archive file is the agent's narrative response, not
    sentinels). The grammar is intentionally narrow — sentinels live
    on their own lines starting with one of the known keys.
    """
    line = line.strip()
    if not line:
        return None
    for kind, pat in _SENTINEL_PATTERNS.items():
        m = pat.match(line)
        if not m:
            continue
        slug = m.group("slug")
        if kind == "task_done_pr":
            return _SentinelAction(
                slug=slug, kind=kind, payload=m.group("url"),
            )
        if kind == "blocked_question":
            return _SentinelAction(
                slug=slug, kind=kind, payload=m.group("text"),
                raised_to_user=True,
                raise_text=f"{slug} blocked: {m.group('text')}",
            )
        if kind == "worker_failed":
            return _SentinelAction(
                slug=slug, kind=kind, payload=m.group("reason"),
            )
        if kind == "new_task":
            return _SentinelAction(slug=slug, kind=kind)
    return None


def _apply_sentinel(
    action: _SentinelAction,
    project: str,
    fleet_bin: str,
    *,
    repo: str = "",
    tasks_by_slug: dict[str, parse.Task] | None = None,
    home: Path | None = None,
    full_tasks_by_slug: dict[str, parse.Task] | None = None,
) -> None:
    """Apply a parsed sentinel via the fleet CLI.

    `--project <project>` is threaded into every mutation so a coord
    whose cwd resolves to a different sanitized name than its project
    can't accidentally mutate a sibling project's tasks.md (see
    _apply_reconcile docstring for the failure mode).

    `repo` + `tasks_by_slug` arrive non-empty in worktree-mode (cap>1):
    on TASK_DONE_PR the worker's worktree is removed (the branch
    lives on, but the working tree under
    ~/.fleet/projects/<p>/worktrees/<slug>/ is no longer needed since
    the PR is open). WORKER_FAILED also clears the worktree because the
    next dispatch creates a fresh one. Both are best-effort — failures
    log to stderr but don't roll back the tasks.md mutation.
    """
    if action.kind == "task_done_pr":
        # Order matters for issue #101: pr_url onto tasks.md FIRST,
        # then status flip, then worker dir delete. The PR URL must
        # be durable on the task entry before the worker dir (the
        # only on-disk source of the URL pre-persist) is rm-rf'd.
        _run_fleet([fleet_bin, "tasks", "set", "--project", project, action.slug, f"pr_url={action.payload}"])
        _run_fleet([fleet_bin, "tasks", "set", "--project", project, action.slug, "status=in-review"])
        _maybe_remove_worktree(action.slug, repo, tasks_by_slug, fleet_bin, project)
        # Worker reached TerminalSuccess — rm-rf workers/<slug>/.
        _maybe_delete_worker_dir(action.slug, fleet_bin, project)
        # Subagent-lifecycle archive receipt. Same Terminal-success
        # path as the reconcile branch above — a TASK_DONE_PR sentinel
        # IS the worker's "I shipped" signal, so we open the
        # post-archive audit window from this point.
        if home is not None:
            try:
                branch = ""
                lookup = full_tasks_by_slug or tasks_by_slug
                if lookup is not None:
                    tk = lookup.get(action.slug)
                    if tk is not None and tk.branch:
                        branch = tk.branch
                _write_subagent_archive_record(
                    home, project, action.slug,
                    expected_pr_url=action.payload,
                    branch=branch,
                )
            except Exception as exc:  # noqa: BLE001
                import sys
                print(
                    f"coord: subagent archive write failed for {action.slug}: {exc}",
                    file=sys.stderr,
                )
    elif action.kind == "blocked_question":
        # Lifecycle Waiting — operator may un-block the task; KEEP
        # the worker dir (its blocked_reason is still useful context).
        _run_fleet([fleet_bin, "tasks", "set", "--project", project, action.slug, "status=blocked"])
        if action.payload:
            _run_fleet([fleet_bin, "tasks", "note", "--project", project, action.slug, f"BLOCKED_QUESTION: {action.payload}"])
    elif action.kind == "worker_failed":
        _run_fleet([fleet_bin, "tasks", "set", "--project", project, action.slug, "status=todo"])
        _run_fleet([fleet_bin, "tasks", "set", "--project", project, action.slug, "worker_pid=0"])
        if action.payload:
            _run_fleet([fleet_bin, "tasks", "note", "--project", project, action.slug, f"WORKER_FAILED: {action.payload}"])
        _maybe_remove_worktree(action.slug, repo, tasks_by_slug, fleet_bin, project)
        # Worker reached TerminalFailure — rm-rf workers/<slug>/.
        _maybe_delete_worker_dir(action.slug, fleet_bin, project)
    elif action.kind == "new_task":
        # Wake-only sentinel — nothing to apply. Presence of the file
        # was the wake; dispatch_ready in the same tick will pick up
        # the new task if it's ready.
        return


def _maybe_delete_worker_dir(slug: str, fleet_bin: str, project: str) -> None:
    """Best-effort `fleet workers delete` for the named slug.

    Issue #101 lifecycle hygiene path. Fired on terminal-phase
    transitions (done/failed) AFTER the operator-visible PR URL has
    been persisted onto the task entry. Idempotent on already-gone
    dirs (the Go-side workers.Delete returns nil on ENOENT).

    Failures are logged to stderr but do NOT bubble up. The worker
    dir may persist on disk; the TUI defense-in-depth `scanWorkers`
    pass picks up any orphan on the next render. Aborting the
    reconcile/sentinel apply on a cleanup failure would leave
    tasks.md inconsistent with reality.
    """
    if not slug or not project:
        return
    try:
        _run_fleet([fleet_bin, "workers", "delete", "--project", project, slug])
    except Exception as exc:  # noqa: BLE001
        import sys
        print(
            f"coord: workers delete failed for {slug}: {exc}",
            file=sys.stderr,
        )


def _sweep_done_worker_dirs(
    tasks: list[parse.Task],
    project: str,
    fleet_bin: str,
    *,
    home: Path | None = None,
) -> int:
    """Defense-in-depth: rm-rf workers/<slug>/ for tasks at status=done
    whose worker dir still exists.

    Catches three accumulation sources the reconcile-transition path
    misses:
      - operator-driven `fleet tasks set status=done` (no skill seam),
      - v0.1-coord transitions that pre-date issue #101's
        delete_worker_dir wiring,
      - any race where the task flipped done elsewhere while a stale
        worker dir lingered.

    Skip on tasks already done from a prior tick: existence-check the
    workers/<slug>/ dir up-front. If absent, the sweep is a no-op for
    that slug (zero CLI invocations). The cost on a clean project is
    one stat() per done task.

    Failures log to stderr; never abort the tick. Returns the count
    of dirs we successfully kicked into `fleet workers delete` (used
    by callers for reporting).
    """
    if not project or not tasks:
        return 0
    fleet_home = home if home is not None else _resolve_home(None)
    project_workers = fleet_home / "projects" / project / "workers"
    swept = 0
    for t in tasks:
        if t.status != "done":
            continue
        # Stat-first short-circuit: workers.Delete is idempotent on
        # ENOENT, but firing the CLI every tick on every done task
        # would emit "already gone" noise to stderr. The on-disk
        # check is one stat() — cheaper than a subprocess fork.
        slug_dir = project_workers / t.slug
        try:
            if not slug_dir.is_dir():
                continue
        except OSError:
            # Permission / FS error on stat — leave it for a future
            # tick. Don't crash the sweep.
            continue
        _maybe_delete_worker_dir(t.slug, fleet_bin, project)
        swept += 1
    return swept


def _maybe_remove_worktree(
    slug: str,
    repo: str,
    tasks_by_slug: dict[str, parse.Task] | None,
    fleet_bin: str,
    project: str,
) -> None:
    """Best-effort `git worktree remove` for the named slug.

    No-op when:
      - tasks_by_slug is None (single-worker tick),
      - the task block has no worktree path set (cap=1 mode),
      - repo is empty (caller didn't supply a base repo).

    Worktree removal failures are logged to stderr but do NOT bubble up.
    The worktree dir may persist on disk; the operator cleans manually
    via `git worktree prune`. Aborting the sentinel apply on a cleanup
    failure would leave tasks.md inconsistent with reality.
    """
    if not tasks_by_slug or not repo:
        return
    t = tasks_by_slug.get(slug)
    if t is None or not t.worktree:
        return
    res = worktree_mod.remove_worktree(repo, t.worktree)
    if res.error:
        # Surface to stderr; the tick result already records the
        # caller's note. We don't fail the sentinel — the task is
        # already in-review per the operator's tasks.md.
        import sys
        print(f"coord: worktree remove failed for {slug}: {res.error}", file=sys.stderr)
        return
    # Clear the worktree field on tasks.md so a re-dispatch (CI red,
    # operator re-promote) doesn't think the old worktree is still live.
    try:
        _run_fleet([fleet_bin, "tasks", "set", "--project", project, slug, "worktree="])
    except Exception:
        # Non-fatal — same logic as worktree-remove failure.
        pass


# ---------- dispatch ----------


@dataclass
class _DispatchAction:
    slug: str
    agent_id: str = ""
    branch: str = ""
    worktree: str = ""  # populated only in cap>1 worktree-mode dispatch;
                        # empty string means "task ran in repo root, no
                        # worktree to clean up on terminal".
    dispatch_instruction: str = ""  # rendered DISPATCH block (issue #84
                                    # Phase A) — coord agent reads tick
                                    # output and invokes Agent tool per
                                    # block. Empty when error is set.
    handoff_phase: str = ""  # three-stage flow: "review-pending"
                              # (reviewer dispatch) or "review-done"
                              # (finisher dispatch). Empty for the
                              # initial worker dispatch — _apply_dispatch
                              # uses this to decide whether to flip
                              # task status / bootstrap state.json
                              # (worker only; reviewer/finisher reuse
                              # the existing slot).
    error: str = ""


def _dispatch_ready(
    *,
    tasks: list[parse.Task],
    project: str,
    cwd: str,
    cap: int,
    fleet_bin: str,
    fleet_home: str,
) -> list[_DispatchAction]:
    """Filter to dispatchable candidates, sort by priority, dispatch
    under cap. Returns the actions we successfully (or unsuccessfully)
    started — each action carries its own error so caller can decide
    what to record.

    Worktree-mode (cap > 1): each dispatched worker gets its own git
    worktree under ~/.fleet/projects/<p>/worktrees/<slug>/, branched
    `worker/<slug>` off the repo's current HEAD. Worker's cwd is the
    worktree path (NOT the main repo). A failed worktree create
    aborts that one dispatch — the loop continues with the next
    candidate so a stale on-disk worktree doesn't block all of cap.

    Single-worker mode (cap == 1): unchanged from v0.2.0 — every
    worker uses the project's main repo as its cwd, no worktree, no
    cleanup. Byte-identical behavior; this is the regression-safe
    path.
    """
    in_progress = [t for t in tasks if t.status == "in-progress"]
    active = len(in_progress)
    if active >= cap:
        return []

    candidates = _filter_ready(tasks)
    candidates.sort(key=_priority_sort_key)
    actions: list[_DispatchAction] = []
    standards_md = dispatch_mod.fetch_standards(project, fleet_bin=fleet_bin)
    learnings_text = dispatch_mod.fetch_learnings(project, fleet_bin=fleet_bin)
    # Per-dispatch git-mode lookup: read meta.json's is_git field. Legacy
    # projects (no is_git) and read errors default to True (git mode) —
    # the conservative branch keeps existing behavior intact.
    is_git = dispatch_mod.project_is_git(project, fleet_home=fleet_home)
    in_flight_after_dispatch: list[parse.Task] = list(in_progress)

    for t in candidates:
        if active >= cap:
            break
        # Conflict check is gated on cap > 1: cap=1 already serializes
        # everything (only one in-progress at a time), so the wrapper is
        # provably a no-op there. Skipping the call keeps single-worker
        # mode byte-identical to v0.2.0 and removes one source of
        # behavior drift from the regression-safe path.
        if cap > 1 and _has_conflict_with_inflight(t, in_flight_after_dispatch):
            continue
        # Worktree mode: cap>1. Resolve canonical path via the Go CLI,
        # then `git worktree add`. On any failure we record the error
        # and skip this task — leaving stale state would corrupt the
        # next tick's view of in-flight tasks.
        #
        # Non-git projects skip worktree creation entirely — there is
        # no git to branch from. The dispatch falls through to single-
        # worker behavior (worker_cwd=cwd, no worktree) regardless of
        # cap. Operators who run cap>1 on a non-git project would
        # otherwise hit `git worktree add` failing on every tick.
        worker_cwd = cwd
        worker_branch = f"worker/{t.slug}"
        worker_worktree = ""
        if cap > 1 and is_git:
            wt_path = worktree_mod.compute_worktree_path(
                project, t.slug, fleet_bin=fleet_bin,
            )
            if not wt_path:
                actions.append(_DispatchAction(
                    slug=t.slug,
                    error=f"worktree-path resolution failed for {t.slug}",
                ))
                continue
            wt_result = worktree_mod.create_worktree(
                cwd, wt_path, worker_branch,
            )
            if wt_result.error:
                actions.append(_DispatchAction(
                    slug=t.slug, error=wt_result.error,
                ))
                continue
            worker_cwd = wt_result.path
            worker_worktree = wt_result.path

        try:
            prompt = dispatch_mod.build_worker_prompt(
                t, project=project,
                standards_md=standards_md,
                learnings_text=learnings_text,
                branch=worker_branch,
                worktree_pre_created=bool(worker_worktree),
                is_git=is_git,
            )
        except dispatch_mod.PromptTooLargeError as exc:
            actions.append(_DispatchAction(slug=t.slug, error=str(exc)))
            continue
        # Issue #84 Phase A: skill mints agent_id, writes inbox, emits
        # a DISPATCH block. Coord agent (Claude session) reads the
        # block from tick stdout and invokes the Agent tool with
        # run_in_background=true — workers show up as native subagents
        # in coord's chat ("N local agents") instead of detached tmux
        # sessions. The Python skill never calls `fleet dispatch` for
        # workers anymore (the Go CLI stays for v0.1 manual use).
        agent_id = dispatch_mod.mint_agent_id()
        try:
            inbox_path = dispatch_mod.write_worker_inbox(
                agent_id, prompt, fleet_home=fleet_home,
            )
        except Exception as exc:
            actions.append(_DispatchAction(
                slug=t.slug, agent_id=agent_id,
                error=f"inbox write failed: {exc}",
            ))
            continue
        try:
            instruction = dispatch_mod.format_dispatch_instruction(
                agent_id=agent_id, slug=t.slug,
                prompt_file=inbox_path,
            )
        except ValueError as exc:
            actions.append(_DispatchAction(
                slug=t.slug, agent_id=agent_id,
                error=f"dispatch instruction format failed: {exc}",
            ))
            continue
        actions.append(_DispatchAction(
            slug=t.slug, agent_id=agent_id, branch=worker_branch,
            worktree=worker_worktree,
            dispatch_instruction=instruction,
        ))
        active += 1
        in_flight_after_dispatch.append(t)
    return actions


def _dispatch_review_handoffs(
    *,
    tasks: list[parse.Task],
    project: str,
    fleet_bin: str,
    fleet_home: str,
    home: Path,
    coord_state: dict | None = None,
) -> list[_DispatchAction]:
    """Detect in-flight tasks whose worker dir signals a stage handoff
    and emit DISPATCH blocks for the next stage's subagent.

    Three-stage flow (reviewer-subagent-arch):

        worker (phase=review-pending) → reviewer subagent
        reviewer (phase=review-done)  → finisher subagent

    Trigger condition is per-task:
      1. task.status == "in-progress" (the slot is owned).
      2. workers/<slug>/state.json reports phase=review-pending OR
         phase=review-done.
      3. The previous subagent has exited — gated on (worker_pid == 0
         OR worker_pid not alive). Without this gate we'd race the
         outgoing worker that's still drawing breath; with it, the
         handoff fires exactly once on the tick AFTER the worker
         exited.

    Each handoff dispatches a NEW subagent (its own agent_id, its own
    inbox file). The worker's state.json stays at the same path —
    successive subagents own + mutate the same state file, the same
    way the worker progresses through phases.

    Suppression of double-dispatch: the loop checks the coord-state
    map's review_handoff_dispatched entry (per slug + phase) to avoid
    re-spawning a reviewer on every tick while the reviewer is still
    running. The check is "did we already dispatch a reviewer/finisher
    for this slug at this phase since the last terminal write?"
    A finisher's phase=done write resets the per-slug entry.

    BUT: the coord-state map mutation lives in `_apply_dispatch_handoff`
    (companion to `_apply_dispatch`). This function only PROPOSES.
    """
    actions: list[_DispatchAction] = []
    if coord_state is not None:
        raw = coord_state.get("review_handoffs_dispatched")
        seen_handoffs = (
            {str(x) for x in raw if isinstance(x, str)}
            if isinstance(raw, list)
            else set()
        )
    else:
        seen_handoffs = _load_review_handoff_state(home, project)
    # Git mode is the same per-tick decision as in _dispatch_ready —
    # the reviewer + finisher prompts branch on this.
    is_git = dispatch_mod.project_is_git(project, fleet_home=fleet_home)
    for t in tasks:
        if t.status != "in-progress":
            continue
        # If the previous subagent is still draining, hold off — the
        # state.json may be in mid-flux. The pid-alive check is the
        # fast path; the slower state-fresh path can lie when a
        # worker exits at phase=review-pending (still under the 15-
        # minute freshness window). We deliberately do NOT consult
        # `_is_worker_alive` here — that helper considers a fresh
        # state.json "alive" even when the OS process has exited,
        # which is exactly the case we want to detect here.
        if t.worker_pid > 0 and _pid_alive(t.worker_pid):
            continue
        st = _read_worker_state(project, t.slug, home=home)
        if st is None:
            continue
        phase = st.get("phase", "")
        if phase not in ("review-pending", "review-done"):
            continue

        # De-dup: don't re-spawn a reviewer/finisher while it's still
        # working. We key on (slug, phase) — once the next subagent
        # writes phase=review-claude (reviewer iterating) or
        # phase=push/done (finisher executing), the key changes and
        # the dispatch is naturally not retriggered.
        key = f"{t.slug}:{phase}"
        if key in seen_handoffs:
            continue

        branch = t.branch or f"worker/{t.slug}"
        try:
            if phase == "review-pending":
                prompt = dispatch_mod.build_reviewer_prompt(
                    t, project=project, branch=branch, is_git=is_git,
                )
                description = f"fleet reviewer {t.slug}"
            else:
                prompt = dispatch_mod.build_finisher_prompt(
                    t, project=project, branch=branch, is_git=is_git,
                )
                description = f"fleet finisher {t.slug}"
        except dispatch_mod.PromptTooLargeError as exc:
            actions.append(_DispatchAction(slug=t.slug, error=str(exc)))
            continue

        agent_id = dispatch_mod.mint_agent_id()
        try:
            inbox_path = dispatch_mod.write_worker_inbox(
                agent_id, prompt, fleet_home=fleet_home,
            )
        except Exception as exc:  # noqa: BLE001
            actions.append(_DispatchAction(
                slug=t.slug, agent_id=agent_id,
                error=f"handoff inbox write failed: {exc}",
            ))
            continue
        try:
            instruction = dispatch_mod.format_dispatch_instruction(
                agent_id=agent_id, slug=t.slug,
                prompt_file=inbox_path, description=description,
            )
        except ValueError as exc:
            actions.append(_DispatchAction(
                slug=t.slug, agent_id=agent_id,
                error=f"handoff instruction format failed: {exc}",
            ))
            continue
        # Tag the action so the apply step knows this is a handoff
        # (which skips status flip + state.json bootstrap — those are
        # already correct from the prior subagent).
        actions.append(_DispatchAction(
            slug=t.slug, agent_id=agent_id, branch=branch,
            dispatch_instruction=instruction,
            handoff_phase=phase,
        ))
    return actions


def _has_conflict_with_inflight(
    candidate: parse.Task, in_flight: list[parse.Task],
) -> bool:
    """Conservative conflict check used by the cap > 1 dispatch loop.

    Layers a "no labeled Files: line → matches anything" rule on top of
    conflict.has_conflict (which is intentionally optimistic when either
    side has no extracted paths). A worker whose task has no operator-
    declared file scope could touch ANY file — running it in parallel
    with another worker risks a clobber on overlapping writes that the
    heuristic can't see ahead of time.

    "Declared scope" is strictly the labeled-path lines (`Files:`,
    `path:`, `paths:`, `file:`) in Spec / Acceptance / Notes — NOT
    inline path mentions in prose. A task whose Spec reads "Investigate
    panic in cmd/fleet/main.go" is NOT considered scope-declared, even
    though `conflict.extract_paths` would surface that path token for
    overlap purposes; the operator never wrote a contract line, so we
    treat the scope as opaque and skip it conservatively.

    Decision tree (candidate vs each in-flight task):

      candidate has no labeled paths               → True  (could touch anything)
      in-flight task has no labeled paths          → True  (opaque scope on the other side)
      explicit overlap (conflict.has_conflict)     → True
      otherwise                                    → False (both sides declared + disjoint)

    Operators opt out of the conservative skip per task by adding an
    explicit `Files: <path-with-extension>` line. The heuristic regex
    requires a real file extension (so a bare token like `Files: *`
    won't satisfy the gate); the intended escape hatch is a real path.

    Self-conflict guard: a candidate may appear in in_flight if a caller
    passes a stale snapshot. conflict.has_conflict already filters
    same-slug pairs; the labeled-paths short-circuits below would
    otherwise flag a self-vs-self comparison, so we explicitly skip the
    candidate when scanning in_flight.
    """
    if not in_flight:
        return False
    cand_labeled = conflict.extract_labeled_paths(candidate)
    # Candidate has no declared scope → assume it matches anything that
    # is already running. Operator opts out via explicit Files: line.
    if not cand_labeled:
        for other in in_flight:
            if other.slug == candidate.slug:
                continue
            return True
        return False
    # Candidate declared scope. Walk the in-flight list: if any other
    # task has no labeled scope (opaque), or has overlapping paths,
    # it's a conflict. Overlap is computed against the FULL extracted
    # path set (labeled + inline) so an in-flight task whose prose
    # mentions a candidate-declared file still trips the gate — false
    # positives there cost one tick of serialization, false negatives
    # could clobber files.
    for other in in_flight:
        if other.slug == candidate.slug:
            continue
        if not conflict.extract_labeled_paths(other):
            return True
        if conflict.conflicts(candidate, other):
            return True
    return False


def _filter_ready(tasks: list[parse.Task]) -> list[parse.Task]:
    """Return tasks that are dispatchable now: status=ready AND deps satisfied.

    A dep is "satisfied" if it's done. Tasks blocked / abandoned /
    in-review / in-progress / done are skipped (only ready tasks
    dispatch). spawned_by is honored: worker-filed tasks (spawned_by !=
    "user") need operator promotion (status=ready, which the promote
    command sets) before they're dispatched. The promotion gate is on
    the `fleet tasks promote` side; here we just trust status.
    """
    by_slug = {t.slug: t for t in tasks}
    out: list[parse.Task] = []
    for t in tasks:
        if t.status != "ready":
            continue
        if not _deps_satisfied(t, by_slug):
            continue
        out.append(t)
    return out


def _deps_satisfied(t: parse.Task, by_slug: dict[str, parse.Task]) -> bool:
    for dep in t.depends_on:
        d = by_slug.get(dep)
        if d is None:
            return False
        if d.status != "done":
            return False
    return True


_PRIORITY_RANK = {"P0": 0, "P1": 1, "P2": 2, "P3": 3}


def _priority_sort_key(t: parse.Task) -> tuple[int, str]:
    """Sort by priority rank, then by created timestamp (older first).

    Slug as the secondary key ensures stable ordering for tests; the
    real ordering is priority + created. Created falls back to "" when
    None so the sort doesn't crash on partially populated tasks.
    """
    rank = _PRIORITY_RANK.get(t.priority, 99)
    created = t.created.isoformat() if t.created else ""
    return (rank, created)


def _apply_dispatch(action: _DispatchAction, project: str, fleet_bin: str) -> None:
    """Mark a task in-progress + pre-seed workers/<slug>/state.json so
    reconcile recognizes the dispatched worker as alive on its first
    pass.

    The agent ID is an 8-hex Fleet agent record key, not an OS PID;
    storing it on the task block requires a future schema bump (the
    in-flight `worker_pid` field on tasks.md still expects an OS PID
    per ENG §3.1). v0.2 dispatches workers via `fleet dispatch` (tmux
    + interactive Claude session); the worker drives the TDD pipeline
    itself and publishes progress via `fleet workers update`, which
    writes workers/<slug>/state.json. For the skill side, we write
    the agent_id as a NOTE so the operator can correlate the agent
    row with the task.

    Codex full-stack iter-1 + iter-2 [P1] regress: the OS pid in
    tasks.md.worker_pid is unreliable across coordinator ticks (the
    coord tick is short-lived; setting worker_pid=os.getpid() in this
    function makes the pid dead by the next tick). Instead, we
    pre-seed workers/<slug>/state.json with phase=starting before
    the worker subprocess has done anything; the reconcile loop's
    state.json freshness check is the canonical liveness signal and
    survives across ticks correctly.

    `--project <project>` is threaded into every mutation so the
    cwd-default project resolution can't misroute writes to a sibling
    project (see _apply_reconcile for the failure mode).
    """
    if action.error:
        return
    # Order matters for crash-recovery + duplicate-dispatch
    # avoidance:
    #  1. status flip FIRST. _dispatch_ready filters by
    #     status=ready, so the moment status flips to in-progress
    #     the task is no longer a candidate for re-dispatch.
    #     Codex iter-4 [P1]: an earlier attempt bootstrapped
    #     state.json before the status flip, which on a partial
    #     failure (status flip raises) left tasks.md still at
    #     ready and the worker's tmux session running — the next
    #     tick re-dispatched the same task and we ran two workers.
    #  2. branch second (so the operator's `fleet tasks show`
    #     lines up with the worker's actual checkout).
    #  3. workers/<slug>/state.json bootstrap third. After the
    #     status flip is durable, an updated_at-fresh state.json
    #     anchors reconcile's _is_worker_alive check on the very
    #     next tick — even if worker_pid is dead by then. A crash
    #     between status flip + bootstrap leaves reconcile with a
    #     1-tick window of "alive=False" and falls through to the
    #     pr_url branch (no PR yet → status=todo + clear_worker)
    #     which IS a duplicate-dispatch opportunity but only on
    #     two consecutive crashes, much narrower than the
    #     bootstrap-first race above.
    #  4. worker_pid sentinel fourth (legacy field; reconcile no
    #     longer trusts it across ticks, but `fleet status` still
    #     renders it).
    #  5. note last (informational; missing it is a graceful
    #     degradation, not state corruption).
    _run_fleet([fleet_bin, "tasks", "set", "--project", project, action.slug, "status=in-progress"])
    if action.branch:
        _run_fleet([fleet_bin, "tasks", "set", "--project", project, action.slug, f"branch={action.branch}"])
    if action.worktree:
        # Persist the worktree path so reconcile knows where to clean
        # up on terminal transition (done/abandoned). Single-worker
        # mode leaves this empty so existing behavior is unchanged.
        _run_fleet([fleet_bin, "tasks", "set", "--project", project, action.slug, f"worktree={action.worktree}"])
    _run_fleet([fleet_bin, "workers", "update", "--project", project, action.slug, "--phase", "starting"])
    _run_fleet([fleet_bin, "tasks", "set", "--project", project, action.slug, f"worker_pid={os.getpid()}"])
    _run_fleet([fleet_bin, "tasks", "note", "--project", project, action.slug, f"dispatched as agent {action.agent_id}"])


def _apply_dispatch_handoff(
    action: _DispatchAction, project: str, fleet_bin: str,
) -> None:
    """Apply a review-handoff dispatch (reviewer or finisher subagent).

    Mechanically simpler than _apply_dispatch:
      - Task status stays in-progress (the slot is already owned).
      - workers/<slug>/state.json stays at its current phase — the
        reviewer/finisher will flip it themselves.
      - Branch + worktree are unchanged (same slug, same checkout).
      - The ONLY persistent mutation is a note recording which
        subagent_id picked up this stage; the supervisor's agent_id
        map gets updated separately by the caller.

    Called from tick() after the DISPATCH block has been collected.
    Coord-state.review_handoffs_dispatched mutation happens in tick()
    too (it needs the same state dict that tick() persists at the end).
    """
    if action.error:
        return
    # Single fleet CLI call: record the new subagent_id under the
    # task's note history. The note's free text is the only persistent
    # breadcrumb the operator sees in `fleet tasks show` for the
    # reviewer/finisher slot.
    label = action.handoff_phase or "handoff"
    if action.agent_id:
        _run_fleet([
            fleet_bin, "tasks", "note", "--project", project, action.slug,
            f"{label}: dispatched as agent {action.agent_id}",
        ])


# ---------- shared CLI helper ----------


# ---------- auto-archive ----------


# Default tasks.md size threshold above which we shell `fleet tasks
# archive` for the oldest done/abandoned rows until count ≤ threshold.
# Tunable via FLEET_AUTO_ARCHIVE_THRESHOLD; setting 0 disables.
_AUTO_ARCHIVE_DEFAULT_THRESHOLD = 50


@dataclass
class _AutoArchiveResult:
    """Captures archive errors for surfacing on the tick result.

    archived: count of slugs successfully shelled to `fleet tasks archive`.
    errors:   any per-slug shell failure / parse error string.
    """

    archived: int = 0
    errors: list[str] = field(default_factory=list)


def _auto_archive_threshold() -> int:
    """Resolve the threshold from env. Returns -1 to mean "disabled".

    FLEET_AUTO_ARCHIVE_THRESHOLD == "0" → disabled.
    Empty / unset                       → _AUTO_ARCHIVE_DEFAULT_THRESHOLD.
    Non-integer                         → fall back to default.
    """
    raw = os.environ.get("FLEET_AUTO_ARCHIVE_THRESHOLD", "")
    if raw == "":
        return _AUTO_ARCHIVE_DEFAULT_THRESHOLD
    try:
        n = int(raw)
    except ValueError:
        return _AUTO_ARCHIVE_DEFAULT_THRESHOLD
    if n <= 0:
        return -1
    return n


def _maybe_auto_archive(
    tasks_path: Path, project: str, fleet_bin: str,
) -> _AutoArchiveResult | None:
    """Trim tasks.md when it grows past FLEET_AUTO_ARCHIVE_THRESHOLD.

    Picks the OLDEST done/abandoned slugs (sort: finished_at asc, fall
    back to updated asc) and shells them to `fleet tasks archive` one
    at a time until the row count is at or below the threshold. Active
    statuses are NEVER archived regardless of count.

    Returns None when the threshold is unmet or disabled. Returns a
    non-None result with `errors` populated if any per-slug archive
    shell failed; the tick adds those to result.errors.

    Idempotency: re-entering this function with tasks.md already at /
    below threshold is a no-op. The threshold check happens on a fresh
    parse so we react to the current on-disk state, not the stale
    in-memory snapshot the tick had earlier.
    """
    threshold = _auto_archive_threshold()
    if threshold <= 0:
        return None
    try:
        f = parse.read(str(tasks_path))
    except Exception as exc:  # noqa: BLE001
        return _AutoArchiveResult(errors=[f"auto-archive read: {exc}"])
    if len(f.tasks) <= threshold:
        return None

    # Build the candidate list: only done/abandoned. Sort oldest-first
    # so the oldest-done is archived first (matches the operator's
    # mental model of "trim the tail").
    candidates = [
        t for t in f.tasks
        if t.status in ("done", "abandoned")
    ]
    if not candidates:
        # 51+ active tasks. Surface a breadcrumb so the operator notices
        # the unbounded growth — but skip silently rather than failing
        # the tick.
        msg = (
            f"auto-archive: tasks.md has {len(f.tasks)} entries "
            f"(threshold={threshold}) but no done/abandoned candidates"
        )
        return _AutoArchiveResult(errors=[msg])

    # Sort: rank = coalesce(finished_at, updated, created), oldest
    # first. Mirrors the Go-side `finishedAtForSort` coalesce semantics
    # used in `fleet tasks list`. Old rows that never stamped
    # finished_at use their updated as the rank — so a task done
    # yesterday (with finished_at=yesterday) outranks a legacy row
    # (finished_at=None) whose updated is from a year ago.
    #
    # All datetimes are RFC3339 with timezone (parse.py rejects naive
    # datetimes by going through fromisoformat with the trailing 'Z'
    # normalized to '+00:00'); _SENTINEL_DT_MIN below stays naive only
    # to satisfy the rare missing-everything edge case (a row missing
    # both finished_at AND updated AND created — should not happen
    # post-parse, but defensive).
    def _sort_key(task) -> tuple:
        rank = task.finished_at or task.updated or task.created
        if rank is None:
            rank = _SENTINEL_DT_MIN
        # Created is the tie-breaker; same coalesce treatment.
        ca = task.created or _SENTINEL_DT_MIN
        return (rank, ca)

    candidates.sort(key=_sort_key)

    # Archive enough rows to fall to or below threshold. Each shell
    # serializes through tasks.Archive's own state-lock — fine, we're
    # NOT holding tasks-archive.lock here, just coordinator.lock.
    surplus = len(f.tasks) - threshold
    to_archive = candidates[:surplus]
    out = _AutoArchiveResult()
    for t in to_archive:
        try:
            _run_fleet(
                [fleet_bin, "tasks", "archive", "--project", project, t.slug],
                timeout_s=30.0,
            )
            out.archived += 1
        except Exception as exc:  # noqa: BLE001
            out.errors.append(f"auto-archive {t.slug}: {exc}")
    return out


# Sentinel datetime "before any real timestamp" used by the
# auto-archive sort when EVERY ranking key is missing on a task (a
# row with no finished_at, no updated, AND no created). Should never
# happen post-parse — required-bullet validation forces created/updated
# — but kept defensive so a malformed row doesn't crash the sort.
#
# datetime.min is naive (no tzinfo); compared against tz-aware values
# it would TypeError. We never expect to hit this branch in practice
# because a real row always has at least `created`, but the cheap
# fallback isolates the edge case from the hot path.
_SENTINEL_DT_MIN = __import__("datetime").datetime.min


def _run_fleet(cmd: list[str], timeout_s: float = 30.0) -> None:
    """Run a fleet CLI command. Raises on failure with cmd + stderr.

    Skill-side mutations are limited to Go-CLI shells, so any failure
    here is an operator-visible problem (the file's locked, the binary
    missing, etc.). Caller wraps in try/except and accumulates into
    TickResult.errors.
    """
    proc = subprocess.run(
        cmd, capture_output=True, text=True, timeout=timeout_s, check=False,
    )
    if proc.returncode != 0:
        msg = (proc.stderr or proc.stdout or "").strip()
        raise RuntimeError(f"{' '.join(cmd)}: {msg or f'exit {proc.returncode}'}")


# ---------- entry point for SKILL.md invocation ----------


def main(argv: Iterable[str] | None = None) -> int:
    """Skill entry point. Reads project from FLEET_PROJECT env or argv.

    Always exits 0 — failures are recorded in the result and surfaced
    to the operator via the agent's blocked_reason; the hook itself
    must not block the agent's turn (matches fleet-guard discipline).
    """
    argv = list(argv) if argv is not None else []
    project = os.environ.get("FLEET_PROJECT", "")
    if argv:
        project = argv[0]
    if not project:
        print("coordinator: no project set (FLEET_PROJECT or argv[0])")
        return 0
    result = tick(project)
    # Issue #84 Phase A: emit DISPATCH blocks BEFORE the JSON summary
    # so the coord agent (Claude) sees them as parseable plain text in
    # the tick output. Each block tells Claude to invoke the Agent
    # tool with run_in_background=true once per block — see SKILL.md
    # "Worker dispatch protocol". Blocks are separated by a blank
    # line so the parser (Claude reasoning over the stdout) can pick
    # them out of multi-block ticks.
    for block in result.dispatch_instructions:
        print(block)
        print()
    print(json.dumps({
        "skipped": result.skipped,
        "reason": result.reason,
        "parsed": result.parsed_tasks,
        "reconciled": result.reconciled,
        "drained": result.drained,
        "dispatched": result.dispatched,
        "raised": result.raised,
        "errors": result.errors,
    }))
    return 0


if __name__ == "__main__":
    import sys
    sys.exit(main(sys.argv[1:]))
