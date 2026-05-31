// Tests for synth.go — synthesizing a recovery-handoff doc from on-disk
// state. Exercised by `fleet dispatch <existing-coord-name>` against a
// coord whose tmux session is gone and whose pid is dead.
package handoff

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/tasks"
)

// withFleetHomeSynth installs a tmp ~/.fleet root for the test and
// returns the projects/ subdir already created. Keeps the test package
// independent of cross-package helpers (the tui's withFleetHome lives
// in a different package and would force a circular dep).
func withFleetHomeSynth(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("FLEET_HOME", root)
	pdir := filepath.Join(root, "projects")
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatalf("mkdir projects: %v", err)
	}
	return pdir
}

// seedCoordState writes ~/.fleet/projects/<p>/coord-state.json with the
// given worker_agent_ids body. Test fixture for the synth path.
func seedCoordState(t *testing.T, projectsRoot, project string, agentIDs map[string]string) {
	t.Helper()
	dir := filepath.Join(projectsRoot, project)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	body := map[string]any{
		"worker_agent_ids": agentIDs,
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal coord-state: %v", err)
	}
	path := filepath.Join(dir, "coord-state.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write coord-state.json: %v", err)
	}
}

// seedWorkerState writes a workers/<slug>/state.json with the given
// phase + pr_url so SynthesizeRecovery's walk picks it up.
func seedWorkerState(t *testing.T, projectsRoot, project, slug, phase, prURL string) {
	t.Helper()
	dir := filepath.Join(projectsRoot, project, "workers", slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	body := map[string]any{
		"slug":    slug,
		"project": project,
		"phase":   phase,
		"pid":     0,
		"pr_url":  prURL,
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal worker state: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), data, 0o644); err != nil {
		t.Fatalf("write state.json: %v", err)
	}
}

// TestSynthesizeRecovery_HasRecoveryType pins the frontmatter handoff_type
// to "recovery-synth" so the successor can distinguish from a clean handoff.
func TestSynthesizeRecovery_HasRecoveryType(t *testing.T) {
	withFleetHomeSynth(t)

	doc, err := SynthesizeRecovery("a1b2c3d4", "myproj", time.Now().UTC())
	if err != nil {
		t.Fatalf("SynthesizeRecovery: %v", err)
	}
	if doc.Type != TypeRecoverySynth {
		t.Errorf("handoff_type: got %q want %q", doc.Type, TypeRecoverySynth)
	}
	if doc.AgentID != "a1b2c3d4" {
		t.Errorf("agent_id: got %q want a1b2c3d4", doc.AgentID)
	}
	if doc.Project != "myproj" {
		t.Errorf("project: got %q want myproj", doc.Project)
	}
}

// TestSynthesizeRecovery_PopulatesActiveSubagents pins that the synth
// walks coord-state.json:worker_agent_ids and workers/<slug>/state.json
// to fill ActiveSubagents — the load-bearing data the successor coord
// uses to re-dispatch.
func TestSynthesizeRecovery_PopulatesActiveSubagents(t *testing.T) {
	pdir := withFleetHomeSynth(t)
	seedCoordState(t, pdir, "myproj", map[string]string{
		"fix-foo-1234": "deadbeef",
		"fix-bar-5678": "cafef00d",
	})
	seedWorkerState(t, pdir, "myproj", "fix-foo-1234", "tdd-green", "")
	seedWorkerState(t, pdir, "myproj", "fix-bar-5678", "push", "https://github.com/owner/repo/pull/42")

	doc, err := SynthesizeRecovery("a1b2c3d4", "myproj", time.Now().UTC())
	if err != nil {
		t.Fatalf("SynthesizeRecovery: %v", err)
	}
	if got := len(doc.ActiveSubagents); got != 2 {
		t.Fatalf("ActiveSubagents: got %d want 2 (%#v)", got, doc.ActiveSubagents)
	}

	byTask := map[string]ActiveSubagent{}
	for _, s := range doc.ActiveSubagents {
		byTask[s.TaskID] = s
	}
	foo, ok := byTask["fix-foo-1234"]
	if !ok {
		t.Fatalf("fix-foo-1234 missing from ActiveSubagents")
	}
	if foo.LastPhase != "tdd-green" {
		t.Errorf("fix-foo phase: got %q want tdd-green", foo.LastPhase)
	}
	if foo.AgentID != "deadbeef" {
		t.Errorf("fix-foo agent_id: got %q want deadbeef", foo.AgentID)
	}
	bar, ok := byTask["fix-bar-5678"]
	if !ok {
		t.Fatalf("fix-bar-5678 missing from ActiveSubagents")
	}
	if bar.LastPhase != "push" {
		t.Errorf("fix-bar phase: got %q want push", bar.LastPhase)
	}
	if bar.PRURL != "https://github.com/owner/repo/pull/42" {
		t.Errorf("fix-bar pr_url: got %q want https://...", bar.PRURL)
	}
}

