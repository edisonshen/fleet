package tui

// Regression guards for coordSpawnLeaseIdentity (rows.go) — the flock-only
// lease read that is the dashboard's + [a]-dedup's coord-identity signal.
//
// PR-2 (flock-only) deleted the `starting` reservation tier: acquiring the
// flock IS becoming the coordinator, so there is no pre-activation starter
// state. The identity now comes from exactly two sources — a process-live
// flock owner (coordlock.LiveOwner) or, mid-handoff, the journal-named
// in-flight successor whose process is alive (coordlock.CurrentHandoffSuccessor).

import (
	"testing"

	"github.com/edisonshen/fleet/internal/coordlock"
	"github.com/edisonshen/fleet/internal/state"
)

func TestCoordSpawnLeaseIdentity_FreeLease_Empty(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())
	const project = "freeproj"
	if _, err := state.EnsureProjectInitialized(project); err != nil {
		t.Fatalf("EnsureProjectInitialized: %v", err)
	}
	// No flock owner, no in-flight handoff -> the lease names nobody -> "".
	if got := coordSpawnLeaseIdentity(project); got != "" {
		t.Errorf("coordSpawnLeaseIdentity on a free lease = %q; want empty", got)
	}
}

// TestResolveCoordSpawnIdentity table-drives the pure branch-ordering decision
// (no I/O — the lease reads are threaded in). Two tiers only under flock-only:
// (1) a process-live flock owner with an AgentID wins; (2) else the in-flight
// handoff successor; (3) else "".
func TestResolveCoordSpawnIdentity(t *testing.T) {
	owner := func(id string) coordlock.Owner { return coordlock.Owner{AgentID: id} }

	tests := []struct {
		name        string
		owner       coordlock.Owner
		ownerOK     bool
		successorID string
		successorOK bool
		want        string
	}{
		{
			name:        "live owner wins over successor",
			owner:       owner("coord-A"),
			ownerOK:     true,
			successorID: "succ-C",
			successorOK: true,
			want:        "coord-A",
		},
		{
			name:        "owner ok but empty AgentID falls through to successor",
			owner:       owner(""),
			ownerOK:     true,
			successorID: "succ-C",
			successorOK: true,
			want:        "succ-C",
		},
		{
			name:        "handoff successor when no live owner",
			successorID: "succ-C",
			successorOK: true,
			want:        "succ-C",
		},
		{
			name:        "successor ok but empty id -> empty",
			successorID: "",
			successorOK: true,
			want:        "",
		},
		{
			name:    "owner ok empty id, no successor -> empty",
			owner:   owner(""),
			ownerOK: true,
			want:    "",
		},
		{
			name: "nothing -> empty",
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveCoordSpawnIdentity(tc.owner, tc.ownerOK, tc.successorID, tc.successorOK)
			if got != tc.want {
				t.Errorf("resolveCoordSpawnIdentity = %q; want %q", got, tc.want)
			}
		})
	}
}
