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

// T13: a healthy leader holds the lease and no barrier is present — drain
// STANDS DOWN: prints "coord live", returns nil, kills nothing, spawns
// nothing, holds no lock.
func TestDrainLease_StandDownUnderLiveLeader(t *testing.T) {
	var killed, resumed int32
	out := &bytes.Buffer{}
	d := drainLeaseDeps{
		LeaderPresent: func(string) bool { return true },
		CurrentEpoch:  func(string) (int64, bool) { return 5, true },
		BarrierExists: func(string, int64) bool { return false },
		KillCoord:     func(coord.KillTarget) error { atomic.AddInt32(&killed, 1); return nil },
		Resume: func(queue.SpawnFresh, string, int, io.Writer, io.Writer) error {
			atomic.AddInt32(&resumed, 1)
			return nil
		},
	}

	err := drainOneLeaseAwareWith(leaseDrainReq(), "/tmp/q.json", 0, 1000, out, out, d)
	if err != nil {
		t.Fatalf("stand-down must return nil, got %v", err)
	}
	if !strings.Contains(out.String(), "coord live") {
		t.Errorf("expected 'coord live' stand-down message, got: %q", out.String())
	}
	if atomic.LoadInt32(&killed) != 0 {
		t.Errorf("kill ran %d times under a live leader, want 0", killed)
	}
	if atomic.LoadInt32(&resumed) != 0 {
		t.Errorf("Resume ran %d times under a live leader, want 0", resumed)
	}
}

// T25: no barrier present and the leader is unresponsive (deadline passes) —
// drain does NOT graceful-kill OLD before the barrier; it escalates instead.
// Combined with T41 here: the escalation calls TakeOver.
func TestDrainLease_NoBarrierNoGracefulKill_Escalates(t *testing.T) {
	var killed, tookOver int32
	out := &bytes.Buffer{}
	d := drainLeaseDeps{
		LeaderPresent: func(string) bool { return false }, // hung/stealable
		CurrentEpoch:  func(string) (int64, bool) { return 5, true },
		BarrierExists: func(string, int64) bool { return false }, // never
		KillCoord:     func(coord.KillTarget) error { atomic.AddInt32(&killed, 1); return nil },
		TakeOver: func(string, string) (bool, error) {
			atomic.AddInt32(&tookOver, 1)
			return true, nil
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
}

// T41: a HUNG OLD (no barrier, deadline passes) escalates to TakeOver and
// never blocks. (Same escalation as T25; this asserts the typed signal +
// that the drain returned rather than hanging.)
func TestDrainLease_HungOldEscalatesToTakeOver(t *testing.T) {
	var tookOverProject string
	out := &bytes.Buffer{}
	done := make(chan error, 1)
	d := drainLeaseDeps{
		LeaderPresent: func(string) bool { return false },
		CurrentEpoch:  func(string) (int64, bool) { return 9, true },
		BarrierExists: func(string, int64) bool { return false },
		TakeOver: func(project, _ string) (bool, error) {
			tookOverProject = project
			return false, nil // OLD fenced; flock not acquired by the drain
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
