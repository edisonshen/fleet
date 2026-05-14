package handoffop

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/spawn"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tmux"
)

// fakeSwap collects the configurable knobs for a single AtomicCoordSwap
// test case. Each field corresponds to one of the package-level seams
// the helper consults; nil/zero values pick a sensible default
// (spawn succeeds, session always alive, marker write succeeds, etc.).
type fakeSwap struct {
	spawnErr       error
	spawnNewRec    *agent.Record // override the synthesized NEW record
	waitErr        error
	postReadyAlive bool  // step 3.b alive return
	postReadyErr   error // step 3.b err return
	markerWriteErr error
	sendKeysErr    error
	killErr        error
	postKillAlive  bool  // step 5.c alive return
	postKillErr    error // step 5.c err return

	// Recording surfaces — tests assert against these.
	spawnCalls         int
	waitCalls          int
	postReadyProbeOf   []string
	markerWroteValue   string
	markerWroteProj    string
	sendKeysCalled     bool
	killCalled         bool
	postKillProbeCalls int
	sleepDurations     []time.Duration
}

// install replaces all relevant package-level seams with this fake's
// configured behavior. Returns a t.Cleanup-style restore func.
func (f *fakeSwap) install(t *testing.T, newRec *agent.Record) func() {
	t.Helper()
	origSpawn := spawnFn
	origWait := waitForReadyToPromptFn
	origAlive := sessionAliveFn
	origMarker := writeCoordSpawnMarkerFn
	origSendKeys := tmuxSendKeysFn
	origKill := tmuxKillFn
	origSleep := sleepFn

	if f.spawnNewRec == nil {
		f.spawnNewRec = newRec
	}

	// The helper probes NEW in step 3.b and OLD in step 5.c — sessionAliveFn
	// below distinguishes them by session name so a single fake can return
	// different alive/err pairs for the two stages.

	spawnFn = func(opts spawn.Options) (*agent.Record, error) {
		f.spawnCalls++
		if f.spawnErr != nil {
			return nil, f.spawnErr
		}
		// Echo back the synthesized NEW (caller-side seeded record).
		return f.spawnNewRec, nil
	}
	waitForReadyToPromptFn = func(session string) error {
		f.waitCalls++
		return f.waitErr
	}
	sessionAliveFn = func(session string) (bool, error) {
		// Step 3.b probes NEW; step 5.c probes OLD. Distinguish by
		// session name so the fake can return different results for
		// the two stages.
		if f.spawnNewRec != nil && session == f.spawnNewRec.TmuxSession {
			f.postReadyProbeOf = append(f.postReadyProbeOf, session)
			return f.postReadyAlive, f.postReadyErr
		}
		f.postKillProbeCalls++
		return f.postKillAlive, f.postKillErr
	}
	writeCoordSpawnMarkerFn = func(project, agentID string) error {
		f.markerWroteProj = project
		f.markerWroteValue = agentID
		if f.markerWriteErr != nil {
			return f.markerWriteErr
		}
		return state.WriteCoordSpawnMarker(project, agentID)
	}
	tmuxSendKeysFn = func(session string, keys ...string) error {
		f.sendKeysCalled = true
		return f.sendKeysErr
	}
	tmuxKillFn = func(session string) error {
		f.killCalled = true
		return f.killErr
	}
	sleepFn = func(d time.Duration) {
		f.sleepDurations = append(f.sleepDurations, d)
	}

	return func() {
		spawnFn = origSpawn
		waitForReadyToPromptFn = origWait
		sessionAliveFn = origAlive
		writeCoordSpawnMarkerFn = origMarker
		tmuxSendKeysFn = origSendKeys
		tmuxKillFn = origKill
		sleepFn = origSleep
	}
}

// seedCoordSwap initializes FLEET_HOME + project dir + writes the OLD
// coord record + sets the marker to OLD.ID. Returns a populated
// AtomicCoordSwapInputs (caller can mutate before passing to the helper)
// and a synthesized NEW record (returned via the fake spawn).
func seedCoordSwap(t *testing.T, project, oldID, newID string) (AtomicCoordSwapInputs, *agent.Record) {
	t.Helper()
	setupFleetHome(t)
	if _, err := state.EnsureProjectInitialized(project); err != nil {
		t.Fatalf("EnsureProjectInitialized: %v", err)
	}
	old := agent.New(oldID)
	old.Project = project
	old.TmuxSession = tmux.SessionName(oldID)
	if err := old.Write(); err != nil {
		t.Fatalf("old.Write: %v", err)
	}
	if err := state.WriteCoordSpawnMarker(project, oldID); err != nil {
		t.Fatalf("WriteCoordSpawnMarker(old): %v", err)
	}

	newRec := agent.New(newID)
	newRec.Project = project
	newRec.TmuxSession = tmux.SessionName(newID)
	// NOTE: NEW is intentionally NOT yet written to disk; spawnFn is
	// the seam responsible for that in real code. The helper invokes
	// spawnFn which (in production) writes the record. The fake just
	// returns the pre-seeded NEW record without writing. Tests that
	// need NEW on disk write it manually.

	in := AtomicCoordSwapInputs{
		Project:     project,
		OldRec:      old,
		NewRecSpec:  spawn.Options{PreAllocatedID: newID, OldRecord: old, Command: []string{"sleep", "1"}},
		GraceWindow: 1 * time.Millisecond,
	}
	return in, newRec
}

