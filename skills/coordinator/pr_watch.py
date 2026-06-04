"""Durable, tick-owned PR watching (PR1 — the tracking half).

Design: docs/DESIGN-coord-pr-watch-durable.md. This module is the
*tracking* half (PR1 of 2): it derives the watch set from durable task
state every tick, probes each watched PR (batched per repo, fail-soft),
reduces the probe through the PR state machine, surfaces actionable /
terminal events, and reconciles MERGED PRs by flipping their backing
tasks done. The *auto-fix* half (PR2) — auto-rebase / fix-subagent
dispatch + §6 action leases — is deliberately NOT built here.

Why a derived invariant, not an action
--------------------------------------
"Watch this PR" is not an event an agent takes; it is a fact the tick
derives from tasks.md every pass. Because the watch set is rebuilt from
tasks.md on every tick, a watch can never be forgotten (it's derived),
can't be lost to a dead chat session (it's on disk, written by the
tick), and can't be stopped early (only a *terminal PR state* — merged
or closed — prunes it). Green-and-open is NON-terminal and stays
watched indefinitely (regression guard for the 2026-06-03 #195 bug).

    ┌──────────────────── every coordinator tick ────────────────────┐
    │ enroll : group owned (pr_url + non-terminal) tasks BY pr_number │
    │ orphan : OPEN watch with empty tasks[] -> raise-hand            │
    │ probe  : per repo: 1 git fetch + 1 batched gh api graphql       │
    │ reduce : snapshot -> event (green is OPEN, not terminal)        │
    │ act    : MERGED -> flip ALL tasks done FIRST, prune watch LAST  │
    │          CLOSED_UNMERGED -> raise-hand, retain                  │
    │          STALE/BEHIND/CI-fail/CHANGES_REQUESTED -> raise-hand   │
    │              (PR1 SURFACES only; PR2 dispatches the fixer)      │
    └─────────────────────────────────────────────────────────────────┘
                              │ writes (atomic .tmp + fsync + rename)
                              ▼
            ~/.fleet/projects/<project>/pr-watches.json

Single writer is the coordinator tick (same discipline as
coord-state.json), called while the tick holds the coordinator flock.
"""
from __future__ import annotations

import datetime as _dt
import json
import os
import re
import subprocess
from dataclasses import dataclass, field
from pathlib import Path
from typing import Callable, Protocol

# ----------------------------------------------------------------------
# Schema / constants
# ----------------------------------------------------------------------

SCHEMA_VERSION = 1
WATCH_FILE = "pr-watches.json"

# Task statuses that END a task's claim on a PR. A watch's tasks[] is the
# set of *live, non-terminal* owned tasks pointing at the PR; a task in
# one of these states no longer backs the watch (enrollment §2).
TERMINAL_TASK_STATUSES = frozenset({"done", "abandoned"})

# PR-watch lifecycle states (§4). ONLY these two are terminal — a green,
# open PR is an OPEN sub-state and stays watched until it merges or
# closes. There is deliberately NO state where a green check ends a
# watch (the structural guard against the 2026-06-03 stop-at-green bug).
STATE_OPEN = "open"
STATE_MERGED = "merged"
STATE_CLOSED_UNMERGED = "closed-unmerged"
_TERMINAL_PR_STATES = frozenset({STATE_MERGED, STATE_CLOSED_UNMERGED})

# Adaptive probe cadence (§3). Actionable / next-to-merge PRs probe every
# tick; a quiescent (green + READY/awaiting-merge) PR only needs to catch
# a merge or a new event, so it probes on a slower cadence to keep the
# aggregate `gh` call rate bounded (one shared token across all coords).
# 1 == "every tick" preserves the bound at the cheap end; tests override.
_SLOW_CADENCE_TICKS_DEFAULT = 5

# Backoff bounds for transient probe failures (§3). A transient gh/git
# error never prunes or flips a watch — it skips this tick and retries
# with a jittered, capped exponential backoff. Stored as a tick budget
# (probe_skip_until_tick) rather than wall-clock so tests stay
# deterministic (no time.Sleep, no clock assertions).
_BACKOFF_BASE_TICKS = 1
_BACKOFF_MAX_TICKS = 8


# ----------------------------------------------------------------------
# Event taxonomy — the reduced result of one probe (§4 / §5.1)
# ----------------------------------------------------------------------

# Non-actionable OPEN sub-states.
EVENT_OPEN = "open"            # green/open, not next-to-merge or not up-to-date: keep watching
EVENT_READY = "ready"          # up-to-date + green + no required review pending: surface "#N mergeable"
# Actionable OPEN sub-states (PR1 SURFACES; PR2 dispatches the fixer).
EVENT_STALE = "stale"          # head does not contain fresh base (strict protection) -> rebase needed
EVENT_BEHIND = "behind"        # mergeStateStatus BEHIND -> rebase needed
EVENT_DIRTY = "dirty"          # mergeStateStatus DIRTY / mergeable CONFLICTING -> rebase needed
EVENT_CI_FAILED = "ci-failed"  # a check FAILURE/ERROR/CANCELLED/TIMED_OUT
EVENT_CHANGES_REQUESTED = "changes-requested"  # reviewDecision CHANGES_REQUESTED
# Terminal.
EVENT_MERGED = "merged"
EVENT_CLOSED_UNMERGED = "closed-unmerged"
# Probe outcomes that are NOT a PR-state event.
EVENT_SKIP = "skip"            # transient probe failure -> retain, retry next tick
EVENT_NOT_FOUND = "not-found"  # definitive 404 -> raise-hand pr-not-found

_ACTIONABLE_EVENTS = frozenset({
    EVENT_STALE, EVENT_BEHIND, EVENT_DIRTY, EVENT_CI_FAILED,
    EVENT_CHANGES_REQUESTED,
})


# ----------------------------------------------------------------------
# Probe snapshot + injectable prober seam
# ----------------------------------------------------------------------


@dataclass
class PRSnapshot:
    """One PR's projection from the batched GraphQL query (§3).

    Mirrors the GitHub fields the reducer needs. `error` is set when the
    probe could not produce a snapshot for this PR — distinguishing a
    transient failure (retain + retry) from a definitive 404 (raise-hand)
    is carried by `not_found`.
    """

    number: int = 0
    pr_state: str = ""              # OPEN | CLOSED | MERGED (GitHub enum)
    merged_at: str | None = None
    merge_state_status: str = ""    # CLEAN | BEHIND | DIRTY | BLOCKED | UNSTABLE | UNKNOWN
    review_decision: str = ""       # APPROVED | CHANGES_REQUESTED | REVIEW_REQUIRED | ""
    head_ref_oid: str = ""
    base_ref_oid: str = ""
    head_ref_name: str = ""
    base_ref_name: str = ""
    is_draft: bool = False          # GitHub draft PR (never mergeable)
    checks: str = ""                # SUCCESS | FAILURE | PENDING (derived rollup)
    # Transport outcome (not part of the GitHub projection):
    error: str = ""                 # non-empty => probe failed for this PR
    not_found: bool = False         # True => definitive 404 (raise-hand)


