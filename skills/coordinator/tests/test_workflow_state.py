"""Unit tests for ``skills/coordinator/workflow_state.py``.

Covers the atomic-publish path (G5 contract) and the schema constraints.
The legacy supervisor-disabled fixture in ``conftest.py`` does not
interfere — these tests don't exercise ``loop.tick``.
"""
from __future__ import annotations

import os

import pytest

import workflow_state as ws  # type: ignore


# ---------------------------------------------------------------------------
# WorkflowEntry validation
# ---------------------------------------------------------------------------

def test_phases_constant_locked() -> None:
    # Schema is locked at v1; SKILL.md and docs/COORDINATOR-WORKFLOW.md
    # reference these exact strings. Adding a phase MUST also update
    # both docs and bump the schema version.
    assert ws.PHASES == (
        "discussing",
        "approved",
        "dispatched",
        "reviewing",
        "pr-open",
        "merged",
        "blocked",
    )


@pytest.mark.parametrize("phase", list(ws.PHASES))
def test_workflow_entry_accepts_every_locked_phase(phase: str) -> None:
    entry = ws.WorkflowEntry(slug="auth-retry-1234", phase=phase)
    assert entry.phase == phase


def test_workflow_entry_rejects_invalid_phase() -> None:
    with pytest.raises(ValueError, match="invalid phase"):
        ws.WorkflowEntry(slug="x", phase="ready")  # not in PHASES


def test_workflow_entry_rejects_empty_slug() -> None:
    with pytest.raises(ValueError, match="invalid slug"):
        ws.WorkflowEntry(slug="", phase="approved")


def test_workflow_entry_rejects_overlong_slug() -> None:
    with pytest.raises(ValueError, match="invalid slug"):
        ws.WorkflowEntry(slug="a" * 65, phase="approved")


def test_workflow_entry_rejects_newline_in_slug() -> None:
    with pytest.raises(ValueError, match="newline"):
        ws.WorkflowEntry(slug="bad\nslug", phase="approved")


def test_workflow_entry_rejects_newline_in_pr_url() -> None:
    with pytest.raises(ValueError, match="pr_url"):
        ws.WorkflowEntry(slug="x", phase="pr-open", pr_url="https://example.com\nnext")


def test_workflow_entry_rejects_newline_in_note() -> None:
    with pytest.raises(ValueError, match="note"):
        ws.WorkflowEntry(slug="x", phase="blocked", note="line1\nline2")


# ---------------------------------------------------------------------------
# render() — pure formatter
# ---------------------------------------------------------------------------

def test_render_minimal_doc_has_frontmatter_and_heading() -> None:
    out = ws.render([], project="fleet", now="2026-05-09T00:00:00Z")
    assert out.startswith("---\nschema: v1\nproject: fleet\nupdated_at: 2026-05-09T00:00:00Z\n---\n")
    assert "\n# workflow\n" in out


def test_render_emits_one_section_per_entry_in_order() -> None:
    entries = [
        ws.WorkflowEntry(slug="alpha", phase="dispatched", note="first"),
        ws.WorkflowEntry(slug="beta", phase="pr-open", pr_url="https://example/1"),
        ws.WorkflowEntry(slug="gamma", phase="merged", pr_url="https://example/2"),
    ]
    out = ws.render(entries, project="fleet", now="2026-05-09T00:00:00Z")
    # Headings appear in passed order.
    idx_a = out.index("## alpha")
    idx_b = out.index("## beta")
    idx_g = out.index("## gamma")
    assert idx_a < idx_b < idx_g
    # Per-entry fields rendered.
    assert "- phase: dispatched" in out
    assert "- pr_url: https://example/1" in out
    assert "- note: first" in out


def test_render_uses_now_when_entry_updated_at_blank() -> None:
    entries = [ws.WorkflowEntry(slug="x", phase="discussing")]
    out = ws.render(entries, project="fleet", now="2026-05-09T12:34:56Z")
    # Both header and row pick up the publish-time stamp.
    assert "updated_at: 2026-05-09T12:34:56Z" in out
    # Two occurrences: header + the single row.
    assert out.count("2026-05-09T12:34:56Z") == 2


def test_render_preserves_per_entry_updated_at() -> None:
    entries = [
        ws.WorkflowEntry(
            slug="x", phase="merged", pr_url="https://e/1",
            updated_at="2026-05-08T01:02:03Z",
        ),
    ]
    out = ws.render(entries, project="fleet", now="2026-05-09T00:00:00Z")
    assert "- updated_at: 2026-05-08T01:02:03Z" in out
    # Header still uses the publish-time now.
    assert "updated_at: 2026-05-09T00:00:00Z" in out


