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

// TestMaybeAutoInit_AlreadyInstalled skips when every embedded file is
// already on disk. Important: the operator who ran `fleet init` once
// shouldn't see a "first run" message every dispatch.
func TestMaybeAutoInit_AlreadyInstalled(t *testing.T) {
	tmp := t.TempDir()
	claudeHome := filepath.Join(tmp, ".claude")

	// Bootstrap a clean install once via runInit so every embedded
	// file lands on disk; then maybeAutoInit should be a no-op.
	if err := runInit(&bytes.Buffer{}, false, claudeHome); err != nil {
		t.Fatalf("setup runInit: %v", err)
	}
	mainPath := filepath.Join(claudeHome, "skills", "fleet-guard", "main.py")

	// Replace main.py contents with a sentinel so we can prove
	// auto-init didn't clobber it.
	const sentinel = "# sentinel — preserved by maybeAutoInit"
	if err := os.WriteFile(mainPath, []byte(sentinel), 0o644); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	var out bytes.Buffer
	maybeAutoInit(&out, claudeHome)

	if got := out.String(); got != "" {
		t.Errorf("expected silence when already installed, got:\n%s", got)
	}

	data, err := os.ReadFile(mainPath)
	if err != nil {
		t.Fatalf("read main.py: %v", err)
	}
	if string(data) != sentinel {
		t.Errorf("auto-init clobbered main.py; got %q", string(data))
	}
}

// TestMaybeAutoInit_HookDriftReinstalls regresses codex iter-2 P2.
// Files all present, but settings.json never had the hook
// registrations (e.g. mergeHookRegistrations crashed in a previous
// run, or the operator hand-edited settings.json and removed them).
// The file-only check used to skip forever; now hooksRegistered
// catches the drift and triggers a re-merge.
func TestMaybeAutoInit_HookDriftReinstalls(t *testing.T) {
	tmp := t.TempDir()
	claudeHome := filepath.Join(tmp, ".claude")

	// Bootstrap files + hooks.
	if err := runInit(&bytes.Buffer{}, false, claudeHome); err != nil {
		t.Fatalf("setup runInit: %v", err)
	}
	// Now wipe settings.json so the hooks are gone but the files
	// remain — exactly the partial-install scenario codex flagged.
	if err := os.Remove(filepath.Join(claudeHome, "settings.json")); err != nil {
		t.Fatalf("remove settings.json: %v", err)
	}

	var out bytes.Buffer
	maybeAutoInit(&out, claudeHome)

	if !strings.Contains(out.String(), "first run") {
		t.Errorf("missing settings.json should trigger re-install, got:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(claudeHome, "settings.json")); err != nil {
		t.Errorf("re-install should restore settings.json: %v", err)
	}
}

// TestMaybeAutoInit_PartialInstallReinstalls is the codex P1
// regression: the previous check "main.py exists → skip" left a
// half-installed skill stuck forever if any sibling file was missing
// (crashed mid-loop, manually deleted, etc.). Now the check walks the
// embedded FS so any missing file triggers a re-install.
func TestMaybeAutoInit_PartialInstallReinstalls(t *testing.T) {
	tmp := t.TempDir()
	claudeHome := filepath.Join(tmp, ".claude")
	skillRoot := filepath.Join(claudeHome, "skills", "fleet-guard")
	if err := os.MkdirAll(skillRoot, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Create JUST main.py — no siblings, no settings.json. This
	// simulates a previous install that crashed after writing the
	// first file.
	if err := os.WriteFile(filepath.Join(skillRoot, "main.py"),
		[]byte("# partial"), 0o644); err != nil {
		t.Fatalf("write partial main.py: %v", err)
	}

	var out bytes.Buffer
	maybeAutoInit(&out, claudeHome)

	got := out.String()
	if !strings.Contains(got, "first run") {
		t.Errorf("partial install should trigger re-install, got:\n%s", got)
	}
	// At least one sibling must now exist on disk.
	handoff := filepath.Join(skillRoot, "handoff.py")
	if _, err := os.Stat(handoff); err != nil {
		t.Errorf("re-install should restore handoff.py: %v", err)
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
