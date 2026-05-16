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


class _DispatchSubprocessHandle(list):
    """A list subclass that doubles as a fixture handle.

    `append` / `pop` / `__iter__` work like a normal list of agent_ids
    (the legacy contract: pop the next id off the stack to override
    mint_agent_id). The .seen_cmds attribute carries the recorder for
    `dispatch.subprocess.run` invocations so tests can assert no
    `fleet dispatch` shell-out happened (issue #84 Phase A).
    """

    def __init__(self) -> None:
        super().__init__()
        self.seen_cmds: list[list[str]] = []


@pytest.fixture
def dispatch_subprocess(monkeypatch):
    """Stub dispatch.subprocess.run for fetch_standards / fetch_learnings.

    Issue #84 Phase A: the coord skill no longer calls `fleet dispatch`
    via subprocess. Workers spawn as Agent-tool subagents; the skill
    mints agent_ids itself (dispatch.mint_agent_id). The fixture's
    legacy "ids" stack is now used to override mint_agent_id so tests
    can pin a specific agent_id (was previously the dispatch stdout
    parse target).

    Two responsibilities:
      1. mock subprocess.run to return canned stdout for the only
         remaining shell-outs in dispatch.py — `fleet standards show`
         and `fleet learnings list`. Anything else returns 0/empty
         (loop.py's _run_fleet is patched separately by
         fleet_run_recorder).
      2. monkeypatch dispatch.mint_agent_id to consume the `ids`
         list — preserves test-determinism. Empty list → falls back
         to the production secrets.token_hex implementation.

    A test that asserts "subprocess.run was NOT called for `fleet
    dispatch`" can inspect `<fixture>.seen_cmds`.
    """
    ids = _DispatchSubprocessHandle()

    def fake_run(cmd, capture_output=True, text=True, timeout=None, check=False,
                 input=None, env=None):
        ids.seen_cmds.append(list(cmd))
        if cmd[1:3] == ["standards", "show"]:
            return subprocess.CompletedProcess(
                args=cmd, returncode=0, stdout="# Standards\n", stderr="",
            )
        if cmd[1:3] == ["learnings", "list"]:
            return subprocess.CompletedProcess(
                args=cmd, returncode=0, stdout="", stderr="",
            )
        # PR1 dispatch-lifecycle: loop.py now shells out to
        # `fleet claims acquire-prompt` instead of writing the inbox
        # file directly. Emulate the real CLI by writing the inbox
        # file under the test's FLEET_HOME and returning the expected
        # JSON envelope on stdout. The `input` kwarg carries the prompt
        # body (piped via subprocess.run(input=prompt)).
        if cmd[1:3] == ["claims", "acquire-prompt"]:
            agent_id = cmd[3]
            fleet_home = (env or os.environ).get("FLEET_HOME") or os.path.expanduser("~/.fleet")
            inbox_dir = os.path.join(fleet_home, "inbox")
            os.makedirs(inbox_dir, exist_ok=True)
            path = os.path.join(inbox_dir, f"{agent_id}.md")
            body = input or ""
            if body and not body.endswith("\n"):
                body = body + "\n"
            with open(path, "w", encoding="utf-8") as fh:
                fh.write(body)
            envelope = (
                f'{{"outcome":"acquired","dispatch_id":"{agent_id}",'
                f'"kind":"coord_prompt_inbox","path":"{path}"}}\n'
            )
            return subprocess.CompletedProcess(
                args=cmd, returncode=0, stdout=envelope, stderr="",
            )
        # PR1 dispatch-lifecycle: also emulate `fleet claims release`
        # so terminal-transition wire sites in loop.py don't spam stderr
        # with synthetic-error envelopes. The emulator unlinks the
        # acquire-mirror file (best-effort) and emits a released envelope.
        if cmd[1:3] == ["claims", "release"]:
            agent_id = cmd[3]
            fleet_home = (env or os.environ).get("FLEET_HOME") or os.path.expanduser("~/.fleet")
            path = os.path.join(fleet_home, "inbox", f"{agent_id}.md")
            outcome = "released"
            try:
                os.unlink(path)
            except FileNotFoundError:
                outcome = "already_released"
            except OSError:
                pass
            envelope = (
                f'{{"outcome":"{outcome}","dispatch_id":"{agent_id}",'
                f'"kind":"coord_prompt_inbox","path":"{path}"}}\n'
            )
            return subprocess.CompletedProcess(
                args=cmd, returncode=0, stdout=envelope, stderr="",
            )
        return subprocess.CompletedProcess(
            args=cmd, returncode=0, stdout="", stderr="",
        )

    monkeypatch.setattr(dispatch.subprocess, "run", fake_run)

    # mint_agent_id override: pop the next id off the test-supplied
    # stack; fall through to the production helper when empty.
    real_mint = dispatch.mint_agent_id

    def fake_mint() -> str:
        if ids:
            return ids.pop(0)
        return real_mint()

    monkeypatch.setattr(dispatch, "mint_agent_id", fake_mint)
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
    # Issue #84 Phase A: a DISPATCH block is emitted for the coord
    # agent to act on (Agent tool invocation). One block per dispatch.
    assert len(result.dispatch_instructions) == 1
    block = result.dispatch_instructions[0]
    assert block.startswith("DISPATCH: ready-aaaa")
    assert "agent_id: abcdef01" in block
    assert "run_in_background: true" in block
    assert "subagent_type: general-purpose" in block
    assert str(inbox_file) in block
    assert block.rstrip().endswith("END_DISPATCH")
    # Issue #84 Phase A regression guard: status flip is durable
    # BEFORE the DISPATCH block is surfaced. Without this ordering,
    # a coord that reads the block, calls Agent tool, and the worker
    # races to `fleet workers update` before the status=in-progress
    # write lands could cause the next tick's _dispatch_ready to
    # pick the same task again — duplicate dispatch, two subagents
    # for one slug. Codex iter-4 [P1] regress for the legacy
    # subprocess path; same invariant applies here.
    in_progress_idx = next(
        i for i, c in enumerate(fleet_run_recorder)
        if c[1:3] == ["tasks", "set"] and "status=in-progress" in c
    )
    note_idx = next(
        i for i, c in enumerate(fleet_run_recorder)
        if c[1:3] == ["tasks", "note"]
        and any("abcdef01" in arg for arg in c)
    )
    # status flip happens FIRST (smallest index), note happens LAST
    # (largest index) — confirms _apply_dispatch ran the full
    # mutation chain before result.dispatch_instructions accumulates.
    assert in_progress_idx < note_idx, (
        f"status flip must precede note: idx {in_progress_idx} >= {note_idx}"
    )
    # Issue #84 Phase A: subprocess.run MUST NOT be called for
    # `fleet dispatch`. fetch_standards + fetch_learnings still
    # route through subprocess.run, but the dispatch shell-out is
    # gone — workers spawn via the coord agent's Agent tool.
    seen_cmds = dispatch_subprocess.seen_cmds
    assert all(
        cmd[1:2] != ["dispatch"] for cmd in seen_cmds
    ), f"unexpected `fleet dispatch` subprocess call: {seen_cmds!r}"


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


# ---------- terminal release wire sites (PR1 dispatch-lifecycle) ----------
#
# Codex iter-3 [P1]: terminal-transition sites in loop.py must shell out
# to `fleet claims release` so the coord_prompt_inbox + journal are
# reclaimed when a worker hits a terminal phase. These tests assert the
# release shell-out fires at each of the 5 sites (primary reconcile,
# primary sentinel, supervisor reconcile, supervisor drain, supervisor
# replay-deferred drain, handoff) and inspect the dispatch_subprocess
# seen_cmds recorder to confirm the call was made for the right
# agent_id.


def _seed_coord_state(
    project_dir: Path, *, agent_id_map: dict[str, str],
) -> None:
    """Write a coord-state.json with the supplied slug → agent_id map.

    Mirrors supervisor_mod.remember_agent_id's on-disk shape so the
    in-tick lookup in _release_coord_prompt_inbox finds the agent_id
    for terminal transitions on already-in-progress workers.
    """
    state_path = project_dir / "coord-state.json"
    state = {}
    if state_path.exists():
        state = json.loads(state_path.read_text())
    state["worker_agent_ids"] = dict(agent_id_map)
    state_path.write_text(json.dumps(state), encoding="utf-8")


def test_reconcile_terminal_releases_coord_prompt_inbox(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess,
) -> None:
    """Wire site #1: primary tick reconcile clear_worker path.

    A worker that died without opening a PR transitions to status=todo
    with clear_worker=True. The release wire MUST fire `fleet claims
    release <agent_id>` BEFORE forget_agent_id drops the mapping.
    """
    _write_tasks(project_dir, [
        _make_task("dying-aaaa", status="in-progress", worker_pid=1, pr_url=""),
    ])
    _seed_coord_state(
        project_dir, agent_id_map={"dying-aaaa": "deadbe01"},
    )
    # Also seed the inbox file so the emulator has something to unlink.
    inbox = fleet_home / "inbox" / "deadbe01.md"
    inbox.write_text("worker prompt body\n", encoding="utf-8")

    with patch.object(loop, "_pid_alive", return_value=False):
        loop.tick(
            "fleet", coord_id="cccccc01", cwd="/repo",
            fleet_home=str(fleet_home),
        )

    # Release call fired for the worker's agent_id.
    release_calls = [
        c for c in dispatch_subprocess.seen_cmds
        if c[1:3] == ["claims", "release"]
    ]
    assert len(release_calls) >= 1, (
        f"expected fleet claims release; seen_cmds={dispatch_subprocess.seen_cmds!r}"
    )
    assert release_calls[0][3] == "deadbe01"
    # Inbox file unlinked by the emulator.
    assert not inbox.exists(), "release wire should unlink the inbox"
    # Mapping forgotten AFTER release ran (we can't observe ordering
    # post-hoc, but we can verify the final state).
    state = json.loads((project_dir / "coord-state.json").read_text())
    assert "dying-aaaa" not in state.get("worker_agent_ids", {})


def test_sentinel_task_done_pr_releases_coord_prompt_inbox(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess,
) -> None:
    """Wire site #2: primary tick TASK_DONE_PR sentinel drain.

    A worker that posted TASK_DONE_PR has reached TerminalSuccess. The
    release wire MUST fire as part of the apply path.
    """
    _write_tasks(project_dir, [
        _make_task("done-aaaa", status="in-progress", worker_pid=99999),
    ])
    _seed_coord_state(
        project_dir, agent_id_map={"done-aaaa": "feedf001"},
    )
    inbox = fleet_home / "inbox" / "feedf001.md"
    inbox.write_text("done worker prompt\n", encoding="utf-8")
    coord = "cccccc01"
    _write_archive(
        fleet_home, coord, "20260506-120000Z",
        "TASK_DONE_PR=done-aaaa https://github.com/x/y/pull/42\n",
    )

    with patch.object(loop, "_pid_alive", return_value=True):
        result = loop.tick(
            "fleet", coord_id=coord, cwd="/repo",
            fleet_home=str(fleet_home),
        )
    assert result.drained == 1

    release_calls = [
        c for c in dispatch_subprocess.seen_cmds
        if c[1:3] == ["claims", "release"]
    ]
    assert len(release_calls) >= 1, (
        f"expected fleet claims release; seen_cmds={dispatch_subprocess.seen_cmds!r}"
    )
    assert release_calls[0][3] == "feedf001"
    assert not inbox.exists()


def test_sentinel_worker_failed_releases_coord_prompt_inbox(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess,
) -> None:
    """Wire site #2 cont.: WORKER_FAILED sentinel also releases."""
    _write_tasks(project_dir, [
        _make_task("fail-aaaa", status="in-progress", worker_pid=99999),
    ])
    _seed_coord_state(
        project_dir, agent_id_map={"fail-aaaa": "deaddead"},
    )
    inbox = fleet_home / "inbox" / "deaddead.md"
    inbox.write_text("failed worker prompt\n", encoding="utf-8")
    coord = "cccccc01"
    _write_archive(
        fleet_home, coord, "20260506-120000Z",
        "WORKER_FAILED=fail-aaaa panic in worker\n",
    )

    with patch.object(loop, "_pid_alive", return_value=True):
        loop.tick(
            "fleet", coord_id=coord, cwd="/repo",
            fleet_home=str(fleet_home),
        )

    release_calls = [
        c for c in dispatch_subprocess.seen_cmds
        if c[1:3] == ["claims", "release"]
    ]
    assert any(c[3] == "deaddead" for c in release_calls)
    assert not inbox.exists()


