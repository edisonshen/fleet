"""SKILL.md role-boundary content tests (issue #80).

The /coordinator skill agent is a manager — it discusses design with
the operator, files tasks, and dispatches workers. It NEVER edits code
or runs tests inline. Two layers enforce this:

  1. The first-turn dispatch prompt (coordSpawnPrompt in
     internal/tui/keys.go) — fresh spawn only.
  2. The "## Coord agent role" section in SKILL.md — survives context
     handoffs; the successor coord re-reads SKILL.md on its first
     turn, not the original spawn prompt.

If layer 2 disappears, a handed-off coord starts editing code and the
constraint is silently lost. These tests pin the SKILL.md content so
the role section can't quietly drift.
"""
from __future__ import annotations

import os


_SKILL_MD = os.path.join(
    os.path.dirname(os.path.dirname(os.path.abspath(__file__))),
    "SKILL.md",
)


def _read_skill_md() -> str:
    with open(_SKILL_MD, "r", encoding="utf-8") as f:
        return f.read()


def test_skill_md_has_coord_agent_role_section():
    """The top-level role section must exist verbatim (matches the
    first-turn dispatch prompt's framing in keys.go)."""
    body = _read_skill_md()
    assert "## Coord agent role" in body, (
        "SKILL.md missing '## Coord agent role' section — handed-off "
        "coords lose the role constraint without it (issue #80)."
    )


def test_skill_md_role_section_includes_constraint_markers():
    """The role section must carry the ROLE / DELEGATE / ALLOWED /
    NEVER markers that mirror the dispatch prompt body."""
    body = _read_skill_md()
    for marker in ("ROLE", "DELEGATE", "ALLOWED", "NEVER"):
        assert marker in body, (
            f"SKILL.md missing {marker!r} marker — role boundary "
            f"wording drifted from the dispatch prompt (issue #80)."
        )


def test_skill_md_role_section_names_dispatch_path():
    """The role section must point the agent at the fleet CLI for
    delegation; otherwise a coord that re-reads SKILL.md after a
    handoff sees the constraint but no path to comply."""
    body = _read_skill_md()
    for needle in (
        "fleet tasks add",
        "fleet tasks promote",
    ):
        assert needle in body, (
            f"SKILL.md role section missing CLI delegation path "
            f"({needle!r}) — coords need an explicit how-to (issue #80)."
        )
