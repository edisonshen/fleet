package gc

// coord_locks.go — fifth classifier for `fleet gc`: scans
// `~/.fleet/projects/<project>/.locks/coordinator.lock` files across
// every project and flags three failure modes that the per-project
// coord lifecycle did NOT clean up:
//
//   1. Mismatch — lock body's agent record reports a DIFFERENT project
//      than the lock's parent dir. PRs #173/#174 (fleet#170/#171) now
//      prevent NEW cross-project hijacks at spawn + tick time; this
//      classifier sweeps historical state from BEFORE that fix landed.
//
//   2. Dead coord — lock body references an agent record file under
//      ~/.fleet/agents/<id>.json that's MISSING. The coord crashed or
//      was force-archived without releasing the lock body.
//
//   3. Stale tmux — agent record exists but the tmux session named on
//      the record is GONE. The agent JSON outlived its tmux session
//      and the lock body hasn't been cleared.
//
// Default behavior: surface (Verb=surface). `--apply` upgrades to
// would-remove / removed.
//
// TDD-red phase: types + ListCoordLocks dep wired, but
// reconcileCoordLocks intentionally returns no actions so the new
// unit tests in gc_test.go fail on disk first. TDD-green restores
// the real classifier body.

import (
	"github.com/edisonshen/fleet/internal/agent"
)

// KindCoordLocks is the fifth classifier — coordinator.lock sweep.
const KindCoordLocks Kind = "coord-locks"

// CoordLockInfo is the minimal per-lock-file shape the classifier
// needs. Path is the absolute path on disk; Project is the lock's
// PARENT-DIR project name (the authoritative "what project does this
// lock belong to?" answer); HolderID is the parsed 8-hex agent ID
// from the lock body's first line (empty when missing/torn/garbage).
type CoordLockInfo struct {
	Path     string
	Project  string
	HolderID string
}

// reconcileCoordLocks — TDD-red stub. Real impl arrives in tdd-green.
func reconcileCoordLocks(r *Report, opts Options, deps Deps) error {
	_ = r
	_ = opts
	_ = deps
	return nil
}

// listCoordLocksOnDisk — TDD-red stub for production wiring. Real impl
// arrives in tdd-green.
func listCoordLocksOnDisk() ([]CoordLockInfo, error) {
	return nil, nil
}

// loadAgentRecordStub keeps the agent import referenced in tdd-red so
// the package compiles. Removed in tdd-green when the real classifier
// uses agent.Load directly.
var _ = agent.Load

// removeCoordLockFile — TDD-red stub.
func removeCoordLockFile(path string) error {
	_ = path
	return nil
}
