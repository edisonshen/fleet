// Tests for the context-pct chip + handoff tag (issues #89, #95).
package tui

import (
	"strings"
	"testing"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/handoff"
)

// floatPtr is a small helper — the *float64 pattern is everywhere
// in the agent package so the tests need a one-liner constructor.
func floatPtr(v float64) *float64 { return &v }

// strPtr matches floatPtr for the *string handoff_type cases.
func strPtr(s string) *string { return &s }

// -- Phase 2: percent renderer ----------------------------------------
//
// Issue #95 dropped the 5-segment bar glyph. The renderer now emits
// a colored "<int>%" only — tests assert on the integer label and the
// threshold zone, not on segment counts (the bar is gone).

func TestContextBar_NilOmitsChip(t *testing.T) {
	got := renderContextBar(nil)
	if got != "" {
		t.Fatalf("nil pct should render empty string, got %q", got)
	}
}

func TestContextBar_NoBarGlyphsRendered(t *testing.T) {
	// Issue #95: the bar (▰/▱) was dropped. Make sure no rendered
	// chip ever carries either glyph again — guards against a
	// regression that re-introduces the segment chunks.
	for _, v := range []float64{0, 12, 48, 50, 69, 70, 95, 100} {
		got := renderContextBar(floatPtr(v))
		if strings.ContainsAny(got, "▰▱") { // ▰ ▱
			t.Errorf("%v%%: chip should not contain bar glyphs, got %q", v, got)
		}
	}
}

func TestContextBar_Empty(t *testing.T) {
	// 0% — green zone, "0%" label.
	got := renderContextBar(floatPtr(0))
	if got == "" {
		t.Fatal("0%% should still render a chip (0%% label)")
	}
	if !strings.Contains(got, "0%") {
		t.Errorf("0%% chip should contain literal \"0%%\", got %q", got)
	}
	// Zone gating is asserted via contextZone() — lipgloss strips
	// ANSI in non-TTY test runs so we can't substring the palette.
	if z := contextZone(0); z != "green" {
		t.Errorf("0%% zone want green; got %q", z)
	}
}

func TestContextBar_Green_Low(t *testing.T) {
	got := renderContextBar(floatPtr(12))
	if !strings.Contains(got, "12%") {
		t.Errorf("12%% chip should contain \"12%%\", got %q", got)
	}
	if z := contextZone(12); z != "green" {
		t.Errorf("12%% zone want green; got %q", z)
	}
}

func TestContextBar_Green_High(t *testing.T) {
	// 48% — under yellow threshold, still green zone.
	got := renderContextBar(floatPtr(48))
	if !strings.Contains(got, "48%") {
		t.Errorf("48%% chip missing label: %q", got)
	}
	if z := contextZone(48); z != "green" {
		t.Errorf("48%% zone want green (still under 50); got %q", z)
	}
}

func TestContextBar_AmberThreshold(t *testing.T) {
	// 50% — exact yellow threshold; zone flips to amber.
	if z := contextZone(50); z != "amber" {
		t.Errorf("50%% zone want amber; got %q", z)
	}
	got := renderContextBar(floatPtr(50))
	if !strings.Contains(got, "50%") {
		t.Errorf("50%% chip missing label: %q", got)
	}
}

func TestContextBar_Amber_High(t *testing.T) {
	// 69% — last point still in amber zone before red.
	if z := contextZone(69); z != "amber" {
		t.Errorf("69%% zone want amber; got %q", z)
	}
	got := renderContextBar(floatPtr(69))
	if !strings.Contains(got, "69%") {
		t.Errorf("69%% chip missing label: %q", got)
	}
}

func TestContextBar_RedThreshold(t *testing.T) {
	// 70% — exact red threshold. Zone flips to red.
	if z := contextZone(70); z != "red" {
		t.Errorf("70%% zone want red; got %q", z)
	}
	got := renderContextBar(floatPtr(70))
	if !strings.Contains(got, "70%") {
		t.Errorf("70%% chip missing label: %q", got)
	}
}

