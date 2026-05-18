"""Tests for branch->PR fallback + _apply_reconcile ordering fix.

Covers the recovery path for tasks whose worker shipped a PR outside the
v0.2 state-machine contract (off-rails finisher / operator manual merge).
State.json is stuck at phase=review-pending or never reached phase=done,
tasks.md.pr_url stays empty, but a merged PR exists on the branch — the
fallback uses `gh pr list --head <branch>` to detect that and flip the
task to done.

Design: docs/DESIGN-reconcile-pr-by-branch.md v2.

Strategy mirrors test_loop.py: stub `_run_fleet`, fabricate state.json on
disk under FLEET_HOME, and monkeypatch `_gh_pr_by_branch` (or its
underlying subprocess) to feed canned PR results.
"""
from __future__ import annotations

import datetime as _dt
import json
import subprocess
from pathlib import Path
from unittest.mock import patch

import pytest

import loop
import parse


# ---------- helpers (kept independent of test_loop.py to avoid coupling) ----------


def _now_z() -> str:
    return _dt.datetime.now(tz=_dt.timezone.utc).isoformat().replace("+00:00", "Z")


def _make_task(
    slug: str,
    *,
    status: str = "in-progress",
    pr_url: str = "",
    branch: str = "",
    worker_pid: int = 0,
) -> parse.Task:
    return parse.Task(
        slug=slug, status=status, priority="P1",
        worker_pid=worker_pid, pr_url=pr_url, branch=branch,
        created=_dt.datetime(2026, 5, 6, 10, 0, 0, tzinfo=_dt.timezone.utc),
        updated=_dt.datetime(2026, 5, 6, 10, 0, 0, tzinfo=_dt.timezone.utc),
        spawned_by="user", depends_on=[], spec="spec", acceptance="acc",
    )


def _write_state(home: Path, project: str, slug: str, body: dict) -> None:
    d = home / "projects" / project / "workers" / slug
    d.mkdir(parents=True, exist_ok=True)
    (d / "state.json").write_text(json.dumps(body), encoding="utf-8")


@pytest.fixture
def fleet_home(tmp_path: Path) -> Path:
    home = tmp_path / "fleet"
    home.mkdir()
    (home / "projects").mkdir()
    return home


# ---------- Part A: _gh_pr_by_branch helper ----------


def test_gh_pr_by_branch_picks_newest_by_created_at(monkeypatch) -> None:
    """Test #3: multiple PRs for the same branch — fallback picks the
    newest by createdAt; older PR ignored. A retry that created PR #2 on
    the same branch must win over the stale PR #1."""
    rows = [
        {
            "number": 1, "state": "CLOSED",
            "url": "https://github.com/x/y/pull/1",
            "mergedAt": None,
            "createdAt": "2026-05-17T00:00:00Z",
            "updatedAt": "2026-05-17T00:00:00Z",
            "headRefName": "worker/foo",
        },
        {
            "number": 2, "state": "MERGED",
            "url": "https://github.com/x/y/pull/2",
            "mergedAt": "2026-05-18T01:00:00Z",
            "createdAt": "2026-05-18T00:30:00Z",
            "updatedAt": "2026-05-18T01:00:00Z",
            "headRefName": "worker/foo",
        },
    ]

    def fake_run(cmd, capture_output=True, text=True, timeout=None, check=False):
        return subprocess.CompletedProcess(
            args=cmd, returncode=0, stdout=json.dumps(rows), stderr="",
        )

    monkeypatch.setattr(loop.subprocess, "run", fake_run)

    pr = loop._gh_pr_by_branch("worker/foo")
    assert pr is not None
    assert pr.number == 2
    assert pr.url == "https://github.com/x/y/pull/2"
    assert pr.merged_at == "2026-05-18T01:00:00Z"
    assert pr.state == "MERGED"


def test_gh_pr_by_branch_returns_none_on_empty(monkeypatch) -> None:
    """Test #4: no PR found → None (caller emits no action)."""
    def fake_run(cmd, capture_output=True, text=True, timeout=None, check=False):
        return subprocess.CompletedProcess(
            args=cmd, returncode=0, stdout="[]", stderr="",
        )

    monkeypatch.setattr(loop.subprocess, "run", fake_run)
    assert loop._gh_pr_by_branch("worker/missing") is None


