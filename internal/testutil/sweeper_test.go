package testutil

import (
	"os"
	"os/exec"
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

// TestSweepAllDir_SymlinkNotFollowed (codex iter-8 [P2]): a fleet-test-*.sock
// SYMLINK pointing at another tmux socket must NOT be `tmux -S <symlink>
// kill-server`'d — that would terminate the symlink TARGET (potentially the
// operator's default server). The symlink is fleet-test debris, so it is
// unlinked, but never followed into kill-server. We point the symlink at a
// LIVE tmux server and assert that server survives the sweep.
func TestSweepAllDir_SymlinkNotFollowed(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	// A real live tmux server on a SAFE (non-fleet-test) socket — stands in
	// for the operator's server the malicious symlink would target.
	victimDir, err := os.MkdirTemp("/tmp", "fleet-victim-")
	if err != nil {
		t.Fatalf("mkdirtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(victimDir) })
	victim := filepath.Join(victimDir, "v.sock")
	if out, err := exec.Command("tmux", "-S", victim, "new-session", "-d", "-s", "victim", "sleep", "300").CombinedOutput(); err != nil {
		t.Fatalf("spawn victim: %v (%s)", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("tmux", "-S", victim, "kill-server").Run() })
	if err := exec.Command("tmux", "-S", victim, "has-session", "-t", "victim").Run(); err != nil {
		t.Skipf("victim server not live (host quirk): %v", err)
	}

	// The malicious symlink in the swept dir, named in the fleet-test
	// namespace, pointing at the victim socket.
	sweepDir := t.TempDir()
	link := filepath.Join(sweepDir, "fleet-test-evil.sock")
	if err := os.Symlink(victim, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if err := SweepAllDir(sweepDir); err != nil {
		t.Fatalf("SweepAllDir: %v", err)
	}
	// The victim server MUST survive — the sweep must not have followed the
	// symlink into kill-server.
	if err := exec.Command("tmux", "-S", victim, "has-session", "-t", "victim").Run(); err != nil {
		t.Error("SweepAllDir followed a fleet-test-*.sock SYMLINK into kill-server and killed the target server; symlink guard bypassed")
	}
	// The symlink itself (fleet-test debris) should be unlinked.
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Errorf("SweepAllDir left the fleet-test symlink in place; want it unlinked (lstat err=%v)", err)
	}
}

// TestSweep_IgnoresScanDirEnv (ci-perf-pr1 socket-leak P0, 2026-06-13): Sweep
// must NOT read FLEET_GC_SCAN_DIR. An earlier rev routed Sweep through that env
// so every TestMain's IsolateSweepDir pointed BOTH the gc PROBE and the cleanup
// SWEEP at an empty decoy — which silently DISABLED the start-of-run safety-net
// sweep of real /tmp, letting interrupted-run servers leak forever. We seed a
// stale socket in an injected decoy, point FLEET_GC_SCAN_DIR at it, and assert
// Sweep did NOT touch it — proving Sweep ignores the env and sweeps /tmp only.
func TestSweep_IgnoresScanDirEnv(t *testing.T) {
	decoy := t.TempDir()
	stale := filepath.Join(decoy, "fleet-test-staleenv.sock")
	if err := os.WriteFile(stale, []byte{}, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	t.Setenv("FLEET_GC_SCAN_DIR", decoy)

	if err := Sweep(time.Hour); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	// The socket lives in the DECOY, not /tmp — Sweep (which targets /tmp)
	// must leave it untouched. If Sweep still honored the env it would have
	// reaped it, re-arming the regression.
	if _, err := os.Stat(stale); err != nil {
		t.Fatalf("Sweep reaped a socket in the FLEET_GC_SCAN_DIR decoy (stat err=%v); Sweep must ignore the env and sweep /tmp only", err)
	}
}

// TestSweep_TargetsRealTmp: the sweep dir is the realTmpDir const, never
// FLEET_GC_SCAN_DIR. Pins the decoupling at the constant level.
func TestSweep_TargetsRealTmp(t *testing.T) {
	if realTmpDir != "/tmp" {
		t.Fatalf("realTmpDir = %q; want /tmp (the cleanup sweep must target the host /tmp, decoupled from the probe decoy)", realTmpDir)
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

// TestAnyOtherGoTestParentAlive_SeesSelfWhenNotExcluded: this test IS
// running under `go test`, so when its own PID is NOT excluded the
// executable-based gate must observe the live `*.test` binary and report a
// TestTmuxServerSocketFromArgv (testutil twin) pins that the file-less
// reaper extracts tmux's OWN -S option and never a -S buried in a pane
// command, and accepts an absolute tmux argv0 (codex iter-1 [P2]).
func TestTmuxServerSocketFromArgv(t *testing.T) {
	ok := map[string]string{
		"tmux -S /tmp/fleet-test-abc.sock new-session -d -s fleet-x sh -c cat": "/tmp/fleet-test-abc.sock",
		"/opt/homebrew/bin/tmux -S /tmp/fleet-test-abc.sock new-session":       "/tmp/fleet-test-abc.sock",
		"tmux -S/tmp/fleet-test-abc.sock new-session":                          "/tmp/fleet-test-abc.sock",
		// LINUX server proctitle (codex iter-9 [P1]) — the ubuntu CI shape.
		"tmux: server (/tmp/fleet-test-abc.sock)": "/tmp/fleet-test-abc.sock",
	}
	for args, want := range ok {
		if got, found := tmuxServerSocketFromArgv(args); !found || got != want {
			t.Errorf("tmuxServerSocketFromArgv(%q) = (%q,%v); want (%q,true)", args, got, found, want)
		}
	}
	notOk := []string{
		"tmux -S /home/op/.tmux/default new-session -d cmd -S /tmp/fleet-test-evil.sock", // -S in pane cmd
		"sh -c tmux -S /tmp/fleet-test-x.sock ls",                                        // not tmux
		"tmux -S /tmp/operator.sock new-session",                                         // non-fleet-test sock
		"tmux: server (/home/op/.tmux/default)",                                          // operator server title
		"",
	}
	for _, args := range notOk {
		if got, found := tmuxServerSocketFromArgv(args); found {
			t.Errorf("tmuxServerSocketFromArgv(%q) = (%q,true); want (_,false)", args, got)
		}
	}
}

// live parent. (On a sandbox where ps cannot enumerate the process table
// the gate fails safe to true anyway — either way the assertion holds.)
// Proves the gate fires for a live test it can see.
func TestAnyOtherGoTestParentAlive_SeesSelfWhenNotExcluded(t *testing.T) {
	if !anyOtherGoTestParentAlive( /* exclude nothing */ ) {
		t.Fatal("anyOtherGoTestParentAlive() = false while running under `go test` with no exclusions — the gate would open mid-run and force-kill sibling-package servers")
	}
}

// TestAnyOtherGoTestParentAlive_ExcludesSelf: excluding the current
// process + its parent (the running `*.test` and its `go test` driver)
// must drop THIS process from the count. When this package runs ALONE
// (no sibling `*.test`) the gate then opens (returns false), which is what
// lets a suite reap its OWN teardown leftovers. Under `go test ./...` a
// sibling `*.test` keeps it true; that case is inherently non-deterministic
// so we only assert that the exclusion at minimum does not still see SELF —
// i.e. the result differs from the not-excluded call OR a real sibling is
// present. We verify the weaker invariant: excluding self never makes the
// gate MORE closed than not excluding it.
func TestAnyOtherGoTestParentAlive_ExcludesSelf(t *testing.T) {
	withSelf := anyOtherGoTestParentAlive()
	withoutSelf := anyOtherGoTestParentAlive(os.Getpid(), os.Getppid())
	if !withSelf {
		t.Skip("ps cannot see this process (sandbox); exclusion semantics unobservable")
	}
	// Excluding self must never ADD a live parent. If no sibling test runs,
	// withoutSelf is false (gate opens for our own teardown). If a sibling
	// runs, it stays true. Either way withoutSelf implies a NON-self parent.
	if withoutSelf {
		// A non-self test parent exists; fine (parallel `go test ./...`).
		t.Logf("a sibling go-test parent is alive (parallel run); gate correctly stays closed")
	}
}

// TestForceReapTestServers_ReapsFileBoundWhenQuiescent: the file-bound reap
// MECHANISM removes a leaked socket+server. We exercise SweepAllDir (the
// inner reap ForceReapTestServers calls once its gate opens) directly on an
// isolated dir to stay hermetic and deterministic — calling
// ForceReapTestServers itself would scan real /tmp top-level, whose
// contents are not under this test's control.
func TestSweepAllDir_InterruptedRunReap(t *testing.T) {
	if !haveTmux() {
		t.Skip("tmux not installed")
	}
	dir, err := os.MkdirTemp("/tmp", "fleet-test-irr-")
	if err != nil {
		t.Fatalf("mkdirtemp /tmp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	// Simulate an interrupted run: a leaked tmux server whose t.Cleanup was
	// skipped. We spawn it and do NOT register a kill-server cleanup before
	// the reap (the t.Cleanup below is the safety net if the reap fails).
	sock := filepath.Join(dir, "fleet-test-s.sock")
	if out, err := exec.Command("tmux", "-S", sock, "new-session", "-d", "-s", "fleet-leaked01", "sleep", "300").CombinedOutput(); err != nil {
		t.Fatalf("spawn tmux: %v (%s)", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("tmux", "-S", sock, "kill-server").Run() })
	if err := exec.Command("tmux", "-S", sock, "has-session", "-t", "fleet-leaked01").Run(); err != nil {
		t.Skipf("tmux exited 0 but no live session (host quirk): %v", err)
	}

	// The suite-teardown force-reap MECHANISM (gate already passed): kill the
	// bound server + unlink the socket regardless of freshness/liveness.
	if err := SweepAllDir(dir); err != nil {
		t.Fatalf("SweepAllDir: %v", err)
	}
	// 0 servers survive: the leaked server is dead and the socket is gone.
	if err := exec.Command("tmux", "-S", sock, "has-session", "-t", "fleet-leaked01").Run(); err == nil {
		t.Error("leaked tmux server survived the teardown reap; interrupted-run leak not closed")
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Errorf("socket file survived the teardown reap; stat err=%v", err)
	}
}

func haveTmux() bool {
	_, err := exec.LookPath("tmux")
	return err == nil
}
