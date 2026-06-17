// Tests for enrich.go — filling the machine-state sections (Active
// Subagents + Open PRs) of a MANUAL handoff doc from on-disk state +
// `gh`, with LIVE (emit-on-missing) semantics that diverge from synth's
// recovery skip. Best-effort: enrichment never fails a handoff.
//
// No real `gh` and no real tmux are touched: ghRunner is swapped for a
// fake per-test (restored via t.Cleanup), and enrichment touches only
// the FLEET_HOME temp tree (t.TempDir, auto-cleaned). Zero
// /tmp/fleet-test-*.sock are created by this file.
package handoff

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/tasks"
)

// fakeGH swaps ghRunner for a stub returning (out, err) and restores the
// production runner on test end. Centralizes the seam so no test forgets
// the restore (which would leak a fake into a sibling test).
func fakeGH(t *testing.T, out []byte, err error) {
	t.Helper()
	prev := ghRunner
	ghRunner = func(dir string, args ...string) ([]byte, error) { return out, err }
	t.Cleanup(func() { ghRunner = prev })
}

// seedTasksMDWithPR writes a tasks.md carrying a pr_url per slug (empty
// pr_url = no PR). Used by the gh-failure tasks.md-fallback test.
func seedTasksMDWithPR(t *testing.T, projectsRoot, project string, prURLBySlug map[string]string) {
	t.Helper()
	dir := filepath.Join(projectsRoot, project)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	f := &tasks.File{Schema: 1}
	slugs := make([]string, 0, len(prURLBySlug))
	for s := range prURLBySlug {
		slugs = append(slugs, s)
	}
	sort.Strings(slugs)
	for _, s := range slugs {
		f.Tasks = append(f.Tasks, &tasks.Task{
			Slug:     s,
			Status:   tasks.Status("in-review"),
			Priority: tasks.Priority("P1"),
			PRURL:    prURLBySlug[s],
			Created:  now,
			Updated:  now,
			Spec:     "enrich-test task " + s,
		})
	}
	if err := tasks.Write(filepath.Join(dir, "tasks.md"), f); err != nil {
		t.Fatalf("write tasks.md: %v", err)
	}
}

// seedTasksMDWithPRAndStatus writes a tasks.md where each slug maps to
// [status, pr_url]. Used to exercise the PR-open promotion when tasks.md
// (not state.json) is the pr_url source.
func seedTasksMDWithPRAndStatus(t *testing.T, projectsRoot, project string, m map[string][2]string) {
	t.Helper()
	dir := filepath.Join(projectsRoot, project)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	f := &tasks.File{Schema: 1}
	slugs := make([]string, 0, len(m))
	for s := range m {
		slugs = append(slugs, s)
	}
	sort.Strings(slugs)
	for _, s := range slugs {
		f.Tasks = append(f.Tasks, &tasks.Task{
			Slug:     s,
			Status:   tasks.Status(m[s][0]),
			Priority: tasks.Priority("P1"),
			PRURL:    m[s][1],
			Created:  now,
			Updated:  now,
			Spec:     "enrich-test task " + s,
		})
	}
	if err := tasks.Write(filepath.Join(dir, "tasks.md"), f); err != nil {
		t.Fatalf("write tasks.md: %v", err)
	}
}

// ghJSON marshals a slice of ghOpenPR to the JSON shape `gh pr list
// --json number,title,headRefName,url` emits, so the parse path is
// exercised end-to-end.
func ghJSON(t *testing.T, prs []ghOpenPR) []byte {
	t.Helper()
	data, err := json.Marshal(prs)
	if err != nil {
		t.Fatalf("marshal gh json: %v", err)
	}
	return data
}

// --- Case 1 (live-first, codex Slice-1 P1): fresh checkpoint present
// BUT live coord-state.json is the authoritative source on a live manual
// handoff → Active Subagents come from the LIVE walk (not the stale
// checkpoint); Open PRs from gh. The checkpoint narrative (decisions →
// NextSteps) is still lifted as a supplement. ---
func TestEnrichManualDoc_LiveStateWinsOverCheckpoint(t *testing.T) {
	pdir := withFleetHomeSynth(t)
	now := time.Now().UTC()

	// LIVE state: the current truth — fix-live-9999 is in flight now.
	seedCoordState(t, pdir, "myproj", map[string]string{"fix-live-9999": "livebeef"})
	seedWorkerState(t, pdir, "myproj", "fix-live-9999", "tdd-green", "")

	// Checkpoint is fresher than the last handoff but STALE vs live —
	// it references work that has since moved on.
	activeRow := `- task="fix-old-1234" branch="worker/fix-old-1234" phase="push" status="in-review" pr_url="https://github.com/o/r/pull/77" agent_id="cafef00d" subagent_id=""`
	seedCheckpoint(t, pdir, "myproj", now, []string{activeRow}, nil,
		[]string{"- dispatched fix-live-9999"})
	lastHandoff := writeFakeHandoffDoc(t, pdir, "deadbeef", now.Add(-time.Hour))

	fakeGH(t, ghJSON(t, []ghOpenPR{
		{Number: 9, Title: "live", HeadRefName: "worker/fix-live-9999", URL: "https://github.com/o/r/pull/9"},
	}), nil)

	doc := NewManualStub("deadbeef", "coord-myproj", "myproj", 1, &lastHandoff, now)
	EnrichManualDoc(doc, "myproj", "deadbeef", "", &lastHandoff, nil)

	if len(doc.ActiveSubagents) != 1 || doc.ActiveSubagents[0].TaskID != "fix-live-9999" {
		t.Fatalf("ActiveSubagents: got %#v want one fix-live-9999 (LIVE wins over checkpoint)", doc.ActiveSubagents)
	}
	if len(doc.OpenPRs) != 1 || doc.OpenPRs[0].Number != 9 {
		t.Fatalf("OpenPRs: got %#v want one #9 (gh)", doc.OpenPRs)
	}
	// Narrative supplement from checkpoint still applied.
	if !strings.Contains(doc.NextSteps, "dispatched fix-live-9999") {
		t.Errorf("NextSteps: got %q want checkpoint decisions lifted", doc.NextSteps)
	}
}