func TestContextBar_Red_High(t *testing.T) {
	got := renderContextBar(floatPtr(95))
	if !strings.Contains(got, "95%") {
		t.Errorf("95%% chip missing label: %q", got)
	}
	if z := contextZone(95); z != "red" {
		t.Errorf("95%% zone want red; got %q", z)
	}
}

func TestContextBar_Full(t *testing.T) {
	// 100% — red zone, label is "100%" (3 digits).
	got := renderContextBar(floatPtr(100))
	if !strings.Contains(got, "100%") {
		t.Errorf("100%% chip missing label: %q", got)
	}
	if z := contextZone(100); z != "red" {
		t.Errorf("100%% zone want red; got %q", z)
	}
}

func TestContextBar_OutOfRangeClamps(t *testing.T) {
	// Defensive: hand-edited or future-schema records can deliver junk.
	// Renderer should clamp into 0..100 instead of emitting "-50%" /
	// "999%". Negative + NaN clamp to 0%; >100 clamps to 100%.
	for _, c := range []struct {
		in   float64
		want string
	}{
		{-1, "0%"},
		{-50, "0%"},
		{150, "100%"},
		{999, "100%"},
	} {
		got := renderContextBar(floatPtr(c.in))
		if !strings.Contains(got, c.want) {
			t.Errorf("clamp %v: want chip containing %q, got %q", c.in, c.want, got)
		}
	}
}

// -- Phase 6: handoff tag ---------------------------------------------

func TestHandoffTag_NilEmpty(t *testing.T) {
	if got := renderHandoffTag(nil); got != "" {
		t.Errorf("nil handoff_type → empty; got %q", got)
	}
	if got := renderHandoffTag(strPtr("")); got != "" {
		t.Errorf("empty string handoff_type → empty; got %q", got)
	}
}

func TestHandoffTag_AutoYellow(t *testing.T) {
	label, zone := handoffTagSpec(strPtr(handoff.TypeAutoYellow))
	if label != "◐ HANDOFF" {
		t.Errorf("auto-yellow label want \"◐ HANDOFF\"; got %q", label)
	}
	if zone != "amber" {
		t.Errorf("auto-yellow zone want amber; got %q", zone)
	}
	got := renderHandoffTag(strPtr(handoff.TypeAutoYellow))
	if !strings.Contains(got, "HANDOFF") {
		t.Errorf("auto-yellow render should contain HANDOFF, got %q", got)
	}
}

func TestHandoffTag_AutoRed(t *testing.T) {
	label, zone := handoffTagSpec(strPtr(handoff.TypeAutoRed))
	if label != "◐ HANDOFF" {
		t.Errorf("auto-red label want \"◐ HANDOFF\"; got %q", label)
	}
	if zone != "red" {
		t.Errorf("auto-red zone want red; got %q", zone)
	}
}

func TestHandoffTag_Manual(t *testing.T) {
	label, zone := handoffTagSpec(strPtr(handoff.TypeManual))
	if label != "◐ HANDOFF" {
		t.Errorf("manual label want \"◐ HANDOFF\"; got %q", label)
	}
	if zone != "dim" {
		t.Errorf("manual zone want dim; got %q", zone)
	}
}

func TestHandoffTag_PreCompact(t *testing.T) {
	label, zone := handoffTagSpec(strPtr(handoff.TypePreCompact))
	if label != "◐ COMPACT" {
		t.Errorf("precompact label want \"◐ COMPACT\"; got %q", label)
	}
	if zone != "dim" {
		t.Errorf("precompact zone want dim; got %q", zone)
	}
}

func TestHandoffTag_UnknownEmpty(t *testing.T) {
	// Future schema drift / hand-edit shouldn't render a mislabeled chip.
	got := renderHandoffTag(strPtr("future-future-mode"))
	if got != "" {
		t.Errorf("unknown handoff_type → empty; got %q", got)
	}
}

// -- Phase 5: hot counts ------------------------------------------------