@dataclass
class RepoProbe:
    """Result of one per-repo probe pass (§3): one fetch + one batched
    query. `error` set => the whole repo probe failed transiently (every
    watch in the repo skips + retains this tick).

    `fresh_base_shas` maps each PR's actual base ref name -> the fetched
    tip of that base, so the §5.1(a) ancestor check compares a PR head
    against ITS OWN base (codex iter-3 [P2]: a stacked PR with a non-main
    base would otherwise be measured against origin/main and mis-read
    READY/STALE — though fleet's PR-base-is-always-main rule makes this
    rare, other coords/repos may stack at the PR level). `fresh_base_sha`
    is the primary/default base tip, kept for the common single-base case.
    """

    snapshots: dict[int, PRSnapshot] = field(default_factory=dict)
    fresh_base_sha: str = ""
    fresh_base_shas: dict[str, str] = field(default_factory=dict)
    fetch_ok: bool = False
    fetch_error: str = ""           # non-empty => the git fetch failed (transient)
    error: str = ""

    def base_sha_for(self, base_ref_name: str) -> str:
        """Fresh tip for a PR's actual base ref; fall back to the
        primary base SHA when that ref wasn't separately fetched."""
        if base_ref_name and base_ref_name in self.fresh_base_shas:
            return self.fresh_base_shas[base_ref_name]
        return self.fresh_base_sha


class Prober(Protocol):
    """Injectable probe seam. Production impl (GhGitProber) shells to
    git + gh; tests inject a fake so they're deterministic (no network,
    no clock). One probe call covers a whole repo's watched PRs.
    """

    def probe_repo(
        self,
        repo_path: str,
        owner_repo: str,
        base_ref: str,
        pr_numbers: list[int],
        head_oids: list[str],
    ) -> RepoProbe:
        ...

    def is_ancestor(self, repo_path: str, ancestor_sha: str, descendant_sha: str) -> bool:
        ...


# ----------------------------------------------------------------------
# Transient-vs-definitive classification (§3)
# ----------------------------------------------------------------------

# A *transient* failure must NEVER prune a watch or flip it terminal — we
# skip + retain + retry. A *definitive* 404 means the PR/repo is genuinely
# gone — raise-hand `pr-not-found`. Everything we can't positively prove
# is a 404 is treated as transient (fail-soft bias: retain, never lose a
# watch on an ambiguous error).
_NOT_FOUND_RE = re.compile(
    r"\b(404|could not resolve to|not found|no such pull request)\b",
    re.IGNORECASE,
)


def classify_probe_error(stderr: str) -> str:
    """Return EVENT_NOT_FOUND for a definitive 404, else EVENT_SKIP.

    Bias is fail-soft: only a clearly-definitive 404 signature returns
    NOT_FOUND. 5xx / timeout / auth blip / rate-limit / anything
    ambiguous -> SKIP (retain + retry), per §3.
    """
    if _NOT_FOUND_RE.search(stderr or ""):
        return EVENT_NOT_FOUND
    return EVENT_SKIP


# ----------------------------------------------------------------------
# pr_url parsing
# ----------------------------------------------------------------------

# https://github.com/<owner>/<repo>/pull/<N>  (also tolerate a trailing
# slash / fragment / query). Captures owner/repo + number.
_PR_URL_RE = re.compile(
    r"github\.com[:/]+(?P<owner>[^/]+)/(?P<repo>[^/]+)/pull/(?P<num>\d+)\b"
)


def parse_pr_url(url: str) -> tuple[str, int] | None:
    """Return (owner_repo, pr_number) parsed from a GitHub PR URL, or None.

    owner_repo is the canonical "<owner>/<repo>" the coord-scope assert
    (§2) compares against the coord's own repo. GitHub owner/repo names
    are CASE-INSENSITIVE, so we lowercase here — and derive_owner_repo
    does the same — so a remote `EdisonShen/Fleet` matches a PR URL
    `edisonshen/fleet` (codex iter-6 [P2]; mirrors coord_config's
    case-insensitive remote match).
    """
    if not url:
        return None
    m = _PR_URL_RE.search(url)
    if not m:
        return None
    owner_repo = f"{m.group('owner')}/{m.group('repo')}".lower()
    try:
        num = int(m.group("num"))
    except ValueError:
        return None
    return owner_repo, num


# ----------------------------------------------------------------------
# Watch-file load / save (single writer = the tick; atomic publish)
# ----------------------------------------------------------------------


def watch_path(project_dir: Path) -> Path:
    return Path(project_dir) / WATCH_FILE


def load_watches(project_dir: Path) -> dict:
    """Load pr-watches.json. Returns a normalized dict with `schema` +
    `watches`. A missing / unreadable / malformed file yields an empty
    schema-v1 doc (the tick recreates watches from tasks.md anyway, so a
    lost file is self-healing — but we never crash a tick on it)."""
    path = watch_path(project_dir)
    try:
        with open(path, "r", encoding="utf-8") as fh:
            data = json.load(fh)
    except (FileNotFoundError, json.JSONDecodeError, OSError):
        data = None
    if not isinstance(data, dict):
        return {"schema": SCHEMA_VERSION, "watches": {}}
    watches = data.get("watches")
    if not isinstance(watches, dict):
        watches = {}
    return {"schema": SCHEMA_VERSION, "watches": watches}


def save_watches(project_dir: Path, doc: dict) -> None:
    """Atomic publish (tmp + fsync + rename) of pr-watches.json. Crash
    between tmp-write and rename leaves the prior file intact (§1)."""
    path = watch_path(project_dir)
    parent = path.parent
    parent.mkdir(parents=True, exist_ok=True)
    import tempfile

    fd, tmp = tempfile.mkstemp(prefix=path.name + ".tmp.", dir=str(parent))
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as fh:
            json.dump(doc, fh, indent=2, sort_keys=True)
            fh.flush()
            os.fsync(fh.fileno())
        os.replace(tmp, path)
    except Exception:
        try:
            os.unlink(tmp)
        except FileNotFoundError:
            pass
        raise


def _new_watch(pr_number: int, pr_url: str, branch: str, base: str) -> dict:
    """A fresh schema-v1 watch record (§1). inflight_action +
    dispatched_events are reserved for PR2 (auto-fix) — present in the
    schema for forward-compat, NOT populated by PR1."""
    return {
        "pr_number": pr_number,
        "pr_url": pr_url,
        "tasks": [],
        "branch": branch,
        "base": base or "main",
        "state": STATE_OPEN,
        "orphaned": False,
        "last_seen_at": None,
        "last_probe_at": None,
        "last_snapshot": None,
        # --- PR2 forward-compat (NOT populated in PR1) ---
        "inflight_action": None,
        "dispatched_events": {},
    }


