package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestRunCoordOwnerRequiresProject: empty --project is a usage error.
func TestRunCoordOwnerRequiresProject(t *testing.T) {
	var buf bytes.Buffer
	if err := runCoordOwner("", true, &buf); err == nil {
		t.Fatal("runCoordOwner(\"\") = nil, want a usage error")
	}
}

// TestRunCoordOwnerJSONShapeFreeLease: a project with no lease record on disk
// (fresh FLEET_HOME) emits a well-formed JSON object with the project set and
// every identity field empty. This pins the shape loop.py's _read_coord_lease_
// identity parses, and exercises the real coordlock read path returning empty.
func TestRunCoordOwnerJSONShapeFreeLease(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())

	var buf bytes.Buffer
	if err := runCoordOwner("proj-no-lease", true, &buf); err != nil {
		t.Fatalf("runCoordOwner: %v", err)
	}

	var got coordOwnerInfo
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v (raw=%q)", err, buf.String())
	}
	if got.Project != "proj-no-lease" {
		t.Errorf("Project = %q, want %q", got.Project, "proj-no-lease")
	}
	if got.LiveOwnerID != "" || got.OwnerID != "" || got.HandoffSuccessorID != "" {
		t.Errorf("free lease must report empty IDs; got %+v", got)
	}
	// The JSON keys are the loop.py contract — assert they are present verbatim.
	for _, key := range []string{"live_owner_id", "owner_id", "handoff_successor_id"} {
		if !strings.Contains(buf.String(), `"`+key+`"`) {
			t.Errorf("JSON output missing key %q: %s", key, buf.String())
		}
	}
}

// TestRunCoordOwnerTextMode: the human (non-JSON) form prints all four labels.
func TestRunCoordOwnerTextMode(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())

	var buf bytes.Buffer
	if err := runCoordOwner("proj-x", false, &buf); err != nil {
		t.Fatalf("runCoordOwner: %v", err)
	}
	for _, label := range []string{"project:", "live_owner_id:", "owner_id:", "handoff_successor_id:"} {
		if !strings.Contains(buf.String(), label) {
			t.Errorf("text output missing %q: %s", label, buf.String())
		}
	}
}
