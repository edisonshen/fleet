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

	leftLines, workerLines, agentLines := buildBodyLines(m, 90, 40)
	leftAll := strings.Join(leftLines, "\n")
	rightAll := strings.Join(append(workerLines, agentLines...), "\n")

	if strings.Contains(leftAll, "1 idle") {
		t.Errorf("agent-idle separator '1 idle' leaked into LEFT column:\n%s", leftAll)
	}
	if !strings.Contains(rightAll, "1 idle") {
		t.Errorf("agent-idle separator '1 idle' missing from RIGHT column:\n%s", rightAll)
	}
}

// ----- fleet#177 TUI right-panel bound tests (Fix 2) -----
//
// renderTwoColumnBody must reserve >=50% of usable vertical rows for the
// LEFT projects column. When the right column (workers + agents) is
// long, overflow rolls into a "K hidden — [↓/↑] scroll" footer instead
// of pushing left-column rows off-screen. Arrow-key scroll inside the
// focused right panel; left-column arrow nav untouched.

// buildLargeWorkerSnapshot helper — synthesizes a snapshot with N
// worker rows so tests can exercise overflow paths without needing real
// fleet state on disk. The synthetic workers all live under one
// project so the LEFT column is small (one project block, fixed).
func buildLargeWorkerSnapshot(projectName string, workers int) *Snapshot {
	snap := &Snapshot{
		Projects: []*ProjectRow{
			{Name: projectName, RepoSlug: projectName, Active: true},
		},
		LoadedAt: time.Now(),
	}
	for i := 0; i < workers; i++ {
		snap.Workers = append(snap.Workers, &WorkerRow{
			Project: projectName,
			Slug:    "synth-worker-" + string(rune('a'+(i%26))) + string(rune('a'+((i/26)%26))),
			Phase:   "review-pending",
		})
	}
	return snap
}

// TestRenderTwoColumnBody_BoundedRightPanel_LongWorkersList: 30 workers
// on a 24-row terminal. The left projects column must still render its
// full block (3 lines for the single project + 1 blank); the right
// column shows the first K-1 workers + a footer line naming the
// overflow + scroll hint.
func TestRenderTwoColumnBody_BoundedRightPanel_LongWorkersList(t *testing.T) {
	withFleetHome(t)
	m := New("test")
	m.width = 140
	m.height = 24 // short terminal
	m.dashboard = buildLargeWorkerSnapshot("fleet", 30)

	body := renderTwoColumnBody(m, 90, 40)
	// The body must include the project name (left column not pushed
	// off-screen) AND the scroll hint footer (right column overflow).
	if !strings.Contains(body, "fleet") {
		t.Errorf("project name 'fleet' missing from rendered body — left column likely pushed off-screen:\n%s", body)
	}
	if !strings.Contains(body, "[↓/↑] scroll") {
		t.Errorf("expected '[↓/↑] scroll' overflow hint in right column body; got:\n%s", body)
	}
	if !strings.Contains(body, "hidden") {
		t.Errorf("expected 'hidden' count in scroll footer; got:\n%s", body)
	}
}

// TestRenderTwoColumnBody_BoundedRightPanel_LongAgentsList: 30 agent
// rows on a 24-row terminal. Same bounded-projects invariant.
func TestRenderTwoColumnBody_BoundedRightPanel_LongAgentsList(t *testing.T) {
	withFleetHome(t)
	m := New("test")
	m.width = 140
	m.height = 24
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{
			{Name: "fleet", RepoSlug: "fleet", Active: true},
		},
		LoadedAt: time.Now(),
	}
	// 30 agent records.
	for i := 0; i < 30; i++ {
		id := "aaaa" + string(rune('a'+(i%26))) + string(rune('a'+((i/26)%26))) + "00"
		m.records = append(m.records, sampleAgent(id))
	}

	body := renderTwoColumnBody(m, 90, 40)
	if !strings.Contains(body, "fleet") {
		t.Errorf("project name 'fleet' missing — left column pushed off-screen:\n%s", body)
	}
	// Either the right-panel scroll hint OR the existing agents-only
	// "[c] view" hint must fire; the new bound applies the unified
	// scroll affordance.
	if !strings.Contains(body, "[↓/↑] scroll") {
		t.Errorf("expected '[↓/↑] scroll' overflow hint with 30 agents; got:\n%s", body)
	}
}

