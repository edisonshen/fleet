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
# Parked: the PR is definitively GONE (a 404 / deleted PR). Like
# CLOSED_UNMERGED, it's retained for operator ack and raises ONLY on the
# transition (never re-queried/re-raised every tick — codex iter-14 [P2]).
STATE_NOT_FOUND = "not-found"
_TERMINAL_PR_STATES = frozenset({STATE_MERGED, STATE_CLOSED_UNMERGED})
# States that should NOT be probed/re-queried: MERGED is irreversible;
# NOT_FOUND is parked until ack; CLOSED_UNMERGED is handled separately
# (re-probed iff it still has live tasks, for reopen support, iter-8).
_NO_PROBE_STATES = frozenset({STATE_MERGED, STATE_NOT_FOUND})

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

# Events that are remedied by a REBASE (auto-rebase the uniquely-eligible
# next-to-merge PR, §5.1b/c). STALE = head missing fresh base under strict
# protection; BEHIND/DIRTY = GitHub's stale/conflict words. All three mean
# "the head must contain a fresher base"; the rebase subagent resolves it.
_REBASE_EVENTS = frozenset({EVENT_STALE, EVENT_BEHIND, EVENT_DIRTY})
# Events remedied by a FIX subagent (re-run/repair CI, apply review fixes).
_FIX_EVENTS = frozenset({EVENT_CI_FAILED, EVENT_CHANGES_REQUESTED})


# ----------------------------------------------------------------------
# §6 crash-safe action leases — outcome, not boolean
# ----------------------------------------------------------------------
#
# A naive "dispatch then persist a flag" is NOT crash-safe (design §6): a
# crash after launching but before persisting wedges/duplicates, and an
# existence-only flag suppresses a NEEDED retry forever if the fixer dies
# before pushing. So an action is a LEASE with an OUTCOME persisted into
# the watch's `inflight_action`, and the lease key + agent_id are written
# BEFORE any launchable DISPATCH block is emitted (commit-then-emit, the
# same ordering the merged dispatch chokepoint enforces for workers).
#
#   planned -> prompt_acquired -> launch_emitted -> running
#                                                      |
#                          terminal{succeeded@<head> | blocked | failed_launch}
#
# Suppression is by OUTCOME (not existence):
#   running   + agent alive  -> suppress (someone's on it)
#   succeeded@<current-head>  -> suppress (already handled THIS head)
#   failed_launch             -> RETRY (it never actually ran)
#   blocked                   -> raise-hand; require operator ack (one
#                                attempt per <headSHA,baseSHA>); no silent
#                                re-fire
# Reclaim (every tick): a `running` lease whose agent_id is provably dead
# (dead pid / missing agent record) is cleared -> eligible to re-dispatch
# (closes the "fixer died silently -> PR unwatched-for-action forever"
# hole). LIVENESS, not age, is the reclaim authority — a provably-alive
# agent is NEVER force-expired (codex iter-11 [P1]: a long-running
# fixer/rebaser must not get a second agent into its branch).
# `dispatched_events` keys whose head-SHA != current head are pruned on
# each persist so the file stays bounded.

ACTION_REBASE = "rebase"
ACTION_FIX = "fix"

OUTCOME_RUNNING = "running"
OUTCOME_BLOCKED = "blocked"
OUTCOME_FAILED_LAUNCH = "failed_launch"
# Reported by the tick's agent_outcome callback (not a persisted lease
# outcome): the journal says the Agent was launched but no live agent record
# confirms liveness (Agent-tool subagents expose no pid). Suppress like
# running, but reclaimable past a generous stale bound (§6 / codex iter-20).
OUTCOME_RUNNING_UNVERIFIED = "running-unverified"
_OUTCOME_SUCCEEDED_PREFIX = "succeeded@"

# Reference "long-lived lease" tick magnitude. NOTE (codex iter-11 [P1]):
# liveness — NOT age — is the lease-reclaim authority. A lease is reclaimed
# only when its agent is provably GONE (dead pid / missing record); a
# provably-ALIVE agent is never force-expired on age, because a legitimate
# fixer/rebaser (gates + multi-round review) can run longer than any age
# budget and force-expiring it would launch a SECOND agent into the same
# branch. This constant is retained for the persisted `leased_tick`
# bookkeeping + as a test magnitude, not as a reclaim trigger.
_LEASE_EXPIRY_TICKS = 12

# Startup grace (codex iter-12 [P1]): a just-launched subagent may not have
# written its agent record (~/.fleet/agents/<id>.json) yet, so the very next
# tick's liveness probe reads "gone". Within this many ticks of `leased_tick`
# a gone/missing record is treated as STILL PENDING (not reclaimed), so the
# normal async launch window can't trigger a duplicate dispatch into the same
# branch. After the grace, a still-gone agent is genuinely dead -> reclaimed.
_LEASE_LAUNCH_GRACE_TICKS = 3

# Generous stale bound for an UNVERIFIED-running lease (codex iter-20 [P1]):
# a PR-watch Agent-tool subagent whose journal says launched but that wrote
# no Fleet agent record (so liveness is unprovable). A genuinely-working
# fixer/rebaser moves the head (push) or sets BLOCKED well before this; a
# launch that never started (broken stdout / crash after
# mark-launch-attempted) would otherwise wedge the lease forever. So past
# this many ticks with NO head move + still no agent record, reclaim +
# retry. Much larger than the launch grace (real work takes many polls:
# fetch + rebase + full gates + multi-round review), small enough to not
# strand a PR for an unbounded time. Tick-budget (deterministic).
_LEASE_STALE_LAUNCH_TICKS = 40


def _succeeded_outcome(head_sha: str) -> str:
    return f"{_OUTCOME_SUCCEEDED_PREFIX}{head_sha}"


def _succeeded_head(outcome: str) -> str | None:
    """Return the head SHA a `succeeded@<sha>` outcome was recorded for,
    or None if `outcome` isn't a succeeded marker."""
    if isinstance(outcome, str) and outcome.startswith(_OUTCOME_SUCCEEDED_PREFIX):
        return outcome[len(_OUTCOME_SUCCEEDED_PREFIX):]
    return None


def lease_key(kind: str, event: str, *, head_sha: str, base_sha: str = "") -> str:
    """Build the idempotency key for one action lease (§6).

    rebase: `rebase:<baseSHA>:<headSHA>` — one attempt per (base, head)
            pair, so a fresh base (an upstack merge moved origin/main) or
            a moved head re-enables a rebase, but the SAME pair never
            re-fires (the blocked latch is keyed on this).
    fix:    `fix:<headSHA>:<event>` — keyed on the head + the failing
            signal, so a NEW push (head change) re-enables a fix, and a
            DIFFERENT event on the same head (CI-fail vs changes-requested)
            is its own key.
    """
    if kind == ACTION_REBASE:
        return f"{ACTION_REBASE}:{base_sha}:{head_sha}"
    return f"{ACTION_FIX}:{head_sha}:{event}"


def _prune_dispatched_events(watch: dict, current_head: str) -> None:
    """Drop `dispatched_events` keys whose head-SHA != the current head
    (§6 — keep the file bounded). A rebase key embeds the head as its LAST
    colon-segment; a fix key embeds it as its SECOND segment. We keep a key
    iff the current head appears in it, so an obsolete head's keys are
    pruned the moment the head advances."""
    de = watch.get("dispatched_events")
    if not isinstance(de, dict) or not current_head:
        return
    kept = {}
    for k, v in de.items():
        # rebase:<base>:<head>  or  fix:<head>:<event>
        parts = k.split(":")
        head_in_key = ""
        if k.startswith(ACTION_REBASE + ":") and len(parts) >= 3:
            head_in_key = parts[-1]
        elif k.startswith(ACTION_FIX + ":") and len(parts) >= 2:
            head_in_key = parts[1]
        if head_in_key == current_head:
            kept[k] = v
    watch["dispatched_events"] = kept


def _action_suppressed(
    watch: dict,
    *,
    key: str,
    current_head: str,
) -> bool:
    """Should re-dispatch of `key` be SUPPRESSED this tick? (§6 outcome-
    based suppression — NOT existence.)

    Called AFTER _reclaim_lease has already resolved (cleared / moved to
    ledger) any stale `running` lease, so the live `inflight_action` here
    is either absent or a genuinely-in-flight action. Checks both the live
    lease and the terminal `dispatched_events` ledger:
      - running (same key, post-reclaim => alive) -> suppress
      - succeeded@<current-head>                  -> suppress (handled)
      - succeeded@<other-head>                    -> NOT suppressed
      - blocked                                   -> suppress auto-redispatch
                                                     (caller raises once)
      - failed_launch / absent                    -> NOT suppressed (retry)
    """
    de = watch.get("dispatched_events")
    if isinstance(de, dict):
        recorded = de.get(key)
        if recorded == OUTCOME_BLOCKED:
            return True
        sh = _succeeded_head(recorded) if isinstance(recorded, str) else None
        if sh is not None and sh == current_head:
            return True

    inflight = watch.get("inflight_action")
    if not isinstance(inflight, dict) or inflight.get("key") != key:
        return False
    outcome = inflight.get("outcome")
    if outcome == OUTCOME_RUNNING:
        # reclaim already ran this tick; a surviving running lease on this
        # key is genuinely in flight -> suppress.
        return True
    if outcome == OUTCOME_BLOCKED:
        return True
    if outcome == OUTCOME_FAILED_LAUNCH:
        return False
    sh = _succeeded_head(outcome) if isinstance(outcome, str) else None
    if sh is not None:
        return sh == current_head
    return False


