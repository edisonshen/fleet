# DESIGN — coord worktree + orphan lifecycle (auto, on every tick)

**Status:** DRAFT rev21 — generation chokepoint, fence token corrected to a coord-owned per-slug-monotonic counter; in combined codex+claude dual-review
**Review log:**
- rev18 (2026-06-02) — combined codex+claude round 1, 2×P1: (A) journal `Generation` is per-attempt-id and restarts at 0 on fresh re-dispatch → NOT per-slug monotonic; `dispatch_generation` redefined as a coord-owned per-slug counter incremented at dispatch (§1, §3 ordering). (B) `_dispatch_review_handoffs` (loop.py:4295) was an unenumerated reader that double-dispatches on a stale `review-done` → added as R8 + test. P2 (Go gc `parked` persistence) deferred.
- rev19 (2026-06-02) — combined codex+claude round 2, 1×P1: §2.4 still said the fence token was "sourced from #184's journal `Generation`," contradicting the rev18 §1 fix → rewritten to the coord-owned per-slug counter (reuse the CAS *pattern*, not the integer). 2×P2 (clarified §2 to "destructive readers only," noted observer-only paths; clarified §4.1 that the Go gc dirty-guard is an independent implementation of the same contract, not a shared `worktree.py` call site) — applied as wording, not deferred.
- rev20 (2026-06-02) — combined codex+claude round 3, 2×P1: (A) a THIRD leftover rev17-ism in the Concurrency section ("the dispatch journal where the `Generation` epoch lives; rev17 reads the generation") still implied the fence reads journal generation → rewritten (journal flock fences *launch* only; task-row `dispatch_generation` is the separate worker-state fence). (B) pending-acquire retry was specified to match on generation alone, but handoffs INHERIT the generation, so a stale same-slug+same-gen pending entry of the wrong `dispatch_kind` could reuse the wrong prompt via `already_acquired` → retry must match BOTH `{dispatch_generation, dispatch_kind}` (+ handoff phase); mismatch → forget+release+rewrite. +T3 wrong-kind test. P2 folded in: added `_sweep_done_worker_dirs` (D1) to the §3 destructive-caller enumeration (gated on `parked`, not generation).
- rev21 (2026-06-02) — combined codex+claude round 4, 1×P1: §3 exact-dispatch-order never pinned WHEN the slug leaves the ready set, so a literal impl could emit a launchable DISPATCH while still `status=ready` → defeating the existing duplicate-dispatch guard (`_apply_dispatch` flips `status=in-progress` before bootstrap; DISPATCH collected only after apply succeeds, loop.py:901/4634). Pinned: `dispatch_generation` persist folds into the SAME pre-launch `status=in-progress` commit (step 2), strictly before prompt-build/emit. +T3 "no launchable DISPATCH while ready" test. P2 folded in: added D2 (Go `fleet gc --kinds=worker-records`, worker_records.go:199/222) to the destructive-caller enumeration — must be `parked`-aware or it erases the §4.2 recovery context the coord sweep protects.
**Scope:** safe auto-reap of TERMINAL-task worktrees on the path that already fires, made correct by routing **every** worker-state reader and writer through ONE generation-aware chokepoint, then enumerating every reader/writer as the acceptance checklist. Reap keys on the persisted `worktree=` field (not `cap`), uses `force=False` + a `git status --porcelain` dirty-guard, caller-specific park on dirty, and a SAFE branch-identity-corroborated orphan-dir resolver on the Go gc backstop. Path C (orphan recovery of NON-terminal dead workers) deferred → `coord-orphan-worker-recovery`.
**Priority:** P1 (cleanup is P0/P1; the disk hit 98% from un-reaped worktrees).
**Task:** `coord-worktree-lifecycle-6338` (supersedes gc-spares-active-worktrees + coord-death-orphans + coord-supervisor-reconci).
**Supersedes/extends:** `docs/DESIGN-coord-death-worker-recovery.md`.
**Author:** coord `cedacc55`, 2026-06-02.

---

## Why rev17 is a restructure, not a rev16+1 patch

rev7→rev16 ran **10 codex rounds**. Each round found the **same bug class** — a
**stale-attempt, generation-unaware worker-state** read or write — on a **new
code path**: reconcile → reaper → sentinel callers → supervisor stuck-check →
coordinator-authored writes → pending-acquire → handoffs → the Go gc belt.
Patching call-site N never converged, because the next round simply found
call-site N+1.

The recurrence is itself the finding: **the design's correctness hinges on one
chokepoint** — every worker-state read goes through a single tri-state,
generation-filtered reader, and every worker-state write is a single
generation-CAS'd writer. rev17 defines that chokepoint **once, normatively**,
then **enumerates every reader and writer in the codebase** as an acceptance
checklist. The original ask (safe worktree reaping) is re-expressed as a
**consumer** of the chokepoint, not a standalone set of patches.

The full round-1..15 patch history is preserved as a one-line-each appendix at
the end — the body below is the clean abstraction.

---

## Problem

Worker worktrees are created on dispatch but only reaped by a **manual**
`fleet gc --kinds=worktrees`. So they accumulate: this session 6.9 GB of
done-task rainier worktrees (1.2 GB each) filled the disk to **98%**.
`prune_worktrees()` runs at tick start but only prunes *git-missing* dirs — it
does NOT run the terminal-task reaper. Three coupled gaps:

