"""supervisor.py tests — issue #79.

Strategy: drive `supervisor.run_supervisor` with fakes (sleep_fn, now_fn,
refresh_probes, reconcile_one, write_state) so we can step the loop
deterministically through any number of polls. Helpers (`is_stuck_idle`,
`nudge_worker`, `safe_mtime`, `tmux_session_alive`) are tested in
isolation without touching the loop body.

Recovery-ladder tests use the env knobs at small values
(stuck_polls=1, threshold=1) so a single pass triggers nudge → escalate
→ block. The full coord-state.json round-trip is exercised by the
"recovery state persisted" test.
"""
from __future__ import annotations

import io
import json
import os
import subprocess
from dataclasses import dataclass
from pathlib import Path
from unittest.mock import patch

import pytest

import supervisor


# ---------- helpers ----------


def _write_state_json(state_path: Path, *, phase: str, updated_at: str = "") -> None:
    state_path.parent.mkdir(parents=True, exist_ok=True)
    payload = {"phase": phase, "updated_at": updated_at, "slug": state_path.parent.name}
    state_path.write_text(json.dumps(payload), encoding="utf-8")


@dataclass
class _FakeTask:
    """Minimal stand-in for parse.Task (only attrs supervisor needs)."""

    slug: str
    status: str = "in-progress"


@pytest.fixture
def fleet_home(tmp_path: Path) -> Path:
    home = tmp_path / "fleet"
    home.mkdir()
    (home / "inbox").mkdir()
    (home / "projects" / "fleet").mkdir(parents=True)
    return home


# ---------- safe_mtime ----------


def test_safe_mtime_returns_zero_on_missing_file(tmp_path: Path) -> None:
    assert supervisor.safe_mtime(tmp_path / "no-such-file") == 0.0


def test_safe_mtime_returns_real_mtime(tmp_path: Path) -> None:
    p = tmp_path / "x"
    p.write_text("hi")
    m = supervisor.safe_mtime(p)
    assert m > 0


# ---------- tmux_session_alive ----------


def test_tmux_session_alive_returns_false_on_empty_session() -> None:
    assert supervisor.tmux_session_alive("") is False


def test_tmux_session_alive_handles_missing_tmux(monkeypatch) -> None:
    """tmux not on PATH → return False (conservative)."""
    def boom(*a, **kw):
        raise FileNotFoundError("tmux")
    monkeypatch.setattr(subprocess, "run", boom)
    assert supervisor.tmux_session_alive("fleet-aaaaaaaa") is False


def test_tmux_session_alive_returns_true_on_exit_zero(monkeypatch) -> None:
    monkeypatch.setattr(
        subprocess, "run",
        lambda *a, **kw: subprocess.CompletedProcess(args=[], returncode=0),
    )
    assert supervisor.tmux_session_alive("fleet-aaaaaaaa") is True


def test_tmux_session_alive_returns_false_on_exit_nonzero(monkeypatch) -> None:
    monkeypatch.setattr(
        subprocess, "run",
        lambda *a, **kw: subprocess.CompletedProcess(args=[], returncode=1),
    )
    assert supervisor.tmux_session_alive("fleet-aaaaaaaa") is False


# ---------- is_stuck_idle ----------


def _now() -> float:
    return 10_000.0


def _sup(slug: str = "x", consecutive: int = 5) -> supervisor.WorkerSupervisorState:
    return supervisor.WorkerSupervisorState(
        slug=slug, consecutive_stuck_polls=consecutive,
    )


def _cfg(**overrides) -> supervisor.SupervisorConfig:
    base = dict(
        poll_interval_s=30, stuck_check_every=10,
        stuck_threshold_s=180, stuck_polls=3, nudge_cooldown_s=120,
        poll_max_s=14400,
    )
    base.update(overrides)
    return supervisor.SupervisorConfig(**base)


def test_stuck_idle_detection_all_four_conditions() -> None:
    """All four conditions hold → is_stuck_idle returns True."""
    state = {
        "phase": "tdd-red",
        "updated_at": "1970-01-01T00:00:00Z",  # very old
    }
    assert supervisor.is_stuck_idle(
        state, _sup(consecutive=3),
        cfg=_cfg(), session_alive=True, now_unix=_now(),
    )


def test_stuck_idle_skips_terminal_workers() -> None:
    """Terminal phase → never stuck."""
    state = {"phase": "done", "updated_at": "1970-01-01T00:00:00Z"}
    assert supervisor.is_stuck_idle(
        state, _sup(consecutive=10),
        cfg=_cfg(), session_alive=True, now_unix=_now(),
    ) is False


def test_stuck_idle_skips_recently_progressing_workers() -> None:
    """Heartbeat within threshold → not stuck."""
    # heartbeat 60 s ago, threshold 180 → fresh
    from datetime import datetime, timezone
    fresh = datetime.fromtimestamp(_now() - 60, tz=timezone.utc).isoformat()
    state = {"phase": "tdd-red", "updated_at": fresh}
    assert supervisor.is_stuck_idle(
        state, _sup(consecutive=10),
        cfg=_cfg(), session_alive=True, now_unix=_now(),
    ) is False


def test_stuck_idle_skips_dead_session_workers() -> None:
    """tmux session gone → don't escalate (no one to nudge)."""
    state = {"phase": "tdd-red", "updated_at": "1970-01-01T00:00:00Z"}
    assert supervisor.is_stuck_idle(
        state, _sup(consecutive=10),
        cfg=_cfg(), session_alive=False, now_unix=_now(),
    ) is False


