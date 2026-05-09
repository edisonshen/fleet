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

// TestExpand_EnterOnSyntheticMarkerIsNoop pins that pressing [⏎]
// on a synthetic Empty/More marker row does NOT open a detail
// modal (those rows have no slug — readTaskDetail("") would
// render a bogus "task not found" panel).
func TestExpand_EnterOnSyntheticMarkerIsNoop(t *testing.T) {
	withFleetHome(t)
	// Synthetic project from an agent record → expansion emits a
	// single Empty marker row.
	m := New("test")
	m.width = 130
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
	// Find the marker row.
	rows := m.dashboardRows()
	markerIdx := -1
	for i, r := range rows {
		if r.kind == rowTask && r.task != nil && r.task.Empty {
			markerIdx = i
			break
		}
	}
	if markerIdx < 0 {
		t.Fatalf("synthetic Empty marker row missing from rows: %+v", rows)
	}
	m.dashCursor = markerIdx

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	mm := updated.(Model)
	if mm.detail != nil {
		t.Errorf("[⏎] on Empty marker should not open detail, got %+v", mm.detail)
	}
}

// TestExpand_EnterOnMoreMarkerIsNoop pins the same no-op behavior
// for the "+N more" tail row.
func TestExpand_EnterOnMoreMarkerIsNoop(t *testing.T) {
	pdir := withFleetHome(t)
	// 12 tasks → 10 visible + "+2 more" tail.
	seedTasks(t, pdir, "fleet", TaskCounts{Todo: 12})

	m := New("test")
	m.width = 140
	m.height = 30
	m.dashboard = scanDashboard(time.Now())
	m.expanded = map[string]bool{"fleet": true}

	rows := m.dashboardRows()
	moreIdx := -1
	for i, r := range rows {
		if r.kind == rowTask && r.task != nil && r.task.More > 0 {
			moreIdx = i
			break
		}
	}
	if moreIdx < 0 {
		t.Fatalf("+more tail row missing from rows: %+v", rows)
	}
	m.dashCursor = moreIdx

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	mm := updated.(Model)
	if mm.detail != nil {
		t.Errorf("[⏎] on +more marker should not open detail, got %+v", mm.detail)
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

// TestRows_HistoryGroup_CollapsedByDefault — issue #101: when a
// project has done/abandoned tasks, an expanded project block shows
// the active tasks inline AND a `─── N done ───` separator below.
// History tasks themselves stay hidden until the operator [enter]s
// the separator.
func TestRows_HistoryGroup_CollapsedByDefault(t *testing.T) {
	m := New("test")
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{
			{Name: "fleet", RepoSlug: "fleet", Tasks: []*taskRow{
				{Slug: "active-1", Status: "todo"},
				{Slug: "shipped-1", Status: "done", PRURL: "https://github.com/x/y/pull/42"},
				{Slug: "abandoned-1", Status: "abandoned"},
			}},
		},
	}
	m.expanded = map[string]bool{"fleet": true}

	rows := m.dashboardRows()
	var seenSeparator, seenHistoryTask, seenActiveTask bool
	for _, r := range rows {
		if r.kind == rowSeparator && r.separator != nil && r.separator.kind == separatorHistory {
			seenSeparator = true
			if r.separator.count != 2 {
				t.Errorf("history separator count=%d; want 2", r.separator.count)
			}
			if r.separator.expanded {
				t.Errorf("default history separator should be collapsed")
			}
			if r.separator.project != "fleet" {
				t.Errorf("history separator project=%q; want fleet", r.separator.project)
			}
		}
		if r.kind == rowTask && r.task != nil {
			switch r.task.Slug {
			case "active-1":
				seenActiveTask = true
			case "shipped-1", "abandoned-1":
				seenHistoryTask = true
			}
		}
	}
	if !seenActiveTask {
		t.Errorf("active task should still render inline")
	}
	if !seenSeparator {
		t.Errorf("history separator should render when project has done/abandoned tasks")
	}
	if seenHistoryTask {
		t.Errorf("history tasks should NOT render when separator is collapsed")
	}
}

// TestRows_HistoryGroup_ExpandedShowsTasks — once the operator opens
// the history group via [enter], done + abandoned task rows render
// below the separator.
func TestRows_HistoryGroup_ExpandedShowsTasks(t *testing.T) {
	m := New("test")
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{
			{Name: "fleet", RepoSlug: "fleet", Tasks: []*taskRow{
				{Slug: "active-1", Status: "todo"},
				{Slug: "shipped-1", Status: "done", PRURL: "https://github.com/x/y/pull/42"},
				{Slug: "abandoned-1", Status: "abandoned"},
			}},
		},
	}
	m.expanded = map[string]bool{"fleet": true}
	m.historyExpanded = map[string]bool{"fleet": true}

	rows := m.dashboardRows()
	var seenShipped, seenAbandoned bool
	for _, r := range rows {
		if r.kind == rowTask && r.task != nil {
			if r.task.Slug == "shipped-1" {
				seenShipped = true
			}
			if r.task.Slug == "abandoned-1" {
				seenAbandoned = true
			}
		}
	}
	if !seenShipped {
		t.Errorf("expanded history should show done task")
	}
	if !seenAbandoned {
		t.Errorf("expanded history should show abandoned task")
	}
}

