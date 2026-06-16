package tui

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/tasks"
	"github.com/edisonshen/fleet/internal/workers"
)

// seedTasks writes a minimal valid tasks.md under the project dir with
// the given counts of each status. Slugs are synthesized as
// "<status>-<n>-<4hex>" so they round-trip through tasks.Add /
// tasks.Write without colliding.
func seedTasks(t *testing.T, projectsRoot, project string, counts TaskCounts) {
	t.Helper()
	dir := filepath.Join(projectsRoot, project)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	f := &tasks.File{Schema: tasks.SchemaVersion}
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	add := func(status tasks.Status, n int) {
		for i := 0; i < n; i++ {
			slug := tasks.GenerateSlug(string(status), "", nil)
			tk := &tasks.Task{
				Slug:       slug,
				Status:     status,
				Priority:   tasks.PriorityP2,
				Created:    now,
				Updated:    now,
				Spec:       "spec body",
				Acceptance: "accept body",
				Notes:      "notes body",
			}
			if status == tasks.StatusBlocked {
				tk.Notes = "blocked because: missing input"
			}
			if err := f.Add(tk); err != nil {
				t.Fatalf("Add %s: %v", slug, err)
			}
		}
	}
	add(tasks.StatusTodo, counts.Todo)
	add(tasks.StatusInProgress, counts.InProgress)
	add(tasks.StatusInReview, counts.InReview)
	add(tasks.StatusBlocked, counts.Blocked)
	add(tasks.StatusDone, counts.Done)
	if err := tasks.Write(filepath.Join(dir, "tasks.md"), f); err != nil {
		t.Fatalf("write tasks.md: %v", err)
	}
}

// seedWorker writes a minimal worker state.json.
func seedWorker(t *testing.T, projectsRoot, project, slug string, s workers.State) {
	t.Helper()
	wDir := filepath.Join(projectsRoot, project, "workers", slug)
	if err := os.MkdirAll(wDir, 0o755); err != nil {
		t.Fatalf("mkdir worker: %v", err)
	}
	if s.Slug == "" {
		s.Slug = slug
	}
	if s.Project == "" {
		s.Project = project
	}
	if s.UpdatedAt.IsZero() {
		s.UpdatedAt = time.Now().UTC()
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wDir, "state.json"), data, 0o644); err != nil {
		t.Fatalf("write state.json: %v", err)
	}
}

// touchCoordLock writes a zero-byte coordinator.lock under the project
// .locks/ dir AND a coord-state.json at the project root with the
// given mtime. The two-file pair mirrors what a running coord leaves on
// disk: the lock from first acquire (mtime fixed) and coord-state.json
// from each tick (mtime advances). The dashboard reads coord-state.json
// for the active heartbeat — see scanProject's commentary.
func touchCoordLock(t *testing.T, projectsRoot, project string, mtime time.Time) {
	t.Helper()
	locksDir := filepath.Join(projectsRoot, project, ".locks")
	if err := os.MkdirAll(locksDir, 0o755); err != nil {
		t.Fatalf("mkdir locks: %v", err)
	}
	lockPath := filepath.Join(locksDir, "coordinator.lock")
	if err := os.WriteFile(lockPath, nil, 0o644); err != nil {
		t.Fatalf("write coord lock: %v", err)
	}
	statePath := filepath.Join(projectsRoot, project, "coord-state.json")
	if err := os.WriteFile(statePath, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write coord-state.json: %v", err)
	}
	if err := os.Chtimes(lockPath, mtime, mtime); err != nil {
		t.Fatalf("chtimes coord lock: %v", err)
	}
	if err := os.Chtimes(statePath, mtime, mtime); err != nil {
		t.Fatalf("chtimes coord-state.json: %v", err)
	}
}

// withFleetHome installs a tmp ~/.fleet root for the test and returns
// the projects/ subdir already created.
func withFleetHome(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("FLEET_HOME", root)
	pdir := filepath.Join(root, "projects")
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatalf("mkdir projects: %v", err)
	}
	return pdir
}

func TestView_DashboardRendersTwoColumns(t *testing.T) {
	withFleetHome(t)

	m := New("test")
	m.width = 120
	m.height = 30
	// Force an empty snapshot so the renderer takes the populated path
	// (column headings still appear regardless).
	m.dashboard = scanDashboard(time.Now())

	out := m.View()
	if !strings.Contains(out, "PROJECTS") {
		t.Errorf("dashboard should include PROJECTS column header, got:\n%s", out)
	}
	if !strings.Contains(out, "WORKERS") {
		t.Errorf("dashboard should include WORKERS column header, got:\n%s", out)
	}
}

func TestView_ProjectShowsTaskCounts(t *testing.T) {
	pdir := withFleetHome(t)
	seedTasks(t, pdir, "fleet", TaskCounts{Todo: 3, InProgress: 1, InReview: 2, Done: 7})

	m := New("test")
	m.width = 130
	m.height = 30
	m.dashboard = scanDashboard(time.Now())

	out := m.View()
	for _, want := range []string{"◌ 3", "▶ 1", "◇ 2", "✓ 7"} {
		if !strings.Contains(out, want) {
			t.Errorf("dashboard should include count chip %q, got:\n%s", want, out)
		}
	}
}

// TestProjectRow_TaskListRendersBeforeCountChips pins the issue #111
// reading-order: when a project is expanded with at least one task,
// the inline task slug must appear ABOVE the count-chip summary in
// the rendered view. Operator dogfood feedback: action first
// (the actual tasks), summary second (the count chips). Without this
// guard the bottom-up refactor in dashboard_view.go could silently
// regress to the old "counts above tasks" layout on a future tweak.
func TestProjectRow_TaskListRendersBeforeCountChips(t *testing.T) {
	pdir := withFleetHome(t)
	// Seed a project with a known-content tasks.md so we can grep the
	// rendered slug deterministically. Counts include all five chip
	// categories so the count-chip line is unambiguously identifiable
	// in the output.
	dir := filepath.Join(pdir, "fleet")
	if err := stateMkdir(dir); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	f := &tasks.File{Schema: tasks.SchemaVersion}
	const knownSlug = "reading-order-zzzz"
	if err := f.Add(&tasks.Task{
		Slug:     knownSlug,
		Status:   tasks.StatusInProgress,
		Priority: tasks.PriorityP2,
		Created:  now,
		Updated:  now,
		Spec:     "spec",
	}); err != nil {
		t.Fatalf("add %s: %v", knownSlug, err)
	}
	if err := tasks.Write(filepath.Join(dir, "tasks.md"), f); err != nil {
		t.Fatal(err)
	}

	m := New("test")
	m.width = 140
	m.height = 30
	m.dashboard = scanDashboard(time.Now())
	m.expanded = map[string]bool{"fleet": true}

	out := m.View()
	taskIdx := strings.Index(out, knownSlug)
	if taskIdx < 0 {
		t.Fatalf("expanded view missing task slug %q:\n%s", knownSlug, out)
	}
	// The count-chip line is identifiable by the in-progress chip
	// "▶ 1" (this project has exactly one in-progress task). Use the
	// LAST occurrence so we land on the project's count chip, not on
	// some unrelated-line chip earlier in the output.
	chipIdx := strings.LastIndex(out, "▶ 1")
	if chipIdx < 0 {
		t.Fatalf("expanded view missing count chip '▶ 1':\n%s", out)
	}
	// The task glyph for in-progress is also "▶". The line containing
	// the slug starts with that glyph; we want chipIdx to point at the
	// SUMMARY ▶, not the task-row ▶. Distinguish by indentation: the
	// task row uses a 4-space indent + "▶ slug"; the count chip uses
	// the project's 2-space prefix + "◌ N  ▶ N …". The cheapest pin
	// that won't fight the lipgloss escape codes: assert the slug's
	// occurrence appears BEFORE the LAST "▶ 1" — and the last "▶ 1"
	// must be on a different line than the slug.
	if taskIdx >= chipIdx {
		t.Errorf("task row must render before count-chip line\n  task slug %q at byte %d\n  count chip '▶ 1' at byte %d\nfull view:\n%s",
			knownSlug, taskIdx, chipIdx, out)
	}
}

func TestView_AttentionBadgeAppearsForBlockedWorker(t *testing.T) {
	pdir := withFleetHome(t)
	seedTasks(t, pdir, "gstack", TaskCounts{Todo: 1})
	seedWorker(t, pdir, "gstack", "fix-x-1a2b", workers.State{
		Phase:         workers.PhaseBlocked,
		BlockedReason: "question about test framework",
		PID:           12345,
	})

	m := New("test")
	m.width = 130
	m.height = 30
	m.dashboard = scanDashboard(time.Now())

	out := m.View()
	if !strings.Contains(out, "▌") {
		t.Errorf("attention row should carry the ▌ red border accent, got:\n%s", out)
	}
	if !strings.Contains(out, "attn") {
		t.Errorf("attention row should carry the 'N attn' chip, got:\n%s", out)
	}
}

func TestView_CoordStatusActiveWhenLockHeld(t *testing.T) {
	pdir := withFleetHome(t)
	seedTasks(t, pdir, "fleet", TaskCounts{Todo: 1})
	now := time.Now()
	touchCoordLock(t, pdir, "fleet", now.Add(-30*time.Second))

	m := New("test")
	m.width = 130
	m.height = 30
	m.dashboard = scanDashboard(now)

	out := m.View()
	if !strings.Contains(out, "● active") {
		t.Errorf("recent coord lock mtime should render as ● active, got:\n%s", out)
	}
}

func TestView_CoordStatusIdleWhenLockStale(t *testing.T) {
	pdir := withFleetHome(t)
	seedTasks(t, pdir, "fleet", TaskCounts{Todo: 1})
	now := time.Now()
	// Older than coordActiveWindow → auto-stopped pill.
	touchCoordLock(t, pdir, "fleet", now.Add(-2*time.Hour))

	m := New("test")
	m.width = 130
	m.height = 30
	m.dashboard = scanDashboard(now)

	out := m.View()
	if !strings.Contains(out, "auto-stopped") {
		t.Errorf("stale coord lock should render as ○ idle · auto-stopped, got:\n%s", out)
	}
}

