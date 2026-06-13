// dispatch_rc_native_test.go pins the native default-on RC contract
// at the dispatch layer (rc-default-native-startup, operator directive
// 2026-05-29):
//
//	T1 — coord-spawn (opts.coordSpawn=true + opts.project!="") bakes
//	     `--remote-control "fleet-coord-<id>-<project>"` into the exec
//	     argv BY DEFAULT — no marker writes of any kind.
//	T2 — the rc-disabled opt-out marker suppresses the inject, and a
//	     coord dispatch must NOT remove it (the operator's `fleet rc
//	     down` survives dispatches).
//	T3 — non-coord (worker / Agent-tool subagent) dispatch writes no
//	     markers; the inject call sites are all inside coordSpawn /
//	     isCoordHandoff branches, so workers never carry the flag
//	     (push-storm protection lives at the call-site gates).
//	T4 — FLEET_RC_BOOTSTRAP_DISABLED env-gate wins over default-on:
//	     test paths never produce a flagged argv.
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
// — both AFTER the coord-spawn RC branch. The error itself is ignored;
// tests assert on marker files + helper side-effects.
func runDispatchIgnoringSpawnErr(t *testing.T, opts *dispatchOpts) {
	t.Helper()
	isolateTmuxSocket(t)
	var out bytes.Buffer
	_ = runDispatch(opts, &out)
}

// requireNoRCMarkers asserts neither the legacy rc-enabled marker nor
// the rc-disabled opt-out marker exists for project.
func requireNoRCMarkers(t *testing.T, fleetHome, project string) {
	t.Helper()
	for _, name := range []string{rc.MarkerFilename, rc.DisabledMarkerFilename} {
		p := filepath.Join(fleetHome, "projects", project, name)
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("dispatch must not write %s; found %s", name, p)
		} else if !os.IsNotExist(err) {
			t.Fatalf("unexpected stat err for %s: %v", p, err)
		}
	}
}

// TestCoordSpawn_NativeDefault_InjectsWithoutMarkers is T1.
func TestCoordSpawn_NativeDefault_InjectsWithoutMarkers(t *testing.T) {
	fleetHome := t.TempDir()
	t.Setenv("FLEET_HOME", fleetHome)
	// TestMain sets FLEET_RC_BOOTSTRAP_DISABLED=1 globally so test
	// dispatches never carry the flag into a real spawn. T1 exercises
	// the default-on gate at the HELPER level with the env-gate
	// cleared; the dispatch itself runs with a custom command (the
	// inject no-ops on non-wrapper shapes) so nothing flagged execs.
	t.Setenv("FLEET_RC_BOOTSTRAP_DISABLED", "")

	const project = "test-native-default"
	seedRecoveryRepo(t, fleetHome, project) // coord spawn binds via resolver (PR3)
	opts := &dispatchOpts{
		taskID:          "coord-" + project,
		project:         project,
		projectExplicit: true,
		coordSpawn:      true,
		command:         []string{"sleep", "30"},
		commandExplicit: true,
	}
	runDispatchIgnoringSpawnErr(t, opts)

	// Native model: NO markers anywhere — neither the legacy opt-in
	// nor the opt-out may appear as a dispatch side effect.
	requireNoRCMarkers(t, fleetHome, project)

	// The production inject helper rewrites the default claude wrapper
	// BY DEFAULT (no marker required).
	rcSessionName := buildCoordRemoteControlSessionName("abcdef12", project)
	rewritten := injectRemoteControlFlagProject(defaultClaudeCommand, rcSessionName, project)
	if sameCommand(rewritten, defaultClaudeCommand) {
		t.Fatalf("injectRemoteControlFlagProject MUST rewrite the default argv by default (native model); got pass-through")
	}
	if len(rewritten) < 3 {
		t.Fatalf("rewritten argv truncated; got len=%d want >=3: %v", len(rewritten), rewritten)
	}
	wantSubstr := `--remote-control "` + rcSessionName + `"`
	if !strings.Contains(rewritten[2], wantSubstr) {
		t.Errorf("rewritten wrapper body missing %q: %q", wantSubstr, rewritten[2])
	}
}

