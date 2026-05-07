// Variant A "Ops Console" dashboard renderer.
//
// Layout (mockup-driven; see tui-mockup-A.sh):
//
//	┌─ FLEET ───────────────────────────────────────────┐
//	│ N projects · M need attention · K ci running · …  │
//	├─────────────────────────────┬─────────────────────┤
//	│ PROJECTS · N ACTIVE         │ WORKERS · M ACTIVE  │
//	│                             │                     │
//	│   <project block>           │   ● <worker row>    │
//	│   <project block>           │   ● <worker row>    │
//	├─────────────────────────────┴─────────────────────┤
//	│ [j/k] nav  [⏎] open  …          uptime HH:MM      │
//	└───────────────────────────────────────────────────┘
//
// Pure formatter — no I/O. Reads m.dashboard (loaded by
// loadDashboardCmd) and renders into a string. Width budget splits
// 64/36 left/right, matching the mockup's 73/37 column ratio rounded
// to a sane minimum on narrow terminals.
package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// renderDashboard returns the full Ops Console view (header strip,
// 2-col body, footer) as a single string. width is the terminal width
// reported by tea.WindowSizeMsg; falls back to a sensible 110 when 0
// (early renders before the size message lands).
func renderDashboard(m Model) string {
	w := m.width
	if w <= 0 {
		w = 110
	}
	// 1-cell right margin matches the v0.1 layout's anti-wrap rule
	// (titleRow / divider). Anything wider triggers phantom-newline
	// wrap on some terminals.
	usable := w - 1
	if usable < 60 {
		usable = 60
	}

	leftW, rightW := splitColumns(usable)

	var b strings.Builder
	b.WriteString(renderDashboardHeader(m, usable))
	b.WriteString("\n")
	b.WriteString(renderColumnHeadings(leftW, rightW, m.dashboard))
	b.WriteString("\n")
	b.WriteString(renderTwoColumnBody(m, leftW, rightW))
	return b.String()
}

// splitColumns returns (leftWidth, rightWidth) summing to usable.
// Mockup ratio: ~64% projects / ~36% workers. We round to even widths
// so the column separator falls on a single cell.
func splitColumns(usable int) (int, int) {
	left := usable * 64 / 100
	if left < 30 {
		left = 30
	}
	right := usable - left - 1 // 1 cell for the │ separator
	if right < 20 {
		right = 20
	}
	return left, right
}

// renderDashboardHeader renders the top "FLEET — N projects · …" strip.
// Single line; pinned styles match the mockup's bright/dim/red/amber
// palette.
func renderDashboardHeader(m Model, usable int) string {
	snap := m.dashboard
	pCount, wCount, aCount, ciCount := 0, 0, 0, 0
	if snap != nil {
		pCount = len(snap.Projects)
		wCount = len(snap.Workers)
		aCount = snap.AttentionProjects()
		ciCount = snap.CIRunning()
	}

	// Build the left segments. Using lipgloss.JoinHorizontal-style
	// concatenation keeps the cell math correct because lipgloss
	// width counts cells, not bytes.
	dot := headerSepStyle.Render(" · ")
	parts := []string{
		headerLabelStyle.Render(fmt.Sprintf("%d", pCount)) + headerTextStyle.Render(" projects"),
	}
	if aCount > 0 {
		parts = append(parts, headerAttnStyle.Render(fmt.Sprintf("%d need attention", aCount)))
	}
	if ciCount > 0 {
		parts = append(parts, headerCIStyle.Render(fmt.Sprintf("%d ci running", ciCount)))
	}
	parts = append(parts, headerLabelStyle.Render(fmt.Sprintf("%d", wCount))+headerTextStyle.Render(" workers active"))
	left := strings.Join(parts, dot)

	// Right side: "user · vX.Y.Z". userName is captured at New().
	right := ""
	if m.userName != "" {
		right = headerSubtleStyle.Render(m.userName + " · " + m.version)
	} else if m.version != "" {
		right = headerSubtleStyle.Render(m.version)
	}

	gap := usable - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	row := left + strings.Repeat(" ", gap) + right
	// Title prefix "FLEET" anchors the row visually. Render as a
	// separate line above the totals strip — matches mockup vertical
	// rhythm without a box-drawing border (cleaner integration with
	// the existing v0.1 title row idiom).
	title := headerLabelStyle.Render("FLEET") + headerSepStyle.Render(" — ") + headerTextStyle.Render("v0.2 Ops Console")
	return title + "\n" + row
}

