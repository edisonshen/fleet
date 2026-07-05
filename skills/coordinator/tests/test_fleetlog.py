"""Unit tests for the Python fleetlog emitter (skills/coordinator/fleetlog.py).

Mirror of the Go internal/fleetlog tests: envelope schema, data cap + raw
values, per-process file naming, raw os.write under a fork stress test,
XDG-aware dir, and retention (direct prune + once/day throttle).

All tests set FLEET_HOME and CLEAR XDG_STATE_HOME (an ambient value would
silently redirect dir()).
"""
from __future__ import annotations

import importlib
import json
import os
import re
import time
from pathlib import Path

import pytest


@pytest.fixture
def flog(tmp_path: Path, monkeypatch):
    """Fresh fleetlog module pointed at a tmp FLEET_HOME, XDG cleared.

    Re-import so the module-level per-process identity is captured fresh
    per test (seq counter starts at 0)."""
    monkeypatch.setenv("FLEET_HOME", str(tmp_path / "fleet"))
    monkeypatch.delenv("XDG_STATE_HOME", raising=False)
    import fleetlog
    importlib.reload(fleetlog)
    return fleetlog


def _read_lines(d: Path):
    out = []
    for f in sorted(d.glob("*.jsonl")):
        for ln in f.read_text().splitlines():
            if ln:
                out.append((f.name, json.loads(ln)))  # json.loads => valid JSON
    return out


