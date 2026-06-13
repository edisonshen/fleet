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
        # supervisor-loop wall-clock cap. 0 here disables the extra cap
        # so legacy tests fall back to poll_max_s governance; tests that
        # exercise the cap override explicitly.
        supervisor_max_s=0,
        # Legacy single-rate driver. Tests that exercise the invariant-4
        # adaptive cadence override these explicitly.
        poll_base_interval_s=0, poll_backoff_interval_s=0,
        poll_stability_window_s=0,
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

    def fake_pass(*, probes, project, home, fleet_bin, cfg, now_unix, log_stream, coord_id="", stuck_alert_mtimes=None):
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


# ---------- sigpipe / wedge guards (loop-supervisor-sigpipe-5263) ----------


class _BrokenStream:
    """A log stream whose write() raises BrokenPipeError, simulating a
    closed stdout (e.g. `python3 loop.py X | head -40` after head exits).
    flush() is a no-op so the failure surfaces only on write."""

    def __init__(self) -> None:
        self.writes = 0

    def write(self, _s: str) -> int:
        self.writes += 1
        raise BrokenPipeError(32, "Broken pipe")

    def flush(self) -> None:  # pragma: no cover - never reached
        pass


def test_supervisor_exits_when_stdout_broken(fleet_home: Path) -> None:
    """Defect #1: when the log/dispatch stream is a broken pipe, the
    supervisor must NOT swallow-and-keep-polling (the wedge). It exits
    promptly with a distinct reason so the caller can release the lock
    and let the next tick retry, instead of spinning until poll_max_s.

    Regression: on the parent commit emit() swallows the BrokenPipeError
    and the loop polls until max-duration (60s budget / 30s polls => 2
    iterations here). The fix breaks on the FIRST broken write, so the
    loop never reaches the second poll.
    """
    a_path = (
        fleet_home / "projects" / "fleet" / "workers" / "alpha-aaaa" / "state.json"
    )
    _write_state_json(a_path, phase="tdd-red", updated_at="2026-01-01T00:00:00Z")
    probe = supervisor.WorkerProbe(
        slug="alpha-aaaa", state_path=a_path,
        agent_id="aaaaaaaa", tmux_session="fleet-aaaaaaaa",
    )
    broken = _BrokenStream()
    res = _drive_loop(
        probes_seq=[[probe]] * 100,  # never goes terminal
        cfg=_cfg(poll_interval_s=30, poll_max_s=60, stuck_check_every=0),
        fleet_home_=fleet_home,
        log=broken,
    )
    assert res.exit_reason == "stdout-broken"
    # The first emit (loop-start banner) is the broken write; the loop
    # must bail before completing a single poll iteration.
    assert res.iterations == 0


def test_supervisor_no_active_workers_stdout_broken(fleet_home: Path) -> None:
    """Codex iter-6 [P3]: the no-active-workers early-exit emit() sits
    BEFORE the main broken-pipe try-block. emit() now re-raises
    BrokenPipeError, so an unguarded emit there would propagate out of
    run_supervisor when stdout is already closed and there are no active
    workers. The guard must catch it and return exit_reason=stdout-broken
    instead of raising."""
    broken = _BrokenStream()
    res = _drive_loop(
        probes_seq=[[]],  # no active workers on the initial probe
        cfg=_cfg(poll_interval_s=30, poll_max_s=60, stuck_check_every=0),
        fleet_home_=fleet_home,
        log=broken,
    )
    # Must return cleanly, not raise.
    assert res.exit_reason == "stdout-broken"
    assert res.iterations == 0


class _StreamBrokenAfterN:
    """A log stream that buffers the first N writes, then raises
    BrokenPipeError. Lets the loop reach a specific emit() deeper in the
    loop body (e.g. the operator-inbox-exit emit) before the pipe breaks.
    """

    def __init__(self, ok_writes: int) -> None:
        self.ok_writes = ok_writes
        self.writes = 0

    def write(self, _s: str) -> int:
        self.writes += 1
        if self.writes > self.ok_writes:
            raise BrokenPipeError(32, "Broken pipe")
        return len(_s)

    def flush(self) -> None:
        pass


def test_supervisor_inbox_emit_broken_pipe_propagates(fleet_home: Path) -> None:
    """loop-supervisor-sigpipe-5263 regression: the operator-inbox-exit
    emit() lives inside a `try/except OSError` that guards direct.stat().
    BrokenPipeError is an OSError subclass, so a bare `except OSError`
    there would SWALLOW the broken pipe and keep the loop spinning to the
    cap — defeating the fast broken-stdout exit on this path. The fix
    re-raises BrokenPipeError so the outer handler sets
    exit_reason="stdout-broken" immediately.

    Setup: a forced wake (force_tick_check → True) with a coord inbox file
    whose mtime is past the baseline, so the loop enters the inbox-exit
    branch and calls emit(). The stream allows the loop-start banner write
    (write #1) then breaks on the inbox emit (write #2). On the parent fix
    (before this regression fix) exit_reason would be "operator-inbox-
    message" and the loop would NOT bail on the broken pipe.
    """
    a_path = (
        fleet_home / "projects" / "fleet" / "workers" / "alpha-aaaa" / "state.json"
    )
    _write_state_json(a_path, phase="tdd-red", updated_at="2026-01-01T00:00:00Z")
    probe = supervisor.WorkerProbe(
        slug="alpha-aaaa", state_path=a_path,
        agent_id="aaaaaaaa", tmux_session="fleet-aaaaaaaa",
    )
    coord_id = "cccccc01"
    inbox = fleet_home / "inbox" / f"{coord_id}.md"
    inbox.parent.mkdir(parents=True, exist_ok=True)
    inbox.write_text("operator: please check on this\n")
    # mtime well past the baseline so the inbox-exit branch is taken.
    import os as _os
    _os.utime(inbox, (1_000_000.0, 1_000_000.0))

    broken = _StreamBrokenAfterN(ok_writes=1)  # banner OK, inbox emit breaks
    seq = iter([[probe]] * 100)
    fake_now = {"t": 0.0}

    res = supervisor.run_supervisor(
        cfg=_cfg(poll_interval_s=30, poll_max_s=3600,
                 supervisor_max_s=0, stuck_check_every=0),
        project="fleet", home=fleet_home, fleet_bin="fleet",
        sleep_fn=lambda s: fake_now.__setitem__("t", fake_now["t"] + s),
        now_fn=lambda: fake_now["t"],
        refresh_probes=lambda: next(seq, []),
        reconcile_one=lambda p: None,
        write_state=lambda: None,
        force_tick_check=lambda: True,
        coord_id=coord_id,
        direct_inbox_session_baseline=0.0,
        log_stream=broken,
    )
    assert res.exit_reason == "stdout-broken", (
        f"inbox-emit broken pipe must propagate to stdout-broken, "
        f"got {res.exit_reason!r}"
    )
    # The inbox emit is the SECOND write (banner = #1). With the fix it
    # propagates immediately, so the stream sees exactly 2 writes and the
    # loop bails on its first iteration. Without the fix the bare
    # `except OSError: pass` swallows the broken inbox emit and the loop
    # keeps spinning — more writes, more iterations — before some later
    # unguarded emit finally propagates. Pin both so the test fails on the
    # parent (swallow) behaviour, not just on the final exit_reason.
    assert broken.writes == 2, (
        f"loop must bail on the broken inbox emit (write #2), not swallow "
        f"and keep writing; saw {broken.writes} writes"
    )
    assert res.iterations == 1


def test_supervisor_respects_supervisor_max_s_cap(fleet_home: Path) -> None:
    """Defect #2: FLEET_COORD_SUPERVISOR_MAX_S caps the loop tighter than
    poll_max_s so a wedged loop can never hold the lock for the full 4h
    poll_max budget. With supervisor_max_s=60 < poll_max_s=14400, the
    loop exits at the supervisor cap.
    """
    a_path = (
        fleet_home / "projects" / "fleet" / "workers" / "alpha-aaaa" / "state.json"
    )
    _write_state_json(a_path, phase="tdd-red", updated_at="2026-01-01T00:00:00Z")
    probe = supervisor.WorkerProbe(
        slug="alpha-aaaa", state_path=a_path,
        agent_id="aaaaaaaa", tmux_session="fleet-aaaaaaaa",
    )
    res = _drive_loop(
        probes_seq=[[probe]] * 100,  # never goes terminal
        cfg=_cfg(
            poll_interval_s=30, poll_max_s=14400,
            supervisor_max_s=60, stuck_check_every=0,
        ),
        fleet_home_=fleet_home,
    )
    assert res.exit_reason == "supervisor-max-duration"
    # 60s cap, 30s polls => at most 2 iters before exit.
    assert res.iterations <= 2


def test_supervisor_max_s_zero_disables_cap(fleet_home: Path) -> None:
    """supervisor_max_s=0 falls back to poll_max_s only (no extra cap)."""
    a_path = (
        fleet_home / "projects" / "fleet" / "workers" / "alpha-aaaa" / "state.json"
    )
    _write_state_json(a_path, phase="tdd-red", updated_at="2026-01-01T00:00:00Z")
    probe = supervisor.WorkerProbe(
        slug="alpha-aaaa", state_path=a_path,
        agent_id="aaaaaaaa", tmux_session="fleet-aaaaaaaa",
    )
    res = _drive_loop(
        probes_seq=[[probe]] * 100,
        cfg=_cfg(
            poll_interval_s=30, poll_max_s=60,
            supervisor_max_s=0, stuck_check_every=0,
        ),
        fleet_home_=fleet_home,
    )
    # With the supervisor cap disabled, poll_max_s governs.
    assert res.exit_reason == "max-duration"


def test_supervisor_sleep_clamped_to_remaining_cap(fleet_home: Path) -> None:
    """loop-supervisor-sigpipe-5263 (codex iter-1 [P2]): a long poll
    interval must not let the loop sleep PAST the cap while holding
    coordinator.lock. With poll_interval_s=3600 (legacy single-rate, so
    compute_next_sleep_s returns 3600) and supervisor_max_s=1800, an
    unclamped loop would sleep the full 3600s and hold the lock for an
    hour — double the bound. The clamp caps each sleep at the remaining
    budget, so no recorded sleep exceeds the cap and the loop exits at it.
    """
    a_path = (
        fleet_home / "projects" / "fleet" / "workers" / "alpha-aaaa" / "state.json"
    )
    _write_state_json(a_path, phase="tdd-red", updated_at="2026-01-01T00:00:00Z")
    probe = supervisor.WorkerProbe(
        slug="alpha-aaaa", state_path=a_path,
        agent_id="aaaaaaaa", tmux_session="fleet-aaaaaaaa",
    )
    sleeps: list[float] = []
    res = _drive_loop(
        probes_seq=[[probe]] * 100,  # never goes terminal
        cfg=_cfg(
            poll_interval_s=3600,   # >> cap; legacy single-rate cadence
            poll_base_interval_s=0,  # disable adaptive → returns poll_interval_s
            poll_max_s=14400,
            supervisor_max_s=1800,
            stuck_check_every=0,
        ),
        fleet_home_=fleet_home,
        sleep_calls=sleeps,
    )
    assert res.exit_reason == "supervisor-max-duration"
    # No single sleep may exceed the cap (unclamped it would be 3600 > 1800).
    assert sleeps, "loop should have slept at least once"
    assert max(sleeps) <= 1800, (
        f"sleep must be clamped to the remaining cap budget; "
        f"saw max sleep {max(sleeps)}s > cap 1800s"
    )


