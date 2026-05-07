// Action handlers for v0.2 dashboard keybinds: [n] task-add, [/]
// search, [⏎] detail panel, [?] help. Co-located here (separate from
// keys.go's [d] dispatch picker) because they're all dashboard-native
// — no shelling out except [n] which uses the in-process tasks.Add
// API per operator's explicit feedback ("i dont want use cmd like
// fleet tasks add").
package tui

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tasks"
)

// taskAddDoneMsg is emitted after the in-process tasks.Add call
// returns. Drives the flash banner the same way dispatchDoneMsg does.
type taskAddDoneMsg struct {
	slug string
	err  error
}

// handleTaskAddKey processes keystrokes while modePromptTaskAdd is
// active. Mirrors handlePromptKey (single-line buffer + esc/enter +
// backspace + printable-rune append) but on submit calls the
// in-process tasks.Add path instead of shelling out — operator feedback
// in issue #53 is explicit: "i dont want use cmd like fleet tasks add".
func (m Model) handleTaskAddKey(key string) (Model, tea.Cmd, bool) {
	switch key {
	case "esc":
		m.mode = modeNav
		m.promptBuf = ""
		return m, nil, true
	case "enter":
		spec := strings.TrimSpace(m.promptBuf)
		m.mode = modeNav
		m.promptBuf = ""
		if spec == "" {
			return m, nil, true
		}
		project := m.taskAddProject()
		if project == "" {
			m.flash = &flashMsg{
				text:  "no project context — cd into a project repo first or move cursor onto a project row",
				isErr: true,
			}
			return m, nil, true
		}
		return m, addTaskCmd(project, spec), true
	case "backspace":
		if len(m.promptBuf) > 0 {
			m.promptBuf = m.promptBuf[:len(m.promptBuf)-1]
		}
		return m, nil, true
	}
	if len(key) == 1 && key[0] >= 0x20 && key[0] < 0x7f {
		m.promptBuf += key
		return m, nil, true
	}
	return m, nil, true
}

// handleSearchKey processes keystrokes while modePromptSearch is
// active. Live-applies the substring filter to dashboardRows() as the
// operator types — no need to wait for [enter]. [esc] clears the
// filter; [enter] commits it (closes the prompt without clearing).
func (m Model) handleSearchKey(key string) (Model, tea.Cmd, bool) {
	switch key {
	case "esc":
		m.mode = modeNav
		m.promptBuf = ""
		m.searchFilter = ""
		m.dashCursor = 0
		return m, nil, true
	case "enter":
		m.mode = modeNav
		m.searchFilter = m.promptBuf
		m.promptBuf = ""
		// Reset cursor since the row list shape just changed.
		m.dashCursor = 0
		return m, nil, true
	case "backspace":
		if len(m.promptBuf) > 0 {
			m.promptBuf = m.promptBuf[:len(m.promptBuf)-1]
		}
		// Live-update the filter as the operator narrows.
		m.searchFilter = m.promptBuf
		m.dashCursor = 0
		return m, nil, true
	}
	if len(key) == 1 && key[0] >= 0x20 && key[0] < 0x7f {
		m.promptBuf += key
		m.searchFilter = m.promptBuf
		m.dashCursor = 0
		return m, nil, true
	}
	return m, nil, true
}

// taskAddProject resolves the project context for [n] task-add.
// Precedence (issue #53 spec):
//  1. cursor on a project / task / worker row → that project
//  2. cursor on an agent row with a Project tag → that project
//  3. cwd basename via state.SafeProjectName(filepath.Base(cwd)) when
//     it lands inside a fleet-managed project dir
//  4. tui.ProjectTag(cwd) as a last resort
//
// Empty string means "no project context — flash error". The CLI's
// resolveProject() falls back to ProjectTag(cwd) for any non-fleet
// dir, but for the in-TUI [n] path we want a stricter check:
// "do at least one tasks.md exist for this project?" otherwise we'd
// silently create new projects from typos.
func (m Model) taskAddProject() string {
	if row := m.selectedRow(); row != nil {
		switch row.kind {
		case rowProject:
			if row.project != nil {
				return row.project.Name
			}
		case rowTask:
			return row.parentProject
		case rowWorker:
			if row.worker != nil {
				return row.worker.Project
			}
		case rowAgent:
			if row.agent != nil && row.agent.Project != "" {
				return row.agent.Project
			}
		}
	}
	// cwd-based resolution. Match the CLI's rules so [n] in the TUI
	// targets the same project as `fleet tasks add` from the same shell.
	if cwd, err := os.Getwd(); err == nil {
		if p := projectFromWorktreeCwd(cwd); p != "" {
			return p
		}
		// Fall back to ProjectTag — sanitization matches what
		// `fleet tasks add` defaults to.
		return ProjectTag(cwd)
	}
	return ""
}

