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
// The sweeper is a self-contained re-implementation of the KindSockets
// subset of `internal/gc.Reconcile` — see sweeper.go:14-23 for the
// import-cycle rationale. The `dir` argument exists so the test can run
// against `t.TempDir()` instead of mutating the operator's real `/tmp/`.
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

// TestSweep_NonDirectoryPath_ReturnsWrappedError pins the error-wrapping
// contract: if SweepDir is called against a path that exists but is not a
// directory, it returns a wrapped read error (not a panic). This is the
// "operator pointed FLEET_TMUX_SOCKET at a file" / "concurrent rename"
// defensive case.
//
// What this test does NOT cover (intentional gap, documented for the
// next reader): the live-socket-kept branch (`socketLive() == true`
// keeps the socket). Spawning a real tmux server on a temp socket
// inside a unit test is awkward on macOS (TMUX_TMPDIR path-length
// limits) and the value-add is marginal — the production probe is a
// 4-line `tmux -S <path> list-sessions` call exercised on every actual
// `fleet gc` invocation. Per PR-B's import-cycle design call
// (sweeper.go:14-23), this package does NOT import internal/gc, so the
// upstream TestReconcile_SocketLive_* tests in internal/gc/gc_test.go
// no longer cover this sweeper's `socketLive()` — that copy is its own
// surface. Acceptable because the implementation is a literal one-liner
// shell-out; if it grows (e.g., flock probing or server-version
// matching), revisit and add a tmuxtest.RequireTmux-backed coverage
// test here.
func TestSweep_NonDirectoryPath_ReturnsWrappedError(t *testing.T) {
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

// TestSweepAllDir_ReapsEverythingIgnoringFreshness pins the T4/T5 contract
// from docs/TASK-PLAN-leak-test-spawn-stub.md: the suite-teardown mode of
// the sweeper bypasses BOTH the freshness window AND the socketLive guard.
// Once `go test` is exiting, a fresh OR live `/tmp/fleet-test-*.sock` is
// BY DEFINITION an orphan — its owning test process is already gone
// (panic, os.Exit, killed mid-run) or about to be (TestMain m.Run()
// returned). The existing SweepDir spares both, which is the gap that
// lets bypassed-t.Cleanup orphans (7-day-old claude/tmux procs in the
// 2026-05-29 OOM) survive.
//
// T4: fresh-but-non-live socket (no tmux server bound). Existing SweepDir
// would skip via the freshness window; SweepAllDir reaps it.
// T5: stale or fresh socket regardless — even when socketLive() would
// keep it (no tmux probe in this unit test; we just assert the freshness
// guard is bypassed).
func TestSweepAllDir_ReapsEverythingIgnoringFreshness(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	fresh := now.Add(-1 * time.Minute) // well inside any maxAge window

	// Seed two fresh sockets that SweepDir(time.Hour) would spare.
	fresh1 := filepath.Join(dir, "fleet-test-aaa111.sock")
	fresh2 := filepath.Join(dir, "fleet-test-bbb222.sock")
	for _, p := range []string{fresh1, fresh2} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatalf("seed %s: %v", p, err)
		}
		if err := os.Chtimes(p, fresh, fresh); err != nil {
			t.Fatalf("chtimes %s: %v", p, err)
		}
	}

	// Unrelated file MUST still be spared (only fleet-test-* in scope).
	unrelated := filepath.Join(dir, "other.sock")
	if err := os.WriteFile(unrelated, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed %s: %v", unrelated, err)
	}

	if err := SweepAllDir(dir); err != nil {
		t.Fatalf("SweepAllDir: %v", err)
	}

	for _, p := range []string{fresh1, fresh2} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("fresh socket %s not reaped by SweepAllDir (stat err=%v); freshness guard MUST be bypassed in suite-teardown mode", p, err)
		}
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Errorf("unrelated file %s should still be kept; got stat err=%v", unrelated, err)
	}
}

// TestSweepAllDir_MissingDir_IsNoop mirrors TestSweep_MissingDir_IsNoop:
// CI runners without any /tmp/fleet-test-* files must not see TestMain
// teardown fail.
func TestSweepAllDir_MissingDir_IsNoop(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist")
	if err := SweepAllDir(dir); err != nil {
		t.Errorf("SweepAllDir on missing dir should be a no-op; got %v", err)
	}
}

