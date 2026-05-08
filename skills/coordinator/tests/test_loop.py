"""loop.py tests: drive a tick with mocked fleet binary + gh subprocess.

Strategy: build tasks.md fixtures inside tmp_path/projects/<p>/, point
loop.tick at it via fleet_home=, and patch subprocess.run to record
argv. The fleet binary itself is never invoked.

Critical scenarios (ENG §7.2):
- test_tick_dispatches_ready_task — happy path
- test_tick_reconciles_dead_worker — pid not alive, no PR → reset to todo
- test_tick_drains_per_task_sentinels — two slugs, two archive files,
  no cross-contamination (the C2 invariant)
- test_tick_respects_cap — cap=1 with two ready tasks → one dispatch
- test_tick_no_op_under_lock_held — second tick exits cleanly
- test_slug_mismatch_sentinel_ignored — unknown slug logs WARN, no mutation
"""
from __future__ import annotations

import datetime as _dt
import fcntl
import json
import os
import subprocess
from pathlib import Path
from unittest.mock import patch

import pytest

import dispatch
import loop
import parse


# ---------- helpers ----------


def _write_tasks(project_dir: Path, tasks: list[parse.Task], footer: str = "") -> None:
    project_dir.mkdir(parents=True, exist_ok=True)
    (project_dir / ".locks").mkdir(exist_ok=True)
    f = parse.File(schema=parse.SCHEMA_VERSION, tasks=tasks, footer=footer)
    parse.write(str(project_dir / "tasks.md"), f)


def _make_task(
    slug: str, status: str = "ready", priority: str = "P1",
    *, worker_pid: int = 0, pr_url: str = "", spec: str = "spec",
    acceptance: str = "acc", notes: str = "",
    depends_on: list[str] | None = None,
) -> parse.Task:
    return parse.Task(
        slug=slug, status=status, priority=priority,
        worker_pid=worker_pid, pr_url=pr_url,
        created=_dt.datetime(2026, 5, 6, 10, 0, 0, tzinfo=_dt.timezone.utc),
        updated=_dt.datetime(2026, 5, 6, 10, 0, 0, tzinfo=_dt.timezone.utc),
        spawned_by="user",
        depends_on=list(depends_on) if depends_on else [],
        spec=spec, acceptance=acceptance, notes=notes,
    )


@pytest.fixture
def fleet_home(tmp_path: Path) -> Path:
    home = tmp_path / "fleet"
    home.mkdir()
    (home / "inbox").mkdir()
    (home / "inbox" / "archive").mkdir()
    (home / "projects").mkdir()
    return home


@pytest.fixture
def project_dir(fleet_home: Path) -> Path:
    p = fleet_home / "projects" / "fleet"
    p.mkdir(parents=True, exist_ok=True)
    (p / ".locks").mkdir(exist_ok=True)
    return p


@pytest.fixture
def fleet_run_recorder():
    """Patch loop._run_fleet to record invocations. Returns the calls list."""
    calls: list[list[str]] = []

    def fake_run(cmd, timeout_s=30.0):
        calls.append(list(cmd))

    with patch.object(loop, "_run_fleet", side_effect=fake_run):
        yield calls


@pytest.fixture
def dispatch_subprocess(monkeypatch):
    """Stub dispatch.subprocess.run to mimic `fleet dispatch` success.

    Returns a stack of agent IDs to hand out — each call pops one.
    """
    ids: list[str] = []

    def fake_run(cmd, capture_output=True, text=True, timeout=None, check=False):
        # Distinguish CLI calls inside dispatch.py: dispatch_worker /
        # fetch_standards / fetch_learnings all route through subprocess.run.
        if cmd[1:3] == ["standards", "show"]:
            return subprocess.CompletedProcess(
                args=cmd, returncode=0, stdout="# Standards\n", stderr="",
            )
        if cmd[1:3] == ["learnings", "list"]:
            return subprocess.CompletedProcess(
                args=cmd, returncode=0, stdout="", stderr="",
            )
        if cmd[1] == "dispatch":
            agent_id = ids.pop(0) if ids else "abcdef01"
            return subprocess.CompletedProcess(
                args=cmd, returncode=0,
                stdout=f"agent {agent_id} dispatched\n", stderr="",
            )
        return subprocess.CompletedProcess(
            args=cmd, returncode=0, stdout="", stderr="",
        )

    monkeypatch.setattr(dispatch.subprocess, "run", fake_run)
    return ids


# ---------- happy path: dispatch a ready task ----------


def test_tick_dispatches_ready_task(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess,
) -> None:
    _write_tasks(project_dir, [_make_task("ready-aaaa", status="ready")])
    dispatch_subprocess.append("abcdef01")

    result = loop.tick(
        "fleet", coord_id="cccccc01", cwd="/repo",
        fleet_home=str(fleet_home),
    )

    assert not result.skipped
    assert result.dispatched == 1
    assert result.reconciled == 0
    # Inbox stub written.
    inbox_file = fleet_home / "inbox" / "abcdef01.md"
    assert inbox_file.exists()
    body = inbox_file.read_text()
    assert "Fleet worker for task: ready-aaaa" in body
    # Confirm we set status=in-progress and recorded the agent ID.
    statuses = [c for c in fleet_run_recorder if "set" in c and "status=in-progress" in c]
    assert len(statuses) == 1
    notes = [c for c in fleet_run_recorder if "note" in c]
    assert any("abcdef01" in (c[-1] if c else "") for c in notes)


# ---------- reconcile: dead worker, no PR ----------


