"""Parent-lease ownership fence for loop.tick() — DESIGN-handoff-drain-
storm-leak PR4 (T7 + T30).

Under FLEET_LEASE_FAILOVER (default ON as of PR4) the Go `fleet coord-run`
supervisor that PARENTS the Python tick holds coordinator.flock + heartbeats
the coordinator.epoch fencing record. Before any disk mutation the tick must
prove it still descends from the ACTIVE lease owner — a FENCED old coord
(its parent's lease was stolen by a successor) must abort the tick WITHOUT
writing and self-demote, exactly as the Go *WithLease APIs reject a stale
token. The proof routes through `fleet lease-check` (one Go source of truth)
via the injectable loop._lease_check_fn seam.

  T7  — tick under a proven parent-held lease proceeds (no second lifetime
        lock; mutation allowed).
  T30 — a fenced tick is REFUSED: skipped + self_exit + reason
        "lease-fenced-self-exit"; the coordinator.lock is never written.
"""
from __future__ import annotations

import json
import subprocess
from pathlib import Path

import pytest

import loop
import parse


def _seed_agent_record(home: Path, agent_id: str, project: str) -> None:
    agents_dir = home / "agents"
    agents_dir.mkdir(parents=True, exist_ok=True)
    (agents_dir / f"{agent_id}.json").write_text(json.dumps({
        "id": agent_id, "project": project, "kind": "coord",
    }))


def _minimal_project(home: Path, project: str) -> Path:
    pdir = home / "projects" / project
    pdir.mkdir(parents=True, exist_ok=True)
    (pdir / ".locks").mkdir(exist_ok=True)
    f = parse.File(schema=parse.SCHEMA_VERSION, tasks=[], footer="")
    parse.write(str(pdir / "tasks.md"), f)
    return pdir


@pytest.fixture
def fleet_home(tmp_path: Path) -> Path:
    home = tmp_path / "fleet"
    for sub in ("inbox/archive", "projects", "agents", "queue"):
        (home / sub).mkdir(parents=True, exist_ok=True)
    return home


# ---------- T30: fenced tick is refused ----------


def test_fenced_tick_refuses_without_writing(
    fleet_home: Path, monkeypatch: pytest.MonkeyPatch,
    capsys: pytest.CaptureFixture[str],
) -> None:
    agent_id = "feedface"
    _seed_agent_record(fleet_home, agent_id, project="fleet")
    pdir = _minimal_project(fleet_home, project="fleet")
    lock_path = pdir / ".locks" / "coordinator.lock"
    lock_path.write_text("PRE_EXISTING_OWNER_DO_NOT_CLOBBER")

    # The proof returns "fenced" — a successor stole our parent's lease.
    monkeypatch.setattr(
        loop, "_lease_check_fn",
        lambda project, *, home, fleet_bin="fleet": "fenced",
    )

    result = loop.tick(
        project="fleet", coord_id=agent_id,
        cwd="/tmp/anywhere", fleet_home=str(fleet_home),
    )

    assert result.skipped is True
    assert result.self_exit is True, "a fenced coord must self-demote"
    assert result.reason == "lease-fenced-self-exit", (
        f"reason should pin the fence; got {result.reason!r}"
    )
    # The fence fired BEFORE _try_lock — the lock content is untouched.
    assert lock_path.read_text() == "PRE_EXISTING_OWNER_DO_NOT_CLOBBER", (
        "fence must run BEFORE the coordinator.lock is taken/written"
    )
    err = capsys.readouterr().err
    assert "fenced" in err.lower(), f"stderr must surface the fence; got {err!r}"


# ---------- T7: proven ownership proceeds ----------


def test_proven_tick_proceeds(
    fleet_home: Path, monkeypatch: pytest.MonkeyPatch,
) -> None:
    agent_id = "0wned0wn"
    _seed_agent_record(fleet_home, agent_id, project="fleet")
    _minimal_project(fleet_home, project="fleet")

    proof_calls = []

    def _proven(project, *, home, fleet_bin="fleet"):
        proof_calls.append(project)
        return "owner"

    monkeypatch.setattr(loop, "_lease_check_fn", _proven)

    result = loop.tick(
        project="fleet", coord_id=agent_id,
        cwd="/tmp/anywhere", fleet_home=str(fleet_home),
    )

    # Proven TWICE: once at step 0.5 (before lock), once after lock acquire +
    # repo resolve, immediately before the mutation phase (codex PR4 [P1]).
    assert proof_calls == ["fleet", "fleet"], (
        f"the tick must prove ownership before lock AND before mutating; got {proof_calls}"
    )
    assert result.reason != "lease-fenced-self-exit", (
        f"a proven tick must not self-fence; got {result.reason!r}"
    )
    assert result.self_exit is False


