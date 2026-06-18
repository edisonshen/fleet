"""Rolling coord checkpoint tests — high-fidelity crash recovery.

The coord skill writes ~/.fleet/projects/<p>/coord-checkpoint.md every
N ticks (configurable via FLEET_COORD_CHECKPOINT_EVERY, default 5). The
checkpoint bounds the recovery window between fleet-guard's 50%/70%
context handoffs — a dead coord between handoffs is resumable from the
most recent checkpoint rather than the last (potentially hours-old)
handoff doc.

The writer lives in dispatch.py (it reuses the same tmp + fsync +
os.replace atomic-publish dance as dispatch.write_worker_inbox). The
body sections (Active Subagents / Open PRs / Recent decisions) match the
handoff doc shape verbatim so internal/handoff/synth.go can lift the
rows into a synthetic handoff without learning a new format.

Atomicity is the load-bearing invariant: a torn write would corrupt the
recovery path that needs the file most.
"""
from __future__ import annotations

import os
from pathlib import Path

import pytest

import dispatch


# ---------- helpers ----------


def _make_state(
    *,
    tick_count: int = 5,
    recent_decisions: list[str] | None = None,
    recent_completions: list[str] | None = None,
) -> dict:
    """Build a coord-state.json-shaped dict for checkpoint input."""
    return {
        "tick_count": tick_count,
        "recent_decisions": list(recent_decisions or []),
        "recent_completions": list(recent_completions or []),
    }


@pytest.fixture
def project_dir(tmp_path: Path) -> Path:
    home = tmp_path / "fleet"
    pdir = home / "projects" / "myproj"
    pdir.mkdir(parents=True)
    return pdir


# ---------- write tests ----------


def test_write_emits_frontmatter_with_schema_v1(project_dir: Path):
    """The checkpoint file must start with `---` and carry schema=v1 +
    coord_id + project + updated_at (RFC3339 UTC) + tick_count.

    Successor coords read this frontmatter to decide between recovering
    from the checkpoint vs. the last handoff doc.
    """
    state = _make_state(tick_count=10)
    path = dispatch.write_coord_checkpoint(
        project_dir=project_dir,
        coord_id="a1b2c3d4",
        project="myproj",
        state=state,
        active_subagents=[],
        open_prs=[],
    )

    body = Path(path).read_text(encoding="utf-8")
    assert body.startswith("---\n"), "missing opening frontmatter delimiter"

    fm_end = body.find("\n---\n", 4)
    assert fm_end > 0, "missing closing frontmatter delimiter"

    fm_block = body[4:fm_end]
    assert "schema: v1" in fm_block, f"missing schema=v1 in:\n{fm_block}"
    assert 'coord_id: "a1b2c3d4"' in fm_block
    assert 'project: "myproj"' in fm_block
    assert "tick_count: 10" in fm_block
    assert "updated_at: " in fm_block
    line = next(line for line in fm_block.splitlines() if line.startswith("updated_at:"))
    assert line.endswith('Z"'), f"updated_at not RFC3339-Z: {line!r}"


def test_write_emits_sections_in_order(project_dir: Path):
    """Body sections must appear in the documented order: Active
    Subagents, Open PRs, Recent decisions, Completed (recent), Drafted
    but unfiled tasks (Slice 2 inserts Completed (recent) after Recent
    decisions, before Drafted)."""
    state = _make_state(
        tick_count=5,
        recent_decisions=["dispatch fix-foo", "drain bar"],
        recent_completions=["done fix-foo", "merged #7 fix-bar"],
    )
    active = [
        {
            "task": "fix-foo", "branch": "worker/fix-foo",
            "phase": "tdd-green", "status": "in-progress",
            "pr_url": "", "agent_id": "deadbeef", "subagent_id": "",
        },
    ]
    open_prs = [
        {"number": 42, "title": "fix: foo", "head": "worker/fix-foo",
         "url": "https://github.com/owner/repo/pull/42"},
    ]
    path = dispatch.write_coord_checkpoint(
        project_dir=project_dir,
        coord_id="a1b2c3d4",
        project="myproj",
        state=state,
        active_subagents=active,
        open_prs=open_prs,
    )
    body = Path(path).read_text(encoding="utf-8")

    idx_active = body.index("### Active Subagents")
    idx_prs = body.index("### Open PRs")
    idx_dec = body.index("### Recent decisions")
    idx_completed = body.index("### Completed (recent)")
    idx_drafted = body.index("### Drafted but unfiled tasks")
    assert idx_active < idx_prs < idx_dec < idx_completed < idx_drafted, (
        f"section order wrong: active={idx_active} prs={idx_prs} "
        f"decisions={idx_dec} completed={idx_completed} drafted={idx_drafted}"
    )