def test_stuck_idle_requires_consecutive_polls() -> None:
    """Counter below threshold → not stuck (transient idle ignored)."""
    state = {"phase": "tdd-red", "updated_at": "1970-01-01T00:00:00Z"}
    assert supervisor.is_stuck_idle(
        state, _sup(consecutive=2),  # below stuck_polls=3
        cfg=_cfg(), session_alive=True, now_unix=_now(),
    ) is False


def test_stuck_idle_skips_when_no_state_json() -> None:
    """No state.json → can't judge, skip recovery."""
    assert supervisor.is_stuck_idle(
        None, _sup(consecutive=10),
        cfg=_cfg(), session_alive=True, now_unix=_now(),
    ) is False


def test_consecutive_stuck_polls_resets_on_phase_change(
    fleet_home: Path, monkeypatch,
) -> None:
    """When the worker's phase advances mid-stuck-check, the counter
    must reset — that's the "worker IS making progress" signal."""
    # Build supervisor state already accumulated.
    coord_state_path = fleet_home / "projects" / "fleet" / "coord-state.json"
    coord_state_path.parent.mkdir(parents=True, exist_ok=True)
    initial = {
        "supervisor": {
            "alpha-aaaa": {
                "last_phase": "tdd-red",
                "consecutive_stuck_polls": 2,
                "nudged_at": 0.0,
                "escalated_at": 0.0,
            }
        }
    }
    coord_state_path.write_text(json.dumps(initial), encoding="utf-8")

    # Worker now in phase tdd-green — different from last_phase=tdd-red.
    state_path = (
        fleet_home / "projects" / "fleet" / "workers" / "alpha-aaaa" / "state.json"
    )
    _write_state_json(state_path, phase="tdd-green", updated_at="1970-01-01T00:00:00Z")

    probes = [supervisor.WorkerProbe(
        slug="alpha-aaaa",
        state_path=state_path,
        agent_id="aaaaaaaa",
        tmux_session="fleet-aaaaaaaa",
    )]
    # Stub tmux as alive so we'd reach recovery if not for phase change.
    monkeypatch.setattr(supervisor, "tmux_session_alive", lambda s: True)

    res = supervisor._run_stuck_check_pass(
        probes=probes, project="fleet",
        home=fleet_home, fleet_bin="fleet",
        cfg=_cfg(stuck_polls=3, stuck_threshold_s=10),
        now_unix=_now(),
        log_stream=io.StringIO(),
    )
    # No nudge fired — counter was reset to 0, can't reach stuck_polls
    # in one pass (incremented to 1, below threshold 3).
    assert res.nudges == 0
    after = json.loads(coord_state_path.read_text())
    sup_after = after["supervisor"]["alpha-aaaa"]
    # Counter reset → 0 → +1 from this pass = 1.
    assert sup_after["consecutive_stuck_polls"] == 1
    assert sup_after["last_phase"] == "tdd-green"


# ---------- nudge_worker ----------


def test_nudge_writes_inbox_with_correct_body(fleet_home: Path) -> None:
    target = supervisor.nudge_worker("aaaaaaaa", fleet_home=fleet_home)
    assert target
    body = (fleet_home / "inbox" / "aaaaaaaa.md").read_text()
    assert "[OPERATOR]" in body
    assert "appear idle" in body
    assert "/coordinator-helper" in body


def test_nudge_rejects_invalid_agent_id(fleet_home: Path) -> None:
    """Bad agent_id → no inbox file written, returns ""."""
    assert supervisor.nudge_worker("not-hex!!", fleet_home=fleet_home) == ""
    assert not (fleet_home / "inbox" / "not-hex!!.md").exists()


def test_nudge_respects_cooldown(fleet_home: Path, monkeypatch) -> None:
    """A nudge already recent (within cooldown) doesn't re-fire."""
    coord_state_path = fleet_home / "projects" / "fleet" / "coord-state.json"
    coord_state_path.parent.mkdir(parents=True, exist_ok=True)
    initial = {
        "supervisor": {
            "alpha-aaaa": {
                "last_phase": "tdd-red",
                "consecutive_stuck_polls": 5,
                # Nudged 30 s ago — under default cooldown.
                "nudged_at": _now() - 30,
                "escalated_at": 0.0,
            }
        },
        "worker_agent_ids": {"alpha-aaaa": "aaaaaaaa"},
    }
    coord_state_path.write_text(json.dumps(initial), encoding="utf-8")
    state_path = (
        fleet_home / "projects" / "fleet" / "workers" / "alpha-aaaa" / "state.json"
    )
    _write_state_json(state_path, phase="tdd-red", updated_at="1970-01-01T00:00:00Z")

    probes = [supervisor.WorkerProbe(
        slug="alpha-aaaa", state_path=state_path,
        agent_id="aaaaaaaa", tmux_session="fleet-aaaaaaaa",
    )]
    monkeypatch.setattr(supervisor, "tmux_session_alive", lambda s: True)

    # Block fleet CLI subprocess so escalate_to_operator can't accidentally
    # mark task as blocked. (Cooldown gate prevents escalate; this is a
    # belt-and-suspenders.)
    monkeypatch.setattr(
        supervisor.subprocess, "run",
        lambda *a, **kw: subprocess.CompletedProcess(args=[], returncode=0),
    )

    res = supervisor._run_stuck_check_pass(
        probes=probes, project="fleet",
        home=fleet_home, fleet_bin="fleet",
        cfg=_cfg(stuck_polls=3, stuck_threshold_s=10, nudge_cooldown_s=120),
        now_unix=_now(),
        log_stream=io.StringIO(),
    )
    # nudged_at already set, escalated_at not set, cooldown not elapsed.
    # → no nudge re-fired, no escalation yet.
    assert res.nudges == 0
    assert res.escalations == 0


