// Tests for the Slice-2 handoff narrative: the Completed (recent)
// checkpoint lift + the live tasks.md collectors (CollectNextSteps /
// CollectOpenQuestions). See docs/TASK-PLAN-handoff-manual-narrative.md.
package handoff

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/tasks"
)

// writeCoordStateJSON writes coord-state.json under pdir with the raw body.
// The session-scoped collectors read session_next_steps + session_tasks from
// it; a per-entry coord_id stamp drives the foreignGeneration filter.
func writeCoordStateJSON(t *testing.T, pdir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(pdir, "coord-state.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write coord-state.json: %v", err)
	}
}

// mkTask builds a tasks.Task with the given shape. created drives both
// Created + Updated; an empty created defaults to a fixed base time.
func mkTask(slug, status, priority, created, spec, parked string) *tasks.Task {
	ts, _ := time.Parse(time.RFC3339, created)
	if ts.IsZero() {
		ts = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	}
	return &tasks.Task{
		Slug:      slug,
		Status:    tasks.Status(status),
		Priority:  tasks.Priority(priority),
		Created:   ts,
		Updated:   ts,
		SpawnedBy: "user",
		Spec:      spec,
		Parked:    parked,
	}
}

// writeTasksFile renders a real tasks.md via the tasks package so the
// strict parser round-trips it (the collectors read it back).
func writeTasksFile(t *testing.T, path string, ts ...*tasks.Task) {
	t.Helper()
	f := &tasks.File{Schema: tasks.SchemaVersion, Tasks: ts}
	if err := tasks.Write(path, f); err != nil {
		t.Fatalf("write tasks.md: %v", err)
	}
}

// --- T9: auto block renders ONLY session_tasks slugs still ready/todo;
// in-progress → dropped (Active Subagents), done → dropped. ---
func TestCollectNextSteps_AutoOnlyReadyTodo(t *testing.T) {
	pdir := t.TempDir()
	writeTasksFile(t, filepath.Join(pdir, "tasks.md"),
		mkTask("foo-1111", "ready", "P1", "2026-06-01T00:00:00Z", "Fix foo", ""),
		mkTask("bar-2222", "in-progress", "P0", "2026-06-01T00:00:00Z", "Work bar", ""),
		mkTask("baz-3333", "done", "P0", "2026-06-01T00:00:00Z", "Done baz", ""),
	)
	writeCoordStateJSON(t, pdir, `{"session_tasks":[
		{"slug":"foo-1111","coord_id":"c1","ts":"t"},
		{"slug":"bar-2222","coord_id":"c1","ts":"t"},
		{"slug":"baz-3333","coord_id":"c1","ts":"t"}]}`)
	got := CollectNextSteps(pdir, "c1")
	if !strings.Contains(got, "- [auto] [P1] foo-1111: Fix foo") {
		t.Errorf("auto foo missing: %q", got)
	}
	if strings.Contains(got, "bar-2222") || strings.Contains(got, "baz-3333") {
		t.Errorf("in-progress/done session slug leaked into Next Steps: %q", got)
	}
}

