package tmuxtest_test

import (
	"os"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/testutil"
)

// TestMain gives the tmuxtest package the SAME suite-start/end sweep the
// real-tmux packages already carry (internal/tmux, internal/spawn,
// internal/dispatch, internal/handoffop, cmd/fleet). Until this landed,
// tmuxtest's own tests exercise RequireTmux / IsolateSocket against REAL
// tmux servers but had NO TestMain teardown — so on the Linux CI runner a
// per-test killServerAndRemove that lost the async-unlink race left a dead
// `srw-------` socket with nothing to reap it. That was a top source of the
// 230-socket leak that reddened the "Assert no /tmp/fleet-test-* leak" gate
// (ci-perf PR-2, 2026-06-14).
//
// Lives in an EXTERNAL test package (tmuxtest_test) so importing the parent
// internal/testutil for the sweeper helpers cannot form an import cycle with
// the tmuxtest package itself.
//
// The three concerns mirror the canonical shape (see
// internal/testutil/sweeper.go for the full rationale):
//
//   - IsolateSweepDir isolates the in-test gc reconcile PROBE to an empty
//     decoy via FLEET_GC_SCAN_DIR.
//   - Sweep at suite START reaps a leaked server from a prior crashed run
//     on real /tmp (guarded: freshness window + socketLive probe).
//   - ForceReapTestServers at suite END force-reaps THIS suite's leftover
//     servers — but ONLY when the host is go-test-quiescent, so a parallel
//     sibling package's live servers are spared.
func TestMain(m *testing.M) {
	cleanup := testutil.IsolateSweepDir()
	_ = testutil.Sweep(time.Hour)
	code := m.Run()
	_ = testutil.ForceReapTestServers()
	cleanup() // before os.Exit — os.Exit skips defers
	os.Exit(code)
}
