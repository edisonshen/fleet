"""parse.py round-trip tests against the Go fixture set.

Critical CI gate (ENG §7.2): every fixture in internal/tasks/testdata/
must Read+Write byte-equal in Python AND match Go's parser output.
The Go parser already gates byte-equal round-trip via its own
TestRoundTrip; we extend the contract to Python so the skill's
read view stays in lockstep.

Skipped fixtures: malformed-* (parse error expected), schema-v2.md
(schema-too-new expected), no-frontmatter.md (legacy v0; auto-upgrades
on Write so the round-trip drifts on purpose — exercised separately).
"""
from __future__ import annotations

import os
from pathlib import Path

import pytest

import parse


_REPO_ROOT = Path(__file__).resolve().parents[3]
_FIXTURES = _REPO_ROOT / "internal" / "tasks" / "testdata"

# Fixtures whose Read+Write is byte-equal to source (the "golden"
# round-trip set). Excludes deliberate parse failures and the v0
# legacy file (which intentionally drifts on Write).
_ROUND_TRIP_FIXTURES = [
    "empty.md",
    "single-todo.md",
    "multi-status.md",
    "with-footer.md",
    "deps.md",
    "worker-notes.md",
    "fifty-tasks.md",
]

_PARSE_ERROR_FIXTURES = [
    "malformed-bad-date.md",
    "malformed-bad-status.md",
]


@pytest.mark.parametrize("fixture", _ROUND_TRIP_FIXTURES)
def test_round_trip_byte_equal(fixture: str) -> None:
    """Read each fixture, render it back, and assert byte-equality.

    This is the contract test: it gates Python ↔ Go parser drift.
    A diff here means the skill would corrupt tasks.md on any path
    that involved a Python-side write (none today; defensive).
    """
    path = _FIXTURES / fixture
    src = path.read_bytes()
    f = parse.parse(src)
    rendered = parse.render(f).encode("utf-8")
    assert rendered == src, (
        f"round-trip drift on {fixture}: see diff below\n"
        f"--- source ---\n{src.decode('utf-8', errors='replace')}\n"
        f"--- rendered ---\n{rendered.decode('utf-8', errors='replace')}"
    )


def test_no_frontmatter_lazy_upgrade() -> None:
    """Files without frontmatter (v0) auto-upgrade to v1 on render.

    Mirrors Go behavior: parse() reads schema=0, render() emits the
    current schema in the frontmatter. Round-trip is INTENTIONALLY
    not byte-equal here.
    """
    path = _FIXTURES / "no-frontmatter.md"
    src = path.read_bytes()
    f = parse.parse(src)
    assert f.schema == 0
    rendered = parse.render(f)
    assert rendered.startswith("---\nschema: v1\n---\n")
    # The legacy task body should otherwise be preserved verbatim.
    assert "## task: legacy-1234" in rendered


def test_schema_too_new_raises() -> None:
    src = (_FIXTURES / "schema-v2.md").read_bytes()
    with pytest.raises(parse.SchemaTooNewError):
        parse.parse(src)


@pytest.mark.parametrize("fixture", _PARSE_ERROR_FIXTURES)
def test_malformed_raises_parse_error(fixture: str) -> None:
    src = (_FIXTURES / fixture).read_bytes()
    with pytest.raises(parse.ParseError) as exc_info:
        parse.parse(src)
    # ParseError must carry line + col + raw line context.
    err = exc_info.value
    assert err.line > 0
    assert err.col > 0
    assert err.raw  # raw line text non-empty


# ---------- targeted parser unit tests ----------


def test_duplicate_slug_in_file_raises() -> None:
    """Two `## task:` blocks with the same slug must error out, not
    silently merge. Mirrors Go's read-side duplicate detection."""
    src = b"""---
schema: v1
---

## task: dup-aaaa

- status: todo
- priority: P1
- worker_pid: 0
- worktree:
- pr_url:
- branch:
- created: 2026-05-06T10:00:00Z
- updated: 2026-05-06T10:00:00Z
- depends_on: []
- spawned_by: user

### Spec

s

### Acceptance

a

### Notes


## task: dup-aaaa

- status: todo
- priority: P1
- worker_pid: 0
- worktree:
- pr_url:
- branch:
- created: 2026-05-06T11:00:00Z
- updated: 2026-05-06T11:00:00Z
- depends_on: []
- spawned_by: user

### Spec

s2

### Acceptance

a2

### Notes

"""
    with pytest.raises(parse.DuplicateSlugError):
        parse.parse(src)


def test_get_task_by_slug() -> None:
    src = (_FIXTURES / "multi-status.md").read_bytes()
    f = parse.parse(src)
    t = f.get("alpha-1111")
    assert t is not None
    assert t.status == "todo"
    assert t.priority == "P0"
    assert f.get("nonexistent") is None


def test_crlf_normalized() -> None:
    """CRLF line endings normalize to LF (matches Go behavior)."""
    src = (_FIXTURES / "single-todo.md").read_text().replace("\n", "\r\n")
    f = parse.parse(src.encode("utf-8"))
    assert len(f.tasks) == 1
    assert f.tasks[0].slug == "add-readme-7a3c"


def test_fence_aware_h3_in_body() -> None:
    """Reserved H3 names inside a fenced block stay in the section body.

    Matches Go's readSection fence semantics — operator-pasted markdown
    examples must not be parsed as structural section headers.
    """
    src = b"""---
schema: v1
---

## task: fenced-aaaa

- status: todo
- priority: P1
- worker_pid: 0
- worktree:
- pr_url:
- branch:
- created: 2026-05-06T10:00:00Z
- updated: 2026-05-06T10:00:00Z
- depends_on: []
- spawned_by: user

### Spec

Example layout:

```
### Acceptance
fake header inside fence
```

### Acceptance

real acceptance

### Notes

"""
    f = parse.parse(src)
    assert len(f.tasks) == 1
    t = f.tasks[0]
    assert "### Acceptance" in t.spec  # fenced header lives in spec body
    assert t.acceptance == "real acceptance"


def test_writes_atomic(tmp_path) -> None:
    """write() must produce a file at the target path with no .tmp leak."""
    f = parse.File()
    src = (_FIXTURES / "single-todo.md").read_bytes()
    f = parse.parse(src)
    out = tmp_path / "tasks.md"
    parse.write(str(out), f)
    assert out.read_bytes() == src
    # No leftover .tmp files in the parent directory.
    leftovers = [p.name for p in tmp_path.iterdir() if ".tmp." in p.name]
    assert leftovers == []
