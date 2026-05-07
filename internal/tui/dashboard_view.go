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

	"github.com/edisonshen/fleet/internal/agent"
)

// renderDashboard returns the full Ops Console view (header strip,
// 2-col body, footer) as a single string. width is the terminal width
// reported by tea.WindowSizeMsg; falls back to a sensible 110 when 0
// (early renders before the size message lands).
//
// The body folds projects, tasks, workers, AND v0.1 agents into the
// rendered output (issue #53 part A). Agents render as a sub-section
// underneath the workers column with the heading "v0.1 agents — N
// active". The cursor lives in dashboardRows() (model.go) and walks
// every row regardless of column.
func renderDashboard(m Model) string {
	w := m.width
	if w <= 0 {
		w = 110
	}
	// 1-cell right margin matches the v0.1 layout's anti-wrap rule.
	// Anything wider triggers phantom-newline wrap on some terminals.
	usable := w - 1
	if usable < 60 {
		usable = 60
	}

	leftW, rightW := splitColumns(usable)

	var b strings.Builder
	b.WriteString(renderDashboardHeader(m, usable))
	b.WriteString("\n")
	b.WriteString(renderColumnHeadings(m, leftW, rightW))
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
// "WORKERS · 5 ACTIVE" strip below the totals. Right column label
// includes the v0.1 agent count when the model has any records,
// matching issue #53 part A's agents-folded-into-dashboard intent.
func renderColumnHeadings(m Model, leftW, rightW int) string {
	pn, wn := 0, 0
	if m.dashboard != nil {
		pn = len(m.dashboard.Projects)
		wn = len(m.dashboard.Workers)
	}
	an := len(m.records)
	leftLabel := columnHeadingStyle.Render(fmt.Sprintf("PROJECTS · %d ACTIVE", pn))
	rightHeading := fmt.Sprintf("WORKERS · %d ACTIVE", wn)
	if an > 0 {
		rightHeading += fmt.Sprintf("  ·  AGENTS %d", an)
	}
	rightLabel := columnHeadingStyle.Render(rightHeading)

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
//
// The body is built from the unified dashboardRows() so the cursor
// can highlight the right line: left column = projects + tasks; right
// column = workers + agents (with a "v0.1 agents — N active"
// sub-heading between worker rows and agent rows when both exist).
func renderTwoColumnBody(m Model, leftW, rightW int) string {
	leftLines, rightLines := buildBodyLines(m, leftW, rightW)

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

// buildBodyLines is the per-column line builder. Iterates
// dashboardRows() ONCE so a single cursor index maps consistently to
// both the rendered output and the action handlers in keys.go.
//
//	left column (top-to-bottom):
//	  project header (3 lines + blank)
//	    └─ task lines (1 each)
//	  next project …
//
//	right column:
//	  worker block (2 lines + blank)
//	  next worker …
//	  v0.1 agents — N active     <- sub-heading when records non-empty
//	    agent block (2 lines + blank)
//	    next agent …
//
// The cursor row is highlighted with a bold ▶ glyph in the left
// margin (project/task) or replacing the leading dot (worker/agent).
func buildBodyLines(m Model, leftW, rightW int) ([]string, []string) {
	rows := m.dashboardRows()

	// Left column: project + task rows.
	var left []string
	if (m.dashboard == nil || len(m.dashboard.Projects) == 0) && !rowsHaveLeft(rows) {
		// Don't advertise [n] here: taskAddProject refuses to create a
		// brand-new project from a random cwd, so on a fresh install
		// pressing [n] would just flash "no project context" (codex
		// iter-7 P2). [d] dispatch is the working bootstrap — it
		// creates ~/.fleet/projects/<tag>/ as a side effect of
		// spawning the first agent, after which [n] works.
		left = append(left, "",
			columnHeadingStyle.Render("  no projects yet — press [d] to dispatch one"))
	}
	for i, row := range rows {
		selected := i == m.dashCursor
		switch row.kind {
		case rowProject:
			left = append(left, projectBlockLines(row.project, leftW, selected)...)
		case rowTask:
			left = append(left, taskBlockLine(row.task, leftW, selected))
		}
	}

	// Right column: worker rows, then a sub-header, then agent rows.
	var right []string
	hasWorkers, hasAgents := false, false
	for i, row := range rows {
		selected := i == m.dashCursor
		if row.kind == rowWorker {
			right = append(right, workerBlockLines(row.worker, rightW, selected)...)
			hasWorkers = true
		}
	}
	if !hasWorkers {
		// Distinguish "no workers exist" from "filter hides workers"
		// (codex iter-6 P3): if the unfiltered snapshot still has
		// workers, the column is just narrowed, not empty.
		hint := "  no workers running"
		if m.searchFilter != "" && m.dashboard != nil && len(m.dashboard.Workers) > 0 {
			hint = fmt.Sprintf("  no workers match /%s — esc clears", m.searchFilter)
		}
		right = append(right, "", columnHeadingStyle.Render(hint))
	}
	// Insert v0.1 agents sub-heading only when records exist.
	if len(m.records) > 0 {
		right = append(right, "",
			columnHeadingStyle.Render(fmt.Sprintf(
				"  v0.1 agents — %d active", len(m.records))))
	}
	for i, row := range rows {
		selected := i == m.dashCursor
		if row.kind == rowAgent {
			right = append(right, agentBlockLines(row.agent, m.aliveByID, rightW, selected)...)
			hasAgents = true
		}
	}
	_ = hasAgents
	return left, right
}

// rowsHaveLeft returns true when at least one row would render in the
// left column. Used to decide whether to emit the empty-projects hint.
func rowsHaveLeft(rows []dashRow) bool {
	for _, r := range rows {
		if r.kind == rowProject || r.kind == rowTask {
			return true
		}
	}
	return false
}

// projectBlockLines renders one project's three-line block:
//
//	<name>                                     [● N attn]
//	<repo>
//	⏳ N ▶ N 👁 N ⚠ N ✓ N    ● active     last-tick
//
// Attention rows get a leading "▌ " accent (red bold) on every line of
// the block to mirror the mockup's left-border treatment. The
// selected variant prefixes line 1 with the cursor glyph "▶" so the
// operator can see which row [⏎]/[a] etc. will act on.
func projectBlockLines(p *ProjectRow, w int, selected bool) []string {
	prefix := "  "
	if p.Attention > 0 {
		prefix = attentionBorderStyle.Render("▌ ") + " "
	}
	cursorPrefix := prefix
	if selected {
		// Replace the 2-cell prefix's leading spaces with the cursor
		// glyph so the cell width stays the same — no shift in the
		// block's right-side stat columns when selection toggles.
		cursorPrefix = cursorGlyphStyle.Render("▶ ")
		if p.Attention > 0 {
			cursorPrefix = cursorGlyphStyle.Render("▶ ") + attentionBorderStyle.Render("▌ ")
		}
	}

	// Line 1: name + (right-flushed) attention chip.
	name := projectNameStyle.Render(p.Name)
	var attnRight string
	if p.Attention > 0 {
		attnRight = attentionChipStyle.Render(fmt.Sprintf("● %d attn", p.Attention))
	}
	line1 := cursorPrefix + name
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

// taskBlockLine renders one task row indented under its parent
// project:
//
//   - todo  add-readme
//
// The selected variant uses the cursor glyph "▶" in place of the
// bullet to mark which task [⏎] open will operate on.
func taskBlockLine(t *taskRow, w int, selected bool) string {
	if t == nil {
		return ""
	}
	prefix := "    "
	bullet := "•"
	bulletStyle := dimStyle
	if selected {
		prefix = "  "
		bullet = "▶"
		bulletStyle = cursorGlyphStyle
	}
	status := projectCountTodoStyle.Render(t.Status)
	slug := workerSlugStyle.Render(t.Slug)
	return prefix + bulletStyle.Render(bullet) + " " + status + "  " + slug
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

// workerBlockLines renders one worker's two-line block:
//
//	● <id>
//	  <project>:<slug>      <age> <state>
//
// The selected variant replaces the leading status dot with the
// cursor glyph "▶" so the row reads as "next [⏎]/[a]/[x] target".
func workerBlockLines(w *WorkerRow, width int, selected bool) []string {
	dot := workerDotStyle(w.Color).Render("●")
	if selected {
		dot = cursorGlyphStyle.Render("▶")
	}
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

// agentBlockLines renders one v0.1 agent's two-line block under the
// "v0.1 agents" sub-heading. Same shape as workerBlockLines so the
// right column reads consistently.
//
//	● <agent-id-short>                       <status>
//	  <project>:<task>                       <age>
func agentBlockLines(r *agent.Record, alive map[string]bool, width int, selected bool) []string {
	if r == nil {
		return nil
	}
	status := deriveStatus(r, alive)
	glyph, gStyle := glyphFor(status)
	if selected {
		glyph = "▶"
		gStyle = cursorGlyphStyle
	}
	idShort := r.ID
	if len(idShort) > 8 {
		idShort = idShort[:8]
	}
	id := workerIDStyle.Render(idShort)
	statusLab := statusStyleFor(status).Render(statusLabel(status))
	line1raw := "  " + gStyle.Render(glyph) + " " + id
	gap1 := width - lipgloss.Width(line1raw) - lipgloss.Width(statusLab) - 2
	if gap1 < 1 {
		gap1 = 1
	}
	line1 := line1raw + strings.Repeat(" ", gap1) + statusLab

	project := r.Project
	if project == "" {
		project = "-"
	}
	task := r.TaskID
	if task == "" {
		task = "-"
	}
	slug := workerSlugStyle.Render(fmt.Sprintf("%s:%s", project, task))
	age := dimStyle.Render(humanAge(time.Since(r.SpawnedAt)))
	gap2 := width - lipgloss.Width("    ") - lipgloss.Width(slug) - lipgloss.Width(age) - 2
	if gap2 < 1 {
		gap2 = 1
	}
	line2 := "    " + slug + strings.Repeat(" ", gap2) + age
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
// Matches the mockup's `[j/k] nav  [⏎] open  [n] task  [a] attach
// [/] search  [?] help`. The right side carries an uptime indicator
// and (when set) the active search filter.
func renderDashboardFooter(uptime time.Duration, usable int, searchFilter string) string {
	chips := []struct{ key, label string }{
		{"j/k", "nav"},
		{"⏎", "open"},
		{"n", "task"},
		{"a", "attach"},
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
	rightParts := []string{}
	if searchFilter != "" {
		rightParts = append(rightParts, searchFooterStyle.Render(
			fmt.Sprintf("/%s · esc clears", searchFilter)))
	}
	rightParts = append(rightParts, headerSubtleStyle.Render(
		fmt.Sprintf("uptime %s", formatUptime(uptime))))
	right := strings.Join(rightParts, "  ")
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