// TestView_CoordStatusIdleWhenLockOnly pins the case where
// coordinator.lock exists but coord-state.json does NOT. flock(2)
// doesn't update mtime, so a stale lock without a fresh state file
// must NOT read as active — it should render as auto-stopped instead.
func TestView_CoordStatusIdleWhenLockOnly(t *testing.T) {
	pdir := withFleetHome(t)
	seedTasks(t, pdir, "fleet", TaskCounts{Todo: 1})
	// Write only the lock file, skipping coord-state.json. mtime
	// freshness on the lock file is irrelevant by design.
	locksDir := filepath.Join(pdir, "fleet", ".locks")
	if err := os.MkdirAll(locksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locksDir, "coordinator.lock"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	m := New("test")
	m.width = 130
	m.height = 30
	m.dashboard = scanDashboard(time.Now())

	out := m.View()
	if strings.Contains(out, "● active") {
		t.Errorf("lock-only (no coord-state.json) must NOT show active, got:\n%s", out)
	}
	if !strings.Contains(out, "auto-stopped") {
		t.Errorf("lock-only should render auto-stopped, got:\n%s", out)
	}
}

func TestView_HeaderShowsTotals(t *testing.T) {
	pdir := withFleetHome(t)
	seedTasks(t, pdir, "fleet", TaskCounts{Todo: 1})
	seedTasks(t, pdir, "gstack", TaskCounts{Todo: 1})
	seedTasks(t, pdir, "side", TaskCounts{Todo: 1})
	for i, slug := range []string{"a-1111", "b-2222", "c-3333", "d-4444", "e-5555"} {
		project := "fleet"
		if i >= 3 {
			project = "gstack"
		}
		seedWorker(t, pdir, project, slug, workers.State{
			Phase: workers.PhaseTDDGreen,
			PID:   1000 + i,
		})
	}

	m := New("test")
	m.width = 140
	m.height = 30
	m.dashboard = scanDashboard(time.Now())

	out := m.View()
	if !strings.Contains(out, "3 projects") {
		t.Errorf("header should show 3 projects, got:\n%s", out)
	}
	if !strings.Contains(out, "5 workers active") {
		t.Errorf("header should show 5 workers active, got:\n%s", out)
	}
}

// TestView_HeaderShowsAgentsChipWhenSubagentsRegistered pins the issue
// #94 Phase C "<N> agents" top-status chip. When at least one worker
// row carries a SubagentID, the strip renders "<N> agent[s]" alongside
// the existing yellow / red chips. Mirrors hide-at-zero so the strip
// stays clean for projects without registered subagents.
func TestView_HeaderShowsAgentsChipWhenSubagentsRegistered(t *testing.T) {
	pdir := withFleetHome(t)
	// Seed two workers with state.json, register subagents for both.
	seedWorker(t, pdir, "fleet", "alpha-aaaa", workers.State{Phase: workers.PhaseTDDRed})
	seedWorker(t, pdir, "fleet", "beta-bbbb", workers.State{Phase: workers.PhaseTDDGreen})
	writeCoordStateWithSubagentMap(t, pdir, "fleet", map[string]string{
		"alpha-aaaa": "claude-sub-1",
		"beta-bbbb":  "claude-sub-2",
	}, false, "")
	m := New("test")
	m.width = 140
	m.height = 30
	m.dashboard = scanDashboard(time.Now())
	out := m.View()
	if !strings.Contains(out, "2 agents") {
		t.Errorf("header should show '2 agents' chip; got:\n%s", out)
	}
}

func TestView_HeaderHidesAgentsChipWhenNoneRegistered(t *testing.T) {
	pdir := withFleetHome(t)
	// Worker exists but no subagent_ids registered. Chip must NOT render.
	seedWorker(t, pdir, "fleet", "alpha-aaaa", workers.State{Phase: workers.PhaseTDDRed})
	m := New("test")
	m.width = 140
	m.height = 30
	m.dashboard = scanDashboard(time.Now())
	out := m.View()
	if strings.Contains(out, " agents") || strings.Contains(out, "1 agent") {
		// Defensive — no `agents` substring with a count prefix anywhere
		// in the header. (Other "agents" strings in the body — like the
		// v0.1 sub-heading — render with a different format.)
		if strings.Contains(out, "0 agent") || strings.Contains(out, "1 agent ") {
			t.Errorf("agents chip should be hidden at 0; got:\n%s", out)
		}
	}
}

func TestView_HeaderShowsSingularAgentChip(t *testing.T) {
	pdir := withFleetHome(t)
	seedWorker(t, pdir, "fleet", "alpha-aaaa", workers.State{Phase: workers.PhaseTDDRed})
	writeCoordStateWithSubagentMap(t, pdir, "fleet", map[string]string{
		"alpha-aaaa": "claude-sub-1",
	}, false, "")
	m := New("test")
	m.width = 140
	m.height = 30
	m.dashboard = scanDashboard(time.Now())
	out := m.View()
	if !strings.Contains(out, "1 agent") {
		t.Errorf("header should show '1 agent' (singular); got:\n%s", out)
	}
	// Must NOT pluralize on N=1.
	if strings.Contains(out, "1 agents") {
		t.Errorf("singular form drift: header rendered '1 agents'; got:\n%s", out)
	}
}

// TestWorkerBlockLines_RendersSubagentChip pins per-row rendering.
// When a WorkerRow carries SubagentID, the first 8 chars render as a
// `· <hash>` chip directly after the worker ID. Empty SubagentID
// yields no chip — the row keeps its 2-token "<dot> <id>" head shape.
func TestWorkerBlockLines_RendersSubagentChip(t *testing.T) {
	w := &WorkerRow{
		ID:         "1a2b",
		Project:    "fleet",
		Slug:       "alpha-1a2b",
		Phase:      workers.PhaseTDDRed,
		Color:      "green",
		Age:        "2m",
		State:      "ok",
		SubagentID: "claude-sub-1",
	}
	lines := workerBlockLines(w, nil, 60, false)
	if len(lines) < 2 {
		t.Fatalf("expected ≥2 lines, got %d", len(lines))
	}
	// First 8 chars of "claude-sub-1" → "claude-s".
	if !strings.Contains(lines[0], "claude-s") {
		t.Errorf("expected truncated subagent chip in line 1, got:\n%s", lines[0])
	}
	if !strings.Contains(lines[0], "·") {
		t.Errorf("expected `·` separator in line 1, got:\n%s", lines[0])
	}
}

func TestWorkerBlockLines_OmitsChipWhenSubagentEmpty(t *testing.T) {
	// Codex regression guard: an empty SubagentID must NOT print a stray
	// `· ` prefix. The row keeps its existing head shape.
	w := &WorkerRow{
		ID:      "1a2b",
		Project: "fleet",
		Slug:    "alpha-1a2b",
		Phase:   workers.PhaseTDDRed,
		Color:   "green",
		Age:     "2m",
		State:   "ok",
		// SubagentID intentionally blank.
	}
	lines := workerBlockLines(w, nil, 60, false)
	if len(lines) < 2 {
		t.Fatalf("expected ≥2 lines, got %d", len(lines))
	}
	// The leading 4-char-id segment "1a2b" must NOT be followed by a `·`
	// before the chip area. The slug "fleet:alpha" appears in line 2,
	// not line 1, so check line 1 specifically.
	// "·" appears for the line-2 :slug separator? No, that uses ":". So
	// line 1 with empty SubagentID should never carry `·`.
	if strings.Contains(lines[0], "·") {
		t.Errorf("empty SubagentID should NOT print `·` separator; got:\n%s", lines[0])
	}
}

func TestWorkerBlockLines_TruncatesLongSubagentID(t *testing.T) {
	// Long Claude tokens render the first 8 runes only — keeps the row
	// width predictable across operator-set wide terminals.
	w := &WorkerRow{
		ID:         "1a2b",
		Project:    "fleet",
		Slug:       "alpha-1a2b",
		Phase:      workers.PhaseTDDRed,
		Color:      "green",
		Age:        "2m",
		State:      "ok",
		SubagentID: "verylongclaudesubagentid123",
	}
	lines := workerBlockLines(w, nil, 80, false)
	if !strings.Contains(lines[0], "verylong") {
		t.Errorf("expected first 8 chars of long token, got:\n%s", lines[0])
	}
	// Should NOT carry the 9th char ('c').
	if strings.Contains(lines[0], "verylongc") {
		t.Errorf("truncation failed — 9th char leaked, got:\n%s", lines[0])
	}
}

// TestView_AgentsRenderInDashboard_NoGToggle pins issue #53 part A:
// agent records appear inside the dashboard's right column under the
// "v0.1 agents — N active" sub-heading, NOT behind a [g] toggle. The
// [g] keybind no longer exists.
func TestView_AgentsRenderInDashboard_NoGToggle(t *testing.T) {
	m := New("test")
	m.width = 140
	m.height = 30
	m.records = []*agent.Record{
		{
			SchemaVersion: 1,
			ID:            "agent99",
			TmuxSession:   "fleet-agent99",
			Project:       "demo",
			TaskID:        "demo-task",
			SpawnedAt:     time.Now().UTC(),
		},
	}
	out := m.View()
	if !strings.Contains(out, "v0.1 agents") {
		t.Errorf("dashboard should include the v0.1 agents sub-heading, got:\n%s", out)
	}
	if !strings.Contains(out, "agent99") {
		t.Errorf("dashboard should render the agent ID, got:\n%s", out)
	}

	// Pressing [g] is a no-op — the legacy toggle is gone.
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("g")})
	if cmd != nil {
		t.Errorf("[g] should be a no-op, got non-nil cmd")
	}
	out2 := updated.(Model).View()
	if !strings.Contains(out2, "v0.1 agents") {
		t.Errorf("[g] must not switch views; dashboard should still render. got:\n%s", out2)
	}
}

func TestScanDashboard_SkipsLocksDir(t *testing.T) {
	// The reserved ".locks" directory under projects/ must not be
	// surfaced as a project row.
	pdir := withFleetHome(t)
	if err := os.MkdirAll(filepath.Join(pdir, ".locks"), 0o755); err != nil {
		t.Fatal(err)
	}
	seedTasks(t, pdir, "fleet", TaskCounts{Todo: 1})

	snap := scanDashboard(time.Now())
	for _, p := range snap.Projects {
		if p.Name == ".locks" {
			t.Errorf(".locks must not be rendered as a project: %+v", p)
		}
	}
}

