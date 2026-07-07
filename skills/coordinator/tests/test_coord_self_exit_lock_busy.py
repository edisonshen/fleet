"""Duplicate-coord self-exit on a busy coordinator.lock — coord-self-exit-when-it-6014.

Background (the live failure mode this fixes):

  When a spawned coord runs /coordinator and coordinator.lock is already
  held by ANOTHER live coord, `loop.tick()` historically returned
  skipped=True/reason="lock-busy" and the *tick* exited — but the
  *claude session* lived on idle forever, holding a `fleet-<id>` tmux
  session and doing no work. That zombie is why [h]/[a] mishaps left
  multiple live coords instead of self-healing to one
  (docs/DESIGN-tui-coord-row-lifecycle.md, the 2026-05-28 fengshen-site
  3-coord tangle).

The fix: in the lock-busy branch, a coord that lost the lock to a
DIFFERENT LIVE coord AND is NOT the project's intended coord (per the
coordinator LEASE — D3, the coord-spawn marker is deleted) sets
result.self_exit=True + emits a clear stderr diagnostic
(feedback_surface_dont_silo). `main()` then kills THIS coord's own tmux
session so the duplicate self-heals.

Conservatism gates (each one => skip, never self-exit):
  - no coord_id (manual shell invocation)
  - lock body has no valid holder ID
  - holder == self (own stale flock)
  - lease names self (live active owner OR in-flight handoff successor
    = the mid-handoff guard)
  - holder's agent record missing OR holder's tmux session dead
    (normal takeover, next tick acquires)

Determinism: we monkeypatch supervisor.tmux_session_alive so no test
shells out to tmux, AND stub loop._coord_lease_identity_fn (autouse
fixture) so no test shells out to the real `fleet coord-owner` binary;
the lock contention is created by holding the real flock on a throwaway
fd inside the test process (deterministic, no sleeps). main()'s session
kill is exercised via the injected loop._kill_own_session_fn seam —
never a real `tmux kill-session`.
"""
from __future__ import annotations

import fcntl
import json
import os
from pathlib import Path

import pytest

import loop
import supervisor as supervisor_mod


# ---------- helpers ----------


def _seed_agent_record(
    home: Path,
    agent_id: str,
    project: str = "fleet",
    *,
    task_id: str | None = None,
) -> None:
    """Seed ~/.fleet/agents/<id>.json. By default the record is THIS
    project's coord (task_id == coord-<project>), matching the live
    record shape (role=executor, task_id=coord-<project>). Pass an
    explicit ``task_id`` to seed a NON-coord record (e.g. a worker) so
    tests can exercise the holder-is-not-a-coord skip path."""
    agents_dir = home / "agents"
    agents_dir.mkdir(parents=True, exist_ok=True)
    (agents_dir / f"{agent_id}.json").write_text(json.dumps({
        "id": agent_id,
        "project": project,
        "task_id": task_id if task_id is not None else f"coord-{project}",
    }))


def _minimal_project(home: Path, project: str = "fleet") -> Path:
    pdir = home / "projects" / project
    pdir.mkdir(parents=True, exist_ok=True)
    (pdir / ".locks").mkdir(exist_ok=True)
    import parse
    f = parse.File(schema=parse.SCHEMA_VERSION, tasks=[], footer="")
    parse.write(str(pdir / "tasks.md"), f)
    return pdir


def _hold_lock(pdir: Path, holder_id: str) -> int:
    """Acquire coordinator.lock (LOCK_EX|LOCK_NB) inside the test
    process and publish `holder_id` into its body, exactly like a live
    coord would via loop._try_lock. Returns the held fd so the caller
    can release it on teardown. Subsequent loop.tick() calls hit the
    lock-busy branch deterministically (no sleeps, no second process)."""
    lock_path = pdir / ".locks" / "coordinator.lock"
    fd = os.open(str(lock_path), os.O_RDWR | os.O_CREAT, 0o644)
    fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
    os.ftruncate(fd, 0)
    os.write(fd, (holder_id + "\n").encode("ascii"))
    os.fsync(fd)
    return fd


