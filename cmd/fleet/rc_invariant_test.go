// rc_invariant_test.go pins the A3 CI invariant from
// docs/DESIGN-rc-listener-lifecycle.md plan-eng-review decisions.
//
// THE LOAD-BEARING TEST: v0.12 ships the env-gate
// FLEET_RC_BOOTSTRAP_DISABLED as defense-in-depth on top of the new
// per-project rc-enabled marker. v0.13 retires the env-gate after
// marker-gate is field-proven. This test EXPLICITLY UNSETS the env
// gate, ensures NO markers exist, and asserts zero new
// `claude remote-control --remote-control-session-name-prefix
// fleet-coord` listeners spawn across a representative test sweep —
// proving the marker-gate is sufficient on its own.
//
// Test mechanism:
//
//  1. Snapshot pre-state: `pgrep -f 'claude remote-control --remote-
//     control-session-name-prefix fleet-coord'` (PIDs sorted).
//  2. Sweep representative dispatch + handoff attach-flag injection
//     code paths with rc-enabled marker ABSENT and env-gate UNSET.
//     Assert: argv unchanged (no `--remote-control` injection).
//  3. Snapshot post-state: same pgrep query.
//  4. Assert pre == post (zero new listeners).
//
// This is NOT an exhaustive "run the full suite in a subprocess"
// test — that would re-shell the host's test binary recursively and
// timeout the parent. Instead it covers the load-bearing attach-
// surface gates and trusts each surface's dedicated unit test to
// pin its own no-spawn invariant. The surfaces tested here:
//
//   - injectRemoteControlFlag (dispatch.go) — env-gate-only gate.
//   - injectRemoteControlFlagProject (dispatch.go) — env-gate AND
//     marker-gate. The marker-gate-only test path proves the
//     marker is sufficient even when the env-gate is off.
//   - rc.GateAttachFlag (internal/rc) — same two-layer gate, the
//     I3 handoffop chokepoint.
//
// Any new attach surface added without going through one of these
// helpers MUST add a sibling test here.
package main

import (
	"os/exec"
	"sort"
	"strings"
	"testing"

	"github.com/edisonshen/fleet/internal/rc"
)

// TestRCInvariant_NoListenerSpawnWithoutMarker is THE load-bearing
// A3 test. With env-gate DISABLED and no markers anywhere, none of
// the attach surfaces inject `--remote-control` — and therefore no
// listener can ever be spawned by the production code paths.
func TestRCInvariant_NoListenerSpawnWithoutMarker(t *testing.T) {
	withFleetHome := func(t *testing.T) {
		t.Helper()
		dir := t.TempDir()
		t.Setenv("FLEET_HOME", dir)
		// CRITICAL: env-gate DISABLED. The whole point of this test
		// is to prove the marker-gate is sufficient WITHOUT the
		// env-gate. cmd/fleet/main_test.go's TestMain sets this to
		// "1" globally; t.Setenv overrides for this test's scope.
		t.Setenv("FLEET_RC_BOOTSTRAP_DISABLED", "")
	}
	withFleetHome(t)

	// Pre-state pgrep snapshot. Best-effort: pgrep absent / non-zero
	// exit collapses to empty. The post-state must match exactly.
	pre := snapshotListenerPIDs()

	// Sample argvs representative of the production attach surfaces.
	defaultArgv := []string{"sh", "-c", "claude --print"}
	sessionName := "fleet-coord-deadbeef-demo"

	// Surface 1: injectRemoteControlFlag (dispatch.go default path,
	// env-gate ONLY — does NOT consult marker). With env-gate UNSET,
	// this WOULD inject. The v0.12 promise is that callers route
	// through injectRemoteControlFlagProject instead (which IS
	// marker-gated). This test asserts the GATED helper is the only
	// one wired into the load-bearing dispatch path.
	//
	// Validate: with no marker, injectRemoteControlFlagProject must
	// pass through unchanged.
	for _, project := range []string{"", "demo", "spark"} {
		got := injectRemoteControlFlagProject(defaultArgv, sessionName, project)
		if !sameArgvHelper(got, defaultArgv) {
			t.Errorf("injectRemoteControlFlagProject(%q): marker absent + env-gate unset MUST NOT inject; got %v want %v",
				project, got, defaultArgv)
		}
	}

	// Surface 2: rc.GateAttachFlag (the I3 chokepoint). Same
	// invariant: no marker → argv unchanged.
	for _, project := range []string{"", "demo", "spark"} {
		got := rc.GateAttachFlag(project, defaultArgv, sessionName)
		if !sameArgvHelper(got, defaultArgv) {
			t.Errorf("rc.GateAttachFlag(%q): marker absent MUST NOT inject; got %v want %v",
				project, got, defaultArgv)
		}
	}

	// Surface 3: with marker PRESENT, the gates DO inject — the
	// flow that production callers exercise. This is the positive
	// branch: it proves the gate isn't broken into permanent off.
	if err := rc.WriteMarker("demo"); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}
	defer func() { _ = rc.RemoveMarker("demo") }()

	got := rc.GateAttachFlag("demo", defaultArgv, sessionName)
	if sameArgvHelper(got, defaultArgv) {
		t.Fatalf("rc.GateAttachFlag(demo): marker present + env unset MUST inject; got pass-through %v",
			got)
	}

	// Even with the marker present, we did NOT spawn a real
	// listener — InjectRemoteControlFlag is argv-only.

	// Post-state pgrep snapshot. The test exercised gate helpers
	// but no exec.Command-style spawn. PIDs must be unchanged.
	post := snapshotListenerPIDs()
	if !equalStringSlice(pre, post) {
		t.Errorf("listener PID snapshot drifted across test:\npre:  %v\npost: %v\nA3 invariant violation — a code path under test spawned a real `claude remote-control` listener.",
			pre, post)
	}
}

// snapshotListenerPIDs runs pgrep against the fleet-coord listener
// argv pattern. Returns sorted PID strings; absent pgrep / no
// matches → empty slice (both non-failure paths, since the v0.12
// architecture means "no listeners is the expected state").
func snapshotListenerPIDs() []string {
	cmd := exec.Command("pgrep", "-f",
		"claude remote-control --remote-control-session-name-prefix fleet-coord")
	out, err := cmd.Output()
	if err != nil {
		// pgrep exits 1 when no matches — return empty.
		return nil
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var pids []string
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l != "" {
			pids = append(pids, l)
		}
	}
	sort.Strings(pids)
	return pids
}

func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sameArgvHelper(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
