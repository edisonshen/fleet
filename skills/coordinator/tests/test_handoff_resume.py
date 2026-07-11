"""Tests for skills/coordinator/handoff_resume.py — issue #93 Phase B2.

Covers:
- parse_handoff_doc: round-trips Active Subagents section back to typed
  list, handles missing section / placeholder body / malformed lines.
- build_resume_dispatches: WIP-existence gating, inbox rewrite, DISPATCH
  block emission. Skipped entries are surfaced as human-readable
  reasons.
- CLI main(): end-to-end with seed handoff doc + WIP files + inbox file.
"""
from __future__ import annotations

import json
import shutil
import subprocess
import sys
import time
from pathlib import Path

import pytest

import dispatch as dispatch_mod
import handoff_resume


@pytest.fixture(scope="session")
def built_fleet_bin(tmp_path_factory) -> str:
    """Build the real `fleet` binary from THIS worktree so the #184
    reset-for-relaunch / acquire flock path is exercised against the
    current code (not whatever older `fleet` is on PATH). Skips if Go is
    unavailable."""
    if shutil.which("go") is None:
        pytest.skip("go toolchain not available")
    repo_root = Path(__file__).resolve().parents[3]
    out_dir = tmp_path_factory.mktemp("fleet-bin")
    binary = out_dir / "fleet"
    proc = subprocess.run(
        ["go", "build", "-o", str(binary), "./cmd/fleet"],
        cwd=str(repo_root), capture_output=True, text=True,
    )
    if proc.returncode != 0:
        pytest.skip(f"fleet build failed: {proc.stderr}")
    return str(binary)


def test_resume_absent_journal_acquires_gen0_passes_gate(
    built_fleet_bin: str, tmp_path: Path, monkeypatch: pytest.MonkeyPatch,
) -> None:
    """Codex iter-1 [P2]: a resume for an id with NO dispatch journal
    (pre-#184 / reaped) must ACQUIRE a fresh journal (gen 0) so the gen-0
    resume block passes the coord's mark-launch-attempted gate. A bare
    write_worker_inbox would leave the journal absent and the gate would
    predicate-fail, silently dropping the resume."""
    fleet_home = tmp_path / "fleet-home"
    wip_dir = tmp_path / "wip"
    (fleet_home / "inbox").mkdir(parents=True)
    (fleet_home / "dispatches").mkdir(parents=True)
    wip_dir.mkdir()
    inbox = fleet_home / "inbox" / "abcd1234.md"
    inbox.write_text("You are a Fleet worker for task: fix-foo\n", encoding="utf-8")
    (wip_dir / "fix-foo.md").write_text("## Phase 1\n- landed\n", encoding="utf-8")

    # Point dispatch_mod's default `fleet` at the freshly-built binary by
    # threading it through the env the helper copies (FLEET_HOME) and the
    # PATH so the bare-string "fleet" resolves to our build.
    bin_dir = str(Path(built_fleet_bin).parent)
    monkeypatch.setenv("PATH", bin_dir + ":" + __import__("os").environ["PATH"])
    monkeypatch.setenv("FLEET_PROJECT", "myproj")

    entries = [
        handoff_resume.ResumeEntry(
            task_id="fix-foo", branch="worker/fix-foo", phase="tdd-green",
            agent_id="abcd1234", subagent_id="",
        )
    ]
    blocks, skipped = handoff_resume.build_resume_dispatches(
        entries, wip_dir=wip_dir, fleet_home=fleet_home, project="myproj",
    )
    assert skipped == [], skipped
    assert len(blocks) == 1
    assert "generation: 0" in blocks[0]

    # The journal must now EXIST at gen 0, ExecPending — so a gen-0
    # mark-launch-attempted (the coord's step 2) returns ok, not the
    # predicate_fail that an absent journal would yield.
    journal_path = fleet_home / "dispatches" / "abcd1234.json"
    assert journal_path.exists(), "absent resume did not acquire a journal"
    j = json.loads(journal_path.read_text(encoding="utf-8"))
    assert j["exec_state"] == "pending"
    assert j["generation"] == 0
    out = dispatch_mod.mark_launch_attempted(
        "abcd1234", 0, fleet_bin=built_fleet_bin, fleet_home=str(fleet_home),
    )
    assert out == "ok", f"gen-0 launch gate must accept the acquired journal, got {out}"


def _seed_handoff(
    doc_path: Path,
    *,
    body_subagents: str,
    body_open_prs: str = "_(no open PRs)_",
    project: str = "myproj",
) -> None:
    """Write a minimal handoff doc with the Active Subagents section
    filled with the given body_subagents string and the Open PRs
    section filled with body_open_prs (defaults to placeholder)."""
    doc = (
        "---\n"
        'agent_id: "abcd1234"\n'
        'task_id: "coord-myproj"\n'
        f'project: "{project}"\n'
        "context_pct_at_handoff: 50\n"
        "previous_handoff: null\n"
        "handoff_number: 1\n"
        'timestamp: "2026-04-28T12:34:56Z"\n'
        'handoff_type: "auto-yellow"\n'
        "---\n\n"
        "## First Action (auto)\nfoo\n\n"
        "## Completed\nx\n\n"
        "## Key Decisions\nx\n\n"
        "## Docs (this session)\nx\n\n"
        "## Open Questions\nx\n\n"
        "## Next Steps (prioritized)\nx\n\n"
        f"## Active Subagents\n{body_subagents}\n\n"
        f"## Open PRs\n{body_open_prs}\n"
    )
    doc_path.write_text(doc, encoding="utf-8")


