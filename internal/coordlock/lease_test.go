//go:build linux || darwin

package coordlock

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/state"
)

// ---- test harness ----
//
// The lease primitive is exercised through acquireLease (the
// failover-gate-free core) with an injected leaseConfig. All seams are
// deterministic: a fake monotonic clock the test advances, a fake
// pid-liveness map, a fixed boot-id, and a controllable kill-stub. No
// time.Sleep-based timing assertions.

// fakeClock is a deterministic monotonic clock the test advances by hand.
type fakeClock struct {
	mu sync.Mutex
	ns int64
}

func (c *fakeClock) now() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ns
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ns += int64(d)
}

// fakeLiveness models pid -> pid_start for live processes. A pid absent
// from the map is "dead". The real self pid is always kept live (acquire
// reads its own pid_start).
type fakeLiveness struct {
	mu   sync.Mutex
	live map[int]int64
}

func newFakeLiveness() *fakeLiveness {
	return &fakeLiveness{live: map[int]int64{os.Getpid(): selfStart}}
}

const selfStart = int64(111111)

func (l *fakeLiveness) get(pid int) (int64, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	st, ok := l.live[pid]
	return st, ok
}

func (l *fakeLiveness) set(pid int, start int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.live[pid] = start
}

func (l *fakeLiveness) kill(pid int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.live, pid)
}

// testCfg builds a leaseConfig wired to the fakes. ttl/heartbeat are
// generous so the fake clock controls expiry, not the wall clock.
func testCfg(clk *fakeClock, live *fakeLiveness) leaseConfig {
	return leaseConfig{
		heartbeat:        10 * time.Millisecond,
		ttl:              30 * time.Second, // expressed in fakeClock ns units
		flockRetryBudget: 2 * time.Second,
		nowMono:          clk.now,
		pidStart:         live.get,
		boot:             func() string { return "test-boot-1" },
		killStub:         func(identity, int64) error { return nil },
	}
}

// setupHome points FLEET_HOME at a temp dir + bootstraps it.
func setupHome(t *testing.T) {
	t.Helper()
	t.Setenv("FLEET_HOME", t.TempDir())
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
}

// readEpochFor reads the on-disk epoch record for project.
func readEpochFor(t *testing.T, project string) epochRecord {
	t.Helper()
	paths, err := resolvePaths(project)
	if err != nil {
		t.Fatalf("resolvePaths: %v", err)
	}
	rec, err := readEpoch(paths.epoch)
	if err != nil {
		t.Fatalf("readEpoch: %v", err)
	}
	return rec
}

// holdFlock opens + LOCK_EX-holds coordinator.flock for project via a
// separate fd, simulating a live holder still owning the flock. Returns a
// release func.
func holdFlock(t *testing.T, project string) func() {
	t.Helper()
	paths, err := resolvePaths(project)
	if err != nil {
		t.Fatalf("resolvePaths: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.flock), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	f, err := os.OpenFile(paths.flock, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open flock: %v", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("flock: %v", err)
	}
	var once sync.Once
	rel := func() {
		once.Do(func() {
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			_ = f.Close()
		})
	}
	t.Cleanup(rel)
	return rel
}

// writeEpochRaw writes an epoch record directly (simulating a holder /
// candidate having written it), via the same atomic path the lease uses.
func writeEpochRaw(t *testing.T, project string, rec epochRecord) {
	t.Helper()
	paths, err := resolvePaths(project)
	if err != nil {
		t.Fatalf("resolvePaths: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.epoch), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(paths.epoch, b, 0o644); err != nil {
		t.Fatalf("write epoch: %v", err)
	}
}

// TestLeaderPresent (codex PR2 iter-11 [P2]): LeaderPresent uses the same
// healthy/in-progress predicate AcquireLease uses, not a bare
// state==active check. Healthy active -> true; stale active past TTL ->
// false; fresh fencing takeover -> true; released / no record -> false.
func TestLeaderPresent(t *testing.T) {
	setupHome(t)
	const project = "lp-test"
	clk := &fakeClock{}
	live := newFakeLiveness()
	cfg := testCfg(clk, live)
	const (
		ownerPid   = 7000
		ownerStart = int64(700700)
		candPid    = 8000
		candStart  = int64(800800)
	)
	live.set(ownerPid, ownerStart)
	live.set(candPid, candStart)
	owner := identity{Pid: ownerPid, PidStart: ownerStart, AgentID: "owner", Project: project}
	cand := identity{Pid: candPid, PidStart: candStart, AgentID: "cand", Project: project}

	// No record -> false.
	if leaderPresentWithCfg(project, cfg) {
		t.Error("no record: want false")
	}

	// Healthy active -> true.
	writeEpochRaw(t, project, epochRecord{
		Epoch: 5, State: stateActive, Owner: owner,
		BootID: "test-boot-1", RenewedAtMono: clk.now(),
	})
	if !leaderPresentWithCfg(project, cfg) {
		t.Error("healthy active: want true")
	}

	// Stale active (owner alive but renewed_at past TTL) -> false.
	staleClk := &fakeClock{}
	staleClk.advance(2 * cfg.ttl) // now is well past renewed_at=0
	if leaderPresentWithCfg(project, testCfg(staleClk, live)) {
		t.Error("stale active past TTL: want false (stealable)")
	}

	// Dead owner (pid gone) -> false even within TTL.
	deadLive := newFakeLiveness() // owner/cand NOT set -> dead
	if leaderPresentWithCfg(project, testCfg(clk, deadLive)) {
		t.Error("dead owner: want false")
	}

	// Fresh fencing takeover (live candidate, in budget) -> true.
	writeEpochRaw(t, project, epochRecord{
		Epoch: 6, State: stateFencing, Owner: owner, Candidate: cand,
		BootID: "test-boot-1", RenewedAtMono: clk.now(),
	})
	if !leaderPresentWithCfg(project, cfg) {
		t.Error("fresh fencing takeover: want true")
	}

	// Released -> false.
	writeEpochRaw(t, project, epochRecord{
		Epoch: 7, State: stateReleased, Owner: owner,
		BootID: "test-boot-1", RenewedAtMono: clk.now(),
	})
	if leaderPresentWithCfg(project, cfg) {
		t.Error("released: want false")
	}
}

// TestLeaderPresent_BusyFlockNoEpoch_Booting (codex PR3 iter-3 [P2]): a
// holder grabbed coordinator.flock but has not written coordinator.epoch yet
// (still booting). LeaderPresent must mirror the acquire path's flock-body
// freshness check: a FRESH same-boot live holder reads as present (so a
// duplicate spawn stands down cleanly), a stale/dead/missing body does not.
func TestLeaderPresent_BusyFlockNoEpoch_Booting(t *testing.T) {
	setupHome(t)
	const project = "lp-busy-no-epoch"
	clk := &fakeClock{}
	live := newFakeLiveness()
	cfg := testCfg(clk, live)
	const holderPid, holderStart = 9100, int64(910910)
	live.set(holderPid, holderStart)

	paths, err := resolvePaths(project)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.flock), 0o755); err != nil {
		t.Fatal(err)
	}
	// Hold the flock for the whole test (simulates the booting holder).
	f, gotFlock, err := tryFlock(paths.flock)
	if err != nil || !gotFlock {
		t.Fatalf("tryFlock: ok=%v err=%v", gotFlock, err)
	}
	defer func() { _ = releaseFlock(f) }()

	writeBody := func(b flockBody) {
		raw, _ := json.Marshal(b)
		if e := f.Truncate(0); e != nil {
			t.Fatal(e)
		}
		if _, e := f.Seek(0, 0); e != nil {
			t.Fatal(e)
		}
		if _, e := f.Write(raw); e != nil {
			t.Fatal(e)
		}
		_ = f.Sync()
	}

	// Fresh same-boot live holder, in-budget -> present.
	writeBody(flockBody{Pid: holderPid, PidStart: holderStart, BootID: "test-boot-1", Mono: clk.now()})
	if !leaderPresentWithCfg(project, cfg) {
		t.Error("busy flock + fresh booting holder: want LeaderPresent true")
	}

	// Hung holder (body mono past TTL) -> not present (stealable).
	staleClk := &fakeClock{}
	staleClk.advance(2 * cfg.ttl)
	if leaderPresentWithCfg(project, testCfg(staleClk, live)) {
		t.Error("busy flock + hung holder past TTL: want LeaderPresent false")
	}

	// Dead holder pid -> not present.
	deadLive := newFakeLiveness() // holder not set -> dead
	if leaderPresentWithCfg(project, testCfg(clk, deadLive)) {
		t.Error("busy flock + dead holder: want LeaderPresent false")
	}
}

// TestReleasedHolderBusyFlock_RetriesNotFenced (codex PR2 iter-12 [P2]):
// a `released` epoch with the flock STILL briefly held (the old supervisor
// is mid-Release/cleanup) must be RETRIED — the candidate acquires when
// the flock frees — and must NOT STONITH the releasing supervisor.
func TestReleasedHolderBusyFlock_RetriesNotFenced(t *testing.T) {
	setupHome(t)
	const project = "rel-busy"
	clk := &fakeClock{}
	live := newFakeLiveness()
	cfg := testCfg(clk, live)

	const oldPid = 4242
	const oldStart = int64(424242)
	live.set(oldPid, oldStart)

	// Old holder demoted its record to `released` but still holds the flock.
	writeEpochRaw(t, project, epochRecord{
		Epoch: 9, State: stateReleased,
		Owner:  identity{Pid: oldPid, PidStart: oldStart, AgentID: "old", Project: project},
		BootID: "test-boot-1", RenewedAtMono: clk.now(),
	})
	releaseFlockHeld := holdFlock(t, project)

	// killStub must NEVER fire — STONITH of a cleanly-releasing supervisor
	// is exactly the bug.
	var killed atomic.Bool
	cfg.killStub = func(identity, int64) error { killed.Store(true); return nil }

	// Free the flock shortly after acquire starts (simulates Release()
	// closing the fd mid-cleanup). retryFlock polls every ~20ms within its
	// budget, so this is caught well inside the bound.
	go func() {
		time.Sleep(40 * time.Millisecond)
		releaseFlockHeld()
	}()

	lease, acquired, err := acquireLease(project, "cand", cfg)
	if err != nil {
		t.Fatalf("acquireLease: %v", err)
	}
	if !acquired || lease == nil {
		t.Fatalf("expected the candidate to acquire after the releaser drops the flock; acquired=%v", acquired)
	}
	defer lease.Release()
	if killed.Load() {
		t.Error("killStub fired — must NOT STONITH a cleanly-releasing supervisor")
	}
	// The candidate is now the active owner at the released epoch+1.
	rec := readEpochFor(t, project)
	if rec.State != stateActive {
		t.Errorf("state = %q, want active after acquire", rec.State)
	}
}

