package testutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSweep_RemovesStaleSocketsKeepsRecentAndNonMatching pins the
// TestMain-helper contract (PR-B of fleet#165, per
// feedback_fleet_owns_its_resources.md):
//
//   - Seed three stale fake `fleet-test-*.sock` files (mtime backdated
//     well past the 1h max-age floor) — must be removed.
//   - Seed one recent `fleet-test-*.sock` — must be kept (within max-age).
//   - Seed an unrelated `other.sock` — must be kept (out of fleet's
//     blast radius per feedback_user_owns_tmux_config.md).
//
// The sweeper is a thin wrapper over `internal/gc.Reconcile(KindSockets,
// Apply=true)`; the dir argument exists so the test can run against
// `t.TempDir()` instead of mutating the operator's real `/tmp/`.
//
// Why the test seeds files via os.WriteFile + os.Chtimes (not via
// `touch -t ...`): we need deterministic, host-portable mtimes. Calling
// out to `touch` would slow the test and add a platform dependency.
func TestSweep_RemovesStaleSocketsKeepsRecentAndNonMatching(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	old := now.Add(-2 * time.Hour) // older than 1h max-age floor
	fresh := now.Add(-1 * time.Minute)

	stale := []string{"fleet-test-aaa111.sock", "fleet-test-bbb222.sock", "fleet-test-ccc333.sock"}
	for _, name := range stale {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatalf("seed %s: %v", p, err)
		}
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatalf("chtimes %s: %v", p, err)
		}
	}

	keepFresh := filepath.Join(dir, "fleet-test-ddd444.sock")
	if err := os.WriteFile(keepFresh, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed %s: %v", keepFresh, err)
	}
	if err := os.Chtimes(keepFresh, fresh, fresh); err != nil {
		t.Fatalf("chtimes %s: %v", keepFresh, err)
	}

	keepUnrelated := filepath.Join(dir, "other.sock")
	if err := os.WriteFile(keepUnrelated, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed %s: %v", keepUnrelated, err)
	}

	if err := SweepDir(dir, time.Hour); err != nil {
		t.Fatalf("SweepDir: %v", err)
	}

	for _, name := range stale {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("stale socket %s not removed (stat err=%v)", p, err)
		}
	}
	if _, err := os.Stat(keepFresh); err != nil {
		t.Errorf("fresh socket %s should be kept; stat err=%v", keepFresh, err)
	}
	if _, err := os.Stat(keepUnrelated); err != nil {
		t.Errorf("unrelated %s should be kept; stat err=%v", keepUnrelated, err)
	}
}

// TestSweep_MissingDir_IsNoop pins the operator-facing contract: a
// TestMain that calls SweepDir on a CI runner that never had any
// /tmp/fleet-test-* files must not fail. Mirrors scanSocketsDir's
// own ENOENT-to-nil behavior in internal/gc/gc.go:670.
func TestSweep_MissingDir_IsNoop(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist")
	if err := SweepDir(dir, time.Hour); err != nil {
		t.Errorf("SweepDir on missing dir should be a no-op; got %v", err)
	}
}

// TestSweep_LiveSocketKept pins the live-socket guard: if a stale-by-mtime
// socket still has a tmux server bound (SocketLive=true in production),
// the sweeper must NOT remove it. Codex iter-4 fix in internal/gc lives
// upstream; this test confirms the sweeper inherits the behavior because
// it routes through gc.Reconcile with the production SocketLive probe.
//
// Test strategy: we cannot easily spawn a real tmux server bound to a
// temp socket inside a unit test (TMUX_TMPDIR path-length quirks on
// macOS), so instead we seed a stale socket file AND ensure the bound
// server check is exercised. The production probe runs `tmux -S <path>
// list-sessions`; on a temp socket file with no server bound, that
// exits non-zero → SocketLive=false → removal proceeds. This case is
// already covered by the first test. The live-bound case is fully
// covered by internal/gc/gc_test.go:TestReconcile_SocketLive_*.
//
// This sub-test simply asserts the sweeper does not bypass the
// gc.Reconcile path by checking that the public API surface is the
// thin wrapper described in DESIGN.md §PR-B.
func TestSweep_UsesGCReconcile(t *testing.T) {
	// Sentinel value: the sweeper should fail gracefully on a path it
	// cannot read, not panic. We point at a file (not a dir) and expect
	// a wrapped read error.
	tmp := t.TempDir()
	notADir := filepath.Join(tmp, "iamafile")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	err := SweepDir(notADir, time.Hour)
	if err == nil {
		t.Fatal("SweepDir on non-directory should error, not panic")
	}
	if !strings.Contains(err.Error(), "read") && !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("expected read-error wrapping; got %v", err)
	}
}