def test_parse_handoff_doc_empty_section(tmp_path: Path) -> None:
    p = tmp_path / "handoff.md"
    _seed_handoff(p, body_subagents="_(none)_")
    got = handoff_resume.parse_handoff_doc(p)
    assert got == []


def test_parse_handoff_doc_single_entry(tmp_path: Path) -> None:
    p = tmp_path / "handoff.md"
    _seed_handoff(
        p,
        body_subagents=(
            '- task="fix-foo" branch="worker/fix-foo" phase="tdd-green" '
            'agent_id="abcd1234" subagent_id="claude-sub-1"'
        ),
    )
    got = handoff_resume.parse_handoff_doc(p)
    assert len(got) == 1
    e = got[0]
    assert e.task_id == "fix-foo"
    assert e.branch == "worker/fix-foo"
    assert e.phase == "tdd-green"
    assert e.agent_id == "abcd1234"
    assert e.subagent_id == "claude-sub-1"


def test_parse_handoff_doc_multi_entry(tmp_path: Path) -> None:
    p = tmp_path / "handoff.md"
    body = (
        '- task="a" branch="worker/a" phase="push" agent_id="11111111" subagent_id=""\n'
        '- task="b" branch="worker/b" phase="tdd-red" agent_id="22222222" subagent_id="claude-sub-2"'
    )
    _seed_handoff(p, body_subagents=body)
    got = handoff_resume.parse_handoff_doc(p)
    assert [e.task_id for e in got] == ["a", "b"]
    assert got[1].subagent_id == "claude-sub-2"


def test_parse_handoff_doc_malformed_line_skipped(tmp_path: Path) -> None:
    p = tmp_path / "handoff.md"
    body = (
        '- task="ok" branch="worker/ok" phase="" agent_id="11111111" subagent_id=""\n'
        "this line is junk\n"
        '- task="also-ok" branch="" phase="" agent_id="22222222" subagent_id=""'
    )
    _seed_handoff(p, body_subagents=body)
    got = handoff_resume.parse_handoff_doc(p)
    assert len(got) == 2
    assert {e.task_id for e in got} == {"ok", "also-ok"}


def test_parse_handoff_doc_quoted_specials_round_trip(tmp_path: Path) -> None:
    """A path with spaces, a phase with a colon, and an embedded
    backslash-escape must round-trip via the Go-quote escape rules."""
    p = tmp_path / "handoff.md"
    body = (
        '- task="weird slug" branch="worker/weird slug" '
        'phase="phase: with colon" agent_id="abcd1234" '
        'subagent_id="with\\"quote"'
    )
    _seed_handoff(p, body_subagents=body)
    got = handoff_resume.parse_handoff_doc(p)
    assert len(got) == 1
    assert got[0].task_id == "weird slug"
    assert got[0].phase == "phase: with colon"
    assert got[0].subagent_id == 'with"quote'


def test_parse_handoff_doc_missing_section(tmp_path: Path) -> None:
    p = tmp_path / "handoff.md"
    p.write_text(
        "---\nagent_id: \"x\"\n---\n\n## First Action (auto)\nfoo\n",
        encoding="utf-8",
    )
    got = handoff_resume.parse_handoff_doc(p)
    assert got == []


def test_build_resume_dispatches_skips_when_wip_missing(tmp_path: Path) -> None:
    fleet_home = tmp_path / "fleet-home"
    wip_dir = tmp_path / "wip"
    fleet_home.mkdir()
    wip_dir.mkdir()
    inbox = fleet_home / "inbox" / "abcd1234.md"
    inbox.parent.mkdir()
    inbox.write_text("original prompt body", encoding="utf-8")
    entries = [
        handoff_resume.ResumeEntry(
            task_id="fix-foo", branch="worker/fix-foo", phase="tdd-green",
            agent_id="abcd1234", subagent_id="",
        )
    ]
    blocks, skipped = handoff_resume.build_resume_dispatches(
        entries, wip_dir=wip_dir, fleet_home=fleet_home,
    )
    assert blocks == []
    assert len(skipped) == 1
    assert "WIP" in skipped[0]


def test_build_resume_dispatches_skips_when_inbox_missing(tmp_path: Path) -> None:
    fleet_home = tmp_path / "fleet-home"
    wip_dir = tmp_path / "wip"
    fleet_home.mkdir()
    wip_dir.mkdir()
    (wip_dir / "fix-foo.md").write_text("phases_completed: [...]\n", encoding="utf-8")
    entries = [
        handoff_resume.ResumeEntry(
            task_id="fix-foo", branch="worker/fix-foo", phase="tdd-green",
            agent_id="abcd1234", subagent_id="",
        )
    ]
    blocks, skipped = handoff_resume.build_resume_dispatches(
        entries, wip_dir=wip_dir, fleet_home=fleet_home,
    )
    assert blocks == []
    assert any("inbox" in s for s in skipped)