// ---- T1 ----

// T1: NB acquire on a healthy heartbeating holder returns
// acquired=false, err=nil immediately (no block).
func TestT1_NBAcquireOnHealthyHolder(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	clk := &fakeClock{}
	live := newFakeLiveness()

	// Holder: pid 4242 alive, holds the flock, fresh active epoch.
	const holderPid = 4242
	const holderStart = int64(222222)
	live.set(holderPid, holderStart)
	holdFlock(t, project)
	writeEpochRaw(t, project, epochRecord{
		Epoch: 5, State: stateActive,
		Owner:  identity{Pid: holderPid, PidStart: holderStart, AgentID: "old", Project: project},
		BootID: "test-boot-1", RenewedAtMono: clk.now(),
	})

	start := time.Now()
	lease, acquired, err := acquireLease(project, "cand", testCfg(clk, live))
	if err != nil {
		t.Fatalf("acquireLease: %v", err)
	}
	if acquired || lease != nil {
		t.Fatalf("expected acquired=false on healthy holder, got acquired=%v lease=%v", acquired, lease)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("acquire blocked %s; should return immediately", elapsed)
	}
}

// ---- T2 ----

// T2: holder death frees the flock (kernel releases on death); a
// candidate then AcquireLease succeeds and bumps the epoch.
func TestT2_LeaseReleasedOnHolderDeath(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	clk := &fakeClock{}
	live := newFakeLiveness()

	// Simulate a dead holder: epoch record exists but the flock is FREE
	// (kernel released it on death) and the pid is gone.
	writeEpochRaw(t, project, epochRecord{
		Epoch: 7, State: stateActive,
		Owner:  identity{Pid: 9999, PidStart: 333333, AgentID: "dead", Project: project},
		BootID: "test-boot-1", RenewedAtMono: clk.now(),
	})

	lease, acquired, err := acquireLease(project, "cand", testCfg(clk, live))
	if err != nil {
		t.Fatalf("acquireLease: %v", err)
	}
	if !acquired || lease == nil {
		t.Fatalf("expected acquired=true on free flock, got acquired=%v", acquired)
	}
	defer lease.Release()

	rec := readEpochFor(t, project)
	if rec.Epoch != 8 {
		t.Fatalf("expected epoch bumped to 8, got %d", rec.Epoch)
	}
	if rec.State != stateActive || rec.Owner.AgentID != "cand" {
		t.Fatalf("expected active candidate owner, got state=%s owner=%+v", rec.State, rec.Owner)
	}
}

// ---- T3 ----

// T3: takeover on TTL expiry of a DEAD holder whose flock is (in this
// simulated harness) still held. Order must be fence (epoch CAS to
// fencing) -> kill -> acquire; the kill-stub releases the simulated
// holder's flock so the candidate can acquire it after the kill.
//
// KP6 rewrite (DESIGN-coord-no-auto-kill): the original T3 holder was
// hung-but-ALIVE and the takeover shot it. A staleness heuristic may no
// longer kill a live coordinator — the live-holder shape now ABORTS
// pre-fence (TestKP6_LiveOwnerAbortsTakeoverPreFence). T3 keeps pinning
// the fence->kill->acquire order on the provably-dead path.
func TestT3_TakeoverOnTTLExpiryHungHolder(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	clk := &fakeClock{}
	live := newFakeLiveness()

	const holderPid = 4242
	const holderStart = int64(222222)
	// Holder pid NOT in the live map: provably dead (all-dead gate passes).
	relHolder := holdFlock(t, project)

	// Active epoch with renewed_at frozen at t=0.
	writeEpochRaw(t, project, epochRecord{
		Epoch: 5, State: stateActive,
		Owner:  identity{Pid: holderPid, PidStart: holderStart, AgentID: "old", Project: project},
		BootID: "test-boot-1", RenewedAtMono: 0,
	})

	cfg := testCfg(clk, live)
	var fenceObservedBeforeKill atomic.Bool
	cfg.killStub = func(owner identity, _ int64) error {
		// At kill time the epoch must already be FENCING (fence-before-kill).
		rec := readEpochFor(t, project)
		if rec.State == stateFencing {
			fenceObservedBeforeKill.Store(true)
		}
		// Simulate STONITH: holder dies -> kernel releases its flock.
		live.kill(owner.Pid)
		relHolder()
		return nil
	}

	// Advance the fake clock past TTL so the holder reads as hung.
	clk.advance(31 * time.Second)

	lease, acquired, err := acquireLease(project, "cand", cfg)
	if err != nil {
		t.Fatalf("acquireLease (takeover): %v", err)
	}
	if !acquired || lease == nil {
		t.Fatalf("expected takeover to acquire, got acquired=%v", acquired)
	}
	defer lease.Release()

	if !fenceObservedBeforeKill.Load() {
		t.Fatal("epoch was not in fencing state at kill time — fence-before-kill order violated")
	}
	rec := readEpochFor(t, project)
	if rec.State != stateActive || rec.Owner.AgentID != "cand" {
		t.Fatalf("expected active candidate after takeover, got state=%s owner=%+v", rec.State, rec.Owner)
	}
	if rec.Epoch != 6 {
		t.Fatalf("expected epoch 6 (5 fenced->6), got %d", rec.Epoch)
	}
}

// ---- T4 ----

// T4: pid-reuse-safe dead detection. The recorded pid now belongs to an
// unrelated live process (recycled pid) whose pid_start differs; the
// holder must read as dead (start-time mismatch), not live.
func TestT4_PidReuseSafeDeadDetection(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	clk := &fakeClock{}
	live := newFakeLiveness()

	const recycledPid = 4242
	// The pid is LIVE but with a DIFFERENT start time than recorded.
	live.set(recycledPid, 999999)
	// Flock is free (the original holder died; its flock was released).
	writeEpochRaw(t, project, epochRecord{
		Epoch: 5, State: stateActive,
		Owner:  identity{Pid: recycledPid, PidStart: int64(222222), AgentID: "old", Project: project},
		BootID: "test-boot-1", RenewedAtMono: clk.now(),
	})

	lease, acquired, err := acquireLease(project, "cand", testCfg(clk, live))
	if err != nil {
		t.Fatalf("acquireLease: %v", err)
	}
	if !acquired {
		t.Fatal("expected acquire (recycled pid => dead via start-time mismatch)")
	}
	defer lease.Release()

	// Also assert the predicate directly: pidAlive must be false for the
	// start-time mismatch.
	l := &Lease{cfg: testCfg(clk, live)}
	if l.pidAlive(identity{Pid: recycledPid, PidStart: int64(222222)}) {
		t.Fatal("pidAlive must be false on pid_start mismatch (recycled pid)")
	}
}

// ---- T5 ----

// T5: no takeover on a healthy heartbeat (now-renewed_at <= TTL). The
// candidate stands down without entering takeover.
func TestT5_NoTakeoverOnHealthyHeartbeat(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	clk := &fakeClock{}
	live := newFakeLiveness()

	const holderPid = 4242
	live.set(holderPid, int64(222222))
	holdFlock(t, project)
	writeEpochRaw(t, project, epochRecord{
		Epoch: 5, State: stateActive,
		Owner:  identity{Pid: holderPid, PidStart: int64(222222), AgentID: "old", Project: project},
		BootID: "test-boot-1", RenewedAtMono: clk.now(),
	})

	cfg := testCfg(clk, live)
	var killed atomic.Bool
	cfg.killStub = func(identity, int64) error { killed.Store(true); return nil }

	// Advance only within TTL.
	clk.advance(10 * time.Second)

	lease, acquired, err := acquireLease(project, "cand", cfg)
	if err != nil {
		t.Fatalf("acquireLease: %v", err)
	}
	if acquired {
		t.Fatal("expected stand-down on healthy heartbeat")
	}
	_ = lease
	if killed.Load() {
		t.Fatal("kill must not fire on a healthy holder")
	}
}

// ---- T6 ----

// T6: epoch fencing rejects a zombie token. Leader captured a token at
// epoch=5; a candidate bumps to epoch=6 on disk; the old token's
// StillOwned() is false and a current-epoch check rejects it.
func TestT6_EpochFencingRejectsZombieToken(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	clk := &fakeClock{}
	live := newFakeLiveness()
	cfg := testCfg(clk, live)

	paths, _ := resolvePaths(project)
	// Old leader's token @ epoch 5.
	tok := LeaseToken{
		Epoch: 5, Project: project, Pid: 4242, PidStart: 222222, AgentID: "old",
		paths: paths, boot: "test-boot-1", cfg: cfg,
	}
	// Disk: a candidate fenced to epoch 6, owner=new.
	writeEpochRaw(t, project, epochRecord{
		Epoch: 6, State: stateActive,
		Owner:  identity{Pid: 7777, PidStart: 444444, AgentID: "new", Project: project},
		BootID: "test-boot-1", RenewedAtMono: clk.now(),
	})

	if tok.StillOwned() {
		t.Fatal("stale-epoch token must NOT be StillOwned")
	}

	// A token whose epoch == current but identity != owner must also fail
	// (epoch-only is insufficient — full-identity check).
	imposter := LeaseToken{
		Epoch: 6, Project: project, Pid: 1234, PidStart: 555555, AgentID: "imposter",
		paths: paths, boot: "test-boot-1", cfg: cfg,
	}
	if imposter.StillOwned() {
		t.Fatal("current-epoch token from a non-owner must NOT be StillOwned")
	}
}

// ---- T8 ----

