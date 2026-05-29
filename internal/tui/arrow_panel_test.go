package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/edisonshen/fleet/internal/agent"
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

// TestArrowKeys_ScrollMovesCursorToo pins cursor co-movement (codex
// iter-2 [P2] follow-up): when ↓ scrolls the workers panel, the
// cursor must also advance to the next worker row so [⏎]/[a]/etc
// stay coherent with the visible row. Without this, repeated ↓
// keeps the cursor pinned to the first worker while content scrolls
// past, and actions target a row the operator can no longer see.
func TestArrowKeys_ScrollMovesCursorToo(t *testing.T) {
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
	startCursor := m.dashCursor
	if startCursor < 0 {
		t.Fatalf("jumpToRightPanel left cursor=%d", startCursor)
	}

	// ↓ on right-column should advance BOTH cursor and offset.
	updated, _ := m.Update(keyMsg("down"))
	mm := updated.(Model)
	if mm.dashCursor == startCursor {
		t.Errorf("↓ on workers panel did not advance dashCursor (was %d, still %d) — cursor will be stranded on scrolled content",
			startCursor, mm.dashCursor)
	}
	if mm.workersScrollOffset == 0 {
		t.Errorf("↓ on workers panel with overflow did not increment scroll offset; got 0")
	}
	// Cursor should still be on a worker row after the co-move.
	row := mm.selectedRow()
	if row == nil || row.kind != rowWorker {
		t.Errorf("after ↓ cursor landed on row kind=%v want rowWorker", func() interface{} {
			if row == nil {
				return "nil"
			}
			return row.kind
		}())
	}
}

// TestArrowKeys_ScrollCursorStaysInPanel pins the kind-anchored cursor
// (codex iter-3 [P2]): pressing ↑ on the FIRST worker row must NOT
// walk the cursor into a left-column row (project / task). Without
// this anchor, a single ↑ at the panel boundary silently moves to a
// different column and subsequent actions target the wrong row.
func TestArrowKeys_ScrollCursorStaysInPanel(t *testing.T) {
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
	// Walk ↑ ten times from the FIRST worker — cursor must NEVER
	// land on a non-worker row.
	for i := 0; i < 10; i++ {
		updated, _ := m.Update(keyMsg("up"))
		m = updated.(Model)
		row := m.selectedRow()
		if row == nil {
			t.Fatalf("iter %d: selectedRow returned nil", i)
		}
		if row.kind != rowWorker {
			t.Errorf("iter %d: ↑ left cursor on kind=%v want rowWorker — would silently change panels", i, row.kind)
		}
	}
}

// ----- tui-nav-handoff-regressions Fix 1: arrow boundary crossing -----
//
// Bug: ↓/↑ can't cross from the workers panel into the agents panel.
// The #177 scrollRightPanel kept returning true at the worker→agent
// boundary so handleKey never fell through to moveCursor, which is the
// helper that crosses panels. Fix: scrollRightPanel detects the
// no-progress case (cursor unchanged) at the panel boundary and returns
// false so the arrow falls through to moveCursor.
//
// rightPanelModel builds a NON-overflowing right column (a handful of
// workers + agents) with a tall terminal so trimWithScroll never hides
// rows — the boundary-crossing path is the one under test, not the
// overflow-scroll path.

// rightPanelModel seeds a dashboard with the given number of workers
// (project "fleet", phase review-pending) plus the given agent records,
// on a tall+wide terminal so neither right-column panel overflows.
func rightPanelModel(t *testing.T, workerCount int, agents ...*agent.Record) Model {
	t.Helper()
	m := New("test")
	m.width = 160
	m.height = 60 // tall enough that a small right column never overflows
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{{Name: "fleet", RepoSlug: "fleet", Active: true}},
		LoadedAt: time.Now(),
	}
	for i := 0; i < workerCount; i++ {
		m.dashboard.Workers = append(m.dashboard.Workers, &WorkerRow{
			Project: "fleet",
			Slug:    "synth-" + string(rune('a'+(i%26))),
			Phase:   "review-pending",
		})
	}
	m.records = agents
	return m
}

// lastRowIndexOfKind returns the index of the LAST dashboardRows() row
// of kind k, or -1 if none.
func lastRowIndexOfKind(m Model, k rowKind) int {
	idx := -1
	for i, r := range m.dashboardRows() {
		if r.kind == k {
			idx = i
		}
	}
	return idx
}

