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
//	│ [⏎] open  [n] task  …           uptime HH:MM      │
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
	"github.com/edisonshen/fleet/internal/tmux"
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
	// Project count uses the unioned list so loose-agent projects
	// (no ~/.fleet/projects/<tag>/ tree yet) still register in the
	// header total (issue #55).
	pCount = len(m.unifiedProjects())
	if snap != nil {
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

	// Hot-count chips (issue #89): "<N> yellow · <M> red" appears
	// after "workers active" when at least one alive agent or worker
	// has crossed the 50%/70% threshold. Skipped when both are zero so
	// the strip stays clean for the all-green common case.
	var workers []*WorkerRow
	if snap != nil {
		workers = snap.Workers
	}
	if y, r := hotCounts(m.records, m.aliveByID, workers); y > 0 || r > 0 {
		parts = append(parts, renderHotCounts(y, r))
	}
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
// includes the live agent count (records minus dead) when any are
// present, matching issue #53 part A's agents-folded-into-dashboard
// intent.
func renderColumnHeadings(m Model, leftW, rightW int) string {
	// LEFT count uses the unioned project list so agents dispatched on
	// a non-v0.2-init'd repo show up in the heading total too (issue
	// #55). Without this, "PROJECTS · 0 ACTIVE" while the operator has
	// agents running on rainier/spark/tatoosh.
	pn := len(m.unifiedProjects())
	wn := 0
	if m.dashboard != nil {
		wn = len(m.dashboard.Workers)
	}
	// Coord IDs are rendered as part of the project block on the LEFT,
	// so they shouldn't count toward the RIGHT-column agents header.
	coordIDs := map[string]bool{}
	for _, p := range m.unifiedProjects() {
		if p.CoordID != "" {
			coordIDs[p.CoordID] = true
		}
	}
	an := 0
	for _, r := range m.records {
		if r != nil && coordIDs[r.ID] {
			continue
		}
		if deriveStatus(r, m.aliveByID) != "dead" {
			an++
		}
	}
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
	if len(m.unifiedProjects()) == 0 && !rowsHaveLeft(rows) {
		// Empty-state hint nudges [n] task to match the footer keybind
		// chip (issue #55). The earlier hint pointed at [d] dispatch
		// (introduced in codex iter-7 to dodge the "no project context"
		// flash on truly-fresh installs); the operator clarified that
		// the hint must agree with the advertised footer key, even at
		// the cost of that bootstrap edge case. On a non-fresh install
		// (cwd is a known v0.2 project, or cursor is on a project row)
		// [n] resolves correctly and adds the task.
		left = append(left, "",
			columnHeadingStyle.Render("  no projects yet — press [n] to add a task"))
	}
	spawnCtx := coordSpawnCtx{
		now:          time.Now(),
		tickFrame:    m.tickCount,
		spawnTimeout: m.coordSpawnTimeout,
		// m.records feeds the Path B "fresh agent record" check inside
		// applyStuckSelfHeal — see coord_spawn.go's two-path heal logic.
		// Loaded once via loadAgentsCmd; nil on cold start (Path B no-ops
		// until the first agent load completes, Path A still fires).
		records: m.records,
	}
	// Activity-grouping helper — when a project block follows the
	// hidden separator AND that separator is expanded, render the
	// project lines dim so the operator's eye reads the rows as
	// secondary content (issue #98).
	insideHiddenGroup := false
	for i, row := range rows {
		selected := i == m.dashCursor
		switch row.kind {
		case rowProject:
			lines := projectBlockLines(row.project, leftW, selected, spawnCtx)
			if insideHiddenGroup {
				lines = applyHiddenStyle(lines)
			}
			left = append(left, lines...)
		case rowTask:
			line := taskBlockLine(row.task, leftW, selected)
			if insideHiddenGroup {
				line = hiddenProjectStyle.Render(line)
			}
			left = append(left, line)
		case rowSeparator:
			left = append(left, separatorBlockLine(row.separator, leftW, selected))
			// Empty trailing line keeps spacing consistent with the
			// project blocks (which end with "" for visual rhythm).
			left = append(left, "")
			if row.separator != nil && row.separator.kind == separatorHidden {
				insideHiddenGroup = true
			} else {
				insideHiddenGroup = false
			}
		}
	}

	// Right column: worker rows, then a sub-header, then agent rows.
	var right []string
	hasWorkers, hasAgents := false, false
	for i, row := range rows {
		selected := i == m.dashCursor
		if row.kind == rowWorker {
			right = append(right, workerBlockLines(row.worker, m.records, rightW, selected)...)
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
	// Insert v0.1 agents sub-heading. The counter shows VISIBLE-LIVE
	// agents — records that survived the filter AND whose derived
	// status isn't "dead" (codex iter-9 P3 — was using len(m.records)
	// which over-counted both filtered-out and orphaned sessions).
	visibleAlive := 0
	for _, row := range rows {
		if row.kind != rowAgent || row.agent == nil {
			continue
		}
		if deriveStatus(row.agent, m.aliveByID) == "dead" {
			continue
		}
		visibleAlive++
	}
	if visibleAlive > 0 || (len(m.records) > 0 && m.searchFilter == "") {
		// When the operator hasn't filtered, still show a sub-header
		// for context (e.g. all agents dead → "v0.1 agents — 0
		// active" tells the operator the section exists).
		right = append(right, "",
			columnHeadingStyle.Render(fmt.Sprintf(
				"  v0.1 agents — %d active", visibleAlive)))
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

// separatorBlockLine renders one "─── N idle ───" / "─── N hidden ───"
// line for the LEFT column (issue #98). The line carries the count + a
// hint about the operator's next move; the cursor glyph "▶" replaces
// the leading dashes when the row is selected.
//
// Width target = leftW. We pad the trailing dashes so the line spans
// the column visually. Lipgloss handles the cell math.
func separatorBlockLine(sep *separatorRow, w int, selected bool) string {
	if sep == nil {
		return ""
	}
	var label string
	switch sep.kind {
	case separatorIdle:
		if sep.expanded {
			label = fmt.Sprintf("%d idle (expanded — [enter] to collapse)", sep.count)
		} else {
			label = fmt.Sprintf("%d idle — [enter] to expand", sep.count)
		}
	case separatorHidden:
		if sep.expanded {
			label = fmt.Sprintf("%d hidden (expanded — [c] to view-toggle)", sep.count)
		} else {
			label = fmt.Sprintf("%d hidden — [enter] to expand, [c] to view-toggle", sep.count)
		}
	case separatorHistory:
		// Issue #101: collapsible "─── N done ───" group inside an
		// expanded project. Operator [enter]s the row to toggle.
		if sep.expanded {
			label = fmt.Sprintf("%d done (expanded — [enter] to collapse)", sep.count)
		} else {
			label = fmt.Sprintf("%d done — [enter] to expand", sep.count)
		}
	default:
		return ""
	}
	style := separatorDimStyle
	cursor := "  "
	if selected {
		style = separatorCursorStyle
		cursor = separatorCursorStyle.Render("▶ ")
	}
	// "─── label ───" — the dash runs flank the label so the row reads
	// as a group divider regardless of label length.
	const prefixDashes = "─── "
	const suffixSeed = " ───"
	body := prefixDashes + label + suffixSeed
	rendered := cursor + style.Render(body)
	// Pad with extra dashes to fill the column. This makes wider
	// terminals show a visible run-out — avoids the row reading as a
	// short floating pill on a wide screen.
	used := lipgloss.Width(rendered)
	if w > used+2 {
		extra := w - used - 2
		rendered += style.Render(strings.Repeat("─", extra))
	}
	return rendered
}

// applyHiddenStyle wraps each non-empty line in the hidden-project dim
// style so a hidden project's block reads as secondary content. Empty
// strings (block-spacing rows) pass through unchanged.
func applyHiddenStyle(lines []string) []string {
	out := make([]string, len(lines))
	for i, ln := range lines {
		if ln == "" {
			out[i] = ln
			continue
		}
		out[i] = hiddenProjectStyle.Render(ln)
	}
	return out
}

// rowsHaveLeft returns true when at least one row would render in the
// left column. Used to decide whether to emit the empty-projects hint.
func rowsHaveLeft(rows []dashRow) bool {
	for _, r := range rows {
		if r.kind == rowProject || r.kind == rowTask || r.kind == rowSeparator {
			return true
		}
	}
	return false
}

// projectBlockLines renders one project's three-line (or four-line)
// block:
//
//	<name>                                     [● N attn]
//	<repo>
//	⏳ N ▶ N 👁 N ⚠ N ✓ N    ● active     last-tick
//	  coord <coord-id>                          (issue #55)
//
// Attention rows get a leading "▌ " accent (red bold) on every line of
// the block to mirror the mockup's left-border treatment. The
// selected variant prefixes line 1 with the cursor glyph "▶" so the
// operator can see which row [⏎]/[a] etc. will act on.
//
// Coord line (issue #55): when ProjectRow.CoordID is set (the project's
// coord skill currently holds the lock AND its coord-state.json is
// fresh), we render a compact "coord <id>" line below the counts row.
// The line is purely informational; the cursor doesn't land on it
// (coord-as-row-target is out of scope for this PR — operator attaches
// via the agent record on the RIGHT only when a coord is also a
// loose-agent-tagged record, which is the v0.2 norm). When CoordID is
// empty, the block stays at three lines and we skip the coord row.
//
// Spawning indicator (issue #86): when the coord-spawn marker exists
// AND coord-state.json hasn't published a fresh tick yet, we render
// "⠋ spawning coord... 1m 23s" in the same slot as the coord-id line.
// Beyond ctx.spawnTimeout, the slot flips to a red "⚠ coord spawn
// stuck — check tmux session fleet-<name>" warning. State derivation
// lives in deriveCoordSpawnState (coord_spawn.go) so it can be tested
// in isolation; this function just renders the result.
func projectBlockLines(p *ProjectRow, w int, selected bool, ctx coordSpawnCtx) []string {
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

	// Line 1: name + (right-flushed) attention chip. The encoded p.Name
	// (cwd-derived, hyphen-separated) is rewritten by projectDisplayName
	// for visual clarity — first hyphen becomes a slash so cwd-derived
	// names like "projects-fleet" render as "projects/fleet" (issue #66).
	// All identity uses (lookups, file paths, search) still consume p.Name.
	name := projectNameStyle.Render(projectDisplayName(p.Name))
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

	// Optional Line 4: coord identifier (issue #55). Rendered only when
	// the project's coord is freshly publishing into the lock body —
	// operators want to see WHICH agent is coordinating without leaving
	// the dashboard for `fleet attach`.
	if p.CoordID != "" {
		coordLabel := dimStyle.Render("coord ") + workerIDStyle.Render(p.CoordID)
		line4 := prefix + coordLabel
		return []string{line1, line2, line3, line4, ""}
	}

	// Optional Line 4 alt: coord-spawn indicator (issue #86). When the
	// coord-spawn marker exists but coord-state.json hasn't published
	// a fresh tick yet, render "⠋ spawning coord... 1m 23s" so the
	// 3-5min cold start isn't a silent wait. The stale/missing branch
	// fires when scanProject sees no fresh coord-state.json — i.e.
	// !p.Active (Active is set iff now-stateMtime ≤ coordActiveWindow).
	// Beyond ctx.spawnTimeout the line flips to a red stuck warning.
	//
	// Issue #96 gap 1 self-heal: when derivation lands on Stuck but the
	// tmux session for the agent_id stored in the marker is gone, the
	// spawn died silently — clear the stale marker so the next render
	// flips back to Idle. We probe the tmux session via sessionAliveFn
	// (existing helper, same one used by the [a] attach branch) so a
	// live session past timeout still surfaces the warning (real hung
	// spawn — the operator should attach via tmux).
	//
	// Issue #96 gap 2: the stuck hint renders `fleet-<agentID>` from
	// the marker, not `fleet-<projectName>`. We read the agent_id once
	// here and thread it through to the renderer; both gaps share the
	// same marker read so the cost is one os.ReadFile per row per render.
	markerMtime, markerOK := coordSpawnMarkerMtimeFn(p.Name)
	markerAgentID := ""
	if markerOK {
		markerAgentID = coordSpawnMarkerFn(p.Name)
	}
	st := deriveCoordSpawnState(
		markerOK, markerMtime,
		!p.LastTick.IsZero(), p.LastTick,
		ctx.now,
		coordActiveWindow, ctx.spawnTimeout,
	)
	if st == coordSpawnStuck && markerAgentID != "" {
		sess := tmuxSessionName(markerAgentID)
		alive := sessionAliveFn(sess)
		// Path B probe: does the agent record on disk show a recent
		// last_activity_ts? If yes, the spawn already succeeded and the
		// marker is stale — heal regardless of tmux state. Path A still
		// covers the dead-tmux + no-record case for silent spawn deaths.
		fresh := isAgentRecordFresh(ctx.records, markerAgentID, ctx.now, agentRecordFreshWindow)
		st, _ = applyStuckSelfHeal(st, markerAgentID, alive, fresh, func() error {
			return removeCoordSpawnMarkerFn(p.Name)
		})
	}
	if line, ok := renderCoordSpawnLineForProject(
		st, prefix, p.Name, markerAgentID, ctx.now, markerMtime, ctx.tickFrame,
	); ok {
		return []string{line1, line2, line3, line, ""}
	}

	return []string{line1, line2, line3, ""}
}

// tmuxSessionName returns the canonical tmux session name for a fleet
// agent ID. Thin wrapper around tmux.SessionName so the dashboard
// renderer's session-naming agrees with the spawn / attach / kill
// paths in internal/tmux without re-deriving the format here.
func tmuxSessionName(agentID string) string {
	return tmux.SessionName(agentID)
}

// taskBlockLine renders one task row indented under its parent
// project. Issue #59: titles only (one per line) — the status is
// already aggregated into the project header's count chips, so we
// keep the inline list compact.
//
//   - <slug>
//
// Issue #75 / #77 — status-aware glyph + color per task row mirrors the
// project-header count-chip palette so a row reads as the same status
// the header counts:
//
//	todo        → "○ <slug>"  dim       (matches projectCountTodoStyle)
//	ready       → "◐ <slug>"  bright    (promote-eligible, no header chip)
//	in-progress → "▶ <slug>"  amber     (matches projectCountInProgStyle)
//	in-review   → "⟳ <slug>"  blue      (matches projectCountReviewStyle)
//	blocked     → "⏸ <slug>"  faint dim (issue #103; planning state — NOT
//	                                    actionable. Worker phase=blocked
//	                                    is the actionable signal and is
//	                                    surfaced via the row attention
//	                                    chip, not the per-task glyph.)
//	done        → "✓ <slug>"  green     (existing projectCountDoneStyle)
//	default     → "• <slug>"  dim       (abandoned / unknown — fallback)
//
// Done tasks are filtered out of row.Tasks today (dashboard.go:215),
// but if a task transitions while the operator's looking, the prefix
// should still be right — defensive coverage is cheap.
//
// Synthetic markers render as hint lines:
//
//	Empty=true  → "  no tasks yet — `fleet init` to create tasks.md"
//	More=N      → "  +N more"
//
// The selected variant uses the cursor glyph "▶" in place of the
// status glyph so the operator can see which row is active. The label
// color still tracks status, so a blocked task under the cursor still
// reads as "needs attention" (PR #76's
// TestRows_BlockedTaskUnderCursorKeepsCursorGlyph extended to all 7
// statuses in this PR's tests).
//
// When the row is a synthetic marker, the cursor glyph still anchors
// the line even though there's no [⏎] action wired (operator can
// move past it; per spec: cursor on task sub-rows is a navigation
// no-op for actions in this PR).
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
	switch {
	case t.Empty:
		// Backticks aren't lipgloss-special; render the hint dim so
		// the operator's eye gets pulled to real task rows when both
		// kinds of expansion are visible across the column.
		hint := dimStyle.Render("no tasks yet — `fleet init` to create tasks.md")
		return prefix + bulletStyle.Render(bullet) + " " + hint
	case t.More > 0:
		more := dimStyle.Render(fmt.Sprintf("+%d more", t.More))
		return prefix + bulletStyle.Render(bullet) + " " + more
	}
	// Status-aware glyph + label styling. Selection's cursor glyph wins
	// over the status glyph; the label color still tracks status. Status
	// strings are tasks.Status enum values — see internal/tasks/tasks.go.
	statusGlyph, statusGlyphStyle, statusLabelStyle := taskStatusStyles(t.Status)
	glyph := bullet
	glyphStyle := bulletStyle
	if !selected {
		glyph = statusGlyph
		glyphStyle = statusGlyphStyle
	}
	slug := statusLabelStyle.Render(t.Slug)
	// Issue #101 history rendering: done + abandoned tasks live in the
	// `─── N done ───` history group. Append the PR number tail when
	// the task entry carries a pr_url so the operator can see the PR
	// from the row without opening the detail panel. Abandoned tasks
	// render with the ✗ glyph (overridden from the default ✓ for done)
	// — taskStatusStyles falls through for "abandoned" so we patch the
	// glyph here.
	tail := ""
	if t.Status == "done" && t.PRURL != "" {
		if num := prNumberFromURL(t.PRURL); num != "" {
			tail = " " + dimStyle.Render("· PR "+num)
		}
	}
	if t.Status == "abandoned" && !selected {
		glyph = "✗"
		glyphStyle = dimStyle
	}
	return prefix + glyphStyle.Render(glyph) + " " + slug + tail
}

// prNumberFromURL extracts the trailing /pull/<N> number from a PR
// URL. Returns "#N" on success, empty string on any parse failure.
// Designed for the issue #101 history row tail render — fallible
// gracefully so a missing parse just omits the tail rather than
// crashing the dashboard.
func prNumberFromURL(url string) string {
	const marker = "/pull/"
	i := strings.LastIndex(url, marker)
	if i < 0 {
		return ""
	}
	rest := url[i+len(marker):]
	// Trim anything after the number (query, fragment, trailing path).
	end := 0
	for end < len(rest) {
		c := rest[end]
		if c < '0' || c > '9' {
			break
		}
		end++
	}
	if end == 0 {
		return ""
	}
	return "#" + rest[:end]
}

// taskStatusStyles returns (glyph, glyphStyle, labelStyle) for the
// given tasks.Status string. Falls back to "•" + dim + workerSlugStyle
// for unknown values (e.g. "abandoned") so a future status still
// renders something sensible.
//
// Pairing glyph color with label color makes the row read as one chip
// rather than two unrelated tokens. Mirrors renderCountChips's per-
// status palette so a task row's color matches the header count.
func taskStatusStyles(status string) (string, lipgloss.Style, lipgloss.Style) {
	switch status {
	case "todo":
		return "○", taskGlyphTodoStyle, taskLabelTodoStyle
	case "ready":
		return "◐", taskGlyphReadyStyle, taskLabelReadyStyle
	case "in-progress":
		return "▶", taskGlyphInProgressStyle, taskLabelInProgressStyle
	case "in-review":
		return "⟳", taskGlyphInReviewStyle, taskLabelInReviewStyle
	case "blocked":
		// Issue #103: ⏸ (pause) + dim/faint signals "task is paused on a
		// sequencing dep" — a planning state. The previous ⚠ + red
		// treatment conflated planning-blocked with worker phase=blocked
		// (the actual raise-hand signal); operators saw "1 attn" on a
		// project whose only "blocked" was an external-dep marker. Worker
		// phase=blocked still drives the row-level attention chip via
		// scanProject's attention loop, which is the load-bearing signal.
		return "⏸", taskGlyphBlockedStyle, taskLabelBlockedStyle
	case "done":
		return "✓", projectCountDoneStyle, projectCountDoneStyle
	}
	return "•", dimStyle, workerSlugStyle
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
//	● <id>           32% ◐ HANDOFF
//	  <project>:<slug>                       <age> <state>
//
// The selected variant replaces the leading status dot with the
// cursor glyph "▶" so the row reads as "next [⏎]/[a]/[x] target".
//
// Issue #89: the worker's context-pct + handoff state is sourced from
// the matching agent record (looked up by workerContextRecord). Workers
// don't write their own context_pct — under PR #87, Agent-tool subagents
// inherit FLEET_AGENT_ID from the coord and the Stop hook writes to the
// coord's record. The chip therefore reflects the coord's session
// pressure, which is the operator-actionable signal for "this worker's
// containing coord is filling up". Lookup miss → omit the chip.
func workerBlockLines(w *WorkerRow, records []*agent.Record, width int, selected bool) []string {
	dot := workerDotStyle(w.Color).Render("●")
	if selected {
		dot = cursorGlyphStyle.Render("▶")
	}
	id := workerIDStyle.Render(w.ID)
	line1 := "  " + dot + " " + id

	// Look up the agent record whose context_pct represents this worker.
	// records may be nil when the dashboard has no agent records yet
	// (early renders) — workerContextRecord handles that and the chips
	// stay empty.
	var bar, tag string
	if rec := workerContextRecord(records, w); rec != nil {
		bar = renderContextBar(rec.ContextPct)
		tag = renderHandoffTag(rec.HandoffType)
	}
	chips := joinChips(bar, tag)
	if chips != "" {
		// Right-flush the chips on line 1 with a 2-cell trailing margin
		// matching the rest of the column's right-edge convention.
		gap1 := width - lipgloss.Width(line1) - lipgloss.Width(chips) - 2
		if gap1 < 1 {
			gap1 = 1
		}
		line1 = line1 + strings.Repeat(" ", gap1) + chips
	}

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
//	● <agent-id-short>          32% ◐ HANDOFF      <status>
//	  <project>:<task>                                    <age>
//
// Issue #89: line 1 picks up an inline colored context-pct + handoff tag
// when the record carries them; nil values omit the chips. The chips
// sit between the agent ID and the right-flushed status so the row
// reads left-to-right as "id → context → handoff state → status".
// Issue #95 dropped the bar glyph; only the colored percent remains.
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

	// Context-pct bar + handoff tag — issue #89. Both omit cleanly
	// when the record has nil values, so non-fleet-guarded agents
	// (legacy v0.1 records or pre-bootstrap dispatches) render the
	// row exactly as before.
	bar := renderContextBar(r.ContextPct)
	tag := renderHandoffTag(r.HandoffType)
	chips := joinChips(bar, tag)

	line1raw := "  " + gStyle.Render(glyph) + " " + id
	if chips != "" {
		line1raw = line1raw + "  " + chips
	}
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

// joinChips concatenates a context-pct bar + handoff tag with a single
// space when both are present, returns the non-empty one when only
// one is present, or "" when neither. Avoids ugly leading/trailing
// spaces inside row format strings.
func joinChips(bar, tag string) string {
	switch {
	case bar != "" && tag != "":
		return bar + " " + tag
	case bar != "":
		return bar
	case tag != "":
		return tag
	}
	return ""
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
// Renders only the action chips — navigation (j/k or ↓/↑) and
// panel-switch (←/→) keys are intuitive enough to stay silent
// (issue #90). The [?] help overlay still documents every key for
// discoverability. The right side carries an uptime indicator and
// (when set) the active search filter.
//
// j/k still work; ↓/↑ are silent aliases handled in model.handleKey.
// ← jumps to the first PROJECTS row, → jumps to the first
// WORKERS/agents row — both unchanged since PR #85, just no longer
// advertised in the footer.
//
// Inter-chip separator is one space (was two pre-#90). Adding the
// [h] handoff and [x] archive chips pushed the legend past the
// usable-width budget on common 100-cell split panes; tightening
// the separator keeps the line on a single row at width >= 96
// without sacrificing readability — chips are still bracketed,
// which carries enough visual separation on its own.
func renderDashboardFooter(uptime time.Duration, usable int, searchFilter string) string {
	return renderDashboardFooterWithHidden(uptime, usable, searchFilter, 0, 0)
}

// renderDashboardFooterWithHidden is the issue #98 extension that
// surfaces a "<N> hidden — [c] view" chip on the right side when N > 0.
// "hiddenWith" appends " · M with activity" when at least one hidden
// project has fresh activity, so operators see the hidden list isn't
// dormant without overriding the hide.
//
// The chip sits between the search filter (when set) and the uptime
// counter so the right edge keeps a consistent layout shape.
func renderDashboardFooterWithHidden(uptime time.Duration, usable int, searchFilter string, hiddenCount, hiddenWith int) string {
	chips := []struct{ key, label string }{
		{"⏎", "open"},
		{"n", "task"},
		{"a", "attach"},
		{"h", "handoff"},
		{"x", "archive"},
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
	left := strings.Join(parts, " ")
	rightParts := []string{}
	if searchFilter != "" {
		rightParts = append(rightParts, searchFooterStyle.Render(
			fmt.Sprintf("/%s · esc clears", searchFilter)))
	}
	if hiddenCount > 0 {
		body := fmt.Sprintf("%d hidden — [c] view", hiddenCount)
		if hiddenWith > 0 {
			body += fmt.Sprintf(" · %d with activity", hiddenWith)
		}
		rightParts = append(rightParts, hiddenChipStyle.Render(body))
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
