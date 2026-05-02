"""Tests for skills/fleet-guard/main.py — the hook entry point.

The dispatch logic is thin (parse stdin → branch on hook_event_name →
delegate), so these tests focus on the wiring contract: which sub-callers
fire on which hook, what injection text concatenation looks like, and the
never-block-the-host failure mode.
"""
from __future__ import annotations

import io
import json
import os
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


    def test_inbox_suppressed_when_drain_in_flight(
        self, fleet_home_tmp: Path, tmp_path: Path,
        capsys: pytest.CaptureFixture, monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        """Codex iter-7 P2 regression. When a handoff is committed AND
        drain is in flight (queue on disk, no stale sentinel), the
        agent is about to die. Don't deliver inbox: maybe_trigger
        won't rewrite the handoff doc, so the inbox-driven turn would
        be invisible to the replacement."""
        import handoff as fleet_handoff  # noqa: WPS433

        _seed_record(fleet_home_tmp,
                     handoff_type="auto-red",
                     handoff_type_at="2026-04-30T00:00:00Z")
        queue_dir = fleet_home_tmp / "queue"
        queue_dir.mkdir(parents=True, exist_ok=True)
        (queue_dir / "spawn-fresh-agent7777.json").write_text("{}")
        # NO sentinel file — drain hasn't been kicked yet, so this
        # Stop's tail will kick. is_drain_in_flight returns True.
        inbox_dir = fleet_home_tmp / "inbox"
        inbox_dir.mkdir(parents=True, exist_ok=True)
        inbox_path = inbox_dir / "agent7777.md"
        inbox_path.write_text("ship by friday", encoding="utf-8")

        popen_calls: list[Any] = []
        monkeypatch.setattr(fleet_handoff.subprocess, "Popen",
                            lambda argv, **_: popen_calls.append(argv) or object())
        monkeypatch.setattr(fleet_handoff.shutil, "which",
                            lambda name: "/usr/local/bin/fleet"
                            if name == "fleet" else None)

        rc, out, _ = _run({
            "hook_event_name": "Stop",
            "transcript_path": str(_transcript(tmp_path, input_tokens=145_000)),
        }, capsys)
        assert rc == 0
        assert "[OPERATOR] ship by friday" not in out
        assert inbox_path.exists()
        drain_calls = [c for c in popen_calls if c and c[-1] == "drain"]
        assert len(drain_calls) == 1

    def test_inbox_delivered_when_drain_failing(
        self, fleet_home_tmp: Path, tmp_path: Path,
        capsys: pytest.CaptureFixture, monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        """Codex iter-8 P1 regression. When drain has been failing for
        longer than the backoff window (DisableAutoResume opt-out,
        legacy v1 record without cwd), the old agent stays alive
        forever. Operator inbox MUST reach it — it's the only live
        target. is_drain_in_flight returns False here because the
        sentinel is older than _KICK_BACKOFF_S."""
        import handoff as fleet_handoff  # noqa: WPS433

        _seed_record(fleet_home_tmp,
                     handoff_type="auto-red",
                     handoff_type_at="2026-04-30T00:00:00Z")
        queue_dir = fleet_home_tmp / "queue"
        queue_dir.mkdir(parents=True, exist_ok=True)
        queue_file = queue_dir / "spawn-fresh-agent7777.json"
        queue_file.write_text("{}")
        # Sentinel exists but is OLDER than backoff → drain has been
        # failing repeatedly; deliver inbox to old agent.
        sentinel = queue_file.with_name(queue_file.name + ".kicked")
        sentinel.touch()
        old = (
            datetime.now(timezone.utc).timestamp()
            - fleet_handoff._KICK_BACKOFF_S - 5
        )
        os.utime(sentinel, (old, old))

        inbox_dir = fleet_home_tmp / "inbox"
        inbox_dir.mkdir(parents=True, exist_ok=True)
        (inbox_dir / "agent7777.md").write_text("retry the build",
                                                encoding="utf-8")

        rc, out, _ = _run({
            "hook_event_name": "Stop",
            "transcript_path": str(_transcript(tmp_path)),
        }, capsys)
        assert rc == 0
        # Inbox WAS delivered — the agent is alive and addressable.
        assert "[OPERATOR] retry the build" in out

    def test_inbox_delivered_when_kick_can_never_launch(
        self, fleet_home_tmp: Path, tmp_path: Path,
        capsys: pytest.CaptureFixture, monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        """Codex iter-9 P2 regression. If the kick path can't launch
        anything (no FLEET_BIN, no fleet on PATH), no sentinel ever
        appears — yet the queue file persists. Without a fallback,
        is_drain_in_flight would loop on the "no sentinel = drain
        about to run" path and silently suppress every inbox. Queue
        mtime carries the signal: a queue file older than the backoff
        window with no sentinel means kick is permanently broken and
        the old agent is the only live target."""
        import handoff as fleet_handoff  # noqa: WPS433

        _seed_record(fleet_home_tmp,
                     handoff_type="auto-red",
                     handoff_type_at="2026-04-30T00:00:00Z")
        queue_dir = fleet_home_tmp / "queue"
        queue_dir.mkdir(parents=True, exist_ok=True)
        queue_file = queue_dir / "spawn-fresh-agent7777.json"
        queue_file.write_text("{}")
        # No sentinel; queue mtime backdated past backoff.
        old = (
            datetime.now(timezone.utc).timestamp()
            - fleet_handoff._KICK_BACKOFF_S - 5
        )
        os.utime(queue_file, (old, old))

        # Kick path totally broken: no FLEET_BIN, no fleet on PATH.
        monkeypatch.delenv("FLEET_BIN", raising=False)
        monkeypatch.setattr(fleet_handoff.shutil, "which", lambda _name: None)

        inbox_dir = fleet_home_tmp / "inbox"
        inbox_dir.mkdir(parents=True, exist_ok=True)
        (inbox_dir / "agent7777.md").write_text("retry the build",
                                                encoding="utf-8")

        rc, out, _ = _run({
            "hook_event_name": "Stop",
            "transcript_path": str(_transcript(tmp_path)),
        }, capsys)
        assert rc == 0
        assert "[OPERATOR] retry the build" in out

    def test_kick_deferred_when_inbox_and_handoff_in_same_turn(
        self, fleet_home_tmp: Path, tmp_path: Path,
        capsys: pytest.CaptureFixture, monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        """Codex iter-6 P2 regression. When inbox AND a fresh red
        handoff land in the same Stop (queue was NOT pending coming
        in, so iter-7's inbox-suppression doesn't fire — inbox
        delivers normally — and maybe_trigger then writes the queue
        on the red threshold trip), the kick must defer. Otherwise
        drain's `/exit` would race the inbox-driven turn the agent
        is about to start, splitting / duplicating its work."""
        import handoff as fleet_handoff  # noqa: WPS433

        _seed_record(fleet_home_tmp)
        # No pre-existing queue file: is_drain_in_flight is False at
        # inbox-check time, so inbox flows through normally.
        inbox_dir = fleet_home_tmp / "inbox"
        inbox_dir.mkdir(parents=True, exist_ok=True)
        (inbox_dir / "agent7777.md").write_text("ship by friday",
                                                encoding="utf-8")

        popen_calls: list[Any] = []
        monkeypatch.setattr(fleet_handoff.subprocess, "Popen",
                            lambda argv, **_: popen_calls.append(argv) or object())
        monkeypatch.setattr(fleet_handoff.shutil, "which",
                            lambda name: "/usr/local/bin/fleet"
                            if name == "fleet" else None)

        # 145k tokens → 72.5% → red. maybe_trigger writes queue this
        # turn. With inbox already in injections, the iter-6 gate
        # defers the kick to the next idle Stop.
        rc, out, _ = _run({
            "hook_event_name": "Stop",
            "transcript_path": str(_transcript(tmp_path, input_tokens=145_000)),
        }, capsys)
        assert rc == 0
        assert "[OPERATOR] ship by friday" in out
        drain_calls = [c for c in popen_calls if c and c[-1] == "drain"]
        assert drain_calls == [], (
            "drain kicked while inbox was being injected; the agent "
            f"would race its own retirement: {popen_calls}")
        # Queue file persists for the next idle Stop / TUI watcher.
        assert (fleet_home_tmp / "queue" / "spawn-fresh-agent7777.json").exists()


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

    def test_does_not_kick_drain(
        self, fleet_home_tmp: Path, tmp_path: Path,
        capsys: pytest.CaptureFixture, monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        """Codex iter-9 P1 regression. PreCompact fires while the
        agent is mid-compaction, NOT idle. Kicking drain here would
        send `/exit` to a session that's actively running its
        compaction turn — interrupts mid-tool-call, producing side
        effects the handoff doc (written before compaction started)
        won't reflect. Queue file persists; the next Stop after
        compaction completes will pick it up."""
        import handoff as fleet_handoff  # noqa: WPS433

        _seed_record(fleet_home_tmp)
        popen_calls: list[Any] = []
        monkeypatch.setattr(fleet_handoff.subprocess, "Popen",
                            lambda argv, **_: popen_calls.append(argv) or object())
        monkeypatch.setattr(fleet_handoff.shutil, "which",
                            lambda name: "/usr/local/bin/fleet"
                            if name == "fleet" else None)

        rc, _, _ = _run({
            "hook_event_name": "PreCompact",
            "transcript_path": str(_transcript(tmp_path)),
        }, capsys)
        assert rc == 0
        drain_calls = [c for c in popen_calls if c and c[-1] == "drain"]
        assert drain_calls == [], (
            "drain kicked from PreCompact would interrupt the agent's "
            f"compaction turn: {popen_calls}")
        # But the queue file IS on disk for the next Stop / TUI to pick up.
        assert (fleet_home_tmp / "queue" / "spawn-fresh-agent7777.json").exists()


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
