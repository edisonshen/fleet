# Fleet

> Everyone is a manager.

Running many Claude Code sessions across many repos turns you into a
human scheduler: which tab is blocked, whose context is hot, which PR
needs a rebase. Fleet collapses that surface into one TUI. A per-project
coordinator owns the task list and dispatches workers; every worker
runs in its own git worktree on its own branch; context handoffs happen
automatically with the prior session's state attached. You stay the
manager. The agents do the work.

![Fleet dashboard](assets/dashboard.png)

## Why Fleet

- **40% context handoff, with context.** `fleet-guard` watches every
  agent's context usage and triggers a handoff at 40% / 50% thresholds.
  The successor receives a structured doc carrying prior state,
  decisions made, files modified, and next steps. You don't babysit
  context.
- **One coordinator per project.** Each project gets a dedicated coord
  session that owns `tasks.md`, dispatches workers, and shepherds PRs.
  Your single point of contact per repo. Coord identity is a
  crash-safe file lease: no staleness heuristic can ever kill a live
  coord, and a coord that died is recovered by pressing `[a]` — Fleet
  spawns a replacement with a synthesized handoff of the dead coord's
  in-flight worker state and archives the corpse only once the
  successor holds the lease.
- **Coords delegate, they don't implement.** `fleet-guard` enforces
  the coordinator's delegate-don't-implement rule mechanically: a
  PreToolUse hook denies inline edits outside `docs/` and test-running
  or file-mutating shell in a coord session, logging each violation
  under `~/.fleet/coord-violations/`. Subagents are exempt;
  `FLEET_COORD_GUARD=off` disables the deny.
- **Coord designs and splits the work.** Describe a problem, the coord
  runs a planning conversation (scope, edge cases, testing plan), then
  splits it into tasks and dispatches workers — only after you approve.
  No surprise scope.
- **Tasks run in parallel.** Coords run 3 workers at a time by default
  (`fleet init` asks; `parallelism: 1` keeps serialized in-place
  dispatch). Every worker runs in its own git worktree on its own
  branch. Multiple PRs open at once, all independently progressing.
- **Per-task status tracking.** `tasks.md` is the source of truth —
  status, priority, lifecycle timestamps, PR URL, notes. Visible in the
  TUI, mutable via `fleet tasks`.
- **Workers learn as they work.** Each project keeps a `learnings.md`
  log that workers append to as they hit gotchas, fixes, or domain
  conventions. The coordinator injects the top recent learnings into
  every subsequent dispatch prompt — so the next worker inherits prior
  workers' hard-won lessons without operator re-onboarding. Project
  intelligence compounds.
- **Two reviewers on every task.** The reviewer stage always resolves
  two slots at high effort: **alpha** (diverse — `codex` on a git tree
  when it is installed, else Claude Sonnet) and **beta** (a Claude Opus
  anchor that never skips). At least one genuine review lands even when
  codex is rate-limited or absent.
- **PR autopilot.** Each worker watches its own PR. CI fails →
  retry-fix subagent. PR goes BEHIND or DIRTY → rebase subagent on an
  isolated worktree. Trivial review comments addressed inline.
  Substantive Go conflicts raise to the operator.
- **Remote control, native and default-on.** Every coord spawn bakes
  `claude --remote-control` into its own argv, so mobile claude.ai can
  pair the moment the coord starts — no listener daemon. Opt a project
  out with `fleet rc down <project>`.
- **Clear install.** Brew tap, one runtime dep (`tmux`), one external
  CLI (`claude`; `codex` optional as the second-opinion reviewer), one
  bootstrap step (`fleet init`). No hidden config.

## Install

```sh
brew install edisonshen/tap/fleet
```

From source (Go 1.25+):

```sh
go install github.com/edisonshen/fleet/cmd/fleet@latest
```

Then bootstrap once:

```sh
fleet init
```

`fleet init` installs the bundled `fleet-guard` and `coordinator`
skills under `~/.claude/skills/`, merges the hook registrations into
`~/.claude/settings.json`, seeds `~/.fleet/standards.md`, and asks how
many workers a coord should run in parallel (default 3; pass
`--parallelism N` to skip the prompt). Re-run with `--upgrade` after
installing a newer binary to refresh the skill copies.

### Skill install shape: copy vs symlink

A bundled skill can sit on disk two ways:

| Shape     | Source              | When a merged skill fix goes live                 | Best for                     |
|-----------|---------------------|---------------------------------------------------|------------------------------|
| **copy**  | embedded binary bytes | after rebuilding the binary **and** re-running `fleet skills sync` | brew / binary-only installs  |
| **symlink** | the repo checkout's `skills/<name>/` | immediately, on the next coord spawn              | developing **on** Fleet      |

`fleet init` and `fleet skills sync` install **copies** — self-contained,
the only safe option when there is no repo checkout (brew). The cost: a
merged skill fix is invisible to a running coord until you rebuild the
binary and re-`sync`. This gap once silently neutered a merged P0
(`#182`): the install was a frozen hand-copied snapshot, so the fix never
reached the running coord.

If you are **developing on Fleet** (you have the repo checked out), use a
symlink so fixes go live the moment they land in your checkout:

```sh
fleet skills link            # symlink ~/.claude/skills/<name> -> <repo>/skills/<name>
                             # (auto-detects the checkout from registered projects)
fleet skills link --from ~/projects/fleet   # or point at a checkout explicitly
```

