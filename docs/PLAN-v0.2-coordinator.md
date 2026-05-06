# Fleet v0.2 — Per-Project Coordinator

## Context

v0.1 (Week 5 release) ships fleet-guard auto-handoff and the dispatch / status / attach / handoff CLI. The pattern works: agents stay healthy, handoff at context limits, operator drives via TUI.

What's missing is the **layer above**: an autonomous coordinator that owns a task list, dispatches workers, monitors PR/CI, and raises hand to the operator only when human input is needed. Current workflow forces the operator to hand-pick every task and drive every dispatch.

This plan adds a per-project coordinator pattern on top of the existing v0.1 primitives. Borrows from Ralph (markdown-as-state, stateless reentry, eventual consistency, no worker-to-worker comms) and Superpowers (TDD enforcement, two-stage review, self-contained worker prompts) — but keeps Fleet's single-binary, filesystem-state philosophy.

Three project-scoped docs (tasks, learnings, standards) replace one inflated tasks file. Standards merges from global + per-project. Workers auto-append learnings. Coordinator dispatches 1 worker by default, scales to 3 when tasks are independent (worktree-isolated).

**Scope:** v0.2 milestone, lands AFTER v0.1 ships and is dogfooded for one week (per Week 6 plan in CLAUDE.md). Pure-skill where possible, minimal Go additions.

---

## Architecture comparison: per-project vs global single coord

You answered "2–3 active concurrently". Both models are viable at that scale; here's a concrete side-by-side so you can pick.

### Option A — Per-project (recommended)

State:
```
~/.fleet/
  standards.md                       # global bar
  agents/<id>.json
  inbox/<coord-id>.md                # one per coord (small)
  queue/, handoffs/                  # existing
  projects/<name>/
    tasks.md                         # per-project task list
    tasks-archive.md
    standards.md                     # per-project override
    learnings.md                     # per-project learnings (worker auto-appends)
    worktrees/<slug>/                # if parallelism cap > 1
    .locks/coordinator.lock          # one coord per project
    .locks/tasks.lock                # serializes tasks.md writes
```

Loop reads ONE project's state per tick:
```
acquire projects/<name>/.locks/coordinator.lock (NB)
tasks = read(projects/<name>/tasks.md)
standards = merge(~/.fleet/standards.md, projects/<name>/standards.md)
learnings = read(projects/<name>/learnings.md)
# ... reconcile, dispatch, sleep ...
```

Worker prompt assembly: trivial. One project = one set of standards/learnings.

### Option B — Global single coord

State:
```
~/.fleet/
  standards.md                       # global bar
  learnings.md                       # cross-project learnings
  registry.md                        # NEW — active project list (sketch below)
  coordinator.lock                   # NEW — exactly one global coord
  inbox/<global-coord-id>.md         # ALL worker reports flow here
  agents/<id>.json
  queue/, handoffs/                  # existing
  projects/<name>/
    tasks.md                         # per-project task list (unchanged location)
    standards.md                     # per-project override (unchanged)
    learnings.md                     # per-project learnings
    worktrees/<slug>/
    .locks/tasks.lock                # per-project tasks-write serialization
```

`registry.md` (NEW for global):
```markdown
# Active projects

- name: fleet
  cwd: /Users/pinkbear/projects/fleet
  added: 2026-04-01T10:00:00Z
  active: yes
- name: g-stack
  cwd: /Users/pinkbear/projects/gstack
  added: 2026-05-02T14:00:00Z
  active: yes
- name: side-experiment
  cwd: /Users/pinkbear/projects/side
  added: 2026-05-05T09:00:00Z
  active: no                          # registered but coord skips
```

Loop reads ALL active projects per tick:
```
acquire ~/.fleet/coordinator.lock (NB)
registry = parse(~/.fleet/registry.md)
work_view = []
for project in registry where active:
  tasks = read(projects/<project>/tasks.md)
  work_view += tag_each(tasks, project)        # so demux works later

# Cross-project priority decision:
candidate = pick_top(work_view, by=(priority, depends_on, age))

# Worker dispatch — MUST demux per project to avoid standards leak:
standards = merge(~/.fleet/standards.md, projects/<candidate.project>/standards.md)
learnings = read(projects/<candidate.project>/learnings.md)
prompt = build(candidate, standards, learnings)
fleet dispatch ... --cwd <candidate.project_cwd>

# Inbox demux:
process(~/.fleet/inbox/<global-coord>.md)
  for each line: parse task_slug → look up project → route
```

