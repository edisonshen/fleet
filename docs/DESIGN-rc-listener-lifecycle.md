# DESIGN: Remote-Control Listener as Operator-Managed Project Resource

**Status:** v8 ✅ **plan-eng-review CLEARED** — codex rounds 1-7 + plan-eng 5 decisions folded in. Ready for operator final approval (G2) → file P1 task → dispatch.
**Author:** coord agent (Feng Shen's session, post-PR-#157-spam-incident)
**Reviewers:** codex (round 1 returned NEEDS_REVISION_BEFORE_DRAFTING; round 2+ pending), plan-eng-review (pending), operator (G2 approval gate)
**Target version:** v0.12.0 — independent of dispatch-lifecycle stack
**Created:** 2026-05-16
**Supersedes:** PR #157 (`FLEET_RC_BOOTSTRAP_DISABLED` env-gate) — kept as test-time defense-in-depth during migration; retired in v0.13 once marker-gate is proven.
**Related (NOT fold-into):** `docs/DESIGN-dispatch-lifecycle.md` shares the boring-by-default infrastructure (state.WriteAtomic, project locks, sweeper patterns) but **RC is NOT a dispatch claim**. Codex round 1: "RC is operator-managed and spans many dispatches; reusing dispatch claims is not [the right reuse]."

---

## TL;DR for implementers

The `claude remote-control` listener daemon is currently spawned as an **implicit side effect** across multiple fleet code paths. PR #157 patched test-path spam with an env-gate; steady-state implicit spawn is still there. The 10-hour zombie-reviewer incident produced ~5,620 mobile push events.

**This design replaces patches with architecture:**
- **`internal/rc` package** — a project-scoped controller; **single owner of spawn, lock, state** (codex round 2: S1 routes through the controller, not just gates on `Enabled`).
- **Flat marker file** `~/.fleet/projects/<p>/rc-enabled` (matches `coord-spawn-marker` shape).
- **`rc-state.json`** with full ownership fields (pid, host_id, working_dir, session_prefix, last_spawn_at, last_error) — codex round 2 schema completion.
- **All 6 attach/spawn surfaces** route through `rc.Up/Down/Connect/Inspect`.
- **`fleet rc up/down/connect/status/list/reset` CLI** — operator-explicit lifecycle control.
- **Project isolation via Claude daemon's directory-keyed registry**, NOT via session-name-prefix filtering. Daemon prefix stays `fleet-coord` (legacy-compatible); the daemon's dir/name registry distinguishes per-project listeners. Sidesteps the coord-session-rename problem (codex round 2).
- **Handoff docs become operator-instruction text** — no bash exec.
- **`fleet rc connect` drives the in-session `/remote-control` slash-command** via `tmux send-keys` to the coord's tmux session — NOT external PID surgery (codex round 2 fix).
- **`rc.Down` kills local PID + removes marker; that's the teardown.** No `claude daemon remote-control remove` in the teardown path (codex round 2: that API is for the dir registry, not live-listener cleanup).
- **Test fake** — PATH-prepend `claude` script + injectable command-runner seam.
- **Standalone PR** (NOT folded into dispatch-lifecycle PR3).

Operator behavior after v0.12:
- Mobile pairing wanted for a project: `fleet rc up <project>` once.
- Stop bridge: `fleet rc down <project>`.
- Already-running coord without RC: `fleet rc connect <project>` (attach existing live session).
- Nothing implicit. No respawn loops. No mobile spam from tests.

---

## Motivation — the leak pattern compounded

[Unchanged from v1; see decision log for evolution.]

The session that produced this design discovered fleet's leak issues compound into **two patterns**:

- **Pattern A: Resource lifecycle gap** — covered by dispatch-lifecycle re-arch.
- **Pattern B: Implicit-spawn side effect** — the RC listener falls here. The 10-hour zombie-reviewer in this session exec'd the bootstrap ~590 times via `go test` invoking `runHandoff()`. PR #157 patched test boundaries; the **steady-state implicit spawn loop is still present** (`skills/coordinator/remote_control.py:294-369` ticks every 30s).

This design treats RC as **operator-managed project state**, not implicit dispatch side effect.

---

## Goals (revised per codex round 1)

1. **Listener-spawn is an explicit operator action** keyed by a per-project marker.
2. **Listener is managed by a dedicated `internal/rc` controller** with its own lifecycle (NOT a dispatch claim).
3. **All 6 attach/spawn surfaces gate on `rc.Enabled(project)`** (the inventory is now complete).
4. **Operator-facing `fleet rc` CLI** with `up/down/connect/status/list/reset`.
5. **`/remote-control` skill becomes a thin wrapper** around `fleet rc up` + `fleet rc connect`.
6. **Test paths cannot spawn the listener** via two layers: PATH-prepended fake `claude` binary + marker absence + env-gate.
7. **Production handoff continuity preserved** when operator opted in. Handoff docs **direct operator to `fleet rc connect`**; they do NOT embed raw bootstrap bash.
8. **Adoption refused unless Fleet-owned** — never adopt arbitrary listeners by pgrep PID alone (codex round 1: "not safe enough").

## Non-goals

- Cross-machine marker sync (single-host invariant).
- Auto-migration of existing pairings; operator opts in per-project at v0.12 cutover.
- Removing the listener mechanism entirely.
- Service-side state cleanup beyond what Claude Code's `claude daemon remote-control remove` offers.
- Reusing the dispatch-claim primitive (codex round 1: wrong ownership model).

---

## Resource shape — project-scoped controller

```go
// internal/rc/rc.go — NEW PACKAGE

package rc

// Enabled returns true iff the per-project rc-enabled marker exists.
// This is the SINGLE source of truth for "should listener spawn for this
// project". Every attach/spawn surface in fleet calls this helper.
func Enabled(project string) bool { ... }

// Up creates the marker + spawns the listener (or adopts a Fleet-owned
// one). Idempotent. Returns Outcome for stable CLI exit codes.
func Up(project string, opts UpOpts) (Outcome, error) { ... }

// Down kills the listener Fleet owns + removes the marker. Idempotent.
func Down(project string) (Outcome, error) { ... }

// Connect attaches the current operator session (interactive claude in
// this terminal) to the project's existing listener. Used by the
// /remote-control skill when a coord is ALREADY running and operator
// just enabled RC after the fact. NOT a spawn — only a state mutation
// + send-keys equivalent for the operator's UX.
func Connect(project string) (Outcome, error) { ... }

// Status returns observed state (marker present? listener PID? last error?).
func Status(project string) (State, error) { ... }

// List enumerates all projects with markers present.
func List() ([]string, error) { ... }

// Reset is the operator-emergency-stop: remove ALL markers + kill ALL
// Fleet-owned listeners across all projects.
func Reset() error { ... }

type Outcome string  // mirrors fleet claims outcome enum

type UpOpts struct {
    AdoptIfFleetOwned bool  // default true — adopt if state.json says it's ours
    AdoptIfUnknown    bool  // default FALSE — codex round 1: don't adopt arbitrary PIDs
}

type State struct {
    Project       string
    Enabled       bool      // marker present
    ListenerPID   int       // 0 if not spawned by Fleet
    HostID        string    // hostname when spawned (codex round 2 schema completion)
    WorkingDir    string    // operator's project working dir at spawn time; used by Claude daemon's directory-keyed registry
    SessionPrefix string    // "fleet-coord" — legacy global prefix; per-project isolation is via WorkingDir
    LastSpawnAt   time.Time
    LastError     string    // empty on success
}
```

### Storage layout

```
~/.fleet/projects/<project>/
├── rc-enabled                # FLAT marker file (zero bytes; matches coord-spawn-marker shape).
│                             # Presence = "operator opted in for this project".
└── rc-state.json             # REQUIRED Fleet-side ownership record:
                              # {schema, project, pid, host_id, working_dir, session_prefix,
                              #  last_spawn_at, last_error}
                              # Written atomically by rc.Up; read by Status + sweeper.
                              # If absent: rc.Up creates it; controller NEVER falls back to
                              # prefix-scan adoption (codex round 3+4: prefix-only is unsafe).
                              # Duplicate-spawn rule (codex round 2): marker present + state.json
                              # absent + Fleet-owned process alive in working_dir → refuse spawn
                              # AND refuse adopt; operator must `fleet rc reset` + re-up.
~/.fleet/claims-locks/rc-<project>.lock  # NB-flock for Up/Down/Connect concurrency.
```

Codex round 1 specifically chose:
- **Flat marker** over JSON (matches `coord-spawn-marker` flat-text convention at `internal/state/state.go:528-566`).
- **Separate state file** if metadata needed (don't pollute marker semantics).

### Adoption policy (codex round 1 fix)

`rc.Up` does NOT pgrep + adopt arbitrary matching processes. Two rules:

1. **Adopt if Fleet-owned:** If `rc-state.json` exists, has a PID, and that PID is alive AND its argv matches the recorded session_prefix, adopt it (idempotent re-Up). This proves Fleet spawned it.
2. **Refuse unknown adoption by default:** If marker absent OR state.json absent OR PID mismatch → spawn a new listener. If a NON-Fleet listener happens to match the same prefix, log a conflict warning and let the operator resolve with `fleet rc reset` (which kills both).

Override: `UpOpts.AdoptIfUnknown=true` for power users. Not exposed via CLI v1.

### Service-side management (codex round 2 correction)

Claude CLI exposes `claude daemon remote-control add/remove/list`, but **that is for the directory/server REGISTRY**, NOT for tearing down a live-listener process (codex round 2). The cleanup path is:

1. **`rc.Down` teardown:** kill local PID (SIGTERM, then SIGKILL after 10s grace); remove marker; remove `rc-state.json`. That's it. The Claude service times out its own stale session entries on its own timeline.
2. **Optional registry hygiene:** `rc.Reset` (operator emergency) may additionally invoke `claude daemon remote-control remove <working_dir>` to clean the dir-registry entry. Not in the per-project `Down` path — `Reset` only.

The earlier v2 proposal of `claude daemon remote-control remove "fleet-coord-<project>"` as a teardown semantic was wrong; codex round 2 caught the API-surface mismatch.

---

## Attach-surface gates — the complete 6-site inventory (codex round 1)

Every site in fleet that either spawns the daemon OR injects the `--remote-control` flag onto a claude argv now calls `rc.Enabled(project)`. Spawn-only sites are 3; flag-injection sites are 3 more — codex round 1 caught that I'd conflated them.

### Daemon-spawn sites (3)

| Site | Today | Post-design |
|---|---|---|
| **(S1)** `skills/coordinator/remote_control.py:spawn_daemon_if_needed` | Coord tick respawns global `fleet-coord` daemon every 30s if absent | **Routes through controller** (codex round 2): the Python function shells out via `subprocess.run(["fleet", "rc", "up", project, "--idempotent"])` and returns based on outcome. Python no longer does its own pgrep/spawn — Go controller is the single owner. Per-project isolation via Claude daemon's dir-registry (working_dir field in rc-state.json), not prefix-filter renaming. |
| **(S2)** `internal/handoff/handoff.go:FirstAction` | Embeds raw `pgrep \|\| nohup claude remote-control` bash in handoff doc | **Removes the bootstrap entirely.** Replaces with: "To re-attach mobile/web pairing, run `fleet rc connect <project>` in your terminal." Handoff doc becomes operator instructions, not exec'd bash. |
| **(S3)** `skills/fleet-guard/handoff.py:first_action` | Python byte-mirror of S2 | Same as S2 — operator-instruction string, not bash. Byte-golden invariant preserved by updating BOTH sides symmetrically (Python `EXPECTED_GOLDEN` + Go `TestRender_SkillByteGolden` both rewrite). |

### Flag-injection sites (3) — codex round 1 catch

| Site | Today | Post-design |
|---|---|---|
| **(I1)** `cmd/fleet/dispatch.go:injectRemoteControlFlag` (the PR #157 chokepoint) | PR #157 env-gates this | Add `rc.Enabled(project)` check; PR #157 env-gate stays as defense-in-depth. Returns input argv unchanged if marker absent OR env set. **No coord session-name change** — daemon prefix stays `fleet-coord`; per-project isolation via dir-registry (codex round 2). |
| **(I2)** `cmd/fleet/handoff.go:704-705` | Injects `--remote-control` on handoff replacement spawn | Gate on `rc.Enabled(project)`. Codex round 1 catch. |
| **(I3)** `internal/handoffop/handoffop.go:525-526` | Auto-handoff drain's injection point | Currently calls `spawn.InjectRemoteControlFlag` DIRECTLY, bypassing cmd/fleet wrapper. **New: dedicated `rc.GateAttachFlag(project, argv)` helper in `internal/rc/` package** is the chokepoint. I3 calls this helper (NOT the cmd/fleet wrapper). Project-aware. Codex round 2 explicit catch: "v2 needs a project-aware gate/helper here, not just 'same as I2' in prose." |

### `cmd/fleet/maintenance.go:348-351` (codex round 1 catch)

The `fleet maintenance` survey reports "agents missing --remote-control" as if it's always a bug. After this design, agents legitimately lack the flag when their project hasn't opted in. The survey output is rewritten to say "no RC enabled" or "RC enabled but not connected" depending on `rc.Status` per project.

### Defense-in-depth: PR #157 env-gate stays

`FLEET_RC_BOOTSTRAP_DISABLED=1` remains the test-suite default. Marker absence is the primary gate; env is the secondary. Tests can't spawn even if the marker is somehow present.

---

## `fleet rc` CLI

```
fleet rc up <project> [--cwd <path>]              # acquire: create marker + spawn listener (or adopt Fleet-owned)
fleet rc down <project>                            # release: SIGTERM PID + remove marker + remove rc-state.json
fleet rc connect <project> [--coord <id>]          # attach: send /remote-control to coord's tmux pane
fleet rc status [<project>] [--healthy]            # observability: marker + PID + last_error; --healthy probes claude daemon (plan-eng A1)
fleet rc list                                      # all projects with markers present
fleet rc reset [<project>]                         # emergency: kill all Fleet-owned listeners; optionally also invoke `claude daemon remote-control remove` for registry hygiene
```

**Note (codex round 3 P2 hygiene):** `fleet rc down` does NOT invoke `claude daemon remote-control remove`. The local PID kill IS the teardown; the daemon-registry call is operator emergency only via `reset`.

### `fleet rc connect` — drives the in-session `/remote-control` slash command (codex round 2 fix)

Codex round 2 confirmed: external "attach this existing claude PID by injecting `--remote-control`" is NOT a supported Claude CLI API. The only attach path is the in-session `/remote-control` slash command that the operator's claude session already supports.

`fleet rc connect <project> [--coord <id>]`:

1. Verify marker present and listener alive for `<project>`.

2. **Target selection — authoritative, not first-record** (codex round 3 P1.2):
   - If `--coord <id>` provided: use that coord.
   - Else: find the **lock-body holder** for the project's `coordinator.lock` (the canonical "active coord" signal — see `internal/tui/rows.go:241-327`).
   - Else (boot window): find the **coord-spawn-marker holder** at `~/.fleet/projects/<p>/coord-spawn-marker`.
   - Else: fail with `multiple coords for project; specify --coord <id>` (with listing of candidates from `fleet workers list`).

3. **Submit-verified send** — mirror `internal/spawn/spawn.go:199,281,327` contract exactly (codex round 5 P1, factual correction):
   - **Readiness-stability wait:** `tmux capture-pane` the target pane; **poll every 100ms** until content is stable for **500ms continuous**, with a **30s overall timeout**. After stability, add a **1.5s post-stability buffer** before typing (matches spawn.go's pre-type settle).
   - **Split-send:** `tmux send-keys -t <session> /remote-control` (text only, NO trailing newline). Then `tmux send-keys -t <session> Enter` as a SEPARATE call. Raw `"\n"` in the text burst can paste-without-submit.
   - **Verify submission (best-effort, ONE retry — codex round 6):** re-capture pane after the first Enter. If `/remote-control` is still visible at the bottom band: sleep `postSendRetryDelay` (matches spawn.go's named constant), send Enter ONCE MORE, re-verify. If STILL visible after that second Enter, fall back to manual-Enter warning: **prompt operator to press Enter manually** in their tmux pane. Do NOT hard-fail.
   - **Outcome reporting:** success on first-attempt verify → `{outcome: connected}`. Success after retry → `{outcome: connected, retried: true}`. Manual-Enter fallback (post-retry the prompt is still visible) → `{outcome: connected, warn: "prompt_unsubmitted_after_retry — operator press Enter in coord pane to submit /remote-control"}` (codex round 7: spawn.go's warning fires after a positive re-check, not on inconclusive capture; label reflects the actual condition). The CLI's exit code remains 0 in all three cases (matches spawn.go's best-effort stance).

4. Print operator-readable status: `Sent /remote-control to coord <id> (tmux: fleet-<id>). Check terminal for QR code / URL.`

5. If no coord running: `No live coord for project '<p>'. Run 'fleet dispatch ...' first, then 'fleet rc connect'.`

This sidesteps the impossible PID-injection mechanism. The operator's existing UX (typing `/remote-control` manually in a coord's tmux pane) is preserved; `fleet rc connect` automates the typing **safely** with submit verification.

### Stable JSON output + exit codes

Mirrors `fleet claims` outcome enum:
- `enabled` / `already_enabled` (0)
- `disabled` / `already_disabled` (0)
- `connected` (0)
- `not_enabled` (10) — `connect` invoked but no marker
- `not_owned` (10) — Fleet found a non-Fleet listener; refusal
- `absent` (11) — Inspect target doesn't exist
- `contested` (12) — Per-project lock held
- `error` (1) — Catch-all

### `/remote-control` skill rewiring

Today (pre-design): spawns global listener with shell bootstrap.

Post-design:
1. Detect current project from cwd basename (or operator arg).
2. Run `fleet rc up <project>` (creates marker + spawns / adopts).
3. Run `fleet rc connect <project>` (attaches current session).
4. Print URL.

Backwards-compat for non-fleet cwd: fall back to a per-machine `fleet-coord` prefix; skill output flags the non-project mode so operator knows.

---

## Handoff doc rewrite (codex round 1 finding)

The current handoff doc's `## First Action (auto)` section embeds raw bootstrap bash. Codex round 1: "a handoff doc should not embed raw daemon bootstrap at all after this redesign; it should tell the operator to run `fleet rc connect` or `/remote-control`."

New shape:

```markdown
## First Action (auto)

To re-attach mobile/web pairing for this coord, run in your terminal:

    fleet rc connect <project>

(Or `/remote-control` from within Claude Code.) The pairing will resume
from where the previous coord left off, provided RC was previously
enabled via `fleet rc up <project>`.

If RC was not previously enabled, run:

    fleet rc up <project>

first, then `fleet rc connect <project>`.
```

This is operator-instruction text. NO bash exec. The handoff continuity story moves from "automated re-bootstrap on read" to "operator runs one command on handoff resume" — a small UX regression but a large architectural win.

If operator wants the automated continuity: they keep `rc-enabled` marker present across handoffs (it's persisted state, not per-session). Then the NEW coord's first tick observes `rc.Enabled(project)=true` AND no listener alive → spawns one. So automated continuity DOES work via the spawn site (S1), just not via the handoff doc.

The handoff-doc rewrite is mainly to remove the dangerous "exec arbitrary bash from a markdown file" semantics that caused the test pollution.

---

## Test fake — PATH-prepend pattern (codex round 1)

Two-layer test boundary:

### Layer 1: Injectable command-runner seam

`internal/rc/rc.go` calls `spawn.Spawn(...)` or equivalent abstraction, not `exec.Command` directly. Tests substitute a fake spawn-runner that records argv and returns synthesized PID + alive state.

### Layer 2: PATH-prepended fake `claude` binary

For end-to-end integration tests where the real `claude` binary's argv parsing matters:

```bash
# In t.TempDir():
cat > "$TMPDIR/claude" <<'EOF'
#!/bin/sh
echo "argv: $@" >> /tmp/fake-claude-invocations.log
# If invoked as 'claude remote-control', print Connected to stderr and wait.
if [ "$1" = "remote-control" ]; then
    echo "Connected · fleet · (fake)" >&2
    # Wait for SIGTERM
    trap 'exit 0' TERM
    sleep 9999 &
    wait $!
fi
EOF
chmod +x "$TMPDIR/claude"
export PATH="$TMPDIR:$PATH"
```

Tests that exercise RC explicitly use the fake binary. The fake responds to SIGTERM cleanly (no leak). It NEVER connects to the Claude Code service — so no mobile push.

### Acceptance gate (CI invariant)

After running the FULL test suite (`go test ./...` + `pytest skills/`), verify NO process matches `claude remote-control --remote-control-session-name-prefix fleet-coord`. CI test fails the build on any spawn.

---

## Marker file shape (codex round 1: flat wins)

`~/.fleet/projects/<p>/rc-enabled` — **flat, zero-byte** marker file. Matches `coord-spawn-marker`'s convention at `internal/state/state.go:528-566`.

`~/.fleet/projects/<p>/rc-state.json` — **REQUIRED** state file. The controller writes it on every successful `rc.Up`; controller cannot adopt or reconcile without it. (Codex round 3 P2 hygiene: removed prior "optional" wording in this section that contradicted the "required" claim below.)

```json
{
  "schema": "v1",
  "project": "projects-fleet",
  "pid": 12345,
  "host_id": "operator-mac.local",
  "working_dir": "/Users/pinkbear/projects/fleet",
  "session_prefix": "fleet-coord",
  "last_spawn_at": "2026-05-16T20:00:00Z",
  "last_error": ""
}
```

**Duplicate-spawn rule (codex round 2):** if marker is present, state.json absent, but a process matching `claude remote-control --remote-control-session-name-prefix fleet-coord` is alive in this project's working dir → controller refuses to spawn a duplicate AND refuses to adopt by PID alone. Operator must `fleet rc reset` (kills, removes marker, fresh slate) and re-`up`. This is the conservative default; an `--adopt-unknown` flag is not exposed.

Read by `rc.Status` and the sweeper. **No prefix-scan fallback exists** (codex round 4: prefix-only adoption is unsafe; conflicts with the "broad `fleet-coord` prefix + dir isolation" model). If state.json absent and operator is asking about state, `rc.Status` returns `{enabled: <marker presence>, pid: 0, last_error: "no state.json"}` and operator can `fleet rc reset` + fresh `up` to re-establish ownership.

Atomic tmp+rename writes (matches `state.WriteAtomic` pattern). Operator can `rm` either file manually for emergency override.

---

## Sequencing (codex round 1: standalone, NOT folded into PR3)

Codex round 1 explicit: "folding this into PR3 makes review harder, not cleaner. PR3 is already the Replace/coord-swap proof step. RC is a different ownership model. Reusing `state.WriteAtomic`, project locks, and sweeper patterns is good; reusing dispatch claims is not."

### Standalone PR (recommended)

- Branched off main (or latest landed dispatch-lifecycle PR).
- ~1500 LoC: `internal/rc` (300) + `cmd/fleet/rc.go` CLI (250) + 6 attach-surface gates (200) + handoff doc rewrite (100) + tests (650).
- Lands independently of PR2-PR4.

### Dependency on dispatch-lifecycle

None at the controller level (codex round 1: different ownership model). BUT this design DOES reuse:
- `internal/state.WriteAtomic` pattern for marker + state files.
- Project-lock file shape from dispatch-lifecycle Adoptable.
- Sweeper integration: `fleet maintenance sweep-leaks` (PR4) calls `rc.SweepAllProjects()` to detect orphan listeners (Fleet-owned PID alive but marker absent → release).

PR4 adds a sweeper hook AFTER this RC PR lands. Order: this RC PR → PR4 (or vice versa with a small forward-compat note).

---

## Plan-eng-review decisions (2026-05-16)

Applied to v8:

1. **A1 — `--healthy` probe.** `fleet rc status --healthy` calls `claude daemon remote-control list` and matches against recorded `session_prefix`. Reports `healthy | dead-no-service-entry | dead-pid` with diagnostic. ~30 LoC. Without it, first signal of broken bridge is silent mobile-no-push.

2. **A2 — no v0.11→v0.12 migration step.** Operator clarification: state continuity for projects comes from `tasks.md` + WIP files (already persistent across versions); `rc-state.json` is per-listener-spawn metadata, not load-bearing project state. Operators run `fleet rc up <p>` when ready; old behavior decays as legacy listeners die or get killed. No migration runbook needed.

3. **A3 — CI invariant test in v0.12 itself.** Before v0.13 retires `FLEET_RC_BOOTSTRAP_DISABLED`, v0.12 must include a test that explicitly UNSETS the env-gate, runs the full test suite, and asserts pre/post pgrep snapshot identical. Pins marker-gate as sufficient. ~50 LoC.

4. **T1 — E2E test infrastructure: real-tmux + fake claude.** Use `internal/testutil/tmuxtest` for tmux server isolation. Inject a fake `claude` script as the "coord" pane that prints a prompt + reads stdin. `fleet rc connect` tests against this fake. Tests the full send-keys + verification + retry + fallback path. ~150 LoC. Mirrors dispatch-lifecycle PR3 test infra.

5. **T2 — critical duplicate-spawn refusal test.** Setup: write marker, simulate Fleet-owned process via fake binary, delete rc-state.json. Call `rc.Up`. Assert outcome ∈ {`contested`, `not_owned`}, NO new spawn, NO state.json rewrite. Pins the unsafe-adopt path codex round 2 explicitly closed. ~50 LoC.

---

## Migration — v0.11.x → v0.12.0

v0.12 introduces:
- `internal/rc` controller.
- `fleet rc` CLI.
- 6 attach-surface gates.
- Handoff doc rewrite (operator-instruction text replaces bash).
- Test fake pattern + acceptance gate.

v0.12 does NOT auto-create markers. Operator opts in per-project.

Migration steps for operator (one-time):
1. Upgrade to v0.12.
2. For each project where mobile pairing is wanted: `fleet rc up <project>`.
3. For currently-running coords without RC: `fleet rc connect <project>` (or accept that mobile pairing comes back on next coord boot).
4. Verify with `fleet rc status`.

The PR #157 env-gate stays through v0.12. v0.13 retires the env-gate after marker-gate is field-proven.

---

## Working-dir provenance — explicit resolution order (codex round 3 P1.1)

`rc.Up(project)` needs the canonical working_dir for the Claude daemon's directory-keyed registry. Today, fleet stores project cwd in three potentially-stale places. Resolution order (codex round 3 explicit):

1. **`--cwd <path>` CLI flag** if provided (operator override, highest priority).
2. **`~/.fleet/projects/<p>/meta.json:repo_path`** if present.
3. **Live coord record `.Cwd`** from `internal/agent/agent.go:94-99` (any alive agent for this project; uses the first alive one).
4. **Fail with diagnostic:** `cannot determine working dir for project '<p>'; pass --cwd <path>, OR re-register the project from the repo root with 'cd <path> && fleet project add <path>' so meta.json carries repo_path` (codex round 5: `fleet project add <path>` is positional, no `--cwd` flag per `cmd/fleet/project.go:45,47`).

The resolved working_dir gets persisted into `rc-state.json:working_dir`. Subsequent operations (`Down`, `Reset`, sweep) use the persisted value — the source-of-truth file, not re-derivation.

### Working-dir rename mid-lifecycle (codex round 3 free-form)

If operator renames or moves the project directory while listener is alive:
- The live listener keeps running (Claude daemon doesn't know about the rename).
- `rc-state.json:working_dir` becomes stale.
- Future `Inspect`/`Down`/`Sweep` keyed on `working_dir` find no daemon registry entry → may falsely declare orphan / refuse cleanup.

**Operator must:**
1. `fleet rc down <project>` — kills the listener using the stale working_dir match (best-effort).
2. Manually update `~/.fleet/projects/<p>/meta.json:repo_path` to the new path.
3. `fleet rc up <project> --cwd <new-path>` — fresh spawn keyed on new dir.

This is documented as an explicit lifecycle break, not a feature.

## Multi-coord-per-project — target selection (codex round 3 free-form)

If operator runs multiple coord agents for the same project (multi-tmux-window dev), `fleet rc connect` needs a deterministic target. Already specified in §`fleet rc connect` step 2. Summary: lock-body holder > coord-spawn-marker holder > require `--coord <id>`.

The marker file itself is per-project, not per-coord; one listener serves all coords for that project via Claude daemon's per-directory model.

## PR4 sweeper integration schema (codex round 3 free-form)

PR4's `fleet maintenance sweep-leaks --orphans` calls `rc.SweepAllProjects()`. The sweeper:

1. Enumerates `~/.fleet/projects/*/rc-state.json`.
2. For each: probe PID alive AND argv matches recorded prefix AND host_id matches current host.
3. Mismatches:
   - Marker absent but `rc-state.json` says PID alive → orphan; release.
   - Marker present but PID dead → respawn candidate (Up loop will pick it up; sweeper doesn't spawn directly).
   - Cross-host (host_id mismatch) → log + refuse (cross-machine cleanup is unsafe).
4. **Never kill on prefix-only evidence** — must have rc-state.json saying Fleet owns the PID.

Schema confirmed (codex round 3): `schema, project, pid, host_id, working_dir, session_prefix, last_spawn_at, last_error`.

## Claude CLI surface — verify-via-smoke-test caveat (codex round 3)

Codex round 3 noted that local `claude --help` exposes `--remote-control` flags but `claude remote-control --help` requires login and `claude daemon remote-control --help` falls back to generic. The doc treats the daemon-registry surface as "verify via smoke test", not settled fact. v0.12 worker dispatch must include a smoke-test step:

1. `claude daemon remote-control list` — verify subcommand exists.
2. `claude daemon remote-control add /tmp/test-dir` then `... remove /tmp/test-dir` — verify add/remove cycle.
3. If unsupported: fall back to local-PID-kill semantics only (already the primary teardown path); `reset` skips the daemon-registry call.

## Risks / open questions (round 1+2 mostly answered; remaining for round 4)

1. **Adopt-unknown override.** v2 design refuses non-Fleet listener adoption by default. Should there be an operator escape hatch (`fleet rc up --force-adopt`)? v1 leans no; v2 confirms no — `fleet rc reset` + fresh `up` is the recovery path.

2. [RESOLVED round 2+3] `fleet rc connect` uses tmux send-keys to coord pane (submit-verified per spawn.Spawn pattern), not PID injection.

3. **Multi-terminal operator.** If operator has 4 active coords across 4 projects, each with markers set, they want mobile pairing on all 4. Does `fleet rc up` on each project produce 4 independent listeners with project-scoped prefixes? Round 2: codex confirm Claude's daemon registry supports this concurrency.

4. **Handoff doc backward-compat.** Existing handoff docs in `~/.fleet/handoff/` were rendered with the OLD bash-bootstrap section. If a v0.12 coord reads a v0.11-rendered handoff doc on resume, what happens? v2 design: the bash is no longer auto-exec'd by anything (the doc is markdown that the operator reads). Old docs harmless. Round 2: codex confirm no automated code path actually exec's the markdown section.

5. **Coord spawn before RC enabled.** Operator spawns coord at T=0. At T=10min, operator runs `fleet rc up`. Coord has been running without `--remote-control`. `fleet rc connect` attaches the live session. Round 2: codex confirm Claude CLI supports retroactive attachment of a non-RC session.

6. **Two coords for the same project.** If operator has two coords for the same project (multi-tmux-window setup), do they share one listener or fight over the marker? v2 design: shared listener (project-scoped, not coord-scoped). The marker reflects per-project intent. Round 2: codex confirm.

7. **Stale `rc-state.json`.** If state.json says PID=12345 but actual listener is dead, `Inspect` returns `Dead`. Next `Up` re-spawns. Race window between Inspect and Up: another process spawns concurrently. Per-project lock catches this. Round 2: codex confirm.

---

## Failure modes / acceptance

| Failure | Behavior |
|---|---|
| `fleet rc up` exec fails | Drop marker + state.json. Return diagnostic (auth missing, claude binary missing, network). Idempotent retry. |
| Listener dies after Up | `rc.Status` returns `Dead`. Next coord tick (S1) re-spawns if marker present. Sweeper releases orphan if marker absent. |
| Operator removes marker manually (rm file) | Coord tick stops respawning. Existing listener kept alive until SIGTERM. Sweeper detects mismatch (state.json PID alive but no marker) → releases. |
| Concurrent `fleet rc up` | Per-project NB-flock; loser sees `already_enabled` and returns. |
| Service-side `claude daemon remote-control remove` fails (only invoked from `rc.Reset`, not `rc.Down`) | Log warning, continue with local kill. Service eventually times out its own session entry. |
| Cross-host claim attempt | `state.json` carries `host_id`; mismatch → refuse. Mirrors dispatch-lifecycle invariant. |

### Cross-cutting acceptance

- `pgrep -f 'claude remote-control --remote-control-session-name-prefix fleet-coord'` returns empty after `go test ./...` AND `pytest skills/` (CI gate).
- `fleet rc up projects-fleet && fleet rc status projects-fleet` shows `enabled=true pid=<int>`.
- `fleet rc down projects-fleet && pgrep ...` returns empty.
- Operator running 4 coord agents observes mobile push only for projects they explicitly `fleet rc up`'d.
- Migration: pre-v0.12 handoff docs in `~/.fleet/handoff/` don't break under v0.12 readers.

---

## Decision log

- **2026-05-16** — Operator decision post-PR-#157: "completed fix leak bug completely from architecture, not just patch, patch, patch". This design replaces env-gate-as-patch with architecture.
- **2026-05-16** — v1 drafted: proposed `rc_listener_daemon` as Exclusive dispatch claim, fold into dispatch-lifecycle PR3.
- **2026-05-16** — Codex round 1 verdict: NEEDS_REVISION_BEFORE_DRAFTING. Findings folded into v2:
  - RC is operator-managed, spans many dispatches → standalone `internal/rc` controller, NOT a dispatch claim.
  - Inventory was 4 paths; actual is 6 (3 spawn + 3 flag-injection). Added `cmd/fleet/handoff.go:704` and `internal/handoffop/handoffop.go:525`.
  - Path 3 in v1 was wrong (flag injection, not daemon spawn).
  - Marker = flat (matches coord-spawn-marker). Separate `rc-state.json` for metadata.
  - Adopt-existing refused by default (codex: not safe to adopt arbitrary PIDs).
  - Service-side cleanup: `claude daemon remote-control remove`, NOT `claude remote-control --stop`.
  - Add `fleet rc connect` for retroactive attach.
  - Handoff doc: instruction text, NOT raw bash exec.
  - Test fake: PATH-prepended `claude` script + injectable runner seam.
  - Maintenance.go survey rewrite for "agents missing --remote-control" semantics.
  - Standalone PR, NOT folded into dispatch-lifecycle PR3.
- **2026-05-16** — Sequencing: standalone PR off main. ~1500 LoC. Independent of PR2-PR4 land cadence.
- **2026-05-16** — plan-eng-review CLEARED ✅ (v8). 5 decisions applied:
  - A1: `fleet rc status --healthy` probes `claude daemon remote-control list`.
  - A2: no migration step; tasks.md + WIP files carry project state continuity.
  - A3: CI invariant test in v0.12 — env-gate disabled, zero listener spawn assertion.
  - T1: E2E real-tmux + fake claude binary for `connect` flow.
  - T2: critical duplicate-spawn refusal test (marker + no-state + Fleet-PID-alive).
- **2026-05-16** — Codex round 7: ✅ **PASS_TO_PLAN_ENG_REVIEW**. One nit folded (v7-final):
  - Warn label changed from `verification_inconclusive` → `prompt_unsubmitted_after_retry` (semantic fidelity to spawn.go:327-348: the warning fires AFTER positive re-check that prompt is still visible post-retry, NOT on inconclusive capture; the inconclusive-capture case is treated as success per spawn.go:375).
  - Codex-rounds phase complete (7 rounds: 1-2 NEEDS_REVISION_BEFORE_DRAFTING → 3-6 CONTINUE_DESIGN → 7 PASS).
- **2026-05-16** — Codex round 6 single-nit fix folded (v7):
  - Send/verify retry policy: spawn.go does ONE explicit retry (first Enter → verify → sleep `postSendRetryDelay` → second Enter → verify → manual-warning fallback). v6 said "no retry loop" — incorrect. v7 matches: one explicit retry then warning.
- **2026-05-16** — Codex round 5 factual corrections folded (v6):
  - Send/verify mechanism matches actual `spawn.go` numbers: poll 100ms, stable 500ms continuous, 30s timeout, 1.5s post-stability buffer.
  - Send verification posture: best-effort (matches spawn.go's "manual Enter may be needed"), not hard-fail-on-mismatch. Prompts operator instead of erroring; returns success with `warn_verification_inconclusive`.
  - Working-dir diagnostic: `fleet project add <path>` is positional (no `--cwd` flag). Operator runs from project root or passes absolute path; meta.json captures repo_path automatically.
- **2026-05-16** — Codex round 4 critique folded (v5):
  - Send/verification mechanism rewritten to match `spawn.go:199,281,327` contract exactly: readiness-stability wait → split text + Enter → verify prompt-no-longer-visible (not "echo appears") → ONE Enter retry (not 3).
  - Storage-layout ASCII block scrubbed: `rc-state.json` "OPTIONAL" → "REQUIRED"; prefix-scan fallback removed; duplicate-spawn rule inlined.
  - "Read by `rc.Status` and sweeper" section: prefix-scan fallback removed; operator path is `reset` + re-up.
  - rc-state.json JSON example: `project` field added (schema match with PR4 sweeper section).
  - Working-dir failure diagnostic: `fleet projects set-cwd` → `fleet project add <path>` (the real CLI is positional, no `--cwd` flag, per `cmd/fleet/project.go:45,47` — codex round 5 catch).
- **2026-05-16** — Codex round 3 critique folded (v4):
  - Working-dir provenance: explicit resolution order (`--cwd > meta.json.repo_path > live coord .Cwd > fail`). Persisted to rc-state.json:working_dir.
  - Working-dir rename mid-lifecycle: documented as explicit lifecycle break; operator must `down` + manual meta.json edit + `up --cwd <new>`.
  - `fleet rc connect` target selection: lock-body holder > coord-spawn-marker holder > require `--coord` flag.
  - `fleet rc connect` send mechanism: submit-verified per `internal/spawn/spawn.go:171-293` pattern (capture-pane readiness check, separate text + Enter sends, capture-pane echo verification, 3 retries).
  - PR4 sweeper schema: `rc.SweepAllProjects()` called from `--orphans` mode. Never kill on prefix-only; refuse cross-host.
  - Claude CLI daemon-registry: verify via smoke test in v0.12 worker; fall back to PID-kill-only if unsupported.
  - Stale doc text scrubbed: `rc-state.json` "optional" wording removed, `fleet rc down` synopsis fixed, old Q2 PID-injection mention deleted, failure table row scoped to Reset path.
- **2026-05-16** — Codex round 2 critique folded (v3):
  - S1 routes through controller (Python shells out to `fleet rc up`; not parallel spawn logic).
  - `fleet rc connect` drives in-session `/remote-control` via `tmux send-keys` (not PID injection — that API doesn't exist).
  - `rc.Down` teardown is local PID kill + marker removal only. `claude daemon remote-control remove` moved to `rc.Reset` emergency path only (it's a directory-registry API, not a live-listener teardown).
  - Coord session naming stays `fleet-coord-<id>-<project>`. Project isolation via Claude daemon's directory-keyed registry (working_dir in state.json), NOT session-name-prefix filter. Sidesteps the rename collision.
  - `rc-state.json` schema completed: added `host_id`, `working_dir`.
  - `rc-state.json` becomes required (not optional) — controller needs it to prove Fleet ownership for adoption.
  - Duplicate-spawn rule: marker present, state.json missing, but Fleet-owned process alive in working_dir → refuse spawn, refuse adopt, require `reset`.
  - I3 chokepoint: dedicated `rc.GateAttachFlag(project, argv)` helper. Not "same as I2".

---

## Open items before draft-freeze

- [ ] Codex round 2 review of v2 → fold findings.
- [ ] Codex round 3+ until clean for two consecutive runs.
- [ ] `/plan-eng-review` lock-in.
- [ ] Operator final approval (G2 gate).
- [ ] After approval: file P1 task `rc-listener-managed-controller` and dispatch worker (manual finalize, no Agent-subagent run-in-background — lesson from PR2 zombie).
