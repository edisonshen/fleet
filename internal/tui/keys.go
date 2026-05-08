package tui

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/edisonshen/fleet/internal/tmux"
)

// sessionAliveFn is the tmux liveness probe used by [a] attach.
// var so tests can stub without forking tmux. Production calls
// tmux.HasSession — for [a] a probe failure is fine to treat as
// "alive" (the operator will see tmux's own error if the actual
// attach fails). Cleanup paths and the STATUS cache use
// sessionProbeFn instead because they MUST distinguish probe
// failures from definitive dead.
var sessionAliveFn = tmux.HasSession

// sessionProbeFn returns the tristate (alive, dead, error) probe
// used by loadAgentsCmd to populate the STATUS-cache without
// mistaking transport errors for dead sessions. var so tests can
// stub without forking tmux.
var sessionProbeFn = tmux.SessionAlive

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
	modePromptTaskAdd
	modePromptSearch
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
//
// Row-type gating (issue #53 / codex iter-1 fix in 240a3b0):
// the dashboard cursor walks across project, task, worker, and agent
// rows; [a]/[h]/[x] only apply to certain row types. We dispatch on
// row type and surface a flash banner when the operator presses an
// action that doesn't apply to the current row — this makes the
// target visible before any destructive op.
func (m Model) handleActionKey(key string) (Model, tea.Cmd, bool) {
	switch m.mode {
	case modePickRepo:
		return m.handlePickerKey(key)
	case modePromptDispatch:
		return m.handlePromptKey(key)
	case modeConfirmArchive:
		return m.handleConfirmArchiveKey(key)
	case modePromptTaskAdd:
		return m.handleTaskAddKey(key)
	case modePromptSearch:
		return m.handleSearchKey(key)
	}
	// Help / detail overlays close on any key. The dismissal absorbs
	// the key (handled=true) so [j]/[k] don't simultaneously move the
	// hidden cursor — operator must press the nav key again after the
	// overlay is gone, which is the right cognitive model for a modal.
	// Quit keys ([q]/ctrl+c) are NOT absorbed: they fall through so
	// the operator can still exit while a panel is up.
	if m.showHelp || m.detail != nil {
		if key == "q" || key == "ctrl+c" {
			return m, nil, false
		}
		m.showHelp = false
		m.detail = nil
		return m, nil, true
	}
	switch key {
	case "?":
		m.showHelp = true
		return m, nil, true
	case "/":
		m.mode = modePromptSearch
		m.promptBuf = m.searchFilter
		return m, nil, true
	case "esc":
		// In modeNav, Esc clears the active search filter. Without
		// this branch, the footer's advertised "/<query> · esc clears"
		// hint would only work while the search prompt is open — once
		// committed, the operator would have to re-press [/] just to
		// dismiss the filter (codex iter-1 P2).
		if m.searchFilter != "" {
			m.searchFilter = ""
			m.dashCursor = 0
			return m, nil, true
		}
		return m, nil, false
	case "n":
		// Freeze the target project at press time. Subsequent ticks +
		// dashboard refreshes can re-sort rows under the prompt; without
		// this freeze, submit time would resolve the project from the
		// (now-shifted) dashCursor and the new task could land in the
		// wrong tasks.md (codex iter-2 P1).
		m.taskAddProjectFrozen = m.taskAddProject()
		m.mode = modePromptTaskAdd
		m.promptBuf = ""
		return m, nil, true
	case "enter":
		return m.openDetail()
	case "h":
		return m.actionHandoff()
	case "x":
		return m.actionArchive()
	case "a":
		return m.actionAttach()
	case "d":
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

// actionHandoff dispatches [h] to the agent at the dashboard cursor.
// Tasks/projects/workers don't have handoff semantics — flash an
// "doesn't apply" banner so the operator sees why nothing happened.
func (m Model) actionHandoff() (Model, tea.Cmd, bool) {
	row := m.selectedRow()
	if row == nil || row.kind != rowAgent || row.agent == nil {
		m.flash = &flashMsg{
			text:  "[h] handoff applies only to v0.1 agents — move cursor onto an agent row",
			isErr: true,
		}
		return m, nil, true
	}
	cur := row.agent
	// Gate [h] only for COMMITTED handoff states (auto-red /
	// precompact) — those have a queue journal on disk, so a manual
	// handoff would race against the in-flight one. auto-yellow is
	// NOT gated: the journal hasn't been written yet (only after
	// MILESTONE), and [h] is the operator's escape hatch when the
	// auto handoff stalls.
	status := deriveStatus(cur, m.aliveByID)
	if status == "auto-red" || status == "precompact" {
		m.flash = &flashMsg{
			text:  fmt.Sprintf("agent %s already has a handoff journal — `fleet drain` first", cur.ID),
			isErr: true,
		}
		return m, nil, true
	}
	return m, m.startHandoff(cur.ID), true
}

// actionArchive dispatches [x] for the row under the cursor.
//
// Worker row → run `fleet workers kill <slug>` (operator wants to
// terminate a stuck worker). Agent row → enter the archive-confirm
// mode (rm is destructive, requires y to commit). Other row types
// flash "doesn't apply".
func (m Model) actionArchive() (Model, tea.Cmd, bool) {
	row := m.selectedRow()
	if row == nil {
		m.flash = &flashMsg{text: "[x] no row selected", isErr: true}
		return m, nil, true
	}
	switch row.kind {
	case rowAgent:
		if row.agent == nil {
			return m, nil, true
		}
		cur := row.agent
		status := deriveStatus(cur, m.aliveByID)
		if status == "auto-red" || status == "precompact" {
			m.flash = &flashMsg{
				text:  fmt.Sprintf("agent %s has a pending handoff journal — `fleet drain` first", cur.ID),
				isErr: true,
			}
			return m, nil, true
		}
		m.mode = modeConfirmArchive
		m.archiveCandidate = cur.ID
		return m, nil, true
	case rowWorker:
		// Worker termination from the TUI is deferred to a follow-up
		// PR — `fleet workers kill` does not exist (only list/prune/
		// update/worktree-path are wired in cmd/fleet/workers.go), and
		// SIGTERMing a mid-phase worker has subtle implications
		// (half-archived state, orphaned worktrees, queue-journal
		// races) that need their own design pass. Until then, flash
		// the operator-actionable hint pointing at the existing
		// `fleet workers prune` path for terminated workers (codex
		// iter-3 P1 — was wired to a non-existent subcommand).
		m.flash = &flashMsg{
			text:  "[x] worker termination not yet wired in TUI — use `fleet workers prune` for finished workers",
			isErr: true,
		}
		return m, nil, true
	default:
		m.flash = &flashMsg{
			text:  "[x] applies only to v0.1 agents in this version — worker kill ships in v0.2.x",
			isErr: true,
		}
		return m, nil, true
	}
}

// actionAttach dispatches [a] for the row under the cursor.
//
// Agent row → tmux attach to the agent's session.
// Worker row → open the worker peek detail panel (output.log + state.json).
// Other row types flash "doesn't apply".
func (m Model) actionAttach() (Model, tea.Cmd, bool) {
	row := m.selectedRow()
	if row == nil {
		m.flash = &flashMsg{text: "[a] no row selected", isErr: true}
		return m, nil, true
	}
	switch row.kind {
	case rowAgent:
		if row.agent == nil {
			return m, nil, true
		}
		cur := row.agent
		// Pre-flight liveness check (same behavior as v0.1 [a] flow):
		// tmux's `attach -t <session>` on a dead session prints "no
		// sessions" and the operator drops back to their shell with
		// no idea why. Surface the diagnosis in-TUI.
		if !sessionAliveFn(cur.TmuxSession) {
			m.flash = &flashMsg{
				text: fmt.Sprintf(
					"agent %s session is dead — claude likely exited inside it. Press [x] to archive the orphan record.",
					cur.ID),
				isErr: true,
			}
			return m, nil, true
		}
		m.pendingAttach = cur.TmuxSession
		return m, tea.Quit, true
	case rowWorker:
		if row.worker == nil {
			return m, nil, true
		}
		// [a] on a worker = peek (no tmux session to attach to —
		// workers run as `claude --print` subprocesses). Open the
		// detail panel inline; same path as [⏎] open on a worker.
		body, title := readWorkerDetail(row.worker.Project, row.worker.Slug)
		m.detail = &detailView{title: title, body: body}
		return m, nil, true
	default:
		m.flash = &flashMsg{
			text:  "[a] attach applies to agents (tmux) or workers (peek); not to tasks/projects",
			isErr: true,
		}
		return m, nil, true
	}
}

// openDetail handles [⏎] open. Behavior by row kind:
//
//	project → toggle inline task-list expansion under the project
//	          header (issue #59). First [⏎] expands; second [⏎] on the
//	          same project collapses. Cursor stays on the project row
//	          so j/k can immediately walk into the task sub-rows.
//	task    → render the task's spec/acceptance/notes detail panel
//	          (existing behavior; task-detail-from-row is a separate
//	          PR's concern but the wiring is already here and tests
//	          depend on it).
//	worker  → render the worker peek panel (state.json + log tail).
//	agent   → render the agent JSON record.
func (m Model) openDetail() (Model, tea.Cmd, bool) {
	row := m.selectedRow()
	if row == nil {
		return m, nil, true
	}
	switch row.kind {
	case rowProject:
		if row.project == nil {
			return m, nil, true
		}
		if m.expanded == nil {
			m.expanded = map[string]bool{}
		}
		// Toggle. Map zero-read returns false, so deleting on collapse
		// keeps the map small over a long session — irrelevant for
		// correctness, just hygiene.
		if m.expanded[row.project.Name] {
			delete(m.expanded, row.project.Name)
		} else {
			m.expanded[row.project.Name] = true
		}
		return m, nil, true
	case rowTask:
		// Synthetic markers (the "no tasks yet" hint and "+N more"
		// footer under expanded projects, issue #59) carry no slug —
		// readTaskDetail("") would render an unhelpful "task not
		// found" modal. Treat as a navigation no-op so j/k can pass
		// through the marker without trapping the operator in a
		// dead-end overlay.
		if row.task == nil || row.task.Empty || row.task.More > 0 {
			return m, nil, true
		}
		body, title := readTaskDetail(row.parentProject, row.task.Slug)
		m.detail = &detailView{title: title, body: body}
	case rowWorker:
		body, title := readWorkerDetail(row.worker.Project, row.worker.Slug)
		m.detail = &detailView{title: title, body: body}
	case rowAgent:
		body, title := readAgentDetail(row.agent)
		m.detail = &detailView{title: title, body: body}
	}
	return m, nil, true
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
