package tui

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/workers"
)

// keyMsg constructs a tea.KeyMsg matching what bubbletea emits for a
// given printable key — without bubbletea, "h" comes through as
// {Type: KeyRunes, Runes: ['h']}. Arrow keys are dedicated KeyType
// constants (their String() resolves to "up"/"down"/"left"/"right"),
// not runes; tests that simulate arrow keys must construct them with
// the matching Type so handleKey's switch lands on the arrow case
// rather than fall through to the runes branch.
func keyMsg(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "backspace":
		return tea.KeyMsg{Type: tea.KeyBackspace}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "left":
		return tea.KeyMsg{Type: tea.KeyLeft}
	case "right":
		return tea.KeyMsg{Type: tea.KeyRight}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

func makeModelWithAgents(records ...*agent.Record) Model {
	m := New("test")
	// v0.2 dashboard is the only view (issue #53). Records appear in the
	// right column under "v0.1 agents — N active"; dashCursor must land
	// on the first agent row for [h]/[x]/[a] to dispatch to it.
	//
	// Issue #55 union: unifiedProjects() now synthesizes a project row
	// for any agent.Project tag absent from m.dashboard. We snap the
	// cursor onto the first rowAgent rather than assuming index 0 —
	// the synthetic-project-row offset is implementation detail the
	// action tests don't care about.
	m.records = records
	for i, r := range m.dashboardRows() {
		if r.kind == rowAgent {
			m.dashCursor = i
			return m
		}
	}
	m.dashCursor = 0
	return m
}

func sampleAgent(id string) *agent.Record {
	r := agent.New(id)
	r.TaskID = "demo"
	r.Project = "proj"
	r.TmuxSession = "fleet-" + id
	r.SpawnedAt = time.Now().UTC()
	return r
}

// stubFleetCmd replaces runFleetCmd for tests so we don't actually
// fork `fleet`. Returns a captured args slice + a controllable msg.
type stubFleetCmd struct {
	calls   [][]string
	stubbed func(args []string) tea.Msg
}

func (s *stubFleetCmd) install(t *testing.T) {
	t.Helper()
	prev := runFleetCmd
	runFleetCmd = func(args []string, msgFn func(string, error) tea.Msg) tea.Cmd {
		s.calls = append(s.calls, append([]string(nil), args...))
		return func() tea.Msg {
			if s.stubbed != nil {
				return s.stubbed(args)
			}
			return msgFn("ok", nil)
		}
	}
	t.Cleanup(func() { runFleetCmd = prev })
}

// -- [h] handoff --------------------------------------------------------
//
// Inversion contract (2026-05-27): [h] is a PROJECT-LEVEL action.
// Pressing [h] on a project row with a live coord shells out to
// `fleet handoff <coordID>`. Pressing [h] on an agent row REJECTS with
// a hint pointing back to the project row. Task / worker / separator
// rows preserve the existing "doesn't apply" flash with updated wording.
//
// Rationale: coords are 1-per-project, so the operator's mental model
// for handoff is "hand off this project's coord," not "hand off this
// loose-agent record." The left-panel project row is the natural
// surface for that operation.

// projectRowModelWithCoord builds a Model whose dashboard carries a
// single project row with the supplied CoordID, plus the supplied agent
// records loaded into m.records. The cursor lands on the project row so
// [h] dispatches from there. aliveByID is threaded so deriveStatus
// produces the test-controlled handoff state.
//
// Mirrors projectRowModel but pins the coord via the dashboard snapshot
// (CoordID-driven), which is the path the project-row [h] handler must
// resolve through findRecordByID + deriveStatus.
func projectRowModelWithCoord(t *testing.T, project, coordID string, records []*agent.Record, aliveByID map[string]bool) Model {
	t.Helper()
	m := New("test")
	m.records = records
	m.aliveByID = aliveByID
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{{Name: project, CoordID: coordID}},
	}
	for i, r := range m.dashboardRows() {
		if r.kind == rowProject && r.project != nil && r.project.Name == project {
			m.dashCursor = i
			return m
		}
	}
	t.Fatalf("no rowProject for %q in dashboardRows; got %+v", project, m.dashboardRows())
	return m
}

func TestActionHandoff_OnProjectRow_WithLiveCoord_FiresHandoff(t *testing.T) {
	stub := &stubFleetCmd{}
	stub.install(t)

	coord := sampleAgent("coord001")
	coord.Project = "demo"
	m := projectRowModelWithCoord(t, "demo", "coord001", []*agent.Record{coord}, nil)

	updated, cmd := m.Update(keyMsg("h"))
	if cmd == nil {
		t.Fatal("expected a tea.Cmd from [h] on project row with live coord, got nil")
	}
	_ = cmd()
	if len(stub.calls) != 1 || len(stub.calls[0]) != 2 ||
		stub.calls[0][0] != "handoff" || stub.calls[0][1] != "coord001" {
		t.Errorf("expected ['handoff', 'coord001'], got %v", stub.calls)
	}
	if _, ok := updated.(Model); !ok {
		t.Errorf("Update returned non-Model: %T", updated)
	}
}

func TestActionHandoff_OnProjectRow_NoLiveCoord_Flashes(t *testing.T) {
	stub := &stubFleetCmd{}
	stub.install(t)
	// Pure-legacy project (no v0.2 dir) so unifiedProjects synthesizes a
	// row without a CoordID. projectRowModel walks the synthetic path.
	(&stubProjectTreeExists{missing: map[string]bool{"orphan": true}}).install(t)

	// Tag at least one agent.Record with "orphan" so unifiedProjects
	// creates the synthetic row, but DON'T set it as the coord (no
	// task_id=coord-orphan + marker).
	a := agent.New("loose001")
	a.Project = "orphan"
	a.TmuxSession = "fleet-loose001"
	m := projectRowModel(t, "orphan", []*agent.Record{a}, nil)

	updated, cmd := m.Update(keyMsg("h"))
	mm := updated.(Model)
	if cmd != nil {
		t.Errorf("[h] on project row with no coord must NOT shell out; got cmd")
	}
	if len(stub.calls) != 0 {
		t.Errorf("expected no fleet invocations, got %v", stub.calls)
	}
	if mm.flash == nil || !mm.flash.isErr {
		t.Fatalf("expected error flash, got %+v", mm.flash)
	}
	if !strings.Contains(mm.flash.text, "orphan") || !strings.Contains(mm.flash.text, "no coord") {
		t.Errorf("flash should name the project and explain no coord; got %q", mm.flash.text)
	}
}

func TestActionHandoff_OnAgentRow_RejectsWithHint(t *testing.T) {
	stub := &stubFleetCmd{}
	stub.install(t)

	m := makeModelWithAgents(sampleAgent("agent01"), sampleAgent("agent02"))
	// Move cursor onto an agent row.
	for i, r := range m.dashboardRows() {
		if r.kind == rowAgent && r.agent != nil && r.agent.ID == "agent02" {
			m.dashCursor = i
			break
		}
	}

	updated, cmd := m.Update(keyMsg("h"))
	mm := updated.(Model)
	if cmd != nil {
		t.Errorf("[h] on agent row must NOT shell out (inverted); got cmd")
	}
	if len(stub.calls) != 0 {
		t.Errorf("expected no fleet invocations on agent row; got %v", stub.calls)
	}
	if mm.flash == nil || !mm.flash.isErr {
		t.Fatalf("expected error flash on agent-row [h], got %+v", mm.flash)
	}
	if !strings.Contains(mm.flash.text, "project") {
		t.Errorf("flash should redirect operator to project rows; got %q", mm.flash.text)
	}
}

