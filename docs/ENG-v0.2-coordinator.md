# Engineering Design: Fleet v0.2 — Per-Project Coordinator

> Implementer's companion to `docs/PLAN-v0.2-coordinator.md`. Where PLAN
> answers *what* and *why*, this doc answers *how the code is organized,
> what runs concurrently with what, and what the tests must prove*.
> Extends `docs/STATE.md` additively; v0.1 invariants (A1–A5, F1–F5) hold.

## 1. Scope & relationship to existing docs

| Doc | Owns |
|-----|------|
| `DESIGN.md` | Product thesis, reliability invariants, big-bang launch shape. |
| `PLAN-v0.2-coordinator.md` | v0.2 plan: per-project coord, three-doc Ralph layout, failure table, sequencing. |
| `STATE.md` | Filesystem contract: layouts, atomic-publish, schema versions, locks. |
| `ENG-v0.2-coordinator.md` (this) | Package split, struct shapes, concurrency, sequence diagrams, test plan, perf budget. |

This doc adds aggregates; it does not change v0.1 ones. `agents/<id>.json`,
`handoffs/`, `inbox/`, `queue/` retain their v0.1 contracts. Coordinator
agents are ordinary Fleet agents — no schema bump on `agents/<id>.json`.

Two cross-cutting invariants drive the rest of the design:

- **C1 — Coordinator handoff is clean.** A coord that hits 50%/70% via
  fleet-guard hands off without losing per-task state. Tasks.md is the
  source of truth, so the replacement coord re-reads it and resumes;
  in-flight worker bookkeeping survives via `worker_id` + `pr_url` +
  `worktree` fields stored on each task. See §5.6.
- **C2 — Worker status reports never mix.** Each worker writes its
  status to its own per-worker channel (the coord's inbox, but
  scoped by a `task_id` line every sentinel must carry). Under
  parallel dispatch (cap > 1), two workers cannot corrupt each
  other's tasks because every mutation in the coord tick is keyed
  by `task_id` + `worker_id` matched pair. See §6.

---

## 2. Module structure

### 2.1 New Go packages

```
internal/
  tasks/         — markdown parser/writer for tasks.md + tasks-archive.md
  learnings/     — append-only writer + reader for learnings.md
  standards/     — merge global + per-project standards.md
  state/         — (extended) ProjectDir, ProjectStateLockPath, CoordinatorLockPath
cmd/fleet/
  tasks.go       — fleet tasks {add,list,show,set,note,archive,promote}
  learnings.go   — fleet learnings {add,list,prune}
  standards.go   — fleet standards {show,edit}
```

Dependency graph (acyclic, edges point at dependencies):

```
cmd/fleet/tasks.go ─────────┐
cmd/fleet/learnings.go ─────┼──► internal/tasks ─────► internal/state
cmd/fleet/standards.go ─────┤    internal/learnings ─► internal/state
                            └──► internal/standards ─► internal/state
```

`cmd/fleet/tasks.go` additionally imports `internal/agent` for the
"is a live coord running?" check (auto-spawn). No new package imports
`internal/tui` or modifies it; v0.2 ships CLI-only inspection.

### 2.2 Per-package contracts

**`internal/state/` (extended).** ~30 LOC added.

| Symbol | Returns | Caller |
|--------|---------|--------|
| `ProjectDir(name)` | `~/.fleet/projects/<safe>/` | tasks/, learnings/, standards/, CLIs |
| `ProjectStateLockPath(name)` | `<dir>/.locks/state.lock` — new tasks/learnings write lock | tasks/, learnings/, CLIs |
| `CoordinatorLockPath(name)` | `<dir>/.locks/coordinator.lock` | skill (path constant only) |
| `WorktreePath(project, slug)` | `<dir>/worktrees/<slug>/` | dispatch.py (cap > 1, deferred) |

The new locks live UNDER the project state-dir, not under
`~/.fleet/projects/.locks/`. They're separate from the v0.1
`ProjectLockPath` (which serializes handoff/dispatch). A coord holding
the new state-lock cannot starve dispatch; the v0.1 lock is untouched
by v0.2 code paths.

**`internal/tasks/` — types and contract**

```go
package tasks

const SchemaVersion = 1

type Status string
const (
    StatusTodo       Status = "todo"
    StatusReady      Status = "ready"
    StatusInProgress Status = "in-progress"
    StatusInReview   Status = "in-review"
    StatusDone       Status = "done"
    StatusBlocked    Status = "blocked"
    StatusAbandoned  Status = "abandoned"
)

type Priority string  // P0..P3

type Task struct {
    Slug        string     // <short>-<4hex>; immutable identifier
    Status      Status
    Priority    Priority
    WorkerID    string     // "" when null
    Worktree    string
    PRURL       string
    Branch      string
    Created     time.Time
    Updated     time.Time
    DependsOn   []string
    SpawnedBy   string     // "user" or "<8-hex>"
    Spec        string     // raw markdown body of ### Spec
    Acceptance  string
    Notes       string     // append-only; preserved verbatim across round-trip
}

type File struct {
    SchemaVersion int
    Tasks         []Task
    Footer        string  // trailing free text preserved on round-trip
}

func Read(project string) (*File, error)
func Write(project string, f *File) error          // caller MUST hold state-lock
func Append(project string, t Task) error          // wraps lock + read + add + write
func (f *File) Get(slug string) (*Task, error)
func (f *File) Set(t Task) error
func Archive(project string, slugs []string) error
```

**Round-trip rule.** `Read → Write` with no edits is byte-identical
except for `updated:` on tasks the caller mutated. The parser preserves
whitespace, footer, and section bodies verbatim. This protects
worker-edited `### Notes` from being clobbered.

**Read locks?** None. Markdown parsers read without locking; STATE.md A1
guarantees the file is either prior or new, never torn (`WriteAtomic`
uses `.tmp` + `rename(2)`). Eventual-consistency Ralph-style.

**`internal/learnings/`**

