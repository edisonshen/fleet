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
	got := coordRunWrap(fleetBin, agentID, project, engine)

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
	_ = coordRunWrap("/bin/fleet", "id1", "p1", engine)
	if !reflect.DeepEqual(engine, orig) {
		t.Errorf("input mutated: got %v want %v", engine, orig)
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
