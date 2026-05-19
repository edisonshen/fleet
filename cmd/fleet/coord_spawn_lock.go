package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/edisonshen/fleet/internal/state"
)

// acquireCoordSpawnLock takes an NB-flock on
// ~/.fleet/projects/<project>/.locks/coord-spawn.lock so the
// (live-coord veto + agent-record-write + spawn) tuple inside
// runDispatch for that project is serialized across concurrent
// `fleet dispatch --coord-spawn` invocations. Closes the v0.12.2 P0
// TOCTOU race that produced duplicate coords for projects-spark on
// 2026-05-19 (agents 72ea51b4 + 04f00601): both dispatchers read the
// pre-spawn agent-records list, neither saw the other's not-yet-
// written record, and both spawned.
//
// Naming: "coord-spawn.lock" — distinct from:
//   - the skill-owned "coordinator.lock" (post-spawn supervisor tick
//     lock; serializes ticks of an ALREADY-spawned coord)
//   - the state-package "state.lock" (per-project tasks.md/learnings.md
//     write serialization)
//
// Different concerns, sibling files under the same .locks/ dir.
//
// Mode: LOCK_EX | LOCK_NB. The first dispatcher to acquire holds it
// across the entire critical section; a second dispatcher fails
// immediately with a clean operator-facing error rather than blocking.
// The fail-fast contract matches existing internal/rc.withLock + the
// skill's coordinator.lock — operator double-presses surface as a
// visible "in progress" message, not a silent wait that confuses the
// mental model.
//
// On contention the error includes the holder's PID (read best-effort
// from the lock-file body) so the operator can identify the in-flight
// dispatcher.
//
// Release MUST be called on function return — both success and error
// paths. Callers `defer release()` immediately after acquire.
func acquireCoordSpawnLock(project string) (release func(), err error) {
	if project == "" {
		return nil, fmt.Errorf("acquireCoordSpawnLock: project must not be empty")
	}
	pdir, err := state.ProjectDir(project)
	if err != nil {
		return nil, fmt.Errorf("acquireCoordSpawnLock: project dir for %q: %w", project, err)
	}
	pdir = filepath.Clean(pdir)
	lockDir := filepath.Join(pdir, ".locks")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		return nil, fmt.Errorf("acquireCoordSpawnLock: mkdir %q: %w", lockDir, err)
	}
	lockPath := filepath.Join(lockDir, "coord-spawn.lock")
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("acquireCoordSpawnLock: open %q: %w", lockPath, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			// Read current holder's body for the operator-facing
			// error. Best-effort — a torn read between the first
			// dispatcher's Truncate and Write is harmless: we just
			// omit the holder field. The lock-file body is a
			// debug hint, not a load-bearing signal.
			holder := ""
			if b, rerr := os.ReadFile(lockPath); rerr == nil && len(b) > 0 {
				holder = string(b)
			}
			holderClause := ""
			if holder != "" {
				holderClause = fmt.Sprintf(" (lock holder: %s)", holder)
			}
			return nil, fmt.Errorf(
				"refusing to spawn coord for project %q: another coord-spawn is in progress%s. "+
					"attach to the in-flight coord via TUI [a] once it finishes booting, "+
					"or wait for the lock to release and retry",
				project, holderClause)
		}
		return nil, fmt.Errorf("acquireCoordSpawnLock: flock %q: %w", lockPath, err)
	}
	// Write a best-effort holder body so a contending dispatcher's
	// error message can surface this PID. The body isn't load-bearing
	// — flock alone enforces mutual exclusion. We intentionally don't
	// fail acquire on a body-write error.
	_ = f.Truncate(0)
	if _, werr := f.Seek(0, 0); werr == nil {
		_, _ = fmt.Fprintf(f, "pid=%d\n", os.Getpid())
	}

	release = func() {
		// Order matters: unlock BEFORE close so a concurrent dispatcher
		// waiting on OpenFile can immediately acquire after we release.
		// On Linux/macOS Close also releases the flock, but explicit
		// unlock is the documented contract.
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}
	return release, nil
}