# ---------- escalate ----------


def test_escalation_marks_task_blocked(
    fleet_home: Path, monkeypatch,
) -> None:
    """Second pass post-nudge marks the task blocked + appends note."""
    coord_state_path = fleet_home / "projects" / "fleet" / "coord-state.json"
    coord_state_path.parent.mkdir(parents=True, exist_ok=True)
    initial = {
        "supervisor": {
            "alpha-aaaa": {
                "last_phase": "tdd-red",
                "consecutive_stuck_polls": 5,
                # Nudged in the past, beyond cooldown.
                "nudged_at": _now() - 1000,
                "escalated_at": 0.0,
            }
        },
        "worker_agent_ids": {"alpha-aaaa": "aaaaaaaa"},
    }
    coord_state_path.write_text(json.dumps(initial), encoding="utf-8")
    state_path = (
        fleet_home / "projects" / "fleet" / "workers" / "alpha-aaaa" / "state.json"
    )
    _write_state_json(state_path, phase="tdd-red", updated_at="1970-01-01T00:00:00Z")

    probes = [supervisor.WorkerProbe(
        slug="alpha-aaaa", state_path=state_path,
        agent_id="aaaaaaaa", tmux_session="fleet-aaaaaaaa",
    )]
    monkeypatch.setattr(supervisor, "tmux_session_alive", lambda s: True)

    calls: list[list[str]] = []

    def fake_run(cmd, *a, **kw):
        calls.append(list(cmd))
        return subprocess.CompletedProcess(args=cmd, returncode=0)

    monkeypatch.setattr(supervisor.subprocess, "run", fake_run)

    res = supervisor._run_stuck_check_pass(
        probes=probes, project="fleet",
        home=fleet_home, fleet_bin="fleet",
        cfg=_cfg(stuck_polls=3, stuck_threshold_s=10, nudge_cooldown_s=60),
        now_unix=_now(),
        log_stream=io.StringIO(),
    )
    assert res.escalations == 1
    # Status flipped to blocked.
    set_calls = [c for c in calls if "set" in c]
    assert any("status=blocked" in c for c in set_calls), set_calls
    # STUCK_IDLE_ESCALATED note appended.
    note_calls = [c for c in calls if "note" in c]
    assert any("STUCK_IDLE_ESCALATED" in c[-1] for c in note_calls), note_calls


# ---------- block phase ----------


def test_block_phase_after_persistent_stuck(
    fleet_home: Path, monkeypatch,
) -> None:
    """Third pass post-escalate runs `fleet workers update --phase blocked`."""
    coord_state_path = fleet_home / "projects" / "fleet" / "coord-state.json"
    coord_state_path.parent.mkdir(parents=True, exist_ok=True)
    initial = {
        "supervisor": {
            "alpha-aaaa": {
                "last_phase": "tdd-red",
                "consecutive_stuck_polls": 5,
                "nudged_at": _now() - 1000,
                "escalated_at": _now() - 200,  # past cooldown of 60
            }
        },
        "worker_agent_ids": {"alpha-aaaa": "aaaaaaaa"},
    }
    coord_state_path.write_text(json.dumps(initial), encoding="utf-8")
    state_path = (
        fleet_home / "projects" / "fleet" / "workers" / "alpha-aaaa" / "state.json"
    )
    _write_state_json(state_path, phase="tdd-red", updated_at="1970-01-01T00:00:00Z")

    probes = [supervisor.WorkerProbe(
        slug="alpha-aaaa", state_path=state_path,
        agent_id="aaaaaaaa", tmux_session="fleet-aaaaaaaa",
    )]
    monkeypatch.setattr(supervisor, "tmux_session_alive", lambda s: True)

    calls: list[list[str]] = []
    monkeypatch.setattr(
        supervisor.subprocess, "run",
        lambda cmd, *a, **kw: (
            calls.append(list(cmd))
            or subprocess.CompletedProcess(args=cmd, returncode=0)
        ),
    )

    res = supervisor._run_stuck_check_pass(
        probes=probes, project="fleet",
        home=fleet_home, fleet_bin="fleet",
        cfg=_cfg(stuck_polls=3, stuck_threshold_s=10, nudge_cooldown_s=60),
        now_unix=_now(),
        log_stream=io.StringIO(),
    )
    assert res.blocks == 1
    workers_update = [
        c for c in calls if c[1:3] == ["workers", "update"]
    ]
    assert workers_update, calls
    assert "--phase" in workers_update[0]
    assert "blocked" in workers_update[0]


# ---------- recovery state persistence ----------


def test_recovery_state_persisted_in_coord_state_json(
    fleet_home: Path, monkeypatch,
) -> None:
    """After a nudge, coord-state.json contains the supervisor block."""
    coord_state_path = fleet_home / "projects" / "fleet" / "coord-state.json"
    coord_state_path.parent.mkdir(parents=True, exist_ok=True)
    coord_state_path.write_text(json.dumps({
        "worker_agent_ids": {"alpha-aaaa": "aaaaaaaa"},
        "supervisor": {
            "alpha-aaaa": {
                "last_phase": "tdd-red",
                "consecutive_stuck_polls": 5,  # already accumulated
                "nudged_at": 0.0, "escalated_at": 0.0,
            },
        },
    }), encoding="utf-8")
    state_path = (
        fleet_home / "projects" / "fleet" / "workers" / "alpha-aaaa" / "state.json"
    )
    _write_state_json(state_path, phase="tdd-red", updated_at="1970-01-01T00:00:00Z")

    probes = [supervisor.WorkerProbe(
        slug="alpha-aaaa", state_path=state_path,
        agent_id="aaaaaaaa", tmux_session="fleet-aaaaaaaa",
    )]
    monkeypatch.setattr(supervisor, "tmux_session_alive", lambda s: True)
    monkeypatch.setattr(
        supervisor.subprocess, "run",
        lambda cmd, *a, **kw: subprocess.CompletedProcess(args=cmd, returncode=0),
    )

    supervisor._run_stuck_check_pass(
        probes=probes, project="fleet",
        home=fleet_home, fleet_bin="fleet",
        cfg=_cfg(stuck_polls=3, stuck_threshold_s=10),
        now_unix=_now(),
        log_stream=io.StringIO(),
    )
    after = json.loads(coord_state_path.read_text())
    sup = after["supervisor"]["alpha-aaaa"]
    # Nudge fired → nudged_at populated.
    assert sup["nudged_at"] == pytest.approx(_now())
    # Counter incremented (was 5, +1 = 6).
    assert sup["consecutive_stuck_polls"] == 6