def test_build_resume_dispatches_emits_block_when_wip_and_inbox_present(
    tmp_path: Path,
) -> None:
    fleet_home = tmp_path / "fleet-home"
    wip_dir = tmp_path / "wip"
    fleet_home.mkdir()
    wip_dir.mkdir()
    (fleet_home / "inbox").mkdir()
    inbox = fleet_home / "inbox" / "abcd1234.md"
    inbox.write_text(
        "You are a Fleet worker for task: fix-foo\nProject: myproj\n",
        encoding="utf-8",
    )
    (wip_dir / "fix-foo.md").write_text(
        "## Phase 1 — 2026-05-09T...\n- what landed: ...\n", encoding="utf-8",
    )
    entries = [
        handoff_resume.ResumeEntry(
            task_id="fix-foo", branch="worker/fix-foo", phase="tdd-green",
            agent_id="abcd1234", subagent_id="",
        )
    ]
    blocks, skipped = handoff_resume.build_resume_dispatches(
        entries, wip_dir=wip_dir, fleet_home=fleet_home,
    )
    assert len(blocks) == 1
    assert skipped == []
    block = blocks[0]
    assert block.startswith("DISPATCH: fix-foo")
    assert "agent_id: abcd1234" in block
    assert "run_in_background: true" in block
    assert "subagent_type: general-purpose" in block
    assert "(resume)" in block
    rewritten = inbox.read_text(encoding="utf-8")
    assert "RESUMING after coord handoff" in rewritten
    assert str(wip_dir / "fix-foo.md") in rewritten
    assert "You are a Fleet worker for task: fix-foo" in rewritten


def test_build_resume_dispatches_skips_entry_with_empty_agent_id(
    tmp_path: Path,
) -> None:
    fleet_home = tmp_path / "fleet-home"
    wip_dir = tmp_path / "wip"
    fleet_home.mkdir()
    wip_dir.mkdir()
    entries = [
        handoff_resume.ResumeEntry(
            task_id="fix-foo", branch="", phase="", agent_id="", subagent_id="",
        )
    ]
    blocks, skipped = handoff_resume.build_resume_dispatches(
        entries, wip_dir=wip_dir, fleet_home=fleet_home,
    )
    assert blocks == []
    assert any("agent_id" in s for s in skipped)


def test_build_resume_dispatches_handles_multiple_entries(tmp_path: Path) -> None:
    """One worker resumable, one not — block emission is per-entry,
    not all-or-nothing."""
    fleet_home = tmp_path / "fleet-home"
    wip_dir = tmp_path / "wip"
    fleet_home.mkdir()
    wip_dir.mkdir()
    (fleet_home / "inbox").mkdir()
    (fleet_home / "inbox" / "11111111.md").write_text("A prompt", encoding="utf-8")
    (wip_dir / "task-a.md").write_text("phase 1", encoding="utf-8")
    (fleet_home / "inbox" / "22222222.md").write_text("B prompt", encoding="utf-8")
    entries = [
        handoff_resume.ResumeEntry(
            task_id="task-a", branch="worker/task-a", phase="push",
            agent_id="11111111", subagent_id="",
        ),
        handoff_resume.ResumeEntry(
            task_id="task-b", branch="worker/task-b", phase="tdd-red",
            agent_id="22222222", subagent_id="",
        ),
    ]
    blocks, skipped = handoff_resume.build_resume_dispatches(
        entries, wip_dir=wip_dir, fleet_home=fleet_home,
    )
    assert len(blocks) == 1
    assert "DISPATCH: task-a" in blocks[0]
    assert len(skipped) == 1
    assert "task-b" in skipped[0]


def test_main_no_args_returns_usage_error(capsys: pytest.CaptureFixture) -> None:
    rc = handoff_resume.main([])
    assert rc == 2
    err = capsys.readouterr().err
    assert "usage" in err


def test_main_missing_doc_returns_one(
    capsys: pytest.CaptureFixture, tmp_path: Path,
) -> None:
    rc = handoff_resume.main([str(tmp_path / "missing.md")])
    assert rc == 1
    assert "not found" in capsys.readouterr().err


def test_main_emits_dispatch_blocks_on_stdout(
    capsys: pytest.CaptureFixture, tmp_path: Path, monkeypatch: pytest.MonkeyPatch,
) -> None:
    fleet_home = tmp_path / "fleet-home"
    wip_dir = tmp_path / "wip"
    fleet_home.mkdir()
    wip_dir.mkdir()
    (fleet_home / "inbox").mkdir()
    (fleet_home / "inbox" / "abcd1234.md").write_text("orig prompt", encoding="utf-8")
    (wip_dir / "fix-foo.md").write_text("phase 1", encoding="utf-8")
    doc = tmp_path / "handoff.md"
    _seed_handoff(
        doc,
        body_subagents=(
            '- task="fix-foo" branch="worker/fix-foo" phase="tdd-green" '
            'agent_id="abcd1234" subagent_id=""'
        ),
    )
    monkeypatch.setenv("FLEET_HOME", str(fleet_home))
    monkeypatch.setenv("FLEET_SUBAGENT_WIP_DIR", str(wip_dir))
    rc = handoff_resume.main([str(doc)])
    assert rc == 0
    out = capsys.readouterr().out
    assert "DISPATCH: fix-foo" in out
    assert "agent_id: abcd1234" in out


