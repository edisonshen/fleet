"""Tests for durable, tick-owned PR watching (PR1 — tracking half).

Design: docs/DESIGN-coord-pr-watch-durable.md (PR1 scope). Covers the
PR1-relevant Test-plan scenarios:

  - enrollment is derived (+ recreate after watch-file deletion)
  - green is non-terminal (regression for the #195 stop-at-green bug)
  - terminal prunes + reconciles (MERGED flips ALL tasks -> gc backstop ->
    prune LAST; CLOSED -> raise-hand, retain)
  - survives restart (fresh process loads from disk)
  - fresh-ref ancestor (stale local origin/main + stale PR -> STALE not
    READY, regression for #199)
  - fail-soft probe (transient -> retain; 404 -> raise-hand)
  - orphan PR (task removed while PR OPEN -> orphaned + raise-hand)
  - dedupe by PR number (two tasks -> one watch/probe)
  - atomic write crash-safety
  - coord-scope assert (foreign-repo PR is skipped)

All deterministic: clock + gh + git are injected via fakes (no network,
no time.Sleep, no wall-clock assertions).
"""
from __future__ import annotations

import datetime as _dt
import json
from pathlib import Path
from unittest.mock import patch

import pytest

import loop
import parse
import pr_watch as pw


# ---------------------------------------------------------------------------
# helpers / fakes
# ---------------------------------------------------------------------------

OWNER_REPO = "edisonshen/fleet"


def _task(slug: str, *, status: str = "in-review", pr_url: str = "",
          branch: str = "", depends_on=None) -> parse.Task:
    return parse.Task(slug=slug, status=status, priority="P1",
                      pr_url=pr_url, branch=branch,
                      depends_on=list(depends_on or []))


def _pr_url(n: int, owner_repo: str = OWNER_REPO) -> str:
    return f"https://github.com/{owner_repo}/pull/{n}"


class FakeProber:
    """Deterministic Prober. `snaps` maps pr_number -> PRSnapshot;
    `repo_error` sets a whole-repo transient failure; `fresh_base` is the
    fetched primary-base SHA; `base_shas` maps base-ref-name -> fresh sha
    (per-PR-base check); `ancestors` is a set of (anc, desc) pairs that
    is_ancestor returns True for. Records the calls so cost-bound /
    cadence tests can assert how many probes fired."""

    def __init__(self, *, snaps=None, repo_error="", fresh_base="BASE",
                 base_shas=None, ancestors=None, fetch_error="",
                 heads_missing=None):
        self.snaps = snaps or {}
        self.repo_error = repo_error
        self.fresh_base = fresh_base
        self.base_shas = base_shas or {}
        self.ancestors = ancestors or set()
        self.fetch_error = fetch_error
        # PR numbers whose head is NOT locally present (head fetch failed).
        # Default: all heads present (the common case).
        self.heads_missing = set(heads_missing or set())
        self.probe_calls = []          # list of (pr_numbers tuple)
        self.ancestor_calls = []

    def probe_repo(self, repo_path, owner_repo, base_ref, pr_numbers, head_oids):
        self.probe_calls.append(tuple(pr_numbers))
        rp = pw.RepoProbe(
            fresh_base_sha=self.fresh_base,
            fresh_base_shas=dict(self.base_shas),
            fetch_ok=bool(self.fresh_base),
            fetch_error=self.fetch_error,
        )
        if self.repo_error:
            rp.error = self.repo_error
            return rp
        for n in pr_numbers:
            if n in self.snaps:
                rp.snapshots[n] = self.snaps[n]
                # mark head present unless the test opted it out.
                if self.snaps[n].head_ref_oid and n not in self.heads_missing:
                    rp.head_present.add(n)
        return rp

    def is_ancestor(self, repo_path, ancestor_sha, descendant_sha):
        self.ancestor_calls.append((ancestor_sha, descendant_sha))
        return (ancestor_sha, descendant_sha) in self.ancestors


def _run(tasks, project_dir, prober, *, owner_repo=OWNER_REPO, now="2026-06-03T08:00:00Z",
         tick_count=1, slow=5, flips=None):
    """Run reconcile_watches with a recording flip callback. Returns the
    WatchOutcome; appends flipped slugs to `flips` (if provided)."""
    def _flip(slug, pr_url=""):
        if flips is not None:
            flips.append(slug)
    return pw.reconcile_watches(
        tasks, project="p", project_dir=project_dir,
        coord_owner_repo=owner_repo, prober=prober,
        flip_task_done=_flip, now_iso=now, tick_count=tick_count,
        slow_cadence_ticks=slow, repo_path="/repo",
    )


# ---------------------------------------------------------------------------
# pr_url parsing
# ---------------------------------------------------------------------------


def test_parse_pr_url() -> None:
    assert pw.parse_pr_url(_pr_url(195)) == (OWNER_REPO, 195)
    assert pw.parse_pr_url("https://github.com/x/y/pull/3?foo=bar") == ("x/y", 3)
    assert pw.parse_pr_url("git@github.com:a/b/pull/9") == ("a/b", 9)
    # case-insensitive: owner/repo lowercased (codex iter-6 [P2]).
    assert pw.parse_pr_url("https://github.com/EdisonShen/Fleet/pull/42") == ("edisonshen/fleet", 42)
    assert pw.parse_pr_url("") is None
    assert pw.parse_pr_url("https://example.com/no-pr") is None
    # lookalike host must NOT parse as GitHub (codex iter-19 [P2]).
    assert pw.parse_pr_url("https://notgithub.com/edisonshen/fleet/pull/195") is None
    assert pw.parse_pr_url("https://evilgithub.com/a/b/pull/1") is None
    # github.com as a PATH segment (not the host) must NOT parse (codex
    # round 27 [P2]).
    assert pw.parse_pr_url("https://tracker.example/github.com/org/repo/pull/1") is None
    # path-embedded `@github.com` must NOT parse as the host (codex round 28).
    assert pw.parse_pr_url("https://tracker.example/foo@github.com/org/repo/pull/123") is None
    # scp-style remote-host form still parses (host at string start + userinfo).
    assert pw.parse_pr_url("github.com:edisonshen/fleet/pull/5") == ("edisonshen/fleet", 5)
    assert pw.parse_pr_url("git@github.com:edisonshen/fleet/pull/7") == ("edisonshen/fleet", 7)
    # ssh:// scheme form parses (urllib host = github.com).
    assert pw.parse_pr_url("ssh://git@github.com/edisonshen/fleet/pull/8") == ("edisonshen/fleet", 8)
    # trailing slash / query tolerated.
    assert pw.parse_pr_url("https://github.com/a/b/pull/9/") == ("a/b", 9)


def test_coord_scope_case_insensitive(tmp_path: Path) -> None:
    """A remote cased EdisonShen/Fleet matches a PR URL edisonshen/fleet —
    the owned PR is enrolled, not skipped as foreign (codex iter-6 [P2])."""
    tasks = [_task("foo", pr_url="https://github.com/edisonshen/fleet/pull/42")]
    snaps = {42: pw.PRSnapshot(number=42, pr_state="OPEN", checks="SUCCESS",
                               head_ref_oid="H")}
    # coord owns the repo under a differently-cased remote.
    out = _run(tasks, tmp_path, FakeProber(snaps=snaps), owner_repo="EdisonShen/Fleet")
    assert out.enrolled == 1
    assert "42" in pw.load_watches(tmp_path)["watches"]
    assert not any("coord-scope" in e for e in out.errors)


# ---------------------------------------------------------------------------
# enrollment is derived
# ---------------------------------------------------------------------------


def test_enrollment_is_derived(tmp_path: Path) -> None:
    """tasks.md with an owned pr_url + non-terminal status, empty
    pr-watches.json -> one tick creates the watch."""
    tasks = [_task("foo", pr_url=_pr_url(195), branch="worker/foo")]
    snaps = {195: pw.PRSnapshot(number=195, pr_state="OPEN",
                                merge_state_status="CLEAN", checks="SUCCESS",
                                head_ref_oid="HEAD195")}
    out = _run(tasks, tmp_path, FakeProber(snaps=snaps))
    assert out.enrolled == 1
    doc = pw.load_watches(tmp_path)
    assert "195" in doc["watches"]
    w = doc["watches"]["195"]
    assert w["pr_number"] == 195
    assert w["tasks"] == ["foo"]
    assert w["branch"] == "worker/foo"
    assert w["state"] == pw.STATE_OPEN
    # forward-compat fields present but unpopulated (PR2 owns them).
    assert w["inflight_action"] is None
    assert w["dispatched_events"] == {}


def test_enrollment_recreates_after_watch_file_deletion(tmp_path: Path) -> None:
    """Deleting pr-watches.json and ticking again recreates the watch
    (the watch is a DERIVED invariant, not durable-by-itself)."""
    tasks = [_task("foo", pr_url=_pr_url(195))]
    snaps = {195: pw.PRSnapshot(number=195, pr_state="OPEN", checks="SUCCESS",
                                head_ref_oid="H")}
    _run(tasks, tmp_path, FakeProber(snaps=snaps))
    assert pw.watch_path(tmp_path).exists()
    pw.watch_path(tmp_path).unlink()
    out = _run(tasks, tmp_path, FakeProber(snaps=snaps))
    assert out.enrolled == 1
    assert "195" in pw.load_watches(tmp_path)["watches"]


def test_enroll_from_pre_reconcile_when_url_cleared(tmp_path: Path) -> None:
    """The legacy reconcile cleared a red-CI task's pr_url + requeued it
    earlier this tick. On a fresh rollout (no watch file), enrolling from
    the PRE-reconcile snapshot still captures the PR durably (codex iter-7
    [P2])."""
    # current task: requeued to todo, pr_url cleared.
    current = [_task("foo", status="todo", pr_url="")]
    # pre-reconcile snapshot: still in-review with the PR url.
    pre = [_task("foo", status="in-review", pr_url=_pr_url(195), branch="worker/foo")]
    snaps = {195: pw.PRSnapshot(number=195, pr_state="OPEN", checks="FAILURE",
                                head_ref_oid="H")}

    flips = []

    def _flip2(slug, pr_url=""):
        flips.append(slug)

    out = pw.reconcile_watches(
        current, project="p", project_dir=tmp_path,
        coord_owner_repo=OWNER_REPO, prober=FakeProber(snaps=snaps),
        flip_task_done=_flip2, now_iso="t", tick_count=1,
        repo_path="/repo", enroll_tasks=pre,
    )
    assert out.enrolled == 1
    w = pw.load_watches(tmp_path)["watches"]["195"]
    assert w["pr_number"] == 195
    # the cleared task IS a genuine backing task (its url was cleared this
    # tick, but the task still exists) -> counts toward tasks[] so a later
    # merge flips it done (codex adversarial [P1]).
    assert w["tasks"] == ["foo"]
    # CI-failure still surfaced (durable watch tracks the PR).
    assert w["last_event"] == pw.EVENT_CI_FAILED


def test_pre_reconcile_task_flipped_done_on_merge(tmp_path: Path) -> None:
    """A pre-reconcile-only backing task is flipped done when its old PR
    merges (codex adversarial [P1]) — NOT left re-dispatchable."""
    current = [_task("foo", status="todo", pr_url="")]   # requeued, url cleared
    pre = [_task("foo", status="in-review", pr_url=_pr_url(195), branch="worker/foo")]
    snaps = {195: pw.PRSnapshot(number=195, pr_state="MERGED", head_ref_oid="H")}
    flips = []
    out = pw.reconcile_watches(
        current, project="p", project_dir=tmp_path,
        coord_owner_repo=OWNER_REPO, prober=FakeProber(snaps=snaps),
        flip_task_done=lambda s, u="": flips.append((s, u)), now_iso="t", tick_count=1,
        repo_path="/repo", enroll_tasks=pre,
    )
    # task flipped done WITH its pr_url restored (codex round 31 [P2]).
    assert flips == [("foo", _pr_url(195))]
    assert out.pruned == 1


def test_pre_reconcile_backing_preserved_across_ticks(tmp_path: Path) -> None:
    """The pre-reconcile snapshot is only available on the clear tick. On a
    LATER tick (no pre-reconcile data, task still requeued with no url) the
    backing slug must be PRESERVED — else a later merge orphan-prunes the
    watch and leaves the task re-dispatchable (codex round 24 [P2])."""
    # tick 1: legacy reconcile cleared url; enroll from pre-reconcile.
    current1 = [_task("foo", status="todo", pr_url="")]
    pre = [_task("foo", status="in-review", pr_url=_pr_url(195), branch="worker/foo")]
    snaps_open = {195: pw.PRSnapshot(number=195, pr_state="OPEN", checks="FAILURE",
                                     head_ref_oid="H")}
    pw.reconcile_watches(
        current1, project="p", project_dir=tmp_path, coord_owner_repo=OWNER_REPO,
        prober=FakeProber(snaps=snaps_open), flip_task_done=lambda s, u="": None,
        now_iso="t1", tick_count=1, repo_path="/repo", enroll_tasks=pre,
    )
    assert pw.load_watches(tmp_path)["watches"]["195"]["tasks"] == ["foo"]
    # tick 2: NO pre-reconcile snapshot; task still todo with no url. Backing
    # preserved because the slug still exists as a live task.
    current2 = [_task("foo", status="todo", pr_url="")]
    _run(current2, tmp_path, FakeProber(snaps=snaps_open), tick_count=2)
    assert pw.load_watches(tmp_path)["watches"]["195"]["tasks"] == ["foo"]
    # tick 3: PR merges -> the preserved backing task is flipped done.
    snaps_merged = {195: pw.PRSnapshot(number=195, pr_state="MERGED", head_ref_oid="H")}
    flips = []
    out = _run(current2, tmp_path, FakeProber(snaps=snaps_merged), tick_count=3, flips=flips)
    assert flips == ["foo"]
    assert out.pruned == 1


def test_old_watch_drops_task_that_moved_to_new_pr(tmp_path: Path) -> None:
    """A retried task that now points at a DIFFERENT pr_url must NOT remain
    backing its OLD watch — else a stale-PR merge flips the active task done
    (codex round 25 [P1]). Only url-less (pure legacy-clear) slugs preserve."""
    # old watch 195 backed by foo; foo now points at a NEW PR 200.
    doc = {"schema": 1, "watches": {"195": pw._new_watch(195, _pr_url(195), "worker/foo", "main")}}
    doc["watches"]["195"]["tasks"] = ["foo"]
    doc["watches"]["195"]["last_snapshot"] = {"pr_state": "OPEN"}
    pw.save_watches(tmp_path, doc)
    tasks = [_task("foo", pr_url=_pr_url(200), branch="worker/foo")]
    snaps = {
        195: pw.PRSnapshot(number=195, pr_state="MERGED", head_ref_oid="H"),  # stale old PR merges
        200: pw.PRSnapshot(number=200, pr_state="OPEN", checks="SUCCESS", head_ref_oid="H2"),
    }
    flips = []
    _run(tasks, tmp_path, FakeProber(snaps=snaps), tick_count=2, flips=flips)
    # foo is attached to PR 200 now; the stale 195 merge must NOT flip it.
    assert flips == []
    # 195 had empty backing -> orphan-merge prune (no task mutated); 200 lives.
    watches = pw.load_watches(tmp_path)["watches"]
    assert "195" not in watches
    assert watches["200"]["tasks"] == ["foo"]


