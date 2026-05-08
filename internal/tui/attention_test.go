// Tests for issue #75 — drill into "need attention" tasks from the
// dashboard. Covers: status-aware glyph in expansion, [⏎] task →
// detail panel rendering (spec + question + worker info), [a] task
// row → worker peek route, [a] in detail panel → worker peek, no-
// worker fallback messaging, and the dashboard refresh recompute of
// the attention count after a status transition.
package tui

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/edisonshen/fleet/internal/tasks"
	"github.com/edisonshen/fleet/internal/workers"
)

// stubReadTaskWorker swaps the package-level readTaskWorker var for
// the lifetime of the test. Returns a restorer the caller defers.
func stubReadTaskWorker(t *testing.T, fn func(project, slug string) (*workers.State, error)) {
	t.Helper()
	orig := readTaskWorker
	readTaskWorker = fn
	t.Cleanup(func() { readTaskWorker = orig })
}

// seedBlockedTask writes a tasks.md file with one task at the named
// status. Lets us hand-build snapshots without dragging in seedTasks's
// multi-status loop.
func seedBlockedTask(t *testing.T, projectsRoot, project, slug string, status tasks.Status, notes string) {
	t.Helper()
	dir := filepath.Join(projectsRoot, project)
	if err := stateMkdir(dir); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	now := time.Now().UTC()
	f := &tasks.File{Schema: tasks.SchemaVersion}
	tk := &tasks.Task{
		Slug:       slug,
		Status:     status,
		Priority:   tasks.PriorityP2,
		Created:    now,
		Updated:    now,
		Spec:       "Implement the gizmo so the widget can foo.",
		Acceptance: "gizmo foos",
		Notes:      notes,
	}
	if err := f.Add(tk); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := tasks.Write(filepath.Join(dir, "tasks.md"), f); err != nil {
		t.Fatalf("write tasks.md: %v", err)
	}
}

// TestRows_AskingTaskGetsAttentionGlyph pins that a blocked-status
// task row renders the ⚠ attention glyph in the expansion. Without
// this signal the operator sees "1 attn" on the project chip but
// can't tell WHICH task is asking.
func TestRows_AskingTaskGetsAttentionGlyph(t *testing.T) {
	pdir := withFleetHome(t)
	seedBlockedTask(t, pdir, "fleet", "needs-input-aaaa", tasks.StatusBlocked,
		"blocked because: need operator clarification on the API shape")

	m := New("test")
	m.width = 130
	m.height = 30
	m.dashboard = scanDashboard(time.Now())
	m.expanded = map[string]bool{"fleet": true}

	out := m.View()
	if !strings.Contains(out, "needs-input-aaaa") {
		t.Fatalf("blocked task slug should render in expansion, got:\n%s", out)
	}
	// The ⚠ glyph must appear ahead of the slug on the same line. We
	// don't pin the exact column (lipgloss escapes around it shift
	// with palette/bold rendering) — substring on the line is enough.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "needs-input-aaaa") {
			if !strings.Contains(line, "⚠") {
				t.Errorf("blocked task line should carry ⚠ attention glyph, got:\n%s", line)
			}
			return
		}
	}
}

// TestRows_DoneTaskGetsCheckGlyph pins that a done-status task row
// renders the ✓ glyph. Defensive: today scanProject filters done
// tasks out of row.Tasks, but the styling must be right if behavior
// changes (e.g. "include archived done" toggle).
func TestRows_DoneTaskGetsCheckGlyph(t *testing.T) {
	tr := &taskRow{Slug: "shipped-x-bbbb", Status: "done"}
	out := taskBlockLine(tr, 60, false)
	if !strings.Contains(out, "✓") {
		t.Errorf("done task line should carry ✓ glyph, got: %q", out)
	}
	if !strings.Contains(out, "shipped-x-bbbb") {
		t.Errorf("done task line should still render slug, got: %q", out)
	}
}

// TestRows_DefaultTaskGetsBulletGlyph pins the existing behavior
// stays intact for non-attention tasks: pending tasks still render
// with a • bullet, no ⚠ / ✓.
func TestRows_DefaultTaskGetsBulletGlyph(t *testing.T) {
	tr := &taskRow{Slug: "ordinary-z-cccc", Status: "todo"}
	out := taskBlockLine(tr, 60, false)
	if strings.Contains(out, "⚠") || strings.Contains(out, "✓") {
		t.Errorf("todo task should NOT carry attention/done glyph, got: %q", out)
	}
	if !strings.Contains(out, "•") {
		t.Errorf("todo task should render with • bullet, got: %q", out)
	}
}

// TestRows_BlockedTaskUnderCursorKeepsCursorGlyph pins that the
// cursor's ▶ marker still wins over the status glyph when the
// blocked task is selected — the operator's focus marker shouldn't
// disappear because the row also wants attention. The label text
// still renders in attention color via labelStyle.
func TestRows_BlockedTaskUnderCursorKeepsCursorGlyph(t *testing.T) {
	tr := &taskRow{Slug: "needs-input-aaaa", Status: "blocked"}
	out := taskBlockLine(tr, 60, true)
	if !strings.Contains(out, "▶") {
		t.Errorf("selected blocked task should keep ▶ cursor glyph, got: %q", out)
	}
}