def test_render_rejects_duplicate_slugs() -> None:
    entries = [
        ws.WorkflowEntry(slug="dup", phase="approved"),
        ws.WorkflowEntry(slug="dup", phase="merged"),
    ]
    with pytest.raises(ValueError, match="duplicate slug"):
        ws.render(entries, project="fleet")


def test_render_rejects_empty_project() -> None:
    with pytest.raises(ValueError, match="project must be non-empty"):
        ws.render([], project="")


# ---------------------------------------------------------------------------
# publish() — atomic write
# ---------------------------------------------------------------------------

def test_publish_writes_target_under_fleet_home(tmp_path) -> None:
    home = tmp_path / "fleet"
    target = ws.publish(
        [ws.WorkflowEntry(slug="t1", phase="approved")],
        project="myproj",
        fleet_home=str(home),
        now="2026-05-09T00:00:00Z",
    )
    expected = str(home / "projects" / "myproj" / "workflow.md")
    assert target == expected
    assert os.path.isfile(target)
    body = open(target, "r", encoding="utf-8").read()
    assert "## t1" in body
    assert "- phase: approved" in body
    assert body.endswith("\n")


def test_publish_creates_parent_dirs_if_missing(tmp_path) -> None:
    home = tmp_path / "fleet"
    # Parent doesn't exist yet — publish must create it.
    target = ws.publish([], project="myproj", fleet_home=str(home))
    assert os.path.isfile(target)
    parent = os.path.dirname(target)
    assert os.path.isdir(parent)


def test_publish_overwrites_existing_file_atomically(tmp_path) -> None:
    home = tmp_path / "fleet"
    target = ws.publish(
        [ws.WorkflowEntry(slug="t1", phase="approved")],
        project="myproj",
        fleet_home=str(home),
        now="2026-05-09T00:00:00Z",
    )
    first_inode = os.stat(target).st_ino
    # Second publish replaces atomically (different inode after os.replace).
    ws.publish(
        [ws.WorkflowEntry(slug="t1", phase="merged", pr_url="https://e/1")],
        project="myproj",
        fleet_home=str(home),
        now="2026-05-09T00:00:01Z",
    )
    body = open(target, "r", encoding="utf-8").read()
    assert "- phase: merged" in body
    assert "- phase: approved" not in body
    second_inode = os.stat(target).st_ino
    assert first_inode != second_inode


def test_publish_leaves_no_tmp_files_on_success(tmp_path) -> None:
    home = tmp_path / "fleet"
    ws.publish(
        [ws.WorkflowEntry(slug="t1", phase="approved")],
        project="myproj",
        fleet_home=str(home),
    )
    parent = home / "projects" / "myproj"
    leftovers = [p for p in parent.iterdir() if p.name.startswith("workflow.tmp.")]
    assert leftovers == []


def test_publish_cleans_up_tmp_on_render_failure(tmp_path, monkeypatch) -> None:
    """If render() raises (e.g. duplicate slug), no tmp file is left behind."""
    home = tmp_path / "fleet"
    parent = home / "projects" / "myproj"
    parent.mkdir(parents=True)

    entries = [
        ws.WorkflowEntry(slug="dup", phase="approved"),
        ws.WorkflowEntry(slug="dup", phase="merged"),
    ]
    with pytest.raises(ValueError, match="duplicate slug"):
        ws.publish(entries, project="myproj", fleet_home=str(home))
    leftovers = [p for p in parent.iterdir() if p.name.startswith("workflow.tmp.")]
    assert leftovers == []


def test_publish_honors_fleet_home_env_var(tmp_path, monkeypatch) -> None:
    home = tmp_path / "envhome"
    monkeypatch.setenv("FLEET_HOME", str(home))
    target = ws.publish(
        [ws.WorkflowEntry(slug="t1", phase="approved")],
        project="myproj",
    )
    assert target.startswith(str(home))
    assert os.path.isfile(target)


def test_publish_empty_entries_writes_steady_state_header(tmp_path) -> None:
    home = tmp_path / "fleet"
    target = ws.publish([], project="myproj", fleet_home=str(home), now="2026-05-09T00:00:00Z")
    body = open(target, "r", encoding="utf-8").read()
    assert "schema: v1" in body
    assert "# workflow" in body
    # No task heading.
    assert "## " not in body


def test_workflow_path_rejects_empty_project(tmp_path) -> None:
    with pytest.raises(ValueError, match="project must be non-empty"):
        ws.workflow_path("", fleet_home=str(tmp_path))
