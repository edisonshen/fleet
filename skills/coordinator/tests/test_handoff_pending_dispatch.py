"""Soft-handoff quiescence: while fleet-guard has marked the coord's own
agent record with a pending `handoff_type`, the tick must NOT open new
worker subagents (they are Agent-tool children of a coord that is about
to be replaced) — but everything that lets in-flight work finish
(reconcile, sentinels, reviewer/finisher handoffs, replay) keeps running
so `worker_agent_ids` drains and fleet-guard can commit the handoff.
"""
from __future__ import annotations

import json
from pathlib import Path
from unittest.mock import patch

import pytest

import loop
from test_single_shot_tick import (  # noqa: F401 — fixtures
    _make_task,
    _write_tasks,
    dispatch_subprocess,
    fleet_home,
    fleet_run_recorder,
    project_dir,
)


def _seed_coord_record(fleet_home: Path, coord_id: str, **fields) -> Path:
    d = fleet_home / "agents"
    d.mkdir(exist_ok=True)
    p = d / f"{coord_id}.json"
    p.write_text(json.dumps({
        "id": coord_id, "project": "fleet", "task_id": "coord-fleet",
        "handoff_type": None, **fields,
    }), encoding="utf-8")
    return p


@pytest.mark.parametrize("handoff_type", ["auto-yellow", "auto-red", "precompact"])
def test_pending_handoff_suppresses_new_worker_dispatch(
    fleet_home: Path, project_dir: Path, fleet_run_recorder,
    dispatch_subprocess, handoff_type,
) -> None:
    _seed_coord_record(fleet_home, "cccccc01", handoff_type=handoff_type)
    _write_tasks(project_dir, [_make_task("ready-eeee", status="ready")])
    dispatch_subprocess.append("abcdef01")

    result = loop.tick(
        "fleet", coord_id="cccccc01", cwd="/repo",
        fleet_home=str(fleet_home), cap=1,
    )

    assert not result.skipped
    assert result.dispatched == 0
    assert result.dispatch_instructions == []
    assert not (fleet_home / "inbox" / "abcdef01.md").exists()
    set_calls = [c for c in fleet_run_recorder if c[1:3] == ["tasks", "set"]]
    assert not any("status=in-progress" in c for c in set_calls)
    assert any("handoff pending" in e for e in result.errors)
    # The ready task is untouched: the successor coord dispatches it.
    state = json.loads((project_dir / "coord-state.json").read_text())
    assert not state.get("worker_agent_ids")


def test_no_pending_handoff_dispatches_normally(
    fleet_home: Path, project_dir: Path, fleet_run_recorder,
    dispatch_subprocess,
) -> None:
    _seed_coord_record(fleet_home, "cccccc01")
    _write_tasks(project_dir, [_make_task("ready-eeee", status="ready")])
    dispatch_subprocess.append("abcdef01")

    result = loop.tick(
        "fleet", coord_id="cccccc01", cwd="/repo",
        fleet_home=str(fleet_home), cap=1,
    )

    assert result.dispatched == 1
    assert result.dispatch_instructions[0].startswith("DISPATCH: ready-eeee")
    assert not any("handoff pending" in e for e in result.errors)


def test_pending_handoff_still_reconciles_dead_worker(
    fleet_home: Path, project_dir: Path, fleet_run_recorder,
    dispatch_subprocess,
) -> None:
    """The soft handoff waits for worker_agent_ids to drain — so the
    reconcile that clears a finished worker's mapping must keep running
    while dispatch is paused, or the wait could never end."""
    _seed_coord_record(fleet_home, "cccccc01", handoff_type="auto-yellow")
    (project_dir / "coord-state.json").write_text(json.dumps({
        "worker_agent_ids": {"dying-ffff": "beefcafe"},
    }), encoding="utf-8")
    _write_tasks(project_dir, [
        _make_task("dying-ffff", status="in-progress", worker_pid=1),
        _make_task("ready-eeee", status="ready"),
    ])

    with patch.object(loop, "_pid_alive", return_value=False):
        result = loop.tick(
            "fleet", coord_id="cccccc01", cwd="/repo",
            fleet_home=str(fleet_home), cap=2,
        )

    assert result.reconciled == 1
    assert result.dispatched == 0
    state = json.loads((project_dir / "coord-state.json").read_text())
    assert "dying-ffff" not in (state.get("worker_agent_ids") or {})


def test_coord_handoff_pending_helper(fleet_home: Path) -> None:
    assert loop._coord_handoff_pending("", home=fleet_home) == ""
    assert loop._coord_handoff_pending("missing0", home=fleet_home) == ""
    p = _seed_coord_record(fleet_home, "cccccc02")
    assert loop._coord_handoff_pending("cccccc02", home=fleet_home) == ""
    _seed_coord_record(fleet_home, "cccccc02", handoff_type="manual")
    assert loop._coord_handoff_pending("cccccc02", home=fleet_home) == ""
    _seed_coord_record(fleet_home, "cccccc02", handoff_type="auto-yellow")
    assert loop._coord_handoff_pending("cccccc02", home=fleet_home) == "auto-yellow"
    p.write_text("{garbage", encoding="utf-8")
    assert loop._coord_handoff_pending("cccccc02", home=fleet_home) == ""