// TestSynthesizeRecovery_RoundTripViaRender pins that the synth doc
// renders + parses cleanly — the successor's parser must see the
// same ActiveSubagents the synthesizer built. End-to-end contract for
// the dispatch-resume path.
func TestSynthesizeRecovery_RoundTripViaRender(t *testing.T) {
	pdir := withFleetHomeSynth(t)
	seedCoordState(t, pdir, "myproj", map[string]string{
		"fix-foo-1234": "deadbeef",
	})
	seedWorkerState(t, pdir, "myproj", "fix-foo-1234", "tdd-green", "")

	doc, err := SynthesizeRecovery("a1b2c3d4", "myproj", time.Now().UTC())
	if err != nil {
		t.Fatalf("SynthesizeRecovery: %v", err)
	}
	body := Render(doc)
	parsed, warnings, perr := ParseActiveSubagents(body)
	if perr != nil {
		t.Fatalf("ParseActiveSubagents: %v", perr)
	}
	if len(warnings) > 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}
	if len(parsed) != 1 {
		t.Fatalf("parsed ActiveSubagents: got %d want 1", len(parsed))
	}
	if parsed[0].TaskID != "fix-foo-1234" {
		t.Errorf("task: got %q want fix-foo-1234", parsed[0].TaskID)
	}
	if parsed[0].LastPhase != "tdd-green" {
		t.Errorf("phase: got %q want tdd-green", parsed[0].LastPhase)
	}
}

// TestSynthesizeRecovery_EmptyProjectIsCleanDoc covers the edge case
// where a coord crashed but never spawned workers. The doc renders
// cleanly with zero ActiveSubagents (the placeholder body) — successor
// finds no work to re-dispatch and proceeds with a fresh supervisor pass.
func TestSynthesizeRecovery_EmptyProjectIsCleanDoc(t *testing.T) {
	withFleetHomeSynth(t)

	doc, err := SynthesizeRecovery("a1b2c3d4", "myproj", time.Now().UTC())
	if err != nil {
		t.Fatalf("SynthesizeRecovery: %v", err)
	}
	if len(doc.ActiveSubagents) != 0 {
		t.Errorf("ActiveSubagents on empty project: got %d want 0", len(doc.ActiveSubagents))
	}
	body := string(Render(doc))
	if !strings.Contains(body, ActiveSubagentsNonePlaceholder) {
		t.Errorf("body missing %q placeholder:\n%s", ActiveSubagentsNonePlaceholder, body)
	}
}

// TestSynthesizeRecovery_SkipsMissingWorkerState covers the legitimate
// case where coord-state.json references a slug but the worker dir was
// already archived (e.g. coord crashed mid-archive). Synth must NOT
// fail; it skips the missing entry so the successor doesn't try to
// re-dispatch a finished worker.
func TestSynthesizeRecovery_SkipsMissingWorkerState(t *testing.T) {
	pdir := withFleetHomeSynth(t)
	seedCoordState(t, pdir, "myproj", map[string]string{
		"fix-foo-1234": "deadbeef",
		"fix-bar-5678": "cafef00d",
	})
	// Only seed one worker dir; fix-bar's state.json is missing.
	seedWorkerState(t, pdir, "myproj", "fix-foo-1234", "tdd-green", "")

	doc, err := SynthesizeRecovery("a1b2c3d4", "myproj", time.Now().UTC())
	if err != nil {
		t.Fatalf("SynthesizeRecovery: %v", err)
	}
	if got := len(doc.ActiveSubagents); got != 1 {
		t.Fatalf("ActiveSubagents: got %d want 1 (only the live worker)", got)
	}
	if doc.ActiveSubagents[0].TaskID != "fix-foo-1234" {
		t.Errorf("task: got %q want fix-foo-1234", doc.ActiveSubagents[0].TaskID)
	}
}

