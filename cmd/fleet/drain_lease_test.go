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
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/coord"
	"github.com/edisonshen/fleet/internal/coordlock"
	"github.com/edisonshen/fleet/internal/queue"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/testutil/tmuxtest"
)

func leaseDrainReq() queue.SpawnFresh {
	return queue.SpawnFresh{
		SchemaVersion: queue.SchemaVersion,
		OldAgentID:    "oldcoord1",
		Project:       "projects-fleet",
		TaskID:        "coord",
		HandoffDoc:    "/tmp/doc.md",
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

// existingQueuePath writes a placeholder queue file in a temp dir and returns
// its path. takeoverAndRecover's concurrent-recovery guard stat()s the queue
// file (a missing file means "already recovered by a peer drain"), so tests
// that exercise the escalation must point at a file that actually exists.
func existingQueuePath(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "spawn-fresh-test.json")
	if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// T13: a healthy leader holds the lease, no barrier, and OLD is ALREADY
// ARCHIVED (the handoff already completed — the queue file is a stale
// leftover) — drain STANDS DOWN: cleans the stale queue, kills nothing,
// spawns nothing.
func TestDrainLease_StandDownStaleQueueUnderLiveLeader(t *testing.T) {
	var killed, resumed int32
	out := &bytes.Buffer{}
	d := drainLeaseDeps{
		DeliverPending: func(queue.SpawnFresh, *agent.Record, io.Writer, io.Writer) error { return nil },
		LeaderPresent:  func(string) bool { return true },
		CurrentEpoch:   func(string) (int64, bool) { return 5, true },
		BarrierExists:  func(string, int64) bool { return false },
		LoadAgent:      func(string) (*agent.Record, error) { return nil, state.ErrNotFound }, // OLD archived
		KillCoord:      func(coord.KillTarget) error { atomic.AddInt32(&killed, 1); return nil },
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
		DeliverPending: func(queue.SpawnFresh, *agent.Record, io.Writer, io.Writer) error { return nil },
		LeaderPresent:  func(string) bool { return true },
		CurrentEpoch:   func(string) (int64, bool) { return 5, true },
		BarrierExists:  func(string, int64) bool { return false }, // graceful producer never ran
		// OLD stays the active owner so the poll doesn't early-break — it polls
		// the (absent) barrier to the budget, then falls back to legacy Resume.
		ActiveOwnerPID: func(string) (int, bool) { return 424242, true },
		LoadAgent:      func(string) (*agent.Record, error) { return oldCoordRec(), nil }, // OLD still live
		LockAgent: func(string) (func(), error) {
			atomic.AddInt32(&locked, 1)
			return func() {}, nil
		},
		Resume: func(queue.SpawnFresh, string, int, io.Writer, io.Writer) error {
			atomic.AddInt32(&resumed, 1)
			return nil
		},
		BarrierPoll: time.Millisecond,
	}
	// small budget so the no-barrier poll converges fast before the fallback.
	if err := drainOneLeaseAwareWith(leaseDrainReq(), existingQueuePath(t), 0, 10, out, out, d); err != nil {
		t.Fatalf("fallback resume returned %v, want nil", err)
	}
	if atomic.LoadInt32(&resumed) != 1 {
		t.Errorf("legacy Resume fallback ran %d times, want 1 (queue must not be stranded)", resumed)
	}
	if atomic.LoadInt32(&locked) != 1 {
		t.Errorf("fallback Resume took the per-agent lock %d times, want 1", locked)
	}
}

// codex PR3 iter-12 [P2]: live leader + no barrier yet, but a GracefulHandoff
// producer writes the barrier mid-poll — the drain must finish the GRACEFUL
// path (wait for OLD's self-release), NOT spawn a second successor via legacy
// Resume.
func TestDrainLease_LiveLeaderBarrierAppearsMidPoll_NoDuplicateResume(t *testing.T) {
	var resumed, barrierReads, delivered int32
	out := &bytes.Buffer{}
	d := drainLeaseDeps{
		LeaderPresent: func(string) bool { return true },
		CurrentEpoch:  func(string) (int64, bool) { return 5, true },
		BarrierExists: func(string, int64) bool {
			// Barrier appears on the 2nd poll (the producer just wrote it).
			return atomic.AddInt32(&barrierReads, 1) >= 2
		},
		// After the barrier, a SUCCESSOR (different pid) acquires the lease so the
		// graceful path completes via the confirmed-successor branch (codex PR3
		// iter-14 [P1]: completion needs a confirmed successor, not !ok).
		ActiveOwnerPID: func(string) (int, bool) { return 555555, true },
		LoadAgent:      func(string) (*agent.Record, error) { return oldCoordRec(), nil },
		KillCoord:      func(coord.KillTarget) error { return nil }, // OLD released; reap its lingering supervisor
		Resume: func(queue.SpawnFresh, string, int, io.Writer, io.Writer) error {
			atomic.AddInt32(&resumed, 1)
			return nil
		},
		LockAgent: func(string) (func(), error) { return func() {}, nil },
		DeliverPending: func(queue.SpawnFresh, *agent.Record, io.Writer, io.Writer) error {
			atomic.AddInt32(&delivered, 1)
			return nil
		},
		BarrierPoll: time.Millisecond,
	}
	if err := drainOneLeaseAwareWith(leaseDrainReq(), existingQueuePath(t), 0, 1000, out, out, d); err != nil {
		t.Fatalf("graceful-mid-poll path returned %v, want nil", err)
	}
	if atomic.LoadInt32(&resumed) != 0 {
		t.Errorf("legacy Resume ran %d times — barrier appeared, should have finished graceful path (no duplicate)", resumed)
	}
	if atomic.LoadInt32(&delivered) != 1 {
		t.Errorf("pending doc delivered %d times before queue clean, want 1 (codex PR3-completion iter-2 [P1])", delivered)
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
		DeliverPending: func(queue.SpawnFresh, *agent.Record, io.Writer, io.Writer) error { return nil },
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

// codex PR3 iter-14 [P1]: the live-leader fallback must gate "successor already
// leads" on a HEALTHY leader, not a raw active-epoch owner. A standby that wrote
// its `active` epoch (pid != OLD) then crashed (LeaderPresent=false) must NOT be
// treated as a completed handoff — deleting the queue would strand the project.
// The drain must instead complete via the legacy Resume fallback.
func TestDrainLease_LiveLeaderFallback_CrashedSuccessorNotCompletion(t *testing.T) {
	var resumed int32
	out := &bytes.Buffer{}
	// LeaderPresent gates the top-level switch (call #1 -> true, routes to the
	// live-leader fallback) and then the fallback's inner healthySuccessorPresent
	// check (call #2 -> false, the "successor" wrote its epoch then crashed). The
	// poll loop in between calls ActiveOwnerPID, not LeaderPresent, so exactly two
	// LeaderPresent reads occur.
	var leaderReads int32
	d := drainLeaseDeps{
		DeliverPending: func(queue.SpawnFresh, *agent.Record, io.Writer, io.Writer) error { return nil },
		LeaderPresent:  func(string) bool { return atomic.AddInt32(&leaderReads, 1) == 1 },
		CurrentEpoch:   func(string) (int64, bool) { return 6, true },
		BarrierExists:  func(string, int64) bool { return false },
		ActiveOwnerPID: func(string) (int, bool) { return 777777, true }, // non-OLD pid, but dead
		LoadAgent:      func(string) (*agent.Record, error) { return oldCoordRec(), nil },
		LockAgent:      func(string) (func(), error) { return func() {}, nil },
		Resume: func(queue.SpawnFresh, string, int, io.Writer, io.Writer) error {
			atomic.AddInt32(&resumed, 1)
			return nil
		},
		BarrierPoll: time.Millisecond,
	}
	if err := drainOneLeaseAwareWith(leaseDrainReq(), existingQueuePath(t), 0, 5, out, out, d); err != nil {
		t.Fatalf("crashed-successor fallback returned %v, want nil (legacy resume completes it)", err)
	}
	if atomic.LoadInt32(&resumed) != 1 {
		t.Errorf("legacy Resume ran %d times, want 1 — a crashed active-epoch owner is NOT a completed handoff", resumed)
	}
}

// T25: no barrier present and the leader is unresponsive (deadline passes) —
// drain does NOT graceful-kill OLD before the barrier; it escalates instead.
// Combined with T41 here: the escalation calls TakeOver.
func TestDrainLease_NoBarrierNoGracefulKill_Escalates(t *testing.T) {
	var killed, tookOver, recovered int32
	var recoveredFromOld *agent.Record
	var recoveredID string
	out := &bytes.Buffer{}
	d := drainLeaseDeps{
		DeliverPending: func(queue.SpawnFresh, *agent.Record, io.Writer, io.Writer) error { return nil },
		LeaderPresent:  func(string) bool { return false }, // hung/stealable
		CurrentEpoch:   func(string) (int64, bool) { return 5, true },
		BarrierExists:  func(string, int64) bool { return false }, // never
		LoadAgent:      func(string) (*agent.Record, error) { return oldCoordRec(), nil },
		KillCoord:      func(coord.KillTarget) error { atomic.AddInt32(&killed, 1); return nil },
		TakeOver: func(string, string) (bool, error) {
			atomic.AddInt32(&tookOver, 1)
			return true, nil
		},
		RecoverSpawn: func(oldRec *agent.Record, _, preAllocatedID string, _ bool, _, _ io.Writer) error {
			atomic.AddInt32(&recovered, 1)
			recoveredFromOld = oldRec
			recoveredID = preAllocatedID
			return nil
		},
		LockAgent:   func(string) (func(), error) { return func() {}, nil },
		BarrierPoll: time.Millisecond,
	}
	// tiny budget so the bounded wait returns fast (not a timing assertion —
	// we assert it RETURNS + escalates, not how long it took).
	req := leaseDrainReq()
	req.NewAgentID = "preallocated-succ" // codex PR3 iter-10 [P2]
	err := drainOneLeaseAwareWith(req, existingQueuePath(t), 0, 5, out, out, d)
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
	// codex PR3 iter-10 [P2]: the queued successor id must be threaded into the
	// recovery spawn so journal/doc/RC correlation holds.
	if recoveredID != "preallocated-succ" {
		t.Errorf("RecoverSpawn preAllocatedID = %q, want the queued successor id", recoveredID)
	}
}

// codex PR3 iter-11 [P1]: takeoverAndRecover serializes on the per-agent lock
// and re-checks the queue file under it — if a concurrent drain already
// recovered (queue gone), this drain stands down WITHOUT a second
// takeover/RecoverSpawn (no duplicate successor).
func TestDrainLease_ConcurrentRecovery_NoDuplicate(t *testing.T) {
	out := &bytes.Buffer{}
	var tookOver, recovered int32
	// The queue file is ALREADY gone (a peer drain deleted it after recovering).
	gonePath := filepath.Join(t.TempDir(), "already-deleted.json")
	d := drainLeaseDeps{
		DeliverPending: func(queue.SpawnFresh, *agent.Record, io.Writer, io.Writer) error { return nil },
		LeaderPresent:  func(string) bool { return false },
		CurrentEpoch:   func(string) (int64, bool) { return 5, true },
		BarrierExists:  func(string, int64) bool { return false },
		LoadAgent:      func(string) (*agent.Record, error) { return oldCoordRec(), nil },
		TakeOver:       func(string, string) (bool, error) { atomic.AddInt32(&tookOver, 1); return true, nil },
		RecoverSpawn: func(*agent.Record, string, string, bool, io.Writer, io.Writer) error {
			atomic.AddInt32(&recovered, 1)
			return nil
		},
		LockAgent:   func(string) (func(), error) { return func() {}, nil },
		BarrierPoll: time.Millisecond,
	}
	if err := drainOneLeaseAwareWith(leaseDrainReq(), gonePath, 0, 5, out, out, d); err != nil {
		t.Fatalf("concurrent-recovery stand-down returned %v, want nil", err)
	}
	if atomic.LoadInt32(&tookOver) != 0 {
		t.Errorf("TakeOver ran %d times though a peer already recovered, want 0", tookOver)
	}
	if atomic.LoadInt32(&recovered) != 0 {
		t.Errorf("RecoverSpawn ran %d times though a peer already recovered, want 0 (no duplicate)", recovered)
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
		DeliverPending: func(queue.SpawnFresh, *agent.Record, io.Writer, io.Writer) error { return nil },
		LeaderPresent:  func(string) bool { return false },
		CurrentEpoch:   func(string) (int64, bool) { return 9, true },
		BarrierExists:  func(string, int64) bool { return false },
		LoadAgent:      func(string) (*agent.Record, error) { return oldCoordRec(), nil },
		TakeOver: func(project, _ string) (bool, error) {
			tookOverProject = project
			return true, nil // takeover acquired -> OLD confirmed gone
		},
		RecoverSpawn: func(*agent.Record, string, string, bool, io.Writer, io.Writer) error {
			atomic.AddInt32(&recovered, 1)
			return nil
		},
		LockAgent:   func(string) (func(), error) { return func() {}, nil },
		BarrierPoll: time.Millisecond,
	}
	qp := existingQueuePath(t)
	go func() {
		done <- drainOneLeaseAwareWith(leaseDrainReq(), qp, 0, 5, out, out, d)
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

// codex PR3 iter-14 [P2]: the queue's DisableAutoResume override must reach
// RecoverSpawn on the takeover-recovery path (so `fleet handoff --no-auto-resume`
// is honored on an escalated drain, not silently overridden by OLD's baseline).
func TestDrainLease_TakeoverRecovery_HonorsQueueAutoResumeOverride(t *testing.T) {
	out := &bytes.Buffer{}
	var gotDisable bool
	var recovered int32
	disable := true // queue override: --no-auto-resume
	req := leaseDrainReq()
	req.DisableAutoResume = &disable
	d := drainLeaseDeps{
		DeliverPending: func(queue.SpawnFresh, *agent.Record, io.Writer, io.Writer) error { return nil },
		LeaderPresent:  func(string) bool { return false },
		CurrentEpoch:   func(string) (int64, bool) { return 9, true },
		BarrierExists:  func(string, int64) bool { return false },
		LoadAgent: func(string) (*agent.Record, error) {
			r := oldCoordRec()
			r.DisableAutoResume = false // baseline DIFFERS from the override
			return r, nil
		},
		TakeOver: func(string, string) (bool, error) { return true, nil },
		RecoverSpawn: func(_ *agent.Record, _, _ string, disableAutoResume bool, _, _ io.Writer) error {
			gotDisable = disableAutoResume
			atomic.AddInt32(&recovered, 1)
			return nil
		},
		LockAgent:   func(string) (func(), error) { return func() {}, nil },
		BarrierPoll: time.Millisecond,
	}
	if err := drainOneLeaseAwareWith(req, existingQueuePath(t), 0, 5, out, out, d); !errors.Is(err, ErrEscalatedToTakeOver) {
		t.Fatalf("expected escalation, got %v", err)
	}
	if atomic.LoadInt32(&recovered) != 1 {
		t.Fatalf("RecoverSpawn ran %d times, want 1", recovered)
	}
	if !gotDisable {
		t.Error("RecoverSpawn received disableAutoResume=false, want true (queue --no-auto-resume override ignored)")
	}
}

// codex PR4 [P1]: draining a BARE (non-lease-wrapped, SupervisorPID==0)
// coord whose stale epoch belongs to an OLDER released generation must route
// through the legacy Resume (kills OLD by id), NOT the safety-net takeover +
// RecoverSpawn (which would acquire the orphan flock and spawn a SECOND
// coordinator while the live bare OLD keeps running).
func TestDrainLease_BareCoordRoutesToLegacyResume_NoDuplicate(t *testing.T) {
	var resumed, recovered, tookOver int32
	out := &bytes.Buffer{}
	bareRec := agent.New("barecoord")
	bareRec.Project = "projects-fleet"
	bareRec.TaskID = "coord"
	// SupervisorPID intentionally 0 -> bare coord (never coord-run-wrapped).
	d := drainLeaseDeps{
		DeliverPending: func(queue.SpawnFresh, *agent.Record, io.Writer, io.Writer) error { return nil },
		LeaderPresent:  func(string) bool { return false }, // released epoch, no healthy leader
		CurrentEpoch:   func(string) (int64, bool) { return 9, true },
		BarrierExists:  func(string, int64) bool { return false },
		LoadAgent:      func(string) (*agent.Record, error) { return bareRec, nil },
		LockAgent:      func(string) (func(), error) { return func() {}, nil },
		Resume: func(queue.SpawnFresh, string, int, io.Writer, io.Writer) error {
			atomic.AddInt32(&resumed, 1)
			return nil
		},
		TakeOver: func(string, string) (bool, error) {
			atomic.AddInt32(&tookOver, 1)
			return true, nil
		},
		RecoverSpawn: func(*agent.Record, string, string, bool, io.Writer, io.Writer) error {
			atomic.AddInt32(&recovered, 1)
			return nil
		},
		BarrierPoll: time.Millisecond,
	}
	if err := drainOneLeaseAwareWith(leaseDrainReq(), existingQueuePath(t), 0, 10, out, out, d); err != nil {
		t.Fatalf("bare-coord drain returned %v, want nil (legacy resume)", err)
	}
	if atomic.LoadInt32(&resumed) != 1 {
		t.Errorf("legacy Resume ran %d times, want 1 (bare coord must drain via Resume)", resumed)
	}
	if atomic.LoadInt32(&tookOver) != 0 {
		t.Errorf("TakeOver ran %d times, want 0 (no takeover for a bare coord)", tookOver)
	}
	if atomic.LoadInt32(&recovered) != 0 {
		t.Errorf("RecoverSpawn ran %d times, want 0 (no duplicate successor)", recovered)
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
		// A SUCCESSOR (different pid) already owns the lease -> OLD released;
		// reap its lingering supervisor directly (not the self-release wait).
		ActiveOwnerPID: func(string) (int, bool) { return 555555, true },
		LoadAgent: func(string) (*agent.Record, error) {
			return oldCoordRec(), nil
		},
		KillCoord: func(kt coord.KillTarget) error {
			target = kt
			return refusal
		},
		DeliverPending: func(queue.SpawnFresh, *agent.Record, io.Writer, io.Writer) error { return nil },
		BarrierPoll:    time.Millisecond,
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
	var killed, delivered int32
	// OLD archived AND a successor holds the lease -> genuinely complete: clean
	// the queue, no kill. (codex PR3 iter-14 [P1]: a confirmed successor, not
	// merely OLD's archive, is what proves completion.) The still-pending doc
	// is delivered to the successor BEFORE the queue is cleaned (codex
	// PR3-completion iter-2 [P1]); OLD is archived so the policy record is nil.
	d := drainLeaseDeps{
		LeaderPresent:  func(string) bool { return true },
		CurrentEpoch:   func(string) (int64, bool) { return 5, true },
		BarrierExists:  func(string, int64) bool { return true },
		ActiveOwnerPID: func(string) (int, bool) { return 555555, true }, // successor leads
		LoadAgent:      func(string) (*agent.Record, error) { return nil, state.ErrNotFound },
		KillCoord:      func(coord.KillTarget) error { atomic.AddInt32(&killed, 1); return nil },
		DeliverPending: func(req queue.SpawnFresh, oldRec *agent.Record, _, _ io.Writer) error {
			atomic.AddInt32(&delivered, 1)
			if oldRec != nil {
				t.Errorf("delivery got oldRec %+v, want nil (OLD archived)", oldRec)
			}
			if req.HandoffDoc != leaseDrainReq().HandoffDoc {
				t.Errorf("delivery doc = %q, want %q", req.HandoffDoc, leaseDrainReq().HandoffDoc)
			}
			return nil
		},
		BarrierPoll: time.Millisecond,
	}
	qp := existingQueuePath(t)
	if err := drainOneLeaseAwareWith(leaseDrainReq(), qp, 0, 1000, out, out, d); err != nil {
		t.Fatalf("archived OLD + successor present should complete cleanly, got %v", err)
	}
	if atomic.LoadInt32(&killed) != 0 {
		t.Errorf("kill ran %d times though OLD was already archived", killed)
	}
	if atomic.LoadInt32(&delivered) != 1 {
		t.Errorf("pending doc delivered %d times before queue clean, want 1", delivered)
	}
	if _, statErr := os.Stat(qp); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("queue must be deleted when archived OLD has a confirmed successor")
	}
}

// codex PR3 iter-14 [P1]: OLD is archived (coord-run released the lease then
// archived OLD) but NO successor has acquired the freed lease yet (standby
// crashed / still mid-poll). Deleting the queue here would strand the project
// coordless with the only recovery request gone. The drain must NOT delete it —
// it preserves the queue + surfaces an error so a later pass (or the standby
// finally acquiring) completes the handoff.
func TestDrainLease_GracefulOldArchivedNoSuccessor_PreservesQueue(t *testing.T) {
	out := &bytes.Buffer{}
	var killed int32
	d := drainLeaseDeps{
		DeliverPending: func(queue.SpawnFresh, *agent.Record, io.Writer, io.Writer) error { return nil },
		LeaderPresent:  func(string) bool { return true },
		CurrentEpoch:   func(string) (int64, bool) { return 5, true },
		BarrierExists:  func(string, int64) bool { return true },
		ActiveOwnerPID: func(string) (int, bool) { return 0, false }, // no successor
		LoadAgent:      func(string) (*agent.Record, error) { return nil, state.ErrNotFound },
		KillCoord:      func(coord.KillTarget) error { atomic.AddInt32(&killed, 1); return nil },
		BarrierPoll:    time.Millisecond,
	}
	qp := existingQueuePath(t)
	err := drainOneLeaseAwareWith(leaseDrainReq(), qp, 0, 5, out, out, d)
	if err == nil {
		t.Fatal("no-successor archived case must surface an error, not declare done")
	}
	if atomic.LoadInt32(&killed) != 0 {
		t.Errorf("kill ran %d times though OLD was already archived", killed)
	}
	if _, statErr := os.Stat(qp); errors.Is(statErr, os.ErrNotExist) {
		t.Error("queue must be PRESERVED when archived OLD has no successor (deleting it strands the project coordless)")
	}
}

// codex PR3 iter-1 [P1]: barrier present + OLD still the ACTIVE lease owner
// -> drain does NOT force-kill the active leader (the epoch gate would
// refuse); it WAITS for OLD's self-release. When OLD releases, the handoff is
// complete and the queue is cleaned — no kill, no escalation.
func TestDrainLease_GracefulWaitsForSelfRelease(t *testing.T) {
	out := &bytes.Buffer{}
	var killed, tookOver, delivered int32
	// OLD is the active owner for the first two reads, then a SUCCESSOR (a
	// DIFFERENT pid) acquires the freed flock. Completion requires a confirmed
	// successor owner, not merely OLD's absence (codex PR3 iter-14 [P1]).
	const successorPid = 555555
	var reads int32
	d := drainLeaseDeps{
		CurrentEpoch:  func(string) (int64, bool) { return 5, true },
		BarrierExists: func(string, int64) bool { return true },
		LeaderPresent: func(string) bool { return true }, // successor heartbeats (healthy)
		ActiveOwnerPID: func(string) (int, bool) {
			if atomic.AddInt32(&reads, 1) <= 2 {
				return 424242, true // OLD still leader
			}
			return successorPid, true // standby acquired the freed lease
		},
		LoadAgent: func(string) (*agent.Record, error) { return oldCoordRec(), nil },
		KillCoord: func(coord.KillTarget) error { atomic.AddInt32(&killed, 1); return nil },
		TakeOver:  func(string, string) (bool, error) { atomic.AddInt32(&tookOver, 1); return true, nil },
		DeliverPending: func(queue.SpawnFresh, *agent.Record, io.Writer, io.Writer) error {
			atomic.AddInt32(&delivered, 1)
			return nil
		},
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
		t.Errorf("escalated to takeover %d times — successor acquired cleanly, no escalation needed", tookOver)
	}
	if atomic.LoadInt32(&delivered) != 1 {
		t.Errorf("pending doc delivered %d times before queue clean, want 1 (codex PR3-completion iter-2 [P1])", delivered)
	}
	if !strings.Contains(out.String(), "successor pid 555555 acquired") {
		t.Errorf("expected confirmed-successor completion message, got: %q", out.String())
	}
}

// codex PR3 iter-14 [P1]: OLD releases the lease but NO successor ever acquires
// (the standby crashed / never came up) — ActiveOwnerPID returns !ok. The drain
// must NOT treat that as completion (deleting the queue would strand the project
// coordless); it waits to the budget then ESCALATES to takeover+recover so a
// successor is brought up.
func TestDrainLease_GracefulOldReleasedNoSuccessor_Escalates(t *testing.T) {
	out := &bytes.Buffer{}
	var killed, tookOver, recovered int32
	var reads int32
	d := drainLeaseDeps{
		DeliverPending: func(queue.SpawnFresh, *agent.Record, io.Writer, io.Writer) error { return nil },
		CurrentEpoch:   func(string) (int64, bool) { return 5, true },
		BarrierExists:  func(string, int64) bool { return true },
		ActiveOwnerPID: func(string) (int, bool) {
			if atomic.AddInt32(&reads, 1) == 1 {
				return 424242, true // OLD still leader -> enters the self-release wait
			}
			return 0, false // OLD released but NO successor acquired
		},
		LoadAgent: func(string) (*agent.Record, error) { return oldCoordRec(), nil },
		KillCoord: func(coord.KillTarget) error { atomic.AddInt32(&killed, 1); return nil },
		TakeOver:  func(string, string) (bool, error) { atomic.AddInt32(&tookOver, 1); return true, nil },
		RecoverSpawn: func(*agent.Record, string, string, bool, io.Writer, io.Writer) error {
			atomic.AddInt32(&recovered, 1)
			return nil
		},
		LockAgent:   func(string) (func(), error) { return func() {}, nil },
		BarrierPoll: time.Millisecond,
	}
	qp := existingQueuePath(t)
	err := drainOneLeaseAwareWith(leaseDrainReq(), qp, 0, 5, out, out, d)
	if !errors.Is(err, ErrEscalatedToTakeOver) {
		t.Fatalf("no-successor self-release must escalate; got %v", err)
	}
	if atomic.LoadInt32(&tookOver) != 1 {
		t.Errorf("TakeOver ran %d times, want 1 (no successor -> must escalate, not declare done)", tookOver)
	}
	if atomic.LoadInt32(&recovered) != 1 {
		t.Errorf("RecoverSpawn ran %d times, want 1 (project must not be left coordless)", recovered)
	}
}

// codex PR3 iter-14 [P1]: a successor that CRASHED after writing its `active`
// epoch but before releasing still shows in ActiveOwnerPID (pid != OLD) — but it
// is NOT a live leader (LeaderPresent=false). The drain must NOT treat that
// stale active owner as a confirmed successor and delete the queue; it must wait
// then escalate to takeover+recover so a LIVE successor is brought up.
func TestDrainLease_GracefulSuccessorCrashedAfterEpoch_Escalates(t *testing.T) {
	out := &bytes.Buffer{}
	var tookOver, recovered int32
	var reads int32
	d := drainLeaseDeps{
		DeliverPending: func(queue.SpawnFresh, *agent.Record, io.Writer, io.Writer) error { return nil },
		CurrentEpoch:   func(string) (int64, bool) { return 5, true },
		BarrierExists:  func(string, int64) bool { return true },
		// OLD active on first read (enters self-release wait); then a DIFFERENT
		// pid owns the active epoch — but it crashed, so LeaderPresent=false.
		ActiveOwnerPID: func(string) (int, bool) {
			if atomic.AddInt32(&reads, 1) == 1 {
				return 424242, true // OLD still leader
			}
			return 777777, true // a successor wrote its epoch then crashed
		},
		LeaderPresent: func(string) bool { return false }, // no HEALTHY leader heartbeating
		LoadAgent:     func(string) (*agent.Record, error) { return oldCoordRec(), nil },
		TakeOver:      func(string, string) (bool, error) { atomic.AddInt32(&tookOver, 1); return true, nil },
		RecoverSpawn: func(*agent.Record, string, string, bool, io.Writer, io.Writer) error {
			atomic.AddInt32(&recovered, 1)
			return nil
		},
		LockAgent:   func(string) (func(), error) { return func() {}, nil },
		BarrierPoll: time.Millisecond,
	}
	qp := existingQueuePath(t)
	err := drainOneLeaseAwareWith(leaseDrainReq(), qp, 0, 5, out, out, d)
	if !errors.Is(err, ErrEscalatedToTakeOver) {
		t.Fatalf("crashed-successor must escalate (not declare done off a stale active epoch); got %v", err)
	}
	if atomic.LoadInt32(&tookOver) != 1 {
		t.Errorf("TakeOver ran %d times, want 1", tookOver)
	}
	if atomic.LoadInt32(&recovered) != 1 {
		t.Errorf("RecoverSpawn ran %d times, want 1 (a dead active-epoch owner is not a successor)", recovered)
	}
}

// codex PR3 iter-1 [P1]: barrier present + OLD STILL the active owner past the
// budget -> escalate to takeover (OLD wedged after writing the barrier).
func TestDrainLease_GracefulOldHoldsLeasePastBudget_Escalates(t *testing.T) {
	out := &bytes.Buffer{}
	var tookOver int32
	var recovered int32
	d := drainLeaseDeps{
		DeliverPending: func(queue.SpawnFresh, *agent.Record, io.Writer, io.Writer) error { return nil },
		CurrentEpoch:   func(string) (int64, bool) { return 5, true },
		BarrierExists:  func(string, int64) bool { return true },
		// OLD holds the lease past the budget; after TakeOver fences+kills it the
		// lease is free with no successor (the post-takeover re-check sees !ok),
		// so RecoverSpawn brings up a replacement.
		ActiveOwnerPID: func(string) (int, bool) {
			if atomic.LoadInt32(&tookOver) == 0 {
				return 424242, true // OLD still leads until takeover reaps it
			}
			return 0, false // OLD killed; no successor yet -> RecoverSpawn
		},
		LoadAgent: func(string) (*agent.Record, error) { return oldCoordRec(), nil },
		TakeOver:  func(string, string) (bool, error) { atomic.AddInt32(&tookOver, 1); return true, nil },
		RecoverSpawn: func(*agent.Record, string, string, bool, io.Writer, io.Writer) error {
			atomic.AddInt32(&recovered, 1)
			return nil
		},
		LockAgent:   func(string) (func(), error) { return func() {}, nil },
		BarrierPoll: time.Millisecond,
	}
	err := drainOneLeaseAwareWith(leaseDrainReq(), existingQueuePath(t), 0, 5, out, out, d)
	if !errors.Is(err, ErrEscalatedToTakeOver) {
		t.Fatalf("expected escalation when OLD holds the lease past budget, got %v", err)
	}
	if atomic.LoadInt32(&tookOver) != 1 {
		t.Errorf("TakeOver ran %d times, want 1", tookOver)
	}
	if atomic.LoadInt32(&recovered) != 1 {
		t.Errorf("RecoverSpawn ran %d times after takeover, want 1", recovered)
	}
}

// codex PR3 iter-9 [P1]: a takeover that does NOT acquire (acquired=false:
// healthy OLD made AcquireLeaseWithKill stand down, or an un-killable hung OLD)
// must NOT spawn a successor (would duplicate / shoot-over). The escalation
// surfaces an error and leaves the queue.
func TestDrainLease_TakeoverNotAcquired_NoRecoverSpawn(t *testing.T) {
	out := &bytes.Buffer{}
	var recovered int32
	d := drainLeaseDeps{
		DeliverPending: func(queue.SpawnFresh, *agent.Record, io.Writer, io.Writer) error { return nil },
		LeaderPresent:  func(string) bool { return false },
		CurrentEpoch:   func(string) (int64, bool) { return 5, true },
		BarrierExists:  func(string, int64) bool { return false },
		LoadAgent:      func(string) (*agent.Record, error) { return oldCoordRec(), nil },
		TakeOver:       func(string, string) (bool, error) { return false, nil }, // did NOT acquire
		RecoverSpawn: func(*agent.Record, string, string, bool, io.Writer, io.Writer) error {
			atomic.AddInt32(&recovered, 1)
			return nil
		},
		LockAgent:   func(string) (func(), error) { return func() {}, nil },
		BarrierPoll: time.Millisecond,
	}
	err := drainOneLeaseAwareWith(leaseDrainReq(), existingQueuePath(t), 0, 5, out, out, d)
	if err == nil || errors.Is(err, ErrEscalatedToTakeOver) {
		t.Fatalf("expected a 'did not confirm gone' error, got %v", err)
	}
	if atomic.LoadInt32(&recovered) != 0 {
		t.Errorf("RecoverSpawn ran %d times despite takeover not acquiring — would duplicate", recovered)
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
		DeliverPending: func(queue.SpawnFresh, *agent.Record, io.Writer, io.Writer) error { return nil },
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

// coldResumeDeps returns deps pinned so drainOneLeaseAwareWith routes to
// coldResume DETERMINISTICALLY — no lease epoch (stealable), no live leader,
// OLD not on disk — regardless of the host's REAL ~/.fleet lease state. A
// developer machine often runs a live projects-fleet coord whose heartbeat
// would flip the default LeaderPresent and reroute these tests through the
// live-leader fallback (flaky stand-down nils). Tests override Resume (and
// LockAgent when they observe it).
func coldResumeDeps() drainLeaseDeps {
	return drainLeaseDeps{
		CurrentEpoch:  func(string) (int64, bool) { return 0, false },
		LeaderPresent: func(string) bool { return false },
		LoadAgent:     func(string) (*agent.Record, error) { return nil, state.ErrNotFound },
		LockAgent:     func(string) (func(), error) { return func() {}, nil },
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
	d := coldResumeDeps()
	d.LockAgent = func(string) (func(), error) {
		atomic.AddInt32(&locked, 1)
		return func() { atomic.AddInt32(&released, 1) }, nil
	}
	d.Resume = func(queue.SpawnFresh, string, int, io.Writer, io.Writer) error {
		// The lock must be held while Resume runs (serialization).
		lockedDuringResume = atomic.LoadInt32(&locked) == 1 && atomic.LoadInt32(&released) == 0
		return nil
	}
	// resumeTimeoutMillis=0 -> coldResume runs synchronously (no timeout), so the
	// lock-held-then-released ordering is deterministic for this contract test.
	if err := drainOneLeaseAwareWith(leaseDrainReq(), "/tmp/q.json", 0, 0, out, out, d); err != nil {
		t.Fatalf("cold resume returned %v, want nil", err)
	}
	if !lockedDuringResume {
		t.Error("cold Resume ran WITHOUT the per-agent lock held — serialization contract broken")
	}
	if atomic.LoadInt32(&released) != 1 {
		t.Errorf("per-agent lock released %d times, want 1 (must always release)", released)
	}
}

// codex PR3 iter-14 [P1]: coldResume HONORS resumeTimeoutMillis — a Resume that
// hangs past the budget must RETURN (surface), not block fleet drain forever
// (the drain-storm class). Deterministic: a Resume that blocks on a channel the
// test never closes, asserted to RETURN at ~budget, not a wall-clock value.
func TestDrainLease_ColdResume_HonorsResumeTimeout(t *testing.T) {
	out := &bytes.Buffer{}
	block := make(chan struct{})
	t.Cleanup(func() { close(block) }) // unblock the abandoned goroutine at test end
	d := coldResumeDeps()
	d.Resume = func(queue.SpawnFresh, string, int, io.Writer, io.Writer) error {
		<-block // hang past the budget
		return nil
	}
	done := make(chan error, 1)
	go func() {
		// 20ms budget; Resume never returns until t.Cleanup -> must time out.
		done <- drainOneLeaseAwareWith(leaseDrainReq(), "/tmp/q.json", 0, 20, out, out, d)
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("coldResume returned nil though Resume hung past the budget; want a timeout error")
		}
		if !strings.Contains(err.Error(), "resume-timeout") {
			t.Errorf("expected a resume-timeout error, got %v", err)
		}
		// DESIGN-handoff-lifecycle-hardening bug A: the timeout is BACKGROUNDED,
		// not failed — it must carry the typed sentinel so runDrain counts it
		// backgrounded (codex iter-1 [P1]: not processed — the handoff may
		// still be pending) instead of reporting "every pending handoff
		// failed".
		if !errors.Is(err, ErrResumeBackgrounded) {
			t.Errorf("timeout must wrap ErrResumeBackgrounded, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("coldResume BLOCKED past the budget — resume-timeout not honored (drain-storm regression)")
	}
}

// Bug A, test 1: a Resume that completes WITHIN the budget behaves exactly as
// today — nil error (counted processed, the only completion signal), no
// backgrounding.
func TestDrainLease_ColdResume_FastResumeWithinBudget(t *testing.T) {
	out := &bytes.Buffer{}
	var resumed int32
	d := coldResumeDeps()
	d.Resume = func(queue.SpawnFresh, string, int, io.Writer, io.Writer) error {
		atomic.AddInt32(&resumed, 1)
		return nil
	}
	// Generous budget; Resume returns immediately — must come back nil.
	if err := drainOneLeaseAwareWith(leaseDrainReq(), "/tmp/q.json", 0, 60000, out, out, d); err != nil {
		t.Fatalf("fast resume within budget must return nil, got %v", err)
	}
	if atomic.LoadInt32(&resumed) != 1 {
		t.Errorf("Resume ran %d times, want 1", resumed)
	}
}

// Bug A, test 5 (coldResume seam): a GENUINE Resume error returned before the
// budget propagates as-is — never the backgrounded sentinel.
func TestDrainLease_ColdResume_GenuineErrorNotBackgrounded(t *testing.T) {
	out := &bytes.Buffer{}
	boom := errors.New("spawn replacement: boom")
	d := coldResumeDeps()
	d.Resume = func(queue.SpawnFresh, string, int, io.Writer, io.Writer) error {
		return boom
	}
	err := drainOneLeaseAwareWith(leaseDrainReq(), "/tmp/q.json", 0, 60000, out, out, d)
	if !errors.Is(err, boom) {
		t.Fatalf("genuine resume error must propagate, got %v", err)
	}
	if errors.Is(err, ErrResumeBackgrounded) {
		t.Error("genuine resume error wrongly classified as backgrounded — masks real failures")
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
		LeaderPresent: func(string) bool { return true }, // successor heartbeats (healthy)
		ActiveOwnerPID: func(string) (int, bool) {
			// First read: OLD still leader (enters the self-release wait, during
			// which we probe the sibling lock). Then a SUCCESSOR (different pid)
			// acquires the freed lease -> graceful completion, lock-free (codex
			// PR3 iter-14 [P1]: completion needs a confirmed healthy successor).
			if atomic.AddInt32(&reads, 1) == 1 {
				close(probed)
				return 424242, true
			}
			return 555555, true
		},
		LoadAgent:      func(string) (*agent.Record, error) { return oldCoordRec(), nil },
		DeliverPending: func(queue.SpawnFresh, *agent.Record, io.Writer, io.Writer) error { return nil },
		BarrierPoll:    50 * time.Millisecond,
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

// codex PR3 iter-14 [P1] REGRESSION: takeoverAndRecover must NOT hold the
// per-agent lock across TakeOver. The production TakeOver SIGTERMs the hung OLD,
// whose coord.Cleanup archives OLD's record under state.LockAgent(OLD). If the
// drain held that SAME lock across TakeOver, OLD's cleanup would block -> OLD is
// SIGKILLed with a STALE live record + orphaned tmux/engine (the deadlock class
// this whole stack exists to kill, reintroduced in PR3's takeover path).
//
// This test wires the REAL state.LockAgent as the seam and, INSIDE the TakeOver
// callback (i.e. while the kill is "in flight"), simulates OLD's coord.Cleanup
// archive grabbing state.LockAgent(OLD). With the lock held across TakeOver that
// sibling acquire deadlocks; with the narrowed critical section it acquires
// immediately. Deterministic: a channel proves the sibling lock was granted
// during TakeOver, no wall-clock timing assertion.
func TestDrainLease_TakeoverDoesNotHoldLockAcrossKill(t *testing.T) {
	setupFleetHome(t)

	out := &bytes.Buffer{}
	siblingGotLock := make(chan struct{})
	var recovered int32

	d := drainLeaseDeps{
		DeliverPending: func(queue.SpawnFresh, *agent.Record, io.Writer, io.Writer) error { return nil },
		LeaderPresent:  func(string) bool { return false },
		CurrentEpoch:   func(string) (int64, bool) { return 9, true },
		BarrierExists:  func(string, int64) bool { return false },
		LoadAgent:      func(string) (*agent.Record, error) { return oldCoordRec(), nil },
		// REAL per-agent lock — the same primitive coord.Cleanup's archive uses.
		LockAgent: state.LockAgent,
		TakeOver: func(string, string) (bool, error) {
			// Simulate OLD's coord.Cleanup archive (archiveAgentRecord ->
			// state.LockAgent(OLD)) running while the kill is in flight. If the
			// drain holds the lock across TakeOver this blocks forever.
			rel, err := state.LockAgent("oldcoord1")
			if err != nil {
				return false, err
			}
			rel()
			close(siblingGotLock)
			return true, nil // takeover acquired -> OLD confirmed gone
		},
		RecoverSpawn: func(*agent.Record, string, string, bool, io.Writer, io.Writer) error {
			atomic.AddInt32(&recovered, 1)
			return nil
		},
		BarrierPoll: time.Millisecond,
	}

	qp := existingQueuePath(t)
	done := make(chan error, 1)
	go func() { done <- drainOneLeaseAwareWith(leaseDrainReq(), qp, 0, 5, out, out, d) }()

	select {
	case <-siblingGotLock:
		// OLD's cleanup-equivalent acquired the lock DURING TakeOver — the drain
		// is not holding it across the kill. No deadlock.
	case <-time.After(5 * time.Second):
		t.Fatal("sibling LockAgent(OLD) BLOCKED during TakeOver — drain holds the per-agent lock across the kill (iter-14 deadlock regression)")
	}

	select {
	case err := <-done:
		if !errors.Is(err, ErrEscalatedToTakeOver) {
			t.Fatalf("expected escalation signal, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("takeover recovery did not converge")
	}
	if atomic.LoadInt32(&recovered) != 1 {
		t.Errorf("RecoverSpawn ran %d times, want 1 (successor recovered under the SHORT post-takeover lock)", recovered)
	}
}

// codex PR3 iter-14 [P1] REGRESSION: a takeover-recovered successor must run the
// post-spawn handoff TAIL — write the coord-spawn marker for the NEW agent (so
// the TUI/attach can discover it; the dead OLD's cleanup cleared the old one)
// AND send handoff.ResumePrompt(docPath) (so the fresh coord actually resumes
// from the handoff doc instead of sitting idle). Without it the replacement is
// invisible + inert.
func TestRecoverHandoffTail_WritesMarkerAndSendsPrompt(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	oldRec := oldCoordRec()
	oldRec.TaskID = "coord-projects-fleet" // IsCoordSpawn requires "coord-<project>"

	var gotMarkerProj, gotMarkerID, gotSession, gotPrompt string
	origMarker := recoverWriteMarkerFn
	origReady := recoverWaitForReadyFn
	origAlive := recoverSessionAliveFn
	origPrompt := recoverSendPromptVerifiedFn
	origCurrentOwner := recoverCurrentOwnerFn
	t.Cleanup(func() {
		recoverWriteMarkerFn = origMarker
		recoverWaitForReadyFn = origReady
		recoverSessionAliveFn = origAlive
		recoverSendPromptVerifiedFn = origPrompt
		recoverCurrentOwnerFn = origCurrentOwner
	})
	recoverWriteMarkerFn = func(project, id string) error { gotMarkerProj, gotMarkerID = project, id; return nil }
	recoverWaitForReadyFn = func(string) error { return nil }
	recoverSessionAliveFn = func(string) (bool, error) { return true, nil }
	recoverSendPromptVerifiedFn = func(session, prompt string) (bool, error) {
		gotSession, gotPrompt = session, prompt
		return true, nil
	}

	rec := agent.New("newcoord9")
	rec.Project = "projects-fleet"
	rec.TmuxSession = "fleet-newcoord9"
	if err := rec.Write(); err != nil {
		t.Fatalf("write rec: %v", err)
	}
	recoverCurrentOwnerFn = func(project string) (coordlock.Owner, bool) {
		return coordlock.Owner{AgentID: rec.ID, PID: 4242, PidStart: 99}, true
	}
	if err := recoverHandoffTail(oldRec, rec, "/tmp/handoff.md", false, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("recoverHandoffTail returned %v", err)
	}

	if gotMarkerProj != "projects-fleet" || gotMarkerID != "newcoord9" {
		t.Errorf("coord-spawn marker = (%q,%q), want (projects-fleet,newcoord9) — TUI cannot discover the replacement without it",
			gotMarkerProj, gotMarkerID)
	}
	if gotSession != "fleet-newcoord9" {
		t.Errorf("resume prompt sent to session %q, want fleet-newcoord9", gotSession)
	}
	if !strings.Contains(gotPrompt, "/tmp/handoff.md") {
		t.Errorf("resume prompt %q must reference the handoff doc /tmp/handoff.md (the coord stays idle otherwise)", gotPrompt)
	}
}

// The tail honors DisableAutoResume: no resume prompt is typed (the operator
// drives the first turn), but the marker is STILL written so discovery works.
func TestRecoverHandoffTail_DisableAutoResume_NoPromptStillMarks(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	oldRec := oldCoordRec()
	oldRec.TaskID = "coord-projects-fleet"

	var markerWritten, promptSent bool
	origMarker := recoverWriteMarkerFn
	origReady := recoverWaitForReadyFn
	origAlive := recoverSessionAliveFn
	origPrompt := recoverSendPromptVerifiedFn
	origCurrentOwner := recoverCurrentOwnerFn
	t.Cleanup(func() {
		recoverWriteMarkerFn = origMarker
		recoverWaitForReadyFn = origReady
		recoverSessionAliveFn = origAlive
		recoverSendPromptVerifiedFn = origPrompt
		recoverCurrentOwnerFn = origCurrentOwner
	})
	recoverWriteMarkerFn = func(string, string) error { markerWritten = true; return nil }
	recoverWaitForReadyFn = func(string) error { return nil }
	recoverSessionAliveFn = func(string) (bool, error) { return true, nil }
	recoverSendPromptVerifiedFn = func(string, string) (bool, error) {
		promptSent = true
		return true, nil
	}

	// disableAutoResume=true (the EFFECTIVE policy, e.g. from a queue override).
	rec := agent.New("newcoord9")
	rec.Project = "projects-fleet"
	rec.TmuxSession = "fleet-newcoord9"
	if err := rec.Write(); err != nil {
		t.Fatalf("write rec: %v", err)
	}
	recoverCurrentOwnerFn = func(project string) (coordlock.Owner, bool) {
		return coordlock.Owner{AgentID: rec.ID, PID: 4242, PidStart: 99}, true
	}
	if err := recoverHandoffTail(oldRec, rec, "/tmp/handoff.md", true, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("recoverHandoffTail returned %v", err)
	}

	if !markerWritten {
		t.Error("marker must be written even when auto-resume is disabled (discovery is independent of resume)")
	}
	if promptSent {
		t.Error("resume prompt must NOT be sent when DisableAutoResume is set")
	}
}

// codex iter-15 [P2] REGRESSION: the lock-owner delivery branch of
// deliverRecoverResumePrompt must honor the at-most-once sentinel just like the
// direct path. If takeoverAndRecover's queue.Delete fails after a successful
// send, the adopt-retry re-enters this branch; without the sentinel it
// re-submits the same resume prompt to the owner.
func TestDeliverRecoverResumePrompt_LockOwner_AtMostOnce(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	const project = "projects-fleet"
	pdir, err := state.EnsureProjectInitialized(project)
	if err != nil {
		t.Fatalf("EnsureProjectInitialized: %v", err)
	}
	// The sentinel lives under <project>/.locks/; create it so the marker write
	// lands (matches the lazily-created lock dir in production).
	if err := os.MkdirAll(filepath.Join(pdir, ".locks"), 0o755); err != nil {
		t.Fatalf("mkdir .locks: %v", err)
	}

	rec := agent.New("newcoord9")
	rec.Project = project
	rec.TmuxSession = "fleet-newcoord9"
	if err := rec.Write(); err != nil {
		t.Fatalf("write rec: %v", err)
	}

	var sends int
	origReady := recoverWaitForReadyFn
	origAlive := recoverSessionAliveFn
	origPrompt := recoverSendPromptVerifiedFn
	origCurrentOwner := recoverCurrentOwnerFn
	origMarker := recoverWriteMarkerFn
	t.Cleanup(func() {
		recoverWaitForReadyFn = origReady
		recoverSessionAliveFn = origAlive
		recoverSendPromptVerifiedFn = origPrompt
		recoverCurrentOwnerFn = origCurrentOwner
		recoverWriteMarkerFn = origMarker
	})
	recoverWaitForReadyFn = func(string) error { return nil }
	recoverSessionAliveFn = func(string) (bool, error) { return true, nil }
	recoverWriteMarkerFn = func(string, string) error { return nil }
	recoverCurrentOwnerFn = func(string) (coordlock.Owner, bool) {
		return coordlock.Owner{AgentID: rec.ID, PID: 4242, PidStart: 99}, true
	}
	recoverSendPromptVerifiedFn = func(string, string) (bool, error) {
		sends++
		return true, nil
	}

	// First pass: delivers + writes the sentinel.
	if err := deliverRecoverResumePrompt(rec, "/tmp/handoff.md", false, true, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	// Second pass (adopt-retry after queue.Delete failed): sentinel short-circuits.
	if err := deliverRecoverResumePrompt(rec, "/tmp/handoff.md", false, true, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("second delivery: %v", err)
	}
	if sends != 1 {
		t.Fatalf("lock-owner resume prompt sent %d times, want exactly 1 (at-most-once)", sends)
	}
}

// codex PR3-completion iter-2 [P1]: the barrier proves doc DURABILITY, not
// DELIVERY. A GracefulCoordSwap completion that failed/crashed between
// retiring OLD and injecting the doc leaves the queue as the only durable
// "successor never got its prompt" signal. drainGraceful must deliver the
// pending doc to the confirmed healthy successor BEFORE queue.Delete — and a
// delivery failure must PRESERVE the queue (next pass retries; at-most-once
// sentinel dedupes), never silently consume the only recovery request.
func TestDrainLease_GracefulDeliveryFails_PreservesQueue(t *testing.T) {
	out := &bytes.Buffer{}
	var killed int32
	wantErr := errors.New("owner readiness never converged")
	d := drainLeaseDeps{
		LeaderPresent:  func(string) bool { return true },
		CurrentEpoch:   func(string) (int64, bool) { return 5, true },
		BarrierExists:  func(string, int64) bool { return true },
		ActiveOwnerPID: func(string) (int, bool) { return 555555, true }, // successor leads
		LoadAgent:      func(string) (*agent.Record, error) { return nil, state.ErrNotFound },
		KillCoord:      func(coord.KillTarget) error { atomic.AddInt32(&killed, 1); return nil },
		DeliverPending: func(queue.SpawnFresh, *agent.Record, io.Writer, io.Writer) error {
			return wantErr
		},
		BarrierPoll: time.Millisecond,
	}
	qp := existingQueuePath(t)
	err := drainOneLeaseAwareWith(leaseDrainReq(), qp, 0, 1000, out, out, d)
	if !errors.Is(err, wantErr) {
		t.Fatalf("delivery failure not surfaced (want errors.Is): %v", err)
	}
	if _, statErr := os.Stat(qp); statErr != nil {
		t.Errorf("queue must be PRESERVED on a delivery failure (it is the only durable inbox): %v", statErr)
	}
	if atomic.LoadInt32(&killed) != 0 {
		t.Errorf("kill ran %d times though OLD was already archived", killed)
	}
}

// codex PR3-completion iter-3 [P1]: a GracefulCoordSwap that retired OLD but
// FAILED delivery leaves the queue pending; once the standby acquires, the
// epoch advances so a later drain MISSES OLD's barrier and routes through
// drainLiveLeaderFallback's "OLD archived -> stale queue" stand-down. That
// cleanup must deliver the pending doc to the live leader BEFORE deleting
// the queue (the queue is the only durable inbox), and a delivery failure
// must preserve it.
func TestDrainLease_MissedBarrierStandDown_DeliversBeforeClean(t *testing.T) {
	out := &bytes.Buffer{}
	var delivered int32
	d := drainLeaseDeps{
		LeaderPresent: func(string) bool { return true },
		CurrentEpoch:  func(string) (int64, bool) { return 6, true }, // successor's NEW epoch
		BarrierExists: func(string, int64) bool { return false },     // OLD's epoch-5 barrier missed
		LoadAgent:     func(string) (*agent.Record, error) { return nil, state.ErrNotFound },
		DeliverPending: func(req queue.SpawnFresh, oldRec *agent.Record, _, _ io.Writer) error {
			atomic.AddInt32(&delivered, 1)
			if oldRec != nil {
				t.Errorf("delivery got oldRec %+v, want nil (OLD archived)", oldRec)
			}
			return nil
		},
		BarrierPoll: time.Millisecond,
	}
	qp := existingQueuePath(t)
	if err := drainOneLeaseAwareWith(leaseDrainReq(), qp, 0, 50, out, out, d); err != nil {
		t.Fatalf("missed-barrier stand-down should complete cleanly, got %v", err)
	}
	if atomic.LoadInt32(&delivered) != 1 {
		t.Errorf("pending doc delivered %d times before the stale-queue clean, want 1", delivered)
	}
	if _, statErr := os.Stat(qp); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("queue must be deleted after a successful deliver-then-clean")
	}
}

func TestDrainLease_MissedBarrierStandDown_DeliveryFailurePreservesQueue(t *testing.T) {
	out := &bytes.Buffer{}
	wantErr := errors.New("owner readiness never converged")
	d := drainLeaseDeps{
		LeaderPresent: func(string) bool { return true },
		CurrentEpoch:  func(string) (int64, bool) { return 6, true },
		BarrierExists: func(string, int64) bool { return false },
		LoadAgent:     func(string) (*agent.Record, error) { return nil, state.ErrNotFound },
		DeliverPending: func(queue.SpawnFresh, *agent.Record, io.Writer, io.Writer) error {
			return wantErr
		},
		BarrierPoll: time.Millisecond,
	}
	qp := existingQueuePath(t)
	err := drainOneLeaseAwareWith(leaseDrainReq(), qp, 0, 50, out, out, d)
	if !errors.Is(err, wantErr) {
		t.Fatalf("delivery failure not surfaced (want errors.Is): %v", err)
	}
	if _, statErr := os.Stat(qp); statErr != nil {
		t.Errorf("queue must be PRESERVED on a delivery failure: %v", statErr)
	}
}

// codex PR3-completion iter-4 [P2]: when OLD is already archived, the
// drain delivery paths must recover the inherited DisableAutoResume baseline
// from the ARCHIVE — an opt-out handoff (no queue override) must not get a
// resume prompt typed into a successor that was meant to stay idle.
func TestDrainGracefulDeliverPending_ArchivedOldPolicy_NoPrompt(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	const project = "projects-fleet"
	if _, err := state.EnsureProjectInitialized(project); err != nil {
		t.Fatalf("EnsureProjectInitialized: %v", err)
	}

	// OLD opted out of auto-resume, then was archived (its retire completed).
	old := agent.New("oldcoord1")
	old.Project = project
	old.TaskID = "coord"
	old.DisableAutoResume = true
	if err := old.Write(); err != nil {
		t.Fatalf("write old: %v", err)
	}
	if err := old.Archive(); err != nil {
		t.Fatalf("archive old: %v", err)
	}

	winner := agent.New("winner01")
	winner.Project = project
	winner.TmuxSession = "fleet-winner01"
	if err := winner.Write(); err != nil {
		t.Fatalf("write winner: %v", err)
	}

	var sends int
	origReady := recoverWaitForReadyFn
	origAlive := recoverSessionAliveFn
	origPrompt := recoverSendPromptVerifiedFn
	origCurrentOwner := recoverCurrentOwnerFn
	origMarker := recoverWriteMarkerFn
	t.Cleanup(func() {
		recoverWaitForReadyFn = origReady
		recoverSessionAliveFn = origAlive
		recoverSendPromptVerifiedFn = origPrompt
		recoverCurrentOwnerFn = origCurrentOwner
		recoverWriteMarkerFn = origMarker
	})
	recoverWaitForReadyFn = func(string) error { return nil }
	recoverSessionAliveFn = func(string) (bool, error) { return true, nil }
	recoverWriteMarkerFn = func(string, string) error { return nil }
	recoverCurrentOwnerFn = func(string) (coordlock.Owner, bool) {
		return coordlock.Owner{AgentID: winner.ID, PID: 4242, PidStart: 99}, true
	}
	recoverSendPromptVerifiedFn = func(string, string) (bool, error) {
		sends++
		return true, nil
	}

	req := queue.SpawnFresh{
		SchemaVersion: queue.SchemaVersion,
		OldAgentID:    old.ID,
		Project:       project,
		TaskID:        "coord",
		HandoffDoc:    "/tmp/handoff.md",
	}
	if err := drainGracefulDeliverPending(req, nil, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("delivery with archived opt-out OLD: %v", err)
	}
	if sends != 0 {
		t.Fatalf("resume prompt sent %d times for an opt-out handoff (archived baseline ignored), want 0", sends)
	}

	// Control: an explicit queue override back to auto-resume DOES prompt.
	override := false
	req.DisableAutoResume = &override
	if err := drainGracefulDeliverPending(req, nil, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("delivery with auto-resume override: %v", err)
	}
	if sends != 1 {
		t.Fatalf("resume prompt sent %d times with the auto-resume override, want 1", sends)
	}
}

// codex PR3-completion iter-6 [P2]: v1 (schema_version < 2) queues predate
// auto-resume; Resume/resumeHandoff never type a resume prompt for them. The
// drain delivery path must agree — cleaning a lingering v1 queue must NOT
// send a prompt. The suppression is a TRANSIENT send gate in
// drainGracefulDeliverPending; effectiveDisableAutoResume itself stays a
// pure BASELINE resolver because its value is persisted into a recovered
// successor's record by RecoverSpawn (codex iter-10 [P2]).
func TestDrainGracefulDeliverPending_LegacySchemaNeverPrompts(t *testing.T) {
	// Every tmux-reaching function var is stubbed below, so this test
	// never touches a real server — but isolate the socket anyway
	// (tmuxtest.IsolateSocket, NOT RequireTmux: no skip on tmux-less CI)
	// so a future stub removal fails loudly instead of leaking onto the
	// operator's default tmux server. Also satisfies
	// scripts/lint-test-isolation.sh, whose `Resume(` trigger matches
	// the effectiveDisableAutoResume call at the end of this test.
	tmuxtest.IsolateSocket(t)
	t.Setenv("FLEET_HOME", t.TempDir())
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	const project = "projects-fleet"
	if _, err := state.EnsureProjectInitialized(project); err != nil {
		t.Fatalf("EnsureProjectInitialized: %v", err)
	}
	winner := agent.New("winner02")
	winner.Project = project
	winner.TmuxSession = "fleet-winner02"
	if err := winner.Write(); err != nil {
		t.Fatalf("write winner: %v", err)
	}

	var sends int
	origReady := recoverWaitForReadyFn
	origAlive := recoverSessionAliveFn
	origPrompt := recoverSendPromptVerifiedFn
	origCurrentOwner := recoverCurrentOwnerFn
	origMarker := recoverWriteMarkerFn
	t.Cleanup(func() {
		recoverWaitForReadyFn = origReady
		recoverSessionAliveFn = origAlive
		recoverSendPromptVerifiedFn = origPrompt
		recoverCurrentOwnerFn = origCurrentOwner
		recoverWriteMarkerFn = origMarker
	})
	recoverWaitForReadyFn = func(string) error { return nil }
	recoverSessionAliveFn = func(string) (bool, error) { return true, nil }
	recoverWriteMarkerFn = func(string, string) error { return nil }
	recoverCurrentOwnerFn = func(string) (coordlock.Owner, bool) {
		return coordlock.Owner{AgentID: winner.ID, PID: 4242, PidStart: 99}, true
	}
	recoverSendPromptVerifiedFn = func(string, string) (bool, error) {
		sends++
		return true, nil
	}

	req := leaseDrainReq()
	req.SchemaVersion = 1
	if err := drainGracefulDeliverPending(req, nil, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("v1-queue delivery pass: %v", err)
	}
	if sends != 0 {
		t.Fatalf("resume prompt sent %d times for a v1 queue, want 0 (Resume parity)", sends)
	}
	// Baseline resolver must NOT bake the schema gate in (it is persisted by
	// RecoverSpawn into the successor record — codex iter-10 [P2]).
	if effectiveDisableAutoResume(req, nil) {
		t.Error("effectiveDisableAutoResume must stay a pure baseline resolver (no schema gate)")
	}
}
