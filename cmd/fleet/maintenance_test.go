package main

import (
	"bytes"
	"errors"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tmux"
)

// pruneTestNow is the canonical "wall clock" the prune-orphan-tmux
// tests pin against. Pairing this with infos built via staleInfos
// (Created = pruneTestNow - 1h) puts every test session well past the
// 90s freshness window, so the freshness gate doesn't affect the
// behavior these tests pin.
var pruneTestNow = time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)

// staleInfos builds SessionInfo entries with Created set 1h before
// pruneTestNow. Use for any test that wants the existing behavior
// (record-absent => orphan) regardless of the new freshness gate.
func staleInfos(names ...string) []tmux.SessionInfo {
	out := make([]tmux.SessionInfo, 0, len(names))
	for _, n := range names {
		out = append(out, tmux.SessionInfo{
			Name:    n,
			Created: pruneTestNow.Add(-time.Hour),
		})
	}
	return out
}

// stateBootstrapForTest is a thin wrapper for tests that need a
// bootstrapped FLEET_HOME but don't want to import state's full
// surface. Keeps the import block of individual tests narrow.
func stateBootstrapForTest() (string, error) { return state.Bootstrap() }

// pruneDirSaneOK is the no-op dirSaneFn used by every prune test that
// doesn't specifically exercise the agentDirSane precondition (codex
// iter-1 [P2]). Returning nil keeps the sweeper iterating sessions as
// it did before the precondition was wired in.
func pruneDirSaneOK() error { return nil }

// TestMaintenanceBootstrapReport_FlagsLiveAgentsMissingRC pins the
// reporter contract: only LIVE agents (HasSession true) whose
// persisted Command lacks the literal `--remote-control` substring
// land in the report. Dead-session records and already-flagged
// records are filtered. Output format exposes id / project / task /
// spawned-at and a one-line remediation suggestion.
func TestMaintenanceBootstrapReport_FlagsLiveAgentsMissingRC(t *testing.T) {
	rcTestFleetHome(t)
	// Native model: RC is default-on, so the test agent's project is in
	// the enabled bucket with no marker setup at all — the survey
	// distinguishes "RC enabled (default) but missing flag" (real
	// remediation: handoff) from "RC disabled via rc-disabled opt-out"
	// (no action).

	now := time.Date(2026, 5, 9, 13, 20, 42, 0, time.UTC)
	records := []*agent.Record{
		{
			ID:          "ca7eb43e",
			TmuxSession: "fleet-ca7eb43e",
			Project:     "projects-fleet",
			TaskID:      "coord-projects-fleet",
			SpawnedAt:   now,
			Command: []string{
				"sh", "-c",
				`claude --dangerously-skip-permissions; cat`,
			},
		},
		{
			ID:          "deadbeef",
			TmuxSession: "fleet-deadbeef",
			Project:     "other",
			TaskID:      "task-x",
			SpawnedAt:   now.Add(-1 * time.Hour),
			// Already flagged — must NOT appear in report.
			Command: []string{
				"sh", "-c",
				`claude --remote-control "fleet-coord-deadbeef" --dangerously-skip-permissions`,
			},
		},
		{
			ID:          "ghost123",
			TmuxSession: "fleet-ghost123",
			Project:     "stale",
			TaskID:      "task-y",
			SpawnedAt:   now.Add(-2 * time.Hour),
			// Missing flag — but session is dead, so filtered.
			Command: []string{"sh", "-c", `claude --dangerously-skip-permissions`},
		},
	}
	live := map[string]bool{
		"fleet-ca7eb43e": true,
		"fleet-deadbeef": true,
		"fleet-ghost123": false, // dead
	}
	listFn := func() ([]*agent.Record, error) { return records, nil }
	hasSessionFn := func(s string) bool { return live[s] }

	var out bytes.Buffer
	if err := runMaintenanceBootstrapRemoteControl(&out, listFn, hasSessionFn); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := out.String()

	// v0.12 header: distinguishes RC-enabled-but-missing from
	// RC-not-enabled. Test agent is RC-enabled (marker present).
	if !strings.Contains(got, "1 live agent(s) with RC enabled but missing --remote-control") {
		t.Errorf("expected v0.12 RC-enabled-missing header; got:\n%s", got)
	}
	if !strings.Contains(got, "ca7eb43e") {
		t.Errorf("expected ca7eb43e in report; got:\n%s", got)
	}
	if !strings.Contains(got, "project=projects-fleet") {
		t.Errorf("expected project=projects-fleet in report; got:\n%s", got)
	}
	if !strings.Contains(got, "task=coord-projects-fleet") {
		t.Errorf("expected task=coord-projects-fleet in report; got:\n%s", got)
	}
	if !strings.Contains(got, "spawned=2026-05-09T13:20:42Z") {
		t.Errorf("expected spawned=2026-05-09T13:20:42Z in report; got:\n%s", got)
	}
	if !strings.Contains(got, "fleet handoff ca7eb43e") {
		t.Errorf("expected remediation 'fleet handoff ca7eb43e' in report; got:\n%s", got)
	}

	// Already-flagged record must be EXCLUDED.
	if strings.Contains(got, "deadbeef") {
		t.Errorf("already-flagged record deadbeef should be excluded from report; got:\n%s", got)
	}
	// Dead-session record must be EXCLUDED.
	if strings.Contains(got, "ghost123") {
		t.Errorf("dead-session record ghost123 should be excluded from report; got:\n%s", got)
	}
}