// firstRowIndexOfKind returns the index of the FIRST dashboardRows()
// row of kind k, or -1 if none.
func firstRowIndexOfKind(m Model, k rowKind) int {
	for i, r := range m.dashboardRows() {
		if r.kind == k {
			return i
		}
	}
	return -1
}

// TestArrowDown_LastWorker_CrossesToFirstAgent: with non-overflowing
// panels, cursor on the last worker row, ↓ crosses into the first agent
// row instead of dead-ending in the workers panel.
func TestArrowDown_LastWorker_CrossesToFirstAgent(t *testing.T) {
	m := rightPanelModel(t, 2, sampleAgent("agent01"), sampleAgent("agent02"))
	last := lastRowIndexOfKind(m, rowWorker)
	if last < 0 {
		t.Fatalf("no worker rows in fixture; rows=%d", len(m.dashboardRows()))
	}
	m.dashCursor = last

	updated, _ := m.Update(keyMsg("down"))
	mm := updated.(Model)
	row := mm.selectedRow()
	if row == nil || row.kind != rowAgent {
		t.Fatalf("↓ on last worker did not cross to an agent row; landed on %v (dashCursor=%d)",
			rowKindOf(row), mm.dashCursor)
	}
	// Should be the FIRST agent (immediately after the last worker).
	if firstAgent := firstRowIndexOfKind(mm, rowAgent); mm.dashCursor != firstAgent {
		t.Errorf("↓ crossed to dashCursor=%d; want first agent row at %d", mm.dashCursor, firstAgent)
	}
}

// TestArrowUp_FirstAgent_CrossesToLastWorker is the symmetric case:
// cursor on the first agent row, ↑ crosses back into the last worker
// row instead of dead-ending in the agents panel.
func TestArrowUp_FirstAgent_CrossesToLastWorker(t *testing.T) {
	m := rightPanelModel(t, 2, sampleAgent("agent01"), sampleAgent("agent02"))
	firstAgent := firstRowIndexOfKind(m, rowAgent)
	if firstAgent < 0 {
		t.Fatalf("no agent rows in fixture; rows=%d", len(m.dashboardRows()))
	}
	m.dashCursor = firstAgent

	updated, _ := m.Update(keyMsg("up"))
	mm := updated.(Model)
	row := mm.selectedRow()
	if row == nil || row.kind != rowWorker {
		t.Fatalf("↑ on first agent did not cross to a worker row; landed on %v (dashCursor=%d)",
			rowKindOf(row), mm.dashCursor)
	}
	if lastWorker := lastRowIndexOfKind(mm, rowWorker); mm.dashCursor != lastWorker {
		t.Errorf("↑ crossed to dashCursor=%d; want last worker row at %d", mm.dashCursor, lastWorker)
	}
}

// TestArrowDown_OverflowWorkerPanel_ScrollsStaysInPanel regresses the
// #177 behavior: when the worker panel overflows and the cursor is
// mid-panel, ↓ scrolls the panel AND advances the cursor but does NOT
// cross into the agents panel. The boundary fall-through must only fire
// when there is genuinely no next worker row.
func TestArrowDown_OverflowWorkerPanel_ScrollsStaysInPanel(t *testing.T) {
	m := New("test")
	m.width = 140
	m.height = 24 // short → 30 workers overflow
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
	m.records = append(m.records, sampleAgent("agent01"))
	m.jumpToRightPanel() // first worker
	startOffset := m.workersScrollOffset

	updated, _ := m.Update(keyMsg("down"))
	mm := updated.(Model)
	row := mm.selectedRow()
	if row == nil || row.kind != rowWorker {
		t.Fatalf("↓ mid-overflow-panel left cursor on %v; want rowWorker (must NOT cross)",
			rowKindOf(row))
	}
	if mm.workersScrollOffset <= startOffset {
		t.Errorf("↓ on overflowing worker panel did not advance scroll offset (was %d, now %d)",
			startOffset, mm.workersScrollOffset)
	}
}

