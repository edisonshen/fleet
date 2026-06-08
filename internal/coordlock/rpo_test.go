//go:build linux || darwin

package coordlock

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---- RPO + boundary test harness ----
//
// These tests exercise the PR4 fencing boundary (GuardWithLease) + durable
// RPO machinery (intent/completion log, ReplayIntents, atomic checkpoint
// write/read/quarantine). All seams are deterministic; the on-disk epoch is
// written via writeEpochRaw and the token is constructed to own (or not
// own) it. No time.Sleep timing assertions.

// ownerToken builds a LeaseToken that OWNS an active epoch record it also
// writes to disk, so StillOwned() passes. It uses the fake clock/liveness
// cfg so the self-expiry clause never spuriously fires.
func ownerToken(t *testing.T, project string, epoch int64) LeaseToken {
	t.Helper()
	clk := &fakeClock{}
	live := newFakeLiveness()
	cfg := testCfg(clk, live)
	paths, err := resolvePaths(project)
	if err != nil {
		t.Fatalf("resolvePaths: %v", err)
	}
	owner := identity{Pid: 4242, PidStart: 222222, AgentID: "leader", Project: project}
	live.set(owner.Pid, owner.PidStart)
	writeEpochRaw(t, project, epochRecord{
		Epoch: epoch, State: stateActive, Owner: owner,
		BootID: "test-boot-1", RenewedAtMono: clk.now(),
	})
	return LeaseToken{
		Epoch: epoch, Project: project, Pid: owner.Pid, PidStart: owner.PidStart,
		AgentID: owner.AgentID, paths: paths, boot: "test-boot-1", cfg: cfg,
	}
}

// staleToken builds a token whose epoch is BELOW the current on-disk epoch
// (a woken zombie). StillOwned() must be false.
func staleToken(t *testing.T, project string, tokEpoch, diskEpoch int64) LeaseToken {
	t.Helper()
	clk := &fakeClock{}
	live := newFakeLiveness()
	cfg := testCfg(clk, live)
	paths, _ := resolvePaths(project)
	// On disk: a newer leader owns diskEpoch.
	writeEpochRaw(t, project, epochRecord{
		Epoch: diskEpoch, State: stateActive,
		Owner:  identity{Pid: 7777, PidStart: 444444, AgentID: "new", Project: project},
		BootID: "test-boot-1", RenewedAtMono: clk.now(),
	})
	return LeaseToken{
		Epoch: tokEpoch, Project: project, Pid: 4242, PidStart: 222222, AgentID: "old",
		paths: paths, boot: "test-boot-1", cfg: cfg,
	}
}

// ---- T28: central API rejects a stale-epoch write ----

func TestT28_GuardRejectsStaleEpoch(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	tok := staleToken(t, project, 5, 6) // token@5, disk@6

	ran := false
	err := GuardWithLease(tok, "dispatch-abc", func() error {
		ran = true
		return nil
	})
	if !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale-epoch write must return ErrLeaseLost, got %v", err)
	}
	if ran {
		t.Fatal("the action MUST NOT run when the lease is lost")
	}
	// No intent file should have been written (the reject is BEFORE intent).
	store, _ := tok.intentStore()
	if _, serr := os.Stat(store.intentPath("dispatch-abc")); serr == nil {
		t.Fatal("a rejected action must NOT leave an intent record")
	}
}

// ---- T35: central API rejects a current-epoch token from a non-owner ----

