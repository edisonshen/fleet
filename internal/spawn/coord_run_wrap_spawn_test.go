// Lifecycle regression tests for the lease-failover pre-launch record
// write in spawn.Spawn (codex PR2 iter-3 [P2]):
//
//	#1  tmux.Spawn fails after the pre-launch write -> the pre-launch
//	    record must be rolled back (no live record for a session that
//	    never came up). fleet-owns-its-resources.
//
// The do-not-resurrect case (#2, an archived record at merge time on a
// stand-down) is covered end-to-end by the cmd/fleet integration test
// (it needs the real fleet binary to run a coord-run that stands down +
// archives). Here we pin the rollback path, which is deterministic via a
// forced tmux.Spawn failure (nonexistent cwd).
package spawn

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tmux"
)

func TestSpawn_LeaseCoord_RollsBackPrelaunchRecordOnTmuxFailure(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)
	t.Setenv("FLEET_LEASE_FAILOVER", "1")
	// Force tmux.Spawn to fail by pointing the socket at an over-long path
	// (exceeds the UNIX socket limit). Canonical fleet-test-* prefix so
	// the runtime sink guard lets us through (mirrors
	// TestSpawn_FailsWhenSocketUnusable). This fails tmux.Spawn AFTER our
	// pre-launch record write, exercising the rollback.
	t.Setenv("FLEET_TMUX_SOCKET", "/tmp/fleet-test-"+strings.Repeat("a", 200)+".sock")

	const id = "rollbk01"
	_, err := Spawn(Options{
		TaskID:         "coord-p", // isCoordSpawn
		Project:        "p",
		Cwd:            t.TempDir(),
		Command:        []string{"sleep", "30"},
		PreAllocatedID: id,
	})
	if err == nil {
		t.Fatal("expected Spawn to fail with an unusable tmux socket")
	}

	// The pre-launch record must NOT be left behind for a session that
	// never came up.
	if _, lerr := agent.Load(id); !errors.Is(lerr, state.ErrNotFound) {
		livePath, _ := state.AgentPath(id)
		_, statErr := os.Stat(livePath)
		t.Errorf("pre-launch record for %s must be rolled back; Load err=%v (stat err=%v)",
			id, lerr, statErr)
	}
}

// codex PR3 iter-12 [P2]: a WARM-STANDBY spawn must NOT run the engine-pid
// resolver — the wrapped `coord-run --standby` supervisor does not launch the
// engine until it acquires the lease, so the pane tree is supervisor-only and
// resolving would corrupt the PID==engine vs SupervisorPID==lease-holder split.
// The standby record keeps a PROVISIONAL pid (os.Getpid, the fleet binary), to
// be re-stamped by the supervisor once it acquires + starts the engine.
func TestSpawn_StandbySkipsEnginePidResolution(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)
	t.Setenv("FLEET_LEASE_FAILOVER", "1")

	old := agent.New("oldstandby")
	old.Project = "p"
	old.TaskID = "coord-p" // isCoordSpawn
	old.Cwd = t.TempDir()
	old.Command = []string{"sh", "-c", "sleep 60"}

	rec, err := Spawn(Options{
		OldRecord: old,
		TaskID:    "coord-p",
		Project:   "p",
		Cwd:       old.Cwd,
		Command:   old.Command,
		Standby:   true,
	})
	if err != nil {
		t.Fatalf("standby Spawn: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(rec.TmuxSession) })

	// Provisional pid (os.Getpid) — the resolver was SKIPPED. A resolved pane
	// pid would be the supervisor's, breaking the split.
	if rec.PID != os.Getpid() {
		t.Errorf("standby rec.PID = %d, want the provisional fleet-binary pid %d (resolver must be skipped)",
			rec.PID, os.Getpid())
	}
}