func TestActionHandoff_OnTaskRow_FlashUnchanged(t *testing.T) {
	stub := &stubFleetCmd{}
	stub.install(t)
	pdir := withFleetHome(t)
	seedTasks(t, pdir, "demo", TaskCounts{Todo: 1})

	m := New("test")
	m.width = 130
	m.height = 30
	m.dashboard = scanDashboard(time.Now())
	// Expand "demo" so a task row exists; then walk the row list to it.
	m.expanded = map[string]bool{"demo": true}
	taskCursor := -1
	for i, r := range m.dashboardRows() {
		if r.kind == rowTask {
			taskCursor = i
			break
		}
	}
	if taskCursor < 0 {
		t.Fatalf("no task row materialised; rows=%+v", m.dashboardRows())
	}
	m.dashCursor = taskCursor

	updated, cmd := m.Update(keyMsg("h"))
	mm := updated.(Model)
	if cmd != nil {
		t.Errorf("[h] on task row must NOT shell out; got cmd")
	}
	if len(stub.calls) != 0 {
		t.Errorf("expected zero fleet calls on task row; got %v", stub.calls)
	}
	if mm.flash == nil || !mm.flash.isErr {
		t.Fatalf("expected error flash, got %+v", mm.flash)
	}
	if !strings.Contains(mm.flash.text, "project") {
		t.Errorf("flash should point at project rows; got %q", mm.flash.text)
	}
}

func TestActionHandoff_OnWorkerRow_FlashUnchanged(t *testing.T) {
	stub := &stubFleetCmd{}
	stub.install(t)

	m := New("test")
	m.width = 130
	m.height = 30
	m.dashboard = &Snapshot{
		Workers: []*WorkerRow{{ID: "w0001", Project: "demo", Slug: "fix-bug"}},
	}
	for i, r := range m.dashboardRows() {
		if r.kind == rowWorker {
			m.dashCursor = i
			break
		}
	}

	updated, cmd := m.Update(keyMsg("h"))
	mm := updated.(Model)
	if cmd != nil {
		t.Errorf("[h] on worker row must NOT shell out; got cmd")
	}
	if len(stub.calls) != 0 {
		t.Errorf("expected zero fleet calls on worker row; got %v", stub.calls)
	}
	if mm.flash == nil || !mm.flash.isErr {
		t.Fatalf("expected error flash, got %+v", mm.flash)
	}
	if !strings.Contains(mm.flash.text, "project") {
		t.Errorf("flash should point at project rows; got %q", mm.flash.text)
	}
}

func TestActionHandoff_OnSeparator_FlashUnchanged(t *testing.T) {
	stub := &stubFleetCmd{}
	stub.install(t)
	// Build a row list with two projects so an idle separator naturally
	// appears (one active, one idle below the activity threshold).
	pdir := withFleetHome(t)
	// "active" project with a recent worker so it classifies active.
	seedTasks(t, pdir, "active", TaskCounts{InProgress: 1})
	seedWorker(t, pdir, "active", "wip-task", workers.State{
		Phase:     workers.PhaseTDDGreen,
		UpdatedAt: time.Now().UTC(),
	})
	// "stale" project with no fresh signals.
	seedTasks(t, pdir, "stale", TaskCounts{Todo: 1})
	dir := filepath.Join(pdir, "stale", "tasks.md")
	// Push the tasks.md mtime way back so projectAddedAt returns an old
	// value and the project classifies as IDLE.
	old := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	m := New("test")
	m.width = 130
	m.height = 30
	m.dashboard = scanDashboard(time.Now())

	sepIdx := -1
	for i, r := range m.dashboardRows() {
		if r.kind == rowSeparator {
			sepIdx = i
			break
		}
	}
	if sepIdx < 0 {
		t.Skip("no separator materialised under current activity classifier — skip")
	}
	m.dashCursor = sepIdx

	updated, cmd := m.Update(keyMsg("h"))
	mm := updated.(Model)
	if cmd != nil {
		t.Errorf("[h] on separator must NOT shell out; got cmd")
	}
	if len(stub.calls) != 0 {
		t.Errorf("expected zero fleet calls on separator; got %v", stub.calls)
	}
	if mm.flash == nil || !mm.flash.isErr {
		t.Fatalf("expected error flash on separator [h]; got %+v", mm.flash)
	}
}

// TestActionHandoff_AutoRedGated_OnProjectCoord pins the moved gate:
// the auto-red / precompact handoff-already-in-flight check that
// previously lived on the agent-row path must move to the project-row
// path so a coord with a committed handoff journal can't be re-handed
// off via [h] on its project.
func TestActionHandoff_AutoRedGated_OnProjectCoord(t *testing.T) {
	stub := &stubFleetCmd{}
	stub.install(t)

	coord := sampleAgent("coord001")
	coord.Project = "demo"
	hoType := "auto-red"
	coord.HandoffType = &hoType
	m := projectRowModelWithCoord(t, "demo", "coord001", []*agent.Record{coord}, nil)

	updated, cmd := m.Update(keyMsg("h"))
	mm := updated.(Model)
	if cmd != nil {
		t.Errorf("[h] on project with auto-red coord must NOT shell out; got cmd")
	}
	if len(stub.calls) != 0 {
		t.Errorf("expected zero fleet calls; got %v", stub.calls)
	}
	if mm.flash == nil || !mm.flash.isErr {
		t.Fatalf("expected error flash on auto-red gate, got %+v", mm.flash)
	}
	if !strings.Contains(mm.flash.text, "drain") {
		t.Errorf("flash should mention `fleet drain`; got %q", mm.flash.text)
	}
}

// TestActionHandoff_PrecompactGated_OnProjectCoord same as the auto-red
// gate but for the precompact handoff state.
func TestActionHandoff_PrecompactGated_OnProjectCoord(t *testing.T) {
	stub := &stubFleetCmd{}
	stub.install(t)

	coord := sampleAgent("coord001")
	coord.Project = "demo"
	hoType := "precompact"
	coord.HandoffType = &hoType
	m := projectRowModelWithCoord(t, "demo", "coord001", []*agent.Record{coord}, nil)

	_, cmd := m.Update(keyMsg("h"))
	if cmd != nil {
		t.Errorf("[h] on project with precompact coord must NOT shell out; got cmd")
	}
	if len(stub.calls) != 0 {
		t.Errorf("expected zero fleet calls; got %v", stub.calls)
	}
}

func TestActionHandoff_OnEmptyList_Noop(t *testing.T) {
	stub := &stubFleetCmd{}
	stub.install(t)

	m := makeModelWithAgents() // no agents, no projects
	_, cmd := m.Update(keyMsg("h"))
	if cmd != nil {
		t.Errorf("expected nil cmd on [h] with empty list")
	}
	if len(stub.calls) != 0 {
		t.Errorf("expected no fleet invocations, got %v", stub.calls)
	}
}

// -- [a] attach ---------------------------------------------------------

// stubSessionAlive replaces sessionAliveFn (used by [a] attach) so
// tests don't shell out to tmux. Returns alive=true unless the
// session is in dead.
type stubSessionAlive struct {
	dead map[string]bool
}

