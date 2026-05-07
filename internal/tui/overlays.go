// Modal overlays for the v0.2 dashboard: [?] help and [⏎] detail.
// Both REPLACE the dashboard body (banner + 2-col + footer) when
// active so the operator's focus is on the panel content; any
// keystroke dismisses them (handled in keys.go's handleActionKey
// overlay branch).
package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// helpEntry is one row in the [?] help overlay.
type helpEntry struct{ key, desc string }

var helpEntries = []helpEntry{
	{"j/k or ↓/↑", "move cursor up/down across all rows (wraps)"},
	{"⏎ enter", "open detail panel for the row under cursor"},
	{"n", "add a new task to the current project"},
	{"a", "attach (agents) or peek (workers)"},
	{"h", "handoff (agents only)"},
	{"x", "archive (agents) or kill (workers)"},
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
// width / height are accepted for symmetry with future clipping
// support but currently unused — the body renders fully and relies on
// the terminal's own scroll for tall content.
func renderDetailOverlay(d detailView, width, height int) string {
	_ = width
	_ = height

	var b strings.Builder
	b.WriteString(headerLabelStyle.Render("FLEET"))
	b.WriteString(headerSepStyle.Render(" — "))
	b.WriteString(headerTextStyle.Render(d.title))
	b.WriteString("\n\n")
	b.WriteString(d.body)
	if !strings.HasSuffix(d.body, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(headerSubtleStyle.Render("press [esc] or [⏎] to close"))
	b.WriteString("\n")
	return b.String()
}
