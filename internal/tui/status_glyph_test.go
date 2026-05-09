// Tests for issue #77 — distinct visual styling per task status in
// the dashboard's project expansion. PR #76 covered blocked + done +
// the cursor-override-on-blocked invariant; this file extends to all
// 7 statuses + cursor-override-on-every-status.
package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// TestRows_TodoTaskGlyph pins ○ for tasks at status=todo. ○ is the
// "queue" glyph — visually quietest of the live statuses, matching
// the dim color of projectCountTodoStyle in the project header.
func TestRows_TodoTaskGlyph(t *testing.T) {
	tr := &taskRow{Slug: "todo-task-aaaa", Status: "todo"}
	out := taskBlockLine(tr, 60, false)
	if !strings.Contains(out, "○") {
		t.Errorf("todo task line should carry ○ glyph, got: %q", out)
	}
	if strings.Contains(out, "•") {
		t.Errorf("todo task line should not carry • fallback bullet, got: %q", out)
	}
	if !strings.Contains(out, "todo-task-aaaa") {
		t.Errorf("todo task line should still render slug, got: %q", out)
	}
}

// TestRows_ReadyTaskGlyph pins ◐ for tasks at status=ready. ◐ ("half-
// circle") visually communicates "loaded, waiting for promote" — a
// step beyond plain todo, hence the bright treatment.
func TestRows_ReadyTaskGlyph(t *testing.T) {
	tr := &taskRow{Slug: "ready-task-bbbb", Status: "ready"}
	out := taskBlockLine(tr, 60, false)
	if !strings.Contains(out, "◐") {
		t.Errorf("ready task line should carry ◐ glyph, got: %q", out)
	}
	if !strings.Contains(out, "ready-task-bbbb") {
		t.Errorf("ready task line should still render slug, got: %q", out)
	}
}

// TestRows_DoingTaskGlyph pins ▶ for tasks at status=in-progress.
// Mirrors the project-header "▶ N" count chip so the row reads as
// the same status the header counts.
func TestRows_DoingTaskGlyph(t *testing.T) {
	tr := &taskRow{Slug: "doing-task-cccc", Status: "in-progress"}
	out := taskBlockLine(tr, 60, false)
	if !strings.Contains(out, "▶") {
		t.Errorf("in-progress task line should carry ▶ glyph, got: %q", out)
	}
	if !strings.Contains(out, "doing-task-cccc") {
		t.Errorf("in-progress task line should still render slug, got: %q", out)
	}
}

// TestRows_InReviewTaskGlyph pins ⟳ for tasks at status=in-review.
// ⟳ ("rotating arrow") evokes an iterative review cycle. The header
// count uses ◇ (white diamond, observation) but ⟳ reads cleaner inline
// next to the slug; both share the blue color family.
func TestRows_InReviewTaskGlyph(t *testing.T) {
	tr := &taskRow{Slug: "review-task-dddd", Status: "in-review"}
	out := taskBlockLine(tr, 60, false)
	if !strings.Contains(out, "⟳") {
		t.Errorf("in-review task line should carry ⟳ glyph, got: %q", out)
	}
	if !strings.Contains(out, "review-task-dddd") {
		t.Errorf("in-review task line should still render slug, got: %q", out)
	}
}

// TestRows_BlockedTaskGlyph pins ‖ for blocked tasks (issue #103).
// "Blocked" on a task is a planning state — operator-set when
// sequencing work — not a worker raise-hand. The ‖ pause glyph signals
// "parked, not asking"; the dim/faint label color makes the row recede
// rather than shout. Compare with worker phase=blocked, which keeps
// the red row-level attention chip via the project header.
//
// Pre-#103 this test asserted ⚠ + red — same code path, different
// visual semantics. Glyph polish: was ⏸ (color-emoji); replaced with ‖
// DOUBLE VERTICAL LINE for monochrome rendering.
func TestRows_BlockedTaskGlyph(t *testing.T) {
	tr := &taskRow{Slug: "blocked-task-eeee", Status: "blocked"}
	out := taskBlockLine(tr, 60, false)
	if !strings.Contains(out, "‖") {
		t.Errorf("blocked task line should carry ‖ glyph, got: %q", out)
	}
	if strings.Contains(out, "⚠") || strings.Contains(out, "▲") {
		t.Errorf("blocked task line must NOT carry the warning glyph (issue #103), got: %q", out)
	}
	if !strings.Contains(out, "blocked-task-eeee") {
		t.Errorf("blocked task line should still render slug, got: %q", out)
	}
}

