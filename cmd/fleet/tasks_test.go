package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tasks"
)

// setupTasksHome bootstraps FLEET_HOME and chdir to a tmpdir so
// resolveProject's cwd-derived default produces a stable name. Returns
// (fleetHome, project) — project is the deterministic name a `fleet
// tasks <subcmd>` (no --project) would resolve to in this dir.
func setupTasksHome(t *testing.T) (string, string) {
	t.Helper()
	fleetHome := t.TempDir()
	t.Setenv("FLEET_HOME", fleetHome)
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	// chdir to a stable, lowercase, tag-safe directory so the default
	// --project resolves to something we can predict and pass to
	// readTasks for assertions.
	workdir := filepath.Join(t.TempDir(), "alpha")
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatalf("mkdir workdir: %v", err)
	}
	cwdBefore, _ := os.Getwd()
	if err := os.Chdir(workdir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwdBefore) })
	// resolveProject runs ProjectTag against cwd; we don't predict it
	// here — we just pass --project=alpha explicitly in tests.
	return fleetHome, "alpha"
}

// TestResolveProject_FromWorkerWorktree regresses codex iter-7 P1.
// When a worker invokes a `fleet <subcommand>` from inside its
// worktree (~/.fleet/projects/<project>/worktrees/<slug>), the
// default --project must resolve to <project>, NOT to the
// parent-basename ProjectTag of the worktree dir (which would be
// "worktrees-<slug>" and silently misroute mutations into a phantom
// project tree the operator never reads).
func TestResolveProject_FromWorkerWorktree(t *testing.T) {
	fleetHome := t.TempDir()
	t.Setenv("FLEET_HOME", fleetHome)
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	wt, err := state.WorktreePath("myproject", "feature-1234")
	if err != nil {
		t.Fatalf("WorktreePath: %v", err)
	}
	wt = strings.TrimSuffix(wt, string(filepath.Separator))
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	cwdBefore, _ := os.Getwd()
	if err := os.Chdir(wt); err != nil {
		t.Fatalf("chdir worktree: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwdBefore) })
	got, err := resolveProject("")
	if err != nil {
		t.Fatalf("resolveProject: %v", err)
	}
	if got != "myproject" {
		t.Errorf("resolveProject from worktree=%q; want %q", got, "myproject")
	}
}

// TestResolveProject_FromOperatorCwd preserves the existing
// behavior: outside a fleet-managed worktree, the cwd's parent-
// basename ProjectTag wins (matches `fleet dispatch` default).
func TestResolveProject_FromOperatorCwd(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())
	workdir := filepath.Join(t.TempDir(), "myrepo")
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cwdBefore, _ := os.Getwd()
	if err := os.Chdir(workdir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwdBefore) })
	got, err := resolveProject("")
	if err != nil {
		t.Fatalf("resolveProject: %v", err)
	}
	// We don't pin the exact value (it depends on tmp-parent name),
	// but it must not be a worktree-extracted project name and must
	// pass ValidateProjectName.
	if got == "" {
		t.Error("resolveProject empty")
	}
	if strings.HasPrefix(got, "worktrees-") {
		t.Errorf("resolveProject=%q; should not look like a worktree-derived tag", got)
	}
}

// TestTasksAdd_DefaultsAndSlug exercises the happy path: add with a
// spec body produces a task with derived slug, status=todo, and the
// timestamps get stamped.
func TestTasksAdd_DefaultsAndSlug(t *testing.T) {
	fleetHome, project := setupTasksHome(t)

	out := &bytes.Buffer{}
	if err := runTasksAdd(&tasksAddOpts{
		project:   project,
		priority:  "P1",
		spec:      "Write the readme",
		spawnedBy: "user",
		status:    "todo",
	}, "", out); err != nil {
		t.Fatalf("add: %v\n%s", err, out.String())
	}

	dir := filepath.Join(fleetHome, "projects", project)
	f, err := tasks.Read(filepath.Join(dir, "tasks.md"))
	if err != nil {
		t.Fatalf("read tasks.md: %v", err)
	}
	if len(f.Tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(f.Tasks))
	}
	tk := f.Tasks[0]
	if !strings.HasPrefix(tk.Slug, "write-the-readme-") {
		t.Errorf("slug not derived from spec: %q", tk.Slug)
	}
	if tk.Status != tasks.StatusTodo || tk.Priority != tasks.PriorityP1 {
		t.Errorf("wrong defaults: status=%q priority=%q", tk.Status, tk.Priority)
	}
	if tk.Created.IsZero() || tk.Updated.IsZero() {
		t.Error("created/updated not stamped")
	}
	if tk.Spec != "Write the readme" {
		t.Errorf("spec not preserved: %q", tk.Spec)
	}
}

