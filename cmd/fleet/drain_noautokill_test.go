//go:build linux || darwin

package main

// drain_noautokill_test.go — KP7 (DESIGN-coord-no-auto-kill, task
// coord-no-auto-kill-ac54): the drain barrier-timeout escalation into
// takeoverAndRecover proceeds ONLY when OLD's supervisor is provably
// dead (pid + pid_start identity). A live OLD — or an OLD whose record
// cannot be loaded (not-provably-dead) — aborts with a loud diagnostic
// and a drain failure; the queue is preserved for retry. All THREE
// takeoverAndRecover call sites are covered:
//
//	drainWaitBarrierOrEscalate (~878, no barrier)      — tests below
//	drainGraceful ~659 (barrier + live OLD past budget) — 7b(a)
//	drainGraceful ~697 (no healthy successor in budget) — 7b(b)

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/coord"
	"github.com/edisonshen/fleet/internal/queue"
	"github.com/edisonshen/fleet/internal/state"
)

// Test 7 (plan), live half: barrier timeout with OLD's supervisor ALIVE
// (pid+pid_start match) — no fence, no kill, no takeover, no recover
// spawn; drain returns failure with a diagnostic and the queue survives.
func TestDrainLease_EscalationGate_LiveOldAborts(t *testing.T) {
	out := &bytes.Buffer{}
	var killed, tookOver, recovered int32
	d := drainLeaseDeps{
		DeliverPending: func(queue.SpawnFresh, *agent.Record, io.Writer, io.Writer) error { return nil },
		LeaderPresent:  func(string) bool { return false }, // hung: heartbeat stale
		CurrentEpoch:   func(string) (int64, bool) { return 5, true },
		BarrierExists:  func(string, int64) bool { return false }, // never appears
		LoadAgent:      func(string) (*agent.Record, error) { return oldCoordRec(), nil },
		// OLD's supervisor process is STILL ALIVE (the machine-stall false
		// positive): a staleness timeout must not become a kill.
		SupervisorAlive: func(pid int, pidStart int64) bool {
			if pid != 424242 || pidStart != 99999 {
				t.Errorf("SupervisorAlive probed pid=%d start=%d, want OLD's recorded identity 424242/99999", pid, pidStart)
			}
			return true
		},
		KillCoord: func(coord.KillTarget) error { atomic.AddInt32(&killed, 1); return nil },
		TakeOver: func(string, string) (bool, []liveHolderInfo, error) {
			atomic.AddInt32(&tookOver, 1)
			return true, nil, nil
		},
		RecoverSpawn: func(*agent.Record, string, string, bool, io.Writer, io.Writer) error {
			atomic.AddInt32(&recovered, 1)
			return nil
		},
		LockAgent:   func(string) (func(), error) { return func() {}, nil },
		BarrierPoll: time.Millisecond,
	}
	qp := existingQueuePath(t)
	err := drainOneLeaseAwareWith(leaseDrainReq(), qp, 0, 5, out, out, d)
	if err == nil || errors.Is(err, ErrEscalatedToTakeOver) {
		t.Fatalf("live OLD must abort the escalation with a failure, got %v", err)
	}
	if atomic.LoadInt32(&tookOver) != 0 {
		t.Errorf("TakeOver ran %d times against a LIVE old coord, want 0", tookOver)
	}
	if atomic.LoadInt32(&killed) != 0 {
		t.Errorf("KillCoord ran %d times against a LIVE old coord, want 0", killed)
	}
	if atomic.LoadInt32(&recovered) != 0 {
		t.Errorf("RecoverSpawn ran %d times, want 0 (no successor over a live coord)", recovered)
	}
	if _, statErr := os.Stat(qp); statErr != nil {
		t.Errorf("queue file must be preserved for retry, stat: %v", statErr)
	}
	if !strings.Contains(out.String(), "alive") {
		t.Errorf("expected a loud live-OLD diagnostic, got: %q", out.String())
	}
}

