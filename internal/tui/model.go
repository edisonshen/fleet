// Package tui owns the bubbletea-based interactive dashboard.
//
// v0.2 (issue #53): the dashboard is the only view. It folds projects
// + tasks (left column) and workers + v0.1 agents (right column) into a
// single ops-console layout with a unified cursor. fsnotify drives
// refreshes when files change; a 1s polling tick is the fallback for
// platforms where fsnotify misbehaves (per docs/DESIGN.md).
//
// Keyboard:
//   - j, ↓ / k, ↑: cursor down / up across all rows (wraps)
//   - ← / →     : jump cursor to first row of left (PROJECTS) /
//     right (WORKERS) panel
//   - ⏎ enter   : open detail panel for the row under cursor
//   - n         : add a new task to the current project (in-process call)
//   - a         : attach (agents, tmux) or peek (workers, log + state)
//   - h         : handoff (agents only)
//   - x         : archive (agents) or kill (workers)
//   - d         : dispatch a new agent (opens repo picker)
//   - /         : substring filter across projects/workers/agents
//   - ?         : help overlay
//   - q, ctrl+c : quit
package tui

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/version"
)

// upgradeNudgeFn resolves the current "is there a newer release?"
// banner text. Defaults to version.Nudge (reads ~/.fleet/version_check.json
// + compares against the running binary's version). var so tests can
// stub without faking a cache file.
var upgradeNudgeFn = version.Nudge

// upgradeStartFn kicks off the async background fetch + cache
// refresh. var so tests can stub the network call out entirely.
var upgradeStartFn = version.Start

// Model is the bubbletea state for the dashboard.
type Model struct {
	records []*agent.Record
	err     error
	width   int
	height  int

	// dashboard is the most recent v0.2 ops-console snapshot. Loaded
	// asynchronously by loadDashboardCmd; nil until the first load
	// lands. Render path is nil-safe so the first frame just shows
	// empty columns.
	dashboard *Snapshot

	// startedAt is captured at New() time for the dashboard footer's
	// "uptime HH:MM" indicator. Tests inject via Model.startedAt
	// directly; a zero value means "show 00:00" rather than crash.
	startedAt time.Time

	// version is shown in the title bar. Caller injects from
	// cmd/fleet/main.go's ldflags-overridable Version.
	version string

	// userName is rendered right-aligned in the title row. Captured
	// once at New() time — it doesn't change mid-session, and looking
	// it up per-render would be a syscall on every tick.
	userName string

	// Action keybind state (Week 4b+4c, picker added Week 5).
	mode      inputMode // modeNav, modePickRepo, or modePromptDispatch
	promptBuf string    // dispatch prompt buffer; empty in modeNav
	flash     *flashMsg // banner shown under the table after an action

	// Repo picker state (modePickRepo). Populated when the operator
	// presses [d]; cleared on esc or after enter advances to
	// modePromptDispatch with pickedRepo locked in.
	repoCandidates []repoCandidate
	pickerFilter   string // case-insensitive substring filter
	pickerCursor   int    // index into the FILTERED slice
	pickedRepo     repoCandidate

	// archiveCandidate is the agent ID the operator pressed [x] on,
	// awaiting y/esc confirmation in modeConfirmArchive. Cleared when
	// the prompt resolves either way.
	archiveCandidate string

	// dismissProjectCandidate is the project name the operator pressed
	// [x] on for the legacy-row dismiss path (issue #96 gap 3). Cleared
	// when modeConfirmDismissProject resolves. Paired with the dead-
	// agent-IDs slice below so the confirm handler dispatches `fleet rm`
	// against the SAME records that were on disk at press time — without
	// this freeze, a refresh between press and confirm could mutate
	// m.records out from under the operator and we'd archive different
	// (or zero) records than the prompt advertised.
	dismissProjectCandidate string
	// dismissProjectDeadAgents is the snapshot of dead-agent IDs tagged
	// with dismissProjectCandidate at the moment the operator pressed
	// [x]. Empty slice on any non-dismiss flow.
	dismissProjectDeadAgents []string

	// resetProjectCandidate is the project name the operator pressed [r]
	// on, awaiting y/esc in modeConfirmReset (dead-end-recovery-r-8559).
	// Cleared when the prompt resolves either way.
	resetProjectCandidate string
	// resetCoordIDs is the snapshot of ALL coord agent-record IDs tagged
	// with resetProjectCandidate at press time — every record whose
	// task_id == coord-<project> AND project == <project>. Frozen here
	// (like dismissProjectDeadAgents) so the confirm handler reaps the
	// SAME records the prompt counted, immune to a refresh between press
	// and confirm. Empty slice on any non-reset flow.
	resetCoordIDs []string

	// aliveByID is the cached tmux liveness snapshot from the most
	// recent agentsMsg. Populated off the render path by
	// loadAgentsCmd; deriveStatus reads from it. Nil/empty means no
	// load has completed yet — deriveStatus treats that as "no
	// evidence of dead", not "definitely dead".
	aliveByID map[string]bool

	// groupKeysByID is the cached projectGroupKey result per record,
	// populated by loadAgentsCmd so renderAgents / footerSummary /
	// sortRecords don't pay the EvalSymlinks stat syscall on every
	// cursor move. Missing key falls back to live computation.
	groupKeysByID map[string]string

	// pendingAttach is set when [a] fires. tea.Quit returns control to
	// tui.Run, which exec's `tmux attach -t <session>` after the
	// program exits. Process replacement only works post-program — a
	// regular tea.Cmd would be inside bubbletea's altscreen and tmux
	// would draw on top of bubbletea's state.
	pendingAttach string

	// coordSpawnInFlight tracks projects whose [a] dispatch goroutine
	// has been launched but coordSpawnDoneMsg hasn't returned yet.
	// During this window the marker file isn't written and the agent
	// record isn't on disk — without this gate, a second [a] press
	// would launch a second `fleet dispatch coord-<project>` and we'd
	// have two competing coord agents (issue #63 codex iter-3 P2
	// follow-up). Map key is project name; value is irrelevant
	// (presence == in-flight). Cleared when coordSpawnDoneMsg arrives
	// or when the err branch fires.
	coordSpawnInFlight map[string]bool

	// projectAddCoordSpawn marks the [+]-hotkey-initiated coord spawns
	// so the coordSpawnDoneMsg handler can format a recovery-oriented
	// failure flash. When a project is added via [+] and the follow-on
	// coord auto-spawn fails (init error, dispatch failure, parse
	// failure), the operator needs to know that the add itself
	// succeeded and the new row exists on the dashboard — pressing
	// [a] on that row retries the spawn. Without this flag the generic
	// "project <name>: <err>" banner is ambiguous about which path
	// failed.
	//
	// Map key is project name; value is irrelevant (presence == "this
	// spawn was kicked off from [+]"). Cleared when coordSpawnDoneMsg
	// arrives regardless of success/error.
	projectAddCoordSpawn map[string]bool

	// upgradeBanner is the rendered "⬆ vX.Y.Z — brew upgrade fleet"
	// chip when a newer release is on disk in the version cache.
	// Empty means no banner — every failure mode in the version
	// package collapses to "" so this field reads as the single
	// authoritative "is there a nudge?" signal. Populated via
	// upgradeAvailableMsg fired from a goroutine launched in Init.
	upgradeBanner string

	// dashCursor is the index into dashboardRows() for the
	// currently-selected row. [j/k] move it; [⏎]/[a]/[h]/[x]/[n]
	// dispatch on the row at this index. Wraps at boundaries.
	dashCursor int

	// workersScrollOffset / agentsScrollOffset are the vertical scroll
	// positions for the right-column workers + agents panels (fleet#177
	// Fix 2). renderTwoColumnBody caps each panel at a computed maxRows
	// budget; lines beyond the budget are hidden and surfaced via a
	// "K hidden — [↓/↑] scroll" footer. ↓/↑ on a right-column row
	// adjusts the matching offset (clamped to [0, len(lines)-visible]).
	// Reset to 0 on tea.WindowSizeMsg — the visible window changes and
	// re-clamping after-the-fact is brittle. Left-column j/k/arrow nav
	// is untouched.
	workersScrollOffset int
	agentsScrollOffset  int

	// projectsScrollOffset is the vertical scroll position for the LEFT
	// PROJECTS column (tui-project-list-truncat). renderTwoColumnBody caps
	// the left column at leftRows; before this fix, lines beyond leftRows
	// were SILENTLY dropped — the operator's 2026-05-29 screenshot showed
	// "8 projects" in the header but only ~5 project groups rendered, with
	// a [+]-added project ("spark") never visible (violates
	// surface-dont-silo). Now the left column reuses trimWithScroll like
	// the #177 right panels: overflow surfaces a "K hidden — [↓/↑] scroll"
	// footer and ↓/↑ on a left-column row pages through. Reset to 0 on
	// tea.WindowSizeMsg (visible window changes; re-clamping after the fact
	// is brittle).
	projectsScrollOffset int

	// searchFilter is the current substring filter applied to
	// dashboardRows(). Empty when no filter is active. Set via [/]
	// search prompt; cleared via [esc] inside the prompt.
	searchFilter string

	// showHelp toggles the help overlay (set by [?], cleared by any
	// other key).
	showHelp bool

	// detail, when non-nil, drives the [⏎] open detail panel. The
	// kind tells the renderer what to show; payload is the row-type-
	// specific text body.
	detail *detailView

	// taskAddProjectFrozen captures the target project at the moment
	// [n] is pressed. Without this, the 1s poll could re-sort
	// dashboardRows() while the prompt is open and the same dashCursor
	// would land on a different project at submit time — operator's
	// new task lands in the wrong tasks.md (codex iter-2 P1). Cleared
	// when modePromptTaskAdd exits.
	taskAddProjectFrozen string

	// expanded tracks which project rows are showing their inline
	// task list. Keyed by project name (matches ProjectRow.Name and
	// dashboardRows()'s rowProject identity). [⏎] on a project row
	// flips the bool; tasks render under the project header iff
	// the bool is true OR the active search filter matches one of
	// that project's tasks (so search keeps matching tasks visible
	// regardless of the operator's expansion choice). Persists across
	// dashboardMsg ticks — the map is never reset on refresh, so the
	// 1s poll doesn't auto-collapse the operator's expansion.
	expanded map[string]bool

	// tickCount is the rolling count of tickMsg events received since
	// program start. Used as the spinner-frame index for the coord-
	// spawn indicator (issue #86) so its glyph rotates once per
	// pollInterval. Wraps modulo len(coordSpawnGlyphs) at render time.
	tickCount int

	// coordSpawnTimeout is the age past which a coord-spawn marker is
	// declared "stuck" and the project row flips from the spawning
	// spinner to the red warning. Resolved once at New() from the
	// FLEET_COORD_SPAWN_TIMEOUT_S env var, defaulting to
	// coordSpawnTimeoutDefault (10 minutes). Cached here so the env
	// isn't re-parsed on every render.
	coordSpawnTimeout time.Duration

	// activeWindow is the threshold past which an agent/worker signal
	// no longer counts toward "ACTIVE" classification (issue #98).
	// Resolved once at New() from FLEET_ACTIVE_WINDOW_DAYS env (default
	// 7 days). Cached so the env isn't re-parsed on every render.
	activeWindow time.Duration

	// showHidden, when true, renders the hidden-projects group ("─── N
	// hidden ───") in the LEFT column with the hidden rows visible
	// (dim) when expanded (issue #98). Toggled via [c] off-row (cursor
	// on a separator or no row selected). Default false — operator
	// never sees the hidden group unless they ask for it.
	showHidden bool

	// idleExpanded, when true, expands the "─── N idle ───" group so
	// each idle project's row renders below the separator. Toggled via
	// [enter] on the idle separator. Default false — idle projects
	// stay collapsed so the dashboard prioritizes ACTIVE work (issue
	// #98).
	//
	// See dashboardRows for the default-expand fallback when zero
	// projects are ACTIVE: the spec's "─── N idle ───" wall isn't the
	// right first impression for an operator with one stale-but-still-
	// relevant project. idleCollapseExplicit (below) tracks whether
	// the operator pressed [enter] on the separator, so the auto-
	// expand only kicks in until the operator chooses.
	idleExpanded bool

	// idleCollapseExplicit tracks whether the operator has explicitly
	// pressed [enter] on the idle separator. Once true, the
	// auto-expand-when-no-active default no longer overrides
	// idleExpanded. Issue #98 ergonomic refinement.
	idleCollapseExplicit bool

	// hiddenExpanded, when true, expands the "─── N hidden ───" group
	// (only visible when showHidden=true) so each hidden project's row
	// renders below the separator. Toggled via [enter] on the hidden
	// separator. Default false (issue #98).
	hiddenExpanded bool

	// historyExpanded tracks per-project whether the `─── N done ───`
	// task-history separator inside the project's expansion is open
	// (issue #101). Keyed by project name. Default closed; the active
	// task list still renders above the separator regardless. Toggled
	// via [enter] on the history separator row. Persists across
	// dashboardMsg ticks so the 1s poll doesn't auto-collapse the
	// operator's choice (mirrors the `expanded` map's persistence).
	historyExpanded map[string]bool

	// agentIdleExpanded, when true, expands the right-column
	// "─── N idle ───" group so each idle v0.1 agent record renders
	// below the separator. Toggled via [enter] on the agent-idle
	// separator. Default false — idle agents stay collapsed so the
	// right column prioritizes asking/blocked agents + fresh records
	// (dashboard-accumulation-f-4421 Sub-fix B). Mirrors idleExpanded's
	// shape but is a separate field so toggling the LEFT and RIGHT
	// idle groups stays independent.
	agentIdleExpanded bool
}