// TestTasksAdd_PositionalSlug — when the positional looks like a slug,
// it's promoted to --slug rather than treated as the spec body. This
// matches the operator's natural mental model: `fleet tasks add
// fix-bug --spec "..."` reads as "add a task with this slug".
func TestTasksAdd_PositionalSlug(t *testing.T) {
	_, project := setupTasksHome(t)

	out := &bytes.Buffer{}
	if err := runTasksAdd(&tasksAddOpts{
		project:   project,
		priority:  "P2",
		spec:      "fix the bug",
		spawnedBy: "user",
		status:    "todo",
	}, "fix-bug", out); err != nil {
		t.Fatalf("add: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "fix-bug-") {
		t.Errorf("output should reference fix-bug-XXXX slug:\n%s", out.String())
	}
}

// TestTasksList_Filters list with --status filter narrows rows.
func TestTasksList_Filters(t *testing.T) {
	_, project := setupTasksHome(t)

	specs := []string{"first task", "second task", "third task"}
	slugs := make([]string, 0, len(specs))
	for _, spec := range specs {
		out := &bytes.Buffer{}
		if err := runTasksAdd(&tasksAddOpts{
			project:   project,
			priority:  "P2",
			spec:      spec,
			spawnedBy: "user",
			status:    "todo",
		}, "", out); err != nil {
			t.Fatalf("add %q: %v", spec, err)
		}
		// Pull the auto-derived slug out of stdout. Format is:
		//   "added <slug> (status=... priority=...) to <path>".
		line := strings.TrimSpace(out.String())
		parts := strings.Fields(line)
		if len(parts) < 2 || parts[0] != "added" {
			t.Fatalf("add output unrecognized: %q", line)
		}
		slugs = append(slugs, parts[1])
	}
	// Flip the second task to ready so unfiltered listing shows a
	// mixed status set, --status=todo trims to two rows, and
	// --status=ready trims to one. Without the flip both branches
	// would pass even if the filter did nothing.
	if err := runTasksSet(&tasksSetOpts{project: project}, slugs[1], "status=ready", &bytes.Buffer{}); err != nil {
		t.Fatalf("set ready: %v", err)
	}

	// rowsByStatus counts data rows (skipping header) whose row has
	// the expected slug. Output uses tabwriter with space padding,
	// so we anchor on slug + a status keyword on the same line.
	rowsContainingSlug := func(buf string, slug string) int {
		c := 0
		for _, line := range strings.Split(buf, "\n") {
			if strings.Contains(line, slug) {
				c++
			}
		}
		return c
	}

	out := &bytes.Buffer{}
	if err := runTasksList(&tasksListOpts{project: project}, out); err != nil {
		t.Fatalf("list: %v", err)
	}
	all := out.String()
	for _, s := range slugs {
		if rowsContainingSlug(all, s) != 1 {
			t.Errorf("unfiltered list missing or duplicating %q: %s", s, all)
		}
	}

	out.Reset()
	if err := runTasksList(&tasksListOpts{project: project, status: "todo"}, out); err != nil {
		t.Fatalf("list --status=todo: %v", err)
	}
	todoOut := out.String()
	if rowsContainingSlug(todoOut, slugs[0]) != 1 || rowsContainingSlug(todoOut, slugs[2]) != 1 {
		t.Errorf("--status=todo missing first/third todo rows: %s", todoOut)
	}
	if strings.Contains(todoOut, slugs[1]) {
		t.Errorf("--status=todo leaked the ready slug %q: %s", slugs[1], todoOut)
	}

	out.Reset()
	if err := runTasksList(&tasksListOpts{project: project, status: "ready"}, out); err != nil {
		t.Fatalf("list --status=ready: %v", err)
	}
	readyOut := out.String()
	if rowsContainingSlug(readyOut, slugs[1]) != 1 {
		t.Errorf("--status=ready missing the ready row: %s", readyOut)
	}
	if strings.Contains(readyOut, slugs[0]) || strings.Contains(readyOut, slugs[2]) {
		t.Errorf("--status=ready leaked todo rows: %s", readyOut)
	}
}

// TestTasksShow_RoundTrips_Markdown verifies show emits the same
// canonical block shape that lives on disk for that task.
func TestTasksShow_RoundTrips_Markdown(t *testing.T) {
	_, project := setupTasksHome(t)

	addOut := &bytes.Buffer{}
	if err := runTasksAdd(&tasksAddOpts{
		project:   project,
		slug:      "alpha-task",
		priority:  "P2",
		spec:      "do alpha",
		spawnedBy: "user",
		status:    "todo",
	}, "", addOut); err != nil {
		t.Fatalf("add: %v", err)
	}
	// The slug got a 4hex suffix; pull it out of the add output.
	parts := strings.Fields(addOut.String())
	if len(parts) < 2 {
		t.Fatalf("unexpected add output: %s", addOut.String())
	}
	slug := parts[1]

	showOut := &bytes.Buffer{}
	if err := runTasksShow(&tasksShowOpts{project: project}, slug, showOut); err != nil {
		t.Fatalf("show: %v", err)
	}
	if !strings.Contains(showOut.String(), "## task: "+slug) {
		t.Errorf("show output missing task header: %s", showOut.String())
	}
	if !strings.Contains(showOut.String(), "### Spec") {
		t.Errorf("show output missing Spec section: %s", showOut.String())
	}
}

// TestTasksSet_StatusValidation rejects unknown status values; valid
// transitions persist.
func TestTasksSet_StatusValidation(t *testing.T) {
	_, project := setupTasksHome(t)
	addOut := &bytes.Buffer{}
	if err := runTasksAdd(&tasksAddOpts{
		project: project, slug: "set-test", priority: "P2",
		spec: "x", spawnedBy: "user", status: "todo",
	}, "", addOut); err != nil {
		t.Fatalf("add: %v", err)
	}
	parts := strings.Fields(addOut.String())
	slug := parts[1]

	// Bad status.
	if err := runTasksSet(&tasksSetOpts{project: project}, slug, "status=bogus", &bytes.Buffer{}); err == nil {
		t.Error("expected error on invalid status, got nil")
	}
	// Good status.
	out := &bytes.Buffer{}
	if err := runTasksSet(&tasksSetOpts{project: project}, slug, "status=ready", out); err != nil {
		t.Errorf("valid status rejected: %v", err)
	}
}

// TestTasksSet_RejectsCreatedUpdated — created/updated are NOT
// operator-settable. Catches a regression where a sloppy implementation
// might write through to those fields.
func TestTasksSet_RejectsCreatedUpdated(t *testing.T) {
	_, project := setupTasksHome(t)
	addOut := &bytes.Buffer{}
	if err := runTasksAdd(&tasksAddOpts{
		project: project, slug: "fixed-test", priority: "P2",
		spec: "y", spawnedBy: "user", status: "todo",
	}, "", addOut); err != nil {
		t.Fatalf("add: %v", err)
	}
	parts := strings.Fields(addOut.String())
	slug := parts[1]
	if err := runTasksSet(&tasksSetOpts{project: project}, slug, "created=2030-01-01T00:00:00Z", &bytes.Buffer{}); err == nil {
		t.Error("expected error on created= mutation")
	}
}

// TestTasksNote_AppendsToSection ensures multi-paragraph notes
// accumulate (paragraph-separator semantics, not overwrite).
func TestTasksNote_AppendsToSection(t *testing.T) {
	_, project := setupTasksHome(t)
	addOut := &bytes.Buffer{}
	if err := runTasksAdd(&tasksAddOpts{
		project: project, slug: "note-test", priority: "P2",
		spec: "n", spawnedBy: "user", status: "todo",
	}, "", addOut); err != nil {
		t.Fatalf("add: %v", err)
	}
	parts := strings.Fields(addOut.String())
	slug := parts[1]

	if err := runTasksNote(&tasksNoteOpts{project: project, section: "notes"}, slug, "first", &bytes.Buffer{}); err != nil {
		t.Fatalf("first note: %v", err)
	}
	if err := runTasksNote(&tasksNoteOpts{project: project, section: "notes"}, slug, "second", &bytes.Buffer{}); err != nil {
		t.Fatalf("second note: %v", err)
	}

	showOut := &bytes.Buffer{}
	if err := runTasksShow(&tasksShowOpts{project: project}, slug, showOut); err != nil {
		t.Fatalf("show: %v", err)
	}
	if !strings.Contains(showOut.String(), "first") || !strings.Contains(showOut.String(), "second") {
		t.Errorf("notes did not accumulate: %s", showOut.String())
	}
}

// TestTasksPromote_TodoToReady — base promotion path. Other
// transitions are rejected (operator must use `set` to override).
func TestTasksPromote_TodoToReady(t *testing.T) {
	_, project := setupTasksHome(t)
	addOut := &bytes.Buffer{}
	if err := runTasksAdd(&tasksAddOpts{
		project: project, slug: "promote-test", priority: "P2",
		spec: "p", spawnedBy: "user", status: "todo",
	}, "", addOut); err != nil {
		t.Fatalf("add: %v", err)
	}
	parts := strings.Fields(addOut.String())
	slug := parts[1]

	out := &bytes.Buffer{}
	if err := runTasksPromote(&tasksPromoteOpts{project: project}, slug, out); err != nil {
		t.Fatalf("promote: %v", err)
	}
	if !strings.Contains(out.String(), "todo → ready") {
		t.Errorf("promote output missing transition: %s", out.String())
	}

	// Now status=in-progress; promote should refuse.
	if err := runTasksSet(&tasksSetOpts{project: project}, slug, "status=in-progress", &bytes.Buffer{}); err != nil {
		t.Fatalf("set status: %v", err)
	}
	if err := runTasksPromote(&tasksPromoteOpts{project: project}, slug, &bytes.Buffer{}); err == nil {
		t.Error("expected promote to refuse non-todo status")
	}
}

// TestTasksArchive_MovesToArchiveFile — happy path archive sends slugs
// to tasks-archive.md and removes them from tasks.md.
func TestTasksArchive_MovesToArchiveFile(t *testing.T) {
	fleetHome, project := setupTasksHome(t)
	addOut := &bytes.Buffer{}
	if err := runTasksAdd(&tasksAddOpts{
		project: project, slug: "archive-me", priority: "P2",
		spec: "a", spawnedBy: "user", status: "todo",
	}, "", addOut); err != nil {
		t.Fatalf("add: %v", err)
	}
	parts := strings.Fields(addOut.String())
	slug := parts[1]

	out := &bytes.Buffer{}
	if err := runTasksArchive(&tasksArchiveOpts{project: project}, []string{slug}, out); err != nil {
		t.Fatalf("archive: %v", err)
	}
	dir := filepath.Join(fleetHome, "projects", project)
	cur, err := tasks.Read(filepath.Join(dir, "tasks.md"))
	if err != nil {
		t.Fatalf("read current: %v", err)
	}
	if len(cur.Tasks) != 0 {
		t.Errorf("tasks.md should be empty after archive, got %d tasks", len(cur.Tasks))
	}
	arc, err := tasks.Read(filepath.Join(dir, "tasks-archive.md"))
	if err != nil {
		t.Fatalf("read archive: %v", err)
	}
	if len(arc.Tasks) != 1 || arc.Tasks[0].Slug != slug {
		t.Errorf("archive should contain %s, got %+v", slug, arc.Tasks)
	}
}

// TestTasksArchive_ReportsActualMoved — codex iter-5 P3: success
// message must reflect slugs actually moved, not the requested count.
func TestTasksArchive_ReportsActualMoved(t *testing.T) {
	_, project := setupTasksHome(t)
	addOut := &bytes.Buffer{}
	if err := runTasksAdd(&tasksAddOpts{
		project: project, slug: "real-archive", priority: "P2",
		spec: "x", spawnedBy: "user", status: "todo",
	}, "", addOut); err != nil {
		t.Fatalf("add: %v", err)
	}
	parts := strings.Fields(addOut.String())
	realSlug := parts[1]

	out := &bytes.Buffer{}
	if err := runTasksArchive(&tasksArchiveOpts{project: project},
		[]string{realSlug, "ghost-9999"}, out); err != nil {
		t.Fatalf("archive: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "archived 1 slug(s)") {
		t.Errorf("count should be 1 (only real slug present), got: %s", got)
	}
	if !strings.Contains(got, "ghost-9999") {
		t.Errorf("missing slug should be reported as skipped: %s", got)
	}
}

// TestResolveProject_RejectsInvalid validates the project guardrail
// short-circuits a bogus name before any disk write.
func TestResolveProject_RejectsInvalid(t *testing.T) {
	if _, err := resolveProject("Invalid/Name"); err == nil {
		t.Error("resolveProject should reject names with path separators")
	}
}

// TestTasksAdd_RejectsUnfencedH2InSpec — codex iter-1 P1 regression:
// a column-0 `## ` in the spec body would self-corrupt tasks.md
// (next Read splits the task at the bogus header). Validation must
// fire on add (not just at parse time).
func TestTasksAdd_RejectsUnfencedH2InSpec(t *testing.T) {
	_, project := setupTasksHome(t)
	err := runTasksAdd(&tasksAddOpts{
		project:  project,
		slug:     "h2-test",
		priority: "P2",
		spec:     "good line\n\n## evil header inside spec\nmore text",
		status:   "todo",
	}, "", &bytes.Buffer{})
	if err == nil {
		t.Error("expected error on unfenced ## in spec body")
	}
}

// TestTasksNote_RejectsReservedH3 — codex iter-1 P1 regression: a
// reserved `### Acceptance` inside a Notes append would terminate the
// Notes section on next Read.
func TestTasksNote_RejectsReservedH3(t *testing.T) {
	_, project := setupTasksHome(t)
	addOut := &bytes.Buffer{}
	if err := runTasksAdd(&tasksAddOpts{
		project: project, slug: "reserved-h3", priority: "P2",
		spec: "x", spawnedBy: "user", status: "todo",
	}, "", addOut); err != nil {
		t.Fatalf("add: %v", err)
	}
	parts := strings.Fields(addOut.String())
	slug := parts[1]
	err := runTasksNote(&tasksNoteOpts{project: project, section: "notes"},
		slug, "real note\n\n### Acceptance\nfake follow-up criteria", &bytes.Buffer{})
	if err == nil {
		t.Error("expected error on reserved ### Acceptance in notes append")
	}
}

// TestTasksAdd_RejectsQuotedDeps — codex iter-1 P1: the on-disk parser
// rejects depends_on entries with quotes (parseDeps), but the writer
// wouldn't catch them. Validate at the CLI boundary so the file never
// becomes unreadable.
func TestTasksAdd_RejectsQuotedDeps(t *testing.T) {
	_, project := setupTasksHome(t)
	err := runTasksAdd(&tasksAddOpts{
		project:   project,
		slug:      "deps-test",
		priority:  "P2",
		spec:      "x",
		dependsOn: []string{`"foo-1234"`},
		status:    "todo",
	}, "", &bytes.Buffer{})
	if err == nil {
		t.Error("expected error on quoted depends_on entry")
	}
}

// TestTasksSet_RejectsQuotedDeps — same rule on the set path.
func TestTasksSet_RejectsQuotedDeps(t *testing.T) {
	_, project := setupTasksHome(t)
	addOut := &bytes.Buffer{}
	if err := runTasksAdd(&tasksAddOpts{
		project: project, slug: "deps-set-test", priority: "P2",
		spec: "x", spawnedBy: "user", status: "todo",
	}, "", addOut); err != nil {
		t.Fatalf("add: %v", err)
	}
	parts := strings.Fields(addOut.String())
	slug := parts[1]
	err := runTasksSet(&tasksSetOpts{project: project}, slug, `depends_on=["foo"]`, &bytes.Buffer{})
	if err == nil {
		t.Error("expected error on quoted depends_on via set")
	}
}

// TestTasksSet_RejectsNonNumericPID — codex iter-3 P2: Sscanf accepts
// `123abc` and stores 123. strconv.Atoi rejects trailing garbage so
// the on-disk value matches what the operator typed (or fails fast).
func TestTasksSet_RejectsNonNumericPID(t *testing.T) {
	_, project := setupTasksHome(t)
	addOut := &bytes.Buffer{}
	if err := runTasksAdd(&tasksAddOpts{
		project: project, slug: "pid-test", priority: "P2",
		spec: "x", spawnedBy: "user", status: "todo",
	}, "", addOut); err != nil {
		t.Fatalf("add: %v", err)
	}
	parts := strings.Fields(addOut.String())
	slug := parts[1]
	if err := runTasksSet(&tasksSetOpts{project: project}, slug, "worker_pid=123abc", &bytes.Buffer{}); err == nil {
		t.Error("expected error on worker_pid with trailing garbage")
	}
	if err := runTasksSet(&tasksSetOpts{project: project}, slug, "worker_pid=-5", &bytes.Buffer{}); err == nil {
		t.Error("expected error on negative worker_pid")
	}
}

// TestTasksAdd_RejectsArchivedSlug — codex iter-4 P2: explicit --slug
// matching an archived slug must fail. Otherwise re-archive later
// returns ErrDuplicateSlug and the task lifecycle gets stuck.
func TestTasksAdd_RejectsArchivedSlug(t *testing.T) {
	_, project := setupTasksHome(t)

	addOut := &bytes.Buffer{}
	if err := runTasksAdd(&tasksAddOpts{
		project: project, slug: "ghost-1234", priority: "P2",
		spec: "first", spawnedBy: "user", status: "todo",
	}, "", addOut); err != nil {
		t.Fatalf("add: %v", err)
	}
	parts := strings.Fields(addOut.String())
	slug := parts[1]
	if err := runTasksArchive(&tasksArchiveOpts{project: project}, []string{slug}, &bytes.Buffer{}); err != nil {
		t.Fatalf("archive: %v", err)
	}
	// Try to reuse the SAME full slug.
	err := runTasksAdd(&tasksAddOpts{
		project: project, slug: slug, priority: "P2",
		spec: "second", spawnedBy: "user", status: "todo",
	}, "", &bytes.Buffer{})
	if err == nil {
		t.Error("expected error reusing archived slug")
	}
}

// TestTasksAdd_RejectsMultilineSpawnedBy — codex iter-4 P2: a newline
// in --spawned-by would corrupt the task block on round-trip.
func TestTasksAdd_RejectsMultilineSpawnedBy(t *testing.T) {
	_, project := setupTasksHome(t)
	err := runTasksAdd(&tasksAddOpts{
		project: project, slug: "multi-spawn", priority: "P2",
		spec: "x", spawnedBy: "user\nrogue line", status: "todo",
	}, "", &bytes.Buffer{})
	if err == nil {
		t.Error("expected error on multiline --spawned-by")
	}
}

// TestTasksSet_RejectsMultilineScalars — codex iter-4 P2: scalar
// bullet mutations must reject newlines.
func TestTasksSet_RejectsMultilineScalars(t *testing.T) {
	_, project := setupTasksHome(t)
	addOut := &bytes.Buffer{}
	if err := runTasksAdd(&tasksAddOpts{
		project: project, slug: "scalar-mut", priority: "P2",
		spec: "x", spawnedBy: "user", status: "todo",
	}, "", addOut); err != nil {
		t.Fatalf("add: %v", err)
	}
	parts := strings.Fields(addOut.String())
	slug := parts[1]
	for _, kv := range []string{
		"pr_url=line1\nline2",
		"branch=foo\nbar",
		"worktree=/tmp/a\n/tmp/b",
		"spawned_by=alice\neve",
	} {
		if err := runTasksSet(&tasksSetOpts{project: project}, slug, kv, &bytes.Buffer{}); err == nil {
			t.Errorf("expected error on multiline value %q", kv)
		}
	}
}

// TestTasksAdd_NeedsSlugOrSpec — empty input refuses up front rather
// than producing an unsalvageable empty task block.
func TestTasksAdd_NeedsSlugOrSpec(t *testing.T) {
	_, project := setupTasksHome(t)
	err := runTasksAdd(&tasksAddOpts{
		project: project, priority: "P2", spawnedBy: "user", status: "todo",
	}, "", &bytes.Buffer{})
	if err == nil {
		t.Error("expected error when slug + spec + positional all empty")
	}
}

// listSeedTask writes a task directly through internal/tasks (bypassing
// runTasksAdd) so the test can pin Status/Created/Updated/StartedAt/
// FinishedAt deterministically. The CLI add path stamps Created/Updated
// to time.Now(), which makes recency-sort tests racy.
func listSeedTask(t *testing.T, project, slug string, status tasks.Status,
	priority tasks.Priority, created, updated, started, finished time.Time,
) {
	t.Helper()
	dir, err := state.ProjectDir(project)
	if err != nil {
		t.Fatalf("ProjectDir: %v", err)
	}
	path := filepath.Join(dir, "tasks.md")
	f, err := tasks.Read(path)
	if err != nil {
		t.Fatalf("Read tasks.md: %v", err)
	}
	tk := &tasks.Task{
		Slug: slug, Status: status, Priority: priority,
		Created: created, Updated: updated, StartedAt: started,
		FinishedAt: finished, SpawnedBy: "user",
		Spec: "spec", Acceptance: "acc", Notes: "",
	}
	if err := f.Add(tk); err != nil {
		t.Fatalf("Add %s: %v", slug, err)
	}
	if err := tasks.Write(path, f); err != nil {
		t.Fatalf("Write %s: %v", slug, err)
	}
}

// listSeedArchiveTask seeds a row directly in tasks-archive.md.
func listSeedArchiveTask(t *testing.T, project, slug string, status tasks.Status,
	priority tasks.Priority, created, updated, finished time.Time,
) {
	t.Helper()
	dir, err := state.ProjectDir(project)
	if err != nil {
		t.Fatalf("ProjectDir: %v", err)
	}
	path := filepath.Join(dir, "tasks-archive.md")
	f, err := tasks.Read(path)
	if err != nil {
		t.Fatalf("Read archive: %v", err)
	}
	tk := &tasks.Task{
		Slug: slug, Status: status, Priority: priority,
		Created: created, Updated: updated, FinishedAt: finished,
		SpawnedBy: "user", Spec: "s", Acceptance: "a", Notes: "",
	}
	if err := f.Add(tk); err != nil {
		t.Fatalf("Add %s to archive: %v", slug, err)
	}
	if err := tasks.Write(path, f); err != nil {
		t.Fatalf("Write archive: %v", err)
	}
}

// listRowSlugs returns the slug column from a `fleet tasks list` output
// in the order they appeared (skipping the header row). Used to assert
// recency ordering.
func listRowSlugs(out string) []string {
	var slugs []string
	for i, line := range strings.Split(out, "\n") {
		if i == 0 || line == "" {
			continue // header / trailing blank
		}
		// Output is tabwriter padded with spaces; first whitespace-
		// delimited field is the slug.
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		slugs = append(slugs, fields[0])
	}
	return slugs
}

// TestTasksList_DefaultRecencyView — the central acceptance: list shows
// every active task in priority/created order, then fills the rest of
// the cap with most-recent done/abandoned. Active is never truncated.
func TestTasksList_DefaultRecencyView(t *testing.T) {
	_, project := setupTasksHome(t)

	base := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	// 3 active rows (P0/P1/P2) and 8 done rows with finished_at spread.
	listSeedTask(t, project, "active-p0", tasks.StatusInProgress, tasks.PriorityP0,
		base.Add(1*time.Hour), base.Add(1*time.Hour), base.Add(2*time.Hour), time.Time{})
	listSeedTask(t, project, "active-p1", tasks.StatusTodo, tasks.PriorityP1,
		base.Add(2*time.Hour), base.Add(2*time.Hour), time.Time{}, time.Time{})
	listSeedTask(t, project, "active-p2", tasks.StatusReady, tasks.PriorityP2,
		base.Add(3*time.Hour), base.Add(3*time.Hour), time.Time{}, time.Time{})
	for i := 0; i < 8; i++ {
		slug := fmt.Sprintf("done-%02d", i)
		fin := base.Add(time.Duration(20+i) * time.Hour) // newer = larger i
		listSeedTask(t, project, slug, tasks.StatusDone, tasks.PriorityP2,
			base.Add(time.Duration(i)*time.Hour),
			fin, base.Add(time.Duration(i)*time.Hour), fin)
	}

	out := &bytes.Buffer{}
	if err := runTasksList(&tasksListOpts{project: project, limit: -1}, out); err != nil {
		t.Fatalf("list: %v", err)
	}
	slugs := listRowSlugs(out.String())
	// Want: 3 active first (P0, P1, P2), then 7 most-recent done (07, 06, 05, 04, 03, 02, 01).
	want := []string{
		"active-p0", "active-p1", "active-p2",
		"done-07", "done-06", "done-05", "done-04", "done-03", "done-02", "done-01",
	}
	if len(slugs) != len(want) {
		t.Fatalf("got %d rows; want %d. got=%v", len(slugs), len(want), slugs)
	}
	for i := range want {
		if slugs[i] != want[i] {
			t.Errorf("row %d: got %q; want %q (full=%v)", i, slugs[i], want[i], slugs)
		}
	}
}

// TestTasksList_ZeroActiveFillsWithDone — when no active tasks remain,
// the list cap is filled entirely from done rows.
func TestTasksList_ZeroActiveFillsWithDone(t *testing.T) {
	_, project := setupTasksHome(t)
	base := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 8; i++ {
		slug := fmt.Sprintf("done-%02d", i)
		fin := base.Add(time.Duration(20+i) * time.Hour)
		listSeedTask(t, project, slug, tasks.StatusDone, tasks.PriorityP2,
			base.Add(time.Duration(i)*time.Hour),
			fin, base.Add(time.Duration(i)*time.Hour), fin)
	}
	out := &bytes.Buffer{}
	if err := runTasksList(&tasksListOpts{project: project, limit: -1}, out); err != nil {
		t.Fatalf("list: %v", err)
	}
	slugs := listRowSlugs(out.String())
	if len(slugs) != 8 {
		t.Errorf("got %d rows; want 8 (all done rows visible). got=%v", len(slugs), slugs)
	}
	if slugs[0] != "done-07" {
		t.Errorf("most-recent done first: got %q; want done-07", slugs[0])
	}
}

// TestTasksList_ManyActiveNeverTruncated — when active count exceeds
// the cap, total visible = active count (active never truncated).
func TestTasksList_ManyActiveNeverTruncated(t *testing.T) {
	_, project := setupTasksHome(t)
	base := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	// 15 active + 5 done. Cap=10 must yield 15 active rows + 0 done.
	for i := 0; i < 15; i++ {
		slug := fmt.Sprintf("active-%02d", i)
		listSeedTask(t, project, slug, tasks.StatusTodo, tasks.PriorityP2,
			base.Add(time.Duration(i)*time.Hour),
			base.Add(time.Duration(i)*time.Hour),
			time.Time{}, time.Time{})
	}
	for i := 0; i < 5; i++ {
		slug := fmt.Sprintf("done-%02d", i)
		fin := base.Add(time.Duration(40+i) * time.Hour)
		listSeedTask(t, project, slug, tasks.StatusDone, tasks.PriorityP2,
			base.Add(time.Duration(i)*time.Hour),
			fin, base.Add(time.Duration(i)*time.Hour), fin)
	}
	out := &bytes.Buffer{}
	if err := runTasksList(&tasksListOpts{project: project, limit: -1}, out); err != nil {
		t.Fatalf("list: %v", err)
	}
	slugs := listRowSlugs(out.String())
	if len(slugs) != 15 {
		t.Errorf("got %d rows; want 15 (all active, never truncated). got=%v", len(slugs), slugs)
	}
	for i := 0; i < 15; i++ {
		if slugs[i] != fmt.Sprintf("active-%02d", i) {
			t.Errorf("row %d: got %q; want active-%02d", i, slugs[i], i)
		}
	}
}

// TestTasksList_ArchiveMerge — done rows in tasks-archive.md show up
// in the recency tail alongside live done rows, sorted together.
func TestTasksList_ArchiveMerge(t *testing.T) {
	_, project := setupTasksHome(t)
	base := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	// 1 active live; 2 done live; 3 done archive. Active(1) + done-tail(7).
	listSeedTask(t, project, "live-active", tasks.StatusInProgress, tasks.PriorityP1,
		base, base, base, time.Time{})
	listSeedTask(t, project, "live-done-newer", tasks.StatusDone, tasks.PriorityP2,
		base, base.Add(50*time.Hour), base, base.Add(50*time.Hour))
	listSeedTask(t, project, "live-done-older", tasks.StatusDone, tasks.PriorityP2,
		base, base.Add(30*time.Hour), base, base.Add(30*time.Hour))
	listSeedArchiveTask(t, project, "arc-newest", tasks.StatusDone, tasks.PriorityP2,
		base, base.Add(70*time.Hour), base.Add(70*time.Hour))
	listSeedArchiveTask(t, project, "arc-mid", tasks.StatusDone, tasks.PriorityP2,
		base, base.Add(40*time.Hour), base.Add(40*time.Hour))
	listSeedArchiveTask(t, project, "arc-oldest", tasks.StatusDone, tasks.PriorityP2,
		base, base.Add(20*time.Hour), base.Add(20*time.Hour))

	out := &bytes.Buffer{}
	if err := runTasksList(&tasksListOpts{project: project, limit: -1}, out); err != nil {
		t.Fatalf("list: %v", err)
	}
	slugs := listRowSlugs(out.String())
	want := []string{
		"live-active",
		"arc-newest", "live-done-newer", "arc-mid", "live-done-older", "arc-oldest",
	}
	if len(slugs) != len(want) {
		t.Fatalf("got %v; want %v", slugs, want)
	}
	for i := range want {
		if slugs[i] != want[i] {
			t.Errorf("row %d: got %q; want %q (full=%v)", i, slugs[i], want[i], slugs)
		}
	}
}

// TestTasksList_NoArchiveSkipsArchive — --no-archive must not include
// archived rows even when they would otherwise fill the cap.
func TestTasksList_NoArchiveSkipsArchive(t *testing.T) {
	_, project := setupTasksHome(t)
	base := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	listSeedTask(t, project, "active-1", tasks.StatusTodo, tasks.PriorityP1,
		base, base, time.Time{}, time.Time{})
	listSeedArchiveTask(t, project, "arc-1", tasks.StatusDone, tasks.PriorityP2,
		base, base.Add(10*time.Hour), base.Add(10*time.Hour))

	out := &bytes.Buffer{}
	if err := runTasksList(&tasksListOpts{
		project: project, limit: -1, noArchive: true,
	}, out); err != nil {
		t.Fatalf("list: %v", err)
	}
	got := out.String()
	if strings.Contains(got, "arc-1") {
		t.Errorf("--no-archive leaked archived slug arc-1: %s", got)
	}
	if !strings.Contains(got, "active-1") {
		t.Errorf("--no-archive dropped live row: %s", got)
	}
}

// TestTasksList_LimitOverride — --limit N changes the cap; --limit 0 +
// --all both unbound the cap.
func TestTasksList_LimitOverride(t *testing.T) {
	_, project := setupTasksHome(t)
	base := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 12; i++ {
		slug := fmt.Sprintf("done-%02d", i)
		fin := base.Add(time.Duration(20+i) * time.Hour)
		listSeedTask(t, project, slug, tasks.StatusDone, tasks.PriorityP2,
			base.Add(time.Duration(i)*time.Hour),
			fin, base.Add(time.Duration(i)*time.Hour), fin)
	}

	cases := []struct {
		name string
		opts *tasksListOpts
		want int
	}{
		{"default cap=10", &tasksListOpts{project: project, limit: -1}, 10},
		{"--limit 3", &tasksListOpts{project: project, limit: 3}, 3},
		{"--limit 0", &tasksListOpts{project: project, limit: 0}, 12},
		{"--all", &tasksListOpts{project: project, limit: -1, all: true}, 12},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := &bytes.Buffer{}
			if err := runTasksList(tc.opts, out); err != nil {
				t.Fatalf("list: %v", err)
			}
			got := len(listRowSlugs(out.String()))
			if got != tc.want {
				t.Errorf("got %d rows; want %d", got, tc.want)
			}
		})
	}
}

