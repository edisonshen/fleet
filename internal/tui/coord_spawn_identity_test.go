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

// resolveCoordSpawnIdentity is the pure branch-ordering + liveness decision.
// Table-drives every cell (mirrors coordreconcile.Resolve's starting gate + the
// coord-owner CLI / loop.py gate-5 handoff-successor identity) without a real
// lease.
func TestResolveCoordSpawnIdentity(t *testing.T) {
	owner := func(id string) (coordlock.Owner, bool) { return coordlock.Owner{AgentID: id}, id != "" }
	// starting builds a StartingStatus; pid==0 models the just-claimed
	// (unknown-pid) boot window, pid>0 + !live models a crashed starter.
	starting := func(id string, pid int, live, withinTTL bool) (coordlock.StartingStatus, bool) {
		if id == "" {
			return coordlock.StartingStatus{}, false
		}
		return coordlock.StartingStatus{
			Owner:     coordlock.Owner{AgentID: id, PID: pid},
			OwnerLive: live,
			WithinTTL: withinTTL,
		}, true
	}
	handoff := func(id string) (coordlock.Handoff, bool) { return coordlock.Handoff{SuccessorID: id}, id != "" }

	tests := []struct {
		name string
		o    coordlock.Owner
		oOK  bool
		st   coordlock.StartingStatus
		stOK bool
		h    coordlock.Handoff
		hOK  bool
		want string
	}{
		{name: "live owner wins over all"},                // filled below
		{name: "booting starter (pid==0)"},                //
		{name: "confirmed-dead starter drops"},            //
		{name: "expired starter drops"},                   //
		{name: "handoff successor when no owner/starter"}, //
		{name: "owner beats handoff"},                     //
		{name: "starter beats handoff"},                   //
		{name: "nothing -> empty"},                        //
	}
	// Populate (kept out of the literal for readability of the builders).
	o1, ook1 := owner("coord-A")
	st1, stok1 := starting("start-B", 0, false, true)
	hh1, hok1 := handoff("succ-C")
	tests[0].o, tests[0].oOK, tests[0].st, tests[0].stOK, tests[0].h, tests[0].hOK, tests[0].want = o1, ook1, st1, stok1, hh1, hok1, "coord-A"

	st2, stok2 := starting("start-B", 0, false, true) // pid unknown -> not confirmed dead
	tests[1].st, tests[1].stOK, tests[1].want = st2, stok2, "start-B"

	st3, stok3 := starting("start-B", 4242, false, true) // pid recorded + dead -> confirmed dead
	hh3, hok3 := handoff("succ-C")
	tests[2].st, tests[2].stOK, tests[2].h, tests[2].hOK, tests[2].want = st3, stok3, hh3, hok3, "succ-C" // falls through to handoff

	st4, stok4 := starting("start-B", 0, false, false)         // past TTL
	tests[3].st, tests[3].stOK, tests[3].want = st4, stok4, "" // no handoff -> empty

	hh5, hok5 := handoff("succ-C")
	tests[4].h, tests[4].hOK, tests[4].want = hh5, hok5, "succ-C"

	o6, ook6 := owner("coord-A")
	hh6, hok6 := handoff("succ-C")
	tests[5].o, tests[5].oOK, tests[5].h, tests[5].hOK, tests[5].want = o6, ook6, hh6, hok6, "coord-A"

	st7, stok7 := starting("start-B", 7373, true, true) // live starter
	hh7, hok7 := handoff("succ-C")
	tests[6].st, tests[6].stOK, tests[6].h, tests[6].hOK, tests[6].want = st7, stok7, hh7, hok7, "start-B"

	// tests[7]: all zero -> "".

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveCoordSpawnIdentity(tc.o, tc.oOK, tc.st, tc.stOK, tc.h, tc.hOK)
			if got != tc.want {
				t.Errorf("resolveCoordSpawnIdentity = %q; want %q", got, tc.want)
			}
		})
	}
}