def test_main_records_resumed_task_into_session_tasks(
    capsys: pytest.CaptureFixture, tmp_path: Path, monkeypatch: pytest.MonkeyPatch,
) -> None:
    """Review iter-2 (codex P1): a resumed task must be stamped into THIS
    (successor) coord's own coord-state.json:session_tasks under its own
    coord_id — otherwise the resumed task silently disappears from the
    session-scoped Next Steps/Open Questions if this coord hands off
    again before any loop.py tick seam independently re-touches it.
    Without the fix, coord-state.json never gets a session_tasks entry
    for a resume-only task."""
    fleet_home = tmp_path / "fleet-home"
    wip_dir = tmp_path / "wip"
    fleet_home.mkdir()
    wip_dir.mkdir()
    (fleet_home / "inbox").mkdir()
    (fleet_home / "inbox" / "abcd1234.md").write_text("orig prompt", encoding="utf-8")
    (wip_dir / "fix-foo.md").write_text("phase 1", encoding="utf-8")
    doc = tmp_path / "handoff.md"
    _seed_handoff(
        doc,
        body_subagents=(
            '- task="fix-foo" branch="worker/fix-foo" phase="tdd-green" '
            'agent_id="abcd1234" subagent_id=""'
        ),
    )
    monkeypatch.setenv("FLEET_HOME", str(fleet_home))
    monkeypatch.setenv("FLEET_SUBAGENT_WIP_DIR", str(wip_dir))
    monkeypatch.setenv("FLEET_PROJECT", "myproj")
    monkeypatch.setenv("FLEET_AGENT_ID", "succ0001")

    rc = handoff_resume.main([str(doc)])
    assert rc == 0

    state_path = fleet_home / "projects" / "myproj" / "coord-state.json"
    cs = json.loads(state_path.read_text(encoding="utf-8"))
    st = cs.get("session_tasks", [])
    assert [e["slug"] for e in st] == ["fix-foo"]
    assert st[0]["coord_id"] == "succ0001"
    assert cs["resumed_handoff_path"] == str(doc)


def test_main_writes_resumed_handoff_marker_on_exit_zero(
    capsys: pytest.CaptureFixture, tmp_path: Path, monkeypatch: pytest.MonkeyPatch,
) -> None:
    fleet_home = tmp_path / "fleet-home"
    wip_dir = tmp_path / "wip"
    fleet_home.mkdir()
    wip_dir.mkdir()
    doc = tmp_path / "handoff.md"
    _seed_handoff(doc, body_subagents="_(none)_")
    monkeypatch.setenv("FLEET_HOME", str(fleet_home))
    monkeypatch.setenv("FLEET_SUBAGENT_WIP_DIR", str(wip_dir))
    monkeypatch.setenv("FLEET_PROJECT", "myproj")

    rc = handoff_resume.main([str(doc)])
    assert rc == 0
    capsys.readouterr()

    state_path = fleet_home / "projects" / "myproj" / "coord-state.json"
    cs = json.loads(state_path.read_text(encoding="utf-8"))
    assert cs["resumed_handoff_path"] == str(doc)


def test_main_stdout_failure_does_not_ack_resume(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch,
) -> None:
    class BrokenStdout:
        def write(self, _s: str) -> int:
            raise BrokenPipeError("stdout closed")

        def flush(self) -> None:
            return None

    fleet_home = tmp_path / "fleet-home"
    wip_dir = tmp_path / "wip"
    fleet_home.mkdir()
    wip_dir.mkdir()
    (fleet_home / "inbox").mkdir()
    (fleet_home / "inbox" / "abcd1234.md").write_text("orig prompt", encoding="utf-8")
    (wip_dir / "fix-foo.md").write_text("phase 1", encoding="utf-8")
    doc = tmp_path / "handoff.md"
    _seed_handoff(
        doc,
        body_subagents=(
            '- task="fix-foo" branch="worker/fix-foo" phase="tdd-green" '
            'agent_id="abcd1234" subagent_id=""'
        ),
    )
    marker_called = False

    def fake_marker(*_args, **_kwargs) -> None:
        nonlocal marker_called
        marker_called = True

    monkeypatch.setenv("FLEET_HOME", str(fleet_home))
    monkeypatch.setenv("FLEET_SUBAGENT_WIP_DIR", str(wip_dir))
    monkeypatch.setenv("FLEET_PROJECT", "myproj")
    monkeypatch.setattr(handoff_resume, "record_resumed_handoff_marker", fake_marker)
    monkeypatch.setattr("sys.stdout", BrokenStdout())

    with pytest.raises(BrokenPipeError):
        handoff_resume.main([str(doc)])

    assert not marker_called


def test_main_transient_resume_skip_does_not_ack(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch,
) -> None:
    fleet_home = tmp_path / "fleet-home"
    wip_dir = tmp_path / "wip"
    fleet_home.mkdir()
    wip_dir.mkdir()
    (fleet_home / "inbox").mkdir()
    (fleet_home / "inbox" / "abcd1234.md").write_text("orig prompt", encoding="utf-8")
    (wip_dir / "fix-foo.md").write_text("phase 1", encoding="utf-8")
    doc = tmp_path / "handoff.md"
    _seed_handoff(
        doc,
        body_subagents=(
            '- task="fix-foo" branch="worker/fix-foo" phase="tdd-green" '
            'agent_id="abcd1234" subagent_id=""'
        ),
    )
    monkeypatch.setenv("FLEET_HOME", str(fleet_home))
    monkeypatch.setenv("FLEET_SUBAGENT_WIP_DIR", str(wip_dir))
    monkeypatch.setenv("FLEET_PROJECT", "myproj")
    monkeypatch.setattr(
        dispatch_mod,
        "reset_for_relaunch",
        lambda *_args, **_kwargs: {"outcome": "contention"},
    )

    rc = handoff_resume.main([str(doc)])

    assert rc == 1
    assert not (fleet_home / "projects" / "myproj" / "coord-state.json").exists()


