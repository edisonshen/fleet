# Engineering Review — 2026-04-17

Reviewer: Claude (plan-eng-review)
Design under review: `docs/DESIGN.md` (approved 2026-04-15)
Repo state: v0.0.0 stub, pre-Week 0.

## Verdict

Plan is structurally sound. Big-bang launch is locked. Architecture review surfaced 8 findings (3 P1, 4 P2, 1 P3); test review surfaced 16 gaps including 1 critical (crash-loop prevention) and 1 needing eval (proxy accuracy). Test strategy is the largest gap — design has none. Recommend filling the test plan before Week 1 begins.

Week 0 spike is correctly gating. The design's `PostResponse` hook reference is wrong (real hook is most likely `Stop`); the spike doc captures this.

## Architecture findings

### A1 — Cross-file atomicity unspecified [P1, conf 9/10]

**Where:** DESIGN §2a "Sync rule" + §"Spawn & Lifecycle"

The state machine spans manifest ↔ task file ↔ agent JSON ↔ queue ↔ inbox ↔ handoff doc. The word "atomic" appears once, mechanism unspecified. Concrete failure: crash during the 6-step cooperative handoff leaves handoff doc on disk + queue trigger written + tmux still alive. On Fleet restart, the queue trigger fires a fresh spawn while the original agent is still running. Two agents on one task.

**Recommendation:** Adopt write-temp-then-rename for every state file write. Add a startup reconcile pass: walk `~/.fleet/queue/`, for each trigger verify the corresponding tmux session is dead before acting. Document invariants in a new `docs/STATE.md`.

### A2 — No lock on concurrent CLI invocations [P1, conf 8/10]

**Where:** DESIGN §2a CLI commands

Two terminals running `fleet deploy rainier` simultaneously both pick the same top-priority `todo` task, both spawn tmux sessions, both update the manifest. Different agent IDs, same task, undefined manifest state.

**Recommendation:** `flock(2)` on the project manifest during the deploy critical section (read manifest → pick task → write `current_agent` → release). Cheap, well-understood. macOS and Linux both support it via `golang.org/x/sys/unix`.

### A3 — Tmux pane grep for sentinel is fragile [P2, conf 7/10]

**Where:** DESIGN §"Spawn & Lifecycle" step 4

Detecting handoff completion via `tmux capture-pane | grep "HANDOFF COMPLETE"` depends on Claude emitting that exact string. Claude may paraphrase, the line may scroll off pane scrollback, operator-typed text could collide.

**Recommendation:** Have `fleet-guard` write a sentinel file (`~/.fleet/queue/handoff-<id>-complete.json`) when the handoff doc is fsynced. Fleet watches that file, not stdout. The skill controls the signal, not free-form Claude output.

### A4 — Auto-spawn loop has no rate limit [P1, conf 8/10]

**Where:** DESIGN §"Auto-spawn flow"

`auto_spawn: true` + an agent that crashes during init = infinite respawn. No backoff, no max-spawns-per-task, no "task is unhealthy" mark.

**Recommendation:** Per-task spawn budget (e.g., max 3 spawns per task per hour). On budget exhaustion, mark task `status: unhealthy` and require operator action. Exponential backoff between rapid handoffs (≥30s between same-task spawns).

### A5 — No version compatibility between binary and skill [P1, conf 9/10]

**Where:** DESIGN §3 + `skills/fleet-guard/SKILL.md`

`fleet` binary and `fleet-guard` skill share JSON shapes (health, queue files, handoff frontmatter). User upgrades binary independently of the skill, shapes drift, silent breakage.

**Recommendation:** Add `schema_version` field to every JSON shape. `fleet` binary checks skill version on startup, refuses to run with a `MAJOR` mismatch, warns on `MINOR`. Document the contract in `docs/STATE.md`.

### A6 — Env injection should be explicit [P2, conf 8/10] APPLIED TO SPIKE

**Where:** DESIGN §"Spawn & Lifecycle"

`fleet deploy` should use `tmux new-session -e FLEET_AGENT_ID=... -e FLEET_EXTRA_CLAUDE_MD=...` explicitly rather than relying on shell env inheritance. Tmux has its own per-session env; user's `.zshrc` could override. Easy fix.

**Status:** Noted in `docs/SPIKE-context-pct.md` as a Week 1 implementation detail.

### A7 — Handoff filename collision possible [P2, conf 8/10] APPLIED TO SPIKE

**Where:** DESIGN state directory layout

`handoffs/<id>-<ts>.md` — two handoffs in the same second collide. Local-time vs. UTC unspecified.

**Recommendation:** `handoffs/<id>-<utc-iso>-<short-uuid>.md`. UTC is portable; UUID suffix prevents same-second collision.

