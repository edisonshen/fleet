// Tests for the `fleet coord-run` subcommand wrapper that owns the
// coord lifecycle's signal + clean-exit + panic paths. See
// docs/DESIGN-cleanup-fleet-owns-resources.md §PR-C.
//
// The subcommand is a thin Go wrapper around the child process
// (claude in production; `sleep` / `true` in tests). It registers a
// SIGTERM/SIGINT signal handler that cancels the child's context, has
// a top-level `defer coord.Cleanup(...)` so cleanup fires regardless of
// exit reason (clean exit, signal, panic), and forwards the child's
// exit code.
//
// Tests in this file exercise the signal + clean-exit paths end-to-end
// against the cmd/fleet wrapper. The internal/coord package owns the
// Cleanup function's own per-step unit coverage.
package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/state"
)

// coordRunTestHome bootstraps an isolated FLEET_HOME + writes a live
// agent record + a marker pointing at the agent. Returns the agent ID
// + project name + marker path so the test can assert post-cleanup
// state.
func coordRunTestHome(t *testing.T, agentID, project, session string) (markerPath string) {
	t.Helper()
	t.Setenv("FLEET_HOME", t.TempDir())
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	rec := agent.New(agentID)
	rec.TmuxSession = session
	rec.Project = project
	if err := rec.Write(); err != nil {
		t.Fatalf("write agent record: %v", err)
	}
	if _, err := state.EnsureProjectInitialized(project); err != nil {
		t.Fatalf("ensure project: %v", err)
	}
	if err := state.WriteCoordSpawnMarker(project, agentID); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	path, err := state.CoordSpawnMarkerPath(project)
	if err != nil {
		t.Fatalf("marker path: %v", err)
	}
	return path
}

func assertCleanupRan(t *testing.T, agentID, markerPath string) {
	t.Helper()
	livePath, err := state.AgentPath(agentID)
	if err != nil {
		t.Fatalf("AgentPath: %v", err)
	}
	if _, err := os.Stat(livePath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("live record still exists at %s (err=%v)", livePath, err)
	}
	archivePath, err := state.AgentArchivePath(agentID)
	if err != nil {
		t.Fatalf("AgentArchivePath: %v", err)
	}
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(archivePath), agentID+"-*.json"))
	if len(matches) != 1 {
		t.Errorf("archive glob = %v, want exactly one UTC-suffixed file", matches)
	}
	if _, err := os.Stat(markerPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("marker still exists at %s", markerPath)
	}
}

// TestCoordRun_CleanExitPath: the child process exits cleanly (true ⇒
// exit 0). runCoordRun must run cleanup after wait returns and return
// nil. Same side-effects as a direct coord.Cleanup call.
func TestCoordRun_CleanExitPath(t *testing.T) {
	const (
		agentID = "exit1234"
		project = "exit-test"
		session = "fleet-coord-exit1234-exit-test"
	)
	markerPath := coordRunTestHome(t, agentID, project, session)

	opts := coordRunOpts{
		agentID: agentID,
		project: project,
		session: session,
		argv:    []string{"true"},
		// Skip the real tmux kill in unit tests so we don't depend on
		// tmux being present + don't risk killing a real session.
		killTmux: func(string) error { return nil },
	}
	if err := runCoordRun(context.Background(), opts, os.Stdout, os.Stderr); err != nil {
		t.Fatalf("runCoordRun: %v", err)
	}
	assertCleanupRan(t, agentID, markerPath)
}

