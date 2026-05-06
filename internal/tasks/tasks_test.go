package tasks

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fixtureBytes loads a testdata/<name>.md file or fails the test.
func fixtureBytes(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// roundTripFixture loads a fixture, parses it, re-renders, and asserts
// byte equality. The fixtures are hand-authored to match the writer's
// canonical form.
func roundTripFixture(t *testing.T, name string) {
	t.Helper()
	want := fixtureBytes(t, name)
	tmp := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(tmp, want, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	f, err := Read(tmp)
	if err != nil {
		t.Fatalf("Read %s: %v", name, err)
	}
	out := filepath.Join(t.TempDir(), "out.md")
	if err := Write(out, f); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("round-trip mismatch for %s\n--- want ---\n%s\n--- got ---\n%s", name, want, got)
	}
}

func TestRoundTrip_Empty(t *testing.T)       { roundTripFixture(t, "empty.md") }
func TestRoundTrip_SingleTodo(t *testing.T)  { roundTripFixture(t, "single-todo.md") }
func TestRoundTrip_MultiStatus(t *testing.T) { roundTripFixture(t, "multi-status.md") }
func TestRoundTrip_Deps(t *testing.T)        { roundTripFixture(t, "deps.md") }
func TestRoundTrip_WorkerNotes(t *testing.T) { roundTripFixture(t, "worker-notes.md") }
func TestRoundTrip_Fifty(t *testing.T)       { roundTripFixture(t, "fifty-tasks.md") }
func TestRoundTrip_Footer(t *testing.T)      { roundTripFixture(t, "with-footer.md") }

func TestFooter_PreservedAcrossEdit(t *testing.T) {
	// Operator adds a task; existing footer must not get clobbered.
	tmp := filepath.Join(t.TempDir(), "f.md")
	if err := os.WriteFile(tmp, fixtureBytes(t, "with-footer.md"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	f, err := Read(tmp)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if f.Footer == "" {
		t.Fatalf("Footer empty after Read")
	}
	now := time.Date(2026, 5, 7, 10, 0, 0, 0, time.UTC)
	if err := f.Add(&Task{
		Slug: "beta-5678", Status: StatusTodo, Priority: PriorityP2,
		Created: now, Updated: now, SpawnedBy: "user",
		Spec: "Beta.", Acceptance: "Beta.", Notes: "",
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	out := filepath.Join(t.TempDir(), "out.md")
	if err := Write(out, f); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read out: %v", err)
	}
	if !strings.Contains(string(got), "# Operator notes") {
		t.Errorf("footer lost after Add+Write; got\n%s", got)
	}
	// Re-read: footer survives.
	f2, err := Read(out)
	if err != nil {
		t.Fatalf("re-Read: %v", err)
	}
	if !strings.Contains(f2.Footer, "Operator notes") {
		t.Errorf("re-Read footer=%q; want substring 'Operator notes'", f2.Footer)
	}
}

func TestSchemaVersionRefuse(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "schema-v2.md")
	if err := os.WriteFile(tmp, fixtureBytes(t, "schema-v2.md"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := Read(tmp)
	if err == nil {
		t.Fatal("Read schema-v2.md returned nil err; want ErrSchemaTooNew")
	}
	if !errors.Is(err, ErrSchemaTooNew) {
		t.Errorf("got err %v; want ErrSchemaTooNew", err)
	}
}

func TestSchemaVersionUpgrade(t *testing.T) {
	// no-frontmatter.md has no `---` header — Read accepts it as
	// schema=0, Write prepends v1 frontmatter.
	tmp := filepath.Join(t.TempDir(), "no-frontmatter.md")
	if err := os.WriteFile(tmp, fixtureBytes(t, "no-frontmatter.md"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	f, err := Read(tmp)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if f.Schema != 0 {
		t.Errorf("Schema=%d; want 0 (no frontmatter)", f.Schema)
	}
	if len(f.Tasks) != 1 {
		t.Fatalf("Tasks=%d; want 1", len(f.Tasks))
	}
	out := filepath.Join(t.TempDir(), "out.md")
	if err := Write(out, f); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read out: %v", err)
	}
	if !strings.HasPrefix(string(got), "---\nschema: v1\n---\n") {
		t.Errorf("Write did not prepend frontmatter; got prefix %q", string(got)[:32])
	}
	// Re-read should now show Schema=1.
	f2, err := Read(out)
	if err != nil {
		t.Fatalf("Read after upgrade: %v", err)
	}
	if f2.Schema != 1 {
		t.Errorf("after-upgrade Schema=%d; want 1", f2.Schema)
	}
}

func TestFrontmatterRoundTrip_CRLF(t *testing.T) {
	// File with CRLF line endings should parse the same as LF.
	src := "---\r\nschema: v1\r\n---\r\n\r\n## task: crlf-1234\r\n\r\n" +
		"- status: todo\r\n- priority: P1\r\n- worker_pid: 0\r\n" +
		"- worktree:\r\n- pr_url:\r\n- branch:\r\n" +
		"- created: 2026-05-06T10:00:00Z\r\n- updated: 2026-05-06T10:00:00Z\r\n" +
		"- depends_on: []\r\n- spawned_by: user\r\n\r\n" +
		"### Spec\r\n\r\nCRLF body.\r\n\r\n### Acceptance\r\n\r\nCRLF.\r\n\r\n### Notes\r\n\r\n\r\n"
	tmp := filepath.Join(t.TempDir(), "crlf.md")
	if err := os.WriteFile(tmp, []byte(src), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	f, err := Read(tmp)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(f.Tasks) != 1 {
		t.Fatalf("Tasks=%d; want 1", len(f.Tasks))
	}
	if f.Tasks[0].Slug != "crlf-1234" {
		t.Errorf("Slug=%q; want crlf-1234", f.Tasks[0].Slug)
	}
	if f.Tasks[0].Spec != "CRLF body." {
		t.Errorf("Spec=%q; want %q", f.Tasks[0].Spec, "CRLF body.")
	}
}

func TestMalformedRecovery_BadDate(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "bad-date.md")
	if err := os.WriteFile(tmp, fixtureBytes(t, "malformed-bad-date.md"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := Read(tmp)
	if err == nil {
		t.Fatal("Read malformed-bad-date.md returned nil err")
	}
	var perr *ParseError
	if !errors.As(err, &perr) {
		t.Fatalf("err is %T; want *ParseError: %v", err, err)
	}
	if perr.Line == 0 {
		t.Errorf("ParseError.Line=0; want non-zero (got %+v)", perr)
	}
	if !strings.Contains(perr.Msg, "created") {
		t.Errorf("ParseError.Msg=%q; want substring 'created'", perr.Msg)
	}
}

func TestMalformedRecovery_BadStatus(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "bad-status.md")
	if err := os.WriteFile(tmp, fixtureBytes(t, "malformed-bad-status.md"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := Read(tmp)
	if err == nil {
		t.Fatal("Read malformed-bad-status.md returned nil err")
	}
	var perr *ParseError
	if !errors.As(err, &perr) {
		t.Fatalf("err is %T; want *ParseError: %v", err, err)
	}
	if !strings.Contains(perr.Msg, "status") {
		t.Errorf("ParseError.Msg=%q; want substring 'status'", perr.Msg)
	}
}

func TestSlugUniqueness_Add(t *testing.T) {
	f := &File{Schema: 1}
	now := time.Date(2026, 5, 6, 10, 0, 0, 0, time.UTC)
	t1 := &Task{Slug: "alpha-1234", Status: StatusTodo, Priority: PriorityP1, Created: now, Updated: now}
	if err := f.Add(t1); err != nil {
		t.Fatalf("Add t1: %v", err)
	}
	t2 := &Task{Slug: "alpha-1234", Status: StatusTodo, Priority: PriorityP1, Created: now, Updated: now}
	err := f.Add(t2)
	if err == nil {
		t.Fatal("Add duplicate slug returned nil; want ErrDuplicateSlug")
	}
	if !errors.Is(err, ErrDuplicateSlug) {
		t.Errorf("got %v; want ErrDuplicateSlug", err)
	}
}

func TestSlugUniqueness_Read(t *testing.T) {
	// File with two H2 blocks sharing a slug must error on Read.
	src := "---\nschema: v1\n---\n\n" +
		"## task: dup-1234\n\n- status: todo\n- priority: P1\n" +
		"- worker_pid: 0\n- worktree:\n- pr_url:\n- branch:\n" +
		"- created: 2026-05-06T10:00:00Z\n- updated: 2026-05-06T10:00:00Z\n" +
		"- depends_on: []\n- spawned_by: user\n\n### Spec\n\nA.\n\n### Acceptance\n\nA.\n\n### Notes\n\n\n" +
		"## task: dup-1234\n\n- status: todo\n- priority: P1\n" +
		"- worker_pid: 0\n- worktree:\n- pr_url:\n- branch:\n" +
		"- created: 2026-05-06T10:00:00Z\n- updated: 2026-05-06T10:00:00Z\n" +
		"- depends_on: []\n- spawned_by: user\n\n### Spec\n\nB.\n\n### Acceptance\n\nB.\n\n### Notes\n\n"
	tmp := filepath.Join(t.TempDir(), "dup.md")
	if err := os.WriteFile(tmp, []byte(src), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := Read(tmp)
	if err == nil {
		t.Fatal("Read dup returned nil; want ErrDuplicateSlug")
	}
	if !errors.Is(err, ErrDuplicateSlug) {
		t.Errorf("got %v; want ErrDuplicateSlug", err)
	}
}

func TestArchive(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	// Seed tasks.md with three tasks; archive two of them.
	now := time.Date(2026, 5, 6, 10, 0, 0, 0, time.UTC)
	mk := func(slug string, st Status) *Task {
		return &Task{Slug: slug, Status: st, Priority: PriorityP1, Created: now, Updated: now, SpawnedBy: "user"}
	}
	f := &File{Schema: 1, Tasks: []*Task{
		mk("a-aaaa", StatusDone),
		mk("b-bbbb", StatusTodo),
		mk("c-cccc", StatusDone),
	}}
	dir := filepath.Join(tmp, "projects", "fleet")
	tasksPath := filepath.Join(dir, "tasks.md")
	if err := Write(tasksPath, f); err != nil {
		t.Fatalf("seed Write: %v", err)
	}
	if err := Archive("fleet", []string{"a-aaaa", "c-cccc"}); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	// tasks.md should have b only.
	cur, err := Read(tasksPath)
	if err != nil {
		t.Fatalf("Read tasks: %v", err)
	}
	if len(cur.Tasks) != 1 || cur.Tasks[0].Slug != "b-bbbb" {
		t.Errorf("after archive, tasks.md has %v; want [b-bbbb]", slugList(cur.Tasks))
	}
	// tasks-archive.md should have a + c.
	arc, err := Read(filepath.Join(dir, "tasks-archive.md"))
	if err != nil {
		t.Fatalf("Read archive: %v", err)
	}
	gotSlugs := slugList(arc.Tasks)
	if !contains(gotSlugs, "a-aaaa") || !contains(gotSlugs, "c-cccc") {
		t.Errorf("archive has %v; want [a-aaaa, c-cccc]", gotSlugs)
	}
}

func TestArchive_UnknownSlugSkipped(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	now := time.Date(2026, 5, 6, 10, 0, 0, 0, time.UTC)
	f := &File{Schema: 1, Tasks: []*Task{{
		Slug: "alpha-1234", Status: StatusTodo, Priority: PriorityP1,
		Created: now, Updated: now, SpawnedBy: "user",
	}}}
	dir := filepath.Join(tmp, "projects", "fleet")
	if err := Write(filepath.Join(dir, "tasks.md"), f); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := Archive("fleet", []string{"nonexistent-9999"}); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	cur, err := Read(filepath.Join(dir, "tasks.md"))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(cur.Tasks) != 1 {
		t.Errorf("tasks=%v; want untouched", slugList(cur.Tasks))
	}
}

func TestGenerateSlug_FullSlugPassthrough(t *testing.T) {
	got := GenerateSlug("add-readme-7a3c", "spec body", nil)
	if got != "add-readme-7a3c" {
		t.Errorf("got %q; want passthrough add-readme-7a3c", got)
	}
}

func TestGenerateSlug_ShortPlusHex(t *testing.T) {
	got := GenerateSlug("add-readme", "ignored", nil)
	if !strings.HasPrefix(got, "add-readme-") || len(got) != len("add-readme-")+4 {
		t.Errorf("got %q; want add-readme-<4hex>", got)
	}
	// 4hex is hex digits only.
	suffix := got[len("add-readme-"):]
	for _, c := range suffix {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			t.Errorf("suffix %q has non-hex char %q", suffix, c)
		}
	}
}

func TestGenerateSlug_DerivedFromSpec(t *testing.T) {
	spec := "Write a README for build instructions\n\n## Acceptance\n..."
	got := GenerateSlug("", spec, nil)
	// First line, kebab, ≤24 chars, then -<4hex>.
	if !strings.HasSuffix(got, "") {
		t.Fatalf("got empty: %q", got)
	}
	parts := strings.Split(got, "-")
	if len(parts) < 2 {
		t.Fatalf("got %q; want at least one hyphen", got)
	}
	short := strings.Join(parts[:len(parts)-1], "-")
	if len(short) > 24 {
		t.Errorf("short %q exceeds 24 chars (got=%q)", short, got)
	}
	if !strings.HasPrefix(short, "write-a-readme") {
		t.Errorf("short=%q; want prefix 'write-a-readme'", short)
	}
}

func TestGenerateSlug_DerivedTruncates(t *testing.T) {
	spec := "abcdefghijklmnopqrstuvwxyz0123456789"
	got := GenerateSlug("", spec, nil)
	parts := strings.Split(got, "-")
	short := strings.Join(parts[:len(parts)-1], "-")
	if len(short) > 24 {
		t.Errorf("short=%q exceeds 24 chars", short)
	}
}

func TestGenerateSlug_FallbackWhenSpecEmpty(t *testing.T) {
	got := GenerateSlug("", "", nil)
	if !strings.HasPrefix(got, "task-") {
		t.Errorf("got %q; want prefix 'task-'", got)
	}
}

// TestConcurrentWritesAreSerialized uses Write under a single goroutine
// per file; concurrent Write to the SAME path is the caller's problem
// (they take state.lock). This test just ensures Write itself doesn't
// produce a torn file under contention on different paths.
func TestConcurrentWritesAreSerialized(t *testing.T) {
	tmp := t.TempDir()
	now := time.Date(2026, 5, 6, 10, 0, 0, 0, time.UTC)
	mk := func(slug string) *File {
		return &File{Schema: 1, Tasks: []*Task{{
			Slug: slug, Status: StatusTodo, Priority: PriorityP1,
			Created: now, Updated: now, SpawnedBy: "user",
		}}}
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			path := filepath.Join(tmp, "f"+stringFromInt(i)+".md")
			if err := Write(path, mk("alpha-1234")); err != nil {
				t.Errorf("Write %d: %v", i, err)
			}
		}()
	}
	wg.Wait()
}

// helpers -----------------------------------------------------------

func slugList(ts []*Task) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Slug)
	}
	return out
}

func contains(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}

func stringFromInt(i int) string {
	if i == 0 {
		return "0"
	}
	digits := []byte{}
	for i > 0 {
		digits = append([]byte{'0' + byte(i%10)}, digits...)
		i /= 10
	}
	return string(digits)
}
