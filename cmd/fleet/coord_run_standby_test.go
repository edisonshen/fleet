// Warm-standby poll-loop tests for `fleet coord-run --standby`
// (DESIGN-handoff-drain-storm-leak §3(A)/§3(B), PR3). These exercise the
// standby branch of runCoordRun through the injectable acquireLease seam
// (no real flock) so they stay deterministic — no time.Sleep timing
// assertions; convergence is observed via channels + the standbyOnAcquired
// seam.
//
//	T33  standby on a busy lease does NOT exit; it POLLS and acquires +
//	     starts the engine only AFTER the leader exits (acquire flips to
//	     acquired==true).
//	     (a) ctx-cancel before acquire -> clean shutdown, child never runs.
//	     (b) acquire fault during the poll -> surfaced, child never runs.
package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// T33: a standby coord polls a busy lease and starts the engine ONLY after
// it acquires the lease (the leader exited / was taken over). The first
// acquire returns busy; subsequent ones return acquired==true.
func TestCoordRun_Standby_PollsThenAcquiresAndStartsChild(t *testing.T) {
	const (
		agentID = "sb1poll"
		project = "sb-poll"
		session = "fleet-sb1poll"
	)
	markerPath := coordRunTestHome(t, agentID, project, session)

	sentinel := filepath.Join(t.TempDir(), "child-ran")
	lease := &fakeLease{}

	// First two acquire calls observe a healthy leader (busy); the third
	// acquires. This models the standby polling while OLD is alive, then
	// OLD exits and the kernel-released flock is acquirable.
	var calls int32
	acquire := func() (coordLease, bool, error) {
		n := atomic.AddInt32(&calls, 1)
		if n < 3 {
			return nil, false, nil // busy: healthy leader still alive
		}
		return lease, true, nil // OLD gone -> we acquire
	}

	acquired := make(chan struct{}, 1)
	opts := coordRunOpts{
		agentID:           agentID,
		project:           project,
		session:           session,
		argv:              childSentinelArgv(sentinel),
		killTmux:          func(string) error { return nil },
		standby:           true,
		standbyPoll:       time.Millisecond, // fast poll: not a timing assertion
		acquireLease:      acquire,
		standbyOnAcquired: func() { acquired <- struct{}{} },
	}

	done := make(chan error, 1)
	go func() { done <- runCoordRun(context.Background(), opts, os.Stdout, os.Stderr) }()

	// The loop must converge (acquire flips to true on the 3rd call).
	select {
	case <-acquired:
	case <-time.After(10 * time.Second):
		t.Fatal("standby never acquired the lease")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runCoordRun: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runCoordRun did not return after acquiring + running child")
	}

	if _, err := os.Stat(sentinel); err != nil {
		t.Errorf("child sentinel %s missing — standby did not start the child after acquire: %v", sentinel, err)
	}
	if got := lease.heartbeats(); got != 1 {
		t.Errorf("Heartbeat starts = %d, want 1 (once, after acquire)", got)
	}
	if got := lease.releases(); got != 1 {
		t.Errorf("Release runs = %d, want 1 (after clean child exit)", got)
	}
	if atomic.LoadInt32(&calls) < 3 {
		t.Errorf("acquire was called %d times, want >= 3 (polled the busy lease)", atomic.LoadInt32(&calls))
	}
	assertCleanupRan(t, agentID, markerPath)
}

// T33(a): a standby canceled before it ever acquires shuts down cleanly —
// it never starts the child, returns nil, and never registers a heartbeat
// or release (it never led).
func TestCoordRun_Standby_CancelBeforeAcquire_NeverStartsChild(t *testing.T) {
	const (
		agentID = "sb2cancel"
		project = "sb-cancel"
		session = "fleet-sb2cancel"
	)
	markerPath := coordRunTestHome(t, agentID, project, session)

	sentinel := filepath.Join(t.TempDir(), "child-ran")
	lease := &fakeLease{}

	// Lease is busy forever (leader never exits during the test window).
	acquire := func() (coordLease, bool, error) { return nil, false, nil }

	ctx, cancel := context.WithCancel(context.Background())
	opts := coordRunOpts{
		agentID:      agentID,
		project:      project,
		session:      session,
		argv:         childSentinelArgv(sentinel),
		killTmux:     func(string) error { return nil },
		standby:      true,
		standbyPoll:  50 * time.Millisecond,
		acquireLease: acquire,
	}

	done := make(chan error, 1)
	go func() { done <- runCoordRun(ctx, opts, os.Stdout, os.Stderr) }()
	cancel() // shut the standby down before it can acquire

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("canceled standby must return nil (clean shutdown), got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("canceled standby did not return")
	}

	if _, err := os.Stat(sentinel); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("child sentinel %s exists — standby MUST NOT start the child without the lease", sentinel)
	}
	if got := lease.heartbeats(); got != 0 {
		t.Errorf("Heartbeat starts = %d, want 0 (never led)", got)
	}
	if got := lease.releases(); got != 0 {
		t.Errorf("Release runs = %d, want 0 (never acquired)", got)
	}
	// coord.Cleanup still ran (our own record archived) on the clean exit.
	assertCleanupRan(t, agentID, markerPath)
}

// T33(b): a real acquire/takeover fault during the poll is surfaced, not
// silently looped — surface-don't-silo. The child never starts.
func TestCoordRun_Standby_AcquireFaultDuringPoll_Surfaces(t *testing.T) {
	const (
		agentID = "sb3fault"
		project = "sb-fault"
		session = "fleet-sb3fault"
	)
	_ = coordRunTestHome(t, agentID, project, session)

	sentinel := filepath.Join(t.TempDir(), "child-ran")
	wantErr := errors.New("epoch dir unreadable")

	// First call busy (enters the poll loop); second call faults.
	var calls int32
	acquire := func() (coordLease, bool, error) {
		if atomic.AddInt32(&calls, 1) == 1 {
			return nil, false, nil
		}
		return nil, false, wantErr
	}

	opts := coordRunOpts{
		agentID:      agentID,
		project:      project,
		session:      session,
		argv:         childSentinelArgv(sentinel),
		killTmux:     func(string) error { return nil },
		standby:      true,
		standbyPoll:  time.Millisecond,
		acquireLease: acquire,
	}

	err := runCoordRun(context.Background(), opts, os.Stdout, os.Stderr)
	if !errors.Is(err, wantErr) {
		t.Fatalf("standby acquire fault not surfaced; got %v, want wrap of %v", err, wantErr)
	}
	if _, serr := os.Stat(sentinel); !errors.Is(serr, os.ErrNotExist) {
		t.Errorf("child sentinel %s exists — child MUST NOT start on a standby acquire fault", sentinel)
	}
}
