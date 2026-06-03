"""Chokepoint primitive + epoch tests (DESIGN-coord-worktree-lifecycle
PR2): the tri-state reader (T1) and dispatch-ordering / epoch (T3).

T2 (the writer CAS) lives Go-side in internal/workers/workers_test.go +
cmd/fleet/workers_test.go — it's a Go `fleet workers update` path.
"""
from __future__ import annotations

import datetime as _dt
import json
from pathlib import Path
from unittest.mock import patch

import pytest

import dispatch
import loop
import parse
import supervisor


# ---------- helpers ----------


def _write_worker_state(
    home: Path, project: str, slug: str, state: dict,
) -> None:
    d = home / "projects" / project / "workers" / slug
    d.mkdir(parents=True, exist_ok=True)
    (d / "state.json").write_text(json.dumps(state), encoding="utf-8")


def _make_task(slug: str, *, status: str = "ready", dispatch_generation: int = 0):
    return parse.Task(
        slug=slug, status=status, priority="P1",
        created=_dt.datetime(2026, 5, 6, 10, 0, 0, tzinfo=_dt.timezone.utc),
        updated=_dt.datetime(2026, 5, 6, 10, 0, 0, tzinfo=_dt.timezone.utc),
        spawned_by="user", spec="spec", acceptance="acc",
        dispatch_generation=dispatch_generation,
    )


# ======================================================================
# T1 — read_current_worker_state (the load-bearing P0): stale != missing
#      != current.
# ======================================================================


class TestReadCurrentWorkerState:
    def test_missing_when_no_state_file(self, tmp_path: Path) -> None:
        cls, st = loop.read_current_worker_state(
            "p", "no-such-slug", 1, home=tmp_path,
        )
        assert cls == loop.WORKER_STATE_MISSING
        assert st is None

    def test_current_when_generation_matches(self, tmp_path: Path) -> None:
        _write_worker_state(
            tmp_path, "p", "cur-aaaa",
            {"slug": "cur-aaaa", "phase": "tdd-green", "dispatch_generation": 3},
        )
        cls, st = loop.read_current_worker_state("p", "cur-aaaa", 3, home=tmp_path)
        assert cls == loop.WORKER_STATE_CURRENT
        assert st is not None and st["phase"] == "tdd-green"

    def test_stale_when_prior_generation(self, tmp_path: Path) -> None:
        # A prior attempt left a TERMINAL phase=done at gen 1; the slug's
        # authority is now 2 (re-dispatched). The reader must classify
        # this STALE — not missing, not current — so callers short-circuit
        # and never reap the live attempt.
        _write_worker_state(
            tmp_path, "p", "stale-bbbb",
            {
                "slug": "stale-bbbb", "phase": "done",
                "pr_url": "https://example.com/pr/old",
                "dispatch_generation": 1,
            },
        )
        cls, st = loop.read_current_worker_state(
            "p", "stale-bbbb", 2, home=tmp_path,
        )
        assert cls == loop.WORKER_STATE_STALE, "prior gen must be stale, not missing/current"
        # The state is returned (for surfacing) but the classification is
        # the load-bearing signal: callers act ONLY on `current`.
        assert st is not None and st["phase"] == "done"

    def test_legacy_zero_is_current_until_redispatched(self, tmp_path: Path) -> None:
        # A pre-migration state with no dispatch_generation key reads 0.
        # When the authority is also 0 (never re-dispatched), it's current;
        # once the slug is re-dispatched (authority >= 1) the legacy 0 is
        # fenced stale.
        _write_worker_state(
            tmp_path, "p", "legacy-cccc",
            {"slug": "legacy-cccc", "phase": "tdd-red"},  # no gen key
        )
        cls0, _ = loop.read_current_worker_state("p", "legacy-cccc", 0, home=tmp_path)
        assert cls0 == loop.WORKER_STATE_CURRENT
        cls1, _ = loop.read_current_worker_state("p", "legacy-cccc", 1, home=tmp_path)
        assert cls1 == loop.WORKER_STATE_STALE

    def test_corrupt_generation_treated_as_legacy_zero(self, tmp_path: Path) -> None:
        _write_worker_state(
            tmp_path, "p", "corrupt-dddd",
            {"slug": "corrupt-dddd", "phase": "done", "dispatch_generation": "x"},
        )
        # Non-int gen → 0 → fenced stale the moment authority >= 1.
        cls, _ = loop.read_current_worker_state("p", "corrupt-dddd", 2, home=tmp_path)
        assert cls == loop.WORKER_STATE_STALE


