// dispatch_rc_auto_marker_test.go pins the v0.12.1 P0 fix:
// `fleet dispatch --coord-spawn --project <p>` MUST auto-write the
// per-project `~/.fleet/projects/<p>/rc-enabled` marker BEFORE the
// existing `injectRemoteControlFlagProject` call so the marker-gate
// inside that helper (which consults `rc.Enabled(project)`) passes
// on the same dispatch. Without this, freshly-dispatched coords spawn
// with plain claude argv and `/remote-control` + `fleet rc connect`
// return `not_enabled` until the operator manually runs `fleet rc up`.
//
// See docs/DESIGN-rc-coord-auto-marker.md (operator G2 2026-05-18) and
// docs/TASK-PLAN-rc-coord-auto-marker-282c.md.
//
// Invariants pinned here:
//
//	T1 — coord-spawn (opts.coordSpawn=true + opts.project!="") writes
//	     the per-project marker. The subsequent inject helper now sees
//	     rc.Enabled(project)=true and rewrites the argv to embed
//	     `--remote-control "fleet-coord-<id>-<project>"`.
//	T2 — non-coord (worker / Agent-tool subagent) dispatch does NOT
//	     enter the modified branch: NO marker, NO inject. Pins the
//	     v0.12 push-storm protection (reviewer-subagent runaways stay
//	     opt-in only).
//	T3 — after T1's marker write, the marker-gate inside `rc.Connect`
//	     (the gate this PR fixes) clears: the returned error does NOT
//	     say `marker absent for project`. Downstream listener-spawn
//	     state is OUT OF SCOPE for this PR; we only verify the gate.
//	T4 — FLEET_RC_BOOTSTRAP_DISABLED=1 still wins on the inject: env-
//	     gate is checked FIRST inside injectRemoteControlFlagProject,
//	     so even with the marker auto-written, the argv stays plain.
//	     Pins the v0.12 env-gate precedence; v0.13 retires the env-gate.
//	T5 — rc.WriteMarker failure is non-fatal: dispatch continues with
//	     plain claude argv (graceful degrade); no fatal error to operator.
//	     Uses the writeMarkerFn package-var seam to inject a failing stub.
package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edisonshen/fleet/internal/rc"
	"github.com/edisonshen/fleet/internal/state"
)

// runDispatchIgnoringSpawnErr invokes runDispatch with a clean
// FLEET_HOME and isolated tmux socket. In CI without a real tmux server
// (the dominant case) runDispatch fails at tmux.Available or spawn.Spawn
// — both AFTER the coord-spawn branch's marker write. The error itself
// is ignored; tests assert on the marker file + helper side-effects.
//
// Mirrors the pattern used by TestDispatch_CoordSpawnAcceptsExplicitProject
// in dispatch_test.go: don't require tmux just to verify a code path that
// runs before spawn.
func runDispatchIgnoringSpawnErr(t *testing.T, opts *dispatchOpts) {
	t.Helper()
	isolateTmuxSocket(t)
	var out bytes.Buffer
	_ = runDispatch(opts, &out)
}

// TestCoordSpawn_AutoWritesMarker_InjectsRemoteControl is T1: the
// load-bearing positive assertion. Coord-spawn with an explicit project
// writes ~/.fleet/projects/<p>/rc-enabled and the inject helper (now
// gated by rc.Enabled which observes the just-written marker) rewrites
// the default argv to embed `--remote-control "fleet-coord-<id>-<p>"`.
func TestCoordSpawn_AutoWritesMarker_InjectsRemoteControl(t *testing.T) {
	fleetHome := t.TempDir()
	t.Setenv("FLEET_HOME", fleetHome)
	// TestMain sets FLEET_RC_BOOTSTRAP_DISABLED=1 globally to keep
	// other tests from spawning real listeners. T1 specifically
	// exercises the marker-gate, so clear the env-gate for the
	// duration of this test (t.Setenv auto-restores on test exit).
	t.Setenv("FLEET_RC_BOOTSTRAP_DISABLED", "")

	const project = "test-auto-marker"
	opts := &dispatchOpts{
		taskID:          "coord-" + project,
		project:         project,
		projectExplicit: true,
		coordSpawn:      true,
	}
	runDispatchIgnoringSpawnErr(t, opts)

	// Marker MUST exist post-dispatch — runDispatch's coord-spawn
	// branch writes it BEFORE spawn.Spawn, so the file persists even
	// when the tmux-less CI environment makes spawn.Spawn fail later.
	markerPath := filepath.Join(fleetHome, "projects", project, rc.MarkerFilename)
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("coord-spawn must auto-write rc-enabled marker at %s; stat err: %v",
			markerPath, err)
	}

	// Cross-check the inject behavior: with the marker now present
	// AND the env-gate cleared, calling injectRemoteControlFlagProject
	// against the default claude wrapper MUST rewrite the argv. This
	// pins that the marker write enables the production inject path,
	// not just that the file lands on disk.
	rcSessionName := buildCoordRemoteControlSessionName("abcdef12", project)
	rewritten := injectRemoteControlFlagProject(defaultClaudeCommand, rcSessionName, project)
	if sameCommand(rewritten, defaultClaudeCommand) {
		t.Fatalf("post-coord-spawn injectRemoteControlFlagProject MUST rewrite the default argv when the marker is auto-written and env-gate is clear; got pass-through")
	}
	// The rewrite embeds `--remote-control "<sessionName>"`. argv[2] is
	// the wrapper script body in the default `sh -c <script>` shape.
	if len(rewritten) < 3 {
		t.Fatalf("rewritten argv truncated; got len=%d want >=3: %v", len(rewritten), rewritten)
	}
	wantSubstr := `--remote-control "` + rcSessionName + `"`
	if !strings.Contains(rewritten[2], wantSubstr) {
		t.Errorf("rewritten wrapper body missing %q: %q", wantSubstr, rewritten[2])
	}
}

