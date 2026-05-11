package tui

import (
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
)

// resetHiddenStubs returns a deferred-cleanup that restores the disk-
// touching var stubs after a test mutates them.
func resetHiddenStubs() func() {
	r := readHiddenProjectsFn
	w := writeHiddenProjectsFn
	tg := toggleHiddenProjectFn
	return func() {
		readHiddenProjectsFn = r
		writeHiddenProjectsFn = w
		toggleHiddenProjectFn = tg
	}
}

func stubHidden(names ...string) {
	readHiddenProjectsFn = func() []string {
		if len(names) == 0 {
			return nil
		}
		out := append([]string{}, names...)
		return out
	}
}

func TestUnifiedProjects_FiltersHidden(t *testing.T) {
	defer resetHiddenStubs()()
	stubHidden("projects-fleet")

	m := New("test")
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{
			{Name: "projects-fleet"},
			{Name: "projects-tatoosh"},
		},
	}
	got := m.unifiedProjects()
	if len(got) != 1 {
		t.Fatalf("expected hidden filter to drop one row, got %d: %+v", len(got), got)
	}
	if got[0].Name != "projects-tatoosh" {
		t.Errorf("wrong row left: %s", got[0].Name)
	}

	all := m.unifiedProjectsAll()
	if len(all) != 2 {
		t.Errorf("unifiedProjectsAll should ignore hidden filter, got %d: %+v", len(all), all)
	}
}

func TestUnifiedProjects_FiltersSyntheticFromAgents(t *testing.T) {
	defer resetHiddenStubs()()
	stubHidden("loose-project")

	m := New("test")
	m.records = []*agent.Record{
		{ID: "aaa11111", Project: "loose-project", TmuxSession: "fleet-aaa"},
	}
	got := m.unifiedProjects()
	for _, p := range got {
		if p.Name == "loose-project" {
			t.Fatalf("hidden filter must drop synthetic-from-agent row too: %+v", got)
		}
	}
}

func TestClassifyProjectActivity_LiveCoord(t *testing.T) {
	now := time.Now()
	p := &ProjectRow{Name: "alpha", Active: true}
	if got := classifyProjectActivity(p, nil, nil, time.Time{}, now, 7*24*time.Hour); got != projectActive {
		t.Errorf("Active=true should be projectActive, got %v", got)
	}
}

func TestClassifyProjectActivity_FreshLastTick(t *testing.T) {
	now := time.Now()
	p := &ProjectRow{Name: "alpha", LastTick: now.Add(-1 * time.Hour)}
	if got := classifyProjectActivity(p, nil, nil, time.Time{}, now, 7*24*time.Hour); got != projectActive {
		t.Errorf("recent LastTick should be projectActive, got %v", got)
	}
}

func TestClassifyProjectActivity_StaleLastTick(t *testing.T) {
	now := time.Now()
	// 8 days ago > 7d window → idle.
	p := &ProjectRow{Name: "alpha", LastTick: now.Add(-8 * 24 * time.Hour)}
	if got := classifyProjectActivity(p, nil, nil, time.Time{}, now, 7*24*time.Hour); got != projectIdle {
		t.Errorf("stale LastTick should be projectIdle, got %v", got)
	}
}

func TestClassifyProjectActivity_FreshAgent(t *testing.T) {
	now := time.Now()
	p := &ProjectRow{Name: "alpha"}
	rec := &agent.Record{ID: "a1", Project: "alpha", LastActivityTS: now.Add(-30 * time.Minute)}
	if got := classifyProjectActivity(p, nil, []*agent.Record{rec}, time.Time{}, now, 7*24*time.Hour); got != projectActive {
		t.Errorf("fresh agent heartbeat should be projectActive, got %v", got)
	}
}

func TestClassifyProjectActivity_StaleAgent(t *testing.T) {
	now := time.Now()
	p := &ProjectRow{Name: "alpha"}
	rec := &agent.Record{ID: "a1", Project: "alpha", LastActivityTS: now.Add(-30 * 24 * time.Hour)}
	if got := classifyProjectActivity(p, nil, []*agent.Record{rec}, time.Time{}, now, 7*24*time.Hour); got != projectIdle {
		t.Errorf("stale agent heartbeat should be projectIdle, got %v", got)
	}
}

func TestClassifyProjectActivity_AgentForDifferentProject(t *testing.T) {
	now := time.Now()
	p := &ProjectRow{Name: "alpha"}
	rec := &agent.Record{ID: "a1", Project: "beta", LastActivityTS: now}
	if got := classifyProjectActivity(p, nil, []*agent.Record{rec}, time.Time{}, now, 7*24*time.Hour); got != projectIdle {
		t.Errorf("agent for different project should not flip alpha to active, got %v", got)
	}
}

