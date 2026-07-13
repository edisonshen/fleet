"""T5 + T6 — worktree-reaping consumer (DESIGN-coord-worktree-lifecycle §4).

The reaper is a CONSUMER of the generation chokepoint: it only removes a
worktree when the current-generation worker is terminal + clean, and it
parks (never blows away) a dirty tree. These tests are deliberately
FAILS-ON-PARENT for the dirty path — the pre-§4 code defaulted
`remove_worktree(force=True)`, which would delete a dirty tree; here we
assert the tree is NOT removed and the worker dir is KEPT.

  T5 — worktree-reaping (clean→removed / dirty→parked, branch-identity,
       TOCTOU, caller-specific park, tree-left keeps worker dir, dispatch
       preflight refuses).
  T6 — cap-independent reap (inherited worktree on a cap==1 coord still
       reaps; genuinely worktree-free task is a no-op).

worktree.py git calls are mocked via subprocess.run so the suite stays
fast + git-free; loop._run_fleet is captured so the emitted tasks.md
mutations are asserted directly.
"""
from __future__ import annotations

import datetime as _dt
import subprocess
from unittest.mock import patch

import dispatch as dispatch_mod
import loop
import parse
import worktree as worktree_mod


# ---------- helpers ----------


def _ok(stdout: str = "", stderr: str = "") -> subprocess.CompletedProcess:
    return subprocess.CompletedProcess(args=[], returncode=0, stdout=stdout, stderr=stderr)


def _err(stderr: str, returncode: int = 1) -> subprocess.CompletedProcess:
    return subprocess.CompletedProcess(args=[], returncode=returncode, stdout="", stderr=stderr)


def _task(slug, *, status="in-progress", worktree="", branch="", dispatch_generation=1):
    return parse.Task(
        slug=slug, status=status, priority="P1",
        created=_dt.datetime(2026, 6, 3, 10, 0, 0, tzinfo=_dt.timezone.utc),
        updated=_dt.datetime(2026, 6, 3, 10, 0, 0, tzinfo=_dt.timezone.utc),
        spawned_by="user", spec="spec", acceptance="acc",
        worktree=worktree, branch=branch, dispatch_generation=dispatch_generation,
    )


def _capture_fleet():
    """Return (calls, patcher) capturing loop._run_fleet argv."""
    calls: list[list[str]] = []
    patcher = patch.object(
        loop, "_run_fleet", side_effect=lambda cmd, timeout_s=30.0: calls.append(list(cmd)),
    )
    return calls, patcher


def _set_calls(calls):
    return [c for c in calls if c[1:3] == ["tasks", "set"]]


def _has_set(calls, slug, fragment):
    return any(
        c[1:3] == ["tasks", "set"] and slug in c and any(fragment in p for p in c)
        for c in calls
    )


# ======================================================================
# T5 — worktree-reaping (the consumer)
# ======================================================================