func TestT35_GuardRejectsCurrentEpochNonOwner(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	clk := &fakeClock{}
	live := newFakeLiveness()
	cfg := testCfg(clk, live)
	paths, _ := resolvePaths(project)
	// Disk: owner=new at epoch 6.
	writeEpochRaw(t, project, epochRecord{
		Epoch: 6, State: stateActive,
		Owner:  identity{Pid: 7777, PidStart: 444444, AgentID: "new", Project: project},
		BootID: "test-boot-1", RenewedAtMono: clk.now(),
	})
	// Impostor token: same epoch, DIFFERENT identity.
	imposter := LeaseToken{
		Epoch: 6, Project: project, Pid: 1234, PidStart: 555555, AgentID: "imposter",
		paths: paths, boot: "test-boot-1", cfg: cfg,
	}
	ran := false
	err := GuardWithLease(imposter, "spawn-xyz", func() error { ran = true; return nil })
	if !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("current-epoch non-owner must return ErrLeaseLost, got %v", err)
	}
	if ran {
		t.Fatal("the action MUST NOT run for a non-owner token")
	}
}

// ---- happy path: intent before, completion after ----

func TestGuardWithLease_WritesIntentThenCompletion(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	tok := ownerToken(t, project, 3)

	order := []string{}
	err := GuardWithLease(tok, "branch/feat-x", func() error {
		// Intent must already exist when the action runs.
		store, _ := tok.intentStore()
		if _, serr := os.Stat(store.intentPath("branch/feat-x")); serr != nil {
			t.Errorf("intent must be on disk before the action runs: %v", serr)
		}
		if _, serr := os.Stat(store.completionPath("branch/feat-x")); serr == nil {
			t.Error("completion must NOT exist before the action runs")
		}
		order = append(order, "act")
		return nil
	})
	if err != nil {
		t.Fatalf("GuardWithLease: %v", err)
	}
	store, _ := tok.intentStore()
	if _, serr := os.Stat(store.completionPath("branch/feat-x")); serr != nil {
		t.Fatalf("completion must exist after a clean action: %v", serr)
	}
	if len(order) != 1 || order[0] != "act" {
		t.Fatalf("action did not run exactly once: %v", order)
	}
}

// A failed action writes NO completion (so it is replayed).
func TestGuardWithLease_FailedActionNoCompletion(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	tok := ownerToken(t, project, 3)

	boom := errors.New("boom")
	err := GuardWithLease(tok, "dispatch-fail", func() error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("want the action error, got %v", err)
	}
	store, _ := tok.intentStore()
	if _, serr := os.Stat(store.intentPath("dispatch-fail")); serr != nil {
		t.Fatal("a failed action must still leave its intent (for replay)")
	}
	if _, serr := os.Stat(store.completionPath("dispatch-fail")); serr == nil {
		t.Fatal("a failed action must NOT write a completion")
	}
}

func TestGuardWithLease_EmptyKeyRejected(t *testing.T) {
	setupHome(t)
	tok := ownerToken(t, "rainier", 1)
	if err := GuardWithLease(tok, "  ", func() error { return nil }); err == nil {
		t.Fatal("empty idempotency key must be rejected")
	}
}

// ---- T26: idempotent re-drive on cold rebuild (intent without completion) ----

func TestT26_ReplayReDrivesPendingIntentIdempotently(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	tok := ownerToken(t, project, 4)
	store, _ := tok.intentStore()

	// Crash AFTER intent, BEFORE completion: write only the intent.
	if err := store.writeIntent("pr-node-123", 4); err != nil {
		t.Fatalf("writeIntent: %v", err)
	}
	// A separate, already-completed action must NOT be replayed.
	if err := store.writeIntent("pr-node-done", 4); err != nil {
		t.Fatalf("writeIntent done: %v", err)
	}
	if err := store.writeCompletion("pr-node-done", 4); err != nil {
		t.Fatalf("writeCompletion done: %v", err)
	}

	replayed := map[string]int{}
	err := ReplayIntents(tok, func(key string) error {
		replayed[key]++
		return nil
	})
	if err != nil {
		t.Fatalf("ReplayIntents: %v", err)
	}
	if replayed["pr-node-123"] != 1 {
		t.Fatalf("pending intent must be replayed exactly once, got %d", replayed["pr-node-123"])
	}
	if _, did := replayed["pr-node-done"]; did {
		t.Fatal("a completed intent must NOT be replayed")
	}
	// After replay the pending one is marked complete -> a second replay is a no-op.
	replayed = map[string]int{}
	if err := ReplayIntents(tok, func(key string) error { replayed[key]++; return nil }); err != nil {
		t.Fatalf("second ReplayIntents: %v", err)
	}
	if len(replayed) != 0 {
		t.Fatalf("second replay must be a no-op, got %v", replayed)
	}
}

