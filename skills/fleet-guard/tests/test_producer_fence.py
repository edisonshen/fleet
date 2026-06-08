"""fleet-guard producer fence + storm back-off — DESIGN-handoff-drain-
storm-leak PR4 (item 10) and the PR5-deferred lease-coupled back-off.

Two layers guard the handoff producer (_do_handoff):

  (a) FENCE (correctness): a FENCED old coord (a successor stole its
      parent's lease) must NOT write a handoff doc / enqueue — that is a
      zombie producer write. _producer_fenced proves parent-lease ownership
      via `fleet lease-check`; a fenced producer returns False from
      _do_handoff (refused), and the caller clears the pending mark.

  (b) BACK-OFF (storm suppression): even when validly the leader, do NOT
      re-fire while a handoff is already in flight (the queue file for this
      agent already exists). This kills the 16-docs-zero-successors storm.
"""
from __future__ import annotations

import json
import subprocess
from pathlib import Path
from typing import Any

import pytest

import handoff

# The autouse conftest fixture stubs handoff._producer_fenced to "not
# fenced" for unrelated tests. The exit-code-mapping tests below exercise
# the REAL implementation, so capture it at import time (before any
# monkeypatch) and call it directly.
_REAL_PRODUCER_FENCED = handoff._producer_fenced


@pytest.fixture
def fleet_home_tmp(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> Path:
    home = tmp_path / "fleet"
    home.mkdir()
    (home / "queue").mkdir()
    (home / "handoffs").mkdir()
    monkeypatch.setenv("FLEET_HOME", str(home))
    return home


def _record(agent_id: str = "abc12345", project: str = "myproj") -> dict[str, Any]:
    return {
        "id": agent_id, "task_id": "demo-task", "project": project,
        "handoff_number": 1, "last_handoff_path": None,
    }


def _stub_collectors(monkeypatch: pytest.MonkeyPatch) -> None:
    """Neuter the slow/external side-collectors so _do_handoff's WRITE
    behavior is what's under test, not pane capture or gh."""
    monkeypatch.setattr(handoff, "capture_recent", lambda *_a, **_k: "recent")
    monkeypatch.setattr(handoff, "_collect_active_subagents", lambda *_a, **_k: [])
    monkeypatch.setattr(handoff, "_collect_open_prs", lambda *_a, **_k: [])


# ---------- (a) fence ----------


def test_fenced_producer_refuses_to_write(
    fleet_home_tmp: Path, monkeypatch: pytest.MonkeyPatch,
) -> None:
    _stub_collectors(monkeypatch)
    monkeypatch.setattr(handoff, "_producer_fenced", lambda _p: True)
    # Sentinel: prove write_doc is never reached.
    monkeypatch.setattr(handoff, "write_doc", lambda **_k: pytest.fail(
        "a fenced producer must NOT write a handoff doc"))

    ok = handoff._do_handoff(_record(), "fleet-abc12345", handoff.TYPE_AUTO_YELLOW, 55.0)
    assert ok is False, "a fenced producer must return False (refused)"
    # No queue file written either.
    assert list((fleet_home_tmp / "queue").iterdir()) == []


def test_unfenced_producer_proceeds(
    fleet_home_tmp: Path, monkeypatch: pytest.MonkeyPatch,
) -> None:
    _stub_collectors(monkeypatch)
    monkeypatch.setattr(handoff, "_producer_fenced", lambda _p: False)
    wrote = {}
    monkeypatch.setattr(handoff, "write_doc",
                        lambda **k: wrote.setdefault("doc", "/tmp/doc.md") or "/tmp/doc.md")
    monkeypatch.setattr(handoff, "write_queue", lambda **k: True)

    ok = handoff._do_handoff(_record(), "fleet-abc12345", handoff.TYPE_AUTO_YELLOW, 55.0)
    assert ok is True
    assert wrote.get("doc") == "/tmp/doc.md", "an un-fenced producer must write the doc"


# ---------- (b) back-off ----------


def test_backoff_when_handoff_in_flight(
    fleet_home_tmp: Path, monkeypatch: pytest.MonkeyPatch,
) -> None:
    _stub_collectors(monkeypatch)
    monkeypatch.setattr(handoff, "_producer_fenced", lambda _p: False)
    # A queue file already enqueued for this agent -> a successor is live.
    qf = fleet_home_tmp / "queue" / "spawn-fresh-abc12345.json"
    qf.write_text(json.dumps({"old_agent_id": "abc12345"}))
    monkeypatch.setattr(handoff, "write_doc", lambda **_k: pytest.fail(
        "must NOT write doc #2 while a handoff is already in flight (storm)"))

    ok = handoff._do_handoff(_record(), "fleet-abc12345", handoff.TYPE_AUTO_YELLOW, 55.0)
    assert ok is True, "back-off treats the in-flight handoff as success (no rollback)"
    # Only the pre-existing queue file remains; no second one.
    assert [p.name for p in (fleet_home_tmp / "queue").iterdir()] == \
        ["spawn-fresh-abc12345.json"]


def test_no_backoff_when_no_queue_file(
    fleet_home_tmp: Path, monkeypatch: pytest.MonkeyPatch,
) -> None:
    _stub_collectors(monkeypatch)
    monkeypatch.setattr(handoff, "_producer_fenced", lambda _p: False)
    wrote = {}
    monkeypatch.setattr(handoff, "write_doc",
                        lambda **k: wrote.setdefault("doc", "/d") or "/d")
    monkeypatch.setattr(handoff, "write_queue", lambda **k: True)
    ok = handoff._do_handoff(_record(), "fleet-abc12345", handoff.TYPE_AUTO_YELLOW, 55.0)
    assert ok is True
    assert wrote.get("doc") == "/d", "no in-flight handoff -> must write the first doc"


# ---------- _producer_fenced exit-code mapping ----------


def _completed(rc: int):
    return subprocess.CompletedProcess(["fleet"], rc, stdout="", stderr="")


def test_producer_fenced_maps_exit_3_only(
    fleet_home_tmp: Path, monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv("FLEET_LEASE_FAILOVER", "1")
    # exit 3 => fenced; everything else => not fenced (fail-open).
    for rc, want in {0: False, 3: True, 1: False, 2: False}.items():
        monkeypatch.setattr(handoff.subprocess, "run",
                            lambda *a, _rc=rc, **k: _completed(_rc))
        assert _REAL_PRODUCER_FENCED("myproj") is want, f"exit {rc}"


def test_producer_fenced_failover_off_is_noop(
    fleet_home_tmp: Path, monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv("FLEET_LEASE_FAILOVER", "0")
    called = []
    monkeypatch.setattr(handoff.subprocess, "run",
                        lambda *a, **k: called.append(1) or _completed(3))
    assert _REAL_PRODUCER_FENCED("myproj") is False
    assert called == [], "failover off must not shell out"


def test_producer_fenced_binary_missing_fail_open(
    fleet_home_tmp: Path, monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv("FLEET_LEASE_FAILOVER", "1")

    def _boom(*a, **k):
        raise FileNotFoundError("no fleet")

    monkeypatch.setattr(handoff.subprocess, "run", _boom)
    assert _REAL_PRODUCER_FENCED("myproj") is False, "missing binary is fail-open"
