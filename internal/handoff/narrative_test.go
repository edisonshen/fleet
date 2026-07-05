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

// --- Reader substring table: T9 (auto ready/todo-only), T10/T10b
// (explicit-slug dedup / no-dedup), T14/T14b (per-entry generation filter),
// and malformed-tasks explicit-survives. Each row names the bug it catches.
// Structural assertions (ordering, cap, empty) live in dedicated tests below
// since they check non-substring properties. ---
func TestCollectNextSteps_Table(t *testing.T) {
	fooReady := []*tasks.Task{mkTask("foo-1111", "ready", "P1", "2026-06-01T00:00:00Z", "Fix foo", "")}
	cases := []struct {
		name        string
		tasks       []*tasks.Task
		rawTasksMD  string // written verbatim when set (malformed-degrade case)
		coordState  string
		agentID     string
		contains    []string
		notContains []string
	}{
		{
			name: "T9 auto renders only ready/todo session slugs",
			tasks: []*tasks.Task{
				mkTask("foo-1111", "ready", "P1", "2026-06-01T00:00:00Z", "Fix foo", ""),
				mkTask("bar-2222", "in-progress", "P0", "2026-06-01T00:00:00Z", "Work bar", ""),
				mkTask("baz-3333", "done", "P0", "2026-06-01T00:00:00Z", "Done baz", ""),
			},
			coordState: `{"session_tasks":[
				{"slug":"foo-1111","coord_id":"c1","ts":"t"},
				{"slug":"bar-2222","coord_id":"c1","ts":"t"},
				{"slug":"baz-3333","coord_id":"c1","ts":"t"}]}`,
			agentID:     "c1",
			contains:    []string{"- [auto] [P1] foo-1111: Fix foo"},
			notContains: []string{"bar-2222", "baz-3333"}, // in-progress/done dropped
		},
		{
			name:  "T10 explicit --slug dedups the auto twin (explicit wins)",
			tasks: fooReady,
			coordState: `{
				"session_next_steps":[{"text":"finish foo","slug":"foo-1111","coord_id":"c1","ts":"t"}],
				"session_tasks":[{"slug":"foo-1111","coord_id":"c1","ts":"t"}]}`,
			agentID:     "c1",
			contains:    []string{"- [explicit] finish foo"},
			notContains: []string{"[auto]"},
		},
		{
			name:  "T10b explicit with no slug does NOT dedup (both render)",
			tasks: fooReady,
			coordState: `{
				"session_next_steps":[{"text":"look at foo again","coord_id":"c1","ts":"t"}],
				"session_tasks":[{"slug":"foo-1111","coord_id":"c1","ts":"t"}]}`,
			agentID:  "c1",
			contains: []string{"- [explicit] look at foo again", "- [auto] [P1] foo-1111"},
		},
		{
			name:  "T14 successor renders none of the predecessor's entries",
			tasks: fooReady,
			coordState: `{
				"session_next_steps":[{"text":"A step","coord_id":"coordA","ts":"t"}],
				"session_tasks":[{"slug":"foo-1111","coord_id":"coordA","ts":"t"}]}`,
			agentID:     "coordB",
			notContains: []string{"A step", "foo-1111", "[auto]", "[explicit]"},
		},
		{
			name: "T14b mixed generation filters per entry",
			tasks: []*tasks.Task{
				mkTask("foo-1111", "ready", "P1", "2026-06-01T00:00:00Z", "Fix foo", ""),
				mkTask("bar-2222", "ready", "P1", "2026-06-01T00:00:00Z", "Fix bar", ""),
			},
			coordState: `{
				"session_next_steps":[
					{"text":"A step","coord_id":"coordA","ts":"t"},
					{"text":"B step","coord_id":"coordB","ts":"t"}],
				"session_tasks":[
					{"slug":"foo-1111","coord_id":"coordA","ts":"t"},
					{"slug":"bar-2222","coord_id":"coordB","ts":"t"}]}`,
			agentID:     "coordB",
			contains:    []string{"B step", "bar-2222"},
			notContains: []string{"A step", "foo-1111"},
		},
		{
			name:       "malformed tasks.md degrades auto to nothing; explicit survives",
			rawTasksMD: "---\nschema: 1\n---\n\n## task: broken-1234\n\n- status: not-a-real-status\n- priority: P1\n\n",
			coordState: `{
				"session_next_steps":[{"text":"explicit survives","coord_id":"c1","ts":"t"}],
				"session_tasks":[{"slug":"broken-1234","coord_id":"c1","ts":"t"}]}`,
			agentID:     "c1",
			contains:    []string{"- [explicit] explicit survives"},
			notContains: []string{"[auto]"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pdir := t.TempDir()
			if tc.rawTasksMD != "" {
				if err := os.WriteFile(filepath.Join(pdir, "tasks.md"), []byte(tc.rawTasksMD), 0o644); err != nil {
					t.Fatalf("write tasks.md: %v", err)
				}
			} else {
				writeTasksFile(t, filepath.Join(pdir, "tasks.md"), tc.tasks...)
			}
			writeCoordStateJSON(t, pdir, tc.coordState)
			got := CollectNextSteps(pdir, tc.agentID)
			for _, want := range tc.contains {
				if !strings.Contains(got, want) {
					t.Errorf("missing %q in:\n%s", want, got)
				}
			}
			for _, bad := range tc.notContains {
				if strings.Contains(got, bad) {
					t.Errorf("must NOT contain %q in:\n%s", bad, got)
				}
			}
		})
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

// --- codex iter-8 [P2]: a ready/todo task that is ALSO parked
// (Parked != "") must render ONLY under Open Questions, NEVER double-listed
// under Next Steps as actionable work. A parked task is waiting, not queued. ---
func TestCollectNextSteps_ParkedReadyTaskNotDoubleListed(t *testing.T) {
	pdir := t.TempDir()
	writeTasksFile(t, filepath.Join(pdir, "tasks.md"),
		mkTask("parked-1111", "ready", "P1", "2026-06-01T00:00:00Z", "queued but parked", "waiting on operator"),
		mkTask("clean-2222", "ready", "P1", "2026-06-01T00:00:00Z", "genuinely next", ""),
	)
	writeCoordStateJSON(t, pdir, `{"session_tasks":[
		{"slug":"parked-1111","coord_id":"c1","ts":"t"},
		{"slug":"clean-2222","coord_id":"c1","ts":"t"}]}`)

	next := CollectNextSteps(pdir, "c1")
	if strings.Contains(next, "parked-1111") {
		t.Errorf("parked ready task must NOT appear in Next Steps (double-list): %q", next)
	}
	if !strings.Contains(next, "clean-2222") {
		t.Errorf("un-parked ready task must still appear in Next Steps: %q", next)
	}
	// It DOES belong in Open Questions (Parked != "").
	oq := CollectOpenQuestions(pdir, "c1")
	if !strings.Contains(oq, "- parked-1111: waiting on operator") {
		t.Errorf("parked task must appear in Open Questions: %q", oq)
	}
}

// --- codex iter-12 [P1]: an explicit next-step is the coord's own NOTE and
// ALWAYS renders — it must NOT vanish just because its --slug task went
// blocked/parked. (A prior revision dropped it, which — combined with
// `tasks set` no longer stamping session_tasks — made a blocked slug-bound
// note disappear from BOTH Next Steps and Open Questions. The safe floor is
// to keep the explicit line unconditionally.) ---
func TestCollectNextSteps_ExplicitSlugBoundLineAlwaysRenders(t *testing.T) {
	pdir := t.TempDir()
	writeTasksFile(t, filepath.Join(pdir, "tasks.md"),
		mkTask("blocked-1111", "blocked", "P1", "2026-06-01T00:00:00Z", "went blocked", ""),
		mkTask("ready-2222", "ready", "P1", "2026-06-01T00:00:00Z", "still ready", ""),
	)
	// blocked-1111 is NOT in session_tasks (tasks set no longer stamps), so
	// its slug-bound explicit note is the ONLY record of it — must survive.
	writeCoordStateJSON(t, pdir, `{"session_next_steps":[
		{"text":"revive blocked-1111","slug":"blocked-1111","coord_id":"c1","ts":"t"},
		{"text":"finish ready-2222","slug":"ready-2222","coord_id":"c1","ts":"t"},
		{"text":"free-form idea no slug","coord_id":"c1","ts":"t"}]}`)

	got := CollectNextSteps(pdir, "c1")
	if !strings.Contains(got, "revive blocked-1111") {
		t.Errorf("slug-bound explicit note for a blocked task must NOT vanish: %q", got)
	}
	if !strings.Contains(got, "finish ready-2222") {
		t.Errorf("explicit line for a still-ready slug must render: %q", got)
	}
	if !strings.Contains(got, "free-form idea no slug") {
		t.Errorf("no-slug free-text explicit line must always render: %q", got)
	}
}

// --- T13: Open Questions = session-touched blocked/parked only, with the
// foreignGeneration filter — a non-session blocked task AND a foreign-coord
// session_task are both omitted. ---
func TestCollectOpenQuestions_SessionScopedAndGenerationFiltered(t *testing.T) {
	pdir := t.TempDir()
	writeTasksFile(t, filepath.Join(pdir, "tasks.md"),
		mkTask("foo-1111", "blocked", "P1", "2026-06-01T00:00:00Z", "stuck", ""),
		mkTask("parked-2222", "ready", "P2", "2026-06-02T00:00:00Z", "spec", "dirty worktree"),
		mkTask("qux-9999", "blocked", "P0", "2026-06-01T00:00:00Z", "also stuck", ""),        // non-session
		mkTask("foreign-8888", "blocked", "P1", "2026-06-01T00:00:00Z", "predecessor's", ""), // foreign gen
	)
	writeCoordStateJSON(t, pdir, `{"session_tasks":[
		{"slug":"foo-1111","coord_id":"c1","ts":"t"},
		{"slug":"parked-2222","coord_id":"c1","ts":"t"},
		{"slug":"foreign-8888","coord_id":"other","ts":"t"}]}`)
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
	if strings.Contains(got, "foreign-8888") {
		t.Errorf("foreign-generation session_task must be filtered: %q", got)
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

// --- codex review (settled across 3 rounds): an UNSTAMPED session entry
// (empty coord_id — an operator-shell `fleet checkpoint next-step` / `fleet
// tasks promote|set` run without FLEET_AGENT_ID) is the PRIMARY operator
// usage mode and MUST stay visible to a real coord's handoff. Only a
// FOREIGN-STAMPED entry (a different coord generation) is filtered; unstamped
// entries pass, exactly like the durable session_docs buffer. ---
func TestCollectSessionScoped_UnstampedEntriesVisibleToStampedReader(t *testing.T) {
	pdir := t.TempDir()
	writeTasksFile(t, filepath.Join(pdir, "tasks.md"),
		mkTask("ready-1111", "ready", "P1", "2026-06-01T00:00:00Z", "live ready", ""),
		mkTask("blocked-2222", "blocked", "P1", "2026-06-01T00:00:00Z", "live blocked", ""),
		mkTask("foreign-3333", "ready", "P1", "2026-06-01T00:00:00Z", "prev gen", ""),
	)
	// Unstamped operator rows + one FOREIGN-stamped row.
	writeCoordStateJSON(t, pdir, `{
		"session_next_steps":[{"text":"operator next-step","ts":"t"}],
		"session_tasks":[
			{"slug":"ready-1111","ts":"t"},
			{"slug":"blocked-2222","ts":"t"},
			{"slug":"foreign-3333","coord_id":"otherCoord","ts":"t"}]}`)

	// A REAL coord (non-empty id) STILL sees the unstamped operator rows...
	next := CollectNextSteps(pdir, "realcoord")
	if !strings.Contains(next, "operator next-step") || !strings.Contains(next, "ready-1111") {
		t.Errorf("stamped reader must keep unstamped operator entries: %q", next)
	}
	if got := CollectOpenQuestions(pdir, "realcoord"); !strings.Contains(got, "blocked-2222") {
		t.Errorf("stamped reader must keep unstamped operator open-questions: %q", got)
	}
	// ...but a FOREIGN-stamped (different generation) row is still filtered.
	if strings.Contains(next, "foreign-3333") {
		t.Errorf("foreign-generation stamped entry must be filtered: %q", next)
	}
}

// --- missing coord-state.json AND tasks.md → both collectors empty (never
// errors). ---
func TestCollectors_MissingFilesEmpty(t *testing.T) {
	pdir := t.TempDir() // nothing seeded
	if got := CollectNextSteps(pdir, "c1"); got != "" {
		t.Errorf("CollectNextSteps missing files: got %q want empty", got)
	}
	if got := CollectOpenQuestions(pdir, "c1"); got != "" {
		t.Errorf("CollectOpenQuestions missing files: got %q want empty", got)
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
