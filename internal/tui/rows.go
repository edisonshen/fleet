// Unified dashboard row list — folds projects, tasks, workers, and
// agents into one cursor-addressable sequence so [j/k] and the
// row-aware action keys ([⏎], [a], [h], [x], [n]) operate on a single
// index regardless of column.
//
// Order matches the rendered layout (issue #53):
//
//	left column:  project1, task1a, task1b, project2, task2a, …
//	right column: worker1, worker2, …, agent1, agent2, …
//
// dashboardRows returns left then right so a "next row" with [j] walks
// the left column top-to-bottom, then jumps to the top of the right
// column. That mirrors the cursor visiting "rows in reading order".
package tui

import (
	"os"
	"sort"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/state"
)

// rowKind discriminates which payload the row carries.
type rowKind int

const (
	rowProject rowKind = iota
	rowTask
	rowWorker
	rowAgent
)

// dashRow is one navigable row. Exactly one of project / task /
// worker / agent is non-nil per row (matching kind).
type dashRow struct {
	kind rowKind

	// project is set for rowProject. Carries Name (project key) for
	// downstream lookups.
	project *ProjectRow
	// task is set for rowTask. parentProject names the project the
	// task belongs to so detail / open can read tasks.md from the
	// right path.
	task          *taskRow
	parentProject string
	// worker is set for rowWorker.
	worker *WorkerRow
	// agent is set for rowAgent.
	agent *agent.Record
}

// taskRow is the per-task line shown under each project block. Only
// the fields the dashboard / detail panel consume — the full Task
// shape lives in internal/tasks and is read fresh for [⏎] open.
type taskRow struct {
	Slug   string
	Status string // tasks.Status as a string — kept stringly so render doesn't import the enum
	// Synthetic markers — set when the row is not a real task entry
	// but a hint line shown under an expanded project (issue #59):
	//
	//	Empty=true   → "no tasks yet — `fleet init` to create tasks.md"
	//	              (synthetic project with no v0.2 dir)
	//	More=N       → "+N more" footer when an expanded project has
	//	              more than maxExpandedTasks visible tasks.
	//
	// Only one of Empty / More is non-zero per row. Real task rows
	// have both unset and Slug populated.
	Empty bool
	More  int
}

// maxExpandedTasks caps how many task rows render under one expanded
// project (issue #59 spec: "up to ~10 visible at full expansion").
// Tasks beyond this collapse into a "+N more" tail row that is itself
// navigable so j/k still walks the column predictably.
const maxExpandedTasks = 10

// unifiedProjects returns the LEFT-column project list: the union of
// v0.2-initialized project dirs (m.dashboard.Projects) plus synthetic
// rows for any project tag carried by an agent record (m.records[*].
// Project) that isn't already represented.
//
// Rationale (issue #55): agents dispatched against a non-v0.2 repo
// (no ~/.fleet/projects/<tag>/ tree) still belong to a project
// conceptually. Without this union the LEFT column would only show
// v0.2-init'd projects, leaving operators with active workers on
// regular repos staring at a blank PROJECTS column.
//
// Synthetic rows carry only Name + RepoSlug — no task counts, no
// coord status, no LastTick. The right-column agent block continues
// to render the agent record itself.
//
// Output is stable: real (v0.2-init'd) projects retain m.dashboard's
// original sort order; synthetic rows append after, alpha-sorted.
func (m Model) unifiedProjects() []*ProjectRow {
	var out []*ProjectRow
	seen := map[string]bool{}
	if m.dashboard != nil {
		for _, p := range m.dashboard.Projects {
			out = append(out, p)
			seen[p.Name] = true
		}
	}
	// Collect synthetic project tags from agent records, dedupe, sort.
	syntheticNames := make([]string, 0, len(m.records))
	for _, r := range m.records {
		if r == nil || r.Project == "" {
			continue
		}
		if seen[r.Project] {
			continue
		}
		seen[r.Project] = true
		syntheticNames = append(syntheticNames, r.Project)
	}
	sort.Strings(syntheticNames)
	for _, name := range syntheticNames {
		out = append(out, &ProjectRow{
			Name:     name,
			RepoSlug: name,
		})
	}
	// Issue #63: task_id fallback signal. When a project's CoordID is
	// empty (lock body hasn't published yet — coord skill is still
	// booting), look for an alive agent record tagged
	// task_id=coord-<name> AND project=<name>. This is the first 10-30s
	// after dispatch; without this fallback, the spawned coord shows on
	// RIGHT under "v0.1 agents" until the skill ticks, and the operator
	// sees a missing-coord row that begs another [a] press → zombie pile.
	//
	// Lock body wins when both signals are present (scanProject sets
	// CoordID only inside the freshness gate); the fallback only fills
	// when CoordID is empty. We clone any row we mutate so the shared
	// snapshot pointer stays untouched (m.dashboard.Projects is read by
	// every renderer + selectedRow, and silent mutation would surprise
	// downstream callers).
	for i, p := range out {
		if p == nil || p.CoordID != "" {
			continue
		}
		rec := findCoordByTaskID(m.records, p.Name)
		if rec == nil {
			continue
		}
		clone := *p
		clone.CoordID = rec.ID
		out[i] = &clone
	}
	return out
}