def test_gh_pr_by_branch_returns_none_on_timeout(monkeypatch) -> None:
    """Test #6: subprocess timeout → None. The fallback must not
    requeue a stale handoff just because GitHub was unavailable."""
    def fake_run(cmd, capture_output=True, text=True, timeout=None, check=False):
        raise subprocess.TimeoutExpired(cmd=cmd, timeout=timeout or 5)

    monkeypatch.setattr(loop.subprocess, "run", fake_run)
    assert loop._gh_pr_by_branch("worker/foo") is None


def test_gh_pr_by_branch_returns_none_on_error(monkeypatch) -> None:
    """Test #7: gh exits non-zero → None."""
    def fake_run(cmd, capture_output=True, text=True, timeout=None, check=False):
        return subprocess.CompletedProcess(
            args=cmd, returncode=1, stdout="", stderr="auth required",
        )

    monkeypatch.setattr(loop.subprocess, "run", fake_run)
    assert loop._gh_pr_by_branch("worker/foo") is None


def test_gh_pr_by_branch_uses_head_filter_not_search(monkeypatch) -> None:
    """Lookup contract (codex round 1 tightening): use `--head <branch>`
    NOT `--search head:<branch>`. Search-string matching is fuzzy and
    can pick the wrong PR when retry created multiple PRs on the same
    branch.
    """
    captured: list[list[str]] = []

    def fake_run(cmd, capture_output=True, text=True, timeout=None, check=False):
        captured.append(list(cmd))
        return subprocess.CompletedProcess(
            args=cmd, returncode=0, stdout="[]", stderr="",
        )

    monkeypatch.setattr(loop.subprocess, "run", fake_run)
    loop._gh_pr_by_branch("worker/foo")
    assert len(captured) == 1
    cmd = captured[0]
    assert "--head" in cmd
    assert "worker/foo" in cmd
    assert "--search" not in cmd
    # State filter must include all PR states so we catch CLOSED/MERGED.
    assert "--state" in cmd
    si = cmd.index("--state")
    assert cmd[si + 1] == "all"


# ---------- Part A: _reconcile_inflight fallback ----------


def test_reconcile_stale_review_pending_with_merged_pr_flips_done(
    fleet_home: Path, monkeypatch,
) -> None:
    """Test #1 (load-bearing): stale state.json at phase=review-pending,
    tasks.md.pr_url empty, worker dead — but a merged PR exists on the
    branch. The fallback MUST run BEFORE the review-pending short-circuit
    at loop.py:2346, else the task stays invisible forever.

    Expected action: new_status=done, set_pr_url=<url>, delete_worker_dir=True.
    """
    monkeypatch.setenv("FLEET_HOME", str(fleet_home))
    project = "fleet"

    # Stale state.json — frozen at review-pending. The mtime/freshness
    # check would treat this as dead so reconcile gets to the fallback.
    stale = _dt.datetime(2026, 5, 17, 7, 47, tzinfo=_dt.timezone.utc).isoformat().replace(
        "+00:00", "Z",
    )
    _write_state(fleet_home, project, "stuck-task", {
        "slug": "stuck-task", "project": project,
        "phase": "review-pending", "updated_at": stale,
    })

    t = _make_task(
        "stuck-task", status="in-progress",
        worker_pid=99999, pr_url="", branch="worker/stuck-task",
    )

    pr = loop._PRSummary(
        number=42, state="MERGED",
        url="https://github.com/x/y/pull/42",
        merged_at="2026-05-18T04:52:00Z",
        created_at="2026-05-17T05:00:00Z",
    )

    with patch.object(loop, "_pid_alive", return_value=False), \
         patch.object(loop, "_gh_pr_by_branch", return_value=pr):
        actions = loop._reconcile_inflight([t], project, "fleet", home=fleet_home)

    assert len(actions) == 1, f"expected 1 action, got {actions}"
    a = actions[0]
    assert a.slug == "stuck-task"
    assert a.new_status == "done"
    assert a.set_pr_url == "https://github.com/x/y/pull/42"
    assert a.delete_worker_dir is True
    assert a.clear_worker is True


