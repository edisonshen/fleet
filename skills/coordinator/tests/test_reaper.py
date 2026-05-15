"""reaper.py tests — DESIGN-subagent-lifecycle.md invariant 5.

Tests cover the judgment table, the kill ladder (send /exit → grace →
fleet rm → hard-kill), state persistence in coord-state.json, and the
"status flip deferred until reaper succeeds" contract.

All kill primitives are stubbed via function-argument hooks so we never
exec tmux / fleet / kill -9 from these tests.
"""
from __future__ import annotations

import io
import json
import signal
from dataclasses import dataclass
from pathlib import Path

import pytest

import reaper


# ---------- helpers ----------


def _inp(
    *,
    slug: str = "alpha-aaaa",
    agent_id: str = "aaaaaaaa",
    tmux_session: str = "fleet-aaaaaaaa",
    worker_state: dict | None = None,
    task_status: str = "in-progress",
    pid: int = 12345,
    is_git: bool = True,
) -> reaper.ReapInputs:
    return reaper.ReapInputs(
        slug=slug, agent_id=agent_id, tmux_session=tmux_session,
        worker_state=worker_state, task_status=task_status,
        pid=pid, is_git=is_git,
    )


@dataclass
class _KillStubs:
    """Capture-and-return stubs for the kill ladder. Each list records
    how many times the primitive was invoked + its args."""
    send_calls: list[str]
    rm_calls: list[tuple[str, str]]
    hard_calls: list[int]
    alive_responses: list[bool]
    rm_responses: list[tuple[bool, str]]
    hard_responses: list[bool]

    def session_alive(self, session: str) -> bool:
        if self.alive_responses:
            return self.alive_responses.pop(0)
        return False

    def send_exit(self, session: str) -> bool:
        self.send_calls.append(session)
        return True

    def fleet_rm(self, fleet_bin: str, agent_id: str) -> tuple[bool, str]:
        self.rm_calls.append((fleet_bin, agent_id))
        if self.rm_responses:
            return self.rm_responses.pop(0)
        return True, ""

    def hard_kill(self, pid: int) -> bool:
        self.hard_calls.append(pid)
        if self.hard_responses:
            return self.hard_responses.pop(0)
        return True


def _stubs(
    *,
    alive: list[bool] | None = None,
    rm: list[tuple[bool, str]] | None = None,
    hard: list[bool] | None = None,
) -> _KillStubs:
    return _KillStubs(
        send_calls=[], rm_calls=[], hard_calls=[],
        alive_responses=list(alive or []),
        rm_responses=list(rm or []),
        hard_responses=list(hard or []),
    )


# ---------- judge_completion ----------


def test_judge_phase_done_with_pr_url_is_complete() -> None:
    j = reaper.judge_completion(
        task_status="in-progress",
        worker_state={"phase": "done", "pr_url": "https://x/y/pull/1"},
        is_git=True,
    )
    assert j == reaper.JUDGE_COMPLETE


def test_judge_phase_done_non_git_without_pr_is_complete() -> None:
    """Non-git projects' finishers write phase=done without a pr_url."""
    j = reaper.judge_completion(
        task_status="in-progress",
        worker_state={"phase": "done"},
        is_git=False,
    )
    assert j == reaper.JUDGE_COMPLETE


def test_judge_phase_done_git_without_pr_is_pending() -> None:
    """Git project + phase=done + no pr_url: worker didn't honor §7 —
    let the existing reconcile path handle it (requeue), not the reaper."""
    j = reaper.judge_completion(
        task_status="in-progress",
        worker_state={"phase": "done", "pr_url": ""},
        is_git=True,
    )
    assert j == reaper.JUDGE_PENDING


def test_judge_phase_failed_is_error_abort() -> None:
    j = reaper.judge_completion(
        task_status="in-progress",
        worker_state={"phase": "failed"},
        is_git=True,
    )
    assert j == reaper.JUDGE_ERROR_ABORT


def test_judge_phase_blocked_is_blocked_not_complete() -> None:
    """Operator-decision lifecycle: do not auto-reap."""
    j = reaper.judge_completion(
        task_status="in-progress",
        worker_state={"phase": "blocked"},
        is_git=True,
    )
    assert j == reaper.JUDGE_BLOCKED


def test_judge_non_terminal_phase_is_continue() -> None:
    j = reaper.judge_completion(
        task_status="in-progress",
        worker_state={"phase": "tdd-red"},
        is_git=True,
    )
    assert j == reaper.JUDGE_CONTINUE


