"""Tests for skills/fleet-guard/jetsam.py — the macOS memorystatus /
jetsam observer parser + incident writer.

Covers PART 3 of the bundled liveness-correctness fix (2026-05-13):
when the kernel kills claude under memory pressure, fleet today has
zero diagnostic record. The parser + writer give fleet-guard a way
to turn an OOM kill into a structured incident the TUI can render."""
from __future__ import annotations

import json
from pathlib import Path

import pytest

import jetsam


@pytest.fixture(autouse=True)
def fleet_home_tmp(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> Path:
    """Redirect ~/.fleet/incidents/ to a per-test tmpdir."""
    home = tmp_path / "fleet"
    monkeypatch.setenv("FLEET_HOME", str(home))
    return home


class TestParseJetsamLine:
    """macOS log lines come in several shapes — kernel revisions tweak
    the wording over time. We pin parsing for the three reason-bucket
    shapes called out in the task spec (highwater / vm-thrashing /
    fork-fail) plus the structural variants the kernel emits."""

    def test_highwater_bracketed_reason(self) -> None:
        line = "2026-05-13 17:20:01.123456-0700  localhost kernel[0]: memorystatus_thread: idle: killing pid 8306 [highwater]"
        got = jetsam.parse_jetsam_line(line)
        assert got == (8306, "highwater")

    def test_vm_thrashing_tail_reason(self) -> None:
        line = "2026-05-13 17:20:01.999999-0700  localhost kernel[0]: jetsam_kill_proc: killed pid 9421 reason: vm-thrashing"
        got = jetsam.parse_jetsam_line(line)
        assert got == (9421, "vm-thrashing")

    def test_fork_fail_bracketed(self) -> None:
        line = "2026-05-13 17:21:02-0700  localhost kernel[0]: memorystatus_thread: killing pid 1234 [fork-fail]"
        got = jetsam.parse_jetsam_line(line)
        assert got == (1234, "fork-fail")

    def test_no_pid_returns_none(self) -> None:
        """memory-pressure advisory without a pid (the kernel logs
        these alongside actual kills) → None. Not every kill-marker
        line carries a pid."""
        line = "2026-05-13 17:20:01-0700  kernel[0]: memorystatus_thread: killing pid (none) [pressure]"
        got = jetsam.parse_jetsam_line(line)
        assert got is None

    def test_non_kill_memorystatus_returns_none(self) -> None:
        """Non-kill memorystatus events (status changes, idle exits)
        share the 'memorystatus' substring but don't carry a kill
        marker → must not produce an incident."""
        line = "2026-05-13 17:20:01-0700  kernel[0]: memorystatus: state changed pid 8306 to suspended"
        got = jetsam.parse_jetsam_line(line)
        assert got is None

    def test_unrelated_log_returns_none(self) -> None:
        line = "2026-05-13 17:20:01-0700  kernel[0]: WiFi: scan results 12 networks"
        got = jetsam.parse_jetsam_line(line)
        assert got is None

    def test_unknown_reason_defaults_to_unknown(self) -> None:
        """Kill line with a pid but no parseable reason → reason='unknown'.
        We still record the incident — the pid is the load-bearing field."""
        line = "kernel[0]: jetsam_kill: killed pid 5555"
        got = jetsam.parse_jetsam_line(line)
        assert got == (5555, "unknown")

    def test_non_string_returns_none(self) -> None:
        """Defensive: the live `log stream` tail could in theory hand
        us a bytes object or None on a transport hiccup. Don't crash."""
        assert jetsam.parse_jetsam_line(None) is None  # type: ignore[arg-type]
        assert jetsam.parse_jetsam_line(b"kernel: killing pid 1234") is None  # type: ignore[arg-type]

    def test_pid_zero_or_negative_returns_none(self) -> None:
        """pid=0 is init / kernel-internal; we shouldn't claim that as
        a fleet-agent kill. Negative pids are nonsensical."""
        line = "kernel: killing pid 0 [highwater]"
        assert jetsam.parse_jetsam_line(line) is None


class TestWriteIncident:
    def test_writes_json_with_expected_fields(self, fleet_home_tmp: Path) -> None:
        ok = jetsam.write_incident(
            agent_id="abcd1234",
            pid=8306,
            reason="highwater",
            when="2026-05-13T17:20:01Z",
            project="projects-spark",
            role="coord",
            command=["sh", "-c", "claude --remote-control fleet-coord-abcd1234"],
            last_activity_ts="2026-05-13T17:18:33Z",
        )
        assert ok is True
        # File should land at ~/.fleet/incidents/abcd1234-<safe-when>.json.
        incidents = fleet_home_tmp / "incidents"
        files = list(incidents.iterdir())
        assert len(files) == 1
        record = json.loads(files[0].read_text(encoding="utf-8"))
        assert record["agent_id"] == "abcd1234"
        assert record["pid"] == 8306
        assert record["reason"] == "highwater"
        assert record["killed_at"] == "2026-05-13T17:20:01Z"
        assert record["project"] == "projects-spark"
        assert record["role"] == "coord"
        assert record["command"] == ["sh", "-c", "claude --remote-control fleet-coord-abcd1234"]

    def test_filename_sanitizes_colons(self, fleet_home_tmp: Path) -> None:
        """':' in RFC3339 timestamps breaks on Windows-shared filesystems;
        the writer replaces ':' with '-' so the file round-trips."""
        ok = jetsam.write_incident(
            agent_id="ag1",
            pid=100,
            reason="vm-thrashing",
            when="2026-05-13T17:20:01Z",
        )
        assert ok is True
        files = list((fleet_home_tmp / "incidents").iterdir())
        assert len(files) == 1
        assert ":" not in files[0].name

    def test_no_tempfile_left_behind(self, fleet_home_tmp: Path) -> None:
        ok = jetsam.write_incident("a", 1, "r", "2026-01-01T00-00-00Z")
        assert ok is True
        files = [p for p in (fleet_home_tmp / "incidents").iterdir()]
        # Only the canonical file, no leftover .tmp.
        for f in files:
            assert not f.name.endswith(".tmp")
            assert ".tmp" not in f.name


class TestListIncidentsForProject:
    def test_returns_matching_incidents(self, fleet_home_tmp: Path) -> None:
        jetsam.write_incident("a1", 1, "r", "2026-01-01T00-00-00Z",
                              project="proj1")
        jetsam.write_incident("a2", 2, "r", "2026-01-01T00-00-01Z",
                              project="proj2")
        jetsam.write_incident("a3", 3, "r", "2026-01-01T00-00-02Z",
                              project="proj1")
        got = jetsam.list_incidents_for_project("proj1")
        assert len(got) == 2
        ids = sorted(r["agent_id"] for r in got)
        assert ids == ["a1", "a3"]

    def test_missing_dir_returns_empty(self, fleet_home_tmp: Path) -> None:
        # Don't write any incidents; the dir doesn't exist.
        got = jetsam.list_incidents_for_project("anything")
        assert got == []

    def test_skips_unparseable_json(self, fleet_home_tmp: Path) -> None:
        dir_ = fleet_home_tmp / "incidents"
        dir_.mkdir(parents=True, exist_ok=True)
        (dir_ / "garbage.json").write_text("{ not json", encoding="utf-8")
        jetsam.write_incident("good", 1, "r", "2026-01-01T00-00-00Z",
                              project="p")
        got = jetsam.list_incidents_for_project("p")
        assert len(got) == 1
        assert got[0]["agent_id"] == "good"