// T8: bounded per-record acquire times out within the budget and the
// error names the stale holder pid stamped in the body. (Exercises
// internal/state's bounded acquire via LockProjectStateTimeout.)
func TestT8_BoundedPerRecordAcquireTimesOut(t *testing.T) {
	setupHome(t)
	const project = "rainier"

	// Hold the state lock from a separate fd (OS-level contention) and
	// stamp a holder pid into the body.
	path, err := state.ProjectStateLockPath(project)
	if err != nil {
		t.Fatalf("ProjectStateLockPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	holder, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = holder.Close() }()
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("flock holder: %v", err)
	}
	if _, err := holder.WriteString("pid=98765 ts=1\n"); err != nil {
		t.Fatalf("stamp body: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Flock(int(holder.Fd()), syscall.LOCK_UN) })

	start := time.Now()
	rel, err := state.LockProjectStateTimeout(project, 200*time.Millisecond)
	elapsed := time.Since(start)
	if err == nil {
		rel()
		t.Fatal("expected timeout error while holder owns the lock")
	}
	if elapsed > 600*time.Millisecond {
		t.Fatalf("bounded acquire took %s; should time out near 200ms", elapsed)
	}
	if msg := err.Error(); !contains(msg, "98765") {
		t.Fatalf("timeout error should name the stale holder pid 98765, got: %s", msg)
	}
}

// ---- T23 ----

// T23: two simultaneous candidates -> exactly one becomes leader. Both
// observe a dead holder (free flock + dead pid) and race acquireLease;
// exactly one wins, the other stands down/loses. Never two active leaders.
func TestT23_TwoCandidatesOneLeader(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	clk := &fakeClock{}
	live := newFakeLiveness()

	// Dead holder: free flock, pid gone.
	writeEpochRaw(t, project, epochRecord{
		Epoch: 5, State: stateActive,
		Owner:  identity{Pid: 9999, PidStart: 333333, AgentID: "dead", Project: project},
		BootID: "test-boot-1", RenewedAtMono: clk.now(),
	})

	// A short flock-retry budget keeps the loser's takeover path fast when
	// it briefly contends the flock the winner already holds.
	cfg := testCfg(clk, live)
	cfg.flockRetryBudget = 200 * time.Millisecond

	candIDs := []string{"candA", "candB"}
	var winners atomic.Int32
	var wg sync.WaitGroup
	leases := make([]*Lease, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			lease, acquired, err := acquireLease(project, candIDs[idx], cfg)
			if err != nil {
				return
			}
			if acquired {
				winners.Add(1)
				leases[idx] = lease
			}
		}(i)
	}
	wg.Wait()
	for _, l := range leases {
		if l != nil {
			defer l.Release()
		}
	}

	if got := winners.Load(); got != 1 {
		t.Fatalf("expected exactly 1 leader, got %d", got)
	}
	rec := readEpochFor(t, project)
	if rec.State != stateActive {
		t.Fatalf("expected active state after race, got %s", rec.State)
	}
}

// ---- T24 ----

// T24: a wall-clock jump does NOT expire a healthy leader. The lease uses
// monotonic elapsed; the fake monotonic clock is unchanged while the wall
// clock "jumps" (we never advance the fake mono clock past TTL).
func TestT24_ClockJumpDoesNotExpireLeader(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	clk := &fakeClock{}
	live := newFakeLiveness()

	const holderPid = 4242
	live.set(holderPid, int64(222222))
	holdFlock(t, project)
	// renewed_at stamped at mono=0; wall-clock value is way in the past.
	writeEpochRaw(t, project, epochRecord{
		Epoch: 5, State: stateActive,
		Owner:         identity{Pid: holderPid, PidStart: int64(222222), AgentID: "old", Project: project},
		BootID:        "test-boot-1",
		RenewedAtMono: 0,
		RenewedAtWall: time.Now().Add(-72 * time.Hour).UnixNano(), // huge wall-clock skew
	})

	// Monotonic elapsed stays within TTL (we do NOT advance the fake mono
	// clock), even though the wall clock is 3 days off.
	lease, acquired, err := acquireLease(project, "cand", testCfg(clk, live))
	if err != nil {
		t.Fatalf("acquireLease: %v", err)
	}
	if acquired {
		t.Fatal("wall-clock skew must NOT expire a monotonically-healthy leader")
	}
	_ = lease
}

// ---- T29 ----

// T29: a zombie heartbeat cannot roll the epoch back. Leader@5 "paused";
// a candidate fences to 6 on disk; the old leader resumes + heartbeats —
// the heartbeat CAS sees epoch != 5, refuses to write, self-demotes; the
// on-disk epoch stays 6.
func TestT29_ZombieHeartbeatCannotRollEpochBack(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	clk := &fakeClock{}
	live := newFakeLiveness()
	cfg := testCfg(clk, live)

	paths, _ := resolvePaths(project)
	// Build a Lease as if the old leader held epoch 5.
	old := &Lease{
		cfg:   cfg,
		paths: paths,
		self:  identity{Pid: os.Getpid(), PidStart: selfStart, AgentID: "old", Project: project},
		host:  "h", boot: "test-boot-1", epoch: 5,
	}
	// A candidate fenced to epoch 6 (owner=new) on disk.
	writeEpochRaw(t, project, epochRecord{
		Epoch: 6, State: stateActive,
		Owner:  identity{Pid: 7777, PidStart: 444444, AgentID: "new", Project: project},
		BootID: "test-boot-1", RenewedAtMono: clk.now(),
	})

	ok, err := old.heartbeatOnce()
	if err != nil {
		t.Fatalf("heartbeatOnce: %v", err)
	}
	if ok {
		t.Fatal("zombie heartbeat must self-demote (ok=false), not renew")
	}
	rec := readEpochFor(t, project)
	if rec.Epoch != 6 || rec.Owner.AgentID != "new" {
		t.Fatalf("epoch must stay 6/new after refused zombie heartbeat, got epoch=%d owner=%s",
			rec.Epoch, rec.Owner.AgentID)
	}
}

// ---- T34 ----

// T34: two-phase takeover — crash between fence and acquire. A candidate
// wrote a fencing record (owner=OLD, candidate=me) then died before
// acquiring the flock. The record still names OLD as owner; a next
// candidate resumes idempotently (re-fence/kill OLD, acquire) and never
// names a non-holder as owner.
func TestT34_TwoPhaseCrashBetweenFenceAndAcquire(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	clk := &fakeClock{}
	live := newFakeLiveness()

	const oldPid = 4242
	const oldStart = int64(222222)
	// OLD holder: flock still held in this simulated harness, pid DEAD.
	// (KP6 rewrite: a live OLD now aborts the resume pre-fence — see
	// TestKP6_LiveOwnerAbortsTakeoverPreFence; the two-phase resume this
	// test pins requires every prospective target provably dead.)
	relOld := holdFlock(t, project)

	// A dead candidate left a stalled fencing record naming OLD as owner.
	deadCand := identity{Pid: 8888, PidStart: 555555, AgentID: "deadcand", Project: project}
	writeEpochRaw(t, project, epochRecord{
		Epoch: 6, State: stateFencing,
		Owner:         identity{Pid: oldPid, PidStart: oldStart, AgentID: "old", Project: project},
		Candidate:     deadCand,
		BootID:        "test-boot-1",
		RenewedAtMono: 0, // stale: candidate died long ago
	})

	cfg := testCfg(clk, live)
	cfg.killStub = func(owner identity, _ int64) error {
		if owner.AgentID != "old" {
			t.Errorf("kill must target OLD owner, got %s", owner.AgentID)
		}
		live.kill(owner.Pid)
		relOld()
		return nil
	}

	// Advance clock so the stalled fencing record is past TTL (resumable).
	clk.advance(31 * time.Second)

	lease, acquired, err := acquireLease(project, "cand2", cfg)
	if err != nil {
		t.Fatalf("acquireLease (resume): %v", err)
	}
	if !acquired {
		t.Fatal("next candidate must resume the stalled takeover and acquire")
	}
	defer lease.Release()

	rec := readEpochFor(t, project)
	if rec.Owner.AgentID == "deadcand" {
		t.Fatal("record must NEVER name the dead candidate as owner")
	}
	if rec.State != stateActive || rec.Owner.AgentID != "cand2" {
		t.Fatalf("expected active cand2 owner after resume, got state=%s owner=%s",
			rec.State, rec.Owner.AgentID)
	}
}

// ---- T37 ----

// T37: free-flock acquire-to-epoch window race. OLD dead -> A NB-acquires
// the free flock; B bumps the epoch before A's active-CAS. A's CAS fails
// -> A releases the flock + restarts (no heartbeat/kill/engine), and the
// system converges to exactly ONE active leader. We exercise A's
// release-on-CAS-fail directly: A acquires the flock, then B advances the
// epoch on disk, then A's casToActiveAfterFlock must return ok=false
// (forcing release+retry).
func TestT37_FreeFlockAcquireToEpochWindowRace(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	clk := &fakeClock{}
	live := newFakeLiveness()
	cfg := testCfg(clk, live)

	// Free flock + a stale active record (epoch 5) — A reads epoch 5.
	writeEpochRaw(t, project, epochRecord{
		Epoch: 5, State: stateActive,
		Owner:  identity{Pid: 9999, PidStart: 333333, AgentID: "dead", Project: project},
		BootID: "test-boot-1", RenewedAtMono: clk.now(),
	})

	paths, _ := resolvePaths(project)
	a := &Lease{
		cfg: cfg, paths: paths,
		self: identity{Pid: os.Getpid(), PidStart: selfStart, AgentID: "A", Project: project},
		host: "h", boot: "test-boot-1",
	}
	// A acquires the free flock.
	f, gotFlock, err := tryFlock(paths.flock)
	if err != nil || !gotFlock {
		t.Fatalf("A should acquire free flock: got=%v err=%v", gotFlock, err)
	}
	defer func() { _ = releaseFlock(f) }()

	// B advances the epoch on disk (a live owner @ epoch 6) before A's CAS.
	const bPid = 7777
	const bStart = int64(444444)
	live.set(bPid, bStart)
	writeEpochRaw(t, project, epochRecord{
		Epoch: 6, State: stateActive,
		Owner:  identity{Pid: bPid, PidStart: bStart, AgentID: "B", Project: project},
		BootID: "test-boot-1", RenewedAtMono: clk.now(),
	})

	// A observed epoch 5 at flock acquire; B advanced the on-disk epoch to
	// 6 in the window. A's snapshot-CAS (observedEpoch=5) must FAIL ->
	// ok=false (release+retry).
	ok, err := a.casToActiveAfterFlock(5)
	if err != nil {
		t.Fatalf("casToActiveAfterFlock: %v", err)
	}
	if ok {
		t.Fatal("A's active-CAS must fail when a live owner advanced the epoch in the window")
	}
	// On-disk owner must remain B (A must not have written itself).
	rec := readEpochFor(t, project)
	if rec.Owner.AgentID != "B" || rec.Epoch != 6 {
		t.Fatalf("epoch must remain B@6 after A's failed CAS, got %s@%d", rec.Owner.AgentID, rec.Epoch)
	}
}

// ---- T38 ----

// T38: a stalled-takeover record (fencing, renewed_at older than TTL,
// candidate died mid-takeover) is resumable by the next candidate, with
// no double-leader and no permanent stall. (Distinct from T34 in that the
// OLD holder is already dead, so the resume completes via the free flock.)
func TestT38_StalledTakeoverResumable(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	clk := &fakeClock{}
	live := newFakeLiveness()

	// OLD holder already dead: flock free, pid gone. Stalled fencing record.
	deadCand := identity{Pid: 8888, PidStart: 555555, AgentID: "deadcand", Project: project}
	writeEpochRaw(t, project, epochRecord{
		Epoch: 6, State: stateFencing,
		Owner:         identity{Pid: 9999, PidStart: 333333, AgentID: "old", Project: project},
		Candidate:     deadCand,
		BootID:        "test-boot-1",
		RenewedAtMono: 0,
	})

	cfg := testCfg(clk, live)
	clk.advance(31 * time.Second) // stalled fencing record is past TTL

	lease, acquired, err := acquireLease(project, "cand2", cfg)
	if err != nil {
		t.Fatalf("acquireLease (resume free-flock): %v", err)
	}
	if !acquired {
		t.Fatal("next candidate must resume the stalled takeover via the free flock")
	}
	defer lease.Release()

	rec := readEpochFor(t, project)
	if rec.State != stateActive || rec.Owner.AgentID != "cand2" {
		t.Fatalf("expected active cand2 after resume, got state=%s owner=%s", rec.State, rec.Owner.AgentID)
	}
	if rec.Owner.AgentID == "deadcand" {
		t.Fatal("record must never name the dead candidate as owner")
	}
}

// ---- Heartbeat happy-path ----

// TestHeartbeatRenewsRenewedAt: a healthy heartbeat updates renewed_at and
// preserves the epoch (the positive complement to T29).
func TestHeartbeatRenewsRenewedAt(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	clk := &fakeClock{}
	live := newFakeLiveness()
	cfg := testCfg(clk, live)
	paths, _ := resolvePaths(project)

	l := &Lease{
		cfg: cfg, paths: paths,
		self: identity{Pid: os.Getpid(), PidStart: selfStart, AgentID: "me", Project: project},
		host: "h", boot: "test-boot-1", epoch: 9,
	}
	writeEpochRaw(t, project, epochRecord{
		Epoch: 9, State: stateActive, Owner: l.self,
		BootID: "test-boot-1", RenewedAtMono: 0,
	})
	clk.advance(5 * time.Second)
	ok, err := l.heartbeatOnce()
	if err != nil || !ok {
		t.Fatalf("healthy heartbeat should renew, got ok=%v err=%v", ok, err)
	}
	rec := readEpochFor(t, project)
	if rec.Epoch != 9 {
		t.Fatalf("heartbeat must preserve the epoch, got %d", rec.Epoch)
	}
	if rec.RenewedAtMono != int64(5*time.Second) {
		t.Fatalf("renewed_at_mono should advance to 5s, got %d", rec.RenewedAtMono)
	}
}

func TestIdleLeaderHeartbeatRenewsAcrossIntervals(t *testing.T) {
	setupHome(t)
	const project = "idle-renew"
	clk := &fakeClock{}
	live := newFakeLiveness()
	cfg := testCfg(clk, live)
	paths, _ := resolvePaths(project)

	l := &Lease{
		cfg: cfg, paths: paths,
		self: identity{Pid: os.Getpid(), PidStart: selfStart, AgentID: "idle-coord", Project: project},
		host: "h", boot: "test-boot-1", epoch: 3,
	}
	writeEpochRaw(t, project, epochRecord{
		Epoch: 3, State: stateActive, Owner: l.self,
		BootID: "test-boot-1", RenewedAtMono: 0,
	})

	for i := 1; i <= 5; i++ {
		clk.advance(cfg.heartbeat)
		ok, err := l.heartbeatOnce()
		if err != nil || !ok {
			t.Fatalf("heartbeat %d should renew an idle leader, got ok=%v err=%v", i, ok, err)
		}
		rec := readEpochFor(t, project)
		want := int64(time.Duration(i) * cfg.heartbeat)
		if rec.RenewedAtMono != want {
			t.Fatalf("heartbeat %d renewed_at_mono = %d, want %d", i, rec.RenewedAtMono, want)
		}
		if rec.Epoch != 3 || rec.Owner.AgentID != "idle-coord" {
			t.Fatalf("heartbeat %d changed lease identity: epoch=%d owner=%s", i, rec.Epoch, rec.Owner.AgentID)
		}
	}
}

// ---- Failover gate ----

// TestAcquireLeaseRefusesWhenFailoverDisabled: T41 — with the PR4 flip the
// flag is ON by default, so the ONLY way to get the legacy bare-child path
// is to EXPLICITLY disable it (=0). The entry point then refuses with
// ErrFailoverDisabled so the caller runs as pre-lease. Proves the flip is
// reversible.
func TestAcquireLeaseRefusesWhenFailoverDisabled(t *testing.T) {
	setupHome(t)
	for _, off := range []string{"0", "false", "off", "no", "FALSE", "Off"} {
		t.Run(off, func(t *testing.T) {
			t.Setenv(FailoverEnvVar, off)
			_, _, err := AcquireLease("rainier", "cand")
			if err == nil {
				t.Fatalf("AcquireLease must refuse when FLEET_LEASE_FAILOVER=%q", off)
			}
			if err.Error() != ErrFailoverDisabled.Error() {
				t.Fatalf("expected ErrFailoverDisabled, got %v", err)
			}
		})
	}
}

// TestAcquireLeaseDefaultsOnWhenUnset: T40 — with no FLEET_LEASE_FAILOVER
// set (the PR4 default), the lease path is LIVE: the first-ever leader
// acquires epoch 1 rather than getting ErrFailoverDisabled. The "not yet
// supported" refusal is gone for the default case.
func TestAcquireLeaseDefaultsOnWhenUnset(t *testing.T) {
	setupHome(t)
	os.Unsetenv(FailoverEnvVar) //nolint:errcheck // ensure truly unset
	lease, acquired, err := AcquireLease("rainier", "cand")
	if err != nil {
		t.Fatalf("AcquireLease with flag UNSET must run the live path (default ON), got err=%v", err)
	}
	if !acquired || lease == nil {
		t.Fatalf("first-ever leader should acquire; acquired=%v lease=%v", acquired, lease)
	}
	t.Cleanup(lease.Release)
	if lease.epoch != 1 {
		t.Fatalf("first-ever epoch want 1, got %d", lease.epoch)
	}
}

// TestParseFailoverTriState exercises the single source-of-truth parser
// directly: explicit disable tokens -> OFF; everything else -> ON.
func TestParseFailoverTriState(t *testing.T) {
	off := []string{"0", "false", "off", "no", " 0 ", "FALSE", "Off", "NO"}
	on := []string{"", "1", "true", "yes", "on", "anything", " 1 ", "enabled"}
	for _, v := range off {
		if parseFailover(v) {
			t.Errorf("parseFailover(%q) = true, want false", v)
		}
	}
	for _, v := range on {
		if !parseFailover(v) {
			t.Errorf("parseFailover(%q) = false, want true", v)
		}
	}
}

// TestAcquireLeaseEnabledByEnv: with the flag on, the public entry point
// runs the real path (first-ever leader acquires epoch 1).
func TestAcquireLeaseEnabledByEnv(t *testing.T) {
	setupHome(t)
	t.Setenv(FailoverEnvVar, "1")
	lease, acquired, err := AcquireLease("rainier", "cand")
	if err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}
	if !acquired || lease == nil {
		t.Fatalf("expected first-ever acquire, got acquired=%v", acquired)
	}
	defer lease.Release()
	rec := readEpochFor(t, "rainier")
	if rec.Epoch != 1 || rec.State != stateActive {
		t.Fatalf("first leader should be epoch 1 active, got %d/%s", rec.Epoch, rec.State)
	}
}

// ---- Platform pinning: P1, P2, P3 ----

// P1: pid_start read matches reality — non-zero, stable across repeated
// reads in the same process, and differs from a freshly-spawned child.
func TestP1_PidStartReadMatchesReality(t *testing.T) {
	self := os.Getpid()
	s1, err := pidStartNanos(self)
	if err != nil {
		t.Fatalf("pidStartNanos(self): %v", err)
	}
	if s1 == 0 {
		t.Fatal("self pid_start should be non-zero")
	}
	s2, err := pidStartNanos(self)
	if err != nil {
		t.Fatalf("pidStartNanos(self) again: %v", err)
	}
	if s1 != s2 {
		t.Fatalf("pid_start must be stable across reads: %d != %d", s1, s2)
	}

	// A freshly-spawned child started later -> different (>=) start time.
	cmd := exec.Command("sleep", "5")
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn child: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	cs, err := pidStartNanos(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("pidStartNanos(child): %v", err)
	}
	if cs == 0 {
		t.Fatal("child pid_start should be non-zero")
	}
	// The child started after this process; on both platforms its start
	// time must be >= ours. (Equality is possible at coarse clock-tick
	// resolution on Linux, so we assert >= and pid difference rather than
	// strict >.)
	if cs < s1 {
		t.Fatalf("child pid_start (%d) should be >= parent (%d)", cs, s1)
	}
	if cmd.Process.Pid == self {
		t.Fatal("child pid must differ from parent pid")
	}
}

// P2: monotonic elapsed is jump-immune — two reads yield a non-negative
// delta reflecting real elapsed time, independent of the wall clock.
func TestP2_MonotonicElapsedNonNegative(t *testing.T) {
	a := monotonicNanos()
	if a <= 0 {
		t.Fatalf("monotonicNanos should be positive, got %d", a)
	}
	// A tiny real sleep guarantees forward progress without asserting a
	// specific duration (no timing-flaky assertion).
	time.Sleep(1 * time.Millisecond)
	b := monotonicNanos()
	if b < a {
		t.Fatalf("monotonic clock went backward: %d -> %d", a, b)
	}
}

// P3: the boot-id guard expires a cross-boot record. A record carrying a
// boot-id != the current host boot-id is treated as expired/stealable.
func TestP3_BootIDGuardExpiresCrossBootRecord(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	clk := &fakeClock{}
	live := newFakeLiveness()

	const holderPid = 4242
	live.set(holderPid, int64(222222))
	// Record from a DIFFERENT boot — fresh renewed_at, live pid, within
	// TTL; only the boot-id mismatch must make it stealable.
	rec := epochRecord{
		Epoch: 5, State: stateActive,
		Owner:         identity{Pid: holderPid, PidStart: int64(222222), AgentID: "old", Project: project},
		BootID:        "PREVIOUS-BOOT",
		RenewedAtMono: clk.now(),
	}

	// holderHealthy must be false purely on the boot-id guard, regardless
	// of TTL/pid — assert the predicate directly (no takeover I/O needed).
	cfg := testCfg(clk, live) // boot returns "test-boot-1" != "PREVIOUS-BOOT"
	l := &Lease{cfg: cfg, boot: "test-boot-1"}
	if l.holderHealthy(rec) {
		t.Fatal("a cross-boot record must be treated as expired (not healthy)")
	}
	// And a SAME-boot record with the same data must be healthy (control).
	rec.BootID = "test-boot-1"
	if !l.holderHealthy(rec) {
		t.Fatal("a same-boot fresh record with a live pid should be healthy")
	}
}

// ---- small helpers ----

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// ---- codex iter-3 [P2] #1: reacquire after a released flock ----

// A holder that calls Release frees the flock but its still-running PID is
// briefly recorded as the active owner. A new candidate that grabs the
// free flock must WIN (the freed flock is proof the old holder stepped
// down) — not loop until maxAttempts because the old PID is still alive.
func TestReacquireAfterReleasedFlockWithLiveOldPid(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	clk := &fakeClock{}
	live := newFakeLiveness()

	// OLD holder: PID still ALIVE, fresh renewed_at, but the flock is FREE
	// (it called Release). The active record still names OLD.
	const oldPid = 4242
	const oldStart = int64(222222)
	live.set(oldPid, oldStart)
	writeEpochRaw(t, project, epochRecord{
		Epoch: 5, State: stateActive,
		Owner:  identity{Pid: oldPid, PidStart: oldStart, AgentID: "old", Project: project},
		BootID: "test-boot-1", RenewedAtMono: clk.now(),
	})

	lease, acquired, err := acquireLease(project, "cand", testCfg(clk, live))
	if err != nil {
		t.Fatalf("acquireLease must succeed on a freed flock even with a live old PID, got: %v", err)
	}
	if !acquired || lease == nil {
		t.Fatalf("expected acquire on freed flock, got acquired=%v", acquired)
	}
	defer lease.Release()
	rec := readEpochFor(t, project)
	if rec.Epoch != 6 || rec.Owner.AgentID != "cand" {
		t.Fatalf("expected cand@6 after reacquire, got %s@%d", rec.Owner.AgentID, rec.Epoch)
	}
}

// ---- codex iter-3 [P2] #2: don't steal a fresh in-progress fencing ----

// A busy flock with a FRESH fencing record (live candidate, within TTL)
// means another candidate is mid-takeover. A new candidate must stand
// down (acquired=false), not barge in and re-fence — that would make
// candidates invalidate each other's CAS.
func TestDoesNotStealFreshInProgressFencing(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	clk := &fakeClock{}
	live := newFakeLiveness()

	// OLD holder still holds the flock (hung). A live candidate just
	// started a takeover: fresh fencing record, candidate alive, within TTL.
	const oldPid = 4242
	live.set(oldPid, int64(222222))
	holdFlock(t, project)
	const liveCandPid = 6000
	live.set(liveCandPid, int64(666666))
	writeEpochRaw(t, project, epochRecord{
		Epoch: 6, State: stateFencing,
		Owner:         identity{Pid: oldPid, PidStart: int64(222222), AgentID: "old", Project: project},
		Candidate:     identity{Pid: liveCandPid, PidStart: int64(666666), AgentID: "cand1", Project: project},
		BootID:        "test-boot-1",
		RenewedAtMono: clk.now(), // fresh
	})

	cfg := testCfg(clk, live)
	var killed atomic.Bool
	cfg.killStub = func(identity, int64) error { killed.Store(true); return nil }

	clk.advance(5 * time.Second) // still within TTL

	lease, acquired, err := acquireLease(project, "cand2", cfg)
	if err != nil {
		t.Fatalf("acquireLease: %v", err)
	}
	if acquired {
		t.Fatal("a 2nd candidate must NOT steal a fresh in-progress fencing record")
	}
	_ = lease
	if killed.Load() {
		t.Fatal("must not re-fence/kill while a live candidate's takeover is in progress")
	}
	// On-disk record must be untouched (still cand1's fencing @ epoch 6).
	rec := readEpochFor(t, project)
	if rec.Epoch != 6 || rec.Candidate.AgentID != "cand1" {
		t.Fatalf("fresh fencing record must be untouched, got epoch=%d candidate=%s",
			rec.Epoch, rec.Candidate.AgentID)
	}
}

// A fencing record whose CANDIDATE is dead (but renewed_at still fresh) IS
// resumable — the candidate crashed mid-takeover before the TTL elapsed.
func TestResumesFencingWhenCandidateDead(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	clk := &fakeClock{}
	live := newFakeLiveness()

	// OLD already dead: flock FREE, pid gone. Fencing record names a DEAD
	// candidate but renewed_at is fresh (died right after writing it).
	writeEpochRaw(t, project, epochRecord{
		Epoch: 6, State: stateFencing,
		Owner:         identity{Pid: 9999, PidStart: 333333, AgentID: "old", Project: project},
		Candidate:     identity{Pid: 8888, PidStart: 555555, AgentID: "deadcand", Project: project},
		BootID:        "test-boot-1",
		RenewedAtMono: clk.now(), // fresh, but candidate is dead
	})

	lease, acquired, err := acquireLease(project, "cand2", testCfg(clk, live))
	if err != nil {
		t.Fatalf("acquireLease: %v", err)
	}
	if !acquired {
		t.Fatal("a fencing record with a DEAD candidate must be resumable even if fresh")
	}
	defer lease.Release()
	rec := readEpochFor(t, project)
	if rec.State != stateActive || rec.Owner.AgentID != "cand2" {
		t.Fatalf("expected active cand2 after resuming dead-candidate fencing, got %s/%s",
			rec.State, rec.Owner.AgentID)
	}
}

// ---- codex iter-4 [P2]: StillOwned rejects a self-expired token ----

// A leader that paused past its OWN TTL and wakes before any candidate has
// fenced it still reads its own active epoch+owner on disk. StillOwned()
// must reject it (self-demote at the boundary) on the stale-renewed_at and
// cross-boot clauses — not only after another candidate bumps the epoch.
func TestStillOwnedRejectsSelfExpiredToken(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	clk := &fakeClock{}
	live := newFakeLiveness()
	cfg := testCfg(clk, live)
	paths, _ := resolvePaths(project)

	owner := identity{Pid: 4242, PidStart: 222222, AgentID: "me", Project: project}
	// On disk: still MY active record @ epoch 5, renewed at mono=0.
	writeEpochRaw(t, project, epochRecord{
		Epoch: 5, State: stateActive, Owner: owner,
		BootID: "test-boot-1", RenewedAtMono: 0,
	})
	tok := LeaseToken{
		Epoch: 5, Project: project, Pid: 4242, PidStart: 222222, AgentID: "me",
		paths: paths, boot: "test-boot-1", cfg: cfg,
	}

	// Fresh: within TTL -> still owned (control).
	if !tok.StillOwned() {
		t.Fatal("a fresh in-TTL token should be StillOwned")
	}

	// Advance the monotonic clock past TTL without anyone fencing us.
	clk.advance(31 * time.Second)
	if tok.StillOwned() {
		t.Fatal("a self-expired token (renewed_at past TTL) must NOT be StillOwned")
	}

	// Cross-boot record is also rejected regardless of TTL.
	clk2 := &fakeClock{}
	cfg2 := testCfg(clk2, live)
	tokOtherBoot := LeaseToken{
		Epoch: 5, Project: project, Pid: 4242, PidStart: 222222, AgentID: "me",
		paths: paths, boot: "DIFFERENT-BOOT", cfg: cfg2,
	}
	if tokOtherBoot.StillOwned() {
		t.Fatal("a token whose boot != record boot must NOT be StillOwned")
	}
}

func TestCurrentOwnerReturnsActiveOwnerTuple(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	clk := &fakeClock{}
	live := newFakeLiveness()
	cfg := testCfg(clk, live)
	const (
		ownerPid   = 4242
		ownerStart = int64(222222)
	)
	live.set(ownerPid, ownerStart) // owner process alive

	// HEALTHY active owner (same boot, within TTL, pid alive) -> deliverable.
	writeEpochRaw(t, project, epochRecord{
		Epoch: 5,
		State: stateActive,
		Owner: identity{
			Pid:      ownerPid,
			PidStart: ownerStart,
			AgentID:  "owner1",
			Project:  project,
		},
		BootID:        "test-boot-1",
		RenewedAtMono: clk.now(),
	})

	owner, ok := currentOwnerWithCfg(project, cfg)
	if !ok {
		t.Fatal("currentOwnerWithCfg ok=false, want true for a healthy active owner")
	}
	if owner.AgentID != "owner1" || owner.PID != ownerPid || owner.PidStart != ownerStart {
		t.Fatalf("currentOwnerWithCfg = %+v, want owner1 pid/start tuple", owner)
	}
	if !owner.EngineStamped {
		t.Fatal("EngineStamped=false, want true for complete owner tuple")
	}

	// codex iter-19 [P2]: a STALE active record (past TTL) must NOT be reported
	// as a deliverable owner — its process may be hung; the resume prompt would
	// be typed into a corpse instead of the healthy takeover owner.
	staleClk := &fakeClock{}
	staleClk.advance(cfg.ttl + time.Second)
	if owner, ok := currentOwnerWithCfg(project, testCfg(staleClk, live)); ok {
		t.Fatalf("stale active owner reported as current: %+v, want ok=false", owner)
	}

	// A DEAD owner (pid no longer alive) is likewise not deliverable.
	deadLive := newFakeLiveness() // owner pid not set -> dead
	if owner, ok := currentOwnerWithCfg(project, testCfg(clk, deadLive)); ok {
		t.Fatalf("dead owner reported as current: %+v, want ok=false", owner)
	}

	// Released epoch -> no owner.
	writeEpochRaw(t, project, epochRecord{
		Epoch: 6,
		State: stateReleased,
		Owner: identity{Pid: ownerPid, PidStart: ownerStart, AgentID: "owner1", Project: project},
	})
	if owner, ok := currentOwnerWithCfg(project, cfg); ok {
		t.Fatalf("released epoch returned owner %+v, want ok=false", owner)
	}
}

// ---- codex iter-5 [P2] #1: free-flock must not steal a fresh fencing ----

// OLD died (flock free) but a live first candidate's FRESH fencing record
// is on disk. A second candidate that grabs the free flock must NOT
// promote itself to active over that healthy in-progress takeover — it
// stands down (acquired=false) and leaves the fencing record intact.
func TestFreeFlockDoesNotStealFreshFencing(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	clk := &fakeClock{}
	live := newFakeLiveness()

	// Flock FREE; fresh fencing record from a LIVE candidate.
	const liveCandPid = 6000
	live.set(liveCandPid, int64(666666))
	writeEpochRaw(t, project, epochRecord{
		Epoch: 6, State: stateFencing,
		Owner:         identity{Pid: 9999, PidStart: 333333, AgentID: "old", Project: project},
		Candidate:     identity{Pid: liveCandPid, PidStart: int64(666666), AgentID: "cand1", Project: project},
		BootID:        "test-boot-1",
		RenewedAtMono: clk.now(), // fresh
	})

	clk.advance(5 * time.Second) // within TTL

	lease, acquired, err := acquireLease(project, "cand2", testCfg(clk, live))
	if err != nil {
		t.Fatalf("acquireLease: %v", err)
	}
	if acquired {
		t.Fatal("free-flock path must NOT promote over a fresh live fencing candidate")
	}
	_ = lease
	rec := readEpochFor(t, project)
	if rec.State != stateFencing || rec.Candidate.AgentID != "cand1" {
		t.Fatalf("fresh fencing record must be left intact, got state=%s candidate=%s",
			rec.State, rec.Candidate.AgentID)
	}
}

// ---- codex iter-5 [P2] #2: busy flock, no epoch record ----

// A holder that grabbed the flock and is still FRESH in the acquire-to-
// epoch window (no epoch file yet, flock-body stamp fresh + live) is a
// legitimate booting coord; a candidate stands down.
func TestBusyFlockNoEpoch_FreshHolderStandsDown(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	clk := &fakeClock{}
	live := newFakeLiveness()
	paths, _ := resolvePaths(project)

	// Simulate a booting holder: hold the flock + stamp a FRESH body, no
	// epoch file written yet.
	relHolder := holdFlock(t, project)
	_ = relHolder
	const bootPid = 7000
	live.set(bootPid, int64(777777))
	body, _ := json.Marshal(flockBody{Pid: bootPid, PidStart: int64(777777), BootID: "test-boot-1", Mono: clk.now()})
	if err := os.WriteFile(paths.flock, body, 0o644); err != nil {
		t.Fatalf("stamp body: %v", err)
	}

	clk.advance(5 * time.Second) // within TTL

	lease, acquired, err := acquireLease(project, "cand", testCfg(clk, live))
	if err != nil {
		t.Fatalf("acquireLease: %v", err)
	}
	if acquired {
		t.Fatal("a fresh booting holder (no epoch yet) must NOT be taken over")
	}
	_ = lease
}

// A holder hung in the acquire-to-epoch window (no epoch file, flock-body
// stamp older than TTL) is recoverable: the candidate enters takeover.
// In PR1 the kill is a no-op stub and the hung holder still holds the
// flock, so the takeover cannot acquire it -> it surfaces a
// fenced_not_acquired escalation (NOT an indefinite silent stand-down).
func TestBusyFlockNoEpoch_StaleHolderRecovers(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	clk := &fakeClock{}
	live := newFakeLiveness()
	paths, _ := resolvePaths(project)

	holdFlock(t, project) // holder's flock still held in this harness
	const hungPid = 7000
	// Holder pid NOT in the live map: provably dead. (KP6 rewrite: a
	// LIVE in-window holder now aborts pre-fence and is quarantined —
	// TestKP6_LiveFlockBodyHolderAborts; the escalate-loudly contract
	// this test pins applies to the provably-dead holder.)
	// Body stamped at mono=0; we advance past TTL so it reads as hung.
	body, _ := json.Marshal(flockBody{Pid: hungPid, PidStart: int64(777777), BootID: "test-boot-1", Mono: 0})
	if err := os.WriteFile(paths.flock, body, 0o644); err != nil {
		t.Fatalf("stamp body: %v", err)
	}

	cfg := testCfg(clk, live)
	cfg.flockRetryBudget = 100 * time.Millisecond // PR1 stub can't free the flock
	var fenced atomic.Bool
	cfg.killStub = func(owner identity, _ int64) error {
		if owner.Pid == hungPid {
			fenced.Store(true)
		}
		return nil // PR1 no-op: holder keeps the flock
	}

	clk.advance(31 * time.Second) // body now past TTL

	_, acquired, err := acquireLease(project, "cand", cfg)
	// PR1: takeover fences + kill-stub(no-op) + cannot acquire the still-
	// held flock -> surfaces an error (escalation), never silent stand-down.
	if acquired {
		t.Fatal("PR1 stub cannot free a real held flock; acquire should not succeed")
	}
	if err == nil {
		t.Fatal("a hung in-window holder must SURFACE (escalation), not silently stand down")
	}
	if !fenced.Load() {
		t.Fatal("takeover must have fenced + targeted the flock-body holder pid")
	}
	// On disk: a fencing/fenced record naming the hung holder, NOT silence.
	rec := readEpochFor(t, project)
	if rec.State != stateFencing && rec.State != stateFencedNotAcquired {
		t.Fatalf("expected a transient takeover record, got state=%s", rec.State)
	}
	if rec.Owner.Pid != hungPid {
		t.Fatalf("transient record must name the hung flock-body holder as owner, got pid=%d", rec.Owner.Pid)
	}
}

// ---- codex iter-6 [P2]: heartbeat must not demote on serializer busy ----

// A healthy leader whose heartbeat tick overlaps a brief writer of
// coordinator.epoch.lock must NOT self-demote: withEpochLock returns the
// transient errSerializerBusy, and the heartbeat treats it as "skip this
// tick", never a permanent stop. (Closes the spurious-TTL-expiry hole.)
func TestHeartbeatDoesNotDemoteOnSerializerBusy(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	clk := &fakeClock{}
	live := newFakeLiveness()
	cfg := testCfg(clk, live)
	paths, _ := resolvePaths(project)

	// Shrink the serializer budget so the test is fast.
	prev := epochLockBudget
	epochLockBudget = 80 * time.Millisecond
	t.Cleanup(func() { epochLockBudget = prev })

	l := &Lease{
		cfg: cfg, paths: paths,
		self: identity{Pid: os.Getpid(), PidStart: selfStart, AgentID: "me", Project: project},
		host: "h", boot: "test-boot-1", epoch: 9,
	}
	writeEpochRaw(t, project, epochRecord{
		Epoch: 9, State: stateActive, Owner: l.self,
		BootID: "test-boot-1", RenewedAtMono: 0,
	})

	// Hold epoch.lock from a separate fd for the whole budget window.
	if err := os.MkdirAll(filepath.Dir(paths.epochLock), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	holder, err := os.OpenFile(paths.epochLock, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open epoch.lock: %v", err)
	}
	defer func() { _ = holder.Close() }()
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("flock holder: %v", err)
	}

	ok, hbErr := l.heartbeatOnce()
	if ok {
		t.Fatal("heartbeatOnce should not report ok while the serializer is busy")
	}
	if !errorsIsSerializerBusy(hbErr) {
		t.Fatalf("expected errSerializerBusy (transient), got: %v", hbErr)
	}

	// Release the serializer; the next heartbeat must succeed (no demotion).
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatalf("unlock holder: %v", err)
	}
	clk.advance(5 * time.Second)
	ok2, err2 := l.heartbeatOnce()
	if err2 != nil || !ok2 {
		t.Fatalf("heartbeat should renew once the serializer frees, got ok=%v err=%v", ok2, err2)
	}
	rec := readEpochFor(t, project)
	if rec.Epoch != 9 || rec.Owner.AgentID != "me" {
		t.Fatalf("leader must still own epoch 9 after a busy tick, got %s@%d", rec.Owner.AgentID, rec.Epoch)
	}
}

