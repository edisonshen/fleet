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


# ---------- Reviewer round 5 (codex [P1]): per-attempt provenance gate ----------


def test_site1_skips_stale_branch_pr_older_than_started_at(
    fleet_home: Path, monkeypatch,
) -> None:
    """Codex reviewer round 5 [P1] regression: at SITE 1 (inside the
    mid_phase review-pending/review-done short-circuit), a branch PR
    whose createdAt is OLDER than the current worker's
    state.json.started_at must be IGNORED. It's from a prior attempt.

    Scenario: prior attempt opened PR A on worker/<slug>. CI red.
    Reconcile cleared pr_url, deleted worker dir. New worker
    re-dispatched, fresh state.json with new started_at. New worker
    reached phase=review-pending but did NOT open a new PR yet
    (review-pending is BEFORE the finisher opens the PR in the v0.2
    three-stage flow). State.json now stale.

    Without the provenance gate, SITE 1 would pick up PR A (stale)
    and flip the task to in-review/done with the wrong URL —
    skipping the reviewer/finisher for the new local commits.
    """
    monkeypatch.setenv("FLEET_HOME", str(fleet_home))
    project = "fleet"

    # New worker started AFTER the stale PR was created.
    started_at = "2026-05-18T00:00:00Z"
    stale_pr_created = "2026-05-15T00:00:00Z"

    _write_state(fleet_home, project, "retry-no-pr", {
        "slug": "retry-no-pr", "project": project,
        "phase": "review-pending",
        "started_at": started_at,
        "updated_at": "2026-05-18T00:30:00Z",
    })

    t = _make_task(
        "retry-no-pr", status="in-progress",
        worker_pid=99999, pr_url="", branch="worker/retry-no-pr",
    )

    # The stale PR is OPEN — without the provenance gate, SITE 1 would
    # treat it as the recovered PR and flip to in-review.
    stale_pr = loop._PRSummary(
        number=100, state="OPEN",
        url="https://github.com/x/y/pull/100",
        merged_at=None,
        created_at=stale_pr_created,
    )

    with patch.object(loop, "_pid_alive", return_value=False), \
         patch.object(loop, "_gh_pr_by_branch", return_value=stale_pr):
        actions = loop._reconcile_inflight([t], project, "fleet", home=fleet_home)

    # No action: provenance gate rejected the stale PR; mid_phase
    # short-circuit then `continue`s without classifying. The next
    # tick will either find a fresh PR (after the finisher runs) or
    # stay stuck waiting for operator intervention.
    assert actions == [], (
        f"stale PR must be ignored; got {actions} — provenance gate "
        f"missing or broken"
    )


def test_site1_accepts_fresh_branch_pr_newer_than_started_at(
    fleet_home: Path, monkeypatch,
) -> None:
    """Codex reviewer round 5 [P1] sibling: the provenance gate must
    NOT block legitimate recovery. A PR opened AFTER the current
    worker's started_at belongs to this attempt — the original
    rc-listener-impl-v0-12-ed95 case where the off-rails finisher
    opened a real PR.
    """
    monkeypatch.setenv("FLEET_HOME", str(fleet_home))
    project = "fleet"

    started_at = "2026-05-17T07:30:00Z"
    fresh_pr_created = "2026-05-17T07:40:00Z"  # 10 min after worker start

    _write_state(fleet_home, project, "rc-listener-like", {
        "slug": "rc-listener-like", "project": project,
        "phase": "review-pending",
        "started_at": started_at,
        "updated_at": "2026-05-17T07:47:00Z",
    })

    t = _make_task(
        "rc-listener-like", status="in-progress",
        worker_pid=99999, pr_url="", branch="worker/rc-listener-like",
    )

    fresh_pr = loop._PRSummary(
        number=159, state="MERGED",
        url="https://github.com/edisonshen/fleet/pull/159",
        merged_at="2026-05-18T04:52:00Z",
        created_at=fresh_pr_created,
    )

    with patch.object(loop, "_pid_alive", return_value=False), \
         patch.object(loop, "_gh_pr_by_branch", return_value=fresh_pr):
        actions = loop._reconcile_inflight([t], project, "fleet", home=fleet_home)

    assert len(actions) == 1
    a = actions[0]
    assert a.new_status == "done"
    assert a.set_pr_url == "https://github.com/edisonshen/fleet/pull/159"


def test_site1_accepts_pr_when_no_started_at_recorded(
    fleet_home: Path, monkeypatch,
) -> None:
    """Backwards-compat: older state.json files (pre-three-stage-flow,
    or pre-v0.2-StartedAt-population) may lack started_at. In that
    case the provenance gate is skipped (min_pr_created_at=""). The
    existing fallback semantics still apply.
    """
    monkeypatch.setenv("FLEET_HOME", str(fleet_home))
    project = "fleet"

    # No started_at field — older state.json shape.
    _write_state(fleet_home, project, "no-started-at", {
        "slug": "no-started-at", "project": project,
        "phase": "review-pending",
        "updated_at": "2026-05-17T07:47:00Z",
    })

    t = _make_task(
        "no-started-at", status="in-progress",
        worker_pid=99999, pr_url="", branch="worker/no-started-at",
    )

    pr = loop._PRSummary(
        number=160, state="MERGED",
        url="https://github.com/x/y/pull/160",
        merged_at="2026-05-18T04:00:00Z",
        created_at="2026-05-17T00:00:00Z",
    )

    with patch.object(loop, "_pid_alive", return_value=False), \
         patch.object(loop, "_gh_pr_by_branch", return_value=pr):
        actions = loop._reconcile_inflight([t], project, "fleet", home=fleet_home)

    # Gate disabled → original fallback fires.
    assert len(actions) == 1
    assert actions[0].new_status == "done"


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


