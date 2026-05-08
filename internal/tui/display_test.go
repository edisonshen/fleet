package tui

import (
	"strings"
	"testing"
	"time"
)

// TestProjectDisplayName_EncodedHyphenToSlash pins the canonical case
// from issue #66: cwd-encoded "projects-fleet" renders as
// "projects/fleet" in the dashboard header.
func TestProjectDisplayName_EncodedHyphenToSlash(t *testing.T) {
	got := projectDisplayName("projects-fleet")
	want := "projects/fleet"
	if got != want {
		t.Errorf("projectDisplayName(%q) = %q, want %q", "projects-fleet", got, want)
	}
}

// TestProjectDisplayName_NoHyphenPassesThrough verifies bare names
// (no encoding to undo) come through untouched.
func TestProjectDisplayName_NoHyphenPassesThrough(t *testing.T) {
	got := projectDisplayName("single")
	want := "single"
	if got != want {
		t.Errorf("projectDisplayName(%q) = %q, want %q", "single", got, want)
	}
}

// TestProjectDisplayName_OnlyFirstHyphenFlipped guards the heuristic
// from going greedy: multi-hyphen names like "work-my-app" must keep
// their tail hyphens intact (issue #66 explicitly chose ambiguity over
// guessing — only the first hyphen flips to a slash).
func TestProjectDisplayName_OnlyFirstHyphenFlipped(t *testing.T) {
	got := projectDisplayName("work-my-app")
	want := "work/my-app"
	if got != want {
		t.Errorf("projectDisplayName(%q) = %q, want %q", "work-my-app", got, want)
	}
}

// TestProjectDisplayName_EmptyStringPassesThrough is a defensive case:
// the helper never panics or inserts a stray slash on an empty input.
func TestProjectDisplayName_EmptyStringPassesThrough(t *testing.T) {
	got := projectDisplayName("")
	want := ""
	if got != want {
		t.Errorf("projectDisplayName(%q) = %q, want %q", "", got, want)
	}
}

// TestDashboard_ProjectHeaderUsesDisplayName drives the rendered view
// end-to-end: a project named "projects-fleet" on disk must appear in
// the dashboard output as "projects/fleet" (the encoded form must NOT
// appear as a header substring).
//
// Substring guard rationale: a naive contains-check on "projects/fleet"
// would also pass if the renderer accidentally printed both forms. We
// additionally assert the encoded "projects-fleet" string is absent
// from the rendered output to catch a regression where someone renders
// both name and display name.
func TestDashboard_ProjectHeaderUsesDisplayName(t *testing.T) {
	pdir := withFleetHome(t)
	seedTasks(t, pdir, "projects-fleet", TaskCounts{Todo: 1})

	m := New("test")
	m.width = 130
	m.height = 30
	m.dashboard = scanDashboard(time.Now())

	out := m.View()
	if !strings.Contains(out, "projects/fleet") {
		t.Errorf("dashboard should render project header as %q, got:\n%s", "projects/fleet", out)
	}
	if strings.Contains(out, "projects-fleet") {
		t.Errorf("dashboard should NOT contain encoded form %q after display transform, got:\n%s", "projects-fleet", out)
	}
}

// TestDashboard_SearchMatchesEncodedFormAfterDisplayTransform pins the
// regression invariant from issue #66 acceptance: even though the
// header now reads "projects/fleet", typing the encoded substring
// "projects-fl" must still narrow dashboardRows() to that project. The
// display transform is rendering-only — search/filter identity stays
// on the encoded form.
func TestDashboard_SearchMatchesEncodedFormAfterDisplayTransform(t *testing.T) {
	pdir := withFleetHome(t)
	seedTasks(t, pdir, "projects-fleet", TaskCounts{Todo: 1})
	seedTasks(t, pdir, "other-repo", TaskCounts{Todo: 1})

	m := New("test")
	m.width = 130
	m.height = 30
	m.dashboard = scanDashboard(time.Now())

	m.searchFilter = "projects-fl"
	rows := m.dashboardRows()

	var sawTarget bool
	for _, r := range rows {
		if r.kind == rowProject && r.project != nil && r.project.Name == "projects-fleet" {
			sawTarget = true
		}
		if r.kind == rowProject && r.project != nil && r.project.Name == "other-repo" {
			t.Errorf("filter %q should exclude other-repo, got it in rows", "projects-fl")
		}
	}
	if !sawTarget {
		t.Errorf("filter %q should still match encoded project name %q (display transform must not affect search), rows=%d",
			"projects-fl", "projects-fleet", len(rows))
	}
}