// TestRenderTwoColumnBody_TallTerminal_NoScrollNeeded: 60-row terminal
// with a comfortable 7 agents + 4 workers. No overflow → no footer
// hint rendered.
func TestRenderTwoColumnBody_TallTerminal_NoScrollNeeded(t *testing.T) {
	withFleetHome(t)
	m := New("test")
	m.width = 140
	m.height = 60
	m.dashboard = buildLargeWorkerSnapshot("fleet", 4)
	for i := 0; i < 7; i++ {
		id := "bbbb" + string(rune('a'+(i%26))) + string(rune('a'+((i/26)%26))) + "00"
		m.records = append(m.records, sampleAgent(id))
	}

	body := renderTwoColumnBody(m, 90, 40)
	if strings.Contains(body, "[↓/↑] scroll") {
		t.Errorf("tall terminal must NOT render scroll-hint footer; got:\n%s", body)
	}
}

// TestRenderTwoColumnBody_ProjectsReservedHalf: the left projects
// column reserves >= 50% of usable rows (minProjectsRows floor: 6).
// On a 60-row terminal this fits all 6 projects (each ~3-4 lines)
// — the load-bearing invariant against the operator's screenshot bug
// where 50+ workers pushed projects off-screen. The pre-#177 layout
// would render the right column unbounded; the new bound keeps the
// left half reserved regardless of right-column overflow.
func TestRenderTwoColumnBody_ProjectsReservedHalf(t *testing.T) {
	withFleetHome(t)
	m := New("test")
	m.width = 140
	m.height = 60

	// 6 projects + 50 workers in one project. The unbounded right
	// column would burn through the full body before the left finishes;
	// the bounded layout caps it at 60/40 of the right budget.
	m.dashboard = &Snapshot{
		LoadedAt: time.Now(),
	}
	for i := 0; i < 6; i++ {
		name := "proj-" + string(rune('a'+i))
		m.dashboard.Projects = append(m.dashboard.Projects, &ProjectRow{
			Name: name, RepoSlug: name, Active: true,
		})
	}
	for i := 0; i < 50; i++ {
		m.dashboard.Workers = append(m.dashboard.Workers, &WorkerRow{
			Project: "proj-a",
			Slug:    "synth-" + string(rune('a'+(i%26))) + string(rune('a'+((i/26)%26))),
			Phase:   "review-pending",
		})
	}

	body := renderTwoColumnBody(m, 90, 40)
	// Every project name must be present in the rendered body — none
	// pushed off-screen by right-column overflow. projectDisplayName
	// rewrites "proj-X" as "proj/X" (first hyphen → slash) so the
	// assertion uses the display form.
	for i := 0; i < 6; i++ {
		want := "proj/" + string(rune('a'+i))
		if !strings.Contains(body, want) {
			t.Errorf("project %q missing from body — left column truncated by right-column overflow:\n%s", want, body)
		}
	}
	// The right column must surface the workers overflow via the
	// scroll-hint footer rather than rendering all 50 workers (which
	// is what the operator's 2026-05-27 screenshot showed).
	if !strings.Contains(body, "hidden — [↓/↑] scroll") {
		t.Errorf("right column must surface workers overflow via scroll-hint footer; got:\n%s", body)
	}
}

// TestRenderTwoColumnBody_ScrollHintHarmonized: the overflow hint must
// match the "K hidden — <action>" shape used by the existing agents
// "K hidden — [c] view" footer. Operator's muscle memory expects the
// same prefix shape on both bounds.
func TestRenderTwoColumnBody_ScrollHintHarmonized(t *testing.T) {
	withFleetHome(t)
	m := New("test")
	m.width = 140
	m.height = 24
	m.dashboard = buildLargeWorkerSnapshot("fleet", 30)

	body := renderTwoColumnBody(m, 90, 40)
	// Shape: "<N> hidden — [↓/↑] scroll" (mirrors agents footer).
	if !strings.Contains(body, "hidden — [↓/↑] scroll") {
		t.Errorf("scroll-hint footer not harmonized with existing 'K hidden — <action>' shape; got:\n%s", body)
	}
}

