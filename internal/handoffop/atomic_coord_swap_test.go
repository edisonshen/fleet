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
// (spawn succeeds, session always alive, handoff reserve succeeds, etc.).
//
// D3: the coord-spawn marker is gone. The helper no longer writes a marker at a
// synchronous commit point; instead it RESERVES a handoff on OLD's lease (naming
// NEW) before retiring OLD. reserveHandoffFn is the injected seam.
type fakeSwap struct {
	spawnErr       error
	spawnNewRec    *agent.Record // override the synthesized NEW record
	waitErr        error
	postReadyAlive bool  // step 3.b alive return
	postReadyErr   error // step 3.b err return
	reserveErr     error // step 4 ReserveHandoff error (best-effort; never rolls back)
	sendKeysErr    error
	killErr        error
	postKillAlive  bool  // step 5.c alive return
	postKillErr    error // step 5.c err return

	// Recording surfaces — tests assert against these.
	spawnCalls         int
	waitCalls          int
	postReadyProbeOf   []string
	reserveCalled      bool
	reservedSuccessor  string
	reservedProj       string
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
	origReserve := reserveHandoffFn
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
	reserveHandoffFn = func(project, successorID string, ttl time.Duration) (bool, error) {
		f.reserveCalled = true
		f.reservedProj = project
		f.reservedSuccessor = successorID
		if f.reserveErr != nil {
			return false, f.reserveErr
		}
		return true, nil
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
		reserveHandoffFn = origReserve
		tmuxSendKeysFn = origSendKeys
		tmuxKillFn = origKill
		sleepFn = origSleep
	}
}

