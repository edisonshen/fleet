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


def _record(agent_id: str = "abc12345", project: str = "myproj",
            task_id: str = "coord-myproj") -> dict[str, Any]:
    return {
        "id": agent_id, "task_id": task_id, "project": project,
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


# codex PR4 [P1]: the fence applies to COORD producers ONLY. A worker pane
# is not a descendant of the coord-run supervisor, so lease-check would
# return exit 3 against the coord lease — but a worker doesn't hold that
# lease and must NOT be fenced by it (that would drop the worker's handoff).
def test_worker_handoff_not_fenced_by_coord_lease(
    fleet_home_tmp: Path, monkeypatch: pytest.MonkeyPatch,
) -> None:
    _stub_collectors(monkeypatch)
    # _producer_fenced would say True (a worker fails the coord ancestry
    # proof) — but a WORKER record (task_id != coord-*) must skip the fence.
    fenced_calls = []
    monkeypatch.setattr(handoff, "_producer_fenced",
                        lambda p: fenced_calls.append(p) or True)
    wrote = {}
    monkeypatch.setattr(handoff, "write_doc",
                        lambda **k: wrote.setdefault("doc", "/w") or "/w")
    monkeypatch.setattr(handoff, "write_queue", lambda **k: True)

    worker = _record(agent_id="w0000001", task_id="some-worker-task")
    ok = handoff._do_handoff(worker, "fleet-w0000001", handoff.TYPE_AUTO_YELLOW, 55.0)
    assert ok is True, "a worker handoff must NOT be refused by the coord lease fence"
    assert wrote.get("doc") == "/w", "worker handoff must write its doc"
    assert fenced_calls == [], "the coord fence must not even be consulted for a worker"


# codex PR4 [P2]: a worker whose slug merely STARTS WITH "coord-" but is not
# the exact "coord-<project>" must NOT be treated as the coord (a prefix
# match would wrongly fence it).
def test_coord_prefix_worker_not_treated_as_coord(
    fleet_home_tmp: Path, monkeypatch: pytest.MonkeyPatch,
) -> None:
    _stub_collectors(monkeypatch)
    fenced_calls = []
    monkeypatch.setattr(handoff, "_producer_fenced",
                        lambda p: fenced_calls.append(p) or True)
    monkeypatch.setattr(handoff, "write_doc", lambda **k: "/w")
    monkeypatch.setattr(handoff, "write_queue", lambda **k: True)
    # task_id "coord-helper" with project "myproj" -> NOT "coord-myproj".
    worker = _record(agent_id="w0000002", task_id="coord-helper", project="myproj")
    ok = handoff._do_handoff(worker, "fleet-w0000002", handoff.TYPE_AUTO_YELLOW, 55.0)
    assert ok is True
    assert fenced_calls == [], "a 'coord-'-prefixed worker must not hit the coord fence"


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


def _completed_err(rc: int, stderr: str):
    return subprocess.CompletedProcess(["fleet"], rc, stdout="", stderr=stderr)


def test_producer_fenced_exit_code_mapping(
    fleet_home_tmp: Path, monkeypatch: pytest.MonkeyPatch,
) -> None:
    # exit 0 -> not fenced; exit 3 -> fenced; exit 1/2 INTERNAL error ->
    # FENCED (cannot prove ownership, codex PR4 [P1]).
    for rc, want in {0: False, 3: True, 1: True, 2: True}.items():
        monkeypatch.setattr(handoff.subprocess, "run",
                            lambda *a, _rc=rc, **k: _completed_err(_rc, "internal boom"))
        assert _REAL_PRODUCER_FENCED("myproj") is want, f"exit {rc}"


def test_producer_fenced_too_old_binary_fails_open(
    fleet_home_tmp: Path, monkeypatch: pytest.MonkeyPatch,
) -> None:
    # too-old binary -> "unknown command" + exit 1 -> fail OPEN (not fenced).
    monkeypatch.setattr(handoff.subprocess, "run",
                        lambda *a, **k: _completed_err(1, 'unknown command "lease-check"'))
    assert _REAL_PRODUCER_FENCED("myproj") is False, "too-old binary must fail open"


def test_producer_fenced_without_project_is_noop(
    fleet_home_tmp: Path, monkeypatch: pytest.MonkeyPatch,
) -> None:
    called = []
    monkeypatch.setattr(handoff.subprocess, "run",
                        lambda *a, **k: called.append(1) or _completed(3))
    assert _REAL_PRODUCER_FENCED("") is False
    assert called == [], "missing project must not shell out"


def test_producer_fenced_binary_missing_fail_open(
    fleet_home_tmp: Path, monkeypatch: pytest.MonkeyPatch,
) -> None:

    def _boom(*a, **k):
        raise FileNotFoundError("no fleet")

    monkeypatch.setattr(handoff.subprocess, "run", _boom)
    assert _REAL_PRODUCER_FENCED("myproj") is False, "missing binary is fail-open"


def test_producer_fenced_oserror_fail_open(
    fleet_home_tmp: Path, monkeypatch: pytest.MonkeyPatch,
) -> None:
    # A non-executable FLEET_BIN raises OSError (PermissionError) -> fail open
    # (codex PR4 [P2]); must NOT escape _producer_fenced.

    def _perm(*a, **k):
        raise PermissionError("not executable")

    monkeypatch.setattr(handoff.subprocess, "run", _perm)
    assert _REAL_PRODUCER_FENCED("myproj") is False, "OSError must fail open"
