"""coord-config.json read/write helper + project↔origin validation.

Schema (additive — new fields land alongside without breaking the loader):

    {
        "parallelism": <int>,    # _load_parallelism (loop.py)
        "repo":        <str>     # absolute path to project's git checkout
    }

The `repo` field plugs the bug described in issue #175: loop.py used
to compute the worktree base via `cwd = cwd or os.getcwd()`. A coord
running in the wrong shell cwd (cross-project tmux env inheritance)
would silently land `git worktree add` against the wrong repo. With
`repo` recorded at coord-spawn time and read at tick-start, the
coord skill always has an authoritative source-of-truth for its
project's checkout — independent of whatever cwd inherited via tmux.

Spawn (internal/spawn/spawn.go) writes this file on coord-spawn:
  - idempotent: existing non-empty `repo` is preserved
  - merge-safe: existing parallelism + future fields untouched
  - atomic: write to tmp + rename + fsync (no half-written files)

loop.tick (skills/coordinator/loop.py) reads + validates:
  - missing field / file → fall back to caller cwd + warn in
    TickResult.errors
  - present field + remote mismatch → BLOCKED, no dispatch, raised++
  - present field + remote matches → use as cwd for git worktree add
"""
from __future__ import annotations

import json
import os
import subprocess
import tempfile
from pathlib import Path


def read_repo(config_path: Path | str) -> str | None:
    """Return the `repo` field from coord-config.json, or None when:
      - file missing
      - file unreadable / not valid JSON
      - root is not an object
      - `repo` field missing
      - `repo` value is non-string or whitespace-only

    None is the signal for "fall back to legacy cwd-based behavior" —
    so the coord skill can keep running on projects whose
    coord-config.json predates this field.
    """
    path = Path(config_path)
    try:
        with open(path, "r", encoding="utf-8") as fh:
            data = json.load(fh)
    except (FileNotFoundError, json.JSONDecodeError, OSError):
        return None
    if not isinstance(data, dict):
        return None
    raw = data.get("repo")
    if not isinstance(raw, str):
        return None
    stripped = raw.strip()
    if not stripped:
        return None
    return stripped


def write_repo_idempotent(config_path: Path | str, repo: str) -> None:
    """Stamp `repo` into coord-config.json, preserving sibling fields.

    Idempotent rules:
      - File missing → create with {"repo": repo}.
      - File exists, `repo` field missing/empty → merge: add `repo`,
        preserve all other fields.
      - File exists, `repo` non-empty string → leave untouched (operator
        may have set this to a fork checkout or out-of-tree location).

    Atomic: write to a tempfile in the same directory, fsync, rename.
    A crash mid-write never leaves coord-config.json in a partial state.

    Note: this writer is invoked from Spawn (Go side has its own
    equivalent). The Python implementation exists so tests of the
    coord skill can exercise the merge logic deterministically without
    shelling out to the Go binary.
    """
    path = Path(config_path)
    parent = path.parent
    parent.mkdir(parents=True, exist_ok=True)
    # Load existing config (if any).
    data: dict = {}
    try:
        with open(path, "r", encoding="utf-8") as fh:
            existing = json.load(fh)
        if isinstance(existing, dict):
            data = existing
    except (FileNotFoundError, json.JSONDecodeError, OSError):
        # Treat unreadable/malformed config as "no config" — we'll write
        # a clean one. This preserves the operator's intent (record the
        # repo) without inheriting garbage.
        data = {}
    # Idempotency: preserve existing non-empty repo.
    existing_repo = data.get("repo")
    if isinstance(existing_repo, str) and existing_repo.strip():
        return
    data["repo"] = repo
    # Atomic write: tmp + fsync + rename.
    fd, tmp = tempfile.mkstemp(prefix=path.name + ".tmp.", dir=str(parent))
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as fh:
            json.dump(data, fh, indent=2, sort_keys=True)
            fh.flush()
            os.fsync(fh.fileno())
        os.replace(tmp, path)
    except Exception:
        try:
            os.unlink(tmp)
        except FileNotFoundError:
            pass
        raise


def git_remote_origin(repo_path: str) -> str:
    """Return `git -C <repo> remote get-url origin`, stripped. Empty on
    any failure (missing git, missing repo, no `origin` remote)."""
    try:
        proc = subprocess.run(
            ["git", "-C", repo_path, "remote", "get-url", "origin"],
            capture_output=True, text=True, timeout=5,
        )
    except (FileNotFoundError, subprocess.TimeoutExpired, OSError):
        return ""
    if proc.returncode != 0:
        return ""
    return (proc.stdout or "").strip()


def remote_matches_project(remote_url: str, project: str) -> bool:
    """Heuristic: does the `origin` URL look like it belongs to `project`?

    Fleet project tags are `<parent-dir>-<repo-basename>` via
    `internal/projects.TagForPath` (sanitized to the
    state.ValidateProjectName allowlist). So `projects-rainier` came
    from `~/projects/rainier`, `repos-my-project` came from
    `/repos/my-project`, and the repo-basename half of the tag is
    what should equal the github repo name. To recover it: strip the
    FIRST `-`-delimited token (the parent dir), keep the rest, and
    strict-equal against the URL basename (last segment after `/` or
    `:`, with any trailing `.git` stripped). When the tag has no `-`
    at all (e.g., a project registered with just a single-segment
    path), match against the whole tag.

    Examples (all True):
      ('https://github.com/edisonshen/rainier.git', 'projects-rainier')
      ('git@github.com:acme/my-project.git', 'repos-my-project')
      ('https://github.com/edisonshen/fleet', 'projects-fleet')
      ('git@github.com:foo/rainier-app.git', 'projects-rainier-app')

    Examples (all False — the iter-3 false-positive cases):
      ('https://github.com/org/rainier-app.git', 'projects-rainier')
      ('git@github.com:foo/fleet-cli.git', 'projects-fleet')
      ('https://github.com/org/projects-rainier-app.git', 'projects-rainier')

    Empty-origin edge case (review iter-4, codex finding): a
    local-only checkout (`git init` with no `origin` remote) returns
    "" from `git_remote_origin`. The CALLER (loop.tick) treats False
    return here as "refuse dispatch — repo doesn't belong to project,"
    which would break local-only repos registered via
    `fleet project add /path/to/local-repo`. Caller must handle the
    empty-origin path differently (skip validation + warn); this
    function honestly says "I can't validate," and an empty needle
    has never matched a real haystack.
    """
    if not remote_url or not project:
        return False
    # Strip protocol + path prefix down to the URL basename. Two URL
    # shapes are common:
    #   1. https://github.com/org/repo.git  → tail = "repo.git"
    #   2. git@github.com:org/repo.git      → after rsplit("/") = "repo.git"
    # The double rsplit handles SCP-style remotes with no nested path
    # (`git@host:repo.git`) as well.
    tail = remote_url.rsplit("/", 1)[-1]
    tail = tail.rsplit(":", 1)[-1]
    if tail.endswith(".git"):
        tail = tail[: -len(".git")]
    if not tail:
        return False
    # Recover the repo-basename half of the project tag by stripping
    # the first `-` token (the parent-dir half from TagForPath). If
    # there's no `-`, the whole tag IS the basename.
    if "-" in project:
        bare = project.split("-", 1)[1]
    else:
        bare = project
    if not bare:
        return False
    return tail == bare
