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
// Implementation: thin wrapper over internal/gc.Reconcile so the
// sweep logic and the `fleet gc` CLI share one code path. Anything
// gc.Reconcile considers safe to reap, the test-suite sweeper reaps
// too — no duplicate policy.
package testutil

import (
	"fmt"
	"os"
	"time"

	"github.com/edisonshen/fleet/internal/gc"
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
func SweepDir(dir string, maxAge time.Duration) error {
	deps := gc.DefaultDeps()
	// Override the production socket lister so it scans the test's
	// chosen dir instead of /tmp. Everything else (the live-socket
	// probe, removal, max-age gate) stays on the production wiring.
	deps.ListSockets = func() ([]gc.SocketInfo, error) {
		return scanSocketsDirShim(dir)
	}
	// Override RemoveSocket to also be predictable on temp paths.
	deps.RemoveSocket = func(path string) error {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	// Pin time.Now via DefaultDeps; callers don't need to inject one.
	_, err := gc.Reconcile(gc.Options{
		Apply:  true,
		MaxAge: maxAge,
		Kinds:  []gc.Kind{gc.KindSockets},
	}, deps)
	if err != nil {
		return fmt.Errorf("testutil.SweepDir(%s): %w", dir, err)
	}
	return nil
}

// scanSocketsDirShim mirrors internal/gc.scanSocketsDir (which is
// unexported). Keeping a tiny private copy here avoids exporting an
// internal helper from the gc package just for test infrastructure
// reuse, and lets SweepDir scan an arbitrary directory for the unit
// test without touching production wiring.
func scanSocketsDirShim(dir string) ([]gc.SocketInfo, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var infos []gc.SocketInfo
	for _, e := range entries {
		name := e.Name()
		if len(name) < len("fleet-test-") || name[:len("fleet-test-")] != "fleet-test-" {
			continue
		}
		if !hasDotSockSuffix(name) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		infos = append(infos, gc.SocketInfo{
			Path:    dir + string(os.PathSeparator) + name,
			ModTime: info.ModTime(),
		})
	}
	return infos, nil
}

func hasDotSockSuffix(name string) bool {
	const sfx = ".sock"
	return len(name) >= len(sfx) && name[len(name)-len(sfx):] == sfx
}