def test_main_transient_skip_suppresses_prepared_blocks(
    capsys: pytest.CaptureFixture, tmp_path: Path, monkeypatch: pytest.MonkeyPatch,
) -> None:
    fleet_home = tmp_path / "fleet-home"
    wip_dir = tmp_path / "wip"
    fleet_home.mkdir()
    wip_dir.mkdir()
    (fleet_home / "inbox").mkdir()
    (fleet_home / "inbox" / "11111111.md").write_text("prompt A", encoding="utf-8")
    (fleet_home / "inbox" / "22222222.md").write_text("prompt B", encoding="utf-8")
    (wip_dir / "task-a.md").write_text("phase A", encoding="utf-8")
    (wip_dir / "task-b.md").write_text("phase B", encoding="utf-8")
    doc = tmp_path / "handoff.md"
    body = (
        '- task="task-a" branch="worker/task-a" phase="tdd-green" '
        'agent_id="11111111" subagent_id=""\n'
        '- task="task-b" branch="worker/task-b" phase="tdd-green" '
        'agent_id="22222222" subagent_id=""'
    )
    _seed_handoff(doc, body_subagents=body)

    def fake_reset(agent_id: str, *_args, **_kwargs) -> dict:
        if agent_id == "11111111":
            return {"outcome": "reset", "generation": 7}
        return {"outcome": "contention"}

    monkeypatch.setenv("FLEET_HOME", str(fleet_home))
    monkeypatch.setenv("FLEET_SUBAGENT_WIP_DIR", str(wip_dir))
    monkeypatch.setenv("FLEET_PROJECT", "myproj")
    monkeypatch.setattr(dispatch_mod, "reset_for_relaunch", fake_reset)

    rc = handoff_resume.main([str(doc)])

    captured = capsys.readouterr()
    assert rc == 1
    assert "DISPATCH: task-a" not in captured.out
    assert "DISPATCH: task-b" not in captured.out
    assert "transient resume skip" in captured.err
    state_path = fleet_home / "projects" / "myproj" / "coord-state.json"
    cs = json.loads(state_path.read_text(encoding="utf-8"))
    assert "resumed_handoff_path" not in cs


def test_main_failure_does_not_write_resumed_handoff_marker(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch,
) -> None:
    fleet_home = tmp_path / "fleet-home"
    fleet_home.mkdir()
    monkeypatch.setenv("FLEET_HOME", str(fleet_home))
    monkeypatch.setenv("FLEET_PROJECT", "myproj")

    rc = handoff_resume.main([str(tmp_path / "missing.md")])

    assert rc == 1
    assert not (fleet_home / "projects" / "myproj" / "coord-state.json").exists()


def test_main_uses_frontmatter_project_for_marker_when_env_unset(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch,
) -> None:
    fleet_home = tmp_path / "fleet-home"
    wip_dir = tmp_path / "wip"
    fleet_home.mkdir()
    wip_dir.mkdir()
    doc = tmp_path / "handoff.md"
    _seed_handoff(doc, body_subagents="_(none)_", project="fallback-proj")
    monkeypatch.setenv("FLEET_HOME", str(fleet_home))
    monkeypatch.setenv("FLEET_SUBAGENT_WIP_DIR", str(wip_dir))
    monkeypatch.delenv("FLEET_PROJECT", raising=False)

    rc = handoff_resume.main([str(doc)])

    assert rc == 0
    state_path = fleet_home / "projects" / "fallback-proj" / "coord-state.json"
    cs = json.loads(state_path.read_text(encoding="utf-8"))
    assert cs["resumed_handoff_path"] == str(doc)


def test_main_rejects_traversal_project_before_resume_side_effects(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch,
) -> None:
    fleet_home = tmp_path / "fleet-home"
    wip_dir = tmp_path / "wip"
    outside = tmp_path / "outside"
    fleet_home.mkdir()
    wip_dir.mkdir()
    (fleet_home / "inbox").mkdir()
    inbox = fleet_home / "inbox" / "abcd1234.md"
    inbox.write_text("orig prompt", encoding="utf-8")
    (wip_dir / "fix-foo.md").write_text("phase 1", encoding="utf-8")
    doc = tmp_path / "handoff.md"
    _seed_handoff(
        doc,
        body_subagents=(
            '- task="fix-foo" branch="worker/fix-foo" phase="tdd-green" '
            'agent_id="abcd1234" subagent_id=""'
        ),
        project="../../outside",
    )
    monkeypatch.setenv("FLEET_HOME", str(fleet_home))
    monkeypatch.setenv("FLEET_SUBAGENT_WIP_DIR", str(wip_dir))
    monkeypatch.delenv("FLEET_PROJECT", raising=False)

    rc = handoff_resume.main([str(doc)])

    assert rc == 1
    assert inbox.read_text(encoding="utf-8") == "orig prompt"
    assert not outside.exists()


