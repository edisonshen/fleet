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
    """

    skipped: bool = False
    reason: str = ""
    parsed_tasks: int = 0
    reconciled: int = 0
    drained: int = 0
    dispatched: int = 0
    raised: int = 0
    errors: list[str] = field(default_factory=list)


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
    lock_fd = _try_lock(lock_path)
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

    # 3. Reconcile in-flight workers.
    reconciled = _reconcile_inflight(f.tasks, project, fleet_bin, home=home)
    pre_reconcile_tasks_by_slug = {t.slug: t for t in f.tasks}
    reconcile_repo = cwd if cap > 1 else ""
    reconcile_tasks_by_slug = pre_reconcile_tasks_by_slug if cap > 1 else None
    for action in reconciled:
        try:
            _apply_reconcile(
                action, project, fleet_bin,
                repo=reconcile_repo,
                tasks_by_slug=reconcile_tasks_by_slug,
            )
            result.reconciled += 1
            if action.raised_to_user:
                result.raised += 1
        except Exception as exc:
            result.errors.append(f"reconcile {action.slug}: {exc}")

    # 4. Drain inbox archive sentinels.
    state_path = project_dir / "coord-state.json"
    state = _load_coord_state(state_path)
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
            )
            result.drained += 1
            if action.raised_to_user:
                result.raised += 1
        except Exception as exc:
            result.errors.append(f"sentinel {action.slug}: {exc}")
    if last_seen:
        state["last_archive_scan_ts"] = last_seen
        _save_coord_state(state_path, state)

    # 5. Dispatch ready tasks under cap.
    # Re-read tasks.md after reconcile/drain so the dispatch-side filter
    # sees the latest in-progress count (mutations went through the
    # fleet CLI — they're durable on disk, we reload).
    try:
        f = parse.read(str(tasks_path))
    except Exception as exc:
        result.errors.append(f"tasks.md re-read failed: {exc}")
        return result
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
            result.dispatched += 1
        except Exception as exc:
            result.errors.append(f"dispatch {action.slug}: {exc}")
    return result


# ---------- lock helpers ----------


def _try_lock(path: Path) -> int | None:
    """Acquire LOCK_EX | LOCK_NB on path. Returns the open fd or None.

    Caller is responsible for unlocking + closing on the success path.
    Re-acquiring across hook fires is the documented pattern (ENG §4.3
    "Coordinator-lock lifecycle") — short-lived Python procs release
    the flock automatically on exit.
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
    import tempfile
    parent = path.parent
    parent.mkdir(parents=True, exist_ok=True)
    fd, tmp = tempfile.mkstemp(prefix=path.name + ".tmp.", dir=str(parent))
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as fh:
            json.dump(state, fh, indent=2, sort_keys=True)
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
            terminal = _worker_terminal_state(project, t.slug, home=home)
            if terminal is not None:
                phase, pr_url, blocked_reason = terminal
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
                    actions.append(action)
                    continue
                if phase == "blocked" and blocked_reason:
                    actions.append(_ReconcileAction(
                        slug=t.slug, new_status="blocked",
                        clear_worker=True,
                        note=f"worker blocked: {blocked_reason}",
                        raised_to_user=True,
                        raise_text=f"{t.slug} blocked: {blocked_reason}",
                    ))
                    continue
                if phase == "failed":
                    actions.append(_ReconcileAction(
                        slug=t.slug, new_status="todo", clear_worker=True,
                        clear_pr_url=True,
                        note="worker failed",
                        raised_to_user=True,
                        raise_text=f"{t.slug} worker failed",
                    ))
                    continue
                # phase=done without pr_url, or phase=blocked
                # without reason — fall through to pr_url + CI; the
                # worker didn't honor the contract.
        if t.pr_url:
            ci = _gh_pr_checks(t.pr_url)
            if ci.all_green and ci.merged:
                actions.append(_ReconcileAction(
                    slug=t.slug, new_status="done", clear_worker=True,
                ))
            elif ci.all_green and not ci.merged:
                actions.append(_ReconcileAction(
                    slug=t.slug, new_status="in-review",
                    raised_to_user=True,
                    raise_text=f"CI green for {t.slug}, ready to merge",
                ))
            elif not ci.mergeable:
                # Rebase needed — keep the existing pr_url; the
                # operator (or a re-dispatch) will rebase the SAME
                # branch onto main, so the PR URL is still the right
                # poll target.
                actions.append(_ReconcileAction(
                    slug=t.slug, new_status="todo", clear_worker=True,
                    note="rebase needed",
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
                ))
            else:
                # CI pending — leave as-is until next tick.
                continue
        else:
            actions.append(_ReconcileAction(
                slug=t.slug, new_status="todo", clear_worker=True,
                note="worker died without PR",
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
        _run_fleet([fleet_bin, "tasks", "set", "--project", project, action.slug, f"pr_url={action.payload}"])
        _run_fleet([fleet_bin, "tasks", "set", "--project", project, action.slug, "status=in-review"])
        _maybe_remove_worktree(action.slug, repo, tasks_by_slug, fleet_bin, project)
    elif action.kind == "blocked_question":
        _run_fleet([fleet_bin, "tasks", "set", "--project", project, action.slug, "status=blocked"])
        if action.payload:
            _run_fleet([fleet_bin, "tasks", "note", "--project", project, action.slug, f"BLOCKED_QUESTION: {action.payload}"])
    elif action.kind == "worker_failed":
        _run_fleet([fleet_bin, "tasks", "set", "--project", project, action.slug, "status=todo"])
        _run_fleet([fleet_bin, "tasks", "set", "--project", project, action.slug, "worker_pid=0"])
        if action.payload:
            _run_fleet([fleet_bin, "tasks", "note", "--project", project, action.slug, f"WORKER_FAILED: {action.payload}"])
        _maybe_remove_worktree(action.slug, repo, tasks_by_slug, fleet_bin, project)
    elif action.kind == "new_task":
        # Wake-only sentinel — nothing to apply. Presence of the file
        # was the wake; dispatch_ready in the same tick will pick up
        # the new task if it's ready.
        return


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
    in_flight_after_dispatch: list[parse.Task] = list(in_progress)

    for t in candidates:
        if active >= cap:
            break
        if conflict.has_conflict(t, in_flight_after_dispatch):
            continue
        # Worktree mode: cap>1. Resolve canonical path via the Go CLI,
        # then `git worktree add`. On any failure we record the error
        # and skip this task — leaving stale state would corrupt the
        # next tick's view of in-flight tasks.
        worker_cwd = cwd
        worker_branch = f"worker/{t.slug}"
        worker_worktree = ""
        if cap > 1:
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
            )
        except dispatch_mod.PromptTooLargeError as exc:
            actions.append(_DispatchAction(slug=t.slug, error=str(exc)))
            continue
        result = dispatch_mod.dispatch_worker(
            t, project=project, cwd=worker_cwd, fleet_bin=fleet_bin,
        )
        if result.error:
            actions.append(_DispatchAction(slug=t.slug, error=result.error))
            continue
        try:
            dispatch_mod.write_worker_inbox(
                result.agent_id, prompt, fleet_home=fleet_home,
            )
        except Exception as exc:
            actions.append(_DispatchAction(
                slug=t.slug, agent_id=result.agent_id,
                error=f"inbox write failed: {exc}",
            ))
            continue
        actions.append(_DispatchAction(
            slug=t.slug, agent_id=result.agent_id, branch=worker_branch,
            worktree=worker_worktree,
        ))
        active += 1
        in_flight_after_dispatch.append(t)
    return actions


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


# ---------- shared CLI helper ----------


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