_NAME_RE = re.compile(r"^fleet-\d{4}-\d{2}-\d{2}-\w+-\d+-\d+\.jsonl$")
_TS_RE = re.compile(r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{6}Z$")


def test_log_writes_envelope_to_own_file(flog):
    d = Path(flog.dir())
    eid = flog.log(
        flog.COMP_COORD, "coord.tick", "info",
        proj="projects-fleet", agent="abcdef01",
        msg="coord tick start", data={"cap": 1},
    )
    assert eid
    lines = _read_lines(d)
    assert len(lines) == 1
    name, m = lines[0]
    assert _NAME_RE.match(name), name
    assert "coord" in name
    # required envelope keys + types
    for k in ("ts", "seq", "type", "lvl", "comp", "pid", "id"):
        assert k in m, k
    assert _TS_RE.match(m["ts"]), m["ts"]
    assert m["type"] == "coord.tick" and m["type"] in flog.TYPES
    assert m["comp"] == "coord" and m["proj"] == "projects-fleet"
    assert m["id"] == eid and eid
    assert m["data"] == {"cap": 1}


def test_log_omits_absent_correlation_keys(flog):
    d = Path(flog.dir())
    flog.log(flog.COMP_WORKER, "state.transition", "info", msg="x")
    _, m = _read_lines(d)[0]
    # absent keys must NOT appear (omitempty parity with Go)
    for absent in ("proj", "agent", "slug", "pr", "session", "dispatch_id",
                   "caused_by", "data", "gen"):
        assert absent not in m, absent


def test_data_cap_and_raw_values(flog):
    d = Path(flog.dir())
    blob = "A" * 3000
    tok = "ghp_EXAMPLETOKEN0123456789"
    flog.log(flog.COMP_COORD, "decision", "info",
             msg="chose X over Y", data={"blob": blob, "tok": tok})
    _, m = _read_lines(d)[0]
    got_blob = m["data"]["blob"]
    assert got_blob.endswith(flog._ELISION)
    assert len(got_blob) <= flog._DATA_CAP + len(flog._ELISION)
    # token logged VERBATIM (no scrub — pins "log raw")
    assert m["data"]["tok"] == tok


def test_data_cap_string_utf8_bytes(flog):
    """String cap uses UTF-8 byte length, not character count. A CJK string
    with ≤ 2048 characters but > 2048 UTF-8 bytes must be truncated."""
    d = Path(flog.dir())
    # Each CJK character is 3 bytes; 700 chars = 2100 bytes > _DATA_CAP.
    cjk_str = "中" * 700  # 700 chars × 3 bytes = 2100 UTF-8 bytes
    assert len(cjk_str) < flog._DATA_CAP  # character count below cap
    assert len(cjk_str.encode("utf-8")) > flog._DATA_CAP  # but bytes exceed cap
    flog.log(flog.COMP_COORD, "decision", "info",
             data={"cjk": cjk_str})
    _, m = _read_lines(d)[0]
    got = m["data"]["cjk"]
    assert got.endswith(flog._ELISION), f"missing elision marker: {got!r}"
    assert len(got.encode("utf-8")) <= flog._DATA_CAP + len(flog._ELISION.encode("utf-8"))


def test_data_cap_non_string(flog):
    """Non-string values (lists, dicts) whose JSON representation exceeds
    _DATA_CAP are replaced with a '<capped: N bytes>' hint."""
    d = Path(flog.dir())
    # Build a list that marshals to > 2 KB.
    big_list = ["x" * 10] * 300  # ~3.6 KB as JSON
    flog.log(flog.COMP_CLI, "cli.start", "info",
             data={"argv": big_list})
    _, m = _read_lines(d)[0]
    argv_val = m["data"]["argv"]
    # Must have been capped — value becomes a "<capped: N bytes>" string.
    assert isinstance(argv_val, str) and argv_val.startswith("<capped:"), (
        f"large list must be capped; got {argv_val!r}"
    )


def test_log_is_compact_single_line(flog):
    d = Path(flog.dir())
    flog.log(flog.COMP_CLI, "cli.start", "info", msg="a", data={"argv": ["drain"]})
    raw = next(Path(flog.dir()).glob("*.jsonl")).read_bytes()
    assert raw.count(b"\n") == 1 and raw.endswith(b"\n")
    # compact: no ", " or ": " separators
    assert b", " not in raw and b'": ' not in raw


def test_log_never_raises_on_unwritable_dir(flog, monkeypatch):
    d = Path(flog.dir())
    d.mkdir(parents=True, exist_ok=True)
    os.chmod(d, 0o500)
    try:
        # must not raise; returns an id regardless
        eid = flog.log(flog.COMP_CLI, "cli.start", "info", msg="x")
        assert eid
        assert list(d.glob("*.jsonl")) == []
    finally:
        os.chmod(d, 0o755)


@pytest.mark.skipif(not hasattr(os, "fork"), reason="fork-only test")
def test_fork_per_process_isolation_no_torn_lines(flog):
    d = Path(flog.dir())
    children = 20
    per_child = 500
    pids = []
    for _ in range(children):
        pid = os.fork()
        if pid == 0:  # child
            try:
                for i in range(per_child):
                    flog.log(flog.COMP_WORKER, "tool.call", "debug",
                             slug="s", data={"i": i})
            finally:
                os._exit(0)
        pids.append(pid)
    for pid in pids:
        os.waitpid(pid, 0)

    files = sorted(d.glob("*.jsonl"))
    # each child re-fingerprints (register_at_fork) -> its own file
    assert len(files) == children, [f.name for f in files]
    total = 0
    for f in files:
        for ln in f.read_text().splitlines():
            if ln:
                json.loads(ln)  # raises if a line was torn/interleaved
                total += 1
    assert total == children * per_child


def test_dir_honors_xdg_state_home(tmp_path, monkeypatch):
    monkeypatch.setenv("FLEET_HOME", str(tmp_path / "fhome"))
    monkeypatch.setenv("XDG_STATE_HOME", str(tmp_path / "xdg"))
    import fleetlog
    importlib.reload(fleetlog)
    assert fleetlog.dir() == str(tmp_path / "xdg" / "fleet" / "logs")
    monkeypatch.delenv("XDG_STATE_HOME", raising=False)
    importlib.reload(fleetlog)
    assert fleetlog.dir() == str(tmp_path / "fhome" / "logs")


def test_prune_older_than(flog):
    d = Path(flog.dir())
    d.mkdir(parents=True, exist_ok=True)
    stale = d / "fleet-2000-01-01-coord-1-1.jsonl"  # ancient -> prune
    today = d / f"fleet-{time.strftime('%Y-%m-%d')}-coord-1-1.jsonl"
    other = d / "notes.txt"
    for p in (stale, today):
        p.write_text("{}\n")
    other.write_text("x")
    flog.prune_older_than(72 * 3600)
    assert not stale.exists()
    assert today.exists()
    assert other.exists()


def test_maybe_prune_daily_throttled(flog):
    d = Path(flog.dir())
    d.mkdir(parents=True, exist_ok=True)
    stale = d / "fleet-2000-01-01-coord-1-1.jsonl"
    stale.write_text("{}\n")
    marker = d / ".last-prune"
    marker.write_text("")
    # last-prune just now -> throttled, no scan, stale survives
    os.utime(marker, (time.time(), time.time()))
    assert flog.maybe_prune_daily(72 * 3600, throttle_s=24 * 3600) is False
    assert stale.exists()


def test_maybe_prune_daily_runs_after_throttle_window(flog):
    d = Path(flog.dir())
    d.mkdir(parents=True, exist_ok=True)
    stale = d / "fleet-2000-01-01-coord-1-1.jsonl"
    stale.write_text("{}\n")
    marker = d / ".last-prune"
    marker.write_text("")
    # last-prune 25h ago -> window elapsed, prune runs
    old = time.time() - 25 * 3600
    os.utime(marker, (old, old))
    assert flog.maybe_prune_daily(72 * 3600, throttle_s=24 * 3600) is True
    assert not stale.exists()
    # marker refreshed
    assert time.time() - os.stat(marker).st_mtime < 60


def test_maybe_prune_daily_first_run_no_marker(flog):
    d = Path(flog.dir())
    d.mkdir(parents=True, exist_ok=True)
    stale = d / "fleet-2000-01-01-coord-1-1.jsonl"
    stale.write_text("{}\n")
    # no marker yet -> prune runs and creates the marker
    assert flog.maybe_prune_daily(72 * 3600) is True
    assert not stale.exists()
    assert (d / ".last-prune").exists()


def test_coord_quarantine_in_types_mirror(flog):
    """DESIGN-coord-no-auto-kill: coord.quarantine joins the closed
    vocabulary; the Python TYPES set is a declared mirror of the Go set
    and must carry it too."""
    assert "coord.quarantine" in flog.TYPES
    d = Path(flog.dir())
    flog.log(flog.COMP_COORD, "coord.quarantine", "warn", proj="rainier",
             agent="stale1", msg="stale competitor detected (report-only)",
             data={"reason": "stale-competitor", "pid": 4242})
    lines = _read_lines(d)
    assert len(lines) == 1
    _, m = lines[0]
    assert m["type"] == "coord.quarantine" and m["type"] in flog.TYPES
    assert m["data"]["reason"] == "stale-competitor"