def test_reconcile_stale_review_pending_with_open_pr_flips_in_review(
    fleet_home: Path, monkeypatch,
) -> None:
    """Test #2: stale state.json at phase=review-pending, tasks.md.pr_url
    empty, worker dead — but an OPEN (not merged) PR exists on the branch.
    Flip to in-review with the PR URL set; the next tick's gh pr checks
    path then drives CI to done.

    State.json is left untouched (the fallback only writes via the
    fleet CLI action queue; it does NOT mutate the worker dir directly).
    """
    monkeypatch.setenv("FLEET_HOME", str(fleet_home))
    project = "fleet"

    stale = _dt.datetime(2026, 5, 17, 7, 47, tzinfo=_dt.timezone.utc).isoformat().replace(
        "+00:00", "Z",
    )
    _write_state(fleet_home, project, "open-pr", {
        "slug": "open-pr", "project": project,
        "phase": "review-pending", "updated_at": stale,
    })

    t = _make_task(
        "open-pr", status="in-progress",
        worker_pid=99999, pr_url="", branch="worker/open-pr",
    )

    pr = loop._PRSummary(
        number=43, state="OPEN",
        url="https://github.com/x/y/pull/43",
        merged_at=None,
        created_at="2026-05-18T00:30:00Z",
    )

    with patch.object(loop, "_pid_alive", return_value=False), \
         patch.object(loop, "_gh_pr_by_branch", return_value=pr):
        actions = loop._reconcile_inflight([t], project, "fleet", home=fleet_home)

    assert len(actions) == 1
    a = actions[0]
    assert a.new_status == "in-review"
    assert a.set_pr_url == "https://github.com/x/y/pull/43"
    # Open PR: worker dir should be cleared (worker is gone) but NOT
    # rm-rf'd — the CI poll path still owns the next transition.
    assert a.clear_worker is True
    assert a.delete_worker_dir is False

    # State.json must be untouched on disk.
    raw = json.loads(
        (fleet_home / "projects" / project / "workers" / "open-pr" / "state.json").read_text(
            encoding="utf-8",
        )
    )
    assert raw["phase"] == "review-pending"


def test_reconcile_closed_unmerged_pr_no_action(
    fleet_home: Path, monkeypatch,
) -> None:
    """Test #5: branch has a CLOSED-unmerged PR — leave the task untouched.
    Operator decides what to do (re-open the PR, retry, abandon).
    Importantly: we do NOT requeue to todo; that would lose the worker's
    history.
    """
    monkeypatch.setenv("FLEET_HOME", str(fleet_home))
    project = "fleet"

    stale = _dt.datetime(2026, 5, 17, 7, 47, tzinfo=_dt.timezone.utc).isoformat().replace(
        "+00:00", "Z",
    )
    _write_state(fleet_home, project, "closed-pr", {
        "slug": "closed-pr", "project": project,
        "phase": "review-pending", "updated_at": stale,
    })

    t = _make_task(
        "closed-pr", status="in-progress",
        worker_pid=99999, pr_url="", branch="worker/closed-pr",
    )

    pr = loop._PRSummary(
        number=44, state="CLOSED",
        url="https://github.com/x/y/pull/44",
        merged_at=None,
        created_at="2026-05-18T00:30:00Z",
    )

    with patch.object(loop, "_pid_alive", return_value=False), \
         patch.object(loop, "_gh_pr_by_branch", return_value=pr):
        actions = loop._reconcile_inflight([t], project, "fleet", home=fleet_home)

    assert actions == [], f"closed-unmerged must emit no action, got {actions}"