def test_write_active_subagents_uses_handoff_row_format(project_dir: Path):
    """Active Subagents body must render one bullet per worker with the
    same 7-field key="value" shape as handoff.py — so the synth.go path
    that lifts these rows verbatim doesn't have to translate."""
    state = _make_state(tick_count=5)
    active = [
        {
            "task": "fix-foo", "branch": "worker/fix-foo",
            "phase": "tdd-green", "status": "in-progress",
            "pr_url": "https://github.com/owner/repo/pull/1",
            "agent_id": "deadbeef", "subagent_id": "sub-id",
        },
    ]
    path = dispatch.write_coord_checkpoint(
        project_dir=project_dir,
        coord_id="a1b2c3d4",
        project="myproj",
        state=state,
        active_subagents=active,
        open_prs=[],
    )
    body = Path(path).read_text(encoding="utf-8")

    expected_line = (
        '- task="fix-foo" branch="worker/fix-foo" phase="tdd-green" '
        'status="in-progress" pr_url="https://github.com/owner/repo/pull/1" '
        'agent_id="deadbeef" subagent_id="sub-id"'
    )
    assert expected_line in body, (
        f"expected handoff-shape row missing; got body:\n{body}"
    )


def test_write_active_subagent_values_are_go_quoted(project_dir: Path):
    """Values with embedded quotes / backslashes must be escaped the
    same way Go's strconv.Quote does, so synth.go's strconv.Unquote
    round-trips them. A bare `"` in a value would break the parser."""
    state = _make_state(tick_count=5)
    active = [
        {
            "task": 'we"ird', "branch": "worker/x",
            "phase": "p", "status": "s",
            "pr_url": "", "agent_id": "deadbeef", "subagent_id": "",
        },
    ]
    path = dispatch.write_coord_checkpoint(
        project_dir=project_dir,
        coord_id="a1b2c3d4",
        project="myproj",
        state=state,
        active_subagents=active,
        open_prs=[],
    )
    body = Path(path).read_text(encoding="utf-8")
    assert r'task="we\"ird"' in body, f"quote not escaped in:\n{body}"


def test_write_empty_sections_render_placeholders(project_dir: Path):
    """Empty active subagents / open PRs / recent decisions each render
    a placeholder body so the synth parser never trips on a missing
    section header."""
    state = _make_state(tick_count=5, recent_decisions=[])
    path = dispatch.write_coord_checkpoint(
        project_dir=project_dir,
        coord_id="a1b2c3d4",
        project="myproj",
        state=state,
        active_subagents=[],
        open_prs=[],
    )
    body = Path(path).read_text(encoding="utf-8")
    assert "_(none)_" in body, "expected _(none)_ placeholder for empty Active Subagents"
    assert "_(no open PRs)_" in body, "expected _(no open PRs)_ placeholder"
    assert "_(no recent decisions)_" in body, "expected _(no recent decisions)_ placeholder"
    assert "_(no recent completions)_" in body, "expected _(no recent completions)_ placeholder"
    assert "_(empty — populated in Phase 2)_" in body, (
        "expected Drafted but unfiled tasks placeholder"
    )


def test_write_open_prs_render_bullets(project_dir: Path):
    """Open PRs render as `- #N title — head — url`, matching handoff.py's
    renderOpenPRs so synth.go can parse them back."""
    state = _make_state(tick_count=5)
    open_prs = [
        {"number": 7, "title": "feat: x", "head": "worker/x",
         "url": "https://github.com/o/r/pull/7"},
        {"number": 8, "title": "fix: y", "head": "worker/y",
         "url": "https://github.com/o/r/pull/8"},
    ]
    path = dispatch.write_coord_checkpoint(
        project_dir=project_dir,
        coord_id="a1b2c3d4",
        project="myproj",
        state=state,
        active_subagents=[],
        open_prs=open_prs,
    )
    body = Path(path).read_text(encoding="utf-8")
    assert "- #7 feat: x — worker/x — https://github.com/o/r/pull/7" in body
    assert "- #8 fix: y — worker/y — https://github.com/o/r/pull/8" in body