// detailView is the inline detail panel shown by [⏎] open. The kind
// hints at the source row type (only used for the panel title); body
// is the pre-rendered multi-line text the panel displays.
//
// Issue #75: when the panel opened from a task row, we carry the
// task's project + slug + status so [a] inside the panel can route
// to the matching worker peek (or the right pre-dispatch hint)
// without rebuilding the row index. taskProject is "" for non-task
// panels, which the [a] interceptor reads as "fall through to
// default attach behavior". Status preserves the row's pre-dispatch
// state (todo/ready) so the panel-side [a] gives the same dispatch
// hint as the row-side [a] (codex iter-2 P2).
type detailView struct {
	title       string
	body        string
	taskProject string
	taskSlug    string
	taskStatus  string
}

// PendingAttach returns the tmux session to attach to after the
// program exits, or "" if no [a] was pressed. tui.Run reads this.
func (m Model) PendingAttach() string { return m.pendingAttach }

// New returns a Model ready to be passed to tea.NewProgram.
func New(version string) Model {
	return Model{
		version:           version,
		userName:          currentUserName(),
		startedAt:         time.Now(),
		coordSpawnTimeout: resolveCoordSpawnTimeout(),
		activeWindow:      resolveActiveWindow(),
	}
}

// currentUserName resolves the operator's username for the title row.
// Prefers os/user (works for unprivileged + chrooted environments),
// falls back to $USER, then "" if both fail. Empty just hides the
// right-aligned label — never blocks the dashboard from rendering.
func currentUserName() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return os.Getenv("USER")
}

// Init is the bubbletea entry point. We kick off the first agent load,
// start the 1s polling tick, and run the upgrade-check probe. fsnotify
// is wired in tui.go's Run.
//
// versionCheckCmd both kicks off the async network fetch (background
// goroutine, never blocks) AND reads the existing on-disk cache so
// the banner can render on startup if a previous run already learned
// about a newer release. Both happen async to keep render
// non-blocking — see the cmd's docstring.
func (m Model) Init() tea.Cmd {
	return tea.Batch(loadAgentsCmd(), loadDashboardCmd(), tickCmd(), versionCheckCmd(m.version))
}

// agentsMsg carries a refreshed list of agent records (or an error)
// from the loader goroutine.
//
// alive snapshots tmux session liveness for each loaded record at
// load time. Probing once per refresh — instead of once per row per
// View() repaint — avoids fanning out to N `tmux has-session`
// subprocesses on every cursor move or 1s tick. deriveStatus reads
// from this cache.
//
// groupKeys snapshots the resolved project group key per record at
// load time. projectGroupKey calls filepath.EvalSymlinks (a stat
// syscall) when r.Cwd is set; running that per-row per-View() means
// dozens of syscalls per cursor move on slow / NFS mounts. The
// loader resolves once and the renderer reads from this map (codex
// iter-5 P2).
type agentsMsg struct {
	records   []*agent.Record
	err       error
	alive     map[string]bool
	groupKeys map[string]string
}

// tickMsg fires every pollInterval so we can re-read agents/ even when
// fsnotify is silent. Important for the platforms where fsnotify
// misbehaves (NFS, certain editor save patterns, etc.).
type tickMsg time.Time

// fsEventMsg is emitted by the fsnotify watcher when ~/.fleet/agents/
// changes. The receiver kicks off a fresh agent load.
type fsEventMsg struct{}

// queueEventMsg is emitted by the fsnotify watcher when
// ~/.fleet/queue/ changes (a producer wrote a spawn-fresh-*.json).
// The receiver shells out to `fleet drain` so the auto-handoff
// completes without operator intervention.
type queueEventMsg struct{}

// upgradeAvailableMsg carries the rendered nudge text when a newer
// release is on disk. Empty text leaves m.upgradeBanner cleared
// (no banner) — every failure mode in the version package collapses
// to "", and Update treats "" as "no signal".
type upgradeAvailableMsg struct {
	text string
}

const pollInterval = 1 * time.Second