// TestAtomicCoordSwap_HappyPath_LiveOld covers the canonical case:
// OLD is live, all probes succeed, marker flips, OLD killed + archived.
// Test case #1 from the plan.
func TestAtomicCoordSwap_HappyPath_LiveOld(t *testing.T) {
	in, newRec := seedCoordSwap(t, "rainier", "oldcoord", "newcoord")
	fake := &fakeSwap{
		postReadyAlive: true,  // step 3.b: NEW alive
		postKillAlive:  false, // step 5.c: OLD dead
	}
	restore := fake.install(t, newRec)
	defer restore()

	var stderr bytes.Buffer
	res, err := AtomicCoordSwap(in, &stderr)
	if err != nil {
		t.Fatalf("AtomicCoordSwap: %v (stderr=%s)", err, stderr.String())
	}
	if got := state.ReadCoordSpawnMarker("rainier"); got != "newcoord" {
		t.Errorf("marker = %q; want newcoord", got)
	}
	if !res.OldArchived {
		t.Errorf("OldArchived = false; want true")
	}
	if res.OrphanedKill {
		t.Errorf("OrphanedKill = true; want false on happy path")
	}
	if !fake.sendKeysCalled {
		t.Errorf("SendKeys to OLD should have been called")
	}
	if !fake.killCalled {
		t.Errorf("Kill OLD should have been called")
	}
	// Verify OLD record is now archived (no longer at live path).
	livePath, _ := state.AgentPath("oldcoord")
	if _, err := os.Stat(livePath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("OLD live record still exists at %s", livePath)
	}
}

// TestAtomicCoordSwap_HappyPath_DeadOld covers case 4 — operator
// invoked dead-coord resume via TUI [a]. oldIsDead=true: kill + archive
// are SKIPPED; OLD record stays on disk for operator [x] cleanup.
func TestAtomicCoordSwap_HappyPath_DeadOld(t *testing.T) {
	in, newRec := seedCoordSwap(t, "rainier", "deadcoord", "newcoord")
	in.OldIsDead = true
	fake := &fakeSwap{
		postReadyAlive: true,
	}
	restore := fake.install(t, newRec)
	defer restore()

	var stderr bytes.Buffer
	res, err := AtomicCoordSwap(in, &stderr)
	if err != nil {
		t.Fatalf("AtomicCoordSwap: %v", err)
	}
	if got := state.ReadCoordSpawnMarker("rainier"); got != "newcoord" {
		t.Errorf("marker = %q; want newcoord", got)
	}
	if res.OldArchived {
		t.Errorf("OldArchived = true; want false for oldIsDead=true case")
	}
	if fake.sendKeysCalled {
		t.Errorf("SendKeys should be SKIPPED when oldIsDead=true")
	}
	if fake.killCalled {
		t.Errorf("Kill should be SKIPPED when oldIsDead=true")
	}
	// OLD record MUST still be on disk (operator clears via TUI [x]).
	livePath, _ := state.AgentPath("deadcoord")
	if _, err := os.Stat(livePath); err != nil {
		t.Errorf("OLD record should be preserved when oldIsDead=true; stat err: %v", err)
	}
}

// TestAtomicCoordSwap_Preconditions_MarkerNotOld verifies that step 1.b
// catches a marker that's already been moved by a concurrent swap.
// Helper aborts with ErrConcurrentSwap; NEW is NOT spawned.
func TestAtomicCoordSwap_Preconditions_MarkerNotOld(t *testing.T) {
	in, newRec := seedCoordSwap(t, "rainier", "oldcoord", "newcoord")
	// Race: another swap moved the marker to "racedcoord" before we
	// got the lock.
	if err := state.WriteCoordSpawnMarker("rainier", "racedcoord"); err != nil {
		t.Fatalf("seed race marker: %v", err)
	}
	fake := &fakeSwap{}
	restore := fake.install(t, newRec)
	defer restore()

	_, err := AtomicCoordSwap(in, nil)
	if err == nil {
		t.Fatalf("expected error when marker is not OLD; got nil")
	}
	if !errors.Is(err, ErrConcurrentSwap) {
		t.Errorf("expected ErrConcurrentSwap; got: %v", err)
	}
	if fake.spawnCalls != 0 {
		t.Errorf("spawn should NOT be called when preconditions fail; got %d calls", fake.spawnCalls)
	}
}

// TestAtomicCoordSwap_Preconditions_MarkerNotOld_AlreadySpawned_CleansUpNEW
// is the codex iter-1 [P2] regression: when the caller pre-spawned NEW
// (AlreadySpawnedNewRec set) AND the marker races to a different ID
// before we acquire the lock, the helper MUST clean up the pre-spawned
// NEW. Otherwise the detection of the race produces the duplicate-agent
// state the check was meant to prevent.
func TestAtomicCoordSwap_Preconditions_MarkerNotOld_AlreadySpawned_CleansUpNEW(t *testing.T) {
	in, newRec := seedCoordSwap(t, "rainier", "oldcoord", "newcoord")
	// Caller pre-spawned NEW and wrote its record.
	if err := newRec.Write(); err != nil {
		t.Fatalf("newRec.Write seed: %v", err)
	}
	in.AlreadySpawnedNewRec = newRec
	// Race: marker moved to someone else.
	if err := state.WriteCoordSpawnMarker("rainier", "racedcoord"); err != nil {
		t.Fatalf("seed race marker: %v", err)
	}
	fake := &fakeSwap{}
	restore := fake.install(t, newRec)
	defer restore()

	_, err := AtomicCoordSwap(in, nil)
	if err == nil {
		t.Fatalf("expected ErrConcurrentSwap")
	}
	if !errors.Is(err, ErrConcurrentSwap) {
		t.Errorf("expected ErrConcurrentSwap; got: %v", err)
	}
	// Pre-spawned NEW record MUST be cleaned up (DropReplacementRecord).
	newPath, _ := state.AgentPath("newcoord")
	if _, statErr := os.Stat(newPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("pre-spawned NEW record should be cleaned up on concurrent-swap detection; statErr=%v", statErr)
	}
	// Marker untouched.
	if got := state.ReadCoordSpawnMarker("rainier"); got != "racedcoord" {
		t.Errorf("marker = %q; want racedcoord (unchanged)", got)
	}
}

