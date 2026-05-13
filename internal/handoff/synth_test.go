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
