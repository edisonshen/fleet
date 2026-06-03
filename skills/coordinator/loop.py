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
import sys
import time
from dataclasses import dataclass, field
from pathlib import Path
from typing import Iterable

import conflict
import coord_config
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
#
# DESIGN-coord-worktree-lifecycle §3 (sentinel-path readers S1-S5): a
# STATE-MUTATING sentinel (task_done_pr, worker_failed, blocked_question)
# carries the worker's dispatch_generation token so the coord can
# corroborate it against the slug's current task-row authority and skip
# ALL terminal side effects on a stale (prior-attempt) sentinel. The
# token is an OPTIONAL `gen=<n>` clause placed immediately after the slug
# (additive: a pre-migration worker that omits it parses as gen 0, the
# tokenless-legacy path). A pure `new_task` wake carries no state
# mutation and stays token-free. The clause sits BEFORE the payload so
# the greedy `.+`/`\S+` payload captures can't swallow it.
_SENTINEL_PATTERNS = {
    "task_done_pr": re.compile(
        r"^TASK_DONE_PR\s*=?\s*(?P<slug>[a-z0-9._-]+)"
        r"(?:\s+gen=(?P<gen>[0-9]+))?\s+(?P<url>\S+)\s*$",
    ),
    "blocked_question": re.compile(
        r"^BLOCKED_QUESTION\s*=?\s*(?P<slug>[a-z0-9._-]+)"
        r"(?:\s+gen=(?P<gen>[0-9]+))?\s+(?P<text>.+)$",
    ),
    "worker_failed": re.compile(
        r"^WORKER_FAILED\s*=?\s*(?P<slug>[a-z0-9._-]+)"
        r"(?:\s+gen=(?P<gen>[0-9]+))?\s+(?P<reason>.+)$",
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

    self_exit=True (reason="duplicate-coord-self-exit") means this
    coord lost the coordinator.lock to a DIFFERENT live coord and is
    NOT the project's intended coord (per the coord-spawn-marker) — it
    is a duplicate session that would otherwise idle forever holding a
    tmux session and doing no work (coord-self-exit-when-it-6014). The
    tick stays side-effect-free; main() reads this flag and tears down
    THIS coord's own tmux session so the duplicate self-heals to one.

    dispatch_instructions carries the rendered DISPATCH blocks (issue
    #84 Phase A) that the coord agent (Claude) is expected to act on
    by invoking the Agent tool once per block on its NEXT assistant
    turn. SKILL.md's "Worker dispatch protocol" section pins the
    contract.
    """

    skipped: bool = False
    reason: str = ""
    # coord-self-exit-when-it-6014: set True when the lock-busy branch
    # detects THIS session is a duplicate coord (a different live coord
    # holds the lock + we are not the project's intended coord). main()
    # acts on it by killing this coord's own tmux session.
    self_exit: bool = False
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

    # 0. Project-ownership guard (fleet#171). The authoritative project
    # owner for a coord agent is its agent record at
    # ~/.fleet/agents/<id>.json — NOT cwd, NOT argv. If a caller hands
    # us a project that doesn't match the agent record's `project`
    # field, refuse before we touch the on-disk lock file. Without
    # this, `python3 loop.py X` invoked from inside coord-for-Y's
    # Claude session silently hijacks project X's coord lock and
    # writes coord-for-Y's agent_id into it (observed live with agent
    # 922e7c7d, projects-spark coord, hijacking fleet's lock).
    #
    # Edge cases:
    #   - empty coord_id (operator-shell invocation, no agent context)
    #     → allow. Backwards-compat for manual `loop.py X` diagnostics.
    #   - missing or unparseable agent record → warn + allow. Legacy
    #     pre-schema records must not block upgrades.
    #   - empty rec.project field → allow. Same legacy reason.
    #   - mismatch → populate skipped/reason/errors + stderr; return
    #     BEFORE _try_lock. We deliberately do NOT sys.exit(2) — the
    #     fleet-guard discipline pinned in loop.main is "always exit
    #     0, surface via TickResult". Stderr write covers manual shell
    #     invocations where the caller doesn't inspect TickResult.
    if coord_id:
        rec_path = home / "agents" / f"{coord_id}.json"
        rec: dict | None = None
        rec_error: str = ""
        try:
            with open(rec_path, "r", encoding="utf-8") as fh:
                rec = json.load(fh)
            if not isinstance(rec, dict):
                rec = None
                rec_error = "agent record is not a JSON object"
        except FileNotFoundError:
            rec_error = "no agent record on disk"
        except (OSError, ValueError) as exc:
            rec_error = f"agent record unreadable: {exc}"
        if rec is not None:
            rec_project = rec.get("project", "") or ""
            if rec_project and rec_project != project:
                import sys as _sys
                msg = (
                    f"coord agent {coord_id} owns project "
                    f"{rec_project!r}, refusing to operate on "
                    f"{project!r} (cwd or argv mismatch — investigate "
                    f"spawn env / cwd / argv chain)"
                )
                _sys.stderr.write(msg + "\n")
                result.errors.append(msg)
                result.skipped = True
                result.reason = "project-ownership-mismatch"
                return result
        elif rec_error:
            # Warn to stderr so an operator running `loop.py X`
            # manually sees it, but DO NOT push into result.errors —
            # that surface is consumed by callers (TUI, `fleet
            # status`) that treat any entry as a real tick error.
            # Legacy/upgrade paths with missing records are common
            # enough that polluting result.errors here would create
            # permanent false alarms in the dashboard.
            import sys as _sys
            _sys.stderr.write(
                f"project-ownership guard: agent {coord_id} — "
                f"{rec_error}; allowing tick (legacy/upgrade path)\n"
            )

    # 1. NB-flock coordinator.lock (PLAN §6 lock acquisition).
    project_dir = home / "projects" / project
    locks_dir = project_dir / ".locks"
    locks_dir.mkdir(parents=True, exist_ok=True)
    lock_path = locks_dir / "coordinator.lock"
    lock_fd = _try_lock(lock_path, holder_id=coord_id)
    if lock_fd is None:
        # coord-self-exit-when-it-6014: the lock is held by SOMEONE.
        # The historical behavior was "skip the tick, exit cleanly" —
        # but the *claude session* lived on idle forever, leaving a
        # zombie coord that holds a tmux session and does no work. That
        # is why [h]/[a] mishaps leave multiple live coords instead of
        # self-healing to one.
        #
        # Decide whether THIS session is a genuine duplicate that
        # should tear itself down. We must be conservative — the lock
        # HOLDER must never self-exit, and a mid-handoff / intended
        # successor coord must never self-exit either.
        #
        #   lock body holder == ""            -> can't prove duplicate; skip only
        #   holder == coord_id                -> our own stale flock;   skip only
        #   spawn-marker == coord_id          -> WE are the intended    skip only
        #                                        coord (mid-handoff /
        #                                        successor); never exit
        #   holder != me AND holder is LIVE   -> DUPLICATE: self-exit
        #   holder dead / stale               -> normal takeover;       skip only
        result.skipped = True
        result.reason = "lock-busy"
        decision, diag = _classify_lock_busy(
            lock_path, project_dir, coord_id, home,
        )
        if decision == "duplicate":
            result.self_exit = True
            result.reason = "duplicate-coord-self-exit"
            # surface_dont_silo: a self-exit must never be silent. The
            # operator (and any shell-invoked `loop.py`) sees exactly
            # why this session is going away and how to confirm.
            sys.stderr.write(diag + "\n")
            result.errors.append(diag)
        return result
    # 1.5. Resolve worktree-base repo (fleet#175).
    #
    # Sources, in order of authority:
    #
    #   1. meta.json::repo_path — set by `fleet project add`.
    #      OPERATOR-AUTHORITATIVE. When present, wins outright;
    #      URL heuristic is bypassed entirely. Custom-named clones,
    #      forks, vanity URLs all work — operator's explicit
    #      registration overrides any heuristic ambiguity.
    #
    #   2. coord-config.json::repo — set by Spawn from cwd at coord-
    #      spawn time. Authoritative for projects NOT registered via
    #      `fleet project add` (legacy / direct-spawn flows). URL
    #      heuristic is the ONLY safety check here, so mismatch
    #      REFUSES dispatch (custom-name operators register via
    #      `fleet project add` to bypass; #175 corruption is
    #      prevented).
    #
    #   3. caller cwd / os.getcwd() — legacy fallback (pre-fleet#175
    #      installs). Warning surfaced.
    #
    # ASCII (iter-19 final):
    #
    #   meta.json::repo_path present?
    #     yes:
    #       path missing on disk? → refuse (meta-repo-missing)
    #       differs from coord-config.json::repo? → use meta.json +
    #                                                divergence warning
    #       matches (or coord-config absent)? → use meta.json silently
    #     no:
    #       coord-config.json::repo present?
    #         path missing? → refuse (coord-config-repo-missing)
    #         path not a git work tree? → refuse (coord-config-repo-not-git)
    #         use as cwd; URL heuristic check:
    #           empty origin (real git repo) → soft warning
    #           heuristic mismatch → soft warning + recovery hint
    #           match → silent
    #       no → fallback to caller cwd + warning
    #
    # Heuristic semantics: soft signal only. Strict safety lives at
    # the meta.json tier (operators wanting refuse-on-mismatch
    # register via `fleet project add`). Codex flipped 5+ times on
    # the heuristic refuse-vs-warn question — final call per
    # feedback_coord_makes_engineering_calls + the operator's
    # stated rubrics.
    #
    # Lives between lock acquire and _tick_locked so the refuse-paths
    # release the lock cleanly via the local fcntl.flock_un dance.
    cfg_path = project_dir / COORD_CONFIG_FILE
    configured_repo = coord_config.read_repo(cfg_path)
    meta_repo = coord_config.read_project_repo_path(project_dir)

    def _refuse_stale(source_name: str, repo_path: str, reason_code: str) -> "TickResult":
        """Helper: refuse dispatch with a stale-path error message
        and release the coord-spawn lock cleanly."""
        msg = (
            f"{source_name}={repo_path!r} for project {project!r} "
            f"is not a directory (deleted, moved, or path typo) — "
            f"refusing dispatch. Fix the source or re-spawn the coord "
            f"from inside the correct checkout."
        )
        import sys as _sys
        _sys.stderr.write(msg + "\n")
        result.errors.append(msg)
        result.skipped = True
        result.reason = reason_code
        result.raised += 1
        try:
            fcntl.flock(lock_fd, fcntl.LOCK_UN)
        finally:
            os.close(lock_fd)
        return result

    if meta_repo:
        # OPERATOR-AUTHORITATIVE source. meta.json wins over everything.
        if not os.path.isdir(meta_repo):
            return _refuse_stale(
                "meta.json::repo_path", meta_repo, "meta-repo-missing",
            )
        # If coord-config.json::repo disagrees, the coord was likely
        # spawned from the wrong cwd (the #175 bug). Surface a clear
        # warning + override with meta.json's authoritative value.
        if configured_repo and configured_repo != meta_repo:
            result.errors.append(
                f"coord-config.json::repo={configured_repo!r} differs "
                f"from meta.json::repo_path={meta_repo!r} for project "
                f"{project!r} — using meta.json (operator-authoritative "
                f"via `fleet project add`). Re-spawn the coord from "
                f"{meta_repo!r} to silence this warning."
            )
        cwd = meta_repo
    elif configured_repo:
        # No meta.json — fall back to spawn-time pin. URL heuristic
        # becomes the only sanity check; soft warning on mismatch
        # because we lack an authoritative source to override.
        if not os.path.isdir(configured_repo):
            return _refuse_stale(
                "coord-config.json::repo",
                configured_repo,
                "coord-config-repo-missing",
            )
        remote = coord_config.git_remote_origin(configured_repo)
        if not remote:
            # No origin URL. Two possibilities:
            #   (a) Real git repo with no `origin` remote (local-only,
            #       legit project shape).
            #   (b) NOT a git work tree at all (e.g., $HOME, /tmp,
            #       arbitrary dir).
            #
            # iter-17 (codex P1): distinguish via the existence of
            # `<repo>/.git` (regular repo dir, or worktree git
            # pointer file). If neither exists, refuse — accepting
            # an arbitrary non-git directory as cwd defeats the #175
            # safety goal (coord dispatched from $HOME could spawn
            # workers anywhere).
            if not os.path.exists(os.path.join(configured_repo, ".git")):
                msg = (
                    f"coord-config.json::repo={configured_repo!r} for "
                    f"project {project!r} is not a git work tree (no "
                    f".git directory or worktree pointer found). "
                    f"Refusing dispatch to prevent worker creation in "
                    f"a non-git directory. Re-spawn the coord from "
                    f"inside the project's git checkout."
                )
                import sys as _sys
                _sys.stderr.write(msg + "\n")
                result.errors.append(msg)
                result.skipped = True
                result.reason = "coord-config-repo-not-git"
                result.raised += 1
                try:
                    fcntl.flock(lock_fd, fcntl.LOCK_UN)
                finally:
                    os.close(lock_fd)
                return result
            # Real git repo with no origin — local-only is supported.
            result.errors.append(
                f"coord-config.json::repo={configured_repo!r} for "
                f"project {project!r} has no `origin` remote — "
                f"skipping repo↔project validation. (Local-only "
                f"checkouts are supported; set an origin remote OR "
                f"run `fleet project add {configured_repo}` to "
                f"register an authoritative path.)"
            )
        elif not coord_config.remote_matches_project(remote, project):
            # Mismatch in the no-meta.json branch is a SOFT WARNING.
            #
            # Iter-history on this question: codex flipped between
            # refuse-on-mismatch (iter-4/6/8/11 P1, "prevents #175
            # corruption") and warn-only (iter-2/3/16/18 P1, "refuse
            # breaks legitimate custom-aliased projects and
            # custom-named clones that have no working `fleet project
            # add` recovery path"). Both are real concerns.
            #
            # Final call (iter-19, codex contradiction resolved per
            # CLAUDE.md §4 + feedback_coord_makes_engineering_calls):
            # warn-but-proceed. The configured `repo` field is what
            # the operator stamped at coord-spawn time; refusing to
            # honor it on a fuzzy name-match would defeat the
            # operator's explicit choice. The heuristic remains as
            # a SIGNAL — operator sees the warning and can investigate
            # — but it is NOT a gate.
            #
            # Strict safety (refuse on mismatch) lives at the
            # meta.json tier: operators wanting the strict check
            # run `fleet project add <path>` and meta.json wins over
            # coord-config + the heuristic doesn't apply. This is the
            # documented "use meta.json for safety" tiered authority
            # introduced in iter-7.
            #
            # Real risk acknowledged: legacy/no-meta.json projects with
            # a wrong coord-config.json::repo silently dispatch from
            # the wrong checkout. Recovery: warning in TickResult.errors
            # surfaces the signal; operator can detect via tick output
            # and run `fleet project add` to register meta.json (which
            # then takes over and refuses if the configured repo is
            # truly wrong).
            result.errors.append(
                f"coord-config.json::repo={configured_repo!r} "
                f"(origin={remote!r}) does not match the heuristic "
                f"for project {project!r}. Proceeding because the "
                f"`repo` field is the operator-stamped spawn-time "
                f"value; if the configured checkout is wrong, fix "
                f"coord-config.json or re-spawn from the correct "
                f"directory. For strict validation, register via "
                f"`fleet project add {configured_repo}` to write "
                f"meta.json::repo_path (the authoritative tier)."
            )
        cwd = configured_repo
    else:
        # Neither meta.json nor coord-config.json::repo — pre-#175
        # install. Fall through to legacy caller-cwd behavior with a
        # warning so the operator knows to re-spawn.
        result.errors.append(
            f"coord-config.json::repo not set for {project!r}; "
            f"falling back to caller cwd={cwd!r}. Re-spawn the coord "
            f"from inside the project's git checkout, or set the field "
            f"manually."
        )
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
    reconciled = _reconcile_inflight(
        f.tasks, project, fleet_bin, home=home, coord_state=state,
    )
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
                # PR1 dispatch-lifecycle: release the coord_prompt_inbox
                # claim BEFORE forget_agent_id so we still have the
                # mapping at release time. Best-effort; non-success
                # logs to stderr but never blocks the status flip.
                # Codex iter-7 [P1]: only forget the mapping on
                # terminal release outcomes — a transient `error`
                # keeps the mapping so the next sweep/reconcile can
                # retry the release. _clear_review_handoff_state runs
                # unconditionally because its keys are tracked by
                # slug (not agent_id) and are stale either way.
                terminal_agent_id = supervisor_mod.load_agent_id_map(
                    state,
                ).get(action.slug, "")
                release_outcome = _release_coord_prompt_inbox(
                    slug=action.slug,
                    agent_id=terminal_agent_id,
                    fleet_bin=fleet_bin,
                    fleet_home=home,
                    site=f"primary-reconcile new_status={action.new_status}",
                )
                if _release_outcome_is_terminal(release_outcome):
                    supervisor_mod.forget_agent_id(state, action.slug)
                elif terminal_agent_id:
                    # Codex iter-10 [P1]: non-done terminal transitions
                    # (todo / blocked) leave the slug out of the in-
                    # flight set; subsequent ticks won't reach this
                    # release wire again. Stash the id in
                    # pending_release_agent_ids so the retry-pending-
                    # releases pass can re-attempt until terminal.
                    supervisor_mod.remember_pending_release_agent_id(
                        state, action.slug, terminal_agent_id,
                    )
                _clear_review_handoff_state(state, action.slug)
                # Codex iter-2 [P1]: do NOT clear reaper_redispatch_pending
                # here. The marker is the explicit signal to dispatch a
                # replacement on the NEXT pass (consumed by the
                # _promote_redispatch_pending hook below). Clearing it
                # before dispatch sees it means a failed worker just
                # lands in `todo` and never gets the promised replacement.
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
            coord_state=state,
        )
        result.reconciled += swept
    except Exception as exc:  # noqa: BLE001
        result.errors.append(f"sweep done worker dirs: {exc}")

    # 3.55. Codex iter-10 [P2]: sweep claim state for slugs whose
    # status was manually flipped to a non-in-flight non-done state
    # (todo / blocked / abandoned / etc). The reconcile path only
    # sees in-progress / in-review, and _sweep_done_worker_dirs only
    # handles done — without this sweep, an operator's manual
    # `fleet tasks set <slug> status=todo` leaves the journal/inbox
    # orphaned.
    try:
        _sweep_non_inflight_claim_state(
            f.tasks, project, fleet_bin, home=home,
            coord_state=state,
        )
    except Exception as exc:  # noqa: BLE001
        result.errors.append(f"sweep non-inflight claims: {exc}")

    # 3.6. Codex iter-9 [P1]: retry releases that failed transiently
    # on prior ticks (e.g., handoff release while `fleet claims
    # release` returned an `error` outcome, or any release that
    # couldn't drop the agent_id mapping because the outcome wasn't
    # terminal). Each entry is an agent_id whose journal/inbox may
    # still be live; retry until success or until PR4 sweeper picks
    # it up.
    try:
        _retry_pending_releases(
            project=project, fleet_bin=fleet_bin, home=home,
            coord_state=state,
        )
    except Exception as exc:  # noqa: BLE001
        result.errors.append(f"retry pending releases: {exc}")

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
    # Codex iter-5 [P1]: stash deferred sentinels in coord-state so the
    # next tick can replay them after the reaper lane clears. Watermark
    # advances normally — the deferred sentinels are queued separately
    # by source_file so the file itself can be safely "consumed" while
    # the action still needs to land.
    deferred_actions: list[_SentinelAction] = []
    # Process any sentinels that were deferred on a PRIOR tick first.
    # Prepend them to `drained` so they're attempted before this tick's
    # fresh batch (FIFO ordering for fairness).
    prior_deferred = _load_deferred_sentinels(state)
    drained = prior_deferred + drained
    for action in drained:
        # Codex iter-3 [P1]: gate worker-clearing sentinels on the
        # reaper lane, matching reconcile. TASK_DONE_PR / WORKER_FAILED
        # both forget the agent_id mapping; running them before the
        # reaper finishes its kill cycle would leave the tmux session
        # un-addressable on the next reap pass. BLOCKED_QUESTION keeps
        # the worker alive (operator decides), so it's not gated.
        # NEW_TASK doesn't touch the in-flight slot at all.
        if action.kind in ("task_done_pr", "worker_failed") and not (
            _reaper_lane_clear_for(state, action.slug)
        ):
            deferred_actions.append(action)
            continue
        try:
            sentinel_outcome = _apply_sentinel(
                action, project, fleet_bin,
                repo=sentinel_repo,
                tasks_by_slug=sentinel_tasks_by_slug,
                home=home,
                full_tasks_by_slug=tasks_by_slug,
            )
            # DESIGN §3 S4: a STALE sentinel performed NO terminal side
            # effects — consume it (no release/forget/handoff-clear, no
            # re-defer). Only `applied` drives the in-flight teardown.
            if sentinel_outcome == SENTINEL_SKIPPED_STALE:
                continue
            result.drained += 1
            if action.raised_to_user:
                result.raised += 1
            # Worker is leaving the in-flight set on TASK_DONE_PR
            # (status → in-review with closed worker) and WORKER_FAILED
            # (status → todo, worker cleared). BLOCKED_QUESTION keeps
            # the worker ALIVE (status → blocked) so the operator can
            # answer via the worker's inbox — codex iter-22 [P1]: we
            # must NOT forget the agent_id in that case; the supervisor
            # / TUI / manual `fleet rm` all need it as the addressing
            # handle.
            if action.kind in ("task_done_pr", "worker_failed"):
                # PR1 dispatch-lifecycle: release the coord_prompt_inbox
                # BEFORE forget_agent_id (same ordering rule as the
                # reconcile site). BLOCKED_QUESTION deliberately skipped
                # — the operator may write back via answer-blocked.
                # Codex iter-7 [P1]: gate forget on the outcome so
                # transient release errors don't drop the mapping.
                terminal_agent_id = supervisor_mod.load_agent_id_map(
                    state,
                ).get(action.slug, "")
                release_outcome = _release_coord_prompt_inbox(
                    slug=action.slug,
                    agent_id=terminal_agent_id,
                    fleet_bin=fleet_bin,
                    fleet_home=home,
                    site=f"primary-sentinel kind={action.kind}",
                )
                if _release_outcome_is_terminal(release_outcome):
                    supervisor_mod.forget_agent_id(state, action.slug)
                elif terminal_agent_id:
                    # Codex iter-10 [P1]: stash for retry — see
                    # primary-reconcile wire for rationale.
                    supervisor_mod.remember_pending_release_agent_id(
                        state, action.slug, terminal_agent_id,
                    )
                _clear_review_handoff_state(state, action.slug)
        except Exception as exc:
            # Codex iter-18 [P2]: re-add the action to the deferred
            # queue so a later pass retries. Otherwise a transient
            # `fleet tasks set/note` failure would permanently drop
            # the transition (the archive file is already past the
            # watermark).
            deferred_actions.append(action)
            result.errors.append(
                f"sentinel {action.slug}: {exc} (re-queued for retry)"
            )
    # Persist the deferred queue (or clear if nothing deferred this tick).
    _save_deferred_sentinels(state, deferred_actions)
    if last_seen:
        state["last_archive_scan_ts"] = last_seen

    # 4.5. Consume reaper_redispatch_pending markers (codex iter-2 [P1]).
    # The reaper flagged slugs whose worker hit phase=failed (judgment=
    # error-abort) for re-dispatch. Reconcile flipped those slugs to
    # `todo`; _filter_ready only picks `ready`. Promote pending slugs
    # from todo → ready so the next _dispatch_ready call sees them.
    # The marker is consumed (cleared) on a successful promote; a slug
    # that's already not in todo state (operator intervened, status
    # diverged) gets its marker dropped without action.
    try:
        _consume_reaper_redispatch(
            project=project, fleet_bin=fleet_bin, home=home,
            coord_state=state, tasks_path=tasks_path,
        )
    except Exception as exc:  # noqa: BLE001
        result.errors.append(f"redispatch promote: {exc}")

    # 5. Dispatch ready tasks under cap.
    # Re-read tasks.md after reconcile/drain so the dispatch-side filter
    # sees the latest in-progress count (mutations went through the
    # fleet CLI — they're durable on disk, we reload).
    try:
        f = parse.read(str(tasks_path))
    except Exception as exc:
        result.errors.append(f"tasks.md re-read failed: {exc}")
        # Codex iter-19 [P1]: persist coord-state before bailing.
        # _reap_inflight may have opened a reaper lane and the drain
        # step may have queued deferred sentinels. Dropping those on
        # a transient parse error would erase the lane gate and let
        # the next tick clear worker_agent_ids before the kill cycle
        # finishes — exactly the orphan-tmux shape invariant 5 exists
        # to prevent. Best-effort save; swallow errors (the next tick
        # re-derives state from disk).
        try:
            _save_coord_state(state_path, state)
        except Exception as save_exc:  # noqa: BLE001
            result.errors.append(
                f"coord-state save on parse-error path failed: {save_exc}"
            )
        return result

    # 5.0. dispatch-durability (#184) — tick-entry REPLAY reconcile.
    # Re-emit DISPATCH blocks for journal entries recorded ExecPending
    # (coord wrote inbox+journal but the launch block never reached the
    # coord — the broken-stdout phantom), and residual-crash-repair any
    # ExecLaunchAttempted that never acked. Runs BEFORE new dispatch so a
    # replayed launch claims the at-most-one-block-per-id-per-tick slot.
    #
    # `emitted_this_tick` is the ONE shared at-most-one-block-per-id set
    # across ALL emitters — replay + _dispatch_ready + the primary/
    # supervisor handoff emitters. The flock CAS (mark-launch-attempted
    # <gen>) is the final backstop, but the shared set avoids emitting two
    # blocks for one id in the first place. Keyed by agent_id.
    emitted_this_tick: set[str] = set()
    try:
        replays = _replay_pending_dispatches(
            project=project,
            home=home,
            fleet_bin=fleet_bin,
            fleet_home=str(home),
            coord_state=state,
            now_unix=now_unix,
        )
    except Exception as exc:  # noqa: BLE001 — replay must never wedge a tick
        replays = []
        result.errors.append(f"replay reconcile: {exc}")
    for rp in replays:
        if rp.raise_msg:
            # Off-channel escalation (NOT the stdout DISPATCH stream): the
            # whole point of replay is that stdout may be broken. Goes to
            # stderr + the tick errors list + raised counter.
            sys.stderr.write(f"coordinator: {rp.raise_msg}\n")
            result.errors.append(rp.raise_msg)
            result.raised += 1
            continue
        if rp.error:
            result.errors.append(f"replay {rp.slug or rp.agent_id}: {rp.error}")
            continue
        if rp.dispatch_instruction and rp.agent_id not in emitted_this_tick:
            result.dispatch_instructions.append(rp.dispatch_instruction)
            emitted_this_tick.add(rp.agent_id)
            result.dispatched += 1
            # A replayed id owns its slot this tick — drop any
            # pending_acquire entry so _dispatch_ready doesn't also emit
            # for the same slug (one owner per id per tick).
            if rp.slug:
                supervisor_mod.forget_pending_acquire_agent_id(state, rp.slug)

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
        # Codex iter-7 [P1]: persist agent_id BEFORE handoff apply.
        # _apply_dispatch_handoff has its own mid-apply failure modes
        # (note write, status mutations); upfront persistence keeps
        # the orphan-claim recovery path viable.
        if action.agent_id:
            supervisor_mod.remember_agent_id(state, action.slug, action.agent_id)
        try:
            _apply_dispatch_handoff(action, project, fleet_bin)
            if action.agent_id:
                # Codex iter-8 [P1]: record the review-handoff marker
                # ONLY after apply succeeds. The marker (slug:phase)
                # suppresses re-dispatch on subsequent ticks; if apply
                # fails before we drop it, the next tick sees the
                # marker as already-dispatched and never retries —
                # the task stays stuck at review-pending / review-done
                # with no replacement subagent.
                _record_review_handoff_dispatched(
                    state, action.slug, action.handoff_phase,
                )
                # Codex iter-5 [P1]: forget the pending-acquire entry
                # AFTER _apply_dispatch_handoff lands. Symmetric to
                # the primary tick + supervisor consumers.
                supervisor_mod.forget_pending_acquire_agent_id(
                    state, action.slug,
                )
            # #184: at-most-one-block-per-id-per-tick across replay +
            # handoff + dispatch (a handoff-apply failure can leave an
            # ExecPending handoff id that BOTH replay and this path would
            # emit). Skip if replay already emitted this id this tick.
            if (
                action.dispatch_instruction
                and action.agent_id not in emitted_this_tick
            ):
                result.dispatch_instructions.append(action.dispatch_instruction)
                if action.agent_id:
                    emitted_this_tick.add(action.agent_id)
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
        coord_state=state,
    )
    for action in dispatched:
        # Codex iter-4 [P1]: an acquire-prompt failure populates
        # action.error AND, prior to this gate, also leaked a freshly-
        # minted agent_id into the success path because the caller
        # checked only `if action.agent_id`. _apply_dispatch returns
        # silently on action.error, but the subsequent
        # remember_agent_id + result.dispatched += 1 would still fire
        # — falsely recording a dispatch the runtime never actually
        # made. Surface the error and skip, mirroring the handoff
        # loop's pre-existing gate.
        if action.error:
            result.errors.append(f"dispatch {action.slug}: {action.error}")
            continue
        # Codex iter-7 [P1]: persist slug → agent_id BEFORE
        # _apply_dispatch runs. _apply_dispatch fires several CLI
        # mutations after the claim has already been acquired (status
        # flip, branch set, workers update, worker_pid, note). If any
        # mid-apply step fails after `status=in-progress` lands, the
        # task is no longer `ready` so the pending-acquire retry
        # never fires; without the agent_id in coord_state, the
        # subsequent reconcile/sweep can't release the orphaned
        # claim. Persisting upfront ensures the release wires always
        # have a handle. Save state once after the loop (heartbeat)
        # so a partial apply still durably records the mapping.
        if action.agent_id:
            supervisor_mod.remember_agent_id(state, action.slug, action.agent_id)
        try:
            _apply_dispatch(action, project, fleet_bin)
            # Codex iter-5 [P1]: forget the pending-acquire entry only
            # AFTER the apply chain has landed. Doing it inside
            # _dispatch_ready (right after a successful acquire-prompt)
            # would leak the claim on any apply-stage failure: the task
            # stays `ready`, next tick mints a fresh id, and the
            # original journal/inbox sit orphaned.
            if action.agent_id:
                supervisor_mod.forget_pending_acquire_agent_id(
                    state, action.slug,
                )
            # Issue #84 Phase A: surface the DISPATCH block so the
            # coord agent (Claude) can invoke the Agent tool. We
            # collect AFTER _apply_dispatch — the status flip /
            # state.json bootstrap MUST land on disk before the
            # coord spawns the subagent, so a worker that races to
            # `fleet workers update` finds an in-progress task.
            #
            # #184: at-most-one-block-per-id-per-tick — skip if replay (or
            # the handoff path) already emitted a block for this id.
            if (
                action.dispatch_instruction
                and action.agent_id not in emitted_this_tick
            ):
                result.dispatch_instructions.append(action.dispatch_instruction)
                if action.agent_id:
                    emitted_this_tick.add(action.agent_id)
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

    # 5.9. Rolling coord checkpoint (#187), part 1 — bump the counter.
    # We bump the per-coord tick counter HERE (so it rides the heartbeat
    # _save_coord_state below in a single write) but DEFER the actual
    # coord-checkpoint.md file write to the END of the tick, after the
    # supervisor loop. Rationale: the supervisor can reconcile + dispatch
    # NEW workers mid-tick (_maybe_dispatch_after_reconcile); writing the
    # checkpoint before the supervisor would snapshot a pre-supervisor
    # view, and because synth.go prefers a fresher checkpoint over the
    # coord-state walk, a crash after a supervisor dispatch would strand
    # that worker. Bumping here + writing last keeps the counter durable
    # while the file reflects the final post-supervisor state.
    #
    #   tick_count++  ─heartbeat persist─▶ … supervisor … ─▶ checkpoint write
    #
    # Fail-soft: neither the bump nor the (deferred) write may wedge a tick.
    should_write_checkpoint = False
    try:
        should_write_checkpoint = _bump_tick_counter(state)
    except Exception as exc:  # noqa: BLE001 — counter bump must never fail a tick
        result.errors.append(f"rolling checkpoint counter: {exc}")

    # Heartbeat: rewrite coord-state.json on EVERY tick, even when nothing
    # was drained or dispatched. The Variant A dashboard reads this file's
    # mtime as the per-tick liveness signal — gating the write on
    # last_seen (issue #50) made dispatch-only ticks invisible to the TUI
    # and surfaced as `○ idle · auto-stopped` while the coord was actually
    # working. tmp+rename is cheap and idempotent on identical state, so
    # the unconditional refresh is correct. The tick_count bumped just
    # above (rolling checkpoint) is persisted here in the same write.
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
            coord_id=coord_id,
            result=result,
            emitted_this_tick=emitted_this_tick,
        )

    # 5.9. Rolling coord checkpoint (#187), part 2 — write the file LAST.
    # Runs after the supervisor so the checkpoint reflects any workers the
    # supervisor dispatched mid-tick (see part-1 comment above). The
    # counter was already bumped + persisted by the heartbeat; this is the
    # pure file write, gated on the interval decision from part 1. We
    # re-load coord-state from disk so the payload's slug→agent_id maps +
    # recent_decisions include the supervisor's own state mutations.
    # Fail-soft: a checkpoint write must never wedge a tick.
    if should_write_checkpoint:
        try:
            checkpoint_state = _load_coord_state(state_path)
            _write_rolling_checkpoint_file(
                project=project,
                project_dir=project_dir,
                coord_id=coord_id,
                state=checkpoint_state,
                home=home,
            )
            # Codex iter-7 [P1]: clear the durable "checkpoint due" latch
            # ONLY after a successful publish, and persist the clear so a
            # later tick doesn't needlessly retry. A failed write (caught
            # below) leaves the latch set → next tick retries.
            if checkpoint_state.get("checkpoint_due"):
                checkpoint_state["checkpoint_due"] = False
                _save_coord_state(state_path, checkpoint_state)
        except Exception as exc:  # noqa: BLE001 — checkpoint must never fail a tick
            result.errors.append(f"rolling checkpoint: {exc}")

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
    coord_id: str,
    result: TickResult,
    emitted_this_tick: set[str] | None = None,
) -> None:
    """Drive the supervisor loop. Hooks defined here so the supervisor
    module stays free of loop.py's mutation surface.

    The supervisor reads coord-state.json on every stuck-check pass
    (its own internal write), so the local `state` dict here is rebuilt
    on each reconcile-on-mtime-change call from disk. The loop is the
    only writer of supervisor.* + worker_agent_ids inside this skill
    (besides the initial dispatch path).

    `emitted_this_tick` (#184) is the SAME at-most-one-block-per-id set
    seeded by the primary phase's replay reconcile; the supervisor's own
    handoff/dispatch emitters respect + extend it so a block already
    emitted by replay (or the primary phase) is never re-emitted in a
    supervisor reconcile pass within the same tick.
    """
    if emitted_this_tick is None:
        emitted_this_tick = set()
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

        Codex iter-1 [P1]: actions with clear_worker=True are deferred
        when the reaper lane is still open for that slug — matches the
        primary tick path's invariant-5 gate. Without this, a worker
        that wrote phase=done mid-supervisor session could get its
        status flipped (and worker_agent_ids cleared) before the reaper
        sent /exit — leaking the tmux session as an orphan.
        """
        try:
            f3 = parse.read(str(tasks_path))
        except Exception as exc:  # noqa: BLE001
            result.errors.append(f"supervisor reconcile read: {exc}")
            return False
        scoped = [t for t in f3.tasks if t.slug in set(slugs)]
        if not scoped:
            return False
        cs = _load_coord_state(state_path)
        actions = _reconcile_inflight(
            scoped, project, fleet_bin, home=home, coord_state=cs,
        )
        slot_freed = False
        full_map = {t.slug: t for t in f3.tasks}
        for action in actions:
            # Invariant 5 gate: defer terminal flips while the reaper
            # still has an open kill cycle for this slug. Primary tick
            # path runs the same check at _tick_locked's reconcile loop;
            # supervisor-driven reconciles need the same protection.
            if action.clear_worker and not _reaper_lane_clear_for(
                cs, action.slug,
            ):
                continue
            try:
                _apply_reconcile(
                    action, project, fleet_bin,
                    repo=(cwd if cap > 1 else ""),
                    tasks_by_slug=(full_map if cap > 1 else None),
                    home=home,
                    full_tasks_by_slug=full_map,
                )
                if action.clear_worker:
                    # PR1 dispatch-lifecycle: release coord_prompt_inbox
                    # BEFORE forget_agent_id (same ordering rule as the
                    # primary tick reconcile site).
                    # Codex iter-7 [P1]: gate forget on outcome.
                    terminal_agent_id = supervisor_mod.load_agent_id_map(
                        cs,
                    ).get(action.slug, "")
                    release_outcome = _release_coord_prompt_inbox(
                        slug=action.slug,
                        agent_id=terminal_agent_id,
                        fleet_bin=fleet_bin,
                        fleet_home=home,
                        site=f"supervisor-reconcile new_status={action.new_status}",
                    )
                    if _release_outcome_is_terminal(release_outcome):
                        supervisor_mod.forget_agent_id(cs, action.slug)
                    elif terminal_agent_id:
                        # Codex iter-10 [P1]: stash for retry.
                        supervisor_mod.remember_pending_release_agent_id(
                            cs, action.slug, terminal_agent_id,
                        )
                    # Three-stage flow (reviewer-subagent-arch): clear
                    # the review-handoff dispatched markers in sync
                    # with the agent_id forget, mirroring the same
                    # cleanup in the primary tick path. Without this,
                    # supervisor-driven terminal transitions leave
                    # stale handoff keys that block future re-dispatches
                    # for the same slug.
                    _clear_review_handoff_state(cs, action.slug)
                    # Codex iter-2 [P1]: do NOT clear reaper_redispatch_
                    # pending here — the dispatch hook is the consumer.
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

        Codex iter-2 [P1]: also consumes reaper_redispatch_pending
        markers — a slug flagged by the error-abort path needs its
        status promoted from todo → ready before _dispatch_ready can
        pick it up.
        """
        # Consume the redispatch marker BEFORE re-reading tasks.md so
        # promoted slugs appear as `ready` to _dispatch_ready.
        cs0 = _load_coord_state(state_path)
        try:
            _consume_reaper_redispatch(
                project=project, fleet_bin=fleet_bin, home=home,
                coord_state=cs0, tasks_path=tasks_path,
            )
            _save_coord_state(state_path, cs0)
        except Exception as exc:  # noqa: BLE001
            result.errors.append(f"supervisor redispatch: {exc}")
        try:
            f4 = parse.read(str(tasks_path))
        except Exception as exc:  # noqa: BLE001
            result.errors.append(f"supervisor dispatch read: {exc}")
            return
        # Codex iter-4 [P1]: load the state ONCE and reuse it across
        # _dispatch_ready + the consumer loop so pending-acquire writes
        # from inside _dispatch_ready survive into the consumer's
        # _save_coord_state call below.
        cs = _load_coord_state(state_path)
        # Codex iter-13 [P1]: also fire the review-handoff dispatch
        # path in supervisor mode. The primary tick wires it, but the
        # supervisor's long-running loop only called _dispatch_ready
        # — so a worker that hit phase=review-pending or review-done
        # AFTER the initial tick would never have a reviewer/finisher
        # spawned until the supervisor exits and a fresh top-level
        # tick runs. _reconcile_inflight explicitly skips handoff
        # phases, so this is the only place that drives the three-
        # stage flow during a supervisor session.
        handoffs = _dispatch_review_handoffs(
            tasks=f4.tasks,
            project=project,
            fleet_bin=fleet_bin,
            fleet_home=str(home),
            home=home,
            coord_state=cs,
        )
        for action in handoffs:
            if action.error:
                result.errors.append(
                    f"supervisor handoff {action.slug}: {action.error}"
                )
                continue
            if action.agent_id:
                supervisor_mod.remember_agent_id(cs, action.slug, action.agent_id)
            try:
                _apply_dispatch_handoff(action, project, fleet_bin)
                if action.agent_id:
                    _record_review_handoff_dispatched(
                        cs, action.slug, action.handoff_phase,
                    )
                    supervisor_mod.forget_pending_acquire_agent_id(
                        cs, action.slug,
                    )
                # #184: shared at-most-one-block-per-id-per-tick.
                if (
                    action.dispatch_instruction
                    and action.agent_id not in emitted_this_tick
                ):
                    result.dispatch_instructions.append(action.dispatch_instruction)
                    if action.agent_id:
                        emitted_this_tick.add(action.agent_id)
            except Exception as exc:  # noqa: BLE001
                result.errors.append(
                    f"supervisor handoff apply {action.slug}: {exc}"
                )
        new_dispatched = _dispatch_ready(
            tasks=f4.tasks,
            project=project,
            cwd=cwd,
            cap=cap,
            fleet_bin=fleet_bin,
            fleet_home=str(home),
            coord_state=cs,
        )
        for action in new_dispatched:
            # Codex iter-4 [P1]: gate on action.error so an
            # acquire-prompt failure doesn't get remembered as a
            # successful dispatch with a stale agent_id (see the
            # matching gate on the primary tick dispatch loop).
            if action.error:
                result.errors.append(
                    f"supervisor dispatch {action.slug}: {action.error}"
                )
                continue
            # Codex iter-7 [P1]: persist agent_id BEFORE apply (same
            # rationale as the primary tick consumer — mid-apply
            # failures must still leave the slug→agent_id record so
            # the orphaned claim is releasable on the next reconcile).
            if action.agent_id:
                supervisor_mod.remember_agent_id(cs, action.slug, action.agent_id)
            try:
                _apply_dispatch(action, project, fleet_bin)
                if action.agent_id:
                    # Codex iter-5 [P1]: same caller-side forget as the
                    # primary tick path — only AFTER apply succeeds.
                    supervisor_mod.forget_pending_acquire_agent_id(
                        cs, action.slug,
                    )
                # Issue #84 Phase A: supervisor-loop dispatches still
                # need to publish the DISPATCH block so the coord
                # agent can spawn the Agent subagent. Without this,
                # the supervisor's mid-tick re-dispatch would write
                # the inbox file but never surface the spawn
                # instruction — task would sit in-progress with no
                # actual worker.
                #
                # #184: shared at-most-one-block-per-id-per-tick.
                if (
                    action.dispatch_instruction
                    and action.agent_id not in emitted_this_tick
                ):
                    result.dispatch_instructions.append(action.dispatch_instruction)
                    if action.agent_id:
                        emitted_this_tick.add(action.agent_id)
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
        # Codex iter-3 [P1]: also drive the rolling checkpoint from the
        # supervisor's periodic heartbeat. The supervisor can hold the
        # lock for hours (poll loop, default 4h max) and dispatch workers
        # mid-session; without this the checkpoint would only refresh at
        # the top of the next top-level tick — i.e. never, during a long
        # session — defeating the bounded-staleness guarantee. Bump the
        # counter (persisted in the same cs write below) and, at interval,
        # write coord-checkpoint.md reflecting the current on-disk state.
        # Fail-soft: a checkpoint failure must not wedge the heartbeat.
        try:
            if _bump_tick_counter(cs):
                _write_rolling_checkpoint_file(
                    project=project, project_dir=project_dir,
                    coord_id=coord_id, state=cs, home=home,
                )
                # Codex iter-7 [P1]: clear the due-latch only after a
                # successful publish (cs is persisted below). A failed
                # write leaves it set → retried next heartbeat / tick.
                cs["checkpoint_due"] = False
        except Exception as exc:  # noqa: BLE001 — never wedge the heartbeat
            result.errors.append(f"supervisor rolling checkpoint: {exc}")
        _save_coord_state(state_path, cs)

    # Invariant 4 force-tick: short-circuit the next sleep when there's
    # a pending inbox event for this coord. Cheap fs scan; the supervisor
    # consults this on every iteration before computing the sleep budget.
    #
    # Codex iter-1 [P1]: the ~/.fleet/inbox/<coord-id>.md file persists
    # across the entire supervisor session — fleet-guard's coord-side
    # hook (not the coord skill) clears it. Existence-alone would wedge
    # this check into "always True" and spin the supervisor at 0-second
    # sleeps. Baseline the mtime at supervisor entry so only an mtime
    # ADVANCE post-entry counts as an event.
    #
    # Codex iter-2 [P1]: after the FIRST force-tick fires, advance the
    # baseline + archive watermark to the current values so the same
    # event doesn't re-fire on the next iteration (which would spin
    # the supervisor at 0-second sleeps). We mutate the closed-over
    # baseline + last_seen_archive on every True return.
    # Codex iter-3 [P3]: use the coord_id tick() already resolved
    # (passed in as a parameter), not a fresh env read. tick(coord_id=
    # "...") with FLEET_AGENT_ID unset or stale would otherwise watch
    # the wrong inbox/archive surface — the supervisor would silently
    # ignore force-tick events.
    coord_id_for_force = coord_id or os.environ.get("FLEET_AGENT_ID", "") or ""
    # Use a mutable singleton dict so the inner hook can update the
    # baseline without `nonlocal`. The hook reads the current value
    # before each check and bumps it after a hit.
    force_tick_baseline = {"mtime": 0.0, "archive": ""}
    if coord_id_for_force:
        baseline_path = home / "inbox" / f"{coord_id_for_force}.md"
        try:
            force_tick_baseline["mtime"] = baseline_path.stat().st_mtime
        except OSError:
            force_tick_baseline["mtime"] = 0.0
        # Seed the archive watermark from coord-state's last_archive_scan_ts.
        cs0 = _load_coord_state(state_path)
        force_tick_baseline["archive"] = str(
            cs0.get("last_archive_scan_ts", "") or ""
        )

    def force_tick_check_hook() -> bool:
        # Codex iter-8 [P2]: the archive watermark lives in coord-state's
        # `last_archive_scan_ts`. We read it FRESH every call so the
        # actual drain side (which respects _ARCHIVE_SCAN_CAP of 200
        # files per pass) is the single source of truth. Previously
        # this hook eagerly advanced its own baseline to the NEWEST
        # archive filename — which meant when 201+ files queued, the
        # drain consumed 200, baseline jumped past all 201, and the
        # 201st never re-fired force_tick. Now: read disk, trust disk.
        cs = _load_coord_state(state_path)
        disk_archive = str(cs.get("last_archive_scan_ts", "") or "")
        # Keep the closed-over field in sync (used for the no-event
        # case + a fast-path if the disk read raced).
        force_tick_baseline["archive"] = disk_archive
        triggered = supervisor_mod.has_pending_inbox_events(
            coord_id=coord_id_for_force,
            fleet_home=home,
            last_seen_archive=force_tick_baseline["archive"],
            direct_inbox_mtime_baseline=force_tick_baseline["mtime"],
        )
        if triggered:
            # Advance ONLY the direct-inbox mtime baseline — the archive
            # baseline advances via the drain side on the same iteration.
            # Without the direct-inbox advance the same operator message
            # would refire on every iteration.
            if coord_id_for_force:
                direct = home / "inbox" / f"{coord_id_for_force}.md"
                try:
                    new_mtime = direct.stat().st_mtime
                    if new_mtime > force_tick_baseline["mtime"]:
                        force_tick_baseline["mtime"] = new_mtime
                except OSError:
                    pass
        return triggered

    # Invariant 5 reaper hook. Runs every iteration so a worker that
    # writes phase=done mid-supervisor session is reaped within the
    # base poll cadence (5 s default) rather than waiting for the
    # next stuck-check pass (5 min default).
    #
    # Codex iter-3 [P2]: returns the list of slugs whose kill cycle
    # COMPLETED this iteration (state ∈ {killed, hard-killed}). The
    # supervisor uses this list to re-trigger reconcile against those
    # slugs immediately — without it, a worker whose mtime advanced
    # several polls earlier (when the lane wasn't clear) wouldn't get
    # its status flip until the periodic full reconcile fires.
    #
    # Codex iter-13 [P1]: when the reaper kills a slug, also replay
    # any deferred sentinels for that slug NOW (during the normal
    # iteration), not on a future forced wake. The deferred-sentinel
    # queue would otherwise hold a stale TASK_DONE_PR / WORKER_FAILED
    # that the next forced wake could replay over already-applied
    # reconcile transitions — restoring stale PR URLs / rolling
    # replacement workers back to todo.
    def reaper_hook_supervisor(probes) -> list[str]:
        try:
            f5 = parse.read(str(tasks_path))
        except Exception as exc:  # noqa: BLE001
            result.errors.append(f"reaper hook tasks-read: {exc}")
            return []
        cs = _load_coord_state(state_path)
        decisions = _reap_inflight(
            f5.tasks, project=project, home=home, fleet_bin=fleet_bin,
            coord_state=cs, now_unix=time.time(),
        )
        reaped: list[str] = []
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
            if dec.state in ("killed", "hard-killed"):
                reaped.append(dec.slug)
        # Replay any deferred sentinels whose reaper lane is now
        # clear. Codex iter-20 [P2]: ALWAYS replay (no `if reaped`
        # gate), not just after a fresh reap. Sentinel deferrals can
        # come from:
        #   (a) reaper-lane-not-clear at apply time — those clear
        #       when the reaper finishes (still triggered by reaped).
        #   (b) transient `fleet tasks set/note` failure in either
        #       drain path — those should retry every iteration
        #       regardless of reaper state.
        replay_applied = _replay_deferred_sentinels(
            project=project, fleet_bin=fleet_bin, home=home,
            cs=cs, cwd=cwd, cap=cap,
        )
        _save_coord_state(state_path, cs)
        # If the replay flipped task status (slot freed), backfill
        # the slot via the same dispatch surface the mtime path uses.
        if replay_applied > 0:
            _maybe_dispatch_after_reconcile()
        return reaped

    def _replay_deferred_sentinels(
        *, project: str, fleet_bin: str, home: Path,
        cs: dict, cwd: str, cap: int,
    ) -> int:
        """Apply queued deferred sentinels whose reaper lane is clear.
        Called from reaper_hook_supervisor right after a successful
        reap so stale sentinels don't sit in the queue waiting for
        the next forced wake (codex iter-13 [P1]).

        Returns the count of sentinels successfully applied this pass.
        The caller uses this to decide whether to fire
        _maybe_dispatch_after_reconcile (codex iter-14 [P1]: a replay
        that frees a slot must trigger dispatch on the SAME tick)."""
        deferred = _load_deferred_sentinels(cs)
        if not deferred:
            return 0
        try:
            f_local = parse.read(str(tasks_path))
        except Exception:
            return 0
        tbs = {t.slug: t for t in f_local.tasks}
        still_deferred: list[_SentinelAction] = []
        sentinel_repo = cwd if cap > 1 else ""
        sentinel_tbs = tbs if cap > 1 else None
        applied = 0
        for action in deferred:
            if action.kind in ("task_done_pr", "worker_failed") and not (
                _reaper_lane_clear_for(cs, action.slug)
            ):
                still_deferred.append(action)
                continue
            try:
                sentinel_outcome = _apply_sentinel(
                    action, project, fleet_bin,
                    repo=sentinel_repo,
                    tasks_by_slug=sentinel_tbs,
                    home=home,
                    full_tasks_by_slug=tbs,
                )
                # DESIGN §3 S4: a STALE deferred-replay sentinel did NO
                # terminal side effect — consume it (drop from the queue,
                # no release/forget/handoff-clear). Not re-deferred: a
                # stale sentinel never becomes current.
                if sentinel_outcome == SENTINEL_SKIPPED_STALE:
                    continue
                # Codex iter-23 [P2]: same blocked_question carve-out
                # as the non-replay drain — blocked workers stay alive,
                # so we must preserve the agent_id mapping. Only the
                # terminating sentinels (task_done_pr, worker_failed)
                # forget the mapping.
                if action.kind in ("task_done_pr", "worker_failed"):
                    # PR1 dispatch-lifecycle: release coord_prompt_inbox
                    # at the same beat as forget. Replay-deferred drain
                    # can run multiple ticks after the worker exited, so
                    # an already_released outcome is common when the
                    # primary drain already ran — that's still a success
                    # outcome (idempotent by design).
                    # Codex iter-7 [P1]: gate forget on outcome so
                    # transient release errors don't lose the only
                    # retry handle.
                    terminal_agent_id = supervisor_mod.load_agent_id_map(
                        cs,
                    ).get(action.slug, "")
                    release_outcome = _release_coord_prompt_inbox(
                        slug=action.slug,
                        agent_id=terminal_agent_id,
                        fleet_bin=fleet_bin,
                        fleet_home=home,
                        site=f"supervisor-sentinel-replay kind={action.kind}",
                    )
                    if _release_outcome_is_terminal(release_outcome):
                        supervisor_mod.forget_agent_id(cs, action.slug)
                    elif terminal_agent_id:
                        # Codex iter-10 [P1]: stash for retry.
                        supervisor_mod.remember_pending_release_agent_id(
                            cs, action.slug, terminal_agent_id,
                        )
                    _clear_review_handoff_state(cs, action.slug)
                applied += 1
            except Exception as exc:  # noqa: BLE001
                # Codex iter-18 [P2]: re-add the action to the
                # deferred queue so a later pass retries. Otherwise a
                # transient `fleet tasks set/note` failure would
                # permanently drop the TASK_DONE_PR / WORKER_FAILED
                # transition (the archive file is already past the
                # last_archive_scan_ts watermark — there's no other
                # source of truth).
                still_deferred.append(action)
                result.errors.append(
                    f"deferred sentinel {action.slug}: {exc} "
                    "(re-queued for retry)"
                )
        _save_deferred_sentinels(cs, still_deferred)
        return applied

    # Codex iter-3 [P2]: drain + dispatch on every forced wake so a
    # NEW_TASK that arrives mid-supervisor session is picked up under
    # spare cap. Calls _maybe_dispatch_after_reconcile (which itself
    # consumes reaper_redispatch_pending markers + runs _dispatch_ready);
    # that's idempotent on a tick where no new task is available.
    #
    # Codex iter-4 [P1]: also drain inbox/archive sentinels on the
    # forced wake. Without this, archive events for in-flight workers
    # (WORKER_FAILED, TASK_DONE_PR) sit in inbox/archive/ while
    # supervisor-mode reconcile already requeues / redispatches the
    # same slug — the next top-level tick then REPLAYS the stale
    # sentinel through _apply_sentinel, which can roll a replacement
    # worker back to `todo` or restore an old PR URL. Fix: drain the
    # archive inside the same wake, advance the watermark to "this
    # tick consumed everything I had", so replay is impossible.
    #
    # Codex iter-8 [P1]: run the reaper BEFORE the drain. For a worker
    # whose state.json reports phase=done/failed (so the reaper would
    # judge it complete), but which hasn't yet finished its kill
    # cycle, we must NOT let the drain's TASK_DONE_PR/WORKER_FAILED
    # sentinel forget the agent_id before the reaper sends /exit. The
    # primary tick already runs reaper-before-drain (step 2.5 then 4);
    # this brings the forced-wake path into alignment.
    def force_tick_dispatch_hook():
        _run_reaper_once_supervisor()
        _drain_inbox_archive_supervisor()
        _maybe_dispatch_after_reconcile()

    def _run_reaper_once_supervisor():
        """One-shot reaper pass under the supervisor lock.
        Mirrors reaper_hook_supervisor's behavior: kills via reaper,
        replays deferred sentinels for just-reaped slugs, then
        reconciles those slugs so the status flip lands THIS tick
        instead of waiting for the next periodic full reconcile
        (codex iter-15 [P2]: a force-tick-driven reap was previously
        invisible to the body's reconcile loop because the entry
        was already cleared by the time the body ran)."""
        try:
            f7 = parse.read(str(tasks_path))
        except Exception as exc:  # noqa: BLE001
            result.errors.append(f"reaper hook tasks-read: {exc}")
            return
        cs = _load_coord_state(state_path)
        decisions = _reap_inflight(
            f7.tasks, project=project, home=home, fleet_bin=fleet_bin,
            coord_state=cs, now_unix=time.time(),
        )
        reaped: list[str] = []
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
            if dec.state in ("killed", "hard-killed"):
                reaped.append(dec.slug)
        # Codex iter-20 [P2]: replay deferred sentinels every pass,
        # not gated on `if reaped`. Transient apply failures need
        # retries even when no new reap happened.
        _replay_deferred_sentinels(
            project=project, fleet_bin=fleet_bin, home=home,
            cs=cs, cwd=cwd, cap=cap,
        )
        _save_coord_state(state_path, cs)
        # Codex iter-15 [P2]: reconcile the just-reaped slugs in
        # the same iteration so the status flip lands this tick.
        # _reconcile_slugs is idempotent on already-flipped slugs.
        if reaped:
            try:
                _reconcile_slugs(reaped)
            except Exception as exc:  # noqa: BLE001
                result.errors.append(f"force-tick reaper reconcile: {exc}")

    def _drain_inbox_archive_supervisor():
        """Drain archive sentinels under the supervisor lock.

        Mirrors the primary tick's drain step (loop._tick_locked step 4)
        with the same reaper-lane gate and the same deferred-sentinel
        queue (codex iter-5 [P1]) — deferred actions are persisted in
        coord-state and replayed on the next pass.
        """
        try:
            f6 = parse.read(str(tasks_path))
        except Exception as exc:  # noqa: BLE001
            result.errors.append(f"supervisor drain tasks-read: {exc}")
            return
        cs = _load_coord_state(state_path)
        tasks_by_slug = {t.slug: t for t in f6.tasks}
        drained, last_seen = _drain_archive(
            home / "inbox" / "archive",
            coord_id=coord_id,
            since=str(cs.get("last_archive_scan_ts", "") or ""),
            tasks_by_slug=tasks_by_slug,
        )
        prior_deferred = _load_deferred_sentinels(cs)
        drained = prior_deferred + drained
        if not drained and not last_seen:
            return
        sentinel_repo = cwd if cap > 1 else ""
        sentinel_tasks_by_slug = tasks_by_slug if cap > 1 else None
        deferred_actions: list[_SentinelAction] = []
        for action in drained:
            if action.kind in ("task_done_pr", "worker_failed") and not (
                _reaper_lane_clear_for(cs, action.slug)
            ):
                deferred_actions.append(action)
                continue
            try:
                sentinel_outcome = _apply_sentinel(
                    action, project, fleet_bin,
                    repo=sentinel_repo,
                    tasks_by_slug=sentinel_tasks_by_slug,
                    home=home,
                    full_tasks_by_slug=tasks_by_slug,
                )
                # DESIGN §3 S4: STALE sentinel → consume, no teardown,
                # no re-defer (a stale sentinel never becomes current).
                if sentinel_outcome == SENTINEL_SKIPPED_STALE:
                    continue
                # Codex iter-22 [P1]: blocked workers stay ALIVE so
                # the operator can answer the BLOCKED_QUESTION. We
                # must NOT forget the agent_id mapping in that case —
                # otherwise the stuck-check and manual-cleanup paths
                # lose the only handle on the still-live tmux session.
                # task_done_pr / worker_failed both terminate the
                # worker; agent_id cleanup is correct for those.
                if action.kind in ("task_done_pr", "worker_failed"):
                    # PR1 dispatch-lifecycle: release the
                    # coord_prompt_inbox before forget — same ordering
                    # rule as the primary drain.
                    # Codex iter-7 [P1]: gate forget on outcome.
                    terminal_agent_id = supervisor_mod.load_agent_id_map(
                        cs,
                    ).get(action.slug, "")
                    release_outcome = _release_coord_prompt_inbox(
                        slug=action.slug,
                        agent_id=terminal_agent_id,
                        fleet_bin=fleet_bin,
                        fleet_home=home,
                        site=f"supervisor-sentinel-drain kind={action.kind}",
                    )
                    if _release_outcome_is_terminal(release_outcome):
                        supervisor_mod.forget_agent_id(cs, action.slug)
                    elif terminal_agent_id:
                        # Codex iter-10 [P1]: stash for retry.
                        supervisor_mod.remember_pending_release_agent_id(
                            cs, action.slug, terminal_agent_id,
                        )
                    _clear_review_handoff_state(cs, action.slug)
            except Exception as exc:  # noqa: BLE001
                # Codex iter-18 [P2]: re-queue on transient failure.
                deferred_actions.append(action)
                result.errors.append(
                    f"supervisor sentinel {action.slug}: {exc} "
                    "(re-queued for retry)"
                )
        _save_deferred_sentinels(cs, deferred_actions)
        if last_seen:
            cs["last_archive_scan_ts"] = last_seen
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
        force_tick_dispatch=force_tick_dispatch_hook,
        reaper_hook=reaper_hook_supervisor,
        coord_id=coord_id,
        # Codex iter-21 [P3]: share the single mtime baseline that
        # force_tick_check_hook closed over (force_tick_baseline["mtime"])
        # so the operator-message-exit gate and the force-tick check
        # both use the same starting point. Eliminates the two-read
        # race where an operator write between the two snapshots
        # could be silenced.
        direct_inbox_session_baseline=force_tick_baseline["mtime"],
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
        # R5 chokepoint (DESIGN §2.1/§3): the reaper judges this slug's
        # worker_state (judge_completion) and may kill/reap on a
        # terminal phase. A STALE state (prior dispatch_generation) must
        # NOT drive a reap of the CURRENT attempt — skip the task
        # entirely from the reap input set so a stale `phase=done`/
        # `phase=failed` never reaps/kills the live worker. `current`
        # passes its state through; `missing` passes None (the reaper
        # judges PENDING — can't judge yet — a benign no-op).
        rcls, rstate = read_current_worker_state(
            project, t.slug, int(t.dispatch_generation), home=home,
        )
        if rcls == WORKER_STATE_STALE:
            import sys
            print(
                f"coord: reaper skipped {t.slug} — stale worker state "
                f"(prior dispatch_generation, authority="
                f"{int(t.dispatch_generation)}); not reaping, surfacing",
                file=sys.stderr,
            )
            continue
        worker_state = rstate
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


# Codex iter-5 [P1]: deferred sentinel queue in coord-state.
# Schema under coord-state["deferred_sentinels"]: a list of dicts, one
# per deferred sentinel action. Each entry preserves the original
# action's slug + kind + payload + raised_to_user + raise_text +
# source_file so the next tick can re-apply it without re-reading the
# archive file. The list is FIFO; new deferrals append, the next tick
# prepends the list to its fresh drained batch.
_DEFERRED_SENTINELS_KEY = "deferred_sentinels"
# Hard cap so a chronic deferral (operator never unblocks) doesn't
# unbounded-grow coord-state.json. 200 matches _ARCHIVE_SCAN_CAP.
_DEFERRED_SENTINELS_CAP = 200


def _load_deferred_sentinels(coord_state: dict) -> list["_SentinelAction"]:
    """Return the FIFO list of sentinels deferred on prior ticks."""
    raw = coord_state.get(_DEFERRED_SENTINELS_KEY, [])
    if not isinstance(raw, list):
        return []
    out: list[_SentinelAction] = []
    for entry in raw:
        if not isinstance(entry, dict):
            continue
        slug = str(entry.get("slug", "") or "")
        kind = str(entry.get("kind", "") or "")
        if not slug or kind not in (
            "task_done_pr", "blocked_question", "worker_failed", "new_task",
        ):
            continue
        # DESIGN §3 S5: round-trip the dispatch_generation token so a
        # deferred→replayed sentinel corroborates on replay (neither
        # fail-open removal nor fail-closed leak). Absent / null in the
        # persisted entry → None (tokenless-legacy).
        raw_gen = entry.get("dispatch_generation", None)
        gen: int | None
        if raw_gen is None:
            gen = None
        else:
            try:
                gen = int(raw_gen)
            except (TypeError, ValueError):
                gen = None
        out.append(_SentinelAction(
            slug=slug, kind=kind,
            payload=str(entry.get("payload", "") or ""),
            raised_to_user=bool(entry.get("raised_to_user", False)),
            raise_text=str(entry.get("raise_text", "") or ""),
            source_file=str(entry.get("source_file", "") or ""),
            dispatch_generation=gen,
        ))
    return out


def _save_deferred_sentinels(
    coord_state: dict, actions: list["_SentinelAction"],
) -> None:
    """Persist the deferred-sentinel queue. Empties the key when the
    list is empty to keep coord-state.json tidy."""
    if not actions:
        coord_state.pop(_DEFERRED_SENTINELS_KEY, None)
        return
    # Codex iter-12 [P2]: do NOT drop entries when over cap. The
    # primary tick's drain advances last_archive_scan_ts past the
    # source files; if we dropped deferred entries the corresponding
    # task transitions would be lost permanently. Instead, log loudly
    # if the queue is unbounded and keep all entries. The cap is now
    # a "soft warning" threshold rather than a hard truncate.
    if len(actions) > _DEFERRED_SENTINELS_CAP:
        import sys as _sys
        _sys.stderr.write(
            f"[P0] coord: deferred-sentinel queue has {len(actions)} entries "
            f"(soft cap {_DEFERRED_SENTINELS_CAP}); reaper-lane churn or "
            "stuck workers may be blocking sentinel replays. Investigate "
            "via `fleet status`.\n"
        )
        _sys.stderr.flush()
    coord_state[_DEFERRED_SENTINELS_KEY] = [
        {
            "slug": a.slug,
            "kind": a.kind,
            "payload": a.payload,
            "raised_to_user": a.raised_to_user,
            "raise_text": a.raise_text,
            "source_file": a.source_file,
            # DESIGN §3 S5: persist the generation token (None for a
            # tokenless-legacy sentinel) so replay corroborates correctly.
            "dispatch_generation": a.dispatch_generation,
        }
        for a in actions
    ]


def _consume_reaper_redispatch(
    *,
    project: str,
    fleet_bin: str,
    home: Path,
    coord_state: dict,
    tasks_path: Path,
) -> None:
    """Promote `reaper_redispatch_pending` slugs from todo → ready.

    Codex iter-2 [P1]: the reaper sets the redispatch marker for slugs
    that hit phase=failed (error-abort judgment). Reconcile flips those
    slugs to `todo`. `_filter_ready` only dispatches status=ready
    tasks, so without an explicit promotion step a failed worker just
    sits idle until the operator manually re-runs it.

    Algorithm:
      - For each slug in the pending set:
          * Re-read tasks.md (cheap; microseconds).
          * If the task is at status=todo, shell `fleet tasks set
            status=ready` so the next _dispatch_ready picks it up.
          * Drop the marker either way: status=todo+promoted → marker
            consumed; status=anything-else → marker no longer relevant.

    Best-effort: a shell failure logs but doesn't block other slugs;
    the next tick retries (the marker stays set on failure).
    """
    pending = reaper_mod.load_redispatch_pending(coord_state)
    if not pending:
        return
    try:
        f = parse.read(str(tasks_path))
    except Exception:
        return
    by_slug = {t.slug: t for t in f.tasks}
    # Codex iter-9 [P1]: when reaper sets redispatch_pending the task
    # is typically still at status=in-progress (the lane is being
    # cleared; reconcile flips it to todo on a subsequent tick).
    # Treating in-progress as "stale" was wrong — drop only the
    # definitively-terminal statuses; leave in-progress untouched so
    # the next reconcile pass can flip it and the consume runs against
    # the new state.
    #
    # Codex iter-16 [P2]: include `in-review` in the drop set. A
    # deferred TASK_DONE_PR replay can move an error-aborted slug
    # from todo BACK to in-review (recovery: the worker that failed
    # actually did ship a PR before the failure write). The marker
    # is then stale; if the PR later hits CI red and reconcile flips
    # back to todo, the stale marker would re-promote to ready and
    # spawn an unwanted replacement worker.
    _DROP_STATUSES = {"done", "blocked", "abandoned", "ready", "in-review"}
    for slug in list(pending):
        t = by_slug.get(slug)
        if t is None:
            # Task disappeared (archived?) — drop the stale marker.
            reaper_mod.clear_redispatch_pending(coord_state, slug)
            continue
        if t.status == "todo":
            # Promote todo → ready. _run_fleet raises on failure → the
            # marker stays set and the next tick retries.
            try:
                _run_fleet([
                    fleet_bin, "tasks", "set", "--project", project,
                    slug, "status=ready",
                ])
                _run_fleet([
                    fleet_bin, "tasks", "note", "--project", project,
                    slug,
                    "reaper: error-abort → re-dispatch (replacement worker)",
                ])
                reaper_mod.clear_redispatch_pending(coord_state, slug)
            except Exception:
                continue
            continue
        if t.status in _DROP_STATUSES:
            # Operator intervened or task reached a terminal state
            # where re-dispatch doesn't apply.
            reaper_mod.clear_redispatch_pending(coord_state, slug)
            continue
        # status in-progress/in-review — reaper or reconcile hasn't
        # finished the transition yet. KEEP the marker; the next tick
        # consumes it after reconcile flips to todo.
        continue


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


# 8-hex agent-id shape. Mirrors internal/tui/dashboard.go:isAgentIDShape
# so the Python coord and the Go dashboard agree on what counts as a
# valid lock-body / marker holder. Anything else (legacy zero-byte
# lock, hand-edit, garbage) is treated as "no holder".
_AGENT_ID_RE = re.compile(r"^[0-9a-f]{8}$")


def _read_lock_holder(lock_path: Path) -> str:
    """Return the agent ID written into coordinator.lock's body, or "".

    The Python coord skill writes ``<coord_id>\\n`` into the lock body
    after acquiring LOCK_EX (`_try_lock`). We read the first line and
    validate the 8-hex shape; anything else returns "" so callers fall
    through to the "no provable holder" path. Mirrors the Go reader at
    internal/tui/dashboard.go:readCoordHolder.

    NOTE: flock(2) does not truncate the body on release, so a stale
    body can name a dead coord. The liveness probe in
    `_classify_lock_busy` is the load-bearing freshness gate.
    """
    try:
        with open(lock_path, "r", encoding="utf-8", errors="replace") as fh:
            first = fh.readline()
    except OSError:
        return ""
    s = first.strip()
    return s if _AGENT_ID_RE.match(s) else ""


def _read_coord_spawn_marker(project_dir: Path) -> str:
    """Return the agent ID in the project's coord-spawn-marker, or "".

    The marker (``.locks/coord-spawn-marker``) names the project's
    INTENDED coord — the TUI/CLI writes it on every fresh coord spawn
    (state.WriteCoordSpawnMarker). When the marker names US, this
    session is the legitimate / successor coord and must never
    self-exit even while a stale predecessor still holds the flock for
    one more tick. Mirrors state.ReadCoordSpawnMarker.
    """
    marker_path = project_dir / ".locks" / "coord-spawn-marker"
    try:
        with open(marker_path, "r", encoding="utf-8", errors="replace") as fh:
            first = fh.readline()
    except OSError:
        return ""
    return first.strip()


def _agent_is_project_coord(home: Path, agent_id: str, project: str) -> bool:
    """True iff ~/.fleet/agents/<id>.json names THIS project's coord.

    Applied to BOTH the lock holder and the current session: a bare
    "the record parses" check is NOT enough to justify a self-exit
    (codex review [P2]). For the HOLDER, the lock body could name a
    same-project WORKER that hijacked coordinator.lock (the fleet#171
    hijack the project-ownership guard exists to catch), or a stale
    body naming a live agent that belongs to a DIFFERENT project — in
    neither case is the holder a legitimate coord to defer to. For the
    CURRENT session, a worker that accidentally runs `loop.py` must not
    be classified a duplicate coord and killed. So both the deferred-to
    holder and the self-exiting session must clear this check.

    We require the record to be genuinely this project's coord on TWO
    axes:

      - ``project`` field == this project (excludes cross-project
        agents whose ID leaked into the body), and
      - ``task_id`` == ``coord-<project>`` (the canonical coord task
        id — see internal/tui/keys.go:coordTaskID,
        internal/handoff/synth.go). A worker's task_id is its feature
        task, never ``coord-<project>``, so this excludes a hijacking
        worker.

    Both must hold; a record missing either field (legacy / partial)
    fails closed -> caller skips rather than self-exits.
    """
    if not agent_id or not project:
        return False
    try:
        with open(home / "agents" / f"{agent_id}.json", "r", encoding="utf-8") as fh:
            rec = json.load(fh)
    except (OSError, ValueError):
        return False
    if not isinstance(rec, dict):
        return False
    if (rec.get("project") or "") != project:
        return False
    return (rec.get("task_id") or "") == f"coord-{project}"


def _classify_lock_busy(
    lock_path: Path,
    project_dir: Path,
    coord_id: str,
    home: Path,
) -> tuple[str, str]:
    """Decide what a coord that LOST the coordinator.lock should do.

    Returns ``(decision, diagnostic)`` where decision is:

      - "duplicate" — a DIFFERENT, LIVE coord holds the lock and this
        session is not the project's intended coord. Caller self-exits.
      - "skip"      — every conservative gate failed; just skip the
        tick (historical behavior). diagnostic is "".

    Conservatism (any one fires -> "skip"):

      1. No coord_id  -> can't reason about identity (manual shell
         invocation). Skip.
      2. THIS session is not itself this project's coord (its own
         record is missing / project-mismatched / task_id !=
         coord-<project>) -> a worker that accidentally ran loop.py
         must never self-exit and kill its own worker session. Skip.
         (codex review [P2], round 2)
      3. Lock body has no valid holder ID -> can't prove a duplicate.
         Skip. (legacy zero-byte lock, hand-edit, race before the
         holder published its body.)
      4. holder == coord_id -> our own stale flock from a prior tick
         that hasn't been reaped yet. Not a duplicate. Skip.
      5. spawn-marker == coord_id -> WE are the project's intended /
         successor coord (mid-handoff). Never self-exit; the stale
         holder is the one to be reaped (by the swap / `fleet gc`).
         Skip.
      6. holder's record is not THIS project's coord (missing record,
         project mismatch, or task_id != coord-<project> — e.g. a
         same-project worker that hijacked the lock, or a cross-project
         agent ID in the body) OR holder's tmux session is dead -> not
         a legitimate live coord to defer to. Skip. (codex review [P2])

    Only when NONE of the above fire is the session a true duplicate.
    """
    if not coord_id:
        return "skip", ""
    # THIS session must itself be this project's coord before we would
    # ever tear it down (codex review [P2], round 2). A WORKER that
    # accidentally runs `loop.py <project>` (or whose FLEET_AGENT_ID is
    # set) while the real coord holds the lock would otherwise be
    # classified "duplicate" and have main() kill `fleet-<worker_id>`,
    # destroying unrelated worker work. Self-exit is a coord-only
    # self-heal; non-coords just skip the tick.
    if not _agent_is_project_coord(home, coord_id, project_dir.name):
        return "skip", ""
    holder = _read_lock_holder(lock_path)
    if not holder:
        return "skip", ""
    if holder == coord_id:
        return "skip", ""
    if _read_coord_spawn_marker(project_dir) == coord_id:
        return "skip", ""
    # The holder must be a genuinely live coord FOR THIS PROJECT, not a
    # stale lock body, a hijacking same-project worker, or a cross-
    # project agent whose ID leaked into the body (codex review [P2]).
    if not _agent_is_project_coord(home, holder, project_dir.name):
        return "skip", ""
    holder_session = supervisor_mod.session_name_for_agent(holder)
    if not supervisor_mod.tmux_session_alive(holder_session):
        return "skip", ""
    diag = (
        f"[coord] coordinator.lock for {project_dir.name!r} is held by a "
        f"different LIVE coord {holder} (tmux {holder_session}); this "
        f"session {coord_id} is a duplicate doing no work — exiting to "
        f"self-heal to one coord. If this is wrong, run `fleet status` "
        f"to inspect the coords for this project."
    )
    return "duplicate", diag


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


_PHANTOM_ESCALATED_KEY = "phantom_launch_escalated"


def _phantom_already_escalated(state: dict, agent_id: str, generation: int) -> bool:
    """True if the residual-crash escalation already fired for this
    (agent_id, generation) phantom. Codex iter-6 [P2]: without a durable
    breadcrumb the repair re-raises the same operator escalation every
    tick. Keyed by generation so a reset-for-relaunch (which bumps gen)
    gets a fresh escalation if the relaunch also phantoms."""
    raw = state.get(_PHANTOM_ESCALATED_KEY)
    if not isinstance(raw, list):
        return False
    return f"{agent_id}:{generation}" in raw


def _record_phantom_escalated(state: dict, agent_id: str, generation: int) -> None:
    """Persist that the residual-crash escalation fired for this
    (agent_id, generation). Mutates coord_state in place; tick() saves it.
    Bounded: capped to the most recent 200 entries so a long-lived coord
    can't grow the list without limit."""
    raw = state.get(_PHANTOM_ESCALATED_KEY)
    if not isinstance(raw, list):
        raw = []
    key = f"{agent_id}:{generation}"
    if key not in raw:
        raw.append(key)
    if len(raw) > 200:
        raw = raw[-200:]
    state[_PHANTOM_ESCALATED_KEY] = raw


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

# 8-hex agent_id shape (matches dispatch.py:_AGENT_ID_FULL_RE + the Go
# DispatchID regex). Used to filter dispatch-journal filenames in the
# replay reconcile.
_AGENT_ID_RE = re.compile(r"^[0-9a-f]{8}$")

# dispatch-durability (#184) tunables.
#
# REPLAY_CAP — total-per-dispatch replay-emission budget. Once a
# dispatch's reserve count reaches this, the journal flips to ExecBlocked
# (durable, off-channel escalation) instead of re-emitting forever. The
# count is durable IN the journal, so it survives coord restarts (a
# broken-stdout incident that recurs N times across restarts hits the cap
# and stops, rather than looping).
_REPLAY_CAP = 5

# LAUNCH_ACK_GRACE_S — how long an ExecLaunchAttempted entry may sit
# un-acked with no live subagent before residual-crash repair flips it to
# ExecBlocked. A crash AFTER mark-launch-attempted but BEFORE the Agent
# tool invocation (or a launch that died instantly) leaves a silent
# phantom that bootstrapped state.json reads "alive" for 15 min — so we
# can't trust state.json freshness; we repair on (launch_attempted + no
# ack + no live registered subagent + elapsed > grace). 2 ticks of slack
# (~the supervisor poll cadence) avoids false-positives on a coord that's
# mid-launch when the next tick fires.
_LAUNCH_ACK_GRACE_S = 90


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


# The chokepoint reader's tri-state outcomes (DESIGN
# DESIGN-coord-worktree-lifecycle §2.1). Strings (not an Enum) so they
# round-trip cleanly through tick summaries / surfaced diagnostics and
# stay trivially comparable in tests.
WORKER_STATE_CURRENT = "current"
WORKER_STATE_STALE = "stale"
WORKER_STATE_MISSING = "missing"

# _apply_sentinel outcomes (DESIGN §3 sentinel-path readers S4). The
# caller gates release/forget/handoff-clear on `applied`; `skipped_stale`
# performs NONE of those (consumed, deliberate no-op); `error` re-queues
# (the watermark already advanced past the only durable record of the
# transition, so a returned `error` must not silently consume it).
SENTINEL_APPLIED = "applied"
SENTINEL_SKIPPED_STALE = "skipped_stale"
SENTINEL_ERROR = "error"


def read_current_worker_state(
    project: str,
    slug: str,
    dispatch_generation: int,
    *,
    home: Path | None = None,
) -> tuple[str, dict | None]:
    """The chokepoint reader (DESIGN §2.1): classify workers/<slug>/
    state.json against the slug's AUTHORITATIVE task-row
    dispatch_generation, returning (classification, state-or-None).

        s = load state.json
        if s is absent:                           -> missing
        if s.dispatch_generation == authority:    -> current
        else (PRIOR generation):                  -> stale

    The load-bearing distinction is ``stale`` != ``missing``:

      - ``current`` — state present, generation matches the task-row
        authority. The caller proceeds as normal (the live attempt).
      - ``stale``  — state present but stamped with a PRIOR generation.
        It is a previous attempt's inert leftover. The caller MUST
        short-circuit for this slug this tick: NO status mutation, NO
        clear_worker, NO worktree removal, NO nudge/escalate/block —
        only surface. (Treating a prior-generation state as ``missing``
        was the recurring bug: the died-without-PR branch then removed
        the CURRENT attempt's worktree + worker dir.)
      - ``missing`` — no state file at all (a genuinely absent state, no
        live attempt). ONLY ``missing`` keeps the existing "worker died
        without PR" semantics.

    A state with NO ``dispatch_generation`` key reads as 0 (legacy /
    pre-migration). When the authority is also 0 (the slug was never
    re-dispatched under the new epoch), legacy state compares
    ``current``; once the slug is re-dispatched (authority >= 1) the
    legacy 0 compares ``stale`` and is fenced out — fail-safe, matching
    the §3 tokenless-legacy rule.

    Returns the raw state dict alongside the classification so callers
    that act on ``current`` don't re-read the file (one read per gate).
    """
    st = _read_worker_state(project, slug, home=home)
    if st is None:
        return (WORKER_STATE_MISSING, None)
    try:
        state_gen = int(st.get("dispatch_generation", 0) or 0)
    except (TypeError, ValueError):
        # A non-int generation on disk is corrupt; treat as legacy 0 so
        # it fences out the moment the slug is re-dispatched (authority
        # >= 1) rather than ever masquerading as the current attempt.
        state_gen = 0
    if state_gen == dispatch_generation:
        return (WORKER_STATE_CURRENT, st)
    return (WORKER_STATE_STALE, st)


def _bump_tick_counter(state: dict) -> bool:
    """Bump state["tick_count"] (mutated in place) and return True when
    this tick should publish a rolling checkpoint (#187).

    Split from the file write so the caller can persist the bumped counter
    via the heartbeat _save_coord_state, then DEFER the actual checkpoint
    file write to the end of the tick (after the supervisor loop, which
    can dispatch new workers mid-tick). Tolerates a corrupt / non-int /
    negative counter by resetting to 0 before the +1 bump.

    Codex iter-7 [P1]: a "checkpoint due" latch (state["checkpoint_due"])
    survives the bump→heartbeat→deferred-write window. When an interval
    tick lands we set the latch BEFORE the heartbeat persists it; the
    write clears it only on success (_write_rolling_checkpoint_file).
    If the write fails / the coord crashes in that window, the latch
    stays True on disk, so the NEXT tick retries the write even though
    the modulo no longer matches. Without the latch the missed checkpoint
    would never be retried and a stale coord-checkpoint.md could shadow
    synth.go's coord-state walk, dropping workers dispatched that tick.
    """
    raw_tick = state.get("tick_count", 0)
    try:
        tick_count = int(raw_tick)
    except (TypeError, ValueError):
        tick_count = 0
    if tick_count < 0:
        tick_count = 0
    tick_count += 1
    state["tick_count"] = tick_count

    every = dispatch_mod.resolve_checkpoint_every()
    # Codex iter-8 [P2]: honor the disable kill-switch even with a stale
    # latch. FLEET_COORD_CHECKPOINT_EVERY=0 means "no checkpoints"; a
    # leftover checkpoint_due from before the operator disabled it must
    # NOT keep forcing (failing) writes every tick. Clear the latch and
    # bail when disabled.
    if every <= 0:
        if state.get("checkpoint_due"):
            state["checkpoint_due"] = False
        return False

    due_now = dispatch_mod.should_checkpoint(tick_count, every)
    # A latch left over from a prior tick whose write never completed.
    pending = bool(state.get("checkpoint_due"))
    if due_now:
        # Latch it durably so a crash before the deferred write retries
        # next tick.
        state["checkpoint_due"] = True
    return due_now or pending


def _write_rolling_checkpoint_file(
    *,
    project: str,
    project_dir: Path,
    coord_id: str,
    state: dict,
    home: Path,
) -> str | None:
    """Publish coord-checkpoint.md (the rolling recovery snapshot, #187).

    Caller is responsible for the counter bump + interval gate (see
    _bump_tick_counter) and for invoking this only on a checkpoint tick.
    Returns the checkpoint path written.

    Always RE-READS tasks.md from disk so the snapshot reflects every
    mutation that landed this tick — including workers the supervisor
    dispatched after the heartbeat — not a pre-dispatch / pre-supervisor
    view. (A crash after a supervisor dispatch must not leave a fresher-
    but-stale checkpoint that shadows synth.go's coord-state walk.)

    Payload derivation — the checkpoint mirrors a handoff doc's two
    live-work sections so synth.go lifts the rows verbatim:
      - active_subagents: every ACTIVE task — both `in-progress` (worker
        running, may not have a PR yet) and `in-review` (PR open, shepherd
        running) — enriched with the worker phase (workers/<slug>/
        state.json) and the slug→id maps from coord_state. This mirrors
        the handoff recovery contract (handoff.go: pr_url + in-review →
        re-spawn shepherd; empty pr_url + in-progress → re-dispatch). A
        checkpoint that dropped in-review tasks would, because synth.go
        prefers a fresher checkpoint over the state walk, strand every
        open-review PR's monitor after a coord crash.
      - open_prs: the subset of those tasks that carry a pr_url.
    Recent decisions ride along from state["recent_decisions"]
    (populated by record_checkpoint_decision at dispatch sites).

    dispatch.py owns write_coord_checkpoint + the env knobs; this helper
    is the loop.py-side glue.
    """
    agent_ids = supervisor_mod.load_agent_id_map(state)
    subagent_ids = supervisor_mod.load_subagent_id_map(state)

    # Re-read tasks.md from disk so a task dispatched THIS tick (flipped
    # to in-progress by _apply_dispatch / the supervisor AFTER the
    # caller's snapshot) lands in the checkpoint.
    #
    # On a parse error (corrupt tasks.md) we have two recovery-safe moves,
    # chosen by whether coord-state still tracks any worker:
    #   - worker_agent_ids NON-EMPTY (codex iter-9 [P2]): fall back to an
    #     empty task list and build a coord-state-only snapshot from
    #     worker_agent_ids + worker state.json (iter-5) — the same data
    #     synth.go's state walk recovers, just without the tasks.md
    #     status/pr_url overlay. Fresher than a possibly-stale prior
    #     checkpoint, and never empty.
    #   - worker_agent_ids EMPTY (codex iter-3 [P2]): RAISE so the
    #     call-site logs + leaves the PREVIOUS checkpoint intact. Writing
    #     an empty checkpoint here would, because synth.go prefers a
    #     fresher checkpoint, hide any worker the prior checkpoint still
    #     records. Nothing to snapshot + don't clobber good recovery data.
    # tasks.md corruption is rare (atomic tmp+rename → readers see a whole
    # old-or-new file), so the missing overlay is an acceptable degradation.
    tasks_path = project_dir / "tasks.md"
    try:
        tasks = parse.read(str(tasks_path)).tasks
    except Exception:  # noqa: BLE001
        if not agent_ids:
            raise  # nothing to snapshot; preserve the prior checkpoint
        tasks = []

    tasks_by_slug = {t.slug: t for t in tasks}

    # Codex iter-5 [P1]: drive the active rows from worker_agent_ids (the
    # SAME source synth.go's state walk uses), then overlay tasks.md
    # metadata. tasks.md alone is not the source of truth: a slug can live
    # in coord-state.json's worker_agent_ids while absent from tasks.md
    # (coord remembered the agent_id but crashed before stamping the task,
    # or tasks.md is missing/partial). synth's walk would still recover
    # such a worker from worker_agent_ids + worker state.json; a checkpoint
    # built from tasks.md only would drop it, and because synth prefers the
    # fresher checkpoint, recovery would lose that in-flight worker.
    #
    # Union = worker_agent_ids slugs (the recovery driver) ∪ active
    # tasks.md slugs (covers a task flipped to in-progress THIS tick before
    # its agent_id is remembered). Sorted for deterministic doc output.
    candidate_slugs = set(agent_ids)
    for t in tasks:
        if t.status in ("in-progress", "in-review"):
            candidate_slugs.add(t.slug)

    active_subagents: list[dict] = []
    open_prs: list[dict] = []
    pr_seq = 0
    for slug in sorted(candidate_slugs):
        t = tasks_by_slug.get(slug)
        # Mirror synth.go's state walk: skip a worker_agent_ids slug whose
        # worker dir is gone AND which isn't an active tasks.md row — it's
        # archived / hand-deleted / never existed, not recoverable.
        st = _read_worker_state(project, slug, home=home)
        st = st if isinstance(st, dict) else {}
        if not st and (t is None or t.status not in ("in-progress", "in-review")):
            continue
        phase = st.get("phase", "")
        status = t.status if t is not None else ""
        branch = (t.branch if t is not None and t.branch else f"worker/{slug}")
        # Codex iter-4 [P1]: prefer tasks.md's pr_url (authoritative once
        # the reconcile stamps it) but fall back to workers/<slug>/
        # state.json's pr_url for the window where the worker has opened a
        # PR but tasks.md isn't stamped yet. synth.go's state walk reads
        # pr_url from worker state, so a checkpoint that only read tasks.md
        # would — because synth prefers the fresher checkpoint — lose the
        # PR and strand its shepherd after a crash in that window.
        task_pr = t.pr_url if t is not None else ""
        state_pr = st.get("pr_url", "") if isinstance(st.get("pr_url"), str) else ""
        pr_url = task_pr or state_pr
        # Codex iter-8 [P2]: if the PR url came from the worker-state
        # FALLBACK (tasks.md not stamped yet → task_pr empty, state_pr
        # set) the task is effectively in-review — a PR is open. Stamp the
        # row status as in-review so handoff_resume treats it shepherd-only
        # (in _NON_REDISPATCH_STATUSES) rather than re-dispatching a worker
        # against an already-open PR. tasks.md's own status (once stamped)
        # always wins; we only synthesize in-review for the unstamped
        # window where status would otherwise be in-progress + pr_url set.
        if not task_pr and state_pr and status in ("", "in-progress"):
            status = "in-review"
        active_subagents.append({
            "task": slug,
            "branch": branch,
            "phase": phase,
            "status": status,
            "pr_url": pr_url,
            "agent_id": agent_ids.get(slug, ""),
            "subagent_id": subagent_ids.get(slug, ""),
        })
        if pr_url:
            # We only know the URL + head branch here; the real gh PR
            # number is unknown (the PR monitor owns gh state). Render a
            # 1-based ordinal so the bullet stays well-formed for synth.go's
            # `- #<n> ...` parser — synth lifts head/url for recovery, the
            # ordinal is just for the human-readable doc, not gh reconcile.
            pr_seq += 1
            open_prs.append({
                "number": pr_seq,
                "title": slug,
                "head": branch,
                "url": pr_url,
            })

    return dispatch_mod.write_coord_checkpoint(
        project_dir=project_dir,
        coord_id=coord_id,
        project=project,
        state=state,
        active_subagents=active_subagents,
        open_prs=open_prs,
    )


# Phases that prove a WORKER (not the dispatch bootstrap) authored the
# state.json. _apply_dispatch bootstraps phase="starting" BEFORE the
# Agent runs; any phase past that is a worker-authored advance. Used by
# residual-crash repair to tell a live-but-unregistered worker (skipped
# best-effort register_subagent) apart from a true phantom that crashed
# before the Agent invoke.
_WORKER_AUTHORED_PHASES = frozenset({
    "branch", "tdd-red", "tdd-green", "tdd-refactor", "review-pending",
    "review-claude", "review-codex", "review-done", "push", "done",
})


def _worker_launch_looks_live(
    project: str, slug: str, *, home: Path | None = None,
    since_unix: float | None = None,
) -> bool:
    """True if workers/<slug>/state.json shows a WORKER-authored progress
    phase whose update landed AFTER `since_unix` — evidence the Agent for
    THIS dispatch genuinely launched and is running, even though
    register_subagent (best-effort) may not have recorded a subagent_id.

    Conservative on purpose:
      - Only a phase the bootstrap never writes counts (the dispatch
        bootstrap sets phase="starting"; a real worker advances to
        branch/tdd-*/review-* etc.). A phantom that crashed right after
        _apply_dispatch reads "starting" → not live.
      - Codex iter-3 [P1]: when `since_unix` is given (this journal's
        launch_attempted_at), the worker's state.json `updated_at` MUST
        be strictly newer. A reviewer/finisher handoff reuses the slug
        but mints a new agent_id; the prior stage already left a
        review-pending / review-done state.json BEFORE the new launch
        flip. Requiring updated_at > launch_attempted_at excludes that
        stale prior-stage state so a handoff that crashed before the
        Agent invoke is correctly seen as a phantom (escalated), not
        mistaken for the still-present prior subagent.

    Missing/unreadable state, or a bootstrap/prior phase, or a stale
    timestamp → not live."""
    st = _read_worker_state(project, slug, home=home)
    if st is None:
        return False
    if st.get("phase", "") not in _WORKER_AUTHORED_PHASES:
        return False
    if since_unix is None:
        return True
    updated = _parse_iso_utc(st.get("updated_at", ""))
    if updated is None:
        # No usable worker timestamp → can't prove the write is for THIS
        # dispatch; treat as not-live (the phantom path escalates, which
        # is safe — it never auto-releases).
        return False
    return updated > since_unix


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


# ---------- branch->PR fallback for stuck-without-pr_url reconcile ----------
#
# Recovery net for the case where a worker shipped a PR outside the
# v0.2 state-machine contract (off-rails finisher; operator-driven
# manual merge). state.json is stuck at a non-terminal phase
# (typically review-pending), tasks.md.pr_url is empty, but a merged
# (or open) PR exists on the worker's branch. Without this lookup the
# reconcile path has no signal and the task stays in-progress forever.
#
# See docs/DESIGN-reconcile-pr-by-branch.md v2 — codex round 1 tightened
# the lookup contract: `--head <branch>` (not `--search head:<branch>`),
# `--limit 10` (not 1), newest-by-createdAt wins. The 10-PR cap is a
# safety bound; the same branch can have multiple PRs if a retry
# reopened a stale branch, and `--limit 1` would non-deterministically
# pick the wrong one.


@dataclass
class _PRSummary:
    """Minimal projection of a `gh pr list` row.

    Returned by _gh_pr_by_branch. The caller (reconcile fallback) only
    needs the four fields below to decide:
      - merged_at non-None → flip task to done (PR shipped).
      - state == "OPEN"    → flip task to in-review.
      - state == "CLOSED" with merged_at None → no action (operator
        decides; never auto-requeue).

    state mirrors GitHub's enum: OPEN | CLOSED | MERGED.
    """
    number: int
    state: str
    url: str
    merged_at: str | None
    created_at: str


def _gh_pr_by_branch(branch: str, timeout_s: float = 5.0) -> _PRSummary | None:
    """Look up the newest PR whose head ref matches `branch`.

    Used by the reconcile fallback to recover tasks whose pr_url was
    never recorded. Mirrors _probe_branch_prs() but returns a richer
    projection (state + mergedAt + url) and applies newest-by-createdAt
    tiebreaking explicitly.

    Returns None on any failure (gh missing, timeout, non-zero exit,
    empty result, parse error). NEVER raises; an unavailable GitHub
    must not requeue a stale handoff.

    Cost: one gh shell-out (~200ms typical), bounded to `timeout_s`.
    """
    if not branch:
        return None
    cmd = [
        "gh", "pr", "list",
        "--head", branch,
        "--state", "all",
        "--json", "number,state,url,mergedAt,createdAt,updatedAt,headRefName",
        "--limit", "10",
    ]
    data, err = _gh_run_json(cmd, timeout_s=timeout_s)
    if err or not isinstance(data, list) or not data:
        return None
    # Pick the newest by createdAt; tie-break on highest `number`. Both
    # fields are GitHub-controlled; the same numeric ordering applies
    # to ISO-8601 strings so a simple max() with a tuple key works.
    best: dict | None = None
    best_key: tuple[str, int] = ("", 0)
    for entry in data:
        if not isinstance(entry, dict):
            continue
        created = str(entry.get("createdAt", "") or "")
        num = entry.get("number")
        if not isinstance(num, int):
            continue
        key = (created, num)
        if best is None or key > best_key:
            best = entry
            best_key = key
    if best is None:
        return None
    return _PRSummary(
        number=int(best.get("number", 0)),
        state=str(best.get("state", "") or ""),
        url=str(best.get("url", "") or ""),
        merged_at=(str(best["mergedAt"]) if best.get("mergedAt") else None),
        created_at=str(best.get("createdAt", "") or ""),
    )


def _branch_pr_fallback_action(
    t: parse.Task,
    *,
    min_pr_created_at: str = "",
) -> _ReconcileAction | None:
    """Branch->PR recovery action for stuck tasks. Returns None if no
    fallback applies (no branch, pr_url already set, gh unavailable,
    PR not found, PR closed-unmerged, or PR too old).

    Callers MUST run this AFTER honoring any fresh terminal state in
    state.json (phase=failed / phase=blocked / phase=done with pr_url).
    Otherwise a re-dispatched worker that died with phase=failed could
    be classified from a stale PR left by a prior attempt on the SAME
    branch — flipping the task to in-review/done and masking the fresh
    terminal failure signal (codex review round 1 [P1]).

    `min_pr_created_at` is the per-attempt epoch (ISO 8601 string) —
    typically state.json.started_at. When set, the helper requires
    `pr.created_at >= min_pr_created_at` so a stale PR opened by a
    prior attempt on the SAME branch never wins (codex review round 5
    [P1]). When empty, no provenance gate is applied — caller is
    responsible for proving freshness another way.
    """
    if t.pr_url:
        return None
    if not t.branch:
        return None
    pr = _gh_pr_by_branch(t.branch)
    if pr is None:
        return None
    # Per-attempt provenance gate: if the caller supplied a per-attempt
    # epoch (state.json.started_at), require the PR to be at least as
    # new. Stale PRs from prior attempts on the same reused branch
    # would otherwise be misattributed to the current worker.
    #
    # String comparison is correct here: ISO 8601 UTC timestamps sort
    # lexicographically the same as chronologically. Both fields are
    # GitHub-controlled / Go-time.Now-controlled.
    if min_pr_created_at and pr.created_at < min_pr_created_at:
        return None
    if pr.merged_at:
        # PR merged — flip to done. delete_worker_dir so the now-junk
        # dir doesn't accumulate. Raise to operator so they see the
        # recovery in `fleet status`.
        return _ReconcileAction(
            slug=t.slug, new_status="done",
            set_pr_url=pr.url,
            clear_worker=True,
            delete_worker_dir=True,
            note=f"PR {pr.url} merged outside state machine",
            raised_to_user=True,
            raise_text=f"reconcile recovered {t.slug}: {pr.url}",
        )
    if pr.state == "OPEN":
        # PR open — flip to in-review. The next tick's gh pr checks
        # path drives CI -> done. Keep the worker dir (operator may
        # want to re-dispatch a fix off the same checkout); only
        # clear worker_pid.
        return _ReconcileAction(
            slug=t.slug, new_status="in-review",
            set_pr_url=pr.url,
            clear_worker=True,
            note=f"PR {pr.url} open; recovered from branch",
            raised_to_user=True,
            raise_text=f"reconcile recovered {t.slug}: {pr.url}",
        )
    # CLOSED-unmerged: operator decides. Leaving the task untouched is
    # the safe call — auto-requeue would lose the worker's history;
    # auto-archive would mask the decision the operator still needs to
    # make.
    return None


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


def _dispatch_wedge_recoverable(
    slug: str,
    project: str,
    home: Path | None,
    coord_state: dict | None,
) -> bool:
    """True iff `slug` is a dispatch PARTIAL-APPLY WEDGE safe to requeue.

    The wedge: _apply_dispatch crashed AFTER the status=in-progress +
    dispatch_generation commit but BEFORE the state.json bootstrap. The
    DISPATCH block is collected by the caller only AFTER _apply_dispatch
    RETURNS (loop.py ~1041), so the worker was NEVER launched — the
    adopted journal is therefore still `pending` (never launch_attempted /
    acked). And the id is still in pending_acquire (forget runs only after
    _apply_dispatch succeeds), so the journal REPLAY skips it
    (_replay_pending_dispatches) and the dispatch-retry path never fires
    (it only picks status=ready). Nobody recovers → wedged in-progress.

    Requeue is SAFE only under ALL of:
      - the slug has an adopted agent_id,
      - that id is in pending_acquire (replay will NOT re-emit it),
      - its journal exists AND is `pending` (the worker was never launched
        — a launch_attempted / acked journal means a worker DID launch, so
        requeuing would double-dispatch; defer to replay's residual-crash
        repair / the live worker's own terminal write instead).

    A False result (no adopted id, not pending_acquire, journal absent /
    non-pending) means reconcile must NOT requeue — replay or a live
    worker owns recovery. Conservative: any ambiguity → False (no requeue).
    """
    if coord_state is None or home is None:
        return False
    try:
        adopted = supervisor_mod.load_agent_id_map(coord_state)
    except Exception:  # noqa: BLE001
        return False
    agent_id = adopted.get(slug, "")
    if not agent_id:
        return False
    try:
        pending = set(
            supervisor_mod.load_pending_acquire_agent_id_map(coord_state).values()
        )
    except Exception:  # noqa: BLE001
        pending = set()
    if agent_id not in pending:
        # NOT a partial-apply wedge: a replay-eligible (applied) id, or an
        # untracked id. Replay / the live worker owns recovery — never
        # requeue (that is the #184 double-dispatch trap).
        return False
    # The id is mid-application. Confirm the journal is `pending` (worker
    # never launched). A launch_attempted / acked / terminal journal means
    # a worker DID launch — do NOT requeue.
    for jid, jslug, j in _iter_project_journals(home, project):
        if jid == agent_id and jslug == slug:
            return j.get("exec_state", "") == "pending"
    # No journal for the adopted id: cannot prove the worker never
    # launched → fail safe (no requeue).
    return False


def _reconcile_inflight(
    tasks: list[parse.Task],
    project: str,
    fleet_bin: str,
    *,
    home: Path | None = None,
    coord_state: dict | None = None,
) -> list[_ReconcileAction]:
    """For each in-flight task, check the worker is alive; otherwise
    decide the next status from state.json's terminal phase, then
    pr_url + CI.

    Returns a list of _ReconcileAction; caller applies via the fleet CLI.

    `coord_state` (optional) enables partial-apply WEDGE recovery: an
    in-progress slug whose state.json is stale/missing, has no live
    worker, AND is NOT replay-recoverable (no pending journal the replay
    would re-emit) is requeued to `todo` instead of being left wedged
    forever (codex [P1] — _apply_dispatch can crash after the
    status=in-progress + dispatch_generation commit but before the
    state.json bootstrap; replay SKIPS such a slug because its id sits in
    pending_acquire). When coord_state is None (legacy callers / tests)
    the recovery is inert and behavior is unchanged.
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
        # R1-R4 chokepoint (DESIGN §2.1/§3): classify the on-disk state
        # against the slug's AUTHORITATIVE task-row dispatch_generation
        # FIRST — BEFORE the liveness check (R1) and the decision tree
        # (R2 terminal-state, R3 mid-phase, R4 died-without-PR — the
        # highest-severity case). Generation must gate liveness too:
        # codex iter-2 [P1] — a re-dispatched slug whose PRIOR attempt
        # left a stale state.json with a fresh non-terminal phase (or no
        # updated_at) makes `_is_worker_alive` return True off the stale
        # file. If the current attempt hasn't bootstrapped its own state
        # yet, that stale file would otherwise suppress reconcile FOREVER
        # (the live attempt never re-evaluated). So `stale` short-circuits
        # the WHOLE per-task pass (no liveness trust, no mutation, no
        # clear_worker / delete_worker_dir / worktree removal) and
        # surfaces; the current attempt is reconciled on a later tick once
        # it writes a current-generation state. `current`/`missing` fall
        # through to the liveness check + the existing tree (`missing`
        # keeps died-without-PR semantics; `current` reads the live
        # attempt's terminal/mid-phase signals below).
        recon_cls, _ = read_current_worker_state(
            project, t.slug, int(t.dispatch_generation), home=home,
        )
        if recon_cls == WORKER_STATE_STALE:
            import sys
            # codex iter-4 [P1]: a `stale` state on an in-progress task can
            # be either (a) a prior attempt's leftover while the current
            # attempt is mid-relaunch, or (b) a dispatch partial-apply (gen
            # bump landed, `starting` bootstrap didn't). The correct owner
            # of re-launch is NORMALLY the #184 dispatch journal replay
            # (_replay_pending_dispatches re-emits the DISPATCH when
            # worker_agent_ids[slug] is the adopted PENDING journal id),
            # which makes the worker write a current-generation state.
            # Reconcile must NOT blindly requeue an in-progress task with a
            # live adopted journal — that is the double-dispatch trap #184
            # closed.
            #
            # BUT replay does NOT cover every partial-apply: when
            # _apply_dispatch crashes after the status=in-progress + gen
            # commit but before the state.json bootstrap, the id is still
            # in pending_acquire (forget runs only AFTER _apply_dispatch
            # returns), and replay SKIPS pending_acquire ids — so NOBODY
            # re-emits and the task is WEDGED in-progress forever (codex
            # [P1]). _dispatch_wedge_recoverable proves the worker was
            # NEVER launched (adopted id in pending_acquire + its journal
            # still `pending`); only THEN is requeue-to-`todo` safe (a
            # launch_attempted / live worker is left to replay's
            # residual-crash repair, never requeued — that is the #184
            # double-dispatch trap). The next dispatch increments the gen
            # → the prior stale state still fences `stale` (monotonic, no
            # reuse). Inert unless coord_state is wired.
            if _dispatch_wedge_recoverable(
                t.slug, project, home, coord_state,
            ):
                print(
                    f"coord: reconcile RECOVERING wedged {t.slug} — stale "
                    f"worker state (authority={int(t.dispatch_generation)}) "
                    f"with no replay owner (dispatch partial-apply); "
                    f"requeue to todo. The next dispatch increments the "
                    f"generation so the prior attempt's state stays stale.",
                    file=sys.stderr,
                )
                actions.append(_ReconcileAction(
                    slug=t.slug, new_status="todo", clear_worker=True,
                    note="dispatch partial-apply wedge — requeued (no PR)",
                    raised_to_user=True,
                    raise_text=f"{t.slug} recovered from dispatch wedge",
                    delete_worker_dir=True,
                ))
                continue
            print(
                f"coord: reconcile skipped {t.slug} — stale worker state "
                f"(prior dispatch_generation, authority="
                f"{int(t.dispatch_generation)}); no mutation. Re-launch (if "
                f"the current attempt's bootstrap was lost) is owned by the "
                f"dispatch-journal replay, not reconcile.",
                file=sys.stderr,
            )
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
                # Stuck-handoff recovery: state.json frozen at review-
                # pending/review-done (handoff chain went off-rails)
                # but the worker may have shipped a PR outside the
                # v0.2 state machine. Try branch->PR lookup before
                # the short-circuit — without this, a merged PR on
                # the worker's branch stays invisible to reconcile
                # forever (rc-listener-impl-v0-12-ed95 spent ~4h
                # stuck on this). See DESIGN-reconcile-pr-by-branch.md
                # v2 §Design Part A; codex review round 1 [P1] moved
                # this check to AFTER any fresh terminal state below
                # (phase=failed / phase=blocked / phase=done with
                # pr_url).
                #
                # Codex review round 5 [P1]: pass state.json.started_at
                # as a per-attempt epoch. If a prior attempt opened a
                # PR on the SAME worker/<slug> branch and the new
                # worker reached review-pending/review-done without
                # opening its own PR, the stale PR would otherwise
                # win. The provenance gate (pr.created_at >=
                # started_at) keeps the fallback from misattributing
                # a prior-attempt PR to the current worker.
                started_at = str(mid_phase.get("started_at", "") or "")
                action = _branch_pr_fallback_action(
                    t, min_pr_created_at=started_at,
                )
                if action is not None:
                    actions.append(action)
                    continue
                # Codex iter-6 [P1] partial-apply recovery: if a prior
                # tick's _apply_reconcile crashed between the set_pr_url
                # write and the status= write, tasks.md is left at
                # status=in-progress WITH a durable pr_url, while
                # state.json is still review-pending/review-done. The
                # fallback helper short-circuits when t.pr_url is set
                # (returns None), so a naked `continue` here would skip
                # the pr_url+CI poll below and leave the task stuck
                # forever. When the fallback action is None AND
                # t.pr_url is already present, fall through so the
                # `if t.pr_url:` block can drive CI -> done/rebase.
                # When t.pr_url is empty (no recovery available, fresh
                # stuck-handoff with no PR yet), keep the short-circuit
                # — falling through would mis-classify it as "worker
                # died without PR" and requeue to todo.
                if not t.pr_url:
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
        # SITE 2 of the branch->PR fallback was REMOVED in codex review
        # round 3 [P1]: when a retried task reuses worker/<slug> after
        # a CI-red attempt, the branch can still carry an older PR. If
        # the new worker dies BEFORE writing state.json (and thus
        # neither mid_phase nor terminal-state above produces a signal),
        # the fallback at this site would have picked up the stale PR
        # and flipped the fresh failure back to in-review/done —
        # masking the operator's need to re-dispatch.
        #
        # The original load-bearing case (rc-listener-impl-v0-12-ed95
        # stuck at phase=review-pending) is still handled by SITE 1
        # inside the mid_phase short-circuit — state.json is present
        # there (just frozen), which proves the worker reached a
        # PR-creating phase. SITE 2's purpose was to also catch the
        # in-review-with-empty-pr_url case, but without a per-attempt
        # epoch we can't tell which PR (if any) belongs to the current
        # attempt. The conservative choice — fall through to the
        # "worker died without PR" requeue — lets the operator
        # explicitly own that recovery.
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


# ---------- terminal release wrapper (PR1 dispatch-lifecycle) ----------
#
#  forget_agent_id (state)        coord_prompt_inbox release flow
#  -----------------------        -------------------------------
#       │                                  │
#       ▼                                  ▼
#   slug → agent_id           ┌───────────────────────────┐
#   mapping cleared           │ dispatch.release_coord_   │
#       │                     │ prompt_inbox via          │
#       │                     │ `fleet claims release`    │
#       │                     │ (best-effort, no raise)   │
#       ▼                     └────────────┬──────────────┘
#   _apply_reconcile/                       │
#   _apply_sentinel terminal              outcome: released |
#   branches → status flip                already_released |
#                                          absent | not_owned |
#                                          error
#                                            │
#                                            ▼
#                                   stderr log on non-success.
#                                   Never blocks status flip.
#
# Ordering rule: release runs BEFORE forget_agent_id so we still know
# the agent_id at the moment the release fires. The supervisor's two
# paths (primary tick + supervisor loop) and the handoff path all
# share the same wrapper to keep the contract uniform.


def _release_coord_prompt_inbox(
    *,
    slug: str,
    agent_id: str,
    fleet_bin: str,
    fleet_home: Path | str | None,
    site: str,
) -> str:
    """Best-effort release of a worker's coord_prompt_inbox claim.

    Wraps `dispatch_mod.release_coord_prompt_inbox` so terminal-
    transition sites (primary tick reconcile/drain, supervisor
    reconcile/drain, replay-deferred drain, handoff) share one
    invocation shape. Never raises; logs non-success outcomes to
    stderr with the `site` tag so post-mortem analysis can correlate
    a leaked inbox to the path that should have released it.

    No-op (silently) when agent_id is empty — the caller may not have
    a mapping (e.g., a sentinel from a pre-PR1 worker that never went
    through the acquire-prompt path). Returns "" in the no-op case.

    Returns the outcome string ("released", "already_released",
    "absent", "not_owned", "error", or "" when skipped) so callers
    can decide whether to forget the agent_id mapping. Codex iter-7
    [P1]: a transient `error` outcome must NOT trigger forget — the
    mapping is the only handle the next reconcile has for retrying
    the release; dropping it permanently leaks the claim.

    `fleet_home` accepts a Path or str (loop.py uses Path internally,
    dispatch_mod expects a string env var) — we coerce here.
    """
    if not agent_id:
        return ""
    home_arg: str | None
    if fleet_home is None:
        home_arg = None
    else:
        home_arg = str(fleet_home)
    try:
        response = dispatch_mod.release_coord_prompt_inbox(
            agent_id,
            fleet_bin=fleet_bin,
            fleet_home=home_arg,
        )
    except Exception as exc:  # noqa: BLE001
        # The helper is documented as never-raising, but a programmer
        # error (e.g., a future refactor accidentally re-raising) must
        # not crash the terminal-transition path.
        import sys
        print(
            f"coord: release coord_prompt_inbox crashed at {site} "
            f"slug={slug} agent_id={agent_id}: {exc}",
            file=sys.stderr,
        )
        return dispatch_mod.RELEASE_OUTCOME_ERROR
    outcome = response.get("outcome", "")
    if outcome in (
        dispatch_mod.RELEASE_OUTCOME_RELEASED,
        dispatch_mod.RELEASE_OUTCOME_ALREADY_RELEASED,
    ):
        return outcome
    # Non-success outcomes: log but don't fail. absent + not_owned are
    # expected in race scenarios; error indicates a fleet-binary fault
    # that the PR4 sweeper will reconcile on its next sweep.
    import sys
    err = response.get("error", "")
    print(
        f"coord: release coord_prompt_inbox non-success at {site} "
        f"slug={slug} agent_id={agent_id} outcome={outcome!r}"
        + (f" error={err}" if err else ""),
        file=sys.stderr,
    )
    return outcome or dispatch_mod.RELEASE_OUTCOME_ERROR


def _retry_pending_releases(
    *,
    project: str,
    fleet_bin: str,
    home: Path,
    coord_state: dict,
) -> None:
    """Retry releases that failed transiently on prior ticks.

    Codex iter-9 [P1]: handoff release errors stash the prior
    agent_id in pending_release_agent_ids (because the new
    dispatch overwrites worker_agent_ids and would lose the
    handle). This retry pass walks the map and tries each release;
    terminal outcomes (released, already_released, absent,
    not_owned) drop the entry. Non-terminal outcomes (error) leave
    the entry for the next tick.

    Codex iter-13 [P1]: SKIP any pending-release id that is
    currently the slug's live worker_agent_ids[slug] OR
    pending_acquire_agent_ids[slug] entry. The scenario: a
    `ready` task with both maps populated has the worker map
    swept; the sweep's release returns error and stashes the
    same id; _dispatch_ready then reuses the pending id and
    repopulates worker_agent_ids with it. The retry pass on the
    next tick would otherwise tear down the live claim. The
    active maps win — drop the pending-release entry instead.

    Best-effort: never raises; logs via _release_coord_prompt_inbox's
    stderr path.
    """
    pending = supervisor_mod.load_pending_release_agent_ids(coord_state)
    if not pending:
        return
    # Codex iter-13 [P1]: skip ids whose slug is currently
    # dispatchable (the worker is/will-be alive). The dangerous
    # scenario the codex finding describes: a `ready` task with both
    # worker_agent_ids AND pending_acquire_agent_ids gets swept; the
    # sweep's release errors and stashes; _dispatch_ready then
    # reuses the same id; the next retry pass would tear down the
    # live worker. We discriminate via TASK STATUS (read fresh from
    # tasks.md), not just the map presence: a stale worker_agent_ids
    # entry after a failed release for a terminal-transitioned slug
    # (status=todo/done/blocked/abandoned) is STILL leaked and
    # should be retried.
    try:
        f = parse.read(
            str(home / "projects" / project / "tasks.md"),
        )
        # Codex iter-16 [P1]: in-review tasks have already exited
        # their worker phase (PR is open, worker is dead). The old
        # coord_prompt_inbox claim must be reclaimable here. Only
        # `ready` and `in-progress` count as "live worker that we
        # must not tear down by mistake".
        live_statuses = {
            t.slug: t.status for t in f.tasks
            if t.status in ("ready", "in-progress")
        }
    except Exception:  # noqa: BLE001
        # On parse error, fall back to map-presence check (more
        # conservative — match iter-13's original semantics).
        active_worker = supervisor_mod.load_agent_id_map(coord_state)
        active_pending = supervisor_mod.load_pending_acquire_agent_id_map(
            coord_state,
        )
        live_statuses = {}
        for s in list(pending.keys()):
            if (
                active_worker.get(s) in pending[s]
                or active_pending.get(s) in pending[s]
            ):
                live_statuses[s] = "in-progress"  # placeholder
    active_worker = supervisor_mod.load_agent_id_map(coord_state)
    active_pending = supervisor_mod.load_pending_acquire_agent_id_map(
        coord_state,
    )
    for slug, ids in pending.items():
        for agent_id in list(ids):
            # Skip when the slug is in a live (dispatchable / in-flight)
            # state AND the id is currently the active worker/pending
            # entry. Both conditions must hold so a stale
            # worker_agent_ids entry on a terminal slug still gets
            # its retry.
            if slug in live_statuses and (
                active_worker.get(slug) == agent_id
                or active_pending.get(slug) == agent_id
            ):
                supervisor_mod.forget_pending_release_agent_id(
                    coord_state, slug, agent_id,
                )
                continue
            outcome = _release_coord_prompt_inbox(
                slug=slug,
                agent_id=agent_id,
                fleet_bin=fleet_bin,
                fleet_home=home,
                site=f"retry-pending-release agent={agent_id}",
            )
            if _release_outcome_is_terminal(outcome):
                supervisor_mod.forget_pending_release_agent_id(
                    coord_state, slug, agent_id,
                )


def _release_outcome_is_terminal(outcome: str) -> bool:
    """Codex iter-7 [P1]: only treat these outcomes as final so
    forget_agent_id is safe.

    released         — happy path; claim torn down.
    already_released — idempotent success.
    absent           — no claim or journal on disk. The mapping is
                       stale; safe to forget.
    not_owned        — another host owns the claim. Keeping the
                       mapping would mislead our supervisor into
                       addressing a worker we don't control; safe to
                       forget on our side.

    `error` and empty string are NOT terminal — keep the mapping so
    the next sweep / reconcile can retry the release.
    """
    return outcome in (
        dispatch_mod.RELEASE_OUTCOME_RELEASED,
        dispatch_mod.RELEASE_OUTCOME_ALREADY_RELEASED,
        dispatch_mod.RELEASE_OUTCOME_ABSENT,
        dispatch_mod.RELEASE_OUTCOME_NOT_OWNED,
    )


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
    #
    # Within the tasks.md mutations themselves, the order is:
    #   1. clear_pr_url  — durable before the rest.
    #   2. set_pr_url    — durable before the status flip; otherwise
    #      a crash between status=in-review and pr_url=<url> leaves
    #      the task at in-review with empty pr_url, invisible to both
    #      the gh pr checks path (no URL to poll) AND the fallback
    #      (pr_url is non-empty so the fallback skips). Matches the
    #      _apply_sentinel() task_done_pr ordering at loop.py:3083-3084.
    #   3. new_status    — only after pr_url is durable.
    #   4. clear_worker  — post-transition cleanup.
    #   5. delete_worker_dir — last.
    if action.clear_pr_url:
        _run_fleet([fleet_bin, "tasks", "set", "--project", project, action.slug, "pr_url="])
    if action.set_pr_url:
        _run_fleet([fleet_bin, "tasks", "set", "--project", project, action.slug, f"pr_url={action.set_pr_url}"])
    if action.new_status:
        _run_fleet([fleet_bin, "tasks", "set", "--project", project, action.slug, f"status={action.new_status}"])
    # DESIGN §1/§3 re-dispatch fence floor. A requeue to `todo` makes the
    # slug re-dispatch-eligible; the NEXT dispatch increments
    # dispatch_generation by 1 (loop.py _dispatch_ready: next_gen =
    # current + 1). A LEGACY slug whose first (pre-epoch) attempt ran
    # tokenless carries gen 0, so its re-dispatch would land at gen 1 —
    # COLLIDING with a genuine first epoch dispatch (also gen 1). A stale
    # tokenless TASK_DONE_PR/WORKER_FAILED from that pre-epoch attempt
    # then corroborates as legacy-trusted at authority 1 and reaps the
    # re-dispatched LIVE tree (codex [P1]). Reserve gen 1 EXCLUSIVELY for
    # the first-ever epoch dispatch by flooring the generation to >= 1 on
    # every requeue: a re-dispatch is then ALWAYS gen >= 2, so
    # _sentinel_corroborates can safely keep its `authority <= 1 => trust`
    # rule (preserving the first-attempt tokenless path) while fencing a
    # genuinely re-dispatched slug at >= 2.
    if action.new_status == "todo":
        _floor_dispatch_generation_for_requeue(
            action.slug, project, fleet_bin,
            full_tasks_by_slug=full_tasks_by_slug,
            tasks_by_slug=tasks_by_slug,
            home=home,
        )
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
    # Codex iter-5 [P1]: source archive filename, set by _drain_archive
    # so callers that defer (e.g., reaper-lane gate) can roll the
    # watermark back to before this file rather than losing the event.
    source_file: str = ""
    # DESIGN §3 (sentinel-path readers S1-S5): the dispatch_generation
    # token the worker stamped into a state-mutating sentinel (gen=<n>),
    # or None for a TOKENLESS pre-migration sentinel. The apply path
    # corroborates this against the slug's current task-row authority;
    # mismatch → skipped_stale (no terminal side effects). new_task is
    # never state-mutating so it stays None. Persisted through the
    # deferred-sentinel queue (S5) so a deferred→replayed sentinel
    # corroborates correctly on replay.
    dispatch_generation: int | None = None
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
            # Codex iter-5 [P1]: stamp the source filename so callers
            # that defer (reaper-lane gate) can roll the watermark
            # back to before this file rather than losing the event.
            sentinel.source_file = name
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
        # DESIGN §3: extract the optional gen=<n> token (state-mutating
        # sentinels only; new_task has no group). None = tokenless
        # pre-migration sentinel → the tokenless-legacy corroboration
        # path in _apply_sentinel.
        gen: int | None = None
        if "gen" in m.groupdict() and m.group("gen") is not None:
            try:
                gen = int(m.group("gen"))
            except (TypeError, ValueError):
                gen = None
        if kind == "task_done_pr":
            return _SentinelAction(
                slug=slug, kind=kind, payload=m.group("url"),
                dispatch_generation=gen,
            )
        if kind == "blocked_question":
            return _SentinelAction(
                slug=slug, kind=kind, payload=m.group("text"),
                raised_to_user=True,
                raise_text=f"{slug} blocked: {m.group('text')}",
                dispatch_generation=gen,
            )
        if kind == "worker_failed":
            return _SentinelAction(
                slug=slug, kind=kind, payload=m.group("reason"),
                dispatch_generation=gen,
            )
        if kind == "new_task":
            return _SentinelAction(slug=slug, kind=kind)
    return None


def _task_row_dispatch_generation(
    project: str,
    slug: str,
    *,
    home: Path | None = None,
    tasks_by_slug: dict[str, parse.Task] | None = None,
    full_tasks_by_slug: dict[str, parse.Task] | None = None,
) -> int | None:
    """Read the slug's AUTHORITATIVE task-row dispatch_generation (the
    sentinel CAS authority, DESIGN §3). Prefers an in-memory pre-mutation
    snapshot (full_tasks_by_slug, then tasks_by_slug); falls back to
    re-reading tasks.md from disk.

    Returns the int generation, or None when the slug is absent / the
    read fails — the caller fails CLOSED (treats a tokenless sentinel as
    legacy-trusted only when the authority is genuinely 0/absent).
    """
    for lookup in (full_tasks_by_slug, tasks_by_slug):
        if lookup is not None:
            tk = lookup.get(slug)
            if tk is not None:
                try:
                    return int(tk.dispatch_generation)
                except (TypeError, ValueError):
                    return 0
    fleet_home = home if home is not None else _resolve_home(None)
    tasks_path = fleet_home / "projects" / project / "tasks.md"
    try:
        f = parse.read(str(tasks_path))
    except Exception:  # noqa: BLE001
        return None
    for t in f.tasks:
        if t.slug == slug:
            try:
                return int(t.dispatch_generation)
            except (TypeError, ValueError):
                return 0
    return None


def _floor_dispatch_generation_for_requeue(
    slug: str,
    project: str,
    fleet_bin: str,
    *,
    full_tasks_by_slug: dict[str, parse.Task] | None = None,
    tasks_by_slug: dict[str, parse.Task] | None = None,
    home: Path | None = None,
) -> None:
    """Ensure a requeued (re-dispatch-eligible) slug carries
    dispatch_generation >= 1 so the NEXT dispatch lands at gen >= 2.

    DESIGN §1/§3 re-dispatch fence: gen 1 is reserved for the FIRST epoch
    dispatch of a never-dispatched slug. A LEGACY slug (pre-epoch attempt,
    gen 0, tokenless sentinels) being re-dispatched would otherwise also
    land at gen 1 (_dispatch_ready: next_gen = 0 + 1), colliding with a
    true first dispatch and letting a stale tokenless prior-attempt
    sentinel pass _sentinel_corroborates's `authority <= 1` trust window
    and reap the LIVE re-dispatched tree. Flooring the gen to 1 on requeue
    makes every re-dispatch reach >= 2, closing the collision.

    Idempotent: a slug already at gen >= 1 is left untouched (no CLI
    write). gen<=0 (or unreadable) is floored to 1.
    """
    current = _task_row_dispatch_generation(
        project, slug, home=home,
        tasks_by_slug=tasks_by_slug,
        full_tasks_by_slug=full_tasks_by_slug,
    )
    if current is not None and current >= 1:
        return
    try:
        _run_fleet([
            fleet_bin, "tasks", "set", "--project", project, slug,
            "dispatch_generation=1",
        ])
    except Exception as exc:  # noqa: BLE001
        import sys
        print(
            f"coord: dispatch_generation floor failed for {slug}: {exc}",
            file=sys.stderr,
        )


def _sentinel_corroborates(
    action: _SentinelAction, authority: int | None,
) -> bool:
    """DESIGN §3 sentinel generation corroboration. Returns True when the
    sentinel's stamped generation matches the slug's current task-row
    authority (so its terminal side effects may apply), False when it is
    a STALE prior-attempt sentinel that must be skipped.

      - Sentinel carries a token (action.dispatch_generation is not None):
        corroborate by integer equality. Mismatch → stale.
      - Tokenless legacy sentinel (None): legacy-trusted while the slug
        has NOT been RE-dispatched. "Not re-dispatched" means the slug is
        still on its FIRST attempt: authority is absent/unknown, 0 (legacy
        / un-migrated), OR 1 (the first dispatch under the epoch sets
        gen 1 — §1). Only a GENUINE re-dispatch advances the authority to
        >= 2 (each re-dispatch increments by 1), so ONLY authority >= 2
        fences a tokenless sentinel out as STALE. This is the rollout
        path: current emitters (the coord agent following SKILL.md) do
        NOT yet stamp `gen=` on every sentinel, so a first-attempt task's
        tokenless TASK_DONE_PR / WORKER_FAILED must still apply — codex
        iter-3 [P1]. The fail-safe still holds: a re-dispatched slug
        (gen >= 2) never reaps its live tree on a tokenless prior-attempt
        sentinel. The window closes as emitters adopt `gen=`.
    """
    token = action.dispatch_generation
    if token is None:
        # Tokenless-legacy: trusted on the FIRST attempt (authority
        # absent/0/1); fenced once genuinely re-dispatched (>= 2).
        return authority is None or authority <= 1
    # Tokened: strict integer corroboration. A None/absent authority
    # means the task row is gone or unreadable — fail closed (skip).
    if authority is None:
        return False
    return int(token) == int(authority)


def _apply_sentinel(
    action: _SentinelAction,
    project: str,
    fleet_bin: str,
    *,
    repo: str = "",
    tasks_by_slug: dict[str, parse.Task] | None = None,
    home: Path | None = None,
    full_tasks_by_slug: dict[str, parse.Task] | None = None,
) -> str:
    """Apply a parsed sentinel via the fleet CLI. Returns one of
    SENTINEL_APPLIED / SENTINEL_SKIPPED_STALE / SENTINEL_ERROR.

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

    DESIGN §3 (S1-S3): a STATE-MUTATING sentinel (task_done_pr,
    worker_failed, blocked_question) is corroborated against the slug's
    current task-row dispatch_generation. A stale (prior-attempt)
    sentinel — even with an IDENTICAL worker/<slug> branch + deterministic
    path — is SKIPPED: NONE of its terminal side effects run (status
    mutation, pr_url, worktree removal, worker-dir delete) — only
    surface. The caller (S4) gates release/forget/handoff-clear on the
    returned APPLIED outcome. new_task carries no state mutation and is
    never gated.
    """
    # DESIGN §3 (S1/S2/S3): corroborate state-mutating sentinels against
    # the slug's current dispatch_generation before ANY side effect.
    if action.kind in ("task_done_pr", "worker_failed", "blocked_question"):
        authority = _task_row_dispatch_generation(
            project, action.slug, home=home,
            tasks_by_slug=tasks_by_slug,
            full_tasks_by_slug=full_tasks_by_slug,
        )
        if not _sentinel_corroborates(action, authority):
            import sys
            print(
                f"coord: sentinel {action.kind} {action.slug} SKIPPED — "
                f"stale dispatch_generation (sentinel="
                f"{action.dispatch_generation}, authority={authority}); "
                f"all terminal side effects skipped, surfacing",
                file=sys.stderr,
            )
            return SENTINEL_SKIPPED_STALE
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
        return SENTINEL_APPLIED
    elif action.kind == "blocked_question":
        # Lifecycle Waiting — operator may un-block the task; KEEP
        # the worker dir (its blocked_reason is still useful context).
        _run_fleet([fleet_bin, "tasks", "set", "--project", project, action.slug, "status=blocked"])
        if action.payload:
            _run_fleet([fleet_bin, "tasks", "note", "--project", project, action.slug, f"BLOCKED_QUESTION: {action.payload}"])
        return SENTINEL_APPLIED
    elif action.kind == "worker_failed":
        _run_fleet([fleet_bin, "tasks", "set", "--project", project, action.slug, "status=todo"])
        _run_fleet([fleet_bin, "tasks", "set", "--project", project, action.slug, "worker_pid=0"])
        # DESIGN §1/§3 re-dispatch fence floor (see
        # _floor_dispatch_generation_for_requeue): WORKER_FAILED requeues
        # the slug, so the next dispatch must reach gen >= 2 to fence any
        # stale tokenless prior-attempt sentinel out at _sentinel_
        # corroborates. Floor a legacy gen-0 slug to 1 here too.
        _floor_dispatch_generation_for_requeue(
            action.slug, project, fleet_bin,
            full_tasks_by_slug=full_tasks_by_slug,
            tasks_by_slug=tasks_by_slug,
            home=home,
        )
        if action.payload:
            _run_fleet([fleet_bin, "tasks", "note", "--project", project, action.slug, f"WORKER_FAILED: {action.payload}"])
        _maybe_remove_worktree(action.slug, repo, tasks_by_slug, fleet_bin, project)
        # Worker reached TerminalFailure — rm-rf workers/<slug>/.
        _maybe_delete_worker_dir(action.slug, fleet_bin, project)
        return SENTINEL_APPLIED
    elif action.kind == "new_task":
        # Wake-only sentinel — nothing to apply. Presence of the file
        # was the wake; dispatch_ready in the same tick will pick up
        # the new task if it's ready. Token-free → always "applied"
        # (a benign no-op the caller consumes).
        return SENTINEL_APPLIED
    return SENTINEL_APPLIED


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
    coord_state: dict | None = None,
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

    Codex iter-6 [P2]: ALSO release the coord_prompt_inbox claim +
    forget the agent_id mapping for the same tasks. Operator-driven
    `fleet tasks set status=done` skips the reconcile/sentinel apply
    path that would normally call _release_coord_prompt_inbox, so
    without this wire the PR1 journal/inbox claim leaks indefinitely
    for every manually-completed task. The release is best-effort:
    a non-success outcome only logs to stderr.

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
    # Codex iter-6 [P2]: cache the agent_id map once per sweep to
    # avoid re-loading the dict for every done task.
    agent_id_map = (
        supervisor_mod.load_agent_id_map(coord_state)
        if coord_state is not None else {}
    )
    # Codex iter-9 [P2]: a slug that the operator marks `done` may
    # have a half-written claim from a failed acquire (tracked in
    # pending_acquire_agent_ids) without ever populating
    # worker_agent_ids. The sweep must release THAT id too so the
    # operator-done path doesn't leak the half-written journal.
    pending_acquire_map = (
        supervisor_mod.load_pending_acquire_agent_id_map(coord_state)
        if coord_state is not None else {}
    )
    swept = 0
    for t in tasks:
        if t.status != "done":
            continue
        # D1 (DESIGN §4.2): a dirty-parked `done` row keeps its worker
        # dir on purpose — it holds the recovery context the operator
        # needs to inspect/commit/discard the leaked dirty worktree.
        # This every-tick sweep must NOT erase it. Skip any worker dir
        # whose task row is `parked`; the field is cleared on resolve
        # (status leaves blocked / explicit `fleet tasks set parked=`)
        # and the next sweep re-arms. (The claim-release wire below is
        # also skipped — a parked row is still mid-lifecycle.)
        if getattr(t, "parked", ""):
            continue
        # Codex iter-6 [P2]: even if the worker dir is already gone
        # (sweep already ran on a prior tick), the slug may still have
        # a live coord_prompt_inbox claim from when it was in-progress.
        # Run the release wire FIRST so a worker_dir-already-gone slug
        # still triggers cleanup. The agent_id_map probe is a no-op
        # when the slug isn't tracked.
        if coord_state is not None:
            # Collect both worker_agent_ids and pending_acquire_agent_ids
            # candidates. Dedupe in case they happen to be the same id.
            candidate_ids: list[str] = []
            wid = agent_id_map.get(t.slug, "")
            if wid:
                candidate_ids.append(wid)
            pid = pending_acquire_map.get(t.slug, "")
            if pid and pid not in candidate_ids:
                candidate_ids.append(pid)
            for cand in candidate_ids:
                # Codex iter-7 [P1]: gate forget on outcome.
                release_outcome = _release_coord_prompt_inbox(
                    slug=t.slug,
                    agent_id=cand,
                    fleet_bin=fleet_bin,
                    fleet_home=fleet_home,
                    site=f"sweep-done-operator-driven id={cand}",
                )
                if _release_outcome_is_terminal(release_outcome):
                    # Drop both maps — the slug is operator-done, so
                    # any pending entry is moot now.
                    if cand == wid:
                        supervisor_mod.forget_agent_id(coord_state, t.slug)
                    if cand == pid:
                        supervisor_mod.forget_pending_acquire_agent_id(
                            coord_state, t.slug,
                        )
                else:
                    # Codex iter-10 [P1]: stash for retry. The next
                    # tick's _retry_pending_releases will re-attempt.
                    supervisor_mod.remember_pending_release_agent_id(
                        coord_state, t.slug, cand,
                    )
            if candidate_ids:
                _clear_review_handoff_state(coord_state, t.slug)
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


def _sweep_non_inflight_claim_state(
    tasks: list[parse.Task],
    project: str,
    fleet_bin: str,
    *,
    home: Path | None = None,
    coord_state: dict | None = None,
) -> None:
    """Codex iter-10 [P2]: release coord_prompt_inbox claims + clear
    map entries for slugs that an operator manually flipped to a non-
    in-flight status OTHER than done (`todo`, `blocked`,
    `abandoned`, `in-review`).

    Reconcile / sentinel paths only fire while status is in-progress /
    in-review, so an operator's manual `fleet tasks set <slug>
    status=todo` would leave any tracked agent_id orphaned in
    coord_state with a live journal+inbox. This sweep walks every
    slug present in worker_agent_ids OR pending_acquire_agent_ids
    and, when the corresponding task is no longer dispatch-actionable
    (status not in `in-progress` / `in-review`), releases the claim
    and drops the map entry.

    Best-effort: never raises. The `done` case is already covered by
    _sweep_done_worker_dirs, so we explicitly exclude it here to
    avoid double-attempt.

    Note: this sweep deliberately targets only slugs that show up in
    one of the in-memory maps. A slug with no tracked agent_id (legacy
    pre-PR1, or already-released) is a no-op.
    """
    if not project or coord_state is None:
        return
    fleet_home = home if home is not None else _resolve_home(None)
    agent_id_map = supervisor_mod.load_agent_id_map(coord_state)
    pending_acquire_map = supervisor_mod.load_pending_acquire_agent_id_map(coord_state)
    tracked_slugs = set(agent_id_map.keys()) | set(pending_acquire_map.keys())
    if not tracked_slugs:
        return
    task_status = {t.slug: t.status for t in tasks}
    # Codex iter-10 / iter-12 / iter-13 / iter-14 scope: we touch
    # slugs in one of these buckets:
    #
    #   - Operator-driven non-inflight non-done states
    #     (`todo`, `abandoned`): both worker_agent_ids and
    #     pending_acquire_agent_ids ids are released. Reconcile /
    #     sentinel paths only target in-progress / in-review, so
    #     these slugs would otherwise orphan their claims.
    #
    #   - Codex iter-14 [P2]: `blocked` with worker dir GONE.
    #     The BLOCKED_QUESTION lifecycle keeps the worker running
    #     and its worker_dir present (state.json updated). A
    #     manual operator-driven `blocked` transition with no live
    #     worker has its worker_dir missing/absent. We use that as
    #     the discriminator. Existing worker_dir → preserve (live
    #     BLOCKED_QUESTION); missing worker_dir → sweep (manual).
    #
    #   - `ready` slugs with a worker_agent_ids entry (codex iter-12
    #     [P1]): an operator manually flipped a dispatched task
    #     back to ready. Without releasing the old claim,
    #     _filter_ready picks the task up and double-dispatches.
    #     Only worker_agent_ids triggers this; pending_acquire on a
    #     `ready` slug is the recovery scenario (next dispatch
    #     reuses the id), so we explicitly skip pending-only ready
    #     slugs.
    #
    #   - Slugs absent from tasks.md (codex iter-12 [P2]): operator
    #     archived/removed the row but the in-memory map still
    #     tracks an agent_id. The slug will never come back through
    #     reconcile so we release here.
    sweep_statuses = {"todo", "abandoned"}
    # For `blocked` slugs we need the worker_dir-existence check
    # below — they're a conditional inclusion.
    project_workers = fleet_home / "projects" / project / "workers"
    for slug in tracked_slugs:
        status = task_status.get(slug, "")
        wid = agent_id_map.get(slug, "")
        pid = pending_acquire_map.get(slug, "")
        # ready + worker_agent_ids: operator-driven reset (P1).
        ready_reset = (status == "ready") and bool(wid)
        # absent from tasks.md AND has any tracked id: archived slug
        # cleanup (P2).
        archived = (slug not in task_status) and (bool(wid) or bool(pid))
        # Codex iter-14 [P2]: blocked + worker_dir GONE → operator-
        # driven manual blocked (no live worker). The
        # BLOCKED_QUESTION lifecycle keeps the worker dir alive.
        blocked_manual = False
        if status == "blocked":
            try:
                if not (project_workers / slug).is_dir():
                    blocked_manual = True
            except OSError:
                # Stat fault: stay conservative and don't sweep.
                pass
        # in-progress / in-review / done / ready-only-pending: skip
        # (lifecycle paths handle them).
        if (
            status not in sweep_statuses
            and not ready_reset
            and not archived
            and not blocked_manual
        ):
            continue
        # On ready_reset we ONLY want to release the worker_agent_ids
        # id — pending_acquire (if present) is the half-written
        # acquire for the next dispatch attempt and must stay so the
        # dispatch path can reuse it.
        candidate_ids: list[tuple[str, str]] = []  # [(id, source)]
        if wid:
            candidate_ids.append((wid, "worker"))
        if not ready_reset and pid and pid != wid:
            candidate_ids.append((pid, "pending"))
        for cand, source in candidate_ids:
            release_outcome = _release_coord_prompt_inbox(
                slug=slug,
                agent_id=cand,
                fleet_bin=fleet_bin,
                fleet_home=fleet_home,
                site=f"sweep-non-inflight status={status!r} id={cand}",
            )
            if _release_outcome_is_terminal(release_outcome):
                if source == "worker":
                    # Codex iter-15 [P1]: in the ready_reset case,
                    # preserve pending_acquire so _dispatch_ready can
                    # reuse the half-written claim's id via the
                    # recovery branch. forget_agent_id's default also
                    # clears pending — opt out for this case only.
                    supervisor_mod.forget_agent_id(
                        coord_state, slug,
                        also_pending=not ready_reset,
                    )
                if source == "pending":
                    supervisor_mod.forget_pending_acquire_agent_id(
                        coord_state, slug,
                    )
            else:
                supervisor_mod.remember_pending_release_agent_id(
                    coord_state, slug, cand,
                )
        if candidate_ids:
            _clear_review_handoff_state(coord_state, slug)


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
    dispatch_generation: int = 0  # coord-owned per-slug fence token
                                  # (DESIGN §1/§3) stamped into this
                                  # dispatch's prompt + persisted to the
                                  # task row in _apply_dispatch's
                                  # pre-launch status=in-progress commit.
                                  # A genuine (re-)dispatch increments
                                  # t.dispatch_generation by 1; a pending-
                                  # acquire retry reuses the recorded gen;
                                  # a handoff inherits the slug's current
                                  # gen (no increment).
    error: str = ""


@dataclass
class _ReplayAction:
    """A replay decision for one dispatch journal entry (#184).

    Mirrors _DispatchAction's emit shape but is produced by the
    tick-entry replay reconcile, NOT a fresh dispatch. agent_id +
    dispatch_instruction populated on a successful reserve; error/raise
    carry escalations (cap reached, residual-crash repair).
    """
    agent_id: str
    slug: str = ""
    dispatch_instruction: str = ""
    error: str = ""       # transient/log-only (contention, parse)
    raise_msg: str = ""   # off-channel escalation (capped / phantom repair)


def _iter_project_journals(
    home: Path, project: str,
) -> "list[tuple[str, str, dict]]":
    """Yield (agent_id, slug, journal_dict) for every dispatch journal
    owned by `project`.

    Ownership is read from the journal's `owner` field
    (`project/<project>/slug/<slug>`) — NEVER from cwd, tasks.md, or
    coord_state (the replay-predicate invariant: key only on journal
    state + project ownership). A journal whose owner doesn't match this
    project is skipped (strict coord scope).
    """
    out: list[tuple[str, str, dict]] = []
    disp_dir = home / "dispatches"
    try:
        entries = sorted(disp_dir.iterdir())
    except (FileNotFoundError, NotADirectoryError, OSError):
        return out
    owner_prefix = f"project/{project}/"
    for p in entries:
        if not p.name.endswith(".json") or p.name.endswith(".json.lock"):
            continue
        agent_id = p.name[: -len(".json")]
        if not _AGENT_ID_RE.fullmatch(agent_id):
            continue
        try:
            j = json.loads(p.read_text(encoding="utf-8"))
        except (json.JSONDecodeError, OSError):
            continue
        if not isinstance(j, dict):
            continue
        owner = j.get("owner", "")
        if not isinstance(owner, str) or not owner.startswith(owner_prefix):
            continue
        slug = owner[len(owner_prefix):]
        slug = slug[len("slug/"):] if slug.startswith("slug/") else slug
        out.append((agent_id, slug, j))
    return out


def _parse_iso_utc(s: str) -> float | None:
    """Parse a Go RFC3339/ISO-8601 UTC timestamp to epoch seconds, or
    None. Go writes e.g. "2026-06-01T12:00:00Z"."""
    if not s or not isinstance(s, str):
        return None
    try:
        # Python's fromisoformat handles the trailing 'Z' from 3.11+.
        from datetime import datetime, timezone
        t = s.replace("Z", "+00:00") if s.endswith("Z") else s
        dt = datetime.fromisoformat(t)
        if dt.tzinfo is None:
            dt = dt.replace(tzinfo=timezone.utc)
        return dt.timestamp()
    except (ValueError, TypeError):
        return None


def _replay_pending_dispatches(
    *,
    project: str,
    home: Path,
    fleet_bin: str,
    fleet_home: str,
    coord_state: dict,
    now_unix: float,
) -> list[_ReplayAction]:
    """Tick-entry replay reconcile (dispatch-durability #184).

    For every dispatch journal owned by this project:

      - ExecPending → reserve-replay (durable cap++); if reserved, re-emit
        the SAME DISPATCH block (same agent_id + inbox) stamped with the
        journal's CURRENT generation so the coord's mark-launch-attempted
        gate validates it against this lifecycle. If the cap is reached,
        the Go side flips the entry to ExecBlocked; we raise an
        off-channel escalation (NOT stdout — a broken stdout is exactly
        why replay exists).
      - ExecLaunchAttempted, un-acked, no live registered subagent, past
        LAUNCH_ACK_GRACE → residual-crash repair: a crash after the launch
        flip but before the Agent invoke (a silent phantom). Flip to
        ExecBlocked + escalate. NEVER blind-replay launch_attempted (the
        double-launch trap) and NEVER trust bootstrapped state.json
        freshness.
      - ExecAcked / live worker / terminal → leave alone.

    Replay keys on journal state + project ownership + DISPATCH IDENTITY:
    a pending journal is re-emitted only when worker_agent_ids[slug] equals
    this journal's agent_id — i.e. this journal IS the task's current,
    applied dispatch. Codex iter-2/iter-4 [P1]: keying on journal state
    alone (the original rev6 invariant) double-launches across the
    acquire-before-apply crash window — an orphaned ExecPending journal A
    (whose coord crashed before persisting state) gets replayed while the
    still-`ready` task is freshly dispatched as agent B. The identity
    predicate ties replay to the dispatch the task actually adopted, so an
    orphan (worker_agent_ids[slug] != A) is never replayed; it's left for
    the cap / sweeper. The phantom case still works: a lost-stdout block's
    journal IS the adopted dispatch (worker_agent_ids[slug] == A, task
    in-progress), so it re-emits.
    """
    actions: list[_ReplayAction] = []
    # Identity maps for the replay predicate. worker_agent_ids[slug] is the
    # agent_id the task ADOPTED (persisted by _apply_dispatch /
    # _apply_dispatch_handoff before coord-state is saved). pending_acquire
    # holds ids that are mid-application (acquired, not yet applied) — those
    # are the normal retry path's, never replay's.
    try:
        adopted_agent_ids = supervisor_mod.load_agent_id_map(coord_state)
    except Exception:  # noqa: BLE001
        adopted_agent_ids = {}
    try:
        pending_acquire_ids = set(
            supervisor_mod.load_pending_acquire_agent_id_map(coord_state).values()
        )
    except Exception:  # noqa: BLE001
        pending_acquire_ids = set()
    for agent_id, slug, j in _iter_project_journals(home, project):
        state = j.get("exec_state", "")
        if state == "pending":
            # Codex iter-4 [P1]: only replay the journal the task currently
            # owns. An orphaned pending journal (coord crashed after
            # acquire but before persisting state, so the task was
            # re-dispatched under a DIFFERENT agent_id) must NOT be
            # replayed — that's the cross-id double-launch the per-id CAS
            # cannot catch. Mid-application ids (in pending_acquire, not
            # yet adopted) are the normal retry's; skip them too.
            if agent_id in pending_acquire_ids:
                continue
            if adopted_agent_ids.get(slug) != agent_id:
                # This journal is not the task's adopted dispatch (orphan
                # from a crash, or a stale prior id). Leave it — replay
                # would launch a worker the task never adopted, racing the
                # real dispatch. The cap path / sweeper reaps the orphan.
                continue
            res = dispatch_mod.reserve_replay(
                agent_id, cap=_REPLAY_CAP,
                fleet_bin=fleet_bin, fleet_home=fleet_home,
            )
            outcome = res.get("outcome")
            if outcome == "reserved":
                inbox = str(home / "inbox" / f"{agent_id}.md")
                try:
                    block = dispatch_mod.format_dispatch_instruction(
                        agent_id=agent_id, slug=slug or agent_id,
                        prompt_file=inbox,
                        description=f"fleet worker {slug or agent_id} (replay)",
                        generation=int(res.get("generation") or 0),
                    )
                except ValueError as exc:
                    actions.append(_ReplayAction(
                        agent_id=agent_id, slug=slug,
                        error=f"replay format failed: {exc}",
                    ))
                    continue
                actions.append(_ReplayAction(
                    agent_id=agent_id, slug=slug,
                    dispatch_instruction=block,
                ))
            elif outcome == "capped":
                # Durable BLOCKED — surface off-channel (NOT stdout; a
                # broken stdout is why replay exists). surface-dont-silo.
                actions.append(_ReplayAction(
                    agent_id=agent_id, slug=slug,
                    raise_msg=(
                        f"dispatch {slug or agent_id} (agent {agent_id}) "
                        f"undelivered after {_REPLAY_CAP} replay attempts; "
                        "marked BLOCKED. stdout to the coord is likely broken "
                        "— re-run the tick with full output captured "
                        "(never `| head`)."
                    ),
                ))
            elif outcome in ("contention", "error"):
                # Transient — retry next tick; log-only.
                actions.append(_ReplayAction(
                    agent_id=agent_id, slug=slug,
                    error=f"replay reserve outcome={outcome!r}; retry next tick",
                ))
            # not_pending / absent: a concurrent writer changed state
            # between our read + the reserve; nothing to do this tick.
        elif state == "launch_attempted":
            # state is "launch_attempted" here by construction; an "acked"
            # entry would have taken neither this branch nor the pending
            # one (replay leaves acked/terminal alone). Residual-crash
            # repair only fires when the launch flip landed but no ack
            # ever followed.
            attempted_at = _parse_iso_utc(j.get("launch_attempted_at", ""))
            if attempted_at is None:
                # No timestamp — can't time-gate; be conservative and
                # leave it (avoid flipping a launch we can't reason about).
                continue
            if now_unix - attempted_at <= _LAUNCH_ACK_GRACE_S:
                continue  # still inside the grace window
            # Codex iter-2 [P1]: register_subagent is best-effort — the
            # protocol explicitly allows it to be SKIPPED on lock
            # contention while the worker still runs. So "launch_attempted
            # + no subagent_id + past grace" does NOT prove a phantom; a
            # healthy worker that just skipped registration looks
            # identical. Gate on the worker's OWN state.json.
            #
            # Codex iter-3 [P1]: the liveness signal MUST be tied to THIS
            # dispatch, not the slug. A reviewer/finisher handoff mints a
            # NEW agent_id but reuses the slug, and coord_state still holds
            # the PRIOR stage's subagent_id while state.json still reads
            # the prior phase (review-pending / review-done). A slug-keyed
            # subagent check or a phase-only check would treat that
            # prior-stage state as proof THIS launch is live and skip
            # repair forever. So we (a) dropped the slug-keyed subagent
            # check entirely, and (b) require the worker's state.json to
            # have advanced AFTER this journal's launch_attempted_at — a
            # prior-stage write predates the new launch flip and does NOT
            # count. Only a fresh-after-launch worker write means a live
            # (just-unregistered) worker for THIS dispatch.
            if _worker_launch_looks_live(
                project, slug, home=home, since_unix=attempted_at,
            ):
                continue
            # Genuinely no live worker after grace → likely crashed
            # between the launch flip and the Agent invoke. ESCALATE
            # off-channel ONLY (surface-dont-silo). We do NOT release /
            # mark the journal failed here: a destructive release could
            # tear down the inbox of a worker we merely failed to observe,
            # and the worker's own terminal transition (or the operator)
            # will release it. Replay never re-emits launch_attempted, so
            # leaving it non-terminal cannot double-launch.
            #
            # Codex iter-6 [P2]: escalate ONCE per (agent_id, generation).
            # The journal stays launch_attempted (we deliberately don't
            # mutate it), so without a durable breadcrumb this branch would
            # re-raise the same operator escalation every tick forever.
            # Record the escalation in coord_state (persisted by tick());
            # a reset-for-relaunch bumps the generation, so a relaunch that
            # also phantoms gets a fresh escalation.
            generation = int(j.get("generation") or 0)
            if _phantom_already_escalated(coord_state, agent_id, generation):
                continue
            _record_phantom_escalated(coord_state, agent_id, generation)
            actions.append(_ReplayAction(
                agent_id=agent_id, slug=slug,
                raise_msg=(
                    f"dispatch {slug or agent_id} (agent {agent_id}) flipped to "
                    f"launch_attempted but never acked and shows no live worker "
                    f"after {_LAUNCH_ACK_GRACE_S}s — likely crashed between the "
                    f"launch flip and the Agent invoke. NOT auto-replayed (would "
                    "double-launch) and NOT auto-released (a live-but-unregistered "
                    "worker looks identical). Re-dispatch manually if the task is "
                    "still needed."
                ),
            ))
        # other states: leave alone.
    return actions


def _dispatch_ready(
    *,
    tasks: list[parse.Task],
    project: str,
    cwd: str,
    cap: int,
    fleet_bin: str,
    fleet_home: str,
    coord_state: dict | None = None,
) -> list[_DispatchAction]:
    """Filter to dispatchable candidates, sort by priority, dispatch
    under cap. Returns the actions we successfully (or unsuccessfully)
    started — each action carries its own error so caller can decide
    what to record.

    Worktree-mode (cap > 1): each dispatched worker gets its own git
    worktree under ~/.fleet/projects/<p>/worktrees/<slug>/, branched
    `worker/<slug>` off the FRESH origin/<default-branch> tip (we fetch
    origin/<default> first), NOT off the coord's local HEAD — so a
    dependency PR that just merged to origin is present in the worker's
    tree even if the coord never pulled. Worker's cwd is the worktree
    path (NOT the main repo). A failed worktree create aborts that one
    dispatch — the loop continues with the next candidate so a stale
    on-disk worktree doesn't block all of cap.

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
            # Pick the worker's base ref. Several pulls are in tension:
            #   (a) the worker must see commits a dependency PR JUST merged
            #       to the remote (the stale-local-HEAD bug this PR fixes),
            #   (b) honor the coord's deliberately checked-out branch — not
            #       the remote default (codex [P1] #1),
            #   (c) honor a DIFFERENTLY-named configured upstream, not an
            #       assumed origin/<current> (codex [P2]), and
            #   (d) NEVER drop the operator's local commits that are ahead
            #       of / diverged from that upstream (codex [P1] #2).
            #
            # resolve_worker_base picks the candidate base: the coord's
            # branch's CONFIGURED upstream (@{upstream}); local HEAD when
            # there's no upstream; the remote default when HEAD is detached.
            # We then fetch that upstream (best-effort) and apply the
            # ancestry gate below.
            #
            #   local HEAD ⊆ fresh upstream?   (merge-base --is-ancestor)
            #     yes → base = upstream    (local adds nothing; take freshness)  (a)
            #     no  → base = "" / HEAD   (local ahead/diverged; preserve it)   (d)
            #
            # fetch is best-effort: on failure the upstream ref is whatever
            # already resolves locally (a possibly-stale ref beats wedging
            # all of cap on a network blip).
            wb = worktree_mod.resolve_worker_base(cwd)
            if wb.fetch_branch:
                fetch_res = worktree_mod.fetch_remote(
                    cwd, wb.fetch_branch, remote=wb.fetch_remote or "origin",
                )
                if fetch_res.error:
                    print(
                        f"coord: {fetch_res.error}; will branch {t.slug} off "
                        f"{wb.base or 'local HEAD'} at its current local tip",
                        file=sys.stderr,
                    )
            base_ref = ""  # default: local HEAD
            # Use the fresh upstream as base ONLY when it (1) resolves to a
            # commit — a remote ref that never landed locally would make
            # `git worktree add` fatal "invalid reference" (codex [P2]) —
            # AND (2) local HEAD is an ancestor of it, i.e. the upstream is
            # a strict superset of local so we lose no local commits (codex
            # [P1] #2). Otherwise fall back to local HEAD.
            if wb.base and worktree_mod.ref_exists(cwd, wb.base):
                if worktree_mod.is_ancestor(cwd, "HEAD", wb.base):
                    base_ref = wb.base
                else:
                    print(
                        f"coord: local HEAD has commits ahead of {wb.base}; "
                        f"branching {t.slug} off local HEAD to preserve them",
                        file=sys.stderr,
                    )
            elif wb.base:
                print(
                    f"coord: {wb.base} not present in {cwd}; branching "
                    f"{t.slug} off local HEAD (no fresh remote base)",
                    file=sys.stderr,
                )
            wt_result = worktree_mod.create_worktree(
                cwd, wt_path, worker_branch, base=base_ref,
            )
            if wt_result.error:
                actions.append(_DispatchAction(
                    slug=t.slug, error=wt_result.error,
                ))
                continue
            worker_cwd = wt_result.path
            worker_worktree = wt_result.path

        # Epoch (DESIGN §1/§3): compute the dispatch_generation BEFORE
        # building the prompt so the gen the worker is told to write is
        # the SAME value _apply_dispatch persists to the task row. A
        # pending-acquire retry REUSES its recorded gen + agent_id (the
        # task row + already-acquired prompt are unchanged; re-
        # incrementing would skew the task row to N+1 while the prompt
        # still says N, wedging every CAS'd `fleet workers update`). The
        # reuse is gated on a FULL {agent_id, gen, kind} match — a stale
        # wrong-kind pending entry (e.g. a reviewer/finisher acquire that
        # errored on the same slug) is forgotten + a fresh worker
        # dispatch is minted (never reused), so acquire_coord_prompt_inbox
        # writes a fresh worker prompt instead of returning already_
        # acquired on a reviewer prompt. A genuine (re-)dispatch
        # increments t.dispatch_generation by exactly 1.
        pending_rec = None
        if coord_state is not None:
            pending_rec = supervisor_mod.load_pending_acquire_record_map(
                coord_state,
            ).get(t.slug)
        pending_kind = pending_rec.get("dispatch_kind") if pending_rec else None
        next_gen = int(t.dispatch_generation) + 1
        # Codex iter-2 [P2]: a worker pending record is reusable ONLY when
        # its recorded gen is consistent with THIS slug's current task row.
        # A worker record was minted as (prior_task_gen + 1). On a clean
        # retry the task row is unchanged so recorded_gen == next_gen
        # (acquire/apply never persisted the bump). If a partial apply DID
        # persist the bump before failing, the row now equals recorded_gen.
        # Any OTHER value means the slug was reset/re-dispatched out from
        # under this record (the row advanced again) — reusing it would
        # write a STALE gen back via _apply_dispatch + reuse an old prompt
        # under an old CAS token, defeating the fence. Forget + mint fresh.
        pending_gen = (
            int(pending_rec["dispatch_generation"]) if pending_rec else None
        )
        worker_gen_consistent = pending_gen in (
            next_gen, int(t.dispatch_generation),
        )
        if pending_rec is not None and pending_kind == "worker" and worker_gen_consistent:
            # A verified worker retry: reuse BOTH agent_id and the
            # recorded gen (the task row + already-acquired prompt are
            # unchanged; re-incrementing would skew them).
            agent_id = pending_rec["agent_id"]
            dispatch_generation = int(pending_rec["dispatch_generation"])
        elif pending_rec is not None and pending_kind == "":
            # Legacy bare-string pending entry (pre-migration, kind
            # unknown). Back-compat: reuse the agent_id as a worker retry
            # AND keep its gen 0 (ungated) so the half-written legacy
            # prompt — which carries no --dispatch-generation — still
            # matches if acquire returns already_acquired. A fresh gen
            # would mint >=1 and skew the unchanged legacy prompt.
            agent_id = pending_rec["agent_id"]
            dispatch_generation = 0
        else:
            if pending_rec is not None and coord_state is not None:
                # Not reusable: a POSITIVE wrong-kind entry (a reviewer/
                # finisher acquire that errored on this slug), OR a
                # worker-kind entry whose recorded gen is inconsistent with
                # the current task row (the slug was reset/re-dispatched
                # out from under it). Either way, drop it so we don't reuse
                # it via already_acquired with the wrong prompt / a stale
                # CAS token. The orphaned journal/inbox is reclaimed by the
                # existing replay/sweeper machinery, same as any orphan.
                supervisor_mod.forget_pending_acquire_agent_id(
                    coord_state, t.slug,
                )
            agent_id = dispatch_mod.mint_agent_id()
            dispatch_generation = next_gen
        try:
            prompt = dispatch_mod.build_worker_prompt(
                t, project=project,
                standards_md=standards_md,
                learnings_text=learnings_text,
                branch=worker_branch,
                worktree_pre_created=bool(worker_worktree),
                is_git=is_git,
                dispatch_generation=dispatch_generation,
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
        #
        # PR1 of dispatch-lifecycle re-arch (DESIGN-dispatch-lifecycle.md):
        # inbox write goes through `fleet claims acquire-prompt` so the
        # coord_prompt_inbox file is owned by a journal at
        # ~/.fleet/dispatches/<agent_id>.json — its Release on terminal
        # transition closes the 30-file leak the prior architecture
        # suffered. Direct `dispatch_mod.write_worker_inbox` stays in
        # the module for handoff_resume.py:366; PR2 migrates that and
        # retires the helper.
        #
        # Codex iter-4 [P1]: a prior tick may have minted an agent_id
        # whose acquire-prompt failed AFTER writing the journal /
        # inbox. We MUST retry with the same id so
        # AcquireCoordPromptInbox's idempotent / recovery path can
        # finish the half-written claim. Without reusing the id, the
        # orphaned journal sits forever (PR4 sweeper eventually
        # reclaims, but PR1 should not depend on that escape hatch).
        #
        # Codex iter-8 [P1]: pending_acquire_agent_ids is now ALSO
        # populated on every acquire-success (cleared on apply-
        # success). That single map covers both gaps:
        #   - acquire failed half-way (pending set by except branch)
        #   - acquire OK but apply failed (pending set after success)
        # The next-tick retry reuses the same id either way.
        # (agent_id + dispatch_generation resolved above from the
        # pending record, or freshly minted/incremented.)
        try:
            inbox_path = dispatch_mod.acquire_coord_prompt_inbox(
                agent_id, prompt,
                owner=f"project/{project}/slug/{t.slug}",
                dispatch_kind="worker",
                fleet_bin=fleet_bin,
                fleet_home=fleet_home,
            )
        except dispatch_mod.AcquirePromptError as exc:
            # On error outcome (binary missing, journal write race),
            # leave the task in `ready` for the next tick to retry. The
            # action carries the error so loop.py's caller surfaces it
            # in the tick summary.
            #
            # Codex iter-4 [P1]: also persist the agent_id as a
            # pending-acquire entry so the NEXT tick reuses it. This
            # is the key to hitting AcquireCoordPromptInbox's recovery
            # branch for half-written journals. The record carries the
            # gen + kind so the retry REUSES this gen (DESIGN §3) and
            # proves it's a worker (not a handoff) retry.
            if coord_state is not None:
                supervisor_mod.remember_pending_acquire_record(
                    coord_state, t.slug, agent_id,
                    dispatch_generation, "worker",
                )
            # agent_id="" on the error action so callers can't
            # mistakenly treat the failed dispatch as successful (see
            # the action.error gates in the dispatch consumer loops).
            actions.append(_DispatchAction(
                slug=t.slug,
                error=f"acquire coord_prompt_inbox failed "
                      f"(outcome={exc.outcome}, exit={exc.exit_code}): "
                      f"{exc.message}",
            ))
            continue
        except Exception as exc:  # noqa: BLE001
            if coord_state is not None:
                supervisor_mod.remember_pending_acquire_record(
                    coord_state, t.slug, agent_id,
                    dispatch_generation, "worker",
                )
            actions.append(_DispatchAction(
                slug=t.slug,
                error=f"acquire coord_prompt_inbox failed: {exc}",
            ))
            continue
        # Acquire succeeded. Codex iter-8 [P1]: persist a pending-
        # acquire entry NOW (alongside the action that the caller
        # will turn into worker_agent_ids). The caller clears this
        # pending entry AFTER _apply_dispatch succeeds. The two
        # in-flight states are:
        #   - "acquire failed half-way" → pending set by except branch
        #   - "acquire OK but apply may fail" → pending set right here
        # Either way, pending_acquire_agent_ids tells the next tick:
        # "this slug already has a journal+inbox keyed off THIS id;
        # reuse it for any retry". On apply success the caller
        # clears the entry and pending becomes empty again, so a
        # SUBSEQUENT dispatch after terminal forget mints fresh.
        if coord_state is not None:
            supervisor_mod.remember_pending_acquire_record(
                coord_state, t.slug, agent_id,
                dispatch_generation, "worker",
            )
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
            dispatch_generation=dispatch_generation,
        ))
        active += 1
        in_flight_after_dispatch.append(t)
    return actions


def _worktree_path_if_present(
    fleet_home: str, project: str, slug: str,
) -> str:
    """Return the worker's worktree path iff it's a real git worktree on
    disk, else "".

    Mirrors Go's state.WorktreePath layout
    (`<fleet_home>/projects/<project>/worktrees/<slug>`). A linked git
    worktree has a `.git` FILE (a gitdir pointer), not a directory — we
    check for that so a stale empty dir (or a non-worktree drop-in)
    doesn't get mistaken for a real checkout. Deriving the path directly
    (instead of shelling to `fleet workers worktree-path`) keeps the
    handoff hot path subprocess-free and trivially testable; the layout
    is a stable contract pinned by state.WorktreePath + its test.

    dispatch-reviewer-finish-9316: the reviewer/finisher use this to
    decide between `cd <worktree>` and `git checkout <branch>`.
    """
    if not fleet_home or not project or not slug:
        return ""
    wt = os.path.join(fleet_home, "projects", project, "worktrees", slug)
    if os.path.exists(os.path.join(wt, ".git")):
        return wt
    return ""


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
        # R8 chokepoint (DESIGN §2.1/§3): route the handoff-trigger read
        # through the generation-aware reader. A STALE `review-pending` /
        # `review-done` from a PRIOR attempt must NOT double-dispatch a
        # reviewer/finisher onto the re-dispatched slug — that spawned
        # subagent would be an accepted CURRENT-gen writer (e.g. a
        # spurious phase=done), losing the live attempt's work + reaping
        # its clean tree. `stale` short-circuits (no release, no pending-
        # acquire mutation, no DISPATCH emit); `missing` falls to the
        # existing handoff no-op; only `current` proceeds. This is the
        # one reader PR2 wires (it's part of the epoch surface); the full
        # R1-R7 reconcile/reaper routing is PR3.
        cls, st = read_current_worker_state(
            project, t.slug, int(t.dispatch_generation), home=home,
        )
        if cls != WORKER_STATE_CURRENT or st is None:
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
        # dispatch-reviewer-finish-9316: if the worker ran in a pre-
        # created worktree (cap>1 dispatch), the branch is checked out
        # THERE. The reviewer/finisher must cd into that worktree rather
        # than `git checkout <branch>` in the main repo (which fatals
        # "branch already used by worktree"). Worktrees live until the
        # task reaches a terminal state, so they're still on disk during
        # the review-pending → review-done handoff window. Empty string
        # (in-place dispatch) keeps the original git-checkout behavior.
        worktree = _worktree_path_if_present(fleet_home, project, t.slug)
        # A handoff INHERITS the slug's current dispatch_generation
        # (DESIGN §3) — no increment. The reviewer/finisher write under
        # the SAME gen as the worker that handed off, so their CAS'd
        # `fleet workers update` lands against the unchanged task-row
        # authority.
        handoff_generation = int(t.dispatch_generation)
        try:
            if phase == "review-pending":
                prompt = dispatch_mod.build_reviewer_prompt(
                    t, project=project, branch=branch,
                    worktree=worktree or None, is_git=is_git,
                    dispatch_generation=handoff_generation,
                )
                description = f"fleet reviewer {t.slug}"
            else:
                prompt = dispatch_mod.build_finisher_prompt(
                    t, project=project, branch=branch,
                    worktree=worktree or None, is_git=is_git,
                    dispatch_generation=handoff_generation,
                )
                description = f"fleet finisher {t.slug}"
        except dispatch_mod.PromptTooLargeError as exc:
            actions.append(_DispatchAction(slug=t.slug, error=str(exc)))
            continue

        # Codex iter-4 [P1]: same pending-acquire reuse pattern as
        # _dispatch_ready. A handoff acquire that failed mid-journal-
        # write on a prior tick must retry with the same agent_id so
        # the recovery path can heal the half-written claim.
        #
        # Codex iter-8 [P1]: pending_acquire_agent_ids is now set
        # on EVERY acquire-success (cleared on apply-success), so it
        # also catches the apply-failed-mid-chain scenario. A retry
        # here that finds a pending entry MUST reuse it.
        # Pending-acquire entries from _dispatch_ready and from
        # _dispatch_review_handoffs share the same keyspace (slug). A
        # retry may reuse the prior tick's agent_id ONLY on a full
        # {dispatch_generation, dispatch_kind} match (DESIGN §3):
        # because handoffs inherit the slug's gen, a worker retry and a
        # reviewer/finisher retry on the same slug carry the SAME gen, so
        # gen equality alone can't tell them apart. A wrong-kind pending
        # entry (e.g. a leftover worker-kind acquire) must NOT be reused
        # via already_acquired — that would hand the reviewer/finisher
        # the worker's prompt. Kind mismatch → forget the stale entry +
        # mint fresh (a fresh acquire writes the correct prompt).
        this_kind = "reviewer" if phase == "review-pending" else "finisher"
        pending_handoff_rec = None
        if coord_state is not None:
            pending_handoff_rec = supervisor_mod.load_pending_acquire_record_map(
                coord_state,
            ).get(t.slug)
        pending_handoff_kind = (
            pending_handoff_rec.get("dispatch_kind") if pending_handoff_rec else None
        )
        # Reuse on a verified same-kind+same-gen match, OR a legacy
        # bare-string entry (kind unknown → back-compat: trust it as this
        # stage's retry). A POSITIVE different-kind entry (e.g. a leftover
        # worker-kind acquire) is NOT reused — that would hand this
        # reviewer/finisher the worker's prompt via already_acquired.
        reuse_handoff = pending_handoff_rec is not None and (
            pending_handoff_kind == ""
            or (
                pending_handoff_kind == this_kind
                and int(pending_handoff_rec.get("dispatch_generation", -1))
                == handoff_generation
            )
        )
        if reuse_handoff:
            agent_id = pending_handoff_rec["agent_id"]
            reusing_pending = True
        else:
            if pending_handoff_rec is not None and coord_state is not None:
                supervisor_mod.forget_pending_acquire_agent_id(
                    coord_state, t.slug,
                )
            agent_id = dispatch_mod.mint_agent_id()
            reusing_pending = False
        # PR1 dispatch-lifecycle: release the PRIOR dispatch's
        # coord_prompt_inbox claim BEFORE acquiring the next one. The
        # worker/reviewer that just emitted phase=review-pending or
        # review-done has reached a terminal phase from THIS coord's
        # perspective — its inbox + journal must be reclaimed before
        # the next stage's subagent dispatches. Without this, every
        # successful handoff leaks one inbox + one journal file.
        #
        # Best-effort: a non-success release outcome only logs to
        # stderr; the handoff still proceeds. We look up the prior
        # agent_id via the supervisor's slug → agent_id map (the only
        # in-memory record of the outgoing subagent's identity).
        #
        # Skip the release when we're reusing a pending agent_id —
        # the prior worker's agent_id is the one we're recovering, so
        # releasing it would conflict with the acquire that follows.
        if coord_state is not None and not reusing_pending:
            prior_agent_id = supervisor_mod.load_agent_id_map(
                coord_state,
            ).get(t.slug, "")
            release_outcome = _release_coord_prompt_inbox(
                slug=t.slug,
                agent_id=prior_agent_id,
                fleet_bin=fleet_bin,
                fleet_home=home,
                site=f"handoff phase={phase}",
            )
            # Codex iter-9 [P1]: if the handoff release failed
            # transiently, the new subagent dispatch is about to
            # overwrite worker_agent_ids with its own id — that would
            # permanently lose the only handle on the prior subagent's
            # claim. Stash the prior_agent_id in pending_release_
            # agent_ids so a later sweep / reconcile can retry. Only
            # do this when the outcome is non-terminal AND we have a
            # real agent_id (skip when prior_agent_id was empty —
            # e.g., legacy pre-PR1 worker).
            if (
                prior_agent_id
                and not _release_outcome_is_terminal(release_outcome)
            ):
                supervisor_mod.remember_pending_release_agent_id(
                    coord_state, t.slug, prior_agent_id,
                )
        # PR1 dispatch-lifecycle migration: same as _dispatch_ready,
        # but dispatch_kind reflects review-pending → "reviewer" and
        # review-done → "finisher" so the journal owner string + dispatch
        # role match the actual subagent shape. handoff_phase is the
        # current phase the worker exited at (set above).
        try:
            inbox_path = dispatch_mod.acquire_coord_prompt_inbox(
                agent_id, prompt,
                owner=f"project/{project}/slug/{t.slug}",
                dispatch_kind=("reviewer" if phase == "review-pending" else "finisher"),
                fleet_bin=fleet_bin,
                fleet_home=fleet_home,
            )
        except dispatch_mod.AcquirePromptError as exc:
            if coord_state is not None:
                supervisor_mod.remember_pending_acquire_record(
                    coord_state, t.slug, agent_id,
                    handoff_generation, this_kind,
                )
            actions.append(_DispatchAction(
                slug=t.slug,
                error=f"handoff acquire coord_prompt_inbox failed "
                      f"(outcome={exc.outcome}, exit={exc.exit_code}): "
                      f"{exc.message}",
            ))
            continue
        except Exception as exc:  # noqa: BLE001
            if coord_state is not None:
                supervisor_mod.remember_pending_acquire_record(
                    coord_state, t.slug, agent_id,
                    handoff_generation, this_kind,
                )
            actions.append(_DispatchAction(
                slug=t.slug,
                error=f"handoff acquire coord_prompt_inbox failed: {exc}",
            ))
            continue
        # Acquire succeeded. Codex iter-8 [P1]: persist pending entry
        # now so a subsequent handoff-apply failure leaves an
        # explicit retry breadcrumb (the next tick reuses this id
        # via the pending-acquire record on a full gen+kind match).
        # The caller clears this entry after _apply_dispatch_handoff
        # lands.
        if coord_state is not None:
            supervisor_mod.remember_pending_acquire_record(
                coord_state, t.slug, agent_id,
                handoff_generation, this_kind,
            )
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
            dispatch_generation=handoff_generation,
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
    # DESIGN §3 dispatch ordering: the dispatch_generation persist is
    # FOLDED INTO the same pre-launch status=in-progress commit (steps
    # 1+2), strictly before the state.json bootstrap (step 3) and before
    # the DISPATCH block is collected by the caller (step 5). Two
    # invariants ride on this:
    #   - the task row's dispatch_generation is the durable CAS authority
    #     (DESIGN §2.2); once it lands, every stale worker's CAS'd
    #     `fleet workers update` is rejected.
    #   - it must NOT be possible to emit a launchable DISPATCH while the
    #     slug is still status=ready — the status flip is first, and the
    #     caller collects the block only after this function returns.
    # gen<=0 (legacy un-migrated dispatch) skips the set, leaving the
    # task-row default (0) intact.
    _run_fleet([fleet_bin, "tasks", "set", "--project", project, action.slug, "status=in-progress"])
    if action.dispatch_generation > 0:
        _run_fleet([
            fleet_bin, "tasks", "set", "--project", project, action.slug,
            f"dispatch_generation={action.dispatch_generation}",
        ])
    if action.branch:
        _run_fleet([fleet_bin, "tasks", "set", "--project", project, action.slug, f"branch={action.branch}"])
    if action.worktree:
        # Persist the worktree path so reconcile knows where to clean
        # up on terminal transition (done/abandoned). Single-worker
        # mode leaves this empty so existing behavior is unchanged.
        _run_fleet([fleet_bin, "tasks", "set", "--project", project, action.slug, f"worktree={action.worktree}"])
    # Bootstrap state.json stamped with the SAME generation just
    # persisted to the task row (step 3). Passing --dispatch-generation
    # routes the bootstrap through the writer CAS: the task-row authority
    # equals action.dispatch_generation (we just set it), so the CAS
    # accepts + stamps the file — and the chokepoint reader classifies it
    # `current`. Without the stamp the bootstrapped file would read as a
    # legacy gen-0 state and be fenced `stale` the moment the slug's
    # authority is >= 1. gen<=0 (legacy) keeps the ungated bootstrap.
    starting_update = [
        fleet_bin, "workers", "update", "--project", project, action.slug,
        "--phase", "starting",
    ]
    if action.dispatch_generation > 0:
        starting_update += ["--dispatch-generation", str(action.dispatch_generation)]
    _run_fleet(starting_update)
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


# coord-self-exit-when-it-6014: injectable so tests can assert the
# self-exit path WITHOUT killing any real tmux session. Production
# implementation runs `tmux kill-session -t fleet-<coord_id>`, which
# tears down THIS coord's own session (the python tick runs inside it
# as a Stop-hook child; killing the session reaps the claude process
# cleanly — no kill -9). Returns True iff the kill command ran exit 0.
def _kill_own_tmux_session(coord_id: str) -> bool:
    session = supervisor_mod.session_name_for_agent(coord_id)
    if not session:
        return False
    try:
        proc = subprocess.run(
            ["tmux", "kill-session", "-t", session],
            capture_output=True, text=True, timeout=5.0, check=False,
        )
    except (FileNotFoundError, subprocess.TimeoutExpired, OSError):
        return False
    return proc.returncode == 0


# Test seam: overridable so the self-exit test never shells out to tmux.
_kill_own_session_fn = _kill_own_tmux_session


def main(argv: Iterable[str] | None = None) -> int:
    """Skill entry point. Reads project from FLEET_PROJECT env or argv.

    Exits 0 on the normal path — failures are recorded in the result and
    surfaced to the operator via the agent's blocked_reason; the hook
    itself must not block the agent's turn (matches fleet-guard
    discipline). The ONE exception (loop-supervisor-sigpipe-5263) is a
    broken stdout while emitting the DISPATCH block: that returns 2 so the
    harness re-ticks, because a swallowed broken pipe would silently drop
    the dispatch instead of delivering it to the coord.
    """
    argv = list(argv) if argv is not None else []
    project = os.environ.get("FLEET_PROJECT", "")
    if argv:
        project = argv[0]
    if not project:
        print("coordinator: no project set (FLEET_PROJECT or argv[0])")
        return 0
    result = tick(project)
    # coord-self-exit-when-it-6014: a duplicate coord (a different live
    # coord holds coordinator.lock + we are not the intended coord) must
    # not idle as a zombie. tick() already emitted the stderr diagnostic
    # (surface_dont_silo) and skipped all work, so there are no DISPATCH
    # blocks. Print the JSON summary so the reason is captured in the
    # tick log, then kill THIS session's own tmux session — the python
    # tick runs inside `fleet-<coord_id>` as a Stop-hook child, so the
    # kill reaps the claude process cleanly (no kill -9, no orphan).
    #
    # We kill AFTER printing so an operator tailing the tick output sees
    # the reason before the session disappears. The lock was never
    # acquired (lock-busy branch), so there is nothing to release.
    if result.self_exit:
        coord_id = os.environ.get("FLEET_AGENT_ID", "")
        print(json.dumps({
            "skipped": result.skipped,
            "reason": result.reason,
            "self_exit": True,
            "parsed": result.parsed_tasks,
            "reconciled": result.reconciled,
            "drained": result.drained,
            "dispatched": result.dispatched,
            "raised": result.raised,
            "errors": result.errors,
        }))
        sys.stdout.flush()
        if coord_id and not _kill_own_session_fn(coord_id):
            sys.stderr.write(
                f"coordinator: self-exit requested for duplicate coord "
                f"{coord_id} but `tmux kill-session -t "
                f"{supervisor_mod.session_name_for_agent(coord_id)}` did "
                f"not succeed; this session may persist. Run `fleet gc` "
                f"or kill the duplicate manually after confirming the "
                f"holder is the live coord.\n"
            )
        return 0
    # Issue #84 Phase A: emit DISPATCH blocks BEFORE the JSON summary
    # so the coord agent (Claude) sees them as parseable plain text in
    # the tick output. Each block tells Claude to invoke the Agent
    # tool with run_in_background=true once per block — see SKILL.md
    # "Worker dispatch protocol". Blocks are separated by a blank
    # line so the parser (Claude reasoning over the stdout) can pick
    # them out of multi-block ticks.
    #
    # loop-supervisor-sigpipe-5263: emitting a DISPATCH block is the
    # WHOLE POINT of the tick — if stdout is closed (e.g. the tick was
    # piped through `head -40` which closed the read end), the block
    # never reaches the coord and there is no way to communicate it.
    # Do NOT swallow the BrokenPipeError and pretend the dispatch
    # happened: surface it on stderr and exit non-zero so the operator
    # / harness sees the failure and re-ticks. The coordinator.lock is
    # already released by tick()'s finally before we reach this print
    # loop, so a non-zero exit here doesn't strand the lock.
    try:
        for block in result.dispatch_instructions:
            print(block, file=sys.stdout)
            print(file=sys.stdout)
        sys.stdout.flush()
    except (BrokenPipeError, OSError) as exc:
        # Avoid a second BrokenPipeError on interpreter shutdown flush.
        try:
            sys.stdout.close()
        except OSError:
            pass
        sys.stderr.write(
            f"coordinator: failed to emit DISPATCH block "
            f"({len(result.dispatch_instructions)} pending) to a closed "
            f"stdout (broken pipe: {exc}); the dispatch did NOT reach the "
            f"coord. Re-run the tick with full output captured (never "
            f"`| head`). Exiting non-zero so the harness re-ticks.\n"
        )
        return 2
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
    sys.exit(main(sys.argv[1:]))