### Side-by-side tradeoffs

| Dimension | Per-project | Global |
|---|---|---|
| State read per tick | 1 project (~5–10K tokens) | N projects (~20–40K tokens at N=3) |
| fleet-guard handoff frequency | Every few hours | Every ~1–2 hours |
| Worker prompt assembly | One function, one project context | Demux required; standards-leak risk; +100 LOC and tests |
| Inbox routing | Trivial (one inbox per coord) | All workers → one inbox; demux by task slug; +50 LOC |
| Cross-project priority | N/A (projects independent) | Coord must rank P0-in-A vs P1-in-B; opinionated |
| Operator UX | N tmux rows; `fleet attach fleet-coord-<proj>` | 1 tmux row; `fleet attach fleet-coord`; operator says "switch to <proj>" |
| Conversation thread | One per project (clean separation) | One global (operator must keep project in mind) |
| Cross-project reasoning | None (manual via operator) | Possible — same bug in 2 repos can be deduplicated |
| Coord crash blast radius | One project pauses | ALL projects pause |
| Handoff bootstrap cost | Read 1 project's state | Read N projects' state |
| Idle cost | N coords × ~2 ticks/hr (mitigated by auto-idle-stop) | 1 coord × ~2 ticks/hr |
| Net new Go LOC | ~600 | ~800 (+200 for registry, demux, cross-project priority) |
| Net new skill complexity | Low (one project per coord) | Medium (project demux logic) |

### Recommendation: Option A + auto-idle-stop

Reasons given your 2–3 active answer:

1. **Auto-idle-stop neutralizes global's main win.** Coord exits cleanly after 4h with zero active tasks; next `fleet tasks add` re-spawns it. So at 2–3 active you have 2–3 coords running, not 10. Idle cost is small.
2. **Context isolation is a real win at any scale > 1.** Per-project coord stays under 50% threshold for many hours; global hits it in 1–2. Each global handoff costs more because the replacement must rebuild N projects' state.
3. **Worker-prompt safety.** Standards-leak across projects in global is a real risk needing test coverage. Per-project removes the failure mode entirely.
4. **Failure isolation.** One project's coord crash doesn't pause your other work.
5. **v0.3 can add a supervisor view** (`fleet status --all`, cross-project aggregation) on top of per-project without rewriting state. The reverse — going from global to per-project later — would require splitting `registry.md` and the global inbox, which is invasive.

Global wins one thing only: cross-project reasoning. At 2–3 active that's rare and the operator can drive it manually.

**Decision below assumes Option A. If you pick Option B instead, the State layout / Worker prompt / Code sections need ~+200 LOC and the tests above; flag that and I'll rewrite.**

---

## State layout

All per-project state under `~/.fleet/projects/<safe-name>/`. Nothing in the repo. Operator-private.

```
~/.fleet/
  standards.md                       # global bar (operator-edited)
  projects/<name>/
    tasks.md                         # task registry (Ralph-style markdown)
    tasks-archive.md                 # done/abandoned tasks
    standards.md                     # per-project override (optional)
    learnings.md                     # shared experience log (worker auto-appends)
    workers/<slug>/                  # active workers — see "Workers" section
      state.json                     # phase + timestamps, atomically updated
      output.log                     # captured stdout/stderr of `claude --print`
    workers/archive/<slug>-<ts>/     # archived on done, auto-pruned at 7d
    .locks/coordinator.lock          # NEW — one coordinator per project
    .locks/state.lock                # NEW — serializes tasks/learnings/standards writes (Q1)
  agents/<id>.json                   # existing — coords only; workers are NOT agents
  inbox/<id>.md                      # existing — operator ↔ coord; workers don't use
  queue/spawn-fresh-<id>.json        # existing
  handoffs/<id>-<ts>-<rand>.md       # existing
  projects/.locks/<safe-name>.lock   # existing per-project handoff flock
```

Reuses `state.ProjectLockPath` (`internal/state/state.go:186`) for handoff serialization. Adds new helpers `state.ProjectDir(name)`, `state.WorkerDir(project, slug)`, and `state.WorkerArchiveDir(project, slug, ts)`.

**Workers are NOT Fleet agents.** They run as `claude --print` subprocesses launched by the coord, write their own state files, and exit. No tmux session, no agent record, no fleet-guard hook. The coord watches `workers/<slug>/state.json` (via fsnotify since coord IS a long-lived process for the duration of its tick cadence) to know when each worker advances phases.

