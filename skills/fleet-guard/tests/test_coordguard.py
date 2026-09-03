"""Coordinator delegation guard — user-scenario tests.

Everything goes through the real hook entrypoint (main.main with a
PreToolUse / Stop / SessionStart payload), the way Claude Code drives it.
Three scenarios: a coord that tries to implement, a coord doing its actual
job, and sessions the guard must leave alone.
"""
from __future__ import annotations

import io
import json
from pathlib import Path

import pytest

import coordguard
import main as fleet_main

AGENT = "c0ffee01"


@pytest.fixture(autouse=True)
def home(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> Path:
    """A live coord session: FLEET_HOME under tmp, FLEET_ROLE=coord, guard on,
    agent record present, and a fake checkout with a docs/ symlink escape."""
    home = tmp_path / "fleet"
    monkeypatch.setenv("FLEET_HOME", str(home))
    monkeypatch.setenv("FLEET_AGENT_ID", AGENT)
    monkeypatch.setenv("FLEET_ROLE", "coord")
    monkeypatch.delenv("FLEET_COORD_GUARD", raising=False)
    (home / "agents").mkdir(parents=True)
    (home / "agents" / f"{AGENT}.json").write_text(
        json.dumps({"id": AGENT, "is_coord": True, "needs_input": False}))
    repo = tmp_path / "repo"
    (repo / "docs").mkdir(parents=True)
    (repo / "internal").mkdir()
    (repo / "docs" / "src").symlink_to(repo / "internal")
    monkeypatch.chdir(repo)
    return home


def hook(event: str, tool: str = "", capsys=None, **tool_input: object) -> str:
    """Run one hook event through main.py the way Claude Code does; return
    stdout (PreToolUse/Stop emit JSON, SessionStart emits plain text)."""
    payload: dict = {"hook_event_name": event}
    if tool:
        payload |= {"tool_name": tool, "tool_input": tool_input}
    assert fleet_main.main(io.StringIO(json.dumps(payload))) == 0
    return capsys.readouterr().out.strip()


def denied(out: str) -> bool:
    return bool(out) and json.loads(out)["hookSpecificOutput"]["permissionDecision"] == "deny"


# -- Scenario 1: coord tries to do a worker's job --------------------------------

IMPLEMENTATION_ATTEMPTS = [
    ("Edit", {"file_path": "internal/spawn/spawn.go"}),
    ("Write", {"file_path": "docs/../cmd/fleet/main.go"}),   # `..` out of docs
    ("Write", {"file_path": "docs/src/x.go"}),               # symlink out of docs
    ("Write", {"file_path": "internal/docs_test.go"}),       # "docs" only in filename
    ("Bash", {"command": "go test -race ./..."}),
    ("Bash", {"command": "cd repo && python3 -m pytest skills/ -q"}),
    ("Bash", {"command": "git add -A && git commit -m fix && git push"}),
    ("Bash", {"command": "git checkout -- internal/x.go"}),
    ("Bash", {"command": "sed -i 's/a/b/' internal/x.go"}),
    ("Bash", {"command": "gofmt -w ."}),
    ("Bash", {"command": "echo a > docs/x.md; echo b > internal/x.go"}),
    ("Bash", {"command": "sudo rm -rf internal/"}),
    ("Bash", {"command": "env FOO=1 go test ./..."}),
    ("Bash", {"command": "find . -name '*.go' | xargs rm"}),
    ("Bash", {"command": "python3 -c 'open(\"x.go\",\"w\")'"}),
]


def test_coord_implementing_is_denied_logged_and_nagged(home: Path, capsys) -> None:
    for tool, tool_input in IMPLEMENTATION_ATTEMPTS:
        out = hook("PreToolUse", tool, capsys, **tool_input)
        assert denied(out), (tool, tool_input, out)
        assert "fleet tasks add" in out

    log = (home / "coord-violations" / f"{AGENT}.jsonl").read_text().splitlines()
    assert len(log) == len(IMPLEMENTATION_ATTEMPTS)
    assert all(json.loads(line)["denied"] for line in log)

    # Stop nags exactly once for the batch, then goes quiet.
    n = len(IMPLEMENTATION_ATTEMPTS)
    stop = json.loads(hook("Stop", capsys=capsys))
    assert stop["decision"] == "block" and f"blocked {n} attempt(s)" in stop["reason"]
    assert hook("Stop", capsys=capsys) == ""


def test_coord_denial_is_advisory_when_guard_off(home: Path, monkeypatch, capsys) -> None:
    monkeypatch.setenv("FLEET_COORD_GUARD", "off")
    assert hook("PreToolUse", "Edit", capsys, file_path="internal/x.go") == ""
    assert coordguard.count_violations(AGENT) == 1  # still visible to the operator


# -- Scenario 2: coord doing its actual job -------------------------------------

COORD_WORKFLOW = [
    ("Read", {"file_path": "internal/spawn/spawn.go"}),
    ("Grep", {"pattern": "go test"}),
    ("Write", {"file_path": "docs/DESIGN-x.md"}),
    ("Edit", {"file_path": "docs/TASK-PLAN-y.md"}),
    ("Write", {"file_path": "{home}/inbox/abc.md"}),
    ("Bash", {"command": "fleet tasks add --project p 'run go test in CI'"}),
    ("Bash", {"command": "fleet tasks note --project p slug --section spec "
                         "'worker: rm -rf build/ first, then go test > out.txt'"}),
    ("Bash", {"command": "fleet rm c0ffee02"}),
    ("Bash", {"command": "git status && git log --grep=merge --oneline -- internal/am"}),
    ("Bash", {"command": "gh pr view 12 --json state"}),
    ("Bash", {"command": "rg -n 'pytest|npm test' skills/"}),
    ("Bash", {"command": "go build ./... 2>&1 | head"}),
    ("Bash", {"command": "mkdir -p docs/plans && echo plan > docs/plans/x.md"}),
    ("Bash", {"command": "python3 x.py > /dev/null"}),
]


def test_coord_planning_workflow_is_untouched(home: Path, capsys) -> None:
    for tool, tool_input in COORD_WORKFLOW:
        tool_input = {k: str(v).format(home=home) for k, v in tool_input.items()}
        assert hook("PreToolUse", tool, capsys, **tool_input) == "", (tool, tool_input)
    assert coordguard.count_violations(AGENT) == 0
    assert hook("Stop", capsys=capsys) == ""


def test_coord_resume_gets_role_reminder_and_stays_idle(home: Path, capsys) -> None:
    assert "coordinator" in hook("SessionStart", capsys=capsys)
    rec = json.loads((home / "agents" / f"{AGENT}.json").read_text())
    assert rec["needs_input"] is True


# -- Scenario 3: sessions the guard must leave alone ----------------------------

@pytest.mark.parametrize("env", [
    pytest.param({"FLEET_ROLE": "worker"}, id="worker"),
    pytest.param({"FLEET_AGENT_ID": None, "FLEET_ROLE": None}, id="plain-claude"),
])
def test_non_coord_sessions_untouched(env: dict, monkeypatch, capsys) -> None:
    for k, v in env.items():
        monkeypatch.delenv(k) if v is None else monkeypatch.setenv(k, v)
    assert hook("PreToolUse", "Edit", capsys, file_path="internal/x.go") == ""
    assert hook("PreToolUse", "Bash", capsys, command="go test ./... && git push") == ""
    assert hook("SessionStart", capsys=capsys) == ""


def test_agent_subagent_inside_coord_may_implement(capsys) -> None:
    payload = {"hook_event_name": "PreToolUse", "agent_id": "sub-1",
               "tool_name": "Edit", "tool_input": {"file_path": "internal/x.go"}}
    assert fleet_main.main(io.StringIO(json.dumps(payload))) == 0
    assert capsys.readouterr().out == ""


def test_role_falls_back_to_agent_record(monkeypatch, capsys) -> None:
    monkeypatch.delenv("FLEET_ROLE")
    assert denied(hook("PreToolUse", "Edit", capsys, file_path="internal/x.go"))
