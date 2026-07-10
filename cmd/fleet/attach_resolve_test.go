package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/coordlock"
	"github.com/edisonshen/fleet/internal/coordreconcile"
	"github.com/edisonshen/fleet/internal/coordreconcile/reconciletest"
)

func installCLIResolveFromState(t *testing.T, st *reconciletest.State) *int {
	t.Helper()
	calls := 0
	prevResolve := attachResolveFnVar
	attachResolveFnVar = func(project, agentID string) (coordreconcile.Verdict, error) {
		calls++
		return coordreconcile.Resolve(st.Deps(), project, agentID)
	}
	t.Cleanup(func() { attachResolveFnVar = prevResolve })
	return &calls
}

func installCLIResolveError(t *testing.T, err error) *int {
	t.Helper()
	calls := 0
	prevResolve := attachResolveFnVar
	attachResolveFnVar = func(string, string) (coordreconcile.Verdict, error) {
		calls++
		return coordreconcile.Verdict{}, err
	}
	t.Cleanup(func() { attachResolveFnVar = prevResolve })
	return &calls
}

func forceCLICurrentOwner(t *testing.T, ok bool, ownerID string) {
	t.Helper()
	prev := attachCurrentOwnerFnVar
	attachCurrentOwnerFnVar = func(string) (coordlock.Owner, bool) {
		if !ok {
			return coordlock.Owner{}, false
		}
		return coordlock.Owner{AgentID: ownerID, PID: 1}, true
	}
	t.Cleanup(func() { attachCurrentOwnerFnVar = prev })
}