// TestKeyEnter_TaskRow_OpensDetailPanel pins that [⏎] on a real
// task row populates m.detail with the task's identity (project +
// slug) so [a] inside the panel can route to the worker peek.
func TestKeyEnter_TaskRow_OpensDetailPanel(t *testing.T) {
	pdir := withFleetHome(t)
	seedBlockedTask(t, pdir, "fleet", "needs-input-aaaa", tasks.StatusBlocked, "blocked: need clarification")
	stubReadTaskWorker(t, func(project, slug string) (*workers.State, error) {
		return nil, nil
	})

	m := New("test")
	m.width = 130
	m.height = 30
	m.dashboard = scanDashboard(time.Now())
	m.expanded = map[string]bool{"fleet": true}

	// Find the task row index.
	rows := m.dashboardRows()
	taskIdx := -1
	for i, r := range rows {
		if r.kind == rowTask && r.task != nil && r.task.Slug == "needs-input-aaaa" {
			taskIdx = i
			break
		}
	}
	if taskIdx < 0 {
		t.Fatalf("task row not found; rows=%+v", rows)
	}
	m.dashCursor = taskIdx

	updated, _ := m.Update(keyMsg("enter"))
	mm := updated.(Model)
	if mm.detail == nil {
		t.Fatalf("[⏎] on task row should open detail panel, got nil")
	}
	if mm.detail.taskProject != "fleet" {
		t.Errorf("detail.taskProject should be 'fleet', got %q", mm.detail.taskProject)
	}
	if mm.detail.taskSlug != "needs-input-aaaa" {
		t.Errorf("detail.taskSlug should be 'needs-input-aaaa', got %q", mm.detail.taskSlug)
	}
}

// TestTaskDetail_RendersTaskSpec pins that the panel surfaces the
// task's spec body so the operator sees what the task is about.
func TestTaskDetail_RendersTaskSpec(t *testing.T) {
	pdir := withFleetHome(t)
	seedBlockedTask(t, pdir, "fleet", "needs-input-aaaa", tasks.StatusBlocked, "")
	stubReadTaskWorker(t, func(project, slug string) (*workers.State, error) {
		return nil, nil
	})

	body, title := readTaskDetail("fleet", "needs-input-aaaa")
	if !strings.Contains(title, "needs-input-aaaa") {
		t.Errorf("title should mention slug, got %q", title)
	}
	if !strings.Contains(body, "Implement the gizmo") {
		t.Errorf("body should include task spec, got:\n%s", body)
	}
	if !strings.Contains(body, "### Spec") {
		t.Errorf("body should include the Spec section header, got:\n%s", body)
	}
}

// TestTaskDetail_RendersAskingQuestion pins that for a blocked task
// with a worker that wrote BlockedReason, the panel surfaces the
// reason as the "current question". This is the operator's payoff:
// drill in → see the question.
func TestTaskDetail_RendersAskingQuestion(t *testing.T) {
	pdir := withFleetHome(t)
	seedBlockedTask(t, pdir, "fleet", "needs-input-aaaa", tasks.StatusBlocked, "task notes here")
	stubReadTaskWorker(t, func(project, slug string) (*workers.State, error) {
		return &workers.State{
			Slug:          slug,
			Project:       project,
			Phase:         workers.PhaseBlocked,
			PID:           4242,
			UpdatedAt:     time.Now().UTC(),
			BlockedReason: "Need operator to confirm whether to overwrite existing fleetdb",
		}, nil
	})

	body, _ := readTaskDetail("fleet", "needs-input-aaaa")
	if !strings.Contains(body, "Need operator to confirm whether to overwrite existing fleetdb") {
		t.Errorf("body should surface worker BlockedReason as the question, got:\n%s", body)
	}
	if !strings.Contains(body, "blocked_reason") {
		t.Errorf("body should label the question with its source (blocked_reason), got:\n%s", body)
	}
}

// TestTaskDetail_RendersWorkerInfo pins that the panel shows the
// worker handle (slug, phase, PID) when a worker exists. Operator
// uses this to distinguish "live worker, attach to peek" from
// "stale worker record, can't act".
func TestTaskDetail_RendersWorkerInfo(t *testing.T) {
	pdir := withFleetHome(t)
	seedBlockedTask(t, pdir, "fleet", "needs-input-aaaa", tasks.StatusBlocked, "")
	stubReadTaskWorker(t, func(project, slug string) (*workers.State, error) {
		return &workers.State{
			Slug:    slug,
			Project: project,
			Phase:   workers.PhaseBlocked,
			PID:     1234,
		}, nil
	})

	body, _ := readTaskDetail("fleet", "needs-input-aaaa")
	if !strings.Contains(body, "### Worker") {
		t.Errorf("body should include Worker section header, got:\n%s", body)
	}
	if !strings.Contains(body, "needs-input-aaaa") {
		t.Errorf("body should include worker slug, got:\n%s", body)
	}
	if !strings.Contains(body, "1234") {
		t.Errorf("body should include worker PID, got:\n%s", body)
	}
	if !strings.Contains(body, "blocked") {
		t.Errorf("body should include worker phase, got:\n%s", body)
	}
}