def test_supervisor_no_poll_body_after_cap_consuming_sleep(fleet_home: Path) -> None:
    """loop-supervisor-sigpipe-5263 (codex iter-3 [P2]): when a clamped
    sleep consumes the entire remaining cap budget, the loop must break
    immediately after the sleep — NOT run a full poll body (refresh_probes
    / reconcile / stuck-check / idle-archive) and only re-check the cap at
    the top of the next iteration. Otherwise the lock is held for
    `cap + one poll body`, overrunning supervisor_max_s.

    With poll_interval_s == supervisor_max_s == 1800, iteration 1 clamps
    the sleep to the full 1800s, wakes exactly at the cap, and must bail.
    The cap is enforced via a no-op probe counter: refresh_probes must
    only be called once (the pre-loop priming read), never again inside
    the loop body.
    """
    a_path = (
        fleet_home / "projects" / "fleet" / "workers" / "alpha-aaaa" / "state.json"
    )
    _write_state_json(a_path, phase="tdd-red", updated_at="2026-01-01T00:00:00Z")
    probe = supervisor.WorkerProbe(
        slug="alpha-aaaa", state_path=a_path,
        agent_id="aaaaaaaa", tmux_session="fleet-aaaaaaaa",
    )
    refresh_calls = {"n": 0}
    fake_now = {"t": 0.0}

    def counting_refresh():
        refresh_calls["n"] += 1
        return [probe]

    res = supervisor.run_supervisor(
        cfg=_cfg(
            poll_interval_s=1800, poll_base_interval_s=0,
            poll_max_s=14400, supervisor_max_s=1800, stuck_check_every=0,
        ),
        project="fleet", home=fleet_home, fleet_bin="fleet",
        sleep_fn=lambda s: fake_now.__setitem__("t", fake_now["t"] + s),
        now_fn=lambda: fake_now["t"],
        refresh_probes=counting_refresh,
        reconcile_one=lambda p: None,
        write_state=lambda: None,
        log_stream=io.StringIO(),
    )
    assert res.exit_reason == "supervisor-max-duration"
    # Pre-loop priming read is call #1. The loop body's refresh_probes
    # (inside the iteration) must NEVER run because we break right after
    # the cap-consuming sleep. Without the post-sleep check it would be 2+.
    assert refresh_calls["n"] == 1, (
        f"loop ran a poll body after the cap was hit; refresh_probes "
        f"called {refresh_calls['n']}x (expected 1 = priming read only)"
    )
    assert res.iterations == 0


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


# ---------- subagent_id helpers (issue #94 Phase C) ----------


def test_remember_and_forget_subagent_id_round_trip() -> None:
    """Mapping survives a remember → forget cycle and ends empty."""
    state: dict = {}
    supervisor.remember_subagent_id(state, "alpha-aaaa", "claude-sub-1")
    assert state["worker_subagent_ids"] == {"alpha-aaaa": "claude-sub-1"}
    supervisor.forget_subagent_id(state, "alpha-aaaa")
    assert state["worker_subagent_ids"] == {}


def test_remember_subagent_id_rejects_invalid() -> None:
    """Empty / whitespace / over-long subagent_ids are silent no-ops.
    Loose validation (printable, non-whitespace, ≤128 chars) — Claude
    doesn't pin a strict shape so we only filter the obviously malformed."""
    state: dict = {}
    # Empty + whitespace inputs.
    supervisor.remember_subagent_id(state, "alpha-aaaa", "")
    supervisor.remember_subagent_id(state, "alpha-aaaa", "   ")
    supervisor.remember_subagent_id(state, "alpha-aaaa", "has space")
    # Empty slug.
    supervisor.remember_subagent_id(state, "", "claude-sub-1")
    # Over the 128-char cap.
    supervisor.remember_subagent_id(state, "alpha-aaaa", "x" * 129)
    # Control / non-printable.
    supervisor.remember_subagent_id(state, "alpha-aaaa", "claude\x00sub")
    assert state.get("worker_subagent_ids", {}) == {}


def test_load_subagent_id_map_filters_malformed_entries() -> None:
    """Hand-crafted state with mixed valid + invalid entries — loader
    drops the bad ones, keeps the good ones, never raises."""
    state = {
        "worker_subagent_ids": {
            "good-aaaa": "claude-sub-1",
            "spaces-bbbb": "has space",  # invalid: whitespace inside
            123: "wrong-key-type",       # invalid: non-str key
            "control-cccc": "x\x00y",   # invalid: non-printable
            "long-dddd": "y" * 200,      # invalid: > 128 chars
            "ok-eeee": "claude-sub-2",
        },
    }
    got = supervisor.load_subagent_id_map(state)
    assert got == {"good-aaaa": "claude-sub-1", "ok-eeee": "claude-sub-2"}


def test_load_subagent_id_map_handles_missing_key() -> None:
    """Empty / missing / wrong-type top-level all return {} cleanly."""
    assert supervisor.load_subagent_id_map({}) == {}
    assert supervisor.load_subagent_id_map({"worker_subagent_ids": None}) == {}
    assert supervisor.load_subagent_id_map({"worker_subagent_ids": []}) == {}
    assert supervisor.load_subagent_id_map({"worker_subagent_ids": "x"}) == {}


def test_forget_agent_id_clears_subagent_id_too() -> None:
    """When the upstream forget_agent_id fires (worker reaches terminal),
    the parallel subagent_id mapping must drop too — otherwise stale
    chips would render on archived workers."""
    state: dict = {}
    supervisor.remember_agent_id(state, "alpha-aaaa", "aaaaaaaa")
    supervisor.remember_subagent_id(state, "alpha-aaaa", "claude-sub-1")
    supervisor.forget_agent_id(state, "alpha-aaaa")
    assert state.get("worker_agent_ids", {}) == {}
    assert state.get("worker_subagent_ids", {}) == {}


def test_remember_subagent_id_preserves_other_slugs() -> None:
    """A second remember_subagent_id call must not clobber an existing
    map entry for a different slug."""
    state: dict = {}
    supervisor.remember_subagent_id(state, "alpha-aaaa", "claude-sub-1")
    supervisor.remember_subagent_id(state, "beta-bbbb", "claude-sub-2")
    assert state["worker_subagent_ids"] == {
        "alpha-aaaa": "claude-sub-1",
        "beta-bbbb": "claude-sub-2",
    }
    supervisor.forget_subagent_id(state, "alpha-aaaa")
    assert state["worker_subagent_ids"] == {"beta-bbbb": "claude-sub-2"}


def test_remember_subagent_id_repairs_corrupt_top_level() -> None:
    """If a malformed coord-state has worker_subagent_ids set to a non-dict
    (operator hand-edit, schema drift), remember_subagent_id replaces it
    with a fresh dict rather than crashing on .get()."""
    state = {"worker_subagent_ids": "not-a-dict"}
    supervisor.remember_subagent_id(state, "alpha-aaaa", "claude-sub-1")
    assert state["worker_subagent_ids"] == {"alpha-aaaa": "claude-sub-1"}


def test_is_subagent_id_rejects_shell_metacharacters() -> None:
    """Defense-in-depth: even though host Claude is trusted, an
    Agent-tool response containing shell metacharacters could survive
    into a Bash tool invocation (per SKILL.md step 3) before argparse
    sees it. _is_subagent_id rejects the operator-visible injection
    vectors so a malicious id never reaches the on-disk map.

    Each metachar is tested independently so a regression on one
    character is a single-line failure (fix isolated)."""
    for meta in [";", "|", "&", "$", "`", "(", ")", "<", ">", "\\", "'", '"']:
        sid = f"claude-sub{meta}1"
        assert not supervisor._is_subagent_id(sid), (
            f"shell meta char {meta!r} must be rejected, but {sid!r} accepted"
        )


def test_is_subagent_id_accepts_legitimate_format() -> None:
    """Legitimate subagent_ids — alphanumeric + hyphen / underscore /
    period — must pass. Pin the legitimate path so tightening the
    blocklist doesn't accidentally reject a real Claude token."""
    for sid in (
        "claude-sub-1",
        "claude_sub_xyz",
        "abc123",
        "claude-sub-abcdef0123456789",  # 32-char hex-style
        "x.y.z",  # period is allowed
    ):
        assert supervisor._is_subagent_id(sid), f"{sid!r} should pass"


# ---------- config from env ----------


def test_supervisor_config_from_env(monkeypatch) -> None:
    monkeypatch.setenv("FLEET_COORD_POLL_INTERVAL_S", "5")
    monkeypatch.setenv("FLEET_COORD_STUCK_CHECK_EVERY", "2")
    monkeypatch.setenv("FLEET_COORD_STUCK_THRESHOLD_S", "10")
    monkeypatch.setenv("FLEET_COORD_STUCK_POLLS", "1")
    monkeypatch.setenv("FLEET_COORD_NUDGE_COOLDOWN_S", "7")
    monkeypatch.setenv("FLEET_COORD_POLL_MAX_S", "60")
    monkeypatch.setenv("FLEET_COORD_SUPERVISOR_MAX_S", "45")
    cfg = supervisor.SupervisorConfig.from_env()
    assert cfg.poll_interval_s == 5
    assert cfg.stuck_check_every == 2
    assert cfg.stuck_threshold_s == 10
    assert cfg.stuck_polls == 1
    assert cfg.nudge_cooldown_s == 7
    assert cfg.poll_max_s == 60
    assert cfg.supervisor_max_s == 45