// TestMaintenanceBootstrapReport_AllAgentsFlagged_PrintsCleanMessage
// pins the empty-report branch: when every live agent already carries
// the flag, the operator sees a clean confirmation rather than an
// awkward "0 agents" header.
func TestMaintenanceBootstrapReport_AllAgentsFlagged_PrintsCleanMessage(t *testing.T) {
	records := []*agent.Record{
		{
			ID:          "alpha",
			TmuxSession: "fleet-alpha",
			Command: []string{
				"sh", "-c",
				`claude --remote-control "fleet-coord-alpha" --dangerously-skip-permissions`,
			},
		},
	}
	listFn := func() ([]*agent.Record, error) { return records, nil }
	hasSessionFn := func(string) bool { return true }

	var out bytes.Buffer
	if err := runMaintenanceBootstrapRemoteControl(&out, listFn, hasSessionFn); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out.String(), "no live agents are missing") {
		t.Errorf("expected clean 'no live agents are missing' message; got:\n%s", out.String())
	}
}

// TestMaintenanceBootstrapReport_StableOrderByOldestFirst pins the
// sort order: oldest-spawned first (then ID alphabetical for
// tie-breaks). Operators triage the longest-stuck agents first.
func TestMaintenanceBootstrapReport_StableOrderByOldestFirst(t *testing.T) {
	t0 := time.Date(2026, 5, 9, 0, 0, 0, 0, time.UTC)
	records := []*agent.Record{
		{
			ID: "newer", TmuxSession: "s-newer",
			SpawnedAt: t0.Add(2 * time.Hour),
			Command:   []string{"sh", "-c", `claude --dangerously-skip-permissions`},
		},
		{
			ID: "oldest", TmuxSession: "s-oldest",
			SpawnedAt: t0,
			Command:   []string{"sh", "-c", `claude --dangerously-skip-permissions`},
		},
		{
			ID: "middle", TmuxSession: "s-middle",
			SpawnedAt: t0.Add(1 * time.Hour),
			Command:   []string{"sh", "-c", `claude --dangerously-skip-permissions`},
		},
	}
	listFn := func() ([]*agent.Record, error) { return records, nil }
	hasSessionFn := func(string) bool { return true }

	var out bytes.Buffer
	if err := runMaintenanceBootstrapRemoteControl(&out, listFn, hasSessionFn); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := out.String()
	idxOldest := strings.Index(got, "oldest")
	idxMiddle := strings.Index(got, "middle")
	idxNewer := strings.Index(got, "newer")
	if idxOldest == -1 || idxMiddle == -1 || idxNewer == -1 {
		t.Fatalf("all three records should appear in report; got:\n%s", got)
	}
	if idxOldest >= idxMiddle || idxMiddle >= idxNewer {
		t.Errorf("expected oldest-first ordering; got positions oldest=%d middle=%d newer=%d in:\n%s",
			idxOldest, idxMiddle, idxNewer, got)
	}
}

// TestCommandHasRemoteControl pins the substring detector. False
// positives on a wrapper that comments the flag are acceptable —
// reporting "already flagged" by mistake is less annoying than
// reporting an already-flagged agent twice.
func TestCommandHasRemoteControl(t *testing.T) {
	cases := []struct {
		name string
		cmd  []string
		want bool
	}{
		{
			name: "default-wrapper-without-flag",
			cmd: []string{"sh", "-c",
				`claude --dangerously-skip-permissions`},
			want: false,
		},
		{
			name: "wrapper-with-flag",
			cmd: []string{"sh", "-c",
				`claude --remote-control "fleet-coord-x" --dangerously-skip-permissions`},
			want: true,
		},
		{
			name: "direct-argv-with-flag",
			cmd:  []string{"claude", "--remote-control", "fleet-x"},
			want: true,
		},
		{
			name: "empty",
			cmd:  []string{},
			want: false,
		},
		{
			name: "nil",
			cmd:  nil,
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := commandHasRemoteControl(tc.cmd); got != tc.want {
				t.Errorf("commandHasRemoteControl(%v) = %v; want %v", tc.cmd, got, tc.want)
			}
		})
	}
}

// --- prune-orphan-tmux tests
// (fix/orphan-tmux-sweeper-and-leak-plug)

