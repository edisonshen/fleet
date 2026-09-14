# Coordinator workflow — what to expect

This is the operator-facing reference for how a Fleet coordinator runs an end-to-end engagement. The coord's own runbook (the same eight steps, written for the agent) lives at [`skills/coordinator/SKILL.md`](../skills/coordinator/SKILL.md). If the two ever drift, the SKILL is the source of truth — file an issue.

## TL;DR

You hand a coord a problem. It plans with you, saves the approved implementation plan as a durable doc, splits, saves worker-ready task plan docs, dispatches task subagents, watches CI, ships PRs, and only interrupts you when something genuinely needs human input.

```
                    operator approves plan (G2)
                              │
   1. DISCUSS  ───────┼→ 2. PLAN-DOC ─→ 3. SPLIT ─→ 4. TASK LIST ─→ 5. TASK-PLAN-DOC ─→ 6. IMPLEMENT
   plan + tests       │  DESIGN-<topic>  ≤10 inline    one-line goal    TASK-PLAN-<slug>     worker→reviewer→finisher
   until approved     │  .md + .html     >10 planner   status fields    .md + .html          cap=1 task by default
                      │                                                                               │
                      ▼                                                                               ▼
              autonomous after                                                                7. PR-TRACK
              PLAN-DOC succeeds                                                               async-waits CI poll (G4)
                                                                                               fail → fix-subagent (G3)
                                                                                               rebase non-trivial → raise-hand
                                                                                                           │
                                                                                                           ▼
                                                                                                     8. DONE
                                                                                                     set pr_url + status=done
                                                                                                     advance until empty
```

## The eight steps

### 1. DISCUSS

You bring a problem; the coord drives a planning conversation. Scope, engineering detail, testing plan, edge cases. Expect the coord to use `AskUserQuestion` when the ambiguity is real, and to push back when scope is creeping. **No work dispatches until you approve the plan** — this is the single approval gate (G2).

What you do: answer questions, refine scope, approve the plan when you're satisfied.

What the coord does: read code (Read / Grep / non-mutating Bash) to ground the discussion. It will not edit source files at this step.

### 2. PLAN-DOC

Once you approve an implementation plan, the coord saves it before it files tasks. This keeps the design decision durable and makes future publishing to `/projdoc/` automatic once a watcher is installed.

- Scope: implementation plans only. Status chats, casual Q&A, and exploratory discussion do not need docs.
- Default location: the active project's `docs/` folder when present. If the project has no clear docs location, the coord asks before writing.
- Filename: `DESIGN-<kebab-topic>.md`.
- Companion render: if the project has a renderer such as `scripts/render-design-doc.py`, the coord also writes `DESIGN-<kebab-topic>.html`.
- Required contents: summary, design decisions, task split, test plan, assumptions, and approval timestamp.

This and TASK-PLAN-DOC are the coord's only source-tree mutation exceptions. If save or render fails, the coord raises hand and does not split tasks.

### 3. SPLIT

Once the plan doc exists, the coord splits the plan into tasks.

- **Inline (≤10 tasks):** the coord writes them itself via `fleet tasks add`; promotion waits for TASK-PLAN-DOC.
- **Planner-subagent (>10 tasks):** the coord dispatches a single subagent whose only job is to produce the task list. It exits without dispatching workers — that's step 5.

Threshold = 10 (G1). You'll see a series of `fleet tasks add ...` invocations or a single Agent-tool dispatch labelled "planner".

### 4. TASK LIST

Each task ends up as a one-line goal in `~/.fleet/projects/<project>/tasks.md`:

```
- <slug>: <one-line goal>
```

Status, branch, PR URL, worker PID, notes — all in structured fields under the task heading. Run `fleet tasks list` to see the current view; `fleet tasks show <slug>` for full detail. The schema is enforced by `fleet tasks` itself — see [`docs/STATE.md`](STATE.md) for the field set.

### 5. TASK-PLAN-DOC

Before implementation, the coord saves a worker-ready task plan doc for each implementation task.

