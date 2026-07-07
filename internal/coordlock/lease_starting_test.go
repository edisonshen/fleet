//go:build linux || darwin

package coordlock

import (
	"testing"
	"time"
)

// startingCfg is testCfg plus the two-phase tunables + startAsStarting, so a
// real-coordinator acquire writes `starting` and the TTL boundaries are driven
// by the injected fakeClock (not the wall clock).
func startingCfg(clk *fakeClock, live *fakeLiveness) leaseConfig {
	cfg := testCfg(clk, live)
	cfg.startAsStarting = true
	cfg.startingTTL = 120 * time.Second
	cfg.handoffTTL = 90 * time.Second
	return cfg
}

// T1: a fresh real-coordinator acquire writes state=starting; Activate flips
// it to active in place (same epoch), reusing the heartbeatOnce guard.
func TestT1_AcquireStarting_ActivateFlipsToActive(t *testing.T) {
	setupHome(t)
	const project = "t1-start"
	clk := &fakeClock{}
	live := newFakeLiveness()

	lease, acquired, _, err := acquireLeaseDetect(project, "coordA", startingCfg(clk, live))
	if err != nil || !acquired {
		t.Fatalf("acquire: acquired=%v err=%v", acquired, err)
	}
	t.Cleanup(lease.Release)
	if rec := readEpochFor(t, project); rec.Epoch != 1 || rec.State != stateStarting {
		t.Fatalf("fresh acquire must be epoch 1 starting, got %d/%s", rec.Epoch, rec.State)
	}
	ok, err := lease.Activate()
	if err != nil || !ok {
		t.Fatalf("Activate: ok=%v err=%v", ok, err)
	}
	if rec := readEpochFor(t, project); rec.Epoch != 1 || rec.State != stateActive {
		t.Fatalf("after Activate: epoch 1 active, got %d/%s", rec.Epoch, rec.State)
	}
}

// T6f: a superseded `starting` owner that un-wedges and attempts its flip
// finds the epoch bumped -> Activate FAILS CLOSED (no write); the replacement's
// record is untouched. Two owners can never coexist.
func TestT6f_SupersededStarterActivateFailsClosed(t *testing.T) {
	setupHome(t)
	const project = "t6f"
	clk := &fakeClock{}
	live := newFakeLiveness()

	lease, acquired, _, err := acquireLeaseDetect(project, "wedged", startingCfg(clk, live))
	if err != nil || !acquired {
		t.Fatalf("acquire: acquired=%v err=%v", acquired, err)
	}
	t.Cleanup(lease.Release)
	before := readEpochFor(t, project)
	if before.State != stateStarting {
		t.Fatalf("precondition: state=%s want starting", before.State)
	}

	// A resolver superseded the wedged starter (pure record CAS, epoch bump).
	ok, err := supersedeStartingWithCfg(project, before.Epoch, startingCfg(clk, live))
	if err != nil || !ok {
		t.Fatalf("SupersedeStartingLease: ok=%v err=%v", ok, err)
	}
	superseded := readEpochFor(t, project)
	if superseded.Epoch != before.Epoch+1 {
		t.Fatalf("supersede must bump epoch: got %d want %d", superseded.Epoch, before.Epoch+1)
	}

	// The wedged starter un-wedges and attempts its flip -> fail closed.
	flipped, err := lease.Activate()
	if err != nil {
		t.Fatalf("Activate returned err: %v", err)
	}
	if flipped {
		t.Fatal("superseded starter's Activate must fail closed (return false), got true")
	}
	after := readEpochFor(t, project)
	if after.Epoch != superseded.Epoch || after.State != stateStarting {
		t.Fatalf("replacement record must be untouched by the refused flip: got %d/%s want %d/starting",
			after.Epoch, after.State, superseded.Epoch)
	}
}

