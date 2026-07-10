//go:build linux || darwin

// Tests for the FLOCK-ONLY lease-aware drain path (PR-2 D8/D9b/D9b′). The whole
// routing keys on the FLOCK + the write-once handoff journal, NOT the deleted
// epoch: `barrierUp` reads the journal's barrier_id and `leaderBusy` is a live
// flock holder (busy⇒owner). There is no autonomous fence→kill takeover any
// more — a hung-but-alive OLD is SURFACED for `fleet rm`, never signaled.
//
// Deterministic: injectable drainLeaseDeps, no real lease/kill/Resume/clock.
// Counters observe convergence; budgets are tiny and tests assert the OUTCOME
// (returns / spawns / preserves-queue), never a wall-clock value.
package main

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
	"github.com/edisonshen/fleet/internal/coordlock"
	"github.com/edisonshen/fleet/internal/queue"
	"github.com/edisonshen/fleet/internal/state"
)

func leaseDrainReq() queue.SpawnFresh {
	return queue.SpawnFresh{
		SchemaVersion: queue.SchemaVersion,
		OldAgentID:    "oldcoord1",
		NewAgentID:    "newcoord1",
		Project:       "projects-fleet",
		TaskID:        "coord",
		HandoffDoc:    "/tmp/doc.md",
	}
}

func oldCoordRec() *agent.Record {
	r := agent.New("oldcoord1")
	r.Project = "projects-fleet"
	r.TaskID = "coord"
	r.LeaseWrapped = true
	r.SupervisorPID = 424242
	r.SupervisorPidStart = 99999
	r.SupervisorExePath = "/usr/local/bin/fleet"
	return r
}

// successorOwner is a live flock body naming the EXPECTED successor (positive
// identity == req.NewAgentID) — the only shape drain treats as a healthy
// successor (D9b).
func successorOwner() coordlock.Owner {
	return coordlock.Owner{AgentID: "newcoord1", PID: 555555, PidStart: 111}
}