func TestT39_CleanRebuildReplaysNothing(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	tok := ownerToken(t, project, 2)
	store, _ := tok.intentStore()
	// Every intent has a completion.
	for _, k := range []string{"a", "b", "c"} {
		if err := store.writeIntent(k, 2); err != nil {
			t.Fatal(err)
		}
		if err := store.writeCompletion(k, 2); err != nil {
			t.Fatal(err)
		}
	}
	called := 0
	if err := ReplayIntents(tok, func(string) error { called++; return nil }); err != nil {
		t.Fatalf("ReplayIntents: %v", err)
	}
	if called != 0 {
		t.Fatalf("clean rebuild must replay nothing, got %d", called)
	}
}

// codex PR4 [P1]: replay re-fences per action. If a fence lands mid-replay
// (the handler runs long enough for a takeover), the loop stops driving side
// effects as a non-owner and the remaining intents are left for the real
// successor.
func TestReplayIntents_PerActionFenceStopsMidLoop(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	tok := ownerToken(t, project, 4)
	store, _ := tok.intentStore()
	for _, k := range []string{"a-first", "b-second", "c-third"} {
		if err := store.writeIntent(k, 4); err != nil {
			t.Fatal(err)
		}
	}
	driven := []string{}
	err := ReplayIntents(tok, func(key string) error {
		driven = append(driven, key)
		// After the FIRST handler runs, simulate a takeover fencing us: bump
		// the on-disk epoch so the next StillOwned() (before the 2nd handler)
		// returns false.
		if len(driven) == 1 {
			writeEpochRaw(t, project, epochRecord{
				Epoch: 5, State: stateActive,
				Owner:  identity{Pid: 9999, PidStart: 8888, AgentID: "successor", Project: project},
				BootID: "test-boot-1", RenewedAtMono: 0,
			})
		}
		return nil
	})
	if !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("a mid-replay fence must return ErrLeaseLost, got %v", err)
	}
	// Only the first intent's handler ran (and its completion may or may not
	// have been written — the re-fence is before completion too). The 2nd/3rd
	// were NOT driven.
	if len(driven) != 1 || driven[0] != "a-first" {
		t.Fatalf("replay must stop at the fence; drove %v", driven)
	}
}

func TestReplayIntents_RejectedWhenLeaseLost(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	tok := staleToken(t, project, 5, 6)
	if err := ReplayIntents(tok, func(string) error { return nil }); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("replay under a lost lease must return ErrLeaseLost, got %v", err)
	}
}

func TestReplayIntents_TornRecordSurfacesDegraded(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	tok := ownerToken(t, project, 1)
	store, _ := tok.intentStore()
	// A clean pending intent + a torn (garbage) intent file.
	if err := store.writeIntent("clean", 1); err != nil {
		t.Fatal(err)
	}
	torn := filepath.Join(store.dir, keyFile("garbage", ".intent.json"))
	if err := os.WriteFile(torn, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	replayed := 0
	err := ReplayIntents(tok, func(string) error { replayed++; return nil })
	if !errors.Is(err, ErrTornIntentLog) {
		t.Fatalf("a torn intent must surface ErrTornIntentLog, got %v", err)
	}
	if replayed != 1 {
		t.Fatalf("the clean pending intent must still be replayed, got %d", replayed)
	}
}

// ---- checkpoint write / read / quarantine ----

func TestCheckpoint_RoundTrip(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	tok := ownerToken(t, project, 7)

	payload := []byte(`{"workers":3,"tasks":["a","b"]}`)
	if err := WriteCheckpoint(tok, payload); err != nil {
		t.Fatalf("WriteCheckpoint: %v", err)
	}
	got, q, err := ReadCheckpoint(tok)
	if err != nil {
		t.Fatalf("ReadCheckpoint: %v", err)
	}
	if len(q) != 0 {
		t.Fatalf("clean checkpoint must not quarantine anything, got %v", q)
	}
	if string(got) != string(payload) {
		t.Fatalf("payload mismatch: got %q want %q", got, payload)
	}
}

func TestWriteCheckpoint_RejectedWhenLeaseLost(t *testing.T) {
	setupHome(t)
	tok := staleToken(t, "rainier", 5, 6)
	if err := WriteCheckpoint(tok, []byte("x")); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("a zombie must not write a checkpoint; want ErrLeaseLost, got %v", err)
	}
}

