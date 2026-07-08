package coordreconcile_test

import (
	"errors"
	"testing"

	"github.com/edisonshen/fleet/internal/coordlock"
	"github.com/edisonshen/fleet/internal/coordreconcile"
	"github.com/edisonshen/fleet/internal/coordreconcile/reconciletest"
)

func TestResolveMatrix(t *testing.T) {
	const project = "projects-fleet"
	const agentID = "newcoord01"

	cases := []struct {
		name      string
		agentID   string
		state     reconciletest.State
		wantDec   coordreconcile.Decision
		wantOwner string // AgentID expected on coordreconcile.Attach; "" otherwise
		// assertions on recorded side effects
		wantSupersede int
		wantSupEpoch  int64
		wantClaim     int
		wantClaimID   string
		wantErr       bool
	}{
		{
			// T3: `[a]`/attach on a live active owner with NO marker file —
			// attaches, never spawns a duplicate (the incident regression).
			name:      "T3_live_active_owner_attaches",
			agentID:   agentID,
			state:     reconciletest.State{LiveOwner: &coordlock.Owner{AgentID: "live01", PID: 4242}},
			wantDec:   coordreconcile.Attach,
			wantOwner: "live01",
		},
		{
			// T7b: owner ALIVE but wedged (stopped renewing, past TTL). LiveOwner
			// is TTL-independent, so it STILL reports the live owner -> ATTACH,
			// never claim/spawn beside a live coord (no-auto-kill). Assert NO
			// ClaimStarting ran.
			name:      "T7b_live_wedged_owner_never_clobbered",
			agentID:   agentID,
			state:     reconciletest.State{LiveOwner: &coordlock.Owner{AgentID: "wedged01", PID: 777}},
			wantDec:   coordreconcile.Attach,
			wantOwner: "wedged01",
			wantClaim: 0,
		},
		{
			// T2: a second [a] while the first coord is `starting` within TTL —
			// WAIT (no duplicate spawn), regardless of OwnerLive.
			name:    "T2_starting_within_ttl_waits",
			agentID: agentID,
			state: reconciletest.State{Starting: &coordlock.StartingStatus{
				Owner: coordlock.Owner{AgentID: "boot01", PID: 555}, OwnerLive: true, WithinTTL: true, Epoch: 5,
			}},
			wantDec:       coordreconcile.Wait,
			wantSupersede: 0,
			wantClaim:     0,
		},
		{
			// T2b: starting within TTL but owner pid NOT yet live (pre-spawn
			// claim window) — still WAIT.
			name:    "T2b_starting_within_ttl_owner_not_yet_live_waits",
			agentID: agentID,
			state: reconciletest.State{Starting: &coordlock.StartingStatus{
				OwnerLive: false, WithinTTL: true, Epoch: 5,
			}},
			wantDec: coordreconcile.Wait,
		},
		{
			// T6s: starting wedged past TTL, owner LIVE -> supersede (record CAS)
			// + SPAWN-STANDBY. Assert Supersede ran against the observed epoch
			// and NO fresh coordreconcile.Spawn/claim.
			name:    "T6s_starting_wedged_live_supersede_and_standby",
			agentID: agentID,
			state: reconciletest.State{
				Starting: &coordlock.StartingStatus{
					Owner: coordlock.Owner{AgentID: "zombie01", PID: 999}, OwnerLive: true, WithinTTL: false, Epoch: 7,
				},
				SupersedeOK: true,
			},
			wantDec:       coordreconcile.SpawnStandby,
			wantSupersede: 1,
			wantSupEpoch:  7,
			wantClaim:     0,
		},
		{
			// T6s race: supersede loses (record flipped/advanced) -> WAIT.
			name:    "T6s_supersede_race_waits",
			agentID: agentID,
			state: reconciletest.State{
				Starting: &coordlock.StartingStatus{
					Owner: coordlock.Owner{AgentID: "zombie01", PID: 999}, OwnerLive: true, WithinTTL: false, Epoch: 7,
				},
				SupersedeOK: false,
			},
			wantDec:       coordreconcile.Wait,
			wantSupersede: 1,
			wantSupEpoch:  7,
		},
		{
			// T6s supersede error surfaces (never silently reconcile).
			name:    "T6s_supersede_error_surfaces",
			agentID: agentID,
			state: reconciletest.State{
				Starting: &coordlock.StartingStatus{
					Owner: coordlock.Owner{AgentID: "zombie01", PID: 999}, OwnerLive: true, WithinTTL: false, Epoch: 7,
				},
				SupersedeErr: errors.New("epoch.lock busy"),
			},
			wantErr:       true,
			wantSupersede: 1,
		},
		{
			// codex D4 iter-6 [P2]: wedged starting with no agentID -> a pure
			// READ must NOT supersede (it can't follow through with a
			// coordreconcile.SpawnStandby, so mutating here would leave the project with no
			// recovery path until a later caller arrives with a real id).
			// WAIT without touching the record.
			name:    "T6s_wedged_no_agentid_waits_without_superseding",
			agentID: "",
			state: reconciletest.State{
				Starting: &coordlock.StartingStatus{
					Owner: coordlock.Owner{AgentID: "zombie01", PID: 999}, OwnerLive: true, WithinTTL: false, Epoch: 7,
				},
				SupersedeOK: true,
			},
			wantDec:       coordreconcile.Wait,
			wantSupersede: 0,
		},
		{
			// starting past TTL with a DEAD owner -> NOT superseded (no live
			// process to fence at the record CAS level); falls through to the
			// free/dead claim branch and SPAWNS.
			name:    "starting_wedged_dead_owner_falls_through_to_spawn",
			agentID: agentID,
			state: reconciletest.State{
				Starting: &coordlock.StartingStatus{
					Owner: coordlock.Owner{AgentID: "deadboot01", PID: 111}, OwnerLive: false, WithinTTL: false, Epoch: 7,
				},
				ClaimOK: true,
			},
			wantDec:       coordreconcile.Spawn,
			wantSupersede: 0,
			wantClaim:     1,
			wantClaimID:   agentID,
		},
		{
			// codex D2 iter-3 [P1]: starting WITHIN TTL but owner pid was
			// stamped and is DEAD (pre-activation SIGKILL) -> do NOT wait out
			// the TTL; fall through to claim + SPAWN. Recovery is immediate,
			// not a ~120s outage.
			name:    "starting_within_ttl_dead_owner_recovers_now",
			agentID: agentID,
			state: reconciletest.State{
				Starting: &coordlock.StartingStatus{
					Owner: coordlock.Owner{AgentID: "crashboot01", PID: 424242}, OwnerLive: false, WithinTTL: true, Epoch: 9,
				},
				ClaimOK: true,
			},
			wantDec:       coordreconcile.Spawn,
			wantSupersede: 0,
			wantClaim:     1,
			wantClaimID:   agentID,
		},
		{
			// Handoff reservation naming a successor -> WAIT for it (#247 rule a).
			name:      "handoff_reservation_waits",
			agentID:   agentID,
			state:     reconciletest.State{Handoff: &coordlock.Handoff{SuccessorID: "succ01"}},
			wantDec:   coordreconcile.Wait,
			wantClaim: 0,
		},
		{
			// T7a: free / dead-active lease -> CLAIM + SPAWN. (A dead active
			// owner is NOT LiveOwner, so we reach here and the claim is safe.)
			name:        "T7a_free_lease_claims_and_spawns",
			agentID:     agentID,
			state:       reconciletest.State{ClaimOK: true},
			wantDec:     coordreconcile.Spawn,
			wantClaim:   1,
			wantClaimID: agentID,
		},
		{
			// T8: two [a] racing on a free lease — the loser's ClaimStarting CAS
			// returns ok=false -> WAIT (re-resolve), so exactly one spawns.
			name:      "T8_claim_race_loser_waits",
			agentID:   agentID,
			state:     reconciletest.State{ClaimOK: false},
			wantDec:   coordreconcile.Wait,
			wantClaim: 1,
		},
		{
			// claim error surfaces.
			name:    "claim_error_surfaces",
			agentID: agentID,
			state:   reconciletest.State{ClaimErr: errors.New("epoch.lock busy")},
			wantErr: true,
		},
		{
			// free lease but no agentID -> WAIT (cannot spawn without an id);
			// never claims.
			name:      "free_no_agentid_waits",
			agentID:   "",
			state:     reconciletest.State{},
			wantDec:   coordreconcile.Wait,
			wantClaim: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := tc.state // copy so recorded-call counters are per-case
			v, err := coordreconcile.Resolve(st.Deps(), project, tc.agentID)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got nil (verdict=%+v)", v)
				}
				if st.SupersedeCalls != tc.wantSupersede {
					t.Errorf("supersede calls = %d, want %d", st.SupersedeCalls, tc.wantSupersede)
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
			if st.SupersedeCalls != tc.wantSupersede {
				t.Errorf("supersede calls = %d, want %d", st.SupersedeCalls, tc.wantSupersede)
			}
			if tc.wantSupEpoch != 0 && st.SupersedeEpoch != tc.wantSupEpoch {
				t.Errorf("supersede epoch = %d, want %d", st.SupersedeEpoch, tc.wantSupEpoch)
			}
			if st.ClaimCalls != tc.wantClaim {
				t.Errorf("claim calls = %d, want %d", st.ClaimCalls, tc.wantClaim)
			}
			if tc.wantClaimID != "" && st.ClaimedID != tc.wantClaimID {
				t.Errorf("claimed id = %q, want %q", st.ClaimedID, tc.wantClaimID)
			}
			if v.Reason == "" {
				t.Errorf("Verdict.Reason is empty; every decision must surface a reason")
			}
		})
	}
}

// TestResolveOrdering pins the precedence: a live owner wins even if a stale
// starting/handoff record also exists on disk (the incident's stale-cache
// class — the live lease always beats a leftover record).
func TestResolveOrdering(t *testing.T) {
	live := coordlock.Owner{AgentID: "live01", PID: 4242}
	st := reconciletest.State{
		LiveOwner: &live,
		Starting:  &coordlock.StartingStatus{OwnerLive: true, WithinTTL: false, Epoch: 3},
		Handoff:   &coordlock.Handoff{SuccessorID: "succ01"},
		ClaimOK:   true,
	}
	v, err := coordreconcile.Resolve(st.Deps(), "projects-fleet", "newcoord01")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Decision != coordreconcile.Attach || v.Owner.AgentID != "live01" {
		t.Fatalf("live owner must win: got %s owner=%q", v.Decision, v.Owner.AgentID)
	}
	if st.SupersedeCalls != 0 || st.ClaimCalls != 0 {
		t.Fatalf("no CAS may run when a live owner is present: supersede=%d claim=%d", st.SupersedeCalls, st.ClaimCalls)
	}
}
