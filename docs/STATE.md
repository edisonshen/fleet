# State & Reliability Reference

> This doc is the implementer's companion to `DESIGN.md`. DESIGN.md owns the
> product thesis and the user-visible surface. STATE.md owns the filesystem
> contract, atomicity rules, schema versions, and the invariants that keep
> Fleet from corrupting itself on crash or concurrent use.

Summary table of invariants is in DESIGN.md under "Reliability Invariants."
Everything below is the full detail implementers reference when writing
state-touching code.

## Scope constraints

Fleet v1 is **single-machine, single-operator**. All state lives in one
operator's `~/.fleet/` on one laptop. No cross-machine state sync, no
distributed locks, no network protocols between fleet binary, fleet-guard
skill, and TUI — all three run on localhost. Multi-machine fleets are v2+.

Fleet is a **supervisor architecture**, not a host architecture. It watches
and orchestrates Claude Code instances; it does not control their inner loop.
Fleet never edits a running agent's conversation history, never makes LLM
calls on the agent's behalf, never reaches inside Claude Code's process
memory. The supervisor's only levers are:

1. The `fleet-guard` skill running inside each agent (reads/writes files, can
   inject next-user-message content via hook stdout directives).
2. The tmux session the agent runs in (can be spawned, killed, attached).

Anything that would require editing Claude Code's internal state is out of
scope. This is why Fleet uses kill-and-respawn with a handoff doc rather than
in-place compression (the pattern Hermes / OpenClaw use — they are hosts).

## File layout and ownership

```
~/.fleet/
├── config.yaml                      # global prefs, defaults, schema_version
├── projects/
│   ├── <name>.yaml                  # project manifest (tasks, auto_spawn)
│   └── .locks/<name>.lock           # flock target for manifest mutations
├── agents/
│   ├── <id>.json                    # live agent health
│   └── archive/<id>-<ts>.json       # dead/crashed agents (7d retention)
├── handoffs/
│   └── <agent-id>-<utc-iso>-<short-uuid>.md
├── inbox/
│   ├── <id>.md                      # pending operator message
│   └── archive/<id>-<ts>-<uuid>.md  # delivered messages (7d retention)
├── progress/
│   └── <task-id>.jsonl              # append-only event log
├── queue/
│   └── spawn-fresh-<id>.json        # command to spawn replacement agent
└── logs/
    └── <id>-<date>.log              # tmux pane capture
```

### Writer table — one writer per aggregate

| Aggregate | Single writer | Readers | Atomic pattern |
|-----------|---------------|---------|----------------|
| `agents/<id>.json` | fleet-guard in agent `<id>`'s process | fleet binary (TUI, CLI), reconcile | `.tmp` + `mv` |
| `projects/<name>.yaml` | fleet binary (mutex via flock on `.locks/<name>.lock`) | fleet-guard, TUI, CLI | `.tmp` + `mv` while holding flock |
| `handoffs/*.md` | fleet-guard writes once, never edits | fleet binary, spawned replacement agent | `.tmp` + `fsync` + `mv` + dir-`fsync` |
| `inbox/<id>.md` | fleet binary (CLI commands) | fleet-guard (reads, archives) | `.tmp` + `mv` |
| `progress/<task-id>.jsonl` | any writer (append-only, bounded line size) | fleet binary | `O_APPEND` with line < PIPE_BUF |
| `queue/*.json` | producer writes once | fleet binary consumes + deletes | `.tmp` + `mv` |

"One writer per aggregate" is the key simplifier. Cross-file transactions
are avoided by design. Where two files must stay in sync (manifest ↔
agent JSON on dispatch), the manifest is the leader and updated last under
flock.

## Atomicity contracts

### A1 — Atomic file publish

**Rule:** Every state file is published by `rename(2)` from a sibling `.tmp`,
not by truncating and writing in place.

**Pattern (bash):**

```bash
write_json_atomic() {
  local dest="$1"
  local content="$2"
  local tmp="${dest}.tmp.$$"
  printf '%s' "$content" > "$tmp"
  mv "$tmp" "$dest"              # atomic on POSIX same-fs
}
```

**Pattern (Go):**

```go
func writeAtomic(path string, data []byte) error {
    tmp := path + ".tmp." + strconv.Itoa(os.Getpid())
    f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
    if err != nil { return err }
    if _, err := f.Write(data); err != nil { f.Close(); os.Remove(tmp); return err }
    if err := f.Sync(); err != nil { f.Close(); os.Remove(tmp); return err }
    if err := f.Close(); err != nil { os.Remove(tmp); return err }
    return os.Rename(tmp, path)
}
```