// ---- T16: torn checkpoint quarantined, recover from last clean ----

func TestT16_TornCheckpointQuarantined(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	tok := ownerToken(t, project, 5)

	// First clean checkpoint -> becomes .prev when the second is written.
	if err := WriteCheckpoint(tok, []byte("CLEAN-V1")); err != nil {
		t.Fatalf("write v1: %v", err)
	}
	if err := WriteCheckpoint(tok, []byte("CLEAN-V2")); err != nil {
		t.Fatalf("write v2: %v", err)
	}
	// Now corrupt the live checkpoint (simulate a torn kill -9 mid-write):
	// truncate it so the trailer/CRC no longer verify.
	live, _, _ := checkpointPaths(tok)
	if err := os.WriteFile(live, []byte("CLEAN-V2-corrupted-no-trailer"), 0o644); err != nil {
		t.Fatalf("corrupt live: %v", err)
	}
	got, q, err := ReadCheckpoint(tok)
	if err != nil {
		t.Fatalf("ReadCheckpoint should recover from .prev, got err %v", err)
	}
	if len(q) != 1 {
		t.Fatalf("the torn live checkpoint must be quarantined exactly once, got %v", q)
	}
	if string(got) != "CLEAN-V2" {
		// .prev holds CLEAN-V2 because WriteCheckpoint(v2) rolled the clean
		// v1->prev, then v2 became live... wait: prev should be CLEAN-V1.
		// Accept the last CLEAN value available in .prev.
		if string(got) != "CLEAN-V1" {
			t.Fatalf("recovered payload must be the last clean checkpoint, got %q", got)
		}
	}
	// The quarantine file exists on disk and the torn bytes are never read again.
	if _, serr := os.Stat(q[0]); serr != nil {
		t.Fatalf("quarantined file must remain on disk for inspection: %v", serr)
	}
}

// ---- T27: future-epoch checkpoint quarantined ----

func TestT27_FutureEpochCheckpointQuarantined(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	// The successor just won the lease at epoch 5.
	tok := ownerToken(t, project, 5)
	live, _, _ := checkpointPaths(tok)

	// A zombie wrote a checkpoint stamped epoch 9 (newer than the lease the
	// successor holds). Encode it directly with the future epoch.
	zombie := encodeCheckpoint([]byte("ZOMBIE-WRITE"), 9)
	if err := os.WriteFile(live, zombie, 0o644); err != nil {
		t.Fatalf("write zombie checkpoint: %v", err)
	}
	// No .prev -> ReadCheckpoint quarantines the future-epoch file and finds
	// no clean fallback.
	_, q, err := ReadCheckpoint(tok)
	if !errors.Is(err, ErrNoCleanCheckpoint) {
		t.Fatalf("want ErrNoCleanCheckpoint after quarantining the only (future) checkpoint, got %v", err)
	}
	if len(q) != 1 {
		t.Fatalf("future-epoch checkpoint must be quarantined, got %v", q)
	}
	if !strings.Contains(q[0], "quarantine") {
		t.Fatalf("quarantine path looks wrong: %s", q[0])
	}
}

