// Package tui owns the bubbletea-based interactive dashboard.
//
// Renders agent records under ~/.fleet/agents/ as a grouped list.
// fsnotify drives refreshes when files change; a 1s polling tick
// is the fallback for platforms where fsnotify misbehaves
// (per docs/DESIGN.md).
//
// Layout (v2 — "the selected row IS the interface"):
//
//	Fleet 0.1.0
//	────────────
//	⏸ 1 blocked  ·  ⚠ 2 hot context              <- alert banner (only if any)
//
//	rainier (1 agent)                            <- project group header
//	  ●  agent01   add-rate-limiting     68%  14m  live
//	▸ ⏸  agent02   rec-engine-v2         41%   6m  blocked     <- selected
//	       ⏸ "which similarity metric — cosine or jaccard?"
//	       [a] attach   [h] handoff   [x] archive
//
//	use j/k to navigate · actions appear on the selected row   <- coach hint
//
//	2 agents · 1 blocked
//	[j/k] navigate  [a] attach  [h] handoff  [d] dispatch  [x] archive  [q] quit
//
// Keyboard:
//   - q, ctrl+c: quit
//   - j, ↓: cursor down
//   - k, ↑: cursor up
//   - g: jump to top
//   - G: jump to bottom
//   - h: handoff selected agent
//   - a: attach to selected agent
//   - d, n: dispatch (opens repo picker)
//   - x: archive selected agent (confirm with y)
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
)

// Model is the bubbletea state for the dashboard.
type Model struct {
	records []*agent.Record
	cursor  int
	err     error
	width   int
	height  int

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

	// coachDismissed flips true after the first nav/action keypress so
	// the coaching hint ("use j/k to navigate · actions appear on the
	// selected row") fades out once the operator has demonstrated they
	// know what they're doing. In-memory only — fresh launches re-show
	// the hint, which is the right default for a CLI that's still in
	// early adoption.
	coachDismissed bool
}

// PendingAttach returns the tmux session to attach to after the
// program exits, or "" if no [a] was pressed. tui.Run reads this.
func (m Model) PendingAttach() string { return m.pendingAttach }