// TestSweep_HonorsScanDirEnv (ci-perf-pr1, codex iter-3 [P1]): Sweep() reads
// FLEET_GC_SCAN_DIR so a TestMain can redirect the sweep at a decoy instead of
// the host /tmp. We seed a stale socket in an injected dir, point the env at
// it, and assert Sweep() removed it — proving Sweep() walked the env dir, not
// /tmp.
func TestSweep_HonorsScanDirEnv(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "fleet-test-staleenv.sock")
	if err := os.WriteFile(stale, []byte{}, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	t.Setenv("FLEET_GC_SCAN_DIR", dir)

	if err := Sweep(time.Hour); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale socket in the env-injected dir should be reaped; stat err=%v (Sweep did not honor FLEET_GC_SCAN_DIR)", err)
	}
}

// TestSweep_DefaultsToTmpWhenEnvUnset: with FLEET_GC_SCAN_DIR unset, sweepDir()
// resolves /tmp — production behavior unchanged.
func TestSweep_DefaultsToTmpWhenEnvUnset(t *testing.T) {
	t.Setenv("FLEET_GC_SCAN_DIR", "")
	if got := sweepDir(); got != "/tmp" {
		t.Fatalf("sweepDir() with FLEET_GC_SCAN_DIR unset = %q; want /tmp", got)
	}
}

// TestIsolateSweepDir_SetsDecoyAndRestores: the helper sets FLEET_GC_SCAN_DIR
// to a fresh empty dir, and cleanup() restores the prior env value (or unsets)
// and removes the decoy.
func TestIsolateSweepDir_SetsDecoyAndRestores(t *testing.T) {
	// Case 1: env previously unset -> cleanup unsets it.
	_ = os.Unsetenv("FLEET_GC_SCAN_DIR")
	cleanup := IsolateSweepDir()
	decoy := os.Getenv("FLEET_GC_SCAN_DIR")
	if decoy == "" {
		t.Fatal("IsolateSweepDir did not set FLEET_GC_SCAN_DIR")
	}
	if fi, err := os.Stat(decoy); err != nil || !fi.IsDir() {
		t.Fatalf("decoy dir %q not created: err=%v", decoy, err)
	}
	cleanup()
	if _, ok := os.LookupEnv("FLEET_GC_SCAN_DIR"); ok {
		t.Errorf("cleanup should unset FLEET_GC_SCAN_DIR when it was unset before")
	}
	if _, err := os.Stat(decoy); !os.IsNotExist(err) {
		t.Errorf("cleanup should remove the decoy dir; stat err=%v", err)
	}

	// Case 2: env previously set -> cleanup restores the prior value.
	t.Setenv("FLEET_GC_SCAN_DIR", "/some/prior")
	cleanup2 := IsolateSweepDir()
	if got := os.Getenv("FLEET_GC_SCAN_DIR"); got == "/some/prior" {
		t.Fatal("IsolateSweepDir should have overridden the prior value with a decoy")
	}
	cleanup2()
	if got := os.Getenv("FLEET_GC_SCAN_DIR"); got != "/some/prior" {
		t.Errorf("cleanup should restore prior FLEET_GC_SCAN_DIR; got %q want /some/prior", got)
	}
}

// TestIsolateSweepDir_DecoyNotFleetTestPrefixed guards the PR #233 fix: the
// EMPTY decoy must NOT carry the `fleet-test-` prefix, or a happy-path run
// where the decoy reaps a moment after CI's leak-gate snapshot (or a
// SIGKILL-skipped cleanup) trips the "Assert no /tmp/fleet-test-* leak" step.
// The decoy holds no socket, so it is never the real leak that gate guards.
func TestIsolateSweepDir_DecoyNotFleetTestPrefixed(t *testing.T) {
	_ = os.Unsetenv("FLEET_GC_SCAN_DIR")
	cleanup := IsolateSweepDir()
	defer cleanup()
	decoy := os.Getenv("FLEET_GC_SCAN_DIR")
	if decoy == "" {
		t.Fatal("IsolateSweepDir did not set FLEET_GC_SCAN_DIR")
	}
	base := filepath.Base(decoy)
	if strings.HasPrefix(base, "fleet-test-") {
		t.Errorf("decoy dir %q must NOT use the `fleet-test-` prefix — CI's "+
			"/tmp/fleet-test-* leak gate would flag this empty scaffolding dir "+
			"as a leak (PR #233)", base)
	}
}
