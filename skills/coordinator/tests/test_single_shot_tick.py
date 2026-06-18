"""Single-shot agent tick (P0 stopgap, Slice 1 of
DESIGN-coord-supervisor-in-daemon).

The agent-invoked tick must be SINGLE-SHOT by default: it does one
reconcile/drain/dispatch pass and returns, never entering the 30-min
in-turn `_run_supervisor` poll loop that held `coordinator.lock` and
SIGPIPE-killed the process (exit 144). The loop is opt-in via
FLEET_COORD_IN_TURN_SUPERVISOR (default OFF), project-agnostic.

Tests map 1:1 to docs/TASK-PLAN-coord-tick-single-shot.md "Tests":

  1. Single-shot default       — _run_supervisor NOT entered by default
  2. Lock hold bound           — lock released before tick() returns
  3. Opt-in still works        — env=1 restores the in-turn loop
  4. Project-agnostic          — rainier AND fleet both single-shot
  5. SIGPIPE -> clean exit      — closed stdout => handled exit, NOT 144
  6. DISPATCH still emitted     — single-shot tick still emits one block
  7. Reconcile still runs       — dead worker reconciled under single-shot
"""
from __future__ import annotations

import datetime as _dt
import os
import subprocess
import sys
from pathlib import Path
from unittest.mock import patch

import pytest

import dispatch
import loop
import parse


# ---------- local fixtures (mirror test_loop.py, kept self-contained) ----------


@pytest.fixture
def fleet_home(tmp_path: Path) -> Path:
    home = tmp_path / "fleet"
    home.mkdir()
    (home / "inbox").mkdir()
    (home / "inbox" / "archive").mkdir()
    (home / "projects").mkdir()
    return home


def _project_dir(fleet_home: Path, project: str) -> Path:
    p = fleet_home / "projects" / project
    p.mkdir(parents=True, exist_ok=True)
    (p / ".locks").mkdir(exist_ok=True)
    return p


@pytest.fixture
def project_dir(fleet_home: Path) -> Path:
    return _project_dir(fleet_home, "fleet")


@pytest.fixture
def fleet_run_recorder():
    calls: list[list[str]] = []

    def fake_run(cmd, timeout_s=30.0):
        calls.append(list(cmd))

    with patch.object(loop, "_run_fleet", side_effect=fake_run):
        yield calls


class _DispatchSubprocessHandle(list):
    def __init__(self) -> None:
        super().__init__()
        self.seen_cmds: list[list[str]] = []


@pytest.fixture
def dispatch_subprocess(monkeypatch):
    """Stub dispatch.subprocess.run for standards/learnings/claims and
    pin mint_agent_id from a test-supplied stack. Mirrors test_loop.py's
    fixture so this module is self-contained."""
    ids = _DispatchSubprocessHandle()

    def fake_run(cmd, capture_output=True, text=True, timeout=None, check=False,
                 input=None, env=None):
        ids.seen_cmds.append(list(cmd))
        if cmd[1:3] == ["standards", "show"]:
            return subprocess.CompletedProcess(
                args=cmd, returncode=0, stdout="# Standards\n", stderr="")
        if cmd[1:3] == ["learnings", "list"]:
            return subprocess.CompletedProcess(
                args=cmd, returncode=0, stdout="", stderr="")
        if cmd[1:3] == ["claims", "acquire-prompt"]:
            agent_id = cmd[3]
            fh = (env or os.environ).get("FLEET_HOME") or os.path.expanduser("~/.fleet")
            inbox_dir = os.path.join(fh, "inbox")
            os.makedirs(inbox_dir, exist_ok=True)
            path = os.path.join(inbox_dir, f"{agent_id}.md")
            body = input or ""
            if body and not body.endswith("\n"):
                body = body + "\n"
            with open(path, "w", encoding="utf-8") as f:
                f.write(body)
            return subprocess.CompletedProcess(
                args=cmd, returncode=0,
                stdout=(f'{{"outcome":"acquired","dispatch_id":"{agent_id}",'
                        f'"kind":"coord_prompt_inbox","path":"{path}"}}\n'),
                stderr="")
        if cmd[1:3] == ["claims", "release"]:
            agent_id = cmd[3]
            fh = (env or os.environ).get("FLEET_HOME") or os.path.expanduser("~/.fleet")
            path = os.path.join(fh, "inbox", f"{agent_id}.md")
            outcome = "released"
            try:
                os.unlink(path)
            except FileNotFoundError:
                outcome = "already_released"
            except OSError:
                pass
            return subprocess.CompletedProcess(
                args=cmd, returncode=0,
                stdout=(f'{{"outcome":"{outcome}","dispatch_id":"{agent_id}",'
                        f'"kind":"coord_prompt_inbox","path":"{path}"}}\n'),
                stderr="")
        return subprocess.CompletedProcess(
            args=cmd, returncode=0, stdout="", stderr="")

    monkeypatch.setattr(dispatch.subprocess, "run", fake_run)
    real_mint = dispatch.mint_agent_id

    def fake_mint() -> str:
        return ids.pop(0) if ids else real_mint()

    monkeypatch.setattr(dispatch, "mint_agent_id", fake_mint)
    return ids