# ---------- supervisor loop driver ----------


def _drive_loop(
    *,
    probes_seq,
    cfg,
    sleep_calls=None,
    fleet_home_=None,
    log=None,
    reconcile_one=None,
    write_state=None,
    project="fleet",
):
    """Run supervisor.run_supervisor with deterministic fakes.

    `probes_seq` is a list of probe-lists, one per refresh_probes() call.
    When exhausted the function returns []. Time is monotonic in 30 s
    ticks unless `cfg.poll_interval_s` is overridden.
    """
    sleep_calls = sleep_calls if sleep_calls is not None else []
    log = log or io.StringIO()
    fleet_home_ = fleet_home_ or Path("/tmp")

    seq_iter = iter(probes_seq)

    def fake_refresh():
        try:
            return next(seq_iter)
        except StopIteration:
            return []

    fake_now_state = {"t": 0.0}

    def fake_now():
        return fake_now_state["t"]

    def fake_sleep(secs):
        sleep_calls.append(secs)
        fake_now_state["t"] += secs

    return supervisor.run_supervisor(
        cfg=cfg, project=project, home=fleet_home_,
        fleet_bin="fleet",
        sleep_fn=fake_sleep, now_fn=fake_now,
        refresh_probes=fake_refresh,
        reconcile_one=reconcile_one or (lambda p: None),
        write_state=write_state or (lambda: None),
        log_stream=log,
    )


def test_supervisor_loop_polls_until_all_workers_terminal(
    fleet_home: Path,
) -> None:
    """Loop exits cleanly when refresh_probes returns []."""
    state_path = (
        fleet_home / "projects" / "fleet" / "workers" / "alpha-aaaa" / "state.json"
    )
    _write_state_json(state_path, phase="tdd-red", updated_at="1970-01-01T00:00:00Z")
    probe = supervisor.WorkerProbe(
        slug="alpha-aaaa", state_path=state_path,
        agent_id="aaaaaaaa", tmux_session="fleet-aaaaaaaa",
    )
    sleep_calls: list[float] = []
    log = io.StringIO()
    res = _drive_loop(
        # iter 1: still alive. iter 2: terminal (empty list → exit).
        probes_seq=[[probe], []],
        cfg=_cfg(stuck_check_every=0),
        sleep_calls=sleep_calls,
        fleet_home_=fleet_home,
        log=log,
    )
    assert res.exit_reason == "all-terminal"
    assert res.iterations == 1  # one sleep, then exit
    assert "all workers terminal" in log.getvalue()


def test_supervisor_loop_respects_max_duration(fleet_home: Path) -> None:
    """When elapsed exceeds poll_max_s the loop exits."""
    state_path = (
        fleet_home / "projects" / "fleet" / "workers" / "alpha-aaaa" / "state.json"
    )
    _write_state_json(state_path, phase="tdd-red", updated_at="1970-01-01T00:00:00Z")
    probe = supervisor.WorkerProbe(
        slug="alpha-aaaa", state_path=state_path,
        agent_id="aaaaaaaa", tmux_session="fleet-aaaaaaaa",
    )
    res = _drive_loop(
        probes_seq=[[probe]] * 100,  # never goes terminal
        cfg=_cfg(poll_interval_s=30, poll_max_s=60, stuck_check_every=0),
        fleet_home_=fleet_home,
    )
    assert res.exit_reason == "max-duration"
    # 60 s budget, 30 s polls → at most 2 iters before exit.
    assert res.iterations <= 2


def test_supervisor_loop_disabled_when_poll_interval_zero(
    fleet_home: Path,
) -> None:
    """poll_interval_s=0 → supervisor returns immediately."""
    sleep_calls: list[float] = []
    res = _drive_loop(
        probes_seq=[],  # never inspected
        cfg=_cfg(poll_interval_s=0),
        sleep_calls=sleep_calls,
        fleet_home_=fleet_home,
    )
    assert res.exit_reason == "supervisor-disabled"
    assert res.iterations == 0
    assert sleep_calls == []


