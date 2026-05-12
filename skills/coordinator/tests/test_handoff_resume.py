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

from pathlib import Path

import pytest

import handoff_resume


def _seed_handoff(
    doc_path: Path,
    *,
    body_subagents: str,
    body_open_prs: str = "_(no open PRs)_",
) -> None:
    """Write a minimal handoff doc with the Active Subagents section
    filled with the given body_subagents string and the Open PRs
    section filled with body_open_prs (defaults to placeholder)."""
    doc = (
        "---\n"
        'agent_id: "abcd1234"\n'
        'task_id: "coord-myproj"\n'
        'project: "myproj"\n'
        "context_pct_at_handoff: 50\n"
        "previous_handoff: null\n"
        "handoff_number: 1\n"
        'timestamp: "2026-04-28T12:34:56Z"\n'
        'handoff_type: "auto-yellow"\n'
        "---\n\n"
        "## First Action (auto)\nfoo\n\n"
        "## Completed\nx\n\n"
        "## Key Decisions\nx\n\n"
        "## Files Modified\nx\n\n"
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
