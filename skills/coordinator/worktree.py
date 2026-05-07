"""Git worktree management for parallel-dispatch (cap > 1) mode.

The coordinator runs in single-worker mode by default (cap=1) — every
worker uses the project's main repo as its cwd. With cap > 1, dispatched
workers must each get an isolated working tree so they don't trip over
each other's `git checkout` / `git commit`. This module owns the seam:

  1. compute_worktree_path(project, slug, fleet_bin) — resolve where the
     worktree should live. Wraps state.WorktreePath via the Go CLI.
  2. create_worktree(repo, wt_path, branch) — `git -C <repo> worktree
     add <wt_path> -b <branch>`. Creates the parent dir; idempotent on
     "already exists at <path>" (treats it as success so a coord that
     crashed mid-tick can resume).
  3. remove_worktree(repo, wt_path, force) — `git -C <repo> worktree
     remove <wt_path>` (with `--force` when caller asks). Refuses to
     touch the main repo path or paths outside ~/.fleet/projects/.

Single-worker mode never calls into this module. The dispatch / loop
modules choose the seam based on the caller-provided `parallelism`
config; this module is the implementation detail of "I have a slug,
go give me a worktree."

Subprocess-only (matches the rest of the skill). All paths through git
+ the fleet binary — Python writes nothing on its own.
"""
from __future__ import annotations

import os
import subprocess
from dataclasses import dataclass


@dataclass
class WorktreeResult:
    """Outcome of a create/remove call.

    path is populated on success (the worktree directory). error is
    non-empty on failure; caller logs and skips dispatch / cleanup.
    Both are set when the operation partially succeeded — e.g. create
    races a sibling tick, the directory exists but `git worktree add`
    failed: caller can re-use path on next tick if it's safe.
    """

    path: str = ""
    error: str = ""


# Paths outside this prefix are NEVER subject to worktree removal.
# Defense-in-depth: if a caller bug routes a bare repo path or the
# project's main checkout into remove_worktree(), we refuse.
_WORKTREE_PREFIX_TAIL = os.path.join("projects", "")


def compute_worktree_path(
    project: str,
    slug: str,
    *,
    fleet_bin: str = "fleet",
    timeout_s: float = 5.0,
) -> str:
    """Resolve the canonical worktree path for (project, slug).

    Shells out to `fleet workers worktree-path --project <p> <slug>`
    so Go's state.WorktreePath stays the single source of truth for
    project-tree layout. Returns "" on any error; caller treats this
    as "skip dispatching this task this tick" (worktree creation is
    the seam, not the goal — there's no recovery without a path).
    """
    try:
        proc = subprocess.run(
            [fleet_bin, "workers", "worktree-path", "--project", project, slug],
            capture_output=True, text=True, timeout=timeout_s, check=False,
        )
    except (FileNotFoundError, subprocess.TimeoutExpired):
        return ""
    if proc.returncode != 0:
        return ""
    out = (proc.stdout or "").strip()
    if not out:
        return ""
    # Normalize: state.WorktreePath returns a trailing-slash path; the
    # git worktree CLI tolerates either form but we strip for log
    # readability + os.path.relpath semantics.
    return out.rstrip(os.sep)


def create_worktree(
    repo: str,
    wt_path: str,
    branch: str,
    *,
    base: str = "",
    timeout_s: float = 30.0,
) -> WorktreeResult:
    """`git -C <repo> worktree add <wt_path> -b <branch> [<base>]`.

    base defaults to HEAD of <repo>'s current branch (i.e. the operator
    chose the integration branch by checking it out before spawning the
    coord). Pass an explicit ref (e.g. "main") to override.

    Returns WorktreeResult with path=wt_path on success. On "already
    exists" we still return success: the most common reason is a
    previous tick crashed after `git worktree add` but before the
    tasks.md update; resuming on the same path is correct.

    Caller MUST resolve wt_path via compute_worktree_path first — this
    function does no validation of the destination's safety. The
    refuse-to-remove guard in remove_worktree is the safety net for
    accidental main-repo removals.
    """
    if not repo or not wt_path or not branch:
        return WorktreeResult(error="create_worktree: empty repo/wt_path/branch")
    parent = os.path.dirname(wt_path)
    if parent and not os.path.isdir(parent):
        try:
            os.makedirs(parent, exist_ok=True)
        except OSError as exc:
            return WorktreeResult(error=f"create_worktree: mkdir {parent}: {exc}")
    cmd = ["git", "-C", repo, "worktree", "add", wt_path, "-b", branch]
    if base:
        cmd.append(base)
    try:
        proc = subprocess.run(
            cmd, capture_output=True, text=True, timeout=timeout_s, check=False,
        )
    except (FileNotFoundError, subprocess.TimeoutExpired) as exc:
        return WorktreeResult(error=f"create_worktree: {exc}")
    if proc.returncode == 0:
        return WorktreeResult(path=wt_path)
    stderr = (proc.stderr or proc.stdout or "").strip()
    # Idempotent path: a coord crash mid-tick can leave the worktree on
    # disk but tasks.md still ready; the next tick re-runs us. git
    # surfaces this as "already exists" or "is already checked out".
    low = stderr.lower()
    if "already exists" in low or "already checked out" in low:
        return WorktreeResult(path=wt_path)
    return WorktreeResult(error=f"create_worktree: git worktree add: {stderr}")


