package tui

// dashboard-accumulation-f-4421 Sub-fix B: right-column v0.1 agent
// idle-collapse separator.
//
// Tests pin the three rendering cases the operator's screenshot
// motivated:
//   - stale, no flags                       → collapsed under separator
//   - flagged asking (NeedsInput=true)      → ALWAYS active (never collapses)
//   - flagged blocked (Blocked=true)        → ALWAYS active (never collapses)
//
// Plus a separator-absence pin (zero idle records → no separator row)
// and a search-mode pin (an active /search filter suppresses the
// separator entirely, matching the LEFT column's override).

import (
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
)

func TestRightColumn_AgentIdleCollapse_StaleAgentCollapses(t *testing.T) {
	withFleetHome(t)
	now := time.Now()
	stale := now.Add(-30 * 24 * time.Hour) // way outside any reasonable window
	m := New("test")
	m.dashboard = &Snapshot{LoadedAt: now}
	m.records = []*agent.Record{
		{
			ID: "stale001", Project: "p", TmuxSession: "fleet-stale001",
			LastActivityTS: stale, SpawnedAt: stale,
		},
	}

	rows := m.dashboardRows()
	var sawSeparator bool
	var inlineAgents []string
	for _, r := range rows {
		if r.kind == rowSeparator && r.separator != nil && r.separator.kind == separatorAgentIdle {
			sawSeparator = true
			if r.separator.count != 1 {
				t.Errorf("separator count = %d, want 1", r.separator.count)
			}
		}
		if r.kind == rowAgent {
			inlineAgents = append(inlineAgents, r.agent.ID)
		}
	}
	if !sawSeparator {
		t.Fatalf("expected agent-idle separator for stale record; rows: %+v", rows)
	}
	// Default agentIdleExpanded=false → the stale agent must NOT appear
	// as an inline row (only behind the separator).
	if len(inlineAgents) != 0 {
		t.Errorf("stale agent should be hidden behind separator (agentIdleExpanded=false); got inline: %v", inlineAgents)
	}
}

func TestRightColumn_AgentIdleCollapse_AskingNeverCollapses(t *testing.T) {
	withFleetHome(t)
	now := time.Now()
	stale := now.Add(-30 * 24 * time.Hour)
	m := New("test")
	m.dashboard = &Snapshot{LoadedAt: now}
	m.records = []*agent.Record{
		{
			ID: "asking01", Project: "p", TmuxSession: "fleet-asking01",
			LastActivityTS: stale, SpawnedAt: stale,
			NeedsInput: true,
		},
	}

	rows := m.dashboardRows()
	var sawSeparator bool
	var inlineAgents []string
	for _, r := range rows {
		if r.kind == rowSeparator && r.separator != nil && r.separator.kind == separatorAgentIdle {
			sawSeparator = true
		}
		if r.kind == rowAgent {
			inlineAgents = append(inlineAgents, r.agent.ID)
		}
	}
	if sawSeparator {
		t.Errorf("asking record (NeedsInput=true) must NOT be folded under idle separator; rows: %+v", rows)
	}
	if len(inlineAgents) != 1 || inlineAgents[0] != "asking01" {
		t.Errorf("asking record must render inline regardless of staleness; got inline: %v", inlineAgents)
	}
}

func TestRightColumn_AgentIdleCollapse_BlockedNeverCollapses(t *testing.T) {
	withFleetHome(t)
	now := time.Now()
	stale := now.Add(-30 * 24 * time.Hour)
	m := New("test")
	m.dashboard = &Snapshot{LoadedAt: now}
	m.records = []*agent.Record{
		{
			ID: "blocked1", Project: "p", TmuxSession: "fleet-blocked1",
			LastActivityTS: stale, SpawnedAt: stale,
			Blocked: true,
		},
	}

	rows := m.dashboardRows()
	var sawSeparator bool
	var inlineAgents []string
	for _, r := range rows {
		if r.kind == rowSeparator && r.separator != nil && r.separator.kind == separatorAgentIdle {
			sawSeparator = true
		}
		if r.kind == rowAgent {
			inlineAgents = append(inlineAgents, r.agent.ID)
		}
	}
	if sawSeparator {
		t.Errorf("blocked record (Blocked=true) must NOT be folded under idle separator; rows: %+v", rows)
	}
	if len(inlineAgents) != 1 || inlineAgents[0] != "blocked1" {
		t.Errorf("blocked record must render inline regardless of staleness; got inline: %v", inlineAgents)
	}
}

func TestRightColumn_AgentIdleCollapse_NoSeparatorWhenEmpty(t *testing.T) {
	withFleetHome(t)
	now := time.Now()
	m := New("test")
	m.dashboard = &Snapshot{LoadedAt: now}
	// Only fresh, non-flagged record — zero idle bucket → zero separator.
	m.records = []*agent.Record{
		{
			ID: "fresh001", Project: "p", TmuxSession: "fleet-fresh001",
			LastActivityTS: now, SpawnedAt: now,
		},
	}

	rows := m.dashboardRows()
	for _, r := range rows {
		if r.kind == rowSeparator && r.separator != nil && r.separator.kind == separatorAgentIdle {
			t.Errorf("agent-idle separator must NOT render when idle bucket is empty; got: %+v", r.separator)
		}
	}
}

