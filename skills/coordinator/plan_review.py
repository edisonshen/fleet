"""Plan-review records: the persisted result of reviewing a TASK-PLAN doc.

`review_slot.py --plan <doc>` reviews a Markdown task plan (not a diff) and
writes one record per task to

    $FLEET_HOME/projects/<project>/plan-reviews/<slug>.json

keyed by the sha256 of the exact doc bytes that were reviewed, so a reader
can tell whether the doc changed after review (`doc_sha256` vs the doc on
disk). See docs/DESIGN-plan-review-gate.md.
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


INSTRUCTION_FILES = ("REVIEW.md", "AGENTS.md")
# Devin Review treats files under these dirs as belonging to the parent.
_META_DIRS = ("", ".agents", ".devin", ".cursor", ".github")
_INSTRUCTION_MAX_BYTES = 16_000
_PATH_TOKEN_RE = re.compile(r"[A-Za-z0-9_.][A-Za-z0-9_./-]*")


def repo_root_for(path: str | os.PathLike[str]) -> Path:
    """Nearest ancestor of ``path`` holding ``.git``; its own dir otherwise."""
    start = Path(path).resolve().parent
    for d in (start, *start.parents):
        if (d / ".git").exists():
            return d
    return start


def referenced_dirs(plan_text: str, repo: Path) -> set[Path]:
    """Repo dirs the plan touches: every token naming an existing repo path."""
    repo = repo.resolve()
    out: set[Path] = set()
    for tok in _PATH_TOKEN_RE.findall(plan_text):
        tok = tok.rstrip(".")
        if "/" not in tok or tok.startswith("/"):
            continue
        cand = (repo / tok).resolve()
        if not cand.is_relative_to(repo) or not cand.exists():
            continue
        out.add(cand if cand.is_dir() else cand.parent)
    return out


def scoped_instruction_files(plan_path: str | os.PathLike[str], repo: Path | None = None) -> list[Path]:
    """REVIEW.md / AGENTS.md files whose directory scope covers the plan.

    A file applies to everything under its directory (``.agents/``,
    ``.devin/``, ``.cursor/``, ``.github/`` count as the parent), so the
    plan picks up the repo root, the plan's own dir, and every ancestor of
    a path the plan references. Root-most first, so deeper (more specific)
    instructions come last.
    """
    plan = Path(plan_path).resolve()
    repo = (repo or repo_root_for(plan)).resolve()
    dirs = {repo}
    seeds = referenced_dirs(plan.read_text(encoding="utf-8", errors="replace"), repo)
    if plan.is_relative_to(repo):
        seeds.add(plan.parent)
    for d in seeds:
        while d.is_relative_to(repo):
            dirs.add(d)
            if d == repo:
                break
            d = d.parent
    found: list[Path] = []
    for d in sorted(dirs, key=lambda x: (len(x.parts), str(x))):
        for meta in _META_DIRS:
            for name in INSTRUCTION_FILES:
                f = d / meta / name if meta else d / name
                if f.is_file():
                    found.append(f)
    return found


def instructions_block(plan_path: str | os.PathLike[str], repo: Path | None = None) -> str:
    """Prompt section carrying the scoped instruction files' contents."""
    plan = Path(plan_path).resolve()
    repo = (repo or repo_root_for(plan)).resolve()
    files = scoped_instruction_files(plan, repo)
    if not files:
        return ""
    parts = [
        "Repository review instructions (REVIEW.md / AGENTS.md, scoped by "
        "directory like Devin Review; later files are more specific and win "
        "on conflict). Apply them when judging the plan:",
    ]
    for f in files:
        scope = f.parent.parent if f.parent.name in _META_DIRS[1:] else f.parent
        rel_scope = scope.relative_to(repo).as_posix()
        rel_scope = "repo root" if rel_scope == "." else rel_scope + "/"
        body = f.read_bytes()[:_INSTRUCTION_MAX_BYTES].decode("utf-8", errors="replace")
        parts += ["", f"--- {f.relative_to(repo).as_posix()} (applies to {rel_scope}) ---", body.rstrip()]
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
