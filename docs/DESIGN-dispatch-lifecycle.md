# DESIGN: Dispatch Lifecycle Primitive — v9 draft

**Status:** DRAFT v9 — codex rounds 1-8 + plan-eng-review findings folded in; operator approval pending

---

## TL;DR for implementers

You are reading this because you're about to implement a PR in this re-arch (1-4) OR you're reviewing one. Read this section first; dive into the full spec below as needed.

**What we're building:** A single dispatch-lifecycle primitive at `internal/dispatch/` that owns resource cleanup for every kind of dispatch (worker, reviewer, finisher, coord). Replaces today's scattered per-resource cleanup hooks. 9 resource kinds across 5 semantic classes (Exclusive, Shared/Adoptable, Derived, Delivery, Audit).

**Why we're building it:** Two leak postmortems in 48h (orphan-tmux 2026-05-13, stale-inbox 2026-05-15) showed per-resource spot-fixes scale linearly. Disease is the architecture, not the bugs.

**Load-bearing invariants:**
- `DispatchID == agent_id` (8-hex, same shape as today's `mint_agent_id`). Promoted to a named type in PR1 with a constructor test.
- `state == terminal ⇒ all claims released` (per-class semantics).
- Single store: claims live inline in `~/.fleet/dispatches/<id>.json`. No separate claim-files dir.
- `host_id` + `tmux_socket` on every claim; sweeper refuses cross-* reclaim.
- Replace's atomic commit = `coord_spawn_marker` claim CAS (not a journal flip).

**PR sequencing (4 stacked PRs over ~3 weeks):**
1. **PR1** — `internal/dispatch/` scaffold + Delivery controller for `coord_prompt_inbox` only + migrate `loop.py` call sites to `fleet claims acquire-prompt`. Closes today's 30-file leak. ~900 LoC.
2. **PR2** — expand to remaining Delivery kinds + Exclusive (tmux/agent_record) + Adoptable (worker_dir/worktree). ~1700 LoC.
3. **PR3** — coord_spawn_marker Exclusive + `Replace` operation (folds atomic-coord-swap-v6 with all codex findings). ~1800 LoC.
4. **PR4** — unified `sweep-leaks` sweeper + observability + archive pruner. ~1300 LoC.

**Gotchas (read these before writing code):**
- Replace's ownership proof is a **nonce file** at `~/.fleet/projects/<p>/coord-replace-nonces/<NEW.DispatchID>-<replaceNonce>.json` (not coord-state.json). OLD lacks the nonce value; that's the only barrier.
- Adoptable lock is per-`task_slug` (not per-`{kind, task_slug}`) — covers worker_dir + worktree bundle. Default 10s wait timeout.
- `coord_prompt_inbox` is read by the **coord agent** (passes content to Agent-tool), not the subagent's first turn. Doc/code mismatch fixed.
- Resume-prompt is 3-phase prepare → deliver → ack with `delivery_id` for receiver-side dedup. Persistent cache at `~/.fleet/projects/<p>/resume-prompts-seen.json`.

**Plan-eng-review decisions (2026-05-15) applied below:** test infra via `internal/testutil/tmuxtest` + boundary fakes; golden-file CLI contract tests; PR1 ships a CRITICAL E2E regression test + kill-9 recovery test; PR4 ships archive pruner alongside sweeper; `AdoptableLockTimeout = 10s`; `DispatchID` named type.

---
**Author:** coord agent + codex (round 1+2 co-author)
**Reviewers:** codex (round 3 pending), plan-eng-review (pending), operator (approval gate)
**Target version:** v0.11.0 (vertical-slice PR1, then PR2-PR4 over ~3 weeks)
**Created:** 2026-05-15
**Supersedes:** `atomic-coord-swap-v6-uni-b09b` task spec (folded in as `Replace` in PR3); `internal/lifecycle/` package (issue #101 prior partial attempt at same problem — retired in PR4)

---

## TL;DR

Replace fleet's scattered per-resource cleanup hooks with **one journal store + typed claim controllers** keyed by 5 resource semantic classes (`exclusive`, `shared-adoptable`, `derived`, `delivery`, `audit`). Every fleet-created resource is owned by a `Dispatch` journal that holds its claims inline. Terminal transitions trigger conditional `Release` per claim controller — never an undifferentiated "delete this".

PR1 ships a **strictly minimal vertical slice**: just the `coord_prompt_inbox` Delivery kind, closing today's 30-file leak end-to-end. The other six resource kinds + Replace + sweeper come in PR2-PR4. The refactor also retires `internal/lifecycle/` (issue #101) and ~9 stale tasks alongside the code that supersedes them.

---

## Motivation — the recurring leak pattern

Fleet has shipped two near-identical resource-leak postmortems in 48 hours:

| Date | Postmortem | Resources leaked | Spot-fix shape |
|---|---|---|---|
| 2026-05-13 | `docs/postmortems/2026-05-14-orphan-tmux-leak.md` | 68 orphan tmux → Mac OOM × 2 | Tristate `SessionAlive()` + `prune-orphan-tmux` (PRs #146 #148) |
| 2026-05-15 | (this design) | 30 stale inbox files, 2 orphan worktrees, 4 supervisor ghosts | TBD — this design |

Both bugs have the same shape: resource created, no destroy gate at the corresponding terminal transition, resource leaks. Per-resource spot-fixes scale linearly with resource types; the disease is the architecture.

Codex round 1 surfaced a deeper truth: **fleet's "resources" are NOT homogeneous** — they fall into 5 semantic classes. Codex round 2 surfaced a second truth: **what looked like ONE resource kind (`inbox_file`) is actually three**, each with a different lifetime story.

This design proposes: **one journal store + typed claim controllers, one controller per resource KIND (not class), with shared infrastructure per class.**

### Prior attempt: `internal/lifecycle/` (issue #101)

A previous PR introduced `internal/lifecycle/` with `Classify()` (5 abstract states) + `OnTerminal()` (per-entity cleanup delegate). It's the right *abstract shape* but only orchestrates — entity packages still own their own cleanup. The leaks keep coming because the entity-package cleanup is what's incomplete. **v0.11's primitive subsumes `internal/lifecycle/`** and retires it.

---

## Goals

1. **One execution journal per dispatch**, with claim records inline (no separate claim store).
2. **One typed controller per resource KIND**, sharing infrastructure within its class.
3. **Adding a resource = pick the class + write a controller.** No new ad-hoc cleanup site.
4. **Coord swap is `Replace`** — a specific call shape on the Exclusive controller for the `coord_spawn_marker`.
5. **Forward-only migration; one-shot manual sweep for legacy leaks.**
6. **Vertical-slice rollout** — PR1 proves the model on coord_prompt_inbox (the actual leak) before scaffolding others.
7. **Refactor cleans the repo** — code/docs/tasks that exist *because of* the old pattern get retired alongside the new code.

## Non-goals

- Cross-machine coordination (single-host invariant; multi-machine sync is out of scope).
- Backfilling manifests for pre-v0.11 dispatches in flight at upgrade time.
- Changing the on-disk shape of any resource itself (paths/names unchanged).
- Replacing tasks.md as the source of truth for tasks.
- A general-purpose ownership/CAS framework — the primitive is fleet-shaped, not a library.
- Removing operator/audit artifacts.

---

## Resource semantic classes (5)

| Class | Members | Ownership story | Release semantics |
|---|---|---|---|
| **Exclusive** | `tmux_session`, `agent_record`, `coord_spawn_marker` | One owner; named by dispatch ID or project. | Owner can release. Non-owner cannot. |
| **Shared / Adoptable** | `worker_dir`, `worktree` | Named by **task slug**. Reused across redispatches (worker → reviewer → finisher all share). | Release ONLY when the *task* is terminal-and-archived in tasks.md. Dispatch terminal alone is insufficient. |
| **Derived projection** | `supervisor_entry`, `worker_agent_ids_entry` | Reconciled from desired state. Not first-class state. | Reconciled, never released. Sweeper compares vs desired and prunes ghosts. |
| **Delivery envelope** | `coord_prompt_inbox`, `handoff_resume_inbox`, `remote_control_inbox` | One-shot read-and-discard. **Three distinct kinds, same class** — they share atomicity primitives but have separate release semantics. | Each kind defines its own. See per-kind table below. |
| **Audit artifact** | `archive/*` entries, subagent WIP, postmortems, design docs, `~/.fleet/incidents/`, `projects/<p>/subagents/*.json` | Intentional retention. | **NEVER swept as leaks.** Sweeper excludes these paths. |

### Delivery kinds — codex round 2 split

What v2 called `inbox_file` is actually three different lifetimes sharing a path shape (`~/.fleet/inbox/<id>.md`):

| Kind | Writer | Reader | Release semantics |
|---|---|---|---|
| `coord_prompt_inbox` | `skills/coordinator/dispatch.py::write_worker_inbox` (called by `loop.py:_dispatch_ready` and `_dispatch_review_handoffs`) | The **coord agent** reads the file body and passes its content as Agent-tool `prompt` parameter (coord_prompt_inbox is staging, not first-turn injection). | Release on dispatch terminal (done/blocked/failed). Default unlink; `preserve=true` archives. **This is the source of the 30-file leak.** |
| `handoff_resume_inbox` | `skills/coordinator/handoff_resume.py:366` (rewrites a `coord_prompt_inbox`) | The resumed subagent | NOT a separate file on disk — same path as the `coord_prompt_inbox` it rewrites. Rewrite atomically transfers ownership via the Delivery controller's `Rewrite()` op; old dispatch's Release becomes no-op (different owner_id). |
| `remote_control_inbox` | `skills/coordinator/remote_control.py:269` | The remote-control bootstrap session | Release on RC session bootstrap completion. Distinct retention policy (operator may inspect the bootstrap content for debugging). |

PR1 only ships `coord_prompt_inbox`. The other two land in PR2.

### Mapping to v1 resource kinds (9 total now, was 7)

| Kind | Class | Today's name shape |
|---|---|---|
| `tmux_session` | Exclusive | `fleet-<agent_id>` |
| `agent_record` | Exclusive | `~/.fleet/agents/<agent_id>.json` |
| `coord_spawn_marker` | Exclusive (singleton per project) | `~/.fleet/projects/<p>/coord-spawn-marker` |
| `coord_prompt_inbox` | Delivery | `~/.fleet/inbox/<agent_id>.md` |
| `handoff_resume_inbox` | Delivery (rewrite-in-place of coord_prompt_inbox) | same path, transferred ownership |
| `remote_control_inbox` | Delivery | `~/.fleet/inbox/<agent_id>.md` (distinguished by writer registration) |
| `worker_dir` | Shared / Adoptable | `~/.fleet/projects/<p>/workers/<slug>/` |
| `worktree` | Shared / Adoptable | `~/.fleet/projects/<p>/worktrees/<slug>/` + `worker/<slug>` branch |
| `supervisor_entry` | Derived | `coord-state.json[supervisor][<slug>]` + `worker_agent_ids[<slug>]` |

### Explicit "never sweep as leaks" (audit class)

- `~/.fleet/agents/archive/` — historical agent records.
- `~/.fleet/inbox/archive/` — fleet-guard archived inboxes.
- `~/.fleet/dispatches/archive/` — completed dispatch journals.
- `~/.fleet/subagent-wip/` — CLAUDE.md §2 phase logs (operator audit).
- `~/.fleet/incidents/` — codex round 2 catch: incident dirs are operator-retained.
- `~/.fleet/projects/<p>/subagents/*.json` — codex round 2 catch: subagent metadata for TUI history.
- Queue files (`~/.fleet/queue/`) — self-cleaning via `fleet drain`.
- Lock files (`*.lock`) — NB-flock, auto-released.
- All `docs/**` and `docs/postmortems/**` — versioned with the repo.

---

## Typed claim controllers

One controller per resource KIND. Class-level interfaces define the **shape**; per-kind controllers implement the specific semantics.

### Exclusive controllers

```go
// internal/dispatch/exclusive.go

type ExclusiveClaim struct {
    Kind         string  // "tmux_session" | "agent_record" | "coord_spawn_marker"
    ID           string  // resource-local identifier
    OwnerID      DispatchID
    HostID       string                 // hostname when claimed
    TmuxSocket   string  `json:",omitempty"`  // codex round 2: same-host different-socket discriminator (tmux_session only)
    State        ClaimState              // allocating | live | releasing | released
    CreatedAt    time.Time
    ReleasedAt   *time.Time `json:",omitempty"`
    Meta         json.RawMessage         // kind-specific payload
}

type ExclusiveController interface {
    // AcquireAndRecord wraps resource creation + claim record in one Go-side
    // transaction. spawn closure does the actual create; controller writes
    // the claim file as allocating (pre-spawn), runs spawn(), then atomically
    // flips claim to live. The journal's ClaimRef is appended in the same
    // transaction (the journal file is co-located; updates use tmp+rename
    // with the journal as the durable record — see "Manifest store" below).
    AcquireAndRecord(ctx context.Context, j *Journal, claim ExclusiveClaim, spawn func() error) error

    // Inspect returns kind-specific normalized status; "unknown" => do not touch.
    Inspect(ctx context.Context, claim ExclusiveClaim) (Status, error)
    // Per kind:
    //   tmux_session:       Alive | Dead | Unknown
    //   agent_record:       Live  | Archived | Missing
    //   coord_spawn_marker: Self  | Other    | Missing

    // Release succeeds only if the on-disk owner still matches claim.OwnerID
    // AND (for tmux_session) the socket still matches claim.TmuxSocket. Otherwise
    // ErrNotOwned (idempotent: already-released is success).
    Release(ctx context.Context, claim ExclusiveClaim) error
}
```

### Shared / Adoptable controller

```go
// internal/dispatch/adoptable.go

type AdoptableClaim struct {
    Kind        string         // "worker_dir" | "worktree"
    TaskSlug    string         // the key (NOT dispatch_id)
    CurrentOwner DispatchID
    Generation  uint64         // monotonic per {kind, task_slug} successful owner change; never resets
    History     []AdoptionRecord
    HostID      string
    State       ClaimState
    Meta        json.RawMessage
}

type AdoptionRecord struct {
    DispatchID DispatchID
    DispatchKind string
    AdoptedAt  time.Time
    ReleasedAt *time.Time
}

type AdoptableController interface {
    // AcquireOrAdopt creates the resource (if absent) or adopts the existing one,
    // incrementing Generation. The current claim is the atomic holder.
    AcquireOrAdopt(ctx context.Context, j *Journal, claim AdoptableClaim, create func() error) error

    Inspect(ctx context.Context, claim AdoptableClaim) (Status, error)
    // Registered | Absent | Unknown

    // ReleaseIfTaskTerminal: release ONLY if the task slug is terminal AND
    // archived in tasks.md. Reads tasks.md authoritatively via internal/tasks
    // — NOT via shell-out (codex round 2: "task terminal + archived must come
    // from one concrete source of truth, not a shell-out closure").
    ReleaseIfTaskTerminal(ctx context.Context, claim AdoptableClaim) error
}
```

Stale-owner adoption rule (codex round 2 fix): if `CurrentOwner`'s dispatch is exec-terminal (done/blocked/failed) AND the claim is still `live`, the next AcquireOrAdopt **CAN** take over — proof of the prior dispatch's terminal state via journal lookup. The Adoptable controller performs this CAS atomically.

**Codex round 4 fix — per-`task_slug` lock (covers whole adoptable bundle).** Round 3 introduced per-`{kind, task_slug}` lock, which serialized worker_dir contenders separately from worktree contenders for the same task. Round 4 caught the split-ownership hole: contender A could win worker_dir while contender B wins worktree, leaving the worker_dir + worktree halves of the bundle owned by different dispatches. v5 widens the lock to **per-`task_slug` only** — one lock at `~/.fleet/claims-locks/<task_slug>.lock` covers ALL adoptable claims for that task (worker_dir + worktree + any future kinds in this class).

AcquireOrAdopt:

1. NB-flock the per-`task_slug` lock file (blocking with timeout, OR returns `ErrClaimContested` immediately, caller chooses).
2. Inside the lock: scan all journals referencing the task_slug to determine the current authoritative owners for every adoptable kind.
3. Run the CAS for the kind being adopted: write current dispatch's journal as new owner; release lock.

The lock is NOT held across `create` closure execution — only across the CAS read+write. Spawn-side races on resource creation are caught by underlying primitive idempotency. The bundle-wide lock guarantees that worker_dir + worktree adoptions on the same task_slug are always serialized.

### Derived reconciler

```go
// internal/dispatch/derived.go

type DerivedReconciler interface {
    // Reconcile recomputes the projection from desired state.
    // Desired state authority order: tasks.md > coord-state.json (codex round 2).
    // Removes ghost entries (in projection but not desired);
    // adds missing entries (in desired but not projection).
    Reconcile(ctx context.Context, project string) error
}
```

Called by the sweeper, never by terminal transitions.

### Delivery controller (3 kinds; common shape)

```go
// internal/dispatch/delivery.go

type DeliveryClaim struct {
    Kind     string     // "coord_prompt_inbox" | "handoff_resume_inbox" | "remote_control_inbox"
    ID       string
    OwnerID  DispatchID
    HostID   string
    State    ClaimState
    Preserve bool       // archive instead of unlink on release
}

type DeliveryController interface {
    AcquireAndDeliver(ctx context.Context, j *Journal, claim DeliveryClaim, content io.Reader) error

    Inspect(ctx context.Context, claim DeliveryClaim) (Status, error)
    // Present | Absent

    Release(ctx context.Context, claim DeliveryClaim) error

    // Rewrite: atomic content + ownership transfer (handoff_resume).
    // The new claim's OwnerID supersedes; old dispatch's Release becomes no-op.
    Rewrite(ctx context.Context, j *Journal, claim DeliveryClaim, newOwner DispatchID, content io.Reader) error
}
```

### Audit artifacts (no controller)

Excluded from the manifest. Sweeper's directory-walk skips them.

### Journal

```go
// internal/dispatch/journal.go

type ExecState string
const (
    ExecPending  = "pending"
    ExecInFlight = "in_flight"
    ExecDone     = "done"
    ExecBlocked  = "blocked"
    ExecFailed   = "failed"
)

type ReclState string
const (
    ReclPending  = "pending"
    ReclPartial  = "partial"
    ReclComplete = "complete"
    ReclBlocked  = "blocked"
)

type Journal struct {
    ID            DispatchID `json:"id"`
    Kind          string     `json:"kind"`   // "worker", "reviewer", "finisher", "coord", "fix", "rebase"
    Owner         string     `json:"owner"`  // "project/<p>/slug/<s>" or "coord/<p>"
    HostID        string     `json:"host_id"`
    TmuxSocket    string     `json:"tmux_socket,omitempty"`  // for any kind that includes tmux_session
    SchemaVer     string     `json:"schema"`
    CreatedAt     time.Time  `json:"created_at"`
    UpdatedAt     time.Time  `json:"updated_at"`
    ExecState     ExecState  `json:"exec_state"`
    ReclState     ReclState  `json:"recl_state"`
    BlockedReason string     `json:"blocked_reason,omitempty"`
    Claims        []ClaimInline `json:"claims"`  // CLAIM DATA STORED INLINE (codex round 2 — single store)
}

type ClaimInline struct {
    Class string          `json:"class"`
    Kind  string          `json:"kind"`
    State ClaimState      `json:"state"`
    Data  json.RawMessage `json:"data"`  // serialized ExclusiveClaim / AdoptableClaim / DeliveryClaim
}
```

**Single store, single source of truth.** No separate `~/.fleet/claims/` directory. All claim state lives inside the journal file. Updates are atomic tmp+rename of the journal. This eliminates codex round 2's split-brain repair class.

---

## Manifest store

```
~/.fleet/dispatches/
├── <dispatch-id>.json              # execution journal — contains inline claims
└── archive/
    └── <dispatch-id>-<stamp>.json  # terminal+recl_complete journals
```

One file per dispatch. Atomic tmp+rename writes. Updates serialize through a per-file flock to handle concurrent reads/writes from controllers + sweeper.

Shared/Adoptable resources are special: their claim data appears inline in EACH dispatch that holds an adoption record. The `CurrentOwner` field in the claim determines who can release. The Adoptable controller's `AcquireOrAdopt` CAS reads ALL dispatches that reference the same task_slug to resolve the current owner authoritatively — this is `O(N_dispatches_per_task)` which is small (≤ 3 for worker→reviewer→finisher).

---

## Atomicity contract

### Per-claim 2-phase

Each claim transitions through `allocating → live → releasing → released`:

1. **Allocating.** Append claim to the journal with `state=allocating`. Journal tmp+rename. This is the intent-to-create journal entry.
2. **Live.** Call the resource-creation closure. On success, atomic-update the journal — flip the claim to `state=live` (tmp+rename of the journal again). On failure, flip claim to `failed-alloc`; the sweeper drops the claim entry on next pass.
3. **Releasing.** Terminal exec_state triggers Release. Controller flips claim to `state=releasing`. Performs teardown. On success, flips to `released`. Idempotent on retry.
4. **Released.** Sweeper archives the journal once all owned claims are `released`.

### Same-file atomicity (codex round 2 fix)

v2's two-store split required cross-file atomicity (a real distributed-systems problem). v3 keeps everything in **one journal file** — same-file tmp+rename is durable + atomic. No cross-store reconciliation needed.

The tradeoff: each AcquireAnd* call rewrites the entire journal file. Journal files are small (a few KB even with 4-7 claims); the rewrite cost is acceptable. The benefit: no split-brain.

### Acquire is Go-side; Python shells out via `fleet claims`

Codex round 2: don't put internal helpers under `fleet dispatch`. New CLI namespace: `fleet claims` (hidden subtree).

```
fleet claims acquire-prompt <dispatch-id> --owner=<task-slug> --host-id=<hostname> \
  --tmux-socket=<path>            # read content from stdin
fleet claims release <dispatch-id> --kind=<kind>
fleet claims rewrite-prompt <new-dispatch-id> --target-old-id=<old-dispatch-id>  # stdin content
fleet claims inspect <dispatch-id>  # JSON output for tooling
```

**Outcome enums** (codex round 3 — stable exit codes + JSON `outcome` field):

| Outcome | Exit | Meaning |
|---|---|---|
| `acquired` | 0 | New claim created. |
| `already_acquired` | 0 | Claim with this dispatch_id + kind already exists; idempotent success. |
| `released` | 0 | Release succeeded. |
| `already_released` | 0 | Claim was already released; idempotent success. |
| `not_owned` | 10 | Release attempted but on-disk owner != caller dispatch_id. Caller error or stale state. |
| `absent` | 11 | Inspect target doesn't exist. |
| `contested` | 12 | Per-`task_slug` adoptable bundle lock held; caller should retry or backoff. |
| `error` | 1 | Catch-all for unexpected failures (disk full, permission, etc.). |

JSON output shape:

```json
{"outcome": "acquired", "dispatch_id": "a690424b", "kind": "coord_prompt_inbox", "path": "/Users/pinkbear/.fleet/inbox/a690424b.md"}
```

All `fleet claims` subcommands:
- Read prompt content from stdin (no `--content-file` flag — avoids path leaks).
- Output JSON on stdout. Stable schema (outcome + kind-specific fields).
- Stable exit codes per the table above.
- Hidden from `fleet --help` (internal helpers).

---

## State machines

### Execution state (per dispatch)

```
pending → in_flight → { done | blocked | failed }
```

### Reclamation state (per dispatch)

```
pending → { complete | partial | blocked }
```

Driven by per-claim release results.

### Per-claim state (per resource)

```
allocating → live → releasing → released
                 ↓
            (failed-alloc)
```

---

## Replace operation (folds atomic-coord-swap-v6)

**Atomic-commit point:** the `coord_spawn_marker` Exclusive claim's owner CAS.

`Replace(project, oldDispatchID, newDispatch *Journal) error`:

0. Generate `replaceNonce = uuid()`. Used in step 4 spawn env + step 6 proof check + step 7 cleanup.
1. Read `oldDispatch` journal. Precondition: `exec_state ∈ {in_flight, pending}`.
2. Acquire `swap.lock` (NB-flock — same as today).
3. Read `coord_spawn_marker` claim's `CurrentOwner` for the project. Three cases:
   - `== oldDispatchID` → expected; proceed.
   - `== newDispatchID` → prior Replace got partway; skip to step 8.
   - `== ""` or missing → OldIsDead path; create marker fresh in step 7.
4. Spawn `newDispatch`: tmux + agent_record + coord_prompt_inbox via AcquireAndRecord (controller writes claim allocating → live). **Spawn env includes `FLEET_REPLACE_NONCE=<replaceNonce>` + `FLEET_DISPATCH_ID=<newDispatchID>` + `FLEET_PROJECT=<project>`.** OLD's process does not have these envs and cannot observe `replaceNonce`.
5. **Pre-commit idempotent ownership trigger** — caller-supplied closure tells `newDispatch` to *boot the /coordinator skill*. Send via send-keys (TUI [a]) or synth handoff doc preamble (queue path). Trigger is a "wake up" signal; safe to retry. Full work-starting resume prompt is sent post-commit (step 9).
6. **Ownership proof gate** (codex round 4 fix — replaced last-writer-wins holder_id with NEW-only nonce file):
   - **(a) NEW's tmux SessionAlive == Alive** via `internal/tmux.SessionAlive(socket, "fleet-<NEW.ID>")` — proves the session booted.
   - **(b) NEW's replace nonce file exists at the expected path with the expected content.** Replace generates `nonce = uuid()` at step 0; spawns NEW with `FLEET_REPLACE_NONCE=<nonce>` env (step 4); NEW's first /coordinator tick, on detecting that env var, atomically writes `~/.fleet/projects/<p>/coord-replace-nonces/<NEW.DispatchID>-<nonce>.json` containing `{"nonce": "<nonce>", "dispatch_id": "<NEW.DispatchID>", "wrote_at": "<ts>"}`. **Filename includes the nonce** (codex round 6 P2 fix) so DispatchID birthday collisions (32-bit ID space — `agent.NewID` is 8 hex = 4.55% collision risk by 20k IDs per `internal/agent/agent.go:209`) never clobber a prior Replace's audit record. Replace polls this exact path; success criterion: file exists AND `nonce` matches AND `dispatch_id == NEW.DispatchID` AND `consumed_at` is null (codex round 5 fix — see step 7). **OLD has the env var path knowledge (any process can list `~/.fleet/`), and OLD can discover NEW.DispatchID via `tmux ls` enumeration once NEW boots.** The single barrier is the nonce VALUE: it's passed only through NEW's spawn env (`FLEET_REPLACE_NONCE`), so OLD doesn't have it absent a process-env read (a same-UID concern; same-UID is the entire fleet trust boundary anyway). Codex round 5 P2: don't overclaim. The proof is "OLD lacks the nonce value", not "OLD is structurally locked out of the path or ID".
   - **(c) marker readback sanity check** — re-read `coord_spawn_marker` claim; if `Self` (already NEW.ID), prior Replace partial; skip to step 8. If `Other` (third dispatch we don't know), FAILURE_MODE_6. If `Missing` or OLD, proceed to step 7. NOT a proof signal — concurrency safety only.
   - Timeout: `dispatch.OwnershipProofTimeout` constant, default 30s (caller-overridable).
   - On timeout: Release all NEW claims, drop NEW journal, leave OLD untouched. Return `ErrOwnershipNotProven`.

   **Code-side: where the nonce file gets written.** Add to `skills/coordinator/loop.py`'s `_tick_locked` **at the very top, before `bootstrap_remote_control()` and any other first-tick work** (codex round 5 placement note): if `FLEET_REPLACE_NONCE` env is set AND the per-dispatch nonce file's content does NOT already match this dispatch's nonce + dispatch_id, write it atomically (tmp+rename). The check is content-based, not absence-based: an already-correctly-written nonce file is left alone; a missing-or-mismatched one gets (re)written. This naturally idempotent because:

   - On first tick: file absent → write.
   - On subsequent ticks: file present with matching content → skip.
   - On step-7-aftermath: the file is NOT unlinked (codex round 5 P1 fix — see step 7 below) so future ticks keep skipping.

   The dispatch ID comes from `FLEET_DISPATCH_ID` env (new, set by Replace step 4's spawn).

   **Nonce file lifecycle (codex round 5 P1 fix):** Replace step 7 does NOT unlink the nonce file. Instead, after the marker CAS commits, Replace atomically updates the nonce file's content to add `consumed_at=<ts>`. The file persists as a per-Replace audit record (truly per-Replace given the nonce-in-filename — codex round 6 P2). NEW's first-tick logic sees the file has matching nonce + dispatch_id and skips writing. A sweeper TTL (30 days, configurable via `FLEET_REPLACE_NONCE_RETENTION_DAYS`) reaps consumed nonce files; non-consumed ones (`consumed_at == null`) older than the Replace timeout window get logged as stale-replace evidence for operator review, **never auto-deleted** (codex round 6: rollback path also doesn't auto-delete; operator triages stale evidence manually).

   This eliminates the round-5 P1 hole: there's no longer a state where step-7 unlink + later-tick env-rewrite can recreate a stale-looking nonce file.
7. **ATOMIC COMMIT** — CAS the `coord_spawn_marker` claim's `CurrentOwner`: `oldDispatchID → newDispatchID` (or `"" → newDispatchID`). Atomic tmp+rename. **Then atomic-update the nonce file** at `~/.fleet/projects/<p>/coord-replace-nonces/<NEW.DispatchID>-<replaceNonce>.json` (path matches step 6 / step 0 — codex round 7 P2 fix; `replaceNonce` is in scope from step 0) to add `consumed_at=<ts>` (preserves the nonce + dispatch_id audit trail; ensures NEW's later ticks skip the rewrite — codex round 5 P1). This is Replace's commit point.
8. **Post-commit 3-phase resume prompt** (codex round 3 fix — add `delivery_id` for deliver-side dedup):
   - **prepare**: write `resume_prompt.prepared_at` + `resume_prompt.delivery_id = uuid()` to NEW journal. Atomic. The delivery_id is the deduplication token; once minted, it persists across retries.
   - **deliver**: send the work-starting content via the caller-supplied closure. The closure MUST receive the delivery_id and embed it in the delivered content (as a frontmatter field for synth-doc path, or as a comment marker for send-keys path). The receiver checks: if it has previously seen this delivery_id, it skips (delivered_already). The transport-side dedup means a delivery that succeeds but loses its ack-write can be safely retried with the same delivery_id; the receiver no-ops the second delivery.
   - **ack**: on closure success, write `resume_prompt.delivered_at` to NEW journal (delivery_id already there from prepare).
   - On retry: controller reads NEW journal; if `delivered_at != null` skip (success); if `prepared_at != null && delivered_at == null` retry deliver+ack with SAME delivery_id (receiver dedups). If neither set, full sequence.
   - **Receiver-side dedup**: a small JSON cache at `~/.fleet/projects/<p>/resume-prompts-seen.json` keyed by delivery_id (TTL 7 days). The coord skill checks this cache on its first tick after spawn; if its delivery_id is present, skip the resume-prompt action (already delivered in a prior attempt). Cache is per-host (delivery_ids are host-scoped via HostID in the journal).
9. **(Live-old) Old retire.** SendKeys(OLD, `/exit`) + grace + tmux.Kill. Probe SessionAlive — if still Alive, FAILURE_MODE_5.
10. Flip `oldDispatch.exec_state = done`. Reclaim against OLD's claims (excludes coord_spawn_marker — now owned by NEW).
11. Release `swap.lock`. Return success.

### Codex findings — all answered

- **R2-finding-4 ownership-proof on broken coord.lock assumption** — step 6 uses SessionAlive + NEW-only nonce file readback + marker readback as a concurrency sanity check. None of these rely on coordinator.lock body. The lock is a per-tick mutex; we treat it as such. (Earlier v3 used mtime advance and v4 used holder_id; both were last-writer-wins and didn't prove NEW. v5's nonce file is the load-bearing proof.)
- **R2-finding-6 exactly-once resume prompt** — step 8 is 3-phase prepare/deliver/ack with journal-anchored idempotency.
- **R1-finding-4 marker compatibility {OLD, NEW, missing}** — step 3 enumerates all three.
- **R1-finding-3 live-old duplicate-coord contract** — steps 6-9 keep OLD alive until NEW proves ownership; the per-tick coordinator.lock still serializes which one is the active writer.

---

## Sweeper modes

`fleet maintenance sweep-leaks` — three modes:

### Mode 1: Orphan detection (`--orphans`)

Walks **on-disk resources NOT in any journal**. Default dry-run; `--kill` cleans.

This is the v0.11 back-compat pass for pre-v0.11 leaks. Goes away after v0.11.0 ships (all in-flight dispatches use new system; sweeper no longer finds journal-less resources except via bug).

### Mode 2: Release retry (`--retry-releases`)

Walks **journals with `exec_state ∈ terminal` and `recl_state ∈ {partial, blocked}`**. For each, retries `Release` on un-released claims. Idempotent.

### Mode 3: Derived reconciliation (`--reconcile-derived`)

Walks **derived projections** (coord-state.json supervisor maps). Compares against tasks.md (authority per codex round 2). Prunes ghosts; adds missing entries.

### Default: all three modes

`fleet maintenance sweep-leaks` (no flags) runs orphans → release-retry → reconcile-derived.

**No split-brain mode needed** (codex round 2: one-store design eliminates this entire class).

### Per-resource TTLs

- `tmux_session`: 90s post-terminal.
- `coord_prompt_inbox`: 168h post-terminal.
- `handoff_resume_inbox`: not applicable (no on-disk lifecycle of its own).
- `remote_control_inbox`: 24h post-terminal.
- `worker_dir`: based on TASK terminal+archived (Adoptable semantics, not TTL).
- `worktree`: based on TASK terminal+archived.
- `agent_record`: 0s — archive on dispatch terminal.
- `coord_spawn_marker`: never swept — owned by `Replace`.

---

## Observability

1. **`fleet dispatches list`** — active dispatches with exec_state + recl_state + claim counts.
2. **`fleet dispatches show <id>`** — full journal + per-claim inspection results + age.
3. **`fleet claims list <class> <kind>`** — list all claims of a kind. Reads across journals.
4. **TUI status banner** — yellow when `count(dispatches with recl_state ∈ {partial, blocked}) > 0`.
5. **`fleet status` project row** — `dispatches: 4 active, 0 blocked-reclaim`.

---

## Repo cleanup — what dies with the refactor

Each PR retires the cruft it supersedes. No separate "tidying" PR.

### Per-PR code retirement

| Item | Class | Retired by | Replacement |
|---|---|---|---|
| `internal/lifecycle/` (issue #101 package + tests) | code | PR4 | Subsumed by `internal/dispatch/`. Migrate `Classify`/`OnTerminal` callers. |
| `skills/coordinator/loop.py:_maybe_delete_worker_dir` worker-dir branch | code | PR2 | Adoptable `ReleaseIfTaskTerminal` |
| `skills/coordinator/loop.py:_sweep_done_worker_dirs` | code | PR2 | Adoptable sweeper hook |
| `skills/coordinator/supervisor.py:forget_agent_id` | code | PR2 (thin call into derived reconciler) | Derived reconciler |
| `skills/fleet-guard/inbox.py:archive()` | code | PR2 | Delivery controller `Release(preserve=true)` |
| `skills/coordinator/loop.py:_dispatch_ready` inbox-write call | code | PR1 | `fleet claims acquire-prompt` (Delivery controller AcquireAndDeliver) |
| `skills/coordinator/loop.py:_dispatch_review_handoffs` inbox-write calls (×2) | code | PR1 | `fleet claims acquire-prompt` |
| `skills/coordinator/dispatch.py:write_worker_inbox` helper (final removal) | code | PR2 | All callers migrated to `fleet claims` |
| `skills/coordinator/handoff_resume.py:366` inbox-rewrite path | code | PR2 | Delivery controller `Rewrite` |
| `skills/coordinator/remote_control.py:269` inbox writer | code | PR2 | Delivery controller `AcquireAndDeliver` (remote_control_inbox kind) |
| `internal/handoffop/atomic_coord_swap.go` body | code | PR3 | `Replace` in `internal/dispatch/` |
| `cmd/fleet/dispatch_recovery.go` (entire file) | code | PR3 | `Replace(OldIsDead=true)` path |
| `internal/handoffop/replacement_cleanup.go` | code | PR3 | Exclusive controller `Release` |
| `cmd/fleet/maintenance.go:prune-orphan-tmux` body | code | PR4 | `sweep-leaks --orphans` |

### Per-PR docs retirement

| Item | Retired by | Replacement |
|---|---|---|
| `docs/PLAN-v0.2-coordinator.md` per-resource-cleanup language | PR2 | Reference DESIGN-dispatch-lifecycle.md |
| `docs/ENG-v0.2-coordinator.md` per-resource-cleanup language | PR2 | Same |
| `docs/postmortems/2026-05-14-orphan-tmux-leak.md` (currently untracked) | PR1 — commit as-is; PR4 — append "v0.11 supersedes" note | Append note |
| `skills/coordinator/SKILL.md` references to retired helpers | PR2 | Reference primitive |

### Stale P3 tasks to triage during PR2-PR3

Each gets per-task decision: **fold** / **keep** / **archive**.

- `tmux-probe-tristate-heal-9c1c` — likely fold into PR4.
- `reconcile-pid-docstring-94b9` — likely fold into PR2.
- `fleet-pid-resolve-s-prop-0ab2` — triage PR1.
- `resolver-revalidate-tent-6006` — likely archive.
- `spawn-pane-unreachable-p-2857` — likely fold into PR2.
- `resolver-direct-cmd-fast-59d3` — triage PR2.
- `reconcile-handoff-sessio-c89b` — likely fold into PR3.
- `reconcile-worker-pid-rec-dea0` — likely fold into PR2.
- `tui-dead-coord-sweep-7844` — likely fold into PR4.

### Untracked-file audit (pre-PR1 prep)

Operator review before PR1 lands:

1. `internal/testutil/tmuxtest/tmuxtest.go` + `tmuxtest_test.go` — uncommitted local edits adding a new test + docstring updates. Decision: commit-as-precursor OR drop.
2. `.claude/` directory — operator-local Claude Code settings. Add to `.gitignore` if not already.
3. `docs/postmortems/2026-05-14-orphan-tmux-leak.md` — commit in PR1 (load-bearing context).
4. **`docs/DESIGN-dispatch-lifecycle.{md,html}`** + **`scripts/render-design-doc.py`** (codex round 2 catch) — commit in PR1 (the design doc + renderer ARE the spec; checking them in makes the spec versioned with the code).

### One-shot leak sweep (pre-PR2 prep)

- 30 stale coord_prompt_inbox files → unlink.
- 2 orphan worktrees → `git worktree remove --force`.
- 4 supervisor ghosts → jq-edit coord-state.json.
- 2 detached procs → inspect; kill only if confirmed orphan.

Script: `scripts/v0-11-pre-migration-sweep.sh`. Delivered with PR2; deleted after v0.11.0 ships.

### Memory entries to retire / revise (post-merge)

- `project_v02_coordinator_design.md` — references may be stale.
- Feedback memories tied to per-resource cleanup — re-check during PR4.

---

## Migration strategy

### Forward-only

- v0.11 introduces the primitive. New dispatches use it from PR1 (for coord_prompt_inbox) onward; PR2 for the remaining kinds.
- Pre-v0.11 dispatches in flight at upgrade time: pre-v0.11 cleanup paths still execute. Sweeper `--orphans` mode catches leaks via probe-and-delete.
- "No journal ≠ orphan" rule: sweeper only sweeps known legacy name shapes (`^fleet-<8hex>$` tmux, `^<8hex>.md$` inbox under `~/.fleet/inbox/`, worktrees under `projects/*/worktrees/`). Unknown names are logged only.

### PR1 ↔ PR2 overlap (codex round 2 catch + round 3 refinement)

During PR1's release window, inbox files have two possible writers:
- Pre-PR1: `dispatch.py:write_worker_inbox` (direct write).
- Post-PR1: `fleet claims acquire-prompt` (controller-managed; journal entry exists).

Distinguish via **journal lookup, not name shape.** `dispatch_id == agent_id` is the invariant (today's `mint_agent_id` is what becomes dispatch_id; the mapping is identity for fleet-spawned subagent dispatches). If `~/.fleet/dispatches/<agent_id>.json` exists with a `coord_prompt_inbox` claim referencing the file → managed. Otherwise legacy.

**Codex round 3 — PR1 helper scope narrowing.** v3's PR1 migrated `dispatch.py:write_worker_inbox` wholesale, but that helper is also called by `handoff_resume.py:366` (in-place resume rewrite — PR2 surface). PR1 migration is narrowed to **only the loop.py call sites that produce coord_prompt_inbox**:
- `loop.py:_dispatch_ready` (worker dispatch path) — migrate to `fleet claims acquire-prompt`.
- `loop.py:_dispatch_review_handoffs` (reviewer + finisher dispatch paths) — migrate to `fleet claims acquire-prompt`.

The helper `dispatch.py:write_worker_inbox` itself is NOT removed in PR1 — `handoff_resume.py:366` still uses it. PR2 migrates the helper's remaining caller (handoff_resume) and finally retires `write_worker_inbox`.

### Pre-migration leak sweep (one-shot)

Documented above.

---

## Vertical-slice sequencing — 4 stacked PRs

### Codex round 2: PR1 must be MINIMAL

v2's PR1 included Delivery controller spanning 3 inbox writers. v3 narrows to **just `coord_prompt_inbox`** — the writer at `dispatch.py:913` that produces today's 30-file leak. The other Delivery kinds (`handoff_resume_inbox`, `remote_control_inbox`) move to PR2.

| PR | Scope | Approx LoC | Closes |
|---|---|---|---|
| **PR1** Vertical slice (coord_prompt_inbox only) | `internal/dispatch/` scaffolding: `Journal`, state enums, manifest store, `fleet claims` CLI namespace, `DispatchID` named type (plan-eng A6). Delivery controller — coord_prompt_inbox kind ONLY. `skills/coordinator/loop.py:_dispatch_ready` and `_dispatch_review_handoffs` migrate to `fleet claims acquire-prompt`. `dispatch.py:write_worker_inbox` helper STAYS in PR1 (still used by `handoff_resume.py:366`; retires in PR2). Terminal-transition reclaim releases the inbox. `scripts/v0-11-pre-migration-sweep.sh`. Untracked-file audit (operator-driven before PR1 lands). **CRITICAL tests (plan-eng-review):** E2E regression test using `internal/testutil/tmuxtest` (dispatch → terminal → inbox-unlinked-and-journal-archived); kill-9 mid-AcquireAndDeliver recovery test; golden-file contract tests for `fleet claims` CLI (`cmd/fleet/testdata/claims/*.json`). | ~900 + ~250 test | The 30-file inbox leak |
| **PR2** Expand Delivery + add Exclusive + Adoptable | Delivery: handoff_resume_inbox + remote_control_inbox. Exclusive controllers: tmux_session, agent_record. Adoptable controllers: worker_dir, worktree. Spawn path migrates. Worker dispatch creates 4 claims. Reviewer/finisher reuse worker_dir via AcquireOrAdopt. Retires `_maybe_delete_worker_dir`, `_sweep_done_worker_dirs`, `forget_agent_id` worker-dir branch, `fleet-guard inbox.archive`, `handoff_resume.py:366`, `remote_control.py:269`. Triages 4-5 P3 tasks. | ~1700 | Multi-kind delivery + adoption bugs |
| **PR3** Replace (coord swap) | `coord_spawn_marker` Exclusive. `Replace` function — folds atomic-coord-swap-v6 with all codex findings answered. 4 call sites flow through Replace. Retires `atomic_coord_swap.go` body + `dispatch_recovery.go` + `replacement_cleanup.go`. Triages 2 P3 tasks. | ~1800 | Coord swap leaks + answers swap-v6 codex findings |
| **PR4** Sweeper + observability + cleanup + **archive pruner** | `sweep-leaks` 3 modes. `dispatches list/show`, `claims list`. TUI banner. Derived reconciler. **Archive pruner (plan-eng A4):** `fleet maintenance prune-dispatch-archive --older-than 90d`, defaults retain 90 days; triggered manually or by sweeper when archive size > 100MB. Retires `prune-orphan-tmux` body + `internal/lifecycle/` package. Docs updates. Final P3 triage. | ~1300 + ~150 pruner | Remaining sweeper coverage + observability + archive growth bound |

`atomic-coord-swap-v6-uni-b09b` is folded into PR3. `internal/lifecycle/` (issue #101) retires in PR4.

---

## Error policy

| Failure | Behavior |
|---|---|
| Claim allocation closure fails | Claim flips to `failed-alloc`; sweeper drops on next pass. |
| Claim live but resource missing on Inspect | Sweeper Release no-ops; claim flips to `released`. |
| Release returns error 3+ times within 1h | Claim flips to `releasing-blocked`. Journal `recl_state=blocked`. Operator-visible. |
| Inspect returns Unknown | Treat as "don't touch". Never sweep on Unknown (codex round 1 lesson). |
| Journal write fails (disk full, perms) | Caller error. No resources created (AcquireAnd* is atomic). |
| Cross-host claim attempt (HostID mismatch) | `ErrCrossHostClaim`. Operator resolves manually. |
| Same-host, different tmux socket (codex round 2 fix) | `ErrCrossSocketClaim`. Operator resolves. |
| `marker == NEW.ID` on Replace entry | Treat as in-flight resume; skip to step 8 (post-commit resume prompt). |
| Two dispatches AcquireOrAdopt same Adoptable bundle (any kind for same task_slug) | Per-`task_slug` bundle lock at `~/.fleet/claims-locks/<task_slug>.lock` (NB-flock). Loser returns `contested` immediately OR blocks until `AdoptableLockTimeout` (plan-eng CQ3: **default 10s**; caller can override via context deadline). On retry, reads the post-CAS state and either adopts or no-ops. CurrentOwner per-kind is authoritative; the bundle lock guarantees all kinds for the same task_slug serialize. |
| Resume-prompt deliver fails after prepare write | Claim has `prepared_at` + `delivery_id` but no `delivered_at`. On retry, controller redelivers with the SAME `delivery_id` (idempotency token). Receiver dedup cache (`~/.fleet/projects/<p>/resume-prompts-seen.json`) no-ops the duplicate if it already saw the delivery_id. |
| Resume-prompt ack write lost (deliver succeeded but `delivered_at` write failed) | Claim still in `prepared_at != null, delivered_at == null`. Retry path: same as above (redeliver with same delivery_id; receiver dedups; ack write retries). |

---

## Open questions — codex round 3 answered all

Round 3 answered all 5 outstanding round-2 questions:

1. **AcquireOrAdopt CAS correctness** — codex round 3 surfaced stale-read window with per-journal flock alone; round 4 surfaced the split-ownership hole if the lock is per-`{kind, task_slug}`. v5 resolution: **per-`task_slug` bundle lock** covering all adoptable kinds for the task (see Adoptable controller above).
2. **Sweeper authority** — codex round 3: operator-invoked sufficient for v0.11.0. Daemon deferred to v0.11.x.
3. **Resume-prompt failure recovery** — codex round 3: 3-phase `prepared + delivered + ack` is correct; ack-observed-by-caller fourth phase not needed. Missing piece (now applied): `delivery_id` for transport-side dedup so a deliver-success-ack-lost retry doesn't duplicate.
4. **`fleet claims` visibility** — codex round 3: keep hidden for v0.11.0. Stable JSON + stable exit codes (now in CLI section) is what matters, not discoverability.
5. **Migration race upgrade hook** — codex round 3: acceptable to defer, provided legacy resources are handled by conservative orphan-detection logic only and never by managed-release paths. The "no journal ≠ orphan" rule plus name-shape matching satisfies this.

---

## Failure modes (cross-PR test plan)

1. Coord crashes between AcquireAndDeliver prepare-write and actual file write → journal has claim in `allocating`, no file. Sweeper drops claim on next pass.
2. Coord crashes between file write and claim-flip-to-live → file exists, claim in `allocating`. Sweeper Inspects, finds file present, flips claim to `live`.
3. Coord crashes during Replace step 7 (CAS) → marker not yet written but NEW's other claims live (including nonce file). Sweeper detects via nonce-file + marker readback; if NEW's coord still alive (nonce file present, valid, `consumed_at == null`), retries CAS. If NEW's coord is dead AND nonce file is unconsumed past timeout, marks it as stale-replace evidence for operator triage — **never auto-deletes** (codex round 6 P2: operator inspects unconsumed nonces; rollback Releases NEW's other claims but leaves the nonce file).
4. Worker dispatch ends at phase=blocked → Delivery releases inbox; Adoptable defers (task not archived); Exclusive archives agent record.
5. Handoff-resume reads + rewrites inbox → Rewrite atomically transfers ownership; old dispatch's Release becomes no-op.
6. Operator runs sweep-leaks --kill while a dispatch is mid-reclaim → idempotent; both passes complete same resources.
7. Cross-host (Dropbox-synced ~/.fleet) → claim has different HostID. Sweeper refuses cross-host reclaim. TUI warns.
8. Same-host, different tmux socket → claim has different TmuxSocket. Sweeper refuses cross-socket reclaim.
9. Resume-prompt 3-phase, ack write fails → controller can recover from journal state.

---

## Test infrastructure (plan-eng-review A9 decision)

All integration tests use **real tmux via `internal/testutil/tmuxtest`** (already in repo) for per-test tmux server isolation. Boundary fakes for:
- fleet-guard heartbeat writer (mock the agent JSON updates).
- coord-state writer (in-memory map; test asserts state at boundaries).
- Agent-tool return (mock subagent termination + state.json updates).

The pattern follows `internal/handoffop/atomic_coord_swap_test.go` today. PR1 lays the groundwork (one E2E test); PR2/PR3/PR4 build on it.

Lessons from the 8-round codex arc inform the test architecture: every state machine codex flagged (holder_id flap, marker-readback ordering, nonce path mismatch, ack-write-lost recovery) needs an explicit test exercising the race. Mocking tmux entirely would miss exactly this class — codex's findings prove it.

## Test plan (cross-cutting)

**PR1:**
- Delivery controller (coord_prompt_inbox only) unit tests: AcquireAndDeliver atomicity, Release idempotent.
- Worker dispatch end-to-end: spawn → terminal → inbox unlinked. Verify via `/tmp/fleet-leak-scan.sh`.
- Crash-recovery: kill coord mid-AcquireAndDeliver, sweeper recovers.
- Pre-migration sweep dry-run lists known leaks; --kill cleans.

**PR2:**
- Delivery: handoff_resume_inbox + remote_control_inbox + Rewrite atomicity.
- Exclusive controllers + per-kind status enums (Alive/Dead/Unknown for tmux; Live/Archived/Missing for agent_record).
- Adoptable: worker → reviewer → finisher adoption; ReleaseIfTaskTerminal no-ops mid-task.
- Cross-socket refusal (same-host different-tmux-socket).
- P3 task triage results in PR description.

**PR3:**
- Replace × 5 failure modes × 4 call sites.
- Ownership proof step 6: SessionAlive + replace-nonce-file presence + marker readback (sanity check).
- Race test: NEW writes its nonce file; verify OLD's concurrent tick CANNOT write a colliding entry. OLD's process lacks `FLEET_REPLACE_NONCE` (env-scoped to NEW's spawn); even though OLD can learn NEW.DispatchID from `tmux ls`, the path includes the nonce so OLD has no way to construct the correct filename without the nonce VALUE.
- Nonce file lifecycle: Replace step 7 commit atomic-updates `consumed_at`; abort/rollback path leaves the file unconsumed for operator triage (no auto-delete). 30-day TTL sweeper retires consumed nonces; unconsumed ones surface as operator-visible stale-replace evidence.
- 3-phase resume prompt with each step failing.
- `marker == NEW.ID` recovery.
- OldIsDead path.
- P3 task triage.

**PR4:**
- Sweeper modes 1+2+3 end-to-end.
- `internal/lifecycle/` callers migrated; package deletable.
- TUI banner; `fleet dispatches/claims` outputs.
- Final P3 triage. Memory updates.
- Leak-scan CI gate: post-test-suite `sweep-leaks --dry-run` returns 0.

---

## Risks & mitigations

| Risk | Mitigation |
|---|---|
| Re-arch introduces new bugs | Per-PR test suites; PR1 minimal blast radius; regression tests for specific bugs closed. |
| 3-week delivery → other P1 work blocked | Other P1s (release-v0-10-0-cut, coord-rolling-checkpoint) ship in parallel. Only swap-v6 + #101 are folded in. PR #135 stays paused. |
| Premature abstraction | Vertical slice in PR1 validates controllers end-to-end before PR2 expansion. |
| Manifest store growth | Archive subdir; operator pruning out of scope v0.11. |
| Migration race v0.10 → v0.11 | Pre-v0.11 cleanup still executes; sweeper --orphans catches legacy. |
| Cross-host / cross-socket | Explicit single-machine v1 invariant + HostID + TmuxSocket on every claim + sweeper refuses cross-* reclaim. |
| Mid-PR triage discovers complexity | Triage can defer to "keep as separate task"; doesn't block PR. |
| Same-file flock contention under load | Per-dispatch journal; multiple dispatches don't contend on each other's journals. Within one dispatch, claim acquisitions are serialized by design. |

---

## Decision log

- **2026-05-15** — Operator approved re-architecture after seeing inbox leak as 2nd instance of pattern within 48h.
- **2026-05-15** — Operator chose "drive design conversation with codex" → 3 codex rounds + plan-eng-review.
- **2026-05-15** — `atomic-coord-swap-v6-uni-b09b` folded into PR3. `internal/lifecycle/` (issue #101) folded into PR4.
- **2026-05-15** — PR #135 (codex-engine MVP) stays paused.
- **2026-05-15** — 3 operator-approved shape decisions: ONE primitive + typed controllers; vertical slice; 5 resource classes.
- **2026-05-15** — Operator added: repo cleanup is part of refactor scope.
- **2026-05-15** — Codex round 1 critique folded (typed controllers, vertical slice, host_id, Inspect vs Probe, 5 classes).
- **2026-05-15** — plan-eng-review decisions folded (v9):
  - A9 (test infra): real-tmux via `internal/testutil/tmuxtest` + boundary fakes for fleet-guard, coord-state, Agent-tool returns. New "Test infrastructure" section.
  - A6 (ID invariant): `DispatchID` as named type wrapping agent_id; constructor test pins 8-hex shape + cross-language (Go ↔ Python) byte-equal.
  - A1 (CLI contract): golden-file tests in `cmd/fleet/claims_test.go` with fixtures under `cmd/fleet/testdata/claims/`. One fixture per (subcommand, outcome) pair.
  - A4 (archive growth): ship `prune-dispatch-archive` in PR4 alongside sweeper. Default 90-day retention; auto-trigger at >100MB archive size. Eliminates the indefinite-deferral risk.
  - CQ3 (lock timeout): `AdoptableLockTimeout = 10 * time.Second` constant; caller overridable via context deadline.
  - CQ1 (doc structure): added "TL;DR for implementers" section at top — 10-line summary, invariants, gotchas, PR sequencing. Decision log stays inline as institutional memory.
  - T1 (regression test): PR1 ships CRITICAL E2E regression test — full dispatch → terminal → inbox-unlinked-and-journal-archived cycle.
  - T2 (crash recovery): PR1 ships explicit kill-9 mid-AcquireAndDeliver recovery test. Sweeper drops orphaned `allocating` claims.
- **2026-05-15** — Codex round 7 critique folded (v8):
  - Step 7 path reference fixed to match step 6's `<NEW.DispatchID>-<replaceNonce>.json` (was lagging behind v7's filename widening). One-line behavioral-bug fix codex caught.
- **2026-05-15** — Codex round 6 critique folded (v7):
  - Nonce filename widened from `<NEW.DispatchID>.json` to `<NEW.DispatchID>-<nonce>.json`. Closes round-6 P2 (32-bit DispatchID birthday collision could clobber an older Replace's audit record).
  - Failure-modes section + PR3 test-plan: scrubbed remaining stale "24h TTL auto-delete" + "step 7 unlinks" + "OLD lacks NEW.DispatchID" prose. Aligned with v6's no-auto-delete + nonce-value-as-barrier model.
  - Stale-replace evidence is now explicitly never auto-deleted (sweeper logs it; operator triages).
- **2026-05-15** — Codex round 5 critique folded (v6):
  - Replace step 7: **stop unlinking the nonce file**. Instead, atomically update it to add `consumed_at=<ts>`. Closes round-5 P1 (the unlink-and-rewrite hole). Nonce file becomes a permanent per-Replace audit record; 30-day TTL sweeper handles retention. Non-consumed nonces older than the Replace timeout are operator-visible stale-replace evidence, not auto-deleted.
  - Nonce-file first-tick write gate: content-match (not absence-match). Idempotent across re-ticks regardless of file state changes.
  - Nonce-write hook explicitly placed at top of `_tick_locked`, before `bootstrap_remote_control()` (codex round 5 placement note).
  - R4.1 prose tightened: OLD CAN learn NEW.DispatchID (via tmux ls) and the nonce file path. The proof's actual barrier is the nonce VALUE (env-var, same-UID boundary). Removed structural-impossibility overclaim.
  - Stale references scrubbed: lock outcome table, error table (per-task_slug bundle lock), answered-question section.
- **2026-05-15** — Codex round 4 critique folded (v5):
  - Replace step 6: `holder_id` in coord-state.json replaced with **per-NEW-dispatch nonce file** under `~/.fleet/projects/<p>/coord-replace-nonces/` (filename shape evolved to `<NEW.DispatchID>-<replaceNonce>.json` in v7 to avoid DispatchID-collision clobber; see Replace step 6/7 for the canonical path). Closes round-4 R3.1 last-writer-wins flap — OLD lacks the nonce VALUE (env-var, same-UID boundary). [Note: v6 prose overclaimed structural impossibility; round 5 P2 + round 6 P3 fixes corrected this to "OLD lacks the nonce value".]
  - Adoptable lock widened from per-`{kind, task_slug}` to per-`task_slug` (drops kind segment). One lock covers worker_dir + worktree bundle. Closes round-4 R3.3 split-ownership hole.
  - Stale-prose scrub: `mtime advance` references removed (no longer the proof signal). `ack_id` row in error table rewritten to use `delivery_id` model. PR1 table row aligned with prose (loop.py call sites migrate; helper stays for PR2).
- **2026-05-15** — Codex round 3 critique folded (v4):
  - Replace step 6 ownership proof: added NEW-specific `coord-state.holder_id` signal written by NEW's first /coordinator tick. Marker readback demoted to sanity check.
  - PR1 helper scope narrowed: migrate only `loop.py` call sites; leave `dispatch.py:write_worker_inbox` helper for PR2 (`handoff_resume.py:366` still uses it).
  - Adoptable AcquireOrAdopt: added per-`{kind, task_slug}` lock under `~/.fleet/claims-locks/`. Mirrors existing `internal/workers/workers.go:423` pattern.
  - Resume-prompt 3-phase: added `delivery_id` minted at prepare, embedded in delivered content, receiver-side dedup cache at `~/.fleet/projects/<p>/resume-prompts-seen.json`.
  - `fleet claims` CLI: stable outcome enums (`acquired | already_acquired | released | already_released | not_owned | absent | contested | error`) with stable exit codes + JSON `outcome` field. `rewrite-prompt` consumes stdin.
  - Pinned `dispatch_id == agent_id` invariant in PR1↔PR2 overlap rule.
  - Doc/code mismatch fixed: coord_prompt_inbox is read by the coord agent (passes content to Agent tool), not the subagent's first turn.
  - All 5 round-3 open questions answered authoritatively; section consolidated.
- **2026-05-15** — Codex round 2 critique folded (v3):
  - Collapsed to ONE manifest store (claims inline in journal). Eliminates split-brain.
  - Split inbox into 3 Delivery kinds (coord_prompt, handoff_resume, remote_control).
  - Replace ownership proof: drop coordinator.lock body assumption; use SessionAlive + marker readback + mtime advance.
  - Added `tmux_socket` field to discriminate same-host different-socket case.
  - PR1 strictly minimal: coord_prompt_inbox only.
  - CLI moved off `fleet dispatch` to hidden `fleet claims`.
  - Resume prompt: 3-phase prepare/deliver/ack.
  - Audit class extended: `~/.fleet/incidents/`, `projects/<p>/subagents/`.
  - Open questions answered: per-resource files, monotonic Generation per {kind, task_slug}, tasks.md wins for derived, AcquireOrAdopt task-terminal reads from `internal/tasks` not shell-out.

---

## Open items before draft-freeze

- [ ] Codex round 3 review of v3 → PASS or NEEDS_REVISION.
- [ ] `/plan-eng-review` lock-in (after codex clean).
- [ ] Operator final approval (G2 gate).
- [ ] After approval: file PR1-PR4 as P1 tasks; dispatch worker on PR1 (vertical slice).
- [ ] Operator pre-PR1 prep:
  - [ ] Untracked-file audit decision (`internal/testutil/tmuxtest`).
  - [ ] Approve pre-migration sweep script (dry-run before --kill).
