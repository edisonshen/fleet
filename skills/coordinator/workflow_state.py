"""Atomic-publish writer for the coord progress doc (G5).

Writes ``~/.fleet/projects/<project>/workflow.md`` — the operator-readable
phase log per task that complements the subagent WIP file (which the
subagent owns) and the agent heartbeat JSON (which fleet-guard owns).

The schema is locked at v1:

::

    ---
    schema: v1
    project: <name>
    updated_at: <RFC3339 UTC>
    ---

    # workflow

    ## <slug>
    - phase: <discussing | approved | dispatched | reviewing | pr-open | merged | blocked>
    - updated_at: <RFC3339 UTC>
    - pr_url: <url or empty>
    - note: <optional one-line context>

Atomic publish: tmp-fd in the same directory → ``fsync`` → ``os.replace``
onto the target. A partial write that fleet-guard or a future TUI render
reads is worse than no file at all. Mirrors ``dispatch.write_worker_inbox``
in ``dispatch.py``.

This module is read by ``loop.py`` and the coord agent — it is not a
public API surface for users. The function set is intentionally tiny:

- ``PHASES`` — the locked vocabulary.
- ``WorkflowEntry`` — one row.
- ``render(entries, project, *, now=...)`` — pure formatter.
- ``publish(entries, project, *, fleet_home=None, now=...)`` — atomic write.
- ``workflow_path(project, *, fleet_home=None)`` — resolve the target path.
"""
from __future__ import annotations

import datetime as _dt
import os
import tempfile
from dataclasses import dataclass
from typing import Iterable

# ASCII flow (also in SKILL.md / docs/COORDINATOR-WORKFLOW.md).
#
#  caller (loop.py / coord agent)
#       │
#       ▼
#  publish(entries, project)
#       │
#       ├─ render() ────────► markdown bytes (pure)
#       │
#       ├─ mkstemp(dir=parent)
#       ├─ write + fsync
#       └─ os.replace(tmp, target)        ◄── atomic on POSIX
#

PHASES: tuple[str, ...] = (
    "discussing",
    "approved",
    "dispatched",
    "reviewing",
    "pr-open",
    "merged",
    "blocked",
)

_SLUG_MAX = 64  # mirrors internal/tasks slug guard; keeps headings sane


@dataclass(frozen=True)
class WorkflowEntry:
    """One per-task row in the progress doc.

    ``slug`` is the task slug exactly as it appears in tasks.md.
    ``phase`` MUST be one of ``PHASES``; ``ValueError`` otherwise.
    ``pr_url`` and ``note`` are optional; empty string renders as blank.
    ``updated_at`` is RFC3339 UTC; rendered verbatim. If empty the writer
    uses the publish-time ``now`` so a single tick produces consistent
    timestamps across rows.
    """

    slug: str
    phase: str
    pr_url: str = ""
    note: str = ""
    updated_at: str = ""

    def __post_init__(self) -> None:
        if not self.slug or len(self.slug) > _SLUG_MAX:
            raise ValueError(f"invalid slug {self.slug!r} (1..{_SLUG_MAX} chars)")
        if "\n" in self.slug or "\r" in self.slug:
            raise ValueError(f"slug contains newline: {self.slug!r}")
        if self.phase not in PHASES:
            raise ValueError(
                f"invalid phase {self.phase!r}; expected one of {PHASES}"
            )
        for label, value in (("pr_url", self.pr_url), ("note", self.note)):
            if "\n" in value or "\r" in value:
                raise ValueError(f"{label} contains newline: {value!r}")


def _utc_now() -> str:
    """RFC3339 UTC stamp with ``Z`` suffix and second precision."""
    return _dt.datetime.now(_dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def workflow_path(project: str, *, fleet_home: str | None = None) -> str:
    """Resolve the absolute path of the project's workflow.md.

    Honors ``FLEET_HOME`` env var; falls back to ``~/.fleet``. Mirrors
    the per-project state-dir convention used elsewhere in the skill
    (``coord-state.json``, ``workers/<slug>/state.json``).
    """
    if not project:
        raise ValueError("project must be non-empty")
    home = fleet_home or os.environ.get("FLEET_HOME") or os.path.expanduser("~/.fleet")
    return os.path.join(home, "projects", project, "workflow.md")


def render(
    entries: Iterable[WorkflowEntry],
    project: str,
    *,
    now: str | None = None,
) -> str:
    """Render the progress doc as a UTF-8 string. Pure — no I/O.

    Order is preserved as the caller passes; the coord supplies a stable
    ordering (priority-then-slug) to keep diffs minimal across ticks.
    """
    if not project:
        raise ValueError("project must be non-empty")
    stamp = now or _utc_now()
    lines: list[str] = [
        "---",
        "schema: v1",
        f"project: {project}",
        f"updated_at: {stamp}",
        "---",
        "",
        "# workflow",
        "",
    ]
    seen: set[str] = set()
    for entry in entries:
        if entry.slug in seen:
            raise ValueError(f"duplicate slug {entry.slug!r} in entries")
        seen.add(entry.slug)
        row_stamp = entry.updated_at or stamp
        lines.append(f"## {entry.slug}")
        lines.append(f"- phase: {entry.phase}")
        lines.append(f"- updated_at: {row_stamp}")
        lines.append(f"- pr_url: {entry.pr_url}")
        lines.append(f"- note: {entry.note}")
        lines.append("")
    return "\n".join(lines)


def publish(
    entries: Iterable[WorkflowEntry],
    project: str,
    *,
    fleet_home: str | None = None,
    now: str | None = None,
) -> str:
    """Atomically write the rendered progress doc. Returns the absolute path.

    Atomic = ``mkstemp`` in the same parent dir + ``fsync`` + ``os.replace``.
    On any error the temp file is unlinked before re-raising. Caller
    receives the canonical target path on success so it can log/audit.

    Empty entries are valid — ``workflow.md`` is published with an empty
    body (header + ``# workflow`` only). This matches the "no in-flight
    tasks" steady state.
    """
    target = workflow_path(project, fleet_home=fleet_home)
    parent = os.path.dirname(target)
    os.makedirs(parent, exist_ok=True)
    body = render(entries, project, now=now)
    fd, tmp = tempfile.mkstemp(prefix="workflow.tmp.", dir=parent)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as fh:
            fh.write(body)
            if not body.endswith("\n"):
                fh.write("\n")
            fh.flush()
            os.fsync(fh.fileno())
        os.replace(tmp, target)
    except Exception:
        try:
            os.unlink(tmp)
        except FileNotFoundError:
            pass
        raise
    return target
