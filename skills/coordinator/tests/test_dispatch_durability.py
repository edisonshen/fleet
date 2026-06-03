"""dispatch-durability (fleet#184) — Python-side tests.

These exercise the loop.py replay reconcile + the dispatch.py launch-state
helpers against the REAL `fleet` binary (built once per session) so the Go
flock RMW journal is genuinely driven end-to-end — the architect-level e2e
the operator's `feedback_e2e_tests_for_all_cases` rule requires. The 7
design test-plan cases:

  (a) launched-but-unacked (no ack) → no replay, no second block.
  (b) ExecLaunchAttempted survives restart → suppresses replay.
  (c) phantom recovery: ExecPending → replay reclaims next tick.
  (d) inbox-missing × journal-state variants.
  (e) replay cap persists + ExecBlocked, no infinite re-emit.
  (f) broken-stdout recurs on replay → durable BLOCKED (off-channel).
  (g) healthy heartbeat + missing ack → repair-not-redispatch.

Plus an integration test: broken-stdout incident → next healthy tick
re-emits the ExecPending dispatch once, no phantom, no double-launch.
"""
from __future__ import annotations

import json
import shutil
import subprocess
import time
from pathlib import Path

import pytest

import dispatch as dispatch_mod
import loop


# ---------------------------------------------------------------------------
# Real `fleet` binary fixture — build once per session.
# ---------------------------------------------------------------------------


@pytest.fixture(scope="session")
def fleet_bin(tmp_path_factory) -> str:
    """Build the real `fleet` binary so the journal flock RMW is exercised
    for real. Skips the durability e2e tests if Go is unavailable."""
    if shutil.which("go") is None:
        pytest.skip("go toolchain not available")
    # Repo root: skills/coordinator/tests/ → up 3.
    repo_root = Path(__file__).resolve().parents[3]
    out_dir = tmp_path_factory.mktemp("fleet-bin")
    binary = out_dir / "fleet"
    proc = subprocess.run(
        ["go", "build", "-o", str(binary), "./cmd/fleet"],
        cwd=str(repo_root), capture_output=True, text=True,
    )
    if proc.returncode != 0:
        pytest.skip(f"fleet build failed: {proc.stderr}")
    return str(binary)


@pytest.fixture
def home(tmp_path: Path) -> Path:
    h = tmp_path / "fleet-home"
    (h / "inbox").mkdir(parents=True)
    (h / "dispatches").mkdir(parents=True)
    (h / "projects" / "myproj").mkdir(parents=True)
    return h


def _acquire(fleet_bin: str, home: Path, agent_id: str, slug: str,
             prompt: str = "worker prompt") -> str:
    """Acquire a coord_prompt_inbox claim (creates a real journal at
    ExecPending). Returns the inbox path."""
    return dispatch_mod.acquire_coord_prompt_inbox(
        agent_id, prompt,
        owner=f"project/myproj/slug/{slug}",
        dispatch_kind="worker",
        fleet_bin=fleet_bin, fleet_home=str(home),
    )


def _journal(home: Path, agent_id: str) -> dict:
    return json.loads(
        (home / "dispatches" / f"{agent_id}.json").read_text(encoding="utf-8")
    )


def _adopt_all(home: Path) -> dict:
    """Build a coord_state that ADOPTS every on-disk dispatch journal:
    worker_agent_ids[slug] = agent_id for each journal. Models the normal
    "the dispatch was applied" state — replay's identity predicate
    (codex iter-4) re-emits a pending journal only when the task adopted
    THAT agent_id. The phantom tests (broken-stdout) are exactly this
    case: the dispatch was applied (adopted), the block was just lost."""
    import json as _json
    agents: dict[str, str] = {}
    ddir = home / "dispatches"
    if ddir.is_dir():
        for p in sorted(ddir.iterdir()):
            if not p.name.endswith(".json") or p.name.endswith(".json.lock"):
                continue
            aid = p.name[: -len(".json")]
            try:
                j = _json.loads(p.read_text(encoding="utf-8"))
            except (OSError, ValueError):
                continue
            owner = j.get("owner", "")
            if isinstance(owner, str) and owner.startswith("project/myproj/slug/"):
                agents[owner[len("project/myproj/slug/"):]] = aid
    return {"worker_agent_ids": agents}


