"""Integration: a real loop.tick() drives the Python fleetlog emitter and
the once/day retention prune (TASK-PLAN T5 ii/iii via the actual wiring,
not just the helper).

Uses the autouse conftest stubs (resolve-repo -> cwd, lease -> owner,
supervisor disabled) so the tick is hermetic.
"""
from __future__ import annotations

import datetime as _dt
import importlib
import json
from pathlib import Path

import pytest

import loop
import parse


def _write_one_ready_task(project_dir: Path, slug: str = "tick-prune-task") -> None:
    project_dir.mkdir(parents=True, exist_ok=True)
    (project_dir / ".locks").mkdir(exist_ok=True)
    now = _dt.datetime(2026, 5, 6, 10, 0, 0, tzinfo=_dt.timezone.utc)
    task = parse.Task(
        slug=slug, status="ready", priority="P1", worker_pid=0, pr_url="",
        created=now, updated=now, spawned_by="user", depends_on=[],
        spec="spec", acceptance="acc", notes="", dispatch_generation=0,
    )
    parse.write(str(project_dir / "tasks.md"),
                parse.File(schema=parse.SCHEMA_VERSION, tasks=[task], footer=""))


def _stub_acquire(monkeypatch, home: Path, agent_id: str) -> None:
    """Stub dispatch.acquire_coord_prompt_inbox — it execs the real `fleet`
    binary (`fleet claims ...`), which CI has no copy of on PATH. Without
    this stub the tick's dispatch fails with "fleet binary not found" and
    dispatched=0. _apply_dispatch still emits the dispatch.worker fleetlog
    event afterward via the separately-stubbed loop._run_fleet, so the
    tick's fleetlog behavior stays under test. Mirrors test_chokepoint's
    acquire stub and the conftest shell-out seam stubs."""
    monkeypatch.setattr(
        "dispatch.acquire_coord_prompt_inbox",
        lambda *a, **k: str(home / "inbox" / f"{agent_id}.md"),
    )


@pytest.fixture
def home(tmp_path, monkeypatch):
    h = tmp_path / "fleet"
    for sub in ("inbox/archive", "agents", "queue", "logs", "projects"):
        (h / sub).mkdir(parents=True, exist_ok=True)
    monkeypatch.setenv("FLEET_HOME", str(h))
    monkeypatch.delenv("XDG_STATE_HOME", raising=False)
    monkeypatch.delenv("FLEET_AGENT_ID", raising=False)
    # Reload fleetlog so its dir() resolves the new FLEET_HOME.
    import fleetlog
    importlib.reload(fleetlog)
    # loop holds a reference to the module object; reload keeps identity.
    return h


def test_tick_emits_coord_events_and_dispatch(home, monkeypatch):
    project = "fleet"
    pdir = home / "projects" / project
    _write_one_ready_task(pdir)
    monkeypatch.setattr("dispatch.mint_agent_id", lambda: "abcdef01")
    monkeypatch.setattr(loop, "_run_fleet", lambda cmd, timeout_s=30.0: None)
    _stub_acquire(monkeypatch, home, "abcdef01")

    result = loop.tick(
        project, coord_id="", cwd=str(pdir), fleet_home=str(home), cap=1,
    )
    assert result.dispatched == 1

    logdir = home / "logs"
    types = []
    for f in logdir.glob("*.jsonl"):
        for ln in f.read_text().splitlines():
            if ln:
                types.append(json.loads(ln)["type"])
    assert types.count("coord.tick") == 2  # start + end
    assert "dispatch.worker" in types


def test_tick_emits_coord_events_under_single_shot_poll(home, monkeypatch):
    """Rebase-integration guard (PR #238 single-shot × #241 fleetlog).

    The default test path runs poll_interval=0 (the legacy "callers never
    wanted the loop" case). This pins the NEW single-shot path: poll IS
    configured (>0) but the in-turn supervisor is gated OFF by default
    (FLEET_COORD_IN_TURN_SUPERVISOR unset). The coord.tick start/end and
    dispatch.worker emits must still fire from the single-shot mainline —
    NOT from the now-gated _run_supervisor branch. If a future refactor
    moved an emit inside that branch, this test fails while the poll=0
    test stays green. Also asserts the supervisor was never entered."""
    project = "fleet"
    pdir = home / "projects" / project
    _write_one_ready_task(pdir)
    monkeypatch.setenv("FLEET_COORD_POLL_INTERVAL_S", "30")
    monkeypatch.delenv("FLEET_COORD_IN_TURN_SUPERVISOR", raising=False)
    monkeypatch.setattr("dispatch.mint_agent_id", lambda: "abcdef09")
    monkeypatch.setattr(loop, "_run_fleet", lambda cmd, timeout_s=30.0: None)
    _stub_acquire(monkeypatch, home, "abcdef09")

    sup_seen = {"n": 0}
    real_sup = loop._run_supervisor

    def _spy(**kwargs):
        sup_seen["n"] += 1
        return real_sup(**kwargs)

    monkeypatch.setattr(loop, "_run_supervisor", _spy)

    result = loop.tick(
        project, coord_id="", cwd=str(pdir), fleet_home=str(home), cap=1,
    )
    assert result.dispatched == 1
    assert sup_seen["n"] == 0  # single-shot default: supervisor not entered

    logdir = home / "logs"
    types = []
    for f in logdir.glob("*.jsonl"):
        for ln in f.read_text().splitlines():
            if ln:
                types.append(json.loads(ln)["type"])
    assert types.count("coord.tick") == 2  # start + end fire on the single-shot path
    assert "dispatch.worker" in types


