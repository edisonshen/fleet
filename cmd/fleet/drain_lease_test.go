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
		BarrierExists: func(string, int64) bool { return false }, // graceful producer never ran
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
	var resumed, barrierReads int32
	out := &bytes.Buffer{}
	d := drainLeaseDeps{
		LeaderPresent: func(string) bool { return true },
		CurrentEpoch:  func(string) (int64, bool) { return 5, true },
		BarrierExists: func(string, int64) bool {
			// Barrier appears on the 2nd poll (the producer just wrote it).
			return atomic.AddInt32(&barrierReads, 1) >= 2
		},
		// After the barrier, OLD releases (active owner flips away) so the
		// graceful self-release wait completes.
		ActiveOwnerPID: func(string) (int, bool) { return 0, false },
		LoadAgent:      func(string) (*agent.Record, error) { return oldCoordRec(), nil },
		Resume: func(queue.SpawnFresh, string, int, io.Writer, io.Writer) error {
			atomic.AddInt32(&resumed, 1)
			return nil
		},
		LockAgent:   func(string) (func(), error) { return func() {}, nil },
		BarrierPoll: time.Millisecond,
	}
	if err := drainOneLeaseAwareWith(leaseDrainReq(), existingQueuePath(t), 0, 1000, out, out, d); err != nil {
		t.Fatalf("graceful-mid-poll path returned %v, want nil", err)
	}
	if atomic.LoadInt32(&resumed) != 0 {
		t.Errorf("legacy Resume ran %d times — barrier appeared, should have finished graceful path (no duplicate)", resumed)
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
	var recoveredID string
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
		RecoverSpawn: func(oldRec *agent.Record, _, preAllocatedID string, _, _ io.Writer) error {
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
		LeaderPresent: func(string) bool { return false },
		CurrentEpoch:  func(string) (int64, bool) { return 5, true },
		BarrierExists: func(string, int64) bool { return false },
		LoadAgent:     func(string) (*agent.Record, error) { return oldCoordRec(), nil },
		TakeOver:      func(string, string) (bool, error) { atomic.AddInt32(&tookOver, 1); return true, nil },
		RecoverSpawn: func(*agent.Record, string, string, io.Writer, io.Writer) error {
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
		LeaderPresent: func(string) bool { return false },
		CurrentEpoch:  func(string) (int64, bool) { return 9, true },
		BarrierExists: func(string, int64) bool { return false },
		LoadAgent:     func(string) (*agent.Record, error) { return oldCoordRec(), nil },
		TakeOver: func(project, _ string) (bool, error) {
			tookOverProject = project
			return true, nil // takeover acquired -> OLD confirmed gone
		},
		RecoverSpawn: func(*agent.Record, string, string, io.Writer, io.Writer) error {
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
	var recovered int32
	d := drainLeaseDeps{
		CurrentEpoch:   func(string) (int64, bool) { return 5, true },
		BarrierExists:  func(string, int64) bool { return true },
		ActiveOwnerPID: func(string) (int, bool) { return 424242, true }, // never releases
		LoadAgent:      func(string) (*agent.Record, error) { return oldCoordRec(), nil },
		TakeOver:       func(string, string) (bool, error) { atomic.AddInt32(&tookOver, 1); return true, nil },
		RecoverSpawn: func(*agent.Record, string, string, io.Writer, io.Writer) error {
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
		LeaderPresent: func(string) bool { return false },
		CurrentEpoch:  func(string) (int64, bool) { return 5, true },
		BarrierExists: func(string, int64) bool { return false },
		LoadAgent:     func(string) (*agent.Record, error) { return oldCoordRec(), nil },
		TakeOver:      func(string, string) (bool, error) { return false, nil }, // did NOT acquire
		RecoverSpawn: func(*agent.Record, string, string, io.Writer, io.Writer) error {
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
		LeaderPresent: func(string) bool { return false },
		CurrentEpoch:  func(string) (int64, bool) { return 9, true },
		BarrierExists: func(string, int64) bool { return false },
		LoadAgent:     func(string) (*agent.Record, error) { return oldCoordRec(), nil },
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
		RecoverSpawn: func(*agent.Record, string, string, io.Writer, io.Writer) error {
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
	oldRec := oldCoordRec()
	oldRec.TaskID = "coord-projects-fleet" // IsCoordSpawn requires "coord-<project>"

	var gotMarkerProj, gotMarkerID, gotSession, gotPrompt string
	origMarker, origPrompt := recoverWriteMarkerFn, recoverSendPromptFn
	t.Cleanup(func() { recoverWriteMarkerFn, recoverSendPromptFn = origMarker, origPrompt })
	recoverWriteMarkerFn = func(project, id string) error { gotMarkerProj, gotMarkerID = project, id; return nil }
	recoverSendPromptFn = func(session, prompt string) error { gotSession, gotPrompt = session, prompt; return nil }

	recoverHandoffTail(oldRec, "newcoord9", "fleet-newcoord9", "/tmp/handoff.md", &bytes.Buffer{})

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
	oldRec := oldCoordRec()
	oldRec.TaskID = "coord-projects-fleet"
	oldRec.DisableAutoResume = true

	var markerWritten, promptSent bool
	origMarker, origPrompt := recoverWriteMarkerFn, recoverSendPromptFn
	t.Cleanup(func() { recoverWriteMarkerFn, recoverSendPromptFn = origMarker, origPrompt })
	recoverWriteMarkerFn = func(string, string) error { markerWritten = true; return nil }
	recoverSendPromptFn = func(string, string) error { promptSent = true; return nil }

	recoverHandoffTail(oldRec, "newcoord9", "fleet-newcoord9", "/tmp/handoff.md", &bytes.Buffer{})

	if !markerWritten {
		t.Error("marker must be written even when auto-resume is disabled (discovery is independent of resume)")
	}
	if promptSent {
		t.Error("resume prompt must NOT be sent when DisableAutoResume is set")
	}
}
