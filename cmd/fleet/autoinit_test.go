package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMaybeAutoInit_FreshHome installs the skill when nothing exists.
// The check that runInit's "wrote:" log appears proves the install
// actually ran (vs a no-op).
func TestMaybeAutoInit_FreshHome(t *testing.T) {
	tmp := t.TempDir()
	claudeHome := filepath.Join(tmp, ".claude")

	var out bytes.Buffer
	maybeAutoInit(&out, claudeHome)

	got := out.String()
	if !strings.Contains(got, "first run") {
		t.Errorf("expected first-run notice in stdout, got:\n%s", got)
	}
	if !strings.Contains(got, "wrote:") {
		t.Errorf("expected runInit to write skill files, got:\n%s", got)
	}

	mainPath := filepath.Join(claudeHome, "skills", "fleet-guard", "main.py")
	if _, err := os.Stat(mainPath); err != nil {
		t.Errorf("main.py should exist after auto-init: %v", err)
	}
}

// TestMaybeAutoInit_AlreadyInstalled skips when main.py exists.
// Important: the operator who ran `fleet init` once shouldn't see a
// "first run" message every dispatch.
func TestMaybeAutoInit_AlreadyInstalled(t *testing.T) {
	tmp := t.TempDir()
	claudeHome := filepath.Join(tmp, ".claude")
	skillRoot := filepath.Join(claudeHome, "skills", "fleet-guard")
	if err := os.MkdirAll(skillRoot, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	mainPath := filepath.Join(skillRoot, "main.py")
	if err := os.WriteFile(mainPath, []byte("# stub"), 0o644); err != nil {
		t.Fatalf("write stub main.py: %v", err)
	}

	var out bytes.Buffer
	maybeAutoInit(&out, claudeHome)

	if got := out.String(); got != "" {
		t.Errorf("expected silence when already installed, got:\n%s", got)
	}

	// Stub content should be untouched (no force-overwrite).
	data, err := os.ReadFile(mainPath)
	if err != nil {
		t.Fatalf("read main.py: %v", err)
	}
	if string(data) != "# stub" {
		t.Errorf("auto-init clobbered existing main.py; got %q", string(data))
	}
}

// TestMaybeAutoInit_DispatchHooked is the regression test for the
// integration: runDispatch must call maybeAutoInit before the rest of
// its work. We verify by stubbing the home dir to a temp claudeHome
// and checking that dispatch's stdout includes the install logs on
// first run. (The dispatch itself fails because tmux isn't usually
// available in CI — that failure is fine; auto-init runs before the
// failure point.)
func TestMaybeAutoInit_DispatchHooked(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	claudeMain := filepath.Join(tmp, ".claude", "skills", "fleet-guard", "main.py")
	if _, err := os.Stat(claudeMain); err == nil {
		t.Fatalf("precondition: main.py must not exist")
	}

	opts := &dispatchOpts{taskID: "demo", project: "default"}
	var out bytes.Buffer
	// Run dispatch; expect it to fail (no tmux / FLEET_HOME may not be
	// writable / etc.), but auto-init should run first.
	_ = runDispatch(opts, &out)

	got := out.String()
	if !strings.Contains(got, "first run") {
		t.Errorf("dispatch did not invoke maybeAutoInit; stdout was:\n%s", got)
	}
}