def _replay(home: Path, fleet_bin: str, *, now_unix: float | None = None,
            coord_state: dict | None = None):
    # Default to the adopted-all coord_state so phantom tests (block lost
    # after the dispatch was applied) re-emit; tests exercising orphans /
    # mid-application / handoffs pass an explicit coord_state.
    return loop._replay_pending_dispatches(
        project="myproj",
        home=home,
        fleet_bin=fleet_bin,
        fleet_home=str(home),
        coord_state=coord_state if coord_state is not None else _adopt_all(home),
        now_unix=now_unix if now_unix is not None else time.time(),
    )


# ---------------------------------------------------------------------------
# Case (c) — phantom recovery: ExecPending → replay reclaims next tick.
# ---------------------------------------------------------------------------


def test_case_c_pending_replayed(fleet_bin: str, home: Path) -> None:
    _acquire(fleet_bin, home, "aac00001", "fix-foo")
    assert _journal(home, "aac00001")["exec_state"] == "pending"

    actions = _replay(home, fleet_bin)
    blocks = [a for a in actions if a.dispatch_instruction]
    assert len(blocks) == 1
    b = blocks[0]
    assert b.agent_id == "aac00001"
    assert "DISPATCH: fix-foo" in b.dispatch_instruction
    assert "generation: 0" in b.dispatch_instruction
    # Durable counter advanced.
    assert _journal(home, "aac00001")["replay_emit_attempts"] == 1


def test_partial_apply_stale_state_replayed_not_wedged(
    fleet_bin: str, home: Path,
) -> None:
    """codex iter-4 [P1] (wtlc-pr3): the dispatch partial-apply window —
    the dispatch_generation task-row bump landed but the `starting`
    state.json bootstrap did NOT (crash / failed _run_fleet). The task is
    in-progress at the NEW generation while only a STALE prior-generation
    state.json exists on disk.

    PR3's reconcile correctly REFUSES to mutate a stale-state slug (it must
    not requeue an in-progress task with an adopted journal — the #184
    double-dispatch trap). Recovery is owned by the dispatch-journal
    REPLAY: this test proves replay re-emits the DISPATCH for exactly this
    case, so the relaunched worker writes a current-generation state and
    the task is NOT wedged."""
    _acquire(fleet_bin, home, "aac00099", "redisp-foo")
    assert _journal(home, "aac00099")["exec_state"] == "pending"
    # A stale prior-generation state.json (gen 1) sits on disk; the task
    # row authority is now gen 2 (re-dispatched). The bootstrap to gen 2
    # never ran (partial apply), so reconcile would short-circuit `stale`.
    wdir = home / "projects" / "myproj" / "workers" / "redisp-foo"
    wdir.mkdir(parents=True)
    (wdir / "state.json").write_text(
        json.dumps({"phase": "done", "dispatch_generation": 1,
                    "pr_url": "https://x/old"}),
        encoding="utf-8")
    # Replay's identity predicate adopts aac00099 for redisp-foo.
    actions = _replay(
        home, fleet_bin,
        coord_state={"worker_agent_ids": {"redisp-foo": "aac00099"}},
    )
    blocks = [a for a in actions if a.dispatch_instruction]
    assert len(blocks) == 1, "replay must re-emit the partial-apply dispatch"
    assert blocks[0].agent_id == "aac00099"
    assert "DISPATCH: redisp-foo" in blocks[0].dispatch_instruction


# ---------------------------------------------------------------------------
# Case (a) — launched-but-unacked (launch_attempted, fresh) → no replay.
# ---------------------------------------------------------------------------


def test_case_a_launch_attempted_no_replay(fleet_bin: str, home: Path) -> None:
    _acquire(fleet_bin, home, "aaa00001", "fix-foo")
    # Coord flips to launch_attempted (the durable launch record).
    assert dispatch_mod.mark_launch_attempted(
        "aaa00001", 0, fleet_bin=fleet_bin, fleet_home=str(home)) == "ok"

    # Fresh launch (now == attempted) → within grace → no replay, no repair.
    actions = _replay(home, fleet_bin, now_unix=time.time())
    assert [a for a in actions if a.dispatch_instruction] == []
    assert [a for a in actions if a.raise_msg] == []
    assert _journal(home, "aaa00001")["exec_state"] == "launch_attempted"


# ---------------------------------------------------------------------------
# Case (b) — ExecLaunchAttempted survives "restart" → still suppresses replay.
# ---------------------------------------------------------------------------