def _make_task(slug, status="ready", *, worker_pid=0, pr_url=""):
    now = _dt.datetime(2026, 5, 6, 10, 0, 0, tzinfo=_dt.timezone.utc)
    return parse.Task(
        slug=slug, status=status, priority="P1",
        worker_pid=worker_pid, pr_url=pr_url, created=now, updated=now,
        spawned_by="user", depends_on=[], spec="spec", acceptance="acc",
        notes="", dispatch_generation=0,
    )


def _write_tasks(project_dir: Path, tasks: list[parse.Task]) -> None:
    project_dir.mkdir(parents=True, exist_ok=True)
    (project_dir / ".locks").mkdir(exist_ok=True)
    f = parse.File(schema=parse.SCHEMA_VERSION, tasks=tasks, footer="")
    parse.write(str(project_dir / "tasks.md"), f)


def _spy_supervisor(monkeypatch):
    """Replace loop._run_supervisor with a counter spy. Returns a dict
    whose `n` records how many times the in-turn loop was entered."""
    seen = {"n": 0}

    def _spy(**kwargs):
        seen["n"] += 1

    monkeypatch.setattr(loop, "_run_supervisor", _spy)
    return seen


# ===========================================================================
# Test 1 — Single-shot default: _run_supervisor NOT entered.
# Setup: 1 in-flight worker, poll_interval>0 (so ONLY the new gate keeps
# the loop off), default env (FLEET_COORD_IN_TURN_SUPERVISOR unset).
# ===========================================================================


def test_single_shot_default_does_not_enter_supervisor(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess, monkeypatch,
) -> None:
    monkeypatch.delenv("FLEET_COORD_IN_TURN_SUPERVISOR", raising=False)
    # poll_interval>0 + an in-flight worker is exactly the condition under
    # which the LEGACY tick entered _run_supervisor. The single-shot gate
    # must still keep it off.
    monkeypatch.setenv("FLEET_COORD_POLL_INTERVAL_S", "30")
    _write_tasks(project_dir, [
        _make_task("live-aaaa", status="in-progress", worker_pid=1),
    ])
    seen = _spy_supervisor(monkeypatch)

    with patch.object(loop, "_pid_alive", return_value=True):
        result = loop.tick(
            "fleet", coord_id="cccccc01", cwd="/repo",
            fleet_home=str(fleet_home),
        )

    assert seen["n"] == 0, "single-shot default must NOT enter _run_supervisor"
    assert not result.skipped


# ===========================================================================
# Test 2 — Lock hold bound: coordinator.lock is released before tick()
# returns (no across-loop hold). We assert the flock fd is None after the
# tick by re-acquiring the lock non-blocking from the same process.
# ===========================================================================


