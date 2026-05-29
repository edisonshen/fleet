package tui

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/coordlock"
	"github.com/edisonshen/fleet/internal/gc"
	"github.com/edisonshen/fleet/internal/projects"
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
	// modeConfirmDismissProject holds the y/N confirmation prompt for
	// the legacy-v0.1 project-row dismiss path (issue #96 gap 3). Distinct
	// from modeConfirmArchive because the destructive action is plural
	// (one `fleet rm` per dead agent record tagged with the project) and
	// the prompt copy must surface that count so the operator knows how
	// many records will be archived.
	modeConfirmDismissProject
	// modeAddProject is the [+] hotkey's repo picker for "register a
	// cloned repo as a fleet project" (no dispatch, no coord). Reuses
	// discoverRepos for the candidate list — same picker UX as [d] —
	// but on enter shells out to `fleet project add <path>` instead of
	// advancing to the dispatch-prompt mode. Refer to the project-add
	// CLI for the disk-mutation contract.
	modeAddProject
	// modeConfirmReset holds the y/N confirmation for the [r] reset path
	// (dead-end-recovery-r-8559). Destructive: confirm reaps ALL of the
	// project's coord sessions/records (`fleet rm <id>` per coord
	// record), clears the stale coordinator.lock (`fleet gc
	// --kinds=coord-locks --apply --project <name>`), then spawns ONE
	// fresh coord. Distinct from modeConfirmArchive (single-agent) and
	// modeConfirmDismissProject (legacy-v0.1 dead-record sweep, no
	// respawn) because reset both reaps AND respawns — the recovery
	// from a tangled/dead-locked project row.
	modeConfirmReset
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

// resetDoneMsg is emitted after the [r] reset reap phase completes
// (dead-end-recovery-r-8559). The reap runs as one tea.Cmd: fan
// `fleet rm <id>` per coord record, then `fleet gc --kinds=coord-locks
// --apply --project <name>` to clear the stale lock. projectName +
// reaped (count of coord records archived) drive the success flash and
// the follow-on fresh-coord spawn. err non-nil means the reap shell-out
// failed; the handler surfaces it and does NOT respawn (a half-reaped
// project shouldn't get a new coord racing the leftovers).
type resetDoneMsg struct {
	projectName string
	reaped      int
	out         string
	err         error
}

// addProjectDoneMsg is emitted after the [+] picker's shell-out to
// `fleet project add <path>` returns. On err == nil the picker closes
// and the dashboard refreshes so the new project appears in alpha
// order; on err != nil the picker stays open with an error flash so
// the operator can pick a different row.
//
// path carries the absolute repo path the operator selected — used in
// the success flash and (on failure) lets the operator see which row
// errored.
type addProjectDoneMsg struct {
	path string
	out  string
	err  error
}

// coordSpawnDoneMsg is emitted after the project-row [a] auto-spawn
// path completes. The flow runs as one tea.Cmd in a goroutine:
//
//  1. Pre-flight: state.EnsureProjectInitialized creates
//     ~/.fleet/projects/<name>/.locks/ so the spawned coord skill's
//     first tick can publish coord-state.json + acquire the flock
//     without racing on a missing parent directory.
//  2. Shell out to `fleet dispatch coord-<name> --project <name>
//     --cwd <repo> --prompt <coordSpawnPrompt(name)>`. The prompt
//     body (issue #80) hard-constrains the coord agent to discuss-
//     and-dispatch — never inline implementation. The task_id is
//     STABLE per project (issue #63): a duplicate [a] press during
//     the skill-boot window finds the in-flight record via
//     findExistingCoordForProject and attaches instead of respawning.
//  3. Parse the new agent's ID from dispatch stdout ("agent <id>
//     spawned" first line). Compute the tmux session name from the
//     ID (tmux.SessionName).
//  4. Hand back projectName + agentID + session. Update sets
//     pendingAttach + tea.Quit so Run() exec's tmux attach to the
//     fresh coord — operator watches Claude boot + invoke
//     /coordinator live.
//
// Crucially we do NOT poll the coordinator.lock body before attaching
// (issue #63 fix): the coord skill takes 10-30s to boot + type the
// /coordinator slash + acquire LOCK_EX. The 2s lock-poll under PR #62
// fired the spawn-timeout banner under normal operation. Now the lock
// body publishes asynchronously; the dashboard's task_id-fallback
// signal renders the agent under its project on LEFT immediately, and
// the lock-body branch upgrades the freshness gate once the skill ticks.
//
// Failures propagate as err: init failure (mkdir denied), dispatch
// failure (binary missing, invalid project name), or agent-ID parse
// failure (dispatch printed unexpected output). The agent record stays
// on disk on parse failure; operator can attach via [a] on its
// right-column row to investigate.
type coordSpawnDoneMsg struct {
	projectName string
	agentID     string
	session     string
	err         error
	// promptDelivered is true when dispatch's stdout did NOT contain
	// the dispatchPromptFailedMarker — i.e. SendInitialPrompt
	// succeeded and /coordinator was actually typed into the pane.
	// False when dispatch returned 0 but warned about a prompt
	// delivery failure (the agent is alive but the coord skill never
	// started; the TUI must not promote it to the project's coord via
	// the marker file). Codex iter-5 P2.
	promptDelivered bool
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
	case modeAddProject:
		return m.handleAddProjectKey(key)
	case modePromptDispatch:
		return m.handlePromptKey(key)
	case modeConfirmArchive:
		return m.handleConfirmArchiveKey(key)
	case modeConfirmDismissProject:
		return m.handleConfirmDismissProjectKey(key)
	case modeConfirmReset:
		return m.handleConfirmResetKey(key)
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
		// Issue #75: [a] inside a task detail panel re-routes to the
		// task's worker peek. Without this interceptor the panel would
		// dismiss and the operator would have to find the task row
		// again to press [a] — defeats "drill in → attach to respond"
		// flow. Worker / agent / help panels still dismiss on [a]
		// because their default behavior IS [a]-as-dismiss.
		//
		// Codex iter-2 P2: pre-dispatch tasks (todo/ready) get the
		// same dispatch hint as the row-side [a] handler — without
		// this gate, opening a todo task's detail and pressing [a]
		// would flash the blocked-task recovery command, which is
		// the wrong next step for a task that just hasn't been
		// dispatched yet.
		if key == "a" && m.detail != nil && m.detail.taskSlug != "" {
			// Codex iter-3 P2 + iter-5 P2: route through the unified
			// attach-or-hint dispatcher. It peeks any existing worker
			// (active or archived) regardless of status so reset-to-
			// todo recovery cases retain log access; only when no
			// worker exists does it surface the status-specific hint.
			return m.attachToTaskOrHint(m.detail.taskProject, m.detail.taskSlug, m.detail.taskStatus)
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
	case "r":
		return m.actionReset()
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
	case "+":
		// [+] enters the add-project picker — same candidate list as
		// [d] but on enter shells out to `fleet project add <path>`
		// instead of advancing to the dispatch prompt. The new project
		// shows up on the dashboard via loadDashboardCmd refresh
		// returned by addProjectDoneMsg.
		m.repoCandidates = discoverRepos()
		m.pickerFilter = ""
		m.pickerCursor = 0
		m.mode = modeAddProject
		return m, nil, true
	case "c":
		return m.actionToggleHide()
	}
	return m, nil, false
}

// actionToggleHide implements [c] (issue #98). Three branches by
// cursor context:
//
//  1. Project row → toggle that project's membership in the hidden
//     list and surface a flash. Hide is HARD: fresh activity does not
//     auto-unhide; only [c] on the row again (with show-hidden mode
//     on, so the row is reachable) un-hides.
//
//  2. Separator row OR no-row → toggle show-hidden mode. The hidden
//     group renders dim under "─── N hidden ───" when on; vanishes
//     when off. Off-separator toggle is the operator's main lever to
//     reach hidden rows again without having to remember which
//     project they hid.
//
//  3. Other row kinds (task / worker / agent) → flash a "doesn't
//     apply" hint. [c] is project-scoped; we don't repurpose it for
//     task/worker hiding (which would need separate persistence).
func (m Model) actionToggleHide() (Model, tea.Cmd, bool) {
	row := m.selectedRow()
	if row == nil {
		// No row selected (empty dashboard) → toggle show-hidden so
		// the operator can still discover hidden projects on a fresh
		// install.
		m.showHidden = !m.showHidden
		m = clampDashCursor(m)
		return m, nil, true
	}
	switch row.kind {
	case rowProject:
		if row.project == nil {
			return m, nil, true
		}
		name := row.project.Name
		nowHidden, err := toggleHiddenProjectFn(name)
		if err != nil {
			m.flash = &flashMsg{
				text:  fmt.Sprintf("hide toggle for %s failed: %v", name, err),
				isErr: true,
			}
			return m, nil, true
		}
		if nowHidden {
			m.flash = &flashMsg{
				text: fmt.Sprintf("project %s hidden — [c] off-row to view; [c] on the hidden row to un-hide", name),
			}
		} else {
			m.flash = &flashMsg{
				text: fmt.Sprintf("project %s un-hidden", name),
			}
		}
		// Cursor may now point past the end of the row list (we just
		// removed a project). Clamp to keep selection sane.
		m = clampDashCursor(m)
		return m, nil, true
	case rowSeparator:
		// Off-row toggle of show-hidden mode. Cursor stays on the
		// separator (or wherever it ends up after the row list shifts).
		m.showHidden = !m.showHidden
		// When toggling on, default the hidden group to expanded so
		// the operator immediately sees the rows they wanted to view.
		// Toggling off resets the expansion so a future [c] starts
		// from a clean state.
		if m.showHidden {
			m.hiddenExpanded = true
		} else {
			m.hiddenExpanded = false
		}
		m = clampDashCursor(m)
		return m, nil, true
	default:
		m.flash = &flashMsg{
			text:  "[c] hides projects — move cursor onto a project row, or off-row to toggle show-hidden mode",
			isErr: true,
		}
		return m, nil, true
	}
}

// clampDashCursor pulls the cursor back into the valid row range when
// a hide-toggle / show-hidden flip changes the row count. Without this
// the cursor could dangle past the end and selectedRow() returns nil
// for the next keypress, making [j]/[k] feel unresponsive until the
// operator notices.
func clampDashCursor(m Model) Model {
	rows := m.dashboardRows()
	if len(rows) == 0 {
		m.dashCursor = 0
		return m
	}
	if m.dashCursor >= len(rows) {
		m.dashCursor = len(rows) - 1
	}
	if m.dashCursor < 0 {
		m.dashCursor = 0
	}
	return m
}

