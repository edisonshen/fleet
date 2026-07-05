package coordlock

// Observability emitters for the lease lifecycle
// (DESIGN-coord-lease-false-fence-prevention piece 2 /
// TASK-PLAN-lease-observability-8c4e). The coord-run supervisor renews the
// lease every 10s but logs nothing today; when rainier's renewal stalled for
// 53 minutes there was zero on-disk evidence. These tests pin the emitter
// contract: renew sampled <=1/min, renew.fail always with the error string,
// acquire/release once each with the epoch, and — the PR #241 wedge-class
// regression — NO emit ever runs inside the short coordinator.epoch.lock.

import (
	"errors"
	"os"
	"sync"
	"testing"
	"time"
)

// capturingEmitter records every (evt, data) the lease emits so a test can
// assert the lifecycle without shelling out to fleetlog.
type capturingEmitter struct {
	mu     sync.Mutex
	events []capturedEvent
}

type capturedEvent struct {
	evt  string
	data map[string]any
}

func (c *capturingEmitter) emit(evt string, data map[string]any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, capturedEvent{evt: evt, data: data})
}

func (c *capturingEmitter) count(evt string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, e := range c.events {
		if e.evt == evt {
			n++
		}
	}
	return n
}

func (c *capturingEmitter) first(evt string) (map[string]any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.events {
		if e.evt == evt {
			return e.data, true
		}
	}
	return nil, false
}

// Test 1: renew sampling — at most one lease.renew per simulated minute over
// 5 minutes at the 10s heartbeat cadence. Drives the sampling helper directly
// with an injected monotonic clock (NOT through the live goroutine — the loop
// clock/ticker seams are scoped to a later task).
func TestRenewSampler_AtMostOncePerMinute(t *testing.T) {
	var s renewSampler
	const step = int64(10 * time.Second) // heartbeat cadence
	emits := 0
	var mono int64
	for beat := 0; beat < 30; beat++ { // 30 beats * 10s = 5 simulated minutes
		if s.shouldEmit(mono, time.Minute) {
			emits++
		}
		mono += step
	}
	// first beat always logs, then one per 60s window: t=0,60,120,180,240 => 5.
	if emits != 5 {
		t.Fatalf("lease.renew emits = %d over 5 simulated minutes, want 5 (<=1/min)", emits)
	}
}

// Test 2: renew failure — an injected write fault (or fenced self-demote) logs
// lease.renew.fail EVERY time (never sampled), carrying the error string.
func TestEmitRenewResult_FailureAlwaysLogsError(t *testing.T) {
	em := &capturingEmitter{}
	l := &Lease{
		cfg:   leaseConfig{emit: em.emit, nowMono: func() int64 { return 0 }},
		epoch: 9,
	}
	writeFault := errors.New("epoch write: disk full")
	// Two consecutive failures both log (no sampling on the failure path).
	l.emitRenewResult(true, writeFault)
	l.emitRenewResult(true, writeFault)
	if got := em.count("lease.renew.fail"); got != 2 {
		t.Fatalf("lease.renew.fail count = %d, want 2 (failures are never sampled)", got)
	}
	data, _ := em.first("lease.renew.fail")
	reason, _ := data["reason"].(string)
	if reason != writeFault.Error() {
		t.Fatalf("lease.renew.fail reason = %q, want the write-fault error string %q", reason, writeFault.Error())
	}
	if data["epoch"] != int64(9) {
		t.Fatalf("lease.renew.fail epoch = %v, want 9", data["epoch"])
	}
	// A fenced self-demote (ok=false, err=nil) also logs, without an error.
	em2 := &capturingEmitter{}
	l.cfg.emit = em2.emit
	l.emitRenewResult(false, nil)
	if em2.count("lease.renew.fail") != 1 {
		t.Fatalf("fenced self-demote (ok=false) must log lease.renew.fail")
	}
}