1. **No auto-reap of terminal worktrees** → disk fills (operator: "can we do
   auto clean stale worktree").
2. **No active-worktree guard** → the 2026-06-02 incident: the coord reaped a
   worktree *mid-work* and lost uncommitted edits. Auto-reap WITHOUT this guard
   repeats that work-loss automatically.
3. **Stale-attempt confusion** → a re-dispatched slug's CURRENT worker is
   reaped/blocked because a PRIOR attempt's `state.json` / sentinel is read as
   authoritative. This is the bug class the 10 codex rounds kept re-finding.

## Goal

Worktrees of **terminal** (done/abandoned) tasks are reaped **automatically**
(the coord tick invokes `fleet gc --kinds=worktrees --apply` every-N-ticks),
**never** removing a tree whose CURRENT-generation worker hasn't finished or
that has uncommitted changes, and **never** acting on a PRIOR attempt's state.
Continuous, safe, no manual `gc`. (Orphan recovery of NON-terminal dead workers
is deferred — see Path C.)

---

## 1. The epoch: a coord-owned, per-slug-monotonic `dispatch_generation`

The worker-state fence token MUST be **monotonic per slug**. #184's journal
`Generation` is NOT — and rev18 corrects rev17's mistake of equating them.

**Why the journal `Generation` alone is unsound (the rev17 bug).** #184's
`Journal.Generation` is scoped to a **single dispatch id** (`agent_id`), and a
fresh journal **starts at 0** (`2b37e42:internal/dispatch/dispatch.go:225` field
doc: "acquire of a fresh journal starts at 0; reset-for-relaunch bumps it";
`newJournal` leaves the int zero-valued). A genuine **re-dispatch** of a slug
(its `todo`/`worker_failed` path) mints a **new `agent_id`** in `_dispatch_ready`
(`loop.py` ~4355, `mint_agent_id()` when no pending entry) → a **new journal** →
`Generation == 0` **again**. So attempt A and attempt B of the SAME slug can both
carry `dispatch_generation == 0`. A stale `phase=done` `state.json` from attempt
A would then compare **equal** to attempt B's task-row authority and be read as
`current` — re-opening the exact stale-attempt removal class the chokepoint
exists to close. `reset-for-relaunch` bumps the gen, but ONLY for handoff_resume
re-arming the SAME dispatch id — it does NOT cover fresh re-dispatch. Therefore
the journal `Generation` is per-attempt-id, not per-slug-monotonic.

**The fix: `dispatch_generation` is a coord-owned per-slug counter.** It is a new
persisted **task-row field** that the coord **increments by exactly 1 on every
genuine (re-)dispatch of that slug** (under the coord lock, as part of the
dispatch-ordering rule in section 3). The authority is the **task row's current
`dispatch_generation`**; the worker's `state.json` and every sentinel carry the
value stamped at their dispatch. Because the coord increments monotonically per
slug and persists BEFORE bootstrapping `state.json` (section 3 ordering),
attempt B always carries a **strictly higher** integer than attempt A — so
attempt A's stale state compares `stale`, never `current`. Absent / legacy = 0;
a slug never dispatched is 0; the first dispatch sets 1.

**Relationship to #184 (we reuse its primitives, not its number).** rev18 keeps
the real #184 machinery as the surrounding context — it does not re-implement
locking or invent a parallel *launch* token:

- **`withJournalLock(id, fn)`** (`store.go:181`) — "the per-id flock RMW
  primitive … ALL writers (incl. acquire/release) route through it." This stays
  #184's launch-path concurrency primitive. rev18 does not touch it.
- **`fleet claims mark-launch-attempted <id> <gen>`** (`cmd/fleet/claims.go:398`)
  — the tri-state CAS (`ok` exit 0 / `predicate_fail` exit 20 / `contention` exit
  21) gating #184's pending→launch_attempted on the **journal** generation. This
  is the **shape** rev18 mirrors for worker-state (a CAS that rejects a stale
  token), but the two tokens are **distinct**: #184's journal gen fences
  *launch*; the task-row `dispatch_generation` fences *worker-state writes*.
- **`Journal.Owner`** (`dispatch.go:213`, `"project/<p>/slug/<s>"`) — the
  slug↔journal linkage is still useful for audit, but it is NOT the source of the
  fence token: multiple non-terminal journals can share one `Owner` across
  attempts (each re-dispatch is a new id), so "the slug's current journal
  generation" is ill-defined. rev18 does NOT read the fence token out of any
  journal — the coord's per-slug counter is the sole source.

A **handoff** (reviewer/finisher) is a *continuation of the same attempt*, so it
**inherits** the slug's current `dispatch_generation` (no increment). Only a
genuine re-dispatch increments. "Is this state/sentinel from the current
attempt?" is then a single integer compare against the task row's
`dispatch_generation`.

**Schema rollout (additive, before any writer).** `dispatch_generation` is a new
persisted **task-row field** (like `parked`), holding the coord-owned per-slug
counter. Both parsers reject unknown task keys today (Python `parse.py`, Go
`internal/tasks/tasks.go`) and `fleet tasks set` rejects unknown keys, so the
ordered rollout is: (1) Python parse/write, (2) Go parse/write, (3) `fleet tasks
set <slug> dispatch_generation=`, (4) TUI/`fleet status` display, (5) ONLY THEN
the dispatch/state writers that set it. Absent = generation 0 / legacy.
Backward-compatible.

---

## 2. The chokepoint (stated ONCE, normatively)

Every **coord-side** access to worker state (`workers/<slug>/state.json`) that
can drive a **mutation, reap, or dispatch** goes through one of two functions.
**No ungated mutation-driving reader or writer exists.** (Pure **observer**
paths — `fleet workers list` / `workers.ListActive`, `fleet peek`,
`fleet status` display, and the shared raw `internal/workers.ReadState` they sit
on — read `state.json` for rendering only; they drive no state mutation and so
need no gate. The enumeration in section 3 is the exhaustive list of the
*destructive* readers/writers.) This is the single fix; sections 3–5 are
consequences.

### 2.1 Reader — `read_current_worker_state(slug, dispatch_generation) -> {current | stale | missing}`

A single tri-state helper. EVERY mutation/reap/dispatch-driving coord-side
worker-state read routes through it (observer-only paths are out of scope per §2).

```
read_current_worker_state(slug, dispatch_generation):
    s = load workers/<slug>/state.json        # raw read
    if s is absent:                 return missing
    if s.dispatch_generation == dispatch_generation:  return current
    else:                           return stale       # PRIOR generation
```

- **`current`** = state present, generation matches the slug's current
  `dispatch_generation` (the task-row authority). The caller proceeds as normal.
- **`stale`** = state present but from a PRIOR generation. **`stale`
  SHORT-CIRCUITS** the caller for that slug this tick: **NO task-status mutation,
  NO `clear_worker`, NO `delete_worker_dir`, NO worktree removal, NO nudge, NO
  escalation, NO block.** The prior attempt's write is inert + **surfaced**. The
  current attempt is re-evaluated on a later tick once it writes a
  `current`-generation state.
- **`missing`** = no state file at all (a genuinely absent state, no live
  attempt). ONLY `missing` keeps the existing **"worker died without PR"**
  semantics (flip to `todo`, clear worker, etc.).

**The load-bearing distinction is `stale` ≠ `missing`.** The 10-round bug class
was, every time, some path treating a PRIOR-generation `state.json` as `missing`
(absent / not-alive / no-terminal-state), which fell through to the
died-without-PR branch and **removed the CURRENT attempt's worktree + worker
dir**. Tri-state makes "I see a prior attempt's state" a first-class,
short-circuiting outcome distinct from "I see nothing."

### 2.2 Writer — every `fleet workers update` carries `--dispatch-generation <gen>` and is a CAS

EVERY worker-state write — worker self-reports, reviewer/finisher reports, AND
coordinator-authored writes (`_mark_worker_blocked` et al.) — passes
`--dispatch-generation <gen>` and is a **compare-and-reject** under the existing
worker lock:

```
fleet workers update <slug> --dispatch-generation <n> ...:
    under the worker lock:
        task_gen = task row's dispatch_generation        # the AUTHORITY
        if n != task_gen:           REJECT + surface      # stale writer
        if state.json absent:       bootstrap fresh ONLY when n == task_gen
        if state.json present, on-disk gen < task_gen, n == task_gen:
                                    REPLACE with FRESH state (not field-merge)
        else (n == task_gen):       apply the update
```

Three properties the 10 rounds proved necessary, kept verbatim as CAS rules:

- **Authority is the TASK ROW's `dispatch_generation`, not the on-disk
  `state.json` alone.** `workers.UpdateState` bootstraps a *missing* file, so an
  on-disk-only compare would let a stale worker RECREATE/poison an absent file
  (then reject the current worker because the stale gen is now "on disk"). The
  task row is the durable CAS authority; a missing file is bootstrapped ONLY by
  the current generation.
- **Repair of a stale-gen file = FRESH replacement, not in-place mutation.** When
  the current generation overwrites a stale-gen file, DISCARD the old object and
  bootstrap brand-new state (fresh `started_at`, EMPTY `pr_url`/review/
  completed-phases). A field-merge would let stale `started_at`/`pr_url` survive
  and poison the branch→PR fallback that trusts `state.json.started_at` as the
  per-attempt PR provenance gate (resolving an OLD PR onto the current task).
- **No ungated writer.** Coordinator-authored `fleet workers update` (e.g.
  `_mark_worker_blocked`) is NOT exempt: it carries the current task generation
  (or routes through the reader and fails closed on `stale`/ambiguous). An
  ungated coord writer would either fail forever after the CAS lands or become a
  dangerous CAS exemption.

### 2.3 Why a chokepoint and not per-path patches

If even one reader bypasses 2.1 or one writer bypasses 2.2, the stale-attempt
class re-appears on that path — exactly what rounds 1–15 demonstrated. The
acceptance bar (section 3) is therefore **"prove every reader/writer routes
through the chokepoint,"** not "patch the paths codex happened to find."

### 2.4 Prior art — this IS the fencing-token pattern
The chokepoint is the textbook **fencing-token** solution to "a paused/stale
actor resumes and writes" (Kleppmann, *How to do distributed locking*): a
**monotonically-increasing token** issued per acquisition, and the **resource
actively rejects any write whose token is older** than the highest it has
seen. Our `dispatch_generation` is the fencing token — a **coord-owned per-slug
counter** incremented on each genuine (re-)dispatch (§1), NOT #184's journal
`Generation` (which is per-attempt-id and restarts at 0 on a fresh re-dispatch,
so it is not per-slug monotonic — the rev17 mistake §1 corrects). We reuse
#184's CAS/locking *pattern* (`mark-launch-attempted`'s tri-state token check
under `withJournalLock`), not its integer. 2.2's CAS is "the resource rejects
stale tokens." The lesson Kleppmann draws — Redlock is unsafe *because the
resource doesn't check the token* — is precisely why the bar is "every writer
gates" (2.3), not "the lock is enough." The 10-round whack-a-mole was the
classic failure mode of token-checking at some call sites but not all; rev17's
own slip — adopting a token that wasn't actually per-resource monotonic — was a
subtler instance of the same trap, caught in dual-review round 1.
- **Path C orphan recovery (deferred)** maps to the **lease + heartbeat**
  pattern (Kubernetes node Leases; Airflow zombie-detection: no heartbeat in
  N seconds → reschedule + "adopt tasks from a dead executor") — the worker
  lease is the primitive that lets us *positively* declare a non-terminal
  worker dead. Its TTL **must exceed the longest worker phase**, or we hit the
  Celery `visibility_timeout < longest-task → duplicate runs` footgun — the
  same reason `_WORKER_STATE_FRESH_S` is deliberately long.