def _lease_head(key: str) -> str:
    """Extract the head SHA a lease key was minted against (rebase key's
    last segment, fix key's second segment), or "" if unparseable."""
    if not isinstance(key, str):
        return ""
    parts = key.split(":")
    if key.startswith(ACTION_REBASE + ":") and len(parts) >= 3:
        return parts[-1]
    if key.startswith(ACTION_FIX + ":") and len(parts) >= 2:
        return parts[1]
    return ""


def _reclaim_lease(
    watch: dict,
    *,
    tick_count: int,
    current_head: str,
    agent_alive: Callable[[str], bool],
    agent_outcome: Callable[[str], str],
) -> tuple[str | None, str | None]:
    """Resolve the watch's in-flight lease (§6). Clears `inflight_action`
    so a re-dispatch can re-lease, moving terminal outcomes to the durable
    `dispatched_events` ledger. Returns (note, raise) — `note` is an
    informational breadcrumb, `raise` is a once-only operator alert (set
    when an action BLOCKED).

    Outcome resolution for a `running` lease, in priority order:
      1. agent reports BLOCKED (agent_outcome == blocked) -> ledger
         blocked + RAISE (one attempt per <head,base> key; never re-fired).
      2. HEAD MOVED (lease's target head != current) -> the fixer/rebaser
         pushed (or a human advanced the head); record succeeded@<oldhead>
         + free the slot.
      3. agent DEAD (not alive) and head UNCHANGED and not blocked -> the
         action died before pushing -> reclaim (clear) so it re-dispatches.
      4. lease EXPIRED (no liveness signal for too long) -> reclaim.
      5. otherwise still running -> leave it.

    `agent_outcome(agent_id)` returns "blocked" | "running" | "gone" | ""
    derived by the tick from the subagent's agent record (blocked flag /
    record presence). pr_watch never reads the record itself (shell-free).
    """
    inflight = watch.get("inflight_action")
    if not isinstance(inflight, dict):
        return None, None
    outcome = inflight.get("outcome")
    key = str(inflight.get("key", "") or "")

    # Already-terminal lease (set in-line by the dispatch path on a launch
    # failure) -> move to ledger + free the slot.
    if outcome != OUTCOME_RUNNING:
        if outcome and key:
            de = watch.setdefault("dispatched_events", {})
            if isinstance(de, dict):
                de[key] = outcome
        watch["inflight_action"] = None
        return None, None

    agent_id = str(inflight.get("agent_id", "") or "")
    reported = agent_outcome(agent_id) if agent_id else "gone"
    lease_head = _lease_head(key)
    head_moved = bool(current_head and lease_head and lease_head != current_head)

    # 1. BLOCKED — the subagent aborted (conflict / guard abort). One
    # attempt per key: record blocked in the ledger; the dispatch pass's
    # suppression branch owns the (once-only) raise off the ledger, so we
    # don't double-raise here.
    if reported == OUTCOME_BLOCKED and not head_moved:
        de = watch.setdefault("dispatched_events", {})
        if isinstance(de, dict):
            de[key] = OUTCOME_BLOCKED
        watch["inflight_action"] = None
        return None, None

    # 1b. AGENT STILL ALIVE -> ALWAYS suppress, even if the head moved
    # (codex iter-24 [P2]). A head move while the fixer is still alive can
    # be an EXTERNAL push (human / other automation) — retiring the lease as
    # succeeded here would let _dispatch_actions launch a SECOND agent into
    # the same branch checkout the first still owns (both prompts work in
    # the existing checkout -> they'd race). Wait for the agent to exit; the
    # head-move-as-success retirement below only fires once it's gone. A
    # provably-running record (record + pid) is the only case treated as
    # "definitely still alive" here — running-unverified falls through so
    # its stale bound can still reclaim a never-started launch.
    if reported == OUTCOME_RUNNING:
        return None, None

    # 2. HEAD MOVED (and the agent is NOT provably-alive) -> success: our
    # fixer pushed + exited, or an external push advanced the head past a
    # dead/finished agent. Retire the lease succeeded@<oldhead>.
    if head_moved:
        de = watch.setdefault("dispatched_events", {})
        if isinstance(de, dict):
            de[key] = _succeeded_outcome(lease_head)
        watch["inflight_action"] = None
        return f"retired succeeded lease {key!r} (head advanced to {current_head})", None

    # 3. agent gone (dead) + head unchanged + not blocked -> died pre-push.
    # BUT honor a STARTUP GRACE (codex iter-12 [P1]): a just-launched
    # subagent may not have written its agent record yet, so the next tick
    # reads "gone". Within _LEASE_LAUNCH_GRACE_TICKS of leased_tick we treat
    # gone/missing as STILL PENDING (not reclaimed) so the async launch
    # window can't double-dispatch into the same branch. After the grace, a
    # still-gone agent is genuinely dead -> reclaimed.
    if reported == "gone" or not agent_alive(agent_id):
        leased_tick = inflight.get("leased_tick")
        within_grace = (
            isinstance(leased_tick, int)
            and tick_count - leased_tick < _LEASE_LAUNCH_GRACE_TICKS
        )
        if within_grace:
            return None, None  # pending startup; keep the lease, suppress
        watch["inflight_action"] = None
        return f"reclaimed dead-agent lease {key!r} (agent {agent_id} not alive)", None

    # 3b. UNVERIFIED-running (codex iter-20 [P1]): the journal says the Agent
    # was launched but there's no live agent record to confirm it's alive
    # (Agent subagents expose no pid). Suppress re-dispatch like `running`,
    # but reclaim if the head HASN'T moved after a GENEROUS stale bound — a
    # working fixer would have pushed (head move) / BLOCKED by then, so a
    # still-stuck launch_attempted with no progress is a launch that never
    # actually started (broken stdout / crash after mark-launch-attempted).
    if reported == OUTCOME_RUNNING_UNVERIFIED:
        leased_tick = inflight.get("leased_tick")
        stale = (
            isinstance(leased_tick, int)
            and tick_count - leased_tick >= _LEASE_STALE_LAUNCH_TICKS
        )
        if stale:
            watch["inflight_action"] = None
            return (
                f"reclaimed stale unverified-launch lease {key!r} "
                f"(no head move + no agent record after {_LEASE_STALE_LAUNCH_TICKS} ticks)"
            ), None
        return None, None  # launched, in flight (unverifiable) -> hold

    # 4. The agent is PROVABLY ALIVE and the head hasn't moved -> it is still
    # working. NEVER expire a live lease on age alone (codex iter-11 [P1]): a
    # legitimately long-running fixer/rebaser (gates + multi-round review can
    # exceed a few coord polls) would otherwise get its lease cleared and a
    # SECOND agent launched into the same branch, racing force-with-lease
    # pushes. Liveness is the authority; the only age-based reclaim is the
    # `gone`/dead path above (a lost liveness signal -> reclaim). So a live
    # lease simply stays running -> suppressed.
    return None, None


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
    # PR numbers whose head OID is locally PRESENT after the fetch — the
    # ancestor check is trustworthy only for these. A PR whose head fetch
    # failed (transient / unavailable fork ref) is absent here, so
    # _apply_probe treats its base-dependent classification as transient
    # rather than false-STALE (codex iter-12 [P2]).
    head_present: set = field(default_factory=set)
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
#
# A *transient* failure must NEVER prune a watch or flip it terminal — we
# skip + retain + retry. A *definitive* 404 means the PR is genuinely gone
# — raise-hand `pr-not-found` + park (STATE_NOT_FOUND). The fail-soft bias
# is strict: we only treat a PR as a genuine 404 when GraphQL returns a
# ZERO-exit response with a null PR ALIAS under a RESOLVED repository
# (see GhGitProber._query_batch). A nonzero `gh` exit, a null repository,
# or any ambiguous/auth/scope/rate-limit error is TRANSIENT — never
# parked, so tracking recovers once the blip / auth clears.


# ----------------------------------------------------------------------
# pr_url parsing
# ----------------------------------------------------------------------

# The path portion of a GitHub PR URL, once the host is validated:
# <owner>/<repo>/pull/<N> with an optional trailing slash/query/fragment.
_PR_PATH_RE = re.compile(
    r"^/?(?P<owner>[^/]+)/(?P<repo>[^/]+)/pull/(?P<num>\d+)(?:[/?#].*)?$"
)
# scp-style git remote: [git@]github.com:owner/repo[/pull/N]. urllib can't
# parse this shape, so we match it explicitly. The host is anchored at the
# start (after an optional `user@`), so a path-embedded `@github.com` can't
# masquerade as the host (codex round 28 [P2]).
_SCP_RE = re.compile(r"^(?:[^/@]+@)?github\.com:(?P<path>.+)$")