// codex D2 iter-1 [P1] regression: a coord that acquired the lease as
// `starting` but exits on a PRE-activation failure path (never flipped to
// active) must have its record demoted to `released` by Release() — NOT left
// behind as `starting`. A leftover `starting` record reads as a live lease
// generation (LeaseRecordActive), so an un-reaped one would keep handoff
// delivery pending until startingTTL even though the owner is already gone.
func TestReleaseDemotesStartingRecord(t *testing.T) {
	setupHome(t)
	const project = "rel-starting"
	clk := &fakeClock{}
	live := newFakeLiveness()

	lease, acquired, _, err := acquireLeaseDetect(project, "crashboot", startingCfg(clk, live))
	if err != nil || !acquired {
		t.Fatalf("acquire: acquired=%v err=%v", acquired, err)
	}
	if rec := readEpochFor(t, project); rec.State != stateStarting {
		t.Fatalf("precondition: state=%s want starting", rec.State)
	}
	// Exit before Activate (pre-activation failure) -> Release must reap it.
	lease.Release()
	rec := readEpochFor(t, project)
	if rec.State != stateReleased {
		t.Fatalf("Release must demote a starting record to released, got %s", rec.State)
	}
	if LeaseRecordActive(project) {
		t.Fatal("a released (was-starting) record must not read as a live lease")
	}
}

// codex D2 iter-2 [P1] regression: a FRESH `starting` record (written by the
// real acquire path, NOT writeEpochRaw) must read as within-startingTTL
// immediately — it carries BootID + RenewedAtMono so a concurrent resolver
// WAITs on a healthy boot instead of superseding it. Before the fix these
// fields were dropped, so every fresh starter read as past-TTL at birth.
func TestFreshAcquire_StartingRecordIsWithinTTL(t *testing.T) {
	setupHome(t)
	const project = "fresh-within-ttl"
	clk := &fakeClock{ns: int64(time.Hour)}
	live := newFakeLiveness()
	cfg := startingCfg(clk, live)

	lease, acquired, _, err := acquireLeaseDetect(project, "boot01", cfg)
	if err != nil || !acquired {
		t.Fatalf("acquire: acquired=%v err=%v", acquired, err)
	}
	t.Cleanup(lease.Release)

	st, ok := currentStartingWithCfg(project, cfg)
	if !ok {
		t.Fatal("CurrentStarting must report the fresh starting record")
	}
	if !st.WithinTTL {
		rec := readEpochFor(t, project)
		t.Fatalf("fresh starter must be within TTL; got WithinTTL=false (bootID=%q renewedAtMono=%d)",
			rec.BootID, rec.RenewedAtMono)
	}
}

// A FRESH pre-spawn claim (ClaimStartingRecord) must likewise read as
// within-TTL, so a second concurrent claim sees a live boot-in-flight and
// stands down instead of overwriting it (spawn-serialization).
func TestFreshClaim_StartingRecordIsWithinTTL(t *testing.T) {
	setupHome(t)
	const project = "fresh-claim-within-ttl"
	clk := &fakeClock{ns: int64(time.Hour)}
	live := newFakeLiveness()
	cfg := startingCfg(clk, live)

	if ok, err := claimStartingWithCfg(project, "newA", cfg); err != nil || !ok {
		t.Fatalf("claim on free lease: ok=%v err=%v", ok, err)
	}
	st, ok := currentStartingWithCfg(project, cfg)
	if !ok || !st.WithinTTL {
		rec := readEpochFor(t, project)
		t.Fatalf("fresh claim must be within TTL; ok=%v withinTTL=%v (bootID=%q renewedAtMono=%d)",
			ok, st.WithinTTL, rec.BootID, rec.RenewedAtMono)
	}
	// A second claim must now stand down (not overwrite the live boot).
	if ok, err := claimStartingWithCfg(project, "newB", cfg); err != nil {
		t.Fatalf("second claim err: %v", err)
	} else if ok {
		t.Fatal("second claim must refuse: a within-TTL boot is in flight")
	}
}

