---
name: coordinator
description: Per-project coordinator that owns tasks.md, dispatches workers as Claude Agent-tool subagents (run_in_background), monitors PR/CI via gh, and raises hand to the operator only when human input is needed. Reads tasks.md (read-only via parse.py) and mutates exclusively through the fleet CLI (`fleet tasks set`, `fleet tasks note`, etc.) — Go remains the authoritative writer. One coordinator per project enforced via NB-flock on coordinator.lock. v0.2 single-worker mode by default; cap > 1 enabled when worktrees land in v0.2.x.
---

# coordinator

Per-project tick loop for Fleet v0.2. Replaces the operator-driven hand-pick-and-dispatch flow with an autonomous coordinator that watches a markdown task list and feeds workers through the existing v0.1 dispatch primitives.

The skill ships agent-side. Each Stop hook fire (or operator-triggered `fleet message <coord_id> ...`) runs `loop.tick()` once and exits. Restart equals resume from disk state — there is no daemon, no shared memory, no asyncio.

Source of truth: `docs/PLAN-v0.2-coordinator.md` (the what + why) and `docs/ENG-v0.2-coordinator.md` (the how — package shapes, sequence diagrams, perf budget, test plan). The skill mirrors the algorithm in those docs; deviations are bugs.

## Coord agent role

The Claude Code session running this skill is a **coordinator**, not a worker. The agent's job is to discuss design with the operator, file tasks, and dispatch workers. Implementation, testing, and any code-touching work goes to detached worker agents — never inline in the coord's own session.

This section mirrors the first-turn dispatch prompt (`coordSpawnPrompt` in `internal/tui/keys.go`) so the constraint survives context handoffs: a successor coord re-reads `SKILL.md` on its first turn but does NOT see the original spawn prompt.

**ROLE — discuss design with the operator, file tasks, dispatch workers. NEVER:**
- Edit code files (no Edit, Write, NotebookEdit on source code).
- Run tests (no `go test`, `pytest`, etc. — workers handle this).
- Implement features inline.
- Run any tool that mutates the project source tree.

**DELEGATE — for any implementation, testing, or code-touching work:**
1. Discuss design with the operator until aligned.
2. File a task via `fleet tasks add --project <project> --spec <body>`.
3. Promote the task with `fleet tasks promote <slug>` when ready.
4. The /coordinator skill auto-dispatches a worker on next tick.
5. Track progress via the supervisor loop.

**ALLOWED — the toolbox is intentionally narrow:**
- Read code files for design discussion (Read, Grep, Bash with non-mutating commands).
- Run fleet CLI: `fleet tasks {add,list,show,set,note,promote}`, `fleet workers list`, `fleet peek`, `fleet learnings`, `fleet standards show`.
- Run gh CLI for status: `gh pr view`, `gh pr checks`, `gh issue view`.
- Talk to the operator about design, scope, priority.

If the operator says "implement X" or "fix this bug", the right response is "that's worker work — let me file the task and dispatch", NOT to start editing files. The coord is a manager; a coord that does the work itself burns the operator's main context on what should be a detached session.

## Workflow runbook — the six-step canon

This is the load-bearing description of how a coord runs an end-to-end engagement. Every coord follows these six steps in order. Operator-readable mirror lives at [`docs/COORDINATOR-WORKFLOW.md`](../../docs/COORDINATOR-WORKFLOW.md).

```
1. DISCUSS           plan + eng detail + testing plan; iterate to operator approval (G2)
2. SPLIT             approved plan → tasks.md (inline ≤10, planner-subagent >10) (G1)
3. TASK LIST         one-line goal per task; status lives in structured fields (G5)
4. IMPLEMENT         dispatch one impl-subagent per task; cap=1 (G7); subagents follow §4
5. PR-TRACK          poll CI via async-waits (G4); on fail → fix-subagent (cap=3) (G3)
6. DONE              fleet tasks set pr_url=<url> + status=done; advance; raise-hand if empty
```

### Step 1 — DISCUSS

Operator brings a problem. Coord drives a planning conversation: scope, engineering detail, testing plan, edge cases. Use plan mode + AskUserQuestion when ambiguity is real; resolve in the chat thread when it's not. **No work dispatches until the operator approves the plan.** This is the only approval gate (G2).

Tools allowed: Read / Grep / Bash (non-mutating) on the project tree to ground the discussion. No Edit, no Write on source.

### Step 2 — SPLIT

Once the plan is approved, split it into tasks.

- **Inline split (≤10 tasks):** the coord writes each task via `fleet tasks add --project <p> --spec <body>` then promotes ready ones with `fleet tasks promote <slug>`.
- **Planner-subagent (>10 tasks):** dispatch a single Agent-tool subagent whose only job is to produce the task list. Its return contract is the list of slugs it created; it exits without dispatching workers.