def test_main_rejects_absolute_project_before_resume_side_effects(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch,
) -> None:
    fleet_home = tmp_path / "fleet-home"
    wip_dir = tmp_path / "wip"
    outside = tmp_path / "outside"
    fleet_home.mkdir()
    wip_dir.mkdir()
    (fleet_home / "inbox").mkdir()
    inbox = fleet_home / "inbox" / "abcd1234.md"
    inbox.write_text("orig prompt", encoding="utf-8")
    (wip_dir / "fix-foo.md").write_text("phase 1", encoding="utf-8")
    doc = tmp_path / "handoff.md"
    _seed_handoff(
        doc,
        body_subagents=(
            '- task="fix-foo" branch="worker/fix-foo" phase="tdd-green" '
            'agent_id="abcd1234" subagent_id=""'
        ),
        project=str(outside),
    )
    monkeypatch.setenv("FLEET_HOME", str(fleet_home))
    monkeypatch.setenv("FLEET_SUBAGENT_WIP_DIR", str(wip_dir))
    monkeypatch.delenv("FLEET_PROJECT", raising=False)

    rc = handoff_resume.main([str(doc)])

    assert rc == 1
    assert inbox.read_text(encoding="utf-8") == "orig prompt"
    assert not outside.exists()


def test_record_resumed_session_tasks_does_not_write_handoff_marker(
    tmp_path: Path,
) -> None:
    fleet_home = tmp_path / "fleet-home"
    project_dir = fleet_home / "projects" / "myproj"
    (project_dir / ".locks").mkdir(parents=True)
    state_path = project_dir / "coord-state.json"
    state_path.write_text(
        json.dumps({"resumed_handoff_path": "/tmp/old.md"}),
        encoding="utf-8",
    )

    handoff_resume.record_resumed_session_tasks(
        ["fix-foo"], project="myproj", coord_id="succ0001", home=fleet_home,
    )

    cs = json.loads(state_path.read_text(encoding="utf-8"))
    assert cs["resumed_handoff_path"] == "/tmp/old.md"


@pytest.mark.parametrize(
    "project",
    ["../../outside", "/tmp/outside", "BadProj", ".locks", "-flag"],
)
def test_record_resumed_handoff_marker_rejects_unsafe_project(
    tmp_path: Path, project: str,
) -> None:
    fleet_home = tmp_path / "fleet-home"
    outside = tmp_path / "outside"

    with pytest.raises(ValueError):
        handoff_resume.record_resumed_handoff_marker(
            "/tmp/handoff.md", project=project, home=fleet_home,
        )

    assert not outside.exists()


def test_record_resumed_handoff_marker_works_while_coordinator_lock_is_held(
    tmp_path: Path,
) -> None:
    fleet_home = tmp_path / "fleet-home"
    project_dir = fleet_home / "projects" / "myproj"
    locks_dir = project_dir / ".locks"
    locks_dir.mkdir(parents=True)
    coord_lock = locks_dir / "coordinator.lock"
    ready = tmp_path / "holder-ready"
    holder = subprocess.Popen(
        [
            sys.executable,
            "-c",
            (
                "import fcntl, pathlib, sys, time; "
                "p = pathlib.Path(sys.argv[1]); "
                "p.parent.mkdir(parents=True, exist_ok=True); "
                "fh = p.open('w'); "
                "fcntl.flock(fh, fcntl.LOCK_EX); "
                "pathlib.Path(sys.argv[2]).write_text('ready', encoding='utf-8'); "
                "time.sleep(30)"
            ),
            str(coord_lock),
            str(ready),
        ],
    )
    try:
        deadline = time.monotonic() + 5
        while not ready.exists() and time.monotonic() < deadline:
            time.sleep(0.01)
        assert ready.exists(), "lock holder did not start"

        handoff_resume.record_resumed_handoff_marker(
            "/tmp/handoff.md", project="myproj", home=fleet_home,
        )

        cs = json.loads((project_dir / "coord-state.json").read_text(encoding="utf-8"))
        assert cs["resumed_handoff_path"] == "/tmp/handoff.md"
    finally:
        holder.terminate()
        try:
            holder.wait(timeout=5)
        except subprocess.TimeoutExpired:
            holder.kill()
            holder.wait(timeout=5)


def test_record_resumed_handoff_marker_preserves_malformed_coord_state(
    tmp_path: Path,
) -> None:
    fleet_home = tmp_path / "fleet-home"
    project_dir = fleet_home / "projects" / "myproj"
    (project_dir / ".locks").mkdir(parents=True)
    state_path = project_dir / "coord-state.json"
    garbage = '{"worker_agent_ids": {"foo": "aaaa1111"}, TRUNCATED'
    state_path.write_text(garbage, encoding="utf-8")

    with pytest.raises(ValueError):
        handoff_resume.record_resumed_handoff_marker(
            "/tmp/handoff.md", project="myproj", home=fleet_home,
        )

    assert state_path.read_text(encoding="utf-8") == garbage


def test_record_resumed_session_tasks_preserves_malformed_coord_state(
    tmp_path: Path,
) -> None:
    """codex iter-10 [P2]: a present-but-MALFORMED coord-state.json must NOT
    be clobbered by the best-effort resume stamp. Overwriting it would drop
    sibling keys (worker_agent_ids, recent_decisions, redispatch markers).
    On a parse failure the safe move is to SKIP the write entirely."""
    fleet_home = tmp_path / "fleet-home"
    project_dir = fleet_home / "projects" / "myproj"
    (project_dir / ".locks").mkdir(parents=True)
    state_path = project_dir / "coord-state.json"
    garbage = '{"worker_agent_ids": {"foo": "aaaa1111"}, TRUNCATED'
    state_path.write_text(garbage, encoding="utf-8")

    handoff_resume.record_resumed_session_tasks(
        ["fix-foo"], project="myproj", coord_id="succ0001", home=fleet_home,
    )

    # The malformed file is left byte-for-byte intact — NOT truncated to a
    # fresh {session_tasks:[…]} that would have lost worker_agent_ids.
    assert state_path.read_text(encoding="utf-8") == garbage