def test_supervisor_max_s_defaults_to_30_min(monkeypatch) -> None:
    """FLEET_COORD_SUPERVISOR_MAX_S unset → 30-min default so a wedged
    supervisor loop can never hold coordinator.lock for the full 4h
    poll_max_s budget."""
    monkeypatch.delenv("FLEET_COORD_SUPERVISOR_MAX_S", raising=False)
    assert supervisor.env_supervisor_max_s() == 1800
    cfg = supervisor.SupervisorConfig.from_env()
    assert cfg.supervisor_max_s == 1800


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
    # without forking the real `fleet` binary. Issue #84 Phase A: the
    # skill no longer shells out to `fleet dispatch` for workers; it
    # mints agent_ids in-process via dispatch.mint_agent_id. We pin
    # the mint to a known token so the assertion is deterministic.
    monkeypatch.setattr(loop, "_run_fleet", lambda *a, **kw: None)

    def fake_dispatch_run(
        cmd, capture_output=True, text=True, timeout=None, check=False,
        input=None, env=None,
    ):
        if cmd[1:3] == ["standards", "show"]:
            return subprocess.CompletedProcess(
                args=cmd, returncode=0, stdout="# Standards\n", stderr="",
            )
        if cmd[1:3] == ["learnings", "list"]:
            return subprocess.CompletedProcess(args=cmd, returncode=0, stdout="", stderr="")
        # PR1 dispatch-lifecycle: emulate `fleet claims acquire-prompt`
        # so the per-tick dispatch actually succeeds (acquire writes the
        # inbox file + returns the JSON envelope). Codex iter-4 [P1]: an
        # acquire failure no longer leaks the failed agent_id into the
        # worker_agent_ids mapping, so the test's terminal assertion
        # now requires a SUCCESSFUL acquire to set the expected entry.
        if cmd[1:3] == ["claims", "acquire-prompt"]:
            agent_id = cmd[3]
            fleet_home = (env or os.environ).get("FLEET_HOME") or os.path.expanduser("~/.fleet")
            inbox_dir = os.path.join(fleet_home, "inbox")
            os.makedirs(inbox_dir, exist_ok=True)
            path = os.path.join(inbox_dir, f"{agent_id}.md")
            with open(path, "w", encoding="utf-8") as fh:
                fh.write((input or "") + ("\n" if (input or "") and not (input or "").endswith("\n") else ""))
            envelope = (
                f'{{"outcome":"acquired","dispatch_id":"{agent_id}",'
                f'"kind":"coord_prompt_inbox","path":"{path}"}}\n'
            )
            return subprocess.CompletedProcess(
                args=cmd, returncode=0, stdout=envelope, stderr="",
            )
        return subprocess.CompletedProcess(args=cmd, returncode=0, stdout="", stderr="")

    import dispatch as dispatch_mod  # noqa: F811
    monkeypatch.setattr(dispatch_mod.subprocess, "run", fake_dispatch_run)
    monkeypatch.setattr(dispatch_mod, "mint_agent_id", lambda: "abcdef01")

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


# ---------- Sub-fix C: agent auto-archive after FLEET_COORD_IDLE_TTL_H ----------


def _write_agent_record(
    fleet_home: Path,
    agent_id: str,
    *,
    project: str,
    last_activity_ts: str = "",
    needs_input: bool = False,
    blocked: bool = False,
    engine: str = "claude-code",
    task_id: str | None = None,
    is_coord: bool | None = None,
) -> Path:
    """Seed ~/.fleet/agents/<id>.json with the minimal shape the sweep
    reads. Mirrors internal/agent.Record's json:"..." tag names verbatim
    so changing the Go schema here without updating the test is caught
    by an assertion failure (defensive against schema drift).

    task_id / is_coord are optional so legacy-record tests can leave the
    field absent entirely (matching omitempty on the Go side): pass None
    (default) to omit, or an explicit value to emit it."""
    agents_dir = fleet_home / "agents"
    agents_dir.mkdir(parents=True, exist_ok=True)
    rec = {
        "schema_version": 1,
        "id": agent_id,
        "project": project,
        "engine": engine,
        "last_activity_ts": last_activity_ts,
        "needs_input": needs_input,
        "blocked": blocked,
    }
    if task_id is not None:
        rec["task_id"] = task_id
    if is_coord is not None:
        rec["is_coord"] = is_coord
    path = agents_dir / f"{agent_id}.json"
    path.write_text(json.dumps(rec), encoding="utf-8")
    return path


def test_env_idle_ttl_h_defaults_to_24() -> None:
    """Default TTL = 24h. Empty env → 24."""
    with patch.dict(os.environ, {}, clear=False):
        os.environ.pop("FLEET_COORD_IDLE_TTL_H", None)
        assert supervisor.env_idle_ttl_h() == 24


def test_env_idle_ttl_h_zero_disables() -> None:
    """Setting the knob to 0 disables the sweep entirely."""
    with patch.dict(os.environ, {"FLEET_COORD_IDLE_TTL_H": "0"}):
        assert supervisor.env_idle_ttl_h() == 0


def test_env_idle_ttl_h_clamps_out_of_range() -> None:
    """Negative and > 720 fall back to the 24h default (defensive
    against a typo wedging archive at "never")."""
    with patch.dict(os.environ, {"FLEET_COORD_IDLE_TTL_H": "-5"}):
        assert supervisor.env_idle_ttl_h() == 24
    with patch.dict(os.environ, {"FLEET_COORD_IDLE_TTL_H": "1000"}):
        assert supervisor.env_idle_ttl_h() == 24
    with patch.dict(os.environ, {"FLEET_COORD_IDLE_TTL_H": "garbage"}):
        assert supervisor.env_idle_ttl_h() == 24


def test_env_idle_ttl_h_accepts_in_range_values() -> None:
    """1..720 are accepted verbatim."""
    with patch.dict(os.environ, {"FLEET_COORD_IDLE_TTL_H": "1"}):
        assert supervisor.env_idle_ttl_h() == 1
    with patch.dict(os.environ, {"FLEET_COORD_IDLE_TTL_H": "720"}):
        assert supervisor.env_idle_ttl_h() == 720


def test_idle_agent_archive_pass_archives_stale_record(
    fleet_home: Path, monkeypatch,
) -> None:
    """Stale-idle agent (no NeedsInput/Blocked flags, LastActivityTS
    older than TTL) gets `fleet rm <id>` invoked. With TTL=1h and a
    70-minute-old heartbeat, the sweep fires.
    """
    project = "fleet"
    _write_agent_record(
        fleet_home, "aaaa0001",
        project=project,
        last_activity_ts="2026-05-11T00:00:00Z",
    )

    calls: list[list[str]] = []

    def fake_run(cmd, capture_output=True, text=True, timeout=None, check=False):
        calls.append(list(cmd))
        return subprocess.CompletedProcess(
            args=cmd, returncode=0, stdout="", stderr="",
        )

    # 70 minutes after the heartbeat (well past TTL=1h = 3600s).
    last_heartbeat = supervisor._parse_rfc3339("2026-05-11T00:00:00Z")
    assert last_heartbeat is not None
    now_unix = last_heartbeat + 70 * 60

    monkeypatch.setenv("FLEET_COORD_IDLE_TTL_H", "1")
    monkeypatch.setattr(subprocess, "run", fake_run)

    archived = supervisor._run_idle_agent_archive_pass(
        project=project, home=fleet_home, fleet_bin="fleet",
        now_unix=now_unix, log_stream=io.StringIO(),
    )

    assert archived == 1, f"expected 1 archive, recorded calls: {calls}"
    rm_calls = [c for c in calls if c[1:3] == ["rm", "aaaa0001"]]
    assert rm_calls, f"expected `fleet rm aaaa0001`, got: {calls}"


# ---------- coord-idle-exempt: coords are NEVER idle-archived ----------
# DESIGN-coord-idle-exempt §4 — exempt on two structural signals,
# OR-combined: is_coord==true (explicit) OR task_id=="coord-<project>"
# (intrinsic, backs up a missing field on legacy records). Workers stay
# fully reapable on the same TTL.


def _exempt_setup(monkeypatch):
    """Shared rig: TTL=1h, a stale heartbeat 70 min in the past, and a
    fake `fleet rm` that records every shell so the test can assert the
    coord was NOT rm'd. Returns (now_unix, calls)."""
    calls: list[list[str]] = []

    def fake_run(cmd, capture_output=True, text=True, timeout=None, check=False):
        calls.append(list(cmd))
        return subprocess.CompletedProcess(
            args=cmd, returncode=0, stdout="", stderr="",
        )

    last_heartbeat = supervisor._parse_rfc3339("2026-05-11T00:00:00Z")
    assert last_heartbeat is not None
    now_unix = last_heartbeat + 70 * 60  # 70 min idle, well past TTL=1h

    monkeypatch.setenv("FLEET_COORD_IDLE_TTL_H", "1")
    monkeypatch.setattr(subprocess, "run", fake_run)
    return now_unix, calls


def test_idle_agent_archive_pass_exempts_coord_explicit_field(
    fleet_home: Path, monkeypatch,
) -> None:
    """Test 1 — coord NOT archived (explicit signal). A record with
    is_coord=true, idle 70 min past TTL, must NOT be `fleet rm`'d; the
    record file stays on disk."""
    project = "fleet"
    path = _write_agent_record(
        fleet_home, "cccc0001",
        project=project,
        last_activity_ts="2026-05-11T00:00:00Z",
        task_id=f"coord-{project}",
        is_coord=True,
    )
    now_unix, calls = _exempt_setup(monkeypatch)

    archived = supervisor._run_idle_agent_archive_pass(
        project=project, home=fleet_home, fleet_bin="fleet",
        now_unix=now_unix, log_stream=io.StringIO(),
    )

    assert archived == 0, f"coord must not be archived; calls: {calls}"
    assert not [c for c in calls if "rm" in c], f"no rm for coord: {calls}"
    assert path.exists(), "coord record file must remain on disk"


def test_idle_agent_archive_pass_archives_worker_with_task_id(
    fleet_home: Path, monkeypatch,
) -> None:
    """Test 2 — worker IS archived (unchanged). A record with a real
    task slug and no is_coord field, idle past TTL, still reaps."""
    project = "fleet"
    _write_agent_record(
        fleet_home, "dddd0002",
        project=project,
        last_activity_ts="2026-05-11T00:00:00Z",
        task_id="some-feature-slug-1234",
    )
    now_unix, calls = _exempt_setup(monkeypatch)

    archived = supervisor._run_idle_agent_archive_pass(
        project=project, home=fleet_home, fleet_bin="fleet",
        now_unix=now_unix, log_stream=io.StringIO(),
    )

    assert archived == 1, f"worker must archive; calls: {calls}"
    assert [c for c in calls if c[1:3] == ["rm", "dddd0002"]], (
        f"expected `fleet rm dddd0002`, got: {calls}"
    )