func tickCmd() tea.Cmd {
	return tea.Tick(pollInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// versionCheckCmd kicks off the background upgrade probe AND reads the
// existing on-disk cache. Two-step pattern:
//
//  1. version.Start spawns a goroutine that hits the GitHub API and
//     writes the cache. Returns immediately — never blocks startup.
//  2. version.Nudge reads whatever cache is already on disk (from a
//     PREVIOUS run) so the banner can render on the very first frame
//     without waiting for the network.
//
// If step 1's fetch eventually populates a fresh cache, step 2 won't
// re-read it — the operator sees the new banner on next launch. That's
// fine: the spec only requires "at most once per day per process",
// and we'd rather be conservative than poll the cache mid-session.
//
// Every failure inside version.Nudge collapses to "" — no errors
// propagate, no logging, nothing on stderr. Banner just doesn't
// render.
func versionCheckCmd(current string) tea.Cmd {
	return func() tea.Msg {
		upgradeStartFn(current)
		return upgradeAvailableMsg{text: upgradeNudgeFn(current)}
	}
}

// loadAgentsCmd reads ~/.fleet/agents/*.json once and returns the
// result as an agentsMsg. Probes tmux liveness for each loaded
// record here (off the render path) so deriveStatus can read from
// the cached map instead of forking `tmux has-session` per row on
// every repaint.
//
// Uses the tristate sessionProbeFn so transport failures (tmux
// missing, broken socket) DON'T poison the cache with false "dead"
// readings — those records are simply omitted from the alive map,
// and deriveStatus's nil-safe fallback renders them as "live"
// rather than mislabeling a healthy agent (codex review iter-5 P2).
func loadAgentsCmd() tea.Cmd {
	return func() tea.Msg {
		records, err := agent.List()
		alive := make(map[string]bool, len(records))
		groupKeys := make(map[string]string, len(records))
		for _, r := range records {
			if r.TmuxSession != "" {
				if ok, probeErr := sessionProbeFn(r.TmuxSession); probeErr == nil {
					alive[r.ID] = ok
				}
				// Transport failure → leave entry missing so the
				// dashboard reads "live" instead of mislabeling.
			}
			groupKeys[r.ID] = projectGroupKey(r)
		}
		return agentsMsg{records: records, err: err, alive: alive, groupKeys: groupKeys}
	}
}

// Update handles every tea.Msg the program receives.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		// fleet#177 Fix 2: a resize changes the visible-row budget for
		// each right-column panel. Re-clamping the offsets after the
		// fact is brittle (the buildBodyLines pass hasn't run yet), so
		// reset both offsets to 0 — the operator scrolls back if they
		// were mid-scroll.
		m.workersScrollOffset = 0
		m.agentsScrollOffset = 0
		m.projectsScrollOffset = 0
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case agentsMsg:
		// Capture the cursor's row identity BEFORE swapping records
		// in. After refresh, refreshCursor relocates the cursor onto
		// the same identity (or clamps to start if it disappeared) so
		// background updates don't silently retarget [⏎]/[a]/[n]/[x]
		// (codex iter-5 P1).
		var prevID string
		if row := m.selectedRow(); row != nil {
			prevID = rowIdentity(*row)
		}
		m.err = msg.err
		m.aliveByID = msg.alive
		m.groupKeysByID = msg.groupKeys
		m.records = sortRecordsBy(msg.records, msg.groupKeys)
		m.refreshCursor(prevID)
		return m, nil

	case tickMsg:
		// Polling fallback: re-read agents/ AND the v0.2 dashboard
		// snapshot every pollInterval, then schedule the next tick.
		// Bump tickCount so the coord-spawn spinner advances one frame
		// per pollInterval (issue #86). Wraps naturally at render time
		// via coordSpawnSpinnerGlyph's modulo arithmetic — no need to
		// reset here.
		m.tickCount++
		return m, tea.Batch(loadAgentsCmd(), loadDashboardCmd(), tickCmd())

	case fsEventMsg:
		// fsnotify saw a change — refresh agents AND the dashboard
		// snapshot now (don't wait for the next tick). Dashboard scan
		// is cheap (a handful of stat + small JSON reads) so we don't
		// gate on which subtree fired the event.
		return m, tea.Batch(loadAgentsCmd(), loadDashboardCmd())

	case dashboardMsg:
		// Capture cursor identity BEFORE swapping the snapshot, then
		// re-locate via refreshCursor (codex iter-5 P1). This handles
		// the row-shift case (project resort by Attention; new tasks
		// inserted ahead of workers/agents) which was silently
		// retargeting [⏎]/[a]/[n]/[x] under iter-1's clamp-only fix.
		var prevID string
		if row := m.selectedRow(); row != nil {
			prevID = rowIdentity(*row)
		}
		m.dashboard = msg.snap
		// Surface scan errors as a flash so an unreadable
		// ~/.fleet/projects/ doesn't silently render as "0 projects"
		// (codex iter-12 P2). Best-effort: per-project errors inside
		// scanDashboard collapse to empty rows; only top-level
		// failures (Snapshot.Err non-nil) need this banner.
		if msg.snap != nil && msg.snap.Err != nil {
			m.flash = &flashMsg{
				text:  fmt.Sprintf("dashboard scan failed: %v", msg.snap.Err),
				isErr: true,
			}
		}
		m.refreshCursor(prevID)
		return m, nil

	case queueEventMsg:
		// fsnotify saw a queue file land — auto-drain. Drain itself is
		// idempotent and per-agent flocked, so multiple events firing
		// in quick succession (e.g. atomic-write rename) are safe.
		return m, runFleetCmd([]string{"drain"}, func(out string, err error) tea.Msg {
			return drainDoneMsg{out: out, err: err}
		})

	case drainDoneMsg:
		// Surface drain output as a flash if there's anything
		// interesting (errors or non-empty output). Empty success runs
		// are silent — we don't want every queue event to spam the
		// banner.
		if msg.err != nil {
			fl := flashMsg{
				text:  fmt.Sprintf("drain failed: %v\n%s", msg.err, msg.out),
				isErr: true,
			}
			m.flash = &fl
		}
		return m, loadAgentsCmd()

	case handoffDoneMsg:
		fl := formatHandoffFlash(msg.out, msg.err)
		m.flash = &fl
		return m, loadAgentsCmd() // refresh: agent should be archived

	case dispatchDoneMsg:
		fl := formatDispatchFlash(msg.out, msg.err)
		m.flash = &fl
		return m, loadAgentsCmd() // refresh: new agent should appear

	case rmDoneMsg:
		fl := formatRmFlash(msg.out, msg.err)
		m.flash = &fl
		return m, loadAgentsCmd() // refresh: agent should be archived

	case resetDoneMsg:
		// [r] reset reap finished (dead-end-recovery-r-8559). On reap
		// failure: surface the error, clear the in-flight gate, and do
		// NOT respawn — a half-reaped project must not get a new coord
		// racing the leftovers. The operator can re-press [r] once the
		// underlying issue (e.g. a `fleet rm` that hit a busy lock) is
		// resolved. On success: spawn ONE fresh coord and flash the
		// reaped count.
		if msg.err != nil {
			if m.coordSpawnInFlight != nil {
				delete(m.coordSpawnInFlight, msg.projectName)
			}
			m.flash = &flashMsg{
				text: fmt.Sprintf(
					"reset of project %s failed after reaping %d coord record(s): %v\n%s — re-press [r] to retry",
					msg.projectName, msg.reaped, msg.err, strings.TrimRight(msg.out, "\n")),
				isErr: true,
			}
			return m, loadAgentsCmd()
		}
		// Reap succeeded — sessions/records gone, lock cleared. Spawn the
		// replacement. EnsureProjectInitialized recreates the .locks/
		// tree the gc sweep may have left bare so the fresh coord's
		// first tick can re-acquire the flock. The coordSpawnInFlight
		// gate was set at confirm time; startCoordSpawn's coordSpawnDone
		// Msg clears it.
		if _, ierr := state.EnsureProjectInitialized(msg.projectName); ierr != nil {
			if m.coordSpawnInFlight != nil {
				delete(m.coordSpawnInFlight, msg.projectName)
			}
			m.flash = &flashMsg{
				text: fmt.Sprintf(
					"reset of project %s reaped %d coord record(s) + cleared lock, but respawn init failed: %v — press [a] on the row to retry",
					msg.projectName, msg.reaped, ierr),
				isErr: true,
			}
			return m, loadAgentsCmd()
		}
		cwd := coordCwdForProject(m.records, msg.projectName)
		m.flash = &flashMsg{
			text: fmt.Sprintf(
				"reset project %s — reaped %d coord record(s), cleared stale lock, spawning fresh coord",
				msg.projectName, msg.reaped),
		}
		return m, tea.Batch(loadAgentsCmd(), m.startCoordSpawn(msg.projectName, cwd))

	case addProjectDoneMsg:
		// On error: re-open the picker (operator picks a different row)
		// and surface the underlying CLI message verbatim. Picker state
		// (candidates, filter, cursor) was cleared on enter; rebuild it
		// here so the operator sees the same candidate list they just
		// picked from. dashboardLoadCmd is NOT triggered on failure —
		// the on-disk state is unchanged.
		if msg.err != nil {
			m.flash = &flashMsg{
				text:  fmt.Sprintf("project add failed (%s): %v\n%s", msg.path, msg.err, msg.out),
				isErr: true,
			}
			m.repoCandidates = discoverRepos()
			m.pickerFilter = ""
			m.pickerCursor = 0
			m.mode = modeAddProject
			return m, nil
		}
		// Success: project is on disk. Surface the CLI's stdout (which
		// already says "added project <tag>" or "refreshed project <tag>")
		// and refresh the dashboard so the new row appears.
		// Also force-clear picker state — the enter handler already
		// dropped to modeNav, but explicitly resetting here makes the
		// success path robust against any edge case where the message
		// arrives while the picker is still open.
		text := strings.TrimRight(msg.out, "\n")
		if text == "" {
			text = "added project"
		}
		m.flash = &flashMsg{text: text}
		m.mode = modeNav
		m.pickerFilter = ""
		m.repoCandidates = nil
		m.pickerCursor = 0
		// Mirror the [a]-on-project-row coord-spawn path so the freshly
		// registered project gets a coord auto-spawned without a second
		// keystroke. Pre-flight failures (project tree init, in-flight
		// guard) collapse to a flash naming the [a]-row recovery; the
		// add itself succeeded so the project IS on disk and the
		// dashboard refresh will surface its row.
		//
		// ASCII flow:
		//
		//   addProjectDoneMsg(success)
		//     → projects.TagForPath(path)
		//     → state.EnsureProjectInitialized(tag)
		//     → mark coordSpawnInFlight[tag] + projectAddCoordSpawn[tag]
		//     → tea.Batch(loadDashboardCmd, startCoordSpawn(tag, path))
		//     → coordSpawnDoneMsg lands → existing handler sets
		//       pendingAttach + tea.Quit (success) OR flashes the
		//       "[+]-initiated spawn failed; [a] on the new row to
		//       retry" hint (err branch, gated on projectAddCoordSpawn).
		tag := ProjectTag(msg.path)
		if _, ierr := state.EnsureProjectInitialized(tag); ierr != nil {
			m.flash = &flashMsg{
				text: fmt.Sprintf(
					"added project %s but coord auto-spawn init failed: %v — press [a] on the new row to retry",
					tag, ierr),
				isErr: true,
			}
			return m, loadDashboardCmd()
		}
		if m.coordSpawnInFlight != nil && m.coordSpawnInFlight[tag] {
			// Rare: a coord spawn for the same tag is already in flight
			// (e.g. operator pressed [a] on a synthetic row before [+]
			// completed). Skip the second dispatch; the existing spawn
			// will surface via its own coordSpawnDoneMsg.
			return m, loadDashboardCmd()
		}
		if m.coordSpawnInFlight == nil {
			m.coordSpawnInFlight = map[string]bool{}
		}
		if m.projectAddCoordSpawn == nil {
			m.projectAddCoordSpawn = map[string]bool{}
		}
		m.coordSpawnInFlight[tag] = true
		m.projectAddCoordSpawn[tag] = true
		return m, tea.Batch(loadDashboardCmd(), m.startCoordSpawn(tag, msg.path))

	case coordSpawnDoneMsg:
		// Clear in-flight gate — regardless of success/error, this
		// dispatch attempt is done. Subsequent [a] presses go through
		// the full lookup path again (find existing → spawn).
		if m.coordSpawnInFlight != nil {
			delete(m.coordSpawnInFlight, msg.projectName)
		}
		// Capture (and clear) the [+]-initiated flag so the err branch
		// below can format the project-added-but-spawn-failed recovery
		// hint. Either branch resets the flag — the operator's next
		// [a] on that row is the normal retry path, not a [+]-initiated
		// follow-up.
		wasAddSpawn := m.projectAddCoordSpawn[msg.projectName]
		if m.projectAddCoordSpawn != nil {
			delete(m.projectAddCoordSpawn, msg.projectName)
		}
		// Issues #60, #63: project-row [a] auto-spawn result. err non-nil
		// covers init failures, dispatch failures, and agent-ID parse
		// failures; surface as a flash so the operator can decide
		// whether to retry. On success, probe the session liveness
		// before committing pendingAttach — a dispatch can return 0 +
		// agent-ID line while claude exits seconds later (binary not
		// found, --dangerously-skip-permissions denied, OOM). Without
		// the probe the TUI quits and runs `tmux attach` against a
		// dead session, regressing to the raw "no sessions" UX (codex
		// review iter-1 P2). Trigger a refresh so the new (possibly
		// dead) agent record appears on the right column for the
		// operator to investigate via [x] or [a].
		//
		// Codex iter-3 P2: write the coord-spawn marker so the
		// dashboard's task_id fallback can validate the agent ID
		// matches our intent. Without this, an operator shelling out
		// `fleet dispatch coord-X --project X --coord-spawn` could
		// hijack the LEFT-column slot. The marker is best-effort: a
		// write failure is logged in the flash but doesn't abort the
		// attach (the agent is up; worst case the dashboard renders
		// the coord on RIGHT until the lock body publishes).
		if msg.err != nil {
			// Spec: a [+]-initiated spawn failure must surface the
			// "project is registered — [a] on the new row to retry"
			// recovery path. The add itself succeeded (we only reach
			// the spawn after addProjectDoneMsg.err == nil) so the
			// dashboard already carries the new row; the operator just
			// needs to know that pressing [a] on it will retry.
			var text string
			if wasAddSpawn {
				text = fmt.Sprintf(
					"project %s added but coord auto-spawn failed: %v — press [a] on the new row to retry",
					msg.projectName, msg.err)
			} else {
				text = fmt.Sprintf("project %s: %v", msg.projectName, msg.err)
			}
			m.flash = &flashMsg{
				text:  text,
				isErr: true,
			}
			return m, loadAgentsCmd()
		}
		// Codex iter-6 P2: probe with the tristate primitive.
		// sessionAliveFn is tmux.HasSession, which conflates "session
		// is gone" with transport errors (bad FLEET_TMUX_SOCKET,
		// restarting tmux server). Treating those as dead would skip
		// a perfectly good attach. sessionProbeOrAliveFn returns true
		// on probe error so we err toward attempting attach — the
		// operator gets tmux's own clear error if it fails.
		if msg.session != "" && !sessionProbeOrAliveFn(msg.session) {
			m.flash = &flashMsg{
				text: fmt.Sprintf(
					"coord %s spawned for project %s but session %s is not alive — claude likely exited at startup; check the agent record (right column) and re-press [a] to respawn after archiving",
					msg.agentID, msg.projectName, msg.session),
				isErr: true,
			}
			return m, loadAgentsCmd()
		}
		// Codex iter-5 P2: only write the coord-spawn marker when the
		// dispatch actually delivered the /coordinator prompt to the
		// pane. A prompt-delivery failure leaves a plain Claude
		// session running with no coord skill — promoting it via the
		// marker would render a healthy in-flight coord in the
		// dashboard while the project is actually unowned. We still
		// attach the operator to the session (they can type the
		// prompt manually), but the dashboard's task_id fallback
		// stays inactive until the operator re-presses [a] from a
		// proper boot.
		switch {
		case !msg.promptDelivered:
			m.flash = &flashMsg{
				text: fmt.Sprintf(
					"coord %s spawned for project %s but the /coordinator prompt failed to deliver — attaching so you can type it manually; project will not show as coord-bound until next [a]",
					msg.agentID, msg.projectName),
				isErr: true,
			}
		default:
			if werr := writeCoordSpawnMarkerFn(msg.projectName, msg.agentID); werr != nil {
				m.flash = &flashMsg{
					text: fmt.Sprintf(
						"coord %s spawned for project %s (marker write failed: %v) — attaching to %s",
						msg.agentID, msg.projectName, werr, msg.session),
				}
			} else {
				m.flash = &flashMsg{
					text: fmt.Sprintf(
						"coord %s spawned for project %s — attaching to %s",
						msg.agentID, msg.projectName, msg.session),
				}
			}
		}
		m.pendingAttach = msg.session
		return m, tea.Quit

	case taskAddDoneMsg:
		if msg.err != nil {
			m.flash = &flashMsg{
				text:  fmt.Sprintf("task add failed: %v", msg.err),
				isErr: true,
			}
		} else {
			m.flash = &flashMsg{text: fmt.Sprintf("added %s", msg.slug)}
		}
		// Refresh dashboard so the new task surfaces in the next render.
		return m, loadDashboardCmd()

	case upgradeAvailableMsg:
		// Banner is set / cleared atomically here. Subsequent renders
		// pick it up via m.upgradeBanner; no other path mutates it.
		m.upgradeBanner = msg.text
		return m, nil
	}
	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()

	// ctrl+c always quits, even mid-prompt — escape hatch for the
	// operator who pressed [d] by accident.
	if key == "ctrl+c" {
		return m, tea.Quit
	}

	// Action keys (handoff/attach/dispatch) and prompt-mode keys take
	// precedence over navigation. handleActionKey returns handled=false
	// only when in nav mode and the key isn't an action key, in which
	// case we fall through to navigation below.
	updated, cmd, handled := m.handleActionKey(key)
	if handled {
		return updated, cmd
	}

	switch key {
	case "q":
		return m, tea.Quit
	case "j":
		// j is row-nav across the unified row list. NEVER scrolls a
		// right panel — the operator who wants vim-style nav keeps
		// using j/k everywhere. But when j lands the cursor on a
		// left-column row that the projects-scroll trim has hidden, the
		// offset must follow so [⏎]/[a] don't target an off-screen
		// project (codex review [P2] — j/k bypassed the arrow-key
		// alignment in scrollLeftPanel).
		m.moveCursor(+1)
		m.alignLeftScrollToCursor()
	case "k":
		m.moveCursor(-1)
		m.alignLeftScrollToCursor()
	case "down":
		// fleet#177 Fix 2: arrow keys scroll the focused right panel.
		// Cursor on a worker row → workersScrollOffset++; cursor on an
		// agent or right-column separator row → agentsScrollOffset++.
		// Cursor on a left-column row → scroll the LEFT projects panel
		// (tui-project-list-truncat). When neither panel consumes the key
		// (e.g. nothing to scroll), fall through to row-nav so j/k-style
		// wrapping still works.
		if !m.scrollRightPanel(+1) && !m.scrollLeftPanel(+1) {
			// Fallback row-nav (e.g. wrap from the last agent back to the
			// first project). Align the left scroll if we landed on a
			// left-column row — otherwise a wrap onto a project hidden
			// behind a nonzero offset would be off-screen (codex [P2]).
			m.moveCursor(+1)
			m.alignLeftScrollToCursor()
		}
	case "up":
		if !m.scrollRightPanel(-1) && !m.scrollLeftPanel(-1) {
			m.moveCursor(-1)
			m.alignLeftScrollToCursor()
		}
	case "left":
		m.jumpToLeftPanel()
	case "right":
		m.jumpToRightPanel()
	}
	return m, nil
}