def test_case_b_launch_attempted_durable(fleet_bin: str, home: Path) -> None:
    _acquire(fleet_bin, home, "aab00001", "fix-foo")
    dispatch_mod.mark_launch_attempted(
        "aab00001", 0, fleet_bin=fleet_bin, fleet_home=str(home))
    # Simulate a coord restart: a brand-new replay call (no in-memory
    # state) reads the durable journal and must NOT re-emit.
    actions = _replay(home, fleet_bin, now_unix=time.time())
    assert [a for a in actions if a.dispatch_instruction] == []


# ---------------------------------------------------------------------------
# Case (g) — healthy heartbeat + missing ack → repair-not-redispatch.
# A launch_attempted whose slug has a live registered subagent is left for
# the supervisor (registration-repair), NEVER replayed/blocked here.
# ---------------------------------------------------------------------------


def test_case_g_live_worker_not_repaired(fleet_bin: str, home: Path) -> None:
    """A live worker (worker-authored state.json advanced AFTER this
    dispatch's launch_attempted_at) is left alone, even past grace and
    even with no journal ack.

    Codex iter-3 [P1]: liveness is gated on the worker's OWN fresh
    state.json, NOT a slug-keyed subagent_id (which can be a stale
    prior-stage mapping in a reviewer/finisher handoff). So the live
    signal here is a fresh worker state write, not coord_state."""
    _acquire(fleet_bin, home, "aa900001", "fix-foo")
    dispatch_mod.mark_launch_attempted(
        "aa900001", 0, fleet_bin=fleet_bin, fleet_home=str(home))
    # Worker is genuinely running: phase past the bootstrap, updated_at
    # after the launch flip.
    wdir = home / "projects" / "myproj" / "workers" / "fix-foo"
    wdir.mkdir(parents=True)
    (wdir / "state.json").write_text(
        json.dumps({"phase": "tdd-green", "updated_at": "2099-01-01T00:00:00Z"}),
        encoding="utf-8")
    actions = _replay(
        home, fleet_bin,
        now_unix=time.time() + 10_000,  # far past grace
        coord_state={},
    )
    # No replay, no repair: a live worker → leave it.
    assert [a for a in actions if a.dispatch_instruction] == []
    assert [a for a in actions if a.raise_msg] == []
    assert _journal(home, "aa900001")["exec_state"] == "launch_attempted"


# ---------------------------------------------------------------------------
# Residual-crash repair: launch_attempted + no ack + no subagent + past
# grace → ExecBlocked/failed + off-channel escalation (NOT redispatch).
# ---------------------------------------------------------------------------


def test_residual_crash_repair(fleet_bin: str, home: Path) -> None:
    _acquire(fleet_bin, home, "aad00001", "fix-foo")
    dispatch_mod.mark_launch_attempted(
        "aad00001", 0, fleet_bin=fleet_bin, fleet_home=str(home))
    actions = _replay(
        home, fleet_bin,
        now_unix=time.time() + 10_000,  # past LAUNCH_ACK_GRACE
        coord_state={},                  # no registered subagent
    )
    # Escalation raised, NOT a redispatch block (no double-launch).
    assert [a for a in actions if a.dispatch_instruction] == []
    raises = [a for a in actions if a.raise_msg]
    assert len(raises) == 1
    assert "never acked" in raises[0].raise_msg
    # Codex iter-2 [P1]: repair ESCALATES off-channel but does NOT
    # destructively release — a live-but-unregistered worker looks
    # identical to a phantom, so tearing down the inbox/journal would
    # risk killing a healthy worker. The journal stays launch_attempted
    # (replay never re-emits it → no double-launch); the operator decides.
    assert _journal(home, "aad00001")["exec_state"] == "launch_attempted"


def test_residual_crash_repair_escalates_once(fleet_bin: str, home: Path) -> None:
    """Codex iter-6 [P2]: the escalation persists a breadcrumb in
    coord_state so it fires ONCE per (agent_id, generation), not every
    tick. The journal is deliberately left launch_attempted, so without
    the breadcrumb the same operator escalation would re-raise forever."""
    _acquire(fleet_bin, home, "aad000e1", "fix-foo")
    dispatch_mod.mark_launch_attempted(
        "aad000e1", 0, fleet_bin=fleet_bin, fleet_home=str(home))
    cs: dict = {}  # SAME coord_state across both ticks (persistence)
    far = time.time() + 10_000
    first = _replay(home, fleet_bin, now_unix=far, coord_state=cs)
    assert len([a for a in first if a.raise_msg]) == 1, "first tick must escalate"
    # Second tick over the SAME (unchanged) journal + coord_state: the
    # breadcrumb suppresses a duplicate escalation.
    second = _replay(home, fleet_bin, now_unix=far, coord_state=cs)
    assert [a for a in second if a.raise_msg] == [], "escalation re-fired (no breadcrumb)"