def test_idle_agent_archive_pass_exempts_coord_legacy_fallback(
    fleet_home: Path, monkeypatch,
) -> None:
    """Test 3 — coord NOT archived via fallback (legacy). A record with
    task_id=="coord-<project>" but NO is_coord field (predates the stamp)
    is still exempt — Signal 1 (the task_id convention) backs up the
    missing explicit field."""
    project = "fleet"
    path = _write_agent_record(
        fleet_home, "cccc0003",
        project=project,
        last_activity_ts="2026-05-11T00:00:00Z",
        task_id=f"coord-{project}",
        # is_coord intentionally absent (legacy record).
    )
    now_unix, calls = _exempt_setup(monkeypatch)

    archived = supervisor._run_idle_agent_archive_pass(
        project=project, home=fleet_home, fleet_bin="fleet",
        now_unix=now_unix, log_stream=io.StringIO(),
    )

    assert archived == 0, f"legacy coord must not archive; calls: {calls}"
    assert not [c for c in calls if "rm" in c], f"no rm for coord: {calls}"
    assert path.exists(), "legacy coord record file must remain on disk"


def test_idle_agent_archive_pass_coord_exempt_respects_zero_ttl(
    fleet_home: Path, monkeypatch,
) -> None:
    """Regression guard: TTL=0 still disables the whole sweep (the
    exemption clause must not accidentally short-circuit past the
    disable check). Coord present, but the sweep does nothing."""
    project = "fleet"
    _write_agent_record(
        fleet_home, "cccc0004",
        project=project,
        last_activity_ts="2020-01-01T00:00:00Z",
        task_id=f"coord-{project}",
        is_coord=True,
    )
    calls: list[list[str]] = []

    def fake_run(cmd, capture_output=True, text=True, timeout=None, check=False):
        calls.append(list(cmd))
        return subprocess.CompletedProcess(args=cmd, returncode=0, stdout="", stderr="")

    monkeypatch.setenv("FLEET_COORD_IDLE_TTL_H", "0")
    monkeypatch.setattr(subprocess, "run", fake_run)

    archived = supervisor._run_idle_agent_archive_pass(
        project=project, home=fleet_home, fleet_bin="fleet",
        now_unix=1_000_000_000.0, log_stream=io.StringIO(),
    )

    assert archived == 0
    assert calls == [], f"TTL=0 disables the whole sweep; got: {calls}"


def test_idle_agent_archive_pass_disabled_by_zero_ttl(
    fleet_home: Path, monkeypatch,
) -> None:
    """TTL=0 short-circuits the sweep — no scan, no shell invocations."""
    _write_agent_record(
        fleet_home, "aaaa0002",
        project="fleet",
        last_activity_ts="2020-01-01T00:00:00Z",  # ancient
    )
    calls: list[list[str]] = []

    def fake_run(cmd, capture_output=True, text=True, timeout=None, check=False):
        calls.append(list(cmd))
        return subprocess.CompletedProcess(args=cmd, returncode=0, stdout="", stderr="")

    monkeypatch.setenv("FLEET_COORD_IDLE_TTL_H", "0")
    monkeypatch.setattr(subprocess, "run", fake_run)

    archived = supervisor._run_idle_agent_archive_pass(
        project="fleet", home=fleet_home, fleet_bin="fleet",
        now_unix=2000000000.0, log_stream=io.StringIO(),
    )
    assert archived == 0
    assert calls == [], "TTL=0 must skip every shell invocation"


def test_idle_agent_archive_pass_skips_asking(
    fleet_home: Path, monkeypatch,
) -> None:
    """NeedsInput=true (asking) records NEVER auto-archive regardless
    of staleness — they need operator attention."""
    _write_agent_record(
        fleet_home, "bbbb0001",
        project="fleet",
        last_activity_ts="2020-01-01T00:00:00Z",  # ancient
        needs_input=True,
    )
    calls: list[list[str]] = []

    def fake_run(cmd, capture_output=True, text=True, timeout=None, check=False):
        calls.append(list(cmd))
        return subprocess.CompletedProcess(args=cmd, returncode=0, stdout="", stderr="")

    monkeypatch.setenv("FLEET_COORD_IDLE_TTL_H", "1")
    monkeypatch.setattr(subprocess, "run", fake_run)

    archived = supervisor._run_idle_agent_archive_pass(
        project="fleet", home=fleet_home, fleet_bin="fleet",
        now_unix=2000000000.0, log_stream=io.StringIO(),
    )
    assert archived == 0
    rm_calls = [c for c in calls if "rm" in c]
    assert rm_calls == [], (
        "asking records must never be auto-archived; got: " + repr(calls)
    )


def test_idle_agent_archive_pass_skips_blocked(
    fleet_home: Path, monkeypatch,
) -> None:
    """Blocked=true records NEVER auto-archive — operator triage path."""
    _write_agent_record(
        fleet_home, "cccc0001",
        project="fleet",
        last_activity_ts="2020-01-01T00:00:00Z",
        blocked=True,
    )
    calls: list[list[str]] = []

    def fake_run(cmd, capture_output=True, text=True, timeout=None, check=False):
        calls.append(list(cmd))
        return subprocess.CompletedProcess(args=cmd, returncode=0, stdout="", stderr="")

    monkeypatch.setenv("FLEET_COORD_IDLE_TTL_H", "1")
    monkeypatch.setattr(subprocess, "run", fake_run)

    archived = supervisor._run_idle_agent_archive_pass(
        project="fleet", home=fleet_home, fleet_bin="fleet",
        now_unix=2000000000.0, log_stream=io.StringIO(),
    )
    assert archived == 0


def test_idle_agent_archive_pass_skips_fresh(
    fleet_home: Path, monkeypatch,
) -> None:
    """Fresh records (heartbeat younger than TTL) stay live."""
    # Heartbeat 30 minutes before now; TTL=1h → not yet eligible.
    _write_agent_record(
        fleet_home, "dddd0001",
        project="fleet",
        last_activity_ts="2026-05-11T00:30:00Z",
    )
    calls: list[list[str]] = []

    def fake_run(cmd, capture_output=True, text=True, timeout=None, check=False):
        calls.append(list(cmd))
        return subprocess.CompletedProcess(args=cmd, returncode=0, stdout="", stderr="")

    # 30 min after heartbeat — well under the 1h TTL.
    heartbeat = supervisor._parse_rfc3339("2026-05-11T00:30:00Z")
    assert heartbeat is not None
    now_unix = heartbeat + 30 * 60
    monkeypatch.setenv("FLEET_COORD_IDLE_TTL_H", "1")
    monkeypatch.setattr(subprocess, "run", fake_run)

    archived = supervisor._run_idle_agent_archive_pass(
        project="fleet", home=fleet_home, fleet_bin="fleet",
        now_unix=now_unix, log_stream=io.StringIO(),
    )
    assert archived == 0
    assert [c for c in calls if "rm" in c] == []


def test_idle_agent_archive_pass_skips_non_claude_engine(
    fleet_home: Path, monkeypatch,
) -> None:
    """Multi-engine MVP (memory project_codex_multi_engine.md): codex
    agents never run Claude Code's Stop hook, so fleet-guard never
    updates their last_activity_ts. An actively-running codex agent
    would look "stale-since-spawn" to the idle-archive sweep, and
    without an engine gate the sweep would `fleet rm` it out from
    under the operator. Engine-gate: skip records where
    engine != "claude-code" until a sibling skill writes heartbeats
    for them.
    """
    # Codex agent with ancient last_activity_ts (would normally be
    # eligible for archive based on staleness alone).
    _write_agent_record(
        fleet_home, "cccc1234",
        project="fleet",
        engine="codex",
        last_activity_ts="2020-01-01T00:00:00Z",
    )
    # And a claude-code control: same staleness, but engine=claude-code
    # so it SHOULD be archived. This pins the engine-gate as the only
    # axis the new branch toggles on.
    _write_agent_record(
        fleet_home, "ccaa1234",
        project="fleet",
        engine="claude-code",
        last_activity_ts="2020-01-01T00:00:00Z",
    )
    calls: list[list[str]] = []

    def fake_run(cmd, capture_output=True, text=True, timeout=None, check=False):
        calls.append(list(cmd))
        return subprocess.CompletedProcess(
            args=cmd, returncode=0, stdout="", stderr="",
        )

    monkeypatch.setenv("FLEET_COORD_IDLE_TTL_H", "1")
    monkeypatch.setattr(subprocess, "run", fake_run)

    archived = supervisor._run_idle_agent_archive_pass(
        project="fleet", home=fleet_home, fleet_bin="fleet",
        now_unix=2000000000.0, log_stream=io.StringIO(),
    )
    # Only the claude-code agent was archived; codex was skipped.
    assert archived == 1
    rm_ids = [c[2] for c in calls if c[1:2] == ["rm"]]
    assert rm_ids == ["ccaa1234"], (
        f"only claude-code agent should be archived; got: {rm_ids}"
    )


def test_idle_agent_archive_pass_legacy_record_treated_as_claude(
    fleet_home: Path, monkeypatch,
) -> None:
    """Legacy agent records (written before v0.9 multi-engine) lack
    the `engine` field. The sweep treats missing/empty engine as
    `claude-code` so the v0 idle-archive behavior is preserved
    byte-for-byte on legacy installs."""
    agents_dir = fleet_home / "agents"
    agents_dir.mkdir(parents=True, exist_ok=True)
    # Legacy record: NO engine field.
    rec = {
        "schema_version": 1,
        "id": "aaaa9999",
        "project": "fleet",
        "last_activity_ts": "2020-01-01T00:00:00Z",
        "needs_input": False,
        "blocked": False,
    }
    (agents_dir / "aaaa9999.json").write_text(json.dumps(rec), encoding="utf-8")

    calls: list[list[str]] = []

    def fake_run(cmd, capture_output=True, text=True, timeout=None, check=False):
        calls.append(list(cmd))
        return subprocess.CompletedProcess(
            args=cmd, returncode=0, stdout="", stderr="",
        )

    monkeypatch.setenv("FLEET_COORD_IDLE_TTL_H", "1")
    monkeypatch.setattr(subprocess, "run", fake_run)

    archived = supervisor._run_idle_agent_archive_pass(
        project="fleet", home=fleet_home, fleet_bin="fleet",
        now_unix=2000000000.0, log_stream=io.StringIO(),
    )
    assert archived == 1
    rm_ids = [c[2] for c in calls if c[1:2] == ["rm"]]
    assert rm_ids == ["aaaa9999"]


def test_idle_agent_archive_pass_scopes_to_own_project(
    fleet_home: Path, monkeypatch,
) -> None:
    """Each project's coord only archives its OWN agents — a sibling
    project's stale agent stays untouched (that project's coord owns
    it)."""
    _write_agent_record(
        fleet_home, "aaaa1111",
        project="fleet",  # this coord's project
        last_activity_ts="2020-01-01T00:00:00Z",
    )
    _write_agent_record(
        fleet_home, "bbbb1111",
        project="rainier",  # sibling project
        last_activity_ts="2020-01-01T00:00:00Z",
    )
    calls: list[list[str]] = []

    def fake_run(cmd, capture_output=True, text=True, timeout=None, check=False):
        calls.append(list(cmd))
        return subprocess.CompletedProcess(args=cmd, returncode=0, stdout="", stderr="")

    monkeypatch.setenv("FLEET_COORD_IDLE_TTL_H", "1")
    monkeypatch.setattr(subprocess, "run", fake_run)

    archived = supervisor._run_idle_agent_archive_pass(
        project="fleet", home=fleet_home, fleet_bin="fleet",
        now_unix=2000000000.0, log_stream=io.StringIO(),
    )
    assert archived == 1
    rm_calls = [c[2] for c in calls if c[1:2] == ["rm"]]
    assert rm_calls == ["aaaa1111"], f"expected only our agent rm'd, got: {rm_calls}"