---

## Three docs, three lifecycles

### `tasks.md` — task registry (Ralph-style markdown)

Each task block:

```markdown
## task: <slug>

- status: todo | ready | in-progress | in-review | done | blocked | abandoned
- priority: P0 | P1 | P2 | P3
- worker_pid: <int> | null      # PID of `claude --print` subprocess; null when no worker active
- worktree: <path> | null
- pr_url: <url> | null
- branch: <name> | null
- created: <RFC3339>
- updated: <RFC3339>
- depends_on: [<slug>, ...]
- spawned_by: user | <agent-slug>  # agent-slug = the task slug of the worker that filed this task

### Spec
[free-form body]

### Acceptance
[criteria]

### Notes
[append-only worker notes]
```

Slug format: `<short-desc>-<4hex>`. Stable identifier even if title text edits.

**Slug input rule (decided 2026-05-06):**
- If user passes `--slug <short-desc>` to `fleet tasks add`, CLI appends `-<4hex>` for uniqueness → final slug is `<short-desc>-<4hex>`.
- If user passes a full slug (already matching `<short-desc>-<4hex>`), CLI uses it as-is (must still be unique; collision → error).
- If `--slug` is omitted, CLI auto-generates `<short-desc>` from the spec body's first line (kebab-cased, ≤24 chars), then appends `-<4hex>`.
- Workers (using `fleet tasks add --spawned-by <agent-id>`) follow the same rule — they can supply `--slug` if they have a clear name, otherwise it's auto-generated. Same goes for the coord agent dispatching internal tasks.

This means: short, memorable slugs when humans/agents care; auto-gen when they don't. The trailing 4hex is always present, always system-generated, always guarantees uniqueness.

### `learnings.md` — shared experience log

Append-only. Workers auto-write when they hit something non-obvious; operator prunes via CLI.

```markdown
## 2026-05-06T14:22:00Z · agent:91f0a2c4 · task:fix-flaky-handoff-test-7a3c · tag:testing

`go test -count=N` passes locally but fails on CI when N>=10 because tmpdir
cleanup races with file handles still held by goroutines. Use t.TempDir()
not os.MkdirTemp + manual cleanup — the testing framework defers properly.

## 2026-05-06T15:01:00Z · operator · tag:review
Always run `golangci-lint run --new-from-rev=main` before pushing — CI runs
the same flag and yells about pre-existing issues otherwise.
```

Each entry: H2 header `## <RFC3339> · <author> · <task or "operator"> · tag:<topic>`. Body is free markdown.

### `standards.md` — the bar

Operator-edited. Global at `~/.fleet/standards.md`, per-project at `~/.fleet/projects/<name>/standards.md`. Worker prompt assembly merges them (global first, then project — project overrides on conflict).

```markdown
# Standards

## Testing
- TDD: failing test on disk before implementation. Honor system in v0.2.
- Tests use stdlib `testing` only. No testify, no ginkgo.
- All bug fixes require a regression test that fails on the parent commit.

## Code review (before push)
- Run /review skill — fix every P0/P1.
- Run /codex review skill — fix every P0/P1.
- Both must report clean OR every flag has a "wontfix" rationale in PR body.

## Commits
- Conventional: `fix(handoff): ...` / `feat(tasks): ...`. Lowercase scope.
- One PR per business unit (multiple commits allowed inside).
- Never amend; always create new commits after hook failures.

## PRs
- Title under 70 chars.
- Body has Summary + Test plan checklist.
- Link issue/task slug in description.
```

Workers receive the merged result inlined in their prompt. Standards is a contract — violations should fail self-review.

---

## Coordinator skill

`skills/coordinator/SKILL.md` is the heart. Coordinator runs as a normal Fleet agent (dispatched via existing `fleet dispatch`), so fleet-guard handles its own context handoffs for free.

### Loop algorithm (one tick per Stop hook fire)

