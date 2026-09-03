"""Tests for skills/fleet-guard/coordguard.py — the coordinator delegation
guard (PreToolUse deny + Stop nag).

Layout: one shared coord environment (fixture below) plus a fake repo tree;
each test class is a single parametrized table where a row states only the
condition that differs (tool/path/command/env) and the expected verdict.
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
    """Coord session env: FLEET_HOME under tmp, FLEET_ROLE=coord, guard on."""
    home = tmp_path / "fleet"
    monkeypatch.setenv("FLEET_HOME", str(home))
    monkeypatch.setenv("FLEET_AGENT_ID", AGENT)
    monkeypatch.setenv("FLEET_ROLE", "coord")
    monkeypatch.delenv("FLEET_COORD_GUARD", raising=False)
    return home


@pytest.fixture
def repo(tmp_path: Path) -> Path:
    """Fake checkout: repo/docs, repo/internal, repo/docs/src -> ../internal."""
    repo = tmp_path / "repo"
    (repo / "docs").mkdir(parents=True)
    (repo / "internal").mkdir()
    (repo / "docs" / "src").symlink_to(repo / "internal")
    return repo


def _payload(tool: str, **tool_input: object) -> dict:
    return {"hook_event_name": "PreToolUse", "tool_name": tool, "tool_input": tool_input}


def _write_record(home: Path, **fields: object) -> Path:
    rec = home / "agents" / f"{AGENT}.json"
    rec.parent.mkdir(parents=True, exist_ok=True)
    rec.write_text(json.dumps({"id": AGENT, **fields}))
    return rec


def _run_hook(payload: dict, capsys: pytest.CaptureFixture[str]) -> str:
    assert fleet_main.main(io.StringIO(json.dumps(payload))) == 0
    return capsys.readouterr().out


# -- classify: edit tools ------------------------------------------------------
# (tool, path template, denied). Templates may reference {repo} and {home}.

EDIT_CASES = [
    *[(t, "/repo/internal/spawn/spawn.go", True) for t in sorted(coordguard.EDIT_TOOLS)],
    ("Write", "/repo/docs/DESIGN-x.md", False),
    ("Edit", "docs/TASK-PLAN-y.md", False),
    ("Write", "{home}/inbox/abc.md", False),
    ("Read", "/repo/main.go", False),
    # docs/ escapes: `..`, symlink out of docs, docs as leaf or filename prefix
    ("Write", "/repo/docs/../main.go", True),
    ("Write", "/repo/docs/./../internal/x.go", True),
    ("Write", "docs/../cmd/fleet/main.go", True),
    ("Write", "{repo}/docs/src/x.go", True),
    ("Write", "/repo/docs", True),
    ("Write", "/repo/docs.go", True),
    ("Write", "/repo/internal/docs_test.go", True),
    # a repo living under the system tmp dir is still source
    ("Write", "{repo}/main.go", True),
]


class TestEditTools:
    @pytest.mark.parametrize("tool,path,denied", EDIT_CASES)
    def test_verdict(self, tool: str, path: str, denied: bool,
                     repo: Path, home: Path) -> None:
        path = path.format(repo=repo, home=home)
        got = coordguard.classify(_payload(tool, file_path=path))
        assert (got is not None) == denied, got
        if denied:
            assert Path(path).name in got

    def test_grep_tool_allowed(self) -> None:
        assert coordguard.classify(_payload("Grep", pattern="x")) is None

    def test_subagent_exempt(self) -> None:
        payload = _payload("Edit", file_path="/repo/main.go")
        payload["agent_id"] = "sub-123"
        assert coordguard.classify(payload) is None


# -- classify: bash ------------------------------------------------------------

BASH_DENIED = [
    "go test ./...",
    "cd repo && go test -race ./internal/...",
    "python3 -m pytest skills/ -q",
    "pytest -q",
    "npm test",
    "git add -A && git commit -m 'x'",
    "git push origin HEAD",
    "git rebase main",
    "git checkout -- internal/x.go",
    "git checkout -b feature",
    "git switch main",
    "git clean -fd",
    "git -C /repo commit -m x",
    "sed -i 's/a/b/' internal/x.go",
    "gofmt -w internal/x.go",
    "gofmt -l -w .",
    "goimports -w .",
    "black skills/",
    "ruff check --fix skills/",
    "prettier --write src/",
    "rm -rf internal/tmp",
    "sudo rm -rf internal/",
    "touch internal/new.go",
    "find . -name '*.go' | xargs rm",
    "cat body | tee cmd/x.go",
    "echo hi > internal/x.go",
    "cat a >> cmd/fleet/main.go",
    "echo a > docs/x.md; echo b > internal/x.go",
    "echo a > docs/x.md && echo b > docs/../main.go",
    "python3 -c 'open(\"x.go\",\"w\").write(\"\")'",
    "node -e 'require(\"fs\").writeFileSync(\"x.js\",\"\")'",
    "env FOO=1 go test ./...",
    "x=$(go test ./... 2>&1)",
]

BASH_ALLOWED = [
    "fleet tasks list --project p",
    "fleet tasks add --project p 'run go test in CI'",
    "fleet tasks note --project p slug --section spec 'Task plan: docs/TASK-PLAN-slug.md'",
    "fleet tasks note --project p slug --section spec 'worker: rm -rf build/ first'",
    'fleet tasks note --project p slug --section spec "then: go test ./... > out.txt"',
    "fleet rm c0ffee02",
    "fleet checkpoint decision 'Stopped rebase of PR #224 — superseded'",
    "fleet checkpoint decision 'worker ran `go test`; passed'",
    "git status",
    "git log --oneline -5",
    "git log --grep=merge --oneline",
    "git log --oneline -- internal/am",
    "git diff main...HEAD --stat",
    "git diff --stat main..add-feature",
    "gh pr view 12 --json state",
    "rg -n 'foo' internal/",
    "rg -n 'go test' .",
    "rg -n 'pytest|npm test' skills/",
    "grep -rn 'git commit' docs/",
    "go build ./... 2>&1 | head",
    "go vet ./...",
    "gofmt -l .",
    "python3 x.py > /dev/null",
    "python3 skills/coordinator/loop.py",
    "pip install -q pytest",
    "mkdir -p docs/plans",
    "echo plan > docs/TASK-PLAN-slug.md",
    "echo a > docs/x.md && echo b > docs/y.md",
    "pandoc docs/DESIGN-x.md -o docs/DESIGN-x.html && open docs/DESIGN-x.html",
]


class TestBash:
    @pytest.mark.parametrize("cmd,denied",
                             [(c, True) for c in BASH_DENIED] + [(c, False) for c in BASH_ALLOWED])
    def test_verdict(self, cmd: str, denied: bool) -> None:
        got = coordguard.classify(_payload("Bash", command=cmd))
        assert (got is not None) == denied, got


# -- on_pre_tool_use gating: role x escape hatch --------------------------------
# (env overrides, expect deny output, expect violation logged)

GATING_CASES = [
    pytest.param({}, True, True, id="coord"),
    pytest.param({"FLEET_ROLE": "worker"}, False, False, id="worker"),
    pytest.param({"FLEET_COORD_GUARD": "off"}, False, True, id="coord-guard-off"),
    pytest.param({"FLEET_ROLE": None}, True, True, id="coord-via-record"),
    pytest.param({"FLEET_ROLE": None, "_record_is_coord": False}, False, False,
                 id="worker-via-record"),
]


class TestGating:
    @pytest.mark.parametrize("env,denies,logs", GATING_CASES)
    def test_verdict(self, env: dict, denies: bool, logs: bool,
                     home: Path, monkeypatch: pytest.MonkeyPatch) -> None:
        _write_record(home, is_coord=env.pop("_record_is_coord", True))
        for k, v in env.items():
            monkeypatch.delenv(k) if v is None else monkeypatch.setenv(k, v)

        out = coordguard.on_pre_tool_use(_payload("Edit", file_path="/r/a.go"), AGENT)

        assert (out is not None) == denies
        assert coordguard.count_violations(AGENT) == (1 if logs else 0)
        if denies:
            hso = out["hookSpecificOutput"]
            assert hso["hookEventName"] == "PreToolUse"
            assert hso["permissionDecision"] == "deny"
            assert "fleet tasks add" in hso["permissionDecisionReason"]
        if logs:
            line = coordguard.violations_path(AGENT).read_text().splitlines()[0]
            assert json.loads(line)["denied"] is denies


# -- Stop nag ------------------------------------------------------------------

class TestStopNag:
    def test_nags_once_per_batch(self) -> None:
        assert coordguard.stop_nag(AGENT) is None
        coordguard.on_pre_tool_use(_payload("Edit", file_path="/r/a.go"), AGENT)
        coordguard.on_pre_tool_use(_payload("Bash", command="go test ./..."), AGENT)
        nag = coordguard.stop_nag(AGENT)
        assert nag is not None and "blocked 2 attempt(s)" in nag
        assert coordguard.stop_nag(AGENT) is None


# -- main.py wiring ------------------------------------------------------------

class TestMainWiring:
    @pytest.mark.parametrize("env,payload,denies", [
        pytest.param({}, _payload("Write", file_path="/r/a.go"), True, id="coord-write"),
        pytest.param({}, _payload("Read", file_path="/r/a.go"), False, id="coord-read"),
        pytest.param({"FLEET_AGENT_ID": None}, _payload("Write", file_path="/r/a.go"), False,
                     id="outside-fleet"),
    ])
    def test_pre_tool_use(self, env: dict, payload: dict, denies: bool,
                          monkeypatch: pytest.MonkeyPatch,
                          capsys: pytest.CaptureFixture[str]) -> None:
        for k, v in env.items():
            monkeypatch.delenv(k) if v is None else monkeypatch.setenv(k, v)
        out = _run_hook(payload, capsys)
        if denies:
            assert json.loads(out)["hookSpecificOutput"]["permissionDecision"] == "deny"
        else:
            assert out == ""

    @pytest.mark.parametrize("role,reminds", [("coord", True), ("worker", False)])
    def test_session_start_role_reminder(self, role: str, reminds: bool, home: Path,
                                         monkeypatch: pytest.MonkeyPatch,
                                         capsys: pytest.CaptureFixture[str]) -> None:
        monkeypatch.setenv("FLEET_ROLE", role)
        rec = _write_record(home, needs_input=False)
        out = _run_hook({"hook_event_name": "SessionStart"}, capsys)
        assert ("coordinator" in out) == reminds
        # the reminder is context, not a prompt: coord still settles to idle
        assert json.loads(rec.read_text())["needs_input"] is True
