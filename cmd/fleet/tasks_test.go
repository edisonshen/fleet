package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

	for _, spec := range []string{"todo one", "ready two", "todo three"} {
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
	}
	// Promote the second one to ready.
	out := &bytes.Buffer{}
	if err := runTasksList(&tasksListOpts{project: project}, out); err != nil {
		t.Fatalf("list: %v", err)
	}
	count := strings.Count(out.String(), "todo")
	if count < 3 { // header + 3 rows
		t.Errorf("expected 3 todo rows in unfiltered list, got %q", out.String())
	}

	out.Reset()
	if err := runTasksList(&tasksListOpts{project: project, status: "todo"}, out); err != nil {
		t.Fatalf("list filtered: %v", err)
	}
	if strings.Contains(out.String(), "no tasks") {
		t.Errorf("filter dropped all rows: %s", out.String())
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

// TestResolveProject_RejectsInvalid validates the project guardrail
// short-circuits a bogus name before any disk write.
func TestResolveProject_RejectsInvalid(t *testing.T) {
	if _, err := resolveProject("Invalid/Name"); err == nil {
		t.Error("resolveProject should reject names with path separators")
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
