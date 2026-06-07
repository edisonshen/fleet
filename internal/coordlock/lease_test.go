//go:build unix

package coordlock

import (
	"encoding/json"
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
		killStub:         func(identity) error { return nil },
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

// T3: takeover on TTL expiry of a HUNG holder whose flock is STILL HELD.
// Order must be fence (epoch CAS to fencing) -> kill -> acquire. The
// kill-stub releases the simulated holder's flock so the candidate can
// acquire it after the kill.
func TestT3_TakeoverOnTTLExpiryHungHolder(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	clk := &fakeClock{}
	live := newFakeLiveness()

	const holderPid = 4242
	const holderStart = int64(222222)
	live.set(holderPid, holderStart)
	relHolder := holdFlock(t, project)

	// Active epoch with renewed_at frozen at t=0.
	writeEpochRaw(t, project, epochRecord{
		Epoch: 5, State: stateActive,
		Owner:  identity{Pid: holderPid, PidStart: holderStart, AgentID: "old", Project: project},
		BootID: "test-boot-1", RenewedAtMono: 0,
	})

	cfg := testCfg(clk, live)
	var fenceObservedBeforeKill atomic.Bool
	cfg.killStub = func(owner identity) error {
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
	cfg.killStub = func(identity) error { killed.Store(true); return nil }

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
	// OLD holder: flock STILL held (hung), pid alive.
	live.set(oldPid, oldStart)
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
	cfg.killStub = func(owner identity) error {
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

	// A's CAS must FAIL (B advanced + is live) -> ok=false (release+retry).
	ok, err := a.casToActiveAfterFlock(f)
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

// ---- Failover gate ----

// TestAcquireLeaseRefusesWhenFailoverDisabled: the public entry point
// refuses with ErrFailoverDisabled while FLEET_LEASE_FAILOVER is off
// (default), so production behavior is unchanged by this merge.
func TestAcquireLeaseRefusesWhenFailoverDisabled(t *testing.T) {
	setupHome(t)
	t.Setenv(FailoverEnvVar, "") // explicitly off
	_, _, err := AcquireLease("rainier", "cand")
	if err == nil {
		t.Fatal("AcquireLease must refuse when FLEET_LEASE_FAILOVER is off")
	}
	if err.Error() != ErrFailoverDisabled.Error() {
		t.Fatalf("expected ErrFailoverDisabled, got %v", err)
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