```go
package learnings

type Entry struct {
    Timestamp time.Time
    Author    string  // "agent:<8-hex>" or "operator"
    TaskSlug  string  // "" for operator entries
    Tag       string  // lowercase
    Body      string  // markdown body sans H2 line
}

func Append(project string, e Entry) error              // takes state-lock
func List(project string) ([]Entry, error)
func Filter(project string, tagSubstr, taskSlug string, limit int) ([]Entry, error)
func PruneOlder(project string, cutoff time.Time) (int, error)
```

Read-modify-write under state-lock (not `O_APPEND`) because PruneOlder
also rewrites and atomic-publish for fsnotify-watching readers. File
is small (<1MB at 1000 entries; §8).

**`internal/standards/`**

```go
package standards

type Section struct{ Name, Body, From string }  // From: "global" | "project"
type Merged struct{ Sections []Section }

func Load(project string) (*Merged, error)
func (m *Merged) Render() string
```

Section-level merge (per H2): per-project replaces global by name;
project-only sections append. See §12 Q2 for trade-off.

### 2.3 Skill side

```
skills/coordinator/
  SKILL.md      — frontmatter + invocation + loop-in-prose + worker prompt template
  loop.py       — one tick: lock, parse, reconcile, drain, dispatch, write-back
  parse.py      — Python tasks.md parser/writer (mirror of internal/tasks)
  dispatch.py   — build worker prompt, invoke `fleet dispatch`, write inbox
  conflict.py   — file-overlap heuristic for cap > 1
  tests/
    test_parse.py     — round-trips against internal/tasks/testdata/ fixtures
    test_loop.py      — drives a tick with mocked fleet binary + gh
    test_dispatch.py  — prompt assembly + subprocess argv assertions
    test_conflict.py  — overlap detection
```

Two parsers (Go + Python) is necessary: Go is the CLI's authoritative
producer, but the in-agent skill is Python and cannot link Go. CI gates
divergence — both parse every fixture in `internal/tasks/testdata/` and
must emit byte-equal output.

---

## 3. Data structures

### 3.1 `tasks.md` grammar

```
file        := frontmatter, blank, task-block*, footer?
frontmatter := "---" newline "schema: v" int newline "---" newline
task-block  := h2-anchor, blank, key-bullets, blank, h3-section+, blank?
h2-anchor   := "## task: " slug newline
slug        := [a-z0-9-]+ "-" hex{4}
key-bullets := key-bullet+
key-bullet  := "- " key ": " value newline
key         := status | priority | worker_id | worktree | pr_url
             | branch | created | updated | depends_on | spawned_by
h3-section  := "### " ("Spec"|"Acceptance"|"Notes") newline body
body        := text up to next "## " or "### " or EOF
footer      := arbitrary trailing markdown (preserved but ignored)
```

**Schema versioning (Q5 decision).** First three lines are a YAML
frontmatter block holding `schema: v1`. Parser hand-rolls the
bounded `key: value` shape between `---` lines (no yaml dep — stdlib
preserved). Parser refuses files with int > `tasks.SchemaVersion`.
Files without frontmatter are treated as v0 — Go parser auto-prepends
the v1 frontmatter on the next Write (lazy upgrade). Skill's Python
parser refuses v0 and waits for the first Go-side write (avoids two
parsers racing to add the header).

Concrete file head:

```markdown
---
schema: v1
---

## task: fix-flaky-handoff-test-7a3c
...
```

**Parse-time invariants.** Duplicate slug → error; unknown `status`
or `priority` → error; non-RFC3339 `created`/`updated` → error;
`depends_on` is a JSON array literal of slugs. Errors carry line + col +
raw line text. Coord skill catches and refuses to tick (§9.3).

**Slug generation rule.** The CLI assembles the final slug from up to
two inputs: `--slug <short>` and the spec body. Three cases:

| Input | Result |
|-------|--------|
| `--slug add-readme` (short form) | CLI appends random 4hex → `add-readme-7a3c` |
| `--slug add-readme-7a3c` (full form, matches `[a-z0-9-]+-[0-9a-f]{4}`) | Use as-is. Collision check; duplicate → error. |
| `--slug` omitted | CLI derives `<short>` from spec body's first line (kebab-case, lowercase, drop punctuation, truncate to 24 chars), then appends 4hex. Example spec line `"Write a README for build instructions"` → `write-a-readme-for-build-7a3c`. |

`internal/tasks.GenerateSlug(short, spec, existing)` lives in the
`internal/tasks` package and is called by both `cmd/fleet/tasks.go`
(operator path) and the worker / coord callers (which shell out to
`fleet tasks add`). Workers and the coord agent get to pass a
`--slug` if they have a clear name in mind; otherwise the CLI handles
it. Auto-generated short-descs are deterministic given the spec — re-
running the same `fleet tasks add` produces the same short prefix
(only the 4hex differs).

### 3.2 `learnings.md` grammar

YAML frontmatter (per Q5) + append-only H2 entries:

```
file         := frontmatter, blank, entry*
frontmatter  := "---" newline "schema: v" int newline "---" newline
entry        := h2-header, blank, body, blank
h2-header    := "## " RFC3339 " · " author " · " task-or-op " · tag:" tag newline
author       := "agent:" hex8 | "operator"
task-or-op   := "task:" slug | "operator"
```

Same lazy-upgrade rule as tasks.md: missing frontmatter → Go parser
prepends on next Write.

### 3.3 `standards.md` grammar

YAML frontmatter (per Q5) + plain markdown sections. Parser cares
about frontmatter and H2 boundaries:

```
file        := frontmatter, blank, "# Standards" newline, blank, section*
frontmatter := "---" newline "schema: v" int newline "---" newline
section     := "## " section-name newline body
```

Section-name is the merge key (Q2 decision); body is opaque.

### 3.4 Atomic-publish writer table (extends STATE.md)

| Aggregate | Writer | Reader | Lock | Atomic |
|-----------|--------|--------|------|--------|
| `projects/<n>/tasks.md` | Go CLI or coord skill (mutex via state-lock) | coord skill, CLI list, TUI v0.3 | state-lock | `WriteAtomic` |
| `projects/<n>/tasks-archive.md` | same | same | state-lock | `WriteAtomic` |
| `projects/<n>/learnings.md` | Go CLI, coord skill, worker (via CLI) | coord skill, operator | state-lock | `WriteAtomic` |
| `projects/<n>/learnings-archive.md` | only `PruneOlder` | rare | state-lock | `WriteAtomic` |
| `projects/<n>/standards.md` | operator via `fleet standards edit` | coord skill, prompt assembly | none | `WriteAtomic` on edit |
| `~/.fleet/standards.md` | operator | coord skill | none | `WriteAtomic` on edit |

