package coordreconcile_test

import (
	"errors"
	"testing"

	"github.com/edisonshen/fleet/internal/coordlock"
	"github.com/edisonshen/fleet/internal/coordreconcile"
	"github.com/edisonshen/fleet/internal/coordreconcile/reconciletest"
	"github.com/edisonshen/fleet/internal/state"
)

// setupReconcileHome points FLEET_HOME at a temp dir + bootstraps it so the real
// coordlock lease primitives (AcquireLease / the flock readers) resolve a
// per-project .locks dir. Used by the integration test below.
func setupReconcileHome(t *testing.T) {
	t.Helper()
	t.Setenv("FLEET_HOME", t.TempDir())
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
}

func TestResolveMatrix(t *testing.T) {
	const project = "projects-fleet"
	const agentID = "newcoord01"

	cases := []struct {
		name      string
		agentID   string
		state     reconciletest.State
		wantDec   coordreconcile.Decision
		wantOwner string // AgentID expected on coordreconcile.Attach; "" otherwise
		wantClear int    // expected ClearHandoff invocations (clear-before-spawn)
		wantErr   bool
	}{
		{
			// T3: a live flock holder WITH an identity ⇒ Attach, never spawn a
			// duplicate (the incident regression). A busy flock is a live coord.
			name:      "T3_live_flock_holder_attaches",
			agentID:   agentID,
			state:     reconciletest.State{LiveOwner: &coordlock.Owner{AgentID: "live01", PID: 4242}},
			wantDec:   coordreconcile.Attach,
			wantOwner: "live01",
		},
		{
			// T13 (D9e): the flock is HELD by a live coord but its body is
			// identity-less (old-binary / torn ⇒ AgentID==""). Resolve WAITs
			// (poll for the identity); attach's PROJECT-RECOVERY falls back to
			// FindLiveCoord. Never Attach-with-empty-id, never spawn beside it.
			name:    "T13_flock_held_identity_less_waits",
			agentID: agentID,
			state:   reconciletest.State{LiveOwner: &coordlock.Owner{AgentID: "", PID: 999}},
			wantDec: coordreconcile.Wait,
		},
		{
			// T4: flock FREE + journal names a successor whose process is ALIVE
			// (handoff in flight) ⇒ WAIT, do not spawn beside the booting
			// successor. No clear.
			name:      "T4_handoff_in_flight_waits",
			agentID:   agentID,
			state:     reconciletest.State{HandoffDisp: coordlock.HandoffInFlight},
			wantDec:   coordreconcile.Wait,
			wantClear: 0,
		},
		{
			// T5: flock FREE + journal names a successor whose process is DEAD
			// (abandoned) ⇒ clear the stale journal FIRST (clear-before-spawn),
			// THEN Spawn.
			name:      "T5_handoff_abandoned_clears_then_spawns",
			agentID:   agentID,
			state:     reconciletest.State{HandoffDisp: coordlock.HandoffAbandoned},
			wantDec:   coordreconcile.Spawn,
			wantClear: 1,
		},
		{
			// T6: flock FREE + no journal ⇒ Spawn a fresh coord. No clear.
			name:      "T6_free_no_journal_spawns",
			agentID:   agentID,
			state:     reconciletest.State{HandoffDisp: coordlock.HandoffNone},
			wantDec:   coordreconcile.Spawn,
			wantClear: 0,
		},
		{
			// Torn/unreadable journal ⇒ ambiguous, a handoff may be in flight ⇒
			// WAIT (never spawn beside a possibly-booting successor). No clear.
			name:      "torn_journal_waits",
			agentID:   agentID,
			state:     reconciletest.State{ClassifyErr: errors.New("torn journal")},
			wantDec:   coordreconcile.Wait,
			wantClear: 0,
		},
		{
			// clear-before-spawn FAILS on an abandoned journal ⇒ error surfaces
			// (never silently spawn beside a stale journal that wedges O_EXCL).
			name:    "abandoned_clear_error_surfaces",
			agentID: agentID,
			state:   reconciletest.State{HandoffDisp: coordlock.HandoffAbandoned, ClearErr: errors.New("clear failed")},
			wantErr: true,
		},
		{
			// free flock, no journal, but no agentID ⇒ WAIT (cannot spawn
			// without an id).
			name:    "free_no_agentid_waits",
			agentID: "",
			state:   reconciletest.State{HandoffDisp: coordlock.HandoffNone},
			wantDec: coordreconcile.Wait,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := tc.state // copy so ClearCalls is per-case
			v, err := coordreconcile.Resolve(st.Deps(), project, tc.agentID)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got nil (verdict=%+v)", v)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if v.Decision != tc.wantDec {
				t.Errorf("Decision = %s, want %s (reason=%q)", v.Decision, tc.wantDec, v.Reason)
			}
			if v.Owner.AgentID != tc.wantOwner {
				t.Errorf("Owner.AgentID = %q, want %q", v.Owner.AgentID, tc.wantOwner)
			}
			if st.ClearCalls != tc.wantClear {
				t.Errorf("ClearHandoff calls = %d, want %d", st.ClearCalls, tc.wantClear)
			}
			if v.Reason == "" {
				t.Errorf("Verdict.Reason is empty; every decision must surface a reason")
			}
		})
	}
}