**Status:** Noted in `docs/SPIKE-context-pct.md` as a Week 1 implementation detail.

### A8 — fsnotify on macOS may need always-poll [P3, conf 6/10] APPLIED TO SPIKE

**Where:** DESIGN §3 fleet-guard

fsnotify misses rename events on macOS for `~/.fleet/queue/`. Polling fallback at 1s covers it but adds up to 1s latency. Worth measuring whether always-poll on darwin is simpler than fsnotify-with-fallback.

**Status:** Added to spike measurement scope.

## Code quality

No code yet. Two terminology nits for when implementation begins:

1. Pick "agent" or "Claude Code instance" — design uses both. "Agent" is shorter and matches the slogan; recommend that.
2. Skill name `fleet-guard` is fine; binary subcommand should NOT be `fleet guard <id>` (collides conceptually with the skill). Use `fleet status <id>` or `fleet peek <id>` (already in design).

## Test review

Coverage diagram (see review section above for the rendered version):

- 16 gaps total
- 1 CRITICAL: crash-loop prevention (A4)
- 1 needs eval: proxy formula accuracy (Spike Q3 doubles as this eval)
- 1 needs E2E: spawn → handoff → respawn full cycle

**Required additions to design before Week 1:**
- A `## Testing` section in DESIGN.md (or a separate `docs/TEST-PLAN.md`) covering the 16 gaps
- CI workflow at `.github/workflows/test.yml` running `go test ./...` on PRs (the design only specifies release-on-tag CI)
- Test fixtures for hook payloads (collected during the Week 0 spike — turn the spike into testable artifacts)

## Performance

No concerns at design level. fsnotify + 1s polling is correct order-of-magnitude for a TUI. Only thing worth measuring during Week 0: time from skill JSON write → TUI re-render. Target: ≤1.5s p95.

## Failure modes

| Codepath | Realistic failure | Test? | Error handling? | User sees? |
|---------|-------------------|-------|------------------|-----------|
| `fleet deploy` (concurrent) | Two agents on one task (A2) | NO | NO | Silent bug, undefined state |
| Handoff mid-flight crash | Double-spawn on restart (A1) | NO | NO | Silent bug, two tmux sessions |
| Auto-spawn crash loop (A4) | Disk fills with archive JSON | NO | NO | Eventually noticed; no clear error |
| Skill/binary version skew (A5) | Shape mismatch, parse errors | NO | NO | Cryptic JSON unmarshal errors |
| Sentinel grep miss (A3) | Handoff hangs in "saving..." forever | NO | NO | Operator confused, must `kill` manually |

**Critical gap:** all 5 above have NO test, NO error handling, and produce silent or confusing failure. Each one warrants a P1 fix in the architectural recommendations.

## Worktree parallelization (post-Week 0)

- **Lane A (sequential):** Week 1 CLI scaffold → Week 4 handoff. Both touch `internal/state/` + `cmd/fleet/`.
- **Lane B (independent):** Weeks 2-3 TUI. Touches `internal/tui/` only.
- **Lane C (independent):** GoReleaser + brew tap. Touches `.github/workflows/`, `.goreleaser.yaml`.

Conflict flag: Lanes A and B both eventually depend on the agent JSON shape. Define the contract in `internal/state/agent.go` first; both lanes import it.

## Open questions for the operator

1. **A1, A2, A4, A5 — how concrete should I make these in DESIGN.md vs. defer to implementation PRs?** I lean: add a "Reliability Invariants" subsection to DESIGN.md covering atomicity, locking, version compatibility, and crash-loop prevention as design-level requirements. Concrete code can come in PRs.
2. **Test plan:** add `## Testing` to DESIGN.md, or a separate `docs/TEST-PLAN.md`?
3. **A3 sentinel mechanism:** keep the cooperative "Claude says HANDOFF COMPLETE" flow as the primary signal with the skill-written file as belt-and-suspenders, or replace entirely?

## Completion summary

- Step 0: Scope challenge — accepted as-is (big-bang locked, complexity justified by strategy)
- Architecture: 8 issues found (3 P1, 4 P2, 1 P3)
- Code quality: 0 issues (greenfield)
- Test review: 16 gaps (1 critical, 1 eval, 1 E2E)
- Performance: 0 issues
- NOT in scope: written
- What already exists: written (nothing internal; prior art well-borrowed)
- TODOS.md: not yet created — pending decisions on A1/A2/A4/A5 will populate it
- Failure modes: 5 critical gaps flagged
- Outside voice: not run (no `codex` configured by default; can be added)
- Parallelization: 3 lanes (1 sequential, 2 independent), 1 contract conflict to coordinate
- Lake score: 7/10 — design is complete on user-facing surface; implementation reliability is under-specified