// TestWorkerSpawn_NoMarker_NoRemoteControl is T2: pins that the auto-
// marker write is EXCLUSIVE to the coord-spawn branch. A non-coord
// dispatch (opts.coordSpawn=false — workers, Agent-tool subagents,
// operator-shelled `fleet dispatch`) MUST NOT write the marker, MUST
// NOT trigger the inject. This preserves the v0.12 push-storm
// protection that targets runaway reviewer subagents (which never
// reach this branch since they spawn through the Agent tool, not via
// --coord-spawn).
func TestWorkerSpawn_NoMarker_NoRemoteControl(t *testing.T) {
	fleetHome := t.TempDir()
	t.Setenv("FLEET_HOME", fleetHome)
	// Clear the env-gate for parity with T1 — if a worker dispatch
	// EVER writes the marker, the inject would fire and we'd push to
	// the operator's mobile.  Both gates (marker absence + env) are
	// the v0.12 protections; here we prove the marker-side stays
	// untouched.
	t.Setenv("FLEET_RC_BOOTSTRAP_DISABLED", "")

	const project = "test-worker-no-marker"
	opts := &dispatchOpts{
		taskID:          "worker-task",
		project:         project,
		projectExplicit: true,
		// coordSpawn deliberately left false: this is the worker /
		// operator-shelled-dispatch / Agent-tool-subagent path.
	}
	runDispatchIgnoringSpawnErr(t, opts)

	markerPath := filepath.Join(fleetHome, "projects", project, rc.MarkerFilename)
	if _, err := os.Stat(markerPath); err == nil {
		t.Fatalf("worker (non-coord) dispatch MUST NOT auto-write rc-enabled marker; found %s", markerPath)
	} else if !os.IsNotExist(err) {
		t.Fatalf("unexpected stat err for %s: %v", markerPath, err)
	}

	// Cross-check inject-path: marker absent => helper passes argv
	// through unchanged. (Redundant with rc_invariant_test.go but
	// pinned here too because this test's failure shape should
	// localize to "worker path leaked marker write" rather than the
	// invariant test's broad sweep.)
	rcSessionName := buildCoordRemoteControlSessionName("deadbeef", project)
	rewritten := injectRemoteControlFlagProject(defaultClaudeCommand, rcSessionName, project)
	if !sameCommand(rewritten, defaultClaudeCommand) {
		t.Errorf("with marker absent, injectRemoteControlFlagProject MUST return argv unchanged; got rewrite")
	}
}

// TestCoordSpawn_RCConnectGateClears is T3: the user-facing assertion.
// After coord-spawn auto-writes the marker, the marker-gate at the top
// of rc.Connect (which returns OutcomeNotEnabled + "marker absent for
// project" error when Enabled(project)=false) MUST NO LONGER fire. The
// downstream listener-spawn state (rc-state.json + live PID + argv-
// match) is out of scope for this PR — those gates may still fire and
// return OutcomeNotEnabled with a DIFFERENT error message. We pin
// specifically that the marker-gate clears: the error must NOT contain
// "marker absent for project".
func TestCoordSpawn_RCConnectGateClears(t *testing.T) {
	fleetHome := t.TempDir()
	t.Setenv("FLEET_HOME", fleetHome)
	t.Setenv("FLEET_RC_BOOTSTRAP_DISABLED", "")

	const project = "test-rc-connect-gate"
	opts := &dispatchOpts{
		taskID:          "coord-" + project,
		project:         project,
		projectExplicit: true,
		coordSpawn:      true,
	}
	runDispatchIgnoringSpawnErr(t, opts)

	// Confirm marker landed (defensive against a regression that
	// would make this whole test trivially pass for the wrong reason).
	if !rc.MarkerPresent(project) {
		t.Fatalf("precondition: coord-spawn must have written the marker; rc.MarkerPresent(%q)=false", project)
	}

	// Drive rc.Connect. In CI without a live coord, this WILL fail —
	// but with a DIFFERENT error than the marker-gate. The marker-gate
	// is line 51-53 of internal/rc/connect.go; it's the one this PR
	// fixes.
	res, err := rc.Connect(project, rc.ConnectOpts{})
	if err == nil {
		// Surprising in CI (no live coord / no listener), but if it
		// happens the gate obviously cleared. Fine.
		return
	}
	if res.Outcome == rc.OutcomeNotEnabled && strings.Contains(err.Error(), "marker absent for project") {
		t.Fatalf("marker-gate inside rc.Connect must NOT fire after coord-spawn auto-write; got outcome=%q err=%q",
			res.Outcome, err.Error())
	}
}

