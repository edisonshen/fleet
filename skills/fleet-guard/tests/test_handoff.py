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
from datetime import datetime, timedelta, timezone
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

    def test_capture_requests_scrollback(
        self, fake_tmux: _FakeTmux,
    ) -> None:
        """find_milestone must capture WITH scrollback (-S -<N>), not
        just the visible pane window. Long-running Yellow cycles (the
        agent emits many turns of output between HANDOFF REQUESTED
        injection and finally writing MILESTONE) outrun the visible
        ~50 lines, so without scrollback both markers scroll off and
        the trigger never fires. Regression for the Apr 2026 case
        where agent 89ebf034 sat at 64% / auto-yellow without
        handing off."""
        fake_tmux.output = (
            f"{handoff.HANDOFF_REQUESTED}: context window over 50%\n"
            "MILESTONE\n"
        )
        handoff.find_milestone("fleet-abc")
        assert len(fake_tmux.calls) >= 1, "tmux capture-pane never invoked"
        cmd = fake_tmux.calls[-1]
        # Scrollback request: `-S -<N>` for some N >= 1.
        assert "-S" in cmd, f"capture missing scrollback flag: {cmd}"
        s_idx = cmd.index("-S")
        assert s_idx + 1 < len(cmd), f"-S without value: {cmd}"
        scroll_arg = cmd[s_idx + 1]
        assert scroll_arg.startswith("-"), \
            f"-S value should be negative line count, got {scroll_arg!r}"
        n = int(scroll_arg[1:])  # raises if non-numeric
        assert n >= 1000, f"scrollback {n} too small to cover busy agent"

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

