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

// TestWorkersWorktreePath_PrintsCanonicalPath — the cap > 1 dispatch
// resolver. Coordinator skill calls `fleet workers worktree-path
// --project <p> <slug>` and uses stdout verbatim as the second arg to
// `git worktree add`. Smoke test that we emit the expected layout
// (~/.fleet/projects/<p>/worktrees/<slug>) without creating the dir.
func TestWorkersWorktreePath_PrintsCanonicalPath(t *testing.T) {
	fleetHome, project := setupTasksHome(t)
	out := &bytes.Buffer{}
	cmd := newWorkersWorktreePathCmd()
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs([]string{"--project", project, "alpha-1234"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("worktree-path: %v\n%s", err, out.String())
	}
	want := filepath.Join(fleetHome, "projects", project, "worktrees", "alpha-1234")
	got := strings.TrimSpace(out.String())
	// state.WorktreePath returns a trailing-slash; tolerate either form.
	if !strings.HasPrefix(got, want) {
		t.Errorf("worktree-path emitted %q, want prefix %q", got, want)
	}
	// Path-only resolver must NOT create the dir.
	if _, err := os.Stat(filepath.Join(fleetHome, "projects", project, "worktrees")); !os.IsNotExist(err) {
		t.Errorf("worktree-path created the directory tree (should be path-only): %v", err)
	}
}

// TestWorkersWorktreePath_RejectsEmptySlug — basic argv guard.
func TestWorkersWorktreePath_RejectsEmptySlug(t *testing.T) {
	_, project := setupTasksHome(t)
	out := &bytes.Buffer{}
	cmd := newWorkersWorktreePathCmd()
	cmd.SetOut(out)
	cmd.SetErr(out)
	// cobra eats the empty string before us, but a single whitespace
	// arg slips through as a positional — we reject explicitly.
	cmd.SetArgs([]string{"--project", project, "   "})
	if err := cmd.Execute(); err == nil {
		t.Errorf("expected error on whitespace-only slug, got nil; out=%q", out.String())
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

// ---------- fleet workers delete (issue #101) ----------

// TestWorkersDelete_RemovesActiveDir verifies the CLI wrapper around
// workers.Delete clears workers/<slug>/ on disk and prints the
// "removed" line.
func TestWorkersDelete_RemovesActiveDir(t *testing.T) {
	fleetHome, project := setupTasksHome(t)

	slug := "del-active-1a2b"
	seedWorker(t, project, slug, workers.PhaseDone, os.Getpid())
	dir := filepath.Join(fleetHome, "projects", project, "workers", slug)
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("seeded dir missing: %v", err)
	}

	out := &bytes.Buffer{}
	cmd := newWorkersDeleteCmd()
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs([]string{"--project", project, slug})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("dir still present: %v", err)
	}
	if !strings.Contains(out.String(), "removed") {
		t.Errorf("expected `removed` in output, got %q", out.String())
	}
}

// TestWorkersDelete_IdempotentMissingDir — running delete on a slug
// whose dir was never created (or was already removed) returns 0 and
// prints `already gone`. The coord skill + TUI defense-in-depth path
// can both fire without tripping over each other.
func TestWorkersDelete_IdempotentMissingDir(t *testing.T) {
	_, project := setupTasksHome(t)

	out := &bytes.Buffer{}
	cmd := newWorkersDeleteCmd()
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs([]string{"--project", project, "never-was-1234"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("delete missing: %v", err)
	}
	if !strings.Contains(out.String(), "already gone") {
		t.Errorf("expected `already gone`, got %q", out.String())
	}
}

// TestWorkersDelete_RejectsArchiveSlug — the literal slug "archive"
// would resolve to workers/archive/ and blow away the entire archive
// retention tree. The CLI must refuse it. Also confirms the operator-
// visible archive dir survives untouched.
func TestWorkersDelete_RejectsArchiveSlug(t *testing.T) {
	fleetHome, project := setupTasksHome(t)

	archRoot := filepath.Join(fleetHome, "projects", project, "workers", "archive")
	if err := os.MkdirAll(archRoot, 0o755); err != nil {
		t.Fatalf("mkdir arch: %v", err)
	}
	canary := filepath.Join(archRoot, "stale-1111-20200101-000000")
	if err := os.MkdirAll(canary, 0o755); err != nil {
		t.Fatalf("mkdir canary: %v", err)
	}

	out := &bytes.Buffer{}
	cmd := newWorkersDeleteCmd()
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs([]string{"--project", project, "archive"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error, got nil")
	}
	if _, err := os.Stat(canary); err != nil {
		t.Errorf("canary archive disturbed: %v", err)
	}
}

// ---------- three-stage flow review CLI (reviewer-subagent-arch) ----------

// TestWorkersUpdateCLI_ReviewCodexSkipReasonAllowlist: the CLI flag
// rejects skip reasons outside the documented allowlist
// (rate-limited|unavailable) BEFORE state.json is touched. The hard
// state-layer gate is workers.WriteState; this CLI guard surfaces a
// friendly error before that fires.
func TestWorkersUpdateCLI_ReviewCodexSkipReasonAllowlist(t *testing.T) {
	_, project := setupTasksHome(t)

	cases := []struct {
		name      string
		reason    string
		wantError bool
	}{
		{name: "rate-limited accepted", reason: "rate-limited", wantError: false},
		{name: "unavailable accepted", reason: "unavailable", wantError: false},
		// "no-git" is the documented skip reason for non-git projects
		// (operator clarification 2026-05-12). Codex review needs a git
		// diff; non-git workers record this and continue.
		{name: "no-git accepted", reason: "no-git", wantError: false},
		{name: "empty rejected", reason: "", wantError: true},
		{name: "didn't feel like it rejected", reason: "lazy", wantError: true},
		{name: "uppercase rejected", reason: "RATE-LIMITED", wantError: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			slug := "rev-cli-" + sanitizeCLISlug(c.name)
			opts := &workersUpdateOpts{
				project: project, phase: "review-done",
				reviewClaudeStatus:       "passed",
				reviewClaudeStatusSet:    true,
				reviewClaudeRounds:       1,
				reviewClaudeRoundsSet:    true,
				reviewCodexStatus:        "skipped",
				reviewCodexStatusSet:     true,
				reviewCodexSkipReason:    c.reason,
				reviewCodexSkipReasonSet: true,
			}
			err := runWorkersUpdate(slug, opts, &bytes.Buffer{})
			if c.wantError && err == nil {
				t.Errorf("reason=%q: expected error", c.reason)
			}
			if !c.wantError && err != nil {
				t.Errorf("reason=%q: unexpected error: %v", c.reason, err)
			}
		})
	}
}

// TestWorkersUpdateCLI_ReviewClaudeSkippedRejected: /review can never
// be skipped — it's the load-bearing reviewer. The CLI rejects
// --review-claude-status=skipped at the flag layer.
func TestWorkersUpdateCLI_ReviewClaudeSkippedRejected(t *testing.T) {
	_, project := setupTasksHome(t)
	err := runWorkersUpdate("rev-cli-claude-skip", &workersUpdateOpts{
		project: project, phase: "review-done",
		reviewClaudeStatus:    "skipped",
		reviewClaudeStatusSet: true,
	}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("--review-claude-status=skipped should be rejected")
	}
	if !strings.Contains(err.Error(), "skipped") {
		t.Errorf("error should mention skipped: %v", err)
	}
}

// TestWorkersUpdateCLI_ReviewCodexSkipReasonWithoutSkippedStatusRejected:
// setting --review-codex-skip-reason without --review-codex-status=skipped
// is a confused call (reason has no terminal status to anchor it).
func TestWorkersUpdateCLI_ReviewCodexSkipReasonWithoutSkippedStatusRejected(t *testing.T) {
	_, project := setupTasksHome(t)
	err := runWorkersUpdate("rev-cli-confused", &workersUpdateOpts{
		project: project, phase: "review-done",
		reviewClaudeStatus:       "passed",
		reviewClaudeStatusSet:    true,
		reviewCodexStatus:        "passed",
		reviewCodexStatusSet:     true,
		reviewCodexSkipReason:    "rate-limited", // dangling
		reviewCodexSkipReasonSet: true,
	}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("skip reason without skipped status should be rejected")
	}
}

// TestWorkersUpdateCLI_ReviewFieldsPersist: the happy path — a
// reviewer subagent transitions a worker through phase=review-done +
// records terminal review status, then the finisher writes phase=push
// and reads the same review_* fields back. Pins the sticky-field
// semantics.
func TestWorkersUpdateCLI_ReviewFieldsPersist(t *testing.T) {
	_, project := setupTasksHome(t)
	slug := "rev-cli-persist-aaaa"

	// Step 1: worker writes review-pending (no review fields yet).
	if err := runWorkersUpdate(slug, &workersUpdateOpts{
		project: project, phase: "review-pending",
	}, &bytes.Buffer{}); err != nil {
		t.Fatalf("review-pending: %v", err)
	}

	// Step 2: reviewer subagent writes review-done + terminal status.
	if err := runWorkersUpdate(slug, &workersUpdateOpts{
		project: project, phase: "review-done",
		reviewClaudeStatus:       "passed",
		reviewClaudeStatusSet:    true,
		reviewClaudeRounds:       3,
		reviewClaudeRoundsSet:    true,
		reviewCodexStatus:        "skipped",
		reviewCodexStatusSet:     true,
		reviewCodexSkipReason:    "rate-limited",
		reviewCodexSkipReasonSet: true,
	}, &bytes.Buffer{}); err != nil {
		t.Fatalf("review-done: %v", err)
	}

	// Step 3: finisher writes phase=push (no review flags — sticky).
	if err := runWorkersUpdate(slug, &workersUpdateOpts{
		project: project, phase: "push",
	}, &bytes.Buffer{}); err != nil {
		t.Fatalf("push: %v", err)
	}

	st, err := workers.ReadState(project, slug)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if st.Phase != workers.PhasePush {
		t.Errorf("phase=%q; want push", st.Phase)
	}
	if st.ReviewClaudeStatus != workers.ReviewStatusPassed {
		t.Errorf("claude=%q; want passed", st.ReviewClaudeStatus)
	}
	if st.ReviewClaudeRounds != 3 {
		t.Errorf("claude rounds=%d; want 3", st.ReviewClaudeRounds)
	}
	if st.ReviewCodexStatus != workers.ReviewStatusSkipped {
		t.Errorf("codex=%q; want skipped", st.ReviewCodexStatus)
	}
	if st.ReviewCodexSkipReason != "rate-limited" {
		t.Errorf("skip reason=%q; want rate-limited", st.ReviewCodexSkipReason)
	}
}

// TestWorkersUpdateCLI_PushRejectedWithoutReview: the CLI must
// surface the state-layer phase=push gate to the caller. A finisher
// that tries to flip phase=push on a worker without review terminal
// status sees the error.
func TestWorkersUpdateCLI_PushRejectedWithoutReview(t *testing.T) {
	_, project := setupTasksHome(t)
	slug := "rev-cli-push-noreview-aaaa"
	// Worker reached review-pending but no reviewer ever ran.
	if err := runWorkersUpdate(slug, &workersUpdateOpts{
		project: project, phase: "review-pending",
	}, &bytes.Buffer{}); err != nil {
		t.Fatalf("review-pending: %v", err)
	}
	// Finisher tries to skip ahead to push without the reviewer.
	err := runWorkersUpdate(slug, &workersUpdateOpts{
		project: project, phase: "push",
	}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("phase=push without review terminal status should be rejected")
	}
}

// TestWorkersUpdateCLI_StartingClearsReviewState: a re-dispatch
// (phase=starting) must reset the prior attempt's review fields.
// Otherwise a worker that bypassed the reviewer on retry would
// inherit "passed" from the previous attempt.
func TestWorkersUpdateCLI_StartingClearsReviewState(t *testing.T) {
	_, project := setupTasksHome(t)
	slug := "rev-cli-redispatch-aaaa"
	// Prior attempt: reached review-done with passed status.
	if err := runWorkersUpdate(slug, &workersUpdateOpts{
		project: project, phase: "review-done",
		reviewClaudeStatus:    "passed",
		reviewClaudeStatusSet: true,
		reviewCodexStatus:     "passed",
		reviewCodexStatusSet:  true,
	}, &bytes.Buffer{}); err != nil {
		t.Fatalf("seed review-done: %v", err)
	}
	// Re-dispatch.
	if err := runWorkersUpdate(slug, &workersUpdateOpts{
		project: project, phase: "starting",
	}, &bytes.Buffer{}); err != nil {
		t.Fatalf("re-dispatch starting: %v", err)
	}
	st, err := workers.ReadState(project, slug)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if st.ReviewClaudeStatus != "" {
		t.Errorf("claude not cleared: %q", st.ReviewClaudeStatus)
	}
	if st.ReviewCodexStatus != "" {
		t.Errorf("codex not cleared: %q", st.ReviewCodexStatus)
	}
}

// sanitizeCLISlug is a tiny helper to make test names slug-safe.
func sanitizeCLISlug(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			out = append(out, c)
		case c >= 'A' && c <= 'Z':
			out = append(out, c+32)
		case c == '-':
			out = append(out, c)
		default:
			out = append(out, '-')
		}
	}
	return string(out)
}

