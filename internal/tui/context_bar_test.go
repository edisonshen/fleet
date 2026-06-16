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

func TestContextBar_ZonesAndLabels(t *testing.T) {
	// Folded 8 per-point permutations into one table. Pins the integer
	// label + threshold zone at both edges of each flip (49/50 green→amber,
	// 69/70 amber→red) plus the 0% and 100% boundary labels. Zone gating
	// is asserted via contextZone() because lipgloss strips ANSI in
	// non-TTY test runs so the palette isn't substring-able.
	cases := []struct {
		pct   float64
		label string
		zone  string
	}{
		{0, "0%", "green"},   // low boundary, still renders a chip
		{12, "12%", "green"}, // interior green
		{48, "48%", "green"}, // just under the 50% flip
		{50, "50%", "amber"}, // exact yellow threshold
		{69, "69%", "amber"}, // just under the 70% flip
		{70, "70%", "red"},   // exact red threshold
		{95, "95%", "red"},   // interior red
		{100, "100%", "red"}, // 3-digit label, high boundary
	}
	for _, c := range cases {
		got := renderContextBar(floatPtr(c.pct))
		if !strings.Contains(got, c.label) {
			t.Errorf("%v%%: chip should contain %q, got %q", c.pct, c.label, got)
		}
		if z := contextZone(c.pct); z != c.zone {
			t.Errorf("%v%% zone want %q; got %q", c.pct, c.zone, z)
		}
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

func TestHandoffTag_SpecByType(t *testing.T) {
	// Folded 4 per-type tests into one table: label + zone for each
	// handoff type. The auto-yellow render-contains-HANDOFF check is
	// kept inline as a representative render smoke (the others share
	// the same renderHandoffTag path keyed off label/zone).
	cases := []struct {
		typ       string
		wantLabel string
		wantZone  string
	}{
		{handoff.TypeAutoYellow, "◐ HANDOFF", "amber"},
		{handoff.TypeAutoRed, "◐ HANDOFF", "red"},
		{handoff.TypeManual, "◐ HANDOFF", "dim"},
		{handoff.TypePreCompact, "◐ COMPACT", "dim"},
	}
	for _, c := range cases {
		label, zone := handoffTagSpec(strPtr(c.typ))
		if label != c.wantLabel {
			t.Errorf("%s label want %q; got %q", c.typ, c.wantLabel, label)
		}
		if zone != c.wantZone {
			t.Errorf("%s zone want %q; got %q", c.typ, c.wantZone, zone)
		}
	}
	if got := renderHandoffTag(strPtr(handoff.TypeAutoYellow)); !strings.Contains(got, "HANDOFF") {
		t.Errorf("auto-yellow render should contain HANDOFF, got %q", got)
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

func TestRenderHotCounts_Shape(t *testing.T) {
	// Folded 4 render-shape permutations into one table: hide-at-zero,
	// yellow-only, red-only, and both. wantSubstr asserts presence;
	// absentSubstr asserts the other half is omitted.
	cases := []struct {
		name         string
		yellow, red  int
		want, absent string // "" → skip that check
	}{
		{"hidden when zero", 0, 0, "", ""},
		{"yellow only", 3, 0, "3 yellow", "red"},
		{"red only", 0, 2, "2 red", "yellow"},
		{"both", 3, 2, "3 yellow", ""}, // both-present checked explicitly below
	}
	for _, c := range cases {
		got := renderHotCounts(c.yellow, c.red)
		if c.name == "hidden when zero" {
			if got != "" {
				t.Errorf("(0,0) → empty; got %q", got)
			}
			continue
		}
		if c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("%s: want %q in %q", c.name, c.want, got)
		}
		if c.absent != "" && strings.Contains(got, c.absent) {
			t.Errorf("%s: did not expect %q in %q", c.name, c.absent, got)
		}
	}
	// "both" must carry both halves.
	if got := renderHotCounts(3, 2); !strings.Contains(got, "3 yellow") || !strings.Contains(got, "2 red") {
		t.Errorf("both: expected 3 yellow + 2 red in %q", got)
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