---

## 4. Concurrency + atomicity

### 4.1 Two locks per project state-dir

```
~/.fleet/projects/<name>/.locks/coordinator.lock   — held by running coord
~/.fleet/projects/<name>/.locks/state.lock         — held during tasks/learnings writes
```

**`coordinator.lock`** (NB-flock, `LOCK_EX | LOCK_NB`). Held by the
coord agent for the lifetime of one tick. Second coord that finds it
held logs and exits cleanly. This is the "one coord per project"
enforcement.

**`state.lock`** (blocking flock). Held briefly during writes by:
- `fleet tasks {add,set,note,archive,promote}` — every mutation.
- `fleet learnings {add,prune}` — every mutation.
- coord skill — only when writing `tasks.md` back at end of tick.

Coord re-reads tasks.md immediately before write to detect operator
interleavings (NEW_TASK additions, manual edits).

### 4.2 Why two locks, not one?

Three options weighed:

1. **One state-lock per state-dir** (chosen). All writes serialize.
   At v0.2 scale (≤2 writers active, micros per write) contention is
   invisible. Impossible to deadlock.
2. **One lock per file** (`tasks.lock`, `learnings.lock`). Maximum
   parallelism but introduces ordering hazards (coord reads tasks AND
   learnings — needs a hierarchy to be race-free).
3. **State-lock + the existing v0.1 `ProjectLockPath`**. Two locks at
   different layers. Cleaner partition: handoff/dispatch keep their
   v0.1 lock; tasks/learnings get the new state-lock.

Option 1 + the existing v0.1 lock kept untouched is the
recommendation. The v0.1 lock continues to serialize handoff/dispatch
as before; the new state-lock is purely for tasks/learnings. They
don't interact — `fleet dispatch` doesn't touch `tasks.md`, and
`fleet tasks add` doesn't dispatch (it just enqueues an inbox sentinel).

### 4.3 Lock acquisition order in a tick

```
1. NB-flock coordinator.lock; on EWOULDBLOCK → log + return 0
2. (no lock) parse tasks.md, learnings.md, standards
3. (no lock) reconcile in-flight workers, drain sentinels, build dispatches
4. for each candidate dispatch: subprocess.run(["fleet", "dispatch", ...])
5. blocking flock on state.lock
6. re-read tasks.md (detect operator interleavings)
7. merge skill mutations on top; state.WriteAtomic
8. release state.lock
9. release coordinator.lock (or hold to next tick — see below)
```

The sleep between ticks is implicit: coord agent emits no output ⇒
Claude Code stops ⇒ fleet-guard fires Stop ⇒ skill ticks again.
Sentinels in coord's inbox + `fleet tasks add` writing `NEW_TASK=...`
to the inbox are the wake mechanism.

**Coordinator-lock lifecycle.** Each Stop hook is a new short-lived
Python process; flocks held by a process die when it exits. To keep
"one coord per project" across hook fires, the lock is re-acquired
on every tick. A sidecar PID file at `.locks/coordinator.lock.pid`
records the agent's tmux PID; if a contending tick sees the PID
matches its own agent, the re-acquire is benign. Other PID + alive →
exit. Other PID + dead → reclaim.

### 4.4 Atomic publish

Every write goes through `state.WriteAtomic` (`.tmp` + fsync + rename).
No parent-dir fsync — these aggregates are operational state, not
durable-event tier (v0.1 handoff docs are; v0.2 tasks.md is not).
Matches STATE.md A1b.

---

## 5. Sequence diagrams

### 5.1 Coordinator tick

```
fleet-guard.Stop fires (coord agent: FLEET_ROLE=coordinator)
       │
       ▼
+-------------------------------------------------+
| skills/coordinator/loop.py: tick()              |
+-------------------------------------------------+
       │ NB-flock coordinator.lock; on EWOULDBLOCK exit cleanly
       │
       │ tasks    = parse(tasks.md)            ──► no lock (eventual consistency)
       │ learn    = parse(learnings.md)
       │ stds     = standards.Load(project)
       │ live     = agent.List() filter project
       │
       │ # 1. Reconcile in-flight (per-task, no cross-task state)
       │ for t in tasks where status in {in-progress, in-review}:
       │   if t.worker_id ∉ live:
       │     if t.pr_url:
       │       ci = `gh pr checks <num>`        ──► 5min TTL cache
       │       (mutate t per CI; see §9.4)
       │     else:
       │       t.status = todo; clear worker; archive worktree
       │
       │ # 2. Drain inbox archive (workers' status reports)
       │ # See §6 — one file per delivery, scoped by task_id sentinel
       │ for archive_file in inbox/archive/<coord_id>-*.md (since last scan):
       │   for line in file:
       │     if TASK_DONE_PR(slug, url):       set t.pr_url, status=in-review
       │     elif BLOCKED_QUESTION(slug, txt): t.status=blocked; raise_to_user
       │     elif WORKER_FAILED(slug, reason): t.status=todo; clear worker
       │     elif NEW_TASK(slug):              wake-only
       │
       │ # 3. Dispatch ready tasks under cap
       │ active = count(status==in-progress)
       │ for t in sort_by_priority(filter_ready(tasks)):
       │   if active >= cap: break
       │   if conflict_with_inflight(t): continue
       │   worktree = create_worktree(t) if cap>1 else repo
       │   prompt   = build_worker_prompt(t, stds, learn)
       │   worker_id = subprocess(["fleet", "dispatch", ...])
       │   write_inbox(worker_id, prompt)
       │   t.worker_id, t.worktree, t.status = worker_id, worktree, in-progress
       │   active += 1
       │
       │ # 4. Write back
       │ flock state.lock (blocking)
       │   re-read tasks.md (catch operator interleavings)
       │   merge skill mutations; state.WriteAtomic
       │ unlock
       │
       │ # 5. Smart sleep: 270s tight / 1800s idle / 4h auto-stop
       ▼
return 0 (release coordinator.lock implicit on Python exit)
```

