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

// resolveProject normalizes --project. Empty input falls back to the
// cwd's last-two-segments tag (matching the TUI's [d]-picker default
// so `fleet dispatch` and `fleet tasks` agree on the default project).
// The result is validated so callers don't have to.
func resolveProject(p string) (string, error) {
	if p == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("getwd: %w", err)
		}
		p = tui.ProjectTag(cwd)
	}
	if err := state.ValidateProjectName(p); err != nil {
		return "", fmt.Errorf("--project: %w", err)
	}
	return p, nil
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

type tasksListOpts struct {
	project string
	status  string
}

func newTasksListCmd() *cobra.Command {
	opts := &tasksListOpts{}
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List tasks in the per-project tasks.md",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runTasksList(opts, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&opts.project, "project", "", "project name (default: cwd basename)")
	cmd.Flags().StringVar(&opts.status, "status", "", "filter by status (todo|ready|in-progress|in-review|done|blocked|abandoned)")
	return cmd
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
	rows := f.Tasks
	if opts.status != "" {
		filtered := make([]*tasks.Task, 0, len(rows))
		for _, t := range rows {
			if string(t.Status) == opts.status {
				filtered = append(filtered, t)
			}
		}
		rows = filtered
	}
	// Sort: priority ascending (P0 first), then slug. Stable so ties
	// preserve file order.
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Priority != rows[j].Priority {
			return rows[i].Priority < rows[j].Priority
		}
		return rows[i].Slug < rows[j].Slug
	})
	if len(rows) == 0 {
		_, _ = fmt.Fprintf(stdout, "no tasks (run `fleet tasks add` to create one)\n")
		return nil
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "SLUG\tSTATUS\tPRIORITY\tAGE\tWORKER_PID")
	now := time.Now().UTC()
	for _, t := range rows {
		pid := "-"
		if t.WorkerPID > 0 {
			pid = fmt.Sprintf("%d", t.WorkerPID)
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			t.Slug,
			t.Status,
			t.Priority,
			humanAge(now.Sub(t.Created)),
			pid,
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
	// Render the single-task block by writing a tasks.File with just
	// this one task and stripping the frontmatter. Avoids drift between
	// `tasks show` output and the on-disk shape.
	one := &tasks.File{Tasks: []*tasks.Task{t}}
	// We can't reach the unexported render() — read-back our own write
	// would require disk. Instead, manually render the canonical shape
	// (a thin mirror of internal/tasks.renderTask).
	_ = one
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
  depends_on (comma-separated), spawned_by

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

	return withTasksLock(project, func() error {
		f, path, err := readTasks(project)
		if err != nil {
			return err
		}
		t, err := f.Get(slug)
		if err != nil {
			return err
		}
		if err := setTaskField(t, key, value); err != nil {
			return err
		}
		t.Updated = time.Now().UTC()
		if err := tasks.Write(path, f); err != nil {
			return fmt.Errorf("write: %w", err)
		}
		_, _ = fmt.Fprintf(stdout, "set %s.%s = %s\n", slug, key, value)
		return nil
	})
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
		return fmt.Errorf("tasks set: unknown key %q (allowed: status, priority, worker_pid, worktree, pr_url, branch, depends_on, spawned_by)", key)
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
