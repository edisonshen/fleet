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