def test_residual_crash_repair_agent_id_breadcrumb_not_suppressing(
    fleet_bin: str, home: Path,
) -> None:
    """Codex iter-1 [P1]: a phantom that crashed after mark-launch-attempted
    but before the Agent call has its worker_agent_ids breadcrumb ALREADY
    set (remember_agent_id runs in _apply_dispatch before the DISPATCH
    block is even emitted). The residual-crash repair MUST still fire — it
    keys only on the post-launch subagent_id, never on the pre-launch
    agent_id. Without the fix the journal sits at launch_attempted forever.
    """
    _acquire(fleet_bin, home, "aae00cab", "fix-foo")
    dispatch_mod.mark_launch_attempted(
        "aae00cab", 0, fleet_bin=fleet_bin, fleet_home=str(home))
    actions = _replay(
        home, fleet_bin,
        now_unix=time.time() + 10_000,  # past grace
        # agent_id breadcrumb present (pre-launch), but NO subagent_id ack.
        coord_state={"worker_agent_ids": {"fix-foo": "aae00cab"}},
    )
    raises = [a for a in actions if a.raise_msg]
    assert len(raises) == 1, "phantom with only agent_id breadcrumb must be repaired"
    assert "never acked" in raises[0].raise_msg
    # Escalate-only (codex iter-2): journal left at launch_attempted, not released.
    assert _journal(home, "aae00cab")["exec_state"] == "launch_attempted"


def test_residual_crash_repair_left_alone_for_live_worker(
    fleet_bin: str, home: Path,
) -> None:
    """Codex iter-2 [P1]: register_subagent is best-effort and may be
    SKIPPED while the worker runs. A launch_attempted entry past grace
    with NO subagent_id but a worker-authored state.json phase is a LIVE
    unregistered worker, NOT a phantom — it must be left alone (no
    escalation, no release/teardown of its inbox). Only a true phantom
    (no worker progress) is escalated."""
    _acquire(fleet_bin, home, "aae00111", "fix-foo")
    dispatch_mod.mark_launch_attempted(
        "aae00111", 0, fleet_bin=fleet_bin, fleet_home=str(home))
    # Seed a worker-authored state.json: phase advanced past the
    # "starting" bootstrap AND updated_at is AFTER this dispatch's
    # launch_attempted_at (codex iter-3: the write must belong to THIS
    # launch, not a stale prior stage).
    wdir = home / "projects" / "myproj" / "workers" / "fix-foo"
    wdir.mkdir(parents=True)
    (wdir / "state.json").write_text(
        json.dumps({"phase": "tdd-green", "updated_at": "2099-01-01T00:00:00Z"}),
        encoding="utf-8")

    actions = _replay(
        home, fleet_bin,
        now_unix=time.time() + 10_000,  # well past grace
        coord_state={},                  # no subagent registration
    )
    # No escalation, no redispatch — the live worker is left for the
    # normal worker-state liveness path.
    assert [a for a in actions if a.raise_msg] == [], "live worker wrongly escalated"
    assert [a for a in actions if a.dispatch_instruction] == []
    # Journal is NOT torn down — still launch_attempted (not released).
    assert _journal(home, "aae00111")["exec_state"] == "launch_attempted"


def test_residual_crash_repair_handoff_stale_priorstage_state(
    fleet_bin: str, home: Path,
) -> None:
    """Codex iter-3 [P1]: a reviewer/finisher handoff mints a NEW agent_id
    but reuses the slug. The PRIOR stage already left a review-pending /
    review-done state.json (and a prior subagent_id in coord_state) BEFORE
    the new launch flip. A handoff that crashes after mark-launch-attempted
    but before the Agent invoke must still be detected as a phantom —
    the stale prior-stage state predates this journal's launch_attempted_at
    and must NOT count as live, and the slug-keyed prior subagent_id must
    NOT suppress repair."""
    _acquire(fleet_bin, home, "aae00333", "fix-foo")
    dispatch_mod.mark_launch_attempted(
        "aae00333", 0, fleet_bin=fleet_bin, fleet_home=str(home))
    # Prior-stage state.json: a review phase, but written BEFORE the new
    # launch flip (timestamp in the past relative to launch_attempted_at).
    wdir = home / "projects" / "myproj" / "workers" / "fix-foo"
    wdir.mkdir(parents=True)
    (wdir / "state.json").write_text(
        json.dumps({"phase": "review-pending", "updated_at": "2000-01-01T00:00:00Z"}),
        encoding="utf-8")

    actions = _replay(
        home, fleet_bin,
        now_unix=time.time() + 10_000,  # past grace
        # Stale prior-stage subagent mapping for the slug.
        coord_state={"worker_subagent_ids": {"fix-foo": "sub-prior"}},
    )
    # Phantom detected → escalated (raise), NOT treated as live.
    raises = [a for a in actions if a.raise_msg]
    assert len(raises) == 1, "handoff phantom with stale prior-stage state not escalated"
    assert [a for a in actions if a.dispatch_instruction] == []
    assert _journal(home, "aae00333")["exec_state"] == "launch_attempted"


