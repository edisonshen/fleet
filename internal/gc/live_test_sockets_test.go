package gc

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/testutil/tmuxtest"
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

// Integration: the production lister enumerates a REAL orphan tmux
// server on a /tmp/fleet-test-*.sock (no live go test owner from gc's
// perspective is impossible inside a test — the running `go test` IS the
// owner — so this test only verifies the lister SEES the server and the
// killer removes it). The live-owner skip is covered by the unit T4.
func TestLiveTestSockets_Integration_ListAndKill(t *testing.T) {
	tmuxtest.RequireTmux(t)
	sock := "/tmp/fleet-test-gcint.sock"
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-S", sock, "kill-server").Run()
		_ = os.Remove(sock)
	})
	// Spawn a real detached server on the custom socket with a fleet-<id>
	// session name.
	if out, err := exec.Command("tmux", "-S", sock, "new-session", "-d",
		"-s", "fleet-deadbeef", "sleep", "300").CombinedOutput(); err != nil {
		t.Fatalf("spawn tmux: %v (%s)", err, out)
	}

	socks, err := listLiveTestSocketsOnDisk()
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

	// Kill via the production killer and confirm the server is gone.
	if err := killTmuxServerOnDisk(sock); err != nil {
		t.Fatalf("killTmuxServerOnDisk: %v", err)
	}
	if err := exec.Command("tmux", "-S", sock, "list-sessions").Run(); err == nil {
		t.Error("server still alive after killTmuxServerOnDisk")
	}
}
