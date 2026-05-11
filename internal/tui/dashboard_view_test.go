package tui

import (
	"strings"
	"testing"
)

// TestDashboard_RendersPostArchiveBadge pins the visual contract for
// the subagent-lifecycle audit: when ANY archived subagent record in
// a project carries a non-empty post_archive_artifacts list, the
// project row gets a "⚠ post-archive activity" indicator. The operator
// scans the dashboard, spots the warning, and drills in via the
// per-project record to see which bonus PR / branch push fired.
//
// PR #124 was the motivating violation: a subagent finished its 8-bullet
// README task, returned the §7 contract, then opened a separate PR
// adding a 9th bullet. Without a dashboard signal the operator had no
// way to discover the drift short of grepping `gh pr list`.
//
// Pure formatter test — feeds a ProjectRow with PostArchiveActivity=true
// directly through projectBlockLines (the rendering function called by
// the dashboard body). No filesystem scan, no gh API.
func TestDashboard_RendersPostArchiveBadge(t *testing.T) {
	p := &ProjectRow{
		Name:                "fleet",
		RepoSlug:            "edisonshen/fleet",
		Counts:              TaskCounts{Todo: 1},
		PostArchiveActivity: true,
	}
	lines := projectBlockLines(p, 80, false, coordSpawnCtx{})
	out := strings.Join(lines, "\n")
	if !strings.Contains(out, "post-archive activity") {
		t.Errorf("project row with PostArchiveActivity=true should render "+
			"the badge text 'post-archive activity', got:\n%s", out)
	}
	if !strings.Contains(out, "⚠") {
		t.Errorf("project row with PostArchiveActivity=true should carry "+
			"a ⚠ warning glyph, got:\n%s", out)
	}
}

// TestDashboard_OmitsPostArchiveBadgeWhenFalse pins the negative case
// — the absence of any post-archive flag means the project row stays
// clean. Without this guard a regression that hard-codes the badge
// would slip past the positive test.
func TestDashboard_OmitsPostArchiveBadgeWhenFalse(t *testing.T) {
	p := &ProjectRow{
		Name:                "fleet",
		RepoSlug:            "edisonshen/fleet",
		Counts:              TaskCounts{Todo: 1},
		PostArchiveActivity: false,
	}
	lines := projectBlockLines(p, 80, false, coordSpawnCtx{})
	out := strings.Join(lines, "\n")
	if strings.Contains(out, "post-archive activity") {
		t.Errorf("clean project row must NOT carry post-archive badge, got:\n%s", out)
	}
}
