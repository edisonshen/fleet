package handoffop

import (
	"errors"
	"testing"

	"github.com/edisonshen/fleet/internal/coordlock"
	"github.com/edisonshen/fleet/internal/testutil/tmuxtest"
)

// The P0 regression guards for the D3 Class-4 crash-recovery resume matrix
// (operator-approved 2026-07-07). These prevent reintroducing the duplicate-
// coord bug the marker deletion exists to fix: on a handoff/drain retry where
// the queued replacement NEW's session is dead, the classifier must decide
// respawn / refuse / wait purely from the LEASE state (+ a defensive OLD probe
// on the committed arm). Deterministic — all lease state is injected.

// leaseCell is one committed/reserved/free lease configuration.
type leaseCell struct {
	owner       string // active lease owner AgentID; "" == no active owner
	handoffSucc string // handoff reservation successor; "" == none
	oldAlive    bool   // defensive OLD-session probe result
	oldProbeErr error  // defensive OLD-session probe error
}

func (c leaseCell) deps() CoordSwapResumeDeps {
	return CoordSwapResumeDeps{
		CurrentOwner: func(string) (coordlock.Owner, bool) {
			if c.owner == "" {
				return coordlock.Owner{}, false
			}
			return coordlock.Owner{AgentID: c.owner}, true
		},
		CurrentHandoff: func(string) (coordlock.Handoff, bool) {
			if c.handoffSucc == "" {
				return coordlock.Handoff{}, false
			}
			return coordlock.Handoff{SuccessorID: c.handoffSucc}, true
		},
		OldSessionAlive: func(string) (bool, error) {
			return c.oldAlive, c.oldProbeErr
		},
	}
}

func TestClassifyCoordSwapResume_Matrix(t *testing.T) {
	// ClassifyCoordSwapResume never spawns tmux — every dep is injected —
	// but the isolation lint (scripts/lint-test-isolation.sh) matches the
	// `Resume(` trigger substring inside the call and requires the canonical
	// FLEET_TMUX_SOCKET guard regardless. Cheap belt-and-suspenders.
	tmuxtest.RequireTmux(t)
	const (
		oldID   = "OLD"
		oldSess = "coord-OLD"
		newID   = "NEW"
	)
	probeErr := errors.New("tmux probe failed")

	tests := []struct {
		name         string
		cell         leaseCell
		force        bool
		wantAction   ResumeAction
		wantOldAlive bool
		wantProbeErr bool
	}{
		{
			// committed × NEW-dead: epoch names NEW, OLD dead (invariant) →
			// refuse (respawn would duplicate).
			name:       "committed/new-dead → refuse",
			cell:       leaseCell{owner: newID, oldAlive: false},
			wantAction: ResumeRefuse,
		},
		{
			// committed × --force-replacement: operator override → respawn.
			name:       "committed/force → respawn",
			cell:       leaseCell{owner: newID, oldAlive: false},
			force:      true,
			wantAction: ResumeRespawn,
		},
		{
			// committed × OLD-STILL-ALIVE: the defensive cross-socket branch.
			// Still refuse; OldStillAlive surfaced for a loud diagnostic. NEVER
			// kills.
			name:         "committed/old-alive (defensive) → refuse + surface",
			cell:         leaseCell{owner: newID, oldAlive: true},
			wantAction:   ResumeRefuse,
			wantOldAlive: true,
		},
		{
			// committed × OLD-probe-error: ambiguous → still refuse; probe err
			// surfaced.
			name:         "committed/old-probe-error → refuse + surface err",
			cell:         leaseCell{owner: newID, oldProbeErr: probeErr},
			wantAction:   ResumeRefuse,
			wantProbeErr: true,
		},
		{
			// reserved × no owner: handoff reservation names NEW, swap in flight
			// → wait (standby recovers), do NOT respawn a 2nd replacement.
			name:       "reserved/no-owner → wait",
			cell:       leaseCell{owner: "", handoffSucc: newID},
			wantAction: ResumeWait,
		},
		{
			// reserved for a DIFFERENT successor → not our swap → respawn.
			name:       "reserved for other successor → respawn",
			cell:       leaseCell{owner: "", handoffSucc: "SOMEONE-ELSE"},
			wantAction: ResumeRespawn,
		},
		{
			// OLD still owns the lease (retire not finished) → not committed →
			// respawn safely.
			name:       "old-owns → respawn",
			cell:       leaseCell{owner: oldID},
			wantAction: ResumeRespawn,
		},
		{
			// free lease + no handoff (pre-commit crash / clean) → respawn.
			name:       "free/no-handoff → respawn",
			cell:       leaseCell{owner: "", handoffSucc: ""},
			wantAction: ResumeRespawn,
		},
		{
			// committed to NEW even though a stale handoff reservation also
			// names NEW: the active-owner arm wins (committed), so refuse — the
			// handoff-wait arm must only fire when there is NO active owner.
			name:       "owner==NEW with stale handoff → refuse (owner wins)",
			cell:       leaseCell{owner: newID, handoffSucc: newID},
			wantAction: ResumeRefuse,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyCoordSwapResume("proj", oldID, oldSess, newID, tc.force, tc.cell.deps())
			if got.Action != tc.wantAction {
				t.Fatalf("Action = %v, want %v (reason=%q)", got.Action, tc.wantAction, got.Reason)
			}
			if got.OldStillAlive != tc.wantOldAlive {
				t.Fatalf("OldStillAlive = %v, want %v", got.OldStillAlive, tc.wantOldAlive)
			}
			if (got.OldProbeErr != nil) != tc.wantProbeErr {
				t.Fatalf("OldProbeErr = %v, want-non-nil=%v", got.OldProbeErr, tc.wantProbeErr)
			}
		})
	}
}