// existingQueuePath writes a placeholder queue file and returns its path.
func existingQueuePath(t *testing.T) string {
	t.Helper()
	p := t.TempDir() + "/spawn-fresh-test.json"
	if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// stubDrainDeps returns fully-stubbed deterministic deps (no seam reaches a real
// flock/kill/Resume). Defaults: flock FREE, OLD provably DEAD, all side-effect
// seams no-op. Tests override the fields relevant to the scenario. Without a
// full stub set, fillDrainLeaseDeps would fill the gaps with PRODUCTION seams.
func stubDrainDeps() drainLeaseDeps {
	return drainLeaseDeps{
		LeaderPresent:   func(string) bool { return false },
		LiveOwner:       func(string) (coordlock.Owner, bool) { return coordlock.Owner{}, false },
		ActiveOwnerPID:  func(string) (int, bool) { return 0, false },
		BarrierExists:   func(string) bool { return false },
		LoadAgent:       func(string) (*agent.Record, error) { return oldCoordRec(), nil },
		KillCoord:       func(coord.KillTarget) error { return nil },
		SupervisorAlive: func(int, int64) bool { return false }, // OLD provably dead by default
		Resume:          func(queue.SpawnFresh, string, int, io.Writer, io.Writer) error { return nil },
		LockAgent:       func(string) (func(), error) { return func() {}, nil },
		RecoverSpawn:    func(*agent.Record, string, string, bool, io.Writer, io.Writer) error { return nil },
		DeliverPending:  func(queue.SpawnFresh, *agent.Record, io.Writer, io.Writer) error { return nil },
		BarrierPoll:     time.Millisecond,
		Self:            func() int { return 999999 },
	}
}

func queueGone(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	return errors.Is(err, os.ErrNotExist)
}

// T13: a live leader holds the flock, no barrier, OLD already ARCHIVED — the
// handoff already completed; the queue file is a stale leftover. STAND DOWN:
// clean the queue, spawn/kill nothing.
func TestDrainLease_StandDownStaleQueueUnderLiveLeader(t *testing.T) {
	req := leaseDrainReq()
	var resumes, recovers int32
	d := stubDrainDeps()
	d.LeaderPresent = func(string) bool { return true }
	d.LoadAgent = func(string) (*agent.Record, error) { return nil, state.ErrNotFound }
	d.Resume = func(queue.SpawnFresh, string, int, io.Writer, io.Writer) error { resumes++; return nil }
	d.RecoverSpawn = func(*agent.Record, string, string, bool, io.Writer, io.Writer) error { recovers++; return nil }

	path := existingQueuePath(t)
	out := &bytes.Buffer{}
	if err := drainOneLeaseAwareWith(req, path, 0, 0, out, out, d); err != nil {
		t.Fatalf("stand-down want nil err, got %v", err)
	}
	if atomic.LoadInt32(&resumes) != 0 || atomic.LoadInt32(&recovers) != 0 {
		t.Errorf("stand-down must spawn nothing: resumes=%d recovers=%d", resumes, recovers)
	}
	if !queueGone(t, path) {
		t.Error("stale queue must be cleaned on stand-down")
	}
}

// T16 / T16b: barrierUp (from the journal, NOT any epoch) routes to the graceful
// completion branch, and the handoff completes ONLY on a POSITIVE successor
// identity (flock body agent_id == req.NewAgentID). LeaderPresent is false here
// (OLD released after writing the barrier) — proving barrierUp alone routes to
// graceful, regressing the "no epoch ⇒ barrierUp always false ⇒ graceful branch
// unreachable" bug (D9b′).
func TestDrainLease_GracefulPositiveSuccessorCompletes(t *testing.T) {
	req := leaseDrainReq()
	var kills, delivers, recovers int32
	d := stubDrainDeps()
	d.BarrierExists = func(string) bool { return true }  // journal barrier present
	d.LeaderPresent = func(string) bool { return false } // OLD released the flock
	d.LiveOwner = func(string) (coordlock.Owner, bool) { return successorOwner(), true }
	d.KillCoord = func(coord.KillTarget) error { kills++; return nil }
	d.DeliverPending = func(queue.SpawnFresh, *agent.Record, io.Writer, io.Writer) error { delivers++; return nil }
	d.RecoverSpawn = func(*agent.Record, string, string, bool, io.Writer, io.Writer) error { recovers++; return nil }

	path := existingQueuePath(t)
	out := &bytes.Buffer{}
	if err := drainOneLeaseAwareWith(req, path, 0, 0, out, out, d); err != nil {
		t.Fatalf("graceful positive-identity completion want nil err, got %v", err)
	}
	if atomic.LoadInt32(&delivers) != 1 {
		t.Errorf("DeliverPending ran %d times, want 1 (deliver before queue clean)", delivers)
	}
	if atomic.LoadInt32(&recovers) != 0 {
		t.Errorf("RecoverSpawn ran %d times, want 0 (successor already leads)", recovers)
	}
	if !queueGone(t, path) {
		t.Error("queue must be cleaned once the expected successor leads")
	}
}

// T16 (R4/B fix): a busy flock held by a coord that is NOT the expected
// successor (bare/booting/foreign body agent_id != req.NewAgentID) is NOT a
// healthy successor, so the queue is PRESERVED — the delivery-delete never
// fires on a bare busy flock. Table covers a foreign id, an empty req.NewAgentID
// (legacy queue), and an identity-less busy body.
func TestDrainLease_HealthySuccessorPositiveIdentityOnly(t *testing.T) {
	cases := []struct {
		name       string
		newAgentID string
		owner      coordlock.Owner
		ownerOK    bool
	}{
		{"foreign_id_not_healthy", "newcoord1", coordlock.Owner{AgentID: "foreigncoord", PID: 7}, true},
		{"empty_expected_id_not_healthy", "", coordlock.Owner{AgentID: "anything", PID: 7}, true},
		{"identity_less_body_not_healthy", "newcoord1", coordlock.Owner{AgentID: "", PID: 7}, true},
		{"free_flock_not_healthy", "newcoord1", coordlock.Owner{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := stubDrainDeps()
			d.LiveOwner = func(string) (coordlock.Owner, bool) { return tc.owner, tc.ownerOK }
			if _, ok := d.healthySuccessorPresent("projects-fleet", tc.newAgentID); ok {
				t.Fatalf("healthySuccessorPresent = true for %s; want false (positive-identity only)", tc.name)
			}
		})
	}
}

// D8 no-kill DEAD recovery: flock FREE, no barrier, OLD provably DEAD — drain
// cold-spawns a fresh successor into the free flock (NO kill) and cleans the
// queue, returning the non-fatal ErrEscalatedToTakeOver.
func TestDrainLease_FreeFlockDeadOldRecoversNoKill(t *testing.T) {
	req := leaseDrainReq()
	var kills, recovers int32
	d := stubDrainDeps()
	// defaults: LeaderPresent=false (free), SupervisorAlive=false (dead).
	d.KillCoord = func(coord.KillTarget) error { kills++; return nil }
	d.RecoverSpawn = func(*agent.Record, string, string, bool, io.Writer, io.Writer) error { recovers++; return nil }

	path := existingQueuePath(t)
	out := &bytes.Buffer{}
	err := drainOneLeaseAwareWith(req, path, 0, 0, out, out, d)
	if !errors.Is(err, ErrEscalatedToTakeOver) {
		t.Fatalf("dead-OLD recovery want ErrEscalatedToTakeOver, got %v", err)
	}
	if atomic.LoadInt32(&recovers) != 1 {
		t.Errorf("RecoverSpawn ran %d times, want 1 (cold-spawn into the free flock)", recovers)
	}
	if atomic.LoadInt32(&kills) != 0 {
		t.Errorf("KillCoord ran %d times, want 0 (no kill — the flock is already free)", kills)
	}
	if !queueGone(t, path) {
		t.Error("queue must be cleaned after a successful recovery")
	}
}

// D8 no-kill SURFACE: flock FREE, no barrier, OLD is still ALIVE (a hung-alive
// process that released without archiving). The KP7 gate REFUSES to fence/kill
// a live coord — it surfaces `fleet rm`, preserves the queue, and recovers
// nothing.
func TestDrainLease_HungAliveOldSurfacesFleetRm(t *testing.T) {
	req := leaseDrainReq()
	var kills, recovers int32
	d := stubDrainDeps()
	d.SupervisorAlive = func(int, int64) bool { return true } // OLD alive
	d.KillCoord = func(coord.KillTarget) error { kills++; return nil }
	d.RecoverSpawn = func(*agent.Record, string, string, bool, io.Writer, io.Writer) error { recovers++; return nil }

	path := existingQueuePath(t)
	out := &bytes.Buffer{}
	err := drainOneLeaseAwareWith(req, path, 0, 0, out, out, d)
	if err == nil || errors.Is(err, ErrEscalatedToTakeOver) {
		t.Fatalf("hung-alive OLD want a preserve-queue abort error, got %v", err)
	}
	if atomic.LoadInt32(&kills) != 0 || atomic.LoadInt32(&recovers) != 0 {
		t.Errorf("hung-alive OLD must never kill/recover: kills=%d recovers=%d", kills, recovers)
	}
	if !strings.Contains(out.String(), "fleet rm") {
		t.Errorf("hung-alive abort must surface `fleet rm`; stderr=%q", out.String())
	}
	if queueGone(t, path) {
		t.Error("queue must be PRESERVED when OLD is not provably dead")
	}
}

// D8: flock FREE, the EXPECTED successor already acquired it (a warm standby) —
// drain completes without recovering, delivering the doc then cleaning the queue.
func TestDrainLease_FreeFlockSuccessorAcquired_NoRecover(t *testing.T) {
	req := leaseDrainReq()
	var recovers, delivers int32
	d := stubDrainDeps()
	d.LiveOwner = func(string) (coordlock.Owner, bool) { return successorOwner(), true }
	d.DeliverPending = func(queue.SpawnFresh, *agent.Record, io.Writer, io.Writer) error { delivers++; return nil }
	d.RecoverSpawn = func(*agent.Record, string, string, bool, io.Writer, io.Writer) error { recovers++; return nil }

	path := existingQueuePath(t)
	out := &bytes.Buffer{}
	if err := drainOneLeaseAwareWith(req, path, 0, 0, out, out, d); err != nil {
		t.Fatalf("successor-acquired want nil err, got %v", err)
	}
	if atomic.LoadInt32(&recovers) != 0 {
		t.Errorf("RecoverSpawn ran %d times, want 0 (successor already holds the flock)", recovers)
	}
	if atomic.LoadInt32(&delivers) != 1 {
		t.Errorf("DeliverPending ran %d times, want 1", delivers)
	}
	if !queueGone(t, path) {
		t.Error("queue must be cleaned once the successor holds the flock")
	}
}

// A live leader still holds the flock with no barrier and OLD is NOT archived —
// the graceful producer hasn't written a barrier yet, so complete via the LEGACY
// Resume path rather than stranding the queue (or spawning a duplicate).
func TestDrainLease_LiveLeaderPendingHandoff_FallsBackToResume(t *testing.T) {
	req := leaseDrainReq()
	var resumes, recovers int32
	d := stubDrainDeps()
	d.LeaderPresent = func(string) bool { return true }
	d.ActiveOwnerPID = func(string) (int, bool) { return 424242, true } // OLD still holds
	d.Resume = func(queue.SpawnFresh, string, int, io.Writer, io.Writer) error { resumes++; return nil }
	d.RecoverSpawn = func(*agent.Record, string, string, bool, io.Writer, io.Writer) error { recovers++; return nil }

	path := existingQueuePath(t)
	out := &bytes.Buffer{}
	if err := drainOneLeaseAwareWith(req, path, 0, 0, out, out, d); err != nil {
		t.Fatalf("live-leader fallback want nil err, got %v", err)
	}
	if atomic.LoadInt32(&resumes) != 1 {
		t.Errorf("Resume ran %d times, want 1 (legacy completion)", resumes)
	}
	if atomic.LoadInt32(&recovers) != 0 {
		t.Errorf("RecoverSpawn ran %d times, want 0", recovers)
	}
}

// A BARE (non-lease-wrapped, SupervisorPID==0) OLD holds no lease — its handoff
// ALWAYS completes via the legacy resume, never a takeover/graceful-reap, even
// if another coord-run currently holds the project lease.
func TestDrainLease_BareCoordRoutesToLegacyResume(t *testing.T) {
	req := leaseDrainReq()
	var resumes, recovers int32
	bare := oldCoordRec()
	bare.SupervisorPID = 0
	bare.LeaseWrapped = false
	d := stubDrainDeps()
	d.LeaderPresent = func(string) bool { return true } // a different coord-run leads
	d.LoadAgent = func(string) (*agent.Record, error) { return bare, nil }
	d.Resume = func(queue.SpawnFresh, string, int, io.Writer, io.Writer) error { resumes++; return nil }
	d.RecoverSpawn = func(*agent.Record, string, string, bool, io.Writer, io.Writer) error { recovers++; return nil }

	path := existingQueuePath(t)
	out := &bytes.Buffer{}
	if err := drainOneLeaseAwareWith(req, path, 0, 0, out, out, d); err != nil {
		t.Fatalf("bare-coord legacy resume want nil err, got %v", err)
	}
	if atomic.LoadInt32(&resumes) != 1 {
		t.Errorf("Resume ran %d times, want 1 (bare coord → legacy)", resumes)
	}
	if atomic.LoadInt32(&recovers) != 0 {
		t.Errorf("RecoverSpawn ran %d times, want 0 (never a takeover for a bare coord)", recovers)
	}
}

// An UNSTAMPED lease-wrapped OLD (SupervisorPID==0, LeaseWrapped==true) has no
// authenticatable identity, so the KP7 gate aborts (not provably dead) and
// PRESERVES the queue — never an unguarded kill/recover.
func TestDrainLease_UnstampedLeaseWrappedCoordPreservesQueue(t *testing.T) {
	req := leaseDrainReq()
	var recovers int32
	unstamped := oldCoordRec()
	unstamped.SupervisorPID = 0
	unstamped.LeaseWrapped = true
	d := stubDrainDeps()
	d.LoadAgent = func(string) (*agent.Record, error) { return unstamped, nil }
	d.RecoverSpawn = func(*agent.Record, string, string, bool, io.Writer, io.Writer) error { recovers++; return nil }

	path := existingQueuePath(t)
	out := &bytes.Buffer{}
	err := drainOneLeaseAwareWith(req, path, 0, 0, out, out, d)
	if err == nil || errors.Is(err, ErrEscalatedToTakeOver) {
		t.Fatalf("unstamped lease-wrapped OLD want a preserve-queue abort, got %v", err)
	}
	if atomic.LoadInt32(&recovers) != 0 {
		t.Errorf("RecoverSpawn ran %d times, want 0 (no recover on an unauthenticatable OLD)", recovers)
	}
	if queueGone(t, path) {
		t.Error("queue must be PRESERVED for an unstamped lease-wrapped OLD")
	}
}

// T42: a graceful reap that hits a KillCoord refusal (PID reuse / would-shoot-
// the-successor) surfaces the error and PRESERVES the queue — never a
// wrong-process kill or a silent drop.
func TestDrainLease_GracefulKillRefusalSurfaced(t *testing.T) {
	req := leaseDrainReq()
	refusal := errors.New("kill refused: pid-start mismatch")
	d := stubDrainDeps()
	d.BarrierExists = func(string) bool { return true }
	d.LiveOwner = func(string) (coordlock.Owner, bool) { return successorOwner(), true }
	d.KillCoord = func(coord.KillTarget) error { return refusal }

	path := existingQueuePath(t)
	out := &bytes.Buffer{}
	err := drainOneLeaseAwareWith(req, path, 0, 0, out, out, d)
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("kill refusal must be surfaced, got %v", err)
	}
	if queueGone(t, path) {
		t.Error("queue must be PRESERVED when the reap is refused")
	}
}