def test_record_resumed_session_tasks_writes_when_absent(
    tmp_path: Path,
) -> None:
    """A MISSING coord-state.json is fine to create fresh (only the absent
    case, never a malformed one)."""
    fleet_home = tmp_path / "fleet-home"
    project_dir = fleet_home / "projects" / "myproj"
    (project_dir / ".locks").mkdir(parents=True)

    handoff_resume.record_resumed_session_tasks(
        ["fix-foo"], project="myproj", coord_id="succ0001", home=fleet_home,
    )
    cs = json.loads((project_dir / "coord-state.json").read_text(encoding="utf-8"))
    assert [e["slug"] for e in cs.get("session_tasks", [])] == ["fix-foo"]


def test_main_no_resumable_writes_footer_to_stderr(
    capsys: pytest.CaptureFixture, tmp_path: Path, monkeypatch: pytest.MonkeyPatch,
) -> None:
    fleet_home = tmp_path / "fleet-home"
    wip_dir = tmp_path / "wip"
    fleet_home.mkdir()
    wip_dir.mkdir()
    doc = tmp_path / "handoff.md"
    _seed_handoff(doc, body_subagents="_(none)_")
    monkeypatch.setenv("FLEET_HOME", str(fleet_home))
    monkeypatch.setenv("FLEET_SUBAGENT_WIP_DIR", str(wip_dir))
    rc = handoff_resume.main([str(doc)])
    assert rc == 0
    captured = capsys.readouterr()
    assert "DISPATCH:" not in captured.out
    assert "no resumable subagents" in captured.err


# ----------------------------------------------------------------------
# v0.8.3 rich-state tests: status + pr_url branching, Open PRs section
# ----------------------------------------------------------------------


def test_parse_handoff_doc_reads_status_and_pr_url(tmp_path: Path) -> None:
    """v0.8.3 7-field row: status + pr_url surface on ResumeEntry."""
    p = tmp_path / "handoff.md"
    _seed_handoff(
        p,
        body_subagents=(
            '- task="fix-foo" branch="worker/fix-foo" phase="review-codex" '
            'status="in-review" pr_url="https://example/pr/137" '
            'agent_id="abcd1234" subagent_id=""'
        ),
    )
    got = handoff_resume.parse_handoff_doc(p)
    assert len(got) == 1
    e = got[0]
    assert e.status == "in-review"
    assert e.pr_url == "https://example/pr/137"


def test_parse_handoff_doc_legacy_5field_row_status_empty(tmp_path: Path) -> None:
    """Backward-compat: a 5-field row (no status/pr_url) parses with
    the new fields defaulting to "" — successor falls back to the
    pre-enrichment 'always re-dispatch Agent' behavior."""
    p = tmp_path / "handoff.md"
    _seed_handoff(
        p,
        body_subagents=(
            '- task="legacy" branch="worker/legacy" phase="tdd-green" '
            'agent_id="abcd1234" subagent_id=""'
        ),
    )
    got = handoff_resume.parse_handoff_doc(p)
    assert len(got) == 1
    assert got[0].status == ""
    assert got[0].pr_url == ""


def test_handoff_resume_does_not_redispatch_when_in_review(tmp_path: Path) -> None:
    """An entry with status=in-review + pr_url set must NOT produce a
    DISPATCH block — the shepherd respawn covers it. skipped reason
    surfaces for operator visibility."""
    fleet_home = tmp_path / "fleet-home"
    wip_dir = tmp_path / "wip"
    fleet_home.mkdir()
    wip_dir.mkdir()
    (fleet_home / "inbox").mkdir()
    (fleet_home / "inbox" / "abcd1234.md").write_text("prompt", encoding="utf-8")
    (wip_dir / "fix-foo.md").write_text("phase 1", encoding="utf-8")
    entries = [
        handoff_resume.ResumeEntry(
            task_id="fix-foo", branch="worker/fix-foo", phase="review-codex",
            status="in-review", pr_url="https://example/pr/137",
            agent_id="abcd1234", subagent_id="",
        )
    ]
    blocks, skipped = handoff_resume.build_resume_dispatches(
        entries, wip_dir=wip_dir, fleet_home=fleet_home,
    )
    assert blocks == []
    assert len(skipped) == 1
    assert "in-review" in skipped[0]
    assert "shepherd-only" in skipped[0]


def test_handoff_resume_redispatches_when_in_progress_no_pr(tmp_path: Path) -> None:
    """An entry with empty pr_url + status=in-progress (or empty for
    legacy rows) MUST produce a DISPATCH block — worker was still
    writing code; resume from WIP."""
    fleet_home = tmp_path / "fleet-home"
    wip_dir = tmp_path / "wip"
    fleet_home.mkdir()
    wip_dir.mkdir()
    (fleet_home / "inbox").mkdir()
    (fleet_home / "inbox" / "abcd1234.md").write_text("orig", encoding="utf-8")
    (wip_dir / "fix-foo.md").write_text("phase 1", encoding="utf-8")
    entries = [
        handoff_resume.ResumeEntry(
            task_id="fix-foo", branch="worker/fix-foo", phase="tdd-green",
            status="in-progress", pr_url="",
            agent_id="abcd1234", subagent_id="",
        )
    ]
    blocks, skipped = handoff_resume.build_resume_dispatches(
        entries, wip_dir=wip_dir, fleet_home=fleet_home,
    )
    assert len(blocks) == 1
    assert "DISPATCH: fix-foo" in blocks[0]
    assert skipped == []


