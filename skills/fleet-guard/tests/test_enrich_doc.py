"""Auto-handoff narrative enrichment.

The auto path (`_do_handoff`) renders the doc with Placeholder in every
narrative section except Completed (pane capture). A coord that hands off
while WAITING on an e2e worker therefore used to leave its successor with
nothing actionable. `_enrich_doc` shells `fleet handoff-enrich <doc>` after
`write_doc` and before `write_queue` so the Go collectors fill Key
Decisions / Docs / Open Questions / Next Steps from coord-state.json,
tasks.md and the rolling checkpoint — the same brief `fleet handoff <id>`
produces. Coord docs only; best-effort.
"""
from __future__ import annotations

import subprocess
from pathlib import Path
from typing import Any

import pytest

import handoff

_REAL_ENRICH_DOC = handoff._enrich_doc


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
    monkeypatch.setattr(handoff, "capture_recent", lambda *_a, **_k: "recent")
    monkeypatch.setattr(handoff, "_collect_active_subagents", lambda *_a, **_k: [])
    monkeypatch.setattr(handoff, "_collect_open_prs", lambda *_a, **_k: [])


# ---------- wiring inside _do_handoff ----------


def test_coord_handoff_enriches_doc_before_queue(
    fleet_home_tmp: Path, monkeypatch: pytest.MonkeyPatch,
) -> None:
    _stub_collectors(monkeypatch)
    order: list[str] = []
    monkeypatch.setattr(handoff, "write_doc", lambda **_k: "/tmp/doc.md")
    monkeypatch.setattr(handoff, "_enrich_doc",
                        lambda p: order.append(f"enrich:{p}") or True)
    monkeypatch.setattr(handoff, "write_queue",
                        lambda **_k: order.append("queue") or True)

    ok = handoff._do_handoff(_record(), "fleet-abc12345", handoff.TYPE_AUTO_YELLOW, 55.0)
    assert ok is True
    assert order == ["enrich:/tmp/doc.md", "queue"], (
        "the doc must be enriched BEFORE the queue file makes it visible to drain")


def test_worker_handoff_skips_enrichment(
    fleet_home_tmp: Path, monkeypatch: pytest.MonkeyPatch,
) -> None:
    _stub_collectors(monkeypatch)
    monkeypatch.setattr(handoff, "write_doc", lambda **_k: "/tmp/doc.md")
    monkeypatch.setattr(handoff, "_enrich_doc", lambda _p: pytest.fail(
        "worker handoffs carry no coord narrative; must not enrich"))
    monkeypatch.setattr(handoff, "write_queue", lambda **_k: True)

    worker = _record(agent_id="w0000001", task_id="fix-thing-1234")
    ok = handoff._do_handoff(worker, "fleet-w0000001", handoff.TYPE_AUTO_YELLOW, 55.0)
    assert ok is True


def test_enrich_failure_does_not_block_handoff(
    fleet_home_tmp: Path, monkeypatch: pytest.MonkeyPatch,
) -> None:
    _stub_collectors(monkeypatch)
    monkeypatch.setattr(handoff, "write_doc", lambda **_k: "/tmp/doc.md")
    monkeypatch.setattr(handoff, "_enrich_doc", lambda _p: False)
    queued = {}
    monkeypatch.setattr(handoff, "write_queue",
                        lambda **k: queued.setdefault("doc_path", k["doc_path"]) or True)

    ok = handoff._do_handoff(_record(), "fleet-abc12345", handoff.TYPE_AUTO_YELLOW, 55.0)
    assert ok is True
    assert queued["doc_path"] == "/tmp/doc.md"


# ---------- the real _enrich_doc subprocess contract ----------


class _Proc:
    def __init__(self, rc: int, stderr: str = "") -> None:
        self.returncode = rc
        self.stderr = stderr
        self.stdout = ""


def test_enrich_doc_invokes_fleet_handoff_enrich(
    fleet_home_tmp: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch,
) -> None:
    fake_bin = tmp_path / "fleet-bin"
    fake_bin.write_text("#!/bin/sh\nexit 0\n")
    fake_bin.chmod(0o755)
    monkeypatch.setenv("FLEET_BIN", str(fake_bin))
    seen: dict[str, Any] = {}

    def fake_run(argv: list[str], **kw: Any) -> _Proc:
        seen["argv"] = argv
        seen["env"] = kw.get("env")
        seen["timeout"] = kw.get("timeout")
        return _Proc(0)

    monkeypatch.setattr(handoff.subprocess, "run", fake_run)
    assert _REAL_ENRICH_DOC("/x/doc.md") is True
    assert seen["argv"] == [str(fake_bin), "handoff-enrich", "/x/doc.md"]
    assert seen["env"]["FLEET_HOME"] == str(fleet_home_tmp)
    assert seen["timeout"] == handoff._ENRICH_TIMEOUT_S


def test_enrich_doc_nonzero_exit_is_false(
    fleet_home_tmp: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch,
    capsys: pytest.CaptureFixture[str],
) -> None:
    fake_bin = tmp_path / "fleet-bin"
    fake_bin.write_text("#!/bin/sh\nexit 1\n")
    fake_bin.chmod(0o755)
    monkeypatch.setenv("FLEET_BIN", str(fake_bin))
    monkeypatch.setattr(handoff.subprocess, "run",
                        lambda *_a, **_k: _Proc(1, "handoff-enrich: read: boom"))
    assert _REAL_ENRICH_DOC("/x/doc.md") is False
    assert "handoff-enrich" in capsys.readouterr().err


def test_enrich_doc_too_old_binary_is_quiet(
    fleet_home_tmp: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch,
    capsys: pytest.CaptureFixture[str],
) -> None:
    fake_bin = tmp_path / "fleet-bin"
    fake_bin.write_text("#!/bin/sh\nexit 1\n")
    fake_bin.chmod(0o755)
    monkeypatch.setenv("FLEET_BIN", str(fake_bin))
    monkeypatch.setattr(handoff.subprocess, "run",
                        lambda *_a, **_k: _Proc(1, 'Error: unknown command "handoff-enrich"'))
    assert _REAL_ENRICH_DOC("/x/doc.md") is False
    assert capsys.readouterr().err == ""


def test_enrich_doc_timeout_and_missing_binary_are_false(
    fleet_home_tmp: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch,
) -> None:
    fake_bin = tmp_path / "fleet-bin"
    fake_bin.write_text("#!/bin/sh\nexit 0\n")
    fake_bin.chmod(0o755)
    monkeypatch.setenv("FLEET_BIN", str(fake_bin))

    def boom(*_a: Any, **_k: Any) -> Any:
        raise subprocess.TimeoutExpired(cmd="fleet", timeout=1)

    monkeypatch.setattr(handoff.subprocess, "run", boom)
    assert _REAL_ENRICH_DOC("/x/doc.md") is False

    # No FLEET_BIN and nothing on PATH → quiet False, no subprocess.
    monkeypatch.delenv("FLEET_BIN")
    monkeypatch.setattr(handoff.shutil, "which", lambda _n: None)
    monkeypatch.setattr(handoff.subprocess, "run",
                        lambda *_a, **_k: pytest.fail("must not spawn without a binary"))
    assert _REAL_ENRICH_DOC("/x/doc.md") is False