// TestTaskDetail_HandlesNoWorker pins the graceful path for a
// blocked task where the worker dir is gone (operator-edited
// tasks.md, or worker was already archived). The panel must show an
// actionable hint, not crash. The recommended command must use the
// real `<slug> <key>=<value>` syntax (codex iter-1 P2 — earlier
// version suggested an invalid `--status pending`).
func TestTaskDetail_HandlesNoWorker(t *testing.T) {
	pdir := withFleetHome(t)
	seedBlockedTask(t, pdir, "fleet", "needs-input-aaaa", tasks.StatusBlocked, "")
	stubReadTaskWorker(t, func(project, slug string) (*workers.State, error) {
		return nil, nil
	})

	body, _ := readTaskDetail("fleet", "needs-input-aaaa")
	if !strings.Contains(body, "no worker state on disk") {
		t.Errorf("body should show no-worker hint for blocked task, got:\n%s", body)
	}
	if !strings.Contains(body, "fleet tasks set") {
		t.Errorf("body should suggest the retry command, got:\n%s", body)
	}
	if !strings.Contains(body, "status=ready") {
		t.Errorf("body should suggest the real `status=<value>` syntax, got:\n%s", body)
	}
	if strings.Contains(body, "--status pending") {
		t.Errorf("body must not suggest the invalid `--status pending` form, got:\n%s", body)
	}
}

// TestTaskDetail_SurfacesReadError pins that a non-ENOENT read
// failure (corrupted state.json, permission denied, etc.) is
// surfaced verbatim instead of being collapsed to "no worker on
// disk". Codex iter-1 P2: collapsing would send the operator down
// the wrong recovery path.
func TestTaskDetail_SurfacesReadError(t *testing.T) {
	pdir := withFleetHome(t)
	seedBlockedTask(t, pdir, "fleet", "broken-task-aaaa", tasks.StatusBlocked, "")
	stubReadTaskWorker(t, func(project, slug string) (*workers.State, error) {
		return nil, errors.New("synthetic: state.json malformed")
	})

	body, _ := readTaskDetail("fleet", "broken-task-aaaa")
	if !strings.Contains(body, "error reading worker state") {
		t.Errorf("body should surface read error, got:\n%s", body)
	}
	if !strings.Contains(body, "synthetic: state.json malformed") {
		t.Errorf("body should include the error verbatim, got:\n%s", body)
	}
	if strings.Contains(body, "no worker state on disk") {
		t.Errorf("read failure must NOT collapse to 'no worker' hint, got:\n%s", body)
	}
}

// TestTaskDetail_TodoTaskSkipsNoWorkerHint pins that the no-worker
// hint is suppressed for todo tasks — those don't yet have a worker
// by design. The hint would be noise.
func TestTaskDetail_TodoTaskSkipsNoWorkerHint(t *testing.T) {
	pdir := withFleetHome(t)
	seedBlockedTask(t, pdir, "fleet", "fresh-task-aaaa", tasks.StatusTodo, "")
	stubReadTaskWorker(t, func(project, slug string) (*workers.State, error) {
		return nil, nil
	})

	body, _ := readTaskDetail("fleet", "fresh-task-aaaa")
	if strings.Contains(body, "no worker state on disk") {
		t.Errorf("todo task should NOT show the no-worker hint (worker is normal-absent), got:\n%s", body)
	}
}

// TestKeyA_TaskRow_AttachesToWorkerPeek pins the task-row [a]
// flow: when a worker exists for the task, [a] opens the worker
// peek panel. Workers in v0.2 are `claude --print` subprocesses (no
// tmux), so "attach" maps to the same path as [a] on a worker row.
func TestKeyA_TaskRow_AttachesToWorkerPeek(t *testing.T) {
	pdir := withFleetHome(t)
	seedBlockedTask(t, pdir, "fleet", "needs-input-aaaa", tasks.StatusBlocked, "")
	// Seed a real worker dir on disk so readWorkerDetail succeeds. The
	// state-stub path drives readTaskDetail; readWorkerDetail reads
	// state.json directly.
	seedWorker(t, pdir, "fleet", "needs-input-aaaa", workers.State{
		Slug:    "needs-input-aaaa",
		Project: "fleet",
		Phase:   workers.PhaseBlocked,
		PID:     4242,
	})
	// readTaskWorker is stubbed to mirror what's on disk so the
	// existence gate sees a worker.
	stubReadTaskWorker(t, func(project, slug string) (*workers.State, error) {
		return &workers.State{
			Slug: slug, Project: project, Phase: workers.PhaseBlocked, PID: 4242,
		}, nil
	})

	m := New("test")
	m.width = 130
	m.height = 30
	m.dashboard = scanDashboard(time.Now())
	m.expanded = map[string]bool{"fleet": true}

	// Land cursor on the task row.
	rows := m.dashboardRows()
	taskIdx := -1
	for i, r := range rows {
		if r.kind == rowTask && r.task != nil && r.task.Slug == "needs-input-aaaa" {
			taskIdx = i
			break
		}
	}
	if taskIdx < 0 {
		t.Fatalf("task row missing; rows=%+v", rows)
	}
	m.dashCursor = taskIdx

	updated, _ := m.Update(keyMsg("a"))
	mm := updated.(Model)
	if mm.detail == nil {
		t.Fatalf("[a] on task row with worker should open peek panel, got nil")
	}
	if !strings.Contains(mm.detail.title, "worker") {
		t.Errorf("panel title should reflect worker peek, got %q", mm.detail.title)
	}
	if mm.flash != nil && mm.flash.isErr {
		t.Errorf("[a] with live worker shouldn't flash an error, got %v", mm.flash)
	}
}