func TestHotCounts_AllGreenZero(t *testing.T) {
	r1 := agent.New("aaa11111")
	r1.ContextPct = floatPtr(10)
	r2 := agent.New("bbb22222")
	r2.ContextPct = floatPtr(48)
	y, r := hotCounts([]*agent.Record{r1, r2}, map[string]bool{r1.ID: true, r2.ID: true}, nil)
	if y != 0 || r != 0 {
		t.Errorf("all green: want (0,0); got (%d,%d)", y, r)
	}
}

func TestHotCounts_YellowCounted(t *testing.T) {
	r1 := agent.New("aaa11111")
	r1.ContextPct = floatPtr(55)
	r2 := agent.New("bbb22222")
	r2.ContextPct = floatPtr(65)
	r3 := agent.New("ccc33333")
	r3.ContextPct = floatPtr(20)
	y, r := hotCounts(
		[]*agent.Record{r1, r2, r3},
		map[string]bool{r1.ID: true, r2.ID: true, r3.ID: true},
		nil,
	)
	if y != 2 || r != 0 {
		t.Errorf("two yellow: want (2,0); got (%d,%d)", y, r)
	}
}

func TestHotCounts_RedCounted(t *testing.T) {
	r1 := agent.New("aaa11111")
	r1.ContextPct = floatPtr(72)
	r2 := agent.New("bbb22222")
	r2.ContextPct = floatPtr(95)
	y, r := hotCounts(
		[]*agent.Record{r1, r2},
		map[string]bool{r1.ID: true, r2.ID: true},
		nil,
	)
	if y != 0 || r != 2 {
		t.Errorf("two red: want (0,2); got (%d,%d)", y, r)
	}
}

func TestHotCounts_DeadAgentSkipped(t *testing.T) {
	// Dead records shouldn't be counted — the operator can't act on
	// them. deriveStatus returns "dead" only when the record has a
	// TmuxSession AND alive[id] == false; reproduce both conditions.
	r := agent.New("deadbeef")
	r.TmuxSession = "fleet-deadbeef"
	r.ContextPct = floatPtr(80)
	y, red := hotCounts([]*agent.Record{r}, map[string]bool{r.ID: false}, nil)
	if y != 0 || red != 0 {
		t.Errorf("dead agent should not count; got (%d,%d)", y, red)
	}
}

func TestHotCounts_NilContextSkipped(t *testing.T) {
	r := agent.New("aaa11111")
	r.ContextPct = nil
	y, red := hotCounts([]*agent.Record{r}, map[string]bool{r.ID: true}, nil)
	if y != 0 || red != 0 {
		t.Errorf("nil context_pct should not count; got (%d,%d)", y, red)
	}
}

func TestHotCounts_WorkerIncluded(t *testing.T) {
	// Worker → coord lookup. Coord has 75% context_pct (red zone). The
	// worker row should NOT add a second red — both share the same
	// underlying record (PR #87 architecture).
	coord := agent.New("c00fc00f")
	coord.Project = "demo"
	coord.TaskID = "coord-demo"
	coord.ContextPct = floatPtr(75)
	worker := &WorkerRow{Project: "demo", Slug: "feat-1a2b"}
	y, red := hotCounts(
		[]*agent.Record{coord},
		map[string]bool{coord.ID: true},
		[]*WorkerRow{worker},
	)
	if y != 0 || red != 1 {
		t.Errorf("coord+worker share record: want (0,1); got (%d,%d)", y, red)
	}
}

// -- renderHotCounts shape ---------------------------------------------

func TestRenderHotCounts_HiddenWhenZero(t *testing.T) {
	if got := renderHotCounts(0, 0); got != "" {
		t.Errorf("(0,0) → empty; got %q", got)
	}
}

func TestRenderHotCounts_YellowOnly(t *testing.T) {
	got := renderHotCounts(3, 0)
	if !strings.Contains(got, "3 yellow") {
		t.Errorf("3 yellow expected in output, got %q", got)
	}
	if strings.Contains(got, "red") {
		t.Errorf("no red expected, got %q", got)
	}
}