func (s *stubSessionAlive) install(t *testing.T) {
	t.Helper()
	prev := sessionAliveFn
	sessionAliveFn = func(session string) bool {
		return !s.dead[session]
	}
	t.Cleanup(func() { sessionAliveFn = prev })
}

// stubProjectTreeExists replaces projectTreeExistsFn for tests so we
// don't need to seed real directories under FLEET_HOME just to exercise
// the dashboard task_id fallback signal. Default returnVal=true so
// tests don't have to opt in to "yes the project tree exists" — the
// gate exists to keep LEGACY records out, not to break ordinary tests.
type stubProjectTreeExists struct {
	missing map[string]bool // names where the gate should return false
}

func (s *stubProjectTreeExists) install(t *testing.T) {
	t.Helper()
	prev := projectTreeExistsFn
	projectTreeExistsFn = func(projectName string) bool {
		return !s.missing[projectName]
	}
	t.Cleanup(func() { projectTreeExistsFn = prev })
}

// stubCoordSpawnMarker replaces coordSpawnMarkerFn for tests. Map
// projectName → agentID; missing key returns "". The dashboard's
// task_id fallback requires the marker to match the candidate
// agent's ID before promoting.
//
// Also installs a fresh-mtime stub so the boot-window freshness gate
// (codex iter-4 P1) doesn't reject the test setup. Tests that need
// to exercise the stale-marker path should use stubCoordSpawnMarkerStale.
type stubCoordSpawnMarker struct {
	markers map[string]string // project → agent ID
}

func (s *stubCoordSpawnMarker) install(t *testing.T) {
	t.Helper()
	prevContent := coordSpawnMarkerFn
	coordSpawnMarkerFn = func(projectName string) string {
		return s.markers[projectName]
	}
	t.Cleanup(func() { coordSpawnMarkerFn = prevContent })

	// Default to "fresh" mtime (now) for all known projects so the
	// freshness gate doesn't reject test fixtures by default.
	prevMtime := coordSpawnMarkerMtimeFn
	coordSpawnMarkerMtimeFn = func(projectName string) (time.Time, bool) {
		if _, ok := s.markers[projectName]; !ok {
			return time.Time{}, false
		}
		return time.Now(), true
	}
	t.Cleanup(func() { coordSpawnMarkerMtimeFn = prevMtime })
}

// stubCoordSpawnMarkerStale installs a marker stub that returns a
// stale mtime (older than coordBootWindow). Used to exercise codex
// iter-4 P1's freshness gate.
type stubCoordSpawnMarkerStale struct {
	markers map[string]string // project → agent ID
}

func (s *stubCoordSpawnMarkerStale) install(t *testing.T) {
	t.Helper()
	prevContent := coordSpawnMarkerFn
	coordSpawnMarkerFn = func(projectName string) string {
		return s.markers[projectName]
	}
	t.Cleanup(func() { coordSpawnMarkerFn = prevContent })

	prevMtime := coordSpawnMarkerMtimeFn
	coordSpawnMarkerMtimeFn = func(projectName string) (time.Time, bool) {
		if _, ok := s.markers[projectName]; !ok {
			return time.Time{}, false
		}
		// 2× the boot window in the past — well outside.
		return time.Now().Add(-2 * coordBootWindow), true
	}
	t.Cleanup(func() { coordSpawnMarkerMtimeFn = prevMtime })
}

// stubWriteCoordSpawnMarker replaces writeCoordSpawnMarkerFn for
// tests so we don't write to FLEET_HOME during unit tests. Captures
// (project, id) tuples per call.
type stubWriteCoordSpawnMarker struct {
	calls map[string]string // project → agent ID
	err   error             // returned from each write
}

func (s *stubWriteCoordSpawnMarker) install(t *testing.T) {
	t.Helper()
	prev := writeCoordSpawnMarkerFn
	writeCoordSpawnMarkerFn = func(projectName, agentID string) error {
		if s.calls == nil {
			s.calls = map[string]string{}
		}
		s.calls[projectName] = agentID
		return s.err
	}
	t.Cleanup(func() { writeCoordSpawnMarkerFn = prev })
}

// stubSessionProbe replaces sessionProbeFn (used by loadAgentsCmd's
// status cache). Distinguishes "definitively dead" (dead=true, no
// err) from "probe failed" (errSessions=true, transport-style
// error) so tests can exercise the don't-poison-cache-on-error
// behavior — codex review iter-5 P2.
type stubSessionProbe struct {
	dead        map[string]bool
	errSessions map[string]bool
}

func (s *stubSessionProbe) install(t *testing.T) {
	t.Helper()
	prev := sessionProbeFn
	sessionProbeFn = func(session string) (bool, error) {
		if s.errSessions[session] {
			return false, errors.New("stub probe transport error")
		}
		return !s.dead[session], nil
	}
	t.Cleanup(func() { sessionProbeFn = prev })
}

func TestKey_AttachSetsPendingAndQuits(t *testing.T) {
	(&stubSessionAlive{}).install(t) // every session alive by default
	m := makeModelWithAgents(sampleAgent("agent01"))
	updated, cmd := m.Update(keyMsg("a"))

	mm := updated.(Model)
	if mm.PendingAttach() != "fleet-agent01" {
		t.Errorf("pendingAttach not set: %q", mm.PendingAttach())
	}
	// tea.Quit returns a tea.QuitMsg when invoked.
	if cmd == nil {
		t.Fatal("expected tea.Quit cmd")
	}
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Errorf("expected tea.QuitMsg, got %T", msg)
	}
}

// TestKey_AttachOnDeadSessionShowsFlash regresses the user-reported
// "no sessions" UX: when the tmux session has died (claude exited
// inside it), [a] used to exec tmux which failed with a cryptic
// shell-side error. Now the TUI surfaces a clear flash and stays put.
func TestKey_AttachOnDeadSessionShowsFlash(t *testing.T) {
	stub := &stubSessionAlive{dead: map[string]bool{"fleet-agent01": true}}
	stub.install(t)

	m := makeModelWithAgents(sampleAgent("agent01"))
	updated, cmd := m.Update(keyMsg("a"))
	mm := updated.(Model)
	if mm.PendingAttach() != "" {
		t.Error("pendingAttach must NOT be set for a dead session")
	}
	if cmd != nil {
		t.Error("dead-session [a] should not produce a tea.Cmd")
	}
	if mm.flash == nil || !mm.flash.isErr {
		t.Fatalf("expected error flash, got %+v", mm.flash)
	}
	if !strings.Contains(mm.flash.text, "agent01") ||
		!strings.Contains(mm.flash.text, "session is dead") {
		t.Errorf("flash should name the dead agent and explain, got: %q", mm.flash.text)
	}
}

func TestKey_AttachWithEmptyListIsNoop(t *testing.T) {
	m := makeModelWithAgents()
	updated, cmd := m.Update(keyMsg("a"))
	if updated.(Model).PendingAttach() != "" {
		t.Error("pendingAttach should not be set with no agents")
	}
	if cmd != nil {
		t.Error("expected nil cmd")
	}
}

// -- [d] / [n] dispatch picker → prompt --------------------------------

// isolatePicker keeps tests reproducible across machines: only the cwd
// row appears in the picker (no surprise repos from $HOME/projects/).
// The path it points at can't exist, so projectDirs() returns an empty
// scan but discoverRepos still adds Getwd().
func isolatePicker(t *testing.T) {
	t.Helper()
	t.Setenv("FLEET_PROJECT_DIRS", t.TempDir())
}

