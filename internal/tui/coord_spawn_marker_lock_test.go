// Tests for writeCoordSpawnMarkerLocked — the production marker-write
// path that wraps state.WriteCoordSpawnMarker in coordlock.Acquire so
// the TUI doesn't race coord.Cleanup's compare-and-remove.
//
// Codex iter-3 [P1]: without holding the same coord-spawn lock that
// dispatch / handoffop / coord.Cleanup take, the TUI's post-spawn
// marker write could interleave INSIDE cleanup's lock-held section
// (because cleanup acquired the lock, but the TUI didn't), and
// cleanup's subsequent unlink would delete the just-written marker.
// This file pins the locked-write contract.
package tui

import (
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/coordlock"
	"github.com/edisonshen/fleet/internal/state"
)

// TestWriteCoordSpawnMarkerLocked_WritesAndReleases is the happy-path
// pin: write succeeds, marker is on disk with the expected body, and
// the lock is released (a subsequent acquire from the same process
// succeeds immediately, proving no held flock).
func TestWriteCoordSpawnMarkerLocked_WritesAndReleases(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	const (
		project = "lock-write-test"
		agentID = "wmlk1234"
	)
	if _, err := state.EnsureProjectInitialized(project); err != nil {
		t.Fatalf("ensure project: %v", err)
	}

	if err := writeCoordSpawnMarkerLocked(project, agentID); err != nil {
		t.Fatalf("writeCoordSpawnMarkerLocked: %v", err)
	}

	body := state.ReadCoordSpawnMarker(project)
	if body != agentID {
		t.Errorf("marker body = %q, want %q", body, agentID)
	}

	// Lock must be released — a follow-up acquire should succeed
	// immediately (non-blocking flock). If the previous call leaked
	// the lock, this would return EWOULDBLOCK.
	release, err := coordlock.Acquire(project)
	if err != nil {
		t.Fatalf("post-write Acquire failed (lock leaked?): %v", err)
	}
	release()
}

// TestWriteCoordSpawnMarkerLocked_BlocksConcurrentCleanup is the
// race-closing pin: while writeCoordSpawnMarkerLocked is running, a
// concurrent coord.Cleanup-style coordlock.Acquire call must FAIL
// (non-blocking) — proving the marker write holds the lock for the
// duration of the write. The race codex iter-3 [P1] flagged depended
// on TUI write NOT taking the lock; this test pins that it does.
func TestWriteCoordSpawnMarkerLocked_BlocksConcurrentCleanup(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	const (
		project = "lock-race-test"
		agentID = "race1234"
	)
	if _, err := state.EnsureProjectInitialized(project); err != nil {
		t.Fatalf("ensure project: %v", err)
	}

	// Hijack state.WriteCoordSpawnMarker via a temporary swap so we
	// can pause inside the lock-held section. We can't monkey-patch
	// state from here; instead, we hold the lock manually from a
	// goroutine that simulates "writeCoordSpawnMarkerLocked is in
	// flight" and assert a concurrent Acquire bounces.
	holdReady := make(chan struct{})
	holdDone := make(chan struct{})
	var contendErr atomic.Value
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		release, err := coordlock.Acquire(project)
		if err != nil {
			t.Errorf("holder Acquire: %v", err)
			close(holdReady)
			return
		}
		close(holdReady)
		<-holdDone
		release()
	}()

	select {
	case <-holdReady:
	case <-time.After(5 * time.Second):
		close(holdDone)
		wg.Wait()
		t.Fatal("holder goroutine did not acquire within 5s")
	}

	// Now writeCoordSpawnMarkerLocked must fail-fast (NB-flock
	// EWOULDBLOCK) because the holder goroutine has the lock.
	err := writeCoordSpawnMarkerLocked(project, agentID)
	if err == nil {
		close(holdDone)
		wg.Wait()
		t.Fatal("writeCoordSpawnMarkerLocked succeeded while lock was held — expected EWOULDBLOCK")
	}
	contendErr.Store(err.Error())

	close(holdDone)
	wg.Wait()

	got := contendErr.Load().(string)
	if !strings.Contains(got, "coord-spawn") && !strings.Contains(got, "in progress") {
		t.Errorf("contended error %q does not surface the lock-contention reason", got)
	}

	// Marker must NOT have been written (the write was blocked).
	if _, err := os.Stat(mustMarkerPath(t, project)); err == nil {
		t.Error("marker exists after blocked write — write should have been atomic-fail")
	}
}

func mustMarkerPath(t *testing.T, project string) string {
	t.Helper()
	p, err := state.CoordSpawnMarkerPath(project)
	if err != nil {
		t.Fatalf("CoordSpawnMarkerPath: %v", err)
	}
	return p
}
