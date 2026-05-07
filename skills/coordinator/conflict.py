"""File-overlap heuristic for parallel-dispatch conflict detection.

Only matters when the operator runs the coord with cap > 1; default
cap=1 serializes everything. The heuristic is INTENTIONALLY cheap
(PLAN §"Conflict detection") — anything more accurate is v0.3.

Approach: scan each task's Spec/Acceptance/Notes for `path:`/`file:`
mentions plus bare-looking file paths. If any pair shares a file,
declare a conflict and skip the candidate this tick. Default behavior
when no file mentions exist is non-conflict (conservative — same
default as the v0.2 PLAN; coord defers to operator's promotion gate
for true ordering).

Stdlib only (re). False positives are okay — they cost one tick of
serialization. False negatives could clobber files; weigh that when
adding new patterns.
"""
from __future__ import annotations

import re
from typing import Iterable

import parse


# `path:` / `file:` / `files:` / `paths:` markers — operators write
# these in spec bodies to flag the files a task touches. Tolerant of
# whitespace and quoting.
_LABELED_PATH_RE = re.compile(
    r"(?im)^\s*(?:path|file|paths|files)\s*[:=]\s*(.+)$",
)

# Inline path tokens (slash-separated) inside body text. Bounded:
# segment chars are [a-zA-Z0-9_./-] with at least one slash, length > 3,
# extension required. Catches `internal/tasks/tasks.go`, `cmd/fleet/main.go`.
# Excludes URL-like prefixes (https://) by requiring no scheme separator.
_INLINE_PATH_RE = re.compile(
    r"(?<![A-Za-z0-9:_/])"          # no leading word char or scheme `://`
    r"([a-zA-Z][a-zA-Z0-9_-]*"        # first segment starts with letter
    r"(?:/[a-zA-Z0-9_.\-]+)+"          # at least one '/segment' follows
    r"\.[a-zA-Z][a-zA-Z0-9]{0,9})"     # extension
)


def extract_paths(task: parse.Task) -> set[str]:
    """Return the set of file-path mentions in a task's bodies.

    Combines Spec + Acceptance + Notes (all three are operator/worker
    writable). The result is normalized: stripped whitespace, no
    surrounding quotes/backticks, paths-only.
    """
    paths: set[str] = set()
    for body in (task.spec, task.acceptance, task.notes):
        if not body:
            continue
        # Labeled paths: `path: foo/bar.go` (one or more comma/space
        # separated values).
        for m in _LABELED_PATH_RE.finditer(body):
            for tok in _split_labeled(m.group(1)):
                normalized = _normalize_path(tok)
                if normalized:
                    paths.add(normalized)
        # Inline path tokens.
        for m in _INLINE_PATH_RE.finditer(body):
            normalized = _normalize_path(m.group(1))
            if normalized:
                paths.add(normalized)
    return paths


def _split_labeled(value: str) -> Iterable[str]:
    """Split a `path:` line's value into individual path tokens.

    Handles `path: a, b, c` and `paths: a b c` and `files: ["a", "b"]`.
    """
    # Strip JSON-array brackets if present.
    value = value.strip()
    if value.startswith("[") and value.endswith("]"):
        value = value[1:-1]
    # Split on commas first (most common), then any remaining whitespace.
    parts: list[str] = []
    for chunk in value.split(","):
        for sub in chunk.split():
            parts.append(sub)
    return parts


def _normalize_path(p: str) -> str:
    """Strip surrounding quotes/backticks/punctuation; reject if too short."""
    p = p.strip().strip("`'\"")
    p = p.rstrip(".,;)]")
    p = p.lstrip("([")
    if len(p) < 4:
        return ""
    # Reject URLs and obvious non-paths.
    if "://" in p:
        return ""
    return p


def conflicts(a: parse.Task, b: parse.Task) -> bool:
    """Return True if two tasks share at least one mentioned file path.

    Same task slug → not a conflict (it's the same task, not two of
    them). Two tasks with no path mentions → not a conflict
    (conservative: the heuristic defers to operator promotion).
    """
    if a.slug == b.slug:
        return False
    pa = extract_paths(a)
    pb = extract_paths(b)
    if not pa or not pb:
        return False
    return bool(pa & pb)


def has_conflict(candidate: parse.Task, in_flight: Iterable[parse.Task]) -> bool:
    """Loop helper: True if candidate conflicts with ANY in-flight task."""
    for other in in_flight:
        if conflicts(candidate, other):
            return True
    return False
