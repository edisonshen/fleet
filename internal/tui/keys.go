package tui

import (
	"bytes"
	"fmt"
	"os/exec"

	tea "github.com/charmbracelet/bubbletea"
)

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
	modePromptDispatch
)

// flash is the banner surfaced under the table after a keybind action
// (success or error). Cleared on the next refresh.
type flashMsg struct {
	text  string
	isErr bool
}

// dispatchDoneMsg / handoffDoneMsg are emitted after the shelled-out
// command returns. Carries combined stdout+stderr so the banner can
// show the operator what happened.
type handoffDoneMsg struct {
	out string
	err error
}
type dispatchDoneMsg struct {
	out string
	err error
}

// runFleetCmd shells out to `fleet <args...>` and returns a tea.Cmd
// that emits the resulting message. Output is captured combined so
// the banner shows the same text the operator would see at the
// shell. Replaced by tests with a stub that returns canned output.
var runFleetCmd = func(args []string, msgFn func(string, error) tea.Msg) tea.Cmd {
	return func() tea.Msg {
		var buf bytes.Buffer
		cmd := exec.Command("fleet", args...)
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
	if m.mode == modePromptDispatch {
		return m.handlePromptKey(key)
	}
	switch key {
	case "h":
		if cur := m.selected(); cur != nil {
			return m, m.startHandoff(cur.ID), true
		}
		return m, nil, true
	case "a":
		if cur := m.selected(); cur != nil {
			m.pendingAttach = cur.TmuxSession
			return m, tea.Quit, true
		}
		return m, nil, true
	case "d", "n":
		m.mode = modePromptDispatch
		m.promptBuf = ""
		return m, nil, true
	}
	return m, nil, false
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

// startDispatch returns a tea.Cmd that runs `fleet dispatch <task>`
// and emits dispatchDoneMsg on completion.
func (m Model) startDispatch(task string) tea.Cmd {
	return runFleetCmd([]string{"dispatch", task}, func(out string, err error) tea.Msg {
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