// TestPruneOrphanTmux_DryRunListsOrphansLeavesAlive verifies the
// default-dry-run contract: orphans (no live record) and non-orphans
// (live record) both appear on stdout with the correct flags, killFn
// is NEVER called (dry-run gate), and exit is nil.
func TestPruneOrphanTmux_DryRunListsOrphansLeavesAlive(t *testing.T) {
	// Use agent.NewID()-shaped IDs (8 lowercase hex) so the codex
	// iter-2 [P1] scope guard treats them as agent sessions.
	live := map[string]bool{
		"a11e0001": true,  // "alive1"
		"a11e0002": true,  // "alive2"
		"01fa0001": false, // "orphan1"
		"01fa0002": false, // "orphan2"
	}
	listInfoFn := func() ([]tmux.SessionInfo, error) {
		return staleInfos(
			"fleet-a11e0001",
			"fleet-01fa0002", // intentionally out of alpha order to test sort
			"fleet-01fa0001",
			"fleet-a11e0002",
			"some-other-session", // unrelated, must be ignored
		), nil
	}
	existsFn := func(id string) bool { return live[id] }
	killCalls := 0
	killFn := func(string) error { killCalls++; return nil }

	var stdout, stderr bytes.Buffer
	if err := runMaintenancePruneOrphanTmux(&stdout, &stderr,
		false /* dry-run */, pruneDirSaneOK, listInfoFn, existsFn, killFn,
		func() time.Time { return pruneTestNow }, 90*time.Second); err != nil {
		t.Fatalf("runMaintenancePruneOrphanTmux: %v", err)
	}
	if killCalls != 0 {
		t.Errorf("dry-run must NOT call killFn; got %d calls", killCalls)
	}
	got := stdout.String()
	// Alphabetical order: 01fa0001, 01fa0002, a11e0001, a11e0002.
	expected := []string{
		"fleet-01fa0001  orphan=true  state=missing  killed=false\n",
		"fleet-01fa0002  orphan=true  state=missing  killed=false\n",
		"fleet-a11e0001  orphan=false  state=live  killed=false\n",
		"fleet-a11e0002  orphan=false  state=live  killed=false\n",
	}
	if got != strings.Join(expected, "") {
		t.Errorf("stdout mismatch:\nGOT:\n%s\nWANT:\n%s", got, strings.Join(expected, ""))
	}
	if strings.Contains(got, "some-other-session") {
		t.Errorf("non-fleet sessions should not appear in output; got:\n%s", got)
	}
}

// TestPruneOrphanTmux_KillModeKillsOnlyOrphans verifies --kill: killFn
// is called for every orphan (and only orphans), output reflects
// killed=true on those rows, and live sessions are untouched. This is
// the operator's safety contract for the sweeper.
func TestPruneOrphanTmux_KillModeKillsOnlyOrphans(t *testing.T) {
	live := map[string]bool{
		"a11e000a": true,  // "alivex"
		"01fa000a": false, // "orphana"
		"01fa000b": false, // "orphanb"
	}
	listInfoFn := func() ([]tmux.SessionInfo, error) {
		return staleInfos("fleet-a11e000a", "fleet-01fa000a", "fleet-01fa000b"), nil
	}
	existsFn := func(id string) bool { return live[id] }
	var killed []string
	killFn := func(s string) error { killed = append(killed, s); return nil }

	var stdout, stderr bytes.Buffer
	if err := runMaintenancePruneOrphanTmux(&stdout, &stderr,
		true /* --kill */, pruneDirSaneOK, listInfoFn, existsFn, killFn,
		func() time.Time { return pruneTestNow }, 90*time.Second); err != nil {
		t.Fatalf("runMaintenancePruneOrphanTmux: %v", err)
	}
	// Sort because killFn invocation order depends on listFn order,
	// which is the input order (not alphabetical — alpha order is for
	// stdout). Either way, the SET of killed sessions must be exactly
	// the orphans.
	wantKilled := []string{"fleet-01fa000a", "fleet-01fa000b"}
	sort.Strings(killed)
	sort.Strings(wantKilled)
	if len(killed) != len(wantKilled) {
		t.Fatalf("killed sessions: got %v; want %v", killed, wantKilled)
	}
	for i := range killed {
		if killed[i] != wantKilled[i] {
			t.Errorf("killed[%d]: got %q; want %q", i, killed[i], wantKilled[i])
		}
	}
	got := stdout.String()
	if !strings.Contains(got, "fleet-01fa000a  orphan=true  state=missing  killed=true\n") {
		t.Errorf("expected 01fa000a row with killed=true; got:\n%s", got)
	}
	if !strings.Contains(got, "fleet-01fa000b  orphan=true  state=missing  killed=true\n") {
		t.Errorf("expected 01fa000b row with killed=true; got:\n%s", got)
	}
	if !strings.Contains(got, "fleet-a11e000a  orphan=false  state=live  killed=false\n") {
		t.Errorf("expected a11e000a row with killed=false; got:\n%s", got)
	}
}

