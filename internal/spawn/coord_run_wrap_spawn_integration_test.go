//go:build integration

// Integration-lane lifecycle tests that launch a REAL lease-wrapped standby
// (`coord-run --standby` tmux pane). Fenced out of the default `go test ./...`
// lane by PR-A's fork-bomb fix: an orphaned default-lane run of these used to
// leave 10-minute standby panes piling up forks until the box couldn't fork().
// Here each sets FLEET_STANDBY_TIMEOUT=3s so even an orphaned run self-reaps in
// seconds, and FLEET_LEASE_FAILOVER=1 to override the TestMain default-OFF guard.
// See docs/DESIGN-spawn-test-fork-bomb-root-fix.md.
package spawn

import (
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/tmux"
)

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
	t.Setenv("FLEET_STANDBY_TIMEOUT", "3s")

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
	t.Setenv("FLEET_STANDBY_TIMEOUT", "3s")

	// Test #1b (counter mechanics, POSITIVE half): a real lease-wrapped coord
	// whose tmux.Spawn succeeds increments the standby-launch counter by exactly
	// one. (The negative half — rollback does NOT increment — is pinned in the
	// default-lane TestSpawn_RollbackDoesNotIncrementStandbyCounter.)
	beforeCount := StandbyLaunchCount()

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
	if got := StandbyLaunchCount() - beforeCount; got != 1 {
		t.Fatalf("standby-launch counter delta = %d after a successful lease-wrapped spawn, want 1", got)
	}
	if got, lerr := agent.Load(wrapped.ID); lerr != nil || !got.LeaseWrapped {
		t.Fatalf("persisted LeaseWrapped=%v (err=%v), want true", got.LeaseWrapped, lerr)
	}

	// DisableLeaseWrap (drain cold-resume) coord -> LeaseWrapped false.
	beforeBare := StandbyLaunchCount()
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
	if got := StandbyLaunchCount() - beforeBare; got != 0 {
		t.Fatalf("standby-launch counter delta = %d after a DisableLeaseWrap (bare) spawn, want 0", got)
	}
	if got, lerr := agent.Load(bare.ID); lerr != nil || got.LeaseWrapped {
		t.Fatalf("persisted bare LeaseWrapped=%v (err=%v), want false", got.LeaseWrapped, lerr)
	}
}
