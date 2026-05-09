// Context-pct chip + handoff-tag rendering for the dashboard.
//
// Issue #89 restored context_pct visibility (lost between v0.1 and v0.2)
// as a 5-segment bar plus an integer-percent label. Issue #95 then
// dropped the bar glyph — the segment chunks read as visual noise next
// to the colored percentage, which already carries the same gradient
// signal at glance distance. We keep the colored number, the per-zone
// thresholds, the hot-count pills, and the handoff tag.
//
//	rendering shape
//	────────────────────────────────────────────────────────────
//	pct:  32%         ← green zone (<50%)
//	      50%         ← amber threshold (50%-69%)
//	      95%         ← red threshold (≥70%)
//	tag:  ◐ HANDOFF   ← when HandoffType is set
//	      ◐ COMPACT   ← when HandoffType is "precompact"
//
// Layer 1 (renderContextBar): per-row colored percent. Pure function, no I/O.
// Layer 2 (renderHotCounts):  top-status "N yellow · M red" pills.
// Layer 3 (renderHandoffTag): small handoff tag right of the percent.
// Layer 4 (worker coverage):  workerContextRecord lookup heuristic.
//
// Color thresholds match fleet-guard's Yellow (50%) / Red (70%) zones
// (skills/fleet-guard/health.py:YELLOW_THRESHOLD/RED_THRESHOLD) so the
// dashboard reads the same colors fleet-guard uses to trigger handoffs.
package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/handoff"
)

// Context-pct thresholds. Mirror fleet-guard so what the dashboard
// shows is the same gradient that triggers auto-handoffs in the agent.
const (
	contextYellowThreshold = 50.0
	contextRedThreshold    = 70.0
)

// Per-zone label styles. Foreground only — no bold, so the chip reads
// as "informational" against the existing bold cursor / attention rows.
var (
	contextBarGreenStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(colorGreen))
	contextBarAmberStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(colorAmber))
	contextBarRedStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color(colorRed))

	// Handoff tag styles. Auto-yellow / auto-red ride the threshold
	// palette; manual + pre-compact stay dim because they're operator-
	// initiated (manual) or compact-driven (precompact) — neither maps
	// cleanly to a context-pct zone.
	handoffTagAutoYellowStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(colorAmber))
	handoffTagAutoRedStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color(colorRed))
	handoffTagDimStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color(colorDim))

	// Top-status hot counts. Bold + zone color so the operator can
	// scan from the top of the screen and see "5 yellow, 2 red" at a
	// glance without walking every agent row.
	hotCountYellowStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(colorAmber))
	hotCountRedStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(colorRed))
)

// renderContextBar formats a context_pct value as a colored integer-
// percent label. Returns "" when pct is nil so callers can splice the
// result into a row without "N/A" placeholders.
//
//	nil           → ""
//	0%            → "0%"   green (still green zone)
//	12%           → "12%"  green
//	48%           → "48%"  green (still <50)
//	50%           → "50%"  amber (yellow threshold)
//	69%           → "69%"  amber
//	70%           → "70%"  red   (red threshold)
//	95%           → "95%"  red
//	100%          → "100%" red
//	negative/NaN  → defensive clamp to 0
//	>100          → defensive clamp to 100
//
// Issue #95: the 5-segment bar glyph (▰▰▰▱▱) was dropped. The colored
// percentage carries the same threshold signal without the visual
// noise — operators read the gradient color at glance distance and
// the exact integer when they want detail.
func renderContextBar(pct *float64) string {
	if pct == nil {
		return ""
	}
	v := *pct
	// Defensive clamp. fleet-guard writes 0..100 but operator hand-edits
	// or future schema drift could deliver out-of-range values; render a
	// sensible chip rather than a Unicode garbage burst.
	if v < 0 || v != v { // NaN compares unequal to itself
		v = 0
	}
	if v > 100 {
		v = 100
	}
	style := contextBarStyleFor(v)
	return style.Render(fmt.Sprintf("%d%%", int(v)))
}

// contextZone returns the threshold zone label for a context_pct
// reading. Public-ish (lowercase, package-private but exposed to
// tests in the same package) so tests can assert the zone without
// pattern-matching ANSI escape sequences. The lipgloss profile is
// stripped in non-TTY runs, so palette-string assertions don't work
// inside `go test`.
//
//	pct < 50          → "green"
//	50 ≤ pct < 70     → "amber"
//	pct ≥ 70          → "red"
func contextZone(pct float64) string {
	switch {
	case pct >= contextRedThreshold:
		return "red"
	case pct >= contextYellowThreshold:
		return "amber"
	default:
		return "green"
	}
}

// contextBarStyleFor picks the percent-label color based on threshold
// zone. Mirrors fleet-guard's Yellow (50%) / Red (70%) zones exactly.
func contextBarStyleFor(pct float64) lipgloss.Style {
	switch contextZone(pct) {
	case "red":
		return contextBarRedStyle
	case "amber":
		return contextBarAmberStyle
	default:
		return contextBarGreenStyle
	}
}