// TestTasksList_StatusFilterIgnoresCap — `--status done` returns every
// done across both files, ignoring the recency cap.
func TestTasksList_StatusFilterIgnoresCap(t *testing.T) {
	_, project := setupTasksHome(t)
	base := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 7; i++ {
		slug := fmt.Sprintf("live-%02d", i)
		fin := base.Add(time.Duration(10+i) * time.Hour)
		listSeedTask(t, project, slug, tasks.StatusDone, tasks.PriorityP2,
			base.Add(time.Duration(i)*time.Hour), fin,
			base.Add(time.Duration(i)*time.Hour), fin)
	}
	for i := 0; i < 8; i++ {
		slug := fmt.Sprintf("arc-%02d", i)
		fin := base.Add(time.Duration(50+i) * time.Hour)
		listSeedArchiveTask(t, project, slug, tasks.StatusDone, tasks.PriorityP2,
			base.Add(time.Duration(i)*time.Hour), fin, fin)
	}
	out := &bytes.Buffer{}
	if err := runTasksList(&tasksListOpts{project: project, status: "done", limit: -1}, out); err != nil {
		t.Fatalf("list: %v", err)
	}
	slugs := listRowSlugs(out.String())
	if len(slugs) != 15 {
		t.Errorf("got %d rows; want 15 (7 live + 8 archive). got=%v", len(slugs), slugs)
	}
	// First row should be the most-recent finished_at.
	if slugs[0] != "arc-07" {
		t.Errorf("first row = %q; want arc-07 (largest finished_at)", slugs[0])
	}
}

