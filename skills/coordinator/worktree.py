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


# Removal outcomes (DESIGN-coord-worktree-lifecycle §4.1). remove_worktree
# returns exactly one of these in WorktreeResult.outcome. The caller
# branches on it: REMOVED/NOOP run the worker-dir delete + worktree= clear;
# DIRTY_PARKED/ERROR leave a tree on disk so the worker dir is KEPT
# (recovery context) and a caller-specific park status is applied.
OUTCOME_REMOVED = "removed"            # tree was clean → git removed it
OUTCOME_DIRTY_PARKED = "dirty-parked"  # `git status --porcelain` non-empty
OUTCOME_NOOP = "noop"                  # nothing to remove (already gone)
OUTCOME_ERROR = "error"                # unexpected git/path failure


@dataclass
class WorktreeResult:
    """Outcome of a create/remove call.

    path is populated on success (the worktree directory). error is
    non-empty on failure; caller logs and skips dispatch / cleanup.
    Both are set when the operation partially succeeded — e.g. create
    races a sibling tick, the directory exists but `git worktree add`
    failed: caller can re-use path on next tick if it's safe.

    outcome is populated by remove_worktree only — one of the OUTCOME_*
    constants above. It is "" for create-path results (callers there
    branch on error/path only). A non-empty outcome is the authoritative
    "what happened" signal the §4.1/§4.2 reaping consumer branches on;
    `error` is still set on OUTCOME_ERROR / OUTCOME_DIRTY_PARKED so the
    existing stderr-surface path keeps working unchanged.
    """

    path: str = ""
    error: str = ""
    outcome: str = ""


# Default subprocess timeout for `git worktree add` (seconds). The add
# is I/O-bound in tracked-file count, not CPU: a ~300K-file monorepo
# checkout routinely runs 60-90s+, so the old 30s killed every dispatch
# mid-checkout and left a half-written tree behind. Callers override per
# project via coord-config.json's `worktree_timeout_s`.
CREATE_TIMEOUT_S = 300.0

# Timeout for the read-only `git worktree list --porcelain` verify. It is
# independent of the (possibly minutes-long) create timeout — listing the
# registry never scales with repo size.
_VERIFY_TIMEOUT_S = 10.0

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