// TestCoordSpawn_EnvGateOverridesAutoMarker is T4: pins that the env-
// gate (FLEET_RC_BOOTSTRAP_DISABLED=1) retains precedence over the
// auto-marker. The env check inside injectRemoteControlFlagProject
// runs BEFORE the rc.Enabled check, so even with the marker
// auto-written, the argv stays plain. The marker IS still written
// (we don't couple marker-write to env state — the write is atomic
// with "this coord exists"); env-gate only suppresses the inject.
//
// v0.13 retires FLEET_RC_BOOTSTRAP_DISABLED after the marker-gate is
// field-proven via rc_invariant_test.go.
func TestCoordSpawn_EnvGateOverridesAutoMarker(t *testing.T) {
	fleetHome := t.TempDir()
	t.Setenv("FLEET_HOME", fleetHome)
	// EXPLICITLY set the env-gate (matches TestMain's default for the
	// rest of the cmd/fleet test binary; restated here for self-
	// documenting test intent).
	t.Setenv("FLEET_RC_BOOTSTRAP_DISABLED", "1")

	const project = "test-env-gate-precedence"
	opts := &dispatchOpts{
		taskID:          "coord-" + project,
		project:         project,
		projectExplicit: true,
		coordSpawn:      true,
	}
	runDispatchIgnoringSpawnErr(t, opts)

	// Marker IS written (operator-explicit-coord-existence semantic
	// doesn't depend on env state). Pinning this here so a future
	// refactor that couples marker write to env doesn't slip in
	// silently.
	markerPath := filepath.Join(fleetHome, "projects", project, rc.MarkerFilename)
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("auto-marker write MUST be independent of FLEET_RC_BOOTSTRAP_DISABLED; marker missing at %s: %v",
			markerPath, err)
	}

	// Inject MUST be suppressed: env-gate wins inside
	// injectRemoteControlFlagProject (checked BEFORE rc.Enabled).
	rcSessionName := buildCoordRemoteControlSessionName("cafebabe", project)
	rewritten := injectRemoteControlFlagProject(defaultClaudeCommand, rcSessionName, project)
	if !sameCommand(rewritten, defaultClaudeCommand) {
		t.Fatalf("FLEET_RC_BOOTSTRAP_DISABLED MUST keep precedence over the auto-written marker; injectRemoteControlFlagProject rewrote argv anyway")
	}
}

// TestCoordSpawn_MarkerWriteFailure_Degrades is T5: pins the graceful-
// degrade contract. If rc.WriteMarker returns an error inside the
// coord-spawn branch, the dispatch MUST continue (the v0.12
// pre-fix behavior is the graceful fallback — operator can still
// recover via `fleet rc up`). Uses the writeMarkerFn package-var seam
// (introduced by this PR) so the test can stub failure without FS
// permission tricks that are brittle on macOS.
func TestCoordSpawn_MarkerWriteFailure_Degrades(t *testing.T) {
	fleetHome := t.TempDir()
	t.Setenv("FLEET_HOME", fleetHome)
	t.Setenv("FLEET_RC_BOOTSTRAP_DISABLED", "")

	// Stub writeMarkerFn to always fail. t.Cleanup restores.
	prev := writeMarkerFn
	stubErr := &writeMarkerStubErr{msg: "simulated marker write failure"}
	writeMarkerFn = func(project string) error { return stubErr }
	t.Cleanup(func() { writeMarkerFn = prev })

	const project = "test-marker-fail-degrade"
	opts := &dispatchOpts{
		taskID:          "coord-" + project,
		project:         project,
		projectExplicit: true,
		coordSpawn:      true,
	}
	// Run + capture stdout to verify the non-fatal warning was emitted.
	isolateTmuxSocket(t)
	var out bytes.Buffer
	// runDispatch will still fail at tmux.Available / spawn.Spawn in
	// CI without tmux — that's the EXPECTED failure (post-marker
	// branch), NOT a fatal-from-our-code error. The contract pinned
	// here is that the marker-write failure itself doesn't short-
	// circuit dispatch with a different error message.
	err := runDispatch(opts, &out)
	if err != nil && strings.Contains(err.Error(), "simulated marker write failure") {
		t.Fatalf("rc.WriteMarker failure MUST be non-fatal; dispatch returned: %v", err)
	}

	// Marker MUST be absent (stub returned error before writing). This
	// pins that the stub actually fired and the production code
	// didn't fall through to a hard-coded rc.WriteMarker bypass.
	markerPath := filepath.Join(fleetHome, "projects", project, rc.MarkerFilename)
	if _, statErr := os.Stat(markerPath); statErr == nil {
		t.Fatalf("expected stub to suppress marker write; found %s on disk", markerPath)
	}

	// Cross-check inject: marker absent ⇒ inject suppressed.
	// Confirms the dispatch did NOT silently force-inject anyway.
	rcSessionName := buildCoordRemoteControlSessionName("babefeed", project)
	rewritten := injectRemoteControlFlagProject(defaultClaudeCommand, rcSessionName, project)
	if !sameCommand(rewritten, defaultClaudeCommand) {
		t.Errorf("with marker-write stubbed to fail, injectRemoteControlFlagProject must NOT rewrite argv; got rewrite")
	}
}