def test_tick_reconciles_dead_worker(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess,
) -> None:
    # PID = 1 but we'll patch _pid_alive to say False.
    _write_tasks(project_dir, [
        _make_task("dying-aaaa", status="in-progress", worker_pid=1, pr_url=""),
    ])
    with patch.object(loop, "_pid_alive", return_value=False):
        result = loop.tick(
            "fleet", coord_id="cccccc01", cwd="/repo",
            fleet_home=str(fleet_home),
        )

    assert result.reconciled == 1
    # Task reset to todo + worker_pid cleared + note appended.
    set_calls = [c for c in fleet_run_recorder if c[1:3] == ["tasks", "set"]]
    note_calls = [c for c in fleet_run_recorder if c[1:3] == ["tasks", "note"]]
    assert any("status=todo" in c for c in set_calls)
    assert any("worker_pid=0" in c for c in set_calls)
    assert any("worker died without PR" in c[-1] for c in note_calls)


def test_tick_reconciles_dead_worker_with_green_ci(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess,
) -> None:
    """Worker died but PR has green CI → coord raises 'ready to merge'."""
    _write_tasks(project_dir, [
        _make_task(
            "merged-aaaa", status="in-review", worker_pid=1,
            pr_url="https://github.com/x/y/pull/1",
        ),
    ])
    green = loop._CIResult(all_green=True, merged=False, mergeable=True)
    with patch.object(loop, "_pid_alive", return_value=False), \
         patch.object(loop, "_gh_pr_checks", return_value=green):
        result = loop.tick(
            "fleet", coord_id="cccccc01", cwd="/repo",
            fleet_home=str(fleet_home),
        )

    assert result.reconciled == 1
    assert result.raised == 1
    set_calls = [c for c in fleet_run_recorder if c[1:3] == ["tasks", "set"]]
    assert any("status=in-review" in c for c in set_calls)


# ---------- drain inbox archive sentinels ----------


def _write_archive(
    fleet_home: Path, coord_id: str, stamp: str, body: str,
) -> Path:
    """Drop a pre-formed archive file with the given body (one sentinel per
    file is the canonical layout per ENG §6.3)."""
    arch_dir = fleet_home / "inbox" / "archive"
    arch_dir.mkdir(parents=True, exist_ok=True)
    target = arch_dir / f"{coord_id}-{stamp}.md"
    target.write_text(body, encoding="utf-8")
    return target


def test_tick_drains_per_task_sentinels(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess,
) -> None:
    """C2 invariant: two sentinels for two slugs mutate ONLY their tasks.

    Ordered by stamp (lex == chronological). Both must apply, neither
    bleeds into the other's task.
    """
    _write_tasks(project_dir, [
        _make_task(
            "a-aaaa", status="in-progress", worker_pid=99999,
        ),
        _make_task(
            "b-bbbb", status="in-progress", worker_pid=99998,
        ),
    ])
    coord = "cccccc01"
    _write_archive(
        fleet_home, coord, "20260506-120000Z",
        "TASK_DONE_PR=a-aaaa https://github.com/x/y/pull/1\n",
    )
    _write_archive(
        fleet_home, coord, "20260506-120030Z",
        "TASK_DONE_PR=b-bbbb https://github.com/x/y/pull/2\n",
    )
    # Mark workers alive so reconcile doesn't fire on them — we want
    # the drain path exclusively. Real-world: workers exit AFTER posting
    # the sentinel, but reconcile + drain run independently per tick.
    with patch.object(loop, "_pid_alive", return_value=True):
        result = loop.tick(
            "fleet", coord_id=coord, cwd="/repo",
            fleet_home=str(fleet_home),
        )

    assert result.drained == 2
    set_calls = [c for c in fleet_run_recorder if c[1:3] == ["tasks", "set"]]
    pr_url_a = [c for c in set_calls if "a-aaaa" in c and "pull/1" in (c[-1] if c else "")]
    pr_url_b = [c for c in set_calls if "b-bbbb" in c and "pull/2" in (c[-1] if c else "")]
    assert len(pr_url_a) == 1
    assert len(pr_url_b) == 1
    # Crucial: no a-aaaa setter contains pull/2 and vice versa.
    assert not any("pull/2" in str(c) for c in pr_url_a)
    assert not any("pull/1" in str(c) for c in pr_url_b)
    # last_archive_scan_ts persisted.
    state = json.loads((project_dir / "coord-state.json").read_text())
    assert state["last_archive_scan_ts"].endswith(".md")


def test_tick_drains_blocked_question_raises_to_user(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess,
) -> None:
    _write_tasks(project_dir, [
        _make_task("blocked-aaaa", status="in-progress", worker_pid=99999),
    ])
    coord = "cccccc01"
    _write_archive(
        fleet_home, coord, "20260506-120000Z",
        "BLOCKED_QUESTION=blocked-aaaa Need credentials for X\n",
    )
    with patch.object(loop, "_pid_alive", return_value=True):
        result = loop.tick(
            "fleet", coord_id=coord, cwd="/repo",
            fleet_home=str(fleet_home),
        )
    assert result.drained == 1
    assert result.raised == 1
    set_calls = [c for c in fleet_run_recorder if c[1:3] == ["tasks", "set"]]
    note_calls = [c for c in fleet_run_recorder if c[1:3] == ["tasks", "note"]]
    assert any("status=blocked" in c for c in set_calls)
    assert any("Need credentials for X" in (c[-1] if c else "") for c in note_calls)


