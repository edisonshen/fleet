package tui

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/state"
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

// coordSpawnDoneMsg is emitted after the project-row [a] auto-spawn
// path completes. The flow runs as one tea.Cmd in a goroutine:
//
//  1. Shell out to `fleet dispatch coord-<8hex> --project <name> --cwd
//     <repo> --prompt "Run the /coordinator skill loop for project
//     <name>."` (uses dispatch's --prompt flag added in issue #60).
//  2. Parse the new agent's ID from dispatch stdout ("agent <id>
//     spawned" first line) so we know whose lock body to expect.
//  3. Poll ~/.fleet/projects/<name>/.locks/coordinator.lock body via
//     readCoordHolder up to lockPollAttempts at lockPollInterval each
//     until the body equals the new agent ID (the coord skill writes
//     it after acquiring LOCK_EX in _try_lock).
//  4. Hand back the resulting tmux session name (and the project name
//     for the flash) so Update sets pendingAttach + tea.Quit, the same
//     way the agent-row [a] path does.
//
// Failures at any step propagate as err: the dispatch failure (binary
// missing, project name invalid), the agent-ID parse failure (dispatch
// printed unexpected output), or the lock-poll timeout (dispatch
// succeeded but the coord skill never acquired the lock — typically
// the operator has /coordinator unconfigured or claude crashed before
// the first turn). The agent record stays on disk in all timeout
// cases; the operator can attach manually via [a] on its right-column
// row to investigate.
type coordSpawnDoneMsg struct {
	projectName string
	agentID     string
	session     string
	err         error
}

// lockPollInterval / lockPollAttempts bound the coord-spawn lock-
// acquisition wait. 100ms × 20 = 2s total budget per the issue #60
// spec ("Wait briefly (≤2s)"). The coord skill's first tick runs
// immediately after claude boots + types the /coordinator slash —
// well within 2s on a normal box. Slower environments fall through
// to the timeout branch and surface a banner.
const (
	lockPollInterval = 100 * time.Millisecond
	lockPollAttempts = 20
)

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
// Agent row   → tmux attach to the agent's session.
// Worker row  → open the worker peek detail panel (output.log + state.json).
// Project row → attach to existing coord OR auto-spawn one (issue #60).
// Task row    → flash "doesn't apply" (no tmux + no peek surface).
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
	case rowProject:
		return m.actionAttachProject(row.project)
	default:
		// Task rows (and any future row kinds) — neither tmux nor a
		// peek surface fits. Flash so the operator sees the no-op.
		m.flash = &flashMsg{
			text:  "[a] attach applies to projects (coord), agents (tmux), or workers (peek); not to tasks",
			isErr: true,
		}
		return m, nil, true
	}
}

// actionAttachProject is the project-row branch of [a] (issue #60).
//
// Two paths:
//
//   - Existing coord: project.CoordID is set (dashboard's freshness gate
//     passed: lock body + coord-state.json mtime within
//     coordActiveWindow). Find the matching agent.Record by ID, attach
//     to its tmux session — same machinery as the rowAgent branch.
//
//   - No coord: auto-spawn one. Shell out to `fleet dispatch` with
//     --prompt "Run the /coordinator skill loop for project <name>.",
//     then poll the project's coordinator.lock body up to 2s for a
//     match against the new agent ID. On success, set pendingAttach to
//     the new tmux session and tea.Quit (Run() exec's tmux attach
//     post-program). On failure (init err, dispatch err, lock-poll
//     timeout), surface a flash; the half-spawned agent (if dispatch
//     succeeded but the lock body never published) stays on disk and
//     the operator can attach via [a] on its right-column row.
//
// Single-coord-per-project enforcement is upstream: the coord skill
// itself NB-flocks coordinator.lock and exits if it can't acquire.
// We rely on the dashboard's freshness gate (coord-state.json mtime)
// to distinguish "alive coord" from "dead coord with stale lock body"
// before deciding which path to take.
func (m Model) actionAttachProject(p *ProjectRow) (Model, tea.Cmd, bool) {
	if p == nil {
		return m, nil, true
	}
	// Path 1: fresh coord exists. Look up the record by ID; verify the
	// session is alive before committing to attach. A coord whose tmux
	// session died but whose lock body wasn't yet stale would get
	// dispatched to a dead session otherwise — same UX bug as the
	// agent-row branch's "no sessions" failure mode.
	if p.CoordID != "" {
		if rec := findRecordByID(m.records, p.CoordID); rec != nil && rec.TmuxSession != "" {
			if !sessionAliveFn(rec.TmuxSession) {
				m.flash = &flashMsg{
					text: fmt.Sprintf(
						"coord %s for project %s has a dead tmux session — press [x] on its agent row to archive, then [a] here to respawn",
						rec.ID, p.Name),
					isErr: true,
				}
				return m, nil, true
			}
			m.pendingAttach = rec.TmuxSession
			return m, tea.Quit, true
		}
		// CoordID set but no matching record loaded yet (race: dashboard
		// snapshot picked up the lock body before agentsMsg refreshed).
		// Fall through to the spawn path's flash on second press —
		// avoiding the spawn here would leave [a] silently dead. But
		// rather than spawning a duplicate, surface a hint that the
		// operator should retry after the next refresh tick.
		m.flash = &flashMsg{
			text: fmt.Sprintf(
				"coord %s for project %s pending refresh — try [a] again in a moment",
				p.CoordID, p.Name),
			isErr: true,
		}
		return m, nil, true
	}
	// Path 2: no coord. Spawn one.
	cwd := coordCwdForProject(m.records, p.Name)
	return m, m.startCoordSpawn(p.Name, cwd), true
}

