package tui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestKey_PlusOpensAddProjectPicker pins the [+] hotkey: pressing it
// in modeNav enters modeAddProject and populates repoCandidates from
// discoverRepos — the same picker the [d] key uses.
func TestKey_PlusOpensAddProjectPicker(t *testing.T) {
	isolatePicker(t)
	m := makeModelWithAgents()
	updated, cmd := m.Update(keyMsg("+"))
	mm := updated.(Model)
	if mm.mode != modeAddProject {
		t.Errorf("expected modeAddProject, got %v", mm.mode)
	}
	if cmd != nil {
		t.Error("opening add-project picker should not return a cmd")
	}
	if len(mm.repoCandidates) == 0 {
		t.Error("picker should at least contain cwd as a candidate")
	}
}

// TestKey_AddProjectEscCancels returns to modeNav and clears state.
func TestKey_AddProjectEscCancels(t *testing.T) {
	isolatePicker(t)
	m := makeModelWithAgents()
	mm, _ := m.Update(keyMsg("+"))
	mm, _ = mm.Update(keyMsg("esc"))
	mmm := mm.(Model)
	if mmm.mode != modeNav {
		t.Errorf("esc should drop to modeNav, got %v", mmm.mode)
	}
	if mmm.repoCandidates != nil {
		t.Error("repoCandidates must be cleared on esc")
	}
}

// TestKey_AddProjectEnterShellsOutToProjectAdd: selecting a row must
// shell out to `fleet project add <path>` and surface the success
// flash. The picker is dismissed back to modeNav so the operator can
// see the dashboard refresh.
func TestKey_AddProjectEnterShellsOutToProjectAdd(t *testing.T) {
	isolatePicker(t)
	stub := &stubFleetCmd{}
	stub.install(t)

	m := makeModelWithAgents()
	mm, _ := m.Update(keyMsg("+"))
	mm, cmd := mm.Update(keyMsg("enter"))
	mmm := mm.(Model)
	if mmm.mode != modeNav {
		t.Errorf("after enter expected modeNav, got %v", mmm.mode)
	}
	if cmd == nil {
		t.Fatal("expected tea.Cmd from add-project enter")
	}
	_ = cmd()

	if len(stub.calls) == 0 {
		t.Fatalf("expected at least one fleet call, got %v", stub.calls)
	}
	args := stub.calls[0]
	if len(args) < 3 || args[0] != "project" || args[1] != "add" {
		t.Errorf("expected ['project','add', <path>], got %v", args)
	}
}

// TestKey_AddProjectFailureKeepsPickerOpen: when the underlying
// `fleet project add` shell-out errors, the picker must stay open so
// the operator can pick a different row. The error flash carries the
// failure text.
func TestKey_AddProjectFailureKeepsPickerOpen(t *testing.T) {
	isolatePicker(t)
	m := makeModelWithAgents()
	mm, _ := m.Update(keyMsg("+"))
	if mm.(Model).mode != modeAddProject {
		t.Fatalf("expected modeAddProject, got %v", mm.(Model).mode)
	}

	// Synthesize the failure path directly via the addProjectDoneMsg.
	updated, _ := mm.Update(addProjectDoneMsg{
		path: "/some/path",
		out:  "not a git repo: /some/path",
		err:  errors.New("not a git repo"),
	})
	mmm := updated.(Model)
	if mmm.mode != modeAddProject {
		t.Errorf("on add-project failure picker should stay open, got mode=%v", mmm.mode)
	}
	if mmm.flash == nil || !mmm.flash.isErr {
		t.Errorf("expected error flash, got %+v", mmm.flash)
	}
	if !strings.Contains(mmm.flash.text, "not a git repo") {
		t.Errorf("flash should mention the underlying error, got %q", mmm.flash.text)
	}
}

// TestKey_AddProjectSuccessClosesPicker drops the picker and surfaces
// a success flash mentioning the project tag. Dashboard refresh
// command is returned so the new project shows up.
func TestKey_AddProjectSuccessClosesPicker(t *testing.T) {
	withFleetHome(t)
	isolatePicker(t)
	stub := &stubFleetCmd{}
	stub.install(t)

	m := makeModelWithAgents()
	mm, _ := m.Update(keyMsg("+"))
	updated, cmd := mm.Update(addProjectDoneMsg{
		path: "/repos/my-project",
		out:  "added project repos-my-project\n",
	})
	mmm := updated.(Model)
	if mmm.mode != modeNav {
		t.Errorf("on success picker should close to nav, got %v", mmm.mode)
	}
	if mmm.flash == nil || mmm.flash.isErr {
		t.Errorf("expected non-error flash, got %+v", mmm.flash)
	}
	if !strings.Contains(mmm.flash.text, "added project") {
		t.Errorf("flash should announce success, got %q", mmm.flash.text)
	}
	if cmd == nil {
		t.Error("success path must return a refresh cmd")
	}
}

