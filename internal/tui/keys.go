package tui

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

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
//  1. Pre-flight: state.EnsureProjectInitialized creates
//     ~/.fleet/projects/<name>/.locks/ so the spawned coord skill's
//     first tick can publish coord-state.json + acquire the flock
//     without racing on a missing parent directory.
//  2. Shell out to `fleet dispatch coord-<name> --project <name>
//     --cwd <repo> --prompt "Run the /coordinator skill loop for
//     project <name>."`. The task_id is STABLE per project (issue
//     #63): a duplicate [a] press during the skill-boot window
//     finds the in-flight record via findExistingCoordForProject
//     and attaches instead of respawning.
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
	// No worker, active or archived. Codex iter-8 P2 #1: if the
	// task itself is gone (liveTaskStatus empty), surface
	// not-found instead of routing on stale snapshot status —
	// suggesting promote/retry for a deleted slug is the same
	// failure mode the detail-panel path was just hardened against.
	live := liveTaskStatus(project, slug)
	if live == "" && snapshotStatus != "" {
		// snapshot says the task existed but the live read can't
		// find it → archived/deleted between refreshes.
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
	// Path 1: fresh coord exists (lock body + freshness gate). Look up
	// the record by ID; verify the session is alive before committing
	// to attach. A coord whose tmux session died but whose lock body
	// wasn't yet stale would get dispatched to a dead session otherwise
	// — same UX bug as the agent-row branch's "no sessions" failure mode.
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
		// codex review (P1): falling through to the spawn path here
		// would launch a duplicate coord while a fresh one already
		// holds the lock — the loadDashboardCmd / loadAgentsCmd race
		// is normal under tea.Batch. Surface a retry hint so the
		// operator can re-press [a] after the next refresh tick. The
		// task_id fallback below is reachable only when CoordID is
		// empty, so we don't accidentally route this case there.
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

// writeCoordSpawnMarkerFn writes the coord-spawn marker file. var so
// tests can stub the disk write. Production calls
// state.WriteCoordSpawnMarker.
var writeCoordSpawnMarkerFn = state.WriteCoordSpawnMarker

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
	prompt := fmt.Sprintf("Run the /coordinator skill loop for project %s.", projectName)
	// --coord-spawn whitelists the reserved "coord-" task_id prefix at
	// the dispatch CLI (issue #63 codex iter-1 P2). Without it, an
	// operator-supplied `fleet dispatch coord-foo` would create a
	// worker the dashboard treats as the project's coord; with it,
	// only this code path can write the prefix.
	args := []string{"dispatch", taskID, "--project", projectName, "--coord-spawn", "--prompt", prompt}
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