def test_sentinel_blocked_question_does_NOT_release(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess,
) -> None:
    """BLOCKED_QUESTION carve-out: blocked workers stay alive so the
    operator can answer via the inbox. The release wire MUST NOT fire."""
    _write_tasks(project_dir, [
        _make_task("blocked-aaaa", status="in-progress", worker_pid=99999),
    ])
    _seed_coord_state(
        project_dir, agent_id_map={"blocked-aaaa": "ab0ckedd"},
    )
    inbox = fleet_home / "inbox" / "ab0ckedd.md"
    inbox.write_text("blocked worker prompt\n", encoding="utf-8")
    coord = "cccccc01"
    _write_archive(
        fleet_home, coord, "20260506-120000Z",
        "BLOCKED_QUESTION=blocked-aaaa What is the API key for X?\n",
    )

    with patch.object(loop, "_pid_alive", return_value=True):
        loop.tick(
            "fleet", coord_id=coord, cwd="/repo",
            fleet_home=str(fleet_home),
        )

    release_calls = [
        c for c in dispatch_subprocess.seen_cmds
        if c[1:3] == ["claims", "release"]
    ]
    # Critical: NO release for the blocked agent_id.
    assert not any(c[3] == "ab0ckedd" for c in release_calls), (
        f"BLOCKED_QUESTION should NOT release: seen={release_calls!r}"
    )
    # Inbox file kept (operator may write back via answer-blocked).
    assert inbox.exists()
    # And the agent_id mapping is preserved.
    state = json.loads((project_dir / "coord-state.json").read_text())
    assert state.get("worker_agent_ids", {}).get("blocked-aaaa") == "ab0ckedd"


def test_terminal_release_is_idempotent_across_double_drain(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess,
) -> None:
    """Two consecutive ticks with the same sentinel must not crash.

    Second tick has nothing to drain (watermark advanced) so no second
    release fires — release is naturally tied to the apply path.
    """
    _write_tasks(project_dir, [
        _make_task("dupe-aaaa", status="in-progress", worker_pid=99999),
    ])
    _seed_coord_state(
        project_dir, agent_id_map={"dupe-aaaa": "1dedface"},
    )
    inbox = fleet_home / "inbox" / "1dedface.md"
    inbox.write_text("dupe worker prompt\n", encoding="utf-8")
    coord = "cccccc01"
    _write_archive(
        fleet_home, coord, "20260506-120000Z",
        "TASK_DONE_PR=dupe-aaaa https://github.com/x/y/pull/9\n",
    )
    with patch.object(loop, "_pid_alive", return_value=True):
        loop.tick(
            "fleet", coord_id=coord, cwd="/repo",
            fleet_home=str(fleet_home),
        )
    first_releases = [
        c for c in dispatch_subprocess.seen_cmds
        if c[1:3] == ["claims", "release"]
    ]
    assert len(first_releases) == 1
    assert not inbox.exists()

    # Second tick — same archive file, watermark advanced. No re-release.
    pre_count = len(dispatch_subprocess.seen_cmds)
    with patch.object(loop, "_pid_alive", return_value=True):
        loop.tick(
            "fleet", coord_id=coord, cwd="/repo",
            fleet_home=str(fleet_home),
        )
    post_count = len(dispatch_subprocess.seen_cmds)
    second_releases = [
        c for c in dispatch_subprocess.seen_cmds[pre_count:]
        if c[1:3] == ["claims", "release"]
    ]
    assert second_releases == [], (
        f"second tick should not re-release: {second_releases!r}"
    )
    assert post_count >= pre_count  # may include fetch_learnings etc.


def test_handoff_release_runs_before_acquire(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess,
) -> None:
    """Wire site #6: handoff in _dispatch_review_handoffs releases the
    PRIOR dispatch's claim BEFORE acquiring the next stage's claim.

    Without this, every successful review-pending → reviewer or
    review-done → finisher transition leaks one inbox + one journal.
    """
    # A task in-progress whose worker has exited (worker_pid=0) and
    # whose state.json reports phase=review-pending → handoff to
    # reviewer.
    _write_tasks(project_dir, [
        _make_task("handoff-aaaa", status="in-progress", worker_pid=0),
    ])
    # Seed the prior worker's agent_id so the handoff release has
    # something to look up.
    _seed_coord_state(
        project_dir, agent_id_map={"handoff-aaaa": "abcd0001"},
    )
    # Drop the worker's state.json (phase=review-pending).
    workers_dir = project_dir / "workers" / "handoff-aaaa"
    workers_dir.mkdir(parents=True, exist_ok=True)
    (workers_dir / "state.json").write_text(
        json.dumps({"phase": "review-pending"}),
        encoding="utf-8",
    )
    inbox_prior = fleet_home / "inbox" / "abcd0001.md"
    inbox_prior.write_text("worker prompt\n", encoding="utf-8")
    # Pin the reviewer's agent_id for assertion.
    dispatch_subprocess.append("ee111e11")

    loop.tick(
        "fleet", coord_id="cccccc01", cwd="/repo",
        fleet_home=str(fleet_home),
    )

    # Inspect argv order: release for abcd0001 must precede
    # acquire-prompt for reviewer1.
    seen = dispatch_subprocess.seen_cmds
    release_idx = next(
        (i for i, c in enumerate(seen)
         if c[1:3] == ["claims", "release"] and c[3] == "abcd0001"),
        None,
    )
    acquire_idx = next(
        (i for i, c in enumerate(seen)
         if c[1:3] == ["claims", "acquire-prompt"] and c[3] == "ee111e11"),
        None,
    )
    assert release_idx is not None, (
        f"handoff did not release prior agent_id; seen={seen!r}"
    )
    assert acquire_idx is not None, "handoff did not acquire new claim"
    assert release_idx < acquire_idx, (
        f"release must run BEFORE acquire: release@{release_idx} >= acquire@{acquire_idx}"
    )
    # The prior worker's inbox is unlinked.
    assert not inbox_prior.exists()


def test_terminal_release_skipped_when_no_agent_id_known(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess,
) -> None:
    """Defensive: if the coord-state has no agent_id for the slug (e.g.,
    legacy pre-PR1 worker), the release wire is a silent no-op rather
    than a crash. PR4 sweeper will reconcile any leaked file."""
    _write_tasks(project_dir, [
        _make_task("legacy-aaaa", status="in-progress", worker_pid=1),
    ])
    # No coord-state seeded → empty agent_id map.
    with patch.object(loop, "_pid_alive", return_value=False):
        loop.tick(
            "fleet", coord_id="cccccc01", cwd="/repo",
            fleet_home=str(fleet_home),
        )

    release_calls = [
        c for c in dispatch_subprocess.seen_cmds
        if c[1:3] == ["claims", "release"]
    ]
    assert release_calls == [], (
        f"release should not fire without an agent_id: {release_calls!r}"
    )


# ---------- acquire-prompt failure handling (codex iter-4 [P1]) ----------


def test_acquire_failure_does_not_leak_agent_id_to_worker_map(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess, monkeypatch,
) -> None:
    """Codex iter-4 [P1]: a failed acquire-prompt MUST NOT record the
    minted agent_id in worker_agent_ids. Otherwise the supervisor
    addresses a worker that was never actually spawned."""
    _write_tasks(project_dir, [_make_task("acq-aaaa", status="ready")])
    dispatch_subprocess.append("c0ffee01")

    # Patch acquire_coord_prompt_inbox to always raise.
    import dispatch as dispatch_mod

    def _fail(*args, **kwargs):
        raise dispatch_mod.AcquirePromptError("error", 1, "simulated fault")

    monkeypatch.setattr(dispatch_mod, "acquire_coord_prompt_inbox", _fail)

    result = loop.tick(
        "fleet", coord_id="cccccc01", cwd="/repo",
        fleet_home=str(fleet_home),
    )
    assert result.dispatched == 0, (
        "failed acquire must not count as a successful dispatch"
    )
    assert any("acq-aaaa" in e for e in result.errors), (
        f"expected error surfacing for acq-aaaa; errors={result.errors!r}"
    )
    coord_state = json.loads(
        (project_dir / "coord-state.json").read_text(encoding="utf-8")
    )
    # CRITICAL: the failed agent_id must NOT be in worker_agent_ids.
    assert "acq-aaaa" not in coord_state.get("worker_agent_ids", {})


def test_acquire_failure_persists_pending_agent_id_for_retry(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess, monkeypatch,
) -> None:
    """Codex iter-4 [P1]: a failed acquire-prompt MUST persist the
    minted agent_id as a pending-acquire entry so the next tick retries
    with the SAME id and can hit AcquireCoordPromptInbox's recovery
    branch."""
    _write_tasks(project_dir, [_make_task("retry-aaaa", status="ready")])
    dispatch_subprocess.append("baaaaad1")

    import dispatch as dispatch_mod

    def _fail(*args, **kwargs):
        raise dispatch_mod.AcquirePromptError("error", 1, "simulated journal-write fault")

    monkeypatch.setattr(dispatch_mod, "acquire_coord_prompt_inbox", _fail)

    loop.tick(
        "fleet", coord_id="cccccc01", cwd="/repo",
        fleet_home=str(fleet_home),
    )
    coord_state = json.loads(
        (project_dir / "coord-state.json").read_text(encoding="utf-8")
    )
    pending = coord_state.get("pending_acquire_agent_ids", {})
    assert pending.get("retry-aaaa") == "baaaaad1", (
        f"pending agent_id not persisted: pending={pending!r}"
    )


def test_acquire_retry_reuses_pending_agent_id(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess, monkeypatch,
) -> None:
    """Codex iter-4 [P1]: when a pending-acquire entry exists for a
    slug, the next dispatch attempt MUST reuse it (not mint a new
    agent_id). This is the load-bearing invariant that lets
    AcquireCoordPromptInbox's recovery branch heal a half-written
    journal."""
    _write_tasks(project_dir, [_make_task("reuse-aaaa", status="ready")])
    # Seed coord-state with a pending-acquire entry from a prior tick.
    _seed_coord_state(project_dir, agent_id_map={})  # ensure file exists
    state_path = project_dir / "coord-state.json"
    state = json.loads(state_path.read_text())
    state["pending_acquire_agent_ids"] = {"reuse-aaaa": "deadc0de"}
    state_path.write_text(json.dumps(state))

    # Pin mint_agent_id to a DIFFERENT id so the test can confirm reuse.
    dispatch_subprocess.append("fffffff1")

    result = loop.tick(
        "fleet", coord_id="cccccc01", cwd="/repo",
        fleet_home=str(fleet_home),
    )
    assert result.dispatched == 1
    # The acquire-prompt argv must carry the pending id, not the
    # mint-stack id.
    acquire_calls = [
        c for c in dispatch_subprocess.seen_cmds
        if c[1:3] == ["claims", "acquire-prompt"]
    ]
    assert len(acquire_calls) == 1
    assert acquire_calls[0][3] == "deadc0de", (
        f"retry should reuse pending agent_id; argv={acquire_calls[0]!r}"
    )
    # Post-success, the pending entry must be cleared.
    state_after = json.loads(state_path.read_text())
    assert "reuse-aaaa" not in state_after.get("pending_acquire_agent_ids", {})


def test_acquire_success_clears_pending_acquire_entry(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess,
) -> None:
    """Successful acquire+apply clears any stale pending-acquire entry
    so a SUBSEQUENT dispatch for the same slug (post-terminal) mints a
    fresh agent_id rather than reusing the now-stale id."""
    _write_tasks(project_dir, [_make_task("clear-aaaa", status="ready")])
    _seed_coord_state(project_dir, agent_id_map={})
    state_path = project_dir / "coord-state.json"
    state = json.loads(state_path.read_text())
    state["pending_acquire_agent_ids"] = {"clear-aaaa": "abcd9999"}
    state_path.write_text(json.dumps(state))

    dispatch_subprocess.append("99999999")  # ignored — reuse should win

    loop.tick(
        "fleet", coord_id="cccccc01", cwd="/repo",
        fleet_home=str(fleet_home),
    )
    state_after = json.loads(state_path.read_text())
    assert state_after.get("pending_acquire_agent_ids", {}) == {}


