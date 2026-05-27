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

    We strip the `projects-` prefix from the fleet-side project name
    (a fleet bookkeeping convention; the github repo is named without
    it) and check the remote URL contains the bare name as a delimited
    token — surrounded by `/`, `:`, `-`, `_`, or string boundary, AND
    the immediately-following char must not be a letter/digit. The
    delimiter check prevents `projects-fleet` from matching a URL
    containing `fleetwood` etc.

    Empty inputs always return False — we never false-positive when
    the data we'd validate against isn't there.
    """
    if not remote_url or not project:
        return False
    bare = project.removeprefix("projects-")
    if not bare:
        return False
    # Trim a trailing `.git` so it doesn't poison the boundary check.
    needle_url = remote_url
    if needle_url.endswith(".git"):
        needle_url = needle_url[: -len(".git")]
    # Look for `<delim>bare<delim>` or `<delim>bare<end>`.
    delims = "/:_-"
    n = len(bare)
    for i in range(len(needle_url) - n + 1):
        if needle_url[i : i + n] != bare:
            continue
        before_ok = i == 0 or needle_url[i - 1] in delims
        after_idx = i + n
        after_ok = after_idx == len(needle_url) or needle_url[after_idx] in delims
        if before_ok and after_ok:
            return True
    return False
