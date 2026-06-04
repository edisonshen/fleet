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
// End-of-run uses testutil.SweepAll (force-mode, ignores freshness +
// socketLive guard) per DESIGN-lifecycle-leak-recurrence PR-A: once
// `go test` is exiting, a LIVE test socket is by definition an orphan.
func TestMain(m *testing.M) {
	_ = testutil.Sweep(time.Hour)
	code := m.Run()
	_ = testutil.SweepAll()
	os.Exit(code)
}
