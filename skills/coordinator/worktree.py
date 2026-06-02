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


def resolve_default_branch(
    repo: str,
    *,
    remote: str = "origin",
    timeout_s: float = 10.0,
) -> str:
    """Resolve <repo>'s default branch name (e.g. "main", "master").

    Strategy (first hit wins):
      1. `git -C <repo> symbolic-ref --short refs/remotes/<remote>/HEAD`
         → e.g. "origin/main"; strip the "<remote>/" prefix. This is the
         remote's published default, recorded at clone time. It is the
         authoritative answer and is what we branch workers off of.
      2. Fallback to "main" when symbolic-ref is unset / errors. A bare
         clone or a repo whose origin/HEAD was never set won't have the
         ref; "main" is the modern default and matches fleet's own repo.

    Returns the branch NAME only (no remote prefix). Never raises — a
    detached / non-git / no-remote repo falls through to "main", which
    the caller then qualifies as "<remote>/main" for the fetch + base.
    """
    if not repo:
        return "main"
    try:
        proc = subprocess.run(
            ["git", "-C", repo, "symbolic-ref", "--short",
             f"refs/remotes/{remote}/HEAD"],
            capture_output=True, text=True, timeout=timeout_s, check=False,
        )
    except (FileNotFoundError, subprocess.TimeoutExpired):
        return "main"
    if proc.returncode != 0:
        return "main"
    out = (proc.stdout or "").strip()
    # symbolic-ref --short returns "<remote>/<branch>"; strip the remote.
    prefix = f"{remote}/"
    if out.startswith(prefix):
        out = out[len(prefix):]
    return out or "main"


def fetch_remote(
    repo: str,
    branch: str,
    *,
    remote: str = "origin",
    timeout_s: float = 60.0,
) -> WorktreeResult:
    """`git -C <repo> fetch <remote> <branch>` — refresh the remote ref.

    Why: a worker worktree branched off the coord's LOCAL HEAD inherits
    whatever the coord last pulled. When a dependency PR merges to
    origin/<branch> AFTER the coord's last fetch, local <branch> is
    stale and the worker's tree is missing the dependency's code. We
    fetch the remote ref immediately before `git worktree add` so the
    worktree can branch off the fresh origin/<branch> tip.

    Returns WorktreeResult with empty error on success. On failure
    (offline, no remote, bad branch) the caller treats it as non-fatal:
    it logs and proceeds to branch off the (possibly stale) origin ref
    that already exists locally — better a stale base than no dispatch.
    A missing remote is the common non-git-server case; we don't want
    to wedge all of cap on a transient network blip.

    Best-effort by contract. Empty repo/branch is a refusing no-op so a
    caller bug can't shell `git fetch origin ''`.
    """
    if not repo or not branch:
        return WorktreeResult(error="fetch_remote: empty repo/branch")
    try:
        proc = subprocess.run(
            ["git", "-C", repo, "fetch", remote, branch],
            capture_output=True, text=True, timeout=timeout_s, check=False,
        )
    except (FileNotFoundError, subprocess.TimeoutExpired) as exc:
        return WorktreeResult(error=f"fetch_remote: {exc}")
    if proc.returncode == 0:
        return WorktreeResult(path=repo)
    stderr = (proc.stderr or proc.stdout or "").strip()
    return WorktreeResult(error=f"fetch_remote: git fetch {remote} {branch}: {stderr}")


def ref_exists(
    repo: str,
    ref: str,
    *,
    timeout_s: float = 10.0,
) -> bool:
    """Return True iff <ref> resolves to an object in <repo>.

    `git -C <repo> rev-parse --verify --quiet <ref>^{commit}` — exit 0
    means the ref names a commit we can branch from. Used to GUARD the
    base passed to `git worktree add`: a non-git project, a repo with no
    `origin` remote, or one whose `origin/<default>` ref was never
    fetched (offline first run) has NO `origin/<default>` object. Passing
    that as base makes `git worktree add` fatal with `invalid reference`,
    which would skip the dispatch entirely (codex [P2]). The caller falls
    back to the local-HEAD base (base="") when this returns False, so
    cap>1 dispatch still proceeds — just off local HEAD, the pre-fix
    behavior, rather than wedging the task forever.

    Never raises — a missing git binary / timeout / empty input is a
    conservative False (caller falls back to local HEAD).
    """
    if not repo or not ref:
        return False
    try:
        proc = subprocess.run(
            ["git", "-C", repo, "rev-parse", "--verify", "--quiet",
             f"{ref}^{{commit}}"],
            capture_output=True, text=True, timeout=timeout_s, check=False,
        )
    except (FileNotFoundError, subprocess.TimeoutExpired):
        return False
    return proc.returncode == 0


