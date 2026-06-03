"""PR3 enumeration tests (DESIGN-coord-worktree-lifecycle §3): route
EVERY mutation/reap/dispatch-driving worker-state reader/writer/sentinel/
deleter through the generation chokepoint PR2 built.

Coverage:
  T1 — per-reader: a STALE state drives NO mutation on R1-R6 specifically
       (R7 worktrees belt + R8 handoff reader are PR4 / already-PR2).
  T4 — sentinel: stale-token sentinel (S1/S2/S3) skips ALL terminal side
       effects even with an identical branch/path; caller gating (S4) at
       the three drain sites; S5 deferred→replay round-trips the token;
       tokenless-legacy rollout.
  D1 — _sweep_done_worker_dirs skips parked rows.

(T2 writer CAS + D2 Go worker-records parked-aware live Go-side.)
"""
from __future__ import annotations

import datetime as _dt
import json
from pathlib import Path
from unittest.mock import patch

import loop
import parse


# ---------- helpers ----------


def _write_worker_state(home: Path, project: str, slug: str, state: dict) -> None:
    d = home / "projects" / project / "workers" / slug
    d.mkdir(parents=True, exist_ok=True)
    (d / "state.json").write_text(json.dumps(state), encoding="utf-8")


def _make_task(
    slug: str,
    *,
    status: str = "in-progress",
    dispatch_generation: int = 0,
    parked: str = "",
    pr_url: str = "",
    worker_pid: int = 0,
    branch: str = "",
):
    return parse.Task(
        slug=slug, status=status, priority="P1",
        created=_dt.datetime(2026, 5, 6, 10, 0, 0, tzinfo=_dt.timezone.utc),
        updated=_dt.datetime(2026, 5, 6, 10, 0, 0, tzinfo=_dt.timezone.utc),
        spawned_by="user", spec="spec", acceptance="acc",
        dispatch_generation=dispatch_generation,
        parked=parked, pr_url=pr_url, worker_pid=worker_pid, branch=branch,
    )


# ======================================================================
# T1 — per-reader stale short-circuit (R1-R6).
#
# Each reader is exercised with a STALE terminal state.json (the worst
# case: a prior attempt's phase=done with a PR url) for a slug whose
# task-row authority has advanced. The assertion is per-path: that
# reader drives NO mutation (no status flip, clear_worker, worktree
# removal, worker-dir delete, nudge/escalate/block).
# ======================================================================


