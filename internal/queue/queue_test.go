package queue

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func setupFleetHome(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	if err := os.MkdirAll(filepath.Join(tmp, "queue"), 0o755); err != nil {
		t.Fatal(err)
	}
	return tmp
}

func TestSpawnFreshName(t *testing.T) {
	if got := SpawnFreshName("a1b2c3d4"); got != "spawn-fresh-a1b2c3d4" {
		t.Errorf("got %q, want spawn-fresh-a1b2c3d4", got)
	}
}

func TestWriteAndRead_RoundTrip(t *testing.T) {
	setupFleetHome(t)

	req := SpawnFresh{
		OldAgentID: "a1b2c3d4",
		HandoffDoc: "/tmp/fleet/handoffs/a1b2c3d4-20260427-184807.md",
		Project:    "rainier",
		TaskID:     "auth-fix",
	}
	path, err := WriteSpawnFresh(req)
	if err != nil {
		t.Fatalf("WriteSpawnFresh: %v", err)
	}
	if filepath.Base(path) != "spawn-fresh-a1b2c3d4.json" {
		t.Errorf("unexpected path: %s", path)
	}

	got, err := ReadSpawnFresh(path)
	if err != nil {
		t.Fatalf("ReadSpawnFresh: %v", err)
	}
	if got.OldAgentID != req.OldAgentID || got.HandoffDoc != req.HandoffDoc ||
		got.Project != req.Project || got.TaskID != req.TaskID {
		t.Errorf("round-trip mismatch: got %+v want %+v", got, req)
	}
	if got.SchemaVersion != SchemaVersion {
		t.Errorf("SchemaVersion not defaulted: got %d want %d", got.SchemaVersion, SchemaVersion)
	}
	if got.EnqueuedAt.IsZero() {
		t.Error("EnqueuedAt should have been auto-populated")
	}
}

func TestWriteSpawnFresh_RequiresFields(t *testing.T) {
	setupFleetHome(t)
	cases := []struct {
		name string
		req  SpawnFresh
	}{
		{"missing OldAgentID", SpawnFresh{HandoffDoc: "/x"}},
		{"missing HandoffDoc", SpawnFresh{OldAgentID: "a1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := WriteSpawnFresh(tc.req); err == nil {
				t.Error("expected validation error")
			}
		})
	}
}

