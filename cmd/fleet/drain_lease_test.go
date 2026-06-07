//go:build linux || darwin

// Tests for the BOUNDED, lease-aware drain path (DESIGN-handoff-drain-
// storm-leak §3(D), PR3). Deterministic — injectable drainLeaseDeps, no real
// lease/kill/Resume/clock. Channels + counters observe convergence; no
// time.Sleep timing assertions (the one bounded-wait test uses a tiny budget
// + a never-appearing barrier, asserting it RETURNS, not a wall-clock value).
//
//	T13  healthy leader + no barrier -> STAND DOWN (exit 0, kill nothing).
//	T25  no barrier (leader unresponsive) -> NO graceful kill; escalate.
//	T42  graceful kill routes through KillCoordIfIdentityMatches; a refusal
//	     (PID reuse) is surfaced, not a wrong-process kill.
//	T41  hung OLD (no barrier, deadline passes) -> escalate to TakeOver.
//	T12  bounded: a Resume that blocks past the budget RETURNS at ~budget,
//	     holds no lock, and a second drain is not blocked.
//	T40  drainOne (failover) holds no lock across Resume — a sibling
//	     LockAgent acquires immediately while Resume is in flight.
package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/coord"
	"github.com/edisonshen/fleet/internal/queue"
	"github.com/edisonshen/fleet/internal/state"
)

func leaseDrainReq() queue.SpawnFresh {
	return queue.SpawnFresh{
		OldAgentID: "oldcoord1",
		Project:    "projects-fleet",
		TaskID:     "coord",
		HandoffDoc: "/tmp/doc.md",
	}
}

func oldCoordRec() *agent.Record {
	r := agent.New("oldcoord1")
	r.Project = "projects-fleet"
	r.TaskID = "coord"
	r.SupervisorPID = 424242
	r.SupervisorPidStart = 99999
	r.SupervisorExePath = "/usr/local/bin/fleet"
	return r
}

// T13: a healthy leader holds the lease, no barrier, and OLD is ALREADY
// ARCHIVED (the handoff already completed — the queue file is a stale
// leftover) — drain STANDS DOWN: cleans the stale queue, kills nothing,
// spawns nothing.
func TestDrainLease_StandDownStaleQueueUnderLiveLeader(t *testing.T) {
	var killed, resumed int32
	out := &bytes.Buffer{}
	d := drainLeaseDeps{
		LeaderPresent: func(string) bool { return true },
		CurrentEpoch:  func(string) (int64, bool) { return 5, true },
		BarrierExists: func(string, int64) bool { return false },
		LoadAgent:     func(string) (*agent.Record, error) { return nil, state.ErrNotFound }, // OLD archived
		KillCoord:     func(coord.KillTarget) error { atomic.AddInt32(&killed, 1); return nil },
		Resume: func(queue.SpawnFresh, string, int, io.Writer, io.Writer) error {
			atomic.AddInt32(&resumed, 1)
			return nil
		},
	}

	err := drainOneLeaseAwareWith(leaseDrainReq(), "/tmp/does-not-exist-q.json", 0, 1000, out, out, d)
	if err != nil {
		t.Fatalf("stale-queue stand-down must return nil, got %v", err)
	}
	if !strings.Contains(out.String(), "already retired") {
		t.Errorf("expected stale-queue stand-down message, got: %q", out.String())
	}
	if atomic.LoadInt32(&killed) != 0 {
		t.Errorf("kill ran %d times, want 0", killed)
	}
	if atomic.LoadInt32(&resumed) != 0 {
		t.Errorf("Resume ran %d times, want 0", resumed)
	}
}

