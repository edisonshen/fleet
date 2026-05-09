// Dashboard data layer — assembles ProjectRow + WorkerRow snapshots
// from ~/.fleet/projects/*/ for the v0.2 "Ops Console" view.
//
// Pure I/O on disk + parse via the existing internal/tasks +
// internal/workers helpers. No mutation. The TUI calls
// loadDashboardCmd once per tick / fsEvent and reads the resulting
// snapshot into Model.dashboard.
//
// Coord status is inferred without disturbing the flock on
// coordinator.lock — we stat coord-state.json's mtime and treat
// anything within coordActiveWindow as "● active". The actual flock
// is held by the running coord, but flock(2) does NOT touch mtime, so
// coordinator.lock keeps the mtime of its first creation and is
// useless as a heartbeat. coord-state.json IS rewritten every tick
// (skills/coordinator/loop.py:_save_coord_state → tmp + rename), so
// its mtime advances with every coord tick. Presence of
// coordinator.lock without a fresh coord-state.json reads as "● idle"
// (operator can see that a coord ran here once but isn't ticking).
package tui

import (
	"bytes"
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
	Active    bool       // coord lock mtime within window
	IdleStop  bool       // file present but stale → "auto-stopped" pill
	LastTick  time.Time  // coord-state.json mtime; zero if missing
	Attention int        // count of workers in phase=blocked (raise-hand). Issue #103: task status=blocked is planning state, not actionable, and is excluded.
	BlockedQ  string     // first blocked worker's reason (for raise-hand expansion P2)
	BlockedID string     // first blocked worker's ID
	Tasks     []*taskRow // task rows for [j/k] navigation + [⏎] open
	// CoordID is the agent ID currently holding coordinator.lock for
	// this project, when freshness-gated by coord-state.json's mtime
	// (within coordActiveWindow). Empty when no coord is active OR the
	// lock body wasn't populated (legacy coords pre-issue #55).
	//
	// Render path: the LEFT column attaches the matching agent record
	// (by Record.ID == CoordID) directly under the project block; the
	// RIGHT column filters that same agent out of its loose-agents
	// section so a coord doesn't double-render.
	CoordID string
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

	// Task counts + per-task rows. Read errors collapse to zero counts
	// + nil Tasks — the row still renders so the operator can see the
	// project exists. Per-task rows feed [j/k] navigation + [⏎] open.
	//
	// Issue #101 lifecycle hygiene: done + abandoned tasks ARE included
	// here (previously filtered out). The row split between active +
	// history happens in the row-list assembly path so the operator
	// can collapse history under a `─── N done ───` separator without
	// losing the entries entirely. This keeps tasks.md as the durable
	// source of truth for task history while the TUI surface stays
	// uncluttered by default.
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
			row.Tasks = append(row.Tasks, &taskRow{
				Slug:   t.Slug,
				Status: string(t.Status),
				PRURL:  t.PRURL,
			})
		}
	}

	// Coord active / idle. coord-state.json's mtime is the per-tick
	// heartbeat (loop.py writes it via tmp+rename every tick under the
	// flock); coordinator.lock's mtime is set at creation and never
	// advances, so it can't drive the active decision. We still check
	// for the lock file's existence as a "coord has been here" signal —
	// when the JSON is missing OR stale AND the lock exists, we render
	// "○ idle · auto-stopped" so the operator knows the coord has run
	// here previously even though it isn't ticking now.
	stateJSON := filepath.Join(dir, "coord-state.json")
	lockFile := filepath.Join(dir, ".locks", "coordinator.lock")
	if info, err := os.Stat(stateJSON); err == nil {
		mt := info.ModTime()
		row.LastTick = mt
		if now.Sub(mt) <= coordActiveWindow {
			row.Active = true
		} else if _, lerr := os.Stat(lockFile); lerr == nil {
			row.IdleStop = true
		}
	} else if _, lerr := os.Stat(lockFile); lerr == nil {
		// Lock exists but no coord-state.json yet — coord has run here
		// at some point but never reached the first state-write. Treat
		// as auto-stopped so the operator sees the project at all.
		row.IdleStop = true
	}
	// Holder ID is only trusted when the coord is fresh (Active). flock
	// doesn't truncate on release, so a stale lock body would otherwise
	// promote a dead coord to LEFT-column rendering. The freshness gate
	// is the load-bearing safeguard from the issue-#55 design — without
	// it the body alone is unreliable.
	if row.Active {
		row.CoordID = readCoordHolder(projectsRoot, name)
	}

	// Workers under workers/<slug>/state.json.
	wrows := scanWorkers(dir, name, now)

	// Attention math: ONLY worker phase=blocked fires the attention chip.
	//
	// Issue #103: task status=blocked is a planning state — operators set
	// it when sequencing work ("blocked by external dep / other task"),
	// not when something needs answering. Rolling task-blocked into
	// row.Attention overcounted "1 need attention" on projects whose only
	// "blocked" was the planning signal, training the operator to ignore
	// the chip. Worker phase=blocked (the loop below) is the load-bearing
	// signal — that's the path a worker raises a question through, and
	// v0.2 Agent-tool subagents share the same code path.
	//
	// Counts.Blocked stays populated on row scan above (line 217) for
	// diagnostics + future filtering; only the attention rollup is
	// dropped. The visual signal for a planning-blocked task is the
	// distinct ⏸ glyph in the per-task expansion (taskStatusStyles), not
	// the row-level attention chip.
	var firstBlocked *WorkerRow
	for _, w := range wrows {
		if w.Blocked {
			row.Attention++
			if firstBlocked == nil {
				firstBlocked = w
			}
		}
	}
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
//
// Lifecycle defense-in-depth (issue #101): when a worker dir's
// state.json reports a TerminalSuccess (phase=done) or TerminalFailure
// (phase=failed) phase, scanWorkers fires `workers.Delete` to rm-rf
// the dir before returning the row. The deleted worker is omitted
// from the returned slice so the dashboard renders the snapshot it
// would see on the next tick (no row), avoiding a one-tick flicker
// of the soon-to-disappear worker.
//
// Blocked is Waiting in the lifecycle classification — the worker dir
// is intentionally kept (operator may inspect blocked_reason) and a
// row is returned so the operator sees the blocked signal in the
// right column.
//
// Coord skill is the primary trigger; this scan is the catch-all for
// orphan dirs (coord crash, manual `fleet workers update`, dirs left
// behind from before issue #101 landed). Both call the same Delete;
// idempotent on ENOENT, so a coord-then-TUI race is safe.
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
		// Skip in-flight delete-staging dirs. workers.Delete renames
		// to `<slug>.deleting-YYYYMMDD-HHMMSS[-N]` before RemoveAll;
		// the dir is short-lived (microseconds on POSIX same-fs)
		// but a tick that catches one mid-rename would otherwise
		// surface a row for a soon-to-vanish worker.
		//
		// We anchor on the shape `*.deleting-<UTCstamp>` rather than
		// a bare substring match so an operator-authored slug whose
		// text happens to contain `.deleting-` (allowed by
		// state.ValidateSlug — periods + hyphens are valid runes)
		// still renders normally.
		if isDeletingStagingName(e.Name()) {
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
		// Defense-in-depth: rm-rf done/failed worker dirs the coord
		// skill missed. Skip the row so the snapshot matches what the
		// next render will see (the dir is gone). Errors are
		// swallowed — the operator's visible state is correct
		// either way; logging here would spam the TUI's stdout
		// (which lipgloss has already painted over).
		if s.Phase == workers.PhaseDone || s.Phase == workers.PhaseFailed {
			_ = workersDeleteFn(project, s.Slug)
			continue
		}
		row := workerRowFor(&s, project, now)
		if row != nil {
			out = append(out, row)
		}
	}
	return out
}

