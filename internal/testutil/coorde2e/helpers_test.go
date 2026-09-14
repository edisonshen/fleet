package coorde2e_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/testutil/coorde2e"
	"github.com/edisonshen/fleet/internal/testutil/tmuxtest"
	"github.com/edisonshen/fleet/internal/tmux"
)

func isolatedHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("FLEET_HOME", home)
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	return home
}

// A live coord seeded at 41% is exactly the state the soft-handoff hold
// is evaluated in: record carries context_pct + handoff_type=auto-yellow,
// and the pane already shows HANDOFF REQUESTED → ⏺ MILESTONE.
func TestSeedCoord_LiveYellow_PaneShowsMilestone(t *testing.T) {
	tmuxtest.RequireTmux(t)
	isolatedHome(t)
	coorde2e.SeedProject(t, "p")

	rec := coorde2e.SeedCoord(t, "p", coorde2e.SeedCoordOpts{Pct: 41, Command: []string{"sleep", "60"}})

	got, err := agent.Load(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsCoord || got.Project != "p" || got.ContextPct == nil || *got.ContextPct != 41 || got.ContextSource != "hook" {
		t.Fatalf("record: %+v", got)
	}
	if got.HandoffType == nil || *got.HandoffType != "auto-yellow" {
		t.Fatalf("handoff_type = %v, want auto-yellow", got.HandoffType)
	}
	if alive, err := tmux.SessionAlive(rec.TmuxSession); err != nil || !alive {
		t.Fatalf("session %s not alive (err=%v)", rec.TmuxSession, err)
	}
	coorde2e.WaitFor(t, 5*time.Second, "milestone banner in pane", func() bool {
		pane, err := tmux.CapturePane(rec.TmuxSession)
		return err == nil && strings.Contains(string(pane), "⏺ MILESTONE")
	})
	pane := coorde2e.CapturePane(t, rec.TmuxSession)
	if !strings.Contains(pane, "HANDOFF REQUESTED: context window is over 40%") {
		t.Fatalf("pane missing HANDOFF REQUESTED line:\n%s", pane)
	}
}

func TestSeedCoord_Dead_NoSessionOldSpawn(t *testing.T) {
	tmuxtest.RequireTmux(t)
	isolatedHome(t)
	coorde2e.SeedProject(t, "p")

	rec := coorde2e.SeedCoord(t, "p", coorde2e.SeedCoordOpts{Dead: true})

	if alive, _ := tmux.SessionAlive(rec.TmuxSession); alive {
		t.Fatalf("dead coord must not have a session, %s is alive", rec.TmuxSession)
	}
	if age := time.Since(rec.SpawnedAt); age < 71*time.Hour {
		t.Fatalf("dead coord spawned_at only %s ago", age)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("FLEET_HOME"), "agents", rec.ID+".json")); err != nil {
		t.Fatalf("record not on disk: %v", err)
	}
}

func TestWriteHookEvent_StopThenPromptSubmit(t *testing.T) {
	tmuxtest.IsolateSocket(t)
	isolatedHome(t)
	coorde2e.SeedProject(t, "p")
	rec := coorde2e.SeedCoord(t, "p", coorde2e.SeedCoordOpts{Dead: true})

	coorde2e.WriteHookEvent(t, "p", 41, "Stop")
	got, err := agent.Load(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ContextPct == nil || *got.ContextPct != 41 || got.ContextSource != "hook" || !got.NeedsInput {
		t.Fatalf("after Stop: pct=%v src=%q needs_input=%v", got.ContextPct, got.ContextSource, got.NeedsInput)
	}

	coorde2e.WriteHookEvent(t, "p", 42, "UserPromptSubmit")
	got, err = agent.Load(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if *got.ContextPct != 42 || got.NeedsInput {
		t.Fatalf("after UserPromptSubmit: pct=%v needs_input=%v", *got.ContextPct, got.NeedsInput)
	}
}