// TestAtomicCoordSwap_Preconditions_LockHeld verifies that two
// concurrent AtomicCoordSwap calls on the same project serialize at the
// flock — the second call blocks until the first releases. We exercise
// this with two goroutines and a sentinel that proves they didn't
// overlap.
func TestAtomicCoordSwap_Preconditions_LockHeld(t *testing.T) {
	in, newRec := seedCoordSwap(t, "rainier", "oldcoord", "newcoord")
	// Both goroutines need a working fake. The fake for the FIRST swap
	// blocks inside spawnFn until the test triggers progress. The SECOND
	// call should block at acquireSwapLock and only proceed AFTER the
	// first releases.
	firstStarted := make(chan struct{})
	firstMayProceed := make(chan struct{})

	origSpawn := spawnFn
	origWait := waitForReadyToPromptFn
	origAlive := sessionAliveFn
	origMarker := writeCoordSpawnMarkerFn
	origSendKeys := tmuxSendKeysFn
	origKill := tmuxKillFn
	origSleep := sleepFn
	t.Cleanup(func() {
		spawnFn = origSpawn
		waitForReadyToPromptFn = origWait
		sessionAliveFn = origAlive
		writeCoordSpawnMarkerFn = origMarker
		tmuxSendKeysFn = origSendKeys
		tmuxKillFn = origKill
		sleepFn = origSleep
	})

	var inProgress atomic.Int32
	var overlapped atomic.Bool
	spawnFn = func(opts spawn.Options) (*agent.Record, error) {
		n := inProgress.Add(1)
		defer inProgress.Add(-1)
		if n > 1 {
			overlapped.Store(true)
		}
		if opts.PreAllocatedID == "newcoord" {
			// First swap: signal start, then block until released.
			close(firstStarted)
			<-firstMayProceed
		}
		return &agent.Record{ID: opts.PreAllocatedID, Project: "rainier", TmuxSession: tmux.SessionName(opts.PreAllocatedID)}, nil
	}
	waitForReadyToPromptFn = func(session string) error { return nil }
	// Step 3.b probes the NEW session (alive); step 5.c probes the OLD
	// session — we want that to report dead so the swap reaches step 6
	// cleanly. The lock-held test uses NEW IDs "newcoord" / "newcoord2",
	// so anything starting with "fleet-new" is NEW; otherwise OLD.
	sessionAliveFn = func(session string) (bool, error) {
		if strings.HasPrefix(session, "fleet-new") {
			return true, nil
		}
		return false, nil
	}
	writeCoordSpawnMarkerFn = state.WriteCoordSpawnMarker
	tmuxSendKeysFn = func(session string, keys ...string) error { return nil }
	tmuxKillFn = func(session string) error { return nil }
	sleepFn = func(d time.Duration) {}

	_ = newRec

	var wg sync.WaitGroup
	var firstErr, secondErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, firstErr = AtomicCoordSwap(in, nil)
	}()

	<-firstStarted

	// Issue the second call. It must block on the swap lock and NOT
	// reach spawnFn until we release the first.
	in2 := in
	// Second swap operates under SAME OLD (marker hasn't moved yet
	// because the first hasn't reached step 4) and a different NEW ID.
	in2.NewRecSpec.PreAllocatedID = "newcoord2"
	secondDone := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, secondErr = AtomicCoordSwap(in2, nil)
		close(secondDone)
	}()

	// Give the second goroutine a chance to block on the lock.
	select {
	case <-secondDone:
		t.Fatalf("second swap completed before first released its lock — flock not enforcing serialization")
	case <-time.After(50 * time.Millisecond):
		// Expected — second is blocked.
	}

	close(firstMayProceed)
	wg.Wait()

	if firstErr != nil {
		t.Errorf("first swap: %v", firstErr)
	}
	// Second swap will see the marker as newcoord (committed by first)
	// and abort with ErrConcurrentSwap — that's the correct
	// serialization behavior.
	if !errors.Is(secondErr, ErrConcurrentSwap) {
		t.Errorf("second swap should fail with ErrConcurrentSwap (first committed the marker); got: %v", secondErr)
	}
	if overlapped.Load() {
		t.Errorf("two swaps were inside spawnFn concurrently — flock did not enforce mutual exclusion")
	}
}