// codex D2 iter-3 [P1]: a within-startingTTL `starting` record whose owner pid
// was stamped (post-claim, a real coord acquired) but is now DEAD (pre-
// activation crash) is CLAIMABLE immediately — recovery must not wait out the
// full TTL. A within-TTL record whose owner is LIVE stays unclaimable.
func TestClaimStarting_WithinTTLDeadOwnerClaimable(t *testing.T) {
	setupHome(t)
	clk := &fakeClock{ns: int64(time.Hour)}
	live := newFakeLiveness()
	cfg := startingCfg(clk, live)

	// Dead owner (pid not in the live map) within TTL -> claimable now.
	const dead = "crash-dead"
	writeEpochRaw(t, dead, epochRecord{
		Epoch: 4, State: stateStarting, BootID: "test-boot-1",
		Owner:         identity{Pid: 424242, PidStart: 999, AgentID: "crashboot", Project: dead},
		RenewedAtMono: clk.now(), // fresh -> within TTL
	})
	if ok, err := claimStartingWithCfg(dead, "newA", cfg); err != nil || !ok {
		t.Fatalf("within-TTL dead-owner starting must be claimable now: ok=%v err=%v", ok, err)
	}

	// Control: a LIVE owner within TTL stays unclaimable (boot in flight).
	const liveP = "live-boot"
	live.set(7373, 555)
	writeEpochRaw(t, liveP, epochRecord{
		Epoch: 4, State: stateStarting, BootID: "test-boot-1",
		Owner:         identity{Pid: 7373, PidStart: 555, AgentID: "booting", Project: liveP},
		RenewedAtMono: clk.now(),
	})
	if ok, err := claimStartingWithCfg(liveP, "newB", cfg); err != nil {
		t.Fatalf("claim err: %v", err)
	} else if ok {
		t.Fatal("within-TTL LIVE-owner starting must stay unclaimable")
	}

	// codex D3 iter-5 [P2] regression: a LIVE owner PAST TTL (wedged, but
	// alive) must ALSO stay unclaimable — this primitive must never clobber
	// a live process. The only sanctioned path to dethrone a live wedged
	// starter is the resolver's Supersede+SpawnStandby record-CAS-first
	// sequence (T6s), not a raw ClaimStartingRecord.
	const wedgedLive = "wedged-live-boot"
	live.set(8484, 666)
	writeEpochRaw(t, wedgedLive, epochRecord{
		Epoch: 4, State: stateStarting, BootID: "test-boot-1",
		Owner: identity{Pid: 8484, PidStart: 666, AgentID: "wedgedboot", Project: wedgedLive},
		// stale: past startingTTL.
		RenewedAtMono: clk.now() - int64(cfg.startingTTL) - int64(time.Second),
	})
	if ok, err := claimStartingWithCfg(wedgedLive, "newC", cfg); err != nil {
		t.Fatalf("claim err: %v", err)
	} else if ok {
		t.Fatal("past-TTL LIVE-owner starting must stay unclaimable (only Supersede+SpawnStandby may dethrone it)")
	}
}

// codex D2 iter-3 [P2]: SupersedeStartingLease returns ok=FALSE when the record
// already advanced past observedEpoch (another resolver bumped it, or a
// replacement went active/released), so Resolve re-resolves instead of
// spawning a standby from a stale snapshot.
func TestSupersede_NewerEpochReResolves(t *testing.T) {
	setupHome(t)
	const project = "sup-newer"
	clk := &fakeClock{ns: int64(time.Hour)}
	live := newFakeLiveness()
	cfg := startingCfg(clk, live)

	// The record is already at epoch 9; a resolver observed the stale epoch 7.
	writeEpochRaw(t, project, epochRecord{
		Epoch: 9, State: stateStarting, BootID: "test-boot-1",
		Owner:         identity{Pid: 8888, PidStart: 333, AgentID: "newer", Project: project},
		RenewedAtMono: clk.now(),
	})
	ok, err := supersedeStartingWithCfg(project, 7, cfg)
	if err != nil {
		t.Fatalf("supersede err: %v", err)
	}
	if ok {
		t.Fatal("supersede of a record already past observedEpoch must return false (re-resolve), not true")
	}
	// The record must be untouched (no spurious epoch bump).
	if rec := readEpochFor(t, project); rec.Epoch != 9 {
		t.Fatalf("record must be untouched at epoch 9, got %d", rec.Epoch)
	}
}

// SupersedeStartingLease is a benign no-op if the record already flipped to
// active (a race where the starter won before the resolver superseded).
func TestSupersede_NoOpIfActivated(t *testing.T) {
	setupHome(t)
	const project = "sup-noop"
	clk := &fakeClock{}
	live := newFakeLiveness()

	lease, _, _, err := acquireLeaseDetect(project, "c", startingCfg(clk, live))
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	t.Cleanup(lease.Release)
	observed := readEpochFor(t, project).Epoch
	if ok, _ := lease.Activate(); !ok {
		t.Fatal("Activate should have flipped")
	}
	ok, err := supersedeStartingWithCfg(project, observed, startingCfg(clk, live))
	if err != nil {
		t.Fatalf("supersede err: %v", err)
	}
	if ok {
		t.Fatal("supersede of an already-active lease must be a no-op (ok=false)")
	}
	if rec := readEpochFor(t, project); rec.State != stateActive {
		t.Fatalf("active record must be untouched, got %s", rec.State)
	}
}