// TestScanDashboard_SkipsMalformedProjectDir regresses
// invalid-project-dir-guar-d636: a "--project" dir (CLI flag-misparse)
// must NOT render as a project row, must NOT inflate the count, and —
// because "--project" sorts before letters — must NOT hijack the title.
// The malformed name is surfaced via Snapshot.SkippedMalformed (loader
// emits a stderr hint), not silently dropped.
func TestScanDashboard_SkipsMalformedProjectDir(t *testing.T) {
	pdir := withFleetHome(t)
	// A real project + the bogus "--project" dir sitting alongside it,
	// shaped like the operator's screenshot: .locks + coord-state.json,
	// NO tasks.md.
	seedTasks(t, pdir, "fleet", TaskCounts{Todo: 1})
	bogus := filepath.Join(pdir, "--project")
	if err := os.MkdirAll(filepath.Join(bogus, ".locks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bogus, "coord-state.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	snap := scanDashboard(time.Now())
	for _, p := range snap.Projects {
		if p.Name == "--project" {
			t.Errorf("malformed --project dir must not render as a project row: %+v", p)
		}
	}
	if len(snap.Projects) != 1 || snap.Projects[0].Name != "fleet" {
		t.Fatalf("expected exactly the valid 'fleet' project; got %d projects %+v",
			len(snap.Projects), snap.Projects)
	}
	// Surfaced, not silently dropped.
	found := false
	for _, n := range snap.SkippedMalformed {
		if n == "--project" {
			found = true
		}
	}
	if !found {
		t.Errorf("--project must be reported in SkippedMalformed; got %v", snap.SkippedMalformed)
	}
}

// TestUnifiedProjects_SkipsMalformedAgentTag regresses the second
// enumeration source: a loose agent record tagged project="--project"
// must not synthesize a row that re-hijacks the title via the
// agent-record path (invalid-project-dir-guar-d636).
func TestUnifiedProjects_SkipsMalformedAgentTag(t *testing.T) {
	// Isolate FLEET_HOME so hiddenProjectsSet() reads an empty hidden
	// list (not the operator's real ~/.fleet) — otherwise the synthetic
	// loop's hidden filter is non-deterministic across machines.
	t.Setenv("FLEET_HOME", t.TempDir())
	m := Model{
		dashboard: &Snapshot{Projects: []*ProjectRow{{Name: "fleet", RepoSlug: "fleet"}}},
		records: []*agent.Record{
			{ID: "aaaa1111", Project: "--project"},
			{ID: "bbbb2222", Project: "rainier"},
		},
	}
	got := m.unifiedProjects()
	for _, p := range got {
		if p.Name == "--project" {
			t.Errorf("agent tag '--project' must not synthesize a project row: %+v", p)
		}
	}
	// The valid synthetic 'rainier' tag should still appear.
	haveRainier := false
	for _, p := range got {
		if p.Name == "rainier" {
			haveRainier = true
		}
	}
	if !haveRainier {
		t.Errorf("valid synthetic project 'rainier' should still appear; got %+v", got)
	}
}

func TestScanDashboard_AttentionProjectsSortedFirst(t *testing.T) {
	pdir := withFleetHome(t)
	seedTasks(t, pdir, "alpha", TaskCounts{Todo: 1})
	// Issue #103: attention is fired by worker phase=blocked only —
	// task status=blocked is planning state and no longer contributes.
	// Seed zulu with a worker raising its hand so the sort-first
	// invariant ("attention projects rise to the top") still has a
	// signal to assert against.
	seedTasks(t, pdir, "zulu", TaskCounts{Todo: 1})
	seedWorker(t, pdir, "zulu", "needs-input-aaaa", workers.State{
		Phase:         workers.PhaseBlocked,
		BlockedReason: "operator clarification on API shape",
	})

	snap := scanDashboard(time.Now())
	if len(snap.Projects) != 2 {
		t.Fatalf("expected 2 projects, got %d", len(snap.Projects))
	}
	if snap.Projects[0].Name != "zulu" {
		t.Errorf("attention project should sort first: got %q first", snap.Projects[0].Name)
	}
}

func TestWorkerRow_ColorByPhase(t *testing.T) {
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		phase     workers.Phase
		wantColor string
		wantState string
	}{
		{workers.PhaseBlocked, "red", "bl"},
		{workers.PhaseFailed, "red", "!!"},
		{workers.PhaseReviewClaude, "blue", "rv"},
		{workers.PhaseReviewCodex, "blue", "rv"},
		{workers.PhaseDone, "green", "ok"},
		{workers.PhaseTDDGreen, "green", "ok"},
		{workers.PhasePush, "amber", "rn"},
	}
	for _, c := range cases {
		t.Run(string(c.phase), func(t *testing.T) {
			s := &workers.State{
				Slug:          "x-1234",
				Phase:         c.phase,
				BlockedReason: "x",
				PRURL:         "https://example/pr/1",
				UpdatedAt:     now.Add(-1 * time.Minute),
			}
			row := workerRowFor(s, "p", now)
			if row.Color != c.wantColor {
				t.Errorf("phase=%s color=%s want=%s", c.phase, row.Color, c.wantColor)
			}
			if row.State != c.wantState {
				t.Errorf("phase=%s state=%s want=%s", c.phase, row.State, c.wantState)
			}
		})
	}
}

func TestWorkerRow_ZeroUpdatedAtRendersDash(t *testing.T) {
	// Defensive: a malformed state.json with no UpdatedAt must not
	// render the age as "734503d" (now minus the zero-time).
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	s := &workers.State{
		Slug:  "x-1234",
		Phase: workers.PhaseTDDGreen,
		// UpdatedAt left as zero on purpose.
	}
	row := workerRowFor(s, "p", now)
	if row.Age != "—" {
		t.Errorf("zero UpdatedAt should render age as '—', got %q", row.Age)
	}
}

func TestWorkerRow_StaleHeartbeatGoesAmber(t *testing.T) {
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	s := &workers.State{
		Slug:      "x-1234",
		Phase:     workers.PhaseTDDGreen,
		UpdatedAt: now.Add(-15 * time.Minute), // > workerStaleWindow
	}
	row := workerRowFor(s, "p", now)
	if row.Color != "amber" || row.State != "!!" {
		t.Errorf("stale worker should be amber/!!, got color=%s state=%s", row.Color, row.State)
	}
}

func TestSnapshot_CIRunningCountsReviewPhases(t *testing.T) {
	now := time.Now()
	snap := &Snapshot{
		Workers: []*WorkerRow{
			{Phase: workers.PhaseReviewClaude},
			{Phase: workers.PhaseReviewCodex},
			{Phase: workers.PhasePush},
			{Phase: workers.PhaseTDDGreen},
			{Phase: workers.PhaseBlocked},
		},
		LoadedAt: now,
	}
	if got := snap.CIRunning(); got != 3 {
		t.Errorf("CIRunning = %d, want 3 (review-claude + review-codex + push)", got)
	}
}

func TestDeriveRepoSlug(t *testing.T) {
	// Three extraction sources folded into one table: github URL in
	// standards.md, `- repo:` bullet, and the fall-back-to-name path
	// when standards.md is absent.
	cases := []struct {
		name      string
		standards string // "" → don't write standards.md (fallback path)
		project   string
		want      string
	}{
		{
			name:      "from github URL",
			standards: "---\nschema: v1\n---\n\n# Standards\n\n## Repo\n\nSee https://github.com/edisonshen/fleet for source.\n",
			project:   "fleet",
			want:      "edisonshen/fleet",
		},
		{
			name:      "from `- repo:` bullet",
			standards: "---\nschema: v1\n---\n\n## Repo\n\n- repo: edisonshen/gstack\n",
			project:   "gstack",
			want:      "edisonshen/gstack",
		},
		{
			name:    "fallback to name when standards.md absent",
			project: "side-experiment",
			want:    "side-experiment",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tmp := t.TempDir()
			if c.standards != "" {
				if err := os.WriteFile(filepath.Join(tmp, "standards.md"), []byte(c.standards), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if got := deriveRepoSlug(tmp, c.project); got != c.want {
				t.Errorf("deriveRepoSlug = %q, want %q", got, c.want)
			}
		})
	}
}

func TestTrimSlug(t *testing.T) {
	cases := map[string]string{
		"fix-toolbar-1a2b":    "fix-toolbar",
		"add-poke-7a3c":       "add-poke",
		"abc":                 "abc",
		"too-short-12":        "too-short-12",
		"non-hex-suffix-zzzz": "non-hex-suffix-zzzz",
	}
	for in, want := range cases {
		if got := trimSlug(in); got != want {
			t.Errorf("trimSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestUpdate_DashboardMsg_Stores pins that the dashboardMsg reducer
// installs the snapshot into Model.dashboard.
func TestUpdate_DashboardMsg_Stores(t *testing.T) {
	m := New("test")
	snap := &Snapshot{LoadedAt: time.Now()}
	updated, _ := m.Update(dashboardMsg{snap: snap})
	if updated.(Model).dashboard != snap {
		t.Errorf("dashboardMsg should install snapshot pointer into Model")
	}
}

// TestView_DashboardEmptyShowsHint pins that an empty projects tree
// renders a coachmark instead of a blank column. The hint nudges to
// [n] task — matches the footer keybind chip (issue #55). The earlier
// [d] text was introduced in codex iter-7 to dodge the "no project
// context" flash on fresh installs; the operator clarified that hint
// + footer must agree on a single key, so we revert to [n].
func TestView_DashboardEmptyShowsHint(t *testing.T) {
	withFleetHome(t)
	m := New("test")
	m.width = 120
	m.height = 30
	m.dashboard = scanDashboard(time.Now())
	out := m.View()
	if !strings.Contains(out, "no projects yet") {
		t.Errorf("empty dashboard should hint, got:\n%s", out)
	}
	if !strings.Contains(out, "[n]") {
		t.Errorf("empty dashboard should nudge to [n], got:\n%s", out)
	}
	if strings.Contains(out, "[d] to dispatch") {
		t.Errorf("empty dashboard must not advertise [d] (footer is [n]); got:\n%s", out)
	}
}

// TestView_DashboardSurfacesBanners regresses the codex P1: in the v0.2
// default dashboard view, View() bypassed renderTop(), so flash, agent
// load errors, and the upgrade banner went unrendered. Operators on
// the default view stopped seeing dispatch/handoff/rm failures and
// upgrade nudges entirely.
func TestView_DashboardSurfacesBanners(t *testing.T) {
	withFleetHome(t)
	m := New("test")
	m.width = 120
	m.height = 30
	m.dashboard = scanDashboard(time.Now())

	// Upgrade banner should render in dashboard mode.
	m.upgradeBanner = "v9.9.9 — brew upgrade fleet"
	out := m.View()
	if !strings.Contains(out, "v9.9.9") {
		t.Errorf("dashboard view should render upgradeBanner, got:\n%s", out)
	}

	// Error flash from a failed dispatch must show.
	m.flash = &flashMsg{text: "dispatch failed: boom", isErr: true}
	out = m.View()
	if !strings.Contains(out, "dispatch failed: boom") {
		t.Errorf("dashboard view should render flash, got:\n%s", out)
	}

	// Agent-load error must show.
	m.flash = nil
	m.upgradeBanner = ""
	m.err = errors.New("read failed")
	out = m.View()
	if !strings.Contains(out, "read failed") {
		t.Errorf("dashboard view should render agent-load err, got:\n%s", out)
	}
}

// TestView_FooterShowsKeybinds pins the footer renders the action
// chip strip + uptime indicator. Issue #90: nav (j/k) and panel
// (←/→) chips are intentionally absent — they're documented in the
// [?] help overlay only.
func TestView_FooterShowsKeybinds(t *testing.T) {
	withFleetHome(t)
	m := New("test")
	m.width = 130
	m.height = 30
	m.dashboard = scanDashboard(time.Now())
	out := m.View()
	for _, want := range []string{"⏎", "open", "uptime"} {
		if !strings.Contains(out, want) {
			t.Errorf("footer should include %q, got:\n%s", want, out)
		}
	}
}

// TestReadCoordHolder pins the lock-body parsing rules: 8-char
// lower-hex passes, anything else returns "" so the dashboard falls
// through to the no-coord render path.
func TestReadCoordHolder(t *testing.T) {
	cases := []struct {
		name string
		body []byte
		want string
	}{
		{"valid 8-hex with newline", []byte("cafef00d\n"), "cafef00d"},
		{"valid 8-hex no newline", []byte("abcd1234"), "abcd1234"},
		{"valid 8-hex with CRLF", []byte("12345678\r\n"), "12345678"},
		{"empty body", []byte(""), ""},
		{"too short", []byte("abc12\n"), ""},
		{"too long", []byte("abcdef01abcdef01\n"), ""},
		{"uppercase rejected", []byte("CAFEF00D\n"), ""},
		{"non-hex chars", []byte("zzzzzzzz\n"), ""},
		{"whitespace stripped, then valid", []byte("  cafef00d  \n"), "cafef00d"},
		{"multi-line takes first", []byte("aaaa1111\nbbbb2222\n"), "aaaa1111"},
		{"first line garbage, second valid", []byte("garbage\naaaa1111\n"), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pdir := t.TempDir()
			locksDir := filepath.Join(pdir, "demo", ".locks")
			if err := os.MkdirAll(locksDir, 0o755); err != nil {
				t.Fatal(err)
			}
			lockPath := filepath.Join(locksDir, "coordinator.lock")
			if err := os.WriteFile(lockPath, c.body, 0o644); err != nil {
				t.Fatal(err)
			}
			got := readCoordHolder(pdir, "demo")
			if got != c.want {
				t.Errorf("readCoordHolder(%q) = %q, want %q", c.body, got, c.want)
			}
		})
	}
}

// TestReadCoordHolder_MissingFile pins the missing-lock case: returns
// "" without surfacing the I/O error to the caller.
func TestReadCoordHolder_MissingFile(t *testing.T) {
	pdir := t.TempDir()
	if got := readCoordHolder(pdir, "nope"); got != "" {
		t.Errorf("missing lock should yield \"\", got %q", got)
	}
}

// TestScanProject_CountsJetsamIncidentsForProject pins PART 3 of the
// bundled liveness-correctness fix (2026-05-13): when the kernel
// kills a claude process under memory pressure (macOS
// memorystatus / jetsam), fleet-guard writes a structured incident
// JSON to ~/.fleet/incidents/. The dashboard scan counts these per
// project so the row can render a "killed by memorystatus" badge.
//
// Incident JSON shape (per skills/fleet-guard/jetsam.py):
//
//	{
//	  "schema_version": 1,
//	  "agent_id": "...",
//	  "pid": N,
//	  "reason": "highwater" | "vm-thrashing" | "fork-fail" | ...,
//	  "killed_at": "<RFC3339>",
//	  "project": "<project-name>",
//	  ...
//	}
//
// scanProject reads the incidents dir once and increments
// row.IncidentCount for each record whose `project` field matches.
func TestScanProject_CountsJetsamIncidentsForProject(t *testing.T) {
	pdir := withFleetHome(t)
	seedTasks(t, pdir, "demo", TaskCounts{Todo: 1})
	root := filepath.Dir(pdir) // ~/.fleet
	incidentsDir := filepath.Join(root, "incidents")
	if err := os.MkdirAll(incidentsDir, 0o755); err != nil {
		t.Fatalf("mkdir incidents: %v", err)
	}
	writeIncident := func(name, project string) {
		body := map[string]interface{}{
			"schema_version": 1,
			"agent_id":       name,
			"pid":            12345,
			"reason":         "highwater",
			"killed_at":      "2026-05-13T17:20:01Z",
			"project":        project,
		}
		data, err := json.MarshalIndent(body, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(incidentsDir, name+".json")
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeIncident("agent1", "demo")
	writeIncident("agent2", "demo")
	writeIncident("agent3", "other-project")

	snap := scanDashboard(time.Now())
	if len(snap.Projects) != 1 {
		t.Fatalf("expected 1 project, got %d", len(snap.Projects))
	}
	p := snap.Projects[0]
	if p.IncidentCount != 2 {
		t.Errorf("IncidentCount = %d, want 2 (only demo-tagged incidents)", p.IncidentCount)
	}
}

// TestScanProject_NoIncidentsLeavesCountZero pins the no-op path:
// when the incidents dir is missing or contains no matching records,
// IncidentCount stays 0 and no badge renders.
func TestScanProject_NoIncidentsLeavesCountZero(t *testing.T) {
	pdir := withFleetHome(t)
	seedTasks(t, pdir, "demo", TaskCounts{Todo: 1})
	snap := scanDashboard(time.Now())
	if len(snap.Projects) != 1 {
		t.Fatalf("expected 1 project, got %d", len(snap.Projects))
	}
	if snap.Projects[0].IncidentCount != 0 {
		t.Errorf("IncidentCount = %d, want 0", snap.Projects[0].IncidentCount)
	}
}

// TestScanProject_SkipsMalformedIncidentJSON ensures one bad incident
// file doesn't poison the whole scan. The bad file is skipped; valid
// matching incidents still count.
func TestScanProject_SkipsMalformedIncidentJSON(t *testing.T) {
	pdir := withFleetHome(t)
	seedTasks(t, pdir, "demo", TaskCounts{Todo: 1})
	root := filepath.Dir(pdir)
	incidentsDir := filepath.Join(root, "incidents")
	if err := os.MkdirAll(incidentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Garbage file — scanner must skip silently.
	if err := os.WriteFile(filepath.Join(incidentsDir, "bad.json"),
		[]byte("{ this is not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	// One good incident.
	body, _ := json.Marshal(map[string]interface{}{
		"schema_version": 1, "agent_id": "g", "pid": 1,
		"reason": "highwater", "project": "demo",
	})
	if err := os.WriteFile(filepath.Join(incidentsDir, "good.json"),
		body, 0o644); err != nil {
		t.Fatal(err)
	}
	snap := scanDashboard(time.Now())
	if len(snap.Projects) != 1 {
		t.Fatalf("expected 1 project, got %d", len(snap.Projects))
	}
	if snap.Projects[0].IncidentCount != 1 {
		t.Errorf("IncidentCount = %d, want 1 (good incident only)",
			snap.Projects[0].IncidentCount)
	}
}

// TestProjectBlock_RendersMemorystatusBadgeOnIncident pins PART 3
// of the bundled liveness-correctness fix: when ProjectRow.IncidentCount
// > 0, the project block header renders an attention-chip badge
// reading "✗ killed by memorystatus" (or "✗ N killed by memorystatus"
// for N > 1). Pluralization mirrors the post-archive badge's compact
// shape (no number when count == 1).
func TestProjectBlock_RendersMemorystatusBadgeOnIncident(t *testing.T) {
	withFleetHome(t)
	now := time.Now()
	stubMarkerMtime(t, "demo", time.Time{}, false) // no spawn marker
	cases := []struct {
		count     int
		wantBadge string
	}{
		{1, "✗ killed by memorystatus"},
		{2, "✗ 2 killed by memorystatus"},
		{5, "✗ 5 killed by memorystatus"},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("count_%d", tc.count), func(t *testing.T) {
			p := &ProjectRow{
				Name:          "demo",
				RepoSlug:      "demo",
				IncidentCount: tc.count,
			}
			ctx := coordSpawnCtx{now: now, tickFrame: 0, spawnTimeout: 10 * time.Minute}
			lines := projectBlockLines(p, 80, false, ctx)
			joined := strings.Join(lines, "\n")
			if !strings.Contains(joined, tc.wantBadge) {
				t.Errorf("incident count %d → badge %q not found; got:\n%s",
					tc.count, tc.wantBadge, joined)
			}
		})
	}
}

// TestProjectBlock_NoBadgeWhenIncidentCountZero ensures the badge
// path is only entered when there's evidence — the badge is too
// visually loud to render unconditionally.
func TestProjectBlock_NoBadgeWhenIncidentCountZero(t *testing.T) {
	withFleetHome(t)
	now := time.Now()
	stubMarkerMtime(t, "demo", time.Time{}, false)
	p := &ProjectRow{Name: "demo", RepoSlug: "demo", IncidentCount: 0}
	ctx := coordSpawnCtx{now: now, tickFrame: 0, spawnTimeout: 10 * time.Minute}
	lines := projectBlockLines(p, 80, false, ctx)
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, "killed by memorystatus") {
		t.Errorf("count=0 must not render badge; got:\n%s", joined)
	}
}

// TestScanProject_AttachesCoordIDWhenFresh pins that scanProject
// populates row.CoordID when the lock body has a valid 8-hex ID AND
// coord-state.json is fresh enough to mark the project Active.
func TestScanProject_AttachesCoordIDWhenFresh(t *testing.T) {
	pdir := withFleetHome(t)
	seedTasks(t, pdir, "demo", TaskCounts{Todo: 1})
	now := time.Now()
	touchCoordLock(t, pdir, "demo", now.Add(-30*time.Second))
	// Overwrite the lock body with a valid coord ID. touchCoordLock
	// writes a zero-byte lock; we want the v0.2 lock-body protocol.
	lockPath := filepath.Join(pdir, "demo", ".locks", "coordinator.lock")
	if err := os.WriteFile(lockPath, []byte("c0ffee01\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Re-stamp the mtime since WriteFile bumped it; freshness gate
	// reads from coord-state.json's mtime, but be paranoid.
	if err := os.Chtimes(filepath.Join(pdir, "demo", "coord-state.json"),
		now.Add(-30*time.Second), now.Add(-30*time.Second)); err != nil {
		t.Fatal(err)
	}
	snap := scanDashboard(now)
	if len(snap.Projects) != 1 {
		t.Fatalf("expected 1 project, got %d", len(snap.Projects))
	}
	p := snap.Projects[0]
	if p.CoordID != "c0ffee01" {
		t.Errorf("expected CoordID=c0ffee01, got %q (Active=%v)", p.CoordID, p.Active)
	}
}

// TestScanProject_StaleCoordLockHasNoCoordID pins the freshness gate.
// A stale coord-state.json (Active=false) must NOT publish a CoordID,
// even when the lock body still carries a valid-shape ID. flock(2)
// doesn't truncate on release, so a body alone is not trustworthy.
func TestScanProject_StaleCoordLockHasNoCoordID(t *testing.T) {
	pdir := withFleetHome(t)
	seedTasks(t, pdir, "demo", TaskCounts{Todo: 1})
	now := time.Now()
	// 2h old → outside coordActiveWindow.
	touchCoordLock(t, pdir, "demo", now.Add(-2*time.Hour))
	lockPath := filepath.Join(pdir, "demo", ".locks", "coordinator.lock")
	if err := os.WriteFile(lockPath, []byte("c0ffee01\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(pdir, "demo", "coord-state.json"),
		now.Add(-2*time.Hour), now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	snap := scanDashboard(now)
	if len(snap.Projects) != 1 {
		t.Fatalf("expected 1 project, got %d", len(snap.Projects))
	}
	p := snap.Projects[0]
	if p.CoordID != "" {
		t.Errorf("stale coord-state.json must zero CoordID, got %q", p.CoordID)
	}
	if p.Active {
		t.Errorf("stale coord must not be Active")
	}
}

// TestUnifiedProjects_UnionsAgentTagsWithDashboardDirs pins the LEFT
// column union (issue #55): projects on disk under
// ~/.fleet/projects/<tag>/ AND project tags carried by agent records
// both surface as project rows. Without this, an operator with active
// agents on non-v0.2-init'd repos sees a blank PROJECTS column.
func TestUnifiedProjects_UnionsAgentTagsWithDashboardDirs(t *testing.T) {
	pdir := withFleetHome(t)
	seedTasks(t, pdir, "fleet", TaskCounts{Todo: 1})
	m := New("test")
	m.dashboard = scanDashboard(time.Now())
	m.records = []*agent.Record{
		{ID: "aaaa1111", Project: "fleet", SpawnedAt: time.Now()},
		{ID: "bbbb2222", Project: "rainier", SpawnedAt: time.Now()},
		{ID: "cccc3333", Project: "spark", SpawnedAt: time.Now()},
		// Duplicate project tag dedupes.
		{ID: "dddd4444", Project: "rainier", SpawnedAt: time.Now()},
		// Empty project skipped.
		{ID: "eeee5555", Project: "", SpawnedAt: time.Now()},
	}
	got := m.unifiedProjects()
	names := make([]string, 0, len(got))
	for _, p := range got {
		names = append(names, p.Name)
	}
	// Expected order: real (v0.2) first → "fleet", then synthetic
	// alpha-sorted → "rainier", "spark".
	want := []string{"fleet", "rainier", "spark"}
	if len(names) != len(want) {
		t.Fatalf("unifiedProjects names = %v, want %v", names, want)
	}
	for i, n := range names {
		if n != want[i] {
			t.Errorf("unifiedProjects[%d] = %q, want %q", i, n, want[i])
		}
	}
	// Synthetic rows must carry RepoSlug == Name as a sane fallback.
	for _, p := range got[1:] {
		if p.RepoSlug != p.Name {
			t.Errorf("synthetic project %q RepoSlug = %q, want %q",
				p.Name, p.RepoSlug, p.Name)
		}
	}
}

// TestUnifiedProjects_RealProjectTakesPrecedenceOverAgentTag pins the
// dedup rule: when an agent's Project tag matches a real v0.2-init'd
// project, the real ProjectRow is preserved (with its tasks/counts/
// coord status) — we don't overwrite it with a synthetic.
func TestUnifiedProjects_RealProjectTakesPrecedenceOverAgentTag(t *testing.T) {
	pdir := withFleetHome(t)
	seedTasks(t, pdir, "fleet", TaskCounts{Todo: 3, InProgress: 1, Done: 5})
	m := New("test")
	m.dashboard = scanDashboard(time.Now())
	m.records = []*agent.Record{
		{ID: "aaaa1111", Project: "fleet", SpawnedAt: time.Now()},
	}
	got := m.unifiedProjects()
	if len(got) != 1 {
		t.Fatalf("expected 1 project (deduped), got %d", len(got))
	}
	if got[0].Counts.Todo != 3 || got[0].Counts.InProgress != 1 || got[0].Counts.Done != 5 {
		t.Errorf("real project's counts must survive dedup: got %+v", got[0].Counts)
	}
}

// TestColumnHeading_ProjectsCountUsesUnion pins the rendered header
// reflects the unioned count, not just v0.2 dirs (issue #55).
func TestColumnHeading_ProjectsCountUsesUnion(t *testing.T) {
	pdir := withFleetHome(t)
	seedTasks(t, pdir, "fleet", TaskCounts{Todo: 1})
	m := New("test")
	m.width = 140
	m.height = 30
	m.dashboard = scanDashboard(time.Now())
	m.records = []*agent.Record{
		{ID: "aaaa1111", Project: "rainier", SpawnedAt: time.Now()},
		{ID: "bbbb2222", Project: "spark", SpawnedAt: time.Now()},
		{ID: "cccc3333", Project: "tatoosh", SpawnedAt: time.Now()},
	}
	out := m.View()
	if !strings.Contains(out, "PROJECTS · 4 ACTIVE") {
		t.Errorf("PROJECTS heading should show 4 (1 real + 3 synthetic), got:\n%s", out)
	}
	if !strings.Contains(out, "4 projects") {
		t.Errorf("header totalizer should show 4 projects, got:\n%s", out)
	}
}

// TestDashboardRows_RendersSyntheticProjectFromAgent pins that a
// synthetic ProjectRow appears in the row list (so the cursor walks
// it) when only agents pin the project tag.
func TestDashboardRows_RendersSyntheticProjectFromAgent(t *testing.T) {
	withFleetHome(t)
	m := New("test")
	m.dashboard = scanDashboard(time.Now())
	m.records = []*agent.Record{
		{ID: "aaaa1111", Project: "rainier", SpawnedAt: time.Now()},
	}
	rows := m.dashboardRows()
	var foundProject bool
	for _, r := range rows {
		if r.kind == rowProject && r.project != nil && r.project.Name == "rainier" {
			foundProject = true
			break
		}
	}
	if !foundProject {
		t.Errorf("expected a rowProject for 'rainier' synthesized from agent record")
	}
}

// TestDashboardRows_CoordAttachedToProjectFiltersFromRight pins that
// when a coord is identified for a project, the matching agent record
// is filtered out of the right-column agents section so it doesn't
// double-render. The coord-on-LEFT visual is owned by the project
// block; the right column lists only loose / non-coord agents.
func TestDashboardRows_CoordAttachedToProjectFiltersFromRight(t *testing.T) {
	withFleetHome(t)
	m := New("test")
	// Build a project with CoordID attached directly (avoids needing
	// to seed coord-state.json + lock body in this row-level test).
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{
			{Name: "fleet", RepoSlug: "fleet", CoordID: "aaaa1111", Active: true},
		},
		LoadedAt: time.Now(),
	}
	m.records = []*agent.Record{
		{ID: "aaaa1111", Project: "fleet", SpawnedAt: time.Now()}, // coord
		{ID: "bbbb2222", Project: "fleet", SpawnedAt: time.Now()}, // worker
	}
	rows := m.dashboardRows()
	var agentIDs []string
	for _, r := range rows {
		if r.kind == rowAgent && r.agent != nil {
			agentIDs = append(agentIDs, r.agent.ID)
		}
	}
	if len(agentIDs) != 1 || agentIDs[0] != "bbbb2222" {
		t.Errorf("right column should list only non-coord agents (bbbb2222), got %v", agentIDs)
	}
}

// TestColumnHeading_AgentCountExcludesCoord pins the AGENTS sub-heading
// counter: the coord doesn't count toward right-column "AGENTS N"
// because it renders on the LEFT under its project (issue #55).
func TestColumnHeading_AgentCountExcludesCoord(t *testing.T) {
	withFleetHome(t)
	m := New("test")
	m.width = 140
	m.height = 30
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{
			{Name: "fleet", RepoSlug: "fleet", CoordID: "aaaa1111", Active: true},
		},
		LoadedAt: time.Now(),
	}
	m.records = []*agent.Record{
		{ID: "aaaa1111", Project: "fleet", SpawnedAt: time.Now()}, // coord
		{ID: "bbbb2222", Project: "fleet", SpawnedAt: time.Now()}, // worker
		{ID: "cccc3333", Project: "fleet", SpawnedAt: time.Now()}, // worker
	}
	// Stub aliveByID so deriveStatus doesn't read all as dead.
	m.aliveByID = map[string]bool{
		"aaaa1111": true, "bbbb2222": true, "cccc3333": true,
	}
	out := m.View()
	// 2 non-coord agents alive → "AGENTS 2".
	if !strings.Contains(out, "AGENTS 2") {
		t.Errorf("coord must be excluded from right-column AGENTS count; expected 'AGENTS 2', got:\n%s", out)
	}
}

// TestView_CoordRendersOnLeftUnderProject pins the LEFT-column coord
// indicator (issue #55): when ProjectRow.CoordID is set, the rendered
// project block carries a "coord <id>" line.
func TestView_CoordRendersOnLeftUnderProject(t *testing.T) {
	withFleetHome(t)
	m := New("test")
	m.width = 140
	m.height = 30
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{
			{Name: "fleet", RepoSlug: "fleet", CoordID: "abcd1234", Active: true},
		},
		LoadedAt: time.Now(),
	}
	out := m.View()
	if !strings.Contains(out, "coord ") {
		t.Errorf("coord label missing from project block, got:\n%s", out)
	}
	if !strings.Contains(out, "abcd1234") {
		t.Errorf("coord ID missing from project block, got:\n%s", out)
	}
}

// TestView_NoCoordLineWhenCoordIDEmpty pins that an unset CoordID
// keeps the project block at its original 3-line shape — no empty
// "coord " line.
func TestView_NoCoordLineWhenCoordIDEmpty(t *testing.T) {
	withFleetHome(t)
	m := New("test")
	m.width = 140
	m.height = 30
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{
			{Name: "fleet", RepoSlug: "fleet"}, // CoordID empty
		},
		LoadedAt: time.Now(),
	}
	out := m.View()
	if strings.Contains(out, "coord ") {
		t.Errorf("coord label should be absent when CoordID empty, got:\n%s", out)
	}
}

// TestDashboard_CoordSignal_LockBodyPrimaryWins pins issue #63's
// precedence rule: when scanProject's lock-body branch sets CoordID
// (freshness gate satisfied), unifiedProjects must NOT overwrite it
// with a different agent that happens to carry coord-<project> task_id.
// Lock body wins.
func TestDashboard_CoordSignal_LockBodyPrimaryWins(t *testing.T) {
	(&stubSessionAlive{}).install(t)
	(&stubProjectTreeExists{}).install(t)

	m := New("test")
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{
			{Name: "demo", RepoSlug: "demo", CoordID: "aaaa1111", Active: true},
		},
		LoadedAt: time.Now(),
	}
	// A second record carrying coord-demo task_id — could happen on a
	// stale spawn whose lock body never published; the freshness gate
	// already kept it out of CoordID. Fallback must NOT promote it.
	m.records = []*agent.Record{
		{ID: "aaaa1111", Project: "demo", TaskID: "coord-demo", TmuxSession: "fleet-aaaa1111", SpawnedAt: time.Now()},
		{ID: "bbbb2222", Project: "demo", TaskID: "coord-demo", TmuxSession: "fleet-bbbb2222", SpawnedAt: time.Now()},
	}
	got := m.unifiedProjects()
	if len(got) != 1 || got[0].CoordID != "aaaa1111" {
		t.Errorf("lock body must win; got CoordID=%q (want aaaa1111)",
			coordIDOrEmpty(got))
	}
}

// TestDashboard_CoordSignal_TaskIDFallbackWhenNoLock pins the fallback:
// when CoordID is empty (lock body not yet published — boot window),
// an alive agent record tagged coord-<project> + project=<project>
// fills CoordID so the dashboard renders the coord under its project
// on LEFT immediately.
func TestDashboard_CoordSignal_TaskIDFallbackWhenNoLock(t *testing.T) {
	(&stubSessionAlive{}).install(t)
	(&stubProjectTreeExists{}).install(t)
	(&stubCoordSpawnMarker{markers: map[string]string{"demo": "c00bf001"}}).install(t)

	m := New("test")
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{
			{Name: "demo", RepoSlug: "demo"}, // CoordID empty
		},
		LoadedAt: time.Now(),
	}
	m.records = []*agent.Record{
		{ID: "c00bf001", Project: "demo", TaskID: "coord-demo", TmuxSession: "fleet-c00bf001", SpawnedAt: time.Now()},
	}
	got := m.unifiedProjects()
	if len(got) != 1 || got[0].CoordID != "c00bf001" {
		t.Errorf("task_id fallback must fill CoordID when lock body absent; got %q (want c00bf001)",
			coordIDOrEmpty(got))
	}
}

// TestDashboard_CoordSignal_NoMatchNoCoord pins the negative path: no
// lock body, no matching record → CoordID stays empty and the project
// block renders without a coord line.
func TestDashboard_CoordSignal_NoMatchNoCoord(t *testing.T) {
	(&stubSessionAlive{}).install(t)
	(&stubProjectTreeExists{}).install(t)

	m := New("test")
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{{Name: "demo", RepoSlug: "demo"}},
		LoadedAt: time.Now(),
	}
	m.records = []*agent.Record{
		// Record exists but task_id is wrong (regular worker).
		{ID: "11111111", Project: "demo", TaskID: "regular-task", TmuxSession: "fleet-11111111", SpawnedAt: time.Now()},
	}
	got := m.unifiedProjects()
	if len(got) != 1 || got[0].CoordID != "" {
		t.Errorf("CoordID must remain empty when no match; got %q",
			coordIDOrEmpty(got))
	}
}

// TestDashboard_CoordSignal_DeadSessionNotPromoted guards the fallback:
// a record with the right task_id but a dead tmux session must NOT
// promote to CoordID — that would render a ghost on LEFT and double
// up on RIGHT (filtered then re-listed when the session check fails
// downstream).
func TestDashboard_CoordSignal_DeadSessionNotPromoted(t *testing.T) {
	(&stubSessionAlive{dead: map[string]bool{"fleet-deadc0de": true}}).install(t)
	(&stubSessionProbe{dead: map[string]bool{"fleet-deadc0de": true}}).install(t)
	(&stubProjectTreeExists{}).install(t)
	(&stubCoordSpawnMarker{markers: map[string]string{"demo": "deadc0de"}}).install(t)

	m := New("test")
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{{Name: "demo", RepoSlug: "demo"}},
		LoadedAt: time.Now(),
	}
	m.records = []*agent.Record{
		{ID: "deadc0de", Project: "demo", TaskID: "coord-demo", TmuxSession: "fleet-deadc0de", SpawnedAt: time.Now()},
	}
	got := m.unifiedProjects()
	if len(got) != 1 || got[0].CoordID != "" {
		t.Errorf("dead session must not promote to CoordID; got %q",
			coordIDOrEmpty(got))
	}
}

// TestDashboard_FiltersClaimedCoordFromRight pins the filter
// invariant: once unifiedProjects fills CoordID via the task_id
// fallback, dashboardRows must NOT also list the same agent in the
// right-column agents section. The coord renders on LEFT only.
func TestDashboard_FiltersClaimedCoordFromRight(t *testing.T) {
	(&stubSessionAlive{}).install(t)
	(&stubProjectTreeExists{}).install(t)
	(&stubCoordSpawnMarker{markers: map[string]string{"demo": "c00bf001"}}).install(t)

	m := New("test")
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{{Name: "demo", RepoSlug: "demo"}},
		LoadedAt: time.Now(),
	}
	m.records = []*agent.Record{
		{ID: "c00bf001", Project: "demo", TaskID: "coord-demo", TmuxSession: "fleet-c00bf001", SpawnedAt: time.Now()},
		{ID: "bbbb2222", Project: "demo", TaskID: "regular-task", TmuxSession: "fleet-bbbb2222", SpawnedAt: time.Now()},
	}
	rows := m.dashboardRows()
	var agentIDs []string
	for _, r := range rows {
		if r.kind == rowAgent && r.agent != nil {
			agentIDs = append(agentIDs, r.agent.ID)
		}
	}
	if len(agentIDs) != 1 || agentIDs[0] != "bbbb2222" {
		t.Errorf("coord must be filtered from RIGHT (claimed by LEFT); got %v want [bbbb2222]", agentIDs)
	}
}

// TestDashboard_CoordSignal_TaskIDFallbackGatedOnMarker pins codex
// iter-3 P2: a record matching task_id + project + alive session
// must ALSO match the coord-spawn marker file's content before
// promotion. Without this, an operator running `fleet dispatch
// coord-X --project X --coord-spawn` could write a record that
// hijacks the LEFT-column slot. The TUI writes the marker post-
// dispatch with the agent ID it's about to attach to; a spoof
// dispatch can't replicate that without ALSO editing the marker file
// directly (which is editing internal state — outside the trust
// boundary regardless).
func TestDashboard_CoordSignal_TaskIDFallbackGatedOnMarker(t *testing.T) {
	(&stubSessionAlive{}).install(t)
	(&stubProjectTreeExists{}).install(t)
	// Marker file is missing for "demo" — no promotion.
	(&stubCoordSpawnMarker{markers: map[string]string{}}).install(t)

	m := New("test")
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{{Name: "demo", RepoSlug: "demo"}},
		LoadedAt: time.Now(),
	}
	m.records = []*agent.Record{
		{ID: "spoofyid", Project: "demo", TaskID: "coord-demo", TmuxSession: "fleet-spoofyid", SpawnedAt: time.Now()},
	}
	got := m.unifiedProjects()
	if len(got) != 1 || got[0].CoordID != "" {
		t.Errorf("missing marker must block promotion; got CoordID=%q", coordIDOrEmpty(got))
	}
}

// TestDashboard_CoordSignal_TaskIDFallback_MarkerMismatchSkipped
// guards against a stale marker matching a different agent ID:
// e.g. previous coord crashed, marker still names the dead one,
// and a new (genuinely-spoofed) record has a different ID. The
// fallback must NOT promote the spoofer just because both share
// the task_id sentinel.
func TestDashboard_CoordSignal_TaskIDFallback_MarkerMismatchSkipped(t *testing.T) {
	(&stubSessionAlive{}).install(t)
	(&stubProjectTreeExists{}).install(t)
	// Marker names a different agent than the record below.
	(&stubCoordSpawnMarker{markers: map[string]string{"demo": "realone1"}}).install(t)

	m := New("test")
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{{Name: "demo", RepoSlug: "demo"}},
		LoadedAt: time.Now(),
	}
	m.records = []*agent.Record{
		{ID: "spoofyid", Project: "demo", TaskID: "coord-demo", TmuxSession: "fleet-spoofyid", SpawnedAt: time.Now()},
	}
	got := m.unifiedProjects()
	if len(got) != 1 || got[0].CoordID != "" {
		t.Errorf("marker mismatch must block promotion; got CoordID=%q", coordIDOrEmpty(got))
	}
}

// TestDashboard_CoordSignal_TaskIDFallback_TmuxProbeErrorNotDead
// pins codex iter-3 P2 #2: tmux probe transport errors (bad socket,
// restarting server) must NOT be conflated with "definitively dead".
// The tristate sessionProbeFn distinguishes them; on probe-error,
// findCoordByTaskID's re-check (sessionProbeOrAliveFn) treats the
// session as alive and the coord stays bound on LEFT.
func TestDashboard_CoordSignal_TaskIDFallback_TmuxProbeErrorNotDead(t *testing.T) {
	// First-pass alive returns false (HasSession says no), but the
	// re-probe via SessionAlive returns (false, transport-err) →
	// sessionProbeOrAliveFn returns true → coord stays bound.
	(&stubSessionAlive{dead: map[string]bool{"fleet-realcoord": true}}).install(t)
	(&stubSessionProbe{errSessions: map[string]bool{"fleet-realcoord": true}}).install(t)
	(&stubProjectTreeExists{}).install(t)
	(&stubCoordSpawnMarker{markers: map[string]string{"demo": "realcoord"}}).install(t)

	m := New("test")
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{{Name: "demo", RepoSlug: "demo"}},
		LoadedAt: time.Now(),
	}
	m.records = []*agent.Record{
		{ID: "realcoord", Project: "demo", TaskID: "coord-demo", TmuxSession: "fleet-realcoord", SpawnedAt: time.Now()},
	}
	got := m.unifiedProjects()
	if len(got) != 1 || got[0].CoordID != "realcoord" {
		t.Errorf("tmux probe error must be conservative-alive (don't drop coord); got CoordID=%q",
			coordIDOrEmpty(got))
	}
}