// TestTasksList_FinishedAtFallback — old rows missing finished_at must
// fall back to updated for the recency tail sort.
func TestTasksList_FinishedAtFallback(t *testing.T) {
	_, project := setupTasksHome(t)
	base := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	// "old-row" has no finished_at; uses Updated as the rank.
	// "new-row" has finished_at older than old-row's Updated.
	listSeedTask(t, project, "old-row", tasks.StatusDone, tasks.PriorityP2,
		base, base.Add(50*time.Hour), time.Time{}, time.Time{})
	listSeedTask(t, project, "new-row", tasks.StatusDone, tasks.PriorityP2,
		base, base.Add(20*time.Hour), base, base.Add(30*time.Hour))

	out := &bytes.Buffer{}
	if err := runTasksList(&tasksListOpts{project: project, limit: -1}, out); err != nil {
		t.Fatalf("list: %v", err)
	}
	slugs := listRowSlugs(out.String())
	if len(slugs) < 2 || slugs[0] != "old-row" {
		t.Errorf("expected old-row first (Updated 50h > new-row finished_at 30h); got %v", slugs)
	}
}

// listTaskRow returns the tasks.Task pointer for slug after re-reading
// the live file. Lifecycle tests assert StartedAt/FinishedAt directly
// against the persisted shape (round-trips through the writer).
func listTaskRow(t *testing.T, project, slug string) *tasks.Task {
	t.Helper()
	dir, err := state.ProjectDir(project)
	if err != nil {
		t.Fatalf("ProjectDir: %v", err)
	}
	f, err := tasks.Read(filepath.Join(dir, "tasks.md"))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	for _, x := range f.Tasks {
		if x.Slug == slug {
			return x
		}
	}
	t.Fatalf("task %s not found in tasks.md", slug)
	return nil
}