def test_slug_mismatch_sentinel_ignored(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess,
) -> None:
    """Sentinel referring to an unknown slug applies nothing.

    The drain path checks tasks_by_slug and skips. No fleet CLI call
    means tasks.md is unchanged (logged-but-not-mutated semantics).
    """
    _write_tasks(project_dir, [_make_task("known-aaaa")])
    coord = "cccccc01"
    _write_archive(
        fleet_home, coord, "20260506-120000Z",
        "TASK_DONE_PR=stranger-9999 https://example.com/pr/1\n",
    )
    result = loop.tick(
        "fleet", coord_id=coord, cwd="/repo",
        fleet_home=str(fleet_home),
    )
    # No set/note calls for stranger-9999.
    bad = [c for c in fleet_run_recorder if "stranger-9999" in c]
    assert bad == []
    # The known task wasn't touched either (it's status=ready and we
    # didn't supply a real dispatch chain in this test fixture's
    # subprocess stub — but it doesn't matter for this assertion).
    assert result.drained == 0


def test_tick_respects_cap_one_with_two_ready(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess,
) -> None:
    _write_tasks(project_dir, [
        _make_task("one-aaaa", priority="P0"),
        _make_task("two-bbbb", priority="P0"),
    ])
    dispatch_subprocess.append("abcdef01")
    result = loop.tick(
        "fleet", coord_id="cccccc01", cwd="/repo",
        cap=1, fleet_home=str(fleet_home),
    )
    assert result.dispatched == 1


def test_tick_dispatches_higher_priority_first(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess,
) -> None:
    _write_tasks(project_dir, [
        _make_task("low-aaaa", priority="P3"),
        _make_task("high-bbbb", priority="P0"),
    ])
    dispatch_subprocess.append("abcdef01")
    result = loop.tick(
        "fleet", coord_id="cccccc01", cwd="/repo",
        cap=1, fleet_home=str(fleet_home),
    )
    assert result.dispatched == 1
    set_calls = [c for c in fleet_run_recorder if c[1:3] == ["tasks", "set"]]
    inprog = [c for c in set_calls if "status=in-progress" in c]
    assert len(inprog) == 1
    assert "high-bbbb" in inprog[0]


def test_tick_skips_ready_tasks_with_unsatisfied_deps(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess,
) -> None:
    _write_tasks(project_dir, [
        _make_task("dep-aaaa", status="ready"),       # not done yet
        _make_task("blocked-bbbb", status="ready", depends_on=["dep-aaaa"]),
    ])
    dispatch_subprocess.append("abcdef01")
    result = loop.tick(
        "fleet", coord_id="cccccc01", cwd="/repo",
        cap=2, fleet_home=str(fleet_home),
    )
    # cap=2 — but blocked-bbbb has unsatisfied dep, dep-aaaa is the
    # only candidate. Exactly one dispatched.
    assert result.dispatched == 1


# ---------- lock contention ----------


def test_tick_no_op_under_lock_held(
    fleet_home: Path, project_dir: Path,
) -> None:
    """Second tick must skip cleanly when the coord lock is held."""
    _write_tasks(project_dir, [_make_task("ready-aaaa")])
    lock_path = project_dir / ".locks" / "coordinator.lock"
    fd = os.open(str(lock_path), os.O_RDWR | os.O_CREAT, 0o644)
    fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
    try:
        result = loop.tick(
            "fleet", coord_id="cccccc01", cwd="/repo",
            fleet_home=str(fleet_home),
        )
        assert result.skipped is True
        assert result.reason == "lock-busy"
        assert result.dispatched == 0
    finally:
        fcntl.flock(fd, fcntl.LOCK_UN)
        os.close(fd)


# ---------- orphan worktree cleanup at tick start ----------


def test_tick_calls_prune_worktrees_in_cap2_mode(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess,
) -> None:
    """cap > 1 ticks invoke `worktree.prune_worktrees(cwd)` once at the
    top, AFTER the NB-flock + BEFORE parse / reconcile / dispatch.

    The prune call is best-effort cleanup of any registry entry whose
    directory no longer exists (e.g. a coord crashed mid-tick after
    `git worktree add` succeeded but before tasks.md was updated).
    Without it, the next dispatch trips on git's "already exists".
    """
    _write_tasks(project_dir, [_make_task("ready-aaaa", status="ready")])
    dispatch_subprocess.append("abcdef01")
    import worktree as worktree_mod
    with patch.object(worktree_mod, "prune_worktrees") as m:
        result = loop.tick(
            "fleet", coord_id="cccccc01", cwd="/repo",
            cap=2, fleet_home=str(fleet_home),
        )
    # Called exactly once with the repo cwd.
    assert m.call_count == 1
    args, _ = m.call_args
    assert args[0] == "/repo"
    # Tick still completed — prune is non-blocking.
    assert not result.skipped


def test_tick_skips_prune_in_cap1_mode(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess,
) -> None:
    """cap=1 mode never creates worktrees — calling prune is harmless
    but unnecessary. Skipping keeps single-worker mode byte-identical
    to v0.2.0 (no extra git invocations on the regression-safe path).
    """
    _write_tasks(project_dir, [_make_task("ready-aaaa", status="ready")])
    dispatch_subprocess.append("abcdef01")
    import worktree as worktree_mod
    with patch.object(worktree_mod, "prune_worktrees") as m:
        loop.tick(
            "fleet", coord_id="cccccc01", cwd="/repo",
            cap=1, fleet_home=str(fleet_home),
        )
    assert m.call_count == 0


