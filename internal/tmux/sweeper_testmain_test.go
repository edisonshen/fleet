package tmux

import (
	"os"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/testutil"
)

// TestMain reaps stale /tmp/fleet-test-*.sock debris at suite start AND
// end (fleet#165 PR-B, per feedback_fleet_owns_its_resources.md). The
// per-test tmuxtest.RequireTmux cleanup is the first line of defense;
// this sweeper is the belt-and-suspenders layer for the rare panic
// path that bypasses t.Cleanup.
//
// End-of-run uses testutil.Sweep (guarded: freshness window +
// socketLive probe). The force-mode testutil.SweepAll is deliberately
// NOT used here: `go test ./...` runs sibling test binaries in
// parallel (default -p=GOMAXPROCS), all sharing the
// /tmp/fleet-test-*.sock namespace via tmuxtest.isolatedSocketPath.
// SweepAll on teardown would force-kill sockets owned by sibling
// packages whose tests are still running, causing a P0 CI regression
// (worker branch full-suite parallel run reproduced this:
// internal/handoffop's SweepAll killed internal/spawn's live tmux
// session mid-test). SweepAll/SweepAllDir remain in sweeper.go for
// PR-D's operator-invoked `fleet gc --force-test-sockets` path.
func TestMain(m *testing.M) {
	_ = testutil.Sweep(time.Hour)
	code := m.Run()
	_ = testutil.Sweep(time.Hour)
	os.Exit(code)
}
