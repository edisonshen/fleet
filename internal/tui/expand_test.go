// Tests for the project-row [⏎] expand/collapse inline task list
// (issue #59). Covers: toggle on enter, fetch from real tasks.md,
// synthetic-project empty-state, refresh-survives-tick, j/k traverses
// task sub-rows, collapsed projects hide their tasks.
package tui

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/tasks"
)

// TestExpand_EnterTogglesProjectExpansion pins the core behavior:
// [⏎] on a project row toggles m.expanded[name]. Second [⏎] on the
// same project collapses back. Cursor stays on the project row so
// j/k can immediately walk into the just-revealed task sub-rows.
func TestExpand_EnterTogglesProjectExpansion(t *testing.T) {
	pdir := withFleetHome(t)
	seedTasks(t, pdir, "fleet", TaskCounts{Todo: 2})

	m := New("test")
	m.width = 130
	m.height = 30
	m.dashboard = scanDashboard(time.Now())
	// Position cursor on the fleet project row.
	rows := m.dashboardRows()
	projectIdx := -1
	for i, r := range rows {
		if r.kind == rowProject && r.project != nil && r.project.Name == "fleet" {
			projectIdx = i
			break
		}
	}
	if projectIdx < 0 {
		t.Fatalf("project row not found in initial rows: %+v", rows)
	}
	m.dashCursor = projectIdx

	// Pre-condition: not expanded → no task rows under fleet.
	for _, r := range m.dashboardRows() {
		if r.kind == rowTask && r.parentProject == "fleet" {
			t.Fatalf("collapsed project must not emit task rows; got: %+v", r)
		}
	}

	// First [⏎] expands.
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	mm := updated.(Model)
	if !mm.expanded["fleet"] {
		t.Errorf("first [⏎] should set expanded[fleet]=true, got %v", mm.expanded)
	}
	taskRowsAfterFirst := 0
	for _, r := range mm.dashboardRows() {
		if r.kind == rowTask && r.parentProject == "fleet" {
			taskRowsAfterFirst++
		}
	}
	if taskRowsAfterFirst != 2 {
		t.Errorf("expanded fleet should emit 2 task rows, got %d", taskRowsAfterFirst)
	}

	// Cursor must still be on the project row (not pushed to task).
	if got := mm.selectedRow(); got == nil || got.kind != rowProject {
		t.Errorf("cursor should remain on project row after expand, got %+v", got)
	}

	// Second [⏎] on same project collapses.
	updated2, _ := mm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	mm2 := updated2.(Model)
	if mm2.expanded["fleet"] {
		t.Errorf("second [⏎] should collapse, got expanded=%v", mm2.expanded)
	}
	for _, r := range mm2.dashboardRows() {
		if r.kind == rowTask && r.parentProject == "fleet" {
			t.Errorf("collapsed project must not emit task rows; got: %+v", r)
		}
	}
}

// TestExpand_TaskTitlesRenderInline pins that real task titles
// (slugs) render as inline rows under the expanded project. The
// view output must contain each slug on its own line under the
// project header. Issue #59 spec: titles only.
func TestExpand_TaskTitlesRenderInline(t *testing.T) {
	pdir := withFleetHome(t)
	// Seed two tasks with known slugs we can grep for in the view.
	dir := filepath.Join(pdir, "fleet")
	if err := stateMkdir(dir); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	f := &tasks.File{Schema: tasks.SchemaVersion}
	for _, slug := range []string{"add-readme-aaaa", "fix-toolbar-bbbb"} {
		if err := f.Add(&tasks.Task{
			Slug:     slug,
			Status:   tasks.StatusTodo,
			Priority: tasks.PriorityP2,
			Created:  now,
			Updated:  now,
			Spec:     "spec",
		}); err != nil {
			t.Fatalf("add %s: %v", slug, err)
		}
	}
	if err := tasks.Write(filepath.Join(dir, "tasks.md"), f); err != nil {
		t.Fatal(err)
	}

	m := New("test")
	m.width = 140
	m.height = 30
	m.dashboard = scanDashboard(time.Now())
	m.expanded = map[string]bool{"fleet": true}

	out := m.View()
	for _, want := range []string{"add-readme-aaaa", "fix-toolbar-bbbb"} {
		if !strings.Contains(out, want) {
			t.Errorf("expanded view should render task slug %q, got:\n%s", want, out)
		}
	}
}

