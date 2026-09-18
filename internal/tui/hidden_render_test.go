package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestSeparatorBlockLine_IdleCollapsedRendersCount(t *testing.T) {
	sep := &separatorRow{kind: separatorIdle, count: 3, expanded: false}
	line := separatorBlockLine(sep, 80, false)
	if !strings.Contains(line, "3 idle") {
		t.Errorf("collapsed idle separator should mention count: got %q", line)
	}
	if !strings.Contains(line, "[enter] to expand") {
		t.Errorf("collapsed idle separator should hint [enter]: got %q", line)
	}
}

func TestSeparatorBlockLine_IdleExpandedRendersCollapseHint(t *testing.T) {
	sep := &separatorRow{kind: separatorIdle, count: 3, expanded: true}
	line := separatorBlockLine(sep, 80, false)
	if !strings.Contains(line, "[enter] to collapse") {
		t.Errorf("expanded idle separator should hint collapse: got %q", line)
	}
}

func TestSeparatorBlockLine_HiddenRendersHiddenLabel(t *testing.T) {
	sep := &separatorRow{kind: separatorHidden, count: 1, expanded: false}
	line := separatorBlockLine(sep, 80, false)
	if !strings.Contains(line, "1 hidden") {
		t.Errorf("hidden separator should mention count: got %q", line)
	}
}

func TestSeparatorBlockLine_MoreRendersSearchHint(t *testing.T) {
	sep := &separatorRow{kind: separatorMore, count: 2}
	line := separatorBlockLine(sep, 80, false)
	if !strings.Contains(line, "2 more") {
		t.Errorf("more separator should mention count: got %q", line)
	}
	if !strings.Contains(line, "[/] search to find") {
		t.Errorf("more separator should hint search: got %q", line)
	}
}

func TestDashboardRows_CapsExpandedHiddenProjects(t *testing.T) {
	defer resetHiddenStubs()()
	names := make([]string, 12)
	projects := make([]*ProjectRow, len(names))
	for i := range names {
		names[i] = fmt.Sprintf("hidden-%02d", i)
		projects[i] = &ProjectRow{Name: names[i], RepoSlug: names[i]}
	}
	stubHidden(names...)
	m := New("test")
	m.showHidden = true
	m.hiddenExpanded = true
	m.dashboard = &Snapshot{Projects: projects}

	rows := m.dashboardRows()
	hiddenIdx := -1
	for i, row := range rows {
		if row.kind == rowSeparator && row.separator != nil && row.separator.kind == separatorHidden {
			hiddenIdx = i
			break
		}
	}
	if hiddenIdx < 0 {
		t.Fatalf("hidden separator missing: %+v", rows)
	}
	projectCount := 0
	for _, row := range rows[hiddenIdx+1:] {
		if row.kind == rowProject {
			projectCount++
		}
	}
	if projectCount != projectRenderCap {
		t.Fatalf("hidden project rows = %d, want %d", projectCount, projectRenderCap)
	}
	moreIdx := hiddenIdx + 1 + projectCount
	if moreIdx >= len(rows) || rows[moreIdx].kind != rowSeparator ||
		rows[moreIdx].separator.kind != separatorMore {
		t.Fatalf("more separator missing after capped projects: %+v", rows)
	}
	if rows[hiddenIdx].separator.count != 12 || rows[moreIdx].separator.count != 2 {
		t.Errorf("separator counts = %d and %d, want 12 and 2",
			rows[hiddenIdx].separator.count, rows[moreIdx].separator.count)
	}
}

func TestDashboardRows_NoHiddenMoreSeparatorAtCap(t *testing.T) {
	defer resetHiddenStubs()()
	names := make([]string, projectRenderCap)
	projects := make([]*ProjectRow, len(names))
	for i := range names {
		names[i] = fmt.Sprintf("hidden-%02d", i)
		projects[i] = &ProjectRow{Name: names[i], RepoSlug: names[i]}
	}
	stubHidden(names...)
	m := New("test")
	m.showHidden = true
	m.hiddenExpanded = true
	m.dashboard = &Snapshot{Projects: projects}

	for _, row := range m.dashboardRows() {
		if row.kind == rowSeparator && row.separator != nil &&
			row.separator.kind == separatorMore {
			t.Fatal("more separator should not render at the cap")
		}
	}
}

func TestDashboardRows_SearchRendersAllExpandedHiddenProjects(t *testing.T) {
	defer resetHiddenStubs()()
	names := make([]string, 12)
	projects := make([]*ProjectRow, len(names))
	for i := range names {
		names[i] = fmt.Sprintf("hidden-%02d", i)
		projects[i] = &ProjectRow{Name: names[i], RepoSlug: names[i]}
	}
	stubHidden(names...)
	m := New("test")
	m.showHidden = true
	m.hiddenExpanded = true
	m.searchFilter = names[len(names)-1]
	m.dashboard = &Snapshot{Projects: projects}

	rows := m.dashboardRows()
	found := false
	for _, row := range rows {
		if row.kind == rowProject && row.project != nil && row.project.Name == names[len(names)-1] {
			found = true
		}
		if row.kind == rowSeparator && row.separator != nil &&
			row.separator.kind == separatorMore {
			t.Fatal("search should bypass hidden render cap")
		}
	}
	if !found {
		t.Fatalf("search result %q missing from rows: %+v", names[len(names)-1], rows)
	}
}

