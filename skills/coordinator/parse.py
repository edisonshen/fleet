"""Python mirror of internal/tasks parser/writer for the coordinator skill.

The Go side at internal/tasks/tasks.go owns the authoritative grammar and
is the only writer that mutates tasks.md (the skill goes through `fleet
tasks set/add/...`). This module gives the in-agent skill READ-ONLY
parsing for the same on-disk grammar so the loop can decide what to do
without shelling out to Go for every read.

Byte-equality contract: Read() then Write() of the same file must produce
output byte-identical to the Go parser's render. CI gates this against
every fixture in internal/tasks/testdata/. Keep this file in lockstep
with internal/tasks/tasks.go.

The parser is intentionally hand-rolled (no yaml/regex deps) — same
discipline as fleet-guard (stdlib only). Grammar lives at ENG §3.1.
"""
from __future__ import annotations

import datetime as _dt
from dataclasses import dataclass, field
from typing import Iterable

# Schema version this module produces; matches Go's tasks.SchemaVersion.
SCHEMA_VERSION = 1

VALID_STATUSES = {
    "todo", "ready", "in-progress", "in-review", "done", "blocked",
    "abandoned",
}
VALID_PRIORITIES = {"P0", "P1", "P2", "P3"}

REQUIRED_TASK_BULLETS = (
    "status", "priority", "worker_pid", "worktree", "pr_url", "branch",
    "created", "updated", "depends_on", "spawned_by",
)

# started_at + finished_at are recognized on read but tolerated as missing
# on existing rows (additive change in v0.6.x). The writer ALWAYS emits
# them so re-saved rows pick up the new shape. Old rows that never get
# re-saved stay un-stamped — acceptable per the dispatch brief
# ("Backward compat: parsers tolerate missing started_at/finished_at on
# existing rows; new transitions stamp going forward").
OPTIONAL_LIFECYCLE_BULLETS = ("started_at", "finished_at")

RESERVED_H3 = ("Spec", "Acceptance", "Notes")

# Slug validation mirrors state.ValidateSlug: lowercase letters, digits,
# and the connector chars `-_.`. Reserved names rejected. Done in
# `_validate_slug` (no separate compiled regex — the per-char loop is
# clearer and matches the Go side byte-for-byte).
_RESERVED_SLUGS = {".", "..", "archive"}


class ParseError(Exception):
    """Carries line + col + raw line for actionable parse errors.

    Mirrors Go's internal/tasks.ParseError. The skill surfaces these by
    setting the coord agent's blocked_reason; the operator fixes the
    file and the next tick proceeds.
    """

    def __init__(self, line: int, col: int, raw: str, msg: str) -> None:
        self.line = line
        self.col = col
        self.raw = raw
        self.msg = msg
        super().__init__(f"tasks.md:{line}:{col}: {msg} (line: {raw!r})")


class SchemaTooNewError(Exception):
    """File schema version exceeds SCHEMA_VERSION."""


class DuplicateSlugError(Exception):
    """Two task blocks with the same slug — corrupt file or merge bug."""


class InvalidTaskError(Exception):
    """Validation failure on a Task field (status, priority, slug, ...)."""


@dataclass
class Task:
    """One `## task: <slug>` block.

    Field order intentionally matches the on-disk bullet order so the
    writer iterates the dataclass without a separate emission table.
    Spec/Acceptance/Notes are raw markdown bodies, preserved verbatim
    across round-trip (so worker-edited notes never get clobbered).
    """

    slug: str = ""
    status: str = ""
    priority: str = ""
    worker_pid: int = 0
    worktree: str = ""
    pr_url: str = ""
    branch: str = ""
    created: _dt.datetime | None = None
    updated: _dt.datetime | None = None
    # Lifecycle stamps. None == zero/unset. started_at is sticky (set on
    # the first todo→in-progress flip, never cleared). finished_at is
    # rewritten on every flip into done/abandoned and CLEARED on a flip
    # back out of those states. See cmd/fleet/tasks.go runTasksSet for
    # the authoritative state machine.
    started_at: _dt.datetime | None = None
    finished_at: _dt.datetime | None = None
    depends_on: list[str] = field(default_factory=list)
    spawned_by: str = ""
    spec: str = ""
    acceptance: str = ""
    notes: str = ""