class TestDetectQuestion:
    """Heuristic to split asking/idle. False positives (rhetorical "?")
    and false negatives ("Let me know if X.") are both expected; these
    tests pin the patterns we DO want to catch + a few we explicitly
    don't, so the heuristic doesn't silently regress."""

    def test_empty_input_is_idle(self) -> None:
        assert handoff.detect_question("") is False

    def test_trailing_question_mark_is_asking(self) -> None:
        pane = "Working on it.\n\nDo you want me to continue?"
        assert handoff.detect_question(pane) is True

    def test_question_opener_without_qmark_is_asking(self) -> None:
        pane = "Build is green.\nShould I open the PR now"
        assert handoff.detect_question(pane) is True

    def test_yn_widget_is_asking(self) -> None:
        pane = "Apply this change to 3 files? [y/n]"
        assert handoff.detect_question(pane) is True

    def test_plain_status_line_is_idle(self) -> None:
        pane = "Done. 5 tests pass.\nAll green."
        assert handoff.detect_question(pane) is False

    def test_question_buried_far_back_in_pane_is_idle(self) -> None:
        """Old answered question in scrollback must not pin the agent
        as 'asking'. Only the last few non-empty lines matter."""
        pane = "Should I proceed?\n" + "\n".join(["work " + str(i) for i in range(20)]) + "\nDone."
        assert handoff.detect_question(pane) is False

    def test_trailing_blank_lines_dont_hide_question(self) -> None:
        pane = "Plan ready.\nReady to apply?\n\n\n  \n"
        assert handoff.detect_question(pane) is True

    def test_bullet_prefix_doesnt_hide_opener(self) -> None:
        pane = "- Should I push to main"
        assert handoff.detect_question(pane) is True

    def test_case_insensitive_opener(self) -> None:
        pane = "DO YOU want this merged"
        assert handoff.detect_question(pane) is True


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
        # handoff_type_at must be set on the same write so the watchdog
        # has a baseline (otherwise Yellow's very next Stop would treat
        # the just-set yellow_pending as "missing timestamp = stuck"
        # and re-inject every fire).
        assert isinstance(record.get("handoff_type_at"), str)
        assert record["handoff_type_at"].endswith("Z")
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
        assert q["schema_version"] == 2
        assert q["task_id"] == "demo-task"
        assert q["project"] == "myproj"

    def test_yellow_pending_fresh_timestamp_noops(
        self, fleet_home_tmp: Path, fake_tmux: _FakeTmux, tmp_path: Path,
    ) -> None:
        """Within the watchdog threshold, yellow_pending without MILESTONE
        is a silent noop — agent has been recently nudged and may still
        be wrapping. Re-injecting on every Stop here would spam the
        agent and waste turns."""
        _seed_record(fleet_home_tmp, "agent05",
                     handoff_type=handoff.TYPE_AUTO_YELLOW,
                     handoff_type_at=health.now_rfc3339())
        fake_tmux.output = "still working...\n"
        result = handoff.maybe_trigger(
            {"transcript_path": str(_transcript(tmp_path, input_tokens=110_000))},
            agent_id="agent05", session="fleet-agent05",
        )
        assert result is None
        assert not (fleet_home_tmp / "handoffs").exists() or \
            list((fleet_home_tmp / "handoffs").iterdir()) == []

    def test_yellow_pending_stuck_too_long_reinjects(
        self, fleet_home_tmp: Path, fake_tmux: _FakeTmux, tmp_path: Path,
    ) -> None:
        """Watchdog: if yellow_pending lingers past the threshold without
        MILESTONE, re-inject HANDOFF REQUESTED so the agent gets a fresh
        nudge. The original injection may have been lost (pre-v0.1.1
        stdout-only Stop output) or ignored. Without this, the agent
        stays wedged until Red at 70% or operator [h]."""
        stale = (datetime.now(timezone.utc)
                 - timedelta(seconds=handoff._YELLOW_RESEND_THRESHOLD_SEC + 60)
                 ).strftime("%Y-%m-%dT%H:%M:%SZ")
        record_path = _seed_record(fleet_home_tmp, "stuck1",
                                   handoff_type=handoff.TYPE_AUTO_YELLOW,
                                   handoff_type_at=stale)
        fake_tmux.output = "still working...\n"
        result = handoff.maybe_trigger(
            {"transcript_path": str(_transcript(tmp_path, input_tokens=110_000))},
            agent_id="stuck1", session="fleet-stuck1",
        )
        assert result is not None, "watchdog must re-inject when stuck"
        assert handoff.HANDOFF_REQUESTED in result
        # Timestamp is bumped so we don't re-inject every Stop until
        # the next threshold window elapses.
        record = json.loads(record_path.read_text(encoding="utf-8"))
        assert record["handoff_type"] == handoff.TYPE_AUTO_YELLOW
        assert record["handoff_type_at"] != stale
        # Still no doc/queue — only HANDOFF REQUESTED nudge.
        assert not (fleet_home_tmp / "handoffs").exists() or \
            list((fleet_home_tmp / "handoffs").iterdir()) == []

    def test_yellow_pending_missing_timestamp_reinjects(
        self, fleet_home_tmp: Path, fake_tmux: _FakeTmux, tmp_path: Path,
    ) -> None:
        """Legacy migration: a record stuck under a pre-watchdog skill
        has handoff_type=auto-yellow but no handoff_type_at. Treat
        missing timestamp as 'very stale' so the operator who upgrades
        explicitly to fix this bug sees recovery on the very next Stop,
        not after another full threshold window."""
        record_path = _seed_record(fleet_home_tmp, "legacy1",
                                   handoff_type=handoff.TYPE_AUTO_YELLOW)
        # Sanity: the seed truly omits handoff_type_at — if a future
        # _seed_record default backfills it, this regression test
        # silently degrades.
        seeded = json.loads(record_path.read_text(encoding="utf-8"))
        assert "handoff_type_at" not in seeded
        fake_tmux.output = "still working...\n"
        result = handoff.maybe_trigger(
            {"transcript_path": str(_transcript(tmp_path, input_tokens=110_000))},
            agent_id="legacy1", session="fleet-legacy1",
        )
        assert result is not None
        assert handoff.HANDOFF_REQUESTED in result
        record = json.loads(record_path.read_text(encoding="utf-8"))
        assert isinstance(record.get("handoff_type_at"), str)

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


# -- Yellow → Red escalation safety net (codex iter-3 P1) ------------------

