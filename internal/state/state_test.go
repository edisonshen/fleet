package state

import (
	"os"
	"path/filepath"
	"testing"
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