# ----------------------------------------------------------------------
# Result of one reconcile pass (surfaced through the tick)
# ----------------------------------------------------------------------


@dataclass
class WatchOutcome:
    """What reconcile_watches did this tick — counters + the raise-hand /
    diagnostic lines the tick funnels into TickResult.errors (raise-hand
    is "append a clear diagnostic", per feedback_surface_dont_silo). The
    tick is the single writer; this struct carries nothing the tick must
    persist beyond the watch doc itself."""

    enrolled: int = 0
    probed: int = 0
    pruned: int = 0
    tasks_flipped: int = 0
    raises: list[str] = field(default_factory=list)
    notes: list[str] = field(default_factory=list)
    errors: list[str] = field(default_factory=list)


# ----------------------------------------------------------------------
# Reduction: snapshot -> event (§4 / §5.1)
# ----------------------------------------------------------------------


def _checks_from_rollup(rollup) -> str:
    """Derive a single SUCCESS/FAILURE/PENDING verdict from a
    statusCheckRollup list. No checks configured == SUCCESS (a repo with
    no required checks is green-by-default). Any pending/queued/in-flight
    -> PENDING; any failing conclusion -> FAILURE; else SUCCESS."""
    if not isinstance(rollup, list) or not rollup:
        return "SUCCESS"
    pending = False
    failed = False
    for c in rollup:
        if not isinstance(c, dict):
            continue
        status = str(c.get("status", "") or "").upper()
        conclusion = str(c.get("conclusion", "") or "").upper()
        # CheckRun uses status+conclusion; StatusContext uses `state`.
        state = str(c.get("state", "") or "").upper()
        if status in ("QUEUED", "IN_PROGRESS", "PENDING", "WAITING", "REQUESTED") \
                or state == "PENDING" \
                or (status == "" and conclusion == "" and state == ""):
            pending = True
        elif conclusion in ("FAILURE", "ERROR", "CANCELLED", "TIMED_OUT", "ACTION_REQUIRED",
                            "STALE", "STARTUP_FAILURE") \
                or state in ("FAILURE", "ERROR"):
            failed = True
        # SUCCESS / NEUTRAL / SKIPPED / EXPECTED == not failing, not pending.
    if failed:
        return "FAILURE"
    if pending:
        return "PENDING"
    return "SUCCESS"


def reduce_snapshot(
    snap: PRSnapshot,
    *,
    fresh_base_sha: str,
    is_ancestor: Callable[[str, str], bool],
) -> str:
    """Reduce one probe snapshot to a single event (§4/§5.1).

    `is_ancestor(ancestor_sha, descendant_sha)` answers the §5.1(a)
    mergeability check against FRESH refs — NEVER the mergeStateStatus
    word. A fetch that couldn't produce a fresh base SHA (empty) makes
    mergeability UNKNOWN: we keep watching (EVENT_OPEN), never assert
    READY (fail-soft, §3 / regression for #199).

    PR1 maps actionable OPEN sub-states (STALE/BEHIND/DIRTY/CI-fail/
    CHANGES_REQUESTED) to their event so the tick SURFACES them; PR2
    dispatches the fixer. Green-and-open is EVENT_OPEN (non-terminal).
    """
    if snap.error:
        return EVENT_NOT_FOUND if snap.not_found else EVENT_SKIP

    pr_state = (snap.pr_state or "").upper()
    if pr_state == "MERGED" or snap.merged_at:
        return EVENT_MERGED
    if pr_state == "CLOSED":
        return EVENT_CLOSED_UNMERGED

    # --- OPEN: classify the sub-state. Order matters: a PR can be BOTH
    # CI-failed AND behind; we surface the most blocking signal first so
    # the operator (PR1) / fixer-dispatcher (PR2) sees the right thing.
    mss = (snap.merge_state_status or "").upper()
    checks = (snap.checks or "").upper()
    review = (snap.review_decision or "").upper()

    if checks == "FAILURE":
        return EVENT_CI_FAILED
    # DIRTY == real merge conflict (mergeable CONFLICTING). Distinct from
    # BEHIND (just stale under non-strict). Both need a rebase subagent.
    if mss == "DIRTY":
        return EVENT_DIRTY
    if mss == "BEHIND":
        return EVENT_BEHIND
    if review == "CHANGES_REQUESTED":
        return EVENT_CHANGES_REQUESTED

    # --- §5.1(a): mergeability is an ancestor check on FRESH refs, never
    # the status word. mergeable <=> checks SUCCESS AND no required review
    # pending AND fresh base is an ancestor of head.
    if not fresh_base_sha or not snap.head_ref_oid:
        # Unknown freshness -> keep watching, never assert READY.
        return EVENT_OPEN
    head_contains_base = is_ancestor(fresh_base_sha, snap.head_ref_oid)
    if not head_contains_base:
        # Head does not contain current base tip. Under strict protection
        # this PR is NOT mergeable even with green CI + 0 reviews -> STALE
        # (regression for the #199 mis-read). PR1 surfaces; PR2 rebases
        # only the uniquely-eligible next-to-merge PR.
        return EVENT_STALE

    # Head DOES contain fresh base.
    if checks != "SUCCESS":
        # green-not-yet (PENDING) — up to date but still running: keep
        # watching, do not surface READY.
        return EVENT_OPEN
    if review == "REVIEW_REQUIRED":
        # required review still pending -> not mergeable yet; keep watching.
        return EVENT_OPEN
    # READY-gate (codex iter-4 [P2]): the ancestor check proves the head
    # is up-to-date, but GitHub can STILL refuse the merge for reasons the
    # ancestry/checks/review triad doesn't capture — a DRAFT PR, or a
    # `BLOCKED` state from unresolved conversations / required deployments
    # / a required-but-unconfigured check. We don't TRUST the status word
    # to assert STALE (§5.1a), but we conservatively WITHHOLD READY when
    # the word says the PR can't merge yet — keep watching instead of
    # falsely surfacing "#N is mergeable".
    if snap.is_draft or mss in ("DRAFT", "BLOCKED"):
        return EVENT_OPEN
    # Up-to-date + green + no required review pending + no blocking word
    # -> READY.
    return EVENT_READY


# ----------------------------------------------------------------------
# The reconcile invariant (§2) — the public entry point for the tick
# ----------------------------------------------------------------------


