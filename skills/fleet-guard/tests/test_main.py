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

    def test_inbox_pending_stays_set_when_archive_fails(
        self, fleet_home_tmp: Path, tmp_path: Path,
        capsys: pytest.CaptureFixture,
        monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        """If inbox.archive returns False (rename failed), the skill must
        NOT clear inbox_pending — otherwise the TUI shows 'no message'
        while the file persists on disk and gets re-delivered next fire."""
        record_path = _seed_record(fleet_home_tmp, inbox_pending=True)
        inbox_dir = fleet_home_tmp / "inbox"
        inbox_dir.mkdir(parents=True, exist_ok=True)
        (inbox_dir / "agent7777.md").write_text("hi", encoding="utf-8")

        import inbox as inbox_module
        monkeypatch.setattr(inbox_module, "archive", lambda _id: False)

        rc, out, _ = _run({
            "hook_event_name": "Stop",
            "transcript_path": str(_transcript(tmp_path)),
        }, capsys)
        assert rc == 0
        # Operator message still delivered to the agent (we have the
        # body in memory; failure is on disk-archive, not delivery).
        assert "[OPERATOR] hi" in out
        # But inbox_pending stays True so the TUI agrees with disk.
        record = json.loads(record_path.read_text(encoding="utf-8"))
        assert record["inbox_pending"] is True


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


# -- needs_input wiring (Stop sets, UserPromptSubmit clears) ---------------

class TestNeedsInputFlag:
    def test_stop_sets_needs_input_true_when_idle(
        self, fleet_home_tmp: Path, tmp_path: Path,
        capsys: pytest.CaptureFixture,
    ) -> None:
        # Fresh Stop with no inbox + no handoff threshold = real idle:
        # claude finished a turn and is now waiting for the operator.
        # The TUI renders "waiting" off this flag.
        record_path = _seed_record(fleet_home_tmp, needs_input=False)
        rc, _, _ = _run({
            "hook_event_name": "Stop",
            "transcript_path": str(_transcript(tmp_path)),
        }, capsys)
        assert rc == 0
        record = json.loads(record_path.read_text(encoding="utf-8"))
        assert record["needs_input"] is True

    def test_stop_with_inbox_does_not_mark_waiting(
        self, fleet_home_tmp: Path, tmp_path: Path,
        capsys: pytest.CaptureFixture,
    ) -> None:
        # When Stop injects an inbox message via stdout, claude starts
        # processing it immediately — that's "working", not "waiting".
        # Marking needs_input=true here would pin the TUI's "waiting"
        # badge on an agent that's actively running (codex iter-2 P2).
        record_path = _seed_record(fleet_home_tmp, needs_input=True)
        inbox_dir = fleet_home_tmp / "inbox"
        inbox_dir.mkdir(parents=True, exist_ok=True)
        (inbox_dir / "agent7777.md").write_text("ship by friday",
                                                encoding="utf-8")
        rc, out, _ = _run({
            "hook_event_name": "Stop",
            "transcript_path": str(_transcript(tmp_path)),
        }, capsys)
        assert rc == 0
        assert "[OPERATOR]" in out
        record = json.loads(record_path.read_text(encoding="utf-8"))
        assert record["needs_input"] is False

    def test_stop_with_handoff_injection_does_not_mark_waiting(
        self, fleet_home_tmp: Path, tmp_path: Path,
        capsys: pytest.CaptureFixture,
    ) -> None:
        # Same logic for the handoff path: yellow-threshold injects
        # HANDOFF REQUESTED so claude continues working on the wrap-up.
        # needs_input must stay false until claude actually idles.
        record_path = _seed_record(fleet_home_tmp, needs_input=False)
        rc, out, _ = _run({
            "hook_event_name": "Stop",
            # 110k / 200k = 55% → yellow injects HANDOFF REQUESTED
            "transcript_path": str(_transcript(tmp_path, input_tokens=110_000)),
        }, capsys)
        assert rc == 0
        assert "HANDOFF REQUESTED" in out
        record = json.loads(record_path.read_text(encoding="utf-8"))
        assert record["needs_input"] is False

    def test_user_prompt_submit_clears_needs_input(
        self, fleet_home_tmp: Path, capsys: pytest.CaptureFixture,
    ) -> None:
        # Seed record in the post-Stop state (waiting); UserPromptSubmit
        # is the moment claude transitions waiting → working, so the
        # flag must drop to false so the TUI stops rendering "waiting".
        record_path = _seed_record(fleet_home_tmp, needs_input=True)
        rc, out, _ = _run({"hook_event_name": "UserPromptSubmit"}, capsys)
        assert rc == 0
        # Stdout is ignored by Claude Code on this hook — keep it empty
        # so we don't accidentally inject into the operator's prompt.
        assert out == ""
        record = json.loads(record_path.read_text(encoding="utf-8"))
        assert record["needs_input"] is False

    def test_user_prompt_submit_clears_has_pending_question(
        self, fleet_home_tmp: Path, capsys: pytest.CaptureFixture,
    ) -> None:
        # has_pending_question must clear when the operator answers —
        # otherwise the TUI renders "asking" while the agent is already
        # processing the new prompt, until the next Stop recomputes
        # (codex review iter for asking/idle split: P2).
        record_path = _seed_record(fleet_home_tmp,
                                   needs_input=True,
                                   has_pending_question=True)
        rc, _, _ = _run({"hook_event_name": "UserPromptSubmit"}, capsys)
        assert rc == 0
        record = json.loads(record_path.read_text(encoding="utf-8"))
        assert record["has_pending_question"] is False
        assert record["needs_input"] is False

    def test_session_start_no_inbox_marks_waiting(
        self, fleet_home_tmp: Path, capsys: pytest.CaptureFixture,
    ) -> None:
        # SessionStart with no inbox means claude is sitting at an idle
        # prompt and the operator must type next. needs_input=true so
        # the TUI renders "waiting" (otherwise fresh agents look "live"
        # while actually blocked on the operator).
        record_path = _seed_record(fleet_home_tmp, needs_input=False)
        rc, _, _ = _run({"hook_event_name": "SessionStart"}, capsys)
        assert rc == 0
        record = json.loads(record_path.read_text(encoding="utf-8"))
        assert record["needs_input"] is True

    def test_session_start_clears_stale_has_pending_question(
        self, fleet_home_tmp: Path, capsys: pytest.CaptureFixture,
    ) -> None:
        # A record that was Stopped with has_pending_question=true and
        # then session-restarted (claude restored, agent paused, etc.)
        # must NOT carry the question flag forward — the resume hands
        # claude a fresh prompt regardless of what the prior turn
        # ended on. Without this clear, the TUI renders the resumed
        # agent as "asking" forever (codex review iter for
        # asking/idle split: P2 reproducer).
        record_path = _seed_record(fleet_home_tmp,
                                   needs_input=False,
                                   has_pending_question=True)
        rc, _, _ = _run({"hook_event_name": "SessionStart"}, capsys)
        assert rc == 0
        record = json.loads(record_path.read_text(encoding="utf-8"))
        assert record["has_pending_question"] is False

    def test_session_start_with_inbox_does_not_mark_waiting(
        self, fleet_home_tmp: Path, capsys: pytest.CaptureFixture,
    ) -> None:
        # SessionStart can inject queued operator context on resume.
        # Claude starts processing it immediately — needs_input=false
        # so the TUI doesn't show "waiting" while the agent is actually
        # working through the injected message (codex iter-2 P2).
        record_path = _seed_record(fleet_home_tmp, needs_input=True)
        inbox_dir = fleet_home_tmp / "inbox"
        inbox_dir.mkdir(parents=True, exist_ok=True)
        (inbox_dir / "agent7777.md").write_text("welcome back",
                                                encoding="utf-8")
        rc, out, _ = _run({"hook_event_name": "SessionStart"}, capsys)
        assert rc == 0
        assert "[OPERATOR]" in out
        record = json.loads(record_path.read_text(encoding="utf-8"))
        assert record["needs_input"] is False


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