def current_branch(
    repo: str,
    *,
    timeout_s: float = 10.0,
) -> str:
    """Return <repo>'s currently checked-out branch name, or "" if the
    repo is in detached-HEAD state / not a git repo.

    `git -C <repo> symbolic-ref --short -q HEAD` prints the branch name
    on a normal checkout and exits non-zero (empty) when HEAD is
    detached. The coord's checkout branch is operator-authoritative: the
    operator chooses the integration branch by checking it out before
    spawning the coord, so workers must branch off ITS upstream, not the
    remote default. Never raises.
    """
    if not repo:
        return ""
    try:
        proc = subprocess.run(
            ["git", "-C", repo, "symbolic-ref", "--short", "-q", "HEAD"],
            capture_output=True, text=True, timeout=timeout_s, check=False,
        )
    except (FileNotFoundError, subprocess.TimeoutExpired):
        return ""
    if proc.returncode != 0:
        return ""
    return (proc.stdout or "").strip()


@dataclass
class WorkerBase:
    """Resolved base for a worker worktree + the branch to fetch first.

    base is what `git worktree add` branches off (passed as create_worktree's
    `base=`); "" means local HEAD. fetch_branch is the branch name to
    `git fetch origin <fetch_branch>` before resolving base ("" = skip
    fetch, e.g. detached HEAD with no resolvable default).
    """

    base: str = ""
    fetch_branch: str = ""


def resolve_worker_base(
    repo: str,
    *,
    remote: str = "origin",
    timeout_s: float = 10.0,
) -> WorkerBase:
    """Pick the base ref a worker worktree should branch from.

    The goal is two-fold and the two pulls are in tension:
      (a) workers must see commits a dependency PR JUST merged to the
          remote (the stale-local-HEAD bug this PR fixes), AND
      (b) workers must honor the coord's deliberately checked-out branch
          (operator may sit on a stacked/integration branch ahead of the
          remote default — branching off origin/<default> would DROP that
          branch's commits; codex [P1]).

    Resolution (first hit wins):
      1. The coord is on branch <cb> with a remote-tracking ref
         origin/<cb>. → fetch <cb>, base = "origin/<cb>". Fresh upstream
         of the operator's OWN branch: satisfies (a) AND (b).
      2. The coord is on branch <cb> with NO origin/<cb> (a purely-local
         stacked branch the operator built and hasn't pushed). → base =
         "" (local HEAD). Honors (b); there's no remote to refresh from.
         fetch_branch stays "" — fetching <cb> would fail (no upstream).
      3. Detached HEAD / no current branch → fall back to the remote
         default branch (resolve_default_branch), fetch it, base =
         "origin/<default>" if it exists else "" (local HEAD).

    Pure resolution: this does NOT fetch or create anything. The caller
    fetches WorkerBase.fetch_branch (best-effort), then verifies/creates.

    ASCII — the decision the caller drives off this:

        coord checkout HEAD ──► current_branch?
              │ detached                  │ branch <cb>
              ▼                            ▼
        default branch <d>         origin/<cb> exists?
        origin/<d> exists?          │ yes        │ no
          y → origin/<d>            ▼            ▼
          n → "" (HEAD)        origin/<cb>     "" (local HEAD)
                               (fetch <cb>)    (no fetch)
    """
    cb = current_branch(repo, timeout_s=timeout_s)
    if cb:
        remote_ref = f"{remote}/{cb}"
        if ref_exists(repo, remote_ref, timeout_s=timeout_s):
            return WorkerBase(base=remote_ref, fetch_branch=cb)
        # On a local-only branch (no upstream): branch off local HEAD so
        # the operator's commits aren't dropped. No fetch — there's no
        # origin/<cb> to refresh, and fetching would just error.
        return WorkerBase(base="", fetch_branch="")
    # Detached HEAD: best we can do is the remote default branch.
    default_branch = resolve_default_branch(repo, remote=remote, timeout_s=timeout_s)
    return WorkerBase(base=f"{remote}/{default_branch}", fetch_branch=default_branch)


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
    # First pass: try to create both the worktree dir AND the branch.
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
    # Branch-already-exists retry path (codex iter-2 [P1]): on TASK_DONE_PR,
    # WORKER_FAILED, CI red, or rebase-needed, reconcile/sentinel cleanup
    # removes the worktree but keeps the branch alive (the open PR points
    # at it). The next dispatch finds the branch still on disk; using
    # `-b` would fatal "branch already exists" and the task would be
    # un-redispatchable forever. Re-run without `-b` so the existing
    # branch is checked out into the new worktree.
    if _is_branch_already_exists(stderr, branch):
        retry = ["git", "-C", repo, "worktree", "add", wt_path, branch]
        try:
            proc2 = subprocess.run(
                retry, capture_output=True, text=True, timeout=timeout_s, check=False,
            )
        except (FileNotFoundError, subprocess.TimeoutExpired) as exc:
            return WorktreeResult(error=f"create_worktree retry: {exc}")
        if proc2.returncode == 0:
            return WorktreeResult(path=wt_path)
        stderr2 = (proc2.stderr or proc2.stdout or "").strip()
        # Retry path can also hit "already checked out" if the branch is
        # checked out elsewhere — that's a real conflict, surface it.
        return WorktreeResult(
            error=f"create_worktree: branch {branch} reuse failed: {stderr2}",
        )
    # Worktree-path collision path: git already has wt_path registered as
    # a worktree (most often because a previous tick crashed mid-add but
    # after the wt-dir landed on disk). Verify wt_path is a REGISTERED
    # worktree before returning success — a stale non-empty directory at
    # the same path also produces "already exists" but is NOT a real
    # checkout, and handing it to the worker would crash the first git
    # step (codex iter-2 [P2]). The verify call goes through git, not
    # os.path checks, so it can't be fooled by a directory drop-in.
    if _is_worktree_path_collision(stderr, wt_path):
        if _is_registered_worktree(repo, wt_path, timeout_s=timeout_s):
            return WorktreeResult(path=wt_path)
        return WorktreeResult(
            error=(
                f"create_worktree: {wt_path} exists on disk but is not a "
                f"registered git worktree; remove it and retry"
            ),
        )
    return WorktreeResult(error=f"create_worktree: git worktree add: {stderr}")


