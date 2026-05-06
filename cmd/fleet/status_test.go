package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stubEnsureFresh swaps statusEnsureFreshFn for a no-op so tests
// don't fan out HTTP calls. Restored when the test ends.
func stubEnsureFresh(t *testing.T) {
	t.Helper()
	prev := statusEnsureFreshFn
	statusEnsureFreshFn = func(string, time.Duration) {}
	t.Cleanup(func() { statusEnsureFreshFn = prev })
}

// TestStatus_TriggersEnsureFresh confirms the CLI path calls the
// version refresher with the binary's current version. This is the
// CLI-only fix from codex review: status was unreachable because
// the cache was never populated outside the TUI.
func TestStatus_TriggersEnsureFresh(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FLEET_HOME", dir)

	called := false
	var gotCurrent string
	prev := statusEnsureFreshFn
	statusEnsureFreshFn = func(current string, _ time.Duration) {
		called = true
		gotCurrent = current
	}
	t.Cleanup(func() { statusEnsureFreshFn = prev })

	var buf bytes.Buffer
	if err := runStatus(&statusOpts{}, &buf, "0.1.2"); err != nil {
		t.Fatalf("runStatus: %v", err)
	}
	if !called {
		t.Errorf("expected statusEnsureFreshFn to be called from runStatus")
	}
	if gotCurrent != "0.1.2" {
		t.Errorf("ensureFresh got current=%q, want 0.1.2", gotCurrent)
	}
}

// TestStatus_PrintsUpgradeFooter verifies that when the version cache
// indicates a newer release is available, the bottom of `fleet status`
// output carries the same nudge as the TUI banner.
func TestStatus_PrintsUpgradeFooter(t *testing.T) {
	stubEnsureFresh(t)
	dir := t.TempDir()
	t.Setenv("FLEET_HOME", dir)

	// Seed the version cache with a fake "new release available"
	// state. The status command reads it the same way the TUI does.
	cache := map[string]any{
		"checked_at": time.Now().UTC().Format(time.RFC3339),
		"latest":     "v0.9.9",
		"current":    "0.1.2",
	}
	data, err := json.Marshal(cache)
	if err != nil {
		t.Fatalf("marshal cache: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "version_check.json"), data, 0o644); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	var buf bytes.Buffer
	if err := runStatus(&statusOpts{}, &buf, "0.1.2"); err != nil {
		t.Fatalf("runStatus: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "v0.9.9") || !strings.Contains(out, "brew upgrade fleet") {
		t.Errorf("expected upgrade footer, got:\n%s", out)
	}
}

// TestStatus_NoUpgradeFooter_WhenUpToDate confirms the footer is
// absent when the cache shows no newer version.
func TestStatus_NoUpgradeFooter_WhenUpToDate(t *testing.T) {
	stubEnsureFresh(t)
	dir := t.TempDir()
	t.Setenv("FLEET_HOME", dir)

	cache := map[string]any{
		"checked_at": time.Now().UTC().Format(time.RFC3339),
		"latest":     "v0.1.2",
		"current":    "0.1.2",
	}
	data, _ := json.Marshal(cache)
	if err := os.WriteFile(filepath.Join(dir, "version_check.json"), data, 0o644); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	var buf bytes.Buffer
	if err := runStatus(&statusOpts{}, &buf, "0.1.2"); err != nil {
		t.Fatalf("runStatus: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "brew upgrade fleet") {
		t.Errorf("up-to-date status should not show upgrade footer; got:\n%s", out)
	}
}

// TestStatus_NoUpgradeFooter_OnJSON ensures --json output stays a
// clean machine-parseable record. A trailing nudge line would break
// jq pipelines.
func TestStatus_NoUpgradeFooter_OnJSON(t *testing.T) {
	stubEnsureFresh(t)
	dir := t.TempDir()
	t.Setenv("FLEET_HOME", dir)

	cache := map[string]any{
		"checked_at": time.Now().UTC().Format(time.RFC3339),
		"latest":     "v0.9.9",
		"current":    "0.1.2",
	}
	data, _ := json.Marshal(cache)
	if err := os.WriteFile(filepath.Join(dir, "version_check.json"), data, 0o644); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	var buf bytes.Buffer
	if err := runStatus(&statusOpts{jsonOut: true}, &buf, "0.1.2"); err != nil {
		t.Fatalf("runStatus: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "brew upgrade fleet") {
		t.Errorf("json output should not append nudge; got:\n%s", out)
	}
}