// seedTasksMD writes a minimal tasks.md with one task per (slug, status)
// entry. Used by status-overlay tests to seed the per-slug status enum
// the synth pass must pick up.
func seedTasksMD(t *testing.T, projectsRoot, project string, statusBySlug map[string]tasks.Status) {
	t.Helper()
	dir := filepath.Join(projectsRoot, project)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	f := &tasks.File{Schema: 1}
	// Deterministic iteration order: sort slugs.
	slugs := make([]string, 0, len(statusBySlug))
	for s := range statusBySlug {
		slugs = append(slugs, s)
	}
	// Stable insertion order (sort).
	for i := 0; i < len(slugs); i++ {
		for j := i + 1; j < len(slugs); j++ {
			if slugs[j] < slugs[i] {
				slugs[i], slugs[j] = slugs[j], slugs[i]
			}
		}
	}
	for _, s := range slugs {
		f.Tasks = append(f.Tasks, &tasks.Task{
			Slug:     s,
			Status:   statusBySlug[s],
			Priority: tasks.Priority("P1"),
			Created:  now,
			Updated:  now,
			Spec:     "synth-test task " + s,
		})
	}
	if err := tasks.Write(filepath.Join(dir, "tasks.md"), f); err != nil {
		t.Fatalf("write tasks.md: %v", err)
	}
}

// TestSynthesizeRecovery_OverlaysStatusFromTasksMD pins codex review
// iter-3 P2: the synth doc must carry the per-slug Status from tasks.md
// so handoff_resume.py's re-dispatch logic can short-circuit
// in-review / blocked / done workers. Without this overlay every row
// would have Status="", treated as "always re-dispatch" by the
// successor — wasting work on already-finished tasks.
func TestSynthesizeRecovery_OverlaysStatusFromTasksMD(t *testing.T) {
	pdir := withFleetHomeSynth(t)
	seedCoordState(t, pdir, "myproj", map[string]string{
		"fix-foo-1234": "deadbeef",
		"fix-bar-5678": "cafef00d",
		"fix-baz-9abc": "feedface",
	})
	seedWorkerState(t, pdir, "myproj", "fix-foo-1234", "tdd-green", "")
	seedWorkerState(t, pdir, "myproj", "fix-bar-5678", "push", "https://github.com/owner/repo/pull/42")
	seedWorkerState(t, pdir, "myproj", "fix-baz-9abc", "done", "https://github.com/owner/repo/pull/43")
	// tasks.md says: foo is in-progress (re-dispatch), bar is in-review
	// (shepherd only — no re-dispatch), baz is done (drop).
	seedTasksMD(t, pdir, "myproj", map[string]tasks.Status{
		"fix-foo-1234": tasks.Status("in-progress"),
		"fix-bar-5678": tasks.Status("in-review"),
		"fix-baz-9abc": tasks.Status("done"),
	})

	doc, err := SynthesizeRecovery("a1b2c3d4", "myproj", time.Now().UTC())
	if err != nil {
		t.Fatalf("SynthesizeRecovery: %v", err)
	}
	byTask := map[string]ActiveSubagent{}
	for _, s := range doc.ActiveSubagents {
		byTask[s.TaskID] = s
	}
	if foo := byTask["fix-foo-1234"]; foo.Status != "in-progress" {
		t.Errorf("fix-foo status: got %q want in-progress", foo.Status)
	}
	if bar := byTask["fix-bar-5678"]; bar.Status != "in-review" {
		t.Errorf("fix-bar status: got %q want in-review", bar.Status)
	}
	if baz := byTask["fix-baz-9abc"]; baz.Status != "done" {
		t.Errorf("fix-baz status: got %q want done", baz.Status)
	}
}