### 5.2 Worker dispatch

```
coord skill                    fleet binary             filesystem
   │                                │                       │
   │── fleet dispatch ─────────────►│                       │
   │   --project <p>                │ allocate id ─────────►│
   │   --task-id <slug>             │ tmux new-session ────►│
   │   --cwd <worktree>             │ write agents/<id>.json
   │◄── stdout: <worker_id> ────────│                       │
   │                                                        │
   │── write inbox (worker_prompt) ───────────────────────►│ inbox/<worker_id>.md
                                                             │
                            fleet-guard.SessionStart fires inside worker
                                                             │ reads inbox, injects body
                                                             │ archives to inbox/archive/
                                                             │
                            worker begins TDD → review → push
```

If the worker's first SessionStart races the inbox write and arrives
first, fleet-guard retries on the worker's first Stop. Acceptable.

### 5.3 Worker report-back (per-worker isolation; see also §6)

```
worker post-PR-push:
   fleet message <coord_id> "TASK_DONE_PR=<slug> <url>"
       │
       ▼ writes ~/.fleet/inbox/<coord_id>.md (state.WriteAtomic)
       │
   fleet-guard.Stop in coord agent delivers it via [OPERATOR] injection
   AND archives the inbox file to inbox/archive/<coord_id>-<UTCstamp>.md
       │
       ▼ next coord tick: skill scans inbox/archive/<coord_id>-*.md since last_scan_ts
       │
       │ each file has a known schema:
       │   one sentinel per file, must include the task slug as second token
       │   (TASK_DONE_PR=<slug> <url>, BLOCKED_QUESTION=<slug> <txt>, ...)
       │
       ▼ coord mutates ONLY the task with matching slug
```

The archive scan (rather than transcript walk) is the dedup-safe path:
fleet-guard guarantees one archive file per delivered message, and
coord can record `last_archive_scan_ts` in `coord-state.json` to never
re-process. This is the central mechanism for **C2 (worker status
reports never mix)**: every sentinel is keyed by `slug`, every file
contains exactly one sentinel, and coord mutates only the task with
that slug.

### 5.4 Raise-hand to operator

```
coord receives BLOCKED_QUESTION=<slug> <txt>  OR  PR-CI-green for <slug>
   │
   │── tasks.md: t.status = blocked; t.notes += question
   │── coord's own agent record: has_pending_question=true,
   │      blocked_reason=first 200 chars
   │      (re-uses fleet-guard's existing fields; no schema change)
   ▼
fleet status / TUI shows "asking" badge on coord row

operator answers via:
   fleet message <coord_id> "<answer>"      OR     fleet attach <coord_id>
```

### 5.5 Auto-idle-stop + respawn

```
coord tick observes: no tasks in {in-progress, in-review, blocked}
   │ idle_streak += 1 in coord-state.json
   │ if idle_streak * sleep_s >= 4h:
   │   log auto_stop reason=idle_4h
   │   final WriteAtomic of tasks.md (already done)
   │   exit 0   ──► Claude Code session ends; fleet-guard archives agent record
                       ── time passes ──
operator: fleet tasks add --project <p> --slug <s> "..."
   │
   │── (cmd/fleet/tasks.go) for r in agent.List():
   │     if r.Project==<p> AND r.Role=="coordinator" AND alive(r.TmuxSession): found
   │── if not found: fleet dispatch fleet-coord-<p> ...
   │── flock state.lock; append task; unlock
   ▼
fresh coord first Stop tick reads tasks.md, picks up the new task
```

### 5.6 C1 — Coordinator handoff is clean

The coord agent is itself under fleet-guard supervision. At 50%/70%
context, fleet-guard hands it off like any other agent. The handoff
must lose nothing.

```
coord at 50% context  →  fleet-guard injects HANDOFF REQUESTED
   │
   │ coord skill is at the "smart sleep" step of a tick — no in-flight
   │ work in coord process memory beyond the sleep loop
   │
   │ coord agent emits MILESTONE on its own line in its tmux pane
   │
   │ fleet-guard writes handoff doc → ~/.fleet/handoffs/<coord_id>-*.md
   │   doc body:
   │     ## Completed   = "tick #N executed; tasks.md is the truth"
   │     ## Files Modified = "~/.fleet/projects/<p>/tasks.md"
   │     ## Next Steps  = "Run /coordinator skill loop for project <p>."
   │
   │ fleet-guard enqueues spawn-fresh; fleet binary spawns successor
   │
successor coord starts; fleet-guard delivers handoff doc on SessionStart
   │
   │ successor reads doc → understands its job is "/coordinator for <p>"
   │ first Stop fires → loop.py runs tick #1
   │   re-acquires coordinator.lock (predecessor's process exited; lock free)
   │   parses tasks.md (truth)
   │   reconciles in-flight workers via worker_id ∈ agent.List() ──► no in-memory state needed
   │   drains inbox archive since coord_state.last_archive_scan_ts
   │
   ▼
no per-task state lost; in-flight workers continue uninterrupted
```

The mechanism is "stateless reentry over markdown-as-truth" — same
Ralph rule the rest of v0.2 follows. Coord process memory carries
nothing across handoff except `coord-state.json` (idle counters,
last_archive_scan_ts), which is on disk and survives. Every per-task
fact lives in `tasks.md`, every per-worker fact lives in
`agents/<worker_id>.json`, and the inbox archive is the durable queue
of pending sentinels.

**Test: `TestCoordHandoffPreservesInflight`** (§7.3). Spawn coord, set
one task in-progress with worker_id=W, write a TASK_DONE_PR archive
file. Force-handoff coord. Successor's first tick must see the
in-progress task, reconcile against W (alive), drain the archive, mark
the task in-review. Zero data loss.

---

## 6. Worker prompt + status isolation contract

