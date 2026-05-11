package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// Issue #90: Up/Down arrows are silent aliases for k/j; footer drops
// nav + panel chips (intuitive enough to stay implicit). j/k still
// work; ←/→ panel-switch from PR #85 still works. Help overlay is
// the canonical surface for arrows + j/k + ←/→ discoverability.

// TestKeyDownArrow_MovesCursorDown pins issue #90: pressing the Down
// arrow moves the cursor down by one row, identical to pressing j.
// Down is already wired in model.handleKey ("j", "down" arm) — this
// test is the regression guard so a future refactor can't quietly
// drop the alias.
func TestKeyDownArrow_MovesCursorDown(t *testing.T) {
	m := New("test")
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{
			{Name: "demo", RepoSlug: "demo"},
			{Name: "other", RepoSlug: "other"},
		},
		LoadedAt: time.Now(),
	}
	m.dashCursor = 0

	updated, _ := m.Update(keyMsg("down"))
	mm := updated.(Model)
	if mm.dashCursor != 1 {
		t.Errorf("after ↓: dashCursor=%d, want 1 (alias for j)", mm.dashCursor)
	}
}

// TestKeyUpArrow_MovesCursorUp pins issue #90: pressing the Up arrow
// moves the cursor up by one row, identical to pressing k. Wraps at
// the top boundary the same way k does.
func TestKeyUpArrow_MovesCursorUp(t *testing.T) {
	m := New("test")
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{
			{Name: "demo", RepoSlug: "demo"},
			{Name: "other", RepoSlug: "other"},
		},
		LoadedAt: time.Now(),
	}
	m.dashCursor = 1

	updated, _ := m.Update(keyMsg("up"))
	mm := updated.(Model)
	if mm.dashCursor != 0 {
		t.Errorf("after ↑: dashCursor=%d, want 0 (alias for k)", mm.dashCursor)
	}
}

// TestKeyJK_StillWork is the regression test pinning that j/k still
// move the cursor after issue #90 lands. The arrow aliases are
// additive; removing j/k would silently break vim-flavored operators.
func TestKeyJK_StillWork(t *testing.T) {
	m := New("test")
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{
			{Name: "demo", RepoSlug: "demo"},
			{Name: "other", RepoSlug: "other"},
		},
		LoadedAt: time.Now(),
	}
	m.dashCursor = 0

	updated, _ := m.Update(keyMsg("j"))
	mm := updated.(Model)
	if mm.dashCursor != 1 {
		t.Errorf("after j: dashCursor=%d, want 1", mm.dashCursor)
	}
	updated, _ = mm.Update(keyMsg("k"))
	mm = updated.(Model)
	if mm.dashCursor != 0 {
		t.Errorf("after k: dashCursor=%d, want 0", mm.dashCursor)
	}
}

// TestFooter_OmitsNavAndPanelChips pins issue #90: the footer must
// NOT advertise nav (j/k) or panel (←/→) chips. Both are intuitive
// enough to stay silent; the [?] help overlay carries the
// discoverability surface.
func TestFooter_OmitsNavAndPanelChips(t *testing.T) {
	out := renderDashboardFooter(0, 200, "")
	for _, banned := range []string{"j/k", "←/→", "[j", "[k", "] nav", "] panel"} {
		if strings.Contains(out, banned) {
			t.Errorf("footer must not contain %q (issue #90), got:\n%s", banned, out)
		}
	}
}

// TestFooter_DocumentsCoreKeysOnly pins the canonical footer shape
// post-tui-footer-swap-n-for-pl-ce97 (operator polish): swap [n] task
// for [+] add project — coords handle most task creation, so
// register-project earns the chip slot. The [n] keybind in keys.go
// stays wired (and [n] stays in the help overlay) but the footer no
// longer advertises it.
//
// Canonical: ⏎ open · + add project · a attach · h handoff · x archive ·
// / search · ? help · q quit. Eight chips total — the count is
// invariant; only the second slot changed.
func TestFooter_DocumentsCoreKeysOnly(t *testing.T) {
	out := renderDashboardFooter(0, 200, "")
	for _, want := range []string{"⏎", "+", "a", "h", "x", "/", "?", "q",
		"open", "add project", "attach", "handoff", "archive", "search", "help", "quit"} {
		if !strings.Contains(out, want) {
			t.Errorf("footer missing required chip %q, got:\n%s", want, out)
		}
	}
}

// TestFooter_DropsNTaskAdvertisement pins the swap direction: the
// footer must NOT advertise [n] task anymore. The keybind itself
// still works (keys.go unchanged) but the chip strip's second slot
// belongs to [+] add project now.
func TestFooter_DropsNTaskAdvertisement(t *testing.T) {
	out := renderDashboardFooter(0, 200, "")
	if strings.Contains(out, "[n]") {
		t.Errorf("footer must not advertise [n] anymore, got:\n%s", out)
	}
	// Also guard against the un-bracketed " task" label slipping through
	// even if the [n] chip itself is gone (defensive — the only "task"
	// substring in the old footer was the [n] task chip).
	if strings.Contains(out, "] task") {
		t.Errorf("footer must not carry '] task' chip suffix, got:\n%s", out)
	}
}

// TestHelpOverlay_DocumentsArrowsAndJKAndLeftRight pins issue #90's
// discoverability requirement: with the footer silent on nav + panel,
// the [?] help overlay must document arrows + j/k + ←/→ so an
// operator who pressed [?] can still find every navigation key.
func TestHelpOverlay_DocumentsArrowsAndJKAndLeftRight(t *testing.T) {
	out := renderHelpOverlay(120)
	for _, want := range []string{"j", "k", "↑", "↓", "←", "→"} {
		if !strings.Contains(out, want) {
			t.Errorf("help overlay missing nav key %q, got:\n%s", want, out)
		}
	}
}

// TestFooter_FitsCommonSplitPaneWidth pins codex iter-1 P2: adding
// [h] handoff + [x] archive chips pushed the legend past the
// renderFooter() usable-width budget (m.width - 1) on 100-column
// split panes. The fix tightens the inter-chip separator from two
// spaces to one. Regress this here so a future "make it pretty"
// pass that re-widens the gap can't silently re-introduce wrap.
//
// Anchor bumped to a 110-column terminal (usable=109) post the
// [n] task → [+] add project chip swap (tui-footer-swap-n-for-pl):
// the new "add project" label is 7 cells longer than "task", which
// pushes the legend over the 100-col budget. 110 is still well
// within the "common split pane" envelope on a 13-inch laptop and
// preserves the regression intent (no silent wrap at common widths).
func TestFooter_FitsCommonSplitPaneWidth(t *testing.T) {
	const splitPaneWidth = 109 // m.width=110 → usable = m.width - 1
	out := renderDashboardFooter(0, splitPaneWidth, "")
	// renderDashboardFooter pads with spaces to reach `usable`; if the
	// content alone exceeds that budget, gap collapses to 1 and the
	// rendered string overruns. Width should be exactly splitPaneWidth.
	if got := lipgloss.Width(out); got > splitPaneWidth {
		t.Errorf("footer rendered %d cells wide; must fit in %d (110-col terminal)",
			got, splitPaneWidth)
	}
}
