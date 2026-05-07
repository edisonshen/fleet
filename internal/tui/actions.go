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
		m.taskAddProjectFrozen = ""
		return m, nil, true
	case "enter":
		spec := strings.TrimSpace(m.promptBuf)
		project := m.taskAddProjectFrozen
		m.mode = modeNav
		m.promptBuf = ""
		m.taskAddProjectFrozen = ""
		if spec == "" {
			return m, nil, true
		}
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
// Precedence (issue #53 spec; codex iter-3 P2 tightened):
//  1. cursor on a project / task / worker row → that project
//  2. cursor on an agent row with a Project tag → that project
//  3. cwd is inside a Fleet-managed worktree under
//     ~/.fleet/projects/<name>/worktrees/<slug>/ → that project
//  4. cwd is the operator's project repo AND that project already
//     exists in ~/.fleet/projects/ (so the operator was previously
//     working on it via `fleet dispatch` or coord) → that project tag
//
// Empty string means "no project context — flash error". This
// deliberately refuses to create a brand-new project from a
// random cwd: an unknown directory pressing [n] would silently
// create ~/.fleet/projects/<random-tag>/ with a phantom task,
// which is worse than asking the operator to be explicit.
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
			// Trust the agent's Project tag. `fleet dispatch` can
			// create agent records BEFORE the per-project state dir
			// exists, so an existence gate here would block [n] on
			// freshly-spawned agents (codex iter-5 P2). The phantom-
			// project risk is bounded: a record under ~/.fleet/agents/
			// only lands when an operator-driven dispatch ran, so the
			// project tag is intentional, not random-cwd noise.
			if row.agent != nil && row.agent.Project != "" {
				return row.agent.Project
			}
		}
	}
	// cwd-based resolution. First check the worktree-cwd path (worker
	// shells live there); fall back to cwd-derived tag IF and ONLY IF
	// the project already exists on disk (codex iter-3 P2). Walk up
	// the directory tree so [n] works from a repo subdirectory like
	// `repo/internal/tui/` — without the walk, ProjectTag(cwd) would
	// produce "internal-tui" and miss the actual repo's project tag
	// (codex iter-10 P2).
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	if p := projectFromWorktreeCwd(cwd); p != "" {
		return p
	}
	// Find the git repo root by walking up looking for a .git entry.
	// Without that boundary, an unbounded walk would keep climbing
	// to the filesystem root, where ProjectTag("/") sanitizes to
	// "fleet" — pressing [n] from any unrelated cwd would silently
	// target the project named "fleet" if it exists (codex iter-11
	// P1). When no repo root is found, refuse so the operator gets
	// an explicit "no project context" error.
	root := repoRootForCwd(cwd)
	if root == "" {
		return ""
	}
	tag := ProjectTag(root)
	if tag != "" && projectDirExists(tag) {
		return tag
	}
	return ""
}

