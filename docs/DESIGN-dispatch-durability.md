# DESIGN — dispatch durability on broken stdout (fleet#184)

**Status:** DRAFT rev6 — architecture confirmed SOUND by round-5 codex+claude (flock RMW + gen-token + atomic reset); rev6 applies the round-5 *mechanical/doc* fixes (tri-state protocol contract, lock idiom, per-id granularity, stdin-before-lock, local-fs). Pending round-6 confirmation → operator approval (`feedback_plan_dual_review_before_impl`)
**Priority:** P1 (blocks loop-supervisor-sigpipe PR + the loop.py backlog)
**Issue:** fleet#184
**Author:** coord `cedacc55`, 2026-05-31
**Review log:**
- rev1 → codex+claude CONCERNS. Core flaw: replay keyed on register-ack would double-launch. Fixed in rev2.
- rev2 → codex+claude CONCERNS: architecture sound + double-launch closed *in concept*, but 4 leaks. Fixed in rev3: (1) journal CAS must be **Go-owned**; (2) EVERY emit must pass the CAS gate; (3) residual crash-after-CAS repair; (4) `ReleaseCoordPromptInbox` clobber.
- rev3 → CONCERNS: (A) "Go-owned" ≠ atomic CAS; (B) incomplete emit inventory. rev4 tried generation-field + reset-for-relaunch.
- rev4 → codex+claude CONCERNS: **the rev4 fix was UNSOUND.** A "lock-free generation int + read-then-WriteAtomic" is NOT a CAS: `WriteAtomic` (state.go:349) does an unconditional `os.Rename` (no `O_EXCL`/`RENAME_NOREPLACE`); generation is compared in each process's memory at read time, nothing makes the *commit* conditional → A reads genN, B reads genN, A renames N+1, B renames N+1 clobbering A → lost update → double-launch survives. Rejecting flock (rev4 lines 113-114) was the bug. rev5 fixes: **(A)** real serialization — per-journal **flock** held across read→predicate→mutate→WriteAtomic for ALL writers; generation kept as an under-lock audit/version field; CAS-contention exhaustion is a *transient* error, never silently skips a launch. **(B)** `reset-for-relaunch` made atomic with the inbox rewrite (one locked Go op) + the DISPATCH block carries a launch token (generation) that `mark-launch-attempted` validates, so a stale block can't consume a later lifecycle. **(C/P2)** the single-emit reserved set extended to the handoff emitters (loop.py:860, 1202) in primary + supervisor flows.

## Problem

`loop.py` dispatches a worker by: (1) writing+fsync'ing the inbox prompt
`~/.fleet/inbox/<agent_id>.md` (and a journal entry
`~/.fleet/dispatches/<agent_id>.json`), (2) recording the task
`in-progress` + bootstrapping `workers/<slug>/state.json`, (3) **printing
the DISPATCH block to stdout** for the coord agent to read and launch the
host Agent.

If step (3)'s print fails (broken stdout — the `| head` SIGPIPE incident)
or the tick dies between record-and-print, the task is recorded dispatched
but **no Agent is launched**: a *phantom dispatch*. loop-supervisor-sigpipe
stops the infinite wedge (the loop now exits on broken stdout) but the
mid-emit dispatch is still lost.

**Corrected recovery-timing (rev1 was wrong):** there is NO fast backstop.
`_apply_dispatch` bootstraps `state.json` with a fresh `updated_at`, so
`_worker_state_fresh` treats the phantom as **alive for
`_WORKER_STATE_FRESH_S` = 15 min**. And `supervisor.is_stuck_idle` gates
on `tmux_session_alive` (supervisor.py:361) — an Agent-tool worker has **no
tmux session**, so the stuck-idle ladder **never fires** for phantoms. The
only existing reclaim is primary reconcile after the ~15-min freshness
window (worker goes stale, no pr_url → requeue). 15 minutes of a silently
dead task, not 180s. This strengthens the case for fast, correct replay.

## Goal

A dispatch loop.py *recorded* is *guaranteed acted upon* (Agent launched),
fast (next healthy tick), with **no double-launch** and no
`state.json`-before-spawn race, and a durable escalation when stdout stays
broken.

