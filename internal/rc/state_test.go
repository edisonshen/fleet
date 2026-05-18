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
	for _, k := range []string{"schema", "project", "pid", "host_id", "working_dir", "session_prefix", "last_spawn_at"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("rc-state.json missing required field %q (got %v)", k, raw)
		}
	}
}