```
acquire ~/.fleet/projects/<name>/.locks/coordinator.lock (LOCK_EX|LOCK_NB)
  if held by another PID: log + exit cleanly

tasks = read_and_parse(tasks.md)
live_agents = agent.List() filtered to project == <name>

# 1. Reconcile in-flight workers
for task in tasks where status in {in-progress, in-review}:
  worker_state = read_state_file(~/.fleet/projects/<name>/workers/<task.slug>/state.json)
  if worker_state == None or worker.process_dead and worker_state.phase != "done":
    if task.pr_url:
      ci = `gh pr checks <num> --json state,conclusion,mergeable`
      if all green and merged:           task.status = done
      elif all green and not merged:     raise_to_user(task, "CI green, ready to merge")
      elif not mergeable:                task.status = todo; clear worker; note "rebase needed"
      elif failed:                       task.status = todo; clear worker; note "CI red <url>"
                                         raise_to_user(task, "CI red — review or re-spec")
    else:                                # worker died before pushing
      task.status = todo; clear worker; note "worker died without PR"
      cleanup_worktree(task.worktree)
      archive_worker_dir(task.slug)

# 2. React to worker state-file updates (replaces sentinel inbox draining)
for slug in fsnotify_changed(~/.fleet/projects/<name>/workers/*/state.json):
  worker_state = read_state_file(...)
  match worker_state.phase:
    "done"     → task.pr_url = worker_state.pr_url; task.status = "in-review"
                 archive_worker_dir(slug)
    "blocked"  → task.status = "blocked"; raise_to_user(task, worker_state.blocked_reason)
                 archive_worker_dir(slug)
    "push"|"review-codex"|"review-claude"|"tdd-*"|"branch"
              → no task.status change; coord logs the phase advance for `fleet peek`
# Operator → coord messages still flow through inbox/archive/ (Q6), unchanged.

# 3. Dispatch ready tasks under parallelism cap
cap = min(3, user_configured)        # default 1
active = count(tasks where status == in-progress)
candidates = tasks where status == ready and deps_satisfied(t) and (t.spawned_by == user or promoted(t))
sort candidates by priority

for task in candidates:
  if active >= cap: break
  if active >= 1 and would_conflict(task, in_flight_tasks):
    continue                            # files overlap; serialize
  worktree = create_worktree(task) if cap > 1 else repo_root
  prompt = build_worker_prompt(task, standards_merged, learnings_excerpt)
  worker_dir = mkdir(~/.fleet/projects/<name>/workers/<task.slug>/)
  initial_state = {"slug": task.slug, "phase": "starting", "started_at": now(), ...}
  state.WriteAtomic(worker_dir/"state.json", initial_state)
  pid = spawn_subprocess("claude --print", stdin=prompt, cwd=worktree,
                         stdout=worker_dir/"output.log", stderr=...)
  task.worker_pid = pid
  task.worktree, task.status = worktree, "in-progress"
  # NOTE: workers are NOT Fleet agents. The task block in tasks.md keeps
  # worker_pid (alive-check) + worker_slug (= task.slug, for archive lookup).
  # Old `worker_id` field removed from schema.
  active++

write_back(tasks.md)

# 4. Smart sleep (cache TTL discipline)
if any task in-review with CI started < 5min ago:  sleep 270   # tight watch, cache warm
elif any task in-progress or in-review:            sleep 270   # something happening
elif any task blocked:                             sleep 1800  # waiting on human
else:                                                          # idle
  if idle_streak >= 4h:
    log "no active tasks 4h, exiting cleanly"
    release lock
    exit 0                                                      # auto-idle-stop
  sleep 1800
# never sleep 300-1200 — that's the worst-of-both for the prompt cache
# auto-respawn: `fleet tasks add --project <name>` checks for a live coord;
# if none, it spawns one via existing `fleet dispatch` before adding the task.
```

### Conflict detection (for >1 worker)

Cheap heuristic in v0.2: parse each task's spec for `path:`/`file:` mentions and compare. If any pair shares a file, serialize. Anything more accurate is v0.3. The default cap=1 means most operators never hit this.

### Worktree management (only when cap > 1)

Worker workflow when in worktree mode:
- Coordinator: `git worktree add <state.WorktreePath(slug)> -b <branch>`
- Pass `--cwd <worktree>` to `fleet dispatch`
- After PR merged or task abandoned: `git worktree remove --force`

Worktrees live at `~/.fleet/projects/<name>/worktrees/<slug>/`. Cleaned on task archive.

---

## Worker prompt template

Built fresh by coordinator on each dispatch. Self-contained — no inheritance.