// TestTaskStatusStyles_BlockedIsDimAndFaint pins the issue #103 visual
// contract: the blocked-task glyph + label both render in faint dim
// style — not bold/red. The previous attentionChipStyle treatment
// conflated planning-blocked tasks with worker phase=blocked
// (raise-hand). With the styles dim, the row recedes; the chip-level
// attention signal (worker-driven) is the only red-alert surface left.
//
// Asserting the lipgloss Style fields directly (Faint + Foreground)
// avoids depending on a TTY profile in unit tests — lipgloss strips
// ANSI in non-TTY runs, so a substring check on the raw rendered
// output can't see the color.
func TestTaskStatusStyles_BlockedIsDimAndFaint(t *testing.T) {
	glyph, glyphStyle, labelStyle := taskStatusStyles("blocked")
	if glyph != "‖" {
		t.Errorf("blocked glyph want ‖, got %q", glyph)
	}
	if !glyphStyle.GetFaint() {
		t.Errorf("blocked glyph style must be faint (issue #103: planning state should recede)")
	}
	if !labelStyle.GetFaint() {
		t.Errorf("blocked label style must be faint (issue #103: planning state should recede)")
	}
	// colorDim ("243") is the dim foreground used across the dim
	// statuses (todo glyph, footer subtle text, etc). Pin it so a
	// future refactor that quietly swaps blocked back to red gets caught.
	if got := glyphStyle.GetForeground(); got != lipgloss.Color("243") {
		t.Errorf("blocked glyph foreground want colorDim (243), got %v", got)
	}
}

// TestRows_CursorOverridesStatusGlyph_AllStatuses extends PR #76's
// blocked-only cursor-override invariant to every status. When a row
// is selected, the cursor ▶ glyph wins over the status glyph so the
// operator's focus marker stays visible regardless of status.
//
// The label color still tracks status — that part is verified
// indirectly by the per-status glyph tests above (those run with
// selected=false; here we only assert cursor-wins-glyph).
func TestRows_CursorOverridesStatusGlyph_AllStatuses(t *testing.T) {
	cases := []struct {
		name   string
		status string
		// statusGlyph is the glyph that would render at !selected.
		// When selected, the row must NOT carry this glyph (cursor ▶
		// replaces it). One exception: in-progress's status glyph IS
		// ▶, which is identical to the cursor glyph — for that case
		// we only assert that ▶ is present (it's both the cursor
		// glyph AND would-be status glyph; the cursor takes the slot
		// either way, so the visible row is the same).
		statusGlyph string
	}{
		{"todo", "todo", "○"},
		{"ready", "ready", "◐"},
		{"in-progress", "in-progress", "▶"},
		{"in-review", "in-review", "⟳"},
		{"blocked", "blocked", "‖"}, // issue #103 + glyph polish: ‖ replaces ⏸ (which replaced ⚠)
		{"done", "done", "✓"},
		{"abandoned", "abandoned", "•"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := &taskRow{Slug: "x-1234", Status: tc.status}
			out := taskBlockLine(tr, 60, true)
			if !strings.Contains(out, "▶") {
				t.Errorf("selected %s task should carry ▶ cursor glyph, got: %q", tc.status, out)
			}
			// in-progress's status glyph is itself ▶, so we can't
			// assert "no status glyph". For every other status the
			// status glyph must be absent under the cursor.
			if tc.status != "in-progress" && strings.Contains(out, tc.statusGlyph) {
				t.Errorf("selected %s task should not carry %s status glyph (cursor wins), got: %q",
					tc.status, tc.statusGlyph, out)
			}
		})
	}
}