def test_replay_skips_mid_application_pending(
    fleet_bin: str, home: Path,
) -> None:
    """Codex iter-2 [P1]: a journal acquired (ExecPending) but not yet
    applied (slug still in pending_acquire_agent_ids) is owned by the
    normal _dispatch_ready retry, NOT replay. Replaying it would launch a
    worker with no applied task state AND clear the pending-acquire
    handle. Replay must skip ids the pending_acquire map still owns."""
    _acquire(fleet_bin, home, "aae00222", "fix-foo")
    actions = _replay(
        home, fleet_bin,
        coord_state={"pending_acquire_agent_ids": {"fix-foo": "aae00222"}},
    )
    # No replay block emitted — the normal retry owns this id.
    assert [a for a in actions if a.dispatch_instruction] == []
    # And the cap was NOT consumed (replay never touched the journal).
    assert _journal(home, "aae00222").get("replay_emit_attempts", 0) == 0


def test_replay_skips_orphaned_pending_not_adopted(
    fleet_bin: str, home: Path,
) -> None:
    """Codex iter-4 [P1]: a pending journal the task did NOT adopt (coord
    crashed after acquire but before persisting state; the still-ready
    task was re-dispatched under a DIFFERENT agent_id) must NOT be
    replayed — replaying journal A while agent B owns the task is a
    cross-id double-launch the per-id CAS cannot stop. Replay re-emits
    only the journal whose agent_id == worker_agent_ids[slug]."""
    _acquire(fleet_bin, home, "aae0a001", "fix-foo")  # orphaned journal A
    # The task adopted a DIFFERENT dispatch (agent B) on re-dispatch.
    actions = _replay(
        home, fleet_bin,
        coord_state={"worker_agent_ids": {"fix-foo": "bbbb0002"}},
    )
    assert [a for a in actions if a.dispatch_instruction] == [], \
        "orphaned pending journal wrongly replayed (cross-id double-launch)"
    # Orphan's cap untouched — left for the cap/sweeper, not relaunched.
    assert _journal(home, "aae0a001").get("replay_emit_attempts", 0) == 0


# ---------------------------------------------------------------------------
# Case (e) — replay cap persists + ExecBlocked, no infinite re-emit.
# ---------------------------------------------------------------------------


def test_case_e_replay_cap_blocks(fleet_bin: str, home: Path) -> None:
    _acquire(fleet_bin, home, "aae00001", "fix-foo")
    cap = loop._REPLAY_CAP
    # Replay CAP times → each reserves a block.
    for i in range(cap):
        actions = _replay(home, fleet_bin)
        assert len([a for a in actions if a.dispatch_instruction]) == 1, i
    # cap+1-th replay → capped → off-channel escalation, no block.
    actions = _replay(home, fleet_bin)
    assert [a for a in actions if a.dispatch_instruction] == []
    raises = [a for a in actions if a.raise_msg]
    assert len(raises) == 1
    assert "undelivered" in raises[0].raise_msg
    assert _journal(home, "aae00001")["exec_state"] == "blocked"
    # Further replays do nothing (no infinite loop).
    actions = _replay(home, fleet_bin)
    assert [a for a in actions if a.dispatch_instruction] == []
    assert [a for a in actions if a.raise_msg] == []


# ---------------------------------------------------------------------------
# Case (f) — broken-stdout recurs on replay → durable BLOCKED off-channel.
# The cap counter is durable in the journal, so repeated replay across
# (simulated) restarts converges to BLOCKED rather than looping forever.
# ---------------------------------------------------------------------------


