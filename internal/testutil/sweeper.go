// Package testutil hosts test-only helpers that don't belong inside
// production packages. Sub-packages (e.g. tmuxtest) carry the bigger
// shared fixtures; this top-level file provides the test-suite sweeper
// used by each test package's TestMain to reap stale
// /tmp/fleet-test-*.sock debris.
//
// Per feedback_fleet_owns_its_resources.md (operator postmortem
// 2026-05-21, 3,570 leaked sockets / 4 GB memory warning): tests must
// clean up their own resources. The canonical per-test cleanup lives
// in internal/testutil/tmuxtest.RequireTmux. This sweeper is the
// belt-and-suspenders layer: even if a test panics before its
// t.Cleanup runs, the next test package's TestMain reaps the orphan
// at startup and on exit.
//
// Why this package does NOT import internal/gc: gc imports tmux,
// spawn, agent, state. The test packages most likely to leak sockets
// (internal/tmux, internal/spawn) are gc's own dependencies — pulling
// gc back in via testutil would create import cycles in test builds.
// The sweep policy here is intentionally a small subset of gc's
// (sockets only, no orphan-agents / orphan-tmux / worktrees) so the
// duplication is one strscan loop, not a maintenance burden. The
// `fleet gc` CLI still uses the full gc.Reconcile path for operator-
// invoked cleanup; this sweeper is just the test-infrastructure half.
package testutil

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Sweep reaps stale /tmp/fleet-test-*.sock files older than maxAge.
// Intended to be called from each test package's TestMain at suite
// start AND end. Errors are returned wrapped so a TestMain that
// surfaces them can include the dir in the message.
//
// Production use:
//
//	func TestMain(m *testing.M) {
//	    _ = testutil.Sweep(time.Hour)
//	    code := m.Run()
//	    _ = testutil.Sweep(time.Hour)
//	    os.Exit(code)
//	}
//
// Errors are intentionally non-fatal at the call site (best-effort
// cleanup; failing the test suite on a sweep glitch would be worse
// than letting the CI gate catch leaks at the assertion step).
func Sweep(maxAge time.Duration) error {
	return SweepDir("/tmp", maxAge)
}

// SweepDir is Sweep with the scan directory injectable for tests.
// Behavior is identical to the production call (which uses /tmp) — the
// dir parameter exists only so the unit test in sweeper_test.go can
// run against t.TempDir() instead of mutating the operator's real /tmp.
//
// Algorithm matches internal/gc.reconcileSockets for the KindSockets
// family:
//
//  1. List entries matching fleet-test-*.sock under dir.
//  2. Skip entries younger than maxAge (within freshness window).
//  3. Probe whether a tmux server is still bound to the socket. If
//     yes, keep it (removing a live socket would strand the bound
//     server's clients — see codex iter-4 [P1] history in
//     internal/gc/gc.go:295).
//  4. Otherwise unlink. ENOENT collapses to success (concurrent
//     removal is fine).
func SweepDir(dir string, maxAge time.Duration) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("testutil.SweepDir read %s: %w", dir, err)
	}
	now := time.Now()
	var firstErr error
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "fleet-test-") {
			continue
		}
		if !strings.HasSuffix(name, ".sock") {
			// Out of scope — `fleet-test-*` without `.sock` includes
			// tmuxtest's temp-dir contents which Go's t.TempDir
			// already reaps. Mirrors scanSocketsDir in internal/gc.
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) < maxAge {
			continue // within freshness window — keep
		}
		path := filepath.Join(dir, name)
		if socketLive(path) {
			continue // live tmux server still bound — keep
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			if firstErr == nil {
				firstErr = fmt.Errorf("testutil.SweepDir remove %s: %w", path, err)
			}
		}
	}
	return firstErr
}

// socketLive probes whether a tmux server is bound to path. Returns
// true ONLY if `tmux -S <path> list-sessions` succeeds. File-gone /
// no-server / probe-error all map to false. Mirrors
// internal/gc.socketLiveOnDisk — duplicated here rather than imported
// to keep this package out of the gc dependency tree (gc → tmux →
// would cycle back if tmux's TestMain imported testutil → gc).
func socketLive(path string) bool {
	if _, err := os.Stat(path); err != nil {
		return false
	}
	if err := exec.Command("tmux", "-S", path, "list-sessions").Run(); err != nil {
		return false
	}
	return true
}