def test_active_retry_not_flipped_by_old_pr_merge(tmp_path: Path) -> None:
    """A re-dispatched retry (in-progress, no pr_url, new worker running)
    must NOT remain backing its OLD watch — else a merge of the old PR
    flips the live retry done + clears its worker, orphaning in-flight work
    (codex round 34 [P1]). Only IDLE (todo) url-less tasks preserve."""
    doc = {"schema": 1, "watches": {"195": pw._new_watch(195, _pr_url(195), "worker/foo", "main")}}
    doc["watches"]["195"]["tasks"] = ["foo"]
    doc["watches"]["195"]["last_snapshot"] = {"pr_state": "OPEN"}
    pw.save_watches(tmp_path, doc)
    # foo was re-dispatched: in-progress, no pr_url (new worker running).
    tasks = [_task("foo", status="in-progress", pr_url="", branch="worker/foo")]
    snaps = {195: pw.PRSnapshot(number=195, pr_state="MERGED", head_ref_oid="H")}
    flips = []
    out = _run(tasks, tmp_path, FakeProber(snaps=snaps), tick_count=2, flips=flips)
    # the live in-progress retry must NOT be flipped done by the old merge.
    assert flips == []
    # 195 had empty backing -> orphan-merge prune (no task mutated).
    assert "195" not in pw.load_watches(tmp_path)["watches"]


def test_idle_todo_retry_preserved_and_flipped_on_merge(tmp_path: Path) -> None:
    """An IDLE (todo) requeued task IS preserved + flipped done when its old
    PR merges (the round-24 case still works; only active retries excluded)."""
    doc = {"schema": 1, "watches": {"195": pw._new_watch(195, _pr_url(195), "worker/foo", "main")}}
    doc["watches"]["195"]["tasks"] = ["foo"]
    doc["watches"]["195"]["last_snapshot"] = {"pr_state": "OPEN"}
    pw.save_watches(tmp_path, doc)
    tasks = [_task("foo", status="todo", pr_url="", branch="worker/foo")]
    snaps = {195: pw.PRSnapshot(number=195, pr_state="MERGED", head_ref_oid="H")}
    flips = []
    out = _run(tasks, tmp_path, FakeProber(snaps=snaps), tick_count=2, flips=flips)
    assert flips == ["foo"]
    assert out.pruned == 1


def test_backing_dropped_when_task_archived(tmp_path: Path) -> None:
    """A persisted backing slug that NO LONGER exists as a live task (it was
    archived / went terminal) is NOT preserved — it correctly drops so the
    orphan/cleanup paths handle it (codex round 24 [P2] guard)."""
    doc = {"schema": 1, "watches": {"195": pw._new_watch(195, _pr_url(195), "worker/foo", "main")}}
    doc["watches"]["195"]["tasks"] = ["foo"]
    doc["watches"]["195"]["last_snapshot"] = {"pr_state": "OPEN"}
    pw.save_watches(tmp_path, doc)
    # foo no longer exists in the current snapshot (archived).
    out = _run([], tmp_path, FakeProber(snaps={195: pw.PRSnapshot(
        number=195, pr_state="OPEN", checks="SUCCESS", head_ref_oid="H")}), tick_count=2)
    w = pw.load_watches(tmp_path)["watches"]["195"]
    assert w["tasks"] == []                           # dropped, not preserved
    assert any("orphaned-pr" in r for r in out.raises)


def test_actionable_event_raises_once_not_every_tick(tmp_path: Path) -> None:
    """An actionable event (CI_FAILED) raises ONCE on the transition, not
    every tick while it persists (codex adversarial [P1])."""
    tasks = [_task("foo", pr_url=_pr_url(195))]
    snaps = {195: pw.PRSnapshot(number=195, pr_state="OPEN", checks="FAILURE",
                                head_ref_oid="H")}
    # tick 1: transition into CI_FAILED -> raise.
    out1 = _run(tasks, tmp_path, FakeProber(snaps=snaps), tick_count=1)
    assert any("CI-FAILED" in r for r in out1.raises)
    # tick 2: still CI_FAILED (no change) -> NOT re-raised.
    out2 = _run(tasks, tmp_path, FakeProber(snaps=snaps), tick_count=2)
    assert not any("CI-FAILED" in r for r in out2.raises)
    # event flips green->fail again later -> re-raises on the new transition.
    green = {195: pw.PRSnapshot(number=195, pr_state="OPEN", checks="SUCCESS",
                                head_ref_oid="H")}
    _run(tasks, tmp_path, FakeProber(snaps=green), tick_count=3)
    out4 = _run(tasks, tmp_path, FakeProber(snaps=snaps), tick_count=4)
    assert any("CI-FAILED" in r for r in out4.raises)


def test_terminal_task_not_enrolled(tmp_path: Path) -> None:
    """A done/abandoned task with a pr_url does NOT back a watch."""
    tasks = [_task("foo", status="done", pr_url=_pr_url(195))]
    out = _run(tasks, tmp_path, FakeProber())
    assert out.enrolled == 0
    assert pw.load_watches(tmp_path)["watches"] == {}


# ---------------------------------------------------------------------------
# green is non-terminal (regression for the 2026-06-03 #195 bug)
# ---------------------------------------------------------------------------


def test_green_is_non_terminal(tmp_path: Path) -> None:
    """A probe returning OPEN + all checks PASSED + BLOCKED/CLEAN ->
    watch RETAINED, no terminal action, last_seen_at advances."""
    tasks = [_task("foo", pr_url=_pr_url(195))]
    # green + up-to-date but NOT next-to-merge eligibility logic — here
    # head contains base + green + no review-required -> READY (still OPEN).
    snaps = {195: pw.PRSnapshot(number=195, pr_state="OPEN",
                                merge_state_status="CLEAN", checks="SUCCESS",
                                review_decision="APPROVED",
                                head_ref_oid="HEAD195")}
    prober = FakeProber(snaps=snaps, fresh_base="BASE",
                        ancestors={("BASE", "HEAD195")})
    out = _run(tasks, tmp_path, prober)
    w = pw.load_watches(tmp_path)["watches"]["195"]
    assert w["state"] == pw.STATE_OPEN        # NOT pruned, NOT terminal
    assert w["last_seen_at"] == "2026-06-03T08:00:00Z"
    assert w["last_event"] == pw.EVENT_READY
    assert out.pruned == 0
    assert out.tasks_flipped == 0


# ---------------------------------------------------------------------------
# terminal: MERGED prunes + reconciles; CLOSED raises hand + retains
# ---------------------------------------------------------------------------


def test_merged_flips_all_tasks_then_prunes(tmp_path: Path) -> None:
    """MERGED -> flip ALL backing tasks done, then prune the watch."""
    tasks = [_task("foo", pr_url=_pr_url(195)), _task("bar", pr_url=_pr_url(195))]
    snaps = {195: pw.PRSnapshot(number=195, pr_state="MERGED",
                                merged_at="2026-06-03T08:00:00Z", head_ref_oid="H")}
    flips = []
    out = _run(tasks, tmp_path, FakeProber(snaps=snaps), flips=flips)
    assert sorted(flips) == ["bar", "foo"]
    assert out.tasks_flipped == 2
    assert out.pruned == 1
    assert "195" not in pw.load_watches(tmp_path)["watches"]


def test_merged_flip_failure_leaves_watch_unpruned(tmp_path: Path) -> None:
    """A flip failure leaves the MERGED watch un-pruned so the next tick
    re-reconciles (flip is idempotent)."""
    tasks = [_task("foo", pr_url=_pr_url(195))]
    snaps = {195: pw.PRSnapshot(number=195, pr_state="MERGED", head_ref_oid="H")}

    def _boom(slug, pr_url=""):
        raise RuntimeError("fleet tasks set failed")

    out = pw.reconcile_watches(
        tasks, project="p", project_dir=tmp_path,
        coord_owner_repo=OWNER_REPO, prober=FakeProber(snaps=snaps),
        flip_task_done=_boom, now_iso="t", tick_count=1, repo_path="/repo",
    )
    assert out.pruned == 0
    assert any("flip foo done" in e for e in out.errors)
    w = pw.load_watches(tmp_path)["watches"]["195"]
    assert w["state"] == pw.STATE_MERGED       # retained for re-reconcile


def test_orphan_merge_records_note_no_flip(tmp_path: Path) -> None:
    """A pre-existing watch whose PR merges with empty tasks[] -> note,
    no task mutated, watch pruned."""
    # seed a watch with empty tasks (orphaned).
    doc = {"schema": 1, "watches": {"195": pw._new_watch(195, _pr_url(195), "b", "main")}}
    doc["watches"]["195"]["orphaned"] = True
    pw.save_watches(tmp_path, doc)
    snaps = {195: pw.PRSnapshot(number=195, pr_state="MERGED", head_ref_oid="H")}
    flips = []
    out = _run([], tmp_path, FakeProber(snaps=snaps), flips=flips)
    assert flips == []
    assert out.pruned == 1
    assert any("orphan PR 195 merged" in n for n in out.notes)


def test_closed_unmerged_raises_hand_and_retains(tmp_path: Path) -> None:
    """CLOSED (no merge) -> raise-hand, watch retained as closed-unmerged."""
    tasks = [_task("foo", pr_url=_pr_url(195))]
    snaps = {195: pw.PRSnapshot(number=195, pr_state="CLOSED", head_ref_oid="H")}
    out = _run(tasks, tmp_path, FakeProber(snaps=snaps))
    assert any("CLOSED without merging" in r for r in out.raises)
    w = pw.load_watches(tmp_path)["watches"]["195"]
    assert w["state"] == pw.STATE_CLOSED_UNMERGED   # retained
    assert out.pruned == 0
    # second tick: closed watch with a LIVE task IS re-probed (operator
    # may reopen) but not re-raised while still closed.
    out2 = _run(tasks, tmp_path, FakeProber(snaps=snaps), tick_count=2)
    assert not any("CLOSED without merging" in r for r in out2.raises)


def test_closed_watch_no_task_not_reprobed(tmp_path: Path) -> None:
    """A closed-unmerged watch with NO live backing task is NOT re-probed
    (parked until operator acks) (codex iter-8 [P2])."""
    doc = {"schema": 1, "watches": {"195": pw._new_watch(195, _pr_url(195), "b", "main")}}
    doc["watches"]["195"]["state"] = pw.STATE_CLOSED_UNMERGED
    pw.save_watches(tmp_path, doc)
    prober = FakeProber(snaps={195: pw.PRSnapshot(number=195, pr_state="OPEN")})
    _run([], tmp_path, prober, tick_count=3)
    assert prober.probe_calls == []                  # not re-probed


def test_reopened_closed_pr_transitions_back_to_open(tmp_path: Path) -> None:
    """A closed-unmerged watch with a LIVE task whose PR is REOPENED ->
    re-probed, transitions back to OPEN, and a later merge reconciles
    (codex iter-8 [P2])."""
    doc = {"schema": 1, "watches": {"195": pw._new_watch(195, _pr_url(195), "worker/foo", "main")}}
    doc["watches"]["195"]["state"] = pw.STATE_CLOSED_UNMERGED
    doc["watches"]["195"]["tasks"] = ["foo"]
    pw.save_watches(tmp_path, doc)
    tasks = [_task("foo", pr_url=_pr_url(195))]
    # tick: PR reopened (OPEN) -> watch back to OPEN, re-probed.
    snaps = {195: pw.PRSnapshot(number=195, pr_state="OPEN", checks="SUCCESS",
                                head_ref_oid="H")}
    prober = FakeProber(snaps=snaps)
    _run(tasks, tmp_path, prober, tick_count=2)
    assert prober.probe_calls == [(195,)]
    w = pw.load_watches(tmp_path)["watches"]["195"]
    assert w["state"] == pw.STATE_OPEN
    # next tick: it merges -> task flipped done.
    snaps2 = {195: pw.PRSnapshot(number=195, pr_state="MERGED", head_ref_oid="H")}
    flips = []
    out = _run(tasks, tmp_path, FakeProber(snaps=snaps2), tick_count=3, flips=flips)
    assert flips == ["foo"]
    assert out.pruned == 1


# ---------------------------------------------------------------------------
# survives restart (fresh process loads from disk)
# ---------------------------------------------------------------------------


def test_survives_restart(tmp_path: Path) -> None:
    """A watch written to disk loads + probes normally on a fresh
    reconcile pass with no in-memory carryover."""
    tasks = [_task("foo", pr_url=_pr_url(195))]
    snaps = {195: pw.PRSnapshot(number=195, pr_state="OPEN", checks="SUCCESS",
                                head_ref_oid="H1")}
    _run(tasks, tmp_path, FakeProber(snaps=snaps), tick_count=1)
    # simulate restart: brand-new prober, brand-new call. The only state
    # is on disk.
    snaps2 = {195: pw.PRSnapshot(number=195, pr_state="MERGED", head_ref_oid="H1")}
    flips = []
    out = _run(tasks, tmp_path, FakeProber(snaps=snaps2), tick_count=2, flips=flips)
    assert flips == ["foo"]
    assert out.pruned == 1


# ---------------------------------------------------------------------------
# fresh-ref ancestor: stale local base + stale PR -> STALE not READY (#199)
# ---------------------------------------------------------------------------


def test_stale_pr_is_stale_not_ready(tmp_path: Path) -> None:
    """next-to-merge PR with mergeStateStatus=BLOCKED, checks SUCCESS, 0
    required reviews, head NOT containing fresh base -> STALE (not READY).
    Regression for the #199 mis-read of the status word."""
    tasks = [_task("foo", pr_url=_pr_url(199))]
    snaps = {199: pw.PRSnapshot(number=199, pr_state="OPEN",
                                merge_state_status="BLOCKED", checks="SUCCESS",
                                review_decision="APPROVED", head_ref_oid="HEAD199")}
    # ancestors EMPTY -> fresh base is NOT an ancestor of head -> stale.
    prober = FakeProber(snaps=snaps, fresh_base="FRESHBASE", ancestors=set())
    out = _run(tasks, tmp_path, prober)
    w = pw.load_watches(tmp_path)["watches"]["199"]
    assert w["last_event"] == pw.EVENT_STALE
    assert w["last_snapshot"]["up_to_date"] is False
    assert w["state"] == pw.STATE_OPEN              # still watched
    assert any("STALE" in r for r in out.raises)


