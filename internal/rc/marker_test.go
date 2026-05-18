package rc

import (
	"os"
	"path/filepath"
	"testing"
)

// withFleetHome points FLEET_HOME at t.TempDir() so each test gets
// an isolated ~/.fleet/. Mirrors the pattern used across the
// internal/state and internal/workers test packages.
func withFleetHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("FLEET_HOME", dir)
	// Clear inherited env-gate so tests that exercise the
	// FLEET_RC_BOOTSTRAP_DISABLED-aware code paths see a known state.
	// (Individual tests override when they need the gate set.)
	t.Setenv("FLEET_RC_BOOTSTRAP_DISABLED", "")
	return dir
}

func TestMarker_AbsentByDefault(t *testing.T) {
	withFleetHome(t)
	if MarkerPresent("demo") {
		t.Fatalf("marker should be absent under fresh FLEET_HOME")
	}
	if Enabled("demo") {
		t.Fatalf("Enabled should be false when marker absent")
	}
}

func TestMarker_WriteRemove_Idempotent(t *testing.T) {
	withFleetHome(t)
	if err := WriteMarker("demo"); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}
	if !MarkerPresent("demo") {
		t.Fatalf("marker should be present after WriteMarker")
	}
	if !Enabled("demo") {
		t.Fatalf("Enabled should be true after WriteMarker")
	}
	// Second WriteMarker is idempotent.
	if err := WriteMarker("demo"); err != nil {
		t.Fatalf("second WriteMarker: %v", err)
	}
	// First RemoveMarker clears.
	if err := RemoveMarker("demo"); err != nil {
		t.Fatalf("RemoveMarker: %v", err)
	}
	if MarkerPresent("demo") {
		t.Fatalf("marker should be absent after RemoveMarker")
	}
	// Second RemoveMarker is idempotent (no error on missing file).
	if err := RemoveMarker("demo"); err != nil {
		t.Fatalf("second RemoveMarker: %v", err)
	}
}

func TestMarker_PathShape(t *testing.T) {
	root := withFleetHome(t)
	path, err := MarkerPath("demo")
	if err != nil {
		t.Fatalf("MarkerPath: %v", err)
	}
	want := filepath.Join(root, "projects", "demo", "rc-enabled")
	if path != want {
		t.Fatalf("MarkerPath shape:\n got %q\nwant %q", path, want)
	}
}

func TestMarker_ZeroByteFile(t *testing.T) {
	withFleetHome(t)
	if err := WriteMarker("demo"); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}
	path, _ := MarkerPath("demo")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(data) != 0 {
		t.Fatalf("marker should be zero-byte; got %d bytes", len(data))
	}
}

func TestMarker_RejectsInvalidProject(t *testing.T) {
	withFleetHome(t)
	if _, err := MarkerPath("../escape"); err == nil {
		t.Fatalf("MarkerPath should reject path-traversal")
	}
}
