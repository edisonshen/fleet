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

	"github.com/edisonshen/fleet/internal/gc"
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
	// ANSI styling verified separately in TestPaintBanner — the
	// runStatus integration here uses bytes.Buffer (non-TTY) so the
	// emitter intentionally omits color codes (codex iter-6 P2).
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
	// ANSI styling verified in TestPaintBanner — non-TTY stderr
	// here doesn't include color codes (codex iter-6 P2).
}

// TestStatus_SessionCapBanner_SmallCapThresholdFaithful regresses
// codex iter-1 P2: with `(max * 80) / 100` integer-division, the
// banner fired too early for small caps (max=1 → threshold=0 →
// always warn; max=4 → threshold=3 → warn at 75%, not 80%).
// The fix cross-multiplies (`total*100 >= max*80`) so the
// advertised 80% holds for every cap.
func TestStatus_SessionCapBanner_SmallCapThresholdFaithful(t *testing.T) {
	stubEnsureFresh(t)
	dir := t.TempDir()
	t.Setenv("FLEET_HOME", dir)

	t.Run("max=1, zero sessions: silent", func(t *testing.T) {
		t.Setenv("FLEET_MAX_SESSIONS", "1")
		withInjectedStatusSessionFns(t,
			func() ([]string, error) { return nil, nil },
			func(string) bool { return true },
		)
		var stdout, stderr bytes.Buffer
		if err := runStatus(&statusOpts{}, &stdout, &stderr, "dev"); err != nil {
			t.Fatalf("runStatus: %v", err)
		}
		if strings.Contains(stderr.String(), "WARNING") {
			t.Errorf("max=1, 0 sessions should be silent; stderr:\n%s",
				stderr.String())
		}
	})

	t.Run("max=4, three sessions (75%): silent", func(t *testing.T) {
		t.Setenv("FLEET_MAX_SESSIONS", "4")
		withInjectedStatusSessionFns(t,
			func() ([]string, error) {
				return []string{"fleet-a", "fleet-b", "fleet-c"}, nil
			},
			func(string) bool { return true },
		)
		var stdout, stderr bytes.Buffer
		if err := runStatus(&statusOpts{}, &stdout, &stderr, "dev"); err != nil {
			t.Fatalf("runStatus: %v", err)
		}
		if strings.Contains(stderr.String(), "WARNING") {
			t.Errorf("max=4, 3 sessions (75%%) should be silent; stderr:\n%s",
				stderr.String())
		}
	})

	t.Run("max=4, four sessions (100%): banner present", func(t *testing.T) {
		t.Setenv("FLEET_MAX_SESSIONS", "4")
		withInjectedStatusSessionFns(t,
			func() ([]string, error) {
				return []string{"fleet-a", "fleet-b", "fleet-c", "fleet-d"}, nil
			},
			func(string) bool { return true },
		)
		var stdout, stderr bytes.Buffer
		if err := runStatus(&statusOpts{}, &stdout, &stderr, "dev"); err != nil {
			t.Fatalf("runStatus: %v", err)
		}
		body := stderr.String()
		if !strings.Contains(body, "WARNING") {
			t.Errorf("max=4, 4 sessions (100%%) should warn; stderr:\n%s", body)
		}
		// ANSI styling verified in TestPaintBanner.
	})
}

// TestStatus_SessionCapBanner_OnJSONPath regresses codex iter-3 P2:
// the banner used to be skipped on `fleet status --json` because the
// JSON branch returned from enc.Encode before reaching the banner
// call. Scripted/monitoring callers piping stdout through jq still
// need the cap warning on stderr.
func TestStatus_SessionCapBanner_OnJSONPath(t *testing.T) {
	stubEnsureFresh(t)
	dir := t.TempDir()
	t.Setenv("FLEET_HOME", dir)
	t.Setenv("FLEET_MAX_SESSIONS", "5")

	// 5/5 = 100% — banner must fire.
	sessions := []string{"fleet-a", "fleet-b", "fleet-c", "fleet-d", "fleet-e"}
	withInjectedStatusSessionFns(t,
		func() ([]string, error) { return sessions, nil },
		func(string) bool { return false },
	)

	var stdout, stderr bytes.Buffer
	if err := runStatus(&statusOpts{jsonOut: true}, &stdout, &stderr, "dev"); err != nil {
		t.Fatalf("runStatus: %v", err)
	}
	if !strings.Contains(stderr.String(), "WARNING") {
		t.Errorf("--json path must still emit cap banner on stderr; stderr:\n%s",
			stderr.String())
	}
	// stdout must remain valid JSON — banner went to stderr.
	if !strings.HasPrefix(strings.TrimSpace(stdout.String()), "[") {
		t.Errorf("--json stdout should be a JSON array, got: %s", stdout.String())
	}
}

