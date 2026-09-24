---
name: coordinator
description: Per-project coordinator that owns tasks.md, saves approved plan docs, dispatches worker/reviewer subagents on the dominant engine (Claude Code Agent tool or Codex spawn_agent), pushes + opens PRs from the tick, monitors PR/CI, and raises hand only when human input is needed. Mutates task state only through fleet CLI. One coordinator per project is enforced by coordinator.lock.
---

# coordinator

**YOU ARE THE COORDINATOR, NOT A WORKER. NEVER:**
- Edit code files.
- Run implementation tests (`go test`, `pytest`, etc.); workers handle them.
- Implement features inline.
- Run any source-tree mutation except the PLAN-DOC and TASK-PLAN-DOC gates.

Enforced mechanically: fleet-guard's `PreToolUse` hook denies these tool calls
in a `FLEET_ROLE=coord` session and logs each attempt to
`~/.fleet/coord-violations/<id>.jsonl`. A denial is not a hint to retry another
way — it means the work belongs in a task for a worker.

Runtime contract for a Fleet coordinator. Keep this file short: coordinators read
it on startup and after handoff, so every line spends context. Long rationale
lives in `docs/COORDINATOR-WORKFLOW.md`, `docs/PLAN-v0.2-coordinator.md`, and
`docs/ENG-v0.2-coordinator.md`.

## Coord agent role

The agent session running this skill is a **coordinator**, not a worker. It
discusses design, writes approved plan docs, files tasks, dispatches
subagents, and shepherds PRs. It does not implement features inline.

### Dominant engine

The operator picks the engine with `fleet -claude` / `fleet -codex` /
`fleet --engine <name>` (default `claude-code`; persisted per project in
`coord-config.json`). That engine is the **dominant** engine: it runs the
coord, every worker/reviewer/finisher subagent, and the review anchor.
`FLEET_ENGINE` tells you which one you are:

| `FLEET_ENGINE` | you are      | spawn tool            | skill home   |
|----------------|--------------|-----------------------|--------------|
| `claude-code`  | Claude Code  | `Agent(...)`          | `~/.claude`  |
| `codex`        | Codex        | `spawn_agent(...)`    | `~/.agents`  |

The *other* provider is an optional **helper**: it only ever runs the alpha
review slot, and only when its binary is installed. Fleet must work with a
single subscription in either direction — never shell out to the helper's
binary for anything but that slot, and never assume it exists.

**ROLE — discuss design with the operator, save approved plan docs, file tasks, dispatch workers.**

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

Any `.html` the coord renders at these gates gets `open`ed in the same step so
the human reviewer sees it immediately.

The plan-doc gates are the coordinator's only source-tree mutation exceptions.
If PLAN-DOC save/render fails, the coord raises hand and does **not** proceed to SPLIT.
If TASK-PLAN-DOC save/render fails, the coord leaves that task unpromoted and
does not proceed to IMPLEMENT for it.

## Workflow

Every engagement follows this eight-step order:

```text
1. DISCUSS        plan + engineering detail + tests; operator approval is G2
2. PLAN-DOC       save docs/DESIGN-<topic>.md (+ render & open .html)
3. SPLIT          approved plan -> tasks.md, inline <=10 or planner >10
4. TASK LIST      one-line goal per task; structured state remains in fields
5. TASK-PLAN-DOC  save docs/TASK-PLAN-<slug>.md (+ render & open .html), link, promote
6. IMPLEMENT      worker -> reviewer -> tick finisher; cap=1 by default
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
- Open: after rendering, run `open docs/DESIGN-<kebab-topic>.html` so the human
  reviewer sees it immediately.
- Contents: summary, design decisions, task split, test plan, assumptions, and
  approval timestamp.
- Record: after the save, run
  `fleet checkpoint doc --role authored docs/DESIGN-<kebab-topic>.md` so the
  doc lands in coord-state.json:session_docs and renders in this coord's
  handoff under "Docs (this session)". Best-effort: a failed call only omits
  the doc from the handoff; it never blocks the step.

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
- Open: after rendering, run `open docs/TASK-PLAN-<slug>.html` so the human
  reviewer sees it immediately.
- Contents: parent design doc link, task goal, acceptance criteria,
  expected files/surfaces, a `## Scenario contract` (below), tests removed /
  KEEP, non-goals, dependencies, and approval timestamp.
