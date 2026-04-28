package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tmux"
)

func setupFleetHome(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	return tmp
}

func requireTmux(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed; skipping integration test")
	}
	out, err := exec.Command("openssl", "rand", "-hex", "3").Output()
	if err != nil {
		t.Fatalf("rand: %v", err)
	}
	t.Setenv("FLEET_TMUX_SOCKET", "/tmp/fleet-test-"+strings.TrimSpace(string(out))+".sock")
}

// seedAgent dispatches a long-lived agent for handoff to operate on.
// Returns the spawned record. Caller cleans up via t.Cleanup if needed.
func seedAgent(t *testing.T) *agent.Record {
	t.Helper()
	out := &bytes.Buffer{}
	if err := runDispatch(&dispatchOpts{
		taskID:  "auth-fix",
		project: "rainier",
		command: []string{"sleep", "60"},
	}, out); err != nil {
		t.Fatalf("dispatch: %v\n%s", err, out.String())
	}
	live, err := agent.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(live) != 1 {
		t.Fatalf("expected 1 live agent after dispatch, got %d", len(live))
	}
	return live[0]
}

func TestHandoff_FailsClearlyOnUnknownAgent(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)

	out := &bytes.Buffer{}
	err := runHandoff(&handoffOpts{
		oldID:       "ghostbas",
		command:     []string{"sleep", "60"},
		graceMillis: 0,
	}, out, out)
	if err == nil {
		t.Error("expected error for unknown agent")
	}
}

func TestHandoff_HappyPath(t *testing.T) {
	requireTmux(t)
	tmp := setupFleetHome(t)

	old := seedAgent(t)
	t.Cleanup(func() { _ = tmux.Kill(old.TmuxSession) })

	out := &bytes.Buffer{}
	if err := runHandoff(&handoffOpts{
		oldID:       old.ID,
		command:     []string{"sleep", "60"},
		graceMillis: 0, // no sleep in tests
	}, out, out); err != nil {
		t.Fatalf("handoff: %v\n%s", err, out.String())
	}

	// Old agent record archived.
	if _, err := os.Stat(filepath.Join(tmp, "agents", old.ID+".json")); !os.IsNotExist(err) {
		t.Errorf("old live record should be gone, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "agents", "archive", old.ID+".json")); err != nil {
		t.Errorf("old archive record missing: %v", err)
	}

	// Old tmux session killed.
	if tmux.HasSession(old.TmuxSession) {
		t.Errorf("old tmux session %s should be dead", old.TmuxSession)
	}

	// Exactly one live agent now (the replacement).
	live, err := agent.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(live) != 1 {
		t.Fatalf("expected 1 live agent after handoff, got %d", len(live))
	}
	newRec := live[0]
	t.Cleanup(func() { _ = tmux.Kill(newRec.TmuxSession) })

	if newRec.ID == old.ID {
		t.Error("new agent must have different ID")
	}
	if newRec.TaskID != "auth-fix" || newRec.Project != "rainier" {
		t.Errorf("task identity not inherited: %+v", newRec)
	}
	if newRec.HandoffNumber != old.HandoffNumber+1 {
		t.Errorf("HandoffNumber: got %d want %d", newRec.HandoffNumber, old.HandoffNumber+1)
	}
	if newRec.LastHandoffPath == nil {
		t.Error("LastHandoffPath should point at the doc just written")
	}
	if newRec.HandoffType == nil || *newRec.HandoffType != "manual" {
		t.Errorf("HandoffType: want manual, got %v", newRec.HandoffType)
	}

	// Handoff doc exists at the path the new agent points at.
	if _, err := os.Stat(*newRec.LastHandoffPath); err != nil {
		t.Errorf("handoff doc missing: %v", err)
	}

	// Queue file is gone (drained).
	queueDir := filepath.Join(tmp, "queue")
	entries, _ := os.ReadDir(queueDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "spawn-fresh-") {
			t.Errorf("queue file %s should have been deleted", e.Name())
		}
	}

	// Output mentions both IDs and the handoff doc path.
	s := out.String()
	for _, want := range []string{old.ID, newRec.ID, "handed off", *newRec.LastHandoffPath} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q:\n%s", want, s)
		}
	}
}

