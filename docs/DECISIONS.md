# Design Decisions Log

Append-only record of non-obvious design choices. Most recent at top.
Each entry: decision, one-line rationale, where it's implemented.

For the full context behind a decision, see the referenced doc section.

---

## 2026-04-26

### v1.1 engine adapter — minimal v1 hooks

v1 ships Claude-Code-only as planned. v1.1 will add a second engine
(Codex CLI is the leading candidate). To avoid a painful migration, v1
carries the *minimum* hooks for engine pluggability — and explicitly
nothing more. No Go `Engine` interface, no plugin system, no multi-engine
TUI affordances. Three concrete v1 changes:

1. **`agents/<id>.json:engine`** — new field, value `"claude-code"` in
   v1. v1.1 writes `"codex"` (or similar) without bumping
   `schema_version`. Reader policy: missing field defaults to
   `"claude-code"` for forward-compatibility on archived records.

2. **`projects/<name>.yaml:engine`** — per-project default. Same
   forward-compat rule: missing → `claude-code`.

3. **`config.yaml:engines.<name>`** — spawn command lookup table.
   v1 has one entry; `fleet dispatch` reads `engines.<engine>.cmd`
   instead of inlining `claude`. v1.1 adds a second entry.

What this explicitly does NOT include:
- No engine interface in Go. Three lines of `if engine == "claude-code"`
  beats a premature abstraction. The shape of the abstraction should be
  informed by the *second* engine's actual surface.
- No `codex-guard` skill design. fleet-guard stays Claude-Code-specific
  by name and implementation. v1.1 ships a sibling skill that writes the
  same `agents/<id>.json` contract — same data, different producer.
- No Codex spike. v1's spike (`docs/SPIKE-context-pct.md`) answers v1's
  question only.

**Why now, not in v1.1:** schema additions cost zero in v1 because no
manifests exist yet. Same change in v1.1 would require migrating live
operator state. The forward-compat default rule (`missing → claude-code`)
also lets v1.1 read v1-era archived records without touching them.

**Why not more:** premature abstraction risk. The right `Engine`
interface depends on what Codex actually exposes for hooks and
transcript-token data — both unknowns until a Codex spike runs. Locking
an interface in now would almost certainly be wrong. v1.1 designs the
interface against a real second engine.

**Where:** `docs/STATE.md` agents/projects schemas (engine field +
forward-compat note); `docs/DESIGN.md` state-directory section
(engines map in config.yaml).

### Phase 6/7/8 walkthroughs — five committed decisions

Phase 6, 7, 8 of `docs/FLOW.md` were sketched in this session. Five
non-obvious choices surfaced that warrant logging.

1. **`queued` is a real persisted task status, not a transient flag.**
   When `max_concurrent_agents` is reached and operator presses `[d]`,
   the task goes to `status: queued` in the manifest. Auto-promotes to
   `doing` when a slot frees. Persisting as a real status (vs.
   `queued_for_dispatch: true` flag on a `todo` row) makes restarts
   coherent: an operator who quits Fleet mid-queue resumes with the
   same dispatch intent visible. Trade-off: enum grows from 7 to 8
   states; worth it for resume coherence.

2. **Retention split: 7 days for operational debris, 30 days for task
   history.** Agent JSONs and tmux logs are debug aids for *recent*
   crashes — 7 days. Handoff docs and progress logs are part of the
   *task's* permanent record — 30 days. Manifests and config never
   auto-prune. Pruning is one-shot at startup + hourly TUI tick, not a
   daemon thread.

3. **Banner severity order, fixed across all phases:** `⚠ unhealthy →
   ⏸ blocked → ✏ needs-input → ⊕ handoff → ⚡ warning`. Counts roll up
   across all projects (operator cares about "do I need to act?", not
   "which project"). Zero-count categories are omitted. The order
   encodes "demands-action-now" → "informational" left-to-right.

4. **Global agent caps as soft + hard:** `global_max_agents: 8` (soft
   warning surfaces `⚡` banner) and `global_hard_max: 16` (refuses
   spawn). v1 scope per DESIGN.md is 1-20 concurrent; the soft cap
   sits well under, leaving headroom. Numbers are guesses — Week 6
   dogfood will tune. Per-project cap (`max_concurrent_agents`,
   default 2) stacks with global; both must allow the spawn.

