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
func TestMain(m *testing.M) {
	_ = testutil.Sweep(time.Hour)
	code := m.Run()
	_ = testutil.Sweep(time.Hour)
	os.Exit(code)
}