class TestR1toR4ReconcileStale:
    """R1 (alive-check fall-through), R2 (terminal-state), R3 (mid-phase),
    R4 (died-without-PR fall-through) all live inside _reconcile_inflight.
    A stale state must short-circuit the WHOLE decision tree for that slug
    — the highest-severity case is R4 (status=todo + clear_worker +
    delete_worker_dir on a stale state would remove the live tree)."""

    def _stale_terminal(self, home: Path, slug: str) -> None:
        # Prior attempt (gen 1) left a terminal phase=done with a PR.
        _write_worker_state(
            home, "p", slug,
            {
                "slug": slug, "phase": "done",
                "pr_url": "https://example.com/pr/STALE",
                "dispatch_generation": 1,
            },
        )

    def test_stale_done_state_drives_no_reconcile_action(self, tmp_path: Path) -> None:
        # Authority is gen 2 (re-dispatched); the on-disk state is gen 1.
        self._stale_terminal(tmp_path, "redisp-aaaa")
        t = _make_task("redisp-aaaa", status="in-progress", dispatch_generation=2)
        # Worker not OS-alive (pid 0) → reconcile reaches the chokepoint.
        # _gh_pr_checks must never be consulted (we short-circuit before
        # any pr_url branch); patch it to blow up if reached.
        with patch.object(loop, "_gh_pr_checks", side_effect=AssertionError("reached CI branch on stale")):
            actions = loop._reconcile_inflight(
                [t], "p", "fleet", home=tmp_path,
            )
        assert actions == [], "stale state must produce ZERO reconcile actions"

    def test_stale_in_review_state_drives_no_action(self, tmp_path: Path) -> None:
        # The in-review path (R2/R4 via pr_url+CI) must also short-circuit
        # on a stale state — a stale done would otherwise re-poll/flip.
        self._stale_terminal(tmp_path, "rev-bbbb")
        t = _make_task(
            "rev-bbbb", status="in-review", dispatch_generation=2,
            pr_url="https://example.com/pr/live",
        )
        with patch.object(loop, "_gh_pr_checks", side_effect=AssertionError("reached CI branch on stale")):
            actions = loop._reconcile_inflight(
                [t], "p", "fleet", home=tmp_path,
            )
        assert actions == []

    def test_current_state_still_reconciles(self, tmp_path: Path) -> None:
        # Sanity: a CURRENT terminal done+PR state still produces the
        # in-review flip (the chokepoint doesn't break the happy path).
        _write_worker_state(
            tmp_path, "p", "cur-cccc",
            {
                "slug": "cur-cccc", "phase": "done",
                "pr_url": "https://example.com/pr/123",
                "dispatch_generation": 2,
            },
        )
        t = _make_task("cur-cccc", status="in-progress", dispatch_generation=2)
        with patch.object(loop, "dispatch_mod") as dm:
            dm.project_is_git.return_value = True
            actions = loop._reconcile_inflight([t], "p", "fleet", home=tmp_path)
        assert len(actions) == 1
        assert actions[0].new_status == "in-review"
        assert actions[0].set_pr_url == "https://example.com/pr/123"

    def test_stale_fresh_nonterminal_state_does_not_mask_as_alive(self, tmp_path: Path) -> None:
        # codex iter-2 [P1]: a re-dispatched slug whose PRIOR attempt left
        # a FRESH non-terminal state.json (phase=tdd-green, recent
        # updated_at) would make _is_worker_alive return True off the
        # stale file. The generation gate must run BEFORE liveness so the
        # stale file does not suppress reconcile forever. Expected: ZERO
        # actions (short-circuited as stale + surfaced) — NOT silently
        # "alive" (which would also be zero actions but for the wrong
        # reason); we assert the surfacing stderr to disambiguate.
        import datetime as dt2
        fresh = dt2.datetime.now(dt2.timezone.utc).isoformat()
        _write_worker_state(
            tmp_path, "p", "mask-eeee",
            {"slug": "mask-eeee", "phase": "tdd-green",
             "dispatch_generation": 1, "updated_at": fresh},
        )
        # worker_pid 0 → liveness relies on state freshness; the stale
        # fresh state would otherwise read alive.
        t = _make_task("mask-eeee", status="in-progress", dispatch_generation=2)
        with patch.object(loop, "_is_worker_alive", side_effect=AssertionError("liveness consulted before gen gate")):
            actions = loop._reconcile_inflight([t], "p", "fleet", home=tmp_path)
        assert actions == [], "stale state must short-circuit before liveness"

    def test_missing_state_keeps_died_without_pr(self, tmp_path: Path) -> None:
        # No state file at all → MISSING → existing died-without-PR
        # semantics still apply (status=todo + clear_worker).
        t = _make_task("gone-dddd", status="in-progress", dispatch_generation=2)
        with patch.object(loop, "dispatch_mod") as dm:
            dm.project_is_git.return_value = True
            actions = loop._reconcile_inflight([t], "p", "fleet", home=tmp_path)
        assert len(actions) == 1
        assert actions[0].new_status == "todo"
        assert actions[0].clear_worker is True
        assert actions[0].delete_worker_dir is True


class TestR5ReaperStale:
    """R5: _build_reap_inputs feeds worker_state into judge_completion,
    which reaps/kills on a terminal phase. A stale state must NOT enter
    the reap input set — else a stale phase=done reaps the live attempt."""

    def test_stale_state_excluded_from_reap_inputs(self, tmp_path: Path) -> None:
        _write_worker_state(
            tmp_path, "p", "reap-aaaa",
            {"slug": "reap-aaaa", "phase": "done", "dispatch_generation": 1,
             "pr_url": "https://x/old"},
        )
        t = _make_task("reap-aaaa", status="in-progress", dispatch_generation=2)
        inputs = loop._build_reap_inputs(
            [t], project="p", home=tmp_path, agent_id_map={}, is_git=True,
        )
        assert inputs == [], "stale slug must be excluded from reap inputs"

    def test_current_state_included(self, tmp_path: Path) -> None:
        _write_worker_state(
            tmp_path, "p", "reap-bbbb",
            {"slug": "reap-bbbb", "phase": "done", "dispatch_generation": 2,
             "pr_url": "https://x/live", "pid": 4321},
        )
        t = _make_task("reap-bbbb", status="in-progress", dispatch_generation=2)
        inputs = loop._build_reap_inputs(
            [t], project="p", home=tmp_path, agent_id_map={}, is_git=True,
        )
        assert len(inputs) == 1
        assert inputs[0].slug == "reap-bbbb"
        assert inputs[0].worker_state is not None
        assert inputs[0].worker_state["phase"] == "done"

    def test_missing_state_included_with_none(self, tmp_path: Path) -> None:
        # MISSING → the reaper judges PENDING (benign); still probed so
        # the tmux-session confirmation can run. worker_state is None.
        t = _make_task("reap-cccc", status="in-progress", dispatch_generation=2)
        inputs = loop._build_reap_inputs(
            [t], project="p", home=tmp_path, agent_id_map={}, is_git=True,
        )
        assert len(inputs) == 1
        assert inputs[0].worker_state is None