// TestKeyA_TaskRow_NoWorker_ShowsInlineMessage pins the graceful
// path when no worker exists for the task. Operator gets an
// actionable retry hint instead of a silent no-op. The hint must
// use the real `<slug> <key>=<value>` syntax (codex iter-1 P2).
func TestKeyA_TaskRow_NoWorker_ShowsInlineMessage(t *testing.T) {
	pdir := withFleetHome(t)
	seedBlockedTask(t, pdir, "fleet", "stale-task-aaaa", tasks.StatusBlocked, "")
	stubReadTaskWorker(t, func(project, slug string) (*workers.State, error) {
		return nil, nil
	})

	m := New("test")
	m.width = 130
	m.height = 30
	m.dashboard = scanDashboard(time.Now())
	m.expanded = map[string]bool{"fleet": true}

	rows := m.dashboardRows()
	taskIdx := -1
	for i, r := range rows {
		if r.kind == rowTask && r.task != nil && r.task.Slug == "stale-task-aaaa" {
			taskIdx = i
			break
		}
	}
	if taskIdx < 0 {
		t.Fatalf("task row missing; rows=%+v", rows)
	}
	m.dashCursor = taskIdx

	updated, _ := m.Update(keyMsg("a"))
	mm := updated.(Model)
	if mm.flash == nil || !mm.flash.isErr {
		t.Fatalf("[a] without worker should flash an error hint, got %v", mm.flash)
	}
	if !strings.Contains(mm.flash.text, "no worker for task") {
		t.Errorf("flash should mention no-worker, got %q", mm.flash.text)
	}
	if !strings.Contains(mm.flash.text, "fleet tasks set") {
		t.Errorf("flash should suggest the retry command, got %q", mm.flash.text)
	}
	if !strings.Contains(mm.flash.text, "status=ready") {
		t.Errorf("flash should use real `status=<value>` syntax, got %q", mm.flash.text)
	}
	if strings.Contains(mm.flash.text, "--status pending") {
		t.Errorf("flash must not suggest invalid `--status pending`, got %q", mm.flash.text)
	}
}

// TestKeyA_TaskRow_TodoTask_PointsAtPromote pins that [a] on a
// todo task row points at `fleet tasks promote` — the actual
// next-step (the coord auto-dispatches `ready` tasks). Codex iter-3
// P1 fix: an earlier hint pointed at [d], but [d] opens the loose-
// agent repo picker, NOT the per-task dispatch flow. Following [d]
// from a todo task hint would send the operator into the wrong flow.
func TestKeyA_TaskRow_TodoTask_PointsAtPromote(t *testing.T) {
	pdir := withFleetHome(t)
	seedBlockedTask(t, pdir, "fleet", "fresh-task-aaaa", tasks.StatusTodo, "")

	m := New("test")
	m.width = 130
	m.height = 30
	m.dashboard = scanDashboard(time.Now())
	m.expanded = map[string]bool{"fleet": true}

	rows := m.dashboardRows()
	taskIdx := -1
	for i, r := range rows {
		if r.kind == rowTask && r.task != nil && r.task.Slug == "fresh-task-aaaa" {
			taskIdx = i
			break
		}
	}
	if taskIdx < 0 {
		t.Fatalf("task row missing; rows=%+v", rows)
	}
	m.dashCursor = taskIdx

	updated, _ := m.Update(keyMsg("a"))
	mm := updated.(Model)
	if mm.flash == nil || !mm.flash.isErr {
		t.Fatalf("[a] on todo task should flash a hint, got %v", mm.flash)
	}
	if !strings.Contains(mm.flash.text, "fleet tasks promote") {
		t.Errorf("todo flash should suggest `fleet tasks promote`, got %q", mm.flash.text)
	}
	if strings.Contains(mm.flash.text, "[d]") {
		t.Errorf("todo flash must NOT point at [d] (loose-agent picker, wrong flow), got %q", mm.flash.text)
	}
	if strings.Contains(mm.flash.text, "no worker for task") {
		t.Errorf("todo task must NOT use the missing-worker phrasing (it's pre-dispatch, not failure), got %q", mm.flash.text)
	}
}

