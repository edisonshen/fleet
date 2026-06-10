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
	"time"

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

// codex PR3 iter-14 [P2]: if `coord-run --standby` acquires the lease and stamps
// the REAL engine pid (agent.StampEnginePID) into the on-disk record BEFORE
// spawn.Spawn's final locked merge runs, the merge must NOT clobber it back to
// the provisional spawning-CLI pid. Simulate the concurrent stamp by writing a
// sentinel engine pid to the pre-launch record while Spawn is mid-flight, then
// assert the final record preserves it.
func TestSpawn_StandbyFinalMergePreservesStampedEnginePID(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)
	t.Setenv("FLEET_LEASE_FAILOVER", "1")

	const id = "sbmerge1"
	const stampedPid = 4242424 // sentinel "real engine pid" a standby would stamp

	old := agent.New("oldsbmerge")
	old.Project = "p"
	old.TaskID = "coord-p" // isCoordSpawn
	old.Cwd = t.TempDir()
	old.Command = []string{"sh", "-c", "sleep 60"}

	// Concurrently stamp a sentinel engine pid once the pre-launch record lands,
	// racing Spawn's final merge — exactly the standby-acquires-mid-spawn window.
	stamped := make(chan struct{})
	go func() {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := agent.Load(id); err == nil {
				if serr := agent.StampEnginePID(id, stampedPid); serr == nil {
					close(stamped)
					return
				}
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	rec, err := Spawn(Options{
		OldRecord:      old,
		TaskID:         "coord-p",
		Project:        "p",
		Cwd:            old.Cwd,
		Command:        old.Command,
		PreAllocatedID: id,
	})
	if err != nil {
		t.Fatalf("standby Spawn: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(rec.TmuxSession) })

	select {
	case <-stamped:
	case <-time.After(2 * time.Second):
		t.Skip("could not stamp before Spawn finished (timing); merge-preservation not exercised")
	}

	// The final on-disk record must carry the stamped engine pid, not the
	// provisional os.Getpid clobbered back over it.
	got, err := agent.Load(id)
	if err != nil {
		t.Fatalf("Load final record: %v", err)
	}
	if got.PID != stampedPid {
		t.Errorf("final standby record PID = %d, want the stamped engine pid %d (merge clobbered the live pid back to provisional)",
			got.PID, stampedPid)
	}
}

// codex iter-24 [P2]: Spawn must persist the ACTUAL lease-wrap state on the
// record so crash-recovery retry paths read the truth (not the producer's
// cap-approval bit). A lease-wrapped coord records LeaseWrapped=true; a coord
// spawned with DisableLeaseWrap (the drain cold-resume path) records false.
func TestSpawn_PersistsLeaseWrappedState(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)
	t.Setenv("FLEET_LEASE_FAILOVER", "1")

	// Lease-wrapped coord -> LeaseWrapped true.
	wrapped, err := Spawn(Options{
		TaskID:         "coord-lwp",
		Project:        "lwp",
		Cwd:            t.TempDir(),
		Command:        []string{"sh", "-c", "sleep 30"},
		PreAllocatedID: "lwcoord1",
	})
	if err != nil {
		t.Fatalf("spawn lease-wrapped coord: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(wrapped.TmuxSession) })
	if !wrapped.LeaseWrapped {
		t.Fatal("lease-wrapped coord record LeaseWrapped=false, want true")
	}
	if got, lerr := agent.Load(wrapped.ID); lerr != nil || !got.LeaseWrapped {
		t.Fatalf("persisted LeaseWrapped=%v (err=%v), want true", got.LeaseWrapped, lerr)
	}

	// DisableLeaseWrap (drain cold-resume) coord -> LeaseWrapped false.
	bare, err := Spawn(Options{
		TaskID:           "coord-lwp",
		Project:          "lwp",
		Cwd:              t.TempDir(),
		Command:          []string{"sh", "-c", "sleep 30"},
		PreAllocatedID:   "barecoord1",
		DisableLeaseWrap: true,
	})
	if err != nil {
		t.Fatalf("spawn bare coord: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(bare.TmuxSession) })
	if bare.LeaseWrapped {
		t.Fatal("DisableLeaseWrap coord record LeaseWrapped=true, want false")
	}
	if got, lerr := agent.Load(bare.ID); lerr != nil || got.LeaseWrapped {
		t.Fatalf("persisted bare LeaseWrapped=%v (err=%v), want false", got.LeaseWrapped, lerr)
	}
}
