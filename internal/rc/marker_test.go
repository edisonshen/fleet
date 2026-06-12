package rc

import (
	"os"
	"path/filepath"
	"testing"
)

// withFleetHome points FLEET_HOME at t.TempDir() so each test gets
// an isolated ~/.fleet/. Mirrors the pattern used across the
// internal/state and internal/workers test packages.
//
// Also installs neutral stubs for the self-healing probes
// (claudeVersionFn, ownerAliveFn) so the production shell-outs don't
// fire on a dev box where `claude --version` returns a real value
// that disagrees with any recorded ClaudeVersion the test seeds. Tests
// that exercise the self-healing branch override these stubs via
// withStubVersionAndOwner.
//
// leak-rc-daemon-lifecycle PR-B: without this neutral default, tests
// that pre-date the schema bump and seed RecordedState with
// ClaudeVersion="" would trigger Up's self-heal kill+respawn on every
// run, signalling os.Getpid() and crashing the test binary.
func withFleetHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("FLEET_HOME", dir)
	// Clear inherited env-gate so tests that exercise the
	// FLEET_RC_BOOTSTRAP_DISABLED-aware code paths see a known state.
	// (Individual tests override when they need the gate set.)
	t.Setenv("FLEET_RC_BOOTSTRAP_DISABLED", "")
	// Neutral self-heal stubs: empty version + alive owner skip both
	// heal branches by default (computeHealReason returns "" on empty
	// curVer; ownerAliveFn always returns true).
	prevV := claudeVersionFn
	claudeVersionFn = func() (string, error) { return "", nil }
	prevO := ownerAliveFn
	ownerAliveFn = func(coordID string) bool { return true }
	t.Cleanup(func() {
		claudeVersionFn = prevV
		ownerAliveFn = prevO
	})
	return dir
}

func TestMarker_AbsentByDefault(t *testing.T) {
	withFleetHome(t)
	if MarkerPresent("demo") {
		t.Fatalf("legacy marker should be absent under fresh FLEET_HOME")
	}
	if DisabledMarkerPresent("demo") {
		t.Fatalf("disabled marker should be absent under fresh FLEET_HOME")
	}
	// Native model: no markers at all means ENABLED (default-on).
	if !Enabled("demo") {
		t.Fatalf("Enabled should default to true with no markers (native model)")
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

// ---------------------------------------------------------------------------
// rc-disabled opt-out marker (native model).
// ---------------------------------------------------------------------------

func TestDisabledMarker_WriteRemove_Idempotent(t *testing.T) {
	withFleetHome(t)
	if err := WriteDisabledMarker("demo"); err != nil {
		t.Fatalf("WriteDisabledMarker: %v", err)
	}
	if !DisabledMarkerPresent("demo") {
		t.Fatalf("disabled marker should be present after WriteDisabledMarker")
	}
	// Second write is idempotent.
	if err := WriteDisabledMarker("demo"); err != nil {
		t.Fatalf("second WriteDisabledMarker: %v", err)
	}
	if err := RemoveDisabledMarker("demo"); err != nil {
		t.Fatalf("RemoveDisabledMarker: %v", err)
	}
	if DisabledMarkerPresent("demo") {
		t.Fatalf("disabled marker should be absent after RemoveDisabledMarker")
	}
	// Second remove is idempotent (no error on missing file).
	if err := RemoveDisabledMarker("demo"); err != nil {
		t.Fatalf("second RemoveDisabledMarker: %v", err)
	}
}

func TestDisabledMarker_PathShape(t *testing.T) {
	root := withFleetHome(t)
	path, err := DisabledMarkerPath("demo")
	if err != nil {
		t.Fatalf("DisabledMarkerPath: %v", err)
	}
	want := filepath.Join(root, "projects", "demo", "rc-disabled")
	if path != want {
		t.Fatalf("DisabledMarkerPath shape:\n got %q\nwant %q", path, want)
	}
}

func TestDisabledMarker_ZeroByteFile(t *testing.T) {
	withFleetHome(t)
	if err := WriteDisabledMarker("demo"); err != nil {
		t.Fatalf("WriteDisabledMarker: %v", err)
	}
	path, _ := DisabledMarkerPath("demo")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(data) != 0 {
		t.Fatalf("disabled marker should be zero-byte; got %d bytes", len(data))
	}
}

func TestDisabledMarker_RejectsInvalidProject(t *testing.T) {
	withFleetHome(t)
	if _, err := DisabledMarkerPath("../escape"); err == nil {
		t.Fatalf("DisabledMarkerPath should reject path-traversal")
	}
}
