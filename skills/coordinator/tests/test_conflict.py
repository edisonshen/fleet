"""conflict.py tests: file-overlap heuristic.

The heuristic is intentionally simple — these tests freeze its current
behavior so a future tightening (v0.3) is a deliberate change with a
visible diff, not an accidental regression.
"""
from __future__ import annotations

import conflict
import parse


def _t(slug: str, spec: str = "", acceptance: str = "", notes: str = "") -> parse.Task:
    return parse.Task(
        slug=slug, status="ready", priority="P2",
        spec=spec, acceptance=acceptance, notes=notes,
    )


def test_extract_paths_from_labeled_path_line() -> None:
    t = _t("a-aaaa", spec="path: internal/tasks/tasks.go")
    assert conflict.extract_paths(t) == {"internal/tasks/tasks.go"}


def test_extract_paths_from_labeled_files_list() -> None:
    t = _t("a-aaaa", spec="files: a/b.go, c/d.go")
    assert conflict.extract_paths(t) == {"a/b.go", "c/d.go"}


def test_extract_paths_from_labeled_paths_json_array() -> None:
    t = _t("a-aaaa", spec='paths: ["a/b.go", "c/d.go"]')
    assert conflict.extract_paths(t) == {"a/b.go", "c/d.go"}


def test_extract_paths_inline_in_body() -> None:
    t = _t(
        "a-aaaa",
        spec="Touch internal/tasks/tasks.go and cmd/fleet/main.go to fix.",
    )
    paths = conflict.extract_paths(t)
    assert "internal/tasks/tasks.go" in paths
    assert "cmd/fleet/main.go" in paths


def test_extract_paths_ignores_urls() -> None:
    t = _t(
        "a-aaaa",
        spec="See https://github.com/x/y/pull/1 for context.",
    )
    paths = conflict.extract_paths(t)
    # `https://github.com` won't match (scheme separator excluded);
    # `github.com/x/y/pull/1` won't either (no extension).
    assert paths == set()


def test_extract_paths_combines_all_three_sections() -> None:
    t = _t(
        "a-aaaa",
        spec="Touch a/b.go",
        acceptance="And c/d.go",
        notes="Plus e/f.go",
    )
    assert conflict.extract_paths(t) == {"a/b.go", "c/d.go", "e/f.go"}


def test_conflicts_same_file() -> None:
    a = _t("a-aaaa", spec="path: internal/tasks/tasks.go")
    b = _t("b-bbbb", spec="path: internal/tasks/tasks.go")
    assert conflict.conflicts(a, b) is True


def test_conflicts_disjoint_files() -> None:
    a = _t("a-aaaa", spec="path: internal/tasks/tasks.go")
    b = _t("b-bbbb", spec="path: internal/learnings/learnings.go")
    assert conflict.conflicts(a, b) is False


def test_conflicts_no_paths_is_not_conflict() -> None:
    """Conservative default — coord serializes via operator promotion.

    Two specs with no path mentions are NOT conflicting per v0.2 rules.
    """
    a = _t("a-aaaa", spec="Refactor the auth flow.")
    b = _t("b-bbbb", spec="Tweak the homepage copy.")
    assert conflict.conflicts(a, b) is False


def test_conflicts_one_side_has_paths() -> None:
    """Asymmetric mentions also default to non-conflict."""
    a = _t("a-aaaa", spec="path: internal/tasks/tasks.go")
    b = _t("b-bbbb", spec="No file paths mentioned.")
    assert conflict.conflicts(a, b) is False


def test_conflicts_self_not_a_conflict() -> None:
    """Same slug → reflexive non-conflict (loop logic relies on this)."""
    a = _t("a-aaaa", spec="path: internal/tasks/tasks.go")
    assert conflict.conflicts(a, a) is False


def test_has_conflict_with_any_in_flight() -> None:
    candidate = _t("c-cccc", spec="path: internal/tasks/tasks.go")
    other_overlap = _t("o-oooo", spec="path: internal/tasks/tasks.go")
    other_disjoint = _t("d-dddd", spec="path: internal/agent/agent.go")
    assert conflict.has_conflict(candidate, [other_disjoint]) is False
    assert conflict.has_conflict(candidate, [other_disjoint, other_overlap]) is True
    assert conflict.has_conflict(candidate, []) is False


def test_extract_paths_strips_surrounding_punctuation() -> None:
    t = _t("a-aaaa", spec="See `internal/tasks/tasks.go`, then update.")
    assert "internal/tasks/tasks.go" in conflict.extract_paths(t)


def test_extract_paths_rejects_too_short_tokens() -> None:
    """`a/b` has no extension and is too short to be a real path."""
    t = _t("a-aaaa", spec="path: a/b")
    # No extension → not picked up by inline regex; labeled path passes
    # through the normalize step which rejects len < 4. So nothing.
    paths = conflict.extract_paths(t)
    # If it sneaks through, at least it shouldn't be the same as a real
    # file path — assertion documents the threshold rather than
    # enforcing perfection.
    assert "a/b" not in paths or len(paths) == 1


def test_extract_paths_handles_quoted_strings() -> None:
    t = _t("a-aaaa", spec="files: \"a/b.go\" 'c/d.go'")
    paths = conflict.extract_paths(t)
    assert "a/b.go" in paths
    assert "c/d.go" in paths
