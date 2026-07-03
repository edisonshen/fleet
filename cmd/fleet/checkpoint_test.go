package main

// checkpoint_test.go — `fleet checkpoint {doc,decision}` RMW behavior:
// session_docs dedupe/cap, recent_decisions append, coordinator.lock
// serialization, and sibling-key preservation. See
// docs/TASK-PLAN-curated-handoff-context-5fdd.md.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/edisonshen/fleet/internal/state"
)

// runCheckpoint executes `fleet checkpoint <args...>` against a fresh root
// command and returns any error. FLEET_HOME must already be set by the
// caller (t.Setenv).
func runCheckpoint(t *testing.T, args ...string) error {
	t.Helper()
	root := newRootCmd()
	root.SetArgs(append([]string{"checkpoint"}, args...))
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	return root.Execute()
}

// readCoordState parses ~/.fleet/projects/<project>/coord-state.json.
func readCoordState(t *testing.T, home, project string) map[string]any {
	t.Helper()
	path := filepath.Join(home, "projects", project, "coord-state.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read coord-state.json: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal coord-state.json: %v\n%s", err, data)
	}
	return m
}

// seedCoordStateFile writes projects/<project>/coord-state.json with body.
func seedCoordStateFile(t *testing.T, home, project, body string) {
	t.Helper()
	dir := filepath.Join(home, "projects", project)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "coord-state.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write coord-state.json: %v", err)
	}
}

func sessionDocsOf(t *testing.T, m map[string]any) []map[string]any {
	t.Helper()
	raw, ok := m["session_docs"].([]any)
	if !ok {
		t.Fatalf("session_docs missing or wrong type: %#v", m["session_docs"])
	}
	out := make([]map[string]any, 0, len(raw))
	for _, e := range raw {
		out = append(out, e.(map[string]any))
	}
	return out
}

// Test 6 — fresh state: `fleet checkpoint doc` creates session_docs under
// the lock; sibling keys (worker_agent_ids, tick_count) are preserved.
func TestCheckpointDoc_CreatesKeyPreservesSiblings(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FLEET_HOME", home)
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	seedCoordStateFile(t, home, "myproj",
		`{"worker_agent_ids":{"slug-a":"aaaa1111"},"tick_count":7}`)

	if err := runCheckpoint(t, "doc", "--project", "myproj", "--role", "authored", "docs/DESIGN-x.md"); err != nil {
		t.Fatalf("checkpoint doc: %v", err)
	}
	m := readCoordState(t, home, "myproj")
	// Sibling keys survive.
	if _, ok := m["worker_agent_ids"].(map[string]any); !ok {
		t.Errorf("worker_agent_ids clobbered: %#v", m["worker_agent_ids"])
	}
	if tc, _ := m["tick_count"].(float64); tc != 7 {
		t.Errorf("tick_count clobbered: got %v want 7", m["tick_count"])
	}
	docs := sessionDocsOf(t, m)
	if len(docs) != 1 || docs[0]["path"] != "docs/DESIGN-x.md" || docs[0]["role"] != "authored" {
		t.Fatalf("session_docs: got %#v", docs)
	}
	if _, ok := docs[0]["ts"].(string); !ok || docs[0]["ts"] == "" {
		t.Errorf("session_docs entry missing ts: %#v", docs[0])
	}
}

// Test 2 — dedupe by path, last role wins.
func TestCheckpointDoc_DedupeLastRoleWins(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FLEET_HOME", home)
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if err := runCheckpoint(t, "doc", "--project", "myproj", "--role", "authored", "docs/D.md"); err != nil {
		t.Fatalf("first doc: %v", err)
	}
	if err := runCheckpoint(t, "doc", "--project", "myproj", "--role", "implementing", "docs/D.md"); err != nil {
		t.Fatalf("second doc: %v", err)
	}
	docs := sessionDocsOf(t, readCoordState(t, home, "myproj"))
	if len(docs) != 1 {
		t.Fatalf("expected 1 deduped entry, got %d: %#v", len(docs), docs)
	}
	if docs[0]["role"] != "implementing" {
		t.Errorf("last role should win: got %v", docs[0]["role"])
	}
}

// Cap: more than sessionDocsMax distinct docs keeps only the newest N.
func TestCheckpointDoc_CapsToNewest(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FLEET_HOME", home)
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	const n = 25 // > sessionDocsMax (20)
	for i := 0; i < n; i++ {
		if err := runCheckpoint(t, "doc", "--project", "myproj", "--role", "authored",
			fmt.Sprintf("docs/f%03d.md", i)); err != nil {
			t.Fatalf("doc %d: %v", i, err)
		}
	}
	docs := sessionDocsOf(t, readCoordState(t, home, "myproj"))
	if len(docs) != 20 {
		t.Fatalf("expected cap 20, got %d", len(docs))
	}
	// Newest kept (f024), oldest dropped (f000).
	if docs[len(docs)-1]["path"] != "docs/f024.md" {
		t.Errorf("newest not kept: %v", docs[len(docs)-1]["path"])
	}
	for _, d := range docs {
		if d["path"] == "docs/f000.md" {
			t.Errorf("oldest should have been dropped by the cap")
		}
	}
}