@dataclass
class File:
    """Parsed shape of tasks.md.

    schema is the version read off the frontmatter; missing-frontmatter
    files read as 0 and are lazy-upgraded on the next Write.
    """

    schema: int = 0
    tasks: list[Task] = field(default_factory=list)
    footer: str = ""

    def get(self, slug: str) -> Task | None:
        for t in self.tasks:
            if t.slug == slug:
                return t
        return None


# ---------- public API ----------


def read(path: str) -> File:
    """Read tasks.md from path. ENOENT returns an empty File at the
    current schema (mirrors Go's tasks.Read). Returns SchemaTooNewError
    if the file claims a higher schema version.
    """
    try:
        with open(path, "rb") as fh:
            data = fh.read()
    except FileNotFoundError:
        return File(schema=SCHEMA_VERSION)
    return parse(data)


def write(path: str, f: File) -> None:
    """Atomically publish f to path via .tmp + rename.

    Caller is responsible for serializing concurrent writers via
    state.lock — this module does NOT take the project state-lock.
    The byte format always emits frontmatter at SCHEMA_VERSION
    regardless of what was read (lazy-upgrade for v0 files).
    """
    import os
    import tempfile

    out = render(f).encode("utf-8")
    parent = os.path.dirname(os.path.abspath(path))
    os.makedirs(parent, exist_ok=True)
    fd, tmp = tempfile.mkstemp(prefix=os.path.basename(path) + ".tmp.", dir=parent)
    try:
        with os.fdopen(fd, "wb") as fh:
            fh.write(out)
            fh.flush()
            os.fsync(fh.fileno())
        os.replace(tmp, path)
    except Exception:
        try:
            os.unlink(tmp)
        except FileNotFoundError:
            pass
        raise


def parse(data: bytes | str) -> File:
    """Parse tasks.md bytes/string into a File.

    Normalizes CRLF → LF before line splitting (matches Go behavior).
    Refuses files claiming schema > SCHEMA_VERSION.
    """
    if isinstance(data, bytes):
        text = data.decode("utf-8")
    else:
        text = data
    text = text.replace("\r\n", "\n")
    lines = text.split("\n")

    f = File()
    schema, consumed = _parse_frontmatter(lines)
    f.schema = schema
    if schema > SCHEMA_VERSION:
        raise SchemaTooNewError(
            f"tasks.md schema=v{schema}, max=v{SCHEMA_VERSION}"
        )
    idx = consumed

    footer_start = -1
    while idx < len(lines):
        line = lines[idx]
        if line.startswith("## task: "):
            t, nxt = _parse_task_block(lines, idx)
            for existing in f.tasks:
                if existing.slug == t.slug:
                    raise DuplicateSlugError(t.slug)
            f.tasks.append(t)
            idx = nxt
            continue
        if line.startswith("## "):
            # Non-task H2 → footer. Refuse if any later line opens a
            # task block (operator misordered the layout).
            for j in range(idx + 1, len(lines)):
                if lines[j].startswith("## task: "):
                    raise ParseError(
                        idx + 1, 1, line,
                        "footer header appears before later task block — "
                        "move footer to end of file",
                    )
            footer_start = idx
            break
        idx += 1

    if footer_start >= 0:
        footer = lines[footer_start:]
        # strings.Split injects a trailing empty string when the file
        # ends in '\n'; drop it so we don't re-emit an extra blank.
        if footer and footer[-1] == "":
            footer = footer[:-1]
        if footer:
            f.footer = "\n".join(footer)
    return f


# ---------- parser helpers ----------


def _parse_frontmatter(lines: list[str]) -> tuple[int, int]:
    """Return (schema_int, lines_consumed). 0 schema means no frontmatter."""
    if not lines or lines[0].strip() != "---":
        return 0, 0
    schema = 0
    # Bound to ~32 lines to refuse pathological inputs.
    bound = min(len(lines), 32)
    for i in range(1, bound):
        line = lines[i]
        if line.strip() == "---":
            return schema, i + 1
        k, v, ok = _split_kv(line)
        if not ok:
            if line.strip() == "":
                continue
            raise ParseError(
                i + 1, 1, line,
                "frontmatter expects key: value or ---",
            )
        if k == "schema":
            schema = _parse_schema_value(v, i + 1, line)
    raise ParseError(
        1, 1, lines[0],
        "unterminated frontmatter (missing closing ---)",
    )


