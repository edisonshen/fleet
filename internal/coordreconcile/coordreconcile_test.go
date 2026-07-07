package coordreconcile

import (
	"errors"
	"testing"

	"github.com/edisonshen/fleet/internal/coordlock"
)

// leaseState is a shared, deterministic builder for a project's lease
// observations. A test sets exactly the fields its scenario needs; the
// zero value is a FREE lease (no owner, no starting record, no handoff).
// It records the CAS side effects (Supersede / ClaimStarting) so a case can
// assert both the returned Decision AND that no forbidden mutation ran
// (e.g. T7b must never claim over a live leader).
type leaseState struct {
	// liveOwner, when set, is the process-live active owner (LiveOwner ok).
	liveOwner *coordlock.Owner
	// starting, when set, is the `starting` record status (CurrentStarting ok).
	starting *coordlock.StartingStatus
	// handoff, when set, is a valid successor reservation (CurrentHandoff ok).
	handoff *coordlock.Handoff

	// supersedeOK / supersedeErr control the Supersede CAS result.
	supersedeOK  bool
	supersedeErr error
	// claimOK / claimErr control the ClaimStarting CAS result.
	claimOK  bool
	claimErr error

	// Recorded calls (asserted by tests).
	supersedeCalls int
	supersedeEpoch int64
	claimCalls     int
	claimedID      string
}

func (s *leaseState) deps() Deps {
	return Deps{
		LiveOwner: func(string) (coordlock.Owner, bool) {
			if s.liveOwner == nil {
				return coordlock.Owner{}, false
			}
			return *s.liveOwner, true
		},
		CurrentStarting: func(string) (coordlock.StartingStatus, bool) {
			if s.starting == nil {
				return coordlock.StartingStatus{}, false
			}
			return *s.starting, true
		},
		CurrentHandoff: func(string) (coordlock.Handoff, bool) {
			if s.handoff == nil {
				return coordlock.Handoff{}, false
			}
			return *s.handoff, true
		},
		Supersede: func(_ string, epoch int64) (bool, error) {
			s.supersedeCalls++
			s.supersedeEpoch = epoch
			return s.supersedeOK, s.supersedeErr
		},
		ClaimStarting: func(_ string, id string) (bool, error) {
			s.claimCalls++
			s.claimedID = id
			return s.claimOK, s.claimErr
		},
	}
}

