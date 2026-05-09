# Changelog

All notable changes to Fleet are documented here. Format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versioning
follows [SemVer](https://semver.org/).

## [Unreleased]

### Added

- Lifecycle hygiene: a new `internal/lifecycle/` package classifies
  tasks, workers, and coord agent records onto a shared 5-state enum
  (Prerun / Active / Waiting / TerminalSuccess / TerminalFailure),
  and the cleanup orchestrator owns per-entity terminal actions.
  Workers reaching phase=done or phase=failed get their on-disk dir
  rm-rf'd via the new `workers.Delete` helper (no archive, no grace
  period — the operator-visible PR URL is already persisted on the
  task entry by then). The coord skill fires the cleanup after every
  reconcile/sentinel apply path that transitions the worker out of
  in-flight; the TUI's `scanWorkers` pass cleans any orphan dir the
  coord missed (defense-in-depth). Tasks split into active vs
  history under each project's expansion: a collapsible
  `─── N done ───` separator (toggled with `[enter]`) keeps the
  active list lean while preserving the operator-visible record of
  shipped + abandoned work. Done tasks render `✓ slug · PR #N`
  when a PR URL is on file; abandoned tasks render `✗ slug`. Worker
  blocked phase stays Waiting (dir is preserved so the operator can
  inspect `blocked_reason`). Coord agent records remain untouched —
  the coord skill's own 4h auto-idle-stop owns that lifecycle.
  Closes [#101](https://github.com/edisonshen/fleet/issues/101).
- New CLI: `fleet workers delete <slug>` removes the worker dir
  outright (idempotent on missing dir; refuses the literal slug
  `archive`). Used by the coord skill's lifecycle cleanup path; safe
  for operator manual use too.

## [0.4.0] - 2026-05-09

Project-list quality-of-life pass: operator can `[c]` hide projects
they're not working on, idle projects auto-collapse under a separator
group, and the context indicator drops its bar glyph in favor of the
colored percentage alone. Plus three coord-spawn-marker bugs that
were producing a "stuck" warning even after the underlying tmux
session had died.

### Added

- Project-list cleanup: `[c]` hides a project from the LEFT column
  and persists the choice in `~/.fleet/hidden-projects.json`
  (per-machine, atomic publish). Off-row `[c]` toggles show-hidden
  mode so the operator can re-discover hidden projects without
  remembering the name. `[enter]` on the new `─── N idle ───` /
  `─── N hidden ───` separators expands or collapses the group.
  Activity grouping classifies projects as ACTIVE (fresh coord tick,
  agent heartbeat, or live worker within `FLEET_ACTIVE_WINDOW_DAYS`,
  default 7d) or IDLE; idle rows collapse under the separator when
  any project is active, render inline otherwise. Footer chip
  surfaces `<N> hidden — [c] view` with `· M with activity` when
  the hidden list isn't dormant. Hide is HARD: fresh activity does
  not auto-unhide; only `[c]` on the row itself flips the bit. Workers
  and agents tagged with a hidden project still appear in the RIGHT
  column under `v0.1 agents`. Closes
  [#98](https://github.com/edisonshen/fleet/issues/98).
- `[x]` on a fully-dead v0.1 project row (no live agents, no v0.2
  project dir) archives the dead-agent records tagged with that
  project and removes the row. v0.2 project rows preserve the
  existing `[x]` task-archive behavior — only fully-dead legacy rows
  get the new dismiss path. Closes
  [#96](https://github.com/edisonshen/fleet/issues/96) gap 3.

### Changed

- Context indicator drops the 5-segment bar glyph (`▰▰▰▱▱`); only the
  colored integer percent remains (e.g. `48%`). Threshold colors and
  the handoff tag (`◐ HANDOFF` / `◐ COMPACT`) are unchanged. The bar
  read as visual noise next to the percentage at glance distance.
  Closes [#95](https://github.com/edisonshen/fleet/issues/95).

### Fixed

- Stale coord-spawn marker now self-heals when the tmux session it
  was tracking is gone. Previously the "⚠ coord spawn stuck" warning
  rendered forever once the marker aged past `FLEET_COORD_SPAWN_TIMEOUT_S`,
  even when the spawning agent had long died. The dashboard now reads
  the agent ID from the marker contents, probes
  `tmux SessionExists("fleet-<id>")`, and removes the marker (state
  flips to Idle) when the session is gone. Closes
  [#96](https://github.com/edisonshen/fleet/issues/96) gap 1.
- Stuck-spawn hint text now points at the correct tmux session name.
  Previously the warning suggested `fleet-<projectName>`, but real
  tmux sessions are `fleet-<agentID>` (8-char). The marker contents
  carry the agent ID; the hint now reads it and renders
  `fleet-<agentID>` so the operator can grep their tmux list and
  actually find the session. Falls back to the project-name framing
  when the marker is empty or unreadable. Closes
  [#96](https://github.com/edisonshen/fleet/issues/96) gap 2.

### Deferred

- Codex review SKIPPED across this release — rate-limited at
  2026-05-08; quota resets 2026-05-13 05:31 UTC. `/review` (gstack
  skill) PASSED on every PR. Codex re-runs queued for post-reset.

## [0.3.0] - 2026-05-09

Worker dispatch shifts from `fleet dispatch` subprocess to Claude's
Agent tool (`run_in_background`), so coord-spawned workers appear in
the coord's chat as the native "N local agents" indicator. Coord
agent role hard-constrained to discuss + dispatch (no inline
implementation). TUI polish wave: context bar with handoff threshold
colors, spawning-coord spinner, status glyphs per phase, arrow-key
navigation across rows and panels.

### Added

- Worker dispatch via Claude Agent tool (`skills/coordinator/`,
  Phase A) — coords now spawn workers as Agent-tool subagents with
  `run_in_background=true` instead of forking `fleet dispatch`.
  Workers surface in coord's chat as the Claude-native "N local
  agents" indicator. Phase B (lifecycle for surviving subagents
  across coord handoff) and Phase C (TUI subagent_id rendering +
  top-status `<N> agents` chip) tracked as
  [#93](https://github.com/edisonshen/fleet/issues/93) and
  [#94](https://github.com/edisonshen/fleet/issues/94).
- Context bar (`▰▰▰▱▱ 48%`) and handoff tag on every agent and
  worker row in the TUI. Five-segment bar with green/amber/red
  zones at fleet-guard's 50% / 70% thresholds. Inline handoff tag
  (`◐ HANDOFF` / `◐ COMPACT`) per `HandoffType`. Top-status hot
  counts (`<N> yellow · <M> red`) hidden when both zero. Worker
  rows look up their coord's record (`task_id == "coord-<project>"`)
  for the context_pct, deduped on agent record ID so coord +
  N subagents don't N+1 multiply-count.
- Spawning-coord indicator on project rows during the 3–5 min
  cold-start wait. State machine on `coord-spawn-marker`:
  marker absent → idle, marker fresh + no coord-state →
  spawning (10-frame braille spinner + `1m 23s` elapsed),
  coord-state fresh → active, marker stale past 10 min → stuck
  warning. Closes
  [#86](https://github.com/edisonshen/fleet/issues/86).
- Distinct glyph + color per task status in the inline expansion:
  `◌` queued, `◐` working, `◉` in-review, `✓` done, `✗` failed,
  `?` asking. Closes
  [#77](https://github.com/edisonshen/fleet/issues/77).
- Drill-into-need-attention task detail panel with `[a]` attach
  to that task's worker. Single-keypress path from the
  attention banner to the live worker tmux session. Closes
  [#75](https://github.com/edisonshen/fleet/issues/75).
- `←` / `→` jump cursor between PROJECTS and WORKERS · AGENTS
  panels; `↑` / `↓` are silent aliases for `j` / `k` row nav.
  Closes
  [#83](https://github.com/edisonshen/fleet/issues/83) and
  [#90](https://github.com/edisonshen/fleet/issues/90).
- Coord supervisor loop — event-driven reconcile + sparse
  stuck-check (2 min cadence vs the prior tight loop). Closes
  [#79](https://github.com/edisonshen/fleet/issues/79).

### Changed

- Coord agent role hard-constrained: discuss + dispatch only,
  no inline implementation. SKILL.md, system prompt, and the
  guard test pin the contract. Closes
  [#80](https://github.com/edisonshen/fleet/issues/80).
- TUI footer drops `[j/k] nav` and `[←/→] panel` chips
  (intuitive keys go silent), adds `[h] handoff` and
  `[x] archive`. Help overlay (`[?]`) is now the canonical
  discoverability surface for arrows + j/k + ←/→. Footer
  width pinned by regression test against 100-col split panes.

### Deferred

- Codex review SKIPPED — rate-limited at 2026-05-08; quota
  resets 2026-05-13. `/review` (gstack skill) PASSED on every
  PR in this release. Codex re-runs queued for post-reset.
- Phase B / Phase C of #84 (Agent-tool dispatch follow-ups)
  — see [#93](https://github.com/edisonshen/fleet/issues/93)
  and [#94](https://github.com/edisonshen/fleet/issues/94).

## [0.2.0] - 2026-05-08

Per-project autonomous coordinator. Brings tasks/learnings/standards
primitives, a coordinator skill that dispatches and reconciles workers
under `fleet dispatch`, and the operator-facing CLI to drive it.
Ships with a v0.2 TUI dashboard ("Ops Console") that surfaces projects
on the left and workers/agents on the right, with one-keypress coord
spawn + attach.

### Added

- v0.2 TUI dashboard ("Ops Console", `internal/tui/`) — 2-column
  layout: PROJECTS on left (with task counts and coord status),
  WORKERS · AGENTS on right. Keybinds: `[j/k]` nav, `[enter]` expand
  project to inline task list, `[a]` attach (or auto-spawn coord on
  empty project), `[h]` handoff, `[x]` archive, `[/]` search, `[?]`
  help, `[n]` task-add inline (no shell-out), `[q]` quit. Project
  rows derived from union of v0.2-initialized dirs and v0.1 agent
  project tags.
- Coord identification — agents holding `coordinator.lock` write
  their `coord_id` into the lock body and publish freshness via
  `coord-state.json` mtime. Dashboard renders coord under its
  project's row; freshness gated by `coordActiveWindow` (5 min).
  Task_id `coord-<project>` is a fallback signal for the boot
  window before the lock body publishes.
- `[a]` on a project row auto-spawns a coord agent if none exists:
  runs `fleet init` for the project, dispatches with stable task_id
  `coord-<project>`, and attaches immediately. Idempotent — second
  press attaches to the same coord.
- Coord agents auto-attach to remote-control via Claude Code's
  `--remote-control "fleet-coord-<id>"` flag (no manual
  `/remote-control` typing needed). Gated on the daemon already
  being up.
- Project name display transform: encoded `projects-fleet` renders
  as `projects/fleet` (replaces first hyphen with slash).
- Robust prompt-send for the `/coordinator` first-turn prompt:
  post-ready buffer (1.5s default, env-overridable via
  `FLEET_POST_READY_BUFFER_MS`), prompt+Enter delay (1s default
  via `FLEET_PROMPT_ENTER_DELAY_MS`), post-send verifier with one
  retry, structured warning on still-unsubmitted.
- Per-project coordinator skill (`skills/coordinator/`) — autonomous
  worker dispatch, status reconciliation via `gh pr checks` +
  `gh pr view`, single-tick design (no daemon), one coordinator per
  project enforced via NB-flock on `coordinator.lock`.
- `fleet tasks {add,list,show,set,note,archive,promote}` — per-project
  task registry. Auto-derives slugs from spec body, promotes
  worker-filed tasks past the dispatch gate, mutations serialize
  through `state.LockProjectState`.
- `fleet learnings {add,list,prune}` — append-only learnings log;
  `prune --before <dur>` accepts both Go-stdlib durations and
  operator-friendly `Nd` / `Nw` shorthand.
- `fleet standards {show,edit}` — global + per-project `standards.md`
  with section-level merge; `edit` opens `$EDITOR` (whitespace-split)
  and seeds a v1 frontmatter stub when missing.
- `fleet workers {list,update,prune}` — table of per-project workers
  (slug, status, phase, pid, age, last_heartbeat); `--all` includes
  archived workers; `prune --older-than 7d` deletes stamp-old
  archives.
- `fleet peek <slug> [--follow] [--logs]` — single-worker inspection
  with archive fallback; `--follow` polls `state.json` and exits on
  terminal phase.
- `internal/{state,tasks,learnings,standards,workers}` Go packages —
  markdown-as-state, atomic publish (`.tmp` + fsync + rename),
  flock-serialized writers.
- `fleet init` now installs every bundled skill under `skills/*/`
  (not just `fleet-guard`) and seeds `~/.fleet/standards.md` from
  the embedded template.
- CI gate: `go test -race` runs on every PR and push to main.
- End-to-end integration tests
  (`cmd/fleet/coordinator_integration_test.go`) drive the real
  coordinator skill against the real `fleet` binary, covering the
  C1 (handoff preserves in-flight) and C2 (parallel worker status
  isolation) invariants.

### Changed

- `fleet init --upgrade` (alias `--force`) refreshes skill files
  but never overwrites a hand-edited `~/.fleet/standards.md`.
- `cmd/fleet/autoinit.go` checks every bundled skill via
  `fleet.SkillFS()`, so v0.1 → v0.2 upgrades auto-install the new
  coordinator on next `fleet dispatch`.
- TUI `ProjectTag` lowercases output, paired with case-insensitive
  slug + project-name validation.

### Fixed

- Slug and project-name validation tightened to lowercase-only and
  case-insensitive-FS-safe (case-insensitive reserved-name checks).
- 13 `init` / `autoinit` tests now sandbox `FLEET_HOME` so they no
  longer write into the operator's real `~/.fleet/`.

### Deferred to v0.2.x

- Worktree creation and `cap > 1` parallel dispatch
  (`conflict.py` is in place; `git worktree add` + `--cwd <wt>` on
  `fleet dispatch` are pending).
- Auto-idle-stop after 4h of zero active tasks
  (PLAN "Smart sleep").
- 5-minute TTL cache on `gh pr checks` (current code hits gh
  synchronously per tick).
- `tasks.Archive` retry edge case —
  [#37](https://github.com/edisonshen/fleet/issues/37).
- `learnings.Prune` retry-dedup —
  [#38](https://github.com/edisonshen/fleet/issues/38).

## [0.1.3] - 2026-05-06

### Added

- TUI banner and `fleet status` footer nudge when a newer GitHub
  release is available; daily-cached version check.

### Fixed

- Several `fleet-guard` drain / handoff race fixes: producer triggers
  drain so handoffs always complete; `FLEET_*` runtime env propagates
  to spawned agents; `FLEET_BIN` stamped at spawn so self-drain
  survives non-PATH installs; throttled drain kicks; deferred drain
  kick at hook tail to avoid record-archive race.
- Version-check cache refreshes from CLI; suppresses dev-build
  upgrade nudges.

### Documentation

- README brew-upgrade path qualified to avoid JetBrains Fleet cask
  collision.

## [0.1.2] - 2026-05-01

### Added

- TUI splits `asking` (operator question pending) from `idle`
  (work done, no question) — distinct color and glyph.

### Fixed

- `fleet-guard` clears `has_pending_question` on prompt / resume.
- Stuck-pending watchdog re-injects Yellow handoff.
- `agent.HandoffTypeAt` typed as `*string` (was `*time.Time`).

### Documentation

- README rewrite for v0.1.x reality with screenshot and TUI mockups.

## [0.1.1] - 2026-05-01

### Added

- Auto-resume replacement after handoff — fresh agent reads the
  handoff doc and resumes inside the existing tmux session
  (hardened across 20 codex iterations).

### Fixed

- `fleet-guard` captures pane scrollback in `find_milestone`.
- Handoff JSON Stop-hook output; restored `[h]` escape for
  auto-yellow.
- Handoff readiness, kill-old, and rollback ordering hardened
  end-to-end.
- TUI splits `idle` from `in review`; paused reviewer stays
  `review` rather than `idle`.
- Spawn splits prompt and Enter to defeat bracketed-paste
  detection.
- Brew formula path pinned to `Formula/`.

## [0.1.0] - 2026-04-30

Initial public release.

### Added

- TUI dashboard (bubbletea + lipgloss) with live agent rows,
  status column, banner aggregation, repo picker, hotkey gates.
- CLI: `fleet dispatch`, `fleet attach`, `fleet status`,
  `fleet handoff`, `fleet drain`, `fleet rm`, `fleet init`.
- `fleet-guard` Claude Code skill — context %, threshold, record
  merge, inbox relay, hook entry point, byte-golden vs Go Render.
- Auto-handoff via `fleet-guard`: Yellow at 50% (graceful) and
  Red at 70% (emergency); operator-triggered manual handoff path.
- GoReleaser config + tag-triggered release workflow.
- Filesystem packages: `internal/state`, `internal/handoff`,
  `internal/queue`, `internal/spawn`, `internal/tmux`.

[Unreleased]: https://github.com/edisonshen/fleet/compare/v0.4.0...HEAD
[0.4.0]: https://github.com/edisonshen/fleet/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/edisonshen/fleet/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/edisonshen/fleet/compare/v0.1.3...v0.2.0
[0.1.3]: https://github.com/edisonshen/fleet/compare/v0.1.2...v0.1.3
[0.1.2]: https://github.com/edisonshen/fleet/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/edisonshen/fleet/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/edisonshen/fleet/releases/tag/v0.1.0