def test_uptodate_green_is_ready(tmp_path: Path) -> None:
    """head DOES contain fresh base + checks green + no required review
    pending + no blocking merge-state word -> READY surfaced."""
    tasks = [_task("foo", pr_url=_pr_url(200))]
    snaps = {200: pw.PRSnapshot(number=200, pr_state="OPEN",
                                merge_state_status="CLEAN", checks="SUCCESS",
                                review_decision="APPROVED", head_ref_oid="HEAD200")}
    prober = FakeProber(snaps=snaps, fresh_base="FRESHBASE",
                        ancestors={("FRESHBASE", "HEAD200")})
    out = _run(tasks, tmp_path, prober)
    w = pw.load_watches(tmp_path)["watches"]["200"]
    assert w["last_event"] == pw.EVENT_READY
    assert w["last_snapshot"]["up_to_date"] is True
    assert any("mergeable (READY)" in n for n in out.notes)


def test_pr_measured_against_its_own_base(tmp_path: Path) -> None:
    """A stacked PR with a non-main base is measured against ITS base,
    not origin/main (codex iter-3 [P2]). Up-to-date vs its parent base +
    green -> READY; same head would be STALE vs main."""
    tasks = [_task("foo", pr_url=_pr_url(300))]
    # PR 300 targets base 'worker/parent'; head contains parent's tip but
    # NOT main's tip. Measured against its own base -> READY.
    snaps = {300: pw.PRSnapshot(number=300, pr_state="OPEN",
                                merge_state_status="CLEAN", checks="SUCCESS",
                                review_decision="APPROVED",
                                head_ref_oid="H300", base_ref_name="worker/parent")}
    prober = FakeProber(
        snaps=snaps, fresh_base="MAINSHA",
        base_shas={"main": "MAINSHA", "worker/parent": "PARENTSHA"},
        ancestors={("PARENTSHA", "H300")},   # contains parent, NOT main
    )
    out = _run(tasks, tmp_path, prober)
    w = pw.load_watches(tmp_path)["watches"]["300"]
    assert w["last_event"] == pw.EVENT_READY
    assert w["last_snapshot"]["up_to_date"] is True
    assert any("mergeable (READY)" in n for n in out.notes)


def test_fetch_fail_never_asserts_ready(tmp_path: Path) -> None:
    """A missing fresh_base with NO fetch_error (e.g. brand-new watch, base
    not yet known) -> mergeability UNKNOWN -> keep watching (EVENT_OPEN),
    never assert READY (fail-soft, §3)."""
    tasks = [_task("foo", pr_url=_pr_url(201))]
    snaps = {201: pw.PRSnapshot(number=201, pr_state="OPEN", checks="SUCCESS",
                                review_decision="APPROVED", head_ref_oid="H")}
    prober = FakeProber(snaps=snaps, fresh_base="")  # no base, no error
    _run(tasks, tmp_path, prober)
    w = pw.load_watches(tmp_path)["watches"]["201"]
    assert w["last_event"] == pw.EVENT_OPEN          # NOT ready
    assert w["last_snapshot"]["up_to_date"] is False


def test_fetch_failure_backs_off_not_silent_open(tmp_path: Path) -> None:
    """A real git fetch failure (fetch_error set, no fresh base) for a PR
    whose classification needs the ancestor check -> transient SKIP:
    RETAIN + back off + surface, NOT a silent plain-OPEN (codex iter-4
    [P2])."""
    tasks = [_task("foo", pr_url=_pr_url(202))]
    snaps = {202: pw.PRSnapshot(number=202, pr_state="OPEN", checks="SUCCESS",
                                review_decision="APPROVED", head_ref_oid="H")}
    prober = FakeProber(snaps=snaps, fresh_base="", fetch_error="fatal: unable to access")
    out = _run(tasks, tmp_path, prober, tick_count=1)
    w = pw.load_watches(tmp_path)["watches"]["202"]
    assert w.get("last_event") is None               # NOT recorded as OPEN
    assert w.get("probe_skip_until_tick", 0) > 1      # backoff armed
    assert any("transient git fetch" in e for e in out.errors)


def test_fetch_failure_still_records_merge(tmp_path: Path) -> None:
    """A git fetch failure must NOT hide a terminal MERGED event — that
    classification doesn't need the base check, so it's recorded."""
    tasks = [_task("foo", pr_url=_pr_url(203))]
    snaps = {203: pw.PRSnapshot(number=203, pr_state="MERGED", head_ref_oid="H")}
    prober = FakeProber(snaps=snaps, fresh_base="", fetch_error="network down")
    flips = []
    out = _run(tasks, tmp_path, prober, flips=flips)
    assert flips == ["foo"]                           # merge still reconciled
    assert out.pruned == 1


def test_missing_head_is_transient_not_false_stale(tmp_path: Path) -> None:
    """A PR whose head object isn't locally present (head fetch failed /
    fork ref unavailable) must NOT be false-STALE — merge-base against a
    missing head returns False. Treat as transient SKIP + backoff (codex
    iter-12 [P2])."""
    tasks = [_task("foo", pr_url=_pr_url(206))]
    snaps = {206: pw.PRSnapshot(number=206, pr_state="OPEN", checks="SUCCESS",
                                review_decision="APPROVED", head_ref_oid="FORKHEAD")}
    # head NOT present; base IS fetched -> would otherwise compute STALE.
    prober = FakeProber(snaps=snaps, fresh_base="B", ancestors=set(),
                        heads_missing={206})
    out = _run(tasks, tmp_path, prober, tick_count=1)
    w = pw.load_watches(tmp_path)["watches"]["206"]
    assert w.get("last_event") is None               # NOT recorded as STALE
    assert w.get("probe_skip_until_tick", 0) > 1      # backoff armed
    assert any("PR head" in e for e in out.errors)


def test_draft_pr_not_ready(tmp_path: Path) -> None:
    """A DRAFT PR (up-to-date + green) is NOT surfaced READY (codex iter-4
    [P2]) — GitHub won't merge a draft."""
    tasks = [_task("foo", pr_url=_pr_url(204))]
    snaps = {204: pw.PRSnapshot(number=204, pr_state="OPEN", checks="SUCCESS",
                                review_decision="APPROVED", is_draft=True,
                                head_ref_oid="H204")}
    prober = FakeProber(snaps=snaps, fresh_base="B", ancestors={("B", "H204")})
    out = _run(tasks, tmp_path, prober)
    w = pw.load_watches(tmp_path)["watches"]["204"]
    assert w["last_event"] == pw.EVENT_OPEN           # NOT ready
    assert not any("READY" in n for n in out.notes)


def test_blocked_uptodate_green_not_ready(tmp_path: Path) -> None:
    """mergeStateStatus=BLOCKED + up-to-date + green -> withhold READY
    (unresolved conversations / required deployments) (codex iter-4 [P2])."""
    tasks = [_task("foo", pr_url=_pr_url(205))]
    snaps = {205: pw.PRSnapshot(number=205, pr_state="OPEN",
                                merge_state_status="BLOCKED", checks="SUCCESS",
                                review_decision="APPROVED", head_ref_oid="H205")}
    prober = FakeProber(snaps=snaps, fresh_base="B", ancestors={("B", "H205")})
    out = _run(tasks, tmp_path, prober)
    w = pw.load_watches(tmp_path)["watches"]["205"]
    assert w["last_event"] == pw.EVENT_OPEN
    assert not any("READY" in n for n in out.notes)


# ---------------------------------------------------------------------------
# actionable events surface (PR1) — CI-fail, behind, dirty, changes-requested
# ---------------------------------------------------------------------------


@pytest.mark.parametrize("snap_kwargs,expect_event", [
    (dict(checks="FAILURE"), pw.EVENT_CI_FAILED),
    (dict(merge_state_status="DIRTY", checks="SUCCESS"), pw.EVENT_DIRTY),
    (dict(merge_state_status="BEHIND", checks="SUCCESS"), pw.EVENT_BEHIND),
    (dict(review_decision="CHANGES_REQUESTED", checks="SUCCESS"),
     pw.EVENT_CHANGES_REQUESTED),
])
def test_actionable_events_surface(tmp_path: Path, snap_kwargs, expect_event) -> None:
    tasks = [_task("foo", pr_url=_pr_url(210))]
    snaps = {210: pw.PRSnapshot(number=210, pr_state="OPEN", head_ref_oid="H",
                                **snap_kwargs)}
    out = _run(tasks, tmp_path, FakeProber(snaps=snaps))
    w = pw.load_watches(tmp_path)["watches"]["210"]
    assert w["last_event"] == expect_event
    assert w["state"] == pw.STATE_OPEN               # actionable is still OPEN
    assert any(expect_event.upper() in r for r in out.raises)


# ---------------------------------------------------------------------------
# fail-soft probe: transient -> retain; 404 -> raise-hand
# ---------------------------------------------------------------------------


def test_transient_probe_retains_watch(tmp_path: Path) -> None:
    """A transient gh 5xx/timeout/rate-limit -> watch RETAINED, not
    pruned, not flipped to merged/closed; backoff armed."""
    tasks = [_task("foo", pr_url=_pr_url(195))]
    prober = FakeProber(repo_error="HTTP 502 Bad Gateway")
    out = _run(tasks, tmp_path, prober, tick_count=1)
    w = pw.load_watches(tmp_path)["watches"]["195"]
    assert w["state"] == pw.STATE_OPEN
    assert w["last_probe_at"] == "2026-06-03T08:00:00Z"
    assert w.get("probe_skip_until_tick", 0) > 1      # backoff armed
    assert any("transient" in e for e in out.errors)
    assert out.pruned == 0


def test_definitive_404_parks_and_raises_once(tmp_path: Path) -> None:
    """A definitive 404 (PR gone) -> raise-hand pr-not-found ONCE, park as
    NOT_FOUND, retain, and do NOT re-query/re-raise every tick (codex
    iter-14 [P2])."""
    tasks = [_task("foo", pr_url=_pr_url(195))]
    snaps = {195: pw.PRSnapshot(number=195, error="HTTP 404 Not Found",
                                not_found=True)}
    # tick 1: raises + parks.
    out1 = _run(tasks, tmp_path, FakeProber(snaps=snaps), tick_count=1)
    assert any("pr-not-found" in r for r in out1.raises)
    w = pw.load_watches(tmp_path)["watches"]["195"]
    assert w["state"] == pw.STATE_NOT_FOUND
    # tick 2: parked -> NOT re-probed, NOT re-raised.
    prober2 = FakeProber(snaps=snaps)
    out2 = _run(tasks, tmp_path, prober2, tick_count=2)
    assert prober2.probe_calls == []
    assert not any("pr-not-found" in r for r in out2.raises)
    assert "195" in pw.load_watches(tmp_path)["watches"]


def test_not_found_watch_pruned_when_task_resolved(tmp_path: Path) -> None:
    """A parked NOT_FOUND watch is pruned once its backing task is resolved
    (no live task) so a handled 404 doesn't keep the file alive (codex
    iter-20 [P2])."""
    doc = {"schema": 1, "watches": {"195": pw._new_watch(195, _pr_url(195), "b", "main")}}
    doc["watches"]["195"]["state"] = pw.STATE_NOT_FOUND
    doc["watches"]["195"]["tasks"] = ["foo"]
    pw.save_watches(tmp_path, doc)
    # task resolved -> no live task; watch pruned + file removed.
    out = _run([], tmp_path, FakeProber(), tick_count=3)
    assert out.pruned == 1
    assert not pw.watch_path(tmp_path).exists()


def test_not_found_watch_retained_while_task_live(tmp_path: Path) -> None:
    """A parked NOT_FOUND watch is RETAINED while a task still points at
    it (operator hasn't resolved) (codex iter-20 [P2])."""
    doc = {"schema": 1, "watches": {"195": pw._new_watch(195, _pr_url(195), "b", "main")}}
    doc["watches"]["195"]["state"] = pw.STATE_NOT_FOUND
    pw.save_watches(tmp_path, doc)
    tasks = [_task("foo", pr_url=_pr_url(195))]
    _run(tasks, tmp_path, FakeProber(), tick_count=3)
    assert "195" in pw.load_watches(tmp_path)["watches"]


def test_missing_from_repo_query_raises_not_found(tmp_path: Path) -> None:
    """Repo query SUCCEEDED but the PR is absent -> 404-class raise-hand."""
    tasks = [_task("foo", pr_url=_pr_url(195))]
    out = _run(tasks, tmp_path, FakeProber(snaps={}))   # repo ok, no PR
    assert any("pr-not-found" in r for r in out.raises)


def test_ghgit_prober_nonzero_exit_is_transient(monkeypatch) -> None:
    """A nonzero `gh` exit (even with a 404-looking stderr) is TRANSIENT
    for every PR — the whole batch failed, we can't prove an individual
    PR is gone, and parking would never recover after auth (codex iter-16
    [P2])."""
    monkeypatch.setattr(
        pw.subprocess, "run",
        lambda cmd, **k: _FakeProc(1, "", "Could not resolve to a Repository (404)")
        if cmd[:3] == ["gh", "api", "graphql"] else _FakeProc(0),
    )
    rp = pw.GhGitProber().probe_repo("/repo", OWNER_REPO, "main", [9, 10], [])
    for n in (9, 10):
        assert rp.snapshots[n].error
        assert rp.snapshots[n].not_found is False     # NOT parked


# ---------------------------------------------------------------------------
# orphan PR
# ---------------------------------------------------------------------------


def test_orphan_pr_raises_hand(tmp_path: Path) -> None:
    """Task removed while PR still OPEN -> watch marked orphaned + raise-hand,
    no auto-action."""
    # tick 1: enroll + probe OPEN (confirmed-OPEN snapshot required for orphan).
    tasks = [_task("foo", pr_url=_pr_url(195))]
    snaps = {195: pw.PRSnapshot(number=195, pr_state="OPEN", checks="SUCCESS",
                                head_ref_oid="H")}
    _run(tasks, tmp_path, FakeProber(snaps=snaps), tick_count=1)
    # tick 2: task gone, PR still OPEN.
    out = _run([], tmp_path, FakeProber(snaps=snaps), tick_count=2)
    w = pw.load_watches(tmp_path)["watches"]["195"]
    assert w["orphaned"] is True
    assert w["tasks"] == []
    assert any("orphaned-pr" in r for r in out.raises)


def test_no_orphan_without_confirmed_open_snapshot(tmp_path: Path) -> None:
    """A watch enrolled but never confirmed-OPEN this tick must NOT
    orphan-raise — we can't assert the PR is OPEN. Regression for the E2E
    false-orphan. (Here the probe finds the PR absent -> parked/cleaned as
    not-found, NOT orphaned.)"""
    # seed a watch with NO snapshot (never probed), empty tasks.
    doc = {"schema": 1, "watches": {"777": pw._new_watch(777, _pr_url(777), "b", "main")}}
    pw.save_watches(tmp_path, doc)
    out = _run([], tmp_path, FakeProber(), tick_count=2)
    # no FALSE orphan alert (the key invariant).
    assert not any("orphaned-pr" in r for r in out.raises)