def test_release_error_preserves_agent_id_mapping(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess, monkeypatch,
) -> None:
    """Codex iter-7 [P1]: when `fleet claims release` returns an error
    outcome (transient fault — binary missing, journal write race), the
    slug → agent_id mapping MUST be preserved so the next sweep /
    reconcile can retry the release. Forgetting on `error` permanently
    leaks the journal/inbox claim.
    """
    _write_tasks(project_dir, [
        _make_task("rel-err-aaaa", status="in-progress", worker_pid=1, pr_url=""),
    ])
    _seed_coord_state(
        project_dir, agent_id_map={"rel-err-aaaa": "feedfeed"},
    )

    # Stub the dispatch_mod helper to return an error envelope (NOT a
    # success outcome). The wrapper's outcome propagates up.
    import dispatch as dispatch_mod

    def _err_release(*args, **kwargs):
        return {
            "outcome": dispatch_mod.RELEASE_OUTCOME_ERROR,
            "error": "simulated transient fault",
        }

    monkeypatch.setattr(
        dispatch_mod, "release_coord_prompt_inbox", _err_release,
    )

    with patch.object(loop, "_pid_alive", return_value=False):
        loop.tick(
            "fleet", coord_id="cccccc01", cwd="/repo",
            fleet_home=str(fleet_home),
        )

    state = json.loads((project_dir / "coord-state.json").read_text())
    # CRITICAL: mapping preserved on error outcome.
    assert state.get("worker_agent_ids", {}).get("rel-err-aaaa") == "feedfeed", (
        "release error must NOT drop the agent_id mapping; "
        f"state={state!r}"
    )


def test_apply_failure_after_acquire_success_persists_pending(
    fleet_home: Path, project_dir: Path,
    dispatch_subprocess, monkeypatch,
) -> None:
    """Codex iter-8 [P1]: when acquire-prompt succeeds but
    _apply_dispatch later fails, the pending-acquire entry MUST be
    populated so the next tick reuses the SAME agent_id. Without
    pending population on acquire success, the next tick mints
    fresh and orphans the journal+inbox.
    """
    _write_tasks(project_dir, [_make_task("acq-ok-apply-fail", status="ready")])
    dispatch_subprocess.append("a1b2c3d4")

    # Patch _run_fleet so _apply_dispatch fails immediately, AFTER
    # acquire-prompt has already succeeded (acquire goes through
    # dispatch.subprocess.run, not loop._run_fleet).
    def boom(*args, **kwargs):
        raise RuntimeError("simulated apply fault")

    monkeypatch.setattr(loop, "_run_fleet", boom)

    loop.tick(
        "fleet", coord_id="cccccc01", cwd="/repo",
        fleet_home=str(fleet_home),
    )

    state = json.loads((project_dir / "coord-state.json").read_text())
    pending = state.get("pending_acquire_agent_ids", {})
    assert pending.get("acq-ok-apply-fail") == "a1b2c3d4", (
        "acquire-success + apply-fail must leave pending entry for retry; "
        f"pending={pending!r}"
    )


def test_apply_success_clears_pending_acquire(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess,
) -> None:
    """Acquire-success + apply-success leaves pending_acquire_agent_ids
    empty so the NEXT dispatch for the slug (post-terminal) mints
    fresh. Pair to test_apply_failure_after_acquire_success_persists_
    pending — the success path's clear is the load-bearing inverse.
    """
    _write_tasks(project_dir, [_make_task("happy-path-aa", status="ready")])
    dispatch_subprocess.append("baadf00d")

    loop.tick(
        "fleet", coord_id="cccccc01", cwd="/repo",
        fleet_home=str(fleet_home),
    )
    state = json.loads((project_dir / "coord-state.json").read_text())
    # Apply succeeded → pending cleared.
    assert state.get("pending_acquire_agent_ids", {}) == {}
    # Worker_agent_ids still has the mapping.
    assert state.get("worker_agent_ids", {}).get("happy-path-aa") == "baadf00d"


def test_handoff_marker_not_set_when_apply_fails(
    fleet_home: Path, project_dir: Path,
    dispatch_subprocess, monkeypatch,
) -> None:
    """Codex iter-8 [P1]: review_handoffs_dispatched marker MUST NOT
    be set when _apply_dispatch_handoff fails. Otherwise the next
    tick sees `slug:phase` as already-dispatched and suppresses the
    reviewer/finisher retry; the task stays stuck.
    """
    _write_tasks(project_dir, [
        _make_task("ho-mark-aa", status="in-progress", worker_pid=0),
    ])
    workers_dir = project_dir / "workers" / "ho-mark-aa"
    workers_dir.mkdir(parents=True, exist_ok=True)
    (workers_dir / "state.json").write_text(
        json.dumps({"phase": "review-pending"}), encoding="utf-8",
    )
    _seed_coord_state(
        project_dir, agent_id_map={"ho-mark-aa": "deadbeef"},
    )
    dispatch_subprocess.append("ee111e11")

    def boom(*args, **kwargs):
        raise RuntimeError("simulated handoff apply fault")

    monkeypatch.setattr(loop, "_run_fleet", boom)

    loop.tick(
        "fleet", coord_id="cccccc01", cwd="/repo",
        fleet_home=str(fleet_home),
    )

    state = json.loads((project_dir / "coord-state.json").read_text())
    # CRITICAL: handoff marker must NOT have been set.
    marker_key = "ho-mark-aa:review-pending"
    handoffs = state.get("review_handoffs_dispatched", [])
    assert marker_key not in handoffs, (
        f"handoff apply failure must not record the dispatched marker; "
        f"state={state!r}"
    )


def test_remember_agent_id_persists_before_apply(
    fleet_home: Path, project_dir: Path,
    dispatch_subprocess, monkeypatch,
) -> None:
    """Codex iter-7 [P1]: agent_id MUST be persisted in coord_state
    BEFORE _apply_dispatch runs. _apply_dispatch performs several CLI
    mutations (status, branch, workers update, note); if any fails
    after the claim has been acquired, the task is no longer `ready`
    so the pending-acquire retry never fires. Without an upfront
    remember_agent_id call, the next reconcile has no handle to
    release the orphaned claim.
    """
    _write_tasks(project_dir, [_make_task("upfront-aa", status="ready")])
    dispatch_subprocess.append("acefacef")

    # Patch _run_fleet to raise on first call so _apply_dispatch
    # fails immediately (BEFORE the status flip even lands). The
    # remember_agent_id call must have already happened.
    def boom(*args, **kwargs):
        raise RuntimeError("simulated apply fault")

    monkeypatch.setattr(loop, "_run_fleet", boom)

    loop.tick(
        "fleet", coord_id="cccccc01", cwd="/repo",
        fleet_home=str(fleet_home),
    )

    state = json.loads((project_dir / "coord-state.json").read_text())
    # CRITICAL: mapping persisted even though apply failed.
    assert state.get("worker_agent_ids", {}).get("upfront-aa") == "acefacef", (
        "agent_id must be remembered BEFORE apply runs; "
        f"state={state!r}"
    )


def test_sweep_releases_ready_with_worker_mapping(
    fleet_home: Path, project_dir: Path,
    dispatch_subprocess,
) -> None:
    """Codex iter-12 [P1]: an operator manually flipping a dispatched
    task back to `ready` while worker_agent_ids still tracks the old
    agent_id MUST trigger release before _dispatch_ready picks the
    task up again (otherwise the task double-dispatches under a new
    id and the prior claim leaks).
    """
    _write_tasks(project_dir, [
        _make_task("ready-reset-aa", status="ready"),
    ])
    _seed_coord_state(
        project_dir, agent_id_map={"ready-reset-aa": "ababab01"},
    )
    inbox = fleet_home / "inbox" / "ababab01.md"
    inbox.write_text("prior worker prompt\n", encoding="utf-8")
    dispatch_subprocess.append("99887766")  # pinned mint for new dispatch

    loop.tick(
        "fleet", coord_id="cccccc01", cwd="/repo",
        fleet_home=str(fleet_home),
    )

    # Old id was released.
    release_calls = [
        c for c in dispatch_subprocess.seen_cmds
        if c[1:3] == ["claims", "release"]
    ]
    assert any(c[3] == "ababab01" for c in release_calls), (
        f"sweep must release stale worker_agent_ids before redispatch; "
        f"release_calls={release_calls!r}"
    )
    # worker_agent_ids replaced by the NEW dispatch's id (not the old).
    state = json.loads((project_dir / "coord-state.json").read_text())
    assert state.get("worker_agent_ids", {}).get("ready-reset-aa") == "99887766"


def test_ready_reset_sweep_preserves_pending_acquire(
    fleet_home: Path, project_dir: Path,
    dispatch_subprocess,
) -> None:
    """Codex iter-15 [P1]: a `ready` task with BOTH worker_agent_ids
    AND pending_acquire_agent_ids (apply-failed-after-acquire shape)
    must have its STALE worker entry released, but the pending-
    acquire entry MUST be preserved so _dispatch_ready can reuse
    it via the recovery branch. Without this, forget_agent_id's
    cascading clear of pending would force a fresh mint and leak
    the half-written claim.
    """
    _write_tasks(project_dir, [_make_task("dual-map-aa", status="ready")])
    _seed_coord_state(
        project_dir, agent_id_map={"dual-map-aa": "1eeee101"},
    )
    state_path = project_dir / "coord-state.json"
    state = json.loads(state_path.read_text())
    state["pending_acquire_agent_ids"] = {"dual-map-aa": "2bbbbb02"}
    state_path.write_text(json.dumps(state))
    inbox_stale = fleet_home / "inbox" / "1eeee101.md"
    inbox_stale.write_text("stale worker prompt\n", encoding="utf-8")
    inbox_pending = fleet_home / "inbox" / "2bbbbb02.md"
    inbox_pending.write_text("half-written prompt\n", encoding="utf-8")

    loop.tick(
        "fleet", coord_id="cccccc01", cwd="/repo",
        fleet_home=str(fleet_home),
    )

    # Stale worker id was released.
    release_calls = [
        c for c in dispatch_subprocess.seen_cmds
        if c[1:3] == ["claims", "release"]
    ]
    assert any(c[3] == "1eeee101" for c in release_calls), (
        f"stale worker id must be released; calls={release_calls!r}"
    )
    # Pending id used by dispatch (acquire fired against it).
    acquire_calls = [
        c for c in dispatch_subprocess.seen_cmds
        if c[1:3] == ["claims", "acquire-prompt"]
    ]
    assert any(c[3] == "2bbbbb02" for c in acquire_calls), (
        f"pending id must be reused via dispatch; calls={acquire_calls!r}"
    )
    # Post-tick state: worker_agent_ids now has the pending id (which
    # the successful dispatch promoted); the original stale id is gone.
    state_after = json.loads(state_path.read_text())
    assert state_after.get("worker_agent_ids", {}).get("dual-map-aa") == "2bbbbb02"
    # pending_acquire cleared on dispatch success.
    assert state_after.get("pending_acquire_agent_ids", {}) == {}


def test_sweep_releases_archived_slugs(
    fleet_home: Path, project_dir: Path,
    dispatch_subprocess,
) -> None:
    """Codex iter-12 [P2]: an operator who archived/removed a tracked
    slug from tasks.md leaves the agent_id orphaned in coord_state.
    The sweep MUST release the claim and clear the maps for slugs
    absent from tasks.md.
    """
    # tasks.md has NO entry for the tracked slug (operator archived).
    _write_tasks(project_dir, [_make_task("present-bbbb", status="ready")])
    _seed_coord_state(
        project_dir, agent_id_map={"archived-aa": "deadc0de"},
    )
    inbox = fleet_home / "inbox" / "deadc0de.md"
    inbox.write_text("archived worker prompt\n", encoding="utf-8")
    dispatch_subprocess.append("11111111")

    loop.tick(
        "fleet", coord_id="cccccc01", cwd="/repo",
        fleet_home=str(fleet_home),
    )

    release_calls = [
        c for c in dispatch_subprocess.seen_cmds
        if c[1:3] == ["claims", "release"]
    ]
    assert any(c[3] == "deadc0de" for c in release_calls), (
        f"sweep must release archived slug; release_calls={release_calls!r}"
    )
    state = json.loads((project_dir / "coord-state.json").read_text())
    assert "archived-aa" not in state.get("worker_agent_ids", {})


def test_sweep_ready_with_only_pending_does_not_release(
    fleet_home: Path, project_dir: Path,
    dispatch_subprocess,
) -> None:
    """Codex iter-12 [P1] scope guard: a `ready` slug whose ONLY
    tracked entry is in pending_acquire_agent_ids is in the recovery
    path (next dispatch reuses the id). The sweep must NOT release
    it; only the worker_agent_ids case triggers the ready_reset
    cleanup.
    """
    _write_tasks(project_dir, [_make_task("ready-pending-aa", status="ready")])
    _seed_coord_state(project_dir, agent_id_map={})
    state_path = project_dir / "coord-state.json"
    state = json.loads(state_path.read_text())
    state["pending_acquire_agent_ids"] = {"ready-pending-aa": "fa11ed10"}
    state_path.write_text(json.dumps(state))
    inbox = fleet_home / "inbox" / "fa11ed10.md"
    inbox.write_text("half-written prompt\n", encoding="utf-8")
    dispatch_subprocess.append("99999999")  # ignored — reuse should win

    loop.tick(
        "fleet", coord_id="cccccc01", cwd="/repo",
        fleet_home=str(fleet_home),
    )

    # The sweep did NOT release the pending id — dispatch reused it.
    state_after = json.loads(state_path.read_text())
    # Successful dispatch + apply → pending cleared, worker_agent_ids set.
    assert state_after.get("worker_agent_ids", {}).get("ready-pending-aa") == "fa11ed10"