// TestCoordSpawn_OptOutSuppressesInject is T2: the rc-disabled marker
// gates the flag off, and a coord dispatch must not remove it.
func TestCoordSpawn_OptOutSuppressesInject(t *testing.T) {
	fleetHome := t.TempDir()
	t.Setenv("FLEET_HOME", fleetHome)
	t.Setenv("FLEET_RC_BOOTSTRAP_DISABLED", "")

	const project = "test-opt-out"
	seedRecoveryRepo(t, fleetHome, project)
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if err := rc.WriteDisabledMarker(project); err != nil {
		t.Fatalf("WriteDisabledMarker: %v", err)
	}

	opts := &dispatchOpts{
		taskID:          "coord-" + project,
		project:         project,
		projectExplicit: true,
		coordSpawn:      true,
		command:         []string{"sleep", "30"},
		commandExplicit: true,
	}
	runDispatchIgnoringSpawnErr(t, opts)

	// Operator opt-out must survive the dispatch.
	if !rc.DisabledMarkerPresent(project) {
		t.Fatalf("coord dispatch must NOT remove the operator's rc-disabled opt-out marker")
	}
	// And the inject helper respects it.
	rcSessionName := buildCoordRemoteControlSessionName("abcdef12", project)
	rewritten := injectRemoteControlFlagProject(defaultClaudeCommand, rcSessionName, project)
	if !sameCommand(rewritten, defaultClaudeCommand) {
		t.Fatalf("opted-out project must NOT get --remote-control; got rewrite %v", rewritten)
	}
}

// TestWorkerSpawn_NoMarkers_NoInjectCallSite is T3: worker dispatches
// write no markers. (The flag itself can't reach workers because every
// inject call site is gated on coordSpawn / isCoordHandoff — pinned by
// the handoff/handoffop gate tests and rc_invariant_test.go.)
func TestWorkerSpawn_NoMarkers_NoInjectCallSite(t *testing.T) {
	fleetHome := t.TempDir()
	t.Setenv("FLEET_HOME", fleetHome)
	t.Setenv("FLEET_RC_BOOTSTRAP_DISABLED", "")

	const project = "test-worker-no-markers"
	opts := &dispatchOpts{
		taskID:          "worker-task",
		project:         project,
		projectExplicit: true,
		// coordSpawn deliberately left false: this is the worker /
		// operator-shelled-dispatch / Agent-tool-subagent path.
		command:         []string{"sleep", "30"},
		commandExplicit: true,
	}
	runDispatchIgnoringSpawnErr(t, opts)

	requireNoRCMarkers(t, fleetHome, project)
}

// TestCoordSpawn_EnvGateOverridesDefault is T4: the env-gate keeps
// precedence over the native default — with FLEET_RC_BOOTSTRAP_DISABLED
// set (the test suite's global hygiene default), the argv stays plain
// even though rc.Enabled(project) is true.
func TestCoordSpawn_EnvGateOverridesDefault(t *testing.T) {
	fleetHome := t.TempDir()
	t.Setenv("FLEET_HOME", fleetHome)
	t.Setenv("FLEET_RC_BOOTSTRAP_DISABLED", "1")

	const project = "test-env-gate-precedence"
	if !rc.Enabled(project) {
		t.Fatalf("precondition: project must be default-enabled")
	}
	rcSessionName := buildCoordRemoteControlSessionName("cafebabe", project)
	rewritten := injectRemoteControlFlagProject(defaultClaudeCommand, rcSessionName, project)
	if !sameCommand(rewritten, defaultClaudeCommand) {
		t.Fatalf("FLEET_RC_BOOTSTRAP_DISABLED MUST keep precedence over default-on; injectRemoteControlFlagProject rewrote argv anyway")
	}
}

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