- **Reaping (section 4)** maps to Kubernetes **owner-references + finalizers**:
  the worktree is a dependent reaped when its owner (task) is terminal; the
  dirty-guard/PARK is a finalizer ("don't delete until safe").

Refs: Kleppmann "How to do distributed locking"; Kubernetes Garbage Collection
(owner refs/finalizers) + Leases; Airflow zombie tasks; Celery
visibility_timeout.

---

## 3. Exhaustive reader/writer enumeration (THE acceptance checklist)

This list IS the implementation acceptance. Each entry must route through 2.1
(readers) or 2.2 (writers). Citations are against current `skills/coordinator/`
and `internal/`/`cmd/` and must be re-verified at implementation time (the tree
moves).

### Readers (route through `read_current_worker_state`)

| # | Reader | Where (verify at impl) | What `stale` must prevent |
|---|--------|------------------------|---------------------------|
| R1 | `_is_worker_alive` | `loop.py` (~2185/2251) | treating a stale terminal `phase` as not-alive → fall-through to died-without-PR |
| R2 | `_worker_terminal_state` | `loop.py` (~2274) | reading a stale `(phase, pr_url, blocked_reason)` as the current terminal outcome |
| R3 | `_reconcile_inflight` mid-phase + alive checks | `loop.py` (~2746/2773/2822) | flipping the CURRENT task `todo` + removing its worktree from a stale terminal state |
| R4 | reconcile died-without-PR branch | `loop.py` (~2963) | `status=todo` + `clear_worker` + `delete_worker_dir` driven by a stale state (the rev14 P0) |
| R5 | reaper input construction (`judge_completion`) | `reaper.py` (~112), `loop.py` (~1757) | reaping/killing the current attempt from a stale `phase=done`/`failed` |
| R6 | supervisor `run_stuck_check` raw reads | `supervisor.py` (~1555/1572/1703) | classifying a stale phase as stuck → nudge/escalate/`status=blocked` on the current task |
| R7 | Go gc belt `isTaskTerminalOnDisk` non-terminal-`state.json` veto | `gc.go` (~929) | a STALE non-terminal state vetoing reap FOREVER (leak never clears) — and conversely a CURRENT non-terminal state correctly vetoing |
| R8 | `_dispatch_review_handoffs` raw `state.json` read → reviewer/finisher dispatch | `loop.py` (~4295) | a STALE `review-pending`/`review-done` **double-dispatches** a reviewer/finisher: it releases the current claim (~4376) + acquires/emits a new dispatch (~4405). If the new prompt is stamped with the slug's current `dispatch_generation`, the spawned finisher becomes an **accepted current-gen writer** (e.g. a spurious `phase=done`) — lost work + clean-tree reap on the live attempt |

For R1–R6 + R8 (Python): replace the raw `_read_worker_state` readers
**wholesale** with the tri-state helper — do not wrap only terminal-state
parsing. R8 acts ONLY on `current`; `stale` short-circuits (no release, no
pending-acquire mutation, no DISPATCH block emitted) and `missing` falls to the
existing handoff no-op. For R7 (Go):
the gc projection carries BOTH the task-row `dispatch_generation` and the
worker-state `dispatch_generation`; the belt vetoes ONLY when they are **equal**
AND the phase is non-terminal. A stale-generation non-terminal state is surfaced
but does NOT veto.

### Writers (carry `--dispatch-generation` + CAS)