def test_reconcile_no_pr_found_no_action(
    fleet_home: Path, monkeypatch,
) -> None:
    """Test #4 (reconcile-level): _gh_pr_by_branch returns None → fallback
    is a no-op. The existing decision paths (terminal-state / pr_url /
    'died without PR') still run.

    Because state.json phase is review-pending, the mid-phase short-circuit
    at loop.py:2346 fires and no action is emitted from there either.
    """
    monkeypatch.setenv("FLEET_HOME", str(fleet_home))
    project = "fleet"

    stale = _dt.datetime(2026, 5, 17, 7, 47, tzinfo=_dt.timezone.utc).isoformat().replace(
        "+00:00", "Z",
    )
    _write_state(fleet_home, project, "no-pr", {
        "slug": "no-pr", "project": project,
        "phase": "review-pending", "updated_at": stale,
    })

    t = _make_task(
        "no-pr", status="in-progress",
        worker_pid=99999, pr_url="", branch="worker/no-pr",
    )

    with patch.object(loop, "_pid_alive", return_value=False), \
         patch.object(loop, "_gh_pr_by_branch", return_value=None):
        actions = loop._reconcile_inflight([t], project, "fleet", home=fleet_home)

    assert actions == [], f"no PR found must emit no action, got {actions}"


def test_reconcile_task_with_pr_url_skips_fallback(
    fleet_home: Path, monkeypatch,
) -> None:
    """Test #8: task already has pr_url → fallback is skipped; the
    existing gh pr checks path owns the decision. The fallback exists
    only to RECOVER tasks where pr_url was never recorded.
    """
    monkeypatch.setenv("FLEET_HOME", str(fleet_home))
    project = "fleet"
    # No state.json — worker dead and gone.

    t = _make_task(
        "has-pr", status="in-review",
        worker_pid=0, pr_url="https://github.com/x/y/pull/99",
        branch="worker/has-pr",
    )

    # If the fallback ran, it would call _gh_pr_by_branch. Patch it to
    # raise so any accidental invocation is loud.
    def boom(*a, **kw):
        raise AssertionError("fallback must not run when pr_url is set")

    # Stub gh pr checks to return green-merged so the existing path still
    # produces a sensible result and we can confirm it ran (not the fallback).
    ci = loop._CIResult(all_green=True, merged=True)

    with patch.object(loop, "_pid_alive", return_value=False), \
         patch.object(loop, "_gh_pr_by_branch", side_effect=boom), \
         patch.object(loop, "_gh_pr_checks", return_value=ci):
        actions = loop._reconcile_inflight([t], project, "fleet", home=fleet_home)

    # Existing path produced the "merged → done" transition.
    assert len(actions) == 1
    assert actions[0].new_status == "done"


def test_reconcile_task_without_branch_skips_fallback(
    fleet_home: Path, monkeypatch,
) -> None:
    """Test #9: task has no branch → fallback is skipped; no gh call.
    Branch is the only durable handle for the lookup; without it the
    fallback would shell out for nothing.
    """
    monkeypatch.setenv("FLEET_HOME", str(fleet_home))
    project = "fleet"

    stale = _dt.datetime(2026, 5, 17, 7, 47, tzinfo=_dt.timezone.utc).isoformat().replace(
        "+00:00", "Z",
    )
    _write_state(fleet_home, project, "no-branch", {
        "slug": "no-branch", "project": project,
        "phase": "review-pending", "updated_at": stale,
    })

    t = _make_task(
        "no-branch", status="in-progress",
        worker_pid=99999, pr_url="", branch="",
    )

    def boom(*a, **kw):
        raise AssertionError("no branch → no gh call")

    with patch.object(loop, "_pid_alive", return_value=False), \
         patch.object(loop, "_gh_pr_by_branch", side_effect=boom):
        actions = loop._reconcile_inflight([t], project, "fleet", home=fleet_home)

    # mid-phase short-circuit fires; no action.
    assert actions == []