def test_in_progress_no_state_json_does_not_pick_up_stale_branch_pr(
    fleet_home: Path, monkeypatch,
) -> None:
    """Codex reviewer round 3 [P1] regression (core case): re-dispatched
    in-progress task, branch reused from prior attempt (still has an
    OPEN PR on GitHub), new worker dies BEFORE writing state.json.

    SITE 2 of the fallback was removed precisely because in this state
    we have no way to tell whether the branch PR belongs to the current
    attempt or a prior one. The conservative move is to fall through to
    the legacy "worker died without PR" requeue.
    """
    monkeypatch.setenv("FLEET_HOME", str(fleet_home))
    project = "fleet"
    # No state.json — new worker died before writing any state.
    # tasks.md.pr_url is empty (cleared by previous CI-red reconcile).

    t = _make_task(
        "retry-bare", status="in-progress",
        worker_pid=99999, pr_url="", branch="worker/retry-bare",
    )

    stale_pr = loop._PRSummary(
        number=500, state="OPEN",
        url="https://github.com/x/y/pull/500",
        merged_at=None,
        created_at="2026-05-14T00:00:00Z",
    )

    def boom(*a, **kw):
        raise AssertionError(
            "SITE 2 fallback must not run — codex round 3 [P1] gate",
        )

    with patch.object(loop, "_pid_alive", return_value=False), \
         patch.object(loop, "_gh_pr_by_branch", side_effect=boom):
        actions = loop._reconcile_inflight([t], project, "fleet", home=fleet_home)

    # Fall through to legacy "worker died without PR" requeue.
    assert len(actions) == 1, f"expected 1 action, got {actions}"
    a = actions[0]
    assert a.new_status == "todo"
    assert a.delete_worker_dir is True
    assert a.set_pr_url == "", "stale branch PR must NOT leak in"
    _ = stale_pr


def test_in_review_with_empty_pr_url_does_not_trigger_stale_branch_pr(
    fleet_home: Path, monkeypatch,
) -> None:
    """Codex reviewer round 3 [P1] regression: SITE 2 of the fallback
    (post-terminal-state fall-through) was REMOVED because it would
    pick up stale branch PRs from prior attempts.

    Specifically: an in-review task whose pr_url was cleared by the
    operator (or by a CI-red reconcile that didn't immediately
    re-flip back to in-progress) cannot prove that a branch PR
    belongs to the current attempt. Without a per-attempt epoch
    timestamp, the safer choice is to fall through to the existing
    "worker died without PR" requeue — letting the operator explicitly
    own the recovery.

    The original load-bearing case (rc-listener-impl-v0-12-ed95 stuck
    at phase=review-pending) is still handled by SITE 1 inside the
    mid_phase short-circuit, where state.json proves the worker
    reached a PR-creating phase.
    """
    monkeypatch.setenv("FLEET_HOME", str(fleet_home))
    project = "fleet"
    # No state.json — worker dead, dir gone, status=in-review with empty
    # pr_url. A stale MERGED PR exists on the branch from a prior attempt.

    t = _make_task(
        "in-review-no-pr", status="in-review",
        worker_pid=0, pr_url="", branch="worker/in-review-no-pr",
    )

    stale_pr = loop._PRSummary(
        number=77, state="MERGED",
        url="https://github.com/x/y/pull/77",
        merged_at="2026-05-18T05:00:00Z",
        created_at="2026-05-18T00:00:00Z",
    )

    # If the fallback fired, it would call _gh_pr_by_branch. Patch it to
    # raise so any accidental invocation is loud (codex round 3 [P1]
    # explicitly says: don't fire SITE 2 without per-attempt provenance).
    def boom(*a, **kw):
        raise AssertionError(
            "SITE 2 fallback must not run — codex round 3 [P1] gate",
        )

    with patch.object(loop, "_pid_alive", return_value=False), \
         patch.object(loop, "_gh_pr_by_branch", side_effect=boom):
        actions = loop._reconcile_inflight([t], project, "fleet", home=fleet_home)

    # In-review with empty pr_url falls through to the existing legacy
    # `else: worker died without PR` requeue — task flips to todo with
    # delete_worker_dir=True. The stale branch PR is NOT picked up.
    assert len(actions) == 1, f"expected 1 action, got {actions}"
    a = actions[0]
    assert a.new_status == "todo", f"expected todo, got {a.new_status}"
    assert a.delete_worker_dir is True
    assert a.set_pr_url == "", "stale branch PR must NOT leak in"
    _ = stale_pr  # silence "unused"


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