// CurrentStarting reports WithinTTL true just under startingTTL and false past
// it (same boot, injected clock).
func TestCurrentStarting_TTLBoundary(t *testing.T) {
	setupHome(t)
	const project = "cs-ttl"
	clk := &fakeClock{ns: int64(time.Hour)}
	live := newFakeLiveness()
	cfg := startingCfg(clk, live)
	live.set(4242, 999)

	writeEpochRaw(t, project, epochRecord{
		Epoch: 3, State: stateStarting, BootID: "test-boot-1",
		Owner:         identity{Pid: 4242, PidStart: 999, AgentID: "boot", Project: project},
		RenewedAtMono: clk.now(),
	})
	st, ok := currentStartingWithCfg(project, cfg)
	if !ok || !st.WithinTTL || !st.OwnerLive {
		t.Fatalf("fresh starting: ok=%v withinTTL=%v ownerLive=%v", ok, st.WithinTTL, st.OwnerLive)
	}
	if st.Epoch != 3 {
		t.Fatalf("epoch=%d want 3", st.Epoch)
	}
	clk.advance(cfg.startingTTL + time.Second)
	st, ok = currentStartingWithCfg(project, cfg)
	if !ok || st.WithinTTL {
		t.Fatalf("past startingTTL: ok=%v withinTTL=%v (want ok=true, withinTTL=false)", ok, st.WithinTTL)
	}
}

// CurrentHandoff returns the reservation within TTL and nothing past it.
func TestCurrentHandoff_ReserveAndExpire(t *testing.T) {
	setupHome(t)
	const project = "ho-res"
	clk := &fakeClock{ns: int64(time.Hour)}
	live := newFakeLiveness()
	cfg := startingCfg(clk, live)

	// An owner record exists (ReserveHandoff edits the current record in place).
	writeEpochRaw(t, project, epochRecord{
		Epoch: 5, State: stateActive, BootID: "test-boot-1",
		Owner:         identity{Pid: 8888, PidStart: 333, AgentID: "old", Project: project},
		RenewedAtMono: clk.now(),
	})
	if ok, err := reserveHandoffWithCfg(project, "succ-1", 90*time.Second, cfg); err != nil || !ok {
		t.Fatalf("ReserveHandoff: ok=%v err=%v", ok, err)
	}
	h, ok := currentHandoffWithCfg(project, cfg)
	if !ok || h.SuccessorID != "succ-1" {
		t.Fatalf("reservation: ok=%v successor=%q", ok, h.SuccessorID)
	}
	clk.advance(91 * time.Second)
	if _, ok := currentHandoffWithCfg(project, cfg); ok {
		t.Fatal("expired reservation must read as absent")
	}
}

// ClaimStartingRecord writes a starting record when the lease is free, and
// refuses when a healthy active owner exists.
func TestClaimStartingRecord_FreeAndBlocked(t *testing.T) {
	setupHome(t)
	clk := &fakeClock{ns: int64(time.Hour)}
	live := newFakeLiveness()
	cfg := startingCfg(clk, live)

	// Free lease -> claim writes a starting record.
	const free = "claim-free"
	ok, err := claimStartingWithCfg(free, "newA", cfg)
	if err != nil || !ok {
		t.Fatalf("claim on free lease: ok=%v err=%v", ok, err)
	}
	if rec := readEpochFor(t, free); rec.State != stateStarting || rec.Owner.AgentID != "newA" {
		t.Fatalf("claim record: %s/%s", rec.State, rec.Owner.AgentID)
	}

	// Healthy active owner -> claim refuses.
	const busy = "claim-busy"
	live.set(7777, 222)
	writeEpochRaw(t, busy, epochRecord{
		Epoch: 2, State: stateActive, BootID: "test-boot-1",
		Owner:         identity{Pid: 7777, PidStart: 222, AgentID: "live", Project: busy},
		RenewedAtMono: clk.now(),
	})
	ok, err = claimStartingWithCfg(busy, "newB", cfg)
	if err != nil {
		t.Fatalf("claim err: %v", err)
	}
	if ok {
		t.Fatal("claim must refuse when a healthy active owner exists")
	}
}