func TestWriteSpawnFresh_OverwritesPriorRequest(t *testing.T) {
	setupFleetHome(t)

	first := SpawnFresh{OldAgentID: "a1b2c3d4", HandoffDoc: "/h/v1.md"}
	if _, err := WriteSpawnFresh(first); err != nil {
		t.Fatalf("first write: %v", err)
	}
	second := SpawnFresh{OldAgentID: "a1b2c3d4", HandoffDoc: "/h/v2.md"}
	path, err := WriteSpawnFresh(second)
	if err != nil {
		t.Fatalf("second write: %v", err)
	}

	got, err := ReadSpawnFresh(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.HandoffDoc != "/h/v2.md" {
		t.Errorf("expected v2 to clobber v1, got HandoffDoc=%q", got.HandoffDoc)
	}
}

func TestListPending_FiltersByPrefix(t *testing.T) {
	tmp := setupFleetHome(t)

	// Two spawn-fresh requests.
	if _, err := WriteSpawnFresh(SpawnFresh{OldAgentID: "aaaa1111", HandoffDoc: "/h/a.md"}); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteSpawnFresh(SpawnFresh{OldAgentID: "bbbb2222", HandoffDoc: "/h/b.md"}); err != nil {
		t.Fatal(err)
	}
	// A different queue-file kind that we should NOT pick up.
	if err := os.WriteFile(filepath.Join(tmp, "queue", "handoff-xxx.json"),
		[]byte(`{"x":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// A non-JSON file.
	if err := os.WriteFile(filepath.Join(tmp, "queue", "spawn-fresh-readme.txt"),
		[]byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := ListPending()
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	sort.Strings(got)
	if len(got) != 2 {
		t.Fatalf("got %d pending, want 2: %v", len(got), got)
	}
	if filepath.Base(got[0]) != "spawn-fresh-aaaa1111.json" {
		t.Errorf("got[0]=%s want spawn-fresh-aaaa1111.json", got[0])
	}
	if filepath.Base(got[1]) != "spawn-fresh-bbbb2222.json" {
		t.Errorf("got[1]=%s want spawn-fresh-bbbb2222.json", got[1])
	}
}

func TestListPending_EmptyDir(t *testing.T) {
	setupFleetHome(t)
	got, err := ListPending()
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 entries, got %v", got)
	}
}

func TestDelete_ReapsSidecar(t *testing.T) {
	cases := []struct {
		name            string
		setup           func(t *testing.T, path string)
		wantErr         bool
		wantJSONExists  bool
		wantKickExists  bool
		wantErrContains string
	}{
		{
			name: "json-only removed",
			setup: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "missing path nil",
		},
		{
			name: "both present both removed",
			setup: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path+".kicked", nil, 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "json absent sidecar present removed",
			setup: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path+".kicked", nil, 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "sidecar error swallowed",
			setup: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				// A non-empty dir yields deterministic non-ENOENT from os.Remove; an empty dir would be removed.
				if err := os.Mkdir(path+".kicked", 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(path+".kicked", "keep"), []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantKickExists: true,
		},
		{
			// codex P2 regression: when the queue-file removal FAILS (file
			// still present), the sidecar must SURVIVE — stripping it would
			// let fleet-guard re-kick `fleet drain` on every hook and reopen
			// the drain storm the sentinel suppresses.
			name: "json error propagates and keeps sidecar",
			setup: func(t *testing.T, path string) {
				t.Helper()
				// A non-empty dir yields deterministic non-ENOENT from os.Remove; an empty dir would be removed.
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(path, "keep"), []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path+".kicked", nil, 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantErr:         true,
			wantJSONExists:  true,
			wantKickExists:  true,
			wantErrContains: "delete queue file ",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmp := setupFleetHome(t)
			slug := strings.NewReplacer(" ", "-", "+", "-", "/", "-").Replace(tc.name)
			path := filepath.Join(tmp, "queue", slug+".json")
			if tc.setup != nil {
				tc.setup(t, path)
			}

			err := Delete(path)
			if tc.wantErr {
				if err == nil {
					t.Fatal("Delete returned nil, want error")
				}
				if tc.wantErrContains != "" && !strings.Contains(err.Error(), tc.wantErrContains) {
					t.Fatalf("Delete error=%q, want substring %q", err, tc.wantErrContains)
				}
			} else if err != nil {
				t.Fatalf("Delete: %v", err)
			}

			if got := pathExists(path); got != tc.wantJSONExists {
				t.Errorf("json exists=%t, want %t", got, tc.wantJSONExists)
			}
			if got := pathExists(path + ".kicked"); got != tc.wantKickExists {
				t.Errorf("sidecar exists=%t, want %t", got, tc.wantKickExists)
			}
		})
	}
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestReadSpawnFresh_RejectsNewerSchema(t *testing.T) {
	tmp := setupFleetHome(t)

	future := map[string]any{
		"schema_version": SchemaVersion + 1,
		"old_agent_id":   "a1",
		"handoff_doc":    "/h.md",
		"enqueued_at":    time.Now().UTC(),
	}
	data, _ := json.Marshal(future)
	path := filepath.Join(tmp, "queue", "spawn-fresh-future.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSpawnFresh(path); err == nil {
		t.Error("expected error for newer schema_version")
	}
}

func TestReadSpawnFresh_NotFound(t *testing.T) {
	setupFleetHome(t)
	_, err := ReadSpawnFresh("/nonexistent/path.json")
	if err == nil {
		t.Error("expected error for missing file")
	}
}