def _parse_schema_value(v: str, lineno: int, raw: str) -> int:
    v = v.strip()
    if not v.startswith("v"):
        raise ParseError(lineno, 1, raw, f"schema must be vN, got {v!r}")
    try:
        return int(v[1:])
    except ValueError as exc:
        raise ParseError(lineno, 1, raw, f"schema vN: {exc}") from None


def _parse_task_block(lines: list[str], start: int) -> tuple[Task, int]:
    """Parse one `## task: <slug>` block starting at lines[start].

    Returns (task, idx_of_next_block). Mirrors Go parseTaskBlock —
    same required-bullets, required-H3 sections, fence-aware H3 body.
    """
    t = Task()
    header = lines[start]
    slug = header[len("## task: "):].strip()
    if not slug:
        raise ParseError(start + 1, 1, header, "task header missing slug")
    _validate_slug(slug, start + 1, header)
    t.slug = slug

    idx = start + 1
    while idx < len(lines) and lines[idx].strip() == "":
        idx += 1

    seen_key: dict[str, bool] = {}
    while idx < len(lines):
        line = lines[idx]
        trimmed = line.strip()
        if trimmed == "":
            idx += 1
            break
        if not line.startswith("- "):
            break
        k, v, ok = _split_kv(line[2:])
        if not ok:
            raise ParseError(idx + 1, 1, line, "expected `- key: value`")
        if seen_key.get(k):
            raise ParseError(
                idx + 1, 1, line, f"duplicate task field: {k}",
            )
        seen_key[k] = True
        _set_kv(t, k, v, idx + 1, line)
        idx += 1

    seen_h3: dict[str, bool] = {}
    while idx < len(lines):
        line = lines[idx]
        if line.startswith("## "):
            break
        if line.startswith("### "):
            name = line[len("### "):].strip()
            if name not in RESERVED_H3:
                raise ParseError(
                    idx + 1, 1, line, f"unknown H3 section: {name}",
                )
            body, nxt = _read_section(lines, idx + 1)
            if seen_h3.get(name):
                raise ParseError(
                    idx + 1, 1, line, f"duplicate ### {name} section",
                )
            seen_h3[name] = True
            if name == "Spec":
                t.spec = body
            elif name == "Acceptance":
                t.acceptance = body
            elif name == "Notes":
                t.notes = body
            idx = nxt
            continue
        if line.strip() != "":
            raise ParseError(
                idx + 1, 1, line,
                "unexpected content between sections (allowed: blank "
                "lines, ### Spec/Acceptance/Notes)",
            )
        idx += 1

    if t.status not in VALID_STATUSES:
        raise ParseError(
            start + 1, 1, header, f"invalid status: {t.status}",
        )
    if t.priority not in VALID_PRIORITIES:
        raise ParseError(
            start + 1, 1, header, f"invalid priority: {t.priority}",
        )
    for key in REQUIRED_TASK_BULLETS:
        if not seen_key.get(key):
            raise ParseError(
                start + 1, 1, header,
                f"task missing required field: - {key}",
            )
    for name in RESERVED_H3:
        if not seen_h3.get(name):
            raise ParseError(
                start + 1, 1, header,
                f"task missing required ### {name} section",
            )
    return t, idx


def _read_section(lines: list[str], start: int) -> tuple[str, int]:
    """Read body of an H3 section. Stops at next H2 or reserved H3 or EOF.

    Fence-aware: a column-0 ``` toggles inFence; structural markers
    inside a fence are body content. Mirrors Go's readSection exactly.
    """
    end = start
    in_fence = False
    while end < len(lines):
        line = lines[end]
        if _is_fence_marker(line):
            in_fence = not in_fence
            end += 1
            continue
        if not in_fence:
            if line.startswith("## "):
                break
            if line.startswith("### "):
                name = line[len("### "):].strip()
                if name in RESERVED_H3:
                    break
        end += 1

    body = lines[start:end]
    if body and body[0].strip() == "":
        body = body[1:]
    while body and body[-1].strip() == "":
        body = body[:-1]
    return "\n".join(body), end


