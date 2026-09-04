package handoff

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// renderAutoStyleDoc mirrors what skills/fleet-guard/handoff.py:_render_doc
// produces for an auto handoff: Completed carries the pane capture, every
// other narrative section is Placeholder, Active Subagents / Open PRs hold
// whatever the live walk found.
func renderAutoStyleDoc(t *testing.T, agentID, project, pane string, prev *string, subs []ActiveSubagent) []byte {
	t.Helper()
	doc := &Doc{
		AgentID:         agentID,
		TaskID:          "coord-" + project,
		Project:         project,
		Type:            "auto-yellow",
		Number:          2,
		PreviousPath:    prev,
		Timestamp:       time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC),
		Completed:       pane,
		KeyDecisions:    Placeholder,
		SessionDocs:     Placeholder,
		OpenQuestions:   Placeholder,
		NextSteps:       Placeholder,
		ActiveSubagents: subs,
	}
	if pane == "" {
		doc.Completed = Placeholder
	}
	return []byte(Render(doc))
}

// seedCoordStateNarrative writes a coord-state.json carrying the session-
// scoped narrative buffers the collectors read, all stamped for coordID.
func seedCoordStateNarrative(t *testing.T, projectsRoot, project, coordID string, workers map[string]string, decisions, nextSteps, docs []string) {
	t.Helper()
	dir := filepath.Join(projectsRoot, project)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	ts := "2026-09-04T11:59:00Z"
	var ns []map[string]string
	for _, s := range nextSteps {
		ns = append(ns, map[string]string{"text": s, "coord_id": coordID, "ts": ts})
	}
	var sd []map[string]string
	for _, d := range docs {
		sd = append(sd, map[string]string{"path": d, "role": "design", "coord_id": coordID, "ts": ts})
	}
	body := map[string]any{
		"worker_agent_ids":       workers,
		"recent_decisions":       decisions,
		"recent_decisions_owner": coordID,
		"session_next_steps":     ns,
		"session_docs":           sd,
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "coord-state.json"), data, 0o644); err != nil {
		t.Fatalf("write coord-state.json: %v", err)
	}
}

func sectionBody(t *testing.T, raw []byte, heading string) string {
	t.Helper()
	s, e, ok := renderedSection(raw, heading)
	if !ok {
		t.Fatalf("section %q not found in:\n%s", heading, raw)
	}
	return string(raw[s:e])
}

// The bug: an auto handoff fired while the coord sat waiting on an e2e
// worker used to hand the successor a doc with Placeholder in every
// narrative section. Durable state has the decisions / next steps / docs —
// enrichment must lift them into the rendered doc.
func TestEnrichRenderedDoc_FillsNarrativeFromDurableState(t *testing.T) {
	pdir := withFleetHomeSynth(t)
	seedCoordStateNarrative(t, pdir, "myproj", "deadbeef",
		map[string]string{"e2e-login-1234": "cafef00d"},
		[]string{"run the e2e suite in a worker, not inline"},
		[]string{"wait for e2e-login-1234 to report PASS, then merge #42"},
		[]string{"docs/DESIGN-e2e.md"},
	)

	pane := "> waiting for e2e-login-1234 to finish the browser run..."
	subs := []ActiveSubagent{{TaskID: "e2e-login-1234", Branch: "worker/e2e-login-1234", LastPhase: "e2e", Status: "in-progress", AgentID: "cafef00d"}}
	raw := renderAutoStyleDoc(t, "deadbeef", "myproj", pane, nil, subs)

	out, changed := EnrichRenderedDoc(raw, "myproj", "deadbeef", "", nil)
	if !changed {
		t.Fatalf("expected enrichment to change the doc")
	}
	if got := sectionBody(t, out, headingKeyDecisions); got != "- run the e2e suite in a worker, not inline" {
		t.Errorf("Key Decisions = %q", got)
	}
	if got := sectionBody(t, out, headingNextSteps); !strings.Contains(got, "wait for e2e-login-1234 to report PASS") {
		t.Errorf("Next Steps = %q", got)
	}
	if got := sectionBody(t, out, headingSessionDocs); !strings.Contains(got, "docs/DESIGN-e2e.md") {
		t.Errorf("Docs = %q", got)
	}
	// Pane capture is preserved; machine sections untouched.
	if got := sectionBody(t, out, headingCompleted); got != pane {
		t.Errorf("Completed = %q, want pane capture kept", got)
	}
	got, _, err := ParseActiveSubagents(out)
	if err != nil || len(got) != 1 || got[0].TaskID != "e2e-login-1234" || got[0].AgentID != "cafef00d" {
		t.Fatalf("Active Subagents after enrichment = %#v (%v)", got, err)
	}
	// The whole doc must still round-trip through the frontmatter parser.
	fm, err := ParseFrontmatter(out)
	if err != nil || fm.AgentID != "deadbeef" || fm.TaskID != "coord-myproj" || fm.Project != "myproj" {
		t.Fatalf("frontmatter after enrichment = %#v (%v)", fm, err)
	}
}

