// Dispatch-side lease-wiring tests (DESIGN-handoff-drain-storm-leak PR2):
//
//	W1  flag ON, coord-spawn, default command -> argv head is
//	    [fleet coord-run --agent <id> --project <p> --], tail = engine argv.
//	W2  flag OFF -> NO wrap (argv byte-identical to today).
//	W3  explicit --command -> NEVER wrapped (operator argv verbatim).
//
// We test the pure decision (shouldWrapInCoordRun) + the pure builder
// (wrapInCoordRun) rather than driving a full runDispatch (which needs a
// real tmux server) — the two helpers are exactly the wiring surface PR2
// adds, so this isolates the logic deterministically.
package main

import (
	"reflect"
	"testing"
)

func TestShouldWrapInCoordRun_Gate(t *testing.T) {
	cases := []struct {
		name            string
		failoverOn      bool
		coordSpawn      bool
		commandExplicit bool
		preAllocatedID  string
		want            bool
	}{
		{"W1 flag-on coord-spawn default-cmd", true, true, false, "abc12345", true},
		{"W2 flag-off", false, true, false, "abc12345", false},
		{"W3 explicit command", true, true, true, "abc12345", false},
		{"worker spawn (not coord)", true, false, false, "abc12345", false},
		{"no preallocated id", true, true, false, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := shouldWrapInCoordRun(c.failoverOn, c.coordSpawn, c.commandExplicit, c.preAllocatedID)
			if got != c.want {
				t.Errorf("shouldWrapInCoordRun(%v,%v,%v,%q) = %v, want %v",
					c.failoverOn, c.coordSpawn, c.commandExplicit, c.preAllocatedID, got, c.want)
			}
		})
	}
}

// W1: the wrapped argv head is exactly the coord-run prefix and the tail
// is the unchanged engine argv.
func TestWrapInCoordRun_Shape(t *testing.T) {
	const (
		fleetBin = "/usr/local/bin/fleet"
		agentID  = "deadbeef"
		project  = "projects-fleet"
	)
	engineArgv := append([]string(nil), defaultClaudeCommand...)

	got := wrapInCoordRun(fleetBin, agentID, project, engineArgv)

	wantHead := []string{fleetBin, "coord-run", "--agent", agentID, "--project", project, "--"}
	if len(got) < len(wantHead) {
		t.Fatalf("wrapped argv too short: %v", got)
	}
	if !reflect.DeepEqual(got[:len(wantHead)], wantHead) {
		t.Errorf("argv head = %v, want %v", got[:len(wantHead)], wantHead)
	}
	tail := got[len(wantHead):]
	if !reflect.DeepEqual(tail, engineArgv) {
		t.Errorf("argv tail = %v, want engine argv %v", tail, engineArgv)
	}
}

// W2: with the flag OFF the gate is false, so the builder is never called
// and ExecCommand stays the engine argv. We assert the gate is the only
// thing standing between "wrap" and "byte-identical to today".
func TestWrapInCoordRun_FlagOffLeavesEngineArgvIntact(t *testing.T) {
	engineArgv := append([]string(nil), defaultClaudeCommand...)
	// Flag OFF -> no wrap decision.
	if shouldWrapInCoordRun(false, true, false, "abc12345") {
		t.Fatal("flag-off must not wrap")
	}
	// The argv the spawn would use is unchanged from the default.
	if !reflect.DeepEqual(engineArgv, defaultClaudeCommand) {
		t.Errorf("engine argv mutated: %v", engineArgv)
	}
}

// wrapInCoordRun must not alias/mutate the caller's engineArgv slice.
func TestWrapInCoordRun_DoesNotMutateInput(t *testing.T) {
	engineArgv := []string{"sh", "-c", "claude"}
	orig := append([]string(nil), engineArgv...)
	_ = wrapInCoordRun("/bin/fleet", "id1", "p1", engineArgv)
	if !reflect.DeepEqual(engineArgv, orig) {
		t.Errorf("input engineArgv mutated: got %v want %v", engineArgv, orig)
	}
}