def reconcile_watches(
    tasks: list,
    *,
    project: str,
    project_dir: Path,
    coord_owner_repo: str | None,
    prober: Prober,
    flip_task_done: Callable[[str], None],
    now_iso: str,
    tick_count: int,
    slow_cadence_ticks: int = _SLOW_CADENCE_TICKS_DEFAULT,
    repo_path: str = "",
    enroll_tasks: list | None = None,
) -> WatchOutcome:
    """Run one PR-watch reconcile pass. Single writer = the tick (caller
    holds the coord flock). Returns a WatchOutcome the tick funnels into
    TickResult.

    Steps (§2):
      1. ENROLL — group this coord's owned (pr_url + non-terminal) tasks
         BY pr_number; tasks[] = ALL live owned tasks for that PR.
      2. REFRESH tasks[] each tick + flag ORPHAN (OPEN watch, empty tasks).
      3. PROBE per repo (one fetch + one batched query, fail-soft, §3).
      4. REDUCE + persist snapshot; PR1 SURFACES actionable events.
      5. TERMINAL — MERGED: flip ALL tasks done FIRST, prune watch LAST
         (gc backstop reaps trees, §2). CLOSED_UNMERGED: raise-hand, retain.

    coord_owner_repo: the coord's own "<owner>/<repo>" — the coord-scope
      assert (§2) drops any watch whose PR repo != this before probing.
      None means we couldn't derive it; we then refuse to probe (coord
      scope is strict — never probe a repo we can't prove is ours).
    flip_task_done: callback the tick supplies (wraps `fleet tasks set
      <slug> status=done`). reconcile_watches never shells out itself —
      the tick owns all CLI mutation (single-seam discipline).
    """
    out = WatchOutcome()
    doc = load_watches(project_dir)
    watches: dict = doc["watches"]

    # Normalize the coord's own repo to lowercase so the coord-scope
    # comparison is case-insensitive (GitHub owner/repo are; codex iter-6
    # [P2]). parse_pr_url already lowercases the PR-side, so both sides
    # match regardless of how the remote / PR URL was cased.
    if coord_owner_repo is not None:
        coord_owner_repo = coord_owner_repo.lower()

    if enroll_tasks is None:
        enroll_tasks = tasks

    def _owned(ts):
        return [
            t for t in ts
            if getattr(t, "pr_url", "") and getattr(t, "status", "") not in TERMINAL_TASK_STATUSES
        ]

    # --- 1. ENROLL: derive watches from durable task state (THIS coord's
    # project ONLY). Watch identity is the PR NUMBER, so two tasks
    # pointing at one PR collapse to one watch (dedupe-by-PR-number).
    #
    # `owned` = current-snapshot owned tasks (drives refresh/orphan/probe).
    # `enroll_owned` = current PLUS any pre-reconcile task with a pr_url
    # that the legacy reconcile cleared earlier this tick (codex iter-7
    # [P2]) — used ONLY to CREATE the watch so a fresh rollout doesn't miss
    # a PR whose url was just cleared. A pre-reconcile dup of a current
    # slug is dropped (current row is fresher).
    owned = _owned(tasks)
    current_slugs = {getattr(t, "slug", "") for t in owned}
    enroll_owned = owned + [
        t for t in _owned(enroll_tasks)
        if getattr(t, "slug", "") not in current_slugs
    ]
    # group enroll_owned by (owner_repo, pr_number); coord-scope assert per
    # PR. `in_scope` is the subset of CURRENT owned tasks that PASS the
    # coord-scope assert — the §2 refresh loop below MUST use this, NOT
    # enroll_owned, else (a) a foreign-repo task sharing a PR number could
    # become backing for the in-scope watch and get flipped done on merge
    # (codex iter-1 [P2]), or (b) a just-cleared pre-reconcile task could
    # wrongly keep tasks[] populated.
    by_pr: dict[int, list] = {}
    pr_meta: dict[int, tuple[str, str]] = {}  # pr_number -> (owner_repo, pr_url)
    in_scope: list = []
    for t in enroll_owned:
        parsed = parse_pr_url(getattr(t, "pr_url", ""))
        if parsed is None:
            continue
        owner_repo, pr_num = parsed
        # COORD-SCOPE ASSERT (§2): never enroll / probe a PR in a repo
        # that isn't this coord's own. A mismatch is a scope violation
        # (feedback_coord_scope_strict) — surface + skip, never probe.
        if coord_owner_repo is not None and owner_repo != coord_owner_repo:
            out.errors.append(
                f"pr-watch: task {getattr(t, 'slug', '?')} PR {owner_repo}#{pr_num} "
                f"is not in this coord's repo {coord_owner_repo!r}; skipping (coord-scope)"
            )
            continue
        # in_scope (drives refresh `live`) is CURRENT owned tasks only — a
        # pre-reconcile-only task creates the watch but does NOT count as a
        # live backing task.
        if getattr(t, "slug", "") in current_slugs:
            in_scope.append(t)
        by_pr.setdefault(pr_num, []).append(t)
        if pr_num not in pr_meta:
            pr_meta[pr_num] = (owner_repo, getattr(t, "pr_url", ""))

    # upsert (idempotent, keyed by PR number): present -> refresh, absent
    # -> create. No start_watch/stop_watch API to misuse (§7).
    for pr_num, tasks_for_pr in by_pr.items():
        key = str(pr_num)
        _owner_repo, url = pr_meta[pr_num]
        branch = next((getattr(t, "branch", "") for t in tasks_for_pr if getattr(t, "branch", "")), "")
        if key not in watches:
            watches[key] = _new_watch(pr_num, url, branch, "main")
            out.enrolled += 1
        w = watches[key]
        # refresh enrollment-derived fields each tick.
        w["pr_url"] = url or w.get("pr_url", "")
        if branch:
            w["branch"] = branch
        w["tasks"] = sorted({getattr(t, "slug", "") for t in tasks_for_pr if getattr(t, "slug", "")})

    # --- 2. REFRESH tasks[] for EVERY existing watch (a task may have been
    # added/removed/gone-terminal independently) + flag ORPHAN.
    for key, w in watches.items():
        try:
            pr_num = int(w.get("pr_number", key))
        except (TypeError, ValueError):
            continue
        live = sorted({
            getattr(t, "slug", "")
            for t in in_scope
            if (parse_pr_url(getattr(t, "pr_url", "")) or (None, None))[1] == pr_num
            and getattr(t, "slug", "")
        })
        w["tasks"] = live
        # Orphan = the PR is CONFIRMED OPEN (we've probed it and saw an
        # OPEN snapshot) but NO live in-scope task points at it anymore.
        # We require the confirmed-OPEN snapshot before raising: a watch
        # enrolled but never successfully probed (e.g. coord-scope can't
        # derive the repo) must NOT assert "PR is OPEN" — we can't prove
        # it, and a task archived before any probe is just a pending
        # prune candidate, not an operator-actionable orphan.
        snap = w.get("last_snapshot")
        confirmed_open = (
            isinstance(snap, dict)
            and (snap.get("pr_state") or "").upper() == "OPEN"
        )
        if w.get("state") == STATE_OPEN and not live and confirmed_open and not w.get("orphaned"):
            # OPEN PR with NO live non-terminal owned task -> orphan.
            # raise-hand, NO auto-action (PR1 surfaces; never auto-rebase).
            w["orphaned"] = True
            out.raises.append(
                f"pr-watch: PR #{pr_num} is OPEN but no live task points at it "
                f"(orphaned-pr); needs operator attention — see {w.get('pr_url', '')}"
            )
        elif live:
            # re-acquired a backing task -> clear the orphan flag.
            w["orphaned"] = False

    # --- 3+4. PROBE per repo (coord owns exactly one repo) + reduce.
    # Only probe watches that are (a) in scope, (b) not already terminal,
    # (c) due this tick (adaptive cadence + transient backoff).
    if coord_owner_repo is None:
        # Can't prove the repo is ours -> refuse to probe (coord-scope
        # strict). We still persisted enrollment/orphan above so the
        # watch survives; a later tick with a derivable repo probes it.
        save_watches(project_dir, doc)
        return out

    due_numbers: list[int] = []
    head_oids: list[str] = []
    for key, w in watches.items():
        state = w.get("state")
        # MERGED is truly terminal (irreversible on GitHub) -> never
        # re-probe. CLOSED_UNMERGED is retained-but-REVERSIBLE: the
        # operator can reopen the PR, and a live task may still point at
        # it — so re-probe a closed watch IFF it still has live backing
        # tasks (codex iter-8 [P2]). A live task on a reopened PR then
        # transitions back to OPEN and reconciles a later merge. A closed
        # watch with NO live task stays parked until the operator acks.
        if state == STATE_MERGED:
            continue
        if state == STATE_CLOSED_UNMERGED and not w.get("tasks"):
            continue
        try:
            pr_num = int(w.get("pr_number", key))
        except (TypeError, ValueError):
            continue
        # COORD-SCOPE re-assert on PROBE (codex iter-2 [P2]): a watch
        # persisted on an earlier tick when derive_owner_repo failed
        # (coord_owner_repo was None) could belong to a FOREIGN repo. Now
        # that the repo is derivable, re-check the watch's stored pr_url
        # owner before probing — else we'd probe <coord_repo>#N for a
        # foreign #N and could record/terminate the wrong PR's state.
        parsed = parse_pr_url(w.get("pr_url", ""))
        if parsed is not None and parsed[0] != coord_owner_repo:
            out.errors.append(
                f"pr-watch: persisted watch {parsed[0]}#{pr_num} is not in this "
                f"coord's repo {coord_owner_repo!r}; not probing (coord-scope)"
            )
            continue
        if not _probe_due(w, tick_count, slow_cadence_ticks):
            continue
        due_numbers.append(pr_num)
        snap_prev = w.get("last_snapshot") or {}
        prev_head = snap_prev.get("head_ref_oid") if isinstance(snap_prev, dict) else None
        if isinstance(prev_head, str) and prev_head:
            head_oids.append(prev_head)

    if due_numbers:
        base_ref = _common_base_ref(watches, due_numbers)
        repo_probe = prober.probe_repo(
            repo_path, coord_owner_repo, base_ref, sorted(set(due_numbers)),
            sorted(set(head_oids)),
        )
        _apply_probe(
            watches, due_numbers, repo_probe, prober, repo_path,
            now_iso=now_iso, tick_count=tick_count, out=out,
        )

    # --- 5. TERMINAL handling — ORDER MATTERS (§2). Iterate a snapshot of
    # the keys because we prune (mutate) MERGED watches as we go.
    for key in list(watches.keys()):
        w = watches[key]
        state = w.get("state")
        if state == STATE_MERGED:
            backing = list(w.get("tasks") or [])
            if backing:
                # 1. flip ALL backing tasks done FIRST (idempotent — a
                # task already done is a no-op on the CLI side).
                for slug in backing:
                    try:
                        flip_task_done(slug)
                        out.tasks_flipped += 1
                    except Exception as exc:  # noqa: BLE001
                        # A flip failure must not lose the watch: leave it
                        # MERGED + un-pruned so the NEXT tick re-reconciles
                        # (flip is idempotent). Surface + skip the prune.
                        out.errors.append(
                            f"pr-watch: flip {slug} done for merged PR {key}: {exc}"
                        )
                        break
                else:
                    # all flips succeeded -> 2. worktree reap is the
                    # existing _maybe_gc_worktrees backstop (NOT a
                    # duplicate per-merge gc here, §2). 3. prune LAST so a
                    # crash mid-way simply re-reconciles next tick.
                    del watches[key]
                    out.pruned += 1
                    out.notes.append(f"pr-watch: PR {key} MERGED; flipped {len(backing)} task(s) done")
            else:
                # Empty tasks[] (orphan merge): record a note, mutate NO
                # task; prune the watch (nothing left to reconcile).
                del watches[key]
                out.pruned += 1
                out.notes.append(
                    f"pr-watch: orphan PR {key} merged with no backing task; no task mutated"
                )
        elif state == STATE_CLOSED_UNMERGED:
            # raise-hand, retain until operator acks (we keep re-raising a
            # short note each tick is noisy — raise once on the transition;
            # _apply_probe sets the state, we raise here only on the first
            # tick it became closed via the `_just_closed` marker).
            if w.pop("_just_closed", False):
                out.raises.append(
                    f"pr-watch: PR #{key} was CLOSED without merging; "
                    f"needs operator attention — {w.get('pr_url', '')}"
                )

    save_watches(project_dir, doc)
    return out


