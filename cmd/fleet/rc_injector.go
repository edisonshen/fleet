package main

import (
	"fmt"

	"github.com/edisonshen/fleet/internal/rc"
	"github.com/edisonshen/fleet/internal/spawn"
)

func init() {
	wireCoordRCInjector()
}

func wireCoordRCInjector() {
	spawn.CoordRCInjector = func(project, agentID string, argv []string) []string {
		sessionName := spawn.CoordRemoteControlSessionName(agentID, project)
		return rc.GateAttachFlag(project, argv, sessionName)
	}
}

func assertCoordRCInjectorWired() error {
	if spawn.CoordRCInjector == nil {
		return fmt.Errorf("fleet startup: spawn.CoordRCInjector is nil; coord remote-control injection is not wired")
	}
	return nil
}
