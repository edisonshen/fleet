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
}

// detailView is the inline detail panel shown by [⏎] open. The kind
// hints at the source row type (only used for the panel title); body
// is the pre-rendered multi-line text the panel displays.
//
// Issue #75: when the panel opened from a task row, we carry the
// task's project + slug so [a] inside the panel can route to the
// matching worker peek without rebuilding the row index. taskProject
// is "" for non-task panels, which the [a] interceptor reads as "fall
// through to default attach behavior".
type detailView struct {
	title       string
	body        string
	taskProject string
	taskSlug    string
}

// PendingAttach returns the tmux session to attach to after the
// program exits, or "" if no [a] was pressed. tui.Run reads this.
func (m Model) PendingAttach() string { return m.pendingAttach }

// New returns a Model ready to be passed to tea.NewProgram.
func New(version string) Model {
	return Model{
		version:   version,
		userName:  currentUserName(),
		startedAt: time.Now(),
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

	case coordSpawnDoneMsg:
		// Clear in-flight gate — regardless of success/error, this
		// dispatch attempt is done. Subsequent [a] presses go through
		// the full lookup path again (find existing → spawn).
		if m.coordSpawnInFlight != nil {
			delete(m.coordSpawnInFlight, msg.projectName)
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
			m.flash = &flashMsg{
				text:  fmt.Sprintf("project %s: %v", msg.projectName, msg.err),
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
	case "j", "down":
		m.moveCursor(+1)
	case "k", "up":
		m.moveCursor(-1)
	}
	return m, nil
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
		b.WriteString(renderDashboardFooter(time.Since(m.startedAt), usable, m.searchFilter))
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