func TestClassifyProjectActivity_LiveWorker(t *testing.T) {
	now := time.Now()
	p := &ProjectRow{Name: "alpha"}
	w := &WorkerRow{Project: "alpha", Slug: "task-1234"}
	if got := classifyProjectActivity(p, []*WorkerRow{w}, nil, time.Time{}, now, 7*24*time.Hour); got != projectActive {
		t.Errorf("live worker should flip project to active, got %v", got)
	}
}

func TestClassifyProjectActivity_RespectsWindow(t *testing.T) {
	now := time.Now()
	p := &ProjectRow{Name: "alpha", LastTick: now.Add(-2 * time.Hour)}
	// Window of 1h → 2h-old tick is idle.
	if got := classifyProjectActivity(p, nil, nil, time.Time{}, now, 1*time.Hour); got != projectIdle {
		t.Errorf("with 1h window, 2h-old tick should be idle; got %v", got)
	}
	// Window of 24h → 2h-old tick is active.
	if got := classifyProjectActivity(p, nil, nil, time.Time{}, now, 24*time.Hour); got != projectActive {
		t.Errorf("with 24h window, 2h-old tick should be active; got %v", got)
	}
}

// TestClassifyProjectActivity_RecentAddedAt regresses the bug: a freshly
// registered project (via `fleet project add`) classified IDLE because
// none of the existing signals (coord-state mtime, agent heartbeats,
// worker rows) fired. The fix adds projects.Meta.AddedAt as a fifth
// ACTIVE signal — a project added within the 7d active window is
// considered live regardless of operator activity. Without this the
// dashboard's [+] hotkey leaves the new row hidden under the
// "─── N idle ───" separator until the operator manually toggles
// show-all.
func TestClassifyProjectActivity_RecentAddedAt(t *testing.T) {
	now := time.Now()
	p := &ProjectRow{Name: "alpha"}
	// AddedAt = 1h ago, no coord/agents/workers.
	addedAt := now.Add(-1 * time.Hour)
	if got := classifyProjectActivity(p, nil, nil, addedAt, now, 7*24*time.Hour); got != projectActive {
		t.Errorf("recent AddedAt (1h ago, 7d window) should be projectActive, got %v", got)
	}
}

// TestClassifyProjectActivity_StaleAddedAt_NoOtherActivity pins the
// upper bound: an AddedAt outside the active window does NOT keep the
// project ACTIVE indefinitely. Without this the recency-of-add signal
// would mask a long-dormant project that should fall to IDLE.
func TestClassifyProjectActivity_StaleAddedAt_NoOtherActivity(t *testing.T) {
	now := time.Now()
	p := &ProjectRow{Name: "alpha"}
	// AddedAt = 8d ago, beyond the 7d window.
	addedAt := now.Add(-8 * 24 * time.Hour)
	if got := classifyProjectActivity(p, nil, nil, addedAt, now, 7*24*time.Hour); got != projectIdle {
		t.Errorf("stale AddedAt (8d ago, 7d window) should be projectIdle, got %v", got)
	}
}

// TestClassifyProjectActivity_ZeroAddedAt_FallsThrough: legacy projects
// pre-dating meta.json have no AddedAt on disk; the caller passes
// time.Time{}. The classifier MUST NOT treat the zero value as
// "infinitely recent" — that would erroneously flip every legacy
// project to ACTIVE. With no other signals present and a zero AddedAt
// the result must be IDLE.
func TestClassifyProjectActivity_ZeroAddedAt_FallsThrough(t *testing.T) {
	now := time.Now()
	p := &ProjectRow{Name: "alpha"}
	if got := classifyProjectActivity(p, nil, nil, time.Time{}, now, 7*24*time.Hour); got != projectIdle {
		t.Errorf("zero AddedAt with no other signals should be projectIdle, got %v", got)
	}
}

func TestResolveActiveWindow_DefaultsTo7d(t *testing.T) {
	t.Setenv("FLEET_ACTIVE_WINDOW_DAYS", "")
	if got := resolveActiveWindow(); got != activeWindowDefault {
		t.Errorf("default should be %v, got %v", activeWindowDefault, got)
	}
}

func TestResolveActiveWindow_EnvOverride(t *testing.T) {
	t.Setenv("FLEET_ACTIVE_WINDOW_DAYS", "3")
	want := 3 * 24 * time.Hour
	if got := resolveActiveWindow(); got != want {
		t.Errorf("env=3 should resolve to %v, got %v", want, got)
	}
}

