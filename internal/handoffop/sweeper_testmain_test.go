package handoffop

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
// ci-perf-pr1 socket-leak P0 (2026-06-13) — the two concerns are DECOUPLED:
//
//   - IsolateSweepDir isolates ONLY the in-test gc reconcile PROBE to an
//     empty decoy via FLEET_GC_SCAN_DIR — the `tmux -S <sock> ls` grind over
//     every /tmp/fleet-test-*.sock is the dirty-runner hang this PR removes.
//   - testutil.Sweep at suite START targets REAL /tmp (guarded: freshness
//     window + socketLive probe) — the safety-net that reaps a leaked server
//     from a prior crashed run. An earlier rev routed Sweep through the decoy
//     too, which silently disabled this net and let interrupted-run servers
//     leak forever.
//   - testutil.ForceReapTestServers at suite END force-reaps THIS suite's
//     leftover fleet-<id> servers on real /tmp — file-bound AND file-less —
//     but ONLY when the host is go-test-quiescent. Under `go test ./...`
//     sibling packages still own live servers, so the gate spares (no-op)
//     until the test fleet is idle; that gate (classifying on argv[0], not
//     the macOS-truncated comm) is what makes the teardown force-kill safe.
func TestMain(m *testing.M) {
	cleanup := testutil.IsolateSweepDir()
	_ = testutil.Sweep(time.Hour)
	code := m.Run()
	_ = testutil.ForceReapTestServers()
	cleanup() // before os.Exit — os.Exit skips defers
	os.Exit(code)
}