class TestMaybeRemoveWorktree:
    """_maybe_remove_worktree outcome contract (§4.1)."""

    def test_clean_terminal_tree_removed_force_false(self, tmp_path):
        # FAILS-ON-PARENT only for dirty; here clean → removed via the
        # NEW force=False path (porcelain returns empty).
        wt = str(tmp_path / "projects" / "p" / "worktrees" / "feat-aaaa")
        (tmp_path / "projects" / "p" / "worktrees" / "feat-aaaa").mkdir(parents=True)
        t = _task("feat-aaaa", worktree=wt, branch="worker/feat-aaaa")
        calls, patcher = _capture_fleet()

        def fake_run(cmd, **kw):
            # branch-identity probe → expected branch
            if cmd[:2] == ["git", "-C"] and "rev-parse" in cmd and "--abbrev-ref" in cmd:
                return _ok("worker/feat-aaaa\n")
            if "status" in cmd and "--porcelain" in cmd:
                return _ok("")  # clean
            if cmd[3:5] == ["worktree", "remove"]:
                # force=False must NOT pass --force
                assert "--force" not in cmd, "reap must use force=False"
                return _ok()
            return _ok()

        with patcher, patch.object(worktree_mod.subprocess, "run", side_effect=fake_run):
            outcome = loop._maybe_remove_worktree(
                "feat-aaaa", str(tmp_path / "repo"), {"feat-aaaa": t}, "fleet", "p",
            )
        assert outcome == worktree_mod.OUTCOME_REMOVED
        assert _has_set(calls, "feat-aaaa", "worktree=")

    def test_dirty_tree_parked_not_removed(self, tmp_path):
        # FAILS-ON-PARENT: parent force=True would delete this tree. The
        # §4.1 dirty-guard must return DIRTY_PARKED + NOT call remove.
        wt = str(tmp_path / "projects" / "p" / "worktrees" / "feat-bbbb")
        (tmp_path / "projects" / "p" / "worktrees" / "feat-bbbb").mkdir(parents=True)
        t = _task("feat-bbbb", worktree=wt, branch="worker/feat-bbbb")
        removed = {"called": False}

        def fake_run(cmd, **kw):
            if "rev-parse" in cmd and "--abbrev-ref" in cmd:
                return _ok("worker/feat-bbbb\n")
            if "status" in cmd and "--porcelain" in cmd:
                return _ok(" M src/foo.py\n")  # DIRTY
            if cmd[3:5] == ["worktree", "remove"]:
                removed["called"] = True
                return _ok()
            return _ok()

        calls, patcher = _capture_fleet()
        with patcher, patch.object(worktree_mod.subprocess, "run", side_effect=fake_run):
            outcome = loop._maybe_remove_worktree(
                "feat-bbbb", str(tmp_path / "repo"), {"feat-bbbb": t}, "fleet", "p",
            )
        assert outcome == worktree_mod.OUTCOME_DIRTY_PARKED
        assert removed["called"] is False, "dirty tree must NOT be removed (fails-on-parent)"
        assert not _has_set(calls, "feat-bbbb", "worktree="), "worktree= must NOT be cleared on park"

    def test_wrong_branch_clean_tree_skipped(self, tmp_path):
        # A CLEAN checkout of the WRONG branch must be SKIPPED (force=False
        # does NOT protect a clean tree). → OUTCOME_ERROR, never removed.
        wt = str(tmp_path / "projects" / "p" / "worktrees" / "feat-cccc")
        (tmp_path / "projects" / "p" / "worktrees" / "feat-cccc").mkdir(parents=True)
        t = _task("feat-cccc", worktree=wt, branch="worker/feat-cccc")
        removed = {"called": False}

        def fake_run(cmd, **kw):
            if "rev-parse" in cmd and "--abbrev-ref" in cmd:
                return _ok("main\n")  # WRONG branch
            if cmd[3:5] == ["worktree", "remove"]:
                removed["called"] = True
                return _ok()
            return _ok()

        with patch.object(worktree_mod.subprocess, "run", side_effect=fake_run):
            outcome = loop._maybe_remove_worktree(
                "feat-cccc", str(tmp_path / "repo"), {"feat-cccc": t}, "fleet", "p",
            )
        assert outcome == worktree_mod.OUTCOME_ERROR
        assert removed["called"] is False, "wrong-branch clean tree must NOT be removed"

    def test_branch_empty_uses_deterministic_worker_slug(self, tmp_path):
        # branch=="" → expected branch is the deterministic worker/<slug>.
        wt = str(tmp_path / "projects" / "p" / "worktrees" / "feat-dddd")
        (tmp_path / "projects" / "p" / "worktrees" / "feat-dddd").mkdir(parents=True)
        t = _task("feat-dddd", worktree=wt, branch="")

        def fake_run(cmd, **kw):
            if "rev-parse" in cmd and "--abbrev-ref" in cmd:
                return _ok("worker/feat-dddd\n")
            if "status" in cmd and "--porcelain" in cmd:
                return _ok("")
            if cmd[3:5] == ["worktree", "remove"]:
                return _ok()
            return _ok()

        calls, patcher = _capture_fleet()
        with patcher, patch.object(worktree_mod.subprocess, "run", side_effect=fake_run):
            outcome = loop._maybe_remove_worktree(
                "feat-dddd", str(tmp_path / "repo"), {"feat-dddd": t}, "fleet", "p",
            )
        assert outcome == worktree_mod.OUTCOME_REMOVED

    def test_toctou_dirty_classified_dirty_parked_not_error(self, tmp_path):
        # Clean at porcelain-check, then git refuses removal (turned dirty)
        # → DIRTY_PARKED, NOT generic error (§4.1).
        wt = str(tmp_path / "projects" / "p" / "worktrees" / "feat-eeee")
        (tmp_path / "projects" / "p" / "worktrees" / "feat-eeee").mkdir(parents=True)
        t = _task("feat-eeee", worktree=wt, branch="worker/feat-eeee")

        def fake_run(cmd, **kw):
            if "rev-parse" in cmd and "--abbrev-ref" in cmd:
                return _ok("worker/feat-eeee\n")
            if "status" in cmd and "--porcelain" in cmd:
                return _ok("")  # clean at check time
            if cmd[3:5] == ["worktree", "remove"]:
                return _err("fatal: 'x' contains modified or untracked files, use --force to delete it")
            return _ok()

        with patch.object(worktree_mod.subprocess, "run", side_effect=fake_run):
            outcome = loop._maybe_remove_worktree(
                "feat-eeee", str(tmp_path / "repo"), {"feat-eeee": t}, "fleet", "p",
            )
        assert outcome == worktree_mod.OUTCOME_DIRTY_PARKED

    def test_noop_already_gone_clears_stale_worktree_field(self, tmp_path):
        # codex [P2]: task carries worktree= but the dir is already gone →
        # remove_worktree returns NOOP; we MUST still clear worktree= so the
        # row doesn't permanently point at a missing checkout.
        gone = str(tmp_path / "projects" / "p" / "worktrees" / "gone-zzzz")
        # Do NOT create the dir → already-gone.
        t = _task("gone-zzzz", worktree=gone, branch="worker/gone-zzzz")
        calls, patcher = _capture_fleet()

        def fake_run(cmd, **kw):
            # remove_worktree's ENOENT path → prune only, returns NOOP.
            return _ok()

        with patcher, patch.object(worktree_mod.subprocess, "run", side_effect=fake_run):
            outcome = loop._maybe_remove_worktree(
                "gone-zzzz", str(tmp_path / "repo"), {"gone-zzzz": t}, "fleet", "p",
            )
        assert outcome == worktree_mod.OUTCOME_NOOP
        assert _has_set(calls, "gone-zzzz", "worktree="), "stale worktree= must be cleared on NOOP"

    def test_per_task_no_worktree_is_noop(self, tmp_path):
        # §4.3 per-task guard: a task with no worktree= → NOOP even when
        # the project has other worktrees.
        t = _task("feat-ffff", worktree="")
        with patch.object(worktree_mod.subprocess, "run", side_effect=AssertionError("git must not run")):
            outcome = loop._maybe_remove_worktree(
                "feat-ffff", str(tmp_path / "repo"), {"feat-ffff": t}, "fleet", "p",
            )
        assert outcome == worktree_mod.OUTCOME_NOOP