// TestTasksSet_LifecycleStampsStartedAt — todo→in-progress sets
// started_at on the first transition.
func TestTasksSet_LifecycleStampsStartedAt(t *testing.T) {
	_, project := setupTasksHome(t)
	addOut := &bytes.Buffer{}
	if err := runTasksAdd(&tasksAddOpts{
		project: project, slug: "lifecycle-1", priority: "P2",
		spec: "x", spawnedBy: "user", status: "todo",
	}, "", addOut); err != nil {
		t.Fatalf("add: %v", err)
	}
	slug := strings.Fields(addOut.String())[1]
	pre := listTaskRow(t, project, slug)
	if !pre.StartedAt.IsZero() {
		t.Fatalf("pre-set started_at non-zero: %v", pre.StartedAt)
	}
	if err := runTasksSet(&tasksSetOpts{project: project}, slug, "status=in-progress", &bytes.Buffer{}); err != nil {
		t.Fatalf("set: %v", err)
	}
	post := listTaskRow(t, project, slug)
	if post.StartedAt.IsZero() {
		t.Errorf("started_at zero after todo→in-progress; want stamped")
	}
}

// TestTasksSet_LifecycleStampsFinishedAt — *→done|abandoned stamps
// finished_at; reopen clears it; re-finish stamps a fresh value.
func TestTasksSet_LifecycleStampsFinishedAt(t *testing.T) {
	_, project := setupTasksHome(t)
	addOut := &bytes.Buffer{}
	if err := runTasksAdd(&tasksAddOpts{
		project: project, slug: "lifecycle-2", priority: "P2",
		spec: "x", spawnedBy: "user", status: "todo",
	}, "", addOut); err != nil {
		t.Fatalf("add: %v", err)
	}
	slug := strings.Fields(addOut.String())[1]
	if err := runTasksSet(&tasksSetOpts{project: project}, slug, "status=in-progress", &bytes.Buffer{}); err != nil {
		t.Fatalf("set in-progress: %v", err)
	}
	if err := runTasksSet(&tasksSetOpts{project: project}, slug, "status=done", &bytes.Buffer{}); err != nil {
		t.Fatalf("set done: %v", err)
	}
	post := listTaskRow(t, project, slug)
	if post.FinishedAt.IsZero() {
		t.Errorf("finished_at zero after in-progress→done; want stamped")
	}
	firstStart := post.StartedAt
	firstFinish := post.FinishedAt
	if firstStart.IsZero() {
		t.Fatalf("started_at zero; want stamped from prior in-progress")
	}
	// Reopen: done → todo. finished_at clears; started_at sticky.
	if err := runTasksSet(&tasksSetOpts{project: project}, slug, "status=todo", &bytes.Buffer{}); err != nil {
		t.Fatalf("set todo: %v", err)
	}
	post = listTaskRow(t, project, slug)
	if !post.FinishedAt.IsZero() {
		t.Errorf("finished_at not cleared on reopen: %v", post.FinishedAt)
	}
	if !post.StartedAt.Equal(firstStart) {
		t.Errorf("started_at changed on reopen; want sticky %v, got %v", firstStart, post.StartedAt)
	}
	// Re-dispatch: todo → in-progress. started_at MUST stay the original
	// (sticky). finished_at remains zero.
	if err := runTasksSet(&tasksSetOpts{project: project}, slug, "status=in-progress", &bytes.Buffer{}); err != nil {
		t.Fatalf("set in-progress (round 2): %v", err)
	}
	post = listTaskRow(t, project, slug)
	if !post.StartedAt.Equal(firstStart) {
		t.Errorf("started_at not sticky across reopen+redispatch; want %v, got %v", firstStart, post.StartedAt)
	}
	// Re-finish: in-progress → done. finished_at gets a NEW timestamp
	// (>= firstFinish; overwrite is intentional).
	if err := runTasksSet(&tasksSetOpts{project: project}, slug, "status=done", &bytes.Buffer{}); err != nil {
		t.Fatalf("set done (round 2): %v", err)
	}
	post = listTaskRow(t, project, slug)
	if post.FinishedAt.IsZero() {
		t.Errorf("finished_at zero after re-finish; want stamped")
	}
	if post.FinishedAt.Before(firstFinish) {
		t.Errorf("finished_at went backward on re-finish: first=%v second=%v",
			firstFinish, post.FinishedAt)
	}
}