func TestRenderHotCounts_RedOnly(t *testing.T) {
	got := renderHotCounts(0, 2)
	if !strings.Contains(got, "2 red") {
		t.Errorf("2 red expected in output, got %q", got)
	}
	if strings.Contains(got, "yellow") {
		t.Errorf("no yellow expected, got %q", got)
	}
}

func TestRenderHotCounts_Both(t *testing.T) {
	got := renderHotCounts(3, 2)
	if !strings.Contains(got, "3 yellow") {
		t.Errorf("expected 3 yellow in %q", got)
	}
	if !strings.Contains(got, "2 red") {
		t.Errorf("expected 2 red in %q", got)
	}
}

// -- renderSubagentCount (issue #94 Phase C) ---------------------------

func TestRenderSubagentCount_HiddenWhenZero(t *testing.T) {
	if got := renderSubagentCount(0); got != "" {
		t.Errorf("0 should render empty, got %q", got)
	}
	if got := renderSubagentCount(-3); got != "" {
		t.Errorf("negative should render empty, got %q", got)
	}
}

func TestRenderSubagentCount_SingularPlural(t *testing.T) {
	// Pluralization mirrors Claude's chat-side "N local agents" wording.
	got1 := renderSubagentCount(1)
	if !strings.Contains(got1, "1 agent") || strings.Contains(got1, "agents") {
		t.Errorf("expected '1 agent' (singular), got %q", got1)
	}
	got2 := renderSubagentCount(2)
	if !strings.Contains(got2, "2 agents") {
		t.Errorf("expected '2 agents' (plural), got %q", got2)
	}
	got10 := renderSubagentCount(10)
	if !strings.Contains(got10, "10 agents") {
		t.Errorf("expected '10 agents', got %q", got10)
	}
}

// -- Phase 4: workerContextRecord lookup --------------------------------

func TestWorkerContextRecord_PrefersCoord(t *testing.T) {
	// Coord record (TaskID == "coord-<project>") wins over a sibling
	// agent in the same project.
	coord := agent.New("c00fc00f")
	coord.Project = "demo"
	coord.TaskID = "coord-demo"
	coord.ContextPct = floatPtr(33)
	other := agent.New("00000001")
	other.Project = "demo"
	other.TaskID = "regular-task"
	other.ContextPct = floatPtr(66)
	w := &WorkerRow{Project: "demo", Slug: "feat-1a2b"}
	got := workerContextRecord([]*agent.Record{other, coord}, w)
	if got == nil || got.ID != coord.ID {
		t.Fatalf("coord should win; got %+v", got)
	}
}

func TestWorkerContextRecord_FallbackToAnyProjectMatch(t *testing.T) {
	// No coord record — fall through to any project-matching record
	// with context_pct set.
	other := agent.New("00000001")
	other.Project = "demo"
	other.TaskID = "regular-task"
	other.ContextPct = floatPtr(66)
	w := &WorkerRow{Project: "demo", Slug: "feat-1a2b"}
	got := workerContextRecord([]*agent.Record{other}, w)
	if got == nil || got.ID != other.ID {
		t.Errorf("fallback should pick the project-match; got %+v", got)
	}
}

func TestWorkerContextRecord_NoMatchReturnsNil(t *testing.T) {
	other := agent.New("00000001")
	other.Project = "different-project"
	other.ContextPct = floatPtr(66)
	w := &WorkerRow{Project: "demo", Slug: "feat-1a2b"}
	if got := workerContextRecord([]*agent.Record{other}, w); got != nil {
		t.Errorf("project mismatch should return nil; got %+v", got)
	}
}

func TestWorkerContextRecord_EmptyProjectReturnsNil(t *testing.T) {
	// Defensive: a worker with no Project tag can't look anything up.
	if got := workerContextRecord(nil, &WorkerRow{Project: ""}); got != nil {
		t.Errorf("empty project should be nil; got %+v", got)
	}
}

