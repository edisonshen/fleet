# DESIGN: Coord-Spawn Auto-Writes RC Marker

**Status:** v1 — operator approved 2026-05-18 (G2 gate cleared). Ready to split into tasks.
**SUPERSEDED (2026-06-11, rc-default-native-startu-a2ce):** remote
control is now NATIVE and DEFAULT-ON — every coord spawn bakes
`claude --remote-control "fleet-coord-<id>-<project>"` into the
coord's own argv. The standalone listener daemon, the per-tick Python
respawn (`spawn_daemon_if_needed`), and the `fleet rc connect`
send-keys path described in this document are retired. The rc-enabled
opt-in marker is replaced by an rc-disabled opt-OUT marker
(`fleet rc down <project>`). This document is kept as historical
context for the v0.12/v0.13 architecture and its cleanup paths
(Down/Reset/sweep still reap leftover legacy daemons).
**Author:** coord agent 5f05977b (Feng Shen's session, projects-fleet)
**Reviewers:** operator (G2); codex + /review at PR time per CLAUDE.md §4.
**Target version:** v0.12.1 (point release on top of v0.12.0 RC architecture).
**Created:** 2026-05-18
**Priority:** P0 — `/remote-control` is broken for fresh coords; operator quote: "we don't allow this."
**Supersedes:** none — narrows one gate inside `DESIGN-rc-listener-lifecycle.md` v8.
**Does NOT change:** `rc.Enabled()` semantics, the marker shape, listener spawn ownership in `internal/rc`, worker/subagent attach behavior, or the FLEET_RC_BOOTSTRAP_DISABLED env-gate.

---

## TL;DR

Today: `fleet dispatch` spawns a coord. The coord-spawn branch in `cmd/fleet/dispatch.go` tries to inject `--remote-control "fleet-coord-<id>-<project>"` into the claude argv, but `injectRemoteControlFlagProject` consults `rc.Enabled(project)` — which reads the per-project `rc-enabled` marker. The marker is only written by `fleet rc up <project>` (operator opt-in). So a fresh coord spawn that hasn't been opt-in'd gets NO `--remote-control` flag, NO marker, and `fleet rc connect <project>` returns `marker absent`.

**This design adds one line to coord-spawn: write the marker before injecting the flag.** Every coord that fleet dispatches is auto-opt-in for RC. Workers and subagents don't reach this branch (`opts.coordSpawn=false`) so they stay plain — the v0.12 push-storm protection (which targeted runaway *reviewer subagents*, not coords) is preserved.

`rc.Enabled()` keeps its role as the single source of truth for "should attach surfaces inject `--remote-control`." The marker just becomes auto-populated for coords at the moment of spawn.

---

## Problem reproduction

```
$ fleet rc connect projects-fleet
{"outcome":"not_enabled","project":"projects-fleet","cmd":"connect",
 "error":"rc.Connect: marker absent for project \"projects-fleet\" (run `fleet rc up projects-fleet` first)"}
```

Inspection of the 4 live coords on the operator's host (rainier/spark/fleet/tatoosh) confirmed **none** had `--remote-control` in their persisted `command`. Every coord was spawned via the standard `fleet dispatch` coord-spawn path, which means the inject was attempted and gated out by `rc.Enabled(project) == false`.

The operator's workflow assumption: "I just spawned a coord; `/remote-control` and `fleet rc connect` should work." Today they don't.

---

## Root cause

`cmd/fleet/dispatch.go:891` — `injectRemoteControlFlagProject`:

```go
func injectRemoteControlFlagProject(command []string, sessionName, project string) []string {
    if os.Getenv("FLEET_RC_BOOTSTRAP_DISABLED") != "" {
        return command
    }
    if !rc.Enabled(project) {        // ← gate fires; coord has no marker
        return command                // ← argv returned unmodified
    }
    return spawn.InjectRemoteControlFlag(command, sessionName)
}
```

The gate is correct in isolation. The bug is upstream: coord-spawn never plants the marker that the gate reads.

V0.12's design (`DESIGN-rc-listener-lifecycle.md`) made the marker an **operator-explicit opt-in** to retire the implicit-spawn architecture that produced ~5,620 mobile-push events from a runaway reviewer subagent. That hazard was about *subagents*, not coords; the coord-spawn lockout is collateral damage.

---

## Design decision

**Coord-spawn auto-writes the marker.** Subagents and workers do not.

Why this is safe:
- The original push-storm came from a reviewer subagent that re-entered `seed_inbox` thousands of times. That subagent never has `opts.coordSpawn=true` — it's dispatched as a `general-purpose` Agent from inside the coord, not via `fleet dispatch --coord-spawn`. So gating coord-spawn separately from subagent-spawn is the right axis to cut.
- Workers and operator-shelled `fleet dispatch` (without `--coord-spawn`) keep the v0.12 behavior: no auto-marker, no auto-inject. Operator still has `fleet rc down <project>` to revoke.
- `rc.Enabled()` is unchanged. Every read site (`GateAttachFlag`, supervisor, `fleet rc connect`, etc.) keeps consulting the same marker — the marker just becomes auto-populated at coord-spawn time.

Why we don't drop `rc.Enabled()` entirely:
- The marker is read by ~10 surfaces in `internal/rc/` and elsewhere. Dropping the gate would force every reader to know about coord-spawn-vs-subagent context, which they don't have.
- Operator still wants `fleet rc down <project>` to actually disable RC for a project (e.g. to silence a noisy listener). Keeping the marker keeps this teardown path working.

---

## Implementation

### Change 1 — `cmd/fleet/dispatch.go` coord-spawn branch

In the `if opts.coordSpawn { ... }` block at line 435, before `injectRemoteControlFlagProject`:

```go
if opts.coordSpawn {
    preAllocatedID = agent.NewID()

    // v0.12.1 P0 fix: coord-spawn auto-opts in to RC so the gate
    // inside injectRemoteControlFlagProject (rc.Enabled) passes.
    // Workers/subagents don't enter this branch — they stay plain.
    if opts.project != "" {
        if err := rc.WriteMarker(opts.project); err != nil {
            // Non-fatal: log + continue. The inject will no-op,
            // and the operator can still recover via `fleet rc up`.
            // Spawn proceeds with plain claude argv.
            log.Printf("dispatch: rc.WriteMarker(%q) failed: %v", opts.project, err)
        }
    }

    rcSessionName := buildCoordRemoteControlSessionName(preAllocatedID, opts.project)
    rewritten := injectRemoteControlFlagProject(opts.command, rcSessionName, opts.project)
    if !sameCommand(rewritten, opts.command) {
        rewrittenExecArgv = rewritten
    }
}
```

Notes:
- `opts.project != ""` gate matches `rc.Enabled`'s defensive shape (empty project is legacy / untargeted dispatch; no marker write).
- `rc.WriteMarker` is already idempotent (zero-byte sentinel; re-writing is a no-op for readers). No "first time only" check needed.
- Failure of `WriteMarker` is logged but doesn't fail the dispatch — degrades gracefully to the pre-fix behavior (operator runs `fleet rc up` manually).
- Marker write happens BEFORE injection so the same dispatch call writes-then-reads its own marker (no race; same goroutine).

### Change 2 — `cmd/fleet/handoff.go` coord-replacement spawn

The same fix shape applies at the handoff spawn site (per `cmd/fleet/handoff.go`'s coord-replacement path). When a coord is replaced via handoff, the replacement is also a coord — it should also auto-opt-in. Locate the analogous coord-spawn branch and add the same `rc.WriteMarker(opts.project)` call. (Idempotent if the marker is already present from the predecessor coord, which is the common case.)

### Change 3 — log line

`log.Printf` is already used in `cmd/fleet/dispatch.go` for non-fatal warnings. Match that style.

### Change 4 — none in `internal/rc/`

`rc.WriteMarker` is already exported and used by `rc.Up`. No new helpers needed. `rc.Enabled` unchanged.

### Change 5 — none in worker/subagent path

`opts.coordSpawn == false` for workers and Agent-tool subagents. They never enter the modified branch. Verified by reading `internal/spawn/spawn.go` — the spawn surface is the same but the per-dispatch options differ.

---

## E2E test

New test file: `cmd/fleet/dispatch_rc_auto_marker_test.go` (or extend `dispatch_test.go`).

### Test 1 — Coord-spawn auto-writes marker + injects flag

```
Given: a clean ~/.fleet/projects/<test-project>/ (no marker)
When:  dispatch a coord via fleet dispatch --coord-spawn --project <test-project>
Then:
  - ~/.fleet/projects/<test-project>/rc-enabled exists (zero bytes)
  - persisted agent.Record.exec_command contains "--remote-control"
  - persisted exec_command contains "fleet-coord-<id>-<test-project>"
  - exec_command's session-name disambiguator matches the agent id
```

### Test 2 — Worker spawn does NOT write marker or inject flag

```
Given: a clean ~/.fleet/projects/<test-project>/ (no marker)
When:  dispatch a worker via fleet dispatch --project <test-project> (no --coord-spawn)
Then:
  - ~/.fleet/projects/<test-project>/rc-enabled does NOT exist
  - persisted agent.Record.command does NOT contain "--remote-control"
```

### Test 3 — `fleet rc connect` succeeds post-coord-spawn

This is the user-facing assertion. After Test 1's coord spawn:
```
When: fleet rc connect <test-project>
Then: outcome != "not_enabled" (specifically: outcome ∈ {acquired, already_connected} — listener-spawn outcome is out of scope for this gate-only test)
```

### Test 4 — `FLEET_RC_BOOTSTRAP_DISABLED` still overrides

```
Given: FLEET_RC_BOOTSTRAP_DISABLED=1 in env
When:  dispatch a coord via fleet dispatch --coord-spawn --project <test-project>
Then:
  - persisted exec_command does NOT contain "--remote-control" (env-gate wins)
  - marker MAY OR MAY NOT exist (degraded path — we'll mark MUST-EXIST for clarity since the env-gate is about inject, not marker)
```

Test 4 pins the invariant that the env-gate retains precedence; v0.13 retires it after the marker-gate is proven via the existing `cmd/fleet/rc_invariant_test.go`.

### Test 5 — `rc.WriteMarker` failure is non-fatal

Mock or make-readonly the project dir to force `WriteMarker` failure; verify dispatch succeeds, logs a warning, argv is plain claude (graceful degrade to pre-fix behavior).

---

## Non-goals (out of scope for this PR)

- Listener-spawn behavior (PR #159 owns that).
- The FLEET_RC_BOOTSTRAP_DISABLED env-gate retirement (v0.13).
- Auto-opt-in for non-coord dispatches (push-storm risk we explicitly preserve).
- Bookkeeping for the existing 14 stale supervisor entries.
- Backfill of marker for ALREADY-RUNNING coords (this fix only touches future spawns; existing coords get the marker when they next handoff or the operator runs `fleet rc up` manually).
- Renaming `--remote-control` to `--rc` (Claude Code CLI surface, not fleet's).

---

## Acceptance gate

- `go build ./...` clean.
- `go test -race -count=1 ./cmd/fleet/... ./internal/rc/...` green.
- `golangci-lint run ./...` clean.
- `python3 -m pytest skills/ -q` green (no skill changes expected; sanity check only).
- The 5 e2e tests above pass.
- `cmd/fleet/rc_invariant_test.go` still passes (the existing CI-invariant test that pins marker-gate behavior).
- PR body documents: the v0.12 "operator-explicit opt-in" stance is narrowed to "explicit for workers/subagents, automatic for coords." Update `docs/DESIGN-rc-listener-lifecycle.md` operator-behavior section to reflect this (one-paragraph amendment, not a full rewrite).

---

## Task split

One worker task, three bookkeeping tasks (the bookkeeping items the operator already approved in the answers preceding this design):

1. **`rc-coord-auto-marker-<hash>`** (P0) — implement Changes 1+2, add 5 e2e tests, update `DESIGN-rc-listener-lifecycle.md` operator-behavior paragraph. Branch off current main.
2. **`close-merged-rc-tasks-<hash>`** (P2) — `fleet tasks set` to flip `rc-listener-impl-v0-12-ed95` (#159) and `rc-session-name-include-ed60` (#155) to `status=done` with `pr_url` set. Pure CLI; no code changes; no PR.
3. **`park-pr2-amendment-docs-<hash>`** (P2) — move the unstaged `docs/DESIGN-dispatch-lifecycle.{md,html}` diff to a new branch `docs/pr2-scope-amendment`, open as a PR. Keeps the operator's restored amendment safe and out of the reconcile-pr-by-branch finisher's path.
4. **`reconcile-pr-by-branch-f3ef`** — already in-flight; finisher subagent dispatch is separate from this design doc.

Tasks 2 and 3 don't need TASK-PLAN-DOCs (mechanical / operator-pre-approved). Task 1 will get a `docs/TASK-PLAN-rc-coord-auto-marker-<hash>.md` before promotion.

---

## Operator approval timestamp

**G2 cleared:** 2026-05-18, operator response "yes, save the design doc" to the design proposal in chat.