## The exactly-once boundary (the crux rev1 got wrong)

The coord protocol (SKILL.md) launches the Agent *before* `register_subagent`,
and registration is **best-effort (skipped on lock contention; worker still
runs)**. So `register_subagent`'s ack has routine false-negatives: a healthy
launched worker frequently has no ack. **Replaying on "no ack" re-emits the
block → a second live Agent on the same agent_id/inbox/worktree.** Two
workers on one branch — worse than the phantom.

Exactly-once across the non-transactional (stdout-print + Agent-launch)
boundary is impossible *unless the launch attempt itself is durably
recorded before the Agent is invoked.* So we add a **launch-attempt state**
the coord flips durably *before* invoking the Agent, and replay only
genuinely-never-launched dispatches.

## Design (rev2)

### Single source of truth: the dispatch journal
Use the existing `~/.fleet/dispatches/<agent_id>.json` (NOT new task fields,
NOT a new sidecar — avoids a second source of truth). Give it an explicit
launch-state machine:

```
ExecPending         inbox written, task recorded, block NOT yet emitted/launched
ExecLaunchAttempted coord durably CAS-flipped this IMMEDIATELY before invoking Agent
ExecAcked           register_subagent succeeded (best-effort; may be skipped)
ExecBlocked         dispatch could not be delivered (see escalation)
ExecDone/Released   terminal
```

**Required change to acquire-prompt** (api.go:67 currently sets
`ExecInFlight` at inbox-write — premature): acquire leaves the entry at
**`ExecPending`**. It is the COORD that flips `ExecPending →
ExecLaunchAttempted`, via a Go CAS subcommand run **immediately before** the
Agent tool invocation.