// TestDashboard_CoordSignal_TaskIDFallback_StaleMarkerSkipped pins
// codex iter-4 P1: when the marker is older than coordBootWindow,
// the fallback must NOT promote — even with a matching task_id +
// project + alive session. Beyond the boot window, the only
// authoritative coord identity signal is the lock-body branch
// (gated by coord-state.json freshness). Without this gate, a
// coord whose claude process exited but whose tmux shell wrapper
// is still alive would be re-promoted forever from the marker.
func TestDashboard_CoordSignal_TaskIDFallback_StaleMarkerSkipped(t *testing.T) {
	(&stubSessionAlive{}).install(t)
	(&stubProjectTreeExists{}).install(t)
	(&stubCoordSpawnMarkerStale{markers: map[string]string{"demo": "stalecrd"}}).install(t)

	m := New("test")
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{{Name: "demo", RepoSlug: "demo"}},
		LoadedAt: time.Now(),
	}
	m.records = []*agent.Record{
		{ID: "stalecrd", Project: "demo", TaskID: "coord-demo", TmuxSession: "fleet-stalecrd", SpawnedAt: time.Now()},
	}
	got := m.unifiedProjects()
	if len(got) != 1 || got[0].CoordID != "" {
		t.Errorf("stale marker (>%s old) must block promotion; got CoordID=%q",
			coordBootWindow, coordIDOrEmpty(got))
	}
	// Stale-marker coord must STAY on RIGHT for [a]/[x] triage.
	rows := m.dashboardRows()
	var seenRight bool
	for _, r := range rows {
		if r.kind == rowAgent && r.agent != nil && r.agent.ID == "stalecrd" {
			seenRight = true
			break
		}
	}
	if !seenRight {
		t.Error("stale-marker record should still appear in RIGHT column for triage")
	}
}

