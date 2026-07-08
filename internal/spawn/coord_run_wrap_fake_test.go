package spawn_test

import (
	"reflect"
	"testing"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/spawn"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/testutil/tmuxfake"
)

func TestSpawnFakeTmuxCoordStaysBare(t *testing.T) {
	f := tmuxfake.InstallFake(t)
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	before := spawn.StandbyLaunchCount()

	command := []string{"sh", "-c", "sleep 30"}
	rec, err := spawn.Spawn(spawn.Options{
		TaskID:         "coord-myproj",
		Project:        "myproj",
		Cwd:            t.TempDir(),
		Command:        command,
		PreAllocatedID: "fakec001",
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if rec.LeaseWrapped {
		t.Fatal("fake-backed coord spawn recorded LeaseWrapped=true, want false")
	}
	if got := spawn.StandbyLaunchCount() - before; got != 0 {
		t.Fatalf("standby-launch counter delta = %d, want 0 for fake tmux", got)
	}
	if got := f.SessionCommand(rec.TmuxSession); !reflect.DeepEqual(got, command) {
		t.Fatalf("fake session command = %v, want bare command %v", got, command)
	}
	loaded, err := agent.Load(rec.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.LeaseWrapped {
		t.Fatal("persisted fake-backed coord LeaseWrapped=true, want false")
	}
}
