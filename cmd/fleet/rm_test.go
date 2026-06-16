package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edisonshen/fleet/internal/queue"
	"github.com/edisonshen/fleet/internal/tmux"
)

func TestRm_LiveSession_KillsAndArchives(t *testing.T) {
	requireFakeTmux(t)
	tmp := setupFleetHome(t)

	old := seedAgent(t)
	t.Cleanup(func() { _ = tmux.Kill(old.TmuxSession) })

	out := &bytes.Buffer{}
	if err := runRm(&rmOpts{id: old.ID}, out, out); err != nil {
		t.Fatalf("rm: %v\n%s", err, out.String())
	}

	// Live record gone, archived.
	if _, err := os.Stat(filepath.Join(tmp, "agents", old.ID+".json")); !os.IsNotExist(err) {
		t.Errorf("live record should be gone, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "agents", "archive", old.ID+".json")); err != nil {
		t.Errorf("archive missing: %v", err)
	}
	// Tmux session killed.
	if tmux.HasSession(old.TmuxSession) {
		t.Errorf("tmux session %s should be dead", old.TmuxSession)
	}
	// No replacement spawned.
	if n := len(listLive(t)); n != 0 {
		t.Errorf("expected 0 live agents post-rm, got %d", n)
	}
	// No handoff doc, no queue file.
	if entries, _ := os.ReadDir(filepath.Join(tmp, "handoffs")); len(entries) > 0 {
		t.Errorf("rm should not write a handoff doc, got: %v", entries)
	}
	if entries, _ := os.ReadDir(filepath.Join(tmp, "queue")); len(entries) > 0 {
		t.Errorf("rm should not leave queue files, got: %v", entries)
	}
	// Output mentions both the id and the "no replacement" semantics so
	// the operator isn't confused by the absence of a successor line.
	s := out.String()
	for _, want := range []string{old.ID, "archived", "no replacement"} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q:\n%s", want, s)
		}
	}
}

func TestRm_DeadSession_ArchivesIdempotently(t *testing.T) {
	// `[a]` on a dead session prompts "press [h] to clean up" today;
	// after PR-B the [x] keybind is the right verb. Make sure rm
	// handles the dead-session case cleanly (kill is a no-op, archive
	// proceeds).
	requireFakeTmux(t)
	tmp := setupFleetHome(t)

	old := seedAgent(t)
	if err := tmux.Kill(old.TmuxSession); err != nil {
		t.Fatalf("seed kill: %v", err)
	}
	if tmux.HasSession(old.TmuxSession) {
		t.Fatalf("seed: session %s should be dead", old.TmuxSession)
	}

	out := &bytes.Buffer{}
	if err := runRm(&rmOpts{id: old.ID}, out, out); err != nil {
		t.Fatalf("rm on dead session: %v\n%s", err, out.String())
	}

	if _, err := os.Stat(filepath.Join(tmp, "agents", "archive", old.ID+".json")); err != nil {
		t.Errorf("dead-session rm should still archive, got: %v", err)
	}
}

func TestRm_UnknownAgent_FailsClearly(t *testing.T) {
	requireFakeTmux(t)
	setupFleetHome(t)

	out := &bytes.Buffer{}
	err := runRm(&rmOpts{id: "ghostbas"}, out, out)
	if err == nil {
		t.Error("expected error for unknown agent")
	}
	if !strings.Contains(err.Error(), "no agent record") {
		t.Errorf("expected 'no agent record' in error, got: %v", err)
	}
}

func TestRm_RefusesWhenHandoffPending(t *testing.T) {
	// A queue file means a handoff is journaled. Removing the outgoing
	// record would orphan the recovery probe in `fleet handoff` (it
	// would see the journal but no old record on retry). rm must
	// refuse and tell the operator to drain first.
	requireFakeTmux(t)
	tmp := setupFleetHome(t)

	old := seedAgent(t)
	t.Cleanup(func() { _ = tmux.Kill(old.TmuxSession) })

	if _, err := queue.WriteSpawnFresh(queue.SpawnFresh{
		OldAgentID: old.ID,
		HandoffDoc: "/tmp/dummy.md",
		Project:    old.Project,
		TaskID:     old.TaskID,
		NewAgentID: "successo",
		NewSession: "fleet-successo",
	}); err != nil {
		t.Fatalf("seed queue: %v", err)
	}

	out := &bytes.Buffer{}
	err := runRm(&rmOpts{id: old.ID}, out, out)
	if err == nil {
		t.Fatal("expected rm to refuse with pending handoff")
	}
	if !strings.Contains(err.Error(), "pending handoff") {
		t.Errorf("expected 'pending handoff' in error, got: %v", err)
	}

	// Live record still present — refusal must be a no-op on disk.
	if _, err := os.Stat(filepath.Join(tmp, "agents", old.ID+".json")); err != nil {
		t.Errorf("rm refusal should leave live record intact, got: %v", err)
	}
	// Session still alive too.
	if !tmux.HasSession(old.TmuxSession) {
		t.Errorf("rm refusal should leave session alive")
	}
}