// TestKeyA_TaskRow_ReadyTask_PointsAtCoord pins that [a] on a ready
// task points at "the coord will pick it up" — the operator's right
// next-step is to verify a coord exists, not to retry-dispatch.
// Codex iter-3 P1.
func TestKeyA_TaskRow_ReadyTask_PointsAtCoord(t *testing.T) {
	pdir := withFleetHome(t)
	seedBlockedTask(t, pdir, "fleet", "ready-task-aaaa", tasks.StatusReady, "")

	m := New("test")
	m.width = 130
	m.height = 30
	m.dashboard = scanDashboard(time.Now())
	m.expanded = map[string]bool{"fleet": true}

	rows := m.dashboardRows()
	taskIdx := -1
	for i, r := range rows {
		if r.kind == rowTask && r.task != nil && r.task.Slug == "ready-task-aaaa" {
			taskIdx = i
			break
		}
	}
	if taskIdx < 0 {
		t.Fatalf("task row missing; rows=%+v", rows)
	}
	m.dashCursor = taskIdx

	updated, _ := m.Update(keyMsg("a"))
	mm := updated.(Model)
	if mm.flash == nil || !mm.flash.isErr {
		t.Fatalf("[a] on ready task should flash a hint, got %v", mm.flash)
	}
	if !strings.Contains(mm.flash.text, "ready") {
		t.Errorf("ready flash should mention status=ready, got %q", mm.flash.text)
	}
	if !strings.Contains(mm.flash.text, "coord") {
		t.Errorf("ready flash should reference the coord, got %q", mm.flash.text)
	}
	if strings.Contains(mm.flash.text, "[d]") {
		t.Errorf("ready flash must NOT point at [d], got %q", mm.flash.text)
	}
}

// TestKeyA_DetailPanel_LiveStatusOverridesCachedStatus pins the
// codex iter-3 P2 fix: a task that transitioned ready → in-progress
// between snapshots must route on the LIVE status (not the cached
// detailView.taskStatus). Without this, [a] in the detail panel
// would dead-end on the stale "ready" branch even though the
// active worker now exists.
func TestKeyA_DetailPanel_LiveStatusOverridesCachedStatus(t *testing.T) {
	pdir := withFleetHome(t)
	// On-disk task is in-progress; the cached detail view says ready.
	seedBlockedTask(t, pdir, "fleet", "race-task-aaaa", tasks.StatusInProgress, "")
	stubReadTaskWorker(t, func(project, slug string) (*workers.State, error) {
		return &workers.State{
			Slug: slug, Project: project, Phase: workers.PhaseTDDGreen, PID: 9999,
		}, nil
	})
	// Seed a worker dir so readWorkerDetail succeeds.
	seedWorker(t, pdir, "fleet", "race-task-aaaa", workers.State{
		Slug: "race-task-aaaa", Project: "fleet", Phase: workers.PhaseTDDGreen, PID: 9999,
	})

	m := New("test")
	m.width = 130
	m.height = 30
	m.detail = &detailView{
		title:       "task: fleet/race-task-aaaa",
		body:        "(prepopulated)",
		taskProject: "fleet",
		taskSlug:    "race-task-aaaa",
		taskStatus:  "ready", // stale cached snapshot value
	}

	updated, _ := m.Update(keyMsg("a"))
	mm := updated.(Model)
	// Live status (in-progress) drives routing → worker peek opens.
	if mm.detail == nil {
		t.Fatalf("[a] should open worker peek using live status, got nil panel")
	}
	if !strings.Contains(mm.detail.title, "worker") {
		t.Errorf("panel should switch to worker peek, got %q", mm.detail.title)
	}
}

// TestKeyA_TaskRow_ArchivedWorker_OpensPeek pins the codex iter-3
// P2 fix: when the active worker dir was archived between snapshots
// but the task row still shows, [a] must surface the archived
// state.json + output.log via readWorkerDetail's archive fallback —
// not flash the no-worker hint.
func TestKeyA_TaskRow_ArchivedWorker_OpensPeek(t *testing.T) {
	pdir := withFleetHome(t)
	seedBlockedTask(t, pdir, "fleet", "archived-task-aaaa", tasks.StatusBlocked, "")
	// readTaskWorker reports ENOENT (active dir is gone) but the
	// archive presence stub says we have one.
	stubReadTaskWorker(t, func(project, slug string) (*workers.State, error) {
		return nil, nil
	})
	origArch := taskWorkerArchiveExists
	taskWorkerArchiveExists = func(project, slug string) bool {
		return project == "fleet" && slug == "archived-task-aaaa"
	}
	t.Cleanup(func() { taskWorkerArchiveExists = origArch })
	// Seed an actual archive dir on disk so readWorkerDetail's
	// fallback finds something to render. Without a real dir, the
	// peek body would just say "no state.json yet" — still a peek
	// (no flash), which is what the contract requires.
	archDir := filepath.Join(pdir, "fleet", "workers", "archive", "archived-task-aaaa-20260101-000000")
	if err := stateMkdir(archDir); err != nil {
		t.Fatal(err)
	}
	stateJSON := []byte(`{"slug":"archived-task-aaaa","project":"fleet","phase":"done"}`)
	if err := os.WriteFile(filepath.Join(archDir, "state.json"), stateJSON, 0o644); err != nil {
		t.Fatal(err)
	}

	m := New("test")
	m.width = 130
	m.height = 30
	m.dashboard = scanDashboard(time.Now())
	m.expanded = map[string]bool{"fleet": true}

	rows := m.dashboardRows()
	taskIdx := -1
	for i, r := range rows {
		if r.kind == rowTask && r.task != nil && r.task.Slug == "archived-task-aaaa" {
			taskIdx = i
			break
		}
	}
	if taskIdx < 0 {
		t.Fatalf("task row missing; rows=%+v", rows)
	}
	m.dashCursor = taskIdx

	updated, _ := m.Update(keyMsg("a"))
	mm := updated.(Model)
	if mm.flash != nil && mm.flash.isErr {
		t.Errorf("archived worker should NOT flash no-worker, got %q", mm.flash.text)
	}
	if mm.detail == nil {
		t.Fatalf("[a] with archived worker should open peek, got nil")
	}
	if !strings.Contains(mm.detail.title, "archived") {
		t.Errorf("title should reflect archived peek, got %q", mm.detail.title)
	}
}