**Why:** `fsnotify` on the reader side fires `CREATE` the instant the file
exists. A partial write would be read as truncated JSON. `rename(2)` on the
same filesystem is atomic — readers see either the old file or the new file,
never a half-written state.

### A1b — fsync requirements

For state that must survive power loss (handoff docs, progress log flush
boundaries), `fsync` both the file data and the parent directory:

```bash
python3 -c "
import os
for p in ['$tmpfile', os.path.dirname('$tmpfile')]:
    fd = os.open(p, os.O_RDONLY); os.fsync(fd); os.close(fd)
"
mv "$tmpfile" "$dest"
python3 -c "
import os
dfd = os.open(os.path.dirname('$dest'), os.O_RDONLY); os.fsync(dfd); os.close(dfd)
"
```

For ephemeral state (agent health JSON rewritten every turn) `fsync` is not
required — the next turn overwrites it. Save fsync for durable events.

### A1c — Startup reconcile pass

On `fleet` binary start, before the TUI accepts input, run the reconcile:

```
For each f in ~/.fleet/queue/*.json:
    agent_id = parse(f).agent_id
    if tmux_session_exists("fleet-" + agent_id):
        skip  (agent still alive, queue trigger stale — delete f)
    else:
        process f as normal (spawn replacement, handoff, etc.)

For each f in ~/.fleet/agents/*.json:
    if not tmux_session_exists("fleet-" + agent_id):
        mv f -> archive/

For each f in ~/.fleet/handoffs/*.md.tmp:
    rm f  (crash during handoff write; incomplete doc, discard)
```

This makes Fleet self-healing on restart without needing a separate
reconcile command. Idempotent; safe to re-run.

## A2 — Concurrent CLI serialization

Two concurrent `fleet dispatch rainier` invocations would race to pick the
top-priority `todo` task. Serialization via `flock(2)` on the project's lock
file.

**Pattern (Go):**

```go
import "golang.org/x/sys/unix"

func withProjectLock(project string, fn func() error) error {
    lockPath := filepath.Join(fleetHome, "projects", ".locks", project+".lock")
    os.MkdirAll(filepath.Dir(lockPath), 0755)
    f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644)
    if err != nil { return err }
    defer f.Close()
    if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil { return err }
    defer unix.Flock(int(f.Fd()), unix.LOCK_UN)
    return fn()
}
```

Lock scope: per-project. Two operators running `fleet dispatch rainier` in
different shells serialize; `fleet dispatch rainier` and `fleet dispatch
gift-finder` run in parallel. Global locking is not needed; no mutation
touches more than one project manifest at a time.

Non-mutating commands (`fleet status`, `fleet peek`) do not acquire the
lock — they read the manifest (tolerate some staleness) and the agent JSONs
(already atomic).

## A3 — Handoff completion signal

fleet-guard writes the handoff doc atomically (`.tmp` + fsync + rename +
dir-fsync) to
`~/.fleet/handoffs/<agent-id>-<utc-iso>-<short-uuid>.md`.

Fleet binary's fsnotify watcher on `~/.fleet/handoffs/` fires `CREATE` on
the rename. Fleet parses frontmatter to correlate `agent_id`, then
proceeds with the handoff sequence (mark agent departing, spawn
replacement if `auto_spawn: true`, etc.).

**The filename IS the signal.** No separate sentinel file in `queue/`. No
tmux pane grep for `MILESTONE` strings (absent `HANDOFF REQUESTED` context). No control decisions derived
from Claude's free-form output.

**Rationale:** atomic rename is a well-understood Unix IPC pattern. Adding a
second signal (tmux grep as fallback) doubles test surface without
meaningfully improving robustness — if the skill fails between writing the
handoff and writing a sentinel, it would also fail between writing the
handoff and the grep-able string. One deterministic path is better than two
probabilistic ones.

## A4 — Auto-spawn rate limiting

When `auto_spawn: true` and an agent crashes during init (malformed handoff
doc, missing task file, tmux failure), Fleet would otherwise respawn
indefinitely, filling `archive/` and burning API credits.

**Budget rules:**