5. **`completion_source: operator` distinguishes manual-mark-done from
   agent-mark-done.** Default is `agent` (omitted from the file).
   Operator-marked done (via `[shift]+[d]`) populates
   `completion_source: operator` and surfaces `done (manual)` in the
   status line. The asymmetry: agent completion goes through review
   loop and is dispatched-then-completed; operator completion is a
   bookkeeping shortcut for work landed outside Fleet. Forensics
   should be able to tell them apart.

**Where:** `docs/FLOW.md` Phases 6-8 (full walkthroughs); `docs/STATE.md`
manifest schema (queued status, A4b retention table, A4 unhealthy_reason);
`docs/DESIGN.md` lifecycle diagram (queued branch).

---

## 2026-04-21

### Threshold revision: 50/70 across all modes (supersedes 60/75)

Earlier in the same session the doing-mode thresholds were set at 60%
(graceful) and 75% (emergency). Revised to **50% graceful, 70%
emergency** to align with thinking-mode reminders (also 50/70). One
pair of numbers across all mode families; enforcement differs.

- Doing modes (execute, fix): graceful handoff at 50% via `MILESTONE`;
  emergency kill at 70%.
- Thinking modes (plan, review): 50% fires `⚡` reminder, 70% fires `⚠`
  urgent reminder. No enforcement in thinking modes.

**Why the revision:** operators remember one pair of numbers (50 / 70)
more easily than mode-specific variants. The 20% runway between 50%
graceful queue and 70% emergency is still wider than the previous
15%, so bounded work units have more room to wrap. Aligns with
Premise 4 ("act at 50%") exactly — 50% is the action threshold in
both families, the action just differs.

**Supersedes:** the 60%/75% doing-mode proposal from earlier this
session. Final answer: 50% graceful / 70% emergency / 50-70% reminders.

**Where:** `docs/DESIGN.md` Health thresholds table; `docs/STATE.md`
F3a (graceful 50%), F3b (emergency 70%); `docs/FLOW.md` Phase 4 §4.4
+ §4.5, Phase 5 §5.1.

### Phase 5 handoff walkthrough — four confirmed details

1. **Framing prefix:** paraphrase of Hermes's "different assistant"
   language, kept in Fleet's handoff doc template. Preserves the
   load-bearing "reference not instructions" framing that prevents
   successor agents from re-executing completed work.
2. **Emergency handoff banner stays until operator acknowledges**
   (`[k]` dismisses). Does not auto-dismiss after first MILESTONE.
   Reason: emergency handoffs signal the agent ignored injected
   instructions or couldn't reach a milestone; operator should see
   each occurrence, not have it silently go away.
3. **Handoff-count escalation surfaces at `≥ 5`** as a `⚡` warning
   banner entry: "task X handed off N times — may be stuck." Drill-in
   pane suggests `[a]ttach` to diagnose or `[p] re-plan` to tighten
   the steps.
4. **Cancelled handoff docs are archived**, not deleted. Moved to
   `~/.fleet/handoffs/archive/.cancelled-<ts>.md` during grace-cancel.
   Retained for debugging; standard 7-day archive retention applies.

**Where:** `docs/FLOW.md` Phase 5 §5.6-§5.11.

---

### Post-execute review loop: 2 rounds against `origin/main`

Every task passes through a 2-round review loop between executor
completion and `done`. Each review is a fresh Claude agent with
clean context. Review scope is always full diff against `main` (not
executor-only commits). If the reviewer finds issues, a fix agent
addresses them; round 2 runs regardless. Reviewer uses `/codex review`
when codex is installed, else Claude Code's built-in `/review`.

**Why 2 rounds:** round 1 catches the obvious problems; round 2
catches what fix-1 missed or introduced. Single-round review is
brittle; open-ended loops are expensive. Two is the minimum that
catches the "fix broke something else" failure mode.

**Why full diff against main:** executor-only commits miss
interactions with prior commits on the branch. Full diff is what will
actually be merged, so that's what should be reviewed.

**Why fresh reviewer, not the executor self-reviewing:** blind-spot
avoidance. The executor's context is biased by its own plan; a fresh
agent re-reads the diff cold.

**Escape hatch:** `--review-rounds=0` skips the loop for trivial
changes. Default remains 2.

**Where:** `docs/FLOW.md` Phase 4 §4.7-4.10; `docs/STATE.md` new
invariant F5 "Post-execute review loop".

### Handoff threshold shifts: graceful at 60% + emergency at 75%  [SUPERSEDED]

