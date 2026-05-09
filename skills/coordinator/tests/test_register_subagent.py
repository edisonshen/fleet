"""register_subagent.py tests — issue #94 Phase C.

Strategy: drive `register_subagent.register` / `clear` / `main` against
a tmp_path-rooted FLEET_HOME. Verify coord-state.json is RMW'd safely,
the lock retry is exercised when a peer holds coordinator.lock, and
malformed inputs surface as RegisterError.
"""
from __future__ import annotations

import errno
import fcntl
import json
import os
from pathlib import Path

import pytest

import register_subagent
import supervisor


@pytest.fixture
def fleet_home(tmp_path: Path, monkeypatch) -> Path:
    """Project tree pre-created. coord-state.json is intentionally
    absent — the helper must bootstrap an empty dict on first call."""
    home = tmp_path / "fleet"
    project_dir = home / "projects" / "demo"
    project_dir.mkdir(parents=True)
    (project_dir / ".locks").mkdir()
    monkeypatch.setenv("FLEET_HOME", str(home))
    return home


# ---------- happy paths ----------


def test_register_writes_mapping(fleet_home: Path) -> None:
    register_subagent.register("demo", "alpha-aaaa", "claude-sub-1")
    state = json.loads((fleet_home / "projects" / "demo" / "coord-state.json").read_text())
    assert state["worker_subagent_ids"] == {"alpha-aaaa": "claude-sub-1"}


def test_register_preserves_existing_keys(fleet_home: Path) -> None:
    """Register must not clobber other coord-state fields like
    `worker_agent_ids` or supervisor sub-state."""
    state_path = fleet_home / "projects" / "demo" / "coord-state.json"
    state_path.write_text(json.dumps({
        "worker_agent_ids": {"alpha-aaaa": "aaaaaaaa"},
        "supervisor": {"alpha-aaaa": {"slug": "alpha-aaaa"}},
        "last_archive_scan_ts": 1234.5,
    }))
    register_subagent.register("demo", "alpha-aaaa", "claude-sub-1")
    state = json.loads(state_path.read_text())
    assert state["worker_agent_ids"] == {"alpha-aaaa": "aaaaaaaa"}
    assert state["supervisor"] == {"alpha-aaaa": {"slug": "alpha-aaaa"}}
    assert state["last_archive_scan_ts"] == 1234.5
    assert state["worker_subagent_ids"] == {"alpha-aaaa": "claude-sub-1"}


def test_register_overwrites_same_slug(fleet_home: Path) -> None:
    """A second register for the same slug must overwrite (worker
    re-dispatched after handoff with a fresh Claude subagent_id)."""
    register_subagent.register("demo", "alpha-aaaa", "claude-sub-1")
    register_subagent.register("demo", "alpha-aaaa", "claude-sub-2")
    state = json.loads((fleet_home / "projects" / "demo" / "coord-state.json").read_text())
    assert state["worker_subagent_ids"] == {"alpha-aaaa": "claude-sub-2"}


def test_clear_removes_mapping(fleet_home: Path) -> None:
    register_subagent.register("demo", "alpha-aaaa", "claude-sub-1")
    assert register_subagent.clear("demo", "alpha-aaaa") is True
    state = json.loads((fleet_home / "projects" / "demo" / "coord-state.json").read_text())
    assert state.get("worker_subagent_ids", {}) == {}


def test_clear_unmapped_slug_is_no_op(fleet_home: Path) -> None:
    """Clearing a slug that's not in the map returns False without
    crashing or rewriting coord-state.json (no spurious mtime bump)."""
    # Pre-write an unrelated mapping — clear must not delete it.
    register_subagent.register("demo", "beta-bbbb", "claude-sub-2")
    state_path = fleet_home / "projects" / "demo" / "coord-state.json"
    mtime_before = state_path.stat().st_mtime_ns
    assert register_subagent.clear("demo", "alpha-aaaa") is False
    # No write means mtime is untouched.
    assert state_path.stat().st_mtime_ns == mtime_before
    state = json.loads(state_path.read_text())
    assert state["worker_subagent_ids"] == {"beta-bbbb": "claude-sub-2"}


def test_clear_missing_project_is_no_op(fleet_home: Path) -> None:
    """Clearing in a project dir that doesn't exist is a clean False —
    avoids crashing the CLI on operator typos in --project."""
    assert register_subagent.clear("missing-project", "alpha-aaaa") is False


# ---------- input validation ----------


def test_register_rejects_empty_slug(fleet_home: Path) -> None:
    with pytest.raises(register_subagent.RegisterError, match="slug must be non-empty"):
        register_subagent.register("demo", "", "claude-sub-1")


def test_register_rejects_invalid_subagent_id(fleet_home: Path) -> None:
    """Loose validation rejects empty / whitespace / over-long /
    non-printable. Tested against each invalid case independently."""
    for bad in ("", "   ", "has space", "x" * 129, "claude\x00sub"):
        with pytest.raises(register_subagent.RegisterError, match="subagent_id rejected"):
            register_subagent.register("demo", "alpha-aaaa", bad)


def test_register_rejects_missing_project_dir(tmp_path: Path, monkeypatch) -> None:
    """A project dir that hasn't been bootstrapped (no coord ever ran)
    yields a typed RegisterError so the CLI exits non-zero with a clear
    message — silent no-op would mask the operator typo."""
    monkeypatch.setenv("FLEET_HOME", str(tmp_path / "fleet"))
    with pytest.raises(register_subagent.RegisterError, match="project dir missing"):
        register_subagent.register("never-existed", "alpha-aaaa", "claude-sub-1")


# ---------- locking ----------


