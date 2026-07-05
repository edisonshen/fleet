// Lease-wiring tests for `fleet coord-run` (DESIGN-handoff-drain-storm-
// leak PR2). These exercise the supervisor's lease lifecycle through the
// injectable acquireLease seam (no real flock) so they stay
// deterministic:
//
//	W4  AcquireLease is called BEFORE the child starts.
//	T9  acquired==false -> stand down, NEVER start the child, return nil.
//	W5  acquired==true  -> Release + heartbeat-stop run after a clean exit.
//	W6  Release runs on SIGTERM (ctx cancel).
//	W7  Release runs on panic (panicAfterStart hook).
//
// The real-flock end-to-end lifetime (W8 renewed_at advance + W11 hold/
// release) lives in coord_run_lease_integration_test.go.
package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeLease records Heartbeat + Release calls so a test can assert the
// every-exit-path lifecycle without a kernel flock.
type fakeLease struct {
	mu            sync.Mutex
	heartbeatRuns int
	releaseRuns   int
}

func (f *fakeLease) Heartbeat() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.heartbeatRuns++
}

func (f *fakeLease) Release() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releaseRuns++
}

func (f *fakeLease) releases() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.releaseRuns
}

func (f *fakeLease) heartbeats() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.heartbeatRuns
}

// childSentinelArgv returns an argv that touches `path` when the child
// runs, so a test can assert whether the child was started.
func childSentinelArgv(path string) []string {
	return []string{"sh", "-c", "touch " + path}
}

// W4 + W5: acquired==true. The fake acquire records its call order
// relative to the child; the child touches a sentinel. After a clean
// child exit, Release must have run exactly once and the heartbeat must
// have been started — alongside coord.Cleanup.
func TestCoordRun_Lease_AcquireBeforeChild_ReleaseOnCleanExit(t *testing.T) {
	const (
		agentID = "lw4release"
		project = "lw-acq-test"
		session = "fleet-lw4release"
	)
	markerPath := coordRunTestHome(t, agentID, project, session)

	sentinel := filepath.Join(t.TempDir(), "child-ran")
	lease := &fakeLease{}
	var order []string
	var orderMu sync.Mutex
	note := func(s string) { orderMu.Lock(); order = append(order, s); orderMu.Unlock() }

	opts := coordRunOpts{
		agentID:  agentID,
		project:  project,
		session:  session,
		argv:     childSentinelArgv(sentinel),
		killTmux: func(string) error { return nil },
		acquireLease: func() (coordLease, bool, []liveHolderInfo, error) {
			note("acquire")
			return lease, true, nil, nil
		},
	}
	// Wrap child observation: the sentinel proves the child ran; we note
	// acquire above. Run.
	if err := runCoordRun(context.Background(), opts, os.Stdout, os.Stderr); err != nil {
		t.Fatalf("runCoordRun: %v", err)
	}
	// W4: acquire was recorded, and the child sentinel exists -> child
	// ran AFTER acquire (acquire is recorded before exec in runCoordRun).
	orderMu.Lock()
	gotAcquire := len(order) == 1 && order[0] == "acquire"
	orderMu.Unlock()
	if !gotAcquire {
		t.Errorf("expected acquire to be called once; order=%v", order)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Errorf("child sentinel %s missing — child did not run: %v", sentinel, err)
	}
	// W5: Release ran exactly once; heartbeat was started.
	if got := lease.releases(); got != 1 {
		t.Errorf("Release runs = %d, want 1", got)
	}
	if got := lease.heartbeats(); got != 1 {
		t.Errorf("Heartbeat starts = %d, want 1", got)
	}
	// coord.Cleanup also ran (record archived, marker cleared).
	assertCleanupRan(t, agentID, markerPath)
}