def test_judge_handoff_phases_are_continue() -> None:
    """Three-stage handoff phases must not reap — the next stage picks
    up the slot."""
    assert reaper.judge_completion(
        task_status="in-progress",
        worker_state={"phase": "review-pending"}, is_git=True,
    ) == reaper.JUDGE_CONTINUE
    assert reaper.judge_completion(
        task_status="in-progress",
        worker_state={"phase": "review-done"}, is_git=True,
    ) == reaper.JUDGE_CONTINUE


def test_judge_missing_state_is_pending() -> None:
    j = reaper.judge_completion(
        task_status="in-progress",
        worker_state=None,
        is_git=True,
    )
    assert j == reaper.JUDGE_PENDING


# ---------- reap_one: directed → killed (within grace) ----------


def test_reaper_complete_judgment_kills_within_grace() -> None:
    """First pass with session alive: send /exit, record kill_directive_ts.
    Second pass (after grace): tmux session is gone → archive succeeds."""
    entry = reaper.ReaperEntry(slug="alpha-aaaa")
    inp = _inp(worker_state={
        "phase": "done", "pr_url": "https://x/y/pull/1",
    })
    # alive=[True]   — pass 1 first-pass probe (codex iter-3 [P2] fast
    #                  path: session alive → start grace timer).
    # alive=[False]  — pass 2 post-grace re-probe: session gone.
    stubs = _stubs(alive=[True, False])

    # Pass 1: probe alive → send /exit
    d1 = reaper.reap_one(
        inp, entry=entry, fleet_bin="fleet",
        now_unix=1000.0, grace_window_s=10,
        session_alive_fn=stubs.session_alive,
        send_exit_fn=stubs.send_exit,
        fleet_rm_fn=stubs.fleet_rm,
        hard_kill_fn=stubs.hard_kill,
    )
    assert d1.judgment == reaper.JUDGE_COMPLETE
    assert d1.state == "directed"
    assert entry.kill_directive_ts == pytest.approx(1000.0)
    assert stubs.send_calls == ["fleet-aaaaaaaa"]
    assert stubs.rm_calls == []

    # Pass 2: post-grace, session dead → archive
    d2 = reaper.reap_one(
        inp, entry=entry, fleet_bin="fleet",
        now_unix=1015.0, grace_window_s=10,
        session_alive_fn=stubs.session_alive,
        send_exit_fn=stubs.send_exit,
        fleet_rm_fn=stubs.fleet_rm,
        hard_kill_fn=stubs.hard_kill,
    )
    assert d2.state == "killed"
    assert len(stubs.rm_calls) == 1
    assert stubs.rm_calls[0] == ("fleet", "aaaaaaaa")


def test_reaper_complete_judgment_already_dead_session_archives_immediately() -> None:
    """Codex iter-3 [P2]: when the tmux session is ALREADY DEAD on the
    first reap pass (worker exited cleanly on its own after writing
    phase=done), the reaper skips the grace timer and archives the
    record on the same pass. Otherwise we'd wait grace_window_s for
    nothing."""
    entry = reaper.ReaperEntry(slug="alpha-aaaa")
    inp = _inp(worker_state={
        "phase": "done", "pr_url": "https://x/y/pull/1",
    })
    # First-pass probe: session already gone.
    stubs = _stubs(alive=[False])
    d = reaper.reap_one(
        inp, entry=entry, fleet_bin="fleet",
        now_unix=1000.0, grace_window_s=10,
        session_alive_fn=stubs.session_alive,
        send_exit_fn=stubs.send_exit,
        fleet_rm_fn=stubs.fleet_rm,
        hard_kill_fn=stubs.hard_kill,
    )
    # No grace timer set; archived in one pass.
    assert d.state == "killed"
    assert entry.kill_directive_ts == 0.0
    assert stubs.send_calls == []
    assert len(stubs.rm_calls) == 1


def test_reaper_within_grace_does_not_fleet_rm() -> None:
    """Within the grace window, reaper just records and waits."""
    entry = reaper.ReaperEntry(
        slug="alpha-aaaa", kill_directive_ts=1000.0,
    )
    inp = _inp(worker_state={
        "phase": "done", "pr_url": "https://x/y/pull/1",
    })
    stubs = _stubs()

    d = reaper.reap_one(
        inp, entry=entry, fleet_bin="fleet",
        now_unix=1005.0, grace_window_s=10,
        session_alive_fn=stubs.session_alive,
        send_exit_fn=stubs.send_exit,
        fleet_rm_fn=stubs.fleet_rm,
        hard_kill_fn=stubs.hard_kill,
    )
    assert d.state == "directed"
    assert stubs.rm_calls == []
    assert stubs.hard_calls == []