def test_tick_prunes_stale_log_when_no_marker(home, monkeypatch):
    project = "fleet"
    _write_one_ready_task(home / "projects" / project)
    monkeypatch.setattr("dispatch.mint_agent_id", lambda: "abcdef02")
    monkeypatch.setattr(loop, "_run_fleet", lambda cmd, timeout_s=30.0: None)
    _stub_acquire(monkeypatch, home, "abcdef02")

    logdir = home / "logs"
    stale = logdir / "fleet-2000-01-01-coord-1-1.jsonl"
    stale.write_text("{}\n")

    loop.tick(project, coord_id="", cwd=str(home / "projects" / project),
              fleet_home=str(home))

    assert not stale.exists()              # tick ran maybe_prune_daily
    assert (logdir / ".last-prune").exists()


def test_tick_throttles_prune_when_marker_fresh(home, monkeypatch):
    import os
    import time
    project = "fleet"
    _write_one_ready_task(home / "projects" / project)
    monkeypatch.setattr("dispatch.mint_agent_id", lambda: "abcdef03")
    monkeypatch.setattr(loop, "_run_fleet", lambda cmd, timeout_s=30.0: None)
    _stub_acquire(monkeypatch, home, "abcdef03")

    logdir = home / "logs"
    stale = logdir / "fleet-2000-01-01-coord-1-1.jsonl"
    stale.write_text("{}\n")
    marker = logdir / ".last-prune"
    marker.write_text("")
    os.utime(marker, (time.time(), time.time()))  # pruned just now -> throttle

    loop.tick(project, coord_id="", cwd=str(home / "projects" / project),
              fleet_home=str(home))

    assert stale.exists()  # throttle skipped the scan; stale survives


