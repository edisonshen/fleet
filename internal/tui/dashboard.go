// Dashboard data layer — assembles ProjectRow + WorkerRow snapshots
// from ~/.fleet/projects/*/ for the v0.2 "Ops Console" view.
//
// Pure I/O on disk + parse via the existing internal/tasks +
// internal/workers helpers. No mutation. The TUI calls
// loadDashboardCmd once per tick / fsEvent and reads the resulting
// snapshot into Model.dashboard.
//
// Coord status is inferred without disturbing the flock on
// coordinator.lock — we stat the file's mtime and treat anything
// within coordActiveWindow as "active". The actual flock is owned by
// the running coord; stat-ing reports filesystem mtime which the coord
// touches every tick (loop.py opens/writes coord-state.json after the
// flock).
package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tasks"
	"github.com/edisonshen/fleet/internal/workers"
)

// coordActiveWindow is how recent coordinator.lock's mtime must be for
// the dashboard to render the project as "● active". Beyond this, the
// row reads "○ idle". Coord ticks ~once per minute under load, so 5
// minutes is a generous floor that survives a stalled tick without
// flipping idle on every quiet stretch.
const coordActiveWindow = 5 * time.Minute

// workerStaleWindow is how long since the worker's state.json
// updated_at before we treat it as idle/heartbeat-stale (amber dot).
// Workers update on every phase transition; 10 minutes between
// transitions is the threshold where the operator should look.
const workerStaleWindow = 10 * time.Minute

// ProjectRow is one row in the left column.
type ProjectRow struct {
	Name      string // bare project name (the dir under ~/.fleet/projects/)
	RepoSlug  string // "edisonshen/<name>" style; falls back to Name
	Counts    TaskCounts
	Active    bool      // coord lock mtime within window
	IdleStop  bool      // file present but stale → "auto-stopped" pill
	LastTick  time.Time // coord-state.json mtime; zero if missing
	Attention int       // count of blocked workers + raise-hand items
	BlockedQ  string    // first blocked worker's reason (for raise-hand expansion P2)
	BlockedID string    // first blocked worker's ID
}

// TaskCounts mirrors the columns in the mockup:
// ⏳ todo  ▶ in-progress  👁 in-review  ⚠ blocked  ✓ done.
// "ready" rolls into todo (operator hasn't dispatched yet).
type TaskCounts struct {
	Todo       int
	InProgress int
	InReview   int
	Blocked    int
	Done       int
}

// WorkerRow is one row in the right column.
type WorkerRow struct {
	ID      string // 8-char short slug suffix, displayed in purple
	Project string
	Slug    string // full task slug (project:slug rendering)
	Phase   workers.Phase
	State   string // "ok" / "rv" / "rn" / "bl" / "!!"
	Color   string // "red" | "blue" | "green" | "amber" — driver of the dot
	Age     string // "2m" / "8m"
	Blocked bool
	Reason  string
}

// Snapshot is the dashboard's read-only render input.
type Snapshot struct {
	Projects []*ProjectRow
	Workers  []*WorkerRow
	CIRuns   int       // count of in-progress CI / PR check runs (best effort)
	LoadedAt time.Time // wall time the snapshot was assembled
	Err      error     // best-effort: collection errors don't block render
}

// dashboardMsg carries a refreshed Snapshot from the loader goroutine.
type dashboardMsg struct {
	snap *Snapshot
}

// loadDashboardCmd reads ~/.fleet/projects/*/ once and returns a
// dashboardMsg. Best-effort: per-project errors collapse the row to
// empty data rather than aborting the whole snapshot.
func loadDashboardCmd() tea.Cmd {
	return func() tea.Msg {
		snap := scanDashboard(time.Now())
		return dashboardMsg{snap: snap}
	}
}