# ----------------------------------------------------------------------
# Probe helpers
# ----------------------------------------------------------------------


def _probe_due(watch: dict, tick_count: int, slow_cadence_ticks: int) -> bool:
    """Is this watch due to probe this tick? (§3 adaptive cadence +
    transient backoff.)

    - A transient backoff (`probe_skip_until_tick`) suppresses probing
      until that tick budget elapses.
    - Actionable / never-probed / non-READY watches probe EVERY tick.
    - A quiescent (last event READY/open-up-to-date) watch probes on the
      slower cadence (only needs to catch a merge / new event).
    """
    skip_until = watch.get("probe_skip_until_tick")
    if isinstance(skip_until, int) and tick_count < skip_until:
        return False
    last_event = watch.get("last_event")
    if last_event in (EVENT_READY,):
        # quiescent: slow cadence. probe when tick_count hits the cadence
        # boundary so the merge is still caught within slow_cadence_ticks.
        if slow_cadence_ticks <= 1:
            return True
        return (tick_count % slow_cadence_ticks) == 0
    # never probed, actionable, or plain-OPEN -> every tick.
    return True


def _common_base_ref(watches: dict, due_numbers: list[int]) -> str:
    """The base ref to fetch for the per-repo probe. All watched PRs in
    one coord target the protected default branch; default to "main"
    when unset. We fetch ONE base ref per repo per tick (§3)."""
    for n in due_numbers:
        w = watches.get(str(n))
        if w and w.get("base"):
            return str(w["base"])
    return "main"