// TestSynthesizeRecovery_MissingTasksMDLeavesStatusEmpty covers the
// fresh-project edge case: coord crashed before tasks.md was ever
// written (or it was hand-deleted). Synth must NOT fail — status is
// just empty, handoff_resume.py falls back to "re-dispatch all" which
// is the safe default. We're proving the recovery path stays resilient
// when tasks.md is absent.
func TestSynthesizeRecovery_MissingTasksMDLeavesStatusEmpty(t *testing.T) {
	pdir := withFleetHomeSynth(t)
	seedCoordState(t, pdir, "myproj", map[string]string{
		"fix-foo-1234": "deadbeef",
	})
	seedWorkerState(t, pdir, "myproj", "fix-foo-1234", "tdd-green", "")
	// No tasks.md.

	doc, err := SynthesizeRecovery("a1b2c3d4", "myproj", time.Now().UTC())
	if err != nil {
		t.Fatalf("SynthesizeRecovery without tasks.md: %v", err)
	}
	if len(doc.ActiveSubagents) != 1 {
		t.Fatalf("ActiveSubagents: got %d want 1", len(doc.ActiveSubagents))
	}
	if doc.ActiveSubagents[0].Status != "" {
		t.Errorf("status without tasks.md: got %q want \"\" (legacy fallback)", doc.ActiveSubagents[0].Status)
	}
}

// TestSynthesizeRecovery_SlugMissingFromTasksMDStaysEmpty covers the
// mid-write crash: coord registered the slug in coord-state.json but
// crashed before adding it to tasks.md. Synth must not synthesize a
// status; empty triggers the safe re-dispatch fallback.
func TestSynthesizeRecovery_SlugMissingFromTasksMDStaysEmpty(t *testing.T) {
	pdir := withFleetHomeSynth(t)
	seedCoordState(t, pdir, "myproj", map[string]string{
		"fix-foo-1234": "deadbeef",
		"fix-bar-5678": "cafef00d", // in coord-state but NOT tasks.md
	})
	seedWorkerState(t, pdir, "myproj", "fix-foo-1234", "tdd-green", "")
	seedWorkerState(t, pdir, "myproj", "fix-bar-5678", "tdd-green", "")
	seedTasksMD(t, pdir, "myproj", map[string]tasks.Status{
		"fix-foo-1234": tasks.Status("in-progress"),
		// fix-bar-5678 absent.
	})

	doc, err := SynthesizeRecovery("a1b2c3d4", "myproj", time.Now().UTC())
	if err != nil {
		t.Fatalf("SynthesizeRecovery: %v", err)
	}
	byTask := map[string]ActiveSubagent{}
	for _, s := range doc.ActiveSubagents {
		byTask[s.TaskID] = s
	}
	if byTask["fix-foo-1234"].Status != "in-progress" {
		t.Errorf("fix-foo status: got %q want in-progress", byTask["fix-foo-1234"].Status)
	}
	if byTask["fix-bar-5678"].Status != "" {
		t.Errorf("fix-bar status (slug missing from tasks.md): got %q want \"\"", byTask["fix-bar-5678"].Status)
	}
}