// codex PR3 iter-5 [P1]: a healthy leader, no barrier, but OLD STILL LIVE +
// a pending coord handoff queue file — the graceful producer hasn't written a
// barrier yet, so drain must COMPLETE the handoff via the legacy Resume
// fallback (under a per-agent lock), NOT stand down and strand the queue.
func TestDrainLease_LiveLeaderPendingHandoff_FallsBackToResume(t *testing.T) {
	var resumed, locked int32
	out := &bytes.Buffer{}
	d := drainLeaseDeps{
		LeaderPresent: func(string) bool { return true },
		CurrentEpoch:  func(string) (int64, bool) { return 5, true },
		BarrierExists: func(string, int64) bool { return false },
		LoadAgent:     func(string) (*agent.Record, error) { return oldCoordRec(), nil }, // OLD still live
		LockAgent: func(string) (func(), error) {
			atomic.AddInt32(&locked, 1)
			return func() {}, nil
		},
		Resume: func(queue.SpawnFresh, string, int, io.Writer, io.Writer) error {
			atomic.AddInt32(&resumed, 1)
			return nil
		},
	}
	if err := drainOneLeaseAwareWith(leaseDrainReq(), "/tmp/q.json", 0, 1000, out, out, d); err != nil {
		t.Fatalf("fallback resume returned %v, want nil", err)
	}
	if atomic.LoadInt32(&resumed) != 1 {
		t.Errorf("legacy Resume fallback ran %d times, want 1 (queue must not be stranded)", resumed)
	}
	if atomic.LoadInt32(&locked) != 1 {
		t.Errorf("fallback Resume took the per-agent lock %d times, want 1", locked)
	}
}

// codex PR3 iter-6 [P2]: after a graceful standby already acquired the lease,
// CurrentEpoch returns the SUCCESSOR's new epoch (so the current-epoch barrier
// check misses OLD's barrier) and OLD's record may not be archived yet. The
// live-leader fallback must NOT run legacy Resume (which would spawn a SECOND
// replacement) — it must detect that the active owner != OLD and clean the
// stale queue.
func TestDrainLease_SuccessorAlreadyLeads_NoDuplicateSpawn(t *testing.T) {
	var resumed int32
	out := &bytes.Buffer{}
	d := drainLeaseDeps{
		LeaderPresent:  func(string) bool { return true },                                 // the SUCCESSOR is healthy
		CurrentEpoch:   func(string) (int64, bool) { return 6, true },                     // successor's new epoch
		BarrierExists:  func(string, int64) bool { return false },                         // no barrier at epoch 6 (OLD's was epoch 5)
		ActiveOwnerPID: func(string) (int, bool) { return 555555, true },                  // successor pid != OLD's 424242
		LoadAgent:      func(string) (*agent.Record, error) { return oldCoordRec(), nil }, // OLD not yet archived
		Resume: func(queue.SpawnFresh, string, int, io.Writer, io.Writer) error {
			atomic.AddInt32(&resumed, 1)
			return nil
		},
		LockAgent: func(string) (func(), error) { return func() {}, nil },
	}
	if err := drainOneLeaseAwareWith(leaseDrainReq(), "/tmp/q.json", 0, 1000, out, out, d); err != nil {
		t.Fatalf("successor-already-leads path returned %v, want nil", err)
	}
	if atomic.LoadInt32(&resumed) != 0 {
		t.Errorf("legacy Resume ran %d times — spawned a DUPLICATE for an already-completed handoff", resumed)
	}
	if !strings.Contains(out.String(), "successor already leads") {
		t.Errorf("expected successor-already-leads message, got: %q", out.String())
	}
}

// T25: no barrier present and the leader is unresponsive (deadline passes) —
// drain does NOT graceful-kill OLD before the barrier; it escalates instead.
// Combined with T41 here: the escalation calls TakeOver.
func TestDrainLease_NoBarrierNoGracefulKill_Escalates(t *testing.T) {
	var killed, tookOver, recovered int32
	var recoveredFromOld *agent.Record
	out := &bytes.Buffer{}
	d := drainLeaseDeps{
		LeaderPresent: func(string) bool { return false }, // hung/stealable
		CurrentEpoch:  func(string) (int64, bool) { return 5, true },
		BarrierExists: func(string, int64) bool { return false }, // never
		LoadAgent:     func(string) (*agent.Record, error) { return oldCoordRec(), nil },
		KillCoord:     func(coord.KillTarget) error { atomic.AddInt32(&killed, 1); return nil },
		TakeOver: func(string, string) (bool, error) {
			atomic.AddInt32(&tookOver, 1)
			return true, nil
		},
		RecoverSpawn: func(oldRec *agent.Record, _ string, _, _ io.Writer) error {
			atomic.AddInt32(&recovered, 1)
			recoveredFromOld = oldRec
			return nil
		},
		BarrierPoll: time.Millisecond,
	}
	// tiny budget so the bounded wait returns fast (not a timing assertion —
	// we assert it RETURNS + escalates, not how long it took).
	err := drainOneLeaseAwareWith(leaseDrainReq(), "/tmp/q.json", 0, 5, out, out, d)
	if !errors.Is(err, ErrEscalatedToTakeOver) {
		t.Fatalf("expected ErrEscalatedToTakeOver, got %v", err)
	}
	if atomic.LoadInt32(&killed) != 0 {
		t.Errorf("graceful kill ran %d times BEFORE the barrier — invariant violated", killed)
	}
	if atomic.LoadInt32(&tookOver) != 1 {
		t.Errorf("TakeOver escalation ran %d times, want 1", tookOver)
	}
	// codex PR3 iter-6/7 [P1]: a lease-wrapped successor must be recovered from
	// the CACHED old record after the takeover (no coordless project; not via
	// handoffop.Resume through the killed record).
	if atomic.LoadInt32(&recovered) != 1 {
		t.Errorf("RecoverSpawn ran %d times after takeover, want 1", recovered)
	}
	if recoveredFromOld == nil || recoveredFromOld.ID != "oldcoord1" {
		t.Errorf("RecoverSpawn was not given the cached old record, got %+v", recoveredFromOld)
	}
}

