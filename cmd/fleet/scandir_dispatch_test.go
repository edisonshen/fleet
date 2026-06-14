package main

// scandir_dispatch_test.go — ci-perf-pr1 (P0): prove the OrphanTmux reconcile
// callsite (live_test_sockets.go) scans FLEET_GC_SCAN_DIR, not the host /tmp.
//
// This is the callsite the PR #232 hang flowed through: `fleet dispatch` runs
// runDispatchReconcile(KindOrphanTmux) which scans the socket dir and execs
// `tmux -S <sock> ls` on every fleet-test-*.sock. We seed ONE real socket in a
// decoy dir, point FLEET_GC_SCAN_DIR at it, install a PATH fake-`tmux` that
// records every invocation, and assert: at least one probe ran, every probed
// socket path lives UNDER the decoy dir, and NONE under /tmp.
//
// The fake-tmux recorder (newFakeTmuxRecorder) is intentionally REUSABLE —
// PR-2 reuses it as a "did a routed test exec real tmux?" bypass detector.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDispatchReconcile_OrphanTmux_ScansInjectedDir_NotTmp(t *testing.T) {
	// A SHORT decoy dir under /tmp (not t.TempDir): firstFleetSession requires
	// a genuine os.ModeSocket, and net.Listen("unix", …) hits the macOS
	// ~104-byte socket-path limit under the long t.TempDir path. The dir is a
	// unique subdir of /tmp, distinct from bare /tmp — the test still proves
	// the scan is scoped to FLEET_GC_SCAN_DIR and never walks /tmp itself.
	dir, err := os.MkdirTemp("/tmp", "fgc-")
	if err != nil {
		t.Fatalf("mkdirtemp /tmp decoy: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := seedDispatchSocket(t, dir, "fleet-test-cabb1e.sock")

	// Override the package-wide decoy (TestMain) with this test's dir so the
	// real DefaultDeps() reconcile (via dispatchReconcileFn, unstubbed) scans
	// the seeded socket.
	t.Setenv("FLEET_GC_SCAN_DIR", dir)

	rec := newFakeTmuxRecorder(t)

	var stderr bytes.Buffer
	// isCoordSpawn=false runs the full OrphanTmux pass. The reconcile error
	// path is non-fatal by contract; we only care about the recorded probes.
	runDispatchReconcile(&stderr, false)

	probes := rec.socketProbes()
	if len(probes) == 0 {
		t.Fatalf("expected >=1 tmux probe on a socket under %s; fake-tmux recorded none.\nall invocations:\n%s\nstderr:\n%s",
			dir, strings.Join(rec.lines(), "\n"), stderr.String())
	}
	sawSeeded := false
	for _, p := range probes {
		// Every probe must target a socket whose PARENT is exactly the
		// injected decoy dir. A regression that scanned bare /tmp would probe
		// sockets whose parent is "/tmp" (not the unique decoy subdir), which
		// this catches.
		if filepath.Dir(p) != dir {
			t.Fatalf("tmux probe targeted a socket OUTSIDE the injected scan dir: %s (parent %s, want parent %s — the OrphanTmux callsite must not walk bare /tmp)",
				p, filepath.Dir(p), dir)
		}
		if p == sock {
			sawSeeded = true
		}
	}
	if !sawSeeded {
		t.Fatalf("the seeded socket %s was never probed; probes=%v", sock, probes)
	}
}

// seedDispatchSocket creates a real Unix-domain socket file under dir. A
// genuine os.ModeSocket is required: firstFleetSession rejects regular files
// via its Lstat+ModeSocket symlink guard, so os.WriteFile would be silently
// skipped and the probe would never fire.
func seedDispatchSocket(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	ln := listenUnix(t, path)
	t.Cleanup(func() { _ = ln.Close() })
	return path
}