// actionHandoff dispatches [h] for the row under the cursor.
//
// Inversion (2026-05-27): [h] is a PROJECT-LEVEL action. Coords are
// 1-per-project, so the operator's mental model for "hand off this
// agent's context" is "hand off this project's coord" — surfaced on
// the left-panel project row where the project context lives. Pressing
// [h] on a right-panel agent row used to be the only path and is now
// REJECTED with a hint pointing back to the project row.
//
// Branches:
//
//	rowProject + project has a live coord (CoordID resolves to a record)
//	  → run the same auto-red / precompact gate as the old agent path
//	    (committed handoff journal forbids manual re-handoff) and shell
//	    out via startHandoff(coord.ID).
//
//	rowProject + no live coord
//	  → flash "no coord for project <name> — spawn one with [a]". [a]
//	    on a project row is the auto-spawn path (issue #60), so it's
//	    the right actionable next step.
//
//	rowAgent
//	  → flash "[h] handoff applies to project rows (left panel) —
//	    move cursor onto a project row". No shell-out. The agent's
//	    coord-handoff is still reachable via [h] on its project row.
//
//	rowTask / rowWorker / rowSeparator / nil
//	  → flash "[h] handoff applies to project rows" with no shell-out.
//	    Old wording named v0.1 agents which is no longer accurate.
func (m Model) actionHandoff() (Model, tea.Cmd, bool) {
	row := m.selectedRow()
	if row == nil {
		m.flash = &flashMsg{
			text:  "[h] handoff applies to project rows (left panel) — move cursor onto a project row",
			isErr: true,
		}
		return m, nil, true
	}
	switch row.kind {
	case rowProject:
		return m.actionHandoffProject(row.project)
	case rowAgent:
		m.flash = &flashMsg{
			text:  "[h] handoff applies to project rows (left panel) — move cursor onto a project row",
			isErr: true,
		}
		return m, nil, true
	default:
		m.flash = &flashMsg{
			text:  "[h] handoff applies to project rows (left panel) — move cursor onto a project row",
			isErr: true,
		}
		return m, nil, true
	}
}

// actionHandoffProject is the project-row branch of [h]. Resolves the
// project's coord via the shared resolveCoordRecord helper (CoordID
// link first, then the records-scan fallback), applies the auto-red /
// precompact gate, and shells out via startHandoff. When the project
// has no live coord by EITHER path, flashes the "spawn a coord" hint.
//
// tui-nav-handoff-regressions Fix 2: the previous version resolved the
// coord ONLY via p.CoordID → findRecordByID. The dashboard's project→
// coord link (CoordID) races the agents refresh, so a live-but-unlinked
// coord made [h] flash "no coord" while the agents panel clearly showed
// it (live repro 2026-05-28: projects-fengshen-site coord 129c9824).
// resolveCoordRecord adds the same records-scan fallback that
// actionAttachProject already used, so [h] finds the in-flight coord.
//
// The gate logic mirrors what used to live on the rowAgent branch:
//
//   - auto-red / precompact: handoff journal is already committed on
//     disk, manual handoff would race the in-flight one. Suggest
//     `fleet drain` as the operator's next step.
//   - auto-yellow / live / asking / idle / blocked / review: gate
//     allows; [h] is the operator's escape hatch.
//
// "No coord" now means BOTH resolution paths missed: CoordID is empty
// (or points at an unloaded record) AND no record carries the
// coord-<project> task_id + marker + alive session. The operator's
// recovery is the same: wait for the coord, or spawn one with [a].
func (m Model) actionHandoffProject(p *ProjectRow) (Model, tea.Cmd, bool) {
	if p == nil {
		return m, nil, true
	}
	coord := m.resolveCoordRecord(p)
	if coord == nil {
		m.flash = &flashMsg{
			text:  fmt.Sprintf("no coord for project %s — spawn one with [a]", p.Name),
			isErr: true,
		}
		return m, nil, true
	}
	// Gate [h] only for COMMITTED handoff states (auto-red / precompact)
	// — those have a queue journal on disk, so a manual handoff would
	// race against the in-flight one. auto-yellow is NOT gated: the
	// journal hasn't been written yet (only after MILESTONE), and [h]
	// is the operator's escape hatch when the auto handoff stalls.
	// Identical predicate to the previous rowAgent branch, just moved
	// to the project-row path along with the rest of the keybind.
	status := deriveStatus(coord, m.aliveByID)
	if status == "auto-red" || status == "precompact" {
		m.flash = &flashMsg{
			text:  fmt.Sprintf("coord %s for project %s already has a handoff journal — `fleet drain` first", coord.ID, p.Name),
			isErr: true,
		}
		return m, nil, true
	}
	return m, m.startHandoff(coord.ID), true
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
	case rowProject:
		return m.actionArchiveProject(row.project)
	default:
		m.flash = &flashMsg{
			text:  "[x] applies only to v0.1 agents in this version — worker kill ships in v0.2.x",
			isErr: true,
		}
		return m, nil, true
	}
}

// actionArchiveProject is the project-row branch of [x] (issue #96 gap 3).
//
// Decision tree:
//
//	v0.2 project (~/.fleet/projects/<name>/ exists)
//	  → preserve existing behavior: flash that [x] doesn't apply.
//	    Task archival on project rows is out of scope for this PR.
//
//	pure-legacy v0.1 row (no v0.2 dir)
//	  ├─ at least one live agent tagged with project
//	  │   → refuse with operator-safety hint (mass-archive while a
//	  │     live agent is running risks killing in-flight work).
//	  └─ all tagged agents are dead
//	      → collect their IDs, enter modeConfirmDismissProject.
//	        y → fan out `fleet rm <id>` for each (existing CLI path);
//	        the rows naturally drop off the dashboard once all dead
//	        records are archived AND no live record carries the tag
//	        (unifiedProjects synthesizes rows from agent records — no
//	        records, no row).
//
// "Live" = deriveStatus(record, aliveByID) != "dead". The aliveByID
// cache is the same one the right-column status pills consume, so a
// project row dismissed here matches the operator's visible state.
//
// We intentionally do NOT mass-archive when ANY live agent is present.
// Surfacing the live-agent count in the flash gives the operator the
// information they need to either let the agent finish or [x] each
// agent individually first.
func (m Model) actionArchiveProject(p *ProjectRow) (Model, tea.Cmd, bool) {
	if p == nil {
		return m, nil, true
	}
	// v0.2 project? Preserve existing flash semantics. Worker-kill /
	// task-archive on project rows is a future PR; today this branch is
	// a no-op for live v0.2 projects.
	if projectTreeExistsFn(p.Name) {
		m.flash = &flashMsg{
			text:  "[x] applies only to v0.1 agents in this version — worker kill ships in v0.2.x",
			isErr: true,
		}
		return m, nil, true
	}
	// Pure-legacy row. Compute live + dead agents tagged with this
	// project. We tolerate nil records (early renders before the first
	// agentsMsg) — the row may have been synthesized by another signal
	// (e.g. unifiedProjects from a stale snapshot); we still let the
	// operator dismiss it cleanly when there are zero dead records too,
	// because the absence of agent records means there's nothing to
	// hold the row visible after the next refresh anyway.
	var live, dead []string
	for _, r := range m.records {
		if r == nil || r.Project != p.Name {
			continue
		}
		if deriveStatus(r, m.aliveByID) == "dead" {
			dead = append(dead, r.ID)
		} else {
			live = append(live, r.ID)
		}
	}
	if len(live) > 0 {
		m.flash = &flashMsg{
			text: fmt.Sprintf(
				"[x] refused: project %s still has %d live agent(s) — archive them first via [x] on each agent row",
				p.Name, len(live)),
			isErr: true,
		}
		return m, nil, true
	}
	// Fully dead (or zero records). Enter the dismiss confirmation.
	// Empty dead-agent list is fine: the confirm handler's `fleet rm`
	// fan-out is a no-op and the next dashboard refresh drops the
	// synthetic row naturally because there's no agent record to keep
	// it visible.
	m.dismissProjectCandidate = p.Name
	m.dismissProjectDeadAgents = append(m.dismissProjectDeadAgents[:0], dead...)
	m.mode = modeConfirmDismissProject
	return m, nil, true
}