def test_idle_agent_archive_pass_tolerates_rm_failure(
    fleet_home: Path, monkeypatch,
) -> None:
    """`fleet rm` exit != 0 (e.g., pending handoff blocks) logs to
    stderr but never crashes the sweep. The next pass retries."""
    _write_agent_record(
        fleet_home, "cccc0002",
        project="fleet",
        last_activity_ts="2020-01-01T00:00:00Z",
    )

    def fake_run(cmd, capture_output=True, text=True, timeout=None, check=False):
        return subprocess.CompletedProcess(
            args=cmd, returncode=1, stdout="",
            stderr="agent has pending handoff",
        )

    monkeypatch.setenv("FLEET_COORD_IDLE_TTL_H", "1")
    monkeypatch.setattr(subprocess, "run", fake_run)

    log = io.StringIO()
    archived = supervisor._run_idle_agent_archive_pass(
        project="fleet", home=fleet_home, fleet_bin="fleet",
        now_unix=2000000000.0, log_stream=log,
    )
    assert archived == 0
    assert "exit=1" in log.getvalue()


def test_idle_agent_archive_pass_tolerates_missing_agents_dir(
    fleet_home: Path, monkeypatch,
) -> None:
    """No ~/.fleet/agents/ at all (fresh install) returns 0 cleanly."""
    # fleet_home fixture creates the dir but no agents subdir — keep it
    # absent for this test. Verify it doesn't exist.
    agents_dir = fleet_home / "agents"
    assert not agents_dir.exists()

    monkeypatch.setenv("FLEET_COORD_IDLE_TTL_H", "1")
    archived = supervisor._run_idle_agent_archive_pass(
        project="fleet", home=fleet_home, fleet_bin="fleet",
        now_unix=2000000000.0, log_stream=io.StringIO(),
    )
    assert archived == 0


def test_idle_agent_archive_pass_skips_malformed_record(
    fleet_home: Path, monkeypatch,
) -> None:
    """A hand-edited record with bad JSON is skipped (not crashed)."""
    agents_dir = fleet_home / "agents"
    agents_dir.mkdir(parents=True, exist_ok=True)
    (agents_dir / "eeee0000.json").write_text("{not valid json", encoding="utf-8")

    monkeypatch.setenv("FLEET_COORD_IDLE_TTL_H", "1")
    archived = supervisor._run_idle_agent_archive_pass(
        project="fleet", home=fleet_home, fleet_bin="fleet",
        now_unix=2000000000.0, log_stream=io.StringIO(),
    )
    assert archived == 0


# ---------- Invariant 4: adaptive cadence (base 5s → backoff 30s) ----------


def _adaptive_cfg(**overrides) -> supervisor.SupervisorConfig:
    """Cfg with adaptive cadence ENABLED. Differs from _cfg by setting
    poll_base_interval_s > 0; the legacy _cfg pins it to 0 so existing
    tests stay on the single-rate driver."""
    base = dict(
        poll_interval_s=5, stuck_check_every=0,
        stuck_threshold_s=180, stuck_polls=3, nudge_cooldown_s=120,
        poll_max_s=14400,
        poll_base_interval_s=5,
        poll_backoff_interval_s=30,
        poll_stability_window_s=300,
    )
    base.update(overrides)
    return supervisor.SupervisorConfig(**base)


def test_legacy_mode_stuck_check_cadence_uses_wall_clock(fleet_home: Path) -> None:
    """Codex iter-8 [P2] regress: forced-wake zero-sleep iterations
    used to inflate poll_count and could trigger the periodic / stuck
    check ladders prematurely in legacy mode. Wall-clock cadence
    means a burst of zero-sleep iterations does NOT advance the
    cadence — only elapsed seconds do."""
    a_path = (
        fleet_home / "projects" / "fleet" / "workers" / "alpha-aaaa" / "state.json"
    )
    _write_state_json(a_path, phase="tdd-red", updated_at="1970-01-01T00:00:00Z")
    probe = supervisor.WorkerProbe(
        slug="alpha-aaaa", state_path=a_path,
        agent_id="aaaaaaaa", tmux_session="fleet-aaaaaaaa",
    )
    stuck_count = {"n": 0}

    def fake_pass(*, probes, project, home, fleet_bin, cfg, now_unix, log_stream, coord_id="", stuck_alert_mtimes=None):
        stuck_count["n"] += 1
        return supervisor._StuckPassResult()
    import unittest.mock as mock
    fake_now_state = {"t": 0.0}

    def fake_sleep(s):
        fake_now_state["t"] += s if s > 0 else 0.0

    seq = iter([[probe]] * 20 + [[]])

    def fake_refresh():
        try:
            return next(seq)
        except StopIteration:
            return []

    cfg = _cfg(
        poll_interval_s=30, stuck_check_every=10,
    )
    # All iterations force-tick → zero sleep. With legacy poll-count
    # cadence, 20 zero-sleep iterations × stuck_check_every=10 would
    # trigger stuck-check twice. With wall-clock cadence, no time
    # passes (fake_sleep recorded 0 every iter), so cadence target
    # (30 × 10 = 300 s) is never reached → zero stuck-checks.
    with mock.patch.object(supervisor, "_run_stuck_check_pass", fake_pass):
        supervisor.run_supervisor(
            cfg=cfg, project="fleet", home=fleet_home, fleet_bin="fleet",
            sleep_fn=fake_sleep, now_fn=lambda: fake_now_state["t"],
            refresh_probes=fake_refresh,
            reconcile_one=lambda p: None,
            write_state=lambda: None,
            force_tick_check=lambda: True,
            log_stream=io.StringIO(),
        )
    assert stuck_count["n"] == 0, (
        f"forced-wake bursts triggered premature stuck-check: {stuck_count['n']}"
    )


def test_env_legacy_poll_interval_does_not_force_adaptive_cadence(
    monkeypatch,
) -> None:
    """Codex iter-5 [P2]: an operator who only sets
    FLEET_COORD_POLL_INTERVAL_S=60 must keep getting 60 s polls.
    Without an explicit FLEET_COORD_POLL_BASE_INTERVAL_S, the adaptive
    cadence stays off (poll_base_interval_s=0)."""
    monkeypatch.setenv("FLEET_COORD_POLL_INTERVAL_S", "60")
    monkeypatch.delenv("FLEET_COORD_POLL_BASE_INTERVAL_S", raising=False)
    cfg = supervisor.SupervisorConfig.from_env()
    assert cfg.poll_interval_s == 60
    # Adaptive cadence disabled — falls through to poll_interval_s.
    assert cfg.poll_base_interval_s == 0


def test_env_explicit_base_interval_enables_adaptive_cadence(
    monkeypatch,
) -> None:
    """Operator who explicitly sets FLEET_COORD_POLL_BASE_INTERVAL_S
    (even to the default 5) gets the adaptive driver."""
    monkeypatch.setenv("FLEET_COORD_POLL_BASE_INTERVAL_S", "5")
    cfg = supervisor.SupervisorConfig.from_env()
    assert cfg.poll_base_interval_s == 5


def test_poll_cadence_default_5s() -> None:
    """When no probe has been stable for stability_window, the next
    sleep is poll_base_interval_s (default 5)."""
    cfg = _adaptive_cfg()
    probe = supervisor.WorkerProbe(
        slug="alpha-aaaa", state_path=Path("/tmp/nope"),
        agent_id="aaaaaaaa", tmux_session="fleet-aaaaaaaa",
    )
    # Fresh start: last_change_ts == now → age 0 < stability window.
    s = supervisor.compute_next_sleep_s(
        cfg=cfg, probes=[probe],
        last_change_ts={"alpha-aaaa": 1000.0},
        now_unix=1000.0,
    )
    assert s == 5.0


def test_poll_cadence_adaptive_backoff_after_5min_stable() -> None:
    """When EVERY probe has been stable for > stability_window, the
    sleep dilates to poll_backoff_interval_s."""
    cfg = _adaptive_cfg()
    probe = supervisor.WorkerProbe(
        slug="alpha-aaaa", state_path=Path("/tmp/nope"),
        agent_id="aaaaaaaa", tmux_session="fleet-aaaaaaaa",
    )
    # last_change 600s ago > stability_window=300 → backoff.
    s = supervisor.compute_next_sleep_s(
        cfg=cfg, probes=[probe],
        last_change_ts={"alpha-aaaa": 1000.0},
        now_unix=1600.0,
    )
    assert s == 30.0


def test_poll_cadence_one_active_probe_keeps_loop_at_base() -> None:
    """A single hot probe pulls the whole loop back to base cadence —
    matches the spec: 'default 5 s PER subagent'."""
    cfg = _adaptive_cfg()
    hot = supervisor.WorkerProbe(
        slug="hot-aaaa", state_path=Path("/tmp/nope"),
        agent_id="aaaaaaaa", tmux_session="fleet-aaaaaaaa",
    )
    cold = supervisor.WorkerProbe(
        slug="cold-bbbb", state_path=Path("/tmp/nope2"),
        agent_id="bbbbbbbb", tmux_session="fleet-bbbbbbbb",
    )
    s = supervisor.compute_next_sleep_s(
        cfg=cfg, probes=[hot, cold],
        # hot probe just changed; cold has been stable for hours.
        last_change_ts={"hot-aaaa": 1500.0, "cold-bbbb": 0.0},
        now_unix=1500.0,
    )
    assert s == 5.0


def test_poll_cadence_legacy_single_rate_when_base_zero() -> None:
    """poll_base_interval_s=0 disables adaptive cadence — fall back to
    poll_interval_s. This preserves v0.2.x byte-identical behavior for
    legacy tests + operators."""
    cfg = _cfg(poll_interval_s=30)
    probe = supervisor.WorkerProbe(
        slug="alpha-aaaa", state_path=Path("/tmp/nope"),
        agent_id="aaaaaaaa", tmux_session="fleet-aaaaaaaa",
    )
    s = supervisor.compute_next_sleep_s(
        cfg=cfg, probes=[probe],
        last_change_ts={"alpha-aaaa": 0.0},
        now_unix=100000.0,
    )
    assert s == 30.0