**Recommendation:** symlink for anyone working on the Fleet repo itself
(this is the default for the maintainer); copy for everyone else.

**Symlink tradeoff:** the linked skill follows your checkout's working
tree. If the checkout sits on a feature branch, the coord runs that
branch's skill code. Keep the linked checkout on `main` for stable
behavior, or `fleet skills sync --force` to convert back to a pinned copy.

`fleet skills status` reports each skill's shape and **flags a copy that
has diverged from the repo** (a merged fix the running coord can't see),
exiting non-zero so CI/scripts catch the drift. Every coord spawn runs
under the `fleet coord-run` supervisor, which also warns to stderr at
startup when the installed copy has diverged.

Runtime deps: `tmux` (brew pulls it transitively) and the
[Claude Code](https://docs.anthropic.com/en/docs/claude-code) CLI.
Optional: the `codex` CLI — when present it takes the alpha reviewer
slot, and `fleet --codex` runs coord, workers, and finisher on codex
for the session (the reviewer still runs claude for a second opinion).

> The brew tap path matters on upgrade: a JetBrains IDE is also
> distributed as a cask named `fleet`, so `brew upgrade fleet`
> resolves to the cask. Always upgrade by tap:
> `brew upgrade edisonshen/tap/fleet`.

## Quickstart

```sh
fleet init                              # one-time: install skills + seed standards
cd ~/projects/myrepo
fleet                                   # open the dashboard
```

From the dashboard:

1. Press `[+]` to register the current repo (or any cloned repo) as a
   Fleet project.
2. Move the cursor to the project row and press `[a]` to spawn its
   coordinator. `[a]` also attaches to a live coord, and auto-recovers
   a dead one (spawns a replacement with a synthesized handoff, then
   archives the corpse once the successor owns the lease).
3. In the coord's tmux session, describe a problem in plain English.
   The coord runs DISCUSS → PLAN-DOC → SPLIT → TASK LIST →
   TASK-PLAN-DOC with you.
4. Approve the plan. The coord saves the approved implementation plan
   under the project's `docs/` folder, writes `tasks.md`, saves
   per-task plan docs, then starts the worker/reviewer/finisher flow on
   its next tick.
5. Watch progress in the dashboard. Coords appear in the agents list
   with a live context %. Press `[a]` on a worker row to peek at its
   heartbeat; press `[h]` on the project row to force a coord handoff
   if context is hot; press `[n]` to queue a new task for the project.

## Hotkeys

| Key       | Action                                                        |
|-----------|---------------------------------------------------------------|
| `j` / `k` | Move cursor (wraps); `←` / `→` jump between panels             |
| `enter`   | Expand/collapse row, or open detail                           |
| `a`       | Attach: coord (project, auto-recovers a dead one), tmux (agent), peek (worker/task) |
| `n`       | Add a new task to the current project's `tasks.md`            |
| `d`       | Dispatch a new agent (opens repo picker)                      |
| `+`       | Register a cloned repo as a fleet project                     |
| `h`       | Handoff the project's coord to a fresh replacement            |
| `r`       | Reset a project: reap tangled/dead-locked coord state, respawn |
| `x`       | Archive the selected agent                                    |
| `c`       | Hide/unhide a project (on row); toggle show-hidden (off row)  |
| `/`       | Filter dashboard rows by substring (`esc` clears)             |
| `?`       | Help                                                          |
| `q`       | Quit                                                          |

Full surface: `fleet --help`.

## Architecture

A **project** is a repo Fleet has seen. A **coordinator** is a
per-project Claude Code session that owns `tasks.md`; its identity is
a three-file lease (flock) that is the single source of truth for who
owns the project, so attach, handoff delivery, and recovery all
resolve through it. Each coord tick is single-shot — reconcile, drain,
dispatch, hand off, exit — run under the `fleet coord-run` supervisor.
A **worker** is a Claude Code (or codex) agent in a detached tmux
session running one task in its own worktree, validated by a
two-reviewer gate before its PR is finished. A **handoff** fires when
an agent's context % gets hot — `fleet-guard` writes a structured doc
(completed work, decisions, files modified, next steps, open
questions, active subagents, open PRs) and a fresh agent picks the
work up on its first tick.

See [docs/DESIGN.md](docs/DESIGN.md) for the design rationale and
[docs/COORDINATOR-WORKFLOW.md](docs/COORDINATOR-WORKFLOW.md) for the
eight-step engagement flow (DISCUSS → PLAN-DOC → SPLIT → TASK LIST →
TASK-PLAN-DOC → IMPLEMENT → PR-TRACK → DONE).

## More

- `fleet --help` — full CLI surface (`dispatch`, `attach`, `handoff`,
  `drain`, `status`, `tasks`, `workers`, `learnings`, `standards`,
  `peek`, `project`, `rc`, `skills`, `gc`, `maintenance`).
- `~/.fleet/coord-config.json` / `<project>/coord-config.json` —
  `parallelism` and `worktree_timeout_s` (default 300s, for monorepos
  where `git worktree add` is slow).
- [CHANGELOG.md](CHANGELOG.md) — release history.
- [skills/fleet-guard/SKILL.md](skills/fleet-guard/SKILL.md) — agent-side
  context watcher and handoff trigger.
- [skills/coordinator/SKILL.md](skills/coordinator/SKILL.md) — per-project
  autonomous task driver.

## License

MIT — see [LICENSE](LICENSE).