```
You are a Fleet worker for task: <slug>
Project: <project>
Coordinator: <coord_agent_id>

## Task

[### Spec body from tasks.md]

## Acceptance

[### Acceptance body]

## Standards (the bar — non-negotiable)

[merged content from global standards.md + per-project standards.md;
 per-project overrides on conflict]

## Relevant prior learnings

[grep learnings.md for entries tagged with topics matching this task —
 most recent 5 entries. Inlined verbatim. If none, omit this section.]

## Required workflow

You are running as a single `claude --print` invocation. You do NOT have a conversation window.
Update your state file at the start of each phase below; coord watches it via fsnotify.

State file: ~/.fleet/projects/<name>/workers/<your_slug>/state.json
Output captured automatically at: ~/.fleet/projects/<name>/workers/<your_slug>/output.log

1. Branch:   git checkout -b <branch>     (in <cwd>; may be a worktree)
             # WRITE state.json: {"phase": "branch", ...}
2. TDD:      failing test → commit → minimal impl → commit → refactor → commit
             # WRITE state.json: {"phase": "tdd-red"} → "tdd-green" → "tdd-refactor"
3. Review:   /review (fix P0/P1) → /codex review (fix P0/P1)
             # WRITE state.json: {"phase": "review-claude"} → "review-codex"
4. Push:     gh pr create
             # WRITE state.json: {"phase": "push", "pr_url": "<url>"}
5. Done:     # WRITE state.json: {"phase": "done", "exit": 0}
             # Then exit 0. Coord sees state.phase == done and archives this dir.
6. Learn:    if anything was non-obvious, append via:
             fleet learnings add --project <name> --tag <topic> \
               --task <your_slug> "<one paragraph: what you learned, why it
               matters, when it applies>"

## Constraints

- Stay on this task. File incidental bugs as new tasks (max 3/session):
    fleet tasks add --project <name> --spawned-by <your_slug> --priority P3 \
      --slug <short> "<one-line spec>"
  Worker-filed tasks need operator promotion before dispatch.
- Do NOT edit tasks.md or standards.md directly. Use `fleet tasks`.
- Learnings: APPEND ONLY. Use `fleet learnings add` (takes the per-project flock).
- Stuck or genuinely confused: WRITE state.json: {"phase": "blocked", "blocked_reason": "<one line>"}, then exit 0.
  Coord raises this to the operator and the operator can resume by clarifying the task spec.

You have: /review, /codex review, gh, git, full repo at <cwd>. No interactive chat — operator can't reply to you mid-flight. Communicate via state.json updates.
```

**Worker → coord contract.** Workers do NOT write inbox sentinels. They write:
- `~/.fleet/projects/<name>/workers/<slug>/state.json` — atomic update on every phase change. Schema: `{slug, project, phase, phases_completed, started_at, updated_at, pid, pr_url, blocked_reason, exit}`.
- `~/.fleet/projects/<name>/workers/<slug>/output.log` — captured stdout/stderr (coord redirects when launching the subprocess).

Coord watches `workers/*/state.json` via fsnotify and reacts when `phase` advances. On `phase == done` (with non-empty `pr_url`), coord transitions task to in-review. On `phase == blocked`, coord raises to operator. On worker process exit without reaching `done`, coord runs the reconcile path and resets the task to todo.

**Operator → coord** still uses the inbox (Q6 mechanism — read inbox/archive/ directly). Workers don't use the inbox at all.

fleet-guard already delivers operator inbox to the coordinator on the next Stop hook — no new plumbing needed (`skills/fleet-guard/inbox.py:32-41`, `main.py:116-124`).

---

## Coordinator-as-Fleet-agent

The coordinator is dispatched via existing `fleet dispatch`:

```
fleet dispatch fleet-coord-<project> \
  --project <project> \
  --cwd <repo-path> \
  --command "claude 'Run the /coordinator skill loop for project <project>.'"
```

Why this beats a standalone daemon:
- **Free handoff at 50%/70%** via fleet-guard. Replacement reads tasks.md fresh; no in-memory state to migrate.
- **Free inbox.** Workers and operator both write to `~/.fleet/inbox/<coord_id>.md`; fleet-guard delivers on Stop.
- **Visible in `fleet status`/TUI** as one row. Existing "asking" state surfaces BLOCKED_QUESTION naturally.
- **Can use skills.** Coordinator runs `/codex review` etc. itself if needed; daemon can't.

Cost: idle ticks at 1800s = 2 fires/hr. Each fire is small (parse tasks.md, maybe one `gh pr checks`, sleep). Acceptable.

---

## Code: skill side vs Go side

### New skill (~700 LOC Python, stdlib only — mirrors fleet-guard discipline)