> **Superseded by the "Threshold revision: 50/70 across all modes"
> entry at the top of this doc (same session).** The original
> decision was 60/75; final is 50/70. Keeping this entry intact for
> the historical record; ignore its numbers when reading current
> state.

Doing modes (execute, fix) get graceful handoff at 60% via the
`MILESTONE` token boundary, with emergency kill-and-respawn at 75% as
the safety net. Agent finishes its current bounded work unit (commit,
test pass, plan sub-step) before handing off. One token — `MILESTONE` —
serves as both progress signal in normal operation and exit trigger
when `HANDOFF REQUESTED` has been injected.

**Why one token** (still current): `MILESTONE` serves both the
progress-append cadence and the handoff boundary. Fewer tokens, simpler
CLAUDE.md snippet.

**Where:** `docs/DESIGN.md` Health thresholds table; `docs/STATE.md`
F3a graceful handoff, F3b emergency threshold.

### Thinking-mode thresholds: 50% / 70% reminders (no enforcement)

Thinking modes (plan, review) fire the 50% and 70% thresholds as TUI
reminders only (`⚡` yellow, `⚠` red). No auto-handoff. Operator
decides. Claude Code's own `/compact` at ~95% is the backstop.

**Why:** plan and review agents accumulate reasoning/judgment on a
specific task or diff. Killing them mid-thought destroys what we're
trying to produce. Different from doing modes where execution is
recoverable across handoffs.

**Why 50/70 (not 50/75):** 70% aligns with doing-mode's own emergency
threshold (see superseding entry at top of doc); single pair of numbers
across mode families. 70% still gives 25%+ runway to Claude's backstop.

**Where:** `docs/DESIGN.md` Health thresholds table; `docs/STATE.md`
F3c plan and review reminder-only.

### Mode vocabulary: `FLEET_MODE` with four values

`FLEET_MODE` ∈ {`execute`, `plan`, `fix`, `review`}. Reviewer is a
mode (under `FLEET_ROLE=executor`) rather than a new role, because
the agent is still supervised, still subject to F1 (no spawning
children), still reads/writes task files. Only the threshold family
differs (thinking vs doing).

**Project-level `FLEET_ROLE=planner`** (from the earlier decision) is
orthogonal — planner role has no thresholds at all, because a planner
session is a long operator-driven conversation, not a Fleet-supervised
work unit.

**Where:** `docs/STATE.md` F3 schema note; F5 phase/mode table.

### "Task already has a live agent" error: three real scenarios

Keeps firing the existing guard, clarifying in docs when it fires:
operator forgets a prior shell dispatch; auto-spawn succeeded and
operator dispatches again; two shells race on the same task. Error
message points at the running agent and offers `attach` or `handoff`.

**Where:** `docs/FLOW.md` Phase 4 §4.2.

---

### Plan mode is exempt from auto-handoff  [NUMBERS SUPERSEDED]

> **Principle still current; numbers superseded.** The mode-aware
> exemption holds: thinking modes (plan, review) are reminder-only.
> But the specific 50%/75% thresholds cited below are stale — final
> numbers are 50%/70%. See the "Threshold revision: 50/70 across all
> modes" entry at the top of this doc.

Auto-handoff is mode-aware: `FLEET_MODE=execute` enforces handoff at
Red; `FLEET_MODE=plan` fires the Red threshold as an operator
reminder (`⚠` in the TUI alerts banner) but does NOT kill-and-respawn.
Operator chooses: `[h]andoff` manually, `[a]ttach` to push to
`PLAN COMPLETE`, or let Claude's own `/compact` at ~95% be the backstop.

**Why:** losing accumulated planning context mid-thought is worse than a
longer session. The whole point of plan mode is to produce reasoning,
and the reasoning lives in the agent's context window. Execute mode is
different — execution is recoverable across handoffs, planning is not.

**Where:** `docs/DESIGN.md` Health thresholds (mode-aware table);
`docs/STATE.md` new invariant F3 "Plan-mode handoff exception".

### Q&A loop ships in v1, not deferred

Plan-mode agent can pause and ask questions when `## Context` is
insufficient. Mechanics: agent writes `## Planner Questions` to the
task file, sets `needs_input: true`, stops active processing. TUI
surfaces via the `✏` alerts channel. Operator answers by `[a]ttach`
(live in tmux) or `[e]dit` (inline in `$EDITOR`, auto-wake via fsnotify
on `<repo>/tasks/` or explicit `fleet plan --continue`).

