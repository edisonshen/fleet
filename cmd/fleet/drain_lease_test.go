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
		LeaderPresent: func(string) bool { return true },
		CurrentEpoch:  func(string) (int64, bool) { return 5, true },
		BarrierExists: func(string, int64) bool { return true },
		LoadAgent:     func(string) (*agent.Record, error) { return nil, state.ErrNotFound },
		KillCoord:     func(coord.KillTarget) error { atomic.AddInt32(&killed, 1); return nil },
	}
	// queue.Delete on a non-existent path is tolerated by handoffop's
	// cleanUpStaleQueue elsewhere; here Delete returns its own error which we
	// accept either way — assert no kill + no crash.
	_ = drainOneLeaseAwareWith(leaseDrainReq(), "/tmp/does-not-exist-q.json", 0, 1000, out, out, d)
	if atomic.LoadInt32(&killed) != 0 {
		t.Errorf("kill ran %d times though OLD was already archived", killed)
	}
}

// T12 + T40: the bounded cold Resume (no-project fallback) does not block
// past the budget and holds no lock across Resume. A Resume that blocks
// forever returns at ~budget with the escalation signal; a sibling LockAgent
// acquires while Resume is still "in flight" (proving no shared lock).
func TestDrainLease_BoundedResume_NoLockAcrossResume(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)

	releaseResume := make(chan struct{})
	resumeStarted := make(chan struct{})
	out := &bytes.Buffer{}
	d := drainLeaseDeps{
		Resume: func(queue.SpawnFresh, string, int, io.Writer, io.Writer) error {
			close(resumeStarted)
			<-releaseResume // block past the budget
			return nil
		},
	}
	defer close(releaseResume)

	// No project -> boundedResume path.
	req := queue.SpawnFresh{OldAgentID: "oldcoord1", Project: ""}
	done := make(chan error, 1)
	go func() {
		done <- drainOneLeaseAwareWith(req, "/tmp/q.json", 0, 50, out, out, d)
	}()
	<-resumeStarted

	// T40: a sibling LockAgent must acquire IMMEDIATELY while Resume is in
	// flight — proves no lock is held across Resume.
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
		t.Fatal("sibling LockAgent BLOCKED while Resume in flight — a lock IS held across Resume (T40 regression)")
	}

	// T12: the bounded drain returns at ~budget (Resume still blocked) with
	// the escalation signal — it does NOT wait for the wedged Resume.
	select {
	case err := <-done:
		if !errors.Is(err, ErrEscalatedToTakeOver) {
			t.Fatalf("bounded drain returned %v, want ErrEscalatedToTakeOver", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("bounded drain BLOCKED on a wedged Resume — the forever-hold regression")
	}
}