// coordBootWindow is the time window during which the task_id
// fallback signal is allowed to bind a coord on LEFT. Beyond this,
// the only valid coord identity signal is the lock-body branch in
// scanProject (gated by coord-state.json mtime within
// coordActiveWindow). The marker file's mtime is the freshness
// reference: it's last written when the TUI dispatches the coord, so
// it pins "the coord skill is still booting".
//
// Codex iter-4 P1: without a freshness gate the fallback would
// resurrect any coord whose claude process exited (leaving the tmux
// shell wrapper alive) — the marker plus the alive shell would re-
// promote forever, hiding the dead agent from [a]/[x] recovery.
// 60s is comfortable: the coord skill normally takes 10-30s to boot
// + write its first coord-state.json. Past 60s either the lock-body
// branch has taken over (coord is genuinely running) or the boot
// failed and the operator should triage via the right column.
const coordBootWindow = 60 * time.Second

// findCoordByTaskID returns the first agent.Record whose task_id is
// coordTaskID(projectName) AND project matches AND tmux session is
// alive AND the per-project state tree exists at
// ~/.fleet/projects/<name>/ AND the project's coord-spawn marker
// (state.CoordSpawnMarkerPath) contains the candidate agent's ID
// AND the marker's mtime is within coordBootWindow.
//
// Marker gate (codex iter-3 P2) closes the "operator runs `fleet
// dispatch coord-<project> --project <project> --coord-spawn`" spoof:
// the spoofer can write a record but cannot make the dashboard treat
// it as the coord (the marker is written by the TUI's
// coordSpawnDoneMsg handler post-dispatch and contains the agent ID
// the TUI is in the middle of attaching to). Without the right
// marker content, no promotion happens.
//
// Marker freshness gate (codex iter-4 P1) keeps the fallback from
// resurrecting a stale coord whose claude process exited but whose
// tmux shell wrapper is still alive — beyond the boot window, the
// only authoritative identity signal is the lock-body branch in
// scanProject (which requires a fresh coord-state.json tick).
//
// Project-tree gate (codex iter-2 P2) keeps legacy records (pre-PR
// agents with the same task_id shape) from auto-promoting on first
// upgrade — the TUI's post-issue-#63 auto-spawn always runs
// state.EnsureProjectInitialized BEFORE dispatch, so post-PR records
// always satisfy the gate.
//
// Distinct from findExistingCoordForProject in keys.go: this one is
// the dashboard's binding signal (does this agent represent the
// project's coord visually?); the keys.go counterpart is the [a]
// idempotency signal. Same predicate; kept separate so the rendering
// layer doesn't import keys.go's action wiring.
func findCoordByTaskID(records []*agent.Record, projectName string) *agent.Record {
	want := "coord-" + projectName
	// Project-tree existence and marker read are 1-3 stats per call,
	// fast enough to run inline in unifiedProjects on every render.
	// Cache only if a future profile shows it's hot.
	if !projectTreeExistsFn(projectName) {
		return nil
	}
	wantID := coordSpawnMarkerFn(projectName)
	if wantID == "" {
		return nil
	}
	// Freshness gate (codex iter-4 P1): the marker's mtime must be
	// within coordBootWindow. Beyond that, the lock-body branch in
	// scanProject is the only valid identity signal.
	mt, ok := coordSpawnMarkerMtimeFn(projectName)
	if !ok {
		return nil
	}
	if nowFn().Sub(mt) > coordBootWindow {
		return nil
	}
	for _, r := range records {
		if r == nil {
			continue
		}
		if r.TaskID != want {
			continue
		}
		if r.Project != projectName {
			continue
		}
		if r.ID != wantID {
			// Marker content names a different agent — this record is
			// either a duplicate spawn or a spoof. Don't promote.
			continue
		}
		if r.TmuxSession == "" {
			continue
		}
		if !sessionAliveFn(r.TmuxSession) {
			// codex iter-3 P2: tmux.HasSession returns false for both
			// "session does not exist" AND transport errors (bad
			// FLEET_TMUX_SOCKET, restarting server). Both cases are
			// handled by sessionAliveFn (which is HasSession in
			// production); a probe failure shouldn't refuse to bind a
			// known coord. Use sessionProbeFn for the tristate read so
			// errors fall through to "treat as alive" (don't drop the
			// claim) rather than "definitively dead".
			if !sessionProbeOrAliveFn(r.TmuxSession) {
				continue
			}
		}
		return r
	}
	return nil
}