class TestYellowToRedEscalation:
    """SKILL.md and DESIGN.md guarantee Red is the safety net when an
    agent ignores or misses MILESTONE after HANDOFF REQUESTED. The
    pending check on Red used to suppress this — auto-yellow counted
    as 'pending' so Red bailed and the agent ran past 70% indefinitely
    with no successor. Codex iter-3 caught it; these tests pin the fix."""

    def test_red_escalates_when_yellow_pending(
        self, fleet_home_tmp: Path, fake_tmux: _FakeTmux, tmp_path: Path,
    ) -> None:
        """Agent in auto-yellow (HANDOFF REQUESTED out, no MILESTONE
        emitted) crossing 70% MUST write the auto-red emergency
        handoff."""
        record_path = _seed_record(fleet_home_tmp, "esc1",
                                   handoff_type=handoff.TYPE_AUTO_YELLOW)
        # Pane has the injection but no MILESTONE — agent ignored it.
        fake_tmux.output = (
            f"{handoff.HANDOFF_REQUESTED}: wrap up\n"
            "agent kept working without writing MILESTONE...\n"
        )
        result = handoff.maybe_trigger(
            {"transcript_path": str(_transcript(tmp_path, input_tokens=145_000))},
            agent_id="esc1", session="fleet-esc1",
        )
        assert result is None  # silent — Red writes the doc, no injection
        docs = list((fleet_home_tmp / "handoffs").glob("esc1-*.md"))
        assert len(docs) == 1, "Red must write doc when escalating from Yellow"
        body = docs[0].read_bytes()
        assert b'handoff_type: "auto-red"' in body, \
            "Escalation doc must use auto-red type"
        # handoff_type on disk now reflects auto-red (overwrote auto-yellow).
        record = json.loads(record_path.read_text(encoding="utf-8"))
        assert record["handoff_type"] == handoff.TYPE_AUTO_RED

    def test_red_bails_only_when_committed(
        self, fleet_home_tmp: Path, fake_tmux: _FakeTmux, tmp_path: Path,
    ) -> None:
        """auto-red and precompact mean the doc + queue already exist;
        re-firing Red would orphan a doc. Both must bail."""
        for committed_type in (handoff.TYPE_AUTO_RED, handoff.TYPE_PRECOMPACT):
            home = fleet_home_tmp  # shared per the autouse fixture
            (home / "handoffs").mkdir(parents=True, exist_ok=True)
            (home / "queue").mkdir(parents=True, exist_ok=True)
            agent_id = f"esc-{committed_type}"
            _seed_record(home, agent_id, handoff_type=committed_type)
            handoff.maybe_trigger(
                {"transcript_path": str(_transcript(tmp_path, input_tokens=145_000))},
                agent_id=agent_id, session=f"fleet-{agent_id}",
            )
            docs = list((home / "handoffs").glob(f"{agent_id}-*.md"))
            assert len(docs) == 0, \
                f"Red must NOT re-write when committed as {committed_type}"

    def test_precompact_escalates_when_yellow_pending(
        self, fleet_home_tmp: Path, fake_tmux: _FakeTmux, tmp_path: Path,
    ) -> None:
        """PreCompact is even more urgent — context is about to be lost.
        An auto-yellow agent must NOT block PreCompact's escalation."""
        record_path = _seed_record(fleet_home_tmp, "escPC1",
                                   handoff_type=handoff.TYPE_AUTO_YELLOW)
        handoff.emergency_trigger(
            {"transcript_path": str(_transcript(tmp_path))},
            agent_id="escPC1", session="fleet-escPC1",
        )
        docs = list((fleet_home_tmp / "handoffs").glob("escPC1-*.md"))
        assert len(docs) == 1
        body = docs[0].read_bytes()
        assert b'handoff_type: "precompact"' in body
        record = json.loads(record_path.read_text(encoding="utf-8"))
        assert record["handoff_type"] == handoff.TYPE_PRECOMPACT

    def test_yellow_noops_when_committed(
        self, fleet_home_tmp: Path, fake_tmux: _FakeTmux, tmp_path: Path,
    ) -> None:
        """An agent that already had Red fire (auto-red committed) but
        whose context bounced back below 70% (compaction by host? unlikely
        but possible) must NOT re-enter Yellow's first-fire branch and
        re-inject. Yellow has its own committed bailout."""
        _seed_record(fleet_home_tmp, "ycom1",
                     handoff_type=handoff.TYPE_AUTO_RED)
        result = handoff.maybe_trigger(
            {"transcript_path": str(_transcript(tmp_path, input_tokens=110_000))},
            agent_id="ycom1", session="fleet-ycom1",
        )
        assert result is None, \
            "Yellow must noop when handoff already committed"