def test_foreign_repo_same_pr_number_not_backing(tmp_path: Path) -> None:
    """An in-scope PR and a foreign-repo task sharing the same PR number:
    the foreign slug must NOT become backing for the in-scope watch (codex
    iter-1 [P2]). On merge only the in-scope task flips done."""
    tasks = [
        _task("mine", pr_url=_pr_url(42, owner_repo=OWNER_REPO)),
        _task("foreign", pr_url=_pr_url(42, owner_repo="someone/other")),
    ]
    snaps = {42: pw.PRSnapshot(number=42, pr_state="MERGED", head_ref_oid="H")}
    flips = []
    out = _run(tasks, tmp_path, FakeProber(snaps=snaps), owner_repo=OWNER_REPO, flips=flips)
    # only the in-scope task flipped — the foreign slug never backed the watch.
    assert flips == ["mine"]
    assert out.tasks_flipped == 1


def test_no_orphan_on_transient_probe_failure(tmp_path: Path) -> None:
    """A transient probe failure on an empty-tasks OPEN watch must NOT
    false-orphan — the PR wasn't observed this tick (it may have
    merged/closed during the outage) (codex iter-18 [P2])."""
    # seed an OPEN watch (prev snapshot OPEN) with empty tasks.
    doc = {"schema": 1, "watches": {"195": pw._new_watch(195, _pr_url(195), "b", "main")}}
    doc["watches"]["195"]["last_snapshot"] = {"pr_state": "OPEN"}
    pw.save_watches(tmp_path, doc)
    # transient repo failure this tick.
    out = _run([], tmp_path, FakeProber(repo_error="HTTP 502"), tick_count=2)
    w = pw.load_watches(tmp_path)["watches"]["195"]
    assert w.get("orphaned") in (False, None)        # NOT orphaned
    assert not any("orphaned-pr" in r for r in out.raises)
    assert any("transient" in e for e in out.errors)


def test_orphan_cleared_when_task_reacquired(tmp_path: Path) -> None:
    """A task re-appearing for an orphaned watch clears the orphan flag."""
    doc = {"schema": 1, "watches": {"195": pw._new_watch(195, _pr_url(195), "b", "main")}}
    doc["watches"]["195"]["orphaned"] = True
    pw.save_watches(tmp_path, doc)
    tasks = [_task("foo", pr_url=_pr_url(195))]
    snaps = {195: pw.PRSnapshot(number=195, pr_state="OPEN", checks="SUCCESS",
                                head_ref_oid="H")}
    _run(tasks, tmp_path, FakeProber(snaps=snaps))
    w = pw.load_watches(tmp_path)["watches"]["195"]
    assert w["orphaned"] is False
    assert w["tasks"] == ["foo"]


# ---------------------------------------------------------------------------
# dedupe by PR number
# ---------------------------------------------------------------------------


def test_dedupe_by_pr_number(tmp_path: Path) -> None:
    """Two task slugs with the same pr_url -> one watch, one probe."""
    tasks = [_task("foo", pr_url=_pr_url(195)), _task("bar", pr_url=_pr_url(195))]
    snaps = {195: pw.PRSnapshot(number=195, pr_state="OPEN", checks="SUCCESS",
                                head_ref_oid="H")}
    prober = FakeProber(snaps=snaps)
    out = _run(tasks, tmp_path, prober)
    assert out.enrolled == 1
    assert len(pw.load_watches(tmp_path)["watches"]) == 1
    assert pw.load_watches(tmp_path)["watches"]["195"]["tasks"] == ["bar", "foo"]
    # exactly ONE probe call covering PR 195 once.
    assert prober.probe_calls == [(195,)]


# ---------------------------------------------------------------------------
# coord-scope assert
# ---------------------------------------------------------------------------


def test_coord_scope_skips_foreign_repo(tmp_path: Path) -> None:
    """A task whose PR is in a DIFFERENT repo than the coord's own is
    skipped (never enrolled / probed) + surfaced (coord-scope strict)."""
    tasks = [_task("foo", pr_url=_pr_url(195, owner_repo="someone/other"))]
    out = _run(tasks, tmp_path, FakeProber(), owner_repo=OWNER_REPO)
    assert out.enrolled == 0
    assert pw.load_watches(tmp_path)["watches"] == {}
    assert any("coord-scope" in e for e in out.errors)


def test_unknown_owner_repo_refuses_probe(tmp_path: Path) -> None:
    """coord_owner_repo=None (can't prove the repo is ours) -> enroll +
    persist but REFUSE to probe (strict scope)."""
    tasks = [_task("foo", pr_url=_pr_url(195))]
    prober = FakeProber()
    out = pw.reconcile_watches(
        tasks, project="p", project_dir=tmp_path,
        coord_owner_repo=None, prober=prober,
        flip_task_done=lambda s, u="": None, now_iso="t", tick_count=1, repo_path="/repo",
    )
    assert out.enrolled == 1
    assert prober.probe_calls == []                  # never probed
    assert "195" in pw.load_watches(tmp_path)["watches"]
    # SURFACE the reason — don't silently disable tracking (codex
    # adversarial [P2] / feedback_surface_dont_silo).
    assert any("cannot derive" in e for e in out.errors)


# ---------------------------------------------------------------------------
# cost bound + adaptive cadence
# ---------------------------------------------------------------------------


def test_ready_pr_uses_slow_cadence(tmp_path: Path) -> None:
    """A quiescent READY watch probes on the slower cadence; an actionable
    / plain-OPEN watch probes every tick."""
    tasks = [_task("foo", pr_url=_pr_url(200))]
    snaps = {200: pw.PRSnapshot(number=200, pr_state="OPEN", checks="SUCCESS",
                                review_decision="APPROVED", head_ref_oid="H")}
    prober = FakeProber(snaps=snaps, fresh_base="B", ancestors={("B", "H")})
    # tick 1: probes, classifies READY.
    _run(tasks, tmp_path, prober, tick_count=1, slow=5)
    assert prober.probe_calls == [(200,)]
    # tick 2 (not a cadence boundary for slow=5) -> NOT probed.
    prober.probe_calls.clear()
    _run(tasks, tmp_path, prober, tick_count=2, slow=5)
    assert prober.probe_calls == []
    # tick 5 (cadence boundary) -> probed again.
    _run(tasks, tmp_path, prober, tick_count=5, slow=5)
    assert prober.probe_calls == [(200,)]


def test_merged_watch_not_probed(tmp_path: Path) -> None:
    """A MERGED watch (truly terminal) is never re-probed."""
    doc = {"schema": 1, "watches": {"195": pw._new_watch(195, _pr_url(195), "b", "main")}}
    doc["watches"]["195"]["state"] = pw.STATE_MERGED
    doc["watches"]["195"]["tasks"] = []   # already reconciled
    pw.save_watches(tmp_path, doc)
    prober = FakeProber()
    _run([], tmp_path, prober, tick_count=3)
    assert prober.probe_calls == []


# ---------------------------------------------------------------------------
# atomic write crash-safety
# ---------------------------------------------------------------------------


def test_atomic_write_leaves_prior_intact_on_crash(tmp_path: Path, monkeypatch) -> None:
    """A crash between .tmp write and rename leaves the prior
    pr-watches.json intact + parseable (§1)."""
    doc1 = {"schema": 1, "watches": {"1": pw._new_watch(1, _pr_url(1), "b", "main")}}
    pw.save_watches(tmp_path, doc1)
    original = pw.watch_path(tmp_path).read_text()

    real_replace = pw.os.replace

    def _boom(src, dst):
        raise OSError("simulated crash before rename")

    monkeypatch.setattr(pw.os, "replace", _boom)
    doc2 = {"schema": 1, "watches": {"2": pw._new_watch(2, _pr_url(2), "b", "main")}}
    with pytest.raises(OSError):
        pw.save_watches(tmp_path, doc2)
    monkeypatch.setattr(pw.os, "replace", real_replace)
    # prior file intact + parseable; no .tmp leftover.
    assert pw.watch_path(tmp_path).read_text() == original
    assert json.loads(original)["watches"] == {"1": doc1["watches"]["1"]}
    leftover = list(tmp_path.glob(pw.WATCH_FILE + ".tmp.*"))
    assert leftover == []


def test_empty_watch_set_removes_file(tmp_path: Path) -> None:
    """After the last watch is pruned (merged), the file is REMOVED so the
    tick's no-PR idle early-out stays byte-identical (codex iter-19 [P2])."""
    tasks = [_task("foo", pr_url=_pr_url(195))]
    snaps = {195: pw.PRSnapshot(number=195, pr_state="MERGED", head_ref_oid="H")}
    _run(tasks, tmp_path, FakeProber(snaps=snaps), flips=[])
    # the only watch merged + pruned -> file removed, not left empty.
    assert not pw.watch_path(tmp_path).exists()


def test_persist_watches_removes_empty(tmp_path: Path) -> None:
    pw.save_watches(tmp_path, {"schema": 1, "watches": {"1": pw._new_watch(1, _pr_url(1), "b", "main")}})
    assert pw.watch_path(tmp_path).exists()
    pw.persist_watches(tmp_path, {"schema": 1, "watches": {}})
    assert not pw.watch_path(tmp_path).exists()


def test_load_drops_malformed_watch_entries(tmp_path: Path) -> None:
    """A valid-JSON file with a non-object watch entry ({"195": null}) has
    that entry dropped on load so the reconcile loop can't error every tick
    (codex round 31 [P3])."""
    pw.watch_path(tmp_path).write_text(json.dumps({"schema": 1, "watches": {
        "195": None,
        "196": "not-an-object",
        "197": pw._new_watch(197, _pr_url(197), "b", "main"),
    }}))
    doc = pw.load_watches(tmp_path)
    assert set(doc["watches"].keys()) == {"197"}


def test_load_malformed_yields_empty_schema(tmp_path: Path) -> None:
    """A malformed pr-watches.json yields an empty schema-v1 doc (never
    crashes the tick — the watch set self-heals from tasks.md)."""
    pw.watch_path(tmp_path).write_text("{ not json")
    doc = pw.load_watches(tmp_path)
    assert doc == {"schema": 1, "watches": {}}


# ---------------------------------------------------------------------------
# reduce_snapshot unit coverage
# ---------------------------------------------------------------------------


def test_reduce_merged_and_closed() -> None:
    anc = lambda a, d: True  # noqa: E731
    assert pw.reduce_snapshot(
        pw.PRSnapshot(pr_state="MERGED"), fresh_base_sha="b", is_ancestor=anc,
    ) == pw.EVENT_MERGED
    assert pw.reduce_snapshot(
        pw.PRSnapshot(pr_state="OPEN", merged_at="2026"), fresh_base_sha="b",
        is_ancestor=anc,
    ) == pw.EVENT_MERGED
    assert pw.reduce_snapshot(
        pw.PRSnapshot(pr_state="CLOSED"), fresh_base_sha="b", is_ancestor=anc,
    ) == pw.EVENT_CLOSED_UNMERGED


def test_reduce_error_snapshots() -> None:
    anc = lambda a, d: True  # noqa: E731
    assert pw.reduce_snapshot(
        pw.PRSnapshot(error="boom"), fresh_base_sha="b", is_ancestor=anc,
    ) == pw.EVENT_SKIP
    assert pw.reduce_snapshot(
        pw.PRSnapshot(error="gone", not_found=True), fresh_base_sha="b",
        is_ancestor=anc,
    ) == pw.EVENT_NOT_FOUND


def test_checks_from_rollup() -> None:
    assert pw._checks_from_rollup([]) == "SUCCESS"
    assert pw._checks_from_rollup(None) == "SUCCESS"
    assert pw._checks_from_rollup(
        [{"status": "COMPLETED", "conclusion": "SUCCESS"}]) == "SUCCESS"
    assert pw._checks_from_rollup(
        [{"status": "COMPLETED", "conclusion": "FAILURE"}]) == "FAILURE"
    assert pw._checks_from_rollup(
        [{"status": "IN_PROGRESS", "conclusion": ""}]) == "PENDING"
    assert pw._checks_from_rollup(
        [{"status": "COMPLETED", "conclusion": "SUCCESS"},
         {"status": "IN_PROGRESS", "conclusion": ""}]) == "PENDING"
    # StatusContext shape (state field).
    assert pw._checks_from_rollup([{"state": "FAILURE"}]) == "FAILURE"


# ---------------------------------------------------------------------------
# GhGitProber + derive_owner_repo (fake subprocess)
# ---------------------------------------------------------------------------


class _FakeProc:
    def __init__(self, returncode=0, stdout="", stderr=""):
        self.returncode = returncode
        self.stdout = stdout
        self.stderr = stderr


def test_parse_remote_owner_repo() -> None:
    cases = {
        "https://github.com/edisonshen/fleet.git": "edisonshen/fleet",
        "https://github.com/edisonshen/fleet": "edisonshen/fleet",
        "git@github.com:edisonshen/fleet.git": "edisonshen/fleet",
        # dotted repo name must survive (codex iter-1 [P2]).
        "https://github.com/owner/foo.bar.git": "owner/foo.bar",
        "git@github.com:owner/foo.bar": "owner/foo.bar",
        # explicit port must parse (codex round 29 [P2]).
        "ssh://git@github.com:22/owner/repo.git": "owner/repo",
        "https://github.com:443/owner/repo": "owner/repo",
        # case-insensitive.
        "git@github.com:EdisonShen/Fleet.git": "edisonshen/fleet",
    }
    for url, expect in cases.items():
        assert pw.parse_remote_owner_repo(url) == expect, url
    # non-github / lookalike / path-embedded / empty -> None.
    assert pw.parse_remote_owner_repo("https://gitlab.com/a/b") is None
    assert pw.parse_remote_owner_repo("https://notgithub.com/a/b") is None
    assert pw.parse_remote_owner_repo("https://x/foo@github.com/a/b") is None
    assert pw.parse_remote_owner_repo("") is None


def test_derive_owner_repo_shells_and_parses(monkeypatch) -> None:
    monkeypatch.setattr(pw.subprocess, "run",
                        lambda *a, **k: _FakeProc(0, "git@github.com:edisonshen/fleet.git\n"))
    assert pw.derive_owner_repo("/repo") == "edisonshen/fleet"
    monkeypatch.setattr(pw.subprocess, "run", lambda *a, **k: _FakeProc(1, "", "no remote"))
    assert pw.derive_owner_repo("/repo") is None
    assert pw.derive_owner_repo("") is None


