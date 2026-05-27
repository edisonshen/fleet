package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// Issue #83: Left/Right arrow keys jump cursor between PROJECTS and
// WORKERS panels. j/k unchanged. Tests pin the dashboardRows() row
// ordering contract — LEFT (projects + tasks) before RIGHT
// (workers + agents) — so cursor jumps land at the visual anchors of
// each panel without a "focused panel" model field.

// TestKeyLeftArrow_JumpsToFirstProjectRow pins issue #83: pressing ←
// from anywhere in the unified row list snaps the cursor to the first
// PROJECTS-column row (the LEFT panel anchor). j/k semantics are
// unchanged elsewhere; this is purely an additive shortcut.
func TestKeyLeftArrow_JumpsToFirstProjectRow(t *testing.T) {
	m := New("test")
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{
			{Name: "demo", RepoSlug: "demo"},
		},
		Workers: []*WorkerRow{
			{Project: "demo", Slug: "feat-1a2b"},
		},
		LoadedAt: time.Now(),
	}
	m.records = append(m.records, sampleAgent("agent01"))

	// Place cursor on the agent row (last in the unified list).
	rows := m.dashboardRows()
	last := len(rows) - 1
	if last < 2 {
		t.Fatalf("expected at least 3 rows for this fixture, got %d", len(rows))
	}
	m.dashCursor = last

	updated, _ := m.Update(keyMsg("left"))
	mm := updated.(Model)
	got := mm.dashboardRows()[mm.dashCursor]
	if got.kind != rowProject {
		t.Fatalf("after ←: cursor on kind=%v, want rowProject", got.kind)
	}
	if got.project == nil || got.project.Name != "demo" {
		t.Errorf("after ←: cursor on project=%+v, want first project (demo)",
			got.project)
	}
}

// TestKeyRightArrow_JumpsToFirstWorkerOrAgentRow pins issue #83:
// pressing → from the LEFT panel snaps the cursor to the first
// WORKERS-row (top of the right panel). When workers are present, the
// jump prefers them over the v0.1 agents sub-block — that matches the
// visual top of the right column.
func TestKeyRightArrow_JumpsToFirstWorkerOrAgentRow(t *testing.T) {
	m := New("test")
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{{Name: "demo", RepoSlug: "demo"}},
		Workers: []*WorkerRow{
			{Project: "demo", Slug: "feat-1a2b"},
			{Project: "demo", Slug: "feat-3c4d"},
		},
		LoadedAt: time.Now(),
	}
	m.records = append(m.records, sampleAgent("agent01"))

	// Cursor on the project row (top of LEFT panel).
	m.dashCursor = 0
	if got := m.dashboardRows()[m.dashCursor].kind; got != rowProject {
		t.Fatalf("setup: cursor at 0 should be project, got %v", got)
	}

	updated, _ := m.Update(keyMsg("right"))
	mm := updated.(Model)
	got := mm.dashboardRows()[mm.dashCursor]
	if got.kind != rowWorker {
		t.Fatalf("after →: cursor on kind=%v, want rowWorker", got.kind)
	}
	if got.worker == nil || got.worker.Slug != "feat-1a2b" {
		t.Errorf("after →: cursor on worker=%+v, want first worker (feat-1a2b)",
			got.worker)
	}
}

// TestKeyRightArrow_FallsBackToFirstAgentWhenNoWorkers covers the
// no-workers branch of jumpToRightPanel: when the dashboard has no
// workers but does have v0.1 agents, → snaps to the first agent row
// because that's still the visual top of the right panel.
func TestKeyRightArrow_FallsBackToFirstAgentWhenNoWorkers(t *testing.T) {
	m := New("test")
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{{Name: "demo", RepoSlug: "demo"}},
		LoadedAt: time.Now(),
	}
	// Two agents in the right panel; no workers.
	m.records = append(m.records, sampleAgent("agent01"), sampleAgent("agent02"))

	m.dashCursor = 0 // project row
	updated, _ := m.Update(keyMsg("right"))
	mm := updated.(Model)
	got := mm.dashboardRows()[mm.dashCursor]
	if got.kind != rowAgent {
		t.Fatalf("after → (no workers): cursor on kind=%v, want rowAgent", got.kind)
	}
}