// scrollRightPanel adjusts the workers or agents scroll offset when the
// cursor is on a right-column row. Returns true when a scroll was
// applied (so the caller skips row-nav); false when the cursor is on
// a LEFT-column row and the arrow should fall through to moveCursor.
//
// fleet#177 Fix 2 right-panel bound — see Model.workersScrollOffset
// doc. Clamping happens in the renderer (trimWithScroll) — we
// optimistically advance here; an over-scroll past the end snaps
// back to the visible window on the next render.
//
// Cursor co-movement (codex iter-2 [P2] follow-up): scroll also
// advances the cursor by the same delta within the unified row list,
// clamped to the panel's row span. Without this, repeated ↓ keeps
// the cursor pinned at the first visible row while content scrolls
// past, so [⏎] / [a] / [r] / [c] target a row that's no longer
// visible — the operator acts on a worker they can't see. Co-moving
// the cursor keeps the selected marker on a visible row and actions
// stay coherent with the screen.
// Boundary fall-through (tui-nav-handoff-regressions Fix 1):
// moveCursorWithinKind stops at the worker↔agent sub-panel edge
// (isOtherPanel guard) rather than crossing. Net effect under #177:
// ↓/↑ got TRAPPED inside each sub-panel — the operator on the last
// worker pressed ↓ and nothing happened. The fix: detect the
// "cursor pinned AND the adjacent row is the OTHER right-column panel"
// case and return false so handleKey falls through to moveCursor,
// which walks the unified row list and crosses the worker↔agent
// boundary naturally (the same path j/k already use).
//
// Scoped to the worker↔agent boundary only. We deliberately do NOT
// fall through at the OUTER edges of the right column:
//
//	↑ on the FIRST worker  → neighbor above is a LEFT-column project /
//	  task row, NOT the other right-column panel → stay (return true).
//	  Arrows must not silently jump out of the right column into the
//	  left column (#177's TestArrowKeys_ScrollCursorStaysInPanel guards
//	  this; ← is the explicit left-jump key).
//	↓ on the LAST agent    → no neighbor below (list end) → fall through
//	  to moveCursor, which wraps to the top per existing behavior; see
//	  fallsThroughAtEdge's no-neighbor branch.
//
// Why cursor progress, not scroll progress, gates "pinned":
// panelMaxOffset is a count-based bound (3 × rowCount), so even a
// NON-overflowing panel reports a positive max offset and
// clampScrollOffset bumps the stored offset by delta. That phantom
// bump means "scroll offset changed" is true at every boundary, so it
// can't distinguish "still has content to scroll" from "at the edge".
// Cursor movement is the honest signal: moveCursorWithinKind advances
// the cursor iff another row of the same kind exists in the delta
// direction. When it can't, we consult the adjacent row to decide
// cross vs. stay, restoring the phantom offset bump on cross.
//
//	last worker, no more workers below, agent below → restore offset,
//	  return false → moveCursor advances to first agent. ✓
//	first agent, last worker above                  → restore offset,
//	  return false → moveCursor steps back to last worker. ✓
//	mid-panel, more rows of this kind ahead         → cursor advances
//	  (offset rides it into view on overflow) → return true, stay. ✓
//	first worker, project above (left column)       → cursor pinned but
//	  no cross-panel neighbor → return true, stay (don't leave right
//	  column via arrow). ✓
func (m *Model) scrollRightPanel(delta int) bool {
	row := m.selectedRow()
	if row == nil {
		return false
	}
	switch row.kind {
	case rowWorker:
		return m.scrollOrCross(delta, rowWorker, &m.workersScrollOffset)
	case rowAgent:
		return m.scrollOrCross(delta, rowAgent, &m.agentsScrollOffset)
	case rowSeparator:
		// Right-column separator (separatorAgentIdle) is part of the
		// agents panel; scroll it. Left-column separators fall through
		// to moveCursor.
		if row.separator != nil && row.separator.kind == separatorAgentIdle {
			return m.scrollOrCross(delta, rowAgent, &m.agentsScrollOffset)
		}
	}
	return false
}

// isLeftColumnRow reports whether a dashRow renders in the LEFT PROJECTS
// column. Projects + tasks always do; separators do UNLESS they are the
// right-column agent-idle group (which lives in the agents sub-panel).
func isLeftColumnRow(r *dashRow) bool {
	if r == nil {
		return false
	}
	switch r.kind {
	case rowProject, rowTask:
		return true
	case rowSeparator:
		return r.separator == nil || r.separator.kind != separatorAgentIdle
	}
	return false
}

// scrollLeftPanel pages the LEFT PROJECTS column by delta when the cursor
// is on a left-column row AND the panel has somewhere to scroll in that
// direction (tui-project-list-truncat). Returns true when the offset
// actually moved (caller skips moveCursor so the arrow scrolls in place);
// false otherwise so handleKey falls through to moveCursor — that keeps
// the wrap-at-boundary j/k behavior intact at the top/bottom edges and on
// a non-overflowing list (no scroll → normal cursor nav).
//
// Mirrors the #177 right-panel arrow-scroll contract but without the
// worker↔agent cross-panel complexity: the left column is a single panel.
//
// The selected row moves by ONE left-column row per press; the scroll
// offset is then aligned to that row's RENDERED line position so the
// block stays inside the visible window. Aligning (rather than bumping
// the offset by a fixed delta) is the fix for the codex [P2] desync: a
// project block is multiple rendered lines, so a 1-line offset bump fell
// out of step with a 1-row cursor move and the selected block drifted
// off-screen after a few presses. Clamping to panelMaxOffset(rowProject)
// prevents the stuck-at-bottom overscroll bug (#177 clampScrollOffset
// rationale).
//
// Returns false (→ caller falls through to moveCursor's wrap) only when
// the cursor is pinned at the first/last left-column row AND the offset
// can't move — i.e. the true top/bottom edge. Otherwise returns true so
// the arrow scrolls in place.
func (m *Model) scrollLeftPanel(delta int) bool {
	row := m.selectedRow()
	if !isLeftColumnRow(row) {
		return false
	}
	beforeCursor := m.dashCursor
	beforeOff := m.projectsScrollOffset
	m.moveCursorWithinLeft(delta)
	m.alignLeftScrollToCursor()
	if m.dashCursor == beforeCursor && m.projectsScrollOffset == beforeOff {
		// Pinned at the first/last left row with nowhere to scroll — let
		// moveCursor handle the wrap so arrows don't dead-end.
		return false
	}
	return true
}

