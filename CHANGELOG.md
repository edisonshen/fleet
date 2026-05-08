# Changelog

All notable changes to Fleet are documented here. Format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versioning
follows [SemVer](https://semver.org/).

## [Unreleased]

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

[Unreleased]: https://github.com/edisonshen/fleet/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/edisonshen/fleet/compare/v0.1.3...v0.2.0
[0.1.3]: https://github.com/edisonshen/fleet/compare/v0.1.2...v0.1.3
[0.1.2]: https://github.com/edisonshen/fleet/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/edisonshen/fleet/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/edisonshen/fleet/releases/tag/v0.1.0