// TestKeyRightArrow_RightPanelEmpty_FlashesAndDoesNotMove pins the
// "right panel is empty" branch (issue #83 fix shape: brief inline
// flash, no cursor movement). When neither workers nor agents exist,
// pressing → must surface a flash explaining the no-op rather than
// silently doing nothing.
func TestKeyRightArrow_RightPanelEmpty_FlashesAndDoesNotMove(t *testing.T) {
	m := New("test")
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{
			{Name: "demo", RepoSlug: "demo"},
			{Name: "other", RepoSlug: "other"},
		},
		LoadedAt: time.Now(),
	}
	// No workers, no agents.
	m.dashCursor = 1 // second project row

	updated, _ := m.Update(keyMsg("right"))
	mm := updated.(Model)
	if mm.dashCursor != 1 {
		t.Errorf("→ on empty right panel moved cursor to %d; want unchanged at 1",
			mm.dashCursor)
	}
	if mm.flash == nil {
		t.Fatal("→ on empty right panel did not set a flash")
	}
	if mm.flash.isErr {
		t.Errorf("flash isErr=true; want non-error (navigation no-op, not failure)")
	}
	if mm.flash.text != "right panel is empty" {
		t.Errorf("flash text=%q; want %q", mm.flash.text, "right panel is empty")
	}
}

// TestKeyJK_UnchangedByArrowPanelChange regresses the j/k contract:
// j/k semantics must NOT change when the panel-switch arrows land.
// Pressing j from the first project row still moves to the next row in
// the unified list (not to the right panel).
func TestKeyJK_UnchangedByArrowPanelChange(t *testing.T) {
	m := New("test")
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{
			{Name: "demo", RepoSlug: "demo"},
			{Name: "other", RepoSlug: "other"},
		},
		Workers:  []*WorkerRow{{Project: "demo", Slug: "feat-1a2b"}},
		LoadedAt: time.Now(),
	}
	m.records = append(m.records, sampleAgent("agent01"))

	m.dashCursor = 0 // first project
	updated, _ := m.Update(keyMsg("j"))
	mm := updated.(Model)
	if mm.dashCursor != 1 {
		t.Errorf("j after ← introduction: dashCursor=%d, want 1 (j unchanged)",
			mm.dashCursor)
	}
	// k from row 1 should bring us back to row 0.
	updated, _ = mm.Update(keyMsg("k"))
	mm = updated.(Model)
	if mm.dashCursor != 0 {
		t.Errorf("k: dashCursor=%d, want 0 (k unchanged)", mm.dashCursor)
	}
}

// TestKeyLeftArrow_NoProjects_NoOp covers the defensive branch when
// dashboardRows() yields no project rows (synthetic-only or empty
// dashboard): ← leaves the cursor where it is rather than hopping to a
// non-project row.
func TestKeyLeftArrow_NoProjects_NoOp(t *testing.T) {
	m := New("test")
	// Bare model: no dashboard, no records → dashboardRows() empty.
	m.dashCursor = 0
	updated, _ := m.Update(keyMsg("left"))
	mm := updated.(Model)
	if mm.dashCursor != 0 {
		t.Errorf("← on empty dashboard moved cursor to %d; want 0 (no-op)", mm.dashCursor)
	}
	if mm.flash != nil {
		t.Errorf("← on empty dashboard set a flash %q; want nil (silent no-op)", mm.flash.text)
	}
}

// TestRenderHelpOverlay_DocsArrowPanelEntry pins the [?] help overlay
// help line for the panel-switch arrows. Issue #90: the footer no
// longer advertises ←/→ — the help overlay is the canonical surface,
// so this test guards the discoverability path.
func TestRenderHelpOverlay_DocsArrowPanelEntry(t *testing.T) {
	out := renderHelpOverlay(120)
	if !strings.Contains(out, "←") || !strings.Contains(out, "→") {
		t.Errorf("help overlay missing arrow keys:\n%s", out)
	}
	if !strings.Contains(out, "PROJECTS") || !strings.Contains(out, "WORKERS") {
		t.Errorf("help overlay missing panel labels (PROJECTS / WORKERS):\n%s", out)
	}
}

// ----- fleet#177 right-panel scroll tests (Fix 2) -----

