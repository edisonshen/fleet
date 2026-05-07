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


def test_apply_dispatch_orders_status_branch_note(
    fleet_run_recorder,
) -> None:
    """status flip first, branch second, note last — for crash-safety.

    Every mutation must also pass `--project <project>` so the CLI's
    cwd-default project resolution can't accidentally write to a
    sibling project's tasks.md (Phase D codex find: project drift
    when ProjectTag(cwd) != coord's project).
    """
    action = loop._DispatchAction(
        slug="ready-aaaa", agent_id="abcdef01", branch="worker/ready-aaaa",
    )
    loop._apply_dispatch(action, "fleet-proj", "fleet")
    # Three calls in order.
    assert len(fleet_run_recorder) == 3
    assert "status=in-progress" in fleet_run_recorder[0]
    assert fleet_run_recorder[1][1:3] == ["tasks", "set"]
    assert "branch=" in fleet_run_recorder[1][-1]
    assert fleet_run_recorder[2][1:3] == ["tasks", "note"]
    # Every call carries --project to defeat cwd-default drift.
    for call in fleet_run_recorder:
        assert "--project" in call, f"missing --project in {call}"
        assert "fleet-proj" in call, f"wrong project in {call}"


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