def test_tick_lock_busy_emits_fleetlog(home, monkeypatch):
    """T2b: a lock-busy early-return emits a coord.tick event so the debug
    log has a record of the skipped tick (not just a gap)."""
    import fcntl
    import os
    import fleetlog
    import importlib
    importlib.reload(fleetlog)

    project = "fleet"
    pdir = home / "projects" / project
    pdir.mkdir(parents=True, exist_ok=True)
    (pdir / ".locks").mkdir(exist_ok=True)

    # Hold the coordinator lock from a separate fd to force lock-busy.
    lock_path = pdir / ".locks" / "coordinator.lock"
    lock_path.touch()
    holder_fd = os.open(str(lock_path), os.O_RDWR)
    fcntl.flock(holder_fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
    try:
        result = loop.tick(project, coord_id="some-coord", cwd=str(pdir),
                           fleet_home=str(home))
        assert result.skipped
        assert result.reason in ("lock-busy", "duplicate-coord-self-exit")

        # The fleetlog must have a coord.tick event with skipped=True.
        log_dir = Path(fleetlog.dir())
        events = []
        for f in sorted(log_dir.glob("*.jsonl")):
            for ln in f.read_text().splitlines():
                if ln:
                    events.append(json.loads(ln))
        skipped = [e for e in events
                   if e.get("type") == "coord.tick"
                   and e.get("data", {}).get("skipped")]
        assert skipped, f"no skipped coord.tick fleetlog event; events: {events}"
    finally:
        fcntl.flock(holder_fd, fcntl.LOCK_UN)
        os.close(holder_fd)


def test_tick_survives_logging_failure(home, monkeypatch):
    """T2: a raising fleetlog.log must NOT propagate — the tick still
    completes and dispatches (logging is fire-and-forget)."""
    project = "fleet"
    pdir = home / "projects" / project
    _write_one_ready_task(pdir)
    monkeypatch.setattr("dispatch.mint_agent_id", lambda: "abcdef04")
    monkeypatch.setattr(loop, "_run_fleet", lambda cmd, timeout_s=30.0: None)
    _stub_acquire(monkeypatch, home, "abcdef04")

    def _boom(*a, **k):
        raise RuntimeError("emit failed")

    monkeypatch.setattr(loop.fleetlog_mod, "log", _boom)
    monkeypatch.setattr(loop.fleetlog_mod, "maybe_prune_daily", _boom)

    result = loop.tick(
        project, coord_id="", cwd=str(pdir), fleet_home=str(home), cap=1,
    )
    assert result.dispatched == 1
    assert not result.skipped


def _assert_coord_lock_free(pdir: Path) -> None:
    """A fresh non-blocking exclusive flock must acquire coordinator.lock.
    flock on a distinct open-file-description contends even within the same
    process, so a leaked-but-still-held lock_fd makes LOCK_NB raise
    BlockingIOError. Used by every 'lock released on BaseException' test."""
    import fcntl
    import os

    lock_path = pdir / ".locks" / "coordinator.lock"
    fd = os.open(str(lock_path), os.O_RDWR)
    try:
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        fcntl.flock(fd, fcntl.LOCK_UN)
    finally:
        os.close(fd)


def _raise_on_msg(real_log, needle: str):
    """Wrap fleetlog.log so an emit whose msg contains `needle` raises a
    BaseException (KeyboardInterrupt) — _flog only swallows Exception, so it
    propagates and exercises the lock-release-before-emit ordering."""

    def _wrapped(comp, evt, lvl="info", **fields):
        if needle in (fields.get("msg") or ""):
            raise KeyboardInterrupt(f"simulated signal during {needle!r} emit")
        return real_log(comp, evt, lvl, **fields)

    return _wrapped


def test_tick_releases_lock_when_end_event_raises_base_exception(home, monkeypatch):
    """Regression: the coordinator lock must be released even if the
    'coord tick end' fleetlog emit raises a BaseException (KeyboardInterrupt /
    SystemExit) — _flog only swallows Exception, so a BaseException propagates.
    The unlock must run BEFORE the end emit; otherwise the lock leaks for the
    process lifetime and wedges every competing coord with lock-busy."""
    project = "fleet"
    pdir = home / "projects" / project
    _write_one_ready_task(pdir)
    monkeypatch.setattr("dispatch.mint_agent_id", lambda: "abcdef05")
    monkeypatch.setattr(loop, "_run_fleet", lambda cmd, timeout_s=30.0: None)
    _stub_acquire(monkeypatch, home, "abcdef05")

    monkeypatch.setattr(loop.fleetlog_mod, "log",
                        _raise_on_msg(loop.fleetlog_mod.log, "coord tick end"))

    with pytest.raises(KeyboardInterrupt):
        loop.tick(project, coord_id="", cwd=str(pdir), fleet_home=str(home))

    _assert_coord_lock_free(pdir)


def test_tick_releases_lock_when_start_event_raises_base_exception(home, monkeypatch):
    """Regression: the 'coord tick start' emit runs while the lock is held; a
    BaseException there must still release the lock (start emit lives inside
    the try whose finally unlocks)."""
    project = "fleet"
    pdir = home / "projects" / project
    _write_one_ready_task(pdir)

    monkeypatch.setattr(loop.fleetlog_mod, "log",
                        _raise_on_msg(loop.fleetlog_mod.log, "coord tick start"))

    with pytest.raises(KeyboardInterrupt):
        loop.tick(project, coord_id="", cwd=str(pdir), fleet_home=str(home))

    _assert_coord_lock_free(pdir)


def test_tick_releases_lock_when_refuse_stale_event_raises_base_exception(home, monkeypatch):
    """Regression: _refuse_stale (repo-resolve refusal) emits while holding the
    lock; a BaseException there must still release it (unlock runs first)."""
    project = "fleet"
    pdir = home / "projects" / project
    pdir.mkdir(parents=True, exist_ok=True)
    # Make the repo binder refuse so tick takes the _refuse_stale path.
    monkeypatch.setattr(
        loop, "_resolve_repo_fn",
        lambda project, *, home, fleet_bin="fleet", cwd=None: (None, "binder refused"),
    )
    monkeypatch.setattr(loop.fleetlog_mod, "log",
                        _raise_on_msg(loop.fleetlog_mod.log, "repo-unresolved"))

    with pytest.raises(KeyboardInterrupt):
        loop.tick(project, coord_id="", cwd=str(pdir), fleet_home=str(home))

    _assert_coord_lock_free(pdir)


def test_tick_releases_lock_when_postlock_fence_event_raises_base_exception(home, monkeypatch):
    """Regression: the post-lock lease-fence emit runs while holding the lock;
    a BaseException there must still release it (unlock runs first). The
    pre-lock proof passes, the post-lock re-fence returns 'fenced'."""
    project = "fleet"
    pdir = home / "projects" / project
    pdir.mkdir(parents=True, exist_ok=True)

    calls = {"n": 0}

    def _fence_second(project, *, home, fleet_bin="fleet"):
        calls["n"] += 1
        return "owner" if calls["n"] == 1 else "fenced"

    monkeypatch.setattr(loop, "_lease_check_fn", _fence_second)
    monkeypatch.setattr(loop.fleetlog_mod, "log",
                        _raise_on_msg(loop.fleetlog_mod.log, "post-lock"))

    with pytest.raises(KeyboardInterrupt):
        loop.tick(project, coord_id="", cwd=str(pdir), fleet_home=str(home))

    _assert_coord_lock_free(pdir)