def test_ghgit_prober_fetches_current_head_then_base(monkeypatch) -> None:
    """The prober queries PRs FIRST (to learn the current head), then a
    BASE fetch (load-bearing) + a SEPARATE best-effort PR-head fetch via
    refs/pull/N/head (fork-safe, codex iter-11 [P2]), and rev-parses the
    base tracking ref (not FETCH_HEAD). Regression for codex iter-1 [P1]."""
    calls = []
    graphql_resp = json.dumps({"data": {"repository": {"pr5": {
        "number": 5, "state": "OPEN", "mergedAt": None,
        "mergeStateStatus": "BLOCKED", "reviewDecision": "APPROVED",
        "headRefName": "worker/x", "baseRefName": "main",
        "headRefOid": "HEADSHA", "baseRefOid": "OLDBASE",
        "commits": {"nodes": [{"commit": {"statusCheckRollup": {
            "state": "SUCCESS", "contexts": {"nodes": []}}}}]},
    }}}})

    def fake_run(cmd, **kw):
        calls.append(cmd)
        if cmd[:3] == ["gh", "api", "graphql"]:
            return _FakeProc(0, graphql_resp)
        if "cat-file" in cmd:
            return _FakeProc(1)               # head NOT local -> must fetch it
        if "fetch" in cmd:
            return _FakeProc(0)
        if "rev-parse" in cmd:
            return _FakeProc(0, "FRESHBASESHA\n")
        return _FakeProc(0)

    monkeypatch.setattr(pw.subprocess, "run", fake_run)
    prober = pw.GhGitProber()
    rp = prober.probe_repo("/repo", OWNER_REPO, "main", [5], [])
    # graphql ran before any fetch (current head learned first).
    gql_idx = next(i for i, c in enumerate(calls) if c[:3] == ["gh", "api", "graphql"])
    fetch_idxs = [i for i, c in enumerate(calls) if "fetch" in c]
    assert gql_idx < fetch_idxs[0]
    # base fetch carries the base tracking refspec, NOT the raw head OID.
    base_fetch = next(c for c in calls if "fetch" in c and any("refs/heads/main" in a for a in c))
    assert "refs/heads/main:refs/remotes/origin/main" in base_fetch
    assert "HEADSHA" not in base_fetch
    # head fetched in a SEPARATE command via the fork-safe PR head ref.
    head_fetch = next(c for c in calls if "fetch" in c and any("refs/pull/5/head" in a for a in c))
    assert "refs/pull/5/head" in head_fetch
    # fresh base rev-parsed from the tracking ref, not FETCH_HEAD.
    rp_cmd = next(c for c in calls if "rev-parse" in c)
    assert "refs/remotes/origin/main" in rp_cmd
    assert rp.fresh_base_sha == "FRESHBASESHA"
    assert rp.fresh_base_shas.get("main") == "FRESHBASESHA"
    assert rp.snapshots[5].head_ref_oid == "HEADSHA"
    assert rp.snapshots[5].base_ref_name == "main"
    assert rp.snapshots[5].checks == "SUCCESS"


def test_ghgit_prober_head_fetch_failure_does_not_poison_base(monkeypatch) -> None:
    """A failed PR-head fetch must NOT set fetch_error or wipe the base
    SHA — base classification for other PRs is unaffected (codex iter-11
    [P2])."""
    resp = json.dumps({"data": {"repository": {"pr5": {
        "number": 5, "state": "OPEN", "baseRefName": "main",
        "headRefOid": "FORKHEAD", "baseRefOid": "B", "mergeStateStatus": "CLEAN",
        "reviewDecision": "APPROVED", "commits": {"nodes": []}}}}})

    def fake_run(cmd, **kw):
        if cmd[:3] == ["gh", "api", "graphql"]:
            return _FakeProc(0, resp)
        if "cat-file" in cmd:
            return _FakeProc(1)               # head not local
        if "fetch" in cmd and any("refs/pull" in a for a in cmd):
            return _FakeProc(1, "", "couldn't find remote ref")  # head fetch FAILS
        if "fetch" in cmd:
            return _FakeProc(0)               # base fetch SUCCEEDS
        if "rev-parse" in cmd:
            return _FakeProc(0, "MAINSHA\n")
        return _FakeProc(0)

    monkeypatch.setattr(pw.subprocess, "run", fake_run)
    rp = pw.GhGitProber().probe_repo("/repo", OWNER_REPO, "main", [5], [])
    assert rp.fetch_error == ""                       # base fetch was fine
    assert rp.fresh_base_shas.get("main") == "MAINSHA"


def test_ghgit_prober_fetches_only_real_base_not_synthetic_main(monkeypatch) -> None:
    """The fetch targets the PR's REAL baseRefName, never a synthetic
    `main` hint that may not exist (codex iter-5 [P2]). A repo whose PR
    targets `trunk` must fetch trunk, not main."""
    calls = []
    resp = json.dumps({"data": {"repository": {"pr7": {
        "number": 7, "state": "OPEN", "headRefOid": "H7", "baseRefOid": "B",
        "baseRefName": "trunk", "mergeStateStatus": "CLEAN",
        "reviewDecision": "APPROVED", "commits": {"nodes": []}}}}})

    def fake_run(cmd, **kw):
        calls.append(cmd)
        if cmd[:3] == ["gh", "api", "graphql"]:
            return _FakeProc(0, resp)
        if "rev-parse" in cmd:
            return _FakeProc(0, "TRUNKSHA\n")
        return _FakeProc(0)

    monkeypatch.setattr(pw.subprocess, "run", fake_run)
    # caller hint is "main" but the PR targets "trunk".
    rp = pw.GhGitProber().probe_repo("/repo", OWNER_REPO, "main", [7], [])
    fetch_cmd = next(c for c in calls if "fetch" in c)
    assert "refs/heads/trunk:refs/remotes/origin/trunk" in fetch_cmd
    # the synthetic main hint is NOT fetched (it may not exist).
    assert "refs/heads/main:refs/remotes/origin/main" not in fetch_cmd
    assert rp.fresh_base_shas.get("trunk") == "TRUNKSHA"


def test_ghgit_prober_null_pr_is_not_found(monkeypatch) -> None:
    resp = json.dumps({"data": {"repository": {"pr9": None}}})
    monkeypatch.setattr(pw.subprocess, "run",
                        lambda cmd, **k: _FakeProc(0, resp) if cmd[:3] == ["gh", "api", "graphql"] else _FakeProc(0))
    prober = pw.GhGitProber()
    rp = prober.probe_repo("/repo", OWNER_REPO, "main", [9], [])
    assert rp.snapshots[9].not_found is True


def test_ghgit_prober_graphql_rate_limit_is_transient(monkeypatch) -> None:
    """HTTP 200 with a top-level errors array (rate-limit) + null alias ->
    TRANSIENT (not_found=False) so the watch backs off instead of raising
    a false pr-not-found (codex iter-9 [P2])."""
    resp = json.dumps({
        "data": {"repository": None},
        "errors": [{"type": "RATE_LIMITED", "message": "API rate limit exceeded"}],
    })
    monkeypatch.setattr(pw.subprocess, "run",
                        lambda cmd, **k: _FakeProc(0, resp) if cmd[:3] == ["gh", "api", "graphql"] else _FakeProc(0))
    rp = pw.GhGitProber().probe_repo("/repo", OWNER_REPO, "main", [9], [])
    assert rp.snapshots[9].error                      # has an error
    assert rp.snapshots[9].not_found is False         # NOT a real 404


def test_ghgit_prober_repo_level_not_found_is_transient(monkeypatch) -> None:
    """A repository-level NOT_FOUND (whole repository null — e.g. private
    repo, token lost scope) is TRANSIENT, not a per-PR 404 — else tracking
    parks forever even after auth is fixed (codex iter-15 [P2])."""
    resp = json.dumps({
        "data": {"repository": None},
        "errors": [{"type": "NOT_FOUND", "message": "Could not resolve to a Repository"}],
    })
    monkeypatch.setattr(pw.subprocess, "run",
                        lambda cmd, **k: _FakeProc(0, resp) if cmd[:3] == ["gh", "api", "graphql"] else _FakeProc(0))
    rp = pw.GhGitProber().probe_repo("/repo", OWNER_REPO, "main", [9, 10], [])
    for n in (9, 10):
        assert rp.snapshots[n].error
        assert rp.snapshots[n].not_found is False     # NOT parked


def test_ghgit_prober_graphql_explicit_not_found(monkeypatch) -> None:
    """A NOT_FOUND-typed GraphQL error with null alias IS a real 404."""
    resp = json.dumps({
        "data": {"repository": {"pr9": None}},
        "errors": [{"type": "NOT_FOUND", "message": "Could not resolve to a PullRequest"}],
    })
    monkeypatch.setattr(pw.subprocess, "run",
                        lambda cmd, **k: _FakeProc(0, resp) if cmd[:3] == ["gh", "api", "graphql"] else _FakeProc(0))
    rp = pw.GhGitProber().probe_repo("/repo", OWNER_REPO, "main", [9], [])
    assert rp.snapshots[9].not_found is True


def test_ghgit_prober_batches_one_call(monkeypatch) -> None:
    """Multiple watched PRs -> exactly ONE `gh api graphql` shell-out
    (codex iter-2 [P2]: one batched query per repo, not O(PRs))."""
    gql_calls = []
    resp = json.dumps({"data": {"repository": {
        "pr5": {"number": 5, "state": "OPEN", "headRefOid": "H5", "baseRefOid": "B",
                "mergeStateStatus": "CLEAN", "reviewDecision": "APPROVED",
                "commits": {"nodes": []}},
        "pr6": {"number": 6, "state": "MERGED", "headRefOid": "H6", "baseRefOid": "B",
                "mergedAt": "2026", "commits": {"nodes": []}},
    }}})

    def fake_run(cmd, **kw):
        if cmd[:3] == ["gh", "api", "graphql"]:
            gql_calls.append(cmd)
            return _FakeProc(0, resp)
        return _FakeProc(0, "SHA\n") if "rev-parse" in cmd else _FakeProc(0)

    monkeypatch.setattr(pw.subprocess, "run", fake_run)
    rp = pw.GhGitProber().probe_repo("/repo", OWNER_REPO, "main", [5, 6], [])
    assert len(gql_calls) == 1                       # ONE batched call
    # the single query aliases both PR numbers.
    qarg = next(a for a in gql_calls[0] if a.startswith("query="))
    assert "pr5: pullRequest(number:5)" in qarg
    assert "pr6: pullRequest(number:6)" in qarg
    assert rp.snapshots[5].pr_state == "OPEN"
    assert rp.snapshots[6].pr_state == "MERGED"


def test_persisted_foreign_watch_not_probed(tmp_path: Path) -> None:
    """A watch persisted on a prior tick (when coord_owner_repo was None)
    that belongs to a FOREIGN repo is NOT probed once the repo is
    derivable (codex iter-2 [P2] coord-scope re-assert on probe)."""
    doc = {"schema": 1, "watches": {
        "42": pw._new_watch(42, _pr_url(42, owner_repo="someone/other"), "b", "main"),
    }}
    pw.save_watches(tmp_path, doc)
    prober = FakeProber(snaps={42: pw.PRSnapshot(number=42, pr_state="MERGED")})
    flips = []
    out = _run([], tmp_path, prober, owner_repo=OWNER_REPO, flips=flips)
    assert prober.probe_calls == []                  # never probed
    assert flips == []                               # never flipped a task
    assert any("coord-scope" in e for e in out.errors)


# ---------------------------------------------------------------------------
# INTEGRATION: drive the watch through a real loop.tick (architect-level e2e)
# ---------------------------------------------------------------------------


def _it_task(slug: str, *, status="in-review", pr_url="", branch="",
             depends_on=None) -> parse.Task:
    return parse.Task(
        slug=slug, status=status, priority="P1", pr_url=pr_url, branch=branch,
        depends_on=list(depends_on or []),
        created=_dt.datetime(2026, 6, 3, 8, 0, tzinfo=_dt.timezone.utc),
        updated=_dt.datetime(2026, 6, 3, 8, 0, tzinfo=_dt.timezone.utc),
        spawned_by="user", spec="spec", acceptance="acc",
    )


@pytest.fixture
def it_home(tmp_path: Path) -> Path:
    home = tmp_path / "fleet"
    (home / "inbox" / "archive").mkdir(parents=True)
    (home / "projects").mkdir(parents=True)
    return home


@pytest.fixture
def it_project_dir(it_home: Path) -> Path:
    p = it_home / "projects" / "fleet"
    (p / ".locks").mkdir(parents=True)
    return p


def _write_tasks(project_dir: Path, tasks: list[parse.Task]) -> None:
    f = parse.File(schema=parse.SCHEMA_VERSION, tasks=tasks, footer="")
    parse.write(str(project_dir / "tasks.md"), f)


def test_tick_enrolls_and_surfaces_through_loop(
    it_home: Path, it_project_dir: Path, monkeypatch,
) -> None:
    """End-to-end through loop.tick: an owned in-review task with a pr_url
    -> the tick creates pr-watches.json, probes via the injected prober,
    and surfaces the watch state. No real gh/git, no real fleet binary."""
    _write_tasks(it_project_dir, [_it_task("foo", pr_url=_pr_url(195), branch="worker/foo")])

    snaps = {195: pw.PRSnapshot(number=195, pr_state="OPEN",
                                merge_state_status="CLEAN", checks="SUCCESS",
                                review_decision="APPROVED", head_ref_oid="H195")}
    prober = FakeProber(snaps=snaps, fresh_base="B", ancestors={("B", "H195")})

    monkeypatch.setattr(loop, "_pr_watch_prober", prober)
    monkeypatch.setattr(loop.pr_watch_mod, "derive_owner_repo", lambda *a, **k: OWNER_REPO)

    fleet_calls: list[list[str]] = []
    with patch.object(loop, "_run_fleet", side_effect=lambda cmd, timeout_s=30.0: fleet_calls.append(list(cmd))):
        result = loop.tick(
            "fleet", coord_id="cccccc01", cwd="/repo",
            fleet_home=str(it_home),
        )

    assert not result.skipped
    doc = pw.load_watches(it_project_dir)
    assert "195" in doc["watches"]
    assert doc["watches"]["195"]["tasks"] == ["foo"]
    assert doc["watches"]["195"]["last_event"] == pw.EVENT_READY
    # READY surfaced as a breadcrumb in the tick result.
    assert any("mergeable (READY)" in e for e in result.errors)
    assert prober.probe_calls == [(195,)]