class TestApplySentinelPark:
    """Caller-specific PARK on _apply_sentinel (§4.2)."""

    def test_task_done_pr_dirty_preserves_in_review_keeps_worker_dir(self):
        calls, patcher = _capture_fleet()
        action = loop._SentinelAction(
            slug="ship-aaaa", kind="task_done_pr",
            payload="https://example.com/pr/1", dispatch_generation=1,
        )
        t = _task("ship-aaaa", status="in-review", worktree="/wt/ship-aaaa", dispatch_generation=1)
        deleted = {"called": False}
        with patcher, \
             patch.object(loop, "_maybe_remove_worktree", return_value=worktree_mod.OUTCOME_DIRTY_PARKED), \
             patch.object(loop, "_maybe_delete_worker_dir", side_effect=lambda *a, **k: deleted.__setitem__("called", True)), \
             patch.object(loop, "_task_row_dispatch_generation", return_value=1):
            outcome = loop._apply_sentinel(
                action, "p", "fleet",
                repo="/repo", tasks_by_slug={"ship-aaaa": t}, full_tasks_by_slug={"ship-aaaa": t},
            )
        assert outcome == loop.SENTINEL_APPLIED
        # in-review preserved (NOT flipped to blocked); parked set; worker dir KEPT.
        assert _has_set(calls, "ship-aaaa", "status=in-review")
        assert not _has_set(calls, "ship-aaaa", "status=blocked")
        assert _has_set(calls, "ship-aaaa", "parked=")
        assert deleted["called"] is False, "tree-left → worker dir KEPT"

    def test_task_done_pr_clean_deletes_worker_dir_no_park(self):
        calls, patcher = _capture_fleet()
        action = loop._SentinelAction(
            slug="ship-bbbb", kind="task_done_pr",
            payload="https://example.com/pr/2", dispatch_generation=1,
        )
        t = _task("ship-bbbb", status="in-review", worktree="/wt/ship-bbbb", dispatch_generation=1)
        deleted = {"called": False}
        with patcher, \
             patch.object(loop, "_maybe_remove_worktree", return_value=worktree_mod.OUTCOME_REMOVED), \
             patch.object(loop, "_maybe_delete_worker_dir", side_effect=lambda *a, **k: deleted.__setitem__("called", True)), \
             patch.object(loop, "_task_row_dispatch_generation", return_value=1):
            loop._apply_sentinel(
                action, "p", "fleet",
                repo="/repo", tasks_by_slug={"ship-bbbb": t}, full_tasks_by_slug={"ship-bbbb": t},
            )
        assert not _has_set(calls, "ship-bbbb", "parked=")
        assert deleted["called"] is True, "clean reap → worker dir deleted"

    def test_worker_failed_dirty_flips_blocked_keeps_worker_dir(self):
        calls, patcher = _capture_fleet()
        action = loop._SentinelAction(
            slug="fail-cccc", kind="worker_failed", payload="boom", dispatch_generation=1,
        )
        t = _task("fail-cccc", status="in-progress", worktree="/wt/fail-cccc", dispatch_generation=1)
        deleted = {"called": False}
        with patcher, \
             patch.object(loop, "_maybe_remove_worktree", return_value=worktree_mod.OUTCOME_DIRTY_PARKED), \
             patch.object(loop, "_maybe_delete_worker_dir", side_effect=lambda *a, **k: deleted.__setitem__("called", True)), \
             patch.object(loop, "_task_row_dispatch_generation", return_value=1):
            loop._apply_sentinel(
                action, "p", "fleet",
                repo="/repo", tasks_by_slug={"fail-cccc": t}, full_tasks_by_slug={"fail-cccc": t},
            )
        # worker_failed is redispatch-eligible → MUST flip to blocked so a
        # re-dispatch can't reuse the dirty tree. parked set; worker dir KEPT.
        assert _has_set(calls, "fail-cccc", "status=blocked")
        assert _has_set(calls, "fail-cccc", "parked=")
        assert deleted["called"] is False