def remove_worktree(
    repo: str,
    wt_path: str,
    *,
    force: bool = True,
    timeout_s: float = 30.0,
) -> WorktreeResult:
    """`git -C <repo> worktree remove [--force] <wt_path>`.

    Refuses to act when wt_path is empty, equal to repo, or outside the
    ~/.fleet/projects/ tree. Returns success when the worktree is already
    absent (ENOENT-style — mirrors `rm -f`). Caller treats `error` as a
    coord-side bug to log; the worktree may still need manual cleanup.

    `force=True` is the default because reconcile callers reach this
    after the worker reported done OR died — either way we don't care
    about uncommitted changes. Operator-initiated cleanup (a future CLI)
    can pass force=False for a safer path.
    """
    if not _safe_to_remove(repo, wt_path):
        return WorktreeResult(error=f"remove_worktree: refuse unsafe path {wt_path!r}")
    if not os.path.exists(wt_path):
        # Already gone — treat as success. The git worktree metadata in
        # the parent repo may still need pruning; we run that next.
        _prune(repo, timeout_s=timeout_s)
        return WorktreeResult(path=wt_path)
    cmd = ["git", "-C", repo, "worktree", "remove"]
    if force:
        cmd.append("--force")
    cmd.append(wt_path)
    try:
        proc = subprocess.run(
            cmd, capture_output=True, text=True, timeout=timeout_s, check=False,
        )
    except (FileNotFoundError, subprocess.TimeoutExpired) as exc:
        return WorktreeResult(error=f"remove_worktree: {exc}")
    if proc.returncode == 0:
        _prune(repo, timeout_s=timeout_s)
        return WorktreeResult(path=wt_path)
    stderr = (proc.stderr or proc.stdout or "").strip()
    return WorktreeResult(error=f"remove_worktree: git worktree remove: {stderr}")


def _safe_to_remove(repo: str, wt_path: str) -> bool:
    """Return True iff wt_path is safe to delete via `git worktree remove`.

    Three rejection rules:
      1. empty wt_path
      2. wt_path == repo (would delete the main checkout)
      3. wt_path is not under ~/.fleet/projects/<x>/worktrees/ — the only
         place worktree paths legitimately live for this skill.

    We don't bother with realpath because state.WorktreePath emits
    canonical absolute paths and create_worktree rejected anything else.
    """
    if not wt_path:
        return False
    repo_norm = os.path.normpath(repo) if repo else ""
    wt_norm = os.path.normpath(wt_path)
    if repo_norm and wt_norm == repo_norm:
        return False
    # Must contain `<sep>projects<sep>` — i.e. live under ~/.fleet/projects/.
    # We don't anchor on $FLEET_HOME because tests use FLEET_HOME=tmpdir
    # and the prefix tail is invariant either way.
    sep = os.sep
    needle = sep + _WORKTREE_PREFIX_TAIL
    if needle not in wt_norm + sep:
        return False
    # And under a worktrees/ subdir — defends against ~/.fleet/projects/
    # /<name>/tasks.md being mistaken for a worktree root.
    if (sep + "worktrees" + sep) not in wt_norm + sep:
        return False
    return True


def _prune(repo: str, *, timeout_s: float = 10.0) -> None:
    """`git -C <repo> worktree prune`. Best-effort; errors swallowed.

    Called after every remove so stale entries in
    `<repo>/.git/worktrees/<slug>` don't accumulate. A failure here is
    cosmetic — git itself prunes lazily on the next worktree command.
    """
    try:
        subprocess.run(
            ["git", "-C", repo, "worktree", "prune"],
            capture_output=True, text=True, timeout=timeout_s, check=False,
        )
    except (FileNotFoundError, subprocess.TimeoutExpired):
        return