def parse_pr_url(url: str) -> tuple[str, int] | None:
    """Return (owner_repo, pr_number) parsed from a GitHub PR URL, or None.

    Validates that `github.com` is the actual URL HOST (via urllib for
    scheme URLs, or an anchored scp-style match) — NOT a lookalike host
    (`notgithub.com`) or a path segment (`.../github.com/...`,
    `...foo@github.com/...`) (codex iter-19 / round 27 / round 28 [P2]).
    Only a genuine GitHub PR may drive task reconciliation.

    owner_repo is the canonical "<owner>/<repo>" the coord-scope assert
    (§2) compares against the coord's own repo. GitHub owner/repo names
    are CASE-INSENSITIVE, so we lowercase here — and derive_owner_repo
    does the same (codex iter-6 [P2]).
    """
    if not url:
        return None
    from urllib.parse import urlparse

    path: str | None = None
    parsed = urlparse(url.strip())
    if parsed.scheme and parsed.hostname is not None:
        # scheme URL (https/http/ssh/git): the HOST must be exactly
        # github.com (hostname strips userinfo + port + path).
        if parsed.hostname.lower() != "github.com":
            return None
        path = parsed.path
    else:
        # no scheme -> try scp-style `[user@]github.com:owner/repo/...`.
        m = _SCP_RE.match(url.strip())
        if m is None:
            return None
        path = "/" + m.group("path")

    mp = _PR_PATH_RE.match(path or "")
    if mp is None:
        return None
    owner_repo = f"{mp.group('owner')}/{mp.group('repo')}".lower()
    try:
        num = int(mp.group("num"))
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
    # Drop malformed individual entries (valid JSON but a non-object watch
    # like {"195": null}) so the reconcile loop's w.get(...) can't error
    # every tick (codex round 31 [P3]). The watch set is a derived
    # invariant — a dropped entry self-heals from tasks.md next tick.
    clean = {k: v for k, v in watches.items() if isinstance(v, dict)}
    return {"schema": SCHEMA_VERSION, "watches": clean}


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
        # fsync the PARENT DIRECTORY so the rename itself is durable — on
        # filesystems that require a directory fsync for rename durability,
        # a crash could otherwise lose the renamed entry (codex adversarial
        # [P2]; matches the project's "fsync before signaling" rule). Best
        # -effort: some platforms can't open a dir for fsync.
        try:
            dfd = os.open(str(parent), os.O_RDONLY)
            try:
                os.fsync(dfd)
            finally:
                os.close(dfd)
        except OSError:
            pass
    except Exception:
        try:
            os.unlink(tmp)
        except FileNotFoundError:
            pass
        raise


def persist_watches(project_dir: Path, doc: dict) -> None:
    """Publish the watch doc, OR REMOVE the file when there are no watches
    left (codex iter-19 [P2]). Leaving an empty pr-watches.json on disk
    would defeat the tick's no-PR idle early-out (it gates on the file's
    existence), so every idle tick would needlessly derive the repo +
    shell `git remote`. Removing it restores the byte-identical idle path;
    the watch set is a derived invariant, so a fresh tick recreates it the
    moment an owned PR task reappears."""
    if doc.get("watches"):
        save_watches(project_dir, doc)
        return
    path = watch_path(project_dir)
    try:
        path.unlink()
    except FileNotFoundError:
        pass
    except OSError:
        # couldn't remove (perms?) — fall back to writing the empty doc so
        # state is at least consistent; idle overhead is the lesser evil.
        save_watches(project_dir, doc)


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
    dispatched: int = 0     # PR2 auto-fix actions launched this pass (§5/§6)
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
                or state in ("PENDING", "EXPECTED") \
                or (status == "" and conclusion == "" and state == ""):
            # state=EXPECTED is a REQUIRED StatusContext that hasn't reported
            # yet (codex iter-33 [P2]) — pending, NOT success. Without this it
            # fell through to SUCCESS and could emit a false READY for a PR
            # whose required check never ran.
            pending = True
        elif conclusion in ("FAILURE", "ERROR", "CANCELLED", "TIMED_OUT", "ACTION_REQUIRED",
                            "STALE", "STARTUP_FAILURE") \
                or state in ("FAILURE", "ERROR"):
            failed = True
        # SUCCESS / NEUTRAL / SKIPPED == not failing, not pending.
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
# §5.1(b) next-to-merge eligibility — by DEPENDENCY, not PR number
# ----------------------------------------------------------------------


def _watch_head(watch: dict) -> str:
    snap = watch.get("last_snapshot")
    if isinstance(snap, dict):
        return str(snap.get("head_ref_oid", "") or "")
    return ""


def compute_rebase_eligibility(
    watches: dict,
    *,
    deps_done: Callable[[list], bool],
    base_protected: Callable[[dict], bool],
    is_ancestor: Callable[[str, str], bool],
    task_deps: Callable[[str], list],
    is_candidate: "Callable[[dict], bool] | None" = None,
) -> tuple[set, bool]:
    """Resolve which OPEN watches are rebase-eligible (§5.1b). Returns
    (eligible_pr_numbers, ambiguous).

    PR NUMBER is creation order, not stack order (parallel / backfilled /
    reopened PRs break it), so eligibility is derived from the BRANCH GRAPH
    + dependency state, never the number. A watched PR is eligible iff:

      1. all its backing tasks' `depends_on` are done, AND
      2. its base is the protected branch (`main`), AND
      3. NO other OPEN watched PR's head is an ancestor of this PR's head
         (i.e. nothing open sits BENEATH it in the branch graph).

    The caller then fires auto-rebase ONLY when exactly ONE PR is eligible:
      - zero eligible  -> do nothing (all blocked on an open upstack; a
                          downstack-stale PR is EXPECTED, not actionable).
      - one eligible   -> rebase it.
      - two+ eligible OR ambiguous -> raise-hand, DO NOT guess (returns
                          `ambiguous=True` when >=2 candidates pass the
                          per-PR gates so the caller can raise instead of
                          picking one).

    `is_ancestor(anc, desc)` runs against FRESH refs (the same prober seam
    the mergeability check uses). An ancestry we can't prove (missing ref,
    git error) reads False — fail-soft: we never assert a PR sits beneath
    another off a missing ref, so a transient outage degrades to "treat as
    independent" (the exactly-one gate then catches a >=2 ambiguity and
    raises rather than rebasing the wrong one).
    """
    # ALL open heads stay in the ancestry GRAPH for gate 3 — including
    # orphaned ones (codex iter-5 [P2]): an orphaned PR can still be the
    # UPSTACK base of a backed PR, and a downstack PR with any open PR
    # beneath it (orphaned or not) is NOT next-to-merge. But only BACKED
    # watches are eligibility CANDIDATES (orphaned/unbacked watches are
    # raised-hand, never auto-rebased), so an orphan sitting open alongside
    # a stale backed PR can't inflate the eligible set to ambiguous and
    # block the legitimate rebase.
    open_heads: dict[int, str] = {}      # the full graph (incl. orphans)
    candidate_nums: set = set()          # dispatchable PRs only
    for key, w in watches.items():
        if w.get("state") != STATE_OPEN:
            continue
        head = _watch_head(w)
        if not head:
            continue
        try:
            n = int(w.get("pr_number", key))
        except (TypeError, ValueError):
            continue
        open_heads[n] = head
        # A candidate must be backed AND DISPATCHABLE. `is_candidate`
        # (codex iter-9 [P2]) excludes a preserved CI-red-requeued watch
        # (non-empty tasks[] kept ONLY for old-PR-merge reconcile, but whose
        # backing task no longer points at this PR) — otherwise such a watch
        # would inflate the eligible set to ambiguous and spuriously block a
        # legitimate live PR's auto-rebase, even though the later
        # live_pr_backed gate would refuse to dispatch it anyway. Default
        # (no callback) keeps the prior backed-and-not-orphaned predicate.
        backed = bool(w.get("tasks")) and not w.get("orphaned")
        if backed and (is_candidate is None or is_candidate(w)):
            candidate_nums.add(n)

    eligible: set = set()
    for n in candidate_nums:
        head = open_heads[n]
        w = watches.get(str(n))
        if w is None:
            continue
        # gate 1: all backing tasks' deps done.
        deps: list = []
        for slug in w.get("tasks") or []:
            deps.extend(task_deps(slug))
        if not deps_done(deps):
            continue
        # gate 2: base is the protected branch.
        if not base_protected(w):
            continue
        # gate 3: no OTHER open watched head (orphaned or backed) is an
        # ancestor of this head.
        beneath = False
        for m, other_head in open_heads.items():
            if m == n or other_head == head:
                continue
            if is_ancestor(other_head, head):
                # `other` sits beneath `n` in the graph -> n is downstack of
                # an open PR -> not next-to-merge.
                beneath = True
                break
        if not beneath:
            eligible.add(n)

    ambiguous = len(eligible) >= 2
    return eligible, ambiguous


