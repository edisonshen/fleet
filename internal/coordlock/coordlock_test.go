// coordlock_test.go pins the v0.12.2 P0 atomic coord-spawn gate at the
// helper layer (DESIGN-coord-spawn-atomic-gate.md, operator G2
// 2026-05-19; v3 Change 7 moved the helper out of cmd/fleet into this
// shared package so both the dispatch CLI and the handoffop drain path
// contend on the SAME project lock).
//
// T6 (the runDispatch integration concurrency test) stays in
// cmd/fleet/dispatch_coord_spawn_lock_test.go because it drives the
// runDispatch entry point.
//
// Invariants pinned here:
//
//	T1 — TestAcquire_SecondConcurrentFails: while the first acquire is
//	     held, the second returns a clean "another coord-spawn is in
//	     progress" error rather than blocking or succeeding.
//
//	T2 — TestAcquire_LockReleasedOnReturn: after the first release()
//	     runs, a fresh acquire on the same project succeeds. Verifies
//	     defer release() actually unlocks.
//
//	T3 — TestAcquire_PerProjectLockIsolation: holding the lock for
//	     project A does NOT block acquiring it for project B
//	     (per-project lock file, not a global mutex).
//
//	T4 — TestAcquire_EmptyProjectRejected: Acquire("") returns a clear
//	     error rather than falling back to the state-package "_default"
//	     dir.
//
//	T5 — TestAcquire_ContentionErrorIncludesHolder: the lock file body
//	     is written with the holder PID at acquire time; a contending
//	     caller surfaces it in the error message so the operator can
//	     identify the in-flight dispatcher.
package coordlock

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edisonshen/fleet/internal/state"
)

// TestAcquire_SecondConcurrentFails is T1: the load-bearing negative
// assertion. A second acquire on the same project while the first is
// held MUST fail fast with a clean operator-facing error.
func TestAcquire_SecondConcurrentFails(t *testing.T) {
	fleetHome := t.TempDir()
	t.Setenv("FLEET_HOME", fleetHome)
	const project = "test-spawn-race-t1"

	if _, err := state.EnsureProjectInitialized(project); err != nil {
		t.Fatalf("EnsureProjectInitialized: %v", err)
	}

	release1, err := Acquire(project)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	t.Cleanup(func() {
		if release1 != nil {
			release1()
		}
	})

	release2, err := Acquire(project)
	if err == nil {
		if release2 != nil {
			release2()
		}
		t.Fatal("second concurrent acquire SUCCEEDED — atomic gate not closed")
	}
	if !strings.Contains(err.Error(), "another coord-spawn is in progress") {
		t.Fatalf("expected contention error mentioning 'another coord-spawn is in progress'; got: %v", err)
	}
	if !strings.Contains(err.Error(), project) {
		t.Fatalf("expected error to mention project %q; got: %v", project, err)
	}
}

// TestAcquire_LockReleasedOnReturn is T2: release() actually unlocks.
// After the first acquire's release, a second acquire succeeds.
func TestAcquire_LockReleasedOnReturn(t *testing.T) {
	fleetHome := t.TempDir()
	t.Setenv("FLEET_HOME", fleetHome)
	const project = "test-spawn-release-t2"
	if _, err := state.EnsureProjectInitialized(project); err != nil {
		t.Fatalf("EnsureProjectInitialized: %v", err)
	}

	release, err := Acquire(project)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	release()

	release2, err := Acquire(project)
	if err != nil {
		t.Fatalf("second acquire after release: %v", err)
	}
	t.Cleanup(func() { release2() })
}

// TestAcquire_PerProjectLockIsolation is T3: different projects don't
// block each other.
func TestAcquire_PerProjectLockIsolation(t *testing.T) {
	fleetHome := t.TempDir()
	t.Setenv("FLEET_HOME", fleetHome)
	const projectA = "test-iso-a"
	const projectB = "test-iso-b"
	if _, err := state.EnsureProjectInitialized(projectA); err != nil {
		t.Fatalf("EnsureProjectInitialized A: %v", err)
	}
	if _, err := state.EnsureProjectInitialized(projectB); err != nil {
		t.Fatalf("EnsureProjectInitialized B: %v", err)
	}

	releaseA, err := Acquire(projectA)
	if err != nil {
		t.Fatalf("acquire A: %v", err)
	}
	t.Cleanup(func() { releaseA() })

	releaseB, err := Acquire(projectB)
	if err != nil {
		t.Fatalf("acquire B while A held — projects must be independent; got: %v", err)
	}
	t.Cleanup(func() { releaseB() })
}

// TestAcquire_EmptyProjectRejected is T4: empty project is rejected at
// helper level. Both call sites (cmd/fleet/dispatch.go and
// internal/handoffop/handoffop.go) gate on non-empty project before
// calling Acquire, but the helper's defensive check keeps the contract
// clear.
func TestAcquire_EmptyProjectRejected(t *testing.T) {
	fleetHome := t.TempDir()
	t.Setenv("FLEET_HOME", fleetHome)
	_, err := Acquire("")
	if err == nil {
		t.Fatal("Acquire(\"\") should reject empty project")
	}
}

// TestAcquire_ContentionErrorIncludesHolder is T5: the contention error
// includes the holder's PID so the operator can identify the in-flight
// dispatcher.
func TestAcquire_ContentionErrorIncludesHolder(t *testing.T) {
	fleetHome := t.TempDir()
	t.Setenv("FLEET_HOME", fleetHome)
	const project = "test-contend-pid-t5"
	if _, err := state.EnsureProjectInitialized(project); err != nil {
		t.Fatalf("EnsureProjectInitialized: %v", err)
	}

	release1, err := Acquire(project)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	t.Cleanup(func() { release1() })

	pdir, err := state.ProjectDir(project)
	if err != nil {
		t.Fatalf("ProjectDir: %v", err)
	}
	lockPath := filepath.Join(filepath.Clean(pdir), ".locks", "coord-spawn.lock")
	body, rerr := os.ReadFile(lockPath)
	if rerr != nil {
		t.Fatalf("read lock body: %v", rerr)
	}
	pidNeedle := "pid="
	if !strings.Contains(string(body), pidNeedle) {
		t.Fatalf("lock body should contain pid=<n>; got %q", body)
	}

	_, err = Acquire(project)
	if err == nil {
		t.Fatal("second acquire succeeded — atomic gate not closed")
	}
	if !strings.Contains(err.Error(), "lock holder") && !strings.Contains(err.Error(), pidNeedle) {
		t.Fatalf("expected contention error to mention holder/pid; got: %v", err)
	}
}
