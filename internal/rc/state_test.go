package rc

import (
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"
)

func TestState_RoundTrip(t *testing.T) {
	withFleetHome(t)
	rec := RecordedState{
		Project:       "demo",
		PID:           42,
		HostID:        "test.local",
		WorkingDir:    "/tmp/demo",
		SessionPrefix: SessionPrefix,
		LastSpawnAt:   time.Now().UTC().Truncate(time.Second),
	}
	if err := WriteState(rec); err != nil {
		t.Fatalf("WriteState: %v", err)
	}
	got, err := ReadState("demo")
	if err != nil {
		t.Fatalf("ReadState: %v", err)
	}
	if got.PID != rec.PID || got.HostID != rec.HostID || got.WorkingDir != rec.WorkingDir {
		t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", got, rec)
	}
	if got.Schema != SchemaVersion {
		t.Fatalf("Schema should default to %q; got %q", SchemaVersion, got.Schema)
	}
	if got.SessionPrefix != SessionPrefix {
		t.Fatalf("SessionPrefix should default to %q; got %q", SessionPrefix, got.SessionPrefix)
	}
}

func TestState_AbsentReturnsErrStateMissing(t *testing.T) {
	withFleetHome(t)
	_, err := ReadState("demo")
	if !errors.Is(err, ErrStateMissing) {
		t.Fatalf("ReadState should return ErrStateMissing on absent file; got %v", err)
	}
}

func TestState_RemoveIdempotent(t *testing.T) {
	withFleetHome(t)
	if err := RemoveState("demo"); err != nil {
		t.Fatalf("RemoveState absent: %v", err)
	}
	if err := WriteState(RecordedState{Project: "demo", PID: 1}); err != nil {
		t.Fatalf("WriteState: %v", err)
	}
	if err := RemoveState("demo"); err != nil {
		t.Fatalf("RemoveState: %v", err)
	}
	if err := RemoveState("demo"); err != nil {
		t.Fatalf("second RemoveState: %v", err)
	}
}

func TestState_JSONShape(t *testing.T) {
	withFleetHome(t)
	rec := RecordedState{
		Project:    "demo",
		PID:        12345,
		HostID:     "host.example",
		WorkingDir: "/tmp/repo",
	}
	if err := WriteState(rec); err != nil {
		t.Fatalf("WriteState: %v", err)
	}
	path, _ := StatePath("demo")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	// leak-rc-daemon-lifecycle PR-B: schema v2 adds claude_version and
	// owning_coord_id so superseded daemons + dead-owner orphans can be
	// recognized as orphans and self-healed.
	for _, k := range []string{"schema", "project", "pid", "host_id", "working_dir", "session_prefix", "last_spawn_at", "claude_version", "owning_coord_id"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("rc-state.json missing required field %q (got %v)", k, raw)
		}
	}
}

// TestState_SchemaV2_RoundTrip pins the new ClaudeVersion +
// OwningCoordID fields end-to-end (write → read). leak-rc-daemon-lifecycle
// PR-B: without these fields recorded, Up's idempotent branch can't
// detect a superseded claude version or a dead owning coord.
func TestState_SchemaV2_RoundTrip(t *testing.T) {
	withFleetHome(t)
	rec := RecordedState{
		Project:        "demo",
		PID:            42,
		HostID:         "test.local",
		WorkingDir:     "/tmp/demo",
		SessionPrefix:  SessionPrefix,
		LastSpawnAt:    time.Now().UTC().Truncate(time.Second),
		ClaudeVersion:  "2.1.156",
		OwningCoordID: "coord-abc123",
	}
	if err := WriteState(rec); err != nil {
		t.Fatalf("WriteState: %v", err)
	}
	got, err := ReadState("demo")
	if err != nil {
		t.Fatalf("ReadState: %v", err)
	}
	if got.ClaudeVersion != rec.ClaudeVersion {
		t.Errorf("ClaudeVersion=%q want %q", got.ClaudeVersion, rec.ClaudeVersion)
	}
	if got.OwningCoordID != rec.OwningCoordID {
		t.Errorf("OwningCoordID=%q want %q", got.OwningCoordID, rec.OwningCoordID)
	}
	if got.Schema != SchemaVersion {
		t.Errorf("Schema=%q want %q (PR-B should bump SchemaVersion)", got.Schema, SchemaVersion)
	}
}

// TestState_SchemaV1_BackcompatLoads pins the back-compat read path:
// older rc-state.json files written under v1 lacked claude_version +
// owning_coord_id. ReadState must succeed (empty values) so the
// self-healing Up path can treat empty version as "always stale" and
// force one heal cycle.
func TestState_SchemaV1_BackcompatLoads(t *testing.T) {
	root := withFleetHome(t)
	projDir := root + "/projects/legacy"
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// Hand-author a v1-shape rc-state.json (no claude_version / owning_coord_id).
	v1 := `{"schema":"v1","project":"legacy","pid":999,"host_id":"h1","working_dir":"/tmp/legacy","session_prefix":"fleet-coord","last_spawn_at":"2026-01-01T00:00:00Z"}` + "\n"
	if err := os.WriteFile(projDir+"/rc-state.json", []byte(v1), 0o644); err != nil {
		t.Fatalf("write v1 state: %v", err)
	}
	got, err := ReadState("legacy")
	if err != nil {
		t.Fatalf("ReadState (v1 backcompat): %v", err)
	}
	if got.PID != 999 {
		t.Fatalf("v1 read lost PID; got %d want 999", got.PID)
	}
	if got.ClaudeVersion != "" {
		t.Errorf("v1 backcompat: ClaudeVersion should default empty; got %q", got.ClaudeVersion)
	}
	if got.OwningCoordID != "" {
		t.Errorf("v1 backcompat: OwningCoordID should default empty; got %q", got.OwningCoordID)
	}
}