// TestKeyA_TaskRow_ReadError_SurfacesError pins that a non-ENOENT
// worker-state read failure is surfaced as a read error, NOT
// collapsed to "no worker". Codex iter-1 P2 fix.
func TestKeyA_TaskRow_ReadError_SurfacesError(t *testing.T) {
	pdir := withFleetHome(t)
	seedBlockedTask(t, pdir, "fleet", "broken-task-aaaa", tasks.StatusBlocked, "")
	stubReadTaskWorker(t, func(project, slug string) (*workers.State, error) {
		return nil, errors.New("synthetic: permission denied on state.json")
	})

	m := New("test")
	m.width = 130
	m.height = 30
	m.dashboard = scanDashboard(time.Now())
	m.expanded = map[string]bool{"fleet": true}

	rows := m.dashboardRows()
	taskIdx := -1
	for i, r := range rows {
		if r.kind == rowTask && r.task != nil && r.task.Slug == "broken-task-aaaa" {
			taskIdx = i
			break
		}
	}
	if taskIdx < 0 {
		t.Fatalf("task row missing; rows=%+v", rows)
	}
	m.dashCursor = taskIdx

	updated, _ := m.Update(keyMsg("a"))
	mm := updated.(Model)
	if mm.flash == nil || !mm.flash.isErr {
		t.Fatalf("[a] with worker read error should flash, got %v", mm.flash)
	}
	if !strings.Contains(mm.flash.text, "unreadable") {
		t.Errorf("flash should label the failure as unreadable state, got %q", mm.flash.text)
	}
	if !strings.Contains(mm.flash.text, "synthetic: permission denied") {
		t.Errorf("flash should include the underlying error, got %q", mm.flash.text)
	}
	if strings.Contains(mm.flash.text, "no worker for task") {
		t.Errorf("read error must NOT collapse to 'no worker' phrasing, got %q", mm.flash.text)
	}
}

// TestKeyA_DetailPanel_RoutesToWorkerPeek pins that pressing [a]
// while a task detail panel is open routes to the worker peek
// instead of dismissing the panel. This is the "drill in → respond"
// flow: operator opens detail, reads question, presses [a] to attach.
func TestKeyA_DetailPanel_RoutesToWorkerPeek(t *testing.T) {
	pdir := withFleetHome(t)
	seedBlockedTask(t, pdir, "fleet", "needs-input-aaaa", tasks.StatusBlocked, "")
	seedWorker(t, pdir, "fleet", "needs-input-aaaa", workers.State{
		Slug: "needs-input-aaaa", Project: "fleet",
		Phase: workers.PhaseBlocked, PID: 4242,
	})
	stubReadTaskWorker(t, func(project, slug string) (*workers.State, error) {
		return &workers.State{
			Slug: slug, Project: project, Phase: workers.PhaseBlocked, PID: 4242,
		}, nil
	})

	m := New("test")
	m.width = 130
	m.height = 30
	m.detail = &detailView{
		title:       "task: fleet/needs-input-aaaa",
		body:        "(prepopulated panel)",
		taskProject: "fleet",
		taskSlug:    "needs-input-aaaa",
	}

	updated, _ := m.Update(keyMsg("a"))
	mm := updated.(Model)
	if mm.detail == nil {
		t.Fatalf("[a] in task detail should open worker peek, got nil panel")
	}
	if !strings.Contains(mm.detail.title, "worker") {
		t.Errorf("panel title should switch to worker peek, got %q", mm.detail.title)
	}
}