| # | Writer | Where (verify at impl) | Stale-write hazard the CAS closes |
|---|--------|------------------------|-----------------------------------|
| W1 | worker self-report `fleet workers update` | worker prompt (`dispatch.py` ~92/134), `workers.go` (~406/521) | a stale worker clobbers the current `state.json` (e.g. stale `phase=done` over live `in-progress`) |
| W2 | reviewer/finisher `fleet workers update` (incl. finisher `phase=done`) | handoff prompts (`loop.py` ~4322/4329), `dispatch.py` (~757) | a handoff write under the wrong generation (must INHERIT the dispatching attempt's gen) |
| W3 | coordinator-authored `_mark_worker_blocked` (+ any coord-side `fleet workers update`) | `supervisor.py` (~1677/1763) | an ungated coord write either fails forever post-CAS or becomes an exemption |
| W4 | dispatch-time `state.json` bootstrap | `_apply_dispatch` (`loop.py` ~4634/4642) | bootstrapping under a generation that isn't yet the task-row authority |

### Destructive worker-dir caller (gated on the `parked` field, not the generation)

| # | Caller | Where (verify at impl) | Hazard the gate closes |
|---|--------|------------------------|------------------------|
| D1 | `_sweep_done_worker_dirs` | `loop.py` (~3660) | deletes ANY `status=done` worker dir every tick, **independent of sentinel/reconcile apply results** — so it can erase the recovery context of a dirty-parked `done` row. It is NOT a `state.json` reader/writer (no generation gate), but it MUST honor the durable `parked` task-row field (§4.2): skip any worker dir whose row is `parked`. |
| D2 | Go `fleet gc --kinds=worker-records` (`KindWorkerRecords`) | `internal/gc/worker_records.go` (~199/222), suggested by `fleet status` (`status.go` ~304) | removes `workers/<slug>/` for task-terminal rows — including a dirty-parked `done` row (which is exactly `status=done` with the dir intentionally kept). Manual/global GC would erase the §4.2 recovery context the coord sweep (D1) protects. The worker-records classifier MUST become `parked`-aware: skip (or surface, not auto-remove) any worker dir whose task row is `parked`. (Same projection-seam dependency as R7/§4.4 — the classifier needs the task row's `parked` field.) |
| D3 | raw `fleet workers delete` / `internal/workers.Delete` (the dir-deleter the coord's `_maybe_delete_worker_dir` calls post-terminal) | `cmd/fleet/workers.go` (~45), `internal/workers/workers.go` (~638, "rm -rf, no archive"; takes NO worker flock) | `RemoveAll`s `workers/<slug>/` unconditionally. The coord's own `_maybe_delete_worker_dir` callers are ALREADY gated (§4.1: any tree-left/dirty-parked outcome SKIPS the delete), so the destructive coord path is closed — but the bare CLI is not. Treat as **operator-explicit/manual** (document that `parked` is only auto-protected from the coord sweep + GC classifiers, not from a hand-run `fleet workers delete`), OR make the command `parked`/task-terminal aware unless `--force`. Not a coord stale-gen hole; listed for literal exhaustiveness. |

### Sentinel-path readers (same generation gate, via the archive sentinel token)

The archive sentinel path is a second class of worker-authored state. The SAME
`Generation` token gates it (the worker stamps its `dispatch_generation` into
every state-mutating sentinel it emits):

| # | Sentinel path | Where (verify at impl) | Stale-sentinel hazard |
|---|---------------|------------------------|------------------------|
| S1 | `_apply_sentinel` `task_done_pr` | `loop.py` (~3585/3587) | a stale `task_done_pr` sets `pr_url` + `in-review` on a re-dispatched slug, then reaps its CLEAN tree |
| S2 | `_apply_sentinel` `worker_failed` | `loop.py` (~3620/3624) | a stale `worker_failed` reopens the current attempt to `todo` + `worker_pid=0` + reaps |
| S3 | `_apply_sentinel` `blocked_question` | `loop.py` (~3613) | a stale `blocked_question` blocks the CURRENT worker + misroutes operator handling |
| S4 | caller release/forget + handoff-clear (gated on apply-result) | primary drain `loop.py` (~720/738), supervisor replay (~1459/1471), supervisor drain (~1628/1642) | the side effects live in the CALLER, after `_apply_sentinel` returns |
| S5 | deferred-sentinel queue serialize/load | `_save_deferred_sentinels` (~1883), `_load_deferred_sentinels` (~1850) | a deferred-then-replayed sentinel arrives with EMPTY generation → fail-open (stale removal) or fail-closed (leak) |

Sentinel discipline (consequence of the token, stated once):

- A **state-mutating** sentinel (`task_done_pr`, `worker_failed`,
  `blocked_question`) carries the `Generation` token and is corroborated against
  the slug's current `dispatch_generation`. **Mismatch → skip ALL terminal side
  effects** (status mutation, `pr_url`, worktree removal, worker-dir delete,
  release/forget, handoff-clear) — only surface. A pure `new_task` wake carries
  no state mutation and stays token-free.
- `_apply_sentinel` returns **`applied | skipped_stale | error`**. EVERY caller
  (S4) gates release/forget/handoff-clear on `applied`; `skipped_stale` performs
  NONE of those. `error` → re-queue/defer (the watermark advances past the only
  durable record of the transition, so a returned `error` must not silently
  consume it); `skipped_stale` → consumed (deliberate no-op); `applied` →
  consumed.
- S5: the deferred-sentinel queue **persists the `Generation` field** (both
  serializer and loader) so a deferred→replayed sentinel corroborates correctly
  on replay — neither fail-open nor fail-closed.
- **Tokenless-legacy rollout:** a sentinel/state with NO generation came from a
  pre-migration worker. Corroborate against the slug's recorded current attempt:
  legacy-trusted ONLY when the slug has NOT been re-dispatched since (recorded
  attempt still matches). If the slug HAS been re-dispatched → `skipped_stale` +
  surface (fail safe: never reap a re-dispatched slug's live tree on a tokenless
  legacy sentinel). The window closes as pre-migration workers drain.

### Dispatch-ordering + retry invariants (so the generation is authoritative)

These are writer-side ordering rules that make 2.2's CAS sound:

- **Exact dispatch order.** (1) read the slug's current task-row
  `dispatch_generation` and **increment by 1** (under the coord lock; a genuine
  re-dispatch only — handoffs reuse, see below); (2) **persist it to the task
  row** (the durable CAS authority) **together with the existing
  `status=in-progress` flip** that removes the slug from the ready set (plus
  `branch=`/`worktree=` as applicable); (3) bootstrap a matching `state.json`; (4)
  build prompts containing that generation; (5) acquire/emit dispatch. Steps
  (1)–(2) make the per-slug counter strictly monotonic, so attempt B's authority
  always exceeds attempt A's stale state. Once step (2) lands, any stale worker's
  `fleet workers update` is CAS-rejected — the window is closed.
  - **Preserve the existing duplicate-dispatch guard: leave the ready set BEFORE
    any launchable DISPATCH block can be emitted.** Today `_apply_dispatch`
    durably flips `status=in-progress` *before* bootstrapping `state.json`, and
    the DISPATCH block is collected ONLY after `_apply_dispatch` succeeds
    (`loop.py` ~901/~4634) — so a crash/retry between ticks never re-emits a
    launchable instruction for a slug that is already `in-progress`. The new
    `dispatch_generation` persist is **folded into that same pre-launch commit**
    (step 2), not added as a later step: it must NOT be possible to emit a
    launchable DISPATCH while the task row is still `ready` (that would let the
    next tick / recovery path redispatch the same slug). Today the prompt is
    built before `agent_id` is minted; rev18+ pins the order above, with the
    `status=in-progress` + `dispatch_generation` commit (2) strictly before
    prompt-build (4) and dispatch-emit (5).
- **A pending-acquire retry REUSES its generation.** The dispatch recovery path
  reuses `pending_acquire_agent_ids` by slug and `AcquireCoordPromptInbox`
  returns `already_acquired` *without rewriting the prompt file*. A retry must
  NOT re-increment: if it did, the task row would expect gen N+1 while the
  already-acquired prompt still tells the worker to write gen N, so EVERY
  `fleet workers update --dispatch-generation N` would be CAS-rejected → silent
  wedge. So a retry reads back the same generation. If the claim must be
  re-minted, **release + rewrite the prompt** with the new generation before
  incrementing — never leave a prompt/task-row generation skew. The
  pending-acquire record stores `{agent_id, dispatch_generation, dispatch_kind}`
  so a retry can PROVE prompt ↔ task-row generation match.
  - **A pending entry is reused ONLY on a full `{dispatch_generation,
    dispatch_kind}` match — generation alone is insufficient.** Today the pending
    map is `slug → agent_id` (`supervisor.py` ~599) and BOTH `_dispatch_ready`
    (worker) and `_dispatch_review_handoffs` (reviewer/finisher) consume the SAME
    slug-keyed map (`loop.py` ~4117/4347). Because handoffs **inherit** the
    attempt's generation (no increment), a worker retry and a reviewer/finisher
    retry on the same slug carry the **same generation** — so generation equality
    cannot tell them apart. If a stale pending entry (e.g. a worker-kind
    acquire that errored) survives into a handoff dispatch with the same
    slug+generation but a DIFFERENT `dispatch_kind`, blind reuse calls
    `AcquireCoordPromptInbox` → `already_acquired` **without rewriting the
    prompt**, so the spawned subagent runs the WRONG prompt (a worker prompt for
    a reviewer slot, or vice-versa). The retry MUST therefore match BOTH
    `dispatch_generation` AND `dispatch_kind` (worker vs reviewer vs finisher —
    and for handoffs the expected phase, `review-pending`→reviewer /
    `review-done`→finisher). **Kind/phase mismatch → forget + release the stale
    claim, rewrite the prompt, then proceed** (never reuse). This requires the
    pending map to actually persist `{agent_id, dispatch_generation,
    dispatch_kind}` (extend the current `slug → agent_id` shape) so the match is
    checkable.
- **Dead-coord resume inherits, never increments.** `handoff_resume.py`
  (`~360`, driven by `fleet dispatch --coord-spawn` recovery, `dispatch.go`
  ~927) rewrites the EXISTING inbox and re-emits a DISPATCH block reusing the
  SAME `agent_id`. This is NOT a genuine re-dispatch of a worker slug — it is the
  COORD's own resume — so any worker-slug `dispatch_generation` it touches is
  preserved (it never increments a worker's per-slug counter). A recovered
  worker DISPATCH from an existing inbox carries the generation already stamped
  at the original dispatch; a tokenless-legacy resume follows the §3
  tokenless-legacy rollout; a stale-generation write is CAS-rejected like any
  other.
- **Handoffs inherit, never increment.** A reviewer/finisher handoff is a
  continuation of the SAME attempt: it inherits the dispatching attempt's
  `Generation`, injected into the reviewer/finisher prompts and passed to their
  `fleet workers update --dispatch-generation`. `_apply_dispatch_handoff` carries
  the generation across. Only a genuine re-dispatch increments.

---

## 4. Worktree-reaping (the original ask) — a CONSUMER of the chokepoint

With section 2 in place, the reaper is small. Reap a worktree only when the
**CURRENT-generation** worker is **terminal + clean**. Every reap decision first
asks the reader (2.1) for `current | stale | missing` and acts only on
`current`; `stale` short-circuits (the prior attempt is inert), `missing` keeps
died-without-PR semantics.

### 4.1 force=False + dirty-guard, on both callers + the Go gc path

`worktree.py remove_worktree`: default **`force=False`**; before removing, run
`git -C <wt> status --porcelain` — **non-empty → do NOT remove; return a
"dirty-parked" outcome.** `force=False` means a tree that turns dirty between the
check and the remove makes `git worktree remove` FAIL → surfaced, never blown
away. This lives at the `worktree.py` level so it covers BOTH Python callers
(`_maybe_remove_worktree` on reconcile, and `_apply_sentinel` task_done_pr /
worker_failed). The **same invariant** (porcelain dirty-guard + `--no-force`)
must be applied **independently** on the Go `fleet gc --kinds=worktrees` path —
that path does NOT call `worktree.py`; today `DefaultDeps.RemoveWorktree =
removeGitWorktree` runs `git worktree remove --force` (`internal/gc/gc.go`
~893). §4.4 + T7 specify the Go-side guard normatively; this is the same
behavioral contract enforced in two implementations, not one shared call site.

`remove_worktree` / `_maybe_remove_worktree` return an explicit outcome:
**`removed | dirty-parked | noop | error`**. Callers branch:

- **`removed` / `noop`** → existing `_maybe_delete_worker_dir` flow runs.
- **`dirty-parked` / `error`** (any outcome that **leaves a tree on disk**) →
  SKIP `_maybe_delete_worker_dir` (keep the recovery context) AND apply the
  caller-specific park status (4.2). A TOCTOU dirtiness-caused remove FAILURE is
  classified **`dirty-parked`**, not generic `error`; only a genuinely unexpected
  failure (path vanished, git internal error) is `error`.

### 4.2 Dirty → caller-specific PARK, NOT blanket `blocked`

Each caller has ALREADY set a status before cleanup runs, so a blanket
`status=blocked` is wrong:

- **`_apply_sentinel` task_done_pr** (already `in-review`) → **preserve
  `in-review`** + surface the dirty path. Never stop PR polling for a shipped
  task.
- **`_apply_reconcile` `done`** (PR merged) → **preserve `done`** + surface.
  Never reopen a merged task.
- **`_apply_reconcile` `todo`** and **`_apply_sentinel` worker_failed** (the ONLY
  redispatch-eligible paths) → flip to **`blocked`** + surface. Leaving them
  `todo` with `worktree=` set makes the next dispatch **reuse the dirty tree**
  (`create_worktree` treats a registered path collision as success without a
  cleanliness/branch check). So these — and ONLY these — block.

In ALL park cases the worker dir is **kept**. Resolution: the operator
inspects/commits/discards, then unblocks (`todo`/`worker_failed`) or reaps the
now-clean leaked dir (`in-review`/`done`).

**The parked rows must survive the done-dir sweep.** `_sweep_done_worker_dirs`
deletes ANY `status=done` worker dir every tick, defeating "keep the worker dir."
So the park is recorded as a durable **`parked` task-row field** (a `- parked:
<UTC ts + reason>` bullet, parsed alongside `worktree=`/`branch=`);
`_sweep_done_worker_dirs` skips any worker dir whose row is `parked`. The field
is **cleared on resolve** (status leaves `blocked`, or explicit `fleet tasks set
<slug> parked=`). `parked` gets the SAME ordered schema rollout as
`dispatch_generation` (parsers + `fleet tasks set` + display BEFORE any writer),
since both parsers reject unknown keys today.

**Redispatch-eligible paths must NOT re-promote a left-behind tree.** On the
`todo`/`worker_failed` paths, ANY outcome that leaves a registered/unverified
tree on disk (`dirty-parked`, `error`, or a branch-mismatch tree-left) must force
**`status=blocked`** (not merely clear a redispatch marker — `_filter_ready`
dispatches purely on `status=ready` + deps). PLUS a **dispatch-side preflight**
keyed on the **computed deterministic path** (`worker/<slug>`, NOT the persisted
`worktree=` which can be empty on a leaked-dir row): before `create_worktree`, if
a tree exists at the computed path, verify `worker/<slug>` is checked out AND the
tree is clean — else refuse dispatch + surface. (`task_done_pr`/reconcile-`done`
are not redispatch-eligible, so they only surface.)

### 4.3 Reap keyed on persisted `worktree=`, NOT `cap`

`cap` is the coord **parallelism knob** — how many workers dispatch in parallel.
It is NOT a 1-worker limit, and it must NOT gate reaping. Today the cap gate is
wired in three places: `prune_worktrees` is `cap > 1`-only; `_apply_reconcile`
gets `repo=cwd if cap > 1 else ""` + `tasks_by_slug=… if cap > 1 else None`; and
`_maybe_remove_worktree` early-returns when `not tasks_by_slug or not repo`. So a
coord now running **`cap==1`** that inherited worktrees from an **earlier
`cap>1` run** (the exact 6.9 GB leak — worktrees persist across cap changes)
passes empty reconcile inputs and **never reaps them**.

rev17: reaping keys on whether the **task has a persisted `worktree=` path**, at
EVERY cap. Always pass `repo=cwd` + the real `tasks_by_slug` whenever the project
has any worktree on disk OR any task carries a `worktree=` field — independent of
`cap`. `_maybe_remove_worktree` then early-returns only when the *specific task*
has no `worktree=` (the correct per-task guard, byte-identical-safe for genuinely
worktree-free tasks). To prevent re-introducing the cap gate on any path, ONE
helper **`_worktree_cleanup_context(cwd, tasks)`** returns the
`(repo, tasks_by_slug)` pair keyed on "project has any worktree"; EVERY
`_apply_reconcile`/`_apply_sentinel` call site (primary drain, supervisor
reconcile, deferred-sentinel replay, supervisor drain) obtains its arguments from
that single helper. No call site computes the cap gate inline.

### 4.4 Go `fleet gc --kinds=worktrees` — defense-in-depth orphan-dir resolver

For trees the reconcile path missed (coord crashed mid-reconcile, or a
non-reconcile terminal transition), the coord tick invokes `fleet gc
--kinds=worktrees --apply --project <p>` every-N-ticks; the operator runs the
same command by hand. ONE Go implementation, not Python/Go duplication. For
these the invariant is **task-terminal (on disk) + dirty-guard + force=False**
(no liveness needed; defense-in-depth).

**Orphan-dir → task resolver** (leaked dir names are heterogeneous: full-slug,
`-rand`-stripped, mid-word-truncated, and no-matching-task):

1. **exact slug match** → use it, **AND require branch identity** (the dir has
   the task's `worker/<slug>` branch checked out; expected branch =
   `task.branch`, or the deterministic `worker/<slug>` fallback when
   `task.branch == ""`). A `worktrees/<terminal-slug>/` can hold a CLEAN checkout
   of the WRONG branch, and `force=False` does NOT protect a clean tree.
2. else **unique-prefix match corroborated by branch identity** — the dir
   actually has the candidate's `worker/<slug>` checked out
   (`git -C <dir> rev-parse --abbrev-ref HEAD` == candidate `branch`). The stored
   `worktree=` is only **supporting evidence**, never an OR substitute for branch
   identity.
3. else (ambiguous >1 candidate, no branch-identity corroboration, or no matching
   task) → **SKIP + SURFACE**, never auto-remove.

Three more Go-side guards, each a consequence of "never reap a clean tree you
can't positively identify as a current-terminal worktree":

- **Repository-registration identity.** Beyond branch identity, the dir must be
  **registered in the EXPECTED PROJECT base repo's** `git worktree list
  --porcelain`, and removal runs **through that base repo** (`git -C <base-repo>
  worktree remove --no-force <path>`). A clean wrong checkout that happens to sit
  at the deterministic path on `worker/<slug>` would still pass a branch-only
  gate; repo-registration closes it. (`Deps` gains the project base-repo path.)
- **Generation-aware non-terminal `state.json` belt** (R7 above). Only a
  CURRENT-generation non-terminal state vetoes; a stale-generation non-terminal
  state is surfaced but never vetoes (else the leak never reaps).
- **Live-over-archive precedence.** Exact slug resolution prefers a LIVE
  `tasks.md` row over an archive row (an archive candidate is eligible only when
  no live row exists). Otherwise an archived terminal row could become
  destructive evidence for a same-slug live non-terminal task. Conflicting exact
  candidates → surface + skip.

**Required Go projection seam.** Today `Deps` exposes only `{Project, Slug,
Path}` worktree entries + `IsTaskTerminal(project, slug) bool`, and `WorkerState`
drops `phase`. The resolver CANNOT be built on that. So a **required acceptance
item**: extend `Deps` with a **full task-candidate index** — ALL live + archive
candidates (`{status, branch, worktree, archive-source, worker-phase,
task-gen, worker-state-gen}`), NOT a per-dir exact `IsTaskTerminal` lookup. The
resolver matches dirs against that index (exact → unique-prefix);
`reconcileWorktrees` consumes it. Removal does not change until the index exists.

Report a count: **N reaped / M dirty-parked / K skipped (ambiguous/no-task)** so
a no-op is visible. Fail-soft: a gc nonzero never wedges the tick (captured to
stderr as a warning).

**Deferred [P2] — Go-gc `parked` persistence.** When `fleet gc
--kinds=worktrees --apply` dirty-parks a `done` task's tree, the `parked`
task-row field (section 4.2) must be set so `_sweep_done_worker_dirs` skips that
dir — but the Go path's contract here is only "report counts," and Go writing
the Python-owned `parked` field is a layering question. Resolution options: (a)
Go gc persists `parked` directly, or (b) the coord consumes the structured dirty
report and sets `parked` before the next done-dir sweep. Deferred to
implementation; either option closes it. (Coord-invoked gc on a non-`done`
terminal task is unaffected — the sweep only targets `done`.)

---

## 5. Path C — DEFERRED (not in this design)

Orphan-recovery of a NON-terminal worker (requeue + reap when the worker is truly
dead) requires deciding a worker is dead — but **post-#84 workers are in-process
Agent subagents** (`dispatch.py`): no `fleet-<agent_id>` tmux session to probe,
`worker_pid` is the coord's pid, `worker_subagent_ids` are opaque, and
`state.json` is written only at phase boundaries (not a heartbeat). There is
currently **no sound destructive-grade liveness signal** for an in-process
worker; doing Path C on these signals re-introduces mid-work work-loss. So Path C
is split out to **`coord-orphan-worker-recovery`**, blocked on first adding a
worker **heartbeat/lease** primitive. Until then, orphaned non-terminal worktrees
are left in place (disk cost bounded; they convert to reapable once their task
reaches terminal via the existing stuck/reconcile paths).

**Path B needs NO new persisted fields beyond `dispatch_generation` + `parked`.**
It removes only the worktree DIR (never deletes branches in this pass), so no
`base_sha`; it does no liveness probe, so no `tmux_session`/`tmux_socket`. (Those
move to the deferred Path C design, IF a heartbeat/lease makes it viable.)

---

## Safety / failure modes

- **Stale attempt:** never drives mutation/removal — `read_current_worker_state`
  returns `stale`, which short-circuits; the CAS rejects stale writes. (The
  10-round bug class, closed at the chokepoint.)
- **Worker not done (current-generation non-terminal phase):** never reaped (the
  reader returns `current` + non-terminal; the Go belt vetoes).
- **Dirty worktree:** never reaped — `git status --porcelain` non-empty → PARK +
  SURFACE, re-checked inside the GC lock immediately before remove.
- **force=False:** a tree that turns dirty between check and remove makes `git
  worktree remove` FAIL → surfaced, not blown away.
- **Branch never deleted in this pass** — only the worktree DIR — so no
  committed-work-loss; branch cleanup is a separate future gc.
- **Reap failure (locked/busy):** surface the path + skip; never force.
- **Fail-soft:** a gc error never wedges the tick.

---

## Concurrency

- **Worker lock** guards every `fleet workers update` CAS (2.2) — the existing
  per-slug worker lock; the task-row generation read + the state write happen
  under it.
- **Project-scoped worktree-GC flock** taken inside the Go command around
  scan → re-check-porcelain → `git worktree remove`, so a coord-invoked run and an
  operator-invoked run can't race the same removal (flock discipline per #184).
- **#184 per-journal flock** (`withJournalLock`) remains the concurrency
  primitive for the dispatch **journal** — i.e. for #184's own *launch*-path
  generation and the `mark-launch-attempted` CAS. The worker-state fence token
  (`dispatch_generation`) is a SEPARATE coord-owned per-slug counter on the task
  row (§1); rev18+ does **not** read any fence token out of a journal and does
  **not** change the journal's locking. The two tokens are distinct: journal
  `Generation` fences launch, task-row `dispatch_generation` fences worker-state.
- **Bounded cadence:** coord invokes gc every-N-ticks (or only when candidates
  exist), not every tick.
- **Cross-project:** the coord invokes gc only for ITS project (coord-scope
  strict). Host-wide reclaim stays the operator's per-project `fleet gc`.

---

## Test plan (per `feedback_e2e_tests_for_all_cases`)

Tests are organized by the chokepoint, then its consumers — mirroring the body.

### T1 — Chokepoint reader (`read_current_worker_state`), the load-bearing P0

- **`stale` ≠ `missing`:** a stale terminal `state.json` (wrong generation) →
  helper returns `stale` → reconcile/reaper/handoff/stuck-check short-circuit for
  that slug: NO `status=todo` flip, NO `clear_worker`, NO `delete_worker_dir`, NO
  worktree removal, NO nudge/escalate/block. **Assert the P0:** treating `stale`
  as `missing` would mutate + remove the current attempt.
- **`missing` keeps died-without-PR:** a genuinely absent state → existing
  died-without-PR semantics still apply.
- **`current` proceeds:** a matching-generation state drives reconcile/reaper as
  normal.
- **Every reader R1–R8 is covered:** one test per reader proving a `stale` state
  produces no mutation on that path specifically (reconcile alive-check,
  reconcile died-without-PR fall-through, reaper input, supervisor stuck-check,
  Go gc belt, **and the handoff-dispatch reader R8**).
- **R8 stale double-dispatch (the new P1):** a stale `review-pending`/
  `review-done` `state.json` on a re-dispatched slug → `_dispatch_review_handoffs`
  short-circuits: **NO** prior-claim release, **NO** pending-acquire mutation,
  **NO** reviewer/finisher DISPATCH block emitted. Assert the hazard: treating the
  stale phase as `current` would release the live claim + emit a second finisher
  that writes an accepted current-gen `phase=done`.

### T2 — Chokepoint writer (`fleet workers update --dispatch-generation` CAS)

- **task-row authority + absent file:** `--dispatch-generation <stale>` against a
  re-dispatched slug → REJECTED under the worker lock + surfaced, current
  `state.json` not clobbered — **including when the file is absent** (a stale
  worker may NOT bootstrap/poison it; a missing file is bootstrapped ONLY by the
  current generation). A matching-generation update succeeds.
- **repair = fresh replacement:** current gen overwrites a stale-gen file → new
  state has EMPTY `pr_url`/review/completed-phases + fresh `started_at` (assert
  the old PR does NOT carry into the branch→PR fallback).
- **coordinator-authored writes carry the generation (W3):**
  `_mark_worker_blocked` issues `--dispatch-generation <current>` → accepted; a
  stale-generation coord write is CAS-rejected (no mutation of the current
  attempt).
- **handoff inherits (W2):** a reviewer/finisher handoff inherits the dispatching
  attempt's generation (unchanged across handoff); the finisher `phase=done`
  carries it and is accepted; a genuine re-dispatch increments.

### T3 — Dispatch ordering + retry

- **per-slug monotonic increment (the P1-A fix):** two SUCCESSIVE genuine
  re-dispatches of the same slug carry **strictly increasing** task-row
  `dispatch_generation` (attempt A = N, attempt B = N+1) — assert that even when
  each underlying #184 journal starts at `Generation == 0`, the task-row counter
  does NOT reset; attempt A's stale `phase=done` `state.json` compares `stale`
  (not `current`) against attempt B's authority and drives no mutation.
- **exact dispatch order:** task-row `dispatch_generation` is persisted BEFORE
  `state.json` bootstrap and BEFORE prompts are built; a stale worker's update
  arriving after step (2) is rejected (no window).
- **no launchable DISPATCH while still `ready` (duplicate-dispatch guard):** the
  `dispatch_generation` persist + `status=in-progress` flip land in the SAME
  pre-launch commit; a crash/retry simulated between the commit and prompt-emit
  never re-emits a launchable DISPATCH block for an already-`in-progress` slug
  (assert: a slug observed `ready` has no DISPATCH collected; the block is
  collected only after the status flip + generation persist succeed).
- **pending-acquire retry reuses generation:** a retried `already_acquired`
  dispatch keeps its generation (assert task row + prompt generation stay equal;
  a worker update with that generation is accepted, NOT wedged). A re-mint path
  releases+rewrites the prompt before incrementing.
- **pending-acquire record stores generation:** the entry carries
  `{agent_id, dispatch_generation, dispatch_kind}`; a stale entry surviving into a
  fresh attempt is detected by generation mismatch and forced to release+rewrite.
- **pending-acquire wrong-KIND reuse is refused:** a stale worker-kind pending
  entry surviving into a `_dispatch_review_handoffs` dispatch with the SAME
  slug+generation but a different `dispatch_kind` (reviewer/finisher) → NOT
  reused: the stale claim is forgotten+released and the prompt rewritten before
  proceeding (assert the reviewer/finisher does NOT inherit the worker prompt via
  `already_acquired`). Symmetric for a stale reviewer entry hitting a worker
  re-dispatch.

### T4 — Sentinel path (generation token)

- a STALE `task_done_pr`/`worker_failed` sentinel for a re-dispatched slug → ALL
  terminal side effects skipped (status unchanged, `pr_url` not set, worktree not
  removed, worker dir kept, slug→agent_id mapping NOT released/forgotten, handoff
  state NOT cleared) — only surfaced.
- **same-slug stale with identical branch/path:** the stale sentinel carries the
  IDENTICAL `worker/<slug>` branch + deterministic path; the **generation token**
  mismatches → skip-all + surface (assert branch/path-OR alone would wrongly
  pass; the live tree survives).
- **caller gating (S4):** on `skipped_stale`, the CALLER does NOT release/forget
  or clear handoff state — tested at the primary-drain, supervisor-replay, AND
  supervisor-drain sites.
- **stale `blocked_question` (S3):** token-mismatch → `skipped_stale`, the current
  worker not flipped to `blocked`, no note written. `new_task` stays token-free.
- **`error` requeues, `skipped_stale` consumed:** a transient apply failure →
  `error` → re-deferred (not lost as the watermark advances); `skipped_stale` →
  consumed.
- **deferred→replay round-trip (S5):** a legitimate terminal sentinel deferred
  behind the reaper lane carries its `Generation` through `_save`/`_load`; on
  replay it corroborates + applies (no fail-open removal, no fail-closed leak).
- **tokenless-legacy rollout:** a pre-migration tokenless sentinel whose slug was
  NOT re-dispatched → legacy-trusted + applies; one whose slug WAS re-dispatched
  → `skipped_stale` + surface (re-dispatched live tree survives).

### T5 — Worktree reaping (the consumer)

- clean terminal (branch matches) → removed (`force=False`), outcome=`removed`.
  Dirty → `dirty-parked`, NOT removed (fails-on-parent: parent `force=True` blows
  it away). Test via reconcile AND `_apply_sentinel` callers.
- **branch-identity corroboration:** `worktree=` points at a CLEAN checkout whose
  branch != expected → SKIP + surface, tree untouched.
- **branch-mismatch-with-tree is tree-left, NOT noop:** wrong-branch registered
  tree → caller KEEPS the worker dir + on `todo`/`worker_failed` flips
  `status=blocked`.
- **branch-empty fallback:** `branch==""` but `worker/<slug>` checked out →
  expected-branch defaults to `worker/<slug>` → terminal clean tree IS reaped.
- **TOCTOU dirty → `dirty-parked` not `error`:** tree turns dirty between
  porcelain-check and remove → outcome=`dirty-parked`, surfaced, worker dir kept.
- **`error` on a redispatch-eligible path → `status=blocked`:** tree left on disk
  → task flipped `blocked` (NOT merely marker-cleared); the next promote does NOT
  reuse the tree.
- **dispatch preflight on the COMPUTED path:** a `status=ready` task with
  `worktree=""` but a registered tree at the computed `worker/<slug>` path whose
  branch != `worker/<slug>` OR dirty → dispatch REFUSES + surfaces.
- **any tree-left keeps worker dir:** `dirty-parked` AND `error` → caller SKIPS
  `_maybe_delete_worker_dir` (assert `state.json` survives); `removed`/`noop` →
  delete runs.
- **caller-specific dirty PARK:** dirty park on `worker_failed`/reconcile-`todo`
  → `blocked`; on `task_done_pr` → stays `in-review`; on reconcile-`done` → stays
  `done`. Each surfaces the dirty path.
- **parked done-dir survives the sweep:** a dirty-parked `done` task →
  `_sweep_done_worker_dirs` SKIPS it (parked marker) → `state.json` survives
  across ticks until park cleared.

### T6 — cap-independent reap

- coord at **`cap==1`** that inherited a terminal task's `worktree=` from an
  earlier `cap>1` run → the tree IS reaped. A genuinely worktree-free `cap==1`
  task → no-op (per-task `not t.worktree` guard).
- **every apply call site** (primary drain, supervisor reconcile, deferred
  replay, supervisor drain) obtains `(repo, tasks_by_slug)` from
  `_worktree_cleanup_context` → each reaps an inherited `cap==1` worktree.

### T7 — Go `fleet gc --kinds=worktrees` orphan-dir resolver

- exact-slug terminal dir **with `worker/<slug>` checked out** → reaped.
- exact-slug terminal dir holding a CLEAN WRONG-branch checkout → SKIP + surface.
- unique-prefix branch-corroborated terminal dir → reaped (fails-on-parent: exact
  lookup no-ops).
- unique-prefix but WRONG/stale `worktree=`, no branch identity → SKIP.
- ambiguous prefix → SKIP + surface, none removed.
- prefix of a LIVE in-progress task → SKIP, tree untouched.
- no-matching-task dir → SKIP + surface, never removed.
- archived-but-non-terminal-phase (CURRENT-generation) state → SKIP + surface.
- **generation-aware belt:** a STALE-generation non-terminal state beside a
  terminal task → does NOT veto → terminal clean tree IS reaped + state surfaced;
  a CURRENT-generation non-terminal state → vetoes.
- **live-over-archive precedence:** same slug in BOTH live (non-terminal) and
  archive (terminal) → resolver prefers LIVE → NOT reaped; conflicting exacts →
  surface + skip.
- **repo-registration identity:** clean tree at the deterministic path on
  `worker/<slug>` but NOT registered in the expected base repo → SKIP + surface;
  a properly-registered terminal tree → reaped through the base repo with
  `--no-force`.
- dirty terminal → parked; report prints N reaped / M dirty-parked / K skipped.
- flock: coord-invoked + operator-invoked gc don't race the same removal.
- cadence: tick invokes gc at most every-N-ticks; nonzero gc → fail-soft.

### T8 — Schema rollout

- Python + Go parsers accept `- dispatch_generation:` AND `- parked:` (no parse
  error / no refuse-to-tick); `fleet tasks set <slug> dispatch_generation=` /
  `parked=` round-trip; a tasks.md carrying both parses cleanly BEFORE any writer
  is exercised.
- `parked` clears on resolve (status leaves `blocked` or `parked=` clear); the
  sweep re-arms.

(Path C orphan/liveness tests → deferred `coord-orphan-worker-recovery`.)

---

## Appendix — superseded review history (rounds 1–15, one line each)

Each round below was a per-path patch for the SAME stale-attempt / generation
class. rev17 subsumes ALL of them into the section-2 chokepoint; they are kept
only for the audit trail. The "Where" file:line citations are point-in-time and
must be re-verified at implementation.

- **Round 1** — destructive decision reuses `_WORKER_STATE_FRESH_S` (no new
  timer); tri-state liveness (tmux + freshness) as the guard; dirty never
  force-removed.
- **Round 2** — terminal-reap (Path B) keys on task-terminal alone, NOT liveness
  (the agent_id map is forgotten on terminal); persist tmux session/socket for
  Path C; both paths `force=False` + porcelain re-check inside the lock; ONE Go
  impl, coord invokes it.
- **Round 3** — Path C deferred (in-process workers have no sound liveness);
  branch never deleted in this pass (no `base_sha`); deleted the impossible
  "terminal AND live-non-terminal" precedence test; tick treats `fleet gc`
  nonzero as a fail-soft warning.
- **Round 4** — found rev4 was written against the wrong code model: the live
  code ALREADY reaps terminal worktrees; there is a remover to make SAFE + a
  slug-match bug, not a missing reconcile pass.
- **Round 5 (P1)** — SAFE orphan-dir resolver: exact → unique-prefix-branch-
  corroborated → else skip+surface (naive prefix-match could reap a live/clean
  wrong tree).
- **Round 6 (6×P1)** — P1a sentinel attempt-provenance gate; P1b caller-specific
  dirty-park (not blanket blocked); P1c `remove_worktree` returns
  `removed|dirty-parked|noop|error`, callers skip delete on dirty-parked; P1d reap
  keys on `worktree=` not `cap`; P1e Go prefix recovery requires branch identity
  (not `worktree=`-OR); P1f Go non-terminal-`state.json` belt.
- **Round 7 (5×P1)** — P1-i provenance gates the WHOLE sentinel (all terminal side
  effects); P1-ii Python remover needs branch identity too; P1-iii TOCTOU →
  `dirty-parked` not `error`; P1-iv `_worktree_cleanup_context` at every call
  site; P1-v deferred-sentinel queue persists provenance.
- **Round 8 (3×P1 + 1 promoted-P2)** — P1-A provenance MUST be a unique
  per-attempt token (branch/path are deterministic per slug); P1-B Go exact-match
  needs branch identity; P1-C `error` blocks redispatch too; P1-D Go projection
  seam is an acceptance item.
- **Round 9 (4×P1 + 1 P2)** — P1-A `_apply_sentinel` returns
  `applied|skipped_stale|error`, callers gate release/forget/handoff-clear; P1-B
  `blocked_question` is NOT token-exempt; P1-C branch-mismatch is tree-left not
  noop; P1-D `status=blocked` (not "and/or") + dispatch preflight; P2-E branch-
  empty `worker/<slug>` fallback.
- **Round 10 (3×P1 + 2×P2)** — P1-A drop the branch/path OR escape-hatch; P1-B
  token plumbing migration (mint agent_id before prompt, inject, extend grammar,
  legacy rollout); P1-C `error` retry semantics; P2-D parked-done sweep guard;
  P2-E Go full task-candidate index (not per-dir exact lookup).
- **Round 11 (4×P1 + 1 P2)** — P1-A `state.json` reconcile-path provenance; P1-B
  persisted dispatch-generation epoch first, tokenless fails closed; P1-C Go
  repo-registration identity + remove through base repo `--no-force`; P1-D
  preflight on the COMPUTED path; P2-E one durable `parked` schema.
- **Round 12 (3×P1 + 1 P2)** — P1-A write-time CAS (`--dispatch-generation`
  rejects stale writes under the worker lock); P1-B single epoch-filtered helper
  makes stale ABSENT on every path; P1-C per-attempt epoch through handoffs; P2-D
  `parked` schema-rollout order.
- **Round 13 (1×P0 + 3×P1)** — **P0** tri-state `current|stale|missing` helper
  (`stale` ≠ `missing`); P1-B `dispatch_generation` schema rollout before any
  writer; P1-C task-row CAS authority (not on-disk alone); P1-D exact dispatch
  order.
- **Round 14 (3×P1 + 1 P2)** — P1-A pending-acquire retry reuses generation; P1-B
  repair = fresh replacement (not field-merge); P1-C generation-aware Go belt;
  P2-D live-over-archive precedence.
- **Round 15 (2×P1 + 1 P2)** — P1 supervisor `run_stuck_check` generation-aware;
  P1 coordinator-authored writers carry the generation; P2 pending-acquire stores
  `{agent_id, dispatch_generation, dispatch_kind}`. **This round's recurrence
  (the same class on yet another path) is what motivated the rev17 chokepoint
  restructure.**

## Open questions (for the operator)

1. **Cadence N** (every-N-ticks) and/or only-when-candidates — pick a value.
2. **Dirty terminal worktree:** park+surface only (default), or also offer
   `fleet gc --kinds=worktrees --apply --force` (operator-explicit) to reap a
   dirty terminal tree?