// TestTasksSet_LifecycleAbandonedStamps — abandoned uses the same path
// as done.
func TestTasksSet_LifecycleAbandonedStamps(t *testing.T) {
	_, project := setupTasksHome(t)
	addOut := &bytes.Buffer{}
	if err := runTasksAdd(&tasksAddOpts{
		project: project, slug: "lifecycle-3", priority: "P2",
		spec: "x", spawnedBy: "user", status: "todo",
	}, "", addOut); err != nil {
		t.Fatalf("add: %v", err)
	}
	slug := strings.Fields(addOut.String())[1]
	if err := runTasksSet(&tasksSetOpts{project: project}, slug, "status=abandoned", &bytes.Buffer{}); err != nil {
		t.Fatalf("set abandoned: %v", err)
	}
	post := listTaskRow(t, project, slug)
	if post.FinishedAt.IsZero() {
		t.Errorf("finished_at zero after todo→abandoned; want stamped")
	}
}

// TestTasksSet_NoOpStatusDoesNotBumpStarted — re-applying the same
// status MUST NOT update started_at (would break sticky semantics).
func TestTasksSet_NoOpStatusDoesNotBumpStarted(t *testing.T) {
	_, project := setupTasksHome(t)
	addOut := &bytes.Buffer{}
	if err := runTasksAdd(&tasksAddOpts{
		project: project, slug: "lifecycle-4", priority: "P2",
		spec: "x", spawnedBy: "user", status: "todo",
	}, "", addOut); err != nil {
		t.Fatalf("add: %v", err)
	}
	slug := strings.Fields(addOut.String())[1]
	if err := runTasksSet(&tasksSetOpts{project: project}, slug, "status=in-progress", &bytes.Buffer{}); err != nil {
		t.Fatalf("first set: %v", err)
	}
	first := listTaskRow(t, project, slug).StartedAt
	// Apply same status again. started_at MUST NOT change.
	if err := runTasksSet(&tasksSetOpts{project: project}, slug, "status=in-progress", &bytes.Buffer{}); err != nil {
		t.Fatalf("second set: %v", err)
	}
	second := listTaskRow(t, project, slug).StartedAt
	if !first.Equal(second) {
		t.Errorf("started_at changed on no-op status set: first=%v second=%v", first, second)
	}
}

