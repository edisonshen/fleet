//go:build linux || darwin

package coordlock

// diagnose_test.go — unit tests for the read-only Diagnose accessor (PR6 of
// DESIGN-handoff-drain-storm-leak). They assert the classification agrees
// with the acquire path's health predicate (the whole point of reusing
// holderHealthy rather than reinventing the staleness math), across every
// LeaseHealth: healthy / hung (alive+frozen) / dead (pid gone) /
// fenced_not_acquired / released / none. Deterministic via the same
// fakeClock + fakeLiveness seams the lease tests use — no time.Sleep.

import "testing"

func TestDiagnose_ClassifiesEveryHealth(t *testing.T) {
	setupHome(t)
	const project = "diag-test"
	clk := &fakeClock{}
	live := newFakeLiveness()
	cfg := testCfg(clk, live)

	const (
		ownerPid   = 7100
		ownerStart = int64(710710)
	)
	live.set(ownerPid, ownerStart)
	owner := identity{Pid: ownerPid, PidStart: ownerStart, AgentID: "owner", Project: project}

	// none — no record at all.
	if got := diagnoseWithCfg(project, cfg); got.Health != LeaseHealthNone || got.HasRecord {
		t.Fatalf("no record: got %+v, want Health=None HasRecord=false", got)
	}

	// OK — healthy active leader (alive, within TTL, same boot).
	writeEpochRaw(t, project, epochRecord{
		Epoch: 5, State: stateActive, Owner: owner,
		BootID: "test-boot-1", RenewedAtMono: clk.now(),
	})
	got := diagnoseWithCfg(project, cfg)
	if got.Health != LeaseHealthOK {
		t.Fatalf("healthy active: Health=%v, want OK", got.Health)
	}
	if got.Epoch != 5 || got.OwnerPID != ownerPid || !got.OwnerAlive || got.State != stateActive {
		t.Fatalf("healthy active snapshot wrong: %+v", got)
	}

	// HUNG — owner pid alive but heartbeat frozen past TTL (the incident).
	hungClk := &fakeClock{}
	hungClk.advance(2 * cfg.ttl) // now is well past renewed_at
	got = diagnoseWithCfg(project, testCfg(hungClk, live))
	if got.Health != LeaseHealthHung {
		t.Fatalf("hung (alive+frozen): Health=%v, want Hung", got.Health)
	}
	if !got.OwnerAlive {
		t.Fatalf("hung: OwnerAlive=false, want true (pid is alive, only heartbeat is frozen)")
	}

	// DEAD — owner pid gone (active record, but liveness probe fails).
	deadLive := newFakeLiveness() // owner NOT set -> dead
	got = diagnoseWithCfg(project, testCfg(clk, deadLive))
	if got.Health != LeaseHealthDead {
		t.Fatalf("dead owner: Health=%v, want Dead", got.Health)
	}
	if got.OwnerAlive {
		t.Fatalf("dead owner: OwnerAlive=true, want false")
	}

	// FENCED_NOT_ACQUIRED — the typed escalation state.
	writeEpochRaw(t, project, epochRecord{
		Epoch: 6, State: stateFencedNotAcquired, Owner: owner,
		BootID: "test-boot-1", RenewedAtMono: clk.now(),
	})
	if got := diagnoseWithCfg(project, cfg); got.Health != LeaseHealthFencedNotAcquired {
		t.Fatalf("fenced_not_acquired: Health=%v, want FencedNotAcquired", got.Health)
	}

	// RELEASED — holder cleanly released; no live leader.
	writeEpochRaw(t, project, epochRecord{
		Epoch: 7, State: stateReleased, Owner: owner,
		BootID: "test-boot-1", RenewedAtMono: clk.now(),
	})
	if got := diagnoseWithCfg(project, cfg); got.Health != LeaseHealthReleased {
		t.Fatalf("released: Health=%v, want Released", got.Health)
	}
}

// TestDiagnose_FailoverDisabled: with the flag explicitly off there is no
// lease in play, so Diagnose reports None regardless of any on-disk record
// (reversibility).
func TestDiagnose_FailoverDisabled(t *testing.T) {
	setupHome(t)
	t.Setenv(FailoverEnvVar, "0")
	const project = "diag-off"
	writeEpochRaw(t, project, epochRecord{
		Epoch: 1, State: stateActive,
		Owner:  identity{Pid: 1, PidStart: 1, Project: project},
		BootID: "test-boot-1",
	})
	if got := Diagnose(project); got.Health != LeaseHealthNone {
		t.Fatalf("failover off: Health=%v, want None", got.Health)
	}
}