> **REVISED 2026-05-06.** Workers are no longer Fleet agents. They run as
> single-shot `claude --print` subprocesses launched by the coord and
> communicate state via per-worker JSON files, NOT via inbox sentinels.
> The C2 invariant (parallel workers never mix status) is preserved
> because each worker writes to its own per-slug directory.

### 6.1 Inputs to prompt assembly

1. The task block (Spec + Acceptance + Notes) from tasks.md.
2. Merged standards (`internal/standards.Load`).
3. Top-N relevant learnings (tag-keyword match against task spec; top 5 by score DESC, timestamp DESC; section omitted if all scores zero).

### 6.2 Worker state file (worker → coord)

Each worker owns one directory: `~/.fleet/projects/<name>/workers/<slug>/`. The two files inside:

| File | Format | Writer | Reader |
|------|--------|--------|--------|
| `state.json` | JSON, atomic-rewritten on every phase change | the worker (via `fleet workers update <slug> --phase <p> [--pr-url <u>]`) | coord (fsnotify watch); operator (`fleet peek`) |
| `output.log` | append-only stdout/stderr capture | OS (coord redirects worker's stdout/stderr) | operator (`fleet peek --logs`) |

`state.json` schema:

```json
{
  "slug": "add-readme-7a3c",
  "project": "fleet",
  "phase": "review-codex",
  "phases_completed": ["branch", "tdd-red", "tdd-green", "tdd-refactor", "review-claude"],
  "started_at": "2026-05-06T14:00:00Z",
  "updated_at": "2026-05-06T14:08:23Z",
  "pid": 12345,
  "pr_url": null,
  "blocked_reason": null,
  "exit": null
}
```

Allowed `phase` values: `starting`, `branch`, `tdd-red`, `tdd-green`, `tdd-refactor`, `review-claude`, `review-codex`, `push`, `done`, `blocked`, `failed`. Phase `done` requires non-empty `pr_url`. Phase `blocked` requires non-empty `blocked_reason`.

### 6.3 Per-worker isolation discipline

Each worker writes to ITS OWN directory `<workers>/<slug>/`. There is no shared inbox file, no contention, no risk of cross-write. Two parallel workers each write to disjoint paths; coord watches both via fsnotify and demuxes by directory name.

Compared to the old sentinel-via-inbox design, this is structurally simpler and deadlock-free.

### 6.4 Slug-mismatch and recovery

- Worker writes a `state.json` whose `slug` field doesn't match the directory name → coord logs WARN, ignores. (Should never happen — coord seeded the directory.)
- Worker process exits without ever reaching `phase=done` → coord's reconcile detects PID is dead AND `phase != done`, runs the recovery path (§9.4): if `pr_url` set, check CI; otherwise reset task to todo.
- Worker writes `phase=blocked` → coord raises to operator with `blocked_reason`, sets task status to `blocked`. Operator clarifies the spec; coord re-dispatches the same slug (or a follow-up).
- Worker writes `phase=failed` → coord resets task to todo with the worker's note; operator can re-dispatch or revise spec.

### 6.5 Rendered worker prompt (~3KB)

```
You are a Fleet worker for task: add-readme-7a3c
Project: fleet
Branch: worker/add-readme-7a3c

You are running as a SINGLE `claude --print` invocation. No interactive
chat. Communicate progress via `fleet workers update <slug> --phase <p>`.

State file:  ~/.fleet/projects/fleet/workers/add-readme-7a3c/state.json
Output log:  ~/.fleet/projects/fleet/workers/add-readme-7a3c/output.log
             (everything you write to stdout/stderr lands here)

## Task
[Spec body, verbatim from tasks.md]

## Acceptance
[Acceptance body, verbatim]

## Standards (the bar — non-negotiable)
[merged standards, section by section]

## Relevant prior learnings
[up to 5 entries, each truncated to 500 chars]

## Required workflow

  fleet workers update add-readme-7a3c --phase branch
1. git checkout -b worker/add-readme-7a3c

  fleet workers update add-readme-7a3c --phase tdd-red
2a. Write the failing test. git commit.

  fleet workers update add-readme-7a3c --phase tdd-green
2b. Write the minimal impl. Test passes. git commit.

  fleet workers update add-readme-7a3c --phase tdd-refactor
2c. Refactor without changing test. git commit.

  fleet workers update add-readme-7a3c --phase review-claude
3a. /review on your diff. Fix every P0/P1.

  fleet workers update add-readme-7a3c --phase review-codex
3b. /codex review on your diff. Fix every P0/P1.

  fleet workers update add-readme-7a3c --phase push
4. gh pr create. Capture the PR URL.

  fleet workers update add-readme-7a3c --phase done --pr-url <url>
5. Done. Coord sees phase=done, archives this dir, transitions task to in-review.
6. Optional: fleet learnings add --project fleet --tag <topic> \
     --task add-readme-7a3c "<one paragraph>"

## Constraints
- Stay on this task. File incidental bugs (max 3/session, honor system):
    fleet tasks add --project fleet --spawned-by add-readme-7a3c --priority P3 \
      --slug <short> "<one-line spec>"
  Operator must promote before dispatch.
- Do NOT edit tasks.md or standards.md directly.
- Stuck or genuinely confused:
    fleet workers update add-readme-7a3c --phase blocked --reason "<one line>"
  Then exit 0. Coord raises to operator.

You have: /review, /codex review, gh, git, full repo at <cwd>.
NO interactive chat — operator can't reply mid-flight. Communicate via
`fleet workers update`, which mutates state.json atomically.
```

Hard cap: 4KB rendered (learnings truncated to 500 chars × 5; standards
inlined verbatim — operator's responsibility to keep them small).

---

## 7. Test strategy

### 7.1 Go unit tests (stdlib `testing`)

`internal/tasks/tasks_test.go`:
- `TestRoundTrip` — every fixture in `testdata/` round-trips byte-equal
  except mutated `updated:` lines.
- `TestSchemaVersionRefuse` — frontmatter `schema: v2` returns error.
- `TestSchemaVersionUpgrade` — file without frontmatter auto-prepends
  the v1 frontmatter block on Write.
- `TestFrontmatterRoundTrip` — parser tolerates trailing whitespace,
  CRLF, blank lines between frontmatter and first task block.
- `TestMalformedRecovery` — bad date in `created:` returns typed error
  with line + col + raw line.
- `TestMultiTask` — 50-task fixture parses + writes stably.
- `TestSlugUniqueness` — duplicate slug → ErrDuplicateSlug.
- `TestArchive` — moves slugs from tasks.md to tasks-archive.md atomically.

`internal/learnings/learnings_test.go`:
- `TestAppendConcurrent` — N goroutines append; final file has N entries.
- `TestPruneByDate` — pre-cutoff entries move to archive; current file
  has only post-cutoff.
- `TestFilter` — tag substring + slug filter returns expected subset.
- `TestParseMalformed` — malformed entry skipped, not crashed.

`internal/standards/standards_test.go`:
- `TestMergeGlobalOnly`
- `TestMergeProjectOverride` — project's `Testing` replaces global's;
  others pass through.
- `TestMergeProjectAddSection` — project-only section appears at end.
- `TestRenderRoundTrip` — Render output is parseable as standards.md.

`cmd/fleet/{tasks,learnings,standards}_test.go`:
- cobra flag table-tests.
- Error paths: missing project, unknown slug, bad priority enum.
- Auto-spawn-on-`fleet tasks add`: stub `agent.List` and dispatch,
  assert dispatch invoked iff no live coord.

### 7.2 Skill tests (Python `unittest`)

`skills/coordinator/tests/test_parse.py`:
- `test_round_trip_against_go_fixtures` — every fixture in
  `internal/tasks/testdata/` parses + writes byte-equal in Python and
  matches Go output.
- `test_schema_version_refuse`, `test_malformed_recovery`.

`skills/coordinator/tests/test_loop.py`:
- Mock `fleet` binary as a Python stub recording argv, returning fixed
  agent ID. Mock `gh pr checks` similarly.
- `test_tick_dispatches_ready_task` — fixture with one ready task →
  one dispatch + inbox + status mutation.
- `test_tick_reconciles_dead_worker` — in-progress task with worker_id
  not in mocked agent.List() → reset to todo with note.
- `test_tick_drains_per_task_sentinels` — two archive files for two
  different slugs → two distinct task mutations, no cross-corruption.
  (**Critical for C2**.)
- `test_tick_respects_cap` — cap=1, two ready tasks → one dispatched.
- `test_tick_no_op_under_lock_held` — coordinator-lock held by another
  PID → tick returns 0 without writing.
- `test_auto_idle_stop_after_4h` — fake clock; idle streak crosses 4h
  → tick exits with auto-stop log.
- `test_slug_mismatch_sentinel_ignored` — sentinel with unknown slug
  logs WARN; tasks.md unchanged.

`skills/coordinator/tests/test_conflict.py`:
- Two tasks both naming `auth/middleware.go` → conflict.
- Disjoint files → no conflict.
- No `path:`/`file:` mentions → conservative non-conflict.

### 7.3 Integration tests (Go in `cmd/fleet/`)

`cmd/fleet/coordinator_integration_test.go`:
- `t.TempDir` for FLEET_HOME; fake `claude` binary on PATH.
- `TestEndToEndDispatchAndComplete` — add task, spawn coord, fake
  worker emits TASK_DONE_PR, assert tasks.md has in-review + pr_url.
- `TestAutoIdleStopRespawn` — coord auto-stops; next `fleet tasks add`
  spawns fresh coord.
- `TestTwoCoordLockContention` — second coord exits cleanly; first is
  the only writer.
- `TestCoordHandoffPreservesInflight` — **C1 test**: one task
  in-progress with worker W, archive has TASK_DONE_PR=W. Force-handoff
  coord. Successor's first tick reconciles: task = in-review, pr_url
  set. Zero data loss.
- `TestParallelWorkerStatusIsolation` — **C2 test**: cap=2; two ready
  tasks dispatched. Two archive files arrive (TASK_DONE_PR=A and
  TASK_DONE_PR=B). Coord tick mutates A and B independently; neither
  task's notes contain the other's PR URL.

### 7.4 Fixtures

`internal/tasks/testdata/`:
- `empty.md`, `single-todo.md`, `multi-status.md`, `deps.md`,
  `worker-notes.md` (multi-paragraph Notes), `malformed-bad-date.md`,
  `malformed-bad-status.md`, `schema-v2.md` (refused), `no-schema.md`
  (auto-upgraded), `fifty-tasks.md` (~50KB).

Both Go and Python parsers run against all. CI fails on divergence.

---

## 8. Performance budget

### 8.1 Tick time

Target: **< 500ms p99** for projects with ≤50 active tasks.

| Step | Cost |
|------|------|
| Parse tasks.md (50 tasks ≈ 50KB) | 5–10ms |
| Parse learnings.md (~500 entries, 1MB cap) | 20–40ms |
| Standards Load | <1ms |
| `agent.List()` glob | 5–15ms |
| `gh pr checks` per in-review task | 200–400ms (network) |
| Conflict detection | <1ms |
| `fleet dispatch` per worker | 100–300ms (tmux) |
| `WriteAtomic` on tasks.md | 5–10ms |

A zero-action tick: ~50ms. One CI check: ~300–450ms. Three CI checks
+ one dispatch: ~1.2s — acceptable; coord agents have nothing to
"respond to," slowness is invisible.

### 8.2 `gh pr checks` cache

5min TTL on `coord-state.json:pr_check_cache[pr_url] = {state, ts}`.
First tick after PR push pays the cost; subsequent ticks within 5min
are free. Cache invalidated on WORKER_FAILED or operator inbox.

### 8.3 File-size bounds

| File | Soft cap | Action |
|------|----------|--------|
| tasks.md | 200 active tasks | `tasks list` shows "consider archiving" |
| tasks-archive.md | 1000 entries | grow forever |
| learnings.md | ~1MB / ~500 entries | `learnings prune --older-than 30d` |
| standards.md | 100 sections | absurd; not enforced |
| coord.log | 10MB | rotates daily, keep 7 |

### 8.4 fsnotify watcher count

Per-project state-dir adds 1 watcher. 10 projects → 10 watchers.
Negligible.

### 8.5 Worker prompt size

≤ 4KB rendered. Header ~200B + spec ≤ 1KB + standards ~1KB +
5×500B learnings + workflow ~600B.

---

## 9. Failure modes (engineering)

Extends PLAN §"Failure modes" with implementation detail.

### 9.1 Logging

`~/.fleet/projects/<n>/coord.log` — one line per event:
`<RFC3339> <level> <event_key>=<value> ...`. Levels INFO/WARN/ERROR.
Rotation: rename to `coord.log.1..7` when size > 10MB, runs at tick start.

Required event keys:

| event_key | Fields |
|-----------|--------|
| `tick.start` | tick_id |
| `tick.parse_error` | tick_id, file, line, msg |
| `tick.reconcile.worker_died` | tick_id, slug, worker_id |
| `tick.dispatch` | tick_id, slug, worker_id |
| `tick.gh_check` | tick_id, slug, pr, state, latency_ms |
| `tick.write_back` | tick_id, n_mutations, latency_ms |
| `tick.end` | tick_id, sleep_s, n_active |
| `auto_stop` | reason, idle_h |

### 9.2 Metrics

v0.2 logs only. No expvar / Unix socket. Counters in
`coord-state.json` (24h rolling): tick_count, dispatch_count,
sentinel_count, gh_check_count, gh_check_error_count.

### 9.3 Recovery: corrupted tasks.md

- Coord skill — refuses to tick. ERROR log. Sets
  `has_pending_question=true` on coord's agent record with
  `blocked_reason="tasks.md parse error: <file>:<line> <msg>"`.
  Operator sees asking badge in `fleet status`.
- CLI `fleet tasks list` — exits 4 (state error per STATE.md table).
- TUI v0.3 — alerts banner.

No auto-repair; operator edits manually.

### 9.4 Recovery: worker died mid-PR-push (`loop.py:reconcile_inflight`)

```python
for t in tasks:
    if t.status not in {"in-progress", "in-review"}: continue
    if t.worker_id and t.worker_id in live_agents: continue

    if t.pr_url:
        ci = run_gh_pr_checks(t.pr_url)  # 5min TTL cache
        if ci.all_green and ci.merged:
            t.status = "done"; archive_worktree_if_any(t)
        elif ci.all_green and not ci.merged:
            raise_to_user(t, "CI green, ready to merge")
            t.status = "in-review"
        elif not ci.mergeable:
            t.status = "todo"; t.worker_id = ""
            t.notes += "\nrebase needed"
        elif ci.failed:
            t.status = "todo"; t.worker_id = ""
            t.notes += f"\nCI red {ci.url}"
            raise_to_user(t, "CI red")
    else:
        t.status = "todo"; t.worker_id = ""
        t.notes += "\nworker died without PR"
        cleanup_worktree(t.worktree); t.worktree = ""
```

`archive_worktree_if_any` runs `git worktree remove --force` and
ignores ENOENT.

### 9.5 Partial write of tasks.md

`state.WriteAtomic` + fsync + rename. Crash leaves either
`tasks.md.tmp.<pid>` (rename pending) or new tasks.md (rename atomic).
`TestWriteAtomicCrashSimulation` covers it. Cleanup at coord start:
remove `tasks.md.tmp.*` older than 1 minute.

### 9.6 Two-coord race

Second coord NB-flock returns EWOULDBLOCK. Logs INFO ("another
coordinator owns project <n>; exiting") and returns 0. tmux session
exists but is benign on each Stop fire. Operator cleans with
`fleet rm <id>`.

### 9.7 Inbox sentinel parse failures

Malformed sentinel (typo, missing slug):
- Log WARN with raw line.
- Continue. No mutation.
- Operator can manually re-send via `fleet message <coord_id> ...`.

---

## 10. Migration from v0.1

### 10.1 No data migration

v0.1 aggregates unchanged. v0.2 lazily creates:

```
~/.fleet/standards.md                  — by `fleet init --upgrade`
                                          (seeded from template, includes
                                          schema-v1 YAML frontmatter)
~/.fleet/projects/<n>/                 — on first `fleet tasks add`
~/.fleet/projects/<n>/.locks/          — on first lock acquire
~/.fleet/projects/<n>/tasks.md         — on first `fleet tasks add`
                                          (Write prepends frontmatter)
~/.fleet/projects/<n>/learnings.md     — on first append
                                          (Write prepends frontmatter)
```

`agents/<id>.json` schema_version stays 1.

**Frontmatter migration (Q5).** A v0.2 install picking up an existing
v0.1 layout finds no per-project state files (none existed in v0.1),
so there's nothing to migrate. If a future v0.2 patch upgrades v1→v2
schema, the parser sees v1 frontmatter, applies the upgrade transform,
and rewrites with v2 frontmatter on next Write — operator does
nothing.

### 10.2 Existing dispatch flow unchanged

```
fleet dispatch fleet-coord-<project> --project <project> --cwd <repo>
```

is plain `fleet dispatch`. No new subcommand for "spawn coord."

### 10.3 `fleet init --upgrade`

`cmd/fleet/init.go:60-72` currently installs fleet-guard. v0.2
generalizes to "install all `skills/*/` from embed" + seed
`~/.fleet/standards.md` from template if missing.
`fleet init --upgrade` overwrites skill files but never overwrites
existing standards.md.

---

## 11. Sequencing — 13 PRs

### Phase A — primitives (sequential)

1. **`feat(state): add ProjectDir + ProjectStateLockPath helpers`**
   - `internal/state/state.go` (~30), test (~50). LOC ~80.

2. **`feat(tasks): markdown task registry parser and writer`**
   - `internal/tasks/tasks.go` (~250), test (~200), `testdata/` (~10
     fixtures). LOC ~500. Depends on 1.

3. **`feat(learnings): append-only learnings log with prune`**  ‖4
   - `internal/learnings/` (~120 + 100). LOC ~220. Depends on 1.

4. **`feat(standards): merge global + per-project standards`**  ‖3
   - `internal/standards/` (~80 + 80). LOC ~160. Depends on 1.

### Phase B — operator CLI (parallel-safe within phase)

5. **`feat(cli): fleet tasks add/list/show/set/note/archive/promote`**
   - `cmd/fleet/tasks.go` (~200 + 150 test), main.go register.
   - Auto-spawn coord on add when no live coord.
   - LOC ~350. Depends on 2.

6. **`feat(cli): fleet learnings add/list/prune`**  ‖7
   - LOC ~200. Depends on 3.

7. **`feat(cli): fleet standards show/edit`**  ‖6
   - LOC ~100. Depends on 4.

### Phase C — skill (sequential within)

8. **`feat(coord): python tasks.md parser matching go contract`**
   - `parse.py` (~150) + tests (~100) byte-equal vs Go on every fixture.
   - LOC ~250. Depends on 2.

9. **`feat(coord): worker prompt assembly + fleet-dispatch caller`**
   - `dispatch.py` (~120 + 80 test). LOC ~200. Depends on 8, 7.

10. **`feat(coord): file-overlap conflict heuristic`**  ‖11
    - `conflict.py` (~80 + 60). LOC ~140. Depends on 8.

11. **`feat(coord): tick loop driver with reconcile + dispatch`**
    - `loop.py` (~250) + SKILL.md (~150) + test_loop.py (~200).
    - Tests **must** include `test_tick_drains_per_task_sentinels`
      (C2) and a coord-handoff equivalent at the skill layer.
    - LOC ~600. Depends on 9, 10.

### Phase D — packaging (sequential)

12. **`feat(init): install coordinator skill + seed standards.md`**
    - `cmd/fleet/init.go` (~30 change) + `embed.go` (~5 change) + test.
    - LOC ~100. Depends on 11.

13. **`test(coord): end-to-end dispatch / report / archive`**
    - `cmd/fleet/coordinator_integration_test.go` (~300).
    - Includes `TestCoordHandoffPreservesInflight` (C1) and
      `TestParallelWorkerStatusIsolation` (C2).
    - LOC ~300. Depends on 12.

### Deferred to v0.2.x

14. Worktree creation (cap > 1 mode).
15. Conflict-aware parallel dispatch wired into loop.py.

Phase A→B→C→D is roughly 6 weeks solo.

---

## 12. Open questions for operator

> **All Q1–Q6 decisions locked 2026-05-06 by operator.**

### Q1. Tasks-write lock granularity — **DECIDED: one per state-dir**

> One state-lock per project state-dir, or per file?

Single flock at `~/.fleet/projects/<name>/.locks/state.lock`. All writers (`fleet tasks/learnings/standards` CLI + coordinator tick write-back) take it. Zero deadlock surface. Contention invisible at v0.2 scale.

### Q2. Standards merge granularity — **DECIDED: section-level per H2**

> Section-level (per H2), or whole-file replace?

Each H2 in per-project `standards.md` replaces the same-named H2 in global. Project-only sections append. ~30 LOC merge logic. Operator writes deltas, not duplicates.

### Q3. `gh pr checks` polling — **DECIDED: synchronous + 5min TTL cache**

> Synchronous in tick, or fire-and-forget cache?

Coord checks gh API only if cache entry for that PR is older than 5 min. Cache lives in `coord-state.json`. First tick after PR push pays ~300ms; subsequent within 5min free. Cache invalidated on `WORKER_FAILED` or operator inbox.

### Q4. Worker session cap on bug-files — **DECIDED: honor system + telemetry**

> Honor system, or CLI-enforced (reject after 3)?

Worker prompt says max 3/session. CLI logs when `--spawned-by` is an agent ID and count rises above 3 but does NOT reject. Worker-filed tasks need operator promotion before dispatch (this is the real recursion gate). v0.3 hardens iff dogfood shows abuse.

### Q5. Schema versioning surface — **DECIDED: YAML frontmatter** (override of recommendation)

> `<!-- schema: v1 -->` HTML comment, or YAML frontmatter?

Each state file (`tasks.md`, `learnings.md`, `standards.md`) starts with:

```
---
schema: v1
---
```

Parser is hand-rolled for the bounded `key: value` shape between two `---` lines (no yaml dep — stdlib-only preserved). ~30 LOC parse + 10 LOC write per file.

**Follow-up edits required elsewhere in this doc:**
- §3 (Data structures) — replace `<!-- schema: v1 -->` lines in the markdown grammars with the YAML frontmatter block.
- §10.1 — migration: `fleet init --upgrade` writes the frontmatter into existing v0.1 files if missing.
- §6 (Worker prompt) — no change (workers don't see the frontmatter; coord and CLI handle it).

### Q6. Coord sentinel-reading mechanism — **DECIDED: read inbox/archive/ directly**

> Should coord parse sentinels from its inbox archive directory, or
> from its own conversation transcript?

Coord skill scans `~/.fleet/inbox/archive/<coord_id>-*.md` files newer than `last_archive_scan_ts` watermark in `coord-state.json`. Parses each, deletes after handling. fleet-guard already produces one archive file per delivery — coord just consumes them.

---

## Notes for implementers

- Per-project state-dir creation is lazy. `state.ProjectDir` does NOT
  call `MkdirAll`; first writer (e.g. `internal/tasks.Write`) does.
  Readers tolerate ENOENT and treat as empty.
- Python and Go parsers MUST stay byte-equal. CI gates this against
  the shared fixture set.
- Coord's role: `agent.Record.Role` currently is `"executor" |
  "planner"`. Add `"coordinator"` as part of PR 12 (one-line enum
  addition).
- `fleet poke` (mentioned in PLAN failure-mode table) is NOT in v0.2.
  Operators wake an idle coord via `fleet message <coord_id>
  NEW_TASK=<slug>` — same path `fleet tasks add` uses internally.
- Worker-side `fleet learnings add` shells out to the Go CLI, which
  takes the state-lock. Workers do NOT write `learnings.md` directly.
  Same for `fleet tasks add` from inside a worker — STATE.md A2,
  single writer per aggregate.
- `coord-state.json` is owned by the coord skill, lives at
  `~/.fleet/projects/<n>/coord-state.json`, atomic via WriteAtomic.
  Not in STATE.md writer table because lifecycle is bounded to the
  skill — successor coord rebuilds counters lazily if missing.