// alignLeftScrollToCursor sets projectsScrollOffset so the selected
// left-column row's rendered block sits inside the visible window
// [offset, offset+visible). It reads the per-row line spans from
// buildBodyLinesCore (the same accounting the renderer uses) so the
// offset can never disagree with what trimWithScroll actually shows.
//
//	cursor line above window  → scroll up so the row is the top line
//	cursor line below window  → scroll down so the row is the bottom line
//	cursor line inside window → leave the offset untouched (no jitter)
//
// No-op on the unbounded-fallback render path (ok=false) or when the
// left column fits (maxOff==0): there is no scrolling to align.
func (m *Model) alignLeftScrollToCursor() {
	leftW, rightW := splitColumns(usableWidth(m.width))
	leftRows, _, ok := m.bodyRowBudget(leftW, rightW)
	if !ok {
		return
	}
	maxOff := m.leftPanelMaxOffset()
	if maxOff == 0 {
		m.projectsScrollOffset = 0
		return
	}
	_, _, _, lineStart := buildBodyLinesCore(*m, leftW, rightW)
	if m.dashCursor < 0 || m.dashCursor >= len(lineStart) {
		return
	}
	start := lineStart[m.dashCursor]
	if start < 0 {
		// Cursor is not on a left-column row — nothing to align.
		return
	}
	// trimWithScroll reserves one row for the overflow footer, so the
	// visible content window is leftRows-1 lines.
	visible := leftRows - 1
	if visible < 1 {
		visible = 1
	}
	off := m.projectsScrollOffset
	if start < off {
		off = start // row is above the window → make it the top line
	} else if start >= off+visible {
		off = start - visible + 1 // row is below → make it the bottom line
	}
	m.projectsScrollOffset = clampScrollOffset(off, maxOff)
}

// moveCursorWithinLeft advances dashCursor by delta but stays anchored to
// LEFT-column rows (project / task / left separator). Skips past any
// right-column rows interleaved in the unified list and stops at the
// first/last left row rather than wrapping. Companion to scrollLeftPanel
// so the cursor follows a scrolled left panel (parity with
// moveCursorWithinKind for the right panels).
func (m *Model) moveCursorWithinLeft(delta int) {
	rows := m.dashboardRows()
	if len(rows) == 0 {
		return
	}
	step := 1
	if delta < 0 {
		step = -1
		delta = -delta
	}
	pos := m.dashCursor
	scan := pos
	for moved := 0; moved < delta; {
		next := scan + step
		if next < 0 || next >= len(rows) {
			break
		}
		scan = next
		if isLeftColumnRow(&rows[scan]) {
			pos = scan
			moved++
		}
	}
	m.dashCursor = pos
}

// scrollOrCross advances the given panel's scroll offset + within-kind
// cursor by delta. Returns true when the cursor made progress inside
// the panel OR the cursor is pinned against a non-right-panel edge
// (stay; caller skips moveCursor). Returns false ONLY when the cursor
// is pinned at the worker↔agent boundary (adjacent row is the other
// right-column panel) — in that case the phantom offset bump is
// restored and the caller falls through to moveCursor to cross.
func (m *Model) scrollOrCross(delta int, kind rowKind, offset *int) bool {
	before := m.dashCursor
	beforeOff := *offset
	*offset = clampScrollOffset(*offset+delta, m.panelMaxOffset(kind))
	m.moveCursorWithinKind(delta, kind)
	if m.dashCursor != before {
		// Cursor advanced within the panel — stay (scroll rode it into
		// view on overflow).
		return true
	}
	// Cursor pinned. Fall through to moveCursor when the immediate
	// neighbor in the delta direction is the OTHER right-column panel
	// (worker↔agent cross) OR there is no neighbor at all (we're at the
	// absolute top/bottom of the unified list — let moveCursor wrap).
	// Stay ONLY when the neighbor is an in-range LEFT-column row: arrows
	// must not silently jump sideways out of the right column (← is the
	// explicit left-jump key; #177 ScrollCursorStaysInPanel guards it).
	if m.fallsThroughAtEdge(before, delta, kind) {
		*offset = beforeOff // undo the phantom count-based bump
		m.alignDestPanelOnCross(delta, kind)
		return false
	}
	return true
}

// alignDestPanelOnCross resets the DESTINATION right-panel's scroll
// offset so the row moveCursor is about to land on stays visible after a
// worker↔agent boundary cross (codex review iter-2 [P2]).
//
// trimWithScroll slices `lines[offset:end]` and does NOT auto-follow the
// cursor — visibility depends entirely on the offset. scrollOrCross only
// restored the SOURCE offset, so if the destination panel was previously
// scrolled away (e.g. agentsScrollOffset > 0 from an earlier overflow
// scroll, then k'd back into workers), crossing selects the first/last
// destination row while the render still shows the old window — the
// selected row is hidden and [⏎]/[a]/[h] target an off-screen agent.
//
//	↓ worker→agent: lands on the FIRST agent → agents panel to top
//	  (offset 0). ✓
//	↑ agent→worker: lands on the LAST worker → workers panel to bottom
//	  (count-based max; trimWithScroll re-clamps to the render-visible
//	  bottom, same contract the in-panel overflow scroll uses). ✓
//
// Only fires when `kind` is the source of a genuine worker↔agent cross.
// The no-neighbor wrap case (↓ on last agent → moveCursor wraps to the
// top of the unified list) resets BOTH offsets to 0 so a wrap onto a
// right-column row is also visible.
func (m *Model) alignDestPanelOnCross(delta int, kind rowKind) {
	switch kind {
	case rowWorker:
		// ↓ from last worker → first agent. (↑ from a worker can't cross
		// upward into agents; it either stays or wraps at index 0.)
		if delta > 0 {
			m.agentsScrollOffset = 0
			return
		}
	case rowAgent:
		// ↑ from first agent → last worker.
		if delta < 0 {
			m.workersScrollOffset = clampScrollOffset(
				m.panelMaxOffset(rowWorker), m.panelMaxOffset(rowWorker))
			return
		}
	}
	// No-neighbor wrap (e.g. ↓ on the last agent): moveCursor wraps to
	// the top of the unified list. Reset both panels to the top so a
	// wrap that lands on a right-column row is visible.
	m.workersScrollOffset = 0
	m.agentsScrollOffset = 0
}

// fallsThroughAtEdge reports whether a pinned cursor at row index `pos`
// stepping by `delta` (±1) should fall through to moveCursor. True when
// the neighbor is the OTHER right-column sub-panel (worker↔agent cross)
// or there is no neighbor (top/bottom of the list → moveCursor wraps).
// False when the neighbor is an in-range LEFT-column row (project /
// task / left separator) — staying keeps arrows from leaving the right
// column sideways.
func (m *Model) fallsThroughAtEdge(pos, delta int, fromKind rowKind) bool {
	rows := m.dashboardRows()
	next := pos
	if delta > 0 {
		next++
	} else {
		next--
	}
	if next < 0 || next >= len(rows) {
		// No neighbor — absolute top/bottom. Let moveCursor wrap.
		return true
	}
	nbr := rows[next]
	switch fromKind {
	case rowWorker:
		if nbr.kind == rowAgent {
			return true
		}
		return nbr.kind == rowSeparator && nbr.separator != nil &&
			nbr.separator.kind == separatorAgentIdle
	case rowAgent:
		return nbr.kind == rowWorker
	}
	return false
}

// clampScrollOffset bounds the stored offset at [0, maxOff]. Without
// the upper clamp (codex iter-3 [P2]), repeated ↓ past the visible
// end leaves the model with an oversized offset; the renderer's
// trimWithScroll then clamps for display only, but the next ↑ press
// just decrements the oversized value — the panel appears stuck at
// the bottom until many extra ↑ presses burn off the overshoot.
func clampScrollOffset(want, maxOff int) int {
	if want < 0 {
		return 0
	}
	if maxOff < 0 {
		maxOff = 0
	}
	if want > maxOff {
		return maxOff
	}
	return want
}

// panelMaxOffset returns a conservative upper bound for the workers
// or agents panel scroll offset based on the count of rows of the
// matching kind. Each worker/agent row produces roughly 3 lines
// (block header + status + blank) in buildBodyLines, so the bound is
// 3 * rowCount. The renderer's trimWithScroll clamps tighter at
// render time, but having a model-side bound prevents the
// stuck-at-bottom UX bug after overscroll (codex iter-3 [P2]).
func (m *Model) panelMaxOffset(kind rowKind) int {
	// LEFT PROJECTS column (tui-project-list-truncat): the panel is the
	// whole projects+tasks column, whose block heights vary
	// (collapsed project ≈ 5 lines, +1 per expanded task). A flat
	// 3×rowCount estimate would under-bound and strand the bottom
	// projects below a scroll ceiling. Instead, compute the EXACT bound
	// from the rendered left-line count minus the visible budget — the
	// same numbers renderTwoColumnBody uses — so ↓ can always reach the
	// last project. trimWithScroll re-clamps at render time; this bound
	// just stops the offset from over/under-shooting.
	if kind == rowProject {
		return m.leftPanelMaxOffset()
	}
	const linesPerRow = 3
	rows := m.dashboardRows()
	count := 0
	for _, r := range rows {
		if r.kind == kind {
			count++
		}
		if kind == rowAgent && r.kind == rowSeparator && r.separator != nil &&
			r.separator.kind == separatorAgentIdle {
			count++
		}
	}
	bound := count * linesPerRow
	if bound < 0 {
		bound = 0
	}
	return bound
}

// leftPanelMaxOffset returns the exact scroll ceiling for the LEFT
// PROJECTS column: total rendered left lines minus the visible left-row
// budget. Mirrors renderTwoColumnBody's width + leftRows math (via the
// shared bodyRowBudget) so the bound agrees with what the renderer
// actually shows. Zero when the left column fits (no overflow → no
// scroll) or on the unbounded-fallback render path.
func (m *Model) leftPanelMaxOffset() int {
	leftW, rightW := splitColumns(usableWidth(m.width))
	leftRows, _, ok := m.bodyRowBudget(leftW, rightW)
	if !ok {
		return 0
	}
	leftLines, _, _ := buildBodyLines(*m, leftW, rightW)
	if len(leftLines) <= leftRows {
		return 0
	}
	// trimWithScroll reserves one row for the footer when overflowing, so
	// the effective visible content rows is leftRows-1. The max offset is
	// the count of lines that can't fit that window.
	visible := leftRows - 1
	if visible < 0 {
		visible = 0
	}
	off := len(leftLines) - visible
	if off < 0 {
		off = 0
	}
	return off
}