// renderColumnHeadings renders the "PROJECTS · 3 ACTIVE" /
// "WORKERS · 5 ACTIVE" strip below the totals.
func renderColumnHeadings(leftW, rightW int, snap *Snapshot) string {
	pn, wn := 0, 0
	if snap != nil {
		pn = len(snap.Projects)
		wn = len(snap.Workers)
	}
	leftLabel := columnHeadingStyle.Render(fmt.Sprintf("PROJECTS · %d ACTIVE", pn))
	rightLabel := columnHeadingStyle.Render(fmt.Sprintf("WORKERS · %d ACTIVE", wn))

	// Pad each label to its column width minus a 2-cell indent that
	// matches the body rows.
	left := "  " + leftLabel + strings.Repeat(" ", maxInt(0, leftW-lipgloss.Width(leftLabel)-2))
	right := "  " + rightLabel + strings.Repeat(" ", maxInt(0, rightW-lipgloss.Width(rightLabel)-2))
	sep := boxBorderStyle.Render("│")
	return left + sep + right
}

// renderTwoColumnBody composes the left + right column lines and
// joins them side-by-side at row level. When one column has fewer
// rows the shorter one is bottom-padded with blank lines so the
// separator stays uniform.
func renderTwoColumnBody(m Model, leftW, rightW int) string {
	leftLines := buildProjectLines(m.dashboard, leftW)
	rightLines := buildWorkerLines(m.dashboard, rightW)

	maxRows := len(leftLines)
	if len(rightLines) > maxRows {
		maxRows = len(rightLines)
	}
	if maxRows < 6 {
		maxRows = 6 // keep a minimum body height so the box doesn't collapse
	}

	sep := boxBorderStyle.Render("│")
	var b strings.Builder
	for i := 0; i < maxRows; i++ {
		l := ""
		r := ""
		if i < len(leftLines) {
			l = leftLines[i]
		}
		if i < len(rightLines) {
			r = rightLines[i]
		}
		// Pad each side to its column width on plain-cell basis.
		l = padPlain(l, leftW)
		r = padPlain(r, rightW)
		b.WriteString(l)
		b.WriteString(sep)
		b.WriteString(r)
		b.WriteString("\n")
	}
	return b.String()
}

// buildProjectLines produces the rendered text lines for the projects
// column (one project = 3 lines + a blank). leftW is the column's
// cell width budget.
func buildProjectLines(snap *Snapshot, leftW int) []string {
	if snap == nil || len(snap.Projects) == 0 {
		return []string{
			"",
			columnHeadingStyle.Render("  no projects yet — run `fleet tasks add` in a repo"),
		}
	}
	var lines []string
	for _, p := range snap.Projects {
		lines = append(lines, projectBlockLines(p, leftW)...)
	}
	return lines
}

// projectBlockLines renders one project's three-line block:
//
//	<name>                                     [● N attn]
//	<repo>
//	⏳ N ▶ N 👁 N ⚠ N ✓ N    ● active     last-tick
//
// Attention rows get a leading "▌ " accent (red bold) on every line of
// the block to mirror the mockup's left-border treatment.
func projectBlockLines(p *ProjectRow, w int) []string {
	prefix := "  "
	if p.Attention > 0 {
		prefix = attentionBorderStyle.Render("▌ ") + " "
	}

	// Line 1: name + (right-flushed) attention chip.
	name := projectNameStyle.Render(p.Name)
	var attnRight string
	if p.Attention > 0 {
		attnRight = attentionChipStyle.Render(fmt.Sprintf("● %d attn", p.Attention))
	}
	line1 := prefix + name
	if attnRight != "" {
		gap := w - lipgloss.Width(line1) - lipgloss.Width(attnRight) - 2
		if gap < 1 {
			gap = 1
		}
		line1 = line1 + strings.Repeat(" ", gap) + attnRight
	}

	// Line 2: repo slug, dim. When no standards.md provided a slug we
	// fall back to the bare project name, which would render twice in
	// a row. Drop the duplicate to keep the block compact.
	line2 := prefix
	if p.RepoSlug != "" && p.RepoSlug != p.Name {
		line2 += projectRepoStyle.Render(p.RepoSlug)
	}

	// Line 3: counts + status.
	counts := renderCountChips(p.Counts)
	status := renderCoordStatus(p)
	line3 := prefix + counts
	if status != "" {
		gap := w - lipgloss.Width(line3) - lipgloss.Width(status) - 2
		if gap < 1 {
			gap = 1
		}
		line3 = line3 + strings.Repeat(" ", gap) + status
	}

	return []string{line1, line2, line3, ""}
}

