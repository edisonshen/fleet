package tui

import (
	"fmt"
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
	// Issue #59: tasks render under their project only when the
	// project is expanded. Pre-expand so the task row is in the list.
	m.expanded = map[string]bool{"fleet": true}
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
	// Issue #59: tasks only render under expanded projects.
	m.expanded = map[string]bool{"fleet": true}
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
	// Position cursor on the agent row by identity. Issue #55 added a
	// synthetic project row when m.records carries a Project tag; the
	// cursor positional index is implementation detail.
	for i, r := range m.dashboardRows() {
		if r.kind == rowAgent {
			m.dashCursor = i
			break
		}
	}

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

// TestKeyN_FreezesProjectAtPressTime regresses codex iter-2 P1: the
// task-add target project must be captured when [n] is pressed, not
// resolved at submit time. Otherwise a background dashboard refresh
// can shift dashboardRows() under the prompt and the new task lands
// in the wrong tasks.md.
func TestKeyN_FreezesProjectAtPressTime(t *testing.T) {
	pdir := withFleetHome(t)
	seedTasks(t, pdir, "alpha", TaskCounts{Todo: 1})
	seedTasks(t, pdir, "zulu", TaskCounts{Todo: 1})

	m := New("test")
	m.width = 130
	m.height = 30
	m.dashboard = scanDashboard(time.Now())
	// dashCursor on the first project (alpha — alpha-sorted).
	m.dashCursor = 0
	if row := m.selectedRow(); row == nil || row.kind != rowProject || row.project.Name != "alpha" {
		t.Fatalf("expected cursor on alpha project; got %+v", row)
	}

	// Press [n] — should freeze "alpha" as the target.
	mm, _ := m.Update(keyMsg("n"))
	got := mm.(Model)
	if got.taskAddProjectFrozen != "alpha" {
		t.Fatalf("taskAddProjectFrozen = %q, want alpha", got.taskAddProjectFrozen)
	}

	// Now SHIFT the dashboard rows so dashCursor=0 would land on a
	// different project. Easiest: simulate a tick that adds a new
	// project that sorts before alpha (just give it attention via a
	// blocked worker). For test purposes we directly mutate the
	// snapshot to put zulu first.
	got.dashboard = &Snapshot{
		Projects: []*ProjectRow{
			{Name: "zulu", Counts: TaskCounts{Todo: 1}},
			{Name: "alpha", Counts: TaskCounts{Todo: 1}},
		},
	}

	// Type a spec, threading the model through each rune so promptBuf
	// actually accumulates across keystrokes.
	current := tea.Model(got)
	for _, r := range "frozen test" {
		current, _ = current.Update(keyMsg(string(r)))
	}
	// Submit.
	_, cmd := current.Update(keyMsg("enter"))
	if cmd == nil {
		t.Fatal("expected tea.Cmd from enter")
	}
	doneMsg := cmd().(taskAddDoneMsg)
	if doneMsg.err != nil {
		t.Fatalf("addTask err: %v", doneMsg.err)
	}

	// The new task MUST land in alpha/tasks.md (the frozen target),
	// NOT zulu (where dashCursor=0 would now point).
	alphaFile, err := tasks.Read(filepath.Join(pdir, "alpha", "tasks.md"))
	if err != nil {
		t.Fatalf("read alpha tasks.md: %v", err)
	}
	zuluFile, err := tasks.Read(filepath.Join(pdir, "zulu", "tasks.md"))
	if err != nil {
		t.Fatalf("read zulu tasks.md: %v", err)
	}
	alphaHas, zuluHas := false, false
	for _, tk := range alphaFile.Tasks {
		if tk.Spec == "frozen test" {
			alphaHas = true
		}
	}
	for _, tk := range zuluFile.Tasks {
		if tk.Spec == "frozen test" {
			zuluHas = true
		}
	}
	if !alphaHas {
		t.Errorf("frozen test task should land in alpha/tasks.md")
	}
	if zuluHas {
		t.Errorf("frozen test task must NOT land in zulu/tasks.md")
	}
}

// TestOverlay_DismissedKeyAbsorbed regresses codex iter-2 P2: when an
// overlay (help / detail) is up, the next key dismisses it AND that
// key is absorbed (does not also move the cursor). Otherwise [j]/[k]
// would silently scroll under the modal.
func TestOverlay_DismissedKeyAbsorbed(t *testing.T) {
	m := New("test")
	m.width = 130
	m.height = 30
	m.records = []*agent.Record{sampleAgent("a"), sampleAgent("b"), sampleAgent("c")}
	m.dashCursor = 1
	m.showHelp = true

	// Press [j] while help is up. Help must dismiss; cursor must NOT move.
	updated, _ := m.Update(keyMsg("j"))
	got := updated.(Model)
	if got.showHelp {
		t.Errorf("[j] should dismiss help overlay")
	}
	if got.dashCursor != 1 {
		t.Errorf("dashCursor moved through dismissed overlay: was 1, now %d", got.dashCursor)
	}
}

// TestKeyX_WorkerRow_FlashesNotImplemented regresses codex iter-3 P1:
// the worker [x] path used to shell out to `fleet workers kill`,
// which doesn't exist. Until kill is wired, [x] on a worker must
// flash a hint and NOT shell out.
func TestKeyX_WorkerRow_FlashesNotImplemented(t *testing.T) {
	stub := &stubFleetCmd{}
	stub.install(t)

	pdir := withFleetHome(t)
	seedTasks(t, pdir, "fleet", TaskCounts{Todo: 1})
	seedWorker(t, pdir, "fleet", "x-1234", workers.State{
		Phase: workers.PhaseTDDGreen,
		PID:   42,
	})

	m := New("test")
	m.width = 130
	m.height = 30
	m.dashboard = scanDashboard(time.Now())
	rows := m.dashboardRows()
	idx := -1
	for i, r := range rows {
		if r.kind == rowWorker {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatal("no worker row")
	}
	m.dashCursor = idx

	updated, cmd := m.Update(keyMsg("x"))
	mm := updated.(Model)
	if cmd != nil {
		t.Errorf("[x] on worker should not shell out, got cmd != nil")
	}
	if mm.flash == nil || !mm.flash.isErr {
		t.Errorf("[x] on worker should flash error, got %+v", mm.flash)
	}
	if len(stub.calls) != 0 {
		t.Errorf("[x] on worker shelled out (calls=%v); should not until kill is wired", stub.calls)
	}
}

// TestKeyN_RefusesUnknownCwd regresses codex iter-3 P2: [n] from a
// random cwd that has no Fleet project state must NOT silently create
// a new project. taskAddProject returns "" → submit shows the
// "no project context" flash.
func TestKeyN_RefusesUnknownCwd(t *testing.T) {
	withFleetHome(t)
	// cwd: a temp dir that's NOT inside ~/.fleet and NOT in projects/.
	tmp := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(cwd) }()
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}

	m := New("test")
	m.width = 130
	m.height = 30
	// No dashboard, no records → no project rows; cursor at 0 with
	// no rows. [n] press → freezes "" → submit flashes error.
	mm, _ := m.Update(keyMsg("n"))
	if mm.(Model).taskAddProjectFrozen != "" {
		t.Errorf("from unknown cwd, [n] should freeze empty project; got %q",
			mm.(Model).taskAddProjectFrozen)
	}
	// Type a spec + submit. Thread the model through each Update so
	// promptBuf accumulates correctly; tea.Model is the interface
	// returned by Update so we keep that as the loop variable.
	current := tea.Model(mm)
	for _, r := range "ghost task" {
		current, _ = current.Update(keyMsg(string(r)))
	}
	updated, cmd := current.Update(keyMsg("enter"))
	if cmd != nil {
		t.Errorf("[n] enter from unknown cwd should NOT trigger tasks.Add cmd")
	}
	got := updated.(Model)
	if got.flash == nil || !got.flash.isErr {
		t.Errorf("[n] from unknown cwd should flash error, got %+v", got.flash)
	}
	if got.flash != nil && !strings.Contains(got.flash.text, "no project") {
		t.Errorf("[n] flash should say 'no project context', got %q", got.flash.text)
	}
}