// --- Checkpoint-as-fallback: live walk yields nothing (coord-state
// unreadable / empty) → fall back to the checkpoint's Active Subagents. ---
func TestEnrichManualDoc_CheckpointFallbackWhenNoLiveState(t *testing.T) {
	pdir := withFleetHomeSynth(t)
	now := time.Now().UTC()
	// No coord-state.json → live walk returns nil.
	activeRow := `- task="cp-only-1234" branch="worker/cp-only-1234" phase="push" status="in-review" pr_url="https://github.com/o/r/pull/77" agent_id="cafef00d" subagent_id=""`
	seedCheckpoint(t, pdir, "myproj", now, []string{activeRow}, nil, nil)
	lastHandoff := writeFakeHandoffDoc(t, pdir, "deadbeef", now.Add(-time.Hour))
	fakeGH(t, []byte("[]"), nil)

	doc := NewManualStub("deadbeef", "coord-myproj", "myproj", 1, &lastHandoff, now)
	EnrichManualDoc(doc, "myproj", "deadbeef", "", &lastHandoff, nil)

	if len(doc.ActiveSubagents) != 1 || doc.ActiveSubagents[0].TaskID != "cp-only-1234" {
		t.Fatalf("ActiveSubagents: got %#v want cp-only-1234 (checkpoint fallback)", doc.ActiveSubagents)
	}
}

// --- codex Slice-1 P1: coord-state.json present with an EMPTY worker map
// (all workers finished since the last checkpoint) is AUTHORITATIVE —
// ## Active Subagents must be CLEARED, not filled from the stale
// checkpoint that still lists finished workers. ---
func TestEnrichManualDoc_EmptyLiveStateClearsStaleCheckpoint(t *testing.T) {
	pdir := withFleetHomeSynth(t)
	now := time.Now().UTC()
	// coord-state.json exists but has NO workers (all done).
	seedCoordState(t, pdir, "myproj", map[string]string{})
	// Checkpoint still lists a (now-finished) worker.
	staleRow := `- task="finished-1234" branch="worker/finished-1234" phase="push" status="in-review" pr_url="https://github.com/o/r/pull/77" agent_id="cafef00d" subagent_id=""`
	seedCheckpoint(t, pdir, "myproj", now, []string{staleRow}, nil, nil)
	lastHandoff := writeFakeHandoffDoc(t, pdir, "deadbeef", now.Add(-time.Hour))
	fakeGH(t, []byte("[]"), nil)

	doc := NewManualStub("deadbeef", "coord-myproj", "myproj", 1, &lastHandoff, now)
	EnrichManualDoc(doc, "myproj", "deadbeef", "", &lastHandoff, nil)

	if len(doc.ActiveSubagents) != 0 {
		t.Fatalf("ActiveSubagents: got %#v want EMPTY (authoritative live state clears stale checkpoint)", doc.ActiveSubagents)
	}
	// Renders the _(none)_ placeholder — successor resumes no finished work.
	if body := string(Render(doc)); !strings.Contains(body, "## Active Subagents\n"+ActiveSubagentsNonePlaceholder) {
		t.Errorf("rendered doc should show Active Subagents placeholder; got:\n%s", body)
	}
}

// --- codex Slice-1 P2: gh fails, tasks.md is READABLE with zero open PRs
// (last PR merged), checkpoint still lists a stale PR → tasks.md is
// authoritative; checkpoint PRs are NOT revived. ---
func TestEnrichManualDoc_ReadableEmptyTasksMDDoesNotReviveCheckpointPRs(t *testing.T) {
	pdir := withFleetHomeSynth(t)
	now := time.Now().UTC()
	seedCoordState(t, pdir, "myproj", map[string]string{"w-1": "idA"})
	seedWorkerState(t, pdir, "myproj", "w-1", "push", "")
	// tasks.md readable, but the task's PR already MERGED (done) → no open PRs.
	seedTasksMDWithPRAndStatus(t, pdir, "myproj", map[string][2]string{
		"w-1": {"done", "https://github.com/o/r/pull/merged"},
	})
	// Checkpoint still carries a stale open-PR row.
	staleRow := "- #99 stale — worker/old-9999 — https://github.com/o/r/pull/99"
	seedCheckpoint(t, pdir, "myproj", now, nil, []string{staleRow}, nil)
	lastHandoff := writeFakeHandoffDoc(t, pdir, "deadbeef", now.Add(-time.Hour))

	fakeGH(t, nil, errors.New("gh down"))

	doc := NewManualStub("deadbeef", "coord-myproj", "myproj", 1, &lastHandoff, now)
	EnrichManualDoc(doc, "myproj", "deadbeef", "", &lastHandoff, nil)

	if len(doc.OpenPRs) != 0 {
		t.Fatalf("OpenPRs: got %#v want EMPTY (readable tasks.md authoritative; no checkpoint revival)", doc.OpenPRs)
	}
}

// --- codex Slice-1 P2: gh down, a worker has a pr_url in state.json but
// tasks.md has NOT been stamped → the PR is preserved in ## Open PRs
// (derived from live worker state), so shepherding restarts. ---
func TestEnrichManualDoc_GhDownPreservesStateJSONPRURL(t *testing.T) {
	pdir := withFleetHomeSynth(t)
	now := time.Now().UTC()
	seedCoordState(t, pdir, "myproj", map[string]string{"w-1": "idA"})
	// state.json carries the pr_url; tasks.md row is in-progress, no pr_url.
	seedWorkerState(t, pdir, "myproj", "w-1", "push", "https://github.com/o/r/pull/61")
	seedTasksMDWithPRAndStatus(t, pdir, "myproj", map[string][2]string{
		"w-1": {"in-progress", ""},
	})

	fakeGH(t, nil, errors.New("gh down"))

	doc := NewManualStub("deadbeef", "coord-myproj", "myproj", 1, nil, now)
	EnrichManualDoc(doc, "myproj", "deadbeef", "", nil, nil)

	if len(doc.OpenPRs) != 1 || doc.OpenPRs[0].URL != "https://github.com/o/r/pull/61" {
		t.Fatalf("OpenPRs: got %#v want pull/61 (preserved from state.json when gh down)", doc.OpenPRs)
	}
}

