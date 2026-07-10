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
	"github.com/edisonshen/fleet/internal/coordlock"
	"github.com/edisonshen/fleet/internal/state"
)

// rowKind discriminates which payload the row carries.
type rowKind int

const (
	rowProject rowKind = iota
	rowTask
	rowWorker
	rowAgent
	// rowSeparator is the "─── N idle ───" / "─── N hidden ───" group
	// row in the LEFT column (issue #98). Cursor lands on it; [enter]
	// expands/collapses the group; [c] off a project row toggles
	// show-hidden mode. The payload lives in dashRow.separator.
	rowSeparator
)

// separatorKind names which group a rowSeparator represents.
type separatorKind int

const (
	separatorIdle      separatorKind = iota // "─── N idle ───"
	separatorHidden                         // "─── N hidden ───" (only when show-hidden mode is on)
	separatorHistory                        // "─── N done ───" inside an expanded project block (issue #101)
	separatorAgentIdle                      // "─── N idle ───" inside the RIGHT-column v0.1 agent list (dashboard-accumulation-f-4421)
)

// separatorRow carries the per-group state for rowSeparator.
type separatorRow struct {
	kind     separatorKind
	count    int  // number of collapsed projects (or tasks, for separatorHistory) under this separator
	expanded bool // when true, the group's contents render after the separator
	// project is set for separatorHistory: the parent project whose
	// done/abandoned tasks the separator gates. Empty for idle/hidden
	// separators (those operate at the project-list level).
	project string
}

// dashRow is one navigable row. Exactly one of project / task /
// worker / agent / separator is non-nil per row (matching kind).
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
	// separator is set for rowSeparator (issue #98).
	separator *separatorRow
}

