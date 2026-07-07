"""Unit tests for loop._read_coord_lease_identity — the Python↔Go seam that
REPLACED the deleted coord-spawn marker as the coordinator-identity reader
(TASK-PLAN-coord-lease-sole-identity D3/D5).

gate 5 of _classify_lock_busy uses this to decide "never self-exit when the
lease names me". Production shells out to `fleet coord-owner --project <p>
--json`; the rest of the suite stubs the seam (_coord_lease_identity_fn) so it
never shells out. THESE tests exercise the real reader against a faked
subprocess so the JSON-key contract with the Go CLI and every fail-soft branch
are actually guarded — otherwise a Go-side key rename or a broken parse would
silently make gate 5 fall through and let a legitimate successor coord self-exit.
"""
from __future__ import annotations

import subprocess
from pathlib import Path

import loop


def _fake_run(monkeypatch, *, returncode=0, stdout="", raises=None):
    """Patch loop.subprocess.run to return a canned CompletedProcess (or raise).

    Records the argv + env it was called with so tests can assert the CLI
    contract (subcommand, --project, --json) and the FLEET_HOME invariant.
    """
    calls = {}

    def _run(argv, **kwargs):
        calls["argv"] = argv
        calls["env"] = kwargs.get("env")
        calls["timeout"] = kwargs.get("timeout")
        if raises is not None:
            raise raises
        return subprocess.CompletedProcess(
            args=argv, returncode=returncode, stdout=stdout, stderr="",
        )

    monkeypatch.setattr(loop.subprocess, "run", _run)
    return calls


def test_parses_live_owner_and_successor(monkeypatch, tmp_path):
    """Happy path: the exact JSON keys the Go `coord-owner --json` emits parse
    into (live_owner_id, handoff_successor_id). Locks the cross-language key
    contract — rename either side and this fails."""
    _fake_run(
        monkeypatch,
        stdout='{"project":"p","live_owner_id":"coord-a","owner_id":"coord-a",'
        '"handoff_successor_id":"coord-b"}',
    )
    assert loop._read_coord_lease_identity("p", home=tmp_path) == ("coord-a", "coord-b")


def test_empty_fields_return_empty(monkeypatch, tmp_path):
    """Free lease / no reservation: all-empty JSON -> ("", "")."""
    _fake_run(
        monkeypatch,
        stdout='{"project":"p","live_owner_id":"","owner_id":"","handoff_successor_id":""}',
    )
    assert loop._read_coord_lease_identity("p", home=tmp_path) == ("", "")


def test_missing_keys_return_empty(monkeypatch, tmp_path):
    """A JSON object lacking the id keys (older/partial output) -> ("", ""),
    never a KeyError."""
    _fake_run(monkeypatch, stdout='{"project":"p"}')
    assert loop._read_coord_lease_identity("p", home=tmp_path) == ("", "")


def test_nonzero_returncode_fails_soft(monkeypatch, tmp_path):
    """Binary too old (no coord-owner subcommand -> exit 1), usage fault, or
    internal error: inconclusive -> ("", ""), fall through to the other gates."""
    _fake_run(monkeypatch, returncode=1, stdout="")
    assert loop._read_coord_lease_identity("p", home=tmp_path) == ("", "")


def test_unparseable_stdout_fails_soft(monkeypatch, tmp_path):
    """Garbage on stdout must not crash the tick -> ("", "")."""
    _fake_run(monkeypatch, stdout="not json at all")
    assert loop._read_coord_lease_identity("p", home=tmp_path) == ("", "")


def test_non_dict_json_fails_soft(monkeypatch, tmp_path):
    """Valid JSON that isn't an object (e.g. a list) -> ("", ""), never an
    AttributeError on .get."""
    _fake_run(monkeypatch, stdout="[1, 2, 3]")
    assert loop._read_coord_lease_identity("p", home=tmp_path) == ("", "")


def test_empty_stdout_fails_soft(monkeypatch, tmp_path):
    """Empty stdout (defaults to "{}") -> ("", "")."""
    _fake_run(monkeypatch, stdout="")
    assert loop._read_coord_lease_identity("p", home=tmp_path) == ("", "")


def test_timeout_fails_soft(monkeypatch, tmp_path):
    """A hung binary (TimeoutExpired) must not wedge the tick -> ("", "")."""
    _fake_run(monkeypatch, raises=subprocess.TimeoutExpired(cmd="fleet", timeout=1))
    assert loop._read_coord_lease_identity("p", home=tmp_path) == ("", "")


def test_oserror_fails_soft(monkeypatch, tmp_path):
    """Binary missing / not executable (OSError) -> ("", "")."""
    _fake_run(monkeypatch, raises=OSError("no such file"))
    assert loop._read_coord_lease_identity("p", home=tmp_path) == ("", "")


def test_invokes_coord_owner_json_with_fleet_home(monkeypatch, tmp_path):
    """CLI contract + the load-bearing invariant: the reader must call
    `<bin> coord-owner --project <p> --json` with FLEET_HOME pointed at the
    coord's home, so gate 5 reads the SAME lease the coord writes (a mismatched
    home would silently name nobody and mis-fire self-exit)."""
    calls = _fake_run(monkeypatch, stdout='{"live_owner_id":"c","handoff_successor_id":""}')
    loop._read_coord_lease_identity("myproj", home=tmp_path, fleet_bin="/opt/fleet")
    argv = calls["argv"]
    assert argv[0] == "/opt/fleet"
    assert argv[1] == "coord-owner"
    assert "--project" in argv and argv[argv.index("--project") + 1] == "myproj"
    assert "--json" in argv
    assert calls["env"]["FLEET_HOME"] == str(tmp_path)


def test_fleet_bin_resolves_from_env(monkeypatch, tmp_path):
    """When fleet_bin defaults to 'fleet', prefer the spawn-stamped FLEET_BIN
    (same resolution the lease-check path uses) over a bare PATH lookup."""
    monkeypatch.setenv("FLEET_BIN", "/spawn/stamped/fleet")
    calls = _fake_run(monkeypatch, stdout='{"live_owner_id":"c","handoff_successor_id":""}')
    loop._read_coord_lease_identity("p", home=tmp_path)
    assert calls["argv"][0] == "/spawn/stamped/fleet"
