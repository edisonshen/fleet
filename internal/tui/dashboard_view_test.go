package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
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

// dashboard-accumulation-f-4421 Sub-fix B: pin the rendered chrome for
// the right-column agent-idle separator. The text must signal the
// count + the toggle keybind so the operator knows how to expand.
func TestSeparatorBlockLine_AgentIdle_CollapsedLabel(t *testing.T) {
	sep := &separatorRow{kind: separatorAgentIdle, count: 12, expanded: false}
	line := separatorBlockLine(sep, 60, false)
	if !strings.Contains(line, "12 idle") {
		t.Errorf("collapsed agent-idle separator must include count '12 idle'; got: %q", line)
	}
	if !strings.Contains(line, "[enter] to expand") {
		t.Errorf("collapsed agent-idle separator must include the [enter] hint; got: %q", line)
	}
}

func TestSeparatorBlockLine_AgentIdle_ExpandedLabel(t *testing.T) {
	sep := &separatorRow{kind: separatorAgentIdle, count: 3, expanded: true}
	line := separatorBlockLine(sep, 60, false)
	if !strings.Contains(line, "3 idle") {
		t.Errorf("expanded agent-idle separator must include count '3 idle'; got: %q", line)
	}
	if !strings.Contains(line, "[enter] to collapse") {
		t.Errorf("expanded agent-idle separator must include the [enter] collapse hint; got: %q", line)
	}
}

// /review iter-1 [P0] regression: the right-column separatorAgentIdle
// row must NEVER bleed into the LEFT column. dashboardRows returns left
// and right rows in one slice; without an explicit skip in the LEFT-
// column loop, the agent-idle separator was being appended to BOTH
// columns (rendering twice).
func TestBuildBodyLines_AgentIdleSeparatorRendersRightOnly(t *testing.T) {
	withFleetHome(t)
	now := time.Now()
	stale := now.Add(-30 * 24 * time.Hour)
	m := New("test")
	m.width = 140
	m.height = 30
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{
			{Name: "fleet", RepoSlug: "fleet", Active: true},
		},
		LoadedAt: now,
	}
	m.records = []*agent.Record{
		{
			ID:             "aaaa0001",
			Project:        "fleet",
			TmuxSession:    "fleet-aaaa0001",
			LastActivityTS: stale, SpawnedAt: stale,
		},
	}

	leftLines, rightLines := buildBodyLines(m, 90, 40)
	leftAll := strings.Join(leftLines, "\n")
	rightAll := strings.Join(rightLines, "\n")

	if strings.Contains(leftAll, "1 idle") {
		t.Errorf("agent-idle separator '1 idle' leaked into LEFT column:\n%s", leftAll)
	}
	if !strings.Contains(rightAll, "1 idle") {
		t.Errorf("agent-idle separator '1 idle' missing from RIGHT column:\n%s", rightAll)
	}
}