// T41: a HUNG OLD (no barrier, deadline passes) escalates to TakeOver, then
// recovers a lease-wrapped successor from the cached record, and never blocks.
func TestDrainLease_HungOldEscalatesToTakeOver(t *testing.T) {
	var tookOverProject string
	var recovered int32
	out := &bytes.Buffer{}
	done := make(chan error, 1)
	d := drainLeaseDeps{
		LeaderPresent: func(string) bool { return false },
		CurrentEpoch:  func(string) (int64, bool) { return 9, true },
		BarrierExists: func(string, int64) bool { return false },
		LoadAgent:     func(string) (*agent.Record, error) { return oldCoordRec(), nil },
		TakeOver: func(project, _ string) (bool, error) {
			tookOverProject = project
			return false, nil // OLD fenced; flock not acquired by the drain
		},
		RecoverSpawn: func(*agent.Record, string, io.Writer, io.Writer) error {
			atomic.AddInt32(&recovered, 1)
			return nil
		},
		BarrierPoll: time.Millisecond,
	}
	go func() {
		done <- drainOneLeaseAwareWith(leaseDrainReq(), "/tmp/q.json", 0, 5, out, out, d)
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrEscalatedToTakeOver) {
			t.Fatalf("expected escalation signal, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("drain BLOCKED on a hung OLD — the forever-hold regression")
	}
	if tookOverProject != "projects-fleet" {
		t.Errorf("TakeOver called for project %q, want projects-fleet", tookOverProject)
	}
	if atomic.LoadInt32(&recovered) != 1 {
		t.Errorf("RecoverSpawn ran %d times after takeover, want 1 (no coordless project)", recovered)
	}
}

// T42: the graceful kill routes through KillCoordIfIdentityMatches. A refusal
// (e.g. PID reuse / start-time mismatch) is surfaced; the drain does not
// claim success and does not delete the queue.
func TestDrainLease_GracefulKillRefusalSurfaced(t *testing.T) {
	out := &bytes.Buffer{}
	refusal := errors.New("PID REUSE — start-time changed")
	var target coord.KillTarget
	d := drainLeaseDeps{
		LeaderPresent: func(string) bool { return true },
		CurrentEpoch:  func(string) (int64, bool) { return 5, true },
		BarrierExists: func(string, int64) bool { return true }, // graceful path
		// OLD is NOT the active owner (already releasing/stale) -> direct reap
		// path (not the self-release wait).
		ActiveOwnerPID: func(string) (int, bool) { return 0, false },
		LoadAgent: func(string) (*agent.Record, error) {
			return oldCoordRec(), nil
		},
		KillCoord: func(kt coord.KillTarget) error {
			target = kt
			return refusal
		},
	}
	err := drainOneLeaseAwareWith(leaseDrainReq(), "/tmp/q.json", 0, 1000, out, out, d)
	if err == nil || !errors.Is(err, refusal) {
		t.Fatalf("expected the kill refusal surfaced, got %v", err)
	}
	// The kill went through the authenticated primitive with OLD's recorded
	// identity (not an unguarded kill).
	if target.Pid != 424242 || target.PidStart != 99999 {
		t.Errorf("kill target identity = %+v, want OLD's supervisor pid/start", target)
	}
	if target.FencerEpoch != 5 {
		t.Errorf("kill FencerEpoch = %d, want the barrier epoch 5", target.FencerEpoch)
	}
}

// T42(b): graceful path — OLD already archived (barrier present, record
// gone) — clean the queue, no kill, no error.
func TestDrainLease_GracefulOldAlreadyArchived(t *testing.T) {
	out := &bytes.Buffer{}
	var killed int32
	d := drainLeaseDeps{
		LeaderPresent:  func(string) bool { return true },
		CurrentEpoch:   func(string) (int64, bool) { return 5, true },
		BarrierExists:  func(string, int64) bool { return true },
		ActiveOwnerPID: func(string) (int, bool) { return 0, false },
		LoadAgent:      func(string) (*agent.Record, error) { return nil, state.ErrNotFound },
		KillCoord:      func(coord.KillTarget) error { atomic.AddInt32(&killed, 1); return nil },
	}
	// queue.Delete on a non-existent path is tolerated by handoffop's
	// cleanUpStaleQueue elsewhere; here Delete returns its own error which we
	// accept either way — assert no kill + no crash.
	_ = drainOneLeaseAwareWith(leaseDrainReq(), "/tmp/does-not-exist-q.json", 0, 1000, out, out, d)
	if atomic.LoadInt32(&killed) != 0 {
		t.Errorf("kill ran %d times though OLD was already archived", killed)
	}
}

// codex PR3 iter-1 [P1]: barrier present + OLD still the ACTIVE lease owner
// -> drain does NOT force-kill the active leader (the epoch gate would
// refuse); it WAITS for OLD's self-release. When OLD releases, the handoff is
// complete and the queue is cleaned — no kill, no escalation.
func TestDrainLease_GracefulWaitsForSelfRelease(t *testing.T) {
	out := &bytes.Buffer{}
	var killed, tookOver int32
	// OLD is the active owner for the first two reads, then releases.
	var reads int32
	d := drainLeaseDeps{
		CurrentEpoch:  func(string) (int64, bool) { return 5, true },
		BarrierExists: func(string, int64) bool { return true },
		ActiveOwnerPID: func(string) (int, bool) {
			if atomic.AddInt32(&reads, 1) <= 2 {
				return 424242, true // OLD still leader
			}
			return 0, false // OLD released
		},
		LoadAgent:   func(string) (*agent.Record, error) { return oldCoordRec(), nil },
		KillCoord:   func(coord.KillTarget) error { atomic.AddInt32(&killed, 1); return nil },
		TakeOver:    func(string, string) (bool, error) { atomic.AddInt32(&tookOver, 1); return true, nil },
		BarrierPoll: time.Millisecond,
	}
	err := drainOneLeaseAwareWith(leaseDrainReq(), "/tmp/q.json", 0, 1000, out, out, d)
	if err != nil {
		t.Fatalf("graceful self-release path returned %v, want nil", err)
	}
	if atomic.LoadInt32(&killed) != 0 {
		t.Errorf("force-killed the active leader %d times — must wait for self-release", killed)
	}
	if atomic.LoadInt32(&tookOver) != 0 {
		t.Errorf("escalated to takeover %d times — OLD released cleanly, no escalation needed", tookOver)
	}
	if !strings.Contains(out.String(), "released the lease") {
		t.Errorf("expected self-release completion message, got: %q", out.String())
	}
}

// codex PR3 iter-1 [P1]: barrier present + OLD STILL the active owner past the
// budget -> escalate to takeover (OLD wedged after writing the barrier).
func TestDrainLease_GracefulOldHoldsLeasePastBudget_Escalates(t *testing.T) {
	out := &bytes.Buffer{}
	var tookOver int32
	d := drainLeaseDeps{
		CurrentEpoch:   func(string) (int64, bool) { return 5, true },
		BarrierExists:  func(string, int64) bool { return true },
		ActiveOwnerPID: func(string) (int, bool) { return 424242, true }, // never releases
		LoadAgent:      func(string) (*agent.Record, error) { return oldCoordRec(), nil },
		TakeOver:       func(string, string) (bool, error) { atomic.AddInt32(&tookOver, 1); return false, nil },
		BarrierPoll:    time.Millisecond,
	}
	err := drainOneLeaseAwareWith(leaseDrainReq(), "/tmp/q.json", 0, 5, out, out, d)
	if !errors.Is(err, ErrEscalatedToTakeOver) {
		t.Fatalf("expected escalation when OLD holds the lease past budget, got %v", err)
	}
	if atomic.LoadInt32(&tookOver) != 1 {
		t.Errorf("TakeOver ran %d times, want 1", tookOver)
	}
}

// codex PR3 iter-1 [P2]: failover on, NO lease epoch for the project (stealable
// / cold) -> cold-spawn the successor via a bounded Resume (NOT a takeover
// loop that never spawns a replacement).
func TestDrainLease_NoEpochColdSpawnsViaResume(t *testing.T) {
	out := &bytes.Buffer{}
	var resumed, tookOver int32
	var locked int32
	d := drainLeaseDeps{
		LeaderPresent:  func(string) bool { return false },
		CurrentEpoch:   func(string) (int64, bool) { return 0, false }, // no lease ever held
		BarrierExists:  func(string, int64) bool { return false },
		ActiveOwnerPID: func(string) (int, bool) { return 0, false },
		LockAgent: func(string) (func(), error) {
			atomic.AddInt32(&locked, 1)
			return func() {}, nil
		},
		Resume: func(queue.SpawnFresh, string, int, io.Writer, io.Writer) error {
			atomic.AddInt32(&resumed, 1)
			return nil
		},
		TakeOver: func(string, string) (bool, error) { atomic.AddInt32(&tookOver, 1); return false, nil },
	}
	err := drainOneLeaseAwareWith(leaseDrainReq(), "/tmp/q.json", 0, 1000, out, out, d)
	if err != nil {
		t.Fatalf("cold-spawn path returned %v, want nil", err)
	}
	if atomic.LoadInt32(&resumed) != 1 {
		t.Errorf("Resume (cold-spawn) ran %d times, want 1", resumed)
	}
	if atomic.LoadInt32(&tookOver) != 0 {
		t.Errorf("escalated to takeover %d times on a cold no-leader queue — must cold-spawn instead", tookOver)
	}
	if atomic.LoadInt32(&locked) != 1 {
		t.Errorf("cold Resume took the per-agent lock %d times, want 1 (serialization contract)", locked)
	}
}

// codex PR3 iter-4 [P1]: the cold Resume path SERIALIZES via a per-agent lock
// (Resume's contract) — coldResume takes LockAgent around Resume. This is the
// stealable-lease cold-spawn case, NOT the hung-leader swap, so the brief lock
// does not reintroduce the 81-leak.
func TestDrainLease_ColdResume_HoldsPerAgentLock(t *testing.T) {
	out := &bytes.Buffer{}
	var lockedDuringResume bool
	var locked, released int32
	d := drainLeaseDeps{
		CurrentEpoch: func(string) (int64, bool) { return 0, false }, // stealable
		LockAgent: func(string) (func(), error) {
			atomic.AddInt32(&locked, 1)
			return func() { atomic.AddInt32(&released, 1) }, nil
		},
		Resume: func(queue.SpawnFresh, string, int, io.Writer, io.Writer) error {
			// The lock must be held while Resume runs (serialization).
			lockedDuringResume = atomic.LoadInt32(&locked) == 1 && atomic.LoadInt32(&released) == 0
			return nil
		},
	}
	if err := drainOneLeaseAwareWith(leaseDrainReq(), "/tmp/q.json", 0, 1000, out, out, d); err != nil {
		t.Fatalf("cold resume returned %v, want nil", err)
	}
	if !lockedDuringResume {
		t.Error("cold Resume ran WITHOUT the per-agent lock held — serialization contract broken")
	}
	if atomic.LoadInt32(&released) != 1 {
		t.Errorf("per-agent lock released %d times, want 1 (must always release)", released)
	}
}

// codex PR3 iter-8 [P1]: a legacy Resume that HANGS past the budget must NOT
// block the drain — coldResume escalates to the safety-net takeover + recovery
// after --resume-timeout-ms instead of blocking forever (the drain-storm
// failure class). The drain returns ErrEscalatedToTakeOver promptly.
func TestDrainLease_ColdResumeHangs_EscalatesAfterBudget(t *testing.T) {
	out := &bytes.Buffer{}
	var tookOver, recovered int32
	blockResume := make(chan struct{})
	defer close(blockResume)

	d := drainLeaseDeps{
		// Live-leader fallback path: healthy leader, no barrier, OLD still live.
		LeaderPresent:  func(string) bool { return true },
		CurrentEpoch:   func(string) (int64, bool) { return 5, true },
		BarrierExists:  func(string, int64) bool { return false },
		ActiveOwnerPID: func(string) (int, bool) { return 424242, true }, // OLD is the owner
		LoadAgent:      func(string) (*agent.Record, error) { return oldCoordRec(), nil },
		LockAgent:      func(string) (func(), error) { return func() {}, nil },
		Resume: func(queue.SpawnFresh, string, int, io.Writer, io.Writer) error {
			<-blockResume // hang past the budget
			return nil
		},
		TakeOver: func(string, string) (bool, error) { atomic.AddInt32(&tookOver, 1); return false, nil },
		RecoverSpawn: func(*agent.Record, string, io.Writer, io.Writer) error {
			atomic.AddInt32(&recovered, 1)
			return nil
		},
	}

	done := make(chan error, 1)
	go func() {
		// tiny budget so the hang is detected fast (assert RETURN + escalate).
		done <- drainOneLeaseAwareWith(leaseDrainReq(), "/tmp/q.json", 0, 5, out, out, d)
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrEscalatedToTakeOver) {
			t.Fatalf("hung legacy resume must escalate, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("coldResume BLOCKED on a hung Resume — the drain-storm regression")
	}
	if atomic.LoadInt32(&tookOver) != 1 {
		t.Errorf("escalation TakeOver ran %d times, want 1", tookOver)
	}
	if atomic.LoadInt32(&recovered) != 1 {
		t.Errorf("escalation RecoverSpawn ran %d times, want 1", recovered)
	}
}

// T40: the GRACEFUL coord path holds NO per-agent lock across the
// kill/self-release work (the root cause of the 81-leak — a lock held across a
// hung leader's slow swap — is gone). Here the graceful self-release wait runs
// while a sibling LockAgent(OLD) acquires immediately. The graceful path never
// calls Resume / LockAgent, so the sibling is never blocked.
func TestDrainLease_GracefulPath_HoldsNoPerAgentLock(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)

	out := &bytes.Buffer{}
	probed := make(chan struct{})
	var reads int32
	d := drainLeaseDeps{
		CurrentEpoch:  func(string) (int64, bool) { return 5, true },
		BarrierExists: func(string, int64) bool { return true },
		ActiveOwnerPID: func(string) (int, bool) {
			// First read: OLD still leader (enters the self-release wait, during
			// which we probe the sibling lock). Then OLD releases.
			if atomic.AddInt32(&reads, 1) == 1 {
				close(probed)
				return 424242, true
			}
			return 0, false
		},
		LoadAgent:   func(string) (*agent.Record, error) { return oldCoordRec(), nil },
		BarrierPoll: 50 * time.Millisecond,
		// LockAgent must NEVER be called on the graceful path; fail loudly if so.
		LockAgent: func(string) (func(), error) {
			t.Error("graceful path took the per-agent lock — must be lock-free")
			return func() {}, nil
		},
	}
	done := make(chan error, 1)
	go func() { done <- drainOneLeaseAwareWith(leaseDrainReq(), "/tmp/q.json", 0, 5000, out, out, d) }()
	<-probed

	// While the graceful self-release wait is running, a sibling LockAgent(OLD)
	// must acquire immediately — the drain holds no per-agent lock (T40).
	lockAcquired := make(chan struct{})
	go func() {
		rel, err := state.LockAgent("oldcoord1")
		if err == nil {
			close(lockAcquired)
			rel()
		}
	}()
	select {
	case <-lockAcquired:
	case <-time.After(5 * time.Second):
		t.Fatal("sibling LockAgent BLOCKED during the graceful wait — a per-agent lock IS held (T40 regression)")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("graceful path did not converge")
	}
}