def _is_fence_marker(line: str) -> bool:
    """Column-0 ``` opener/closer (with or without language tag).

    Reject ```` (4+ backticks) and indented fences — matches Go's
    isFenceMarker semantics.
    """
    if not line.startswith("```"):
        return False
    rest = line[3:]
    return not rest.startswith("`")


def _split_kv(line: str) -> tuple[str, str, bool]:
    """Split `key: value` on the first colon. Returns (k, v, ok)."""
    i = line.find(":")
    if i < 0:
        return "", "", False
    k = line[:i].strip()
    v = line[i + 1:].strip()
    if not k:
        return "", "", False
    return k, v, True


def _set_kv(t: Task, k: str, v: str, lineno: int, raw: str) -> None:
    if k == "status":
        if v not in VALID_STATUSES:
            raise ParseError(lineno, 1, raw, f"invalid status: {v}")
        t.status = v
    elif k == "priority":
        if v not in VALID_PRIORITIES:
            raise ParseError(lineno, 1, raw, f"invalid priority: {v}")
        t.priority = v
    elif k == "worker_pid":
        if v in ("", "null"):
            t.worker_pid = 0
            return
        try:
            t.worker_pid = int(v)
        except ValueError:
            raise ParseError(
                lineno, 1, raw, f"invalid worker_pid: {v}",
            ) from None
    elif k == "worktree":
        t.worktree = "" if v == "null" else v
    elif k == "pr_url":
        t.pr_url = "" if v == "null" else v
    elif k == "branch":
        t.branch = "" if v == "null" else v
    elif k == "created":
        t.created = _parse_time(v, lineno, raw, "created")
    elif k == "updated":
        t.updated = _parse_time(v, lineno, raw, "updated")
    elif k == "started_at":
        # Optional lifecycle bullet (v0.6.x). Tolerated as missing on
        # old rows; setKV-side dispatch only sees this when the bullet
        # is present in the file.
        t.started_at = _parse_time(v, lineno, raw, "started_at")
    elif k == "finished_at":
        t.finished_at = _parse_time(v, lineno, raw, "finished_at")
    elif k == "depends_on":
        t.depends_on = _parse_deps(v, lineno, raw)
    elif k == "spawned_by":
        t.spawned_by = v
    else:
        raise ParseError(lineno, 1, raw, f"unknown task key: {k}")


def _parse_time(v: str, lineno: int, raw: str, field_name: str) -> _dt.datetime | None:
    if v in ("", "null"):
        return None
    # Go parses strict RFC3339. Python's fromisoformat is permissive in
    # 3.11+, but on 3.9/3.10 it can't handle the trailing 'Z'. Normalize
    # the trailing Z manually for portability across the supported
    # version range.
    s = v
    if s.endswith("Z"):
        s = s[:-1] + "+00:00"
    try:
        return _dt.datetime.fromisoformat(s)
    except ValueError as exc:
        raise ParseError(
            lineno, 1, raw, f"invalid {field_name}: {exc}",
        ) from None


def _parse_deps(v: str, lineno: int, raw: str) -> list[str]:
    v = v.strip()
    if v == "" or v == "[]":
        return []
    if not (v.startswith("[") and v.endswith("]")):
        raise ParseError(
            lineno, 1, raw,
            f"depends_on must be [slug, ...]: {v!r}",
        )
    inner = v[1:-1].strip()
    if inner == "":
        return []
    out: list[str] = []
    for part in inner.split(","):
        part = part.strip()
        if part == "":
            raise ParseError(
                lineno, 1, raw, "depends_on has empty entry",
            )
        if any(c in part for c in ("\"", "'")):
            raise ParseError(
                lineno, 1, raw,
                f"depends_on entry should be bare slug, got {part!r}",
            )
        out.append(part)
    return out