// projectTreeExistsFn returns true when ~/.fleet/projects/<name>/ is
// a directory on disk. var so tests can stub the disk check without
// seeding a tmp tree. Production calls projectTreeExists.
var projectTreeExistsFn = projectTreeExists

// projectTreeExists is the production stat path used by
// findCoordByTaskID's anti-promotion gate.
func projectTreeExists(projectName string) bool {
	dir, err := state.ProjectDir(projectName)
	if err != nil {
		return false
	}
	info, err := os.Stat(dir)
	if err != nil {
		return false
	}
	return info.IsDir()
}

// coordSpawnMarkerFn returns the agent ID stored in the project's
// coord-spawn marker file. var so tests can stub. Production calls
// state.ReadCoordSpawnMarker.
var coordSpawnMarkerFn = state.ReadCoordSpawnMarker

// coordSpawnMarkerMtimeFn returns the marker file's modification time
// + ok=true when present. var so tests can stub the freshness check
// without timing-sensitive disk writes. Production stat-reads the
// marker; missing returns ok=false.
var coordSpawnMarkerMtimeFn = func(projectName string) (time.Time, bool) {
	path, err := state.CoordSpawnMarkerPath(projectName)
	if err != nil {
		return time.Time{}, false
	}
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}, false
	}
	return info.ModTime(), true
}

// nowFn returns the current wall-clock time. var so tests can pin
// "now" deterministically without touching time.Now globally.
var nowFn = time.Now

// sessionProbeOrAliveFn returns true when the session is alive OR
// when the probe failed (transport error — don't drop a claim on a
// tmux hiccup). False only on a definitive "no such session" answer.
//
// Strategy: call sessionAliveFn first (the bool primitive most tests
// stub). If it says alive, we're done. Only on a "false" answer do
// we re-probe with the tristate sessionProbeFn to distinguish dead
// from probe-error. This keeps unit tests that stub only
// sessionAliveFn from needing to also stub sessionProbeFn for the
// happy path. Codex iter-3 P2 / iter-6 P2.
var sessionProbeOrAliveFn = func(session string) bool {
	if sessionAliveFn(session) {
		return true
	}
	alive, err := sessionProbeFn(session)
	if err != nil {
		return true // transport error → conservative: treat as alive
	}
	return alive
}