// TestCoordRun_SignalReapsTmuxAndArchivesRecord: parent receives
// SIGTERM (simulated by ctx cancel which is the same code path —
// signal.NotifyContext just wires OS signals to ctx.Done). The child
// (`sleep 30`) gets killed via the cancelled context; cleanup still
// runs after wait. This pins the load-bearing failure-path: the OS
// SIGTERM'd the parent and cleanup STILL fired.
func TestCoordRun_SignalReapsTmuxAndArchivesRecord(t *testing.T) {
	const (
		agentID = "sig12345"
		project = "sig-test"
		session = "fleet-coord-sig12345-sig-test"
	)
	markerPath := coordRunTestHome(t, agentID, project, session)

	ctx, cancel := context.WithCancel(context.Background())
	opts := coordRunOpts{
		agentID:  agentID,
		project:  project,
		session:  session,
		argv:     []string{"sleep", "30"},
		killTmux: func(string) error { return nil },
	}
	done := make(chan error, 1)
	go func() {
		done <- runCoordRun(ctx, opts, os.Stdout, os.Stderr)
	}()
	// Give the child a moment to start, then cancel — simulates SIGTERM
	// arriving at the parent. We don't t.Sleep-assert timing; we cancel
	// once and wait for the result channel with a generous bound.
	// (Per CLAUDE.md §8 we avoid sleep-based ASSERTIONS; a one-shot
	// cancel + bounded wait isn't a timing assertion.)
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		// Non-nil error is fine (signal-killed child returns non-zero) —
		// what we care about is that cleanup ran.
		_ = err
	case <-time.After(10 * time.Second):
		t.Fatal("runCoordRun did not return within 10s of cancel")
	}
	assertCleanupRan(t, agentID, markerPath)
}

// TestCoordRun_PanicViaDefer: an inner step of runCoordRun panics
// (simulated by a child argv that points at a binary we know panics —
// in tests we inject via the test-only `panicAfterStart` hook). The
// top-level `defer Cleanup` must still fire.
//
// We test this by setting a hook the implementation honors only when
// it's set non-nil. Production has no way to set the hook; the hook
// exists for this test exclusively.
func TestCoordRun_PanicViaDefer(t *testing.T) {
	const (
		agentID = "pan12345"
		project = "pan-test"
		session = "fleet-coord-pan12345-pan-test"
	)
	markerPath := coordRunTestHome(t, agentID, project, session)

	opts := coordRunOpts{
		agentID:  agentID,
		project:  project,
		session:  session,
		argv:     []string{"true"},
		killTmux: func(string) error { return nil },
		// Force a panic between child-start and child-wait so we
		// exercise the defer-cleanup path. The implementation honors
		// this hook for tests only.
		panicAfterStart: true,
	}
	// runCoordRun must catch the panic via defer, run cleanup, and
	// re-panic so the operator sees the stack (we don't want silent
	// panic swallowing in production). The test recovers here so the
	// process doesn't crash — but it asserts cleanup ran AFTER the
	// panic.
	defer func() {
		_ = recover()
		assertCleanupRan(t, agentID, markerPath)
	}()
	_ = runCoordRun(context.Background(), opts, os.Stdout, os.Stderr)
	t.Fatal("expected panic from panicAfterStart hook")
}

// TestCoordRun_RealOSSignal exercises the actual signal.NotifyContext
// wiring by sending a real SIGTERM to the current process. Race-safe
// because notifyCoordRun installs its handler before the child starts
// and `cancel()` is deferred so the default handler is restored before
// the test returns (or before any concurrent test re-installs its own
// handler).
//
// Skipped if signal-tests are disabled via env (e.g. CI containers
// that translate SIGTERM into immediate kill).
func TestCoordRun_RealOSSignal(t *testing.T) {
	if os.Getenv("FLEET_SKIP_SIGNAL_TESTS") != "" {
		t.Skip("FLEET_SKIP_SIGNAL_TESTS set; skipping real-OS-signal test")
	}
	const (
		agentID = "ossig123"
		project = "ossig-test"
		session = "fleet-coord-ossig123-ossig-test"
	)
	markerPath := coordRunTestHome(t, agentID, project, session)

	opts := coordRunOpts{
		agentID:  agentID,
		project:  project,
		session:  session,
		argv:     []string{"sleep", "30"},
		killTmux: func(string) error { return nil },
	}

	// Use a sync signal so we send SIGTERM only AFTER notifyCoordRun has
	// installed its signal handler — otherwise a race where SIGTERM
	// arrives before NotifyContext is wired up would default-kill the
	// test process and abort the whole suite.
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		// notifyCoordRun calls into a hook (set via the test-only
		// notifyReady field on opts) right after signal.NotifyContext is
		// installed.
		done <- notifyCoordRun(opts.withReady(func() { close(ready) }), os.Stdout, os.Stderr)
	}()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("notifyCoordRun did not signal ready within 5s")
	}
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("kill self with SIGTERM: %v", err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("notifyCoordRun did not return within 10s of SIGTERM")
	}
	assertCleanupRan(t, agentID, markerPath)
}