// moveCursorWithinKind advances dashCursor by delta but stays anchored
// to rows of the target kind (rowWorker or rowAgent) on the right
// column. Falls off the end of the kind-run → clamps to the LAST row
// of that kind it found (no wrap into projects, no crossover into the
// other right-column panel). Used by scrollRightPanel so the cursor
// follows the scrolled panel rather than getting stranded on a hidden
// row.
//
// Boundary behavior (codex iter-3 [P2]): when stepping in `step`
// direction would land on a non-matching row (e.g., ↑ from first
// worker into a left-column row, or ↓ from last worker into the agent
// sub-heading), the cursor stays put rather than crossing the panel
// boundary. Crossover would silently re-route subsequent actions
// ([⏎] / [a]) to the wrong panel.
//
// Implementation: walk the unified row list from the current cursor
// in delta-sign steps. Only commit pos when the candidate row is of
// the matching kind; non-matching rows are SKIPPED past (so a few
// non-matching rows between two workers don't strand the cursor) but
// crossing a different RIGHT-column kind (e.g., from worker into
// agent) ends the walk early.
func (m *Model) moveCursorWithinKind(delta int, kind rowKind) {
	rows := m.dashboardRows()
	if len(rows) == 0 {
		return
	}
	step := 1
	if delta < 0 {
		step = -1
		delta = -delta
	}
	matches := func(r dashRow) bool {
		if r.kind == kind {
			return true
		}
		// Agents panel includes the right-column separator (idle agents).
		if kind == rowAgent && r.kind == rowSeparator && r.separator != nil &&
			r.separator.kind == separatorAgentIdle {
			return true
		}
		return false
	}
	// Crossing into a DIFFERENT right-column kind ends the walk —
	// don't silently change panels.
	isOtherPanel := func(r dashRow) bool {
		switch kind {
		case rowWorker:
			if r.kind == rowAgent {
				return true
			}
			if r.kind == rowSeparator && r.separator != nil &&
				r.separator.kind == separatorAgentIdle {
				return true
			}
		case rowAgent:
			if r.kind == rowWorker {
				return true
			}
		}
		return false
	}
	pos := m.dashCursor
	scan := pos
	for moved := 0; moved < delta; {
		next := scan + step
		if next < 0 || next >= len(rows) {
			break
		}
		if isOtherPanel(rows[next]) {
			break
		}
		scan = next
		if matches(rows[scan]) {
			pos = scan
			moved++
		}
	}
	m.dashCursor = pos
}

// moveCursor advances dashCursor by delta across the unified row list
// (projects + tasks + workers + agents). Wraps at boundaries so j at
// the bottom returns to the top — matches issue #53 spec ("Wraps at
// boundaries"). When the row list is empty (early renders before the
// first dashboardMsg lands), no-op.
func (m *Model) moveCursor(delta int) {
	rows := m.dashboardRows()
	if len(rows) == 0 {
		return
	}
	n := len(rows)
	m.dashCursor = ((m.dashCursor+delta)%n + n) % n
}

// jumpToLeftPanel snaps the cursor onto the first PROJECTS-column row
// (issue #83). Operators with long task expansions can otherwise burn
// many j/k presses just to walk back across the unified row list. The
// PROJECTS column always leads dashboardRows() (see rows.go's row
// ordering contract), so the first row of kind rowProject is the
// LEFT-panel anchor.
//
// Issue #98: when every project is idle AND the operator has explicitly
// collapsed the idle group, the LEFT column starts with a separator
// (no project rows visible). Fall back to the first rowSeparator so the
// operator can still ← into the LEFT panel and press [enter] to expand.
// Without this fallback, ← would no-op and cursor would stay stuck on
// the right column, hiding the separator from keyboard access.
//
// No-op only when truly no LEFT-column row exists — leaves the cursor
// where it is rather than hopping to 0, which could be a worker/agent
// row in pathological layouts.
//
// j/k behavior is unchanged: this is an additive shortcut, not a
// rebinding. Idempotent — pressing ← when already on the first project
// is a no-op (no flash churn).
func (m *Model) jumpToLeftPanel() {
	rows := m.dashboardRows()
	for i, r := range rows {
		if r.kind == rowProject {
			m.dashCursor = i
			return
		}
	}
	// Fallback: first rowSeparator (issue #98). Reachable when every
	// project is idle + explicitly collapsed.
	for i, r := range rows {
		if r.kind == rowSeparator {
			m.dashCursor = i
			return
		}
	}
}

// jumpToRightPanel snaps the cursor onto the first WORKERS/agents-column
// row (issue #83). Right-panel ordering in dashboardRows() is workers
// first then v0.1 agents, so we prefer the first rowWorker and fall back
// to the first rowAgent — this matches the visual top of the right panel
// in either populated state.
//
// When the right panel is empty (no workers and no agents), set a
// non-error flash so the operator sees why the keypress did nothing.
// We don't move the cursor in that case; bouncing to row 0 would just
// scroll the operator's selection without telling them why.
func (m *Model) jumpToRightPanel() {
	rows := m.dashboardRows()
	// First pass: prefer a worker row (top of WORKERS · N ACTIVE block).
	for i, r := range rows {
		if r.kind == rowWorker {
			m.dashCursor = i
			return
		}
	}
	// Second pass: fall back to the first agent row when no workers
	// are loaded (right panel still has agents in the v0.1 sub-block).
	for i, r := range rows {
		if r.kind == rowAgent {
			m.dashCursor = i
			return
		}
	}
	// Right panel is genuinely empty. Surface a flash; non-error
	// because pressing → on an empty right panel is a navigation
	// no-op, not a failure.
	m.flash = &flashMsg{text: "right panel is empty"}
}

// View renders the current state. Called by bubbletea on every model
// update. Keep this pure: no I/O, no time.Now(), no surprises.
//
// time.Since is called for "age" and "active X ago" derivations — the
// alternative would be to snapshot the time on every Update, which
// would be churn for a render-only quantity. We accept the impurity
// for the sake of a live age column.
//
// Layout is built in two halves — the "top" (title, banner, agent
// list, coach hint, flash) and the "footer" (summary + chips, or the
// active prompt) — then padToBottom inserts blank lines between them
// so the footer pins to the bottom of the terminal regardless of
// agent count. m.height comes from tea.WindowSizeMsg.
func (m Model) View() string {
	// v0.2 (issue #53): the dashboard is the only view. Legacy v0.1
	// agents-list rendering paths are gone; agents fold into the
	// dashboard's right column. Modal overlays (help, detail panel,
	// task-add prompt, search) are composed on top of the dashboard
	// body or replace the footer; they don't switch to a separate
	// view.
	//
	// Banners (upgrade nudge, agent-load error, flash) are prepended
	// so dispatch/handoff/rm failure output and load errors stay
	// visible — operators must still see when commands fail or records
	// are malformed.
	body := m.renderDashboardBanners() + renderDashboard(m)
	if overlay := m.renderOverlay(); overlay != "" {
		body = overlay
	}
	footer := m.renderFooter()
	return padToBottom(body, footer, m.height, m.width)
}

// renderOverlay returns a modal overlay (help / detail panel) that
// REPLACES the dashboard body when active. Empty string means no
// overlay → dashboard renders normally. Modal overlays compose on top
// of the dashboard so the banner row + footer keep their position.
func (m Model) renderOverlay() string {
	switch {
	case m.showHelp:
		return renderHelpOverlay(m.width)
	case m.detail != nil:
		return renderDetailOverlay(*m.detail, m.width, m.height)
	}
	return ""
}

// renderDashboardBanners renders the upgrade banner, agent-load error,
// the agent-derived alert chips ("1 blocked  2 hot context"), and the
// active flash above the dashboard body. Returns "" when nothing is
// active so the dashboard layout is not shifted by an empty row.
//
// Order (lowest urgency → highest, most ephemeral last):
//  1. upgrade nudge
//  2. agent-load error
//  3. alert banner (per-agent statuses aggregated)
//  4. flash (last action's success/failure)
func (m Model) renderDashboardBanners() string {
	alert := renderAlertBanner(m.records, m.aliveByID)
	if m.upgradeBanner == "" && m.err == nil && m.flash == nil && alert == "" {
		return ""
	}
	var b strings.Builder
	if m.upgradeBanner != "" {
		b.WriteString(upgradeBannerStyle.Render(m.upgradeBanner))
		b.WriteString("\n")
	}
	if m.err != nil {
		b.WriteString(errStyle.Render(fmt.Sprintf("error reading agents: %v", m.err)))
		b.WriteString("\n")
	}
	if alert != "" {
		b.WriteString(alert)
	}
	if m.flash != nil {
		style := dimStyle
		if m.flash.isErr {
			style = errStyle
		}
		b.WriteString(style.Render(m.flash.text))
		b.WriteString("\n")
	}
	return b.String()
}

// (renderTop removed in v0.2 issue #53: the dashboard is now the only
// rendered view. renderDashboardBanners handles the upgrade chip,
// agent-load error, and flash that used to live here. Alert banner
// (blocked/hot-context counts across agents) is folded into
// renderDashboardBanners for the same reason.)