| File | Purpose |
|------|---------|
| `skills/coordinator/SKILL.md` | Frontmatter + invocation rules + the loop in prose + worker prompt template + failure runbook |
| `skills/coordinator/parse.py` | tasks.md parser/writer (regex on `## task:`) |
| `skills/coordinator/loop.py` | One-tick loop driver invoked by skill |
| `skills/coordinator/dispatch.py` | Builds worker prompt, calls `fleet dispatch`, writes inbox |
| `skills/coordinator/conflict.py` | Cheap file-overlap heuristic for parallel dispatch |
| `skills/coordinator/tests/test_parse.py` | pytest |
| `skills/coordinator/tests/test_loop.py` | pytest |
| `skills/coordinator/tests/test_conflict.py` | pytest |

### New Go (~800 LOC + tests, after worker-as-CLI refinement)

| File | Purpose |
|------|---------|
| `cmd/fleet/tasks.go` | `fleet tasks {add,list,show,set,note,archive,promote}` |
| `cmd/fleet/learnings.go` | `fleet learnings {add,list,prune}` |
| `cmd/fleet/standards.go` | `fleet standards {show,edit}` (just opens $EDITOR on the right file) |
| `cmd/fleet/peek.go` | `fleet peek <slug> [--follow] [--logs]` — show worker state.json + tail output.log |
| `cmd/fleet/workers.go` | `fleet workers {list,prune}` — table of active/archived workers |
| `internal/tasks/tasks.go` | Parser/writer for tasks.md, flock + atomic write |
| `internal/learnings/learnings.go` | Append-only writer for learnings.md |
| `internal/standards/standards.go` | Merge logic (global + per-project) |
| `internal/workers/workers.go` | state.json schema + atomic update + alive-check (kill -0) + archive-on-done + 7d prune |

### Modified Go

| File:line | Change |
|-----------|--------|
| `cmd/fleet/main.go:41-48` | Register `tasks`, `learnings`, `standards` subcommands |
| `embed.go` | Add `skills/coordinator/` to embed directives |
| `cmd/fleet/init.go:60-72` | Generalize from "install fleet-guard" to "install all skills/*/"; also seed `~/.fleet/standards.md` from a template if missing |
| `internal/state/state.go:21-33` | Add `state.ProjectDir(name)` and `state.WorktreePath(project, slug)` helpers (~20 LOC) |

### Reused as-is

`fleet dispatch`, fleet-guard skill, queue, inbox, agent records, TUI, per-project flock at `state.ProjectLockPath` (`internal/state/state.go:186`), `state.WriteAtomic` (`internal/state/state.go:266`).

---

## Failure modes

| Failure | Recovery |
|---------|----------|
| Coordinator crashes mid-loop | flock auto-releases on PID death; next dispatch (manual or via TUI re-spawn from queue) re-takes lock; re-parses tasks.md fresh. Eventual consistency. |
| Two coordinators on same project | Second hits `EWOULDBLOCK`, logs + exits cleanly. Operator notices via `fleet status` showing two coord rows; one is no-op. |
| Worker dies before pushing PR | Reconcile detects `worker_id ∉ live_agents AND pr_url == null`. Resets to todo, clears worker, removes worktree, leaves note. Next dispatch picks up. |
| PR has merge conflicts | `gh pr checks --json mergeable` returns false. Status → todo, note "rebase needed". Fresh worker dispatched against current main. (v0.3 adds in-place rebase.) |
| Worker fires recursive bug-files | Worker-filed tasks have `spawned_by: <agent-id>`; coordinator skips them in dispatch loop until operator runs `fleet tasks promote <slug>`. Worker session-cap of 3 is honor-system in worker prompt. |
| User adds task while coordinator writing | Per-project flock at `state.ProjectLockPath` serializes. User blocks briefly on next CLI call. Coordinator's read sees prior or new state — both consistent. |
| User adds task during long sleep | `fleet tasks add` writes `NEW_TASK=<slug>` to coord inbox. Wakes coordinator on next Stop tick. (If coordinator is genuinely idle, ticks are 1800s apart — operator can `fleet poke <coord_id>` for instant wake. Add `fleet poke` if missing.) |
| Coord auto-stopped 4h ago, user adds task | `fleet tasks add` checks for live coord via `agent.List()`. None found → spawns fresh coord via `fleet dispatch fleet-coord-<project> ...` THEN writes the task. Fresh coord reads tasks.md on first tick, picks up the new task. |
| CI red | Reconcile sets task to todo, raises to user (does NOT auto-redispatch). User decides: retry / re-spec / abandon. |
| Worktree cleanup leaks | On task archive, `git worktree remove --force` runs unconditionally. If branch is unmerged, archive prompts confirm. |
| Standards conflict (global vs per-project) | Per-project wins. Documented in `standards.md` template. Worker prompt assembly logs which file each section came from for debugging. |
| Learnings file balloons | `fleet learnings prune --older-than 30d` archives old entries to `learnings-archive.md`. Worker prompt only injects 5 most-relevant by tag — pruning doesn't break anything. |

