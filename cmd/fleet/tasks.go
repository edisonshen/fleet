package main

// fleet tasks {add,list,show,set,note,archive,promote} — operator-facing
// CLI for the per-project tasks.md registry (PLAN-v0.2-coordinator.md
// "New Go" table; ENG §2.1).
//
// Every subcommand resolves --project to a canonical name (defaults to
// the cwd's basename via tui.ProjectTag, sanitized to the
// state.ValidateProjectName allowlist) and serializes any tasks.md
// mutation through state.LockProjectState — Q1's single state-lock per
// project state-dir. Reads are lock-free (rename-publish makes torn
// reads impossible).

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tasks"
	"github.com/edisonshen/fleet/internal/tui"
)

// newTasksCmd wires the umbrella `fleet tasks` command and its
// subcommands. Cobra renders the umbrella's help with the subcommand
// names; per-subcommand help is on each leaf.
func newTasksCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tasks",
		Short: "Manage the per-project tasks.md registry",
		Long: `tasks owns the per-project task list at
~/.fleet/projects/<project>/tasks.md — the v0.2 source of truth the
coordinator dispatches workers from. Subcommands cover the full
add/list/show/set/note/archive/promote lifecycle.

Default --project is the current directory's basename (via the same
sanitizer fleet dispatch uses). All mutations serialize through the
per-project state-lock, so concurrent invocations are safe.`,
	}
	cmd.AddCommand(
		newTasksAddCmd(),
		newTasksListCmd(),
		newTasksShowCmd(),
		newTasksSetCmd(),
		newTasksNoteCmd(),
		newTasksArchiveCmd(),
		newTasksPromoteCmd(),
	)
	return cmd
}

// resolveProject normalizes --project. Empty input falls back to:
//  1. cwd if cwd is inside ~/.fleet/projects/<project>/worktrees/<slug>
//     → extract <project>. Workers spawned by the coord run inside
//     this layout; their cwd's parent-basename would otherwise be
//     "worktrees-<slug>" and silently misroute mutations into a
//     phantom project (codex iter-7 P1).
//  2. otherwise the cwd's last-two-segments tag (matching the TUI's
//     [d]-picker default so `fleet dispatch` and `fleet tasks` agree
//     on the default project for operator-side invocations).
//
// The result is validated so callers don't have to.
func resolveProject(p string) (string, error) {
	if p == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("getwd: %w", err)
		}
		if proj := projectFromWorktreeCwd(cwd); proj != "" {
			p = proj
		} else {
			p = tui.ProjectTag(cwd)
		}
	}
	if err := state.ValidateProjectName(p); err != nil {
		return "", fmt.Errorf("--project: %w", err)
	}
	return p, nil
}

// projectFromWorktreeCwd returns the project name when cwd is under
// ~/.fleet/projects/<project>/worktrees/<slug>(/...). Empty string
// means "not inside a fleet-managed worktree" — caller falls back to
// the operator-side ProjectTag(cwd) heuristic.
//
// We resolve symlinks on both sides because state.WorktreePath returns
// a path that may itself be under a symlinked $HOME on macOS, while
// os.Getwd() may report the realpath; without resolution the prefix
// match fails for real worker shells.
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
	// Need at least <project>/worktrees/<slug> after "projects/".
	if len(parts) < 3 || parts[1] != "worktrees" {
		return ""
	}
	return parts[0]
}

// readTasks loads tasks.md for project. Missing file returns an empty
// File at the current schema (the lazy-create contract).
func readTasks(project string) (*tasks.File, string, error) {
	dir, err := state.ProjectDir(project)
	if err != nil {
		return nil, "", err
	}
	path := filepath.Join(dir, "tasks.md")
	f, err := tasks.Read(path)
	if err != nil {
		return nil, "", err
	}
	return f, path, nil
}

// withTasksLock acquires the project state-lock for the duration of fn.
// The lock serializes all tasks/learnings writes (Q1 single lock per
// project state-dir).
func withTasksLock(project string, fn func() error) error {
	release, err := state.LockProjectState(project)
	if err != nil {
		return fmt.Errorf("lock state: %w", err)
	}
	defer release()
	return fn()
}

// existingSlugs returns every slug currently in tasks.md (used for
// GenerateSlug collision avoidance).
func existingSlugs(f *tasks.File) []string {
	out := make([]string, 0, len(f.Tasks))
	for _, t := range f.Tasks {
		out = append(out, t.Slug)
	}
	return out
}

// archivedSlugList returns the slugs currently in
// tasks-archive.md. Missing file → empty slice (not an error).
func archivedSlugList(project string) ([]string, error) {
	dir, err := state.ProjectDir(project)
	if err != nil {
		return nil, err
	}
	archPath := filepath.Join(dir, "tasks-archive.md")
	if _, err := os.Stat(archPath); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	f, err := tasks.Read(archPath)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(f.Tasks))
	for _, t := range f.Tasks {
		out = append(out, t.Slug)
	}
	return out, nil
}

// readArchiveTasks loads tasks-archive.md (or empty if missing) and
// returns its Task slice. Used by `tasks list` to merge done/abandoned
// rows from the archive into the recent-activity view. Missing file is
// NOT an error — most projects archive lazily and won't have one until
// the first archive run.
func readArchiveTasks(project string) ([]*tasks.Task, error) {
	dir, err := state.ProjectDir(project)
	if err != nil {
		return nil, err
	}
	archPath := filepath.Join(dir, "tasks-archive.md")
	if _, err := os.Stat(archPath); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	f, err := tasks.Read(archPath)
	if err != nil {
		return nil, err
	}
	return f.Tasks, nil
}