@pytest.fixture(autouse=True)
def _stub_lease_identity(monkeypatch: pytest.MonkeyPatch) -> None:
    """Default: the coordinator lease names nobody this session recognizes, so
    gate 5 (lease-names-us) never fires and the remaining gates decide. This
    replaces the deleted coord-spawn marker as the identity source (D3) AND
    keeps the suite deterministic — no test ever shells out to the real
    `fleet coord-owner` binary (which would be the CI trap). Tests exercising
    the lease-names-us skip override this with their own stub."""
    monkeypatch.setattr(
        loop, "_coord_lease_identity_fn",
        lambda project, *, home, fleet_bin="fleet": ("", ""),
    )


@pytest.fixture
def fleet_home(tmp_path: Path) -> Path:
    home = tmp_path / "fleet"
    home.mkdir()
    (home / "inbox").mkdir()
    (home / "projects").mkdir()
    (home / "agents").mkdir()
    return home


@pytest.fixture
def held_lock():
    """Track held lock fds and release them on teardown so a busy lock
    never leaks into the next test (fleet owns its resources)."""
    fds: list[int] = []

    def _hold(pdir: Path, holder_id: str) -> int:
        fd = _hold_lock(pdir, holder_id)
        fds.append(fd)
        return fd

    yield _hold
    for fd in fds:
        try:
            fcntl.flock(fd, fcntl.LOCK_UN)
        finally:
            os.close(fd)


# ---------- the duplicate path: self-exit ----------


def test_live_other_coord_not_intended_triggers_self_exit(
    fleet_home: Path,
    held_lock,
    monkeypatch: pytest.MonkeyPatch,
    capsys: pytest.CaptureFixture[str],
) -> None:
    """Lock held by a DIFFERENT live coord, spawn-marker does NOT name
    us => self_exit=True + diagnostic on stderr + in result.errors."""
    me = "aaaa1111"
    holder = "bbbb2222"
    pdir = _minimal_project(fleet_home)
    _seed_agent_record(fleet_home, me)
    _seed_agent_record(fleet_home, holder)
    held_lock(pdir, holder)
    # Marker names the holder (the real intended coord), not us.
    # Holder's tmux session is "alive" (deterministic stub).
    monkeypatch.setattr(
        supervisor_mod, "tmux_session_alive",
        lambda session, **kw: session == f"fleet-{holder}",
    )

    result = loop.tick(
        project="fleet", coord_id=me, cwd="/tmp/x",
        fleet_home=str(fleet_home),
    )

    assert result.self_exit is True, "duplicate coord must signal self_exit"
    assert result.reason == "duplicate-coord-self-exit", (
        f"reason should pin the duplicate self-exit; got {result.reason!r}"
    )
    joined = "\n".join(result.errors)
    assert holder in joined and me in joined, (
        f"diagnostic must name both holder and self; got {result.errors!r}"
    )
    captured = capsys.readouterr()
    assert "duplicate" in captured.err and holder in captured.err, (
        f"stderr must carry the surface-dont-silo diagnostic; "
        f"got err={captured.err!r}"
    )


