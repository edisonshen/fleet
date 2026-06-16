package tmuxfake_test

import (
	"os"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/testutil"
)

// TestMain gives the tmuxfake package the SAME suite-start/end sweep the
// real-tmux packages carry. Most tmuxfake tests run fully in-process
// against the fake backend, but parity_test.go deliberately stands up a
// REAL tmux server (via tmuxtest.RequireTmux) to assert fake/real parity —
// so this package leaks real sockets on the Linux CI runner exactly like
// the others did, yet it had NO TestMain teardown. That was a source of the
// 230 dead `srw-------` sockets that reddened the leak gate (ci-perf PR-2,
// 2026-06-14).
//
// External test package (tmuxfake_test) so importing the parent
// internal/testutil for the sweeper helpers cannot cycle with tmuxfake.
//
// See internal/testutil/sweeper.go for the full IsolateSweepDir / Sweep /
// ForceReapTestServers rationale and the host-quiescence gate that keeps
// the end-of-run force-reap from killing a parallel sibling's live servers.
func TestMain(m *testing.M) {
	cleanup := testutil.IsolateSweepDir()
	_ = testutil.Sweep(time.Hour)
	code := m.Run()
	_ = testutil.ForceReapTestServers()
	cleanup() // before os.Exit — os.Exit skips defers
	os.Exit(code)
}