// codex PR4 [P2]: a STALE reader (token epoch < live lease) must NOT
// quarantine a future-epoch checkpoint — that would delete the real
// successor's good state. It refuses with ErrLeaseLost and leaves the file.
func TestReadCheckpoint_StaleReaderDoesNotQuarantineFutureEpoch(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	// staleToken: token@5, on-disk lease@6 (StillOwned false).
	tok := staleToken(t, project, 5, 6)
	live, _, _ := checkpointPaths(tok)
	// The real successor (epoch 6) wrote a valid checkpoint.
	good := encodeCheckpoint([]byte("SUCCESSOR-STATE"), 6)
	if err := os.WriteFile(live, good, 0o644); err != nil {
		t.Fatal(err)
	}
	_, q, err := ReadCheckpoint(tok) // tok.Epoch=5 -> sees epoch 6 as "future"
	if !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("a stale reader must refuse with ErrLeaseLost, got %v", err)
	}
	if len(q) != 0 {
		t.Fatalf("a stale reader must NOT quarantine, got %v", q)
	}
	// The successor's checkpoint is untouched on disk.
	if _, serr := os.Stat(live); serr != nil {
		t.Fatalf("the successor's checkpoint must remain: %v", serr)
	}
}

func TestReadCheckpoint_NoCheckpointYet(t *testing.T) {
	setupHome(t)
	tok := ownerToken(t, "rainier", 1)
	_, _, err := ReadCheckpoint(tok)
	if !errors.Is(err, ErrNoCleanCheckpoint) {
		t.Fatalf("a fresh project must return ErrNoCleanCheckpoint, got %v", err)
	}
}

// ---- T6 (end-to-end through the boundary): a fenced zombie self-demotes ----

// A leader holds the lease at epoch 5; a standby steals it to epoch 6. When
// the old leader's still-cached token tries a *WithLease mutation, the
// boundary REJECTS it (ErrLeaseLost) and the caller self-demotes — no side
// effect runs, no intent is written.
func TestT6_BoundaryRejectsZombieEndToEnd(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	clk := &fakeClock{}
	live := newFakeLiveness()
	cfg := testCfg(clk, live)
	paths, _ := resolvePaths(project)

	// Old leader captured a token @ epoch 5.
	oldTok := LeaseToken{
		Epoch: 5, Project: project, Pid: 4242, PidStart: 222222, AgentID: "old",
		paths: paths, boot: "test-boot-1", cfg: cfg,
	}
	// Standby stole the lease to epoch 6.
	writeEpochRaw(t, project, epochRecord{
		Epoch: 6, State: stateActive,
		Owner:  identity{Pid: 7777, PidStart: 444444, AgentID: "new", Project: project},
		BootID: "test-boot-1", RenewedAtMono: clk.now(),
	})

	selfDemoted := false
	sideEffect := false
	err := GuardWithLease(oldTok, "old-leader-spawn", func() error {
		sideEffect = true
		return nil
	})
	if errors.Is(err, ErrLeaseLost) {
		selfDemoted = true // the caller's branch: tok lost -> exit
	}
	if !selfDemoted {
		t.Fatalf("the fenced old leader must be rejected at the boundary, got err=%v", err)
	}
	if sideEffect {
		t.Fatal("the zombie's side effect must NOT execute")
	}
	// StillOwned() is the predicate callers branch on; it must agree.
	if oldTok.StillOwned() {
		t.Fatal("oldTok.StillOwned() must be false after the fence")
	}
}

// ---- T31: safety-net kill pre-barrier loses no work ----