// seedCheckpoint writes ~/.fleet/projects/<p>/coord-checkpoint.md with
// the rolling-checkpoint shape: frontmatter (schema/v1, coord_id,
// project, updated_at, tick_count) + the four body sections. Test
// fixture for the synth checkpoint-preference path.
func seedCheckpoint(
	t *testing.T,
	projectsRoot, project string,
	updatedAt time.Time,
	activeRows []string,
	openPRRows []string,
	decisionRows []string,
) {
	t.Helper()
	dir := filepath.Join(projectsRoot, project)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("schema: v1\n")
	b.WriteString("coord_id: \"deadbeef\"\n")
	b.WriteString("project: \"" + project + "\"\n")
	b.WriteString("updated_at: \"" + updatedAt.UTC().Format(time.RFC3339) + "\"\n")
	b.WriteString("tick_count: 5\n")
	b.WriteString("---\n\n")
	b.WriteString("### Active Subagents\n")
	if len(activeRows) == 0 {
		b.WriteString("_(none)_\n\n")
	} else {
		for _, r := range activeRows {
			b.WriteString(r + "\n")
		}
		b.WriteString("\n")
	}
	b.WriteString("### Open PRs\n")
	if len(openPRRows) == 0 {
		b.WriteString("_(no open PRs)_\n\n")
	} else {
		for _, r := range openPRRows {
			b.WriteString(r + "\n")
		}
		b.WriteString("\n")
	}
	b.WriteString("### Recent decisions\n")
	if len(decisionRows) == 0 {
		b.WriteString("_(no recent decisions)_\n\n")
	} else {
		for _, r := range decisionRows {
			b.WriteString(r + "\n")
		}
		b.WriteString("\n")
	}
	b.WriteString("### Drafted but unfiled tasks\n")
	b.WriteString("_(empty — populated in Phase 2)_\n")
	path := filepath.Join(dir, "coord-checkpoint.md")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write coord-checkpoint.md: %v", err)
	}
}

// writeFakeHandoffDoc emits a minimal handoff doc with a controlled
// timestamp encoded in the filename. synth.go's freshness comparison
// reads the filename stamp, so only the path layout matters here.
func writeFakeHandoffDoc(t *testing.T, projectsRoot, agentID string, ts time.Time) string {
	t.Helper()
	root := filepath.Dir(projectsRoot) // strip the /projects suffix
	hdir := filepath.Join(root, "handoffs")
	if err := os.MkdirAll(hdir, 0o755); err != nil {
		t.Fatalf("mkdir handoffs: %v", err)
	}
	stamp := ts.UTC().Format("20060102-150405")
	path := filepath.Join(hdir, agentID+"-"+stamp+"-abcd.md")
	body := "---\n" +
		"agent_id: \"" + agentID + "\"\n" +
		"task_id: \"coord-myproj\"\n" +
		"project: \"myproj\"\n" +
		"timestamp: \"" + ts.UTC().Format(time.RFC3339) + "\"\n" +
		"handoff_type: \"manual\"\n" +
		"---\n\n## Completed\n_(stub)_\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write fake handoff: %v", err)
	}
	return path
}

// TestSynthesizeRecovery_PrefersCheckpointWhenNewer pins the
// rolling-checkpoint preference: when coord-checkpoint.md exists with a
// fresher updated_at than the last handoff doc, the synth pulls its
// Active Subagents + Open PRs from the checkpoint verbatim rather than
// re-walking worker state.json. The recovery window between handoffs is
// thereby bounded to FLEET_COORD_CHECKPOINT_EVERY ticks.
func TestSynthesizeRecovery_PrefersCheckpointWhenNewer(t *testing.T) {
	pdir := withFleetHomeSynth(t)
	now := time.Now().UTC()

	// On-disk state would synthesize one (obsolete) row.
	seedCoordState(t, pdir, "myproj", map[string]string{
		"fix-old-0000": "obsolete",
	})
	seedWorkerState(t, pdir, "myproj", "fix-old-0000", "tdd-green", "")

	// Checkpoint is newer + says the lane carries different work.
	activeRow := `- task="fix-new-1234" branch="worker/fix-new-1234" phase="push" status="in-review" pr_url="https://github.com/owner/repo/pull/77" agent_id="cafef00d" subagent_id=""`
	prRow := "- #77 fix: new — worker/fix-new-1234 — https://github.com/owner/repo/pull/77"
	seedCheckpoint(t, pdir, "myproj", now,
		[]string{activeRow},
		[]string{prRow},
		[]string{"- dispatched fix-new-1234", "- promoted bar to ready"},
	)
	// Last handoff doc timestamp is BEFORE the checkpoint.
	lastHandoff := writeFakeHandoffDoc(t, pdir, "deadbeef", now.Add(-1*time.Hour))

	doc, err := SynthesizeRecoveryWithLastHandoff("deadbeef", "myproj", lastHandoff, now.Add(1*time.Second))
	if err != nil {
		t.Fatalf("SynthesizeRecoveryWithLastHandoff: %v", err)
	}
	if len(doc.ActiveSubagents) != 1 {
		t.Fatalf("ActiveSubagents: got %d want 1 (the checkpoint row)", len(doc.ActiveSubagents))
	}
	got := doc.ActiveSubagents[0]
	if got.TaskID != "fix-new-1234" {
		t.Errorf("task: got %q want fix-new-1234 (checkpoint wins)", got.TaskID)
	}
	if got.Status != "in-review" {
		t.Errorf("status: got %q want in-review", got.Status)
	}
	if got.PRURL != "https://github.com/owner/repo/pull/77" {
		t.Errorf("pr_url: got %q want pull/77", got.PRURL)
	}
	if len(doc.OpenPRs) != 1 {
		t.Fatalf("OpenPRs: got %d want 1", len(doc.OpenPRs))
	}
	if doc.OpenPRs[0].Number != 77 {
		t.Errorf("PR number: got %d want 77", doc.OpenPRs[0].Number)
	}
}

