//go:build linux || darwin

package coordlock

// diagnose_test.go — unit tests for the read-only Diagnose accessor (PR6 of
// DESIGN-handoff-drain-storm-leak). They assert the classification agrees
// with the acquire path's health predicate (the whole point of reusing
// holderHealthy rather than reinventing the staleness math), across every
// LeaseHealth: healthy / hung (alive+frozen) / dead (pid gone) /
// fenced_not_acquired / released / none. Deterministic via the same
// fakeClock + fakeLiveness seams the lease tests use — no time.Sleep.

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

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

// TestDiagnose_BusyFlockNoEpoch_Hung: a holder grabbed coordinator.flock and
// hung BEFORE writing coordinator.epoch (the acquire-to-epoch window). The
// acquire path treats that as recoverable via the flock body, so Diagnose
// must classify it Hung (not None) — else `fleet doctor` says "no coord" and
// leaves the stuck holder (codex PR6 iter-4 [P2]).
func TestDiagnose_BusyFlockNoEpoch_Hung(t *testing.T) {
	setupHome(t)
	const project = "diag-busyflock"
	clk := &fakeClock{}
	live := newFakeLiveness()
	cfg := testCfg(clk, live)

	paths, err := resolvePaths(project)
	if err != nil {
		t.Fatalf("resolvePaths: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.flock), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Hold the flock from a separate fd (a "holder"), write NO epoch, and stamp
	// a STALE flock body (mono far in the past) so flockHolderRecoverable=true.
	f, err := os.OpenFile(paths.flock, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open flock: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() })
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("flock: %v", err)
	}
	// Stale body: pid not live + mono older than TTL -> recoverable (hung).
	staleBody := `{"pid":999999,"pid_start":1,"boot_id":"test-boot-1","mono":0}`
	if _, werr := f.WriteAt([]byte(staleBody), 0); werr != nil {
		t.Fatalf("write body: %v", werr)
	}
	_ = f.Sync()
	clk.advance(2 * cfg.ttl) // now is well past the body's mono=0

	got := diagnoseWithCfg(project, cfg)
	if got.Health != LeaseHealthHung {
		t.Fatalf("busy flock + no epoch + stale body: Health=%v, want Hung", got.Health)
	}
}

// TestDiagnose_NoEpoch_FreeFlock_None: no epoch AND a FREE flock -> truly no
// leader (None).
func TestDiagnose_NoEpoch_FreeFlock_None(t *testing.T) {
	setupHome(t)
	const project = "diag-noflock"
	cfg := testCfg(&fakeClock{}, newFakeLiveness())
	if got := diagnoseWithCfg(project, cfg); got.Health != LeaseHealthNone {
		t.Fatalf("no epoch + free flock: Health=%v, want None", got.Health)
	}
}

// TestDiagnose_Released_AlwaysReleased: a `released` record is ALWAYS
// LeaseHealthReleased (a coord that intended to stop) — even if the flock is
// still held in the normal Release window. A one-shot read-only probe can't
// tell the millisecond drop window from a wedged releaser, and respawning an
// intentionally-stopping coord is wrong; gc reaps a truly-stuck flock (codex
// PR6 iter-9 [P2]).
func TestDiagnose_Released_AlwaysReleased(t *testing.T) {
	setupHome(t)
	const project = "diag-released"
	clk := &fakeClock{}
	live := newFakeLiveness()
	cfg := testCfg(clk, live)

	owner := identity{Pid: 4242, PidStart: 424242, AgentID: "rel", Project: project}
	live.set(owner.Pid, owner.PidStart)
	writeEpochRaw(t, project, epochRecord{
		Epoch: 8, State: stateReleased, Owner: owner,
		BootID: "test-boot-1", RenewedAtMono: clk.now(),
	})

	// Flock free -> Released.
	if got := diagnoseWithCfg(project, cfg); got.Health != LeaseHealthReleased {
		t.Fatalf("released + flock free: Health=%v, want Released", got.Health)
	}
	// Flock STILL held (Release window) -> STILL Released, NOT Hung.
	rel := holdFlock(t, project)
	defer rel()
	if got := diagnoseWithCfg(project, cfg); got.Health != LeaseHealthReleased {
		t.Fatalf("released + flock held (Release window): Health=%v, want Released (never respawn an intentional stop)", got.Health)
	}
}

// TestDiagnose_ReadOnly_DoesNotCreateFlock: a read-only Diagnose must NOT
// create coordinator.flock just to inspect a project with no epoch (codex PR6
// iter-8 [P2]).
func TestDiagnose_ReadOnly_DoesNotCreateFlock(t *testing.T) {
	setupHome(t)
	const project = "diag-nocreate"
	// Initialize the project's .locks dir but write NO flock and NO epoch.
	paths, err := resolvePaths(project)
	if err != nil {
		t.Fatalf("resolvePaths: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.flock), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	cfg := testCfg(&fakeClock{}, newFakeLiveness())
	if got := diagnoseWithCfg(project, cfg); got.Health != LeaseHealthNone {
		t.Fatalf("no epoch + no flock: Health=%v, want None", got.Health)
	}
	if _, statErr := os.Stat(paths.flock); statErr == nil {
		t.Fatalf("Diagnose created coordinator.flock during a read-only inspection")
	} else if !os.IsNotExist(statErr) {
		t.Fatalf("unexpected stat error: %v", statErr)
	}
}