func TestTA1_CLIVerdictMatrixConsumesResolveActions(t *testing.T) {
	cases := []struct {
		name             string
		state            reconciletest.State
		seedOwner        string
		ownerReachable   bool
		ownerProbeErr    bool
		currentOwnerOK   bool
		wantErr          bool
		wantExit         int
		wantAttached     string
		wantDispatches   int
		wantWarning      string
		wantDiagnostic   string
		wantClaims       int
		wantSupersedes   int
		wantResolveMin   int
		wantNoOrphanKill bool
	}{
		{
			name:           "Attach-healthy",
			state:          reconciletest.State{LiveOwner: &coordlock.Owner{AgentID: "owneraaa", PID: 101}},
			seedOwner:      "owneraaa",
			ownerReachable: true,
			currentOwnerOK: true,
			wantAttached:   "fleet-owneraaa",
			wantResolveMin: 1,
		},
		{
			name:           "Attach-hung-TTL",
			state:          reconciletest.State{LiveOwner: &coordlock.Owner{AgentID: "hungaaaa", PID: 102}},
			seedOwner:      "hungaaaa",
			ownerReachable: true,
			currentOwnerOK: false,
			wantAttached:   "fleet-hungaaaa",
			wantWarning:    "heartbeat is stale",
			wantResolveMin: 1,
		},
		{
			name:           "Attach-unreachable-cross-socket",
			state:          reconciletest.State{LiveOwner: &coordlock.Owner{AgentID: "xsocket1", PID: 103}},
			seedOwner:      "xsocket1",
			ownerReachable: false,
			ownerProbeErr:  true,
			currentOwnerOK: true,
			wantErr:        true,
			wantExit:       dispatchVetoExitCode,
			wantDiagnostic: "FLEET_TMUX_SOCKET",
			wantResolveMin: 1,
		},
		{
			// T13 (flock-only D6): the flock is HELD by a live coord (LiveOwner
			// ok) but its body is identity-less (AgentID==""). Resolve returns
			// WAIT, not Attach-with-empty-id — so attach does a bounded lease wait
			// (poll for the identity), NEVER a hard "empty owner id" dead-end and
			// NEVER a respawn beside the live holder (attach-never-exits).
			name:           "Wait-identity-less-flock-holder-no-respawn",
			state:          reconciletest.State{LiveOwner: &coordlock.Owner{AgentID: "", PID: 104}},
			wantErr:        true,
			wantExit:       dispatchVetoExitCode,
			wantDiagnostic: "bounded lease wait",
			wantResolveMin: 2,
		},
		{
			name:           "Attach-owner-record-load-failure-no-respawn",
			state:          reconciletest.State{LiveOwner: &coordlock.Owner{AgentID: "missing1", PID: 105}},
			wantErr:        true,
			wantExit:       dispatchVetoExitCode,
			wantDiagnostic: "agent record is not readable",
			wantResolveMin: 1,
		},
		{
			name:           "Attach-definitively-dead-session-no-respawn",
			state:          reconciletest.State{LiveOwner: &coordlock.Owner{AgentID: "deadlv01", PID: 106}},
			seedOwner:      "deadlv01",
			ownerReachable: false,
			currentOwnerOK: true,
			wantErr:        true,
			wantExit:       dispatchVetoExitCode,
			wantDiagnostic: "session fleet-deadlv01 is gone",
			wantResolveMin: 1,
		},
		{
			name: "Wait-starting",
			state: reconciletest.State{Starting: &coordlock.StartingStatus{
				Owner: coordlock.Owner{AgentID: "bootaaaa", PID: 201}, OwnerLive: true, WithinTTL: true, Epoch: 3,
			}},
			wantErr:        true,
			wantExit:       dispatchVetoExitCode,
			wantDiagnostic: "bounded lease wait",
			wantResolveMin: 2,
		},
		{
			name:           "Wait-handoff-reservation-247",
			state:          reconciletest.State{Handoff: &coordlock.Handoff{SuccessorID: "succaaaa"}},
			wantErr:        true,
			wantExit:       dispatchVetoExitCode,
			wantDiagnostic: "bounded lease wait",
			wantResolveMin: 2,
		},
		{
			name:           "Wait-fencing-CAS-race",
			state:          reconciletest.State{ClaimOK: false},
			wantErr:        true,
			wantExit:       dispatchVetoExitCode,
			wantDiagnostic: "bounded lease wait",
			wantResolveMin: 2,
			wantClaims:     2,
		},
		{
			name:           "Spawn-free",
			state:          reconciletest.State{ClaimOK: true},
			wantAttached:   "fleet-newcoord",
			wantDispatches: 1,
			wantClaims:     1,
			wantResolveMin: 1,
		},
		{
			name: "Spawn-dead-starting",
			state: reconciletest.State{
				Starting: &coordlock.StartingStatus{
					Owner: coordlock.Owner{AgentID: "deadboot", PID: 202}, OwnerLive: false, WithinTTL: false, Epoch: 4,
				},
				ClaimOK: true,
			},
			wantAttached:   "fleet-newcoord",
			wantDispatches: 1,
			wantClaims:     1,
			wantResolveMin: 1,
		},
		{
			name: "SpawnStandby",
			state: reconciletest.State{
				Starting: &coordlock.StartingStatus{
					Owner: coordlock.Owner{AgentID: "zombie01", PID: 203}, OwnerLive: true, WithinTTL: false, Epoch: 5,
				},
				SupersedeOK: true,
			},
			wantAttached:     "fleet-newcoord",
			wantDispatches:   1,
			wantSupersedes:   1,
			wantResolveMin:   1,
			wantNoOrphanKill: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newFailoverSetup(t)
			s.installStubs(t)
			if tc.seedOwner != "" {
				if tc.ownerReachable {
					s.addLiveCoord(t, "demo", tc.seedOwner)
				} else {
					s.addRecordOnly(t, "demo", tc.seedOwner)
				}
				forceCLICurrentOwner(t, tc.currentOwnerOK, tc.seedOwner)
				if tc.ownerProbeErr {
					prevProbe := sessionProbeFnVar
					session := "fleet-" + tc.seedOwner
					sessionProbeFnVar = func(probed string) (bool, error) {
						if probed == session {
							return false, errors.New("tmux socket refused")
						}
						return s.aliveSessions[probed], nil
					}
					t.Cleanup(func() { sessionProbeFnVar = prevProbe })
				}
			}
			st := tc.state
			resolveCalls := installCLIResolveFromState(t, &st)

			stderr, err := s.run(t, "tok", AttachOpts{
				Project:             "demo",
				IsTty:               false,
				ResolveWaitInterval: 0,
				ResolveMaxAttempts:  1,
			})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if got := ExitCodeFor(err); got != tc.wantExit {
					t.Fatalf("ExitCodeFor(err) = %d, want %d (err=%v)", got, tc.wantExit, err)
				}
				if tc.wantDiagnostic != "" && !strings.Contains(err.Error()+stderr.String(), tc.wantDiagnostic) {
					t.Fatalf("diagnostic should contain %q; stderr=%q err=%v", tc.wantDiagnostic, stderr.String(), err)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v\nstderr=%s", err, stderr.String())
			}
			if s.attachedTo != tc.wantAttached {
				t.Errorf("attachedTo = %q, want %q", s.attachedTo, tc.wantAttached)
			}
			if len(s.dispatched) != tc.wantDispatches {
				t.Errorf("dispatches = %v, want %d", s.dispatched, tc.wantDispatches)
			}
			if tc.wantWarning != "" && !strings.Contains(stderr.String(), tc.wantWarning) {
				t.Errorf("stderr should contain warning %q; got %q", tc.wantWarning, stderr.String())
			}
			if st.ClaimCalls != tc.wantClaims {
				t.Errorf("ClaimStarting calls = %d, want %d", st.ClaimCalls, tc.wantClaims)
			}
			if st.SupersedeCalls != tc.wantSupersedes {
				t.Errorf("Supersede calls = %d, want %d", st.SupersedeCalls, tc.wantSupersedes)
			}
			if tc.wantClaims > 0 && st.ClaimedID != "claimtok" {
				t.Errorf("claimed id = %q, want preallocated claimtok", st.ClaimedID)
			}
			if *resolveCalls < tc.wantResolveMin {
				t.Errorf("Resolve calls = %d, want at least %d", *resolveCalls, tc.wantResolveMin)
			}
			if tc.wantNoOrphanKill && len(s.killedSessions) != 0 {
				t.Errorf("SpawnStandby must not run orphan kill; killed %v", s.killedSessions)
			}
		})
	}
}