// T9: acquired==false. The supervisor stands down — it must NOT start the
// child, must return nil (exit 0), and must NOT register a heartbeat.
func TestCoordRun_Lease_StandDownDoesNotStartChild(t *testing.T) {
	const (
		agentID = "lt9stand"
		project = "lw-standdown"
		session = "fleet-lt9stand"
	)
	_ = coordRunTestHome(t, agentID, project, session)

	sentinel := filepath.Join(t.TempDir(), "child-ran")
	lease := &fakeLease{}
	stoodDown := false
	opts := coordRunOpts{
		agentID:  agentID,
		project:  project,
		session:  session,
		argv:     childSentinelArgv(sentinel),
		killTmux: func(string) error { return nil },
		acquireLease: func() (coordLease, bool, []liveHolderInfo, error) {
			return lease, false, nil, nil // busy: healthy leader exists
		},
		onStandDown: func() { stoodDown = true },
	}
	if err := runCoordRun(context.Background(), opts, os.Stdout, os.Stderr); err != nil {
		t.Fatalf("stand-down must return nil (exit 0), got %v", err)
	}
	if !stoodDown {
		t.Errorf("onStandDown was not invoked")
	}
	if _, err := os.Stat(sentinel); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("child sentinel %s exists — child MUST NOT start on stand-down", sentinel)
	}
	if got := lease.heartbeats(); got != 0 {
		t.Errorf("Heartbeat starts = %d, want 0 (no leadership)", got)
	}
	if got := lease.releases(); got != 0 {
		t.Errorf("Release runs = %d, want 0 (we never acquired)", got)
	}
}

// W6: Release runs on SIGTERM (modeled by ctx cancel — the same code path
// signal.NotifyContext drives). The child (`sleep 30`) is killed via the
// cancelled context; Release + Cleanup must still run.
func TestCoordRun_Lease_ReleaseOnSignal(t *testing.T) {
	const (
		agentID = "lw6signal"
		project = "lw-signal"
		session = "fleet-lw6signal"
	)
	markerPath := coordRunTestHome(t, agentID, project, session)

	lease := &fakeLease{}
	ctx, cancel := context.WithCancel(context.Background())
	opts := coordRunOpts{
		agentID:      agentID,
		project:      project,
		session:      session,
		argv:         []string{"sleep", "30"},
		killTmux:     func(string) error { return nil },
		acquireLease: func() (coordLease, bool, []liveHolderInfo, error) { return lease, true, nil, nil },
	}
	done := make(chan error, 1)
	go func() { done <- runCoordRun(ctx, opts, os.Stdout, os.Stderr) }()
	time.Sleep(50 * time.Millisecond) // let the child start (not a timing assert)
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runCoordRun did not return within 10s of cancel")
	}
	if got := lease.releases(); got != 1 {
		t.Errorf("Release runs on SIGTERM = %d, want 1", got)
	}
	assertCleanupRan(t, agentID, markerPath)
}

// W7: Release runs on panic (panicAfterStart hook fires between
// child-start and Wait). The top-level defer stack must run Release +
// Cleanup before the panic unwinds past runCoordRun.
func TestCoordRun_Lease_ReleaseOnPanic(t *testing.T) {
	const (
		agentID = "lw7panic"
		project = "lw-panic"
		session = "fleet-lw7panic"
	)
	markerPath := coordRunTestHome(t, agentID, project, session)

	lease := &fakeLease{}
	opts := coordRunOpts{
		agentID:         agentID,
		project:         project,
		session:         session,
		argv:            []string{"true"},
		killTmux:        func(string) error { return nil },
		acquireLease:    func() (coordLease, bool, []liveHolderInfo, error) { return lease, true, nil, nil },
		panicAfterStart: true,
	}
	defer func() {
		_ = recover()
		if got := lease.releases(); got != 1 {
			t.Errorf("Release runs on panic = %d, want 1", got)
		}
		assertCleanupRan(t, agentID, markerPath)
	}()
	_ = runCoordRun(context.Background(), opts, os.Stdout, os.Stderr)
	t.Fatal("expected panic from panicAfterStart hook")
}

// A real acquire fault (not the failover-disabled sentinel) must abort —
// runCoordRun refuses to start an unsupervised child.
func TestCoordRun_Lease_AcquireFaultAborts(t *testing.T) {
	const (
		agentID = "lwfault"
		project = "lw-fault"
		session = "fleet-lwfault"
	)
	_ = coordRunTestHome(t, agentID, project, session)
	sentinel := filepath.Join(t.TempDir(), "child-ran")
	wantErr := errors.New("disk gone")
	opts := coordRunOpts{
		agentID:      agentID,
		project:      project,
		session:      session,
		argv:         childSentinelArgv(sentinel),
		killTmux:     func(string) error { return nil },
		acquireLease: func() (coordLease, bool, []liveHolderInfo, error) { return nil, false, nil, wantErr },
	}
	err := runCoordRun(context.Background(), opts, os.Stdout, os.Stderr)
	if err == nil {
		t.Fatal("acquire fault must abort runCoordRun")
	}
	if _, serr := os.Stat(sentinel); !errors.Is(serr, os.ErrNotExist) {
		t.Errorf("child must NOT start on acquire fault")
	}
}
