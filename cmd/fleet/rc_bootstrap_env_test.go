package main

import (
	"os"
	"strings"
	"testing"

	"github.com/edisonshen/fleet/internal/spawn"
)

// enableRCBootstrapForTest clears the FLEET_RC_BOOTSTRAP_DISABLED env
// var for the duration of the calling test, restoring it via t.Cleanup.
// Used by the rewrite-mechanism tests that pre-date this PR — they
// assert the cmd/fleet wrapper actually injects `--remote-control` into
// the spawned argv, which is the exact behaviour the package-shared
// TestMain disables for the rest of the cmd/fleet test binary
// (rc-listener-bootstrap-sk-3e98). The two regression tests in this
// file pin the contract that toggling this env produces the expected
// behaviour on both branches.
func enableRCBootstrapForTest(t *testing.T) {
	t.Helper()
	t.Setenv("FLEET_RC_BOOTSTRAP_DISABLED", "")
}

// TestRCBootstrap_Disabled_OmitsListenerSpawn pins the regression for
// rc-listener-bootstrap-sk-3e98: when FLEET_RC_BOOTSTRAP_DISABLED=1 is
// set in the process env, the cmd/fleet wrapper `spawn.CoordRCInjector`
// MUST return the input command unchanged — no `--remote-control` flag
// injected. The wrapper is the single gate point for the three cmd/fleet
// call sites (dispatch coord-spawn, dispatch recovery, handoff
// replacement). With injection skipped, tmux-spawned wrappers do not
// drag the real `claude` binary into a remote-control attach, which had
// been pinging the operator's mobile on every test run.
//
// Inputs use the default shell-wrapped claude shape so the relaxed
// matcher in spawn.InjectRemoteControlFlag would normally fire and
// rewrite the body. The contract under test is the env-gate, not the
// rewrite itself (covered by internal/spawn/argv_test.go).
//
// Acceptance:
//   - env unset / empty → injection fires (production behaviour preserved).
//   - env set to any non-empty value → injection skipped, returns the
//     input slice byte-identical.
//
// Companion: the package-local TestMain in main_test.go pre-sets the env
// for all cmd/fleet tests so handoff_test / dispatch_test runs do not
// trigger real listener spawns. This test temporarily unsets the env to
// exercise the enabled branch as well.
func TestRCBootstrap_Disabled_OmitsListenerSpawn(t *testing.T) {
	// TestMain sets FLEET_RC_BOOTSTRAP_DISABLED=1 for the whole cmd/fleet
	// test binary. We do NOT clear it here — the contract is exactly that
	// with this env set, injection is skipped.
	if os.Getenv("FLEET_RC_BOOTSTRAP_DISABLED") == "" {
		t.Fatal("FLEET_RC_BOOTSTRAP_DISABLED must be set by TestMain; test setup invariant violated")
	}

	input := []string{"sh", "-c", "claude --dangerously-skip-permissions; exit"}
	got := spawn.CoordRCInjector("rainier", "deadbeef", input)

	// Must return the slice unchanged. We assert (a) length match and
	// (b) element-by-element equality. We also check the body does NOT
	// contain `--remote-control` (the substring the relaxed matcher
	// would have inserted).
	if len(got) != len(input) {
		t.Fatalf("spawn.CoordRCInjector with env-disabled returned len=%d, want %d (slice should be unchanged): got=%v",
			len(got), len(input), got)
	}
	for i := range got {
		if got[i] != input[i] {
			t.Errorf("spawn.CoordRCInjector[%d] = %q, want %q (env-disabled must return slice unchanged)",
				i, got[i], input[i])
		}
	}
	if strings.Contains(got[2], "--remote-control") {
		t.Errorf("env-disabled wrapper body still contains `--remote-control`: %q", got[2])
	}
}

// TestRCBootstrap_Enabled_EmitsListenerSpawn is the positive branch:
// with the env var cleared, `spawn.CoordRCInjector` rewrites the
// wrapper body to inject `--remote-control "<sessionName>"` (production
// behaviour). This pins that the env-gate is opt-in and does NOT
// regress the default rewrite behaviour relied on by coord and handoff
// spawn paths.
func TestRCBootstrap_Enabled_EmitsListenerSpawn(t *testing.T) {
	// Clear the env for the duration of this test only.
	enableRCBootstrapForTest(t)

	input := []string{"sh", "-c", "claude --dangerously-skip-permissions; exit"}
	sessionName := "fleet-coord-deadbeef-rainier"
	got := spawn.CoordRCInjector("rainier", "deadbeef", input)

	if len(got) != 3 {
		t.Fatalf("spawn.CoordRCInjector returned len=%d, want 3: got=%v", len(got), got)
	}
	if got[0] != "sh" || got[1] != "-c" {
		t.Errorf("argv[0:2] mutated: got=%v, want sh -c prefix", got[:2])
	}
	wantSubstr := `--remote-control "` + sessionName + `"`
	if !strings.Contains(got[2], wantSubstr) {
		t.Errorf("env-enabled wrapper body missing %q: %q", wantSubstr, got[2])
	}
}