- **Hard ceiling:** ≤3 spawns per task per rolling 1-hour window.
- **Cooldown:** ≥30 seconds between same-task spawns.
- **On budget exhaustion:** set task `status: unhealthy` in the manifest,
  populate `unhealthy_reason: <string>` (e.g.,
  `"3 crashes in 1h · last: skill load fail"`). TUI surfaces
  `⚠ Task X unhealthy: 3 crashes in 1h`. Operator must clear via
  `fleet tasks unblock <project>/<task>` before auto-spawn resumes.
  Unblock clears both `spawn_history` and `unhealthy_reason`, sets
  `status: todo`.
- **State lives** in the manifest itself: per-task `spawn_history: [<ts>,
  <ts>, ...]`, pruned to the last hour on each read.

**Observed crash signals:** agent exits within 60s of spawn OR tmux session
not found 30s after `fleet dispatch` returned OR health JSON never written.

## A4b — Retention and pruning

Every operational file has a retention window. Pruning runs at startup
(after the A1c reconcile pass) and hourly during a TUI session;
`fleet prune` and `fleet prune --dry-run` are explicit CLI invocations.

| Path | Retention | Reason |
|------|-----------|--------|
| `agents/archive/<id>-<ts>.json` | 7 days | operational debris (recent crash forensics) |
| `logs/<id>-<date>.log` | 7 days | tmux pane capture |
| `inbox/archive/<id>-<ts>-<uuid>.md` | 7 days | already-consumed messages |
| `handoffs/<agent-id>-<utc-iso>-<uuid>.md` | 30 days from `timestamp:` | task history, outlives agents |
| `progress/<task-id>.jsonl` | 30 days from last write | audit trail |
| `projects/<name>.yaml` | never auto-pruned | source of truth for project state |
| `config.yaml` | never auto-pruned | user config |

**7d vs 30d split:** agent JSONs and tmux logs are operational debris —
they exist to debug recent crashes, not keep a permanent record. Handoff
docs and progress logs are part of the *task's* history and should
outlive the agents that wrote them. 30 days is enough to debug "what
happened to X last month?" without growing unbounded.

**Why deletion, not compaction:** compacting a JSONL file in place needs
locking, tempfiles, atomic-rename. Deleting a whole archived file is one
syscall. Files within their retention window stay as-is; we don't trim
line-by-line. Manifests and config never auto-prune.

**Pruning as one-shot, not daemon:** Fleet doesn't run a background
sweeper thread. Two trigger points are enough — startup catches the case
where Fleet was off for a week, the hourly tick catches the case where
Fleet runs continuously. CLI-only runs (no TUI) skip the hourly tick;
operators in CLI-only mode rely on startup pruning or explicit
`fleet prune`.

## A5 — Schema versioning

Every JSON shape carries `schema_version: <int>`. The binary embeds the
schemas it knows:

```go
const (
    AgentJSONSchemaV        = 1
    ManifestSchemaV         = 1
    HandoffFrontmatterV     = 1
    QueueTriggerSchemaV     = 1
    InboxMessageSchemaV     = 1
)
```

### Startup check

On `fleet` binary start, read the skill's declared version from
`~/.claude/skills/fleet-guard/SKILL.md` frontmatter's `version:` field
(semver). Compare against the binary's embedded matching version:

- **MAJOR mismatch:** refuse to run. Print:
  `fleet binary v<b>.x incompatible with skill v<s>.x. Run \`fleet init\` to reinstall the matching skill.`
- **MINOR mismatch:** warn, continue. Print:
  `fleet binary expects skill v<b>.<n>.x; found v<s>.<m>.x. Consider running \`fleet init\` for optimal compatibility.`
- **PATCH mismatch:** silent.

### Per-file check

When reading any JSON state file, inspect `schema_version`:

- If `> max-known`: skip that file (do not crash the binary). Log an error.
  Increment the TUI `⚡ warning` counter; the warning detail pane lists
  affected file paths and recommends `fleet init`. Exception: if the skipped
  file is a project manifest, the entire project's tasks are hidden from the
  dashboard but the TUI stays up — the operator sees the project name
  greyed out with a `⚡ manifest schema ahead of binary` note.
- If `< max-known`: run the migration chain (`migrateV1ToV2`,
  `migrateV2ToV3`, ...) before using.

Migration functions are scaffolded from v1 day-one even when empty, so
adding v2 later is mechanical, not architectural.