def test_poll_cadence_force_tick_on_inbox_event(fleet_home: Path) -> None:
    """force_tick_check returning True → sleep_fn invoked with 0
    (no actual wait). Verified by capturing sleep_fn args."""
    a_path = (
        fleet_home / "projects" / "fleet" / "workers" / "alpha-aaaa" / "state.json"
    )
    _write_state_json(a_path, phase="tdd-red", updated_at="2026-01-01T00:00:00Z")
    probe = supervisor.WorkerProbe(
        slug="alpha-aaaa", state_path=a_path,
        agent_id="aaaaaaaa", tmux_session="fleet-aaaaaaaa",
    )
    sleep_calls: list[float] = []
    fake_now = {"t": 0.0}

    def fake_sleep(s):
        sleep_calls.append(s)
        fake_now["t"] += s if s > 0 else 1.0

    # First refresh returns the probe; second returns [] to exit loop.
    seq = iter([[probe], [probe], []])

    def fake_refresh():
        try:
            return next(seq)
        except StopIteration:
            return []

    # First iteration: force-tick returns True → sleep_s=0.
    # Second iteration: returns False → sleep_s>0.
    force_calls = {"n": 0}

    def fake_force():
        force_calls["n"] += 1
        return force_calls["n"] == 1

    cfg = _adaptive_cfg(stuck_check_every=0)
    res = supervisor.run_supervisor(
        cfg=cfg, project="fleet", home=fleet_home, fleet_bin="fleet",
        sleep_fn=fake_sleep, now_fn=lambda: fake_now["t"],
        refresh_probes=fake_refresh,
        reconcile_one=lambda p: None,
        write_state=lambda: None,
        force_tick_check=fake_force,
        log_stream=io.StringIO(),
    )
    # Iter 1: force-tick fired → 0.1s throttle sleep (codex iter-19
    # [P2]: floor on forced wakes to prevent spin on parse-error
    # spin scenarios).
    # Iter 2: force-tick False → full base/backoff cadence (5 or 30 s).
    assert force_calls["n"] >= 2
    assert any(s >= 5.0 for s in sleep_calls), sleep_calls
    # The first sleep is the throttle for the forced wake; the second
    # is the genuine cadence sleep after force-tick returned False.
    assert sleep_calls[0] == 0.1
    assert res.exit_reason == "all-terminal"


def test_poll_detects_stuck_via_last_activity_ts_plus_session_alive(
    fleet_home: Path, monkeypatch,
) -> None:
    """Invariant 4 stuck condition: last_activity_ts stale AND tmux
    alive AND status=running → mark stuck. Verify is_stuck_idle
    captures all four conditions."""
    state = {
        "phase": "tdd-red",
        "updated_at": "1970-01-01T00:00:00Z",  # very stale
    }
    sup = supervisor.WorkerSupervisorState(
        slug="alpha-aaaa", consecutive_stuck_polls=5,
    )
    cfg = _cfg(stuck_polls=3, stuck_threshold_s=10)
    assert supervisor.is_stuck_idle(
        state, sup, cfg=cfg, session_alive=True, now_unix=10_000.0,
    ) is True
    # Tmux dead breaks the contract — not stuck (nothing to nudge).
    assert supervisor.is_stuck_idle(
        state, sup, cfg=cfg, session_alive=False, now_unix=10_000.0,
    ) is False
    # Terminal phase breaks it too.
    state2 = {"phase": "done", "updated_at": "1970-01-01T00:00:00Z"}
    assert supervisor.is_stuck_idle(
        state2, sup, cfg=cfg, session_alive=True, now_unix=10_000.0,
    ) is False


def test_stuck_alert_drops_inbox_line(fleet_home: Path) -> None:
    """emit_stuck_alert writes [STUCK] line into ~/.fleet/inbox/<coord>.md.
    The TUI/operator surface is this file."""
    target = supervisor.emit_stuck_alert(
        "c00bf001", "alpha-aaaa", fleet_home=fleet_home,
        detail="phase=tdd-red idle since X",
    )
    assert target
    body = (fleet_home / "inbox" / "c00bf001.md").read_text()
    assert "[STUCK]" in body
    assert "alpha-aaaa" in body
    assert "phase=tdd-red" in body


def test_stuck_alert_appends_does_not_clobber(fleet_home: Path) -> None:
    """Append-only: an existing inbox file is preserved."""
    inbox = fleet_home / "inbox" / "c00bf001.md"
    inbox.write_text("[OPERATOR] previous message\n", encoding="utf-8")
    supervisor.emit_stuck_alert(
        "c00bf001", "alpha-aaaa", fleet_home=fleet_home, detail="d",
    )
    body = inbox.read_text()
    assert "[OPERATOR] previous message" in body
    assert "[STUCK]" in body


def test_has_pending_inbox_events_true_on_direct_inbox(fleet_home: Path) -> None:
    """Direct inbox file present with mtime > baseline → True."""
    (fleet_home / "inbox" / "c00bf001.md").write_text("hi", encoding="utf-8")
    # Default baseline=0.0 → any mtime triggers event.
    assert supervisor.has_pending_inbox_events(
        "c00bf001", fleet_home=fleet_home, last_seen_archive="",
    ) is True


def test_has_pending_inbox_events_false_when_baseline_matches_mtime(
    fleet_home: Path,
) -> None:
    """Codex iter-1 [P1] regress: a coord inbox file that persists
    across the supervisor session must NOT keep force-ticking. With
    baseline = current mtime, only an mtime advance counts."""
    inbox = fleet_home / "inbox" / "c00bf001.md"
    inbox.write_text("hi", encoding="utf-8")
    import os as _os
    cur_mtime = _os.stat(inbox).st_mtime
    # Pretend the supervisor recorded this mtime at session start.
    assert supervisor.has_pending_inbox_events(
        "c00bf001", fleet_home=fleet_home, last_seen_archive="",
        direct_inbox_mtime_baseline=cur_mtime,
    ) is False
    # Touch the file (advance mtime) — event fires.
    _os.utime(inbox, (cur_mtime + 10, cur_mtime + 10))
    assert supervisor.has_pending_inbox_events(
        "c00bf001", fleet_home=fleet_home, last_seen_archive="",
        direct_inbox_mtime_baseline=cur_mtime,
    ) is True


def test_has_pending_inbox_events_true_on_archive_post_watermark(
    fleet_home: Path,
) -> None:
    """Archive file > watermark → force-tick returns True."""
    archive = fleet_home / "inbox" / "archive"
    archive.mkdir(parents=True, exist_ok=True)
    (archive / "c00bf001-20260101-000000Z-msg.md").write_text("hi", encoding="utf-8")
    # Watermark below the file: should detect it.
    assert supervisor.has_pending_inbox_events(
        "c00bf001", fleet_home=fleet_home,
        last_seen_archive="c00bf001-20250101-000000Z-old.md",
    ) is True
    # Watermark at-or-above the file: no event.
    assert supervisor.has_pending_inbox_events(
        "c00bf001", fleet_home=fleet_home,
        last_seen_archive="c00bf001-20270101-000000Z-old.md",
    ) is False


def test_has_pending_inbox_events_false_on_empty(fleet_home: Path) -> None:
    """No inbox files at all → False (no event)."""
    assert supervisor.has_pending_inbox_events(
        "c00bf001", fleet_home=fleet_home, last_seen_archive="",
    ) is False


def test_has_pending_inbox_events_false_on_empty_coord_id(fleet_home: Path) -> None:
    """Empty coord_id is a no-op (we don't know whose inbox to check)."""
    assert supervisor.has_pending_inbox_events(
        "", fleet_home=fleet_home, last_seen_archive="",
    ) is False


def test_force_tick_does_not_spin_after_first_hit(fleet_home: Path) -> None:
    """Codex iter-2 [P1] regress: after a force-tick fires on an
    inbox event, the supervisor MUST eventually sleep again (the event
    is processed and the baseline/watermark advances). Without this
    advance, the supervisor spins at 0-second sleeps forever.

    Driven via the production loop.tick path so the integration is
    end-to-end: place an inbox file, run a single supervisor iteration
    that force-ticks once, then assert the NEXT iteration would NOT
    force-tick again."""
    inbox = fleet_home / "inbox" / "c00bf001.md"
    inbox.write_text("[OPERATOR] hi\n", encoding="utf-8")
    import os as _os
    cur_mtime = _os.stat(inbox).st_mtime
    # Build the hook the way loop.py does, with a mutable baseline.
    baseline = {"mtime": cur_mtime - 10.0, "archive": ""}

    def hook():
        triggered = supervisor.has_pending_inbox_events(
            "c00bf001", fleet_home=fleet_home,
            last_seen_archive=baseline["archive"],
            direct_inbox_mtime_baseline=baseline["mtime"],
        )
        if triggered:
            try:
                baseline["mtime"] = _os.stat(inbox).st_mtime
            except OSError:
                pass
        return triggered

    # First call: mtime > baseline → fires.
    assert hook() is True
    # Second call: baseline now equals mtime → does NOT fire.
    assert hook() is False


def test_supervisor_force_tick_skips_sleep_when_inbox_event_pending(
    fleet_home: Path,
) -> None:
    """Wire test: force_tick_check returning True causes the next
    iteration to NOT sleep (sleep_fn invoked with 0 → loop skips).
    Verify with sleep_calls capture."""
    a_path = (
        fleet_home / "projects" / "fleet" / "workers" / "alpha-aaaa" / "state.json"
    )
    _write_state_json(a_path, phase="tdd-red", updated_at="2026-01-01T00:00:00Z")
    probe = supervisor.WorkerProbe(
        slug="alpha-aaaa", state_path=a_path,
        agent_id="aaaaaaaa", tmux_session="fleet-aaaaaaaa",
    )
    sleep_calls: list[float] = []
    fake_now = {"t": 0.0}

    def fake_sleep(s):
        sleep_calls.append(s)
        fake_now["t"] += s

    seq = iter([[probe], [probe], []])

    def fake_refresh():
        try:
            return next(seq)
        except StopIteration:
            return []

    # ALWAYS force-tick → 0.1s throttle on each iteration (codex
    # iter-19 [P2]: floor on forced wakes to prevent spin on
    # parse-error scenarios).
    cfg = _adaptive_cfg(stuck_check_every=0)
    res = supervisor.run_supervisor(
        cfg=cfg, project="fleet", home=fleet_home, fleet_bin="fleet",
        sleep_fn=fake_sleep, now_fn=lambda: fake_now["t"],
        refresh_probes=fake_refresh,
        reconcile_one=lambda p: None,
        write_state=lambda: None,
        force_tick_check=lambda: True,
        log_stream=io.StringIO(),
    )
    # Every forced iteration applied the 0.1s throttle.
    assert all(s == 0.1 for s in sleep_calls), sleep_calls
    assert res.iterations >= 1