def test_mtime_change_triggers_reconcile_for_one_worker(
    fleet_home: Path,
) -> None:
    """A worker whose state.json mtime advances gets reconciled exactly
    once per change. Other workers stay untouched."""
    a_path = (
        fleet_home / "projects" / "fleet" / "workers" / "alpha-aaaa" / "state.json"
    )
    b_path = (
        fleet_home / "projects" / "fleet" / "workers" / "beta-bbbb" / "state.json"
    )
    _write_state_json(a_path, phase="tdd-red", updated_at="2026-01-01T00:00:00Z")
    _write_state_json(b_path, phase="tdd-red", updated_at="2026-01-01T00:00:00Z")
    probes = [
        supervisor.WorkerProbe(
            slug="alpha-aaaa", state_path=a_path,
            agent_id="aaaaaaaa", tmux_session="fleet-aaaaaaaa",
        ),
        supervisor.WorkerProbe(
            slug="beta-bbbb", state_path=b_path,
            agent_id="bbbbbbbb", tmux_session="fleet-bbbbbbbb",
        ),
    ]
    reconciled: list[str] = []

    def reconcile(p):
        reconciled.append(p.slug)
        # Bump alpha's mtime mid-loop so the next iteration also sees a
        # change. Beta stays untouched so it never reconciles.
        if p.slug == "alpha-aaaa":
            os.utime(a_path, None)

    # 3 iterations, then probes empty → exit. We bump alpha's mtime once
    # before iter 1 so it triggers reconcile on iter 1.
    seq_iter = iter([probes, probes, probes, []])
    fake_now_state = {"t": 0.0}

    def fake_refresh():
        try:
            return next(seq_iter)
        except StopIteration:
            return []

    sleep_count = {"n": 0}

    def fake_sleep(s):
        # Bump alpha's mtime AFTER each sleep so the next mtime
        # comparison sees the change. Touch beta only once mid-loop to
        # confirm targeted reconcile.
        fake_now_state["t"] += s
        sleep_count["n"] += 1
        # Bump alpha every iteration; beta only on iter 2.
        os.utime(a_path, None)
        if sleep_count["n"] == 2:
            os.utime(b_path, None)

    res = supervisor.run_supervisor(
        cfg=_cfg(stuck_check_every=0),
        project="fleet", home=fleet_home, fleet_bin="fleet",
        sleep_fn=fake_sleep, now_fn=lambda: fake_now_state["t"],
        refresh_probes=fake_refresh,
        reconcile_one=reconcile,
        write_state=lambda: None,
        log_stream=io.StringIO(),
    )
    # Alpha reconciled every iteration, beta only iter 2.
    alpha_count = reconciled.count("alpha-aaaa")
    beta_count = reconciled.count("beta-bbbb")
    assert alpha_count >= 2
    assert beta_count == 1


def test_mtime_unchanged_skips_reconcile(fleet_home: Path) -> None:
    """No mtime change → no reconcile_one calls."""
    a_path = (
        fleet_home / "projects" / "fleet" / "workers" / "alpha-aaaa" / "state.json"
    )
    _write_state_json(a_path, phase="tdd-red", updated_at="2026-01-01T00:00:00Z")
    probe = supervisor.WorkerProbe(
        slug="alpha-aaaa", state_path=a_path,
        agent_id="aaaaaaaa", tmux_session="fleet-aaaaaaaa",
    )
    reconciled: list[str] = []
    res = _drive_loop(
        probes_seq=[[probe], [probe], [probe], []],
        cfg=_cfg(stuck_check_every=0),
        fleet_home_=fleet_home,
        reconcile_one=lambda p: reconciled.append(p.slug),
    )
    assert reconciled == []
    assert res.iterations == 3


def test_stuck_check_runs_only_every_n_polls(
    fleet_home: Path, monkeypatch,
) -> None:
    """Stuck-check runs every Nth poll, not every poll."""
    a_path = (
        fleet_home / "projects" / "fleet" / "workers" / "alpha-aaaa" / "state.json"
    )
    _write_state_json(a_path, phase="tdd-red", updated_at="1970-01-01T00:00:00Z")
    probe = supervisor.WorkerProbe(
        slug="alpha-aaaa", state_path=a_path,
        agent_id="aaaaaaaa", tmux_session="fleet-aaaaaaaa",
    )
    # Stub out actual stuck-check pass to count invocations.
    pass_count = {"n": 0}

    def fake_pass(*, probes, project, home, fleet_bin, cfg, now_unix, log_stream):
        pass_count["n"] += 1
        return supervisor._StuckPassResult()

    monkeypatch.setattr(supervisor, "_run_stuck_check_pass", fake_pass)

    res = _drive_loop(
        # First refresh runs OUTSIDE the loop to baseline mtimes; each
        # iteration then consumes one more entry. 7 [probe] items lets
        # iter 6 still see active workers, with iter 7 returning [] to
        # trigger the all-terminal exit.
        probes_seq=[[probe]] * 7 + [[]],
        cfg=_cfg(stuck_check_every=3),  # every 3 polls
        fleet_home_=fleet_home,
    )
    # Stuck-check runs at poll_count 3 and 6 → 2 passes.
    assert pass_count["n"] == 2
    assert res.stuck_check_passes == 2


def test_supervisor_loop_releases_lock_on_exit(tmp_path: Path) -> None:
    """The supervisor itself doesn't hold the lock — loop._tick_locked
    does. Sanity-check that running the supervisor doesn't crash + exits
    cleanly even when lock-related state is empty.
    """
    res = _drive_loop(
        probes_seq=[],
        cfg=_cfg(poll_interval_s=0),
        fleet_home_=tmp_path,
    )
    assert res.exit_reason == "supervisor-disabled"


def test_supervisor_emits_summary_lines_to_stdout(fleet_home: Path) -> None:
    """The supervisor emits structured lines for operator visibility."""
    a_path = (
        fleet_home / "projects" / "fleet" / "workers" / "alpha-aaaa" / "state.json"
    )
    _write_state_json(a_path, phase="tdd-red", updated_at="2026-01-01T00:00:00Z")
    probe = supervisor.WorkerProbe(
        slug="alpha-aaaa", state_path=a_path,
        agent_id="aaaaaaaa", tmux_session="fleet-aaaaaaaa",
    )
    log = io.StringIO()
    _drive_loop(
        probes_seq=[[probe], []],
        cfg=_cfg(stuck_check_every=0),
        fleet_home_=fleet_home,
        log=log,
    )
    output = log.getvalue()
    assert "[coord]" in output
    assert "supervisor loop starting" in output
    assert "supervisor loop exiting" in output