// repoRootForCwd walks up from cwd until it finds a directory
// containing `.git` (file or dir — covers worktrees). Empty string
// when no repo boundary is reached before hitting the filesystem
// root or $HOME.
func repoRootForCwd(cwd string) string {
	dir := cwd
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// projectDirExists returns true when ~/.fleet/projects/<name>/ is
// present on disk. Used by both the cursor-on-agent-row branch and
// the cwd fallback to refuse pressing [n] into a project that doesn't
// exist (codex iter-4 P2).
func projectDirExists(name string) bool {
	root, err := state.Root()
	if err != nil {
		return false
	}
	_, err = os.Stat(filepath.Join(root, "projects", name))
	return err == nil
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
	// 4hex collisions. Archive read errors must NOT be silently
	// ignored — if tasks-archive.md is corrupted, we'd otherwise
	// regenerate slugs that already exist there and the next archive
	// merge would conflict (codex iter-4 P3). ENOENT is fine (no
	// archive yet); anything else surfaces to the operator via the
	// taskAddDoneMsg flash.
	existing := make([]string, 0, len(f.Tasks))
	for _, t := range f.Tasks {
		existing = append(existing, t.Slug)
	}
	archivePath := filepath.Join(dir, "tasks-archive.md")
	if _, statErr := os.Stat(archivePath); statErr == nil {
		archive, archErr := tasks.Read(archivePath)
		if archErr != nil {
			return "", fmt.Errorf("read tasks-archive.md: %w", archErr)
		}
		for _, t := range archive.Tasks {
			existing = append(existing, t.Slug)
		}
	} else if !os.IsNotExist(statErr) {
		return "", fmt.Errorf("stat tasks-archive.md: %w", statErr)
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
// inline: parsed state.json + last 50 lines of output.log. Falls
// back to the archive directory when the active worker dir's
// state.json is gone (e.g. the worker finished + was archived
// between the last dashboard refresh and the operator's keypress) —
// matches `fleet peek`'s readWorkerStateAnywhere behavior so the
// inline peek doesn't degrade to ENOENT for a row the dashboard
// still happens to show (codex iter-10 P3).
func readWorkerDetail(project, slug string) (string, string) {
	title := fmt.Sprintf("worker: %s/%s", project, slug)
	dir, err := state.WorkerDir(project, slug)
	if err != nil {
		return fmt.Sprintf("error: %v", err), title
	}
	dir = strings.TrimSuffix(dir, string(filepath.Separator))
	statePath := filepath.Join(dir, "state.json")
	logPath := filepath.Join(dir, "output.log")
	// Active dir gone? Try the archive (same lookup `fleet peek`
	// uses). When the archive entry is found, repoint state/log paths
	// at it so the rest of the function doesn't need to know where
	// the data came from.
	if _, statErr := os.Stat(statePath); errors.Is(statErr, os.ErrNotExist) {
		if archDir := newestArchiveWorkerDir(project, slug); archDir != "" {
			dir = archDir
			statePath = filepath.Join(dir, "state.json")
			logPath = filepath.Join(dir, "output.log")
			title = fmt.Sprintf("worker (archived): %s/%s", project, slug)
		}
	}

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

// newestArchiveWorkerDir scans
// ~/.fleet/projects/<project>/workers/archive/ for entries shaped
// `<slug>-YYYYMMDD-HHMMSS[-N]` and returns the lexicographically-
// largest match (newest stamp). Empty string when no match. Mirrors
// cmd/fleet/peek.go's readArchivedWorkerState lookup but returns
// only the directory path — the caller reads state.json + output.log
// the same way it would for an active worker.
func newestArchiveWorkerDir(project, slug string) string {
	pdir, err := state.ProjectDir(project)
	if err != nil {
		return ""
	}
	archRoot := filepath.Join(pdir, "workers", "archive")
	entries, err := os.ReadDir(archRoot)
	if err != nil {
		return ""
	}
	const stampLen = len("YYYYMMDD-HHMMSS") // 15
	var newest string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		// Strip optional `-N` collision-retry salt (single digit).
		stripped := name
		if len(stripped) >= 2 && stripped[len(stripped)-2] == '-' &&
			stripped[len(stripped)-1] >= '0' && stripped[len(stripped)-1] <= '9' {
			stripped = stripped[:len(stripped)-2]
		}
		if len(stripped) < stampLen+1+len(slug) {
			continue
		}
		end := len(stripped) - stampLen - 1
		if stripped[end] != '-' || stripped[:end] != slug {
			continue
		}
		if name > newest {
			newest = name
		}
	}
	if newest == "" {
		return ""
	}
	return filepath.Join(archRoot, newest)
}

// readLastLines returns the last n lines of path. Reads from the
// end of the file in a fixed-size window so a long-running worker
// with a multi-MB output.log doesn't block the TUI render path or
// allocate proportional memory (codex iter-5 P2). The window is
// sized as max(64KiB, n*256B) which comfortably covers 50 lines of
// typical worker output even when a single line is very long.
func readLastLines(path string, n int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	size := info.Size()
	const minWindow int64 = 64 * 1024
	want := int64(n) * 256
	if want < minWindow {
		want = minWindow
	}
	off := size - want
	if off < 0 {
		off = 0
	}
	// Probe the byte just before the seek point so we can tell
	// whether `off` lands mid-line (partial rune we must discard) or
	// exactly on a newline boundary (line is whole, keep it). codex
	// iter-7 P3 — previous variant always dropped the first line and
	// silently hid the most-recent intact log line when the window
	// happened to start cleanly.
	startsMidLine := false
	if off > 0 {
		buf := make([]byte, 1)
		if _, perr := f.ReadAt(buf, off-1); perr == nil {
			startsMidLine = buf[0] != '\n'
		}
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return "", err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	text := strings.TrimRight(string(data), "\n")
	lines := strings.Split(text, "\n")
	if startsMidLine && len(lines) > 0 {
		lines = lines[1:]
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n") + "\n", nil
}