// scanDashboard is the pure load body, factored for tests.
//
//	~/.fleet/projects/
//	  ├─ <name>/
//	  │   ├─ tasks.md           → counts
//	  │   ├─ standards.md       → repo slug fallback
//	  │   ├─ coord-state.json   → LastTick (mtime)
//	  │   ├─ .locks/coordinator.lock  → Active if mtime fresh
//	  │   └─ workers/<slug>/state.json → WorkerRow
//	  └─ .locks/                → reserved (skipped)
//
// now is injected so tests can assert age math deterministically.
func scanDashboard(now time.Time) *Snapshot {
	root, err := state.Root()
	if err != nil {
		return &Snapshot{Err: err, LoadedAt: now}
	}
	projDir := filepath.Join(root, "projects")
	entries, err := os.ReadDir(projDir)
	if err != nil {
		if os.IsNotExist(err) {
			return &Snapshot{LoadedAt: now}
		}
		return &Snapshot{Err: err, LoadedAt: now}
	}

	snap := &Snapshot{LoadedAt: now}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// Skip reserved directories. ".locks" sits alongside per-project
		// dirs (state.Bootstrap creates it) but isn't a project.
		name := e.Name()
		if name == ".locks" || strings.HasPrefix(name, ".") {
			continue
		}
		row, wrows := scanProject(projDir, name, now)
		if row != nil {
			snap.Projects = append(snap.Projects, row)
		}
		snap.Workers = append(snap.Workers, wrows...)
	}
	// Stable order: projects with attention first, then alpha. Workers
	// alpha by project then slug. Matches the mockup's "needs attention"
	// projects rising to the top.
	sort.SliceStable(snap.Projects, func(i, j int) bool {
		a, b := snap.Projects[i], snap.Projects[j]
		if (a.Attention > 0) != (b.Attention > 0) {
			return a.Attention > b.Attention
		}
		return a.Name < b.Name
	})
	sort.SliceStable(snap.Workers, func(i, j int) bool {
		a, b := snap.Workers[i], snap.Workers[j]
		if a.Project != b.Project {
			return a.Project < b.Project
		}
		return a.Slug < b.Slug
	})
	return snap
}

// scanProject walks one ~/.fleet/projects/<name>/ subtree and returns
// the assembled ProjectRow plus all worker rows under it. nil row +
// empty slice when the project dir is malformed (e.g. missing tasks.md
// AND no workers/ — nothing to show).
func scanProject(projectsRoot, name string, now time.Time) (*ProjectRow, []*WorkerRow) {
	dir := filepath.Join(projectsRoot, name)
	row := &ProjectRow{
		Name:     name,
		RepoSlug: deriveRepoSlug(dir, name),
	}

	// Task counts. Read errors collapse to zero counts — the row still
	// renders so the operator can see the project exists.
	if f, err := tasks.Read(filepath.Join(dir, "tasks.md")); err == nil {
		for _, t := range f.Tasks {
			switch t.Status {
			case tasks.StatusTodo, tasks.StatusReady:
				row.Counts.Todo++
			case tasks.StatusInProgress:
				row.Counts.InProgress++
			case tasks.StatusInReview:
				row.Counts.InReview++
			case tasks.StatusBlocked:
				row.Counts.Blocked++
			case tasks.StatusDone:
				row.Counts.Done++
			}
		}
	}

	// Coord active / idle. Lock file mtime is the heartbeat.
	if info, err := os.Stat(filepath.Join(dir, ".locks", "coordinator.lock")); err == nil {
		mt := info.ModTime()
		if now.Sub(mt) <= coordActiveWindow {
			row.Active = true
		} else {
			row.IdleStop = true
		}
	}

	// Last-tick age — coord-state.json's mtime, written every tick after
	// the flock is acquired (skills/coordinator/loop.py).
	if info, err := os.Stat(filepath.Join(dir, "coord-state.json")); err == nil {
		row.LastTick = info.ModTime()
	}

	// Workers under workers/<slug>/state.json.
	wrows := scanWorkers(dir, name, now)

	// Attention math: blocked workers OR blocked tasks. Coord raise-hand
	// inbox is read separately (P2); for v1 we surface the worker-side
	// blocked signal which IS the operator's job to answer.
	var firstBlocked *WorkerRow
	for _, w := range wrows {
		if w.Blocked {
			row.Attention++
			if firstBlocked == nil {
				firstBlocked = w
			}
		}
	}
	row.Attention += row.Counts.Blocked
	if firstBlocked != nil {
		row.BlockedID = firstBlocked.ID
		row.BlockedQ = firstBlocked.Reason
	}

	// CI running — best-effort. v0.2.0 doesn't cache pr_check yet, so we
	// approximate by counting workers in PhaseReviewClaude /
	// PhaseReviewCodex / PhasePush — those are the phases where the
	// worker is waiting on CI/review feedback.
	return row, wrows
}

// scanWorkers reads <project>/workers/<slug>/state.json for every
// active worker (skips archive/). Returns rows in undefined order; the
// caller sorts.
func scanWorkers(projectDir, project string, now time.Time) []*WorkerRow {
	wDir := filepath.Join(projectDir, "workers")
	entries, err := os.ReadDir(wDir)
	if err != nil {
		return nil
	}
	var out []*WorkerRow
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "archive" {
			continue
		}
		path := filepath.Join(wDir, e.Name(), "state.json")
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var s workers.State
		if err := json.Unmarshal(data, &s); err != nil {
			continue
		}
		row := workerRowFor(&s, project, now)
		if row != nil {
			out = append(out, row)
		}
	}
	return out
}

