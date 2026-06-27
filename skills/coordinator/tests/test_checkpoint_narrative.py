"""Slice 3 Key Decisions seam tests (loop.py).

The tick records a capped rolling `recent_decisions` buffer into
coord-state.json that the successor's handoff doc lifts into Key Decisions
(every applied dispatch / reconcile / sentinel action — the "why").

These tests pin the shared `_record_decision` / `_decision_line` seam — the
action→line classification — without standing up a full tick. The contract:
dispatch / worker_failed are decisions but NOT completions (the Slice 2
recent_completions buffer, wired separately in loop.py, must stay empty for
a start or a requeue).
"""
from __future__ import annotations

import loop


def _dispatch(**kw):
    return loop._DispatchAction(slug=kw.pop("slug", "s-1111"), **kw)


def _reconcile(**kw):
    return loop._ReconcileAction(slug=kw.pop("slug", "s-1111"), **kw)


def _sentinel(kind, slug="s-1111", **kw):
    return loop._SentinelAction(slug=slug, kind=kind, **kw)


# ---------- _decision_line classification ----------


def test_decision_line_dispatch_worker():
    line = loop._decision_line(_dispatch(
        agent_id="aaaa", dispatch_instruction="DISPATCH ...", dispatch_generation=2,
    ))
    assert line == "dispatched worker s-1111 (gen 2)"


def test_decision_line_dispatch_reviewer_finisher():
    rev = loop._decision_line(_dispatch(agent_id="a", handoff_phase="review-pending"))
    fin = loop._decision_line(_dispatch(agent_id="a", handoff_phase="review-done"))
    assert rev == "dispatched reviewer for s-1111"
    assert fin == "dispatched finisher for s-1111"


def test_decision_line_dispatch_error_is_empty():
    """A failed / no-op dispatch is NOT a decision (no noise)."""
    assert loop._decision_line(_dispatch(error="acquire failed")) == ""
    assert loop._decision_line(_dispatch()) == ""  # no instruction, no agent_id


def test_decision_line_reconcile_status_and_raise():
    assert loop._decision_line(_reconcile(new_status="done")) == "reconciled s-1111 → done"
    raised = loop._decision_line(_reconcile(raised_to_user=True, raise_text="CI red"))
    assert raised == "raised hand: s-1111 — CI red"
    assert loop._decision_line(_reconcile()) == ""  # status-less reconcile


def test_decision_line_reconcile_status_and_raise_together():
    """A reconcile that BOTH moves the task AND raises must record the
    status transition, not just the alert prose (codex P2: recovering a
    shipped PR sets new_status AND raised_to_user)."""
    both = loop._decision_line(_reconcile(
        new_status="in-review", raised_to_user=True, raise_text="needs review",
    ))
    assert both == "reconciled s-1111 → in-review, raised hand — needs review"
    # raise without text still keeps the transition.
    no_text = loop._decision_line(_reconcile(new_status="done", raised_to_user=True))
    assert no_text == "reconciled s-1111 → done, raised hand"


def test_decision_line_sentinel_kinds():
    assert loop._decision_line(_sentinel("task_done_pr")) == "worker s-1111 finished → in-review"
    assert loop._decision_line(_sentinel("worker_failed")) == "requeued worker-failed task s-1111"
    assert loop._decision_line(_sentinel("blocked_question")) == "parked task s-1111: blocked question"
    # new_task is plumbing, not a decision.
    assert loop._decision_line(_sentinel("new_task")) == ""


# ---------- _record_decision buffer ----------


def test_record_decision_writes_into_state():
    state = {}
    loop._record_decision(state, _dispatch(agent_id="a", dispatch_instruction="x", dispatch_generation=1))
    assert state["recent_decisions"] == ["dispatched worker s-1111 (gen 1)"]


def test_record_decision_noop_action_writes_nothing():
    state = {}
    loop._record_decision(state, _dispatch(error="boom"))
    assert state.get("recent_decisions", []) == []


def test_dispatch_and_worker_failed_are_decisions_not_completions():
    """TASK-PLAN T3: a tick with only a dispatch + a worker_failed records
    DECISIONS but appends NOTHING to recent_completions."""
    state = {}
    loop._record_decision(state, _dispatch(agent_id="a", dispatch_instruction="x", dispatch_generation=1))
    loop._record_decision(state, _sentinel("worker_failed", slug="failed-2222"))
    assert state["recent_decisions"] == [
        "dispatched worker s-1111 (gen 1)",
        "requeued worker-failed task failed-2222",
    ]
    # The completion buffer must stay empty — neither is a true completion.
    assert state.get("recent_completions", []) == []