func errorsIsSerializerBusy(err error) bool {
	// errSerializerBusy is unexported; compare via the package-internal
	// sentinel directly (this test is in-package).
	return err != nil && err == errSerializerBusy //nolint:errorlint // exact sentinel, never wrapped here.
}

// ---- codex iter-9 [P2] / KP6 rewrite: live flock holder is quarantined ----

// When resuming a STALE fencing record whose previous candidate acquired
// coordinator.flock and stamped its body before hanging, the live flock
// holder is that CANDIDATE, not the original `old` owner. The gate must
// COVER the candidate (it is a prospective kill target) — and because it
// is ALIVE, the resume aborts pre-fence and surfaces it in the detection
// result (DESIGN-coord-no-auto-kill: staleness never kills a live coord).
func TestResumeTakeover_LiveFlockBodyHolderQuarantined(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	clk := &fakeClock{}
	live := newFakeLiveness()
	paths, _ := resolvePaths(project)

	// A hung previous CANDIDATE holds the flock and stamped its body.
	const candPid = 5500
	const candStart = int64(550000)
	live.set(candPid, candStart)
	relCand := holdFlock(t, project)
	body, _ := json.Marshal(flockBody{Pid: candPid, PidStart: candStart, BootID: "test-boot-1", Mono: 0})
	if err := os.WriteFile(paths.flock, body, 0o644); err != nil {
		t.Fatalf("stamp body: %v", err)
	}

	// Stale fencing record: owner=OLD (dead), candidate=the hung candidate.
	const oldPid = 9999 // original owner, already dead (not in live map)
	writeEpochRaw(t, project, epochRecord{
		Epoch: 6, State: stateFencing,
		Owner:         identity{Pid: oldPid, PidStart: 333333, AgentID: "old", Project: project},
		Candidate:     identity{Pid: candPid, PidStart: candStart, AgentID: "cand1", Project: project},
		BootID:        "test-boot-1",
		RenewedAtMono: 0, // stalled
	})

	cfg := testCfg(clk, live)
	var kills atomic.Int64
	cfg.killStub = func(identity, int64) error {
		kills.Add(1)
		return nil
	}
	_ = relCand

	clk.advance(31 * time.Second) // fencing record past TTL -> resumable

	// KP6 rewrite (DESIGN-coord-no-auto-kill): the hung candidate is
	// ALIVE, so the resume-takeover must ABORT pre-fence — quarantine the
	// live flock-body holder in the detection result instead of shooting
	// it. (The original test asserted kill+acquire; killing a live
	// coordinator on a staleness heuristic is the incident class this
	// task removes. A DEAD candidate resume still proceeds — T34.)
	lease, acquired, liveHolders, err := acquireLeaseDetect(project, "cand2", cfg)
	if err != nil {
		t.Fatalf("acquireLeaseDetect (resume): %v", err)
	}
	if acquired || lease != nil {
		t.Fatal("resume against a LIVE hung candidate must stand down, not acquire")
	}
	if kills.Load() != 0 {
		t.Fatalf("kill fn fired %d times on a live flock-body holder", kills.Load())
	}
	found := false
	for _, h := range liveHolders {
		if h.Pid == candPid && h.PidStart == candStart {
			found = true
		}
	}
	if !found {
		t.Fatalf("detection %+v must name the live flock-body holder pid=%d", liveHolders, candPid)
	}
	rec := readEpochFor(t, project)
	if rec.State != stateFencing || rec.Epoch != 6 {
		t.Fatalf("record must be untouched (fencing/epoch 6), got %s/%d", rec.State, rec.Epoch)
	}
}