// workerRowFor maps one State into a WorkerRow with display fields.
// The Color drives the leading dot; the State 2-letter code is the
// trailing label per the mockup ("8m bl", "2m rv", "1m ok", "22m !!").
//
//	red    blocked / failed                   bl / !!
//	blue   in-review                          rv
//	amber  running clean (heartbeat fresh)    rn / ok
//	green  done (still showing pre-archive)   ok
//	amber  stale heartbeat                    !!
func workerRowFor(s *workers.State, project string, now time.Time) *WorkerRow {
	if s == nil {
		return nil
	}
	row := &WorkerRow{
		ID:      shortWorkerID(s.Slug),
		Project: project,
		Slug:    s.Slug,
		Phase:   s.Phase,
		Reason:  s.BlockedReason,
	}
	row.Age = humanAge(now.Sub(s.UpdatedAt))

	switch s.Phase {
	case workers.PhaseBlocked:
		row.Color = "red"
		row.State = "bl"
		row.Blocked = true
	case workers.PhaseFailed:
		row.Color = "red"
		row.State = "!!"
	case workers.PhaseReviewClaude, workers.PhaseReviewCodex:
		row.Color = "blue"
		row.State = "rv"
	case workers.PhaseDone:
		row.Color = "green"
		row.State = "ok"
	default:
		// running clean. Stale heartbeat flips to amber + "!!".
		if !s.UpdatedAt.IsZero() && now.Sub(s.UpdatedAt) > workerStaleWindow {
			row.Color = "amber"
			row.State = "!!"
		} else if s.Phase == workers.PhaseTDDRefactor || s.Phase == workers.PhasePush {
			row.Color = "amber"
			row.State = "rn"
		} else {
			row.Color = "green"
			row.State = "ok"
		}
	}
	return row
}

// shortWorkerID returns the 8-char anchor string the mockup shows in
// purple (e.g. "91f0a2c4"). Slugs end in a 4-hex suffix per
// tasks.GenerateSlug; we pull the last 8 hex/alpha chars so two slugs
// in the same project rarely collide visually. Falls back to the full
// slug when shorter than 8 chars.
func shortWorkerID(slug string) string {
	if len(slug) <= 8 {
		return slug
	}
	return slug[len(slug)-8:]
}

// deriveRepoSlug pulls "edisonshen/<name>" from a project's standards.md
// when the operator left a `repo:` hint, or falls back to "<name>".
// Standards files are small; we read once and return the first match.
//
// Match heuristic: a line containing "github.com/<owner>/<repo>" or a
// "- repo: <owner>/<repo>" bullet. Both shapes match what operators
// typically write into standards. When neither is present, we return
// the bare project name (matches the mockup's "side-experiment" row
// where the slug is just the dir name).
func deriveRepoSlug(projectDir, name string) string {
	path := filepath.Join(projectDir, "standards.md")
	data, err := os.ReadFile(path)
	if err != nil {
		return name
	}
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "- repo:") {
			s := strings.TrimSpace(strings.TrimPrefix(line, "- repo:"))
			if s != "" {
				return s
			}
		}
		// "github.com/owner/repo" → owner/repo (drop host + any trailing
		// path / query / .git).
		if i := strings.Index(line, "github.com/"); i >= 0 {
			s := line[i+len("github.com/"):]
			s = strings.TrimSuffix(s, ".git")
			// Stop at first whitespace, ), or quote.
			cut := strings.IndexAny(s, " \t)\"'")
			if cut > 0 {
				s = s[:cut]
			}
			// Keep only owner/repo (drop /tree/foo or /pull/123).
			if parts := strings.Split(s, "/"); len(parts) >= 2 {
				return parts[0] + "/" + parts[1]
			}
		}
	}
	return name
}

// CIRunning counts workers waiting on CI signals. Used by the header
// strip's "<K> ci running" totalizer. Pure derivation from a Snapshot.
func (s *Snapshot) CIRunning() int {
	if s == nil {
		return 0
	}
	n := 0
	for _, w := range s.Workers {
		switch w.Phase {
		case workers.PhaseReviewClaude, workers.PhaseReviewCodex, workers.PhasePush:
			n++
		}
	}
	return n
}

// AttentionProjects counts projects with at least one attention item
// (blocked worker or blocked task).
func (s *Snapshot) AttentionProjects() int {
	if s == nil {
		return 0
	}
	n := 0
	for _, p := range s.Projects {
		if p.Attention > 0 {
			n++
		}
	}
	return n
}
