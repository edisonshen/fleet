package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// errProbeFail is a sentinel returned by the injected listFn in
// TestStatus_SessionCapBanner_AbsentOnProbeFailure.
var errProbeFail = errors.New("tmux: command not found")

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

	var buf, stderr bytes.Buffer
	if err := runStatus(&statusOpts{}, &buf, &stderr, "0.1.2"); err != nil {
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

	var buf, stderr bytes.Buffer
	if err := runStatus(&statusOpts{}, &buf, &stderr, "0.1.2"); err != nil {
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

	var buf, stderr bytes.Buffer
	if err := runStatus(&statusOpts{}, &buf, &stderr, "0.1.2"); err != nil {
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

	var buf, stderr bytes.Buffer
	if err := runStatus(&statusOpts{jsonOut: true}, &buf, &stderr, "0.1.2"); err != nil {
		t.Fatalf("runStatus: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "brew upgrade fleet") {
		t.Errorf("json output should not append nudge; got:\n%s", out)
	}
}

// withInjectedStatusSessionFns swaps the package-level
// statusSessionListFn / statusSessionExistsFn for the test's duration.
// Mirrors withInjectedSessionFns in session_cap_test.go.
func withInjectedStatusSessionFns(t *testing.T,
	listFn func() ([]string, error),
	existsFn func(id string) bool,
) {
	t.Helper()
	prevList := statusSessionListFn
	prevExists := statusSessionExistsFn
	statusSessionListFn = listFn
	statusSessionExistsFn = existsFn
	t.Cleanup(func() {
		statusSessionListFn = prevList
		statusSessionExistsFn = prevExists
	})
}

// TestStatus_SessionCapBanner_BelowThreshold pins the no-banner case
// (count < 80% of cap) so noise stays absent during normal operation.
func TestStatus_SessionCapBanner_BelowThreshold(t *testing.T) {
	stubEnsureFresh(t)
	dir := t.TempDir()
	t.Setenv("FLEET_HOME", dir)
	t.Setenv("FLEET_MAX_SESSIONS", "10")

	// 7/10 = 70% — below 80% threshold, silent.
	sessions := []string{
		"fleet-a", "fleet-b", "fleet-c", "fleet-d",
		"fleet-e", "fleet-f", "fleet-g",
	}
	withInjectedStatusSessionFns(t,
		func() ([]string, error) { return sessions, nil },
		func(string) bool { return true },
	)

	var stdout, stderr bytes.Buffer
	if err := runStatus(&statusOpts{}, &stdout, &stderr, "dev"); err != nil {
		t.Fatalf("runStatus: %v", err)
	}
	if strings.Contains(stderr.String(), "WARNING") {
		t.Errorf("70%% should be silent; stderr:\n%s", stderr.String())
	}
}

// TestStatus_SessionCapBanner_AtWarnThreshold pins the warning-tier
// case (count == 80% of cap) — banner present with the yellow style.
func TestStatus_SessionCapBanner_AtWarnThreshold(t *testing.T) {
	stubEnsureFresh(t)
	dir := t.TempDir()
	t.Setenv("FLEET_HOME", dir)
	t.Setenv("FLEET_MAX_SESSIONS", "10")

	// 8/10 = 80% — at threshold, warning emitted. 5 live + 3 orphan.
	sessions := []string{
		"fleet-a", "fleet-b", "fleet-c", "fleet-d",
		"fleet-e", "fleet-f", "fleet-g", "fleet-h",
	}
	live := map[string]bool{"a": true, "b": true, "c": true, "d": true, "e": true}
	withInjectedStatusSessionFns(t,
		func() ([]string, error) { return sessions, nil },
		func(id string) bool { return live[id] },
	)

	var stdout, stderr bytes.Buffer
	if err := runStatus(&statusOpts{}, &stdout, &stderr, "dev"); err != nil {
		t.Fatalf("runStatus: %v", err)
	}
	body := stderr.String()
	if !strings.Contains(body, "WARNING") {
		t.Errorf("80%% should emit banner; stderr:\n%s", body)
	}
	if !strings.Contains(body, "8/10") {
		t.Errorf("banner should show count/max; stderr:\n%s", body)
	}
	if !strings.Contains(body, "5 live") || !strings.Contains(body, "3 orphan") {
		t.Errorf("banner should show live/orphan breakdown; stderr:\n%s", body)
	}
	if !strings.Contains(body, "prune-orphan-tmux") {
		t.Errorf("banner should point at prune command; stderr:\n%s", body)
	}
	// Yellow/warning style (not red).
	if !strings.Contains(body, "\x1b[1;33m") {
		t.Errorf("at-threshold should use yellow ANSI; stderr:\n%s", body)
	}
	if strings.Contains(body, "\x1b[1;31m") {
		t.Errorf("at-threshold should NOT use red ANSI; stderr:\n%s", body)
	}
}

// TestStatus_SessionCapBanner_AtCap pins the critical-tier case
// (count >= cap) — banner uses red ANSI.
func TestStatus_SessionCapBanner_AtCap(t *testing.T) {
	stubEnsureFresh(t)
	dir := t.TempDir()
	t.Setenv("FLEET_HOME", dir)
	t.Setenv("FLEET_MAX_SESSIONS", "5")

	// 5/5 = 100%, all orphan.
	sessions := []string{"fleet-a", "fleet-b", "fleet-c", "fleet-d", "fleet-e"}
	withInjectedStatusSessionFns(t,
		func() ([]string, error) { return sessions, nil },
		func(string) bool { return false },
	)

	var stdout, stderr bytes.Buffer
	if err := runStatus(&statusOpts{}, &stdout, &stderr, "dev"); err != nil {
		t.Fatalf("runStatus: %v", err)
	}
	body := stderr.String()
	if !strings.Contains(body, "WARNING") {
		t.Errorf("100%% should emit banner; stderr:\n%s", body)
	}
	if !strings.Contains(body, "5/5") {
		t.Errorf("banner should show 5/5; stderr:\n%s", body)
	}
	if !strings.Contains(body, "0 live") || !strings.Contains(body, "5 orphan") {
		t.Errorf("banner should show all-orphan breakdown; stderr:\n%s", body)
	}
	// Red/critical style.
	if !strings.Contains(body, "\x1b[1;31m") {
		t.Errorf("at-cap should use red ANSI; stderr:\n%s", body)
	}
}

// TestStatus_SessionCapBanner_AbsentOnProbeFailure regresses the
// "transient tmux unreachable" case. The status command must not
// spam the operator on every run when tmux is briefly missing.
func TestStatus_SessionCapBanner_AbsentOnProbeFailure(t *testing.T) {
	stubEnsureFresh(t)
	dir := t.TempDir()
	t.Setenv("FLEET_HOME", dir)
	t.Setenv("FLEET_MAX_SESSIONS", "5")

	withInjectedStatusSessionFns(t,
		func() ([]string, error) {
			return nil, errProbeFail
		},
		func(string) bool { return true },
	)

	var stdout, stderr bytes.Buffer
	if err := runStatus(&statusOpts{}, &stdout, &stderr, "dev"); err != nil {
		t.Fatalf("runStatus: %v", err)
	}
	if strings.Contains(stderr.String(), "WARNING") {
		t.Errorf("probe failure should not emit banner; stderr:\n%s",
			stderr.String())
	}
}