// dashboardRows assembles the unified row list from unifiedProjects() +
// m.records, applying the search filter when set. Always returns left-
// column rows (projects + tasks) before right-column rows (workers +
// agents) so cursor reading order matches visual reading order.
//
// Filter behavior on project blocks: a project is included when the
// project name OR repo slug matches, OR when at least one of its
// tasks matches. The latter case keeps the parent project header
// rendered alongside the matching tasks so a task-slug query like
// "/fix-toolbar-1a2b" surfaces a navigable row instead of silently
// dropping the whole block (codex iter-1 P2).
//
// Expansion gating (issue #59): task rows render under a project
// header iff the project is expanded (m.expanded[name] == true) OR
// the active search filter is the reason this project was kept in
// the list (project name didn't match, but a task slug did). The
// search override means /fix-toolbar still surfaces the matching
// task row even with the parent project collapsed. When neither
// gate triggers the project row appears alone — the operator presses
// [⏎] to open the inline task list.
//
// Coord-on-LEFT (issue #55): a coord agent (record whose ID matches
// some ProjectRow.CoordID) is filtered out of the right-column agents
// section. The project-side render attaches the coord visually under
// the project block; double-rendering it on the right would create
// the appearance of two agents for one underlying record.
func (m Model) dashboardRows() []dashRow {
	var rows []dashRow
	projects := m.unifiedProjects()
	coordIDs := map[string]bool{}
	for _, p := range projects {
		if p.CoordID != "" {
			coordIDs[p.CoordID] = true
		}
	}
	for _, p := range projects {
		projectMatches := m.matchesFilter(p.Name) || m.matchesFilter(p.RepoSlug)
		// Pre-check: do any tasks match? If yes, render the parent
		// project header even though its name didn't match.
		anyTaskMatches := false
		if !projectMatches {
			for _, t := range p.Tasks {
				if m.matchesFilter(t.Slug) {
					anyTaskMatches = true
					break
				}
			}
		}
		if !projectMatches && !anyTaskMatches {
			continue
		}
		rows = append(rows, dashRow{kind: rowProject, project: p})
		// Expansion gate: tasks render only when the operator has
		// explicitly expanded the project OR a task-slug search is
		// the reason we're showing this project (search override —
		// without it /fix-toolbar would surface a parent-only row
		// and silently hide the matching task).
		expanded := m.expanded != nil && m.expanded[p.Name]
		if !expanded && !anyTaskMatches {
			continue
		}
		// Synthetic projects (no v0.2 dir, no tasks.md) carry zero
		// p.Tasks. Surface a single "no tasks yet" hint row so the
		// expansion still renders something the operator can read,
		// matching the spec.
		if len(p.Tasks) == 0 {
			rows = append(rows, dashRow{
				kind:          rowTask,
				task:          &taskRow{Empty: true},
				parentProject: p.Name,
			})
			continue
		}
		// Real tasks. Honor the per-task filter when only tasks
		// matched, and cap the visible count at maxExpandedTasks
		// with a "+N more" tail so an expanded project doesn't
		// dominate the column.
		//
		// Two-pass: first collect every task that's eligible to
		// show (passes the filter), then emit up to N + a "+more"
		// tail. Single-pass with a running counter would
		// mis-attribute "+more" to filtered-out rows below the cap.
		var eligible []*taskRow
		for _, t := range p.Tasks {
			if !projectMatches && !expanded && !m.matchesFilter(t.Slug) {
				continue
			}
			eligible = append(eligible, t)
		}
		shown := eligible
		var more int
		if len(eligible) > maxExpandedTasks {
			shown = eligible[:maxExpandedTasks]
			more = len(eligible) - maxExpandedTasks
		}
		for _, t := range shown {
			rows = append(rows, dashRow{
				kind:          rowTask,
				task:          t,
				parentProject: p.Name,
			})
		}
		if more > 0 {
			rows = append(rows, dashRow{
				kind:          rowTask,
				task:          &taskRow{More: more},
				parentProject: p.Name,
			})
		}
	}
	if m.dashboard != nil {
		for _, w := range m.dashboard.Workers {
			if !m.matchesFilter(w.Slug) && !m.matchesFilter(w.Project) {
				continue
			}
			rows = append(rows, dashRow{kind: rowWorker, worker: w})
		}
	}
	for _, r := range m.records {
		if r != nil && coordIDs[r.ID] {
			// Coord renders on the LEFT under its project row; skip it
			// here so the right-column doesn't double-count.
			continue
		}
		if !m.matchesFilter(r.ID) && !m.matchesFilter(r.TaskID) && !m.matchesFilter(r.Project) {
			continue
		}
		rows = append(rows, dashRow{kind: rowAgent, agent: r})
	}
	return rows
}