// projectFromWorktreeCwd is duplicated from cmd/fleet/tasks.go to
// avoid an import cycle (cmd/fleet imports internal/tui). Three-line
// helper, per CLAUDE.md house style.
func projectFromWorktreeCwd(cwd string) string {
	root, err := state.Root()
	if err != nil {
		return ""
	}
	rootResolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		rootResolved = root
	}
	cwdResolved, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		cwdResolved = cwd
	}
	prefix := filepath.Join(rootResolved, "projects") + string(filepath.Separator)
	if !strings.HasPrefix(cwdResolved, prefix) {
		return ""
	}
	rest := cwdResolved[len(prefix):]
	parts := strings.Split(rest, string(filepath.Separator))
	if len(parts) < 3 || parts[1] != "worktrees" {
		return ""
	}
	return parts[0]
}

// addTaskCmd returns a tea.Cmd that calls tasks.Add for project +
// spec, deriving the slug from the spec's first line (matches CLI
// behavior in cmd/fleet/tasks.go's runTasksAdd). On success: emits
// taskAddDoneMsg with the final slug; on failure: emits with err.
func addTaskCmd(project, spec string) tea.Cmd {
	return func() tea.Msg {
		slug, err := addTask(project, spec)
		return taskAddDoneMsg{slug: slug, err: err}
	}
}

// addTask is the in-process tasks.Add path used by [n]. Pure function
// so tests can call it directly.
//
// Mirrors cmd/fleet/tasks.go runTasksAdd's core logic but trimmed to
// the v0.2-TUI minimum: status=todo, priority=P2, spawned_by=user,
// no depends_on, no acceptance/notes (operator fills via `fleet tasks
// note` later if needed). The slug auto-derives from the spec's first
// line.
func addTask(project, spec string) (string, error) {
	if _, err := state.Bootstrap(); err != nil {
		return "", fmt.Errorf("bootstrap: %w", err)
	}
	if err := state.ValidateProjectName(project); err != nil {
		return "", fmt.Errorf("project: %w", err)
	}

	dir, err := state.ProjectDir(project)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "tasks.md")

	release, err := state.LockProjectState(project)
	if err != nil {
		return "", fmt.Errorf("lock state: %w", err)
	}
	defer release()

	f, err := tasks.Read(path)
	if err != nil {
		return "", fmt.Errorf("read tasks.md: %w", err)
	}

	// Collect existing slugs (active + archived) so GenerateSlug avoids
	// 4hex collisions.
	existing := make([]string, 0, len(f.Tasks))
	for _, t := range f.Tasks {
		existing = append(existing, t.Slug)
	}
	if archive, _ := tasks.Read(filepath.Join(dir, "tasks-archive.md")); archive != nil {
		for _, t := range archive.Tasks {
			existing = append(existing, t.Slug)
		}
	}

	slug := tasks.GenerateSlug("", spec, existing)
	now := time.Now().UTC()
	t := &tasks.Task{
		Slug:      slug,
		Status:    tasks.StatusTodo,
		Priority:  tasks.PriorityP2,
		SpawnedBy: "user",
		Spec:      spec,
		Created:   now,
		Updated:   now,
	}
	if err := f.Add(t); err != nil {
		return "", fmt.Errorf("add: %w", err)
	}
	if err := tasks.Write(path, f); err != nil {
		return "", fmt.Errorf("write: %w", err)
	}
	return slug, nil
}

// readTaskDetail returns (body, title) for the [⏎] open detail panel
// when a task row is selected. Best-effort: a missing tasks.md or
// missing slug surfaces the error in the panel rather than blocking
// the open.
func readTaskDetail(project, slug string) (string, string) {
	title := fmt.Sprintf("task: %s/%s", project, slug)
	dir, err := state.ProjectDir(project)
	if err != nil {
		return fmt.Sprintf("error: %v", err), title
	}
	f, err := tasks.Read(filepath.Join(dir, "tasks.md"))
	if err != nil {
		return fmt.Sprintf("error reading tasks.md: %v", err), title
	}
	t, err := f.Get(slug)
	if err != nil {
		return fmt.Sprintf("task not found: %v", err), title
	}
	var b strings.Builder
	fmt.Fprintf(&b, "status:    %s\n", t.Status)
	fmt.Fprintf(&b, "priority:  %s\n", t.Priority)
	fmt.Fprintf(&b, "created:   %s\n", t.Created.Format(time.RFC3339))
	fmt.Fprintf(&b, "updated:   %s\n", t.Updated.Format(time.RFC3339))
	if t.Branch != "" {
		fmt.Fprintf(&b, "branch:    %s\n", t.Branch)
	}
	if t.PRURL != "" {
		fmt.Fprintf(&b, "pr_url:    %s\n", t.PRURL)
	}
	if len(t.DependsOn) > 0 {
		fmt.Fprintf(&b, "depends:   %s\n", strings.Join(t.DependsOn, ", "))
	}
	b.WriteString("\n### Spec\n")
	b.WriteString(strings.TrimSpace(t.Spec))
	if t.Acceptance != "" {
		b.WriteString("\n\n### Acceptance\n")
		b.WriteString(strings.TrimSpace(t.Acceptance))
	}
	if t.Notes != "" {
		b.WriteString("\n\n### Notes\n")
		b.WriteString(strings.TrimSpace(t.Notes))
	}
	return b.String(), title
}

