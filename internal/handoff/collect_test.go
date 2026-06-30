package handoff

// collect_test.go — Slice 3 (Key Decisions + Files Modified) collector +
// lifecycle tests. Drives the manual EnrichManualDoc + recovery
// SynthesizeRecovery paths and the live git collector (CollectFilesModified)
// + the checkpoint recent_decisions → doc.KeyDecisions mapping. The Slice 2
// collectors (CollectNextSteps / CollectOpenQuestions) and their unit tests
// live in narrative_test.go; the lifecycle tests below assert all five
// sections fill end-to-end. See docs/DESIGN-handoff-doc-slice3-decisions-
// files.md.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/tasks"
)

// seedRichTasksMD writes a tasks.md under <projectsRoot>/<project> from
// the given tasks via the real tasks package (so the strict parser the
// collectors use round-trips it). Created/Updated are stamped now unless
// already set, with a small per-index skew so age ordering is testable.
func seedRichTasksMD(t *testing.T, projectsRoot, project string, ts []*tasks.Task) {
	t.Helper()
	dir := filepath.Join(projectsRoot, project)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	for i, task := range ts {
		if task.Created.IsZero() {
			task.Created = base.Add(time.Duration(i) * time.Minute)
		}
		if task.Updated.IsZero() {
			task.Updated = task.Created
		}
	}
	if err := tasks.Write(filepath.Join(dir, "tasks.md"), &tasks.File{Tasks: ts}); err != nil {
		t.Fatalf("write tasks.md: %v", err)
	}
}