// The committed arm must not perform the defensive probe when --force is given
// (operator override short-circuits) — and respawn regardless of OLD liveness.
func TestClassifyCoordSwapResume_ForceSkipsProbe(t *testing.T) {
	tmuxtest.RequireTmux(t) // isolation lint: `Resume(` trigger; no real spawn.
	probed := false
	d := CoordSwapResumeDeps{
		CurrentOwner: func(string) (coordlock.Owner, bool) {
			return coordlock.Owner{AgentID: "NEW"}, true
		},
		CurrentHandoff: func(string) (coordlock.Handoff, bool) { return coordlock.Handoff{}, false },
		OldSessionAlive: func(string) (bool, error) {
			probed = true
			return true, nil
		},
	}
	got := ClassifyCoordSwapResume("proj", "OLD", "coord-OLD", "NEW", true, d)
	if got.Action != ResumeRespawn {
		t.Fatalf("force override: Action = %v, want respawn", got.Action)
	}
	if probed {
		t.Fatal("force override must short-circuit before the defensive OLD probe")
	}
}

// A nil OLD probe seam (or empty session) on the committed arm must still
// refuse — the probe is belt-and-suspenders, never load-bearing for the verdict.
func TestClassifyCoordSwapResume_NilProbeStillRefuses(t *testing.T) {
	tmuxtest.RequireTmux(t) // isolation lint: `Resume(` trigger; no real spawn.
	d := CoordSwapResumeDeps{
		CurrentOwner:    func(string) (coordlock.Owner, bool) { return coordlock.Owner{AgentID: "NEW"}, true },
		CurrentHandoff:  func(string) (coordlock.Handoff, bool) { return coordlock.Handoff{}, false },
		OldSessionAlive: nil,
	}
	got := ClassifyCoordSwapResume("proj", "OLD", "", "NEW", false, d)
	if got.Action != ResumeRefuse {
		t.Fatalf("nil-probe committed arm: Action = %v, want refuse", got.Action)
	}
	if got.OldStillAlive {
		t.Fatal("nil-probe must not report OldStillAlive")
	}
}