// renderCountChips builds "⏳ 3  ▶ 1  👁 1  ⚠ 1  ✓ 12" with the
// per-status colors. Zero counts are omitted so the row stays compact
// — except for ⏳ todo + ✓ done which always show because they're the
// most common signals.
func renderCountChips(c TaskCounts) string {
	parts := []string{
		projectCountTodoStyle.Render(fmt.Sprintf("⏳ %d", c.Todo)),
	}
	if c.InProgress > 0 {
		parts = append(parts, projectCountInProgStyle.Render(fmt.Sprintf("▶ %d", c.InProgress)))
	}
	if c.InReview > 0 {
		parts = append(parts, projectCountReviewStyle.Render(fmt.Sprintf("👁 %d", c.InReview)))
	}
	if c.Blocked > 0 {
		parts = append(parts, projectCountBlockedStyle.Render(fmt.Sprintf("⚠ %d", c.Blocked)))
	}
	parts = append(parts, projectCountDoneStyle.Render(fmt.Sprintf("✓ %d", c.Done)))
	return strings.Join(parts, "  ")
}

// renderCoordStatus renders the right-side "● active   2m" or
// "○ idle 4h+ · auto-stopped" indicator on the project's third line.
func renderCoordStatus(p *ProjectRow) string {
	switch {
	case p.Active:
		age := "—"
		if !p.LastTick.IsZero() {
			age = humanAge(time.Since(p.LastTick))
		}
		return coordActiveStyle.Render("● active") + " " + headerSubtleStyle.Render(age)
	case p.IdleStop:
		return coordIdleStyle.Render("○ idle · auto-stopped")
	default:
		return coordIdleStyle.Render("○ idle")
	}
}

// buildWorkerLines produces the rendered text lines for the workers
// column. Each worker takes two lines + a blank separator, matching
// the mockup's spacing.
func buildWorkerLines(snap *Snapshot, rightW int) []string {
	if snap == nil || len(snap.Workers) == 0 {
		return []string{
			"",
			columnHeadingStyle.Render("  no workers running"),
		}
	}
	var lines []string
	for _, wkr := range snap.Workers {
		lines = append(lines, workerBlockLines(wkr, rightW)...)
	}
	return lines
}

// workerBlockLines renders one worker's two-line block:
//
//	● <id>
//	  <project>:<slug>      <age> <state>
func workerBlockLines(w *WorkerRow, width int) []string {
	dot := workerDotStyle(w.Color).Render("●")
	id := workerIDStyle.Render(w.ID)
	line1 := "  " + dot + " " + id

	slug := workerSlugStyle.Render(fmt.Sprintf("%s:%s", w.Project, trimSlug(w.Slug)))
	stateLabel := workerStateStyle(w.Color).Render(fmt.Sprintf("%s %s", w.Age, w.State))
	gap := width - lipgloss.Width("    ") - lipgloss.Width(slug) - lipgloss.Width(stateLabel) - 2
	if gap < 1 {
		gap = 1
	}
	line2 := "    " + slug + strings.Repeat(" ", gap) + stateLabel
	return []string{line1, line2, ""}
}

// trimSlug drops the trailing "-<4hex>" suffix from a slug for compact
// display ("fix-toolbar-1a2b" → "fix-toolbar"). Slugs shorter than the
// "-XXXX" pattern return as-is.
func trimSlug(slug string) string {
	if len(slug) < 6 {
		return slug
	}
	if slug[len(slug)-5] != '-' {
		return slug
	}
	for _, c := range slug[len(slug)-4:] {
		if !isHexLower(c) {
			return slug
		}
	}
	return slug[:len(slug)-5]
}

func isHexLower(c rune) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
}

// renderDashboardFooter draws the bottom keybind legend line.
// Matches the mockup's `[j/k] nav  [⏎] open  [n] task  [a] attach  [/] search  [?] help`.
// The right side carries an uptime indicator.
func renderDashboardFooter(uptime time.Duration, usable int) string {
	chips := []struct{ key, label string }{
		{"j/k", "nav"},
		{"⏎", "open"},
		{"n", "task"},
		{"a", "attach"},
		{"g", "agents"},
		{"/", "search"},
		{"?", "help"},
		{"q", "quit"},
	}
	parts := make([]string, 0, len(chips))
	for _, c := range chips {
		parts = append(parts,
			footerLabelStyle.Render("[")+
				footerKeyStyle.Render(c.key)+
				footerLabelStyle.Render("] ")+
				footerLabelStyle.Render(c.label),
		)
	}
	left := strings.Join(parts, "  ")
	right := headerSubtleStyle.Render(fmt.Sprintf("uptime %s", formatUptime(uptime)))
	gap := usable - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right
}

// formatUptime renders d as HH:MM (capped at 99:59 for display).
func formatUptime(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	h := int(d.Hours())
	m := int(d.Minutes()) - h*60
	if h > 99 {
		h = 99
		m = 59
	}
	return fmt.Sprintf("%02d:%02d", h, m)
}

// padPlain right-pads s with spaces to reach w cells (lipgloss-aware
// width). Used by renderTwoColumnBody to keep the column separator
// aligned across rows.
func padPlain(s string, w int) string {
	cur := lipgloss.Width(s)
	if cur >= w {
		return s
	}
	return s + strings.Repeat(" ", w-cur)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
