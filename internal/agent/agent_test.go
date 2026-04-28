package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestNewID_HexAndUnique(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		id := NewID()
		if len(id) != 8 {
			t.Errorf("expected 8 chars, got %q (len=%d)", id, len(id))
		}
		for _, c := range id {
			if !isHexLower(c) {
				t.Errorf("non-hex char %q in id %q", c, id)
			}
		}
		if seen[id] {
			t.Errorf("collision on %q within 100 generations", id)
		}
		seen[id] = true
	}
}

func TestNew_Defaults(t *testing.T) {
	r := New("a1b2c3d4")
	if r.SchemaVersion != SchemaVersion {
		t.Errorf("SchemaVersion: got %d want %d", r.SchemaVersion, SchemaVersion)
	}
	if r.ID != "a1b2c3d4" {
		t.Errorf("ID: got %q", r.ID)
	}
	if r.Engine != "claude-code" {
		t.Errorf("Engine: got %q want claude-code", r.Engine)
	}
	if r.Role != "executor" {
		t.Errorf("Role: got %q want executor", r.Role)
	}
	if r.Mode != "execute" {
		t.Errorf("Mode: got %q want execute", r.Mode)
	}
	if r.HandoffNumber != 1 {
		t.Errorf("HandoffNumber: got %d want 1 (first agent on task)", r.HandoffNumber)
	}
	if r.LastHandoffPath != nil {
		t.Errorf("LastHandoffPath: got %v want nil (first agent inherits nothing)", r.LastHandoffPath)
	}
	if r.SpawnedAt.IsZero() {
		t.Error("SpawnedAt should be populated")
	}
	if r.LastActivityTS.IsZero() {
		t.Error("LastActivityTS should be populated")
	}
}

func TestWriteAndLoad_RoundTrip(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	if err := os.MkdirAll(filepath.Join(tmp, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}

	r := New("a1b2c3d4")
	r.PID = 12345
	r.TmuxSession = "fleet-a1b2c3d4"
	r.TaskID = "auth-token-refresh"
	r.Project = "rainier"

	if err := r.Write(); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := Load("a1b2c3d4")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.ID != r.ID || got.PID != r.PID || got.TaskID != r.TaskID {
		t.Errorf("round-trip mismatch: got %+v want %+v", got, r)
	}
	if got.Engine != "claude-code" {
		t.Errorf("Engine field lost: got %q", got.Engine)
	}
}

func TestList_Filters(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	if err := os.MkdirAll(filepath.Join(tmp, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"aaaaaaaa", "bbbbbbbb", "cccccccc"} {
		if err := New(id).Write(); err != nil {
			t.Fatal(err)
		}
	}
	// A non-JSON file should be skipped.
	if err := os.WriteFile(filepath.Join(tmp, "agents", "README"), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The archive subdir should be skipped.
	if err := os.MkdirAll(filepath.Join(tmp, "agents", "archive"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("got %d records, want 3", len(got))
	}
}

func TestLoad_NotFound(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	if err := os.MkdirAll(filepath.Join(tmp, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := Load("nonexistent")
	if err == nil {
		t.Error("expected error for missing record")
	}
}

func TestRecord_JSONShape(t *testing.T) {
	r := New("a1b2c3d4")
	r.PID = 12345
	r.TaskID = "task1"

	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	// Verify a few load-bearing fields are present in the JSON.
	s := string(data)
	for _, want := range []string{
		`"schema_version": 1`,
		`"engine": "claude-code"`,
		`"role": "executor"`,
		`"mode": "execute"`,
		`"id": "a1b2c3d4"`,
		`"handoff_number": 1`,
		`"last_handoff_path": null`,
	} {
		if !contains(s, want) {
			t.Errorf("missing %q in JSON:\n%s", want, s)
		}
	}
}

func TestChainFields_RoundTrip(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	if err := os.MkdirAll(filepath.Join(tmp, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}

	prev := "/Users/op/.fleet/handoffs/aaaa1111-20260427-184807.md"
	r := New("bbbb2222")
	r.LastHandoffPath = &prev
	r.HandoffNumber = 4

	if err := r.Write(); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := Load("bbbb2222")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.LastHandoffPath == nil || *got.LastHandoffPath != prev {
		t.Errorf("LastHandoffPath: got %v want %q", got.LastHandoffPath, prev)
	}
	if got.HandoffNumber != 4 {
		t.Errorf("HandoffNumber: got %d want 4", got.HandoffNumber)
	}
}

func TestArchive_MovesFileToArchive(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	if err := os.MkdirAll(filepath.Join(tmp, "agents", "archive"), 0o755); err != nil {
		t.Fatal(err)
	}

	r := New("aaaa1111")
	if err := r.Write(); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if err := r.Archive(); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	// Live file is gone.
	if _, err := os.Stat(filepath.Join(tmp, "agents", "aaaa1111.json")); !os.IsNotExist(err) {
		t.Errorf("live file should be gone, stat err: %v", err)
	}
	// Archive file exists.
	if _, err := os.Stat(filepath.Join(tmp, "agents", "archive", "aaaa1111.json")); err != nil {
		t.Errorf("archive file missing: %v", err)
	}
	// List() no longer returns the archived agent.
	live, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(live) != 0 {
		t.Errorf("List() returned %d records, want 0", len(live))
	}
}

func TestArchive_ErrorWhenLiveMissing(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	if err := os.MkdirAll(filepath.Join(tmp, "agents", "archive"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := New("ghostid0")
	if err := r.Archive(); err == nil {
		t.Error("expected error archiving non-existent record")
	}
}

func TestLoad_BackfillsHandoffNumberFromZero(t *testing.T) {
	// Pre-PR records (written before chain fields existed) lack
	// handoff_number entirely. json.Unmarshal leaves the field at 0,
	// which would break the chain on first post-upgrade handoff.
	// Load() backfills 0 → 1 so the first agent on a task always
	// reports as handoff #1.
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	if err := os.MkdirAll(filepath.Join(tmp, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Hand-craft a pre-PR record JSON: no handoff_number field.
	preUpgrade := `{
  "schema_version": 1,
  "id": "preupgrd",
  "engine": "claude-code",
  "role": "executor",
  "mode": "execute",
  "task_id": "t",
  "project": "p"
}`
	if err := os.WriteFile(filepath.Join(tmp, "agents", "preupgrd.json"),
		[]byte(preUpgrade), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := Load("preupgrd")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.HandoffNumber != 1 {
		t.Errorf("HandoffNumber: got %d want 1 (backfilled from missing field)", got.HandoffNumber)
	}
}

func TestArchive_ErrorWhenArchiveAlreadyExists(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	if err := os.MkdirAll(filepath.Join(tmp, "agents", "archive"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "agents", "archive", "dupe1111.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := New("dupe1111")
	if err := r.Write(); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := r.Archive(); err == nil {
		t.Error("expected error when archive path already taken")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func isHexLower(c rune) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
}