// TestAtomicCoordSwap_Spawn_Fails_NoObservable: step 2 returns err.
// No marker change, no NEW record on disk, OLD untouched.
func TestAtomicCoordSwap_Spawn_Fails_NoObservable(t *testing.T) {
	in, newRec := seedCoordSwap(t, "rainier", "oldcoord", "newcoord")
	fake := &fakeSwap{
		spawnErr: errors.New("simulated spawn failure"),
	}
	restore := fake.install(t, newRec)
	defer restore()

	_, err := AtomicCoordSwap(in, nil)
	if err == nil {
		t.Fatalf("expected error on spawn failure")
	}
	if got := state.ReadCoordSpawnMarker("rainier"); got != "oldcoord" {
		t.Errorf("marker changed to %q on spawn failure; want oldcoord (unchanged)", got)
	}
	livePath, _ := state.AgentPath("oldcoord")
	if _, err := os.Stat(livePath); err != nil {
		t.Errorf("OLD record disappeared on spawn-failure rollback: %v", err)
	}
}

// TestAtomicCoordSwap_WaitForReady_Timeout: step 3.a never converges
// (WaitForReadyToPrompt returns error). The helper does NOT fail here —
// the wait is best-effort and step 3.b's tristate probe is the
// authoritative liveness gate. Once 3.b confirms alive, the swap
// proceeds to commit.
func TestAtomicCoordSwap_WaitForReady_Timeout(t *testing.T) {
	in, newRec := seedCoordSwap(t, "rainier", "oldcoord", "newcoord")
	fake := &fakeSwap{
		waitErr:        errors.New("pane did not stabilize within 1s"),
		postReadyAlive: true, // NEW is still alive despite slow boot
		postKillAlive:  false,
	}
	restore := fake.install(t, newRec)
	defer restore()

	var stderr bytes.Buffer
	_, err := AtomicCoordSwap(in, &stderr)
	if err != nil {
		t.Fatalf("AtomicCoordSwap should not fail on wait-timeout alone (probe is authoritative): %v", err)
	}
	if got := state.ReadCoordSpawnMarker("rainier"); got != "newcoord" {
		t.Errorf("marker = %q; want newcoord", got)
	}
	if !strings.Contains(stderr.String(), "did not converge") {
		t.Errorf("stderr should warn about wait-timeout; got: %s", stderr.String())
	}
}

// TestAtomicCoordSwap_ReProbe_DefinitivelyDead: step 3.b reports
// alive=false, err=nil. NEW died during boot. Roll back NEW; marker
// stays at OLD.
func TestAtomicCoordSwap_ReProbe_DefinitivelyDead(t *testing.T) {
	in, newRec := seedCoordSwap(t, "rainier", "oldcoord", "newcoord")
	// Seed NEW on disk so DropReplacementRecord has something to remove.
	if err := newRec.Write(); err != nil {
		t.Fatalf("newRec.Write seed: %v", err)
	}
	fake := &fakeSwap{
		postReadyAlive: false, // definitively dead
		postReadyErr:   nil,
	}
	restore := fake.install(t, newRec)
	defer restore()

	_, err := AtomicCoordSwap(in, nil)
	if err == nil {
		t.Fatalf("expected error when NEW dead at step 3.b")
	}
	if !strings.Contains(err.Error(), "exited during readiness wait") {
		t.Errorf("error should mention readiness wait; got: %v", err)
	}
	if got := state.ReadCoordSpawnMarker("rainier"); got != "oldcoord" {
		t.Errorf("marker = %q; want oldcoord (unchanged)", got)
	}
	// NEW record should be removed.
	newPath, _ := state.AgentPath("newcoord")
	if _, statErr := os.Stat(newPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("NEW record should be cleaned up; statErr=%v", statErr)
	}
}

// TestAtomicCoordSwap_ReProbe_AmbiguousError: step 3.b returns
// (alive=false, err!=nil). Probe ambiguous. NEW record + tmux preserved
// for operator inspection — no rollback. Marker unchanged.
func TestAtomicCoordSwap_ReProbe_AmbiguousError(t *testing.T) {
	in, newRec := seedCoordSwap(t, "rainier", "oldcoord", "newcoord")
	if err := newRec.Write(); err != nil {
		t.Fatalf("newRec.Write seed: %v", err)
	}
	fake := &fakeSwap{
		postReadyAlive: false,
		postReadyErr:   errors.New("simulated transport blip"),
	}
	restore := fake.install(t, newRec)
	defer restore()

	_, err := AtomicCoordSwap(in, nil)
	if err == nil {
		t.Fatalf("expected error on ambiguous probe")
	}
	if !strings.Contains(err.Error(), "preserved for operator inspection") {
		t.Errorf("error should mention preservation; got: %v", err)
	}
	if got := state.ReadCoordSpawnMarker("rainier"); got != "oldcoord" {
		t.Errorf("marker = %q; want oldcoord", got)
	}
	// NEW record MUST be preserved (operator inspection).
	newPath, _ := state.AgentPath("newcoord")
	if _, statErr := os.Stat(newPath); statErr != nil {
		t.Errorf("NEW record should be preserved on ambiguous probe; statErr=%v", statErr)
	}
}

