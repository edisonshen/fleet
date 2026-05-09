# Coordinator workflow — what to expect

This is the operator-facing reference for how a Fleet coordinator runs an end-to-end engagement. The coord's own runbook (the same six steps, written for the agent) lives at [`skills/coordinator/SKILL.md`](../skills/coordinator/SKILL.md). If the two ever drift, the SKILL is the source of truth — file an issue.

## TL;DR

You hand a coord a problem. It plans with you, splits, dispatches workers, watches CI, ships PRs, and only interrupts you when something genuinely needs human input.

```
                    operator approves plan (G2)
                              │
   1. DISCUSS  ───────────────┼──→  2. SPLIT  ──→  3. TASK LIST  ──→  4. IMPLEMENT
   plan + eng + tests         │     ≤10 inline       one-line goal       one impl-subagent
   AskUserQuestion            │     >10 planner-     status in            per task; cap=1
   until approved             │     subagent (G1)    structured fields    (G7); §4 reviews
                              │                                                │
                              ▼                                                ▼
                       autonomous from                              5. PR-TRACK
                       step 2 onward                                async-waits CI poll (G4)
                                                                    fail → fix-subagent (cap=3) (G3)
                                                                    rebase trivial → rebase-subagent
                                                                    rebase non-trivial → raise-hand
                                                                                │
                                                                                ▼
                                                                          6. DONE
                                                                          fleet tasks set pr_url=…
                                                                          fleet tasks set status=done
                                                                          advance; loop until empty
```

## The six steps

### 1. DISCUSS

You bring a problem; the coord drives a planning conversation. Scope, engineering detail, testing plan, edge cases. Expect the coord to use `AskUserQuestion` when the ambiguity is real, and to push back when scope is creeping. **No work dispatches until you approve the plan** — this is the single approval gate (G2).

What you do: answer questions, refine scope, approve the plan when you're satisfied.

What the coord does: read code (Read / Grep / non-mutating Bash) to ground the discussion. It will not edit source files at this step.

### 2. SPLIT

Once you approve, the coord splits the plan into tasks.

- **Inline (≤10 tasks):** the coord writes them itself via `fleet tasks add`.
- **Planner-subagent (>10 tasks):** the coord dispatches a single subagent whose only job is to produce the task list. It exits without dispatching workers — that's step 4.

Threshold = 10 (G1). You'll see a series of `fleet tasks add ...` invocations or a single Agent-tool dispatch labelled "planner".

### 3. TASK LIST

Each task ends up as a one-line goal in `~/.fleet/projects/<project>/tasks.md`:

```
- <slug>: <one-line goal>
```

Status, branch, PR URL, worker PID, notes — all in structured fields under the task heading. Run `fleet tasks list` to see the current view; `fleet tasks show <slug>` for full detail. The schema is enforced by `fleet tasks` itself — see [`docs/STATE.md`](STATE.md) for the field set.

### 4. IMPLEMENT

The coord dispatches **one** impl-subagent per task. v0.2 cap is 1 in flight per project (G7). When worktrees land in v0.2.x via `coord-config.json:parallelism`, the cap lifts.

Each impl-subagent follows the global Subagent Dispatch Contract:

- Reads `~/.claude/CLAUDE.md` and the project `CLAUDE.md` first.
- Writes WIP checkpoints to `~/.fleet/subagent-wip/<task-tag>.md`.
- Runs **codex review + /review skill** in multiple rounds until both return clean.
- Pushes its branch and opens its PR autonomously when reviewers are clean (no operator gate at the push step — that gate was step 1).
- Returns the PR URL (or `BLOCKED` with reason).

The coord does NOT foreground-poll the subagent. It resumes off the harness's `<task-notification>` when the subagent finishes.

### 5. PR-TRACK

The impl-subagent returns a PR URL. The coord:

1. **Tells you** about the PR (no permission ask — that ship sailed at step 1).
2. **Watches CI** using the `## Async waits` pattern from `~/.fleet/standards.md` (G4): a single background bash with an `until` loop polling `gh pr view --json state`, resumed by `<task-notification>` when CI flips. No foreground polling, no prompt-cache thrash.
3. **On CI fail** (G3): dispatches a **fix-subagent** against the same branch with the failure log. Cap = 3 attempts per task. On the 4th failure, raise-hand to you with the WIP path and the failure log.
4. **On rebase conflict** (G3):
   - Mechanical (e.g., parallel CHANGELOG.md edits) → **rebase-subagent** with strict "rebase only, no scope changes" instructions.
   - Business-logic conflict → raise-hand. Operators decide rebase semantics, not coords.
