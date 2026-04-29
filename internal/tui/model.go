// Package tui owns the bubbletea-based interactive dashboard.
//
// The MVP shows a live-updating table of every agent record under
// ~/.fleet/agents/. fsnotify drives refreshes when files change; a 1s
// polling tick is the fallback for platforms where fsnotify misbehaves
// (per docs/DESIGN.md).
//
// Keyboard:
//   - q, ctrl+c: quit
//   - j, ↓: cursor down
//   - k, ↑: cursor up
//   - g: jump to top
//   - G: jump to bottom
//
// Out of scope for the MVP (deferred to follow-up PRs):
//   - [n] new project, [d] dispatch, [a] attach
//   - banner aggregation (⚠ N unhealthy ...)
//   - glyph column (● ✏ ⊕)
//   - project groupings
package tui

import (
	"fmt"
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

	// pendingAttach is set when [a] fires. tea.Quit returns control to
	// tui.Run, which exec's `tmux attach -t <session>` after the
	// program exits. Process replacement only works post-program — a
	// regular tea.Cmd would be inside bubbletea's altscreen and tmux
	// would draw on top of bubbletea's state.
	pendingAttach string
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
type agentsMsg struct {
	records []*agent.Record
	err     error
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
// result as an agentsMsg.
func loadAgentsCmd() tea.Cmd {
	return func() tea.Msg {
		records, err := agent.List()
		return agentsMsg{records: records, err: err}
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
func (m Model) View() string {
	var b strings.Builder

	title := fmt.Sprintf("Fleet %s", m.version)
	b.WriteString(titleStyle.Render(title))
	b.WriteString("\n\n")

	if m.err != nil {
		b.WriteString(errStyle.Render(fmt.Sprintf("error reading agents: %v", m.err)))
		b.WriteString("\n\n")
	}

	if len(m.records) == 0 {
		b.WriteString(dimStyle.Render("no agents — press [d] to dispatch one"))
		b.WriteString("\n")
	} else {
		b.WriteString(renderTable(m.records, m.cursor))
		b.WriteString("\n")
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
	default:
		footer := fmt.Sprintf("%d agent(s)  ·  [j/k] navigate  [h] handoff  [a] attach  [d] dispatch  [q] quit",
			len(m.records))
		b.WriteString("\n")
		b.WriteString(dimStyle.Render(footer))
		// Detach hint sits below the keybind row because [a] attach
		// replaces the TUI process with `tmux attach` via syscall.Exec
		// — once tmux takes the screen there's no place for fleet to
		// surface the detach key. Operators who don't know tmux's
		// Ctrl-b prefix end up stuck inside the agent's session.
		b.WriteString("\n")
		b.WriteString(dimStyle.Render(
			"after [a] attach: Ctrl-b then d detaches back to your shell — rerun `fleet` to return here"))
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

// sortRecords returns a copy sorted newest-first by SpawnedAt — same
// rule fleet status uses, so the two views agree.
func sortRecords(in []*agent.Record) []*agent.Record {
	out := make([]*agent.Record, len(in))
	copy(out, in)
	sort.Slice(out, func(i, j int) bool {
		return out[i].SpawnedAt.After(out[j].SpawnedAt)
	})
	return out
}

// renderTable produces the tabular agent list with the cursor row
// highlighted. Columns: AGENT  PROJECT  TASK  MODE  AGE  BLOCKED.
func renderTable(records []*agent.Record, cursor int) string {
	header := []string{"AGENT", "PROJECT", "TASK", "MODE", "AGE", "BLOCKED"}
	rows := make([][]string, 0, len(records))
	for _, r := range records {
		rows = append(rows, []string{
			r.ID,
			defaultStr(r.Project, "-"),
			defaultStr(r.TaskID, "-"),
			defaultStr(r.Mode, "-"),
			humanAge(time.Since(r.SpawnedAt)),
			boolStr(r.Blocked),
		})
	}
	widths := columnWidths(header, rows)

	var b strings.Builder
	b.WriteString(headerStyle.Render(joinCols(header, widths)))
	b.WriteString("\n")
	for i, row := range rows {
		line := joinCols(row, widths)
		if i == cursor {
			b.WriteString(cursorStyle.Render("▸ " + line))
		} else {
			b.WriteString("  " + line)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func joinCols(cols []string, widths []int) string {
	parts := make([]string, len(cols))
	for i, c := range cols {
		// Pad each column to its width, except the last (avoid
		// trailing whitespace).
		if i == len(cols)-1 {
			parts[i] = c
		} else {
			parts[i] = padRight(c, widths[i])
		}
	}
	return strings.Join(parts, "  ")
}

// columnWidths returns max(len) per column across header + rows.
func columnWidths(header []string, rows [][]string) []int {
	widths := make([]int, len(header))
	for i, h := range header {
		widths[i] = len(h)
	}
	for _, row := range rows {
		for i, c := range row {
			if i < len(widths) && len(c) > widths[i] {
				widths[i] = len(c)
			}
		}
	}
	return widths
}

func padRight(s string, w int) string {
	if len(s) >= w {
		return s
	}
	return s + strings.Repeat(" ", w-len(s))
}

func defaultStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func boolStr(b bool) string {
	if b {
		return "yes"
	}
	return "no"
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
	titleStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("63"))
	headerStyle = lipgloss.NewStyle().Bold(true).Faint(true)
	cursorStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("63"))
	dimStyle    = lipgloss.NewStyle().Faint(true)
	errStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	promptStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("226"))
)
