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

	"github.com/edisonshen/fleet/internal/agent"
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

// TestIsCoordHandoffForProject_GatesOnCoordTaskID is the codex review
// iter-1 [P1] regression, re-keyed onto the coord LEASE / task-based
// detector after the coord-spawn marker was deleted (D3). Handoff
// replacements MUST gate the rc-enabled auto-inject on whether the OLD
// agent is actually the project's coord — now determined by
// spawn.IsCoordSpawn(rec.TaskID, rec.Project) (TaskID == "coord-<project>"),
// not a marker file. Without this gate, a `fleet handoff <worker-id>`
// for a worker would auto-opt the project into RC and inject
// --remote-control into the worker replacement's argv, violating the
// v0.12 push-storm protection that keeps workers / subagents on strict
// opt-in.
//
// We test the predicate directly (not via runHandoff): the gate's
// correctness IS the predicate, and it is a pure function of the
// record's TaskID + Project — no on-disk state to seed.
func TestIsCoordHandoffForProject_GatesOnCoordTaskID(t *testing.T) {
	const project = "test-handoff-gate"
	const coordID = "aaaaaaaa"

	// Sub-test matrix: nil record / empty project / feature (worker) task
	// / coord task. Only a coord-<project> TaskID with a non-empty project
	// should report true.
	cases := []struct {
		name string
		rec  *agent.Record
		want bool
	}{
		{"nil record rejects", nil, false},
		{"empty project rejects",
			&agent.Record{ID: coordID, Project: "", TaskID: "coord-" + project}, false},
		{"feature task rejects",
			&agent.Record{ID: coordID, Project: project, TaskID: "auth-fix"}, false},
		{"worker task rejects",
			&agent.Record{ID: coordID, Project: project, TaskID: "worker-" + project}, false},
		{"coord task accepts",
			&agent.Record{ID: coordID, Project: project, TaskID: "coord-" + project}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isCoordHandoffForProject(tc.rec); got != tc.want {
				t.Errorf("isCoordHandoffForProject(%+v): got %v, want %v", tc.rec, got, tc.want)
			}
		})
	}
}