def test_fence_after_lock_acquire_self_demotes(
    fleet_home: Path, monkeypatch: pytest.MonkeyPatch,
) -> None:
    # codex PR4 [P1]: ownership proven at step 0.5 (before lock) but FENCED by
    # the post-lock re-check (a successor took the lease during lock-acquire /
    # repo-resolve). The tick must self-demote WITHOUT entering _tick_locked.
    agent_id = "racecond"
    _seed_agent_record(fleet_home, agent_id, project="fleet")
    _minimal_project(fleet_home, project="fleet")

    calls = {"n": 0}

    def _flip(project, *, home, fleet_bin="fleet"):
        calls["n"] += 1
        return "owner" if calls["n"] == 1 else "fenced"  # 2nd call = fenced

    monkeypatch.setattr(loop, "_lease_check_fn", _flip)

    result = loop.tick(
        project="fleet", coord_id=agent_id,
        cwd="/tmp/anywhere", fleet_home=str(fleet_home),
    )
    assert calls["n"] == 2, "must re-check after lock acquire"
    assert result.self_exit is True
    assert result.reason == "lease-fenced-self-exit"


# ---------- the proof helper maps `fleet lease-check` exit codes ----------


def _fake_completed(returncode: int, stderr: str = ""):
    return subprocess.CompletedProcess(
        args=["fleet", "lease-check"], returncode=returncode,
        stdout="", stderr=stderr,
    )


def test_prove_helper_exit_code_mapping(
    fleet_home: Path, monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv("FLEET_LEASE_FAILOVER", "1")
    # exit 0 -> owner; exit 3 -> fenced; exit 1/2 with a genuine internal
    # error -> FENCED (cannot prove ownership, codex PR4 [P1]).
    cases = {0: "owner", 3: "fenced", 1: "fenced", 2: "fenced"}
    for rc, want in cases.items():
        monkeypatch.setattr(
            loop.subprocess, "run",
            lambda *a, _rc=rc, **k: _fake_completed(_rc, "internal boom"),
        )
        got = loop._prove_parent_lease_ownership(
            "fleet", home=fleet_home, fleet_bin="fleet",
        )
        assert got == want, f"exit {rc} should map to {want!r}, got {got!r}"


def test_prove_helper_too_old_binary_fails_open(
    fleet_home: Path, monkeypatch: pytest.MonkeyPatch,
) -> None:
    # A binary too old to have `lease-check` -> cobra "unknown command" +
    # exit 1 -> fail OPEN ("unknown"), don't wedge a pre-lease coord.
    monkeypatch.setenv("FLEET_LEASE_FAILOVER", "1")
    monkeypatch.setattr(
        loop.subprocess, "run",
        lambda *a, **k: _fake_completed(1, 'unknown command "lease-check" for "fleet"'),
    )
    got = loop._prove_parent_lease_ownership("fleet", home=fleet_home, fleet_bin="fleet")
    assert got == "unknown", "a too-old binary (unknown command) must fail open"


def test_prove_helper_failover_off_is_noop(
    fleet_home: Path, monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv("FLEET_LEASE_FAILOVER", "0")
    called = []
    monkeypatch.setattr(
        loop.subprocess, "run",
        lambda *a, **k: called.append(1) or _fake_completed(0),
    )
    got = loop._prove_parent_lease_ownership(
        "fleet", home=fleet_home, fleet_bin="fleet",
    )
    assert got == "owner"
    assert called == [], "failover off must NOT shell out to lease-check"


def test_prove_helper_prefers_fleet_bin_env(
    fleet_home: Path, monkeypatch: pytest.MonkeyPatch,
) -> None:
    # codex PR4 [P2]: when fleet_bin is the default sentinel, the FLEET_BIN
    # the spawn stamped must be used (not a bare `fleet` on PATH).
    monkeypatch.setenv("FLEET_LEASE_FAILOVER", "1")
    monkeypatch.setenv("FLEET_BIN", "/stamped/fleet")
    seen = {}

    def _capture(cmd, **k):
        seen["bin"] = cmd[0]
        return _fake_completed(0)

    monkeypatch.setattr(loop.subprocess, "run", _capture)
    loop._prove_parent_lease_ownership("fleet", home=fleet_home)  # default fleet_bin
    assert seen["bin"] == "/stamped/fleet", "must invoke the FLEET_BIN-stamped binary"


def test_prove_helper_binary_missing_is_unknown(
    fleet_home: Path, monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv("FLEET_LEASE_FAILOVER", "1")

    def _boom(*a, **k):
        raise FileNotFoundError("no fleet")

    monkeypatch.setattr(loop.subprocess, "run", _boom)
    got = loop._prove_parent_lease_ownership(
        "fleet", home=fleet_home, fleet_bin="fleet",
    )
    assert got == "unknown", "a missing binary must be fail-open, not a fence"
