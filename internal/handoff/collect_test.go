package handoff

// collect_test.go — lifecycle tests for the manual EnrichManualDoc +
// recovery SynthesizeRecovery paths: they assert all five body sections
// (Completed, Key Decisions, Docs (this session), Open Questions, Next
// Steps) fill end-to-end from durable on-disk state. The CollectSessionDocs
// / CollectRecentDecisionsLive unit tests live in sessiondocs_test.go; the
// CollectNextSteps / CollectOpenQuestions unit tests in narrative_test.go.

import (
	"encoding/json"
	"os"
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

// addSessionDocs merges a session_docs list into the project's existing
// coord-state.json (preserving worker_agent_ids etc.), mirroring what
// `fleet checkpoint doc` writes. ts is fixed so the fixture is deterministic.
func addSessionDocs(t *testing.T, projectsRoot, project string, docs [][2]string) {
	t.Helper()
	path := filepath.Join(projectsRoot, project, "coord-state.json")
	m := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &m)
	}
	var arr []any
	for _, d := range docs {
		arr = append(arr, map[string]any{"path": d[1], "role": d[0], "ts": "2026-07-02T00:00:00Z"})
	}
	m["session_docs"] = arr
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatalf("marshal coord-state: %v", err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatalf("write coord-state: %v", err)
	}
}