func TestHandoff_ChainGrowsAcrossSequentialHandoffs(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)

	// Initial dispatch.
	first := seedAgent(t)
	t.Cleanup(func() { _ = tmux.Kill(first.TmuxSession) })

	// Three handoffs in a row.
	currentID := first.ID
	var lastDocPath string
	for i := 0; i < 3; i++ {
		out := &bytes.Buffer{}
		if err := runHandoff(&handoffOpts{
			oldID:       currentID,
			command:     []string{"sleep", "60"},
			graceMillis: 0,
		}, out, out); err != nil {
			t.Fatalf("handoff #%d: %v\n%s", i+1, err, out.String())
		}
		live, err := agent.List()
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(live) != 1 {
			t.Fatalf("expected 1 live, got %d (after handoff #%d)", len(live), i+1)
		}
		current := live[0]
		t.Cleanup(func() { _ = tmux.Kill(current.TmuxSession) })

		wantNumber := first.HandoffNumber + i + 1
		if current.HandoffNumber != wantNumber {
			t.Errorf("handoff #%d: got HandoffNumber=%d want %d",
				i+1, current.HandoffNumber, wantNumber)
		}
		if current.LastHandoffPath == nil {
			t.Errorf("handoff #%d: LastHandoffPath nil", i+1)
		}
		// The previous_handoff field of doc N points to doc N-1.
		if i > 0 && lastDocPath == "" {
			t.Errorf("handoff #%d: chain broken, no prev doc tracked", i+1)
		}
		lastDocPath = *current.LastHandoffPath
		currentID = current.ID
	}
}

func TestHandoff_ConcurrentHandoffDetectedAfterArchive(t *testing.T) {
	// Simulates the race the flock-first ordering prevents: handoff
	// runs once, archives the agent. A second handoff invocation for
	// the same ID then loads the agent (succeeds — caller just read
	// from disk before lock), acquires the flock, re-loads under the
	// lock, sees ErrNotFound, bails. This test exercises the second
	// invocation against an already-archived record.
	requireTmux(t)
	setupFleetHome(t)

	old := seedAgent(t)
	t.Cleanup(func() { _ = tmux.Kill(old.TmuxSession) })

	// First handoff: succeeds, archives the agent.
	out1 := &bytes.Buffer{}
	if err := runHandoff(&handoffOpts{
		oldID:       old.ID,
		command:     []string{"sleep", "60"},
		graceMillis: 0,
	}, out1, out1); err != nil {
		t.Fatalf("first handoff: %v\n%s", err, out1.String())
	}
	for _, l := range listLive(t) {
		t.Cleanup(func() { _ = tmux.Kill(l.TmuxSession) })
	}

	// Second handoff for the same OLD ID: should fail at the
	// re-load-under-lock step. The agent is gone from agents/.
	out2 := &bytes.Buffer{}
	err := runHandoff(&handoffOpts{
		oldID:       old.ID,
		command:     []string{"sleep", "60"},
		graceMillis: 0,
	}, out2, out2)
	if err == nil {
		t.Fatalf("expected second handoff to fail (record archived), got success:\n%s", out2.String())
	}
	if !strings.Contains(err.Error(), "concurrent handoff") &&
		!strings.Contains(err.Error(), "not found") {
		t.Errorf("expected error to mention concurrent handoff or not found, got: %v", err)
	}

	// Live count should still be 1 (only the first handoff's
	// replacement, no second double-spawn).
	if n := len(listLive(t)); n != 1 {
		t.Errorf("expected 1 live agent after blocked second handoff, got %d", n)
	}
}

func listLive(t *testing.T) []*agent.Record {
	t.Helper()
	live, err := agent.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return live
}

func TestHandoff_DocBodyContainsPlaceholders(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)

	old := seedAgent(t)
	t.Cleanup(func() { _ = tmux.Kill(old.TmuxSession) })

	out := &bytes.Buffer{}
	if err := runHandoff(&handoffOpts{
		oldID:       old.ID,
		command:     []string{"sleep", "60"},
		graceMillis: 0,
	}, out, out); err != nil {
		t.Fatalf("handoff: %v", err)
	}

	live, _ := agent.List()
	if len(live) == 0 {
		t.Fatal("no live agent after handoff")
	}
	t.Cleanup(func() { _ = tmux.Kill(live[0].TmuxSession) })

	body, err := os.ReadFile(*live[0].LastHandoffPath)
	if err != nil {
		t.Fatalf("read doc: %v", err)
	}
	for _, want := range []string{
		"## Completed",
		"## Key Decisions",
		"## Files Modified",
		"## Open Questions",
		"## Next Steps (prioritized)",
		"_(operator-triggered handoff",
		`handoff_type: "manual"`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("doc missing %q:\n%s", want, string(body))
		}
	}
}