def test_no_marker_live_other_coord_still_self_exits(
    fleet_home: Path,
    held_lock,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """No spawn-marker at all + lock held by a live OTHER coord =>
    still a duplicate (the marker guard only PROTECTS us when it names
    us; its absence is not a free pass to keep idling)."""
    me = "aaaa1111"
    holder = "bbbb2222"
    pdir = _minimal_project(fleet_home)
    _seed_agent_record(fleet_home, me)
    _seed_agent_record(fleet_home, holder)
    held_lock(pdir, holder)
    monkeypatch.setattr(
        supervisor_mod, "tmux_session_alive", lambda session, **kw: True,
    )

    result = loop.tick(
        project="fleet", coord_id=me, cwd="/tmp/x",
        fleet_home=str(fleet_home),
    )

    assert result.self_exit is True
    assert result.reason == "duplicate-coord-self-exit"


# ---------- the lock holder itself must NOT self-exit ----------


def test_lock_holder_self_does_not_self_exit(
    fleet_home: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """The coord that actually HOLDS the lock takes the normal path
    (it acquires the lock and never reaches the lock-busy branch)."""
    me = "cccc3333"
    _minimal_project(fleet_home)
    _seed_agent_record(fleet_home, me)
    monkeypatch.setattr(
        supervisor_mod, "tmux_session_alive", lambda session, **kw: True,
    )

    result = loop.tick(
        project="fleet", coord_id=me, cwd="/tmp/x",
        fleet_home=str(fleet_home),
    )

    assert result.self_exit is False, (
        "the lock HOLDER must never self-exit"
    )
    assert result.reason != "duplicate-coord-self-exit"


def test_own_stale_flock_body_does_not_self_exit(
    fleet_home: Path,
    held_lock,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """Lock body names US (our own stale flock from a prior tick that
    hasn't been reaped). Not a duplicate => skip, never self-exit."""
    me = "dddd4444"
    pdir = _minimal_project(fleet_home)
    _seed_agent_record(fleet_home, me)
    held_lock(pdir, me)  # body names self
    monkeypatch.setattr(
        supervisor_mod, "tmux_session_alive", lambda session, **kw: True,
    )

    result = loop.tick(
        project="fleet", coord_id=me, cwd="/tmp/x",
        fleet_home=str(fleet_home),
    )

    assert result.self_exit is False
    assert result.reason == "lock-busy"


# ---------- mid-handoff / intended-successor coord must NOT self-exit ----------


def test_intended_coord_per_marker_does_not_self_exit(
    fleet_home: Path,
    held_lock,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """spawn-marker names US => we are the project's intended /
    successor coord (mid-handoff). Even with a live OTHER coord still
    holding the flock for one more tick, we must NEVER self-exit — the
    stale holder is the one to be reaped, not us."""
    me = "eeee5555"
    old = "ffff6666"
    pdir = _minimal_project(fleet_home)
    _seed_agent_record(fleet_home, me)
    _seed_agent_record(fleet_home, old)
    held_lock(pdir, old)
    # The coordinator lease names US as the live active owner => intended /
    # successor coord => never self-exit (D3: replaces the marker-names-us guard).
    monkeypatch.setattr(
        loop, "_coord_lease_identity_fn",
        lambda project, *, home, fleet_bin="fleet": (me, ""),
    )
    monkeypatch.setattr(
        supervisor_mod, "tmux_session_alive", lambda session, **kw: True,
    )

    result = loop.tick(
        project="fleet", coord_id=me, cwd="/tmp/x",
        fleet_home=str(fleet_home),
    )

    assert result.self_exit is False, (
        "the intended/successor coord (marker names us) must never self-exit"
    )
    assert result.reason == "lock-busy"


def test_handoff_successor_does_not_self_exit(
    fleet_home: Path,
    held_lock,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """Gate-5 handoff-successor arm: the lease has NO live active owner but an
    in-flight handoff reservation names US as the successor (the production
    mid-handoff state — predecessor still holds the flock, epoch not yet
    committed to us). `live_owner==coord_id OR handoff_succ==coord_id` must skip
    on the SUCCESSOR clause too, not only the owner clause. A live OTHER coord
    holding the flock must NOT make us self-exit."""
    me = "eeee7777"
    old = "ffff8888"
    pdir = _minimal_project(fleet_home)
    _seed_agent_record(fleet_home, me)
    _seed_agent_record(fleet_home, old)
    held_lock(pdir, old)
    # No active owner; the handoff reservation names US as successor.
    monkeypatch.setattr(
        loop, "_coord_lease_identity_fn",
        lambda project, *, home, fleet_bin="fleet": ("", me),
    )
    monkeypatch.setattr(
        supervisor_mod, "tmux_session_alive", lambda session, **kw: True,
    )

    result = loop.tick(
        project="fleet", coord_id=me, cwd="/tmp/x",
        fleet_home=str(fleet_home),
    )

    assert result.self_exit is False, (
        "the named handoff successor must never self-exit (gate-5 succ arm)"
    )
    assert result.reason == "lock-busy"


def test_inconclusive_lease_read_does_not_self_exit(
    fleet_home: Path,
    held_lock,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """P0 regression: gate-5's `fleet coord-owner` read FAILED (stale FLEET_BIN
    lacking the subcommand — the #182 class — timeout, or unparseable output),
    returning the INCONCLUSIVE sentinel (None, None). Even with a live OTHER
    coord holding the flock, an unknowable identity must SKIP, never self-exit:
    a failed binary read must not be mistaken for 'lease empty' and neuter the
    mid-handoff guard into over-eager self-exit of a legit successor
    (no-auto-kill invariant). If this regresses, a stale binary silently kills
    live successors."""
    me = "eeee9999"
    old = "ffffaaaa"
    pdir = _minimal_project(fleet_home)
    _seed_agent_record(fleet_home, me)
    _seed_agent_record(fleet_home, old)
    held_lock(pdir, old)
    # coord-owner read failed -> INCONCLUSIVE. (Contrast: the default autouse
    # stub returns the CONCLUSIVE-empty ("", "") which DOES fall through to
    # gate 6 and self-exits — see test_live_other_coord_not_intended_...).
    monkeypatch.setattr(
        loop, "_coord_lease_identity_fn",
        lambda project, *, home, fleet_bin="fleet": (None, None),
    )
    monkeypatch.setattr(
        supervisor_mod, "tmux_session_alive", lambda session, **kw: True,
    )

    result = loop.tick(
        project="fleet", coord_id=me, cwd="/tmp/x",
        fleet_home=str(fleet_home),
    )

    assert result.self_exit is False, (
        "an inconclusive lease-identity read must skip, never self-exit"
    )
    assert result.reason == "lock-busy"


# ---------- conservatism gates: each one => skip, never self-exit ----------


def test_dead_holder_does_not_self_exit(
    fleet_home: Path,
    held_lock,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """Holder's tmux session is DEAD => normal takeover scenario, the
    next tick acquires the lock. Must not self-exit."""
    me = "aaaa0001"
    holder = "bbbb0002"
    pdir = _minimal_project(fleet_home)
    _seed_agent_record(fleet_home, me)
    _seed_agent_record(fleet_home, holder)
    held_lock(pdir, holder)
    monkeypatch.setattr(
        supervisor_mod, "tmux_session_alive", lambda session, **kw: False,
    )

    result = loop.tick(
        project="fleet", coord_id=me, cwd="/tmp/x",
        fleet_home=str(fleet_home),
    )

    assert result.self_exit is False
    assert result.reason == "lock-busy"


def test_holder_without_agent_record_does_not_self_exit(
    fleet_home: Path,
    held_lock,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """Holder ID has no agent record on disk => cannot prove a live
    coord; skip rather than self-exit."""
    me = "aaaa0003"
    holder = "bbbb0004"
    pdir = _minimal_project(fleet_home)
    _seed_agent_record(fleet_home, me)
    # Deliberately do NOT seed holder's record.
    held_lock(pdir, holder)
    monkeypatch.setattr(
        supervisor_mod, "tmux_session_alive", lambda session, **kw: True,
    )

    result = loop.tick(
        project="fleet", coord_id=me, cwd="/tmp/x",
        fleet_home=str(fleet_home),
    )

    assert result.self_exit is False
    assert result.reason == "lock-busy"


def test_self_is_worker_does_not_self_exit(
    fleet_home: Path,
    held_lock,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """codex review [P2] round 2: THIS session is a same-project WORKER
    (task_id is a feature task, not coord-<project>) that accidentally
    ran loop.py while the real coord holds the lock. It must NOT be
    classified a duplicate coord and have its worker session killed —
    just skip the tick."""
    me_worker = "aaaa0031"
    real_coord = "bbbb0032"
    pdir = _minimal_project(fleet_home)
    # THIS session is a worker: same project, feature task_id.
    _seed_agent_record(
        fleet_home, me_worker, project="fleet", task_id="feature-task-9999",
    )
    # The lock is held by the genuine project coord.
    _seed_agent_record(fleet_home, real_coord)
    held_lock(pdir, real_coord)
    monkeypatch.setattr(
        supervisor_mod, "tmux_session_alive", lambda session, **kw: True,
    )

    result = loop.tick(
        project="fleet", coord_id=me_worker, cwd="/tmp/x",
        fleet_home=str(fleet_home),
    )

    assert result.self_exit is False, (
        "a worker that ran loop.py must never self-exit (would kill worker work)"
    )
    assert result.reason == "lock-busy"


def test_holder_is_same_project_worker_does_not_self_exit(
    fleet_home: Path,
    held_lock,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """codex review [P2]: the lock body names a LIVE same-project agent
    whose task_id is a feature task, not coord-<project> (a worker that
    hijacked coordinator.lock — the fleet#171 scenario). It is NOT a
    legitimate coord to defer to, so we must skip, never self-exit."""
    me = "aaaa0021"
    worker = "bbbb0022"
    pdir = _minimal_project(fleet_home)
    _seed_agent_record(fleet_home, me)
    # Holder is a WORKER: same project, but task_id is a feature task.
    _seed_agent_record(
        fleet_home, worker, project="fleet", task_id="some-feature-task-1234",
    )
    held_lock(pdir, worker)
    monkeypatch.setattr(
        supervisor_mod, "tmux_session_alive", lambda session, **kw: True,
    )

    result = loop.tick(
        project="fleet", coord_id=me, cwd="/tmp/x",
        fleet_home=str(fleet_home),
    )

    assert result.self_exit is False, (
        "a same-project worker holding the lock must not trigger self-exit"
    )
    assert result.reason == "lock-busy"


def test_holder_is_cross_project_coord_does_not_self_exit(
    fleet_home: Path,
    held_lock,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """codex review [P2]: the lock body names a LIVE coord that belongs
    to a DIFFERENT project (stale/leaked body). Its project field does
    not match ours, so it is not a legitimate holder for THIS project's
    lock — skip, never self-exit."""
    me = "aaaa0023"
    other = "bbbb0024"
    pdir = _minimal_project(fleet_home)
    _seed_agent_record(fleet_home, me)
    # Holder is a coord, but for a DIFFERENT project.
    _seed_agent_record(
        fleet_home, other, project="rainier", task_id="coord-rainier",
    )
    held_lock(pdir, other)
    monkeypatch.setattr(
        supervisor_mod, "tmux_session_alive", lambda session, **kw: True,
    )

    result = loop.tick(
        project="fleet", coord_id=me, cwd="/tmp/x",
        fleet_home=str(fleet_home),
    )

    assert result.self_exit is False, (
        "a cross-project coord in the lock body must not trigger self-exit"
    )
    assert result.reason == "lock-busy"


def test_empty_lock_body_does_not_self_exit(
    fleet_home: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """Lock held but body has no valid holder ID (legacy zero-byte
    lock / race before the holder published its body) => can't prove a
    duplicate; skip."""
    me = "aaaa0005"
    pdir = _minimal_project(fleet_home)
    _seed_agent_record(fleet_home, me)
    # Hold the lock but leave the body empty (zero bytes).
    lock_path = pdir / ".locks" / "coordinator.lock"
    fd = os.open(str(lock_path), os.O_RDWR | os.O_CREAT, 0o644)
    fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
    monkeypatch.setattr(
        supervisor_mod, "tmux_session_alive", lambda session, **kw: True,
    )
    try:
        result = loop.tick(
            project="fleet", coord_id=me, cwd="/tmp/x",
            fleet_home=str(fleet_home),
        )
    finally:
        fcntl.flock(fd, fcntl.LOCK_UN)
        os.close(fd)

    assert result.self_exit is False
    assert result.reason == "lock-busy"


def test_empty_coord_id_does_not_self_exit(
    fleet_home: Path,
    held_lock,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """Manual `python3 loop.py <project>` shell invocation (no
    FLEET_AGENT_ID / coord_id). We can't reason about identity =>
    skip, never self-exit."""
    holder = "bbbb0006"
    pdir = _minimal_project(fleet_home)
    _seed_agent_record(fleet_home, holder)
    held_lock(pdir, holder)
    monkeypatch.delenv("FLEET_AGENT_ID", raising=False)
    monkeypatch.setattr(
        supervisor_mod, "tmux_session_alive", lambda session, **kw: True,
    )

    result = loop.tick(
        project="fleet", coord_id="", cwd="/tmp/x",
        fleet_home=str(fleet_home),
    )

    assert result.self_exit is False
    assert result.reason == "lock-busy"


# ---------- main() integration: self_exit => kill own session ----------


def test_main_self_exit_invokes_session_kill(
    fleet_home: Path,
    held_lock,
    monkeypatch: pytest.MonkeyPatch,
    capsys: pytest.CaptureFixture[str],
) -> None:
    """End-to-end through main(): a duplicate coord prints a JSON
    summary carrying self_exit + reason, then calls the injected
    session-kill seam with its OWN coord_id (never a real tmux kill)."""
    me = "aaaa0007"
    holder = "bbbb0008"
    pdir = _minimal_project(fleet_home)
    _seed_agent_record(fleet_home, me)
    _seed_agent_record(fleet_home, holder)
    held_lock(pdir, holder)
    monkeypatch.setenv("FLEET_HOME", str(fleet_home))
    monkeypatch.setenv("FLEET_PROJECT", "fleet")
    monkeypatch.setenv("FLEET_AGENT_ID", me)
    monkeypatch.setattr(
        supervisor_mod, "tmux_session_alive", lambda session, **kw: True,
    )
    killed: list[str] = []
    monkeypatch.setattr(
        loop, "_kill_own_session_fn",
        lambda cid: (killed.append(cid) or True),
    )

    rc = loop.main(["fleet"])

    assert rc == 0, "self-exit path returns 0 (clean exit, not an error code)"
    assert killed == [me], (
        f"main() must kill THIS coord's own session exactly once; "
        f"got {killed!r}"
    )
    captured = capsys.readouterr()
    payload = json.loads(captured.out.strip().splitlines()[-1])
    assert payload["self_exit"] is True
    assert payload["reason"] == "duplicate-coord-self-exit"


def test_main_self_exit_kill_failure_surfaces_diagnostic(
    fleet_home: Path,
    held_lock,
    monkeypatch: pytest.MonkeyPatch,
    capsys: pytest.CaptureFixture[str],
) -> None:
    """If the session kill fails, main() must surface a concrete
    operator hint on stderr (surface_dont_silo) rather than silently
    leaving the zombie."""
    me = "aaaa0009"
    holder = "bbbb0010"
    pdir = _minimal_project(fleet_home)
    _seed_agent_record(fleet_home, me)
    _seed_agent_record(fleet_home, holder)
    held_lock(pdir, holder)
    monkeypatch.setenv("FLEET_HOME", str(fleet_home))
    monkeypatch.setenv("FLEET_PROJECT", "fleet")
    monkeypatch.setenv("FLEET_AGENT_ID", me)
    monkeypatch.setattr(
        supervisor_mod, "tmux_session_alive", lambda session, **kw: True,
    )
    monkeypatch.setattr(loop, "_kill_own_session_fn", lambda cid: False)

    rc = loop.main(["fleet"])

    assert rc == 0
    captured = capsys.readouterr()
    assert "fleet gc" in captured.err or "kill" in captured.err, (
        f"kill failure must surface an operator next-step; "
        f"got err={captured.err!r}"
    )


def test_main_normal_lock_busy_does_not_kill(
    fleet_home: Path,
    held_lock,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """A plain lock-busy skip (not a duplicate — holder dead) must NOT
    invoke the session kill through main()."""
    me = "aaaa0011"
    holder = "bbbb0012"
    pdir = _minimal_project(fleet_home)
    _seed_agent_record(fleet_home, me)
    _seed_agent_record(fleet_home, holder)
    held_lock(pdir, holder)
    monkeypatch.setenv("FLEET_HOME", str(fleet_home))
    monkeypatch.setenv("FLEET_PROJECT", "fleet")
    monkeypatch.setenv("FLEET_AGENT_ID", me)
    monkeypatch.setattr(
        supervisor_mod, "tmux_session_alive", lambda session, **kw: False,
    )
    killed: list[str] = []
    monkeypatch.setattr(
        loop, "_kill_own_session_fn",
        lambda cid: (killed.append(cid) or True),
    )

    rc = loop.main(["fleet"])

    assert rc == 0
    assert killed == [], "normal lock-busy skip must not kill any session"