// matchesFilter returns true when the search filter is empty OR s
// contains the filter as a case-insensitive substring. Empty s with a
// non-empty filter never matches — search shouldn't catch nameless rows
// when the operator is hunting for something specific.
func (m Model) matchesFilter(s string) bool {
	if m.searchFilter == "" {
		return true
	}
	if s == "" {
		return false
	}
	return containsFold(s, m.searchFilter)
}

// selectedRow returns the dashRow under m.dashCursor, or nil when the
// list is empty / cursor out of bounds. Action handlers call this to
// dispatch [a]/[h]/[x]/[⏎] based on row type.
func (m Model) selectedRow() *dashRow {
	rows := m.dashboardRows()
	if len(rows) == 0 || m.dashCursor < 0 || m.dashCursor >= len(rows) {
		return nil
	}
	return &rows[m.dashCursor]
}

// rowIdentity returns a string key for r that survives reorders /
// refreshes. Used to re-locate the cursor after a dashboardMsg or
// agentsMsg shifts the row list (codex iter-5 P1). Identity shape:
//
//	"P:<name>"             project
//	"T:<project>:<slug>"   task
//	"W:<project>:<slug>"   worker
//	"A:<id>"               agent
//
// Empty string means "no identity available" (defensive — caller
// falls back to cursor 0).
func rowIdentity(r dashRow) string {
	switch r.kind {
	case rowProject:
		if r.project != nil {
			return "P:" + r.project.Name
		}
	case rowTask:
		if r.task != nil {
			// Synthetic markers (empty-state hint, "+N more" footer)
			// have no slug — give them a stable identity per project so
			// refreshCursor relocates back onto the same hint row across
			// dashboardMsg ticks. Without this the operator's cursor
			// would snap to dashCursor=0 every refresh while the cursor
			// was on a hint row.
			switch {
			case r.task.Empty:
				return "T:" + r.parentProject + ":__empty__"
			case r.task.More > 0:
				return "T:" + r.parentProject + ":__more__"
			}
			return "T:" + r.parentProject + ":" + r.task.Slug
		}
	case rowWorker:
		if r.worker != nil {
			return "W:" + r.worker.Project + ":" + r.worker.Slug
		}
	case rowAgent:
		if r.agent != nil {
			return "A:" + r.agent.ID
		}
	}
	return ""
}

// refreshCursor relocates dashCursor onto the same row identity it
// pointed at before the refresh. Called from dashboardMsg / agentsMsg
// reducers so background updates don't silently retarget [enter] /
// [a] / [n] / [x] onto a different row.
//
// Strategy: if we know the previously-selected row's identity, look
// for the same identity in the new dashboardRows() and snap the
// cursor there. If not found (row disappeared), clamp to the start
// of the list — better than dangling on a stale index.
func (m *Model) refreshCursor(prevIdentity string) {
	rows := m.dashboardRows()
	if len(rows) == 0 {
		m.dashCursor = 0
		return
	}
	if prevIdentity != "" {
		for i, r := range rows {
			if rowIdentity(r) == prevIdentity {
				m.dashCursor = i
				return
			}
		}
		// We HAD a previously-selected row identity but it's gone from
		// the new row list. Reset to the top rather than silently
		// keeping the same numeric index — the row at that index is a
		// different entity now, and acting on it via [⏎]/[a]/[h]/[x]
		// would target the wrong record (codex iter-6 P1).
		m.dashCursor = 0
		return
	}
	// No previous identity (early renders before the first
	// agentsMsg/dashboardMsg). Just clamp to range.
	if m.dashCursor < 0 || m.dashCursor >= len(rows) {
		m.dashCursor = 0
	}
}

// containsFold is a case-insensitive strings.Contains. Avoids importing
// strings just for ToLower in rows.go.
func containsFold(haystack, needle string) bool {
	if len(needle) > len(haystack) {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if equalFoldASCII(haystack[i:i+len(needle)], needle) {
			return true
		}
	}
	return false
}

func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