// workersDeleteFn is the Delete function the dashboard scan calls.
// var so tests can stub the disk write without seeding a real worker
// tree. Production calls workers.Delete.
var workersDeleteFn = workers.Delete

// isDeletingStagingName reports whether name matches the shape
// workers.Delete renames to before RemoveAll:
//
//	<base>.deleting-YYYYMMDD-HHMMSS
//	<base>.deleting-YYYYMMDD-HHMMSS-<digit>   (collision-retry suffix)
//
// We anchor on the trailing 15-char timestamp + optional `-<digit>` so
// an operator-authored slug whose text legitimately contains the
// substring `.deleting-` (allowed by state.ValidateSlug) still passes.
// Returns false on any pattern mismatch.
func isDeletingStagingName(name string) bool {
	const marker = ".deleting-"
	const stampLen = len("YYYYMMDD-HHMMSS") // 15
	i := strings.LastIndex(name, marker)
	if i < 0 {
		return false
	}
	rest := name[i+len(marker):]
	// Strip optional `-<digit>` collision suffix.
	if len(rest) >= 2 && rest[len(rest)-2] == '-' && rest[len(rest)-1] >= '0' && rest[len(rest)-1] <= '9' {
		rest = rest[:len(rest)-2]
	}
	if len(rest) != stampLen {
		return false
	}
	// Validate stamp shape: 8 digits, hyphen, 6 digits.
	for k, c := range rest {
		switch k {
		case 8:
			if c != '-' {
				return false
			}
		default:
			if c < '0' || c > '9' {
				return false
			}
		}
	}
	return true
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
	// Defend against state.json with no UpdatedAt — a malformed write
	// or a hand-edit can leave the field zero, and humanAge(now - 0001)
	// renders as a nonsense ~700000000d. Show "—" instead so the row
	// remains legible.
	if s.UpdatedAt.IsZero() {
		row.Age = "—"
	} else {
		row.Age = humanAge(now.Sub(s.UpdatedAt))
	}

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

// shortWorkerID returns the purple anchor string per the mockup. We
// surface the trailing 4-hex suffix that tasks.GenerateSlug appends
// (e.g. slug "fix-toolbar-1a2b" → "1a2b"). The mockup illustrates
// 8-char hex IDs but Fleet slugs only carry a 4-hex disambiguator;
// 4 chars is enough to scan-distinguish workers in the same project.
// Slugs that don't end in `-XXXX` (4 hex) fall back to the full slug
// — covers legacy / hand-edited entries.
func shortWorkerID(slug string) string {
	if len(slug) >= 5 && slug[len(slug)-5] == '-' {
		tail := slug[len(slug)-4:]
		ok := true
		for _, c := range tail {
			if !isHexLower(c) {
				ok = false
				break
			}
		}
		if ok {
			return tail
		}
	}
	return slug
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

// readCoordHolder returns the agent ID currently holding
// coordinator.lock for the given project, or "" when unknown.
//
// The Python coord skill writes <coord_id>\n into the lock-file body
// after acquiring LOCK_EX (skills/coordinator/loop.py:_try_lock,
// issue #55). We read the body, take the first line, and validate it
// shape-matches an 8-char lower-hex agent ID — anything else (legacy
// zero-byte lock, hand-edit, garbage) returns "" so the caller falls
// through to the no-coord render path.
//
// Important: this function does NOT validate freshness. flock(2) does
// not truncate the body on release, so a stale lock from a dead coord
// can still report a valid-shape ID. Callers gate "is this current?"
// on coord-state.json's mtime within coordActiveWindow before
// trusting the holder. See projectsRoot/<name>/coord-state.json.
func readCoordHolder(projectsRoot, projectName string) string {
	path := filepath.Join(projectsRoot, projectName, ".locks", "coordinator.lock")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	// Take the first line only; Python writes "<id>\n", but be tolerant
	// of CRLF or operator hand-edits with extra trailing lines.
	line := data
	if i := bytes.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	if i := bytes.IndexByte(line, '\r'); i >= 0 {
		line = line[:i]
	}
	s := strings.TrimSpace(string(line))
	if !isAgentIDShape(s) {
		return ""
	}
	return s
}

// isAgentIDShape returns true when s looks like an 8-char lower-hex
// agent ID (the shape agent.NewID generates). Anything else — empty
// string, wrong length, mixed case, non-hex chars — returns false.
func isAgentIDShape(s string) bool {
	if len(s) != 8 {
		return false
	}
	for _, c := range s {
		if !isHexLower(c) {
			return false
		}
	}
	return true
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
// (worker in phase=blocked). Issue #103: task status=blocked is a
// planning state and does NOT contribute to attention.
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
