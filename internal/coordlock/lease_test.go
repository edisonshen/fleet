//go:build linux || darwin

package coordlock

import (
	"encoding/json"
	"errors"
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

// ---- flock-only test harness (PR-2) ----
//
// The lease primitive is exercised through acquireLease (the internal core)
// with an injected leaseConfig. Every seam is deterministic: a fake monotonic
// clock (drives only the DORMANT LeaseToken.StillOwned self-expiry — no reader
// gates on it any more), a fake pid-liveness map, an injectable ppid walk (the
// skill-side ownership proof), and a fixed boot id. There is NO heartbeat, no
// TTL, no kill stub — the flock is the only lease. No time.Sleep timing
// assertions.

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

// fakeLiveness models pid -> pid_start for live processes. A pid absent from
// the map is "dead". The real self pid is always kept live (acquire reads its
// own pid_start via selfIdentity).
type fakeLiveness struct {
	mu   sync.Mutex
	live map[int]int64
}

const selfStart = int64(111111)

func newFakeLiveness() *fakeLiveness {
	return &fakeLiveness{live: map[int]int64{os.Getpid(): selfStart}}
}

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

// testCfg builds a flock-only leaseConfig wired to the fakes. ttl bounds only
// the dormant StillOwned self-expiry clause; the fake clock controls it, not
// the wall clock.
func testCfg(clk *fakeClock, live *fakeLiveness) leaseConfig {
	return leaseConfig{
		ttl:      30 * time.Second,
		nowMono:  clk.now,
		pidStart: live.get,
		boot:     func() string { return "test-boot-1" },
		ppid:     ppidOf,
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

// heldFlock is the flock-only reader-test primitive: it takes a real LOCK_EX on
// project's coordinator.flock (so the LOCK_SH ownership probe reads BUSY⇒owner,
// the kernel-proven liveness the readers now trust) and stamps the given body
// (nil ⇒ empty/torn body, an identity-less holder). The LOCK_EX is dropped + the
// fd closed via t.Cleanup (and via the returned release func, for tests that
// model a graceful mid-test Release()).
func heldFlock(t *testing.T, project string, body *flockBody) func() {
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
	if body != nil {
		raw, merr := json.Marshal(*body)
		if merr != nil {
			t.Fatalf("marshal flock body: %v", merr)
		}
		if _, werr := f.Write(raw); werr != nil {
			t.Fatalf("write flock body: %v", werr)
		}
		_ = f.Sync()
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

// writeFlockBodyRaw stamps a flock body onto a FREE coordinator.flock file (no
// LOCK held) — models a stale body left by a released/dead holder. Used by the
// lease-check "flock free + stale body" case (T12c) and the acquire-path /
// LeaseRecordActive collapse tests (T20/T21).
func writeFlockBodyRaw(t *testing.T, project string, body flockBody) {
	t.Helper()
	paths, err := resolvePaths(project)
	if err != nil {
		t.Fatalf("resolvePaths: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.flock), 0o755); err != nil {
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

// writeEpochRaw writes an epoch record directly. Retained ONLY for the DORMANT
// LeaseToken.StillOwned / GuardWithLease-family tests (rpo_test.go) that build a
// token against a hand-written epoch — PR-2 stopped WRITING the epoch through
// the lease, so no acquire path touches it, but the read-only vestige + its
// PR-3-dormant machinery still round-trip it.
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

// assertReaders checks that ALL FIVE ownership readers agree on the flock-only
// truth for project (PR-2 D9a repointed the last two, so every reader is now a
// LOCK_SH busy-probe of the flock):
//
//	LeaseRecordActive / LiveOwner / LeaderPresent : busy⇒present (even for an
//	                                                 identity-less body).
//	CurrentOwner / CurrentActiveOwnerPID          : additionally require a
//	                                                 readable agent_id / pid
//	                                                 (identity-pending otherwise).
//
// wantPresent=false means the flock is free (no owner) for every reader.
func assertReaders(t *testing.T, project string, wantPresent bool, wantAgentID string, wantPID int) {
	t.Helper()
	if got := LeaseRecordActive(project); got != wantPresent {
		t.Errorf("LeaseRecordActive = %v, want %v", got, wantPresent)
	}
	if got := LeaderPresent(project); got != wantPresent {
		t.Errorf("LeaderPresent = %v, want %v", got, wantPresent)
	}
	// LiveOwner: spawn gate — busy⇒ok=true even for an identity-less body.
	lo, ok := LiveOwner(project)
	if ok != wantPresent {
		t.Errorf("LiveOwner ok = %v, want %v", ok, wantPresent)
	}
	if ok && lo.AgentID != wantAgentID {
		t.Errorf("LiveOwner.AgentID = %q, want %q", lo.AgentID, wantAgentID)
	}
	// CurrentOwner: delivery — requires a readable agent_id AND pid.
	co, cok := CurrentOwner(project)
	wantCoOK := wantPresent && wantAgentID != "" && wantPID > 0
	if cok != wantCoOK {
		t.Errorf("CurrentOwner ok = %v, want %v", cok, wantCoOK)
	}
	if cok && (co.AgentID != wantAgentID || co.PID != wantPID) {
		t.Errorf("CurrentOwner = %+v, want agent=%q pid=%d", co, wantAgentID, wantPID)
	}
	// CurrentActiveOwnerPID: the kill/gc reader — busy AND a readable pid.
	pid, pok := CurrentActiveOwnerPID(project)
	wantPidOK := wantPresent && wantPID > 0
	if pok != wantPidOK {
		t.Errorf("CurrentActiveOwnerPID ok = %v, want %v", pok, wantPidOK)
	}
	if pok && pid != wantPID {
		t.Errorf("CurrentActiveOwnerPID = %d, want %d", pid, wantPID)
	}
}

// TestFlockBodyOwnerReaders is the flock-only reader matrix (T1/T6/T9/T15/T21):
// with the epoch write path deleted, ALL FIVE readers are a LOCK_SH busy-probe
// of coordinator.flock. A busy flock (real LOCK_EX via heldFlock) makes each
// name the holder — kernel-proven, epoch-independent. A free/absent flock makes
// each report no owner. Body variants exercise the identity split: a full body
// ⇒ deliverable owner; an old-schema/torn body ⇒ present (busy) but delivery/pid
// readers identity-pending. No fakeClock/pid seam needed — the flock probe is
// the oracle.
func TestFlockBodyOwnerReaders(t *testing.T) {
	const holderPID = 4242
	full := &flockBody{Pid: holderPID, PidStart: 222222, AgentID: "holder01", Project: "p", BootID: "b"}
	cases := []struct {
		name        string
		hold        bool
		createFree  bool // create coordinator.flock but do NOT hold it (free)
		body        *flockBody
		rawBody     string // when set, overwrite the held flock with these raw bytes (unparseable)
		wantPresent bool
		wantAgentID string
		wantPID     int
	}{
		// T1/T6: full body ⇒ every reader names the holder; delivery deliverable.
		{name: "T1_T6_full_body_names_holder", hold: true, body: full, wantPresent: true, wantAgentID: "holder01", wantPID: holderPID},
		// T15: old-schema body (pid, NO agent_id) — the bridge is deleted (D9c),
		// so busy⇒present with a pid but delivery is identity-pending (no agent_id).
		{name: "T15_old_schema_no_agentid", hold: true, wantPresent: true, wantAgentID: "", wantPID: holderPID,
			body: &flockBody{Pid: holderPID, PidStart: 222222, BootID: "b"}},
		// T9: torn/empty body (mid-stamp). Busy⇒present; no pid/id ⇒ delivery + pid
		// readers degrade to identity-pending, NOT "no owner".
		{name: "T9_torn_empty_body", hold: true, body: nil, wantPresent: true, wantAgentID: "", wantPID: 0},
		// T9b: torn NON-empty body (unparseable JSON mid-write). Busy⇒present still
		// holds, identity degrades to pending (not "no owner").
		{name: "T9b_torn_unparseable_body", hold: true, rawBody: "{not valid json", wantPresent: true, wantAgentID: "", wantPID: 0},
		// flock file exists but is FREE (no holder) ⇒ no owner.
		{name: "free_flock", createFree: true, wantPresent: false},
		// flock file never created (ENOENT) ⇒ no owner, no crash.
		{name: "enoent_never_created", wantPresent: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setupHome(t)
			const project = "flock-readers"
			switch {
			case tc.hold:
				heldFlock(t, project, tc.body)
				if tc.rawBody != "" {
					// Overwrite the (empty) held body with raw unparseable bytes.
					// os.WriteFile uses its own fd — flock is advisory, so the held
					// LOCK_EX still makes the probe read BUSY.
					paths, _ := resolvePaths(project)
					if err := os.WriteFile(paths.flock, []byte(tc.rawBody), 0o644); err != nil {
						t.Fatal(err)
					}
				}
			case tc.createFree:
				paths, _ := resolvePaths(project)
				if err := os.MkdirAll(filepath.Dir(paths.flock), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(paths.flock, nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			assertReaders(t, project, tc.wantPresent, tc.wantAgentID, tc.wantPID)
		})
	}
}

// TestReadersRepointedToFlock_EpochIgnored (T15 — D9a): the last two deferred
// readers (CurrentActiveOwnerPID + LeaderPresent) are repointed onto the flock
// in PR-2. A busy flock whose body names holder A, with a STALE epoch on disk
// naming a DIFFERENT owner B, must resolve to the FLOCK holder A for EVERY
// reader — the epoch no longer governs any ownership read.
func TestReadersRepointedToFlock_EpochIgnored(t *testing.T) {
	setupHome(t)
	const project = "flock-epoch-ignored"
	const holderPID = 4242
	heldFlock(t, project, &flockBody{Pid: holderPID, PidStart: 222222, AgentID: "flockA", Project: project, BootID: "b"})
	// A stale epoch naming a different owner B — must be ignored by all readers.
	writeEpochRaw(t, project, epochRecord{
		Epoch: 9, State: stateActive,
		Owner:  identity{Pid: 5555, PidStart: 333333, AgentID: "epochB", Project: project},
		BootID: "test-boot-1", RenewedAtMono: 0,
	})
	assertReaders(t, project, true, "flockA", holderPID)
	// Explicit: the two repointed readers name the FLOCK holder, not epoch B.
	if pid, ok := CurrentActiveOwnerPID(project); !ok || pid != holderPID {
		t.Fatalf("CurrentActiveOwnerPID = %d ok=%v, want %d (flock, not epoch B)", pid, ok, holderPID)
	}
	if !LeaderPresent(project) {
		t.Fatal("LeaderPresent = false, want true (busy flock)")
	}
}

// TestEpochIdentityBridgeDeleted (T17 — D9c): the PR-1 epoch-identity bridge is
// gone. A NEW-binary body (has agent_id) resolves from the body. An OLD-schema
// body (no agent_id), EVEN WITH a matching epoch on disk that PR-1 would have
// bridged from, resolves to identity-pending (CurrentOwner ok=false) — no epoch
// read supplies the identity any more. Ownership stays the held flock
// (busy⇒present) either way, so a spawn-beside never happens.
func TestEpochIdentityBridgeDeleted(t *testing.T) {
	const pid, start = 4242, int64(222222)

	// (a) new-binary body: identity comes from the body.
	t.Run("new_binary_body_resolves", func(t *testing.T) {
		setupHome(t)
		const project = "bridge-new"
		heldFlock(t, project, &flockBody{Pid: pid, PidStart: start, AgentID: "newcoord", Project: project, BootID: "b"})
		if o, ok := CurrentOwner(project); !ok || o.AgentID != "newcoord" {
			t.Fatalf("CurrentOwner = %+v ok=%v, want newcoord (from body)", o, ok)
		}
	})

	// (b) old-schema body + a matching epoch on disk ⇒ still identity-pending
	// (bridge deleted). LiveOwner present, CurrentOwner ok=false.
	t.Run("old_schema_body_matching_epoch_not_bridged", func(t *testing.T) {
		setupHome(t)
		const project = "bridge-old"
		heldFlock(t, project, &flockBody{Pid: pid, PidStart: start, BootID: "test-boot-1"}) // NO agent_id
		writeEpochRaw(t, project, epochRecord{
			Epoch: 5, State: stateActive, BootID: "test-boot-1",
			Owner: identity{Pid: pid, PidStart: start, AgentID: "oldcoord", Project: project},
		})
		if o, ok := CurrentOwner(project); ok {
			t.Fatalf("CurrentOwner ok=true (%+v); the epoch bridge is deleted — want identity-pending", o)
		}
		if o, ok := LiveOwner(project); !ok || o.AgentID != "" {
			t.Fatalf("LiveOwner = %+v ok=%v, want owner-present identity-less", o, ok)
		}
		if !LeaseRecordActive(project) {
			t.Fatal("LeaseRecordActive = false, want true (busy flock; identity-pending)")
		}
	})
}

// TestFlockOwner_ReleasedButAliveIsFree (T11): a holder stamped its body then
// dropped the flock (a graceful Release() drops LOCK_EX while the process still
// lives). The LOCK_SH probe ACQUIRES the freed flock ⇒ every reader reports
// FREE, even though the body still names the holder — a body-pid check alone
// would wrongly still name the alive-but-released holder. This is why the probe
// is LOCK_SH, not a body read.
func TestFlockOwner_ReleasedButAliveIsFree(t *testing.T) {
	setupHome(t)
	const project = "flock-released-alive"
	rel := heldFlock(t, project, &flockBody{Pid: 4242, PidStart: 222222, AgentID: "holder01", Project: project, BootID: "b"})
	rel() // drop LOCK_EX (Release semantics) — the body persists on disk
	assertReaders(t, project, false, "", 0)
}

// TestFlockOwner_SharedProbesCoexist is the regression test for D1's load-
// bearing LOCK_SH choice. The probe uses a SHARED lock so concurrent readers
// coexist and never make each other misread "busy". On a FREE flock, N readers
// probing at once must ALL read owner=false: with LOCK_SH they all acquire the
// shared lock together; a regression to an EXCLUSIVE probe (LOCK_EX) would let
// one reader win and the rest see EWOULDBLOCK ⇒ false-positive owner=true on a
// free flock (a spawn-gate misread). A start barrier maximizes overlap so a
// LOCK_EX regression trips. Deterministic for the correct impl (always
// all-false); no time.Sleep.
func TestFlockOwner_SharedProbesCoexist(t *testing.T) {
	setupHome(t)
	const project = "flock-shared-probes"
	paths, err := resolvePaths(project)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.flock), 0o755); err != nil {
		t.Fatal(err)
	}
	// Create the flock file but do NOT hold it -> free.
	if err := os.WriteFile(paths.flock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	const readers = 16
	for round := 0; round < 8; round++ {
		start := make(chan struct{})
		var wg sync.WaitGroup
		var busyCount atomic.Int32
		for i := 0; i < readers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start // barrier: all readers probe simultaneously
				if _, present := flockBodyOwner(project); present {
					busyCount.Add(1)
				}
			}()
		}
		close(start)
		wg.Wait()
		if n := busyCount.Load(); n != 0 {
			t.Fatalf("round %d: %d/%d concurrent readers misread a FREE flock as busy (LOCK_SH regression?)", round, n, readers)
		}
	}
}

// TestAcquireIsCoordinator_FlockOnly (T1 / T20 — D1/D9f): acquiring the flock IS
// becoming the coordinator, and the decision keys on the FLOCK ALONE (no epoch
// read, no starting state, no Activate).
func TestAcquireIsCoordinator_FlockOnly(t *testing.T) {
	// (T1) Free flock ⇒ acquire; immediately the owner; NO epoch is written.
	t.Run("free_flock_acquires_no_epoch_write", func(t *testing.T) {
		setupHome(t)
		const project = "acq-free"
		lease, acquired, err := AcquireLease(project, "cand")
		if err != nil || !acquired || lease == nil {
			t.Fatalf("AcquireLease: acquired=%v lease=%v err=%v", acquired, lease, err)
		}
		t.Cleanup(lease.Release)
		// The acquire path writes NO epoch record (the write path is deleted).
		paths, _ := resolvePaths(project)
		if _, serr := os.Stat(paths.epoch); !errors.Is(serr, os.ErrNotExist) {
			t.Fatalf("acquire wrote/expected-no coordinator.epoch, stat err=%v", serr)
		}
		// Every reader names the live holder (self).
		assertReaders(t, project, true, "cand", os.Getpid())
	})

	// (T20) Busy flock + a stale/absent epoch that (in the old code) would have
	// let the caller reacquire/take over ⇒ stand down. The epoch is NOT read.
	t.Run("busy_flock_stands_down_ignoring_epoch", func(t *testing.T) {
		setupHome(t)
		const project = "acq-busy"
		heldFlock(t, project, &flockBody{Pid: 7000, PidStart: 700700, AgentID: "holder", Project: project, BootID: "test-boot-1"})
		// A stale ACTIVE epoch naming the CALLER's own identity — the old
		// reacquire path would have promoted; flock-only must stand down.
		writeEpochRaw(t, project, epochRecord{
			Epoch: 3, State: stateActive, BootID: "test-boot-1",
			Owner: identity{Pid: os.Getpid(), PidStart: selfStart, AgentID: "cand", Project: project},
		})
		paths, _ := resolvePaths(project)
		before, _ := os.ReadFile(paths.epoch)
		lease, acquired, err := AcquireLease(project, "cand")
		if err != nil {
			t.Fatalf("AcquireLease: want nil error (stand-down shape), got %v", err)
		}
		if acquired || lease != nil {
			t.Fatalf("busy flock must stand down; got acquired=%v lease=%v", acquired, lease)
		}
		// The acquire path never touched the epoch (byte-unchanged).
		after, _ := os.ReadFile(paths.epoch)
		if string(before) != string(after) {
			t.Fatal("acquire mutated the epoch on a busy-flock stand-down (should never read/write it)")
		}
	})
}

// TestSuccessorAcquiresAfterRelease (T2): a successor acquires the flock the OLD
// coord freed with Release(). The acquire IS the commit — no epoch handoff.
func TestSuccessorAcquiresAfterRelease(t *testing.T) {
	setupHome(t)
	const project = "succ-after-release"
	first, acquired, err := AcquireLease(project, "first")
	if err != nil || !acquired {
		t.Fatalf("first acquire: acquired=%v err=%v", acquired, err)
	}
	first.Release()
	// The flock is now free.
	if _, ok := LiveOwner(project); ok {
		t.Fatal("after first.Release the flock must read free")
	}
	second, acquired2, err := AcquireLease(project, "second")
	if err != nil || !acquired2 || second == nil {
		t.Fatalf("successor acquire: acquired=%v err=%v", acquired2, err)
	}
	t.Cleanup(second.Release)
	if o, ok := CurrentOwner(project); !ok || o.AgentID != "second" {
		t.Fatalf("CurrentOwner = %+v ok=%v, want second", o, ok)
	}
}

// TestLeaseRecordActive_PureFlockBusy (T21 — D9f): LeaseRecordActive is a PURE
// flock-busy read; the epoch clause is dropped. A leftover active/terminal epoch
// record on a FREE flock must NOT make it spuriously true (which would keep a
// handoff doc PENDING forever). Busy ⇒ true; free ⇒ false regardless of any
// leftover epoch.
func TestLeaseRecordActive_PureFlockBusy(t *testing.T) {
	setupHome(t)
	const project = "lra-flock"

	// No flock at all ⇒ false.
	if LeaseRecordActive(project) {
		t.Fatal("no flock ⇒ LeaseRecordActive should be false")
	}
	// Busy flock (even identity-less body) ⇒ true.
	rel := heldFlock(t, project, nil)
	if !LeaseRecordActive(project) {
		t.Fatal("busy flock ⇒ LeaseRecordActive should be true")
	}
	rel() // free the flock; the file now exists but is unheld

	// Free flock + a leftover ACTIVE epoch record ⇒ STILL false (epoch clause
	// dropped). Before D9f the epoch clause made this spuriously true.
	writeEpochRaw(t, project, epochRecord{
		Epoch: 5, State: stateActive, BootID: "test-boot-1",
		Owner: identity{Pid: 4242, PidStart: 222222, AgentID: "owner1", Project: project},
	})
	if LeaseRecordActive(project) {
		t.Fatal("free flock + leftover active epoch ⇒ LeaseRecordActive should be false (D9f)")
	}
}

// TestStampFlockBody_FailClosed (T13 / D6): a failed flock-body stamp must fail
// the acquire and RELEASE the flock — never leave a body-less held flock a
// reader would see as identity-pending forever. Exercised via the stampErr
// fault seam: acquire fails with the injected fault, returns no lease, and a
// subsequent tryFlock SUCCEEDS (the flock was freed).
func TestStampFlockBody_FailClosed(t *testing.T) {
	setupHome(t)
	const project = "stampfail-free"
	stampFault := errors.New("injected stamp fault")
	cfg := testCfg(&fakeClock{}, newFakeLiveness())
	cfg.stampErr = stampFault

	lease, acquired, err := acquireLease(project, "cand", cfg)
	if acquired || err == nil {
		t.Fatalf("fail-closed acquire: acquired=%v err=%v", acquired, err)
	}
	if !errors.Is(err, stampFault) {
		t.Fatalf("acquire must fail via the injected stamp fault; got %v", err)
	}
	if lease != nil {
		t.Fatalf("fail-closed acquire returned a lease: %v", lease)
	}
	// The flock was released (fail-closed): a fresh acquire of it succeeds.
	paths, _ := resolvePaths(project)
	f, got, ferr := tryFlock(paths.flock)
	if ferr != nil || !got {
		t.Fatalf("flock leaked (not released) after fail-closed acquire: got=%v err=%v", got, ferr)
	}
	_ = releaseFlock(f)
}

// TestIntegration_BusyCoordReadersNameHolder (T15 integration): a real
// AcquireLease makes EVERY reader name the live holder (self) with no epoch on
// disk, and after Release every reader reports free.
func TestIntegration_BusyCoordReadersNameHolder(t *testing.T) {
	setupHome(t)
	const project = "integ-busy"
	lease, acquired, err := AcquireLease(project, "holder01")
	if err != nil || !acquired {
		t.Fatalf("AcquireLease: acquired=%v err=%v", acquired, err)
	}
	t.Cleanup(lease.Release)
	assertReaders(t, project, true, "holder01", os.Getpid())
	lease.Release()
	assertReaders(t, project, false, "", 0)
}

// ---- Platform pinning: P1, P2 ----

// P1: pid_start read matches reality — non-zero, stable across repeated reads in
// the same process, and set for a freshly-spawned child.
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
	// A freshly-spawned child started later -> non-zero, >= our start time.
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
	if cs < s1 {
		t.Fatalf("child pid_start (%d) should be >= parent (%d)", cs, s1)
	}
	if cmd.Process.Pid == self {
		t.Fatal("child pid must differ from parent pid")
	}
}

// P2: monotonic elapsed is jump-immune — two reads yield a non-negative delta.
func TestP2_MonotonicElapsedNonNegative(t *testing.T) {
	a := monotonicNanos()
	if a <= 0 {
		t.Fatalf("monotonicNanos should be positive, got %d", a)
	}
	// A tiny real sleep guarantees forward progress without asserting a specific
	// duration (no timing-flaky assertion).
	time.Sleep(1 * time.Millisecond)
	b := monotonicNanos()
	if b < a {
		t.Fatalf("monotonic clock went backward: %d -> %d", a, b)
	}
}