Threshold = 10 (G1). Above that, splitting inline burns the coord's context for no benefit; below it, the indirection through a planner adds latency without saving anything.

### Step 3 — TASK LIST

Each task line in tasks.md is a **one-line summary** of the goal. Format:

```
- <slug>: <one-line goal>
```

Status, branch, pr_url, worker_pid, notes — all live in the structured fields under the task heading; `fleet tasks` already enforces the schema. The one-liner is what humans scan; the fields are what the coord acts on. See `docs/STATE.md` for the full per-task field set.

### Step 4 — IMPLEMENT

The coord dispatches **one** impl-subagent per task. v0.2 cap is 1 in flight per project (G7) — `coordinator.lock` (NB-flock) plus the dispatch-loop active-count guard already enforce this; do not add new locking. Cap > 1 unlocks when worktrees land in v0.2.x via `coord-config.json:parallelism`.

The impl-subagent is bound by the §4 reviewer contract from `/Users/pinkbear/.claude/CLAUDE.md`: codex review + /review skill, multiple rounds, until two consecutive clean passes each. The subagent pushes its branch and opens its PR autonomously when both reviewers are clean — no operator gate at the push step (the gate was step 1).

The coord does NOT foreground-poll the subagent. The harness fires a `<task-notification>` when the Agent-tool subagent finishes; the coord resumes off that notification and reads the subagent's return message (PR URL or BLOCKED).

**Dispatch template (impl-subagent):** prompt must reference the global Subagent Dispatch Contract by name, list the task slug as the WIP tag (`~/.fleet/subagent-wip/<slug>.md`), state the base branch, state explicit non-goals so the subagent doesn't expand scope, and require the §7 return-format contract.

### Step 5 — PR-TRACK

When the impl-subagent returns with a PR URL, the coord **shepherds**
the PR to merge — does not just watch it. A PR you own does not stop at
a single terminal event between open and merge: it can flip BEHIND
(another PR landed), DIRTY (conflict), CI-red, or CHANGES_REQUESTED.
The coord wakes on **any** of those, acts, and re-spawns the watch.

The full polling pattern + per-state action matrix + worktree-isolation
rule live in the `## Async waits` → `### PR shepherding` subsection of
`~/.fleet/standards.md` (mirror at
[`docs/STANDARDS-BASELINE.md`](../../docs/STANDARDS-BASELINE.md)) — the
coord inherits that subsection through the merged standards block.
**Read it.** The short form below references it; the matrix lives
there.