# -- wedge protection: clear pending on producer failure (codex P1) -------

class TestWedgeRecovery:
    """If write_doc or write_queue fails after handoff_type was pre-marked,
    the rollback in _clear_pending must restore handoff_type=None so the
    next fire retries instead of being silently wedged in pending state.
    Without rollback, is_handoff_pending stays True and Red short-circuits
    forever — agent is stuck until manual repair."""

    def _seed_and_force_doc_failure(
        self, fleet_home_tmp: Path, agent_id: str,
        monkeypatch: pytest.MonkeyPatch,
    ) -> Path:
        record_path = _seed_record(fleet_home_tmp, agent_id)
        # Make write_doc return "" (failure sentinel from the function).
        monkeypatch.setattr(handoff, "write_doc", lambda **_: "")
        return record_path

    def test_red_failure_clears_handoff_type(
        self, fleet_home_tmp: Path, fake_tmux: _FakeTmux, tmp_path: Path,
        monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        record_path = self._seed_and_force_doc_failure(
            fleet_home_tmp, "wedgeR1", monkeypatch)
        handoff.maybe_trigger(
            {"transcript_path": str(_transcript(tmp_path, input_tokens=145_000))},
            agent_id="wedgeR1", session="fleet-wedgeR1",
        )
        # After failure, handoff_type rolled back to None so next fire
        # retries from the top instead of bailing on pending=True.
        record = json.loads(record_path.read_text(encoding="utf-8"))
        assert record["handoff_type"] is None, \
            f"expected rollback to None, got {record['handoff_type']!r}"

    def test_red_retries_after_failure(
        self, fleet_home_tmp: Path, fake_tmux: _FakeTmux, tmp_path: Path,
        monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        """End-to-end on the wedge: first fire fails the doc write,
        second fire (after the failure-induced clear) must NOT short-
        circuit — it has to write fresh doc + queue."""
        _seed_record(fleet_home_tmp, "wedgeR2")

        real_write_doc = handoff.write_doc
        call_count = {"n": 0}

        def write_doc_first_fails(**kwargs):
            call_count["n"] += 1
            if call_count["n"] == 1:
                return ""  # fail
            return real_write_doc(**kwargs)
        monkeypatch.setattr(handoff, "write_doc", write_doc_first_fails)

        payload = {"transcript_path": str(
            _transcript(tmp_path, input_tokens=145_000))}
        handoff.maybe_trigger(
            payload, agent_id="wedgeR2", session="fleet-wedgeR2")
        handoff.maybe_trigger(
            payload, agent_id="wedgeR2", session="fleet-wedgeR2")

        assert call_count["n"] == 2, "second fire must retry, not bail"
        docs = list((fleet_home_tmp / "handoffs").glob("wedgeR2-*.md"))
        assert len(docs) == 1, "second fire's retry must produce a doc"
        queues = list((fleet_home_tmp / "queue").glob("spawn-fresh-*.json"))
        assert len(queues) == 1

    def test_precompact_failure_clears_handoff_type(
        self, fleet_home_tmp: Path, fake_tmux: _FakeTmux, tmp_path: Path,
        monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        """PreCompact's wedge case is the worst — compaction is imminent,
        and being stuck means future Yellow fires also bail (pending=True)
        with no MILESTONE recovery path. Rollback must work here too."""
        record_path = self._seed_and_force_doc_failure(
            fleet_home_tmp, "wedgePC", monkeypatch)
        handoff.emergency_trigger(
            {"transcript_path": str(_transcript(tmp_path))},
            agent_id="wedgePC", session="fleet-wedgePC",
        )
        record = json.loads(record_path.read_text(encoding="utf-8"))
        assert record["handoff_type"] is None

    def test_yellow_milestone_failure_clears_handoff_type(
        self, fleet_home_tmp: Path, fake_tmux: _FakeTmux, tmp_path: Path,
        monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        """Yellow's MILESTONE-triggered _do_handoff also rolls back on
        failure. Pre-state: agent is auto-yellow pending. _do_handoff
        fails. Post-state: handoff_type=None so the next fire either
        re-injects HANDOFF REQUESTED (Yellow first-fire) or re-checks
        Red threshold; either way, no silent wedge."""
        record_path = _seed_record(fleet_home_tmp, "wedgeY1",
                                   handoff_type=handoff.TYPE_AUTO_YELLOW)
        fake_tmux.output = (
            f"{handoff.HANDOFF_REQUESTED}: wrap up\n"
            "MILESTONE\n"
        )
        monkeypatch.setattr(handoff, "write_doc", lambda **_: "")
        handoff.maybe_trigger(
            {"transcript_path": str(_transcript(tmp_path, input_tokens=110_000))},
            agent_id="wedgeY1", session="fleet-wedgeY1",
        )
        record = json.loads(record_path.read_text(encoding="utf-8"))
        assert record["handoff_type"] is None

    def test_queue_write_failure_also_clears(
        self, fleet_home_tmp: Path, fake_tmux: _FakeTmux, tmp_path: Path,
        monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        """write_doc succeeds but write_queue fails — rollback still
        fires. (Without this, an orphan doc lingers AND the agent is
        wedged.)"""
        record_path = _seed_record(fleet_home_tmp, "wedgeQ1")
        monkeypatch.setattr(handoff, "write_queue",
                            lambda **_: False)
        handoff.maybe_trigger(
            {"transcript_path": str(_transcript(tmp_path, input_tokens=145_000))},
            agent_id="wedgeQ1", session="fleet-wedgeQ1",
        )
        record = json.loads(record_path.read_text(encoding="utf-8"))
        assert record["handoff_type"] is None


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


# -- producer-triggers-consumer (drain auto-kick) ----------------------------

class TestKickDrain:
    """When _do_handoff successfully writes the queue file, fleet-guard
    fires `fleet drain` as a detached subprocess so the handoff completes
    end-to-end without depending on a running TUI watcher / operator
    intervention. Dogfood report: "context >50%, injection happened, but
    no kill, no new instance" on a laptop where fleet TUI wasn't running.
    The TUI's fsnotify watcher is the OLD consumer; this is the producer
    triggering its own consumer."""

    def _mock_drain_calls(self, monkeypatch: pytest.MonkeyPatch) -> list[list[str]]:
        """Capture every Popen invocation so tests can assert on argv.
        Returns a list that gets appended to as Popen is called.

        The PATH branch is what these tests exercise — FLEET_BIN is
        deleted so _kick_drain falls through to shutil.which. Tests
        that want to exercise the FLEET_BIN branch set the env var
        themselves and should not rely on this helper's which() stub
        (or override it)."""
        calls: list[list[str]] = []

        # Sandbox: dev shell may export FLEET_BIN; clear it so the
        # which() branch is what these tests assert against.
        monkeypatch.delenv("FLEET_BIN", raising=False)

        # which() must return a path so _kick_drain doesn't bail early.
        monkeypatch.setattr(handoff.shutil, "which",
                            lambda name: "/usr/local/bin/fleet" if name == "fleet" else None)

        def fake_popen(argv, **kwargs):
            calls.append(argv)
            # Return something Popen-shaped enough to not crash callers
            # if they inspect attrs (we don't, but defensive).
            class _FakeProc:
                pass
            return _FakeProc()

        monkeypatch.setattr(handoff.subprocess, "Popen", fake_popen)
        return calls

    def test_yellow_milestone_kicks_drain(
        self, fleet_home_tmp: Path, fake_tmux: _FakeTmux, tmp_path: Path,
        monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        """Yellow + MILESTONE writes doc + queue → drain auto-kicks."""
        calls = self._mock_drain_calls(monkeypatch)
        _seed_record(fleet_home_tmp, "kick1",
                     handoff_type=handoff.TYPE_AUTO_YELLOW,
                     handoff_type_at=health.now_rfc3339())
        fake_tmux.output = (
            f"{handoff.HANDOFF_REQUESTED}: wrap up\nMILESTONE\n"
        )
        handoff.maybe_trigger(
            {"transcript_path": str(_transcript(tmp_path, input_tokens=110_000))},
            agent_id="kick1", session="fleet-kick1",
        )
        assert len(calls) == 1, f"expected 1 drain kick, got {len(calls)}: {calls}"
        assert calls[0] == ["/usr/local/bin/fleet", "drain"]

    def test_red_kicks_drain(
        self, fleet_home_tmp: Path, fake_tmux: _FakeTmux, tmp_path: Path,
        monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        """Red emergency writes doc + queue → drain auto-kicks."""
        calls = self._mock_drain_calls(monkeypatch)
        _seed_record(fleet_home_tmp, "kick2")
        handoff.maybe_trigger(
            {"transcript_path": str(_transcript(tmp_path, input_tokens=145_000))},
            agent_id="kick2", session="fleet-kick2",
        )
        assert len(calls) == 1
        assert calls[0][-1] == "drain"

    def test_precompact_kicks_drain(
        self, fleet_home_tmp: Path, fake_tmux: _FakeTmux, tmp_path: Path,
        monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        """PreCompact emergency_trigger also writes the queue, so it
        too must kick drain — without this the operator's most urgent
        handoff path (compaction imminent) silently piles up."""
        calls = self._mock_drain_calls(monkeypatch)
        _seed_record(fleet_home_tmp, "kick3")
        handoff.emergency_trigger(
            {"transcript_path": str(_transcript(tmp_path))},
            agent_id="kick3", session="fleet-kick3",
        )
        assert len(calls) == 1
        assert calls[0][-1] == "drain"

    def test_no_kick_when_queue_write_fails(
        self, fleet_home_tmp: Path, fake_tmux: _FakeTmux, tmp_path: Path,
        monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        """If write_queue fails, _do_handoff returns False and the kick
        must NOT fire. Otherwise drain runs against a missing queue
        file and either errors or silently noops — both are wasted
        work and noise."""
        calls = self._mock_drain_calls(monkeypatch)
        _seed_record(fleet_home_tmp, "kick4")
        monkeypatch.setattr(handoff, "write_queue", lambda **_: False)
        handoff.maybe_trigger(
            {"transcript_path": str(_transcript(tmp_path, input_tokens=145_000))},
            agent_id="kick4", session="fleet-kick4",
        )
        assert calls == [], f"drain must not kick on queue-write failure, got: {calls}"

    def test_silent_when_fleet_binary_missing(
        self, fleet_home_tmp: Path, fake_tmux: _FakeTmux, tmp_path: Path,
        monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        """If `fleet` isn't on PATH (rare — operator installed via brew),
        _kick_drain noops silently. The queue file stays on disk and
        any later drain run consumes it. The skill must never raise."""
        # Both resolution sources blank → no Popen call.
        monkeypatch.delenv("FLEET_BIN", raising=False)
        monkeypatch.setattr(handoff.shutil, "which", lambda _name: None)
        popen_calls: list[Any] = []
        monkeypatch.setattr(handoff.subprocess, "Popen",
                            lambda *a, **kw: popen_calls.append(a) or None)
        _seed_record(fleet_home_tmp, "kick5")
        handoff.maybe_trigger(
            {"transcript_path": str(_transcript(tmp_path, input_tokens=145_000))},
            agent_id="kick5", session="fleet-kick5",
        )
        assert popen_calls == [], "Popen must not run when fleet binary is absent"

    def test_fleet_bin_env_preferred_over_path(
        self, fleet_home_tmp: Path, fake_tmux: _FakeTmux, tmp_path: Path,
        monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        """FLEET_BIN (stamped by spawn) wins over `which fleet`. This is
        the codex P1 fix: dev runs / non-PATH installs need the producer
        to invoke the SAME fleet binary that spawned the agent, not
        whatever (or nothing) PATH resolves to."""
        # Create a real executable at a non-PATH location so os.access
        # X_OK passes. Tempfile + chmod is enough — we never run it,
        # subprocess.Popen is faked.
        stamped = tmp_path / "fleet-from-spawn"
        stamped.write_text("#!/bin/sh\nexit 0\n")
        stamped.chmod(0o755)
        monkeypatch.setenv("FLEET_BIN", str(stamped))
        # which() returns a different path so the test fails loudly if
        # _kick_drain falls through to PATH.
        monkeypatch.setattr(handoff.shutil, "which",
                            lambda _name: "/should/not/be/used")
        calls: list[list[str]] = []
        monkeypatch.setattr(handoff.subprocess, "Popen",
                            lambda argv, **_kw: calls.append(argv) or object())

        _seed_record(fleet_home_tmp, "kickbin1")
        handoff.maybe_trigger(
            {"transcript_path": str(_transcript(tmp_path, input_tokens=145_000))},
            agent_id="kickbin1", session="fleet-kickbin1",
        )
        assert calls == [[str(stamped), "drain"]]

    def test_fleet_bin_falls_back_when_path_missing(
        self, fleet_home_tmp: Path, fake_tmux: _FakeTmux, tmp_path: Path,
        monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        """If FLEET_BIN points at a path that no longer exists (e.g.
        `go run` temp build evaporated, or operator deleted a side-loaded
        binary mid-session), _kick_drain falls back to `which fleet`.
        Without this fallback the producer-trigger silently breaks the
        moment the stamped path becomes stale."""
        ghost = tmp_path / "fleet-was-here"  # never created
        monkeypatch.setenv("FLEET_BIN", str(ghost))
        monkeypatch.setattr(handoff.shutil, "which",
                            lambda name: "/usr/local/bin/fleet"
                            if name == "fleet" else None)
        calls: list[list[str]] = []
        monkeypatch.setattr(handoff.subprocess, "Popen",
                            lambda argv, **_kw: calls.append(argv) or object())

        _seed_record(fleet_home_tmp, "kickbin2")
        handoff.maybe_trigger(
            {"transcript_path": str(_transcript(tmp_path, input_tokens=145_000))},
            agent_id="kickbin2", session="fleet-kickbin2",
        )
        assert calls == [["/usr/local/bin/fleet", "drain"]]

    def test_fleet_bin_falls_back_when_not_executable(
        self, fleet_home_tmp: Path, fake_tmux: _FakeTmux, tmp_path: Path,
        monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        """FLEET_BIN points at a real file that lost its execute bit
        (rare: filesystem perms drift, partial brew install). Falling
        back to PATH is safer than running an un-executable file
        — Popen would raise PermissionError and the queue would still
        get drained, but we'd have spent the producer's exception
        budget on something the fallback handles cleanly."""
        no_exec = tmp_path / "fleet-no-exec"
        no_exec.write_text("not executable")
        no_exec.chmod(0o644)
        monkeypatch.setenv("FLEET_BIN", str(no_exec))
        monkeypatch.setattr(handoff.shutil, "which",
                            lambda name: "/usr/local/bin/fleet"
                            if name == "fleet" else None)
        calls: list[list[str]] = []
        monkeypatch.setattr(handoff.subprocess, "Popen",
                            lambda argv, **_kw: calls.append(argv) or object())

        _seed_record(fleet_home_tmp, "kickbin3")
        handoff.maybe_trigger(
            {"transcript_path": str(_transcript(tmp_path, input_tokens=145_000))},
            agent_id="kickbin3", session="fleet-kickbin3",
        )
        assert calls == [["/usr/local/bin/fleet", "drain"]]

    def test_popen_failure_is_swallowed(
        self, fleet_home_tmp: Path, fake_tmux: _FakeTmux, tmp_path: Path,
        monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        """Popen raising (rare: kernel resource limits, transient sandbox
        errors) must not propagate — the producer's job is done as soon
        as the queue file lands. Drain failures are recoverable; skill
        crashes are not (host turn blocks)."""
        monkeypatch.delenv("FLEET_BIN", raising=False)
        monkeypatch.setattr(handoff.shutil, "which",
                            lambda _name: "/usr/local/bin/fleet")

        def boom(*_a, **_kw):
            raise OSError("simulated Popen failure")
        monkeypatch.setattr(handoff.subprocess, "Popen", boom)

        _seed_record(fleet_home_tmp, "kick6")
        # Should not raise; record the queue file is what matters.
        handoff.maybe_trigger(
            {"transcript_path": str(_transcript(tmp_path, input_tokens=145_000))},
            agent_id="kick6", session="fleet-kick6",
        )
        # Queue file landed despite drain kick failure.
        queues = list((fleet_home_tmp / "queue").glob("spawn-fresh-*.json"))
        assert len(queues) == 1


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