def test_tick_merged_flips_task_done_through_loop(
    it_home: Path, it_project_dir: Path, monkeypatch,
) -> None:
    """End-to-end MERGED reconcile: the tick flips the backing task done
    via `fleet tasks set ... status=done` and prunes the watch."""
    _write_tasks(it_project_dir, [_it_task("foo", pr_url=_pr_url(195), branch="worker/foo")])

    snaps = {195: pw.PRSnapshot(number=195, pr_state="MERGED",
                                merged_at="2026-06-03T08:00:00Z", head_ref_oid="H")}
    prober = FakeProber(snaps=snaps)
    monkeypatch.setattr(loop, "_pr_watch_prober", prober)
    monkeypatch.setattr(loop.pr_watch_mod, "derive_owner_repo", lambda *a, **k: OWNER_REPO)

    fleet_calls: list[list[str]] = []
    with patch.object(loop, "_run_fleet", side_effect=lambda cmd, timeout_s=30.0: fleet_calls.append(list(cmd))):
        loop.tick("fleet", coord_id="cccccc01", cwd="/repo", fleet_home=str(it_home))

    # the watch flipped the task done via the fleet CLI seam.
    assert any(
        c[1:3] == ["tasks", "set"] and "status=done" in c and "foo" in c
        for c in fleet_calls
    ), f"expected status=done flip in {fleet_calls!r}"
    # worker_pid cleared too (no stale worker on a done task; iter-20 P2).
    assert any(
        c[1:3] == ["tasks", "set"] and "worker_pid=0" in c and "foo" in c
        for c in fleet_calls
    ), f"expected worker_pid=0 clear in {fleet_calls!r}"
    # watch pruned + file removed.
    assert not pw.watch_path(it_project_dir).exists()


def test_tick_coord_scope_skips_foreign_pr_through_loop(
    it_home: Path, it_project_dir: Path, monkeypatch,
) -> None:
    """A task whose PR lives in a foreign repo is never enrolled when the
    tick derives a different coord-owned repo (coord-scope strict)."""
    _write_tasks(it_project_dir, [
        _it_task("foo", pr_url=_pr_url(1, owner_repo="someone/other"), branch="worker/foo"),
    ])
    prober = FakeProber()
    monkeypatch.setattr(loop, "_pr_watch_prober", prober)
    monkeypatch.setattr(loop.pr_watch_mod, "derive_owner_repo", lambda *a, **k: OWNER_REPO)

    with patch.object(loop, "_run_fleet", side_effect=lambda cmd, timeout_s=30.0: None):
        result = loop.tick("fleet", coord_id="cccccc01", cwd="/repo", fleet_home=str(it_home))

    assert pw.load_watches(it_project_dir)["watches"] == {}
    assert prober.probe_calls == []
    assert any("coord-scope" in e for e in result.errors)


# ===========================================================================
# PR2 — auto-fix half (§5.1b eligibility, §5.1c rebase guard, §6 leases).
# Design: docs/DESIGN-coord-pr-watch-durable.md §5/§6.
#
# These exercise the pr_watch logic in isolation (the loop.py dispatch
# callback is a recording fake here; the loop wiring + prompts are covered
# by the integration tests further down). All deterministic — prober + the
# dispatch / agent-outcome / deps-done callbacks are injected fakes.
# ===========================================================================


class _DispatchRecorder:
    """Records ActionDispatch objects + returns a minted agent_id (or "" to
    simulate a launch failure for the failed_launch path)."""

    def __init__(self, *, return_id="aaaa0001", fail=False):
        self.calls = []
        self.return_id = return_id
        self.fail = fail

    def __call__(self, action):
        self.calls.append(action)
        if self.fail:
            return ""
        return self.return_id


def _stale_snap(n, head="HEAD1"):
    # OPEN, green CI, 0 reviews, BLOCKED word (stale-under-strict): head
    # does NOT contain fresh base -> EVENT_STALE.
    return pw.PRSnapshot(number=n, pr_state="OPEN", merge_state_status="BLOCKED",
                         review_decision="", checks="SUCCESS", head_ref_oid=head,
                         base_ref_name="main")


def _ci_fail_snap(n, head="HEAD1"):
    return pw.PRSnapshot(number=n, pr_state="OPEN", merge_state_status="UNSTABLE",
                         review_decision="", checks="FAILURE", head_ref_oid=head,
                         base_ref_name="main")


def _changes_snap(n, head="HEAD1"):
    return pw.PRSnapshot(number=n, pr_state="OPEN", merge_state_status="BLOCKED",
                         review_decision="CHANGES_REQUESTED", checks="SUCCESS",
                         head_ref_oid=head, base_ref_name="main")


def _run2(tasks, project_dir, prober, *, dispatch=None, agent_outcome=None,
          deps_done=None, owner_repo=OWNER_REPO, now="2026-06-03T08:00:00Z",
          tick_count=1, slow=5, flips=None, enroll=None, release=None):
    """reconcile_watches WITH the PR2 auto-fix seam wired."""
    def _flip(slug, pr_url=""):
        if flips is not None:
            flips.append(slug)
    return pw.reconcile_watches(
        tasks, project="p", project_dir=project_dir,
        coord_owner_repo=owner_repo, prober=prober,
        flip_task_done=_flip, now_iso=now, tick_count=tick_count,
        slow_cadence_ticks=slow, repo_path="/repo",
        enroll_tasks=enroll,
        dispatch_action=dispatch or _DispatchRecorder(),
        agent_outcome=agent_outcome or (lambda _aid: "running"),
        deps_done=deps_done or (lambda _deps: True),
        release_action=release,
    )


# --- §5.1b eligibility -----------------------------------------------------


def test_single_eligible_stale_pr_dispatches_rebase(tmp_path: Path) -> None:
    """One STALE next-to-merge PR, deps done, base main, nothing beneath ->
    exactly one rebase dispatch (§5.1b exactly-one)."""
    tasks = [_task("a", pr_url=_pr_url(195), branch="worker/a")]
    snaps = {195: _stale_snap(195, head="HEAD195")}
    # fresh base NOT an ancestor of head -> STALE.
    prober = FakeProber(snaps=snaps, fresh_base="FRESHBASE", ancestors=set())
    disp = _DispatchRecorder()
    out = _run2(tasks, tmp_path, prober, dispatch=disp)
    assert out.dispatched == 1
    assert len(disp.calls) == 1
    act = disp.calls[0]
    assert act.kind == pw.ACTION_REBASE
    assert act.pr_number == 195
    assert act.head_sha == "HEAD195"
    assert act.base_sha == "FRESHBASE"
    # lease persisted with the agent_id + running outcome.
    w = pw.load_watches(tmp_path)["watches"]["195"]
    assert w["inflight_action"]["outcome"] == pw.OUTCOME_RUNNING
    assert w["inflight_action"]["agent_id"] == "aaaa0001"
    assert w["inflight_action"]["key"] == act.key


def test_downstack_stale_pr_no_rebase(tmp_path: Path) -> None:
    """A downstack PR whose upstack PR is still OPEN (its head is an
    ancestor) -> NOT eligible -> no rebase, no raise (expected-stale)."""
    tasks = [
        _task("up", pr_url=_pr_url(195), branch="worker/up"),
        _task("down", pr_url=_pr_url(196), branch="worker/down"),
    ]
    snaps = {195: _stale_snap(195, head="UP"), 196: _stale_snap(196, head="DOWN")}
    # UP is an ancestor of DOWN -> DOWN sits above UP -> UP is next-to-merge.
    # But UP is also stale; only UP eligible (nothing beneath UP). DOWN has
    # UP beneath it -> NOT eligible.
    prober = FakeProber(snaps=snaps, fresh_base="FRESHBASE",
                        ancestors={("UP", "DOWN")})
    disp = _DispatchRecorder()
    out = _run2(tasks, tmp_path, prober, dispatch=disp)
    # exactly one eligible (UP) -> one rebase, for 195 not 196.
    assert out.dispatched == 1
    assert disp.calls[0].pr_number == 195


def test_two_independent_eligible_raises_no_rebase(tmp_path: Path) -> None:
    """Two independent eligible STALE PRs (neither beneath the other) ->
    ambiguous -> raise-hand, ZERO auto-rebase (§5.1b never-guess)."""
    tasks = [
        _task("a", pr_url=_pr_url(195), branch="worker/a"),
        _task("b", pr_url=_pr_url(196), branch="worker/b"),
    ]
    snaps = {195: _stale_snap(195, head="A"), 196: _stale_snap(196, head="B")}
    prober = FakeProber(snaps=snaps, fresh_base="FRESHBASE", ancestors=set())
    disp = _DispatchRecorder()
    out = _run2(tasks, tmp_path, prober, dispatch=disp)
    assert out.dispatched == 0
    assert disp.calls == []
    assert any("AMBIGUOUS" in r for r in out.raises)


def test_deps_not_done_not_eligible(tmp_path: Path) -> None:
    """A STALE PR whose task depends on an unfinished task -> NOT eligible
    -> no rebase (zero-eligible noop)."""
    tasks = [_task("a", pr_url=_pr_url(195), branch="worker/a", depends_on=["x"])]
    snaps = {195: _stale_snap(195, head="A")}
    prober = FakeProber(snaps=snaps, fresh_base="FRESHBASE", ancestors=set())
    disp = _DispatchRecorder()
    out = _run2(tasks, tmp_path, prober, dispatch=disp, deps_done=lambda deps: not deps)
    assert out.dispatched == 0
    assert disp.calls == []


def test_high_number_base_not_mispicked(tmp_path: Path) -> None:
    """A HIGH-number PR (#300) that is actually the BASE of a low-number
    one (#195) is the next-to-merge; PR number must not pick #195. #300's
    head is an ancestor of #195's head."""
    tasks = [
        _task("base", pr_url=_pr_url(300), branch="worker/base"),
        _task("top", pr_url=_pr_url(195), branch="worker/top"),
    ]
    snaps = {300: _stale_snap(300, head="BASE"), 195: _stale_snap(195, head="TOP")}
    prober = FakeProber(snaps=snaps, fresh_base="FRESHBASE",
                        ancestors={("BASE", "TOP")})
    disp = _DispatchRecorder()
    out = _run2(tasks, tmp_path, prober, dispatch=disp)
    assert out.dispatched == 1
    assert disp.calls[0].pr_number == 300  # the base, not the high/low number


# --- fix dispatch ----------------------------------------------------------


def test_ci_failure_dispatches_fix(tmp_path: Path) -> None:
    tasks = [_task("a", pr_url=_pr_url(195), branch="worker/a")]
    snaps = {195: _ci_fail_snap(195, head="H1")}
    prober = FakeProber(snaps=snaps, fresh_base="FRESHBASE", ancestors=set())
    disp = _DispatchRecorder()
    out = _run2(tasks, tmp_path, prober, dispatch=disp)
    assert out.dispatched == 1
    assert disp.calls[0].kind == pw.ACTION_FIX
    assert disp.calls[0].event == pw.EVENT_CI_FAILED


def test_changes_requested_dispatches_fix(tmp_path: Path) -> None:
    tasks = [_task("a", pr_url=_pr_url(195), branch="worker/a")]
    snaps = {195: _changes_snap(195, head="H1")}
    prober = FakeProber(snaps=snaps, fresh_base="FRESHBASE", ancestors=set())
    disp = _DispatchRecorder()
    out = _run2(tasks, tmp_path, prober, dispatch=disp)
    assert out.dispatched == 1
    assert disp.calls[0].kind == pw.ACTION_FIX
    assert disp.calls[0].event == pw.EVENT_CHANGES_REQUESTED


# --- §6 idempotency / lease ------------------------------------------------


def test_running_lease_suppresses_redispatch(tmp_path: Path) -> None:
    """Same head + same event next tick while the lease is running + agent
    alive -> no duplicate dispatch."""
    tasks = [_task("a", pr_url=_pr_url(195), branch="worker/a")]
    snaps = {195: _ci_fail_snap(195, head="H1")}
    prober = FakeProber(snaps=snaps, fresh_base="FRESHBASE", ancestors=set())
    disp = _DispatchRecorder()
    _run2(tasks, tmp_path, prober, dispatch=disp, tick_count=1)
    # tick 2: lease still running, agent alive -> suppressed.
    out2 = _run2(tasks, tmp_path, prober, dispatch=disp, tick_count=2,
                 agent_outcome=lambda _aid: "running")
    assert out2.dispatched == 0
    assert len(disp.calls) == 1  # only the first tick dispatched


def test_succeeded_head_change_redispatches(tmp_path: Path) -> None:
    """After the fixer pushes (head advances), the old lease retires
    (succeeded) and a NEW actionable event on the new head re-dispatches."""
    tasks = [_task("a", pr_url=_pr_url(195), branch="worker/a")]
    disp = _DispatchRecorder()
    # tick 1: CI fail on H1 -> dispatch fix.
    p1 = FakeProber(snaps={195: _ci_fail_snap(195, head="H1")},
                    fresh_base="FRESHBASE", ancestors=set())
    _run2(tasks, tmp_path, p1, dispatch=disp, tick_count=1)
    assert len(disp.calls) == 1
    # tick 2: head advanced to H2, still CI-failing -> old lease retires
    # (head moved) + new dispatch for H2.
    p2 = FakeProber(snaps={195: _ci_fail_snap(195, head="H2")},
                    fresh_base="FRESHBASE", ancestors=set())
    out2 = _run2(tasks, tmp_path, p2, dispatch=disp, tick_count=2,
                 agent_outcome=lambda _aid: "running")
    assert out2.dispatched == 1
    assert len(disp.calls) == 2
    assert disp.calls[1].head_sha == "H2"


def test_failed_launch_retries(tmp_path: Path) -> None:
    """A dispatch that fails to launch (callback returns "") marks the
    lease failed_launch -> next tick retries."""
    tasks = [_task("a", pr_url=_pr_url(195), branch="worker/a")]
    snaps = {195: _ci_fail_snap(195, head="H1")}
    prober = FakeProber(snaps=snaps, fresh_base="FRESHBASE", ancestors=set())
    fail_disp = _DispatchRecorder(fail=True)
    out1 = _run2(tasks, tmp_path, prober, dispatch=fail_disp, tick_count=1)
    assert out1.dispatched == 0
    w = pw.load_watches(tmp_path)["watches"]["195"]
    assert w["inflight_action"]["outcome"] == pw.OUTCOME_FAILED_LAUNCH
    # tick 2: a working dispatcher retries.
    ok_disp = _DispatchRecorder()
    out2 = _run2(tasks, tmp_path, prober, dispatch=ok_disp, tick_count=2)
    assert out2.dispatched == 1


def test_dead_agent_lease_reclaimed_and_redispatched(tmp_path: Path) -> None:
    """A running lease whose agent is gone (and head unchanged, not blocked)
    -> reclaimed -> re-dispatch allowed."""
    tasks = [_task("a", pr_url=_pr_url(195), branch="worker/a")]
    snaps = {195: _ci_fail_snap(195, head="H1")}
    prober = FakeProber(snaps=snaps, fresh_base="FRESHBASE", ancestors=set())
    disp = _DispatchRecorder()
    _run2(tasks, tmp_path, prober, dispatch=disp, tick_count=1)
    # tick 2: agent gone, head unchanged -> reclaim + re-dispatch.
    out2 = _run2(tasks, tmp_path, prober, dispatch=disp, tick_count=2,
                 agent_outcome=lambda _aid: "gone")
    assert out2.dispatched == 1
    assert len(disp.calls) == 2


