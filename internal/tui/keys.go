package tui

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/edisonshen/fleet/internal/tmux"
)

// sessionAliveFn is the tmux liveness probe used by [a] attach. var
// so tests can stub without forking tmux. Production calls
// tmux.HasSession.
var sessionAliveFn = tmux.HasSession

// Action keybinds added in Week 4b+4c. Layered onto the existing
// navigation set (j/k/g/G/q) without touching them.
//
//	[h] handoff: shell out to `fleet handoff <id>` so the existing
//	    operator path runs unchanged. Stdout/stderr surface in the
//	    flash banner.
//	[a] attach: bubbletea + tmux attach is awkward (tmux replaces the
//	    process). Solution: set pendingAttach, tea.Quit, then in
//	    Run() exec tmux attach after the program returns.
//	[d] dispatch / [n] new-task: open a one-line prompt for a task
//	    ID, then shell out to `fleet dispatch <task>`. n is an alias
//	    for d in v0.1 — both invoke the same flow. Differentiating
//	    them (e.g., n for "new project") is a follow-up.
//
// The shelling-out keeps the TUI process boundary clean. A bug in
// `fleet handoff` cannot crash the TUI; the operator just sees the
// error in the banner.

// inputMode controls whether keystrokes navigate or feed a prompt.
type inputMode int

const (
	modeNav inputMode = iota
	modePickRepo
	modePromptDispatch
	modeConfirmArchive
)

// flash is the banner surfaced under the table after a keybind action
// (success or error). Cleared on the next refresh.
type flashMsg struct {
	text  string
	isErr bool
}

// dispatchDoneMsg / handoffDoneMsg / drainDoneMsg are emitted after
// the shelled-out command returns. Carries combined stdout+stderr so
// the banner can show the operator what happened.
type handoffDoneMsg struct {
	out string
	err error
}
type dispatchDoneMsg struct {
	out string
	err error
}
type drainDoneMsg struct {
	out string
	err error
}
type rmDoneMsg struct {
	out string
	err error
}

// fleetBinary is resolved once at startup via os.Executable() so the
// TUI invokes ITSELF for sub-commands rather than depending on the
// PATH-resolved `fleet`. Critical for dev runs (`go run`, install
// paths not on PATH) where `exec.Command("fleet", ...)` would emit
// "fleet: command not found" on every queueEventMsg, silently
// killing auto-drain. Falls back to "fleet" on PATH only as a last
// resort. Tests stub runFleetCmd directly so this path isn't taken.
var fleetBinary = func() string {
	if exe, err := os.Executable(); err == nil {
		return exe
	}
	return "fleet"
}()

// runFleetCmd shells out to `<fleetBinary> <args...>` and returns a
// tea.Cmd that emits the resulting message. Output is captured
// combined so the banner shows the same text the operator would see
// at the shell. Replaced by tests with a stub that returns canned
// output.
var runFleetCmd = func(args []string, msgFn func(string, error) tea.Msg) tea.Cmd {
	return func() tea.Msg {
		var buf bytes.Buffer
		cmd := exec.Command(fleetBinary, args...)
		cmd.Stdout = &buf
		cmd.Stderr = &buf
		err := cmd.Run()
		return msgFn(buf.String(), err)
	}
}