// TestSynthesizeRecovery_FallsThroughWhenNoCheckpoint pins that the
// legacy state.json-walk path stays the default when no checkpoint file
// exists — protects every existing synth test from a silent regression.
func TestSynthesizeRecovery_FallsThroughWhenNoCheckpoint(t *testing.T) {
	pdir := withFleetHomeSynth(t)
	seedCoordState(t, pdir, "myproj", map[string]string{
		"fix-foo-1234": "deadbeef",
	})
	seedWorkerState(t, pdir, "myproj", "fix-foo-1234", "tdd-green", "")
	// No coord-checkpoint.md.

	doc, err := SynthesizeRecoveryWithLastHandoff("a1b2c3d4", "myproj", "", time.Now().UTC())
	if err != nil {
		t.Fatalf("SynthesizeRecoveryWithLastHandoff: %v", err)
	}
	if len(doc.ActiveSubagents) != 1 {
		t.Fatalf("ActiveSubagents: got %d want 1", len(doc.ActiveSubagents))
	}
	if doc.ActiveSubagents[0].TaskID != "fix-foo-1234" {
		t.Errorf("task: got %q want fix-foo-1234 (state.json fallback)", doc.ActiveSubagents[0].TaskID)
	}
}

// TestSynthesizeRecovery_PrefersHandoffWhenCheckpointStale pins the
// other direction: when the last handoff doc is NEWER than the
// checkpoint, the state.json walk is used because the successor's
// handoff_resume.py already loads state from the fresh handoff doc.
func TestSynthesizeRecovery_PrefersHandoffWhenCheckpointStale(t *testing.T) {
	pdir := withFleetHomeSynth(t)
	now := time.Now().UTC()

	seedCoordState(t, pdir, "myproj", map[string]string{
		"fix-fresh-9999": "deadbeef",
	})
	seedWorkerState(t, pdir, "myproj", "fix-fresh-9999", "tdd-green", "")

	// Checkpoint is OLDER than the last handoff doc.
	staleRow := `- task="fix-stale-0000" branch="worker/fix-stale-0000" phase="push" status="in-progress" pr_url="" agent_id="abc123ab" subagent_id=""`
	seedCheckpoint(t, pdir, "myproj", now.Add(-2*time.Hour),
		[]string{staleRow}, nil, nil,
	)
	lastHandoff := writeFakeHandoffDoc(t, pdir, "deadbeef", now.Add(-30*time.Minute))

	doc, err := SynthesizeRecoveryWithLastHandoff("deadbeef", "myproj", lastHandoff, now)
	if err != nil {
		t.Fatalf("SynthesizeRecoveryWithLastHandoff: %v", err)
	}
	if len(doc.ActiveSubagents) != 1 {
		t.Fatalf("ActiveSubagents: got %d want 1", len(doc.ActiveSubagents))
	}
	if doc.ActiveSubagents[0].TaskID != "fix-fresh-9999" {
		t.Errorf("task: got %q want fix-fresh-9999 (state.json fallback)", doc.ActiveSubagents[0].TaskID)
	}
}

