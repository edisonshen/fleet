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
	// v0.2 default startup view is the dashboard; record-scoped action
	// tests target the agents view because that's where the cursor is
	// visible and [h]/[x]/[a] operate. The dashboard-gating behavior
	// (flash hint instead of action) gets its own coverage in
	// TestKey_RecordActionsGatedInDashboardView.
	m.view = viewAgents
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

func TestKey_NisAliasForD(t *testing.T) {
	isolatePicker(t)
	m := makeModelWithAgents()
	updated, _ := m.Update(keyMsg("n"))
	if updated.(Model).mode != modePickRepo {
		t.Error("[n] should also enter the repo picker")
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

// TestKey_RecordActionsGatedInDashboardView regresses the codex P1
// finding: with the v0.2 default dashboard startup view, [h]/[x]/[a]
// operate on m.records[m.cursor] but the dashboard does not render
// that selection. Without a gate, [j]/[k] can move a hidden cursor and
// the next action attaches/handoffs/archives the wrong agent. The
// gate flashes a hint and refuses to act until the operator switches
// to the agents view via [g].
func TestKey_RecordActionsGatedInDashboardView(t *testing.T) {
	stub := &stubFleetCmd{}
	stub.install(t)

	for _, key := range []string{"h", "x", "a"} {
		m := New("test")
		m.records = []*agent.Record{sampleAgent("agent01"), sampleAgent("agent02")}
		// Move hidden cursor — proves the bug surface that the gate
		// closes: [j] in dashboard moves m.cursor with no visible
		// indication, then [h]/[x]/[a] would have fired on the wrong
		// agent.
		m.cursor = 1

		updated, cmd := m.Update(keyMsg(key))
		mm := updated.(Model)

		if cmd != nil {
			t.Errorf("[%s] in dashboard view returned a cmd; expected nil (action gated)", key)
		}
		if mm.mode != modeNav {
			t.Errorf("[%s] in dashboard view changed mode to %v; expected modeNav (action gated)", key, mm.mode)
		}
		if mm.flash == nil || !mm.flash.isErr {
			t.Errorf("[%s] in dashboard view did not set an error flash; got %+v", key, mm.flash)
		}
		if mm.flash != nil && !strings.Contains(mm.flash.text, "[g]") {
			t.Errorf("[%s] flash should hint to press [g], got: %q", key, mm.flash.text)
		}
		if len(stub.calls) != 0 {
			t.Errorf("[%s] in dashboard view shelled out (calls=%v); expected zero", key, stub.calls)
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
	beforeCursor := mm.(Model).cursor

	updated, _ := mm.(Model).Update(keyMsg("j"))
	mmm := updated.(Model)
	if mmm.mode != modeConfirmArchive {
		t.Errorf("j should not exit modeConfirmArchive, got %v", mmm.mode)
	}
	if mmm.cursor != beforeCursor {
		t.Errorf("cursor moved during confirmation: was %d, now %d", beforeCursor, mmm.cursor)
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