// findRecordByID returns the first agent.Record with id, or nil.
// Linear scan is fine: m.records is bounded by the operator's concurrent
// agent count (single digits in practice).
func findRecordByID(records []*agent.Record, id string) *agent.Record {
	for _, r := range records {
		if r != nil && r.ID == id {
			return r
		}
	}
	return nil
}

// coordCwdForProject best-guess resolves the working directory for a
// freshly-spawned coord agent.
//
// Precedence:
//  1. Any existing agent record tagged with the same Project — reuse
//     its Cwd. Operators dispatch agents into the project's actual
//     repo, so this is the most accurate signal.
//  2. The TUI process's own cwd via os.Getwd() — works when the
//     operator launched `fleet` from inside the project repo.
//  3. Empty string — `fleet dispatch` resolves empty cwd to caller's
//     wd, same as (2). Acceptable fallback.
//
// The coord skill operates on ~/.fleet/projects/<name>/, not on the
// repo, so the cwd choice is mostly cosmetic — it sets where the
// agent's tmux session lands and where /coordinator's first
// directory-relative shell commands resolve. Wrong cwd doesn't break
// correctness; it just nudges the agent toward the wrong starting
// directory.
func coordCwdForProject(records []*agent.Record, projectName string) string {
	for _, r := range records {
		if r != nil && r.Project == projectName && r.Cwd != "" {
			return r.Cwd
		}
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return ""
}

// dispatchAgentIDPattern matches the first line of `fleet dispatch`
// stdout: "agent <8-hex-id> spawned". We extract the ID so the
// follow-up lock-poll knows whose holder to expect. The stdout shape
// is stable across v0.1 / v0.2 — see cmd/fleet/dispatch.go runDispatch.
var dispatchAgentIDPattern = regexp.MustCompile(`(?m)^agent ([0-9a-f]{8}) spawned`)

// startCoordSpawn shells out to `fleet dispatch` with the coord
// auto-prompt + polls for lock acquisition. Runs entirely inside one
// tea.Cmd's goroutine so the bubbletea event loop stays responsive
// during the 2s lock-poll window.
//
// task ID format: "coord-<UTC YYYYMMDD-HHMMSS>" — sortable + unique
// across rapid re-spawns. The meaningful identifier is the
// agent.NewID() the spawn assigns; the task ID is just a label so the
// agent record's TaskID isn't empty.
func (m Model) startCoordSpawn(projectName, cwd string) tea.Cmd {
	taskID := fmt.Sprintf("coord-%s", time.Now().UTC().Format("20060102-150405"))
	prompt := fmt.Sprintf("Run the /coordinator skill loop for project %s.", projectName)
	args := []string{"dispatch", taskID, "--project", projectName, "--prompt", prompt}
	if cwd != "" {
		args = append(args, "--cwd", cwd)
	}
	return runFleetCmd(args, func(out string, err error) tea.Msg {
		if err != nil {
			return coordSpawnDoneMsg{
				projectName: projectName,
				err:         fmt.Errorf("dispatch: %w\n%s", err, out),
			}
		}
		// Parse the new agent's ID from dispatch stdout. Without it we
		// can't tell which holder we're waiting for and would have to
		// flash "spawned but can't track" — a bad UX. Treat parse
		// failure as fatal so the operator notices the dispatch output
		// drift before this becomes a silent "lock never matches" loop.
		match := dispatchAgentIDPattern.FindStringSubmatch(out)
		if len(match) != 2 {
			return coordSpawnDoneMsg{
				projectName: projectName,
				err:         fmt.Errorf("dispatch output missing agent ID line:\n%s", out),
			}
		}
		agentID := match[1]
		// Poll the coord lock for body == agentID. The coord skill writes
		// it inside _try_lock after acquiring LOCK_EX | LOCK_NB; until
		// the first tick fires, the body is empty (or carries a stale
		// previous holder, dropped by the truncate-before-write in
		// loop.py). On match → success; on timeout → return the agent
		// ID anyway so the operator knows the spawn happened (and the
		// agent shows up on the right column).
		session := tmux.SessionName(agentID)
		if pollCoordLock(projectName, agentID, lockPollAttempts, lockPollInterval) {
			return coordSpawnDoneMsg{
				projectName: projectName,
				agentID:     agentID,
				session:     session,
			}
		}
		return coordSpawnDoneMsg{
			projectName: projectName,
			agentID:     agentID,
			err: fmt.Errorf(
				"coord spawn timed out — agent %s spawned but lock body never published; check tmux session %s",
				agentID, session),
		}
	})
}

// pollCoordLock polls coordinator.lock every interval for up to attempts
// iterations, returning true once readCoordHolder returns wantID. var so
// tests can stub the timing-sensitive loop without sleeping for real.
var pollCoordLock = func(projectName, wantID string, attempts int, interval time.Duration) bool {
	root, err := state.Root()
	if err != nil {
		return false
	}
	projectsRoot := filepath.Join(root, "projects")
	for i := 0; i < attempts; i++ {
		if got := readCoordHolder(projectsRoot, projectName); got == wantID {
			return true
		}
		time.Sleep(interval)
	}
	return false
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