// --- codex Slice-1 P2: coord-state.json WITHOUT a worker_agent_ids key
// (legacy / partial upgrade) is NOT authoritative → fall back to the
// checkpoint's Active Subagents rather than clearing the section. ---
func TestEnrichManualDoc_LegacyCoordStateFallsBackToCheckpoint(t *testing.T) {
	pdir := withFleetHomeSynth(t)
	now := time.Now().UTC()
	// coord-state.json present but NO worker_agent_ids key.
	dir := filepath.Join(pdir, "myproj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "coord-state.json"), []byte(`{"schema":1}`), 0o644); err != nil {
		t.Fatalf("write coord-state: %v", err)
	}
	cpRow := `- task="cp-worker-1234" branch="worker/cp-worker-1234" phase="push" status="in-review" pr_url="https://github.com/o/r/pull/12" agent_id="cafef00d" subagent_id=""`
	seedCheckpoint(t, pdir, "myproj", now, []string{cpRow}, nil, nil)
	lastHandoff := writeFakeHandoffDoc(t, pdir, "deadbeef", now.Add(-time.Hour))
	fakeGH(t, []byte("[]"), nil)

	doc := NewManualStub("deadbeef", "coord-myproj", "myproj", 1, &lastHandoff, now)
	EnrichManualDoc(doc, "myproj", "deadbeef", "", &lastHandoff, nil)

	if len(doc.ActiveSubagents) != 1 || doc.ActiveSubagents[0].TaskID != "cp-worker-1234" {
		t.Fatalf("ActiveSubagents: got %#v want cp-worker-1234 (legacy coord-state → checkpoint fallback)", doc.ActiveSubagents)
	}
}

// --- codex Slice-1 P2: gh down AND tasks.md has SCHEMA DRIFT (newer
// schema header that tasks.Read would refuse) but still contains a valid
// `## task:` block with an open PR → the tolerant scan recovers that PR
// (a downgraded binary / schema bump must not lose in-review shepherds
// that exist only in tasks.md). ---
func TestEnrichManualDoc_SchemaDriftTasksMDStillRecoversPR(t *testing.T) {
	pdir := withFleetHomeSynth(t)
	now := time.Now().UTC()
	// A task already moved to in-review and DROPPED from the worker map —
	// it lives only in tasks.md, not coord-state.json.
	seedCoordState(t, pdir, "myproj", map[string]string{})
	// tasks.md claims a future schema (tasks.Read would refuse the whole
	// file) yet carries a well-formed task block with a pr_url.
	dir := filepath.Join(pdir, "myproj")
	body := "---\nschema: v99\n---\n\n" +
		"## task: dropped-1234\n" +
		"- status: in-review\n" +
		"- pr_url: https://github.com/o/r/pull/88\n\n" +
		"### Spec\n- not a field bullet\n"
	if err := os.WriteFile(filepath.Join(dir, "tasks.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write schema-drift tasks.md: %v", err)
	}
	lastHandoff := writeFakeHandoffDoc(t, pdir, "deadbeef", now.Add(-time.Hour))

	fakeGH(t, nil, errors.New("gh down"))

	doc := NewManualStub("deadbeef", "coord-myproj", "myproj", 1, &lastHandoff, now)
	EnrichManualDoc(doc, "myproj", "deadbeef", "", &lastHandoff, nil)

	if len(doc.OpenPRs) != 1 || doc.OpenPRs[0].URL != "https://github.com/o/r/pull/88" {
		t.Fatalf("OpenPRs: got %#v want pull/88 (tolerant scan recovers despite schema drift)", doc.OpenPRs)
	}
}

// --- codex Slice-1 P2: under schema drift, the live walk overlays
// status/pr_url via the SAME tolerant scan as Open PRs, so the two
// sections agree — a PR-open worker is promoted to in-review (shepherd-
// only) instead of being re-dispatched while a shepherd also respawns. ---
func TestEnrichManualDoc_SchemaDriftLiveWalkStaysConsistent(t *testing.T) {
	pdir := withFleetHomeSynth(t)
	now := time.Now().UTC()
	seedCoordState(t, pdir, "myproj", map[string]string{"w-1": "idA"})
	seedWorkerState(t, pdir, "myproj", "w-1", "push", "") // state.json has no pr_url
	// tasks.md: future schema header (tasks.Read would refuse) + a valid
	// block carrying status=in-progress + a pr_url.
	dir := filepath.Join(pdir, "myproj")
	body := "---\nschema: v99\n---\n\n## task: w-1\n- status: in-progress\n- pr_url: https://github.com/o/r/pull/42\n"
	if err := os.WriteFile(filepath.Join(dir, "tasks.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write tasks.md: %v", err)
	}
	fakeGH(t, nil, errors.New("gh down"))

	doc := NewManualStub("deadbeef", "coord-myproj", "myproj", 1, nil, now)
	EnrichManualDoc(doc, "myproj", "deadbeef", "", nil, nil)

	if len(doc.ActiveSubagents) != 1 {
		t.Fatalf("ActiveSubagents: got %d want 1", len(doc.ActiveSubagents))
	}
	// status recovered + promoted to in-review (PR open) — NOT re-dispatch.
	if doc.ActiveSubagents[0].Status != "in-review" {
		t.Errorf("status: got %q want in-review (tolerant scan recovered pr_url → promote)", doc.ActiveSubagents[0].Status)
	}
	// Open PRs section carries the same PR — consistent, no duplicate.
	if len(doc.OpenPRs) != 1 || doc.OpenPRs[0].URL != "https://github.com/o/r/pull/42" {
		t.Errorf("OpenPRs: got %#v want pull/42 (consistent with Active Subagents)", doc.OpenPRs)
	}
}