// Test 7 (plan), dead half: OLD provably dead -> escalation proceeds
// exactly as today (takeover + recover spawn + queue cleaned).
func TestDrainLease_EscalationGate_DeadOldProceeds(t *testing.T) {
	out := &bytes.Buffer{}
	var tookOver, recovered int32
	d := drainLeaseDeps{
		DeliverPending:  func(queue.SpawnFresh, *agent.Record, io.Writer, io.Writer) error { return nil },
		LeaderPresent:   func(string) bool { return false },
		CurrentEpoch:    func(string) (int64, bool) { return 5, true },
		BarrierExists:   func(string, int64) bool { return false },
		LoadAgent:       func(string) (*agent.Record, error) { return oldCoordRec(), nil },
		SupervisorAlive: func(int, int64) bool { return false }, // provably dead
		TakeOver: func(string, string) (bool, []liveHolderInfo, error) {
			atomic.AddInt32(&tookOver, 1)
			return true, nil, nil
		},
		RecoverSpawn: func(*agent.Record, string, string, bool, io.Writer, io.Writer) error {
			atomic.AddInt32(&recovered, 1)
			return nil
		},
		LockAgent:   func(string) (func(), error) { return func() {}, nil },
		BarrierPoll: time.Millisecond,
	}
	err := drainOneLeaseAwareWith(leaseDrainReq(), existingQueuePath(t), 0, 5, out, out, d)
	if !errors.Is(err, ErrEscalatedToTakeOver) {
		t.Fatalf("dead OLD must escalate as today, got %v", err)
	}
	if atomic.LoadInt32(&tookOver) != 1 || atomic.LoadInt32(&recovered) != 1 {
		t.Errorf("tookOver=%d recovered=%d, want 1/1 (dead-OLD path byte-equivalent to today)",
			tookOver, recovered)
	}
}

// Test 7 (plan), unloadable half: OLD's agent record cannot be loaded =
// NOT provably dead -> abort, queue preserved. (An unreadable record
// gives no pid+pid_start identity to authenticate the escalation with.)
func TestDrainLease_EscalationGate_UnloadableRecordAborts(t *testing.T) {
	out := &bytes.Buffer{}
	var tookOver, recovered int32
	d := drainLeaseDeps{
		DeliverPending:  func(queue.SpawnFresh, *agent.Record, io.Writer, io.Writer) error { return nil },
		LeaderPresent:   func(string) bool { return false },
		CurrentEpoch:    func(string) (int64, bool) { return 5, true },
		BarrierExists:   func(string, int64) bool { return false },
		LoadAgent:       func(string) (*agent.Record, error) { return nil, state.ErrNotFound },
		SupervisorAlive: func(int, int64) bool { return false }, // never consulted: no identity
		TakeOver: func(string, string) (bool, []liveHolderInfo, error) {
			atomic.AddInt32(&tookOver, 1)
			return true, nil, nil
		},
		RecoverSpawn: func(*agent.Record, string, string, bool, io.Writer, io.Writer) error {
			atomic.AddInt32(&recovered, 1)
			return nil
		},
		LockAgent:   func(string) (func(), error) { return func() {}, nil },
		BarrierPoll: time.Millisecond,
	}
	qp := existingQueuePath(t)
	err := drainOneLeaseAwareWith(leaseDrainReq(), qp, 0, 5, out, out, d)
	if err == nil || errors.Is(err, ErrEscalatedToTakeOver) {
		t.Fatalf("unloadable OLD record must abort (not-provably-dead), got %v", err)
	}
	if atomic.LoadInt32(&tookOver) != 0 || atomic.LoadInt32(&recovered) != 0 {
		t.Errorf("tookOver=%d recovered=%d, want 0/0 on not-provably-dead", tookOver, recovered)
	}
	if _, statErr := os.Stat(qp); statErr != nil {
		t.Errorf("queue file must be preserved for retry, stat: %v", statErr)
	}
}