// actionAttach dispatches [a] for the row under the cursor.
//
// Agent row   → tmux attach to the agent's session.
// Worker row  → tmux attach to the coord's session (issue #93 Phase B1):
//
//	workers in v0.2.x are Agent-tool subagents of the
//	project's coord, so the only live surface is the coord's
//	chat. Falls back to a "no attachable session" flash when
//	the coord can't be found / its session is dead. [⏎]
//	still opens the read-only peek panel for state.json +
//	output.log tail.
//
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
			// Resume-suggestion gate (resume-dead-coord-ab65): a dead
			// COORD agent (TaskID == "coord-<project>") has a working
			// resume path via [a] on the project row, which goes through
			// the dispatch CLI's synth-handoff recovery flow. Suggest
			// that instead of archive so the operator doesn't throw
			// state away. Non-coord dead agents (workers, manual
			// dispatches) keep the archive suggestion — there's no
			// resume path to point at.
			if cur.Project != "" && cur.TaskID == coordTaskID(cur.Project) {
				m.flash = &flashMsg{
					text: fmt.Sprintf(
						"coord %s session is dead — claude likely exited inside it. Press [a] on project %s to resume from last checkpoint (or [x] here to archive the orphan record).",
						cur.ID, cur.Project),
					isErr: true,
				}
				return m, nil, true
			}
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
		// Issue #93 Phase B1: workers in v0.2.x are Agent-tool subagents
		// of the project's coord — they have NO tmux session of their
		// own. Their "live" surface is the coord's chat window (where
		// the Agent tool renders the subagent's output as the
		// "N local agents" indicator). So [a] on a worker row routes to
		// the COORD's tmux session, not a per-worker session.
		//
		// Routing decision tree (in order):
		//
		//   coord found (alive)    → attach to coord (the place where
		//                             the operator can actually interact
		//                             with the worker subagent)
		//   no live coord found    → flash "no attachable session for
		//                             <slug>" — the worker is orphaned
		//                             (its parent coord exited or its
		//                             session is dead). [⏎] still opens
		//                             the peek panel as before.
		//
		// findExistingCoordForProject already gates on a live tmux
		// session via sessionProbeOrAliveFn, so the dead-session case
		// folds into the same orphan flash — no separate branch.
		//
		// The peek panel ([⏎]) path is preserved untouched — operators
		// who want state.json + output.log tail still get it via Enter.
		// [a] is now consistently "attach to a live session" across
		// agent / project / worker rows (no tmux means no attach).
		w := row.worker
		coord, ok := findExistingCoordForProject(m.records, w.Project)
		if !ok {
			m.flash = &flashMsg{
				text: fmt.Sprintf(
					"no attachable session for worker %s — its coord exited or its session is dead; press [⏎] for the worker peek panel",
					w.Slug),
				isErr: true,
			}
			return m, nil, true
		}
		// Successful coord attach — surface the routing so the operator
		// knows they're landing in the coord chat (where the worker's
		// Agent-tool output renders), not in a per-worker session.
		m.flash = &flashMsg{
			text: fmt.Sprintf(
				"viewing coord chat for project %s (worker %s renders as a local agent there)",
				w.Project, w.Slug),
		}
		m.pendingAttach = coord.TmuxSession
		return m, tea.Quit, true
	case rowProject:
		return m.actionAttachProject(row.project)
	case rowTask:
		// Issue #75: [a] on a task row routes to the task's worker
		// peek panel — same path as [a] on a worker row. Workers in
		// v0.2 are `claude --print` subprocesses (no tmux), so
		// "attach" maps to opening the worker peek (state.json +
		// output.log tail) which IS where the operator sees what the
		// worker is asking. When no worker exists for the task, flash
		// the actionable retry hint.
		//
		// Codex iter-1 P2: gate on task status so todo/ready tasks
		// don't get a misleading "no worker" error. Those tasks
		// legitimately have no worker yet (operator hasn't dispatched);
		// flash a different hint that points at the right next step
		// (`fleet dispatch <task>`).
		if row.task == nil || row.task.Empty || row.task.More > 0 {
			return m, nil, true
		}
		// Codex iter-3 P1: pre-dispatch tasks need accurate next-step
		// guidance. Workers are spawned by the coord, NOT by [d] (which
		// is the loose-agent repo picker). The coord auto-dispatches
		// tasks in `ready` status, so the right operator action is:
		//   - todo  → `fleet tasks promote <slug>` to flip to ready.
		//   - ready → wait for the coord to pick it up (or check that
		//             a coord exists for this project via [a] on the
		//             project row).
		//
		// Codex iter-4 P3: re-read live status from tasks.md so a
		// task transitioning between snapshots doesn't dead-end on
		// stale routing.
		//
		// Codex iter-5 P2: prefer attaching to an existing worker
		// peek even when status is todo/ready. The reconcile path
		// flips failed workers back to todo by clearing only
		// worker_pid; the worker dir + state.json + output.log are
		// preserved for post-mortem. Routing on status alone would
		// hide that history. attachToTaskWorker is the right
		// dispatcher: it peeks when a worker exists (active or
		// archived) and falls through to the no-worker hint when
		// neither does, at which point the row-side handler still
		// gets the chance to override with the more specific
		// promote / wait-for-coord guidance.
		return m.attachToTaskOrHint(row.parentProject, row.task.Slug, row.task.Status)
	default:
		// Future row kinds — neither tmux nor a peek surface fits. Flash
		// so the operator sees the no-op.
		m.flash = &flashMsg{
			text:  "[a] attach applies to projects (coord), agents (tmux), workers (peek), or tasks (worker peek)",
			isErr: true,
		}
		return m, nil, true
	}
}

// noWorkerHintTodo and noWorkerRecoveryHint emit the operator-facing
// flash text for [a] on a task with no worker. Codex iter-4 P2: both
// shapes embed the project name + --project flag because the
// dashboard cursor can be on a project different from the operator's
// shell cwd. Without --project, `fleet tasks promote <slug>` resolves
// against cwd's project tag and updates the wrong tasks.md (or
// errors). Rendering the project explicitly lets the operator copy-
// paste the command without tripping over cross-project context.
func noWorkerHintTodo(project, slug string) string {
	return fmt.Sprintf(
		"task %s/%s is todo — `fleet tasks promote %s --project %s` to make it eligible for the coord",
		project, slug, slug, project)
}

func noWorkerHintReady(project, slug string) string {
	return fmt.Sprintf(
		"task %s/%s is ready — waiting for the coord to dispatch a worker; check coord on the project row",
		project, slug)
}

// noWorkerHintInReview emits the post-completion holding-state hint.
// Codex iter-6 P2: in-review tasks have no active worker BY DESIGN —
// the coord archives the worker dir on phase=done and the task
// status flips to in-review pending CI / merge. Suggesting
// status=ready would silently re-dispatch already-completed work.
// `fleet peek <slug>` reads from the active dir then falls back to
// archive automatically (peek.go's readWorkerStateAnywhere).
func noWorkerHintInReview(project, slug string) string {
	return fmt.Sprintf(
		"task %s/%s is in-review — work is done, awaiting CI / merge; `fleet peek %s --project %s` for the archived worker logs",
		project, slug, slug, project)
}

// noWorkerHintTerminal covers done / abandoned. Worker dir is archived
// and may have been pruned (default 7d). No re-dispatch path applies.
func noWorkerHintTerminal(project, slug, status string) string {
	return fmt.Sprintf(
		"task %s/%s is %s — terminal state, no worker to attach to; `fleet peek %s --project %s` for archived logs (if not yet pruned)",
		project, slug, status, slug, project)
}

func noWorkerRecoveryHint(project, slug string) string {
	return fmt.Sprintf(
		"no worker for task %s/%s — `fleet tasks set %s status=ready --project %s` to retry",
		project, slug, slug, project)
}

// attachToTaskOrHint is the unified [a] dispatcher for task rows and
// the task detail panel. Routes by worker existence, NOT by task
// status:
//
//  1. Worker state.json exists OR worker dir exists (state.json may
//     not have landed yet — startup/rename window) OR archive dir
//     exists → open peek panel via attachToTaskWorker (which handles
//     all three via readWorkerDetail's active+archive split).
//  2. No worker on disk
//     → flash status-aware guidance (promote / wait-for-coord /
//     retry) so the operator's next step matches the task's
//     actual lifecycle position.
//
// Codex iter-5 P2 fix: the previous version short-circuited on
// status BEFORE checking worker existence, hiding existing logs from
// reset-to-todo recovery flows.
//
// Codex iter-3 P2 / iter-4 P3 are preserved here: live status read
// overrides the snapshot status so a transition between refreshes
// doesn't dead-end on stale routing.
//
// Codex iter-8 P2 #1: when liveTaskStatus returns "" AND no worker
// exists (active/archive/dir), the task has disappeared between the
// snapshot and the keypress. Surface a not-found error instead of
// suggesting promote/retry against a phantom slug.
//
// Codex iter-8 P2 #2: when the worker dir exists but state.json
// hasn't landed yet (startup window), readWorkerDetail's
// "(no state.json yet)" path is the right peek — route through it
// instead of the no-worker hint.
func (m Model) attachToTaskOrHint(project, slug, snapshotStatus string) (Model, tea.Cmd, bool) {
	if slug == "" {
		return m, nil, true
	}
	// Worker check first. Active state.json:
	ws, err := readTaskWorker(project, slug)
	if err == nil && ws != nil {
		return m.attachToTaskWorker(project, slug)
	}
	// Genuine read error (corrupt state.json, permission denied) —
	// surface verbatim. attachToTaskWorker carries the same error
	// rendering, so route through it for consistency.
	if err != nil {
		return m.attachToTaskWorker(project, slug)
	}
	// ws == nil && err == nil → ENOENT on state.json. Worker dir
	// itself may still exist (startup window before first
	// UpdateState). readWorkerDetail's "(no state.json yet)" path
	// is the right surface in that case (codex iter-8 P2).
	if taskWorkerDirExists(project, slug) {
		return m.attachToTaskWorker(project, slug)
	}
	// Active dir gone. Try the archive.
	if taskWorkerArchiveExists(project, slug) {
		return m.attachToTaskWorker(project, slug)
	}
	// No worker, active or archived. Codex iter-8 P2 #1 / iter-9 P2:
	// if the task itself is genuinely missing from tasks.md, surface
	// not-found. liveTaskStatus's missing=true flag is the
	// authoritative signal — distinguishes "tasks.md read OK, slug
	// gone" (deleted) from "tasks.md unreadable" (transient I/O).
	live, missing := liveTaskStatus(project, slug)
	if missing {
		m.flash = &flashMsg{
			text: fmt.Sprintf(
				"task %s/%s no longer exists in tasks.md (archived or deleted) — refresh and pick a current task",
				project, slug),
			isErr: true,
		}
		m.detail = nil
		return m, nil, true
	}
	status := snapshotStatus
	if live != "" {
		status = live
	}
	switch status {
	case "todo":
		m.flash = &flashMsg{text: noWorkerHintTodo(project, slug), isErr: true}
	case "ready":
		m.flash = &flashMsg{text: noWorkerHintReady(project, slug), isErr: true}
	case "in-review":
		// Codex iter-6 P2: in-review is the post-completion holding
		// state — the coord moves finished tasks here AFTER archiving
		// the worker. Once the archive is pruned (default 7d) the
		// task naturally has no worker on disk; suggesting status=ready
		// would re-dispatch already-merged work. Surface the
		// review-pending state instead.
		m.flash = &flashMsg{text: noWorkerHintInReview(project, slug), isErr: true}
	case "done", "abandoned":
		// Terminal states. The worker is gone by design (worker dir
		// archived + pruned). Don't suggest a re-dispatch path.
		m.flash = &flashMsg{text: noWorkerHintTerminal(project, slug, status), isErr: true}
	default:
		// blocked / in-progress — the task should have a worker.
		// Surface the recovery hint so the operator can re-dispatch.
		m.flash = &flashMsg{text: noWorkerRecoveryHint(project, slug), isErr: true}
	}
	// Dismiss any open detail panel so the flash isn't hidden behind
	// a stale overlay.
	m.detail = nil
	return m, nil, true
}

// taskWorkerDirExists returns true when ~/.fleet/projects/<project>/
// workers/<slug>/ exists on disk. Codex iter-8 P2: closes the
// state.json-not-yet-written gap so the [a] flow doesn't dead-end on
// the no-worker hint during the worker's first-tick startup window.
// var so tests can stub.
var taskWorkerDirExists = func(project, slug string) bool {
	dir, err := state.WorkerDir(project, slug)
	if err != nil {
		return false
	}
	info, err := os.Stat(strings.TrimSuffix(dir, string(filepath.Separator)))
	if err != nil {
		return false
	}
	return info.IsDir()
}

