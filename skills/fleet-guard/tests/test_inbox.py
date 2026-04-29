"""Tests for skills/fleet-guard/inbox.py.

Inbox is the simplest module — one-shot relay with archival. Tests cover the
ordering contract (read does not consume; archive moves), the timestamp
collision guard, and the failure-is-non-fatal contract.
"""
from __future__ import annotations

import re
from pathlib import Path

import pytest

import inbox


@pytest.fixture(autouse=True)
def fleet_home_tmp(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> Path:
    home = tmp_path / "fleet"
    monkeypatch.setenv("FLEET_HOME", str(home))
    return home


def _write_inbox(home: Path, agent_id: str, content: str) -> Path:
    path = home / "inbox" / f"{agent_id}.md"
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(content, encoding="utf-8")
    return path


# -- read_pending ------------------------------------------------------------

class TestReadPending:
    def test_returns_content(self, fleet_home_tmp: Path) -> None:
        _write_inbox(fleet_home_tmp, "agent01", "tighten the loop on retries")
        assert inbox.read_pending("agent01") == "tighten the loop on retries"

    def test_returns_none_if_absent(self, fleet_home_tmp: Path) -> None:
        assert inbox.read_pending("missing") is None

    def test_does_not_consume(self, fleet_home_tmp: Path) -> None:
        # The orchestrator (main.py) must be free to read first, decide
        # whether to deliver, and only then archive. read_pending leaves
        # the file in place.
        path = _write_inbox(fleet_home_tmp, "agent02", "hi")
        inbox.read_pending("agent02")
        inbox.read_pending("agent02")
        assert path.exists()


# -- deliver -----------------------------------------------------------------

class TestDeliver:
    def test_wraps_with_operator_marker(self) -> None:
        assert inbox.deliver("merge main") == "[OPERATOR] merge main"

    def test_strips_trailing_whitespace_only(self) -> None:
        # Trailing newlines from `fleet message ... > inbox.md` would
        # otherwise inflate the injection. Strip only trailing whitespace,
        # preserve internal structure.
        assert inbox.deliver("line1\nline2\n\n") == "[OPERATOR] line1\nline2"


# -- archive -----------------------------------------------------------------

class TestArchive:
    def test_moves_to_archive(self, fleet_home_tmp: Path) -> None:
        path = _write_inbox(fleet_home_tmp, "agent03", "hello")
        ok = inbox.archive("agent03")
        assert ok is True
        assert not path.exists()
        archives = list((fleet_home_tmp / "inbox" / "archive").glob("agent03-*.md"))
        assert len(archives) == 1
        assert archives[0].read_text(encoding="utf-8") == "hello"

    def test_filename_has_utc_timestamp(self, fleet_home_tmp: Path) -> None:
        _write_inbox(fleet_home_tmp, "agent04", "x")
        inbox.archive("agent04")
        archives = list((fleet_home_tmp / "inbox" / "archive").iterdir())
        assert len(archives) == 1
        # Format: <id>-<YYYYMMDD>-<HHMMSS>Z[-<rand>].md
        assert re.match(r"agent04-\d{8}-\d{6}Z(?:-[0-9a-f]{4})?\.md$",
                        archives[0].name)

    def test_returns_false_when_no_inbox_file(self, fleet_home_tmp: Path) -> None:
        assert inbox.archive("missing") is False

    def test_creates_archive_dir_if_missing(self, fleet_home_tmp: Path) -> None:
        _write_inbox(fleet_home_tmp, "agent05", "x")
        # No archive dir yet — first archive must create it.
        assert not (fleet_home_tmp / "inbox" / "archive").exists()
        ok = inbox.archive("agent05")
        assert ok is True
        assert (fleet_home_tmp / "inbox" / "archive").exists()

    def test_collision_appends_random_suffix(
        self, fleet_home_tmp: Path, monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        """Two archives in the same UTC second must both survive — the
        operator's audit trail can't lose a message because the clock didn't
        tick. Frozen time forces the collision to fire deterministically."""
        from datetime import datetime, timezone
        frozen = datetime(2026, 4, 28, 12, 0, 0, tzinfo=timezone.utc)

        class _Frozen:
            @staticmethod
            def now(tz=None):
                return frozen
        monkeypatch.setattr(inbox, "datetime", _Frozen)

        _write_inbox(fleet_home_tmp, "agent06", "first")
        ok1 = inbox.archive("agent06")
        _write_inbox(fleet_home_tmp, "agent06", "second")
        ok2 = inbox.archive("agent06")
        assert ok1 is True
        assert ok2 is True

        archives = sorted((fleet_home_tmp / "inbox" / "archive").iterdir())
        assert len(archives) == 2
        bodies = sorted(p.read_text(encoding="utf-8") for p in archives)
        assert bodies == ["first", "second"]