// codex-adversarial review finding: LiveOwner (the resolver's #1
// decision-matrix branch, ATTACH-gate) must never report a CROSS-BOOT
// record's owner as live, even when its raw pid+pid_start happens to
// match a currently-live process. On Linux, pidStartNanos is boot-relative
// (platform_linux.go) — comparable for equality ONLY within the same boot
// — so a stale pre-reboot record's (pid, pid_start) pair can coincidentally
// collide with an unrelated post-reboot process (e.g. a low-numbered,
// deterministic-timing early-boot daemon). Every other pidAlive caller
// (holderHealthy) already gates on BootID first; this locks in that
// LiveOwner and CurrentStarting.OwnerLive do too.
func TestLiveOwner_CrossBootRecordNeverLive(t *testing.T) {
	setupHome(t)
	clk := &fakeClock{ns: int64(time.Hour)}
	live := newFakeLiveness()
	cfg := startingCfg(clk, live)

	// Same-boot, pid genuinely live -> LiveOwner reports it.
	const sameBoot = "liveowner-sameboot"
	live.set(4242, 111)
	writeEpochRaw(t, sameBoot, epochRecord{
		Epoch: 1, State: stateActive, BootID: "test-boot-1",
		Owner:         identity{Pid: 4242, PidStart: 111, AgentID: "coordA", Project: sameBoot},
		RenewedAtMono: clk.now(),
	})
	if owner, ok := liveOwnerWithCfg(sameBoot, cfg); !ok || owner.AgentID != "coordA" {
		t.Fatalf("same-boot live owner must be reported: ok=%v owner=%+v", ok, owner)
	}

	// Cross-boot record naming the SAME (pid, pid_start) that IS currently
	// live (the exact collision scenario) -> must NOT be reported live.
	const crossBoot = "liveowner-crossboot"
	writeEpochRaw(t, crossBoot, epochRecord{
		Epoch: 1, State: stateActive, BootID: "stale-boot-from-before-reboot",
		Owner:         identity{Pid: 4242, PidStart: 111, AgentID: "coordA-stale", Project: crossBoot},
		RenewedAtMono: clk.now(),
	})
	if owner, ok := liveOwnerWithCfg(crossBoot, cfg); ok {
		t.Fatalf("cross-boot record must NEVER be reported live even on a pid+pid_start match: got owner=%+v", owner)
	}

	// Same collision, but via CurrentStarting.OwnerLive (the `starting`
	// counterpart) — must also refuse.
	const crossBootStarting = "starting-crossboot"
	writeEpochRaw(t, crossBootStarting, epochRecord{
		Epoch: 1, State: stateStarting, BootID: "stale-boot-from-before-reboot",
		Owner:         identity{Pid: 4242, PidStart: 111, AgentID: "coordA-stale", Project: crossBootStarting},
		RenewedAtMono: clk.now(),
	})
	st, ok := currentStartingWithCfg(crossBootStarting, cfg)
	if !ok {
		t.Fatal("CurrentStarting must still report the record (state=starting), just not as live")
	}
	if st.OwnerLive {
		t.Fatal("CurrentStarting.OwnerLive must be false for a cross-boot record, even on a pid+pid_start match")
	}
	if st.WithinTTL {
		t.Fatal("a cross-boot record must also never read as WithinTTL")
	}
}

// T11: writeEpochLocked leaves a durable, re-readable record (parent-dir fsync
// path exercised on the happy write).
func TestT11_EpochWriteDurableReadback(t *testing.T) {
	setupHome(t)
	const project = "t11"
	clk := &fakeClock{ns: int64(time.Hour)}
	live := newFakeLiveness()
	lease, _, _, err := acquireLeaseDetect(project, "c", startingCfg(clk, live))
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	t.Cleanup(lease.Release)
	rec := readEpochFor(t, project)
	if rec.RenewedAtMono == 0 || rec.RenewedAtWall == 0 {
		t.Fatalf("write must stamp renewed_at: mono=%d wall=%d", rec.RenewedAtMono, rec.RenewedAtWall)
	}
}
