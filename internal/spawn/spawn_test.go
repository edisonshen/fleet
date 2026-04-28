package spawn

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/handoff"
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
}

func TestSpawn_RequiresCommand(t *testing.T) {
	setupFleetHome(t)
	if _, err := Spawn(Options{}); err == nil {
		t.Error("expected error for missing command")
	}
}

func TestSpawn_FreshDispatch(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)

	rec, err := Spawn(Options{
		TaskID:  "auth-fix",
		Project: "rainier",
		Command: []string{"sleep", "30"},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(rec.TmuxSession) })

	if rec.ID == "" {
		t.Error("ID not set")
	}
	if rec.TmuxSession != "fleet-"+rec.ID {
		t.Errorf("TmuxSession=%q want fleet-%s", rec.TmuxSession, rec.ID)
	}
	if rec.TaskID != "auth-fix" || rec.Project != "rainier" {
		t.Errorf("task identity not set: %+v", rec)
	}
	if rec.HandoffNumber != 1 {
		t.Errorf("HandoffNumber: got %d want 1 (fresh dispatch)", rec.HandoffNumber)
	}
	if rec.LastHandoffPath != nil {
		t.Errorf("LastHandoffPath: want nil for fresh dispatch, got %v", rec.LastHandoffPath)
	}
	if rec.HandoffType != nil {
		t.Errorf("HandoffType: want nil for fresh dispatch, got %v", rec.HandoffType)
	}
	if !tmux.HasSession(rec.TmuxSession) {
		t.Errorf("tmux session %s should be alive", rec.TmuxSession)
	}

	// Record should be on disk and loadable.
	loaded, err := agent.Load(rec.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.TaskID != "auth-fix" {
		t.Errorf("loaded.TaskID=%q", loaded.TaskID)
	}
}

func TestSpawn_FromHandoffInheritsAndIncrements(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)

	// Build an "old" record as if we just wrote a handoff doc for it.
	old := agent.New("aaaa1111")
	old.TaskID = "auth-fix"
	old.Project = "rainier"
	old.Engine = "claude-code"
	old.Role = "executor"
	old.Mode = "execute"
	old.HandoffNumber = 3
	prevPath := "/some/handoffs/aaaa0000-20260427-180000.md"
	old.LastHandoffPath = &prevPath

	docPath := "/some/handoffs/aaaa1111-20260427-184807.md"

	rec, err := Spawn(Options{
		OldRecord:  old,
		NewDocPath: docPath,
		Command:    []string{"sleep", "30"},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(rec.TmuxSession) })

	if rec.ID == old.ID {
		t.Error("new agent must have a different ID than old")
	}
	if rec.TaskID != "auth-fix" || rec.Project != "rainier" {
		t.Errorf("task identity not inherited: %+v", rec)
	}
	if rec.Engine != "claude-code" || rec.Role != "executor" || rec.Mode != "execute" {
		t.Errorf("engine/role/mode not inherited: %+v", rec)
	}
	if rec.HandoffNumber != 4 {
		t.Errorf("HandoffNumber: got %d want 4 (3+1)", rec.HandoffNumber)
	}
	if rec.LastHandoffPath == nil || *rec.LastHandoffPath != docPath {
		t.Errorf("LastHandoffPath not set to NewDocPath: %v", rec.LastHandoffPath)
	}
	if rec.HandoffType == nil || *rec.HandoffType != handoff.TypeManual {
		t.Errorf("HandoffType: want manual, got %v", rec.HandoffType)
	}
}

func TestSpawn_RollsBackTmuxOnRecordWriteFailure(t *testing.T) {
	requireTmux(t)
	tmp := setupFleetHome(t)

	// Sabotage the agents/ directory: replace it with a regular file
	// so any record write fails.
	agentsDir := filepath.Join(tmp, "agents")
	if err := os.RemoveAll(agentsDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agentsDir, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Spawn(Options{
		TaskID:  "x",
		Project: "y",
		Command: []string{"sleep", "30"},
	})
	if err == nil {
		t.Fatal("expected Spawn to fail on record write")
	}

	// No tmux sessions should be left behind. List all fleet-* sessions
	// and assert none exist (rollback killed the orphan).
	out, lerr := exec.Command("tmux", "list-sessions", "-F", "#{session_name}").Output()
	if lerr != nil {
		// "no server running" is expected if no other tests are running
		// concurrently — that's the success case.
		if !strings.Contains(string(out), "no server") {
			return
		}
	}
	for _, name := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(name, "fleet-") && !strings.HasPrefix(name, "fleet-test-") {
			t.Errorf("orphan tmux session not cleaned up: %s", name)
		}
	}
}

func TestSpawn_FleetAgentIDInEnv(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)

	cmd := []string{"sh", "-c", "echo AGENT_ID=$FLEET_AGENT_ID; cat"}
	rec, err := Spawn(Options{
		TaskID:  "x",
		Project: "y",
		Command: cmd,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(rec.TmuxSession) })

	// Wait briefly for the shell to print, then capture pane.
	// (No sync primitive — tmux send-keys to the pane is async.)
	out, err := exec.Command("tmux", "capture-pane", "-t", rec.TmuxSession, "-p").Output()
	if err != nil {
		t.Fatalf("capture-pane: %v", err)
	}
	want := "AGENT_ID=" + rec.ID
	if !strings.Contains(string(out), want) {
		t.Errorf("expected %q in pane:\n%s", want, string(out))
	}
}
