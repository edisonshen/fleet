"""Tests for skills/fleet-guard/main.py — the hook entry point.

The dispatch logic is thin (parse stdin → branch on hook_event_name →
delegate), so these tests focus on the wiring contract: which sub-callers
fire on which hook, what injection text concatenation looks like, and the
never-block-the-host failure mode.
"""
from __future__ import annotations

import io
import json
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

import pytest

import main as fleet_main


@pytest.fixture(autouse=True)
def fleet_home_tmp(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> Path:
    home = tmp_path / "fleet"
    monkeypatch.setenv("FLEET_HOME", str(home))
    monkeypatch.setenv("FLEET_AGENT_ID", "agent7777")
    return home


def _seed_record(home: Path, **overrides: Any) -> Path:
    record_dir = home / "agents"
    record_dir.mkdir(parents=True, exist_ok=True)
    base = {
        "schema_version": 1,
        "id": "agent7777",
        "pid": 1,
        "tmux_session": "fleet-agent7777",
        "engine": "claude-code",
        "role": "executor",
        "mode": "execute",
        "task_id": "demo-task",
        "project": "myproj",
        "review_round": None,
        "context_pct": None,
        "context_source": "",
        "last_activity_ts": "2026-04-28T00:00:00Z",
        "blocked": False,
        "blocked_reason": None,
        "blocked_since": None,
        "needs_input": False,
        "inbox_pending": False,
        "handoff_type": None,
        "last_handoff_path": None,
        "handoff_number": 1,
        "cwd": "/x",
        "command": ["claude"],
        "spawned_at": "2026-04-28T00:00:00Z",
    }
    base.update(overrides)
    path = record_dir / "agent7777.json"
    path.write_text(json.dumps(base, indent=2) + "\n", encoding="utf-8")
    return path


def _transcript(tmp_path: Path, *, input_tokens: int = 10_000) -> Path:
    path = tmp_path / "transcript.jsonl"
    path.write_text(json.dumps({
        "type": "assistant",
        "message": {
            "model": "claude-sonnet-4-6",
            "usage": {"input_tokens": input_tokens},
        },
    }) + "\n", encoding="utf-8")
    return path


def _run(payload: dict | str, capsys: pytest.CaptureFixture) -> tuple[int, str, str]:
    """Invoke main() with payload as stdin. Returns (rc, stdout, stderr)."""
    raw = payload if isinstance(payload, str) else json.dumps(payload)
    rc = fleet_main.main(stdin=io.StringIO(raw))
    captured = capsys.readouterr()
    return rc, captured.out, captured.err


# -- exit-silently paths -----------------------------------------------------

class TestExitSilently:
    def test_no_agent_id_env_no_writes(
        self, fleet_home_tmp: Path, monkeypatch: pytest.MonkeyPatch,
        tmp_path: Path, capsys: pytest.CaptureFixture,
    ) -> None:
        monkeypatch.delenv("FLEET_AGENT_ID", raising=False)
        rc, out, err = _run({
            "hook_event_name": "Stop",
            "transcript_path": str(_transcript(tmp_path)),
        }, capsys)
        assert rc == 0
        assert out == ""
        assert err == ""

    def test_blank_stdin_no_op(
        self, fleet_home_tmp: Path, capsys: pytest.CaptureFixture,
    ) -> None:
        rc, out, err = _run("", capsys)
        assert rc == 0
        assert out == ""

    def test_garbage_stdin_logs_and_exits_zero(
        self, fleet_home_tmp: Path, capsys: pytest.CaptureFixture,
    ) -> None:
        rc, out, err = _run("not json at all", capsys)
        assert rc == 0
        assert out == ""
        assert "payload parse error" in err

    def test_unknown_hook_event_silent(
        self, fleet_home_tmp: Path, capsys: pytest.CaptureFixture,
    ) -> None:
        rc, out, err = _run({"hook_event_name": "PostToolUse"}, capsys)
        assert rc == 0
        assert out == ""


# -- Stop hook ---------------------------------------------------------------

class TestStopHook:
    def test_updates_health(
        self, fleet_home_tmp: Path, tmp_path: Path,
        capsys: pytest.CaptureFixture,
    ) -> None:
        record_path = _seed_record(fleet_home_tmp)
        rc, _, _ = _run({
            "hook_event_name": "Stop",
            "transcript_path": str(_transcript(tmp_path, input_tokens=20_000)),
        }, capsys)
        assert rc == 0
        record = json.loads(record_path.read_text(encoding="utf-8"))
        # 20_000 / 200_000 = 10.0% green
        assert record["context_pct"] == 10.0
        assert record["context_source"] == "hook"

    def test_yellow_emits_handoff_injection(
        self, fleet_home_tmp: Path, tmp_path: Path,
        capsys: pytest.CaptureFixture,
    ) -> None:
        _seed_record(fleet_home_tmp)
        rc, out, _ = _run({
            "hook_event_name": "Stop",
            # 110k / 200k = 55% → yellow first fire
            "transcript_path": str(_transcript(tmp_path, input_tokens=110_000)),
        }, capsys)
        assert rc == 0
        assert "HANDOFF REQUESTED" in out
        assert "MILESTONE" in out

    def test_inbox_delivered_then_archived(
        self, fleet_home_tmp: Path, tmp_path: Path,
        capsys: pytest.CaptureFixture,
    ) -> None:
        _seed_record(fleet_home_tmp)
        inbox_dir = fleet_home_tmp / "inbox"
        inbox_dir.mkdir(parents=True, exist_ok=True)
        (inbox_dir / "agent7777.md").write_text("ship by friday",
                                                encoding="utf-8")

        rc, out, _ = _run({
            "hook_event_name": "Stop",
            "transcript_path": str(_transcript(tmp_path)),
        }, capsys)
        assert rc == 0
        assert "[OPERATOR] ship by friday" in out
        # archived
        assert not (inbox_dir / "agent7777.md").exists()
        archived = list((inbox_dir / "archive").glob("agent7777-*.md"))
        assert len(archived) == 1
        assert archived[0].read_text(encoding="utf-8") == "ship by friday"

    def test_inbox_then_handoff_concatenated_in_order(
        self, fleet_home_tmp: Path, tmp_path: Path,
        capsys: pytest.CaptureFixture,
    ) -> None:
        """Inbox message must precede handoff prompt — operator context
        informs whether the agent should wrap with MILESTONE this turn."""
        _seed_record(fleet_home_tmp)
        inbox_dir = fleet_home_tmp / "inbox"
        inbox_dir.mkdir(parents=True, exist_ok=True)
        (inbox_dir / "agent7777.md").write_text("operator says hi",
                                                encoding="utf-8")
        rc, out, _ = _run({
            "hook_event_name": "Stop",
            "transcript_path": str(_transcript(tmp_path, input_tokens=110_000)),
        }, capsys)
        assert rc == 0
        op_idx = out.index("[OPERATOR]")
        h_idx = out.index("HANDOFF REQUESTED")
        assert op_idx < h_idx, \
            f"inbox must come before handoff:\n{out}"

    def test_inbox_pending_flag_cleared(
        self, fleet_home_tmp: Path, tmp_path: Path,
        capsys: pytest.CaptureFixture,
    ) -> None:
        record_path = _seed_record(fleet_home_tmp, inbox_pending=True)
        inbox_dir = fleet_home_tmp / "inbox"
        inbox_dir.mkdir(parents=True, exist_ok=True)
        (inbox_dir / "agent7777.md").write_text("hi", encoding="utf-8")
        _run({
            "hook_event_name": "Stop",
            "transcript_path": str(_transcript(tmp_path)),
        }, capsys)
        record = json.loads(record_path.read_text(encoding="utf-8"))
        assert record["inbox_pending"] is False


# -- PreCompact hook ---------------------------------------------------------

class TestPreCompactHook:
    def test_writes_doc_and_queue_at_low_context(
        self, fleet_home_tmp: Path, tmp_path: Path,
        capsys: pytest.CaptureFixture,
    ) -> None:
        _seed_record(fleet_home_tmp)
        rc, out, _ = _run({
            "hook_event_name": "PreCompact",
            "transcript_path": str(_transcript(tmp_path, input_tokens=5_000)),
        }, capsys)
        assert rc == 0
        # PreCompact ignores stdout — the compaction is already happening.
        assert out == ""
        docs = list((fleet_home_tmp / "handoffs").glob("agent7777-*.md"))
        assert len(docs) == 1
        body = docs[0].read_bytes()
        assert b'handoff_type: "precompact"' in body


# -- SessionStart hook -------------------------------------------------------

class TestSessionStartHook:
    def test_delivers_inbox(
        self, fleet_home_tmp: Path, capsys: pytest.CaptureFixture,
    ) -> None:
        _seed_record(fleet_home_tmp)
        inbox_dir = fleet_home_tmp / "inbox"
        inbox_dir.mkdir(parents=True, exist_ok=True)
        (inbox_dir / "agent7777.md").write_text("welcome back",
                                                encoding="utf-8")
        rc, out, _ = _run({"hook_event_name": "SessionStart"}, capsys)
        assert rc == 0
        assert out == "[OPERATOR] welcome back"
        assert not (inbox_dir / "agent7777.md").exists()

    def test_no_inbox_silent(
        self, fleet_home_tmp: Path, capsys: pytest.CaptureFixture,
    ) -> None:
        _seed_record(fleet_home_tmp)
        rc, out, _ = _run({"hook_event_name": "SessionStart"}, capsys)
        assert rc == 0
        assert out == ""


# -- never-block-the-host ---------------------------------------------------

class TestNeverBlocks:
    def test_internal_exception_is_swallowed(
        self, fleet_home_tmp: Path, monkeypatch: pytest.MonkeyPatch,
        capsys: pytest.CaptureFixture,
    ) -> None:
        """If a sub-handler raises, main() logs to stderr and returns 0.
        The host agent's turn must continue."""
        _seed_record(fleet_home_tmp)
        # Inject a hard failure by replacing a downstream callable.
        import handoff
        def explode(*a, **k):
            raise RuntimeError("boom")
        monkeypatch.setattr(handoff, "maybe_trigger", explode)

        rc, _, err = _run({
            "hook_event_name": "Stop",
            "transcript_path": "/dev/null",
        }, capsys)
        assert rc == 0
        assert "dispatch error" in err
        assert "boom" in err