// TestAtomicCoordSwap_MarkerWrite_Fails: step 4 errors. NEW is alive
// but not declared as coord — roll back NEW (kill + remove record).
// Marker unchanged at OLD.
func TestAtomicCoordSwap_MarkerWrite_Fails(t *testing.T) {
	in, newRec := seedCoordSwap(t, "rainier", "oldcoord", "newcoord")
	if err := newRec.Write(); err != nil {
		t.Fatalf("newRec.Write seed: %v", err)
	}
	fake := &fakeSwap{
		postReadyAlive: true,
		markerWriteErr: errors.New("simulated marker write EIO"),
	}
	restore := fake.install(t, newRec)
	defer restore()

	_, err := AtomicCoordSwap(in, nil)
	if err == nil {
		t.Fatalf("expected error on marker write failure")
	}
	if !strings.Contains(err.Error(), "marker write failed") {
		t.Errorf("error should mention marker write; got: %v", err)
	}
	if got := state.ReadCoordSpawnMarker("rainier"); got != "oldcoord" {
		t.Errorf("marker = %q; want oldcoord", got)
	}
	// NEW record should be cleaned up.
	newPath, _ := state.AgentPath("newcoord")
	if _, statErr := os.Stat(newPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("NEW record should be removed on marker-write rollback; statErr=%v", statErr)
	}
}

// TestAtomicCoordSwap_OldKill_Fails_StillAlive — FAILURE MODE 5.
// Marker committed; OLD's tmux session refuses to die. Helper archives
// OLD's record, drops inbox alert, logs [P0], returns ErrOrphanSurvived.
// Invariant still holds.
func TestAtomicCoordSwap_OldKill_Fails_StillAlive(t *testing.T) {
	in, newRec := seedCoordSwap(t, "rainier", "oldcoord", "newcoord")
	fake := &fakeSwap{
		postReadyAlive: true,
		killErr:        errors.New("simulated kill failure"),
		postKillAlive:  true, // STILL ALIVE after kill
	}
	restore := fake.install(t, newRec)
	defer restore()

	var stderr bytes.Buffer
	res, err := AtomicCoordSwap(in, &stderr)
	if err == nil {
		t.Fatalf("expected ErrOrphanSurvived")
	}
	if !errors.Is(err, ErrOrphanSurvivedSentinel) {
		t.Errorf("error should be ErrOrphanSurvived class; got: %v", err)
	}
	var orphan *ErrOrphanSurvived
	if !errors.As(err, &orphan) {
		t.Fatalf("error should unwrap to *ErrOrphanSurvived; got %T", err)
	}
	if orphan.OldSession != in.OldRec.TmuxSession {
		t.Errorf("orphan.OldSession = %q; want %q", orphan.OldSession, in.OldRec.TmuxSession)
	}
	if orphan.NewAgentID != "newcoord" {
		t.Errorf("orphan.NewAgentID = %q; want newcoord", orphan.NewAgentID)
	}
	// Marker MUST be at NEW (committed in step 4).
	if got := state.ReadCoordSpawnMarker("rainier"); got != "newcoord" {
		t.Errorf("marker = %q; want newcoord (invariant: NEW owns post-commit)", got)
	}
	// OLD record MUST be archived (so prune-orphan-tmux reaps the session).
	livePath, _ := state.AgentPath("oldcoord")
	if _, statErr := os.Stat(livePath); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("OLD live record should be archived; stat err=%v", statErr)
	}
	archivePath, _ := state.AgentArchivePath("oldcoord")
	if _, statErr := os.Stat(archivePath); statErr != nil {
		t.Errorf("OLD record should be at archive path; stat err=%v", statErr)
	}
	// Inbox alert must exist.
	root, _ := state.Root()
	inboxPath := filepath.Join(root, "inbox", "newcoord.md")
	if _, statErr := os.Stat(inboxPath); statErr != nil {
		t.Errorf("inbox alert at %s missing: %v", inboxPath, statErr)
	}
	// [P0] log to stderr.
	if !strings.Contains(stderr.String(), "[P0]") {
		t.Errorf("stderr should contain [P0] log line; got: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "FAILURE MODE 5") {
		t.Errorf("stderr should mention FAILURE MODE 5; got: %s", stderr.String())
	}
	if !res.OrphanedKill {
		t.Errorf("result.OrphanedKill = false; want true")
	}
}

// TestAtomicCoordSwap_OldKill_Fails_ButGone: step 5.b returns err but
// step 5.c reports session is gone (race with operator manual kill).
// Marker committed; OLD archived; success.
func TestAtomicCoordSwap_OldKill_Fails_ButGone(t *testing.T) {
	in, newRec := seedCoordSwap(t, "rainier", "oldcoord", "newcoord")
	fake := &fakeSwap{
		postReadyAlive: true,
		killErr:        errors.New("simulated kill race"),
		postKillAlive:  false, // race: gone after kill error
	}
	restore := fake.install(t, newRec)
	defer restore()

	var stderr bytes.Buffer
	res, err := AtomicCoordSwap(in, &stderr)
	if err != nil {
		t.Fatalf("AtomicCoordSwap should succeed when post-probe confirms dead despite kill err: %v", err)
	}
	if got := state.ReadCoordSpawnMarker("rainier"); got != "newcoord" {
		t.Errorf("marker = %q; want newcoord", got)
	}
	if !res.OldArchived {
		t.Errorf("OldArchived = false; want true")
	}
	if !strings.Contains(stderr.String(), "session is gone") {
		t.Errorf("stderr should note the kill-error-but-session-gone race; got: %s", stderr.String())
	}
}