def test_write_recent_decisions_renders_bullets(project_dir: Path):
    """Recent decisions section emits one bullet per entry, in order."""
    state = _make_state(
        tick_count=5,
        recent_decisions=[
            "dispatched fix-foo",
            "promoted bar to ready",
            "operator inbox: archive done",
        ],
    )
    path = dispatch.write_coord_checkpoint(
        project_dir=project_dir,
        coord_id="a1b2c3d4",
        project="myproj",
        state=state,
        active_subagents=[],
        open_prs=[],
    )
    body = Path(path).read_text(encoding="utf-8")
    decisions_idx = body.index("### Recent decisions")
    drafted_idx = body.index("### Drafted but unfiled tasks")
    section = body[decisions_idx:drafted_idx]
    for line in (
        "- dispatched fix-foo",
        "- promoted bar to ready",
        "- operator inbox: archive done",
    ):
        assert line in section, f"missing decision line: {line!r} in:\n{section}"


def test_write_recent_completions_renders_bullets(project_dir: Path):
    """Completed (recent) section emits one bullet per entry, in order,
    between Recent decisions and Drafted but unfiled tasks."""
    state = _make_state(
        tick_count=5,
        recent_completions=[
            "done fix-foo",
            "merged #7 fix-bar",
        ],
    )
    path = dispatch.write_coord_checkpoint(
        project_dir=project_dir,
        coord_id="a1b2c3d4",
        project="myproj",
        state=state,
        active_subagents=[],
        open_prs=[],
    )
    body = Path(path).read_text(encoding="utf-8")
    completed_idx = body.index("### Completed (recent)")
    drafted_idx = body.index("### Drafted but unfiled tasks")
    section = body[completed_idx:drafted_idx]
    for line in (
        "- done fix-foo",
        "- merged #7 fix-bar",
    ):
        assert line in section, f"missing completion line: {line!r} in:\n{section}"


def test_write_is_atomic_against_partial_writes(project_dir: Path, monkeypatch):
    """A crash mid-write must NOT leave a half-written coord-checkpoint.md
    at the target path. We simulate the crash by making os.replace raise
    and verify (a) the prior body is preserved, (b) the tmp file is
    cleaned up."""
    state = _make_state(tick_count=5)
    target = project_dir / "coord-checkpoint.md"

    prior = "---\nschema: v1\ncoord_id: \"prior\"\n---\nold body\n"
    target.write_text(prior, encoding="utf-8")

    boom = RuntimeError("simulated crash mid-replace")

    def explode(src, dst):
        raise boom

    monkeypatch.setattr(os, "replace", explode)

    with pytest.raises(RuntimeError):
        dispatch.write_coord_checkpoint(
            project_dir=project_dir,
            coord_id="a1b2c3d4",
            project="myproj",
            state=state,
            active_subagents=[],
            open_prs=[],
        )
    assert target.read_text(encoding="utf-8") == prior
    leftovers = [
        p for p in project_dir.iterdir()
        if p.name.startswith("coord-checkpoint.md.tmp.")
    ]
    assert leftovers == [], f"leftover tmp files: {leftovers}"


def test_write_returns_target_path(project_dir: Path):
    """The writer returns the absolute path written so callers can log
    it. Path must be `<project_dir>/coord-checkpoint.md` exactly — the
    successor resume path looks for this literal filename."""
    state = _make_state(tick_count=5)
    path = dispatch.write_coord_checkpoint(
        project_dir=project_dir,
        coord_id="a1b2c3d4",
        project="myproj",
        state=state,
        active_subagents=[],
        open_prs=[],
    )
    assert Path(path) == project_dir / "coord-checkpoint.md"


def test_write_creates_missing_project_dir(tmp_path: Path):
    """The writer creates the project dir if it doesn't exist yet (a
    coord's first checkpoint may precede any other file under the dir)."""
    pdir = tmp_path / "fleet" / "projects" / "fresh"
    assert not pdir.exists()
    state = _make_state(tick_count=5)
    path = dispatch.write_coord_checkpoint(
        project_dir=pdir,
        coord_id="a1b2c3d4",
        project="fresh",
        state=state,
        active_subagents=[],
        open_prs=[],
    )
    assert Path(path).exists()


