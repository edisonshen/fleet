package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/workers"
)

// seedWorker plants a worker state.json in the project's workers/<slug>/
// directory at the given phase. Returns the slug for assertions.
func seedWorker(t *testing.T, project, slug string, phase workers.Phase, pid int) {
	t.Helper()
	exitCode := 0
	st := &workers.State{
		Slug:    slug,
		Project: project,
		Phase:   phase,
		PID:     pid,
		Exit:    &exitCode,
	}
	// Done phase requires pr_url; blocked requires reason.
	switch phase {
	case workers.PhaseDone:
		st.PRURL = "https://example.invalid/pr/1"
	case workers.PhaseBlocked:
		st.BlockedReason = "needs operator clarification"
	}
	if err := workers.WriteState(project, slug, st); err != nil {
		t.Fatalf("seed worker %q: %v", slug, err)
	}
}

// TestWorkersList_EmptyAndPopulated covers both the no-rows banner and
// a populated table.
func TestWorkersList_EmptyAndPopulated(t *testing.T) {
	_, project := setupTasksHome(t)

	// Empty case.
	out := &bytes.Buffer{}
	if err := runWorkersList(&workersListOpts{project: project}, out); err != nil {
		t.Fatalf("list (empty): %v", err)
	}
	if !strings.Contains(out.String(), "no active workers") {
		t.Errorf("empty output should explain how to seed work: %s", out.String())
	}

	// Seed an active starting worker (not done — done would need pr_url).
	seedWorker(t, project, "alpha-1234", workers.PhaseTDDRed, os.Getpid())
	out.Reset()
	if err := runWorkersList(&workersListOpts{project: project}, out); err != nil {
		t.Fatalf("list (populated): %v", err)
	}
	if !strings.Contains(out.String(), "alpha-1234") {
		t.Errorf("worker slug missing: %s", out.String())
	}
	if !strings.Contains(out.String(), "tdd-red") {
		t.Errorf("phase missing: %s", out.String())
	}
}

// TestWorkersList_AllShowsArchived — --all surfaces archived dirs.
func TestWorkersList_AllShowsArchived(t *testing.T) {
	fleetHome, project := setupTasksHome(t)
	// Plant an archived worker dir directly (Archive needs a live dir
	// + state.json + lock dance — we shortcut for the table check).
	archDir := filepath.Join(fleetHome, "projects", project, "workers", "archive", "old-7a3c-20260501-100000")
	if err := os.MkdirAll(archDir, 0o755); err != nil {
		t.Fatalf("mkdir arch: %v", err)
	}
	if err := os.WriteFile(filepath.Join(archDir, "state.json"), []byte(`{
		"slug":"old-7a3c","project":"`+project+`",
		"phase":"done","pr_url":"https://example.invalid/pr/9",
		"started_at":"2026-05-01T10:00:00Z","updated_at":"2026-05-01T11:00:00Z"
	}`), 0o644); err != nil {
		t.Fatalf("write arch state: %v", err)
	}

	out := &bytes.Buffer{}
	if err := runWorkersList(&workersListOpts{project: project, all: true}, out); err != nil {
		t.Fatalf("list --all: %v", err)
	}
	if !strings.Contains(out.String(), "old-7a3c") {
		t.Errorf("archived worker not surfaced under --all: %s", out.String())
	}
}

// TestWorkersList_StartingHasPidZero — codex iter-6 P2: a fresh
// state.json (phase=starting, pid=0) must NOT render as "dead".
func TestWorkersList_StartingHasPidZero(t *testing.T) {
	_, project := setupTasksHome(t)
	// Bootstrap a worker via UpdateState — that's the path the coord
	// uses, and it produces phase=starting with pid=0.
	if err := workers.UpdateState(project, "fresh-7777", func(s *workers.State) {
		// Don't mutate; leave the bootstrap defaults.
	}); err != nil {
		t.Fatalf("UpdateState: %v", err)
	}
	out := &bytes.Buffer{}
	if err := runWorkersList(&workersListOpts{project: project}, out); err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out.String(), "starting") {
		t.Errorf("starting bucket missing for pid=0 worker: %s", out.String())
	}
	if strings.Contains(out.String(), "\tdead\t") {
		t.Errorf("starting worker should not render as dead: %s", out.String())
	}
}

// TestWorkersPrune_BadDuration — invalid --older-than rejects.
func TestWorkersPrune_BadDuration(t *testing.T) {
	_, project := setupTasksHome(t)
	err := runWorkersPrune(&workersPruneOpts{project: project, older: "not-a-duration"}, &bytes.Buffer{})
	if err == nil {
		t.Error("expected error on bogus --older-than")
	}
}

// TestWorkersPrune_RemovesOldArchive — plants two archive dirs (one
// old, one fresh) and verifies prune drops only the old one.
func TestWorkersPrune_RemovesOldArchive(t *testing.T) {
	fleetHome, project := setupTasksHome(t)

	archRoot := filepath.Join(fleetHome, "projects", project, "workers", "archive")
	if err := os.MkdirAll(archRoot, 0o755); err != nil {
		t.Fatalf("mkdir arch root: %v", err)
	}
	old := filepath.Join(archRoot, "stale-1111-20200101-000000")
	fresh := filepath.Join(archRoot, "fresh-2222-"+time.Now().UTC().Format("20060102-150405"))
	for _, d := range []string{old, fresh} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	out := &bytes.Buffer{}
	if err := runWorkersPrune(&workersPruneOpts{project: project, older: "7d"}, out); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("old archive should be gone: stat err=%v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh archive should remain: %v", err)
	}
}