class TestApplyReconcilePark:
    """Caller-specific PARK on _apply_reconcile (§4.2)."""

    def test_reconcile_done_dirty_preserves_done_keeps_worker_dir(self):
        calls, patcher = _capture_fleet()
        action = loop._ReconcileAction(
            slug="merged-aaaa", new_status="done", delete_worker_dir=True,
        )
        deleted = {"called": False}
        with patcher, \
             patch.object(loop, "_maybe_remove_worktree", return_value=worktree_mod.OUTCOME_DIRTY_PARKED), \
             patch.object(loop, "_maybe_delete_worker_dir", side_effect=lambda *a, **k: deleted.__setitem__("called", True)):
            loop._apply_reconcile(
                action, "p", "fleet", repo="/repo",
                tasks_by_slug={"merged-aaaa": _task("merged-aaaa", worktree="/wt/m")},
            )
        # done is NOT redispatch-eligible → NOT flipped to blocked; parked
        # set; worker dir KEPT (tree-left).
        assert not _has_set(calls, "merged-aaaa", "status=blocked")
        assert _has_set(calls, "merged-aaaa", "parked=")
        assert deleted["called"] is False

    def test_reconcile_todo_dirty_flips_blocked(self):
        calls, patcher = _capture_fleet()
        action = loop._ReconcileAction(
            slug="died-bbbb", new_status="todo", delete_worker_dir=True,
        )
        with patcher, \
             patch.object(loop, "_maybe_remove_worktree", return_value=worktree_mod.OUTCOME_DIRTY_PARKED), \
             patch.object(loop, "_maybe_delete_worker_dir"):
            loop._apply_reconcile(
                action, "p", "fleet", repo="/repo",
                tasks_by_slug={"died-bbbb": _task("died-bbbb", worktree="/wt/d")},
            )
        # todo is redispatch-eligible → flip blocked + park.
        assert _has_set(calls, "died-bbbb", "status=blocked")
        assert _has_set(calls, "died-bbbb", "parked=")