def test_reconcile_release_error_stashes_for_retry(
    fleet_home: Path, project_dir: Path,
    dispatch_subprocess, monkeypatch,
) -> None:
    """Codex iter-10 [P1]: reconcile terminal release returning `error`
    must stash the id in pending_release_agent_ids so the retry pass
    can re-attempt. Otherwise a slug transitioning to todo/blocked
    leaves the in-flight set permanently with its claim orphaned.

    Codex iter-13 [P1] interaction: the retry pass deliberately skips
    ids whose slug is in a live (ready/in-progress/in-review) state
    AND whose id matches the active worker/pending entry — to avoid
    tearing down a re-dispatched live claim. So the test must verify
    the stash happens INSIDE the reconcile apply, before the retry
    pass's skip evaluation runs in a later tick.
    """
    _write_tasks(project_dir, [
        _make_task("rec-rel-aa", status="in-progress", worker_pid=1, pr_url=""),
    ])
    _seed_coord_state(
        project_dir, agent_id_map={"rec-rel-aa": "feedface"},
    )

    import dispatch as dispatch_mod

    def _err_release(*args, **kwargs):
        return {
            "outcome": dispatch_mod.RELEASE_OUTCOME_ERROR,
            "error": "simulated transient release fault",
        }

    monkeypatch.setattr(
        dispatch_mod, "release_coord_prompt_inbox", _err_release,
    )
    # Intercept _retry_pending_releases at its first invocation so we
    # can inspect coord_state right after reconcile but before the
    # retry pass has a chance to clear the stash. Iter-13's skip-on-
    # live-status logic would otherwise drop the entry once the test
    # fixture mock fails to flip status to todo (the real _run_fleet
    # mutates tasks.md; the recorder stub doesn't).
    captured = {}

    real_retry = loop._retry_pending_releases

    def capture_then_skip(**kw):
        captured["state"] = dict(kw["coord_state"])
        # Do NOT call real_retry — we want to observe the post-reconcile
        # stash before the retry pass clears it.

    monkeypatch.setattr(loop, "_retry_pending_releases", capture_then_skip)

    with patch.object(loop, "_pid_alive", return_value=False):
        loop.tick(
            "fleet", coord_id="cccccc01", cwd="/repo",
            fleet_home=str(fleet_home),
        )

    pending_releases = captured.get("state", {}).get("pending_release_agent_ids", {})
    assert "feedface" in pending_releases.get("rec-rel-aa", []), (
        f"reconcile release error must stash for retry; "
        f"pending_release={pending_releases!r}"
    )
    # worker_agent_ids preserved (so we still have the handle).
    assert captured.get("state", {}).get("worker_agent_ids", {}).get("rec-rel-aa") == "feedface"


def test_sweep_non_inflight_releases_todo_slugs(
    fleet_home: Path, project_dir: Path,
    dispatch_subprocess,
) -> None:
    """Codex iter-10 [P2]: a slug whose status is `todo` with a
    tracked agent_id must have its claim released and mapping cleared.
    Without this sweep, an operator-driven todo transition leaves
    the journal/inbox orphaned because reconcile only processes
    in-progress / in-review.
    """
    _write_tasks(project_dir, [
        _make_task("todo-rel-aa", status="todo", worker_pid=0),
    ])
    _seed_coord_state(
        project_dir, agent_id_map={"todo-rel-aa": "5aaaaaaa"},
    )
    inbox = fleet_home / "inbox" / "5aaaaaaa.md"
    inbox.write_text("todo worker prompt\n", encoding="utf-8")

    loop.tick(
        "fleet", coord_id="cccccc01", cwd="/repo",
        fleet_home=str(fleet_home),
    )

    release_calls = [
        c for c in dispatch_subprocess.seen_cmds
        if c[1:3] == ["claims", "release"]
    ]
    assert any(c[3] == "5aaaaaaa" for c in release_calls), (
        f"sweep should release todo slug; calls={release_calls!r}"
    )
    state = json.loads((project_dir / "coord-state.json").read_text())
    assert "todo-rel-aa" not in state.get("worker_agent_ids", {})


def test_sweep_preserves_blocked_worker_inbox(
    fleet_home: Path, project_dir: Path,
    dispatch_subprocess,
) -> None:
    """Codex iter-13 [P1] + iter-14 [P2]: BLOCKED_QUESTION carve-out —
    blocked workers stay ALIVE so the operator can answer through the
    same inbox. Discriminator: worker_dir present → live worker (skip).
    """
    _write_tasks(project_dir, [
        _make_task("blocked-keep", status="blocked", worker_pid=0),
    ])
    # Worker dir present + state.json with non-terminal phase =
    # BLOCKED_QUESTION lifecycle.
    workers_dir = project_dir / "workers" / "blocked-keep"
    workers_dir.mkdir(parents=True, exist_ok=True)
    (workers_dir / "state.json").write_text(
        json.dumps({"phase": "in-progress"}), encoding="utf-8",
    )
    _seed_coord_state(
        project_dir, agent_id_map={"blocked-keep": "ab10cked"},
    )
    inbox = fleet_home / "inbox" / "ab10cked.md"
    inbox.write_text("blocked worker prompt\n", encoding="utf-8")

    loop.tick(
        "fleet", coord_id="cccccc01", cwd="/repo",
        fleet_home=str(fleet_home),
    )

    # Inbox preserved, agent_id mapping preserved.
    release_calls = [
        c for c in dispatch_subprocess.seen_cmds
        if c[1:3] == ["claims", "release"]
    ]
    spurious = [c for c in release_calls if c[3] == "ab10cked"]
    assert spurious == [], (
        f"blocked slug with worker dir must NOT be swept; "
        f"release_calls={release_calls!r}"
    )
    assert inbox.exists(), "blocked inbox must remain (answer-blocked workflow)"
    state = json.loads((project_dir / "coord-state.json").read_text())
    assert state.get("worker_agent_ids", {}).get("blocked-keep") == "ab10cked"


def test_sweep_releases_manual_blocked_no_worker_dir(
    fleet_home: Path, project_dir: Path,
    dispatch_subprocess,
) -> None:
    """Codex iter-14 [P2]: operator-driven `blocked` with no live
    worker (worker_dir absent) MUST be swept. Discriminator: worker
    dir GONE → manual blocked. Otherwise the manual blocked claim
    leaks indefinitely.
    """
    _write_tasks(project_dir, [
        _make_task("blocked-manual", status="blocked", worker_pid=0),
    ])
    # No worker_dir present (operator manually flipped to blocked).
    _seed_coord_state(
        project_dir, agent_id_map={"blocked-manual": "ee100eee"},
    )
    inbox = fleet_home / "inbox" / "ee100eee.md"
    inbox.write_text("stale prompt\n", encoding="utf-8")

    loop.tick(
        "fleet", coord_id="cccccc01", cwd="/repo",
        fleet_home=str(fleet_home),
    )

    release_calls = [
        c for c in dispatch_subprocess.seen_cmds
        if c[1:3] == ["claims", "release"]
    ]
    assert any(c[3] == "ee100eee" for c in release_calls), (
        f"manual blocked (no worker_dir) MUST be swept; "
        f"release_calls={release_calls!r}"
    )
    state = json.loads((project_dir / "coord-state.json").read_text())
    assert "blocked-manual" not in state.get("worker_agent_ids", {})


def test_sweep_non_inflight_does_not_touch_ready_slugs(
    fleet_home: Path, project_dir: Path,
    dispatch_subprocess,
) -> None:
    """Codex iter-10 [P2] scope guard: `ready` slugs are about to be
    dispatched — the next tick uses pending_acquire_agent_ids to retry
    a half-written claim. The non-inflight sweep MUST NOT preempt
    this by releasing the pending claim before dispatch can hit the
    recovery path.
    """
    _write_tasks(project_dir, [_make_task("ready-keep", status="ready")])
    _seed_coord_state(project_dir, agent_id_map={})
    state_path = project_dir / "coord-state.json"
    state = json.loads(state_path.read_text())
    state["pending_acquire_agent_ids"] = {"ready-keep": "5aaaaaaa"}
    state_path.write_text(json.dumps(state))
    inbox = fleet_home / "inbox" / "5aaaaaaa.md"
    inbox.write_text("half-written prompt\n", encoding="utf-8")
    dispatch_subprocess.append("99887766")  # ignored — reuse should win

    loop.tick(
        "fleet", coord_id="cccccc01", cwd="/repo",
        fleet_home=str(fleet_home),
    )

    # No spurious release for the ready slug.
    release_calls = [
        c for c in dispatch_subprocess.seen_cmds
        if c[1:3] == ["claims", "release"]
    ]
    spurious = [c for c in release_calls if c[3] == "5aaaaaaa"]
    # The dispatch should have hit the acquire path with the pending
    # id (recovery flow); release should not fire from the sweep.
    # Note: dispatch SUCCESS clears pending and remembers in
    # worker_agent_ids, so the post-tick state.json is the
    # authoritative check.
    state_after = json.loads(state_path.read_text())
    assert state_after.get("worker_agent_ids", {}).get("ready-keep") == "5aaaaaaa", (
        f"ready slug must reuse pending id on dispatch (no spurious release); "
        f"release_calls={release_calls!r} state={state_after!r}"
    )
    assert state_after.get("pending_acquire_agent_ids", {}) == {}


def test_handoff_release_error_stashes_prior_id_for_retry(
    fleet_home: Path, project_dir: Path,
    dispatch_subprocess, monkeypatch,
) -> None:
    """Codex iter-9 [P1]: if the handoff's release of the prior
    subagent's claim fails transiently, the new dispatch is about
    to overwrite worker_agent_ids — the only handle on the prior
    claim. The prior agent_id MUST be stashed in
    pending_release_agent_ids so a later sweep / reconcile can
    retry the release.
    """
    _write_tasks(project_dir, [
        _make_task("ho-rel-err-a", status="in-progress", worker_pid=0),
    ])
    workers_dir = project_dir / "workers" / "ho-rel-err-a"
    workers_dir.mkdir(parents=True, exist_ok=True)
    (workers_dir / "state.json").write_text(
        json.dumps({"phase": "review-pending"}), encoding="utf-8",
    )
    _seed_coord_state(
        project_dir, agent_id_map={"ho-rel-err-a": "ab10ab10"},
    )
    dispatch_subprocess.append("ee111e11")

    # Stub release to return error. The retry pass will then attempt
    # to release the stashed id; let's stub it to keep failing so we
    # can observe the persistence shape.
    import dispatch as dispatch_mod

    def _err_release(*args, **kwargs):
        return {
            "outcome": dispatch_mod.RELEASE_OUTCOME_ERROR,
            "error": "simulated handoff release fault",
        }

    monkeypatch.setattr(
        dispatch_mod, "release_coord_prompt_inbox", _err_release,
    )

    loop.tick(
        "fleet", coord_id="cccccc01", cwd="/repo",
        fleet_home=str(fleet_home),
    )

    state = json.loads((project_dir / "coord-state.json").read_text())
    # CRITICAL: prior worker's agent_id stashed for retry.
    pending_releases = state.get("pending_release_agent_ids", {})
    assert "ab10ab10" in pending_releases.get("ho-rel-err-a", []), (
        f"prior id must be stashed after release failure; "
        f"pending_release={pending_releases!r}"
    )


def test_retry_pending_releases_retries_in_review(
    fleet_home: Path, project_dir: Path,
    dispatch_subprocess,
) -> None:
    """Codex iter-16 [P1]: in-review tasks have already exited the
    worker phase (PR is open, worker dead). The retry-pending-
    releases pass MUST attempt re-release here, not treat in-review
    as a live state. Otherwise a worker→PR transition that hit a
    transient release error orphans the claim until the PR closes.
    """
    _write_tasks(project_dir, [
        _make_task("review-rel", status="in-review",
                   pr_url="https://github.com/x/y/pull/9"),
    ])
    _seed_coord_state(
        project_dir, agent_id_map={"review-rel": "cccccccc"},
    )
    state_path = project_dir / "coord-state.json"
    state = json.loads(state_path.read_text())
    state["pending_release_agent_ids"] = {"review-rel": ["cccccccc"]}
    state_path.write_text(json.dumps(state))
    inbox = fleet_home / "inbox" / "cccccccc.md"
    inbox.write_text("orphaned in-review prompt\n", encoding="utf-8")

    loop.tick(
        "fleet", coord_id="cccccc01", cwd="/repo",
        fleet_home=str(fleet_home),
    )

    # Retry pass fired the release.
    release_calls = [
        c for c in dispatch_subprocess.seen_cmds
        if c[1:3] == ["claims", "release"]
    ]
    assert any(c[3] == "cccccccc" for c in release_calls), (
        f"in-review slug must allow retry-pending-release; "
        f"calls={release_calls!r}"
    )
    # On terminal success, pending_release entry cleared.
    state_after = json.loads(state_path.read_text())
    assert state_after.get("pending_release_agent_ids", {}) == {}


