# Fleet

> Command console for running many Claude Code agents in parallel.

Operating multiple Claude Code sessions across multiple repos is a
context-switching tax. Fleet collapses that surface into one TUI: a
single dashboard shows every agent's health, a per-project coordinator
dispatches workers autonomously, and structured handoffs keep work
moving when a session runs out of context.

![Fleet dashboard](assets/dashboard.png)

## Install

```sh
brew install edisonshen/tap/fleet
```

From source (Go 1.25+):

```sh
go install github.com/edisonshen/fleet/cmd/fleet@latest
```

Runtime dependency: `tmux` (brew pulls it transitively). Fleet drives
the `claude` CLI, so install [Claude Code](https://docs.anthropic.com/en/docs/claude-code)
first.

> The brew tap path matters on upgrade: a JetBrains IDE is also
> distributed as a cask named `fleet`, so `brew upgrade fleet`
> resolves to the cask. Always upgrade by tap:
> `brew upgrade edisonshen/tap/fleet`.

## Quickstart

```sh
fleet init                              # one-time: install skills + seed standards
cd ~/projects/myrepo
fleet                                   # open the dashboard
fleet tasks add "fix the auth retry"    # queue a task; coord picks it up
fleet peek <slug> --follow              # watch the worker's heartbeat
```

`fleet init` installs the bundled `fleet-guard` and `coordinator`
skills under `~/.claude/skills/` and seeds `~/.fleet/standards.md`.
Re-run with `--upgrade` to refresh skills without touching hand-edited
standards.

Inside the dashboard, press `[a]` on a project row to spawn its
coordinator; the coord then dispatches workers for queued tasks on
its next tick.

## Hotkeys

| Key       | Action                                            |
|-----------|---------------------------------------------------|
| `j` / `k` | Move cursor                                       |
| `enter`   | Expand/collapse row, or open detail               |
| `a`       | Attach: coord (project), tmux (agent), peek (worker) |
| `d`       | Dispatch a new agent (opens repo picker)          |
| `+`       | Register a cloned repo as a fleet project (no dispatch) |
| `n`       | Add a task to the current project                 |
| `h`       | Handoff the selected agent to a fresh replacement |
| `x`       | Archive the selected agent                        |
| `c`       | Hide/show a project row                           |
| `?`       | Help                                              |
| `q`       | Quit                                              |

Full surface: `fleet --help`.

## How it fits together

A **project** is a repo Fleet has seen under your cwd. A **coordinator**
is a per-project Claude Code session that owns `tasks.md` and dispatches
workers; one coord per project, NB-flocked. A **worker** is a Claude
Code agent in a detached tmux session, running one task. A **handoff**
fires when an agent's context % gets hot — `fleet-guard` writes a
structured doc and a fresh agent picks the work up.

For the design rationale, see [docs/DESIGN.md](docs/DESIGN.md). The
six-step engagement flow (DISCUSS → SPLIT → TASK LIST → IMPLEMENT →
PR-TRACK → DONE) is in [docs/COORDINATOR-WORKFLOW.md](docs/COORDINATOR-WORKFLOW.md).

## More

- `fleet --help` — full CLI surface (`dispatch`, `attach`, `handoff`,
  `drain`, `tasks`, `workers`, `learnings`, `standards`, `peek`).
- `fleet maintenance bootstrap-remote-control` — report live agents
  that pre-date the v0.7.0 remote-control injection fix and need a
  handoff to regain mobile pairing.
- [CHANGELOG.md](CHANGELOG.md) — release history.
- [skills/fleet-guard/SKILL.md](skills/fleet-guard/SKILL.md) — agent-side
  context watcher and handoff trigger.
- [skills/coordinator/SKILL.md](skills/coordinator/SKILL.md) — per-project
  autonomous task driver.

## License

MIT — see [LICENSE](LICENSE).