# ---------- agent_id mapping ----------


def test_remember_and_forget_agent_id() -> None:
    state: dict = {}
    supervisor.remember_agent_id(state, "alpha-aaaa", "aaaaaaaa")
    assert state["worker_agent_ids"] == {"alpha-aaaa": "aaaaaaaa"}
    supervisor.forget_agent_id(state, "alpha-aaaa")
    assert state["worker_agent_ids"] == {}


def test_remember_agent_id_rejects_invalid() -> None:
    state: dict = {}
    supervisor.remember_agent_id(state, "alpha-aaaa", "not-hex!!")
    supervisor.remember_agent_id(state, "", "aaaaaaaa")
    assert "worker_agent_ids" not in state or state["worker_agent_ids"] == {}


# ---------- config from env ----------


def test_supervisor_config_from_env(monkeypatch) -> None:
    monkeypatch.setenv("FLEET_COORD_POLL_INTERVAL_S", "5")
    monkeypatch.setenv("FLEET_COORD_STUCK_CHECK_EVERY", "2")
    monkeypatch.setenv("FLEET_COORD_STUCK_THRESHOLD_S", "10")
    monkeypatch.setenv("FLEET_COORD_STUCK_POLLS", "1")
    monkeypatch.setenv("FLEET_COORD_NUDGE_COOLDOWN_S", "7")
    monkeypatch.setenv("FLEET_COORD_POLL_MAX_S", "60")
    cfg = supervisor.SupervisorConfig.from_env()
    assert cfg.poll_interval_s == 5
    assert cfg.stuck_check_every == 2
    assert cfg.stuck_threshold_s == 10
    assert cfg.stuck_polls == 1
    assert cfg.nudge_cooldown_s == 7
    assert cfg.poll_max_s == 60


# ---------- codex iter-2 P1/P2 regressions ----------


def test_phase_change_resets_recovery_ladder(
    fleet_home: Path, monkeypatch,
) -> None:
    """codex iter-2 [P1]: a worker that recovered (phase advanced)
    must reset the FULL recovery ladder, not just the counter. Otherwise
    a later stall jumps straight to escalate/block."""
    coord_state_path = fleet_home / "projects" / "fleet" / "coord-state.json"
    coord_state_path.parent.mkdir(parents=True, exist_ok=True)
    coord_state_path.write_text(json.dumps({
        "supervisor": {
            "alpha-aaaa": {
                "last_phase": "tdd-red",
                "consecutive_stuck_polls": 3,
                "nudged_at": _now() - 1000,         # already nudged
                "escalated_at": _now() - 500,       # already escalated
            }
        },
        "worker_agent_ids": {"alpha-aaaa": "aaaaaaaa"},
    }), encoding="utf-8")
    state_path = (
        fleet_home / "projects" / "fleet" / "workers" / "alpha-aaaa" / "state.json"
    )
    # Worker advanced to phase tdd-green (different from last_phase).
    _write_state_json(state_path, phase="tdd-green", updated_at="1970-01-01T00:00:00Z")

    probes = [supervisor.WorkerProbe(
        slug="alpha-aaaa", state_path=state_path,
        agent_id="aaaaaaaa", tmux_session="fleet-aaaaaaaa",
        live_worker=True,
    )]
    monkeypatch.setattr(supervisor, "tmux_session_alive", lambda s: True)
    monkeypatch.setattr(
        supervisor.subprocess, "run",
        lambda cmd, *a, **kw: subprocess.CompletedProcess(args=cmd, returncode=0),
    )

    supervisor._run_stuck_check_pass(
        probes=probes, project="fleet",
        home=fleet_home, fleet_bin="fleet",
        cfg=_cfg(stuck_polls=3, stuck_threshold_s=10),
        now_unix=_now(),
        log_stream=io.StringIO(),
    )
    after = json.loads(coord_state_path.read_text())
    sup = after["supervisor"]["alpha-aaaa"]
    # Phase change → counter, nudged_at, escalated_at all reset.
    assert sup["consecutive_stuck_polls"] in (0, 1)  # may have re-incremented
    assert sup["nudged_at"] == 0.0
    assert sup["escalated_at"] == 0.0
    assert sup["last_phase"] == "tdd-green"