def test_tick_prune_failure_does_not_abort_tick(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess,
) -> None:
    """An exception inside prune_worktrees would abort the tick — but
    the helper itself swallows errors. Belt-and-suspenders: even if a
    bug ever leaked an exception out of prune, the tick should not
    silently drop dispatch. Here we patch prune to raise and assert
    the tick still raises (i.e. the exception is NOT swallowed at the
    loop level — that's the prune helper's job, and we want a noisy
    failure if the contract changes)."""
    _write_tasks(project_dir, [_make_task("ready-aaaa", status="ready")])
    dispatch_subprocess.append("abcdef01")
    import worktree as worktree_mod
    with patch.object(
        worktree_mod, "prune_worktrees",
        side_effect=RuntimeError("simulated prune crash"),
    ):
        with pytest.raises(RuntimeError, match="simulated prune crash"):
            loop.tick(
                "fleet", coord_id="cccccc01", cwd="/repo",
                cap=2, fleet_home=str(fleet_home),
            )


# ---------- parse error ----------


def test_tick_skips_on_parse_error(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder,
) -> None:
    # tasks.md with bad schema.
    (project_dir / "tasks.md").write_text("---\nschema: v999\n---\n")
    result = loop.tick(
        "fleet", coord_id="cccccc01", cwd="/repo",
        fleet_home=str(fleet_home),
    )
    assert result.skipped is True
    assert result.reason == "parse-error"
    assert result.errors


# ---------- archive scan idempotency ----------


def test_tick_does_not_re_drain_seen_archive(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess,
) -> None:
    _write_tasks(project_dir, [
        _make_task("a-aaaa", status="in-progress", worker_pid=99999),
    ])
    coord = "cccccc01"
    _write_archive(
        fleet_home, coord, "20260506-120000Z",
        "TASK_DONE_PR=a-aaaa https://example.com/pr/1\n",
    )
    with patch.object(loop, "_pid_alive", return_value=True):
        first = loop.tick(
            "fleet", coord_id=coord, cwd="/repo",
            fleet_home=str(fleet_home),
        )
        assert first.drained == 1
        # Re-write the task so it goes back to in-progress for tick 2
        # (in real life the fleet CLI would already have moved status to
        # in-review; we don't have a real CLI here).
        before = len(fleet_run_recorder)
        second = loop.tick(
            "fleet", coord_id=coord, cwd="/repo",
            fleet_home=str(fleet_home),
        )
    # Tick 2 must NOT re-drain the same archive file.
    assert second.drained == 0
    # No new fleet calls for the already-drained sentinel.
    after = fleet_run_recorder[before:]
    assert all("a-aaaa" not in str(c) or "set" not in c[1:3] or "pr_url=" not in c[-1] for c in after)


# ---------- coord-state.json heartbeat (issue #50) ----------


def test_tick_writes_coord_state_even_on_empty_dispatch(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess,
) -> None:
    """Empty tick (no ready tasks, no inbox sentinels) must still
    refresh coord-state.json — the Variant A dashboard reads this
    file's mtime as the per-tick heartbeat. Gating the write on
    last_seen (issue #50) made empty + dispatch-only ticks invisible
    to the TUI as `○ idle · auto-stopped`.
    """
    # No ready tasks → dispatch path is a no-op. No archive files →
    # drain path is a no-op. The tick is otherwise an empty cycle.
    _write_tasks(project_dir, [_make_task("idle-aaaa", status="todo")])
    state_path = project_dir / "coord-state.json"
    assert not state_path.exists()

    result = loop.tick(
        "fleet", coord_id="cccccc01", cwd="/repo",
        fleet_home=str(fleet_home),
    )

    assert not result.skipped
    assert result.dispatched == 0
    assert result.drained == 0
    # Regression gate for #50: the file MUST appear after a tick, even
    # with nothing to apply.
    assert state_path.exists()


def test_tick_writes_coord_state_on_dispatch_only(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess,
) -> None:
    """Dispatch-only tick (one ready task, no inbox sentinels) must
    refresh coord-state.json. Before the fix, _save_coord_state only
    fired when last_seen was set on the drain path, so a fresh project
    that just dispatched its first worker showed `○ idle` until the
    first sentinel landed.
    """
    _write_tasks(project_dir, [_make_task("ready-aaaa", status="ready")])
    dispatch_subprocess.append("abcdef01")
    state_path = project_dir / "coord-state.json"
    assert not state_path.exists()

    result = loop.tick(
        "fleet", coord_id="cccccc01", cwd="/repo",
        fleet_home=str(fleet_home),
    )

    assert not result.skipped
    assert result.dispatched == 1
    assert result.drained == 0
    # File written even though no sentinel was drained.
    assert state_path.exists()


# ---------- sentinel grammar ----------


def test_drain_archive_one_sentinel_per_file(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess,
) -> None:
    """ENG §6.3 contract: one sentinel per file. If an archive file
    has multiple sentinel-shaped lines (operator narrative drift, or
    a malformed delivery), only the first one applies.
    """
    _write_tasks(project_dir, [
        _make_task("a-aaaa", status="in-progress", worker_pid=99999),
        _make_task("b-bbbb", status="in-progress", worker_pid=99998),
    ])
    coord = "cccccc01"
    # File contains two sentinels (only the first should fire).
    _write_archive(
        fleet_home, coord, "20260506-120000Z",
        "TASK_DONE_PR=a-aaaa https://x/y/pull/1\n"
        "TASK_DONE_PR=b-bbbb https://x/y/pull/2\n",
    )
    with patch.object(loop, "_pid_alive", return_value=True):
        result = loop.tick(
            "fleet", coord_id=coord, cwd="/repo",
            fleet_home=str(fleet_home),
        )
    # First sentinel applied, second ignored.
    assert result.drained == 1
    set_calls = [c for c in fleet_run_recorder if c[1:3] == ["tasks", "set"]]
    a_calls = [c for c in set_calls if "a-aaaa" in c]
    b_calls = [c for c in set_calls if "b-bbbb" in c]
    assert any("pull/1" in (c[-1] if c else "") for c in a_calls)
    assert b_calls == []