# ======================================================================
# T3 — dispatch ordering + epoch.
# ======================================================================


@pytest.fixture
def fleet_home(tmp_path: Path) -> Path:
    home = tmp_path / "fleet"
    (home / "inbox").mkdir(parents=True)
    (home / "projects").mkdir(parents=True)
    return home


def _dispatch_ready_one(
    fleet_home: Path, task: parse.Task, coord_state: dict | None,
):
    """Drive _dispatch_ready for a single ready task with all external
    shell-outs stubbed, returning the produced actions. mint_agent_id is
    pinned so assertions are deterministic; acquire/instruction are no-ops
    that succeed."""
    with patch.object(dispatch, "fetch_standards", return_value="# Standards"), \
         patch.object(dispatch, "fetch_learnings", return_value=""), \
         patch.object(dispatch, "project_is_git", return_value=True), \
         patch.object(dispatch, "mint_agent_id", return_value="aaaaaaaa"), \
         patch.object(
             dispatch, "acquire_coord_prompt_inbox",
             return_value=str(fleet_home / "inbox" / "aaaaaaaa.md"),
         ), \
         patch.object(
             dispatch, "format_dispatch_instruction", return_value="DISPATCH ...",
         ):
        return loop._dispatch_ready(
            tasks=[task],
            project="fleet",
            cwd="/repo",
            cap=1,
            fleet_bin="fleet",
            fleet_home=str(fleet_home),
            coord_state=coord_state,
        )


def test_genuine_redispatch_increments_generation(fleet_home: Path) -> None:
    # First dispatch of a fresh slug (task gen 0) → action gen 1.
    coord_state: dict = {}
    actions = _dispatch_ready_one(
        fleet_home, _make_task("epoch-aaaa", dispatch_generation=0), coord_state,
    )
    assert len(actions) == 1 and not actions[0].error
    assert actions[0].dispatch_generation == 1

    # A genuine re-dispatch (the slug's task row is now gen 1, e.g. a
    # prior attempt failed back to ready) → action gen 2. STRICTLY
    # increasing per slug, even though #184's journal Generation restarts
    # at 0 on every fresh agent_id.
    coord_state2: dict = {}
    actions2 = _dispatch_ready_one(
        fleet_home, _make_task("epoch-aaaa", dispatch_generation=1), coord_state2,
    )
    assert actions2[0].dispatch_generation == 2


def test_pending_acquire_retry_reuses_generation_and_records_kind(
    fleet_home: Path,
) -> None:
    # A prior tick acquired (or errored mid-acquire) and stored a worker
    # record at gen 5. The task row is still gen 4 (the flip never landed
    # because apply hasn't run). A retry must REUSE gen 5 + the same
    # agent_id — re-incrementing would skew the task row to 5 while the
    # prompt says 5... actually skew it to expect 5 vs a re-mint's 6.
    coord_state: dict = {}
    supervisor.remember_pending_acquire_record(
        coord_state, "retry-bbbb", "deadc0de", 5, "worker",
    )
    with patch.object(dispatch, "fetch_standards", return_value="# Standards"), \
         patch.object(dispatch, "fetch_learnings", return_value=""), \
         patch.object(dispatch, "project_is_git", return_value=True), \
         patch.object(dispatch, "mint_agent_id", return_value="ffffffff"), \
         patch.object(
             dispatch, "acquire_coord_prompt_inbox",
             return_value=str(fleet_home / "inbox" / "deadc0de.md"),
         ) as acq, \
         patch.object(
             dispatch, "format_dispatch_instruction", return_value="DISPATCH ...",
         ):
        actions = loop._dispatch_ready(
            tasks=[_make_task("retry-bbbb", dispatch_generation=4)],
            project="fleet", cwd="/repo", cap=1,
            fleet_bin="fleet", fleet_home=str(fleet_home),
            coord_state=coord_state,
        )
    assert len(actions) == 1 and not actions[0].error
    # Reused the recorded gen (5), NOT t.dispatch_generation+1 (=5 here by
    # coincidence) — make the point unambiguous by checking the agent_id
    # is the recorded one, proving the reuse branch fired.
    assert actions[0].agent_id == "deadc0de"
    assert actions[0].dispatch_generation == 5
    # acquire was called with the REUSED id (recovery path), not the mint.
    assert acq.call_args.args[0] == "deadc0de"
    # The pending record persisted carries {agent_id, gen, kind}.
    rec = supervisor.load_pending_acquire_record_map(coord_state).get("retry-bbbb")
    assert rec == {
        "agent_id": "deadc0de", "dispatch_generation": 5, "dispatch_kind": "worker",
    }