class TestDispatchPreflight:
    """Dispatch preflight on the COMPUTED path (§4.2)."""

    def test_no_tree_at_computed_path_ok(self, tmp_path):
        ok, why = dispatch_mod.worktree_preflight_ok(
            str(tmp_path / "nope"), "worker/feat-aaaa",
        )
        assert ok and why == ""

    def test_wrong_branch_tree_refuses(self, tmp_path):
        d = tmp_path / "wt"
        d.mkdir()

        def fake_run(cmd, **kw):
            if "rev-parse" in cmd and "--abbrev-ref" in cmd:
                return _ok("main\n")
            return _ok("")

        with patch.object(dispatch_mod.subprocess, "run", side_effect=fake_run):
            ok, why = dispatch_mod.worktree_preflight_ok(str(d), "worker/feat-aaaa")
        assert not ok
        assert "branch" in why

    def test_dirty_tree_refuses(self, tmp_path):
        d = tmp_path / "wt"
        d.mkdir()

        def fake_run(cmd, **kw):
            if "rev-parse" in cmd and "--abbrev-ref" in cmd:
                return _ok("worker/feat-aaaa\n")
            if "status" in cmd and "--porcelain" in cmd:
                return _ok(" M x\n")
            return _ok("")

        with patch.object(dispatch_mod.subprocess, "run", side_effect=fake_run):
            ok, why = dispatch_mod.worktree_preflight_ok(str(d), "worker/feat-aaaa")
        assert not ok
        assert "uncommitted" in why

    def test_right_branch_clean_tree_ok(self, tmp_path):
        d = tmp_path / "wt"
        d.mkdir()

        def fake_run(cmd, **kw):
            if "rev-parse" in cmd and "--abbrev-ref" in cmd:
                return _ok("worker/feat-aaaa\n")
            if "status" in cmd and "--porcelain" in cmd:
                return _ok("")
            return _ok("")

        with patch.object(dispatch_mod.subprocess, "run", side_effect=fake_run):
            ok, why = dispatch_mod.worktree_preflight_ok(str(d), "worker/feat-aaaa")
        assert ok and why == ""


# ======================================================================
# T6 — cap-independent reap
# ======================================================================


class TestWorktreeCleanupContext:
    def test_inherited_worktree_reaped_at_cap1(self):
        # A task carrying worktree= (inherited from an earlier cap>1 run)
        # → context returns (cwd, map) regardless of cap.
        tasks = [_task("inh-aaaa", worktree="/wt/inh-aaaa")]
        repo, tbs = loop._worktree_cleanup_context("/repo", tasks)
        assert repo == "/repo"
        assert tbs is not None and "inh-aaaa" in tbs

    def test_no_worktree_anywhere_is_noop_context(self):
        tasks = [_task("plain-bbbb", worktree="")]
        repo, tbs = loop._worktree_cleanup_context("/repo", tasks)
        assert repo == "" and tbs is None

    def test_mixed_project_returns_full_map(self):
        # ONE task with a worktree → the WHOLE project gets the context
        # (per-task guard inside _maybe_remove_worktree keeps the
        # worktree-free task safe).
        tasks = [_task("a", worktree="/wt/a"), _task("b", worktree="")]
        repo, tbs = loop._worktree_cleanup_context("/repo", tasks)
        assert repo == "/repo"
        assert set(tbs.keys()) == {"a", "b"}