func TestKey_DispatchOpensPicker(t *testing.T) {
	isolatePicker(t)
	m := makeModelWithAgents()
	updated, cmd := m.Update(keyMsg("d"))
	mm := updated.(Model)
	if mm.mode != modePickRepo {
		t.Errorf("expected modePickRepo, got %v", mm.mode)
	}
	if cmd != nil {
		t.Error("opening picker should not return a cmd")
	}
	if len(mm.repoCandidates) == 0 {
		t.Error("picker should at least contain cwd as a candidate")
	}
}

// TestKeyN_OpensTaskAddPrompt pins issue #53 part B: [n] opens the
// task-add prompt (not the repo picker — that was the v0.1 alias).
func TestKeyN_OpensTaskAddPrompt(t *testing.T) {
	m := makeModelWithAgents()
	updated, _ := m.Update(keyMsg("n"))
	mm := updated.(Model)
	if mm.mode != modePromptTaskAdd {
		t.Errorf("[n] should enter modePromptTaskAdd, got %v", mm.mode)
	}
	if !strings.Contains(mm.View(), "task spec:") {
		t.Errorf("[n] view should show task-add prompt, got:\n%s", mm.View())
	}
}

func TestKey_PickerEnterAdvancesToPrompt(t *testing.T) {
	isolatePicker(t)
	m := makeModelWithAgents()
	mm, _ := m.Update(keyMsg("d"))
	mm, _ = mm.Update(keyMsg("enter"))
	mmm := mm.(Model)
	if mmm.mode != modePromptDispatch {
		t.Errorf("expected modePromptDispatch after picker enter, got %v", mmm.mode)
	}
	if mmm.pickedRepo.Path == "" {
		t.Error("pickedRepo should be set after enter")
	}
}

func TestKey_PromptCollectsRunesAndSubmits(t *testing.T) {
	isolatePicker(t)
	stub := &stubFleetCmd{}
	stub.install(t)

	m := makeModelWithAgents()
	// Open picker → enter to pick cwd
	mm, _ := m.Update(keyMsg("d"))
	mm, _ = mm.Update(keyMsg("enter"))
	pickedPath := mm.(Model).pickedRepo.Path
	if pickedPath == "" {
		t.Fatal("picker did not record a path")
	}
	// Type "fix-bug" into the dispatch prompt
	for _, r := range "fix-bug" {
		mm, _ = mm.Update(keyMsg(string(r)))
	}
	if mm.(Model).promptBuf != "fix-bug" {
		t.Errorf("promptBuf=%q want %q", mm.(Model).promptBuf, "fix-bug")
	}
	// Enter to submit
	mm, cmd := mm.Update(keyMsg("enter"))
	mmm := mm.(Model)
	if mmm.mode != modeNav {
		t.Errorf("mode not reset to nav: %v", mmm.mode)
	}
	if mmm.promptBuf != "" {
		t.Errorf("promptBuf not cleared: %q", mmm.promptBuf)
	}
	if cmd == nil {
		t.Fatal("expected tea.Cmd from prompt submit")
	}
	_ = cmd()
	if len(stub.calls) != 1 {
		t.Fatalf("expected one fleet call, got %v", stub.calls)
	}
	args := stub.calls[0]
	if args[0] != "dispatch" || args[1] != "fix-bug" {
		t.Errorf("expected ['dispatch', 'fix-bug', ...], got %v", args)
	}
	// --cwd and --project must accompany a picked repo so the spawn
	// lands deterministically in the operator's chosen directory.
	if !containsPair(args, "--cwd", pickedPath) {
		t.Errorf("expected --cwd %q in args, got %v", pickedPath, args)
	}
	// --project is parent-basename via ProjectTag — see codex P2.
	wantTag := ProjectTag(pickedPath)
	if !containsPair(args, "--project", wantTag) {
		t.Errorf("expected --project %q in args, got %v", wantTag, args)
	}
}

func TestKey_PromptEscCancels(t *testing.T) {
	isolatePicker(t)
	stub := &stubFleetCmd{}
	stub.install(t)

	m := makeModelWithAgents()
	mm, _ := m.Update(keyMsg("d"))
	mm, _ = mm.Update(keyMsg("enter")) // pick cwd → prompt mode
	mm, _ = mm.Update(keyMsg("x"))
	mm, cmd := mm.Update(keyMsg("esc"))
	mmm := mm.(Model)
	if mmm.mode != modeNav {
		t.Errorf("expected modeNav after esc, got %v", mmm.mode)
	}
	if cmd != nil {
		t.Error("esc should not start a fleet command")
	}
	if len(stub.calls) != 0 {
		t.Errorf("no fleet calls expected, got %v", stub.calls)
	}
}

func TestKey_PromptBackspaceDeletesRune(t *testing.T) {
	isolatePicker(t)
	m := makeModelWithAgents()
	mm, _ := m.Update(keyMsg("d"))
	mm, _ = mm.Update(keyMsg("enter")) // pick cwd
	for _, r := range "abc" {
		mm, _ = mm.Update(keyMsg(string(r)))
	}
	mm, _ = mm.Update(keyMsg("backspace"))
	if mm.(Model).promptBuf != "ab" {
		t.Errorf("backspace failed: %q", mm.(Model).promptBuf)
	}
}

func TestKey_PromptEmptySubmitDoesNotShellOut(t *testing.T) {
	isolatePicker(t)
	stub := &stubFleetCmd{}
	stub.install(t)

	m := makeModelWithAgents()
	mm, _ := m.Update(keyMsg("d"))
	mm, _ = mm.Update(keyMsg("enter")) // pick cwd
	mm, cmd := mm.Update(keyMsg("enter"))
	if cmd != nil {
		t.Error("empty submit should be a noop")
	}
	if mm.(Model).mode != modeNav {
		t.Error("empty submit should still close the prompt")
	}
	if len(stub.calls) != 0 {
		t.Errorf("no calls expected, got %v", stub.calls)
	}
}

// -- picker-specific behavior -----------------------------------------

func TestKey_PickerEscCancels(t *testing.T) {
	isolatePicker(t)
	m := makeModelWithAgents()
	mm, _ := m.Update(keyMsg("d"))
	mm, cmd := mm.Update(keyMsg("esc"))
	mmm := mm.(Model)
	if mmm.mode != modeNav {
		t.Errorf("esc should return to nav, got %v", mmm.mode)
	}
	if cmd != nil {
		t.Error("esc should not produce a cmd")
	}
	if mmm.repoCandidates != nil {
		t.Error("repoCandidates should be cleared on esc")
	}
}

func TestKey_PickerFilterTyping(t *testing.T) {
	isolatePicker(t)
	m := makeModelWithAgents()
	mm, _ := m.Update(keyMsg("d"))
	for _, r := range "xyz" {
		mm, _ = mm.Update(keyMsg(string(r)))
	}
	if mm.(Model).pickerFilter != "xyz" {
		t.Errorf("filter=%q want xyz", mm.(Model).pickerFilter)
	}
	mm, _ = mm.Update(keyMsg("backspace"))
	if mm.(Model).pickerFilter != "xy" {
		t.Errorf("backspace filter failed: %q", mm.(Model).pickerFilter)
	}
}

