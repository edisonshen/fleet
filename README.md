# Fleet

> **Everyone is a manager.**

An open-source command console for running many Claude Code agents in parallel. One operator, many concurrent agents across many repos, one TUI to keep them all productive.

![Fleet dashboard](assets/dashboard.png)

Four agents, four repos, four states — coding, blocked on you, asking, reviewing. One operator. That is the moment Fleet exists for. ([Phase 7 spec](docs/FLOW.md#phase-7--multi-task-parallel))

## Status

**v0.1.1 shipped to brew** ([releases](https://github.com/edisonshen/fleet/releases)). The Week 0 spike resolved (Stop hooks deliver per-session context % at low latency, see [`docs/SPIKE-context-pct.md`](docs/SPIKE-context-pct.md)) and the v1 surface — TUI, dispatch / attach / handoff / drain, fleet-guard auto-handoffs at 50% (graceful) and 70% (emergency) — is live. We're dogfooding through Week 6 and shipping bug-fix patches behind the scenes; expect frequent `brew upgrade fleet`.

The full design is at [`docs/DESIGN.md`](docs/DESIGN.md).

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
$ fleet init                    # install the fleet-guard skill into ~/.claude/
$ fleet                         # opens the TUI
```

Upgrade later with `brew update && brew upgrade fleet`.

From source (Go 1.25+):

```sh
$ go install github.com/edisonshen/fleet/cmd/fleet@latest
$ fleet init
```

## Commands

```sh
$ fleet                   # opens the dashboard TUI
$ fleet dispatch <task>   # spawn a Claude Code agent in a detached tmux session
$ fleet attach <agent>    # take over a running agent (Ctrl-b d to detach)
$ fleet status            # one-shot health summary of every live agent
$ fleet handoff <agent>   # manually hand off a running agent to a fresh replacement
$ fleet drain             # process pending fleet-guard auto-handoff queue files
$ fleet rm <agent>        # archive an agent (kill its tmux session, no replacement)
$ fleet init              # install the fleet-guard skill into ~/.claude/skills/
```

Most operators live in the TUI. The shell subcommands are there for scripting, dotfile aliases, and CI.

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

The agent-side half is a Claude Code skill ([`skills/fleet-guard/`](skills/fleet-guard/SKILL.md)) that runs on every Stop / SessionStart / UserPromptSubmit / PreCompact hook. It writes `~/.fleet/agents/<id>.json` for the TUI to read, delivers operator inbox messages, and queues handoff requests at `~/.fleet/queue/` for `fleet drain` to consume.

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
