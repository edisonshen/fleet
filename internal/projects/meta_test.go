package projects

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMeta_RoundTrip pins the basic Read/Write contract: a Meta
// written to disk parses back to an equal struct.
func TestMeta_RoundTrip(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())

	m := Meta{
		Schema:   "v1",
		RepoPath: "/abs/path/to/repo",
		AddedAt:  time.Date(2026, 5, 9, 20, 30, 0, 0, time.UTC),
	}
	if err := Write("demo", m); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := Read("demo")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Schema != m.Schema {
		t.Errorf("Schema=%q want %q", got.Schema, m.Schema)
	}
	if got.RepoPath != m.RepoPath {
		t.Errorf("RepoPath=%q want %q", got.RepoPath, m.RepoPath)
	}
	if !got.AddedAt.Equal(m.AddedAt) {
		t.Errorf("AddedAt=%v want %v", got.AddedAt, m.AddedAt)
	}
}

// TestMeta_WriteAtomicNoLeftoverTmp regresses the "atomic publish"
// guarantee: after a successful Write, no .tmp files linger in the
// project's directory. Mirrors hidden_test.go's atomic check.
func TestMeta_WriteAtomicNoLeftoverTmp(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEET_HOME", root)

	m := Meta{Schema: "v1", RepoPath: "/abs/path", AddedAt: time.Now().UTC()}
	if err := Write("demo", m); err != nil {
		t.Fatalf("Write: %v", err)
	}

	dir := filepath.Join(root, "projects", "demo")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp.") {
			t.Errorf("leftover tmp file: %s", e.Name())
		}
	}
}

// TestMeta_ReadMissingReturnsErrNotFound: callers must distinguish
// "no meta on disk" from "read failed". Existing projects without
// meta.json must continue to work — Read signals absence so callers
// can choose to ignore or fall back.
func TestMeta_ReadMissingReturnsErrNotFound(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())
	_, err := Read("ghost")
	if err == nil {
		t.Fatal("expected error reading missing meta, got nil")
	}
	if err != ErrNotFound {
		t.Errorf("err=%v, want ErrNotFound", err)
	}
}

// TestMeta_OnDiskShape pins the JSON keys the spec requires
// (schema, repo_path, added_at). Future readers (per-project
// default-cwd, summary feature) depend on these names — bumping
// them is a schema change.
func TestMeta_OnDiskShape(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEET_HOME", root)

	m := Meta{
		Schema:   "v1",
		RepoPath: "/abs/path",
		AddedAt:  time.Date(2026, 5, 9, 20, 30, 0, 0, time.UTC),
	}
	if err := Write("demo", m); err != nil {
		t.Fatalf("Write: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(root, "projects", "demo", "meta.json"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if raw["schema"] != "v1" {
		t.Errorf("schema field=%v, want v1", raw["schema"])
	}
	if raw["repo_path"] != "/abs/path" {
		t.Errorf("repo_path field=%v, want /abs/path", raw["repo_path"])
	}
	if _, ok := raw["added_at"]; !ok {
		t.Errorf("added_at field missing; got keys=%v", raw)
	}
}

// TestMeta_RejectsInvalidProjectName regresses the safe-name rule.
// The state package validates project names; Write must fail loudly
// on inputs that would silently alias on disk.
func TestMeta_RejectsInvalidProjectName(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())
	err := Write("Foo/Bar", Meta{Schema: "v1", RepoPath: "/x", AddedAt: time.Now().UTC()})
	if err == nil {
		t.Fatal("expected validation error on bad project name, got nil")
	}
}
