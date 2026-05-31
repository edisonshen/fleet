package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/edisonshen/fleet/internal/agent"
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

// TestRenderTwoColumnBody_LeftPanel_OverflowSurfacesFooter is the
// behavior-only regression (no reference to the new field) that fails on
// the PARENT commit: with more projects than fit, the parent renderer
// silently dropped the overflow with no affordance, so the body contained
// neither the off-screen project names nor a "N hidden" footer. The fix
// surfaces a footer (and the names become reachable via scroll — covered
// by the LongProjectsList test). This one stays green-on-parent-impossible
// purely from public render output.
func TestRenderTwoColumnBody_LeftPanel_OverflowSurfacesFooter(t *testing.T) {
	withFleetHome(t)
	m := New("test")
	m.width = 140
	m.height = 20
	m.dashboard = buildManyProjectsSnapshot(12)

	body := renderTwoColumnBody(m, 90, 40)
	if !strings.Contains(body, "hidden — [↓/↑] scroll") {
		t.Errorf("left column silently truncates without a scroll affordance (surface-dont-silo violation); got:\n%s", body)
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

// TestArrowDown_ScrollsLeftProjectsPanel: walking ↓ through the left
// column eventually advances projectsScrollOffset so the operator reaches
// the off-screen projects, and ↑ brings it back. The offset is aligned to
// the cursor's rendered line (not bumped a fixed amount per press), so the
// FIRST ↓ near the top may leave the offset at 0 — the invariant is that
// it advances by the time the cursor walks past the visible window, and
// the selected row never goes negative.
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

	// Walk ↓ across every project; the offset must strictly advance past 0
	// somewhere along the way (the bottom projects don't fit at offset 0).
	cur := tea.Model(m)
	maxSeen := 0
	for i := 0; i < 11; i++ {
		cur, _ = cur.(Model).Update(keyMsg("down"))
		if off := cur.(Model).projectsScrollOffset; off > maxSeen {
			maxSeen = off
		}
	}
	if maxSeen == 0 {
		t.Errorf("walking ↓ through 12 projects must advance projectsScrollOffset past 0; stayed 0")
	}
	mDown := cur.(Model)

	// ↑ must reduce the offset back toward 0 and never go negative.
	cur2 := tea.Model(mDown)
	for i := 0; i < 11; i++ {
		cur2, _ = cur2.(Model).Update(keyMsg("up"))
		if off := cur2.(Model).projectsScrollOffset; off < 0 {
			t.Fatalf("projectsScrollOffset must never go negative; got %d", off)
		}
	}
	if cur2.(Model).projectsScrollOffset >= mDown.projectsScrollOffset {
		t.Errorf("walking ↑ must reduce projectsScrollOffset; down=%d up=%d",
			mDown.projectsScrollOffset, cur2.(Model).projectsScrollOffset)
	}
}

// TestArrowDown_KeepsSelectedRowVisible is the regression for the codex
// [P2] desync: as the cursor walks ↓ through an overflowing left column,
// the selected project must stay inside the rendered window at every step
// (the scroll offset is aligned to the cursor's line, not bumped a fixed
// amount). On the pre-fix code the offset advanced one line per press
// while the cursor advanced one multi-line block, so the selected row
// drifted off-screen and [⏎]/[a] targeted an invisible project.
func TestArrowDown_KeepsSelectedRowVisible(t *testing.T) {
	withFleetHome(t)
	m := New("test")
	m.width = 140
	m.height = 20
	m.dashboard = buildManyProjectsSnapshot(12)

	cur := tea.Model(m)
	for i := 0; i < 11; i++ {
		cur, _ = cur.(Model).Update(keyMsg("down"))
		mm := cur.(Model)
		sel := mm.selectedRow()
		if sel == nil || sel.project == nil {
			continue
		}
		want := projectDisplayName(sel.project.Name)
		frame := renderTwoColumnBody(mm, 90, 40)
		if !strings.Contains(frame, want) {
			t.Fatalf("after %d ↓ presses the selected project %q is off-screen (offset=%d):\n%s",
				i+1, want, mm.projectsScrollOffset, frame)
		}
	}
}

// TestJK_KeepsSelectedRowVisible is the regression for the codex [P2] on
// the vim-nav path: j/k call moveCursor directly (not scrollLeftPanel), so
// without an explicit alignment they leave the cursor on an off-screen
// project while the render stays at offset 0. Each j press must keep the
// selected project inside the rendered window.
func TestJK_KeepsSelectedRowVisible(t *testing.T) {
	withFleetHome(t)
	m := New("test")
	m.width = 140
	m.height = 20
	m.dashboard = buildManyProjectsSnapshot(12)

	cur := tea.Model(m)
	advanced := false
	for i := 0; i < 11; i++ {
		cur, _ = cur.(Model).Update(keyMsg("j"))
		mm := cur.(Model)
		if mm.projectsScrollOffset > 0 {
			advanced = true
		}
		sel := mm.selectedRow()
		if sel == nil || sel.project == nil {
			continue
		}
		want := projectDisplayName(sel.project.Name)
		frame := renderTwoColumnBody(mm, 90, 40)
		if !strings.Contains(frame, want) {
			t.Fatalf("after %d j presses the selected project %q is off-screen (offset=%d):\n%s",
				i+1, want, mm.projectsScrollOffset, frame)
		}
	}
	if !advanced {
		t.Errorf("walking j through 12 projects must advance projectsScrollOffset past 0")
	}
}

// TestArrowWrap_FromRightPaneAlignsLeftScroll is the regression for the
// codex [P2] on the arrow fallback path: after the left pane is scrolled
// to a nonzero offset, ↓ on the last right-column row falls through to
// moveCursor and WRAPS to the first project. Without alignment the left
// pane stays scrolled and the wrapped-to project is off-screen. The fix
// aligns the offset on the fallback move so the selected project shows.
func TestArrowWrap_FromRightPaneAlignsLeftScroll(t *testing.T) {
	withFleetHome(t)
	m := New("test")
	m.width = 140
	m.height = 20
	m.dashboard = buildManyProjectsSnapshot(12)
	m.records = []*agent.Record{sampleAgent("agent01")}
	// Pre-scroll the left pane so a wrap onto project 0 would be hidden.
	m.projectsScrollOffset = m.panelMaxOffset(rowProject)
	if m.projectsScrollOffset == 0 {
		t.Fatalf("expected a nonzero max offset to exercise the wrap")
	}

	// Park the cursor on the LAST dashboard row (the trailing right-column
	// row); ↓ from there wraps to index 0 (first project).
	rows := m.dashboardRows()
	m.dashCursor = len(rows) - 1

	updated, _ := m.Update(keyMsg("down"))
	m2 := updated.(Model)
	if m2.dashCursor != 0 {
		t.Fatalf("↓ on the last row must wrap to row 0; got cursor=%d", m2.dashCursor)
	}
	frame := renderTwoColumnBody(m2, 90, 40)
	want := projectDisplayName(m.dashboard.Projects[0].Name)
	if !strings.Contains(frame, want) {
		t.Errorf("after wrap to project 0 the left scroll must align so %q is visible (offset=%d):\n%s",
			want, m2.projectsScrollOffset, frame)
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