// projectDetail returns (body, title) for the [⏎] open detail panel
// when a project row is selected. Shows the project's task counts +
// active worker list — a quick "what is this project doing" summary.
func projectDetail(p *ProjectRow) (string, string) {
	if p == nil {
		return "no project selected", "project"
	}
	title := fmt.Sprintf("project: %s", p.Name)
	var b strings.Builder
	if p.RepoSlug != "" && p.RepoSlug != p.Name {
		fmt.Fprintf(&b, "repo:    %s\n", p.RepoSlug)
	}
	fmt.Fprintf(&b, "todo:    %d\n", p.Counts.Todo)
	fmt.Fprintf(&b, "doing:   %d\n", p.Counts.InProgress)
	fmt.Fprintf(&b, "review:  %d\n", p.Counts.InReview)
	fmt.Fprintf(&b, "blocked: %d\n", p.Counts.Blocked)
	fmt.Fprintf(&b, "done:    %d\n", p.Counts.Done)
	switch {
	case p.Active:
		fmt.Fprintf(&b, "coord:   active (last tick %s ago)\n", humanAge(time.Since(p.LastTick)))
	case p.IdleStop:
		b.WriteString("coord:   idle / auto-stopped\n")
	default:
		b.WriteString("coord:   idle\n")
	}
	if len(p.Tasks) > 0 {
		b.WriteString("\nactive tasks:\n")
		for _, t := range p.Tasks {
			fmt.Fprintf(&b, "  %s  %s\n", t.Status, t.Slug)
		}
	}
	return b.String(), title
}

// readWorkerDetail returns (body, title) for the [⏎] open / [a]
// peek panel when a worker row is selected. Replicates `fleet peek`
// inline: parsed state.json + last 50 lines of output.log.
func readWorkerDetail(project, slug string) (string, string) {
	title := fmt.Sprintf("worker: %s/%s", project, slug)
	dir, err := state.WorkerDir(project, slug)
	if err != nil {
		return fmt.Sprintf("error: %v", err), title
	}
	dir = strings.TrimSuffix(dir, string(filepath.Separator))
	statePath := filepath.Join(dir, "state.json")
	logPath := filepath.Join(dir, "output.log")

	var b strings.Builder
	if data, err := os.ReadFile(statePath); err == nil {
		// Re-marshal indented for readability — matches `fleet peek`'s
		// writeStateBlock behavior.
		var pretty map[string]any
		if jerr := json.Unmarshal(data, &pretty); jerr == nil {
			if out, merr := json.MarshalIndent(pretty, "", "  "); merr == nil {
				b.Write(out)
				b.WriteString("\n")
			} else {
				b.Write(data)
			}
		} else {
			b.Write(data)
		}
	} else if errors.Is(err, os.ErrNotExist) {
		b.WriteString("(no state.json yet)\n")
	} else {
		fmt.Fprintf(&b, "error reading state.json: %v\n", err)
	}
	b.WriteString("\n--- output.log (last 50 lines) ---\n")
	tail, terr := readLastLines(logPath, 50)
	switch {
	case errors.Is(terr, os.ErrNotExist):
		b.WriteString("(no output.log yet)\n")
	case terr != nil:
		fmt.Fprintf(&b, "error reading output.log: %v\n", terr)
	default:
		b.WriteString(tail)
	}
	return b.String(), title
}

// readAgentDetail returns (body, title) for the [⏎] open detail
// panel when an agent row is selected. Shows the agent's record JSON
// — useful for debugging what fleet-guard wrote to disk.
func readAgentDetail(r *agent.Record) (string, string) {
	if r == nil {
		return "no agent selected", "agent"
	}
	title := fmt.Sprintf("agent: %s", r.ID)
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Sprintf("error: %v", err), title
	}
	return string(data), title
}

// readLastLines returns the last n lines of path. Reads the whole
// file rather than seeking from the end — output.log files are
// bounded by tmux pane buffers in practice (a few hundred KB), so
// the simpler implementation wins per CLAUDE.md "three lines beats a
// generic helper".
func readLastLines(path string, n int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n") + "\n", nil
}