**The CAS MUST be Go-owned (rev3 — round-2 P1).** `SaveJournal` is a bare
`WriteAtomic` (tmp+rename, store.go:79-90) with **no lock** —
`coordinator.lock` guards only `coord-state.json`, not the journal. A Python
`tmp+rename` read-modify-write would race the 4+ Go journal writers
(acquire / release / acquire-recovery / sweeper): atomic rename ≠ CAS, so a
lost update could leave the entry at `ExecPending` *after* a launch → the
next replay double-launches, **reopening the rev1 P0.** Therefore launch-
marking is a Go subcommand on the same write path with a real
`exec_state` compare-and-set: **`fleet claims mark-launch-attempted <id>
<gen>`** (tri-state result — see the authoritative §"Coord protocol change"
step 2: ok / predicate-fail / contention; never a binary "nonzero→skip").
No Python journal writer is introduced. (Resolves Open Q#2: Go, not optional.)

### Journal concurrency: versioned optimistic CAS (rev4 — round-3 P1-A)
"Go-owned" is necessary but NOT sufficient: `SaveJournal`/`writeJournal`
(store.go:63-93) is bare `state.WriteAtomic` (tmp+rename+fsync) with **no
lock and no version check**. Atomic rename prevents torn writes but NOT
lost updates across a read-modify-write — and rev3 added a *second* RMW
writer (the `replay_emit_attempts` increment), so two writers can interleave
and clobber the launch flip (the round-3 P1-A interleave → double-launch).
`coordinator.lock` does NOT help (it guards coord-state.json, and the
mark-launch CAS runs outside the tick, across the agent-turn boundary).

**Mandate (rev5): a real lock.** `WriteAtomic`'s unconditional `os.Rename`
cannot provide compare-at-commit, so a lock-free generation field is NOT a
CAS (rev4's mistake). Instead: a **per-id `flock`** on
`~/.fleet/dispatches/<id>.json.lock` (rev6: per-id, NOT a dir-wide `.lock` —
matches the existing `LockAgent` per-agent precedent, lock_unix.go:75-89; a
dir-wide lock would needlessly serialize every dispatch launch coord-wide)
held across the ENTIRE read → predicate → mutate → WriteAtomic critical
section, taken by **EVERY** journal writer: acquire, release,
mark-launch-attempted, mark-acked, reserve-replay, reset-for-relaunch,
sweeper.

**Lock idiom (rev6):** non-blocking `LOCK_EX|LOCK_NB` + a bounded
retry/backoff loop with a deadline (mirrors coordlock/rc). On deadline →
return `contention` (transient; caller retries) — **never** unbounded
`LOCK_EX` (would stall the tick) and **never** map `EWOULDBLOCK` → skip
(would drop a launch). flock auto-releases on process death (fd close;
verified against lock_unix.go:108-113), so a dead coord cannot hold it.
**Requires `~/.fleet` on a local POSIX fs** (flock over NFS is unreliable);
fail closed with a clear diagnostic otherwise. Under the lock the
read-modify-write is genuinely atomic — the losing interleave is impossible
because B blocks until A's rename completes, then reads A's fresh state. A
`generation int` is kept as an **audit/version field bumped under the
lock** (debuggability + stale-block detection, below), not as the
concurrency primitive. **No Python journal writes**: reserve-replay /
mark-launch-attempted / mark-acked / reset-for-relaunch are Go subcommands
that take the flock; loop.py only READS (a lock-free read of an
atomically-renamed file is fine — it sees one consistent version).
**Contention vs predicate-fail are DISTINCT exits:** `mark-launch-attempted`
returns (a) `ok` (was ExecPending, flipped), (b) `predicate-fail` (not
ExecPending — already attempted; coord skips, correct), (c) `contention`
(could not acquire flock within timeout — TRANSIENT; coord must retry, NEVER
treat as skip, or it silently drops a launch). (flock's "lock-ordering
surface" — rev4's rejection reason — is moot: there is exactly one lock, the
journal's; nothing else is taken inside it.)

### DISPATCH emit inventory + universal gating (rev4 — round-3 P1-B)
ALL emit paths, and how each enters the gate (every launch the coord makes
passes `mark-launch-attempted <id>` in protocol step 2):

| Emit path | Journal state at emit | Gate behavior |
|---|---|---|
| primary worker (loop.py:918) | acquire → `ExecPending` | CAS-gate as designed |
| primary handoff (loop.py:860) | acquire → `ExecPending` | CAS-gate as designed |
| supervisor worker (loop.py:1248) | acquire → `ExecPending` | CAS-gate as designed |
| supervisor handoff (loop.py:1202) | acquire → `ExecPending` | CAS-gate as designed |
| tick-entry replay | `ExecPending` only | CAS-gate; reserve via Go |
| **`handoff_resume.py:366` (re-launch existing id)** | terminal/acked from the ORIGINAL dispatch | **must reset to a fresh `ExecPending` first** |

The resume case is the round-3 P1-B trap: `handoff_resume.py` re-emits a
block for an *existing* agent_id whose journal entry is `ExecAcked`/terminal
— a blind `mark-launch-attempted` returns predicate-fail → coord skips →
resume never launches. Fix: resume **journal-owns its re-launch atomically**
(rev5 — closes the round-4 reset↔replay race): a single locked Go op
**`fleet claims reset-for-relaunch <id>`** that **reads the new prompt fully
from stdin into memory FIRST, THEN takes the flock** (rev6 — never hold the
journal lock across a slow/stalled stdin pipe) and, under the flock, does
only the two bounded writes: (1) rewrite the inbox prompt AND (2) reset the
entry to fresh `ExecPending` with a **bumped generation** and
`replay_emit_attempts=0` — in one critical section, so no tick can observe
the reset `ExecPending` before the inbox is in place. (Same "read input
before lock" rule applies to acquire.)

**Stale-block guard (rev5 — round-4):** every DISPATCH block carries the
entry's `generation` as a launch token; `mark-launch-attempted <id> <gen>`
flips only if the on-disk generation still equals the token (under the
flock). A stale re-emitted block (from a tick that read an older lifecycle)
carries an old gen → predicate-fail → cannot consume a later lifecycle's
launch. This also makes replay safe against reset-for-relaunch: replay's
reserved block carries gen N; if a relaunch bumped to N+1, the replay block
is stale and rejected.

**No emit path bypasses the gate; every gated launch starts from a fresh
`ExecPending` whose generation the launch block must match.**

### Coord protocol change (SKILL.md "Worker dispatch protocol")
For each DISPATCH block, the coord now:
1. Reads `prompt_file`. The block carries the entry's `<gen>` launch token.
2. **`fleet claims mark-launch-attempted <agent_id> <gen>`** — under the
   flock, flip `ExecPending → ExecLaunchAttempted` (+ `launch_attempted_at`)
   **iff** state is `ExecPending` AND on-disk generation == `<gen>`. THREE
   distinct exit codes — the coord MUST branch on all three (collapsing to
   "nonzero → skip" is the lost-launch bug):
   - **`ok` (0)** → launched the flip; **proceed to step 3 (launch Agent).**
   - **`predicate-fail`** (state ≠ ExecPending, or gen mismatch = stale
     block) → another tick/path already owns this launch → **skip, do NOT
     launch.**
   - **`contention`** (could not take the flock within the deadline) →
     **TRANSIENT** → **retry the SAME block** (next tick re-emits it; never
     treat as skip — that would silently drop the launch).
3. Invokes the Agent tool once.
4. `register_subagent` (best-effort) → `ExecAcked`.

(SKILL.md's "Worker dispatch protocol" is updated to this tri-state contract
verbatim — the old binary "nonzero → skip" wording is removed.)

### Universal CAS-gate invariant (rev3 — round-2 P1)
There are TWO emit channels in loop.py: the new tick-entry **replay** and
the existing **`pending_acquire_agent_ids` retry** inside `_dispatch_ready`
(on acquire-success + `_apply_dispatch`-failure the slug stays `ready` and
the id is retried). Both can emit a block for the same id in one tick. The
binding invariant: **every DISPATCH emit — replay AND `_dispatch_ready`
retry — targets a journal entry that the coord will CAS-gate in step 2, and
at most ONE block per id is emitted per tick.** Implementation: ONE tick-wide "already-emitted-this-tick" set keyed by
agent_id, shared by **all** emitters — replay, `_dispatch_ready`, AND the
review/handoff emitters in BOTH primary (loop.py:860, :918) and supervisor
(loop.py:1202, :1248) flows (rev5 — round-4 P2: a handoff-apply failure can
leave an `ExecPending` handoff id that both replay and the handoff path
emit). Replay-emitted slugs are removed from `pending_acquire` (one owner).
Regression tests: "at most one block per id per tick" for replay-vs-primary
AND replay-vs-handoff (primary + supervisor). The flock CAS in step 2 (with
the generation token) is the final backstop — even if two blocks reach the
coord, only the one matching the on-disk generation flips `ExecPending` and
launches; the stale one predicate-fails.

### Residual crash repair (rev3 — round-2 P1; resolves Open Q#3)
A crash *after* step-2 CAS but *before* step-3 invoke leaves
`ExecLaunchAttempted` with no Agent and no ack — a silent phantom (replay
correctly suppresses `ExecLaunchAttempted`, and bootstrapped `state.json`
reads alive for 15 min / indefinitely with no `updated_at`, and stuck-idle
never fires without a tmux session). So: on tick entry, for any
`ExecLaunchAttempted` with no `ExecAcked` and no live registered subagent
where `now - launch_attempted_at > LAUNCH_ACK_GRACE` (short, e.g. 1–2
ticks), transition to `ExecBlocked(reason="launch_unconfirmed")` + off-
channel escalation (NOT trusting bootstrapped state.json freshness). This
is registration-repair / raise, **never blind replay**, so it cannot
double-launch. Only then is "guaranteed acted upon, fast" actually true.

### Replay predicate (runs at tick entry, before new dispatch)
```
for each journal entry with delivery live for this project:
  if state == ExecPending and replay_count < CAP:
      # genuinely never launched (coord never flipped it) → safe to re-emit
      replay_count++ ; last_replay_at=now      # persist BEFORE adding to output
      re-emit DISPATCH block (same agent_id + inbox)
  elif state == ExecPending and replay_count >= CAP:
      state = ExecBlocked(reason="dispatch_undelivered") ; escalate (non-stdout)
  elif state == ExecLaunchAttempted and not acked and worker-state stale/absent:
      # launched-but-maybe-died, or attempted-then-crashed — NEVER auto-replay
      surface for registration-repair / WORKER_FAILED to operator
  # ExecAcked / live-heartbeat worker → leave alone
```
- **Never replay `ExecLaunchAttempted`** — that's the double-launch trap.
- Replay only `ExecPending` (coord never flipped it ⇒ never launched).
- **Idempotent** only because re-emit happens exclusively from
  ExecPending; the CAS in protocol-step-2 prevents two ticks racing the
  same entry.

### Replay predicate invariant (rev3 — round-2 P2, verbatim)
For `ExecPending`, **replay keys ONLY on journal state + project ownership;
never on task status, worker_agent_ids, `pending_acquire`, or `state.json`
freshness.** (Bootstrapped `state.json` is fake liveness for a phantom; the
journal is the only trustworthy signal.)

### Durable replay cap + non-stdout escalation
- `replay_emit_attempts` (rev3 rename — it counts *reserved emissions*, not
  deliveries), `last_replay_at`, `last_replay_error` persist IN the journal
  entry. Increment **before** adding the block to tick output (so a
  broken-pipe print still advances the cap — no infinite loop across coord
  restarts). Cap is **total-per-dispatch**, not per-coord-process. BOTH emit
  paths (replay + `_dispatch_ready` retry) respect the same counter so the
  cap can't be bypassed.
- **Persistent broken stdout:** replay re-emits to stdout; if stdout is
  still broken, re-emit re-breaks AND a stdout BLOCKED message also fails.
  So escalation MUST be non-stdout: set journal `ExecBlocked` +
  `reason="dispatch_emit_broken_stdout"`, write a coord-state/task-note
  error, and a TUI-visible `blocked_reason`. Replay degrades to durable
  BLOCKED, surfaced off-channel (surface-dont-silo).

### Why not the alternatives (unchanged from rev1, re-confirmed)
- **Emit-before-commit reorder** — reintroduces the deliberate
  `state.json`-before-spawn race (double-dispatch via concurrent tick). Rejected.
- **Synchronous ack** — loop.py can't invoke/await the host Agent. Replay
  is the async equivalent. Rejected.
- **`Path.exists()` on the inbox file as the signal** — can't distinguish
  consumed/never-written/removed-while-live; the Go release path flips the
  journal even on live-claim+missing-file. Key off journal state, never file
  presence. Rejected.

### ExecInFlight migration + release semantics (rev3 — round-2 P2; resolves Open Q#1)
- The journal `ExecState` enum (dispatch.go:46-52) currently has only
  `pending/in_flight/done/blocked/failed`. rev3 ADDS
  `ExecLaunchAttempted` + `ExecAcked`. `ExecInFlight` is **retired** from
  the live path (acquire stops setting it); keep it parseable as a legacy
  alias for old on-disk entries. Audit confirms no production Go/Python
  consumer reads `exec_state==in_flight` outside `internal/dispatch`, and
  `InspectCoordPromptInbox` keys off live-claim + file presence (not
  exec_state) — so the acquire change is otherwise safe. Update the Go
  tests that assert post-acquire `ExecInFlight`.
- **`ReleaseCoordPromptInbox` (api.go:148-150) force-flips ANY non-terminal
  state to `ExecDone`** — this would clobber `ExecLaunchAttempted`/
  `ExecAcked`/`ExecBlocked`. Fix: release must NOT downgrade
  `ExecBlocked`/`ExecFailed`; releasing an un-acked `ExecLaunchAttempted`
  resolves to `ExecFailed`/`ExecBlocked` (launch unconfirmed), not `ExecDone`.

## Scope / files
- `internal/dispatch/store.go` — **add a per-journal `flock` + a single
  locked RMW primitive** (`flock → load → predicate → mutate → WriteAtomic →
  unlock`); add `generation int` (bumped under the lock, used as the launch
  token + audit). ALL writers route through it. This is the load-bearing
  rev5 change (closes P1-A; the rev4 lock-free version was unsound).
- `cmd/fleet/claims.go` (journal CLI) — Go subcommands, all taking the
  flock: **`mark-launch-attempted <id> <gen>`** (flip iff ExecPending AND
  on-disk gen==token; distinct exits: ok / predicate-fail / **contention**),
  **`mark-acked <id>`**, **`reserve-replay <id>`** (bump
  `replay_emit_attempts`, return new gen), **`reset-for-relaunch <id>`**
  (reads new prompt on **stdin**; under one lock: rewrite inbox + reset to
  fresh `ExecPending`, bump gen, attempts=0). NO Python journal writer.
- `internal/dispatch` (api.go/dispatch.go) — acquire leaves `ExecPending`;
  add `ExecLaunchAttempted`/`ExecAcked` states + `launch_attempted_at` +
  `replay_emit_attempts` + `generation` fields; fix `ReleaseCoordPromptInbox`
  clobber; retire-but-parse `ExecInFlight`.
- `skills/coordinator/handoff_resume.py` — call `reset-for-relaunch` before
  re-emitting a resume DISPATCH block (so resume enters the universal gate).
- `skills/coordinator/loop.py` — tick-entry replay-reconcile (predicate +
  invariant above) sharing the emit-set with `_dispatch_ready`/
  `pending_acquire`; cap; residual-crash repair; non-stdout BLOCKED
  escalation. (loop.py changes land on the **loop-supervisor-sigpipe
  branch** — one combined PR per operator's hold.)
- `skills/coordinator/register_subagent.py` — set `ExecAcked` via the Go
  path (resolve Fleet agent_id→journal id; best-effort, lock-contention-safe).
- `skills/coordinator/SKILL.md` — the new launch-attempt protocol step.
- `skills/coordinator/loop.py` journal reader — read launch-state (Python
  reads the Go-written journal; reads are safe, writes go through the Go CLI).

## Test plan (per `feedback_e2e_tests_for_all_cases`) — rev2 expanded
- (a) Agent launched but register failed (no ack) → **no replay, no second block**.
- (b) Durable `ExecLaunchAttempted` survives coord restart → suppresses replay.
- (c) Phantom bootstrap state reads alive 15 min + stuck-idle needs tmux →
  assert NO fast reclaim today (motivates replay); assert replay reclaims next tick.
- (d) inbox-missing × journal {live/released/absent} → distinct outcomes, none double-dispatch.
- (e) Replay cap persists across coord restart; CAP reached → ExecBlocked, no infinite re-emit.
- (f) stdout broken recurs on replay → transitions to durable ExecBlocked + off-channel surface (not stdout).
- (g) Healthy heartbeat + missing ack → registration-repair, NOT redispatch.
- Integration: simulate the broken-stdout incident end-to-end → next healthy tick re-emits the ExecPending dispatch, coord launches once, acked; no phantom, no double-launch. Fails-on-parent.

## Resolved (round-2)
1. **ExecInFlight consumers** — audited: none read `in_flight` outside
   `internal/dispatch`; acquire change safe. Retire-but-parse as legacy.
   Fix `ReleaseCoordPromptInbox` clobber (above).
2. **CAS owner** — **Go** (`fleet claims mark-launch-attempted`), not
   Python. The lockless journal + Go writers make a Python tmp+rename unsafe.
3. **Residual crash repair** — `LAUNCH_ACK_GRACE` (1–2 ticks); after it,
   `ExecLaunchAttempted` w/o ack + no live subagent → `ExecBlocked` +
   off-channel escalation, ignoring bootstrapped state.json.

## Open questions (round-3 reviewers + operator)
- `LAUNCH_ACK_GRACE` exact value (1 tick? 2?) — balance fast-repair vs
  false-positive on a slow coord launch.
- Should `register_subagent`'s `ExecAcked` write be a new
  `fleet claims mark-acked <fleet-id>` subcommand too (consistency), given
  it currently only writes coord-state.json and lacks the journal-id mapping?