def test_fresh_heartbeat_resets_recovery_ladder(
    fleet_home: Path, monkeypatch,
) -> None:
    """codex iter-2 [P1]: heartbeat caught up (within threshold) →
    treat the same as phase change for ladder-reset purposes."""
    coord_state_path = fleet_home / "projects" / "fleet" / "coord-state.json"
    coord_state_path.parent.mkdir(parents=True, exist_ok=True)
    coord_state_path.write_text(json.dumps({
        "supervisor": {
            "alpha-aaaa": {
                "last_phase": "tdd-red",
                "consecutive_stuck_polls": 5,
                "nudged_at": _now() - 1000,
                "escalated_at": _now() - 500,
            }
        },
        "worker_agent_ids": {"alpha-aaaa": "aaaaaaaa"},
    }), encoding="utf-8")
    state_path = (
        fleet_home / "projects" / "fleet" / "workers" / "alpha-aaaa" / "state.json"
    )
    # Heartbeat fresh (within threshold) — same phase, but progressing.
    from datetime import datetime, timezone
    fresh = datetime.fromtimestamp(_now(), tz=timezone.utc).strftime(
        "%Y-%m-%dT%H:%M:%SZ"
    )
    _write_state_json(state_path, phase="tdd-red", updated_at=fresh)

    probes = [supervisor.WorkerProbe(
        slug="alpha-aaaa", state_path=state_path,
        agent_id="aaaaaaaa", tmux_session="fleet-aaaaaaaa",
        live_worker=True,
    )]
    monkeypatch.setattr(supervisor, "tmux_session_alive", lambda s: True)

    supervisor._run_stuck_check_pass(
        probes=probes, project="fleet",
        home=fleet_home, fleet_bin="fleet",
        cfg=_cfg(stuck_polls=3, stuck_threshold_s=180),
        now_unix=_now(),
        log_stream=io.StringIO(),
    )
    after = json.loads(coord_state_path.read_text())
    sup = after["supervisor"]["alpha-aaaa"]
    assert sup["consecutive_stuck_polls"] == 0
    assert sup["nudged_at"] == 0.0
    assert sup["escalated_at"] == 0.0


def test_periodic_reconcile_fires_when_stuck_check_disabled(
    fleet_home: Path,
) -> None:
    """codex iter-2 [P2]: setting FLEET_COORD_STUCK_CHECK_EVERY=0 must
    NOT also disable periodic_full_reconcile. PR/CI sweeps for in-review
    tasks need to keep running even with the recovery ladder off."""
    a_path = (
        fleet_home / "projects" / "fleet" / "workers" / "alpha-aaaa" / "state.json"
    )
    _write_state_json(a_path, phase="tdd-red", updated_at="2026-01-01T00:00:00Z")
    probe = supervisor.WorkerProbe(
        slug="alpha-aaaa", state_path=a_path,
        agent_id="aaaaaaaa", tmux_session="fleet-aaaaaaaa",
        live_worker=True,
    )
    full_reconcile_calls = {"n": 0}

    def fake_full_reconcile():
        full_reconcile_calls["n"] += 1

    # 11 active polls + exit. Fallback cadence is 10 → exactly 1 call.
    seq_iter = iter([[probe]] * 11 + [[]])
    fake_now_state = {"t": 0.0}

    def fake_refresh():
        try:
            return next(seq_iter)
        except StopIteration:
            return []

    def fake_sleep(s):
        fake_now_state["t"] += s

    res = supervisor.run_supervisor(
        # stuck_check_every=0 disables the recovery ladder, but the
        # periodic_full_reconcile fallback cadence (10 polls) still
        # fires.
        cfg=_cfg(stuck_check_every=0),
        project="fleet", home=fleet_home, fleet_bin="fleet",
        sleep_fn=fake_sleep, now_fn=lambda: fake_now_state["t"],
        refresh_probes=fake_refresh,
        reconcile_one=lambda p: None,
        write_state=lambda: None,
        periodic_full_reconcile=fake_full_reconcile,
        log_stream=io.StringIO(),
    )
    assert res.stuck_check_passes == 0  # ladder disabled
    assert full_reconcile_calls["n"] >= 1  # PR/CI sweep still ran


# ---------- codex iter-1 P1 regressions ----------


def test_in_review_probe_skips_stuck_detection(
    fleet_home: Path, monkeypatch,
) -> None:
    """codex iter-1 [P1]: in-review tasks have no live worker. Stuck-
    check must NOT nudge them (the worker subprocess is gone — there's
    no agent to deliver the inbox to).
    """
    coord_state_path = fleet_home / "projects" / "fleet" / "coord-state.json"
    coord_state_path.parent.mkdir(parents=True, exist_ok=True)
    coord_state_path.write_text(json.dumps({
        "supervisor": {
            "alpha-aaaa": {
                "last_phase": "tdd-red",
                "consecutive_stuck_polls": 5,  # would trigger if live
                "nudged_at": 0.0, "escalated_at": 0.0,
            }
        },
        "worker_agent_ids": {"alpha-aaaa": "aaaaaaaa"},
    }), encoding="utf-8")
    state_path = (
        fleet_home / "projects" / "fleet" / "workers" / "alpha-aaaa" / "state.json"
    )
    _write_state_json(state_path, phase="tdd-red", updated_at="1970-01-01T00:00:00Z")

    # Note live_worker=False — this is the in-review probe shape.
    probes = [supervisor.WorkerProbe(
        slug="alpha-aaaa", state_path=state_path,
        agent_id="aaaaaaaa", tmux_session="fleet-aaaaaaaa",
        live_worker=False,
    )]
    monkeypatch.setattr(supervisor, "tmux_session_alive", lambda s: True)

    calls: list[list[str]] = []
    monkeypatch.setattr(
        supervisor.subprocess, "run",
        lambda cmd, *a, **kw: (
            calls.append(list(cmd))
            or subprocess.CompletedProcess(args=cmd, returncode=0)
        ),
    )

    res = supervisor._run_stuck_check_pass(
        probes=probes, project="fleet",
        home=fleet_home, fleet_bin="fleet",
        cfg=_cfg(stuck_polls=3, stuck_threshold_s=10),
        now_unix=_now(),
        log_stream=io.StringIO(),
    )
    assert res.nudges == 0
    assert res.escalations == 0
    assert res.blocks == 0
    # No fleet CLI calls fired for the in-review task.
    assert not any("alpha-aaaa" in str(c) for c in calls), calls