// TestPaintBanner pins the ANSI escape codes for the warning and
// critical tiers. The actual emit path in runStatus gates this on
// TTY detection (codex iter-6 P2) so non-terminal stderr stays
// plain — this test exercises the pure styling function directly.
func TestPaintBanner(t *testing.T) {
	line := "x\n"
	t.Run("warning style: bold yellow", func(t *testing.T) {
		got := paintBanner(line, bannerStyleWarning)
		if !strings.Contains(got, "\x1b[1;33m") {
			t.Errorf("warning should use bold yellow; got %q", got)
		}
		if !strings.HasSuffix(got, "\x1b[0m") {
			t.Errorf("output should end with reset; got %q", got)
		}
	})
	t.Run("critical style: bold red", func(t *testing.T) {
		got := paintBanner(line, bannerStyleCritical)
		if !strings.Contains(got, "\x1b[1;31m") {
			t.Errorf("critical should use bold red; got %q", got)
		}
		if !strings.HasSuffix(got, "\x1b[0m") {
			t.Errorf("output should end with reset; got %q", got)
		}
	})
}

// TestStderrIsTerminal pins the TTY-detection helper used to gate
// ANSI emission. bytes.Buffer is not a *os.File, so it always
// returns false — which is what tests rely on to assert
// uncolored banner output.
func TestStderrIsTerminal(t *testing.T) {
	if stderrIsTerminal(&bytes.Buffer{}) {
		t.Errorf("bytes.Buffer must not register as a terminal")
	}
	// os.DevNull is a real *os.File but is NOT a terminal — must
	// also return false so redirected runs stay plain.
	dn, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open /dev/null: %v", err)
	}
	defer func() { _ = dn.Close() }()
	if stderrIsTerminal(dn) {
		t.Errorf("/dev/null must not register as a terminal")
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

// ----------------- fleet#165 PR-D: auto-reconciliation --------------
//
// runStatus calls gc.Reconcile(Apply=false, Kinds=all) AFTER the
// agent table renders. Any actions returned are surfaced under an
// "Orphans (operator action)" section with a per-kind cleanup
// one-liner. Empty report → section omitted (no noise on healthy
// state). Reconcile errors → logged to stderr, status still exits 0
// (informational view — should never block triage).
//
// The tests below pin the wiring at the cmd/fleet seam
// (statusReconcileFn package var). End-to-end probe behavior is
// covered in internal/gc/gc_test.go.

// stubStatusReconcile swaps statusReconcileFn for the test's duration,
// returning a canned report+error and recording the Options it saw.
// Restores the production wiring via t.Cleanup.
func stubStatusReconcile(t *testing.T, report gc.Report, err error) *gc.Options {
	t.Helper()
	captured := &gc.Options{}
	prev := statusReconcileFn
	statusReconcileFn = func(opts gc.Options) (gc.Report, error) {
		*captured = opts
		return report, err
	}
	t.Cleanup(func() { statusReconcileFn = prev })
	return captured
}

// TestStatus_SurfacesOrphanAgentRecord pins the orphan-agents surface:
// when reconcile returns a would-archive action, runStatus must print
// it under "Orphans (operator action)" with the cleanup command. Exit
// code is implicit — runStatus returning nil error.
func TestStatus_SurfacesOrphanAgentRecord(t *testing.T) {
	stubEnsureFresh(t)
	dir := t.TempDir()
	t.Setenv("FLEET_HOME", dir)

	report := gc.Report{Actions: []gc.Action{
		{Kind: gc.KindOrphanAgents, Target: "deadbeef", Verb: gc.VerbWouldArchive, Reason: "tmux session fleet-deadbeef gone"},
	}}
	stubStatusReconcile(t, report, nil)

	var stdout, stderr bytes.Buffer
	if err := runStatus(&statusOpts{}, &stdout, &stderr, "dev"); err != nil {
		t.Fatalf("runStatus: %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "Orphans") {
		t.Errorf("stdout should surface an Orphans section; got:\n%s", out)
	}
	if !strings.Contains(out, "deadbeef") {
		t.Errorf("stdout should name the orphan agent ID; got:\n%s", out)
	}
	// The one-liner must point at the canonical cleanup command so
	// the operator can copy-paste it.
	if !strings.Contains(out, "fleet gc") {
		t.Errorf("orphan-agents row should suggest `fleet gc`; got:\n%s", out)
	}
}

// TestStatus_SurfacesOrphanTmuxSession pins the orphan-tmux surface:
// surface action prints with the `tmux kill-session -t ...` one-liner.
// The session is NEVER auto-killed from `fleet status`
// (feedback_surface_dont_silo + feedback_user_owns_tmux_config).
func TestStatus_SurfacesOrphanTmuxSession(t *testing.T) {
	stubEnsureFresh(t)
	dir := t.TempDir()
	t.Setenv("FLEET_HOME", dir)

	report := gc.Report{Actions: []gc.Action{
		{Kind: gc.KindOrphanTmux, Target: "fleet-c0ffee01", Verb: gc.VerbSurface, Reason: "no agent record"},
	}}
	stubStatusReconcile(t, report, nil)

	var stdout, stderr bytes.Buffer
	if err := runStatus(&statusOpts{}, &stdout, &stderr, "dev"); err != nil {
		t.Fatalf("runStatus: %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "Orphans") {
		t.Errorf("stdout should surface an Orphans section; got:\n%s", out)
	}
	if !strings.Contains(out, "fleet-c0ffee01") {
		t.Errorf("stdout should name the orphan tmux session; got:\n%s", out)
	}
	if !strings.Contains(out, "tmux kill-session -t fleet-c0ffee01") {
		t.Errorf("orphan-tmux row should suggest the kill-session one-liner; got:\n%s", out)
	}
}

// TestStatus_HealthyState_NoOrphanSection pins the no-noise contract:
// empty reconcile report → no Orphans section in stdout.
func TestStatus_HealthyState_NoOrphanSection(t *testing.T) {
	stubEnsureFresh(t)
	dir := t.TempDir()
	t.Setenv("FLEET_HOME", dir)

	stubStatusReconcile(t, gc.Report{}, nil)

	var stdout, stderr bytes.Buffer
	if err := runStatus(&statusOpts{}, &stdout, &stderr, "dev"); err != nil {
		t.Fatalf("runStatus: %v", err)
	}
	out := stdout.String()
	for _, fragment := range []string{"Orphans", "would-archive", "kill-session"} {
		if strings.Contains(out, fragment) {
			t.Errorf("healthy state should not surface %q on stdout; got:\n%s", fragment, out)
		}
	}
}

// TestStatus_ReconcilePassesAllKinds pins the call shape: status
// surfaces ALL four kinds (sockets, orphan-agents, orphan-tmux,
// worktrees), so the reconcile Options must opt into all of them and
// run dry-run (Apply=false).
func TestStatus_ReconcilePassesAllKinds(t *testing.T) {
	stubEnsureFresh(t)
	dir := t.TempDir()
	t.Setenv("FLEET_HOME", dir)
	opts := stubStatusReconcile(t, gc.Report{}, nil)

	var stdout, stderr bytes.Buffer
	if err := runStatus(&statusOpts{}, &stdout, &stderr, "dev"); err != nil {
		t.Fatalf("runStatus: %v", err)
	}
	if opts.Apply {
		t.Errorf("status reconcile must be dry-run (Apply=false); got Apply=%t", opts.Apply)
	}
	// Must include all four kinds so the operator sees a complete
	// picture of orphan state.
	wantKinds := map[gc.Kind]bool{
		gc.KindSockets: true, gc.KindOrphanAgents: true,
		gc.KindOrphanTmux: true, gc.KindWorktrees: true,
	}
	got := map[gc.Kind]bool{}
	for _, k := range opts.Kinds {
		got[k] = true
	}
	for k := range wantKinds {
		if !got[k] {
			t.Errorf("status reconcile missing kind %q; opts.Kinds=%v", k, opts.Kinds)
		}
	}
}

// TestStatus_ReconcileError_LoggedNotReturned pins the
// surface-don't-silo contract: a reconcile error must reach stderr
// AND status must still exit 0 (informational view). Returning err
// would surface as a non-zero exit, but `fleet status` is a
// read-only triage command — a reconcile probe failure shouldn't
// stop the operator from seeing the agent table.
func TestStatus_ReconcileError_LoggedNotReturned(t *testing.T) {
	stubEnsureFresh(t)
	dir := t.TempDir()
	t.Setenv("FLEET_HOME", dir)
	bad := errors.New("reconcile probe failure: stat agent dir: permission denied")
	stubStatusReconcile(t, gc.Report{}, bad)

	var stdout, stderr bytes.Buffer
	if err := runStatus(&statusOpts{}, &stdout, &stderr, "dev"); err != nil {
		t.Errorf("reconcile error must NOT propagate as runStatus return; got %v", err)
	}
	if !strings.Contains(stderr.String(), "reconcile") {
		t.Errorf("reconcile error must be logged to stderr; got:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "permission denied") {
		t.Errorf("reconcile error message must be preserved on stderr; got:\n%s", stderr.String())
	}
}

// TestStatusReconcileFn_DefaultsToGCReconcile pins the default wiring:
// the seam must call into gc.Reconcile with production Deps when no
// test stub is installed. Without this, a refactor that nils the var
// would silently degrade status to "no reconcile ever fires" without
// any test failing.
func TestStatusReconcileFn_DefaultsToGCReconcile(t *testing.T) {
	if statusReconcileFn == nil {
		t.Fatal("statusReconcileFn must default to a non-nil func wrapping gc.Reconcile")
	}
	// Empty kinds → empty report (gc.Reconcile treats nil Kinds as
	// "do nothing"). Proves the var is callable + routes through gc
	// without side effects.
	rep, err := statusReconcileFn(gc.Options{Kinds: nil})
	if err != nil {
		t.Fatalf("default statusReconcileFn with empty kinds returned err: %v", err)
	}
	if len(rep.Actions) != 0 {
		t.Errorf("empty kinds should yield empty report; got %d actions", len(rep.Actions))
	}
}