def test_reaper_hook_return_triggers_reconcile_for_just_reaped_slugs(
    fleet_home: Path,
) -> None:
    """Codex iter-3 [P2]: when the reaper hook returns a list of slugs
    whose kill cycle completed THIS iteration, those slugs get added
    to `changed` so reconcile re-runs against them — closing the
    "deferred status flip waits for periodic full reconcile" gap that
    blocks cap=1 dispatch for ~5 min."""
    a_path = (
        fleet_home / "projects" / "fleet" / "workers" / "alpha-aaaa" / "state.json"
    )
    _write_state_json(a_path, phase="done", updated_at="2026-01-01T00:00:00Z")
    probe = supervisor.WorkerProbe(
        slug="alpha-aaaa", state_path=a_path,
        agent_id="aaaaaaaa", tmux_session="fleet-aaaaaaaa",
    )
    reconcile_calls: list[str] = []

    def fake_reconcile(p):
        reconcile_calls.append(p.slug)

    # Reaper hook reports alpha-aaaa just got reaped (returned the slug).
    def fake_reap(probes):
        return ["alpha-aaaa"]

    sleep_calls: list[float] = []
    fake_now = {"t": 0.0}

    def fake_sleep(s):
        sleep_calls.append(s)
        fake_now["t"] += s if s > 0 else 1.0

    seq = iter([[probe], [probe], []])

    def fake_refresh():
        try:
            return next(seq)
        except StopIteration:
            return []

    cfg = _adaptive_cfg(stuck_check_every=0)
    supervisor.run_supervisor(
        cfg=cfg, project="fleet", home=fleet_home, fleet_bin="fleet",
        sleep_fn=fake_sleep, now_fn=lambda: fake_now["t"],
        refresh_probes=fake_refresh,
        reconcile_one=fake_reconcile,
        write_state=lambda: None,
        reaper_hook=fake_reap,
        log_stream=io.StringIO(),
    )
    # Even though state.json mtime didn't advance, reconcile fired
    # because the reaper hook returned alpha-aaaa as just-reaped.
    assert "alpha-aaaa" in reconcile_calls, f"reconcile_calls={reconcile_calls}"


def test_supervisor_exits_when_operator_writes_direct_inbox(
    fleet_home: Path,
) -> None:
    """Codex iter-10 [P2] regress: the coord inbox at
    ~/.fleet/inbox/<coord>.md is consumed by fleet-guard's
    SessionStart hook on the NEXT Claude-agent turn, not by the coord
    skill itself. While the supervisor is running, an operator message
    sent via `fleet message <coord>` would otherwise stay invisible
    until the supervisor exits (could be 4 h).

    Fix: when a direct-inbox file's mtime ADVANCES past the start-of-
    session baseline (codex iter-11 [P1]), exit the supervisor so
    the next turn fires fleet-guard.
    """
    a_path = (
        fleet_home / "projects" / "fleet" / "workers" / "alpha-aaaa" / "state.json"
    )
    _write_state_json(a_path, phase="tdd-red", updated_at="2026-01-01T00:00:00Z")
    probe = supervisor.WorkerProbe(
        slug="alpha-aaaa", state_path=a_path,
        agent_id="aaaaaaaa", tmux_session="fleet-aaaaaaaa",
    )
    # Pre-existing inbox file (e.g., a [STUCK] alert from a previous
    # supervisor session); baseline must capture this so it doesn't
    # cause a premature exit. Set mtime to a known-old value so the
    # supervisor records that as the baseline.
    inbox = fleet_home / "inbox" / "c00bf001.md"
    inbox.write_text("[STUCK] stale alpha-aaaa phase=tdd-red\n", encoding="utf-8")
    import os as _os
    pre_mtime = 1000.0
    _os.utime(inbox, (pre_mtime, pre_mtime))

    # force_tick will fire each iteration; on the second call, we
    # bump the inbox mtime past the baseline to simulate an operator
    # writing a new message mid-supervision.
    check_count = {"n": 0}

    def fake_force():
        check_count["n"] += 1
        if check_count["n"] == 2:
            _os.utime(inbox, (pre_mtime + 100, pre_mtime + 100))
        return True

    sleep_calls: list[float] = []
    fake_now = {"t": 0.0}

    def fake_sleep(s):
        sleep_calls.append(s)
        fake_now["t"] += s if s > 0 else 1.0

    seq = iter([[probe], [probe], [probe], []])

    def fake_refresh():
        try:
            return next(seq)
        except StopIteration:
            return []

    cfg = _adaptive_cfg(stuck_check_every=0)
    res = supervisor.run_supervisor(
        cfg=cfg, project="fleet", home=fleet_home, fleet_bin="fleet",
        sleep_fn=fake_sleep, now_fn=lambda: fake_now["t"],
        refresh_probes=fake_refresh,
        reconcile_one=lambda p: None,
        write_state=lambda: None,
        force_tick_check=fake_force,
        coord_id="c00bf001",
        log_stream=io.StringIO(),
    )
    # Supervisor exited because the operator wrote to the direct inbox
    # (mtime advanced past the start-of-session baseline).
    assert res.exit_reason == "operator-inbox-message"


def test_supervisor_own_stuck_alert_does_not_trigger_inbox_exit(
    fleet_home: Path, monkeypatch,
) -> None:
    """Codex iter-12 [P1] regress: when _run_stuck_check_pass emits a
    [STUCK] alert into the coord's own inbox, the supervisor MUST NOT
    treat that as an operator message and exit. The session baseline
    is bumped to swallow the supervisor's own write.

    Driven by stubbing the stuck-check pass to write to the inbox
    directly + asserting the supervisor reaches all-terminal rather
    than operator-inbox-message exit.
    """
    a_path = (
        fleet_home / "projects" / "fleet" / "workers" / "alpha-aaaa" / "state.json"
    )
    _write_state_json(a_path, phase="tdd-red", updated_at="2026-01-01T00:00:00Z")
    probe = supervisor.WorkerProbe(
        slug="alpha-aaaa", state_path=a_path,
        agent_id="aaaaaaaa", tmux_session="fleet-aaaaaaaa",
    )
    # No pre-existing inbox file — baseline=0; the stub will create it.
    import os as _os
    inbox = fleet_home / "inbox" / "c00bf001.md"

    # Stub stuck-check to write the [STUCK] alert (simulating
    # _run_stuck_check_pass's emit_stuck_alert call). The stub also
    # appends the post-write mtime to stuck_alert_mtimes so the
    # supervisor's baseline-update path treats our write as a
    # supervisor-side write (not an operator message).
    def fake_pass(*, probes, project, home, fleet_bin, cfg, now_unix, log_stream, coord_id="", stuck_alert_mtimes=None):
        inbox.write_text("[STUCK] alpha-aaaa\n", encoding="utf-8")
        if stuck_alert_mtimes is not None:
            stuck_alert_mtimes.append(inbox.stat().st_mtime)
        return supervisor._StuckPassResult(nudges=1, stuck_alerts=1)
    monkeypatch.setattr(supervisor, "_run_stuck_check_pass", fake_pass)

    sleep_calls: list[float] = []
    fake_now = {"t": 0.0}

    def fake_sleep(s):
        sleep_calls.append(s)
        fake_now["t"] += s if s > 0 else 1.0

    seq = iter([[probe], [probe], [probe], [probe], []])

    def fake_refresh():
        try:
            return next(seq)
        except StopIteration:
            return []

    # stuck_check_every=1 → run stuck pass every iter; force_tick=True →
    # every iter is forced. Without the baseline bump, the supervisor
    # would exit after the first stuck-check write.
    cfg = _adaptive_cfg(stuck_check_every=1)
    res = supervisor.run_supervisor(
        cfg=cfg, project="fleet", home=fleet_home, fleet_bin="fleet",
        sleep_fn=fake_sleep, now_fn=lambda: fake_now["t"],
        refresh_probes=fake_refresh,
        reconcile_one=lambda p: None,
        write_state=lambda: None,
        force_tick_check=lambda: True,
        coord_id="c00bf001",
        log_stream=io.StringIO(),
    )
    # Supervisor reached all-terminal — its OWN [STUCK] writes did NOT
    # trigger the operator-inbox exit.
    assert res.exit_reason == "all-terminal"


def test_supervisor_does_not_exit_on_stale_inbox_file(fleet_home: Path) -> None:
    """Codex iter-11 [P1] regress: a pre-existing inbox file (e.g., a
    [STUCK] alert dropped by emit_stuck_alert itself in a prior tick)
    must NOT trigger the operator-message-exit on every forced wake.
    The exit only fires when mtime advances past the start-of-session
    baseline."""
    a_path = (
        fleet_home / "projects" / "fleet" / "workers" / "alpha-aaaa" / "state.json"
    )
    _write_state_json(a_path, phase="tdd-red", updated_at="2026-01-01T00:00:00Z")
    probe = supervisor.WorkerProbe(
        slug="alpha-aaaa", state_path=a_path,
        agent_id="aaaaaaaa", tmux_session="fleet-aaaaaaaa",
    )
    # Pre-existing inbox file — should NOT trigger exit because the
    # mtime baseline captures its initial state.
    inbox = fleet_home / "inbox" / "c00bf001.md"
    inbox.write_text("[STUCK] stale\n", encoding="utf-8")

    sleep_calls: list[float] = []
    fake_now = {"t": 0.0}

    def fake_sleep(s):
        sleep_calls.append(s)
        fake_now["t"] += s if s > 0 else 1.0

    seq = iter([[probe], [probe], []])

    def fake_refresh():
        try:
            return next(seq)
        except StopIteration:
            return []

    cfg = _adaptive_cfg(stuck_check_every=0)
    res = supervisor.run_supervisor(
        cfg=cfg, project="fleet", home=fleet_home, fleet_bin="fleet",
        sleep_fn=fake_sleep, now_fn=lambda: fake_now["t"],
        refresh_probes=fake_refresh,
        reconcile_one=lambda p: None,
        write_state=lambda: None,
        force_tick_check=lambda: True,
        coord_id="c00bf001",
        log_stream=io.StringIO(),
    )
    # Supervisor reached "all-terminal" — the stale inbox did NOT
    # trigger a premature operator-message exit.
    assert res.exit_reason == "all-terminal"


