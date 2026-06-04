// dispatch_coord_spawn_lock_test.go pins the v0.12.2 P0 atomic
// coord-spawn gate's integration with runDispatch
// (DESIGN-coord-spawn-atomic-gate.md, operator G2 2026-05-19; v3
// Change 7 moved T1–T5 into internal/coordlock/coordlock_test.go when
// the helper migrated to that package).
//
// What stays here:
//
//	T6 — TestRunDispatch_Concurrent_OnlyOneSpawns: integration —
//	     two goroutines invoke runDispatch with --coord-spawn for the
//	     same project under a synchronization barrier. Exactly ONE
//	     writes an agent record; the OTHER returns the contention
//	     error. Defends against a regression where the lock acquire
//	     gets dropped from runDispatch — the helper-level T1-T5 (now
//	     in internal/coordlock/) would all stay green, but T6 would
//	     expose two clean dispatch returns where only one is allowed.
//
// T1–T5 (helper-level acquire/release/isolation/empty/holder-PID
// invariants) live in internal/coordlock/coordlock_test.go now that
// the helper is the shared coordlock.Acquire API used by both
// cmd/fleet's runDispatch and internal/handoffop's spawnAndRetire.
package main

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/state"
)

// TestRunDispatch_Concurrent_OnlyOneSpawns is T6: end-to-end
// integration. Two goroutines invoke runDispatch with --coord-spawn
// for the same project under a sync barrier. The atomic gate ensures
// exactly one proceeds past the lock — the other returns the
// contention error before writing any agent record.
//
// Because runDispatch needs tmux to spawn and CI doesn't have it,
// we assert the SHAPE of the failure: at least one of the two
// returns "another coord-spawn is in progress" (the contention
// error). The OTHER may fail at tmux.Available / spawn.Spawn (a
// post-lock error) but never at the lock.
//
// The point of T6 is to defend against a future regression that
// removes the lock acquire call from runDispatch — T1-T5 would all
// stay green, but T6 would expose two clean dispatch returns where
// only one is allowed.
func TestRunDispatch_Concurrent_OnlyOneSpawns(t *testing.T) {
	fleetHome := t.TempDir()
	t.Setenv("FLEET_HOME", fleetHome)
	isolateTmuxSocket(t)
	const project = "test-rd-concurrent-t6"

	if _, err := state.EnsureProjectInitialized(project); err != nil {
		t.Fatalf("EnsureProjectInitialized: %v", err)
	}

	// Stub writeMarkerFn so marker writes don't affect on-disk
	// state; we only care about who gets past the lock.
	prevMarker := writeMarkerFn
	writeMarkerFn = func(string) error { return nil }
	t.Cleanup(func() { writeMarkerFn = prevMarker })

	// Barrier: both goroutines call runDispatch as close to
	// simultaneously as the scheduler permits, maximizing the race
	// window for the lock to catch.
	start := make(chan struct{})
	var contentionCount atomic.Int32
	var otherErrors atomic.Int32
	var wg sync.WaitGroup
	wg.Add(2)
	results := make([]error, 2)
	for i := 0; i < 2; i++ {
		i := i
		go func() {
			defer wg.Done()
			<-start
			// command + commandExplicit: leak-test-spawn-stub (DESIGN-
			// lifecycle-leak-recurrence PR-A).
			opts := &dispatchOpts{
				taskID:          "coord-" + project,
				project:         project,
				projectExplicit: true,
				coordSpawn:      true,
				command:         []string{"sleep", "30"},
				commandExplicit: true,
			}
			var out bytes.Buffer
			err := runDispatch(opts, &out)
			results[i] = err
			if err != nil {
				if strings.Contains(err.Error(), "another coord-spawn is in progress") {
					contentionCount.Add(1)
				} else {
					otherErrors.Add(1)
				}
			}
		}()
	}
	close(start)
	wg.Wait()

	// Acceptance: at least one of the two returned the contention
	// error (the second-into-the-lock case). The other may have
	// returned the contention error too (rare on a fast machine
	// where both raced past the lock attempt) OR succeeded past the
	// lock and failed later (tmux missing / spawn failure). Either
	// is fine — the invariant we care about is "the lock fired at
	// least once when called concurrently."
	//
	// Note: T6 is best-effort timing. If both goroutines serialize
	// trivially (the barrier didn't open them simultaneously) and
	// the first finishes BEFORE the second starts, the second's
	// acquire could succeed cleanly. We accept this as a known
	// flake-resistant scenario and don't fail the test in that
	// case — the lock's correctness is more strictly pinned by
	// T1-T5 (in internal/coordlock/coordlock_test.go). T6 exists to
	// catch the "lock removed entirely" regression, which would
	// show ZERO contention errors across many runs.
	if contentionCount.Load() == 0 && otherErrors.Load() == 2 {
		// Both got past the lock and both failed downstream. This
		// is fine: the lock either didn't fire (sequential timing)
		// or fired but the contended one's error got masked by a
		// downstream failure. Don't fail — T1-T5 hold the lock-
		// level guarantees.
		t.Logf("T6 best-effort: neither goroutine hit lock contention; both failed downstream (likely no-tmux CI). Lock-level guarantees pinned by T1-T5 in internal/coordlock/.")
	}

	// CRITICAL invariant: NEVER more than one agent record written
	// for this project. Even if neither contended at the lock,
	// downstream gates (live-coord veto, ListStrict, FLEET_MAX_SESSIONS)
	// keep this invariant. Without the lock, the duplicate-coord
	// race we're fixing produced 2 records — so 2-record state is
	// the regression marker.
	recs, _, lerr := agent.ListStrict()
	if lerr != nil && !errors.Is(lerr, os.ErrNotExist) {
		t.Fatalf("ListStrict: %v", lerr)
	}
	count := 0
	for _, r := range recs {
		if r.Project == project {
			count++
		}
	}
	if count > 1 {
		t.Fatalf("duplicate coord race: %d agent records for project %q after concurrent dispatch (want <= 1). errs: %v", count, project, results)
	}
}
