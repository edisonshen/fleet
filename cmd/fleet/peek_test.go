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

// TestPeek_OnceFallsBackToArchive — codex iter-2 P2: a worker that's
// already been archived must still be inspectable by `peek` (the slug
// is still surfaced by `workers list --all`).
func TestPeek_OnceFallsBackToArchive(t *testing.T) {
	fleetHome, project := setupTasksHome(t)

	archDir := filepath.Join(fleetHome, "projects", project, "workers", "archive", "archived-3333-20260501-100000")
	if err := os.MkdirAll(archDir, 0o755); err != nil {
		t.Fatalf("mkdir arch: %v", err)
	}
	if err := os.WriteFile(filepath.Join(archDir, "state.json"), []byte(`{
		"slug":"archived-3333","project":"`+project+`",
		"phase":"done","pr_url":"https://example.invalid/pr/1",
		"started_at":"2026-05-01T10:00:00Z","updated_at":"2026-05-01T11:00:00Z"
	}`), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(archDir, "output.log"), []byte("archived-log-content"), 0o644); err != nil {
		t.Fatalf("seed log: %v", err)
	}

	out := &bytes.Buffer{}
	if err := runPeek(&peekOpts{project: project, logs: true}, "archived-3333", out, &bytes.Buffer{}); err != nil {
		t.Fatalf("peek archived: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, `"phase": "done"`) {
		t.Errorf("archived state phase missing: %s", got)
	}
	if !strings.Contains(got, "archived-log-content") {
		t.Errorf("archived log not surfaced under --logs: %s", got)
	}
}

// TestPeek_FollowExitsCleanlyOnArchive — codex iter-2 P1: a worker
// archived between two polls should resolve via the archive and exit
// cleanly, not time out at 30s.
func TestPeek_FollowExitsCleanlyOnArchive(t *testing.T) {
	fleetHome, project := setupTasksHome(t)
	seedWorker(t, project, "fast-4444", workers.PhaseTDDRed, os.Getpid())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	out := &bytes.Buffer{}
	done := make(chan error, 1)
	go func() {
		done <- peekFollow(ctx, project, "fast-4444", false, out, &bytes.Buffer{})
	}()

	// Let peek see the initial state.
	time.Sleep(2 * pollInterval)

	// Simulate the coord's archive: rm the active dir, plant an
	// archive entry. (Real path goes through workers.Archive which
	// requires more setup; this is the same final on-disk state.)
	activeDir := filepath.Join(fleetHome, "projects", project, "workers", "fast-4444")
	if err := os.RemoveAll(activeDir); err != nil {
		t.Fatalf("rm active: %v", err)
	}
	archDir := filepath.Join(fleetHome, "projects", project, "workers", "archive", "fast-4444-20260506-120000")
	if err := os.MkdirAll(archDir, 0o755); err != nil {
		t.Fatalf("mkdir arch: %v", err)
	}
	if err := os.WriteFile(filepath.Join(archDir, "state.json"), []byte(`{
		"slug":"fast-4444","project":"`+project+`",
		"phase":"done","pr_url":"https://example.invalid/pr/2",
		"started_at":"2026-05-06T12:00:00Z","updated_at":"2026-05-06T12:00:00Z"
	}`), 0o644); err != nil {
		t.Fatalf("seed arch: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("peekFollow returned error after archive: %v", err)
		}
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("peekFollow did not exit after archive")
	}
	if !strings.Contains(out.String(), `"phase": "done"`) {
		t.Errorf("final archived state not rendered: %s", out.String())
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
