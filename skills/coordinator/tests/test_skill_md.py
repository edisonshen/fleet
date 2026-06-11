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


def test_skill_md_requires_plan_doc_before_split():
    """Approved implementation plans must be durable docs before the
    coord mutates tasks.md. This pins the PLAN-DOC gate between DISCUSS
    and SPLIT so a handed-off coord does not regress to chat-only plans."""
    body = _read_skill_md()
    assert "PLAN-DOC" in body
    assert "Before splitting tasks" in body
    assert "approved implementation plan" in body
    assert "docs/DESIGN-<kebab-topic>.md" in body


def test_skill_md_requires_task_plan_doc_before_implement():
    """Task-level docs must exist before promotion/implementation so each
    worker has a durable worker-ready plan, not just a tasks.md row."""
    body = _read_skill_md()
    assert "TASK-PLAN-DOC" in body
    assert "Before any task is promoted to ready" in body
    assert "docs/TASK-PLAN-<slug>.md" in body
    assert "fleet tasks note --project <project> <slug> --section spec" in body
    assert "worker-visible task text" in body
    assert "fleet tasks promote <slug>` happens only after" in body


def test_skill_md_names_narrow_plan_doc_write_exception():
    """The coord remains read-only on code; the only source-tree write it
    may perform is saving/rendering approved plan docs under docs/."""
    body = _read_skill_md()
    for needle in (
        "source-tree mutation exceptions",
        "Write/render approved implementation plan docs and per-task plan docs",
        "does **not** proceed to SPLIT",
    ):
        assert needle in body


def test_skill_md_opens_rendered_html_at_doc_gates():
    """After rendering a plan-doc or task-plan-doc .html, the coord must
    `open` it so the human reviewer sees it immediately (auto-open-html
    rule). Pinned at both gates; if the instruction drifts out, a coord
    silently renders HTML the operator never gets shown."""
    body = _read_skill_md()
    assert "open docs/DESIGN-<kebab-topic>.html" in body, (
        "SKILL.md PLAN-DOC gate must instruct opening the rendered .html "
        "(auto-open-html rule)."
    )
    assert "open docs/TASK-PLAN-<slug>.html" in body, (
        "SKILL.md TASK-PLAN-DOC gate must instruct opening the rendered "
        ".html (auto-open-html rule)."
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


# ---------- register_subagent step (issue #94 Phase C) ----------


def test_skill_md_dispatch_protocol_names_register_subagent():
    """The Worker dispatch protocol must instruct the coord agent to
    capture the Agent tool's `subagent_id` and run register_subagent.py
    so the fleet TUI can render `· <8-char>` cross-reference chips
    (issue #94 Phase C). Drift here silently leaves the chip empty."""
    body = _read_skill_md()
    for marker in (
        "register_subagent",
        "subagent_id",
        # CLI usage pattern — operators can grep this in handoff docs.
        "register_subagent.py",
    ):
        assert marker in body, (
            f"SKILL.md Worker dispatch protocol missing {marker!r} — "
            f"coord agent skips Phase C subagent_id capture (issue #94)."
        )


# ---------- three-stage flow §6 documentation (reviewer-subagent-arch) ----------


def test_skill_md_step6_documents_three_stage_flow():
    """The Step 6 IMPLEMENT section must spell out the worker →
    reviewer → finisher split. Drift here lets handed-off coords
    revert to the old single-subagent dispatch pattern, which is the
    structural failure mode the three-stage flow exists to prevent."""
    body = _read_skill_md()
    # Section heading.
    assert "### Step 6 — IMPLEMENT" in body
    # The three-stage phrase has to appear in the IMPLEMENT prose so future
    # readers grep for it.
    assert "three-stage flow" in body or "three subagents" in body
    # Each of the three roles is named.
    for role in ("worker", "reviewer", "finisher"):
        assert role in body, f"SKILL.md IMPLEMENT section missing role {role!r}"
    # The handoff phases.
    for phase in ("review-pending", "review-done"):
        assert phase in body, f"SKILL.md IMPLEMENT section missing handoff phase {phase!r}"
    # The codex skip allowlist is documented (operator-readable guard).
    assert "rate-limited" in body and "unavailable" in body
    # /review is never skippable (load-bearing reviewer).
    assert "NEVER skippable" in body or "never skippable" in body


# ---------- Task-plan review SOP (operator-approved 2026-06-11) ----------


def test_skill_md_step5_documents_task_plan_review_sop():
    """Step 5 must carry the dual-review SOP: the coord never reviews
    inline, every TASK-PLAN doc set gets a dispatched codex + Claude
    dual review before promote, the fix/re-review loop runs until both
    are P0/P1-clean, and reviews-clean never auto-promotes. Drift here
    lets a handed-off coord regress to inline self-review or skip the
    pre-promote review gate entirely.

    Assertions are scoped to the Step 5 section (not the whole file) so
    review-flavored text in Step 6 can never mask deletion of an SOP
    line, and pin full operator-approved clauses (hard wraps flattened)
    rather than word fragments so the meaning can't drift while the
    keywords survive."""
    body = _read_skill_md()
    header = "Task-plan review SOP (operator-approved 2026-06-11, all projects)"
    assert header in body, (
        "SKILL.md missing the task-plan review SOP header — handed-off "
        "coords lose the pre-promote dual-review gate."
    )
    # Scope to Step 5: the SOP must live at the TASK-PLAN-DOC gate, not
    # drift into a footnote where a promoting coord never re-reads it.
    assert "### Step 5 — TASK-PLAN-DOC" in body
    step5 = body.split("### Step 5 — TASK-PLAN-DOC", 1)[1].split("\n### ", 1)[0]
    assert header in step5, (
        "Task-plan review SOP moved out of the Step 5 — TASK-PLAN-DOC "
        "section — the pre-promote gate must sit where promotion is defined."
    )
    # Flatten hard line wraps so full clauses can be pinned as substrings.
    flat = " ".join(step5.split())
    for clause in (
        # Coord never reviews inline; all review work is dispatched.
        "The coord NEVER reviews inline (no codex exec, no self-review).",
        "All review / debug / investigation / PR-review work is "
        "DISPATCHED to subagents",
        # The dual review is a pre-promote gate, via dispatched subagents.
        "Before promote, every TASK-PLAN doc set gets one dual review via "
        "dispatched subagents",
        "a codex reviewer (codex exec, high reasoning) — design-fidelity, "
        "code-reality, implementability",
        "an independent Claude reviewer — cross-task seams between the "
        "plans, testability, plus the same lenses",
        # Fix/re-review loop until both reviewers are P0/P1-clean.
        "the coord applies doc-level fixes (plan docs are its only allowed "
        "write surface) and re-dispatches confirm reviews until BOTH "
        "return no P0/P1",
        # Clean reviews never bypass the operator promote gate.
        "Reviews-clean never auto-promotes — the operator promote gate "
        "remains separate",
    ):
        assert clause in flat, (
            f"SKILL.md task-plan review SOP missing clause {clause!r} — "
            f"SOP wording drifted from the operator-approved text."
        )