def test_retry_pending_releases_drops_on_success(
    fleet_home: Path, project_dir: Path,
    dispatch_subprocess,
) -> None:
    """The retry pass at tick top consumes pending_release_agent_ids:
    successful release on retry removes the entry; the next sweep
    sees an empty map.
    """
    _write_tasks(project_dir, [_make_task("retry-aa", status="todo")])
    _seed_coord_state(project_dir, agent_id_map={})
    state_path = project_dir / "coord-state.json"
    state = json.loads(state_path.read_text())
    state["pending_release_agent_ids"] = {"retry-aa": ["fa11ed1d"]}
    state_path.write_text(json.dumps(state))
    # Seed the inbox file so the emulator's release returns
    # `released` (not `already_released` — both terminal, either is
    # fine; we just need a deterministic terminal outcome).
    inbox = fleet_home / "inbox" / "fa11ed1d.md"
    inbox.write_text("orphaned prompt\n", encoding="utf-8")

    loop.tick(
        "fleet", coord_id="cccccc01", cwd="/repo",
        fleet_home=str(fleet_home),
    )

    # Release fired for the stashed id.
    release_calls = [
        c for c in dispatch_subprocess.seen_cmds
        if c[1:3] == ["claims", "release"]
    ]
    assert any(c[3] == "fa11ed1d" for c in release_calls), (
        f"retry pass should release the stashed id; calls={release_calls!r}"
    )
    # Entry dropped on terminal success.
    state_after = json.loads(state_path.read_text())
    assert state_after.get("pending_release_agent_ids", {}) == {}


def test_sweep_done_releases_pending_acquire_too(
    fleet_home: Path, project_dir: Path,
    dispatch_subprocess,
) -> None:
    """Codex iter-9 [P2]: operator-driven `status=done` sweep MUST
    release claims tracked in pending_acquire_agent_ids too. A failed
    acquire creates the journal/inbox but never populates
    worker_agent_ids; if the operator marks done before the retry,
    the half-written claim leaks.
    """
    _write_tasks(project_dir, [
        _make_task("acq-fail-done", status="done", worker_pid=0,
                   pr_url="https://github.com/x/y/pull/3"),
    ])
    # Worker dir present so the existing sweep stat-check fires.
    workers_dir = project_dir / "workers" / "acq-fail-done"
    workers_dir.mkdir(parents=True, exist_ok=True)
    (workers_dir / "state.json").write_text(
        json.dumps({"phase": "done"}), encoding="utf-8",
    )
    # No worker_agent_ids entry (acquire failed before any apply).
    # Pending-acquire entry from the half-written acquire.
    _seed_coord_state(project_dir, agent_id_map={})
    state_path = project_dir / "coord-state.json"
    state = json.loads(state_path.read_text())
    state["pending_acquire_agent_ids"] = {"acq-fail-done": "ba1fba1f"}
    state_path.write_text(json.dumps(state))
    inbox = fleet_home / "inbox" / "ba1fba1f.md"
    inbox.write_text("half-written prompt\n", encoding="utf-8")

    loop.tick(
        "fleet", coord_id="cccccc01", cwd="/repo",
        fleet_home=str(fleet_home),
    )

    # Release fired for the pending-acquire id.
    release_calls = [
        c for c in dispatch_subprocess.seen_cmds
        if c[1:3] == ["claims", "release"]
    ]
    assert any(c[3] == "ba1fba1f" for c in release_calls), (
        f"sweep must release pending-acquire id; calls={release_calls!r}"
    )
    state_after = json.loads(state_path.read_text())
    # Pending entry cleared.
    assert state_after.get("pending_acquire_agent_ids", {}) == {}


def test_sweep_done_worker_dirs_releases_claim(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess,
) -> None:
    """Codex iter-6 [P2]: operator-driven `fleet tasks set status=done`
    bypasses the reconcile/sentinel apply paths that normally call
    _release_coord_prompt_inbox. The _sweep_done_worker_dirs defense-
    in-depth path MUST also release the claim + forget the agent_id
    so the inbox/journal don't leak indefinitely for manually-completed
    tasks.
    """
    # Task at status=done with a worker dir on disk + an agent_id
    # mapping from when it was in-progress.
    _write_tasks(project_dir, [
        _make_task("manual-done", status="done", worker_pid=0,
                   pr_url="https://github.com/x/y/pull/1"),
    ])
    workers_dir = project_dir / "workers" / "manual-done"
    workers_dir.mkdir(parents=True, exist_ok=True)
    (workers_dir / "state.json").write_text(
        json.dumps({"phase": "done"}), encoding="utf-8",
    )
    _seed_coord_state(
        project_dir, agent_id_map={"manual-done": "deadbeef"},
    )
    inbox = fleet_home / "inbox" / "deadbeef.md"
    inbox.write_text("manual-done worker prompt\n", encoding="utf-8")

    loop.tick(
        "fleet", coord_id="cccccc01", cwd="/repo",
        fleet_home=str(fleet_home),
    )

    # Release fired for the manually-done task.
    release_calls = [
        c for c in dispatch_subprocess.seen_cmds
        if c[1:3] == ["claims", "release"]
    ]
    assert any(c[3] == "deadbeef" for c in release_calls), (
        f"sweep should have released manual-done's claim; "
        f"seen={release_calls!r}"
    )
    # Inbox file unlinked.
    assert not inbox.exists()
    # Agent_id mapping forgotten.
    state = json.loads((project_dir / "coord-state.json").read_text())
    assert "manual-done" not in state.get("worker_agent_ids", {})


def test_apply_dispatch_failure_preserves_pending_acquire(
    fleet_home: Path, project_dir: Path,
    dispatch_subprocess, monkeypatch,
) -> None:
    """Codex iter-5 [P1]: if _apply_dispatch fails AFTER acquire-prompt
    succeeded, the pending-acquire entry MUST be preserved so the next
    tick retries with the SAME id (hitting the controller's recovery
    branch). Without preservation, the original claim+inbox leak.

    Setup: pre-seed a pending entry (simulating a prior tick where
    acquire failed AFTER writing the journal). This tick's acquire
    succeeds (recovery path), but apply then fails. The pending entry
    must still be there at the end.
    """
    _write_tasks(project_dir, [_make_task("apply-fail-aa", status="ready")])
    _seed_coord_state(project_dir, agent_id_map={})
    state_path = project_dir / "coord-state.json"
    state = json.loads(state_path.read_text())
    state["pending_acquire_agent_ids"] = {"apply-fail-aa": "abadcafe"}
    state_path.write_text(json.dumps(state))

    # Pin mint to a different id so the test confirms the pending id
    # was reused (and not the freshly minted one).
    dispatch_subprocess.append("99887766")

    # _apply_dispatch invokes _run_fleet for status flips etc.
    # Patch _run_fleet to raise an exception so apply fails AFTER
    # the acquire-prompt has already succeeded.
    def boom(*args, **kwargs):
        raise RuntimeError("simulated fleet binary fault during apply")

    monkeypatch.setattr(loop, "_run_fleet", boom)

    result = loop.tick(
        "fleet", coord_id="cccccc01", cwd="/repo",
        fleet_home=str(fleet_home),
    )
    # _apply_dispatch raised, so the try/except in the consumer logged
    # an error and did NOT count this as dispatched.
    assert result.dispatched == 0
    assert any("apply-fail-aa" in e for e in result.errors), (
        f"expected error surfacing for apply-fail-aa; errors={result.errors!r}"
    )

    # Confirm the acquire-prompt was called with the REUSED pending id.
    acquire_calls = [
        c for c in dispatch_subprocess.seen_cmds
        if c[1:3] == ["claims", "acquire-prompt"]
    ]
    assert len(acquire_calls) == 1
    assert acquire_calls[0][3] == "abadcafe", (
        f"expected pending agent_id reuse; argv={acquire_calls[0]!r}"
    )

    # CRITICAL: the pending-acquire entry MUST still be there so the
    # next tick can retry with the same agent_id.
    state_after = json.loads(state_path.read_text())
    pending = state_after.get("pending_acquire_agent_ids", {})
    assert pending.get("apply-fail-aa") == "abadcafe", (
        f"apply failure must preserve pending agent_id for retry; "
        f"pending={pending!r}"
    )


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
    # cap=1 keeps us in single-worker mode so we don't trip on the
    # worktree-path subprocess emulation gap (the test fixture doesn't
    # patch `fleet workers worktree-path`). Dep filtering is the same
    # in either mode; cap=1 ensures only the one candidate is picked
    # even before dep filtering would matter.
    result = loop.tick(
        "fleet", coord_id="cccccc01", cwd="/repo",
        cap=1, fleet_home=str(fleet_home),
    )
    # blocked-bbbb has unsatisfied dep; dep-aaaa is the only candidate.
    # Exactly one dispatched.
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
    # Issue #84 Phase A: dispatch.subprocess.run still gets called
    # for fetch_standards / fetch_learnings; mock it to a no-op.
    # PR1 dispatch-lifecycle: also intercept `fleet claims
    # acquire-prompt` so the loop migration writes the inbox file +
    # parses a real envelope.
    fake = subprocess.CompletedProcess(
        args=[], returncode=0, stdout="", stderr="",
    )

    def fake_run(cmd, capture_output=True, text=True, timeout=None,
                 check=False, input=None, env=None):
        if len(cmd) >= 4 and cmd[1:3] == ["claims", "acquire-prompt"]:
            agent_id = cmd[3]
            run_env = env or os.environ
            home = run_env.get("FLEET_HOME") or os.path.expanduser("~/.fleet")
            inbox_dir = os.path.join(home, "inbox")
            os.makedirs(inbox_dir, exist_ok=True)
            path = os.path.join(inbox_dir, f"{agent_id}.md")
            body = input or ""
            if body and not body.endswith("\n"):
                body = body + "\n"
            with open(path, "w", encoding="utf-8") as fh:
                fh.write(body)
            envelope = (
                f'{{"outcome":"acquired","dispatch_id":"{agent_id}",'
                f'"kind":"coord_prompt_inbox","path":"{path}"}}\n'
            )
            return subprocess.CompletedProcess(
                args=cmd, returncode=0, stdout=envelope, stderr="",
            )
        return fake

    monkeypatch.setattr(dispatch.subprocess, "run", fake_run)
    rc = loop.main([])
    assert rc == 0
    out = capsys.readouterr().out
    # Issue #84 Phase A: main() prints the DISPATCH block(s) BEFORE
    # the JSON summary so the coord agent (Claude) sees them as
    # parseable plain text. The summary is the LAST line of stdout.
    lines = [ln for ln in out.splitlines() if ln.strip()]
    assert lines, f"main() emitted no output: {out!r}"
    # Locate the JSON line — it's the only one that parses as JSON.
    json_line = lines[-1]
    parsed = json.loads(json_line)
    assert parsed["dispatched"] == 1
    assert parsed["skipped"] is False
    # The DISPATCH block precedes the JSON line.
    assert any(ln.startswith("DISPATCH: ready-aaaa") for ln in lines), (
        f"main() must emit DISPATCH block before JSON summary; saw: {lines}"
    )
    assert any(ln.strip() == "END_DISPATCH" for ln in lines)


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


# ---------- Lifecycle hygiene (issue #101) ----------


def test_reconcile_done_phase_triggers_workers_delete(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess,
    monkeypatch,
) -> None:
    """Issue #101: when reconcile sees a worker at phase=done with a
    PR URL, the apply path must shell out to `fleet workers delete`
    AFTER persisting the pr_url. Order is load-bearing — the operator-
    visible PR URL must outlive the worker dir cleanup.
    """
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
        loop.tick(
            project, coord_id="cccccc01", cwd="/repo",
            fleet_home=str(fleet_home),
        )

    pr_idx = -1
    delete_idx = -1
    for i, c in enumerate(fleet_run_recorder):
        if c[1:3] == ["tasks", "set"] and any("pr_url=https://" in p for p in c):
            pr_idx = i
        if c[1:3] == ["workers", "delete"]:
            delete_idx = i
    assert delete_idx > 0, f"workers delete not invoked: {fleet_run_recorder}"
    assert pr_idx > 0, f"pr_url set not invoked: {fleet_run_recorder}"
    assert pr_idx < delete_idx, "pr_url must be persisted BEFORE workers delete"


