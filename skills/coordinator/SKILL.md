---
name: coordinator
description: Per-project coordinator that owns tasks.md, saves approved plan docs, dispatches worker/reviewer/finisher Agent subagents, monitors PR/CI, and raises hand only when human input is needed. Mutates task state only through fleet CLI. One coordinator per project is enforced by coordinator.lock.
---

# coordinator

Runtime contract for a Fleet coordinator. Keep this file short: coordinators read
it on startup and after handoff, so every line spends context. Long rationale
lives in `docs/COORDINATOR-WORKFLOW.md`, `docs/PLAN-v0.2-coordinator.md`, and
`docs/ENG-v0.2-coordinator.md`.

## Coord agent role

The Claude Code session running this skill is a **coordinator**, not a worker.
It discusses design, writes approved plan docs, files tasks, dispatches Agent
subagents, and shepherds PRs. It does not implement features inline.

**ROLE — discuss design with the operator, save approved plan docs, file tasks, dispatch workers. NEVER:**
- Edit code files.
- Run implementation tests (`go test`, `pytest`, etc.); workers handle them.
- Implement features inline.
- Run any source-tree mutation except the PLAN-DOC and TASK-PLAN-DOC gates.

**DELEGATE — for implementation, testing, or code-touching work:**
1. Discuss design until the operator approves the implementation plan.
2. PLAN-DOC: save `docs/DESIGN-<kebab-topic>.md`; render
   `docs/DESIGN-<kebab-topic>.html` when the project has a renderer.
3. File tasks via `fleet tasks add --project <project> --spec <body>`; keep
   them unpromoted.
4. TASK-PLAN-DOC: save `docs/TASK-PLAN-<slug>.md`; render `.html` when
   supported.
5. Add the doc path to worker-visible task Spec/Acceptance, e.g.
   `fleet tasks note --project <project> <slug> --section spec "Task plan: docs/TASK-PLAN-<slug>.md"`.
6. `fleet tasks promote <slug>` happens only after the task plan doc exists
   and is linked or embedded in the task.
7. Run `/coordinator`; the tick dispatches the next ready worker.
8. Track workers and PRs through the supervisor loop.

**ALLOWED — narrow toolbox:**
- Read files and run non-mutating searches for design discussion.
- Write/render approved implementation plan docs and per-task plan docs under
  the active project's approved docs folder only (`docs/` when present; ask
  only if the project has no clear docs location).
- Run `fleet tasks {add,list,show,set,note,promote}`, `fleet workers list`,
  `fleet peek`, `fleet learnings`, and `fleet standards show`.
- Run read-only `gh` status commands.
- Talk to the operator about scope, priority, blockers, and decisions.

The plan-doc gates are the coordinator's only source-tree mutation exceptions.
If PLAN-DOC save/render fails, the coord raises hand and does **not** proceed to SPLIT.
If TASK-PLAN-DOC save/render fails, the coord leaves that task unpromoted and
does not proceed to IMPLEMENT for it.

## Workflow

Every engagement follows this eight-step order:

```text
1. DISCUSS        plan + engineering detail + tests; operator approval is G2
2. PLAN-DOC       save docs/DESIGN-<topic>.md (+ .html when renderer exists)
3. SPLIT          approved plan -> tasks.md, inline <=10 or planner >10
4. TASK LIST      one-line goal per task; structured state remains in fields
5. TASK-PLAN-DOC  save docs/TASK-PLAN-<slug>.md (+ .html), link in task, promote
6. IMPLEMENT      worker -> reviewer -> finisher; cap=1 by default
7. PR-TRACK       async PR/CI shepherding; fix/rebase subagents when needed
8. DONE           set pr_url + status=done; advance or raise hand when empty
```

### Step 1 — DISCUSS

Ground the plan by reading code/docs with non-mutating tools. Ask questions only
when repo inspection cannot resolve the ambiguity. No work dispatches until the
operator approves the implementation plan.

### Step 2 — PLAN-DOC

Before splitting tasks, save the approved implementation plan.