def test_write_tolerates_non_int_tick_count(project_dir: Path):
    """A corrupted tick_count in state.json must not crash the writer —
    it falls back to 0 rather than failing the checkpoint."""
    state = {"tick_count": "garbage", "recent_decisions": []}
    path = dispatch.write_coord_checkpoint(
        project_dir=project_dir,
        coord_id="a1b2c3d4",
        project="myproj",
        state=state,
        active_subagents=[],
        open_prs=[],
    )
    body = Path(path).read_text(encoding="utf-8")
    assert "tick_count: 0" in body


# ---------- interval tests ----------


@pytest.mark.parametrize(
    "tick_count,every,expected",
    [
        (1, 5, False),
        (4, 5, False),
        (5, 5, True),
        (6, 5, False),
        (10, 5, True),
        (3, 3, True),
        (0, 5, False),  # tick_count=0 means coord just started, no checkpoint yet
        (1, 1, True),
        (5, 0, False),  # every=0 disables
        (50, 0, False),  # every=0 disables irrespective of tick
    ],
)
def test_should_checkpoint_interval_logic(tick_count: int, every: int, expected: bool):
    """should_checkpoint(tick_count, every) returns True only when
    tick_count > 0 AND every > 0 AND tick_count % every == 0."""
    assert dispatch.should_checkpoint(tick_count, every) == expected


def test_resolve_checkpoint_every_defaults_to_5(monkeypatch):
    monkeypatch.delenv("FLEET_COORD_CHECKPOINT_EVERY", raising=False)
    assert dispatch.resolve_checkpoint_every() == 5


def test_resolve_checkpoint_every_honors_override(monkeypatch):
    monkeypatch.setenv("FLEET_COORD_CHECKPOINT_EVERY", "3")
    assert dispatch.resolve_checkpoint_every() == 3


def test_resolve_checkpoint_every_zero_disables(monkeypatch):
    monkeypatch.setenv("FLEET_COORD_CHECKPOINT_EVERY", "0")
    assert dispatch.resolve_checkpoint_every() == 0


def test_resolve_checkpoint_every_invalid_falls_back(monkeypatch):
    monkeypatch.setenv("FLEET_COORD_CHECKPOINT_EVERY", "not-an-int")
    assert dispatch.resolve_checkpoint_every() == 5


def test_resolve_checkpoint_every_negative_falls_back(monkeypatch):
    """Negative interval is operator misconfig — refuse rather than
    silently flipping to "every tick" which amplifies the misconfig
    into disk pressure."""
    monkeypatch.setenv("FLEET_COORD_CHECKPOINT_EVERY", "-1")
    assert dispatch.resolve_checkpoint_every() == 5


def test_resolve_checkpoint_decisions_defaults_to_10(monkeypatch):
    monkeypatch.delenv("FLEET_COORD_CHECKPOINT_DECISIONS", raising=False)
    assert dispatch.resolve_checkpoint_decisions() == 10


def test_resolve_checkpoint_decisions_honors_override(monkeypatch):
    monkeypatch.setenv("FLEET_COORD_CHECKPOINT_DECISIONS", "25")
    assert dispatch.resolve_checkpoint_decisions() == 25


def test_resolve_checkpoint_decisions_invalid_falls_back(monkeypatch):
    monkeypatch.setenv("FLEET_COORD_CHECKPOINT_DECISIONS", "abc")
    assert dispatch.resolve_checkpoint_decisions() == 10


def test_resolve_checkpoint_decisions_negative_falls_back(monkeypatch):
    monkeypatch.setenv("FLEET_COORD_CHECKPOINT_DECISIONS", "-3")
    assert dispatch.resolve_checkpoint_decisions() == 10


# ---------- record_decision tests ----------


def test_record_decision_appends_and_caps_state(monkeypatch):
    """record_checkpoint_decision mutates state in place, appending the
    new event and trimming the buffer to max_decisions entries."""
    monkeypatch.setenv("FLEET_COORD_CHECKPOINT_DECISIONS", "3")
    state = {"recent_decisions": []}
    dispatch.record_checkpoint_decision(state, "first")
    dispatch.record_checkpoint_decision(state, "second")
    dispatch.record_checkpoint_decision(state, "third")
    dispatch.record_checkpoint_decision(state, "fourth")
    assert state["recent_decisions"] == ["second", "third", "fourth"]


