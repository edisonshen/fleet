# Subagent lifecycle spec

Authoritative contract for how fleet subagents (workers, reviewers, finishers — every spawn that isn't the coord itself) are dispatched, supervised, handed-off, and reaped. Approved by operator 2026-05-14.

This spec is the design input for the implementation work in Tasks #7 / #8 and any future lifecycle hardening PR. When in doubt, prefer the rule in this doc over inferences from existing code.

## Six invariants

### 0. Uniqueness — one worker per {task, job}

A **task** can have multiple workers when those workers are doing different **jobs** for it (e.g., the three-stage flow shipped in PR #133: `implement` worker → `review` worker → `finish` worker). A **project** can have many tasks and therefore many workers in flight.

**The strict rule:** for any single `(task_id, job)` pair there is **at most one live worker at any instant.** Where "live" means tmux session alive AND state.json present AND status != terminal-and-archived.

Examples that are fine:
- Task T1 has `(T1, implement)` worker A and `(T1, review)` worker B running concurrently — different jobs, allowed.
- Project P has tasks T1, T2, T3 each with their own `(Ti, implement)` worker — different tasks, allowed.

Examples that violate the invariant:
- Task T1 has two `(T1, implement)` workers — same task + same job. Forbidden.
- During a handoff for `(T1, implement)`: for one observable moment, OLD and NEW are both alive on the same {task, job}. **Forbidden** — must be made atomic per invariant 3 (registry update is the commit point; old's tmux is killed in the same transaction).

**Enforcement:**
- The dispatch path (`fleet dispatch`, `dispatch.write_worker_inbox`) MUST refuse a spawn when a live worker already owns the `(task_id, job)` pair. Returns an error pointing at the existing worker's ID so the operator (or coord) can attach / handoff instead of double-spawning.
- The coord's poll loop MUST flag any task with `count(live workers for (task, job)) > 1` as an integrity violation, surface it loudly (TUI red, inbox alert), and refuse to assign new work until the operator resolves it.

**Why this matters:** without uniqueness, two workers can race on the same scope (both editing the same files, both opening the same PR, both committing on the same branch). The operator's three-stage-flow architecture deliberately splits work into jobs so multiple agents can act on a task in parallel — uniqueness keeps that parallelism well-defined.

## Six invariants — detail

### 1. Single dispatch authority

**Only the coordinator agent dispatches subagents.** No other entity — not the TUI, not the dashboard, not a worker, not a finisher — may spawn a subagent. Workers MUST NOT dispatch sub-workers (no recursion). The dashboard `[a]` hotkey for "attach coord" is operator-triggered and dispatches a *coord*, not a subagent — that path is out of scope of this rule.

**Rationale:** every subagent must be tracked by exactly one coord. Multiple dispatch paths produce ownership ambiguity and registry drift.

**Enforcement:** the dispatch API (`fleet dispatch`, `dispatch.write_worker_inbox`, Agent-tool wrappers in the coord skill) MUST refuse to spawn a subagent unless the caller identity is a registered coord for the project. Implementation: a coord-id check before any spawn — drop spawns from non-coord PIDs/process trees with a clear error.

### 2. Response is a signal, NOT a completion contract

A subagent delivers a response when it has something to say. That response can be:

- A **question** to the coord ("the operator asked me to delete a prod DB — confirm?")
- An **error** the worker hit ("test failed: X. Should I roll back or fix forward?")
- A **progress milestone** ("phase 1 done, starting phase 2")
- A **final result** with the §7 return-contract payload ("PR #N opened, all gates pass")
- A **blocked** state ("CI fails on Y and I can't fix without operator decision")

**The coordinator — not the worker — judges whether the response means "task complete."** The same response payload can be "done" in one context and "needs more work" in another (e.g., a worker reporting "tests pass" is done if the task was "make tests pass" but is mid-task if the task was "ship feature X" and the PR isn't open yet).

**Lifecycle: request → response → coord-judgment → directive:**

```
  worker.send(response)
        │
        ▼
  coord.receive(response)
        │
        ▼
  coord.judge(task, response) ∈ { complete, ask-followup, error-retry, error-abort, continue }
        │
        ├── complete       → coord.kill(worker)                         (invariant 5)
        ├── ask-followup   → coord.send(worker, next-prompt)            (worker resumes)
        ├── error-retry    → coord.send(worker, fix-instructions)       (worker resumes)
        ├── error-abort    → coord.kill(worker) + coord.dispatch(new)   (fresh worker)
        └── continue       → coord.send(worker, "carry on")             (worker resumes)
```

**Worker behavior between response and coord directive:** the worker waits for the coord's reply. It does NOT idle indefinitely. Specifically:
- After sending the response, the worker enters a bounded wait for the coord's directive.
- If the coord's directive arrives → worker acts (resume / die).
- If the directive does NOT arrive within `responseTimeoutWindow` (default 60 s — see polling cadence in invariant 4): worker writes `status=waiting_for_coord` and remains in waiting state. NOT idle-burning-cycles — the worker process should be parked on a read of its inbox file, not spinning.

**Worker behavior AFTER coord's directive:** the worker MUST NOT idle. Either it's being killed (act on SIGTERM / `/exit`) or it has a new prompt (resume work). The window between "received directive" and "doing the next thing" is < 1 s.

**Why this matters:** the prior incident showed workers accumulating after their "I'm done" was sent. Treating response as auto-completion would close that window mechanically, but it hides bugs in the coord's judgment loop. By making the coord the explicit judge, missed judgments surface as workers in `status=waiting_for_coord` (visible in TUI / `fleet status`) — operator sees the supervision gap and can act.

### 3. Atomic handoff at 50% / 70% context

When a subagent's context window crosses the configured thresholds, a **handoff** spawns a successor subagent and atomically retires the predecessor:

| Threshold | Mode | Behavior |
|---|---|---|
| **≥ 50%** | Graceful (Yellow) | Inject `HANDOFF REQUESTED` marker into the running subagent. Wait for the subagent to emit a `MILESTONE` line in its pane on its own schedule. On MILESTONE: write the handoff doc, queue the successor spawn. |
| **≥ 70%** | Hard (Red) | Skip the MILESTONE wait. Write handoff doc immediately, queue successor spawn, kill predecessor in the same atomic operation. |

**Atomicity guarantee:** at any observable moment during a handoff, EITHER the OLD subagent is the live worker for that task OR the NEW one is. Never both. Never neither.

**Required ordering (matches the existing handoff retire flow):**
1. Spawn NEW (writes NEW tmux session + NEW state.json).
2. Probe NEW liveness via `tmux.SessionAlive` (tristate — refuse to proceed if probe is ambiguous or NEW is dead).
3. Update the **task registry** to point at NEW. The task registry MUST be updated *before* OLD is killed — never after — so the coord's poll loop never observes a task with no live worker.
4. Send `/exit` to OLD, grace window, `tmux.Kill(OLD)`. Confirm OLD session is gone via `tmux.SessionAlive` (false).
5. Archive OLD state.json.
6. Notify the coord (drop a queue file or state-change event at `~/.fleet/inbox/<coord-id>.md` so the coord's poll loop sees the swap on its next tick).

**Rollback:** if step 2 fails (NEW dead at startup): drop NEW record + kill NEW tmux session, OLD untouched, surface error to operator. If step 3 fails (registry/marker write failed after NEW spawned): kill NEW, drop NEW record, OLD untouched, surface error. If step 4 fails (OLD won't die): the swap is **already committed** — the marker (step 3) is the atomic commit point. The OLD tmux session is now an orphan process, NOT the coord. The helper archives OLD's record so `fleet maintenance prune-orphan-tmux` will reap the leaked session, drops an inbox alert at `~/.fleet/inbox/<NEW-id>.md`, logs a `[P0]` line to stderr, and returns `ErrOrphanSurvived` (the invariant still holds: marker resolves to NEW; NEW is alive; OLD is reapable orphan).

This "registry-first" rollback resolves the contradiction in pre-v5 versions of this doc, which said both "update registry BEFORE OLD is killed" (step 3 before step 4) AND "if step 4 fails, refuse to update registry" — those cannot both hold given the ordering. The v5 reading: registry update IS the commit point; everything before is rollback-able, everything after is best-effort retire that degrades to "loud orphan" rather than registry rollback.

**Why "atomic": registry update must be the commit point.** The visible state of "which subagent owns task X" flips in exactly one filesystem rename. Before the rename: OLD owns. After: NEW owns. No coord-poll tick can ever see "no owner."

### 4. Coord polls subagent status on a fixed cadence

The coord **MUST actively poll** each subagent's status, not rely solely on file-notify (fsnotify) edge events. File-notify is the fast path; polling is the safety net.

**Polled fields per subagent:**
- `tmux.SessionAlive(session)` — alive / dead / probe-error.
- `state.json.last_activity_ts` freshness — stale = stuck.
- `state.json.status` — `running` / `idle` / `done` / `failed` / `blocked`.
- `state.json.context_pct` — for handoff trigger decisions.
- (optional) Pane capture-pane snippet — for last-N-lines visibility.

**Cadence:**
- Default poll interval: **5 seconds** per subagent.
- Adaptive: backs off to 30 s when a subagent has been stable (no state-change events) for > 5 minutes.
- Forces a poll tick immediately on any inbox or queue event.

**Output:** the coord's view of "what's alive and what's stuck" must be derivable from polling alone, with file-notify as latency reducer, not source of truth.

**Stuck detection:** `(last_activity_ts older than stuckThreshold) AND (tmux.SessionAlive == true) AND (status == running)` → mark as `stuck`. Stuck subagents are surfaced to operator (TUI badge, optional inbox alert), NOT auto-killed (operator decides).

### 5. Graceful kill when coord judges complete (no idling)

When the **coord judges a subagent complete** (per the judgment loop in invariant 2), the coord's reaper kills it within a bounded window. The trigger is the coord's *judgment* — never the worker's self-reported status alone, never a timer.

**Reaper algorithm (per coord, runs each poll tick):**

```
for each subagent the coord owns:
    rec = state.json[subagent_id]
    judgment = coord.judge_completion(rec)
    if judgment == complete:
        coord.send_kill_directive(rec)            # /exit + grace
        if (now - rec.kill_directive_ts) > graceWindow:  # default 10 s
            if tmux.SessionAlive(rec.session) == alive:
                handoffop.SafeKillAndArchive(rec)   # PR #146 helper
            else:
                # already exited cleanly, just archive the record
                rec.Archive()
        # else: still within grace, leave alone — subagent flushing
    elif judgment == ask-followup, error-retry, continue:
        coord.send_directive(rec, judgment.prompt)
        # worker resumes; reaper does not act
    elif judgment == error-abort:
        coord.send_kill_directive(rec)
        coord.dispatch_replacement(rec.task_id)
    elif judgment == pending:
        # coord has not yet judged; do nothing this tick
        pass
```

**Grace window:** target 10 s after the coord sends `/exit`. Long enough for a clean Claude exit (writing final WIP, flushing logs); short enough that the worker doesn't burn measurable RAM after being told to die.

**Hard-kill escape hatch:** if grace expires AND `tmux.Kill` fails AND session still alive on re-probe → `kill -9` the pane leader PID (from `state.json.pid`). LAST resort. Logs loudly. Operator must see "had to SIGKILL subagent X — investigate."

**Why coord-judges-not-worker-asserts:** if the worker's terminal status alone triggered the kill, a worker that misjudges its own task (e.g., reports `done` when the PR is still in draft and CI is red) would be reaped before the coord could surface the discrepancy. The coord's judgment loop is the bug-surfacing layer — it's the explicit place where "is this actually done?" gets checked against the task contract before the irrevocable kill.

**Audit invariant:** when this spec is fully implemented, a healthy fleet running for 24+ hours should show:
- `tmux ls | grep fleet- | wc -l` ≈ count of *currently running* tasks (not historical sum).
- Zero `fleet-<id>` tmux sessions whose `state.json` is in `~/.fleet/agents/archive/` (archive = "this should be dead and reaped, not idling").
- Zero `fleet-<id>` tmux sessions whose `state.json` is missing entirely (the orphan-leak shape that this whole effort is plugging).

## Implementation map (which PR enforces which invariant)

| Invariant | Enforced by | Status |
|---|---|---|
| 0 — uniqueness one-worker-per-{task,job} | Dispatch-path precheck (refuse double-spawn) + coord poll-loop integrity check | Task #8 (queued) |
| 1 — single dispatch authority | New caller-identity check in `fleet dispatch` and skill-side `register_subagent.py` | Task #8 (queued) |
| 2 — response is a signal, coord judges completion | Coord-side judgment loop + worker-side bounded `waiting_for_coord` state with inbox-park | Task #8 (queued) |
| 3 — atomic handoff w/ registry-first ordering | PR #146 (leak plug) + Subagent #2 (atomic swap helper) | In progress |
| 4 — coord poll loop with fixed cadence + adaptive backoff | Skill change in `skills/coordinator/loop.py` + state.json probe path | Task #8 (queued) |
| 5 — reaper loop with 10 s grace + hard-kill escape | Skill change in `skills/coordinator/loop.py` + uses `handoffop.SafeKillAndArchive` from PR #146 | Task #8 (queued) |

Test isolation hardening (Task #7) is orthogonal but lands alongside because it's the same "kill orphans completely" theme.

## Non-goals

- Worker-spawned subagents (recursive dispatch). Workers do their task and return. If decomposition is needed, the coord splits at dispatch time.
- Auto-killing stuck subagents without operator decision (invariant 4 surfaces, operator acts).
- Replacing fleet-guard's MILESTONE protocol. Yellow handoff still waits for MILESTONE per its existing spec.
- Reaper loop running outside the coord process. Reaping is the coord's job — putting it in a separate daemon adds ownership ambiguity.

## Open questions

- Coord-of-coord supervision. If a coord itself fails an invariant (e.g., it doesn't reap a `done` subagent), who reaps it? Probably the operator via TUI, but worth confirming.
- Grace window tuning. 10 s is a reasonable default for Claude SDK exit cost. May need lengthening if final-flush operations are heavier than expected.
- Polling cadence under heavy load (10+ concurrent subagents). 5-s polling × 10 subagents = 2 probes/sec, well within tmux server capacity, but worth measuring.