func TestResolveActiveWindow_BadValueFallsBack(t *testing.T) {
	for _, raw := range []string{"abc", "0", "-1"} {
		t.Setenv("FLEET_ACTIVE_WINDOW_DAYS", raw)
		if got := resolveActiveWindow(); got != activeWindowDefault {
			t.Errorf("FLEET_ACTIVE_WINDOW_DAYS=%q should fall back, got %v", raw, got)
		}
	}
}

func TestDashboardRows_IdleSeparatorWhenActiveExists(t *testing.T) {
	defer resetHiddenStubs()()
	stubHidden() // none hidden

	now := time.Now()
	prevNow := nowFn
	nowFn = func() time.Time { return now }
	defer func() { nowFn = prevNow }()

	m := New("test")
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{
			// One ACTIVE: fresh coord-state.
			{Name: "alpha", Active: true},
			// Two IDLE: stale lastTick or no signal.
			{Name: "beta"},
			{Name: "gamma"},
		},
	}
	rows := m.dashboardRows()
	// Expected: alpha (project), separator (idle, count=2, collapsed)
	if len(rows) < 2 {
		t.Fatalf("expected at least 2 rows, got %d: %+v", len(rows), rows)
	}
	if rows[0].kind != rowProject || rows[0].project.Name != "alpha" {
		t.Errorf("row 0 should be alpha project, got %+v", rows[0])
	}
	// Find separator
	var sepIdx = -1
	for i, r := range rows {
		if r.kind == rowSeparator && r.separator.kind == separatorIdle {
			sepIdx = i
			break
		}
	}
	if sepIdx < 0 {
		t.Fatalf("expected idle separator, got rows: %+v", rows)
	}
	if rows[sepIdx].separator.count != 2 {
		t.Errorf("separator count should be 2, got %d", rows[sepIdx].separator.count)
	}
	if rows[sepIdx].separator.expanded {
		t.Errorf("separator should be collapsed by default when active rows exist")
	}
	// Idle projects should NOT be in the row list when collapsed.
	for _, r := range rows {
		if r.kind == rowProject && (r.project.Name == "beta" || r.project.Name == "gamma") {
			t.Errorf("idle project %s should be hidden under collapsed separator, got rows %+v", r.project.Name, rows)
		}
	}
}

func TestDashboardRows_IdleSeparatorExpands(t *testing.T) {
	defer resetHiddenStubs()()
	stubHidden()
	now := time.Now()
	prevNow := nowFn
	nowFn = func() time.Time { return now }
	defer func() { nowFn = prevNow }()

	m := New("test")
	m.idleExpanded = true
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{
			{Name: "alpha", Active: true},
			{Name: "beta"},
		},
	}
	rows := m.dashboardRows()
	// Expect alpha, separator(expanded=true), beta.
	var sawSep, sawBeta bool
	for _, r := range rows {
		if r.kind == rowSeparator && r.separator.kind == separatorIdle {
			sawSep = true
			if !r.separator.expanded {
				t.Errorf("separator should be expanded when idleExpanded=true")
			}
		}
		if r.kind == rowProject && r.project.Name == "beta" {
			sawBeta = true
		}
	}
	if !sawSep {
		t.Errorf("expected idle separator")
	}
	if !sawBeta {
		t.Errorf("expected beta project to render after expanded separator")
	}
}

func TestDashboardRows_NoActiveAutoSuppressesSeparator(t *testing.T) {
	defer resetHiddenStubs()()
	stubHidden()
	now := time.Now()
	prevNow := nowFn
	nowFn = func() time.Time { return now }
	defer func() { nowFn = prevNow }()

	m := New("test")
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{
			{Name: "alpha"}, // idle (no signal)
			{Name: "beta"},  // idle
		},
	}
	rows := m.dashboardRows()
	// With zero active and no explicit collapse, we suppress the
	// separator and render idle projects inline so the operator's
	// first impression isn't an empty wall.
	for _, r := range rows {
		if r.kind == rowSeparator && r.separator.kind == separatorIdle {
			t.Errorf("expected no idle separator when zero active rows, got rows: %+v", rows)
		}
	}
	// Both idle projects should be visible.
	var sawAlpha, sawBeta bool
	for _, r := range rows {
		if r.kind == rowProject && r.project.Name == "alpha" {
			sawAlpha = true
		}
		if r.kind == rowProject && r.project.Name == "beta" {
			sawBeta = true
		}
	}
	if !sawAlpha || !sawBeta {
		t.Errorf("both idle projects should render inline when no active exists")
	}
}