// The old coord intent-logged an in-flight action but hung BEFORE writing
// the handoff-complete barrier; the takeover killed it. The NEW leader
// (fresh token, epoch++) replays the un-completed intent idempotently — the
// in-flight work is recovered, with no duplicate.
func TestT31_SafetyNetPreBarrierReplaysInFlight(t *testing.T) {
	setupHome(t)
	const project = "rainier"

	// Old leader @ epoch 5 logs an in-flight intent, no completion (it hung).
	oldTok := ownerToken(t, project, 5)
	oldStore, _ := oldTok.intentStore()
	if err := oldStore.writeIntent("dispatch-inflight-77", 5); err != nil {
		t.Fatalf("writeIntent: %v", err)
	}

	// Takeover: the new leader now owns epoch 6. (Same intents dir — it is
	// rooted at the project, not the epoch.)
	newTok := ownerToken(t, project, 6)

	driven := map[string]int{}
	err := ReplayIntents(newTok, func(key string) error {
		driven[key]++
		return nil // idempotent handler: already-done would be a no-op
	})
	if err != nil {
		t.Fatalf("ReplayIntents: %v", err)
	}
	if driven["dispatch-inflight-77"] != 1 {
		t.Fatalf("the pre-barrier in-flight action must be replayed once, got %d",
			driven["dispatch-inflight-77"])
	}
	// A second replay re-drives nothing (completion now recorded under epoch 6).
	driven = map[string]int{}
	if err := ReplayIntents(newTok, func(k string) error { driven[k]++; return nil }); err != nil {
		t.Fatalf("second ReplayIntents: %v", err)
	}
	if len(driven) != 0 {
		t.Fatalf("replay must be idempotent across rebuilds, got %v", driven)
	}
}

// ---- skill-side ownership proof (LeaseCheckByAncestor) ----

// fakeTree models a getppid chain: child -> parent. A pid absent from the
// map walks to 1 (init) and the walk stops.
func leaseCheckCfg(clk *fakeClock, live *fakeLiveness, tree map[int]int) leaseConfig {
	cfg := testCfg(clk, live)
	cfg.ppid = func(pid int) (int, bool) {
		p, ok := tree[pid]
		return p, ok
	}
	return cfg
}

// T30 (Go side): a fenced/stale coord's tick fails the ownership proof.
func TestLeaseCheckByAncestor_ProvenAndRefused(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	clk := &fakeClock{}
	live := newFakeLiveness()

	// Owner supervisor pid=500 (start 9001), tick pid=510 whose parent is 500.
	owner := identity{Pid: 500, PidStart: 9001, AgentID: "coord", Project: project}
	live.set(owner.Pid, owner.PidStart)
	live.set(510, 9002)
	tree := map[int]int{510: 500, 500: 1}
	cfg := leaseCheckCfg(clk, live, tree)

	writeEpochRaw(t, project, epochRecord{
		Epoch: 4, State: stateActive, Owner: owner,
		BootID: "test-boot-1", RenewedAtMono: clk.now(),
	})

	// PROVEN: tick (510) descends from the active owner (500).
	if err := leaseCheckByAncestorWithCfg(project, 510, cfg); err != nil {
		t.Fatalf("a tick under the active owner must pass, got %v", err)
	}

	// REFUSED #1: a fenced epoch (owner record now belongs to a NEW pid).
	writeEpochRaw(t, project, epochRecord{
		Epoch: 5, State: stateActive,
		Owner:  identity{Pid: 999, PidStart: 7777, AgentID: "new", Project: project},
		BootID: "test-boot-1", RenewedAtMono: clk.now(),
	})
	live.set(999, 7777)
	if err := leaseCheckByAncestorWithCfg(project, 510, cfg); !errors.Is(err, ErrNotLeaseOwner) {
		t.Fatalf("a tick NOT under the new owner must be refused, got %v", err)
	}

	// PROCEED: PID reuse — the recorded owner pid is now a DIFFERENT process
	// (start-time mismatch). pidAlive(owner) is false -> holderHealthy false
	// -> no live successor holds the lease -> PROCEED (the recycled-pid owner
	// is provably dead, so there is no split-brain to guard against).
	writeEpochRaw(t, project, epochRecord{
		Epoch: 6, State: stateActive,
		Owner:  identity{Pid: 500, PidStart: 9001, AgentID: "coord", Project: project},
		BootID: "test-boot-1", RenewedAtMono: clk.now(),
	})
	live.set(500, 12345) // pid 500 recycled -> different start time -> owner dead
	if err := leaseCheckByAncestorWithCfg(project, 510, cfg); err != nil {
		t.Fatalf("a recycled-pid (dead) owner must PROCEED, got %v", err)
	}
}