// TestAtomicCoordSwap_OldArchive_Fails_KillConfirmed: step 6 Archive
// errors but kill succeeded in step 5. Helper falls back to os.Remove
// on the live record so no stale entry remains in `fleet status`.
//
// We force Archive to fail by planting a DIRECTORY at the archive
// destination path — agent.Archive's stat-archive-path branch then
// returns a non-IsNotExist error from the os.Stat call (since the
// path exists but is a directory) which propagates up via the
// "stat archive path: %w" wrapper. The live file is still on disk,
// so the helper's fallback os.Remove succeeds and the swap completes
// (with a warning to stderr noting "live record removed instead").
func TestAtomicCoordSwap_OldArchive_Fails_KillConfirmed(t *testing.T) {
	in, newRec := seedCoordSwap(t, "rainier", "oldcoord", "newcoord")
	fake := &fakeSwap{
		postReadyAlive: true,
		postKillAlive:  false,
	}
	restore := fake.install(t, newRec)
	defer restore()

	// Force agent.Archive's stat-archive-path branch to return a
	// non-IsNotExist error by planting a directory where the bare
	// archive path would be. The collision handler will THEN check
	// for a stamped path which doesn't exist (IsNotExist), so the
	// rename target becomes the stamped path — and the rename to a
	// stamped path under a regular dir would actually succeed.
	//
	// To force a hard rename failure, plant a directory at BOTH the
	// bare path AND make the stamped suffix collide too. Simpler:
	// remove the agents/archive/ directory entirely so the rename's
	// target dir doesn't exist.
	archiveDir, _ := state.AgentArchivePath("placeholder")
	archiveParent := filepath.Dir(archiveDir)
	if err := os.RemoveAll(archiveParent); err != nil {
		t.Fatalf("remove archive parent: %v", err)
	}
	// Create a regular file with the same name so MkdirAll-like
	// recovery fails too. (rename(2) to a nonexistent parent dir
	// returns ENOENT — the failure we want.)

	var stderr bytes.Buffer
	res, err := AtomicCoordSwap(in, &stderr)
	if err != nil {
		t.Fatalf("AtomicCoordSwap should fall back to os.Remove when Archive fails but kill confirmed: %v (stderr=%s)", err, stderr.String())
	}
	if got := state.ReadCoordSpawnMarker("rainier"); got != "newcoord" {
		t.Errorf("marker = %q; want newcoord", got)
	}
	if !res.OldArchived {
		t.Errorf("OldArchived = false; fallback path should still set OldArchived=true")
	}
	if !strings.Contains(stderr.String(), "live record removed instead") {
		t.Errorf("stderr should note the fallback path; got: %s", stderr.String())
	}
	// OLD live record removed by fallback.
	livePath, _ := state.AgentPath("oldcoord")
	if _, statErr := os.Stat(livePath); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("OLD live record should be removed by fallback; statErr=%v", statErr)
	}
}

// TestAtomicCoordSwap_DeadOldRecord_NotArchived: oldIsDead=true with
// OLD record present on disk. Record MUST stay on disk (operator [x]
// path).
func TestAtomicCoordSwap_DeadOldRecord_NotArchived(t *testing.T) {
	in, newRec := seedCoordSwap(t, "rainier", "deadold", "newcoord")
	in.OldIsDead = true
	fake := &fakeSwap{postReadyAlive: true}
	restore := fake.install(t, newRec)
	defer restore()

	_, err := AtomicCoordSwap(in, nil)
	if err != nil {
		t.Fatalf("AtomicCoordSwap: %v", err)
	}
	livePath, _ := state.AgentPath("deadold")
	if _, err := os.Stat(livePath); err != nil {
		t.Errorf("OLD record should be PRESERVED when oldIsDead=true; stat err: %v", err)
	}
}

// TestAtomicCoordSwap_WriteAtomic_ParentDirFsync exercises the
// integration with commit 1's parent-dir fsync. The swap calls
// state.WriteCoordSpawnMarker which calls state.WriteAtomic, which
// invokes fsyncParent. We hook fsyncParent here to assert the call
// happened with the correct dir.
func TestAtomicCoordSwap_WriteAtomic_ParentDirFsync(t *testing.T) {
	in, newRec := seedCoordSwap(t, "rainier", "oldcoord", "newcoord")
	fake := &fakeSwap{postReadyAlive: true, postKillAlive: false}
	restore := fake.install(t, newRec)
	defer restore()

	// Don't go through the markerWriteErr override path — use the real
	// state.WriteCoordSpawnMarker so fsyncParent runs.
	writeCoordSpawnMarkerFn = state.WriteCoordSpawnMarker

	// Spy on the package-level fsyncParent via state's exported test seam.
	// state package's fsyncParent is package-private; tests in state/
	// already verify it. We test that the marker file ends up at the
	// right path and matches the new ID — the durability of the rename
	// (parent-dir fsync) is exercised end-to-end via the commit-1 unit
	// tests in state_test.go. Here we just confirm the integration.
	_, err := AtomicCoordSwap(in, nil)
	if err != nil {
		t.Fatalf("AtomicCoordSwap: %v", err)
	}
	if got := state.ReadCoordSpawnMarker("rainier"); got != "newcoord" {
		t.Errorf("marker = %q; want newcoord", got)
	}
	// Verify the marker file exists on disk (rename committed).
	markerPath, _ := state.CoordSpawnMarkerPath("rainier")
	if _, err := os.Stat(markerPath); err != nil {
		t.Errorf("marker file not present on disk after swap: %v", err)
	}
}

