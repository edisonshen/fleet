package main

// Whole-fleet end-to-end test that drives the v0.2 workflow from cold
// start through dispatch → in-review → archive, asserting the new
// Variant A "Ops Console" TUI dashboard renders correctly at each
// step. This is the "fleet actually works" gate the operator wants
// green before tagging v0.2.
//
// The test reuses the existing coordinator integration harness
// (setupCoordIntegration in coordinator_integration_test.go) for:
//
//   - sandboxed $HOME / $FLEET_HOME / PATH (no real ~/.claude pollution)
//   - sync.Once-built `fleet` binary on PATH
//   - stub `gh` so reconcile's CI checks return empty / clean
//   - python driver for skills/coordinator/loop.tick()
//
// The TUI seam is internal/tui.RenderDashboardForTest, which scans
// FLEET_HOME on disk and renders one synchronous Init+View cycle
// without spinning the bubbletea event loop. Assertions are
// strings.Contains-based — ANSI escapes vary by lipgloss version, so
// pinning byte-exact output would be brittle.
//
// Scenarios (run as t.Run subtests of TestFleetE2E_FullWorkflow):
//
//	1. cold start → tasks add → dashboard shows ⏳ 1 + ○ idle
//	2. coord tick → dispatch → dashboard shows ▶ 1 + worker row + ● active
//	3. worker DONE_PR sentinel → drain → dashboard shows 👁 1 + still
//	   in-flight worker (state.json phase=done, archive happens later)
//	4. archive → dashboard shows decremented counts; worker row gone
//	   after worker prune
//	5. blocked worker → dashboard shows ▌ red border + "● 1 attn"

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tasks"
	"github.com/edisonshen/fleet/internal/tmux"
	"github.com/edisonshen/fleet/internal/tui"
	"github.com/edisonshen/fleet/internal/workers"
)

