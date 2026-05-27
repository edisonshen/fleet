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

loop.tick (skills/coordinator/loop.py) reads + validates (iter-11
tiered authority):

  1. meta.json::repo_path (operator-set via `fleet project add`) is
     the AUTHORITATIVE source. When present, wins outright; URL
     heuristic bypassed. Custom-named clones / forks / vanity URLs
     all work — operator's explicit registration overrides
     heuristic ambiguity.

  2. coord-config.json::repo (set by Spawn from cwd at spawn-time)
     is the fallback for projects without meta.json. URL heuristic
     is the ONLY safety check here, so mismatch REFUSES dispatch.
     Custom-named operators bypass via `fleet project add` →
     meta.json; #175 cross-project corruption is prevented.

  3. Caller cwd / os.getcwd() — legacy fallback (pre-#175 installs).

  Refuse-with-skip paths (return early after lock release):
    - meta.json::repo_path points at missing dir → meta-repo-missing
    - coord-config.json::repo points at missing dir →
      coord-config-repo-missing
    - coord-config.json::repo + origin URL mismatches heuristic
      (no meta.json) → coord-config-repo-mismatch (includes
      `fleet project add` recovery hint in error message)

  Warn-and-proceed paths (cwd set, dispatch continues):
    - meta.json present + differs from coord-config → use meta.json
      + divergence warning
    - coord-config present + no `origin` remote (local-only repo) →
      soft warning, can't validate
    - No repo at all → fall back to caller cwd + warning
"""
from __future__ import annotations

import json
import os
import subprocess
import tempfile
from pathlib import Path


def read_project_repo_path(project_dir: Path | str) -> str | None:
    """Return `repo_path` from ~/.fleet/projects/<p>/meta.json — the
    operator-authoritative checkout path set by `fleet project add`.

    Returns None when:
      - meta.json doesn't exist (project pre-dates `fleet project add`,
        or coord was spawned without prior `fleet project add`)
      - meta.json unreadable / malformed JSON
      - root is not an object
      - `repo_path` field missing or non-string or whitespace-only

    Callers (loop.tick) use this as the AUTHORITATIVE source for the
    worktree-base cwd when present. Coord-config.json::repo is
    secondary (written by Spawn from the cwd at spawn-time, which
    can be wrong per issue #175). When meta.json::repo_path is
    present AND it differs from coord-config.json::repo, the meta.json
    value wins — the operator explicitly registered that path via
    `fleet project add`.
    """
    path = Path(project_dir) / "meta.json"
    try:
        with open(path, "r", encoding="utf-8") as fh:
            data = json.load(fh)
    except (FileNotFoundError, json.JSONDecodeError, OSError):
        return None
    if not isinstance(data, dict):
        return None
    raw = data.get("repo_path")
    if not isinstance(raw, str):
        return None
    stripped = raw.strip()
    if not stripped:
        return None
    return stripped


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
    # iter-9 (codex P1): always overwrite `repo` with the respawn
    # cwd. See internal/spawn/coord_config.go for the full rationale —
    # short version: respawning IS the operator's signal that the
    # previous value was wrong. Preservation would trap projects
    # without meta.json in the #175 wrong-repo state. Operators
    # wanting a permanent pin use `fleet project add` to set
    # meta.json::repo_path (which wins over coord-config in loop.tick).
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
    `internal/projects.TagForPath`. Both halves can themselves contain
    `-` (sanitized parent `my-org`, sanitized base `my-project`), so
    the tag `my-org-my-project` cannot be unambiguously inverted by
    splitting on `-`. Rather than try, this function checks whether
    the URL basename appears as the **suffix** of the project tag,
    preceded by `-` (the parent/base boundary). If the tag has no
    `-` at all (single-segment registration like just `fleet`),
    match the whole tag against the URL basename.

    Examples (all True):
      ('https://github.com/edisonshen/rainier.git', 'projects-rainier')
      ('git@github.com:acme/my-project.git', 'repos-my-project')
      ('git@github.com:src/my-project.git', 'my-org-my-project')
      ('https://github.com/edisonshen/fleet', 'projects-fleet')
      ('git@github.com:foo/rainier-app.git', 'projects-rainier-app')

    Examples (all False — iter-3 false-positive cases preserved):
      ('https://github.com/org/rainier-app.git', 'projects-rainier')
      ('git@github.com:foo/fleet-cli.git', 'projects-fleet')
      ('https://github.com/org/projects-rainier-app.git', 'projects-rainier')

    Empty-origin edge case: a local-only checkout (`git init` with no
    `origin` remote) returns "" from `git_remote_origin`. The CALLER
    (loop.tick) treats False return here as "refuse dispatch" by
    default, which would break local-only repos. loop.tick branches
    on empty origin BEFORE calling this function — see
    test_tick_local_only_repo_no_origin_warns_but_proceeds.
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
    # If the project tag has no `-`, it was registered from a single-
    # segment path (or sanitized down to one) — match whole-string.
    # Otherwise: URL basename must appear as a hyphen-bounded suffix
    # of the tag. This permits hyphenated parent dirs (`my-org/repo`
    # → tag `my-org-repo`, basename `repo`) AND hyphenated bases
    # (`projects/rainier-app` → tag `projects-rainier-app`, basename
    # `rainier-app`), while still rejecting suffix lookalikes
    # (basename `rainier-app` doesn't end the tag `projects-rainier`).
    if "-" not in project:
        return tail == project
    return project.endswith("-" + tail)