def test_gh_pr_checks_runs_both_checks_and_view(monkeypatch) -> None:
    """ENG §9.4 reconcile relies on merged + mergeable signals — they
    require gh pr view, not just gh pr checks. Earlier code queried
    only checks and left both fields hardcoded.
    """
    calls: list[list[str]] = []

    def fake_run(cmd, capture_output=True, text=True, timeout=None, check=False):
        calls.append(list(cmd))
        if "checks" in cmd:
            return subprocess.CompletedProcess(
                args=cmd, returncode=0,
                stdout='[{"state":"COMPLETED","conclusion":"SUCCESS"}]',
                stderr="",
            )
        if "view" in cmd:
            return subprocess.CompletedProcess(
                args=cmd, returncode=0,
                stdout='{"state":"MERGED","mergeable":"MERGEABLE"}',
                stderr="",
            )
        return subprocess.CompletedProcess(args=cmd, returncode=2, stdout="", stderr="unknown")

    monkeypatch.setattr(loop.subprocess, "run", fake_run)
    res = loop._gh_pr_checks("https://github.com/x/y/pull/1")
    assert res.error == ""
    assert res.all_green is True
    assert res.merged is True
    assert res.mergeable is True
    # Both subcommands invoked.
    assert any("checks" in c for c in calls)
    assert any("view" in c for c in calls)


def test_gh_pr_checks_detects_conflicting(monkeypatch) -> None:
    def fake_run(cmd, capture_output=True, text=True, timeout=None, check=False):
        if "checks" in cmd:
            return subprocess.CompletedProcess(
                args=cmd, returncode=0, stdout="[]", stderr="",
            )
        return subprocess.CompletedProcess(
            args=cmd, returncode=0,
            stdout='{"state":"OPEN","mergeable":"CONFLICTING"}',
            stderr="",
        )

    monkeypatch.setattr(loop.subprocess, "run", fake_run)
    res = loop._gh_pr_checks("https://github.com/x/y/pull/1")
    assert res.mergeable is False
    assert res.merged is False


def test_gh_pr_checks_unknown_mergeable_treated_as_mergeable(monkeypatch) -> None:
    """gh's mergeable=UNKNOWN is transient; don't trigger a rebase on it."""

    def fake_run(cmd, capture_output=True, text=True, timeout=None, check=False):
        if "checks" in cmd:
            return subprocess.CompletedProcess(args=cmd, returncode=0, stdout="[]", stderr="")
        return subprocess.CompletedProcess(
            args=cmd, returncode=0,
            stdout='{"state":"OPEN","mergeable":"UNKNOWN"}', stderr="",
        )

    monkeypatch.setattr(loop.subprocess, "run", fake_run)
    res = loop._gh_pr_checks("https://github.com/x/y/pull/1")
    assert res.mergeable is True


def test_gh_pr_checks_propagates_view_error(monkeypatch) -> None:
    """Checks succeeds but view fails → caller must see error and skip
    rather than misroute on a missing mergeable signal."""

    def fake_run(cmd, capture_output=True, text=True, timeout=None, check=False):
        if "checks" in cmd:
            return subprocess.CompletedProcess(args=cmd, returncode=0, stdout="[]", stderr="")
        return subprocess.CompletedProcess(
            args=cmd, returncode=1, stdout="", stderr="not found",
        )

    monkeypatch.setattr(loop.subprocess, "run", fake_run)
    res = loop._gh_pr_checks("https://github.com/x/y/pull/1")
    assert "not found" in res.error


def test_apply_dispatch_orders_status_branch_state_pid_note(
    fleet_run_recorder,
) -> None:
    """status flip FIRST (defeats duplicate-dispatch on partial
    failure), then branch, state.json bootstrap, worker_pid, note.

    Codex iter-4 [P1] regress: an earlier ordering bootstrapped
    state.json BEFORE the status flip. If the status flip raised
    after bootstrap succeeded, tasks.md still said "ready" and the
    next tick's _dispatch_ready picked the task up again — running
    a second tmux session for the same slug. Status-first kills
    that race because the moment status=in-progress is durable,
    _dispatch_ready filters the task out.

    Every mutation must also pass `--project <project>` so the CLI's
    cwd-default project resolution can't accidentally write to a
    sibling project's tasks.md (Phase D codex find: project drift
    when ProjectTag(cwd) != coord's project).
    """
    action = loop._DispatchAction(
        slug="ready-aaaa", agent_id="abcdef01", branch="worker/ready-aaaa",
    )
    loop._apply_dispatch(action, "fleet-proj", "fleet")
    # Five calls in order: status, branch, workers update bootstrap,
    # worker_pid, note.
    assert len(fleet_run_recorder) == 5
    assert "status=in-progress" in fleet_run_recorder[0]
    assert fleet_run_recorder[1][1:3] == ["tasks", "set"]
    assert "branch=" in fleet_run_recorder[1][-1]
    assert fleet_run_recorder[2][1:3] == ["workers", "update"]
    assert "--phase" in fleet_run_recorder[2]
    assert "starting" in fleet_run_recorder[2]
    assert fleet_run_recorder[3][1:3] == ["tasks", "set"]
    assert fleet_run_recorder[3][-1].startswith("worker_pid=")
    # The PID written must be the live coord's PID — non-zero, not
    # the agent_id's hex value, not 0. Tested via parse-back.
    pid_str = fleet_run_recorder[3][-1].split("=", 1)[1]
    assert int(pid_str) > 0, f"worker_pid must be live PID, got {pid_str!r}"
    assert fleet_run_recorder[4][1:3] == ["tasks", "note"]
    # Every call carries --project to defeat cwd-default drift.
    for call in fleet_run_recorder:
        assert "--project" in call, f"missing --project in {call}"
        assert "fleet-proj" in call, f"wrong project in {call}"