def _apply_probe(
    watches: dict,
    due_numbers: list[int],
    repo_probe: RepoProbe,
    prober: Prober,
    repo_path: str,
    *,
    now_iso: str,
    tick_count: int,
    out: WatchOutcome,
) -> None:
    """Persist each due watch's snapshot + reduce to an event. Fail-soft:
    a whole-repo transient failure (repo_probe.error) skips + RETAINS
    every due watch with a jittered backoff (§3); a per-PR transient
    failure does the same for just that PR; a definitive 404 raises hand."""
    is_anc = lambda a, d: prober.is_ancestor(repo_path, a, d)  # noqa: E731

    for n in due_numbers:
        key = str(n)
        w = watches.get(key)
        if w is None:
            continue
        w["last_probe_at"] = now_iso

        if repo_probe.error:
            # whole-repo transient failure -> skip + retain + backoff.
            _arm_backoff(w, tick_count)
            out.errors.append(
                f"pr-watch: probe for PR #{n} skipped (transient): {repo_probe.error}"
            )
            continue

        snap = repo_probe.snapshots.get(n)
        if snap is None:
            # PR absent from the batched result but the repo query
            # SUCCEEDED -> the PR genuinely no longer exists -> 404-class.
            out.raises.append(
                f"pr-watch: PR #{n} not found in repo query (pr-not-found); "
                f"needs operator attention — {w.get('pr_url', '')}"
            )
            continue

        if snap.error:
            if snap.not_found:
                out.raises.append(
                    f"pr-watch: PR #{n} not found (pr-not-found); "
                    f"needs operator attention — {w.get('pr_url', '')}"
                )
            else:
                _arm_backoff(w, tick_count)
                out.errors.append(
                    f"pr-watch: probe for PR #{n} skipped (transient): {snap.error}"
                )
            continue

        # mergeability ancestor check uses the FRESH tip of the PR's OWN
        # base ref the fetch captured — never a stale local ref, and never
        # the wrong base for a stacked PR (§5.1a / #199 + codex iter-3 [P2]).
        pr_base_sha = repo_probe.base_sha_for(snap.base_ref_name)

        # Fetch-failure handling (codex iter-4 [P2]): the GraphQL query
        # succeeded but the git fetch failed, so we have NO fresh base for
        # this PR's mergeability check. An OPEN PR whose only remaining
        # classification depends on the ancestor check would silently
        # reduce to plain OPEN forever (hiding READY/STALE transitions)
        # while re-fetching every due tick. Treat that as a transient
        # SKIP: RETAIN the watch, back off, surface — same as a probe
        # failure. We still RECORD terminal/CI/conflict events that DON'T
        # need the base check (MERGED/CLOSED/CI-fail/DIRTY/BEHIND/
        # CHANGES_REQUESTED are decided before the ancestor step in
        # reduce_snapshot), so a fetch blip never hides a real merge.
        pre = reduce_snapshot(snap, fresh_base_sha="__present__", is_ancestor=lambda a, d: True)
        needs_base_check = pre in (EVENT_READY, EVENT_STALE, EVENT_OPEN)
        if repo_probe.fetch_error and not pr_base_sha and needs_base_check:
            _arm_backoff(w, tick_count)
            out.errors.append(
                f"pr-watch: probe for PR #{n} skipped (transient git fetch): "
                f"{repo_probe.fetch_error}"
            )
            continue

        # successful probe -> clear any backoff, reduce, persist snapshot.
        w.pop("probe_skip_until_tick", None)
        w["_backoff_n"] = 0
        out.probed += 1

        head_contains_base = False
        if pr_base_sha and snap.head_ref_oid:
            head_contains_base = is_anc(pr_base_sha, snap.head_ref_oid)

        event = reduce_snapshot(
            snap, fresh_base_sha=pr_base_sha, is_ancestor=is_anc,
        )

        w["last_snapshot"] = {
            "pr_state": (snap.pr_state or "").upper(),
            "merge_state_status": (snap.merge_state_status or "").upper(),
            "review_decision": (snap.review_decision or "").upper(),
            "checks": (snap.checks or "").upper(),
            "head_ref_oid": snap.head_ref_oid,
            "base_ref_oid": snap.base_ref_oid,
            "is_draft": snap.is_draft,
            "up_to_date": head_contains_base,
        }
        w["last_event"] = event

        if event == EVENT_MERGED:
            w["state"] = STATE_MERGED
            # terminal-handling (flip + prune) runs in the §5 pass.
        elif event == EVENT_CLOSED_UNMERGED:
            if w.get("state") != STATE_CLOSED_UNMERGED:
                w["_just_closed"] = True
            w["state"] = STATE_CLOSED_UNMERGED
        else:
            # OPEN sub-state. last_seen_at advances; stays watched.
            w["state"] = STATE_OPEN
            w["last_seen_at"] = now_iso
            if event == EVENT_READY:
                out.notes.append(f"pr-watch: #{n} is mergeable (READY)")
            elif event in _ACTIONABLE_EVENTS:
                # PR1 SURFACES the actionable event (raise-hand /
                # diagnostic). PR2 dispatches the fixer here.
                out.raises.append(
                    f"pr-watch: PR #{n} is {event.upper()} and needs a fix "
                    f"(PR1 surfaces only; auto-fix lands in PR2) — {w.get('pr_url', '')}"
                )


def _arm_backoff(watch: dict, tick_count: int) -> None:
    """Set probe_skip_until_tick with a capped exponential backoff + a
    deterministic jitter derived from the PR number (no wall-clock /
    random so tests stay deterministic, per the no-time.Sleep rule)."""
    n = int(watch.get("_backoff_n", 0) or 0) + 1
    watch["_backoff_n"] = n
    delay = min(_BACKOFF_BASE_TICKS * (2 ** (n - 1)), _BACKOFF_MAX_TICKS)
    # jitter in [0, delay) derived from pr_number so two PRs failing on
    # the same tick don't re-probe in lockstep. Deterministic.
    try:
        pr_num = int(watch.get("pr_number", 0) or 0)
    except (TypeError, ValueError):
        pr_num = 0
    jitter = (pr_num % max(delay, 1)) if delay > 0 else 0
    watch["probe_skip_until_tick"] = tick_count + delay + jitter


# ----------------------------------------------------------------------
# Production prober — shells to git + gh (one fetch + one batched query)
# ----------------------------------------------------------------------

# The per-PR projection, reused inside each alias of the BATCHED query so
# one `gh api graphql` call covers every watched PR in the repo (§3 cost
# model — one fetch + one batched query per repo per tick).
_PR_FIELDS = """{
      number state mergedAt mergeStateStatus reviewDecision isDraft
      headRefName baseRefName headRefOid baseRefOid
      commits(last:1){nodes{commit{statusCheckRollup{state contexts(first:100){nodes{
        __typename
        ... on CheckRun{status conclusion}
        ... on StatusContext{state}
      }}}}}}
    }"""


