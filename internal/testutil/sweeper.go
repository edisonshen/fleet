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
//
// ci-perf-pr1 (P0), codex iter-3 [P1]: Sweep honors FLEET_GC_SCAN_DIR (the
// SAME env the internal/gc scan seam reads), defaulting to /tmp when unset.
// Without this, every test package's TestMain (cmd/fleet, internal/spawn,
// internal/handoffop, internal/tmux, internal/dispatch) probed EVERY leaked
// /tmp/fleet-test-*.sock with `tmux -S <sock> list-sessions` at suite start —
// on a dirty runner that is N tmux subprocesses BEFORE any test, the exact
// hang this PR removes. Pointing the env at an empty decoy (see
// IsolateSweepDir) makes the sweep a no-op. Production has no FLEET_GC_SCAN_DIR
// set, so operator-side behavior is unchanged.
func Sweep(maxAge time.Duration) error {
	return SweepDir(sweepDir(), maxAge)
}

// sweepDir resolves the sweep directory from FLEET_GC_SCAN_DIR, defaulting to
// /tmp. Mirrors internal/gc.gcScanDir so the test sweeper and the production
// reconcile scan honor the SAME isolation env — one knob isolates both.
func sweepDir() string {
	if dir := os.Getenv("FLEET_GC_SCAN_DIR"); dir != "" {
		return dir
	}
	return "/tmp"
}

// IsolateSweepDir points FLEET_GC_SCAN_DIR at a fresh empty decoy dir for the
// whole test binary and returns a cleanup func that restores the prior env and
// removes the decoy. Call it FIRST in a TestMain so the start/end Sweep calls
// (and any FLEET_GC_SCAN_DIR-honoring reconcile the package's tests trigger)
// scan the decoy instead of the host /tmp. There is no *testing.T in TestMain,
// so this hand-rolls the env save/restore instead of t.Setenv.
//
//	func TestMain(m *testing.M) {
//	    cleanup := testutil.IsolateSweepDir()
//	    _ = testutil.Sweep(time.Hour)
//	    code := m.Run()
//	    _ = testutil.Sweep(time.Hour)
//	    cleanup()
//	    os.Exit(code) // cleanup() BEFORE os.Exit — os.Exit skips defers
//	}
func IsolateSweepDir() func() {
	prev, had := os.LookupEnv("FLEET_GC_SCAN_DIR")
	decoy, err := os.MkdirTemp("", "fleet-gc-sweep-decoy-")
	if err != nil {
		// MkdirTemp failed: we can't isolate. Rather than SILENTLY falling back
		// to the prior (possibly /tmp) behavior — which would re-arm the very
		// hang this seam prevents with no trace — emit a loud stderr diagnostic
		// (feedback_surface_dont_silo) so a CI/dev run that suddenly grinds /tmp
		// has a breadcrumb. We still return rather than panic: a TestMain must
		// not abort the whole package over a transient tmp failure (claude
		// adversarial F4).
		fmt.Fprintf(os.Stderr,
			"testutil.IsolateSweepDir: WARNING could not create decoy scan dir (%v); "+
				"FLEET_GC_SCAN_DIR left as-is — the test sweep may scan real /tmp and grind on leaked sockets\n",
			err)
		return func() {}
	}
	_ = os.Setenv("FLEET_GC_SCAN_DIR", decoy)
	return func() {
		if had {
			_ = os.Setenv("FLEET_GC_SCAN_DIR", prev)
		} else {
			_ = os.Unsetenv("FLEET_GC_SCAN_DIR")
		}
		_ = os.RemoveAll(decoy)
	}
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

// SweepAll is the suite-teardown variant of Sweep. Reaps EVERY
// /tmp/fleet-test-*.sock regardless of freshness AND regardless of
// whether a tmux server is still bound. Use ONLY from TestMain
// teardown (after m.Run()) — once `go test` is exiting, a live test
// socket is by definition an orphan whose owning test process either
// panicked, called os.Exit, or was killed mid-run.
//
// Closes the gap that lets bypassed-t.Cleanup orphans (7-day-old
// claude/tmux procs in the 2026-05-29 OOM, per
// docs/DESIGN-lifecycle-leak-recurrence.md PR-A root cause #1) survive.
// Sweep (with freshness + socketLive guards) is still correct for the
// SUITE-START sweep where another concurrent `go test` may legitimately
// own a fresh live socket; SweepAll is intentionally narrower in scope.
func SweepAll() error {
	return SweepAllDir("/tmp")
}

// SweepAllDir is SweepAll with the scan directory injectable for tests.
// Like SweepDir but bypasses BOTH the freshness window AND the
// socketLive() guard. For each `fleet-test-*.sock` entry:
//
//  1. If a tmux server is still bound to the socket, kill it via
//     `tmux -S <path> kill-server` (idempotent — exits non-zero with no
//     server, which we ignore). This stops the leaked claude/tmux
//     process before the file is unlinked, so we don't strand orphans
//     after the .sock disappears.
//  2. Unlink the socket file. ENOENT is success.
func SweepAllDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("testutil.SweepAllDir read %s: %w", dir, err)
	}
	var firstErr error
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "fleet-test-") {
			continue
		}
		if !strings.HasSuffix(name, ".sock") {
			continue
		}
		path := filepath.Join(dir, name)
		// kill-server is idempotent: tmux exits non-zero when no
		// server is running, which is fine — we only want any bound
		// server reaped before the socket file is unlinked. Errors
		// here are best-effort; we proceed to unlink regardless.
		_ = exec.Command("tmux", "-S", path, "kill-server").Run()
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			if firstErr == nil {
				firstErr = fmt.Errorf("testutil.SweepAllDir remove %s: %w", path, err)
			}
		}
	}
	return firstErr
}