- Worker visibility: before promotion, either embed the task plan in Spec or
  append its path to Spec/Acceptance, for example:
  `fleet tasks note --project <project> <slug> --section spec "Task plan: docs/TASK-PLAN-<slug>.md"`.
- Record: after the save, run
  `fleet checkpoint doc --role authored docs/TASK-PLAN-<slug>.md` ("Docs
  (this session)" in the handoff).
- Promotion: run `fleet tasks promote <slug>` only after the doc exists and is
  linked or embedded in worker-visible task text.

**Scenario contract (replaces the per-test "test plan" list):**

Every TASK-PLAN doc carries a `## Scenario contract` table — one row per
requirement the worker must prove on the running product, not one row per test
function:

```markdown
## Scenario contract

| S | Requirement | Stand-up | Trigger | Observable outcome | Level | Harness |
|---|-------------|----------|---------|--------------------|-------|---------|
| S1 | WHEN … THE … SHALL … | seeded state | the real command / key / request | screen text, file content, response, exit code | e2e | `up` / `observe` from `## Sandbox` |
```

Rules:
- **Observable outcome** is what the user/operator would see — response body,
  file content, screen/pane text, exit code, DB row, PR state. Never "function X
  is called".
- **Level defaults to the project's max level**, read from the merged
  standards' `## Sandbox` tier (`fleet standards show --merged --project <p>`):
  `tier: local` or `tier: remote` → `e2e` (the running product via the
  project's `up` / `observe` / `down`); `tier: none` → `replay` (the
  highest-fidelity artifact — recorded request, log, fixture — driven through
  the real boundary). `integ` = a package test at a real boundary that needs no
  `up`. `unit` = pure logic the scenario cannot reach. A row below the max
  level carries a one-word reason in the Level cell (`unit — pure`,
  `integ — hermetic`, `replay — no-artifact`), or a reviewer files it as P1.
- **Level never exceeds the tier**: plan review rejects a row whose Level the
  project cannot run (`contract S<n> needs e2e but tier=none`).
- **Harness** names how the row is driven: the `up` / `observe` commands for
  `e2e`, the artifact for `replay`, the package harness for `integ`.
- **Standards onboarding**: if the merged standards lack `## Sandbox` or
  `## Gates`, the task plan carries a "standards onboarding" note and the coord
  raises it to the operator at promote (`fleet standards edit --project <p>`);
  workers cannot reproduce or gate against a project that has not declared how
  it runs.
- **Grouping is decided in the plan**: rows that share a stand-up become one
  table test, one row each; the plan says which rows group.
- **Escape hatch:** a task with nothing observable (pure docs, rename) writes
  `none — <reason>` instead of a table. The worker then records
  `--scenarios-total 0`.
- **Tests removed / KEEP** lists the existing tests the new scenarios subsume
  (delete them) and the ones that stay.

**Task-plan review SOP (operator-approved 2026-06-11, all projects):**

- The coord NEVER reviews inline (no codex exec, no self-review). All review /
  debug / investigation / PR-review work is DISPATCHED to subagents; the coord
  only talks to the operator, dispatches, and enforces return contracts.
- Before promote, every TASK-PLAN doc set gets one dual review via dispatched
  subagents, launched in parallel (dominant engine and helper concurrently):
  1. a helper-engine reviewer (the other provider's CLI, high reasoning) —
     design-fidelity, code-reality, implementability; when no helper is
     installed, a second independent dominant-engine reviewer takes this seat;
  2. an independent dominant-engine reviewer — cross-task seams between the
     plans, testability, plus the same lenses.
- Fan-out: with many task plans, per-plan reviewers also dispatch in parallel;
  only the cross-task-seam pass needs the full plan set in one reviewer's
  context.
- Loop: the coord applies doc-level fixes (plan docs are its only allowed write
  surface) and re-dispatches confirm reviews until BOTH return no P0/P1.
- Reviews-clean never auto-promotes — the operator promote gate remains
  separate.

### Plan & design-doc writing standard

Every PLAN-DOC and TASK-PLAN-DOC is **problem-first and readable by a
non-expert engineer**. The doc is for a human to understand and approve, not a
dump of implementer notes. This standard gates the operator-approval step:

1. **Lead with the PROBLEM in plain English** — what is broken now, the
   concrete symptom, why it matters — before any solution. Define jargon on
   first use; no unexplained symbols or `file:line` citations at the top.
2. **Structure:** Problem -> How it works today -> What goes wrong -> The fix
   -> then a clearly-labeled `Implementation detail (for engineers)` section.
   Push the dense spec (exact mechanics, exit codes, edge-case enumeration)
   into that last section, after the accessible explanation.
3. **ASCII diagrams** — one clean diagram each for today's flow, the failure,
   and the fix. Cut redundant ones.
4. **Short sentences, one idea per paragraph.** Halve length by removing
   redundancy, never by dropping a technical decision.
5. **Scenario contract = one row per requirement** (stand-up / trigger /
   observable outcome / level / harness); Level defaults to the project's
   `## Sandbox` max level and rows sharing a stand-up group into one table
   test. The contract names what the worker reproduces, not which functions
   it unit-tests.
6. **Final version only:** no rev-by-rev review log, no "round 1/2/3" history,
   no "superseded" appendix inside the doc — that lives in the task/PR. Minimal
   header: status / scope / priority / depends-on / PR-base.

Ship `.md` (agent source-of-truth) + rendered `.html` (human review) per the
PLAN-DOC/TASK-PLAN-DOC steps. Readability rewrites NEVER change an agreed
technical decision — preserve every invariant verbatim in meaning.

### Step 6 — IMPLEMENT

Implementation is a three-stage flow: two subagents on the dominant engine,
then a deterministic finisher that runs inside the coord tick (`finisher.py`,
no subagent, no coordinator context):

```text
worker                       reviewer                    finisher (tick)
------                       --------                    ---------------
reproduce -> encode -> verify  -> alpha/beta slot loop    -> push + PR
verification.md (evidence)      contract lens; re-verify    ## Verification = the
local commits                   after any fix; no push      evidence, verbatim
phase=review-pending            phase=review-done           phase=done + pr_url
```

Rules:
- The worker runs the scenario-first arc (`spec-repro` -> `spec-encode` ->
  `verify`) using only the merged standards' `## Sandbox` and `## Gates`:
  export `FLEET_WORKER_SLUG=<slug>`, `eval "$(<up>)"`, run each e2e row's
  trigger, capture with `<observe>`, `<down>` (replay rows: obtain the named
  artifact) and record the wrong outcome verbatim; encode one test per row at
  the row's level in the project's own framework, failing for that reason; fix,
  re-run by hand and via the test, run every `## Gates` line in order. Evidence
  lives in `~/.fleet/projects/<project>/workers/<slug>/verification.md` with
  sections `## Scenario contract`, `## Evidence — before`, `## Evidence — after`,
  `## Gates`, `## Baseline`, `## Unit tests`. Before `review-pending` it records
  `--scenarios-total M --scenarios-verified N --gates-status passed`. A red gate
  of its own is fixed, never handed off; a red it believes pre-existing is
  re-run on untouched `origin/main` with the standards' `baseline:` command and
  both results go under `## Baseline`. A row above the tier, a missing artifact,
  or an outcome that is not the wrong one blocks with `<S<n>>: <what you saw>`;
  the worker never mocks a boundary `## Sandbox` can run.
- The worker exits at `review-pending`; it does not run `/review` and does not
  push.
- The reviewer runs both resolved slots concurrently with one
  `review_slot.py --both` call (per-slot `exit` in the JSON report) with the
  Scenario-contract lens (every row has a test at its stated level asserting the
  observable outcome; `## Evidence — before` proves it would have failed;
  a missing row, a row below the `## Sandbox` max level without the plan's
  reason, or a mock for a boundary `## Sandbox` can run is P1), fixes P0/P1
  findings, and after any fix re-runs the touched scenarios plus every
  `## Gates` line and appends `## Review re-verification` to verification.md
  before the terminal write. It
  records slot-named alpha/beta terminal fields. Beta is the
  **dominant-engine anchor** and must pass; the beta review is
  NEVER skippable (a rate-limited/unavailable anchor is `blocked`, not skipped).
- Alpha is the optional helper slot (the other provider) when its binary is
  installed; it may be recorded as skipped only for `rate-limited` or
  `unavailable`. Without a helper, alpha is a second dominant-engine model,
  or `single-engine-degraded` when only one model is available.
- Non-git projects: Claude slots run a raw structured review; Codex slots run
  `codex exec --output-schema` (there is no diff base for `codex review`).
  `review_slot.py` picks this per slot; the reviewer never calls engines
  directly.
- The finisher runs in the tick on `phase=review-done` (and again on
  `phase=push` if an earlier tick died mid-finish): `--phase push` (the
  worker state validator gates on review terminal fields), `git push -u` from
  the worker's worktree (`--force-with-lease` only on a rejected push), then
  `gh pr create` — or `gh pr edit` on an already-open PR for the branch — with
  `## Summary` / `## Review` / `## Verification` = verification.md verbatim,
  then `--phase done --pr-url`. Non-git projects skip push/PR: the same
  `## Verification` block is written as a task note (once, marker-guarded)
  before `--phase done`. A missing file or an empty `## Gates` /
  `## Evidence — after` blocks with `finisher: no verification evidence
  (<path>)` — no push, no PR, no done. Any push / gh failure (including a
  timeout or missing binary) blocks with the error inlined; the outcome lands
  as a task note.
- Default parallelism is 3. Resolution order: the project's
  `coord-config.json:parallelism`, then `~/.fleet/coord-config.json:parallelism`
  (what `fleet init` prompts for / `fleet init --parallelism N` writes), then 3.
  Parallelism above 1 uses worktrees and conflict checks; set `parallelism: 1`
  for serialized in-place dispatch.
- `coord-config.json:review_effort` (`low|medium|high`, default `high`;
  project file, then `~/.fleet/coord-config.json`) sets the reasoning effort
  passed to both review slots. Lower it on projects with small diffs where
  review wall-time matters more than depth.
- Worker model routing: workers/reviewers/PR-watch fixers run on a cheaper
  model than the coord. The tick picks a tier per task and stamps
  `tier/model/effort/fallback_models` on the DISPATCH block:
  - `simple` (typo, rename, docs-only, lint, help text) → codex
    `gpt-5.6-luna` medium; `coding` (default) → `gpt-5.6-terra` medium;
    `complex` (migration, protocol, schema, concurrency, cross-cutting, P0,
    many deps / e2e rows, plan-doc linked) → `gpt-5.6-sol` high.
  - Claude coords route every tier to `claude-opus-5-5` medium (fallback
    `claude-opus-5`). Codex falls back to `gpt-5.5` then `gpt-5.4`, medium.
  - Explicit beats inferred: set `complexity:` when you file the task
    (`fleet tasks add --complexity simple|coding|complex`, or
    `fleet tasks set <slug> complexity=<tier>`); unset → inferred from the
    spec/acceptance text. Each re-dispatch of a slug (worker-failed, review
    rejection) escalates one tier, capped at `complex`. PR-watch rebase is
    always `simple`; PR-watch fix/rederive is `coding`.
  - `coord-config.json:unavailable_models` (array of model ids; project file
    unioned with `~/.fleet/coord-config.json`) skips models your account
    can't use; when the whole ladder is unavailable the block carries no
    `model:` line and the worker inherits the coord's model.
- `coord-config.json:worktree_timeout_s` bounds `git worktree add` (default
  300s, clamped to 5..3600). Raise it on repos whose full checkout takes longer
  than that — a timeout kills the add mid-checkout.
- Record: when dispatching a worker for a task, run
  `fleet checkpoint doc --role implementing docs/TASK-PLAN-<slug>.md` so the
  handoff's "Docs (this session)" shows what this coord is actively
  implementing (dedupe by path — the role flips from `authored` to
  `implementing`).

### Decision log

Key Decisions in the handoff is fed **automatically** — you do not have to
remember a logging step. Three producers write the durable per-coord
`session_decisions` buffer in `coord-state.json` (cap 50, stamped with your
agent id, deduped by text; tick noise never evicts it):

1. **Your own state changes.** `fleet tasks set <slug> status=… / priority=… /
   parked=…` and `fleet tasks promote` from your shell self-record one line
   (`set foo status ready → in-progress`, `promoted foo todo → ready`). No-op
   sets and bookkeeping fields (`pr_url`, `worker_pid`, …) are skipped.
2. **The tick's material transitions.** Reconciled → in-review, worker
   finished, requeued worker-failed, parked blocked-question, raised hand.
   Dispatches stay in the rolling `recent_decisions` only — Active Subagents /
   Status already narrate them.
3. **Operator conversation.** fleet-guard's Stop hook records the last
   operator prompt and your final reply as one line
   (`operator: defer cache, do auth first → coord: parked cache-1234, …`) —
   once per operator turn. A decision that lives only in chat still reaches
   the successor.

`fleet checkpoint decision "<what> — <why>"` remains for a rationale none of
the above can infer (e.g. `"Stopped rebase of PR #224 — superseded PR for an
operator-paused task"`); it writes the same buffer plus the rolling one.

The handoff doc's `## Status` block (task → status, PR state from
`pr-watches.json`, next job; then one `Next job:` line) is derived from
`tasks.md` + `coord-state.json` — keep `fleet tasks set` / `fleet checkpoint
next-step` current and the successor's first screen is accurate without any
extra work at handoff time. The same per-task line lands in each in-flight
task's `notes` in `tasks.md` at handoff.

### Context-window handoff (fleet-guard)

At 40% context fleet-guard injects `HANDOFF REQUESTED … SOFT coordinator
handoff`. From that turn the tick emits **no new worker or PR-watch fixer
DISPATCH blocks** (`result.errors` says `handoff pending`); reviewer/finisher
handoffs, reconcile, and PR-watch tracking keep running so in-flight work
lands. Do not start anything new — keep ticking as subagents return, record
what a successor needs (`fleet checkpoint next-step` / `decision`), then emit
`MILESTONE` on its own line. The doc is written and this coord retired only
once `worker_agent_ids` and the PR-watch running leases are empty; at 50% the
handoff is forced with whatever is still in flight listed for the successor.

### Step 7 — PR-TRACK

Shepherd every PR you own. Watch for terminal close/merge, CI failure, BEHIND,
DIRTY, and CHANGES_REQUESTED.

**The durable PR-watch is now the source of truth, not a hand-armed shell loop.**
Every tick derives a watch for each owned, non-terminal task with a `pr_url`
(state on disk at `~/.fleet/projects/<project>/pr-watches.json`, keyed by PR
number) and probes it — so a PR you own can never go unwatched across
compaction / handoff / restart, and "CI green" never ends a watch (only MERGED
or CLOSED does). The tick:
- on **MERGED**: flips ALL backing tasks `done` and prunes the watch (worktree
  reap rides the existing gc backstop);
- on **CLOSED without merge / orphaned PR / definitive 404**: raises hand;
- on **STALE (head not up-to-date under strict protection) / BEHIND / DIRTY /
  CI-fail / CHANGES_REQUESTED**: surfaces the event in the tick result.

A background `until ...; do sleep 30; done` loop may still be used **only** as an
optional wake-accelerator that triggers a tick sooner — correctness never
depends on it (the next tick re-derives the watch regardless).

Actions (PR2 will auto-dispatch these; until then act on the surfaced event):
- CI red: dispatch a fix-subagent on the same branch.
- BEHIND/DIRTY/STALE: dispatch a rebase-subagent in an isolated worktree.
- Substantive conflict or design feedback: raise hand.
- Merged: Step 8 (the watch already flipped the task `done`).
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

`loop.py` cannot invoke the host engine's spawn tool. It emits DISPATCH blocks
and the coord agent must act on them immediately.

Block shape:

```text
DISPATCH: <slug>
  agent_id: <8hex>
  generation: <int>
  description: <short>
  prompt_file: <abs path>
  run_in_background: true
  subagent_type: general-purpose
  engine: claude-code|codex
  tier: simple|coding|complex
  model: <model id>
  effort: low|medium|high
  fallback_models: <id>, <id>
END_DISPATCH
```

`engine` is the dominant engine (same as your `FLEET_ENGINE`). It selects the
spawn tool in step 3; the worker inherits it through its environment.

`tier`/`model`/`effort`/`fallback_models` are optional (see "Worker model
routing" above). When present, pass `model` + `effort` to the spawn call in
step 3. If the spawn rejects the model as unknown/unavailable, retry ONCE per
entry in `fallback_models` (left to right, same `effort`), then add the
rejected ids to `coord-config.json:unavailable_models` so the next tick skips
them. When absent, spawn with no model override — the worker inherits yours.

For each block:
1. Read `prompt_file`. Note the block's `agent_id` and `generation` (the
   launch token).
2. **Durably record the launch attempt BEFORE invoking the Agent** — the
   tri-state CAS that closes the broken-stdout phantom (dispatch-durability
   #184). Run:

   ```bash
   fleet claims mark-launch-attempted <agent_id> <generation>
   ```

   Parse the JSON `outcome` field and branch on ALL THREE results — do NOT
   collapse them to "nonzero → skip" (that silently drops a launch):
   - **`ok`** (exit 0) → the journal flipped `pending → launch_attempted`;
     **proceed to step 3 and launch the Agent.**
   - **`predicate_fail`** (exit 20) → the entry is not pending, or the
     generation is stale (another tick/path already owns this launch, or
     this is a stale re-emitted block) → **SKIP this block; do NOT launch.**
   - **`contention`** (exit 21) → the per-id flock could not be taken in
     time. **TRANSIENT** → **do NOT launch, do NOT mark it done; the next
     tick re-emits the same block. NEVER treat contention as a skip.**
3. Spawn the subagent ONCE, with the tool for the block's `engine`:
   - `engine: claude-code` — invoke the Agent tool. Use `description`, full
     prompt body, `subagent_type=general-purpose`, `run_in_background=true`,
     and the tool's model parameter set to `model` when the block carries one.
   - `engine: codex` — invoke `spawn_agent` with `task_name=<slug>` and
     `message=<full prompt body>`; when the block carries `model`/`effort`,
     pass them through the tool's model and reasoning-effort parameters. It
     is non-blocking and returns a task
     handle (`{"task_name": "/root/<slug>"}`); do NOT `wait_agent` on it in
     the same turn — the supervisor loop tracks the worker through its
     Fleet agent record. `send_message` is for follow-ups only.
4. Capture the returned handle (`subagent_id` for Claude, the `task_name`
   path for Codex) and best-effort register it (this also flips the journal
   `launch_attempted → acked`):

```bash
python3 /path/to/skills/coordinator/register_subagent.py \
  --project <project> <slug> <subagent_id>
```

   **EXCEPTION — `register: false` blocks.** A DISPATCH block carrying a
   `register: false` line is a PR-watch auto-fix/rebase dispatch whose
   `slug` is a synthetic `pr-fix-<n>` / `pr-rebase-<n>` label, NOT a
   tasks.md worker. Do the `mark-launch-attempted` gate + the spawn call as
   normal, but SKIP `register_subagent.py` for it: that script keys on the
   worker slug→agent_id map and would pollute worker state with a non-worker
   label. The coordinator tick reaps these journals/inboxes itself via the
   PR-watch lease lifecycle.

One spawn call per DISPATCH block whose `mark-launch-attempted` returned
`ok`. If a tick emits N blocks, run the step-2 gate then the spawn call for
each before doing anything else. Skip registration only if no `subagent_id`
is available or the brief register call hits lock contention; the worker
still runs (the residual-crash repair handles a never-acked launch, and
replay never re-emits `launch_attempted`, so a missed ack can't
double-launch).

**Replayed blocks** (description ends `(replay)`) are re-emissions of a
dispatch that was recorded but whose launch block never reached the coord
(the broken-stdout incident). Treat them identically — the `generation`
token + `mark-launch-attempted` gate guarantee at-most-once launch even if
a stale block and a replay block both arrive.

- `FLEET_AGENT_ID` — coord's 8-hex ID. Without it the skill exits silently (fleet-guard discipline).
- `FLEET_HOME` — defaults to `~/.fleet/`. Override for sandboxed tests.
- `FLEET_PROJECT` — set by the dispatch path; falls back to argv[0] when invoked manually.
- `FLEET_RC_BOOTSTRAP_DISABLED` — test-hygiene env-gate: when set to any non-empty value, the Go attach-flag helpers never bake `--remote-control` into a spawn argv. Set by `skills/coordinator/tests/conftest.py` (and the Go suites' TestMain) so test runs never produce a flagged argv. The coord skill itself no longer invokes any RC bootstrap (native model below).

## Remote control (native, default-on)

Remote control is Claude-Code-only in v1 (a codex-dominant coord has no RC; the flag is engine-gated at every inject site). Remote control is NATIVE: `fleet dispatch --coord-spawn` (and the handoff / drain replacement paths) bake `claude --remote-control "fleet-coord-<id>-<project>"` into the coord's own claude argv, so mobile / claude.ai pairing is live the moment the coord starts. There is NO standalone `claude remote-control` listener daemon, NO per-tick respawn (`remote_control.spawn_daemon_if_needed` is a retired no-op shim), and NO send-keys injection. The gate is opt-OUT: the per-project `~/.fleet/projects/<p>/rc-disabled` marker (written by `fleet rc down`) suppresses the flag on the next coord spawn.

Workers and in-session subagents NEVER carry the flag — every inject site is gated on coord-ness (`--coord-spawn` / coord-spawn marker). That call-site carve-out is the architectural fix that retires the 5,620-mobile-push reviewer-loop hazard: there is no listener to respawn and no path that attaches RC to a reviewer loop.

Operator commands:
- `fleet rc up <project>` — re-enable: remove the rc-disabled opt-out marker (takes effect on next coord spawn).
- `fleet rc down <project>` — disable: write the opt-out marker + reap any legacy (pre-native) listener. A LIVE coord keeps its RC session until exit/handoff — `fleet handoff <coord-id>` respawns it without RC.
- `fleet rc connect <project>` — DEPRECATED no-op (native startup replaced the send-keys attach).
- `fleet rc status [<project>] [--healthy]` — observability; enabled = no opt-out marker.
- `fleet rc list` — enumerate projects with RC DISABLED (the exceptions).
- `fleet rc reset [<project>]` — emergency: reap legacy listener state + corrupt rc files (opt-out markers preserved).

## Resume after handoff

On first turn after coordinator handoff:

1. Read the handoff doc named in `record.last_handoff_path` for this coord's
   own agent record. If a spawn prompt also names a doc path, it should match
   this record field; the record is the durable source of truth. Follow
   `previous_handoff` links only when needed.
2. Run:

```bash
python3 /path/to/skills/coordinator/handoff_resume.py <handoff-doc-path>
```

3. This helper must run to completion. Its exit 0 writes
   `coord-state.json:resumed_handoff_path`, which is the applied ack the
   `fleet coord-run` supervisor reads to stop resume nudges.
4. For every DISPATCH block it emits, use the Worker dispatch protocol above.
5. The helper reads `Active Subagents`, checks WIP files, rewrites inbox prompts
   with a resume preamble, and skips entries already `in-review` or terminal.
6. For open PR hints, respawn PR shepherd waits on the next supervisor tick.

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
TASK_DONE_PR=<slug> [gen=<n>] <pr-url>
BLOCKED_QUESTION=<slug> [gen=<n>] <one-line text>
WORKER_FAILED=<slug> [gen=<n>] <reason>
NEW_TASK=<slug>
```

Slug mismatch means ignore and log. A sentinel mutates only its own slug.

State-mutating sentinels (TASK_DONE_PR / BLOCKED_QUESTION / WORKER_FAILED) SHOULD
carry the dispatch generation as `gen=<n>` immediately after the slug, where `<n>`
is the worker's `--dispatch-generation` value (the coord-owned per-slug fence
token). The coord corroborates `gen` against the slug's current task-row
`dispatch_generation` and SKIPS all terminal side effects on a mismatch — so a
stale prior-attempt sentinel can never reap a re-dispatched slug's live worktree
(DESIGN-coord-worktree-lifecycle §3). A sentinel that omits `gen=` is treated as a
pre-migration tokenless signal: trusted only while the slug has not been
re-dispatched. `NEW_TASK` is a wake-only sentinel and carries no token.

## Non-git Projects

Same phases, no branch/commit/push/PR. Reviewer runs both resolved slots
through `review_slot.py` as raw structured reviews (a Codex helper is not
offered here — `codex review` needs a diff base; a Claude helper still is),
records both slots passed (or `single-engine-degraded`), and the finisher
writes `phase=done` directly with a diff summary.

## Failure Modes

- Parse error: skip tick and report parse error.
- Lease fenced (`fleet lease-check` exit 3): skip the tick — no mutation, no
  dispatch — emit a loud stderr diagnostic, and STAY ALIVE (tick reason
  `lease-fenced`). A fence verdict NEVER kills the session
  (DESIGN-coord-lease-false-fence-prevention): the tick's `lease-check
  --reacquire` renews our own expired ACTIVE (rival-free) lease in place at
  the same epoch, so a tick fence means a rival takeover is live, an
  abandoned takeover awaits a successor, or a transient — the next tick
  re-checks. (Non-tick lease-check callers are read-only and may fence on
  a bare expiry; only the tick renews.) Session teardown of a genuinely
  superseded coord belongs to handoff/drain/gc, never to the fence path.
  The duplicate-coord lock-busy self-exit below is a SEPARATE route and
  stays.
- Lock busy: skip tick, no mutation. EXCEPTION (coord-self-exit-when-it-6014):
  if a *different live* coord holds `coordinator.lock` AND the coordinator LEASE
  (`fleet coord-owner`) does not name this session — neither the live active owner
  nor an in-flight handoff successor — (i.e. we are not the project's intended/
  successor coord), this session is a duplicate that would otherwise idle forever. It
  emits a stderr diagnostic and `main()` runs `tmux kill-session -t fleet-<coord_id>`
  to tear down its own session, self-healing to one coord. The lock holder and the
  intended successor coord (the lease names them) never self-exit.
- Prompt too large: leave task ready/todo and report error.
- Worker died without PR: requeue and note.
- CI red: requeue/fix-subagent path.
- Rebase conflict requiring business semantics: raise hand.
- Reviewer tries to bypass terminal review fields: workers state validator
  rejects push/done phase.

## Module Map

- `loop.py` — tick driver and dispatch block emission.
- `parse.py` — read-only Python task parser mirror.
- `dispatch.py` — worker/reviewer prompt builders and inbox writes.
- `finisher.py` — in-tick push + PR (or non-git done note) on `review-done`.
- `workflow_state.py` — atomic `workflow.md` writer.
- `handoff_resume.py` — successor coord resume helper.
- `register_subagent.py` — records the host engine's subagent handle
  (Claude `subagent_id` / Codex `spawn_agent` task path).
- `reviewcfg.py` — resolves alpha/beta slots from the dominant engine +
  helper availability.
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