def _build_batched_query(pr_numbers: list[int]) -> str:
    """Build ONE GraphQL query aliasing each watched PR as `prN<num>` so a
    single `gh api graphql` call returns every PR (codex iter-2 [P2])."""
    aliases = "\n".join(
        f"    pr{n}: pullRequest(number:{n}) {_PR_FIELDS}"
        for n in pr_numbers
    )
    return (
        "query($owner:String!,$name:String!){\n"
        "  repository(owner:$owner,name:$name){\n"
        f"{aliases}\n"
        "  }\n"
        "}\n"
    )


class GhGitProber:
    """Production Prober: one `git fetch` + one batched `gh api graphql`
    per repo per tick (§3). Fail-soft — every shell-out returns through
    RepoProbe.error / PRSnapshot.error rather than raising, so a transient
    gh/git failure can never prune or flip a watch.
    """

    def __init__(self, timeout_s: float = 20.0) -> None:
        self.timeout_s = timeout_s

    def probe_repo(
        self,
        repo_path: str,
        owner_repo: str,
        base_ref: str,
        pr_numbers: list[int],
        head_oids: list[str],
    ) -> RepoProbe:
        result = RepoProbe()
        owner, _, name = owner_repo.partition("/")
        if not owner or not name:
            result.error = f"unparseable owner/repo {owner_repo!r}"
            return result

        # 1. Query the PRs FIRST, in ONE batched `gh api graphql` call
        # (codex iter-2 [P2] — one shell-out per repo, not O(PRs)). The
        # §5.1(a) ancestor check needs the CURRENT head OID (which lives
        # on GitHub), not last tick's snapshot — so we must learn the
        # current heads from GraphQL before we fetch them (codex iter-1
        # [P1]). `head_oids` (prior snapshot heads) is still passed for
        # the cheap "is it already local?" pre-filter; the authoritative
        # head set comes from here.
        result.snapshots = self._query_batch(owner, name, pr_numbers)

        # 2. ONE git fetch of EVERY distinct base ref the PRs ACTUALLY
        # target (codex iter-3 [P2]: a stacked PR's base may differ;
        # codex iter-5 [P2]: derive bases from the PRs' real baseRefName,
        # NOT a hard-coded `main` hint — fetching a synthetic `main` that
        # doesn't exist fails the WHOLE fetch and starves every PR) +
        # every current head OID not already local, so the ancestor check
        # runs against FRESH, locally-present SHAs (codex iter-1 [P1]: a
        # plain `git fetch origin main` leaves the tip in FETCH_HEAD and
        # never fetches the PR heads). One fetch shell-out per repo.
        base_refs = sorted({
            s.base_ref_name for s in result.snapshots.values() if s.base_ref_name
        })
        # Fallback to the caller's hint ONLY when no PR reported a base
        # (e.g. every PR 404'd) — keeps the legacy single-base path working.
        if not base_refs and base_ref:
            base_refs = [base_ref]
        if repo_path and base_refs:
            want_heads = [
                s.head_ref_oid for s in result.snapshots.values()
                if s.head_ref_oid and not self._object_present(repo_path, s.head_ref_oid)
            ]
            base_refspecs = [
                f"refs/heads/{r}:refs/remotes/origin/{r}" for r in base_refs
            ]
            fetch_cmd = ["git", "-C", repo_path, "fetch", "origin", *base_refspecs, *want_heads]
            try:
                proc = subprocess.run(
                    fetch_cmd, capture_output=True, text=True,
                    timeout=self.timeout_s, check=False,
                )
                result.fetch_ok = proc.returncode == 0
                if proc.returncode == 0:
                    for r in base_refs:
                        sha = self._rev_parse(repo_path, f"refs/remotes/origin/{r}")
                        if sha:
                            result.fresh_base_shas[r] = sha
                    # default base sha = the caller's hint if fetched, else
                    # any fetched base (per-PR checks use base_sha_for()).
                    result.fresh_base_sha = (
                        result.fresh_base_shas.get(base_ref)
                        or next(iter(result.fresh_base_shas.values()), "")
                    )
                else:
                    # transient fetch failure -> surface so _apply_probe
                    # backs off instead of recording a misleading OPEN
                    # (codex iter-4 [P2]).
                    result.fetch_error = (proc.stderr or proc.stdout or "").strip() or \
                        f"git fetch exit {proc.returncode}"
            except (FileNotFoundError, subprocess.TimeoutExpired, OSError) as exc:
                # transient — record the error so the watch backs off +
                # retries rather than silently reducing to plain OPEN.
                result.fetch_ok = False
                result.fetch_error = str(exc)
        return result

    def _object_present(self, repo_path: str, oid: str) -> bool:
        """True if `oid` is already a local git object (skip re-fetching)."""
        try:
            proc = subprocess.run(
                ["git", "-C", repo_path, "cat-file", "-e", f"{oid}^{{commit}}"],
                capture_output=True, text=True, timeout=self.timeout_s, check=False,
            )
        except (FileNotFoundError, subprocess.TimeoutExpired, OSError):
            return False
        return proc.returncode == 0

    def _query_batch(self, owner: str, name: str, pr_numbers: list[int]) -> dict[int, PRSnapshot]:
        """ONE `gh api graphql` call for all watched PRs (codex iter-2
        [P2]). On a whole-call failure (auth/5xx/timeout/rate-limit) EVERY
        PR gets a transient error snapshot (fail-soft — retain + retry).
        A single PR aliased to `null` (404-class) gets a not_found snap
        while the rest parse normally."""
        if not pr_numbers:
            return {}
        cmd = [
            "gh", "api", "graphql",
            "-f", f"query={_build_batched_query(pr_numbers)}",
            "-F", f"owner={owner}",
            "-F", f"name={name}",
        ]
        try:
            proc = subprocess.run(
                cmd, capture_output=True, text=True,
                timeout=self.timeout_s, check=False,
            )
        except (FileNotFoundError, subprocess.TimeoutExpired, OSError) as exc:
            return {n: PRSnapshot(number=n, error=str(exc)) for n in pr_numbers}
        if proc.returncode != 0:
            stderr = (proc.stderr or proc.stdout or "").strip()
            not_found = classify_probe_error(stderr) == EVENT_NOT_FOUND
            return {
                n: PRSnapshot(number=n, error=stderr or "gh nonzero exit",
                              not_found=not_found)
                for n in pr_numbers
            }
        try:
            data = json.loads(proc.stdout or "null")
        except json.JSONDecodeError as exc:
            return {n: PRSnapshot(number=n, error=f"json decode: {exc}") for n in pr_numbers}

        # GraphQL fail-soft (codex iter-9 [P2]): `gh api graphql` can exit
        # 0 with HTTP 200 yet carry a top-level `errors` array (rate-limit
        # / RATE_LIMITED, FORBIDDEN/auth-scope, transient resolution
        # errors) AND null data. Treating a null alias as not_found in that
        # case raises FALSE `pr-not-found` alerts. So: collect the error
        # `type`s; a null alias is NOT_FOUND-class ONLY when the errors
        # array is empty OR explicitly carries a NOT_FOUND type — otherwise
        # it's transient (SKIP + backoff, retain the watch).
        gql_errors = data.get("errors") if isinstance(data, dict) else None
        err_types = set()
        err_summary = ""
        if isinstance(gql_errors, list) and gql_errors:
            for e in gql_errors:
                if isinstance(e, dict):
                    err_types.add(str(e.get("type", "") or "").upper())
            err_summary = "; ".join(
                str(e.get("message", "")) for e in gql_errors if isinstance(e, dict)
            )[:300]
        has_transient_error = bool(err_types) and err_types != {"NOT_FOUND"}

        repo = (data.get("data") or {}).get("repository") if isinstance(data, dict) else None
        out: dict[int, PRSnapshot] = {}
        for n in pr_numbers:
            pr = repo.get(f"pr{n}") if isinstance(repo, dict) else None
            out[n] = self._snapshot_from_pr(
                n, pr, has_transient_error=has_transient_error,
                err_summary=err_summary,
            )
        return out

    @staticmethod
    def _snapshot_from_pr(num: int, pr, *, has_transient_error: bool = False,
                          err_summary: str = "") -> PRSnapshot:
        if pr is None:
            # alias resolved to null. If the response carried a transient
            # top-level error (rate-limit/auth/etc.), this is NOT a real
            # 404 -> SKIP + backoff (retain). Only an error-free (or
            # explicitly NOT_FOUND) null is treated as genuinely gone.
            if has_transient_error:
                return PRSnapshot(
                    number=num,
                    error=f"graphql transient error: {err_summary or 'unknown'}",
                    not_found=False,
                )
            return PRSnapshot(number=num, error="pullRequest is null", not_found=True)
        rollup_state = ""
        contexts = []
        try:
            nodes = pr["commits"]["nodes"]
            if nodes:
                rollup = nodes[0]["commit"]["statusCheckRollup"]
                if rollup:
                    rollup_state = str(rollup.get("state", "") or "")
                    ctx = rollup.get("contexts", {})
                    contexts = ctx.get("nodes", []) if isinstance(ctx, dict) else []
        except (TypeError, KeyError, IndexError):
            pass
        # prefer the rollup `state` if present, else derive from contexts.
        checks = _rollup_state_to_verdict(rollup_state) if rollup_state else _checks_from_rollup(contexts)
        return PRSnapshot(
            number=int(pr.get("number", num) or num),
            pr_state=str(pr.get("state", "") or ""),
            merged_at=(str(pr["mergedAt"]) if pr.get("mergedAt") else None),
            merge_state_status=str(pr.get("mergeStateStatus", "") or ""),
            review_decision=str(pr.get("reviewDecision", "") or ""),
            head_ref_oid=str(pr.get("headRefOid", "") or ""),
            base_ref_oid=str(pr.get("baseRefOid", "") or ""),
            head_ref_name=str(pr.get("headRefName", "") or ""),
            base_ref_name=str(pr.get("baseRefName", "") or ""),
            is_draft=bool(pr.get("isDraft", False)),
            checks=checks,
        )

    def _rev_parse(self, repo_path: str, ref: str) -> str:
        try:
            proc = subprocess.run(
                ["git", "-C", repo_path, "rev-parse", ref],
                capture_output=True, text=True, timeout=self.timeout_s, check=False,
            )
        except (FileNotFoundError, subprocess.TimeoutExpired, OSError):
            return ""
        return proc.stdout.strip() if proc.returncode == 0 else ""

    def is_ancestor(self, repo_path: str, ancestor_sha: str, descendant_sha: str) -> bool:
        """git merge-base --is-ancestor on FRESH refs (§5.1a). Returns
        False on any failure (missing ref, git error) — fail-soft: an
        un-provable ancestor is treated as 'not contained' so we never
        falsely assert READY/mergeable on a stale or missing ref."""
        if not repo_path or not ancestor_sha or not descendant_sha:
            return False
        try:
            proc = subprocess.run(
                ["git", "-C", repo_path, "merge-base", "--is-ancestor",
                 ancestor_sha, descendant_sha],
                capture_output=True, text=True, timeout=self.timeout_s, check=False,
            )
        except (FileNotFoundError, subprocess.TimeoutExpired, OSError):
            return False
        return proc.returncode == 0