# ---------- reap_one: hard-kill escape hatch ----------


def test_reaper_kill_fails_session_alive_hard_kills_via_pid() -> None:
    """fleet rm fails, session STILL alive on re-probe → SIGKILL pid."""
    entry = reaper.ReaperEntry(
        slug="alpha-aaaa", kill_directive_ts=1000.0,
    )
    inp = _inp(worker_state={
        "phase": "done", "pr_url": "https://x/y/pull/1",
    }, pid=99999)
    # session_alive_fn called twice: once for the grace-expired probe
    # (alive=True), once for the post-rm re-probe (still alive=True →
    # hard-kill).
    stubs = _stubs(
        alive=[True, True],
        rm=[(False, "queue file present")],
    )
    log = io.StringIO()
    d = reaper.reap_one(
        inp, entry=entry, fleet_bin="fleet",
        now_unix=1015.0, grace_window_s=10,
        session_alive_fn=stubs.session_alive,
        send_exit_fn=stubs.send_exit,
        fleet_rm_fn=stubs.fleet_rm,
        hard_kill_fn=stubs.hard_kill,
        log_stream=log,
    )
    assert d.state == "hard-killed"
    assert d.hard_killed is True
    assert stubs.hard_calls == [99999]
    assert "[P0]" in log.getvalue()
    assert "hard-killed" in log.getvalue()
    assert entry.hard_killed is True


def test_reaper_kill_fails_session_dead_archives_idempotently() -> None:
    """fleet rm fails, session was already dead → still "killed" state."""
    entry = reaper.ReaperEntry(
        slug="alpha-aaaa", kill_directive_ts=1000.0,
    )
    inp = _inp(worker_state={
        "phase": "done", "pr_url": "https://x/y/pull/1",
    })
    # grace-expired probe: alive=True (so we enter the rm branch);
    # rm fails; re-probe: alive=False (rm raced with another reaper).
    stubs = _stubs(
        alive=[True, False],
        rm=[(False, "agent already archived")],
    )
    d = reaper.reap_one(
        inp, entry=entry, fleet_bin="fleet",
        now_unix=1015.0, grace_window_s=10,
        session_alive_fn=stubs.session_alive,
        send_exit_fn=stubs.send_exit,
        fleet_rm_fn=stubs.fleet_rm,
        hard_kill_fn=stubs.hard_kill,
    )
    assert d.state == "killed"
    assert stubs.hard_calls == []  # didn't escalate to SIGKILL


def test_reaper_no_address_returns_killed_passthrough() -> None:
    """No agent_id and no tmux_session → reaper is no-op (already gone),
    state=killed so reconcile can proceed."""
    entry = reaper.ReaperEntry(slug="alpha-aaaa")
    inp = _inp(agent_id="", tmux_session="", worker_state={
        "phase": "done", "pr_url": "https://x/y/pull/1",
    })
    stubs = _stubs()

    d = reaper.reap_one(
        inp, entry=entry, fleet_bin="fleet",
        now_unix=1000.0, grace_window_s=10,
        session_alive_fn=stubs.session_alive,
        send_exit_fn=stubs.send_exit,
        fleet_rm_fn=stubs.fleet_rm,
        hard_kill_fn=stubs.hard_kill,
    )
    assert d.state == "killed"
    assert stubs.send_calls == []
    assert stubs.rm_calls == []


# ---------- reap_one: disabled ----------


def test_reaper_disabled_env_short_circuits(monkeypatch) -> None:
    monkeypatch.setenv("FLEET_COORD_REAPER_DISABLED", "1")
    entry = reaper.ReaperEntry(slug="alpha-aaaa")
    inp = _inp(worker_state={
        "phase": "done", "pr_url": "https://x/y/pull/1",
    })
    stubs = _stubs()
    d = reaper.reap_one(
        inp, entry=entry, fleet_bin="fleet",
        now_unix=1000.0, grace_window_s=10,
        session_alive_fn=stubs.session_alive,
        send_exit_fn=stubs.send_exit,
        fleet_rm_fn=stubs.fleet_rm,
        hard_kill_fn=stubs.hard_kill,
    )
    assert d.state == "no-op"
    assert stubs.send_calls == []
    assert stubs.rm_calls == []


# ---------- reap_probes (multi-probe + state persistence) ----------