// handleActionKey is called by Update for keys that drive an action
// (handoff, attach, dispatch). Navigation keys stay in handleKey.
// Returns (model, cmd, handled). When handled=false, caller falls
// back to the navigation handler.
func (m Model) handleActionKey(key string) (Model, tea.Cmd, bool) {
	if m.mode == modePickRepo {
		return m.handlePickerKey(key)
	}
	if m.mode == modePromptDispatch {
		return m.handlePromptKey(key)
	}
	if m.mode == modeConfirmArchive {
		return m.handleConfirmArchiveKey(key)
	}
	switch key {
	case "h":
		if cur := m.selected(); cur != nil {
			return m, m.startHandoff(cur.ID), true
		}
		return m, nil, true
	case "x":
		// [x] archive: enter a y/esc confirmation mode rather than firing
		// immediately. rm is destructive (kills the tmux session + deletes
		// the record, no replacement) and a stray keypress on a busy
		// dashboard would lose work irretrievably.
		if cur := m.selected(); cur != nil {
			m.mode = modeConfirmArchive
			m.archiveCandidate = cur.ID
			return m, nil, true
		}
		return m, nil, true
	case "a":
		if cur := m.selected(); cur != nil {
			// Pre-flight liveness check: tmux's `attach -t <session>`
			// on a dead session prints "no sessions" / "can't find
			// session" and the operator drops back to their shell with
			// no idea why. The most common cause is claude having
			// exited inside the tmux session (Ctrl-D / /exit /
			// SIGCHLD), which terminates the session because claude
			// was the session's only process. Surface the diagnosis
			// in-TUI so the operator knows what happened and can
			// clean up via `[h]` handoff.
			if !sessionAliveFn(cur.TmuxSession) {
				m.flash = &flashMsg{
					text: fmt.Sprintf(
						"agent %s session is dead — claude likely exited inside it. Press [h] to clean up the orphan record.",
						cur.ID),
					isErr: true,
				}
				return m, nil, true
			}
			m.pendingAttach = cur.TmuxSession
			return m, tea.Quit, true
		}
		return m, nil, true
	case "d", "n":
		// [d] enters the repo picker. discoverRepos runs synchronously;
		// the cost is one Getwd + one ReadDir per project root, fine for
		// hundreds of repos. If discovery fails (no cwd, no projects/),
		// the picker still renders — just empty — and esc cancels.
		m.repoCandidates = discoverRepos()
		m.pickerFilter = ""
		m.pickerCursor = 0
		m.mode = modePickRepo
		return m, nil, true
	}
	return m, nil, false
}

// handleConfirmArchiveKey runs while modeConfirmArchive is active. The
// only confirm key is `y`/`Y`; everything else (including `n`/`N` and
// `esc`) cancels. We deliberately swallow other keys instead of falling
// through to nav so an absent-minded `j`/`k` while the prompt is up
// can't move the cursor and lose the operator's place.
func (m Model) handleConfirmArchiveKey(key string) (Model, tea.Cmd, bool) {
	switch key {
	case "y", "Y":
		id := m.archiveCandidate
		m.mode = modeNav
		m.archiveCandidate = ""
		if id == "" {
			return m, nil, true
		}
		return m, m.startRm(id), true
	case "esc", "n", "N":
		m.mode = modeNav
		m.archiveCandidate = ""
		return m, nil, true
	}
	return m, nil, true
}

// handlePickerKey processes keystrokes while the repo picker is active.
// Up/Down (or Ctrl-N/Ctrl-P) navigate; enter picks; esc cancels;
// printable runes — including j/k — feed the substring filter (matching
// fzf semantics). Backspace trims the filter.
func (m Model) handlePickerKey(key string) (Model, tea.Cmd, bool) {
	filtered := filterCandidates(m.repoCandidates, m.pickerFilter)
	switch key {
	case "esc":
		m.mode = modeNav
		m.pickerFilter = ""
		m.repoCandidates = nil
		return m, nil, true
	case "enter":
		if len(filtered) == 0 || m.pickerCursor >= len(filtered) {
			return m, nil, true
		}
		m.pickedRepo = m.repoCandidates[filtered[m.pickerCursor]]
		m.mode = modePromptDispatch
		m.promptBuf = ""
		return m, nil, true
	case "down", "ctrl+n":
		if m.pickerCursor < len(filtered)-1 {
			m.pickerCursor++
		}
		return m, nil, true
	case "up", "ctrl+p":
		if m.pickerCursor > 0 {
			m.pickerCursor--
		}
		return m, nil, true
	case "backspace":
		if len(m.pickerFilter) > 0 {
			m.pickerFilter = m.pickerFilter[:len(m.pickerFilter)-1]
			m.pickerCursor = 0
		}
		return m, nil, true
	}
	if len(key) == 1 && key[0] >= 0x20 && key[0] < 0x7f {
		m.pickerFilter += key
		m.pickerCursor = 0
		return m, nil, true
	}
	// Unknown key in picker mode → swallow rather than fall through to
	// nav (otherwise [j/k] would also move the agent table cursor under
	// the picker).
	return m, nil, true
}