def test_reconcile_fallback_runs_before_review_pending_short_circuit(
    fleet_home: Path, monkeypatch,
) -> None:
    """Test #1 corollary (codex-driven): explicitly verify ordering —
    the fallback fires for a task whose state.json says phase=review-pending,
    PROVING the fallback runs BEFORE the mid-phase short-circuit at
    loop.py:2346. Without this ordering, stuck-handoff tasks remain
    invisible to reconcile forever (the symptom rc-listener-impl-v0-12-ed95
    exhibited).
    """
    monkeypatch.setenv("FLEET_HOME", str(fleet_home))
    project = "fleet"

    stale = _dt.datetime(2026, 5, 17, 7, 47, tzinfo=_dt.timezone.utc).isoformat().replace(
        "+00:00", "Z",
    )
    # phase=review-pending: if fallback runs AFTER the short-circuit, the
    # short-circuit `continue` swallows this task and no action emitted.
    _write_state(fleet_home, project, "ordering-test", {
        "slug": "ordering-test", "project": project,
        "phase": "review-pending", "updated_at": stale,
    })

    t = _make_task(
        "ordering-test", status="in-progress",
        worker_pid=99999, pr_url="", branch="worker/ordering-test",
    )

    pr = loop._PRSummary(
        number=99, state="MERGED",
        url="https://github.com/x/y/pull/99",
        merged_at="2026-05-18T04:00:00Z",
        created_at="2026-05-17T00:00:00Z",
    )

    with patch.object(loop, "_pid_alive", return_value=False), \
         patch.object(loop, "_gh_pr_by_branch", return_value=pr):
        actions = loop._reconcile_inflight([t], project, "fleet", home=fleet_home)

    # If the short-circuit fired first, actions would be []. The fact
    # that we got an action proves the fallback ran first.
    assert len(actions) == 1
    assert actions[0].new_status == "done"
    assert actions[0].set_pr_url == "https://github.com/x/y/pull/99"


# ---------- Reviewer round 1 (codex [P1]): terminal state wins over branch fallback ----------


def test_terminal_failed_state_wins_over_stale_branch_pr(
    fleet_home: Path, monkeypatch,
) -> None:
    """Codex reviewer round 1 [P1] regression: a re-dispatched worker that
    writes phase=failed must NOT be overridden by a stale PR left on the
    same branch by a prior attempt.

    Scenario: worker for slug X shipped a PR. CI went red. Reconcile
    flipped to todo and cleared pr_url. Operator re-dispatched. New
    worker died with phase=failed on the SAME branch. The branch still
    has the prior OPEN PR on GitHub.

    Correct behavior: classify as failed (requeue to todo + clear_pr_url +
    delete_worker_dir). The stale branch PR must NOT override.

    Before the fix this masked fresh terminal failures behind stale
    branch state — silent regression.
    """
    monkeypatch.setenv("FLEET_HOME", str(fleet_home))
    project = "fleet"

    _write_state(fleet_home, project, "retry-fail", {
        "slug": "retry-fail", "project": project,
        "phase": "failed", "updated_at": _now_z(),
    })

    t = _make_task(
        "retry-fail", status="in-progress",
        worker_pid=99999, pr_url="", branch="worker/retry-fail",
    )

    # Stale OPEN PR from a prior attempt — must NOT be acted on.
    stale_pr = loop._PRSummary(
        number=200, state="OPEN",
        url="https://github.com/x/y/pull/200",
        merged_at=None,
        created_at="2026-05-15T00:00:00Z",
    )

    with patch.object(loop, "_pid_alive", return_value=False), \
         patch.object(loop, "_gh_pr_by_branch", return_value=stale_pr) as gh_mock:
        actions = loop._reconcile_inflight([t], project, "fleet", home=fleet_home)

    assert len(actions) == 1, f"expected 1 action, got {actions}"
    a = actions[0]
    # Fresh terminal state wins: requeue to todo with clear_pr_url=True
    # and delete_worker_dir=True (matches the `phase == "failed"` branch
    # in _reconcile_inflight).
    assert a.new_status == "todo", f"expected todo, got {a.new_status}"
    assert a.clear_pr_url is True, "must clear pr_url on failed retry"
    assert a.delete_worker_dir is True
    # set_pr_url must NOT be the stale URL.
    assert a.set_pr_url == "", f"stale PR leaked into action: {a.set_pr_url}"
    # And critically, _gh_pr_by_branch must NOT have been called (terminal
    # state fires first; the fallback never runs in this code path).
    assert gh_mock.call_count == 0, (
        "branch->PR lookup must not run when terminal state is present "
        "(codex review round 1 [P1])"
    )


