# Fleet

> **Everyone is a manager.**

An open-source command console for running many Claude Code agents in parallel. One operator, many concurrent agents across many repos, one TUI to keep them all productive.

![Fleet dashboard](assets/dashboard.png)

Four agents, four repos, four states — coding, blocked on you, asking, reviewing. One operator. That is the moment Fleet exists for. ([Phase 7 spec](docs/FLOW.md#phase-7--multi-task-parallel))

## Status

**v0.1.3 shipped to brew** ([releases](https://github.com/edisonshen/fleet/releases)). The Week 0 spike resolved (Stop hooks deliver per-session context % at low latency, see [`docs/SPIKE-context-pct.md`](docs/SPIKE-context-pct.md)) and the v1 surface — TUI, dispatch / attach / handoff / drain, fleet-guard auto-handoffs at 50% (graceful) and 70% (emergency) — is live. We're dogfooding through Week 6 and shipping bug-fix patches behind the scenes; expect frequent `brew upgrade edisonshen/tap/fleet`.

**v0.2 in flight on `main`.** Adds a per-project autonomous coordinator (see below) plus task / learnings / standards primitives. See [`CHANGELOG.md`](CHANGELOG.md) for the full v0.2.0 entry; tag-and-ship pending Week 6 dogfood.

The full design is at [`docs/DESIGN.md`](docs/DESIGN.md).

## What v0.2 adds — the coordinator

Fleet v0.1 is the operator → agent surface. v0.2 adds an autonomous layer that sits between you and the dispatch loop: a **per-project coordinator** that owns `tasks.md`, dispatches workers under `fleet dispatch`, monitors PR / CI via `gh`, and only raises a hand to the operator when human input is genuinely needed.

You write a one-line task; the coordinator picks it up on its next tick, generates a slug, dispatches a worker into a fresh tmux session, watches the worker's `state.json` heartbeat, and reconciles status off `gh pr checks` once a PR appears. No daemon — each tick is a single process under an NB-flock, so restart equals resume.

```sh
$ fleet tasks add "fix the auth retry bug"
queued: auth-retry-bug-7c12

# coordinator's next tick (skill auto-runs from a Claude Code hook):
# - dispatches fleet worker for slug=auth-retry-bug-7c12
# - flips status: ready -> in-progress
# - records worker pid + branch in tasks.md

$ fleet peek auth-retry-bug-7c12 --follow
slug: auth-retry-bug-7c12
status: in-progress
phase: tdd-red
pid: 41218
heartbeat: 3s ago
```

When the worker pushes a branch and opens a PR, the coordinator transcribes the PR URL onto `tasks.md`, polls `gh pr checks` on subsequent ticks, and flips the row to `done` once CI is green and the PR merges. If CI goes red, it clears `pr_url` and re-queues the task for a retry. C1 (handoff preserves in-flight) and C2 (parallel worker status reports never mix) are integration-tested in `cmd/fleet/coordinator_integration_test.go`.

One coordinator per project (NB-flock on `coordinator.lock`); single-worker mode by default in v0.2; cap > 1 with worktrees lands in v0.2.x.

The end-to-end engagement — discuss → split → task list → implement → PR-track → done — is documented in [`docs/COORDINATOR-WORKFLOW.md`](docs/COORDINATOR-WORKFLOW.md). Read it before running your first coord.

## Why

The bottleneck running multiple Claude Code agents is not Claude. It is the operator. Every context switch costs minutes of re-onboarding. Run four agents naively and you spend more time re-engaging than supervising.

Fleet treats supervision capacity as the constraint and optimizes everything around it: a single TUI shows every agent's health (context %, last activity, blocked state, pending question), automatic handoffs prevent context-degraded decisions, and a structured handoff format keeps the next agent productive within seconds.

## What ships in v1

Three pillars launched together. The parallelism demo only carries with the full picture, so it is one big-bang launch, not a trickle.

1. **Fleet view** — TUI showing every agent across every project with health badges, banner aggregation, and one-keystroke attach.
2. **Dispatch / attach / handoff / drain** — full operator → agent surface, plus an async inbox at `~/.fleet/inbox/<id>.md` that doesn't interrupt a turn.
3. **Context-guard** — the [`fleet-guard`](skills/fleet-guard/SKILL.md) Claude Code skill watches context % and triggers structured handoffs at **50% (graceful) / 70% (emergency)**. Same numbers across executing and thinking modes; only the enforcement differs. See [`docs/DECISIONS.md`](docs/DECISIONS.md#threshold-revision-5070-across-all-modes-supersedes-6075).

## More views

### Attach — `[a]` drops you into the agent

Press `[a]` on a row. Fleet quits the TUI and execs `tmux attach -t fleet-<agent>`, putting your terminal inside that agent's claude session. Type as you would normally; press `Ctrl-b d` to detach back to your shell. Re-running `fleet` brings you back to the dashboard with the agent still running.

```
$ fleet
                                 [navigate to a8, press [a]]

[detached fleet, attached to fleet-a8]

╭──── claude code · feat-rate-limiting ────────────────────╮
│                                                          │
│ > Implement IP-based rate limiting middleware.          │
│                                                          │
│   ⏺ Read(internal/middleware/auth.go)                    │
│   ⏺ Write(internal/middleware/ratelimit.go)              │
│       Added sliding-window limiter, 60 req/min/IP.       │
│   ⏺ Bash(go test ./internal/middleware/... -run Rate)    │
│       PASS  6 tests                                      │
│                                                          │
│   Committed feat(auth): add IP rate limiter (a4f8c12).   │
│                                                          │
│ ▌ Should I open the PR now, or run a broader smoke pass?│
│                                                          │
│ Ctrl-b d to return to fleet                              │
╰──────────────────────────────────────────────────────────╯
```

### Handoff in flight — fleet-guard hands the work off cleanly

Around 50% context, `fleet-guard` injects `HANDOFF REQUESTED` into the agent's next turn. The agent wraps the current sub-task, writes `MILESTONE` on its own line, and stops. Fleet then writes a structured handoff doc, spawns a fresh replacement, and the new agent reads the doc and resumes. The dashboard shows both rows during the brief transition.

```
┌─ Fleet 0.1.2 ─────────────────────────────────── edisonshen ─┐
│ △ 1 hot context                                              │
├──────────────────────────────────────────────────────────────┤
│ projects/fleet (2 tasks, 3 active)                           │
│   ⊕ a8   add-rate-limiting    ●52%  24m  handoff   3s        │
│         ⊕ handoff in flight (auto-yellow) → a11              │
│   ● a11  add-rate-limiting      5%   2s  doing     #2        │
│         resumed from .fleet/handoffs/a8-20260501-141812.md   │
│   ○ a4   docs-cleanup           18%  21m  idle     #0        │
│                                                              │
│ 1 project · 2 tasks · 3 agents · 0 queued                    │
│ [j/k] navigate  [a] attach  [d] dispatch  [q] quit           │
└──────────────────────────────────────────────────────────────┘
```

`a8` is in `auto-yellow`; `a11` was just spawned and is reading the handoff doc. A few seconds later `a8` archives itself and you're back to two rows. ([handoff state machine](skills/fleet-guard/SKILL.md#handoff-thresholds))

## Quick start

`tmux` is the only runtime dependency. Brew pulls it transitively:

```sh
$ brew install edisonshen/tap/fleet
$ fleet init                    # install bundled skills + seed standards.md into ~/.fleet/
$ fleet                         # opens the TUI
```

`fleet init` (v0.2) installs every bundled skill under `skills/*/` (`fleet-guard` plus the new `coordinator`) and seeds `~/.fleet/standards.md` from the embedded template. `fleet init --upgrade` (alias `--force`) refreshes skill files but never overwrites a hand-edited `standards.md`.

Upgrade later with `brew update && brew upgrade edisonshen/tap/fleet`.

### Coordinator quickstart (v0.2)

```sh
$ cd ~/projects/myrepo
$ fleet init                                     # seeds skills + standards.md
$ fleet tasks add "fix the auth retry bug"       # queues task; status=ready
# next coordinator tick (auto-runs from Claude Code hook): dispatches worker
$ fleet peek <slug> --follow                     # watch state.json + phase
$ fleet tasks list                               # see all tasks for this project
```

Tasks live in `~/.fleet/projects/<project>/tasks.md` (markdown-as-state, atomic writes, flock-serialized). Workers heartbeat into `~/.fleet/projects/<project>/workers/<slug>/state.json`. `fleet peek` falls back to the archive directory so completed workers are still inspectable.

> The fully-qualified tap path matters on `upgrade`: there's a JetBrains IDE also distributed as a brew cask called `fleet`, so `brew upgrade fleet` resolves to that cask first and errors with `cask 'fleet' is not installed`. Always upgrade by tap path, or use `brew upgrade --formula fleet` to disambiguate.

From source (Go 1.25+):

```sh
$ go install github.com/edisonshen/fleet/cmd/fleet@latest
$ fleet init
```

## Commands

Operator surface (v0.1):

```sh
$ fleet                   # opens the dashboard TUI
$ fleet dispatch <task>   # spawn a Claude Code agent in a detached tmux session
$ fleet attach <agent>    # take over a running agent (Ctrl-b d to detach)
$ fleet status            # one-shot health summary of every live agent
$ fleet handoff <agent>   # manually hand off a running agent to a fresh replacement
$ fleet drain             # process pending fleet-guard auto-handoff queue files
$ fleet rm <agent>        # archive an agent (kill its tmux session, no replacement)
$ fleet init              # install bundled skills + seed standards.md (--upgrade refreshes)
```

Coordinator surface (v0.2 — per-project task / learnings / standards / workers):

```sh
$ fleet tasks add <spec>          # queue a task (auto-derives slug from body)
$ fleet tasks list                # show all tasks for this project
$ fleet tasks show <slug>         # render one task
$ fleet tasks set <slug> <k> <v>  # mutate a task field (status, pr_url, etc.)
$ fleet tasks note <slug> <text>  # append a worker / coord note
$ fleet tasks archive <slug>      # move a finished task into archive
$ fleet tasks promote <slug>      # promote a worker-filed task past the gate

$ fleet learnings add <body>      # append; --tag t1 --tag t2 joins with '+'
$ fleet learnings list            # render the log
$ fleet learnings prune --before 30d  # prune old entries (Nd / Nw / Go duration)

$ fleet standards show            # default --merged: global + project section-merged
$ fleet standards show --global       # ~/.fleet/standards.md only
$ fleet standards show --project-only # per-project only
$ fleet standards edit            # opens $EDITOR on the right scope

$ fleet workers list              # active workers (slug, status, phase, pid, age, hb)
$ fleet workers list --all        # include archived
$ fleet workers update --phase X  # worker-side heartbeat (called from worker prompt)
$ fleet workers prune --older-than 7d  # delete stamp-old archives

$ fleet peek <slug>               # one-shot inspection of a worker
$ fleet peek <slug> --follow      # poll state.json until terminal phase
$ fleet peek <slug> --logs        # include log tail
```

Most operators live in the TUI for v0.1 flows; the v0.2 surface is shell-first by design — the coordinator skill drives most of it autonomously, and you mostly type `fleet tasks add` and `fleet peek`.

The `--project <name>` flag defaults to `tui.ProjectTag(cwd)` so `fleet tasks` and `fleet dispatch` always agree on the project name.

## TUI vocabulary

The status column tells you in one word what each agent is doing:

| Word     | Means                                                                            | Glyph |
|----------|----------------------------------------------------------------------------------|-------|
| `doing`  | Agent is actively running a turn                                                 | `●` green   |
| `asking` | Agent stopped on a question for the operator (`Do you want X?`, `[y/n]`, etc.)   | `●` bright cyan |
| `idle`   | Agent stopped, work done, no question pending — safe to ignore                   | `○` dim     |
| `review` | Agent dispatched as a reviewer (`Mode == "review"`)                              | `●` soft cyan |
| `blocked`| Agent flagged itself blocked (`fleet-guard` / explicit operator stamp)           | `▌` orange  |
| `handoff`| Auto-handoff in flight (`auto-yellow`, `auto-red`, or `precompact`)              | `⊕` yellow/red |
| `dead`   | tmux session is gone (claude exited inside it)                                   | `✗` faint   |

The banner aggregates the rows that need attention (anything other than `doing` / `idle`):

```
▌ N blocked   △ N hot context   ● N asking   ● N in review   ○ N idle   ✗ N dead
```

`hot context` fires whenever any agent crosses 70%, independent of its status. A clean dashboard hides the banner entirely.

### Hotkeys

| Key | Action |
|-----|--------|
| `j` / `k` | Navigate |
| `g` / `G` | Jump to top / bottom |
| `a` | Attach to the selected agent's tmux session |
| `d` | Dispatch a new agent (opens repo picker → prompt) |
| `h` | Hand off the selected agent (manual escape hatch) |
| `x` | Archive the selected agent (with `y/esc` confirm) |
| `q` / `Ctrl-c` | Quit |

### Tmux tip — scroll wheel in attached sessions

Claude Code captures mouse events in altscreen mode ([anthropics/claude-code#15780](https://github.com/anthropics/claude-code/issues/15780)), so the wheel doesn't scroll claude's output by default when you `fleet attach`. Add this to `~/.tmux.conf`:

```sh
echo 'set -g mouse on' >> ~/.tmux.conf
tmux source-file ~/.tmux.conf
```

Now the wheel scrolls into tmux copy-mode (press `q` to exit). Tradeoff: native click-drag text selection becomes shift+drag inside tmux — standard mouse-mode behavior. `Ctrl-b [` is the always-works keyboard alternative.

## Architecture

Single Go binary. Filesystem state under `~/.fleet/`. fsnotify for live updates with a 1s polling fallback. tmux as the only runtime dependency. Per-project `flock(2)` for write contention. Atomic writes (`.tmp` then rename, fsync before signaling).

Two Claude Code skills ship inside the binary and install to `~/.claude/skills/`:

- [`skills/fleet-guard/`](skills/fleet-guard/SKILL.md) — agent-side health watcher. Runs on every Stop / SessionStart / UserPromptSubmit / PreCompact hook, writes `~/.fleet/agents/<id>.json` for the TUI, delivers operator inbox messages, and queues handoff requests at `~/.fleet/queue/` for `fleet drain` to consume.
- [`skills/coordinator/`](skills/coordinator/SKILL.md) — per-project task driver (v0.2). Single tick per invocation, NB-flock on `coordinator.lock`, mutates state exclusively through `fleet tasks set` / `fleet tasks note` so Go remains the authoritative writer. Byte-equal Python ↔ Go parser parity is a CI gate.

| Doc | What it covers |
|---|---|
| [`docs/DESIGN.md`](docs/DESIGN.md) | Approved design — problem, premises, approach, lifecycle, alert surface, three-tier memory, distribution. |
| [`docs/FLOW.md`](docs/FLOW.md) | Eight-phase operator walkthrough from install through edge cases. |
| [`docs/STATE.md`](docs/STATE.md) | Filesystem schema and reliability invariants. |
| [`docs/DECISIONS.md`](docs/DECISIONS.md) | Committed design decisions with reasoning, indexed by date. |
| [`docs/SPIKE-context-pct.md`](docs/SPIKE-context-pct.md) | Week 0 feasibility spike — gating questions and findings. |
| [`docs/COORDINATOR-WORKFLOW.md`](docs/COORDINATOR-WORKFLOW.md) | Operator-facing six-step coord workflow (DISCUSS → SPLIT → TASK LIST → IMPLEMENT → PR-TRACK → DONE). |
| [`skills/fleet-guard/SKILL.md`](skills/fleet-guard/SKILL.md) | Agent-side Claude Code skill that watches context % and triggers handoffs. |
| [`skills/coordinator/SKILL.md`](skills/coordinator/SKILL.md) | Per-project autonomous coordinator (v0.2) — task dispatch, reconcile, gh polling. |
| [`CHANGELOG.md`](CHANGELOG.md) | Per-release additions, changes, fixes. |

## License

MIT — see [LICENSE](LICENSE).