def test_reap_probes_persists_state_and_drops_on_kill() -> None:
    """After a successful kill, the slug's entry is removed from the
    coord-state reaper sub-dict — re-dispatch on the same slug starts
    with a fresh ledger."""
    coord_state: dict = {}
    inputs = [_inp(worker_state={
        "phase": "done", "pr_url": "https://x/y/pull/1",
    })]
    # First-pass probe: alive (so grace timer fires);
    # second-pass probe: dead (grace expired).
    stubs = _stubs(alive=[True, False])

    # Pass 1: session alive → send /exit; entry is opened.
    decisions = reaper.reap_probes(
        probes_inputs=inputs, coord_state=coord_state,
        fleet_bin="fleet", now_unix=1000.0, grace_window_s=10,
        session_alive_fn=stubs.session_alive,
        send_exit_fn=stubs.send_exit,
        fleet_rm_fn=stubs.fleet_rm,
        hard_kill_fn=stubs.hard_kill,
    )
    assert decisions[0].state == "directed"
    assert "alpha-aaaa" in coord_state.get("reaper", {})

    # Pass 2: grace expired, session gone → killed, entry dropped.
    decisions = reaper.reap_probes(
        probes_inputs=inputs, coord_state=coord_state,
        fleet_bin="fleet", now_unix=1015.0, grace_window_s=10,
        session_alive_fn=stubs.session_alive,
        send_exit_fn=stubs.send_exit,
        fleet_rm_fn=stubs.fleet_rm,
        hard_kill_fn=stubs.hard_kill,
    )
    assert decisions[0].state == "killed"
    assert "alpha-aaaa" not in coord_state.get("reaper", {})


def test_error_abort_judgment_kills_and_flags_redispatch() -> None:
    """phase=failed → reaper kills AND flags slug for re-dispatch.

    Two-pass scenario: first pass session alive → /exit + grace timer;
    second pass session dead → archive succeeds + redispatch flagged.
    """
    coord_state: dict = {}
    inputs = [_inp(worker_state={"phase": "failed"})]
    # First-pass probe: alive (start grace); second-pass: dead.
    stubs = _stubs(alive=[True, False])

    # Pass 1: /exit sent, entry opened.
    decisions = reaper.reap_probes(
        probes_inputs=inputs, coord_state=coord_state,
        fleet_bin="fleet", now_unix=1000.0, grace_window_s=10,
        session_alive_fn=stubs.session_alive,
        send_exit_fn=stubs.send_exit,
        fleet_rm_fn=stubs.fleet_rm,
        hard_kill_fn=stubs.hard_kill,
    )
    assert decisions[0].judgment == reaper.JUDGE_ERROR_ABORT
    assert decisions[0].state == "directed"

    # Pass 2: kill succeeds, redispatch flagged.
    decisions = reaper.reap_probes(
        probes_inputs=inputs, coord_state=coord_state,
        fleet_bin="fleet", now_unix=1015.0, grace_window_s=10,
        session_alive_fn=stubs.session_alive,
        send_exit_fn=stubs.send_exit,
        fleet_rm_fn=stubs.fleet_rm,
        hard_kill_fn=stubs.hard_kill,
    )
    assert decisions[0].state == "killed"
    pending = reaper.load_redispatch_pending(coord_state)
    assert "alpha-aaaa" in pending


def test_error_abort_judgment_immediate_kill_when_session_dead() -> None:
    """Codex iter-3 [P2]: phase=failed worker that already exited (rare
    but possible — worker crashed before /exit could land) gets archived
    in one pass + redispatch flagged. No grace wait."""
    coord_state: dict = {}
    inputs = [_inp(worker_state={"phase": "failed"})]
    stubs = _stubs(alive=[False])  # first-pass probe: already dead
    decisions = reaper.reap_probes(
        probes_inputs=inputs, coord_state=coord_state,
        fleet_bin="fleet", now_unix=1000.0, grace_window_s=10,
        session_alive_fn=stubs.session_alive,
        send_exit_fn=stubs.send_exit,
        fleet_rm_fn=stubs.fleet_rm,
        hard_kill_fn=stubs.hard_kill,
    )
    assert decisions[0].judgment == reaper.JUDGE_ERROR_ABORT
    assert decisions[0].state == "killed"
    assert stubs.send_calls == []  # no /exit sent on dead session
    pending = reaper.load_redispatch_pending(coord_state)
    assert "alpha-aaaa" in pending