// TestExpand_SyntheticProjectShowsEmptyHint pins the spec for
// projects without a v0.2 dir: expansion shows the
// "no tasks yet — `fleet init` to create tasks.md" hint. We
// trigger this by having m.records carry a Project tag with no
// matching ~/.fleet/projects/<tag>/ dir, which goes through
// unifiedProjects' synthetic branch.
func TestExpand_SyntheticProjectShowsEmptyHint(t *testing.T) {
	withFleetHome(t)

	m := New("test")
	m.width = 140
	m.height = 30
	m.records = []*agent.Record{
		{
			SchemaVersion: 1,
			ID:            "agent01",
			TmuxSession:   "fleet-agent01",
			Project:       "scratch",
			TaskID:        "scratch-task",
			SpawnedAt:     time.Now().UTC(),
		},
	}
	m.expanded = map[string]bool{"scratch": true}

	out := m.View()
	if !strings.Contains(out, "no tasks yet") {
		t.Errorf("synthetic expanded project should render empty-state hint, got:\n%s", out)
	}
	if !strings.Contains(out, "fleet init") {
		t.Errorf("empty-state hint should mention `fleet init`, got:\n%s", out)
	}
}

// TestExpand_SurvivesDashboardRefresh pins that a dashboardMsg tick
// (1s polling fallback) does NOT auto-collapse the operator's
// expansion. m.expanded persists across the reducer.
func TestExpand_SurvivesDashboardRefresh(t *testing.T) {
	pdir := withFleetHome(t)
	seedTasks(t, pdir, "fleet", TaskCounts{Todo: 1})

	m := New("test")
	m.width = 130
	m.height = 30
	m.dashboard = scanDashboard(time.Now())
	m.expanded = map[string]bool{"fleet": true}

	// Simulate a refresh tick — fresh snapshot, same project.
	snap := scanDashboard(time.Now())
	updated, _ := m.Update(dashboardMsg{snap: snap})
	mm := updated.(Model)
	if !mm.expanded["fleet"] {
		t.Errorf("dashboardMsg refresh must preserve expanded state, got %v", mm.expanded)
	}
	// Task row should still be in the new dashboardRows().
	saw := false
	for _, r := range mm.dashboardRows() {
		if r.kind == rowTask && r.parentProject == "fleet" {
			saw = true
			break
		}
	}
	if !saw {
		t.Errorf("expanded project's task row missing after refresh; rows=%+v", mm.dashboardRows())
	}
}

// TestExpand_JKTraversesTaskSubrows pins that j/k cursor walks
// through the project header AND its expanded task sub-rows. After
// landing on a project row and expanding, [j] should move to the
// first task; another [j] to the second; etc.
func TestExpand_JKTraversesTaskSubrows(t *testing.T) {
	pdir := withFleetHome(t)
	seedTasks(t, pdir, "fleet", TaskCounts{Todo: 3})

	m := New("test")
	m.width = 130
	m.height = 30
	m.dashboard = scanDashboard(time.Now())
	// Cursor on project row.
	rows := m.dashboardRows()
	projectIdx := -1
	for i, r := range rows {
		if r.kind == rowProject && r.project.Name == "fleet" {
			projectIdx = i
			break
		}
	}
	if projectIdx < 0 {
		t.Fatalf("project row missing")
	}
	m.dashCursor = projectIdx

	// Expand.
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	mm := updated.(Model)

	// j → first task sub-row.
	updated, _ = mm.Update(keyMsg("j"))
	mm = updated.(Model)
	row := mm.selectedRow()
	if row == nil || row.kind != rowTask {
		t.Fatalf("j after expand should land on task row, got %+v", row)
	}

	// j → second task sub-row.
	updated, _ = mm.Update(keyMsg("j"))
	mm = updated.(Model)
	row = mm.selectedRow()
	if row == nil || row.kind != rowTask {
		t.Errorf("second j should still be on a task row, got %+v", row)
	}

	// k twice → back on project row.
	updated, _ = mm.Update(keyMsg("k"))
	mm = updated.(Model)
	updated, _ = mm.Update(keyMsg("k"))
	mm = updated.(Model)
	row = mm.selectedRow()
	if row == nil || row.kind != rowProject {
		t.Errorf("k k should walk back to project row, got %+v", row)
	}
}

