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
	"errors"
	"strings"
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

// TestWriteCoordSpawnMarkerLocked_RetriesAndSucceedsAfterContention
// pins the codex iter-6 [P1] fix: the marker write retries on
// EWOULDBLOCK rather than fail-fast. We stub coordlockAcquireFn to
// return contention for the first N attempts then succeed, and
// assert the marker is ultimately written and the lock-acquire is
// invoked > 1 time.
func TestWriteCoordSpawnMarkerLocked_RetriesAndSucceedsAfterContention(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	const (
		project = "retry-success-test"
		agentID = "rtry1234"
	)
	if _, err := state.EnsureProjectInitialized(project); err != nil {
		t.Fatalf("ensure project: %v", err)
	}

	prev := coordlockAcquireFn
	t.Cleanup(func() { coordlockAcquireFn = prev })

	var attempts int32
	const contendFor = 3 // first 3 attempts contend; 4th succeeds.
	coordlockAcquireFn = func(p string) (func(), error) {
		n := atomic.AddInt32(&attempts, 1)
		if n <= contendFor {
			return nil, errors.New("simulated coord-spawn lock contention")
		}
		return func() {}, nil
	}

	if err := writeCoordSpawnMarkerLocked(project, agentID); err != nil {
		t.Fatalf("writeCoordSpawnMarkerLocked: %v", err)
	}
	body := state.ReadCoordSpawnMarker(project)
	if body != agentID {
		t.Errorf("marker body = %q, want %q (written after retry)", body, agentID)
	}
	if got := atomic.LoadInt32(&attempts); got <= 1 {
		t.Errorf("expected > 1 Acquire attempt (retry path), got %d", got)
	}
}

// TestWriteCoordSpawnMarkerLocked_FailsAfterRetryBudget pins the
// budget bound: persistent contention eventually surfaces an error
// rather than hanging forever. The stub returns contention every
// time; we assert the function returns within reasonable bounds and
// carries the operator-facing message.
func TestWriteCoordSpawnMarkerLocked_FailsAfterRetryBudget(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	const (
		project = "retry-fail-test"
		agentID = "rfl12345"
	)
	if _, err := state.EnsureProjectInitialized(project); err != nil {
		t.Fatalf("ensure project: %v", err)
	}

	prev := coordlockAcquireFn
	t.Cleanup(func() { coordlockAcquireFn = prev })
	coordlockAcquireFn = func(string) (func(), error) {
		return nil, errors.New("permanently contended")
	}

	start := time.Now()
	err := writeCoordSpawnMarkerLocked(project, agentID)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("writeCoordSpawnMarkerLocked: expected error after budget exhausted, got nil")
	}
	if !strings.Contains(err.Error(), "contended") {
		t.Errorf("error %q does not surface contention reason", err)
	}
	// Budget is 2s; allow a generous upper bound on slow CI.
	if elapsed > 5*time.Second {
		t.Errorf("retry took %v, expected ~2s", elapsed)
	}
	if elapsed < 1*time.Second {
		t.Errorf("retry took only %v; expected at least ~1s of retries", elapsed)
	}
}