// TestDashboard_CoordSignal_TaskIDFallbackGatedOnProjectTree pins
// codex iter-2 P2: a record with the right task_id but NO
// ~/.fleet/projects/<name>/ tree on disk (legacy / hand-edited /
// pre-issue-#63 build) must NOT auto-promote to coord. The TUI's
// post-issue-#63 auto-spawn always runs state.EnsureProjectInitialized
// before dispatch, so post-PR records satisfy the gate; older records
// stay on the RIGHT column where the operator can triage them.
func TestDashboard_CoordSignal_TaskIDFallbackGatedOnProjectTree(t *testing.T) {
	(&stubSessionAlive{}).install(t)
	// Project tree missing → fallback must NOT promote even with a
	// matching task_id + project tag + alive session.
	(&stubProjectTreeExists{missing: map[string]bool{"demo": true}}).install(t)

	m := New("test")
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{{Name: "demo", RepoSlug: "demo"}},
		LoadedAt: time.Now(),
	}
	m.records = []*agent.Record{
		{ID: "legacy01", Project: "demo", TaskID: "coord-demo", TmuxSession: "fleet-legacy01", SpawnedAt: time.Now()},
	}
	got := m.unifiedProjects()
	if len(got) != 1 || got[0].CoordID != "" {
		t.Errorf("legacy record without project tree must not promote to coord; got CoordID=%q",
			coordIDOrEmpty(got))
	}
	// And the legacy agent must STAY on the RIGHT column (not filtered).
	rows := m.dashboardRows()
	var seenRight bool
	for _, r := range rows {
		if r.kind == rowAgent && r.agent != nil && r.agent.ID == "legacy01" {
			seenRight = true
			break
		}
	}
	if !seenRight {
		t.Error("legacy coord-* record should still appear in RIGHT column for triage")
	}
}

