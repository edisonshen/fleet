package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tasks"
	"github.com/edisonshen/fleet/internal/workers"
)

// TestKeyN_TaskAddCallsTasksAdd pins the issue #53 spec: pressing [n],
// typing a spec, and hitting Enter must persist a new task to the
// project's tasks.md WITHOUT shelling out to `fleet tasks add`. The
// in-process `internal/tasks.Add` path is the load-bearing API.
func TestKeyN_TaskAddCallsTasksAdd(t *testing.T) {
	pdir := withFleetHome(t)
	// Seed an existing project with one task so resolveProject and the
	// dashboard's project row both have something to anchor on.
	seedTasks(t, pdir, "fleet", TaskCounts{Todo: 1})

	m := New("test")
	m.width = 130
	m.height = 30
	m.dashboard = scanDashboard(time.Now())
	// Cursor on the project row → [n] targets that project.
	m.dashCursor = 0

	// [n] enters modePromptTaskAdd.
	mm, _ := m.Update(keyMsg("n"))
	if mm.(Model).mode != modePromptTaskAdd {
		t.Fatalf("[n] should enter modePromptTaskAdd, got %v", mm.(Model).mode)
	}

	// Type the spec body.
	for _, r := range "do thing" {
		mm, _ = mm.(Model).Update(keyMsg(string(r)))
	}
	if mm.(Model).promptBuf != "do thing" {
		t.Fatalf("promptBuf=%q want %q", mm.(Model).promptBuf, "do thing")
	}

	// Submit.
	mm, cmd := mm.(Model).Update(keyMsg("enter"))
	if cmd == nil {
		t.Fatal("enter should produce a tea.Cmd that performs the add")
	}
	// Drain the cmd so addTask actually runs.
	doneMsg := cmd()
	if msg, ok := doneMsg.(taskAddDoneMsg); !ok {
		t.Fatalf("expected taskAddDoneMsg, got %T", doneMsg)
	} else if msg.err != nil {
		t.Fatalf("addTask returned err: %v", msg.err)
	}

	// Read tasks.md back and verify a new task with spec="do thing"
	// appeared.
	dir := filepath.Join(pdir, "fleet")
	f, err := tasks.Read(filepath.Join(dir, "tasks.md"))
	if err != nil {
		t.Fatalf("read tasks.md: %v", err)
	}
	var found bool
	for _, tk := range f.Tasks {
		if strings.Contains(tk.Spec, "do thing") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("new task with spec 'do thing' not found in tasks.md\ntasks: %+v", f.Tasks)
	}

	// Mode reset and prompt cleared after submit.
	if mm.(Model).mode != modeNav {
		t.Errorf("mode after submit = %v, want modeNav", mm.(Model).mode)
	}
	if mm.(Model).promptBuf != "" {
		t.Errorf("promptBuf not cleared: %q", mm.(Model).promptBuf)
	}
}

// TestKeyEnter_OpensTaskDetail pins [⏎] open on a task row → detail
// panel renders with Spec/Acceptance/Notes.
func TestKeyEnter_OpensTaskDetail(t *testing.T) {
	pdir := withFleetHome(t)
	// Seed one project with a known-slug task. We bypass the test
	// helper's auto-derived slug because we need to address the task
	// by a stable string in the assertion.
	dir := filepath.Join(pdir, "fleet")
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if err := stateMkdir(dir); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	now := time.Now().UTC()
	f := &tasks.File{Schema: tasks.SchemaVersion}
	tk := &tasks.Task{
		Slug:       "demo-task-1234",
		Status:     tasks.StatusTodo,
		Priority:   tasks.PriorityP2,
		Spec:       "do the demo thing",
		Acceptance: "demo passes",
		Notes:      "scratch notes",
		Created:    now,
		Updated:    now,
	}
	if err := f.Add(tk); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := tasks.Write(filepath.Join(dir, "tasks.md"), f); err != nil {
		t.Fatalf("write: %v", err)
	}

	m := New("test")
	m.width = 130
	m.height = 30
	m.dashboard = scanDashboard(time.Now())
	// Find the task row index in dashboardRows().
	rows := m.dashboardRows()
	taskIdx := -1
	for i, r := range rows {
		if r.kind == rowTask && r.task != nil && r.task.Slug == "demo-task-1234" {
			taskIdx = i
			break
		}
	}
	if taskIdx < 0 {
		t.Fatalf("task row not found in dashboardRows: %+v", rows)
	}
	m.dashCursor = taskIdx

	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := mm.(Model)
	if got.detail == nil {
		t.Fatalf("[⏎] on task row should open detail panel; detail=nil")
	}
	out := got.View()
	if !strings.Contains(out, "do the demo thing") {
		t.Errorf("detail panel should render Spec text, got:\n%s", out)
	}
	if !strings.Contains(out, "Acceptance") {
		t.Errorf("detail panel should render Acceptance section, got:\n%s", out)
	}
}