// TestResolveOrdering pins the precedence: a live flock holder wins even if a
// journal also exists (the flock is the sole source of truth — a live holder is
// never spawned beside, and the journal is only consulted when the flock is
// FREE).
func TestResolveOrdering(t *testing.T) {
	live := coordlock.Owner{AgentID: "live01", PID: 4242}
	st := reconciletest.State{
		LiveOwner:   &live,
		HandoffDisp: coordlock.HandoffAbandoned, // would clear+spawn IF the flock were free
	}
	v, err := coordreconcile.Resolve(st.Deps(), "projects-fleet", "newcoord01")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Decision != coordreconcile.Attach || v.Owner.AgentID != "live01" {
		t.Fatalf("live flock holder must win: got %s owner=%q", v.Decision, v.Owner.AgentID)
	}
	if st.ClearCalls != 0 {
		t.Fatalf("no journal clear may run when a live holder is present: clear=%d", st.ClearCalls)
	}
}

// TestResolve_RealFlockReaders is the flock-only INTEGRATION test: it drives
// coordreconcile.Resolve with the REAL coordlock readers (DefaultDeps) against a
// REAL held flock (via coordlock.AcquireLease), crossing the
// coordlock→coordreconcile boundary the unit table fakes. Covers T1 (busy coord
// with identity ⇒ Attach; freed ⇒ Spawn) and T13 (identity-less body ⇒ Wait).
func TestResolve_RealFlockReaders(t *testing.T) {
	t.Run("T1_busy_coord_with_identity_attaches_then_freed_spawns", func(t *testing.T) {
		setupReconcileHome(t)
		const project = "resolve-integ-attach"
		lease, acquired, err := coordlock.AcquireLease(project, "holder01")
		if err != nil || !acquired {
			t.Fatalf("AcquireLease: acquired=%v err=%v", acquired, err)
		}
		t.Cleanup(lease.Release)

		// Busy flock with a stamped identity ⇒ Attach — never spawn beside it
		// (the incident regression), even though the epoch is only `starting`.
		v, err := coordreconcile.Resolve(coordreconcile.DefaultDeps(), project, "newcaller")
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if v.Decision != coordreconcile.Attach || v.Owner.AgentID != "holder01" {
			t.Fatalf("busy coord ⇒ Attach holder01; got %s owner=%q", v.Decision, v.Owner.AgentID)
		}

		// Release frees the flock ⇒ a fresh Resolve now Spawns.
		lease.Release()
		v, err = coordreconcile.Resolve(coordreconcile.DefaultDeps(), project, "newcaller")
		if err != nil {
			t.Fatalf("Resolve after release: %v", err)
		}
		if v.Decision != coordreconcile.Spawn {
			t.Fatalf("freed lease ⇒ Spawn; got %s (reason=%q)", v.Decision, v.Reason)
		}
	})

	t.Run("T13_identity_less_flock_waits", func(t *testing.T) {
		setupReconcileHome(t)
		const project = "resolve-integ-wait"
		// An empty agentID stamps an identity-less body (the old-binary / torn
		// body case) — the flock is held but names no agent.
		lease, acquired, err := coordlock.AcquireLease(project, "")
		if err != nil || !acquired {
			t.Fatalf("AcquireLease: acquired=%v err=%v", acquired, err)
		}
		t.Cleanup(lease.Release)
		v, err := coordreconcile.Resolve(coordreconcile.DefaultDeps(), project, "newcaller")
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if v.Decision != coordreconcile.Wait {
			t.Fatalf("flock held with identity-less body ⇒ Wait; got %s owner=%q", v.Decision, v.Owner.AgentID)
		}
	})
}
