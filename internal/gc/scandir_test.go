package gc

// scandir_test.go — ci-perf-pr1 (P0): the env-aware GC socket-scan seam.
//
// PROBLEM this seam fixes. A unit test that drives a real fleet code path
// (dispatch/status) triggers a reconcile that scans the host's real /tmp and
// runs `tmux -S <sock> ls` on EVERY fleet-test-*.sock it finds. On a host with
// hundreds of leaked sockets (fleet#165) that is N tmux subprocesses per test
// and the suite hangs (24-min kill on PR #232). gcScanDir() lets tests point
// the scan at an empty decoy so no unit test touches real /tmp.
//
// These tests pin the seam's three load-bearing properties:
//   - production default: FLEET_GC_SCAN_DIR unset => both closures scan /tmp.
//   - injectable scan dir: DefaultDepsWithScanDir(dir) => the KindSockets
//     ListSockets closure scans dir, and the per-socket liveness probe targets
//     a path UNDER dir, never /tmp.
//   - grep guard (in scandir_grepguard_test.go-style assertion below): the
//     `/tmp` literal lives ONLY inside gcScanDir(); neither callsite hardcodes
//     scanSocketsDir with a `/tmp` argument.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestNoHardcodedScanSocketsDirTmp is the grep guard from the task plan: the
// `/tmp` literal must live ONLY inside gcScanDir(). Both callsites
// (ListSockets at gc.go:948, listLiveTestSocketsOnDisk) must scan
// gcScanDir()'s result, never `scanSocketsDir("/tmp")` directly — otherwise a
// callsite bypasses FLEET_GC_SCAN_DIR and the test-isolation hole reopens.
// Enforced in CI (a Go test), not just a manual `grep`, so a future edit that
// reintroduces the hardcoded call fails the suite.
func TestNoHardcodedScanSocketsDirTmp(t *testing.T) {
	// Scan the package's non-test .go sources. A stray literal inside a test
	// fixture (e.g. the integration test that legitimately scans /tmp) is out
	// of scope — the guard targets production callsites.
	const needle = `scanSocketsDir("/tmp")`
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir .: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("ReadFile %s: %v", name, err)
		}
		if strings.Contains(string(b), needle) {
			t.Errorf("%s contains hardcoded %s — callsites must use scanSocketsDir(gcScanDir()); the /tmp literal belongs only inside gcScanDir()", name, needle)
		}
	}
}

// TestGCScanDir_DefaultIsTmp: env unset => gcScanDir() resolves /tmp, the
// historical hardcoded value. This is the production-default guard from the
// task plan ("Production unchanged"). Cannot use t.Parallel (mutates env).
func TestGCScanDir_DefaultIsTmp(t *testing.T) {
	t.Setenv("FLEET_GC_SCAN_DIR", "") // unset => default branch
	if got := gcScanDir(); got != "/tmp" {
		t.Fatalf("gcScanDir() with FLEET_GC_SCAN_DIR unset = %q; want /tmp", got)
	}
}

// TestGCScanDir_EnvOverride: a non-empty env value wins. This is what
// cmd/fleet TestMain relies on to redirect the whole package at a decoy.
func TestGCScanDir_EnvOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FLEET_GC_SCAN_DIR", dir)
	if got := gcScanDir(); got != dir {
		t.Fatalf("gcScanDir() with FLEET_GC_SCAN_DIR=%q = %q; want the env value", dir, got)
	}
}

