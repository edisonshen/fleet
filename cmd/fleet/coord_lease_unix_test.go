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