// coordIDOrEmpty extracts the first project's CoordID for error
// messages. Returns "" when slice is empty.
func coordIDOrEmpty(ps []*ProjectRow) string {
	if len(ps) == 0 || ps[0] == nil {
		return ""
	}
	return ps[0].CoordID
}

func TestFormatUptime(t *testing.T) {
	cases := map[time.Duration]string{
		0:                            "00:00",
		1 * time.Minute:              "00:01",
		1 * time.Hour:                "01:00",
		2*time.Hour + 35*time.Minute: "02:35",
		100 * time.Hour:              "99:59",
		-1 * time.Second:             "00:00",
	}
	for d, want := range cases {
		if got := formatUptime(d); got != want {
			t.Errorf("formatUptime(%v) = %q, want %q", d, got, want)
		}
	}
}

// TestScanWorkers_DeletesDoneWorkerDir — issue #101 lifecycle hygiene
// defense-in-depth. A worker dir whose state.json shows phase=done
// is rm-rf'd during the scan AND omitted from the returned rows so
// the dashboard renders the snapshot it would see on the next tick.
func TestScanWorkers_DeletesDoneWorkerDir(t *testing.T) {
	pdir := withFleetHome(t)
	exit := 0
	seedWorker(t, pdir, "fleet", "done-1a2b", workers.State{
		Phase: workers.PhaseDone,
		PRURL: "https://example.invalid/pr/1",
		Exit:  &exit,
	})
	rows := scanWorkers(filepath.Join(pdir, "fleet"), "fleet", time.Now(), nil)
	for _, r := range rows {
		if r.Slug == "done-1a2b" {
			t.Fatalf("done worker should be filtered from rows; got %+v", r)
		}
	}
	if _, err := os.Stat(filepath.Join(pdir, "fleet", "workers", "done-1a2b")); !os.IsNotExist(err) {
		t.Fatalf("done worker dir should be removed; stat err=%v", err)
	}
}

