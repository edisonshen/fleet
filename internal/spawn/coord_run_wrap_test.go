// Tests for the lease-failover coord-run wrap applied inside spawn.Spawn
// (DESIGN-handoff-drain-storm-leak PR2). The wrap lives in spawn — not at
// the dispatch call site — so EVERY coord spawn (fresh dispatch + the
// handoff/drain replacements) gets the lease supervisor (codex PR2 iter-1
// [P1]). These exercise the pure decision + builder so they stay
// deterministic without a real tmux server:
//
//	W1  coordRunWrap shape: head = [fleet coord-run --agent <id>
//	    --project <p> --], tail = the engine argv.
//	W2  flag OFF -> leaseFailoverEnabled() false -> no wrap.
//	W3  alreadyCoordRunWrapped guards against double/nested wrapping.
package spawn

import (
	"reflect"
	"testing"
)

func TestCoordRunWrap_Shape(t *testing.T) {
	const (
		fleetBin = "/usr/local/bin/fleet"
		agentID  = "deadbeef"
		project  = "projects-fleet"
	)
	engine := []string{"sh", "-c", "claude --dangerously-skip-permissions"}
	got := coordRunWrap(fleetBin, agentID, project, false, engine)

	wantHead := []string{fleetBin, "coord-run", "--agent", agentID, "--project", project, "--"}
	if len(got) < len(wantHead) {
		t.Fatalf("wrapped argv too short: %v", got)
	}
	if !reflect.DeepEqual(got[:len(wantHead)], wantHead) {
		t.Errorf("argv head = %v, want %v", got[:len(wantHead)], wantHead)
	}
	if tail := got[len(wantHead):]; !reflect.DeepEqual(tail, engine) {
		t.Errorf("argv tail = %v, want %v", tail, engine)
	}
}

func TestCoordRunWrap_DoesNotMutateInput(t *testing.T) {
	engine := []string{"sh", "-c", "claude"}
	orig := append([]string(nil), engine...)
	_ = coordRunWrap("/bin/fleet", "id1", "p1", false, engine)
	if !reflect.DeepEqual(engine, orig) {
		t.Errorf("input mutated: got %v want %v", engine, orig)
	}
}

// PR3 (warm standby): standby=true inserts `--standby` between the project
// flag and the `--` separator so the supervisor polls the busy lease.
func TestCoordRunWrap_StandbyInsertsFlag(t *testing.T) {
	const (
		fleetBin = "/usr/local/bin/fleet"
		agentID  = "sb-deadbeef"
		project  = "projects-fleet"
	)
	engine := []string{"sh", "-c", "claude"}
	got := coordRunWrap(fleetBin, agentID, project, true, engine)

	want := []string{fleetBin, "coord-run", "--agent", agentID, "--project", project, "--standby", "--"}
	want = append(want, engine...)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("standby wrap = %v, want %v", got, want)
	}
}

// W2: the gate is OFF unless FLEET_LEASE_FAILOVER selects it.
func TestLeaseFailoverEnabled_Gate(t *testing.T) {
	cases := map[string]bool{"": false, "0": false, "false": false, "1": true, "yes": true, "on": true}
	for v, want := range cases {
		t.Setenv("FLEET_LEASE_FAILOVER", v)
		if got := leaseFailoverEnabled(); got != want {
			t.Errorf("FLEET_LEASE_FAILOVER=%q -> %v, want %v", v, got, want)
		}
	}
}

// codex PR2 iter-7/8 [P1]: FRESH coord spawns AND dead-coord RECOVERIES
// are lease-wrapped (both are fresh leaders); only a LIVE handoff/drain
// successor is NOT (the outgoing coord still holds the lease, so a wrapped
// successor would stand down and never start). The dead-coord recovery
// case is exercised here via liveHandoffSuccessor=false (the dispatch
// call computes OldRecord!=nil && !RecoverDeadCoord).
func TestShouldLeaseWrap(t *testing.T) {
	cases := []struct {
		name                                      string
		failoverOn, isCoord, liveHandoffSuccessor bool
		want                                      bool
	}{
		{"fresh coord, flag on", true, true, false, true},
		{"dead-coord recovery (not a live successor)", true, true, false, true},
		{"live handoff successor, flag on", true, true, true, false},
		{"flag off", false, true, false, false},
		{"worker (not coord)", true, false, false, false},
		{"live handoff worker", true, false, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldLeaseWrap(c.failoverOn, c.isCoord, c.liveHandoffSuccessor); got != c.want {
				t.Errorf("shouldLeaseWrap(%v,%v,%v) = %v, want %v",
					c.failoverOn, c.isCoord, c.liveHandoffSuccessor, got, c.want)
			}
		})
	}
}

// W3: the idempotency guard recognizes an already-wrapped argv so a
// re-spawn never nests coord-run inside coord-run.
func TestAlreadyCoordRunWrapped(t *testing.T) {
	yes := [][]string{
		{"/usr/local/bin/fleet", "coord-run", "--agent", "x", "--", "sh"},
		{"/tmp/go-build/exe/fleet.test", "coord-run", "--", "true"},
	}
	no := [][]string{
		{"sh", "-c", "claude"},
		{"/usr/local/bin/fleet", "dispatch", "task"},
		{"/usr/local/bin/claude"},
		{},
		{"fleet"},
	}
	for _, a := range yes {
		if !alreadyCoordRunWrapped(a) {
			t.Errorf("alreadyCoordRunWrapped(%v) = false, want true", a)
		}
	}
	for _, a := range no {
		if alreadyCoordRunWrapped(a) {
			t.Errorf("alreadyCoordRunWrapped(%v) = true, want false", a)
		}
	}
}
