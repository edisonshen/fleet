// F18 — attach-failover-59db: TUI agent-row [a] uses the same
// shared resolver as the CLI.
//
// Scenario per docs/TASK-PLAN-attach-failover.md F18:
//   "TUI cursor on agent-row whose record is alive but `tmux has-
//    session` is non-zero; project has live coord X" →
//   "Resolver path identical to F6 — TUI flashes a status, then
//    exec attach to live tail. Not a separate code path — calls the
//    shared resolver."
//
// The TUI cannot exec tmux directly (bubbletea owns the screen). The
// "exec attach to live tail" outcome here means setting
// m.pendingAttach to the live coord's session and returning tea.Quit;
// the main runtime then runs tmux.Attach after Quit unwinds.

package tui

import (
	"strings"
	"testing"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/projectlookup"
)

// TestF18_AgentRowDeadCoord_AttachesToLiveCoord — [a] on a dead-
// session coord-tagged agent row resolves to the project's live coord
// via projectlookup.FindLiveCoord and attaches to its session. The
// "press [x] to archive" flash is the OLD behavior; the new contract
// surfaces "attaching to live coord <id>" so the operator sees the
// rotation.
func TestF18_AgentRowDeadCoord_AttachesToLiveCoord(t *testing.T) {
	// Dead session for the dead-row coord, alive for the successor.
	(&stubSessionAlive{dead: map[string]bool{"fleet-deadcoor": true}}).install(t)
	(&stubSessionProbe{dead: map[string]bool{"fleet-deadcoor": true}}).install(t)
	// projectlookup has its own seam vars; stub them so FindLiveCoord
	// reports the successor alive without spinning up a real tmux.
	restorePL := projectlookup.SetTestStubs(
		func(s string) bool { return s == "fleet-livecoor" },
		func(s string) (bool, error) { return s == "fleet-livecoor", nil },
		nil,
	)
	t.Cleanup(restorePL)

	dead := sampleAgent("deadcoor")
	dead.Project = "demo"
	dead.TaskID = coordTaskID("demo")
	live := sampleAgent("livecoor")
	live.Project = "demo"
	live.TaskID = coordTaskID("demo")

	m := makeModelWithAgents(dead, live)
	// makeModelWithAgents lands the cursor on the FIRST rowAgent; make
	// sure that's the dead one (snap if not).
	for i, r := range m.dashboardRows() {
		if r.kind == rowAgent && r.agent != nil && r.agent.ID == "deadcoor" {
			m.dashCursor = i
			break
		}
	}

	updated, cmd := m.Update(keyMsg("a"))
	mm := updated.(Model)
	if mm.pendingAttach != "fleet-livecoor" {
		t.Errorf("pendingAttach = %q; want fleet-livecoor (Tier 3 rotated to live coord)", mm.pendingAttach)
	}
	if cmd == nil {
		t.Error("expected tea.Quit cmd after rotating to live coord")
	}
	if mm.flash == nil {
		t.Fatal("expected status flash naming the rotation")
	}
	if mm.flash.isErr {
		t.Errorf("flash should be informational, not error; got: %+v", mm.flash)
	}
	if !strings.Contains(mm.flash.text, "livecoor") || !strings.Contains(mm.flash.text, "demo") {
		t.Errorf("flash should name the live coord + project; got %q", mm.flash.text)
	}
}

// TestF18_AgentRowDeadCoord_NoLiveSuccessor_PreservesArchiveHint —
// when no live coord exists for the same project, the agent-row [a]
// keeps the existing "press [a] on project row" hint (delegates to the
// project-row recovery flow). Ensures we don't break the no-rotation
// case while wiring F18.
func TestF18_AgentRowDeadCoord_NoLiveSuccessor_PreservesArchiveHint(t *testing.T) {
	(&stubSessionAlive{dead: map[string]bool{"fleet-deadcoor": true}}).install(t)
	(&stubSessionProbe{dead: map[string]bool{"fleet-deadcoor": true}}).install(t)
	restorePL := projectlookup.SetTestStubs(
		func(s string) bool { return false },
		func(s string) (bool, error) { return false, nil },
		nil,
	)
	t.Cleanup(restorePL)

	dead := sampleAgent("deadcoor")
	dead.Project = "demo"
	dead.TaskID = coordTaskID("demo")
	m := makeModelWithAgents(dead)
	for i, r := range m.dashboardRows() {
		if r.kind == rowAgent && r.agent != nil && r.agent.ID == "deadcoor" {
			m.dashCursor = i
			break
		}
	}

	updated, cmd := m.Update(keyMsg("a"))
	mm := updated.(Model)
	if mm.pendingAttach != "" {
		t.Errorf("pendingAttach = %q; want empty (no live successor → no auto-attach)", mm.pendingAttach)
	}
	if cmd != nil {
		t.Errorf("expected no tea.Cmd when no successor; got %v", cmd)
	}
	if mm.flash == nil || !mm.flash.isErr {
		t.Fatalf("expected error flash with archive hint; got %+v", mm.flash)
	}
	if !strings.Contains(mm.flash.text, "project") {
		t.Errorf("flash should redirect to project row; got %q", mm.flash.text)
	}

	// agent package import keeps the file honest if test changes drop
	// the direct reference; sampleAgent already pulls in agent.
	_ = agent.New
}