Rationale for skip-not-crash: one future-schema file should not take down
the whole dashboard. Isolating blast radius keeps other agents visible and
gives the operator a clear action (`fleet init`) rather than a panic.

## F1 — Dispatch depth limit

Recursive `fleet dispatch` and `fleet plan` are blocked. When either runs,
it checks `FLEET_AGENT_ID` env:

- **Unset:** dispatching from operator's shell — allowed.
- **Set:** dispatching from inside a supervised Claude Code instance — refused
  with clear message:
  `fleet dispatch: supervised agents cannot spawn children. Use the handoff doc to pass task state to the next agent.`

Rationale: Fleet's thesis is horizontal parallelism (N equivalent agents),
not hierarchy (org chart with delegation). Allowing recursion would make
lineage tracking required and pull Fleet toward SwarmClaw's shape. Easier
to refuse than to do it right.

## F3 — Mode-aware handoff thresholds

Threshold effects depend on `FLEET_MODE`. Modes split into two families:

- **Doing modes** (`execute`, `fix`) — graceful handoff at 40% via
  `MILESTONE` boundary; hard forced handoff at 50%.
- **Thinking modes** (`plan`, `review`) — reminder-only at 40/50;
  operator decides. Claude Code's own `/compact` at ~95% is the
  backstop.

The 40/50 numbers are shared across mode families for operator sanity
(one pair of numbers to remember). Only the enforcement differs.

**Schema.** Every `agents/<id>.json` carries `mode: "plan" | "execute"
| "fix" | "review"` (plus `role: "executor" | "planner"`; the `planner`
role is for project-level chat sessions, which have no thresholds at
all). The fleet binary inspects `mode` when deciding what the Red
threshold does.

### F3a — Graceful handoff at 40% (doing modes)

At 40% context, fleet-guard injects into the agent's next turn:

```
HANDOFF REQUESTED — finalize current milestone (commit if stable),
then the next `MILESTONE` token is your exit.
```

The agent wraps its current bounded work unit (commit, test pass,
sub-task of the plan), emits `MILESTONE` on its own line. fleet-guard
detects the token and triggers the existing handoff sequence
(DESIGN.md "Restart on handoff"): save handoff doc, 3s operator grace,
spawn replacement with handoff pre-loaded.

One token serves two purposes: `MILESTONE` is a progress signal in
normal operation; when `HANDOFF REQUESTED` has been injected, the next
`MILESTONE` is the exit trigger. Same token, context-dependent meaning.
Keeps the CLAUDE.md snippet minimal.

**Rationale for 40% (not 50% or 60%).** Cutting mid-work unit loses
coherent progress. 40% is early enough that the agent has well over
half its context budget still available to finish a milestone (commit,
pass tests, finish a sub-step) before handing off, and keeps the whole
handoff cycle inside the window where model quality is still high.
Aligns with Premise 4's "act at 40%". (Shipped at 50% — the
Hermes/OpenClaw auto-compact threshold — and tightened 2026-09-04; see
DECISIONS.md.)

### F3b — Hard forced handoff at 50% (doing modes)