// TestRows_HistoryGroup_NoSeparatorWhenAllActive — projects without
// any done/abandoned tasks must NOT render a separator.
func TestRows_HistoryGroup_NoSeparatorWhenAllActive(t *testing.T) {
	m := New("test")
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{
			{Name: "fleet", RepoSlug: "fleet", Tasks: []*taskRow{
				{Slug: "active-1", Status: "todo"},
				{Slug: "active-2", Status: "in-progress"},
			}},
		},
	}
	m.expanded = map[string]bool{"fleet": true}

	for _, r := range m.dashboardRows() {
		if r.kind == rowSeparator && r.separator != nil && r.separator.kind == separatorHistory {
			t.Fatalf("no history separator should appear; got %+v", r.separator)
		}
	}
}

// TestRows_HistoryGroup_HiddenWhenProjectCollapsed — history group
// only renders inside an expanded project block. When the project
// header is collapsed (no [enter] yet), neither active nor history
// tasks render.
func TestRows_HistoryGroup_HiddenWhenProjectCollapsed(t *testing.T) {
	m := New("test")
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{
			{Name: "fleet", RepoSlug: "fleet", Tasks: []*taskRow{
				{Slug: "active-1", Status: "todo"},
				{Slug: "shipped-1", Status: "done"},
			}},
		},
	}
	// expanded NOT set
	for _, r := range m.dashboardRows() {
		if r.kind == rowSeparator && r.separator != nil && r.separator.kind == separatorHistory {
			t.Fatalf("history separator should not render when project is collapsed")
		}
	}
}

// TestRows_HistoryGroup_SearchOverridesSplit — an active search
// filter merges history tasks back into the inline list so a slug
// query matches done tasks too.
func TestRows_HistoryGroup_SearchOverridesSplit(t *testing.T) {
	m := New("test")
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{
			{Name: "fleet", RepoSlug: "fleet", Tasks: []*taskRow{
				{Slug: "shipped-target", Status: "done"},
			}},
		},
	}
	m.expanded = map[string]bool{"fleet": true}
	m.searchFilter = "shipped-target"

	var foundInline bool
	for _, r := range m.dashboardRows() {
		if r.kind == rowTask && r.task != nil && r.task.Slug == "shipped-target" {
			foundInline = true
		}
		if r.kind == rowSeparator && r.separator != nil && r.separator.kind == separatorHistory {
			t.Fatalf("search mode should suppress the history separator")
		}
	}
	if !foundInline {
		t.Errorf("search match should surface the history task inline")
	}
}

// TestKey_HistorySeparatorEnter_TogglesExpansion — [enter] on the
// `─── N done ───` separator flips m.historyExpanded[<project>].
func TestKey_HistorySeparatorEnter_TogglesExpansion(t *testing.T) {
	m := New("test")
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{
			{Name: "fleet", RepoSlug: "fleet", Tasks: []*taskRow{
				{Slug: "shipped-1", Status: "done"},
			}},
		},
	}
	m.expanded = map[string]bool{"fleet": true}
	// Locate the history separator's index.
	idx := -1
	for i, r := range m.dashboardRows() {
		if r.kind == rowSeparator && r.separator != nil && r.separator.kind == separatorHistory {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatal("no history separator found")
	}
	m.dashCursor = idx

	mm, _, _ := m.openDetail()
	if !mm.historyExpanded["fleet"] {
		t.Errorf("expected historyExpanded[fleet]=true after [enter]")
	}
	// Second press collapses.
	mm.dashCursor = idx
	mm2, _, _ := mm.openDetail()
	if mm2.historyExpanded["fleet"] {
		t.Errorf("expected historyExpanded[fleet] cleared after second [enter]")
	}
}

// TestRender_DonePRTail — done tasks with a PR URL render the
// "✓ slug · PR #42" tail; without a URL they render just "✓ slug".
func TestRender_DonePRTail(t *testing.T) {
	withURL := taskBlockLine(&taskRow{
		Slug: "done-x-1234", Status: "done",
		PRURL: "https://github.com/x/y/pull/42",
	}, 60, false)
	if !strings.Contains(withURL, "PR #42") {
		t.Errorf("done task with PR URL should carry tail; got %q", withURL)
	}
	withoutURL := taskBlockLine(&taskRow{
		Slug: "done-y-5678", Status: "done",
	}, 60, false)
	if strings.Contains(withoutURL, "PR #") {
		t.Errorf("done task without PR URL should NOT carry tail; got %q", withoutURL)
	}
	if !strings.Contains(withoutURL, "✓") {
		t.Errorf("done task should still carry ✓ glyph; got %q", withoutURL)
	}
}