def is_ancestor(
    repo: str,
    ancestor: str,
    descendant: str,
    *,
    timeout_s: float = 10.0,
) -> bool:
    """Return True iff <ancestor> is an ancestor of (or equal to)
    <descendant> in <repo>'s commit graph.

    `git -C <repo> merge-base --is-ancestor <ancestor> <descendant>`
    (exit 0 = yes, 1 = no). Used to decide whether branching a worker off
    a fresh remote ref would DROP the coord's local commits: if local
    HEAD is an ancestor of the fetched upstream, the upstream is a strict
    superset (local adds nothing) and is safe to use as the base; if NOT
    (local ahead/diverged), we must branch off local HEAD to preserve the
    operator's un-pushed commits (codex [P1]). Never raises — any
    error / missing ref returns False (conservative: keep local HEAD).
    """
    if not repo or not ancestor or not descendant:
        return False
    try:
        proc = subprocess.run(
            ["git", "-C", repo, "merge-base", "--is-ancestor",
             ancestor, descendant],
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


def upstream_ref(
    repo: str,
    branch: str = "HEAD",
    *,
    timeout_s: float = 10.0,
) -> str:
    """Return <branch>'s CONFIGURED upstream remote-tracking ref, e.g.
    "origin/main", or "" when no upstream is set / on error.

    `git -C <repo> rev-parse --abbrev-ref --symbolic-full-name <branch>@{u}`.
    Unlike assuming "<remote>/<branch>", this reads the branch's ACTUAL
    `branch.<name>.merge`/`.remote` config — so a local branch tracking a
    DIFFERENTLY-NAMED upstream (`git checkout -b integration --track
    origin/main`) resolves to the real "origin/main", not the bogus
    "origin/integration" (codex [P2]). Never raises.
    """
    if not repo or not branch:
        return ""
    try:
        proc = subprocess.run(
            ["git", "-C", repo, "rev-parse", "--abbrev-ref",
             "--symbolic-full-name", f"{branch}@{{upstream}}"],
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
    `base=`); "" means local HEAD. fetch_remote/fetch_branch name the
    `git fetch <fetch_remote> <fetch_branch>` to run before resolving base
    (both "" = skip fetch, e.g. a local-only branch with no upstream).
    """

    base: str = ""
    fetch_remote: str = ""
    fetch_branch: str = ""


def _split_remote_ref(ref: str, default_remote: str = "origin") -> tuple[str, str]:
    """Split "origin/main" → ("origin", "main"); "origin/feat/x" →
    ("origin", "feat/x"). A ref with no "/" is treated as a bare branch
    on default_remote. Returns ("", "") for empty input."""
    if not ref:
        return ("", "")
    parts = ref.split("/", 1)
    if len(parts) == 2:
        return (parts[0], parts[1])
    return (default_remote, parts[0])


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
      1. The coord is on branch <cb> with a CONFIGURED upstream
         <remote>/<up> (read via `@{upstream}`, NOT assumed to be
         origin/<cb> — codex [P2]: a branch can track a differently-named
         upstream). → fetch <up> from <remote>, base = "<remote>/<up>".
         Fresh upstream of the operator's OWN branch: satisfies (a)+(b).
      2. The coord is on branch <cb> with NO upstream (a purely-local
         stacked branch the operator built and hasn't pushed). → base =
         "" (local HEAD). Honors (b); there's no remote to refresh from.
      3. Detached HEAD / no current branch → fall back to the remote
         default branch (resolve_default_branch), fetch it, base =
         "<remote>/<default>" (caller verifies + falls back to HEAD).

    Pure resolution: this does NOT fetch or create anything. The caller
    fetches WorkerBase.fetch_{remote,branch} (best-effort), then
    verifies WorkerBase.base via ref_exists and creates.

    ASCII — the decision the caller drives off this:

        coord checkout HEAD ──► current_branch?
              │ detached                  │ branch <cb>
              ▼                            ▼
        default branch <d>         configured upstream @{u}?
        fetch <d>                   │ <r>/<up>    │ none
        base=<remote>/<d>           ▼             ▼
        (caller verifies)      <r>/<up>        "" (local HEAD)
                               (fetch <r> <up>) (no fetch)
    """
    cb = current_branch(repo, timeout_s=timeout_s)
    if cb:
        up = upstream_ref(repo, cb, timeout_s=timeout_s)
        if up:
            up_remote, up_branch = _split_remote_ref(up, default_remote=remote)
            return WorkerBase(
                base=up, fetch_remote=up_remote, fetch_branch=up_branch,
            )
        # Local-only branch (no upstream): branch off local HEAD so the
        # operator's commits aren't dropped. No fetch — nothing to refresh.
        return WorkerBase(base="", fetch_remote="", fetch_branch="")
    # Detached HEAD: best we can do is the remote default branch.
    default_branch = resolve_default_branch(repo, remote=remote, timeout_s=timeout_s)
    return WorkerBase(
        base=f"{remote}/{default_branch}", fetch_remote=remote,
        fetch_branch=default_branch,
    )


def create_worktree(
    repo: str,
    wt_path: str,
    branch: str,
    *,
    base: str = "",
    timeout_s: float = CREATE_TIMEOUT_S,
) -> WorktreeResult:
    """`git -C <repo> worktree add <wt_path> -b <branch> [<base>]`.

    base defaults to HEAD of <repo>'s current branch (i.e. the operator
    chose the integration branch by checking it out before spawning the
    coord). Pass an explicit ref (e.g. "main") to override.

    Returns WorktreeResult with path=wt_path on success. On "already
    exists" we still return success: the most common reason is a
    previous tick crashed after `git worktree add` but before the
    tasks.md update; resuming on the same path is correct.

    timeout_s bounds the `git worktree add` subprocess. It defaults to
    CREATE_TIMEOUT_S; the coord passes coord-config.json's
    `worktree_timeout_s` when the operator sets one (a checkout on a
    very large repo is I/O-bound and can exceed even the default).

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
        # Same adoption guard the first-attempt path runs below: a
        # worktree ALREADY sitting at wt_path (previous tick crashed after
        # the add) is reusable, not a permanent failure. Without this, a
        # valid, clean, correctly-registered worktree at the exact expected
        # path+branch could never be adopted and the task was
        # un-redispatchable.
        if _is_worktree_path_collision(stderr2, wt_path):
            return _adopt_existing_worktree(repo, wt_path, branch)
        # Retry path can also hit "already checked out" if the branch is
        # checked out elsewhere — that's a real conflict, surface it.
        return WorktreeResult(
            error=f"create_worktree: branch {branch} reuse failed: {stderr2}",
        )
    # Worktree-path collision path: something is already sitting at
    # wt_path (most often a previous tick that crashed mid-add but after
    # the wt-dir landed on disk). _adopt_existing_worktree decides whether
    # it is a checkout we can resume on or wreckage we must refuse.
    if _is_worktree_path_collision(stderr, wt_path):
        return _adopt_existing_worktree(repo, wt_path, branch)
    return WorktreeResult(error=f"create_worktree: git worktree add: {stderr}")


def _adopt_existing_worktree(
    repo: str, wt_path: str, branch: str,
) -> WorktreeResult:
    """Decide whether the tree already sitting at wt_path is reusable.

    Both `git worktree add` paths (`-b <branch>` and the existing-branch
    retry) can fail with "already exists" for three very different
    reasons, and only one of them is safe to dispatch into:

      registered + unlocked + on <branch>  → adopt (a previous tick
          crashed after the add landed; resuming on it is correct)
      not registered                       → a stale directory drop-in,
          not a checkout at all
      locked                               → `git worktree add` did not
          finish (killed mid-checkout by the subprocess timeout — the
          #284 case). The tree is missing files git believes are there
      wrong branch / detached HEAD         → hands the worker code from
          another task

    The verification goes through git rather than os.path checks so a
    directory drop-in can't fool it.
    """
    record = _worktree_record(repo, wt_path)
    if record is None:
        return WorktreeResult(
            error=(
                f"create_worktree: {wt_path} exists on disk but is not a "
                f"registered git worktree; remove it and retry"
            ),
        )
    if "locked" in record:
        return WorktreeResult(
            error=(
                f"create_worktree: {wt_path} is a locked (incomplete) "
                f"worktree — a previous `git worktree add` was interrupted; "
                f"remove it and retry"
            ),
        )
    checked_out = worktree_branch(wt_path, timeout_s=_VERIFY_TIMEOUT_S)
    if checked_out == branch:
        return WorktreeResult(path=wt_path)
    return WorktreeResult(
        error=(
            f"create_worktree: {wt_path} is a registered worktree on "
            f"branch {checked_out or 'detached HEAD'}, expected "
            f"{branch}; remove it and retry"
        ),
    )


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


def _worktree_record(
    repo: str, wt_path: str, *, timeout_s: float = _VERIFY_TIMEOUT_S,
) -> set[str] | None:
    """Return the flags git records for wt_path, or None if git doesn't
    list it among <repo>'s worktrees at all.

    Runs `git -C <repo> worktree list --porcelain` and returns the first
    token of every attribute line in wt_path's record — `{"bare"}`,
    `{"detached"}`, `{"branch", "locked"}`, ... — so callers can ask both
    "is it registered?" (record is not None) and "is it locked?" off one
    subprocess. Comparison uses os.path.realpath on both sides because
    git emits the canonical absolute path even when the caller passed a
    symlinked or trailing-slash variant.

    Treats subprocess errors as "not registered": the caller surfaces a
    helpful error so the operator can clean the path manually.
    """
    if not repo or not wt_path:
        return None
    try:
        proc = subprocess.run(
            ["git", "-C", repo, "worktree", "list", "--porcelain"],
            capture_output=True, text=True, timeout=timeout_s, check=False,
        )
    except (FileNotFoundError, subprocess.TimeoutExpired):
        return None
    if proc.returncode != 0:
        return None
    target = os.path.realpath(wt_path)
    flags: set[str] | None = None
    for line in (proc.stdout or "").splitlines():
        if line.startswith("worktree "):
            if flags is not None:
                return flags
            listed = line[len("worktree "):].strip()
            try:
                if listed and os.path.realpath(listed) == target:
                    flags = set()
            except OSError:
                pass
            continue
        if flags is not None and line.strip():
            flags.add(line.split(" ", 1)[0])
    return flags


def _is_registered_worktree(
    repo: str, wt_path: str, *, timeout_s: float = _VERIFY_TIMEOUT_S,
) -> bool:
    """Return True iff git lists wt_path among <repo>'s worktrees."""
    return _worktree_record(repo, wt_path, timeout_s=timeout_s) is not None


def worktree_is_dirty(
    wt_path: str,
    *,
    timeout_s: float = 15.0,
) -> tuple[bool, str]:
    """Return (is_dirty, error) for the worktree at wt_path.

    `git -C <wt> status --porcelain` — any non-empty output means the
    tree has uncommitted changes (modified, staged, or untracked) and
    must NOT be reaped (DESIGN §4.1 dirty-guard). This is the gate that
    stops the mid-work data-loss class (problem #2): a worker still
    editing in its worktree would otherwise have its tree blown away.

    Returns:
      (True,  "")           — dirty (porcelain non-empty). DO NOT reap.
      (False, "")           — clean. Safe to attempt remove.
      (False, "<err>")      — could not determine cleanliness (git error,
                              timeout, missing binary). The caller treats
                              an undeterminable status as a REFUSAL to
                              reap (fail-safe: never delete a tree we
                              can't prove clean).

    Never raises.
    """
    if not wt_path:
        return (False, "worktree_is_dirty: empty wt_path")
    try:
        proc = subprocess.run(
            ["git", "-C", wt_path, "status", "--porcelain"],
            capture_output=True, text=True, timeout=timeout_s, check=False,
        )
    except (FileNotFoundError, subprocess.TimeoutExpired) as exc:
        return (False, f"worktree_is_dirty: {exc}")
    if proc.returncode != 0:
        stderr = (proc.stderr or proc.stdout or "").strip()
        return (False, f"worktree_is_dirty: git status --porcelain: {stderr}")
    return (bool((proc.stdout or "").strip()), "")


def worktree_branch(
    wt_path: str,
    *,
    timeout_s: float = 10.0,
) -> str:
    """Return the branch checked out in the worktree at wt_path, or "".

    `git -C <wt> rev-parse --abbrev-ref HEAD`. Empty on detached HEAD,
    non-git path, or any error. Used by the branch-identity guard
    (DESIGN §4.4 / §4.2): a CLEAN checkout of the WRONG branch sitting at
    a terminal-slug worktree path must NOT be reaped — `force=False`
    does not protect a clean tree, so we corroborate the dir actually has
    the task's expected `worker/<slug>` branch checked out before reaping.
    Never raises.
    """
    if not wt_path:
        return ""
    try:
        proc = subprocess.run(
            ["git", "-C", wt_path, "rev-parse", "--abbrev-ref", "HEAD"],
            capture_output=True, text=True, timeout=timeout_s, check=False,
        )
    except (FileNotFoundError, subprocess.TimeoutExpired):
        return ""
    if proc.returncode != 0:
        return ""
    out = (proc.stdout or "").strip()
    # Detached HEAD prints "HEAD"; treat as no-branch-identity.
    return "" if out == "HEAD" else out


def remove_worktree(
    repo: str,
    wt_path: str,
    *,
    force: bool = False,
    timeout_s: float = 30.0,
) -> WorktreeResult:
    """`git -C <repo> worktree remove [--force] <wt_path>` — dirty-guarded.

    Refuses to act when wt_path is empty, equal to repo, or outside the
    ~/.fleet/projects/ tree. Returns OUTCOME_NOOP when the worktree is
    already absent (ENOENT-style — mirrors `rm -f`).

    DESIGN §4.1: `force=False` is now the DEFAULT. Before removing, run
    `git -C <wt> status --porcelain` — a non-empty result (uncommitted
    work) → DO NOT remove; return OUTCOME_DIRTY_PARKED (the caller parks
    + surfaces, keeping the worker's unsaved edits). `force=False` also
    means a tree that turns dirty BETWEEN the porcelain check and the
    `git worktree remove` makes git FAIL rather than blow the tree away;
    we classify that TOCTOU failure as OUTCOME_DIRTY_PARKED too (not a
    generic error), because the cause is the same — uncommitted work.

    `force=True` is OPERATOR-EXPLICIT only (a future `--force` reap path,
    DESIGN open question #2); no coord caller passes it. It skips both the
    porcelain guard and the `--force`-free git invocation.

    Returns WorktreeResult with .outcome set to exactly one OUTCOME_*:
      - OUTCOME_REMOVED      git removed the (clean) tree.
      - OUTCOME_DIRTY_PARKED porcelain non-empty, OR a TOCTOU dirty
                             remove failure. error is set + path kept.
      - OUTCOME_NOOP         nothing to remove (already gone / safe-skip).
      - OUTCOME_ERROR        unexpected failure (path vanished mid-op,
                             git internal error). error is set.
    """
    if not _safe_to_remove(repo, wt_path):
        return WorktreeResult(
            outcome=OUTCOME_ERROR,
            error=f"remove_worktree: refuse unsafe path {wt_path!r}",
        )
    if not os.path.exists(wt_path):
        # Already gone — treat as a clean no-op. The git worktree metadata
        # in the parent repo may still need pruning; we run that next.
        _prune(repo, timeout_s=timeout_s)
        return WorktreeResult(path=wt_path, outcome=OUTCOME_NOOP)
    if not force:
        # Dirty-guard (DESIGN §4.1). An undeterminable status is treated
        # as a refusal to reap (fail-safe) — surfaced as DIRTY_PARKED so
        # the caller keeps the tree + worker dir rather than risk a
        # destructive remove on a tree we couldn't prove clean.
        dirty, derr = worktree_is_dirty(wt_path, timeout_s=timeout_s)
        if derr:
            return WorktreeResult(
                path=wt_path, outcome=OUTCOME_DIRTY_PARKED,
                error=f"remove_worktree: cleanliness undeterminable: {derr}",
            )
        if dirty:
            return WorktreeResult(
                path=wt_path, outcome=OUTCOME_DIRTY_PARKED,
                error=f"remove_worktree: {wt_path} has uncommitted changes; parked",
            )
    cmd = ["git", "-C", repo, "worktree", "remove"]
    if force:
        cmd.append("--force")
    cmd.append(wt_path)
    try:
        proc = subprocess.run(
            cmd, capture_output=True, text=True, timeout=timeout_s, check=False,
        )
    except (FileNotFoundError, subprocess.TimeoutExpired) as exc:
        return WorktreeResult(
            path=wt_path, outcome=OUTCOME_ERROR, error=f"remove_worktree: {exc}",
        )
    if proc.returncode == 0:
        _prune(repo, timeout_s=timeout_s)
        return WorktreeResult(path=wt_path, outcome=OUTCOME_REMOVED)
    stderr = (proc.stderr or proc.stdout or "").strip()
    # TOCTOU: force=False git refuses a tree that turned dirty between the
    # porcelain check above and now. git reports "contains modified or
    # untracked files, use --force to delete it". Classify as DIRTY_PARKED
    # (same cause as the porcelain guard), NOT a generic error — the
    # caller keeps the tree + worker dir rather than re-trying a remove.
    if _is_dirty_remove_refusal(stderr):
        return WorktreeResult(
            path=wt_path, outcome=OUTCOME_DIRTY_PARKED,
            error=f"remove_worktree: git refused dirty tree (TOCTOU): {stderr}",
        )
    return WorktreeResult(
        path=wt_path, outcome=OUTCOME_ERROR,
        error=f"remove_worktree: git worktree remove: {stderr}",
    )


def _is_dirty_remove_refusal(stderr: str) -> bool:
    """Return True iff git refused `worktree remove` because the tree is
    dirty (the TOCTOU window: clean at the porcelain check, dirty by the
    time git ran). git's phrasing:
      "fatal: '<path>' contains modified or untracked files, use --force
       to delete it"
    Anchored on the stable substring so it survives path interpolation.
    """
    if not stderr:
        return False
    low = stderr.lower()
    return "use --force to delete it" in low or (
        "modified or untracked files" in low
    )


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