func TestTA2_CLICoordTierHitResolvesCurrentHolder(t *testing.T) {
	s := newFailoverSetup(t)
	s.installStubs(t)
	s.addLiveCoord(t, "demo", "oldcoord")
	s.addLiveCoord(t, "demo", "holder01")

	var gotProject, gotAgentID string
	prevResolve := attachResolveFnVar
	attachResolveFnVar = func(project, agentID string) (coordreconcile.Verdict, error) {
		if agentID != "" {
			gotProject, gotAgentID = project, agentID
		}
		return coordreconcile.Verdict{
			Decision: coordreconcile.Attach,
			Owner:    coordlock.Owner{AgentID: "holder01", PID: 1},
			Reason:   "holder wins",
		}, nil
	}
	t.Cleanup(func() { attachResolveFnVar = prevResolve })

	stderr, err := s.run(t, "oldcoord", AttachOpts{IsTty: false})
	if err != nil {
		t.Fatalf("unexpected error: %v\nstderr=%s", err, stderr.String())
	}
	if gotProject != "demo" {
		t.Fatalf("Resolve project = %q, want live record project demo", gotProject)
	}
	if gotAgentID == "" {
		t.Fatal("Resolve must receive a preallocated agent id")
	}
	if s.attachedTo != "fleet-holder01" {
		t.Fatalf("attachedTo = %q, want holder session; direct old coord attach would be fleet-oldcoord", s.attachedTo)
	}
	if len(s.dispatched) != 0 {
		t.Fatalf("coord Tier-1 holder resolution must not spawn; dispatched=%v", s.dispatched)
	}
}

func TestTA4_CLIResolveErrorSurfacesRecovery(t *testing.T) {
	s := newFailoverSetup(t)
	s.installStubs(t)
	resolveCalls := installCLIResolveError(t, errors.New("epoch unreadable"))

	stderr, err := s.run(t, "tok", AttachOpts{Project: "demo", IsTty: false})
	if err == nil {
		t.Fatal("expected resolve error")
	}
	if ExitCodeFor(err) != 70 {
		t.Fatalf("ExitCodeFor(err) = %d, want 70 (err=%v)", ExitCodeFor(err), err)
	}
	for _, want := range []string{"cannot resolve coordinator lease", "fleet doctor", "fleet attach tok --project demo"} {
		if !strings.Contains(err.Error()+stderr.String(), want) {
			t.Fatalf("resolve error diagnostic missing %q; stderr=%q err=%v", want, stderr.String(), err)
		}
	}
	if *resolveCalls != 1 {
		t.Fatalf("Resolve calls = %d, want 1", *resolveCalls)
	}
	if s.attachCalls != 0 || len(s.dispatched) != 0 {
		t.Fatalf("resolve error must not attach/spawn; attach=%d dispatch=%v", s.attachCalls, s.dispatched)
	}
}

