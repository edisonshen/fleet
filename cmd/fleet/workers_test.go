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

// TestWorkersUpdate_SetsPhase_NoExplicitPid — happy path without
// --pid: state.json materializes with phase set, but pid stays at
// the previous value (0 for a fresh bootstrap). Codex iter-4 [P1]
// regress: defaulting --pid to os.Getpid() of the short-lived
// `fleet` helper made workers list/peek render active workers as
// dead almost immediately. We now require --pid to be explicit.
func TestWorkersUpdate_SetsPhase_NoExplicitPid(t *testing.T) {
	_, project := setupTasksHome(t)
	out := &bytes.Buffer{}
	opts := &workersUpdateOpts{
		project: project,
		phase:   "tdd-red",
	}
	if err := runWorkersUpdate("alpha-1234", opts, out); err != nil {
		t.Fatalf("update: %v", err)
	}
	st, err := workers.ReadState(project, "alpha-1234")
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if st.Phase != workers.PhaseTDDRed {
		t.Errorf("phase = %q, want tdd-red", st.Phase)
	}
	// Without --pid, pid stays at the bootstrap default (0), NOT
	// os.Getpid() of this test process.
	if st.PID != 0 {
		t.Errorf("pid = %d, want 0 (no --pid passed)", st.PID)
	}
	if !strings.Contains(out.String(), "tdd-red") {
		t.Errorf("stdout should report new phase: %s", out.String())
	}
}

// TestWorkersUpdate_ExplicitPidIsRecorded — passing --pid <n> writes
// that pid to state.json. Workers running inside a stable subprocess
// pass it explicitly; the helper-process default (no flag) is
// "preserve existing pid" so we don't trash a live worker's pid.
func TestWorkersUpdate_ExplicitPidIsRecorded(t *testing.T) {
	_, project := setupTasksHome(t)
	opts := &workersUpdateOpts{
		project: project,
		phase:   "tdd-red",
		pid:     42424,
		pidSet:  true,
	}
	if err := runWorkersUpdate("alpha-1234", opts, &bytes.Buffer{}); err != nil {
		t.Fatalf("update: %v", err)
	}
	st, err := workers.ReadState(project, "alpha-1234")
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if st.PID != 42424 {
		t.Errorf("pid = %d, want 42424 (explicit --pid)", st.PID)
	}
}

// TestWorkersUpdate_NonTerminalClearsTerminalMetadata — codex iter-4
// [P2] regress: a retry that reuses the same workers/<slug>/state.json
// (e.g. after CI red, the coord re-dispatches the same task) must
// not carry the previous attempt's pr_url / blocked_reason / exit
// fields onto the new attempt. --phase starting clears them.
func TestWorkersUpdate_NonTerminalClearsTerminalMetadata(t *testing.T) {
	_, project := setupTasksHome(t)

	// First attempt: worker shipped a PR.
	if err := runWorkersUpdate("alpha-9876", &workersUpdateOpts{
		project: project, phase: "done",
		prURL: "https://example.invalid/pr/old",
		exit:  0, exitSet: true,
	}, &bytes.Buffer{}); err != nil {
		t.Fatalf("first attempt: %v", err)
	}

	// Second attempt: coord re-dispatches; bootstrap to phase=starting.
	if err := runWorkersUpdate("alpha-9876", &workersUpdateOpts{
		project: project, phase: "starting",
	}, &bytes.Buffer{}); err != nil {
		t.Fatalf("second attempt bootstrap: %v", err)
	}

	st, err := workers.ReadState(project, "alpha-9876")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if st.PRURL != "" {
		t.Errorf("retry should clear prior pr_url, got %q", st.PRURL)
	}
	if st.BlockedReason != "" {
		t.Errorf("retry should clear prior blocked_reason, got %q",
			st.BlockedReason)
	}
	if st.Exit != nil {
		t.Errorf("retry should clear prior exit, got %v", st.Exit)
	}
}

// TestWorkersUpdate_DonePhaseRequiresPRURL — phase=done is rejected
// without --pr-url because workers.writeStateLocked enforces
// ErrPhaseRequiresPR. The CLI must surface that error rather than
// silently writing an inconsistent state.
func TestWorkersUpdate_DonePhaseRequiresPRURL(t *testing.T) {
	_, project := setupTasksHome(t)
	err := runWorkersUpdate("alpha-2222", &workersUpdateOpts{
		project: project, phase: "done",
	}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("phase=done without --pr-url should error")
	}
	if !strings.Contains(err.Error(), "pr_url") {
		t.Errorf("error should mention pr_url: %v", err)
	}
}

// TestWorkersUpdate_BlockedRequiresReason — phase=blocked is rejected
// without --reason. Same contract as done+pr-url; this lets the
// coordinator distinguish "worker said it was blocked" from
// "worker died" without parsing the OS PID.
func TestWorkersUpdate_BlockedRequiresReason(t *testing.T) {
	_, project := setupTasksHome(t)
	err := runWorkersUpdate("alpha-3333", &workersUpdateOpts{
		project: project, phase: "blocked",
	}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("phase=blocked without --reason should error")
	}
	if !strings.Contains(err.Error(), "blocked_reason") {
		t.Errorf("error should mention blocked_reason: %v", err)
	}
}

// TestWorkersUpdate_DoneWithPRURL — phase=done + --pr-url succeeds
// and persists the URL so the coord's reconcile loop sees it.
func TestWorkersUpdate_DoneWithPRURL(t *testing.T) {
	_, project := setupTasksHome(t)
	if err := runWorkersUpdate("alpha-4444", &workersUpdateOpts{
		project: project, phase: "done",
		prURL: "https://example.invalid/pr/42",
	}, &bytes.Buffer{}); err != nil {
		t.Fatalf("done update: %v", err)
	}
	st, err := workers.ReadState(project, "alpha-4444")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if st.Phase != workers.PhaseDone {
		t.Errorf("phase = %q, want done", st.Phase)
	}
	if st.PRURL != "https://example.invalid/pr/42" {
		t.Errorf("pr_url = %q, want example URL", st.PRURL)
	}
}

// TestWorkersUpdate_AppendsPhaseHistory — every phase change after
// the initial bootstrap appends the previous phase to phases_completed,
// so the coord/peek surfaces can show a worker's TDD pipeline progress.
func TestWorkersUpdate_AppendsPhaseHistory(t *testing.T) {
	_, project := setupTasksHome(t)
	steps := []string{"branch", "tdd-red", "tdd-green", "tdd-refactor"}
	for _, p := range steps {
		if err := runWorkersUpdate("alpha-5555", &workersUpdateOpts{
			project: project, phase: p,
		}, &bytes.Buffer{}); err != nil {
			t.Fatalf("update %s: %v", p, err)
		}
	}
	st, err := workers.ReadState(project, "alpha-5555")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if st.Phase != workers.PhaseTDDRefactor {
		t.Errorf("final phase = %q", st.Phase)
	}
	// Bootstrap is "starting"; first transition appends "starting",
	// then each subsequent transition appends the previous phase.
	// Expected history: starting, branch, tdd-red, tdd-green.
	wantLen := len(steps) // starting + (steps-1) prior phases = len(steps)
	if len(st.PhasesCompleted) != wantLen {
		t.Errorf("phases_completed = %v (len %d), want len %d",
			st.PhasesCompleted, len(st.PhasesCompleted), wantLen)
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
