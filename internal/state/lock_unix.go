//go:build unix

package state

import (
	"fmt"
	"os"
	"syscall"
)

// LockProject takes an exclusive flock on
// ~/.fleet/projects/.locks/<project>.lock and returns a release
// function. Concurrent calls for the same project block; different
// projects proceed in parallel.
//
// Used by the handoff/spawn drain loop so two consumers (e.g., the
// CLI inline drain and a future TUI background drain) cannot race
// on the same project's queue. The .lock file is created if absent
// and never removed — a tiny zero-byte sentinel, the lock state
// lives in the kernel.
//
// Pattern:
//
//	release, err := state.LockProject("rainier")
//	if err != nil { return err }
//	defer release()
//	// ... drain queue, spawn replacement, etc ...
//
// Bootstrap creates ~/.fleet/projects/.locks/, so this never has to
// MkdirAll. If FLEET_HOME points at a fresh directory and Bootstrap
// has not run yet, opening the file will fail — surfacing the bug
// where it can be fixed.
//
// flock(2) is advisory and process-scoped. Two goroutines in the
// same process do NOT serialize via this — they share the same fd.
// That matches what we want for v1: per-process, per-project.
func LockProject(project string) (func(), error) {
	path, err := ProjectLockPath(project)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("flock %s: %w", path, err)
	}
	return func() {
		// Closing the fd releases the flock. Errors here are best-effort
		// — if Close fails, the kernel still cleans up on process exit,
		// and the lock file itself persists (zero-byte sentinel).
		_ = f.Close()
	}, nil
}
