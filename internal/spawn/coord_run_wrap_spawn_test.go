// Lifecycle regression tests for the lease-supervised pre-launch record
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
)

func TestSpawn_LeaseCoord_RollsBackPrelaunchRecordOnTmuxFailure(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)
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

// TestSpawn_StandbyFinalMergePreservesStampedEnginePID and
// TestSpawn_PersistsLeaseWrappedState launch a REAL lease-wrapped standby and so
// live in the integration lane (coord_run_wrap_spawn_integration_test.go,
// //go:build integration) per PR-A's fork-bomb fence. The DisableLeaseWrap
// no-standby sub-case rides along inside PersistsLeaseWrappedState there.