def test_build_worker_probes_marks_in_review_as_non_live(
    tmp_path: Path,
) -> None:
    """build_worker_probes flags in-review tasks live_worker=False."""
    @dataclass
    class FakeTask:
        slug: str
        status: str

    probes = supervisor.build_worker_probes(
        project="fleet", home=tmp_path,
        tasks=[
            FakeTask(slug="alpha-aaaa", status="in-progress"),
            FakeTask(slug="beta-bbbb", status="in-review"),
        ],
        agent_id_map={"alpha-aaaa": "aaaaaaaa", "beta-bbbb": "bbbbbbbb"},
    )
    by_slug = {p.slug: p for p in probes}
    assert by_slug["alpha-aaaa"].live_worker is True
    assert by_slug["beta-bbbb"].live_worker is False


def test_periodic_full_reconcile_runs_on_stuck_check_pass(
    fleet_home: Path,
) -> None:
    """codex iter-1 [P1]: in-review CI checks must run on the periodic
    full-reconcile sweep, not be gated on state.json mtime change."""
    a_path = (
        fleet_home / "projects" / "fleet" / "workers" / "alpha-aaaa" / "state.json"
    )
    _write_state_json(a_path, phase="tdd-red", updated_at="2026-01-01T00:00:00Z")
    probe = supervisor.WorkerProbe(
        slug="alpha-aaaa", state_path=a_path,
        agent_id="aaaaaaaa", tmux_session="fleet-aaaaaaaa",
        live_worker=True,
    )
    full_reconcile_calls = {"n": 0}

    def fake_full_reconcile():
        full_reconcile_calls["n"] += 1

    seq_iter = iter([[probe]] * 5 + [[]])  # 5 polls, then exit on iter 6
    fake_now_state = {"t": 0.0}

    def fake_refresh():
        try:
            return next(seq_iter)
        except StopIteration:
            return []

    def fake_sleep(s):
        fake_now_state["t"] += s

    res = supervisor.run_supervisor(
        cfg=_cfg(stuck_check_every=2),  # every 2 polls
        project="fleet", home=fleet_home, fleet_bin="fleet",
        sleep_fn=fake_sleep, now_fn=lambda: fake_now_state["t"],
        refresh_probes=fake_refresh,
        reconcile_one=lambda p: None,
        write_state=lambda: None,
        periodic_full_reconcile=fake_full_reconcile,
        log_stream=io.StringIO(),
    )
    # 5 active polls, every 2 → poll 2, 4 = 2 full-reconcile calls.
    assert full_reconcile_calls["n"] == 2


def test_supervisor_config_negative_values_clamped(monkeypatch) -> None:
    """Negative env values fall back to defaults (don't disable
    accidentally — disabling is the explicit 0 case)."""
    monkeypatch.setenv("FLEET_COORD_POLL_INTERVAL_S", "-5")
    cfg = supervisor.SupervisorConfig.from_env()
    assert cfg.poll_interval_s == 30  # default


# ---------- loop.py integration: dispatch records agent_id ----------


def test_loop_dispatch_persists_agent_id_mapping(
    tmp_path: Path, monkeypatch,
) -> None:
    """A successful dispatch must persist slug → agent_id into
    coord-state.json so the supervisor's nudge path can locate the
    worker's inbox without re-parsing tasks.md notes.
    """
    # Local import to avoid cyclic import quirks at test collection.
    import datetime as _dt
    import dispatch as dispatch_mod  # noqa: F401  (mocked below)
    import loop
    import parse

    home = tmp_path / "fleet"
    (home / "inbox").mkdir(parents=True)
    (home / "inbox" / "archive").mkdir()
    project_dir = home / "projects" / "fleet"
    project_dir.mkdir(parents=True)
    (project_dir / ".locks").mkdir()

    task = parse.Task(
        slug="ready-aaaa", status="ready", priority="P1",
        worker_pid=0, pr_url="",
        created=_dt.datetime(2026, 5, 8, tzinfo=_dt.timezone.utc),
        updated=_dt.datetime(2026, 5, 8, tzinfo=_dt.timezone.utc),
        spawned_by="user",
        spec="spec", acceptance="acc", notes="",
    )
    parse.write(str(project_dir / "tasks.md"), parse.File(
        schema=parse.SCHEMA_VERSION, tasks=[task],
    ))

    # Stub fleet CLI calls + dispatch subprocess so we run end-to-end
    # without forking the real `fleet` binary.
    monkeypatch.setattr(loop, "_run_fleet", lambda *a, **kw: None)

    def fake_dispatch_run(cmd, capture_output=True, text=True, timeout=None, check=False):
        if cmd[1:3] == ["standards", "show"]:
            return subprocess.CompletedProcess(
                args=cmd, returncode=0, stdout="# Standards\n", stderr="",
            )
        if cmd[1:3] == ["learnings", "list"]:
            return subprocess.CompletedProcess(args=cmd, returncode=0, stdout="", stderr="")
        if cmd[1] == "dispatch":
            return subprocess.CompletedProcess(
                args=cmd, returncode=0,
                stdout="agent abcdef01 dispatched\n", stderr="",
            )
        return subprocess.CompletedProcess(args=cmd, returncode=0, stdout="", stderr="")

    import dispatch as dispatch_mod  # noqa: F811
    monkeypatch.setattr(dispatch_mod.subprocess, "run", fake_dispatch_run)

    res = loop.tick(
        "fleet", coord_id="cccccc01", cwd=str(tmp_path),
        fleet_home=str(home),
    )
    assert res.dispatched == 1

    # coord-state.json carries the slug → agent_id mapping.
    coord_state = json.loads(
        (project_dir / "coord-state.json").read_text(encoding="utf-8")
    )
    assert coord_state.get("worker_agent_ids", {}) == {"ready-aaaa": "abcdef01"}