def test_case_f_broken_stdout_converges_to_blocked(fleet_bin: str, home: Path) -> None:
    _acquire(fleet_bin, home, "aaf00001", "fix-foo")
    blocked = False
    # Each iteration = a fresh tick (broken stdout means the block never
    # lands, but reserve-replay already advanced the durable counter).
    for _ in range(loop._REPLAY_CAP + 1):
        actions = _replay(home, fleet_bin)
        if any(a.raise_msg and "undelivered" in a.raise_msg for a in actions):
            blocked = True
            break
    assert blocked, "broken-stdout replay never converged to BLOCKED"
    assert _journal(home, "aaf00001")["exec_state"] == "blocked"


# ---------------------------------------------------------------------------
# Case (d) — inbox-missing × journal-state. A released journal is not
# pending → no replay even if the inbox file lingers/absent.
# ---------------------------------------------------------------------------


def test_case_d_released_journal_no_replay(fleet_bin: str, home: Path) -> None:
    _acquire(fleet_bin, home, "aae0000d", "fix-foo")
    # Release it (terminal). Inbox unlinked by release.
    dispatch_mod.release_coord_prompt_inbox(
        "aae0000d", fleet_bin=fleet_bin, fleet_home=str(home))
    j = _journal(home, "aae0000d")
    assert j["exec_state"] in ("done", "released", "failed", "blocked")
    # Replay: not pending → no re-emit, no escalation.
    actions = _replay(home, fleet_bin)
    assert [a for a in actions if a.dispatch_instruction] == []
    assert [a for a in actions if a.raise_msg] == []


def test_case_d_absent_journal_skipped(fleet_bin: str, home: Path) -> None:
    # No journal at all → nothing to replay.
    actions = _replay(home, fleet_bin)
    assert actions == []


# ---------------------------------------------------------------------------
# Project-ownership: a journal owned by ANOTHER project is never replayed
# (strict coord scope — replay keys on owner, not cwd/tasks).
# ---------------------------------------------------------------------------


def test_replay_ignores_other_projects(fleet_bin: str, home: Path) -> None:
    dispatch_mod.acquire_coord_prompt_inbox(
        "aaa0000e", "p",
        owner="project/otherproj/slug/x",
        dispatch_kind="worker",
        fleet_bin=fleet_bin, fleet_home=str(home))
    actions = _replay(home, fleet_bin)  # project=myproj
    assert actions == []


# ---------------------------------------------------------------------------
# Integration: broken-stdout incident → next healthy tick re-emits the
# ExecPending dispatch ONCE, coord launches once (gate), no phantom, no
# double-launch, fails-on-parent.
# ---------------------------------------------------------------------------


def test_integration_broken_stdout_then_healthy_tick(fleet_bin: str, home: Path) -> None:
    # 1. Dispatch recorded (inbox + journal ExecPending) but the launch
    #    block never reached the coord — the broken-stdout phantom.
    _acquire(fleet_bin, home, "11110001", "fix-foo")
    assert _journal(home, "11110001")["exec_state"] == "pending"

    # 2. Next healthy tick: replay re-emits exactly ONE block, carrying the
    #    journal's generation.
    actions = _replay(home, fleet_bin)
    blocks = [a for a in actions if a.dispatch_instruction]
    assert len(blocks) == 1
    gen = _journal(home, "11110001")["generation"]
    assert f"generation: {gen}" in blocks[0].dispatch_instruction

    # 3. Coord runs the launch gate ONCE → ok, flips to launch_attempted.
    assert dispatch_mod.mark_launch_attempted(
        "11110001", gen, fleet_bin=fleet_bin, fleet_home=str(home)) == "ok"

    # 4. A SECOND, stale replay block (same id, same gen) reaching the gate
    #    must predicate-fail — no double-launch.
    assert dispatch_mod.mark_launch_attempted(
        "11110001", gen, fleet_bin=fleet_bin, fleet_home=str(home)) == "predicate_fail"

    # 5. A subsequent replay tick (still launch_attempted, fresh) does NOT
    #    re-emit (the double-launch trap is closed).
    actions = _replay(home, fleet_bin, now_unix=time.time())
    assert [a for a in actions if a.dispatch_instruction] == []

    # 6. register flips to acked; release → done (not clobbered to a wrong
    #    state).
    assert dispatch_mod.mark_acked(
        "11110001", fleet_bin=fleet_bin, fleet_home=str(home)) == "acked"
    dispatch_mod.release_coord_prompt_inbox(
        "11110001", fleet_bin=fleet_bin, fleet_home=str(home))
    assert _journal(home, "11110001")["exec_state"] == "done"