// New returns a Model ready to be passed to tea.NewProgram.
func New(version string) Model {
	return Model{version: version, userName: currentUserName()}
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

// Init is the bubbletea entry point. We kick off the first agent load
// and start the 1s polling tick. fsnotify is wired in tui.go's Run.
func (m Model) Init() tea.Cmd {
	return tea.Batch(loadAgentsCmd(), tickCmd())
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

const pollInterval = 1 * time.Second

func tickCmd() tea.Cmd {
	return tea.Tick(pollInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
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
		m.err = msg.err
		m.aliveByID = msg.alive
		m.groupKeysByID = msg.groupKeys
		m.records = sortRecordsBy(msg.records, msg.groupKeys)
		// Keep the cursor in bounds when the list shrinks.
		if m.cursor >= len(m.records) {
			m.cursor = max(0, len(m.records)-1)
		}
		return m, nil

	case tickMsg:
		// Polling fallback: re-read agents/ every pollInterval, then
		// schedule the next tick.
		return m, tea.Batch(loadAgentsCmd(), tickCmd())

	case fsEventMsg:
		// fsnotify saw a change — refresh agents now (don't wait for
		// the next tick).
		return m, loadAgentsCmd()

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

	// Coach hint fades on the first nav-mode keypress. We gate on
	// modeNav so picker filter typing doesn't (re-)dismiss something
	// that's already gone, and so the dismissal corresponds 1:1 with
	// the operator demonstrating they know how to interact with the
	// dashboard.
	if m.mode == modeNav {
		m.coachDismissed = true
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
		if m.cursor < len(m.records)-1 {
			m.cursor++
		}
	case "k", "up":
		if m.cursor > 0 {
			m.cursor--
		}
	case "g":
		m.cursor = 0
	case "G":
		if len(m.records) > 0 {
			m.cursor = len(m.records) - 1
		}
	}
	return m, nil
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
	top := m.renderTop()
	footer := m.renderFooter()
	return padToBottom(top, footer, m.height, m.width)
}

// renderTop returns everything above the footer: title block,
// optional error, alert banner (with section divider), agent list,
// coach hint, flash. Always ends with a trailing newline.
func (m Model) renderTop() string {
	var b strings.Builder

	// Title row: "Fleet x.y.z" left, username right. Leading blank
	// line keeps the title from sitting on the very first row of
	// the alt-screen — some terminals (Warp, certain tmux configs)
	// overlay command/status chrome on row 1 and the title vanishes
	// behind it. Pushing the title to row 2 makes it always visible
	// regardless of host UI.
	title := fmt.Sprintf("Fleet %s", m.version)
	b.WriteString("\n")
	b.WriteString(titleRow(title, m.userName, m.width))
	b.WriteString("\n")
	b.WriteString(dividerStyle.Render(divider(m.width, lipgloss.Width(title)+2)))
	b.WriteString("\n")

	if m.err != nil {
		b.WriteString(errStyle.Render(fmt.Sprintf("error reading agents: %v", m.err)))
		b.WriteString("\n\n")
	}

	if len(m.records) == 0 {
		b.WriteString("\n")
		b.WriteString(dimStyle.Render("no agents — press [d] to dispatch one"))
		b.WriteString("\n")
	} else {
		// Alert banner: sits inside its own divider-bracketed section
		// so it reads as a dashboard-level summary, not part of the
		// agent list. Skipped entirely on a clean dashboard.
		if banner := renderAlertBanner(m.records, m.aliveByID); banner != "" {
			b.WriteString(banner)
			b.WriteString(dividerStyle.Render(divider(m.width, 0)))
			b.WriteString("\n")
		}
		b.WriteString(renderAgents(m.records, m.cursor, m.aliveByID, m.groupKeysByID, m.width))
		if !m.coachDismissed {
			b.WriteString("\n")
			b.WriteString(coachStyle.Render(
				"  use j/k to navigate · actions appear on the selected row"))
			b.WriteString("\n")
		}
	}

	if m.flash != nil {
		style := dimStyle
		if m.flash.isErr {
			style = errStyle
		}
		b.WriteString("\n")
		b.WriteString(style.Render(m.flash.text))
		b.WriteString("\n")
	}
	return b.String()
}

// renderFooter returns the bottom-of-screen block — either a mode
// prompt (picker / dispatch / confirm) or the smart footer (divider +
// summary + chip row). Always opens with a divider line so the
// footer reads as its own section pinned to the terminal bottom.
func (m Model) renderFooter() string {
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
	default:
		// Smart footer: summary line + chip row. The summary is
		// fleet-level state ("4 projects · 4 agents · 1 blocked");
		// the chips are the always-available global keys. The
		// cursor row's inline chips (renderAgentDetail) carry the
		// context-sensitive row actions — the footer is the floor,
		// not the ceiling. Two-space separator + compact "[k]label"
		// chips match the v2 mockup.
		chips := strings.Join([]string{
			keyChip("[j/k]", "navigate"),
			keyChip("[a]", "attach"),
			keyChip("[d]", "dispatch new"),
			keyChip("[q]", "quit"),
		}, "  ")
		b.WriteString(dimStyle.Render(footerSummary(m.records, m.aliveByID, m.groupKeysByID)))
		b.WriteString("\n")
		b.WriteString(chips)
		b.WriteString("\n")
		// Detach hint lives in the spawned session's tmux status bar
		// (see tmux.SetStatusHint), not here — by the time the
		// operator needs it, the TUI is gone and tmux owns the screen.
	}
	return b.String()
}

// titleRow renders the top line: bold cyan "Fleet x.y.z" on the left,
// faint username on the right, padded with spaces between them.
// Stops one cell short of width so the rightmost terminal column
// stays empty — many terminals auto-wrap when content lands in the
// final column, and the resulting phantom newline visually eats the
// row. When width is unknown (early renders) or username is empty,
// falls back to just the title — keeping the row stable as the
// terminal reports its size in.
func titleRow(title, name string, width int) string {
	left := titleStyle.Render(title)
	if name == "" || width <= 0 {
		return left
	}
	right := userStyle.Render(name)
	// width-1 leaves a 1-cell margin so the row never lands a
	// printable character in the final column.
	gap := (width - 1) - lipgloss.Width(title) - lipgloss.Width(name)
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right
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

// statusColW pins the rendered status word to a fixed cell width so
// every row's right block is the same total width — the percent and
// age columns then align across rows instead of drifting under
// shorter words like "doing". 7 covers the longest known label
// ("blocked", "handoff", "planned"); a one-off longer label gets
// truncated by the row, not rejected.
const statusColW = 7

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

// renderAgents produces the grouped agent list. Project group headers
// are emitted whenever the project changes between consecutive
// records, and the cursor row gets a 2-line detail block (progress
// quote + inline action chips) under it. Records arrive pre-sorted
// by sortRecords (project asc, spawned desc) so a single pass is
// enough.
//
// width is the terminal width; the selected row's background tint is
// padded to it so the highlight reads as a full-width band. When 0
// (no WindowSizeMsg yet), the highlight falls back to inline-only
// styling — degrading gracefully instead of collapsing to a 0-cell
// row.
//
// alive is the cached tmux liveness snapshot from the most recent
// load. deriveStatus reads from it instead of probing tmux per-row,
// so render is pure formatting (no subprocess fan-out, no I/O).
func renderAgents(records []*agent.Record, cursor int,
	alive map[string]bool, groupKeys map[string]string, width int) string {

	idW := columnWidth(records, func(r *agent.Record) string { return r.ID }, 6)
	taskW := columnWidth(records,
		func(r *agent.Record) string { return defaultStr(r.TaskID, "-") }, 8)

	// Cap task width so a single 80-char task ID doesn't push ctx/age
	// off-screen on a 120-col terminal. Long task IDs truncate with an
	// ellipsis; the full value is still visible via [a] attach.
	if taskW > 40 {
		taskW = 40
	}

	// Group counts so the header can show "(N tasks, M active)".
	// Counts key on projectGroupKey (cwd path / Project tag) so
	// display-only differences don't fragment a group; the header
	// label still uses projectDisplay for the human-readable form.
	//
	// "tasks" deduplicates by TaskID so a planner+executor pair on
	// the same task counts as 1, not 2 (codex iter-5 P2). Records
	// with empty TaskID each count as a distinct task — operator
	// dispatched without a task id and we have nothing to merge on.
	// "active" = task IDs whose any record isn't dead.
	type groupCounts struct {
		seenTasks   map[string]struct{}
		activeTasks map[string]struct{}
		anonTotal   int
		anonActive  int
	}
	counts := map[string]*groupCounts{}
	for _, r := range records {
		key := groupKeyFor(r, groupKeys)
		gc := counts[key]
		if gc == nil {
			gc = &groupCounts{
				seenTasks:   map[string]struct{}{},
				activeTasks: map[string]struct{}{},
			}
			counts[key] = gc
		}
		notDead := deriveStatus(r, alive) != "dead"
		if r.TaskID == "" {
			gc.anonTotal++
			if notDead {
				gc.anonActive++
			}
			continue
		}
		gc.seenTasks[r.TaskID] = struct{}{}
		if notDead {
			gc.activeTasks[r.TaskID] = struct{}{}
		}
	}

	var b strings.Builder
	lastKey := ""
	for i, r := range records {
		key := groupKeyFor(r, groupKeys)
		if key != lastKey {
			if lastKey != "" {
				b.WriteString("\n") // blank line between groups
			}
			gc := counts[key]
			total := len(gc.seenTasks) + gc.anonTotal
			active := len(gc.activeTasks) + gc.anonActive
			b.WriteString(renderProjectHeader(projectDisplay(r), total, active))
			lastKey = key
		}

		status := deriveStatus(r, alive)
		selected := i == cursor
		row := renderAgentLine(r, status, selected, idW, taskW, width)
		if !selected {
			b.WriteString(row)
			b.WriteString("\n")
			continue
		}
		// Selected: pin row + detail block under a dark-blue
		// highlight. lipgloss handles ANSI-reset reapplication so
		// the internal fg colors survive the wrapper.
		//
		// width-1 leaves the same 1-cell right margin titleRow and
		// divider use — padding to the full terminal width writes
		// into the final column and re-triggers the phantom-newline
		// auto-wrap that adds a stray row to the highlight, which
		// throws off padToBottom's height accounting and slides the
		// footer off-screen.
		detail := renderAgentDetail(r, status)
		block := row + "\n" + strings.TrimRight(detail, "\n")
		if width > 1 {
			b.WriteString(selectedRowStyle.Width(width - 1).Render(block))
		} else {
			b.WriteString(block)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// renderProjectHeader returns "rainier (3 tasks, 1 active)" with the
// project label bold-fg and the count dim. Empty project labels show
// as "(no project)" so legacy / pre-spawn records still get a header
// instead of slipping into a nameless group.
func renderProjectHeader(name string, total, active int) string {
	label := name
	if label == "" || label == "-" {
		label = "(no project)"
	}
	suffix := fmt.Sprintf("(%d task%s, %d active)", total, plural(total), active)
	return groupHeaderStyle.Render(label) + " " + dimStyle.Render(suffix) + "\n"
}

// renderAgentLine renders one collapsed agent row.
//
// Layout (cells, monospaced):
//
//	"▶ " | "● " | id | "  " | task   <flex filler>   "  90%" "  14m" "  doing"
//
// The right-side stat columns (ctx % / age / status word) right-align
// to the terminal edge: a calculated filler of spaces sits between
// the task cell and the right block so percent/age/status anchor at
// width-of-terminal. When the terminal width is unknown (early
// render before WindowSizeMsg lands) we fall back to a single-gap
// layout — nothing is right-aligned, but the row still renders.
//
// The 2-char gutter holds the cursor arrow on the selected row and
// is blank otherwise. Cursor glyph "▶" + bold cyan matches the v2
// mockup.
func renderAgentLine(r *agent.Record, status string, selected bool,
	idW, taskW, width int) string {

	glyph, glyphStyle := glyphFor(status)
	id := padRight(r.ID, idW)
	task := truncate(defaultStr(r.TaskID, "-"), taskW)
	ctxText, ctxStyle := formatCtxPct(r.ContextPct)
	age := padLeft(humanAge(time.Since(r.SpawnedAt)), 5)
	label := statusLabel(status)

	gutter := "  "
	if selected {
		gutter = cursorStyle.Render("▶ ")
	}

	// Left half: gutter + glyph + id + 2-space gap + task name.
	// Task isn't padded to taskW here — width-based filler does the
	// alignment instead, so short task names don't drag the right
	// columns inward.
	left := gutter +
		glyphStyle.Render(glyph+" ") +
		idStyle.Render(id) + "  " +
		taskStyle.Render(task)

	// Right half: ctx % (5 cells) + 2-space gap + age (5 cells) +
	// 2-space gap + status (statusColW cells). Status padded so the
	// total right-block width is identical across rows — otherwise
	// "doing" rows (5) and "blocked" rows (7) push the percent
	// column to different offsets and the columns visibly drift.
	right := ctxStyle.Render(ctxText) + "  " +
		dimStyle.Render(age) + "  " +
		statusStyleFor(status).Render(padRight(label, statusColW))

	// Plain widths (cells, ignoring ANSI escapes) drive the filler
	// math. width-1 leaves a 1-cell right margin so the status word
	// never lands in the terminal's final column — see titleRow for
	// the phantom-newline rationale. The selected row's bg highlight
	// is applied by the caller at the Width(width) wrapper, so we
	// don't need to compensate for it here.
	leftW := lipgloss.Width(left)
	rightW := lipgloss.Width(right)
	gap := (width - 1) - leftW - rightW
	if width <= 0 || gap < 2 {
		gap = 2 // narrow terminals + early renders fall back to a flat gap
	}
	return left + strings.Repeat(" ", gap) + right
}

// renderAgentDetail returns the 2-line block under the selected row:
// progress/status quote and inline action chips. Hangs off the row
// at a 7-char indent (gutter + glyph + ~2 chars into id) so it
// visually subordinates to the row above.
func renderAgentDetail(r *agent.Record, status string) string {
	const indent = "       " // 7 cells

	var b strings.Builder
	if line := agentProgressLine(r, status); line != "" {
		b.WriteString(indent)
		b.WriteString(detailStyle.Render(line))
		b.WriteString("\n")
	}
	chips := strings.Join(actionChipsFor(status), "  ")
	b.WriteString(indent)
	b.WriteString(chips)
	b.WriteString("\n")
	return b.String()
}

// agentProgressLine builds the contextual one-liner under the
// selected row. Blocked agents get their reason in quotes (or a
// generic prompt if no reason was recorded); dead agents get an
// archive hint; otherwise we summarize last-activity age, mode, and
// handoff count.
func agentProgressLine(r *agent.Record, status string) string {
	switch status {
	case "dead":
		return "session ended — press [h] to recover or [x] to archive"
	case "blocked":
		if r.BlockedReason != nil && strings.TrimSpace(*r.BlockedReason) != "" {
			return "⏸ \"" + strings.TrimSpace(*r.BlockedReason) + "\""
		}
		return "⏸ blocked — needs your input"
	case "auto-yellow", "auto-red", "precompact":
		return "⊕ handoff in flight (" + status + ")"
	}
	var parts []string
	if !r.LastActivityTS.IsZero() {
		parts = append(parts, "active "+humanAge(time.Since(r.LastActivityTS))+" ago")
	}
	if r.Mode != "" {
		parts = append(parts, r.Mode)
	}
	if r.HandoffNumber > 0 {
		parts = append(parts, fmt.Sprintf("handoff #%d", r.HandoffNumber))
	}
	return strings.Join(parts, " · ")
}

// actionChipsFor returns the inline action chips shown on the
// selected row. Status-aware so we don't dangle keys that won't
// work.
//
// auto-yellow keeps the full chip set because the queue journal
// isn't written until the agent reaches MILESTONE. In the window
// between "HANDOFF REQUESTED" being injected and that journal
// landing, both `fleet handoff` and `fleet rm` still work
// (cmd/fleet/rm.go:99 only refuses when the journal file exists).
// [h] is the operator's escape hatch when the auto-handoff
// stalls — observed when the agent goes idle after Yellow fires
// without ever taking another turn to emit MILESTONE; without
// [h] the agent stays stuck in auto-yellow forever.
//
// auto-red and precompact happen after MILESTONE → journal exists
// → both `fleet handoff` and `fleet rm` will refuse → hide them.
//
// Dead agents keep [h] AND [x] because either could be the right
// recovery path: if a journal landed before the session died, only
// `fleet handoff` (or `fleet drain`) resumes recovery; otherwise
// `fleet rm` cleans up. Showing both lets the operator pick —
// agentProgressLine surfaces the same hint inline.
func actionChipsFor(status string) []string {
	switch status {
	case "dead":
		return []string{
			keyChip("[h]", "handoff"),
			keyChip("[x]", "archive"),
		}
	case "auto-red", "precompact":
		return []string{keyChip("[a]", "attach")}
	}
	// auto-yellow falls through to the default (full chip set) — see
	// comment above.
	return []string{
		keyChip("[a]", "attach"),
		keyChip("[h]", "handoff"),
		keyChip("[x]", "archive"),
	}
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

// footerSummary builds the one-line "N projects · M agents · K blocked"
// summary shown above the global chip row. Project count keys on
// the cached projectGroupKey so display tweaks don't change the
// count, matching the renderAgents grouping.
func footerSummary(records []*agent.Record, alive map[string]bool, groupKeys map[string]string) string {
	projects := map[string]struct{}{}
	var blocked, dead int
	for _, r := range records {
		projects[groupKeyFor(r, groupKeys)] = struct{}{}
		switch deriveStatus(r, alive) {
		case "blocked":
			blocked++
		case "dead":
			dead++
		}
	}
	parts := []string{
		fmt.Sprintf("%d project%s", len(projects), plural(len(projects))),
		fmt.Sprintf("%d agent%s", len(records), plural(len(records))),
	}
	if blocked > 0 {
		parts = append(parts, fmt.Sprintf("%d blocked", blocked))
	}
	if dead > 0 {
		parts = append(parts, fmt.Sprintf("%d dead", dead))
	}
	return strings.Join(parts, " · ")
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

// formatCtxPct returns the right-aligned 5-cell context-percent text
// and the health-tinted style. Renders "    —" when the agent has no
// context source yet — same column footprint, no number.
func formatCtxPct(p *float64) (string, lipgloss.Style) {
	if p == nil {
		return "    —", dimStyle
	}
	pct := int(*p)
	if pct > 999 {
		pct = 999 // clamp absurd values without breaking the column width
	}
	return fmt.Sprintf("%4d%%", pct), ctxColorFor(*p)
}

// ctxColorFor maps a context percentage to its health color.
// Thresholds match fleet-guard: <50 healthy, 50-69 warning, ≥70 hot.
func ctxColorFor(p float64) lipgloss.Style {
	switch {
	case p < 50:
		return statusLiveStyle
	case p < 70:
		return statusBlockedStyle // orange — warning band, not yet hot
	default:
		return statusUrgentStyle
	}
}

// columnWidth returns max(len(extract(r))) across records, floored
// to min. Uses lipgloss.Width so multi-byte runes count their cell
// span, not their byte length.
func columnWidth(records []*agent.Record, extract func(*agent.Record) string, min int) int {
	w := min
	for _, r := range records {
		if n := lipgloss.Width(extract(r)); n > w {
			w = n
		}
	}
	return w
}

func padRight(s string, w int) string {
	cur := lipgloss.Width(s)
	if cur >= w {
		return s
	}
	return s + strings.Repeat(" ", w-cur)
}

func padLeft(s string, w int) string {
	cur := lipgloss.Width(s)
	if cur >= w {
		return s
	}
	return strings.Repeat(" ", w-cur) + s
}

// truncate clips s to w cells, replacing the tail with "…" when it
// overflows. Width is cell-based so multi-byte runes don't undercount.
func truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= w {
		return s
	}
	// Walk runes from the start until the next rune would push us
	// past w-1 cells, then append "…" to use the last cell.
	out := make([]rune, 0, w)
	used := 0
	for _, r := range s {
		rw := lipgloss.Width(string(r))
		if used+rw > w-1 {
			break
		}
		out = append(out, r)
		used += rw
	}
	return string(out) + "…"
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
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

// projectDisplay derives the human-readable project label for the
// PROJECT column and the project-group header. Prefers the last two
// path segments of r.Cwd joined with "/" — /Users/op/projects/fleet
// renders as "projects/fleet" — so dashboards from typical
// ~/projects/<repo> layouts read like file paths.
//
// filepath.Clean drops trailing slashes and "/." tails before the
// Base/Dir split (codex iter-9 P3) — without it, --cwd values like
// "/path/to/repo/" or "/path/to/repo/." would derive base="repo"
// AND parent="repo" (or "."), rendering "repo/repo" or "repo/.".
//
// Fallback (no Cwd captured): r.Project as-is. r.Project is
// operator-set via `--project` (default "default") and never
// auto-derived from the cwd — so legacy records and new records
// from the same checkout DON'T inherently disagree. They DO
// disagree when an operator explicitly chose `--project foo-bar`
// for a checkout at /path/to/foo/bar (rare); that case lands in
// two project headers, which is honest because the Project tag
// truly differs.
func projectDisplay(r *agent.Record) string {
	if r.Cwd != "" {
		clean := filepath.Clean(r.Cwd)
		base := filepath.Base(clean)
		parent := filepath.Base(filepath.Dir(clean))
		if parent != "" && parent != "." && parent != string(filepath.Separator) {
			return parent + "/" + base
		}
		if base != "" && base != "." {
			return base
		}
	}
	return defaultStr(r.Project, "-")
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

// Lipgloss styles. Palette matches the v2 mockup screenshot —
// soft cyan title, orange/red/cyan alert glyphs, yellow action
// chips, dim slate dividers. Co-located with View() so changes
// are easy to scan.
var (
	titleStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("117")) // soft cyan
	userStyle    = lipgloss.NewStyle().Faint(true).Foreground(lipgloss.Color("245"))
	dividerStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("238"))
	cursorStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("117"))
	dimStyle     = lipgloss.NewStyle().Faint(true)
	errStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	promptStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("226"))
	// Cyan chip key + light foreground label. Bare-foreground (no
	// Faint) keeps the labels readable against the selected row's
	// dark-blue background. Cyan keys match the title color in the
	// v2 mockup — yellow read as alert, not as affordance.
	keyStyle      = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("117"))
	keyLabelStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))

	// v2 layout styles.
	idStyle          = lipgloss.NewStyle().Foreground(lipgloss.Color("245")) // dim — IDs are anchors, not focal
	taskStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("252")) // task name reads at default fg
	groupHeaderStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("252"))
	detailStyle      = lipgloss.NewStyle().Faint(true).Italic(true)
	coachStyle       = lipgloss.NewStyle().Faint(true).Italic(true).Foreground(lipgloss.Color("245"))

	// Selected row gets a subtle dark-blue background that spans the
	// full terminal width. Width is applied at render time (m.width),
	// not on the style itself.
	selectedRowStyle = lipgloss.NewStyle().Background(lipgloss.Color("237"))

	// Glyph styles for the leading status icon column.
	glyphLiveStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("78"))              // green
	glyphAskingStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("87"))   // bright cyan — needs answer
	glyphReviewStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("117"))             // soft cyan — needs review
	glyphIdleStyle    = lipgloss.NewStyle().Faint(true).Foreground(lipgloss.Color("244")) // dim — finished, ignorable
	glyphBlockedStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("208"))
	glyphDeadStyle    = lipgloss.NewStyle().Faint(true).Foreground(lipgloss.Color("244"))
	glyphHandoffStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("220"))
	glyphUrgentStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("203"))

	// Per-status colors for the STATUS label and the alert banner.
	// The padded plain text is built first (so column widths remain
	// correct), then wrapped in these styles — lipgloss adds
	// zero-width ANSI escapes so the alignment math is unaffected.
	statusLiveStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("78"))              // green — doing
	statusAskingStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("87"))   // bright cyan — asking
	statusReviewStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("117"))             // soft cyan — review
	statusIdleStyle    = lipgloss.NewStyle().Faint(true).Foreground(lipgloss.Color("244")) // dim — idle, ignorable
	statusBlockedStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("208"))             // orange
	statusDeadStyle    = lipgloss.NewStyle().Faint(true).Foreground(lipgloss.Color("244"))
	statusHandoffStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("220"))
	statusUrgentStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("203")) // red — hot context / auto-red
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

// keyChip renders a "[k]label" pair with a colored bracketed key and
// a foreground label. Mockup uses [a]attach (no space) so the chips
// read as compact tokens, not "key + descriptor".
func keyChip(key, label string) string {
	return keyStyle.Render(key) + keyLabelStyle.Render(label)
}
