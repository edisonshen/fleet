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
agent JSON on deploy), the manifest is the leader and updated last under
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

Two concurrent `fleet deploy rainier` invocations would race to pick the
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

Lock scope: per-project. Two operators running `fleet deploy rainier` in
different shells serialize; `fleet deploy rainier` and `fleet deploy
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
tmux pane grep for "HANDOFF COMPLETE" strings. No control decisions derived
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
- **On budget exhaustion:** set task `status: unhealthy` in the manifest.
  TUI surfaces `⚠ Task X unhealthy: 3 crashes in 1h`. Operator must clear
  via `fleet tasks unblock <project>/<task>` before auto-spawn resumes.
- **State lives** in the manifest itself: per-task `spawn_history: [<ts>,
  <ts>, ...]`, pruned to the last hour on each read.

**Observed crash signals:** agent exits within 60s of spawn OR tmux session
not found 30s after `fleet deploy` returned OR health JSON never written.

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

- If `> max-known`: error, refuse to process that file.
- If `< max-known`: run the migration chain (`migrateV1ToV2`,
  `migrateV2ToV3`, ...) before using.

Migration functions are scaffolded from v1 day-one even when empty, so
adding v2 later is mechanical, not architectural.

## F1 — Deploy depth limit

Recursive `fleet deploy` is blocked. When `fleet deploy` runs, it checks
`FLEET_AGENT_ID` env:

- **Unset:** deploying from operator's shell — allowed.
- **Set:** deploying from inside a supervised Claude Code instance — refused
  with clear message:
  `fleet deploy: supervised agents cannot spawn children. Use the handoff doc to pass task state to the next agent.`

Rationale: Fleet's thesis is horizontal parallelism (N equivalent agents),
not hierarchy (org chart with delegation). Allowing recursion would make
lineage tracking required and pull Fleet toward SwarmClaw's shape. Easier
to refuse than to do it right.

## F2 — Supervised-agent guardrails

Two layers:

### L1 — Prompt guardrail (fleet-guard skill)

The skill's injected CLAUDE.md snippet includes:

```
You are working inside Fleet-supervised Claude Code session.
Do NOT run `fleet deploy`, `fleet handoff`, `fleet msg`, `fleet broadcast`,
`fleet tasks`, or any other mutating Fleet subcommand. Those commands
belong to the operator, not to you.

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
            "fleet: supervised agents cannot run `%s`. Allowed: status, peek.\n",
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
  "task_id": "auth-token-refresh",
  "project": "rainier",
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

### projects/<name>.yaml

```yaml
schema_version: 1
name: rainier
repo: /Users/edison/projects/rainier
auto_spawn: true
max_concurrent_agents: 2
tasks:
  - id: auth-token-refresh
    status: doing
    priority: 1
    current_agent: a4
    handoff_count: 3
    created: 2026-04-15
    task_file: tasks/auth-token-refresh.md
    spawn_history:
      - 2026-04-15T10:00:00Z
      - 2026-04-15T12:15:00Z
      - 2026-04-15T14:32:00Z
```

### handoffs/<agent-id>-<utc-iso>-<uuid>.md frontmatter

```yaml
schema_version: 1
agent_id: a1
task_id: auth-token-refresh
project: rainier
context_pct_at_handoff: 72
handoff_type: normal                  # normal | emergency_precompact | operator_triggered
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
- The 50% / 75% thresholds are not guesses — Hermes Agent's
  `ContextCompressor` independently picks 50% for auto-compaction, OpenClaw
  auto-compacts at similar-order thresholds. Prior art validates the call.
- Agents are peers, not children. No ID parent/child tracking in v1.
  `previous_handoff` in frontmatter is enough to reconstruct a task's agent
  chain for human-readable timelines.
- fsnotify on macOS is flaky for rename events in some scenarios. Always
  pair fsnotify with a 1s polling fallback for correctness; fsnotify is
  latency optimization, not a correctness primitive.
