# Fleet

> **Everyone is a manager.**

An open-source command console for running many Claude Code agents in parallel. One operator, many concurrent agents across many repos, one TUI to keep them all productive.

```
┌─ Fleet 0.1.0 ───────────────────────────── edisonshen ──┐
│ ⚠ 1 unhealthy   ✏ 1 needs-input   ⚡ 1 warning          │
├─────────────────────────────────────────────────────────┤
│ rainier (3 tasks, 1 active)                             │
│   ● a8   add-rate-limiting       ●68% 14m  doing   #1   │
│         ⚡ approaching warning threshold                 │
│   ✓     fix-token-refresh-race   —    —    done    #0   │
│                                                         │
│ gift-finder (2 tasks, 1 active)                         │
│   ✏ a2  rec-engine-v2            ●41% 6m   blocked ⏸    │
│         "blocked: which similarity metric?"             │
│                                                         │
│ caching (1 task, 1 active)                              │
│   ⊕ a9  cache-eviction-policy    ●52% 2m   handoff 3s   │
│         graceful → spawning a11…                        │
│                                                         │
│ side-tool (1 task, 1 active)                            │
│   ◉ a10 parser-rewrite           ●23% 8m   review #1    │
│                                                         │
│ 4 projects · 7 tasks · 4 agents · 1 queued              │
│ [a]ttach   [d]ispatch   [c]hat   [n]ew   [q]uit         │
└─────────────────────────────────────────────────────────┘
```

Four agents, four repos, four states — coding, blocked on you, handing off, reviewing. One operator. That is the moment Fleet exists for. ([Phase 7 spec](docs/FLOW.md#phase-7--multi-task-parallel))

## Status

Pre-v0.1. The Week 0 feasibility spike — does a Stop hook give us reliable per-session context % at low latency? — gates the whole build. See [`docs/SPIKE-context-pct.md`](docs/SPIKE-context-pct.md).

The full design is at [`docs/DESIGN.md`](docs/DESIGN.md).

## Why

The bottleneck running multiple Claude Code agents is not Claude. It is the operator. Every context switch costs minutes of re-onboarding. Run four agents naively and you spend more time re-engaging than supervising.

Fleet treats supervision capacity as the constraint and optimizes everything around it: a single TUI shows every agent's health (context %, last activity, blocked state), automatic handoffs prevent context-degraded decisions, and a structured handoff format keeps the next agent productive within seconds.

## What ships in v1

Three pillars launched together. The parallelism demo only carries with the full picture, so it is one big-bang launch, not a trickle.

1. **Fleet view** — TUI showing every agent across every project with health badges, banner aggregation, and one-keystroke attach.
2. **Deploy / attach / peek / message** — full operator → agent communication surface, including async messages that don't interrupt a turn.
3. **Context-guard** — `fleet-guard` Claude Code skill that watches context % and triggers structured handoffs at **50% (graceful) / 70% (emergency)** thresholds. Same numbers across doing and thinking modes; only the enforcement differs. See [`docs/DECISIONS.md`](docs/DECISIONS.md#threshold-revision-5070-across-all-modes-supersedes-6075).

## Quick start

Until v0.1 is tagged, install from source (requires Go 1.25+ and `tmux` on `$PATH`):

```sh
$ go install github.com/edisonshen/fleet/cmd/fleet@main
$ fleet                    # opens the TUI
$ fleet dispatch <task>    # spawn an agent on a task
$ fleet attach <agent-id>  # take over a running agent
$ fleet status             # one-shot health summary
```

Once v0.1 ships, brew is the recommended path (pulls `tmux` transitively):

```sh
$ brew install edisonshen/tap/fleet
```

The full operator walkthrough — registering a project, planning, dispatching, handing off, hitting `max_concurrent_agents` — lives at [`docs/FLOW.md`](docs/FLOW.md).

### Tmux tip — scroll wheel in attached sessions

Claude Code captures mouse events in altscreen mode ([anthropics/claude-code#15780](https://github.com/anthropics/claude-code/issues/15780)), so the wheel doesn't scroll claude's output by default when you `fleet attach`. Add this to `~/.tmux.conf`:

```sh
echo 'set -g mouse on' >> ~/.tmux.conf
tmux source-file ~/.tmux.conf   # apply to running server
```

Now the wheel scrolls into tmux copy-mode (press `q` to exit). Tradeoff: native click-drag text selection becomes shift+drag inside tmux — standard mouse-mode behavior. `Ctrl-b [` is the always-works keyboard alternative.

## Architecture

Single Go binary. Filesystem state under `~/.fleet/`. fsnotify for live updates with a 1s polling fallback. tmux as the only runtime dependency. Per-project `flock(2)` for write contention. Atomic writes (`.tmp` then rename, fsync before signaling).

| Doc | What it covers |
|---|---|
| [`docs/DESIGN.md`](docs/DESIGN.md) | Approved design — problem, premises, approach, lifecycle, alert surface, three-tier memory, distribution. |
| [`docs/FLOW.md`](docs/FLOW.md) | Eight-phase operator walkthrough from install through edge cases. |
| [`docs/STATE.md`](docs/STATE.md) | Filesystem schema and reliability invariants. |
| [`docs/DECISIONS.md`](docs/DECISIONS.md) | Committed design decisions with reasoning, indexed by date. |
| [`docs/SPIKE-context-pct.md`](docs/SPIKE-context-pct.md) | Week 0 feasibility spike — gating questions and findings. |
| [`skills/fleet-guard/SKILL.md`](skills/fleet-guard/SKILL.md) | Agent-side Claude Code skill that watches context % and triggers handoffs. |

## License

MIT — see [LICENSE](LICENSE).