// TestArrowDown_LastAgent_WrapsToTop: cursor on the last agent row, ↓
// falls through to moveCursor which wraps to the top of the unified row
// list (existing wrap behavior, now reachable because the boundary
// fall-through fires).
func TestArrowDown_LastAgent_WrapsToTop(t *testing.T) {
	m := rightPanelModel(t, 1, sampleAgent("agent01"), sampleAgent("agent02"))
	last := lastRowIndexOfKind(m, rowAgent)
	if last < 0 {
		t.Fatalf("no agent rows in fixture")
	}
	m.dashCursor = last

	updated, _ := m.Update(keyMsg("down"))
	mm := updated.(Model)
	if mm.dashCursor != 0 {
		t.Errorf("↓ on last agent did not wrap to top; dashCursor=%d want 0", mm.dashCursor)
	}
}

// TestArrowNav_FullTopToBottom_TraversesAllRightRows: from the first
// worker, repeated ↓ visits every worker then every agent without
// getting stuck at the worker→agent boundary.
func TestArrowNav_FullTopToBottom_TraversesAllRightRows(t *testing.T) {
	m := rightPanelModel(t, 3, sampleAgent("agent01"), sampleAgent("agent02"))
	firstWorker := firstRowIndexOfKind(m, rowWorker)
	if firstWorker < 0 {
		t.Fatalf("no worker rows in fixture")
	}
	m.dashCursor = firstWorker

	// Collect the kinds visited over enough ↓ presses to reach the last
	// agent. Workers (3) + agents (2) = 5 right-column rows; 4 presses
	// walk from first worker to last agent.
	wantWorkers := 0
	for _, r := range m.dashboardRows() {
		if r.kind == rowWorker {
			wantWorkers++
		}
	}
	sawAgent := false
	for i := 0; i < wantWorkers+1; i++ {
		row := m.selectedRow()
		if row != nil && row.kind == rowAgent {
			sawAgent = true
			break
		}
		updated, _ := m.Update(keyMsg("down"))
		m = updated.(Model)
	}
	if !sawAgent {
		t.Errorf("walking ↓ from the first worker never reached an agent row — stuck at the boundary")
	}
}

// Test_jk_UnaffectedByBoundaryFix regresses the j/k contract: j/k call
// moveCursor directly and ALREADY cross panels freely; the Fix 1 change
// to scrollRightPanel must not touch that path. j on the last worker
// lands on the first agent (same as the fixed ↓), and j never scrolls.
func Test_jk_UnaffectedByBoundaryFix(t *testing.T) {
	m := rightPanelModel(t, 2, sampleAgent("agent01"), sampleAgent("agent02"))
	last := lastRowIndexOfKind(m, rowWorker)
	m.dashCursor = last
	startOffset := m.workersScrollOffset

	updated, _ := m.Update(keyMsg("j"))
	mm := updated.(Model)
	row := mm.selectedRow()
	if row == nil || row.kind != rowAgent {
		t.Fatalf("j on last worker did not cross to agent; landed on %v", rowKindOf(row))
	}
	if mm.workersScrollOffset != startOffset {
		t.Errorf("j touched workersScrollOffset (%d → %d); j must NEVER scroll a panel",
			startOffset, mm.workersScrollOffset)
	}
	// k from the first agent crosses back to the last worker.
	updated, _ = mm.Update(keyMsg("k"))
	mm2 := updated.(Model)
	row = mm2.selectedRow()
	if row == nil || row.kind != rowWorker {
		t.Errorf("k on first agent did not cross back to worker; landed on %v", rowKindOf(row))
	}
}

// TestArrowDown_CrossToAgent_ResetsAgentsScrollOffset pins codex review
// iter-2 [P2]: trimWithScroll does NOT auto-follow the cursor, so a
// worker→agent cross must reset the DESTINATION (agents) panel offset to
// keep the just-selected first-agent row visible. Setup: a previously
// scrolled-away agents panel (agentsScrollOffset > 0), cursor on the
// last worker, ↓ crosses to the first agent. Without the reset the
// agents panel would still render from the old offset and the selected
// agent would be off-screen — actions then target an unseen row.
func TestArrowDown_CrossToAgent_ResetsAgentsScrollOffset(t *testing.T) {
	m := rightPanelModel(t, 2,
		sampleAgent("agent01"), sampleAgent("agent02"), sampleAgent("agent03"))
	last := lastRowIndexOfKind(m, rowWorker)
	if last < 0 {
		t.Fatalf("no worker rows in fixture")
	}
	m.dashCursor = last
	// Simulate the agents panel having been scrolled away earlier (e.g.
	// operator scrolled agents down, then k'd back up into the workers).
	m.agentsScrollOffset = 2

	updated, _ := m.Update(keyMsg("down"))
	mm := updated.(Model)
	row := mm.selectedRow()
	if row == nil || row.kind != rowAgent {
		t.Fatalf("↓ on last worker did not cross to an agent row; landed on %v", rowKindOf(row))
	}
	if mm.agentsScrollOffset != 0 {
		t.Errorf("crossing into the agents panel left agentsScrollOffset=%d; want 0 "+
			"(selected first-agent row would be hidden)", mm.agentsScrollOffset)
	}
}

