package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBootstrap_CreatesAllSubdirs(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)

	root, err := Bootstrap()
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if root != tmp {
		t.Fatalf("Root mismatch: got %q want %q", root, tmp)
	}

	for _, sub := range subdirs {
		path := filepath.Join(tmp, sub)
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("missing subdir %s: %v", sub, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("%s is not a directory", sub)
		}
	}
}

func TestBootstrap_Idempotent(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)

	if _, err := Bootstrap(); err != nil {
		t.Fatalf("first Bootstrap: %v", err)
	}
	if _, err := Bootstrap(); err != nil {
		t.Fatalf("second Bootstrap: %v", err)
	}
}

func TestWriteAtomic_PublishesFile(t *testing.T) {
	tmp := t.TempDir()
	dest := filepath.Join(tmp, "out.json")
	want := []byte(`{"hello":"world"}`)

	if err := WriteAtomic(dest, want); err != nil {
		t.Fatalf("WriteAtomic: %v", err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("content mismatch: got %q want %q", got, want)
	}
}

func TestWriteAtomic_NoTmpLeftover(t *testing.T) {
	tmp := t.TempDir()
	dest := filepath.Join(tmp, "out.json")

	if err := WriteAtomic(dest, []byte("ok")); err != nil {
		t.Fatalf("WriteAtomic: %v", err)
	}

	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "out.json" {
			t.Errorf("unexpected leftover: %s", e.Name())
		}
	}
}

func TestWriteAtomic_OverwritesExisting(t *testing.T) {
	tmp := t.TempDir()
	dest := filepath.Join(tmp, "out.json")

	if err := WriteAtomic(dest, []byte("v1")); err != nil {
		t.Fatalf("first WriteAtomic: %v", err)
	}
	if err := WriteAtomic(dest, []byte("v2")); err != nil {
		t.Fatalf("second WriteAtomic: %v", err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "v2" {
		t.Errorf("got %q want v2", got)
	}
}

func TestRoot_HonorsFLEET_HOME(t *testing.T) {
	t.Setenv("FLEET_HOME", "/tmp/fleet-test-root")
	root, err := Root()
	if err != nil {
		t.Fatalf("Root: %v", err)
	}
	if root != "/tmp/fleet-test-root" {
		t.Errorf("got %q", root)
	}
}

func TestAgentPath(t *testing.T) {
	t.Setenv("FLEET_HOME", "/tmp/fleet-test")
	got, err := AgentPath("a1b2")
	if err != nil {
		t.Fatalf("AgentPath: %v", err)
	}
	want := "/tmp/fleet-test/agents/a1b2.json"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestAgentArchivePath(t *testing.T) {
	t.Setenv("FLEET_HOME", "/tmp/fleet-test")
	got, err := AgentArchivePath("a1b2")
	if err != nil {
		t.Fatalf("AgentArchivePath: %v", err)
	}
	want := "/tmp/fleet-test/agents/archive/a1b2.json"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestHandoffDir(t *testing.T) {
	t.Setenv("FLEET_HOME", "/tmp/fleet-test")
	got, err := HandoffDir()
	if err != nil {
		t.Fatalf("HandoffDir: %v", err)
	}
	want := "/tmp/fleet-test/handoffs"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestHandoffPath_FormatsUTCTimestamp(t *testing.T) {
	t.Setenv("FLEET_HOME", "/tmp/fleet-test")

	// Pacific time input — must be rendered as the equivalent UTC
	// in the filename so different operators produce identical paths
	// for identical handoffs.
	pacific, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skip("zoneinfo not available on this platform")
	}
	ts := time.Date(2026, 4, 15, 7, 32, 0, 0, pacific) // 14:32:00 UTC

	got, err := HandoffPath("a1b2", ts)
	if err != nil {
		t.Fatalf("HandoffPath: %v", err)
	}
	want := "/tmp/fleet-test/handoffs/a1b2-20260415-143200.md"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestHandoffPath_AlreadyUTCRoundTrips(t *testing.T) {
	t.Setenv("FLEET_HOME", "/tmp/fleet-test")
	ts := time.Date(2026, 4, 27, 18, 48, 7, 0, time.UTC)
	got, err := HandoffPath("7f3a92e1", ts)
	if err != nil {
		t.Fatalf("HandoffPath: %v", err)
	}
	want := "/tmp/fleet-test/handoffs/7f3a92e1-20260427-184807.md"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestQueueDir(t *testing.T) {
	t.Setenv("FLEET_HOME", "/tmp/fleet-test")
	got, err := QueueDir()
	if err != nil {
		t.Fatalf("QueueDir: %v", err)
	}
	want := "/tmp/fleet-test/queue"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestQueuePath_AppendsJSONExtension(t *testing.T) {
	t.Setenv("FLEET_HOME", "/tmp/fleet-test")
	got, err := QueuePath("spawn-fresh-a1b2")
	if err != nil {
		t.Fatalf("QueuePath: %v", err)
	}
	want := "/tmp/fleet-test/queue/spawn-fresh-a1b2.json"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestProjectLockPath(t *testing.T) {
	t.Setenv("FLEET_HOME", "/tmp/fleet-test")
	got, err := ProjectLockPath("rainier")
	if err != nil {
		t.Fatalf("ProjectLockPath: %v", err)
	}
	want := "/tmp/fleet-test/projects/.locks/rainier.lock"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestProjectLockPath_RejectsUnsafeNames(t *testing.T) {
	t.Setenv("FLEET_HOME", "/tmp/fleet-test")
	for _, name := range []string{
		"",
		".",
		"..",
		"owner/repo",       // would land in non-existent .locks/owner/
		"../../etc/passwd", // path traversal
		"foo bar",          // space
		"foo:bar",          // colon
		"foo\nbar",         // newline
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ProjectLockPath(name); err == nil {
				t.Errorf("expected ProjectLockPath(%q) to reject, got nil error", name)
			}
		})
	}
}

func TestValidateProjectName_Accepts(t *testing.T) {
	for _, name := range []string{
		"rainier",
		"gift-finder",
		"caching",
		"v2.1",
		"my_project",
		"abc123",
		"a",
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateProjectName(name); err != nil {
				t.Errorf("ValidateProjectName(%q) rejected: %v", name, err)
			}
		})
	}
}

func TestBootstrap_CreatesQueueAndHandoffAndLockDirs(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)

	if _, err := Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	for _, sub := range []string{"queue", "handoffs", "projects/.locks", "agents/archive"} {
		info, err := os.Stat(filepath.Join(tmp, sub))
		if err != nil {
			t.Errorf("missing %s: %v", sub, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("%s is not a directory", sub)
		}
	}
}