// TestKeyN_AgentProjectAcceptsFreshDispatch regresses codex iter-5 P2:
// `fleet dispatch` creates an agent record before the per-project
// state dir exists, so [n] on an agent row must accept the agent's
// Project tag even when ~/.fleet/projects/<tag>/ is not yet present.
// (iter-4 was over-tightened.)
func TestKeyN_AgentProjectAcceptsFreshDispatch(t *testing.T) {
	withFleetHome(t)
	cwd, _ := os.Getwd()
	defer func() { _ = os.Chdir(cwd) }()
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}

	r := sampleAgent("fresh")
	r.Project = "newly-dispatched"

	m := New("test")
	m.records = []*agent.Record{r}
	m.dashCursor = 0

	mm, _ := m.Update(keyMsg("n"))
	if mm.(Model).taskAddProjectFrozen != "newly-dispatched" {
		t.Errorf("[n] on agent row should accept agent.Project; got %q",
			mm.(Model).taskAddProjectFrozen)
	}
}

// TestAddTask_ArchiveReadErrorSurfaces regresses codex iter-4 P3:
// when tasks-archive.md is unreadable / corrupted, addTask must
// return an error rather than silently treating the archive as empty
// (which would let GenerateSlug reuse archived slugs).
func TestAddTask_ArchiveReadErrorSurfaces(t *testing.T) {
	pdir := withFleetHome(t)
	dir := filepath.Join(pdir, "broken")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// tasks.md present (empty) so the active read succeeds.
	if err := os.WriteFile(filepath.Join(dir, "tasks.md"),
		[]byte("---\nschema: v1\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// tasks-archive.md is malformed: a real schema header but an
	// invalid version that internal/tasks.parse refuses with
	// ErrSchemaTooNew.
	if err := os.WriteFile(filepath.Join(dir, "tasks-archive.md"),
		[]byte("---\nschema: v99\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := addTask("broken", "test spec")
	if err == nil {
		t.Fatal("addTask should return error when tasks-archive.md is corrupted")
	}
	if !strings.Contains(err.Error(), "tasks-archive") {
		t.Errorf("error should reference tasks-archive: %v", err)
	}
}

// TestCursor_StableAcrossDashboardRefresh regresses codex iter-5 P1:
// when a tick or fsEvent re-sorts dashboardRows() (project gains
// attention, new task inserted), the cursor must follow its
// previously-selected row by identity, not by raw index.
func TestCursor_StableAcrossDashboardRefresh(t *testing.T) {
	pdir := withFleetHome(t)
	seedTasks(t, pdir, "alpha", TaskCounts{Todo: 1})
	seedTasks(t, pdir, "zulu", TaskCounts{Todo: 1})

	m := New("test")
	m.dashboard = scanDashboard(time.Now())
	// Find the zulu project row index (alpha sorts first by default).
	rows := m.dashboardRows()
	zuluIdx := -1
	for i, r := range rows {
		if r.kind == rowProject && r.project.Name == "zulu" {
			zuluIdx = i
			break
		}
	}
	if zuluIdx < 0 {
		t.Fatalf("zulu row not found")
	}
	m.dashCursor = zuluIdx

	// Now simulate a refresh that puts zulu FIRST (e.g. it gained
	// attention). Construct a snapshot with zulu first, alpha second.
	snap := &Snapshot{
		Projects: []*ProjectRow{
			{Name: "zulu", Counts: TaskCounts{Todo: 1}, Attention: 1},
			{Name: "alpha", Counts: TaskCounts{Todo: 1}},
		},
	}
	updated, _ := m.Update(dashboardMsg{snap: snap})
	got := updated.(Model)

	if row := got.selectedRow(); row == nil || row.kind != rowProject || row.project.Name != "zulu" {
		t.Errorf("cursor should follow zulu after refresh; got %+v", row)
	}
}

// TestCursor_StableAcrossAgentsMsgRefresh covers the agentsMsg path
// — a new agent landing must not bump the cursor onto a different row.
//
// Layout note (issue #55): unifiedProjects() now synthesizes a project
// row for any agent.Project tag absent from m.dashboard. sampleAgent
// pins Project = "proj", so both records dedupe into one synthetic
// project row preceding the two agent rows. We locate "agent b" by
// identity rather than hardcoding an index.
func TestCursor_StableAcrossAgentsMsgRefresh(t *testing.T) {
	m := New("test")
	m.records = []*agent.Record{sampleAgent("a"), sampleAgent("b")}
	// Locate agent "b" by walking the row list (positional index changed
	// when we added the synthetic-project union).
	for i, r := range m.dashboardRows() {
		if r.kind == rowAgent && r.agent != nil && r.agent.ID == "b" {
			m.dashCursor = i
			break
		}
	}
	if row := m.selectedRow(); row == nil || row.kind != rowAgent || row.agent.ID != "b" {
		t.Fatalf("cursor should be on agent b initially; got %+v", row)
	}

	// New agentsMsg with an additional agent inserted at the top.
	updated, _ := m.Update(agentsMsg{records: []*agent.Record{
		sampleAgent("c"),
		sampleAgent("a"),
		sampleAgent("b"),
	}})
	got := updated.(Model)

	if row := got.selectedRow(); row == nil || row.kind != rowAgent || row.agent.ID != "b" {
		t.Errorf("cursor should still be on agent b after refresh; got %+v", row)
	}
}

// TestCursor_ResetsWhenSelectedRowDisappears regresses codex iter-6
// P1: when the previously-selected row is removed by a refresh, the
// cursor must reset to 0 rather than dangle on the same numeric
// index. With identity-aware refresh, the operator at minimum gets a
// well-defined "top of the list" landing instead of silent
// retargeting onto whoever happens to occupy the old slot.
func TestCursor_ResetsWhenSelectedRowDisappears(t *testing.T) {
	m := New("test")
	// fakeRecords spaces records 1 minute apart; sortRecordsBy puts
	// newest first → after sort the list is [a2, a1, a0]. Locate a1's
	// row by identity (issue #55 added a synthetic project row when
	// records share a Project tag, so positional indexing into
	// m.records would no longer match dashboardRows() indexing).
	m.records = sortRecords(fakeRecords(3))
	if m.records[1].ID != "a1" {
		t.Fatalf("test setup: expected a1 at index 1, got %s", m.records[1].ID)
	}
	for i, r := range m.dashboardRows() {
		if r.kind == rowAgent && r.agent != nil && r.agent.ID == "a1" {
			m.dashCursor = i
			break
		}
	}

	// Refresh: remove a1. With raw clamping, dashCursor would silently
	// land on whatever's now at the same numeric index (the wrong
	// agent). With identity-aware refresh, dashCursor resets to 0.
	all := fakeRecords(3)
	updated, _ := m.Update(agentsMsg{records: []*agent.Record{all[0], all[2]}})
	got := updated.(Model)
	if got.dashCursor != 0 {
		t.Errorf("dashCursor should reset to 0 when selected row disappears; got %d", got.dashCursor)
	}
}

// TestView_SearchHidesWorkersShowsMatchHint regresses codex iter-6
// P3: when a [/] filter hides all workers but the snapshot still has
// active workers, the right column hint must say "no workers match
// /<query>" rather than the misleading "no workers running".
func TestView_SearchHidesWorkersShowsMatchHint(t *testing.T) {
	pdir := withFleetHome(t)
	seedTasks(t, pdir, "demo", TaskCounts{Todo: 1})
	seedWorker(t, pdir, "demo", "real-worker-aaaa", workers.State{
		Phase: workers.PhaseTDDGreen,
		PID:   1,
	})

	m := New("test")
	m.width = 130
	m.height = 30
	m.dashboard = scanDashboard(time.Now())

	// Apply a filter that doesn't match the worker.
	m.searchFilter = "zzzz-no-match"
	out := m.View()
	if !strings.Contains(out, "no workers match") {
		t.Errorf("filtered-empty workers should hint about the active filter, got:\n%s", out)
	}
	if strings.Contains(out, "no workers running") {
		t.Errorf("must NOT show the unfiltered 'no workers running' hint when filter is active, got:\n%s", out)
	}
}

// TestReadLastLines_KeepsLineAtBoundary regresses codex iter-7 P3:
// when the seek window lands exactly on a newline boundary, the
// first line of the read window is whole and must NOT be discarded.
func TestReadLastLines_KeepsLineAtBoundary(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "out.log")
	// 70 lines of "AAAA". Seek window will be 64KiB or n*256, so for
	// n=5 the window is 64KiB — way bigger than this file. Push
	// instead with a very large file: ~80KB so the 64KiB window
	// truncates from the middle.
	var b strings.Builder
	for i := 0; i < 8000; i++ {
		fmt.Fprintf(&b, "line%05d\n", i)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	tail, err := readLastLines(path, 5)
	if err != nil {
		t.Fatalf("readLastLines: %v", err)
	}
	// The very last line should be present in the tail.
	if !strings.Contains(tail, "line07999") {
		t.Errorf("last line missing from tail:\n%s", tail)
	}
	// Tail should have 5 lines.
	got := strings.Count(strings.TrimRight(tail, "\n"), "\n") + 1
	if got != 5 {
		t.Errorf("tail line count = %d, want 5\ntail:\n%s", got, tail)
	}
}

// TestDetailOverlay_ClipsLongBody regresses codex iter-8 P2: when the
// detail panel's body would push the close-hint off the alt-screen,
// it must be clipped with a "(N more)" tail so the operator always
// sees the dismiss affordance.
func TestDetailOverlay_ClipsLongBody(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 60; i++ {
		fmt.Fprintf(&b, "body line %02d\n", i)
	}
	d := detailView{title: "test", body: b.String()}
	out := renderDetailOverlay(d, 120, 24)
	if !strings.Contains(out, "more") {
		t.Errorf("clipped overlay should announce remaining lines, got:\n%s", out)
	}
	if !strings.Contains(out, "press [esc] or [⏎] to close") {
		t.Errorf("clipped overlay should keep close hint visible, got:\n%s", out)
	}
}

// TestDetailOverlay_AccountsForSoftWrap regresses codex iter-9 P2:
// when a single body line is wider than the terminal, the panel
// budget must count the wrapped rows (cell width / termWidth), not
// just the line count. Otherwise the close hint scrolls off-screen.
func TestDetailOverlay_AccountsForSoftWrap(t *testing.T) {
	// Two long lines that each soft-wrap to ~10 rows on an 80-cell
	// terminal — together exceed the body budget for a 24-row
	// alt-screen (24 - 8 chrome = 16 rows). Without visualRows-aware
	// clipping, len(lines)==2 < 16 so no clip would fire.
	wide := strings.Repeat("X", 800)
	body := wide + "\n" + wide + "\n"
	d := detailView{title: "test", body: body}
	out := renderDetailOverlay(d, 80, 24)
	if !strings.Contains(out, "more") {
		t.Errorf("wrapped body should still trigger clipping hint; got:\n%s", out)
	}
	if !strings.Contains(out, "press [esc] or [⏎] to close") {
		t.Errorf("close hint must remain visible; got:\n%s", out)
	}
}

// TestAgentsSubheader_ShowsVisibleAliveOnly regresses codex iter-9 P3:
// the "v0.1 agents — N active" sub-header must not count records
// hidden by the search filter, and must not count records whose
// derived status is "dead".
func TestAgentsSubheader_ShowsVisibleAliveOnly(t *testing.T) {
	(&stubSessionProbe{
		dead: map[string]bool{"fleet-zombie": true},
	}).install(t)

	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	if err := os.MkdirAll(tmp+"/agents", 0o755); err != nil {
		t.Fatalf("mkdir agents: %v", err)
	}
	// Two agents: one live, one zombie (probe says dead).
	live := agent.New("liveone")
	live.TmuxSession = "fleet-liveone"
	live.SpawnedAt = time.Now().UTC()
	if err := live.Write(); err != nil {
		t.Fatal(err)
	}
	zombie := agent.New("zombie")
	zombie.TmuxSession = "fleet-zombie"
	zombie.SpawnedAt = time.Now().UTC()
	if err := zombie.Write(); err != nil {
		t.Fatal(err)
	}

	// Run loadAgentsCmd to populate aliveByID with the dead probe.
	msg := loadAgentsCmd()().(agentsMsg)

	m := New("test")
	m.width = 130
	m.height = 30
	updated, _ := m.Update(msg)
	got := updated.(Model)

	out := got.View()
	if !strings.Contains(out, "v0.1 agents — 1 active") {
		t.Errorf("agents sub-header should report 1 active (excluding dead zombie), got:\n%s", out)
	}

	// Now apply a filter that hides everyone. Sub-header should NOT
	// appear (or should report 0 active).
	got.searchFilter = "no-match-zzz"
	out = got.View()
	if strings.Contains(out, "v0.1 agents — 1 active") || strings.Contains(out, "v0.1 agents — 2 active") {
		t.Errorf("filtered-empty agents must not over-report active count, got:\n%s", out)
	}
}

// TestKeyN_WalksUpToFindProject regresses codex iter-10 P2: pressing
// [n] from a repo SUBDIRECTORY must still resolve to the parent
// project. Previously ProjectTag(cwd) was applied to the cwd itself,
// so `repo/internal/tui` produced "internal-tui" and missed the real
// project.
func TestKeyN_WalksUpToFindProject(t *testing.T) {
	pdir := withFleetHome(t)
	// Stand up a project named after a repo we'll cd into.
	seedTasks(t, pdir, "myrepo", TaskCounts{Todo: 1})

	// Build a sibling temp tree that ProjectTag(cwd) sees as
	// "<parent>-myrepo": grandparent/parent/myrepo. The tree must be
	// such that walking from a sub-subdir of myrepo eventually
	// resolves to a tag matching "myrepo".
	tmp := t.TempDir()
	repoRoot := filepath.Join(tmp, "myrepo")
	subDir := filepath.Join(repoRoot, "internal", "tui")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// .git boundary is required so the walk-up stops at repoRoot
	// instead of climbing further (codex iter-11 P1).
	if err := os.MkdirAll(filepath.Join(repoRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Walk-up logic: ProjectTag(repoRoot) = "<tmp_basename>-myrepo".
	// We want the project name to MATCH the cwd tag. Easiest: name
	// the project to match. Re-seed with the actual derived tag.
	wantTag := ProjectTag(repoRoot)
	if wantTag == "myrepo" {
		// Tag turned out parent-less; simple match.
	} else {
		// Re-seed with the derived tag so walk-up finds it.
		seedTasks(t, pdir, wantTag, TaskCounts{Todo: 1})
	}

	cwd, _ := os.Getwd()
	defer func() { _ = os.Chdir(cwd) }()
	if err := os.Chdir(subDir); err != nil {
		t.Fatal(err)
	}

	m := New("test")
	got := m.taskAddProject()
	if got != wantTag {
		t.Errorf("taskAddProject from subdir should walk up to %q, got %q",
			wantTag, got)
	}
}

// TestReadWorkerDetail_FallsBackToArchive regresses codex iter-10 P3:
// inline peek must show the archived worker's state.json + log when
// the active workers/<slug>/ has been moved to archive between the
// last dashboard refresh and the operator pressing [a]/Enter.
func TestReadWorkerDetail_FallsBackToArchive(t *testing.T) {
	pdir := withFleetHome(t)
	// Don't create the active workers/<slug>/ dir at all — only the
	// archive entry. Stamp follows YYYYMMDD-HHMMSS.
	archDir := filepath.Join(pdir, "myproj", "workers", "archive",
		"finished-x-aa11-20260101-120000")
	if err := os.MkdirAll(archDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(archDir, "state.json"),
		[]byte(`{"slug":"finished-x-aa11","phase":"done","pid":7}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(archDir, "output.log"),
		[]byte("archived log line\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	body, title := readWorkerDetail("myproj", "finished-x-aa11")
	if !strings.Contains(title, "archived") {
		t.Errorf("title should mark archived: %q", title)
	}
	if !strings.Contains(body, "finished-x-aa11") {
		t.Errorf("body should render archived state.json, got:\n%s", body)
	}
	if !strings.Contains(body, "archived log line") {
		t.Errorf("body should render archived output.log tail, got:\n%s", body)
	}
}

// TestKeyN_RefusesUnrelatedCwd_WithFleetProject regresses codex
// iter-11 P1: pressing [n] from a cwd that is NOT a git repo (or any
// ancestor of a git repo with a matching project) must refuse, even
// when ~/.fleet/projects/fleet/ exists. The previous unbounded walk
// would resolve "/" to the "fleet" project and silently accept.
func TestKeyN_RefusesUnrelatedCwd_WithFleetProject(t *testing.T) {
	pdir := withFleetHome(t)
	seedTasks(t, pdir, "fleet", TaskCounts{Todo: 1})

	tmp := t.TempDir() // no .git anywhere
	cwd, _ := os.Getwd()
	defer func() { _ = os.Chdir(cwd) }()
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}

	m := New("test")
	got := m.taskAddProject()
	if got != "" {
		t.Errorf("taskAddProject from non-repo cwd must refuse; got %q (would silently target /-resolved fleet project)", got)
	}
}

// TestDetailOverlay_NoClipWhenFits regresses codex iter-11 P2: a body
// whose total visual rows equals the budget exactly must NOT trigger
// the "(N more)" truncation; the operator should see every line.
func TestDetailOverlay_NoClipWhenFits(t *testing.T) {
	// height=24 → bodyBudget=16. Generate exactly 16 short lines.
	var b strings.Builder
	for i := 0; i < 16; i++ {
		fmt.Fprintf(&b, "row%02d\n", i)
	}
	d := detailView{title: "test", body: b.String()}
	out := renderDetailOverlay(d, 120, 24)
	if strings.Contains(out, "more") {
		t.Errorf("body that exactly fits should not be clipped; got:\n%s", out)
	}
	if !strings.Contains(out, "row15") {
		t.Errorf("last line of fitting body must render; got:\n%s", out)
	}
}

// TestUpdate_DashboardMsgSurfacesScanErr regresses codex iter-12 P2:
// a Snapshot.Err set by scanDashboard (e.g. unreadable
// ~/.fleet/projects/) must surface as a flash so the dashboard
// doesn't silently render as "0 projects".
func TestUpdate_DashboardMsgSurfacesScanErr(t *testing.T) {
	m := New("test")
	scanErr := fmt.Errorf("permission denied")
	updated, _ := m.Update(dashboardMsg{snap: &Snapshot{
		Err:      scanErr,
		LoadedAt: time.Now(),
	}})
	got := updated.(Model)
	if got.flash == nil || !got.flash.isErr {
		t.Fatalf("dashboard scan err should set error flash; got %+v", got.flash)
	}
	if !strings.Contains(got.flash.text, "permission denied") {
		t.Errorf("flash should reference scan err; got %q", got.flash.text)
	}
}

// TestRepoRootForCwd_StopsAtHome regresses codex iter-12 P3: a
// git-tracked $HOME (dotfiles checkout) must NOT capture [n] from a
// random subdirectory like ~/Downloads. repoRootForCwd should refuse
// at the $HOME boundary.
func TestRepoRootForCwd_StopsAtHome(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	// Make $HOME a git checkout (dotfiles pattern).
	if err := os.MkdirAll(filepath.Join(tmp, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Subdir below $HOME with no .git of its own.
	sub := filepath.Join(tmp, "Downloads", "stuff")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	if got := repoRootForCwd(sub); got != "" {
		t.Errorf("repoRootForCwd should refuse at $HOME; got %q", got)
	}
}

// TestKeyA_ProjectRow_AttachesToExistingFreshCoord pins issue #60's
// happy path: when a project's CoordID is set (dashboard freshness gate
// passed) AND the matching agent record is loaded, [a] sets
// pendingAttach + tea.Quit to attach to that coord's tmux session.
// No dispatch shells out.
func TestKeyA_ProjectRow_AttachesToExistingFreshCoord(t *testing.T) {
	(&stubSessionAlive{}).install(t)
	stub := &stubFleetCmd{}
	stub.install(t)

	// Build a model with a project row whose CoordID matches a loaded
	// agent record.
	coord := sampleAgent("coord001")
	coord.Project = "demo"
	m := New("test")
	m.records = []*agent.Record{coord}
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{{Name: "demo", CoordID: "coord001"}},
	}
	// Find the project row index in the unified row list.
	for i, r := range m.dashboardRows() {
		if r.kind == rowProject && r.project != nil && r.project.Name == "demo" {
			m.dashCursor = i
			break
		}
	}
	if row := m.selectedRow(); row == nil || row.kind != rowProject {
		t.Fatalf("expected cursor on project row; got %+v", row)
	}

	updated, cmd := m.Update(keyMsg("a"))
	mm := updated.(Model)

	if mm.pendingAttach != "fleet-coord001" {
		t.Errorf("pendingAttach = %q; want fleet-coord001", mm.pendingAttach)
	}
	if cmd == nil {
		t.Error("expected tea.Quit cmd from [a] on existing coord")
	}
	if len(stub.calls) != 0 {
		t.Errorf("[a] on existing coord should NOT shell out; got %v", stub.calls)
	}
}

// TestKeyA_ProjectRow_CoordIDSetButRecordNotLoaded_FlashesRetry pins
// codex iter-1 P1: when the dashboard snapshot has CoordID set
// (loadDashboardCmd ran) but the matching agent record isn't in
// m.records yet (loadAgentsCmd hasn't fired), a duplicate spawn would
// race against the live coord. The handler must flash a retry hint
// rather than fall through to the spawn path. This race is normal
// under tea.Batch and is not a bug — but spawning a duplicate coord IS
// a bug.
func TestKeyA_ProjectRow_CoordIDSetButRecordNotLoaded_FlashesRetry(t *testing.T) {
	(&stubSessionAlive{}).install(t)
	stub := &stubFleetCmd{}
	stub.install(t)

	m := New("test")
	// CoordID set, but m.records is empty — the loadAgentsCmd refresh
	// hasn't published the record yet.
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{{Name: "demo", CoordID: "missingrec"}},
	}
	for i, r := range m.dashboardRows() {
		if r.kind == rowProject {
			m.dashCursor = i
			break
		}
	}
	updated, cmd := m.Update(keyMsg("a"))
	mm := updated.(Model)

	if mm.flash == nil || !mm.flash.isErr {
		t.Fatalf("expected error flash for refresh-race; got %+v", mm.flash)
	}
	if !strings.Contains(mm.flash.text, "pending refresh") {
		t.Errorf("flash should mention 'pending refresh'; got %q", mm.flash.text)
	}
	if cmd != nil {
		t.Error("refresh-race must NOT produce a cmd (no spawn)")
	}
	if mm.pendingAttach != "" {
		t.Errorf("pendingAttach must NOT be set; got %q", mm.pendingAttach)
	}
	if len(stub.calls) != 0 {
		t.Errorf("refresh-race must NOT shell out (would dup-spawn); got %v", stub.calls)
	}
}

// TestKeyA_ProjectRow_DeadCoordSessionFlashes regresses the case where
// project.CoordID is set but the tmux session has died. We must not
// silently exec tmux on a dead session; flash a clear hint instead.
func TestKeyA_ProjectRow_DeadCoordSessionFlashes(t *testing.T) {
	(&stubSessionAlive{dead: map[string]bool{"fleet-coord001": true}}).install(t)
	stub := &stubFleetCmd{}
	stub.install(t)

	coord := sampleAgent("coord001")
	coord.Project = "demo"
	m := New("test")
	m.records = []*agent.Record{coord}
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{{Name: "demo", CoordID: "coord001"}},
	}
	for i, r := range m.dashboardRows() {
		if r.kind == rowProject && r.project != nil && r.project.Name == "demo" {
			m.dashCursor = i
			break
		}
	}

	updated, cmd := m.Update(keyMsg("a"))
	mm := updated.(Model)

	if mm.pendingAttach != "" {
		t.Errorf("pendingAttach must NOT be set for dead session; got %q", mm.pendingAttach)
	}
	if cmd != nil {
		t.Errorf("[a] on dead coord should not produce cmd; got non-nil")
	}
	if mm.flash == nil || !mm.flash.isErr {
		t.Errorf("[a] on dead coord should flash an error; got %+v", mm.flash)
	}
	if len(stub.calls) != 0 {
		t.Errorf("[a] on dead coord should NOT shell out; got %v", stub.calls)
	}
}

// TestKeyA_ProjectRow_NoCoord_SpawnsAndAttaches pins the auto-spawn
// path: when project.CoordID is empty AND no matching record exists,
// [a] shells out to `fleet dispatch` with --project, --cwd, --prompt,
// then on success sets pendingAttach to the new coord's tmux session.
// Issue #63: stable task_id = "coord-<project>" (no timestamp), no
// lock-poll — attach immediately after dispatch returns.
func TestKeyA_ProjectRow_NoCoord_SpawnsAndAttaches(t *testing.T) {
	withFleetHome(t)
	(&stubSessionAlive{}).install(t)

	stub := &stubFleetCmd{
		stubbed: func(args []string) tea.Msg {
			// Mimic real `fleet dispatch` output so the regex parser
			// finds the new agent ID.
			out := "agent abcd1234 spawned\n  task: coord-demo\n  project: demo\n  tmux: fleet-abcd1234\n"
			return coordSpawnDoneMsgFromArgs(args, out, nil)
		},
	}
	stub.install(t)

	m := New("test")
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{{Name: "demo"}}, // no CoordID
	}
	for i, r := range m.dashboardRows() {
		if r.kind == rowProject {
			m.dashCursor = i
			break
		}
	}

	_, cmd := m.Update(keyMsg("a"))
	if cmd == nil {
		t.Fatal("[a] on project with no coord should produce a spawn cmd")
	}
	// Drain the cmd to fire the dispatch. coordSpawnDoneMsg is what we
	// expect back. Crucially: no lock-poll, so success requires only
	// that dispatch returned the agent-ID line.
	msg := cmd()
	doneMsg, ok := msg.(coordSpawnDoneMsg)
	if !ok {
		t.Fatalf("expected coordSpawnDoneMsg; got %T (%+v)", msg, msg)
	}
	if doneMsg.err != nil {
		t.Fatalf("spawn returned err: %v", doneMsg.err)
	}
	if doneMsg.agentID != "abcd1234" {
		t.Errorf("agentID = %q; want abcd1234", doneMsg.agentID)
	}
	if doneMsg.session != "fleet-abcd1234" {
		t.Errorf("session = %q; want fleet-abcd1234", doneMsg.session)
	}
	if doneMsg.projectName != "demo" {
		t.Errorf("projectName = %q; want demo", doneMsg.projectName)
	}

	// Verify the dispatch arg shape.
	if len(stub.calls) != 1 {
		t.Fatalf("expected 1 fleet call; got %d (%v)", len(stub.calls), stub.calls)
	}
	args := stub.calls[0]
	if args[0] != "dispatch" {
		t.Errorf("first arg = %q; want dispatch", args[0])
	}
	// Issue #63: task_id MUST be the stable "coord-<project>" form, not
	// a timestamp. The parent agent at args[1] is the dispatch task_id.
	if len(args) < 2 || args[1] != "coord-demo" {
		t.Errorf("dispatch task_id (args[1]) = %q; want coord-demo (stable per-project)", argString(args, 1))
	}
	// Must carry --project demo, --prompt with the coordinator skill loop.
	hasProject, hasPrompt := false, false
	for i, a := range args {
		if a == "--project" && i+1 < len(args) && args[i+1] == "demo" {
			hasProject = true
		}
		if a == "--prompt" && i+1 < len(args) &&
			strings.Contains(args[i+1], "/coordinator skill loop for project demo") {
			hasPrompt = true
		}
	}
	if !hasProject {
		t.Errorf("dispatch args missing --project demo: %v", args)
	}
	if !hasPrompt {
		t.Errorf("dispatch args missing --prompt with coord skill loop: %v", args)
	}

	// Now feed the doneMsg back into Update — pendingAttach must be set
	// and the cmd must be tea.Quit.
	updated, cmd2 := m.Update(doneMsg)
	mm := updated.(Model)
	if mm.pendingAttach != "fleet-abcd1234" {
		t.Errorf("after coordSpawnDoneMsg, pendingAttach = %q; want fleet-abcd1234",
			mm.pendingAttach)
	}
	if cmd2 == nil {
		t.Error("expected tea.Quit cmd after spawn done")
	}
}

// argString returns args[i] or "" when i is out of range.
func argString(args []string, i int) string {
	if i < 0 || i >= len(args) {
		return ""
	}
	return args[i]
}

// coordSpawnDoneMsgFromArgs mimics the real startCoordSpawn closure
// post-issue-#63: parse the agent ID and return the coordSpawnDoneMsg
// shape. No lock-poll branch — if dispatch printed the agent line,
// attach. If not, error.
func coordSpawnDoneMsgFromArgs(args []string, out string, dispatchErr error) tea.Msg {
	if dispatchErr != nil {
		return coordSpawnDoneMsg{
			projectName: argValue(args, "--project"),
			err:         fmt.Errorf("dispatch: %w\n%s", dispatchErr, out),
		}
	}
	match := dispatchAgentIDPattern.FindStringSubmatch(out)
	if len(match) != 2 {
		return coordSpawnDoneMsg{
			projectName: argValue(args, "--project"),
			err:         fmt.Errorf("dispatch output missing agent ID line:\n%s", out),
		}
	}
	agentID := match[1]
	return coordSpawnDoneMsg{
		projectName: argValue(args, "--project"),
		agentID:     agentID,
		session:     "fleet-" + agentID,
	}
}

// argValue returns the value following flag in args, or "" if absent.
func argValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// TestKeyA_ProjectRow_DispatchErr regresses the dispatch-error path:
// when `fleet dispatch` returns non-zero, coordSpawnDoneMsg carries err
// and Update surfaces it as a flash. pendingAttach stays empty.
func TestKeyA_ProjectRow_DispatchErr(t *testing.T) {
	withFleetHome(t)
	stub := &stubFleetCmd{
		stubbed: func(args []string) tea.Msg {
			return coordSpawnDoneMsgFromArgs(args, "boom\n", fmt.Errorf("exit 1"))
		},
	}
	stub.install(t)

	m := New("test")
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{{Name: "demo"}},
	}
	for i, r := range m.dashboardRows() {
		if r.kind == rowProject {
			m.dashCursor = i
			break
		}
	}
	_, cmd := m.Update(keyMsg("a"))
	if cmd == nil {
		t.Fatal("[a] should produce a spawn cmd")
	}
	doneMsg := cmd().(coordSpawnDoneMsg)
	if doneMsg.err == nil {
		t.Fatal("expected err on dispatch failure")
	}
	updated, _ := m.Update(doneMsg)
	mm := updated.(Model)
	if mm.flash == nil || !mm.flash.isErr {
		t.Errorf("expected error flash; got %+v", mm.flash)
	}
	if !strings.Contains(mm.flash.text, "demo") {
		t.Errorf("flash should reference project name; got %q", mm.flash.text)
	}
	if mm.pendingAttach != "" {
		t.Errorf("pendingAttach must NOT be set on dispatch failure; got %q", mm.pendingAttach)
	}
}

// TestKeyA_ProjectRow_DispatchOutputUnparseable regresses the agent-ID
// parse-failure path: dispatch returns 0 but stdout shape drifted (no
// "agent <id> spawned" line). We must surface a parse error rather
// than silently attaching to a wrong/empty session.
func TestKeyA_ProjectRow_DispatchOutputUnparseable(t *testing.T) {
	withFleetHome(t)
	stub := &stubFleetCmd{
		stubbed: func(args []string) tea.Msg {
			return coordSpawnDoneMsgFromArgs(args, "weird unexpected output\n", nil)
		},
	}
	stub.install(t)

	m := New("test")
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{{Name: "demo"}},
	}
	for i, r := range m.dashboardRows() {
		if r.kind == rowProject {
			m.dashCursor = i
			break
		}
	}
	_, cmd := m.Update(keyMsg("a"))
	doneMsg := cmd().(coordSpawnDoneMsg)
	if doneMsg.err == nil {
		t.Fatal("expected err on unparseable dispatch output")
	}
	if !strings.Contains(doneMsg.err.Error(), "missing agent ID") {
		t.Errorf("err should reference missing agent ID; got %q", doneMsg.err.Error())
	}
}

// TestKeyA_TaskRow_FlashesNotApplicable regresses issue #60's hard
// non-goal: [a] on a task row must still flash "doesn't apply". The
// new project-row branch must not accidentally swallow task rows.
func TestKeyA_TaskRow_FlashesNotApplicable(t *testing.T) {
	pdir := withFleetHome(t)
	seedTasks(t, pdir, "demo", TaskCounts{Todo: 1})
	(&stubSessionAlive{}).install(t)
	stub := &stubFleetCmd{}
	stub.install(t)

	m := New("test")
	m.dashboard = scanDashboard(time.Now())
	m.expanded = map[string]bool{"demo": true} // expand so task rows are present
	taskIdx := -1
	for i, r := range m.dashboardRows() {
		if r.kind == rowTask {
			taskIdx = i
			break
		}
	}
	if taskIdx < 0 {
		t.Fatal("no task row in dashboardRows")
	}
	m.dashCursor = taskIdx

	updated, cmd := m.Update(keyMsg("a"))
	mm := updated.(Model)
	if cmd != nil {
		t.Errorf("[a] on task row should not produce a cmd; got non-nil")
	}
	if mm.pendingAttach != "" {
		t.Errorf("pendingAttach must NOT be set on task row; got %q", mm.pendingAttach)
	}
	if mm.flash == nil || !mm.flash.isErr {
		t.Errorf("[a] on task row should flash an error; got %+v", mm.flash)
	}
	if strings.Contains(mm.flash.text, "tasks/projects") {
		t.Errorf("flash text leaks the old 'tasks/projects' wording: %q", mm.flash.text)
	}
	if len(stub.calls) != 0 {
		t.Errorf("[a] on task row should NOT shell out; got %v", stub.calls)
	}
}

// TestHelpOverlay_AttachDescriptionMentionsProjects pins issue #60: the
// [?] help overlay must describe [a] as covering projects (coord),
// agents (tmux), AND workers (peek). Old text was "(agents) or peek
// (workers)" and would silently mislead operators about the new
// project-row capability.
func TestHelpOverlay_AttachDescriptionMentionsProjects(t *testing.T) {
	out := renderHelpOverlay(120)
	if !strings.Contains(out, "projects") {
		t.Errorf("help overlay [a] description should mention projects; got:\n%s", out)
	}
	// Sanity: the agent + worker semantics must still be advertised.
	if !strings.Contains(out, "agents") {
		t.Errorf("help overlay [a] description should still mention agents; got:\n%s", out)
	}
	if !strings.Contains(out, "workers") {
		t.Errorf("help overlay [a] description should still mention workers; got:\n%s", out)
	}
}

// TestCoordCwdForProject_PrefersAgentRecordCwd verifies the cwd
// resolution: when an agent record carries the matching Project tag
// AND a Cwd, we reuse it. Otherwise fall back to TUI's cwd.
func TestCoordCwdForProject_PrefersAgentRecordCwd(t *testing.T) {
	r := agent.New("a1")
	r.Project = "demo"
	r.Cwd = "/Users/op/projects/demo"
	got := coordCwdForProject([]*agent.Record{r}, "demo")
	if got != "/Users/op/projects/demo" {
		t.Errorf("coordCwdForProject should prefer agent record's Cwd; got %q", got)
	}
}

// TestCoordCwdForProject_FallsBackToWd: with no matching agent record,
// we fall through to os.Getwd().
func TestCoordCwdForProject_FallsBackToWd(t *testing.T) {
	got := coordCwdForProject(nil, "ghost-project")
	wd, _ := os.Getwd()
	if got != wd {
		t.Errorf("coordCwdForProject with no records should fall back to os.Getwd(); got %q want %q",
			got, wd)
	}
}

// TestKeyA_ProjectRow_ForwardsCwdFromAgentRecord pins the wiring:
// when an existing agent record carries a Cwd matching the project,
// the spawned coord's `fleet dispatch` invocation must include
// --cwd <that path>. Otherwise the coord lands in the TUI's cwd
// (often a worktree dir for fleet developers) and /coordinator's
// directory-relative shell calls resolve to the wrong place.
func TestKeyA_ProjectRow_ForwardsCwdFromAgentRecord(t *testing.T) {
	withFleetHome(t)
	(&stubSessionAlive{}).install(t)

	stub := &stubFleetCmd{
		stubbed: func(args []string) tea.Msg {
			out := "agent feedface spawned\n  tmux: fleet-feedface\n"
			return coordSpawnDoneMsgFromArgs(args, out, nil)
		},
	}
	stub.install(t)

	// Existing worker agent for "demo" carries Cwd = a known path.
	// coordCwdForProject should pick this path and forward it as --cwd.
	worker := sampleAgent("worker01")
	worker.Project = "demo"
	worker.Cwd = "/Users/op/projects/demo"

	m := New("test")
	m.records = []*agent.Record{worker}
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{{Name: "demo"}}, // no CoordID — spawn path
	}
	for i, r := range m.dashboardRows() {
		if r.kind == rowProject && r.project != nil && r.project.Name == "demo" {
			m.dashCursor = i
			break
		}
	}

	_, cmd := m.Update(keyMsg("a"))
	if cmd == nil {
		t.Fatal("[a] should produce a spawn cmd")
	}
	_ = cmd() // drain so stub.calls populates

	if len(stub.calls) != 1 {
		t.Fatalf("expected 1 fleet call; got %d", len(stub.calls))
	}
	args := stub.calls[0]
	hasCwd := false
	for i, a := range args {
		if a == "--cwd" && i+1 < len(args) && args[i+1] == "/Users/op/projects/demo" {
			hasCwd = true
			break
		}
	}
	if !hasCwd {
		t.Errorf("dispatch args missing --cwd /Users/op/projects/demo: %v", args)
	}
}

// TestFindExistingCoordForProject_ReturnsAliveMatch pins issue #63's
// idempotency primitive: a record tagged task_id=coord-<project> with
// project=<project> and an alive tmux session is the in-flight coord —
// findExistingCoordForProject returns it (record, true).
func TestFindExistingCoordForProject_ReturnsAliveMatch(t *testing.T) {
	(&stubSessionAlive{}).install(t)
	(&stubProjectTreeExists{}).install(t)
	(&stubCoordSpawnMarker{markers: map[string]string{"demo": "c0ffeec0"}}).install(t)

	r := agent.New("c0ffeec0")
	r.Project = "demo"
	r.TaskID = "coord-demo"
	r.TmuxSession = "fleet-c0ffeec0"

	got, ok := findExistingCoordForProject([]*agent.Record{r}, "demo")
	if !ok {
		t.Fatal("expected found=true; got false")
	}
	if got != r {
		t.Errorf("got record = %+v; want pointer to seeded r", got)
	}
}

// TestFindExistingCoordForProject_ReturnsFalseOnNoMatch pins the
// negative path: no record with the right task_id → no match.
func TestFindExistingCoordForProject_ReturnsFalseOnNoMatch(t *testing.T) {
	(&stubSessionAlive{}).install(t)
	(&stubProjectTreeExists{}).install(t)
	other := agent.New("12345678")
	other.Project = "demo"
	other.TaskID = "regular-task"
	other.TmuxSession = "fleet-12345678"

	if _, ok := findExistingCoordForProject([]*agent.Record{other}, "demo"); ok {
		t.Error("expected found=false when no record carries coord-<project> task_id")
	}
}

// TestFindExistingCoordForProject_ReturnsFalseOnDeadSession pins that
// dead-session matches are NOT counted — caller must spawn a fresh
// coord rather than attaching to a graveyard.
func TestFindExistingCoordForProject_ReturnsFalseOnDeadSession(t *testing.T) {
	(&stubSessionAlive{dead: map[string]bool{"fleet-deadc0de": true}}).install(t)
	(&stubProjectTreeExists{}).install(t)
	(&stubCoordSpawnMarker{markers: map[string]string{"demo": "deadc0de"}}).install(t)
	// Also stub session probe so the conservative re-probe path
	// (sessionProbeOrAliveFn) returns "definitively dead" rather than
	// "probe error → alive".
	(&stubSessionProbe{dead: map[string]bool{"fleet-deadc0de": true}}).install(t)

	r := agent.New("deadc0de")
	r.Project = "demo"
	r.TaskID = "coord-demo"
	r.TmuxSession = "fleet-deadc0de"

	if _, ok := findExistingCoordForProject([]*agent.Record{r}, "demo"); ok {
		t.Error("dead session should not count as an in-flight coord match")
	}
}

// TestFindExistingCoordForProject_ProjectMismatchedSkipped guards
// against cross-project leakage: a record tagged coord-foo must NOT
// match a search for "bar". Project prefix in task_id is the gate.
func TestFindExistingCoordForProject_ProjectMismatchedSkipped(t *testing.T) {
	(&stubSessionAlive{}).install(t)
	(&stubProjectTreeExists{}).install(t)
	r := agent.New("11111111")
	r.Project = "foo"
	r.TaskID = "coord-foo"
	r.TmuxSession = "fleet-11111111"

	if _, ok := findExistingCoordForProject([]*agent.Record{r}, "bar"); ok {
		t.Error("coord-foo record must not match bar")
	}
}

// TestKeyA_ProjectRow_FindsExistingCoordByTaskID pins issue #63's
// idempotency: a second [a] press during the skill-boot window
// (CoordID empty because lock body hasn't published yet) finds the
// in-flight record by task_id and attaches WITHOUT spawning a duplicate.
func TestKeyA_ProjectRow_FindsExistingCoordByTaskID(t *testing.T) {
	(&stubSessionAlive{}).install(t)
	(&stubProjectTreeExists{}).install(t)
	(&stubCoordSpawnMarker{markers: map[string]string{"demo": "c00bf001"}}).install(t)
	stub := &stubFleetCmd{}
	stub.install(t)

	// Existing in-flight coord record: tagged coord-demo, alive session,
	// no lock body yet (CoordID empty on the dashboard row).
	coord := agent.New("c00bf001")
	coord.Project = "demo"
	coord.TaskID = "coord-demo"
	coord.TmuxSession = "fleet-c00bf001"

	m := New("test")
	m.records = []*agent.Record{coord}
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{{Name: "demo"}}, // no CoordID
	}
	for i, r := range m.dashboardRows() {
		if r.kind == rowProject && r.project != nil && r.project.Name == "demo" {
			m.dashCursor = i
			break
		}
	}

	updated, cmd := m.Update(keyMsg("a"))
	mm := updated.(Model)

	if mm.pendingAttach != "fleet-c00bf001" {
		t.Errorf("pendingAttach = %q; want fleet-c00bf001 (in-flight coord)", mm.pendingAttach)
	}
	if cmd == nil {
		t.Error("expected tea.Quit cmd from task_id-fallback attach")
	}
	if len(stub.calls) != 0 {
		t.Errorf("[a] on in-flight coord must NOT shell out to dispatch; got %v", stub.calls)
	}
}

// TestKeyA_ProjectRow_AttachesImmediatelyAfterDispatch pins the new
// flow: dispatch returns → coordSpawnDoneMsg has no err → Update sets
// pendingAttach. No lock-poll, no timeout banner under normal operation.
func TestKeyA_ProjectRow_AttachesImmediatelyAfterDispatch(t *testing.T) {
	withFleetHome(t)
	(&stubSessionAlive{}).install(t)

	stub := &stubFleetCmd{
		stubbed: func(args []string) tea.Msg {
			out := "agent abcd1234 spawned\n  task: coord-demo\n  project: demo\n"
			return coordSpawnDoneMsgFromArgs(args, out, nil)
		},
	}
	stub.install(t)

	m := New("test")
	m.dashboard = &Snapshot{Projects: []*ProjectRow{{Name: "demo"}}}
	for i, r := range m.dashboardRows() {
		if r.kind == rowProject {
			m.dashCursor = i
			break
		}
	}
	_, cmd := m.Update(keyMsg("a"))
	if cmd == nil {
		t.Fatal("[a] should produce spawn cmd")
	}
	doneMsg := cmd().(coordSpawnDoneMsg)
	if doneMsg.err != nil {
		t.Fatalf("expected no err on successful dispatch; got %v", doneMsg.err)
	}
	updated, cmd2 := m.Update(doneMsg)
	mm := updated.(Model)
	if mm.pendingAttach != "fleet-abcd1234" {
		t.Errorf("pendingAttach = %q; want fleet-abcd1234 (immediate attach)", mm.pendingAttach)
	}
	if cmd2 == nil {
		t.Error("expected tea.Quit cmd after coordSpawnDoneMsg")
	}
	// Banner must NOT mention "timed out" under the new flow.
	if mm.flash != nil && strings.Contains(mm.flash.text, "timed out") {
		t.Errorf("flash should not mention 'timed out'; got %q", mm.flash.text)
	}
}

// TestKeyA_ProjectRow_InFlightSpawn_RejectsDuplicate pins codex
// iter-3 P2 follow-up: while a coord-spawn dispatch is in flight (cmd
// goroutine launched but coordSpawnDoneMsg hasn't arrived yet), the
// agent record + marker file don't exist on disk. A second [a] press
// during this window must NOT launch a duplicate dispatch — it should
// flash a "spawn in flight" hint and let the operator wait.
func TestKeyA_ProjectRow_InFlightSpawn_RejectsDuplicate(t *testing.T) {
	withFleetHome(t)
	(&stubSessionAlive{}).install(t)
	(&stubProjectTreeExists{}).install(t)

	stub := &stubFleetCmd{
		stubbed: func(args []string) tea.Msg {
			// Don't return — leave the cmd hanging so we can probe the
			// in-flight state. Tests don't actually run the goroutine.
			return coordSpawnDoneMsgFromArgs(args, "agent abcd1234 spawned\n", nil)
		},
	}
	stub.install(t)

	m := New("test")
	m.dashboard = &Snapshot{Projects: []*ProjectRow{{Name: "demo"}}}
	for i, r := range m.dashboardRows() {
		if r.kind == rowProject {
			m.dashCursor = i
			break
		}
	}
	// First [a] launches the cmd.
	updated, cmd := m.Update(keyMsg("a"))
	if cmd == nil {
		t.Fatal("first [a] should produce a spawn cmd")
	}
	mm := updated.(Model)
	if !mm.coordSpawnInFlight["demo"] {
		t.Fatal("after first [a], coordSpawnInFlight[demo] should be true")
	}
	// Second [a] WITHOUT draining the first cmd. Should be rejected.
	beforeCalls := len(stub.calls)
	updated2, cmd2 := mm.Update(keyMsg("a"))
	mm2 := updated2.(Model)
	if cmd2 != nil {
		t.Error("second [a] during in-flight should not produce a cmd")
	}
	if mm2.flash == nil || !mm2.flash.isErr {
		t.Fatalf("second [a] should flash; got %+v", mm2.flash)
	}
	if !strings.Contains(mm2.flash.text, "in flight") {
		t.Errorf("flash should mention 'in flight'; got %q", mm2.flash.text)
	}
	if len(stub.calls) != beforeCalls {
		t.Errorf("second [a] must NOT shell out; got %d calls (was %d)", len(stub.calls), beforeCalls)
	}
}

// TestModel_CoordSpawnDoneMsg_ClearsInFlight pins that the in-flight
// gate clears regardless of err/success — once the dispatch attempt
// resolves, subsequent [a] presses go through the full lookup again.
func TestModel_CoordSpawnDoneMsg_ClearsInFlight(t *testing.T) {
	(&stubSessionAlive{}).install(t)
	// Marker stub so writeCoordSpawnMarkerFn doesn't hit FLEET_HOME.
	(&stubWriteCoordSpawnMarker{}).install(t)

	m := New("test")
	m.coordSpawnInFlight = map[string]bool{"demo": true}

	updated, _ := m.Update(coordSpawnDoneMsg{
		projectName: "demo",
		agentID:     "abcd1234",
		session:     "fleet-abcd1234",
	})
	mm := updated.(Model)
	if mm.coordSpawnInFlight["demo"] {
		t.Error("coordSpawnInFlight[demo] should be cleared after coordSpawnDoneMsg")
	}
}

// TestModel_CoordSpawnDoneMsg_WritesMarker pins that the success
// path writes the coord-spawn marker before tea.Quit so the
// dashboard's task_id fallback can validate the agent ID on its
// next refresh.
func TestModel_CoordSpawnDoneMsg_WritesMarker(t *testing.T) {
	(&stubSessionAlive{}).install(t)
	markerStub := &stubWriteCoordSpawnMarker{}
	markerStub.install(t)

	m := New("test")
	updated, _ := m.Update(coordSpawnDoneMsg{
		projectName: "demo",
		agentID:     "abcd1234",
		session:     "fleet-abcd1234",
	})
	mm := updated.(Model)
	if mm.pendingAttach != "fleet-abcd1234" {
		t.Errorf("pendingAttach = %q; want fleet-abcd1234", mm.pendingAttach)
	}
	if got := markerStub.calls["demo"]; got != "abcd1234" {
		t.Errorf("marker call for demo = %q; want abcd1234", got)
	}
}

// TestKeyA_ProjectRow_InitErrShowsBanner pins the init-failure path:
// when state.EnsureProjectInitialized fails (e.g. permission denied
// on parent dir), [a] must NOT shell out to dispatch — surface the
// error as a banner so the operator sees what to fix.
func TestKeyA_ProjectRow_InitErrShowsBanner(t *testing.T) {
	// Force EnsureProjectInitialized to fail by pointing FLEET_HOME at
	// a path where MkdirAll cannot succeed. A regular file as parent
	// triggers ENOTDIR.
	tmp := t.TempDir()
	notDirPath := filepath.Join(tmp, "fleet-as-file")
	if err := os.WriteFile(notDirPath, []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLEET_HOME", notDirPath)

	(&stubSessionAlive{}).install(t)
	stub := &stubFleetCmd{}
	stub.install(t)

	m := New("test")
	m.dashboard = &Snapshot{Projects: []*ProjectRow{{Name: "demo"}}}
	for i, r := range m.dashboardRows() {
		if r.kind == rowProject {
			m.dashCursor = i
			break
		}
	}
	updated, cmd := m.Update(keyMsg("a"))
	mm := updated.(Model)

	if mm.flash == nil || !mm.flash.isErr {
		t.Fatalf("init failure should flash; got %+v", mm.flash)
	}
	if !strings.Contains(mm.flash.text, "init failed") {
		t.Errorf("flash should mention 'init failed'; got %q", mm.flash.text)
	}
	if cmd != nil {
		t.Error("init failure should not produce a cmd")
	}
	if len(stub.calls) != 0 {
		t.Errorf("init failure must NOT shell out to dispatch; got %v", stub.calls)
	}
}

// TestKeyA_ProjectRow_DeadSessionAfterSpawn_FlashesNoAttach pins codex
// iter-1 P2: dispatch returns 0 with the agent-ID line, but the new
// tmux session is dead by the time coordSpawnDoneMsg arrives (claude
// exited at startup — bad --command flag, OOM, missing binary). We
// must NOT pendingAttach + tea.Quit and exec tmux against a dead
// session — surface a flash and stay in the TUI.
func TestKeyA_ProjectRow_DeadSessionAfterSpawn_FlashesNoAttach(t *testing.T) {
	withFleetHome(t)
	// Spawn writes a session name, but our liveness probe says it's
	// already dead — claude exited between dispatch returning and the
	// coordSpawnDoneMsg arriving in Update.
	(&stubSessionAlive{dead: map[string]bool{"fleet-deadbeef": true}}).install(t)

	stub := &stubFleetCmd{
		stubbed: func(args []string) tea.Msg {
			out := "agent deadbeef spawned\n  tmux: fleet-deadbeef\n"
			return coordSpawnDoneMsgFromArgs(args, out, nil)
		},
	}
	stub.install(t)

	m := New("test")
	m.dashboard = &Snapshot{Projects: []*ProjectRow{{Name: "demo"}}}
	for i, r := range m.dashboardRows() {
		if r.kind == rowProject {
			m.dashCursor = i
			break
		}
	}
	_, cmd := m.Update(keyMsg("a"))
	doneMsg := cmd().(coordSpawnDoneMsg)
	if doneMsg.err != nil {
		t.Fatalf("expected dispatch ok; got err %v", doneMsg.err)
	}
	updated, cmd2 := m.Update(doneMsg)
	mm := updated.(Model)

	if mm.pendingAttach != "" {
		t.Errorf("pendingAttach must NOT be set when post-spawn session is dead; got %q", mm.pendingAttach)
	}
	if mm.flash == nil || !mm.flash.isErr {
		t.Fatalf("expected error flash; got %+v", mm.flash)
	}
	if !strings.Contains(mm.flash.text, "not alive") {
		t.Errorf("flash should mention 'not alive'; got %q", mm.flash.text)
	}
	// Refresh cmd is fine (loadAgentsCmd) — operator can investigate
	// via the right-column row. Just must not be tea.Quit.
	if cmd2 != nil {
		msg := cmd2()
		if _, isQuit := msg.(tea.QuitMsg); isQuit {
			t.Error("expected loadAgentsCmd refresh, NOT tea.Quit on dead post-spawn session")
		}
	}
}

// TestKeyA_ProjectRow_DispatchInvocationCarriesCoordSpawnFlag pins
// codex iter-1 P2: the TUI's auto-spawn shells out with --coord-spawn
// so the dispatch CLI accepts the reserved "coord-<project>" task_id
// prefix. Without the flag, runDispatch rejects the prefix.
func TestKeyA_ProjectRow_DispatchInvocationCarriesCoordSpawnFlag(t *testing.T) {
	withFleetHome(t)
	(&stubSessionAlive{}).install(t)

	stub := &stubFleetCmd{
		stubbed: func(args []string) tea.Msg {
			out := "agent c0c0c0c0 spawned\n  tmux: fleet-c0c0c0c0\n"
			return coordSpawnDoneMsgFromArgs(args, out, nil)
		},
	}
	stub.install(t)

	m := New("test")
	m.dashboard = &Snapshot{Projects: []*ProjectRow{{Name: "demo"}}}
	for i, r := range m.dashboardRows() {
		if r.kind == rowProject {
			m.dashCursor = i
			break
		}
	}
	_, cmd := m.Update(keyMsg("a"))
	_ = cmd()

	if len(stub.calls) != 1 {
		t.Fatalf("expected 1 dispatch call; got %d", len(stub.calls))
	}
	hasCoordSpawn := false
	for _, a := range stub.calls[0] {
		if a == "--coord-spawn" {
			hasCoordSpawn = true
			break
		}
	}
	if !hasCoordSpawn {
		t.Errorf("dispatch args missing --coord-spawn (would be rejected by reserved-prefix gate): %v", stub.calls[0])
	}
}