def test_expired_lease_reclaimed(tmp_path: Path) -> None:
    """A running lease past the expiry tick budget with no liveness signal
    -> reclaimed -> re-dispatch."""
    tasks = [_task("a", pr_url=_pr_url(195), branch="worker/a")]
    snaps = {195: _ci_fail_snap(195, head="H1")}
    prober = FakeProber(snaps=snaps, fresh_base="FRESHBASE", ancestors=set())
    disp = _DispatchRecorder()
    _run2(tasks, tmp_path, prober, dispatch=disp, tick_count=1)
    # tick way past expiry; agent still "alive" but expired -> reclaim.
    out2 = _run2(tasks, tmp_path, prober, dispatch=disp,
                 tick_count=1 + pw._LEASE_EXPIRY_TICKS,
                 agent_outcome=lambda _aid: "running")
    assert out2.dispatched == 1


def test_blocked_outcome_raises_once_not_refired(tmp_path: Path) -> None:
    """A subagent that reports BLOCKED (conflict / guard abort) -> ledger
    blocked + raise-hand ONCE; subsequent ticks suppress (no silent re-fire,
    no repeated raise)."""
    tasks = [_task("a", pr_url=_pr_url(195), branch="worker/a")]
    snaps = {195: _stale_snap(195, head="H1")}
    prober = FakeProber(snaps=snaps, fresh_base="FRESHBASE", ancestors=set())
    disp = _DispatchRecorder()
    _run2(tasks, tmp_path, prober, dispatch=disp, tick_count=1)
    # tick 2: agent reports blocked -> ledger blocked + one raise.
    out2 = _run2(tasks, tmp_path, prober, dispatch=disp, tick_count=2,
                 agent_outcome=lambda _aid: "blocked")
    assert out2.dispatched == 0
    assert any("BLOCKED" in r for r in out2.raises)
    # tick 3: still stale, blocked latched -> suppressed, NO new raise.
    out3 = _run2(tasks, tmp_path, prober, dispatch=disp, tick_count=3,
                 agent_outcome=lambda _aid: "gone")
    assert out3.dispatched == 0
    assert not any("BLOCKED" in r for r in out3.raises)


def test_preserved_todo_slug_not_auto_fixed(tmp_path: Path) -> None:
    """A watch backed ONLY by a preserved CI-red-requeued `todo` slug (its
    pr_url cleared, kept for old-PR-merge reconcile) is NOT auto-fixed
    (codex iter-7 [P2]) — that retry rides the normal cap'd worker path."""
    # tick 1: enroll the watch while the task still has the PR url.
    pre = [_task("a", status="in-review", pr_url=_pr_url(195), branch="worker/a")]
    pw.reconcile_watches(
        pre, project="p", project_dir=tmp_path, coord_owner_repo=OWNER_REPO,
        prober=FakeProber(snaps={195: _ci_fail_snap(195, head="H1")},
                          fresh_base="B", ancestors=set()),
        flip_task_done=lambda s, u="": None,
        now_iso="2026-06-03T08:00:00Z", tick_count=1, repo_path="/repo",
    )
    # tick 2: task 'a' is now a preserved todo (no pr_url); PR still CI-red.
    current = [_task("a", status="todo", pr_url="", branch="worker/a")]
    disp = _DispatchRecorder()
    out = _run2(current, tmp_path, FakeProber(snaps={195: _ci_fail_snap(195, head="H1")},
                                              fresh_base="B", ancestors=set()),
                dispatch=disp, tick_count=2, enroll=pre)
    assert out.dispatched == 0
    assert disp.calls == []


def test_foreign_repo_same_number_does_not_enable_dispatch(tmp_path: Path) -> None:
    """A foreign-repo task sharing a PR number must NOT make a preserved
    owned watch look dispatchable (codex iter-8 [P2] — gate on owner/repo,
    not the bare number)."""
    # tick 1: enroll the owned watch (#195 in OWNER_REPO).
    pre = [_task("a", status="in-review", pr_url=_pr_url(195), branch="worker/a")]
    pw.reconcile_watches(
        pre, project="p", project_dir=tmp_path, coord_owner_repo=OWNER_REPO,
        prober=FakeProber(snaps={195: _ci_fail_snap(195, head="H1")},
                          fresh_base="B", ancestors=set()),
        flip_task_done=lambda s, u="": None,
        now_iso="2026-06-03T08:00:00Z", tick_count=1, repo_path="/repo",
    )
    # tick 2: owned task 'a' is a preserved todo (no url); a FOREIGN-repo
    # task 'b' happens to point at #195 in someone/other.
    current = [
        _task("a", status="todo", pr_url="", branch="worker/a"),
        _task("b", status="in-review",
              pr_url=_pr_url(195, owner_repo="someone/other"), branch="worker/b"),
    ]
    disp = _DispatchRecorder()
    out = _run2(current, tmp_path,
                FakeProber(snaps={195: _ci_fail_snap(195, head="H1")},
                           fresh_base="B", ancestors=set()),
                dispatch=disp, tick_count=2, enroll=pre)
    # the foreign #195 must not enable an auto-fix on the owned watch.
    assert out.dispatched == 0
    assert disp.calls == []


def test_running_lease_released_when_pr_merges(tmp_path: Path) -> None:
    """A PR that MERGES while a fixer lease is still running -> the lease's
    journal+inbox are released before the terminal prune (codex iter-7
    [P3]) — no pr-* journal leak."""
    tasks = [_task("a", pr_url=_pr_url(195), branch="worker/a")]
    disp = _DispatchRecorder()
    # tick 1: CI fail -> dispatch fix (lease running).
    p1 = FakeProber(snaps={195: _ci_fail_snap(195, head="H1")},
                    fresh_base="B", ancestors=set())
    _run2(tasks, tmp_path, p1, dispatch=disp, tick_count=1)
    leased = pw.load_watches(tmp_path)["watches"]["195"]["inflight_action"]["agent_id"]
    # tick 2: PR merges while the fixer is still running.
    p2 = FakeProber(snaps={195: pw.PRSnapshot(number=195, pr_state="MERGED",
                                              merged_at="t", head_ref_oid="H1",
                                              base_ref_name="main")},
                    fresh_base="B", ancestors=set())
    released: list[str] = []
    pw.reconcile_watches(
        tasks, project="p", project_dir=tmp_path, coord_owner_repo=OWNER_REPO,
        prober=p2, flip_task_done=lambda s, u="": None,
        now_iso="2026-06-03T08:00:00Z", tick_count=2, repo_path="/repo",
        dispatch_action=disp, agent_outcome=lambda _a: "running",
        deps_done=lambda _d: True, release_action=lambda aid: released.append(aid),
    )
    assert leased in released  # journal released before prune
    assert "195" not in pw.load_watches(tmp_path)["watches"]  # pruned


def test_no_action_on_stale_probe_tick(tmp_path: Path) -> None:
    """If the probe is SKIPPED this tick (transient/backoff), the dispatch
    pass acts on NOTHING — no reclaim, no re-dispatch off a stale head
    (codex iter-6 [P1]). We dispatch on tick 1 (fresh probe), then tick 2
    with a whole-repo transient error: the lease must be untouched + no new
    dispatch."""
    tasks = [_task("a", pr_url=_pr_url(195), branch="worker/a")]
    snaps = {195: _ci_fail_snap(195, head="H1")}
    p1 = FakeProber(snaps=snaps, fresh_base="FRESHBASE", ancestors=set())
    disp = _DispatchRecorder()
    _run2(tasks, tmp_path, p1, dispatch=disp, tick_count=1)
    lease1 = pw.load_watches(tmp_path)["watches"]["195"]["inflight_action"]
    assert lease1 and lease1["outcome"] == pw.OUTCOME_RUNNING
    # tick 2: probe transient-fails -> _probed_ok_at not advanced -> the
    # dispatch pass skips this watch entirely (lease preserved, agent gone
    # reported but NOT reclaimed because we never reach reclaim).
    p2 = FakeProber(repo_error="gh 503")
    out2 = _run2(tasks, tmp_path, p2, dispatch=disp, tick_count=2,
                 agent_outcome=lambda _aid: "gone")
    assert out2.dispatched == 0
    lease2 = pw.load_watches(tmp_path)["watches"]["195"]["inflight_action"]
    assert lease2 is not None  # untouched, NOT reclaimed off the stale tick


def test_release_called_on_lease_resolution(tmp_path: Path) -> None:
    """When a lease leaves `running` (here: agent gone, head unchanged ->
    reclaim), release_action is invoked with the prior agent_id so its
    journal+inbox get reaped (codex iter-6 [P2] cleanup)."""
    tasks = [_task("a", pr_url=_pr_url(195), branch="worker/a")]
    snaps = {195: _ci_fail_snap(195, head="H1")}
    prober = FakeProber(snaps=snaps, fresh_base="FRESHBASE", ancestors=set())
    disp = _DispatchRecorder()
    _run2(tasks, tmp_path, prober, dispatch=disp, tick_count=1)
    leased_agent = pw.load_watches(tmp_path)["watches"]["195"]["inflight_action"]["agent_id"]
    released: list[str] = []
    # tick 2: agent gone -> reclaim -> release_action(leased_agent).
    pw.reconcile_watches(
        tasks, project="p", project_dir=tmp_path,
        coord_owner_repo=OWNER_REPO, prober=prober,
        flip_task_done=lambda s, u="": None, now_iso="2026-06-03T08:00:00Z",
        tick_count=2, slow_cadence_ticks=5, repo_path="/repo",
        dispatch_action=disp, agent_outcome=lambda _aid: "gone",
        deps_done=lambda _d: True,
        release_action=lambda aid: released.append(aid),
    )
    assert leased_agent in released


def test_active_retry_not_preserved_on_old_watch(tmp_path: Path) -> None:
    """A CI-red task whose pr_url was cleared + RE-DISPATCHED (current row
    in-progress, empty pr_url) must NOT be re-added to the OLD PR's watch
    via pre-reconcile enrollment (codex iter-6 [P1]) — else a merge of the
    old PR flips the live retry done."""
    # pre-reconcile snapshot: task 'a' still had the old PR url.
    pre = [_task("a", status="in-review", pr_url=_pr_url(195), branch="worker/a")]
    # current snapshot: 'a' was requeued + re-dispatched -> in-progress, no url.
    current = [_task("a", status="in-progress", pr_url="", branch="worker/a")]
    # old PR 195 merges this tick.
    snaps = {195: pw.PRSnapshot(number=195, pr_state="MERGED", merged_at="t",
                                head_ref_oid="H1", base_ref_name="main")}
    prober = FakeProber(snaps=snaps, fresh_base="B", ancestors=set())
    flips: list[str] = []
    # seed the watch first (tick 1, pre-reconcile had the url).
    pw.reconcile_watches(
        pre, project="p", project_dir=tmp_path, coord_owner_repo=OWNER_REPO,
        prober=FakeProber(snaps={195: _ci_fail_snap(195, head="H1")},
                          fresh_base="B", ancestors=set()),
        flip_task_done=lambda s, u="": flips.append(s),
        now_iso="2026-06-03T08:00:00Z", tick_count=1, repo_path="/repo",
    )
    # tick 2: current row is in-progress/no-url; old PR merges. The active
    # retry 'a' must NOT be flipped done.
    out = pw.reconcile_watches(
        current, project="p", project_dir=tmp_path, coord_owner_repo=OWNER_REPO,
        prober=prober, flip_task_done=lambda s, u="": flips.append(s),
        now_iso="2026-06-03T08:00:00Z", tick_count=2, repo_path="/repo",
        enroll_tasks=pre,
    )
    assert "a" not in flips  # the active retry was NOT flipped done


def test_orphan_open_pr_does_not_block_legit_rebase(tmp_path: Path) -> None:
    """A parked-open ORPHANED PR (no backing task) must not count as a
    next-to-merge candidate; a single legitimately-stale BACKED PR alongside
    it still auto-rebases (codex iter-5 [P2] — orphan doesn't inflate the
    eligible set to ambiguous)."""
    # Seed two watches via two ticks. PR 300 is orphaned-open (its task gets
    # deleted); PR 195 is backed + stale. They are NOT in each other's
    # ancestry (independent), so without the fix both would be 'eligible'.
    tasks_t1 = [
        _task("a", pr_url=_pr_url(195), branch="worker/a"),
        _task("orph", pr_url=_pr_url(300), branch="worker/orph"),
    ]
    green300 = pw.PRSnapshot(number=300, pr_state="OPEN", merge_state_status="BLOCKED",
                             checks="SUCCESS", review_decision="", head_ref_oid="O300",
                             base_ref_name="main")
    snaps_t1 = {195: _stale_snap(195, head="H195"), 300: green300}
    prober1 = FakeProber(snaps=snaps_t1, fresh_base="FRESHBASE", ancestors=set())
    _run2(tasks_t1, tmp_path, prober1, dispatch=_DispatchRecorder(), tick_count=1)

    # tick 2: PR300's task deleted (orphaned), PR195 still stale + backed.
    tasks_t2 = [_task("a", pr_url=_pr_url(195), branch="worker/a")]
    snaps_t2 = {195: _stale_snap(195, head="H195"), 300: green300}
    prober2 = FakeProber(snaps=snaps_t2, fresh_base="FRESHBASE", ancestors=set())
    disp = _DispatchRecorder()
    out = _run2(tasks_t2, tmp_path, prober2, dispatch=disp, tick_count=2)
    # the orphan (300) is raised-hand but does NOT block 195's rebase.
    assert out.dispatched == 1
    assert disp.calls[0].pr_number == 195
    assert disp.calls[0].kind == pw.ACTION_REBASE


def test_pr_watch_journals_excluded_from_worker_replay(tmp_path: Path) -> None:
    """A PR-watch dispatch journal (kind pr-fix / pr-rebase) is NOT a worker
    dispatch -> excluded from _iter_project_journals so the worker replay /
    residual-crash path never fires a false phantom escalation against a
    live PR-watch subagent (codex iter-4 [P2]). A real worker journal in the
    same dir is still yielded."""
    home = tmp_path / "fleet"
    disp = home / "dispatches"
    disp.mkdir(parents=True)
    (disp / "aaaa0001.json").write_text(json.dumps({
        "owner": "project/myproj/slug/pr-fix-195", "kind": "pr-fix",
        "exec_state": "launch_attempted",
    }))
    (disp / "bbbb0002.json").write_text(json.dumps({
        "owner": "project/myproj/slug/pr-rebase-196", "kind": "pr-rebase",
        "exec_state": "pending",
    }))
    (disp / "cccc0003.json").write_text(json.dumps({
        "owner": "project/myproj/slug/real-worker", "kind": "worker",
        "exec_state": "pending",
    }))
    got = {aid for aid, _slug, _j in loop._iter_project_journals(home, "myproj")}
    assert got == {"cccc0003"}  # only the real worker, not the pr-* journals