// Checkpoint completions go ABOVE the pane capture so the successor sees
// what shipped this session AND the last screen — neither is lost.
func TestEnrichRenderedDoc_PrependsCheckpointCompletionsToPane(t *testing.T) {
	pdir := withFleetHomeSynth(t)
	now := time.Now().UTC()
	seedCheckpoint(t, pdir, "myproj", now, nil, nil, []string{"- dispatched e2e-login-1234"})
	// Append a Completed (recent) buffer to the seeded checkpoint.
	cpPath := filepath.Join(pdir, "myproj", "coord-checkpoint.md")
	data, err := os.ReadFile(cpPath)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, []byte("\n### Completed (recent)\n- merged #41 fix-auth-0001\n")...)
	if err := os.WriteFile(cpPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	prev := writeFakeHandoffDoc(t, pdir, "deadbeef", now.Add(-time.Hour))

	raw := renderAutoStyleDoc(t, "deadbeef", "myproj", "pane line", &prev, nil)
	out, changed := EnrichRenderedDoc(raw, "myproj", "deadbeef", prev, nil)
	if !changed {
		t.Fatal("expected change")
	}
	if got := sectionBody(t, out, headingCompleted); got != "- merged #41 fix-auth-0001\n\npane line" {
		t.Errorf("Completed = %q", got)
	}
	if got := sectionBody(t, out, headingKeyDecisions); got != "- dispatched e2e-login-1234" {
		t.Errorf("Key Decisions (checkpoint fallback) = %q", got)
	}
}

func TestEnrichRenderedDoc_NoDurableStateLeavesDocUnchanged(t *testing.T) {
	withFleetHomeSynth(t)
	raw := renderAutoStyleDoc(t, "deadbeef", "myproj", "pane", nil, nil)
	out, changed := EnrichRenderedDoc(raw, "myproj", "deadbeef", "", nil)
	if changed || string(out) != string(raw) {
		t.Fatalf("expected no-op; changed=%v", changed)
	}
}

// A predecessor's narrative must not leak into the successor's doc.
func TestEnrichRenderedDoc_SkipsForeignGeneration(t *testing.T) {
	pdir := withFleetHomeSynth(t)
	seedCoordStateNarrative(t, pdir, "myproj", "0ldc00rd", nil,
		[]string{"old decision"}, []string{"old next step"}, []string{"docs/old.md"})
	raw := renderAutoStyleDoc(t, "deadbeef", "myproj", "", nil, nil)
	out, changed := EnrichRenderedDoc(raw, "myproj", "deadbeef", "", nil)
	if changed {
		t.Fatalf("foreign-generation state must be ignored; got:\n%s", out)
	}
}

// Re-running on an already-filled doc must not duplicate or clobber.
func TestEnrichRenderedDoc_Idempotent(t *testing.T) {
	pdir := withFleetHomeSynth(t)
	seedCoordStateNarrative(t, pdir, "myproj", "deadbeef", nil,
		[]string{"d1"}, []string{"n1"}, []string{"docs/x.md"})
	raw := renderAutoStyleDoc(t, "deadbeef", "myproj", "pane", nil, nil)
	once, _ := EnrichRenderedDoc(raw, "myproj", "deadbeef", "", nil)
	twice, changed := EnrichRenderedDoc(once, "myproj", "deadbeef", "", nil)
	if changed || string(twice) != string(once) {
		t.Fatalf("second pass must be a no-op")
	}
}

// A pane capture that itself contains markdown H2 lines must not confuse
// section delimiting — bodies end at the next KNOWN heading only.
func TestEnrichRenderedDoc_PaneWithHeadingsStaysIntact(t *testing.T) {
	pdir := withFleetHomeSynth(t)
	seedCoordStateNarrative(t, pdir, "myproj", "deadbeef", nil, []string{"d1"}, nil, nil)
	pane := "## Summary\nworker printed a header\n\n## Details\nmore"
	raw := renderAutoStyleDoc(t, "deadbeef", "myproj", pane, nil, nil)
	out, changed := EnrichRenderedDoc(raw, "myproj", "deadbeef", "", nil)
	if !changed {
		t.Fatal("expected Key Decisions fill")
	}
	if got := sectionBody(t, out, headingCompleted); got != pane {
		t.Errorf("Completed = %q", got)
	}
	if got := sectionBody(t, out, headingKeyDecisions); got != "- d1" {
		t.Errorf("Key Decisions = %q", got)
	}
}

func TestParseFrontmatter(t *testing.T) {
	prev := "/home/u/.fleet/handoffs/deadbeef-20260904-110000-abcd.md"
	raw := renderAutoStyleDoc(t, "deadbeef", "myproj", "", &prev, nil)
	fm, err := ParseFrontmatter(raw)
	if err != nil {
		t.Fatal(err)
	}
	if fm.AgentID != "deadbeef" || fm.TaskID != "coord-myproj" || fm.Project != "myproj" || fm.PreviousHandoff != prev {
		t.Fatalf("got %#v", fm)
	}
	raw = renderAutoStyleDoc(t, "deadbeef", "myproj", "", nil, nil)
	fm, err = ParseFrontmatter(raw)
	if err != nil || fm.PreviousHandoff != "" {
		t.Fatalf("null previous_handoff: %#v (%v)", fm, err)
	}
	if _, err := ParseFrontmatter([]byte("no frontmatter")); err == nil {
		t.Fatal("expected error")
	}
}
