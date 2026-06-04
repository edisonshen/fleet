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
          branch: str = "") -> parse.Task:
    return parse.Task(slug=slug, status=status, priority="P1",
                      pr_url=pr_url, branch=branch)


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
                 base_shas=None, ancestors=None):
        self.snaps = snaps or {}
        self.repo_error = repo_error
        self.fresh_base = fresh_base
        self.base_shas = base_shas or {}
        self.ancestors = ancestors or set()
        self.probe_calls = []          # list of (pr_numbers tuple)
        self.ancestor_calls = []

    def probe_repo(self, repo_path, owner_repo, base_ref, pr_numbers, head_oids):
        self.probe_calls.append(tuple(pr_numbers))
        rp = pw.RepoProbe(
            fresh_base_sha=self.fresh_base,
            fresh_base_shas=dict(self.base_shas),
            fetch_ok=bool(self.fresh_base),
        )
        if self.repo_error:
            rp.error = self.repo_error
            return rp
        for n in pr_numbers:
            if n in self.snaps:
                rp.snapshots[n] = self.snaps[n]
        return rp

    def is_ancestor(self, repo_path, ancestor_sha, descendant_sha):
        self.ancestor_calls.append((ancestor_sha, descendant_sha))
        return (ancestor_sha, descendant_sha) in self.ancestors