---

## Out of scope (defer to v0.3+)

- Multiple coordinators per project (different roles)
- Cross-project coordinator
- Auto-retry on CI red with retry counter
- Brainstorm-before-spec interactive skill
- Pre-dispatch design/eng/CEO review chain
- GitHub webhook → inbox push (replaces polling)
- In-place rebase on merge conflicts
- Heartbeat-based stuck-worker detection
- sqlite/Temporal task store
- Web UI / mobile push
- Worker-to-worker communication (intentionally never)

---

## Verification

End-to-end test plan once implemented:

1. **Fresh setup.**
   ```
   fleet init
   ls ~/.fleet/standards.md           # exists, populated from template
   ```

2. **Per-project bootstrap.**
   ```
   cd /path/to/repo
   fleet tasks add --project myrepo --priority P1 --slug add-readme \
     "Write a README for this repo with build instructions"
   ls ~/.fleet/projects/myrepo/       # tasks.md exists
   fleet tasks list --project myrepo  # shows the task
   ```

3. **Spawn coordinator.**
   ```
   fleet dispatch fleet-coord-myrepo --project myrepo --cwd $(pwd) \
     --command "claude 'Run /coordinator for myrepo.'"
   fleet status                       # shows coordinator row
   ```

4. **Verify single worker dispatch (default cap=1).**
   - Wait one coordinator tick (~270s or `fleet poke <coord_id>`).
   - `fleet status` shows worker row with task=add-readme, mode=execute.
   - `fleet attach <worker_id>` shows worker running TDD → review → push.

5. **Verify PR monitoring.**
   - Worker pushes PR.
   - `cat ~/.fleet/projects/myrepo/tasks.md` shows status=in-review, pr_url populated.
   - Wait CI green; coordinator raises hand via inbox.

6. **Verify learnings auto-append.**
   - `cat ~/.fleet/projects/myrepo/learnings.md` shows entries written by worker after task completion.

7. **Verify standards merge.**
   - Add `~/.fleet/standards.md` with global rule.
   - Add `~/.fleet/projects/myrepo/standards.md` with override.
   - Dispatch new task; `fleet attach <worker>`; verify worker prompt contains merged content.

8. **Verify failure recovery.**
   - Kill a worker mid-task: `tmux kill-session -t fleet-<worker_id>`.
   - Wait coordinator tick. Task in tasks.md should reset to todo, worker_id cleared, note appended.

9. **Verify parallelism cap and worktrees.**
   - Add two file-disjoint tasks. Set cap to 2 (config TBD; flag on `fleet dispatch fleet-coord-...`).
   - Verify both workers dispatched concurrently, each in its own `~/.fleet/projects/myrepo/worktrees/<slug>/`.
   - Add two file-overlapping tasks. Verify only one dispatched (conflict detection serializes).

10. **Verify worker recursion cap.**
    - Make a worker file 4 new tasks (intentionally over the per-session cap of 3).
    - Verify worker prompt's honor-system caps at 3; 4th attempt fails or is logged. (v0.2: honor-system, log only.)
    - Verify worker-filed tasks stay `spawned_by: <agent-id>` and are NOT auto-dispatched until `fleet tasks promote <slug>` runs.

11. **Lint and type checks.**
    ```
    cd /Users/pinkbear/projects/fleet
    go build ./...
    go test ./...
    golangci-lint run ./...
    cd skills/coordinator && python -m pytest tests/
    ```

12. **End-to-end dogfood (Week 6).**
    Use the coordinator to ship at least one real Fleet feature (say, `fleet poke`). Verify the full loop: task added → dispatched → TDD → review → PR → CI green → raise-hand → merged.

---

## Critical files for implementation