def test_terminal_blocked_state_wins_over_stale_branch_pr(
    fleet_home: Path, monkeypatch,
) -> None:
    """Codex reviewer round 1 [P1] regression (sibling): same constraint
    for phase=blocked. The blocked reason is the load-bearing signal —
    a stale branch PR must not mask it.
    """
    monkeypatch.setenv("FLEET_HOME", str(fleet_home))
    project = "fleet"

    _write_state(fleet_home, project, "retry-blocked", {
        "slug": "retry-blocked", "project": project,
        "phase": "blocked", "blocked_reason": "needs operator decision",
        "updated_at": _now_z(),
    })

    t = _make_task(
        "retry-blocked", status="in-progress",
        worker_pid=99999, pr_url="", branch="worker/retry-blocked",
    )

    stale_pr = loop._PRSummary(
        number=201, state="MERGED",
        url="https://github.com/x/y/pull/201",
        merged_at="2026-05-15T01:00:00Z",
        created_at="2026-05-15T00:00:00Z",
    )

    with patch.object(loop, "_pid_alive", return_value=False), \
         patch.object(loop, "_gh_pr_by_branch", return_value=stale_pr) as gh_mock:
        actions = loop._reconcile_inflight([t], project, "fleet", home=fleet_home)

    assert len(actions) == 1
    a = actions[0]
    assert a.new_status == "blocked"
    assert "needs operator decision" in (a.note or a.raise_text or "")
    assert a.set_pr_url == ""
    assert gh_mock.call_count == 0


def test_terminal_done_with_pr_url_wins_over_stale_branch_pr(
    fleet_home: Path, monkeypatch,
) -> None:
    """Codex reviewer round 1 [P1] regression (sibling): phase=done with
    pr_url written by the worker must win. The worker's pr_url is the
    authoritative signal; a stale branch PR (e.g., a closed-without-merge
    PR from a prior attempt) must not be picked up instead.
    """
    monkeypatch.setenv("FLEET_HOME", str(fleet_home))
    project = "fleet"

    _write_state(fleet_home, project, "done-fresh", {
        "slug": "done-fresh", "project": project,
        "phase": "done",
        "pr_url": "https://github.com/x/y/pull/300",
        "updated_at": _now_z(),
    })

    t = _make_task(
        "done-fresh", status="in-progress",
        worker_pid=99999, pr_url="", branch="worker/done-fresh",
    )

    stale_pr = loop._PRSummary(
        number=299, state="CLOSED",
        url="https://github.com/x/y/pull/299",
        merged_at=None,
        created_at="2026-05-15T00:00:00Z",
    )

    with patch.object(loop, "_pid_alive", return_value=False), \
         patch.object(loop, "_gh_pr_by_branch", return_value=stale_pr) as gh_mock:
        actions = loop._reconcile_inflight([t], project, "fleet", home=fleet_home)

    assert len(actions) == 1
    a = actions[0]
    assert a.new_status == "in-review"
    # The fresh state.json pr_url must be used, NOT the stale branch PR.
    assert a.set_pr_url == "https://github.com/x/y/pull/300"
    assert gh_mock.call_count == 0


# ---------- Part C: periodic supervisor reconcile catches stale state.json ----------


