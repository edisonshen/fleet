"""Scenario: the plan gate end to end against the BUILT fleet binary.

Operator files a task, the coord links a TASK-PLAN doc, review_slot.py
--plan reviews it (fake claude), the operator approves, promote passes —
and every shortcut on the way is refused. Runs in the `fleet_sandbox`
(isolated FLEET_HOME, registered repo at <home>/repo).
"""
from __future__ import annotations

import json
import os
import subprocess
import sys
from pathlib import Path

REVIEW_SLOT = Path(__file__).resolve().parents[1] / "review_slot.py"
SLUG = "gate-0001"


def _fake_claude(bin_dir: Path, inner: dict) -> None:
    bin_dir.mkdir(parents=True, exist_ok=True)
    out = json.dumps({"type": "result", "subtype": "success", "result": json.dumps(inner)})
    script = bin_dir / "claude"
    script.write_text(f"#!/bin/sh\ncat <<'JSON'\n{out}\nJSON\n", encoding="utf-8")
    script.chmod(0o755)


def test_plan_gate_review_approve_promote(fleet_sandbox, tmp_path: Path) -> None:
    sb = fleet_sandbox
    op_env = {**sb.env, "FLEET_ROLE": ""}
    coord_env = {**sb.env, "FLEET_ROLE": "coord"}

    def fleet(*args: str, env: dict[str, str] = op_env) -> subprocess.CompletedProcess[str]:
        return subprocess.run(
            [sb.bin, *args], cwd=sb.home, env=env, capture_output=True, text=True, check=False
        )

    def review(inner: dict) -> subprocess.CompletedProcess[str]:
        bin_dir = tmp_path / f"bin-{len(list(tmp_path.iterdir()))}"
        _fake_claude(bin_dir, inner)
        env = {**op_env, "PATH": f"{bin_dir}{os.pathsep}{op_env['PATH']}"}
        return subprocess.run(
            [sys.executable, str(REVIEW_SLOT), "--plan", str(doc), "--project", sb.project,
             "--slug", SLUG, "--engine", "claude", "--model", "m"],
            cwd=os.path.join(sb.home, "repo"), env=env, capture_output=True, text=True,
            check=False,
        )

    added = fleet("tasks", "add", "--project", sb.project, "--slug", SLUG,
                  "--priority", "P2", "--spec", "plan gate scenario")
    assert added.returncode == 0, added.stderr

    blocked = fleet("tasks", "promote", "--project", sb.project, SLUG)
    assert blocked.returncode != 0 and "(no-plan)" in blocked.stderr, blocked.stderr

    doc = Path(sb.home) / "repo" / "docs" / f"TASK-PLAN-{SLUG}.md"
    doc.parent.mkdir(parents=True, exist_ok=True)
    brief = doc.parent / f"BRIEF-{SLUG}.md"
    brief.write_text(
        "# Brief\n\n## Relevant files\n\n- a.go\n\n## Current behavior\n\nx\n\n"
        "## Options\n\n### A. one\n\n### B. two\n\n## Open questions\n\n- which?\n\n"
        "## Decision\n\nTBD\n",
        encoding="utf-8",
    )
    doc.write_text(
        f"# Task plan\n\nBrief: `docs/BRIEF-{SLUG}.md`\n\n## Goal\n\ng\n\n## Files\n\n- a.go\n\n"
        "## Steps\n1. build it\n\n## Verification\n\nscenario\n\n## Open questions\n\nNone.\n",
        encoding="utf-8",
    )
    noted = fleet("tasks", "note", "--project", sb.project, SLUG, "--section", "spec",
                  f"Task plan: docs/TASK-PLAN-{SLUG}.md")
    assert noted.returncode == 0, noted.stderr

    blocked = fleet("tasks", "promote", "--project", sb.project, SLUG)
    assert "(unreviewed)" in blocked.stderr, blocked.stderr

    bad = review({"clean": False, "findings": [
        {"severity": "P1", "category": "bug", "section": "Steps", "summary": "names a missing file"},
    ]})
    assert bad.returncode == 1, bad.stderr
    blocked = fleet("tasks", "promote", "--project", sb.project, SLUG)
    assert "(findings)" in blocked.stderr, blocked.stderr
    assert fleet("tasks", "approve", "--project", sb.project, SLUG).returncode != 0

    good = review({"clean": True, "findings": []})
    assert good.returncode == 0, good.stderr

    self_approve = fleet("tasks", "approve", "--project", sb.project, SLUG, env=coord_env)
    assert self_approve.returncode != 0 and "operator-only" in self_approve.stderr

    undecided = fleet("tasks", "approve", "--project", sb.project, SLUG)
    assert undecided.returncode != 0 and "(incomplete)" in undecided.stderr, undecided.stderr
    assert "unanswered open questions" in undecided.stderr, undecided.stderr
    brief.write_text(
        brief.read_text().replace("- which?", "None.").replace("TBD", "Option A."),
        encoding="utf-8",
    )

    approved = fleet("tasks", "approve", "--project", sb.project, SLUG)
    assert approved.returncode == 0, approved.stderr

    doc.write_text(doc.read_text().replace("1. build it", "1. build it\n2. and one more thing"), encoding="utf-8")
    blocked = fleet("tasks", "promote", "--project", sb.project, SLUG)
    assert "(stale)" in blocked.stderr, blocked.stderr

    assert review({"clean": True, "findings": []}).returncode == 0
    assert fleet("tasks", "approve", "--project", sb.project, SLUG).returncode == 0
    promoted = fleet("tasks", "promote", "--project", sb.project, SLUG)
    assert promoted.returncode == 0, promoted.stderr

    # Park it so no later sandbox tick dispatches this scenario's task.
    parked = fleet("tasks", "set", "--project", sb.project, SLUG, "status=done")
    assert parked.returncode == 0, parked.stderr