// attachToTaskWorker is the [a] handler for both task rows and the
// task detail panel. Routes to the task's worker peek panel when a
// worker exists (active OR archived); flashes a context-appropriate
// hint otherwise.
//
// Workers in v0.2 are `claude --print` subprocesses, NOT tmux
// sessions — so "attach" here means "open the same peek panel that
// [a] on a worker row opens" (state.json + last 50 lines of
// output.log via readWorkerDetail). That's the v0.2 surface where
// the operator sees what the worker wrote when it transitioned to
// blocked.
//
// readTaskWorker is the indirection layer — same stub used by
// readTaskDetail — so tests can seed a worker without a real
// state.json file. (nil, nil) means "no worker on disk" (ErrNotFound
// flattened); (nil, err) means a real read error.
//
// Codex iter-1 P2: distinguish "no worker" from "read failed" so the
// flash points at the right next step. The fleet tasks set syntax is
// `<slug> status=<value>` (NOT --status); valid statuses are
// todo|ready|in-progress|in-review|done|blocked|abandoned (no
// "pending"). Suggesting an invalid command would just fail validation
// and leave the operator stuck.
//
// Codex iter-3 P2: when the active worker dir is gone but an archive
// dir exists for the slug, route to readWorkerDetail anyway —
// readWorkerDetail's archive fallback (newestArchiveWorkerDir) will
// surface the archived state.json + output.log. Without this, [a] on
// a task whose worker was archived between snapshots would dead-end at
// the retry hint while the worker's row still has a live peek path.
func (m Model) attachToTaskWorker(project, slug string) (Model, tea.Cmd, bool) {
	if slug == "" {
		return m, nil, true
	}
	ws, err := readTaskWorker(project, slug)
	switch {
	case err != nil:
		// Real read error — surface verbatim so the operator can see
		// the underlying disk/parse failure. "No worker" would mislead.
		m.flash = &flashMsg{
			text:  fmt.Sprintf("worker state for %s unreadable: %v", slug, err),
			isErr: true,
		}
		m.detail = nil
		return m, nil, true
	case ws == nil:
		// ErrNotFound on state.json. Three reasons to still peek:
		//   1. worker dir exists but state.json hasn't landed yet
		//      (startup window — codex iter-8 P2 #2).
		//   2. archive dir exists (codex iter-3 P2).
		// Otherwise flash the recovery hint.
		if !taskWorkerDirExists(project, slug) && !taskWorkerArchiveExists(project, slug) {
			m.flash = &flashMsg{
				text:  noWorkerRecoveryHint(project, slug),
				isErr: true,
			}
			// Dismiss the detail panel if it was open: the flash carries
			// the actionable hint and a stale panel under it would just
			// confuse the operator.
			m.detail = nil
			return m, nil, true
		}
		// Fall through to readWorkerDetail — handles the
		// "(no state.json yet)" case AND the archive fallback.
	}
	body, title := readWorkerDetail(project, slug)
	m.detail = &detailView{title: title, body: body}
	return m, nil, true
}

// taskWorkerArchiveExists returns true when ~/.fleet/projects/<project>/
// workers/archive/ contains at least one entry whose stripped name
// matches the slug. Wraps newestArchiveWorkerDir's existing detection so
// attachToTaskWorker can decide whether to peek (active or archived) or
// flash the no-worker hint. Codex iter-3 P2.
//
// var so tests can stub the archive presence without seeding archive
// dirs. Production calls newestArchiveWorkerDir which is the same
// helper readWorkerDetail uses, keeping the two paths in sync.
var taskWorkerArchiveExists = func(project, slug string) bool {
	return newestArchiveWorkerDir(project, slug) != ""
}

// actionAttachProject is the project-row branch of [a] (issues #60, #63).
//
// Three paths, in order:
//
//  1. Lock-body match: project.CoordID is set (dashboard freshness gate
//     passed: lock body + coord-state.json mtime within
//     coordActiveWindow). Find the matching agent.Record by ID, attach.
//
//  2. Task_id fallback: even when CoordID is empty, an agent record
//     tagged task_id == coord-<project> AND project == <project> with
//     an alive tmux session is the project's coord by intent — the
//     skill just hasn't ticked yet (10-30s boot window). Attach to it
//     instead of spawning a duplicate. This is the primary issue-#63
//     idempotency mechanism: a second [a] press during the boot window
//     finds the in-flight record here.
//
//  3. No coord: pre-init the project tree, then spawn one. Stable
//     task_id ("coord-<name>") so subsequent [a] presses route into
//     path 2 instead of stacking duplicates. Attach immediately after
//     dispatch returns — no lock-poll (PR #62's 2s budget fired the
//     spawn-timeout banner under normal operation).
//
// Single-coord-per-project enforcement remains upstream: the coord
// skill NB-flocks coordinator.lock at first tick and exits cleanly if
// another coord beats it.
func (m Model) actionAttachProject(p *ProjectRow) (Model, tea.Cmd, bool) {
	if p == nil {
		return m, nil, true
	}
	// Path 1: fresh coord exists (lock body + freshness gate). Resolve
	// the record via the shared resolveCoordRecord helper (CoordID link
	// first, then the records-scan fallback — tui-nav-handoff-regressions
	// Fix 2) so attach benefits from the same fallback [h] now uses when
	// CoordID points at an unloaded record. Verify the session is alive
	// before committing to attach. A coord whose tmux session died but
	// whose lock body wasn't yet stale would get dispatched to a dead
	// session otherwise — same UX bug as the agent-row branch's "no
	// sessions" failure mode. Downstream dead-session recovery + the
	// "pending refresh" race flash are unchanged; only the lookup that
	// produces `rec` swapped to the shared helper.
	if p.CoordID != "" {
		if rec := m.resolveCoordRecord(p); rec != nil && rec.TmuxSession != "" {
			// Probe with sessionProbeOrAliveFn (codex review iter-6 P1):
			// the bare sessionAliveFn returns false on transport errors
			// (bad FLEET_TMUX_SOCKET, restarting tmux server, etc.),
			// which would falsely trigger the recovery spawn against a
			// still-live coord on a different socket. The
			// sessionProbeOrAliveFn helper distinguishes definitive
			// "no such session" from probe-error: the latter is treated
			// as alive (conservative) so we don't dispatch a duplicate
			// over a transport hiccup.
			if !sessionProbeOrAliveFn(rec.TmuxSession) {
				// Dead tmux session: re-dispatch the coord under the
				// same stable task_id ("coord-<project>"). The dispatch
				// CLI's dead-coord detection (see
				// cmd/fleet/dispatch_recovery.go) writes a synth-handoff
				// doc from on-disk state and points the successor at it,
				// so the resume picks up where the dead coord left
				// off without throwing state away. Previously this
				// case flashed "press [x] to archive, then [a] to
				// respawn" — which threw state away by archiving the
				// only link the dispatch path has back to the dead
				// coord's worker state. Flash the resume so the
				// operator knows we're not silently respawning.
				if _, err := state.EnsureProjectInitialized(p.Name); err != nil {
					m.flash = &flashMsg{
						text:  fmt.Sprintf("project %s: init failed: %v", p.Name, err),
						isErr: true,
					}
					return m, nil, true
				}
				// In-flight guard (codex review iter-6 P2): the lower
				// fresh-spawn path 2.6 below has the same check; we
				// honor it here too so an operator's double-tap on
				// [a] before the first coordSpawnDoneMsg arrives
				// doesn't fire a second recovery dispatch against the
				// same project state.
				if m.coordSpawnInFlight[p.Name] {
					m.flash = &flashMsg{
						text: fmt.Sprintf(
							"coord resume for project %s already in flight — wait for it to finish",
							p.Name),
						isErr: true,
					}
					return m, nil, true
				}
				if m.coordSpawnInFlight == nil {
					m.coordSpawnInFlight = map[string]bool{}
				}
				m.coordSpawnInFlight[p.Name] = true
				// Pass the DEAD COORD's own cwd, not the project's
				// first-record cwd (codex review iter-11 P2). The
				// dispatch CLI's recovery path falls back to the dead
				// coord's Cwd when --cwd is empty (codex iter-9 P2),
				// but if we pass any --cwd here it wins. Sending
				// rec.Cwd specifically keeps the resumed coord in the
				// SAME checkout the dead coord ran in, even when
				// other workers/reviewers for the same project live
				// in different worktrees.
				//
				// Empty rec.Cwd legitimately falls through to
				// startCoordSpawn → omits --cwd → dispatch CLI's
				// OldRecord.Cwd fallback fires.
				m.flash = &flashMsg{
					text: fmt.Sprintf(
						"resuming coord %s for project %s from last checkpoint (dead tmux session — synth handoff)",
						rec.ID, p.Name),
				}
				return m, m.startCoordSpawn(p.Name, rec.Cwd), true
			}
			m.pendingAttach = rec.TmuxSession
			return m, tea.Quit, true
		}
		// CoordID set but resolveCoordRecord returned nil. Two distinct
		// causes, and they need OPPOSITE handling (dead-end-recovery
		// -r-8559):
		//
		//   A. Transient snapshot-race — the dashboard picked up the
		//      lock body before agentsMsg refreshed m.records, and the
		//      holder is NOT archived. The record will appear next
		//      tick. Keep the iter-1 P2 guard: flash "pending refresh"
		//      and DO NOT scan (the scan keys off the marker and could
		//      bind a different stale coord). The operator re-presses
		//      [a] after the next refresh.
		//
		//   B. Terminally-stale link — the holder was archived (handoff
		//      / crash reaped it) yet coordinator.lock still names it.
		//      Re-pressing [a] would loop "pending refresh" FOREVER
		//      because the record will NEVER load (it's gone). This is
		//      the operator's hard dead-end (2026-05-28: projects-
		//      fengshen-site coord 129c9824). Fall through to the SAME
		//      scan fallbacks Path 2/2.5 use to resolve the LIVE
		//      successor coord; if a successor is found, attach to it.
		//      If no successor resolves either, surface the stale-lock
		//      diagnosis with the concrete next step ([r] reset) rather
		//      than the misleading "try again" flash.
		if coordArchivedFn(p.CoordID) {
			// Terminal stale: the linked holder is in the archive.
			// Resolve the live successor via the read-only scan
			// fallbacks (marker scan, then lock body). These are the
			// same tiers actionAttachProject's empty-CoordID path uses;
			// reaching them here is safe because the CoordID link is
			// known-dead, so the iter-1 P2 "don't bind a stale coord"
			// concern doesn't apply (the linked coord is already gone).
			if rec, ok := findExistingCoordForProject(m.records, p.Name); ok {
				m.pendingAttach = rec.TmuxSession
				return m, tea.Quit, true
			}
			if rec, ok := findCoordByLockBody(m.records, p.Name); ok {
				m.pendingAttach = rec.TmuxSession
				return m, tea.Quit, true
			}
			// No live successor reachable. Surface the stale-lock
			// diagnosis + the [r] reset escape hatch (feedback_surface
			// _dont_silo: name the concrete next command, never loop a
			// dead-end flash).
			m.flash = &flashMsg{
				text: fmt.Sprintf(
					"coord %s for project %s is stale (archived holder still in coordinator.lock) — press [r] to reset and respawn",
					p.CoordID, p.Name),
				isErr: true,
			}
			return m, nil, true
		}
		// Transient race: holder not archived, record just not loaded
		// yet. Keep the retry hint + the no-scan guard.
		m.flash = &flashMsg{
			text: fmt.Sprintf(
				"coord %s for project %s pending refresh — try [a] again in a moment",
				p.CoordID, p.Name),
			isErr: true,
		}
		return m, nil, true
	}
	// Path 2: task_id fallback. Idempotency for [a] during the
	// skill-boot window — find the alive in-flight record by task_id
	// and attach instead of spawning a duplicate (issue #63).
	if rec, ok := findExistingCoordForProject(m.records, p.Name); ok {
		m.pendingAttach = rec.TmuxSession
		return m, tea.Quit, true
	}
	// Path 2.5: lock-body fallback (codex iter-7 P2). Recovery case
	// after a prompt-delivery failure: the operator attached and
	// manually typed /coordinator; the skill ran and wrote the lock
	// body — but no coord-spawn marker exists (we deliberately
	// skipped it on the failed dispatch). The dashboard's freshness
	// gate may also lag if coord-state.json hasn't ticked yet.
	// Reading the lock body directly bridges that gap: an ID written
	// into coordinator.lock came from a coord that successfully
	// acquired LOCK_EX, so it's authoritative regardless of marker
	// state.
	if rec, ok := findCoordByLockBody(m.records, p.Name); ok {
		m.pendingAttach = rec.TmuxSession
		return m, tea.Quit, true
	}
	// Path 2.6: in-flight gate. coordSpawnInFlight tracks projects
	// whose dispatch goroutine has launched but coordSpawnDoneMsg
	// hasn't arrived yet. During this window the agent record + marker
	// don't exist on disk, so paths 1/2/2.5 would all miss and
	// we'd duplicate-spawn (codex iter-3 P2 follow-up).
	if m.coordSpawnInFlight[p.Name] {
		m.flash = &flashMsg{
			text: fmt.Sprintf(
				"coord spawn for project %s is in flight — wait a moment then re-press [a]",
				p.Name),
			isErr: true,
		}
		return m, nil, true
	}
	// Path 3: no coord. Pre-init the project tree (so the skill's first
	// tick can write coord-state.json and acquire the flock), then spawn.
	if _, err := state.EnsureProjectInitialized(p.Name); err != nil {
		m.flash = &flashMsg{
			text:  fmt.Sprintf("project %s: init failed: %v", p.Name, err),
			isErr: true,
		}
		return m, nil, true
	}
	if m.coordSpawnInFlight == nil {
		m.coordSpawnInFlight = map[string]bool{}
	}
	m.coordSpawnInFlight[p.Name] = true
	cwd := coordCwdForProject(m.records, p.Name)
	return m, m.startCoordSpawn(p.Name, cwd), true
}