class TestR6SupervisorStuckStale:
    """R6: the stuck-check reads state.json and may nudge/escalate/block.
    A stale state must short-circuit before any ladder advance."""

    def test_stuck_check_skips_stale_state(self, tmp_path: Path) -> None:
        import supervisor

        _write_worker_state(
            tmp_path, "p", "stuck-aaaa",
            {"slug": "stuck-aaaa", "phase": "tdd-green",
             "dispatch_generation": 1,
             "updated_at": "2000-01-01T00:00:00Z"},  # ancient → would-be stuck
        )
        probe = supervisor.WorkerProbe(
            slug="stuck-aaaa",
            state_path=tmp_path / "projects" / "p" / "workers" / "stuck-aaaa" / "state.json",
            agent_id="aaaaaaaa",
            tmux_session="fleet-aaaaaaaa",
            live_worker=True,
            dispatch_generation=2,  # authority advanced → on-disk gen 1 is stale
        )
        cfg = supervisor.SupervisorConfig()
        # If the stale state were treated as current, _mark_worker_blocked /
        # nudge would fire. Patch them to fail loudly.
        with patch.object(supervisor, "nudge_worker", side_effect=AssertionError("nudged stale")), \
             patch.object(supervisor, "_mark_worker_blocked", side_effect=AssertionError("blocked stale")), \
             patch.object(supervisor, "_escalate_to_operator", side_effect=AssertionError("escalated stale")), \
             patch.object(supervisor, "emit_stuck_alert", side_effect=AssertionError("alerted stale")):
            out = supervisor._run_stuck_check_pass(
                probes=[probe], project="p", home=tmp_path,
                fleet_bin="fleet", cfg=cfg, now_unix=2_000_000_000.0,
                log_stream=None, coord_id="cccccccc",
            )
        assert out.nudges == 0 and out.escalations == 0 and out.blocks == 0

    def test_build_worker_probes_captures_generation(self, tmp_path: Path) -> None:
        import supervisor

        t = _make_task("probe-bbbb", status="in-progress", dispatch_generation=7)
        probes = supervisor.build_worker_probes(
            project="p", home=tmp_path, tasks=[t], agent_id_map={},
        )
        assert len(probes) == 1
        assert probes[0].dispatch_generation == 7


# ======================================================================
# T4 — sentinel path (S1-S5): the generation token gates ALL terminal
#      side effects; caller gating; deferred round-trip; tokenless-legacy.
# ======================================================================


class TestSentinelGrammarToken:
    """The grammar parses an optional gen=<n> token immediately after the
    slug, additively (tokenless lines still parse)."""

    def test_task_done_pr_with_gen(self) -> None:
        s = loop._parse_sentinel("TASK_DONE_PR=alpha-aaaa gen=3 https://x/y/1")
        assert s is not None and s.kind == "task_done_pr"
        assert s.slug == "alpha-aaaa"
        assert s.dispatch_generation == 3
        assert s.payload == "https://x/y/1"

    def test_worker_failed_with_gen(self) -> None:
        s = loop._parse_sentinel("WORKER_FAILED=beta-bbbb gen=5 disk full and more")
        assert s is not None and s.dispatch_generation == 5
        assert s.payload == "disk full and more"

    def test_blocked_question_with_gen(self) -> None:
        s = loop._parse_sentinel("BLOCKED_QUESTION=gamma-cccc gen=2 which API?")
        assert s is not None and s.dispatch_generation == 2
        assert s.payload == "which API?"

    def test_tokenless_lines_parse_as_none(self) -> None:
        s = loop._parse_sentinel("TASK_DONE_PR=delta-dddd https://x/y/2")
        assert s is not None and s.dispatch_generation is None
        assert s.payload == "https://x/y/2"

    def test_new_task_is_tokenless(self) -> None:
        s = loop._parse_sentinel("NEW_TASK=eps-eeee")
        assert s is not None and s.dispatch_generation is None


