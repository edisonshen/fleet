"""Tests for skills/fleet-guard/coordguard.py — the coordinator delegation
guard (PreToolUse deny + Stop nag)."""
from __future__ import annotations

import io
import json
from pathlib import Path

import pytest

import coordguard
import main as fleet_main


@pytest.fixture(autouse=True)
def coord_env(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> Path:
    home = tmp_path / "fleet"
    monkeypatch.setenv("FLEET_HOME", str(home))
    monkeypatch.setenv("FLEET_AGENT_ID", "c0ffee01")
    monkeypatch.setenv("FLEET_ROLE", "coord")
    monkeypatch.delenv("FLEET_COORD_GUARD", raising=False)
    return home


def _payload(tool: str, **tool_input: object) -> dict:
    return {
        "hook_event_name": "PreToolUse",
        "tool_name": tool,
        "tool_input": tool_input,
    }


# -- classify: edit tools ------------------------------------------------------

class TestEditTools:
    @pytest.mark.parametrize("tool", sorted(coordguard.EDIT_TOOLS))
    def test_source_edit_denied(self, tool: str) -> None:
        got = coordguard.classify(_payload(tool, file_path="/repo/internal/spawn/spawn.go"))
        assert got is not None and "spawn.go" in got

    def test_docs_edit_allowed(self) -> None:
        assert coordguard.classify(_payload("Write", file_path="/repo/docs/DESIGN-x.md")) is None
        assert coordguard.classify(_payload("Edit", file_path="docs/TASK-PLAN-y.md")) is None

    def test_fleet_home_write_allowed(self, coord_env: Path) -> None:
        p = coord_env / "inbox" / "abc.md"
        assert coordguard.classify(_payload("Write", file_path=str(p))) is None

    @pytest.mark.parametrize("path", [
        "/repo/docs/../main.go",
        "/repo/docs/./../internal/x.go",
        "docs/../cmd/fleet/main.go",
        "/repo/docs",  # the dir itself is not a doc file
        "/repo/docs.go",
        "/repo/internal/docs_test.go",
    ])
    def test_docs_escape_denied(self, path: str) -> None:
        assert coordguard.classify(_payload("Write", file_path=path)) is not None

    def test_docs_symlink_escape_denied(self, tmp_path: Path) -> None:
        repo = tmp_path / "repo"
        (repo / "docs").mkdir(parents=True)
        (repo / "internal").mkdir()
        (repo / "docs" / "src").symlink_to(repo / "internal")
        target = repo / "docs" / "src" / "x.go"
        assert coordguard.classify(_payload("Write", file_path=str(target))) is not None

    def test_repo_under_tmp_still_guarded(self, tmp_path: Path) -> None:
        assert coordguard.classify(
            _payload("Write", file_path=str(tmp_path / "repo" / "main.go"))) is not None

    def test_subagent_exempt(self) -> None:
        payload = _payload("Edit", file_path="/repo/main.go")
        payload["agent_id"] = "sub-123"
        assert coordguard.classify(payload) is None

    def test_read_only_tools_allowed(self) -> None:
        assert coordguard.classify(_payload("Read", file_path="/repo/main.go")) is None
        assert coordguard.classify(_payload("Grep", pattern="x")) is None


# -- classify: bash ------------------------------------------------------------

class TestBash:
    @pytest.mark.parametrize("cmd", [
        "go test ./...",
        "cd repo && go test -race ./internal/...",
        "python3 -m pytest skills/ -q",
        "pytest -q",
        "npm test",
        "git add -A && git commit -m 'x'",
        "git push origin HEAD",
        "git rebase main",
        "sed -i 's/a/b/' internal/x.go",
        "rm -rf internal/tmp",
        "echo hi > internal/x.go",
        "cat a >> cmd/fleet/main.go",
        "cat body | tee cmd/x.go",
        "gofmt -w internal/x.go",
        "gofmt -l -w .",
        "goimports -w .",
        "black skills/",
        "ruff check --fix skills/",
        "prettier --write src/",
        "touch internal/new.go",
        "git checkout -- internal/x.go",
        "git checkout -b feature",
        "git switch main",
        "git clean -fd",
        "git -C /repo commit -m x",
        "python3 -c 'open(\"x.go\",\"w\").write(\"\")'",
        "node -e 'require(\"fs\").writeFileSync(\"x.js\",\"\")'",
        "echo a > docs/x.md; echo b > internal/x.go",
        "echo a > docs/x.md && echo b > docs/../main.go",
        "sudo rm -rf internal/",
        "env FOO=1 go test ./...",
        "x=$(go test ./... 2>&1)",
        "find . -name '*.go' | xargs rm",
    ])
    def test_denied(self, cmd: str) -> None:
        assert coordguard.classify(_payload("Bash", command=cmd)) is not None

    @pytest.mark.parametrize("cmd", [
        "fleet tasks list --project p",
        "fleet tasks note --project p slug --section spec 'Task plan: docs/TASK-PLAN-slug.md'",
        "fleet rm c0ffee02",
        "fleet checkpoint decision 'Stopped rebase of PR #224 — superseded'",
        "git status",
        "git log --oneline -5",
        "git diff main...HEAD --stat",
        "gh pr view 12 --json state",
        "rg -n 'foo' internal/",
        "go build ./... 2>&1 | head",
        "python3 x.py > /dev/null",
        "pandoc docs/DESIGN-x.md -o docs/DESIGN-x.html && open docs/DESIGN-x.html",
        "echo plan > docs/TASK-PLAN-slug.md",
        "echo a > docs/x.md && echo b > docs/y.md",
        "rg -n 'go test' .",
        "rg -n 'pytest|npm test' skills/",
        "grep -rn 'git commit' docs/",
        'fleet tasks note --project p slug --section spec "then: go test ./... > out.txt"',
        "fleet checkpoint decision 'worker ran `go test`; passed'",
        "fleet tasks add --project p 'run go test in CI'",
        "fleet tasks note --project p slug --section spec 'worker: rm -rf build/ first'",
        "git log --grep=merge --oneline",
        "git log --oneline -- internal/am",
        "git diff --stat main..add-feature",
        "gofmt -l .",
        "go vet ./...",
        "mkdir -p docs/plans",
        "pip install -q pytest",
        "python3 skills/coordinator/loop.py",
    ])
    def test_allowed(self, cmd: str) -> None:
        assert coordguard.classify(_payload("Bash", command=cmd)) is None


# -- gating --------------------------------------------------------------------

class TestGating:
    def test_worker_role_untouched(self, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setenv("FLEET_ROLE", "worker")
        assert coordguard.on_pre_tool_use(_payload("Edit", file_path="/r/a.go"), "c0ffee01") is None

    def test_role_falls_back_to_record(self, coord_env: Path,
                                       monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.delenv("FLEET_ROLE")
        rec = coord_env / "agents" / "c0ffee01.json"
        rec.parent.mkdir(parents=True)
        rec.write_text(json.dumps({"id": "c0ffee01", "is_coord": True}))
        assert coordguard.is_coord_session() is True

    def test_deny_output_shape_and_log(self, coord_env: Path) -> None:
        out = coordguard.on_pre_tool_use(_payload("Edit", file_path="/r/a.go"), "c0ffee01")
        assert out is not None
        hso = out["hookSpecificOutput"]
        assert hso["hookEventName"] == "PreToolUse"
        assert hso["permissionDecision"] == "deny"
        assert "fleet tasks add" in hso["permissionDecisionReason"]
        lines = coordguard.violations_path("c0ffee01").read_text().splitlines()
        assert len(lines) == 1
        assert json.loads(lines[0])["denied"] is True

    def test_guard_off_logs_but_allows(self, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setenv("FLEET_COORD_GUARD", "off")
        out = coordguard.on_pre_tool_use(_payload("Edit", file_path="/r/a.go"), "c0ffee01")
        assert out is None
        assert coordguard.count_violations("c0ffee01") == 1


# -- Stop nag ------------------------------------------------------------------

class TestStopNag:
    def test_nags_once_per_batch(self) -> None:
        assert coordguard.stop_nag("c0ffee01") is None
        coordguard.on_pre_tool_use(_payload("Edit", file_path="/r/a.go"), "c0ffee01")
        coordguard.on_pre_tool_use(_payload("Bash", command="go test ./..."), "c0ffee01")
        nag = coordguard.stop_nag("c0ffee01")
        assert nag is not None and "blocked 2 attempt(s)" in nag
        assert coordguard.stop_nag("c0ffee01") is None


# -- main.py wiring ------------------------------------------------------------

class TestMainWiring:
    def test_pre_tool_use_prints_deny(self, capsys: pytest.CaptureFixture[str]) -> None:
        rc = fleet_main.main(io.StringIO(json.dumps(_payload("Write", file_path="/r/a.go"))))
        assert rc == 0
        out = json.loads(capsys.readouterr().out)
        assert out["hookSpecificOutput"]["permissionDecision"] == "deny"

    def test_pre_tool_use_silent_when_allowed(self, capsys: pytest.CaptureFixture[str]) -> None:
        rc = fleet_main.main(io.StringIO(json.dumps(_payload("Read", file_path="/r/a.go"))))
        assert rc == 0
        assert capsys.readouterr().out == ""

    def test_session_start_emits_role_reminder_and_stays_idle(
            self, coord_env: Path, capsys: pytest.CaptureFixture[str]) -> None:
        rec = coord_env / "agents" / "c0ffee01.json"
        rec.parent.mkdir(parents=True)
        rec.write_text(json.dumps({"id": "c0ffee01", "needs_input": False}))
        fleet_main.main(io.StringIO(json.dumps({"hook_event_name": "SessionStart"})))
        assert "coordinator" in capsys.readouterr().out
        assert json.loads(rec.read_text())["needs_input"] is True

    def test_session_start_no_reminder_for_worker(
            self, monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]) -> None:
        monkeypatch.setenv("FLEET_ROLE", "worker")
        fleet_main.main(io.StringIO(json.dumps({"hook_event_name": "SessionStart"})))
        assert capsys.readouterr().out == ""

    def test_pre_tool_use_silent_outside_fleet(self, monkeypatch: pytest.MonkeyPatch,
                                               capsys: pytest.CaptureFixture[str]) -> None:
        monkeypatch.delenv("FLEET_AGENT_ID")
        fleet_main.main(io.StringIO(json.dumps(_payload("Write", file_path="/r/a.go"))))
        assert capsys.readouterr().out == ""