// actionReset dispatches [r] for the row under the cursor
// (dead-end-recovery-r-8559). [r] is a PROJECT-LEVEL recovery key: it
// reaps a tangled / dead-locked project's coord state and respawns one
// clean coord. Only project rows apply; every other row kind (agent,
// task, worker, separator, nil) flashes a "doesn't apply" hint pointing
// at the project row.
func (m Model) actionReset() (Model, tea.Cmd, bool) {
	row := m.selectedRow()
	if row == nil {
		m.flash = &flashMsg{
			text:  "[r] reset applies to project rows (left panel) — move cursor onto a project row",
			isErr: true,
		}
		return m, nil, true
	}
	switch row.kind {
	case rowProject:
		return m.actionResetProject(row.project)
	default:
		m.flash = &flashMsg{
			text:  "[r] reset applies to project rows (left panel) — move cursor onto a project row",
			isErr: true,
		}
		return m, nil, true
	}
}

// actionResetProject is the project-row branch of [r]. It does NOT
// mutate on press: it freezes the project name + the snapshot of ALL
// coord-record IDs tagged with the project, then enters modeConfirmReset
// for a y/N gate (destructive — reaps sessions/records AND clears the
// lock). The reap + respawn fires only on confirm (handleConfirmResetKey).
//
// Freezing the coord IDs here (not at confirm time) makes the prompt
// count authoritative: a refresh between press and confirm can't change
// WHICH records [y] reaps.
func (m Model) actionResetProject(p *ProjectRow) (Model, tea.Cmd, bool) {
	if p == nil {
		return m, nil, true
	}
	// codex iter-3 [P2]: refuse to arm reset while a coord spawn for this
	// project is still in flight ([a]/[+] dispatch goroutine running, its
	// coordSpawnDoneMsg not yet handled). The dispatch hasn't written the
	// new coord's record/marker yet, so m.records would freeze a snapshot
	// that MISSES it; reaping that snapshot then respawning would
	// duplicate the just-spawned coord. Make the operator wait for the
	// spawn to settle (the flash names the next step — surface-don't-silo).
	if m.coordSpawnInFlight[p.Name] {
		m.flash = &flashMsg{
			text: fmt.Sprintf(
				"coord spawn for project %s is in flight — wait for it to settle, then press [r] (resetting now could duplicate the spawning coord)",
				p.Name),
			isErr: true,
		}
		return m, nil, true
	}
	m.resetProjectCandidate = p.Name
	m.resetCoordIDs = coordRecordIDsForProject(m.records, p.Name)
	m.mode = modeConfirmReset
	return m, nil, true
}

// coordRecordIDsForProject returns the IDs of every agent record that is
// a coord for projectName by intent — task_id == coord-<project> AND
// project == <project>. Unlike findExistingCoordForProject this does NOT
// gate on marker presence or session liveness: reset must reap EVERY
// coord record for the project (live, dead, duplicate, marker-less), so
// the count is the full blast radius the operator confirms.
func coordRecordIDsForProject(records []*agent.Record, projectName string) []string {
	want := coordTaskID(projectName)
	var ids []string
	for _, r := range records {
		if r == nil {
			continue
		}
		if r.Project == projectName && r.TaskID == want {
			ids = append(ids, r.ID)
		}
	}
	return ids
}

// handleConfirmResetKey runs while modeConfirmReset is active
// (dead-end-recovery-r-8559). Same y/N/esc swallow-rest contract as the
// other confirm modes — j/k while the prompt is up must not move the
// cursor.
//
// On confirm (y/Y): emit the reap tea.Cmd (startReset) which fans
// `fleet rm <id>` per frozen coord ID, then clears the stale lock via
// `fleet gc --kinds=coord-locks --apply --project <name>`. The fresh
// coord spawn happens in the resetDoneMsg handler AFTER the reap
// returns, so the new coord doesn't race the leftovers it's replacing.
// We mark coordSpawnInFlight here so a stray [a] during the reap window
// doesn't double-spawn.
//
// On cancel (esc/n/N): drop to nav, clear the frozen state, no mutation.
func (m Model) handleConfirmResetKey(key string) (Model, tea.Cmd, bool) {
	switch key {
	case "y", "Y":
		project := m.resetProjectCandidate
		ids := m.resetCoordIDs
		m.mode = modeNav
		m.resetProjectCandidate = ""
		m.resetCoordIDs = nil
		if project == "" {
			return m, nil, true
		}
		// Gate the follow-on respawn so a stray [a] during the reap
		// window can't fire a competing dispatch.
		if m.coordSpawnInFlight == nil {
			m.coordSpawnInFlight = map[string]bool{}
		}
		m.coordSpawnInFlight[project] = true
		m.flash = &flashMsg{
			text: fmt.Sprintf(
				"resetting project %s — reaping %d coord record(s) + clearing stale lock, then respawning",
				project, len(ids)),
		}
		return m, m.startReset(project, ids), true
	case "esc", "n", "N":
		m.mode = modeNav
		m.resetProjectCandidate = ""
		m.resetCoordIDs = nil
		return m, nil, true
	}
	return m, nil, true
}

// startReset returns a tea.Cmd that runs the [r] reset reap as one
// shell-out chain, then emits resetDoneMsg. Order matters:
//
//  1. `fleet rm <id>` for each frozen coord record — kills the
//     duplicate fleet-<id> tmux session AND archives the record.
//  2. `fleet gc --kinds=coord-locks --apply --project <name>` — clears
//     the stale coordinator.lock (fleet owns its resources; we shell to
//     gc rather than hand-rolling rm so the lock-classifier's split-
//     brain defenses still run).
//
// The fresh-coord spawn is deliberately NOT here — it fires in the
// resetDoneMsg handler so a reap failure aborts before a new coord is
// born into a half-reaped project.
//
// All shell-outs go through runFleetCmd's serialized invocation; we
// chain them inside one tea.Cmd closure so the whole reap is one
// observable event boundary (a single resetDoneMsg), matching the
// confirm-dismiss handler's "one confirm → one event" UX.
func (m Model) startReset(project string, coordIDs []string) tea.Cmd {
	return func() tea.Msg {
		return resetReapFn(project, coordIDs)
	}
}