class TestSentinelCorroboration:
    """_apply_sentinel returns skipped_stale and runs NO side effect when
    the sentinel's token mismatches the task-row authority — even with an
    identical branch/path (S1/S2/S3). The fleet CLI is patched so any
    mutation would be recorded; a stale sentinel records nothing."""

    def _no_mutation(self, action, authority, **kw):
        # An identical worker/<slug> branch + deterministic path on the
        # re-dispatched slug: branch match alone would WRONGLY pass.
        t = _make_task(
            action.slug, status="in-progress", dispatch_generation=authority,
            branch=f"worker/{action.slug}",
        )
        calls: list[list[str]] = []
        with patch.object(loop, "_run_fleet", side_effect=lambda cmd, timeout_s=30.0: calls.append(list(cmd))), \
             patch.object(loop, "_maybe_remove_worktree") as mrw, \
             patch.object(loop, "_maybe_delete_worker_dir") as mdwd:
            outcome = loop._apply_sentinel(
                action, "p", "fleet",
                tasks_by_slug={action.slug: t},
                full_tasks_by_slug={action.slug: t},
                **kw,
            )
        return outcome, calls, mrw, mdwd

    def test_stale_task_done_pr_skips_all_side_effects(self) -> None:
        action = loop._SentinelAction(
            slug="ship-aaaa", kind="task_done_pr",
            payload="https://x/pr/STALE", dispatch_generation=1,
        )
        outcome, calls, mrw, mdwd = self._no_mutation(action, authority=2)
        assert outcome == loop.SENTINEL_SKIPPED_STALE
        assert calls == [], "no tasks.md mutation on a stale sentinel"
        mrw.assert_not_called()
        mdwd.assert_not_called()

    def test_stale_worker_failed_skips_all_side_effects(self) -> None:
        action = loop._SentinelAction(
            slug="fail-bbbb", kind="worker_failed",
            payload="crashed", dispatch_generation=1,
        )
        outcome, calls, mrw, mdwd = self._no_mutation(action, authority=2)
        assert outcome == loop.SENTINEL_SKIPPED_STALE
        assert calls == []
        mrw.assert_not_called()
        mdwd.assert_not_called()

    def test_stale_blocked_question_not_flipped(self) -> None:
        # S3: a stale blocked_question must NOT flip the CURRENT worker to
        # blocked nor add a note.
        action = loop._SentinelAction(
            slug="blk-cccc", kind="blocked_question",
            payload="why?", dispatch_generation=1,
        )
        outcome, calls, _mrw, _mdwd = self._no_mutation(action, authority=2)
        assert outcome == loop.SENTINEL_SKIPPED_STALE
        assert calls == []

    def test_current_token_applies(self) -> None:
        action = loop._SentinelAction(
            slug="ship-dddd", kind="task_done_pr",
            payload="https://x/pr/live", dispatch_generation=2,
        )
        outcome, calls, mrw, mdwd = self._no_mutation(action, authority=2)
        assert outcome == loop.SENTINEL_APPLIED
        # pr_url + status=in-review set; worktree + dir cleanup invoked.
        joined = [" ".join(c) for c in calls]
        assert any("pr_url=https://x/pr/live" in j for j in joined)
        assert any("status=in-review" in j for j in joined)
        mrw.assert_called_once()
        mdwd.assert_called_once()

    def test_tokenless_legacy_trusted_when_authority_zero(self) -> None:
        # Tokenless sentinel + authority 0 (legacy / un-migrated) → trusted.
        action = loop._SentinelAction(
            slug="leg-eeee", kind="task_done_pr",
            payload="https://x/pr/legacy", dispatch_generation=None,
        )
        outcome, calls, mrw, _mdwd = self._no_mutation(action, authority=0)
        assert outcome == loop.SENTINEL_APPLIED
        mrw.assert_called_once()

    def test_tokenless_legacy_trusted_on_first_dispatch_gen1(self) -> None:
        # codex iter-3 [P1] rollout: the FIRST dispatch under the epoch
        # sets gen 1, but current emitters don't yet stamp gen= on the
        # sentinel. A tokenless TASK_DONE_PR for a first-attempt (gen 1,
        # NOT re-dispatched) slug must STILL apply — else every dispatched
        # task's completion silently stops draining.
        action = loop._SentinelAction(
            slug="leg-first", kind="task_done_pr",
            payload="https://x/pr/first", dispatch_generation=None,
        )
        outcome, _calls, mrw, _mdwd = self._no_mutation(action, authority=1)
        assert outcome == loop.SENTINEL_APPLIED
        mrw.assert_called_once()

    def test_tokenless_legacy_skipped_when_redispatched(self) -> None:
        # Tokenless sentinel + authority >= 2 (slug GENUINELY re-dispatched
        # — each re-dispatch increments by 1, so >= 2 means a second
        # attempt) → skipped_stale (fail safe: never reap a re-dispatched
        # live tree on a tokenless prior-attempt sentinel).
        action = loop._SentinelAction(
            slug="leg-ffff", kind="task_done_pr",
            payload="https://x/pr/legacy", dispatch_generation=None,
        )
        outcome, calls, mrw, _mdwd = self._no_mutation(action, authority=2)
        assert outcome == loop.SENTINEL_SKIPPED_STALE
        assert calls == []
        mrw.assert_not_called()

    def test_authority_read_from_disk_when_no_snapshot(self, tmp_path: Path) -> None:
        # When no in-memory snapshot is passed, the authority is read from
        # tasks.md on disk. Write a task row at gen 2 and verify a gen-1
        # sentinel is skipped.
        proj_dir = tmp_path / "projects" / "p"
        proj_dir.mkdir(parents=True)
        t = _make_task("disk-gggg", status="in-progress", dispatch_generation=2)
        f = parse.File(schema=parse.SCHEMA_VERSION, tasks=[t])
        parse.write(str(proj_dir / "tasks.md"), f)
        action = loop._SentinelAction(
            slug="disk-gggg", kind="task_done_pr",
            payload="https://x/pr/old", dispatch_generation=1,
        )
        with patch.object(loop, "_run_fleet", side_effect=AssertionError("mutated on stale")), \
             patch.object(loop, "_maybe_remove_worktree"), \
             patch.object(loop, "_maybe_delete_worker_dir"):
            outcome = loop._apply_sentinel(action, "p", "fleet", home=tmp_path)
        assert outcome == loop.SENTINEL_SKIPPED_STALE