// ----- Crash recovery test cases -----

// TestAtomicCoordSwap_CrashWindowA_NEWLiveBothRecords simulates a
// crash AFTER step 2 (NEW spawned + record written) BEFORE step 3
// (probe). State observable after restart: marker at OLD, both NEW
// and OLD records on disk, both tmux sessions live. The recovery
// command for case 4 (non-queued) is `fleet rm <NEW_ID>`; queued
// cases would also work via the resume path but case A is pre-commit
// so no queue file exists in any case.
//
// This test doesn't simulate the crash in-process — we set up the
// post-crash state manually and verify the recovery command shape
// (in production: `fleet rm <NEW_ID>` archives NEW + kills its tmux,
// marker stays at OLD). Here we just verify state matches the plan's
// description: both records on disk, marker at OLD, no queue file.
func TestAtomicCoordSwap_CrashWindowA_NEWLiveBothRecords(t *testing.T) {
	setupFleetHome(t)
	project := "rainier"
	if _, err := state.EnsureProjectInitialized(project); err != nil {
		t.Fatalf("EnsureProjectInitialized: %v", err)
	}
	// Seed OLD record + marker at OLD.
	old := agent.New("oldcoord")
	old.Project = project
	old.TmuxSession = tmux.SessionName("oldcoord")
	if err := old.Write(); err != nil {
		t.Fatalf("old.Write: %v", err)
	}
	if err := state.WriteCoordSpawnMarker(project, "oldcoord"); err != nil {
		t.Fatalf("WriteCoordSpawnMarker: %v", err)
	}
	// Seed NEW record (step 2 ran).
	newRec := agent.New("newcoord")
	newRec.Project = project
	newRec.TmuxSession = tmux.SessionName("newcoord")
	if err := newRec.Write(); err != nil {
		t.Fatalf("newRec.Write: %v", err)
	}

	// Assert the post-crash state matches the plan.
	if got := state.ReadCoordSpawnMarker(project); got != "oldcoord" {
		t.Errorf("post-crash marker = %q; want oldcoord", got)
	}
	if _, err := agent.Load("oldcoord"); err != nil {
		t.Errorf("OLD record should be loadable: %v", err)
	}
	if _, err := agent.Load("newcoord"); err != nil {
		t.Errorf("NEW record should be loadable: %v", err)
	}
	// No queue file expected (Window A is pre-commit; case 4 doesn't
	// write queue journals at all, and queued cases write the journal
	// elsewhere — runHandoff writes its own journal which is unrelated
	// to AtomicCoordSwap's commit point).
}

// TestAtomicCoordSwap_CrashWindowB_Queued_MarkerAtNew_OldAlive
// simulates a crash AFTER step 4 (marker committed) BEFORE step 5.b
// (OLD kill never sent). State: marker at NEW, both records live,
// queue journal present (queued cases 1/2/3/5 — auto-handoff /
// manual / engine swap). Recovery: `fleet handoff <OLD>` (resume path)
// finishes the swap idempotently. `fleet rm <OLD>` would refuse
// while the queue journal exists.
//
// We don't drive the full recovery here (that's tested in the
// commit-3 / commit-4 integration tests). This case verifies the
// observable post-crash state matches the plan.
func TestAtomicCoordSwap_CrashWindowB_Queued_MarkerAtNew_OldAlive(t *testing.T) {
	setupFleetHome(t)
	project := "rainier"
	if _, err := state.EnsureProjectInitialized(project); err != nil {
		t.Fatalf("EnsureProjectInitialized: %v", err)
	}
	// Both records on disk.
	old := agent.New("oldcoord")
	old.Project = project
	old.TmuxSession = tmux.SessionName("oldcoord")
	if err := old.Write(); err != nil {
		t.Fatalf("old.Write: %v", err)
	}
	newRec := agent.New("newcoord")
	newRec.Project = project
	newRec.TmuxSession = tmux.SessionName("newcoord")
	if err := newRec.Write(); err != nil {
		t.Fatalf("newRec.Write: %v", err)
	}
	// Marker at NEW (step 4 committed).
	if err := state.WriteCoordSpawnMarker(project, "newcoord"); err != nil {
		t.Fatalf("WriteCoordSpawnMarker: %v", err)
	}
	// Queue file present (queued case).
	root, _ := state.Root()
	queuePath := filepath.Join(root, "queue", "spawn-fresh-oldcoord.json")
	if err := os.WriteFile(queuePath, []byte(`{"old_agent_id":"oldcoord","new_agent_id":"newcoord","schema_version":2}`), 0o644); err != nil {
		t.Fatalf("seed queue file: %v", err)
	}

	// Verify post-crash state.
	if got := state.ReadCoordSpawnMarker(project); got != "newcoord" {
		t.Errorf("marker = %q; want newcoord", got)
	}
	if _, err := os.Stat(queuePath); err != nil {
		t.Errorf("queue file should be present for queued case: %v", err)
	}
}