def test_is_worker_alive_reads_state_json_freshness(
    fleet_home: Path, project_dir: Path,
    monkeypatch,
) -> None:
    """Codex full-stack [P1] regress: a worker whose tasks.md worker_pid
    is dead but whose state.json was just updated must still count as
    alive — otherwise every coord-dispatched task gets requeued before
    the worker can finish even one phase.

    The fix: _is_worker_alive falls through to _worker_state_fresh,
    which checks workers/<slug>/state.json. Fresh updated_at →
    presumed alive; stale or terminal phase → presumed dead.
    """
    monkeypatch.setenv("FLEET_HOME", str(fleet_home))
    project = "fleet"
    workers_dir = fleet_home / "projects" / project / "workers" / "alpha-1234"
    workers_dir.mkdir(parents=True, exist_ok=True)

    fresh = _dt.datetime.now(tz=_dt.timezone.utc).isoformat().replace(
        "+00:00", "Z",
    )
    (workers_dir / "state.json").write_text(json.dumps({
        "slug": "alpha-1234", "project": project,
        "phase": "tdd-red", "updated_at": fresh,
    }), encoding="utf-8")

    t = _make_task("alpha-1234", status="in-progress", worker_pid=99999)
    # PID 99999 is dead in this test env, but state.json is fresh →
    # _is_worker_alive returns True via the state.json fallback.
    with patch.object(loop, "_pid_alive", return_value=False):
        assert loop._is_worker_alive(t, project) is True


def test_is_worker_alive_treats_stale_state_json_as_dead(
    fleet_home: Path, project_dir: Path,
    monkeypatch,
) -> None:
    """Stale state.json (last update > _WORKER_STATE_FRESH_S ago) does
    NOT count as alive — the worker has wedged or crashed silently."""
    monkeypatch.setenv("FLEET_HOME", str(fleet_home))
    project = "fleet"
    workers_dir = fleet_home / "projects" / project / "workers" / "alpha-9999"
    workers_dir.mkdir(parents=True, exist_ok=True)

    stale = _dt.datetime(2020, 1, 1, tzinfo=_dt.timezone.utc).isoformat().replace(
        "+00:00", "Z",
    )
    (workers_dir / "state.json").write_text(json.dumps({
        "slug": "alpha-9999", "project": project,
        "phase": "tdd-red", "updated_at": stale,
    }), encoding="utf-8")

    t = _make_task("alpha-9999", status="in-progress", worker_pid=99999)
    with patch.object(loop, "_pid_alive", return_value=False):
        assert loop._is_worker_alive(t, project) is False


def test_reconcile_phase_done_with_state_pr_url_flips_to_in_review(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess,
    monkeypatch,
) -> None:
    """Codex full-stack iter-2 [P1] regress: a worker that finishes
    cleanly via `fleet workers update --phase done --pr-url X` and
    exits would otherwise get classified as "died without PR" by
    reconcile (because tasks.md.pr_url is empty until reconcile
    transcribes it). The fix: reconcile reads state.json's terminal
    phase + pr_url and flips status to in-review with the URL set."""
    monkeypatch.setenv("FLEET_HOME", str(fleet_home))
    project = "fleet"
    workers_dir = fleet_home / "projects" / project / "workers" / "shipper-aaaa"
    workers_dir.mkdir(parents=True, exist_ok=True)
    fresh = _dt.datetime.now(tz=_dt.timezone.utc).isoformat().replace(
        "+00:00", "Z",
    )
    (workers_dir / "state.json").write_text(json.dumps({
        "slug": "shipper-aaaa", "project": project,
        "phase": "done", "updated_at": fresh,
        "pr_url": "https://github.com/x/y/pull/7",
    }), encoding="utf-8")

    _write_tasks(project_dir, [
        _make_task(
            "shipper-aaaa", status="in-progress",
            worker_pid=99999, pr_url="",
        ),
    ])
    with patch.object(loop, "_pid_alive", return_value=False):
        result = loop.tick(
            project, coord_id="cccccc01", cwd="/repo",
            fleet_home=str(fleet_home),
        )

    assert result.reconciled == 1
    set_calls = [c for c in fleet_run_recorder if c[1:3] == ["tasks", "set"]]
    assert any("status=in-review" in c for c in set_calls)
    assert any(
        "pr_url=https://github.com/x/y/pull/7" in c[-1]
        for c in set_calls
    ), f"pr_url not transcribed onto tasks.md: {set_calls}"