// --role must be one of authored|implementing.
func TestCheckpointDoc_RejectsBadRole(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FLEET_HOME", home)
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if err := runCheckpoint(t, "doc", "--project", "myproj", "--role", "bogus", "docs/x.md"); err == nil {
		t.Fatalf("expected error on bad --role")
	}
}

// A doc path carrying embedded CR/LF is flattened to spaces before it is
// stored, so it can't forge a fake `## Section` header when the handoff
// renderer emits `- <role>: <path>` verbatim. Mirrors the decision-text
// flatten; regression for the section-injection vector.
func TestCheckpointDoc_FlattensNewlinesInPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FLEET_HOME", home)
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	// A path attempting to inject a forged header + a CR variant.
	inject := "docs/x.md\n## INJECTED HEADER\r- forged: docs/evil.md"
	if err := runCheckpoint(t, "doc", "--project", "myproj", "--role", "authored", inject); err != nil {
		t.Fatalf("checkpoint doc: %v", err)
	}
	docs := sessionDocsOf(t, readCoordState(t, home, "myproj"))
	if len(docs) != 1 {
		t.Fatalf("expected 1 entry, got %d: %#v", len(docs), docs)
	}
	got, _ := docs[0]["path"].(string)
	if strings.ContainsAny(got, "\r\n") {
		t.Errorf("stored path retains CR/LF (header-injection vector): %q", got)
	}
	// The '##' bytes survive but no longer start a line, so the renderer's
	// `- <role>: <path>` stays a single bullet — no forged section.
	want := "docs/x.md ## INJECTED HEADER - forged: docs/evil.md"
	if got != want {
		t.Errorf("flattened path: got %q want %q", got, want)
	}
}

// Test 7 — `fleet checkpoint decision` appends to recent_decisions.
func TestCheckpointDecision_AppendsBuffer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FLEET_HOME", home)
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	seedCoordStateFile(t, home, "myproj", `{"tick_count":3}`)
	if err := runCheckpoint(t, "decision", "--project", "myproj",
		"Stopped rebase of PR #224 — superseded, task paused"); err != nil {
		t.Fatalf("checkpoint decision: %v", err)
	}
	m := readCoordState(t, home, "myproj")
	raw, ok := m["recent_decisions"].([]any)
	if !ok || len(raw) != 1 {
		t.Fatalf("recent_decisions: got %#v", m["recent_decisions"])
	}
	if raw[0] != "Stopped rebase of PR #224 — superseded, task paused" {
		t.Errorf("decision text: got %q", raw[0])
	}
	// Sibling preserved.
	if tc, _ := m["tick_count"].(float64); tc != 3 {
		t.Errorf("tick_count clobbered: %v", m["tick_count"])
	}
}

// Blank / whitespace-only decision text is rejected (no empty bullet).
func TestCheckpointDecision_RejectsBlank(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FLEET_HOME", home)
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if err := runCheckpoint(t, "decision", "--project", "myproj", "   "); err == nil {
		t.Fatalf("expected error on blank decision text")
	}
}

// Test 12 — concurrent checkpoint writers serialize on coordinator.lock:
// every distinct decision survives (no lost update / no corruption).
func TestCheckpoint_ConcurrentWritersSerialize(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FLEET_HOME", home)
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	// Pre-seed a sibling key that must survive all the concurrent RMWs.
	seedCoordStateFile(t, home, "myproj", `{"worker_agent_ids":{"keep":"me01"}}`)

	const writers = 8
	var wg sync.WaitGroup
	errs := make([]error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = runCheckpoint(t, "decision", "--project", "myproj",
				fmt.Sprintf("decision-%d", i))
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
	}
	m := readCoordState(t, home, "myproj")
	raw, ok := m["recent_decisions"].([]any)
	if !ok {
		t.Fatalf("recent_decisions missing: %#v", m)
	}
	seen := map[string]bool{}
	for _, d := range raw {
		seen[d.(string)] = true
	}
	// recent_decisions caps at 10; writers=8 < cap so all survive.
	for i := 0; i < writers; i++ {
		if !seen[fmt.Sprintf("decision-%d", i)] {
			t.Errorf("lost decision-%d under concurrency: %v", i, raw)
		}
	}
	if _, ok := m["worker_agent_ids"].(map[string]any); !ok {
		t.Errorf("sibling worker_agent_ids lost under concurrency: %#v", m["worker_agent_ids"])
	}
}