def test_register_blocks_when_coord_lock_held(fleet_home: Path, monkeypatch) -> None:
    """When another process holds coordinator.lock, register must fail
    after the retry budget rather than blocking forever. Operator can
    retry from the coord agent on the next dispatch."""
    # Hold the lock externally.
    lock_path = fleet_home / "projects" / "demo" / ".locks" / "coordinator.lock"
    lock_fd = os.open(str(lock_path), os.O_RDWR | os.O_CREAT, 0o644)
    fcntl.flock(lock_fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
    # Shrink retry budget so the test runs in <1s.
    monkeypatch.setattr(register_subagent, "_LOCK_RETRY_ATTEMPTS", 2)
    monkeypatch.setattr(register_subagent, "_LOCK_RETRY_INTERVAL_S", 0.01)
    try:
        with pytest.raises(register_subagent.RegisterError, match="coord lock busy"):
            register_subagent.register("demo", "alpha-aaaa", "claude-sub-1")
    finally:
        try:
            fcntl.flock(lock_fd, fcntl.LOCK_UN)
        finally:
            os.close(lock_fd)


def test_register_recovers_after_lock_released(fleet_home: Path, monkeypatch) -> None:
    """Lock contention that clears within the retry budget succeeds —
    drives the retry loop's recovery branch."""
    lock_path = fleet_home / "projects" / "demo" / ".locks" / "coordinator.lock"
    lock_fd = os.open(str(lock_path), os.O_RDWR | os.O_CREAT, 0o644)
    fcntl.flock(lock_fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
    monkeypatch.setattr(register_subagent, "_LOCK_RETRY_ATTEMPTS", 5)
    monkeypatch.setattr(register_subagent, "_LOCK_RETRY_INTERVAL_S", 0.05)
    # Release the lock from a sleep callback the retry loop will hit on
    # its second attempt. Approximation: monkey-patch sleep to release
    # on first invocation. Deterministic (no wall-clock dependency).
    released = {"done": False}

    real_sleep = register_subagent.time.sleep

    def releasing_sleep(_seconds: float) -> None:
        if not released["done"]:
            try:
                fcntl.flock(lock_fd, fcntl.LOCK_UN)
            finally:
                released["done"] = True
        real_sleep(0)

    monkeypatch.setattr(register_subagent.time, "sleep", releasing_sleep)
    try:
        register_subagent.register("demo", "alpha-aaaa", "claude-sub-1")
    finally:
        try:
            os.close(lock_fd)
        except OSError:
            pass
    state = json.loads((fleet_home / "projects" / "demo" / "coord-state.json").read_text())
    assert state["worker_subagent_ids"] == {"alpha-aaaa": "claude-sub-1"}


# ---------- CLI entry ----------


def test_main_register_happy_path(fleet_home: Path, capsys) -> None:
    rc = register_subagent.main(["--project", "demo", "alpha-aaaa", "claude-sub-1"])
    assert rc == 0
    out = capsys.readouterr().out
    assert "demo:alpha-aaaa -> claude-sub-1" in out


def test_main_register_uses_fleet_project_env(fleet_home: Path, monkeypatch, capsys) -> None:
    """No --project flag → falls back to $FLEET_PROJECT."""
    monkeypatch.setenv("FLEET_PROJECT", "demo")
    rc = register_subagent.main(["alpha-aaaa", "claude-sub-1"])
    assert rc == 0


def test_main_register_no_project(monkeypatch, capsys) -> None:
    """No --project, no $FLEET_PROJECT → exit 2 + stderr message."""
    monkeypatch.delenv("FLEET_PROJECT", raising=False)
    rc = register_subagent.main(["alpha-aaaa", "claude-sub-1"])
    assert rc == 2
    err = capsys.readouterr().err
    assert "--project required" in err


def test_main_register_invalid_input(fleet_home: Path, capsys) -> None:
    rc = register_subagent.main(["--project", "demo", "alpha-aaaa", "has space"])
    assert rc == 1
    err = capsys.readouterr().err
    assert "subagent_id rejected" in err


def test_main_clear_succeeds(fleet_home: Path, capsys) -> None:
    register_subagent.register("demo", "alpha-aaaa", "claude-sub-1")
    rc = register_subagent.main(["--project", "demo", "--clear", "alpha-aaaa"])
    assert rc == 0
    out = capsys.readouterr().out
    assert "cleared demo:alpha-aaaa" in out


def test_main_clear_unmapped_slug_no_op(fleet_home: Path, capsys) -> None:
    rc = register_subagent.main(["--project", "demo", "--clear", "alpha-aaaa"])
    assert rc == 0
    out = capsys.readouterr().out
    assert "not in map" in out


def test_main_subagent_id_required_unless_clear(fleet_home: Path, capsys) -> None:
    """argparse error path — `register --project demo slug` without an
    id and without --clear exits non-zero (argparse exits 2 by default)."""
    with pytest.raises(SystemExit) as exc:
        register_subagent.main(["--project", "demo", "alpha-aaaa"])
    assert exc.value.code != 0


# ---------- atomic write discipline ----------


def test_register_atomic_write_leaves_no_tmp_files(fleet_home: Path) -> None:
    """tmp+rename discipline: a successful register must clean up its
    tmp scratch file. Any leftover .coord-state.json.tmp.* would surface
    as a future read-time noise (and possibly corrupt the supervisor's
    next pass)."""
    register_subagent.register("demo", "alpha-aaaa", "claude-sub-1")
    project_dir = fleet_home / "projects" / "demo"
    leftover = list(project_dir.glob("coord-state.json.tmp.*"))
    assert leftover == [], f"unexpected tmp files: {leftover}"