// TestKeyA_DetailPanel_NoWorker_DismissesWithFlash pins that the
// detail-panel [a] interceptor still surfaces the no-worker hint
// when the worker is gone. Panel dismisses (so the flash isn't
// hidden behind a stale overlay).
func TestKeyA_DetailPanel_NoWorker_DismissesWithFlash(t *testing.T) {
	stubReadTaskWorker(t, func(project, slug string) (*workers.State, error) {
		return nil, nil
	})

	m := New("test")
	m.width = 130
	m.height = 30
	m.detail = &detailView{
		title:       "task: fleet/stale",
		body:        "(prepopulated panel)",
		taskProject: "fleet",
		taskSlug:    "stale",
	}

	updated, _ := m.Update(keyMsg("a"))
	mm := updated.(Model)
	if mm.detail != nil {
		t.Errorf("no-worker [a] from panel should dismiss the panel, got %+v", mm.detail)
	}
	if mm.flash == nil || !mm.flash.isErr {
		t.Fatalf("expected error flash, got %v", mm.flash)
	}
	if !strings.Contains(mm.flash.text, "no worker for task") {
		t.Errorf("flash should mention no-worker, got %q", mm.flash.text)
	}
	if !strings.Contains(mm.flash.text, "status=ready") {
		t.Errorf("flash should suggest the real `status=<value>` syntax, got %q", mm.flash.text)
	}
}

// TestKeyA_DetailPanel_TodoTask_PointsAtPromote pins that pressing
// [a] in a todo task's detail panel flashes the promote hint
// (NOT the missing-worker recovery command, NOT the [d] hint).
// Codex iter-3 P1: [d] is the loose-agent picker, not a per-task
// dispatch — promote is the correct next step.
func TestKeyA_DetailPanel_TodoTask_PointsAtPromote(t *testing.T) {
	// liveTaskStatus stub: no on-disk task → returns "" so the
	// cached taskStatus drives routing. Tests for the live-override
	// path live separately.
	origLive := liveTaskStatus
	liveTaskStatus = func(project, slug string) string { return "" }
	t.Cleanup(func() { liveTaskStatus = origLive })

	m := New("test")
	m.width = 130
	m.height = 30
	m.detail = &detailView{
		title:       "task: fleet/fresh-task-aaaa",
		body:        "(prepopulated panel)",
		taskProject: "fleet",
		taskSlug:    "fresh-task-aaaa",
		taskStatus:  "todo",
	}

	updated, _ := m.Update(keyMsg("a"))
	mm := updated.(Model)
	if mm.detail != nil {
		t.Errorf("[a] on todo-task panel should dismiss, got %+v", mm.detail)
	}
	if mm.flash == nil || !mm.flash.isErr {
		t.Fatalf("[a] should flash a hint, got %v", mm.flash)
	}
	if !strings.Contains(mm.flash.text, "fleet tasks promote") {
		t.Errorf("todo flash should suggest promote, got %q", mm.flash.text)
	}
	if strings.Contains(mm.flash.text, "[d]") {
		t.Errorf("todo flash must NOT point at [d] (loose-agent picker), got %q", mm.flash.text)
	}
	if strings.Contains(mm.flash.text, "status=ready") {
		t.Errorf("todo-task panel must NOT suggest the blocked recovery command, got %q", mm.flash.text)
	}
}

// TestKeyEnter_TaskRow_PreservesStatusOnDetail pins that opening a
// task detail captures the row's status so the panel-side [a]
// handler can apply the same status-aware routing as the row-side
// handler.
func TestKeyEnter_TaskRow_PreservesStatusOnDetail(t *testing.T) {
	pdir := withFleetHome(t)
	seedBlockedTask(t, pdir, "fleet", "fresh-task-aaaa", tasks.StatusReady, "")
	stubReadTaskWorker(t, func(project, slug string) (*workers.State, error) {
		return nil, nil
	})

	m := New("test")
	m.width = 130
	m.height = 30
	m.dashboard = scanDashboard(time.Now())
	m.expanded = map[string]bool{"fleet": true}

	rows := m.dashboardRows()
	taskIdx := -1
	for i, r := range rows {
		if r.kind == rowTask && r.task != nil && r.task.Slug == "fresh-task-aaaa" {
			taskIdx = i
			break
		}
	}
	if taskIdx < 0 {
		t.Fatalf("task row missing; rows=%+v", rows)
	}
	m.dashCursor = taskIdx

	updated, _ := m.Update(keyMsg("enter"))
	mm := updated.(Model)
	if mm.detail == nil {
		t.Fatalf("[⏎] should open detail panel")
	}
	if mm.detail.taskStatus != "ready" {
		t.Errorf("detail.taskStatus should reflect row status, got %q", mm.detail.taskStatus)
	}
}

// TestKeyA_DetailPanel_NonTask_StillDismisses pins the regression
// case: pressing [a] in a worker / agent / help panel should still
// dismiss the panel as before — the task-aware re-route only fires
// when taskSlug is set.
func TestKeyA_DetailPanel_NonTask_StillDismisses(t *testing.T) {
	m := New("test")
	m.width = 130
	m.height = 30
	m.detail = &detailView{
		title: "worker: fleet/somewhere",
		body:  "(worker panel)",
	}

	updated, _ := m.Update(keyMsg("a"))
	mm := updated.(Model)
	if mm.detail != nil {
		t.Errorf("non-task panel should dismiss on any key, got %+v", mm.detail)
	}
}