def test_reconcile_in_review_does_not_re_apply_terminal_phase(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess,
    monkeypatch,
) -> None:
    """Codex iter-3 [P1] regress: a stale state.json with phase=done
    must NOT keep re-flipping a task already at status=in-review back
    to in-review every tick. The terminal-phase branch is gated to
    status=in-progress so subsequent ticks (already in-review) drive
    the gh pr checks → done lifecycle instead of short-circuiting it.
    """
    monkeypatch.setenv("FLEET_HOME", str(fleet_home))
    project = "fleet"
    workers_dir = fleet_home / "projects" / project / "workers" / "settled-aaaa"
    workers_dir.mkdir(parents=True, exist_ok=True)
    fresh = _dt.datetime.now(tz=_dt.timezone.utc).isoformat().replace(
        "+00:00", "Z",
    )
    (workers_dir / "state.json").write_text(json.dumps({
        "slug": "settled-aaaa", "project": project,
        "phase": "done", "updated_at": fresh,
        "pr_url": "https://github.com/x/y/pull/9",
    }), encoding="utf-8")

    # Task is ALREADY in-review with the PR URL set; CI says merged.
    _write_tasks(project_dir, [
        _make_task(
            "settled-aaaa", status="in-review",
            worker_pid=99999, pr_url="https://github.com/x/y/pull/9",
        ),
    ])
    merged = loop._CIResult(all_green=True, merged=True, mergeable=True)
    with patch.object(loop, "_pid_alive", return_value=False), \
         patch.object(loop, "_gh_pr_checks", return_value=merged):
        result = loop.tick(
            project, coord_id="cccccc01", cwd="/repo",
            fleet_home=str(fleet_home),
        )

    assert result.reconciled == 1
    set_calls = [c for c in fleet_run_recorder if c[1:3] == ["tasks", "set"]]
    # The CI-driven done flip must run; the terminal-state branch
    # must NOT have fired (which would have re-flipped to in-review).
    assert any("status=done" in c for c in set_calls), (
        f"merged PR should advance to done, calls: {set_calls}"
    )
    assert not any("status=in-review" in c for c in set_calls), (
        f"in-review task should not re-flip to in-review, calls: {set_calls}"
    )


def test_reconcile_ci_red_clears_pr_url_for_retry(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess,
) -> None:
    """Codex iter-3 [P2] regress: when CI is red and the task gets
    requeued for retry, the stale pr_url must be cleared so the
    re-dispatched worker's NEW PR becomes the next reconcile target.
    Without this, every subsequent tick re-polls the dead failed PR
    forever."""
    _write_tasks(project_dir, [
        _make_task(
            "redci-aaaa", status="in-review",
            worker_pid=1, pr_url="https://github.com/x/y/pull/3",
        ),
    ])
    failed = loop._CIResult(failed=True, mergeable=True)
    with patch.object(loop, "_pid_alive", return_value=False), \
         patch.object(loop, "_gh_pr_checks", return_value=failed):
        result = loop.tick(
            "fleet", coord_id="cccccc01", cwd="/repo",
            fleet_home=str(fleet_home),
        )

    assert result.reconciled == 1
    set_calls = [c for c in fleet_run_recorder if c[1:3] == ["tasks", "set"]]
    # Both status=todo AND pr_url= (clear) must fire.
    assert any("status=todo" in c for c in set_calls)
    assert any(
        c[-1] == "pr_url=" for c in set_calls
    ), f"CI-red retry must clear pr_url, calls: {set_calls}"


def test_reconcile_phase_blocked_with_reason_flips_to_blocked(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess,
    monkeypatch,
) -> None:
    """phase=blocked + blocked_reason in state.json → reconcile flips
    status to blocked + raises to operator. Without this, a stuck
    worker that exited gets requeued to todo silently."""
    monkeypatch.setenv("FLEET_HOME", str(fleet_home))
    project = "fleet"
    workers_dir = fleet_home / "projects" / project / "workers" / "stuck-aaaa"
    workers_dir.mkdir(parents=True, exist_ok=True)
    fresh = _dt.datetime.now(tz=_dt.timezone.utc).isoformat().replace(
        "+00:00", "Z",
    )
    (workers_dir / "state.json").write_text(json.dumps({
        "slug": "stuck-aaaa", "project": project,
        "phase": "blocked", "updated_at": fresh,
        "blocked_reason": "API key missing",
    }), encoding="utf-8")

    _write_tasks(project_dir, [
        _make_task("stuck-aaaa", status="in-progress", worker_pid=99999),
    ])
    with patch.object(loop, "_pid_alive", return_value=False):
        result = loop.tick(
            project, coord_id="cccccc01", cwd="/repo",
            fleet_home=str(fleet_home),
        )

    assert result.reconciled == 1
    assert result.raised == 1
    set_calls = [c for c in fleet_run_recorder if c[1:3] == ["tasks", "set"]]
    assert any("status=blocked" in c for c in set_calls)
    note_calls = [c for c in fleet_run_recorder if c[1:3] == ["tasks", "note"]]
    assert any("API key missing" in c[-1] for c in note_calls)


def test_is_worker_alive_terminal_phase_is_not_alive(
    fleet_home: Path, project_dir: Path,
    monkeypatch,
) -> None:
    """phase=done|blocked|failed always counts as not-alive even if
    updated_at is fresh — terminal phases mean the worker is gone."""
    monkeypatch.setenv("FLEET_HOME", str(fleet_home))
    project = "fleet"
    for phase in ("done", "blocked", "failed"):
        slug = f"term-{phase}-aaaa"
        workers_dir = fleet_home / "projects" / project / "workers" / slug
        workers_dir.mkdir(parents=True, exist_ok=True)
        fresh = _dt.datetime.now(tz=_dt.timezone.utc).isoformat().replace(
            "+00:00", "Z",
        )
        (workers_dir / "state.json").write_text(json.dumps({
            "slug": slug, "project": project,
            "phase": phase, "updated_at": fresh,
        }), encoding="utf-8")
        t = _make_task(slug, status="in-progress", worker_pid=99999)
        with patch.object(loop, "_pid_alive", return_value=False):
            assert loop._is_worker_alive(t, project) is False, phase


