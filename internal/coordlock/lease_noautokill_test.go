//go:build linux || darwin

package coordlock

// lease_noautokill_test.go — KP6 pre-fence liveness gate
// (DESIGN-coord-no-auto-kill, task coord-no-auto-kill-ac54).
//
// The invariant under test: a takeover driven by a STALENESS heuristic
// (lease TTL) may proceed only when EVERY prospective kill target
// (recorded owner, prior fencing candidate, flock-body holder) is
// provably dead. Any live target => write NOTHING (epoch record
// byte-unchanged), kill fn never invoked, and the acquire returns the
// STAND-DOWN shape (acquired=false, nil error) so a standby keeps
// polling instead of exiting via the error branch.

import (
	"bytes"
	"encoding/json"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// readEpochBytes reads the raw on-disk epoch file for byte-unchanged
// assertions (stronger than a field compare: the gate must not rewrite
// even an equivalent record).
func readEpochBytes(t *testing.T, project string) []byte {
	t.Helper()
	paths, err := resolvePaths(project)
	if err != nil {
		t.Fatalf("resolvePaths: %v", err)
	}
	b, err := os.ReadFile(paths.epoch)
	if err != nil {
		t.Fatalf("read epoch: %v", err)
	}
	return b
}

// writeFlockBodyRaw stamps a flock body (as a hung holder would have)
// BEFORE the test fd takes the flock. os.WriteFile then holdFlock keeps
// the body readable while the flock is busy.
func writeFlockBodyRaw(t *testing.T, project string, body flockBody) {
	t.Helper()
	paths, err := resolvePaths(project)
	if err != nil {
		t.Fatalf("resolvePaths: %v", err)
	}
	if err := os.MkdirAll(dirOf(paths.flock), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	if err := os.WriteFile(paths.flock, b, 0o644); err != nil {
		t.Fatalf("write flock body: %v", err)
	}
}

func dirOf(p string) string {
	i := len(p) - 1
	for i >= 0 && p[i] != '/' {
		i--
	}
	if i <= 0 {
		return "/"
	}
	return p[:i]
}

// Test 2 (plan): takeover live-owner pre-fence gate. Stale ACTIVE lease
// record whose owner pid is ALIVE (hung-but-alive leader, the incident
// shape). The gate must abort BEFORE the fencing write: epoch record
// byte-unchanged, kill fn never invoked, stand-down shape (acquired=false,
// nil error — NOT a "did not converge" attempt-exhaustion error), and the
// live owner surfaces in the typed detection result.
func TestKP6_LiveOwnerAbortsTakeoverPreFence(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	clk := &fakeClock{}
	live := newFakeLiveness()

	const holderPid = 4242
	const holderStart = int64(222222)
	live.set(holderPid, holderStart)
	holdFlock(t, project)

	writeEpochRaw(t, project, epochRecord{
		Epoch: 5, State: stateActive,
		Owner:  identity{Pid: holderPid, PidStart: holderStart, AgentID: "old", Project: project},
		BootID: "test-boot-1", RenewedAtMono: 0,
	})
	before := readEpochBytes(t, project)

	cfg := testCfg(clk, live)
	var kills atomic.Int64
	cfg.killStub = func(identity, int64) error { kills.Add(1); return nil }

	clk.advance(31 * time.Second) // past TTL: owner reads as hung

	lease, acquired, liveHolders, err := acquireLeaseDetect(project, "cand", cfg)
	if err != nil {
		t.Fatalf("acquireLeaseDetect: want nil error (stand-down shape), got %v", err)
	}
	if acquired || lease != nil {
		t.Fatalf("want acquired=false lease=nil on live-owner abort, got acquired=%v", acquired)
	}
	if kills.Load() != 0 {
		t.Fatalf("kill fn invoked %d times; must never fire on a live target", kills.Load())
	}
	after := readEpochBytes(t, project)
	if !bytes.Equal(before, after) {
		t.Fatalf("epoch record changed across aborted takeover:\n before=%s\n after=%s", before, after)
	}
	if len(liveHolders) != 1 || liveHolders[0].Pid != holderPid || liveHolders[0].PidStart != holderStart {
		t.Fatalf("typed detection = %+v, want the live owner pid=%d start=%d", liveHolders, holderPid, holderStart)
	}
}

// Test 5 (plan): leader survives an aborted takeover. After the test-2
// abort the untouched leader's heartbeatOnce at its own epoch must renew
// normally (no self-demote) — proving the gate did not fence it as a
// side effect.
func TestKP6_LeaderHeartbeatRenewsAfterAbortedTakeover(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	clk := &fakeClock{}
	live := newFakeLiveness()

	const holderPid = 4242
	const holderStart = int64(222222)
	live.set(holderPid, holderStart)
	holdFlock(t, project)

	ownerID := identity{Pid: holderPid, PidStart: holderStart, AgentID: "old", Project: project}
	writeEpochRaw(t, project, epochRecord{
		Epoch: 5, State: stateActive, Owner: ownerID,
		BootID: "test-boot-1", RenewedAtMono: 0,
	})
	clk.advance(31 * time.Second)

	cfg := testCfg(clk, live)
	if _, acquired, _, err := acquireLeaseDetect(project, "cand", cfg); err != nil || acquired {
		t.Fatalf("aborted takeover precondition failed: acquired=%v err=%v", acquired, err)
	}

	// The hung leader wakes up and heartbeats at ITS epoch.
	paths, err := resolvePaths(project)
	if err != nil {
		t.Fatalf("resolvePaths: %v", err)
	}
	leader := &Lease{cfg: cfg, paths: paths, self: ownerID, host: "h", boot: "test-boot-1", epoch: 5}
	ok, herr := leader.heartbeatOnce()
	if herr != nil {
		t.Fatalf("heartbeatOnce: %v", herr)
	}
	if !ok {
		t.Fatal("leader self-demoted after an aborted takeover; the gate must not fence the live leader")
	}
	rec := readEpochFor(t, project)
	if rec.State != stateActive || rec.Epoch != 5 || rec.Owner.AgentID != "old" {
		t.Fatalf("post-renew record = %+v, want epoch 5 active owner=old", rec)
	}
	if rec.RenewedAtMono != clk.now() {
		t.Fatalf("renewed_at_mono = %d, want %d (renewed)", rec.RenewedAtMono, clk.now())
	}
}

// Test 3 (plan): live prior-candidate gate. Owner DEAD, but the stale
// FENCING record's candidate (a prior takeover driver that may hold the
// flock) is ALIVE and != self. Any-live blocks: abort, record unchanged.
func TestKP6_LivePriorCandidateAbortsResume(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	clk := &fakeClock{}
	live := newFakeLiveness()

	const deadOwnerPid = 4242
	const candPid = 5555
	const candStart = int64(333333)
	live.set(candPid, candStart) // prior candidate ALIVE; owner absent = dead
	holdFlock(t, project)

	writeEpochRaw(t, project, epochRecord{
		Epoch: 6, State: stateFencing,
		Owner:     identity{Pid: deadOwnerPid, PidStart: 222222, AgentID: "old", Project: project},
		Candidate: identity{Pid: candPid, PidStart: candStart, AgentID: "prior-cand", Project: project},
		BootID:    "test-boot-1", RenewedAtMono: 0,
	})
	before := readEpochBytes(t, project)

	cfg := testCfg(clk, live)
	var kills atomic.Int64
	cfg.killStub = func(identity, int64) error { kills.Add(1); return nil }
	clk.advance(31 * time.Second) // fencing record stale -> resumable

	lease, acquired, liveHolders, err := acquireLeaseDetect(project, "cand", cfg)
	if err != nil {
		t.Fatalf("acquireLeaseDetect: %v", err)
	}
	if acquired || lease != nil {
		t.Fatalf("want abort on live prior candidate, got acquired=%v", acquired)
	}
	if kills.Load() != 0 {
		t.Fatalf("kill fn fired %d times on a live prior candidate", kills.Load())
	}
	if !bytes.Equal(before, readEpochBytes(t, project)) {
		t.Fatal("epoch record changed; pre-fence abort must write nothing")
	}
	if len(liveHolders) != 1 || liveHolders[0].Pid != candPid {
		t.Fatalf("detection = %+v, want live prior candidate pid=%d", liveHolders, candPid)
	}
}

// Test 3b (plan): resume-own-fencing unaffected. A stale fencing record
// whose prior candidate is SELF must NOT be probed (self exclusion
// retained) — the takeover resume proceeds exactly as today.
func TestKP6_ResumeOwnFencingNotBlockedBySelfProbe(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	clk := &fakeClock{}
	live := newFakeLiveness()

	const deadOwnerPid = 4242
	relHolder := holdFlock(t, project)

	selfID := identity{Pid: os.Getpid(), PidStart: selfStart, AgentID: "cand", Project: project}
	writeEpochRaw(t, project, epochRecord{
		Epoch: 6, State: stateFencing,
		Owner:     identity{Pid: deadOwnerPid, PidStart: 222222, AgentID: "old", Project: project},
		Candidate: selfID,
		BootID:    "test-boot-1", RenewedAtMono: 0,
	})

	cfg := testCfg(clk, live)
	var kills atomic.Int64
	cfg.killStub = func(identity, int64) error {
		kills.Add(1)
		relHolder() // the (dead) holder's flock frees
		return nil
	}
	clk.advance(31 * time.Second)

	lease, acquired, liveHolders, err := acquireLeaseDetect(project, "cand", cfg)
	if err != nil {
		t.Fatalf("acquireLeaseDetect: %v", err)
	}
	if !acquired || lease == nil {
		t.Fatalf("resume-own-fencing must proceed (self never probed); got acquired=%v holders=%+v",
			acquired, liveHolders)
	}
	defer lease.Release()
	rec := readEpochFor(t, project)
	if rec.State != stateActive || rec.Owner.AgentID != "cand" || rec.Epoch != 7 {
		t.Fatalf("post-resume record = %+v, want epoch 7 active owner=cand", rec)
	}
}

// Test 3c (plan): flock-holder third target. Owner + candidate DEAD but
// the flock BODY names a LIVE holder (a hung process that stamped the
// flock). The gate must cover this third member of the target set: abort,
// record unchanged.
func TestKP6_LiveFlockBodyHolderAborts(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	clk := &fakeClock{}
	live := newFakeLiveness()

	const bodyPid = 7777
	const bodyStart = int64(444444)
	live.set(bodyPid, bodyStart)

	// Stamp the flock body BEFORE the test fd holds the flock.
	writeFlockBodyRaw(t, project, flockBody{
		Pid: bodyPid, PidStart: bodyStart, BootID: "test-boot-1", Mono: 0,
	})
	holdFlock(t, project)

	writeEpochRaw(t, project, epochRecord{
		Epoch: 6, State: stateFencing,
		Owner:     identity{Pid: 4242, PidStart: 222222, AgentID: "old", Project: project},
		Candidate: identity{Pid: 5555, PidStart: 333333, AgentID: "prior-cand", Project: project},
		BootID:    "test-boot-1", RenewedAtMono: 0,
	})
	before := readEpochBytes(t, project)

	cfg := testCfg(clk, live)
	var kills atomic.Int64
	cfg.killStub = func(identity, int64) error { kills.Add(1); return nil }
	clk.advance(31 * time.Second)

	lease, acquired, liveHolders, err := acquireLeaseDetect(project, "cand", cfg)
	if err != nil {
		t.Fatalf("acquireLeaseDetect: %v", err)
	}
	if acquired || lease != nil {
		t.Fatalf("want abort on live flock-body holder, got acquired=%v", acquired)
	}
	if kills.Load() != 0 {
		t.Fatalf("kill fn fired %d times on a live flock-body holder", kills.Load())
	}
	if !bytes.Equal(before, readEpochBytes(t, project)) {
		t.Fatal("epoch record changed; pre-fence abort must write nothing")
	}
	if len(liveHolders) != 1 || liveHolders[0].Pid != bodyPid {
		t.Fatalf("detection = %+v, want live flock-body holder pid=%d", liveHolders, bodyPid)
	}
}

// Test 4 (plan): all-dead cleanup proceeds exactly as today. Owner,
// prior candidate, and flock-body holder ALL dead: fencing write + kill
// fn invoked with today's semantics; acquire succeeds.
func TestKP6_AllDeadTakeoverProceedsAsToday(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	clk := &fakeClock{}
	live := newFakeLiveness()

	writeFlockBodyRaw(t, project, flockBody{
		Pid: 6666, PidStart: 555555, BootID: "test-boot-1", Mono: 0,
	})
	relHolder := holdFlock(t, project)

	writeEpochRaw(t, project, epochRecord{
		Epoch: 6, State: stateFencing,
		Owner:     identity{Pid: 4242, PidStart: 222222, AgentID: "old", Project: project},
		Candidate: identity{Pid: 5555, PidStart: 333333, AgentID: "prior-cand", Project: project},
		BootID:    "test-boot-1", RenewedAtMono: 0,
	})

	cfg := testCfg(clk, live)
	var kills atomic.Int64
	var fenceObservedBeforeKill atomic.Bool
	cfg.killStub = func(identity, int64) error {
		if readEpochFor(t, project).State == stateFencing {
			fenceObservedBeforeKill.Store(true)
		}
		kills.Add(1)
		relHolder()
		return nil
	}
	clk.advance(31 * time.Second)

	lease, acquired, liveHolders, err := acquireLeaseDetect(project, "cand", cfg)
	if err != nil {
		t.Fatalf("acquireLeaseDetect: %v", err)
	}
	if !acquired || lease == nil {
		t.Fatalf("all-dead takeover must acquire; got acquired=%v holders=%+v", acquired, liveHolders)
	}
	defer lease.Release()
	if kills.Load() == 0 {
		t.Fatal("kill fn must still be invoked on the all-dead path (today's semantics)")
	}
	if !fenceObservedBeforeKill.Load() {
		t.Fatal("fence-before-kill order violated on the all-dead path")
	}
	rec := readEpochFor(t, project)
	if rec.State != stateActive || rec.Owner.AgentID != "cand" || rec.Epoch != 7 {
		t.Fatalf("post-takeover record = %+v, want epoch 7 active owner=cand", rec)
	}
}

// Test 6 (plan, coordlock half): standby poll no livelock, no epoch
// inflation. Repeated aborted takeovers against the same live-stale
// leader must leave the epoch record byte-identical across rounds (a
// post-fence abort would inflate the epoch every round — the round-2
// review P0 this placement exists to prevent).
func TestKP6_RepeatedAbortsKeepEpochConstant(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	clk := &fakeClock{}
	live := newFakeLiveness()

	const holderPid = 4242
	const holderStart = int64(222222)
	live.set(holderPid, holderStart)
	holdFlock(t, project)

	writeEpochRaw(t, project, epochRecord{
		Epoch: 5, State: stateActive,
		Owner:  identity{Pid: holderPid, PidStart: holderStart, AgentID: "old", Project: project},
		BootID: "test-boot-1", RenewedAtMono: 0,
	})
	before := readEpochBytes(t, project)
	cfg := testCfg(clk, live)
	clk.advance(31 * time.Second)

	for round := 0; round < 3; round++ {
		lease, acquired, liveHolders, err := acquireLeaseDetect(project, "cand", cfg)
		if err != nil || acquired || lease != nil {
			t.Fatalf("round %d: want stand-down (nil err), got acquired=%v err=%v", round, acquired, err)
		}
		if len(liveHolders) != 1 || liveHolders[0].Pid != holderPid {
			t.Fatalf("round %d: detection = %+v, want live holder pid=%d", round, liveHolders, holderPid)
		}
		if !bytes.Equal(before, readEpochBytes(t, project)) {
			t.Fatalf("round %d: epoch record mutated across aborted rounds", round)
		}
		clk.advance(2 * time.Second)
	}
}
