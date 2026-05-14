package main

import (
	"bytes"
	"errors"
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

// TestMaintenanceBootstrapReport_FlagsLiveAgentsMissingRC pins the
// reporter contract: only LIVE agents (HasSession true) whose
// persisted Command lacks the literal `--remote-control` substring
// land in the report. Dead-session records and already-flagged
// records are filtered. Output format exposes id / project / task /
// spawned-at and a one-line remediation suggestion.
func TestMaintenanceBootstrapReport_FlagsLiveAgentsMissingRC(t *testing.T) {
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

	// Header counts only the live + missing record (ca7eb43e).
	if !strings.Contains(got, "1 live agent(s) missing --remote-control") {
		t.Errorf("expected '1 live agent(s) missing --remote-control' header; got:\n%s", got)
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
	live := map[string]bool{
		"alive1":  true,
		"alive2":  true,
		"orphan1": false,
		"orphan2": false,
	}
	listInfoFn := func() ([]tmux.SessionInfo, error) {
		return staleInfos(
			"fleet-alive1",
			"fleet-orphan2", // intentionally out of alpha order to test sort
			"fleet-orphan1",
			"fleet-alive2",
			"some-other-session", // unrelated, must be ignored
		), nil
	}
	existsFn := func(id string) bool { return live[id] }
	killCalls := 0
	killFn := func(string) error { killCalls++; return nil }

	var stdout, stderr bytes.Buffer
	if err := runMaintenancePruneOrphanTmux(&stdout, &stderr,
		false /* dry-run */, listInfoFn, existsFn, killFn,
		func() time.Time { return pruneTestNow }, 90*time.Second); err != nil {
		t.Fatalf("runMaintenancePruneOrphanTmux: %v", err)
	}
	if killCalls != 0 {
		t.Errorf("dry-run must NOT call killFn; got %d calls", killCalls)
	}
	got := stdout.String()
	// Alphabetical order: alive1, alive2, orphan1, orphan2.
	expected := []string{
		"fleet-alive1  orphan=false  state=live  killed=false\n",
		"fleet-alive2  orphan=false  state=live  killed=false\n",
		"fleet-orphan1  orphan=true  state=missing  killed=false\n",
		"fleet-orphan2  orphan=true  state=missing  killed=false\n",
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
		"alivex":  true,
		"orphana": false,
		"orphanb": false,
	}
	listInfoFn := func() ([]tmux.SessionInfo, error) {
		return staleInfos("fleet-alivex", "fleet-orphana", "fleet-orphanb"), nil
	}
	existsFn := func(id string) bool { return live[id] }
	var killed []string
	killFn := func(s string) error { killed = append(killed, s); return nil }

	var stdout, stderr bytes.Buffer
	if err := runMaintenancePruneOrphanTmux(&stdout, &stderr,
		true /* --kill */, listInfoFn, existsFn, killFn,
		func() time.Time { return pruneTestNow }, 90*time.Second); err != nil {
		t.Fatalf("runMaintenancePruneOrphanTmux: %v", err)
	}
	// Sort because killFn invocation order depends on listFn order,
	// which is the input order (not alphabetical — alpha order is for
	// stdout). Either way, the SET of killed sessions must be exactly
	// the orphans.
	wantKilled := []string{"fleet-orphana", "fleet-orphanb"}
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
	if !strings.Contains(got, "fleet-orphana  orphan=true  state=missing  killed=true\n") {
		t.Errorf("expected orphana row with killed=true; got:\n%s", got)
	}
	if !strings.Contains(got, "fleet-orphanb  orphan=true  state=missing  killed=true\n") {
		t.Errorf("expected orphanb row with killed=true; got:\n%s", got)
	}
	if !strings.Contains(got, "fleet-alivex  orphan=false  state=live  killed=false\n") {
		t.Errorf("expected alivex row with killed=false; got:\n%s", got)
	}
}

// TestPruneOrphanTmux_KillFailureSurfacedAsWarning verifies that a
// killFn error does NOT abort the sweep: the operator can chain this
// into a cron job and trust that one stuck session doesn't block the
// rest. The error message goes to stderr, exit is nil, and the row
// shows killed=false for the failed session.
func TestPruneOrphanTmux_KillFailureSurfacedAsWarning(t *testing.T) {
	listInfoFn := func() ([]tmux.SessionInfo, error) {
		return staleInfos("fleet-failkill", "fleet-okkill"), nil
	}
	existsFn := func(string) bool { return false } // both orphans
	killFn := func(s string) error {
		if s == "fleet-failkill" {
			return errors.New("simulated tmux failure")
		}
		return nil
	}

	var stdout, stderr bytes.Buffer
	if err := runMaintenancePruneOrphanTmux(&stdout, &stderr,
		true, listInfoFn, existsFn, killFn,
		func() time.Time { return pruneTestNow }, 90*time.Second); err != nil {
		t.Fatalf("runMaintenancePruneOrphanTmux: %v", err)
	}
	if !strings.Contains(stderr.String(), "warning: kill fleet-failkill") {
		t.Errorf("stderr should contain kill-failure warning; got: %s", stderr.String())
	}
	if !strings.Contains(stdout.String(), "fleet-failkill  orphan=true  state=missing  killed=false\n") {
		t.Errorf("stdout should show killed=false for the failed session; got:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "fleet-okkill  orphan=true  state=missing  killed=true\n") {
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
		listInfoFn, existsFn, killFn,
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
			"fleet-",        // degenerate — no ID
			"unrelated",     // non-fleet
			"work",          // non-fleet
			"fleet-real-id", // genuine orphan
		), nil
	}
	existsFn := func(string) bool { return false }
	killCalls := []string{}
	killFn := func(s string) error { killCalls = append(killCalls, s); return nil }

	var stdout, stderr bytes.Buffer
	if err := runMaintenancePruneOrphanTmux(&stdout, &stderr, true,
		listInfoFn, existsFn, killFn,
		func() time.Time { return pruneTestNow }, 90*time.Second); err != nil {
		t.Fatalf("runMaintenancePruneOrphanTmux: %v", err)
	}
	// Only the genuine fleet-<id> orphan should be reported / killed.
	got := stdout.String()
	if got != "fleet-real-id  orphan=true  state=missing  killed=true\n" {
		t.Errorf("expected only fleet-real-id row; got:\n%s", got)
	}
	if len(killCalls) != 1 || killCalls[0] != "fleet-real-id" {
		t.Errorf("expected exactly one kill of fleet-real-id; got %v", killCalls)
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
			{Name: "fleet-fresh01", Created: pruneTestNow.Add(-5 * time.Second)},
			// Stale: created 2 hours ago, comfortably past the window.
			{Name: "fleet-stale02", Created: pruneTestNow.Add(-2 * time.Hour)},
			// Edge case: zero Created (parse failure) — treat as fresh.
			{Name: "fleet-unknown03"},
		}, nil
	}
	existsFn := func(string) bool { return false } // all three records missing
	var killed []string
	killFn := func(s string) error { killed = append(killed, s); return nil }

	var stdout, stderr bytes.Buffer
	if err := runMaintenancePruneOrphanTmux(&stdout, &stderr,
		true /* --kill */, listInfoFn, existsFn, killFn,
		func() time.Time { return pruneTestNow }, 90*time.Second); err != nil {
		t.Fatalf("runMaintenancePruneOrphanTmux: %v", err)
	}

	// Only the stale session should be killed.
	if len(killed) != 1 || killed[0] != "fleet-stale02" {
		t.Fatalf("expected exactly one kill of fleet-stale02; got %v", killed)
	}

	got := stdout.String()
	// Fresh sessions: state=fresh, orphan=false, killed=false.
	wantFresh := "fleet-fresh01  orphan=false  state=fresh  killed=false\n"
	if !strings.Contains(got, wantFresh) {
		t.Errorf("expected fresh row %q; got:\n%s", wantFresh, got)
	}
	// Zero-Created session also treated as fresh (safer than orphan).
	wantUnknown := "fleet-unknown03  orphan=false  state=fresh  killed=false\n"
	if !strings.Contains(got, wantUnknown) {
		t.Errorf("expected zero-created row classified as fresh %q; got:\n%s", wantUnknown, got)
	}
	// Stale session: state=missing, orphan=true, killed=true.
	wantStale := "fleet-stale02  orphan=true  state=missing  killed=true\n"
	if !strings.Contains(got, wantStale) {
		t.Errorf("expected stale row %q; got:\n%s", wantStale, got)
	}
}

// TestPruneOrphanTmux_BoundaryAtFreshness pins the inclusive/exclusive
// behavior at the threshold: a session exactly at freshness counts as
// stale (now.Sub(created) < freshness is FALSE at exactly == freshness).
func TestPruneOrphanTmux_BoundaryAtFreshness(t *testing.T) {
	listInfoFn := func() ([]tmux.SessionInfo, error) {
		return []tmux.SessionInfo{
			// 89s old: still fresh.
			{Name: "fleet-89s", Created: pruneTestNow.Add(-89 * time.Second)},
			// 90s old: at boundary → stale.
			{Name: "fleet-90s", Created: pruneTestNow.Add(-90 * time.Second)},
			// 91s old: stale.
			{Name: "fleet-91s", Created: pruneTestNow.Add(-91 * time.Second)},
		}, nil
	}
	existsFn := func(string) bool { return false }
	var killed []string
	killFn := func(s string) error { killed = append(killed, s); return nil }

	var stdout, stderr bytes.Buffer
	if err := runMaintenancePruneOrphanTmux(&stdout, &stderr,
		true, listInfoFn, existsFn, killFn,
		func() time.Time { return pruneTestNow }, 90*time.Second); err != nil {
		t.Fatalf("runMaintenancePruneOrphanTmux: %v", err)
	}

	sort.Strings(killed)
	want := []string{"fleet-90s", "fleet-91s"}
	if len(killed) != len(want) {
		t.Fatalf("killed: got %v; want %v", killed, want)
	}
	for i := range killed {
		if killed[i] != want[i] {
			t.Errorf("killed[%d] = %q; want %q", i, killed[i], want[i])
		}
	}
	got := stdout.String()
	if !strings.Contains(got, "fleet-89s  orphan=false  state=fresh") {
		t.Errorf("89s row should be fresh; got:\n%s", got)
	}
	if !strings.Contains(got, "fleet-90s  orphan=true  state=missing") {
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