// TestKeyEnter_OpensWorkerPeek pins [⏎] open on a worker row → detail
// panel renders state.json fields + log tail.
func TestKeyEnter_OpensWorkerPeek(t *testing.T) {
	pdir := withFleetHome(t)
	seedTasks(t, pdir, "fleet", TaskCounts{Todo: 1})
	seedWorker(t, pdir, "fleet", "do-x-1a2b", workers.State{
		Phase: workers.PhaseTDDGreen,
		PID:   42,
	})
	// Seed a small output.log so the panel has something to tail.
	logPath := filepath.Join(pdir, "fleet", "workers", "do-x-1a2b", "output.log")
	if err := stateWriteFile(logPath, "hello from worker\nsecond line\n"); err != nil {
		t.Fatalf("write output.log: %v", err)
	}

	m := New("test")
	m.width = 130
	m.height = 30
	m.dashboard = scanDashboard(time.Now())
	// Find the worker row.
	rows := m.dashboardRows()
	workerIdx := -1
	for i, r := range rows {
		if r.kind == rowWorker && r.worker != nil && r.worker.Slug == "do-x-1a2b" {
			workerIdx = i
			break
		}
	}
	if workerIdx < 0 {
		t.Fatalf("worker row not found in dashboardRows")
	}
	m.dashCursor = workerIdx

	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := mm.(Model)
	if got.detail == nil {
		t.Fatal("[⏎] on worker row should open detail panel; detail=nil")
	}
	out := got.View()
	// state.json fields must appear (we render parsed JSON).
	if !strings.Contains(out, "do-x-1a2b") || !strings.Contains(out, "tdd-green") {
		t.Errorf("detail panel should render state.json fields, got:\n%s", out)
	}
	// Log tail must show.
	if !strings.Contains(out, "hello from worker") {
		t.Errorf("detail panel should render output.log tail, got:\n%s", out)
	}
}

// TestKeySlash_OpensFilterPrompt pins [/] enters modePromptSearch with
// an empty filter buffer (or the existing one as the seed).
func TestKeySlash_OpensFilterPrompt(t *testing.T) {
	m := New("test")
	m.width = 130
	m.height = 30
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	got := mm.(Model)
	if got.mode != modePromptSearch {
		t.Errorf("mode = %v, want modePromptSearch", got.mode)
	}
	out := got.View()
	if !strings.Contains(out, "/") {
		t.Errorf("filter prompt should render in footer, got:\n%s", out)
	}
}

// TestKeyQuestionMark_OpensHelpOverlay pins [?] toggles m.showHelp and
// the help table renders.
func TestKeyQuestionMark_OpensHelpOverlay(t *testing.T) {
	m := New("test")
	m.width = 130
	m.height = 30
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("?")})
	got := mm.(Model)
	if !got.showHelp {
		t.Errorf("[?] should set showHelp=true, got false")
	}
	out := got.View()
	// Help table includes the "[?] this help" line — assert at least
	// one keybind description is present.
	if !strings.Contains(out, "Help") {
		t.Errorf("help overlay should render the FLEET — Help title, got:\n%s", out)
	}
	if !strings.Contains(out, "task-add prompt") && !strings.Contains(out, "task to the current project") {
		t.Errorf("help overlay should describe [n], got:\n%s", out)
	}
}

// TestKeyA_AttachAgent_NotApplicableOnTaskRow regresses issue #53's
// row-type gating: [a] on a task row flashes "doesn't apply" and does
// NOT shell out to tmux or set pendingAttach.
func TestKeyA_AttachAgent_NotApplicableOnTaskRow(t *testing.T) {
	pdir := withFleetHome(t)
	seedTasks(t, pdir, "fleet", TaskCounts{Todo: 1})
	(&stubSessionAlive{}).install(t)

	m := New("test")
	m.width = 130
	m.height = 30
	m.dashboard = scanDashboard(time.Now())
	// Find a task row.
	rows := m.dashboardRows()
	taskIdx := -1
	for i, r := range rows {
		if r.kind == rowTask {
			taskIdx = i
			break
		}
	}
	if taskIdx < 0 {
		t.Fatalf("no task row in dashboardRows: %+v", rows)
	}
	m.dashCursor = taskIdx

	updated, cmd := m.Update(keyMsg("a"))
	mm := updated.(Model)
	if cmd != nil {
		t.Errorf("[a] on task row should NOT produce a cmd, got non-nil")
	}
	if mm.pendingAttach != "" {
		t.Errorf("[a] on task row should not set pendingAttach, got %q", mm.pendingAttach)
	}
	if mm.flash == nil || !mm.flash.isErr {
		t.Errorf("[a] on task row should flash an error, got %+v", mm.flash)
	}
}