func TestKey_PickerArrowsNavigateFiltered(t *testing.T) {
	isolatePicker(t)
	m := makeModelWithAgents()
	// Inject a multi-row candidate list directly to exercise nav.
	m.mode = modePickRepo
	m.repoCandidates = []repoCandidate{
		{Path: "/a", Display: "alpha"},
		{Path: "/b", Display: "beta"},
		{Path: "/c", Display: "charlie"},
	}
	mm, _ := m.Update(keyMsg("down"))
	if mm.(Model).pickerCursor != 1 {
		t.Errorf("down should move cursor to 1, got %d", mm.(Model).pickerCursor)
	}
	mm, _ = mm.Update(keyMsg("down"))
	mm, _ = mm.Update(keyMsg("down")) // bound at len-1
	if mm.(Model).pickerCursor != 2 {
		t.Errorf("cursor should clamp at 2, got %d", mm.(Model).pickerCursor)
	}
	mm, _ = mm.Update(keyMsg("up"))
	if mm.(Model).pickerCursor != 1 {
		t.Errorf("up should move cursor to 1, got %d", mm.(Model).pickerCursor)
	}
}

func TestKey_PickerEmptyEnterIsNoop(t *testing.T) {
	isolatePicker(t)
	m := makeModelWithAgents()
	m.mode = modePickRepo
	m.repoCandidates = nil // simulate "no repos" case
	mm, cmd := m.Update(keyMsg("enter"))
	if cmd != nil {
		t.Error("empty picker enter should not advance")
	}
	if mm.(Model).mode != modePickRepo {
		t.Error("empty picker should stay in picker mode")
	}
}