// writeMarkerStubErr is a typed error used by T5 to assert the
// production code's non-fatal handling didn't accidentally surface
// our stubbed error as the dispatch's exit error.
type writeMarkerStubErr struct{ msg string }

func (e *writeMarkerStubErr) Error() string { return e.msg }

// TestIsCoordHandoffForProject_GatesOnCoordSpawnMarker is the codex
// review iter-1 [P1] regression: handoff replacements MUST gate the
// rc-enabled marker auto-write on whether the OLD agent is actually
// the project's coord (coord-spawn marker resolves to oldRec.ID).
// Without this gate, a `fleet handoff <worker-id>` for a worker in a
// project with no rc-enabled marker would auto-opt the project into
// RC and inject --remote-control into the worker replacement's argv,
// violating the v0.12 push-storm protection that keeps workers /
// subagents on strict opt-in.
//
// We test the predicate directly (not via runHandoff) because the
// gate's correctness IS the predicate; runHandoff's early-exit gates
// (tmux probe / dead-session archive / legacy-record refusal) would
// short-circuit before reaching the marker write in CI without tmux,
// and exercising those gates only proves the marker isn't written —
// not WHY it isn't written. The predicate test isolates the gate
// from the surrounding handoff state machine.
func TestIsCoordHandoffForProject_GatesOnCoordSpawnMarker(t *testing.T) {
	fleetHome := t.TempDir()
	t.Setenv("FLEET_HOME", fleetHome)

	const project = "test-handoff-gate"
	const coordID = "aaaaaaaa"
	const workerID = "bbbbbbbb"

	// Sub-test matrix: empty project / unset marker / marker == workerID
	// / marker == coordID. Only the last case should report true.
	cases := []struct {
		name      string
		project   string
		agentID   string
		setMarker string // "" = don't write marker
		want      bool
	}{
		{"empty project rejects", "", coordID, "", false},
		{"unset marker rejects", project, coordID, "", false},
		{"marker points elsewhere rejects",
			project, workerID, coordID, false},
		{"marker matches agentID accepts",
			project, coordID, coordID, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Each sub-test gets a fresh project dir so markers from a
			// previous case don't leak. WriteCoordSpawnMarker stamps
			// the marker file; absent setMarker, the previous case's
			// marker would otherwise persist across iterations.
			subHome := t.TempDir()
			t.Setenv("FLEET_HOME", subHome)
			if tc.setMarker != "" {
				// state.WriteCoordSpawnMarker requires the per-project
				// .locks/ parent to exist; bootstrap initialises ~/.fleet
				// + the project tree.
				if _, err := state.Bootstrap(); err != nil {
					t.Fatalf("setup Bootstrap: %v", err)
				}
				if _, err := state.EnsureProjectInitialized(tc.project); err != nil {
					t.Fatalf("setup EnsureProjectInitialized: %v", err)
				}
				if err := state.WriteCoordSpawnMarker(tc.project, tc.setMarker); err != nil {
					t.Fatalf("setup WriteCoordSpawnMarker: %v", err)
				}
			}
			got := isCoordHandoffForProject(tc.project, tc.agentID)
			if got != tc.want {
				t.Errorf("isCoordHandoffForProject(%q, %q) with marker=%q: got %v, want %v",
					tc.project, tc.agentID, tc.setMarker, got, tc.want)
			}
		})
	}
}