def test_orphan_watch_no_auto_dispatch(tmp_path: Path) -> None:
    """A watch whose backing task was deleted while the PR is still OPEN +
    actionable -> NO auto fix/rebase (orphan -> raise-hand only, §5/§6
    codex iter-3 [P1]). The orphan raise still fires."""
    # tick 1: enroll + dispatch nothing (green) so the watch exists.
    tasks = [_task("a", pr_url=_pr_url(195), branch="worker/a")]
    p_green = pw.PRSnapshot(number=195, pr_state="OPEN", merge_state_status="CLEAN",
                            checks="SUCCESS", review_decision="APPROVED",
                            head_ref_oid="H1", base_ref_name="main")
    prober1 = FakeProber(snaps={195: p_green}, fresh_base="B", ancestors={("B", "H1")})
    _run2(tasks, tmp_path, prober1, dispatch=_DispatchRecorder(), tick_count=1)
    # tick 2: task deleted (empty task list), PR now CI-FAILING + still OPEN.
    disp = _DispatchRecorder()
    prober2 = FakeProber(snaps={195: _ci_fail_snap(195, head="H1")},
                         fresh_base="B", ancestors=set())
    out = _run2([], tmp_path, prober2, dispatch=disp, tick_count=2)
    assert out.dispatched == 0
    assert disp.calls == []
    assert any("orphaned-pr" in r for r in out.raises)


def test_live_lease_blocks_different_event_same_head(tmp_path: Path) -> None:
    """A live running lease for one event suppresses a dispatch for a
    DIFFERENT event on the SAME head — one in-flight action per watch
    (codex iter-2 [P1]: never put two agents on the same branch)."""
    tasks = [_task("a", pr_url=_pr_url(195), branch="worker/a")]
    disp = _DispatchRecorder()
    # tick 1: CI fail on H1 -> dispatch fix.
    p1 = FakeProber(snaps={195: _ci_fail_snap(195, head="H1")},
                    fresh_base="FRESHBASE", ancestors=set())
    _run2(tasks, tmp_path, p1, dispatch=disp, tick_count=1)
    assert len(disp.calls) == 1
    # tick 2: SAME head H1, but now CHANGES_REQUESTED (a different event/key)
    # while the fix agent is still alive -> must NOT dispatch a second agent.
    p2 = FakeProber(snaps={195: _changes_snap(195, head="H1")},
                    fresh_base="FRESHBASE", ancestors=set())
    out2 = _run2(tasks, tmp_path, p2, dispatch=disp, tick_count=2,
                 agent_outcome=lambda _aid: "running")
    assert out2.dispatched == 0
    assert len(disp.calls) == 1  # the live lease is preserved


def test_watch_base_refreshed_from_snapshot(tmp_path: Path) -> None:
    """A PR targeting a non-main base -> watch['base'] is refreshed from the
    probe snapshot (codex iter-2 [P2]); a non-main PR is NOT treated as
    main-based / rebase-eligible."""
    tasks = [_task("a", pr_url=_pr_url(195), branch="worker/a")]
    snap = pw.PRSnapshot(number=195, pr_state="OPEN", merge_state_status="BLOCKED",
                         review_decision="", checks="SUCCESS", head_ref_oid="H1",
                         base_ref_name="release-branch")
    prober = FakeProber(snaps={195: snap}, fresh_base="FRESHBASE", ancestors=set())
    disp = _DispatchRecorder()
    out = _run2(tasks, tmp_path, prober, dispatch=disp)
    # base refreshed to the real ref.
    w = pw.load_watches(tmp_path)["watches"]["195"]
    assert w["base"] == "release-branch"
    # non-main base -> not rebase-eligible -> no rebase dispatched.
    assert out.dispatched == 0
    assert disp.calls == []


def test_dispatched_events_pruned_by_head(tmp_path: Path) -> None:
    """dispatched_events keys whose head != current head are pruned on each
    persist so the file stays bounded (§6)."""
    tasks = [_task("a", pr_url=_pr_url(195), branch="worker/a")]
    disp = _DispatchRecorder()
    # tick 1: fix on H1 -> dispatch.
    p1 = FakeProber(snaps={195: _ci_fail_snap(195, head="H1")},
                    fresh_base="FRESHBASE", ancestors=set())
    _run2(tasks, tmp_path, p1, dispatch=disp, tick_count=1)
    # tick 2: head H2 -> H1 lease retires to ledger (succeeded@H1).
    p2 = FakeProber(snaps={195: _ci_fail_snap(195, head="H2")},
                    fresh_base="FRESHBASE", ancestors=set())
    _run2(tasks, tmp_path, p2, dispatch=disp, tick_count=2,
          agent_outcome=lambda _aid: "running")
    w = pw.load_watches(tmp_path)["watches"]["195"]
    de = w["dispatched_events"]
    # the H1 key was pruned (head advanced to H2); only H2-headed keys (if
    # any) survive. The succeeded@H1 ledger entry's key embeds H1 -> pruned.
    assert all("H1" not in k for k in de)


def test_pr2_active_suppresses_pr1_surface_raise(tmp_path: Path) -> None:
    """With the PR2 seam wired, a dispatchable actionable event is NOT also
    surfaced as the PR1 'surfaces only' raise (no double-noise)."""
    tasks = [_task("a", pr_url=_pr_url(195), branch="worker/a")]
    snaps = {195: _ci_fail_snap(195, head="H1")}
    prober = FakeProber(snaps=snaps, fresh_base="FRESHBASE", ancestors=set())
    out = _run2(tasks, tmp_path, prober, dispatch=_DispatchRecorder())
    assert not any("surfaces only" in r for r in out.raises)


def test_dedupe_two_tasks_one_pr_one_dispatch(tmp_path: Path) -> None:
    """Two task slugs sharing one PR -> one watch -> one fix dispatch."""
    tasks = [
        _task("a", pr_url=_pr_url(195), branch="worker/a"),
        _task("b", pr_url=_pr_url(195), branch="worker/a"),
    ]
    snaps = {195: _ci_fail_snap(195, head="H1")}
    prober = FakeProber(snaps=snaps, fresh_base="FRESHBASE", ancestors=set())
    disp = _DispatchRecorder()
    out = _run2(tasks, tmp_path, prober, dispatch=disp)
    assert out.dispatched == 1
    assert len(disp.calls) == 1


# --- crash matrix (durability core) ----------------------------------------


def test_crash_after_lease_before_launch_no_dup(tmp_path: Path) -> None:
    """Simulate a crash AFTER the lease persisted but BEFORE the launch
    completed: the dispatch callback raises. The lease is marked
    failed_launch; the next tick retries (no lost watch, no dup)."""
    tasks = [_task("a", pr_url=_pr_url(195), branch="worker/a")]
    snaps = {195: _ci_fail_snap(195, head="H1")}
    prober = FakeProber(snaps=snaps, fresh_base="FRESHBASE", ancestors=set())

    def _boom(_action):
        raise RuntimeError("crash mid-launch")

    out1 = _run2(tasks, tmp_path, prober, dispatch=_boom, tick_count=1)
    assert out1.dispatched == 0
    w = pw.load_watches(tmp_path)["watches"]["195"]
    assert w["inflight_action"]["outcome"] == pw.OUTCOME_FAILED_LAUNCH
    # next tick retries cleanly.
    ok = _DispatchRecorder()
    out2 = _run2(tasks, tmp_path, prober, dispatch=ok, tick_count=2)
    assert out2.dispatched == 1


def test_crash_after_launch_before_clear_replays_not_dups(tmp_path: Path) -> None:
    """A crash after launch but before the watch-clear: the running lease is
    on disk. A fresh process (new reconcile) loads it and suppresses
    re-dispatch while the agent is alive (replay, not dup)."""
    tasks = [_task("a", pr_url=_pr_url(195), branch="worker/a")]
    snaps = {195: _ci_fail_snap(195, head="H1")}
    prober = FakeProber(snaps=snaps, fresh_base="FRESHBASE", ancestors=set())
    disp = _DispatchRecorder()
    _run2(tasks, tmp_path, prober, dispatch=disp, tick_count=1)
    # simulate a fresh process: a brand-new dispatch recorder, lease loaded
    # from disk, agent reported alive -> suppress.
    disp2 = _DispatchRecorder()
    out2 = _run2(tasks, tmp_path, prober, dispatch=disp2, tick_count=2,
                 agent_outcome=lambda _aid: "running")
    assert out2.dispatched == 0
    assert disp2.calls == []


def test_no_rebase_when_base_sha_unknown(tmp_path: Path) -> None:
    """A STALE next-to-merge PR whose probe could not capture a fresh base
    SHA (cadence/backoff skip) -> no rebase dispatched onto an unknown base.
    We simulate by reconciling with a watch that has a stale event but no
    probe this tick (slow cadence, READY-quiescent never applies to STALE,
    so instead drive an empty base)."""
    tasks = [_task("a", pr_url=_pr_url(195), branch="worker/a")]
    snaps = {195: _stale_snap(195, head="H1")}
    # fresh_base empty -> base_sha_for returns "" -> rebase skipped.
    prober = FakeProber(snaps=snaps, fresh_base="", ancestors=set(),
                        fetch_error="net blip")
    disp = _DispatchRecorder()
    out = _run2(tasks, tmp_path, prober, dispatch=disp)
    # the probe itself skips (fetch failed for the base check) -> no event
    # recorded as STALE -> no rebase. Either way: zero dispatch.
    assert out.dispatched == 0


# --- PR2 e2e through loop.tick --------------------------------------------


def _fake_acquire_factory(it_home: Path):
    """A deterministic stand-in for dispatch.acquire_coord_prompt_inbox:
    writes the inbox file (so the e2e assertions are real) WITHOUT shelling
    out to a real `fleet` binary. Returns the inbox path, mirroring the
    real helper's contract."""
    def _fake(agent_id, prompt, *, owner, dispatch_kind="worker",
             fleet_bin="fleet", fleet_home=None, **kw):
        inbox_dir = it_home / "inbox"
        inbox_dir.mkdir(parents=True, exist_ok=True)
        path = inbox_dir / f"{agent_id}.md"
        path.write_text(prompt)
        return str(path)
    return _fake


def test_tick_dispatches_fix_subagent_e2e(
    it_home: Path, it_project_dir: Path, monkeypatch,
) -> None:
    """End-to-end through loop.tick: a watched PR with a FAILING check ->
    the tick emits a DISPATCH block, writes the subagent inbox, and persists
    a §6 running lease in pr-watches.json. No real gh/git/fleet binary."""
    _write_tasks(it_project_dir, [_it_task("foo", pr_url=_pr_url(195), branch="worker/foo")])

    snaps = {195: pw.PRSnapshot(number=195, pr_state="OPEN",
                                merge_state_status="UNSTABLE", checks="FAILURE",
                                review_decision="", head_ref_oid="H195",
                                base_ref_name="main")}
    prober = FakeProber(snaps=snaps, fresh_base="B", ancestors=set())
    monkeypatch.setattr(loop, "_pr_watch_prober", prober)
    monkeypatch.setattr(loop.pr_watch_mod, "derive_owner_repo", lambda *a, **k: OWNER_REPO)
    monkeypatch.setattr(loop.dispatch_mod, "fetch_standards", lambda *a, **k: "STANDARDS")
    monkeypatch.setattr(loop.dispatch_mod, "acquire_coord_prompt_inbox",
                        _fake_acquire_factory(it_home))

    with patch.object(loop, "_run_fleet", side_effect=lambda cmd, timeout_s=30.0: None):
        result = loop.tick("fleet", coord_id="cccccc01", cwd="/repo",
                           fleet_home=str(it_home))

    assert not result.skipped
    # a DISPATCH block was emitted for the fix subagent + counted.
    blocks = [b for b in result.dispatch_instructions if "pr-fix-195" in b]
    assert len(blocks) == 1
    assert result.dispatched >= 1
    # the lease is persisted (running) with the minted agent_id.
    w = pw.load_watches(it_project_dir)["watches"]["195"]
    assert w["inflight_action"]["kind"] == pw.ACTION_FIX
    assert w["inflight_action"]["outcome"] == pw.OUTCOME_RUNNING
    agent_id = w["inflight_action"]["agent_id"]
    assert agent_id
    # the subagent inbox prompt was written.
    inbox = it_home / "inbox" / f"{agent_id}.md"
    assert inbox.exists()
    assert "FIX subagent for PR #195" in inbox.read_text()


def test_tick_idempotent_no_duplicate_dispatch_e2e(
    it_home: Path, it_project_dir: Path, monkeypatch,
) -> None:
    """Two consecutive ticks on the SAME failing head + a still-alive agent
    -> exactly ONE dispatch (the lease suppresses the second). Simulates the
    agent record so _agent_outcome reports running."""
    _write_tasks(it_project_dir, [_it_task("foo", pr_url=_pr_url(195), branch="worker/foo")])
    snaps = {195: pw.PRSnapshot(number=195, pr_state="OPEN",
                                merge_state_status="UNSTABLE", checks="FAILURE",
                                review_decision="", head_ref_oid="H195",
                                base_ref_name="main")}
    prober = FakeProber(snaps=snaps, fresh_base="B", ancestors=set())
    monkeypatch.setattr(loop, "_pr_watch_prober", prober)
    monkeypatch.setattr(loop.pr_watch_mod, "derive_owner_repo", lambda *a, **k: OWNER_REPO)
    monkeypatch.setattr(loop.dispatch_mod, "fetch_standards", lambda *a, **k: "S")
    monkeypatch.setattr(loop.dispatch_mod, "acquire_coord_prompt_inbox",
                        _fake_acquire_factory(it_home))

    with patch.object(loop, "_run_fleet", side_effect=lambda cmd, timeout_s=30.0: None):
        r1 = loop.tick("fleet", coord_id="cccccc01", cwd="/repo", fleet_home=str(it_home))
    agent_id = pw.load_watches(it_project_dir)["watches"]["195"]["inflight_action"]["agent_id"]
    n1 = len([b for b in r1.dispatch_instructions if "pr-fix-195" in b])
    assert n1 == 1

    # Write a live agent record so _agent_outcome reports "running".
    (it_home / "agents").mkdir(exist_ok=True)
    import os as _os
    (it_home / "agents" / f"{agent_id}.json").write_text(
        json.dumps({"id": agent_id, "pid": _os.getpid(), "blocked": False})
    )
    with patch.object(loop, "_run_fleet", side_effect=lambda cmd, timeout_s=30.0: None):
        r2 = loop.tick("fleet", coord_id="cccccc01", cwd="/repo", fleet_home=str(it_home))
    n2 = len([b for b in r2.dispatch_instructions if "pr-fix-195" in b])
    assert n2 == 0  # suppressed by the running lease
