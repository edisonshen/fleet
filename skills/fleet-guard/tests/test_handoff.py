"""Tests for skills/fleet-guard/handoff.py.

The byte-shape of the rendered doc is the load-bearing contract. A drift
here breaks 4a's chain reader silently. Two-sided verification:

- The Python golden (test_render_byte_golden) pins the exact bytes Python
  produces.
- A Go test in internal/handoff (added in this PR) pins the same bytes
  from Render. If either drifts, both must change.

Other tests cover the state machine (maybe_trigger / emergency_trigger)
with monkey-patched tmux + a seeded record, plus _go_quote / _format_float
edge cases. See test_health.py for the FLEET_HOME redirect fixture.
"""
from __future__ import annotations

import json
import re
import subprocess
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

import pytest

import handoff
import health
import ids


# -- shared fixtures ---------------------------------------------------------

@pytest.fixture(autouse=True)
def fleet_home_tmp(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> Path:
    home = tmp_path / "fleet"
    monkeypatch.setenv("FLEET_HOME", str(home))
    return home


def _seed_record(home: Path, agent_id: str, **overrides: Any) -> Path:
    """Same shape as test_health._seed_record — duplicated here intentionally
    so test files don't depend on each other through implicit imports."""
    record_dir = home / "agents"
    record_dir.mkdir(parents=True, exist_ok=True)
    base = {
        "schema_version": 1,
        "id": agent_id,
        "pid": 12345,
        "tmux_session": f"fleet-{agent_id}",
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
        "cwd": "/home/op/projects/myproj",
        "command": ["claude"],
        "spawned_at": "2026-04-28T00:00:00Z",
    }
    base.update(overrides)
    path = record_dir / f"{agent_id}.json"
    path.write_text(json.dumps(base, indent=2) + "\n", encoding="utf-8")
    return path


def _transcript(tmp_path: Path, *, model: str = "claude-sonnet-4-6",
                input_tokens: int = 50_000) -> Path:
    """Minimal transcript producing a known context_pct."""
    path = tmp_path / "transcript.jsonl"
    path.write_text(json.dumps({
        "type": "assistant",
        "message": {
            "model": model,
            "usage": {"input_tokens": input_tokens},
        },
    }) + "\n", encoding="utf-8")
    return path


class _FakeTmux:
    """Stand-in for subprocess.run that drives tmux capture-pane output.
    Test sets `output` (full pane) and asserts on `calls` to verify the
    skill invoked tmux with the right session / -S flag."""
    def __init__(self) -> None:
        self.output: str = ""
        self.returncode: int = 0
        self.calls: list[list[str]] = []

    def __call__(self, cmd, **kwargs):  # mimics subprocess.run signature
        self.calls.append(list(cmd))
        return subprocess.CompletedProcess(
            args=cmd, returncode=self.returncode,
            stdout=self.output, stderr="",
        )


@pytest.fixture
def fake_tmux(monkeypatch: pytest.MonkeyPatch) -> _FakeTmux:
    fake = _FakeTmux()
    monkeypatch.setattr(handoff.subprocess, "run", fake)
    return fake


# -- _go_quote ---------------------------------------------------------------

class TestGoQuote:
    def test_simple_ascii(self) -> None:
        assert handoff._go_quote("hello") == '"hello"'

    def test_empty(self) -> None:
        assert handoff._go_quote("") == '""'

    def test_quote_escaped(self) -> None:
        assert handoff._go_quote('foo"bar') == r'"foo\"bar"'

    def test_backslash_escaped(self) -> None:
        assert handoff._go_quote("foo\\bar") == r'"foo\\bar"'

    def test_newline_escaped(self) -> None:
        assert handoff._go_quote("foo\nbar") == r'"foo\nbar"'

    def test_em_dash_passes_through(self) -> None:
        # Go's %q (without +) leaves printable Unicode unescaped. The
        # canonical PLACEHOLDER contains an em dash and MUST round-trip.
        assert handoff._go_quote("a — b") == '"a — b"'

    def test_yaml_metacharacter_quoted_safely(self) -> None:
        # Operator-supplied paths can contain colons. Quoting with Go %q
        # makes them YAML-flow-scalar-safe, which is the whole point of
        # handoff.go's quoting strategy.
        out = handoff._go_quote("/path/with: colon")
        assert out == '"/path/with: colon"'


# -- _format_float_go --------------------------------------------------------

class TestFormatFloatGo:
    @pytest.mark.parametrize("value,expected", [
        (0.0, "0"),
        (50.0, "50"),
        (100.0, "100"),
        (25.17, "25.17"),
        (0.5, "0.5"),
        (49.99, "49.99"),
        (70.001, "70.001"),
    ])
    def test_matches_go_format(self, value: float, expected: str) -> None:
        assert handoff._format_float_go(value) == expected


# -- _render_doc byte golden -------------------------------------------------

# This golden MUST match what internal/handoff.Render produces for the same
# inputs. A paired Go test (handoff_test.go:TestRender_SkillByteGolden) asserts
# the same bytes from the Go side. If either drifts, both fail.
EXPECTED_GOLDEN = (
    b"---\n"
    b'agent_id: "abcd1234"\n'
    b'task_id: "demo-task"\n'
    b'project: "myproj"\n'
    b"context_pct_at_handoff: 50\n"
    b'previous_handoff: "/home/op/.fleet/handoffs/prev.md"\n'
    b"handoff_number: 2\n"
    b'timestamp: "2026-04-28T12:34:56Z"\n'
    b'handoff_type: "auto-yellow"\n'
    b"---\n"
    b"\n"
    b"## Completed\n"
    b"Wrote tests for foo\n"
    b"\n"
    b"## Key Decisions\n"
    b"_(operator-triggered handoff \xe2\x80\x94 fill in before resuming)_\n"
    b"\n"
    b"## Files Modified\n"
    b"_(operator-triggered handoff \xe2\x80\x94 fill in before resuming)_\n"
    b"\n"
    b"## Open Questions\n"
    b"_(operator-triggered handoff \xe2\x80\x94 fill in before resuming)_\n"
    b"\n"
    b"## Next Steps (prioritized)\n"
    b"_(operator-triggered handoff \xe2\x80\x94 fill in before resuming)_\n"
)


class TestRenderByteGolden:
    def test_byte_for_byte_match(self) -> None:
        ts = datetime(2026, 4, 28, 12, 34, 56, tzinfo=timezone.utc)
        got = handoff._render_doc(
            agent_id="abcd1234",
            task_id="demo-task",
            project="myproj",
            handoff_type="auto-yellow",
            number=2,
            prev_path="/home/op/.fleet/handoffs/prev.md",
            context_pct=50.0,
            ts=ts,
            recent_activity="Wrote tests for foo",
        )
        assert got == EXPECTED_GOLDEN, _diff_first_byte(got, EXPECTED_GOLDEN)

    def test_null_context_pct_and_prev_path(self) -> None:
        ts = datetime(2026, 4, 28, 12, 34, 56, tzinfo=timezone.utc)
        got = handoff._render_doc(
            agent_id="abcd1234",
            task_id="demo-task",
            project="myproj",
            handoff_type="precompact",
            number=1,
            prev_path=None,
            context_pct=None,
            ts=ts,
            recent_activity="",
        )
        assert b"context_pct_at_handoff: null\n" in got
        assert b"previous_handoff: null\n" in got
        # Empty recent_activity falls back to the canonical placeholder so
        # the body never has a literal blank section.
        assert handoff.PLACEHOLDER.encode("utf-8") in got


def _diff_first_byte(got: bytes, want: bytes) -> str:
    """Format a useful diff message: index + context around first divergence."""
    n = min(len(got), len(want))
    for i in range(n):
        if got[i] != want[i]:
            return (
                f"first diff at byte {i}: "
                f"got {got[max(0,i-20):i+20]!r} vs want {want[max(0,i-20):i+20]!r}"
            )
    if len(got) != len(want):
        return f"length mismatch: got {len(got)} vs want {len(want)}"
    return "no diff"


# -- find_milestone ----------------------------------------------------------

class TestFindMilestone:
    def test_milestone_on_own_line(self, fake_tmux: _FakeTmux) -> None:
        fake_tmux.output = (
            f"{handoff.HANDOFF_REQUESTED}: wrap up\n"
            "doing work\n"
            "MILESTONE\n"
            "next line\n"
        )
        assert handoff.find_milestone("fleet-abc") is True

    def test_milestone_with_surrounding_whitespace(
        self, fake_tmux: _FakeTmux,
    ) -> None:
        fake_tmux.output = (
            f"{handoff.HANDOFF_REQUESTED}: wrap up\n"
            "  MILESTONE  \n"
        )
        assert handoff.find_milestone("fleet-abc") is True

    def test_milestones_word_does_not_match(self, fake_tmux: _FakeTmux) -> None:
        # Common false-positive guard: the agent might say "MILESTONES are"
        # in normal narration. Must not trigger.
        fake_tmux.output = "MILESTONES are documented in docs/MILESTONES.md\n"
        assert handoff.find_milestone("fleet-abc") is False

    def test_milestone_inline_does_not_match(self, fake_tmux: _FakeTmux) -> None:
        fake_tmux.output = "ok MILESTONE: done\n"
        assert handoff.find_milestone("fleet-abc") is False

    def test_old_milestone_before_handoff_requested_ignored(
        self, fake_tmux: _FakeTmux,
    ) -> None:
        """A MILESTONE line earlier in pane history (from a prior wrap-up
        or from the agent narrating the marker) MUST NOT satisfy the
        check on the very turn HANDOFF REQUESTED was injected. Otherwise
        Yellow would cut off active work the moment the injection lands."""
        fake_tmux.output = (
            "earlier work\n"
            "MILESTONE\n"                 # historical, must be ignored
            "more work\n"
            f"{handoff.HANDOFF_REQUESTED}: context window over 50%\n"
            "agent is wrapping up...\n"
        )
        assert handoff.find_milestone("fleet-abc") is False

    def test_milestone_after_handoff_requested_matches(
        self, fake_tmux: _FakeTmux,
    ) -> None:
        fake_tmux.output = (
            f"{handoff.HANDOFF_REQUESTED}: context window over 50%\n"
            "wrapping...\n"
            "MILESTONE\n"
        )
        assert handoff.find_milestone("fleet-abc") is True

    def test_no_handoff_requested_in_pane_returns_false(
        self, fake_tmux: _FakeTmux,
    ) -> None:
        """If HANDOFF REQUESTED scrolled off pane history, the bounded
        search returns False rather than counting any MILESTONE as the
        trigger. The Red threshold + emergency_trigger eventually catch
        a runaway agent."""
        fake_tmux.output = "no injection in this pane\nMILESTONE\n"
        assert handoff.find_milestone("fleet-abc") is False

    def test_tmux_failure_returns_false(self, fake_tmux: _FakeTmux) -> None:
        fake_tmux.returncode = 1  # tmux error
        assert handoff.find_milestone("fleet-abc") is False

    def test_invokes_correct_session(self, fake_tmux: _FakeTmux) -> None:
        fake_tmux.output = "MILESTONE\n"
        handoff.find_milestone("fleet-deadbeef")
        assert fake_tmux.calls[0][:5] == [
            "tmux", "capture-pane", "-t", "fleet-deadbeef", "-p",
        ]


# -- capture_recent ----------------------------------------------------------

class TestCaptureRecent:
    def test_strips_ansi_sgr(self, fake_tmux: _FakeTmux) -> None:
        fake_tmux.output = "\x1b[31mred text\x1b[0m and \x1b[1mbold\x1b[0m\n"
        out = handoff.capture_recent("fleet-abc")
        assert out == "red text and bold\n"

    def test_passes_lines_arg(self, fake_tmux: _FakeTmux) -> None:
        fake_tmux.output = "x"
        handoff.capture_recent("fleet-abc", lines=50)
        assert "-S" in fake_tmux.calls[0]
        assert "-50" in fake_tmux.calls[0]

    def test_returns_empty_on_failure(self, fake_tmux: _FakeTmux) -> None:
        fake_tmux.returncode = 1
        assert handoff.capture_recent("fleet-abc") == ""


# -- maybe_trigger state machine --------------------------------------------

class TestMaybeTrigger:
    def test_missing_record_returns_none(self) -> None:
        assert handoff.maybe_trigger({}, agent_id="missing", session="x") is None

    def test_thinking_mode_skips(
        self, fleet_home_tmp: Path, fake_tmux: _FakeTmux, tmp_path: Path,
    ) -> None:
        _seed_record(fleet_home_tmp, "agent01", mode="plan")
        result = handoff.maybe_trigger(
            {"transcript_path": str(_transcript(tmp_path, input_tokens=180_000))},
            agent_id="agent01", session="fleet-agent01",
        )
        # Even at >70% context, a planning agent must not be auto-handoffed.
        assert result is None
        assert not (fleet_home_tmp / "queue").exists() or \
            list((fleet_home_tmp / "queue").iterdir()) == []

    def test_green_returns_none_no_writes(
        self, fleet_home_tmp: Path, fake_tmux: _FakeTmux, tmp_path: Path,
    ) -> None:
        record_path = _seed_record(fleet_home_tmp, "agent02")
        result = handoff.maybe_trigger(
            {"transcript_path": str(_transcript(tmp_path, input_tokens=10_000))},
            agent_id="agent02", session="fleet-agent02",
        )
        assert result is None
        record = json.loads(record_path.read_text(encoding="utf-8"))
        assert record["handoff_type"] is None  # not marked pending

    def test_yellow_first_fire_injects_and_marks_pending(
        self, fleet_home_tmp: Path, fake_tmux: _FakeTmux, tmp_path: Path,
    ) -> None:
        record_path = _seed_record(fleet_home_tmp, "agent03")
        result = handoff.maybe_trigger(
            # 110_000 / 200_000 = 55% → yellow
            {"transcript_path": str(_transcript(tmp_path, input_tokens=110_000))},
            agent_id="agent03", session="fleet-agent03",
        )
        assert result is not None
        assert handoff.HANDOFF_REQUESTED in result
        assert handoff.MILESTONE in result
        record = json.loads(record_path.read_text(encoding="utf-8"))
        assert record["handoff_type"] == handoff.TYPE_AUTO_YELLOW
        # No queue file written yet — Yellow waits for MILESTONE.
        queue_dir = fleet_home_tmp / "queue"
        assert not queue_dir.exists() or list(queue_dir.iterdir()) == []

    def test_yellow_pending_with_milestone_writes_doc_and_queue(
        self, fleet_home_tmp: Path, fake_tmux: _FakeTmux, tmp_path: Path,
    ) -> None:
        _seed_record(fleet_home_tmp, "agent04",
                     handoff_type=handoff.TYPE_AUTO_YELLOW)
        fake_tmux.output = (
            f"{handoff.HANDOFF_REQUESTED}: wrap up at next safe boundary\n"
            "wrapped up the work\n"
            "MILESTONE\n"
        )
        result = handoff.maybe_trigger(
            {"transcript_path": str(_transcript(tmp_path, input_tokens=110_000))},
            agent_id="agent04", session="fleet-agent04",
        )
        assert result is None  # silent on the "doc-written" path
        # Doc written
        docs = list((fleet_home_tmp / "handoffs").glob("agent04-*.md"))
        assert len(docs) == 1
        body = docs[0].read_bytes()
        assert b'handoff_type: "auto-yellow"' in body
        # Queue file written, schema-correct
        queue_files = list((fleet_home_tmp / "queue").glob("spawn-fresh-*.json"))
        assert len(queue_files) == 1
        q = json.loads(queue_files[0].read_text(encoding="utf-8"))
        assert q["old_agent_id"] == "agent04"
        assert q["new_agent_id"] != ""
        assert q["new_session"] == f"fleet-{q['new_agent_id']}"
        assert q["handoff_doc"] == str(docs[0])
        assert q["schema_version"] == 1
        assert q["task_id"] == "demo-task"
        assert q["project"] == "myproj"

    def test_yellow_pending_without_milestone_noop(
        self, fleet_home_tmp: Path, fake_tmux: _FakeTmux, tmp_path: Path,
    ) -> None:
        _seed_record(fleet_home_tmp, "agent05",
                     handoff_type=handoff.TYPE_AUTO_YELLOW)
        fake_tmux.output = "still working...\n"
        result = handoff.maybe_trigger(
            {"transcript_path": str(_transcript(tmp_path, input_tokens=110_000))},
            agent_id="agent05", session="fleet-agent05",
        )
        assert result is None
        assert not (fleet_home_tmp / "handoffs").exists() or \
            list((fleet_home_tmp / "handoffs").iterdir()) == []

    def test_red_writes_immediately(
        self, fleet_home_tmp: Path, fake_tmux: _FakeTmux, tmp_path: Path,
    ) -> None:
        _seed_record(fleet_home_tmp, "agent06")
        result = handoff.maybe_trigger(
            # 145_000 / 200_000 = 72.5% → red
            {"transcript_path": str(_transcript(tmp_path, input_tokens=145_000))},
            agent_id="agent06", session="fleet-agent06",
        )
        assert result is None
        docs = list((fleet_home_tmp / "handoffs").glob("agent06-*.md"))
        assert len(docs) == 1
        body = docs[0].read_bytes()
        assert b'handoff_type: "auto-red"' in body
        queue_files = list((fleet_home_tmp / "queue").glob("spawn-fresh-*.json"))
        assert len(queue_files) == 1


# -- successor agent must not start "pending" (codex P1.A) -----------------

class TestPendingDistinguishesAutoFromManual:
    def test_manual_handoff_type_is_not_pending(self) -> None:
        """internal/spawn/spawn.go stamps every successor record with
        handoff_type='manual'. is_handoff_pending must NOT treat that as
        a live auto-handoff request — otherwise every successor starts
        life with pending=True and Yellow's first-fire never injects."""
        assert handoff.is_handoff_pending({"handoff_type": "manual"}) is False

    @pytest.mark.parametrize("t", [
        handoff.TYPE_AUTO_YELLOW,
        handoff.TYPE_AUTO_RED,
        handoff.TYPE_PRECOMPACT,
    ])
    def test_auto_types_are_pending(self, t: str) -> None:
        assert handoff.is_handoff_pending({"handoff_type": t}) is True

    def test_none_or_missing_is_not_pending(self) -> None:
        assert handoff.is_handoff_pending({"handoff_type": None}) is False
        assert handoff.is_handoff_pending({}) is False
        assert handoff.is_handoff_pending({"handoff_type": ""}) is False

    def test_yellow_first_fire_on_successor_with_manual_stamp(
        self, fleet_home_tmp: Path, fake_tmux: _FakeTmux, tmp_path: Path,
    ) -> None:
        """End-to-end on the bug: a successor record (handoff_type=manual)
        crossing 50% must inject HANDOFF REQUESTED on its first fire, NOT
        skip straight to MILESTONE grep."""
        _seed_record(fleet_home_tmp, "successor1", handoff_type="manual")
        result = handoff.maybe_trigger(
            {"transcript_path": str(_transcript(tmp_path, input_tokens=110_000))},
            agent_id="successor1", session="fleet-successor1",
        )
        # Injection MUST fire — successor was a fresh agent, not pending.
        assert result is not None
        assert handoff.HANDOFF_REQUESTED in result

    def test_red_on_successor_with_manual_stamp_writes_handoff(
        self, fleet_home_tmp: Path, fake_tmux: _FakeTmux, tmp_path: Path,
    ) -> None:
        """Same bug, Red side: a successor crossing 70% must still write
        the emergency handoff. The 'manual' stamp is not a pending state."""
        _seed_record(fleet_home_tmp, "successor2", handoff_type="manual")
        handoff.maybe_trigger(
            {"transcript_path": str(_transcript(tmp_path, input_tokens=145_000))},
            agent_id="successor2", session="fleet-successor2",
        )
        docs = list((fleet_home_tmp / "handoffs").glob("successor2-*.md"))
        assert len(docs) == 1, \
            "Red on successor with manual stamp must still write a doc"


# -- re-fire race protection (P1.4) -----------------------------------------

class TestRefireProtection:
    def test_red_refire_while_pending_is_noop(
        self, fleet_home_tmp: Path, fake_tmux: _FakeTmux, tmp_path: Path,
    ) -> None:
        """A Red agent that already enqueued a handoff (handoff_type set)
        must NOT re-render on the next Stop fire — that would write a
        second doc with a different timestamp/random suffix and orphan
        the first one (the queue file gets clobbered to point at the
        second doc; the first becomes garbage on disk)."""
        _seed_record(fleet_home_tmp, "agentR1",
                     handoff_type=handoff.TYPE_AUTO_RED)
        result = handoff.maybe_trigger(
            {"transcript_path": str(_transcript(tmp_path, input_tokens=145_000))},
            agent_id="agentR1", session="fleet-agentR1",
        )
        assert result is None
        # Crucially, NO new doc was written for this fire.
        docs = list((fleet_home_tmp / "handoffs").glob("agentR1-*.md"))
        assert len(docs) == 0

    def test_red_pre_marks_handoff_type_before_render(
        self, fleet_home_tmp: Path, fake_tmux: _FakeTmux,
        tmp_path: Path, monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        """Pre-mark must land BEFORE write_doc so a re-fire racing
        _do_handoff sees pending=True and short-circuits. Test by
        sniffing the order of calls via monkey-patching write_doc to
        record the record's handoff_type at the moment write_doc is
        invoked."""
        record_path = _seed_record(fleet_home_tmp, "agentR2")
        observed: dict = {}

        real_write_doc = handoff.write_doc

        def spy_write_doc(**kwargs):
            current = json.loads(record_path.read_text(encoding="utf-8"))
            observed["handoff_type_at_write"] = current.get("handoff_type")
            return real_write_doc(**kwargs)
        monkeypatch.setattr(handoff, "write_doc", spy_write_doc)

        handoff.maybe_trigger(
            {"transcript_path": str(_transcript(tmp_path, input_tokens=145_000))},
            agent_id="agentR2", session="fleet-agentR2",
        )
        assert observed.get("handoff_type_at_write") == handoff.TYPE_AUTO_RED, \
            "handoff_type was not pre-marked on disk before write_doc"


# -- emergency_trigger -------------------------------------------------------

class TestEmergencyTrigger:
    def test_writes_precompact_doc_regardless_of_threshold(
        self, fleet_home_tmp: Path, fake_tmux: _FakeTmux, tmp_path: Path,
    ) -> None:
        _seed_record(fleet_home_tmp, "agent07")
        # Even at 5% context, PreCompact must write a handoff — the
        # compaction itself is the trigger.
        handoff.emergency_trigger(
            {"transcript_path": str(_transcript(tmp_path, input_tokens=10_000))},
            agent_id="agent07", session="fleet-agent07",
        )
        docs = list((fleet_home_tmp / "handoffs").glob("agent07-*.md"))
        assert len(docs) == 1
        body = docs[0].read_bytes()
        assert b'handoff_type: "precompact"' in body

    def test_missing_record_silent_noop(
        self, fleet_home_tmp: Path, fake_tmux: _FakeTmux,
    ) -> None:
        # Doesn't raise, doesn't write anything.
        handoff.emergency_trigger({}, agent_id="missing", session="x")
        assert not (fleet_home_tmp / "handoffs").exists() or \
            list((fleet_home_tmp / "handoffs").iterdir()) == []

    def test_refire_while_pending_is_noop(
        self, fleet_home_tmp: Path, fake_tmux: _FakeTmux, tmp_path: Path,
    ) -> None:
        """PreCompact firing twice (e.g., upstream double-fire) must
        not write a second handoff doc — same orphan-doc concern as
        the Red branch in maybe_trigger."""
        _seed_record(fleet_home_tmp, "agentP1",
                     handoff_type=handoff.TYPE_PRECOMPACT)
        handoff.emergency_trigger(
            {"transcript_path": str(_transcript(tmp_path, input_tokens=10_000))},
            agent_id="agentP1", session="fleet-agentP1",
        )
        docs = list((fleet_home_tmp / "handoffs").glob("agentP1-*.md"))
        assert len(docs) == 0


# -- write_queue schema ------------------------------------------------------

class TestWriteQueue:
    def test_includes_new_session(self, fleet_home_tmp: Path) -> None:
        ts = datetime(2026, 4, 28, 12, 34, 56, tzinfo=timezone.utc)
        ok = handoff.write_queue(
            old_id="aaaa1111", new_id="bbbb2222",
            doc_path="/tmp/doc.md", project="proj", task_id="t",
            ts=ts,
        )
        assert ok is True
        path = fleet_home_tmp / "queue" / "spawn-fresh-aaaa1111.json"
        q = json.loads(path.read_text(encoding="utf-8"))
        assert q["new_agent_id"] == "bbbb2222"
        assert q["new_session"] == "fleet-bbbb2222"
        assert q["enqueued_at"] == "2026-04-28T12:34:56Z"
