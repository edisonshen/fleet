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
	return Model{version: version}
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
type agentsMsg struct {
	records []*agent.Record
	err     error
	alive   map[string]bool
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
		for _, r := range records {
			if r.TmuxSession == "" {
				continue
			}
			ok, probeErr := sessionProbeFn(r.TmuxSession)
			if probeErr != nil {
				// Transport failure — leave the entry missing so the
				// dashboard reads "live" instead of mislabeling.
				continue
			}
			alive[r.ID] = ok
		}
		return agentsMsg{records: records, err: err, alive: alive}
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
		m.records = sortRecords(msg.records)
		m.aliveByID = msg.alive
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
func (m Model) View() string {
	var b strings.Builder

	// Title block: blank line above so the title isn't flush with the
	// terminal top, padded title text, dim divider underneath. Gives
	// the header a clear visual zone instead of a single dim line.
	title := fmt.Sprintf("Fleet %s", m.version)
	b.WriteString("\n")
	b.WriteString(titleStyle.Render(title))
	b.WriteString("\n")
	b.WriteString(dividerStyle.Render(strings.Repeat("─", lipgloss.Width(title)+2)))
	b.WriteString("\n\n")

	if m.err != nil {
		b.WriteString(errStyle.Render(fmt.Sprintf("error reading agents: %v", m.err)))
		b.WriteString("\n\n")
	}

	if len(m.records) == 0 {
		b.WriteString(dimStyle.Render("no agents — press [d] to dispatch one"))
		b.WriteString("\n")
	} else {
		// Alert banner above the list — only when there's something
		// urgent. A clean dashboard means no banner.
		if banner := renderAlertBanner(m.records, m.aliveByID); banner != "" {
			b.WriteString(banner)
			b.WriteString("\n")
		}
		b.WriteString(renderAgents(m.records, m.cursor, m.aliveByID))
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

	switch m.mode {
	case modePickRepo:
		b.WriteString("\n")
		b.WriteString(renderPicker(m))
	case modePromptDispatch:
		b.WriteString("\n")
		header := "dispatch task"
		if m.pickedRepo.Display != "" {
			header += " in " + m.pickedRepo.Display
		}
		b.WriteString(promptStyle.Render(header + ": " + m.promptBuf + "█"))
		b.WriteString("\n")
		b.WriteString(dimStyle.Render("[enter] submit  [esc] cancel"))
		b.WriteString("\n")
	case modeConfirmArchive:
		b.WriteString("\n")
		b.WriteString(promptStyle.Render(fmt.Sprintf(
			"Archive agent %s? Kills tmux session + deletes record (no replacement). [y/N]",
			m.archiveCandidate)))
		b.WriteString("\n")
	default:
		// Smart footer: summary line + chip row. The summary is
		// fleet-level state ("2 agents · 1 blocked"); the chips are
		// the always-available global keys. The cursor row's inline
		// chips (in renderAgents) handle context-sensitive actions —
		// the footer chips are the floor, not the ceiling.
		sep := dimStyle.Render("  ·  ")
		chips := strings.Join([]string{
			keyChip("[j/k]", "navigate"),
			keyChip("[a]", "attach"),
			keyChip("[h]", "handoff"),
			keyChip("[d]", "dispatch"),
			keyChip("[x]", "archive"),
			keyChip("[q]", "quit"),
		}, sep)
		b.WriteString("\n")
		b.WriteString(dimStyle.Render(footerSummary(m.records, m.aliveByID)))
		b.WriteString("\n")
		b.WriteString(chips)
		// Detach hint lives in the spawned session's tmux status bar
		// (see tmux.SetStatusHint), not here — by the time the
		// operator needs it, the TUI is gone and tmux owns the screen.
	}
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

// sortRecords returns a copy sorted by (project asc, spawned desc) so
// agents cluster under their project group header in the v2 layout.
// Within a group, newest-first matches `fleet status` (and matches
// the v1 behavior — for repos with one project, this is equivalent).
func sortRecords(in []*agent.Record) []*agent.Record {
	out := make([]*agent.Record, len(in))
	copy(out, in)
	sort.SliceStable(out, func(i, j int) bool {
		pi, pj := projectDisplay(out[i]), projectDisplay(out[j])
		if pi != pj {
			return pi < pj
		}
		return out[i].SpawnedAt.After(out[j].SpawnedAt)
	})
	return out
}

// renderAgents produces the grouped agent list. Project group headers
// are emitted whenever the project changes between consecutive
// records, and the cursor row gets a 2-line detail block (progress
// quote + inline action chips) under it. Records arrive pre-sorted
// by sortRecords (project asc, spawned desc) so a single pass is
// enough.
//
// alive is the cached tmux liveness snapshot from the most recent
// load. deriveStatus reads from it instead of probing tmux per-row,
// so render is pure formatting (no subprocess fan-out, no I/O).
func renderAgents(records []*agent.Record, cursor int, alive map[string]bool) string {
	idW := columnWidth(records, func(r *agent.Record) string { return r.ID }, 6)
	taskW := columnWidth(records,
		func(r *agent.Record) string { return defaultStr(r.TaskID, "-") }, 8)

	// Cap task width so a single 80-char task ID doesn't push ctx/age
	// off-screen on a 120-col terminal. Long task IDs truncate with an
	// ellipsis; the full value is still visible via [a] attach.
	if taskW > 40 {
		taskW = 40
	}

	// Group counts so the header can show "(N agents)".
	counts := map[string]int{}
	for _, r := range records {
		counts[projectDisplay(r)]++
	}

	var b strings.Builder
	lastProject := ""
	for i, r := range records {
		proj := projectDisplay(r)
		if proj != lastProject {
			if lastProject != "" {
				b.WriteString("\n") // blank line between groups
			}
			b.WriteString(renderProjectHeader(proj, counts[proj]))
			lastProject = proj
		}

		status := deriveStatus(r, alive)
		selected := i == cursor
		b.WriteString(renderAgentLine(r, status, selected, idW, taskW))
		b.WriteString("\n")
		if selected {
			b.WriteString(renderAgentDetail(r, status))
		}
	}
	return b.String()
}

// renderProjectHeader returns "rainier (2 agents)" with project
// label bold-fg and the count dim. Empty project labels show as
// "(no project)" so legacy / pre-spawn records still get a header
// instead of slipping into a nameless group.
func renderProjectHeader(name string, count int) string {
	label := name
	if label == "" || label == "-" {
		label = "(no project)"
	}
	suffix := fmt.Sprintf("(%d agent%s)", count, plural(count))
	return groupHeaderStyle.Render(label) + " " + dimStyle.Render(suffix) + "\n"
}

// renderAgentLine renders one collapsed agent row.
//
// Layout (cells, monospaced):
//
//	"▸ " | "● " | id (idW) | "  " | task (taskW) | "  " | "  68%" | "  " | "  14m" | "  " | live
//
// The 2-char gutter holds the cursor arrow on the selected row and
// is blank otherwise — keeping every row indented the same amount so
// the columns line up across the whole list.
func renderAgentLine(r *agent.Record, status string, selected bool,
	idW, taskW int) string {

	glyph, glyphStyle := glyphFor(status)
	id := padRight(r.ID, idW)
	task := truncate(defaultStr(r.TaskID, "-"), taskW)
	taskCell := padRight(task, taskW)
	ctxText, ctxStyle := formatCtxPct(r.ContextPct)
	age := padLeft(humanAge(time.Since(r.SpawnedAt)), 5)

	gutter := "  "
	if selected {
		gutter = cursorStyle.Render("▸ ")
	}
	idCell := idStyle.Render(id)
	if selected {
		idCell = cursorStyle.Render(id)
	}

	return gutter +
		glyphStyle.Render(glyph+" ") +
		idCell + "  " +
		taskCell + "  " +
		ctxStyle.Render(ctxText) + "  " +
		dimStyle.Render(age) + "  " +
		statusStyleFor(status).Render(status)
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
		return "session ended — press [x] to archive"
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
// work — a dead agent only offers [x] archive, and an in-flight
// handoff hides [h] (already in flight).
func actionChipsFor(status string) []string {
	switch status {
	case "dead":
		return []string{keyChip("[x]", "archive")}
	case "auto-yellow", "auto-red", "precompact":
		return []string{
			keyChip("[a]", "attach"),
			keyChip("[x]", "archive"),
		}
	}
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
// Counts run independently: a record can be both "blocked" AND have
// hot context, so it bumps both counts. That's intentional — the
// banner is a heads-up, not a partition.
func renderAlertBanner(records []*agent.Record, alive map[string]bool) string {
	var blocked, waiting, hot, dead int
	for _, r := range records {
		switch deriveStatus(r, alive) {
		case "blocked":
			blocked++
		case "waiting":
			waiting++
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
			fmt.Sprintf("⏸ %d blocked", blocked)))
	}
	if waiting > 0 {
		parts = append(parts, statusWaitingStyle.Render(
			fmt.Sprintf("◐ %d waiting", waiting)))
	}
	if hot > 0 {
		parts = append(parts, statusUrgentStyle.Render(
			fmt.Sprintf("⚠ %d hot context", hot)))
	}
	if dead > 0 {
		parts = append(parts, statusDeadStyle.Render(
			fmt.Sprintf("✗ %d dead", dead)))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, dimStyle.Render("  ·  ")) + "\n"
}

// footerSummary builds the one-line "N agents · M blocked" summary
// shown above the global chip row. Skips zero-counts so the line
// stays scannable.
func footerSummary(records []*agent.Record, alive map[string]bool) string {
	projects := map[string]struct{}{}
	var blocked, dead int
	for _, r := range records {
		projects[projectDisplay(r)] = struct{}{}
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
// style. The glyph palette mirrors the v2 design (● for live, ⏸ for
// blocked, etc.) so the dashboard reads the same way at a glance
// without having to parse the status word.
func glyphFor(status string) (string, lipgloss.Style) {
	switch status {
	case "live":
		return "●", glyphLiveStyle
	case "waiting":
		return "◐", glyphWaitingStyle
	case "blocked":
		return "⏸", glyphBlockedStyle
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
		return statusWaitingStyle
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

// projectDisplay derives the human-readable project label for the
// PROJECT column. Prefers the last two path segments of r.Cwd
// joined with "/" (so /Users/op/projects/fleet renders as
// "projects/fleet") instead of the on-disk Project tag, which
// hyphen-joins parent + basename for filesystem safety
// (projects-fleet) and reads as one mashed word in the dashboard.
//
// filepath.Clean drops trailing slashes and "/.“ tails before the
// Base/Dir split (codex iter-9 P3) — without it, --cwd values like
// "/path/to/repo/" or "/path/to/repo/." would derive base="repo"
// AND parent="repo" (or "."), rendering "repo/repo" or "repo/.".
//
// Falls back to r.Project — and then to "-" — for legacy records or
// agents whose Cwd wasn't captured at dispatch.
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
//  4. waiting      — needs_input=true (Stop fired, awaiting operator)
//  5. live         — fresh spawn or actively-running turn
//
// dead wins over everything because the other states are meaningless
// when the underlying process is gone. In-flight handoff wins over
// blocked / waiting because the agent is being retired regardless of
// what it was doing. blocked wins over waiting because a hard block
// is more urgent for the operator to see than ambient idle.
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
	if r.NeedsInput {
		return "waiting"
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

// Lipgloss styles. Kept in the same file as the View() that uses them
// so changes are co-located.
var (
	titleStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("63")).PaddingLeft(1).PaddingRight(1)
	dividerStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("60"))
	cursorStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("63"))
	dimStyle      = lipgloss.NewStyle().Faint(true)
	errStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	promptStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("226"))
	keyStyle      = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("220")) // yellow — action keybind chips
	keyLabelStyle = lipgloss.NewStyle().Faint(true)

	// v2 layout styles.
	idStyle          = lipgloss.NewStyle().Foreground(lipgloss.Color("81")) // cyan — agent ids stand out
	groupHeaderStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("252"))
	detailStyle      = lipgloss.NewStyle().Faint(true).Italic(true)
	coachStyle       = lipgloss.NewStyle().Faint(true).Italic(true).Foreground(lipgloss.Color("245"))

	// Glyph styles for the leading status icon column.
	glyphLiveStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	glyphWaitingStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	glyphBlockedStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))
	glyphDeadStyle    = lipgloss.NewStyle().Faint(true).Foreground(lipgloss.Color("244"))
	glyphHandoffStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("220"))
	glyphUrgentStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("196"))

	// Per-status colors for the STATUS label and the alert banner.
	// The padded plain text is built first (so column widths remain
	// correct), then wrapped in these styles — lipgloss adds
	// zero-width ANSI escapes so the alignment math is unaffected.
	statusLiveStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))  // green
	statusWaitingStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("214")) // amber
	statusBlockedStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("196"))
	statusDeadStyle    = lipgloss.NewStyle().Faint(true).Foreground(lipgloss.Color("244"))
	statusHandoffStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("220")) // yellow — handoff in flight
	statusUrgentStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("196")) // red — auto-red / precompact
)

// statusStyleFor maps a STATUS value to its lipgloss style. Falls back
// to dim for unknown values so a future status that lands on disk
// before the TUI is rebuilt still renders legibly.
func statusStyleFor(status string) lipgloss.Style {
	switch status {
	case "live":
		return statusLiveStyle
	case "waiting":
		return statusWaitingStyle
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

// keyChip renders a "[k] label" pair with a colored bracketed key and
// a dim label. Used by the footer so the operator's eye can land on
// the action keys without scanning the whole prose line.
func keyChip(key, label string) string {
	return keyStyle.Render(key) + " " + keyLabelStyle.Render(label)
}