// scanTasksMetaTolerant unit edges: prose bullets in Spec/Notes are not
// mistaken for task fields; the footer H2 stops scanning.
func TestScanTasksMetaTolerant_IgnoresProseBullets(t *testing.T) {
	body := "## task: a-1\n" +
		"- status: in-progress\n" +
		"- pr_url: https://x/1\n" +
		"### Spec\n" +
		"- status: THIS IS PROSE not a field\n" +
		"- pr_url: https://evil/should-not-win\n\n" +
		"## footer\n" +
		"- status: ignored\n"
	meta := scanTasksMetaTolerant([]byte(body))
	if len(meta) != 1 {
		t.Fatalf("meta: got %d entries want 1 (footer H2 stops scan)", len(meta))
	}
	got := meta["a-1"]
	if got.status != "in-progress" || got.prURL != "https://x/1" {
		t.Errorf("a-1 meta: got %+v want {in-progress https://x/1} (prose bullets ignored)", got)
	}
}

// scanTasksMetaTolerant: a blank line WITHIN the leading field bullets
// (hand edit / future writer) must NOT stop field reading (matches
// handoff.py reopen behavior; codex Slice-1 P2).
func TestScanTasksMetaTolerant_BlankLineInBulletsKeepsScanning(t *testing.T) {
	body := "## task: a-1\n" +
		"- status: in-review\n" +
		"\n" + // blank line splits the field bullets
		"- pr_url: https://x/77\n"
	meta := scanTasksMetaTolerant([]byte(body))
	got := meta["a-1"]
	if got.status != "in-review" || got.prURL != "https://x/77" {
		t.Errorf("a-1 meta: got %+v want {in-review https://x/77} (blank line must not stop field scan)", got)
	}
}

// --- Case 2: no checkpoint, live workers on disk → Active Subagents
// from emit-on-missing walk; Open PRs from gh. ---
func TestEnrichManualDoc_NoCheckpointLiveWalk(t *testing.T) {
	pdir := withFleetHomeSynth(t)
	now := time.Now().UTC()
	seedCoordState(t, pdir, "myproj", map[string]string{
		"fix-foo-1234": "deadbeef",
		"fix-bar-5678": "cafef00d",
	})
	seedWorkerState(t, pdir, "myproj", "fix-foo-1234", "tdd-green", "")
	seedWorkerState(t, pdir, "myproj", "fix-bar-5678", "push", "https://github.com/o/r/pull/9")

	fakeGH(t, ghJSON(t, []ghOpenPR{
		{Number: 9, Title: "bar", HeadRefName: "worker/fix-bar-5678", URL: "https://github.com/o/r/pull/9"},
	}), nil)

	doc := NewManualStub("deadbeef", "coord-myproj", "myproj", 1, nil, now)
	EnrichManualDoc(doc, "myproj", "deadbeef", "", nil, nil)

	if len(doc.ActiveSubagents) != 2 {
		t.Fatalf("ActiveSubagents: got %d want 2 (live walk)", len(doc.ActiveSubagents))
	}
	// Sorted by slug: bar before foo.
	if doc.ActiveSubagents[0].TaskID != "fix-bar-5678" || doc.ActiveSubagents[1].TaskID != "fix-foo-1234" {
		t.Errorf("order: got %q,%q want fix-bar-5678,fix-foo-1234",
			doc.ActiveSubagents[0].TaskID, doc.ActiveSubagents[1].TaskID)
	}
	if doc.ActiveSubagents[1].LastPhase != "tdd-green" {
		t.Errorf("foo phase: got %q want tdd-green", doc.ActiveSubagents[1].LastPhase)
	}
	if len(doc.OpenPRs) != 1 || doc.OpenPRs[0].Number != 9 {
		t.Fatalf("OpenPRs: got %#v want one #9", doc.OpenPRs)
	}
}

// --- Case 3: gh fails / non-git repo → Open PRs fall back to checkpoint,
// then placeholder; handoff doc still renders with the placeholder. ---
func TestEnrichManualDoc_GhFailsFallsBackToPlaceholder(t *testing.T) {
	pdir := withFleetHomeSynth(t)
	now := time.Now().UTC()
	seedCoordState(t, pdir, "myproj", map[string]string{"fix-foo-1234": "deadbeef"})
	seedWorkerState(t, pdir, "myproj", "fix-foo-1234", "tdd-green", "")

	fakeGH(t, nil, errors.New("not a git repository")) // gh exits non-zero

	doc := NewManualStub("deadbeef", "coord-myproj", "myproj", 1, nil, now)
	EnrichManualDoc(doc, "myproj", "deadbeef", "", nil, nil)

	if len(doc.OpenPRs) != 0 {
		t.Fatalf("OpenPRs: got %#v want empty (gh failed, no checkpoint fallback)", doc.OpenPRs)
	}
	// Renders the placeholder, handoff succeeds.
	if body := string(Render(doc)); !strings.Contains(body, "## Open PRs\n"+OpenPRsNonePlaceholder) {
		t.Errorf("rendered doc missing Open PRs placeholder; got:\n%s", body)
	}
	// Active Subagents still filled from the live walk despite gh failure.
	if len(doc.ActiveSubagents) != 1 {
		t.Errorf("ActiveSubagents: got %d want 1 (live walk independent of gh)", len(doc.ActiveSubagents))
	}
}

// --- Case 3b: gh fails BUT a checkpoint carried Open PRs → fall back to
// the checkpoint's snapshot rather than empty. ---
func TestEnrichManualDoc_GhFailsFallsBackToCheckpointPRs(t *testing.T) {
	pdir := withFleetHomeSynth(t)
	now := time.Now().UTC()
	prRow := "- #50 fix: cp — worker/fix-cp-0001 — https://github.com/o/r/pull/50"
	seedCheckpoint(t, pdir, "myproj", now, nil, []string{prRow}, nil)
	lastHandoff := writeFakeHandoffDoc(t, pdir, "deadbeef", now.Add(-time.Hour))

	fakeGH(t, nil, errors.New("gh down"))

	doc := NewManualStub("deadbeef", "coord-myproj", "myproj", 1, &lastHandoff, now)
	EnrichManualDoc(doc, "myproj", "deadbeef", "", &lastHandoff, nil)

	if len(doc.OpenPRs) != 1 || doc.OpenPRs[0].Number != 50 {
		t.Fatalf("OpenPRs: got %#v want checkpoint fallback #50", doc.OpenPRs)
	}
}