def test_reap_probes_pending_judgment_no_entry_kept() -> None:
    """Worker still running (phase=tdd-red) → no entry opened."""
    coord_state: dict = {}
    inputs = [_inp(worker_state={"phase": "tdd-red"})]
    stubs = _stubs()

    decisions = reaper.reap_probes(
        probes_inputs=inputs, coord_state=coord_state,
        fleet_bin="fleet", now_unix=1000.0, grace_window_s=10,
        session_alive_fn=stubs.session_alive,
        send_exit_fn=stubs.send_exit,
        fleet_rm_fn=stubs.fleet_rm,
        hard_kill_fn=stubs.hard_kill,
    )
    assert decisions[0].state == "pending"
    assert decisions[0].judgment == reaper.JUDGE_CONTINUE
    assert "reaper" not in coord_state or "alpha-aaaa" not in coord_state.get(
        "reaper", {}
    )


# ---------- hard_kill_pid primitive ----------


def test_hard_kill_pid_returns_false_on_process_lookup_error(monkeypatch) -> None:
    """Codex iter-4 [P1]: ProcessLookupError must return False, NOT True.
    The caller invokes hard_kill_pid AFTER confirming the tmux session
    is still alive — if the pid is missing but the session lives, we
    didn't actually kill anything; reporting True would falsely close
    the kill cycle and orphan the session."""
    import os as _os

    def boom(pid, sig):
        raise ProcessLookupError("no such process")

    monkeypatch.setattr(_os, "kill", boom)
    assert reaper.hard_kill_pid(12345) is False


def test_hard_kill_pid_returns_true_on_signal_delivery(monkeypatch) -> None:
    """Happy path: os.kill returns normally → True."""
    import os as _os
    called: list[tuple[int, int]] = []

    def fake_kill(pid, sig):
        called.append((pid, sig))

    monkeypatch.setattr(_os, "kill", fake_kill)
    assert reaper.hard_kill_pid(12345) is True
    assert called == [(12345, signal.SIGKILL)] or called[0][0] == 12345


def test_hard_kill_pid_returns_false_on_permission_error(monkeypatch) -> None:
    """Cross-uid kill → False so the caller can escalate."""
    import os as _os

    def boom(pid, sig):
        raise PermissionError("not yours")

    monkeypatch.setattr(_os, "kill", boom)
    assert reaper.hard_kill_pid(12345) is False


def test_hard_kill_pid_rejects_zero_pid() -> None:
    """Defensive: pid<=0 → False."""
    assert reaper.hard_kill_pid(0) is False
    assert reaper.hard_kill_pid(-1) is False


# ---------- reaper_lane_clear gate ----------


def test_reaper_lane_clear_after_killed_entry_dropped() -> None:
    """Lane is clear when the reaper has no open entry for the slug."""
    coord_state: dict = {}
    assert reaper.reaper_lane_clear(coord_state, "alpha-aaaa") is True
    coord_state = {
        "reaper": {
            "alpha-aaaa": {"kill_directive_ts": 1000.0, "hard_killed": False},
        }
    }
    assert reaper.reaper_lane_clear(coord_state, "alpha-aaaa") is False
    # Drop the entry → lane clear again.
    del coord_state["reaper"]["alpha-aaaa"]
    assert reaper.reaper_lane_clear(coord_state, "alpha-aaaa") is True


# ---------- forget + redispatch helpers ----------


def test_forget_reaper_entry_idempotent() -> None:
    coord_state = {
        "reaper": {"alpha-aaaa": {"kill_directive_ts": 1.0}}
    }
    reaper.forget_reaper_entry(coord_state, "alpha-aaaa")
    assert coord_state["reaper"] == {}
    reaper.forget_reaper_entry(coord_state, "alpha-aaaa")
    # Idempotent — no exception.


def test_add_clear_redispatch_pending_roundtrip() -> None:
    coord_state: dict = {}
    reaper.add_redispatch_pending(coord_state, "alpha-aaaa")
    reaper.add_redispatch_pending(coord_state, "beta-bbbb")
    # Duplicates squashed.
    reaper.add_redispatch_pending(coord_state, "alpha-aaaa")
    assert reaper.load_redispatch_pending(coord_state) == {
        "alpha-aaaa", "beta-bbbb",
    }
    reaper.clear_redispatch_pending(coord_state, "alpha-aaaa")
    assert reaper.load_redispatch_pending(coord_state) == {"beta-bbbb"}


# ---------- ReaperEntry round-trip ----------


def test_reaper_entry_round_trip_through_dict() -> None:
    e = reaper.ReaperEntry(
        slug="alpha", kill_directive_ts=42.5,
        hard_killed=True, judgment=reaper.JUDGE_ERROR_ABORT,
    )
    d = e.to_dict()
    assert d["kill_directive_ts"] == 42.5
    assert d["hard_killed"] is True
    e2 = reaper.ReaperEntry.from_dict("alpha", d)
    assert e2 == e