def test_gen_inconsistent_worker_record_is_forgotten(fleet_home: Path) -> None:
    # Codex iter-2 [P2]: a worker-kind pending record whose recorded gen
    # is inconsistent with the current task row (the slug was reset/re-
    # dispatched out from under it) must NOT be reused — reusing would
    # write a stale gen back + reuse an old prompt under an old CAS token.
    # Forget it + mint fresh at next_gen.
    coord_state: dict = {}
    # Record gen 2; but the task row has advanced to gen 7 (re-dispatched
    # twice since), so the record is stale. next_gen would be 8.
    supervisor.remember_pending_acquire_record(
        coord_state, "skew-eeee", "deadc0de", 2, "worker",
    )
    with patch.object(dispatch, "fetch_standards", return_value="# Standards"), \
         patch.object(dispatch, "fetch_learnings", return_value=""), \
         patch.object(dispatch, "project_is_git", return_value=True), \
         patch.object(dispatch, "mint_agent_id", return_value="ffffffff"), \
         patch.object(
             dispatch, "acquire_coord_prompt_inbox",
             return_value=str(fleet_home / "inbox" / "ffffffff.md"),
         ) as acq, \
         patch.object(dispatch, "format_dispatch_instruction", return_value="DISPATCH ..."):
        actions = loop._dispatch_ready(
            tasks=[_make_task("skew-eeee", dispatch_generation=7)],
            project="fleet", cwd="/repo", cap=1,
            fleet_bin="fleet", fleet_home=str(fleet_home),
            coord_state=coord_state,
        )
    assert len(actions) == 1 and not actions[0].error
    # Stale record discarded → fresh mint + next_gen (8), NOT the recorded
    # agent/gen.
    assert actions[0].agent_id == "ffffffff"
    assert actions[0].dispatch_generation == 8
    assert acq.call_args.args[0] == "ffffffff"


def test_apply_dispatch_persists_generation_with_in_progress_flip(
    fleet_home: Path,
) -> None:
    # _apply_dispatch persists dispatch_generation in the pre-launch
    # commit, BEFORE state.json bootstrap. codex iter-2 [P1]: the gen set
    # must come BEFORE the status=in-progress flip — `fleet tasks set` is
    # one field per call (not atomic across two), so a crash between them
    # must NOT leave the row in-progress at the OLD generation (which would
    # let a stale prior state read `current`). Committing gen first leaves
    # any crash window at status=ready under the NEW gen (re-dispatchable),
    # never in-progress-under-old-gen. No launchable DISPATCH is emitted
    # mid-apply (the caller collects the block only after this returns), so
    # the brief ready window cannot double-dispatch.
    calls: list[list[str]] = []
    with patch.object(loop, "_run_fleet", side_effect=lambda cmd, timeout_s=30.0: calls.append(list(cmd))):
        loop._apply_dispatch(
            loop._DispatchAction(
                slug="order-cccc", agent_id="aaaaaaaa", branch="worker/order-cccc",
                dispatch_instruction="DISPATCH ...", dispatch_generation=2,
            ),
            "fleet", "fleet",
        )

    def idx(pred) -> int:
        for i, c in enumerate(calls):
            if pred(c):
                return i
        return -1

    i_status = idx(lambda c: c[1:3] == ["tasks", "set"] and "status=in-progress" in c)
    i_gen = idx(lambda c: c[1:3] == ["tasks", "set"] and "dispatch_generation=2" in c)
    i_bootstrap = idx(
        lambda c: c[1:3] == ["workers", "update"] and "starting" in c,
    )
    assert i_gen == 0, "dispatch_generation must be the FIRST mutation (before status)"
    assert i_status > i_gen, "status=in-progress comes AFTER the gen commit (atomicity-safe order)"
    assert i_bootstrap > i_status, "state.json bootstrap comes AFTER the pre-launch commit"
    # The bootstrap state write carries --dispatch-generation so the
    # bootstrapped state.json reads `current` (not legacy-0 / stale).
    assert "--dispatch-generation" in calls[i_bootstrap]
    assert "2" in calls[i_bootstrap]