// --- Case 3c (codex Slice-1 P1): gh fails, NO checkpoint snapshot, but
// tasks.md carries pr_url per slug → fall back to tasks.md so the
// successor still re-spawns shepherds for in-review PRs (rather than
// dropping all PR supervision). ---
func TestEnrichManualDoc_GhFailsFallsBackToTasksMD(t *testing.T) {
	pdir := withFleetHomeSynth(t)
	now := time.Now().UTC()
	seedCoordState(t, pdir, "myproj", map[string]string{
		"fix-foo-1234": "deadbeef",
		"fix-bar-5678": "cafef00d",
	})
	seedWorkerState(t, pdir, "myproj", "fix-foo-1234", "push", "")
	seedWorkerState(t, pdir, "myproj", "fix-bar-5678", "push", "")
	// tasks.md: foo has an open PR, bar does not.
	seedTasksMDWithPR(t, pdir, "myproj", map[string]string{
		"fix-foo-1234": "https://github.com/o/r/pull/42",
		"fix-bar-5678": "",
	})

	fakeGH(t, nil, errors.New("not a git repository")) // gh fails, no checkpoint

	doc := NewManualStub("deadbeef", "coord-myproj", "myproj", 1, nil, now)
	EnrichManualDoc(doc, "myproj", "deadbeef", "", nil, nil)

	if len(doc.OpenPRs) != 1 {
		t.Fatalf("OpenPRs: got %#v want 1 (tasks.md fallback for the slug with a pr_url)", doc.OpenPRs)
	}
	pr := doc.OpenPRs[0]
	if pr.URL != "https://github.com/o/r/pull/42" {
		t.Errorf("PR URL: got %q want pull/42", pr.URL)
	}
	if pr.HeadRefName != "worker/fix-foo-1234" {
		t.Errorf("PR head: got %q want worker/fix-foo-1234", pr.HeadRefName)
	}
	// The rendered row must carry a trailing URL so handoff_resume.py's
	// shepherd-respawn regex (\s—\s(https?://\S+)$) matches it.
	body := string(Render(doc))
	if !strings.Contains(body, " — https://github.com/o/r/pull/42") {
		t.Errorf("rendered Open PRs row missing trailing URL; got:\n%s", body)
	}
}

// --- Case 3d (codex Slice-1 P2): tasks.md fallback excludes terminal
// (done/abandoned) tasks even when they carry a pr_url, so completed PRs
// are not reintroduced as live shepherd watches. ---
func TestEnrichManualDoc_TasksFallbackExcludesTerminal(t *testing.T) {
	pdir := withFleetHomeSynth(t)
	now := time.Now().UTC()
	seedCoordState(t, pdir, "myproj", map[string]string{"live-1111": "deadbeef"})
	seedWorkerState(t, pdir, "myproj", "live-1111", "push", "")

	// Hand-build tasks.md with mixed statuses, all carrying a pr_url.
	dir := filepath.Join(pdir, "myproj")
	f := &tasks.File{Schema: 1}
	add := func(slug string, st tasks.Status) {
		f.Tasks = append(f.Tasks, &tasks.Task{
			Slug: slug, Status: st, Priority: tasks.Priority("P1"),
			PRURL: "https://github.com/o/r/pull/" + slug, Created: now, Updated: now, Spec: "x",
		})
	}
	add("a-inreview", tasks.StatusInReview)
	add("b-done", tasks.StatusDone)
	add("c-abandoned", tasks.StatusAbandoned)
	add("d-inprogress", tasks.StatusInProgress)
	if err := tasks.Write(filepath.Join(dir, "tasks.md"), f); err != nil {
		t.Fatalf("write tasks.md: %v", err)
	}

	fakeGH(t, nil, errors.New("gh down")) // force tasks.md fallback

	doc := NewManualStub("deadbeef", "coord-myproj", "myproj", 1, nil, now)
	EnrichManualDoc(doc, "myproj", "deadbeef", "", nil, nil)

	got := map[string]bool{}
	for _, pr := range doc.OpenPRs {
		got[pr.Title] = true // Title = slug for tasks.md rows
	}
	if !got["a-inreview"] || !got["d-inprogress"] {
		t.Errorf("expected in-review + in-progress PRs kept; got %#v", doc.OpenPRs)
	}
	if got["b-done"] || got["c-abandoned"] {
		t.Errorf("terminal (done/abandoned) PRs must be excluded; got %#v", doc.OpenPRs)
	}
	if len(doc.OpenPRs) != 2 {
		t.Errorf("OpenPRs: got %d want 2 (non-terminal only)", len(doc.OpenPRs))
	}
}

// --- codex Slice-1 P1: a worker with a PR open (state.json pr_url) but
// tasks.md still in-progress is promoted to in-review in the live walk,
// so the successor takes the shepherd-only path and does NOT re-dispatch
// the Agent against an already-open PR. ---
func TestCollectActiveSubagentsLive_PromotesPROpenToInReview(t *testing.T) {
	pdir := withFleetHomeSynth(t)
	seedCoordState(t, pdir, "myproj", map[string]string{
		"pr-open-1111":   "idA",
		"writing-2222":   "idB",
		"no-status-3333": "idC",
	})
	// pr-open: PR opened in state.json, tasks.md still in-progress.
	seedWorkerState(t, pdir, "myproj", "pr-open-1111", "push", "https://github.com/o/r/pull/7")
	// writing: no PR yet, in-progress.
	seedWorkerState(t, pdir, "myproj", "writing-2222", "tdd-green", "")
	// no-status: PR in state.json but NO tasks.md row at all (empty status).
	seedWorkerState(t, pdir, "myproj", "no-status-3333", "push", "https://github.com/o/r/pull/8")
	seedTasksMD(t, pdir, "myproj", map[string]tasks.Status{
		"pr-open-1111": tasks.StatusInProgress,
		"writing-2222": tasks.StatusInProgress,
		// no-status-3333 intentionally absent from tasks.md.
	})

	subs, _ := CollectActiveSubagentsLive("myproj")
	byTask := map[string]ActiveSubagent{}
	for _, s := range subs {
		byTask[s.TaskID] = s
	}
	if got := byTask["pr-open-1111"].Status; got != "in-review" {
		t.Errorf("pr-open status: got %q want in-review (PR open → shepherd-only)", got)
	}
	if got := byTask["no-status-3333"].Status; got != "in-review" {
		t.Errorf("no-status (PR open, empty tasks status): got %q want in-review", got)
	}
	if got := byTask["writing-2222"].Status; got != "in-progress" {
		t.Errorf("writing (no PR) status: got %q want in-progress (still re-dispatch)", got)
	}
}