// reapCoordIDsForProject is the authoritative coord-record set the
// reap reaps (codex iter-1 [P1]: re-read on-disk state at reap time).
// The frozen `coordIDs` (snapshotted at [r]-press from the in-memory
// dashboard) can MISS a live coord when [r] is pressed during the
// dashboard/agents refresh race, or while a coord spawn is still in
// flight — m.records simply hasn't loaded it yet. Reaping only the
// frozen set would leave that live coord's session/record alive while
// gc clears the lock and we spawn a replacement: split-brain (two live
// coords for one project).
//
// Fix: at reap time, re-enumerate the CURRENT on-disk coord records for
// the project (agent.List → same coord-by-intent filter) and UNION with
// the frozen IDs. The union (not replace) keeps every record the
// operator's confirm prompt counted AND catches any the snapshot missed.
// If the disk read fails we fall back to the frozen set rather than
// abort — a partial reap that includes the confirmed records still beats
// refusing recovery entirely (the operator can re-press [r]).
//
// Order is preserved deterministically: frozen IDs first (the order the
// prompt counted), then any newly-discovered on-disk IDs appended.
func reapCoordIDsForProject(project string, frozen []string) []string {
	ids := append([]string(nil), frozen...)
	seen := make(map[string]bool, len(frozen))
	for _, id := range frozen {
		seen[id] = true
	}
	records, err := agent.List()
	if err != nil {
		return ids // disk read failed: reap at least the confirmed set
	}
	for _, id := range coordRecordIDsForProject(records, project) {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids
}

// resetReapFn performs the [r] reset reap shell-out chain and returns
// the resulting resetDoneMsg. var so tests stub it without forking
// `fleet`. Production re-reads the CURRENT on-disk coord set (union with
// the frozen IDs — see reapCoordIDsForProject, codex iter-1 [P1]) so a
// coord missed by the press-time snapshot still gets reaped, then runs
// the rm-per-coord then gc-coord-locks chain; the first non-zero exit
// aborts and surfaces (reaped left at the count completed so the
// operator sees partial progress in the flash).
var resetReapFn = func(project string, frozenIDs []string) tea.Msg {
	coordIDs := reapCoordIDsForProject(project, frozenIDs)
	var out strings.Builder
	for i, id := range coordIDs {
		cmd := exec.Command(fleetBinary, "rm", id)
		b, err := cmd.CombinedOutput()
		out.Write(b)
		if err != nil {
			return resetDoneMsg{
				projectName: project,
				reaped:      i,
				out:         out.String(),
				err:         fmt.Errorf("rm %s: %w", id, err),
			}
		}
	}
	gc := exec.Command(fleetBinary, "gc",
		"--kinds=coord-locks", "--apply", "--project", project)
	b, err := gc.CombinedOutput()
	out.Write(b)
	if err != nil {
		return resetDoneMsg{
			projectName: project,
			reaped:      len(coordIDs),
			out:         out.String(),
			err:         fmt.Errorf("gc coord-locks: %w", err),
		}
	}
	// codex iter-2 [P1]: `fleet gc --kinds=coord-locks --apply` exits 0
	// even when it REFUSES to unlink a lock (verb=surface) — e.g. the
	// holder tmux is still alive, the SessionAlive probe was ambiguous,
	// or the inode-swap defense aborted the remove. Treating any zero
	// exit as "lock cleared" would let reset spawn a fresh coord while
	// the stale lock + its old holder are still present → split-brain.
	// Parse the per-action report: a coord-locks line whose verb is
	// anything other than `removed` is a refusal → fail the reset (no
	// respawn). No coord-locks line at all = there was nothing to clear
	// (the lock was already gone), which is a legitimate success.
	if refusal := coordLockRefusal(out.String()); refusal != "" {
		return resetDoneMsg{
			projectName: project,
			reaped:      len(coordIDs),
			out:         out.String(),
			err:         fmt.Errorf("gc coord-locks refused to clear the stale lock: %s", refusal),
		}
	}
	// codex iter-3 [P2]: the line-parse above catches an EXPLICIT refusal
	// (verb=surface), but gc can also emit NO coord-locks action at all
	// for an existing lock it couldn't classify — an unreadable / parse-
	// failing holder record, a malformed/torn body, or an ambiguous probe
	// all fall through to "skip" with no action line, yet the lock file is
	// STILL on disk. Treating that as success would respawn over a lock
	// the reset never proved gone. Backstop with a direct stat.
	if lerr := coordLockStillPresent(project); lerr != nil {
		return resetDoneMsg{
			projectName: project,
			reaped:      len(coordIDs),
			out:         out.String(),
			err:         lerr,
		}
	}
	return resetDoneMsg{
		projectName: project,
		reaped:      len(coordIDs),
		out:         out.String(),
	}
}

// coordLockStillPresent is the codex iter-3 [P2] post-gc backstop: after
// the reset reap archived every coord record for the project and gc ran
// (without an explicit refusal), the project's coordinator.lock MUST be
// gone for the "clear the stale lock" guarantee to hold. A surviving
// file means gc couldn't classify it (unreadable holder record, torn
// body, ambiguous probe → no action line) — the reset did NOT clear it,
// so we fail closed (no respawn). Respawn hasn't run yet, so nothing
// should be legitimately recreating the lock between reap and this stat.
//
// Returns nil when the lock is absent (success) or the path can't be
// resolved (can't make a claim → don't block recovery on a path error,
// the line-parse refusal already covered the classifiable cases). A stat
// error other than not-exist is itself ambiguous → fail closed.
func coordLockStillPresent(project string) error {
	lp, perr := state.CoordinatorLockPath(project)
	if perr != nil {
		return nil
	}
	_, serr := os.Stat(lp)
	if serr == nil {
		return fmt.Errorf(
			"coordinator.lock still present at %s after gc (gc could not classify it — likely an unreadable holder record or torn lock body); reset did not clear it",
			lp)
	}
	if !os.IsNotExist(serr) {
		return fmt.Errorf("stat coordinator.lock for %s after gc: %w", project, serr)
	}
	return nil
}

// coordLockRefusal scans `fleet gc` output for a coord-locks action that
// was NOT removed (codex iter-2 [P1]). Returns the reason text of the
// first such refusal, or "" if every coord-locks line was removed (or
// there were no coord-locks lines — nothing to clear). The per-action
// line format is pinned by cmd/fleet/gc.go's renderReport:
//
//	coord-locks  <target>  verb=<v>  reason=<r>
//
// Under --apply the only success verb is `removed`; `surface` (live
// holder / ambiguous probe / remove failed / inode-swap abort) and the
// dry-run `would-remove` both mean the lock is STILL on disk.
func coordLockRefusal(gcOutput string) string {
	for _, line := range strings.Split(gcOutput, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, string(gc.KindCoordLocks)+"  ") {
			continue
		}
		if strings.Contains(line, "verb="+string(gc.VerbRemoved)+"  ") ||
			strings.HasSuffix(line, "verb="+string(gc.VerbRemoved)) {
			continue // this lock was actually unlinked
		}
		// A coord-locks action that wasn't removed — surface the reason.
		if i := strings.Index(line, "reason="); i >= 0 {
			return strings.TrimSpace(line[i+len("reason="):])
		}
		return line
	}
	return ""
}

// resolveCoordRecord finds the live coord record for a project row,
// trying the dashboard-linked CoordID first, then the records scan that
// actionAttachProject already relies on (findExistingCoordForProject:
// an agent tagged task_id=coord-<project>, project matching, marker on
// disk, alive tmux session). Read-only — no spawn, no dead-session
// recovery; callers handle the not-found (nil) case.
//
// tui-nav-handoff-regressions Fix 2: shared by actionHandoffProject and
// actionAttachProject's initial coord lookup so [h] and [a] resolve the
// project's coord through the SAME chain. Previously only [a] had the
// scan fallback, so [h] flashed "no coord" when the dashboard's
// CoordID link hadn't published yet but a coord was demonstrably live
// in the agents panel.
//
// codex review iter-1 [P2]: the scan fallbacks fire ONLY when CoordID
// is empty. A non-empty CoordID is the dashboard's authoritative lock-
// body link — when it's set but the matching record hasn't loaded yet
// (the snapshot/agents refresh race), we must NOT fall through to the
// marker scan, which keys off coordSpawnMarkerFn and can resolve a
// DIFFERENT, stale coord-<project> record (e.g., an old session still
// alive mid-handoff while the new coord already holds the lock). Routing
// [a]/[h] to that stale coord would hand off / attach the wrong owner.
// Returning nil here preserves actionAttachProject's intentional
// "pending refresh — try again" flash for the CoordID-set race; the
// operator re-presses after the next tick binds the record.
//
// codex review iter-2 [P2]: when CoordID is empty, mirror BOTH of
// actionAttachProject's read-only fallbacks in order — the marker scan
// (findExistingCoordForProject, path 2) THEN the lock-body fallback
// (findCoordByLockBody, path 2.5). Without the lock-body tier, the
// prompt-delivery-recovery state (operator attached + manually typed
// /coordinator, so coordinator.lock names an alive coord but no marker
// was written) let [a] attach while [h] still flashed "no coord". The
// design's G2 — "[h] resolves via the SAME chain [a] uses" — requires
// the lock-body tier here too.
func (m Model) resolveCoordRecord(p *ProjectRow) *agent.Record {
	if p == nil {
		return nil
	}
	if p.CoordID != "" {
		// Authoritative link set: resolve by ID only. Do NOT scan —
		// the scan keys off the on-disk marker, which can lag behind a
		// fresh CoordID and surface a stale coord.
		return findRecordByID(m.records, p.CoordID)
	}
	if rec, ok := findExistingCoordForProject(m.records, p.Name); ok {
		return rec
	}
	if rec, ok := findCoordByLockBody(m.records, p.Name); ok {
		return rec
	}
	return nil
}