def _rollup_state_to_verdict(state: str) -> str:
    s = (state or "").upper()
    if s in ("SUCCESS",):
        return "SUCCESS"
    if s in ("PENDING", "EXPECTED"):
        return "PENDING"
    if s in ("FAILURE", "ERROR"):
        return "FAILURE"
    return "SUCCESS"


# ----------------------------------------------------------------------
# Coord-own-repo derivation (coord-scope assert source, §2)
# ----------------------------------------------------------------------

# Owner is a path segment (no slash); repo allows dots (GitHub repo names
# may contain `.`, e.g. `owner/foo.bar`). We strip an optional trailing
# `.git` + `/` FIRST (below), then this matches the last two segments so a
# dotted repo name is preserved (codex iter-1 [P2]).
_REMOTE_URL_RE = re.compile(
    r"github\.com[:/]+(?P<owner>[^/]+)/(?P<repo>[^/]+?)$"
)


def derive_owner_repo(repo_path: str, *, timeout_s: float = 5.0) -> str | None:
    """Return the coord's own "<owner>/<repo>" from `git remote get-url
    origin` at repo_path, or None when it can't be derived (no path, no
    remote, parse failure). None => coord-scope can't be proven; the
    caller refuses to probe (strict scope)."""
    if not repo_path:
        return None
    try:
        proc = subprocess.run(
            ["git", "-C", repo_path, "remote", "get-url", "origin"],
            capture_output=True, text=True, timeout=timeout_s, check=False,
        )
    except (FileNotFoundError, subprocess.TimeoutExpired, OSError):
        return None
    if proc.returncode != 0:
        return None
    url = proc.stdout.strip()
    # Strip an optional trailing `.git` and slash BEFORE matching so a
    # dotted repo name (`owner/foo.bar`) survives — `.git` is a suffix,
    # not part of the repo name (codex iter-1 [P2]).
    url = url.rstrip("/")
    if url.endswith(".git"):
        url = url[: -len(".git")]
    m = _REMOTE_URL_RE.search(url)
    if not m:
        return None
    # lowercase: GitHub owner/repo is case-insensitive, so the coord-scope
    # comparison must match a differently-cased PR URL (codex iter-6 [P2]).
    return f"{m.group('owner')}/{m.group('repo')}".lower()


def utc_now_iso(now: _dt.datetime | None = None) -> str:
    """UTC ISO-8601 with a trailing Z (matches the rest of the skill)."""
    dt = now or _dt.datetime.now(tz=_dt.timezone.utc)
    return dt.isoformat().replace("+00:00", "Z")