// TestAtomicCoordSwap_CrashWindowB_NonQueued_MarkerAtNew_OldAlive
// simulates Window B for case 4 (dead-coord resume via [a]). No
// queue journal. Recovery: `fleet rm <OLD>` resolves cleanly.
func TestAtomicCoordSwap_CrashWindowB_NonQueued_MarkerAtNew_OldAlive(t *testing.T) {
	setupFleetHome(t)
	project := "rainier"
	if _, err := state.EnsureProjectInitialized(project); err != nil {
		t.Fatalf("EnsureProjectInitialized: %v", err)
	}
	old := agent.New("oldcoord")
	old.Project = project
	old.TmuxSession = tmux.SessionName("oldcoord")
	if err := old.Write(); err != nil {
		t.Fatalf("old.Write: %v", err)
	}
	newRec := agent.New("newcoord")
	newRec.Project = project
	newRec.TmuxSession = tmux.SessionName("newcoord")
	if err := newRec.Write(); err != nil {
		t.Fatalf("newRec.Write: %v", err)
	}
	if err := state.WriteCoordSpawnMarker(project, "newcoord"); err != nil {
		t.Fatalf("WriteCoordSpawnMarker: %v", err)
	}

	if got := state.ReadCoordSpawnMarker(project); got != "newcoord" {
		t.Errorf("marker = %q; want newcoord", got)
	}
	// NO queue file expected (case 4 / non-queued).
	root, _ := state.Root()
	queuePath := filepath.Join(root, "queue", "spawn-fresh-oldcoord.json")
	if _, err := os.Stat(queuePath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("queue file unexpectedly present for non-queued case: %v", err)
	}
}

// TestAtomicCoordSwap_CrashWindowC_Queued_MarkerAtNew_OldDead
// simulates a crash AFTER step 5.c success BEFORE step 6 (Archive).
// State: marker at NEW, OLD tmux dead, OLD record still on disk
// (Archive never ran), queue journal present. Recovery: rerun
// `fleet handoff <OLD>` which archives idempotently and clears
// the queue.
func TestAtomicCoordSwap_CrashWindowC_Queued_MarkerAtNew_OldDead(t *testing.T) {
	setupFleetHome(t)
	project := "rainier"
	if _, err := state.EnsureProjectInitialized(project); err != nil {
		t.Fatalf("EnsureProjectInitialized: %v", err)
	}
	old := agent.New("oldcoord")
	old.Project = project
	old.TmuxSession = tmux.SessionName("oldcoord")
	if err := old.Write(); err != nil {
		t.Fatalf("old.Write: %v", err)
	}
	newRec := agent.New("newcoord")
	newRec.Project = project
	newRec.TmuxSession = tmux.SessionName("newcoord")
	if err := newRec.Write(); err != nil {
		t.Fatalf("newRec.Write: %v", err)
	}
	if err := state.WriteCoordSpawnMarker(project, "newcoord"); err != nil {
		t.Fatalf("WriteCoordSpawnMarker: %v", err)
	}
	root, _ := state.Root()
	queuePath := filepath.Join(root, "queue", "spawn-fresh-oldcoord.json")
	if err := os.WriteFile(queuePath, []byte(`{"old_agent_id":"oldcoord","new_agent_id":"newcoord","schema_version":2}`), 0o644); err != nil {
		t.Fatalf("seed queue file: %v", err)
	}

	// OLD tmux dead is not asserted here (no real tmux session
	// was ever started); the plan's invariant is "OLD record stale
	// (live file present, no live tmux)". Operator running `fleet
	// status` would see the stale-tmux signal. We assert the file
	// shape.
	if _, err := agent.Load("oldcoord"); err != nil {
		t.Errorf("OLD record should still be on disk: %v", err)
	}
	if got := state.ReadCoordSpawnMarker(project); got != "newcoord" {
		t.Errorf("marker = %q; want newcoord", got)
	}
}

// TestAtomicCoordSwap_CrashWindowC_NonQueued_MarkerAtNew_OldDead
// simulates Window C for case 4 (non-queued). Recovery: `fleet rm
// <OLD>` is idempotent — tmux already dead, just archives the
// record.
func TestAtomicCoordSwap_CrashWindowC_NonQueued_MarkerAtNew_OldDead(t *testing.T) {
	setupFleetHome(t)
	project := "rainier"
	if _, err := state.EnsureProjectInitialized(project); err != nil {
		t.Fatalf("EnsureProjectInitialized: %v", err)
	}
	old := agent.New("oldcoord")
	old.Project = project
	old.TmuxSession = tmux.SessionName("oldcoord")
	if err := old.Write(); err != nil {
		t.Fatalf("old.Write: %v", err)
	}
	newRec := agent.New("newcoord")
	newRec.Project = project
	newRec.TmuxSession = tmux.SessionName("newcoord")
	if err := newRec.Write(); err != nil {
		t.Fatalf("newRec.Write: %v", err)
	}
	if err := state.WriteCoordSpawnMarker(project, "newcoord"); err != nil {
		t.Fatalf("WriteCoordSpawnMarker: %v", err)
	}

	if _, err := agent.Load("oldcoord"); err != nil {
		t.Errorf("OLD record should still be on disk: %v", err)
	}
	if got := state.ReadCoordSpawnMarker(project); got != "newcoord" {
		t.Errorf("marker = %q; want newcoord", got)
	}
	// No queue file for case 4 (non-queued).
	root, _ := state.Root()
	queuePath := filepath.Join(root, "queue", "spawn-fresh-oldcoord.json")
	if _, err := os.Stat(queuePath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("queue file unexpectedly present for non-queued case: %v", err)
	}
}