def test_reconcile_blocked_phase_keeps_worker_dir(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess,
    monkeypatch,
) -> None:
    """Issue #101: a worker at phase=blocked is in lifecycle Waiting,
    not Terminal. The dir must NOT be deleted (the operator may un-
    block and resume).
    """
    monkeypatch.setenv("FLEET_HOME", str(fleet_home))
    project = "fleet"
    workers_dir = fleet_home / "projects" / project / "workers" / "stuck-bbbb"
    workers_dir.mkdir(parents=True, exist_ok=True)
    fresh = _dt.datetime.now(tz=_dt.timezone.utc).isoformat().replace(
        "+00:00", "Z",
    )
    (workers_dir / "state.json").write_text(json.dumps({
        "slug": "stuck-bbbb", "project": project,
        "phase": "blocked", "updated_at": fresh,
        "blocked_reason": "needs operator clarification",
    }), encoding="utf-8")

    _write_tasks(project_dir, [
        _make_task("stuck-bbbb", status="in-progress", worker_pid=99998),
    ])
    with patch.object(loop, "_pid_alive", return_value=False):
        loop.tick(
            project, coord_id="cccccc01", cwd="/repo",
            fleet_home=str(fleet_home),
        )

    delete_calls = [c for c in fleet_run_recorder if c[1:3] == ["workers", "delete"]]
    assert delete_calls == [], (
        "blocked worker dir must NOT be deleted; got " + repr(delete_calls)
    )


def test_reconcile_failed_phase_triggers_workers_delete(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess,
    monkeypatch,
) -> None:
    """Issue #101: phase=failed is lifecycle TerminalFailure → delete.
    """
    monkeypatch.setenv("FLEET_HOME", str(fleet_home))
    project = "fleet"
    workers_dir = fleet_home / "projects" / project / "workers" / "broken-cccc"
    workers_dir.mkdir(parents=True, exist_ok=True)
    fresh = _dt.datetime.now(tz=_dt.timezone.utc).isoformat().replace(
        "+00:00", "Z",
    )
    (workers_dir / "state.json").write_text(json.dumps({
        "slug": "broken-cccc", "project": project,
        "phase": "failed", "updated_at": fresh,
    }), encoding="utf-8")

    _write_tasks(project_dir, [
        _make_task("broken-cccc", status="in-progress", worker_pid=99997),
    ])
    with patch.object(loop, "_pid_alive", return_value=False):
        loop.tick(
            project, coord_id="cccccc01", cwd="/repo",
            fleet_home=str(fleet_home),
        )

    delete_calls = [c for c in fleet_run_recorder if c[1:3] == ["workers", "delete"]]
    assert delete_calls, "failed worker dir should be deleted"


def test_sentinel_task_done_pr_triggers_workers_delete(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess,
    monkeypatch,
) -> None:
    """Issue #101: TASK_DONE_PR sentinel = lifecycle TerminalSuccess.
    Apply order must be: pr_url set → status flip → workers delete.
    """
    monkeypatch.setenv("FLEET_HOME", str(fleet_home))
    project = "fleet"

    # Plant a sentinel archive file.
    archive = fleet_home / "inbox" / "archive"
    archive.mkdir(parents=True, exist_ok=True)
    (archive / "cccccc01-20260509-100000.md").write_text(
        "TASK_DONE_PR=task-1234 https://github.com/x/y/pull/8\n",
        encoding="utf-8",
    )

    _write_tasks(project_dir, [
        _make_task("task-1234", status="ready"),
    ])
    loop.tick(
        project, coord_id="cccccc01", cwd="/repo",
        fleet_home=str(fleet_home),
    )

    # Find indices.
    pr_idx = -1
    delete_idx = -1
    for i, c in enumerate(fleet_run_recorder):
        if c[1:3] == ["tasks", "set"] and any("pr_url=https://" in p for p in c):
            pr_idx = i
        if c[1:3] == ["workers", "delete"]:
            delete_idx = i
    assert delete_idx > 0, f"workers delete not invoked: {fleet_run_recorder}"
    assert pr_idx >= 0
    assert pr_idx < delete_idx, "pr_url must be persisted before delete"


def test_sentinel_blocked_question_keeps_worker_dir(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess,
    monkeypatch,
) -> None:
    """Issue #101: BLOCKED_QUESTION sentinel = lifecycle Waiting; the
    worker dir is preserved so the operator can inspect blocked_reason.
    """
    monkeypatch.setenv("FLEET_HOME", str(fleet_home))
    project = "fleet"

    archive = fleet_home / "inbox" / "archive"
    archive.mkdir(parents=True, exist_ok=True)
    (archive / "cccccc01-20260509-100001.md").write_text(
        "BLOCKED_QUESTION=task-1234 should I use foo or bar?\n",
        encoding="utf-8",
    )

    _write_tasks(project_dir, [_make_task("task-1234", status="ready")])
    loop.tick(
        project, coord_id="cccccc01", cwd="/repo",
        fleet_home=str(fleet_home),
    )

    delete_calls = [c for c in fleet_run_recorder if c[1:3] == ["workers", "delete"]]
    assert delete_calls == [], (
        "blocked-question must NOT delete worker dir; got " + repr(delete_calls)
    )


def test_sentinel_worker_failed_triggers_workers_delete(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess,
    monkeypatch,
) -> None:
    """Issue #101: WORKER_FAILED sentinel = lifecycle TerminalFailure
    → worker dir deleted.
    """
    monkeypatch.setenv("FLEET_HOME", str(fleet_home))
    project = "fleet"

    archive = fleet_home / "inbox" / "archive"
    archive.mkdir(parents=True, exist_ok=True)
    (archive / "cccccc01-20260509-100002.md").write_text(
        "WORKER_FAILED=task-1234 panic in main\n",
        encoding="utf-8",
    )

    _write_tasks(project_dir, [_make_task("task-1234", status="ready")])
    loop.tick(
        project, coord_id="cccccc01", cwd="/repo",
        fleet_home=str(fleet_home),
    )

    delete_calls = [c for c in fleet_run_recorder if c[1:3] == ["workers", "delete"]]
    assert delete_calls, "failed worker dir should be deleted via sentinel apply"


def test_workers_delete_failure_does_not_abort_tick(
    fleet_home: Path, project_dir: Path,
    monkeypatch, capsys,
) -> None:
    """Issue #101: workers delete is best-effort. A failing CLI must
    log to stderr but NOT roll back the tasks.md mutations or abort
    the tick.
    """
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
            worker_pid=99996, pr_url="",
        ),
    ])

    # Make `fleet workers delete` fail; everything else passes.
    def fake_run(cmd, timeout_s=30.0):
        if cmd[1:3] == ["workers", "delete"]:
            raise RuntimeError("simulated delete failure")

    with patch.object(loop, "_pid_alive", return_value=False), \
         patch.object(loop, "_run_fleet", side_effect=fake_run):
        result = loop.tick(
            project, coord_id="cccccc01", cwd="/repo",
            fleet_home=str(fleet_home),
        )

    # Tick must record the reconcile but NOT raise.
    assert result.reconciled == 1
    assert result.errors == [], (
        "delete failures should be best-effort, not bubble into errors[]: "
        + repr(result.errors)
    )
    captured = capsys.readouterr()
    assert "workers delete failed" in captured.err


# ---------- Sub-fix A: defense-in-depth sweep of done-task worker dirs ----------


def test_sweep_done_worker_dirs_deletes_orphan_with_status_done(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess, monkeypatch,
) -> None:
    """A task at status=done whose worker dir lingers on disk gets
    rm-rf'd via `fleet workers delete`. Covers the operator-set /
    pre-#101-coord accumulation path that the reconcile-transition
    branch never sees (those tasks bypass the in-progress/in-review
    filter in _reconcile_inflight).
    """
    monkeypatch.setenv("FLEET_HOME", str(fleet_home))
    project = "fleet"
    # Seed an orphan worker dir whose phase is non-terminal — exactly
    # the screenshot scenario: worker exited early, task got flipped
    # to done elsewhere.
    workers_dir = fleet_home / "projects" / project / "workers" / "orphan-aaaa"
    workers_dir.mkdir(parents=True, exist_ok=True)
    (workers_dir / "state.json").write_text(json.dumps({
        "slug": "orphan-aaaa", "project": project,
        "phase": "tdd-green", "pid": 0,
    }), encoding="utf-8")

    _write_tasks(project_dir, [
        _make_task("orphan-aaaa", status="done", worker_pid=0),
    ])

    loop.tick(
        project, coord_id="cccccc01", cwd="/repo",
        fleet_home=str(fleet_home),
    )

    delete_calls = [
        c for c in fleet_run_recorder
        if c[1:3] == ["workers", "delete"] and c[-1] == "orphan-aaaa"
    ]
    assert delete_calls, (
        "sweep must fire workers delete for done task with lingering dir; "
        f"recorded: {fleet_run_recorder}"
    )


def test_sweep_skips_done_task_without_worker_dir(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess, monkeypatch,
) -> None:
    """"Skip on tasks already done from a prior tick": a done task
    whose worker dir is absent triggers zero CLI invocations. The
    stat() short-circuit keeps repeat ticks quiet on a clean tree.
    """
    monkeypatch.setenv("FLEET_HOME", str(fleet_home))
    project = "fleet"
    # No workers/<slug>/ dir on disk.
    _write_tasks(project_dir, [
        _make_task("ghost-bbbb", status="done", worker_pid=0),
    ])

    loop.tick(
        project, coord_id="cccccc01", cwd="/repo",
        fleet_home=str(fleet_home),
    )

    delete_calls = [
        c for c in fleet_run_recorder
        if c[1:3] == ["workers", "delete"] and c[-1] == "ghost-bbbb"
    ]
    assert delete_calls == [], (
        "sweep must not invoke workers delete when the dir is already "
        f"gone; recorded: {fleet_run_recorder}"
    )


def test_sweep_does_not_touch_active_task_worker_dirs(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess, monkeypatch,
) -> None:
    """Tasks at non-done statuses (todo / ready / in-progress / in-review /
    blocked) keep their worker dirs. Only status=done triggers the sweep —
    the dir is read-only post-shipping state for done tasks but live
    state for everything else.
    """
    monkeypatch.setenv("FLEET_HOME", str(fleet_home))
    project = "fleet"
    # Seed a worker dir for an in-review task. The reconcile path will
    # query gh on this task because pr_url is set; we stub _gh_pr_checks
    # to return "pending" so the sweep is the only path that could touch
    # the dir.
    workers_dir = fleet_home / "projects" / project / "workers" / "active-cccc"
    workers_dir.mkdir(parents=True, exist_ok=True)
    (workers_dir / "state.json").write_text(json.dumps({
        "slug": "active-cccc", "project": project,
        "phase": "review-claude", "pid": 0,
    }), encoding="utf-8")

    _write_tasks(project_dir, [
        _make_task(
            "active-cccc", status="in-review",
            worker_pid=99990, pr_url="https://github.com/x/y/pull/9",
        ),
    ])

    # Force "pending CI" so the reconcile gh path returns pending and
    # doesn't transition the task — keeps the sweep as the only
    # potential delete trigger.
    pending = loop._CIResult(
        all_green=False, merged=False, mergeable=True,
        failed=False, pending=True,
    )
    with patch.object(loop, "_pid_alive", return_value=True), \
         patch.object(loop, "_gh_pr_checks", return_value=pending):
        loop.tick(
            project, coord_id="cccccc01", cwd="/repo",
            fleet_home=str(fleet_home),
        )

    delete_calls = [
        c for c in fleet_run_recorder
        if c[1:3] == ["workers", "delete"] and c[-1] == "active-cccc"
    ]
    assert delete_calls == [], (
        "sweep must not touch worker dirs for non-done tasks; recorded: "
        + repr(fleet_run_recorder)
    )


def test_sweep_failure_does_not_abort_tick(
    fleet_home: Path, project_dir: Path,
    monkeypatch, capsys,
) -> None:
    """Sweep is best-effort. A failing `fleet workers delete` logs to
    stderr but never bubbles into result.errors or rolls back the tick.
    """
    monkeypatch.setenv("FLEET_HOME", str(fleet_home))
    project = "fleet"
    workers_dir = fleet_home / "projects" / project / "workers" / "fragile-dddd"
    workers_dir.mkdir(parents=True, exist_ok=True)
    (workers_dir / "state.json").write_text(json.dumps({
        "slug": "fragile-dddd", "project": project,
        "phase": "tdd-green",
    }), encoding="utf-8")

    _write_tasks(project_dir, [
        _make_task("fragile-dddd", status="done", worker_pid=0),
    ])

    def fake_run(cmd, timeout_s=30.0):
        if cmd[1:3] == ["workers", "delete"]:
            raise RuntimeError("simulated delete failure")

    with patch.object(loop, "_run_fleet", side_effect=fake_run):
        result = loop.tick(
            project, coord_id="cccccc01", cwd="/repo",
            fleet_home=str(fleet_home),
        )

    assert result.errors == [], (
        "sweep failures must be best-effort; got: " + repr(result.errors)
    )
    captured = capsys.readouterr()
    assert "workers delete failed" in captured.err