// ---- codex iter-10 [P2]: don't silently stand down on fenced_not_acquired ----

// A fenced_not_acquired record is a GAVE-UP escalation, not an in-progress
// takeover. A new AcquireLease must immediately re-attempt recovery (and,
// in PR1 where the kill is a no-op and a real flock is still held, SURFACE
// an escalation) — never return acquired=false,err=nil silently until TTL.
func TestFencedNotAcquiredDoesNotSilentlyStandDown(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	clk := &fakeClock{}
	live := newFakeLiveness()

	// Busy flock (still held in this harness), fenced_not_acquired record,
	// renewed_at FRESH (within TTL). Owner + candidate are DEAD (not in
	// the live map): the KP6 gate passes and the re-attempt proceeds.
	// (The LIVE-holder shape now stands down with a quarantine detection
	// instead — TestFencedNotAcquired_LiveHoldersQuarantined below.)
	const oldPid = 4242
	holdFlock(t, project)
	const deadCandPid = 6000
	writeEpochRaw(t, project, epochRecord{
		Epoch: 7, State: stateFencedNotAcquired,
		Owner:         identity{Pid: oldPid, PidStart: int64(222222), AgentID: "old", Project: project},
		Candidate:     identity{Pid: deadCandPid, PidStart: int64(666666), AgentID: "cand1", Project: project},
		BootID:        "test-boot-1",
		RenewedAtMono: clk.now(), // fresh, within TTL
	})

	cfg := testCfg(clk, live)
	cfg.flockRetryBudget = 100 * time.Millisecond // PR1 stub can't free a real flock
	var fenced atomic.Bool
	cfg.killStub = func(identity, int64) error { fenced.Store(true); return nil }

	clk.advance(5 * time.Second) // still within TTL

	_, acquired, err := acquireLease(project, "cand2", cfg)
	if acquired {
		t.Fatal("PR1 stub cannot free the held flock; should not acquire")
	}
	// Must NOT silently stand down: it re-attempts the takeover (fences +
	// kill-stub) and surfaces an escalation error since the flock is held.
	if err == nil {
		t.Fatal("fenced_not_acquired must trigger recovery/escalation, not silent stand-down")
	}
	if !fenced.Load() {
		t.Fatal("a fenced_not_acquired record must drive a fresh takeover attempt (re-fence/kill)")
	}
}