**Why:** plan-first is the core flow; plan quality is load-bearing for
executor agent success. Shipping v1 without Q&A means thin-context
tasks produce bad plans — exactly the failure mode plan-mode is
supposed to prevent. The mechanism reuses the existing `needs_input`
flag and `✏` alert surface; no new channels needed.

**Where:** `docs/FLOW.md` Phase 3 §3.6; `docs/STATE.md` new invariant
F4 "Plan-mode Q&A loop".

### `PLAN COMPLETE` grep signal (v1); migrate to Stop-hook post-spike

Plan-mode agent emits `PLAN COMPLETE` on its own line when done.
fleet-guard greps the tmux pane capture, marks `status: planned`, and
sends `/exit`. Same grep-for-token pattern as the other phase signals
(`MILESTONE` handoff trigger, `READY FOR REVIEW`, `REVIEW COMPLETE`,
`FIXES COMPLETE`). Week 0 spike is expected to expose a cleaner
Stop-hook payload shape; migrate all detectors to the hook once
validated.

**Why:** grep-for-token is simple, unambiguous, and symmetric with the
existing handoff signal. Stop-hook is cleaner but premature until the
spike confirms what Claude Code's hook payload actually exposes.

**Where:** `docs/FLOW.md` Phase 3 §3.3 (Stop-signal rationale paragraph).

---

## 2026-04-19

### Project-level planning is a Claude Code conversation, not a Fleet UX

From a zero-task project, `[c]hat` spawns a planner session (tmux +
Claude + `fleet-planner` skill). The operator uses existing Claude Code
skills (`/office-hours`, `/plan-ceo-review`, `/plan-eng-review`, etc.) to
think through the work. When ready, `/fleet-sync` extracts a ranked task
list and writes it to `~/.fleet/queue/proposed-tasks-<session>.json`.
Fleet's TUI shows a modal review pane with inline checkboxes; operator
approves, tasks land in the manifest under flock.

**Why:** Fleet is a supervisor, not a host. Planning is thinking, and
Claude Code + its skills ecosystem already do that well. Rebuilding
planning UX inside Fleet would duplicate real tools badly. Delegating
planning to Claude also means Fleet automatically benefits from every
new planning skill the ecosystem ships.

**Where:** `docs/FLOW.md` Phase 2; `docs/DESIGN.md` CLI + TUI keys;
`docs/STATE.md` F1/F2 planner-role exception; `skills/fleet-planner/`
(to be scaffolded).

### Keys separated: `[c]hat` project-level, `[p]lan` task-level

`[c]` on a project row → planner session (chat to decompose into tasks).
`[p]` on a task row → `fleet plan <task>` (write execution plan for a
specific task). Two different activities, two different keys.

**Why:** context-sensitive keys confused the user in review. Separate
letters are clearer and map to distinct mental models: "I want to think
about this project" vs "I want to plan this specific task".

**Where:** `docs/DESIGN.md` TUI navigation bullet list + keybinding strip.

### Planner sessions: new agent role (`FLEET_ROLE=planner`)

Planner sessions are Fleet-spawned tmux sessions with `FLEET_ROLE=planner`.
Shown in the TUI under a "Planning" section with glyph `○` in magenta
(distinct from dim `○` todo tasks). Lifecycle rules differ from executor
agents: no context-% threshold, no auto-handoff, no dispatch authority,
but can write proposed-task files via the `fleet-planner` skill.

**Why:** planning needs long conversations (no context cap) and produces
artifacts (task proposals) that executor agents don't. Distinct role
keeps the F1/F2 guardrails clear about what each session can do.

**Where:** `docs/STATE.md` F1/F2 planner-role exception;
`docs/FLOW.md` Phase 2 §2.5, §2.7.

### Review pane is modal with inline checkboxes

Task review (approve/reject proposed tasks) happens in a modal pane with
inline checkboxes and 2-3 line context previews per task. `[Space]`
toggles, `[e]` drops to `$EDITOR` for full-file edit before approval,
`[Enter]` commits approved tasks atomically under flock.

**Why:** bulk approval needs to be fast. Inline checkboxes are faster
than `$EDITOR` diff-style review. Context previews let operator sanity-
check without drilling in. `[e]` escape hatch covers edit-before-approve.

**Where:** `docs/FLOW.md` Phase 2 §2.3.

---

## 2026-04-18

### Form presentation: modal first project, inline Nth project

