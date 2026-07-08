// rc_invariant_test.go pins the native-model RC invariants
// (rc-default-native-startup; lineage: the v0.12 A3 CI invariant from
// docs/DESIGN-rc-listener-lifecycle.md).
//
// THE LOAD-BEARING TEST. Native model:
//
//   - RC is DEFAULT-ON: with no markers and the env-gate unset, the
//     attach-flag helpers DO rewrite a coord argv to carry
//     `--remote-control` — that's the feature.
//   - The opt-out is honored: the per-project rc-disabled marker
//     (`fleet rc down`) suppresses the rewrite on every surface.
//   - Empty/invalid projects never get the flag.
//   - The FLEET_RC_BOOTSTRAP_DISABLED env-gate (test hygiene) beats
//     everything, so the test suite can never produce a flagged argv
//     that something might exec.
//   - NO CODE PATH SPAWNS A STANDALONE LISTENER. The v0.12 listener
//     spawner is deleted from production; this test pins the absence
//     with a pgrep pre/post snapshot. The 5,620-mobile-push incident
//     class (reviewer loop respawning listeners) is structurally
//     impossible when nothing can spawn one.
//
// Test mechanism:
//
//  1. Snapshot pre-state: `pgrep -f 'claude remote-control --remote-
//     control-session-name-prefix fleet-coord'` (PIDs sorted).
//  2. Drive the attach-flag helpers through default-on / opt-out /
//     empty-project / env-gate cases. Assert rewrite vs pass-through.
//  3. Snapshot post-state: same pgrep query. Assert no NEW PID
//     appeared (unrelated legacy listeners exiting mid-test are
//     tolerated; an appearing one is the violation).
//
// The surfaces covered here:
//
//   - spawn.CoordRCInjector (wired by cmd/fleet) — env-gate AND
//     opt-out gate; used by the centralized coord-spawn seam.
//   - rc.GateAttachFlag (internal/rc) — same two-layer gate, the
//     gate primitive underneath the wiring seam.
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
	"github.com/edisonshen/fleet/internal/spawn"
)

// TestRCInvariant_NativeDefaultOn_OptOutHonored_NoListenerSpawn is THE
// load-bearing invariant test for the native model.
func TestRCInvariant_NativeDefaultOn_OptOutHonored_NoListenerSpawn(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FLEET_HOME", dir)
	// CRITICAL: env-gate CLEARED. This test proves the opt-out marker
	// gate is sufficient WITHOUT the env-gate, and that even the
	// default-on rewrite path cannot spawn a listener (it's argv-only;
	// the spawner no longer exists). cmd/fleet/main_test.go's TestMain
	// sets the gate to "1" globally; t.Setenv overrides for this scope.
	t.Setenv("FLEET_RC_BOOTSTRAP_DISABLED", "")

	// Pre-state pgrep snapshot. Best-effort: pgrep absent / non-zero
	// exit collapses to empty.
	pre := snapshotListenerPIDs()

	defaultArgv := []string{"sh", "-c", "claude --print"}
	sessionName := "fleet-coord-deadbeef-demo"

	// Invariant 1 — DEFAULT-ON: a valid project with no markers gets
	// the flag on both surfaces.
	for _, surface := range []struct {
		name string
		fn   func(project string) []string
	}{
		{"spawn.CoordRCInjector", func(p string) []string {
			return spawn.CoordRCInjector(p, "deadbeef", defaultArgv)
		}},
		{"rc.GateAttachFlag", func(p string) []string {
			return rc.GateAttachFlag(p, defaultArgv, sessionName)
		}},
	} {
		got := surface.fn("demo")
		if sameArgvHelper(got, defaultArgv) {
			t.Errorf("%s(demo): native default-on MUST inject with no markers; got pass-through %v",
				surface.name, got)
		}

		// Invariant 2 — empty project never gets the flag.
		got = surface.fn("")
		if !sameArgvHelper(got, defaultArgv) {
			t.Errorf("%s(\"\"): empty project MUST NOT inject; got %v", surface.name, got)
		}
	}

	// Invariant 3 — OPT-OUT honored: rc-disabled suppresses on both
	// surfaces.
	if err := rc.WriteDisabledMarker("demo"); err != nil {
		t.Fatalf("WriteDisabledMarker: %v", err)
	}
	if got := spawn.CoordRCInjector("demo", "deadbeef", defaultArgv); !sameArgvHelper(got, defaultArgv) {
		t.Errorf("spawn.CoordRCInjector(demo): rc-disabled marker MUST suppress inject; got %v", got)
	}
	if got := rc.GateAttachFlag("demo", defaultArgv, sessionName); !sameArgvHelper(got, defaultArgv) {
		t.Errorf("rc.GateAttachFlag(demo): rc-disabled marker MUST suppress inject; got %v", got)
	}
	if err := rc.RemoveDisabledMarker("demo"); err != nil {
		t.Fatalf("RemoveDisabledMarker: %v", err)
	}

	// Invariant 4 — env-gate beats default-on (the test suite's global
	// hygiene guarantee).
	t.Setenv("FLEET_RC_BOOTSTRAP_DISABLED", "1")
	if got := spawn.CoordRCInjector("demo", "deadbeef", defaultArgv); !sameArgvHelper(got, defaultArgv) {
		t.Errorf("spawn.CoordRCInjector(demo): env-gate MUST suppress inject; got %v", got)
	}
	if got := rc.GateAttachFlag("demo", defaultArgv, sessionName); !sameArgvHelper(got, defaultArgv) {
		t.Errorf("rc.GateAttachFlag(demo): env-gate MUST suppress inject; got %v", got)
	}

	// Invariant 5 — zero listener spawn. Everything above is argv-only
	// rewriting; production no longer contains a listener spawner at
	// all. Assert no NEW PID appears (post ⊆ pre) rather than exact
	// equality — an unrelated legacy listener on the host exiting
	// mid-test must not fail the suite (/review specialist note); only
	// an APPEARING listener is an invariant violation.
	post := snapshotListenerPIDs()
	preSet := map[string]bool{}
	for _, p := range pre {
		preSet[p] = true
	}
	for _, p := range post {
		if !preSet[p] {
			t.Errorf("NEW listener PID %s appeared across test:\npre:  %v\npost: %v\ninvariant violation — a code path under test spawned a real `claude remote-control` listener.",
				p, pre, post)
		}
	}
}

// snapshotListenerPIDs runs pgrep against the legacy fleet-coord
// listener argv pattern. Returns sorted PID strings; absent pgrep / no
// matches → empty slice (both non-failure paths — "no listeners" is
// the only expected state under the native model).
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