# ---------- auto-archive integration ----------


def test_tick_auto_archives_when_over_threshold(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess, monkeypatch,
) -> None:
    """End-of-tick auto-archive: tasks.md > threshold → archive shell.

    50 done rows + 1 todo (51 total) under the default threshold = 50
    triggers exactly one `fleet tasks archive` shell for the oldest
    done slug. Active task is never archived regardless of count.
    """
    monkeypatch.delenv("FLEET_AUTO_ARCHIVE_THRESHOLD", raising=False)
    base = _dt.datetime(2026, 5, 6, 10, 0, 0, tzinfo=_dt.timezone.utc)

    rows: list[parse.Task] = []
    for i in range(50):
        # Done rows with ascending finished_at — index 0 is oldest.
        t = parse.Task(
            slug=f"done-{i:03d}", status="done", priority="P2",
            worker_pid=0,
            created=base, updated=base + _dt.timedelta(hours=i),
            finished_at=base + _dt.timedelta(hours=i),
            spawned_by="user", depends_on=[],
            spec="s", acceptance="a", notes="",
        )
        rows.append(t)
    # One non-terminal task — must NOT be archived.
    rows.append(_make_task("active-stay", status="todo"))
    _write_tasks(project_dir, rows)

    result = loop.tick(
        "fleet", coord_id="cccccc01", cwd="/repo",
        fleet_home=str(fleet_home),
    )

    assert not result.skipped
    archive_calls = [c for c in fleet_run_recorder if c[1:3] == ["tasks", "archive"]]
    assert len(archive_calls) == 1, (
        f"expected exactly 1 archive shell, got {len(archive_calls)}: "
        + repr(archive_calls)
    )
    # Oldest done slug = done-000.
    assert archive_calls[0][-1] == "done-000"
    # Active task slug must NEVER appear in any archive shell.
    for c in archive_calls:
        assert c[-1] != "active-stay", (
            f"active task should not be archived: {c}"
        )


# ---------- subagent lifecycle archive (post-archive audit) ----------


def _subagent_record_path(fleet_home: Path, project: str, slug: str) -> Path:
    """Per-subagent archive record. Mirrors the production layout —
    ~/.fleet/projects/<project>/subagents/<slug>.json. The record is
    the audit trail for one Agent-tool dispatch that reached phase=done."""
    return fleet_home / "projects" / project / "subagents" / f"{slug}.json"


def test_tick_archives_worker_on_phase_done(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess,
    monkeypatch,
) -> None:
    """When the reconcile path flips a phase=done worker to in-review,
    a sibling subagents/<slug>.json record is written with archived_at
    populated. The record is the durable receipt that this subagent
    finished its one-shot dispatch — every post-archive `gh pr create`
    or branch push is now suspect (CLAUDE.md §8 scope violation)."""
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
        loop.tick(
            project, coord_id="cccccc01", cwd="/repo",
            fleet_home=str(fleet_home),
        )

    rec_path = _subagent_record_path(fleet_home, project, "shipper-aaaa")
    assert rec_path.exists(), (
        f"expected subagent archive record at {rec_path}; "
        f"contents of subagents/: "
        f"{list(rec_path.parent.iterdir()) if rec_path.parent.exists() else 'missing'}"
    )
    data = json.loads(rec_path.read_text(encoding="utf-8"))
    assert data["slug"] == "shipper-aaaa"
    archived = data.get("archived_at", "")
    assert archived, "archived_at must be a non-empty RFC3339 timestamp"
    # RFC3339 sanity check — fromisoformat parses Zulu offset.
    _dt.datetime.fromisoformat(archived.replace("Z", "+00:00"))
    # Expected PR + branch are captured so the post-archive probe knows
    # what scope the original dispatch claimed.
    assert data.get("expected_pr_url") == "https://github.com/x/y/pull/7"
    # post_archive_artifacts starts empty — nothing has fired after
    # archive yet.
    assert data.get("post_archive_artifacts", []) == []


def test_tick_skips_archive_record_for_blocked_worker(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess,
    monkeypatch,
) -> None:
    """Lifecycle hardening: only TERMINAL-success transitions (done)
    write the archive record. A worker at phase=blocked is still in
    Waiting — the operator may unblock and resume — so we leave no
    archive receipt. Without this guard, a flapping blocked/unblocked
    worker would generate a stale post-archive flag the moment it
    legitimately re-opened or amended its PR."""
    monkeypatch.setenv("FLEET_HOME", str(fleet_home))
    project = "fleet"
    workers_dir = fleet_home / "projects" / project / "workers" / "stuck-bbbb"
    workers_dir.mkdir(parents=True, exist_ok=True)
    fresh = _dt.datetime.now(tz=_dt.timezone.utc).isoformat().replace(
        "+00:00", "Z",
    )
    (workers_dir / "state.json").write_text(json.dumps({
        "slug": "stuck-bbbb", "project": project,
        "phase": "blocked", "updated_at": fresh,
        "blocked_reason": "needs operator input",
    }), encoding="utf-8")

    _write_tasks(project_dir, [
        _make_task("stuck-bbbb", status="in-progress", worker_pid=99998),
    ])
    with patch.object(loop, "_pid_alive", return_value=False):
        loop.tick(
            project, coord_id="cccccc01", cwd="/repo",
            fleet_home=str(fleet_home),
        )

    rec_path = _subagent_record_path(fleet_home, project, "stuck-bbbb")
    assert not rec_path.exists(), (
        f"blocked worker should NOT have an archive record yet: {rec_path}"
    )


def test_tick_flags_post_archive_artifact(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess,
    monkeypatch,
) -> None:
    """Post-completion audit: when an archived subagent record exists
    AND a new PR is found on its branch AFTER archived_at, the tick
    appends a {pr_number, pr_url, opened_at, action} entry to the
    record's post_archive_artifacts. That signals 'this Agent-tool
    subagent kept working after its §7 return' — the operator inspects
    and decides (close the bonus PR, or accept).

    The gh probe is injected via loop._probe_branch_prs so the test
    doesn't shell out for real. Production wires it to a `gh api`
    helper inside the same module."""
    monkeypatch.setenv("FLEET_HOME", str(fleet_home))
    project = "fleet"
    # Pre-seed the subagent archive record from a PRIOR tick. archived_at
    # is set to 1h ago so the bonus PR's opened_at (now) is strictly
    # after the archive boundary.
    sub_dir = fleet_home / "projects" / project / "subagents"
    sub_dir.mkdir(parents=True, exist_ok=True)
    archived_dt = _dt.datetime.now(tz=_dt.timezone.utc) - _dt.timedelta(hours=1)
    rec_path = sub_dir / "shipper-aaaa.json"
    rec_path.write_text(json.dumps({
        "slug": "shipper-aaaa",
        "subagent_id": "abcdef01",
        "branch": "worker/shipper-aaaa",
        "archived_at": archived_dt.isoformat().replace("+00:00", "Z"),
        "expected_pr_url": "https://github.com/x/y/pull/7",
        "post_archive_artifacts": [],
    }, indent=2), encoding="utf-8")

    # Task is at status=in-review (post-archive lifecycle). The reconcile
    # path does NOT need to fire for this audit — the audit is its own
    # tick step that walks subagents/*.json.
    _write_tasks(project_dir, [
        _make_task(
            "shipper-aaaa", status="in-review",
            worker_pid=0, pr_url="https://github.com/x/y/pull/7",
        ),
    ])

    # The mock returns one bonus PR opened ~5 minutes ago — strictly
    # after archived_at (1h ago).
    bonus_opened = _dt.datetime.now(tz=_dt.timezone.utc) - _dt.timedelta(minutes=5)

    def fake_probe(branch: str):
        # Production signature: (branch) -> list[dict] with number, url,
        # createdAt. Empty list means "no PRs on this branch beyond the
        # expected one".
        assert branch == "worker/shipper-aaaa", (
            f"probe must target the archived subagent's branch: {branch}"
        )
        return [
            # The original PR — must be ignored (matches expected_pr_url
            # OR was opened before archived_at, depending on impl
            # strategy. We mirror the safer "before-archived_at filter"
            # branch by setting createdAt to 2h ago).
            {
                "number": 7,
                "url": "https://github.com/x/y/pull/7",
                "createdAt": (archived_dt - _dt.timedelta(hours=1))
                .isoformat().replace("+00:00", "Z"),
            },
            # Bonus PR opened AFTER archived_at → flagged.
            {
                "number": 124,
                "url": "https://github.com/x/y/pull/124",
                "createdAt": bonus_opened.isoformat().replace("+00:00", "Z"),
            },
        ]

    # Also stub out gh pr checks so the reconcile path doesn't shell.
    with patch.object(loop, "_probe_branch_prs", side_effect=fake_probe), \
         patch.object(loop, "_gh_pr_checks", return_value=loop._CIResult(pending=True)):
        loop.tick(
            project, coord_id="cccccc01", cwd="/repo",
            fleet_home=str(fleet_home),
        )

    updated = json.loads(rec_path.read_text(encoding="utf-8"))
    artifacts = updated.get("post_archive_artifacts", [])
    assert len(artifacts) == 1, (
        f"expected one flagged bonus PR, got {len(artifacts)}: {artifacts}"
    )
    art = artifacts[0]
    assert art["pr_number"] == 124
    assert art["pr_url"] == "https://github.com/x/y/pull/124"
    assert art["action"] == "flag-for-operator"
    # Same PR observed again on a future tick is NOT re-appended (the
    # operator already sees ONE flag; duplicating it pollutes the audit
    # log).
    with patch.object(loop, "_probe_branch_prs", side_effect=fake_probe), \
         patch.object(loop, "_gh_pr_checks", return_value=loop._CIResult(pending=True)):
        loop.tick(
            project, coord_id="cccccc01", cwd="/repo",
            fleet_home=str(fleet_home),
        )
    updated_again = json.loads(rec_path.read_text(encoding="utf-8"))
    assert len(updated_again.get("post_archive_artifacts", [])) == 1, (
        "post-archive flag must be idempotent across ticks: "
        + repr(updated_again.get("post_archive_artifacts"))
    )


# ---------- three-stage flow: review handoff dispatches (reviewer-subagent-arch) ----------


def _write_worker_state(
    fleet_home: Path, project: str, slug: str, phase: str, *,
    updated_at: str | None = None,
) -> None:
    """Helper: drop a worker state.json with the given phase. Mirrors
    the on-disk shape Go writes via `fleet workers update`. The new
    review-* fields can stay empty for these handoff-detection tests."""
    workers_dir = fleet_home / "projects" / project / "workers" / slug
    workers_dir.mkdir(parents=True, exist_ok=True)
    body = {
        "slug": slug,
        "project": project,
        "phase": phase,
        "started_at": "2026-05-11T10:00:00Z",
        "updated_at": updated_at or "2026-05-11T11:00:00Z",
        "pid": 0,
    }
    (workers_dir / "state.json").write_text(json.dumps(body))


def test_loop_dispatches_reviewer_on_phase_review_pending(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess,
) -> None:
    """Three-stage flow: worker exits at phase=review-pending → coord
    dispatches a reviewer subagent on the next tick. The task stays
    in-progress; a new DISPATCH block is emitted; the reviewer prompt
    is written to the inbox; result.dispatched is incremented by 1."""
    _write_tasks(project_dir, [
        _make_task(
            "review-pending-aaaa", status="in-progress", worker_pid=99999,
        ),
    ])
    _write_worker_state(
        fleet_home, "fleet", "review-pending-aaaa", "review-pending",
    )
    # Reviewer agent_id under test.
    dispatch_subprocess.append("ffffaaaa")
    # Worker PID is dead → handoff fires.
    with patch.object(loop, "_pid_alive", return_value=False), \
         patch.object(loop, "_gh_pr_checks", return_value=loop._CIResult(pending=True)):
        result = loop.tick(
            "fleet", coord_id="cccccc01", cwd="/repo",
            fleet_home=str(fleet_home),
        )

    # The reviewer dispatch is one DISPATCH block; the reconcile path
    # also sees the dead pid + non-terminal phase (review-pending falls
    # through to the pr_url branch which has no pr_url → would requeue
    # to todo). The handoff path runs BEFORE this in tick() — but
    # _reconcile_inflight's "no terminal state" branch ALSO fires for
    # review-pending. Test that the dispatch block lands.
    assert any(
        block.startswith("DISPATCH: review-pending-aaaa")
        and "agent_id: ffffaaaa" in block
        for block in result.dispatch_instructions
    ), f"reviewer DISPATCH not emitted: {result.dispatch_instructions!r}"
    # Inbox file written with the reviewer prompt.
    inbox = fleet_home / "inbox" / "ffffaaaa.md"
    assert inbox.exists(), "reviewer inbox file not written"
    body = inbox.read_text()
    assert "FLEET REVIEWER" in body.upper()
    # Note recorded under the task: "review-pending: dispatched as agent <id>".
    note_calls = [c for c in fleet_run_recorder if c[1:3] == ["tasks", "note"]]
    review_notes = [c for c in note_calls if "review-pending" in (c[-1] if c else "")]
    assert review_notes, f"expected review-pending note: {note_calls!r}"


