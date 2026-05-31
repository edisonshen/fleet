package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// buildManyProjectsSnapshot returns a snapshot with `n` distinct projects
// (each its own ProjectRow) and no workers, so the LEFT column is the only
// thing that can overflow. Project names are "proj-aa", "proj-ab", ... so
// projectDisplayName rewrites them to "proj/aa" in the rendered body.
func buildManyProjectsSnapshot(n int) *Snapshot {
	snap := &Snapshot{LoadedAt: time.Now()}
	for i := 0; i < n; i++ {
		name := "proj-" + string(rune('a'+(i/26))) + string(rune('a'+(i%26)))
		snap.Projects = append(snap.Projects, &ProjectRow{
			Name: name, RepoSlug: name, Active: true,
		})
	}
	return snap
}

func projectDisplayWant(i int) string {
	return "proj/" + string(rune('a'+(i/26))) + string(rune('a'+(i%26)))
}

// TestRenderTwoColumnBody_LeftPanelBounded_LongProjectsList is the
// architect-level regression for the operator's 2026-05-29 bug: the
// header read "8 projects" but the left PROJECTS pane silently truncated
// to ~5 groups when project count exceeded the visible height, with NO
// scroll affordance (violates surface-dont-silo).
//
// On the PARENT commit (no left-panel bound) this fails: the off-screen
// projects are simply absent AND there is no "N hidden — [↓/↑] scroll"
// footer on the left column.
//
// Acceptance (1)+(3): every project is either rendered now OR reachable
// by scrolling the left offset, AND a visible "N hidden" affordance
// appears when projects overflow.
func TestRenderTwoColumnBody_LeftPanelBounded_LongProjectsList(t *testing.T) {
	withFleetHome(t)
	m := New("test")
	m.width = 140
	m.height = 20 // short terminal — 12 projects * 3 lines >> usable left rows
	m.dashboard = buildManyProjectsSnapshot(12)

	// The visible window at offset 0 must surface overflow via the
	// harmonized footer, NOT silently drop the off-screen projects.
	body := renderTwoColumnBody(m, 90, 40)
	if !strings.Contains(body, "hidden — [↓/↑] scroll") {
		t.Fatalf("left column must surface project overflow via the scroll-hint footer; got:\n%s", body)
	}

	// Every project must be reachable: render across the full scroll
	// range and assert each project name appears in at least one frame.
	maxOff := m.panelMaxOffset(rowProject)
	seen := map[string]bool{}
	for off := 0; off <= maxOff; off++ {
		m.projectsScrollOffset = off
		frame := renderTwoColumnBody(m, 90, 40)
		for i := 0; i < 12; i++ {
			if strings.Contains(frame, projectDisplayWant(i)) {
				seen[projectDisplayWant(i)] = true
			}
		}
	}
	for i := 0; i < 12; i++ {
		if !seen[projectDisplayWant(i)] {
			t.Errorf("project %q never visible across any scroll offset — silently truncated", projectDisplayWant(i))
		}
	}
}

// TestRenderTwoColumnBody_LeftPanel_NoFooterWhenFits: the left-panel
// footer must NOT appear when all projects fit the budget (clean look at
// the common case — no false overflow chrome on a tall terminal).
func TestRenderTwoColumnBody_LeftPanel_NoFooterWhenFits(t *testing.T) {
	withFleetHome(t)
	m := New("test")
	m.width = 140
	m.height = 60 // tall enough for 3 projects
	m.dashboard = buildManyProjectsSnapshot(3)

	body := renderTwoColumnBody(m, 90, 40)
	// All three project names present and no scroll footer.
	for i := 0; i < 3; i++ {
		if !strings.Contains(body, projectDisplayWant(i)) {
			t.Errorf("project %q missing on a tall terminal that fits all projects:\n%s", projectDisplayWant(i), body)
		}
	}
	if strings.Contains(body, "hidden — [↓/↑] scroll") {
		t.Errorf("left column must NOT show a scroll footer when all projects fit:\n%s", body)
	}
}

// TestArrowDown_ScrollsLeftProjectsPanel: pressing ↓ while the cursor is
// on a left-column (project) row advances projectsScrollOffset so the
// operator can reach the off-screen projects. Mirrors the #177 right-panel
// arrow-scroll contract on the left column.
func TestArrowDown_ScrollsLeftProjectsPanel(t *testing.T) {
	withFleetHome(t)
	m := New("test")
	m.width = 140
	m.height = 20
	m.dashboard = buildManyProjectsSnapshot(12)
	// Cursor starts at row 0 (first project, a LEFT-column row).
	if got := m.selectedRow(); got == nil || got.kind != rowProject {
		t.Fatalf("expected cursor on a project row to start; got %+v", got)
	}

	before := m.projectsScrollOffset
	updated, _ := m.Update(keyMsg("down"))
	m2 := updated.(Model)
	if m2.projectsScrollOffset <= before {
		t.Errorf("↓ on a project row must advance projectsScrollOffset; before=%d after=%d", before, m2.projectsScrollOffset)
	}

	// ↑ must bring it back down (clamped at 0, never negative).
	updated2, _ := m2.Update(keyMsg("up"))
	m3 := updated2.(Model)
	if m3.projectsScrollOffset < 0 {
		t.Errorf("projectsScrollOffset must never go negative; got %d", m3.projectsScrollOffset)
	}
	if m3.projectsScrollOffset >= m2.projectsScrollOffset {
		t.Errorf("↑ must reduce projectsScrollOffset; before=%d after=%d", m2.projectsScrollOffset, m3.projectsScrollOffset)
	}
}

// TestWindowResize_ResetsProjectsScrollOffset: the visible window changes
// on resize, so the stored left-panel offset resets to 0 (parity with the
// workers/agents offset reset in #177).
func TestWindowResize_ResetsProjectsScrollOffset(t *testing.T) {
	withFleetHome(t)
	m := New("test")
	m.dashboard = buildManyProjectsSnapshot(12)
	m.projectsScrollOffset = 5

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m2 := updated.(Model)
	if m2.projectsScrollOffset != 0 {
		t.Errorf("WindowSizeMsg must reset projectsScrollOffset to 0; got %d", m2.projectsScrollOffset)
	}
}

// TestAddProjectDone_RefreshesDashboard: a project added via [+] must
// trigger a dashboard reload (loadDashboardCmd) so the new row appears
// immediately. Guards acceptance (2): a newly-[+]-added project shows up.
func TestAddProjectDone_RefreshesDashboard(t *testing.T) {
	withFleetHome(t)
	m := New("test")
	// Success path: addProjectDoneMsg with no error must return a non-nil
	// command (the loadDashboardCmd batch) and clear picker state.
	updated, cmd := m.Update(addProjectDoneMsg{path: "/tmp/does-not-exist-proj", out: "added project spark"})
	m2 := updated.(Model)
	if cmd == nil {
		t.Fatalf("addProjectDoneMsg success must return a refresh command so the new project appears")
	}
	if m2.mode != modeNav {
		t.Errorf("addProjectDoneMsg success must drop to modeNav; got %v", m2.mode)
	}
	if m2.flash == nil || !strings.Contains(m2.flash.text, "spark") {
		t.Errorf("addProjectDoneMsg success must surface the CLI stdout; got %+v", m2.flash)
	}
}
