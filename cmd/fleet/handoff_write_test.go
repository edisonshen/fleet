package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/handoff"
	"github.com/edisonshen/fleet/internal/queue"
)

// seedLiveRecord writes a live agent record without spawning tmux —
// `fleet handoff-write` only reads the record, it never touches the pane.
func seedLiveRecord(t *testing.T, id, taskID, project string) *agent.Record {
	t.Helper()
	rec := agent.New(id)
	rec.TaskID = taskID
	rec.Project = project
	rec.Cwd = t.TempDir()
	rec.Command = []string{"sleep", "60"}
	rec.TmuxSession = "fleet-" + id
	if err := rec.Write(); err != nil {
		t.Fatalf("write record: %v", err)
	}
	return rec
}

// seedCoordProject lays down the durable coord state a live coordinator
// accumulates while waiting on an e2e worker: coord-state.json with the
// worker map + recorded decision + next step, and the worker's state.json.
func seedCoordProject(t *testing.T, fleetHome, project, coordID string) {
	t.Helper()
	pdir := filepath.Join(fleetHome, "projects", project)
	wdir := filepath.Join(pdir, "workers", "e2e-login-1234")
	if err := os.MkdirAll(wdir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cs := map[string]any{
		"worker_agent_ids":       map[string]string{"e2e-login-1234": "beefcafe"},
		"recent_decisions":       []string{"wait for e2e-login-1234's browser run before merging #42"},
		"recent_decisions_owner": coordID,
		"session_next_steps": []map[string]string{
			{"text": "merge #42 once e2e-login-1234 reports green", "coord_id": coordID, "ts": "2026-09-05T00:00:00Z"},
		},
	}
	data, err := json.Marshal(cs)
	if err != nil {
		t.Fatalf("marshal coord-state: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pdir, "coord-state.json"), data, 0o644); err != nil {
		t.Fatalf("write coord-state.json: %v", err)
	}
	ws := map[string]any{
		"slug": "e2e-login-1234", "project": project, "phase": "e2e", "pid": 0,
		"pr_url": "https://github.com/o/r/pull/42",
	}
	data, err = json.Marshal(ws)
	if err != nil {
		t.Fatalf("marshal worker state: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wdir, "state.json"), data, 0o644); err != nil {
		t.Fatalf("write worker state.json: %v", err)
	}
}

// noGH empties PATH so the Open PRs collector's `gh pr list` fails fast
// and deterministically (→ tasks.md / checkpoint fallback) instead of
// hitting the network on a developer box.
func noGH(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

func runWrite(t *testing.T, opts *handoffWriteOpts, pane string) (handoffWriteResult, string, error) {
	t.Helper()
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	err := runHandoffWrite(opts, strings.NewReader(pane), stdout, stderr)
	var res handoffWriteResult
	if err == nil {
		if jerr := json.Unmarshal(stdout.Bytes(), &res); jerr != nil {
			t.Fatalf("stdout not JSON: %v\n%s", jerr, stdout.String())
		}
	}
	return res, stderr.String(), err
}

func section(t *testing.T, doc, heading string) string {
	t.Helper()
	marker := "## " + heading + "\n"
	i := strings.Index(doc, marker)
	if i < 0 {
		t.Fatalf("section %q missing from doc:\n%s", heading, doc)
	}
	rest := doc[i+len(marker):]
	if j := strings.Index(rest, "\n## "); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

// The bug this PR fixes: a coord auto-handed-off while waiting on an e2e
// worker must hand its successor the worker, the decision to wait, and
// the next step — from the SAME collectors `fleet handoff` uses.
func TestHandoffWrite_CoordEnrichesFromDurableState(t *testing.T) {
	noGH(t)
	home := setupFleetHome(t)
	rec := seedLiveRecord(t, "c0ffee01", "coord-myproj", "myproj")
	seedCoordProject(t, home, "myproj", rec.ID)

	pane := "⏺ Waiting for e2e-login-1234 to finish the browser run…\n"
	res, stderr, err := runWrite(t, &handoffWriteOpts{agentID: rec.ID, typ: handoff.TypeAutoYellow, contextPct: "41.5"}, pane)
	if err != nil {
		t.Fatalf("handoff-write: %v\nstderr: %s", err, stderr)
	}

	raw, err := os.ReadFile(res.DocPath)
	if err != nil {
		t.Fatalf("read doc: %v", err)
	}
	doc := string(raw)
	for _, want := range []string{
		`handoff_type: "auto-yellow"`,
		`agent_id: "` + rec.ID + `"`,
		`task_id: "coord-myproj"`,
		`project: "myproj"`,
		"context_pct_at_handoff: 41.5",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("frontmatter missing %q:\n%s", want, doc)
		}
	}
	active := section(t, doc, "Active Subagents")
	if !strings.Contains(active, `task="e2e-login-1234"`) || !strings.Contains(active, `agent_id="beefcafe"`) ||
		!strings.Contains(active, `phase="e2e"`) {
		t.Errorf("Active Subagents lacks the in-flight e2e worker: %q", active)
	}
	if kd := section(t, doc, "Key Decisions"); !strings.Contains(kd, "wait for e2e-login-1234") {
		t.Errorf("Key Decisions not lifted from coord-state: %q", kd)
	}
	if ns := section(t, doc, "Next Steps (prioritized)"); !strings.Contains(ns, "merge #42 once e2e-login-1234") {
		t.Errorf("Next Steps not lifted from coord-state: %q", ns)
	}
	if done := section(t, doc, "Completed"); !strings.Contains(done, "Waiting for e2e-login-1234") {
		t.Errorf("Completed lacks the pane capture: %q", done)
	}
	if fa := section(t, doc, "First Action (auto)"); !strings.Contains(fa, "myproj") {
		t.Errorf("First Action not project-bound: %q", fa)
	}
	for _, h := range []string{"Completed", "Key Decisions", "Next Steps (prioritized)", "Active Subagents"} {
		if strings.Contains(section(t, doc, h), handoff.Placeholder) {
			t.Errorf("%s still carries the placeholder — durable state not lifted", h)
		}
	}

	// Queue file: written AFTER the doc, points at it, successor
	// pre-allocated exactly as `fleet handoff` does.
	req, err := queue.ReadSpawnFresh(res.QueuePath)
	if err != nil {
		t.Fatalf("read queue: %v", err)
	}
	if req.HandoffDoc != res.DocPath || req.OldAgentID != rec.ID || req.Project != "myproj" || req.TaskID != "coord-myproj" {
		t.Errorf("queue file = %+v, want doc=%s old=%s", req, res.DocPath, rec.ID)
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}$`).MatchString(req.NewAgentID) || req.NewSession != "fleet-"+req.NewAgentID {
		t.Errorf("successor pre-allocation: id=%q session=%q", req.NewAgentID, req.NewSession)
	}
	if req.NewAgentID != res.NewAgentID {
		t.Errorf("stdout new_agent_id %q != queue %q", res.NewAgentID, req.NewAgentID)
	}
	if filepath.Base(res.QueuePath) != queue.SpawnFreshName(rec.ID)+".json" {
		t.Errorf("queue path %q not keyed by old agent", res.QueuePath)
	}
	if filepath.Dir(res.DocPath) != filepath.Join(home, "handoffs") {
		t.Errorf("doc %q not under <FLEET_HOME>/handoffs", res.DocPath)
	}
}

// A worker in the same project must NOT inherit the coord's project-wide
// state — the successor worker would otherwise resume against a brief
// about other agents' work.
func TestHandoffWrite_WorkerIsNotEnriched(t *testing.T) {
	noGH(t)
	home := setupFleetHome(t)
	rec := seedLiveRecord(t, "0000beef", "e2e-login-1234", "myproj")
	seedCoordProject(t, home, "myproj", "c0ffee01")

	res, stderr, err := runWrite(t, &handoffWriteOpts{agentID: rec.ID, typ: handoff.TypeAutoRed}, "npx playwright test\n")
	if err != nil {
		t.Fatalf("handoff-write: %v\nstderr: %s", err, stderr)
	}
	raw, err := os.ReadFile(res.DocPath)
	if err != nil {
		t.Fatalf("read doc: %v", err)
	}
	doc := string(raw)
	if a := section(t, doc, "Active Subagents"); strings.Contains(a, "e2e-login-1234") {
		t.Errorf("worker doc leaked coord Active Subagents: %q", a)
	}
	for _, h := range []string{"Key Decisions", "Next Steps (prioritized)"} {
		if s := section(t, doc, h); strings.TrimSpace(s) != handoff.Placeholder {
			t.Errorf("%s = %q, want placeholder for a worker", h, s)
		}
	}
	if done := section(t, doc, "Completed"); !strings.Contains(done, "npx playwright test") {
		t.Errorf("Completed lacks pane capture: %q", done)
	}
}

// Identity is exact `coord-<project>`, not a prefix match: a worker whose
// slug happens to start with "coord-" is still a worker.
func TestHandoffWrite_CoordPrefixSlugIsNotCoord(t *testing.T) {
	noGH(t)
	home := setupFleetHome(t)
	rec := seedLiveRecord(t, "0000cafe", "coord-helper-9999", "myproj")
	seedCoordProject(t, home, "myproj", "c0ffee01")

	res, _, err := runWrite(t, &handoffWriteOpts{agentID: rec.ID, typ: handoff.TypePreCompact}, "")
	if err != nil {
		t.Fatalf("handoff-write: %v", err)
	}
	raw, err := os.ReadFile(res.DocPath)
	if err != nil {
		t.Fatalf("read doc: %v", err)
	}
	if a := section(t, string(raw), "Active Subagents"); strings.Contains(a, "beefcafe") {
		t.Errorf("prefix-matched slug enriched as coord: %q", a)
	}
}

// Missing / malformed durable state degrades to placeholders — never a
// failed handoff (the alternative is a wedged agent with no successor).
func TestHandoffWrite_CoordWithMalformedStateStillWrites(t *testing.T) {
	noGH(t)
	home := setupFleetHome(t)
	rec := seedLiveRecord(t, "c0ffee02", "coord-broken", "broken")
	pdir := filepath.Join(home, "projects", "broken")
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pdir, "coord-state.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, _, err := runWrite(t, &handoffWriteOpts{agentID: rec.ID, typ: handoff.TypeAutoRed}, "pane\n")
	if err != nil {
		t.Fatalf("handoff-write must not fail on malformed coord-state: %v", err)
	}
	if _, err := os.Stat(res.DocPath); err != nil {
		t.Errorf("doc: %v", err)
	}
	if _, err := os.Stat(res.QueuePath); err != nil {
		t.Errorf("queue: %v", err)
	}
}

func TestHandoffWrite_RejectsBadInputs(t *testing.T) {
	noGH(t)
	setupFleetHome(t)
	rec := seedLiveRecord(t, "c0ffee03", "coord-p", "p")

	cases := map[string]*handoffWriteOpts{
		"manual type":  {agentID: rec.ID, typ: handoff.TypeManual},
		"unknown type": {agentID: rec.ID, typ: "yolo"},
		"bad pct":      {agentID: rec.ID, typ: handoff.TypeAutoRed, contextPct: "lots"},
		"ghost agent":  {agentID: "ghostbas", typ: handoff.TypeAutoRed},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := runWrite(t, opts, ""); err == nil {
				t.Fatalf("expected error")
			}
		})
	}
	// Nothing was published for any of the rejected calls.
	entries, err := os.ReadDir(filepath.Join(os.Getenv("FLEET_HOME"), "queue"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("rejected calls published queue files: %v", entries)
	}
}

// Manual (`fleet handoff`) and auto (`fleet handoff-write`) docs for the
// same coord must be byte-identical modulo frontmatter type / pct /
// timestamp and the pane capture — proof there is one renderer.
func TestHandoffWrite_ManualAndAutoShareRenderer(t *testing.T) {
	noGH(t)
	home := setupFleetHome(t)
	rec := seedLiveRecord(t, "c0ffee04", "coord-same", "same")
	seedCoordProject(t, home, "same", rec.ID)

	now := time.Date(2026, 9, 5, 1, 2, 3, 0, time.UTC)
	stderr := &bytes.Buffer{}
	manualPath, err := writeHandoffDoc(rec, handoff.TypeManual, nil, "", rec.Cwd, now, stderr)
	if err != nil {
		t.Fatalf("manual: %v", err)
	}
	autoPath, err := writeHandoffDoc(rec, handoff.TypeAutoYellow, nil, "", rec.Cwd, now, stderr)
	if err != nil {
		t.Fatalf("auto: %v", err)
	}
	m, _ := os.ReadFile(manualPath)
	a, _ := os.ReadFile(autoPath)
	norm := func(b []byte) string {
		return strings.Replace(string(b), `handoff_type: "`+handoff.TypeManual+`"`, `handoff_type: "`+handoff.TypeAutoYellow+`"`, 1)
	}
	if norm(m) != norm(a) {
		t.Errorf("manual and auto docs diverge:\n--- manual\n%s\n--- auto\n%s", m, a)
	}
}