func TestRightColumn_AgentIdleCollapse_SearchSuppressesSeparator(t *testing.T) {
	withFleetHome(t)
	now := time.Now()
	stale := now.Add(-30 * 24 * time.Hour)
	m := New("test")
	m.dashboard = &Snapshot{LoadedAt: now}
	m.records = []*agent.Record{
		{
			ID: "stale001", Project: "p", TmuxSession: "fleet-stale001",
			LastActivityTS: stale, SpawnedAt: stale,
		},
	}
	// Active filter — should override the collapse so the matching
	// row surfaces inline, matching the LEFT-column projects path.
	m.searchFilter = "stale001"

	rows := m.dashboardRows()
	var sawSeparator bool
	var inlineAgents []string
	for _, r := range rows {
		if r.kind == rowSeparator && r.separator != nil && r.separator.kind == separatorAgentIdle {
			sawSeparator = true
		}
		if r.kind == rowAgent {
			inlineAgents = append(inlineAgents, r.agent.ID)
		}
	}
	if sawSeparator {
		t.Errorf("active search filter must suppress the agent-idle separator; rows: %+v", rows)
	}
	if len(inlineAgents) != 1 || inlineAgents[0] != "stale001" {
		t.Errorf("search-matched stale record must render inline; got inline: %v", inlineAgents)
	}
}

func TestRightColumn_AgentIdleCollapse_ExpandedShowsIdleRows(t *testing.T) {
	withFleetHome(t)
	now := time.Now()
	stale := now.Add(-30 * 24 * time.Hour)
	m := New("test")
	m.dashboard = &Snapshot{LoadedAt: now}
	m.records = []*agent.Record{
		{
			ID: "stale001", Project: "p", TmuxSession: "fleet-stale001",
			LastActivityTS: stale, SpawnedAt: stale,
		},
		{
			ID: "stale002", Project: "p", TmuxSession: "fleet-stale002",
			LastActivityTS: stale, SpawnedAt: stale,
		},
	}
	m.agentIdleExpanded = true

	rows := m.dashboardRows()
	var sawSeparator bool
	var inlineAgents []string
	for _, r := range rows {
		if r.kind == rowSeparator && r.separator != nil && r.separator.kind == separatorAgentIdle {
			sawSeparator = true
			if !r.separator.expanded {
				t.Errorf("separator.expanded must mirror m.agentIdleExpanded")
			}
		}
		if r.kind == rowAgent {
			inlineAgents = append(inlineAgents, r.agent.ID)
		}
	}
	if !sawSeparator {
		t.Errorf("expanded mode must still render the separator (as the toggle anchor)")
	}
	if len(inlineAgents) != 2 {
		t.Errorf("expanded mode must show both stale agents; got %v", inlineAgents)
	}
}

// /review iter-1 [P1] regression: the new separatorAgentIdle kind needs
// a stable rowIdentity string so refreshCursor (called on every
// dashboardMsg / agentsMsg tick — 1s poll) snaps back onto the
// separator row across refreshes. Without this, the cursor sitting on
// the right-column "─── N idle ───" separator silently re-points to
// whatever row happens to be at the same numeric index after the
// next poll.
func TestRowIdentity_AgentIdleSeparatorIsStable(t *testing.T) {
	r := dashRow{
		kind: rowSeparator,
		separator: &separatorRow{
			kind:  separatorAgentIdle,
			count: 5,
		},
	}
	id := rowIdentity(r)
	if id == "" {
		t.Fatalf("separatorAgentIdle must have a non-empty identity; got %q", id)
	}
	if id != "S:agent-idle" {
		t.Errorf("separatorAgentIdle identity = %q, want %q", id, "S:agent-idle")
	}
}

func TestClassifyAgentActivity_HonorsResolveActiveWindow(t *testing.T) {
	// Direct test of the classifier so the env-driven window is pinned
	// without going through the full dashboardRows render. Spec: reuse
	// resolveActiveWindow's behavior (FLEET_ACTIVE_WINDOW_DAYS, default 7d).
	t.Setenv("FLEET_ACTIVE_WINDOW_DAYS", "1")
	window := resolveActiveWindow()
	if window != 24*time.Hour {
		t.Fatalf("resolveActiveWindow with FLEET_ACTIVE_WINDOW_DAYS=1 = %v, want 24h", window)
	}
	now := time.Now()
	cases := []struct {
		name string
		rec  *agent.Record
		want projectActivity
	}{
		{
			name: "fresh heartbeat → active",
			rec:  &agent.Record{LastActivityTS: now.Add(-1 * time.Hour)},
			want: projectActive,
		},
		{
			name: "stale heartbeat → idle",
			rec:  &agent.Record{LastActivityTS: now.Add(-48 * time.Hour)},
			want: projectIdle,
		},
		{
			name: "stale heartbeat but asking → active (operator must triage)",
			rec:  &agent.Record{LastActivityTS: now.Add(-48 * time.Hour), NeedsInput: true},
			want: projectActive,
		},
		{
			name: "stale heartbeat but blocked → active",
			rec:  &agent.Record{LastActivityTS: now.Add(-48 * time.Hour), Blocked: true},
			want: projectActive,
		},
		{
			name: "zero heartbeat, fresh spawn → active (just dispatched)",
			rec:  &agent.Record{SpawnedAt: now.Add(-30 * time.Minute)},
			want: projectActive,
		},
		{
			name: "all timestamps zero → idle (no freshness signal)",
			rec:  &agent.Record{},
			want: projectIdle,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyAgentActivity(tc.rec, now, window)
			if got != tc.want {
				t.Errorf("classifyAgentActivity = %v, want %v", got, tc.want)
			}
		})
	}
}