// KP6 companion: a fenced_not_acquired record whose owner/candidate are
// STILL ALIVE is not silently ignored either — the gate stands down
// WITHOUT killing and returns the live holders as the typed detection
// (cmd/fleet's poll loop reports/quarantines them).
func TestFencedNotAcquired_LiveHoldersQuarantined(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	clk := &fakeClock{}
	live := newFakeLiveness()

	const oldPid = 4242
	live.set(oldPid, int64(222222))
	holdFlock(t, project)
	const liveCandPid = 6000
	live.set(liveCandPid, int64(666666))
	writeEpochRaw(t, project, epochRecord{
		Epoch: 7, State: stateFencedNotAcquired,
		Owner:         identity{Pid: oldPid, PidStart: int64(222222), AgentID: "old", Project: project},
		Candidate:     identity{Pid: liveCandPid, PidStart: int64(666666), AgentID: "cand1", Project: project},
		BootID:        "test-boot-1",
		RenewedAtMono: clk.now(),
	})

	cfg := testCfg(clk, live)
	var kills atomic.Int64
	cfg.killStub = func(identity, int64) error { kills.Add(1); return nil }
	clk.advance(5 * time.Second)

	lease, acquired, liveHolders, err := acquireLeaseDetect(project, "cand2", cfg)
	if err != nil {
		t.Fatalf("acquireLeaseDetect: %v", err)
	}
	if acquired || lease != nil {
		t.Fatal("must stand down against live holders")
	}
	if kills.Load() != 0 {
		t.Fatalf("kill fn fired %d times on live holders", kills.Load())
	}
	if len(liveHolders) != 2 {
		t.Fatalf("detection = %+v, want both live holders (owner + candidate)", liveHolders)
	}
	rec := readEpochFor(t, project)
	if rec.State != stateFencedNotAcquired || rec.Epoch != 7 {
		t.Fatalf("record must be untouched, got %s/%d", rec.State, rec.Epoch)
	}
}

