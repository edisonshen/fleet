package gc

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TDD suite for leak-gc-live-testsock (PR-D, DESIGN-lifecycle-leak-recurrence.md).
//
// A LIVE `fleet-<id>` tmux server bound to /tmp/fleet-test-*.sock whose
// owning `go test` process is gone is the resource the operator had to
// kill BY HAND during the 2026-05-29 OOM. This classifier closes that
// gap: surface such orphans by default, kill them only under
// --apply --aggressive. A socket still owned by a live `go test` process
// is NEVER touched (in-flight tests are protected).
//
// The classifier runs as part of the orphan-tmux kind (Action.Kind =
// KindOrphanTmux) so `fleet gc --kinds orphan-tmux` covers it.

// withLiveTestSocketStubs wires the live-test-socket classifier deps on
// top of stubDeps. Tests override individual hooks to exercise specific
// branches.
func withLiveTestSocketStubs(d Deps) Deps {
	d.ListLiveTestSockets = func() ([]LiveTestSocket, error) { return nil, nil }
	d.KillTmuxServer = func(string) error {
		return errors.New("stubDeps: KillTmuxServer should not run")
	}
	return d
}

// T1 — orphan test-sock tmux surfaced in dry-run (default, no kill).
func TestReconcile_LiveTestSock_OrphanSurfacedDryRun(t *testing.T) {
	now := time.Date(2026, 5, 29, 12, 0, 0, 0, time.UTC)
	deps := withLiveTestSocketStubs(stubDeps(now))
	killed := false
	deps.ListLiveTestSockets = func() ([]LiveTestSocket, error) {
		return []LiveTestSocket{{
			SocketPath:  "/tmp/fleet-test-AAA.sock",
			SessionName: "fleet-orphan",
			OwnerPID:    0, // no live go test parent
		}}, nil
	}
	deps.KillTmuxServer = func(string) error { killed = true; return nil }

	got, err := Reconcile(Options{Kinds: []Kind{KindOrphanTmux}}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	a, ok := findAction(got, KindOrphanTmux, "fleet-orphan")
	if !ok {
		t.Fatalf("expected orphan-tmux action for fleet-orphan, got %+v", got.Actions)
	}
	if a.Verb != VerbSurface {
		t.Errorf("verb = %q, want %q (dry-run surfaces, never kills)", a.Verb, VerbSurface)
	}
	if !strings.Contains(a.Reason, "/tmp/fleet-test-AAA.sock") {
		t.Errorf("reason missing socket path: %q", a.Reason)
	}
	if !strings.Contains(a.Reason, "--aggressive") {
		t.Errorf("reason should point at --aggressive escape hatch: %q", a.Reason)
	}
	if killed {
		t.Error("KillTmuxServer ran during dry-run; must not mutate")
	}
}

// T2 — --apply --aggressive kills the orphan test-sock tmux server.
func TestReconcile_LiveTestSock_ApplyAggressiveKills(t *testing.T) {
	now := time.Date(2026, 5, 29, 12, 0, 0, 0, time.UTC)
	deps := withLiveTestSocketStubs(stubDeps(now))
	var killedPath string
	deps.ListLiveTestSockets = func() ([]LiveTestSocket, error) {
		return []LiveTestSocket{{
			SocketPath:  "/tmp/fleet-test-AAA.sock",
			SessionName: "fleet-orphan",
			OwnerPID:    0,
		}}, nil
	}
	deps.KillTmuxServer = func(p string) error { killedPath = p; return nil }

	got, err := Reconcile(Options{Apply: true, Aggressive: true, Kinds: []Kind{KindOrphanTmux}}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	a, ok := findAction(got, KindOrphanTmux, "fleet-orphan")
	if !ok || a.Verb != VerbKilled {
		t.Fatalf("verb = %q (ok=%v), want %q", a.Verb, ok, VerbKilled)
	}
	if killedPath != "/tmp/fleet-test-AAA.sock" {
		t.Errorf("KillTmuxServer called with %q, want /tmp/fleet-test-AAA.sock", killedPath)
	}
}

// T3 — --apply WITHOUT --aggressive spares the orphan (surface only).
func TestReconcile_LiveTestSock_ApplyWithoutAggressiveSpares(t *testing.T) {
	now := time.Date(2026, 5, 29, 12, 0, 0, 0, time.UTC)
	deps := withLiveTestSocketStubs(stubDeps(now))
	killed := false
	deps.ListLiveTestSockets = func() ([]LiveTestSocket, error) {
		return []LiveTestSocket{{
			SocketPath:  "/tmp/fleet-test-AAA.sock",
			SessionName: "fleet-orphan",
			OwnerPID:    0,
		}}, nil
	}
	deps.KillTmuxServer = func(string) error { killed = true; return nil }

	got, err := Reconcile(Options{Apply: true, Aggressive: false, Kinds: []Kind{KindOrphanTmux}}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	a, ok := findAction(got, KindOrphanTmux, "fleet-orphan")
	if !ok || a.Verb != VerbSurface {
		t.Fatalf("verb = %q (ok=%v), want %q (surface only without --aggressive)", a.Verb, ok, VerbSurface)
	}
	if killed {
		t.Error("KillTmuxServer ran without --aggressive; surface-don't-silo violated")
	}
}

// T4 — a server still owned by a LIVE go test process is NEVER reaped,
// even under --apply --aggressive.
func TestReconcile_LiveTestSock_LiveOwnerNotReaped(t *testing.T) {
	now := time.Date(2026, 5, 29, 12, 0, 0, 0, time.UTC)
	deps := withLiveTestSocketStubs(stubDeps(now))
	killed := false
	deps.ListLiveTestSockets = func() ([]LiveTestSocket, error) {
		return []LiveTestSocket{{
			SocketPath:  "/tmp/fleet-test-LIVE.sock",
			SessionName: "fleet-live1234",
			OwnerPID:    4242, // live go test parent
		}}, nil
	}
	deps.KillTmuxServer = func(string) error { killed = true; return nil }

	got, err := Reconcile(Options{Apply: true, Aggressive: true, Kinds: []Kind{KindOrphanTmux}}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, ok := findAction(got, KindOrphanTmux, "fleet-live1234"); ok {
		t.Error("live-owned test-sock server was surfaced/reaped; must be left alone")
	}
	if killed {
		t.Error("KillTmuxServer ran against a live-owned server; in-flight test would break")
	}
}

// Owner-probe-failure (sentinel ownerProbeFailedPID) must SPARE the
// server — an ambiguous probe never drives a kill (fail-safe).
func TestReconcile_LiveTestSock_ProbeFailureSpares(t *testing.T) {
	now := time.Date(2026, 5, 29, 12, 0, 0, 0, time.UTC)
	deps := withLiveTestSocketStubs(stubDeps(now))
	killed := false
	deps.ListLiveTestSockets = func() ([]LiveTestSocket, error) {
		return []LiveTestSocket{{
			SocketPath:  "/tmp/fleet-test-AAA.sock",
			SessionName: "fleet-orphan",
			OwnerPID:    ownerProbeFailedPID, // owner unknown
		}}, nil
	}
	deps.KillTmuxServer = func(string) error { killed = true; return nil }

	got, err := Reconcile(Options{Apply: true, Aggressive: true, Kinds: []Kind{KindOrphanTmux}}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, ok := findAction(got, KindOrphanTmux, "fleet-orphan"); ok {
		t.Error("server with unknown owner was reaped; ambiguous probe must spare it")
	}
	if killed {
		t.Error("KillTmuxServer ran despite unknown owner")
	}
}

// T6 — the detector only runs under the orphan-tmux kind, not under
// sockets / other kinds. An orphan test-sock server is invisible when
// orphan-tmux is not requested.
func TestReconcile_LiveTestSock_OnlyUnderOrphanTmuxKind(t *testing.T) {
	now := time.Date(2026, 5, 29, 12, 0, 0, 0, time.UTC)
	deps := withLiveTestSocketStubs(stubDeps(now))
	called := false
	deps.ListLiveTestSockets = func() ([]LiveTestSocket, error) {
		called = true
		return []LiveTestSocket{{
			SocketPath:  "/tmp/fleet-test-AAA.sock",
			SessionName: "fleet-orphan",
			OwnerPID:    0,
		}}, nil
	}

	got, err := Reconcile(Options{Kinds: []Kind{KindSockets}}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if called {
		t.Error("ListLiveTestSockets ran under sockets kind; should be gated on orphan-tmux")
	}
	if _, ok := findAction(got, KindOrphanTmux, "fleet-orphan"); ok {
		t.Error("orphan-tmux action emitted when only sockets kind requested")
	}
}

// Lister/listing-error surfaces (don't silently swallow a read failure).
func TestReconcile_LiveTestSock_ListErrorSurfaced(t *testing.T) {
	now := time.Date(2026, 5, 29, 12, 0, 0, 0, time.UTC)
	deps := withLiveTestSocketStubs(stubDeps(now))
	deps.ListLiveTestSockets = func() ([]LiveTestSocket, error) {
		return nil, errors.New("boom")
	}
	_, err := Reconcile(Options{Kinds: []Kind{KindOrphanTmux}}, deps)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected list error surfaced via Reconcile, got %v", err)
	}
}

// Kill-failure is reported on the action (verb stays surface, reason
// carries the error) — no false "killed" report.
func TestReconcile_LiveTestSock_KillFailureReported(t *testing.T) {
	now := time.Date(2026, 5, 29, 12, 0, 0, 0, time.UTC)
	deps := withLiveTestSocketStubs(stubDeps(now))
	deps.ListLiveTestSockets = func() ([]LiveTestSocket, error) {
		return []LiveTestSocket{{
			SocketPath:  "/tmp/fleet-test-AAA.sock",
			SessionName: "fleet-orphan",
			OwnerPID:    0,
		}}, nil
	}
	deps.KillTmuxServer = func(string) error { return errors.New("kill exploded") }

	got, err := Reconcile(Options{Apply: true, Aggressive: true, Kinds: []Kind{KindOrphanTmux}}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	a, ok := findAction(got, KindOrphanTmux, "fleet-orphan")
	if !ok {
		t.Fatalf("expected action for fleet-orphan")
	}
	if a.Verb == VerbKilled {
		t.Error("verb=killed reported despite kill failure")
	}
	if !strings.Contains(a.Reason, "kill exploded") {
		t.Errorf("kill error not surfaced in reason: %q", a.Reason)
	}
}

// firstFleetSession must reject a path that is NOT a real Unix socket —
// a regular file or a symlink in world-writable /tmp could otherwise be
// followed into the operator's default tmux server and killed under
// --apply --aggressive (codex iter-2 [P2] symlink guard).
func TestFirstFleetSession_RejectsNonSocket(t *testing.T) {
	dir := t.TempDir()

	// Regular file named like a test sock — not a socket → reject.
	regular := filepath.Join(dir, "fleet-test-regular.sock")
	if err := os.WriteFile(regular, []byte("not a socket"), 0o600); err != nil {
		t.Fatalf("write regular file: %v", err)
	}
	if _, ok := firstFleetSession(regular); ok {
		t.Error("firstFleetSession accepted a regular file; symlink/non-socket guard bypassed")
	}

	// Symlink named like a test sock, pointing anywhere → reject without
	// dereferencing (the target could be the operator's default server).
	link := filepath.Join(dir, "fleet-test-link.sock")
	if err := os.Symlink("/tmp/some-other-tmux.sock", link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, ok := firstFleetSession(link); ok {
		t.Error("firstFleetSession followed a symlink; could kill the operator's default server")
	}

	// Missing path → reject (no panic).
	if _, ok := firstFleetSession(filepath.Join(dir, "absent.sock")); ok {
		t.Error("firstFleetSession accepted a missing path")
	}
}

// killTmuxServerOnDisk must be a no-op (and never remove the path) when
// handed a non-socket — defense-in-depth for the symlink guard so a path
// that became a symlink between enumeration and apply is not kill-server'd
// or unlinked (codex iter-2 [P2]).
func TestKillTmuxServerOnDisk_NonSocketNoOp(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "fleet-test-link.sock")
	if err := os.Symlink("/tmp/some-other-tmux.sock", link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := killTmuxServerOnDisk(link); err != nil {
		t.Errorf("killTmuxServerOnDisk on a symlink returned err %v; want nil no-op", err)
	}
	// The symlink itself must NOT be removed (we never touch non-sockets).
	if _, err := os.Lstat(link); err != nil {
		t.Errorf("killTmuxServerOnDisk removed the symlink; want it left intact: %v", err)
	}
}

// psCanSeeSelfTestExe reports whether the REAL (un-stubbed) production
// lister observes this running process's own `*.test` / `go test`
// executable. Some sandboxes restrict `ps` cross-process visibility, so
// the host-wide gate is unobservable there and dependent assertions skip
// rather than report a false failure. On a real operator host ps sees the
// live test binary and the assertions run for real.
func psCanSeeSelfTestExe(t *testing.T) bool {
	t.Helper()
	procs, err := psProcLister()
	if err != nil {
		return false
	}
	for _, p := range procs {
		if isGoTestProc(p) {
			return true
		}
	}
	return false
}

// withProcLister swaps the package-level goTestProcLister seam for the
// duration of one test and restores it via t.Cleanup. Pass the fake
// process table the test wants anyGoTestRunning to see.
func withProcLister(t *testing.T, procs []procInfo, err error) {
	t.Helper()
	prev := goTestProcLister
	goTestProcLister = func() ([]procInfo, error) { return procs, err }
	t.Cleanup(func() { goTestProcLister = prev })
}

// isGoTestProc is the load-bearing classifier behind the 2026-06-13
// deadlock fix: the detector must match the real test PARENT by its
// EXECUTABLE, never any process that merely references a `.test` path in
// a later argv token (the leaked tmux server's `-e FLEET_BIN=...spawn.test`
// is the exact false positive that pinned the gate true and spared 199
// orphans forever).
func TestIsGoTestProc_ExecutableOnly(t *testing.T) {
	live := []procInfo{
		// Compiled pkg.test binary (argv[0] basename ends `.test`).
		{Args: "/var/.../go-build/b168/spawn.test -test.v"},
		{Args: "/tmp/go-build/gc.test -test.run=Foo"},
		// macOS ps may paren-wrap argv[0] for the live process — must still
		// classify (the 2026-06-13 sibling-kill bug exposed comm truncation;
		// argv[0] with parens is the other ps quirk to tolerate).
		{Args: "(spawn.test) -test.v"},
		// Full go-build temp path (the form `go test ./...` actually runs) —
		// proves we read argv[0] not the truncated comm.
		{Args: "/var/folders/69/xyz/T/go-build523005417/b001/handoffop.test -test.paniconexit0 -test.timeout=5m0s"},
		// `go` driver running the `test` subcommand.
		{Args: "go test ./..."},
		{Args: "go -C /repo test -race ./internal/gc"}, // leading flags skipped
	}
	for _, p := range live {
		if !isGoTestProc(p) {
			t.Errorf("isGoTestProc(%+v) = false; a real go-test parent must count (else live in-flight servers get reaped)", p)
		}
	}

	notLive := []procInfo{
		// THE DEADLOCK: a leaked tmux server whose argv carries
		// `-e FLEET_BIN=...spawn.test` but whose argv[0] is tmux.
		{Args: "tmux -S /tmp/fleet-test-XX.sock new-session -d -s fleet-abc -e FLEET_BIN=/var/.../go-build/b168/spawn.test sh -c cat"},
		// A shell child of the leaked server, same story.
		{Args: "sh -c echo FLEET_BIN=/var/.../spawn.test; cat"},
		// `go build` / `go vet` are NOT a test run.
		{Args: "go build ./..."},
		{Args: "go vet ./internal/gc"},
		// Unrelated binaries that merely look testy.
		{Args: "pytest -q tests/"},
		{Args: "/usr/bin/latest --foo"},
		{Args: "contest --run"},
		{Args: "/opt/mytestrunner"},
		// A process that holds a `.test` path in env but is not a test.
		{Args: "cat"},
		// Empty argv (defensive — no panic, not a test).
		{Args: ""},
	}
	for _, p := range notLive {
		if isGoTestProc(p) {
			t.Errorf("isGoTestProc(%+v) = true; only the real test executable may count — this would pin the gate and silently defeat the reaper", p)
		}
	}
}

// anyGoTestRunning must return false when the ONLY processes referencing
// `.test` are leaked tmux servers (their executable is tmux) — this is
// the regression that left `fleet gc --apply --aggressive` reaping 0 of
// 199 orphans. With the executable-based detector the host is correctly
// classified quiescent, so the orphan is reapable.
func TestAnyGoTestRunning_OrphansDoNotPinGate(t *testing.T) {
	withProcLister(t, []procInfo{
		{Args: "tmux -S /tmp/fleet-test-XX.sock new-session -d -e FLEET_BIN=/var/.../spawn.test sh -c cat"},
		{Args: "sh -c cat"},
		{Args: "cat"},
	}, nil)
	if anyGoTestRunning() {
		t.Fatal("anyGoTestRunning() = true with only leaked tmux orphans present — the gate is still deadlocked; orphans would never be reaped")
	}
	if v := goTestOwnerVerdict(); v != 0 {
		t.Errorf("goTestOwnerVerdict() = %d; want 0 (host is quiescent) so the orphan classifies reapable", v)
	}
}

// anyGoTestRunning must return true when a REAL `*.test` / `go test`
// parent is alive — the in-flight-test spare must survive the fix.
func TestAnyGoTestRunning_SparesRealTestParent(t *testing.T) {
	withProcLister(t, []procInfo{
		{Args: "tmux -S /tmp/fleet-test-XX.sock new-session -d sh -c cat"},
		{Args: "/var/.../go-build/b168/spawn.test -test.v"},
	}, nil)
	if !anyGoTestRunning() {
		t.Fatal("anyGoTestRunning() = false with a live *.test parent — live in-flight servers would be reaped")
	}
	if v := goTestOwnerVerdict(); v != ownerProbeFailedPID {
		t.Errorf("goTestOwnerVerdict() = %d while a real test runs; want spare sentinel %d", v, ownerProbeFailedPID)
	}
}

// Fail-safe: if the process lister errors (cannot read the process
// table), the host's quiescence is unprovable, so anyGoTestRunning
// returns true (spare) and the verdict is the unknown sentinel.
func TestAnyGoTestRunning_ListerErrorSpares(t *testing.T) {
	withProcLister(t, nil, errors.New("ps exploded"))
	if !anyGoTestRunning() {
		t.Fatal("anyGoTestRunning() = false on lister error — must fail safe to spare")
	}
	if v := goTestOwnerVerdict(); v != ownerProbeFailedPID {
		t.Errorf("goTestOwnerVerdict() = %d on probe failure; want spare sentinel %d", v, ownerProbeFailedPID)
	}
}

// tmuxServerSocketFromArgv must extract tmux's OWN -S socket and NEVER a
// -S that appears inside a pane command / env value (codex iter-1 [P2]:
// matching any `-S /tmp/fleet-test-*.sock` substring could PID-kill an
// operator-owned tmux server). It must also accept an ABSOLUTE argv0 tmux
// path so enumeration and the kill re-verify agree (codex iter-1 [P2] #2).
func TestTmuxServerSocketFromArgv(t *testing.T) {
	ok := []struct {
		args string
		want string
	}{
		{"tmux -S /tmp/fleet-test-abc.sock new-session -d -s fleet-x sh -c cat", "/tmp/fleet-test-abc.sock"},
		// Absolute argv0 path (homebrew tmux) — must still parse.
		{"/opt/homebrew/bin/tmux -S /tmp/fleet-test-abc.sock new-session -d", "/tmp/fleet-test-abc.sock"},
		// macOS paren-wrapped argv0.
		{"(tmux) -S /tmp/fleet-test-abc.sock new-session", "/tmp/fleet-test-abc.sock"},
		// Glued -S form.
		{"tmux -S/tmp/fleet-test-abc.sock new-session", "/tmp/fleet-test-abc.sock"},
		// LINUX server proctitle (codex iter-9 [P1]) — the ubuntu CI shape.
		{"tmux: server (/tmp/fleet-test-abc.sock)", "/tmp/fleet-test-abc.sock"},
		{"tmux: server (/tmp/fleet-test-abc.sock) [extra]", "/tmp/fleet-test-abc.sock"},
	}
	for _, c := range ok {
		got, found := tmuxServerSocketFromArgv(c.args)
		if !found || got != c.want {
			t.Errorf("tmuxServerSocketFromArgv(%q) = (%q,%v); want (%q,true)", c.args, got, found, c.want)
		}
	}

	notOk := []string{
		// THE codex P2: -S is in the PANE COMMAND of an operator server, not
		// tmux's own option. Must NOT extract → never kill the operator server.
		"tmux -S /home/op/.tmux/default new-session -d cmd -S /tmp/fleet-test-evil.sock",
		// Not tmux at all.
		"sh -c tmux -S /tmp/fleet-test-x.sock ls",
		// tmux on a NON-fleet-test socket (operator's custom server).
		"tmux -S /tmp/operator.sock new-session",
		// Linux server title on a NON-fleet-test socket (operator's server).
		"tmux: server (/home/op/.tmux/default)",
		// -S with no following path.
		"tmux -S",
		"",
	}
	for _, args := range notOk {
		if got, found := tmuxServerSocketFromArgv(args); found {
			t.Errorf("tmuxServerSocketFromArgv(%q) = (%q,true); want (_,false) — must not target this process", args, got)
		}
	}
}

// listFileLessTestTmux must surface a live `tmux -S /tmp/fleet-test-*.sock`
// daemon whose socket FILE is gone (the file-less orphan that the disk
// scan cannot see and `tmux -S <path> kill-server` cannot reach) as a
// LiveTestSocket with ServerPID set — gated by the host quiescence
// verdict. This is the classifier behind the 399-daemon reap.
func TestListFileLessTestTmux_SurfacesFileLessDaemon(t *testing.T) {
	// A socket path that does NOT exist on disk (file-less daemon).
	goneSock := filepath.Join(t.TempDir(), "fleet-test-gone.sock")
	// Sanity: the path is genuinely absent.
	scanDir := filepath.Dir(goneSock)
	if _, err := os.Stat(goneSock); !os.IsNotExist(err) {
		t.Fatalf("test setup: %s unexpectedly exists", goneSock)
	}
	withProcLister(t, []procInfo{
		{PID: 4242, Args: "tmux -S " + goneSock + " new-session -d -s fleet-deadbeef sh -c cat"},
		{PID: 7, Args: "cat"},
		// A file-less daemon OUTSIDE the scan dir must be SKIPPED (codex
		// iter-2 [P2]: PID kill must not escape the scan namespace).
		{PID: 8, Args: "tmux -S /some/other/dir/fleet-test-elsewhere.sock new-session"},
	}, nil)

	got := listFileLessTestTmux(scanDir, 0) // 0 = host quiescent
	if len(got) != 1 {
		t.Fatalf("listFileLessTestTmux returned %d entries, want 1 (the out-of-scan-dir daemon must be skipped): %+v", len(got), got)
	}
	if got[0].ServerPID != 4242 {
		t.Errorf("ServerPID = %d, want 4242 (file-less daemon must be killed by PID)", got[0].ServerPID)
	}
	if got[0].SocketPath != goneSock {
		t.Errorf("SocketPath = %q, want %q", got[0].SocketPath, goneSock)
	}
	if got[0].OwnerPID != 0 {
		t.Errorf("OwnerPID = %d, want 0 (host quiescent → reapable)", got[0].OwnerPID)
	}
}

// listFileLessTestTmux must SKIP a daemon whose socket file STILL exists
// (the disk scan owns that one — returning it here would double-count) and
// must SKIP non-tmux processes and tmux servers on non-fleet-test sockets.
func TestListFileLessTestTmux_SkipsFileBoundAndUnrelated(t *testing.T) {
	// A socket file that DOES exist → owned by the disk scan, skip here.
	dir := t.TempDir()
	presentSock := filepath.Join(dir, "fleet-test-present.sock")
	if err := os.WriteFile(presentSock, []byte{}, 0o600); err != nil {
		t.Fatalf("seed present sock: %v", err)
	}
	withProcLister(t, []procInfo{
		{PID: 1, Args: "tmux -S " + presentSock + " new-session -d -s fleet-x sh -c cat"},
		{PID: 2, Args: "tmux -S /tmp/some-operator.sock new-session -d -s work"}, // not fleet-test
		{PID: 3, Args: "sh -c echo tmux -S /tmp/fleet-test-fake.sock"},           // not a tmux exe
	}, nil)

	got := listFileLessTestTmux(dir, 0)
	if len(got) != 0 {
		t.Fatalf("listFileLessTestTmux returned %d entries, want 0 (file-bound + unrelated must be skipped): %+v", len(got), got)
	}
}

// reconcileLiveTestSockets must kill a file-less daemon via KillTmuxProc
// (PID), NOT KillTmuxServer (socket path) — the daemon is unreachable by
// path. Under --apply --aggressive with a quiescent host (OwnerPID==0).
func TestReconcile_LiveTestSock_FileLessKilledByPID(t *testing.T) {
	now := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	deps := withLiveTestSocketStubs(stubDeps(now))
	var killedPID int
	pathKillCalled := false
	deps.ListLiveTestSockets = func() ([]LiveTestSocket, error) {
		return []LiveTestSocket{{
			SocketPath: "/tmp/fleet-test-gone.sock",
			OwnerPID:   0,
			ServerPID:  9931, // file-less daemon
		}}, nil
	}
	var killedSock string
	deps.KillTmuxProc = func(pid int, sock string) error { killedPID = pid; killedSock = sock; return nil }
	deps.KillTmuxServer = func(string) error { pathKillCalled = true; return nil }

	got, err := Reconcile(Options{Apply: true, Aggressive: true, Kinds: []Kind{KindOrphanTmux}}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	a, ok := findAction(got, KindOrphanTmux, "/tmp/fleet-test-gone.sock")
	if !ok || a.Verb != VerbKilled {
		t.Fatalf("verb = %q (ok=%v), want %q", a.Verb, ok, VerbKilled)
	}
	if killedPID != 9931 {
		t.Errorf("KillTmuxProc called with pid %d, want 9931", killedPID)
	}
	if killedSock != "/tmp/fleet-test-gone.sock" {
		t.Errorf("KillTmuxProc called with expectSock %q, want /tmp/fleet-test-gone.sock (PID-reuse re-verify must pin the exact socket)", killedSock)
	}
	if pathKillCalled {
		t.Error("KillTmuxServer (socket-path kill) ran for a file-less daemon; must kill by PID")
	}
	// The action must carry a SAFE CleanupHint so surface consumers
	// (dispatch/status) point at the PID-reuse-safe `fleet gc` reap, not a
	// bogus `tmux kill-session -t /tmp/fleet-test-gone.sock` NOR a raw
	// `kill <pid>` that could race PID reuse (codex iter-2/5 [P2]).
	if !strings.Contains(a.CleanupHint, "fleet gc --apply --aggressive") {
		t.Errorf("CleanupHint = %q, want a `fleet gc --apply --aggressive` reap hint (file-less orphan: session-kill is bogus, raw kill races PID reuse)", a.CleanupHint)
	}
}

// A file-less daemon with KillTmuxProc unwired must surface (not falsely
// report killed) so the operator sees it needs the seam.
func TestReconcile_LiveTestSock_FileLessUnwiredSurfaces(t *testing.T) {
	now := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	deps := withLiveTestSocketStubs(stubDeps(now))
	deps.ListLiveTestSockets = func() ([]LiveTestSocket, error) {
		return []LiveTestSocket{{
			SocketPath: "/tmp/fleet-test-gone.sock",
			OwnerPID:   0,
			ServerPID:  9931,
		}}, nil
	}
	deps.KillTmuxProc = nil // seam unwired

	got, err := Reconcile(Options{Apply: true, Aggressive: true, Kinds: []Kind{KindOrphanTmux}}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	a, ok := findAction(got, KindOrphanTmux, "/tmp/fleet-test-gone.sock")
	if !ok {
		t.Fatalf("expected action for file-less daemon")
	}
	if a.Verb == VerbKilled {
		t.Error("verb=killed reported despite unwired KillTmuxProc seam")
	}
	if !strings.Contains(a.Reason, "kill seam unwired") {
		t.Errorf("reason should flag the unwired seam: %q", a.Reason)
	}
}

// killTmuxProcByPID on a definitely-dead PID is a no-op success (the daemon
// is already gone — `ps -p` exits 1 empty). It must NOT error, since the
// caller treats nil as "reaped" and a gone daemon IS effectively reaped.
func TestKillTmuxProcByPID_GonePidIsNoopSuccess(t *testing.T) {
	if _, err := exec.LookPath("ps"); err != nil {
		t.Skip("ps unavailable")
	}
	// Spawn a trivial process and reap it so its PID is definitively dead.
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("seed true: %v", err)
	}
	deadPID := cmd.Process.Pid
	if err := killTmuxProcByPID(deadPID, "/tmp/fleet-test-x.sock"); err != nil {
		t.Errorf("killTmuxProcByPID(dead pid) = %v; want nil (gone == already reaped)", err)
	}
}

// killTmuxProcByPID must REFUSE (error, not nil) when the live PID is not the
// expected daemon — a recycled PID must never be killed, and the caller must
// not report VerbKilled. We point it at our OWN test PID (alive, but not a
// fleet-test tmux daemon on expectSock).
func TestKillTmuxProcByPID_WrongIdentityRefuses(t *testing.T) {
	if _, err := exec.LookPath("ps"); err != nil {
		t.Skip("ps unavailable")
	}
	err := killTmuxProcByPID(os.Getpid(), "/tmp/fleet-test-x.sock")
	if err == nil {
		t.Fatal("killTmuxProcByPID against a non-daemon PID returned nil; must refuse (recycled-PID guard) so the action is not reported killed")
	}
	if !strings.Contains(err.Error(), "refusing to kill") {
		t.Errorf("error = %q; want a 'refusing to kill' recycled-PID refusal", err)
	}
}

// goTestOwnerVerdict must report SPARE (the unknown sentinel) whenever a
// `go test` is alive on the host — and this test IS running under
// `go test`, so the REAL (un-stubbed) production lister must see this
// process's own `*.test` executable. This is the production-side proof
// that the executable-based detector still observes a live test on a
// real host (vs the stubbed unit tests above).
func TestGoTestOwnerVerdict_SparesWhileTestRunning(t *testing.T) {
	procs, err := psProcLister()
	if err != nil {
		t.Skipf("ps unavailable in this sandbox: %v", err)
	}
	sawSelf := false
	for _, p := range procs {
		if isGoTestProc(p) {
			sawSelf = true
			break
		}
	}
	if !sawSelf {
		t.Skip("ps cannot enumerate this process's *.test executable (sandbox); host-wide gate unobservable here")
	}
	if !anyGoTestRunning() {
		t.Fatal("anyGoTestRunning() = false while running under `go test` — the executable detector missed the test binary")
	}
	if v := goTestOwnerVerdict(); v != ownerProbeFailedPID {
		t.Errorf("goTestOwnerVerdict() = %d while a go test runs; want spare sentinel %d (a 0 verdict would reap live test servers)", v, ownerProbeFailedPID)
	}
}

// Integration: the production lister enumerates a REAL tmux server on a
// /tmp/fleet-test-*.sock. Because the test itself runs under `go test`,
// the host is NOT quiescent, so the lister MUST mark the server spared
// (OwnerPID == ownerProbeFailedPID) — this is the live-in-flight-spare
// guarantee that closes codex iter-1 [P1] at the production-helper level
// (the unit T4 only covers the stubbed-classifier branch). The killer is
// then exercised directly to confirm it removes the server.
func TestLiveTestSockets_Integration_ListAndKill(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed; skipping integration test")
	}
	// codex iter-6 [P1]: scan a SHORT ISOLATED /tmp subdir, not bare /tmp. The
	// old form scanned bare /tmp, so on a dirty runner with many stale
	// /tmp/fleet-test-*.sock files firstFleetSession would `tmux -S` probe
	// EVERY one before filtering to this test's fixture — the exact
	// dirty-runner grind this PR removes. We bind the test's real tmux server
	// into our own short subdir and scan only that dir; the production /tmp
	// default stays covered by the TestGCScanDir_DefaultIsTmp unit check.
	//
	// The subdir + socket name are short (ft-<8hex>/s.sock ≈ 20 chars under
	// /tmp) so the bound socket stays well under the macOS ~104-byte limit.
	scanDir, err := os.MkdirTemp("/tmp", "fleet-test-ltsi-")
	if err != nil {
		t.Fatalf("mkdirtemp /tmp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(scanDir) })
	// Socket name must carry the fleet-test-*.sock pattern scanSocketsDir
	// matches; kept short so the bound path stays under the macOS limit.
	sock := filepath.Join(scanDir, "fleet-test-s.sock")
	// Spawn a real detached server on the isolated socket with a fleet-<id>
	// session name. Reap the server in cleanup (RemoveAll alone leaves the
	// daemon running on the now-deleted socket inode).
	if out, err := exec.Command("tmux", "-S", sock, "new-session", "-d",
		"-s", "fleet-deadbeef", "sleep", "300").CombinedOutput(); err != nil {
		t.Fatalf("spawn tmux: %v (%s)", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("tmux", "-S", sock, "kill-server").Run() })
	// codex iter-1 [P2]: `tmux new-session` can print "error creating ..."
	// yet exit 0 on some hosts, so a clean exit is NOT proof the server
	// exists. Verify the session is actually live before asserting the
	// lister sees it — otherwise the failure surfaces downstream as a
	// confusing "lister did not see live server" instead of a spawn skip.
	if err := exec.Command("tmux", "-S", sock, "has-session", "-t", "fleet-deadbeef").Run(); err != nil {
		t.Skipf("tmux exited 0 but no live session on %s (host tmux quirk): %v", sock, err)
	}

	socks, err := listLiveTestSocketsOnDisk(scanDir)
	if err != nil {
		t.Fatalf("listLiveTestSocketsOnDisk: %v", err)
	}
	var found *LiveTestSocket
	for i := range socks {
		if socks[i].SocketPath == sock {
			found = &socks[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("lister did not see live server on %s; got %+v", sock, socks)
	}
	if found.SessionName != "fleet-deadbeef" {
		t.Errorf("session name = %q, want fleet-deadbeef", found.SessionName)
	}
	// codex iter-1 [P1] regression: the running test IS a `go test`, so
	// the host is non-quiescent and the production lister must SPARE the
	// server (never report OwnerPID==0 mid-run). A regression to the old
	// lsof-on-socket logic would report OwnerPID==0 here and the kill path
	// would reap a live test server. Only assertable where `ps` can see
	// this process's *.test executable (skipped in sandboxes that neuter ps).
	if psCanSeeSelfTestExe(t) && found.OwnerPID == 0 {
		t.Errorf("OwnerPID=0 while a go test is running — live-in-flight server would be reaped (lsof-on-socket regression); want spare sentinel %d", ownerProbeFailedPID)
	}

	// Kill via the production killer and confirm the server is gone.
	if err := killTmuxServerOnDisk(sock); err != nil {
		t.Fatalf("killTmuxServerOnDisk: %v", err)
	}
	if err := exec.Command("tmux", "-S", sock, "list-sessions").Run(); err == nil {
		t.Error("server still alive after killTmuxServerOnDisk")
	}
}