func TestSeparatorBlockLine_SelectedRendersCursorGlyph(t *testing.T) {
	sep := &separatorRow{kind: separatorIdle, count: 2}
	line := separatorBlockLine(sep, 80, true)
	if !strings.Contains(line, "▶") {
		t.Errorf("selected separator should render cursor glyph: got %q", line)
	}
}

func TestRenderDashboardFooter_HiddenChipShown(t *testing.T) {
	out := renderDashboardFooterWithHidden(
		time.Minute, 200, "", 3, 0,
	)
	if !strings.Contains(out, "3 hidden") {
		t.Errorf("footer should surface hidden chip when count > 0: %q", out)
	}
	if !strings.Contains(out, "[c] view") {
		t.Errorf("footer chip should hint [c] view: %q", out)
	}
}

func TestRenderDashboardFooter_HiddenChipOmittedWhenZero(t *testing.T) {
	out := renderDashboardFooterWithHidden(time.Minute, 200, "", 0, 0)
	if strings.Contains(out, "hidden") {
		t.Errorf("zero hidden should omit chip, got %q", out)
	}
}

func TestRenderDashboardFooter_HiddenWithActivityChip(t *testing.T) {
	out := renderDashboardFooterWithHidden(time.Minute, 200, "", 3, 1)
	if !strings.Contains(out, "1 with activity") {
		t.Errorf("footer should surface activity nudge when hiddenWith > 0: %q", out)
	}
}

func TestRenderDashboardFooter_HiddenWithActivityZeroOmitsActivity(t *testing.T) {
	out := renderDashboardFooterWithHidden(time.Minute, 200, "", 3, 0)
	if strings.Contains(out, "with activity") {
		t.Errorf("zero hiddenWith should not append activity nudge: %q", out)
	}
}

// TestView_FooterIncludesHiddenChip is the integration test: an
// operator with a hidden list should see the chip on the dashboard.
func TestView_FooterIncludesHiddenChip(t *testing.T) {
	defer resetHiddenStubs()()
	stubHidden("alpha", "beta")

	m := New("test")
	m.width = 200
	m.height = 40
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{{Name: "live", Active: true}},
	}
	out := m.View()
	if !strings.Contains(out, "2 hidden") {
		t.Errorf("dashboard footer should surface hidden chip: %q", out)
	}
}

// TestView_HiddenSeparatorRendered tests the separator string surfaces
// in the rendered output when show-hidden is on.
func TestView_HiddenSeparatorRendered(t *testing.T) {
	defer resetHiddenStubs()()
	stubHidden("alpha")

	m := New("test")
	m.width = 200
	m.height = 40
	m.showHidden = true
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{
			{Name: "alpha"},
			{Name: "beta", Active: true},
		},
	}
	out := m.View()
	if !strings.Contains(out, "1 hidden") {
		t.Errorf("dashboard should render '1 hidden' separator: %q", out)
	}
}

// TestView_IdleSeparatorRenderedWithActiveProjects tests the idle
// separator surfaces when there's an ACTIVE/IDLE split.
func TestView_IdleSeparatorRenderedWithActiveProjects(t *testing.T) {
	defer resetHiddenStubs()()
	stubHidden()

	now := time.Now()
	prevNow := nowFn
	nowFn = func() time.Time { return now }
	defer func() { nowFn = prevNow }()

	m := New("test")
	m.width = 200
	m.height = 40
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{
			{Name: "alpha", Active: true},
			{Name: "beta"},  // idle
			{Name: "gamma"}, // idle
		},
	}
	out := m.View()
	if !strings.Contains(out, "2 idle") {
		t.Errorf("dashboard should render '2 idle' separator: %q", out)
	}
}

func TestApplyHiddenStyle_PreservesLengthAndEmpty(t *testing.T) {
	in := []string{"line one", "", "line two"}
	out := applyHiddenStyle(in)
	if len(out) != len(in) {
		t.Errorf("applyHiddenStyle should preserve length: got %d, want %d", len(out), len(in))
	}
	if out[1] != "" {
		t.Errorf("empty line should pass through unchanged, got %q", out[1])
	}
	// In non-TTY test runs lipgloss strips ANSI codes, so the styled
	// output may equal the input verbatim. The contract we test is:
	// non-empty lines contain the original text (not dropped) AND the
	// helper doesn't crash on mixed empty / non-empty input. Cell-
	// width math elsewhere depends only on visible characters, which
	// the no-op style preserves.
	for i, ln := range in {
		if ln != "" && !strings.Contains(out[i], ln) {
			t.Errorf("non-empty line should be preserved, got %q", out[i])
		}
	}
}
