# Week 4b + 4c implementation plan — fleet-guard skill + auto-handoff

**Status:** approved 2026-04-28, not started
**Predecessor:** Week 4a (PR #12, merged 2026-04-28) — operator-triggered manual handoff
**Successor:** Week 5 release (GoReleaser, brew tap, demo gif)

---

## What this PR delivers

Skill-driven auto-handoff. Agent's context fills → Claude Code PostResponse hook fires → fleet-guard skill detects threshold → skill triggers handoff via the same `~/.fleet/queue/` primitive built in 4a → fleet drains → replacement spawns with chain semantics preserved.

End state: operator no longer watches `/context` by hand. The fleet really is "self-healing." Plus TUI keybinds (`[h]andoff`, `[a]ttach`, `[d]ispatch`, `[n]ew-task`) so handoff is one keystroke from the dashboard.

This is the missing v0.1 piece. After this lands, Week 5 (release) is GoReleaser/brew/demo-gif only — no functional gaps left.

## Decisions (locked 2026-04-28)

| ID | Decision | Choice |
|---|---|---|
| D1 | Skill language | **Python** (stdlib JSON, ~50ms startup acceptable) |
| D2 | How skill injects `HANDOFF REQUESTED` | **Hook stdout** (PostResponse stdout-as-injection confirmed) |
| D3 | How skill populates handoff doc body | **Capture-and-dump tmux pane** into `## Recent activity` section. Five-section structure (Completed/Decisions/Files/Open/Next) becomes operator-fillable placeholders for first iteration. |
| D4 | Drain consumer | **TUI fsnotify on `queue/`** (deferred from 4a) **+ new `fleet drain` standalone command** for non-TUI operators. |
| D5 | TUI keybinds | **Bundle into this PR** (no defers — operator's "no defers" override of original recommend-defer). |
| D6 | Skill testing | **pytest** with mocked filesystem + hook payload fixtures. Same discipline as 4a's Go tests. |

## Skill installation model

- **Source of truth:** `skills/fleet-guard/` inside fleet repo (versioned with binary).
- **Install destination:** `~/.claude/skills/fleet-guard/` (user-scoped, not project-scoped — matches DESIGN.md line 316).
- **Mechanism:** new `fleet init` subcommand. Skill files embedded into Go binary via `//go:embed`, written to `~/.claude/skills/fleet-guard/` on init. Single-binary distribution preserved.
- **Why user-scoped:** agents run in different cwds (per-project repos), but one operator runs one Claude Code installation. Skill must be loadable regardless of cwd.

## Package layout

```
skills/fleet-guard/                    NEW — Python skill, embedded in binary
├── SKILL.md                           hook registration + skill manifest
├── main.py                            PostResponse + PreCompact hook entry
├── health.py                          context_pct read, agent record write
├── handoff.py                         tmux pane grep, doc write, queue write
├── inbox.py                           operator → agent message relay
├── ids.py                             new ID allocator (matches agent.NewID)
└── tests/
    ├── test_health.py                 pytest, mock tmux + fs
    ├── test_handoff.py                MILESTONE detection, doc rendering
    └── test_inbox.py                  inbox file parsing

cmd/fleet/init.go                      NEW — `fleet init` subcommand
cmd/fleet/drain.go                     NEW — `fleet drain` subcommand
internal/embed/skill.go                NEW — //go:embed for skill files
internal/handoffop/handoffop.go        NEW — extracted from cmd/fleet/handoff.go
                                       so internal/tui can reuse the orchestrator
internal/tui/tui.go                    EXTEND — fsnotify on queue/, drain on event
internal/tui/model.go                  EXTEND — [h]/[a]/[d]/[n] keybinds
internal/tui/keys.go                   NEW — keybind handlers
```

## File-by-file breakdown

### `skills/fleet-guard/SKILL.md` (~80 lines)

YAML frontmatter declaring:
- `name: fleet-guard`
- `version: 0.1.0`
- `hooks: [PostResponse, PreCompact]`
- `entry: python3 main.py`
- Required env: `FLEET_HOME` (defaults `~/.fleet/`), `FLEET_AGENT_ID` (set by spawn.go)
- Required tools: `tmux` (capture-pane)

Body documents the hook contract:
- PostResponse: stdin = JSON payload with `usage.input_tokens`, `usage.output_tokens`, `model`. Stdout (if non-empty) injects into next turn.
- PreCompact: stdin = same JSON shape minus token deltas. Stdout ignored.

### `skills/fleet-guard/main.py` (~150 lines)

Hook entry point. `if __name__ == '__main__'`:
1. Parse stdin JSON payload.
2. Detect hook type from `payload['hook']` (or env var if Claude Code uses that).
3. Dispatch:
   - PostResponse: `health.update(payload)` → `inbox.deliver(payload)` → `handoff.maybe_trigger(payload)`
   - PreCompact: `handoff.emergency_trigger(payload)` (skip MILESTONE wait, treat as Red)
4. Each step prints injection text to stdout if any (concatenated in order: inbox first, then handoff).

Errors logged to stderr, never raised — a crashing skill must NOT block the agent's turn.

### `skills/fleet-guard/health.py` (~80 lines)

```python
def read_context_pct(payload: dict) -> float | None:
    """Extract context_pct from hook payload. Returns None if unavailable."""

def update_record(agent_id: str, *, context_pct: float, blocked: bool = False,
                  needs_input: bool = False) -> None:
    """Atomic write to ~/.fleet/agents/<id>.json. Reads existing record,
    updates context_pct + last_activity_ts + flags, writes via .tmp + rename.
    Mirrors agent.Record schema (matches SchemaVersion=1)."""

def threshold(context_pct: float) -> str:
    """Returns 'red' (>=70), 'yellow' (>=50), or 'green' (<50)."""
```

### `skills/fleet-guard/handoff.py` (~250 lines) — the meat

```python
HANDOFF_REQUESTED = "HANDOFF REQUESTED"
MILESTONE = "MILESTONE"

def is_handoff_pending(agent_id: str) -> bool:
    """Read agent record; True if HandoffType is set (waiting for MILESTONE)."""

def inject_handoff_requested() -> str:
    """Returns the injection text. Caller (main.py) prints to stdout."""

def find_milestone(session: str) -> bool:
    """Run `tmux capture-pane -t <session> -p`; grep for ^MILESTONE$ on a line."""

def capture_recent(session: str, lines: int = 200) -> str:
    """Capture last N lines of pane. Strips ANSI escape codes (basic regex —
    polish later)."""

def write_doc(*, agent_id: str, doc_path: str, body: str, ctx_pct: float | None,
              prev_path: str | None, number: int, handoff_type: str,
              task_id: str, project: str, ts: datetime) -> None:
    """Writes handoff doc with frontmatter + body. Frontmatter shape MUST
    match what cmd/fleet/handoff.go writes for operator-triggered handoffs
    (so resume probe + future readers don't have to discriminate)."""

def write_queue(*, old_id: str, new_id: str, doc_path: str,
                task_id: str, project: str) -> str:
    """Writes ~/.fleet/queue/spawn-fresh-<old>.json. NewAgentID set so
    crash recovery probe in fleet handoff knows the successor."""

def maybe_trigger(payload: dict) -> str | None:
    """Orchestrator. Returns injection text or None.
    - context_pct >= 70 (Red, doing modes): emergency. Write doc + queue
      immediately. No MILESTONE wait. Returns None (no injection).
    - context_pct >= 50 AND not is_handoff_pending: inject HANDOFF REQUESTED,
      mark pending in record. Returns the injection.
    - is_handoff_pending: grep tmux pane for MILESTONE. If found, write
      doc + queue. Returns None.
    - context_pct < 50 AND not pending: noop. Returns None.
    - Thinking modes (plan, review): emit '⚡' (50%) or '⚠' (70%) reminder
      via inbox archive (TUI banner picks up). Never auto-trigger.
    """

def emergency_trigger(payload: dict) -> None:
    """PreCompact hook path: write doc + queue immediately. No threshold
    check. The compaction is about to happen; we save what we can."""
```

### `skills/fleet-guard/inbox.py` (~50 lines)

```python
def read_pending(agent_id: str) -> str | None:
    """Returns ~/.fleet/inbox/<id>.md content if present, else None."""

def deliver(content: str) -> str:
    """Returns the injection text wrapped in a marker so the agent knows
    it's an operator message: '[OPERATOR] <content>'."""

def archive(agent_id: str) -> None:
    """Move ~/.fleet/inbox/<id>.md → ~/.fleet/inbox/archive/<id>-<ts>.md."""
```

### `skills/fleet-guard/ids.py` (~10 lines)

```python
import secrets
def new_id() -> str:
    """Returns 8 lowercase hex chars, matching agent.NewID() in Go."""
    return secrets.token_hex(4)
```

### `cmd/fleet/init.go` (~80 lines)

```go
func newInitCmd() *cobra.Command {
    cmd := &cobra.Command{
        Use: "init",
        Short: "Install fleet-guard skill into ~/.claude/skills/",
        ...
    }
    cmd.Flags().Bool("force", false, "overwrite existing skill files")
    return cmd
}

func runInit(force bool, stdout io.Writer) error {
    // Walks internal/embed.SkillFS().
    // For each file, computes destination ~/.claude/skills/fleet-guard/<rel>.
    // Mkdir intermediate dirs.
    // If destination exists: skip + warn (or overwrite if --force).
    // Write file with mode 0644 (or 0755 if .py / .sh).
    // Print summary.
}
```

### `cmd/fleet/drain.go` (~120 lines)

```go
func newDrainCmd() *cobra.Command { ... }

func runDrain(stdout, stderr io.Writer) error {
    // queue.ListPending() returns []path
    // For each path:
    //   queue.ReadSpawnFresh(path) → req
    //   acquire state.LockAgent(req.OldAgentID)
    //   call handoffop.Resume(req) — same logic as resumeHandoff in handoff.go
    //   release lock
    // Print one summary line per processed file.
}
```

### `internal/embed/skill.go` (~20 lines)

```go
package embed

import (
    "embed"
    "io/fs"
)

//go:embed skills/fleet-guard
var skillFS embed.FS

func SkillFS() fs.FS {
    sub, _ := fs.Sub(skillFS, "skills/fleet-guard")
    return sub
}
```

(The `//go:embed` path is relative to the .go file. Will need to adjust to project layout — possibly the embed directive lives in cmd/fleet/init.go to keep paths simple.)

### `internal/handoffop/handoffop.go` (~150 lines, mostly extracted)

Move the body of `cmd/fleet/handoff.go::runHandoff` into `handoffop.Run(opts handoffop.Options) error`. Same logic, just package-importable. Also extract `resumeHandoff` → `handoffop.Resume`.

`cmd/fleet/handoff.go` shrinks to just the cobra wiring + a thin wrapper calling `handoffop.Run`.

`cmd/fleet/drain.go` and `internal/tui/keys.go` both import `handoffop`.

### `internal/tui/tui.go` extension (~50 lines)

Extend `startWatcher()` to add a second fsnotify path: `~/.fleet/queue/`. On any event:
- Send `queueEventMsg{}` into the bubbletea program.
- Model's update loop dispatches `runDrain` as a `tea.Cmd` (background).
- Drain results refresh the agent list (existing fsEventMsg path picks up the new agent).

### `internal/tui/model.go` + `internal/tui/keys.go` extension (~150 lines)

Keybind handlers:

- **`[h]` — handoff selected agent.** Calls `handoffop.Run(...)` inside a `tea.Cmd`. On completion, refreshes agent list. Failure → flash banner with error.

- **`[a]` — attach.** Tricky: `tmux attach` replaces the current process. Solution:
  1. Set `model.pendingAttach = session`
  2. Return `tea.Quit` cmd
  3. In `tui.Run()`, after `prog.Run()` returns, check the returned model. If `pendingAttach` is set, exec `tmux attach -t <session>` (operator returns to TUI manually after).

- **`[d]` — dispatch.** Opens a textinput modal for task ID. On submit, calls into `internal/dispatchop` (similar extraction needed from `cmd/fleet/dispatch.go`). Or just shell out to `fleet dispatch <task>` — simpler, less refactor.

- **`[n]` — new task.** Same as `[d]` for v1; could differentiate later.

## Test plan

### Skill (Python, pytest)

`skills/fleet-guard/tests/test_health.py`:
- `read_context_pct` returns expected float from sample payloads
- `read_context_pct` returns None on missing fields
- `update_record` creates new record with all required fields
- `update_record` preserves existing fields not being modified
- `threshold` boundaries (0, 49.99, 50, 69.99, 70, 100)

`skills/fleet-guard/tests/test_handoff.py`:
- `find_milestone` positive (MILESTONE on its own line)
- `find_milestone` negative (MILESTONE in middle of word, e.g. "MILESTONES")
- `capture_recent` strips ANSI codes
- `write_doc` produces YAML matching Go's handoff.Render output (golden test)
- `write_queue` writes valid JSON matching queue.SpawnFresh schema
- `maybe_trigger` Yellow path: injects + marks pending
- `maybe_trigger` Red path: writes doc + queue immediately, no inject
- `maybe_trigger` pending + MILESTONE: writes doc + queue
- `maybe_trigger` pending + no MILESTONE: noop, returns None
- `emergency_trigger`: writes doc + queue regardless of threshold

`skills/fleet-guard/tests/test_inbox.py`:
- `read_pending` returns content if file present
- `read_pending` returns None if absent
- `archive` moves file with correct timestamp suffix

### Go extensions

`cmd/fleet/init_test.go`:
- Sets HOME=tempdir, runs runInit(force=false), asserts all skill files present at ~/.claude/skills/fleet-guard/
- Re-runs runInit, asserts no overwrite (file mtime unchanged)
- Re-runs with force=true, asserts files overwritten
- Skill file content matches embedded source (byte-equal)

`cmd/fleet/drain_test.go`:
- Seed two queue files (different agents). runDrain. Both processed, both deleted.
- Seed queue file referencing nonexistent agent. runDrain. File deleted with warning.
- Reuses fixtures from cmd/fleet/handoff_test.go where possible.

`internal/tui/keys_test.go`:
- Mock model state with selected agent. Press `[h]`. Assert handoffop.Run called with correct opts.
- Press `[a]`. Assert pendingAttach set, tea.Quit returned.
- Press `[d]`. Assert textinput state activated.
- Unknown key. Assert noop.

### End-to-end (manual smoke test, step 11)

```bash
# Build + install skill
go install ./cmd/fleet
fleet init                            # writes skill to ~/.claude/skills/fleet-guard/

# Start an agent in a sandbox
export FLEET_HOME=/tmp/fleet-4bc-test
export FLEET_TMUX_SOCKET=/tmp/fleet-4bc.sock
fleet dispatch real-task --project myrepo --cwd ~/projects/myrepo

# Attach, work normally, watch context fill
fleet attach <id>
# In Claude session, do work that grows context. When you cross 50%:
#   - Skill should inject "HANDOFF REQUESTED" on next turn
#   - Agent should wrap with MILESTONE on its own line
#   - Skill writes doc + queue
#   - TUI fsnotify (or `fleet drain`) processes queue
#   - Replacement spawns; old session exits

# Verify in another terminal
fleet status                          # new agent visible
ls /tmp/fleet-4bc-test/handoffs/      # auto-handoff doc present, body has "Recent activity"
ls /tmp/fleet-4bc-test/queue/         # empty (drained)
```

## Implementation order

1. `skills/fleet-guard/SKILL.md` + frontmatter
2. `skills/fleet-guard/health.py` + tests
3. `skills/fleet-guard/handoff.py` + tests (the meat — biggest single chunk)
4. `skills/fleet-guard/inbox.py` + tests
5. `skills/fleet-guard/main.py` + tests (orchestrator)
6. `internal/embed/` + `cmd/fleet/init.go` + tests
7. `cmd/fleet/drain.go` + tests
8. `internal/handoffop/` extraction (refactor handoff.go)
9. `internal/tui/keys.go` + model wiring + tests
10. `internal/tui/tui.go` queue fsnotify
11. End-to-end smoke test
12. `/review` loop until clean (target: 0 critical, 0 medium)
13. `/codex review` loop until P0/P1 clean (or B-policy: stop unless P0/P1 after first iteration)
14. `/ship` → PR

## Estimate

- Production code: **~1100 lines** (Python ~500, Go ~600)
- Tests: **~600 lines**
- Commits: **~12-15**, single PR
- Claude Code time: **~4-6 hours** (more than 4a; Python+Go split adds friction; TUI interactivity is fiddly; codex iterations on the skill will find Python-specific edge cases we haven't seen yet)

## Risks / things to watch

1. **Tmux pane capture quality.** `tmux capture-pane -p` includes ANSI codes, partial output, prompt lines. Body will be ugly first iteration. Polish (strip ANSI, find natural section boundaries) is iteration loop, not blocking.

2. **Hook payload schema.** I'm assuming `payload['usage']['input_tokens']` per Claude Code's hook spec. If the actual key path differs (or context_pct is exposed directly as a percentage), the skill code will need adjustment in iteration 1.

3. **TUI attach flow.** Bubbletea + tmux-attach is awkward (process replacement). The "set pendingAttach + tea.Quit + exec after Run() returns" pattern may have edge cases (terminal state restoration, signal handling). May need iteration.

4. **MILESTONE convention.** Agent has to actually emit `MILESTONE` on its own line for the Yellow path to fire. If agents ignore the HANDOFF REQUESTED injection, handoff doesn't trigger and the agent eventually hits Red (emergency path). Acceptable failure mode.

5. **Embed path resolution.** `//go:embed skills/fleet-guard` must be relative to the .go file. May need a `//go:embed` in cmd/fleet (project-root-adjacent) and pass the FS to internal/embed for re-export.

6. **Skill testing in CI.** Adds a Python test step to CI. CI must have python3 + pytest available. May need a `pip install` in the workflow.

## What ships AFTER this PR (Week 5)

- GoReleaser config
- Brew tap setup
- Demo gif (operator dispatches 3 agents, watches one auto-handoff in real-time)
- Tag v0.1.0
