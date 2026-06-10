//go:build linux || darwin

package main

import (
	"os"
	"testing"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/coord"
)

func TestSweepStaleCompetitorsReapsUnstampedLosingStandby(t *testing.T) {
	setupFleetHome(t)
	const project = "rainier"
	self := agent.New("winner1")
	self.Project = project
	self.TaskID = "coord-" + project
	self.SupervisorPID = os.Getpid()
	self.TmuxSession = "fleet-winner1"
	if err := self.Write(); err != nil {
		t.Fatalf("write self: %v", err)
	}
	loser := agent.New("loser1")
	loser.Project = project
	loser.TaskID = "coord-" + project
	loser.TmuxSession = "fleet-loser1"
	loser.LeaseWrapped = true // a lease-wrapped standby that lost the race
	if err := loser.Write(); err != nil {
		t.Fatalf("write loser: %v", err)
	}

	origKillCoord := sweepKillCoordFn
	origKillSession := sweepKillSessionFn
	var killedSession string
	var coordKills int
	t.Cleanup(func() {
		sweepKillCoordFn = origKillCoord
		sweepKillSessionFn = origKillSession
	})
	sweepKillCoordFn = func(coord.KillTarget) error {
		coordKills++
		return nil
	}
	sweepKillSessionFn = func(session string) error {
		killedSession = session
		return nil
	}

	sweepStaleCompetitors(self.ID, project, os.Stderr)

	if killedSession != loser.TmuxSession {
		t.Fatalf("killed session = %q, want losing standby %q", killedSession, loser.TmuxSession)
	}
	if coordKills != 0 {
		t.Fatalf("authenticated coord kill calls = %d, want 0 for unstamped standby", coordKills)
	}
}

// TestSweepStaleCompetitorsSpareLegacyBareCoord pins codex iter-28 P1: a live
// LEGACY/BARE coord (no supervisor, so SupervisorPID==0 and LeaseWrapped==false)
// must NOT be reaped by the unstamped-standby sweep. SupervisorPID==0 alone is
// ambiguous — it matches both a lease-wrapped standby that lost the race (safe
// to reap) AND a bare coord that may be the only working coordinator. Blindly
// killing the bare coord's session bypasses the handoff readiness/rollback path
// and can strand the project coord-less. LeaseWrapped is the discriminator.
func TestSweepStaleCompetitorsSpareLegacyBareCoord(t *testing.T) {
	setupFleetHome(t)
	const project = "rainier"
	self := agent.New("winner1")
	self.Project = project
	self.TaskID = "coord-" + project
	self.SupervisorPID = os.Getpid()
	self.LeaseWrapped = true
	self.TmuxSession = "fleet-winner1"
	if err := self.Write(); err != nil {
		t.Fatalf("write self: %v", err)
	}
	// A bare/legacy coord: real coord-spawn record, live session, but never
	// ran a supervisor (SupervisorPID==0, LeaseWrapped==false).
	bare := agent.New("barecord")
	bare.Project = project
	bare.TaskID = "coord-" + project
	bare.TmuxSession = "fleet-barecord"
	bare.LeaseWrapped = false // legacy/bare — NOT a lease-wrapped standby
	if err := bare.Write(); err != nil {
		t.Fatalf("write bare: %v", err)
	}

	origKillCoord := sweepKillCoordFn
	origKillSession := sweepKillSessionFn
	var killedSession string
	var coordKills int
	t.Cleanup(func() {
		sweepKillCoordFn = origKillCoord
		sweepKillSessionFn = origKillSession
	})
	sweepKillCoordFn = func(coord.KillTarget) error {
		coordKills++
		return nil
	}
	sweepKillSessionFn = func(session string) error {
		killedSession = session
		return nil
	}

	sweepStaleCompetitors(self.ID, project, os.Stderr)

	if killedSession != "" {
		t.Fatalf("sweep killed session %q; a legacy/bare coord (LeaseWrapped=false) must be spared", killedSession)
	}
	if coordKills != 0 {
		t.Fatalf("authenticated coord kill calls = %d, want 0 (bare coord must not be swept)", coordKills)
	}
}