def test_lock_released_before_tick_returns(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess, monkeypatch,
) -> None:
    import fcntl

    monkeypatch.delenv("FLEET_COORD_IN_TURN_SUPERVISOR", raising=False)
    monkeypatch.setenv("FLEET_COORD_POLL_INTERVAL_S", "30")
    _write_tasks(project_dir, [
        _make_task("live-bbbb", status="in-progress", worker_pid=1),
    ])
    _spy_supervisor(monkeypatch)

    with patch.object(loop, "_pid_alive", return_value=True):
        loop.tick(
            "fleet", coord_id="cccccc01", cwd="/repo",
            fleet_home=str(fleet_home),
        )

    # After the tick the lock must be free: a fresh NB-flock succeeds.
    lock_path = project_dir / ".locks" / "coordinator.lock"
    fd = os.open(str(lock_path), os.O_CREAT | os.O_RDWR, 0o644)
    try:
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)  # raises if still held
        fcntl.flock(fd, fcntl.LOCK_UN)
    finally:
        os.close(fd)


# ===========================================================================
# Test 3 — Opt-in still works: FLEET_COORD_IN_TURN_SUPERVISOR=1 restores
# the in-turn loop (back-compat escape hatch).
# ===========================================================================


def test_opt_in_env_restores_supervisor(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess, monkeypatch,
) -> None:
    monkeypatch.setenv("FLEET_COORD_IN_TURN_SUPERVISOR", "1")
    monkeypatch.setenv("FLEET_COORD_POLL_INTERVAL_S", "30")
    _write_tasks(project_dir, [
        _make_task("live-cccc", status="in-progress", worker_pid=1),
    ])
    seen = _spy_supervisor(monkeypatch)

    with patch.object(loop, "_pid_alive", return_value=True):
        loop.tick(
            "fleet", coord_id="cccccc01", cwd="/repo",
            fleet_home=str(fleet_home),
        )

    assert seen["n"] == 1, "env=1 must restore the in-turn supervisor loop"


@pytest.mark.parametrize("val", ["0", "", "no", "off", "false", "  "])
def test_falsey_env_keeps_single_shot(val, monkeypatch) -> None:
    """Only truthy tokens enable the loop; everything else is single-shot."""
    monkeypatch.setenv("FLEET_COORD_IN_TURN_SUPERVISOR", val)
    assert loop._in_turn_supervisor_enabled() is False


@pytest.mark.parametrize("val", ["1", "true", "YES", "On", "TRUE"])
def test_truthy_env_enables(val, monkeypatch) -> None:
    monkeypatch.setenv("FLEET_COORD_IN_TURN_SUPERVISOR", val)
    assert loop._in_turn_supervisor_enabled() is True


def test_unset_env_is_single_shot(monkeypatch) -> None:
    monkeypatch.delenv("FLEET_COORD_IN_TURN_SUPERVISOR", raising=False)
    assert loop._in_turn_supervisor_enabled() is False


# ===========================================================================
# Test 4 — Project-agnostic: rainier AND fleet both single-shot under the
# same default. No project-name branch governs the gate.
# ===========================================================================


@pytest.mark.parametrize("project", ["projects-rainier", "fleet", "projects-fleet"])
def test_single_shot_is_project_agnostic(
    fleet_home: Path, fleet_run_recorder, dispatch_subprocess,
    monkeypatch, project,
) -> None:
    monkeypatch.delenv("FLEET_COORD_IN_TURN_SUPERVISOR", raising=False)
    monkeypatch.setenv("FLEET_COORD_POLL_INTERVAL_S", "30")
    monkeypatch.setenv("FLEET_PROJECT", project)
    pdir = _project_dir(fleet_home, project)
    _write_tasks(pdir, [
        _make_task("live-dddd", status="in-progress", worker_pid=1),
    ])
    seen = _spy_supervisor(monkeypatch)

    with patch.object(loop, "_pid_alive", return_value=True):
        loop.tick(
            project, coord_id="cccccc01", cwd="/repo",
            fleet_home=str(fleet_home),
        )

    assert seen["n"] == 0, f"{project}: in-turn supervisor must stay off"


def test_gate_helper_reads_no_project_signal(monkeypatch) -> None:
    """The gate is env-only — setting FLEET_PROJECT must not change it."""
    monkeypatch.delenv("FLEET_COORD_IN_TURN_SUPERVISOR", raising=False)
    monkeypatch.setenv("FLEET_PROJECT", "projects-rainier")
    assert loop._in_turn_supervisor_enabled() is False
    monkeypatch.setenv("FLEET_PROJECT", "projects-fleet")
    assert loop._in_turn_supervisor_enabled() is False