func TestTA6_CLIOrphanTmuxKilledBeforeSpawn(t *testing.T) {
	s := newFailoverSetup(t)
	s.installStubs(t)
	s.addArchivedRecord(t, "deadbeef", "demo", agent.ArchivedCauseKill, "")
	s.addOrphanTmux(t, "deadbeef")
	st := reconciletest.State{ClaimOK: true}
	installCLIResolveFromState(t, &st)

	stderr, err := s.run(t, "tok", AttachOpts{Project: "demo", IsTty: false})
	if err != nil {
		t.Fatalf("unexpected error: %v\nstderr=%s", err, stderr.String())
	}
	if got := strings.Join(s.killedSessions, ","); got != "fleet-deadbeef" {
		t.Fatalf("killed sessions = %v, want fleet-deadbeef before spawn", s.killedSessions)
	}
	if len(s.dispatched) != 1 || s.dispatched[0] != "demo" {
		t.Fatalf("dispatches = %v, want one demo spawn", s.dispatched)
	}
	if s.attachedTo != "fleet-newcoord" {
		t.Fatalf("attachedTo = %q, want spawned coord", s.attachedTo)
	}
	if strings.Contains(strings.Join(s.aliveSessionsList(), ","), "fleet-deadbeef") {
		t.Fatalf("orphan session still present after pre-spawn cleanup: %v", s.aliveSessions)
	}
}

func TestTA7a_CLIWaitLoopIsBoundedAndNoSleep(t *testing.T) {
	s := newFailoverSetup(t)
	s.installStubs(t)
	st := reconciletest.State{Handoff: &coordlock.Handoff{SuccessorID: "succwait"}}
	resolveCalls := installCLIResolveFromState(t, &st)

	stderr, err := s.run(t, "tok", AttachOpts{
		Project:             "demo",
		IsTty:               false,
		ResolveWaitInterval: 0,
		ResolveMaxAttempts:  2,
	})
	if err == nil {
		t.Fatal("expected bounded wait exhaustion")
	}
	if ExitCodeFor(err) != dispatchVetoExitCode {
		t.Fatalf("ExitCodeFor(err) = %d, want %d", ExitCodeFor(err), dispatchVetoExitCode)
	}
	if *resolveCalls != 3 {
		t.Fatalf("Resolve calls = %d, want initial + 2 retries", *resolveCalls)
	}
	if len(s.dispatched) != 0 || s.attachCalls != 0 {
		t.Fatalf("wait loop must not spawn or attach; dispatch=%v attach=%d", s.dispatched, s.attachCalls)
	}
	if !strings.Contains(err.Error()+stderr.String(), "bounded lease wait") {
		t.Fatalf("wait exhaustion diagnostic missing bounded lease wait; stderr=%q err=%v", stderr.String(), err)
	}
}

func TestTA8_CLISpawnVetoReResolvesLease(t *testing.T) {
	s := newFailoverSetup(t)
	s.installStubs(t)
	s.addLiveCoord(t, "demo", "winner01")
	spawnCalls := s.installVetoStub(t)
	forceCLICurrentOwner(t, true, "winner01")

	var readOnlyResolve int
	prevResolve := attachResolveFnVar
	attachResolveFnVar = func(project, agentID string) (coordreconcile.Verdict, error) {
		if agentID == "" {
			readOnlyResolve++
			return coordreconcile.Verdict{
				Decision: coordreconcile.Attach,
				Owner:    coordlock.Owner{AgentID: "winner01", PID: 1},
				Reason:   "veto winner",
			}, nil
		}
		return coordreconcile.Verdict{Decision: coordreconcile.Spawn, Reason: "try spawn"}, nil
	}
	t.Cleanup(func() { attachResolveFnVar = prevResolve })

	stderr, err := s.run(t, "tok", AttachOpts{Project: "demo", IsTty: false})
	if err != nil {
		t.Fatalf("unexpected error: %v\nstderr=%s", err, stderr.String())
	}
	if *spawnCalls != 1 {
		t.Fatalf("spawn calls = %d, want exactly one", *spawnCalls)
	}
	if readOnlyResolve != 1 {
		t.Fatalf("read-only re-Resolve calls = %d, want 1", readOnlyResolve)
	}
	if s.attachedTo != "fleet-winner01" {
		t.Fatalf("attachedTo = %q, want veto winner", s.attachedTo)
	}
}