1. **Notify the operator** with the PR URL (push, don't ask). No
   "should I open the PR?" dialog — that ship sailed at step 1.
2. **Shepherd the PR** with one background `until` loop per PR
   (`Bash(run_in_background=true)`), waking on actionable states:
   `state != OPEN` OR `mergeStateStatus == BEHIND` OR
   `mergeStateStatus == DIRTY` OR any `statusCheckRollup[].conclusion
   == FAILURE` OR `reviewDecision == CHANGES_REQUESTED`. The harness
   `<task-notification>` resumes the coord when the loop exits. On
   wake, dispatch by predicate (matrix below) and **re-spawn the
   loop** so the PR is always under an active watch. (G4 — async
   waits.)
3. **On CI fail** (G3): dispatch a **fix-subagent** against the SAME
   branch with the failure log. Fix-subagent has the same §4 review
   contract as the impl-subagent. Cap = 3 attempts per task. On the
   4th failure, raise-hand with WIP path + failure log.
4. **On BEHIND or DIRTY**: dispatch a **rebase-subagent** on an
   isolated git worktree (`git worktree add /tmp/fleet-<task>-<pr>
   <branch>`) with explicit "rebase only, no scope changes"
   instructions. Markdown conflicts resolve as additive (keep both
   sides). Substantive Go-code business-logic conflicts: abort +
   raise-hand — operators decide merge semantics, not coords.
5. **On CHANGES_REQUESTED**: address straightforward feedback (typo /
   style) inline; raise-hand for substantive design feedback or
   scope-change requests.
6. **On TERMINAL MERGED** → step 6 (DONE).
7. **On TERMINAL CLOSED-without-merge**: raise-hand — was the work
   abandoned?

**Fix-subagent dispatch template (skeleton):**

```
You are a fix-subagent for task <slug>. The PR at <url> failed CI:
<paste failure log>

Fix the failure. Stay on branch <branch>. Iterate codex + /review until clean.
Push, do NOT close+reopen the PR. Return the PR URL and a one-line summary
of the fix. WIP at ~/.fleet/subagent-wip/<slug>.md (resume if present).
```

**Rebase-subagent dispatch template (skeleton):**

```
You are a rebase-subagent for task <slug>. PR <url> has rebase conflicts
against <base>. Rebase ONLY — no scope changes, no opportunistic refactors.
If a conflict requires business-logic decisions, return BLOCKED with the
conflict hunks. Otherwise rebase, run the verify suite, push --force-with-
lease, return the PR URL.
```

### Step 6 — DONE

CI green + PR merged → coord runs (`fleet tasks set` accepts one `key=value` per call, so this is two invocations):

```bash
fleet tasks set <slug> pr_url=<url>
fleet tasks set <slug> status=done
```

Advance to the next task in priority order. When the task list is empty, the coord raises hand: `"all tasks done; next direction?"` and waits.

### Approval gate (G2)

There is **one** approval gate: step 1. After the operator approves the plan, the coord runs steps 2 → 6 autonomously. The coord raises hand only when:

- An impl- / fix- / rebase-subagent returns BLOCKED (preserve the WIP file path; surface to operator).
- The CI fix-loop hits cap=3 on a single task.
- The impl-subagent discovers mid-implementation that the plan is wrong (scope-change discovery — the plan needs revision before more dispatches).
- A new P0 message lands in `~/.fleet/inbox/<coord-id>.md`.

Anything else — CI pending, rebase trivial, subagent iterating — is normal flow and stays autonomous.

### State documents (G5) — three kinds, three owners

| Doc | Path | Owner | Purpose |
|-----|------|-------|---------|
| Subagent doc | `~/.fleet/subagent-wip/<task-tag>.md` | impl- / fix- / rebase-subagent | Phase log per the global CLAUDE.md §2 contract. Coord reads on BLOCKED to recover; otherwise treats as opaque. |
| **Progress doc** (NEW) | `~/.fleet/projects/<project>/workflow.md` | **coord** | Operator-readable phase log. One row per task with `phase = discussing \| approved \| dispatched \| reviewing \| pr-open \| merged \| blocked`. Atomic publish (`.tmp` + rename + fsync). |
| Coord doc | `~/.fleet/agents/<coord-id>.json` | fleet-guard heartbeat | Live-state source for the TUI. No coord-side change — documented here for completeness. |

The **progress doc** schema:

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

Atomic publish is mandatory — a partial write that fleet-guard or a TUI render reads is worse than no file. The coord uses the same tmp-fd → fsync → `os.replace` dance as `dispatch.write_worker_inbox` (`dispatch.py:354`). The TUI rendering of `workflow.md` is out of scope for this revision; the file is operator-readable via plain `cat` for now.

### Mid-flight intervention (G6)

The coord polls `~/.fleet/inbox/<coord-id>.md` **each turn** (fleet-guard already routes operator → agent messages to that path). A new P0 message preempts after the current atomic step completes — meaning the coord finishes the in-flight tool call (one CLI mutation, one Agent dispatch) and then handles the message before continuing the loop. **Do not interrupt mid-tool-call**; partial state at a tool boundary is the only invariant.

### Parallelism cap (G7)

v0.2 enforces **one** impl-subagent at a time per project. Two layers of enforcement:

1. `coordinator.lock` (NB-flock) — only one coord process per project (`SKILL.md` two-coord-race section).
2. The dispatch-loop active-count guard at step 5 of the loop algorithm above (`active >= cap: break`).

**Do not add new locking.** Cap > 1 lifts via `coord-config.json:parallelism > 1` once worktrees ship in v0.2.x; the existing conflict heuristic (`conflict.py` + `loop._has_conflict_with_inflight`) handles the file-overlap concern.

### Async-waits citation (G4)

CI tracking uses `~/.fleet/standards.md` `## Async waits` verbatim — background-bash + `until` loop + `<task-notification>` resume. Do not invent a parallel mechanism. The 30 s poll interval matches PR-merge cadence; tighten only when the operator explicitly asks for faster reaction time on a particular task.

## Worker dispatch protocol (issue #84 Phase A)

Workers are spawned as **Agent-tool subagents** of this coord agent's Claude session — NOT as detached tmux sessions. They appear in the operator's chat as "N local agents" (Claude-native indicator) and the coord supervisor reads their `state.json` files identically to the v0.2.0 detached-tmux flow.

The Python skill (`/coordinator`) cannot invoke Claude's Agent tool directly — Agent is a Claude tool, not a Python API. Instead, the skill emits structured `DISPATCH:` blocks on stdout and YOU (the coord agent) act on them.

**Block format the skill emits:**

```
DISPATCH: <slug>
  agent_id: <8hex>
  description: <short>
  prompt_file: <abs path>
  run_in_background: true
  subagent_type: general-purpose
END_DISPATCH
```

**What you (the coord agent) MUST do every time you see a DISPATCH block:**

1. Read the file at `prompt_file` (absolute path under `~/.fleet/inbox/<agent_id>.md`).
2. Invoke the Agent tool exactly once with these parameters:
   - `description` = the value on the `description:` line.
   - `prompt` = the full content of the prompt file (the body, verbatim).
   - `subagent_type` = the value on the `subagent_type:` line (currently `general-purpose`).
   - `run_in_background` = `true` (workers are async; they write progress to `state.json` and the supervisor loop reconciles).
3. One Agent call per DISPATCH block. Do this BEFORE the next assistant turn — the dispatch contract is "tick output → spawn subagent" with no detour through prose, design discussion, or other tool calls.
4. The skill has already written the inbox file, minted the `agent_id`, flipped tasks.md to `status=in-progress`, and recorded the agent_id as a note. You are NOT responsible for any of that bookkeeping — only for invoking the Agent tool.

If you see N DISPATCH blocks in one tick output, you make N Agent calls (one per block) on this turn before doing anything else.

**Why this matters:** if you skip the Agent call, the worker subagent never spawns. The task sits in `status=in-progress` in tasks.md and the supervisor loop tries to reconcile a worker that doesn't exist. Reliability of this protocol depends on you following it every time without fail. The supervisor will eventually flip the task back to `todo` after stuck-check timeout, but that's hours of lost time.

Phase B (issue #93) covers the `[a]` task-attach replacement on the TUI side and coord-handoff continuity (see "## Resume after handoff" below). Phase C (deferred): TUI subagent_id rendering.

## Resume after handoff (issue #93 Phase B2)

When fleet-guard hands off the coord at 50/70% context, any in-flight worker subagents the outgoing coord had spawned via the Agent tool die with the parent process. The successor coord (a freshly-spawned Claude session inheriting the role) MUST re-issue the Agent calls for those workers on its first turn — otherwise the workers' WIP files sit orphaned and the operator's next `fleet tasks` poll surfaces stuck-in-progress rows.

The handoff doc carries enough state for this to be deterministic:

- Frontmatter `previous_handoff: <path>` — the chain pointer. Empty / null on the first handoff for the agent.
- `## Active Subagents` body section — one machine-readable line per in-flight worker, written by `fleet-guard/handoff.py` from the outgoing coord's `coord-state.json:worker_agent_ids` map.

**What you (the coord agent) MUST do on first turn after a handoff:**

1. Read your handoff doc (the `## First Action (auto)` section already directs you there). The doc path is in your spawn prompt or in tasks.md.
2. Run the resume helper:

   ```bash
   python3 /path/to/skills/coordinator/handoff_resume.py <handoff-doc-path>
   ```

   The helper:
   - Parses the `## Active Subagents` section into a typed list.
   - For each entry, checks `~/.fleet/subagent-wip/<task-tag>.md` exists.
   - For each (entry, WIP) pair, rewrites the worker's inbox file (`~/.fleet/inbox/<agent_id>.md`) with a "RESUMING" preamble that points the worker at its WIP, then emits a DISPATCH block on stdout in the same format as `loop.py`'s tick output.
3. For each DISPATCH block in the helper's stdout, follow the exact same protocol as "## Worker dispatch protocol" above — one Agent tool invocation per block, `run_in_background: true`, `prompt` = the body of the inbox file referenced in the block.

The resume preamble explicitly tells the worker subagent to read its WIP first and continue from `next_steps` rather than restart — this is the CLAUDE.md §2 contract (Subagent Dispatch — bake in WIP checkpoints + resumability) operationalized across coord handoff.

If the helper emits zero DISPATCH blocks, the outgoing coord had no in-flight workers (the steady-state case) — proceed with the regular `/coordinator` tick. The helper writes a one-line footer to stderr in this case so the absence is observable.

**Skipped entries** (helper writes them to stderr as `# task <slug>: ...`) are not errors — they're entries whose WIP or inbox files have been pruned (worker finished cleanly) or whose `agent_id` was malformed. The successor coord moves on; the supervisor stuck-check will catch any task that stayed in-progress without a worker.

## Invocation

The coordinator agent is dispatched via existing `fleet dispatch`:

```bash
fleet dispatch fleet-coord-<project> \
  --project <project> \
  --cwd <repo-path> \
  --command "claude 'Run the /coordinator skill loop for project <project>.'"
```

Agent-side environment (set by `fleet dispatch`):

- `FLEET_AGENT_ID` — coord's 8-hex ID. Without it the skill exits silently (fleet-guard discipline).
- `FLEET_HOME` — defaults to `~/.fleet/`. Override for sandboxed tests.
- `FLEET_PROJECT` — set by the dispatch path; falls back to argv[0] when invoked manually.

The skill does NOT need a hook payload — `loop.main` reads `FLEET_PROJECT` (or argv) and runs one tick. The tick is short (target < 500ms p99 per ENG §8.1) and emits a JSON summary to stdout.

## Files this skill writes

| Path | Content | Atomicity |
|------|---------|-----------|
| `~/.fleet/projects/<p>/.locks/coordinator.lock` | flock target (zero-byte) | NB acquired per tick, released on exit |
| `~/.fleet/projects/<p>/coord-state.json` | `last_archive_scan_ts`, `worker_agent_ids`, `supervisor.<slug>` (nudged_at, escalated_at, consecutive_stuck_polls, last_phase) | tmp + rename + fsync |
| `~/.fleet/inbox/<worker_id>.md` | freshly built worker prompt for one dispatch, OR a stuck-idle nudge body (issue #79) | tmp + rename + fsync |

## Files this skill reads (config)

| Path | Content | Default |
|------|---------|---------|
| `~/.fleet/projects/<p>/coord-config.json` | `{"parallelism": <int 1..50>}` | `{"parallelism": 1}` (single-worker mode) |

`parallelism > 1` enables worktree-mode dispatch: each worker gets a
git worktree at `~/.fleet/projects/<p>/worktrees/<slug>/` branched
`worker/<slug>` off the repo's HEAD. Worker cwd is the worktree, not
the main repo. Worktrees are removed via `git worktree remove --force`
when the worker's task transitions to `in-review` (TASK_DONE_PR
sentinel) or `done` (CI merged via reconcile). The branch lives on so
the PR stays valid; only the working tree is freed.

The skill does NOT write `tasks.md` directly. Every mutation goes through `fleet tasks set <slug> <key>=<value>` or `fleet tasks note <slug> <text>`, so Go remains the only writer of the per-project task registry. parse.py is read-only inside the skill.

## Loop algorithm

```
1. NB-flock coordinator.lock
   on EWOULDBLOCK → log + exit 0  ("another tick in progress, skipping")

1.5. Orphan worktree cleanup (cap > 1 only):
     `git -C <repo> worktree prune` — drops registry entries whose dirs
     are missing (e.g. coord crashed mid-tick after `git worktree add`).
     Idempotent and best-effort; failures log to stderr but never abort
     the tick. cap=1 mode skips this step (worktrees never created).

2. tasks   = parse(tasks.md)              # parse.py — read-only mirror of internal/tasks
   stds   = `fleet standards show --merged --project <p>`
   learn  = `fleet learnings list --project <p> --limit 20`

3. Reconcile in-flight workers:
   for t in tasks where status in {in-progress, in-review}:
     if t.worker_pid is alive (kill -0):     skip
     elif t.pr_url:
       ci = gh pr checks <url> --json state,conclusion
       all green + merged       → status=done, clear worker
       all green + not merged   → status=in-review, raise "ready to merge"
       not mergeable            → status=todo, clear worker, note "rebase needed"
       failed                   → status=todo, clear worker, note "CI red <url>", raise
       pending                  → leave as-is until next tick
     else:
       status=todo, clear worker, note "worker died without PR"
   apply via `fleet tasks set/note`

4. Drain inbox archive:
   for f in ~/.fleet/inbox/archive/<coord_id>-*.md (newer than last_archive_scan_ts):
     for line in f:
       TASK_DONE_PR=<slug> <url>      → set pr_url, status=in-review
       BLOCKED_QUESTION=<slug> <txt>  → status=blocked, note BLOCKED_QUESTION
       WORKER_FAILED=<slug> <reason>  → status=todo, clear worker, note WORKER_FAILED
       NEW_TASK=<slug>                → wake-only (no mutation)
   persist last_archive_scan_ts to coord-state.json

5. Dispatch ready tasks under cap (default 1; coord-config.json overrides):
   active = count(status==in-progress)
   for t in sort_by_priority(tasks where status==ready and deps_satisfied):
     if active >= cap: break
     # cap > 1 only: skip on file-overlap with any in-flight worker.
     # Conservative — a task with no Files: line is treated as "could
     # touch anything" and is skipped while any worker is in flight.
     # Operators opt out per task by adding `Files: <path-with-ext>`
     # to Spec / Acceptance / Notes (a real path, not a wildcard;
     # the heuristic regex requires a file extension).
     if cap > 1 and _has_conflict_with_inflight(t, in_flight_after_dispatch): continue
     # cap > 1: per-slug git worktree under projects/<p>/worktrees/<slug>
     if cap > 1:
       wt = `fleet workers worktree-path --project <p> <slug>`
       `git -C <repo> worktree add <wt> -b worker/<slug>`
       worker_cwd = wt
     else:
       worker_cwd = repo
     prompt = build_worker_prompt(t, stds, learn, branch=worker/<slug>)
     # Issue #84 Phase A: skill mints agent_id, writes inbox, emits a
     # DISPATCH block. Coord agent (Claude session) reads the block
     # and invokes the Agent tool with run_in_background=true. The
     # Python skill no longer shells out to `fleet dispatch` for
     # workers — that path stayed for v0.1 manual use only.
     agent_id = mint_agent_id()                           # 8-hex token
     write_worker_inbox(agent_id, prompt)                # ~/.fleet/inbox/<agent>.md
     emit DISPATCH block on stdout                        # coord acts on it
     `fleet tasks set <slug> status=in-progress`
     `fleet tasks set <slug> branch=worker/<slug>`
     `fleet tasks set <slug> worktree=<wt>`               # cap > 1 only
     `fleet tasks note <slug> "dispatched as agent <agent_id>"`
     active += 1

6. Return TickResult{skipped, parsed, reconciled, drained, dispatched, raised, errors}

7. Supervisor loop (issue #79). After the initial reconcile/drain/dispatch pass, the coord
   keeps the lock and watches in-flight workers via cheap state.json mtime polling. When a
   worker's mtime advances, scoped reconcile fires for that one slug. Every Nth poll a
   sparse stuck-check pass walks all probes and runs the recovery ladder for any worker that
   has gone idle (heartbeat stale + counter ≥ threshold + tmux session alive).

   Recovery ladder (per worker, per stuck-check pass):
     no nudge yet            → write inbox `[OPERATOR] You appear idle. ...`; record nudged_at
     nudged + cooldown elapsed → `fleet tasks set <slug> status=blocked`; append note
                                  `STUCK_IDLE_ESCALATED: <reason>`; record escalated_at
     escalated + cooldown elapsed → `fleet workers update <slug> --phase blocked --reason ...`

   Env-overridable knobs:
     FLEET_COORD_POLL_INTERVAL_S    default 30 (0 → supervisor disabled, single-tick fallback)
     FLEET_COORD_STUCK_CHECK_EVERY  default 10 (every 10 polls = 5 min on 30 s base)
     FLEET_COORD_STUCK_THRESHOLD_S  default 180 (3 min stale heartbeat)
     FLEET_COORD_STUCK_POLLS        default 3 (consecutive stuck passes before recovery)
     FLEET_COORD_NUDGE_COOLDOWN_S   default 120
     FLEET_COORD_POLL_MAX_S         default 14400 (4 h hard cap)
```

The fleet CLI commands above came from Phase B (`fleet tasks {add,list,show,set,note,archive,promote}`, `fleet learnings {add,list,prune}`, `fleet standards {show,edit}`, `fleet workers {list,prune}`, `fleet peek <slug>`). Their argv contracts must stay stable for this skill — see `cmd/fleet/tasks.go` etc.

## Worker prompt template

Built fresh per dispatch by `dispatch.build_worker_prompt(task, project, standards_md, learnings_text)`. ENG §6.5 specifies the layout. Hard cap 16KB rendered; oversized prompts raise `PromptTooLargeError` and the loop records the failure rather than dispatch.

The rendered prompt is:

```
You are a Fleet worker for task: <slug>
Project: <project>
Branch: worker/<slug>

You are running as a Fleet-dispatched Claude session. The operator is NOT
watching this terminal — communicate progress via `fleet workers update
<slug> --phase <p>` after every phase boundary. Exit cleanly (Ctrl-D /
/exit) once you reach phase=done or phase=blocked; the coordinator polls
workers/<slug>/state.json to know when to advance the task.

State file:  ~/.fleet/projects/<p>/workers/<slug>/state.json
Output log:  ~/.fleet/projects/<p>/workers/<slug>/output.log

## Task
<### Spec body, verbatim from tasks.md>

## Acceptance
<### Acceptance body, verbatim>

## Standards (the bar — non-negotiable)
<merged content from `fleet standards show --merged`>

## Relevant prior learnings
<top entries from `fleet learnings list --limit=20`, ≤500 chars × 5>
(section omitted entirely when no learnings recorded)

## Required workflow
  fleet workers update <slug> --project <project> --phase branch
1. git checkout -b worker/<slug>
  ... (TDD red/green/refactor → review-claude → review-codex → push → done)

## Constraints
- Stay on this task. File incidental bugs (max 3/session, honor system).
- Do NOT edit tasks.md or standards.md directly.
- Stuck → fleet workers update <slug> --project <project> --phase blocked --reason "<one line>"
```

Every `fleet workers update` invocation in the rendered prompt includes `--project <project>` so heartbeats land in the right `~/.fleet/projects/<project>/workers/...` tree even when the worker's cwd basename differs from the project name.

The `## Standards (the bar — non-negotiable)` block in the rendered prompt inlines whatever `fleet standards show --merged` emits. The fleet-shipped baseline (seeded by `fleet init`) carries Testing, Code review, and Async waits sections — see [`docs/STANDARDS-BASELINE.md`](../../docs/STANDARDS-BASELINE.md). The Async waits section is the canonical recipe workers should reach for when reconcile or post-push paths need to wait on PR-merge / CI-green / deploy-finish state changes (issue [#105](https://github.com/edisonshen/fleet/issues/105)): a `Bash(run_in_background=true)` call running an `until <check>; do sleep 30; done` loop fires a `<task-notification>` on exit so the worker resumes without foreground sleep chains, operator pings, or prompt-cache thrash.

`dispatch.write_worker_inbox(agent_id, prompt)` drops the rendered prompt at `~/.fleet/inbox/<agent_id>.md`. The coord agent (Claude session) reads that file and passes the body verbatim as the Agent tool's `prompt` parameter (issue #84 Phase A). fleet-guard's SessionStart hook injection from the v0.1/v0.2.0 era still works for any tmux-spawned worker (e.g., a manual `fleet dispatch` invocation), but the coord skill itself no longer takes that path — workers are Agent subagents now.

## Sentinel grammar (worker → coord)

Workers write status reports to their own `state.json` in v0.2's revised contract (ENG §6.2). The coord watches via the inbox archive when a worker uses `fleet message <coord_id>` to bubble status. Each archive file MUST contain at most one sentinel per line, scoped by task slug:

```
TASK_DONE_PR=<slug> <pr-url>            # worker post-PR-push
BLOCKED_QUESTION=<slug> <one-line text> # worker stuck, needs operator
WORKER_FAILED=<slug> <reason>           # worker hit unrecoverable error
NEW_TASK=<slug>                         # operator (`fleet tasks add`) wake
```

Slug-keyed payloads are the C2 invariant (worker status reports never mix). The drain logic mutates only the task whose slug matches; unknown slugs are silently ignored. `_parse_sentinel` lives in `loop.py` for the canonical grammar.

## Reconcile + raise-hand

When a task is in-progress or in-review and its worker is no longer alive (worker_pid dead AND `workers/<slug>/state.json` either missing, stale, or terminal), the coord decides the next status from two signals in order:

1. **State.json terminal phase (in-progress only).** If the worker exited with `phase=done` + `pr_url`, flip to `in-review`, transcribe the PR URL onto tasks.md, raise "worker shipped". `phase=blocked` + reason → flip to `blocked`, raise. `phase=failed` → requeue to `todo`, clear pr_url, raise. The terminal-phase branch is gated to `status=in-progress` so subsequent ticks (already in-review) drive CI checks instead of re-flipping.
2. **PR URL + `gh pr checks` (in-review or fallback).** When the task is in-review or when in-progress fell through (no terminal state), the coord queries CI:
   - All green + merged → `status=done`.
   - All green + not merged → `status=in-review`, raise ("ready to merge").
   - Not mergeable → `status=todo`, clear worker, note "rebase needed" (keeps pr_url — same branch, different rebase).
   - Failed → `status=todo`, clear worker, **clear pr_url** (next worker opens a NEW PR), raise "CI red".
   - Pending → leave as-is.
   - No PR URL at all → `status=todo`, clear worker, note "worker died without PR".

`gh pr checks` is hit synchronously per tick. ENG §8.2 caches results on `coord-state.json:pr_check_cache` for 5 minutes; v0.2.0 ships without the cache (300ms cost is invisible in idle ticks). v0.2.1 may add it.

## Two-coord race

Second coord on the same project hits NB-flock EWOULDBLOCK. It logs + exits cleanly (`TickResult.skipped=True, reason="lock-busy"`). Operator notices via `fleet status` showing two coord rows; one is no-op. Cleanup: `fleet rm <id>` for the redundant coord.

## Auto-idle-stop (deferred)

PLAN §6 specifies coord auto-stops after 4h of zero active tasks. v0.2.0 does NOT implement this — the coord ticks until manually killed. v0.2.x adds the idle-streak tracking on `coord-state.json` and the clean-exit path.

## Coordinator handoff (C1)

The coord agent is itself under fleet-guard supervision. At 50%/70% context, fleet-guard hands it off. Because tasks.md is the source of truth and `coord-state.json` carries last_archive_scan_ts on disk, the successor coord re-reads tasks.md on its first tick, reconciles in-flight workers via `worker_pid` alive-check, and resumes. ENG §5.6 walks through this in detail.

The skill's tick lifecycle is intentionally lean (no in-process state survives across hook fires) so handoff is clean by construction.

## Failure modes

| Failure | Behavior |
|---------|----------|
| tasks.md parse error | `tick()` returns `skipped=True, reason="parse-error"` and records the error. Coord logs to stderr; operator fixes manually. |
| Two coordinators race | NB-flock makes the second exit cleanly. |
| Coord agent skips a DISPATCH block | Worker never spawns. Task stays in `status=in-progress`; supervisor stuck-check eventually flips to `todo` and re-dispatches. Mitigation: SKILL.md "Worker dispatch protocol" pins the contract; review skipping behavior on every coord handoff. |
| Inbox write fails (permissions, disk full) | Recorded in `TickResult.errors`; the candidate task stays in ready, retried next tick. No DISPATCH block emitted, no Agent call attempted. |
| `fleet tasks set` exits non-zero | Recorded; partial mutations possible (e.g. status set but note not). Next reconcile catches it. |
| `gh pr checks` errors / not installed | `_CIResult(error=...)` — caller leaves the task as-is until next tick. |
| Inbox archive scan finds nothing | Tick proceeds normally. |
| Slug mismatch in a sentinel | Logged via `errors[]`; no mutation. |
| Prompt over hard cap | `PromptTooLargeError` — task NOT dispatched; recorded in errors. Operator shrinks standards/learnings/spec. |

## Tools used

- `python3` ≥ 3.9 (stdlib only — `subprocess`, `pathlib`, `re`, `tempfile`, `fcntl`, `json`).
- `fleet` binary on PATH (provides Phase B CLI: `fleet tasks ...`, `fleet learnings ...`, `fleet standards ...`, `fleet message`, `fleet workers update`). Issue #84 Phase A: the coord skill no longer shells out to `fleet dispatch` for worker spawn — that path stays for v0.1 manual use only.
- `gh` binary on PATH for PR-status checks. Optional: when missing, the reconcile path skips CI evaluation and leaves PR'd tasks as in-review.

## Hook bindings

Unlike fleet-guard, this skill is NOT bound to Claude Code hooks via `~/.claude/settings.json`. It runs as a normal slash-skill the coord agent invokes on its own (`/coordinator`). The Stop hook still drives the cadence — the coord's own assistant turns trigger Stop, fleet-guard ticks, and at the natural sleep boundary the coord runs `/coordinator` again.

## Module layout

| File | Purpose |
|------|---------|
| `SKILL.md` | This document. Frontmatter + invocation + loop-in-prose + worker prompt template. |
| `loop.py` | One-tick driver. Public entry: `tick(project, ...)` and `main(argv)`. |
| `parse.py` | Python mirror of `internal/tasks` — read-only inside the skill, byte-equal with Go. |
| `dispatch.py` | Worker prompt assembly + `mint_agent_id` + DISPATCH-block formatter + inbox writer (issue #84 Phase A — no longer shells out to `fleet dispatch`). |
| `conflict.py` | File-overlap heuristic for cap > 1 (default cap=1 never exercises this). Optimistic on no-paths inputs. |
| `loop._has_conflict_with_inflight` | Conservative loop-side wrapper: a task with no `Files:` line is treated as matching every in-flight task. Operators opt out per task by adding `Files: <real-path>`. |
| `workflow_state.py` | Atomic-publish writer for `~/.fleet/projects/<p>/workflow.md` — the operator-readable phase log per task (G5). Tmp-fd → fsync → `os.replace`; no daemon, no in-process state. |

## Tests

```
python3 -m pytest skills/coordinator/tests/ -v
```

Critical cases (mirrored from ENG §7.2):

- `test_parse.py::test_round_trip_byte_equal` — every fixture in `internal/tasks/testdata/` round-trips byte-equal in Python (CI gate against parser drift).
- `test_loop.py::test_tick_drains_per_task_sentinels` — C2 invariant: two slugs, two archive files, no cross-mutation.
- `test_loop.py::test_tick_no_op_under_lock_held` — second tick exits cleanly under lock contention.
- `test_loop.py::test_slug_mismatch_sentinel_ignored` — unknown slug logs WARN but mutates nothing.
- `test_loop.py::test_tick_skips_on_parse_error` — corrupt tasks.md doesn't crash the coord.
- `test_dispatch.py::test_dispatch_worker_invokes_correct_argv` — exact argv to `fleet dispatch`.
- `test_dispatch.py::test_write_worker_inbox_atomic_and_under_fleet_home` — inbox stub is tmp+rename.
- `test_conflict.py::*` — file-overlap heuristic positive + negative cases.
- `test_loop_conflict.py::*` — loop-side conservative wrapper at cap > 1: skip overlapping tasks, allow disjoint, conservative skip on no-Files candidates, cap=1 bypass.

The fleet binary itself is never invoked in unit tests; subprocess.run is mocked. End-to-end coverage lives in `cmd/fleet/coordinator_integration_test.go` (Phase D).

## Why these design choices

- **Skill is Python, parser ships in Go and Python.** Two parsers mean a CI-gated byte-equality contract. Removing one would force the other side to call out (Go shelling to Python skill = brittle; Python shelling to a Go binary on every tick = expensive). PLAN §"Code: skill side vs Go side".
- **Mutations through CLI, not Python writes.** Single writer per aggregate (STATE.md A2). Skill stays read-only on disk; locking and validation live in Go. Adding a parallel Python writer would require porting state.LockProjectState semantics.
- **One tick per invocation, no daemon.** Stateless reentry over markdown-as-truth is the Ralph rule. Restart equals resume; coord-state.json is the only across-tick state, and it's a couple of timestamps. Every per-task fact lives in tasks.md, every per-worker fact lives in workers/<slug>/state.json or agents/<id>.json.
- **NB-flock per tick, not per coord lifetime.** Flock auto-releases when the Python process exits, so re-acquiring on every tick is the documented pattern (ENG §4.3). Survives coord restarts and handoffs without leaking the lock.