def test_record_decision_initializes_missing_list():
    """record_checkpoint_decision tolerates a state dict that has never
    carried a recent_decisions key (fresh coord first tick)."""
    state = {}
    dispatch.record_checkpoint_decision(state, "hello")
    assert state["recent_decisions"] == ["hello"]


def test_record_decision_no_blank_lines():
    """Empty / whitespace-only decisions are silently dropped — they'd
    render as bare ` - ` bullets which is meaningless noise."""
    state = {"recent_decisions": []}
    dispatch.record_checkpoint_decision(state, "")
    dispatch.record_checkpoint_decision(state, "   ")
    dispatch.record_checkpoint_decision(state, "\n")
    assert state["recent_decisions"] == []


def test_record_decision_strips_newlines():
    """Embedded newlines would corrupt the bullet-per-line markdown
    contract; flatten them to spaces on ingestion."""
    state = {"recent_decisions": []}
    dispatch.record_checkpoint_decision(state, "line1\nline2")
    assert state["recent_decisions"] == ["line1 line2"]


def test_record_decision_tolerates_corrupt_buffer():
    """A non-list recent_decisions (corrupt state.json) is reset to a
    fresh list rather than crashing."""
    state = {"recent_decisions": "not-a-list"}
    dispatch.record_checkpoint_decision(state, "hello")
    assert state["recent_decisions"] == ["hello"]


# ---------- record_checkpoint_completion (Slice 2) ----------


def test_record_completion_appends_and_caps_state(monkeypatch):
    """record_checkpoint_completion mutates state in place, appending the
    line and capping to the FLEET_COORD_CHECKPOINT_DECISIONS limit — the
    same cap that bounds recent_decisions."""
    monkeypatch.setenv("FLEET_COORD_CHECKPOINT_DECISIONS", "3")
    state = {"recent_completions": []}
    dispatch.record_checkpoint_completion(state, "first")
    dispatch.record_checkpoint_completion(state, "second")
    dispatch.record_checkpoint_completion(state, "third")
    dispatch.record_checkpoint_completion(state, "fourth")
    assert state["recent_completions"] == ["second", "third", "fourth"]


def test_record_completion_initializes_missing_list():
    """Tolerates a state dict that has never carried a recent_completions
    key (fresh coord first tick)."""
    state = {}
    dispatch.record_checkpoint_completion(state, "hello")
    assert state["recent_completions"] == ["hello"]


def test_record_completion_no_blank_lines():
    """Blank / whitespace-only entries are dropped."""
    state = {"recent_completions": []}
    dispatch.record_checkpoint_completion(state, "")
    dispatch.record_checkpoint_completion(state, "   ")
    dispatch.record_checkpoint_completion(state, "\n")
    assert state["recent_completions"] == []


def test_record_completion_strips_newlines():
    """Embedded newlines flatten to spaces (bullet-per-line contract)."""
    state = {"recent_completions": []}
    dispatch.record_checkpoint_completion(state, "line1\nline2")
    assert state["recent_completions"] == ["line1 line2"]


def test_record_completion_tolerates_corrupt_buffer():
    """A non-list recent_completions (corrupt state.json) is reset to a
    fresh list rather than crashing."""
    state = {"recent_completions": "not-a-list"}
    dispatch.record_checkpoint_completion(state, "hello")
    assert state["recent_completions"] == ["hello"]


# ---------- round-trip: frontmatter parses ----------


def test_write_round_trip_parses_frontmatter(project_dir: Path):
    """The frontmatter we emit must round-trip through a line-based
    key:value reader (the same grammar synth.go uses)."""
    state = _make_state(tick_count=42)
    path = dispatch.write_coord_checkpoint(
        project_dir=project_dir,
        coord_id="a1b2c3d4",
        project="myproj",
        state=state,
        active_subagents=[],
        open_prs=[],
    )
    body = Path(path).read_text(encoding="utf-8")
    assert body.startswith("---\n")
    end = body.index("\n---\n", 4)
    fm = body[4:end]
    parsed: dict[str, str] = {}
    for line in fm.splitlines():
        if ":" not in line:
            continue
        k, _, v = line.partition(":")
        parsed[k.strip()] = v.strip()
    assert parsed["schema"] == "v1"
    assert parsed["coord_id"] == '"a1b2c3d4"'
    assert parsed["project"] == '"myproj"'
    assert parsed["tick_count"] == "42"
    assert parsed["updated_at"].startswith('"') and parsed["updated_at"].endswith('Z"')