// renderFooter returns the bottom-of-screen block — either a mode
// prompt (picker / dispatch / confirm) or the smart footer (divider +
// summary + chip row). Always opens with a divider line so the
// footer reads as its own section pinned to the terminal bottom.
//
// When the model is rendering the v0.2 Variant A dashboard view AND
// no modal prompt is active, we substitute the dashboard's keybind
// strip (mockup-aligned) so the row reads as part of the ops console
// instead of the v0.1 chip layout. Modal prompts (picker, dispatch,
// confirm-archive) keep the v0.1 footer because the prompts are still
// agent-centric in v0.2.
func (m Model) renderFooter() string {
	usable := m.width - 1
	if usable < 60 {
		usable = 60
	}
	var b strings.Builder
	b.WriteString(dividerStyle.Render(divider(m.width, 0)))
	b.WriteString("\n")

	switch m.mode {
	case modePickRepo:
		b.WriteString(renderPicker(m))
	case modeAddProject:
		b.WriteString(renderAddProjectPicker(m))
	case modePromptDispatch:
		header := "dispatch task"
		if m.pickedRepo.Display != "" {
			header += " in " + m.pickedRepo.Display
		}
		b.WriteString(promptStyle.Render(header + ": " + m.promptBuf + "█"))
		b.WriteString("\n")
		b.WriteString(dimStyle.Render("[enter] submit  [esc] cancel"))
		b.WriteString("\n")
	case modeConfirmArchive:
		b.WriteString(promptStyle.Render(fmt.Sprintf(
			"Archive agent %s? Kills tmux session + deletes record (no replacement). [y/N]",
			m.archiveCandidate)))
		b.WriteString("\n")
	case modeConfirmDismissProject:
		// Surface the dead-agent count so the operator sees how many
		// records the [y] confirm will archive (issue #96 gap 3). Zero
		// is fine — the row exists only as a synthetic and will drop
		// from view after refresh.
		count := len(m.dismissProjectDeadAgents)
		b.WriteString(promptStyle.Render(fmt.Sprintf(
			"Dismiss legacy project %s? Archives %d dead agent record(s) — no live agents will be touched. [y/N]",
			m.dismissProjectCandidate, count)))
		b.WriteString("\n")
	case modeConfirmReset:
		// Reset is the most destructive project-row op: it reaps every
		// coord record (kills tmux + archives) AND clears the stale lock,
		// then respawns one fresh coord. Surface the coord-record count
		// so the operator sees the blast radius before confirming
		// (dead-end-recovery-r-8559).
		count := len(m.resetCoordIDs)
		b.WriteString(promptStyle.Render(fmt.Sprintf(
			"Reset project %s? Reaps %d coord record(s) + clears stale coordinator.lock, then spawns ONE fresh coord. [y/N]",
			m.resetProjectCandidate, count)))
		b.WriteString("\n")
	case modePromptTaskAdd:
		b.WriteString(promptStyle.Render("task spec: " + m.promptBuf + "█"))
		b.WriteString("\n")
		b.WriteString(dimStyle.Render("[enter] submit  [esc] cancel"))
		b.WriteString("\n")
	case modePromptSearch:
		b.WriteString(promptStyle.Render("/" + m.promptBuf + "█"))
		b.WriteString("\n")
		b.WriteString(dimStyle.Render("[esc] clear  [enter] keep filter"))
		b.WriteString("\n")
	default:
		// Hidden-projects chip data (issue #98). hiddenWith is the
		// count of hidden projects that DO have fresh activity — the
		// nudge tells operators the hidden list isn't dormant without
		// overriding the hide.
		hiddenSet := hiddenProjectsSet()
		hiddenCount := len(hiddenSet)
		hiddenWith := 0
		if hiddenCount > 0 {
			window := m.activeWindow
			if window <= 0 {
				window = activeWindowDefault
			}
			var workers []*WorkerRow
			if m.dashboard != nil {
				workers = m.dashboard.Workers
			}
			hiddenWith = hiddenWithActivity(
				m.unifiedProjectsAll(),
				hiddenSet,
				projectAddedAtFn,
				workers,
				m.records,
				nowFn(),
				window,
			)
		}
		b.WriteString(renderDashboardFooterWithHidden(
			time.Since(m.startedAt), usable, m.searchFilter,
			hiddenCount, hiddenWith,
		))
		b.WriteString("\n")
	}
	return b.String()
}

// divider returns a horizontal line of ─ characters spanning the
// terminal width minus one cell. The 1-cell right margin matches
// titleRow's reasoning: rows that print into the final column can
// trigger phantom-newline wrap on some terminals, eating the row
// visually. When width is unknown (early renders before
// WindowSizeMsg lands), falls back to a sensible minimum so the
// title divider still draws under the heading.
func divider(width, fallback int) string {
	const minWidth = 12
	w := width
	if w <= 0 {
		w = fallback
	}
	if w > 1 {
		w-- // 1-cell right margin
	}
	if w < minWidth {
		w = minWidth
	}
	return strings.Repeat("─", w)
}

// padToBottom inserts blank lines between top and bottom so the
// combined output fills targetHeight visual rows on a terminal of
// termWidth cells. "Visual rows" matters because long lines (the
// archive-confirm prompt, blocked-reason quotes) soft-wrap on
// narrow terminals — counting only "\n" undercounts those rows and
// pushes the footer off-screen on 80-col displays. termWidth ≤ 0
// (early renders before WindowSizeMsg lands) collapses to logical
// row count, which is fine because no wrap happens at unknown
// width. When content is already taller than targetHeight, returns
// top+bottom unpadded — the footer scrolls off the top, which beats
// truncating it.
func padToBottom(top, bottom string, targetHeight, termWidth int) string {
	if targetHeight <= 0 {
		return top + bottom
	}
	used := visualRows(top, termWidth) + visualRows(bottom, termWidth)
	if used >= targetHeight {
		return top + bottom
	}
	return top + strings.Repeat("\n", targetHeight-used) + bottom
}

// visualRows counts how many terminal rows s consumes when rendered
// in a window of termWidth cells. Each logical line (separated by
// \n) takes ceil(width / termWidth) rows after soft-wrap; empty
// lines count as 1. A trailing "\n" doesn't add an extra empty row.
// termWidth ≤ 0 falls back to logical line counting (no wrap math).
func visualRows(s string, termWidth int) int {
	if s == "" {
		return 0
	}
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return 1 // s was just "\n"
	}
	rows := 0
	for _, line := range strings.Split(s, "\n") {
		w := lipgloss.Width(line)
		if termWidth <= 0 || w <= termWidth {
			rows++
			continue
		}
		rows += (w + termWidth - 1) / termWidth
	}
	return rows
}

// renderAddProjectPicker draws the [+] add-project picker. Same shape
// as renderPicker but with a header that names the action ("add
// project") so the operator doesn't confuse it with [d] dispatch when
// glancing at a screenshot.
func renderAddProjectPicker(m Model) string {
	var b strings.Builder
	b.WriteString(promptStyle.Render("add project: " + m.pickerFilter + "█"))
	b.WriteString("\n")

	filtered := filterCandidates(m.repoCandidates, m.pickerFilter)
	switch {
	case len(m.repoCandidates) == 0:
		b.WriteString(dimStyle.Render(
			"  no repos found — set $FLEET_PROJECT_DIRS or run from a repo dir"))
		b.WriteString("\n")
	case len(filtered) == 0:
		b.WriteString(dimStyle.Render("  (no matches)"))
		b.WriteString("\n")
	default:
		start := 0
		if m.pickerCursor >= pickerVisibleRows {
			start = m.pickerCursor - pickerVisibleRows + 1
		}
		end := start + pickerVisibleRows
		if end > len(filtered) {
			end = len(filtered)
		}
		for i := start; i < end; i++ {
			line := m.repoCandidates[filtered[i]].Display
			if i == m.pickerCursor {
				b.WriteString(cursorStyle.Render("▸ " + line))
			} else {
				b.WriteString("  " + line)
			}
			b.WriteString("\n")
		}
		if remaining := len(filtered) - end; remaining > 0 {
			b.WriteString(dimStyle.Render(
				fmt.Sprintf("  (%d more — type to filter)", remaining)))
			b.WriteString("\n")
		}
	}
	b.WriteString(dimStyle.Render(
		"[↑/↓] navigate  [enter] add  [esc] cancel  type to filter"))
	b.WriteString("\n")
	return b.String()
}

// pickerVisibleRows caps how many repos are listed at once. Anything
// further is reachable via the filter.
const pickerVisibleRows = 8

// renderPicker draws the [d] repo picker: a one-line filter input, a
// scrolling list of matched candidates with the cursor, an overflow
// hint when the filter is too broad, and a footer of keybinds.
func renderPicker(m Model) string {
	var b strings.Builder
	b.WriteString(promptStyle.Render("pick repo: " + m.pickerFilter + "█"))
	b.WriteString("\n")

	filtered := filterCandidates(m.repoCandidates, m.pickerFilter)
	switch {
	case len(m.repoCandidates) == 0:
		b.WriteString(dimStyle.Render(
			"  no repos found — set $FLEET_PROJECT_DIRS or run from a repo dir"))
		b.WriteString("\n")
	case len(filtered) == 0:
		b.WriteString(dimStyle.Render("  (no matches)"))
		b.WriteString("\n")
	default:
		// Scroll window: keep the cursor in the top half whenever
		// possible. start drifts forward as the cursor approaches the
		// last visible row.
		start := 0
		if m.pickerCursor >= pickerVisibleRows {
			start = m.pickerCursor - pickerVisibleRows + 1
		}
		end := start + pickerVisibleRows
		if end > len(filtered) {
			end = len(filtered)
		}
		for i := start; i < end; i++ {
			line := m.repoCandidates[filtered[i]].Display
			if i == m.pickerCursor {
				b.WriteString(cursorStyle.Render("▸ " + line))
			} else {
				b.WriteString("  " + line)
			}
			b.WriteString("\n")
		}
		if remaining := len(filtered) - end; remaining > 0 {
			b.WriteString(dimStyle.Render(
				fmt.Sprintf("  (%d more — type to filter)", remaining)))
			b.WriteString("\n")
		}
	}
	b.WriteString(dimStyle.Render(
		"[↑/↓] navigate  [enter] pick  [esc] cancel  type to filter"))
	b.WriteString("\n")
	return b.String()
}

// sortRecords returns a copy sorted by (group-key asc, spawned desc).
// Computes group keys live — used by tests and any caller that
// doesn't have a precomputed cache. Production renders go through
// sortRecordsBy with the loadAgentsCmd-computed cache.
func sortRecords(in []*agent.Record) []*agent.Record {
	return sortRecordsBy(in, nil)
}

// sortRecordsBy returns a copy sorted by (group-key asc, spawned
// desc) using a precomputed groupKeys map (id → key) when present.
// Falls back to live projectGroupKey calls per record when the map
// is nil or missing entries — keeps tests + early-render paths
// working before the first agentsMsg lands.
func sortRecordsBy(in []*agent.Record, groupKeys map[string]string) []*agent.Record {
	out := make([]*agent.Record, len(in))
	copy(out, in)
	sort.SliceStable(out, func(i, j int) bool {
		ki := groupKeyFor(out[i], groupKeys)
		kj := groupKeyFor(out[j], groupKeys)
		if ki != kj {
			return ki < kj
		}
		return out[i].SpawnedAt.After(out[j].SpawnedAt)
	})
	return out
}

// groupKeyFor reads from the cache when present, falls back to a
// live projectGroupKey() call when the cache is nil or the entry is
// missing (legacy code paths, early renders).
func groupKeyFor(r *agent.Record, cache map[string]string) string {
	if cache != nil {
		if k, ok := cache[r.ID]; ok {
			return k
		}
	}
	return projectGroupKey(r)
}