func TestWorkerContextRecord_NilWorkerReturnsNil(t *testing.T) {
	if got := workerContextRecord(nil, nil); got != nil {
		t.Errorf("nil worker should be nil; got %+v", got)
	}
}

// -- Phase 3: agent row integration ------------------------------------

func TestRows_AgentRowRendersContextPct(t *testing.T) {
	r := agent.New("aaaa1111")
	r.Project = "demo"
	r.TaskID = "feat-1a2b"
	r.ContextPct = floatPtr(48)
	out := strings.Join(agentBlockLines(r, map[string]bool{r.ID: true}, 80, false), "\n")
	if !strings.Contains(out, "48%") {
		t.Errorf("agent row should carry the 48%% chip, got:\n%s", out)
	}
	// Issue #95: no bar glyphs anywhere in the row.
	if strings.ContainsAny(out, "▰▱") { // ▰ ▱
		t.Errorf("agent row should not carry bar glyphs, got:\n%s", out)
	}
}

func TestRows_AgentRowOmitsChipWhenContextPctNil(t *testing.T) {
	r := agent.New("aaaa1111")
	r.Project = "demo"
	r.TaskID = "feat-1a2b"
	r.ContextPct = nil
	out := strings.Join(agentBlockLines(r, map[string]bool{r.ID: true}, 80, false), "\n")
	if strings.Contains(out, "%") {
		t.Errorf("nil context_pct should not render a percent label, got:\n%s", out)
	}
	if strings.ContainsAny(out, "▰▱") {
		t.Errorf("nil context_pct should not render bar glyphs, got:\n%s", out)
	}
}

// -- Phase 4: worker row integration -----------------------------------

func TestRows_WorkerRowRendersContextPctFromAgentRecordLookup(t *testing.T) {
	// PR #87 architecture: worker subagent shares FLEET_AGENT_ID with
	// its coord. So the worker's context_pct lives on the COORD's
	// record. Lookup by project + coord task_id finds the right one.
	coord := agent.New("c00fc00f")
	coord.Project = "demo"
	coord.TaskID = "coord-demo"
	coord.ContextPct = floatPtr(72)
	w := &WorkerRow{
		ID:      "1a2b",
		Project: "demo",
		Slug:    "feat-1a2b",
		Color:   "green",
		Age:     "3m",
		State:   "ok",
	}
	out := strings.Join(workerBlockLines(w, []*agent.Record{coord}, 80, false), "\n")
	if !strings.Contains(out, "72%") {
		t.Errorf("worker row should pick up coord's 72%% chip, got:\n%s", out)
	}
	if strings.ContainsAny(out, "▰▱") {
		t.Errorf("worker row should not carry bar glyphs, got:\n%s", out)
	}
}

func TestRows_WorkerRowOmitsChipOnLookupMiss(t *testing.T) {
	// No matching agent record → no chip. Defensive: workers spawned
	// before the coord's first Stop hook fires (early-spawn race) hit
	// this path.
	w := &WorkerRow{
		ID:      "1a2b",
		Project: "no-such-project",
		Slug:    "feat-1a2b",
		Color:   "green",
		Age:     "3m",
		State:   "ok",
	}
	other := agent.New("00000001")
	other.Project = "different-project"
	other.ContextPct = floatPtr(80)
	out := strings.Join(workerBlockLines(w, []*agent.Record{other}, 80, false), "\n")
	if strings.Contains(out, "%") {
		t.Errorf("lookup miss should not render a chip, got:\n%s", out)
	}
}

func TestRows_WorkerRowOmitsChipOnNilRecords(t *testing.T) {
	// Defensive: early renders before agentsMsg lands have nil records.
	w := &WorkerRow{
		ID:      "1a2b",
		Project: "demo",
		Slug:    "feat-1a2b",
		Color:   "green",
		Age:     "3m",
		State:   "ok",
	}
	out := strings.Join(workerBlockLines(w, nil, 80, false), "\n")
	if strings.Contains(out, "%") {
		t.Errorf("nil records should not render a chip, got:\n%s", out)
	}
}