class TestSweepDoneDirtyGuard:
    """codex round-4 [P2]: the done-dir sweep must NOT erase a worker dir
    whose `done` task still has a dirty worktree on disk (the operator-
    manual-`done` window, before the every-N-ticks Go backstop sees it)."""

    def test_sweep_keeps_dir_when_worktree_dirty(self, tmp_path):
        home = tmp_path
        wdir = home / "projects" / "p" / "workers" / "manual-aaaa"
        wdir.mkdir(parents=True)
        (wdir / "state.json").write_text("{}", encoding="utf-8")
        wt = home / "projects" / "p" / "worktrees" / "manual-aaaa"
        wt.mkdir(parents=True)
        t = _task("manual-aaaa", status="done", worktree=str(wt), branch="worker/manual-aaaa")
        deleted = {"called": False}
        with patch.object(worktree_mod, "worktree_is_dirty", return_value=(True, "")), \
             patch.object(loop, "_maybe_delete_worker_dir",
                          side_effect=lambda *a, **k: deleted.__setitem__("called", True)):
            swept = loop._sweep_done_worker_dirs([t], "p", "fleet", home=home)
        assert deleted["called"] is False, "dirty done worktree → worker dir KEPT"
        assert swept == 0

    def test_sweep_deletes_dir_when_worktree_clean(self, tmp_path):
        home = tmp_path
        wdir = home / "projects" / "p" / "workers" / "clean-bbbb"
        wdir.mkdir(parents=True)
        (wdir / "state.json").write_text("{}", encoding="utf-8")
        wt = home / "projects" / "p" / "worktrees" / "clean-bbbb"
        wt.mkdir(parents=True)
        t = _task("clean-bbbb", status="done", worktree=str(wt), branch="worker/clean-bbbb")
        deleted = {"called": False}
        with patch.object(worktree_mod, "worktree_is_dirty", return_value=(False, "")), \
             patch.object(loop, "_maybe_delete_worker_dir",
                          side_effect=lambda *a, **k: deleted.__setitem__("called", True)):
            loop._sweep_done_worker_dirs([t], "p", "fleet", home=home)
        assert deleted["called"] is True, "clean done worktree → worker dir swept"


class TestDispatchClearsParked:
    """codex [P2]: (re-)dispatch must clear a resolved `parked` marker so
    the next completion's worker-dir sweep / Go GC don't hard-keep it."""

    def test_apply_dispatch_clears_parked(self):
        calls, patcher = _capture_fleet()
        action = loop._DispatchAction(
            slug="resolved-aaaa", agent_id="aaaaaaaa",
            branch="worker/resolved-aaaa", dispatch_instruction="DISPATCH ...",
            dispatch_generation=2,
        )
        with patcher:
            loop._apply_dispatch(action, "p", "fleet")
        # The dispatch must emit a `parked=` clear (resolves any prior park).
        assert _has_set(calls, "resolved-aaaa", "parked=")
        # And it lands AFTER the status flip (status=in-progress is first).
        order = [c for c in calls if c[1:3] == ["tasks", "set"]]
        i_status = next(i for i, c in enumerate(order) if any("status=in-progress" in p for p in c))
        i_parked = next(i for i, c in enumerate(order) if any(p == "parked=" for p in c))
        assert i_parked > i_status


class _Result:
    def __init__(self):
        self.errors: list[str] = []


class TestWorktreeGCCadence:
    """_maybe_gc_worktrees bounded cadence + fail-soft (§4.4)."""

    def test_runs_on_cadence_tick(self, monkeypatch):
        monkeypatch.setenv("FLEET_WORKTREE_GC_EVERY", "20")
        res = _Result()
        with patch.object(loop.subprocess, "run", return_value=_ok()) as run:
            ran = loop._maybe_gc_worktrees(
                {"tick_count": 20}, project="p", cwd="/repo", cap=2,
                fleet_bin="fleet", result=res,
            )
        assert ran is True
        argv = run.call_args.args[0]
        assert argv[:2] == ["fleet", "gc"]
        assert "--kinds=worktrees" in argv and "--apply" in argv
        assert "--project" in argv and "p" in argv
        assert res.errors == []

    def test_skips_off_cadence_tick(self, monkeypatch):
        monkeypatch.setenv("FLEET_WORKTREE_GC_EVERY", "20")
        res = _Result()
        with patch.object(loop.subprocess, "run", side_effect=AssertionError("must not shell out")):
            ran = loop._maybe_gc_worktrees(
                {"tick_count": 7}, project="p", cwd="/repo", cap=2,
                fleet_bin="fleet", result=res,
            )
        assert ran is False

    def test_disabled_when_every_zero(self, monkeypatch):
        monkeypatch.setenv("FLEET_WORKTREE_GC_EVERY", "0")
        res = _Result()
        with patch.object(loop.subprocess, "run", side_effect=AssertionError("must not shell out")):
            ran = loop._maybe_gc_worktrees(
                {"tick_count": 40}, project="p", cwd="/repo", cap=2,
                fleet_bin="fleet", result=res,
            )
        assert ran is False

    def test_nonzero_exit_failsoft(self, monkeypatch):
        monkeypatch.setenv("FLEET_WORKTREE_GC_EVERY", "20")
        res = _Result()
        with patch.object(loop.subprocess, "run", return_value=_err("boom")):
            ran = loop._maybe_gc_worktrees(
                {"tick_count": 20}, project="p", cwd="/repo", cap=2,
                fleet_bin="fleet", result=res,
            )
        assert ran is True
        assert any("worktree-gc backstop nonzero exit" in e for e in res.errors)

    def test_timeout_failsoft(self, monkeypatch):
        monkeypatch.setenv("FLEET_WORKTREE_GC_EVERY", "20")
        res = _Result()
        with patch.object(loop.subprocess, "run", side_effect=subprocess.TimeoutExpired("fleet", 120.0)):
            ran = loop._maybe_gc_worktrees(
                {"tick_count": 20}, project="p", cwd="/repo", cap=2,
                fleet_bin="fleet", result=res,
            )
        assert ran is True
        assert any("worktree-gc backstop" in e for e in res.errors)