The new-project form is a modal pane when the dashboard is empty (first
project) and an inline row at the top of the dashboard when content
already exists (Nth project). Same fields and validation, different
chrome.

**Why:** modal maximizes clarity when there's nothing to hide. Inline
preserves operator context (visible projects/agents underneath) once
there's something worth keeping visible.

**Where:** `docs/FLOW.md` Phase 1 §1.3.

### Task lifecycle is plan-first, two commands

`fleet plan` spawns a plan-mode agent that writes `## Plan` to the task file
and stops. `fleet dispatch` requires `## Plan` to exist (or `--skip-plan`
for trivial tasks). Task status:
`todo → planning → planned → doing → done`. `--redo` on plan re-plans
stale plans.

**Why:** separates thinking from execution. Operator reviews the plan
before execution burns an agent's full context window. Matches how real
engineering work happens: plan, review, execute.

**Where:** `docs/DESIGN.md` "Task lifecycle (plan-first)" + CLI commands
list; task status enum.

### Rename `fleet deploy` → `fleet dispatch`

The word "deploy" is overloaded with "ship code to production." Fleet's
verb is dispatching agents to tasks, not deploying services.

**Why:** avoids cognitive collision with CI/CD. Fits Fleet's command-console
metaphor (manager dispatches workers). TUI key `[d]` preserved.

**Where:** throughout docs. F1/F2 error copy in `docs/STATE.md`.

### TUI alerts surface: unified 4-category banner + inline + detail

Banner strip under title shows `⚠ N unhealthy · ⏸ N blocked · ⚡ N warning · ✏ N needs input` severity-ordered, zero-count categories hidden. Inline
glyph on row matches firing category. Enter or action-key drills into a
detail pane. A4 detail pane shows budget + deduped crash reasons +
`[u] unblock` `[l] tail logs` `[Esc] back`.

**Why:** A4 unhealthy and related operator-attention states (blocked,
warnings, needs-input) share the same journey; one unified surface is
simpler than per-category UIs. Inline keeps context; banner makes it
unmissable; detail gives action.

**Where:** `docs/DESIGN.md` "TUI Alert Surface" section.

### A5 per-file future-schema: skip the file, don't crash the binary

When reading any JSON file with `schema_version > max_known`, skip it and
increment the `⚡ warning` count; surface affected paths in the warning
detail pane. Manifest exception: hide that project's tasks, show `⚡
manifest schema ahead of binary` inline on the project header row. TUI
stays up.

**Why:** one future-schema file should not take down the whole dashboard.
Isolate blast radius; give operator a clear action (`fleet init`) rather
than a panic.

**Where:** `docs/STATE.md` A5 "Per-file check".

### A5 MINOR warning: stderr at startup + persistent ⚡ banner

Non-blocking version mismatch surfaces twice: stderr line at binary start
(catches scripting users of `fleet status`) and `⚡ warning` banner entry
(catches long-running TUI sessions). Persistent until `fleet init` runs.

**Why:** the two channels don't overlap audiences, so duplication is
additive not noisy.

**Where:** `docs/STATE.md` A5 startup check; `docs/DESIGN.md` TUI Alert
Surface glyph vocabulary.

### Error and glyph conventions (minimum for A4/A5/F1/F2)

stderr-only errors. Subcommand-prefixed (`fleet dispatch: ...`) or bare
(`fleet: ...`). Exit codes: 0 ok, 1 usage, 2 policy refusal, 3 version
mismatch, 4 state error. Glyph vocabulary: `●` active, `○` queued/todo,
`⚠` unhealthy, `⏸` blocked, `⚡` warning, `✏` needs-input. `NO_COLOR`
respected; `FLEET_ASCII=1` fallback; minimum terminal 80×24.

**Why:** the four invariants introduce new user-facing behavior that
needs consistent vocabulary. Broader CLI/TUI design system is deferred
to TUI implementation (Weeks 2-3).

**Where:** `docs/DESIGN.md` "Error and glyph conventions" subsection.

### F2 allowlist copy: `status, peek, version`

The error message for mutating-subcommand refusal was `Allowed: status,
peek` but the actual allowlist per STATE.md:317 also includes `version`
and `--help`. Fixed to list `status, peek, version`; `--help` is a flag
not a subcommand so intentionally skipped from the user-facing list.

**Why:** drift between implementation and error copy is a reliability
bug, not cosmetic. Operator-agent trust depends on accurate messages.

**Where:** `docs/STATE.md` L2 binary refusal snippet.