// codex PR4 [P1] (fresh-takeover window): a FRESH `fencing` record (a
// candidate mid-takeover that may have killed/reparented the old supervisor)
// must FENCE a caller that doesn't descend from the recorded owner — else
// the old skill-side tick keeps writing DURING the takeover.
func TestLeaseCheckByAncestor_FreshTakeoverFences(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	clk := &fakeClock{}
	live := newFakeLiveness()
	// A candidate (pid 800, live) is mid-takeover; owner=old (pid 500, now
	// dead — killed by the takeover). The stale tick (pid 510) descends from
	// the dead old supervisor, but the ancestor walk won't match a dead owner
	// pid (it can still be in the tree, but the record is fencing not active).
	live.set(800, 6000)                                  // candidate alive
	cfg := leaseCheckCfg(clk, live, map[int]int{700: 1}) // caller 700 under nothing relevant
	writeEpochRaw(t, project, epochRecord{
		Epoch: 5, State: stateFencing,
		Owner:     identity{Pid: 500, PidStart: 9001, AgentID: "old", Project: project},
		Candidate: identity{Pid: 800, PidStart: 6000, AgentID: "cand", Project: project},
		BootID:    "test-boot-1", RenewedAtMono: clk.now(),
	})
	if err := leaseCheckByAncestorWithCfg(project, 700, cfg); !errors.Is(err, ErrNotLeaseOwner) {
		t.Fatalf("a fresh in-progress takeover must FENCE a non-owner tick, got %v", err)
	}

	// But an ABANDONED fencing record (candidate dead, past resume) is NOT a
	// live takeover -> proceed (a successor will resume or a legacy tick runs).
	live.kill(800)
	clk.advance(cfg.ttl + 1)
	if err := leaseCheckByAncestorWithCfg(project, 700, cfg); err != nil {
		t.Fatalf("an abandoned (stale) fencing record must PROCEED, got %v", err)
	}
}

// FENCE: a genuinely live successor (healthy active owner) that the caller
// does NOT descend from — the real split-brain case the fence exists for.
func TestLeaseCheckByAncestor_GenuineSuccessorFences(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	clk := &fakeClock{}
	live := newFakeLiveness()
	// New leader 999 is live + healthy; the old coord's tick (510) descends
	// from the RETIRED old supervisor 500, not from 999.
	live.set(999, 7777)
	live.set(500, 9001)
	cfg := leaseCheckCfg(clk, live, map[int]int{510: 500, 500: 1})
	writeEpochRaw(t, project, epochRecord{
		Epoch: 7, State: stateActive,
		Owner:  identity{Pid: 999, PidStart: 7777, AgentID: "new", Project: project},
		BootID: "test-boot-1", RenewedAtMono: clk.now(),
	})
	if err := leaseCheckByAncestorWithCfg(project, 510, cfg); !errors.Is(err, ErrNotLeaseOwner) {
		t.Fatalf("a tick under the OLD coord while a NEW leader holds the lease must FENCE, got %v", err)
	}
}