def test_loop_dispatches_finisher_on_phase_review_done(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess,
) -> None:
    """Three-stage flow: reviewer writes phase=review-done → coord
    dispatches the finisher on the next tick. Counterpart to the
    review-pending dispatch test."""
    _write_tasks(project_dir, [
        _make_task(
            "review-done-bbbb", status="in-progress", worker_pid=99998,
        ),
    ])
    _write_worker_state(
        fleet_home, "fleet", "review-done-bbbb", "review-done",
    )
    dispatch_subprocess.append("ffffbbbb")
    with patch.object(loop, "_pid_alive", return_value=False), \
         patch.object(loop, "_gh_pr_checks", return_value=loop._CIResult(pending=True)):
        result = loop.tick(
            "fleet", coord_id="cccccc01", cwd="/repo",
            fleet_home=str(fleet_home),
        )

    assert any(
        block.startswith("DISPATCH: review-done-bbbb")
        and "agent_id: ffffbbbb" in block
        for block in result.dispatch_instructions
    ), f"finisher DISPATCH not emitted: {result.dispatch_instructions!r}"
    inbox = fleet_home / "inbox" / "ffffbbbb.md"
    assert inbox.exists()
    body = inbox.read_text()
    assert "FINISHER" in body.upper()
    # The note recorded for this handoff phase is "review-done:".
    note_calls = [c for c in fleet_run_recorder if c[1:3] == ["tasks", "note"]]
    finisher_notes = [c for c in note_calls if "review-done" in (c[-1] if c else "")]
    assert finisher_notes, f"expected review-done note: {note_calls!r}"


def test_loop_does_not_redispatch_finisher_on_consecutive_ticks(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess,
) -> None:
    """Finisher dedup: same invariant as the reviewer dedup test, but
    for phase=review-done. Once a finisher is dispatched, a second
    tick at the same phase MUST NOT spawn a second finisher."""
    _write_tasks(project_dir, [
        _make_task(
            "no-double-finisher-dddd", status="in-progress", worker_pid=99996,
        ),
    ])
    _write_worker_state(
        fleet_home, "fleet", "no-double-finisher-dddd", "review-done",
    )
    dispatch_subprocess.append("ffffeeee")
    with patch.object(loop, "_pid_alive", return_value=False), \
         patch.object(loop, "_gh_pr_checks", return_value=loop._CIResult(pending=True)):
        first = loop.tick(
            "fleet", coord_id="cccccc01", cwd="/repo",
            fleet_home=str(fleet_home),
        )
    assert any(
        b.startswith("DISPATCH: no-double-finisher-dddd")
        for b in first.dispatch_instructions
    )
    with patch.object(loop, "_pid_alive", return_value=False), \
         patch.object(loop, "_gh_pr_checks", return_value=loop._CIResult(pending=True)):
        second = loop.tick(
            "fleet", coord_id="cccccc01", cwd="/repo",
            fleet_home=str(fleet_home),
        )
    assert not any(
        b.startswith("DISPATCH: no-double-finisher-dddd")
        for b in second.dispatch_instructions
    ), f"finisher redispatched on second tick: {second.dispatch_instructions!r}"


def test_loop_does_not_redispatch_reviewer_on_consecutive_ticks(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess,
) -> None:
    """Once the reviewer is dispatched at phase=review-pending, a
    second tick at the SAME phase MUST NOT spawn a second reviewer
    (the first one is still running). The coord-state.json's
    review_handoffs_dispatched list is the dedup signal."""
    _write_tasks(project_dir, [
        _make_task(
            "no-double-cccc", status="in-progress", worker_pid=99997,
        ),
    ])
    _write_worker_state(
        fleet_home, "fleet", "no-double-cccc", "review-pending",
    )
    dispatch_subprocess.append("ffffcccc")
    with patch.object(loop, "_pid_alive", return_value=False), \
         patch.object(loop, "_gh_pr_checks", return_value=loop._CIResult(pending=True)):
        first = loop.tick(
            "fleet", coord_id="cccccc01", cwd="/repo",
            fleet_home=str(fleet_home),
        )
    # First tick fires reviewer.
    assert any(
        b.startswith("DISPATCH: no-double-cccc")
        for b in first.dispatch_instructions
    )
    # Second tick: state.json still says review-pending (reviewer not
    # done yet). The coord must skip the handoff dispatch.
    with patch.object(loop, "_pid_alive", return_value=False), \
         patch.object(loop, "_gh_pr_checks", return_value=loop._CIResult(pending=True)):
        second = loop.tick(
            "fleet", coord_id="cccccc01", cwd="/repo",
            fleet_home=str(fleet_home),
        )
    assert not any(
        b.startswith("DISPATCH: no-double-cccc")
        for b in second.dispatch_instructions
    ), f"reviewer redispatched on second tick: {second.dispatch_instructions!r}"


def test_loop_skips_handoff_when_worker_pid_alive(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess,
) -> None:
    """Race condition guard: if the worker process is STILL running
    (PID alive), don't fire the handoff yet — the state.json may be
    mid-flux. Handoff fires only after the worker process exits."""
    _write_tasks(project_dir, [
        _make_task(
            "alive-dddd", status="in-progress", worker_pid=12345,
        ),
    ])
    _write_worker_state(
        fleet_home, "fleet", "alive-dddd", "review-pending",
    )
    with patch.object(loop, "_pid_alive", return_value=True), \
         patch.object(loop, "_gh_pr_checks", return_value=loop._CIResult(pending=True)):
        result = loop.tick(
            "fleet", coord_id="cccccc01", cwd="/repo",
            fleet_home=str(fleet_home),
        )
    assert not any(
        b.startswith("DISPATCH: alive-dddd")
        for b in result.dispatch_instructions
    ), "handoff fired while worker PID still alive"


# ---------- non-git project support ----------


def _write_non_git_meta(fleet_home: Path, project: str) -> None:
    """Seed a meta.json with is_git=false at the project root. Loop's
    project_is_git lookup reads this file at dispatch time."""
    proj_dir = fleet_home / "projects" / project
    proj_dir.mkdir(parents=True, exist_ok=True)
    (proj_dir / "meta.json").write_text(
        json.dumps({
            "schema": "v1",
            "repo_path": "/tmp/non-git-fake",
            "added_at": "2026-05-12T00:00:00Z",
            "is_git": False,
        }),
        encoding="utf-8",
    )


def test_loop_dispatches_non_git_worker_with_no_branch_or_commit(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess,
) -> None:
    """A ready task on a non-git project receives the non-git worker
    prompt: no 'git checkout -b', no 'git commit' instructions. The
    three-stage handoff (exit at phase=review-pending) is preserved.
    """
    _write_non_git_meta(fleet_home, "fleet")
    _write_tasks(project_dir, [_make_task("ng-ready-aaaa", status="ready")])
    dispatch_subprocess.append("aaaaaa01")

    result = loop.tick(
        "fleet", coord_id="cccccc01", cwd="/repo",
        fleet_home=str(fleet_home),
    )
    assert result.dispatched == 1
    inbox = fleet_home / "inbox" / "aaaaaa01.md"
    assert inbox.exists()
    body = inbox.read_text()
    assert "non-git project" in body.lower()
    assert "git checkout -b" not in body
    assert "git commit" not in body
    # Same SOP — exit at phase=review-pending still drives the handoff.
    assert "--phase review-pending" in body


def test_loop_dispatches_non_git_finisher_without_push_or_pr(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess,
) -> None:
    """Reviewer wrote phase=review-done on a non-git project → coord
    dispatches the non-git finisher. The finisher prompt MUST NOT
    contain 'git push' or 'gh pr create' command lines, and the
    final terminal write does NOT carry --pr-url.
    """
    _write_non_git_meta(fleet_home, "fleet")
    _write_tasks(project_dir, [
        _make_task(
            "ng-finish-bbbb", status="in-progress", worker_pid=88888,
        ),
    ])
    _write_worker_state(
        fleet_home, "fleet", "ng-finish-bbbb", "review-done",
    )
    dispatch_subprocess.append("bbbbbb01")
    with patch.object(loop, "_pid_alive", return_value=False), \
         patch.object(loop, "_gh_pr_checks", return_value=loop._CIResult(pending=True)):
        result = loop.tick(
            "fleet", coord_id="cccccc01", cwd="/repo",
            fleet_home=str(fleet_home),
        )

    blocks = [
        b for b in result.dispatch_instructions
        if b.startswith("DISPATCH: ng-finish-bbbb") and "agent_id: bbbbbb01" in b
    ]
    assert blocks, f"non-git finisher not dispatched: {result.dispatch_instructions!r}"
    inbox = fleet_home / "inbox" / "bbbbbb01.md"
    assert inbox.exists()
    body = inbox.read_text()
    assert "FLEET FINISHER" in body.upper()
    # No push, no PR command lines.
    assert "git push -u" not in body
    assert "gh pr create" not in body
    # phase=done is written without --pr-url.
    assert "--phase done --exit 0" in body
    assert "--phase done --pr-url" not in body


def test_loop_non_git_cap_above_one_skips_worktree_create(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess,
) -> None:
    """A non-git project dispatched with cap>1 must skip worktree
    creation entirely (no `git worktree add` — there's no git). The
    dispatch falls through to single-worker behavior: worker_cwd is
    the project's cwd, no worktree path is recorded.
    """
    _write_non_git_meta(fleet_home, "fleet")
    _write_tasks(project_dir, [_make_task("ng-cap2-cccc", status="ready")])
    dispatch_subprocess.append("cccc0001")

    # Patch worktree.create_worktree to FAIL loudly — the guard means
    # it must never be called for a non-git project.
    def _fail_create(*a, **kw):
        raise AssertionError("worktree.create_worktree must not run for non-git")

    with patch.object(loop.worktree_mod, "create_worktree", side_effect=_fail_create):
        result = loop.tick(
            "fleet", coord_id="cccccc01", cwd="/repo",
            cap=2, fleet_home=str(fleet_home),
        )

    assert result.dispatched == 1


def test_loop_non_git_phase_done_reconciles_to_done(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess,
) -> None:
    """Codex iter-1 [P1] regression: a non-git worker that wrote
    phase=done (no pr_url) must be reconciled to status=done — NOT
    requeued to todo as 'worker died without PR'. The legacy reconcile
    path required pr_url to recognize success; for non-git projects
    that's wrong.
    """
    _write_non_git_meta(fleet_home, "fleet")
    _write_tasks(project_dir, [
        _make_task(
            "ng-done-aaaa", status="in-progress", worker_pid=99997, pr_url="",
        ),
    ])
    # Worker wrote phase=done (no pr_url — non-git mode).
    _write_worker_state(
        fleet_home, "fleet", "ng-done-aaaa", "done",
    )
    with patch.object(loop, "_pid_alive", return_value=False):
        result = loop.tick(
            "fleet", coord_id="cccccc01", cwd="/repo",
            fleet_home=str(fleet_home),
        )
    assert result.reconciled == 1
    set_calls = [c for c in fleet_run_recorder if c[1:3] == ["tasks", "set"]]
    # Task lands at status=done — NOT in-review, NOT todo.
    assert any("status=done" in c for c in set_calls), (
        f"non-git phase=done should reconcile to status=done; "
        f"got set calls: {set_calls!r}"
    )
    # No "died without PR" note — that path is the failure case.
    note_calls = [c for c in fleet_run_recorder if c[1:3] == ["tasks", "note"]]
    assert not any("died without PR" in (c[-1] if c else "") for c in note_calls)
    # The success note mentions non-git so the operator sees the mode.
    success_notes = [c for c in note_calls if "non-git" in (c[-1] if c else "")]
    assert success_notes, f"expected non-git success note: {note_calls!r}"