# ===========================================================================
# Test 5 — SIGPIPE -> clean exit. A closed stdout while emitting the tick's
# output must yield a HANDLED non-zero exit (_EXIT_BROKEN_PIPE), never a
# fatal SIGPIPE (128+13 = 141 on most platforms, "exit 144" in the bug
# report's shell). We exercise both write sites: the DISPATCH-block emit
# and the final JSON summary.
# ===========================================================================


class _BrokenStdout:
    """stdout whose write() raises BrokenPipeError, simulating
    `python3 loop.py X | head` after head closes the read end."""

    def __init__(self) -> None:
        self.writes = 0

    def write(self, _s):
        self.writes += 1
        raise BrokenPipeError(32, "Broken pipe")

    def flush(self):
        raise BrokenPipeError(32, "Broken pipe")

    def fileno(self):  # no real fd -> exercises the AttributeError-free fallback
        raise OSError("no fileno")

    def close(self):
        pass


def test_dispatch_emit_broken_pipe_is_handled_not_signal(
    monkeypatch, capsys,
) -> None:
    fake = loop.TickResult()
    fake.dispatched = 1
    fake.dispatch_instructions = ["DISPATCH: ready-aaaa\nEND_DISPATCH"]
    monkeypatch.setattr(loop, "tick", lambda *a, **kw: fake)
    monkeypatch.setenv("FLEET_PROJECT", "fleet")

    broken = _BrokenStdout()
    monkeypatch.setattr(loop.sys, "stdout", broken)

    rc = loop.main([])

    # Handled non-zero, and crucially NOT a signal-derived 14x code.
    assert rc == loop._EXIT_BROKEN_PIPE
    assert rc != 144 and rc != 141 and rc < 128
    assert broken.writes >= 1
    err = capsys.readouterr().err
    assert "broken" in err.lower() or "pipe" in err.lower()


def test_final_json_broken_pipe_is_handled_not_signal(
    monkeypatch, capsys,
) -> None:
    """No DISPATCH blocks -> the only write is the final JSON summary.
    A broken pipe there must still exit cleanly (handled), never 144."""
    fake = loop.TickResult()
    fake.dispatched = 0
    fake.dispatch_instructions = []
    monkeypatch.setattr(loop, "tick", lambda *a, **kw: fake)
    monkeypatch.setenv("FLEET_PROJECT", "fleet")

    broken = _BrokenStdout()
    monkeypatch.setattr(loop.sys, "stdout", broken)

    rc = loop.main([])

    assert rc == loop._EXIT_BROKEN_PIPE
    assert rc != 144 and rc != 141 and rc < 128
    err = capsys.readouterr().err
    assert "broken" in err.lower() or "pipe" in err.lower()


def test_real_subprocess_piped_through_head_never_exit_144(tmp_path: Path) -> None:
    """End-to-end: run loop.py as a real subprocess with stdout piped to a
    reader that closes early (like `| head`). The process must exit with a
    handled code, NEVER a signal-derived 14x / negative returncode.

    This is the regression that reproduces the operator's 'coord killed'
    (exit 144 = 128 + SIGPIPE) and proves it's gone.
    """
    skill_dir = Path(loop.__file__).resolve().parent
    # The driver builds many large DISPATCH blocks AT RUNTIME (not inlined
    # into argv) so the writer keeps going after the reader closes,
    # maximizing the chance of a SIGPIPE on an unguarded write.
    driver = (
        "import sys\n"
        f"sys.path.insert(0, {str(skill_dir)!r})\n"
        "import loop\n"
        "class FakeResult:\n"
        "    skipped=False; reason=''; parsed_tasks=0; reconciled=0\n"
        "    drained=0; dispatched=0; raised=0; errors=[]; self_exit=False\n"
        "    dispatch_instructions=['DISPATCH: t%d\\n%s\\nEND_DISPATCH' "
        "% (i, 'y'*4096) for i in range(4000)]\n"
        "loop.tick = lambda *a, **k: FakeResult()\n"
        "raise SystemExit(loop.main(['fleet']))\n"
    )
    driver_path = tmp_path / "driver.py"
    driver_path.write_text(driver, encoding="utf-8")
    # `head -c 16` reads a few bytes then closes the pipe -> the writer's
    # subsequent writes hit a broken pipe. In a shell pipeline the overall
    # status is the LAST command's (head, exit 0), so we capture the
    # WRITER's status via bash ${PIPESTATUS[0]} and print it on its own
    # line. That is the number that was 144 (128+SIGPIPE) before the fix.
    script = (
        f"python3 {_shquote(str(driver_path))} | head -c 16 >/dev/null; "
        'echo "WRITER_RC=${PIPESTATUS[0]}"'
    )
    proc = subprocess.run(
        ["bash", "-c", script],
        env={**os.environ, "FLEET_RC_BOOTSTRAP_DISABLED": "1"},
        capture_output=True, text=True, timeout=30,
    )
    line = next(
        ln for ln in proc.stdout.splitlines() if ln.startswith("WRITER_RC=")
    )
    writer_rc = int(line.split("=", 1)[1])
    # The WRITER must exit with the handled code, never a signal-derived
    # one. 128+13 = 141 is the bash exit for SIGPIPE; the operator's shell
    # reported 144. Either way: a value >= 128 means it was signal-killed.
    assert writer_rc == loop._EXIT_BROKEN_PIPE, (
        f"writer rc={writer_rc} (expected handled {loop._EXIT_BROKEN_PIPE}); "
        f"stderr={proc.stderr[-300:]!r}"
    )
    assert writer_rc != 141 and writer_rc != 144 and writer_rc < 128


