package tui

import (
	"strings"
	"testing"
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
		err:  errSentinel("not a git repo"),
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
	isolatePicker(t)
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

// errSentinel makes a typed error for the test without pulling errors.New
// into the test imports (the rest of this file is import-light by
// design — see keys_test.go for the imports it actually needs).
type errSentinelString string

func (e errSentinelString) Error() string { return string(e) }
func errSentinel(s string) error          { return errSentinelString(s) }