def test_handoff_resume_skips_terminal_status(tmp_path: Path) -> None:
    """status=done/abandoned/blocked entries are terminal — no
    re-dispatch action (don't re-spawn a worker on a finished task)."""
    fleet_home = tmp_path / "fleet-home"
    wip_dir = tmp_path / "wip"
    fleet_home.mkdir()
    wip_dir.mkdir()
    entries = [
        handoff_resume.ResumeEntry(
            task_id="done-task", branch="worker/done-task", phase="push",
            status="done", pr_url="",
            agent_id="abcd1234", subagent_id="",
        ),
        handoff_resume.ResumeEntry(
            task_id="blocked-task", branch="worker/blocked-task", phase="blocked",
            status="blocked", pr_url="",
            agent_id="22222222", subagent_id="",
        ),
    ]
    blocks, skipped = handoff_resume.build_resume_dispatches(
        entries, wip_dir=wip_dir, fleet_home=fleet_home,
    )
    assert blocks == []
    assert len(skipped) == 2
    assert any("done" in s for s in skipped)
    assert any("blocked" in s for s in skipped)


def test_handoff_resume_legacy_empty_status_falls_back_to_redispatch(
    tmp_path: Path,
) -> None:
    """Legacy 5-field row → status="" → pre-enrichment 'always re-
    dispatch if WIP+inbox' behavior is preserved."""
    fleet_home = tmp_path / "fleet-home"
    wip_dir = tmp_path / "wip"
    fleet_home.mkdir()
    wip_dir.mkdir()
    (fleet_home / "inbox").mkdir()
    (fleet_home / "inbox" / "abcd1234.md").write_text("orig", encoding="utf-8")
    (wip_dir / "legacy.md").write_text("phase 1", encoding="utf-8")
    entries = [
        handoff_resume.ResumeEntry(
            task_id="legacy", branch="worker/legacy", phase="tdd-green",
            status="", pr_url="",
            agent_id="abcd1234", subagent_id="",
        )
    ]
    blocks, skipped = handoff_resume.build_resume_dispatches(
        entries, wip_dir=wip_dir, fleet_home=fleet_home,
    )
    assert len(blocks) == 1
    assert "DISPATCH: legacy" in blocks[0]


def test_parse_open_prs_placeholder(tmp_path: Path) -> None:
    """Placeholder body parses to empty list."""
    p = tmp_path / "handoff.md"
    _seed_handoff(p, body_subagents="_(none)_")
    assert handoff_resume.parse_open_prs(p) == []


def test_parse_open_prs_single_url(tmp_path: Path) -> None:
    p = tmp_path / "handoff.md"
    _seed_handoff(
        p,
        body_subagents="_(none)_",
        body_open_prs="- #137 feat(handoff): rich state — worker/a — https://github.com/edisonshen/fleet/pull/137",
    )
    got = handoff_resume.parse_open_prs(p)
    assert got == ["https://github.com/edisonshen/fleet/pull/137"]


def test_parse_open_prs_multiple_urls(tmp_path: Path) -> None:
    p = tmp_path / "handoff.md"
    body = (
        "- #137 rich state — worker/a — https://example/pr/137\n"
        "- #138 next slice — worker/b — https://example/pr/138"
    )
    _seed_handoff(p, body_subagents="_(none)_", body_open_prs=body)
    got = handoff_resume.parse_open_prs(p)
    assert got == ["https://example/pr/137", "https://example/pr/138"]


def test_parse_open_prs_missing_section(tmp_path: Path) -> None:
    """A doc with no `## Open PRs` header (pre-v0.8.3) returns []."""
    p = tmp_path / "handoff.md"
    p.write_text(
        '---\nagent_id: "x"\n---\n\n## Active Subagents\n_(none)_\n',
        encoding="utf-8",
    )
    assert handoff_resume.parse_open_prs(p) == []


def test_handoff_resume_respawns_shepherd_for_open_prs(
    capsys: pytest.CaptureFixture, tmp_path: Path, monkeypatch: pytest.MonkeyPatch,
) -> None:
    """The main() CLI surfaces open-PR shepherd hints on stderr (one
    line per URL) so the coord agent can re-establish the watch loops.
    Two URLs in → two `# open-pr-shepherd:` lines on stderr."""
    fleet_home = tmp_path / "fleet-home"
    wip_dir = tmp_path / "wip"
    fleet_home.mkdir()
    wip_dir.mkdir()
    doc = tmp_path / "handoff.md"
    _seed_handoff(
        doc,
        body_subagents="_(none)_",
        body_open_prs=(
            "- #137 rich state — worker/a — https://example/pr/137\n"
            "- #138 next slice — worker/b — https://example/pr/138"
        ),
    )
    monkeypatch.setenv("FLEET_HOME", str(fleet_home))
    monkeypatch.setenv("FLEET_SUBAGENT_WIP_DIR", str(wip_dir))
    rc = handoff_resume.main([str(doc)])
    assert rc == 0
    err = capsys.readouterr().err
    assert "open-pr-shepherd: https://example/pr/137" in err
    assert "open-pr-shepherd: https://example/pr/138" in err
