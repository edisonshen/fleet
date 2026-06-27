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

    result = loop.tick(project, coord_id="", cwd=str(pdir), fleet_home=str(home))
    assert result.dispatched == 1

    logdir = home / "logs"
    types = []
    for f in logdir.glob("*.jsonl"):
        for ln in f.read_text().splitlines():
            if ln:
                types.append(json.loads(ln)["type"])
    assert types.count("coord.tick") == 2  # start + end
    assert "dispatch.worker" in types


def test_tick_prunes_stale_log_when_no_marker(home, monkeypatch):
    project = "fleet"
    _write_one_ready_task(home / "projects" / project)
    monkeypatch.setattr("dispatch.mint_agent_id", lambda: "abcdef02")
    monkeypatch.setattr(loop, "_run_fleet", lambda cmd, timeout_s=30.0: None)

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

    def _boom(*a, **k):
        raise RuntimeError("emit failed")

    monkeypatch.setattr(loop.fleetlog_mod, "log", _boom)
    monkeypatch.setattr(loop.fleetlog_mod, "maybe_prune_daily", _boom)

    result = loop.tick(project, coord_id="", cwd=str(pdir), fleet_home=str(home))
    assert result.dispatched == 1
    assert not result.skipped