// Test 7b(a) (plan): drainGraceful barrier-present site (~659). OLD wrote
// the barrier, stays the active owner past the budget, and its supervisor
// is ALIVE — the escalation gate aborts: no takeover, no kill, drain
// failure, queue preserved.
func TestDrainLease_GracefulLiveWedgedOld_BarrierSite_Aborts(t *testing.T) {
	out := &bytes.Buffer{}
	var killed, tookOver, recovered int32
	d := drainLeaseDeps{
		DeliverPending:  func(queue.SpawnFresh, *agent.Record, io.Writer, io.Writer) error { return nil },
		CurrentEpoch:    func(string) (int64, bool) { return 5, true },
		BarrierExists:   func(string, int64) bool { return true },
		ActiveOwnerPID:  func(string) (int, bool) { return 424242, true }, // OLD never yields
		LoadAgent:       func(string) (*agent.Record, error) { return oldCoordRec(), nil },
		SupervisorAlive: func(int, int64) bool { return true }, // live-wedged
		KillCoord:       func(coord.KillTarget) error { atomic.AddInt32(&killed, 1); return nil },
		TakeOver: func(string, string) (bool, []liveHolderInfo, error) {
			atomic.AddInt32(&tookOver, 1)
			return true, nil, nil
		},
		RecoverSpawn: func(*agent.Record, string, string, bool, io.Writer, io.Writer) error {
			atomic.AddInt32(&recovered, 1)
			return nil
		},
		LockAgent:   func(string) (func(), error) { return func() {}, nil },
		BarrierPoll: time.Millisecond,
	}
	qp := existingQueuePath(t)
	err := drainOneLeaseAwareWith(leaseDrainReq(), qp, 0, 5, out, out, d)
	if err == nil || errors.Is(err, ErrEscalatedToTakeOver) {
		t.Fatalf("live-wedged OLD at the barrier site must abort, got %v", err)
	}
	if atomic.LoadInt32(&tookOver) != 0 || atomic.LoadInt32(&killed) != 0 || atomic.LoadInt32(&recovered) != 0 {
		t.Errorf("tookOver=%d killed=%d recovered=%d, want all 0 against a live OLD",
			tookOver, killed, recovered)
	}
	if _, statErr := os.Stat(qp); statErr != nil {
		t.Errorf("queue must be preserved, stat: %v", statErr)
	}
}

// Test 7b(b) (plan): drainGraceful no-healthy-successor site (~697). OLD
// released the lease, no successor acquires within the budget, and OLD's
// supervisor is still ALIVE — the gate aborts instead of fencing+killing.
func TestDrainLease_GracefulLiveOld_NoSuccessorSite_Aborts(t *testing.T) {
	out := &bytes.Buffer{}
	var tookOver, recovered int32
	var reads int32
	d := drainLeaseDeps{
		DeliverPending: func(queue.SpawnFresh, *agent.Record, io.Writer, io.Writer) error { return nil },
		CurrentEpoch:   func(string) (int64, bool) { return 5, true },
		BarrierExists:  func(string, int64) bool { return true },
		ActiveOwnerPID: func(string) (int, bool) {
			if atomic.AddInt32(&reads, 1) == 1 {
				return 424242, true // OLD still leader on the first read
			}
			return 0, false // released; no successor ever acquires
		},
		LoadAgent:       func(string) (*agent.Record, error) { return oldCoordRec(), nil },
		SupervisorAlive: func(int, int64) bool { return true }, // OLD process still alive
		TakeOver: func(string, string) (bool, []liveHolderInfo, error) {
			atomic.AddInt32(&tookOver, 1)
			return true, nil, nil
		},
		RecoverSpawn: func(*agent.Record, string, string, bool, io.Writer, io.Writer) error {
			atomic.AddInt32(&recovered, 1)
			return nil
		},
		LockAgent:   func(string) (func(), error) { return func() {}, nil },
		BarrierPoll: time.Millisecond,
	}
	qp := existingQueuePath(t)
	err := drainOneLeaseAwareWith(leaseDrainReq(), qp, 0, 5, out, out, d)
	if err == nil || errors.Is(err, ErrEscalatedToTakeOver) {
		t.Fatalf("live OLD at the no-successor site must abort, got %v", err)
	}
	if atomic.LoadInt32(&tookOver) != 0 || atomic.LoadInt32(&recovered) != 0 {
		t.Errorf("tookOver=%d recovered=%d, want 0/0 against a live OLD", tookOver, recovered)
	}
	if _, statErr := os.Stat(qp); statErr != nil {
		t.Errorf("queue must be preserved, stat: %v", statErr)
	}
}
