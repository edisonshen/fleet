"""S5 (scenario-testing PR-4): a real coordinator tick against the
`fleet_sandbox` fixture — the BUILT fleet binary, real tmux on an isolated
socket, the fake `claude`, real filesystem state. No stubs on the dispatch
path: `loop.tick` decides, `fleet dispatch` spawns, the sandbox records it.

Not a unit test of loop.tick (test_loop.py owns that). It proves the
fixture stands the product up and tears it back down, so coordinator
scenarios can be written as: seed → tick() → assert on tasks.md / workers.
"""
from __future__ import annotations

import glob
import json
import os
import subprocess


def test_fleet_sandbox_tick_dispatches_ready_task(fleet_sandbox):
    sb = fleet_sandbox
    assert os.path.isdir(os.path.join(sb.home, "agents"))
    assert os.path.isdir(os.path.join(sb.home, "projects", sb.project))
    assert sb.home.startswith("/tmp/fleet-test-pytest-")
    assert sb.env["FLEET_TMUX_SOCKET"] == sb.home + ".sock"

    # The registered repo dir must be a git repo for `fleet dispatch` to cut
    # the worker's worktree; `up` leaves it plain so scenarios choose.
    repo = os.path.join(sb.home, "repo")
    subprocess.run(["git", "init", "-q", "-b", "main"], cwd=repo, check=True)
    subprocess.run(
        ["git", "-c", "user.email=t@example.com", "-c", "user.name=t",
         "commit", "-q", "--allow-empty", "-m", "init"],
        cwd=repo, check=True,
    )

    # Operator flow: file the task (todo), then promote it (ready).
    added = sb.run([
        sb.bin, "tasks", "add", "--project", sb.project, "--slug", "smoke-0001",
        "--priority", "P1", "--spec", "S5 smoke task",
    ])
    assert added.returncode == 0, added.stderr
    promoted = sb.run([sb.bin, "tasks", "promote", "--project", sb.project, "smoke-0001"])
    assert promoted.returncode == 0, promoted.stderr

    res = sb.tick()

    assert not res.skipped, res.reason
    assert res.errors == []
    assert res.dispatched == 1

    tasks_md = open(os.path.join(sb.home, "projects", sb.project, "tasks.md")).read()
    assert "smoke-0001" in tasks_md and "in-progress" in tasks_md, tasks_md
    assert "status: ready" not in tasks_md, tasks_md

    states = glob.glob(os.path.join(sb.home, "projects", sb.project, "workers", "*", "state.json"))
    assert len(states) == 1, states
    state = json.loads(open(states[0]).read())
    assert state["slug"] == "smoke-0001", state
    assert state["project"] == sb.project
    assert state["phase"] == "starting"

    # Every mutation went through the built binary: the note it appended
    # names the dispatch, and `fleet tasks` reads the same in-progress row.
    assert "dispatched as agent" in tasks_md, tasks_md
    shown = sb.run([sb.bin, "tasks", "list", "--project", sb.project])
    assert shown.returncode == 0, shown.stderr
    assert state["slug"] in shown.stdout and "in-progress" in shown.stdout, shown.stdout