// seedCoordSwap initializes FLEET_HOME + project dir + writes the OLD
// coord record. Returns a populated AtomicCoordSwapInputs (caller can mutate
// before passing to the helper) and a synthesized NEW record (returned via the
// fake spawn). D3: no marker is seeded — the lease is the identity, and
// AtomicCoordSwap has no marker precondition anymore.
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
	if !fake.reserveCalled || fake.reservedSuccessor != "newcoord" {
		t.Errorf("expected ReserveHandoff(newcoord) on the commit path; reserveCalled=%v successor=%q", fake.reserveCalled, fake.reservedSuccessor)
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

// TestAtomicCoordSwap_ReserveFailure_SoftGuard is the D3 regression guard for
// the new soft-reserve contract that REPLACED the old marker-write commit. The
// deleted marker write was HARD: a write failure rolled back NEW and aborted the
// swap (ErrConcurrentSwap / marker-write-failed). The lease reserve is SOFT — a
// best-effort record-only CAS whose failure MUST NOT roll back NEW or abort the
// retire: OLD is still retired + archived, the swap completes, and the failure
// only logs (the identity commit is the winner's async epoch bump, not this
// reserve). Without this test a future refactor could silently re-harden the
// reserve into an abort and reintroduce a stuck-handoff class.
func TestAtomicCoordSwap_ReserveFailure_SoftGuard(t *testing.T) {
	in, newRec := seedCoordSwap(t, "rainier", "oldcoord", "newcoord")
	fake := &fakeSwap{
		postReadyAlive: true,                               // NEW alive
		postKillAlive:  false,                              // OLD dead after kill
		reserveErr:     errors.New("epoch lock contended"), // reserve fails
	}
	restore := fake.install(t, newRec)
	defer restore()

	var stderr bytes.Buffer
	res, err := AtomicCoordSwap(in, &stderr)
	// Soft guard: reserve failure must NOT abort the swap.
	if err != nil {
		t.Fatalf("reserve failure must NOT abort the swap; got err=%v (stderr=%s)", err, stderr.String())
	}
	if !fake.reserveCalled {
		t.Errorf("ReserveHandoff should still be attempted on the live-OLD commit path")
	}
	// Retire proceeds despite the failed reserve — NEW is NOT rolled back, OLD is
	// killed + archived exactly as on the happy path.
	if !fake.killCalled {
		t.Errorf("Kill OLD must still run after a soft reserve failure")
	}
	if !res.OldArchived {
		t.Errorf("OldArchived = false; the swap must complete despite the reserve failure")
	}
	livePath, _ := state.AgentPath("oldcoord")
	if _, statErr := os.Stat(livePath); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("OLD live record still exists at %s; retire must proceed", livePath)
	}
	// NEW is NOT rolled back: every old rollback path returned an error, so
	// err==nil + OldArchived above already proves the swap kept NEW and retired
	// OLD despite the reserve failure.
	// Surface-don't-silo: the failure is logged, not swallowed.
	if !strings.Contains(stderr.String(), "reserve handoff") {
		t.Errorf("expected a stderr warning about the failed reserve; got %q", stderr.String())
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
	// oldIsDead=true: the handoff reservation is SKIPPED (no live OLD lease to
	// reserve against) — NEW is spawned and OLD is preserved for operator [x].
	if fake.reserveCalled {
		t.Errorf("ReserveHandoff must be SKIPPED when oldIsDead=true; reserved %q", fake.reservedSuccessor)
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
	origReserve := reserveHandoffFn
	origSendKeys := tmuxSendKeysFn
	origKill := tmuxKillFn
	origSleep := sleepFn
	t.Cleanup(func() {
		spawnFn = origSpawn
		waitForReadyToPromptFn = origWait
		sessionAliveFn = origAlive
		reserveHandoffFn = origReserve
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
	reserveHandoffFn = func(project, successorID string, ttl time.Duration) (bool, error) { return true, nil }
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
	// D3: the second swap no longer aborts on a marker precondition, so it now
	// runs the full retire (including in.OldRec.ArchiveWithHandoff). Give it a
	// DISTINCT OldRec pointer — the real production shape is two independent
	// swap invocations, each with its own record — so the two goroutines don't
	// race on the shared *agent.Record.
	oldCopy := *in.OldRec
	in2.OldRec = &oldCopy
	// Second swap uses a different NEW ID.
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
	// D3: the marker precondition is gone, so the second swap no longer aborts
	// with a concurrent-swap error — the load-bearing guarantee this test pins is
	// SERIALIZATION: the two swaps never overlapped inside spawnFn (proven below),
	// and the second blocked on the flock until the first released (proven by the
	// secondDone select above). We deliberately do NOT assert on secondErr's value
	// (the second swap runs against an already-retired OLD, a benign edge).
	_ = secondErr
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
	if fake.reserveCalled {
		t.Errorf("swap aborted pre-reserve; ReserveHandoff must NOT be called (reserved %q)", fake.reservedSuccessor)
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
	if !fake.reserveCalled || fake.reservedSuccessor != "newcoord" {
		t.Errorf("expected ReserveHandoff(newcoord) on the commit path; reserveCalled=%v successor=%q", fake.reserveCalled, fake.reservedSuccessor)
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
	if fake.reserveCalled {
		t.Errorf("swap aborted pre-reserve; ReserveHandoff must NOT be called (reserved %q)", fake.reservedSuccessor)
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
	if fake.reserveCalled {
		t.Errorf("swap aborted pre-reserve; ReserveHandoff must NOT be called (reserved %q)", fake.reservedSuccessor)
	}
	// NEW record MUST be preserved (operator inspection).
	newPath, _ := state.AgentPath("newcoord")
	if _, statErr := os.Stat(newPath); statErr != nil {
		t.Errorf("NEW record should be preserved on ambiguous probe; statErr=%v", statErr)
	}
}

// TestAtomicCoordSwap_OldKill_Fails_StillAlive — FAILURE MODE 5.
// Marker committed; OLD's tmux session refuses to die. Helper PRESERVES
// OLD's record (codex iter-7 [P1] — was previously archived; reverted
// to preserve so cross-socket case keeps operator-visible signal),
// drops incident alert, logs [P0], returns ErrOrphanSurvived.
// Invariant still holds (marker at NEW; OLD is now a live tmux session
// that operator must clean up manually).
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
	if orphan.OldAgentID != "oldcoord" {
		t.Errorf("orphan.OldAgentID = %q; want oldcoord", orphan.OldAgentID)
	}
	if orphan.NewAgentID != "newcoord" {
		t.Errorf("orphan.NewAgentID = %q; want newcoord", orphan.NewAgentID)
	}
	// Marker MUST be at NEW (committed in step 4).
	if !fake.reserveCalled || fake.reservedSuccessor != "newcoord" {
		t.Errorf("expected ReserveHandoff(newcoord) on the commit path; reserveCalled=%v successor=%q", fake.reserveCalled, fake.reservedSuccessor)
	}
	// OLD record MUST be PRESERVED on disk (codex iter-7 [P1]).
	// Operator can see the live OLD via `fleet status` and decide
	// whether the cross-socket case obtains.
	livePath, _ := state.AgentPath("oldcoord")
	if _, statErr := os.Stat(livePath); statErr != nil {
		t.Errorf("OLD live record should be PRESERVED for operator triage; stat err=%v", statErr)
	}
	archivePath, _ := state.AgentArchivePath("oldcoord")
	if _, statErr := os.Stat(archivePath); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("OLD record should NOT be archived (codex iter-7); statErr=%v", statErr)
	}
	// Incident alert must exist (codex iter-6 [P2] — moved off the
	// agent inbox channel to operator-facing incidents/ dir).
	root, _ := state.Root()
	incidentsDir := filepath.Join(root, "incidents")
	entries, _ := os.ReadDir(incidentsDir)
	hasIncident := false
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "coord-swap-") {
			hasIncident = true
			break
		}
	}
	if !hasIncident {
		t.Errorf("incidents/coord-swap-* alert missing under %s; entries=%v", incidentsDir, entries)
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

// TestAtomicCoordSwap_OldKill_ProbeAmbiguous covers the codex iter-4 [P1]
// fix: post-kill probe returns (alive=false, err!=nil). The marker is
// committed; we MUST NOT archive OLD (cross-socket case could hide a
// live coord). Helper returns *ErrOldKillProbeAmbiguous; OLD record
// stays on disk; inbox alert dropped.
func TestAtomicCoordSwap_OldKill_ProbeAmbiguous(t *testing.T) {
	in, newRec := seedCoordSwap(t, "rainier", "oldcoord", "newcoord")
	fake := &fakeSwap{
		postReadyAlive: true,
		killErr:        nil,
		postKillAlive:  false,
		postKillErr:    errors.New("simulated transport blip on post-kill probe"),
	}
	restore := fake.install(t, newRec)
	defer restore()

	var stderr bytes.Buffer
	_, err := AtomicCoordSwap(in, &stderr)
	if err == nil {
		t.Fatalf("expected ErrOldKillProbeAmbiguous")
	}
	if !errors.Is(err, ErrOldKillProbeAmbiguousSentinel) {
		t.Errorf("expected ErrOldKillProbeAmbiguous; got: %v", err)
	}
	// Marker committed.
	if !fake.reserveCalled || fake.reservedSuccessor != "newcoord" {
		t.Errorf("expected ReserveHandoff(newcoord) on the commit path; reserveCalled=%v successor=%q", fake.reserveCalled, fake.reservedSuccessor)
	}
	// OLD record MUST stay on disk (operator triage).
	livePath, _ := state.AgentPath("oldcoord")
	if _, statErr := os.Stat(livePath); statErr != nil {
		t.Errorf("OLD record should be preserved on ambiguous probe; statErr=%v", statErr)
	}
	// OLD record NOT archived.
	archivePath, _ := state.AgentArchivePath("oldcoord")
	if _, statErr := os.Stat(archivePath); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("OLD record should NOT be archived on ambiguous probe; statErr=%v", statErr)
	}
	// Incident alert dropped (codex iter-6 [P2] — operator-facing
	// incidents dir, not the agent inbox channel).
	root, _ := state.Root()
	incidentsDir := filepath.Join(root, "incidents")
	entries, _ := os.ReadDir(incidentsDir)
	hasIncident := false
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "coord-swap-") {
			hasIncident = true
			break
		}
	}
	if !hasIncident {
		t.Errorf("incidents/coord-swap-* alert missing under %s; entries=%v", incidentsDir, entries)
	}
	// [P1] log to stderr.
	if !strings.Contains(stderr.String(), "[P1]") {
		t.Errorf("stderr should contain [P1] log; got: %s", stderr.String())
	}
}

// TestAtomicCoordSwap_OldKill_Fails_ButGone: step 5.b returns err but
// step 5.c reports session is gone (race with operator manual kill).
// Marker committed; OLD archived; success.
// TestAtomicCoordSwap_OldKill_Fails_PostProbeNotOnSocket pins codex
// iter-16 [P1]: `tmux.SessionAlive(oldSession) == (false, nil)` only
// proves OLD is gone on the CURRENT tmux socket. If killErr != nil
// (we couldn't confirm a kill ran), OLD may be alive on a different
// socket (cross-socket coord). Pre-iter-16 the helper would archive
// OLD here and the project could end up with the marker at NEW (which
// loses the lock race to the hidden OLD) plus OLD archived — no
// visible live coord. Post-fix: treat as ErrOldKillProbeAmbiguous,
// preserve OLD record.
func TestAtomicCoordSwap_OldKill_Fails_PostProbeNotOnSocket(t *testing.T) {
	in, newRec := seedCoordSwap(t, "rainier", "oldcoord", "newcoord")
	fake := &fakeSwap{
		postReadyAlive: true,
		killErr:        errors.New("simulated kill race"),
		postKillAlive:  false, // not on our socket after kill error
	}
	restore := fake.install(t, newRec)
	defer restore()

	var stderr bytes.Buffer
	res, err := AtomicCoordSwap(in, &stderr)
	if err == nil {
		t.Fatalf("expected ErrOldKillProbeAmbiguous on kill-err + not-on-socket; got nil")
	}
	if !errors.Is(err, ErrOldKillProbeAmbiguousSentinel) {
		t.Errorf("expected ErrOldKillProbeAmbiguous; got: %v", err)
	}
	// Marker still committed in step 4 — that's the load-bearing
	// invariant of the helper.
	if !fake.reserveCalled || fake.reservedSuccessor != "newcoord" {
		t.Errorf("expected ReserveHandoff(newcoord) on the commit path; reserveCalled=%v successor=%q", fake.reserveCalled, fake.reservedSuccessor)
	}
	// OLD record MUST be preserved (operator triage).
	if res.OldArchived {
		t.Errorf("OldArchived = true; want false (ambiguous cross-socket: preserve OLD for operator triage)")
	}
	if !strings.Contains(stderr.String(), "cross-socket") {
		t.Errorf("stderr should mention cross-socket hazard; got: %s", stderr.String())
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
	if !fake.reserveCalled || fake.reservedSuccessor != "newcoord" {
		t.Errorf("expected ReserveHandoff(newcoord) on the commit path; reserveCalled=%v successor=%q", fake.reserveCalled, fake.reservedSuccessor)
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
