"""Plan-review records: the persisted result of reviewing a TASK-PLAN doc.

`review_slot.py --plan <doc>` reviews a Markdown task plan (not a diff) and
writes one record per task to

    $FLEET_HOME/projects/<project>/plan-reviews/<slug>.json

keyed by the sha256 of the exact doc bytes that were reviewed. The Go side
(internal/planreview) reads the same file: `fleet tasks approve` and the
`fleet tasks promote` gate compare `doc_sha256` against the doc on disk, so
any edit after review makes the record stale. See
docs/DESIGN-plan-review-gate.md.
"""
from __future__ import annotations

import hashlib
import json
import os
import re
import tempfile
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

RECORD_SCHEMA = 1
CATEGORIES = ("bug", "flag", "security")
_NAME_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]*$")


def fleet_home(override: str | None = None) -> Path:
    if override:
        return Path(override)
    env = os.environ.get("FLEET_HOME")
    if env:
        return Path(env)
    return Path(os.path.expanduser("~/.fleet"))


def _check_name(kind: str, value: str) -> str:
    if not _NAME_RE.match(value) or value in (".", ".."):
        raise ValueError(f"invalid {kind} {value!r}")
    return value


def record_path(project: str, slug: str, home: str | None = None) -> Path:
    return (
        fleet_home(home)
        / "projects"
        / _check_name("project", project)
        / "plan-reviews"
        / f"{_check_name('slug', slug)}.json"
    )


def doc_sha256(path: str | os.PathLike[str]) -> str:
    with open(path, "rb") as fh:
        return hashlib.sha256(fh.read()).hexdigest()


def build_plan_schema() -> dict[str, Any]:
    """Closed schema (codex rejects open/optional properties)."""
    return {
        "type": "object",
        "properties": {
            "clean": {"type": "boolean"},
            "findings": {
                "type": "array",
                "items": {
                    "type": "object",
                    "properties": {
                        "severity": {"type": "string", "enum": ["P0", "P1", "P2", "P3"]},
                        "category": {"type": "string", "enum": list(CATEGORIES)},
                        "section": {"type": "string"},
                        "summary": {"type": "string"},
                    },
                    "required": ["severity", "category", "section", "summary"],
                    "additionalProperties": False,
                },
            },
        },
        "required": ["clean", "findings"],
        "additionalProperties": False,
    }


def plan_review_prompt(doc_path: str, sha: str, instructions: str = "") -> str:
    parts = [
        f"Review the task plan document at {doc_path} (sha256 {sha}). This is a "
        "PLAN review, not a code review: read the document in full, then read "
        "the code it references to check it against reality. Do not modify any "
        "file.",
        "",
        "Check, in order:",
        "1. Design fidelity: the plan matches the parent DESIGN doc it links "
        "(if any) and does not silently drop or widen scope.",
        "2. Code reality: files, functions, flags, and behaviours the plan "
        "names exist and work the way the plan claims.",
        "3. Implementability: every step is concrete enough for a worker to "
        "build without guessing; acceptance criteria are testable.",
        "4. Scenario contract: each acceptance scenario names a test level the "
        "project's sandbox tier allows (`fleet standards show --merged`).",
        "5. Security and data safety: secrets, auth, destructive operations, "
        "injection, races, irreversible migrations.",
        "",
        "Classify every finding with a category: \"bug\" (the plan is wrong "
        "and would produce broken code), \"flag\" (ambiguity, missing case, or "
        "risk the operator should decide on), or \"security\". Severity: P0 = "
        "plan cannot work or causes damage, P1 = must fix before "
        "implementation, P2 = should fix, P3 = nit. `section` is the plan "
        "heading the finding is about. clean=true only when there are no "
        "findings.",
    ]
    if instructions:
        parts += ["", instructions]
    parts += [
        "",
        "Return ONLY structured JSON matching the provided schema "
        '{"clean": bool, "findings": [{"severity", "category", "section", '
        '"summary"}]} — no prose, markdown, or code fences.',
    ]
    return "\n".join(parts)


def utc_now() -> str:
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def build_record(
    *,
    project: str,
    slug: str,
    doc: str,
    sha: str,
    exit_code: int,
    slots: dict[str, dict[str, Any]],
) -> dict[str, Any]:
    findings: list[dict[str, Any]] = []
    for name, slot in slots.items():
        for finding in slot.get("findings", []):
            findings.append({**finding, "slot": name})
    return {
        "schema": RECORD_SCHEMA,
        "project": project,
        "slug": slug,
        "doc": doc,
        "doc_sha256": sha,
        "reviewed_at": utc_now(),
        "exit": exit_code,
        "clean": exit_code == 0,
        "slots": {
            name: {k: v for k, v in slot.items() if k != "findings"}
            for name, slot in slots.items()
        },
        "findings": findings,
    }


def write_record(record: dict[str, Any], home: str | None = None) -> Path:
    path = record_path(record["project"], record["slug"], home)
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, tmp = tempfile.mkstemp(prefix=f".{path.name}.", dir=path.parent)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as fh:
            json.dump(record, fh, indent=2, sort_keys=True)
            fh.write("\n")
            fh.flush()
            os.fsync(fh.fileno())
        os.replace(tmp, path)
    except BaseException:
        try:
            os.unlink(tmp)
        except FileNotFoundError:
            pass
        raise
    return path