class TestSentinelDeferredRoundTrip:
    """S5: the deferred-sentinel queue persists the generation token so a
    deferred→replayed sentinel corroborates correctly on replay."""

    def test_token_round_trips_through_save_load(self) -> None:
        cs: dict = {}
        actions = [
            loop._SentinelAction(slug="d-aaaa", kind="task_done_pr",
                                 payload="https://x/1", dispatch_generation=4),
            loop._SentinelAction(slug="d-bbbb", kind="worker_failed",
                                 payload="boom", dispatch_generation=None),
        ]
        loop._save_deferred_sentinels(cs, actions)
        loaded = loop._load_deferred_sentinels(cs)
        assert len(loaded) == 2
        assert loaded[0].dispatch_generation == 4
        assert loaded[1].dispatch_generation is None

    def test_legacy_entry_without_gen_key_loads_as_none(self) -> None:
        # A coord-state.json written before this change has no
        # dispatch_generation key in the entry → None (tokenless).
        cs = {
            loop._DEFERRED_SENTINELS_KEY: [
                {"slug": "old-cccc", "kind": "task_done_pr",
                 "payload": "https://x/2"},
            ],
        }
        loaded = loop._load_deferred_sentinels(cs)
        assert len(loaded) == 1
        assert loaded[0].dispatch_generation is None


# ======================================================================
# D1 — _sweep_done_worker_dirs skips parked rows.
# ======================================================================


class TestD1ParkedSweepGuard:
    def _make_done_dir(self, home: Path, slug: str) -> Path:
        d = home / "projects" / "p" / "workers" / slug
        d.mkdir(parents=True, exist_ok=True)
        (d / "state.json").write_text("{}", encoding="utf-8")
        return d

    def test_parked_done_row_dir_survives_sweep(self, tmp_path: Path) -> None:
        self._make_done_dir(tmp_path, "park-aaaa")
        t = _make_task(
            "park-aaaa", status="done", dispatch_generation=1,
            parked="2026-06-03T00:00:00Z dirty worktree",
        )
        with patch.object(loop, "_maybe_delete_worker_dir") as mdwd:
            swept = loop._sweep_done_worker_dirs(
                [t], "p", "fleet", home=tmp_path,
            )
        assert swept == 0
        mdwd.assert_not_called()

    def test_unparked_done_row_dir_swept(self, tmp_path: Path) -> None:
        self._make_done_dir(tmp_path, "sweep-bbbb")
        t = _make_task("sweep-bbbb", status="done", dispatch_generation=1)
        with patch.object(loop, "_maybe_delete_worker_dir") as mdwd:
            swept = loop._sweep_done_worker_dirs(
                [t], "p", "fleet", home=tmp_path,
            )
        assert swept == 1
        mdwd.assert_called_once()


# ======================================================================
# P1 #1 — the legacy 0->1 re-dispatch fence collision (codex [P1]).
#
# A LEGACY slug (pre-epoch attempt, gen 0, tokenless sentinels) being
# re-dispatched lands at gen 1 (_dispatch_ready: next_gen = 0 + 1) —
# colliding with a genuine FIRST epoch dispatch (also gen 1). A stale
# tokenless prior-attempt sentinel then passes _sentinel_corroborates's
# `authority <= 1` trust window and reaps the LIVE re-dispatched tree.
#
# Fix: every requeue floors dispatch_generation to >= 1, so the next
# dispatch ALWAYS reaches gen >= 2. Then a tokenless prior-attempt
# sentinel against the re-dispatched authority (>= 2) is fenced
# skipped_stale — while the legitimate first-attempt (gen 1, NEVER
# requeued) tokenless sentinel still applies.
# ======================================================================