// --- codex Slice-1 P1 (inverse direction): tasks.md has the pr_url but
// state.json does NOT yet — promotion must still fire. pr_url is sourced
// from EITHER store. ---
func TestCollectActiveSubagentsLive_PromotesFromTasksMDPRURL(t *testing.T) {
	pdir := withFleetHomeSynth(t)
	seedCoordState(t, pdir, "myproj", map[string]string{"tonly-1111": "idA"})
	// state.json: in-flight, NO pr_url.
	seedWorkerState(t, pdir, "myproj", "tonly-1111", "tdd-green", "")
	// tasks.md: in-progress but carries a pr_url (coord stamped tasks.md
	// before state.json caught up).
	seedTasksMDWithPRAndStatus(t, pdir, "myproj", map[string][2]string{
		"tonly-1111": {"in-progress", "https://github.com/o/r/pull/99"},
	})

	subs, _ := CollectActiveSubagentsLive("myproj")
	if len(subs) != 1 {
		t.Fatalf("subs: got %d want 1", len(subs))
	}
	if subs[0].Status != "in-review" {
		t.Errorf("status: got %q want in-review (tasks.md pr_url drives promotion)", subs[0].Status)
	}
	if subs[0].PRURL != "https://github.com/o/r/pull/99" {
		t.Errorf("pr_url: got %q want pull/99 (sourced from tasks.md)", subs[0].PRURL)
	}
}

// --- repoDir is forwarded to gh so `gh pr list` binds to the handed-off
// coord's checkout regardless of the operator's CWD (codex Slice-1 P1). ---
func TestCollectOpenPRs_PassesRepoDirToGh(t *testing.T) {
	var gotDir string
	prev := ghRunner
	ghRunner = func(dir string, args ...string) ([]byte, error) {
		gotDir = dir
		return []byte("[]"), nil
	}
	t.Cleanup(func() { ghRunner = prev })

	_, _ = CollectOpenPRs("/some/coord/repo", nil)
	if gotDir != "/some/coord/repo" {
		t.Errorf("gh working dir: got %q want /some/coord/repo", gotDir)
	}
}

// --- codex Slice-1 P2: gh fails AND a checkpoint snapshot HAS PRs, but
// tasks.md carries fresher per-task pr_url → tasks.md wins over the stale
// checkpoint snapshot (a PR opened since the last checkpoint tick is not
// dropped). ---
func TestEnrichManualDoc_TasksMDWinsOverCheckpointPRsOnGhFail(t *testing.T) {
	pdir := withFleetHomeSynth(t)
	now := time.Now().UTC()
	seedCoordState(t, pdir, "myproj", map[string]string{"fresh-1111": "idA"})
	seedWorkerState(t, pdir, "myproj", "fresh-1111", "push", "")
	// Checkpoint snapshot: ONE old PR.
	oldRow := "- #10 old — worker/old-0000 — https://github.com/o/r/pull/10"
	seedCheckpoint(t, pdir, "myproj", now, nil, []string{oldRow}, nil)
	// tasks.md: a DIFFERENT, fresher PR opened since the checkpoint.
	seedTasksMDWithPR(t, pdir, "myproj", map[string]string{
		"fresh-1111": "https://github.com/o/r/pull/55",
	})
	lastHandoff := writeFakeHandoffDoc(t, pdir, "deadbeef", now.Add(-time.Hour))

	fakeGH(t, nil, errors.New("gh down"))

	doc := NewManualStub("deadbeef", "coord-myproj", "myproj", 1, &lastHandoff, now)
	EnrichManualDoc(doc, "myproj", "deadbeef", "", &lastHandoff, nil)

	if len(doc.OpenPRs) != 1 {
		t.Fatalf("OpenPRs: got %#v want 1 (tasks.md fresher than checkpoint)", doc.OpenPRs)
	}
	if doc.OpenPRs[0].URL != "https://github.com/o/r/pull/55" {
		t.Errorf("PR URL: got %q want pull/55 (tasks.md wins over stale checkpoint #10)", doc.OpenPRs[0].URL)
	}
}

// --- Case 4: 3 open worker PRs → all render `- #N title — head — url`. ---
func TestEnrichManualDoc_ThreeOpenPRsRender(t *testing.T) {
	pdir := withFleetHomeSynth(t)
	now := time.Now().UTC()
	seedCoordState(t, pdir, "myproj", map[string]string{"a-1": "id1"})
	seedWorkerState(t, pdir, "myproj", "a-1", "push", "")

	fakeGH(t, ghJSON(t, []ghOpenPR{
		{Number: 1, Title: "one", HeadRefName: "worker/a-1", URL: "https://x/1"},
		{Number: 2, Title: "two", HeadRefName: "worker/b-2", URL: "https://x/2"},
		{Number: 3, Title: "three", HeadRefName: "worker/c-3", URL: "https://x/3"},
	}), nil)

	doc := NewManualStub("deadbeef", "coord-myproj", "myproj", 1, nil, now)
	EnrichManualDoc(doc, "myproj", "deadbeef", "", nil, nil)

	body := string(Render(doc))
	for _, want := range []string{
		"- #1 one — worker/a-1 — https://x/1",
		"- #2 two — worker/b-2 — https://x/2",
		"- #3 three — worker/c-3 — https://x/3",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered doc missing %q; got:\n%s", want, body)
		}
	}
}