// --- Auto ordering (priority-desc, then age) + Spec truncation to <=80 +
// empty-Spec slug renders no trailing colon. ---
func TestCollectNextSteps_AutoPriorityAgeAndTruncation(t *testing.T) {
	pdir := t.TempDir()
	long := strings.Repeat("x", 120)
	writeTasksFile(t, filepath.Join(pdir, "tasks.md"),
		mkTask("alpha-1111", "ready", "P1", "2026-06-01T00:00:00Z", long+"\nsecond", ""),
		mkTask("zulu-2222", "todo", "P0", "2026-06-10T00:00:00Z", "Patch zulu", ""),
		mkTask("bravo-3333", "ready", "P1", "2026-06-05T00:00:00Z", "Refactor bravo", ""),
		mkTask("nospec-4444", "ready", "P2", "2026-06-01T00:00:00Z", "", ""),
	)
	writeCoordStateJSON(t, pdir, `{"session_tasks":[
		{"slug":"alpha-1111","coord_id":"c1","ts":"t"},
		{"slug":"zulu-2222","coord_id":"c1","ts":"t"},
		{"slug":"bravo-3333","coord_id":"c1","ts":"t"},
		{"slug":"nospec-4444","coord_id":"c1","ts":"t"}]}`)
	got := CollectNextSteps(pdir, "c1")
	want := []string{
		"- [auto] [P0] zulu-2222: Patch zulu",
		"- [auto] [P1] alpha-1111: " + strings.Repeat("x", 80),
		"- [auto] [P1] bravo-3333: Refactor bravo",
	}
	for i, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in:\n%s", w, got)
		}
		if i > 0 && strings.Index(got, w) < strings.Index(got, want[i-1]) {
			t.Errorf("order wrong: %q before %q in:\n%s", w, want[i-1], got)
		}
	}
	if strings.Contains(got, "second") {
		t.Errorf("goal must be first Spec line only: %q", got)
	}
	if !strings.Contains(got, "- [auto] [P2] nospec-4444") || strings.Contains(got, "nospec-4444:") {
		t.Errorf("empty-spec auto slug should have no trailing colon: %q", got)
	}
}

// --- T10: explicit --slug == auto slug → one line, explicit wins. ---
func TestCollectNextSteps_ExplicitAutoDedupMatch(t *testing.T) {
	pdir := t.TempDir()
	writeTasksFile(t, filepath.Join(pdir, "tasks.md"),
		mkTask("foo-1111", "ready", "P1", "2026-06-01T00:00:00Z", "Fix foo", ""),
	)
	writeCoordStateJSON(t, pdir, `{
		"session_next_steps":[{"text":"finish foo","slug":"foo-1111","coord_id":"c1","ts":"t"}],
		"session_tasks":[{"slug":"foo-1111","coord_id":"c1","ts":"t"}]}`)
	got := CollectNextSteps(pdir, "c1")
	if !strings.Contains(got, "- [explicit] finish foo") {
		t.Errorf("explicit line missing: %q", got)
	}
	if strings.Contains(got, "[auto]") {
		t.Errorf("auto twin must be dropped when explicit --slug matches: %q", got)
	}
}

// --- T10b: explicit with NO slug + same-topic auto → both rendered (no
// fuzzy match). ---
func TestCollectNextSteps_ExplicitNoSlugNoDedup(t *testing.T) {
	pdir := t.TempDir()
	writeTasksFile(t, filepath.Join(pdir, "tasks.md"),
		mkTask("foo-1111", "ready", "P1", "2026-06-01T00:00:00Z", "Fix foo", ""),
	)
	writeCoordStateJSON(t, pdir, `{
		"session_next_steps":[{"text":"look at foo again","coord_id":"c1","ts":"t"}],
		"session_tasks":[{"slug":"foo-1111","coord_id":"c1","ts":"t"}]}`)
	got := CollectNextSteps(pdir, "c1")
	if !strings.Contains(got, "- [explicit] look at foo again") {
		t.Errorf("explicit line missing: %q", got)
	}
	if !strings.Contains(got, "- [auto] [P1] foo-1111") {
		t.Errorf("auto line must remain (no fuzzy match): %q", got)
	}
}

// --- T11: empty session buffers + large backlog → placeholder (empty),
// NEVER a backlog dump. ---
func TestCollectNextSteps_EmptyBuffersNoBacklogDump(t *testing.T) {
	pdir := t.TempDir()
	var backlog []*tasks.Task
	for i := 0; i < 15; i++ {
		backlog = append(backlog,
			mkTask(fmt.Sprintf("old-%04d", i), "ready", "P0", "2026-05-01T00:00:00Z", "ancient backlog", ""))
	}
	writeTasksFile(t, filepath.Join(pdir, "tasks.md"), backlog...)
	// No coord-state buffers at all.
	if got := CollectNextSteps(pdir, "c1"); got != "" {
		t.Errorf("empty session buffers must yield placeholder (empty), got backlog dump:\n%s", got)
	}
}

