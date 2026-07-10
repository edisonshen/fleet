//go:build linux || darwin

// KP7 dead-targets-only escalation gate (DESIGN-coord-no-auto-kill / PR-2 D8).
// Under the flock-only lease there is NO autonomous fence→kill takeover: the
// no-kill recovery (takeoverAndRecover) may cold-spawn a successor ONLY when OLD
// is PROVABLY DEAD (pid + pid_start). A LIVE / unauthenticatable OLD makes it
// abort — preserve the queue + surface `fleet rm`, never signal a live process.
// These cover the gate at the GRACEFUL (barrier) escalation site and the
// unloadable-record path; the free-flock site is covered in drain_lease_test.go.
package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/coord"
	"github.com/edisonshen/fleet/internal/coordlock"
)

// foreignOwner is a live flock body that is NOT the expected successor, so
// healthySuccessorPresent never matches and the graceful path escalates.
func foreignOwner() coordlock.Owner {
	return coordlock.Owner{AgentID: "foreigncoord", PID: 7777, PidStart: 222}
}

// A barrier is up (graceful producer ran) but the expected successor never
// acquires and OLD is still ALIVE. The graceful path times out and reaches the
// KP7 gate, which REFUSES to fence/kill the live OLD — surface `fleet rm`,
// preserve the queue, recover nothing.
func TestDrainNoAutoKill_GracefulLiveWedgedOldAborts(t *testing.T) {
	req := leaseDrainReq()
	var kills, recovers int32
	d := stubDrainDeps()
	d.BarrierExists = func(string) bool { return true }
	d.LiveOwner = func(string) (coordlock.Owner, bool) { return foreignOwner(), true } // not the successor
	d.SupervisorAlive = func(int, int64) bool { return true }                          // OLD alive
	d.KillCoord = func(coord.KillTarget) error { kills++; return nil }
	d.RecoverSpawn = func(*agent.Record, string, string, bool, io.Writer, io.Writer) error { recovers++; return nil }

	path := existingQueuePath(t)
	out := &bytes.Buffer{}
	err := drainOneLeaseAwareWith(req, path, 0, 0, out, out, d)
	if err == nil || errors.Is(err, ErrEscalatedToTakeOver) {
		t.Fatalf("graceful live-wedged OLD want a preserve-queue abort, got %v", err)
	}
	if atomic.LoadInt32(&kills) != 0 || atomic.LoadInt32(&recovers) != 0 {
		t.Errorf("live OLD must never be killed/recovered: kills=%d recovers=%d", kills, recovers)
	}
	if !strings.Contains(out.String(), "fleet rm") {
		t.Errorf("abort must surface `fleet rm`; stderr=%q", out.String())
	}
	if queueGone(t, path) {
		t.Error("queue must be PRESERVED for a live wedged OLD")
	}
}

// The graceful gate is not over-broad: a barrier up + no successor + OLD
// PROVABLY DEAD proceeds to a no-kill cold recovery.
func TestDrainNoAutoKill_GracefulDeadOldRecovers(t *testing.T) {
	req := leaseDrainReq()
	var kills, recovers int32
	d := stubDrainDeps()
	d.BarrierExists = func(string) bool { return true }
	d.LiveOwner = func(string) (coordlock.Owner, bool) { return foreignOwner(), true }
	// SupervisorAlive default false ⇒ OLD dead.
	d.KillCoord = func(coord.KillTarget) error { kills++; return nil }
	d.RecoverSpawn = func(*agent.Record, string, string, bool, io.Writer, io.Writer) error { recovers++; return nil }

	path := existingQueuePath(t)
	out := &bytes.Buffer{}
	if err := drainOneLeaseAwareWith(req, path, 0, 0, out, out, d); !errors.Is(err, ErrEscalatedToTakeOver) {
		t.Fatalf("graceful dead-OLD recovery want ErrEscalatedToTakeOver, got %v", err)
	}
	if atomic.LoadInt32(&recovers) != 1 {
		t.Errorf("RecoverSpawn ran %d times, want 1 (dead OLD ⇒ cold recover)", recovers)
	}
	if atomic.LoadInt32(&kills) != 0 {
		t.Errorf("KillCoord ran %d times, want 0 (dead OLD already freed the flock)", kills)
	}
}

// The KP7 gate matrix as a unit test on takeoverAndRecover directly: dead ⇒
// recover; alive / nil-record / no-identity ⇒ abort (no recover, queue preserved).
func TestDrainNoAutoKill_GateMatrix(t *testing.T) {
	cases := []struct {
		name        string
		cachedOld   *agent.Record
		alive       bool
		wantRecover bool
	}{
		{"dead_recovers", oldCoordRec(), false, true},
		{"alive_aborts", oldCoordRec(), true, false},
		{"nil_record_aborts", nil, false, false},
		{"no_supervisor_identity_aborts", func() *agent.Record { r := oldCoordRec(); r.SupervisorPID = 0; return r }(), false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var recovers int32
			d := stubDrainDeps()
			d.SupervisorAlive = func(int, int64) bool { return tc.alive }
			d.RecoverSpawn = func(*agent.Record, string, string, bool, io.Writer, io.Writer) error { recovers++; return nil }
			path := existingQueuePath(t)
			out := &bytes.Buffer{}
			err := takeoverAndRecover(leaseDrainReq(), path, tc.cachedOld, out, out, d)
			gotRecover := atomic.LoadInt32(&recovers) == 1
			if gotRecover != tc.wantRecover {
				t.Fatalf("recover=%v want %v (err=%v)", gotRecover, tc.wantRecover, err)
			}
			if tc.wantRecover && !errors.Is(err, ErrEscalatedToTakeOver) {
				t.Fatalf("recover path want ErrEscalatedToTakeOver, got %v", err)
			}
			if !tc.wantRecover && (err == nil || errors.Is(err, ErrEscalatedToTakeOver)) {
				t.Fatalf("abort path want a preserve-queue error, got %v", err)
			}
			if !tc.wantRecover && queueGone(t, path) {
				t.Error("abort must PRESERVE the queue")
			}
		})
	}
}