// TestExpand_CollapsedHidesTaskRows pins that a project NOT in
// m.expanded emits zero rowTask entries from dashboardRows(),
// even when the project has tasks on disk.
func TestExpand_CollapsedHidesTaskRows(t *testing.T) {
	pdir := withFleetHome(t)
	seedTasks(t, pdir, "alpha", TaskCounts{Todo: 5})
	seedTasks(t, pdir, "zulu", TaskCounts{Todo: 3})

	m := New("test")
	m.width = 130
	m.height = 30
	m.dashboard = scanDashboard(time.Now())
	// No m.expanded set → all collapsed.

	for _, r := range m.dashboardRows() {
		if r.kind == rowTask {
			t.Errorf("collapsed-by-default must not emit task rows; got %+v", r)
		}
	}
}

// TestExpand_CapsAtMaxWithMoreTail pins that a project with more
// than maxExpandedTasks visible tasks renders only the first
// maxExpandedTasks and a "+N more" tail row.
func TestExpand_CapsAtMaxWithMoreTail(t *testing.T) {
	pdir := withFleetHome(t)
	// Seed 12 todo tasks — over the cap of 10.
	seedTasks(t, pdir, "fleet", TaskCounts{Todo: 12})

	m := New("test")
	m.width = 140
	m.height = 30
	m.dashboard = scanDashboard(time.Now())
	m.expanded = map[string]bool{"fleet": true}

	rows := m.dashboardRows()
	taskRows := 0
	moreRows := 0
	moreCount := 0
	for _, r := range rows {
		if r.kind != rowTask || r.parentProject != "fleet" {
			continue
		}
		if r.task != nil && r.task.More > 0 {
			moreRows++
			moreCount = r.task.More
			continue
		}
		taskRows++
	}
	if taskRows != maxExpandedTasks {
		t.Errorf("visible task rows = %d, want %d", taskRows, maxExpandedTasks)
	}
	if moreRows != 1 {
		t.Errorf("expected exactly 1 +more tail row, got %d", moreRows)
	}
	if moreCount != 12-maxExpandedTasks {
		t.Errorf("+more count = %d, want %d", moreCount, 12-maxExpandedTasks)
	}

	// View should render "+N more".
	out := m.View()
	if !strings.Contains(out, "+2 more") {
		t.Errorf("view should render '+2 more' tail, got:\n%s", out)
	}
}

// TestExpand_SearchKeepsTaskVisibleWithoutExpansion pins the search
// override: when the active filter matches a task slug under a
// collapsed project, the task row still renders so the operator
// can navigate to it. Without this carve-out, /<slug> would surface
// a parent-only row.
func TestExpand_SearchKeepsTaskVisibleWithoutExpansion(t *testing.T) {
	pdir := withFleetHome(t)
	dir := filepath.Join(pdir, "alpha")
	if err := stateMkdir(dir); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	f := &tasks.File{Schema: tasks.SchemaVersion}
	if err := f.Add(&tasks.Task{
		Slug:     "find-me-abcd",
		Status:   tasks.StatusTodo,
		Priority: tasks.PriorityP2,
		Created:  now,
		Updated:  now,
		Spec:     "needle",
	}); err != nil {
		t.Fatal(err)
	}
	if err := tasks.Write(filepath.Join(dir, "tasks.md"), f); err != nil {
		t.Fatal(err)
	}

	m := New("test")
	m.dashboard = scanDashboard(time.Now())
	// Filter on the task slug. Project NOT expanded.
	m.searchFilter = "find-me"

	rows := m.dashboardRows()
	sawTask := false
	for _, r := range rows {
		if r.kind == rowTask && r.task != nil && r.task.Slug == "find-me-abcd" {
			sawTask = true
		}
	}
	if !sawTask {
		t.Errorf("search filter on task slug must surface the task row even when project is collapsed; rows=%+v", rows)
	}
}