// TestArrowUp_CrossToWorker_ResetsWorkersScrollOffsetToBottom is the
// symmetric guard: ↑ from the first agent crosses to the LAST worker, so
// the workers panel offset must move to the bottom (not stay at a stale
// top-scrolled value) to keep the last-worker row visible.
func TestArrowUp_CrossToWorker_ResetsWorkersScrollOffsetToBottom(t *testing.T) {
	m := rightPanelModel(t, 3,
		sampleAgent("agent01"), sampleAgent("agent02"))
	firstAgent := firstRowIndexOfKind(m, rowAgent)
	if firstAgent < 0 {
		t.Fatalf("no agent rows in fixture")
	}
	m.dashCursor = firstAgent
	// Workers panel was scrolled to the top earlier; crossing up should
	// move it to the bottom so the last worker is visible.
	m.workersScrollOffset = 0

	updated, _ := m.Update(keyMsg("up"))
	mm := updated.(Model)
	row := mm.selectedRow()
	if row == nil || row.kind != rowWorker {
		t.Fatalf("↑ on first agent did not cross to a worker; landed on %v", rowKindOf(row))
	}
	// The reset targets the count-based panel max; trimWithScroll
	// re-clamps to the render-visible bottom. The contract under test is
	// "non-zero / bottom-aligned", which equals panelMaxOffset(rowWorker).
	if mm.workersScrollOffset != mm.panelMaxOffset(rowWorker) {
		t.Errorf("crossing up into the workers panel left workersScrollOffset=%d; want %d "+
			"(bottom-aligned so the last-worker row is visible)",
			mm.workersScrollOffset, mm.panelMaxOffset(rowWorker))
	}
}

// rowKindOf is a nil-safe helper for test error messages.
func rowKindOf(r *dashRow) interface{} {
	if r == nil {
		return "nil"
	}
	return r.kind
}

// TestArrowKeys_ScrollOffsetClampsAtMax pins the model-side upper
// clamp (codex iter-3 [P2]): pressing ↓ many times past the visible
// end must NOT leave workersScrollOffset growing unbounded. Without
// the upper clamp, the next ↑ press just decrements the oversized
// value and the panel appears stuck at the bottom for many keypresses.
func TestArrowKeys_ScrollOffsetClampsAtMax(t *testing.T) {
	m := New("test")
	m.width = 140
	m.height = 24
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{{Name: "fleet", RepoSlug: "fleet", Active: true}},
		LoadedAt: time.Now(),
	}
	// 5 workers — small enough that the upper bound is tight (15 lines
	// at 3 lines/row).
	for i := 0; i < 5; i++ {
		m.dashboard.Workers = append(m.dashboard.Workers, &WorkerRow{
			Project: "fleet",
			Slug:    "synth-" + string(rune('a'+(i%26))) + string(rune('a'+((i/26)%26))),
			Phase:   "review-pending",
		})
	}
	m.jumpToRightPanel()
	// Press ↓ 100 times — way past any reasonable line count.
	for i := 0; i < 100; i++ {
		updated, _ := m.Update(keyMsg("down"))
		m = updated.(Model)
	}
	// Bound: 5 workers * 3 lines/row = 15. Stored offset must not
	// exceed that.
	const wantBound = 5 * 3
	if m.workersScrollOffset > wantBound {
		t.Errorf("100 × ↓ left workersScrollOffset=%d, want <= %d (upper clamp not enforced — panel will appear stuck on subsequent ↑)",
			m.workersScrollOffset, wantBound)
	}
}