// The dead-OLD recovery honors the queue's per-handoff auto-resume OVERRIDE:
// req.DisableAutoResume=&true flows into RecoverSpawn's disableAutoResume arg.
func TestDrainLease_DeadRecoveryHonorsQueueAutoResumeOverride(t *testing.T) {
	req := leaseDrainReq()
	disable := true
	req.DisableAutoResume = &disable
	var gotDisable atomic.Bool
	var recovers int32
	d := stubDrainDeps()
	d.RecoverSpawn = func(_ *agent.Record, _ string, _ string, disableAutoResume bool, _ io.Writer, _ io.Writer) error {
		gotDisable.Store(disableAutoResume)
		recovers++
		return nil
	}

	path := existingQueuePath(t)
	out := &bytes.Buffer{}
	if err := drainOneLeaseAwareWith(req, path, 0, 0, out, out, d); !errors.Is(err, ErrEscalatedToTakeOver) {
		t.Fatalf("dead recovery want ErrEscalatedToTakeOver, got %v", err)
	}
	if atomic.LoadInt32(&recovers) != 1 {
		t.Fatalf("RecoverSpawn ran %d times, want 1", recovers)
	}
	if !gotDisable.Load() {
		t.Error("RecoverSpawn disableAutoResume = false, want true (queue override &true)")
	}
}

// Concurrent recovery: the queue file was already deleted by a peer drain, so
// the takeover pre-check stands down (nothing to recover) rather than spawning a
// duplicate.
func TestDrainLease_ConcurrentRecovery_NoDuplicate(t *testing.T) {
	req := leaseDrainReq()
	var recovers int32
	d := stubDrainDeps()
	d.RecoverSpawn = func(*agent.Record, string, string, bool, io.Writer, io.Writer) error { recovers++; return nil }

	// A path that never existed models a peer drain having already cleaned it.
	path := t.TempDir() + "/already-recovered.json"
	out := &bytes.Buffer{}
	if err := drainOneLeaseAwareWith(req, path, 0, 0, out, out, d); err != nil {
		t.Fatalf("concurrent stand-down want nil err, got %v", err)
	}
	if atomic.LoadInt32(&recovers) != 0 {
		t.Errorf("RecoverSpawn ran %d times, want 0 (peer already recovered)", recovers)
	}
}
