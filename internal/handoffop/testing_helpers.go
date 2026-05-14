package handoffop

// SetTmuxForCleanupForTest replaces the package-level kill + alive
// helpers KillAgentsForTask uses. Returns a restore function the
// caller defers / Cleanup-s. Intended for cross-package test seams
// (cmd/fleet/tasks_test.go's worker-cleanup gate integration tests);
// in-package tests use stubWorkerTmux in worker_cleanup_test.go.
//
// Exported because go's _test.go visibility doesn't cross package
// boundaries. Production binaries never call this.
//
//nolint:revive // exported helper for cross-package test setup
func SetTmuxForCleanupForTest(
	kill func(string) error,
	alive func(string) (bool, error),
) (restore func()) {
	origKill := tmuxKillForCleanup
	origAlive := tmuxSessionAliveForCleanup
	tmuxKillForCleanup = kill
	tmuxSessionAliveForCleanup = alive
	return func() {
		tmuxKillForCleanup = origKill
		tmuxSessionAliveForCleanup = origAlive
	}
}