- Filename: `docs/TASK-PLAN-<slug>.md`, plus `.html` when a renderer is available.
- Contents: parent design doc, task goal, acceptance criteria, expected files/surfaces, a `## Scenario contract`, tests removed / KEEP, non-goals, dependencies, and approval timestamp.
- Scenario contract: one row per requirement the worker must prove on the running product — `| S | Requirement | Stand-up | Trigger | Observable outcome | Level | Harness |`. The observable outcome is what the user/operator would see (response body, file content, screen/pane text, exit code, DB row, PR state), never "function X is called". Level defaults to the project's max level from the merged standards' `## Sandbox` tier: `local`/`remote` → `e2e` (the running product via the project's `up` / `observe` / `down`), `none` → `replay` (the highest-fidelity artifact driven through the real boundary); `integ` is a package test at a real boundary that needs no `up`; `unit` is only for pure logic a scenario cannot reach. A row below the max level carries a one-word reason (`unit — pure`, `integ — hermetic`, `replay — no-artifact`); plan review rejects a row whose Level exceeds the tier (`contract S<n> needs e2e but tier=none`). The Harness column names the `up` / `observe` commands (e2e), the artifact (replay), or the package harness (integ). If the merged standards lack `## Sandbox` or `## Gates`, the plan carries a "standards onboarding" note and the coord raises it to the operator at promote. Rows sharing a stand-up group into one table test, as the plan says. A task with nothing observable writes `none — <reason>` instead of a table.
- Worker visibility: the coord links or embeds the task plan in worker-visible task text before promotion, for example `fleet tasks note --project <project> <slug> --section spec "Task plan: docs/TASK-PLAN-<slug>.md"`.
- Promotion: the coord runs `fleet tasks promote <slug>` only after the task plan doc exists, the HTML render succeeds when configured, and the task text points workers at the plan.

If save or render fails, that task stays unpromoted and no worker is dispatched for it.

### 6. IMPLEMENT

The coord dispatches one active implementation task at a time by default (G7). Each task moves through three detached subagents: worker → reviewer → finisher.

- **Worker:** runs the scenario-first arc using only the merged standards' `## Sandbox` and `## Gates` — `spec-repro` (export `FLEET_WORKER_SLUG=<slug>`, `eval "$(<up>)"`, run each e2e row's trigger, capture with `<observe>`, `<down>`; replay rows obtain the named artifact; the wrong outcome is recorded verbatim, and a row above the tier, a missing artifact, or an outcome that is not the wrong one blocks — never a mock for a boundary `## Sandbox` can run), `spec-encode` (one test per row at its level in the project's own framework, failing for that reason), `verify` (fix; re-run by hand and via the test; every `## Gates` line in order, `baseline:` for red believed pre-existing). Evidence goes in `~/.fleet/projects/<project>/workers/<slug>/verification.md` (`## Scenario contract`, `## Evidence — before`, `## Evidence — after`, `## Gates`, `## Baseline`, `## Unit tests`); a red gate that is the worker's own is fixed, never handed off, and a red believed pre-existing is re-run on untouched `origin/main` with both results under `## Baseline`. It records `--scenarios-total M --scenarios-verified N --gates-status passed`, commits locally, updates `phase=review-pending`, then exits. It does not run review and does not push.
- **Reviewer:** runs `/review` and `codex review` until clean with the Scenario-contract lens (every row has a test at its stated level asserting the observable outcome; `## Evidence — before` proves it would have failed; a missing row, a row below the `## Sandbox` max level without the plan's reason, or a mock for a boundary `## Sandbox` can run is P1), fixes any P0/P1 findings with tests, and after any fix re-runs the touched scenarios plus every `## Gates` line and appends `## Review re-verification` to verification.md. It records terminal review fields, updates `phase=review-done`, then exits. `/review` is never skippable; codex can be recorded as skipped only for the allowed reasons.
- **Finisher:** verifies terminal review fields, reads verification.md, pushes the branch, opens the PR with `## Verification` = verification.md verbatim (the non-git finisher writes the same block into the done note), records `phase=done + pr_url`, then exits. A missing verification.md or an empty `## Gates` / `## Evidence — after` blocks with `finisher: no verification evidence (<path>)` instead.

The coord does NOT foreground-poll the subagent. It resumes off the harness's `<task-notification>` when the subagent finishes.

### 7. PR-TRACK

The finisher returns a PR URL. The coord:

1. **Tells you** about the PR (no permission ask — that ship sailed at step 1).
2. **Watches CI** using the `## Async waits` pattern from `~/.fleet/standards.md` (G4): a single background bash with an `until` loop polling `gh pr view --json state`, resumed by `<task-notification>` when CI flips. No foreground polling, no prompt-cache thrash.
3. **On CI fail** (G3): dispatches a **fix-subagent** against the same branch with the failure log. Cap = 3 attempts per task. On the 4th failure, raise-hand to you with the WIP path and the failure log.
4. **On rebase conflict** (G3):
   - Mechanical (e.g., parallel CHANGELOG.md edits) → **rebase-subagent** with strict "rebase only, no scope changes" instructions.
   - Business-logic conflict → raise-hand. Operators decide rebase semantics, not coords.
5. **On merge** → step 8.

### 8. DONE

CI green + PR merged → coord runs `fleet tasks set <slug> pr_url=<url>` followed by `fleet tasks set <slug> status=done` (the CLI accepts one `key=value` per call). Coord advances to the next task.

When the task list is empty: `"all tasks done; next direction?"` — and the coord waits.

## Approval gate (G2)

There is exactly **one** approval gate: step 1. After plan approval, the coord saves the plan doc, then runs steps 3-8 autonomously. It raises hand only when:

- A subagent (worker / reviewer / finisher / fix / rebase) returns `BLOCKED`.
- The CI fix-loop hits cap=3 on a single task.
- An impl-subagent discovers mid-implementation that the plan is wrong (scope-change discovery).
- A new P0 message lands in `~/.fleet/inbox/<coord-id>.md`.

CI pending, rebase trivial, subagent iterating — all normal flow, all autonomous.

## State documents (G5)

Three kinds of doc, three owners:

| Doc | Path | Owner | What it's for |
|-----|------|-------|---------------|
| Subagent doc | `~/.fleet/subagent-wip/<task-tag>.md` | worker / reviewer / finisher / fix / rebase subagent | Phase log per the global Subagent Dispatch Contract. Coord reads on BLOCKED to recover. |
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