// coordArchivedFn reports whether an agent record was ARCHIVED — i.e.
// ~/.fleet/agents/archive/<id>.json exists. A CoordID that resolves
// neither to a live m.records entry NOR to a fresh on-disk record, but
// IS present in the archive, is a TERMINALLY STALE link: the holder
// handed off / crashed and got reaped, but the project's
// coordinator.lock body still names it. This is the
// dead-end-recovery-r-8559 root cause — a stale lock body strands the
// operator on the project row while a live successor coord sits
// unreachable. Distinguished from a transient snapshot-race (the record
// simply hasn't loaded yet AND is not in the archive) so the iter-1 P2
// "don't scan when CoordID set" guard still holds for the race.
//
// var so tests stub the archive presence without seeding archive dirs.
var coordArchivedFn = func(id string) bool {
	if id == "" {
		return false
	}
	p, err := state.AgentArchivePath(id)
	if err != nil {
		return false
	}
	_, statErr := os.Stat(p)
	return statErr == nil
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

// coordTaskID returns the canonical task_id used to mark an agent record
// as the coordinator for projectName. The shape is a stable, deterministic
// string — NOT a timestamp — so that:
//
//   - A second `[a]` press during the boot window finds the in-flight
//     coord (idempotency: same project → same task_id → existing record
//     wins, no duplicate spawn).
//   - The dashboard can fall back to the task_id signal when the lock
//     body hasn't published yet (issue #63: 10-30s boot window where the
//     lock body is empty but the agent IS the project's coord by intent).
//
// Project names are validated upstream via state.ValidateProjectName,
// so this never embeds a path-unsafe component.
func coordTaskID(projectName string) string {
	return "coord-" + projectName
}

// findCoordByLockBody returns the alive agent whose ID is written
// into ~/.fleet/projects/<projectName>/.locks/coordinator.lock. var
// for stub-ability; production reads the file via dashboard's
// readCoordHolder helper. Returns (nil, false) when:
//   - the lock file is missing / empty / malformed body,
//   - no record has the matching ID,
//   - the matching record's tmux session is not alive.
//
// Used by actionAttachProject's path 2.5 to handle the prompt-
// delivery-recovery case (codex iter-7 P2): operator attached after
// a prompt-failed dispatch, typed /coordinator manually, the skill
// acquired LOCK_EX and wrote its ID into the lock body. The marker
// file is absent (we skipped writing it), but the lock body is
// authoritative — re-attach instead of spawning a duplicate.
//
// Differs from path 1 (project.CoordID-driven) in that it does NOT
// require coord-state.json freshness. Lock body is set via LOCK_EX
// in the coord skill's _try_lock; presence of an ID there means a
// coord successfully acquired the lock at least once. flock doesn't
// truncate on release, so a stale body is possible — but we gate on
// the matching agent's tmux session being alive, which catches the
// "coord crashed but lock body remains" case.
var findCoordByLockBody = func(records []*agent.Record, projectName string) (*agent.Record, bool) {
	root, err := state.Root()
	if err != nil {
		return nil, false
	}
	holderID := readCoordHolder(filepath.Join(root, "projects"), projectName)
	if holderID == "" {
		return nil, false
	}
	for _, r := range records {
		if r == nil || r.ID != holderID {
			continue
		}
		if r.TmuxSession == "" {
			continue
		}
		if !sessionProbeOrAliveFn(r.TmuxSession) {
			continue
		}
		return r, true
	}
	return nil, false
}

// findExistingCoordForProject searches records for an alive agent
// already tagged as the coord for projectName. "Tagged" means
// task_id == coordTaskID(projectName) AND project == projectName
// AND ID matches the project's coord-spawn marker. "Alive" uses the
// tristate session probe so a tmux transport error doesn't drop a
// live claim.
//
// Returns (record, true) on a match; (nil, false) when nothing matches
// or every match has a dead session. Used by the project-row [a] handler
// to skip a duplicate dispatch when a coord was already spawned for the
// same project (issue #63's "[a] press during the 30s skill-boot window
// piles up zombies" failure mode).
//
// Marker requirement (codex iter-6 P1): a coord whose initial-prompt
// delivery FAILED has no marker on disk. The [a] re-attach path must
// NOT bind that session — the operator's intent on second [a] is to
// recover from the prompt failure, which means spawning fresh, not
// dropping back into a plain Claude shell. The marker requirement
// distinguishes "we successfully booted a coord here" from "we tried
// to but the prompt didn't land".
//
// No freshness / project-tree gates here (codex iter-5 P1): a coord
// stalled at a permissions prompt past 60s, or one whose project
// tree got moved, must still be re-attached rather than respawned.
// The dashboard's findCoordByTaskID is stricter because that's about
// rendering identity; this is about "is the in-flight session still
// the right thing to re-enter?"
//
// Tristate liveness (codex iter-6 P2): use sessionProbeOrAliveFn so a
// tmux transport error (bad FLEET_TMUX_SOCKET, restarting server)
// doesn't drop a live coord and force a duplicate spawn.
func findExistingCoordForProject(records []*agent.Record, projectName string) (*agent.Record, bool) {
	want := coordTaskID(projectName)
	wantID := coordSpawnMarkerFn(projectName)
	if wantID == "" {
		// No marker → either no coord ever booted here, or the previous
		// spawn's prompt failed (so we deliberately skipped writing
		// the marker). Either way [a] should fall through to spawn.
		return nil, false
	}
	for _, r := range records {
		if r == nil {
			continue
		}
		if r.TaskID != want {
			continue
		}
		if r.Project != projectName {
			continue
		}
		if r.ID != wantID {
			continue
		}
		if r.TmuxSession == "" {
			continue
		}
		if !sessionProbeOrAliveFn(r.TmuxSession) {
			continue
		}
		return r, true
	}
	return nil, false
}

// coordCwdForProject best-guess resolves the working directory for a
// freshly-spawned coord agent.
//
// Precedence:
//  1. The registered project meta.json repo_path. Fresh dashboard
//     coord spawns need this path because PLAN-DOC/TASK-PLAN-DOC write
//     relative docs/ files in the target project.
//  2. Any existing agent record tagged with the same Project — legacy
//     fallback for projects registered before meta.json existed.
//  3. The TUI process's own cwd via os.Getwd() — works when the
//     operator launched `fleet` from inside the project repo.
//  4. Empty string — `fleet dispatch` resolves empty cwd to caller's
//     wd, same as (2). Acceptable fallback.
func coordCwdForProject(records []*agent.Record, projectName string) string {
	if meta, err := projects.Read(projectName); err == nil && meta.RepoPath != "" {
		return meta.RepoPath
	}
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

// writeCoordSpawnMarkerFn writes the coord-spawn marker file under the
// per-project coord-spawn NB-flock so a concurrent coord.Cleanup on an
// older agent can't race our write (codex iter-3 [P1]: TUI marker write
// happens AFTER `fleet dispatch` releases its own coordlock, so without
// re-acquiring here the old coord's cleanup can interleave between our
// read and our unlink — even though cleanup itself holds the lock for
// its compare-and-remove step, the TUI write outside the lock reopens
// the race window).
//
// On contention (extremely rare — another dispatch / drain is in flight
// for the same project), we surface the error to the caller so the TUI
// flash message stays accurate. The caller already handles a non-nil
// werr by warning the operator that marker write failed (model.go:718).
//
// var so tests can stub the disk write (test helpers in
// internal/tui/keys_test.go install a fake that records calls without
// hitting FLEET_HOME or shelling out to flock).
var writeCoordSpawnMarkerFn = writeCoordSpawnMarkerLocked

// writeCoordSpawnMarkerLocked is the production implementation of
// writeCoordSpawnMarkerFn: acquire coord-spawn lock, write marker,
// release. The lock is the SAME one cmd/fleet/dispatch.go +
// internal/handoffop + internal/coord/cleanup take for their own
// marker-touching sections, so all four writers / clearers serialize
// cleanly. Without this serialization, codex iter-3 [P1] showed the
// race: cleanup acquires lock after dispatch released, reads old
// marker body, TUI writes new id (outside lock), cleanup unlinks the
// just-written new marker.
//
// Retry budget (codex iter-6 [P1]): coordlock.Acquire is non-blocking
// (LOCK_NB) and returns EWOULDBLOCK on contention. The previous fail-
// fast behavior was wrong for THIS caller: TUI invokes us AFTER a
// successful spawn — failing the marker write strands the new coord
// (record exists, marker doesn't, dashboard misses it). cleanup holds
// the lock for microseconds while it captures + restores the marker;
// a short retry budget rides over that window without making the
// operator wait noticeably. Dispatch / drain callers stay fail-fast
// because their criticial sections are longer and contention there
// means a real concurrent spawn, not a transient cleanup blip.
//
// Budget: 2 seconds total at 50ms intervals = 40 attempts. Well over
// the upper bound for a cleanup compare-and-remove; well under the
// human-perceptible latency floor (the TUI is already mid-attach when
// this runs).
func writeCoordSpawnMarkerLocked(projectName, agentID string) error {
	const (
		retryInterval = 50 * time.Millisecond
		retryBudget   = 2 * time.Second
	)
	deadline := time.Now().Add(retryBudget)
	var lastErr error
	for {
		release, err := coordlockAcquireFn(projectName)
		if err == nil {
			defer release()
			return state.WriteCoordSpawnMarker(projectName, agentID)
		}
		lastErr = err
		if time.Now().After(deadline) {
			return fmt.Errorf("writeCoordSpawnMarker: coord-spawn lock contended after %s of retries: %w",
				retryBudget, lastErr)
		}
		time.Sleep(retryInterval)
	}
}

// coordlockAcquireFn is the seam that the marker-lock unit test
// stubs to deterministically simulate contention without spinning
// up a real flock holder for the full 2s budget.
var coordlockAcquireFn = coordlock.Acquire

// dispatchAgentIDPattern matches the first line of `fleet dispatch`
// stdout: "agent <8-hex-id> spawned". We extract the ID so the
// follow-up lock-poll knows whose holder to expect. The stdout shape
// is stable across v0.1 / v0.2 — see cmd/fleet/dispatch.go runDispatch.
var dispatchAgentIDPattern = regexp.MustCompile(`(?m)^agent ([0-9a-f]{8}) spawned`)

// dispatchPromptFailedMarker is the stdout sigil printed by
// runDispatch when SendInitialPrompt failed. The dispatch CLI exits 0
// in this case (the agent + tmux session ARE up; the operator just
// has to type the prompt manually) but the coord skill never started
// — so the TUI must NOT write the coord-spawn marker, which would
// cause the dashboard to render a plain Claude session as the
// project's verified coord (codex iter-5 P2).
//
// Stable across the dispatch.go output shape; the CLI test
// TestDispatch_PromptFailureWarningShape pins the literal text.
const dispatchPromptFailedMarker = "initial prompt not delivered"

// startCoordSpawn shells out to `fleet dispatch` with the coord
// auto-prompt and returns immediately (issue #63: no lock-poll). On
// success the resulting coordSpawnDoneMsg carries the new agent's ID
// + tmux session; Update sets pendingAttach + tea.Quit so Run() exec's
// tmux attach and the operator watches Claude boot live.
//
// task ID format: "coord-<projectName>" — stable + deterministic per
// project. A duplicate [a] press during the skill-boot window finds
// the existing record via findExistingCoordForProject and attaches
// without re-dispatching (idempotency). Project names pass through
// state.ValidateProjectName upstream, so embedding the name is path-
// safe.
func (m Model) startCoordSpawn(projectName, cwd string) tea.Cmd {
	taskID := coordTaskID(projectName)
	prompt := coordSpawnPrompt(projectName)
	// --coord-spawn whitelists the reserved "coord-" task_id prefix at
	// the dispatch CLI (issue #63 codex iter-1 P2). Without it, an
	// operator-supplied `fleet dispatch coord-foo` would create a
	// worker the dashboard treats as the project's coord; with it,
	// only this code path can write the prefix.
	args := []string{"dispatch", taskID, "--project", projectName, "--coord-spawn", "--prompt", prompt}
	if cwd != "" {
		args = append(args, "--cwd", cwd)
	}
	// Engine propagation gated to claude-code (codex review iter-2 P1):
	// the TUI's `[a]` auto-spawn path bootstraps a project's coordinator,
	// and the coordinator skill (skills/coordinator/loop.py +
	// dispatch.py:format_dispatch_instruction) emits Claude-Agent-tool
	// DISPATCH blocks that ONLY a claude-code session can consume. If we
	// forwarded FLEET_ENGINE=codex unfiltered, an operator running `fleet
	// -codex` who clicks `[a]` would silently spawn a codex coord that
	// can't fan out workers/reviewers/finishers — the project row would
	// stall the first time it needed a subagent.
	//
	// MVP scope (memory project_codex_multi_engine.md): `fleet --engine
	// codex dispatch ...` works for explicit worker dispatches today;
	// codex-aware coord skill is a follow-up. Until then the TUI's auto-
	// spawn always launches a claude-code coord regardless of the
	// operator's session engine. Operators who want a codex coord
	// explicitly can `fleet --engine codex dispatch coord-<project>
	// --coord-spawn --project <project>` from the shell once the skill
	// supports it.
	//
	// We keep the explicit --engine claude-code stamp (rather than
	// letting it default) so the audit trail / process listing makes the
	// gating decision obvious and a future regression that drops this
	// guard is caught by reviewer eyeballs.
	args = append(args, "--engine", "claude-code")
	return runFleetCmd(args, func(out string, err error) tea.Msg {
		if err != nil {
			return coordSpawnDoneMsg{
				projectName: projectName,
				err:         fmt.Errorf("dispatch: %w\n%s", err, out),
			}
		}
		// Parse the new agent's ID from dispatch stdout. Without it we
		// can't tell which session to attach to and would have to
		// flash "spawned but can't track" — a bad UX. Treat parse
		// failure as fatal so the operator notices the dispatch output
		// drift; the agent record itself remains on disk and the
		// operator can attach via [a] on its right-column row.
		match := dispatchAgentIDPattern.FindStringSubmatch(out)
		if len(match) != 2 {
			return coordSpawnDoneMsg{
				projectName: projectName,
				err:         fmt.Errorf("dispatch output missing agent ID line:\n%s", out),
			}
		}
		agentID := match[1]
		// Codex iter-5 P2: if dispatch's stdout warned about a prompt
		// delivery failure, the coord skill never started — propagate
		// the signal so model.Update skips the marker write.
		promptOK := !bytes.Contains([]byte(out), []byte(dispatchPromptFailedMarker))
		return coordSpawnDoneMsg{
			projectName:     projectName,
			agentID:         agentID,
			session:         tmux.SessionName(agentID),
			promptDelivered: promptOK,
		}
	})
}

// coordSpawnPrompt builds the first-turn prompt body for a freshly
// spawned coord agent (issue #80). The body hard-constrains the agent's
// role to discuss, save approved plan docs, save task plan docs, and dispatch;
// implementation work is delegated to detached workers via the fleet CLI.
//
// The wording is mirrored verbatim by skills/coordinator/SKILL.md's
// "## Coord agent role" section so the constraint survives context
// handoffs (the successor coord re-reads SKILL.md on its first turn,
// not the original dispatch prompt).
//
// Multi-line: bracketed paste in sendInitialPrompt handles newlines.
func coordSpawnPrompt(projectName string) string {
	return fmt.Sprintf(`You are a Fleet COORDINATOR agent for project %s.

ROLE — discuss design with the operator, save approved plan docs, file tasks, dispatch workers. NEVER:
- Edit code files (no Edit, Write, NotebookEdit on source code).
- Run tests (no `+"`go test`"+`, `+"`pytest`"+`, etc. — workers handle this).
- Implement features inline.
- Run any tool that mutates the project source tree, except the narrow PLAN-DOC and TASK-PLAN-DOC steps below.

DELEGATE — for any implementation, testing, or code-touching work:
1. Discuss design with the operator until aligned.
2. PLAN-DOC: save the approved implementation plan as `+"`docs/DESIGN-<kebab-topic>.md`"+` and render `+"`docs/DESIGN-<kebab-topic>.html`"+` when the project has a renderer.
3. File tasks via `+"`fleet tasks add --project %s --spec <body>`"+` while keeping them unpromoted.
4. TASK-PLAN-DOC: save `+"`docs/TASK-PLAN-<slug>.md`"+` and render `+"`docs/TASK-PLAN-<slug>.html`"+` when supported.
5. Add the task plan path to worker-visible task text, e.g. `+"`fleet tasks note --project %s <slug> --section spec \"Task plan: docs/TASK-PLAN-<slug>.md\"`"+`.
6. Promote the task with `+"`fleet tasks promote <slug>`"+` only after its task plan doc exists and is linked or embedded.
7. The /coordinator skill auto-dispatches a worker on next tick.
8. Track progress via the supervisor loop.

ALLOWED — your toolbox is intentionally narrow:
- Read code files for design discussion (Read, Grep, Bash with non-mutating commands).
- Write/render approved implementation plan docs and per-task plan docs under the project's approved docs folder only (`+"`docs/`"+` when present).
- Run fleet CLI: `+"`fleet tasks {add,list,show,set,note,promote}`"+`, `+"`fleet workers list`"+`, `+"`fleet peek`"+`, `+"`fleet learnings`"+`, `+"`fleet standards show`"+`.
- Run gh CLI for status: `+"`gh pr view`"+`, `+"`gh pr checks`"+`, `+"`gh issue view`"+`.
- Talk to the operator about design, scope, priority.

Run /coordinator now to begin the supervisor loop.`, projectName, projectName, projectName)
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
		body, title, loaded := readTaskDetail(row.parentProject, row.task.Slug)
		// Issue #75: carry task identity (project + slug + status) so
		// [a] inside the panel routes to the matching worker peek for
		// blocked/in-progress tasks AND to the dispatch hint for
		// todo/ready tasks. Without status the panel-side [a] would
		// regress on pre-dispatch tasks (codex iter-2 P2). Non-task
		// panels leave these empty and the [a] interceptor falls
		// through to default attach behavior.
		//
		// Codex iter-7 P3: only arm task-panel [a] when the task
		// loaded successfully. If readTaskDetail returned an error
		// body (slug missing, tasks.md unreadable), the panel still
		// renders the error text but [a] should fall through to
		// default dismiss instead of running the stale-row attach
		// flow against a task that may no longer exist.
		dv := &detailView{title: title, body: body}
		if loaded {
			dv.taskProject = row.parentProject
			dv.taskSlug = row.task.Slug
			dv.taskStatus = row.task.Status
		}
		m.detail = dv
	case rowWorker:
		body, title := readWorkerDetail(row.worker.Project, row.worker.Slug)
		m.detail = &detailView{title: title, body: body}
	case rowAgent:
		body, title := readAgentDetail(row.agent)
		m.detail = &detailView{title: title, body: body}
	case rowSeparator:
		// Issue #98: [enter] on a separator toggles its group's
		// expansion. Cursor stays on the separator so [j]/[k] can
		// walk into the newly-revealed rows.
		if row.separator == nil {
			return m, nil, true
		}
		switch row.separator.kind {
		case separatorIdle:
			m.idleExpanded = !m.idleExpanded
			// Mark explicit so the auto-suppress logic in
			// dashboardRows doesn't override the operator's choice on
			// the next render (an explicit collapse should stick even
			// when zero projects are active).
			m.idleCollapseExplicit = true
		case separatorHidden:
			m.hiddenExpanded = !m.hiddenExpanded
		case separatorHistory:
			// Issue #101: [enter] on the `─── N done ───` separator
			// toggles the history group expansion for the parent
			// project. Cursor stays on the separator so [j] walks
			// into newly-revealed history rows.
			if m.historyExpanded == nil {
				m.historyExpanded = map[string]bool{}
			}
			proj := row.separator.project
			if m.historyExpanded[proj] {
				delete(m.historyExpanded, proj)
			} else {
				m.historyExpanded[proj] = true
			}
		case separatorAgentIdle:
			// dashboard-accumulation-f-4421 Sub-fix B: [enter] on the
			// right-column "─── N idle ───" separator toggles the v0.1
			// agent-idle group. asking/blocked agents are never
			// inside this bucket — they render above the separator
			// regardless of staleness — so the toggle only ever
			// exposes truly-idle records.
			m.agentIdleExpanded = !m.agentIdleExpanded
		}
		m = clampDashCursor(m)
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

// handleConfirmDismissProjectKey runs while modeConfirmDismissProject
// is active (issue #96 gap 3). Same y/N/esc swallow-rest contract as
// modeConfirmArchive — j/k while the prompt is up must not move the
// cursor or change WHICH project the next [y] would dismiss.
//
// On confirm: dispatch one `fleet rm <id>` per dead-agent ID captured
// at press time. The CLI is the existing destructive path; we don't
// reimplement archive here. Returns a tea.Batch so the operator's
// confirm produces a single observable event boundary even when the
// project had multiple dead records.
//
// On a row with zero dead records (operator dismissed a synthetic row
// kept alive only by a transient render-time fluke), confirm is a
// no-op + a refresh — the next dashboardMsg / agentsMsg will drop the
// row naturally.
func (m Model) handleConfirmDismissProjectKey(key string) (Model, tea.Cmd, bool) {
	switch key {
	case "y", "Y":
		project := m.dismissProjectCandidate
		dead := m.dismissProjectDeadAgents
		m.mode = modeNav
		m.dismissProjectCandidate = ""
		m.dismissProjectDeadAgents = nil
		if len(dead) == 0 {
			// Nothing to archive — just nudge the loaders so the
			// synthetic row drops on the next render.
			m.flash = &flashMsg{
				text: fmt.Sprintf(
					"project %s dismissed (no agent records to archive)", project),
			}
			return m, loadAgentsCmd(), true
		}
		cmds := make([]tea.Cmd, 0, len(dead))
		for _, id := range dead {
			cmds = append(cmds, m.startRm(id))
		}
		// Surface the count so the operator sees the fan-out is
		// progressing; per-agent rmDoneMsg banners still arrive
		// individually as each `fleet rm` completes.
		m.flash = &flashMsg{
			text: fmt.Sprintf(
				"dismissing project %s — archiving %d dead agent record(s)",
				project, len(dead)),
		}
		return m, tea.Batch(cmds...), true
	case "esc", "n", "N":
		m.mode = modeNav
		m.dismissProjectCandidate = ""
		m.dismissProjectDeadAgents = nil
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

// handleAddProjectKey processes keystrokes while the [+] add-project
// picker is active. Behavior mirrors handlePickerKey (same nav rules,
// same filter typing) but on enter shells out to
// `fleet project add <path>` instead of advancing to the dispatch
// prompt. The picker stays open on a fleet-side failure so the operator
// can pick a different row without re-pressing [+].
func (m Model) handleAddProjectKey(key string) (Model, tea.Cmd, bool) {
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
		picked := m.repoCandidates[filtered[m.pickerCursor]]
		// Drop to nav immediately. The shell-out runs in a goroutine
		// (returned tea.Cmd); addProjectDoneMsg's failure branch
		// re-opens the picker if the add errored.
		m.mode = modeNav
		m.pickerFilter = ""
		m.repoCandidates = nil
		return m, m.startAddProject(picked.Path), true
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
	return m, nil, true
}

// startAddProject returns a tea.Cmd that runs
// `fleet project add <path>` and emits addProjectDoneMsg on completion.
// path is the absolute path captured from the picker row.
func (m Model) startAddProject(path string) tea.Cmd {
	return runFleetCmd([]string{"project", "add", path}, func(out string, err error) tea.Msg {
		return addProjectDoneMsg{path: path, out: out, err: err}
	})
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