func TestTA9_CLIWaitHappyPathFlipsToAttach(t *testing.T) {
	s := newFailoverSetup(t)
	s.installStubs(t)
	s.addLiveCoord(t, "demo", "holder09")
	forceCLICurrentOwner(t, true, "holder09")

	calls := 0
	prevResolve := attachResolveFnVar
	attachResolveFnVar = func(project, agentID string) (coordreconcile.Verdict, error) {
		calls++
		if calls <= 2 {
			return coordreconcile.Verdict{Decision: coordreconcile.Wait, Reason: fmt.Sprintf("cycle %d", calls)}, nil
		}
		return coordreconcile.Verdict{
			Decision: coordreconcile.Attach,
			Owner:    coordlock.Owner{AgentID: "holder09", PID: 1},
			Reason:   "winner published",
		}, nil
	}
	t.Cleanup(func() { attachResolveFnVar = prevResolve })

	stderr, err := s.run(t, "tok", AttachOpts{
		Project:             "demo",
		IsTty:               false,
		ResolveWaitInterval: 0,
		ResolveMaxAttempts:  3,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v\nstderr=%s", err, stderr.String())
	}
	if calls != 3 {
		t.Fatalf("Resolve calls = %d, want two waits then attach", calls)
	}
	if s.attachedTo != "fleet-holder09" {
		t.Fatalf("attachedTo = %q, want holder09", s.attachedTo)
	}
	if len(s.dispatched) != 0 {
		t.Fatalf("wait happy path to Attach must not spawn; dispatched=%v", s.dispatched)
	}
}

func TestTA11_CLISpawnFailureAfterPreClaimSurfacesSelfHeal(t *testing.T) {
	s := newFailoverSetup(t)
	s.installStubs(t)
	s.addArchivedRecord(t, "badc0ffe", "demo", agent.ArchivedCauseKill, "")
	s.addOrphanTmux(t, "badc0ffe")
	st := reconciletest.State{ClaimOK: true}
	installCLIResolveFromState(t, &st)

	prevKill := killTmuxSessionFnVar
	killTmuxSessionFnVar = func(session string) error {
		return fmt.Errorf("refuse kill %s", session)
	}
	t.Cleanup(func() { killTmuxSessionFnVar = prevKill })

	stderr, err := s.run(t, "tok", AttachOpts{Project: "demo", IsTty: false})
	if err == nil {
		t.Fatal("expected orphan cleanup failure")
	}
	if st.ClaimCalls != 1 || st.ClaimedID != "claimtok" {
		t.Fatalf("spawn verdict must pre-claim once before fallible cleanup; calls=%d id=%q", st.ClaimCalls, st.ClaimedID)
	}
	for _, want := range []string{"pre-spawn orphan cleanup failed", "self-heal after its TTL"} {
		if !strings.Contains(err.Error()+stderr.String(), want) {
			t.Fatalf("diagnostic missing %q; stderr=%q err=%v", want, stderr.String(), err)
		}
	}
	if len(s.dispatched) != 0 || s.attachCalls != 0 {
		t.Fatalf("cleanup failure must not dispatch/attach; dispatch=%v attach=%d", s.dispatched, s.attachCalls)
	}
}

func (s *failoverSetup) aliveSessionsList() []string {
	out := make([]string, 0, len(s.aliveSessions))
	for session := range s.aliveSessions {
		out = append(out, session)
	}
	return out
}