// --- T12: 15 combined (5 explicit + 10 auto) → 10 rendered, explicit first. ---
func TestCollectNextSteps_CapCombinedExplicitFirst(t *testing.T) {
	pdir := t.TempDir()
	var tsk []*tasks.Task
	var autoEntries []string
	for i := 0; i < 10; i++ {
		slug := fmt.Sprintf("auto-%04d", i)
		tsk = append(tsk, mkTask(slug, "ready", "P1", "2026-06-01T00:00:00Z", "goal", ""))
		autoEntries = append(autoEntries, fmt.Sprintf(`{"slug":"%s","coord_id":"c1","ts":"t"}`, slug))
	}
	writeTasksFile(t, filepath.Join(pdir, "tasks.md"), tsk...)
	var explicit []string
	for i := 0; i < 5; i++ {
		explicit = append(explicit, fmt.Sprintf(`{"text":"explicit-%d","coord_id":"c1","ts":"t"}`, i))
	}
	writeCoordStateJSON(t, pdir, fmt.Sprintf(
		`{"session_next_steps":[%s],"session_tasks":[%s]}`,
		strings.Join(explicit, ","), strings.Join(autoEntries, ",")))
	got := CollectNextSteps(pdir, "c1")
	lines := strings.Split(got, "\n")
	if len(lines) != 10 {
		t.Fatalf("expected 10 lines (cap), got %d:\n%s", len(lines), got)
	}
	for i := 0; i < 5; i++ {
		if !strings.Contains(lines[i], "[explicit]") {
			t.Errorf("line %d should be explicit-first, got: %q", i, lines[i])
		}
	}
}

// --- T13: Open Questions = session-touched blocked/parked only; a
// non-session blocked task is omitted. ---
func TestCollectOpenQuestions_SessionScoped(t *testing.T) {
	pdir := t.TempDir()
	writeTasksFile(t, filepath.Join(pdir, "tasks.md"),
		mkTask("foo-1111", "blocked", "P1", "2026-06-01T00:00:00Z", "stuck", ""),
		mkTask("parked-2222", "ready", "P2", "2026-06-02T00:00:00Z", "spec", "dirty worktree"),
		mkTask("qux-9999", "blocked", "P0", "2026-06-01T00:00:00Z", "also stuck", ""),
	)
	writeCoordStateJSON(t, pdir, `{"session_tasks":[
		{"slug":"foo-1111","coord_id":"c1","ts":"t"},
		{"slug":"parked-2222","coord_id":"c1","ts":"t"}]}`)
	got := CollectOpenQuestions(pdir, "c1")
	if !strings.Contains(got, "- foo-1111: blocked") {
		t.Errorf("session blocked task missing: %q", got)
	}
	if !strings.Contains(got, "- parked-2222: dirty worktree") {
		t.Errorf("session parked task missing: %q", got)
	}
	if strings.Contains(got, "qux-9999") {
		t.Errorf("non-session blocked task must be omitted: %q", got)
	}
}

// --- Open Questions: no session-touched blocked → placeholder even with a
// backlog full of blocked rows. ---
func TestCollectOpenQuestions_NoSessionBlockedPlaceholder(t *testing.T) {
	pdir := t.TempDir()
	writeTasksFile(t, filepath.Join(pdir, "tasks.md"),
		mkTask("qux-9999", "blocked", "P0", "2026-06-01T00:00:00Z", "backlog blocked", ""),
	)
	writeCoordStateJSON(t, pdir, `{"session_tasks":[]}`)
	if got := CollectOpenQuestions(pdir, "c1"); got != "" {
		t.Errorf("no session-blocked → placeholder, got: %q", got)
	}
}