def _is_worktree_path_collision(stderr: str, wt_path: str) -> bool:
    """Return True iff git's stderr reports that the worktree DIRECTORY
    at wt_path already exists.

    Two phrasings:
      "fatal: '<wt_path>' already exists"
      "fatal: '<wt_path>' is already checked out at <ref>"
    Anchored on wt_path (lowercase compare) so the branch-collision
    phrasing
      "fatal: A branch named '<branch>' already exists."
    can't be misclassified as idempotent.
    """
    if not stderr or not wt_path:
        return False
    low = stderr.lower()
    wt_low = wt_path.lower()
    if wt_low not in low:
        return False
    return ("already exists" in low) or ("already checked out" in low)


def _is_branch_already_exists(stderr: str, branch: str) -> bool:
    """Return True iff git's stderr reports that the branch already
    exists (i.e. `git worktree add -b <branch>` is being told to make
    a branch that's already on disk).

    Phrasing:
      "fatal: A branch named '<branch>' already exists."
    Anchored on the branch name so a worktree-path collision (which
    has its own retry semantics) can't be misclassified.
    """
    if not stderr or not branch:
        return False
    low = stderr.lower()
    # Match the phrase + the branch name (case-insensitive). Don't
    # anchor on quotes — git renders the branch name with single quotes
    # but downstream localization could change that. The phrase
    # "branch named" + the literal branch text is enough.
    if "branch named" not in low:
        return False
    return branch.lower() in low


def _is_registered_worktree(
    repo: str, wt_path: str, *, timeout_s: float = 10.0,
) -> bool:
    """Return True iff git lists wt_path among <repo>'s registered
    worktrees.

    Runs `git -C <repo> worktree list --porcelain` and parses the
    `worktree <abs-path>` lines. Comparison uses os.path.realpath on
    both sides because git emits the canonical absolute path even
    when the caller passed a symlinked or trailing-slash variant.

    Treats subprocess errors as "not registered" — caller surfaces a
    helpful error so the operator can clean the path manually.
    """
    if not repo or not wt_path:
        return False
    try:
        proc = subprocess.run(
            ["git", "-C", repo, "worktree", "list", "--porcelain"],
            capture_output=True, text=True, timeout=timeout_s, check=False,
        )
    except (FileNotFoundError, subprocess.TimeoutExpired):
        return False
    if proc.returncode != 0:
        return False
    target = os.path.realpath(wt_path)
    for line in (proc.stdout or "").splitlines():
        if not line.startswith("worktree "):
            continue
        listed = line[len("worktree "):].strip()
        if not listed:
            continue
        try:
            if os.path.realpath(listed) == target:
                return True
        except OSError:
            continue
    return False


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


def prune_worktrees(repo: str, *, timeout_s: float = 10.0) -> None:
    """Public tick-start helper: best-effort `git -C <repo> worktree prune`.

    Called once per coord tick (after the NB-flock, before parse +
    reconcile + dispatch) to clean up the registry entries of any
    worktree directory that no longer exists on disk. The classic
    failure mode this guards: a coord crashes mid-tick after `git
    worktree add` succeeded but before the tasks.md mutation; a
    second run of the same dispatch path then trips on git's "already
    exists" record. `git worktree prune` is idempotent and only drops
    records pointing at missing directories — it never touches a live
    checkout.

    Differs from the internal `_prune` only in error handling: a
    subprocess error here is logged to stderr (so an operator
    debugging coord behavior sees the failure) but never bubbles up
    so the tick can proceed. Orphan cleanup is best-effort; if it
    fails the dispatch path may still hit "already exists" later and
    surface the underlying problem.

    Empty repo is a no-op (no Go-CLI failure mode that would surface
    a blank repo, but defending the call site is cheaper than tracing
    one if it ever shows up).
    """
    if not repo:
        return
    try:
        proc = subprocess.run(
            ["git", "-C", repo, "worktree", "prune"],
            capture_output=True, text=True, timeout=timeout_s, check=False,
        )
    except (FileNotFoundError, subprocess.TimeoutExpired) as exc:
        import sys
        print(f"coord: worktree prune failed for {repo}: {exc}", file=sys.stderr)
        return
    if proc.returncode != 0:
        import sys
        msg = (proc.stderr or proc.stdout or "").strip() or f"exit {proc.returncode}"
        print(f"coord: worktree prune failed for {repo}: {msg}", file=sys.stderr)