// TestKey_AddProjectSuccessSpawnsCoord regresses Bug 1: after
// `fleet project add` returns successfully, the TUI must kick off a
// coord auto-spawn for the new project — same Cmd path the [a]-on-
// project-row keybind uses. Without this the [+] hotkey leaves the
// project registered but coord-less, forcing the operator to navigate
// to the new row and press [a] manually.
//
// Mock surface mirrors TestKeyA_ProjectRow_NoCoord_SpawnsAndAttaches:
// stubFleetCmd captures `fleet dispatch`-shaped args; we assert
// args[0]=="dispatch", args[1]=="coord-<tag>", and the --project flag
// is the tag computed from the picked path via projects.TagForPath.
func TestKey_AddProjectSuccessSpawnsCoord(t *testing.T) {
	withFleetHome(t)
	isolatePicker(t)
	(&stubSessionAlive{}).install(t)

	stub := &stubFleetCmd{
		stubbed: func(args []string) tea.Msg {
			// Project-add shell-out: no coord-spawn yet, return a benign
			// add-project success.
			if len(args) >= 2 && args[0] == "project" && args[1] == "add" {
				return addProjectDoneMsg{
					path: args[len(args)-1],
					out:  "added project repos-my-project\n",
				}
			}
			// Coord-spawn dispatch: mimic real `fleet dispatch` stdout
			// so the regex parser finds the agent ID.
			out := "agent abcd1234 spawned\n  task: coord-repos-my-project\n  project: repos-my-project\n  tmux: fleet-abcd1234\n"
			return coordSpawnDoneMsgFromArgs(args, out, nil)
		},
	}
	stub.install(t)

	m := makeModelWithAgents()
	// Drive the success branch directly via the addProjectDoneMsg —
	// avoids depending on filterCandidates picking a particular row.
	mm, _ := m.Update(keyMsg("+"))
	updated, cmd := mm.Update(addProjectDoneMsg{
		path: "/repos/my-project",
		out:  "added project repos-my-project\n",
	})
	mmm := updated.(Model)
	if mmm.mode != modeNav {
		t.Fatalf("on success picker should close to nav, got %v", mmm.mode)
	}
	if cmd == nil {
		t.Fatal("success branch must return a tea.Cmd (refresh + coord-spawn batch)")
	}
	// Drain the batch. tea.Batch produces a BatchMsg containing the
	// underlying Cmds; we walk it and trigger them to capture the
	// dispatch call into the stub.
	msg := cmd()
	drainBatch(t, msg)

	if len(stub.calls) == 0 {
		t.Fatalf("expected at least one fleet call (coord-spawn dispatch), got %v", stub.calls)
	}

	// Find the dispatch call. The batch may also include dashboard
	// reload (which does NOT go through runFleetCmd — it calls
	// scanDashboard directly), so the stub captures only the dispatch.
	var dispatchArgs []string
	for _, args := range stub.calls {
		if len(args) > 0 && args[0] == "dispatch" {
			dispatchArgs = args
			break
		}
	}
	if dispatchArgs == nil {
		t.Fatalf("expected a dispatch call in stub.calls, got %v", stub.calls)
	}
	// args[1] is the task_id, which MUST be "coord-<tag>" — same
	// stable-per-project shape the [a] coord-spawn path uses
	// (issue #63). The tag derives from the picked path via
	// projects.TagForPath.
	if len(dispatchArgs) < 2 {
		t.Fatalf("dispatch call missing task_id: %v", dispatchArgs)
	}
	wantTaskID := "coord-repos-my-project"
	if dispatchArgs[1] != wantTaskID {
		t.Errorf("dispatch task_id = %q, want %q", dispatchArgs[1], wantTaskID)
	}
	// --project must equal the tag, --coord-spawn must be present.
	hasProject := false
	hasCoordSpawn := false
	for i, a := range dispatchArgs {
		if a == "--project" && i+1 < len(dispatchArgs) && dispatchArgs[i+1] == "repos-my-project" {
			hasProject = true
		}
		if a == "--coord-spawn" {
			hasCoordSpawn = true
		}
	}
	if !hasProject {
		t.Errorf("dispatch args missing --project repos-my-project: %v", dispatchArgs)
	}
	if !hasCoordSpawn {
		t.Errorf("dispatch args missing --coord-spawn flag: %v", dispatchArgs)
	}
}

// TestKey_AddProjectSpawnFailureFlashesRecoveryHint pins the spec'd
// failure UX: when `fleet project add` succeeds but the coord auto-
// spawn fails downstream, the operator gets a flash that names the
// recovery path — the project IS registered, [a] on the new row will
// retry. Without this hint the operator sees only a generic "project
// <name>: <err>" banner and may not realize the add itself succeeded.
func TestKey_AddProjectSpawnFailureFlashesRecoveryHint(t *testing.T) {
	withFleetHome(t)

	m := makeModelWithAgents()
	// Mark the spawn as [+]-initiated, then feed the err msg directly.
	m.projectAddCoordSpawn = map[string]bool{"repos-my-project": true}
	updated, _ := m.Update(coordSpawnDoneMsg{
		projectName: "repos-my-project",
		err:         errors.New("dispatch: exit 1"),
	})
	mmm := updated.(Model)
	if mmm.flash == nil || !mmm.flash.isErr {
		t.Fatalf("expected error flash, got %+v", mmm.flash)
	}
	if !strings.Contains(mmm.flash.text, "[a]") {
		t.Errorf("flash should mention the [a] recovery path, got %q", mmm.flash.text)
	}
	if !strings.Contains(mmm.flash.text, "repos-my-project") {
		t.Errorf("flash should name the project, got %q", mmm.flash.text)
	}
	// The flag must be cleared after the message lands — subsequent
	// [a] failures on the same project should NOT re-trigger the
	// "added but spawn failed" hint.
	if mmm.projectAddCoordSpawn["repos-my-project"] {
		t.Errorf("projectAddCoordSpawn flag should be cleared after coordSpawnDoneMsg")
	}
}

// drainBatch walks a tea.BatchMsg-shaped value (it's []tea.Cmd under
// the hood) and invokes each Cmd so the stub records the call. Bubble-
// tea exports BatchMsg as []tea.Cmd in v0.x; we type-switch defensively
// in case the shape ever changes.
func drainBatch(t *testing.T, msg tea.Msg) {
	t.Helper()
	if msg == nil {
		return
	}
	switch v := msg.(type) {
	case tea.BatchMsg:
		for _, c := range v {
			if c == nil {
				continue
			}
			_ = c() // trigger
		}
	default:
		// Single non-batch cmd already drained by caller; nothing to do.
	}
}