// Test 3: acquire + release — a normal lifecycle logs exactly one
// lease.acquire and one lease.release, both carrying the SAME epoch.
func TestLeaseAcquireRelease_EmitsLifecycle(t *testing.T) {
	setupHome(t)
	const project = "obs-lifecycle"
	clk := &fakeClock{}
	live := newFakeLiveness()
	em := &capturingEmitter{}
	cfg := testCfg(clk, live)
	cfg.emit = em.emit

	lease, acquired, err := acquireLease(project, "cand", cfg)
	if err != nil || !acquired {
		t.Fatalf("acquireLease: acquired=%v err=%v", acquired, err)
	}
	epoch := lease.epoch
	lease.Release()

	if got := em.count("lease.acquire"); got != 1 {
		t.Fatalf("lease.acquire count = %d, want 1", got)
	}
	if got := em.count("lease.release"); got != 1 {
		t.Fatalf("lease.release count = %d, want 1", got)
	}
	acq, _ := em.first("lease.acquire")
	rel, _ := em.first("lease.release")
	if acq["epoch"] != epoch || rel["epoch"] != epoch {
		t.Fatalf("acquire epoch=%v release epoch=%v, want both %d", acq["epoch"], rel["epoch"], epoch)
	}
	// A clean release demoted the record out of active.
	if rel["demoted"] != true {
		t.Errorf("clean release demoted = %v, want true", rel["demoted"])
	}
}

// A Release that CANNOT demote the epoch record (corrupt/unreadable epoch
// file) must log lease.release with demoted=false — the on-disk lease may
// still be active with valid tokens until TTL, so the log must not overstate
// success on the exact path operators inspect during a bad handoff (codex P3).
func TestLeaseRelease_FailedDemoteLogsNotDemoted(t *testing.T) {
	setupHome(t)
	const project = "obs-faildemote"
	clk := &fakeClock{}
	live := newFakeLiveness()
	em := &capturingEmitter{}
	cfg := testCfg(clk, live)
	cfg.emit = em.emit

	lease, acquired, err := acquireLease(project, "cand", cfg)
	if err != nil || !acquired {
		t.Fatalf("acquireLease: acquired=%v err=%v", acquired, err)
	}
	// Corrupt the epoch record so the demote loop's readEpoch fails on every
	// attempt -> guaranteed stays false -> demoted=false.
	if werr := os.WriteFile(lease.paths.epoch, []byte("not-json"), 0o644); werr != nil {
		t.Fatalf("corrupt epoch file: %v", werr)
	}
	lease.Release()

	if got := em.count("lease.release"); got != 1 {
		t.Fatalf("lease.release count = %d, want 1", got)
	}
	rel, _ := em.first("lease.release")
	if rel["demoted"] != false {
		t.Errorf("failed-demote release demoted = %v, want false", rel["demoted"])
	}
}

// Test 5: no emit ever runs inside coordinator.epoch.lock (the PR #241
// wedge class). The seam-injected sink tries to LOCK_NB the epoch lock on
// every emit; if the emitting code path still held the lock, the NB acquire
// fails (flock is per-fd, so a second fd in the same process cannot barge the
// held lock) and the test fails.
func TestLeaseEmitters_NeverInsideEpochLock(t *testing.T) {
	setupHome(t)
	const project = "obs-nolock"
	clk := &fakeClock{}
	live := newFakeLiveness()
	paths, err := resolvePaths(project)
	if err != nil {
		t.Fatalf("resolvePaths: %v", err)
	}

	violated := false
	var vmu sync.Mutex
	sink := func(evt string, _ map[string]any) {
		f, got, ferr := tryFlock(paths.epochLock)
		if ferr != nil {
			t.Errorf("emit %q: tryFlock epochLock: %v", evt, ferr)
			return
		}
		if !got {
			vmu.Lock()
			violated = true
			vmu.Unlock()
			return
		}
		_ = releaseFlock(f)
	}

	cfg := testCfg(clk, live)
	cfg.emit = sink

	lease, acquired, err := acquireLease(project, "cand", cfg) // emits lease.acquire
	if err != nil || !acquired {
		t.Fatalf("acquireLease: acquired=%v err=%v", acquired, err)
	}
	// Drive the renewal emitters directly (outside the goroutine).
	lease.emitRenewResult(true, errors.New("boom")) // lease.renew.fail
	lease.renewSampler = renewSampler{}             // reset so the next success logs
	lease.emitRenewResult(true, nil)                // lease.renew (sampled, first => logs)
	lease.Release()                                 // lease.release

	vmu.Lock()
	defer vmu.Unlock()
	if violated {
		t.Fatal("an emit ran while coordinator.epoch.lock was held (PR #241 wedge-class regression)")
	}
}