class TestRedispatchGenerationFloor:
    def _floor_calls(self, task, *, home=None):
        calls: list[list[str]] = []
        with patch.object(
            loop, "_run_fleet",
            side_effect=lambda cmd, timeout_s=30.0: calls.append(list(cmd)),
        ):
            loop._floor_dispatch_generation_for_requeue(
                task.slug, "p", "fleet",
                full_tasks_by_slug={task.slug: task},
                home=home,
            )
        return calls

    def test_legacy_gen0_requeue_floors_to_one(self) -> None:
        # A legacy gen-0 slug requeued → floored to dispatch_generation=1
        # so the NEXT dispatch reaches gen 2 (>= 2 fences the tokenless
        # prior-attempt sentinel).
        t = _make_task("leg-floor", status="todo", dispatch_generation=0)
        calls = self._floor_calls(t)
        joined = [" ".join(c) for c in calls]
        assert any("dispatch_generation=1" in j for j in joined), joined

    def test_epoch_gen1_requeue_is_idempotent_no_write(self) -> None:
        # An already-epoch-dispatched slug (gen 1) requeued → NO write
        # (re-dispatch already reaches gen 2 from current+1).
        t = _make_task("epoch-floor", status="todo", dispatch_generation=1)
        calls = self._floor_calls(t)
        assert calls == [], "gen>=1 already fences; no redundant CLI write"

    def test_worker_failed_sentinel_floors_legacy_gen(self) -> None:
        # WORKER_FAILED requeues the slug; a legacy gen-0 row must be
        # floored to 1 inside the apply so the re-dispatch reaches >= 2.
        action = loop._SentinelAction(
            slug="wf-floor", kind="worker_failed",
            payload="crashed", dispatch_generation=None,  # tokenless legacy
        )
        t = _make_task("wf-floor", status="in-progress", dispatch_generation=0)
        calls: list[list[str]] = []
        with patch.object(
            loop, "_run_fleet",
            side_effect=lambda cmd, timeout_s=30.0: calls.append(list(cmd)),
        ), patch.object(loop, "_maybe_remove_worktree"), \
             patch.object(loop, "_maybe_delete_worker_dir"):
            outcome = loop._apply_sentinel(
                action, "p", "fleet",
                tasks_by_slug={t.slug: t}, full_tasks_by_slug={t.slug: t},
            )
        assert outcome == loop.SENTINEL_APPLIED  # gen0 tokenless = first attempt
        joined = [" ".join(c) for c in calls]
        assert any("dispatch_generation=1" in j for j in joined), joined

    def test_reconcile_todo_floors_legacy_gen(self, tmp_path: Path) -> None:
        # _apply_reconcile on a todo requeue floors a legacy gen-0 slug.
        action = loop._ReconcileAction(
            slug="rc-floor", new_status="todo", clear_worker=True,
            delete_worker_dir=True, note="worker died without PR",
        )
        t = _make_task("rc-floor", status="in-progress", dispatch_generation=0)
        calls: list[list[str]] = []
        with patch.object(
            loop, "_run_fleet",
            side_effect=lambda cmd, timeout_s=30.0: calls.append(list(cmd)),
        ), patch.object(loop, "_maybe_remove_worktree"), \
             patch.object(loop, "_maybe_delete_worker_dir"):
            loop._apply_reconcile(
                action, "p", "fleet",
                full_tasks_by_slug={t.slug: t}, home=tmp_path,
            )
        joined = [" ".join(c) for c in calls]
        assert any("dispatch_generation=1" in j for j in joined), joined

    def test_stale_tokenless_after_redispatch_authority2_skipped(self) -> None:
        # End-to-end of the fix's purpose: after the floor+increment the
        # re-dispatched authority is >= 2, so a stale tokenless prior-
        # attempt TASK_DONE_PR is fenced (NO reap of the live tree) — even
        # with an identical worker/<slug> branch.
        action = loop._SentinelAction(
            slug="e2e-redisp", kind="task_done_pr",
            payload="https://x/pr/STALE", dispatch_generation=None,
        )
        t = _make_task(
            "e2e-redisp", status="in-progress", dispatch_generation=2,
            branch="worker/e2e-redisp",
        )
        calls: list[list[str]] = []
        with patch.object(
            loop, "_run_fleet",
            side_effect=lambda cmd, timeout_s=30.0: calls.append(list(cmd)),
        ), patch.object(loop, "_maybe_remove_worktree") as mrw, \
             patch.object(loop, "_maybe_delete_worker_dir") as mdwd:
            outcome = loop._apply_sentinel(
                action, "p", "fleet",
                tasks_by_slug={t.slug: t}, full_tasks_by_slug={t.slug: t},
            )
        assert outcome == loop.SENTINEL_SKIPPED_STALE
        assert calls == []
        mrw.assert_not_called()
        mdwd.assert_not_called()

    def test_first_attempt_tokenless_gen1_still_applies(self) -> None:
        # Preserve codex iter-3: a genuine first-attempt (gen 1, never
        # requeued, so never floored ABOVE 1) tokenless sentinel still
        # applies — the floor reserves gen 1 for exactly this case.
        action = loop._SentinelAction(
            slug="e2e-first", kind="task_done_pr",
            payload="https://x/pr/first", dispatch_generation=None,
        )
        t = _make_task(
            "e2e-first", status="in-progress", dispatch_generation=1,
            branch="worker/e2e-first",
        )
        with patch.object(loop, "_run_fleet"), \
             patch.object(loop, "_maybe_remove_worktree") as mrw, \
             patch.object(loop, "_maybe_delete_worker_dir"):
            outcome = loop._apply_sentinel(
                action, "p", "fleet",
                tasks_by_slug={t.slug: t}, full_tasks_by_slug={t.slug: t},
            )
        assert outcome == loop.SENTINEL_APPLIED
        mrw.assert_called_once()