// TestScanWorkers_DeletesFailedWorkerDir mirrors the done case for
// the TerminalFailure path.
func TestScanWorkers_DeletesFailedWorkerDir(t *testing.T) {
	pdir := withFleetHome(t)
	exit := 1
	seedWorker(t, pdir, "fleet", "failed-cd34", workers.State{
		Phase:         workers.PhaseFailed,
		BlockedReason: "exit 1",
		Exit:          &exit,
	})
	rows := scanWorkers(filepath.Join(pdir, "fleet"), "fleet", time.Now(), nil)
	for _, r := range rows {
		if r.Slug == "failed-cd34" {
			t.Fatalf("failed worker should be filtered from rows; got %+v", r)
		}
	}
	if _, err := os.Stat(filepath.Join(pdir, "fleet", "workers", "failed-cd34")); !os.IsNotExist(err) {
		t.Fatalf("failed worker dir should be removed; stat err=%v", err)
	}
}

// TestScanWorkers_KeepsBlockedWorkerDir — Blocked is Waiting in the
// lifecycle classification. Dir must survive AND row must render so
// the operator sees the blocked signal in the right column.
func TestScanWorkers_KeepsBlockedWorkerDir(t *testing.T) {
	pdir := withFleetHome(t)
	seedWorker(t, pdir, "fleet", "blocked-1a2b", workers.State{
		Phase:         workers.PhaseBlocked,
		BlockedReason: "needs operator clarification",
	})
	rows := scanWorkers(filepath.Join(pdir, "fleet"), "fleet", time.Now(), nil)
	found := false
	for _, r := range rows {
		if r.Slug == "blocked-1a2b" {
			found = true
		}
	}
	if !found {
		t.Fatal("blocked worker should appear in rows")
	}
	if _, err := os.Stat(filepath.Join(pdir, "fleet", "workers", "blocked-1a2b")); err != nil {
		t.Fatalf("blocked worker dir should survive: %v", err)
	}
}

// TestScanWorkers_KeepsActiveWorkerDir — running workers are obviously
// preserved.
func TestScanWorkers_KeepsActiveWorkerDir(t *testing.T) {
	pdir := withFleetHome(t)
	seedWorker(t, pdir, "fleet", "active-1a2b", workers.State{
		Phase: workers.PhaseTDDRed,
	})
	rows := scanWorkers(filepath.Join(pdir, "fleet"), "fleet", time.Now(), nil)
	found := false
	for _, r := range rows {
		if r.Slug == "active-1a2b" {
			found = true
		}
	}
	if !found {
		t.Fatal("active worker should appear in rows")
	}
	if _, err := os.Stat(filepath.Join(pdir, "fleet", "workers", "active-1a2b")); err != nil {
		t.Fatalf("active worker dir should survive: %v", err)
	}
}

// TestScanWorkers_SkipsDeletingStagingDir — workers.Delete renames
// to <slug>.deleting-<stamp>/ before RemoveAll. A staging dir caught
// mid-rm must not surface as a worker row.
func TestScanWorkers_SkipsDeletingStagingDir(t *testing.T) {
	pdir := withFleetHome(t)
	staging := filepath.Join(pdir, "fleet", "workers", "tgt-1a2b.deleting-20260509-100000")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatalf("mkdir staging: %v", err)
	}
	if err := os.WriteFile(filepath.Join(staging, "state.json"), []byte(`{"slug":"tgt-1a2b","phase":"done","pr_url":"https://x"}`), 0o644); err != nil {
		t.Fatalf("write state.json in staging: %v", err)
	}
	rows := scanWorkers(filepath.Join(pdir, "fleet"), "fleet", time.Now(), nil)
	for _, r := range rows {
		if strings.Contains(r.Slug, "deleting") {
			t.Fatalf("staging dir should not surface; got row %+v", r)
		}
	}
}