// handlePromptKey processes keystrokes while the dispatch prompt is
// active. Enter submits, Esc cancels, Backspace deletes one rune,
// printable runes append to the buffer.
func (m Model) handlePromptKey(key string) (Model, tea.Cmd, bool) {
	switch key {
	case "esc":
		m.mode = modeNav
		m.promptBuf = ""
		return m, nil, true
	case "enter":
		task := m.promptBuf
		m.mode = modeNav
		m.promptBuf = ""
		if task == "" {
			return m, nil, true
		}
		return m, m.startDispatch(task), true
	case "backspace":
		if len(m.promptBuf) > 0 {
			m.promptBuf = m.promptBuf[:len(m.promptBuf)-1]
		}
		return m, nil, true
	}
	// Treat any single-rune printable key as input. Multi-key sequences
	// (e.g. "ctrl+x") fall through unhandled so the nav layer can act
	// on them (currently only ctrl+c, which still quits via Update).
	if len(key) == 1 && key[0] >= 0x20 && key[0] < 0x7f {
		m.promptBuf += key
		return m, nil, true
	}
	return m, nil, true
}

// startHandoff returns a tea.Cmd that runs `fleet handoff <id>` and
// emits handoffDoneMsg on completion.
func (m Model) startHandoff(id string) tea.Cmd {
	return runFleetCmd([]string{"handoff", id}, func(out string, err error) tea.Msg {
		return handoffDoneMsg{out: out, err: err}
	})
}

// startRm returns a tea.Cmd that runs `fleet rm <id>` and emits
// rmDoneMsg on completion. Only called after the operator confirms in
// modeConfirmArchive.
func (m Model) startRm(id string) tea.Cmd {
	return runFleetCmd([]string{"rm", id}, func(out string, err error) tea.Msg {
		return rmDoneMsg{out: out, err: err}
	})
}

// startDispatch returns a tea.Cmd that runs `fleet dispatch <task>`
// and emits dispatchDoneMsg on completion. When the picker recorded a
// repo, --cwd and --project pin the spawn to that directory; the
// project tag includes the parent directory so two repos that share a
// basename (~/work/fleet vs ~/personal/fleet) tag distinctly and
// don't share fleet-guard's per-project locks. See ProjectTag.
func (m Model) startDispatch(task string) tea.Cmd {
	args := []string{"dispatch", task}
	if m.pickedRepo.Path != "" {
		args = append(args, "--cwd", m.pickedRepo.Path,
			"--project", ProjectTag(m.pickedRepo.Path))
	}
	return runFleetCmd(args, func(out string, err error) tea.Msg {
		return dispatchDoneMsg{out: out, err: err}
	})
}

// selected returns the cursor's record or nil if the list is empty.
func (m Model) selected() *agentRow {
	if m.cursor < 0 || m.cursor >= len(m.records) {
		return nil
	}
	r := m.records[m.cursor]
	return &agentRow{ID: r.ID, TmuxSession: r.TmuxSession}
}

// agentRow is a thin DTO so tests don't depend on agent.Record's full
// schema.
type agentRow struct {
	ID          string
	TmuxSession string
}

// formatHandoffFlash converts a handoffDoneMsg into a banner string.
// Centralized so the success / failure formatting stays consistent
// across the model and tests.
func formatHandoffFlash(out string, err error) flashMsg {
	if err != nil {
		return flashMsg{
			text:  fmt.Sprintf("handoff failed: %v\n%s", err, out),
			isErr: true,
		}
	}
	return flashMsg{text: out}
}

func formatDispatchFlash(out string, err error) flashMsg {
	if err != nil {
		return flashMsg{
			text:  fmt.Sprintf("dispatch failed: %v\n%s", err, out),
			isErr: true,
		}
	}
	return flashMsg{text: out}
}

func formatRmFlash(out string, err error) flashMsg {
	if err != nil {
		return flashMsg{
			text:  fmt.Sprintf("rm failed: %v\n%s", err, out),
			isErr: true,
		}
	}
	return flashMsg{text: out}
}