def test_periodic_reconcile_catches_stuck_without_mtime_change(
    fleet_home: Path, monkeypatch,
) -> None:
    """Test #10 (codex-driven): the supervisor's periodic tick calls
    _reconcile_inflight(); the fallback runs there too. Simulates the
    case where state.json is STALE — its mtime has not changed since
    the worker exited — and a merged PR exists on the branch.

    No supervisor-side change is required: this test just confirms that
    calling _reconcile_inflight with a stale state.json yields the
    recovery action, independent of any mtime trigger.
    """
    monkeypatch.setenv("FLEET_HOME", str(fleet_home))
    project = "fleet"

    # Write state.json with a stale updated_at (well past _WORKER_STATE_FRESH_S).
    # Then force the mtime older than "now" too, simulating a file that
    # has not changed in hours. The fallback must still fire.
    old_iso = _dt.datetime(2026, 5, 17, 7, 47, tzinfo=_dt.timezone.utc).isoformat().replace(
        "+00:00", "Z",
    )
    _write_state(fleet_home, project, "periodic-stuck", {
        "slug": "periodic-stuck", "project": project,
        "phase": "review-pending", "updated_at": old_iso,
    })
    state_path = (
        fleet_home / "projects" / project / "workers"
        / "periodic-stuck" / "state.json"
    )
    # Force mtime back in time to confirm we don't depend on a recent
    # mtime to trigger the recovery.
    old_unix = _dt.datetime(2026, 5, 17, 7, 47, tzinfo=_dt.timezone.utc).timestamp()
    import os as _os
    _os.utime(state_path, (old_unix, old_unix))

    t = _make_task(
        "periodic-stuck", status="in-progress",
        worker_pid=99999, pr_url="", branch="worker/periodic-stuck",
    )

    pr = loop._PRSummary(
        number=159, state="MERGED",
        url="https://github.com/edisonshen/fleet/pull/159",
        merged_at="2026-05-18T04:52:00Z",
        created_at="2026-05-17T05:00:00Z",
    )

    with patch.object(loop, "_pid_alive", return_value=False), \
         patch.object(loop, "_gh_pr_by_branch", return_value=pr):
        actions = loop._reconcile_inflight([t], project, "fleet", home=fleet_home)

    # The fallback fired without any mtime change to state.json — the
    # only inputs were (a) the periodic call into _reconcile_inflight,
    # and (b) the gh lookup result.
    assert len(actions) == 1
    a = actions[0]
    assert a.new_status == "done"
    assert a.set_pr_url == "https://github.com/edisonshen/fleet/pull/159"
    # And confirm we didn't touch the state.json mtime.
    assert state_path.stat().st_mtime == pytest.approx(old_unix, abs=2)


# ---------- Part B: _apply_reconcile ordering ----------


def test_apply_reconcile_writes_pr_url_before_status() -> None:
    """Test #11 (codex-driven): _apply_reconcile must write pr_url BEFORE
    status. The docstring already says this is the contract; the code
    today disagrees (status first at loop.py:2863). The fix inverts the
    order so a crash window between the two writes can't leave a task
    at status=in-review with pr_url="".

    Asserted via _run_fleet call sequence: clear_pr_url → set_pr_url →
    new_status → clear_worker → delete_worker_dir. Matches _apply_sentinel
    semantics at loop.py:3078-3084.
    """
    calls: list[list[str]] = []

    def fake_run(cmd, timeout_s=30.0):
        calls.append(list(cmd))

    action = loop._ReconcileAction(
        slug="ord-test",
        new_status="in-review",
        set_pr_url="https://github.com/x/y/pull/77",
        clear_worker=True,
    )

    with patch.object(loop, "_run_fleet", side_effect=fake_run):
        loop._apply_reconcile(action, "fleet", "fleet")

    # Find the index of each operation in the call sequence.
    set_calls = [
        i for i, c in enumerate(calls) if c[1:3] == ["tasks", "set"]
    ]
    # Identify by the trailing key=value arg.
    def _kv(call: list[str]) -> str:
        return call[-1]

    pr_url_idx = None
    status_idx = None
    worker_pid_idx = None
    for i in set_calls:
        kv = _kv(calls[i])
        if kv.startswith("pr_url="):
            pr_url_idx = i
        elif kv.startswith("status="):
            status_idx = i
        elif kv.startswith("worker_pid="):
            worker_pid_idx = i

    assert pr_url_idx is not None, f"no pr_url write in calls: {calls}"
    assert status_idx is not None, f"no status write in calls: {calls}"
    assert worker_pid_idx is not None, f"no worker_pid write in calls: {calls}"

    # The load-bearing invariant: pr_url BEFORE status.
    assert pr_url_idx < status_idx, (
        f"pr_url (#{pr_url_idx}) must be written BEFORE status (#{status_idx}); "
        f"calls={calls}"
    )
    # And status BEFORE clear_worker (per design: status flip is the
    # operator-visible transition; worker_pid=0 is post-transition cleanup).
    assert status_idx < worker_pid_idx, (
        f"status (#{status_idx}) must be written BEFORE worker_pid "
        f"(#{worker_pid_idx}); calls={calls}"
    )