// TestDefaultDeps_ScanDirFromEnv: DefaultDeps() (the production constructor)
// reads the env PER CALL and threads it into BOTH /tmp-scanning closures, not
// just one. We seed a real socket in a decoy dir, point the env at it, and
// assert ListSockets enumerates the decoy socket — proving the env read is
// live for the production path, not just the explicit DefaultDepsWithScanDir
// helper.
func TestDefaultDeps_ScanDirFromEnv(t *testing.T) {
	dir := t.TempDir()
	sock := seedSocket(t, dir, "fleet-test-deadbeef.sock")
	t.Setenv("FLEET_GC_SCAN_DIR", dir)

	deps := DefaultDeps()
	infos, err := deps.ListSockets()
	if err != nil {
		t.Fatalf("ListSockets: %v", err)
	}
	if !containsPath(infos, sock) {
		t.Fatalf("DefaultDeps().ListSockets did not enumerate seeded socket %s; got %+v", sock, infos)
	}
	// And every enumerated path must live under the decoy dir — NONE under
	// /tmp. A regression that left a callsite hardcoded to /tmp would surface
	// here as a path outside dir.
	for _, info := range infos {
		if filepath.Dir(info.Path) != dir {
			t.Fatalf("ListSockets returned a path outside the scan dir: %s (want parent %s)", info.Path, dir)
		}
	}
}

// TestKindSockets_ScansInjectedDir_NotTmp: the gc.go:948 callsite. Drive a
// full Reconcile(KindSockets) through DefaultDepsWithScanDir(dir) with an
// AGED real socket (mtime > MaxAge) so it passes the `age < MaxAge` skip at
// reconcileSockets and reaches the SocketLive probe. We override SocketLive to
// a recorder so the test is deterministic + parallel-safe (no real tmux exec),
// and assert (a) the socket under dir is reconciled (ReadDir scanned dir) and
// (b) the liveness probe was handed a path UNDER dir, never /tmp.
//
// Why aged + KindSockets-specific: a FRESH socket via the OrphanTmux path
// would be enumerated even if gc.go:948 stayed hardcoded to /tmp, so the
// socket MUST be aged and routed through KindSockets to actually exercise this
// callsite (per the task plan's acceptance mechanics).
func TestKindSockets_ScansInjectedDir_NotTmp(t *testing.T) {
	dir := t.TempDir()
	sock := seedSocket(t, dir, "fleet-test-aged01.sock")
	// Age the socket well past MaxAge so reconcileSockets does not `continue`
	// on the freshness gate and reaches the SocketLive probe.
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(sock, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	deps := DefaultDepsWithScanDir(dir)
	// Pin "now" so the age math is deterministic regardless of wall clock.
	deps.Now = func() time.Time { return time.Now() }
	var probed []string
	deps.SocketLive = func(path string) bool {
		probed = append(probed, path)
		return false // not live => the socket becomes a would-remove action
	}
	// Don't actually unlink the seeded file (dry-run); we only need the action.
	got, err := Reconcile(Options{Apply: false, MaxAge: 24 * time.Hour, Kinds: []Kind{KindSockets}}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if _, ok := findAction(got, KindSockets, sock); !ok {
		t.Fatalf("expected a KindSockets action for the aged socket %s (proves ReadDir scanned %s); got %+v", sock, dir, got.Actions)
	}
	if len(probed) == 0 {
		t.Fatalf("SocketLive was never probed — the aged socket did not reach the liveness gate")
	}
	for _, p := range probed {
		if filepath.Dir(p) != dir {
			t.Fatalf("SocketLive probed a path outside the injected scan dir: %s (want parent %s, never /tmp)", p, dir)
		}
	}
}

// seedSocket creates a fleet-test-*.sock fixture under dir for the scan to
// enumerate. scanSocketsDir filters purely by name (prefix `fleet-test-`,
// suffix `.sock`) and reads e.Info() — it does NOT check os.ModeSocket — and
// these tests stub SocketLive, so a plain regular file is a sufficient and
// portable fixture (a real net.Listen unix socket would hit the macOS
// ~104-byte socket-path limit under t.TempDir). The ModeSocket symlink guard
// only gates firstFleetSession on the OrphanTmux/dispatch path, covered by the
// cmd/fleet test. t.TempDir removes the file on cleanup.
func seedSocket(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte{}, 0o600); err != nil {
		t.Fatalf("seed socket fixture %s: %v", path, err)
	}
	return path
}

// containsPath reports whether infos holds a SocketInfo with Path == want.
func containsPath(infos []SocketInfo, want string) bool {
	for _, info := range infos {
		if info.Path == want {
			return true
		}
	}
	return false
}