// TestTasksSet_NonStatusKeyDoesNotStamp — setting priority/etc. must
// not touch lifecycle timestamps.
func TestTasksSet_NonStatusKeyDoesNotStamp(t *testing.T) {
	_, project := setupTasksHome(t)
	addOut := &bytes.Buffer{}
	if err := runTasksAdd(&tasksAddOpts{
		project: project, slug: "lifecycle-5", priority: "P2",
		spec: "x", spawnedBy: "user", status: "todo",
	}, "", addOut); err != nil {
		t.Fatalf("add: %v", err)
	}
	slug := strings.Fields(addOut.String())[1]
	if err := runTasksSet(&tasksSetOpts{project: project}, slug, "priority=P0", &bytes.Buffer{}); err != nil {
		t.Fatalf("set priority: %v", err)
	}
	post := listTaskRow(t, project, slug)
	if !post.StartedAt.IsZero() {
		t.Errorf("started_at stamped on priority change: %v", post.StartedAt)
	}
	if !post.FinishedAt.IsZero() {
		t.Errorf("finished_at stamped on priority change: %v", post.FinishedAt)
	}
}

// TestTasksList_CollisionPrefersLive — slug existing in BOTH tasks.md
// and tasks-archive.md (retry-recovery window) shows once, taken from
// the live tasks.md row.
func TestTasksList_CollisionPrefersLive(t *testing.T) {
	_, project := setupTasksHome(t)
	created := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	listSeedTask(t, project, "shared-slug", tasks.StatusInProgress, tasks.PriorityP1,
		created, created.Add(1*time.Hour), created, time.Time{})
	// Archive with same slug + Created (retry-recovery).
	listSeedArchiveTask(t, project, "shared-slug", tasks.StatusDone, tasks.PriorityP1,
		created, created.Add(2*time.Hour), created.Add(2*time.Hour))

	out := &bytes.Buffer{}
	if err := runTasksList(&tasksListOpts{project: project, limit: -1}, out); err != nil {
		t.Fatalf("list: %v", err)
	}
	slugs := listRowSlugs(out.String())
	if len(slugs) != 1 {
		t.Errorf("got %d rows; want 1 (deduped). got=%v", len(slugs), slugs)
	}
	// Must show the LIVE row's status (in-progress), not archive's done.
	if !strings.Contains(out.String(), "in-progress") {
		t.Errorf("dedup picked archive (status=done); expected live (status=in-progress): %s", out.String())
	}
}