// TestPruneOrphanTmux_KillFailureSurfacedAsWarning verifies that a
// killFn error does NOT abort the sweep: the operator can chain this
// into a cron job and trust that one stuck session doesn't block the
// rest. The error message goes to stderr, exit is nil, and the row
// shows killed=false for the failed session.
func TestPruneOrphanTmux_KillFailureSurfacedAsWarning(t *testing.T) {
	listInfoFn := func() ([]tmux.SessionInfo, error) {
		// 8-hex-char IDs for the codex iter-2 [P1] scope guard.
		return staleInfos("fleet-fa11aaaa", "fleet-0ffaa111"), nil
	}
	existsFn := func(string) bool { return false } // both orphans
	killFn := func(s string) error {
		if s == "fleet-fa11aaaa" {
			return errors.New("simulated tmux failure")
		}
		return nil
	}

	var stdout, stderr bytes.Buffer
	if err := runMaintenancePruneOrphanTmux(&stdout, &stderr,
		true, pruneDirSaneOK, listInfoFn, existsFn, killFn,
		func() time.Time { return pruneTestNow }, 90*time.Second); err != nil {
		t.Fatalf("runMaintenancePruneOrphanTmux: %v", err)
	}
	if !strings.Contains(stderr.String(), "warning: kill fleet-fa11aaaa") {
		t.Errorf("stderr should contain kill-failure warning; got: %s", stderr.String())
	}
	if !strings.Contains(stdout.String(), "fleet-fa11aaaa  orphan=true  state=missing  killed=false\n") {
		t.Errorf("stdout should show killed=false for the failed session; got:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "fleet-0ffaa111  orphan=true  state=missing  killed=true\n") {
		t.Errorf("stdout should show killed=true for the successful session; got:\n%s", stdout.String())
	}
}

// TestPruneOrphanTmux_ListErrorSurfacedAsError ensures we don't
// silently swallow a tmux probe failure — if we can't list sessions,
// we exit non-zero so cron / scripts notice instead of treating
// "couldn't even check" as "no orphans".
func TestPruneOrphanTmux_ListErrorSurfacedAsError(t *testing.T) {
	listInfoFn := func() ([]tmux.SessionInfo, error) {
		return nil, errors.New("simulated tmux ls failure")
	}
	existsFn := func(string) bool { return true }
	killFn := func(string) error { return nil }

	var stdout, stderr bytes.Buffer
	err := runMaintenancePruneOrphanTmux(&stdout, &stderr, false,
		pruneDirSaneOK, listInfoFn, existsFn, killFn,
		func() time.Time { return pruneTestNow }, 90*time.Second)
	if err == nil {
		t.Fatalf("expected error when listFn fails; got nil")
	}
	if !strings.Contains(err.Error(), "list tmux sessions") {
		t.Errorf("error should describe the operation; got: %v", err)
	}
}

// TestPruneOrphanTmux_IgnoresNonFleetAndDegeneratePrefixes pins the
// scope guard: sessions named "fleet-" with no ID, or sessions not
// prefixed with "fleet-", are silently dropped. Operator's "other"
// tmux work is left alone.
func TestPruneOrphanTmux_IgnoresNonFleetAndDegeneratePrefixes(t *testing.T) {
	listInfoFn := func() ([]tmux.SessionInfo, error) {
		return staleInfos(
			"fleet-",         // degenerate — no ID
			"unrelated",      // non-fleet
			"work",           // non-fleet
			"fleet-1ea11d00", // genuine orphan (8 hex chars)
		), nil
	}
	existsFn := func(string) bool { return false }
	killCalls := []string{}
	killFn := func(s string) error { killCalls = append(killCalls, s); return nil }

	var stdout, stderr bytes.Buffer
	if err := runMaintenancePruneOrphanTmux(&stdout, &stderr, true,
		pruneDirSaneOK, listInfoFn, existsFn, killFn,
		func() time.Time { return pruneTestNow }, 90*time.Second); err != nil {
		t.Fatalf("runMaintenancePruneOrphanTmux: %v", err)
	}
	// Only the genuine fleet-<id> orphan should be reported / killed.
	got := stdout.String()
	if got != "fleet-1ea11d00  orphan=true  state=missing  killed=true\n" {
		t.Errorf("expected only fleet-1ea11d00 row; got:\n%s", got)
	}
	if len(killCalls) != 1 || killCalls[0] != "fleet-1ea11d00" {
		t.Errorf("expected exactly one kill of fleet-1ea11d00; got %v", killCalls)
	}
}

// TestPruneOrphanTmux_FreshSessionSpared pins the spawn-race regression
// (codex review iter-2 [P1]): a tmux session whose creation time is
// inside the freshness window must NOT be classified as an orphan, even
// when its agent record is absent — that's the legitimate window
// between tmux.Spawn and rec.Write inside spawn.Spawn. Killing it would
// reproduce the original leak shape (live agent torn down).
func TestPruneOrphanTmux_FreshSessionSpared(t *testing.T) {
	listInfoFn := func() ([]tmux.SessionInfo, error) {
		return []tmux.SessionInfo{
			// Fresh: created 5s ago, well inside the 90s window.
			{Name: "fleet-f1e50001", Created: pruneTestNow.Add(-5 * time.Second)},
			// Stale: created 2 hours ago, comfortably past the window.
			{Name: "fleet-5fa10002", Created: pruneTestNow.Add(-2 * time.Hour)},
			// Edge case: zero Created (parse failure) — treat as fresh.
			{Name: "fleet-01dd0003"},
		}, nil
	}
	existsFn := func(string) bool { return false } // all three records missing
	var killed []string
	killFn := func(s string) error { killed = append(killed, s); return nil }

	var stdout, stderr bytes.Buffer
	if err := runMaintenancePruneOrphanTmux(&stdout, &stderr,
		true /* --kill */, pruneDirSaneOK, listInfoFn, existsFn, killFn,
		func() time.Time { return pruneTestNow }, 90*time.Second); err != nil {
		t.Fatalf("runMaintenancePruneOrphanTmux: %v", err)
	}

	// Only the stale session should be killed.
	if len(killed) != 1 || killed[0] != "fleet-5fa10002" {
		t.Fatalf("expected exactly one kill of fleet-5fa10002; got %v", killed)
	}

	got := stdout.String()
	// Fresh sessions: state=fresh, orphan=false, killed=false.
	wantFresh := "fleet-f1e50001  orphan=false  state=fresh  killed=false\n"
	if !strings.Contains(got, wantFresh) {
		t.Errorf("expected fresh row %q; got:\n%s", wantFresh, got)
	}
	// Zero-Created session also treated as fresh (safer than orphan).
	wantUnknown := "fleet-01dd0003  orphan=false  state=fresh  killed=false\n"
	if !strings.Contains(got, wantUnknown) {
		t.Errorf("expected zero-created row classified as fresh %q; got:\n%s", wantUnknown, got)
	}
	// Stale session: state=missing, orphan=true, killed=true.
	wantStale := "fleet-5fa10002  orphan=true  state=missing  killed=true\n"
	if !strings.Contains(got, wantStale) {
		t.Errorf("expected stale row %q; got:\n%s", wantStale, got)
	}
}

// TestPruneOrphanTmux_BoundaryAtFreshness pins the inclusive/exclusive
// behavior at the threshold: a session exactly at freshness counts as
// stale (now.Sub(created) < freshness is FALSE at exactly == freshness).
func TestPruneOrphanTmux_BoundaryAtFreshness(t *testing.T) {
	// Use agent.NewID()-shaped IDs for the codex iter-2 [P1] scope guard.
	// (The "89", "90", "91" suffixes from the previous test names doubled
	// as timing labels — keep that as a comment for readers.)
	listInfoFn := func() ([]tmux.SessionInfo, error) {
		return []tmux.SessionInfo{
			// 89s old: still fresh.
			{Name: "fleet-89000089", Created: pruneTestNow.Add(-89 * time.Second)},
			// 90s old: at boundary → stale.
			{Name: "fleet-90000090", Created: pruneTestNow.Add(-90 * time.Second)},
			// 91s old: stale.
			{Name: "fleet-91000091", Created: pruneTestNow.Add(-91 * time.Second)},
		}, nil
	}
	existsFn := func(string) bool { return false }
	var killed []string
	killFn := func(s string) error { killed = append(killed, s); return nil }

	var stdout, stderr bytes.Buffer
	if err := runMaintenancePruneOrphanTmux(&stdout, &stderr,
		true, pruneDirSaneOK, listInfoFn, existsFn, killFn,
		func() time.Time { return pruneTestNow }, 90*time.Second); err != nil {
		t.Fatalf("runMaintenancePruneOrphanTmux: %v", err)
	}

	sort.Strings(killed)
	want := []string{"fleet-90000090", "fleet-91000091"}
	if len(killed) != len(want) {
		t.Fatalf("killed: got %v; want %v", killed, want)
	}
	for i := range killed {
		if killed[i] != want[i] {
			t.Errorf("killed[%d] = %q; want %q", i, killed[i], want[i])
		}
	}
	got := stdout.String()
	if !strings.Contains(got, "fleet-89000089  orphan=false  state=fresh") {
		t.Errorf("89s row should be fresh; got:\n%s", got)
	}
	if !strings.Contains(got, "fleet-90000090  orphan=true  state=missing") {
		t.Errorf("90s row should be missing/orphan; got:\n%s", got)
	}
}

// TestAgentRecordExists_LiveRecordPresent is the happy path: a freshly
// written agent record is visible to the orphan check.
func TestAgentRecordExists_LiveRecordPresent(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	if _, err := stateBootstrapForTest(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	rec := agent.New("hasone")
	if err := rec.Write(); err != nil {
		t.Fatalf("rec.Write: %v", err)
	}
	if !agentRecordExists("hasone") {
		t.Errorf("agentRecordExists(\"hasone\") = false; want true (record was written)")
	}
}

// TestAgentRecordExists_MissingReturnsFalse is the orphan-detection
// bar: a record that was never written (or was deleted) reports false.
func TestAgentRecordExists_MissingReturnsFalse(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	if _, err := stateBootstrapForTest(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if agentRecordExists("nonexistent") {
		t.Errorf("agentRecordExists(\"nonexistent\") = true; want false (no record on disk)")
	}
}

// -- codex iter-1 [P2]: agentDirSane precondition --------------------------
//
// Without this precondition, if ~/.fleet/agents is missing or not a
// directory (mispointed FLEET_HOME, dotfile rebuild, transient mount
// issue), every agentRecordExists call returns false → every fleet-<id>
// tmux session is classified as orphan → `--kill` mass-terminates
// healthy agents. The precondition fails closed: error out before
// iterating sessions, so nothing gets killed.

// TestAgentDirSane_DirExists_NoError verifies the happy path: after
// Bootstrap creates ~/.fleet/agents/, agentDirSane returns nil. This is
// the precondition that allows the sweeper to iterate sessions.
func TestAgentDirSane_DirExists_NoError(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	if _, err := stateBootstrapForTest(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if err := agentDirSane(); err != nil {
		t.Errorf("agentDirSane() returned error despite bootstrapped agent dir: %v", err)
	}
}

// TestAgentDirSane_DirMissing_Errors verifies the failure path: with
// FLEET_HOME pointed at a temp dir but NO bootstrap call (so
// ~/.fleet/agents doesn't exist), agentDirSane must return a non-nil
// error mentioning the missing dir. This is the regression bar — the
// pre-fix code had no such precondition, so the sweeper would proceed
// and misclassify every session.
func TestAgentDirSane_DirMissing_Errors(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	// Do NOT call Bootstrap — agents/ subdir never created.

	err := agentDirSane()
	if err == nil {
		t.Fatalf("agentDirSane() returned nil despite missing agent dir")
	}
	// Error message must be unambiguous so the operator can grep for it
	// in their cron logs.
	if !strings.Contains(err.Error(), "stat agent dir") {
		t.Errorf("error should describe the operation; got: %v", err)
	}
	if !strings.Contains(err.Error(), "refusing to sweep") {
		t.Errorf("error should explain why sweep aborted; got: %v", err)
	}
}

// TestAgentDirSane_DirIsFile_Errors verifies the not-a-directory failure:
// if something stomped on ~/.fleet/agents and left a regular file in
// its place, agentDirSane must refuse rather than mass-classify
// sessions as orphan.
func TestAgentDirSane_DirIsFile_Errors(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	// Create ~/.fleet (root) but plant a regular file at ~/.fleet/agents
	// instead of a directory.
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	fakeAgentsFile := tmp + "/agents"
	if err := os.WriteFile(fakeAgentsFile, []byte("not a dir"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	err := agentDirSane()
	if err == nil {
		t.Fatalf("agentDirSane() returned nil despite agents/ being a regular file")
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("error should mention 'not a directory'; got: %v", err)
	}
}

// -- codex iter-3 [P1]: freshness scales with FLEET_PID_RESOLVE_S ----------
//
// The original constant pruneOrphanTmuxFreshness = 90s assumed every
// healthy spawn writes its agent record within 90 seconds. But
// spawn.Spawn blocks record creation on resolveEnginePid, whose budget
// is operator-configurable via FLEET_PID_RESOLVE_S (default 10s).
// Operators on slow-spawning wrapper engines who raise that env var
// above 45s would hit the regression: a legitimate replacement sits in
// the `session exists + record missing` state for >90s before its
// record lands, and --kill mass-terminates it.
//
// Fix: pruneOrphanTmuxFreshness() returns
// max(pruneOrphanTmuxMinFreshness, 2 * spawn.PidResolveTimeout()) so
// the window scales with the configured pid-resolve budget.

// TestPruneOrphanTmuxFreshness_DefaultUsesMinFloor: without the env var,
// the freshness window equals the static floor. This is the most
// common case — operators don't tune FLEET_PID_RESOLVE_S.
func TestPruneOrphanTmuxFreshness_DefaultUsesMinFloor(t *testing.T) {
	t.Setenv("FLEET_PID_RESOLVE_S", "")
	got := pruneOrphanTmuxFreshness()
	if got != pruneOrphanTmuxMinFreshness {
		t.Errorf("default freshness = %v; want %v (the static floor)",
			got, pruneOrphanTmuxMinFreshness)
	}
}

// TestPruneOrphanTmuxFreshness_ShortTimeoutKeepsFloor: even if the
// operator sets FLEET_PID_RESOLVE_S to a small value (say 5s), the
// floor still applies — we never go BELOW 90s.
func TestPruneOrphanTmuxFreshness_ShortTimeoutKeepsFloor(t *testing.T) {
	t.Setenv("FLEET_PID_RESOLVE_S", "5")
	got := pruneOrphanTmuxFreshness()
	if got != pruneOrphanTmuxMinFreshness {
		t.Errorf("freshness with FLEET_PID_RESOLVE_S=5 = %v; want %v (floor wins)",
			got, pruneOrphanTmuxMinFreshness)
	}
}

// TestPruneOrphanTmuxFreshness_LargeTimeoutScales: this is the load-
// bearing regression for codex iter-3 [P1]. With FLEET_PID_RESOLVE_S=120,
// the operator has explicitly said "this engine takes up to 120s to
// start." The freshness window MUST be at least 240s (2x) so a healthy
// replacement booting in 120s doesn't get killed.
func TestPruneOrphanTmuxFreshness_LargeTimeoutScales(t *testing.T) {
	t.Setenv("FLEET_PID_RESOLVE_S", "120")
	got := pruneOrphanTmuxFreshness()
	want := 240 * time.Second
	if got != want {
		t.Errorf("freshness with FLEET_PID_RESOLVE_S=120 = %v; want %v (2x scaling)",
			got, want)
	}
	if got <= pruneOrphanTmuxMinFreshness {
		t.Errorf("freshness should EXCEED floor %v when pid-resolve raised; got %v",
			pruneOrphanTmuxMinFreshness, got)
	}
}

// TestPruneOrphanTmux_FreshnessHonorsPidResolveEnv is the end-to-end
// behavior: when FLEET_PID_RESOLVE_S=120 and a session has been alive
// for 100s (record missing), the sweeper must NOT classify it as orphan
// — the operator's wrapper engine could legitimately still be inside the
// pid-resolve wait.
func TestPruneOrphanTmux_FreshnessHonorsPidResolveEnv(t *testing.T) {
	t.Setenv("FLEET_PID_RESOLVE_S", "120") // → freshness window = 240s
	listInfoFn := func() ([]tmux.SessionInfo, error) {
		return []tmux.SessionInfo{
			// 100s old. Below the static 90s floor would have killed
			// it, but with FLEET_PID_RESOLVE_S=120 the floor becomes
			// 240s — this is still inside the spawn window.
			{Name: "fleet-aaaaaaaa", Created: pruneTestNow.Add(-100 * time.Second)},
			// 300s old: definitely past the dynamic 240s window.
			{Name: "fleet-bbbbbbbb", Created: pruneTestNow.Add(-300 * time.Second)},
		}, nil
	}
	existsFn := func(string) bool { return false }
	var killed []string
	killFn := func(s string) error { killed = append(killed, s); return nil }

	var stdout, stderr bytes.Buffer
	if err := runMaintenancePruneOrphanTmux(&stdout, &stderr,
		true /* --kill */, pruneDirSaneOK, listInfoFn, existsFn, killFn,
		func() time.Time { return pruneTestNow },
		pruneOrphanTmuxFreshness()); err != nil {
		t.Fatalf("runMaintenancePruneOrphanTmux: %v", err)
	}

	// Only the 300s-old session is past the dynamic window.
	if len(killed) != 1 || killed[0] != "fleet-bbbbbbbb" {
		t.Fatalf("expected exactly one kill of fleet-bbbbbbbb (the past-240s session); got %v",
			killed)
	}
	// 100s-old session must be classified as fresh, not orphan.
	got := stdout.String()
	if !strings.Contains(got, "fleet-aaaaaaaa  orphan=false  state=fresh") {
		t.Errorf("100s-old session should be fresh (FLEET_PID_RESOLVE_S=120 → window=240s); got:\n%s", got)
	}
}

// -- codex iter-2 [P1]: fleet-prefix scope guard --------------------------
//
// Before the regex gate, any tmux session whose name started with
// "fleet-" was a candidate for orphan classification — including
// operator-named sessions like `fleet-debug` or `fleet-prod-shell`
// that aren't actually fleet agents. With `--kill`, those got
// mass-terminated. The fix restricts the sweeper to sessions whose
// suffix matches agent.NewID()'s shape (8 hex chars, or t+7 hex
// fallback).

// TestPruneOrphanTmux_IgnoresNonAgentSuffixes is the load-bearing
// regression test for codex iter-2 [P1]. Operators routinely name
// unrelated sessions with the "fleet-" prefix. The sweeper must
// recognize that "fleet-debug", "fleet-prod-shell", "fleet-Notes",
// and other non-agent suffixes are NOT agent sessions, even when
// older than the freshness window and "record absent" via existsFn.
// No killFn calls allowed for those rows.
func TestPruneOrphanTmux_IgnoresNonAgentSuffixes(t *testing.T) {
	listInfoFn := func() ([]tmux.SessionInfo, error) {
		return staleInfos(
			// Real agent ID shape — must be killed when classified as orphan.
			"fleet-a1b2c3d4",
			// Fallback ID shape (crypto/rand failure path) — also killable.
			"fleet-t1a2b3c4",
			// Operator's unrelated sessions — must be SKIPPED, even old.
			"fleet-debug",
			"fleet-prod-shell",
			"fleet-Notes",             // uppercase, never an agent ID
			"fleet-a1b2c3d",           // 7 hex chars (short by one)
			"fleet-a1b2c3d4e",         // 9 hex chars (long by one)
			"fleet-A1B2C3D4",          // uppercase hex — agent.NewID is lowercase
			"fleet-coord-handoff-foo", // structured non-agent name
			"fleet-zxyq3210",          // contains non-hex digits
		), nil
	}
	existsFn := func(string) bool { return false } // every record "missing"
	var killed []string
	killFn := func(s string) error { killed = append(killed, s); return nil }

	var stdout, stderr bytes.Buffer
	if err := runMaintenancePruneOrphanTmux(&stdout, &stderr,
		true /* --kill */, pruneDirSaneOK, listInfoFn, existsFn, killFn,
		func() time.Time { return pruneTestNow }, 90*time.Second); err != nil {
		t.Fatalf("runMaintenancePruneOrphanTmux: %v", err)
	}

	// Only the two real agent-ID-shaped sessions should be killed.
	sort.Strings(killed)
	want := []string{"fleet-a1b2c3d4", "fleet-t1a2b3c4"}
	if len(killed) != len(want) {
		t.Fatalf("killed: got %v; want %v (non-agent suffixes must be skipped)", killed, want)
	}
	for i := range killed {
		if killed[i] != want[i] {
			t.Errorf("killed[%d] = %q; want %q", i, killed[i], want[i])
		}
	}

	// None of the operator's unrelated sessions should appear in stdout
	// either — they're skipped entirely (matching the existing
	// "ignored non-fleet-prefix" behavior).
	got := stdout.String()
	forbidden := []string{
		"fleet-debug",
		"fleet-prod-shell",
		"fleet-Notes",
		"fleet-coord-handoff-foo",
		"fleet-A1B2C3D4",
	}
	for _, name := range forbidden {
		if strings.Contains(got, name) {
			t.Errorf("operator's unrelated session %q appeared in output (must be silently skipped); got:\n%s",
				name, got)
		}
	}
}

// TestFleetAgentIDPattern pins the regex behavior directly so future
// edits to the pattern can't accidentally widen or narrow the scope
// without explicit test updates.
func TestFleetAgentIDPattern(t *testing.T) {
	tests := []struct {
		name string
		id   string
		want bool
	}{
		// Real agent.NewID() outputs — must match.
		{"happy 8 hex", "a1b2c3d4", true},
		{"happy 8 hex zeros", "00000000", true},
		{"happy 8 hex max", "ffffffff", true},
		// Crypto/rand fallback shape — must match BOTH t+7hex
		// (value <= 0x0fffffff) and t+8hex (value > 0x0fffffff,
		// the more common case — %07x is min-width, not max).
		{"fallback t+7hex", "t1a2b3c4", true},
		{"fallback t+7hex zeros", "t0000000", true},
		{"fallback t+8hex", "tffffffff", true}, // codex iter-5 [P3]
		{"fallback t+8hex typical", "t89abcdef", true},
		// Wrong length — must NOT match.
		{"7 hex short", "a1b2c3d", false},
		{"9 hex long", "a1b2c3d4e", false},
		{"empty", "", false},
		// Wrong case — agent IDs are lowercase hex only.
		{"uppercase hex", "A1B2C3D4", false},
		{"mixed case hex", "a1B2c3D4", false},
		// Wrong characters — must NOT match.
		{"contains g (not hex)", "g1b2c3d4", false},
		{"contains z (not hex)", "z1234567", false},
		// Operator's unrelated session suffixes — must NOT match.
		{"plain word", "debug", false},
		{"hyphenated", "prod-shell", false},
		{"with dot", "foo.bar01", false},
		// Fallback shape variants that should NOT match.
		{"t+9hex (too long)", "t1a2b3c4de", false},
		{"t+7nonhex", "tzzzzzzz", false},
		{"T uppercase prefix", "T1234567", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := fleetAgentIDPattern.MatchString(tc.id)
			if got != tc.want {
				t.Errorf("fleetAgentIDPattern.MatchString(%q) = %t; want %t", tc.id, got, tc.want)
			}
		})
	}
}

// TestPruneOrphanTmux_AbortsWhenAgentsDirMissing is the load-bearing
// regression test for codex iter-1 [P2]: when the precondition fails,
// runMaintenancePruneOrphanTmux must error out IMMEDIATELY without
// iterating sessions, without calling listInfoFn, without calling
// existsFn, and (critically) without calling killFn. This protects
// healthy live agents from being mass-killed by a sweeper that doesn't
// know its state root went missing.
func TestPruneOrphanTmux_AbortsWhenAgentsDirMissing(t *testing.T) {
	// Failing dirSaneFn simulates the post-`rm -rf ~/.fleet/agents`
	// state (or any other reason the state root went missing).
	wantDirErr := errors.New("simulated: ~/.fleet/agents went missing")
	dirSaneFn := func() error { return wantDirErr }

	// These must NOT be called when the precondition fails.
	var listCalls, existsCalls, killCalls int
	listInfoFn := func() ([]tmux.SessionInfo, error) {
		listCalls++
		return staleInfos("fleet-1c1ca110"), nil
	}
	existsFn := func(string) bool { existsCalls++; return false }
	killFn := func(string) error { killCalls++; return nil }

	var stdout, stderr bytes.Buffer
	err := runMaintenancePruneOrphanTmux(&stdout, &stderr,
		true /* --kill */, dirSaneFn, listInfoFn, existsFn, killFn,
		func() time.Time { return pruneTestNow }, 90*time.Second)
	if err == nil {
		t.Fatalf("expected error when agent dir is missing; got nil")
	}
	if !strings.Contains(err.Error(), "agent dir sanity check") {
		t.Errorf("error should describe the operation; got: %v", err)
	}
	if !errors.Is(err, wantDirErr) {
		t.Errorf("error should wrap the underlying sanity-check error; got: %v", err)
	}
	// Regression bar: NOTHING after the precondition runs.
	if listCalls != 0 {
		t.Errorf("listInfoFn called %d times after sanity-check failure; want 0", listCalls)
	}
	if existsCalls != 0 {
		t.Errorf("existsFn called %d times after sanity-check failure; want 0", existsCalls)
	}
	if killCalls != 0 {
		t.Errorf("killFn called %d times after sanity-check failure; want 0 (this is the mass-kill regression bar)", killCalls)
	}
	// No output rows either.
	if stdout.Len() != 0 {
		t.Errorf("stdout should be empty after sanity-check failure; got: %q", stdout.String())
	}
}