// handoffTagSpec returns (label, zone) for a handoff_type value.
// Zone discriminates the palette without pinning ANSI escape codes:
//
//	auto-yellow → ("◐ HANDOFF", "amber")
//	auto-red    → ("◐ HANDOFF", "red")
//	manual      → ("◐ HANDOFF", "dim")
//	precompact  → ("◐ COMPACT", "dim")
//	other / nil → ("", "")
//
// Tests assert label + zone; render combines them via the lipgloss
// styles below. Same factoring as contextZone — keeps test assertions
// independent of the lipgloss color-profile-strip-in-CI behavior.
func handoffTagSpec(handoffType *string) (label, zone string) {
	if handoffType == nil || *handoffType == "" {
		return "", ""
	}
	switch *handoffType {
	case handoff.TypeAutoYellow:
		return "◐ HANDOFF", "amber"
	case handoff.TypeAutoRed:
		return "◐ HANDOFF", "red"
	case handoff.TypeManual:
		return "◐ HANDOFF", "dim"
	case handoff.TypePreCompact:
		return "◐ COMPACT", "dim"
	}
	return "", ""
}

// renderHandoffTag renders a one-token handoff state chip when the
// agent record has HandoffType set, else "". See handoffTagSpec for
// the value table; constants live in internal/handoff/handoff.go.
func renderHandoffTag(handoffType *string) string {
	label, zone := handoffTagSpec(handoffType)
	if label == "" {
		return ""
	}
	switch zone {
	case "amber":
		return handoffTagAutoYellowStyle.Render(label)
	case "red":
		return handoffTagAutoRedStyle.Render(label)
	default:
		return handoffTagDimStyle.Render(label)
	}
}

// hotCounts walks every alive agent + worker on the dashboard and
// returns (yellow, red) counters: agents/workers whose effective
// context_pct sits in the 50%-69% zone or the ≥70% zone respectively.
//
// "Effective" means: for agent rows, the agent record's own
// ContextPct; for worker rows, the context_pct of the agent record
// the worker maps to via workerContextRecord (the coord shares
// FLEET_AGENT_ID with its Agent-tool subagents per PR #87, so a
// worker's context_pct effectively lives on its coord's record).
//
// Counts dedupe on agent record ID — when a coord and N workers all
// resolve to the same coord record, we only count the coord once.
// Otherwise N workers + 1 coord on a project would produce N+1 hot
// counts for one underlying record, which would lie about how many
// agents are actually filling up.
func hotCounts(records []*agent.Record, alive map[string]bool, workers []*WorkerRow) (int, int) {
	if len(records) == 0 {
		return 0, 0
	}
	seen := map[string]bool{}
	yellow, red := 0, 0
	count := func(r *agent.Record) {
		if r == nil || r.ContextPct == nil || seen[r.ID] {
			return
		}
		// Skip dead records — they're informational only and operator
		// shouldn't be told to handoff a dead agent.
		if deriveStatus(r, alive) == "dead" {
			return
		}
		seen[r.ID] = true
		v := *r.ContextPct
		switch {
		case v >= contextRedThreshold:
			red++
		case v >= contextYellowThreshold:
			yellow++
		}
	}
	for _, r := range records {
		count(r)
	}
	for _, w := range workers {
		if w == nil {
			continue
		}
		rec := workerContextRecord(records, w)
		count(rec)
	}
	return yellow, red
}

// renderHotCounts formats the (yellow, red) counts as bold pills. Returns
// "" when both are zero so the top status line stays clean for the
// happy-path "every agent under 50%" case.
//
//	(0, 0) → ""                    no chip
//	(3, 0) → "3 yellow"            amber bold
//	(0, 2) → "2 red"               red bold
//	(3, 2) → "3 yellow · 2 red"    both, dim separator
func renderHotCounts(yellow, red int) string {
	if yellow == 0 && red == 0 {
		return ""
	}
	parts := []string{}
	if yellow > 0 {
		parts = append(parts, hotCountYellowStyle.Render(fmt.Sprintf("%d yellow", yellow)))
	}
	if red > 0 {
		parts = append(parts, hotCountRedStyle.Render(fmt.Sprintf("%d red", red)))
	}
	return strings.Join(parts, headerSepStyle.Render(" · "))
}

// workerContextRecord finds the agent record whose context_pct best
// represents this worker's session. Architecture (PR #87):
//
//	coord agent (FLEET_AGENT_ID=<coord-id>)
//	  └─ Agent-tool subagent worker (inherits FLEET_AGENT_ID)
//	     └─ Stop hook fires → writes context_pct to <coord-id>.json
//
// So a worker's context_pct lands on its coord's agent record, NOT a
// separate per-worker record. Lookup heuristic:
//
//  1. Prefer the project's coord record (TaskID == "coord-<project>"),
//     since that's the one fleet-guard writes to from the subagent's
//     Stop hook.
//  2. Fallback: ANY agent record matching the worker's project that
//     has a non-nil ContextPct. Covers legacy / hand-spawned setups
//     where the coord task_id convention differs.
//  3. Nothing → return nil so the row omits the chip rather than
//     misreporting.
//
// Lookup is O(n) on records but n is bounded by the operator's
// concurrent agent ceiling (1-20 in v1). Caching is not worth the
// complexity here.
func workerContextRecord(records []*agent.Record, w *WorkerRow) *agent.Record {
	if w == nil || w.Project == "" {
		return nil
	}
	wantTask := "coord-" + w.Project
	var fallback *agent.Record
	for _, r := range records {
		if r == nil || r.Project != w.Project {
			continue
		}
		if r.TaskID == wantTask {
			return r
		}
		// Pick the first project-matching record with a context_pct as a
		// fallback so non-coord setups still render *something*. Records
		// without context_pct contribute nothing to the bar.
		if fallback == nil && r.ContextPct != nil {
			fallback = r
		}
	}
	return fallback
}