// TestDashboard_AttentionCount_DecrementsOnTaskStatusChange pins the
// regression: after a task transitions out of blocked, a fresh
// scanDashboard must drop the ⚠ count. Without this the project
// chip would lie about the attention state until the operator
// restarts.
func TestDashboard_AttentionCount_DecrementsOnTaskStatusChange(t *testing.T) {
	pdir := withFleetHome(t)
	// Start with a blocked task — Attention should be 1.
	seedBlockedTask(t, pdir, "fleet", "needs-input-aaaa", tasks.StatusBlocked, "")
	snap := scanDashboard(time.Now())
	if snap == nil || len(snap.Projects) != 1 {
		t.Fatalf("expected 1 project, got %+v", snap)
	}
	if snap.Projects[0].Attention != 1 {
		t.Errorf("blocked task should yield Attention=1, got %d", snap.Projects[0].Attention)
	}
	if snap.Projects[0].Counts.Blocked != 1 {
		t.Errorf("blocked task should yield Counts.Blocked=1, got %d", snap.Projects[0].Counts.Blocked)
	}

	// Transition the task out of blocked → in-progress. Re-write
	// tasks.md and re-scan; the new snapshot must show 0 attention.
	seedBlockedTask(t, pdir, "fleet", "needs-input-aaaa", tasks.StatusInProgress, "")
	snap2 := scanDashboard(time.Now())
	if snap2 == nil || len(snap2.Projects) != 1 {
		t.Fatalf("expected 1 project after transition, got %+v", snap2)
	}
	if snap2.Projects[0].Attention != 0 {
		t.Errorf("after transition, Attention should be 0, got %d", snap2.Projects[0].Attention)
	}
	if snap2.Projects[0].Counts.Blocked != 0 {
		t.Errorf("after transition, Counts.Blocked should be 0, got %d", snap2.Projects[0].Counts.Blocked)
	}
	if snap2.Projects[0].Counts.InProgress != 1 {
		t.Errorf("after transition, Counts.InProgress should be 1, got %d", snap2.Projects[0].Counts.InProgress)
	}
}

// TestExpandThenAttachFlow is the end-to-end interaction test
// matching the issue #75 acceptance: cursor on project → [⏎]
// expand → [j] to task → [⏎] open detail → [a] attach.
func TestExpandThenAttachFlow(t *testing.T) {
	pdir := withFleetHome(t)
	seedBlockedTask(t, pdir, "fleet", "needs-input-aaaa", tasks.StatusBlocked, "")
	seedWorker(t, pdir, "fleet", "needs-input-aaaa", workers.State{
		Slug: "needs-input-aaaa", Project: "fleet",
		Phase: workers.PhaseBlocked, PID: 4242,
		BlockedReason: "operator-clarification-needed",
	})
	stubReadTaskWorker(t, func(project, slug string) (*workers.State, error) {
		return &workers.State{
			Slug: slug, Project: project, Phase: workers.PhaseBlocked, PID: 4242,
			BlockedReason: "operator-clarification-needed",
		}, nil
	})

	m := New("test")
	m.width = 130
	m.height = 30
	m.dashboard = scanDashboard(time.Now())

	// 1. Land cursor on the project row.
	rows := m.dashboardRows()
	projectIdx := -1
	for i, r := range rows {
		if r.kind == rowProject && r.project != nil && r.project.Name == "fleet" {
			projectIdx = i
			break
		}
	}
	if projectIdx < 0 {
		t.Fatalf("project row missing; rows=%+v", rows)
	}
	m.dashCursor = projectIdx

	// 2. [⏎] expands.
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	mm := updated.(Model)

	// 3. [j] onto the task row.
	updated, _ = mm.Update(keyMsg("j"))
	mm = updated.(Model)
	row := mm.selectedRow()
	if row == nil || row.kind != rowTask {
		t.Fatalf("[j] after expand should land on task row, got %+v", row)
	}

	// 4. [⏎] opens the detail panel.
	updated, _ = mm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	mm = updated.(Model)
	if mm.detail == nil {
		t.Fatalf("[⏎] on task should open detail panel")
	}
	if mm.detail.taskSlug != "needs-input-aaaa" {
		t.Errorf("detail panel should bind task slug, got %q", mm.detail.taskSlug)
	}
	if !strings.Contains(mm.detail.body, "operator-clarification-needed") {
		t.Errorf("detail body should surface BlockedReason, got:\n%s", mm.detail.body)
	}

	// 5. [a] from the panel routes to worker peek.
	updated, _ = mm.Update(keyMsg("a"))
	mm = updated.(Model)
	if mm.detail == nil {
		t.Fatalf("[a] from task panel should open worker peek")
	}
	if !strings.Contains(mm.detail.title, "worker") {
		t.Errorf("panel should be the worker peek, got %q", mm.detail.title)
	}
}