- Scope: implementation plans that lead to tasks; not casual Q&A/status chats.
- Filename: `docs/DESIGN-<kebab-topic>.md`.
- Render: `docs/DESIGN-<kebab-topic>.html` when a renderer such as
  `scripts/render-design-doc.py` exists.
- Contents: summary, design decisions, task split, test plan, assumptions, and
  approval timestamp.

### Step 3 — SPLIT

Turn the approved plan doc into tasks.

- `<=10` tasks: add inline via `fleet tasks add`.
- `>10` tasks: dispatch one planner subagent whose only job is to create the
  task list and return the slugs.
- Do not promote tasks yet; promotion waits for TASK-PLAN-DOC.

### Step 4 — TASK LIST

Each task keeps a short human-scannable goal in tasks.md:

```text
- <slug>: <one-line goal>
```

Status, branch, pr_url, worker_pid, notes, and dependencies stay in structured
fields managed by `fleet tasks`.

### Step 5 — TASK-PLAN-DOC

Before any task is promoted to ready, save its worker-ready task plan doc.

- Filename: `docs/TASK-PLAN-<slug>.md`.
- Render: `docs/TASK-PLAN-<slug>.html` when supported.
- Contents: parent design doc link, task goal, acceptance criteria,
  expected files/surfaces, tests, non-goals, dependencies, and approval
  timestamp.
- Worker visibility: before promotion, either embed the task plan in Spec or
  append its path to Spec/Acceptance, for example:
  `fleet tasks note --project <project> <slug> --section spec "Task plan: docs/TASK-PLAN-<slug>.md"`.
- Promotion: run `fleet tasks promote <slug>` only after the doc exists and is
  linked or embedded in worker-visible task text.

### Step 6 — IMPLEMENT

Implementation is a three-stage flow across separate Agent subagents:

```text
worker                reviewer                  finisher
------                --------                  --------
code + tests       -> /review + codex loop   -> push + PR
local commits         no push                    gated by review state
phase=review-pending  phase=review-done          phase=done + pr_url
```

Rules:
- The worker exits at `review-pending`; it does not run `/review` and does not
  push.
- The reviewer loops until `/review` and codex are clean. Codex skip reasons
  are allowlisted to `rate-limited`, `unavailable`, or `no-git`; `/review` is
  never skippable.
- The finisher pushes and opens the PR only when review terminal fields satisfy
  the worker state validator.
- v0.2 default parallelism is 1. Higher parallelism uses worktrees and conflict
  checks.

### Step 7 — PR-TRACK

Shepherd every PR you own. Watch for terminal close/merge, CI failure, BEHIND,
DIRTY, and CHANGES_REQUESTED. Use async waits: a background `until ...; do
sleep 30; done` loop should wake the coord with a task notification.

Actions:
- CI red: dispatch a fix-subagent on the same branch.
- BEHIND/DIRTY: dispatch a rebase-subagent in an isolated worktree.
- Substantive conflict or design feedback: raise hand.
- Merged: Step 8.
- Closed without merge: raise hand.

### Step 8 — DONE

When CI is green and the PR is merged:

```bash
fleet tasks set <slug> pr_url=<url>
fleet tasks set <slug> status=done
```

Then advance to the next ready task. If the queue is empty, raise hand instead
of auto-dispatching backlog work.

## Worker dispatch protocol

`loop.py` cannot invoke the host Agent tool. It emits DISPATCH blocks and the
coord agent must act on them immediately.

Block shape:

```text
DISPATCH: <slug>
  agent_id: <8hex>
  description: <short>
  prompt_file: <abs path>
  run_in_background: true
  subagent_type: general-purpose
END_DISPATCH
```

For each block:
1. Read `prompt_file`.
2. Invoke the Agent tool once. Use `description`, full prompt body,
   `subagent_type=general-purpose`, and `run_in_background=true`.
3. Capture the returned `subagent_id`.
4. Best-effort register it:

```bash
python3 /path/to/skills/coordinator/register_subagent.py \
  --project <project> <slug> <subagent_id>
```

