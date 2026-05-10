package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
)

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