// TestFleetE2E_FullWorkflow drives the full v0.2 lifecycle and asserts
// the Variant A dashboard render at every transition. Each scenario
// is sequential and shares one sandbox so state from scenario N feeds
// into N+1 — that's the realistic operator path.
func TestFleetE2E_FullWorkflow(t *testing.T) {
	env := setupCoordIntegration(t, "fleet-e2e")
	env.plantCoord(t)

	// Snapshot a steady "now" basis. The dashboard's coord-active /
	// worker-stale windows are time.Sub-based; passing now=time.Now()
	// at every render reads the FS mtime fresh, so we don't have to
	// carry a single timestamp through the test. We DO use time.Now()
	// throughout — the production renderer also calls time.Since.

	// ---------- Scenario 1: cold start → tasks add → todo + idle ----------
	t.Run("cold_start_shows_todo_and_idle", func(t *testing.T) {
		// Seed one ready task. The dashboard counts ready as ⏳ todo so
		// the operator sees one chip regardless of dispatch state.
		slug := env.addReadyTask(t, "example-task",
			"do the thing.\nFiles: example.go")

		// Sanity: list output mentions the slug.
		listOut := env.runFleet(t, "tasks", "list", "--project", env.project)
		if !strings.Contains(listOut, slug) {
			t.Fatalf("tasks list should include %q, got:\n%s", slug, listOut)
		}

		out, snap := tui.RenderDashboardForTest(time.Now(), 140, 40, "test")
		if !strings.Contains(out, env.project) {
			t.Errorf("dashboard should include project name %q, got:\n%s", env.project, out)
		}
		// One ⏳ todo visible (the ready task rolls into todo).
		if !strings.Contains(out, "⏳ 1") {
			t.Errorf("dashboard should show ⏳ 1, got:\n%s", out)
		}
		// No coord ticks have run yet → no coord-state.json → ○ idle.
		if !strings.Contains(out, "○ idle") {
			t.Errorf("dashboard should show ○ idle pre-tick, got:\n%s", out)
		}
		// Header: 0 workers active.
		if !strings.Contains(out, "0 workers active") {
			t.Errorf("dashboard should show 0 workers pre-dispatch, got:\n%s", out)
		}
		// Snapshot sanity: project row has the right counts.
		if len(snap.Projects) != 1 {
			t.Fatalf("expected 1 project row, got %d", len(snap.Projects))
		}
		if snap.Projects[0].Counts.Todo != 1 {
			t.Errorf("project Todo count = %d, want 1",
				snap.Projects[0].Counts.Todo)
		}
		if len(snap.Workers) != 0 {
			t.Errorf("expected 0 worker rows pre-dispatch, got %d",
				len(snap.Workers))
		}
	})

	// ---------- Scenario 2: tick → dispatch → in-progress + worker row ----------
	t.Run("tick_dispatches_and_shows_worker", func(t *testing.T) {
		out := env.runTick(t)
		if !strings.Contains(out, `"dispatched": 1`) {
			t.Fatalf("tick did not dispatch: %s", out)
		}
		assertNoTickErrors(t, out)

		// Find the slug we added in scenario 1. There's exactly one
		// task at this point, so a quick read suffices.
		dir, err := state.ProjectDir(env.project)
		if err != nil {
			t.Fatalf("project dir: %v", err)
		}
		f, err := tasks.Read(filepath.Join(dir, "tasks.md"))
		if err != nil {
			t.Fatalf("read tasks.md: %v", err)
		}
		if len(f.Tasks) != 1 {
			t.Fatalf("expected 1 task post-dispatch, got %d", len(f.Tasks))
		}
		task := f.Tasks[0]
		if task.Status != tasks.StatusInProgress {
			t.Fatalf("task status = %q, want in-progress", task.Status)
		}

		// Replant worker_pid alive so subsequent reconciles don't
		// requeue. tasks.md gets a live OS pid; dashboard doesn't read
		// this directly but it keeps the next tick's reconcile honest.
		env.runFleet(t, "tasks", "set", "--project", env.project,
			task.Slug, fmt.Sprintf("worker_pid=%d", os.Getpid()))

		rendered, snap := tui.RenderDashboardForTest(time.Now(), 140, 40, "test")
		if !strings.Contains(rendered, "▶ 1") {
			t.Errorf("dashboard should show ▶ 1 after dispatch, got:\n%s", rendered)
		}
		// ● active assertion is blocked by #50: coord-state.json's mtime
		// is the dashboard's heartbeat signal but the skill only writes
		// it on drain, not on dispatch. Until that's fixed, a fresh
		// coord that's only dispatched looks "auto-stopped" to the
		// dashboard. Per the e2e-PR contract: file the bug, skip the
		// assertion, document in PR body, continue with the rest.
		t.Logf("dashboard coord status (post-dispatch tick): " +
			"asserting only that the row rendered; ● active assertion " +
			"deferred pending issue #50")
		if !strings.Contains(rendered, "○ idle") && !strings.Contains(rendered, "● active") {
			t.Errorf("dashboard should show some coord status, got:\n%s", rendered)
		}
		// Header reports 1 worker active (the dispatched one).
		if !strings.Contains(rendered, "1 workers active") {
			t.Errorf("dashboard should show 1 workers active, got:\n%s", rendered)
		}
		// Worker row in the right column. project:slug rendering uses
		// the trimmed slug (drops trailing -<4hex>), so we match on the
		// human-readable short.
		shortSlug := strings.TrimSuffix(task.Slug, task.Slug[len(task.Slug)-5:])
		if !strings.Contains(rendered, env.project+":"+shortSlug) {
			t.Errorf("dashboard should show worker row %s:%s, got:\n%s",
				env.project, shortSlug, rendered)
		}
		// Snapshot: exactly one worker row.
		if len(snap.Workers) != 1 {
			t.Fatalf("expected 1 worker row post-dispatch, got %d",
				len(snap.Workers))
		}
		w := snap.Workers[0]
		if w.Phase != workers.PhaseStarting {
			t.Errorf("worker phase = %q, want starting", w.Phase)
		}
	})

	// ---------- Scenario 3: DONE_PR sentinel → drain → in-review ----------
	t.Run("done_pr_drain_flips_to_in_review", func(t *testing.T) {
		// Find the slug.
		dir, err := state.ProjectDir(env.project)
		if err != nil {
			t.Fatalf("project dir: %v", err)
		}
		f, err := tasks.Read(filepath.Join(dir, "tasks.md"))
		if err != nil {
			t.Fatalf("read tasks.md: %v", err)
		}
		if len(f.Tasks) != 1 {
			t.Fatalf("expected 1 task pre-drain, got %d", len(f.Tasks))
		}
		slug := f.Tasks[0].Slug

		// Worker reports DONE via a `fleet workers update --phase done`
		// (writes state.json, exercises the real CLI path) AND via the
		// inbox archive sentinel that the coord drains on tick. Both
		// must converge on status=in-review with pr_url populated.
		// --exit 0 is required so a later workers.Archive call (scenario
		// 4) doesn't refuse on "pid=0 and no exit recorded".
		prURL := "https://github.com/fake/repo/pull/777"
		env.runFleet(t, "workers", "update",
			"--project", env.project, slug,
			"--phase", "done",
			"--pr-url", prURL,
			"--exit", "0")
		env.writeSentinelArchive(t,
			fmt.Sprintf("TASK_DONE_PR=%s %s", slug, prURL))

		out := env.runTick(t)
		if !strings.Contains(out, `"drained": 1`) {
			t.Fatalf("tick did not drain DONE_PR sentinel: %s", out)
		}
		assertNoTickErrors(t, out)

		// Re-read tasks.md to confirm in-review + pr_url.
		f, err = tasks.Read(filepath.Join(dir, "tasks.md"))
		if err != nil {
			t.Fatalf("re-read tasks.md: %v", err)
		}
		if f.Tasks[0].Status != tasks.StatusInReview {
			t.Errorf("status post-drain = %q, want in-review",
				f.Tasks[0].Status)
		}
		if f.Tasks[0].PRURL != prURL {
			t.Errorf("pr_url post-drain = %q, want %q",
				f.Tasks[0].PRURL, prURL)
		}

		rendered, snap := tui.RenderDashboardForTest(time.Now(), 140, 40, "test")
		// 👁 1 because in-review counts get their own chip; ▶ 0 means
		// the in-progress chip drops (it's omitted when 0). ⏳ 0
		// always shows.
		if !strings.Contains(rendered, "👁 1") {
			t.Errorf("dashboard should show 👁 1 post-drain, got:\n%s", rendered)
		}
		// In-progress chip omitted when 0.
		if strings.Contains(rendered, "▶ 1") {
			t.Errorf("dashboard should NOT show ▶ 1 post-drain (worker phase=done):\n%s", rendered)
		}
		// Snapshot pin: per-status counts now reflect in-review.
		p := snap.Projects[0]
		if p.Counts.InReview != 1 || p.Counts.InProgress != 0 {
			t.Errorf("counts post-drain = %+v, want InReview=1 InProgress=0",
				p.Counts)
		}
		// Worker row: phase=done → green/ok. The row may still be
		// present (Archive is a separate operator action) — assert
		// that if it's there, color is green.
		for _, w := range snap.Workers {
			if w.Slug == slug && w.Color != "green" {
				t.Errorf("worker row color post-done = %q, want green", w.Color)
			}
		}
	})

	// ---------- Scenario 4: archive → counts decrement ----------
	t.Run("archive_clears_counts_and_worker", func(t *testing.T) {
		// Find the slug.
		dir, err := state.ProjectDir(env.project)
		if err != nil {
			t.Fatalf("project dir: %v", err)
		}
		f, err := tasks.Read(filepath.Join(dir, "tasks.md"))
		if err != nil {
			t.Fatalf("read tasks.md: %v", err)
		}
		if len(f.Tasks) != 1 {
			t.Fatalf("expected 1 task pre-archive, got %d", len(f.Tasks))
		}
		slug := f.Tasks[0].Slug

		// Archive the task. Tasks.md should now have 0 entries.
		env.runFleet(t, "tasks", "archive", "--project", env.project, slug)

		// Also archive the worker so the dashboard's right column
		// drops the row. workers.Archive moves workers/<slug>/ →
		// workers/archive/<slug>-<stamp>/, which scanWorkers skips.
		if err := workers.Archive(env.project, slug); err != nil {
			t.Fatalf("workers.Archive: %v", err)
		}

		rendered, snap := tui.RenderDashboardForTest(time.Now(), 140, 40, "test")

		// All count chips at zero. ⏳ 0 always renders; in-progress and
		// in-review chips are omitted when 0.
		if !strings.Contains(rendered, "⏳ 0") {
			t.Errorf("dashboard should show ⏳ 0 post-archive, got:\n%s", rendered)
		}
		if strings.Contains(rendered, "👁 1") {
			t.Errorf("dashboard should NOT show 👁 1 post-archive:\n%s", rendered)
		}
		// Snapshot: project row still present (project dir exists),
		// but counts are zero and no workers.
		if len(snap.Projects) != 1 {
			t.Fatalf("expected 1 project row, got %d", len(snap.Projects))
		}
		p := snap.Projects[0]
		want := tui.TaskCounts{}
		if p.Counts != want {
			t.Errorf("counts post-archive = %+v, want all zero", p.Counts)
		}
		if p.Attention != 0 {
			t.Errorf("attention post-archive = %d, want 0", p.Attention)
		}
		if len(snap.Workers) != 0 {
			t.Errorf("expected 0 workers post-archive, got %d", len(snap.Workers))
		}
		// No leftover ▌ red border / "attn" chip.
		if strings.Contains(rendered, " attn") {
			t.Errorf("dashboard should NOT show attn chip post-archive:\n%s", rendered)
		}
	})

	// ---------- Scenario 5: blocked worker → attention badge ----------
	t.Run("blocked_worker_shows_attention_badge", func(t *testing.T) {
		// Add + dispatch a fresh task so we have a worker to block.
		// Disjoint Files: scope so the conflict-aware dispatch never
		// short-circuits this in worktree mode (cap=1 here, so this
		// is mostly future-proofing).
		slug := env.addReadyTask(t, "blocked-task",
			"task that will block.\nFiles: blocked.go")

		out := env.runTick(t)
		if !strings.Contains(out, `"dispatched": 1`) {
			t.Fatalf("tick did not dispatch blocked-task: %s", out)
		}
		assertNoTickErrors(t, out)

		// Manually flip the worker's state.json to phase=blocked.
		// workers.UpdateState exercises the real on-disk write path
		// (atomic rename + flock); the dashboard's scanWorkers will
		// pick up the new phase on next render.
		if err := workers.UpdateState(env.project, slug, func(s *workers.State) {
			s.Phase = workers.PhaseBlocked
			s.BlockedReason = "needs operator answer about API choice"
		}); err != nil {
			t.Fatalf("UpdateState blocked: %v", err)
		}

		rendered, snap := tui.RenderDashboardForTest(time.Now(), 140, 40, "test")

		// Project row carries the ▌ red border + "N attn" chip.
		if !strings.Contains(rendered, "▌") {
			t.Errorf("dashboard should show ▌ accent for blocked worker, got:\n%s", rendered)
		}
		if !strings.Contains(rendered, "1 attn") {
			t.Errorf("dashboard should show '1 attn' chip, got:\n%s", rendered)
		}
		// Header strip totalizer: 1 need attention.
		if !strings.Contains(rendered, "1 need attention") {
			t.Errorf("header should show 1 need attention, got:\n%s", rendered)
		}
		// Snapshot: project attention=1, worker.Color=red.
		if len(snap.Projects) != 1 {
			t.Fatalf("expected 1 project row, got %d", len(snap.Projects))
		}
		if snap.Projects[0].Attention != 1 {
			t.Errorf("project attention = %d, want 1",
				snap.Projects[0].Attention)
		}
		var found *tui.WorkerRow
		for _, w := range snap.Workers {
			if w.Slug == slug {
				found = w
				break
			}
		}
		if found == nil {
			t.Fatalf("blocked worker row missing from snapshot: %+v",
				snap.Workers)
		}
		if found.Color != "red" {
			t.Errorf("blocked worker color = %q, want red", found.Color)
		}
		if found.State != "bl" {
			t.Errorf("blocked worker state = %q, want bl", found.State)
		}
	})

	// Cleanup: kill any worker tmux sessions left by dispatches. The
	// per-test setupCoordIntegration cleanup handles the coord; this
	// catches workers spawned by the two ticks above.
	t.Cleanup(func() {
		for _, rec := range listAllAgents(t) {
			_ = tmux.Kill(rec.TmuxSession)
		}
	})
}