// -- Phase 6: handoff tag on row ---------------------------------------

func TestRows_HandoffTagRendersForAutoYellow(t *testing.T) {
	r := agent.New("aaaa1111")
	r.Project = "demo"
	r.TaskID = "feat-1a2b"
	r.ContextPct = floatPtr(55)
	r.HandoffType = strPtr(handoff.TypeAutoYellow)
	out := strings.Join(agentBlockLines(r, map[string]bool{r.ID: true}, 80, false), "\n")
	if !strings.Contains(out, "HANDOFF") {
		t.Errorf("auto-yellow record should render HANDOFF tag, got:\n%s", out)
	}
}

func TestRows_HandoffTagRendersForAutoRed(t *testing.T) {
	r := agent.New("aaaa1111")
	r.Project = "demo"
	r.TaskID = "feat-1a2b"
	r.ContextPct = floatPtr(75)
	r.HandoffType = strPtr(handoff.TypeAutoRed)
	out := strings.Join(agentBlockLines(r, map[string]bool{r.ID: true}, 80, false), "\n")
	if !strings.Contains(out, "HANDOFF") {
		t.Errorf("auto-red record should render HANDOFF tag, got:\n%s", out)
	}
}

func TestRows_HandoffTagAbsentWhenNil(t *testing.T) {
	r := agent.New("aaaa1111")
	r.Project = "demo"
	r.TaskID = "feat-1a2b"
	r.ContextPct = floatPtr(48)
	r.HandoffType = nil
	out := strings.Join(agentBlockLines(r, map[string]bool{r.ID: true}, 80, false), "\n")
	if strings.Contains(out, "HANDOFF") {
		t.Errorf("nil handoff_type should not render HANDOFF tag, got:\n%s", out)
	}
	if strings.Contains(out, "COMPACT") {
		t.Errorf("nil handoff_type should not render COMPACT tag, got:\n%s", out)
	}
}

// -- Phase 5: top status line hot counts -------------------------------

func TestDashboard_TopStatusShowsYellowCount(t *testing.T) {
	m := New("test")
	m.width = 130
	m.height = 30
	r := agent.New("aaaa1111")
	r.Project = "demo"
	r.ContextPct = floatPtr(55)
	m.records = []*agent.Record{r}
	m.aliveByID = map[string]bool{r.ID: true}
	out := renderDashboardHeader(m, 120)
	if !strings.Contains(out, "1 yellow") {
		t.Errorf("top status should show \"1 yellow\", got:\n%s", out)
	}
	if strings.Contains(out, "red") {
		t.Errorf("top status should not show red when only yellow agents exist, got:\n%s", out)
	}
}

func TestDashboard_TopStatusShowsRedCount(t *testing.T) {
	m := New("test")
	m.width = 130
	m.height = 30
	r1 := agent.New("aaaa1111")
	r1.Project = "demo"
	r1.ContextPct = floatPtr(75)
	r2 := agent.New("bbbb2222")
	r2.Project = "demo"
	r2.ContextPct = floatPtr(85)
	m.records = []*agent.Record{r1, r2}
	m.aliveByID = map[string]bool{r1.ID: true, r2.ID: true}
	out := renderDashboardHeader(m, 120)
	if !strings.Contains(out, "2 red") {
		t.Errorf("top status should show \"2 red\", got:\n%s", out)
	}
}

func TestDashboard_TopStatusHidesHotCountsAtZero(t *testing.T) {
	m := New("test")
	m.width = 130
	m.height = 30
	r := agent.New("aaaa1111")
	r.Project = "demo"
	r.ContextPct = floatPtr(20) // green zone
	m.records = []*agent.Record{r}
	m.aliveByID = map[string]bool{r.ID: true}
	out := renderDashboardHeader(m, 120)
	if strings.Contains(out, " yellow") || strings.Contains(out, " red") {
		t.Errorf("all-green should hide hot counts, got:\n%s", out)
	}
}
