package spawn

import (
	"strings"
	"testing"
	"time"
)

// PR-A fork-bomb fix: unit tests for the FLEET_STANDBY_TIMEOUT seam and the
// standby-launch counter. These are pure-logic (no spawn) and stay default-lane.

// Test #4 — production default is unchanged when the env seam is unset.
func TestStandbyTimeoutOrDefault_ProductionDefault(t *testing.T) {
	// Pin it empty here so a host-exported value can't perturb the assert.
	t.Setenv("FLEET_STANDBY_TIMEOUT", "")
	if got := standbyTimeoutOrDefault(0); got != DefaultStandbyTimeout {
		t.Fatalf("standbyTimeoutOrDefault(0) with seam unset = %v, want %v (production default)",
			got, DefaultStandbyTimeout)
	}
}

// Test #4b — a valid seam value wins when the caller passes the zero default.
func TestStandbyTimeoutOrDefault_SeamHonored(t *testing.T) {
	t.Setenv("FLEET_STANDBY_TIMEOUT", "3s")
	if got := standbyTimeoutOrDefault(0); got != 3*time.Second {
		t.Fatalf("standbyTimeoutOrDefault(0) with seam=3s = %v, want 3s", got)
	}
}

// Test #4c — the seam overrides even an explicit nonzero caller value (the
// runHandoff/runDispatch/Resume/GracefulHandoff path passes an explicit 10m).
func TestStandbyTimeoutOrDefault_SeamOverridesExplicit(t *testing.T) {
	t.Setenv("FLEET_STANDBY_TIMEOUT", "3s")
	if got := standbyTimeoutOrDefault(10 * time.Minute); got != 3*time.Second {
		t.Fatalf("standbyTimeoutOrDefault(10m) with seam=3s = %v, want 3s (env must win)", got)
	}
}

// Test — an invalid or non-positive seam value is ignored (falls through to the
// existing d<=0 -> DefaultStandbyTimeout logic), never panics.
func TestStandbyTimeoutOrDefault_InvalidSeamIgnored(t *testing.T) {
	for _, bad := range []string{"garbage", "0s", "-5s", "12"} {
		t.Setenv("FLEET_STANDBY_TIMEOUT", bad)
		if got := standbyTimeoutOrDefault(0); got != DefaultStandbyTimeout {
			t.Fatalf("standbyTimeoutOrDefault(0) with invalid seam %q = %v, want default %v",
				bad, got, DefaultStandbyTimeout)
		}
		// A nonzero explicit caller value survives an invalid seam.
		if got := standbyTimeoutOrDefault(2 * time.Minute); got != 2*time.Minute {
			t.Fatalf("standbyTimeoutOrDefault(2m) with invalid seam %q = %v, want 2m",
				bad, got)
		}
	}
}

// Test #1b (counter mechanics, NEGATIVE half) — the rollback case must NOT
// increment the standby-launch counter. We can't increment the global counter
// in a default-lane test (the non-integration TestMain asserts it stays zero,
// the whole point of the fork-bomb gate), so the POSITIVE half ("a real
// lease-wrapped spawn increments it by 1") lives in the integration lane
// (TestSpawn_PersistsLeaseWrappedState). Here we pin the negative: the
// default-lane rollback test (TestSpawn_LeaseCoord_RollsBackPrelaunchRecordOnTmuxFailure)
// forces tmux.Spawn to fail, and the package-wide gate passing at zero proves
// it did NOT increment. This test makes the contract explicit: a forced-fail
// lease-wrapped spawn leaves the counter unchanged.
func TestSpawn_RollbackDoesNotIncrementStandbyCounter(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)
	// counter bump (which is gated on tmux.Spawn success). Canonical fleet-test-
	// prefix so the runtime sink guard admits it.
	t.Setenv("FLEET_TMUX_SOCKET", "/tmp/fleet-test-"+strings.Repeat("a", 200)+".sock")

	before := StandbyLaunchCount()
	_, err := Spawn(Options{
		TaskID:         "coord-rbk", // isCoordSpawn -> lease-wrap attempted
		Project:        "rbk",
		Cwd:            t.TempDir(),
		Command:        []string{"sleep", "30"},
		PreAllocatedID: "rbkcnt01",
	})
	if err == nil {
		t.Fatal("expected Spawn to fail with an unusable tmux socket")
	}
	if got := StandbyLaunchCount() - before; got != 0 {
		t.Fatalf("rollback (forced tmux.Spawn fail) incremented the standby counter by %d, want 0", got)
	}
}
