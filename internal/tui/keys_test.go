package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/edisonshen/fleet/internal/agent"
)

// keyMsg constructs a tea.KeyMsg matching what bubbletea emits for a
// given printable key — without bubbletea, "h" comes through as
// {Type: KeyRunes, Runes: ['h']}.
func keyMsg(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "backspace":
		return tea.KeyMsg{Type: tea.KeyBackspace}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

func makeModelWithAgents(records ...*agent.Record) Model {
	m := New("test")
	m.records = records
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
	calls    [][]string
	stubbed  func(args []string) tea.Msg
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

func TestKey_HandoffShellsOutWithSelectedID(t *testing.T) {
	stub := &stubFleetCmd{}
	stub.install(t)

	m := makeModelWithAgents(sampleAgent("agent01"), sampleAgent("agent02"))
	m.cursor = 1

	updated, cmd := m.Update(keyMsg("h"))
	if cmd == nil {
		t.Fatal("expected a tea.Cmd from [h], got nil")
	}
	// Drain the cmd so the stub records the call.
	_ = cmd()
	if len(stub.calls) != 1 || len(stub.calls[0]) != 2 ||
		stub.calls[0][0] != "handoff" || stub.calls[0][1] != "agent02" {
		t.Errorf("expected ['handoff', 'agent02'], got %v", stub.calls)
	}
	if _, ok := updated.(Model); !ok {
		t.Errorf("Update returned non-Model: %T", updated)
	}
}

func TestKey_HandoffWithEmptyListIsNoop(t *testing.T) {
	stub := &stubFleetCmd{}
	stub.install(t)

	m := makeModelWithAgents() // no agents
	_, cmd := m.Update(keyMsg("h"))
	if cmd != nil {
		t.Errorf("expected nil cmd on [h] with empty list")
	}
	if len(stub.calls) != 0 {
		t.Errorf("expected no fleet invocations, got %v", stub.calls)
	}
}

// -- [a] attach ---------------------------------------------------------

func TestKey_AttachSetsPendingAndQuits(t *testing.T) {
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

// -- [d] / [n] dispatch prompt -----------------------------------------

func TestKey_DispatchOpensPrompt(t *testing.T) {
	m := makeModelWithAgents()
	updated, cmd := m.Update(keyMsg("d"))
	mm := updated.(Model)
	if mm.mode != modePromptDispatch {
		t.Errorf("expected modePromptDispatch, got %v", mm.mode)
	}
	if cmd != nil {
		t.Error("opening prompt should not return a cmd")
	}
}

func TestKey_NisAliasForD(t *testing.T) {
	m := makeModelWithAgents()
	mm := m
	mm, _ = func() (Model, tea.Cmd) {
		updated, cmd := m.Update(keyMsg("n"))
		return updated.(Model), cmd
	}()
	if mm.mode != modePromptDispatch {
		t.Error("[n] should also enter dispatch prompt")
	}
}

func TestKey_PromptCollectsRunesAndSubmits(t *testing.T) {
	stub := &stubFleetCmd{}
	stub.install(t)

	m := makeModelWithAgents()
	// Open prompt
	mm, _ := m.Update(keyMsg("d"))
	// Type "fix-bug"
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
	if len(stub.calls) != 1 || stub.calls[0][0] != "dispatch" ||
		stub.calls[0][1] != "fix-bug" {
		t.Errorf("expected ['dispatch', 'fix-bug'], got %v", stub.calls)
	}
}

func TestKey_PromptEscCancels(t *testing.T) {
	stub := &stubFleetCmd{}
	stub.install(t)

	m := makeModelWithAgents()
	mm, _ := m.Update(keyMsg("d"))
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
	m := makeModelWithAgents()
	mm, _ := m.Update(keyMsg("d"))
	for _, r := range "abc" {
		mm, _ = mm.Update(keyMsg(string(r)))
	}
	mm, _ = mm.Update(keyMsg("backspace"))
	if mm.(Model).promptBuf != "ab" {
		t.Errorf("backspace failed: %q", mm.(Model).promptBuf)
	}
}

func TestKey_PromptEmptySubmitDoesNotShellOut(t *testing.T) {
	stub := &stubFleetCmd{}
	stub.install(t)

	m := makeModelWithAgents()
	mm, _ := m.Update(keyMsg("d"))
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

func TestKey_NavStillWorksAfterActionWiring(t *testing.T) {
	m := makeModelWithAgents(sampleAgent("a"), sampleAgent("b"), sampleAgent("c"))
	mm, _ := m.Update(keyMsg("j"))
	if mm.(Model).cursor != 1 {
		t.Errorf("cursor=%d want 1", mm.(Model).cursor)
	}
	mm, _ = mm.Update(keyMsg("G"))
	if mm.(Model).cursor != 2 {
		t.Errorf("G failed: cursor=%d", mm.(Model).cursor)
	}
}