# ---------- Codex iter-6 [P1] partial-apply recovery ----------


def test_partial_apply_pr_url_in_progress_falls_through_to_ci_poll(
    fleet_home: Path, monkeypatch,
) -> None:
    """Codex iter-6 [P1] regression: if a prior tick's _apply_reconcile
    crashed between the set_pr_url write and the status= write, tasks.md
    is left at status=in-progress WITH a durable pr_url, while state.json
    is still review-pending/review-done.

    Before the fix, the SITE 1 mid_phase short-circuit always
    `continue`d after calling the fallback helper. The helper short-
    circuits (returns None) when t.pr_url is already set — so the
    pr_url+CI poll branch below was never reached, and the task was
    stuck forever at in-progress with a real PR URL attached.

    The fix: when the fallback returns None AND t.pr_url is set, fall
    through. The existing `if t.pr_url: ci = _gh_pr_checks(...)` block
    now drives the partial-apply task forward to in-review/done/etc.
    """
    monkeypatch.setenv("FLEET_HOME", str(fleet_home))
    project = "fleet"

    # Stuck handoff state: state.json frozen at phase=review-pending.
    _write_state(fleet_home, project, "partial-apply", {
        "slug": "partial-apply", "project": project,
        "phase": "review-pending",
        "started_at": "2026-05-18T00:00:00Z",
        "updated_at": "2026-05-18T07:33:00Z",
    })

    # Task is at status=in-progress (status= write crashed) but pr_url is
    # already durable from the prior tick's first half of the apply.
    t = _make_task(
        "partial-apply", status="in-progress",
        worker_pid=12345,
        pr_url="https://github.com/edisonshen/fleet/pull/200",
        branch="worker/partial-apply",
    )

    # The fallback helper would short-circuit on t.pr_url and return None.
    # Patch _gh_pr_by_branch to raise: if it gets called, the test fails
    # loudly (the helper should not even reach the gh call when t.pr_url
    # is set).
    def boom(*a, **kw):
        raise AssertionError(
            "_gh_pr_by_branch must not run when t.pr_url is already set",
        )

    # _gh_pr_checks returns CI-green + merged so the task progresses to
    # done. This is the load-bearing assertion: the CI poll branch WAS
    # entered. If the bug regressed, no action would emit.
    ci_done = loop._CIResult(all_green=True, merged=True)

    with patch.object(loop, "_pid_alive", return_value=False), \
         patch.object(loop, "_gh_pr_by_branch", side_effect=boom), \
         patch.object(loop, "_gh_pr_checks", return_value=ci_done):
        actions = loop._reconcile_inflight([t], project, "fleet", home=fleet_home)

    assert len(actions) == 1, (
        f"expected 1 action (CI poll path), got {actions} — fall-through bug"
    )
    a = actions[0]
    assert a.new_status == "done", f"expected done, got {a.new_status}"
    assert a.delete_worker_dir is True


def test_partial_apply_no_pr_url_still_short_circuits(
    fleet_home: Path, monkeypatch,
) -> None:
    """Companion to test_partial_apply_pr_url_in_progress_falls_through:
    confirms the fix is correctly gated on t.pr_url. When t.pr_url is
    EMPTY (the original stuck-handoff case with no PR yet), the
    short-circuit MUST still fire — otherwise we'd fall through to the
    "worker died without PR" branch and incorrectly requeue the task
    to todo while a reviewer/finisher dispatch may still be in flight.
    """
    monkeypatch.setenv("FLEET_HOME", str(fleet_home))
    project = "fleet"

    # Stuck handoff state, but the worker never opened a PR yet (the
    # original rc-listener-impl-v0-12-ed95 shape BEFORE any PR existed).
    _write_state(fleet_home, project, "stuck-no-pr", {
        "slug": "stuck-no-pr", "project": project,
        "phase": "review-pending",
        "started_at": "2026-05-18T00:00:00Z",
        "updated_at": "2026-05-18T07:33:00Z",
    })

    t = _make_task(
        "stuck-no-pr", status="in-progress",
        worker_pid=12345, pr_url="",  # no PR yet
        branch="worker/stuck-no-pr",
    )

    # _gh_pr_by_branch returns None (no PR on the branch yet, matching
    # the in-flight handoff case).
    def no_pr(*a, **kw):
        return None

    # _gh_pr_checks must NOT run — task has no pr_url and the
    # short-circuit must keep the fall-through-to-requeue branch closed.
    def ci_boom(*a, **kw):
        raise AssertionError(
            "_gh_pr_checks must not run when t.pr_url is empty in this path",
        )

    with patch.object(loop, "_pid_alive", return_value=False), \
         patch.object(loop, "_gh_pr_by_branch", side_effect=no_pr), \
         patch.object(loop, "_gh_pr_checks", side_effect=ci_boom):
        actions = loop._reconcile_inflight([t], project, "fleet", home=fleet_home)

    # No action emitted: the mid_phase short-circuit prevented the
    # legacy "worker died" requeue. The handoff dispatch path owns
    # recovery for this case.
    assert actions == [], (
        f"expected no action (handoff dispatch owns recovery), got {actions}"
    )