// taskRow is the per-task line shown under each project block. Only
// the fields the dashboard / detail panel consume — the full Task
// shape lives in internal/tasks and is read fresh for [⏎] open.
type taskRow struct {
	Slug   string
	Status string // tasks.Status as a string — kept stringly so render doesn't import the enum
	// PRURL is the task entry's pr_url (from tasks.md). Carried through
	// to the rendering layer for the issue #101 history group: a done
	// task with a PR URL renders as `✓ slug · PR #N` so the operator
	// can see the link without opening the detail panel. Empty for
	// non-history rows + history rows whose task never opened a PR.
	PRURL string
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
//
// Hidden filter (issue #98): names on the operator's hidden list are
// stripped from BOTH the v0.2-dirs source AND the synthetic-from-
// agent-tag source uniformly. Workers/agents tagged with a hidden
// project still appear in the RIGHT column (workers + "v0.1 agents")
// — that's the agent listing, not the project listing. This is the
// single point where the hidden filter applies so the rest of the
// rendering pipeline doesn't need to know about it.
func (m Model) unifiedProjects() []*ProjectRow {
	return m.unifiedProjectsFiltered(hiddenProjectsSet())
}

// unifiedProjectsAll returns the un-filtered union (no hidden filter
// applied). Used by the activity-aware footer chip and the hidden-
// group rendering, both of which need to see hidden projects to count
// them. Distinct from unifiedProjects so the hot path (rendering the
// LEFT column) always strips hidden uniformly.
func (m Model) unifiedProjectsAll() []*ProjectRow {
	return m.unifiedProjectsFiltered(nil)
}

// unifiedProjectsFiltered is the worker. hiddenSet, when non-nil,
// removes any name in the set from both data sources. nil hiddenSet
// returns the full union.
func (m Model) unifiedProjectsFiltered(hiddenSet map[string]bool) []*ProjectRow {
	var out []*ProjectRow
	seen := map[string]bool{}
	if m.dashboard != nil {
		for _, p := range m.dashboard.Projects {
			if p == nil {
				continue
			}
			if hiddenSet[p.Name] {
				// Drop hidden project from the v0.2-dirs source.
				continue
			}
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
		// Skip malformed project tags from agent records too
		// (invalid-project-dir-guar-d636). A record tagged
		// project="--project" (CLI flag-misparse) must not synthesize a
		// row that re-hijacks the title / inflates the count via the
		// agent-record source, even though scanDashboard already filters
		// the on-disk dir.
		if err := state.ValidateProjectName(r.Project); err != nil {
			continue
		}
		if hiddenSet[r.Project] {
			// Drop hidden project from the synthetic-from-agent source.
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

// findCoordByTaskID returns the first agent.Record whose task_id is
// coordTaskID(projectName) AND project matches AND tmux session is
// alive AND the per-project state tree exists at ~/.fleet/projects/<name>/
// AND the coordinator LEASE names the candidate agent's ID (the live active
// owner, or the journal-named in-flight handoff successor).
//
// Lease gate (D3, replaces the deleted coord-spawn marker): the coordinator
// lease is the identity now, so the "operator hand-spawns a coord-shaped
// record" spoof no longer promotes — a bare record that never acquired the
// lease is never named by LiveOwner/CurrentHandoffSuccessor. The lease's
// process-liveness (LiveOwner) + successor process-liveness
// (CurrentHandoffSuccessor) subsume the old marker-mtime freshness gate: a
// coord whose process exited stops being named, so a stale tmux shell
// wrapper can't resurrect a dead coord.
//
// Project-tree gate (codex iter-2 P2) keeps legacy records (pre-PR
// agents with the same task_id shape) from auto-promoting on first
// upgrade.
//
// Distinct from findExistingCoordForProject in keys.go: this one is
// the dashboard's binding signal (does this agent represent the
// project's coord visually?); the keys.go counterpart is the [a]
// idempotency signal. Both read the same lease identity.
func findCoordByTaskID(records []*agent.Record, projectName string) *agent.Record {
	want := coordTaskID(projectName)
	// Project-tree existence is 1 stat per call, fast enough to run inline in
	// unifiedProjects on every render.
	if !projectTreeExistsFn(projectName) {
		return nil
	}
	wantID := coordSpawnIdentityFn(projectName)
	if wantID == "" {
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
			// The lease names a different agent — this record is either a
			// duplicate spawn or a spoof. Don't promote.
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

// coordSpawnIdentityFn returns the agent ID the coordinator LEASE names as the
// project's coord — the live flock owner (coordlock.LiveOwner) or, mid-handoff,
// the journal-named in-flight successor whose process is alive
// (coordlock.CurrentHandoffSuccessor). Empty when no coord holds the flock and
// no handoff is in flight. PR-2 (D4/D5) dropped the `starting` tier — acquiring
// the flock IS becoming the coordinator, so there is no pre-activation starter
// state any more. var so tests can stub the lease read.
//
// LiveOwner is process-live (kernel-proven — no staleness), so a coord whose
// process died no longer holds the flock and no longer names an identity; the
// dashboard row flips to Idle automatically.
var coordSpawnIdentityFn = coordSpawnLeaseIdentity

func coordSpawnLeaseIdentity(projectName string) string {
	owner, ownerOK := coordlock.LiveOwner(projectName)
	successorID, successorOK := coordlock.CurrentHandoffSuccessor(projectName)
	return resolveCoordSpawnIdentity(owner, ownerOK, successorID, successorOK)
}

// resolveCoordSpawnIdentity is the PURE identity decision (no I/O — the lease
// reads are threaded in) so the branch ordering is unit testable without a real
// lease. It mirrors the Python `fleet coord-owner` path + loop.py gate-5, which
// treat BOTH the live flock owner AND the in-flight handoff successor as coord
// identity.
//
// Priority: a process-live flock owner, else the journal-named in-flight
// successor (process alive).
func resolveCoordSpawnIdentity(
	owner coordlock.Owner, ownerOK bool,
	successorID string, successorOK bool,
) string {
	// (1) A process-live flock owner is unambiguously the coord.
	if ownerOK && owner.AgentID != "" {
		return owner.AgentID
	}
	// (2) In-flight handoff successor: OLD released the flock and NEW is the
	// journal-named successor whose process is alive (CurrentHandoffSuccessor is
	// liveness-gated) but has not yet acquired the flock. Mirror the coord-owner
	// CLI + loop.py gate-5 — otherwise the dashboard flashes "no coord"
	// mid-handoff and [a] dedup falls through to a duplicate fresh dispatch.
	if successorOK && successorID != "" {
		return successorID
	}
	return ""
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
//
// Activity grouping + hidden list (issue #98): projects are partitioned
// into ACTIVE / IDLE / HIDDEN buckets BEFORE the per-project task
// expansion runs. ACTIVE rows render in their natural order at the top.
// IDLE rows collapse under a "─── N idle ───" separator that the
// operator can [enter] to expand. HIDDEN rows are filtered out by
// unifiedProjects() and rejoin the row list under "─── N hidden ───"
// only when m.showHidden is true. An active search filter overrides
// the idle separator (matching rows surface inline regardless of
// activity) so / search keeps working as before.
func (m Model) dashboardRows() []dashRow {
	var rows []dashRow
	projects := m.unifiedProjects()
	hiddenSet := hiddenProjectsSet()

	// Coord-on-LEFT skip set (issue #55): collect across the visible
	// projects so the right-column agent block knows which records to
	// drop. Hidden projects are already absent from `projects`, so
	// their coord IDs naturally fall through to the agent list — that
	// matches the spec ("agents tagged with a hidden project still
	// appear in 'v0.1 agents'").
	coordIDs := map[string]bool{}
	for _, p := range projects {
		if p.CoordID != "" {
			coordIDs[p.CoordID] = true
		}
	}

	// Search-mode override: when an active search filter is set, the
	// idle separator is suppressed and idle projects merge inline so
	// matching rows surface. The operator's hunting for a specific
	// row, not browsing groups.
	searchActive := m.searchFilter != ""

	// Partition into ACTIVE / IDLE buckets. Activity classification
	// runs once per render — pure, no I/O.
	now := nowFn()
	var workers []*WorkerRow
	if m.dashboard != nil {
		workers = m.dashboard.Workers
	}
	window := m.activeWindow
	if window <= 0 {
		window = activeWindowDefault
	}
	var active, idle []*ProjectRow
	for _, p := range projects {
		addedAt := projectAddedAtFn(p.Name)
		if classifyProjectActivity(p, workers, m.records, addedAt, now, window) == projectActive {
			active = append(active, p)
		} else {
			idle = append(idle, p)
		}
	}

	// ACTIVE bucket — render normally.
	rows = m.appendProjectRows(rows, active)

	// IDLE bucket — collapse under separator unless search is active.
	//
	// Default-expansion rule (issue #98 ergonomic refinement): when no
	// projects are ACTIVE, treat the idle group as expanded inline and
	// SUPPRESS the separator. The "─── N idle ───" wall hiding every
	// project would invert the "see everything that's there" first-
	// impression for an operator with one stale-but-still-relevant
	// project. With no active rows above to focus on, the separator's
	// "you have collapsed stuff" signal is redundant.
	//
	// Once any project is ACTIVE, the separator does its job: surface
	// the count, let [enter] expand. The operator's explicit toggle
	// via [enter] (idleCollapseExplicit=true) overrides the auto-
	// suppress so a deliberate "show me everything" stays sticky.
	switch {
	case searchActive:
		rows = m.appendProjectRows(rows, idle)
	case len(idle) > 0:
		// Auto-expand-no-separator only fires when:
		//   - no projects are ACTIVE (no anchor for the operator's eye)
		//   - operator hasn't pressed [enter] on the separator (no
		//     explicit choice)
		//   - operator hasn't toggled idleExpanded via the keybind
		// Otherwise honor the operator's expansion state with the
		// separator visible.
		autoSuppress := !m.idleCollapseExplicit && !m.idleExpanded && len(active) == 0
		if autoSuppress {
			rows = m.appendProjectRows(rows, idle)
		} else {
			rows = append(rows, dashRow{
				kind: rowSeparator,
				separator: &separatorRow{
					kind:     separatorIdle,
					count:    len(idle),
					expanded: m.idleExpanded,
				},
			})
			if m.idleExpanded {
				rows = m.appendProjectRows(rows, idle)
			}
		}
	}

	// HIDDEN bucket — only when show-hidden mode is on. Reads the
	// un-filtered list so hidden-but-still-on-disk projects can render
	// dim under their own separator.
	if m.showHidden && len(hiddenSet) > 0 {
		all := m.unifiedProjectsAll()
		var hidden []*ProjectRow
		for _, p := range all {
			if p == nil || !hiddenSet[p.Name] {
				continue
			}
			hidden = append(hidden, p)
		}
		if len(hidden) > 0 {
			rows = append(rows, dashRow{
				kind: rowSeparator,
				separator: &separatorRow{
					kind:     separatorHidden,
					count:    len(hidden),
					expanded: m.hiddenExpanded,
				},
			})
			if m.hiddenExpanded {
				rows = m.appendProjectRows(rows, hidden)
			}
		}
	}

	// RIGHT column — workers, then v0.1 agents.
	if m.dashboard != nil {
		for _, w := range m.dashboard.Workers {
			if !m.matchesFilter(w.Slug) && !m.matchesFilter(w.Project) {
				continue
			}
			rows = append(rows, dashRow{kind: rowWorker, worker: w})
		}
	}
	// Partition v0.1 agent records into Active / Idle buckets:
	//   - asking (NeedsInput=true) or blocked (Blocked=true) ALWAYS render
	//     active regardless of staleness — those need operator attention.
	//   - LastActivityTS within `window` (the same threshold used for
	//     LEFT-column project activity) → active.
	//   - everything else → idle (collapses under the agent-idle separator).
	// Search override: when a filter is set the idle separator is
	// suppressed and all surviving agents render inline (matches the
	// LEFT-column projects' search-mode override).
	var activeAgents, idleAgents []*agent.Record
	for _, r := range m.records {
		if r == nil {
			continue
		}
		if coordIDs[r.ID] {
			// Coord renders on the LEFT under its project row; skip it
			// here so the right-column doesn't double-count.
			continue
		}
		if !m.matchesFilter(r.ID) && !m.matchesFilter(r.TaskID) && !m.matchesFilter(r.Project) {
			continue
		}
		if classifyAgentActivity(r, now, window) == projectActive {
			activeAgents = append(activeAgents, r)
		} else {
			idleAgents = append(idleAgents, r)
		}
	}
	for _, r := range activeAgents {
		rows = append(rows, dashRow{kind: rowAgent, agent: r})
	}
	switch {
	case len(idleAgents) == 0:
		// No idle bucket; nothing to collapse, no separator.
	case searchActive:
		// Search override — show all surviving agents inline.
		for _, r := range idleAgents {
			rows = append(rows, dashRow{kind: rowAgent, agent: r})
		}
	default:
		rows = append(rows, dashRow{
			kind: rowSeparator,
			separator: &separatorRow{
				kind:     separatorAgentIdle,
				count:    len(idleAgents),
				expanded: m.agentIdleExpanded,
			},
		})
		if m.agentIdleExpanded {
			for _, r := range idleAgents {
				rows = append(rows, dashRow{kind: rowAgent, agent: r})
			}
		}
	}
	return rows
}

// classifyAgentActivity returns Active for v0.1 agent records that need
// operator attention regardless of staleness (asking/blocked flags) OR
// have heartbeat / spawn freshness inside the configured active window.
// Idle otherwise. Mirrors classifyProjectActivity's shape so the
// operator's mental model for "what counts as active" stays one rule
// across columns.
//
// Asking (NeedsInput) and Blocked are sticky-active by design: a record
// flagged for either has unresolved state the operator must triage
// before it should disappear from view. Collapsing them under the idle
// separator would hide the exact rows that motivate keeping the panel
// open. Both flags survive staleness regardless of LastActivityTS.
//
// SpawnedAt is the fallback freshness signal — a brand-new agent whose
// fleet-guard hook hasn't yet written its first heartbeat (or a record
// authored in code without LastActivityTS, e.g. test fixtures) still
// reads as active when spawned within `window`. Without this, freshly
// dispatched agents would briefly collapse under the idle separator
// before their first Stop-hook fires.
func classifyAgentActivity(
	r *agent.Record,
	now time.Time,
	window time.Duration,
) projectActivity {
	if r == nil {
		return projectIdle
	}
	if r.NeedsInput || r.Blocked {
		return projectActive
	}
	if !r.LastActivityTS.IsZero() && now.Sub(r.LastActivityTS) <= window {
		return projectActive
	}
	if !r.SpawnedAt.IsZero() && now.Sub(r.SpawnedAt) <= window {
		return projectActive
	}
	return projectIdle
}

// appendProjectRows adds project + (optionally expanded) task rows for
// each entry in projects. Encapsulates the per-project filter +
// expansion logic that dashboardRows runs across both ACTIVE and IDLE
// buckets (issue #98). Returns the extended row slice.
//
// Behavior matches the pre-#98 dashboardRows inner loop verbatim — the
// only change is that this helper is called per-bucket so an ACTIVE
// project and an IDLE-but-expanded project render identically once
// they reach this function.
func (m Model) appendProjectRows(rows []dashRow, projects []*ProjectRow) []dashRow {
	for _, p := range projects {
		if p == nil {
			continue
		}
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
		//
		// Issue #101 lifecycle hygiene: split eligible tasks into
		// active (lifecycle Prerun/Active/Waiting) and history
		// (lifecycle TerminalSuccess/TerminalFailure). Active
		// renders inline as before; history collapses under a
		// `─── N done ───` separator that the operator [enter]s
		// to expand. Search overrides the split — when a task slug
		// matches the active filter, surface it inline regardless
		// of lifecycle so / search keeps working as before.
		var activeEligible, historyEligible []*taskRow
		for _, t := range p.Tasks {
			if !projectMatches && !expanded && !m.matchesFilter(t.Slug) {
				continue
			}
			if isHistoryTaskRow(t) && m.searchFilter == "" {
				historyEligible = append(historyEligible, t)
				continue
			}
			activeEligible = append(activeEligible, t)
		}
		shown := activeEligible
		var more int
		if len(activeEligible) > maxExpandedTasks {
			shown = activeEligible[:maxExpandedTasks]
			more = len(activeEligible) - maxExpandedTasks
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
		// History group (issue #101). Only renders when the project
		// has at least one done/abandoned task AND we're not inside
		// an active search (search merges everything inline).
		if len(historyEligible) > 0 {
			histExpanded := m.historyExpanded != nil && m.historyExpanded[p.Name]
			rows = append(rows, dashRow{
				kind:          rowSeparator,
				parentProject: p.Name,
				separator: &separatorRow{
					kind:     separatorHistory,
					count:    len(historyEligible),
					expanded: histExpanded,
					project:  p.Name,
				},
			})
			if histExpanded {
				// Reuse the same maxExpandedTasks cap so a project
				// with hundreds of done tasks doesn't blow out the
				// column. "+N more" tail is identical to the active
				// path above; both lists wear the cap independently.
				histShown := historyEligible
				var histMore int
				if len(historyEligible) > maxExpandedTasks {
					histShown = historyEligible[:maxExpandedTasks]
					histMore = len(historyEligible) - maxExpandedTasks
				}
				for _, t := range histShown {
					rows = append(rows, dashRow{
						kind:          rowTask,
						task:          t,
						parentProject: p.Name,
					})
				}
				if histMore > 0 {
					rows = append(rows, dashRow{
						kind:          rowTask,
						task:          &taskRow{More: histMore},
						parentProject: p.Name,
					})
				}
			}
		}
	}
	return rows
}

// isHistoryTaskRow reports whether t belongs in the project's history
// (done / abandoned) group. Synthetic markers (Empty / More) are NOT
// history — they're navigation aids that always render in the active
// section. Issue #101.
func isHistoryTaskRow(t *taskRow) bool {
	if t == nil || t.Empty || t.More > 0 {
		return false
	}
	return t.Status == "done" || t.Status == "abandoned"
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
//	"S:idle"               idle separator (issue #98)
//	"S:hidden"             hidden separator (issue #98)
//	"S:history:<project>"  task-history separator inside a project (issue #101)
//	"S:agent-idle"         right-column v0.1 agent-idle separator
//	                       (dashboard-accumulation-f-4421)
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
	case rowSeparator:
		if r.separator != nil {
			switch r.separator.kind {
			case separatorIdle:
				return "S:idle"
			case separatorHidden:
				return "S:hidden"
			case separatorHistory:
				return "S:history:" + r.separator.project
			case separatorAgentIdle:
				return "S:agent-idle"
			}
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