// --- T14: generation non-leak — a successor (different id, no records)
// renders placeholder; predecessor's entries are foreign-filtered. ---
func TestCollectNextSteps_GenerationNonLeak(t *testing.T) {
	pdir := t.TempDir()
	writeTasksFile(t, filepath.Join(pdir, "tasks.md"),
		mkTask("foo-1111", "ready", "P1", "2026-06-01T00:00:00Z", "Fix foo", ""),
		mkTask("blk-2222", "blocked", "P1", "2026-06-01T00:00:00Z", "stuck", ""),
	)
	writeCoordStateJSON(t, pdir, `{
		"session_next_steps":[{"text":"A step","coord_id":"coordA","ts":"t"}],
		"session_tasks":[
			{"slug":"foo-1111","coord_id":"coordA","ts":"t"},
			{"slug":"blk-2222","coord_id":"coordA","ts":"t"}]}`)
	if got := CollectNextSteps(pdir, "coordB"); got != "" {
		t.Errorf("successor must not render predecessor's Next Steps: %q", got)
	}
	if got := CollectOpenQuestions(pdir, "coordB"); got != "" {
		t.Errorf("successor must not render predecessor's Open Questions: %q", got)
	}
}

// --- T14b: mixed-generation — per-entry filter keeps only the acting
// coord's entries (not all-or-nothing). ---
func TestCollectNextSteps_MixedGenerationFiltersPerEntry(t *testing.T) {
	pdir := t.TempDir()
	writeTasksFile(t, filepath.Join(pdir, "tasks.md"),
		mkTask("foo-1111", "ready", "P1", "2026-06-01T00:00:00Z", "Fix foo", ""),
		mkTask("bar-2222", "ready", "P1", "2026-06-01T00:00:00Z", "Fix bar", ""),
	)
	writeCoordStateJSON(t, pdir, `{
		"session_next_steps":[
			{"text":"A step","coord_id":"coordA","ts":"t"},
			{"text":"B step","coord_id":"coordB","ts":"t"}],
		"session_tasks":[
			{"slug":"foo-1111","coord_id":"coordA","ts":"t"},
			{"slug":"bar-2222","coord_id":"coordB","ts":"t"}]}`)
	got := CollectNextSteps(pdir, "coordB")
	if !strings.Contains(got, "B step") || !strings.Contains(got, "bar-2222") {
		t.Errorf("coordB's own entries must render: %q", got)
	}
	if strings.Contains(got, "A step") || strings.Contains(got, "foo-1111") {
		t.Errorf("coordA's entries must be filtered from coordB handoff: %q", got)
	}
}

// --- missing coord-state.json AND tasks.md → empty (never errors). ---
func TestCollectors_MissingFilesEmpty(t *testing.T) {
	pdir := t.TempDir() // nothing seeded
	if got := CollectNextSteps(pdir, "c1"); got != "" {
		t.Errorf("CollectNextSteps missing files: got %q want empty", got)
	}
	if got := CollectOpenQuestions(pdir, "c1"); got != "" {
		t.Errorf("CollectOpenQuestions missing files: got %q want empty", got)
	}
}

