# Changelog

All notable changes to Fleet are documented here. Format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versioning
follows [SemVer](https://semver.org/).

## [Unreleased]

## [0.9.0] - 2026-05-13

`fleet project add <path>` now accepts non-git directories. Operators
can dispatch fleet workers against debug / polish / scratch projects
without first running `git init`. The worker SOP is identical to git
projects (TDD → phase=review-pending → reviewer-subagent runs /review
iterations → done) — only the finisher's `git push` + `gh pr create`
step is skipped. Deliverable is the file diff in place. Codex review
auto-skips with reason `no-git` since `codex review --base main`
needs a git diff.

### Added

- `fleet project add <path>` accepts directories without a `.git` entry; meta.json records `is_git=false` and stderr emits a warning. Existing git projects continue to require + assert the `.git` entry (#140).
- `Meta.IsGit` (pointer field for forward-compat) + `GitMode()` helper. Legacy meta.json files without the field default to `GitMode()=true` (treated as git-backed, byte-equal behavior to pre-v0.9.0).
- `internal/workers/workers.go` phase-machine accepts codex skip reason `no-git` (alongside `rate-limited` and `unavailable`) and allows `phase=done` directly for non-git projects without requiring the `push` transition (#140).
- `skills/coordinator/dispatch.py` worker / reviewer / finisher prompt builders branch on the project's `GitMode()`. Non-git workers edit files in place; non-git finishers mark `phase=done` and exit without pushing.
- `skills/coordinator/loop.py` reconcile accepts `phase=done` without `pr_url` for non-git projects and transitions the task to `status=done` directly (no PR-track / CI-poll).
- `skills/coordinator/SKILL.md` documents the non-git mode in a dedicated section.

## [0.8.3] - 2026-05-12

Coord handoff (auto via fleet-guard or interactive `fleet handoff`)
now writes enough state for the successor coord to fully reconstruct
continuity. Previously the `## Active Subagents` section only carried
`task / branch / phase / agent_id / subagent_id` — missing the PR URL
and task status. A handoff in the middle of a session left the
successor reading tasks.md by hand and re-spawning shepherds blind.
This release enriches the schema and adds an `## Open PRs` snapshot
so resume is deterministic.

### Fixed

- `## Active Subagents` rows now carry `status` + `pr_url` alongside the existing fields (#138). Legacy 5-field rows still parse with empty defaults — forward-compat with existing on-disk handoff docs.
- New `## Open PRs` section snapshots `gh pr list --state open --search head:worker/` at handoff time. Empty list renders `(no open PRs)` placeholder.
- `handoff_resume.py` consumes both: re-spawns a shepherd `until` loop for each open PR, and skips re-dispatch when the in-flight entry's status is already `in-review`, `done`, `abandoned`, or `blocked` (parked, not actively writing).
- SKILL.md "Resume after handoff" documents the enriched schema + selective re-dispatch behavior.

## [0.8.2] - 2026-05-12

Fixes a bug where auto-handoff (50%/70% context) lost the project's
coord identity. The replacement coord agent spawned successfully but
the project's coord-spawn marker still pointed at the dead old agent;
pressing `[a]` on the project row in the TUI then spawned a third
coord instead of attaching to the live replacement. The handoff doc
existed but the lineage was "isolated" — disconnected from the
project's official coord slot. Fix transfers the marker to the
replacement's agent ID at the end of both auto-drain and interactive
`fleet handoff` paths.

### Fixed

- Coord-spawn marker now transfers to the replacement agent on handoff (#136). Auto-handoff and `fleet handoff` both update the marker; worker handoffs leave the marker untouched as before. Six new regression tests pin the behavior across both paths.

## [0.8.1] - 2026-05-12

Coordinator now structurally enforces local `/review` iteration —
workers write code, mark `phase=review-pending`, exit; coord dispatches
a fresh reviewer-subagent that iterates `/review` to two-consecutive-
clean, runs codex once (skip if rate-limited), then a finisher-
subagent pushes and opens the PR. State-machine gate in `fleet workers
update` refuses `--phase push` without terminal review status. Fix-
subagent retry cap removed — coord keeps dispatching until CI green.

### Added

- Three-stage worker dispatch flow: code-writer → reviewer-subagent →
  push-finisher (#133). Eliminates the §7a exit-before-push pattern by
  structurally separating concerns — the code-writer never gets to
  push, so it cannot exit before review; the reviewer-subagent owns
  the `/review` loop; the finisher owns the push + PR open.
- Worker state fields `review_claude_status` / `review_codex_status` /
  `review_*_rounds` (#133). `fleet workers update --review-claude-
  status passed`, `--review-codex-status skipped --review-codex-skip-
  reason rate-limited`. Reviewer-subagent writes these as it iterates
  so the coord can gate `phase=push` on terminal review state.
- `fleet workers update --phase push` now rejects unless both review
  statuses are terminal; codex skip allowlist `rate-limited|
  unavailable` (#133). State-machine gate prevents a worker from ever
  reaching `phase=push` without going through the reviewer-subagent.

### Changed

- Worker dispatch prompt no longer instructs inline `/review` + push +
  PR (#133). Worker stops at `phase=review-pending` and exits; coord
  takes over dispatching the reviewer-subagent. Strips the
  multi-paragraph review section from the worker prompt — workers are
  now strictly code-writers.
- Fix-subagent retry cap removed from `skills/coordinator/SKILL.md`
  (#133). Coord keeps dispatching fix-subagents on CI fail until
  success or operator intervention. No code change — the cap was
  documentation-only.

## [0.8.0] - 2026-05-12

TUI `[+]` hotkey now auto-spawns the coord and freshly-added projects
classify ACTIVE; right-column dashboard accumulation eliminated
(stuck-worker sweep + idle-agent collapse + 24h auto-archive); Phase
B/C coord skill files now actually ship in the binary, with a
drift-prevention test pinning disk against the embed FS.

### Added

- `[+]` TUI hotkey auto-spawns the coordinator after `fleet project
  add` succeeds (#128), mirroring the `[a]`-on-project-row coord-spawn
  path. The flash on failure tells the operator the project is
  registered and to press `[a]` on the new row to retry.
- Freshly-registered projects classify ACTIVE for the
  `FLEET_ACTIVE_WINDOW_DAYS` window via `Meta.AddedAt` (#128), so a
  just-added project appears above the `─── N idle ───` separator
  instead of getting collapsed on first render. Legacy pre-meta
  projects fall through to existing rules unchanged.
- Right-column idle-agent collapse separator (`─── N idle ───`) for
  v0.1 agent records past the active window (#129), mirroring the
  left column's worker collapse. `asking` (NeedsInput) and `blocked`
  records always render active regardless of staleness — they need
  operator attention. `[enter]` toggles expansion.
- Coord supervisor auto-archives agents idle >24h (#129); configurable
  via `FLEET_COORD_IDLE_TTL_H` (range 1..720, zero disables).
- Drift-prevention test `TestSkillEmbedMatchesDisk` (#131) asserts
  set-equality between `skills/<name>/` on disk and the corresponding
  embedded `fs.FS` (`CoordinatorFS()`, `FleetGuardFS()`) so future
  skill files added on disk but forgotten in the `//go:embed`
  directive fail CI instead of shipping silently broken.

### Fixed

- Coord reconcile now sweeps `workers/<slug>/` dirs when a task
  transitions to `status=done` (#129), eliminating stuck worker rows
  after PR merges. Defense-in-depth tick-time sweep catches the cases
  the transition-time delete misses: operator-driven `fleet tasks set
  status=done`, pre-#101 coord versions, and races where status
  flipped done while a stale dir lingered.
- Phase B/C coord skill files now bundled in the binary (#130):
  `register_subagent.py` (Phase C subagent_id register),
  `handoff_resume.py` (Phase B2 re-dispatch after coord handoff), and
  `workflow_state.py` (G5 operator-readable workflow.md writer) were
  in the repo but missing from the `//go:embed` directive, so `brew
  install + fleet init` deployed a partial coord skill. Per-worker
  subagent chips on the dashboard now render on fresh installs.

## [0.7.1] - 2026-05-11

Quick polish patch — narrow-width title now keeps the brand mark, and
the footer advertises the more-commonly-needed `[+]` register hotkey.

### Changed

- Dashboard footer chip row now advertises `[+] add project` instead
  of `[n] task` (#122). The register-project verb is operationally
  more important than inline task-add — coords handle most task
  creation, so surfacing the clone-and-register hotkey on the footer
  reflects actual use. The `[n]` keybind still works as an internal
  task-add shortcut; it's just no longer the chip the footer markets.
- Title row uses greedy width-aware layout (#122): at narrow terminal
  widths, drops stat chips first, then project name, then version —
  but always preserves the `FLΞΞT` wordmark. The brand mark survives
  every fallback tier so the dashboard never renders a chip-only
  header that loses its identity.

### Deferred

- Codex review SKIPPED — rate-limited at 2026-05-11; quota resets
  2026-05-13 05:31 UTC. `/review` (gstack skill) PASSED. Codex re-run
  queued for post-reset.

## [0.7.0] - 2026-05-10

Handoff hardening — the v0.6.0 P0 remote-control fix shipped with two
latent gaps that surface only after a handoff. v0.7.0 closes both, adds
v0.2 Phase B/C coord-subagent continuity (handoff-survival + TUI
visibility), lands `fleet project add` for clone-and-register flow,
upgrades `fleet tasks list` to a recency view with auto-archive, and
finishes with a small TUI brand polish (FLΞΞT wordmark on the title
row).

### Added

- v0.2 Phase B coord-handoff continuity for live Agent-tool subagents
  (#113). When fleet-guard hands off the coord at 50/70% context, any
  Agent-tool subagent it spawned dies with the parent. The successor
  coord now reads a new `## Active Subagents` section in the handoff
  doc (Go-side `internal/handoff` Doc + Python-side `handoff.py` byte-
  golden cross-verify), and a new `skills/coordinator/handoff_resume.py`
  helper re-dispatches each worker, gated on WIP-file existence at
  `~/.fleet/subagent-wip/<task>.md`, with a RESUMING preamble pointing
  at the WIP. SKILL.md gains a `## Resume after handoff` section so
  successor coords re-read from disk and run the protocol. Plus B1:
  `[a]` on a worker row in the TUI now attaches to the project's
  coord chat (where the worker subagent's output renders as a "local
  agent" indicator), since worker tmux sessions went away in v0.3
  Phase A — orphan / dead-coord cases flash a status hint instead of
  crashing.
- v0.2 Phase C subagent_id rendering in the TUI (#116). Worker rows
  now carry a `· <8-char>` chip when the coord registered an
  Agent-tool subagent_id, plus a top-status `<N> agents` chip
  paralleling the existing `<N> yellow · <M> red` context-pct chips
  (hidden at zero, plural-aware). Coord-side `supervisor.py` gains
  `remember_subagent_id` / `forget_subagent_id` / `load_subagent_id_map`
  helpers and a `register_subagent.py` CLI the coord agent calls
  after each Agent-tool dispatch returns. Defense-in-depth shell-
  metachar blocklist on subagent_id input rejects `;`, `|`, `` ` ``,
  `$`, etc. with regression coverage.
- `fleet tasks list` recency view + lifecycle stamps + auto-archive
  (#114). List now defaults to ACTIVE rows in priority asc + created
  asc; if active count < 10, fills remaining slots with most-recent
  done/abandoned (`finished_at` desc, fall back to `updated` desc;
  tie-break created desc). Total visible = `max(10, active_count)` —
  active is never truncated. New flags: `--limit N` (`0` = unbounded),
  `--all`, `--no-archive`, plus `--status S` ignores cap. Lifecycle
  bullets `started_at` (sticky on first todo→in-progress) and
  `finished_at` (overwrite on done/abandoned, clear on flip back) land
  in the same transaction as the status flip — readers never see a
  half-stamped row. Schema stays v1 (additive optional bullets).
  Auto-archive at end of every coord tick: when `tasks.md` grows past
  `FLEET_AUTO_ARCHIVE_THRESHOLD` (default 50, `0` disables), the coord
  shells `fleet tasks archive` for the OLDEST done/abandoned slugs
  until count drops to threshold. Active statuses NEVER archived
  regardless of count.
- `fleet project add <path>` CLI + `[+]` TUI hotkey (#115). Lightweight
  way to register a cloned repo as a fleet project without dispatching
  a task or auto-spawning a coord. Creates
  `~/.fleet/projects/<tag>/{tasks.md, meta.json}`; idempotent on the
  same path; refreshes `repo_path` and warns on a tag-collision with a
  different repo. TUI `[+]` reuses the `[d]` repo picker, shells out
  to the same CLI, keeps the picker open on failure. New
  `internal/projects/meta.go` carries the `Meta` struct (schema,
  repo_path, added_at) with atomic Read/Write helpers; canonical
  `TagForPath` lives here so future per-project default-cwd consumers
  can derive the tag without dragging the bubbletea/lipgloss deps
  (TUI `ProjectTag` is now a re-export).
- `fleet maintenance bootstrap-remote-control` survey subcommand
  (#117) walks live agent records and surfaces those whose persisted
  Command lacks `--remote-control "<session>"`, printing one-line
  remediation suggestions (`fleet handoff <id>`). Used by operators
  to catch coords stuck on the pre-v0.6.0 spawn path; flags the
  candidates without auto-handing off (operator's call).

### Changed

- Dashboard title wordmark now renders as `FLΞΞT` (#119), replacing
  the prior plain `FLEET` label. The two `Ξ` glyphs (Greek capital
  Xi, U+039E) render bold + F1 brand red `#E10600`; `F`, `L`, `T`
  stay bold on the terminal default foreground so the mark adapts
  to light/dark themes without palette detection. Single-line, no
  layout shift (cell width preserved at 5). The Greek Xi reads as
  horizontal speed-lines without needing a tail graphic.

### Fixed

- **P0 follow-up to v0.6.0** — handoff replacement spawn now
  correctly injects `--remote-control "<session>"` into the standard
  `["sh", "-c", "claude ..."]` shell-wrapper command shape (#117).
  The v0.6.0 fix used a strict byte-equality matcher against
  `DefaultClaudeWrapperScript` that silently skipped any persisted
  Command body that diverged from the wrapper script bit-for-bit
  (forensic case: coord ca7eb43e lost remote-control after a manual
  handoff). Matcher in `internal/spawn.InjectRemoteControlFlag` is
  now SHAPE-based: any `["sh", "-c", "<body>"]` whose body begins
  (after optional leading whitespace) with the `claude ` token gets
  the flag injected immediately after that token. Custom non-claude
  wrappers, direct argvs, and bash-c shapes still return unchanged.
  12 new positive/negative shape tests plus an end-to-end integration
  test (`TestHandoff_ReplacementSpawnedWithRemoteControlFlag`) drive
  the handoff path against a wrapper-shape command using a shim
  `claude` in PATH.
- **P0 follow-up to v0.6.0** — handoff doc First Action now
  instructs the new coord to run `/coordinator` after `/remote-control`
  (#118), so the supervisor restarts in the replacement and the
  dashboard's project-row coord-name column updates to the
  successor's 8-hex agent ID. Previously the replacement only
  bootstrapped remote-control; the supervisor loop never restarted,
  the `coordinator.lock` retained the predecessor's ID, and tasks
  silently queued. Universal injection (not gated on coord-vs-non-coord
  task_id) because `/coordinator` is idempotent: NB-flock skips when
  held, and a non-coord lineage has no project to supervise → exits
  cleanly. Ordering pinned by test: `/remote-control` first so
  `/coordinator` startup output streams through the operator's mobile
  session. Fix lands byte-identically on both the Go-side
  operator-triggered handoff and the Python-side auto-handoff paths.

### Deferred

- Codex review SKIPPED across this release — rate-limited at
  2026-05-10; quota resets 2026-05-13 05:31 UTC. `/review` (gstack
  skill) PASSED on every PR. Codex re-runs queued for post-reset.

## [0.6.0] - 2026-05-09

Coordinator workflow + active PR shepherding ship as fleet-blessed
standards. Plus operator-facing TUI polish (title shows live binary
version, count-chip glyphs swap to flat monochrome unicode, expanded
task list now reads above the status-icons row) and a critical P0
fix: remote-control flag injection was silently broken across both
the coord-spawn path AND the handoff-replacement path, so mobile
claude.ai pairing couldn't see fleet-coord-* sessions at all.

### Added

- Active PR shepherding SOP in the bundled `~/.fleet/standards.md`
  template (`templates/standards.md` `## Async waits` → new
  `### PR shepherding` subsection; mirrored in
  `docs/STANDARDS-BASELINE.md`; cross-linked from the coordinator
  skill's Step 5 PR-TRACK runbook). Encodes the operator directive —
  *"if the pr is out of date, trigger it update, if the pr is ci
  failed, fix ci, if there are some comments, try to give solution.
  if there are some conflicts, resolve it, rebase it, push it again.
  not just watch and do nothing"* — as a per-PR background `until`
  loop that wakes on actionable states (BEHIND, DIRTY, CI failure,
  CHANGES_REQUESTED) in addition to terminal MERGED, with a per-state
  action matrix (rebase-shepherd / fix-subagent / inline-fix /
  operator-escalate) and a mandatory git-worktree-isolation rule for
  rebases. Motivated by live demonstration in fleet's own dogfooding
  (PR #106 merge → 3 PRs flipped BEHIND → rebase-shepherd round 1 →
  PR #108 merge → 2 PRs flipped BEHIND → rebase-shepherd round 2);
  without the actionable-state predicates a watcher polling only on
  `state != OPEN` slept through both rounds.
- Coordinator workflow runbook: the six-step engagement flow
  (DISCUSS → SPLIT → TASK LIST → IMPLEMENT → PR-TRACK → DONE) is now
  codified in `skills/coordinator/SKILL.md` and mirrored at
  `docs/COORDINATOR-WORKFLOW.md` for operators. Locks in approval-gate
  semantics, fix-/rebase-subagent dispatch templates, and the G5
  progress-doc schema (`~/.fleet/projects/<p>/workflow.md`).
- `skills/coordinator/workflow_state.py`: atomic-publish writer for
  the per-project progress doc — tmp-fd → fsync → `os.replace`,
  schema v1, with 28 unit tests covering validation and overwrite
  atomicity.
- Async-waits baseline in the bundled `~/.fleet/standards.md` template
  (`templates/standards.md`). Every freshly-spawned worker now inherits a
  blessed recipe for polling external state changes (PR merges, CI
  completion, deploys, file arrivals) — `Bash(run_in_background=true)`
  running an `until <check>; do sleep 30; done` loop, with the harness
  firing a `<task-notification>` on exit. Codifies the pattern so workers
  stop re-discovering foreground sleep chains, operator pings, and fixed-
  interval cron polling. Existing operators with hand-edited standards.md
  are untouched (`fleet init --upgrade` skips the seed when the file
  exists). New reference doc at `docs/STANDARDS-BASELINE.md` documents
  the full baseline + cite trail. Closes
  [#105](https://github.com/edisonshen/fleet/issues/105).

### Changed

- TUI title now uses the injected binary version (`FLEET — v0.6.0
  Ops Console`) instead of a hardcoded `v0.2 Ops Console` label.
  Fixes the "is my fleet up to date?" confusion the operator hit
  when the brew binary was at v0.5.0 but the title still read v0.2.
  Dev / unset versions render `FLEET — Ops Console` (bare) so a
  `go run` build doesn't lie about its provenance.
- TUI count-chip glyphs swap from emoji presentation
  (`⏳ ▶ 👁 ⚠ ✓`) to flat monochrome unicode (geometric shapes,
  Apple-flat aesthetic). Renders cleanly in Ghostty + iTerm + tmux
  without the 3D color emoji that broke the otherwise-monochrome
  Ops Console palette. Test pinning preserved per-glyph so the
  contract can't drift silently.
- TUI: expanded task list now renders ABOVE the count-chips row,
  not below. Reading order: project name → tasks (the actionable
  rows) → count chips (the summary). Operator dogfood: count chips
  are the summary, tasks are the action; eyes should land on the
  action first. Collapsed projects are byte-identical to today.
  New `TestProjectRow_TaskListRendersBeforeCountChips` pins the
  byte-order contract.

### Fixed

- Stuck-marker self-heal now also fires when the agent record is
  fresh (heartbeat within `2 × coordActiveWindow`), independent of
  tmux-session liveness. Previously the v0.4.0 self-heal only
  caught the dead-tmux case (issue #96 gap 1); a fresh-coord case
  with a stale `coord-spawn-marker` left the `⚠ coord spawn stuck`
  warning rendered forever even though the coord was happily
  ticking. The dashboard now cross-references `~/.fleet/agents/<id>.json`
  and clears the marker the moment the spawn is provably alive.
  Two heal paths (Path A: dead-tmux, Path B: fresh-record) live
  side-by-side in `internal/tui/coord_spawn.go` with five new
  regression tests pinning each.
- **P0** — fleet-coord remote-control daemon launch + flag
  injection were both silently broken, so mobile claude.ai pairing
  couldn't see fleet-coord-* sessions at all. Three concrete bugs:
  (1) `skills/coordinator/remote_control.py:spawn_daemon_if_needed`
  used a too-coarse `pgrep -f "claude.*remote-control"` regex that
  matched the always-running `fleet-handoff` daemon and silently
  skipped its own coord-daemon launch; now anchors `^claude` plus
  the `fleet-coord` prefix to mirror the Go-side regex; (2)
  `cmd/fleet/dispatch.go` gated `--remote-control` flag injection
  on the coord daemon being already-running, so coord-spawned
  agents were dispatched without the flag whenever the daemon
  hadn't booted yet (which was always, due to bug 1); injection is
  now unconditional for coord-spawn paths since `claude
  --remote-control "<name>"` retries connection if the daemon
  comes up later; (3) `internal/handoffop/handoffop.go` and
  `cmd/fleet/handoff.go` re-spawn paths for handoff-replacement
  agents also didn't inject the flag, so handed-off coords
  silently dropped off remote-control; both paths now share the
  new `internal/spawn/argv.go` helper. Plus `bootstrap_remote_control`
  no longer fails silently — returns a status enum that bubbles
  failures into the coord tick's `errors` channel and logs to
  `/tmp/fleet-bootstrap.log`. The load-bearing regression pin
  `test_pgrep_pattern_does_not_match_fleet_handoff_daemon`
  guards against the original misshape.

## [0.5.0] - 2026-05-09

Worker lifecycle hygiene + attention-chip accuracy. Workers reaching a
terminal phase (done / failed) now auto-delete their on-disk dir — no
archive, no grace period — and tasks split into active vs history under
a collapsible `─── N done ───` separator that keeps the active list
lean while preserving shipped + abandoned work. The attention chip no
longer overcounts on planning-blocked tasks: only worker phase=blocked
(the actual raise-hand) drives the red-alert; task `status=blocked`
rows render with a distinct `⏸` glyph in dim style.

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

### Fixed

- Attention chip no longer overcounts on planning-blocked tasks. The
  `row.Attention` rollup at `internal/tui/dashboard.go` previously
  added `row.Counts.Blocked` (operator-set planning state — "blocked
  by external dep / sequencing") on top of the worker-side
  raise-hand count, training the operator to ignore "1 need
  attention" because most of the time nothing was actionable. Only
  worker phase=blocked (the actual raise-hand) now drives the chip.
  Task `status=blocked` rows render with a distinct `⏸` (pause) glyph
  in faint dim style — visible "this task is parked" without the
  red-alert that the worker-blocked path keeps. `Counts.Blocked`
  still increments on scan for diagnostics + future filtering.
  Closes [#103](https://github.com/edisonshen/fleet/issues/103).

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

[Unreleased]: https://github.com/edisonshen/fleet/compare/v0.9.0...HEAD
[0.9.0]: https://github.com/edisonshen/fleet/compare/v0.8.3...v0.9.0
[0.8.3]: https://github.com/edisonshen/fleet/compare/v0.8.2...v0.8.3
[0.8.2]: https://github.com/edisonshen/fleet/compare/v0.8.1...v0.8.2
[0.8.1]: https://github.com/edisonshen/fleet/compare/v0.8.0...v0.8.1
[0.8.0]: https://github.com/edisonshen/fleet/compare/v0.7.1...v0.8.0
[0.7.1]: https://github.com/edisonshen/fleet/compare/v0.7.0...v0.7.1
[0.7.0]: https://github.com/edisonshen/fleet/compare/v0.6.0...v0.7.0
[0.6.0]: https://github.com/edisonshen/fleet/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/edisonshen/fleet/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/edisonshen/fleet/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/edisonshen/fleet/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/edisonshen/fleet/compare/v0.1.3...v0.2.0
[0.1.3]: https://github.com/edisonshen/fleet/compare/v0.1.2...v0.1.3
[0.1.2]: https://github.com/edisonshen/fleet/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/edisonshen/fleet/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/edisonshen/fleet/releases/tag/v0.1.0