// TestSynthesizeRecovery_NoHandoffPathButCheckpointWins covers the
// fresh-project edge case where the coord crashed before ever writing a
// handoff doc (no LastHandoffPath). The checkpoint must still be
// preferred — its mere presence beats the empty-handoff baseline.
func TestSynthesizeRecovery_NoHandoffPathButCheckpointWins(t *testing.T) {
	pdir := withFleetHomeSynth(t)
	now := time.Now().UTC()

	seedCoordState(t, pdir, "myproj", map[string]string{
		"fix-old-0000": "obsolete",
	})
	seedWorkerState(t, pdir, "myproj", "fix-old-0000", "tdd-green", "")

	activeRow := `- task="fix-new-1234" branch="worker/fix-new-1234" phase="tdd-green" status="in-progress" pr_url="" agent_id="cafef00d" subagent_id=""`
	seedCheckpoint(t, pdir, "myproj", now, []string{activeRow}, nil, nil)

	doc, err := SynthesizeRecoveryWithLastHandoff("deadbeef", "myproj", "", now)
	if err != nil {
		t.Fatalf("SynthesizeRecoveryWithLastHandoff: %v", err)
	}
	if len(doc.ActiveSubagents) != 1 {
		t.Fatalf("ActiveSubagents: got %d want 1", len(doc.ActiveSubagents))
	}
	if doc.ActiveSubagents[0].TaskID != "fix-new-1234" {
		t.Errorf("task: got %q want fix-new-1234 (checkpoint beats empty handoff)", doc.ActiveSubagents[0].TaskID)
	}
}

// TestSynthesizeRecovery_MalformedCheckpointFallsThrough pins that a
// checkpoint with unparseable frontmatter (torn write, schema drift)
// does NOT shadow the state.json walk — we refuse to lift from a doc we
// can't trust and fall back to the well-tested legacy path.
func TestSynthesizeRecovery_MalformedCheckpointFallsThrough(t *testing.T) {
	pdir := withFleetHomeSynth(t)
	seedCoordState(t, pdir, "myproj", map[string]string{
		"fix-foo-1234": "deadbeef",
	})
	seedWorkerState(t, pdir, "myproj", "fix-foo-1234", "tdd-green", "")

	// Write a checkpoint with no closing frontmatter delimiter.
	dir := filepath.Join(pdir, "myproj")
	if err := os.WriteFile(filepath.Join(dir, "coord-checkpoint.md"),
		[]byte("---\nschema: v1\nupdated_at: garbage\n"), 0o644); err != nil {
		t.Fatalf("write malformed checkpoint: %v", err)
	}

	doc, err := SynthesizeRecoveryWithLastHandoff("deadbeef", "myproj", "", time.Now().UTC())
	if err != nil {
		t.Fatalf("SynthesizeRecoveryWithLastHandoff: %v", err)
	}
	if len(doc.ActiveSubagents) != 1 {
		t.Fatalf("ActiveSubagents: got %d want 1 (state.json fallback)", len(doc.ActiveSubagents))
	}
	if doc.ActiveSubagents[0].TaskID != "fix-foo-1234" {
		t.Errorf("task: got %q want fix-foo-1234", doc.ActiveSubagents[0].TaskID)
	}
}

// TestSynthesizeRecovery_LegacyWrapperUnchanged pins that the legacy
// SynthesizeRecovery entry point still works (it delegates to the new
// signature with an empty lastHandoffPath). No checkpoint present, so
// it walks state.json as before.
func TestSynthesizeRecovery_LegacyWrapperUnchanged(t *testing.T) {
	pdir := withFleetHomeSynth(t)
	seedCoordState(t, pdir, "myproj", map[string]string{
		"fix-foo-1234": "deadbeef",
	})
	seedWorkerState(t, pdir, "myproj", "fix-foo-1234", "push", "")

	doc, err := SynthesizeRecovery("deadbeef", "myproj", time.Now().UTC())
	if err != nil {
		t.Fatalf("SynthesizeRecovery: %v", err)
	}
	if len(doc.ActiveSubagents) != 1 || doc.ActiveSubagents[0].TaskID != "fix-foo-1234" {
		t.Fatalf("legacy wrapper regressed: %+v", doc.ActiveSubagents)
	}
}