// --- Case 5: a slug whose workers/<slug>/state.json is MISSING → row
// STILL emitted with phase="" (live-vs-synth-skip divergence). ---
func TestEnrichManualDoc_EmitsOnMissingWorkerState(t *testing.T) {
	pdir := withFleetHomeSynth(t)
	now := time.Now().UTC()
	seedCoordState(t, pdir, "myproj", map[string]string{
		"has-state-1111": "id-has",
		"no-state-2222":  "id-none",
	})
	// Only seed ONE worker dir; the other has no state.json.
	seedWorkerState(t, pdir, "myproj", "has-state-1111", "push", "")

	fakeGH(t, []byte("[]"), nil) // gh returns no PRs

	doc := NewManualStub("deadbeef", "coord-myproj", "myproj", 1, nil, now)
	EnrichManualDoc(doc, "myproj", "deadbeef", "", nil, nil)

	if len(doc.ActiveSubagents) != 2 {
		t.Fatalf("ActiveSubagents: got %d want 2 (emit-on-missing); rows=%#v", len(doc.ActiveSubagents), doc.ActiveSubagents)
	}
	byTask := map[string]ActiveSubagent{}
	for _, s := range doc.ActiveSubagents {
		byTask[s.TaskID] = s
	}
	missing, ok := byTask["no-state-2222"]
	if !ok {
		t.Fatalf("no-state-2222 was SKIPPED — should be EMITTED (live semantics)")
	}
	if missing.LastPhase != "" {
		t.Errorf("missing-state row phase: got %q want \"\"", missing.LastPhase)
	}
	if missing.AgentID != "id-none" {
		t.Errorf("missing-state row agent_id: got %q want id-none", missing.AgentID)
	}
	if missing.Branch != "worker/no-state-2222" {
		t.Errorf("missing-state row branch: got %q", missing.Branch)
	}
}

// Direct contrast: synth's recovery walk SKIPS the same missing slug.
// Pins the deliberate divergence the design calls load-bearing.
func TestCollectActiveSubagentsLive_DivergesFromSynthSkip(t *testing.T) {
	pdir := withFleetHomeSynth(t)
	seedCoordState(t, pdir, "myproj", map[string]string{
		"present-1111": "idA",
		"missing-2222": "idB",
	})
	seedWorkerState(t, pdir, "myproj", "present-1111", "push", "")

	// Live walk emits both.
	live, _ := CollectActiveSubagentsLive("myproj")
	if len(live) != 2 {
		t.Fatalf("live walk: got %d want 2 (emit-on-missing)", len(live))
	}

	// Synth recovery walk skips the missing one.
	doc, err := SynthesizeRecovery("idA", "myproj", time.Now().UTC())
	if err != nil {
		t.Fatalf("SynthesizeRecovery: %v", err)
	}
	if len(doc.ActiveSubagents) != 1 || doc.ActiveSubagents[0].TaskID != "present-1111" {
		t.Fatalf("synth walk: got %#v want only present-1111 (skip-on-missing)", doc.ActiveSubagents)
	}
}

// --- Case 6: checkpoint coord_id != handing-off coord → checkpoint
// REJECTED (generation guard); falls back to live walk. ---
func TestEnrichManualDoc_RejectsForeignGenerationCheckpoint(t *testing.T) {
	pdir := withFleetHomeSynth(t)
	now := time.Now().UTC()
	// seedCheckpoint hardcodes coord_id="deadbeef".
	foreignRow := `- task="from-prev-gen" branch="worker/from-prev-gen" phase="" status="" pr_url="" agent_id="x" subagent_id=""`
	seedCheckpoint(t, pdir, "myproj", now, []string{foreignRow}, nil, nil)
	lastHandoff := writeFakeHandoffDoc(t, pdir, "newcoord1", now.Add(-time.Hour))

	// Live state belongs to the current (different) coord generation.
	seedCoordState(t, pdir, "myproj", map[string]string{"live-9999": "newcoord1"})
	seedWorkerState(t, pdir, "myproj", "live-9999", "tdd-green", "")

	fakeGH(t, []byte("[]"), nil)

	// Handing-off coord is "newcoord1", checkpoint's coord_id is "deadbeef".
	doc := NewManualStub("newcoord1", "coord-myproj", "myproj", 1, &lastHandoff, now)
	EnrichManualDoc(doc, "myproj", "newcoord1", "", &lastHandoff, nil)

	if len(doc.ActiveSubagents) != 1 || doc.ActiveSubagents[0].TaskID != "live-9999" {
		t.Fatalf("ActiveSubagents: got %#v want live-9999 (checkpoint rejected by gen guard)", doc.ActiveSubagents)
	}
}

// --- Case 7: no checkpoint file at all (pre-first-tick) → machine
// sections from live walk/gh; narrative still placeholder. ---
func TestEnrichManualDoc_NoCheckpointNarrativePlaceholder(t *testing.T) {
	pdir := withFleetHomeSynth(t)
	now := time.Now().UTC()
	seedCoordState(t, pdir, "myproj", map[string]string{"w-1": "id1"})
	seedWorkerState(t, pdir, "myproj", "w-1", "push", "")
	fakeGH(t, []byte("[]"), nil)

	doc := NewManualStub("deadbeef", "coord-myproj", "myproj", 1, nil, now)
	EnrichManualDoc(doc, "myproj", "deadbeef", "", nil, nil)

	if len(doc.ActiveSubagents) != 1 {
		t.Errorf("ActiveSubagents: got %d want 1 (live walk)", len(doc.ActiveSubagents))
	}
	// Narrative sections untouched in Slice 1.
	if doc.Completed != Placeholder {
		t.Errorf("Completed: got %q want placeholder (Slice 1 leaves narrative alone)", doc.Completed)
	}
	if doc.KeyDecisions != Placeholder {
		t.Errorf("KeyDecisions: got %q want placeholder", doc.KeyDecisions)
	}
	if doc.NextSteps != Placeholder {
		t.Errorf("NextSteps: got %q want placeholder (no checkpoint decisions to lift)", doc.NextSteps)
	}
}

