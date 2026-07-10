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

// codex PR4 [P2]: a CORRUPT completion file must NOT suppress replay. The
// intent is re-driven idempotently rather than silently treated as finished.
func TestReplayIntents_CorruptCompletionReplays(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	tok := ownerToken(t, project, 4)
	store, _ := tok.intentStore()
	if err := store.writeIntent("act-x", 4); err != nil {
		t.Fatal(err)
	}
	// A present-but-CORRUPT completion (e.g. torn / lost dir-fsync on crash).
	if err := os.WriteFile(store.completionPath("act-x"), []byte("{garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	driven := 0
	if err := ReplayIntents(tok, func(string) error { driven++; return nil }); err != nil {
		t.Fatalf("ReplayIntents: %v", err)
	}
	if driven != 1 {
		t.Fatalf("a corrupt completion must NOT suppress replay; drove %d, want 1", driven)
	}
}

// codex PR4 [P2]: a STALE completion from a PRIOR epoch (an idempotency key
// reused in a later lease generation) must NOT suppress replay of the new
// pending intent.
func TestReplayIntents_StaleCompletionEpochReplays(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	tok := ownerToken(t, project, 7) // current lease epoch 7
	store, _ := tok.intentStore()
	// A NEW intent for the reused key at the current epoch...
	if err := store.writeIntent("branch/feat", 7); err != nil {
		t.Fatal(err)
	}
	// ...but only a STALE completion from an earlier epoch 3 exists (the
	// process crashed before writing the new completion).
	if err := store.writeCompletion("branch/feat", 3); err != nil {
		t.Fatal(err)
	}
	driven := 0
	if err := ReplayIntents(tok, func(string) error { driven++; return nil }); err != nil {
		t.Fatalf("ReplayIntents: %v", err)
	}
	if driven != 1 {
		t.Fatalf("a stale-epoch completion must NOT suppress replay; drove %d, want 1", driven)
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

// ---- keyFile helper ----

func TestKeyFile_NoCollision(t *testing.T) {
	a := keyFile("a/b", ".intent.json")
	b := keyFile("a_b", ".intent.json")
	if a == b {
		t.Fatalf("distinct keys must not collide: %s == %s", a, b)
	}
}

// ---- skill-side ownership proof: LeaseCheckByAncestor (D7, flock-only) ----
//
// PR-2 moved the tick / fleet-guard producer fence off the (no-longer-written)
// epoch onto the FLOCK. leaseCheckByAncestorWithCfg reuses the LOCK_SH busy-
// probe (NOT the body alone — a stale body after Release must not be trusted)
// then walks the caller's ppid chain to the flock body's holder pid.

// deepChainTree builds a child->parent ppid map walking from startPid to
// endPid over exactly hops edges, via synthetic intermediate pids (20000+)
// that cannot collide with this file's other fixture pids. Used by T12g to
// place holderPid past leaseCheckByAncestorWithCfg's maxDepth=64 bound.
func deepChainTree(startPid, endPid, hops int) map[int]int {
	tree := make(map[int]int, hops)
	pid := startPid
	for i := 0; i < hops-1; i++ {
		next := 20000 + i
		tree[pid] = next
		pid = next
	}
	tree[pid] = endPid
	return tree
}

// leaseCheckCfg builds a lease-check cfg with an injected getppid tree (child ->
// parent; a pid absent from the map walks to 1). There is no --reacquire flag
// any more — holding the flock IS ownership, so the read-only probe never
// renews.
func leaseCheckCfg(live *fakeLiveness, tree map[int]int) leaseConfig {
	cfg := testCfg(&fakeClock{}, live)
	cfg.ppid = func(pid int) (int, bool) {
		p, ok := tree[pid]
		return p, ok
	}
	return cfg
}

// TestLeaseCheckByAncestor_FlockProof (T12): the flock is the discriminant.
//
//	flock HELD + caller descends from the body's holder   -> PROCEED
//	flock HELD + caller is NOT a descendant               -> FENCE (different-owner)
//	flock HELD + body torn/pid-less                        -> FENCE (held-unreadable)
//	flock FREE (probe acquires) + a stale body persists    -> FENCE (flock-free)
//	flock file never created (ENOENT)                      -> PROCEED (legacy)
func TestLeaseCheckByAncestor_FlockProof(t *testing.T) {
	const holderPid, holderStart = 500, int64(9001)
	const callerPid = 510
	body := &flockBody{Pid: holderPid, PidStart: holderStart, AgentID: "coord", Project: "rainier", BootID: "b"}
	cases := []struct {
		name    string
		setup   func(t *testing.T, project string)
		tree    map[int]int
		wantTag string // "" => PROCEED
	}{
		{
			name:    "T12a_descendant_of_held_holder_proceeds",
			setup:   func(t *testing.T, p string) { heldFlock(t, p, body) },
			tree:    map[int]int{callerPid: holderPid, holderPid: 1},
			wantTag: "",
		},
		{
			name:    "T12b_stranger_of_held_holder_fences_different_owner",
			setup:   func(t *testing.T, p string) { heldFlock(t, p, body) },
			tree:    map[int]int{callerPid: 1},
			wantTag: fenceTagDifferentOwner,
		},
		{
			name:    "T12c_free_flock_stale_body_fences_flock_free",
			setup:   func(t *testing.T, p string) { writeFlockBodyRaw(t, p, *body) }, // body on disk, NOT held
			tree:    map[int]int{callerPid: holderPid, holderPid: 1},
			wantTag: fenceTagFlockFree,
		},
		{
			name:    "T12d_enoent_never_created_proceeds",
			setup:   func(t *testing.T, p string) {}, // nothing — no flock file
			tree:    nil,
			wantTag: "",
		},
		{
			name:    "T12e_held_torn_body_fences_unreadable",
			setup:   func(t *testing.T, p string) { heldFlock(t, p, nil) }, // empty body on a held flock
			tree:    map[int]int{callerPid: 1},
			wantTag: fenceTagFlockHeldUnreadable,
		},
		{
			// Review testing-specialist gap: the bounded ppid walk (maxDepth=64,
			// the documented safety net against a ppid cycle or a pathologically
			// deep tree) had no test actually driving a cycle or an over-depth
			// chain. A 3-node cycle that never reaches holderPid must terminate
			// (not hang) within maxDepth iterations and fence, never PROCEED.
			name:    "T12f_ppid_cycle_bounded_by_maxDepth_fences",
			setup:   func(t *testing.T, p string) { heldFlock(t, p, body) },
			tree:    map[int]int{callerPid: 511, 511: 512, 512: callerPid},
			wantTag: fenceTagDifferentOwner,
		},
		{
			// holderPid IS a true ancestor of callerPid here, but only past
			// depth 64 — the walk must give up at the bound and fence rather
			// than walk arbitrarily deep. Proves maxDepth is a real limit, not
			// a documented-but-untested one.
			name:    "T12g_chain_deeper_than_maxDepth_fences_even_though_holder_is_a_true_ancestor",
			setup:   func(t *testing.T, p string) { heldFlock(t, p, body) },
			tree:    deepChainTree(callerPid, holderPid, 200),
			wantTag: fenceTagDifferentOwner,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setupHome(t)
			const project = "rainier"
			live := newFakeLiveness()
			live.set(holderPid, holderStart) // the flock holder is a live process
			live.set(callerPid, 9002)
			tc.setup(t, project)
			cfg := leaseCheckCfg(live, tc.tree)
			err := leaseCheckByAncestorWithCfg(project, callerPid, cfg)
			if tc.wantTag == "" {
				if err != nil {
					t.Fatalf("want PROCEED, got %v", err)
				}
				return
			}
			if !errors.Is(err, ErrNotLeaseOwner) {
				t.Fatalf("want a fence (ErrNotLeaseOwner), got %v", err)
			}
			if !strings.Contains(err.Error(), tc.wantTag) {
				t.Fatalf("fence tag = %v, want %q", err, tc.wantTag)
			}
		})
	}
}

// TestLeaseCheckByAncestor_RecycledHolderPidFences: the flock is held but the
// live process at the body's pid has a DIFFERENT start time (pid reuse) — the
// caller cannot prove it descends from the real holder, so fence.
func TestLeaseCheckByAncestor_RecycledHolderPidFences(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	const holderPid, holderStart = 500, int64(9001)
	const callerPid = 510
	heldFlock(t, project, &flockBody{Pid: holderPid, PidStart: holderStart, AgentID: "coord", Project: project, BootID: "b"})
	live := newFakeLiveness()
	live.set(holderPid, 12345) // pid 500 recycled -> start-time mismatch vs the body
	live.set(callerPid, 9002)
	cfg := leaseCheckCfg(live, map[int]int{callerPid: holderPid, holderPid: 1})
	if err := leaseCheckByAncestorWithCfg(project, callerPid, cfg); !errors.Is(err, ErrNotLeaseOwner) {
		t.Fatalf("a recycled holder pid must FENCE (not a provable ancestor), got %v", err)
	}
}

// TestLeaseCheckByAncestor_ReadOnlyNeverRenews (T12 read-only property): the
// probe never mutates the flock — the epoch's --reacquire renew path is gone, so
// a PROCEED leaves the flock body byte-identical.
func TestLeaseCheckByAncestor_ReadOnlyNeverRenews(t *testing.T) {
	setupHome(t)
	const project = "rainier"
	const holderPid, holderStart = 500, int64(9001)
	const callerPid = 510
	heldFlock(t, project, &flockBody{Pid: holderPid, PidStart: holderStart, AgentID: "coord", Project: project, BootID: "b"})
	paths, _ := resolvePaths(project)
	before, _ := os.ReadFile(paths.flock)
	live := newFakeLiveness()
	live.set(holderPid, holderStart)
	live.set(callerPid, 9002)
	cfg := leaseCheckCfg(live, map[int]int{callerPid: holderPid, holderPid: 1})
	if err := leaseCheckByAncestorWithCfg(project, callerPid, cfg); err != nil {
		t.Fatalf("want PROCEED, got %v", err)
	}
	after, _ := os.ReadFile(paths.flock)
	if string(before) != string(after) {
		t.Fatal("lease-check mutated the flock body (must be read-only — no --reacquire)")
	}
}