// initGitRepoWithDocs creates a git repo at dir with an uncommitted
// tracked docs/DESIGN-x.md and a gitignored docs/DESIGN-x.html, returning
// dir. Mirrors the coordinator's real plan-doc footprint.
func initGitRepoWithDocs(t *testing.T, dir string) {
	t.Helper()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		// Quiet identity so commits don't depend on global git config.
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q")
	docs := filepath.Join(dir, "docs")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatalf("mkdir docs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("docs/*.html\n"), 0o644); err != nil {
		t.Fatalf("write .gitignore: %v", err)
	}
	if err := os.WriteFile(filepath.Join(docs, "DESIGN-x.md"), []byte("# design\n"), 0o644); err != nil {
		t.Fatalf("write DESIGN-x.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(docs, "DESIGN-x.html"), []byte("<html></html>\n"), 0o644); err != nil {
		t.Fatalf("write DESIGN-x.html: %v", err)
	}
}

// ---------- CollectFilesModified ----------

func TestCollectFilesModified_ListsTrackedAndIgnoredDocs(t *testing.T) {
	repo := t.TempDir()
	initGitRepoWithDocs(t, repo)
	got := CollectFilesModified(repo)
	if !strings.Contains(got, "docs/DESIGN-x.md") {
		t.Errorf("FilesModified missing tracked .md: %q", got)
	}
	if !strings.Contains(got, "docs/DESIGN-x.html") {
		t.Errorf("FilesModified missing gitignored .html (--ignored=matching): %q", got)
	}
}

func TestCollectFilesModified_NonGitOrEmptyIsEmpty(t *testing.T) {
	// Non-git dir → "".
	if got := CollectFilesModified(t.TempDir()); got != "" {
		t.Errorf("non-git dir: got %q want empty", got)
	}
	// Empty repoDir → "" (no shell-out).
	if got := CollectFilesModified(""); got != "" {
		t.Errorf("empty repoDir: got %q want empty", got)
	}
}

func TestCollectFilesModified_CapsLargeOutput(t *testing.T) {
	// Inject a fake git emitting more than filesModifiedMax untracked docs
	// entries so the cap + overflow tail are exercised deterministically.
	const n = filesModifiedMax + 7
	orig := gitRunner
	gitRunner = func(string, ...string) ([]byte, error) {
		var b strings.Builder
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, "?? docs/scratch/f%03d.md\n", i)
		}
		return []byte(b.String()), nil
	}
	t.Cleanup(func() { gitRunner = orig })

	got := CollectFilesModified("/some/repo")
	lines := strings.Split(got, "\n")
	// filesModifiedMax bullets + 1 overflow tail line.
	if len(lines) != filesModifiedMax+1 {
		t.Fatalf("line count: got %d want %d", len(lines), filesModifiedMax+1)
	}
	wantTail := fmt.Sprintf("- … and %d more", n-filesModifiedMax)
	if lines[len(lines)-1] != wantTail {
		t.Errorf("overflow tail: got %q want %q", lines[len(lines)-1], wantTail)
	}
}

func TestCollectFilesModified_QuotePathDisabled(t *testing.T) {
	// The collector must request core.quotePath=false so UTF-8 paths render
	// literally rather than C-quoted. Assert the flag is passed to git.
	var gotArgs []string
	orig := gitRunner
	gitRunner = func(_ string, args ...string) ([]byte, error) {
		gotArgs = args
		return []byte("?? docs/café.md\n"), nil
	}
	t.Cleanup(func() { gitRunner = orig })

	got := CollectFilesModified("/some/repo")
	if !strings.Contains(strings.Join(gotArgs, " "), "-c core.quotePath=false") {
		t.Errorf("git args missing core.quotePath=false: %v", gotArgs)
	}
	if !strings.Contains(got, "docs/café.md") {
		t.Errorf("UTF-8 path not rendered literally: %q", got)
	}
}

// ---------- applyCheckpointToDoc mapping ----------

func TestApplyCheckpointToDoc_CompletionsAndDecisions(t *testing.T) {
	doc := &Doc{Completed: Placeholder, KeyDecisions: Placeholder, NextSteps: Placeholder, OpenQuestions: Placeholder}
	cp := &checkpointDoc{
		recentCompletions: []string{"reconciled a → done", "PR merged → task b done"},
		recentDecisions:   []string{"dispatched worker c (gen 1)"},
	}
	applyCheckpointToDoc(doc, cp)
	if doc.Completed != "- reconciled a → done\n- PR merged → task b done" {
		t.Errorf("Completed: got %q", doc.Completed)
	}
	if doc.KeyDecisions != "- dispatched worker c (gen 1)" {
		t.Errorf("KeyDecisions: got %q", doc.KeyDecisions)
	}
	// NextSteps + OpenQuestions are NOT touched by the checkpoint lift —
	// they come live from tasks.md.
	if doc.NextSteps != Placeholder || doc.OpenQuestions != Placeholder {
		t.Errorf("checkpoint lift must not touch NextSteps/OpenQuestions: %q / %q", doc.NextSteps, doc.OpenQuestions)
	}
}

func TestApplyCheckpointToDoc_EmptyBuffersLeavePlaceholder(t *testing.T) {
	doc := &Doc{Completed: Placeholder, KeyDecisions: Placeholder}
	applyCheckpointToDoc(doc, &checkpointDoc{})
	if doc.Completed != Placeholder || doc.KeyDecisions != Placeholder {
		t.Errorf("empty buffers must leave placeholders: %q / %q", doc.Completed, doc.KeyDecisions)
	}
}

// seedCheckpointFull writes a coord-checkpoint.md including the Slice-2
// `### Completed (recent)` section (seedCheckpoint omits it). coord_id is
// hardcoded "deadbeef" to match the synth/enrich generation guard.
func seedCheckpointFull(
	t *testing.T,
	projectsRoot, project string,
	updatedAt time.Time,
	activeRows, decisionRows, completionRows []string,
) {
	t.Helper()
	dir := filepath.Join(projectsRoot, project)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	var b strings.Builder
	b.WriteString("---\nschema: v1\ncoord_id: \"deadbeef\"\nproject: \"" + project + "\"\n")
	b.WriteString("updated_at: \"" + updatedAt.UTC().Format(time.RFC3339) + "\"\ntick_count: 5\n---\n\n")
	b.WriteString("### Active Subagents\n")
	if len(activeRows) == 0 {
		b.WriteString("_(none)_\n\n")
	} else {
		b.WriteString(strings.Join(activeRows, "\n") + "\n\n")
	}
	b.WriteString("### Open PRs\n_(no open PRs)_\n\n")
	b.WriteString("### Recent decisions\n")
	if len(decisionRows) == 0 {
		b.WriteString("_(no recent decisions)_\n\n")
	} else {
		b.WriteString(strings.Join(decisionRows, "\n") + "\n\n")
	}
	b.WriteString("### Completed (recent)\n")
	if len(completionRows) == 0 {
		b.WriteString("_(no recent completions)_\n\n")
	} else {
		b.WriteString(strings.Join(completionRows, "\n") + "\n\n")
	}
	b.WriteString("### Drafted but unfiled tasks\n_(empty — populated in Phase 2)_\n")
	if err := os.WriteFile(filepath.Join(dir, "coord-checkpoint.md"), []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write checkpoint: %v", err)
	}
}

// lifecycleState seeds the full on-disk state both the manual + synth
// paths read: 2 active subagents (one missing state.json), a checkpoint
// carrying recent_completions + recent_decisions, and a rich tasks.md.
// Returns the project name.
func lifecycleState(t *testing.T, pdir string, now time.Time) {
	t.Helper()
	seedCoordState(t, pdir, "myproj", map[string]string{
		"have-state-1111": "agentaaaa",
		"no-state-2222":   "agentbbbb",
	})
	// Only one worker has a state.json on disk; the other is emit-on-missing.
	seedWorkerState(t, pdir, "myproj", "have-state-1111", "tdd-green", "")
	seedCheckpointFull(t, pdir, "myproj", now,
		nil,
		[]string{"- dispatched worker have-state-1111 (gen 1)"},
		[]string{"- reconciled old-task → done", "- PR merged → task merged-task done"},
	)
	seedRichTasksMD(t, pdir, "myproj", []*tasks.Task{
		{Slug: "have-state-1111", Status: tasks.StatusInProgress, Priority: tasks.PriorityP1, Spec: "the active one", Created: now.Add(-5 * time.Minute)},
		{Slug: "no-state-2222", Status: tasks.StatusInProgress, Priority: tasks.PriorityP1, Spec: "the missing one", Created: now.Add(-4 * time.Minute)},
		{Slug: "ready-3333", Status: tasks.StatusReady, Priority: tasks.PriorityP0, Spec: "queued urgent", Created: now.Add(-3 * time.Minute)},
		{Slug: "todo-4444", Status: tasks.StatusTodo, Priority: tasks.PriorityP2, Spec: "queued later", Created: now.Add(-2 * time.Minute)},
		{Slug: "blocked-5555", Status: tasks.StatusBlocked, Priority: tasks.PriorityP1, Created: now.Add(-1 * time.Minute)},
		{Slug: "parked-6666", Status: tasks.StatusInProgress, Priority: tasks.PriorityP1, Parked: "awaiting operator decision", Created: now},
	})
}

// T1 — LIFECYCLE: a real manual handoff fills the five remaining sections
// AND the two #236-shipped sections do not regress.
func TestEnrichManualDoc_Lifecycle_FillsFiveSections(t *testing.T) {
	pdir := withFleetHomeSynth(t)
	now := time.Now().UTC()
	lifecycleState(t, pdir, now)

	// Manual path also fills Files Modified from the bound repo.
	repo := t.TempDir()
	initGitRepoWithDocs(t, repo)

	// gh returns two open worker PRs.
	fakeGH(t, ghJSON(t, []ghOpenPR{
		{Number: 1, Title: "p1", HeadRefName: "worker/have-state-1111", URL: "https://github.com/o/r/pull/1"},
		{Number: 2, Title: "p2", HeadRefName: "worker/no-state-2222", URL: "https://github.com/o/r/pull/2"},
	}), nil)

	doc := NewManualStub("deadbeef", "coord-myproj", "myproj", 1, nil, now)
	EnrichManualDoc(doc, "myproj", "deadbeef", repo, nil, nil)

	// 1. Completed — the two checkpoint completion lines; NOT a dispatch.
	if !strings.Contains(doc.Completed, "reconciled old-task → done") ||
		!strings.Contains(doc.Completed, "PR merged → task merged-task done") {
		t.Errorf("Completed: got %q", doc.Completed)
	}
	if strings.Contains(doc.Completed, "dispatched") {
		t.Errorf("Completed must not contain dispatch events: %q", doc.Completed)
	}
	// 2. Next Steps — ready + todo (priority-desc); NOT in-progress.
	if doc.NextSteps != "- [P0] ready-3333: queued urgent\n- [P2] todo-4444: queued later" {
		t.Errorf("NextSteps: got %q", doc.NextSteps)
	}
	if strings.Contains(doc.NextSteps, "have-state-1111") {
		t.Errorf("NextSteps must exclude in-progress: %q", doc.NextSteps)
	}
	// 3. Open Questions — blocked + parked.
	if !strings.Contains(doc.OpenQuestions, "- blocked-5555: blocked") ||
		!strings.Contains(doc.OpenQuestions, "- parked-6666: awaiting operator decision") {
		t.Errorf("OpenQuestions: got %q", doc.OpenQuestions)
	}
	// 4. Key Decisions — the checkpoint decision line.
	if !strings.Contains(doc.KeyDecisions, "dispatched worker have-state-1111 (gen 1)") {
		t.Errorf("KeyDecisions: got %q", doc.KeyDecisions)
	}
	// 5. Files Modified — the .md AND the gitignored .html companion.
	if !strings.Contains(doc.FilesModified, "docs/DESIGN-x.md") ||
		!strings.Contains(doc.FilesModified, "docs/DESIGN-x.html") {
		t.Errorf("FilesModified: got %q", doc.FilesModified)
	}

	// Sanity (#236-shipped, must not regress): both subagents listed
	// (missing-state one with phase=""), both Open PRs present.
	if len(doc.ActiveSubagents) != 2 {
		t.Fatalf("ActiveSubagents: got %d want 2", len(doc.ActiveSubagents))
	}
	var missing *ActiveSubagent
	for i := range doc.ActiveSubagents {
		if doc.ActiveSubagents[i].TaskID == "no-state-2222" {
			missing = &doc.ActiveSubagents[i]
		}
	}
	if missing == nil || missing.LastPhase != "" {
		t.Errorf("emit-on-missing subagent: got %#v want phase=\"\"", missing)
	}
	if len(doc.OpenPRs) != 2 {
		t.Errorf("OpenPRs: got %d want 2", len(doc.OpenPRs))
	}
}

// T2 — recovery-synth parity: the four checkpoint/tasks.md-derived
// sections match the manual path; Files Modified stays placeholder (synth
// is subprocess-free).
func TestSynthesizeRecovery_Lifecycle_ParityExceptFilesModified(t *testing.T) {
	pdir := withFleetHomeSynth(t)
	now := time.Now().UTC()
	lifecycleState(t, pdir, now)

	doc, err := SynthesizeRecoveryWithLastHandoff("deadbeef", "myproj", "", now.Add(time.Second))
	if err != nil {
		t.Fatalf("SynthesizeRecoveryWithLastHandoff: %v", err)
	}
	if !strings.Contains(doc.Completed, "reconciled old-task → done") {
		t.Errorf("synth Completed: got %q", doc.Completed)
	}
	if doc.NextSteps != "- [P0] ready-3333: queued urgent\n- [P2] todo-4444: queued later" {
		t.Errorf("synth NextSteps: got %q", doc.NextSteps)
	}
	if !strings.Contains(doc.OpenQuestions, "blocked-5555") || !strings.Contains(doc.OpenQuestions, "parked-6666") {
		t.Errorf("synth OpenQuestions: got %q", doc.OpenQuestions)
	}
	if !strings.Contains(doc.KeyDecisions, "dispatched worker have-state-1111") {
		t.Errorf("synth KeyDecisions: got %q", doc.KeyDecisions)
	}
	// Files Modified stays placeholder — synth can't shell git.
	if doc.FilesModified != Placeholder {
		t.Errorf("synth FilesModified: got %q want placeholder", doc.FilesModified)
	}
}

// T4 — generation guard: a checkpoint stamped with a foreign coord_id is
// rejected, so the Completed + Key Decisions buffers are NOT lifted (fall
// back to placeholder / live state).
func TestEnrichManualDoc_ForeignGenerationRejectsCompletionsAndDecisions(t *testing.T) {
	pdir := withFleetHomeSynth(t)
	now := time.Now().UTC()
	// Checkpoint coord_id is "deadbeef" (seedCheckpointFull hardcodes it),
	// but we hand off a DIFFERENT coord → generation guard rejects.
	seedCheckpointFull(t, pdir, "myproj", now, nil,
		[]string{"- stale decision"}, []string{"- stale completion"})
	seedCoordState(t, pdir, "myproj", map[string]string{})
	fakeGH(t, []byte("[]"), nil)

	doc := NewManualStub("freshcoo", "coord-myproj", "myproj", 1, nil, now)
	EnrichManualDoc(doc, "myproj", "freshcoo", "", nil, nil)

	if doc.Completed != Placeholder {
		t.Errorf("Completed must NOT inherit stale generation: got %q", doc.Completed)
	}
	if doc.KeyDecisions != Placeholder {
		t.Errorf("KeyDecisions must NOT inherit stale generation: got %q", doc.KeyDecisions)
	}
}

// T5 — best-effort: a panicking collector must leave the section's
// placeholder and the handoff still completes. We inject a panicking
// gitRunner so CollectFilesModified's caller path is exercised under the
// top-level recover() guard.
func TestEnrichManualDoc_PanicLeavesPlaceholdersHandoffSucceeds(t *testing.T) {
	pdir := withFleetHomeSynth(t)
	now := time.Now().UTC()
	seedCoordState(t, pdir, "myproj", map[string]string{})
	fakeGH(t, []byte("[]"), nil)

	orig := gitRunner
	gitRunner = func(string, ...string) ([]byte, error) { panic("boom") }
	t.Cleanup(func() { gitRunner = orig })

	doc := NewManualStub("deadbeef", "coord-myproj", "myproj", 1, nil, now)
	// Must not panic out — the recover() guard swallows it.
	EnrichManualDoc(doc, "myproj", "deadbeef", "/some/repo", nil, nil)

	if doc.FilesModified != Placeholder {
		t.Errorf("FilesModified after panic: got %q want placeholder", doc.FilesModified)
	}
}