// TestIsDeletingStagingName — issue #101 regression. Anchor the
// staging-dir filter on the exact shape `<base>.deleting-<UTCstamp>`
// so a legitimate slug whose text contains the substring `.deleting-`
// still renders normally. ValidateSlug allows periods + hyphens, so
// `phase.deleting-stage-1a2b` is a valid slug today.
func TestIsDeletingStagingName(t *testing.T) {
	t.Run("matches canonical", func(t *testing.T) {
		if !isDeletingStagingName("foo-1a2b.deleting-20260509-100000") {
			t.Fatal("canonical staging name should match")
		}
	})
	t.Run("matches collision suffix", func(t *testing.T) {
		if !isDeletingStagingName("foo-1a2b.deleting-20260509-100000-1") {
			t.Fatal("collision-suffixed staging name should match")
		}
	})
	t.Run("rejects bare substring", func(t *testing.T) {
		// Legitimate slug; not a staging dir.
		if isDeletingStagingName("phase.deleting-stage-1a2b") {
			t.Fatal("legitimate slug containing `.deleting-` should NOT match")
		}
	})
	t.Run("rejects non-stamp tail", func(t *testing.T) {
		if isDeletingStagingName("foo.deleting-not-a-stamp") {
			t.Fatal("non-stamp tail should NOT match")
		}
	})
	t.Run("rejects missing marker", func(t *testing.T) {
		if isDeletingStagingName("foo-1a2b") {
			t.Fatal("regular slug without marker should NOT match")
		}
	})
}

// TestScanWorkers_DoesNotMisclassifyLegitimateSlug — defense-in-depth
// regression. A worker slug whose text contains `.deleting-` (allowed
// by state.ValidateSlug) must still render normally — not be silently
// dropped by the staging-dir filter.
func TestScanWorkers_DoesNotMisclassifyLegitimateSlug(t *testing.T) {
	pdir := withFleetHome(t)
	// `phase.deleting-stage-1a2b` is a legitimate slug per
	// state.ValidateSlug (lowercase + digits + . / - / _ allowed).
	seedWorker(t, pdir, "fleet", "phase.deleting-stage-1a2b", workers.State{
		Phase: workers.PhaseTDDRed,
	})
	rows := scanWorkers(filepath.Join(pdir, "fleet"), "fleet", time.Now(), nil)
	found := false
	for _, r := range rows {
		if r.Slug == "phase.deleting-stage-1a2b" {
			found = true
		}
	}
	if !found {
		t.Fatal("legitimate slug containing `.deleting-` substring should render normally")
	}
}

// ---------- Phase C — subagent_id rendering (issue #94) ----------

// writeCoordStateWithSubagentMap drops a coord-state.json at the
// expected path with `worker_subagent_ids` populated. mtime is fresh
// (now) so scanProject reads it as Active and the subagent map flows
// through to the row scan. raw=true bypasses the JSON encoder for the
// "malformed file" test.
func writeCoordStateWithSubagentMap(t *testing.T, projectsRoot, project string, m map[string]string, raw bool, body string) {
	t.Helper()
	dir := filepath.Join(projectsRoot, project)
	if err := os.MkdirAll(filepath.Join(dir, ".locks"), 0o755); err != nil {
		t.Fatalf("mkdir locks: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".locks", "coordinator.lock"), nil, 0o644); err != nil {
		t.Fatalf("write coord lock: %v", err)
	}
	path := filepath.Join(dir, "coord-state.json")
	if raw {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write raw coord-state: %v", err)
		}
		return
	}
	payload := map[string]any{}
	if len(m) > 0 {
		payload["worker_subagent_ids"] = m
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		t.Fatalf("marshal coord-state: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write coord-state: %v", err)
	}
}

func TestReadWorkerSubagentMap_ReadsValidEntries(t *testing.T) {
	pdir := withFleetHome(t)
	writeCoordStateWithSubagentMap(t, pdir, "fleet", map[string]string{
		"alpha-aaaa": "claude-sub-1",
		"beta-bbbb":  "claude-sub-2",
	}, false, "")
	got := readWorkerSubagentMap(filepath.Join(pdir, "fleet", "coord-state.json"))
	if got["alpha-aaaa"] != "claude-sub-1" || got["beta-bbbb"] != "claude-sub-2" {
		t.Fatalf("unexpected map: %v", got)
	}
}

func TestReadWorkerSubagentMap_ReturnsNilOnMissing(t *testing.T) {
	pdir := withFleetHome(t)
	if got := readWorkerSubagentMap(filepath.Join(pdir, "no-such-project", "coord-state.json")); got != nil {
		t.Fatalf("missing file should return nil, got %v", got)
	}
}

func TestReadWorkerSubagentMap_ReturnsNilOnMalformed(t *testing.T) {
	pdir := withFleetHome(t)
	writeCoordStateWithSubagentMap(t, pdir, "fleet", nil, true, "{ not valid json")
	got := readWorkerSubagentMap(filepath.Join(pdir, "fleet", "coord-state.json"))
	if got != nil {
		t.Fatalf("malformed JSON should return nil, got %v", got)
	}
}

func TestReadWorkerSubagentMap_ReturnsNilOnEmptyMap(t *testing.T) {
	pdir := withFleetHome(t)
	writeCoordStateWithSubagentMap(t, pdir, "fleet", nil, true, `{"worker_subagent_ids": {}}`)
	got := readWorkerSubagentMap(filepath.Join(pdir, "fleet", "coord-state.json"))
	if got != nil {
		t.Fatalf("empty map should return nil, got %v", got)
	}
}

func TestReadWorkerSubagentMap_FiltersWhitespaceValues(t *testing.T) {
	pdir := withFleetHome(t)
	writeCoordStateWithSubagentMap(t, pdir, "fleet", map[string]string{
		"good-aaaa":  "claude-sub-1",
		"empty-bbbb": "",
		"space-cccc": "   ",
	}, false, "")
	got := readWorkerSubagentMap(filepath.Join(pdir, "fleet", "coord-state.json"))
	if len(got) != 1 || got["good-aaaa"] != "claude-sub-1" {
		t.Fatalf("filter expected only 'good-aaaa' entry, got %v", got)
	}
}

func TestScanWorkers_AttachesSubagentID(t *testing.T) {
	pdir := withFleetHome(t)
	seedWorker(t, pdir, "fleet", "alpha-aaaa", workers.State{
		Phase: workers.PhaseTDDRed,
	})
	subMap := map[string]string{"alpha-aaaa": "claude-sub-1"}
	rows := scanWorkers(filepath.Join(pdir, "fleet"), "fleet", time.Now(), subMap)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0].SubagentID != "claude-sub-1" {
		t.Fatalf("SubagentID = %q, want %q", rows[0].SubagentID, "claude-sub-1")
	}
}

func TestScanWorkers_NoSubagentMapYieldsEmptyID(t *testing.T) {
	pdir := withFleetHome(t)
	seedWorker(t, pdir, "fleet", "alpha-aaaa", workers.State{
		Phase: workers.PhaseTDDRed,
	})
	rows := scanWorkers(filepath.Join(pdir, "fleet"), "fleet", time.Now(), nil)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0].SubagentID != "" {
		t.Fatalf("SubagentID should be empty without map, got %q", rows[0].SubagentID)
	}
}

func TestScanProject_PlumbsSubagentMap(t *testing.T) {
	// End-to-end: scanProject reads coord-state.json, scanWorkers
	// gets the map, the resulting WorkerRow carries the SubagentID.
	pdir := withFleetHome(t)
	seedWorker(t, pdir, "fleet", "alpha-aaaa", workers.State{
		Phase: workers.PhaseTDDRed,
	})
	writeCoordStateWithSubagentMap(t, pdir, "fleet", map[string]string{
		"alpha-aaaa": "claude-sub-1",
	}, false, "")
	_, wrows := scanProject(pdir, "fleet", time.Now())
	if len(wrows) != 1 {
		t.Fatalf("expected 1 worker row, got %d", len(wrows))
	}
	if wrows[0].SubagentID != "claude-sub-1" {
		t.Fatalf("scanProject failed to plumb SubagentID; got %q", wrows[0].SubagentID)
	}
}

func TestShortSubagentHash_Truncates(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"   ", ""},
		{"a", "a"},
		{"abcd", "abcd"},
		{"abcdefgh", "abcdefgh"},
		{"abcdefghi", "abcdefgh"},        // 9 → 8
		{"abcdefghijklmnop", "abcdefgh"}, // long → 8
		{"  abc12345  ", "abc12345"},     // trim then truncate
	}
	for _, c := range cases {
		if got := shortSubagentHash(c.in); got != c.want {
			t.Errorf("shortSubagentHash(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestShortSubagentHash_RuneSafeTruncation(t *testing.T) {
	// Multi-byte runes must not split a code point on truncation.
	in := "αβγδεζηθικ" // 10 runes, > 8
	got := shortSubagentHash(in)
	want := "αβγδεζηθ" // first 8 runes
	if got != want {
		t.Fatalf("rune truncation failed: got %q, want %q", got, want)
	}
}

func TestSnapshot_SubagentsRunning(t *testing.T) {
	cases := []struct {
		name string
		s    *Snapshot
		want int
	}{
		{"nil snapshot", nil, 0},
		{"no workers", &Snapshot{}, 0},
		{"all empty", &Snapshot{Workers: []*WorkerRow{
			{ID: "a"}, {ID: "b"},
		}}, 0},
		{"one populated", &Snapshot{Workers: []*WorkerRow{
			{ID: "a", SubagentID: "x"},
			{ID: "b"},
		}}, 1},
		{"multiple", &Snapshot{Workers: []*WorkerRow{
			{ID: "a", SubagentID: "x"},
			{ID: "b", SubagentID: "y"},
			{ID: "c", SubagentID: "z"},
		}}, 3},
		{"nil row tolerated", &Snapshot{Workers: []*WorkerRow{
			nil,
			{ID: "a", SubagentID: "x"},
		}}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.s.SubagentsRunning(); got != c.want {
				t.Fatalf("got %d, want %d", got, c.want)
			}
		})
	}
}