def test_handoff_reader_short_circuits_on_stale(fleet_home: Path) -> None:
    # R8 (DESIGN §2.1/§3): a STALE prior-generation review-pending state
    # must NOT dispatch a reviewer onto the re-dispatched slug. The task
    # row authority is gen 2; the leftover state.json is gen 1.
    task = _make_task("ho-stale", status="in-progress", dispatch_generation=2)
    _write_worker_state(
        fleet_home, "fleet", "ho-stale",
        {"slug": "ho-stale", "phase": "review-pending", "dispatch_generation": 1},
    )
    with patch.object(dispatch, "project_is_git", return_value=True), \
         patch.object(dispatch, "build_reviewer_prompt") as rb, \
         patch.object(dispatch, "build_finisher_prompt") as fb:
        actions = loop._dispatch_review_handoffs(
            tasks=[task], project="fleet", fleet_bin="fleet",
            fleet_home=str(fleet_home), home=fleet_home, coord_state={},
        )
    assert actions == [], "stale handoff must not dispatch"
    rb.assert_not_called()
    fb.assert_not_called()


def test_handoff_reader_dispatches_on_current(fleet_home: Path) -> None:
    # The same review-pending state at the CURRENT gen DOES dispatch a
    # reviewer (proving the short-circuit is gen-specific, not blanket).
    task = _make_task("ho-cur", status="in-progress", dispatch_generation=2)
    _write_worker_state(
        fleet_home, "fleet", "ho-cur",
        {"slug": "ho-cur", "phase": "review-pending", "dispatch_generation": 2},
    )
    with patch.object(dispatch, "project_is_git", return_value=True), \
         patch.object(dispatch, "build_reviewer_prompt", return_value="reviewer prompt") as rb, \
         patch.object(dispatch, "mint_agent_id", return_value="bbbbbbbb"), \
         patch.object(
             dispatch, "acquire_coord_prompt_inbox",
             return_value=str(fleet_home / "inbox" / "bbbbbbbb.md"),
         ), \
         patch.object(dispatch, "format_dispatch_instruction", return_value="DISPATCH ..."):
        actions = loop._dispatch_review_handoffs(
            tasks=[task], project="fleet", fleet_bin="fleet",
            fleet_home=str(fleet_home), home=fleet_home, coord_state={},
        )
    assert len(actions) == 1 and not actions[0].error
    assert actions[0].handoff_phase == "review-pending"
    # The reviewer prompt INHERITS the slug's current gen (no increment).
    assert actions[0].dispatch_generation == 2
    assert rb.call_args.kwargs["dispatch_generation"] == 2


def test_no_launchable_dispatch_emitted_while_ready(fleet_home: Path) -> None:
    # Structural invariant: _dispatch_ready does NOT itself flip the task
    # row (it only PROPOSES actions); the status=in-progress flip happens
    # in _apply_dispatch, which the consumer runs BEFORE collecting the
    # DISPATCH block. So a tick that builds an action leaves the task at
    # `ready` on disk until apply runs — proving no launchable block can
    # be emitted while the slug is still ready.
    coord_state: dict = {}
    task = _make_task("emit-dddd", dispatch_generation=0)
    actions = _dispatch_ready_one(fleet_home, task, coord_state)
    assert actions and actions[0].dispatch_generation == 1
    # _dispatch_ready performed no task-row mutation: the in-memory task
    # status is untouched (the apply step owns the durable flip).
    assert task.status == "ready"