// ---- codex iter-12 [P2]: Release invalidates outstanding tokens ----

// After a clean Release(), a previously-issued LeaseToken must STOP
// validating immediately (the record is demoted to `released`), so a
// racing goroutine's lease-gated mutation can't be admitted in the window
// before TTL/successor.
func TestReleaseInvalidatesOutstandingToken(t *testing.T) {
	setupHome(t)
	t.Setenv(FailoverEnvVar, "1")
	const project = "rainier"

	lease, acquired, err := acquireLease(project, "me", testCfg(&fakeClock{}, newFakeLiveness()))
	if err != nil || !acquired {
		t.Fatalf("first acquire: acquired=%v err=%v", acquired, err)
	}
	tok := lease.Token()
	if !tok.StillOwned() {
		t.Fatal("token should be valid while the lease is held")
	}

	lease.Release()

	if tok.StillOwned() {
		t.Fatal("token must NOT be StillOwned after Release() (record demoted to released)")
	}
	rec := readEpochFor(t, project)
	if rec.State != stateReleased {
		t.Fatalf("expected released state after Release, got %s", rec.State)
	}
}

// A successor must still be able to acquire after a clean Release (the
// released record + freed flock -> free-flock promote).
func TestSuccessorAcquiresAfterRelease(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	clk := &fakeClock{}
	live := newFakeLiveness()

	first, acquired, err := acquireLease(project, "first", testCfg(clk, live))
	if err != nil || !acquired {
		t.Fatalf("first acquire: acquired=%v err=%v", acquired, err)
	}
	firstEpoch := first.epoch
	first.Release()

	second, acquired2, err := acquireLease(project, "second", testCfg(clk, live))
	if err != nil || !acquired2 {
		t.Fatalf("successor acquire after release: acquired=%v err=%v", acquired2, err)
	}
	defer second.Release()
	rec := readEpochFor(t, project)
	if rec.State != stateActive || rec.Owner.AgentID != "second" {
		t.Fatalf("expected active second after release, got %s/%s", rec.State, rec.Owner.AgentID)
	}
	if rec.Epoch <= firstEpoch {
		t.Fatalf("successor epoch (%d) must advance past the released epoch (%d)", rec.Epoch, firstEpoch)
	}
}

// ---- codex iter-13 [P2] / KP6 rewrite: candidate covered with no flock body ----