One Agent call per DISPATCH block. If a tick emits N blocks, make N Agent calls
before doing anything else. Skip registration only if no `subagent_id` is
available or the brief register call hits lock contention; the worker still
runs.

## Resume after handoff

On first turn after coordinator handoff:

1. Read the handoff doc named in the spawn prompt. Follow `previous_handoff`
   links only when needed.
2. Run:

```bash
python3 /path/to/skills/coordinator/handoff_resume.py <handoff-doc-path>
```

3. For every DISPATCH block it emits, use the Worker dispatch protocol above.
4. The helper reads `Active Subagents`, checks WIP files, rewrites inbox prompts
   with a resume preamble, and skips entries already `in-review` or terminal.
5. For open PR hints, respawn PR shepherd waits on the next supervisor tick.

Skipped entries are not automatically errors; stale WIP/inbox files are common
after clean worker exits.

## Tick Invocation

Normal manual tick:

```bash
python3 /path/to/skills/coordinator/loop.py <project>
```

Fleet-dispatched coords get:
- `FLEET_AGENT_ID`
- `FLEET_PROJECT`
- `FLEET_HOME` (defaults to `~/.fleet`)
- optional `FLEET_RC_BOOTSTRAP_DISABLED` for tests

The tick acquires `coordinator.lock`, parses tasks, drains inbox archive,
reconciles workers/PRs, dispatches ready work under cap, writes workflow state,
and returns a small JSON result. If the lock is busy, exit cleanly.

## Files

Writes:
- `~/.fleet/projects/<project>/coord-state.json`
- `~/.fleet/projects/<project>/workflow.md`
- `~/.fleet/inbox/<agent_id>.md`
- approved docs under project `docs/` during PLAN-DOC/TASK-PLAN-DOC

Reads:
- `~/.fleet/projects/<project>/tasks.md`
- `~/.fleet/projects/<project>/coord-config.json`
- worker `state.json` files
- merged standards and learnings via Fleet CLI

Do not write `tasks.md` directly. Use Fleet CLI mutations only.

## Sentinels

Inbox archive lines:

```text
TASK_DONE_PR=<slug> <pr-url>
BLOCKED_QUESTION=<slug> <one-line text>
WORKER_FAILED=<slug> <reason>
NEW_TASK=<slug>
```

Slug mismatch means ignore and log. A sentinel mutates only its own slug.

## Non-git Projects

Same phases, no branch/commit/push/PR. Reviewer records codex as skipped with
reason `no-git`; `/review` remains mandatory. Finisher writes `phase=done`
directly and notes the diff summary.

## Failure Modes

- Parse error: skip tick and report parse error.
- Lock busy: skip tick, no mutation.
- Prompt too large: leave task ready/todo and report error.
- Worker died without PR: requeue and note.
- CI red: requeue/fix-subagent path.
- Rebase conflict requiring business semantics: raise hand.
- Reviewer tries to bypass terminal review fields: workers state validator
  rejects push/done phase.

## Module Map

- `loop.py` — tick driver and dispatch block emission.
- `parse.py` — read-only Python task parser mirror.
- `dispatch.py` — worker/reviewer/finisher prompt builders and inbox writes.
- `workflow_state.py` — atomic `workflow.md` writer.
- `handoff_resume.py` — successor coord resume helper.
- `register_subagent.py` — records host Agent `subagent_id`.
- `remote_control.py`, `supervisor.py`, `reaper.py`, `worktree.py` — runtime
  helpers.

## Tests

```bash
python3 -m pytest skills/coordinator/tests/ -q
```

Use targeted tests while editing this skill:

```bash
python3 -m pytest skills/coordinator/tests/test_skill_md.py -q
go test ./internal/tui -run CoordSpawnPrompt
```

## Hook Bindings

This skill is not bound directly to Codex/Claude hooks. It runs as a slash-skill
or via the coordinator spawn prompt. Fleet-guard hooks may cause future ticks by
resuming the coord session, but `loop.py` stays stateless across invocations.