5. **On merge** → step 6.

### 6. DONE

CI green + PR merged → coord runs `fleet tasks set <slug> pr_url=<url>` followed by `fleet tasks set <slug> status=done` (the CLI accepts one `key=value` per call). Coord advances to the next task.

When the task list is empty: `"all tasks done; next direction?"` — and the coord waits.

## Approval gate (G2)

There is exactly **one** approval gate: step 1. After plan approval, the coord runs steps 2–6 autonomously. It raises hand only when:

- A subagent (impl / fix / rebase) returns `BLOCKED`.
- The CI fix-loop hits cap=3 on a single task.
- An impl-subagent discovers mid-implementation that the plan is wrong (scope-change discovery).
- A new P0 message lands in `~/.fleet/inbox/<coord-id>.md`.

CI pending, rebase trivial, subagent iterating — all normal flow, all autonomous.

## State documents (G5)

Three kinds of doc, three owners:

| Doc | Path | Owner | What it's for |
|-----|------|-------|---------------|
| Subagent doc | `~/.fleet/subagent-wip/<task-tag>.md` | impl- / fix- / rebase-subagent | Phase log per the global Subagent Dispatch Contract. Coord reads on BLOCKED to recover. |
| Progress doc | `~/.fleet/projects/<project>/workflow.md` | **coord** | Operator-readable phase log. One section per task with `phase = discussing \| approved \| dispatched \| reviewing \| pr-open \| merged \| blocked`. |
| Coord doc | `~/.fleet/agents/<coord-id>.json` | fleet-guard | Live-state heartbeat the TUI renders. No coord-side change. |

The **progress doc** has a stable schema:

```
---
schema: v1
project: <name>
updated_at: <RFC3339 UTC>
---

# workflow

## <slug-1>
- phase: <discussing | approved | dispatched | reviewing | pr-open | merged | blocked>
- updated_at: <RFC3339 UTC>
- pr_url: <url or empty>
- note: <optional one-line context>

## <slug-2>
...
```

Atomic publish is mandatory: tmp-fd → `fsync` → `os.replace`. A partial-write `workflow.md` is worse than no file. TUI rendering of `workflow.md` is out of scope for now; a `cat ~/.fleet/projects/<project>/workflow.md` reads it directly.

## Mid-flight intervention (G6)

You can interject by writing to `~/.fleet/inbox/<coord-id>.md` (or via `fleet message <coord-id> ...`). The coord polls the inbox **each turn** — fleet-guard already routes operator → agent messages there. A new P0 preempts after the **current atomic step completes** (one CLI mutation, one Agent dispatch). The coord will not interrupt itself mid-tool-call; partial state at a tool boundary is the only invariant. Expect a turn or two of latency before the message is acknowledged on a busy coord.

## Parallelism cap (G7)

v0.2 enforces one impl-subagent at a time per project. Two layers:

1. `coordinator.lock` (NB-flock) — one coord process per project.
2. The dispatch loop's `active >= cap: break` guard.

No new locking. The cap lifts when worktrees ship in v0.2.x via `coord-config.json:parallelism > 1`; the existing file-overlap heuristic (`skills/coordinator/conflict.py`) handles the conflict concern.

## Async-waits (G4)

The coord's CI poll is **the** load-bearing background-bash example in Fleet. It uses the canonical pattern from `~/.fleet/standards.md` `## Async waits` — background bash + `until` loop + `<task-notification>` resume. The 30 s poll interval matches PR-merge cadence; tighten only when you explicitly want faster reaction on a particular task. Don't ask the coord to invent a different mechanism — the standards section is the contract for every dispatched worker, including the coord itself.

## Out of scope (this revision)

- TUI rendering of `workflow.md` — the file is written, not yet drawn. A follow-up adds a column in the dashboard.
- Cap > 1 with worktrees — gated until v0.2.x.
- Multi-project supervision — v0.3 adds a thin operator view across coords; the per-project workflow above does not change.

## Related

- [`skills/coordinator/SKILL.md`](../skills/coordinator/SKILL.md) — the coord's own runbook (this doc's mirror).
- [`docs/PLAN-v0.2-coordinator.md`](PLAN-v0.2-coordinator.md) — the v0.2 design that introduced the coord.
- [`docs/ENG-v0.2-coordinator.md`](ENG-v0.2-coordinator.md) — package shapes, sequence diagrams, perf budget.
- [`docs/STATE.md`](STATE.md) — filesystem schema and reliability invariants.
- `~/.fleet/standards.md` — the project-wide standards baseline; `## Async waits` is the canonical CI-polling pattern.