# ----------------------------------------------------------------------
# The reconcile invariant (§2) — the public entry point for the tick
# ----------------------------------------------------------------------


@dataclass
class ActionDispatch:
    """One auto-fix action the tick should launch (§5.1c / §5 / §6). The
    tick's `dispatch_action` callback consumes this: it mints an agent_id,
    writes the subagent's inbox prompt, emits the DISPATCH block, and
    returns the agent_id on a successful PROMPT-acquire+emit (commit the
    lease BEFORE the launchable block). A failure returns "" (the lease is
    marked failed_launch -> retried next tick)."""

    kind: str            # ACTION_REBASE | ACTION_FIX
    event: str           # the EVENT_* that triggered it (carried into the prompt)
    pr_number: int
    pr_url: str
    branch: str
    base: str
    head_sha: str        # watch-snapshot head — the rebase guard verifies it's unchanged
    base_sha: str        # fresh base tip to rebase onto (rebase only)
    key: str             # the §6 lease key (idempotency)


def reconcile_watches(
    tasks: list,
    *,
    project: str,
    project_dir: Path,
    coord_owner_repo: str | None,
    prober: Prober,
    flip_task_done: Callable[[str, str], None],
    now_iso: str,
    tick_count: int,
    slow_cadence_ticks: int = _SLOW_CADENCE_TICKS_DEFAULT,
    repo_path: str = "",
    enroll_tasks: list | None = None,
    dispatch_action: "Callable[[ActionDispatch], str] | None" = None,
    agent_outcome: "Callable[[str], str] | None" = None,
    deps_done: "Callable[[list], bool] | None" = None,
    release_action: "Callable[[str], None] | None" = None,
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
    dispatch_action / agent_outcome / deps_done: the PR2 (auto-fix) seam.
      When `dispatch_action` is provided, actionable events drive a §6-
      leased auto-rebase / fix-subagent dispatch (see _dispatch_actions);
      when None, the function behaves exactly like PR1 (surface-only).
      `agent_outcome(agent_id)` returns "blocked"|"running"|"gone"|"" from
      the subagent's agent record (lease resolution / reclaim);
      `deps_done(deps)` answers §5.1b dependency-completion. Both default
      to safe stubs when only some are supplied.
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
    # A pre-reconcile row is re-added to enrollment ONLY when its CURRENT
    # same-slug row is safe to associate with the old PR (codex iter-6 [P1]).
    # The legacy CI-red path clears a slug's pr_url + requeues it; if
    # _dispatch_ready then RE-DISPATCHED it, the current row is `in-progress`
    # with an empty pr_url — an ACTIVE retry doing live work on a NEW branch.
    # Re-adding the stale pre-reconcile row (old pr_url) would back the OLD
    # watch with that active retry, so a merge of the OLD PR would flip the
    # live retry `done`/clear its worker. So we re-add the pre-reconcile row
    # only when the current row is ABSENT (fully archived) or still an IDLE
    # `todo` (a requeued-but-not-yet-redispatched retry, safe to close on the
    # old PR's merge). An `in-progress` current row is an active retry -> NOT
    # re-added (its new PR, when opened, enrolls a fresh watch).
    current_by_slug = {getattr(t, "slug", ""): t for t in tasks}

    def _safe_to_preserve(slug: str) -> bool:
        cur = current_by_slug.get(slug)
        if cur is None:
            return True  # archived off the list -> safe to close old PR
        return getattr(cur, "status", "") == "todo"

    enroll_owned = owned + [
        t for t in _owned(enroll_tasks)
        if getattr(t, "slug", "") not in current_slugs
        and _safe_to_preserve(getattr(t, "slug", ""))
    ]
    # group enroll_owned by (owner_repo, pr_number); coord-scope assert per
    # PR. `in_scope` is every enroll_owned task that PASSES the coord-scope
    # assert — INCLUDING a pre-reconcile-only task whose pr_url the legacy
    # reconcile cleared this tick. That task IS a genuine backing task (it
    # still exists, just got requeued), so it MUST count toward `tasks[]`
    # (codex adversarial [P1]): otherwise a merge of its old PR takes the
    # empty-backing prune path and the task is left re-dispatchable after
    # its PR already merged. The coord-scope assert above already drops
    # foreign-repo tasks before they reach in_scope, so a foreign task
    # sharing a PR number can't sneak in (codex iter-1 [P2]).
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
    # added/removed/gone-terminal independently). The ORPHAN *raise* is
    # deferred until AFTER the probe (§3) so it reflects the CURRENT PR
    # state — raising here off the previous tick's snapshot would emit a
    # false orphan alert for a PR that merged/closed this tick, and a
    # slow-cadence READY watch might not even re-probe (codex iter-17 [P2]).
    # Slugs eligible to PRESERVE old-watch backing after their pr_url was
    # cleared by legacy CI-red handling. The pre-reconcile enroll snapshot
    # is only available on the clear tick; on later ticks we'd recompute
    # `live` to [] and lose the backing, so a later merge of the old PR
    # would orphan-prune the watch and leave the requeued task
    # re-dispatchable (codex round 24 [P2]).
    #
    # Eligibility is NARROW — a task qualifies ONLY when it is:
    #   - status == "todo"  (IDLE / requeued, NOT yet re-dispatched), AND
    #   - pr_url == ""       (the pure legacy-clear case).
    # Excluding non-todo statuses is load-bearing (codex round 34 [P1]): a
    # re-dispatched retry is `in-progress` with no pr_url until its NEW
    # worker opens a replacement PR. Preserving that ACTIVE attempt on the
    # OLD watch would let a merge of the old PR flip the live retry to
    # `done` and clear its worker — orphaning in-flight work. A task that
    # now points at a DIFFERENT pr_url is attached to its NEW watch, so the
    # empty-pr_url gate also keeps it off the old watch (codex round 25
    # [P1]).
    preservable_slugs = {
        getattr(t, "slug", "")
        for t in tasks
        if getattr(t, "status", "") == "todo"
        and not getattr(t, "pr_url", "")
        and getattr(t, "slug", "")
    }
    for key, w in watches.items():
        try:
            pr_num = int(w.get("pr_number", key))
        except (TypeError, ValueError):
            continue
        live = {
            getattr(t, "slug", "")
            for t in in_scope
            if (parse_pr_url(getattr(t, "pr_url", "")) or (None, None))[1] == pr_num
            and getattr(t, "slug", "")
        }
        # PRESERVE persisted backing slugs that still exist as a live task
        # but lost their pr_url (legacy-reconcile clear). A slug that no
        # longer exists as a live task (archived / went terminal) is NOT
        # preserved -> it correctly drops out (orphan/cleanup paths handle
        # it). MERGED/NOT_FOUND watches don't preserve (terminal handling
        # owns them).
        if w.get("state") not in (STATE_MERGED, STATE_NOT_FOUND):
            for slug in w.get("tasks") or []:
                if slug in preservable_slugs:
                    live.add(slug)
        live = sorted(live)
        w["tasks"] = live
        if live:
            # re-acquired a backing task -> clear any orphan flag.
            w["orphaned"] = False

    # --- 3+4. PROBE per repo (coord owns exactly one repo) + reduce.
    # Only probe watches that are (a) in scope, (b) not already terminal,
    # (c) due this tick (adaptive cadence + transient backoff).
    if coord_owner_repo is None:
        # Can't prove the repo is ours -> refuse to probe (coord-scope
        # strict). We still persisted enrollment/orphan above so the
        # watch survives; a later tick with a derivable repo probes it.
        # SURFACE the reason (feedback_surface_dont_silo / codex
        # adversarial [P2]): a bad meta.json repo_path / missing origin /
        # transient git failure would otherwise silently disable PR
        # tracking with no operator-visible signal. Only emit when there
        # ARE watches to probe, so a no-PR project stays quiet.
        if watches:
            out.errors.append(
                "pr-watch: cannot derive this coord's owner/repo from the "
                "project's git remote (origin) — PR tracking is paused until "
                "resolved. Check `git -C <repo> remote get-url origin` / "
                "meta.json repo_path."
            )
        persist_watches(project_dir, doc)
        return out

    due_numbers: list[int] = []
    head_oids: list[str] = []
    foreign_keys: list[str] = []   # persisted watches that fail coord scope
    for key, w in watches.items():
        state = w.get("state")
        # MERGED is truly terminal (irreversible on GitHub) -> never
        # re-probe. NOT_FOUND is parked (404) until operator ack -> never
        # re-query/re-raise (codex iter-14 [P2]). CLOSED_UNMERGED is
        # retained-but-REVERSIBLE: the operator can reopen the PR, and a
        # live task may still point at it — so re-probe a closed watch IFF
        # it still has live backing tasks (codex iter-8 [P2]). A live task
        # on a reopened PR transitions back to OPEN and reconciles a later
        # merge; a closed watch with NO live task stays parked until ack.
        if state in _NO_PROBE_STATES:
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
            # DROP a foreign-repo persisted watch (codex iter-14 [P3]):
            # created on a tick when coord_owner_repo was underivable (None),
            # it now resolves to a DIFFERENT repo. Enrollment already drops
            # foreign tasks, so this watch has no legit backing here; an OPEN
            # foreign watch never reaches terminal/orphan pruning, so it would
            # otherwise linger forever in pr-watches.json, re-erroring every
            # tick + defeating the idle early-out. Reap it to match the
            # enrollment-time coord-scope behavior (fleet owns its state).
            out.errors.append(
                f"pr-watch: dropping persisted watch {parsed[0]}#{pr_num} — not in "
                f"this coord's repo {coord_owner_repo!r} (coord-scope)"
            )
            foreign_keys.append(key)
            continue
        if not _probe_due(w, tick_count, slow_cadence_ticks):
            continue
        due_numbers.append(pr_num)
        snap_prev = w.get("last_snapshot") or {}
        prev_head = snap_prev.get("head_ref_oid") if isinstance(snap_prev, dict) else None
        if isinstance(prev_head, str) and prev_head:
            head_oids.append(prev_head)

    # Reap foreign-repo persisted watches (codex iter-14 [P3]) AFTER the
    # iteration (can't mutate dict during it). pruned++ surfaces the reap.
    for fk in foreign_keys:
        if fk in watches:
            del watches[fk]
            out.pruned += 1

    pr2_active = dispatch_action is not None
    last_repo_probe: RepoProbe | None = None
    if due_numbers:
        base_ref = _common_base_ref(watches, due_numbers)
        repo_probe = prober.probe_repo(
            repo_path, coord_owner_repo, base_ref, sorted(set(due_numbers)),
            sorted(set(head_oids)),
        )
        last_repo_probe = repo_probe
        _apply_probe(
            watches, due_numbers, repo_probe, prober, repo_path,
            now_iso=now_iso, tick_count=tick_count, out=out,
            pr2_active=pr2_active,
        )

    # --- 4.6. AUTO-FIX dispatch (§5.1b/c + §6) — only when the PR2 seam is
    # wired (dispatch_action provided). Reclaim stale leases, derive the
    # uniquely-eligible next-to-merge PR for rebases, and dispatch ONE
    # action per watch under the §6 outcome-suppression lease. When the
    # seam is absent we behave exactly like PR1 (surface-only, above).
    if pr2_active:
        _dispatch_actions(
            watches, tasks=tasks, prober=prober, repo_path=repo_path,
            repo_probe=last_repo_probe, tick_count=tick_count,
            dispatch_action=dispatch_action,
            agent_outcome=agent_outcome or (lambda _aid: "gone"),
            deps_done=deps_done or (lambda _deps: True),
            release_action=release_action,
            now_iso=now_iso, out=out,
        )

    # --- 4.5. ORPHAN raise — AFTER the probe (codex iter-17 [P2]) so it
    # reflects the CURRENT PR state. An OPEN watch with no live task that
    # was SUCCESSFULLY PROBED THIS TICK and is still OPEN is a genuine
    # orphan: the PR is live but nothing backs it. A watch that merged/closed
    # this tick is handled by §5 (not orphaned); one we couldn't probe this
    # tick (transient / coord-scope) is left for a later tick rather than
    # raised off a stale snapshot. Freshness is keyed on `_probed_ok_tick ==
    # tick_count` (codex iter-35 [P2]), NOT `_probed_ok_at == now_iso`: two
    # consecutive reconcile passes can share a wall-clock now_iso (the
    # supervisor cadence), so a stale successful probe would otherwise
    # false-orphan a watch on a later pass whose probe was skipped/failed.
    for key, w in watches.items():
        if w.get("state") != STATE_OPEN or w.get("tasks") or w.get("orphaned"):
            continue
        snap = w.get("last_snapshot")
        probed_open_now = (
            w.get("_probed_ok_tick") == tick_count  # SUCCESSFUL probe this tick
            and isinstance(snap, dict)
            and (snap.get("pr_state") or "").upper() == "OPEN"
        )
        if probed_open_now:
            w["orphaned"] = True
            out.raises.append(
                f"pr-watch: PR #{w.get('pr_number', key)} is OPEN but no live task "
                f"points at it (orphaned-pr); needs operator attention — "
                f"see {w.get('pr_url', '')}"
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
                        # pass the watch's pr_url so the callback can RESTORE
                        # it before flipping done — a task whose pr_url was
                        # cleared by legacy CI-red handling (then preserved as
                        # backing) would otherwise lose the URL of the PR that
                        # actually landed (codex round 31 [P2]).
                        flip_task_done(slug, w.get("pr_url", "") or "")
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
            elif not w.get("tasks"):
                # operator resolved the backing task (went terminal /
                # archived) -> the parked closed watch is done its job;
                # prune so it doesn't keep pr-watches.json alive forever.
                del watches[key]
                out.pruned += 1
        elif state == STATE_NOT_FOUND:
            # parked 404: prune once the backing task is resolved (no live
            # task left) so a handled 404 doesn't keep the file on disk +
            # defeat the idle early-out forever (codex iter-20 [P2]). While
            # a task still points at it, retain (operator hasn't acted).
            if not w.get("tasks"):
                del watches[key]
                out.pruned += 1

    persist_watches(project_dir, doc)
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
    # A watch with NO live backing task is a pending orphan candidate — it
    # MUST probe this tick so the orphan decision (§ post-probe) reflects
    # the CURRENT PR state (the PR may have merged/closed), overriding the
    # slow cadence (codex iter-17 [P2]).
    if not watch.get("tasks"):
        return True
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
    pr2_active: bool = False,
) -> None:
    """Persist each due watch's snapshot + reduce to an event. Fail-soft:
    a whole-repo transient failure (repo_probe.error) skips + RETAINS
    every due watch with a jittered backoff (§3); a per-PR transient
    failure does the same for just that PR; a definitive 404 raises hand.

    `pr2_active`: when True the auto-fix seam is wired, so an actionable
    OPEN event is NOT surfaced here as a "PR1 surfaces only" raise — the
    post-probe _dispatch_actions pass owns it (launching the fixer or
    raising-hand with the right reason). READY notes still surface."""
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
            _park_not_found(w, n, out)
            continue

        if snap.error:
            if snap.not_found:
                _park_not_found(w, n, out)
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
        if needs_base_check:
            # transient if: the BASE couldn't be fetched, OR the PR's HEAD
            # object isn't locally present (head fetch failed / unavailable
            # fork ref) — running merge-base against a missing head returns
            # False and would FALSE-STALE a green PR (codex iter-4 + iter-12
            # [P2]). Either way: SKIP + backoff + retain, don't record.
            base_missing = repo_probe.fetch_error and not pr_base_sha
            head_missing = bool(snap.head_ref_oid) and n not in repo_probe.head_present
            if base_missing or head_missing:
                _arm_backoff(w, tick_count)
                reason = "base ref" if base_missing else "PR head"
                out.errors.append(
                    f"pr-watch: probe for PR #{n} skipped (transient git fetch — "
                    f"{reason} unavailable): {repo_probe.fetch_error or 'head object missing'}"
                )
                continue

        # successful probe -> clear any backoff, reduce, persist snapshot.
        w.pop("probe_skip_until_tick", None)
        w["_backoff_n"] = 0
        out.probed += 1

        # Refresh the watch's BASE from the PR's actual base ref (codex
        # iter-2 [P2]). _new_watch defaults base to "main", but a PR can
        # target a non-main base (stacked at the PR level). The PR2 rebase
        # path keys _base_protected / base_sha_for / the rebase prompt on
        # watch["base"], so a stale "main" default would mis-classify a
        # non-main PR as rebase-eligible and rebase it onto the wrong base.
        # The probe is the authoritative source of the real base name.
        if snap.base_ref_name:
            w["base"] = snap.base_ref_name

        head_contains_base = False
        if pr_base_sha and snap.head_ref_oid:
            head_contains_base = is_anc(pr_base_sha, snap.head_ref_oid)

        prev_event = w.get("last_event")
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
        # SUCCESSFUL-reduce marker (codex iter-18 [P2]): only set on a real
        # snapshot reduction, NOT on a transient failure (which still
        # stamps last_probe_at for backoff bookkeeping). The orphan pass
        # gates on THIS so a probe outage can't false-orphan a watch whose
        # PR may have merged/closed during the blip. We stamp BOTH the
        # wall-clock now_iso (orphan pass) AND the tick_count (the auto-fix
        # dispatch pass): the dispatch gate must distinguish "probed THIS
        # tick" from "probed a prior tick", and tick_count is monotonic +
        # deterministic where two consecutive ticks can share a now_iso
        # (codex iter-6 [P1]).
        w["_probed_ok_at"] = now_iso
        w["_probed_ok_tick"] = tick_count

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
            # Surface READY / actionable events ONLY on a TRANSITION (the
            # event changed since last probe) — NOT every tick. A red /
            # stale / behind PR probes every tick (it's actionable), so
            # raising unconditionally would spam result.errors until the
            # PR is fixed (codex adversarial [P1]). Matches the once-raise
            # discipline of CLOSED_UNMERGED / NOT_FOUND. PR2's
            # dispatched_events latch will own re-dispatch suppression; PR1
            # just avoids the alert spam.
            if event != prev_event:
                if event == EVENT_READY:
                    out.notes.append(f"pr-watch: #{n} is mergeable (READY)")
                elif event in _ACTIONABLE_EVENTS and not pr2_active:
                    # PR1 SURFACES the actionable event (raise-hand /
                    # diagnostic). With the PR2 seam wired, _dispatch_actions
                    # owns this event (launch the fixer or raise with the
                    # right reason) — so we do NOT double-surface here.
                    out.raises.append(
                        f"pr-watch: PR #{n} is {event.upper()} and needs a fix "
                        f"(PR1 surfaces only; auto-fix lands in PR2) — {w.get('pr_url', '')}"
                    )


def _reclaim_and_release(
    watch: dict,
    pr_num: int,
    *,
    tick_count: int,
    current_head: str,
    agent_outcome: Callable[[str], str],
    release_action: "Callable[[str], None] | None",
    out: WatchOutcome,
) -> None:
    """Resolve the watch's in-flight lease (§6) AND release its journal +
    inbox if the lease left `running` this tick (codex iter-6/7 [P2]).

    A PR-watch dispatch journal (kind pr-*) is invisible to the worker
    register_subagent / terminal-release paths, so the §6 lease resolution
    is the ONLY place that reaps it — fleet owns the lifecycle of what it
    creates. Best-effort release: never raises (logged to out.errors)."""
    prev_inflight = watch.get("inflight_action")
    prev_agent = ""
    if isinstance(prev_inflight, dict) and prev_inflight.get("outcome") == OUTCOME_RUNNING:
        prev_agent = str(prev_inflight.get("agent_id", "") or "")
    rec_note, _rec_raise = _reclaim_lease(
        watch, tick_count=tick_count, current_head=current_head,
        agent_alive=lambda aid: agent_outcome(aid) not in ("gone", ""),
        agent_outcome=agent_outcome,
    )
    if rec_note:
        out.notes.append(f"pr-watch: PR #{pr_num} {rec_note}")
    if prev_agent and watch.get("inflight_action") is None and release_action is not None:
        try:
            release_action(prev_agent)
        except Exception as exc:  # noqa: BLE001 — best-effort cleanup, never wedge
            out.errors.append(f"pr-watch: release {prev_agent}: {exc}")


def _dispatch_actions(
    watches: dict,
    *,
    tasks: list,
    prober: Prober,
    repo_path: str,
    repo_probe: "RepoProbe | None",
    tick_count: int,
    dispatch_action: Callable[["ActionDispatch"], str],
    agent_outcome: Callable[[str], str],
    deps_done: Callable[[list], bool],
    release_action: "Callable[[str], None] | None",
    now_iso: str,
    out: WatchOutcome,
) -> None:
    """The PR2 auto-fix pass (§5.1b/c + §6). Runs AFTER probe+reduce, for
    every OPEN watch whose reduced `last_event` is actionable.

    Per watch, in order:
      1. RECLAIM a stale lease (dead agent / expiry; terminal -> ledger).
      2. PRUNE dispatched_events keys whose head != current head (bounded).
      3. Decide the action from last_event:
           STALE/BEHIND/DIRTY -> REBASE (only if uniquely eligible §5.1b),
           CI_FAILED          -> FIX (CI),
           CHANGES_REQUESTED   -> FIX (review) unless substantive -> raise.
      4. SUPPRESS by §6 outcome; otherwise lease + dispatch.

    The lease (key + agent_id + outcome=running) is persisted into the
    watch BEFORE the launchable DISPATCH block is emitted — the callback
    does the emit, and the tick persists the whole doc at the end of
    reconcile_watches, so a crash replays (lease present) rather than
    duplicates. A callback returning "" means the prompt-acquire/emit
    failed: we mark the lease failed_launch (retried next tick).
    """
    is_anc = lambda a, d: prober.is_ancestor(repo_path, a, d)  # noqa: E731

    # slug -> (status, depends_on) for the §5.1b eligibility gates.
    task_status: dict[str, str] = {}
    task_deps_map: dict[str, list] = {}
    for t in tasks:
        slug = getattr(t, "slug", "")
        if not slug:
            continue
        task_status[slug] = getattr(t, "status", "")
        task_deps_map[slug] = list(getattr(t, "depends_on", []) or [])

    # PR numbers with a LIVE, CURRENT backing task — i.e. a non-terminal task
    # whose pr_url STILL points at the PR (codex iter-7 [P2]). A watch's
    # tasks[] can carry a PRESERVED slug (a CI-red-requeued `todo` row whose
    # pr_url was cleared) that exists ONLY so an old-PR merge reconciles it —
    # that row is NOT a dispatchable backer (its retry rides the normal
    # cap'd worker path, NOT a PR-watch fixer; dispatching here would double-
    # work the same branch). Auto-fix fires ONLY for a PR that still has a
    # live PR-pointing task.
    # Keyed on (owner_repo, pr_number) — NOT the number alone (codex iter-8
    # [P2]): a FOREIGN-repo task sharing a PR number must not make an owned
    # watch look dispatchable. The watch's own pr_url owner is matched
    # against this set below.
    # `blocked` is NOT terminal (the watch stays — the PR may still merge),
    # but a coord-blocked task is operator-attention work, so it must NOT be
    # auto-dispatchable (codex iter-14 [P2]) — matches the worker dispatch /
    # resume paths that skip blocked tasks. Keep watching, never auto-fix.
    _NON_DISPATCH_STATUSES = TERMINAL_TASK_STATUSES | {"blocked"}
    live_pr_backed: set = set()
    for t in tasks:
        if getattr(t, "status", "") in _NON_DISPATCH_STATUSES:
            continue
        parsed = parse_pr_url(getattr(t, "pr_url", ""))
        if parsed is not None:
            live_pr_backed.add(parsed)  # (owner_repo, pr_number)

    def _task_deps(slug: str) -> list:
        return task_deps_map.get(slug, [])

    def _base_protected(w: dict) -> bool:
        return (w.get("base") or "main") == "main"

    # §5.1b: derive the rebase-eligible set ONCE for the whole repo so the
    # exactly-one gate is consistent across all watches this tick.
    def _is_candidate(w: dict) -> bool:
        # dispatchable iff the watch's own PR still has a LIVE PR-pointing
        # task in this repo (same predicate the dispatch gate uses below).
        p = parse_pr_url(w.get("pr_url", ""))
        return p is not None and p in live_pr_backed

    eligible, ambiguous = compute_rebase_eligibility(
        watches,
        deps_done=deps_done,
        base_protected=_base_protected,
        is_ancestor=is_anc,
        task_deps=_task_deps,
        is_candidate=_is_candidate,
    )

    for key in list(watches.keys()):
        w = watches[key]
        if w.get("state") != STATE_OPEN:
            # A watch that went TERMINAL (MERGED / CLOSED) this tick with a
            # still-`running` lease must release that lease's journal+inbox
            # BEFORE the §5 prune deletes the watch (codex iter-7 [P3]):
            # the dispatch loop skips non-OPEN watches, and worker replay
            # skips pr-* journals, so this is the only reap point — else the
            # pr-* journal/inbox leaks forever when a PR merges mid-fix.
            try:
                pr_num_t = int(w.get("pr_number", key))
            except (TypeError, ValueError):
                pr_num_t = 0
            inflight = w.get("inflight_action")
            if isinstance(inflight, dict) and inflight.get("outcome") == OUTCOME_RUNNING:
                agent = str(inflight.get("agent_id", "") or "")
                w["inflight_action"] = None
                if agent and release_action is not None:
                    try:
                        release_action(agent)
                    except Exception as exc:  # noqa: BLE001 — best-effort cleanup
                        out.errors.append(f"pr-watch: release {agent}: {exc}")
            continue
        # NEVER auto-dispatch on an ORPHANED / unbacked watch (codex iter-3
        # [P1]). A watch that lost all backing tasks but whose PR is still
        # open + actionable must NOT have a fixer/rebaser launched against
        # it — that's work on an unowned PR. The post-probe orphan pass
        # raises-hand for it; the design is explicit: orphan -> raise-hand,
        # no auto-rebase/fix. (`orphaned` may not be set yet this tick — the
        # orphan raise runs after this pass — so we gate on the empty
        # backing set directly, which is the orphan's defining condition.)
        if w.get("orphaned") or not w.get("tasks"):
            # An orphaned/unbacked watch is never DISPATCHED to, but if it
            # already holds a lease (the backing task went terminal / was
            # removed WHILE a fixer was in flight), that lease + its pr-*
            # journal/inbox must still be RESOLVED + released (codex iter-29
            # [P2]) — worker replay skips pr-* journals, so this is the only
            # reap point. Resolve the lease (head-moved success / dead-agent
            # reclaim / release the journal) before skipping dispatch.
            try:
                _orphan_pr_num = int(w.get("pr_number", key))
            except (TypeError, ValueError):
                _orphan_pr_num = 0
            if isinstance(w.get("inflight_action"), dict):
                _reclaim_and_release(
                    w, _orphan_pr_num, tick_count=tick_count,
                    current_head=_watch_head(w), agent_outcome=agent_outcome,
                    release_action=release_action, out=out,
                )
            continue
        # REQUIRE A FRESH PROBE THIS TICK before reclaiming/dispatching
        # (codex iter-6 [P1]). If the probe was skipped (transient error /
        # backoff cadence), `last_event` + `head` are from a PRIOR tick — a
        # fixer that already pushed + exited could be re-dispatched against
        # the stale CI_FAILED/STALE head, and reclaim would clear its lease
        # off an unobserved-current head. Acting only on a snapshot we
        # confirmed THIS tick (`_probed_ok_tick == tick_count`) guarantees
        # `head` is the live head and the lease's head-moved/dead resolution
        # is judged against ground truth. A non-probed watch simply waits
        # for its next successful probe — its lease is preserved untouched.
        # (tick_count, not now_iso: two consecutive ticks can share a
        # wall-clock string but never a tick number — codex iter-6 [P1].)
        if w.get("_probed_ok_tick") != tick_count:
            continue
        try:
            pr_num = int(w.get("pr_number", key))
        except (TypeError, ValueError):
            continue
        # Only auto-fix a PR that still has a LIVE PR-pointing task (codex
        # iter-7 [P2]) IN THIS COORD'S REPO (codex iter-8 [P2] — match the
        # watch's own owner/repo, not just the number, so a foreign-repo
        # task sharing a number can't make the watch look dispatchable). A
        # watch backed solely by a preserved CI-red-requeued `todo` slug
        # (pr_url cleared, kept only for old-PR-merge reconcile) is NOT
        # dispatchable here — its retry rides the normal cap'd worker path.
        # The lease (if any) is still reclaimed below so a resolved lease's
        # journal gets released, but no NEW action is dispatched.
        w_parsed = parse_pr_url(w.get("pr_url", ""))
        if w_parsed is None or w_parsed not in live_pr_backed:
            # still reclaim a resolved lease (release its journal) but never
            # dispatch a fresh action.
            _reclaim_and_release(
                w, pr_num, tick_count=tick_count, current_head=_watch_head(w),
                agent_outcome=agent_outcome, release_action=release_action, out=out,
            )
            continue
        head = _watch_head(w)
        if not head:
            continue

        # 1. reclaim/resolve the in-flight lease (blocked / head-moved /
        # dead agent / expiry; terminal -> ledger) + release a resolved
        # lease's journal+inbox.
        _reclaim_and_release(
            w, pr_num, tick_count=tick_count, current_head=head,
            agent_outcome=agent_outcome, release_action=release_action, out=out,
        )
        # 2. keep the dispatched_events ledger bounded.
        _prune_dispatched_events(w, head)

        event = w.get("last_event")
        if event not in _ACTIONABLE_EVENTS:
            continue

        # 3. decide the action.
        if event in _REBASE_EVENTS:
            # rebase only the UNIQUELY-eligible next-to-merge PR (§5.1b).
            if ambiguous:
                # 2+ eligible -> never guess; raise-hand once.
                if event != w.get("_raised_event"):
                    w["_raised_event"] = event
                    out.raises.append(
                        f"pr-watch: PR #{pr_num} is {event.upper()} but rebase "
                        f"eligibility is AMBIGUOUS (>=2 next-to-merge candidates); "
                        f"not auto-rebasing — operator must pick — {w.get('pr_url', '')}"
                    )
                continue
            # NON-PROTECTED BASE (codex iter-32 [P2]): a watched PR whose
            # base isn't the protected branch (e.g. `master`, a release
            # branch, or a stacked PR-level base) is intentionally NOT
            # auto-rebased — but it must not vanish silently. PR2 mode
            # suppressed the PR1 actionable raise in _apply_probe, so SURFACE
            # it here (once per event) so the operator can rebase it by hand.
            if not _base_protected(w):
                if event != w.get("_raised_event"):
                    w["_raised_event"] = event
                    out.raises.append(
                        f"pr-watch: PR #{pr_num} is {event.upper()} but its base "
                        f"{w.get('base', '?')!r} is not the protected branch; not "
                        f"auto-rebasing — operator must handle — {w.get('pr_url', '')}"
                    )
                continue
            if pr_num not in eligible:
                # zero-eligible-for-this-PR: a downstack PR with an open
                # upstack is EXPECTED-stale, not actionable. Do nothing
                # (no churn, no raise).
                continue
            # The fresh base SHA the probe captured for this PR's base —
            # the rebase subagent rebases onto exactly this. Empty when
            # there was no probe this tick (cadence/backoff): we then skip
            # the rebase dispatch rather than rebase onto an unknown base.
            base_sha = ""
            if repo_probe is not None:
                base_sha = repo_probe.base_sha_for(w.get("base") or "main")
            if not base_sha:
                continue
            kind = ACTION_REBASE
            key_str = lease_key(kind, event, head_sha=head, base_sha=base_sha)
            action = ActionDispatch(
                kind=kind, event=event, pr_number=pr_num,
                pr_url=w.get("pr_url", "") or "", branch=w.get("branch", "") or "",
                base=w.get("base") or "main", head_sha=head, base_sha=base_sha,
                key=key_str,
            )
        elif event == EVENT_CHANGES_REQUESTED:
            # CHANGES_REQUESTED: a fix subagent handles typo/style; a
            # substantive/design change needs operator judgement. We can't
            # read the review body here (the probe doesn't fetch it), so we
            # dispatch a fix subagent whose prompt instructs it to apply
            # mechanical fixes and RAISE (return BLOCKED) on substantive
            # asks — keeping the "substantive -> raise-hand" guard in the
            # subagent where the review text is available.
            kind = ACTION_FIX
            key_str = lease_key(kind, event, head_sha=head)
            action = ActionDispatch(
                kind=kind, event=event, pr_number=pr_num,
                pr_url=w.get("pr_url", "") or "", branch=w.get("branch", "") or "",
                base=w.get("base") or "main", head_sha=head, base_sha="",
                key=key_str,
            )
        else:  # EVENT_CI_FAILED
            kind = ACTION_FIX
            key_str = lease_key(kind, event, head_sha=head)
            action = ActionDispatch(
                kind=kind, event=event, pr_number=pr_num,
                pr_url=w.get("pr_url", "") or "", branch=w.get("branch", "") or "",
                base=w.get("base") or "main", head_sha=head, base_sha="",
                key=key_str,
            )

        # 4a. ONE in-flight action per watch (codex iter-2 [P1]). A live
        # `running` lease — for ANY key, not just this event's — means a
        # subagent is already operating on this PR's branch. If the
        # actionable signal CHANGED while it ran (CI_FAILED -> STALE, or
        # CI_FAILED -> CHANGES_REQUESTED on the same head), per-key
        # suppression would miss it and overwrite the live lease, putting a
        # SECOND agent on the same branch concurrently. Reclaim already ran
        # this tick (dead / head-moved / blocked leases are cleared), so a
        # surviving `running` lease is genuinely in flight -> suppress all
        # dispatch until it resolves.
        inflight = w.get("inflight_action")
        if isinstance(inflight, dict) and inflight.get("outcome") == OUTCOME_RUNNING:
            continue

        # 4b. §6 per-key outcome suppression. A blocked latch raises-hand once.
        if _action_suppressed(w, key=key_str, current_head=head):
            de = w.get("dispatched_events")
            inflight = w.get("inflight_action")
            blocked_here = (
                (isinstance(de, dict) and de.get(key_str) == OUTCOME_BLOCKED)
                or (isinstance(inflight, dict)
                    and inflight.get("key") == key_str
                    and inflight.get("outcome") == OUTCOME_BLOCKED)
            )
            if blocked_here and key_str != w.get("_raised_blocked_key"):
                w["_raised_blocked_key"] = key_str
                out.raises.append(
                    f"pr-watch: PR #{pr_num} {kind} is BLOCKED (one attempt per "
                    f"head/base; conflict or guard abort) — needs operator "
                    f"attention — {w.get('pr_url', '')}"
                )
            continue

        # 5. LEASE then DISPATCH. Persist the lease (running) BEFORE the
        # callback emits the launchable DISPATCH block (commit-then-emit).
        # We set it on the watch dict; the doc is persisted at the end of
        # reconcile_watches, so a crash after emit but before that persist
        # replays — and the callback itself is the launch boundary.
        w["inflight_action"] = {
            "kind": kind,
            "key": key_str,
            "agent_id": "",
            "leased_at": now_iso,
            "leased_tick": tick_count,
            "outcome": OUTCOME_RUNNING,
        }
        try:
            agent_id = dispatch_action(action)
        except Exception as exc:  # noqa: BLE001 — a dispatch failure must not wedge the tick
            w["inflight_action"]["outcome"] = OUTCOME_FAILED_LAUNCH
            out.errors.append(
                f"pr-watch: dispatch {kind} for PR #{pr_num} failed: {exc}"
            )
            continue
        if not agent_id:
            # prompt-acquire/emit failed -> the action never ran. Mark
            # failed_launch so the next tick retries (not a permanent latch).
            w["inflight_action"]["outcome"] = OUTCOME_FAILED_LAUNCH
            out.errors.append(
                f"pr-watch: dispatch {kind} for PR #{pr_num} did not launch; "
                f"will retry"
            )
            continue
        w["inflight_action"]["agent_id"] = agent_id
        out.dispatched += 1
        out.notes.append(
            f"pr-watch: dispatched {kind} subagent {agent_id} for PR #{pr_num} "
            f"({event.upper()})"
        )


def _park_not_found(watch: dict, pr_num: int, out: WatchOutcome) -> None:
    """Park a watch whose PR is definitively GONE (404). Raise-hand ONLY
    on the transition into not-found (like CLOSED_UNMERGED's once-raise),
    then retain in STATE_NOT_FOUND so the probe gate skips it — no
    re-query / re-raise every tick (codex iter-14 [P2]). The watch stays
    on disk until the operator resolves it (the task going terminal +
    enrollment dropping it, or operator deleting it)."""
    if watch.get("state") != STATE_NOT_FOUND:
        out.raises.append(
            f"pr-watch: PR #{pr_num} not found (pr-not-found); "
            f"needs operator attention — {watch.get('pr_url', '')}"
        )
    watch["state"] = STATE_NOT_FOUND


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

        # 2. Fetch fresh refs for the ancestor check, in TWO separate
        # commands so one unavailable head can't poison the base fetch
        # (codex iter-11 [P2]):
        #   (a) BASE refs — load-bearing. We fetch every distinct base the
        #       PRs ACTUALLY target (codex iter-3 [P2]: stacked PRs differ;
        #       codex iter-5 [P2]: derive from real baseRefName, not a
        #       hard-coded `main` that may not exist). A failure here sets
        #       fetch_error so _apply_probe backs off (codex iter-4 [P2]).
        #   (b) PR HEADS — best-effort, via `refs/pull/<N>/head` (always
        #       advertised by origin, even for FORK PRs — a raw head OID
        #       refspec fails for forks/deleted heads and would poison the
        #       whole fetch). A head-fetch failure is TOLERATED: that one
        #       PR's head may be absent (its ancestor check then reads
        #       UNKNOWN), but base-dependent classification for OTHER PRs
        #       in the repo is unaffected.
        base_refs = sorted({
            s.base_ref_name for s in result.snapshots.values() if s.base_ref_name
        })
        if not base_refs and base_ref:
            base_refs = [base_ref]
        if repo_path and base_refs:
            base_refspecs = [
                f"refs/heads/{r}:refs/remotes/origin/{r}" for r in base_refs
            ]
            try:
                proc = subprocess.run(
                    ["git", "-C", repo_path, "fetch", "origin", *base_refspecs],
                    capture_output=True, text=True,
                    timeout=self.timeout_s, check=False,
                )
                result.fetch_ok = proc.returncode == 0
                if proc.returncode == 0:
                    for r in base_refs:
                        sha = self._rev_parse(repo_path, f"refs/remotes/origin/{r}")
                        if sha:
                            result.fresh_base_shas[r] = sha
                    result.fresh_base_sha = (
                        result.fresh_base_shas.get(base_ref)
                        or next(iter(result.fresh_base_shas.values()), "")
                    )
                else:
                    result.fetch_error = (proc.stderr or proc.stdout or "").strip() or \
                        f"git fetch exit {proc.returncode}"
            except (FileNotFoundError, subprocess.TimeoutExpired, OSError) as exc:
                result.fetch_ok = False
                result.fetch_error = str(exc)

            # (b) best-effort PR-head fetch — only for heads not already
            # local. Use the PR-number head ref (fork-safe). Failure is
            # swallowed at the command level, but we RE-CHECK each head's
            # local presence afterwards and record it in head_present so
            # _apply_probe can distinguish "head missing -> transient" from
            # "head present but not an ancestor -> STALE" (codex iter-12
            # [P2]: a missing head must not be false-STALE).
            need_head = {
                n: s.head_ref_oid
                for n, s in result.snapshots.items()
                if s.head_ref_oid and not self._object_present(repo_path, s.head_ref_oid)
            }
            if need_head:
                pr_head_refspecs = [f"refs/pull/{n}/head" for n in need_head]
                try:
                    subprocess.run(
                        ["git", "-C", repo_path, "fetch", "origin", *pr_head_refspecs],
                        capture_output=True, text=True,
                        timeout=self.timeout_s, check=False,
                    )
                except (FileNotFoundError, subprocess.TimeoutExpired, OSError):
                    pass  # tolerated — presence re-check below decides
            # heads present = (already-local) ∪ (now-fetched-and-present).
            for n, s in result.snapshots.items():
                if s.head_ref_oid and self._object_present(repo_path, s.head_ref_oid):
                    result.head_present.add(n)
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
            # NONZERO exit -> the WHOLE batched call failed; we CANNOT prove
            # any individual PR is a genuine 404 (a private-repo scope/auth
            # blip or a repository-level NOT_FOUND looks the same as a real
            # missing PR). Bias TRANSIENT for every PR (codex iter-16 [P2]):
            # parking on a nonzero exit would move them to STATE_NOT_FOUND
            # and never recover after auth is fixed. A true per-PR 404 only
            # manifests as a null ALIAS under a RESOLVED repository on a
            # ZERO-exit response (handled in the parsing path below).
            stderr = (proc.stderr or proc.stdout or "").strip()
            return {
                n: PRSnapshot(number=n, error=stderr or "gh nonzero exit",
                              not_found=False)
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

        # Repository-level NOT_FOUND (codex iter-15 [P2]): when the WHOLE
        # `repository` is null, the response is ambiguous — it can mean the
        # repo is genuinely gone OR the `gh` token lost scope for a private
        # repo. Parking every PR as NOT_FOUND would stop tracking forever
        # even after auth is fixed. So treat a null repository as TRANSIENT
        # (SKIP + backoff + retain). Only a null PR alias UNDER A RESOLVED
        # repository is a genuine per-PR 404.
        if repo is None:
            reason = err_summary or "repository unresolved (auth/scope?)"
            return {
                n: PRSnapshot(number=n, error=f"graphql repository null: {reason}",
                              not_found=False)
                for n in pr_numbers
            }

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

# owner/repo[/...] tail of a remote URL path. repo allows dots (GitHub
# repo names may contain `.`, e.g. `owner/foo.bar`, codex iter-1 [P2]).
_REMOTE_PATH_RE = re.compile(r"^/?(?P<owner>[^/]+)/(?P<repo>[^/]+?)/?$")
# scp-style remote: [user@]github.com[:port-not-valid-here]:owner/repo.
# (git scp syntax has no port; ssh:// with a port goes through urlparse.)
_REMOTE_SCP_RE = re.compile(r"^(?:[^/@]+@)?github\.com:(?P<path>.+)$")


def parse_remote_owner_repo(url: str) -> str | None:
    """Parse "<owner>/<repo>" (lowercased) from a GitHub remote URL, or
    None. Validates github.com is the actual HOST via urllib (port-aware:
    `ssh://git@github.com:22/owner/repo.git` parses, codex round 29 [P2])
    or an anchored scp-style match. A trailing `.git`/slash is stripped so
    a dotted repo name survives (codex iter-1 [P2]). Lowercased so the
    coord-scope compare matches a differently-cased PR URL (iter-6 [P2])."""
    if not url:
        return None
    from urllib.parse import urlparse

    raw = url.strip()
    path: str | None = None
    parsed = urlparse(raw)
    if parsed.scheme and parsed.hostname is not None:
        # scheme URL (https/ssh/git): host must be exactly github.com.
        # `.hostname` strips userinfo + PORT, so an explicit port is fine.
        if parsed.hostname.lower() != "github.com":
            return None
        path = parsed.path
    else:
        m = _REMOTE_SCP_RE.match(raw)
        if m is None:
            return None
        path = "/" + m.group("path")

    # strip trailing `.git` + slash so a dotted repo name survives.
    path = (path or "").rstrip("/")
    if path.endswith(".git"):
        path = path[: -len(".git")]
    mp = _REMOTE_PATH_RE.match(path)
    if mp is None:
        return None
    return f"{mp.group('owner')}/{mp.group('repo')}".lower()


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
    return parse_remote_owner_repo(proc.stdout.strip())


def utc_now_iso(now: _dt.datetime | None = None) -> str:
    """UTC ISO-8601 with a trailing Z (matches the rest of the skill)."""
    dt = now or _dt.datetime.now(tz=_dt.timezone.utc)
    return dt.isoformat().replace("+00:00", "Z")