// --- Case 8: enrichment panics mid-build → handoff completes with
// placeholder sections (best-effort recover). ---
func TestEnrichManualDoc_RecoversFromPanic(t *testing.T) {
	pdir := withFleetHomeSynth(t)
	now := time.Now().UTC()
	seedCoordState(t, pdir, "myproj", map[string]string{"w-1": "id1"})

	// Force a panic from inside the gh runner.
	prev := ghRunner
	ghRunner = func(dir string, args ...string) ([]byte, error) { panic("boom from gh runner") }
	t.Cleanup(func() { ghRunner = prev })

	doc := NewManualStub("deadbeef", "coord-myproj", "myproj", 1, nil, now)

	var logged []string
	// Must NOT panic out of EnrichManualDoc.
	EnrichManualDoc(doc, "myproj", "deadbeef", "", nil, func(m string) { logged = append(logged, m) })

	// Open PRs left as placeholder; the doc still renders.
	if len(doc.OpenPRs) != 0 {
		t.Errorf("OpenPRs: got %#v want empty after panic recover", doc.OpenPRs)
	}
	if body := string(Render(doc)); !strings.Contains(body, OpenPRsNonePlaceholder) {
		t.Errorf("doc should still render Open PRs placeholder after panic")
	}
	foundRecover := false
	for _, m := range logged {
		if strings.Contains(m, "recovered from panic") {
			foundRecover = true
		}
	}
	if !foundRecover {
		t.Errorf("expected a 'recovered from panic' diagnostic; got %#v", logged)
	}
}

// --- CollectOpenPRs unit edges: empty gh output and bad JSON both
// fall back (ok=false) without erroring. ---
func TestCollectOpenPRs_EmptyAndBadJSON(t *testing.T) {
	t.Run("empty output", func(t *testing.T) {
		fakeGH(t, []byte(""), nil)
		prs, ok := CollectOpenPRs("", nil)
		if ok || prs != nil {
			t.Errorf("empty gh output: got (%#v,%v) want (nil,false)", prs, ok)
		}
	})
	t.Run("bad json", func(t *testing.T) {
		fakeGH(t, []byte("not json"), nil)
		prs, ok := CollectOpenPRs("", nil)
		if ok || prs != nil {
			t.Errorf("bad json: got (%#v,%v) want (nil,false)", prs, ok)
		}
	})
	t.Run("valid", func(t *testing.T) {
		fakeGH(t, ghJSON(t, []ghOpenPR{{Number: 5, Title: "t", HeadRefName: "worker/x", URL: "u"}}), nil)
		prs, ok := CollectOpenPRs("", nil)
		if !ok || len(prs) != 1 || prs[0].Number != 5 {
			t.Errorf("valid: got (%#v,%v) want one #5", prs, ok)
		}
	})
}

// --- EnrichManualDoc on a nil doc is a no-op (defensive). ---
func TestEnrichManualDoc_NilDoc(t *testing.T) {
	withFleetHomeSynth(t)
	fakeGH(t, []byte("[]"), nil)
	EnrichManualDoc(nil, "myproj", "deadbeef", "", nil, nil) // must not panic
}

// --- Case R: refactor-parity. SynthesizeRecoveryWithLastHandoff output
// after the applyCheckpointToDoc extraction is byte-identical to the
// pre-refactor inline behavior. We assert against an explicitly
// reconstructed golden so a future change to applyCheckpointToDoc (e.g.
// Slice 2 narrative) trips this test rather than silently drifting
// recovery output. ---
func TestSynthesizeRecovery_RefactorParity_CheckpointLift(t *testing.T) {
	pdir := withFleetHomeSynth(t)
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)

	activeRow := `- task="fix-new-1234" branch="worker/fix-new-1234" phase="push" status="in-review" pr_url="https://github.com/o/r/pull/77" agent_id="cafef00d" subagent_id=""`
	prRow := "- #77 fix: new — worker/fix-new-1234 — https://github.com/o/r/pull/77"
	seedCheckpoint(t, pdir, "myproj", now,
		[]string{activeRow}, []string{prRow},
		[]string{"- dispatched fix-new-1234", "- promoted bar to ready"},
	)
	lastHandoff := writeFakeHandoffDoc(t, pdir, "deadbeef", now.Add(-time.Hour))

	doc, err := SynthesizeRecoveryWithLastHandoff("deadbeef", "myproj", lastHandoff, now.Add(time.Second))
	if err != nil {
		t.Fatalf("SynthesizeRecoveryWithLastHandoff: %v", err)
	}

	// Pre-refactor inline behavior: ActiveSubagents + OpenPRs lifted
	// verbatim; recent decisions rendered into NextSteps as a bullet
	// list with the exact header + trailing-newline-trimmed shape.
	wantNextSteps := "Recent coord decisions (from checkpoint):\n" +
		"- dispatched fix-new-1234\n" +
		"- promoted bar to ready"
	if doc.NextSteps != wantNextSteps {
		t.Errorf("NextSteps drift:\n got %q\nwant %q", doc.NextSteps, wantNextSteps)
	}
	if len(doc.ActiveSubagents) != 1 || doc.ActiveSubagents[0].TaskID != "fix-new-1234" {
		t.Errorf("ActiveSubagents drift: %#v", doc.ActiveSubagents)
	}
	if len(doc.OpenPRs) != 1 || doc.OpenPRs[0].Number != 77 {
		t.Errorf("OpenPRs drift: %#v", doc.OpenPRs)
	}
	// Narrative sections stay placeholder (synth never sets them from cp).
	if doc.Completed != Placeholder || doc.KeyDecisions != Placeholder {
		t.Errorf("narrative drift: Completed=%q KeyDecisions=%q", doc.Completed, doc.KeyDecisions)
	}
}

// applyRecentDecisions formatting parity: empty buffer passes the
// fallback through unchanged; non-empty renders the exact bullet shape.
func TestApplyRecentDecisions_Format(t *testing.T) {
	if got := applyRecentDecisions(Placeholder, nil); got != Placeholder {
		t.Errorf("empty decisions: got %q want fallback passthrough", got)
	}
	got := applyRecentDecisions(Placeholder, []string{"a", "b"})
	want := "Recent coord decisions (from checkpoint):\n- a\n- b"
	if got != want {
		t.Errorf("format:\n got %q\nwant %q", got, want)
	}
}