// TestSearchFiltersDashboardRows pins live-filter behavior: typing
// into the search prompt narrows dashboardRows() to substring matches.
func TestSearchFiltersDashboardRows(t *testing.T) {
	pdir := withFleetHome(t)
	seedTasks(t, pdir, "alpha", TaskCounts{Todo: 1})
	seedTasks(t, pdir, "zulu", TaskCounts{Todo: 1})

	m := New("test")
	m.width = 130
	m.height = 30
	m.dashboard = scanDashboard(time.Now())
	beforeRows := len(m.dashboardRows())

	// Apply filter to "alp" — only the alpha project + its task should
	// remain.
	m.searchFilter = "alp"
	afterRows := m.dashboardRows()
	if len(afterRows) >= beforeRows {
		t.Errorf("filter should reduce rows: before=%d after=%d", beforeRows, len(afterRows))
	}
	for _, r := range afterRows {
		switch r.kind {
		case rowProject:
			if !strings.Contains(r.project.Name, "alp") {
				t.Errorf("filter leaked: project %q", r.project.Name)
			}
		}
	}
}

// TestRowTypeGating_AgentActionsOnAgentRows verifies [a] on an agent
// row sets pendingAttach (positive case to balance the negative-case
// task-row test above).
func TestRowTypeGating_AgentActionsOnAgentRows(t *testing.T) {
	(&stubSessionAlive{}).install(t)
	m := New("test")
	m.width = 130
	m.height = 30
	m.records = []*agent.Record{sampleAgent("agent01")}
	m.dashCursor = 0 // agent row

	updated, cmd := m.Update(keyMsg("a"))
	mm := updated.(Model)
	if mm.pendingAttach != "fleet-agent01" {
		t.Errorf("[a] on agent row should set pendingAttach to tmux session, got %q", mm.pendingAttach)
	}
	if cmd == nil {
		t.Errorf("[a] on agent row should produce tea.Quit cmd")
	}
}

// stateMkdir / stateWriteFile centralize the os.MkdirAll +
// os.WriteFile boilerplate so the test bodies stay focused on the
// dashboard-level assertions they're regressing.
func stateMkdir(path string) error {
	return os.MkdirAll(path, 0o755)
}

func stateWriteFile(path, data string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(data), 0o644)
}

// TestEsc_ClearsActiveSearchFilter regresses codex iter-1 P2: in
// modeNav, [esc] must clear an active search filter so the footer's
// "/<query> · esc clears" hint is honored.
func TestEsc_ClearsActiveSearchFilter(t *testing.T) {
	m := New("test")
	m.searchFilter = "fix-bug"
	m.dashCursor = 0

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	got := updated.(Model)
	if got.searchFilter != "" {
		t.Errorf("[esc] should clear searchFilter, got %q", got.searchFilter)
	}
}

// TestSearch_TaskSlugMatchKeepsParentProject regresses codex iter-1
// P2: filtering on a task slug must not drop the parent project block
// — the task is unreachable otherwise.
func TestSearch_TaskSlugMatchKeepsParentProject(t *testing.T) {
	pdir := withFleetHome(t)
	dir := filepath.Join(pdir, "alpha")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	f := &tasks.File{Schema: tasks.SchemaVersion}
	tk := &tasks.Task{
		Slug:     "find-me-abcd",
		Status:   tasks.StatusTodo,
		Priority: tasks.PriorityP2,
		Spec:     "needle",
		Created:  now,
		Updated:  now,
	}
	if err := f.Add(tk); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := tasks.Write(filepath.Join(dir, "tasks.md"), f); err != nil {
		t.Fatalf("write: %v", err)
	}

	m := New("test")
	m.dashboard = scanDashboard(time.Now())
	// Filter on the task slug.
	m.searchFilter = "find-me"
	rows := m.dashboardRows()

	// Parent project must appear (so the operator can navigate to it).
	var sawProject, sawTask bool
	for _, r := range rows {
		if r.kind == rowProject && r.project.Name == "alpha" {
			sawProject = true
		}
		if r.kind == rowTask && r.task.Slug == "find-me-abcd" {
			sawTask = true
		}
	}
	if !sawProject {
		t.Errorf("task-slug filter should keep parent project visible; rows: %+v", rows)
	}
	if !sawTask {
		t.Errorf("task-slug filter should expose the matching task row; rows: %+v", rows)
	}
}

// TestDashboardMsg_ReClampsCursor regresses codex iter-1 P2: a
// dashboard refresh that drops rows (project archived, worker
// finished) must clamp dashCursor back into bounds, otherwise [⏎]
// silently no-ops.
func TestDashboardMsg_ReClampsCursor(t *testing.T) {
	m := New("test")
	// Pretend dashCursor was sitting on row 5.
	m.dashCursor = 5
	// dashboardMsg with an empty snapshot — dashboardRows() is now empty.
	updated, _ := m.Update(dashboardMsg{snap: &Snapshot{LoadedAt: time.Now()}})
	if updated.(Model).dashCursor != 0 {
		t.Errorf("dashboardMsg should re-clamp dashCursor when rows shrink; got %d", updated.(Model).dashCursor)
	}
}