def test_parse_sentinel_known_kinds() -> None:
    cases = [
        ("TASK_DONE_PR=alpha-aaaa https://x/y/1", "task_done_pr", "alpha-aaaa"),
        ("BLOCKED_QUESTION=alpha-aaaa Why?", "blocked_question", "alpha-aaaa"),
        ("WORKER_FAILED=alpha-aaaa Disk full", "worker_failed", "alpha-aaaa"),
        ("NEW_TASK=new-aaaa", "new_task", "new-aaaa"),
    ]
    for line, kind, slug in cases:
        s = loop._parse_sentinel(line)
        assert s is not None, line
        assert s.kind == kind
        assert s.slug == slug


def test_parse_sentinel_ignores_narrative() -> None:
    for line in ("Everything is fine.", "[OPERATOR] hi", ""):
        assert loop._parse_sentinel(line) is None


# ---------- entry point ----------


def test_main_with_no_project_returns_zero(capsys) -> None:
    rc = loop.main([])
    assert rc == 0
    captured = capsys.readouterr()
    assert "no project set" in captured.out


def test_main_writes_json_summary_on_run(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, monkeypatch, capsys,
) -> None:
    _write_tasks(project_dir, [_make_task("ready-aaaa")])
    monkeypatch.setenv("FLEET_HOME", str(fleet_home))
    monkeypatch.setenv("FLEET_AGENT_ID", "cccccc01")
    monkeypatch.setenv("FLEET_PROJECT", "fleet")
    # Patch dispatch's subprocess to satisfy the dispatch chain.
    fake = subprocess.CompletedProcess(
        args=[], returncode=0,
        stdout="agent abcdef01 dispatched\n", stderr="",
    )

    def fake_run(cmd, capture_output=True, text=True, timeout=None, check=False):
        return fake

    monkeypatch.setattr(dispatch.subprocess, "run", fake_run)
    rc = loop.main([])
    assert rc == 0
    out = capsys.readouterr().out.strip()
    parsed = json.loads(out)
    assert parsed["dispatched"] == 1
    assert parsed["skipped"] is False


# ---------- coord_id published into coordinator.lock body (issue #55) ----------


def test_try_lock_writes_holder_id_into_body(tmp_path: Path) -> None:
    """_try_lock writes <coord_id>\\n into the lock body so the Go-side
    dashboard can identify which agent record is the project's coord.

    Lock-body publication is the v0.2 issue-#55 mechanism: the LEFT
    column of the ops console pulls the holder ID from the lock body
    and looks up the matching agent record to render coord-on-project.
    """
    lock_path = tmp_path / "coordinator.lock"
    fd = loop._try_lock(lock_path, holder_id="cafef00d")
    assert fd is not None
    try:
        body = lock_path.read_bytes()
        assert body == b"cafef00d\n", f"unexpected lock body: {body!r}"
    finally:
        fcntl.flock(fd, fcntl.LOCK_UN)
        os.close(fd)


def test_try_lock_no_holder_id_leaves_body_empty(tmp_path: Path) -> None:
    """When holder_id is empty (legacy callers, or coord_id unset), the
    lock file body must remain empty. The Go-side reader treats empty
    body as "unknown holder" and renders ○ no coord.
    """
    lock_path = tmp_path / "coordinator.lock"
    fd = loop._try_lock(lock_path)  # default holder_id=""
    assert fd is not None
    try:
        body = lock_path.read_bytes()
        assert body == b"", f"expected empty body, got: {body!r}"
    finally:
        fcntl.flock(fd, fcntl.LOCK_UN)
        os.close(fd)


def test_try_lock_truncates_previous_holder_body(tmp_path: Path) -> None:
    """A prior holder may have written a longer ID into the body. The
    new holder must truncate before writing so trailing bytes from the
    old holder don't shadow the new one.
    """
    lock_path = tmp_path / "coordinator.lock"
    # Pre-seed with a longer body that the new holder must overwrite.
    lock_path.write_bytes(b"deadbeefdeadbeef\n")
    fd = loop._try_lock(lock_path, holder_id="aaaa1111")
    assert fd is not None
    try:
        body = lock_path.read_bytes()
        assert body == b"aaaa1111\n", f"unexpected lock body: {body!r}"
    finally:
        fcntl.flock(fd, fcntl.LOCK_UN)
        os.close(fd)


def test_try_lock_returns_none_when_already_held(tmp_path: Path) -> None:
    """A second _try_lock on a held lock returns None (LOCK_NB
    semantics). Body publication must NOT clobber the live holder's
    body when the second call fails.
    """
    lock_path = tmp_path / "coordinator.lock"
    fd1 = loop._try_lock(lock_path, holder_id="aaaa1111")
    assert fd1 is not None
    try:
        body_after_first = lock_path.read_bytes()
        # Second acquisition fails — must not touch the body.
        fd2 = loop._try_lock(lock_path, holder_id="bbbb2222")
        assert fd2 is None
        body_after_second = lock_path.read_bytes()
        assert body_after_second == body_after_first
        assert body_after_second == b"aaaa1111\n"
    finally:
        fcntl.flock(fd1, fcntl.LOCK_UN)
        os.close(fd1)


def test_tick_publishes_coord_id_in_lock_body(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess,
) -> None:
    """End-to-end: a tick() with non-empty coord_id leaves the project's
    coordinator.lock with that ID in its body, so the dashboard can read
    it on the next refresh.
    """
    _write_tasks(project_dir, [_make_task("ready-bbbb", status="ready")])
    dispatch_subprocess.append("dddddd02")

    loop.tick(
        "fleet", coord_id="cccccc01", cwd="/repo",
        fleet_home=str(fleet_home),
    )

    lock_path = project_dir / ".locks" / "coordinator.lock"
    assert lock_path.exists()
    assert lock_path.read_bytes() == b"cccccc01\n"