// coordFooterLine returns the "coord <id>…" line from a projectFooterLines
// result (or "" when none). The coord-id line is the only one carrying the
// "coord " label; the counts/status line never does, and the spawn-fallback
// text reads "spawning coord..." (no trailing space), so a "coord " match is
// unambiguous. lipgloss renders plain in a non-TTY `go test`, so the returned
// line is directly comparable/searchable without ANSI stripping.
func coordFooterLine(lines []string) string {
	for _, l := range lines {
		if strings.Contains(l, "coord ") {
			return l
		}
	}
	return ""
}

// TestProjectFooterLines_CoordContext pins the left-column coord line's
// context-% chip: projectFooterLines resolves the coord's ContextPct from
// ctx.records BY p.CoordID (not the first record) and appends renderContextBar
// inline after the id (`coord <id> 49%`). A coord whose record has a nil
// ContextPct — or no matching record at all — renders exactly "coord <id>"
// with no trailing bar or space, unchanged from before the chip. The decoy
// record makes a first-record-wins lookup bug fail the "49% and not 8%" case.
func TestProjectFooterLines_CoordContext(t *testing.T) {
	// Keep the coord-spawn fallback (CoordID=="" row) deterministic
	// regardless of the host's ~/.fleet lease state.
	prev := coordSpawnIdentityFn
	coordSpawnIdentityFn = func(string) string { return "" }
	t.Cleanup(func() { coordSpawnIdentityFn = prev })

	rec := func(id string, pct *float64) *agent.Record {
		return &agent.Record{ID: id, ContextPct: pct}
	}

	tests := []struct {
		name     string
		coordID  string
		records  []*agent.Record
		wantLine string // exact-equality expectation (ANSI-free in test)
		wantSub  string // substring the coord line must contain
		wantNot  string // substring the coord line must NOT contain
		noCoord  bool   // true → no coord-id line at all
	}{
		{
			name:    "context chip keyed on coord id, decoy ignored",
			coordID: "abcd1234",
			records: []*agent.Record{
				rec("zzzz9999", floatPtr(8.0)),  // decoy first
				rec("abcd1234", floatPtr(49.0)), // the coord
			},
			wantSub: "49%",
			wantNot: "8%",
		},
		{
			name:     "nil context renders bare coord line",
			coordID:  "abcd1234",
			records:  []*agent.Record{rec("abcd1234", nil)},
			wantLine: "coord abcd1234",
		},
		{
			name:     "no matching record renders bare coord line",
			coordID:  "abcd1234",
			records:  []*agent.Record{rec("zzzz9999", floatPtr(8.0))},
			wantLine: "coord abcd1234",
		},
		{
			name:    "empty coord id renders no coord line",
			coordID: "",
			records: nil,
			noCoord: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &ProjectRow{Name: "fleet", RepoSlug: "fleet", CoordID: tt.coordID}
			lines := projectFooterLines(p, 80, "", coordSpawnCtx{records: tt.records})
			coordLine := coordFooterLine(lines)

			if tt.noCoord {
				if coordLine != "" {
					t.Fatalf("CoordID=\"\" must render no coord line; got %q", coordLine)
				}
				return
			}
			if coordLine == "" {
				t.Fatalf("expected a coord line; got none in %q", lines)
			}
			if tt.wantLine != "" && coordLine != tt.wantLine {
				t.Errorf("coord line = %q, want exactly %q", coordLine, tt.wantLine)
			}
			if tt.wantSub != "" && !strings.Contains(coordLine, tt.wantSub) {
				t.Errorf("coord line %q missing %q", coordLine, tt.wantSub)
			}
			if tt.wantNot != "" && strings.Contains(coordLine, tt.wantNot) {
				t.Errorf("coord line %q must not contain decoy %q", coordLine, tt.wantNot)
			}
		})
	}
}