// addSessionScoped merges session_next_steps (explicit free-text) +
// session_tasks (auto slug set) into the project's coord-state.json, each
// entry stamped with coordID — mirroring what `fleet checkpoint next-step`
// and the tick's _record_session_task write. These drive the session-scoped
// Next Steps / Open Questions collectors.
func addSessionScoped(t *testing.T, projectsRoot, project, coordID string, nextStepTexts, taskSlugs []string) {
	t.Helper()
	path := filepath.Join(projectsRoot, project, "coord-state.json")
	m := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &m)
	}
	var steps []any
	for _, txt := range nextStepTexts {
		steps = append(steps, map[string]any{"text": txt, "coord_id": coordID, "ts": "2026-07-02T00:00:00Z"})
	}
	var tsk []any
	for _, slug := range taskSlugs {
		tsk = append(tsk, map[string]any{"slug": slug, "coord_id": coordID, "ts": "2026-07-02T00:00:00Z"})
	}
	m["session_next_steps"] = steps
	m["session_tasks"] = tsk
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatalf("marshal coord-state: %v", err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatalf("write coord-state: %v", err)
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
	// Plan docs this coord recorded via `fleet checkpoint doc` — merged into
	// the same coord-state.json (source for CollectSessionDocs).
	addSessionDocs(t, pdir, "myproj", [][2]string{
		{"authored", "docs/DESIGN-x.md"},
		{"implementing", "docs/TASK-PLAN-x.md"},
	})
	// Session-scoped Next Steps / Open Questions buffers (coord_id "deadbeef"
	// matches the handing-off coord id both lifecycle tests pass). One
	// explicit free-text step + the slugs this coord acted on this session.
	addSessionScoped(t, pdir, "myproj", "deadbeef",
		[]string{"revive codex-engine-mvp"},
		[]string{"have-state-1111", "ready-3333", "todo-4444", "blocked-5555", "parked-6666"})
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

	// gh returns two open worker PRs.
	fakeGH(t, ghJSON(t, []ghOpenPR{
		{Number: 1, Title: "p1", HeadRefName: "worker/have-state-1111", URL: "https://github.com/o/r/pull/1"},
		{Number: 2, Title: "p2", HeadRefName: "worker/no-state-2222", URL: "https://github.com/o/r/pull/2"},
	}), nil)

	doc := NewManualStub("deadbeef", "coord-myproj", "myproj", 1, nil, now)
	// repoDir is now only used for `gh pr list` (faked here); session docs
	// come from coord-state.json, not the repo. Pass "" to prove that.
	EnrichManualDoc(doc, "myproj", "deadbeef", "", nil, nil)

	// 1. Completed — the two checkpoint completion lines; NOT a dispatch.
	if !strings.Contains(doc.Completed, "reconciled old-task → done") ||
		!strings.Contains(doc.Completed, "PR merged → task merged-task done") {
		t.Errorf("Completed: got %q", doc.Completed)
	}
	if strings.Contains(doc.Completed, "dispatched") {
		t.Errorf("Completed must not contain dispatch events: %q", doc.Completed)
	}
	// 2. Next Steps — SESSION-SCOPED: explicit first, then this coord's
	// promoted/dispatched slugs still ready/todo (priority-desc); the
	// in-progress session slug (have-state-1111) is NOT double-listed here.
	wantNext := "- [explicit] revive codex-engine-mvp\n" +
		"- [auto] [P0] ready-3333: queued urgent\n" +
		"- [auto] [P2] todo-4444: queued later"
	if doc.NextSteps != wantNext {
		t.Errorf("NextSteps: got %q want %q", doc.NextSteps, wantNext)
	}
	if strings.Contains(doc.NextSteps, "have-state-1111") {
		t.Errorf("NextSteps must exclude in-progress session slug: %q", doc.NextSteps)
	}
	// 3. Open Questions — SESSION-SCOPED blocked + parked slugs only.
	if !strings.Contains(doc.OpenQuestions, "- blocked-5555: blocked") ||
		!strings.Contains(doc.OpenQuestions, "- parked-6666: awaiting operator decision") {
		t.Errorf("OpenQuestions: got %q", doc.OpenQuestions)
	}
	// 4. Key Decisions — the checkpoint decision line.
	if !strings.Contains(doc.KeyDecisions, "dispatched worker have-state-1111 (gen 1)") {
		t.Errorf("KeyDecisions: got %q", doc.KeyDecisions)
	}
	// 5. Docs (this session) — the recorded plan docs, role-tagged; never a
	// git dump of every untracked file.
	if !strings.Contains(doc.SessionDocs, "- authored: docs/DESIGN-x.md") ||
		!strings.Contains(doc.SessionDocs, "- implementing: docs/TASK-PLAN-x.md") {
		t.Errorf("SessionDocs: got %q", doc.SessionDocs)
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

// T2 — recovery-synth parity: ALL five narrative sections match the
// manual path. Docs (this session) now fills on the synth path too —
// coord-state.json:session_docs is a pure FS read, so synth stays
// subprocess-free while gaining the section the old git-shell-out
// CollectFilesModified could never give it.
func TestSynthesizeRecovery_Lifecycle_FullSectionParity(t *testing.T) {
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
	// T15 recovery parity: the synth (dead-coord) path fills Next Steps
	// identically to the live manual path — same coord-state source, same
	// coord id.
	wantSynthNext := "- [explicit] revive codex-engine-mvp\n" +
		"- [auto] [P0] ready-3333: queued urgent\n" +
		"- [auto] [P2] todo-4444: queued later"
	if doc.NextSteps != wantSynthNext {
		t.Errorf("synth NextSteps: got %q want %q", doc.NextSteps, wantSynthNext)
	}
	if !strings.Contains(doc.OpenQuestions, "blocked-5555") || !strings.Contains(doc.OpenQuestions, "parked-6666") {
		t.Errorf("synth OpenQuestions: got %q", doc.OpenQuestions)
	}
	if !strings.Contains(doc.KeyDecisions, "dispatched worker have-state-1111") {
		t.Errorf("synth KeyDecisions: got %q", doc.KeyDecisions)
	}
	// Docs (this session) — same coord-state source as the manual path.
	if !strings.Contains(doc.SessionDocs, "- authored: docs/DESIGN-x.md") ||
		!strings.Contains(doc.SessionDocs, "- implementing: docs/TASK-PLAN-x.md") {
		t.Errorf("synth SessionDocs: got %q", doc.SessionDocs)
	}
}

// T15b — recovery, empty session buffers: a crashed coord that never
// recorded a next-step / session-task hands off a PLACEHOLDER Next Steps +
// Open Questions, NOT a dump of the (present) ready/blocked backlog.
func TestSynthesizeRecovery_EmptyBuffersPlaceholderNoBacklogDump(t *testing.T) {
	pdir := withFleetHomeSynth(t)
	now := time.Now().UTC()
	seedCoordState(t, pdir, "myproj", map[string]string{})
	// A real backlog exists in tasks.md — none of it is session-scoped.
	seedRichTasksMD(t, pdir, "myproj", []*tasks.Task{
		{Slug: "old-ready-1", Status: tasks.StatusReady, Priority: tasks.PriorityP0, Spec: "ancient P0"},
		{Slug: "old-blocked-1", Status: tasks.StatusBlocked, Priority: tasks.PriorityP0, Spec: "stuck"},
	})

	doc, err := SynthesizeRecoveryWithLastHandoff("deadbeef", "myproj", "", now.Add(time.Second))
	if err != nil {
		t.Fatalf("SynthesizeRecoveryWithLastHandoff: %v", err)
	}
	if doc.NextSteps != Placeholder {
		t.Errorf("empty session → Next Steps must be placeholder, got: %q", doc.NextSteps)
	}
	if doc.OpenQuestions != Placeholder {
		t.Errorf("empty session → Open Questions must be placeholder, got: %q", doc.OpenQuestions)
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

// T5 — best-effort: a panicking collector must leave the sections'
// placeholders and the handoff still completes. We inject a panicking
// ghRunner (the one remaining subprocess seam now that the git shell-out
// is retired) so CollectOpenPRs blows up mid-enrichment and the
// top-level recover() guard is exercised. The gh panic fires BEFORE the
// SessionDocs collection, so Docs (this session) keeps its placeholder
// too — proving a partial enrichment never corrupts the doc.
func TestEnrichManualDoc_PanicLeavesPlaceholdersHandoffSucceeds(t *testing.T) {
	pdir := withFleetHomeSynth(t)
	now := time.Now().UTC()
	seedCoordState(t, pdir, "myproj", map[string]string{})

	orig := ghRunner
	ghRunner = func(string, ...string) ([]byte, error) { panic("boom") }
	t.Cleanup(func() { ghRunner = orig })

	doc := NewManualStub("deadbeef", "coord-myproj", "myproj", 1, nil, now)
	// Must not panic out — the recover() guard swallows it.
	EnrichManualDoc(doc, "myproj", "deadbeef", "/some/repo", nil, nil)

	if doc.SessionDocs != Placeholder {
		t.Errorf("SessionDocs after panic: got %q want placeholder", doc.SessionDocs)
	}
	if doc.KeyDecisions != Placeholder {
		t.Errorf("KeyDecisions after panic: got %q want placeholder", doc.KeyDecisions)
	}
	if len(doc.OpenPRs) != 0 {
		t.Errorf("OpenPRs after panic: got %v want none", doc.OpenPRs)
	}
}