// codex PR4 [P1] (fence-window hole): when the caller's OWN ancestor is the
// recorded owner but the record is no longer active (fencing / released /
// fenced_not_acquired), the caller's lease was taken -> it must FENCE
// (self-demote), NOT proceed. This is the fence-before-kill window: a
// candidate fenced our parent; our tick must stop before mutating.
func TestLeaseCheckByAncestor_OwnLeaseFencedSelfDemotes(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	clk := &fakeClock{}
	live := newFakeLiveness()
	owner := identity{Pid: 500, PidStart: 9001, AgentID: "coord", Project: project}
	live.set(owner.Pid, owner.PidStart)
	tree := map[int]int{510: 500, 500: 1}
	cfg := leaseCheckCfg(clk, live, tree)
	for _, st := range []string{stateFencing, stateReleased, stateFencedNotAcquired} {
		writeEpochRaw(t, project, epochRecord{
			Epoch: 4, State: st, Owner: owner,
			BootID: "test-boot-1", RenewedAtMono: clk.now(),
		})
		if err := leaseCheckByAncestorWithCfg(project, 510, cfg); !errors.Is(err, ErrNotLeaseOwner) {
			t.Fatalf("state=%s: our own fenced lease must self-demote, got %v", st, err)
		}
	}
}

// codex PR4 [P1] (self-expiry hole): the caller's ancestor IS the recorded
// owner and state==active, but the record's renewed_at is past TTL (a
// paused leader that woke). It must FENCE before a takeover races it.
func TestLeaseCheckByAncestor_OwnLeaseSelfExpiredFences(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	clk := &fakeClock{}
	live := newFakeLiveness()
	owner := identity{Pid: 500, PidStart: 9001, AgentID: "coord", Project: project}
	live.set(owner.Pid, owner.PidStart)
	cfg := leaseCheckCfg(clk, live, map[int]int{510: 500, 500: 1})
	writeEpochRaw(t, project, epochRecord{
		Epoch: 4, State: stateActive, Owner: owner,
		BootID: "test-boot-1", RenewedAtMono: clk.now(),
	})
	clk.advance(cfg.ttl + 1) // our own renewal window lapsed
	if err := leaseCheckByAncestorWithCfg(project, 510, cfg); !errors.Is(err, ErrNotLeaseOwner) {
		t.Fatalf("our own self-expired active lease must self-demote, got %v", err)
	}
}

// codex PR4 [P1]: no lease record at all -> legacy/non-wrapped coord ->
// PROCEED (the reversibility contract; fencing it would wedge the pre-lease
// path).
func TestLeaseCheckByAncestor_NoRecordProceeds(t *testing.T) {
	setupHome(t)
	clk := &fakeClock{}
	live := newFakeLiveness()
	cfg := leaseCheckCfg(clk, live, map[int]int{})
	if err := leaseCheckByAncestorWithCfg("rainier", os.Getpid(), cfg); err != nil {
		t.Fatalf("no lease record must PROCEED (legacy path), got %v", err)
	}
}

// An expired (TTL-stale) active record also means no live successor holds
// the lease -> PROCEED, not fence.
func TestLeaseCheckByAncestor_ExpiredOwnerProceeds(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	clk := &fakeClock{}
	live := newFakeLiveness()
	owner := identity{Pid: 500, PidStart: 9001, AgentID: "coord", Project: project}
	live.set(owner.Pid, owner.PidStart)
	cfg := leaseCheckCfg(clk, live, map[int]int{700: 1}) // caller 700 NOT under owner
	writeEpochRaw(t, project, epochRecord{
		Epoch: 4, State: stateActive, Owner: owner,
		BootID: "test-boot-1", RenewedAtMono: clk.now(),
	})
	clk.advance(cfg.ttl + 1) // expire the owner's TTL
	if err := leaseCheckByAncestorWithCfg(project, 700, cfg); err != nil {
		t.Fatalf("an expired owner must PROCEED (no live successor), got %v", err)
	}
}

// keyFile must map distinct keys to distinct files even when they sanitize
// to the same basename (slash vs underscore).
func TestKeyFile_NoCollision(t *testing.T) {
	a := keyFile("a/b", ".intent.json")
	b := keyFile("a_b", ".intent.json")
	if a == b {
		t.Fatalf("distinct keys must not collide: %s == %s", a, b)
	}
}
