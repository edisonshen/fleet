package tui

// Regression guards for coordSpawnLeaseIdentity (rows.go) — the lease-based
// coord-identity read that REPLACED the deleted coord-spawn marker as the
// dashboard's + [a]-dedup's identity signal (D3).
//
// The starting-owner arm mirrors coordreconcile.Resolve's exact gate
// (WithinTTL AND NOT confirmed-dead, where confirmed-dead needs a RECORDED
// pid). The dead-pid decision itself is exhaustively covered by
// coordreconcile_test.go; here we pin the two properties unique to this
// wrapper that a naive `&& OwnerLive` gate would have broken:
//   1. a just-claimed starter (pid unknown pre-spawn, PID==0) MUST still be
//      returned as the identity — otherwise the boot-window [a]-dedup breaks
//      and a second [a] spawns a duplicate coord.
//   2. a free lease (no owner, no starting record) returns "".

import (
	"testing"

	"github.com/edisonshen/fleet/internal/coordlock"
	"github.com/edisonshen/fleet/internal/state"
)

func TestCoordSpawnLeaseIdentity_StartingBootWindow_KeepsIdentity(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())
	const project = "bootproj"
	if _, err := state.EnsureProjectInitialized(project); err != nil {
		t.Fatalf("EnsureProjectInitialized: %v", err)
	}
	// ClaimStartingRecord writes a `starting` record with the owner pid UNKNOWN
	// (PID==0) — the pre-spawn boot window. WithinTTL is true, and because the
	// pid is not recorded the starter is NOT "confirmed dead".
	ok, err := coordlock.ClaimStartingRecord(project, "coord-booting")
	if err != nil {
		t.Fatalf("ClaimStartingRecord: %v", err)
	}
	if !ok {
		t.Fatalf("ClaimStartingRecord: ok=false on a free lease; cannot set up the boot window")
	}
	// Sanity: this is exactly the PID==0 / not-confirmed-dead shape.
	st, sOK := coordlock.CurrentStarting(project)
	if !sOK || !st.WithinTTL {
		t.Fatalf("CurrentStarting: ok=%v withinTTL=%v; expected an in-TTL starting record", sOK, st.WithinTTL)
	}
	if st.Owner.PID > 0 {
		t.Fatalf("expected an unknown-pid (PID==0) boot-window record; got PID=%d", st.Owner.PID)
	}

	if got := coordSpawnLeaseIdentity(project); got != "coord-booting" {
		t.Errorf("coordSpawnLeaseIdentity = %q; want coord-booting — a just-claimed "+
			"starter (pid unknown) must stay the identity so the boot-window [a]-dedup holds", got)
	}
}

func TestCoordSpawnLeaseIdentity_FreeLease_Empty(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())
	const project = "freeproj"
	if _, err := state.EnsureProjectInitialized(project); err != nil {
		t.Fatalf("EnsureProjectInitialized: %v", err)
	}
	// No owner, no starting record -> the lease names nobody -> "".
	if got := coordSpawnLeaseIdentity(project); got != "" {
		t.Errorf("coordSpawnLeaseIdentity on a free lease = %q; want empty", got)
	}
}
