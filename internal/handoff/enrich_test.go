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

// --- Case 1: fresh checkpoint present → Active Subagents from
// checkpoint; Open PRs from gh. ---
func TestEnrichManualDoc_FreshCheckpoint(t *testing.T) {
	pdir := withFleetHomeSynth(t)
	now := time.Now().UTC()

	// Stale on-disk state that the checkpoint should override.
	seedCoordState(t, pdir, "myproj", map[string]string{"fix-old-0000": "obsolete"})
	seedWorkerState(t, pdir, "myproj", "fix-old-0000", "tdd-green", "")

	activeRow := `- task="fix-new-1234" branch="worker/fix-new-1234" phase="push" status="in-review" pr_url="https://github.com/o/r/pull/77" agent_id="cafef00d" subagent_id=""`
	seedCheckpoint(t, pdir, "myproj", now, []string{activeRow}, nil, nil)
	lastHandoff := writeFakeHandoffDoc(t, pdir, "deadbeef", now.Add(-time.Hour))

	fakeGH(t, ghJSON(t, []ghOpenPR{
		{Number: 77, Title: "fix: new", HeadRefName: "worker/fix-new-1234", URL: "https://github.com/o/r/pull/77"},
	}), nil)

	doc := NewManualStub("deadbeef", "coord-myproj", "myproj", 1, &lastHandoff, now)
	EnrichManualDoc(doc, "myproj", "deadbeef", "", &lastHandoff, nil)

	if len(doc.ActiveSubagents) != 1 || doc.ActiveSubagents[0].TaskID != "fix-new-1234" {
		t.Fatalf("ActiveSubagents: got %#v want one fix-new-1234 (checkpoint wins)", doc.ActiveSubagents)
	}
	if len(doc.OpenPRs) != 1 || doc.OpenPRs[0].Number != 77 {
		t.Fatalf("OpenPRs: got %#v want one #77 (gh)", doc.OpenPRs)
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
	live := CollectActiveSubagentsLive("myproj")
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