func TestResolveMatrix(t *testing.T) {
	const project = "projects-fleet"
	const agentID = "newcoord01"

	cases := []struct {
		name      string
		agentID   string
		state     leaseState
		wantDec   Decision
		wantOwner string // AgentID expected on Attach; "" otherwise
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
			state:     leaseState{liveOwner: &coordlock.Owner{AgentID: "live01", PID: 4242}},
			wantDec:   Attach,
			wantOwner: "live01",
		},
		{
			// T7b: owner ALIVE but wedged (stopped renewing, past TTL). LiveOwner
			// is TTL-independent, so it STILL reports the live owner -> ATTACH,
			// never claim/spawn beside a live coord (no-auto-kill). Assert NO
			// ClaimStarting ran.
			name:      "T7b_live_wedged_owner_never_clobbered",
			agentID:   agentID,
			state:     leaseState{liveOwner: &coordlock.Owner{AgentID: "wedged01", PID: 777}},
			wantDec:   Attach,
			wantOwner: "wedged01",
			wantClaim: 0,
		},
		{
			// T2: a second [a] while the first coord is `starting` within TTL —
			// WAIT (no duplicate spawn), regardless of OwnerLive.
			name:    "T2_starting_within_ttl_waits",
			agentID: agentID,
			state: leaseState{starting: &coordlock.StartingStatus{
				Owner: coordlock.Owner{AgentID: "boot01", PID: 555}, OwnerLive: true, WithinTTL: true, Epoch: 5,
			}},
			wantDec:       Wait,
			wantSupersede: 0,
			wantClaim:     0,
		},
		{
			// T2b: starting within TTL but owner pid NOT yet live (pre-spawn
			// claim window) — still WAIT.
			name:    "T2b_starting_within_ttl_owner_not_yet_live_waits",
			agentID: agentID,
			state: leaseState{starting: &coordlock.StartingStatus{
				OwnerLive: false, WithinTTL: true, Epoch: 5,
			}},
			wantDec: Wait,
		},
		{
			// T6s: starting wedged past TTL, owner LIVE -> supersede (record CAS)
			// + SPAWN-STANDBY. Assert Supersede ran against the observed epoch
			// and NO fresh Spawn/claim.
			name:    "T6s_starting_wedged_live_supersede_and_standby",
			agentID: agentID,
			state: leaseState{
				starting: &coordlock.StartingStatus{
					Owner: coordlock.Owner{AgentID: "zombie01", PID: 999}, OwnerLive: true, WithinTTL: false, Epoch: 7,
				},
				supersedeOK: true,
			},
			wantDec:       SpawnStandby,
			wantSupersede: 1,
			wantSupEpoch:  7,
			wantClaim:     0,
		},
		{
			// T6s race: supersede loses (record flipped/advanced) -> WAIT.
			name:    "T6s_supersede_race_waits",
			agentID: agentID,
			state: leaseState{
				starting: &coordlock.StartingStatus{
					Owner: coordlock.Owner{AgentID: "zombie01", PID: 999}, OwnerLive: true, WithinTTL: false, Epoch: 7,
				},
				supersedeOK: false,
			},
			wantDec:       Wait,
			wantSupersede: 1,
			wantSupEpoch:  7,
		},
		{
			// T6s supersede error surfaces (never silently reconcile).
			name:    "T6s_supersede_error_surfaces",
			agentID: agentID,
			state: leaseState{
				starting: &coordlock.StartingStatus{
					Owner: coordlock.Owner{AgentID: "zombie01", PID: 999}, OwnerLive: true, WithinTTL: false, Epoch: 7,
				},
				supersedeErr: errors.New("epoch.lock busy"),
			},
			wantErr:       true,
			wantSupersede: 1,
		},
		{
			// wedged starting with no agentID -> superseded but caller can't
			// spawn a standby -> WAIT (still superseded, so the zombie is fenced).
			name:    "T6s_wedged_no_agentid_waits_after_supersede",
			agentID: "",
			state: leaseState{
				starting: &coordlock.StartingStatus{
					Owner: coordlock.Owner{AgentID: "zombie01", PID: 999}, OwnerLive: true, WithinTTL: false, Epoch: 7,
				},
				supersedeOK: true,
			},
			wantDec:       Wait,
			wantSupersede: 1,
		},
		{
			// starting past TTL with a DEAD owner -> NOT superseded (no live
			// process to fence at the record CAS level); falls through to the
			// free/dead claim branch and SPAWNS.
			name:    "starting_wedged_dead_owner_falls_through_to_spawn",
			agentID: agentID,
			state: leaseState{
				starting: &coordlock.StartingStatus{
					Owner: coordlock.Owner{AgentID: "deadboot01", PID: 111}, OwnerLive: false, WithinTTL: false, Epoch: 7,
				},
				claimOK: true,
			},
			wantDec:       Spawn,
			wantSupersede: 0,
			wantClaim:     1,
			wantClaimID:   agentID,
		},
		{
			// codex D4 iter-1 [P1] regression: a pre-activation crash — the
			// owner pid IS stamped (post-flock-acquire) but confirmed DEAD —
			// must fall through to spawn IMMEDIATELY even while still
			// WithinTTL. No-auto-kill protects LIVE processes only; a
			// confirmed-dead owner must not stall attach/spawn recovery for
			// the rest of startingTTL (~120s).
			name:    "starting_within_ttl_pid_stamped_dead_falls_through_immediately",
			agentID: agentID,
			state: leaseState{
				starting: &coordlock.StartingStatus{
					Owner: coordlock.Owner{AgentID: "crashedboot01", PID: 222}, OwnerLive: false, WithinTTL: true, Epoch: 9,
				},
				claimOK: true,
			},
			wantDec:       Spawn,
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
			state: leaseState{
				starting: &coordlock.StartingStatus{
					Owner: coordlock.Owner{AgentID: "crashboot01", PID: 424242}, OwnerLive: false, WithinTTL: true, Epoch: 9,
				},
				claimOK: true,
			},
			wantDec:       Spawn,
			wantSupersede: 0,
			wantClaim:     1,
			wantClaimID:   agentID,
		},
		{
			// Handoff reservation naming a successor -> WAIT for it (#247 rule a).
			name:      "handoff_reservation_waits",
			agentID:   agentID,
			state:     leaseState{handoff: &coordlock.Handoff{SuccessorID: "succ01"}},
			wantDec:   Wait,
			wantClaim: 0,
		},
		{
			// T7a: free / dead-active lease -> CLAIM + SPAWN. (A dead active
			// owner is NOT LiveOwner, so we reach here and the claim is safe.)
			name:        "T7a_free_lease_claims_and_spawns",
			agentID:     agentID,
			state:       leaseState{claimOK: true},
			wantDec:     Spawn,
			wantClaim:   1,
			wantClaimID: agentID,
		},
		{
			// T8: two [a] racing on a free lease — the loser's ClaimStarting CAS
			// returns ok=false -> WAIT (re-resolve), so exactly one spawns.
			name:      "T8_claim_race_loser_waits",
			agentID:   agentID,
			state:     leaseState{claimOK: false},
			wantDec:   Wait,
			wantClaim: 1,
		},
		{
			// claim error surfaces.
			name:    "claim_error_surfaces",
			agentID: agentID,
			state:   leaseState{claimErr: errors.New("epoch.lock busy")},
			wantErr: true,
		},
		{
			// free lease but no agentID -> WAIT (cannot spawn without an id);
			// never claims.
			name:      "free_no_agentid_waits",
			agentID:   "",
			state:     leaseState{},
			wantDec:   Wait,
			wantClaim: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := tc.state // copy so recorded-call counters are per-case
			v, err := Resolve(st.deps(), project, tc.agentID)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got nil (verdict=%+v)", v)
				}
				if st.supersedeCalls != tc.wantSupersede {
					t.Errorf("supersede calls = %d, want %d", st.supersedeCalls, tc.wantSupersede)
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
			if st.supersedeCalls != tc.wantSupersede {
				t.Errorf("supersede calls = %d, want %d", st.supersedeCalls, tc.wantSupersede)
			}
			if tc.wantSupEpoch != 0 && st.supersedeEpoch != tc.wantSupEpoch {
				t.Errorf("supersede epoch = %d, want %d", st.supersedeEpoch, tc.wantSupEpoch)
			}
			if st.claimCalls != tc.wantClaim {
				t.Errorf("claim calls = %d, want %d", st.claimCalls, tc.wantClaim)
			}
			if tc.wantClaimID != "" && st.claimedID != tc.wantClaimID {
				t.Errorf("claimed id = %q, want %q", st.claimedID, tc.wantClaimID)
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
	st := leaseState{
		liveOwner: &live,
		starting:  &coordlock.StartingStatus{OwnerLive: true, WithinTTL: false, Epoch: 3},
		handoff:   &coordlock.Handoff{SuccessorID: "succ01"},
		claimOK:   true,
	}
	v, err := Resolve(st.deps(), "projects-fleet", "newcoord01")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Decision != Attach || v.Owner.AgentID != "live01" {
		t.Fatalf("live owner must win: got %s owner=%q", v.Decision, v.Owner.AgentID)
	}
	if st.supersedeCalls != 0 || st.claimCalls != 0 {
		t.Fatalf("no CAS may run when a live owner is present: supersede=%d claim=%d", st.supersedeCalls, st.claimCalls)
	}
}