// stampFlockBody is best-effort: the prior candidate may hold the flock
// with a MISSING body stamp. The takeover's target set must still cover
// that candidate (read from rec.Candidate), not only the flock-body
// holder — and since it is ALIVE, the KP6 gate quarantines it pre-fence
// instead of killing (DESIGN-coord-no-auto-kill).
func TestResumeTakeover_LiveCandidateWithoutFlockBodyQuarantined(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	clk := &fakeClock{}
	live := newFakeLiveness()

	// Hung candidate holds the flock — but we DO NOT stamp the flock body
	// (simulate a best-effort stamp that never landed). holdFlock leaves
	// the body empty.
	const candPid = 5500
	const candStart = int64(550000)
	live.set(candPid, candStart)
	relCand := holdFlock(t, project)

	// Stale fencing record naming the dead OLD owner + the LIVE hung candidate.
	writeEpochRaw(t, project, epochRecord{
		Epoch: 6, State: stateFencing,
		Owner:         identity{Pid: 9999, PidStart: 333333, AgentID: "old", Project: project},
		Candidate:     identity{Pid: candPid, PidStart: candStart, AgentID: "cand1", Project: project},
		BootID:        "test-boot-1",
		RenewedAtMono: 0, // stalled
	})

	cfg := testCfg(clk, live)
	var kills atomic.Int64
	cfg.killStub = func(identity, int64) error { kills.Add(1); return nil }
	_ = relCand

	clk.advance(31 * time.Second) // past TTL -> resumable

	// KP6 rewrite: the recorded candidate is a prospective target even
	// with NO flock body (best-effort stamp never landed) — and it is
	// ALIVE, so the resume aborts pre-fence and surfaces it. This pins
	// the enumeration: dropping the candidate from the gate's target set
	// would let the takeover fence + shoot a live process.
	lease, acquired, liveHolders, err := acquireLeaseDetect(project, "cand2", cfg)
	if err != nil {
		t.Fatalf("acquireLeaseDetect (resume): %v", err)
	}
	if acquired || lease != nil {
		t.Fatal("resume against a LIVE recorded candidate must stand down")
	}
	if kills.Load() != 0 {
		t.Fatalf("kill fn fired %d times on a live recorded candidate", kills.Load())
	}
	if len(liveHolders) != 1 || liveHolders[0].Pid != candPid {
		t.Fatalf("detection = %+v, want the live recorded candidate pid=%d (no flock body needed)",
			liveHolders, candPid)
	}
	rec := readEpochFor(t, project)
	if rec.State != stateFencing || rec.Epoch != 6 {
		t.Fatalf("record must be untouched, got %s/%d", rec.State, rec.Epoch)
	}
}

// ---- codex iter-15 [P2] #1: candidate resumes its OWN fencing record ----

// After casFencingToActive hits errSerializerBusy and releases the flock,
// acquireLease retries; the candidate then sees its OWN fresh fencing
// record. transientResumable must return true for the candidate's own
// record (else it stands down with no active leader until TTL).
func TestTransientResumable_OwnFencingRecord(t *testing.T) {
	clk := &fakeClock{}
	live := newFakeLiveness()
	cfg := testCfg(clk, live)
	self := identity{Pid: os.Getpid(), PidStart: selfStart, AgentID: "me", Project: "rainier"}
	l := &Lease{cfg: cfg, self: self, boot: "test-boot-1"}

	// Our OWN fresh fencing record (candidate == self, alive, within TTL).
	own := epochRecord{
		Epoch: 6, State: stateFencing,
		Owner:         identity{Pid: 9999, AgentID: "old", Project: "rainier"},
		Candidate:     self,
		BootID:        "test-boot-1",
		RenewedAtMono: clk.now(),
	}
	if !l.transientResumable(own) {
		t.Fatal("a candidate must always be able to resume its OWN fencing record")
	}

	// A DIFFERENT live candidate's fresh record is NOT resumable.
	other := own
	other.Candidate = identity{Pid: 6000, PidStart: 660000, AgentID: "other", Project: "rainier"}
	live.set(6000, 660000)
	if l.transientResumable(other) {
		t.Fatal("a different live candidate's fresh record must NOT be resumable")
	}
}

// End-to-end: a candidate whose fencing CAS lost the serializer (busy) on
// the first pass must still converge to active leadership on retry, not
// stand down. We simulate by pre-writing the candidate's own fresh fencing
// record with a FREE flock; acquireLease must promote it (resume own
// takeover via the free-flock path), not stand down.
func TestAcquireResumesOwnFencingAfterSerializerBusy(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	clk := &fakeClock{}
	live := newFakeLiveness()

	self := identity{Pid: os.Getpid(), PidStart: selfStart, AgentID: "me", Project: project}
	// Free flock + our own fresh fencing record (candidate == self).
	writeEpochRaw(t, project, epochRecord{
		Epoch: 6, State: stateFencing,
		Owner:         identity{Pid: 9999, PidStart: 333333, AgentID: "old", Project: project},
		Candidate:     self,
		BootID:        "test-boot-1",
		RenewedAtMono: clk.now(),
	})

	lease, acquired, err := acquireLease(project, "me", testCfg(clk, live))
	if err != nil {
		t.Fatalf("acquireLease: %v", err)
	}
	if !acquired {
		t.Fatal("a candidate must resume its OWN fencing record to active, not stand down")
	}
	defer lease.Release()
	rec := readEpochFor(t, project)
	if rec.State != stateActive || rec.Owner.AgentID != "me" {
		t.Fatalf("expected active me after resuming own fencing, got %s/%s", rec.State, rec.Owner.AgentID)
	}
}

// ---- codex iter-15 [P2] #2: Release demote retries past serializer contention ----

// If epoch.lock is briefly contended during Release, the demote must RETRY
// and still land (record -> released, token invalid), not silently leave a
// stale `active` record. We hold epoch.lock from another fd, start Release
// (which polls within its bounded budget), release the holder, and assert
// the record demoted and the token invalidated.
func TestReleaseDemoteRetriesPastSerializerContention(t *testing.T) {
	setupHome(t)
	t.Setenv(FailoverEnvVar, "1")
	const project = "rainier"

	cfg := defaultLeaseConfig()
	lease, acquired, err := acquireLease(project, "me", cfg)
	if err != nil || !acquired {
		t.Fatalf("acquire: acquired=%v err=%v", acquired, err)
	}
	tok := lease.Token()
	paths, _ := resolvePaths(project)

	// Shrink the per-attempt serializer budget so the 5-attempt retry loop
	// spans a short, deterministic window.
	prevBudget := epochLockBudget
	epochLockBudget = 60 * time.Millisecond
	t.Cleanup(func() { epochLockBudget = prevBudget })

	// Hold epoch.lock from a separate fd, then release it shortly so an
	// in-flight Release retry can win.
	holder, err := os.OpenFile(paths.epochLock, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open epoch.lock: %v", err)
	}
	defer func() { _ = holder.Close() }()
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("flock holder: %v", err)
	}

	releaseDone := make(chan struct{})
	go func() {
		lease.Release()
		close(releaseDone)
	}()

	// Let Release exhaust at least one serializer-busy attempt, then free
	// the serializer so a subsequent retry demotes the record.
	time.Sleep(80 * time.Millisecond)
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatalf("unlock holder: %v", err)
	}

	select {
	case <-releaseDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Release did not complete")
	}

	rec, rerr := readEpoch(paths.epoch)
	if rerr != nil {
		t.Fatalf("readEpoch: %v", rerr)
	}
	if rec.State != stateReleased {
		t.Fatalf("Release must demote to released after the serializer frees, got %s", rec.State)
	}
	if tok.StillOwned() {
		t.Fatal("token must be invalid after a (retried) Release demote")
	}
}

// ---- codex iter-16 [P2]: surface release demotion failure on a real fault ----

// A real read/write fault during Release's demote (e.g. a corrupted epoch
// file) must SURFACE a warning — not silently set "demoted" and skip it,
// which would leave a possibly-active record + a still-valid token with no
// operator-visible signal.
func TestReleaseSurfacesDemoteFaultOnCorruptEpoch(t *testing.T) {
	setupHome(t)
	t.Setenv(FailoverEnvVar, "1")
	const project = "rainier"

	lease, acquired, err := acquireLease(project, "me", defaultLeaseConfig())
	if err != nil || !acquired {
		t.Fatalf("acquire: acquired=%v err=%v", acquired, err)
	}
	paths, _ := resolvePaths(project)

	// Corrupt the epoch file so readEpoch fails with a real (non-NotExist)
	// unmarshal error during the demote.
	if werr := os.WriteFile(paths.epoch, []byte("{not valid json"), 0o644); werr != nil {
		t.Fatalf("corrupt epoch: %v", werr)
	}

	// Capture os.Stderr around Release.
	origStderr := os.Stderr
	r, w, perr := os.Pipe()
	if perr != nil {
		t.Fatalf("pipe: %v", perr)
	}
	os.Stderr = w
	lease.Release()
	_ = w.Close()
	os.Stderr = origStderr
	out, _ := io.ReadAll(r)

	if !contains(string(out), "could not demote released lease") {
		t.Fatalf("Release must surface a warning on a real demote fault; stderr was:\n%s", out)
	}
}

// codex iter-23 [P1] regression: LeaseRecordActive distinguishes a real lease
// generation (active / fencing / fenced_not_acquired) from "no lease" (released
// / missing). A failed takeover (fenced_not_acquired) MUST count as a real lease
// so handoff delivery stays pending/doctor-gated instead of direct-sending to a
// queued replacement as if the coord were legacy/bare.
func TestLeaseRecordActive(t *testing.T) {
	setupHome(t)
	const project = "lra-test"

	if LeaseRecordActive(project) {
		t.Fatal("no epoch record -> LeaseRecordActive should be false")
	}
	owner := identity{Pid: 4242, PidStart: 222222, AgentID: "owner1", Project: project}
	for _, tc := range []struct {
		state string
		want  bool
	}{
		{stateActive, true},
		{stateFencing, true},
		{stateFencedNotAcquired, true},
		{stateReleased, false},
	} {
		writeEpochRaw(t, project, epochRecord{Epoch: 5, State: tc.state, Owner: owner, BootID: "test-boot-1"})
		if got := LeaseRecordActive(project); got != tc.want {
			t.Fatalf("LeaseRecordActive(state=%q) = %v, want %v", tc.state, got, tc.want)
		}
	}
}