func TestDashboardRows_HiddenSeparatorOnlyWhenShowHidden(t *testing.T) {
	defer resetHiddenStubs()()
	stubHidden("projects-fleet")
	now := time.Now()
	prevNow := nowFn
	nowFn = func() time.Time { return now }
	defer func() { nowFn = prevNow }()

	m := New("test")
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{
			{Name: "projects-fleet"},
			{Name: "projects-tatoosh", Active: true},
		},
	}
	// Default: show-hidden off → no hidden separator.
	rows := m.dashboardRows()
	for _, r := range rows {
		if r.kind == rowSeparator && r.separator.kind == separatorHidden {
			t.Errorf("hidden separator should not render when showHidden=false")
		}
		if r.kind == rowProject && r.project.Name == "projects-fleet" {
			t.Errorf("hidden project should not render when showHidden=false")
		}
	}
	// Toggle show-hidden on → separator appears.
	m.showHidden = true
	rows = m.dashboardRows()
	var sawHiddenSep bool
	for _, r := range rows {
		if r.kind == rowSeparator && r.separator.kind == separatorHidden {
			sawHiddenSep = true
			if r.separator.count != 1 {
				t.Errorf("hidden count should be 1, got %d", r.separator.count)
			}
		}
	}
	if !sawHiddenSep {
		t.Errorf("expected hidden separator with showHidden=true")
	}
}

func TestDashboardRows_HiddenSeparatorExpands(t *testing.T) {
	defer resetHiddenStubs()()
	stubHidden("projects-fleet")
	now := time.Now()
	prevNow := nowFn
	nowFn = func() time.Time { return now }
	defer func() { nowFn = prevNow }()

	m := New("test")
	m.showHidden = true
	m.hiddenExpanded = true
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{
			{Name: "projects-fleet"},
			{Name: "projects-tatoosh", Active: true},
		},
	}
	rows := m.dashboardRows()
	var sawHiddenProject bool
	for _, r := range rows {
		if r.kind == rowProject && r.project.Name == "projects-fleet" {
			sawHiddenProject = true
		}
	}
	if !sawHiddenProject {
		t.Errorf("hidden project should render when hiddenExpanded=true")
	}
}

func TestDashboardRows_SearchSuppressesIdleSeparator(t *testing.T) {
	defer resetHiddenStubs()()
	stubHidden()
	now := time.Now()
	prevNow := nowFn
	nowFn = func() time.Time { return now }
	defer func() { nowFn = prevNow }()

	m := New("test")
	m.searchFilter = "beta"
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{
			{Name: "alpha", Active: true},
			{Name: "beta"}, // idle, but matches filter
		},
	}
	rows := m.dashboardRows()
	for _, r := range rows {
		if r.kind == rowSeparator {
			t.Errorf("search should suppress idle separator: rows=%+v", rows)
		}
	}
	var sawBeta bool
	for _, r := range rows {
		if r.kind == rowProject && r.project.Name == "beta" {
			sawBeta = true
		}
	}
	if !sawBeta {
		t.Errorf("search-matched idle project should surface inline")
	}
}

func TestRowIdentity_Separator(t *testing.T) {
	idle := dashRow{
		kind:      rowSeparator,
		separator: &separatorRow{kind: separatorIdle, count: 3},
	}
	if got := rowIdentity(idle); got != "S:idle" {
		t.Errorf("idle separator identity: got %q, want S:idle", got)
	}
	hidden := dashRow{
		kind:      rowSeparator,
		separator: &separatorRow{kind: separatorHidden, count: 1},
	}
	if got := rowIdentity(hidden); got != "S:hidden" {
		t.Errorf("hidden separator identity: got %q, want S:hidden", got)
	}
}

func TestHiddenWithActivity_CountsFreshHiddenProjects(t *testing.T) {
	now := time.Now()
	all := []*ProjectRow{
		{Name: "alpha"}, // not hidden, not active
		{Name: "beta", Active: true},
		{Name: "gamma", Active: true},
		{Name: "delta"},
	}
	hiddenSet := map[string]bool{"beta": true, "delta": true}
	got := hiddenWithActivity(all, hiddenSet, nil, nil, nil, now, 7*24*time.Hour)
	// beta is hidden + active → 1; delta is hidden but idle → 0.
	if got != 1 {
		t.Errorf("hiddenWithActivity should count 1 (beta), got %d", got)
	}
}