// --- malformed tasks.md → auto block degrades to nothing, but the explicit
// block (sourced from coord-state, not tasks.md) still renders. ---
func TestCollectNextSteps_MalformedTasksExplicitSurvives(t *testing.T) {
	pdir := t.TempDir()
	body := "---\nschema: 1\n---\n\n## task: broken-1234\n\n- status: not-a-real-status\n- priority: P1\n\n"
	if err := os.WriteFile(filepath.Join(pdir, "tasks.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	writeCoordStateJSON(t, pdir, `{
		"session_next_steps":[{"text":"explicit survives","coord_id":"c1","ts":"t"}],
		"session_tasks":[{"slug":"broken-1234","coord_id":"c1","ts":"t"}]}`)
	got := CollectNextSteps(pdir, "c1")
	if !strings.Contains(got, "- [explicit] explicit survives") {
		t.Errorf("explicit block must survive malformed tasks.md: %q", got)
	}
	if strings.Contains(got, "[auto]") {
		t.Errorf("auto block must degrade to nothing on malformed tasks.md: %q", got)
	}
}

// --- Test 9: checkpoint WITH Completed (recent) → shared lift sets
// doc.Completed on BOTH manual + recovery docs. ---
func TestApplyCheckpoint_CompletedLift(t *testing.T) {
	cp := &checkpointDoc{
		recentCompletions: []string{"done fix-foo", "merged bar https://x/pull/9"},
	}
	doc := &Doc{Completed: Placeholder}
	applyCheckpointToDoc(doc, cp)
	if !strings.Contains(doc.Completed, "fix-foo") || !strings.Contains(doc.Completed, "merged bar") {
		t.Errorf("Completed lift: got %q want both completion lines", doc.Completed)
	}
	if doc.Completed == Placeholder {
		t.Errorf("Completed should be filled from the checkpoint buffer")
	}
}

// --- Test 10 / 11b: empty Completed (recent) → parser short-circuits the
// placeholder to nil; applyCheckpointToDoc leaves the doc placeholder. ---
func TestApplyCheckpoint_EmptyCompletedKeepsPlaceholder(t *testing.T) {
	cp := &checkpointDoc{recentCompletions: nil}
	doc := &Doc{Completed: Placeholder}
	applyCheckpointToDoc(doc, cp)
	if doc.Completed != Placeholder {
		t.Errorf("empty completions: got %q want placeholder preserved", doc.Completed)
	}
}

// --- Test 11 / 11b: round-trip — a checkpoint emitting Completed (recent)
// parses back via parseCheckpoint into recentCompletions; the empty
// placeholder round-trips to nil. ---
func TestParseCheckpoint_CompletedSection(t *testing.T) {
	withSection := "---\nschema: v1\ncoord_id: \"c1\"\nproject: \"p\"\n" +
		"updated_at: \"" + time.Now().UTC().Format(time.RFC3339) + "\"\ntick_count: 5\n---\n\n" +
		"### Active Subagents\n_(none)_\n\n" +
		"### Open PRs\n_(no open PRs)_\n\n" +
		"### Recent decisions\n_(no recent decisions)_\n\n" +
		"### Completed (recent)\n- done fix-foo\n- merged #7 bar\n\n" +
		"### Drafted but unfiled tasks\n_(empty — populated in Phase 2)_\n"
	cp, ok := parseCheckpoint([]byte(withSection))
	if !ok {
		t.Fatal("parseCheckpoint returned ok=false")
	}
	if len(cp.recentCompletions) != 2 {
		t.Fatalf("recentCompletions: got %#v want 2 lines", cp.recentCompletions)
	}
	if cp.recentCompletions[0] != "done fix-foo" || cp.recentCompletions[1] != "merged #7 bar" {
		t.Errorf("recentCompletions content drift: %#v", cp.recentCompletions)
	}

	// Empty placeholder → nil (short-circuit, like decisions).
	emptySection := strings.Replace(
		withSection,
		"### Completed (recent)\n- done fix-foo\n- merged #7 bar\n\n",
		"### Completed (recent)\n_(no recent completions)_\n\n",
		1,
	)
	cp2, ok := parseCheckpoint([]byte(emptySection))
	if !ok {
		t.Fatal("parseCheckpoint (empty) returned ok=false")
	}
	if cp2.recentCompletions != nil {
		t.Errorf("empty Completed placeholder must parse to nil, got %#v", cp2.recentCompletions)
	}
}

// --- Test 10: older checkpoint WITHOUT the Completed (recent) section
// still parses; recentCompletions is nil → doc.Completed placeholder. ---
func TestParseCheckpoint_AbsentCompletedSection(t *testing.T) {
	older := "---\nschema: v1\ncoord_id: \"c1\"\nproject: \"p\"\n" +
		"updated_at: \"" + time.Now().UTC().Format(time.RFC3339) + "\"\ntick_count: 5\n---\n\n" +
		"### Active Subagents\n_(none)_\n\n" +
		"### Open PRs\n_(no open PRs)_\n\n" +
		"### Recent decisions\n_(no recent decisions)_\n\n" +
		"### Drafted but unfiled tasks\n_(empty — populated in Phase 2)_\n"
	cp, ok := parseCheckpoint([]byte(older))
	if !ok {
		t.Fatal("parseCheckpoint (absent section) returned ok=false")
	}
	if cp.recentCompletions != nil {
		t.Errorf("absent Completed section must yield nil, got %#v", cp.recentCompletions)
	}
}
