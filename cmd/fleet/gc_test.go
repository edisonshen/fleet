package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/edisonshen/fleet/internal/gc"
)

// Pure-CLI-shell coverage for cmd/fleet/gc.go. The reconciliation
// logic itself lives in internal/gc/gc_test.go — these tests focus on
// the flag-parsing + report-rendering surface that the package boundary
// makes hard to exercise from the gc package alone.

func TestParseKindsCSV_Empty_DefaultsToAllKinds(t *testing.T) {
	got, err := parseKindsCSV("")
	if err != nil {
		t.Fatalf("parseKindsCSV: %v", err)
	}
	if len(got) != len(gc.AllKinds) {
		t.Fatalf("got %v, want %v", got, gc.AllKinds)
	}
}

func TestParseKindsCSV_Subset(t *testing.T) {
	got, err := parseKindsCSV("sockets,worktrees")
	if err != nil {
		t.Fatalf("parseKindsCSV: %v", err)
	}
	if len(got) != 2 || got[0] != gc.KindSockets || got[1] != gc.KindWorktrees {
		t.Fatalf("got %v, want [sockets worktrees]", got)
	}
}

func TestParseKindsCSV_AcceptsCoordLocks(t *testing.T) {
	// fleet#172: --kinds=coord-locks must parse cleanly. Empty-default
	// (AllKinds) inclusion is covered by TestParseKindsCSV_Empty.
	got, err := parseKindsCSV("coord-locks")
	if err != nil {
		t.Fatalf("parseKindsCSV: %v", err)
	}
	if len(got) != 1 || got[0] != gc.KindCoordLocks {
		t.Fatalf("got %v, want [coord-locks]", got)
	}
}

func TestParseKindsCSV_UnknownRejected(t *testing.T) {
	if _, err := parseKindsCSV("sockets,foo"); err == nil {
		t.Fatal("parseKindsCSV accepted unknown kind 'foo'")
	}
}

func TestParseKindsCSV_TrimsWhitespace(t *testing.T) {
	got, err := parseKindsCSV(" sockets , orphan-tmux ")
	if err != nil {
		t.Fatalf("parseKindsCSV: %v", err)
	}
	if len(got) != 2 || got[0] != gc.KindSockets || got[1] != gc.KindOrphanTmux {
		t.Fatalf("got %v", got)
	}
}

func TestRenderReport_FormatPinned(t *testing.T) {
	// Output contract from the design doc — pin it so a careless edit
	// doesn't break the operator's eyeballed dry-run output.
	var buf bytes.Buffer
	renderReport(&buf, gc.Options{Apply: false, Aggressive: false, MaxAge: 0, Kinds: gc.AllKinds},
		gc.Report{Actions: []gc.Action{
			{Kind: gc.KindSockets, Target: "/tmp/fleet-test-abcdef.sock", Verb: gc.VerbWouldRemove, Reason: "age=2d"},
			{Kind: gc.KindOrphanAgents, Target: "deadbeef", Verb: gc.VerbWouldArchive, Reason: "tmux gone"},
			{Kind: gc.KindOrphanTmux, Target: "fleet-aaaaaaaa", Verb: gc.VerbSurface, Reason: "no agent record"},
			{Kind: gc.KindWorktrees, Target: "/wt/old", Verb: gc.VerbWouldRemove, Reason: "task done"},
			{Kind: gc.KindCoordLocks, Target: "/fake/projects/p/.locks/coordinator.lock",
				Verb: gc.VerbSurface, Reason: "dead coord (agent record deadbeef missing)"},
		}})
	out := buf.String()
	for _, want := range []string{
		"mode=dry-run",
		"sockets  /tmp/fleet-test-abcdef.sock  verb=would-remove  reason=age=2d",
		"orphan-agents  deadbeef  verb=would-archive  reason=tmux gone",
		"orphan-tmux  fleet-aaaaaaaa  verb=surface  reason=no agent record",
		"worktrees  /wt/old  verb=would-remove  reason=task done",
		"coord-locks  /fake/projects/p/.locks/coordinator.lock  verb=surface  reason=dead coord (agent record deadbeef missing)",
		"summary: 1 sockets, 1 agents, 1 tmux (surface only by default), 1 worktrees, 1 coord-locks",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("renderReport output missing %q\n--- got ---\n%s", want, out)
		}
	}
}

func TestRenderReport_ApplyMode(t *testing.T) {
	var buf bytes.Buffer
	renderReport(&buf, gc.Options{Apply: true, Aggressive: true, MaxAge: 0, Kinds: gc.AllKinds},
		gc.Report{})
	out := buf.String()
	if !strings.Contains(out, "mode=apply") {
		t.Errorf("apply mode missing in output:\n%s", out)
	}
	if !strings.Contains(out, "aggressive=true") {
		t.Errorf("aggressive flag missing in output:\n%s", out)
	}
}

func TestGCCmd_RegisteredOnRoot(t *testing.T) {
	// Defensive: the dispatch wiring is one line in main.go; this test
	// guards against an accidental delete.
	root := newRootCmd()
	for _, sub := range root.Commands() {
		if sub.Use == "gc" {
			return
		}
	}
	t.Fatal("gc subcommand not registered on root")
}