def _run(tasks, project_dir, prober, *, owner_repo=OWNER_REPO, now="2026-06-03T08:00:00Z",
         tick_count=1, slow=5, flips=None):
    """Run reconcile_watches with a recording flip callback. Returns the
    WatchOutcome; appends flipped slugs to `flips` (if provided)."""
    def _flip(slug):
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
    assert pw.parse_pr_url("") is None
    assert pw.parse_pr_url("https://example.com/no-pr") is None


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

    def _boom(slug):
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
    # second tick: closed watch is terminal -> not re-probed, not re-raised.
    out2 = _run(tasks, tmp_path, FakeProber(snaps=snaps), tick_count=2)
    assert not any("CLOSED without merging" in r for r in out2.raises)


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
    pending -> READY surfaced."""
    tasks = [_task("foo", pr_url=_pr_url(200))]
    snaps = {200: pw.PRSnapshot(number=200, pr_state="OPEN",
                                merge_state_status="BLOCKED", checks="SUCCESS",
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
                                merge_state_status="BLOCKED", checks="SUCCESS",
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
    """A failed fetch (empty fresh_base) -> mergeability UNKNOWN -> keep
    watching (EVENT_OPEN), never assert READY (fail-soft, §3)."""
    tasks = [_task("foo", pr_url=_pr_url(201))]
    snaps = {201: pw.PRSnapshot(number=201, pr_state="OPEN", checks="SUCCESS",
                                review_decision="APPROVED", head_ref_oid="H")}
    prober = FakeProber(snaps=snaps, fresh_base="")  # fetch failed
    _run(tasks, tmp_path, prober)
    w = pw.load_watches(tmp_path)["watches"]["201"]
    assert w["last_event"] == pw.EVENT_OPEN          # NOT ready
    assert w["last_snapshot"]["up_to_date"] is False


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


def test_definitive_404_raises_hand(tmp_path: Path) -> None:
    """A definitive 404 (PR genuinely gone) -> raise-hand pr-not-found."""
    tasks = [_task("foo", pr_url=_pr_url(195))]
    snaps = {195: pw.PRSnapshot(number=195, error="HTTP 404 Not Found",
                                not_found=True)}
    out = _run(tasks, tmp_path, FakeProber(snaps=snaps))
    assert any("pr-not-found" in r for r in out.raises)
    # watch retained (not silently dropped) — operator must act.
    assert "195" in pw.load_watches(tmp_path)["watches"]


def test_missing_from_repo_query_raises_not_found(tmp_path: Path) -> None:
    """Repo query SUCCEEDED but the PR is absent -> 404-class raise-hand."""
    tasks = [_task("foo", pr_url=_pr_url(195))]
    out = _run(tasks, tmp_path, FakeProber(snaps={}))   # repo ok, no PR
    assert any("pr-not-found" in r for r in out.raises)


def test_classify_probe_error() -> None:
    assert pw.classify_probe_error("HTTP 404") == pw.EVENT_NOT_FOUND
    assert pw.classify_probe_error("Could not resolve to a PullRequest") == pw.EVENT_NOT_FOUND
    assert pw.classify_probe_error("HTTP 502 Bad Gateway") == pw.EVENT_SKIP
    assert pw.classify_probe_error("timeout") == pw.EVENT_SKIP
    assert pw.classify_probe_error("rate limit exceeded") == pw.EVENT_SKIP


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
    """A watch enrolled but never successfully probed-OPEN (e.g. its task
    was archived before any probe) must NOT orphan-raise — we can't assert
    the PR is OPEN. Regression for the E2E false-orphan."""
    # seed a watch with NO snapshot (never probed), empty tasks.
    doc = {"schema": 1, "watches": {"777": pw._new_watch(777, _pr_url(777), "b", "main")}}
    pw.save_watches(tmp_path, doc)
    out = _run([], tmp_path, FakeProber(), tick_count=2)
    w = pw.load_watches(tmp_path)["watches"]["777"]
    assert w["orphaned"] is False
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
        flip_task_done=lambda s: None, now_iso="t", tick_count=1, repo_path="/repo",
    )
    assert out.enrolled == 1
    assert prober.probe_calls == []                  # never probed
    assert "195" in pw.load_watches(tmp_path)["watches"]


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


def test_terminal_watch_not_probed(tmp_path: Path) -> None:
    """A closed-unmerged (terminal) watch is never re-probed."""
    doc = {"schema": 1, "watches": {"195": pw._new_watch(195, _pr_url(195), "b", "main")}}
    doc["watches"]["195"]["state"] = pw.STATE_CLOSED_UNMERGED
    pw.save_watches(tmp_path, doc)
    tasks = [_task("foo", pr_url=_pr_url(195))]
    prober = FakeProber()
    _run(tasks, tmp_path, prober, tick_count=3)
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


def test_derive_owner_repo_variants(monkeypatch) -> None:
    cases = {
        "https://github.com/edisonshen/fleet.git": "edisonshen/fleet",
        "https://github.com/edisonshen/fleet": "edisonshen/fleet",
        "git@github.com:edisonshen/fleet.git": "edisonshen/fleet",
        # dotted repo name must survive (codex iter-1 [P2]).
        "https://github.com/owner/foo.bar.git": "owner/foo.bar",
        "git@github.com:owner/foo.bar": "owner/foo.bar",
    }
    for url, expect in cases.items():
        monkeypatch.setattr(pw.subprocess, "run",
                            lambda *a, **k: _FakeProc(0, url + "\n"))
        assert pw.derive_owner_repo("/repo") == expect, url
    # non-github / failure -> None.
    monkeypatch.setattr(pw.subprocess, "run", lambda *a, **k: _FakeProc(0, "https://gitlab.com/a/b"))
    assert pw.derive_owner_repo("/repo") is None
    monkeypatch.setattr(pw.subprocess, "run", lambda *a, **k: _FakeProc(1, "", "no remote"))
    assert pw.derive_owner_repo("/repo") is None
    assert pw.derive_owner_repo("") is None


def test_ghgit_prober_fetches_current_head_then_base(monkeypatch) -> None:
    """The prober queries PRs FIRST (to learn the current head), then ONE
    fetch of the base ref + each non-local head, and rev-parses the base
    tracking ref (not FETCH_HEAD). Regression for codex iter-1 [P1]."""
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
    # graphql ran before fetch (current head learned first).
    gql_idx = next(i for i, c in enumerate(calls) if c[:3] == ["gh", "api", "graphql"])
    fetch_idx = next(i for i, c in enumerate(calls) if "fetch" in c)
    assert gql_idx < fetch_idx
    # the fetch included the base tracking refspec AND the non-local head.
    fetch_cmd = calls[fetch_idx]
    assert "refs/heads/main:refs/remotes/origin/main" in fetch_cmd
    assert "HEADSHA" in fetch_cmd
    # fresh base rev-parsed from the tracking ref, not FETCH_HEAD.
    rp_cmd = next(c for c in calls if "rev-parse" in c)
    assert "refs/remotes/origin/main" in rp_cmd
    assert rp.fresh_base_sha == "FRESHBASESHA"
    assert rp.fresh_base_shas.get("main") == "FRESHBASESHA"
    assert rp.snapshots[5].head_ref_oid == "HEADSHA"
    assert rp.snapshots[5].base_ref_name == "main"
    assert rp.snapshots[5].checks == "SUCCESS"


def test_ghgit_prober_null_pr_is_not_found(monkeypatch) -> None:
    resp = json.dumps({"data": {"repository": {"pr9": None}}})
    monkeypatch.setattr(pw.subprocess, "run",
                        lambda cmd, **k: _FakeProc(0, resp) if cmd[:3] == ["gh", "api", "graphql"] else _FakeProc(0))
    prober = pw.GhGitProber()
    rp = prober.probe_repo("/repo", OWNER_REPO, "main", [9], [])
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


def _it_task(slug: str, *, status="in-review", pr_url="", branch="") -> parse.Task:
    return parse.Task(
        slug=slug, status=status, priority="P1", pr_url=pr_url, branch=branch,
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
    # watch pruned.
    assert "195" not in pw.load_watches(it_project_dir)["watches"]


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