def test_reaper_hook_runs_before_reconcile_on_mtime_change(
    fleet_home: Path,
) -> None:
    """Codex iter-1 [P1] regress: when a worker's state.json mtime
    advances mid-supervisor session (phase=done write), the reaper hook
    MUST run BEFORE reconcile_one. Otherwise reconcile would flip status
    + forget the agent_id mapping before the reaper sends /exit — leaking
    the tmux session as an orphan. Order verified by capturing call
    ordering."""
    a_path = (
        fleet_home / "projects" / "fleet" / "workers" / "alpha-aaaa" / "state.json"
    )
    _write_state_json(a_path, phase="tdd-red", updated_at="2026-01-01T00:00:00Z")
    probe = supervisor.WorkerProbe(
        slug="alpha-aaaa", state_path=a_path,
        agent_id="aaaaaaaa", tmux_session="fleet-aaaaaaaa",
    )
    call_order: list[str] = []

    def fake_reap(probes):
        call_order.append("reap")

    def fake_reconcile_one(p):
        call_order.append("reconcile")

    sleep_calls: list[float] = []
    fake_now = {"t": 0.0}

    def fake_sleep(s):
        sleep_calls.append(s)
        fake_now["t"] += s if s > 0 else 1.0
        # Touch the state.json so the NEXT iteration sees mtime change.
        os.utime(a_path, None)

    seq = iter([[probe], [probe], []])

    def fake_refresh():
        try:
            return next(seq)
        except StopIteration:
            return []

    cfg = _adaptive_cfg(stuck_check_every=0)
    supervisor.run_supervisor(
        cfg=cfg, project="fleet", home=fleet_home, fleet_bin="fleet",
        sleep_fn=fake_sleep, now_fn=lambda: fake_now["t"],
        refresh_probes=fake_refresh,
        reconcile_one=fake_reconcile_one,
        write_state=lambda: None,
        reaper_hook=fake_reap,
        log_stream=io.StringIO(),
    )
    # First active iteration: reap, then reconcile. Verify reap precedes
    # reconcile every time both fire.
    reap_idx = [i for i, c in enumerate(call_order) if c == "reap"]
    reconcile_idx = [i for i, c in enumerate(call_order) if c == "reconcile"]
    assert reap_idx, f"reaper hook never fired: {call_order}"
    if reconcile_idx:
        # If reconcile fired this iteration, the reap call must precede it.
        for r_idx in reconcile_idx:
            preceding_reaps = [i for i in reap_idx if i < r_idx]
            assert preceding_reaps, (
                f"reconcile fired before reaper: {call_order}"
            )


def test_reaper_hook_called_each_iteration(fleet_home: Path) -> None:
    """The reaper_hook is invoked once per supervisor iteration. Verify
    by counting calls."""
    a_path = (
        fleet_home / "projects" / "fleet" / "workers" / "alpha-aaaa" / "state.json"
    )
    _write_state_json(a_path, phase="tdd-red", updated_at="2026-01-01T00:00:00Z")
    probe = supervisor.WorkerProbe(
        slug="alpha-aaaa", state_path=a_path,
        agent_id="aaaaaaaa", tmux_session="fleet-aaaaaaaa",
    )
    reap_calls: list[int] = []

    def fake_reap(probes):
        reap_calls.append(len(probes))

    sleep_calls: list[float] = []
    fake_now = {"t": 0.0}

    def fake_sleep(s):
        sleep_calls.append(s)
        fake_now["t"] += s if s > 0 else 1.0

    seq = iter([[probe], [probe], [probe], []])

    def fake_refresh():
        try:
            return next(seq)
        except StopIteration:
            return []

    cfg = _adaptive_cfg(stuck_check_every=0)
    res = supervisor.run_supervisor(
        cfg=cfg, project="fleet", home=fleet_home, fleet_bin="fleet",
        sleep_fn=fake_sleep, now_fn=lambda: fake_now["t"],
        refresh_probes=fake_refresh,
        reconcile_one=lambda p: None,
        write_state=lambda: None,
        reaper_hook=fake_reap,
        log_stream=io.StringIO(),
    )
    # res.iterations counts the iteration that exited via "all-terminal"
    # too — the reaper hook only fires on iterations with active probes.
    # 3 iterations: 1 + 2 with probes, 3rd refresh returns [] → exit.
    assert len(reap_calls) >= 1
    assert all(n == 1 for n in reap_calls)
    # Total iterations include the empty-probes exit iteration.
    assert res.iterations >= len(reap_calls)


# ===========================================================================
# DESIGN-pr-watch-autoremediate §1 — the ~60-second PR-watch poll floor
# ===========================================================================


def _floor_harness(fleet_home: Path, *, floor_s, floor_due, n_iters=40,
                   rng=None):
    """Drive run_supervisor with a long stuck/periodic cadence so ONLY the
    floor can fire the PR-watch pass, with a deterministic clock. Returns
    (res, periodic_calls, sleeps)."""
    a_path = (
        fleet_home / "projects" / "fleet" / "workers" / "alpha-aaaa" / "state.json"
    )
    _write_state_json(a_path, phase="tdd-red", updated_at="2026-01-01T00:00:00Z")
    probe = supervisor.WorkerProbe(
        slug="alpha-aaaa", state_path=a_path, agent_id="aaaaaaaa",
        tmux_session="fleet-aaaaaaaa", live_worker=True,
    )
    periodic_calls = {"n": 0}

    def fake_periodic():
        periodic_calls["n"] += 1
        return None

    seq = iter([[probe]] * n_iters + [[]])

    def fake_refresh():
        try:
            return next(seq)
        except StopIteration:
            return []

    now = {"t": 0.0}
    sleeps: list[float] = []

    def fake_sleep(s):
        sleeps.append(s)
        now["t"] += s

    res = supervisor.run_supervisor(
        # poll_interval 10s, stuck_every 1000 -> periodic/stuck cadence is
        # 10000s (effectively never within this test) so ONLY the floor can
        # fire the pass.
        cfg=_cfg(poll_interval_s=10, stuck_check_every=1000,
                 pr_watch_poll_floor_s=floor_s),
        project="fleet", home=fleet_home, fleet_bin="fleet",
        sleep_fn=fake_sleep, now_fn=lambda: now["t"],
        refresh_probes=fake_refresh,
        reconcile_one=lambda p: None,
        write_state=lambda: None,
        periodic_full_reconcile=fake_periodic,
        pr_watch_floor_due=floor_due,
        rng=rng,
        log_stream=io.StringIO(),
    )
    return res, periodic_calls, sleeps


def test_poll_floor_fires_pass_when_open_watch(fleet_home: Path) -> None:
    """Floor on + at least one open watched PR -> the PR-watch PASS
    (periodic_full_reconcile) actually fires on the ~60 s cadence, not the
    slow stuck/periodic gate; the sleep budget is clamped to <= the floor."""
    import random
    res, periodic_calls, sleeps = _floor_harness(
        fleet_home, floor_s=60, floor_due=lambda: True,
        rng=random.Random(0),
    )
    assert res.pr_watch_floor_passes >= 1, "floor must fire the PR-watch pass"
    assert periodic_calls["n"] >= 1
    # every sleep is clamped to <= floor + jitter band (no 10000 s waits).
    assert sleeps, "expected at least one sleep"
    assert max(sleeps) <= 60 + 5 + 0.001


def test_poll_floor_inert_with_no_open_watch(fleet_home: Path) -> None:
    """Floor on but ZERO open watched PRs -> floor inert: no floored pass,
    and the sleep budget is NOT clamped to the floor (idle coord relaxes)."""
    res, periodic_calls, _sleeps = _floor_harness(
        fleet_home, floor_s=60, floor_due=lambda: False, n_iters=20,
    )
    assert res.pr_watch_floor_passes == 0
    # the slow periodic cadence (10000 s) never elapsed in this window.
    assert periodic_calls["n"] == 0


def test_poll_floor_disabled_legacy_cadence(fleet_home: Path) -> None:
    """PR_WATCH_POLL_FLOOR_S=0 -> the floor is OFF; even with an open watch
    the floored pass never fires (legacy: only the slow gate runs it)."""
    res, periodic_calls, _sleeps = _floor_harness(
        fleet_home, floor_s=0, floor_due=lambda: True, n_iters=20,
    )
    assert res.pr_watch_floor_passes == 0
    assert periodic_calls["n"] == 0


def test_poll_floor_env_default_is_60() -> None:
    import os
    os.environ.pop("PR_WATCH_POLL_FLOOR_S", None)
    assert supervisor.env_pr_watch_poll_floor_s() == 60
    os.environ["PR_WATCH_POLL_FLOOR_S"] = "0"
    try:
        assert supervisor.env_pr_watch_poll_floor_s() == 0
    finally:
        del os.environ["PR_WATCH_POLL_FLOOR_S"]


def test_effective_floor_jitter_bounded() -> None:
    import random
    for seed in range(20):
        f = supervisor._pr_watch_effective_floor_s(60.0, random.Random(seed))
        assert 60.0 <= f < 65.0


def test_floor_uses_pr_watch_only_pass_not_full_reconcile(fleet_home: Path) -> None:
    """codex P2: a floor-only fire (no periodic due) must invoke the
    PR-watch-ONLY pass (pr_watch_floor_pass), NOT periodic_full_reconcile —
    so the tight ~60 s floor cadence doesn't drag the legacy in-flight/CI
    sweep down to ~1 min."""
    a_path = (
        fleet_home / "projects" / "fleet" / "workers" / "alpha-aaaa" / "state.json"
    )
    _write_state_json(a_path, phase="tdd-red", updated_at="2026-01-01T00:00:00Z")
    probe = supervisor.WorkerProbe(
        slug="alpha-aaaa", state_path=a_path, agent_id="aaaaaaaa",
        tmux_session="fleet-aaaaaaaa", live_worker=True,
    )
    periodic_calls = {"n": 0}
    floor_calls = {"n": 0}

    def fake_periodic():
        periodic_calls["n"] += 1
        return None

    def fake_floor_pass():
        floor_calls["n"] += 1
        return None

    seq = iter([[probe]] * 40 + [[]])

    def fake_refresh():
        try:
            return next(seq)
        except StopIteration:
            return []

    now = {"t": 0.0}

    def fake_sleep(s):
        now["t"] += s

    res = supervisor.run_supervisor(
        cfg=_cfg(poll_interval_s=10, stuck_check_every=1000,
                 pr_watch_poll_floor_s=60),
        project="fleet", home=fleet_home, fleet_bin="fleet",
        sleep_fn=fake_sleep, now_fn=lambda: now["t"],
        refresh_probes=fake_refresh,
        reconcile_one=lambda p: None,
        write_state=lambda: None,
        periodic_full_reconcile=fake_periodic,
        pr_watch_floor_due=lambda: True,
        pr_watch_floor_pass=fake_floor_pass,
        rng=__import__("random").Random(0),
        log_stream=io.StringIO(),
    )
    assert res.pr_watch_floor_passes >= 1
    # floor fired the PR-watch-ONLY pass, NOT the full periodic reconcile.
    assert floor_calls["n"] >= 1
    assert periodic_calls["n"] == 0, "floor must not run the full legacy reconcile"