// containsPair reports whether args contains `flag <value>` adjacent.
func containsPair(args []string, flag, value string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

// -- handoffDoneMsg / dispatchDoneMsg → flash --------------------------

func TestUpdate_HandoffDoneSetsFlashAndRefreshes(t *testing.T) {
	m := makeModelWithAgents()
	updated, cmd := m.Update(handoffDoneMsg{out: "agent foo handed off → bar"})
	mm := updated.(Model)
	if mm.flash == nil || !strings.Contains(mm.flash.text, "handed off") {
		t.Errorf("flash not set: %+v", mm.flash)
	}
	if mm.flash.isErr {
		t.Error("success flash should not be marked error")
	}
	if cmd == nil {
		t.Error("expected refresh cmd after handoffDoneMsg")
	}
}

func TestUpdate_HandoffDoneFailureMarksFlashError(t *testing.T) {
	m := makeModelWithAgents()
	updated, _ := m.Update(handoffDoneMsg{
		out: "boom",
		err: errors.New("exit 1"),
	})
	mm := updated.(Model)
	if mm.flash == nil || !mm.flash.isErr {
		t.Errorf("expected error flash, got %+v", mm.flash)
	}
	if !strings.Contains(mm.flash.text, "handoff failed") {
		t.Errorf("error flash missing prefix: %q", mm.flash.text)
	}
}

// -- queueEventMsg → drain --------------------------------------------

func TestUpdate_QueueEventMsgShellsOutToDrain(t *testing.T) {
	stub := &stubFleetCmd{}
	stub.install(t)

	m := makeModelWithAgents()
	_, cmd := m.Update(queueEventMsg{})
	if cmd == nil {
		t.Fatal("expected a tea.Cmd from queueEventMsg")
	}
	_ = cmd()
	if len(stub.calls) != 1 || stub.calls[0][0] != "drain" {
		t.Errorf("expected ['drain'], got %v", stub.calls)
	}
}

func TestUpdate_DrainDoneSuccessIsSilent(t *testing.T) {
	m := makeModelWithAgents()
	updated, cmd := m.Update(drainDoneMsg{out: "drained agent01 -> agent02"})
	mm := updated.(Model)
	// Successful drain must NOT set a flash — the queue fsnotify will
	// fire on every spawn-fresh write, and a banner per drain would
	// spam the operator.
	if mm.flash != nil {
		t.Errorf("expected no flash on successful drain, got: %+v", mm.flash)
	}
	if cmd == nil {
		t.Error("expected agent-list refresh cmd")
	}
}

func TestUpdate_DrainDoneFailureSetsErrorFlash(t *testing.T) {
	m := makeModelWithAgents()
	updated, _ := m.Update(drainDoneMsg{
		out: "lock failed",
		err: errors.New("exit 1"),
	})
	mm := updated.(Model)
	if mm.flash == nil || !mm.flash.isErr {
		t.Errorf("expected error flash, got: %+v", mm.flash)
	}
	if !strings.Contains(mm.flash.text, "drain failed") {
		t.Errorf("error flash missing prefix: %q", mm.flash.text)
	}
}

// -- nav still works alongside actions --------------------------------

// TestKeyJK_MovesCursor pins issue #53 spec: [j/k] moves the
// dashCursor across all rows. makeModelWithAgents drops the cursor
// onto the first rowAgent (issue #55 union puts a synthetic project
// row above the agents); j advances by one row, k pulls back.
func TestKeyJK_MovesCursor(t *testing.T) {
	m := makeModelWithAgents(sampleAgent("a"), sampleAgent("b"), sampleAgent("c"))
	start := m.dashCursor
	mm, _ := m.Update(keyMsg("j"))
	if mm.(Model).dashCursor != start+1 {
		t.Errorf("after j: dashCursor=%d want %d", mm.(Model).dashCursor, start+1)
	}
	mm, _ = mm.Update(keyMsg("j"))
	if mm.(Model).dashCursor != start+2 {
		t.Errorf("after jj: dashCursor=%d want %d", mm.(Model).dashCursor, start+2)
	}
	mm, _ = mm.Update(keyMsg("k"))
	if mm.(Model).dashCursor != start+1 {
		t.Errorf("after jjk: dashCursor=%d want %d", mm.(Model).dashCursor, start+1)
	}
}

// TestKey_RowTypeGatingForActions regresses the codex iter-1 fix in
// 240a3b0, extended for issue #53 row-type discrimination: [x] applies
// to agent (archive) or worker (kill) rows. When the cursor lands on a
// project row, [x] flashes "doesn't apply" and does NOT shell out.
//
// [a] is intentionally NOT covered here as of issue #60: [a] on a
// project row now spawns / attaches a coord (see
// TestKeyA_ProjectRow_SpawnsCoord and friends).
//
// [h] is intentionally NOT covered here as of the 2026-05-27 inversion:
// [h] on a project row now FIRES handoff for the project's coord
// (or flashes "no coord"). See TestActionHandoff_OnProjectRow_*.
// The agent-row inversion is asserted by TestActionHandoff_OnAgentRow_RejectsWithHint.
func TestKey_RowTypeGatingForActions(t *testing.T) {
	pdir := withFleetHome(t)
	seedTasks(t, pdir, "demo", TaskCounts{Todo: 1})

	for _, key := range []string{"x"} {
		stub := &stubFleetCmd{}
		stub.install(t)

		m := New("test")
		m.width = 130
		m.height = 30
		m.dashboard = scanDashboard(time.Now())
		// dashCursor=0 lands on the project row (left column, top).
		m.dashCursor = 0

		updated, cmd := m.Update(keyMsg(key))
		mm := updated.(Model)

		if cmd != nil {
			t.Errorf("[%s] on project row returned a cmd; expected nil (gated)", key)
		}
		if mm.mode != modeNav {
			t.Errorf("[%s] on project row changed mode to %v; expected modeNav", key, mm.mode)
		}
		if mm.flash == nil || !mm.flash.isErr {
			t.Errorf("[%s] on project row did not set an error flash; got %+v", key, mm.flash)
		}
		if len(stub.calls) != 0 {
			t.Errorf("[%s] on project row shelled out (calls=%v); expected zero", key, stub.calls)
		}
	}
}

// -- [x] archive (confirmation flow) ----------------------------------

func TestKey_ArchiveEntersConfirmModeNoFleetCallYet(t *testing.T) {
	stub := &stubFleetCmd{}
	stub.install(t)

	m := makeModelWithAgents(sampleAgent("agent01"))
	updated, cmd := m.Update(keyMsg("x"))

	mm := updated.(Model)
	if mm.mode != modeConfirmArchive {
		t.Errorf("mode = %v, want modeConfirmArchive", mm.mode)
	}
	if mm.archiveCandidate != "agent01" {
		t.Errorf("archiveCandidate = %q, want agent01", mm.archiveCandidate)
	}
	if cmd != nil {
		t.Errorf("[x] alone should not shell out, got cmd != nil")
	}
	if len(stub.calls) != 0 {
		t.Errorf("expected zero fleet calls before confirmation, got %v", stub.calls)
	}
	// View must show the confirmation banner so the operator knows
	// they're one keypress away from a destructive action.
	if !strings.Contains(mm.View(), "Archive agent agent01") {
		t.Errorf("confirmation banner missing from view, got:\n%s", mm.View())
	}
}

func TestKey_ArchiveConfirmYShellsOutWithRm(t *testing.T) {
	stub := &stubFleetCmd{}
	stub.install(t)

	m := makeModelWithAgents(sampleAgent("agent01"))
	mm, _ := m.Update(keyMsg("x"))
	updated, cmd := mm.(Model).Update(keyMsg("y"))

	mmm := updated.(Model)
	if mmm.mode != modeNav {
		t.Errorf("mode after [y] = %v, want modeNav", mmm.mode)
	}
	if mmm.archiveCandidate != "" {
		t.Errorf("archiveCandidate not cleared, got %q", mmm.archiveCandidate)
	}
	if cmd == nil {
		t.Fatal("expected a tea.Cmd from [y] confirm, got nil")
	}
	_ = cmd()
	if len(stub.calls) != 1 || stub.calls[0][0] != "rm" || stub.calls[0][1] != "agent01" {
		t.Errorf("expected ['rm', 'agent01'], got %v", stub.calls)
	}
}

func TestKey_ArchiveConfirmEscCancels(t *testing.T) {
	stub := &stubFleetCmd{}
	stub.install(t)

	m := makeModelWithAgents(sampleAgent("agent01"))
	mm, _ := m.Update(keyMsg("x"))
	updated, cmd := mm.(Model).Update(keyMsg("esc"))

	mmm := updated.(Model)
	if mmm.mode != modeNav {
		t.Errorf("esc should return to modeNav, got %v", mmm.mode)
	}
	if mmm.archiveCandidate != "" {
		t.Errorf("archiveCandidate not cleared on cancel, got %q", mmm.archiveCandidate)
	}
	if cmd != nil {
		t.Errorf("esc cancel should produce no cmd")
	}
	if len(stub.calls) != 0 {
		t.Errorf("expected zero fleet calls on cancel, got %v", stub.calls)
	}
}

func TestKey_ArchiveConfirmNAlsoCancels(t *testing.T) {
	// `n` in modeConfirmArchive cancels the prompt; it must NOT fall
	// through to the [n]/[d] dispatch picker (which would be jarring
	// — the operator just declined a destructive action and would
	// suddenly be in a repo picker).
	stub := &stubFleetCmd{}
	stub.install(t)

	m := makeModelWithAgents(sampleAgent("agent01"))
	mm, _ := m.Update(keyMsg("x"))
	updated, _ := mm.(Model).Update(keyMsg("n"))

	mmm := updated.(Model)
	if mmm.mode != modeNav {
		t.Errorf("n should cancel to modeNav, got %v (picker would be modePickRepo=%v)",
			mmm.mode, modePickRepo)
	}
	if len(stub.calls) != 0 {
		t.Errorf("expected zero fleet calls, got %v", stub.calls)
	}
}

func TestKey_ArchiveOtherKeysSwallowedDuringConfirm(t *testing.T) {
	// `j`/`k` while the destructive prompt is up must not move the
	// cursor — that would silently change WHICH agent the next [y]
	// would archive.
	m := makeModelWithAgents(sampleAgent("agent01"), sampleAgent("agent02"))
	mm, _ := m.Update(keyMsg("x"))
	beforeCursor := mm.(Model).dashCursor

	updated, _ := mm.(Model).Update(keyMsg("j"))
	mmm := updated.(Model)
	if mmm.mode != modeConfirmArchive {
		t.Errorf("j should not exit modeConfirmArchive, got %v", mmm.mode)
	}
	if mmm.dashCursor != beforeCursor {
		t.Errorf("dashCursor moved during confirmation: was %d, now %d", beforeCursor, mmm.dashCursor)
	}
}

func TestUpdate_RmDoneSetsFlashAndRefreshes(t *testing.T) {
	m := makeModelWithAgents()
	updated, cmd := m.Update(rmDoneMsg{out: "agent agent01 archived (no replacement spawned)\n"})

	mm := updated.(Model)
	if mm.flash == nil || mm.flash.isErr {
		t.Errorf("expected non-error flash, got: %+v", mm.flash)
	}
	if !strings.Contains(mm.flash.text, "archived") {
		t.Errorf("flash should surface command output, got: %q", mm.flash.text)
	}
	if cmd == nil {
		t.Errorf("rmDone should trigger a refresh (loadAgentsCmd), got nil")
	}
}

func TestUpdate_RmDoneFailureSetsErrorFlash(t *testing.T) {
	m := makeModelWithAgents()
	updated, _ := m.Update(rmDoneMsg{
		out: "no agent record",
		err: errors.New("exit 1"),
	})
	mm := updated.(Model)
	if mm.flash == nil || !mm.flash.isErr {
		t.Errorf("expected error flash, got: %+v", mm.flash)
	}
	if !strings.Contains(mm.flash.text, "rm failed") {
		t.Errorf("error flash missing prefix: %q", mm.flash.text)
	}
}

// TestKeyX_ArchiveCoord_CleansUpAgentAndSession pins issue #63's "fleet
// manages it, user does management" rule: [x] on a coord-tagged agent
// (task_id=coord-<project>) routes through the same `fleet rm` shell-
// out as any other agent. The coord identity is purely metadata; rm
// kills the tmux session + archives the record. The next dashboard
// refresh sees no alive coord by EITHER signal (lock-body or task_id
// fallback), so the project row's coord-row disappears organically —
// no extra cleanup needed in [x] beyond what `fleet rm` already does.
//
// Test setup: cursor lands on the coord's agent row in the RIGHT
// column. We construct the row list directly (m.dashboard nil + only
// one record + cursor walked manually onto its rowAgent slot) so the
// dashboard's task_id-fallback claim doesn't filter the coord out of
// RIGHT before the test can act on it. In production, an operator
// reaches this case when the coord's session has died (so the alive
// gate fails and the row resurfaces on RIGHT) or when they explicitly
// archive via `fleet rm <id>` from the shell.
func TestKeyX_ArchiveCoord_CleansUpAgentAndSession(t *testing.T) {
	stub := &stubFleetCmd{}
	stub.install(t)
	// Dead session: the coord shows on RIGHT (alive-gate in the
	// dashboard fallback claims fails) so the cursor can land on it.
	// [x] still routes through to `fleet rm`; the rm command does the
	// dead-session-tolerant cleanup itself (rm.go line 119 — skips kill
	// when SessionAlive returns false, archives anyway).
	(&stubSessionAlive{dead: map[string]bool{"fleet-c00bf001": true}}).install(t)

	coord := agent.New("c00bf001")
	coord.Project = "" // no project tag → no synthetic-project filter on RIGHT
	coord.TaskID = "coord-demo"
	coord.TmuxSession = "fleet-c00bf001"

	m := makeModelWithAgents(coord)
	row := m.selectedRow()
	if row == nil || row.kind != rowAgent || row.agent == nil || row.agent.ID != "c00bf001" {
		t.Fatalf("cursor not on coord's agent row; got %+v", row)
	}
	// actionArchive's rowAgent branch is gated on deriveStatus != auto-red/
	// precompact. With no handoff_type the coord status is "ok"/"dead";
	// either passes the gate.
	mm1, _, handled := m.actionArchive()
	if !handled {
		t.Fatal("[x] not handled by actionArchive on coord row")
	}
	if mm1.mode != modeConfirmArchive {
		t.Fatalf("after [x], mode = %v; want modeConfirmArchive", mm1.mode)
	}
	if mm1.archiveCandidate != "c00bf001" {
		t.Fatalf("archiveCandidate = %q; want c00bf001", mm1.archiveCandidate)
	}
	// Confirm via [y] — must shell out to `fleet rm c00bf001`.
	updated, cmd := mm1.Update(keyMsg("y"))
	if cmd == nil {
		t.Fatal("expected rm cmd from [y] confirm")
	}
	mm2 := updated.(Model)
	if mm2.mode != modeNav {
		t.Errorf("mode after [y] = %v; want modeNav", mm2.mode)
	}
	_ = cmd()
	if len(stub.calls) != 1 {
		t.Fatalf("expected 1 fleet call (rm); got %d (%v)", len(stub.calls), stub.calls)
	}
	if stub.calls[0][0] != "rm" || stub.calls[0][1] != "c00bf001" {
		t.Errorf("expected ['rm', 'c00bf001']; got %v", stub.calls[0])
	}
}

// -- [x] dismiss legacy v0.1 project row (issue #96 gap 3) -------------

// projectRowModel builds a Model whose first navigable row is a
// rowProject for the named project, with the supplied agent records
// populating the right column. dashboard is left empty so the project
// only surfaces via the unifiedProjects synthetic path (which is the
// pure-legacy-v0.1 case the dismiss flow targets).
//
// aliveByID is threaded through so deriveStatus produces "dead" when
// the test wants — the dismiss gate's safety check counts live agents.
func projectRowModel(t *testing.T, project string, records []*agent.Record, aliveByID map[string]bool) Model {
	t.Helper()
	m := New("test")
	m.records = records
	m.aliveByID = aliveByID
	// Locate the rowProject for the target project.
	for i, r := range m.dashboardRows() {
		if r.kind == rowProject && r.project != nil && r.project.Name == project {
			m.dashCursor = i
			return m
		}
	}
	t.Fatalf("no rowProject for %q in dashboardRows; got %+v", project, m.dashboardRows())
	return m
}

// TestKeyX_LegacyProjectRow_FullyDeadEntersDismissConfirm pins the
// happy path of issue #96 gap 3: pressing [x] on a pure-legacy v0.1
// project row whose tagged agents are all dead enters the dismiss
// confirmation mode WITHOUT shelling out yet.
func TestKeyX_LegacyProjectRow_FullyDeadEntersDismissConfirm(t *testing.T) {
	stub := &stubFleetCmd{}
	stub.install(t)
	// Project tree absent → pure-legacy classification.
	(&stubProjectTreeExists{missing: map[string]bool{"pedregal": true}}).install(t)

	// Two dead records tagged with project "pedregal".
	a1 := agent.New("dead0001")
	a1.Project = "pedregal"
	a1.TmuxSession = "fleet-dead0001"
	a2 := agent.New("dead0002")
	a2.Project = "pedregal"
	a2.TmuxSession = "fleet-dead0002"
	// aliveByID is keyed by record.ID (not session name) and false →
	// deriveStatus="dead".
	alive := map[string]bool{"dead0001": false, "dead0002": false}

	m := projectRowModel(t, "pedregal", []*agent.Record{a1, a2}, alive)

	mm, cmd, handled := m.actionArchive()
	if !handled {
		t.Fatal("[x] on legacy project row not handled")
	}
	if cmd != nil {
		t.Errorf("[x] press alone must not shell out; got cmd != nil")
	}
	if mm.mode != modeConfirmDismissProject {
		t.Errorf("mode = %v; want modeConfirmDismissProject", mm.mode)
	}
	if mm.dismissProjectCandidate != "pedregal" {
		t.Errorf("dismissProjectCandidate = %q; want pedregal", mm.dismissProjectCandidate)
	}
	if len(mm.dismissProjectDeadAgents) != 2 {
		t.Errorf("dead agent snapshot length = %d; want 2", len(mm.dismissProjectDeadAgents))
	}
	if len(stub.calls) != 0 {
		t.Errorf("expected zero fleet calls before confirm; got %v", stub.calls)
	}
}

// TestKeyX_LegacyProjectRow_DismissConfirmYFansOutRm: the [y] confirm
// dispatches one `fleet rm <id>` per dead agent captured at press time.
func TestKeyX_LegacyProjectRow_DismissConfirmYFansOutRm(t *testing.T) {
	stub := &stubFleetCmd{}
	stub.install(t)
	(&stubProjectTreeExists{missing: map[string]bool{"pedregal": true}}).install(t)

	a1 := agent.New("dead0001")
	a1.Project = "pedregal"
	a1.TmuxSession = "fleet-dead0001"
	a2 := agent.New("dead0002")
	a2.Project = "pedregal"
	a2.TmuxSession = "fleet-dead0002"
	// aliveByID keyed by record.ID; explicit false → deriveStatus="dead".
	alive := map[string]bool{"dead0001": false, "dead0002": false}

	m := projectRowModel(t, "pedregal", []*agent.Record{a1, a2}, alive)
	mm, _, _ := m.actionArchive()
	updated, cmd := mm.Update(keyMsg("y"))
	mmm := updated.(Model)
	if mmm.mode != modeNav {
		t.Errorf("mode after [y] = %v; want modeNav", mmm.mode)
	}
	if mmm.dismissProjectCandidate != "" {
		t.Errorf("dismissProjectCandidate not cleared; got %q", mmm.dismissProjectCandidate)
	}
	if mmm.dismissProjectDeadAgents != nil {
		t.Errorf("dismissProjectDeadAgents not cleared; got %v", mmm.dismissProjectDeadAgents)
	}
	if cmd == nil {
		t.Fatal("expected a tea.Cmd from [y] confirm; got nil")
	}
	// Drain the cmd. tea.Batch returns a batchMsg containing the wrapped
	// commands; we drain each one to record the stub calls.
	msg := cmd()
	switch v := msg.(type) {
	case tea.BatchMsg:
		for _, c := range v {
			_ = c()
		}
	default:
		// tea.Batch with a single cmd may return that cmd's msg directly;
		// fall back to running the full slice we know we built.
		_ = v
	}
	if len(stub.calls) != 2 {
		t.Fatalf("expected 2 rm calls; got %d (%v)", len(stub.calls), stub.calls)
	}
	gotIDs := map[string]bool{}
	for _, c := range stub.calls {
		if c[0] != "rm" {
			t.Errorf("expected rm subcommand; got %v", c)
		}
		gotIDs[c[1]] = true
	}
	if !gotIDs["dead0001"] || !gotIDs["dead0002"] {
		t.Errorf("expected rm of both dead0001+dead0002; got %v", gotIDs)
	}
}

// TestKeyX_LegacyProjectRow_DismissConfirmEscCancels: [esc] cancels
// the dismiss confirm and clears the candidate state. No fleet calls.
func TestKeyX_LegacyProjectRow_DismissConfirmEscCancels(t *testing.T) {
	stub := &stubFleetCmd{}
	stub.install(t)
	(&stubProjectTreeExists{missing: map[string]bool{"pedregal": true}}).install(t)

	a1 := agent.New("dead0001")
	a1.Project = "pedregal"
	a1.TmuxSession = "fleet-dead0001"
	m := projectRowModel(t, "pedregal", []*agent.Record{a1}, map[string]bool{"dead0001": false})

	mm, _, _ := m.actionArchive()
	updated, _ := mm.Update(keyMsg("esc"))
	mmm := updated.(Model)
	if mmm.mode != modeNav {
		t.Errorf("esc should return to modeNav; got %v", mmm.mode)
	}
	if mmm.dismissProjectCandidate != "" || mmm.dismissProjectDeadAgents != nil {
		t.Errorf("candidate state not cleared on cancel; got cand=%q dead=%v",
			mmm.dismissProjectCandidate, mmm.dismissProjectDeadAgents)
	}
	if len(stub.calls) != 0 {
		t.Errorf("expected zero fleet calls on cancel; got %v", stub.calls)
	}
}

// TestKeyX_LegacyProjectRow_LiveAgentRefuses: a project with at least
// one live agent must NOT enter dismiss mode (operator-safety rule).
// The flash points the operator at the per-agent [x] flow.
func TestKeyX_LegacyProjectRow_LiveAgentRefuses(t *testing.T) {
	stub := &stubFleetCmd{}
	stub.install(t)
	(&stubProjectTreeExists{missing: map[string]bool{"pedregal": true}}).install(t)

	deadRec := agent.New("dead0001")
	deadRec.Project = "pedregal"
	deadRec.TmuxSession = "fleet-dead0001"
	liveRec := agent.New("live0001")
	liveRec.Project = "pedregal"
	liveRec.TmuxSession = "fleet-live0001"
	// aliveByID keyed by record.ID; deriveStatus reads alive[r.ID].
	// false → "dead"; true → falls through to "live" (or another non-dead
	// status). The live record only needs to read != "dead".
	alive := map[string]bool{"live0001": true, "dead0001": false}

	m := projectRowModel(t, "pedregal", []*agent.Record{deadRec, liveRec}, alive)
	mm, cmd, handled := m.actionArchive()
	if !handled {
		t.Fatal("[x] not handled")
	}
	if cmd != nil {
		t.Errorf("[x] with live agent must not produce a cmd; got cmd != nil")
	}
	if mm.mode != modeNav {
		t.Errorf("must NOT enter dismiss mode when live agent present; got mode=%v", mm.mode)
	}
	if mm.flash == nil || !mm.flash.isErr {
		t.Fatalf("expected error flash; got %+v", mm.flash)
	}
	if !strings.Contains(mm.flash.text, "live agent") {
		t.Errorf("flash should mention live agent count; got %q", mm.flash.text)
	}
	if len(stub.calls) != 0 {
		t.Errorf("expected zero fleet calls; got %v", stub.calls)
	}
}

// TestKeyX_V02ProjectRow_PreservesExistingFlash: regression-pin the
// pre-#96 [x] semantics on a v0.2-initialized project row. The existing
// flash text + no-shell-out behavior must NOT change. Worker kill is
// still deferred to v0.2.x.
func TestKeyX_V02ProjectRow_PreservesExistingFlash(t *testing.T) {
	stub := &stubFleetCmd{}
	stub.install(t)
	// Project tree EXISTS → v0.2 classification — the dismiss path must
	// NOT trigger.
	(&stubProjectTreeExists{}).install(t)

	// Build the model via a v0.2 dashboard snapshot (not synthetic).
	m := New("test")
	m.dashboard = &Snapshot{Projects: []*ProjectRow{{Name: "fleet", RepoSlug: "fleet"}}}
	for i, r := range m.dashboardRows() {
		if r.kind == rowProject && r.project != nil && r.project.Name == "fleet" {
			m.dashCursor = i
			break
		}
	}

	mm, cmd, handled := m.actionArchive()
	if !handled {
		t.Fatal("[x] not handled")
	}
	if cmd != nil {
		t.Errorf("[x] on v0.2 project must not produce a cmd; got cmd != nil")
	}
	if mm.mode != modeNav {
		t.Errorf("mode should stay modeNav; got %v", mm.mode)
	}
	if mm.dismissProjectCandidate != "" {
		t.Errorf("v0.2 [x] must not seed dismiss candidate; got %q", mm.dismissProjectCandidate)
	}
	if mm.flash == nil || !mm.flash.isErr {
		t.Fatalf("expected error flash; got %+v", mm.flash)
	}
	if !strings.Contains(mm.flash.text, "v0.1 agents") {
		t.Errorf("flash should preserve existing wording; got %q", mm.flash.text)
	}
	if len(stub.calls) != 0 {
		t.Errorf("expected zero fleet calls; got %v", stub.calls)
	}
}

// TestKeyX_LegacyProjectRow_DismissConfirmJKSwallowed: arrow / nav
// keystrokes while the dismiss confirm is up must not move the cursor
// or fall through to nav — same swallow-rest rule as modeConfirmArchive.
func TestKeyX_LegacyProjectRow_DismissConfirmJKSwallowed(t *testing.T) {
	stub := &stubFleetCmd{}
	stub.install(t)
	(&stubProjectTreeExists{missing: map[string]bool{"pedregal": true}}).install(t)

	a := agent.New("dead0001")
	a.Project = "pedregal"
	a.TmuxSession = "fleet-dead0001"
	m := projectRowModel(t, "pedregal", []*agent.Record{a}, map[string]bool{"dead0001": false})

	mm, _, _ := m.actionArchive()
	beforeCursor := mm.dashCursor
	updated, _ := mm.Update(keyMsg("j"))
	mmm := updated.(Model)
	if mmm.mode != modeConfirmDismissProject {
		t.Errorf("j should not exit modeConfirmDismissProject; got %v", mmm.mode)
	}
	if mmm.dashCursor != beforeCursor {
		t.Errorf("dashCursor moved during dismiss confirm: was %d, now %d", beforeCursor, mmm.dashCursor)
	}
	if len(stub.calls) != 0 {
		t.Errorf("expected zero fleet calls during confirm; got %v", stub.calls)
	}
}