// TestArrowKeys_ScrollWorkersPanel_DownIncrementsOffset: when the cursor
// is on a worker row (right column focused) AND the right column has
// overflow, ↓ increments the workers scroll offset. Boundary: capped
// at maxScrollOffset so ↓ past the end doesn't underflow the slice.
func TestArrowKeys_ScrollWorkersPanel_DownIncrementsOffset(t *testing.T) {
	m := New("test")
	m.width = 140
	m.height = 24
	// 30 workers to force overflow.
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{{Name: "fleet", RepoSlug: "fleet", Active: true}},
		LoadedAt: time.Now(),
	}
	for i := 0; i < 30; i++ {
		m.dashboard.Workers = append(m.dashboard.Workers, &WorkerRow{
			Project: "fleet",
			Slug:    "synth-" + string(rune('a'+(i%26))) + string(rune('a'+((i/26)%26))),
			Phase:   "review-pending",
		})
	}
	// Jump cursor to first worker row.
	m.jumpToRightPanel()
	if m.workersScrollOffset != 0 {
		t.Fatalf("setup: workersScrollOffset=%d, want 0", m.workersScrollOffset)
	}
	// ↓ on right-column focus → scroll, not row movement.
	updated, _ := m.Update(keyMsg("down"))
	mm := updated.(Model)
	if mm.workersScrollOffset == 0 {
		t.Errorf("after ↓ on workers panel with overflow: workersScrollOffset=%d, want > 0", mm.workersScrollOffset)
	}
}

// TestArrowKeys_ScrollWorkersPanel_UpClampsAtZero: ↑ on a panel whose
// offset is already zero must clamp (not negative).
func TestArrowKeys_ScrollWorkersPanel_UpClampsAtZero(t *testing.T) {
	m := New("test")
	m.width = 140
	m.height = 24
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{{Name: "fleet", RepoSlug: "fleet", Active: true}},
		LoadedAt: time.Now(),
	}
	for i := 0; i < 30; i++ {
		m.dashboard.Workers = append(m.dashboard.Workers, &WorkerRow{
			Project: "fleet",
			Slug:    "synth-" + string(rune('a'+(i%26))) + string(rune('a'+((i/26)%26))),
			Phase:   "review-pending",
		})
	}
	m.jumpToRightPanel()
	m.workersScrollOffset = 0
	updated, _ := m.Update(keyMsg("up"))
	mm := updated.(Model)
	if mm.workersScrollOffset < 0 {
		t.Errorf("after ↑ on workers panel offset=0: workersScrollOffset=%d, want clamped to 0", mm.workersScrollOffset)
	}
}

// TestArrowKeys_LeftColumnUnaffectedByRightScroll: when the cursor is
// on a LEFT-column row (project / task), ↓ moves the cursor normally
// (j/k semantics) and does NOT touch workersScrollOffset.
func TestArrowKeys_LeftColumnUnaffectedByRightScroll(t *testing.T) {
	m := New("test")
	m.width = 140
	m.height = 24
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{
			{Name: "fleet", RepoSlug: "fleet", Active: true},
			{Name: "demo", RepoSlug: "demo", Active: true},
		},
		LoadedAt: time.Now(),
	}
	for i := 0; i < 30; i++ {
		m.dashboard.Workers = append(m.dashboard.Workers, &WorkerRow{
			Project: "fleet",
			Slug:    "synth-" + string(rune('a'+(i%26))) + string(rune('a'+((i/26)%26))),
			Phase:   "review-pending",
		})
	}
	// Place cursor on left column (project row).
	m.jumpToLeftPanel()
	startCursor := m.dashCursor
	startOffset := m.workersScrollOffset

	// ↓ on left-column focus → cursor moves, no scroll change.
	updated, _ := m.Update(keyMsg("down"))
	mm := updated.(Model)
	if mm.dashCursor == startCursor {
		t.Errorf("↓ on left column did not move cursor; dashCursor=%d (want > %d)", mm.dashCursor, startCursor)
	}
	if mm.workersScrollOffset != startOffset {
		t.Errorf("↓ on left column touched right-panel scroll offset: workersScrollOffset=%d, want %d (unchanged)",
			mm.workersScrollOffset, startOffset)
	}
}

// TestRightPanelScrollOffsetResetsOnResize: a terminal resize event
// must reset the right-panel offsets to 0 (the visible window changes,
// and clamping after-the-fact is brittle).
func TestRightPanelScrollOffsetResetsOnResize(t *testing.T) {
	m := New("test")
	m.workersScrollOffset = 5
	m.agentsScrollOffset = 3

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 140, Height: 24})
	mm := updated.(Model)
	if mm.workersScrollOffset != 0 || mm.agentsScrollOffset != 0 {
		t.Errorf("resize did not reset right-panel offsets: workers=%d, agents=%d (want 0,0)",
			mm.workersScrollOffset, mm.agentsScrollOffset)
	}
}
