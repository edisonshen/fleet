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


# ---------- Worker dispatch protocol (issue #84 Phase A) ----------


def test_skill_md_has_worker_dispatch_protocol_section():
    """The Worker dispatch protocol section is the contract between
    the Python skill (which emits DISPATCH blocks) and the coord
    agent (Claude session, which invokes the Agent tool). If this
    section is missing, a fresh or handed-off coord doesn't know to
    act on DISPATCH blocks and workers never spawn — task sits in
    in-progress with no actual worker, supervisor eventually flips
    to todo, hours of lost time."""
    body = _read_skill_md()
    assert "## Worker dispatch protocol" in body, (
        "SKILL.md missing '## Worker dispatch protocol' section — "
        "coord agent has no instructions for handling DISPATCH blocks "
        "(issue #84 Phase A)."
    )


def test_skill_md_dispatch_protocol_explains_agent_tool_invocation():
    """The protocol section MUST tell Claude to invoke the Agent tool
    with run_in_background=true once per DISPATCH block. Drift here
    silently breaks the worker-spawn path."""
    body = _read_skill_md()
    for marker in (
        "DISPATCH:",
        "Agent tool",
        "run_in_background",
        "subagent_type",
        "general-purpose",
        "prompt_file",
        "agent_id",
    ):
        assert marker in body, (
            f"SKILL.md Worker dispatch protocol missing {marker!r} — "
            f"coord agent's parser-by-reasoning would miss the field "
            f"(issue #84 Phase A)."
        )


# ---------- Resume after handoff (issue #93 Phase B2) ----------


def test_skill_md_has_resume_after_handoff_section():
    """The successor coord re-reads SKILL.md on first turn — without a
    'Resume after handoff' section it has no instructions for picking
    up the outgoing coord's in-flight worker subagents (issue #93)."""
    body = _read_skill_md()
    assert "## Resume after handoff" in body, (
        "SKILL.md missing '## Resume after handoff' section — "
        "successor coord won't re-dispatch surviving workers (issue #93 Phase B2)."
    )


def test_skill_md_resume_section_names_helper_module():
    """The section must point at handoff_resume.py + the explicit
    DISPATCH-block protocol so the coord can follow the existing
    Worker dispatch protocol pattern verbatim."""
    body = _read_skill_md()
    for marker in (
        "handoff_resume",
        "Active Subagents",
        "previous_handoff",
        "DISPATCH",
        "WIP",
    ):
        assert marker in body, (
            f"SKILL.md Resume section missing {marker!r} — coord agent "
            f"can't execute the resume protocol (issue #93 Phase B2)."
        )


def test_skill_md_dispatch_protocol_pins_one_call_per_block():
    """The contract is exactly one Agent call per DISPATCH block — N
    blocks → N calls. Drift to "one call per tick" would silently
    drop workers under cap > 1 dispatch."""
    body = _read_skill_md()
    # Look for the explicit framing the coord is expected to follow.
    assert "One Agent call per DISPATCH block" in body or \
        "one Agent call per DISPATCH block" in body or \
        "One Agent call per block" in body or \
        "one Agent call per block" in body or \
        "one per dispatch block" in body.lower() or \
        "one Agent call per" in body, (
            "SKILL.md must pin 'one Agent call per DISPATCH block' so "
            "the coord agent doesn't collapse multi-block ticks into a "
            "single Agent call (issue #84 Phase A)."
        )
