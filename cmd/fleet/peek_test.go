package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/workers"
)

// TestPeek_OnceMissingSlug — peek on a non-existent slug fails fast
// in the non-follow path.
func TestPeek_OnceMissingSlug(t *testing.T) {
	_, project := setupTasksHome(t)
	err := runPeek(&peekOpts{project: project}, "nonexistent-1234", &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil {
		t.Error("expected error for missing slug")
	}
}

// TestPeek_OncePrintsState — happy path: peek on an existing worker
// prints the JSON state block.
func TestPeek_OncePrintsState(t *testing.T) {
	_, project := setupTasksHome(t)
	seedWorker(t, project, "alpha-1234", workers.PhaseTDDRed, os.Getpid())

	out := &bytes.Buffer{}
	if err := runPeek(&peekOpts{project: project}, "alpha-1234", out, &bytes.Buffer{}); err != nil {
		t.Fatalf("peek: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, `"slug": "alpha-1234"`) {
		t.Errorf("output missing slug: %s", got)
	}
	if !strings.Contains(got, `"phase": "tdd-red"`) {
		t.Errorf("output missing phase: %s", got)
	}
}

// TestPeek_OnceWithLogs — --logs dumps output.log when present.
func TestPeek_OnceWithLogs(t *testing.T) {
	fleetHome, project := setupTasksHome(t)
	seedWorker(t, project, "logs-test-7a3c", workers.PhaseTDDGreen, os.Getpid())
	logPath := filepath.Join(fleetHome, "projects", project, "workers", "logs-test-7a3c", "output.log")
	if err := os.WriteFile(logPath, []byte("hello from worker\nstep 2\n"), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	out := &bytes.Buffer{}
	if err := runPeek(&peekOpts{project: project, logs: true}, "logs-test-7a3c", out, &bytes.Buffer{}); err != nil {
		t.Fatalf("peek: %v", err)
	}
	if !strings.Contains(out.String(), "hello from worker") {
		t.Errorf("log content not surfaced: %s", out.String())
	}
}

// TestPeek_FollowExitsOnTerminalPhase — when the worker is already in a
// terminal phase, follow returns immediately after one print.
func TestPeek_FollowExitsOnTerminalPhase(t *testing.T) {
	_, project := setupTasksHome(t)
	// Phase=blocked is terminal AND doesn't need pr_url, just a reason
	// (which seedWorker provides).
	seedWorker(t, project, "blocked-1111", workers.PhaseBlocked, 0)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	out := &bytes.Buffer{}
	done := make(chan error, 1)
	go func() {
		done <- peekFollow(ctx, project, "blocked-1111", false, out, &bytes.Buffer{})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("peekFollow returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("peekFollow on terminal-phase worker did not return promptly")
	}
	if !strings.Contains(out.String(), `"phase": "blocked"`) {
		t.Errorf("expected blocked phase in output: %s", out.String())
	}
}

// TestPeek_FollowReactsToPhaseChange — peek --follow prints initial
// state, sees the phase change to done, and exits cleanly with both
// states in the output.
func TestPeek_FollowReactsToPhaseChange(t *testing.T) {
	_, project := setupTasksHome(t)
	seedWorker(t, project, "evolve-2222", workers.PhaseTDDRed, os.Getpid())

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	out := &bytes.Buffer{}
	done := make(chan error, 1)
	go func() {
		done <- peekFollow(ctx, project, "evolve-2222", false, out, &bytes.Buffer{})
	}()

	// Give peek time to see the initial state.
	time.Sleep(2 * pollInterval)

	// Flip to done — that's a terminal phase and exits the follow loop.
	exitCode := 0
	finalState := &workers.State{
		Slug: "evolve-2222", Project: project,
		Phase: workers.PhaseDone, PRURL: "https://example.invalid/pr/42",
		Exit: &exitCode,
	}
	if err := workers.WriteState(project, "evolve-2222", finalState); err != nil {
		t.Fatalf("update state: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("peekFollow returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("peekFollow did not exit after terminal phase")
	}
	got := out.String()
	if !strings.Contains(got, `"phase": "tdd-red"`) {
		t.Errorf("initial phase missing: %s", got)
	}
	if !strings.Contains(got, `"phase": "done"`) {
		t.Errorf("final done phase missing: %s", got)
	}
}
