// Tests for the Slice-2 handoff narrative: the Completed (recent)
// checkpoint lift + the live tasks.md collectors (CollectNextSteps /
// CollectOpenQuestions). See docs/TASK-PLAN-handoff-manual-narrative.md.
package handoff

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/tasks"
)

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

// --- Test 17: Next Steps from ready/todo queue, priority-desc then age. ---
func TestCollectNextSteps_PriorityAndAgeOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.md")
	writeTasksFile(t, path,
		// P1, older
		mkTask("alpha-1111", "ready", "P1", "2026-06-01T00:00:00Z", "Fix the alpha widget", ""),
		// P0, newest — should sort first (priority desc)
		mkTask("zulu-2222", "todo", "P0", "2026-06-10T00:00:00Z", "Patch the zulu hole", ""),
		// P1, newer than alpha — sorts after alpha (same priority, older first)
		mkTask("bravo-3333", "ready", "P1", "2026-06-05T00:00:00Z", "Refactor bravo", ""),
	)

	got := CollectNextSteps(path)
	// Expect P0 first, then the two P1s oldest-first.
	wantOrder := []string{
		"- [P0] zulu-2222: Patch the zulu hole",
		"- [P1] alpha-1111: Fix the alpha widget",
		"- [P1] bravo-3333: Refactor bravo",
	}
	for i, w := range wantOrder {
		if !strings.Contains(got, w) {
			t.Errorf("Next Steps missing line %q in:\n%s", w, got)
		}
		// Order check: each subsequent line appears after the prior.
		if i > 0 {
			if strings.Index(got, w) < strings.Index(got, wantOrder[i-1]) {
				t.Errorf("Next Steps order wrong: %q before %q in:\n%s", w, wantOrder[i-1], got)
			}
		}
	}
}

// --- Test 17b: ready task with empty Spec → slug only, no trailing colon. ---
func TestCollectNextSteps_EmptySpecSlugOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.md")
	writeTasksFile(t, path,
		mkTask("nospec-1234", "ready", "P2", "2026-06-01T00:00:00Z", "", ""),
	)

	got := CollectNextSteps(path)
	if !strings.Contains(got, "- [P2] nospec-1234") {
		t.Errorf("expected slug-only bullet, got:\n%s", got)
	}
	if strings.Contains(got, "nospec-1234:") {
		t.Errorf("empty Spec must NOT render a trailing colon, got:\n%s", got)
	}
}

// --- Spec first-line truncation to <=80 chars. ---
func TestCollectNextSteps_SpecTruncated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.md")
	long := strings.Repeat("x", 120)
	writeTasksFile(t, path,
		mkTask("longspec-1234", "ready", "P1", "2026-06-01T00:00:00Z", long+"\nsecond line", ""),
	)

	got := CollectNextSteps(path)
	// Single task → the body is exactly one bullet. The goal must be the
	// first Spec line truncated to 80 x's, no more.
	want := "- [P1] longspec-1234: " + strings.Repeat("x", 80)
	if got != want {
		t.Errorf("Spec not truncated to 80 chars:\n got %q\nwant %q", got, want)
	}
	if strings.Contains(got, "second line") {
		t.Errorf("goal must be first non-blank Spec line only, got:\n%s", got)
	}
}

// --- Test 20: only terminal / in-progress tasks → empty (placeholder). ---
func TestCollectNextSteps_ExcludesInProgressAndTerminal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.md")
	writeTasksFile(t, path,
		mkTask("done-1111", "done", "P1", "2026-06-01T00:00:00Z", "shipped", ""),
		mkTask("inflight-2222", "in-progress", "P1", "2026-06-01T00:00:00Z", "working", ""),
		mkTask("dropped-3333", "abandoned", "P1", "2026-06-01T00:00:00Z", "nope", ""),
	)

	if got := CollectNextSteps(path); got != "" {
		t.Errorf("Next Steps should be empty (no ready/todo), got:\n%s", got)
	}
}

// --- Test 18: Open Questions selects blocked OR Parked!=""; reason text. ---
func TestCollectOpenQuestions_BlockedOrParked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.md")
	writeTasksFile(t, path,
		mkTask("stuck-1111", "blocked", "P1", "2026-06-01T00:00:00Z", "stuck spec", ""),
		mkTask("parked-2222", "ready", "P2", "2026-06-02T00:00:00Z", "parked spec", "2026-06-02 dirty worktree"),
		mkTask("fine-3333", "ready", "P1", "2026-06-03T00:00:00Z", "fine", ""),
	)

	got := CollectOpenQuestions(path)
	if !strings.Contains(got, "stuck-1111") || !strings.Contains(got, "blocked") {
		t.Errorf("Open Questions missing blocked task reason-less, got:\n%s", got)
	}
	if !strings.Contains(got, "parked-2222") || !strings.Contains(got, "dirty worktree") {
		t.Errorf("Open Questions missing parked task + Parked text, got:\n%s", got)
	}
	if strings.Contains(got, "fine-3333") {
		t.Errorf("Open Questions must not include a non-blocked non-parked task, got:\n%s", got)
	}
}

// --- Test 19: missing file → empty (never errors). ---
func TestCollectors_MissingFileEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.md")
	if got := CollectNextSteps(path); got != "" {
		t.Errorf("CollectNextSteps missing file: got %q want empty", got)
	}
	if got := CollectOpenQuestions(path); got != "" {
		t.Errorf("CollectOpenQuestions missing file: got %q want empty", got)
	}
}

// --- Test 19: malformed block → strict parse fails → collectors degrade
// to empty (no panic, no error surfaced). ---
func TestCollectors_MalformedDegradesToEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.md")
	// A block with a bad status enum trips the strict tasks.Read. Written
	// as raw bytes (the tasks writer would refuse to render an invalid
	// task). Collectors must swallow the parse error → empty.
	body := "---\nschema: 1\n---\n\n## task: broken-1234\n\n- status: not-a-real-status\n- priority: P1\n\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if got := CollectNextSteps(path); got != "" {
		t.Errorf("CollectNextSteps malformed: got %q want empty", got)
	}
	if got := CollectOpenQuestions(path); got != "" {
		t.Errorf("CollectOpenQuestions malformed: got %q want empty", got)
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
