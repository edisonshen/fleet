package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/handoff"
)

func writeAutoStyleDoc(t *testing.T, dir, agentID, taskID, project string) string {
	t.Helper()
	doc := &handoff.Doc{
		AgentID:       agentID,
		TaskID:        taskID,
		Project:       project,
		Type:          "auto-yellow",
		Number:        1,
		Timestamp:     time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC),
		Completed:     "pane",
		KeyDecisions:  handoff.Placeholder,
		SessionDocs:   handoff.Placeholder,
		OpenQuestions: handoff.Placeholder,
		NextSteps:     handoff.Placeholder,
	}
	path := filepath.Join(dir, agentID+"-20260904-120000-abcd.md")
	if err := os.WriteFile(path, []byte(handoff.Render(doc)), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func seedNarrativeCoordState(t *testing.T, root, project, coordID string) {
	t.Helper()
	dir := filepath.Join(root, "projects", project)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{
		"worker_agent_ids": map[string]string{"e2e-login-1234": "cafef00d"},
		"session_next_steps": []map[string]string{
			{"text": "wait for e2e-login-1234, then merge", "coord_id": coordID, "ts": "2026-09-04T11:59:00Z"},
		},
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "coord-state.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestHandoffEnrich_FillsCoordDocInPlace(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEET_HOME", root)
	seedNarrativeCoordState(t, root, "myproj", "deadbeef")
	path := writeAutoStyleDoc(t, root, "deadbeef", "coord-myproj", "myproj")

	var stdout, stderr bytes.Buffer
	if err := runHandoffEnrich(path, &stdout, &stderr); err != nil {
		t.Fatalf("runHandoffEnrich: %v (stderr=%s)", err, stderr.String())
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "wait for e2e-login-1234, then merge") {
		t.Fatalf("Next Steps not filled:\n%s", got)
	}
	if !strings.Contains(stdout.String(), "filled narrative sections") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

// Worker docs must never receive project-wide coord narrative.
func TestHandoffEnrich_SkipsWorkerDoc(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEET_HOME", root)
	seedNarrativeCoordState(t, root, "myproj", "cafef00d")
	path := writeAutoStyleDoc(t, root, "cafef00d", "e2e-login-1234", "myproj")
	before, _ := os.ReadFile(path)

	var stdout, stderr bytes.Buffer
	if err := runHandoffEnrich(path, &stdout, &stderr); err != nil {
		t.Fatalf("runHandoffEnrich: %v", err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatalf("worker doc was modified")
	}
	if !strings.Contains(stdout.String(), "not a coord handoff") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestHandoffEnrich_MissingDocErrors(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := runHandoffEnrich(filepath.Join(t.TempDir(), "nope.md"), &stdout, &stderr); err == nil {
		t.Fatal("expected error for missing doc")
	}
}