def _validate_slug(slug: str, lineno: int, raw: str) -> None:
    """Mirror state.ValidateSlug.

    Allowed: lowercase letters, digits, hyphen, underscore, period.
    Reserved: empty, ".", "..", "archive". Uppercase rejected (case-
    insensitive filesystems alias variants).
    """
    if not slug:
        raise ParseError(lineno, 1, raw, "slug must not be empty")
    if slug in _RESERVED_SLUGS:
        raise ParseError(lineno, 1, raw, f"slug {slug!r} reserved")
    for c in slug:
        if "a" <= c <= "z" or "0" <= c <= "9" or c in "-_.":
            continue
        if "A" <= c <= "Z":
            raise ParseError(
                lineno, 1, raw,
                f"slug {slug!r} contains uppercase {c!r} (lowercase-only)",
            )
        raise ParseError(
            lineno, 1, raw,
            f"slug {slug!r} contains invalid character {c!r}",
        )


# ---------- writer ----------


def render(f: File) -> str:
    """Emit the canonical bytes for f. Always at SCHEMA_VERSION."""
    parts: list[str] = ["---\n", "schema: v", str(SCHEMA_VERSION), "\n---\n"]
    if f.tasks:
        parts.append("\n")
        for i, t in enumerate(f.tasks):
            parts.append(_render_task(t))
            if i < len(f.tasks) - 1:
                parts.append("\n")
    if f.footer:
        parts.append("\n")
        parts.append(f.footer)
        if not f.footer.endswith("\n"):
            parts.append("\n")
    return "".join(parts)


def _render_task(t: Task) -> str:
    if not t.slug:
        raise InvalidTaskError("empty slug")
    if t.status not in VALID_STATUSES:
        raise InvalidTaskError(f"status={t.status!r}")
    if t.priority not in VALID_PRIORITIES:
        raise InvalidTaskError(f"priority={t.priority!r}")
    parts: list[str] = []
    parts.append(f"## task: {t.slug}\n\n")
    parts.append(f"- status: {t.status}\n")
    parts.append(f"- priority: {t.priority}\n")
    parts.append(f"- worker_pid: {t.worker_pid}\n")
    parts.append(_optional("worktree", t.worktree))
    parts.append(_optional("pr_url", t.pr_url))
    parts.append(_optional("branch", t.branch))
    parts.append(_optional("created", _format_time(t.created)))
    parts.append(_optional("updated", _format_time(t.updated)))
    # Always emit lifecycle bullets (empty for unset) so re-saved old
    # rows pick up the new shape. Matches Go's renderTask order.
    parts.append(_optional("started_at", _format_time(t.started_at)))
    parts.append(_optional("finished_at", _format_time(t.finished_at)))
    parts.append(f"- depends_on: {_format_deps(t.depends_on)}\n")
    parts.append(_optional("spawned_by", t.spawned_by))
    parts.append("\n")
    parts.append(_render_section("Spec", t.spec))
    parts.append(_render_section("Acceptance", t.acceptance))
    parts.append(_render_section("Notes", t.notes))
    return "".join(parts)


def _render_section(name: str, body: str) -> str:
    if body == "":
        return f"### {name}\n\n"
    return f"### {name}\n\n{body}\n\n"


def _optional(key: str, value: str) -> str:
    if value == "":
        return f"- {key}:\n"
    return f"- {key}: {value}\n"


def _format_time(t: _dt.datetime | None) -> str:
    if t is None:
        return ""
    # Match Go: time.UTC().Format(time.RFC3339) → "2006-01-02T15:04:05Z"
    # (no fractional seconds, trailing Z form).
    if t.tzinfo is None:
        utc = t.replace(tzinfo=_dt.timezone.utc)
    else:
        utc = t.astimezone(_dt.timezone.utc)
    return utc.strftime("%Y-%m-%dT%H:%M:%SZ")


def _format_deps(deps: Iterable[str]) -> str:
    items = list(deps)
    if not items:
        return "[]"
    # Defensive sort matches Go's sort.Strings — slugs are unique, this
    # produces stable output even if the caller hands us an unsorted slice.
    items = sorted(items)
    return "[" + ", ".join(items) + "]"