// isFullSlug mirrors internal/tasks.isFullSlug — `<short>-<4hex>`.
// Used to decide whether a `--slug` value passed by the operator is
// already a final slug (so we should refuse on archive collision)
// versus a short name that GenerateSlug will postfix.
func isFullSlug(s string) bool {
	if len(s) < 6 {
		return false
	}
	if s[len(s)-5] != '-' {
		return false
	}
	for _, c := range s[:len(s)-5] {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	for _, c := range s[len(s)-4:] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// validateScalarBullet rejects values that would corrupt a `- key:
// value` bullet on the next round-trip (codex iter-4 P2). Newlines
// turn the tail of the value into free-floating markdown that
// internal/tasks.Read then refuses with "unexpected content between
// sections", bricking tasks.md for the whole project. Multi-line
// content belongs in section bodies (Spec / Acceptance / Notes), not
// in scalar bullets.
func validateScalarBullet(key, value string) error {
	if strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("%s contains newline (scalar bullet must be one line; multi-line content goes in Spec/Acceptance/Notes)", key)
	}
	return nil
}

// validateSectionBody mirrors the unfenced-header check inside
// internal/tasks.File.Add (codex iter-1 P1). Operator-supplied bodies
// must NOT contain column-0 `## ` (would split the task on next read)
// nor reserved `### Spec|Acceptance|Notes` (would terminate the
// section). Fenced code blocks are exempt — examples that paste
// markdown stay quoted.
//
// Section name is one of "spec" / "acceptance" / "notes" — used only
// for the error message.
func validateSectionBody(section, body string) error {
	inFence := false
	for _, ln := range strings.Split(body, "\n") {
		if isFenceMarker(ln) {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if strings.HasPrefix(ln, "## ") {
			return fmt.Errorf("%s body has unfenced column-0 '## ' (use ### for in-section headings, or wrap in ``` for examples)", section)
		}
		if strings.HasPrefix(ln, "### ") {
			name := strings.TrimSpace(strings.TrimPrefix(ln, "### "))
			switch name {
			case "Spec", "Acceptance", "Notes":
				return fmt.Errorf("%s body contains unfenced reserved heading '### %s' (wrap in ``` for examples)", section, name)
			}
		}
	}
	return nil
}

// isFenceMarker mirrors internal/tasks.isFenceMarker — column-0 ```
// opener/closer with optional language tag, rejecting 4+ backticks.
func isFenceMarker(line string) bool {
	if !strings.HasPrefix(line, "```") {
		return false
	}
	rest := line[3:]
	return !strings.HasPrefix(rest, "`")
}

// validateDependencySlugs rejects entries the on-disk parser would
// later refuse (codex iter-1 P1). The internal parser disallows quoted
// strings and empty entries; the writer does not double-check, so a
// bogus `depends_on=["foo"]` would silently land on disk and brick the
// next Read with ErrInvalidTask.
func validateDependencySlugs(deps []string) error {
	for _, d := range deps {
		if d == "" {
			return fmt.Errorf("depends_on has empty entry")
		}
		if strings.ContainsAny(d, `"'`) {
			return fmt.Errorf("depends_on entry %q must be a bare slug (no quotes)", d)
		}
		if err := state.ValidateSlug(d); err != nil {
			return fmt.Errorf("depends_on entry %q: %w", d, err)
		}
	}
	return nil
}

// ---------- fleet tasks add ----------

type tasksAddOpts struct {
	project   string
	slug      string
	priority  string
	dependsOn []string
	spec      string
	spawnedBy string
	status    string
}

func newTasksAddCmd() *cobra.Command {
	opts := &tasksAddOpts{}
	cmd := &cobra.Command{
		Use:   "add [<slug-or-spec>]",
		Short: "Add a new task to the per-project tasks.md",
		Long: `add appends a task block to tasks.md. The positional argument is
either a short slug (` + "`<short-desc>`" + `) the CLI will postfix with a
random 4-hex suffix, OR a spec body whose first line is kebab-cased to
derive the short. Pass --slug to override; pass --spec to provide the
spec body when the positional is a slug.

Defaults: status=todo, priority=P2, spawned_by=user. created/updated
are stamped to time.Now().UTC().`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			positional := ""
			if len(args) == 1 {
				positional = args[0]
			}
			return runTasksAdd(opts, positional, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&opts.project, "project", "", "project name (default: cwd basename)")
	cmd.Flags().StringVar(&opts.slug, "slug", "", "explicit slug (`<short>` gets a 4hex suffix; `<short>-<4hex>` used as-is)")
	cmd.Flags().StringVar(&opts.priority, "priority", "P2", "priority: P0|P1|P2|P3")
	cmd.Flags().StringSliceVar(&opts.dependsOn, "depends-on", nil, "comma-separated list of dependency slugs")
	cmd.Flags().StringVar(&opts.spec, "spec", "", "spec body (markdown; defaults to positional arg if positional isn't a slug)")
	cmd.Flags().StringVar(&opts.spawnedBy, "spawned-by", "user", "who spawned this task: user | <agent-slug>")
	cmd.Flags().StringVar(&opts.status, "status", string(tasks.StatusTodo), "initial status: todo|ready|in-progress|in-review|done|blocked|abandoned")
	return cmd
}

func runTasksAdd(opts *tasksAddOpts, positional string, stdout io.Writer) error {
	if _, err := state.Bootstrap(); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	project, err := resolveProject(opts.project)
	if err != nil {
		return err
	}

	// Resolve slug + spec from positional/flag inputs. Three shapes:
	//   1. --slug X              → slug=X (CLI may add 4hex suffix)
	//   2. positional looks like slug → treat as --slug
	//   3. positional or --spec  → spec body
	slug := opts.slug
	spec := opts.spec
	if positional != "" {
		// If the positional looks like a one-line slug AND --spec was
		// passed, treat positional as --slug. Otherwise treat positional
		// as spec body. Heuristic: a slug is single-line, no spaces,
		// only [a-z0-9._-] chars. Anything else is a spec.
		if slug == "" && isLikelySlug(positional) {
			slug = positional
		} else if spec == "" {
			spec = positional
		}
	}
	if spec == "" && slug == "" {
		return fmt.Errorf("tasks add: provide a slug, --spec, or a positional spec body")
	}
	if err := validateSectionBody("spec", spec); err != nil {
		return fmt.Errorf("tasks add: %w", err)
	}
	deps := nonEmptyStrings(opts.dependsOn)
	if err := validateDependencySlugs(deps); err != nil {
		return fmt.Errorf("tasks add: %w", err)
	}
	if err := validateScalarBullet("--spawned-by", opts.spawnedBy); err != nil {
		return fmt.Errorf("tasks add: %w", err)
	}

	now := time.Now().UTC()
	st := tasks.Status(opts.status)
	pri := tasks.Priority(opts.priority)

	return withTasksLock(project, func() error {
		f, path, err := readTasks(project)
		if err != nil {
			return err
		}
		// Also load tasks-archive.md so GenerateSlug avoids picking a
		// 4hex collision with an archived slug — and so an explicit
		// `--slug <full>` value already living in archive is rejected
		// before we write a duplicate (codex iter-4 P2). Archive read
		// is best-effort: a parse failure surfaces as an error so the
		// operator notices the corrupted archive instead of silently
		// recreating slug collisions.
		archiveSlugs, err := archivedSlugList(project)
		if err != nil {
			return fmt.Errorf("read tasks-archive.md: %w", err)
		}
		// Pre-check the user-supplied slug against the archive set.
		if isFullSlug(slug) {
			for _, a := range archiveSlugs {
				if a == slug {
					return fmt.Errorf("slug %q already exists in tasks-archive.md (would block future tasks.Archive on duplicate); pick a different slug", slug)
				}
			}
		}
		all := append(existingSlugs(f), archiveSlugs...)
		finalSlug := tasks.GenerateSlug(slug, spec, all)
		t := &tasks.Task{
			Slug:      finalSlug,
			Status:    st,
			Priority:  pri,
			DependsOn: deps,
			SpawnedBy: opts.spawnedBy,
			Spec:      spec,
			// Acceptance/Notes left empty — operator/worker fills via
			// `fleet tasks note --section=acceptance ...` later.
			Created: now,
			Updated: now,
		}
		if err := f.Add(t); err != nil {
			return fmt.Errorf("add: %w", err)
		}
		if err := tasks.Write(path, f); err != nil {
			return fmt.Errorf("write: %w", err)
		}
		_, _ = fmt.Fprintf(stdout, "added %s (status=%s priority=%s) to %s\n",
			t.Slug, t.Status, t.Priority, path)
		return nil
	})
	// NOTE (codex iter-8 [P1] — deliberately NOT implemented): codex asked
	// to stamp `fleet tasks add` into session_tasks so a filed-then-handed-
	// off todo shows in Next Steps. Rejected with a test-backed reason: the
	// session_tasks writer creates/touches coord-state.json, and the TUI
	// dashboard treats coord-state.json's mtime AS the coord heartbeat
	// (dashboard.go: os.Stat → ModTime → "● active"). Stamping tasks add
	// would make merely FILING a task falsely flip the project to "coord
	// active" — TestFleetE2E_FullWorkflow/cold_start_shows_todo_and_idle
	// enforces the opposite invariant (file a task → still ○ idle, no coord
	// running). The filed task is NOT lost: it lives in tasks.md and the
	// successor sees it in its backlog view. The fix is worse than the gap.
}

// isLikelySlug returns true if s could be interpreted as a slug (no
// whitespace or newlines, only [a-z0-9._-]). Used to decide whether a
// positional like "fix-bug-1234" is the slug vs the spec body.
func isLikelySlug(s string) bool {
	if s == "" || strings.ContainsAny(s, " \t\n\r") {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9':
		case c == '-' || c == '_' || c == '.':
		default:
			return false
		}
	}
	return true
}

// nonEmptyStrings drops empty strings from s. Cobra's StringSlice splits
// "a,,b" into ["a", "", "b"]; we want ["a", "b"].
func nonEmptyStrings(s []string) []string {
	out := make([]string, 0, len(s))
	for _, v := range s {
		v = strings.TrimSpace(v)
		if v != "" {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ---------- fleet tasks list ----------

// listDefaultCap is the per-list default ceiling — active tasks are
// always shown in full, then up to (cap - active_count) most-recent
// done/abandoned rows fill the remainder. Total visible rows =
// max(cap, active_count). Override with --limit / --all.
const listDefaultCap = 10

type tasksListOpts struct {
	project   string
	status    string
	limit     int  // -1 = use default cap (listDefaultCap); 0 = no cap.
	all       bool // alias for --limit 0.
	noArchive bool // skip reading tasks-archive.md.
}

func newTasksListCmd() *cobra.Command {
	opts := &tasksListOpts{limit: -1}
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List tasks (active + recent done/abandoned, recency-merged with archive)",
		Long: `list shows the project's task registry as a recent-activity view.

Default behavior (no flags):
  - All ACTIVE tasks (status in {todo, ready, in-progress, in-review,
    blocked}) shown in priority asc + created asc order.
  - If active count < ` + strconv.Itoa(listDefaultCap) + `, fill remaining slots with the most-
    recent done/abandoned tasks sorted by finished_at desc (falls back
    to updated desc when finished_at is missing on older rows; tie-
    break by created desc).
  - Total visible rows = max(` + strconv.Itoa(listDefaultCap) + `, active_count).
  - Sources merged: tasks.md + tasks-archive.md (skip the archive with
    --no-archive).

Flags:
  --limit N      Override the cap. --limit 0 = unbounded.
  --all          Alias for --limit 0.
  --no-archive   Read tasks.md only; skip tasks-archive.md.
  --status S     Status filter; ignores cap and returns the exact set
                 across both files.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runTasksList(opts, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&opts.project, "project", "", "project name (default: cwd basename)")
	cmd.Flags().StringVar(&opts.status, "status", "", "filter by status (todo|ready|in-progress|in-review|done|blocked|abandoned)")
	cmd.Flags().IntVar(&opts.limit, "limit", -1, "max rows shown (default 10; 0 = no cap; ignored under --status)")
	cmd.Flags().BoolVar(&opts.all, "all", false, "show every task (alias for --limit 0)")
	cmd.Flags().BoolVar(&opts.noArchive, "no-archive", false, "skip tasks-archive.md (default reads it)")
	return cmd
}

// listIsActive distinguishes the always-visible active set from the
// recency-tail backfill set. Active = anything not yet finished;
// done/abandoned populate the tail.
func listIsActive(s tasks.Status) bool {
	switch s {
	case tasks.StatusTodo, tasks.StatusReady, tasks.StatusInProgress,
		tasks.StatusInReview, tasks.StatusBlocked:
		return true
	}
	return false
}

// finishedAtForSort returns the timestamp used to rank a done/abandoned
// task in the recency tail. Order of precedence (per task spec):
//  1. finished_at (new lifecycle bullet)
//  2. updated   (fallback for old rows missing finished_at)
//
// Tie-breaks happen on Created in the caller.
func finishedAtForSort(t *tasks.Task) time.Time {
	if !t.FinishedAt.IsZero() {
		return t.FinishedAt
	}
	return t.Updated
}

// resolveListLimit converts the flag combination into the effective
// cap. Returns -1 to mean "unbounded".
//
//	--all                → -1 (unbounded)
//	--limit 0            → -1 (unbounded)
//	--limit N (N > 0)    → N
//	--limit -1 (default) → listDefaultCap
func resolveListLimit(opts *tasksListOpts) int {
	if opts.all || opts.limit == 0 {
		return -1
	}
	if opts.limit < 0 {
		return listDefaultCap
	}
	return opts.limit
}

func runTasksList(opts *tasksListOpts, stdout io.Writer) error {
	project, err := resolveProject(opts.project)
	if err != nil {
		return err
	}
	f, _, err := readTasks(project)
	if err != nil {
		return err
	}
	live := f.Tasks
	var archive []*tasks.Task
	if !opts.noArchive {
		archive, err = readArchiveTasks(project)
		if err != nil {
			return fmt.Errorf("read tasks-archive.md: %w", err)
		}
	}

	// --status overrides the cap and merges across both files. Operators
	// asking "show every done task" expect to see them all.
	if opts.status != "" {
		liveSlugs := make(map[string]struct{}, len(live))
		filtered := make([]*tasks.Task, 0, len(live)+len(archive))
		for _, t := range live {
			liveSlugs[t.Slug] = struct{}{}
			if string(t.Status) == opts.status {
				filtered = append(filtered, t)
			}
		}
		// Archive entries with the same slug as a live row are dropped
		// — Archive's "tasks.md is source of truth" rule. Without
		// dedup, a retry-recovery window (slug temporarily in BOTH
		// files) would produce duplicate rows in --status output.
		for _, t := range archive {
			if _, dup := liveSlugs[t.Slug]; dup {
				continue
			}
			if string(t.Status) == opts.status {
				filtered = append(filtered, t)
			}
		}
		// Sort done/abandoned by finished_at desc for consistency with
		// the recency tail; non-terminal statuses fall back to priority
		// asc + slug asc (matches the legacy unfiltered ordering).
		switch tasks.Status(opts.status) {
		case tasks.StatusDone, tasks.StatusAbandoned:
			sort.SliceStable(filtered, func(i, j int) bool {
				ti := finishedAtForSort(filtered[i])
				tj := finishedAtForSort(filtered[j])
				if !ti.Equal(tj) {
					return ti.After(tj)
				}
				return filtered[i].Created.After(filtered[j].Created)
			})
		default:
			sort.SliceStable(filtered, func(i, j int) bool {
				if filtered[i].Priority != filtered[j].Priority {
					return filtered[i].Priority < filtered[j].Priority
				}
				return filtered[i].Slug < filtered[j].Slug
			})
		}
		return writeTaskListRows(stdout, filtered)
	}

	// Default recency view: all active first, then most-recent
	// done/abandoned fill the cap.
	//
	// Sort order:
	//   active rows  → priority asc, then created asc
	//   done/abandoned → finished_at desc, fallback updated desc, tie created desc
	activeRows := make([]*tasks.Task, 0, len(live))
	doneRows := make([]*tasks.Task, 0, len(live)+len(archive))
	liveSlugs := make(map[string]struct{}, len(live))
	for _, t := range live {
		liveSlugs[t.Slug] = struct{}{}
		switch {
		case listIsActive(t.Status):
			activeRows = append(activeRows, t)
		case t.Status == tasks.StatusDone || t.Status == tasks.StatusAbandoned:
			doneRows = append(doneRows, t)
		}
	}
	for _, t := range archive {
		if _, dup := liveSlugs[t.Slug]; dup {
			continue // live row wins on retry-recovery overlap
		}
		if t.Status == tasks.StatusDone || t.Status == tasks.StatusAbandoned {
			doneRows = append(doneRows, t)
		}
	}
	sort.SliceStable(activeRows, func(i, j int) bool {
		if activeRows[i].Priority != activeRows[j].Priority {
			return activeRows[i].Priority < activeRows[j].Priority
		}
		return activeRows[i].Created.Before(activeRows[j].Created)
	})
	sort.SliceStable(doneRows, func(i, j int) bool {
		ti := finishedAtForSort(doneRows[i])
		tj := finishedAtForSort(doneRows[j])
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return doneRows[i].Created.After(doneRows[j].Created)
	})

	limit := resolveListLimit(opts)
	rows := make([]*tasks.Task, 0, len(activeRows)+len(doneRows))
	rows = append(rows, activeRows...)
	if limit < 0 {
		// Unbounded: include every done row.
		rows = append(rows, doneRows...)
	} else {
		// Active is never truncated. Total cap = max(cap, len(active));
		// done backfill fills the remaining slots.
		remaining := limit - len(activeRows)
		if remaining > 0 {
			if remaining > len(doneRows) {
				remaining = len(doneRows)
			}
			rows = append(rows, doneRows[:remaining]...)
		}
	}
	return writeTaskListRows(stdout, rows)
}

// writeTaskListRows emits the canonical tabwriter output. Empty rows
// produce the legacy "no tasks" hint so existing operators / scripts
// see the same shape.
func writeTaskListRows(stdout io.Writer, rows []*tasks.Task) error {
	if len(rows) == 0 {
		_, _ = fmt.Fprintf(stdout, "no tasks (run `fleet tasks add` to create one)\n")
		return nil
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	// GEN = dispatch_generation (coord fence token, DESIGN §1); PARKED =
	// the durable dirty-worktree park marker (§4.2), shown as the truncated
	// reason or "-" when unset.
	_, _ = fmt.Fprintln(tw, "SLUG\tSTATUS\tPRIORITY\tAGE\tWORKER_PID\tGEN\tPARKED")
	now := time.Now().UTC()
	for _, t := range rows {
		pid := "-"
		if t.WorkerPID > 0 {
			pid = fmt.Sprintf("%d", t.WorkerPID)
		}
		parked := "-"
		if t.Parked != "" {
			parked = t.Parked
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%s\n",
			t.Slug,
			t.Status,
			t.Priority,
			humanAge(now.Sub(t.Created)),
			pid,
			t.DispatchGeneration,
			parked,
		)
	}
	return tw.Flush()
}

// ---------- fleet tasks show ----------

type tasksShowOpts struct {
	project string
}

func newTasksShowCmd() *cobra.Command {
	opts := &tasksShowOpts{}
	cmd := &cobra.Command{
		Use:   "show <slug>",
		Short: "Print one task's full markdown block",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTasksShow(opts, args[0], cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&opts.project, "project", "", "project name (default: cwd basename)")
	return cmd
}

func runTasksShow(opts *tasksShowOpts, slug string, stdout io.Writer) error {
	project, err := resolveProject(opts.project)
	if err != nil {
		return err
	}
	f, _, err := readTasks(project)
	if err != nil {
		return err
	}
	t, err := f.Get(slug)
	if err != nil {
		return err
	}
	// Render the single-task block via renderTaskMarkdown — a thin
	// mirror of the unexported internal/tasks.renderTask. We can't
	// reach the on-disk renderer without a write, so the format lives
	// here too; tests assert byte-equality against the canonical
	// `## task: <slug>` shape so drift is caught.
	return renderTaskMarkdown(stdout, t)
}

// renderTaskMarkdown writes the canonical task block (matching
// internal/tasks.renderTask) for one task to w. Kept in this file so
// `tasks show` output stays byte-equal to the on-disk block — operators
// piping `fleet tasks show` into a grep/diff get the same shape they
// see if they cat tasks.md.
func renderTaskMarkdown(w io.Writer, t *tasks.Task) error {
	var b strings.Builder
	fmt.Fprintf(&b, "## task: %s\n\n", t.Slug)
	fmt.Fprintf(&b, "- status: %s\n", t.Status)
	fmt.Fprintf(&b, "- priority: %s\n", t.Priority)
	if t.WorkerPID == 0 {
		b.WriteString("- worker_pid: 0\n")
	} else {
		fmt.Fprintf(&b, "- worker_pid: %d\n", t.WorkerPID)
	}
	writeOptionalBullet(&b, "worktree", t.Worktree)
	writeOptionalBullet(&b, "pr_url", t.PRURL)
	writeOptionalBullet(&b, "branch", t.Branch)
	writeOptionalBullet(&b, "created", formatTimeRFC3339(t.Created))
	writeOptionalBullet(&b, "updated", formatTimeRFC3339(t.Updated))
	writeOptionalBullet(&b, "started_at", formatTimeRFC3339(t.StartedAt))
	writeOptionalBullet(&b, "finished_at", formatTimeRFC3339(t.FinishedAt))
	// Keep byte-equal to internal/tasks.renderTask: dispatch_generation
	// always emitted, parked optional.
	fmt.Fprintf(&b, "- dispatch_generation: %d\n", t.DispatchGeneration)
	writeOptionalBullet(&b, "parked", t.Parked)
	fmt.Fprintf(&b, "- depends_on: %s\n", formatDepsList(t.DependsOn))
	writeOptionalBullet(&b, "spawned_by", t.SpawnedBy)
	b.WriteByte('\n')
	writeShowSection(&b, "Spec", t.Spec)
	writeShowSection(&b, "Acceptance", t.Acceptance)
	writeShowSection(&b, "Notes", t.Notes)
	_, err := io.WriteString(w, b.String())
	return err
}

func writeOptionalBullet(b *strings.Builder, key, value string) {
	if value == "" {
		fmt.Fprintf(b, "- %s:\n", key)
		return
	}
	fmt.Fprintf(b, "- %s: %s\n", key, value)
}

func writeShowSection(b *strings.Builder, name, body string) {
	fmt.Fprintf(b, "### %s\n\n", name)
	if body == "" {
		return
	}
	b.WriteString(body)
	b.WriteString("\n\n")
}

func formatTimeRFC3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func formatDepsList(deps []string) string {
	if len(deps) == 0 {
		return "[]"
	}
	cp := append([]string(nil), deps...)
	sort.Strings(cp)
	return "[" + strings.Join(cp, ", ") + "]"
}

// ---------- fleet tasks set ----------

type tasksSetOpts struct {
	project string
}

func newTasksSetCmd() *cobra.Command {
	opts := &tasksSetOpts{}
	cmd := &cobra.Command{
		Use:   "set <slug> <key>=<value>",
		Short: "Mutate one bullet on a task (status, priority, depends_on, ...)",
		Long: `set updates exactly one bullet on a task. Allowed keys mirror the
on-disk grammar:

  status, priority, worker_pid, worktree, pr_url, branch,
  dispatch_generation (int), parked, depends_on (comma-separated),
  spawned_by

Examples:
  fleet tasks set <slug> status=in-progress
  fleet tasks set <slug> depends_on=foo-1234,bar-5678

Validation matches the parser: invalid status/priority/depends_on
syntax returns an error.

created/updated are NOT settable here — the parser owns created and
the writer auto-bumps updated on every mutation.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTasksSet(opts, args[0], args[1], cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&opts.project, "project", "", "project name (default: cwd basename)")
	return cmd
}

func runTasksSet(opts *tasksSetOpts, slug, kv string, stdout io.Writer) error {
	if _, err := state.Bootstrap(); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	project, err := resolveProject(opts.project)
	if err != nil {
		return err
	}
	idx := strings.Index(kv, "=")
	if idx <= 0 {
		return fmt.Errorf("tasks set: expected key=value, got %q", kv)
	}
	key := strings.TrimSpace(kv[:idx])
	value := strings.TrimSpace(kv[idx+1:])

	// codex iter-11 [P1] — `fleet tasks set` deliberately does NOT stamp
	// session_tasks. session_tasks lives in coord-state.json, whose mtime IS
	// the coord tick heartbeat that coordStateFresh() (dispatch_recovery.go)
	// and the TUI dashboard read as proof a coordinator is alive. A CLI write
	// from an operator shell (no live coord) would SPOOF that heartbeat —
	// flipping the project to "● active" and making `fleet dispatch
	// --coord-spawn` refuse for ~coordFreshnessWindow. session_tasks is
	// instead recorded ONLY by the coord TICK (loop.py _record_session_task
	// at every dispatch / reaper-promote seam), which legitimately owns the
	// heartbeat write. A coord-driven status transition is thus already in
	// session_tasks via its dispatch seam; the reader overlays live tasks.md
	// status. (Adjusts the mechanism of design D2 — session_tasks captured by
	// the tick, not the CLI — to eliminate the verified heartbeat-spoof; the
	// feature intent is preserved.)
	return withTasksLock(project, func() error {
		f, path, err := readTasks(project)
		if err != nil {
			return err
		}
		t, err := f.Get(slug)
		if err != nil {
			return err
		}
		// Capture pre-mutation status so a status= transition can trigger
		// the lifecycle stamping rules below. Other key= mutations leave
		// status unchanged and skip the stamping path.
		oldStatus := t.Status
		if err := setTaskField(t, key, value); err != nil {
			return err
		}
		now := time.Now().UTC()
		// Lifecycle transitions stamp started_at / finished_at in the
		// SAME transaction as the status flip — readers don't see a
		// half-stamped row. Rules (per task spec):
		//
		//   to=in-progress AND started_at zero → stamp started_at = now
		//   (started_at is sticky: once set, never cleared, even on
		//   bounce-and-redispatch)
		//
		//   to ∈ {done, abandoned} → stamp finished_at = now
		//   (overwrite on re-finish so reopens reflect the latest)
		//
		//   from ∈ {done, abandoned} AND to ∉ {done, abandoned}
		//     → clear finished_at (reopen / re-dispatch)
		if key == "status" && t.Status != oldStatus {
			stampLifecycleTransition(t, oldStatus, t.Status, now)
		}
		t.Updated = now
		if err := tasks.Write(path, f); err != nil {
			return fmt.Errorf("write: %w", err)
		}
		_, _ = fmt.Fprintf(stdout, "set %s.%s = %s\n", slug, key, value)
		return nil
	})
}

// stampLifecycleTransition applies the lifecycle stamping rules in one
// place so the state machine is auditable. Called only when status
// actually changed (no-op transitions skip stamping — re-running
// `tasks set <slug> status=in-progress` on an already-in-progress task
// must NOT bump started_at; otherwise sticky-on-first semantics break).
//
//	old → new                       effect
//	-------------------------------- -------------------------------------
//	* → in-progress (started zero)  started_at = now
//	* → in-progress (started set)   no-op (sticky)
//	* → done|abandoned              finished_at = now (overwrite OK)
//	done|abandoned → !{done,aband}  finished_at = zero (reopen)
//	other                           no-op
func stampLifecycleTransition(t *tasks.Task, oldStatus, newStatus tasks.Status, now time.Time) {
	if newStatus == tasks.StatusInProgress && t.StartedAt.IsZero() {
		t.StartedAt = now
	}
	if newStatus == tasks.StatusDone || newStatus == tasks.StatusAbandoned {
		t.FinishedAt = now
		return
	}
	// Reopen path: leaving the terminal set clears finished_at so the
	// next finish stamps fresh. started_at stays sticky — the task's
	// "first start" is still the original.
	if oldStatus == tasks.StatusDone || oldStatus == tasks.StatusAbandoned {
		t.FinishedAt = time.Time{}
	}
}

// setTaskField applies key=value to t with the same enum/format
// validation the parser uses. Validation routes through the public
// Status/Priority types (which the package's renderer also checks) so
// no parse-only validators are bypassed.
func setTaskField(t *tasks.Task, key, value string) error {
	switch key {
	case "status":
		s := tasks.Status(value)
		// Force a write+read round-trip via Add-equivalent validation:
		// build a temp file with the new status and let the writer
		// catch invalid values. Cheap: tasks.File.Add re-validates.
		// Simpler: route through a sentinel File so invalid status →
		// ErrInvalidTask out of Add. We don't actually call Add here;
		// renderTask validates on Write, which is what catches us.
		// To keep the error surface friendly + immediate, we use the
		// renderer's invariant: Write fails on invalid status. Easier:
		// inline the same enum check by attempting a renderTask via a
		// temp File with a copy of t.
		t.Status = s
	case "priority":
		t.Priority = tasks.Priority(value)
	case "worker_pid":
		if value == "" || value == "null" || value == "0" {
			t.WorkerPID = 0
			return nil
		}
		// strconv.Atoi rejects trailing garbage (`123abc` → error),
		// matching internal/tasks.setKV's parser exactly. Sscanf
		// would silently consume a numeric prefix and persist a
		// different value than the operator typed (codex iter-3 P2).
		pid, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("worker_pid: %w", err)
		}
		if pid < 0 {
			return fmt.Errorf("worker_pid: must be non-negative, got %d", pid)
		}
		t.WorkerPID = pid
	case "worktree":
		if err := validateScalarBullet(key, value); err != nil {
			return err
		}
		t.Worktree = nullOrValue(value)
	case "pr_url":
		if err := validateScalarBullet(key, value); err != nil {
			return err
		}
		t.PRURL = nullOrValue(value)
	case "branch":
		if err := validateScalarBullet(key, value); err != nil {
			return err
		}
		t.Branch = nullOrValue(value)
	case "dispatch_generation":
		// Coord-owned per-slug fence token (DESIGN §1). Settable here for
		// testing/manual use; the coord increments it on dispatch in a
		// later PR. Empty / "null" → 0. Routes through the same tight
		// [0-9]+ grammar the parser uses so CLI and on-disk validation
		// stay identical (and in lockstep with parse.py).
		if value == "" || value == "null" {
			t.DispatchGeneration = 0
			return nil
		}
		n, err := tasks.ParseDispatchGeneration(value)
		if err != nil {
			return fmt.Errorf("tasks set: %w", err)
		}
		t.DispatchGeneration = n
	case "parked":
		// Durable dirty-worktree park marker (DESIGN §4.2). Free-form
		// scalar; "null" / "" clears it. Settable here for manual use.
		if err := validateScalarBullet(key, value); err != nil {
			return err
		}
		t.Parked = nullOrValue(value)
	case "depends_on":
		deps := parseDependsOn(value)
		if err := validateDependencySlugs(deps); err != nil {
			return fmt.Errorf("depends_on: %w", err)
		}
		t.DependsOn = deps
	case "spawned_by":
		if err := validateScalarBullet(key, value); err != nil {
			return err
		}
		t.SpawnedBy = value
	case "created", "updated":
		return fmt.Errorf("tasks set: %s is not settable (parser owns created; updated bumps automatically)", key)
	default:
		return fmt.Errorf("tasks set: unknown key %q (allowed: status, priority, worker_pid, worktree, pr_url, branch, dispatch_generation, parked, depends_on, spawned_by)", key)
	}
	// Round-trip the file through the writer to catch invalid enum
	// values (Status / Priority) at the same point Add would. We can't
	// easily do that without a write, but tasks.Write itself runs the
	// per-task render which calls validStatus/validPriority. The next
	// caller-side tasks.Write will reject invalid values; check now
	// for fast-fail UX so the error blames the right key.
	if !taskStatusValid(t.Status) {
		return fmt.Errorf("tasks set: invalid status %q (allowed: todo, ready, in-progress, in-review, done, blocked, abandoned)", t.Status)
	}
	if !taskPriorityValid(t.Priority) {
		return fmt.Errorf("tasks set: invalid priority %q (allowed: P0, P1, P2, P3)", t.Priority)
	}
	return nil
}

// taskStatusValid mirrors internal/tasks.validStatus. We re-check here
// (instead of importing the unexported helper) so `tasks set` fails
// fast with a key-targeted error rather than the more generic
// renderer error from tasks.Write.
func taskStatusValid(s tasks.Status) bool {
	switch s {
	case tasks.StatusTodo, tasks.StatusReady, tasks.StatusInProgress,
		tasks.StatusInReview, tasks.StatusDone, tasks.StatusBlocked,
		tasks.StatusAbandoned:
		return true
	}
	return false
}

func taskPriorityValid(p tasks.Priority) bool {
	switch p {
	case tasks.PriorityP0, tasks.PriorityP1, tasks.PriorityP2, tasks.PriorityP3:
		return true
	}
	return false
}

func nullOrValue(v string) string {
	if v == "null" {
		return ""
	}
	return v
}

func parseDependsOn(v string) []string {
	v = strings.TrimSpace(v)
	if v == "" || v == "[]" {
		return nil
	}
	// Accept either bare comma-list (`a,b,c`) or JSON-array (`[a, b]`).
	v = strings.TrimPrefix(v, "[")
	v = strings.TrimSuffix(v, "]")
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ---------- fleet tasks note ----------

type tasksNoteOpts struct {
	project string
	section string
}

func newTasksNoteCmd() *cobra.Command {
	opts := &tasksNoteOpts{}
	cmd := &cobra.Command{
		Use:   "note <slug> <text>",
		Short: "Append text to a task's spec/acceptance/notes section",
		Long: `note appends free-form markdown to a task's section body. Default
section is "notes" (the worker's append-only log). Use --section=spec
or --section=acceptance to amend those instead.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTasksNote(opts, args[0], args[1], cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&opts.project, "project", "", "project name (default: cwd basename)")
	cmd.Flags().StringVar(&opts.section, "section", "notes", "section: spec|acceptance|notes")
	return cmd
}

func runTasksNote(opts *tasksNoteOpts, slug, text string, stdout io.Writer) error {
	if _, err := state.Bootstrap(); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	project, err := resolveProject(opts.project)
	if err != nil {
		return err
	}
	section := strings.ToLower(opts.section)
	switch section {
	case "spec", "acceptance", "notes":
	default:
		return fmt.Errorf("tasks note: --section must be spec|acceptance|notes, got %q", opts.section)
	}
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("tasks note: text is empty")
	}
	if err := validateSectionBody(section, text); err != nil {
		return fmt.Errorf("tasks note: %w", err)
	}

	return withTasksLock(project, func() error {
		f, path, err := readTasks(project)
		if err != nil {
			return err
		}
		t, err := f.Get(slug)
		if err != nil {
			return err
		}
		appendBody := func(existing, addition string) string {
			if existing == "" {
				return addition
			}
			// Two newlines = paragraph separator. Preserves multi-
			// paragraph notes round-trip.
			return existing + "\n\n" + addition
		}
		switch section {
		case "spec":
			t.Spec = appendBody(t.Spec, text)
		case "acceptance":
			t.Acceptance = appendBody(t.Acceptance, text)
		case "notes":
			t.Notes = appendBody(t.Notes, text)
		}
		t.Updated = time.Now().UTC()
		if err := tasks.Write(path, f); err != nil {
			return fmt.Errorf("write: %w", err)
		}
		_, _ = fmt.Fprintf(stdout, "appended to %s.%s\n", slug, section)
		return nil
	})
}

// ---------- fleet tasks archive ----------

type tasksArchiveOpts struct {
	project string
}

func newTasksArchiveCmd() *cobra.Command {
	opts := &tasksArchiveOpts{}
	cmd := &cobra.Command{
		Use:   "archive <slug> [<slug>...]",
		Short: "Move tasks from tasks.md to tasks-archive.md",
		Long: `archive moves the named slugs to tasks-archive.md. Slugs not present
in tasks.md are silently skipped (idempotent).`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTasksArchive(opts, args, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&opts.project, "project", "", "project name (default: cwd basename)")
	return cmd
}

func runTasksArchive(opts *tasksArchiveOpts, slugs []string, stdout io.Writer) error {
	if _, err := state.Bootstrap(); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	project, err := resolveProject(opts.project)
	if err != nil {
		return err
	}
	// Snapshot the live tasks set BEFORE the lock-free Archive call.
	// We diff before/after so the success message reports the slugs
	// that actually moved (codex iter-5 P3). tasks.Archive itself
	// silently skips slugs already absent — operators couldn't
	// otherwise tell whether anything happened.
	before, _, err := readTasks(project)
	if err != nil {
		return fmt.Errorf("read tasks: %w", err)
	}
	beforeSet := make(map[string]struct{}, len(before.Tasks))
	for _, t := range before.Tasks {
		beforeSet[t.Slug] = struct{}{}
	}
	want := make(map[string]struct{}, len(slugs))
	for _, s := range slugs {
		want[s] = struct{}{}
	}
	moved := 0
	skippedAbsent := make([]string, 0)
	for s := range want {
		if _, present := beforeSet[s]; present {
			moved++
		} else {
			skippedAbsent = append(skippedAbsent, s)
		}
	}
	// tasks.Archive takes its own lock; don't double-lock here.
	if err := tasks.Archive(project, slugs); err != nil {
		return fmt.Errorf("archive: %w", err)
	}
	_, _ = fmt.Fprintf(stdout, "archived %d slug(s)\n", moved)
	if len(skippedAbsent) > 0 {
		sort.Strings(skippedAbsent)
		_, _ = fmt.Fprintf(stdout, "  skipped (not in tasks.md): %s\n",
			strings.Join(skippedAbsent, ", "))
	}
	return nil
}

// ---------- fleet tasks promote ----------

type tasksPromoteOpts struct {
	project string
}

func newTasksPromoteCmd() *cobra.Command {
	opts := &tasksPromoteOpts{}
	cmd := &cobra.Command{
		Use:   "promote <slug>",
		Short: "Promote a todo task to ready (eligible for coordinator dispatch)",
		Long: `promote flips status: todo → ready. Worker-filed tasks default to
todo + spawned_by=<agent-slug> precisely so the coordinator skips them
until an operator promotes (PLAN failure-mode "worker fires recursive
bug-files"). This is the operator-side gate.

If the task is already ready, promote is a no-op and prints a notice.
Other statuses are rejected (use ` + "`fleet tasks set <slug> status=...`" + `
to override explicitly).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTasksPromote(opts, args[0], cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&opts.project, "project", "", "project name (default: cwd basename)")
	return cmd
}

func runTasksPromote(opts *tasksPromoteOpts, slug string, stdout io.Writer) error {
	if _, err := state.Bootstrap(); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	project, err := resolveProject(opts.project)
	if err != nil {
		return err
	}
	// codex iter-11 [P1] — like `fleet tasks set`, promote does NOT stamp
	// session_tasks: a CLI write to coord-state.json spoofs the coord tick
	// heartbeat (coordStateFresh / dashboard read its mtime), which on an
	// idle project falsely shows "● active" and can make `fleet dispatch
	// --coord-spawn` refuse. The coord TICK records session_tasks at its
	// reaper-promote + dispatch seams (loop.py _record_session_task), which
	// legitimately owns the heartbeat write — so a coord-driven promote is
	// captured there. (Adjusts design D2's mechanism to remove the verified
	// heartbeat-spoof while preserving the session-scoped Next Steps intent.)
	return withTasksLock(project, func() error {
		f, path, err := readTasks(project)
		if err != nil {
			return err
		}
		t, err := f.Get(slug)
		if err != nil {
			return err
		}
		switch t.Status {
		case tasks.StatusTodo:
			t.Status = tasks.StatusReady
			t.Updated = time.Now().UTC()
			if err := tasks.Write(path, f); err != nil {
				return fmt.Errorf("write: %w", err)
			}
			_, _ = fmt.Fprintf(stdout, "promoted %s: todo → ready\n", slug)
			return nil
		case tasks.StatusReady:
			_, _ = fmt.Fprintf(stdout, "%s already ready (no-op)\n", slug)
			return nil
		default:
			return fmt.Errorf("tasks promote: %s has status=%s — only todo→ready is allowed (use `fleet tasks set` for other transitions)", slug, t.Status)
		}
	})
}