def _shquote(s: str) -> str:
    import shlex

    return shlex.quote(s)


# ===========================================================================
# Test 6 — DISPATCH still emitted under single-shot: a ready task under cap
# still produces exactly one DISPATCH block + the registration path intact.
# ===========================================================================


def test_dispatch_block_emitted_under_single_shot(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess, monkeypatch,
) -> None:
    monkeypatch.delenv("FLEET_COORD_IN_TURN_SUPERVISOR", raising=False)
    monkeypatch.setenv("FLEET_COORD_POLL_INTERVAL_S", "30")
    _write_tasks(project_dir, [_make_task("ready-eeee", status="ready")])
    dispatch_subprocess.append("abcdef01")
    seen = _spy_supervisor(monkeypatch)

    result = loop.tick(
        "fleet", coord_id="cccccc01", cwd="/repo",
        fleet_home=str(fleet_home),
    )

    assert seen["n"] == 0
    assert result.dispatched == 1
    assert len(result.dispatch_instructions) == 1
    block = result.dispatch_instructions[0]
    assert block.startswith("DISPATCH: ready-eeee")
    assert "agent_id: abcdef01" in block
    assert block.rstrip().endswith("END_DISPATCH")
    # Registration path intact: inbox stub written + status flipped.
    assert (fleet_home / "inbox" / "abcdef01.md").exists()
    set_calls = [c for c in fleet_run_recorder if c[1:3] == ["tasks", "set"]]
    assert any("status=in-progress" in c for c in set_calls)


# ===========================================================================
# Test 7 — Reconcile still runs under single-shot: a worker whose pid is
# dead is reconciled per existing rules (no regression from gating the loop).
# ===========================================================================


def test_reconcile_runs_under_single_shot(
    fleet_home: Path, project_dir: Path,
    fleet_run_recorder, dispatch_subprocess, monkeypatch,
) -> None:
    monkeypatch.delenv("FLEET_COORD_IN_TURN_SUPERVISOR", raising=False)
    monkeypatch.setenv("FLEET_COORD_POLL_INTERVAL_S", "30")
    _write_tasks(project_dir, [
        _make_task("dying-ffff", status="in-progress", worker_pid=1, pr_url=""),
    ])
    seen = _spy_supervisor(monkeypatch)

    with patch.object(loop, "_pid_alive", return_value=False):
        result = loop.tick(
            "fleet", coord_id="cccccc01", cwd="/repo",
            fleet_home=str(fleet_home),
        )

    assert seen["n"] == 0, "reconcile path must not enter the in-turn loop"
    assert result.reconciled == 1
    set_calls = [c for c in fleet_run_recorder if c[1:3] == ["tasks", "set"]]
    assert any("status=todo" in c for c in set_calls)
    assert any("worker_pid=0" in c for c in set_calls)