# ======================================================================
# P1 #2 — dispatch partial-apply WEDGE recovery (codex [P1]).
#
# _apply_dispatch crashes after the status=in-progress + dispatch_
# generation commit but BEFORE the state.json bootstrap. The id is still
# in pending_acquire (forget runs only after _apply_dispatch returns), so
# the journal replay SKIPS the slug — and reconcile previously short-
# circuited on `stale`, deferring to a replay that never fires. The task
# wedged in-progress forever. Reconcile now detects "no replay owner" and
# requeues to todo (gen preserved; the next dispatch increments so the
# prior attempt's state stays `stale`, monotonic, no reuse).
# ======================================================================


class TestPartialApplyWedgeRecovery:
    def _journal(self, home: Path, agent_id: str, slug: str, state: str) -> None:
        d = home / "dispatches"
        d.mkdir(parents=True, exist_ok=True)
        (d / f"{agent_id}.json").write_text(
            json.dumps({
                "owner": f"project/p/slug/{slug}",
                "exec_state": state,
            }),
            encoding="utf-8",
        )

    def _stale_state(self, home: Path, slug: str, gen: int) -> None:
        _write_worker_state(
            home, "p", slug,
            {"slug": slug, "phase": "starting", "dispatch_generation": gen},
        )

    def test_wedge_requeued_when_worker_never_launched(
        self, tmp_path: Path,
    ) -> None:
        # Partial-apply: authority gen 2, only a stale gen-1 state on disk,
        # id in pending_acquire (replay SKIPS it), journal PENDING (worker
        # never launched). Safe to requeue.
        slug = "wedge-aaaa"
        agent_id = "abcd1234"
        self._stale_state(tmp_path, slug, gen=1)
        self._journal(tmp_path, agent_id, slug, "pending")
        coord_state = {
            "worker_agent_ids": {slug: agent_id},
            "pending_acquire_agent_ids": {slug: agent_id},
        }
        t = _make_task(slug, status="in-progress", dispatch_generation=2)
        with patch.object(loop, "_is_worker_alive", return_value=False):
            actions = loop._reconcile_inflight(
                [t], "p", "fleet", home=tmp_path, coord_state=coord_state,
            )
        # NOT wedged: recovered to a re-dispatchable status.
        assert len(actions) == 1
        a = actions[0]
        assert a.new_status == "todo"
        assert a.clear_worker is True
        assert a.delete_worker_dir is True

    def test_cross_restart_orphan_pending_journal_requeued(
        self, tmp_path: Path,
    ) -> None:
        # review-iter [P1]: coord crashed BEFORE saving coord-state, so
        # worker_agent_ids/pending_acquire are EMPTY after restart. The
        # durable orphan PENDING journal (worker never launched) for the
        # slug is the only evidence — replay also skips it (no adopted id).
        # Reconcile must still recover the wedge.
        slug = "restart-yyyy"
        agent_id = "deed0001"
        self._stale_state(tmp_path, slug, gen=1)
        self._journal(tmp_path, agent_id, slug, "pending")
        coord_state: dict = {}  # coord-state LOST on the crash
        t = _make_task(slug, status="in-progress", dispatch_generation=2)
        with patch.object(loop, "_is_worker_alive", return_value=False):
            actions = loop._reconcile_inflight(
                [t], "p", "fleet", home=tmp_path, coord_state=coord_state,
            )
        assert len(actions) == 1
        assert actions[0].new_status == "todo"
        assert actions[0].clear_worker is True

    def test_cross_restart_launched_journal_NOT_requeued(
        self, tmp_path: Path,
    ) -> None:
        # Cross-restart, but the orphan journal is launch_attempted — a
        # worker DID launch before the crash. Requeuing would double-
        # dispatch. Must NOT requeue.
        slug = "restart-launched"
        agent_id = "deed0002"
        self._stale_state(tmp_path, slug, gen=1)
        self._journal(tmp_path, agent_id, slug, "launch_attempted")
        t = _make_task(slug, status="in-progress", dispatch_generation=2)
        with patch.object(loop, "_is_worker_alive", return_value=False):
            actions = loop._reconcile_inflight(
                [t], "p", "fleet", home=tmp_path, coord_state={},
            )
        assert actions == [], "launched worker (no coord-state) must not requeue"

    def test_launch_attempted_journal_NOT_requeued(
        self, tmp_path: Path,
    ) -> None:
        # A worker that DID launch (journal launch_attempted) must NOT be
        # requeued even though its id is in pending_acquire — requeuing a
        # launched worker is the #184 double-dispatch trap. Replay's
        # residual-crash repair / the live worker owns recovery.
        slug = "launched-xxxx"
        agent_id = "1eaf0001"
        self._stale_state(tmp_path, slug, gen=1)
        self._journal(tmp_path, agent_id, slug, "launch_attempted")
        coord_state = {
            "worker_agent_ids": {slug: agent_id},
            "pending_acquire_agent_ids": {slug: agent_id},
        }
        t = _make_task(slug, status="in-progress", dispatch_generation=2)
        with patch.object(loop, "_is_worker_alive", return_value=False):
            actions = loop._reconcile_inflight(
                [t], "p", "fleet", home=tmp_path, coord_state=coord_state,
            )
        assert actions == [], "a launched worker must not be requeued"

    def test_prior_stale_state_still_fences_after_recovery(
        self, tmp_path: Path,
    ) -> None:
        # After recovery the task row keeps gen 2; the NEXT dispatch
        # increments to gen 3 (the floor is for legacy gen-0 only — gen 2
        # already reaches 3 via current+1). A prior-attempt stale state
        # (gen 1 or 2) then classifies `stale`, NOT `current`, against the
        # new authority — proving no generation REUSE re-validates it.
        slug = "wedge-fence"
        # state on disk is gen 2 (the wedged attempt's authority); the
        # NEXT dispatch will be gen 3.
        self._stale_state(tmp_path, slug, gen=2)
        cls, _st = loop.read_current_worker_state(
            "p", slug, 3, home=tmp_path,
        )
        assert cls == loop.WORKER_STATE_STALE

    def test_replay_recoverable_slug_not_requeued(self, tmp_path: Path) -> None:
        # A `stale` slug whose adopted PENDING journal is NOT in
        # pending_acquire IS replay-eligible → reconcile must short-
        # circuit (NO requeue) so we don't double-dispatch the #184 trap.
        slug = "live-bbbb"
        agent_id = "beef0001"
        self._stale_state(tmp_path, slug, gen=1)
        self._journal(tmp_path, agent_id, slug, "pending")
        coord_state = {
            "worker_agent_ids": {slug: agent_id},
            # NOT in pending_acquire → replay WILL re-emit.
        }
        t = _make_task(slug, status="in-progress", dispatch_generation=2)
        with patch.object(loop, "_is_worker_alive", return_value=False):
            actions = loop._reconcile_inflight(
                [t], "p", "fleet", home=tmp_path, coord_state=coord_state,
            )
        assert actions == [], "replay owns recovery; reconcile must not requeue"

    def test_no_coord_state_preserves_legacy_shortcircuit(
        self, tmp_path: Path,
    ) -> None:
        # Legacy callers / tests that pass no coord_state keep the
        # pre-fix behavior: a stale in-progress slug short-circuits (no
        # requeue) and defers to replay.
        slug = "legacy-cccc"
        self._stale_state(tmp_path, slug, gen=1)
        t = _make_task(slug, status="in-progress", dispatch_generation=2)
        with patch.object(loop, "_is_worker_alive", return_value=False):
            actions = loop._reconcile_inflight(
                [t], "p", "fleet", home=tmp_path,  # no coord_state
            )
        assert actions == []

    def test_wedge_recoverable_predicate(self, tmp_path: Path) -> None:
        slug = "pred-dddd"
        agent_id = "cafe0001"
        self._journal(tmp_path, agent_id, slug, "pending")
        # id in pending_acquire + pending journal → WEDGE (worker never
        # launched), safe to requeue.
        assert loop._dispatch_wedge_recoverable(
            slug, "p", tmp_path,
            {"worker_agent_ids": {slug: agent_id},
             "pending_acquire_agent_ids": {slug: agent_id}},
        ) is True
        # id NOT in pending_acquire (replay-eligible / applied) → NOT a
        # wedge; replay owns it.
        assert loop._dispatch_wedge_recoverable(
            slug, "p", tmp_path,
            {"worker_agent_ids": {slug: agent_id}},
        ) is False
        # no adopted id BUT orphan pending journal (cross-restart, path B)
        # → wedge (worker never launched), requeue-safe.
        assert loop._dispatch_wedge_recoverable(slug, "p", tmp_path, {}) is True
        # launched (journal launch_attempted) → not requeue-safe (both A+B).
        self._journal(tmp_path, agent_id, slug, "launch_attempted")
        assert loop._dispatch_wedge_recoverable(
            slug, "p", tmp_path,
            {"worker_agent_ids": {slug: agent_id},
             "pending_acquire_agent_ids": {slug: agent_id}},
        ) is False
        # cross-restart with launched journal → also not requeue-safe.
        assert loop._dispatch_wedge_recoverable(slug, "p", tmp_path, {}) is False