class TestOrphanAgentsGCCadence:
    """_maybe_gc_orphan_agents bounded cadence + coord-scope-strict + fail-soft
    (Slice B safe auto-reap)."""

    def test_runs_on_cadence_tick_scoped_to_project(self, monkeypatch):
        # B4: the coord-tick reap is --project-scoped (project X only) and
        # targets the orphan-agents kind.
        monkeypatch.setenv("FLEET_ORPHAN_AGENTS_GC_EVERY", "20")
        res = _Result()
        with patch.object(loop.subprocess, "run", return_value=_ok()) as run:
            ran = loop._maybe_gc_orphan_agents(
                {"tick_count": 20}, project="X", fleet_bin="fleet", result=res,
            )
        assert ran is True
        argv = run.call_args.args[0]
        assert argv[:2] == ["fleet", "gc"]
        assert "--apply" in argv and "--kinds=orphan-agents" in argv
        # Coord-scope strict: exactly --project X, no other project.
        assert argv[argv.index("--project") + 1] == "X"
        assert res.errors == []

    def test_skips_off_cadence_tick(self, monkeypatch):
        monkeypatch.setenv("FLEET_ORPHAN_AGENTS_GC_EVERY", "20")
        res = _Result()
        with patch.object(loop.subprocess, "run", side_effect=AssertionError("must not shell out")):
            ran = loop._maybe_gc_orphan_agents(
                {"tick_count": 7}, project="X", fleet_bin="fleet", result=res,
            )
        assert ran is False

    def test_disabled_when_every_zero(self, monkeypatch):
        monkeypatch.setenv("FLEET_ORPHAN_AGENTS_GC_EVERY", "0")
        res = _Result()
        with patch.object(loop.subprocess, "run", side_effect=AssertionError("must not shell out")):
            ran = loop._maybe_gc_orphan_agents(
                {"tick_count": 40}, project="X", fleet_bin="fleet", result=res,
            )
        assert ran is False

    def test_nonzero_exit_failsoft(self, monkeypatch):
        monkeypatch.setenv("FLEET_ORPHAN_AGENTS_GC_EVERY", "20")
        res = _Result()
        with patch.object(loop.subprocess, "run", return_value=_err("boom")):
            ran = loop._maybe_gc_orphan_agents(
                {"tick_count": 20}, project="X", fleet_bin="fleet", result=res,
            )
        assert ran is True
        assert any("orphan-agents-gc backstop nonzero exit" in e for e in res.errors)

    def test_timeout_failsoft(self, monkeypatch):
        monkeypatch.setenv("FLEET_ORPHAN_AGENTS_GC_EVERY", "20")
        res = _Result()
        with patch.object(loop.subprocess, "run", side_effect=subprocess.TimeoutExpired("fleet", 120.0)):
            ran = loop._maybe_gc_orphan_agents(
                {"tick_count": 20}, project="X", fleet_bin="fleet", result=res,
            )
        assert ran is True
        assert any("orphan-agents-gc backstop" in e for e in res.errors)