- `/Users/pinkbear/projects/fleet/skills/coordinator/SKILL.md`
- `/Users/pinkbear/projects/fleet/skills/coordinator/loop.py`
- `/Users/pinkbear/projects/fleet/skills/coordinator/parse.py`
- `/Users/pinkbear/projects/fleet/skills/coordinator/dispatch.py`
- `/Users/pinkbear/projects/fleet/skills/coordinator/conflict.py`
- `/Users/pinkbear/projects/fleet/cmd/fleet/tasks.go`
- `/Users/pinkbear/projects/fleet/cmd/fleet/learnings.go`
- `/Users/pinkbear/projects/fleet/cmd/fleet/standards.go`
- `/Users/pinkbear/projects/fleet/internal/tasks/tasks.go`
- `/Users/pinkbear/projects/fleet/internal/learnings/learnings.go`
- `/Users/pinkbear/projects/fleet/internal/standards/standards.go`
- `/Users/pinkbear/projects/fleet/internal/state/state.go` (add ProjectDir + WorktreePath helpers)
- `/Users/pinkbear/projects/fleet/cmd/fleet/main.go` (register subcommands)
- `/Users/pinkbear/projects/fleet/cmd/fleet/init.go` (install all skills + seed standards)
- `/Users/pinkbear/projects/fleet/embed.go` (embed coordinator skill)

---

## Design brief (handoff to design agent)

The following surfaces need mockups before TUI/CLI implementation begins. Pass this section to a design subagent (e.g. `/design-shotgun` for variants, `/design-consultation` for a system, or `/plan-design-review` to grade the plan's UX assumptions).

### Surfaces

1. **TUI dashboard** (extends `fleet status` TUI in `internal/tui/`)
   - Coord rows distinct from worker rows (mode `coord`). Show project, todo / in-progress / blocked / in-review counts.
   - Workers visually grouped under their coord (indent or `└─` prefix).
   - Raise-hand state must be loud (color + icon) when coord has BLOCKED_QUESTION or PR awaiting merge.
   - Top summary bar: "3 projects · 2 need attention · 1 PR awaiting CI · 5 workers active".
   - Auto-stopped coords: greyed-out row marked "idle 4h+, respawns on next task".
   - Mockup states needed: quiet / active / demand / idle.

2. **CLI output design** for: `fleet tasks list/add/show`, `fleet learnings list`, `fleet standards show`, `fleet poke`. Status icons (⏳▶👁✓⚠), priority pills (`[P0]` red…), pagination for large lists.

3. **Coordinator conversation** (when operator runs `fleet attach fleet-coord-<project>`). Design as transcripts. Tone: terse, status-first.
   Transcripts needed: first-attach (project intro in ≤4 lines), mid-work attach, raise-hand for BLOCKED_QUESTION, raise-hand for CI-green PR, conversation-driven task add, standards-drift suggestion.

4. **Raise-hand notification** beyond TUI: optional tmux status-line integration, optional terminal bell on first raise per session, `fleet inbox` for cross-project attention digest.

5. **Markdown templates** (user-readable surfaces):
   - `tasks.md` rendered with 8 mixed-status tasks
   - `learnings.md` with 6 entries spanning 2 weeks
   - `standards.md` starter template (global) + typical per-project override

### UX moments to nail

- Glance-and-know in ≤2s
- Raise-hand quality: 1-sentence question, 1-sentence answer
- <10s task-add via CLI; ≤30s in conversation
- Auto-stopped coord doesn't look broken
- Standards visible, workers can't ship without meeting them

### Constraints

- Terminal-only. 80-col min. ASCII / Unicode box-drawing.
- Color via ANSI; respect NO_COLOR.
- Reuse Bubbletea + Lipgloss style system already in `internal/tui/`. Don't introduce a new palette.

### Design priorities (in order)

1. TUI row hierarchy for coord + workers + raise-hand
2. Coordinator conversation transcripts (4 samples)
3. `tasks.md` rendered format
4. CLI output for `tasks list` / `tasks show`
5. `learnings.md` and `standards.md` templates
6. Notification / raise-hand visualization beyond TUI row

---

## Sequencing within v0.2

Build in this order so each step is shippable independently:

1. `internal/tasks/`, `internal/learnings/`, `internal/standards/` + tests (Go primitives)
2. `fleet tasks` / `fleet learnings` / `fleet standards` CLI (operator can use these standalone, even without coordinator skill)
3. `skills/coordinator/` skill + tests (operator can run it manually)
4. `init.go` generalization + embed updates (ships skill in binary)
5. Worktree + parallelism cap (defer until single-worker mode dogfooded)
6. Conflict detection for cap >1 (defer until parallelism is needed)
7. Dogfood week — use coordinator to build the next feature
8. Tag v0.2

Steps 5–6 can land as v0.2.x point releases if v0.2.0 ships single-worker only.
