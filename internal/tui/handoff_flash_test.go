package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
)

// handoff-identity-cont-3f1d Piece 3 — visual rotation flash.
//
// On handoffDoneMsg for a project, render "→ <successor>" dimmed token
// on the predecessor row for 3s, then drop. Successor row appears via
// the normal agents refresh.

// newFlashTestModel keeps the rotation-flash tests local; uses the
// package's New constructor.
func newFlashTestModel() Model { return New("test") }

// TestHandoffDoneMsg_ParsesPredAndSuccessor_PinsFlash pins that the
// flash map gets populated when fleet handoff's stdout matches the
// "agent <pred> handed off → <succ>" shape.
func TestHandoffDoneMsg_ParsesPredAndSuccessor_PinsFlash(t *testing.T) {
	m := newFlashTestModel()
	out := "agent aaaaaaaa handed off → bbbbbbbb\n  task:    t\n  project: p\n"
	updated, _ := m.Update(handoffDoneMsg{projectName: "p", out: out})
	um, ok := updated.(Model)
	if !ok {
		t.Fatal("Update did not return Model")
	}
	if got := um.rotationFlash["aaaaaaaa"]; got != "bbbbbbbb" {
		t.Errorf("rotationFlash[pred]: got %q want bbbbbbbb", got)
	}
}

// TestHandoffDoneMsg_NoSuccessorInOutput_NoFlashPinned — when the
// output doesn't carry "→ <succ>" (e.g. error or unexpected shape),
// no flash entry is created. Existing flash banner still surfaces.
func TestHandoffDoneMsg_NoSuccessorInOutput_NoFlashPinned(t *testing.T) {
	m := newFlashTestModel()
	out := "agent aaaaaaaa: handoff aborted (something failed)"
	updated, _ := m.Update(handoffDoneMsg{projectName: "p", out: out})
	um := updated.(Model)
	if len(um.rotationFlash) != 0 {
		t.Errorf("rotationFlash should be empty on parse-fail; got %v", um.rotationFlash)
	}
}

// TestRotationFlashDrop_ClearsEntry — the drop msg removes the
// predecessor entry. Other entries (if any) survive.
func TestRotationFlashDrop_ClearsEntry(t *testing.T) {
	m := newFlashTestModel()
	m.rotationFlash = map[string]string{"aaaaaaaa": "bbbbbbbb", "cccccccc": "dddddddd"}
	updated, _ := m.Update(rotationFlashDropMsg{predID: "aaaaaaaa"})
	um := updated.(Model)
	if _, ok := um.rotationFlash["aaaaaaaa"]; ok {
		t.Error("drop msg should remove the entry")
	}
	if got := um.rotationFlash["cccccccc"]; got != "dddddddd" {
		t.Errorf("other entry should survive; got %q", got)
	}
}

// TestRotationFlashDrop_OnEmptyMap_NoPanic — the drop msg is safe
// against the empty map (e.g. an early agents refresh nuked the
// entry already, then the timer fires).
func TestRotationFlashDrop_OnEmptyMap_NoPanic(t *testing.T) {
	m := newFlashTestModel()
	// rotationFlash nil
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("drop on empty/nil map panicked: %v", r)
		}
	}()
	_, _ = m.Update(rotationFlashDropMsg{predID: "missing0"})
}

// TestAgentBlockLines_RendersRotationToken_WhenSuccessorPinned —
// the agent row for a predecessor with an active rotation flash
// includes "→ <succ>" in its rendered output.
func TestAgentBlockLines_RendersRotationToken_WhenSuccessorPinned(t *testing.T) {
	r := agent.New("aaaaaaaa")
	r.SpawnedAt = time.Now()
	r.TmuxSession = "fleet-aaaaaaaa"
	out := strings.Join(
		agentBlockLinesWithFlash(r, map[string]bool{r.ID: true}, 80, false, "bbbbbbbb"),
		"\n",
	)
	if !strings.Contains(out, "→ bbbbbbbb") {
		t.Errorf("expected '→ bbbbbbbb' in render; got:\n%s", out)
	}
}

// TestAgentBlockLines_NoFlash_NoToken — control test: without a
// successor chip, the row does NOT contain "→".
func TestAgentBlockLines_NoFlash_NoToken(t *testing.T) {
	r := agent.New("aaaaaaaa")
	r.SpawnedAt = time.Now()
	r.TmuxSession = "fleet-aaaaaaaa"
	out := strings.Join(
		agentBlockLinesWithFlash(r, map[string]bool{r.ID: true}, 80, false, ""),
		"\n",
	)
	if strings.Contains(out, "→") {
		t.Errorf("no successor chip expected; got:\n%s", out)
	}
}

// TestActionAttach_AgentRow_WalksChainOnStaleRow — handoff-identity-cont-3f1d
// Piece 2: the rowAgent [a] handler runs the chain resolver before
// the existing live-record path. When the row's agent has been
// archived as a handoff and the live tail exists with a healthy tmux
// session, [a] attaches to the live tail, not the stale row's session.
func TestActionAttach_AgentRow_WalksChainOnStaleRow(t *testing.T) {
	prevResolve := chainResolveFn
	prevAlive := sessionAliveFn
	t.Cleanup(func() {
		chainResolveFn = prevResolve
		sessionAliveFn = prevAlive
	})
	// Resolver claims the row id (cedacc55) handed off 2 hops away
	// to 50fc19be (live).
	tail := agent.New("50fc19be")
	tail.TmuxSession = "fleet-50fc19be"
	chainResolveFn = func(id string) (*agent.Record, int, error) {
		if id == "cedacc55" {
			return tail, 2, nil
		}
		return nil, 0, agent.ErrNoLiveSuccessor
	}
	// Live tail's tmux session is alive.
	sessionAliveFn = func(session string) bool {
		return session == "fleet-50fc19be"
	}

	m := New("test")
	// Seed one agent row + cursor on it.
	stale := agent.New("cedacc55")
	stale.TmuxSession = "fleet-cedacc55"
	m.records = []*agent.Record{stale}
	m.aliveByID = map[string]bool{"cedacc55": true}
	m.dashCursor = findCursorOnAgent(t, m, "cedacc55")

	updated, _, _ := m.actionAttach()
	um := updated
	if um.pendingAttach != "fleet-50fc19be" {
		t.Errorf("pendingAttach: got %q want fleet-50fc19be", um.pendingAttach)
	}
	if um.flash == nil || !strings.Contains(um.flash.text, "rotated through 2 hops") {
		t.Errorf("flash should mention rotation; got: %#v", um.flash)
	}
}

// findCursorOnAgent locates the agent row in dashboardRows() so the
// test can position the cursor on it before calling actionAttach.
func findCursorOnAgent(t *testing.T, m Model, id string) int {
	t.Helper()
	rows := m.dashboardRows()
	for i, r := range rows {
		if r.kind == rowAgent && r.agent != nil && r.agent.ID == id {
			return i
		}
	}
	t.Fatalf("no agent row for %q in dashboardRows()", id)
	return -1
}
