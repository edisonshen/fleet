package main

import (
	"strings"
	"testing"

	"github.com/edisonshen/fleet/internal/rc"
	"github.com/edisonshen/fleet/internal/spawn"
)

func TestCoordRCInjectorWiredAtStartup(t *testing.T) {
	if err := assertCoordRCInjectorWired(); err != nil {
		t.Fatal(err)
	}
}

func TestAssertCoordRCInjectorWiredFailsLoud(t *testing.T) {
	prev := spawn.CoordRCInjector
	spawn.CoordRCInjector = nil
	t.Cleanup(func() { spawn.CoordRCInjector = prev })

	if err := assertCoordRCInjectorWired(); err == nil {
		t.Fatal("expected nil CoordRCInjector to fail startup assertion")
	}
}

func TestCoordRCInjectorUsesProjectGate(t *testing.T) {
	fleetHome := t.TempDir()
	t.Setenv("FLEET_HOME", fleetHome)
	t.Setenv("FLEET_RC_BOOTSTRAP_DISABLED", "")
	wireCoordRCInjector()

	argv := []string{"sh", "-c", "claude --print"}
	got := spawn.CoordRCInjector("demo", "deadbeef", argv)
	if !strings.Contains(strings.Join(got, " "), `--remote-control "fleet-coord-deadbeef-demo"`) {
		t.Fatalf("wired injector missing coord session name: %v", got)
	}

	if err := rc.WriteDisabledMarker("demo"); err != nil {
		t.Fatalf("WriteDisabledMarker: %v", err)
	}
	if got := spawn.CoordRCInjector("demo", "deadbeef", argv); !spawn.SameCommand(got, argv) {
		t.Fatalf("rc-disabled marker must suppress wired injector; got %v", got)
	}
	if err := rc.RemoveDisabledMarker("demo"); err != nil {
		t.Fatalf("RemoveDisabledMarker: %v", err)
	}

	t.Setenv("FLEET_RC_BOOTSTRAP_DISABLED", "1")
	if got := spawn.CoordRCInjector("demo", "deadbeef", argv); !spawn.SameCommand(got, argv) {
		t.Fatalf("env gate must suppress wired injector; got %v", got)
	}
}
