package tui

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

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
	for _, want := range []string{"⏳ 3", "▶ 1", "👁 2", "✓ 7"} {
		if !strings.Contains(out, want) {
			t.Errorf("dashboard should include count chip %q, got:\n%s", want, out)
		}
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

func TestKeyG_SwitchesToAgentsView(t *testing.T) {
	m := New("test")
	if m.view != viewDashboard {
		t.Fatalf("default view should be dashboard, got %v", m.view)
	}

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("g")})
	if updated.(Model).view != viewAgents {
		t.Errorf("after [g] view should be agents, got %v", updated.(Model).view)
	}

	// Pressing g again toggles back.
	m2 := updated.(Model)
	updated2, _ := m2.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("g")})
	if updated2.(Model).view != viewDashboard {
		t.Errorf("second [g] should toggle back to dashboard, got %v", updated2.(Model).view)
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

func TestScanDashboard_AttentionProjectsSortedFirst(t *testing.T) {
	pdir := withFleetHome(t)
	seedTasks(t, pdir, "alpha", TaskCounts{Todo: 1})
	seedTasks(t, pdir, "zulu", TaskCounts{Todo: 1, Blocked: 1})

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

func TestDeriveRepoSlug_FromGithubURL(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "standards.md"), []byte(
		"---\nschema: v1\n---\n\n# Standards\n\n## Repo\n\nSee https://github.com/edisonshen/fleet for source.\n",
	), 0o644); err != nil {
		t.Fatal(err)
	}
	got := deriveRepoSlug(tmp, "fleet")
	if got != "edisonshen/fleet" {
		t.Errorf("deriveRepoSlug from github URL: got %q want edisonshen/fleet", got)
	}
}

func TestDeriveRepoSlug_FromBullet(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "standards.md"), []byte(
		"---\nschema: v1\n---\n\n## Repo\n\n- repo: edisonshen/gstack\n",
	), 0o644); err != nil {
		t.Fatal(err)
	}
	got := deriveRepoSlug(tmp, "gstack")
	if got != "edisonshen/gstack" {
		t.Errorf("deriveRepoSlug from `- repo:` bullet: got %q", got)
	}
}

func TestDeriveRepoSlug_FallbackToName(t *testing.T) {
	got := deriveRepoSlug(t.TempDir(), "side-experiment")
	if got != "side-experiment" {
		t.Errorf("missing standards.md should fall back to name, got %q", got)
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
// renders a coachmark instead of a blank column.
func TestView_DashboardEmptyShowsHint(t *testing.T) {
	withFleetHome(t)
	m := New("test")
	m.width = 120
	m.height = 30
	m.dashboard = scanDashboard(time.Now())
	out := m.View()
	if !strings.Contains(out, "no projects yet") {
		t.Errorf("empty dashboard should hint at fleet tasks add, got:\n%s", out)
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

// TestView_FooterShowsKeybinds pins the footer renders the mockup's
// chip strip + uptime indicator.
func TestView_FooterShowsKeybinds(t *testing.T) {
	withFleetHome(t)
	m := New("test")
	m.width = 130
	m.height = 30
	m.dashboard = scanDashboard(time.Now())
	out := m.View()
	for _, want := range []string{"j/k", "nav", "uptime"} {
		if !strings.Contains(out, want) {
			t.Errorf("footer should include %q, got:\n%s", want, out)
		}
	}
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
