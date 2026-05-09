// Modal overlays for the v0.2 dashboard: [?] help and [⏎] detail.
// Both REPLACE the dashboard body (banner + 2-col + footer) when
// active so the operator's focus is on the panel content; any
// keystroke dismisses them (handled in keys.go's handleActionKey
// overlay branch).
package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// helpEntry is one row in the [?] help overlay.
type helpEntry struct{ key, desc string }

var helpEntries = []helpEntry{
	{"j / ↓", "move cursor down (wraps)"},
	{"k / ↑", "move cursor up (wraps)"},
	{"← / →", "jump cursor to first row of left (PROJECTS) / right (WORKERS) panel"},
	{"⏎ enter", "expand/collapse project (on a project row) or open detail (other rows)"},
	{"n", "add a new task to the current project"},
	{"a", "attach coord (projects), tmux (agents), peek (workers/tasks)"},
	{"h", "handoff (agents only)"},
	{"x", "archive (agents) — worker kill ships in v0.2.x"},
	{"d", "dispatch a new agent (opens repo picker)"},
	{"/", "filter dashboard rows by substring"},
	{"?", "this help"},
	{"q or ctrl+c", "quit fleet"},
}

// renderHelpOverlay returns the [?] modal content. Matches the dashboard
// header's vertical rhythm (title + spacer + table) so the panel reads
// as a transient slot inside the same layout. width is unused today
// (the help table fits any terminal ≥ 60 cells); kept in the signature
// for symmetry with renderDetailOverlay and future right-flush hints.
func renderHelpOverlay(width int) string {
	_ = width

	var b strings.Builder
	b.WriteString(headerLabelStyle.Render("FLEET"))
	b.WriteString(headerSepStyle.Render(" — "))
	b.WriteString(headerTextStyle.Render("Help"))
	b.WriteString("\n\n")

	// Find the longest key for column alignment.
	keyW := 0
	for _, e := range helpEntries {
		if w := lipgloss.Width(e.key); w > keyW {
			keyW = w
		}
	}
	for _, e := range helpEntries {
		key := footerKeyStyle.Render(padRight(e.key, keyW))
		b.WriteString("  ")
		b.WriteString(key)
		b.WriteString("  ")
		b.WriteString(headerTextStyle.Render(e.desc))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(headerSubtleStyle.Render("press any key to dismiss"))
	b.WriteString("\n")
	return b.String()
}

// renderDetailOverlay returns the [⏎] open modal content for the
// supplied detailView. Title sits at the top in the FLEET-style
// header; body is rendered verbatim (caller pre-formats so the panel
// can show JSON, markdown, log tails, etc. without re-parsing).
//
// width drives soft-wrap math (codex iter-9 P2: long log/JSON lines
// on a narrow terminal occupy more visual rows than logical lines,
// and len(lines) alone undercounts the row budget). height clips
// long bodies so the close-hint stays visible even when the body
// would otherwise scroll past the alt-screen bottom. Operators who
// need the full content of a clipped panel get a "(N more — fleet
// peek)" tail hint; bubbletea's alt-screen has no inline scroll
// affordance and adding one would require viewport state we don't
// otherwise carry.
func renderDetailOverlay(d detailView, width, height int) string {
	body := d.body
	if height > 0 {
		// Reserve: 1 banner (model.go pre-pends), 1 title, 1 blank, 1
		// blank-before-hint, 1 hint, 2 footer lines (divider + chip
		// row). 7 rows total chrome + a 1-row safety margin = 8.
		const chromeRows = 8
		bodyBudget := height - chromeRows
		if bodyBudget < 5 {
			bodyBudget = 5
		}
		// Walk lines, accumulating their wrapped-row cost via
		// visualRows() so a 200-cell-wide log entry on an 80-cell
		// terminal counts as 3 visual rows, not 1 (codex iter-9 P2).
		// Two-pass: first pass measures total cost; if it fits, no
		// truncation. Otherwise second pass cuts lines to fit
		// bodyBudget-1 (reserving the last row for the "more" hint).
		// Without the two-pass split, a body that exactly fits would
		// still get its last line clipped (codex iter-11 P2).
		lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
		total := 0
		for _, line := range lines {
			cost := visualRows(line, width)
			if cost == 0 {
				cost = 1
			}
			total += cost
		}
		if total > bodyBudget {
			used := 0
			cut := -1
			for i, line := range lines {
				cost := visualRows(line, width)
				if cost == 0 {
					cost = 1
				}
				if used+cost > bodyBudget-1 {
					cut = i
					break
				}
				used += cost
			}
			if cut >= 0 {
				more := len(lines) - cut
				lines = append(lines[:cut],
					headerSubtleStyle.Render(
						fmt.Sprintf("… %d more — `fleet peek <slug>` for full content", more)))
				body = strings.Join(lines, "\n")
			}
		}
	}

	var b strings.Builder
	b.WriteString(headerLabelStyle.Render("FLEET"))
	b.WriteString(headerSepStyle.Render(" — "))
	b.WriteString(headerTextStyle.Render(d.title))
	b.WriteString("\n\n")
	b.WriteString(body)
	if !strings.HasSuffix(body, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(headerSubtleStyle.Render("press [esc] or [⏎] to close"))
	b.WriteString("\n")
	return b.String()
}
