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

import "github.com/edisonshen/fleet/internal/agent"

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
}

// dashboardRows assembles the unified row list from m.dashboard +
// m.records, applying the search filter when set. Always returns left-
// column rows (projects + tasks) before right-column rows (workers +
// agents) so cursor reading order matches visual reading order.
func (m Model) dashboardRows() []dashRow {
	var rows []dashRow
	if m.dashboard != nil {
		for _, p := range m.dashboard.Projects {
			if !m.matchesFilter(p.Name) && !m.matchesFilter(p.RepoSlug) {
				// Project doesn't match — skip the whole block (tasks
				// included). filter "" matches everything.
				continue
			}
			rows = append(rows, dashRow{kind: rowProject, project: p})
			for _, t := range p.Tasks {
				if !m.matchesFilter(t.Slug) {
					continue
				}
				rows = append(rows, dashRow{
					kind:          rowTask,
					task:          t,
					parentProject: p.Name,
				})
			}
		}
		for _, w := range m.dashboard.Workers {
			if !m.matchesFilter(w.Slug) && !m.matchesFilter(w.Project) {
				continue
			}
			rows = append(rows, dashRow{kind: rowWorker, worker: w})
		}
	}
	for _, r := range m.records {
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