If context exceeds 50% without `MILESTONE` firing (agent ignored the
queued handoff or couldn't reach a stable boundary), fleet-guard
triggers immediate kill-and-respawn. Same mechanics as the existing
handoff flow, re-purposed as the safety net.

10% runway between graceful queue (40%) and hard force (50%) is
the design's bet that any bounded work unit wraps in under 10% of
context. Larger wraps indicate the task's mini-tasks are too coarse —
operator should re-plan with finer steps.

### F3c — Plan and review: reminder-only at 40/50

`FLEET_MODE=plan` and `FLEET_MODE=review` are thinking modes. The
thresholds fire as operator-facing reminders only:

- 40%: `⚡` warning in the TUI alerts banner
- 50%: `⚠` urgent reminder
- No automatic handoff at any threshold

**Rationale.** Plan-mode sessions accumulate reasoning about a specific
task; review-mode sessions accumulate judgment about a diff. Killing
either mid-thought destroys the thinking we're trying to produce. The
operator is in a better position to decide: handoff the draft, attach
and push to completion, or let Claude Code's own `/compact` handle it.

**Hard backstop.** Fleet cannot prevent Claude Code's internal
compression; `/compact` applies inside any mode. Fleet's guarantee is
only that Fleet itself doesn't force the kill-and-respawn in thinking
modes. The mode-aware TUI reminders give the operator enough warning
to act before Claude's own limit.

## F5 — Post-execute review loop

Every task passes through a two-round review loop between executor
completion and `done` status. The loop is Fleet-orchestrated (not
agent-initiated) because F1 depth limits prevent agents from spawning
agents.

**Phases within `status: doing`.** Manifest task field `phase`:

- `execute` — initial work (exits with `READY FOR REVIEW`)
- `review-1` — fresh Claude reviews diff (exits with `REVIEW COMPLETE`)
- `fix-1` — only if review-1 had comments; addresses them (exits with `FIXES COMPLETE`)
- `review-2` — second pass on updated diff
- `fix-2` — only if review-2 had comments
- `done` — after round 2 completes (with or without fix-2)

Status stays `doing` throughout the loop. Only `phase` changes.

**Review scope.** Full diff against `main`:
`git fetch origin main && git diff origin/main..HEAD`. Reviewer sees
the task's complete change against the merge base, not just the
executor's commits. Catches interactions with prior commits.

**Reviewer skill selection.** The reviewer's CLAUDE.md snippet detects
whether `codex` is installed:

```bash
if command -v codex >/dev/null 2>&1; then
    # Invoke /codex review — independent model, second opinion
else
    # Invoke /review — Claude Code's built-in review skill
fi
```

`codex` gives adversarial independent-model review where available.
`/review` is the fallback.

**Reviewers and fix agents: role + mode split.**

| Phase    | `FLEET_ROLE` | `FLEET_MODE` | Threshold family |
|----------|--------------|--------------|------------------|
| execute  | executor     | execute      | doing (40%/50%)  |
| review-N | executor     | review       | thinking (40/50 reminder) |
| fix-N    | executor     | fix          | doing (40%/50%)  |

Review is "thinking" (reminder-only); fix is "doing" (graceful + emergency).

**Reviewers do NOT count against `max_concurrent_agents`.** Review is
a serial follow-on to execute, not parallel execution. Counting them
would starve other project slots during review.

**Task file accumulates review sections.** In-repo task file grows:

```markdown
## Context
## Plan
## Progress
## Review Round 1  (written by review agent)
## Review Round 2  (written by review agent)
```

Fix agents append summaries to `## Progress`, not a separate section.

**Escape hatch.** `--review-rounds=N` flag on `fleet dispatch`. Default
`2`. `0` skips the loop for trivial changes (typos, version bumps).

## F4 — Plan-mode Q&A loop

Plan-mode agents may pause and ask the operator questions when `##
Context` is insufficient. Mechanics:

1. Agent writes a `## Planner Questions` section to
   `<repo>/tasks/<slug>.md`, appending after `## Context`.
2. Agent sets `needs_input: true` in its `agents/<id>.json`.
3. Agent stops active processing (no `PLAN COMPLETE`, no further writes
   to `## Plan`). The tmux session stays alive; the Claude process is
   idle waiting for the next user turn.

The TUI surfaces the pause as `✏ needs input` in the alerts banner and
on the task row, using the same channel designed for A4-style attention
states.

**Answer paths (two, operator's choice):**

- **Attach.** `[a]ttach <agent>` drops the operator into the tmux
  session. Operator types answers directly. Claude consumes the next
  user turn and resumes planning. `needs_input` clears on the next
  health JSON write.
- **Edit the task file.** Operator edits `## Planner Questions` in
  `$EDITOR`, writes answers inline under each question, saves. The
  fleet-guard skill watches `<repo>/tasks/` via fsnotify. On detected
  write, it injects into the agent's next turn: *"Operator answered
  your questions in the task file. Re-read it and continue the plan."*

**Explicit wake fallback.** `fleet plan --continue <task>` writes a
trigger to `~/.fleet/queue/resume-<agent-id>.json` that fleet-guard
reads on next poll; same effect as fsnotify delivery. Use when fsnotify
misses (macOS rename-event flakiness).

**State machine.** `needs_input` is a sub-state of `planning`, not a
new top-level task status. The status flag on the manifest stays
`planning` throughout; only `needs_input` in the agent JSON toggles.

## F2 — Supervised-agent guardrails

Two layers, with a role-aware exception for planner sessions.

**Role env.** Fleet spawns every tmux session with `FLEET_ROLE` set:

- `FLEET_ROLE=executor` — default for `fleet dispatch`. The fleet-guard
  skill is loaded. F2 refuses all mutating subcommands.
- `FLEET_ROLE=planner` — set by `fleet chat`. The fleet-planner skill
  is loaded instead of fleet-guard. The binary allows `fleet-sync`
  writes to `~/.fleet/queue/` via the skill, but still refuses `fleet
  dispatch`, `fleet handoff`, `fleet msg`, `fleet broadcast`,
  `fleet plan`. Planner sessions propose; they do not dispatch.

Both roles set `FLEET_AGENT_ID`, so F1 (depth limit) applies to both:
neither role can spawn children.

### L1 — Prompt guardrail (fleet-guard skill)

The skill's injected CLAUDE.md snippet includes:

```
You are working inside Fleet-supervised Claude Code session.
Do NOT run `fleet plan`, `fleet dispatch`, `fleet handoff`, `fleet msg`,
`fleet broadcast`, `fleet tasks`, or any other mutating Fleet subcommand.
Those commands belong to the operator, not to you.

If you need to communicate state to your successor, write it into the task
file's Progress section or into the handoff doc (fleet-guard handles
the doc mechanics when context fills).

`fleet status` and `fleet peek` are allowed for read-only introspection.
```

### L2 — Binary refusal

`fleet` binary, at startup, checks `FLEET_AGENT_ID`:

```go
if os.Getenv("FLEET_AGENT_ID") != "" {
    if !isReadOnlySubcommand(subcmd) {
        fmt.Fprintf(os.Stderr,
            "fleet: supervised agents cannot run `%s`. Allowed: status, peek, version.\n",
            subcmd)
        os.Exit(2)
    }
}
```

Allowlist: `status`, `peek`, `version`, `--help`. Everything else refused.

Both layers matter — the prompt keeps well-behaved agents from trying, the
binary stops mis-behaving ones from succeeding.

## Schemas (canonical v1 shapes)

### agents/<id>.json

```json
{
  "schema_version": 1,
  "id": "a1b2",
  "pid": 84217,
  "tmux_session": "fleet-a1b2",
  "engine": "claude-code",
  "role": "executor",
  "mode": "execute",
  "task_id": "auth-token-refresh",
  "project": "rainier",
  "review_round": null,
  "context_pct": 43,
  "context_source": "hook",
  "last_activity_ts": "2026-04-18T19:22:14Z",
  "blocked": false,
  "blocked_reason": null,
  "blocked_since": null,
  "needs_input": false,
  "inbox_pending": false,
  "handoff_type": null,
  "spawned_at": "2026-04-18T18:15:02Z"
}
```

- `role`: `"executor" | "planner"`. Planner is for project-level chat
  sessions (Phase 2 FLOW.md); executor covers execute/plan/fix/review.
- `mode`: `"execute" | "plan" | "fix" | "review"` when `role=executor`;
  `null` when `role=planner`. Drives the threshold family (doing vs
  thinking) per F3.
- `review_round`: `1 | 2 | null`. Populated when `mode` is `review` or
  `fix`; `null` otherwise.
- `engine`: which agent runtime spawned this process. v1 only writes
  `"claude-code"`. Field exists from day one so v1.1 can add a second
  engine (e.g. `"codex"`) without a schema migration. Forward-compat
  rule: a missing `engine` field defaults to `"claude-code"` so v1.1
  readers handle v1-era archived records without touching them. The
  Fleet binary reads this only to look up the spawn command in
  `config.yaml:engines.<engine>` and to route engine-specific behavior
  (currently none). See `docs/DECISIONS.md` 2026-04-26 entry "v1.1
  engine adapter — minimal v1 hooks".

### projects/<name>.yaml

```yaml
schema_version: 1
name: rainier
repo: /Users/edison/projects/rainier
auto_spawn: true
max_concurrent_agents: 2
review_rounds: 2                          # default; per-task override allowed
engine: claude-code                       # default engine for new agents in this project; v1 only writes claude-code
tasks:
  - id: auth-token-refresh
    status: doing                         # todo | planning | planned | queued | doing | blocked | unhealthy | done
    phase: execute                        # execute | review-1 | fix-1 | review-2 | fix-2 (only when status=doing)
    priority: 1
    current_agent: a4
    handoff_count: 3
    review_rounds: 2                      # optional per-task override; inherits project default
    review_rounds_completed: 0            # incremented on REVIEW COMPLETE
    created: 2026-04-15
    task_file: tasks/auth-token-refresh.md
    spawn_history:
      - 2026-04-15T10:00:00Z
      - 2026-04-15T12:15:00Z
      - 2026-04-15T14:32:00Z
  # Completed task — fields present after agent emits REVIEW COMPLETE
  - id: fix-logging-format
    status: done
    priority: 3
    current_agent: null
    handoff_count: 0                      # resets on completion (live count, not historical)
    review_rounds_completed: 2
    created: 2026-04-14
    completed: 2026-04-15                 # UTC date
    last_commit: 4f8a2c1                  # short SHA from `git rev-parse HEAD` at done
    # completion_source: operator         # only present if marked done via [shift]+[d]
    # previous_last_commit: <sha>         # only present after revert via [shift]+[u]
    task_file: tasks/fix-logging-format.md
  # Unhealthy task — fields present when A4 budget exhausted
  - id: cleanup-task
    status: unhealthy
    priority: 4
    current_agent: null
    handoff_count: 0
    unhealthy_reason: "3 crashes in 1h · last: skill load fail"
    spawn_history: [...]                  # operator clears via `fleet tasks unblock`
  # Queued task — capacity exceeded, dispatch deferred
  - id: refactor-storage
    status: queued                        # cap reached; will auto-promote when slot frees
    priority: 5
    current_agent: null
    queued_at: 2026-04-19T14:08:33Z       # tracks how long it's been waiting
```

**Status state machine:**

```
todo ──[plan]──▶ planning ──▶ planned
                                │
                                ▼
                              [dispatch]
                                │
   queued ◀──[cap reached]──────┤
     │                          │
     └──[slot frees]──▶ doing ──▶ done
                          │
              ┌───────────┼──────────┐
              ▼           ▼          ▼
           blocked    unhealthy    (any state can be reverted to todo)
```

`queued` is a real persisted status, not a transient flag — it must
survive TUI restart. Auto-promotion happens when an active agent in the
project completes, hands off, or is archived, freeing a
`max_concurrent_agents` slot.

### handoffs/<agent-id>-<utc-iso>-<uuid>.md frontmatter

```yaml
schema_version: 1
agent_id: a1
successor_hint: a2                    # pre-assigned; null if unknown at write time
task_id: auth-token-refresh
project: rainier
role: executor
mode: execute                         # execute | plan | fix | review (role=executor); null for planner role
phase: execute                        # matches manifest task `phase` at handoff time
context_pct_at_handoff: 52
handoff_type: graceful                # graceful | emergency | operator
previous_handoff: ~/.fleet/handoffs/a1-20260415T140000Z-abc12345.md
handoff_number: 3
timestamp: 2026-04-15T14:32:00Z
```

### queue/spawn-fresh-<id>.json

```json
{
  "schema_version": 1,
  "event": "spawn_fresh",
  "task_id": "auth-token-refresh",
  "project": "rainier",
  "from_handoff": "~/.fleet/handoffs/a1-20260415T143200Z-ef9a.md",
  "created_at": "2026-04-15T14:32:05Z"
}
```

### inbox/<id>.md

```markdown
---
schema_version: 1
from: operator
timestamp: 2026-04-15T14:10:00Z
---
Staging Redis connection string is redis://staging-redis.internal:6379/0.
Not in vault yet; hardcode for now.
```

## Notes for implementers

- Keep progress JSONL events under 1KB each. Append is atomic only up to
  `PIPE_BUF` (512B-4KB depending on platform). Large event payloads need
  a separate mechanism.
- Don't hold flock while calling out to tmux; tmux can block and a held
  flock blocks everyone else. Read manifest, release lock, then spawn.
- The 40% / 50% thresholds: shipped at 50%/70% (50% validated against
  prior art — Hermes Agent's `ContextCompressor` auto-compacts at 50%,
  OpenClaw at similar-order thresholds; 70% was Fleet's own emergency
  runway). Tightened to 40%/50% on 2026-09-04 by operator directive:
  graceful at 40%, hard force at 50% (10% runway). See DECISIONS.md.
- Agents are peers, not children. No ID parent/child tracking in v1.
  `previous_handoff` in frontmatter is enough to reconstruct a task's agent
  chain for human-readable timelines.
- fsnotify on macOS is flaky for rename events in some scenarios. Always
  pair fsnotify with a 1s polling fallback for correctness; fsnotify is
  latency optimization, not a correctness primitive.