// renderAlertBanner aggregates the urgent counts across all records
// into a single line. Empty when nothing's wrong — a clean dashboard
// shouldn't waste a line on "0 of everything".
//
// Glyphs and colors:
//   - ▌ orange   blocked
//   - △ red      hot context (≥70% on any record)
//   - ● cyan     asking (agent ended its last turn on a question)
//   - ● cyan     in review (Mode=="review")
//   - ○ dim      idle (stopped, work done, no question)
//   - ✗ faint    dead
//
// The asking/idle split (open ○ vs filled ●, faint vs cyan) is what
// lets the operator scan the dashboard and instantly see which rows
// need attention. "in review" gets its own chip even though it shares
// the asking color because mode is a separate signal — a paused
// reviewer must read as review, not asking/idle.
//
// Counts run independently: a record can be both "blocked" AND have
// hot context, so it bumps both counts. That's intentional — the
// banner is a heads-up, not a partition.
func renderAlertBanner(records []*agent.Record, alive map[string]bool) string {
	var blocked, asking, idle, review, hot, dead int
	for _, r := range records {
		switch deriveStatus(r, alive) {
		case "blocked":
			blocked++
		case "asking":
			asking++
		case "idle":
			idle++
		case "review":
			review++
		case "dead":
			dead++
		}
		if r.ContextPct != nil && *r.ContextPct >= 70 {
			hot++
		}
	}
	var parts []string
	if blocked > 0 {
		parts = append(parts, statusBlockedStyle.Render(
			fmt.Sprintf("▌ %d blocked", blocked)))
	}
	if hot > 0 {
		parts = append(parts, statusUrgentStyle.Render(
			fmt.Sprintf("△ %d hot context", hot)))
	}
	if asking > 0 {
		parts = append(parts, statusAskingStyle.Render(
			fmt.Sprintf("● %d asking", asking)))
	}
	if review > 0 {
		parts = append(parts, statusReviewStyle.Render(
			fmt.Sprintf("● %d in review", review)))
	}
	if idle > 0 {
		parts = append(parts, statusIdleStyle.Render(
			fmt.Sprintf("○ %d idle", idle)))
	}
	if dead > 0 {
		parts = append(parts, statusDeadStyle.Render(
			fmt.Sprintf("✗ %d dead", dead)))
	}
	if len(parts) == 0 {
		return ""
	}
	// Two-space gap between chips matches mockup spacing — bullet
	// dot was the v1 separator and read busier than the design.
	return strings.Join(parts, "   ") + "\n"
}

// glyphFor returns the single-character status glyph and its lipgloss
// style. Glyphs match the v2 mockup: filled green dot for doing, solid
// orange bar for blocked, cyan dot for review, dim ✗ for dead.
func glyphFor(status string) (string, lipgloss.Style) {
	switch status {
	case "live":
		return "●", glyphLiveStyle
	case "asking":
		return "●", glyphAskingStyle
	case "review":
		return "●", glyphReviewStyle
	case "idle":
		return "○", glyphIdleStyle
	case "blocked":
		return "▌", glyphBlockedStyle
	case "dead":
		return "✗", glyphDeadStyle
	case "auto-yellow":
		return "⊕", glyphHandoffStyle
	case "auto-red", "precompact":
		return "⊕", glyphUrgentStyle
	}
	return "·", dimStyle
}

func padRight(s string, w int) string {
	cur := lipgloss.Width(s)
	if cur >= w {
		return s
	}
	return s + strings.Repeat(" ", w-cur)
}

func defaultStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// projectGroupKey returns the stable identity used for sorting +
// grouping records. Decoupled from projectDisplay so display tweaks
// (path-segment derivation, future relabeling) can't accidentally
// reshuffle groups.
//
// Key shape: "<resolved-cwd>\x00<project-tag>" so two agents from
// the same physical checkout but with different `--project` values
// (different lock domains, fleet's own model of "distinct project"
// — see internal/state lock files) keep separate group headers.
// The NUL separator means a project tag containing "/" or other
// path chars can't accidentally collide with a cwd boundary.
//
// EvalSymlinks resolves macOS /var → /private/var and other
// symlinked worktree paths so the same physical checkout reached
// via different path spellings collapses into one group. Falls
// back to filepath.Clean when the path no longer exists.
//
// Legacy / Cwd-empty records key on r.Project alone.
func projectGroupKey(r *agent.Record) string {
	tag := defaultStr(r.Project, "-")
	if r.Cwd == "" {
		return tag
	}
	var resolved string
	if eval, err := filepath.EvalSymlinks(r.Cwd); err == nil {
		resolved = eval
	} else {
		resolved = filepath.Clean(r.Cwd)
	}
	return resolved + "\x00" + tag
}

// deriveStatus picks one short label that summarizes the agent's
// current condition for the STATUS column. Precedence (most-urgent
// first):
//
//  1. dead         — tmux session is gone (claude exited inside it)
//  2. <handoff>    — handoff_type set to an in-flight value
//     (auto-yellow / auto-red / precompact). "manual"
//     is a spawn-origin label set by spawn.Spawn on
//     every successor and is NOT surfaced — it would
//     pin "manual" on every post-handoff agent forever
//     (skills/fleet-guard/handoff.py:113-119).
//  3. blocked      — fleet-guard / operator flagged the agent blocked
//  4. review       — Mode=="review" (an agent dispatched as reviewer)
//  5. asking       — needs_input=true AND has_pending_question=true
//     (agent stopped on a question for the operator)
//  6. idle         — needs_input=true AND has_pending_question=false
//     (agent stopped, work done, nothing pending)
//  7. live         — fresh spawn or actively-running turn
//
// dead wins over everything because the other states are meaningless
// when the underlying process is gone. In-flight handoff wins over
// blocked / waiting because the agent is being retired regardless of
// what it was doing. blocked wins over waiting because a hard block
// is more urgent for the operator to see than ambient idle.
//
// "review" precedence sits ABOVE "waiting" because fleet-guard sets
// NeedsInput=true on every Stop with no injected follow-up, so a
// reviewer between turns has both flags. The mode is the more
// informative signal — "this is a reviewer, currently paused" beats
// "this is some agent, currently idle". Pre-split (waiting+review →
// "review") this didn't matter; post-split, putting waiting first
// would mislabel paused reviewers as `idle` for most of their life
// (codex review for split iter: paused-reviewer regression).
//
// alive is the cached liveness snapshot from the most recent
// loadAgentsCmd. Reading from cache (instead of probing tmux here)
// keeps subprocess fan-out off the render path. A nil/missing entry
// conservatively reads as "live" rather than "dead" so a not-yet-
// probed record never falsely paints as dead before the first
// agentsMsg lands.
func deriveStatus(r *agent.Record, alive map[string]bool) string {
	if r.TmuxSession != "" {
		if probed, ok := alive[r.ID]; ok && !probed {
			return "dead"
		}
	}
	if r.HandoffType != nil {
		switch *r.HandoffType {
		case "auto-yellow", "auto-red", "precompact":
			return *r.HandoffType
		}
		// "manual" and unknown values fall through — they are not
		// in-flight indicators.
	}
	if r.Blocked {
		return "blocked"
	}
	if r.Mode == "review" {
		return "review"
	}
	if r.NeedsInput {
		if r.HasPendingQuestion {
			return "asking"
		}
		return "idle"
	}
	return "live"
}

// humanAge — same shape as cmd/fleet/status.go. Duplicated here rather
// than moved to a shared helper because (a) it's three lines, (b) the
// two callers may diverge (TUI may want "now" for <2s, status doesn't).
// Per CLAUDE.md house style: three similar lines beats a generic helper.
func humanAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// Lipgloss styles. Palette matches the v0.2 mockup. v0.1-only styles
// (titleStyle/userStyle/idStyle/taskStyle/groupHeaderStyle/detailStyle/
// coachStyle/selectedRowStyle) were deleted in issue #53 along with
// the agents-list view. Status & glyph styles remain because they
// drive the dashboard's alert banner and agent-row coloring.
var (
	dividerStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("238"))
	cursorStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("117"))
	dimStyle     = lipgloss.NewStyle().Faint(true)
	errStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	promptStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("226"))

	// Glyph styles for the leading status icon column on agent rows
	// + the alert banner.
	glyphLiveStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("78"))              // green
	glyphAskingStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("87"))   // bright cyan — needs answer
	glyphReviewStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("117"))             // soft cyan — needs review
	glyphIdleStyle    = lipgloss.NewStyle().Faint(true).Foreground(lipgloss.Color("244")) // dim — finished, ignorable
	glyphBlockedStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("208"))
	glyphDeadStyle    = lipgloss.NewStyle().Faint(true).Foreground(lipgloss.Color("244"))
	glyphHandoffStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("220"))
	glyphUrgentStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("203"))

	// Per-status colors for the STATUS label and the alert banner.
	statusLiveStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("78"))              // green — doing
	statusAskingStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("87"))   // bright cyan — asking
	statusReviewStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("117"))             // soft cyan — review
	statusIdleStyle    = lipgloss.NewStyle().Faint(true).Foreground(lipgloss.Color("244")) // dim — idle, ignorable
	statusBlockedStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("208"))             // orange
	statusDeadStyle    = lipgloss.NewStyle().Faint(true).Foreground(lipgloss.Color("244"))
	statusHandoffStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("220"))
	statusUrgentStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("203")) // red — hot context / auto-red

	// Upgrade-nudge banner. Bold cyan ⬆ glyph + light foreground for
	// the message body.
	upgradeBannerStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("117"))
)

// statusStyleFor maps a STATUS value to its lipgloss style. Falls back
// to dim for unknown values so a future status that lands on disk
// before the TUI is rebuilt still renders legibly.
func statusStyleFor(status string) lipgloss.Style {
	switch status {
	case "live":
		return statusLiveStyle
	case "asking":
		return statusAskingStyle
	case "review":
		return statusReviewStyle
	case "idle":
		return statusIdleStyle
	case "blocked":
		return statusBlockedStyle
	case "dead":
		return statusDeadStyle
	case "auto-yellow":
		return statusHandoffStyle
	case "auto-red", "precompact":
		return statusUrgentStyle
	}
	return dimStyle
}

// statusLabel maps the canonical deriveStatus value to the
// operator-facing word shown in the STATUS column. The vocabulary:
// doing / asking / idle / review / blocked / dead / handoff.
//
// "asking" (NeedsInput=true && HasPendingQuestion=true) — the agent's
// last turn ended on a question for the operator (heuristic in
// fleet-guard). The cyan ● glyph + bright cyan label calls attention.
//
// "idle" (NeedsInput=true && HasPendingQuestion=false) — the agent
// stopped, work done, no question pending. The dim ○ glyph + faint
// label tells the operator they can ignore this row at a glance.
//
// "review" (Mode == "review") — agent dispatched as a reviewer.
// Distinct cyan ● from asking; review precedence sits above the
// asking/idle split so paused reviewers stay labeled "review" even
// when fleet-guard would otherwise classify them as asking/idle.
func statusLabel(status string) string {
	switch status {
	case "live":
		return "doing"
	case "asking":
		return "asking"
	case "idle":
		return "idle"
	case "review":
		return "review"
	case "auto-yellow", "auto-red", "precompact":
		return "handoff"
	}
	return status
}