// TestWorkersUpdate_DispatchGenerationCAS is the CLI half of T2: the
// `fleet workers update --dispatch-generation` path reads the
// AUTHORITATIVE dispatch_generation from the task row and CAS-rejects a
// stale write (this covers W1 worker self-report AND W3 coord-authored
// writes, which share this CLI). A matching gen succeeds + stamps the
// state.
func TestWorkersUpdate_DispatchGenerationCAS(t *testing.T) {
	_, project := setupTasksHome(t)
	// Seed a task row, then advance its authoritative gen to 2 (a
	// re-dispatched slug).
	if err := runTasksAdd(&tasksAddOpts{
		project: project, slug: "gen-cas-aaaa", priority: "P1",
		spec: "x", status: "ready",
	}, "gen-cas-aaaa", &bytes.Buffer{}); err != nil {
		t.Fatalf("add task: %v", err)
	}
	if err := runTasksSet(&tasksSetOpts{project: project},
		"gen-cas-aaaa", "dispatch_generation=2", &bytes.Buffer{}); err != nil {
		t.Fatalf("set dispatch_generation: %v", err)
	}

	// A stale gen-1 worker self-report is CAS-rejected.
	staleErr := runWorkersUpdate("gen-cas-aaaa", &workersUpdateOpts{
		project:               project,
		phase:                 "tdd-green",
		dispatchGeneration:    1,
		dispatchGenerationSet: true,
	}, &bytes.Buffer{})
	if staleErr == nil || !strings.Contains(staleErr.Error(), "stale dispatch_generation") {
		t.Fatalf("stale gen should be CAS-rejected, got %v", staleErr)
	}
	// The rejected write did not bootstrap state.json.
	if _, err := workers.ReadState(project, "gen-cas-aaaa"); err == nil {
		t.Fatalf("stale write bootstrapped state.json (should not)")
	}

	// A matching gen-2 write succeeds and stamps the gen.
	if err := runWorkersUpdate("gen-cas-aaaa", &workersUpdateOpts{
		project:               project,
		phase:                 "tdd-green",
		dispatchGeneration:    2,
		dispatchGenerationSet: true,
	}, &bytes.Buffer{}); err != nil {
		t.Fatalf("matching gen should succeed: %v", err)
	}
	st, err := workers.ReadState(project, "gen-cas-aaaa")
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if st.DispatchGeneration != 2 {
		t.Errorf("stamped gen = %d, want 2", st.DispatchGeneration)
	}
	if st.Phase != workers.PhaseTDDGreen {
		t.Errorf("phase = %q, want tdd-green", st.Phase)
	}

	// Codex iter-3 [P1]: once a slug's task row has gen > 0, an UNGATED
	// update (no --dispatch-generation) is REJECTED — a stale/legacy
	// worker must not bypass the fence by loading + rewriting the current
	// state.json with the gen preserved.
	ungatedErr := runWorkersUpdate("gen-cas-aaaa", &workersUpdateOpts{
		project: project, phase: "tdd-refactor",
	}, &bytes.Buffer{})
	if ungatedErr == nil || !strings.Contains(ungatedErr.Error(), "must pass --dispatch-generation") {
		t.Fatalf("ungated update on a gen>0 slug must be rejected, got %v", ungatedErr)
	}
}

// TestWorkersUpdate_UngatedAllowedWhenAuthorityZero confirms the legacy
// path stays open for genuinely un-migrated slugs: a task row at gen 0
// (or no task row) accepts an ungated `fleet workers update` so
// pre-epoch workers don't fail open (codex iter-3 [P1] back-compat).
func TestWorkersUpdate_UngatedAllowedWhenAuthorityZero(t *testing.T) {
	_, project := setupTasksHome(t)
	if err := runTasksAdd(&tasksAddOpts{
		project: project, slug: "legacy-zzzz", priority: "P1",
		spec: "x", status: "ready",
	}, "legacy-zzzz", &bytes.Buffer{}); err != nil {
		t.Fatalf("add task: %v", err)
	}
	// Task row gen defaults to 0; an ungated update is allowed.
	if err := runWorkersUpdate("legacy-zzzz", &workersUpdateOpts{
		project: project, phase: "tdd-red",
	}, &bytes.Buffer{}); err != nil {
		t.Fatalf("ungated update on a gen-0 slug should succeed: %v", err)
	}
}
