package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/gc"
	"github.com/edisonshen/fleet/internal/state"
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

func TestParseKindsCSV_AcceptsWorkerRecords(t *testing.T) {
	// fleet#177: --kinds=worker-records must parse cleanly. Empty-default
	// (AllKinds) inclusion is covered by TestParseKindsCSV_Empty.
	got, err := parseKindsCSV("worker-records")
	if err != nil {
		t.Fatalf("parseKindsCSV: %v", err)
	}
	if len(got) != 1 || got[0] != gc.KindWorkerRecords {
		t.Fatalf("got %v, want [worker-records]", got)
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
			{Kind: gc.KindWorkerRecords, Target: "/fake/projects/p/workers/stale-aaaa/",
				Verb: gc.VerbSurface, Reason: "stale-heartbeat (pid=0, last update 25h ago)"},
		}})
	out := buf.String()
	for _, want := range []string{
		"mode=dry-run",
		"sockets  /tmp/fleet-test-abcdef.sock  verb=would-remove  reason=age=2d",
		"orphan-agents  deadbeef  verb=would-archive  reason=tmux gone",
		"orphan-tmux  fleet-aaaaaaaa  verb=surface  reason=no agent record",
		"worktrees  /wt/old  verb=would-remove  reason=task done",
		"coord-locks  /fake/projects/p/.locks/coordinator.lock  verb=surface  reason=dead coord (agent record deadbeef missing)",
		"worker-records  /fake/projects/p/workers/stale-aaaa/  verb=surface  reason=stale-heartbeat (pid=0, last update 25h ago)",
		"summary: 1 sockets, 1 agents, 1 tmux (surface only by default), 1 worktrees, 1 coord-locks, 1 worker-records",
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

// TestRunGC_CoordLocks_MultiSocketWarning pins codex review iter-2
// [P2]: when FLEET_TMUX_SOCKET is set AND --apply AND coord-locks is
// in the active kinds list, runGC must emit a stderr warning. The
// classifier's SessionAlive probes use the current socket; a healthy
// coord spawned against a different socket can look stale-tmux (or
// dead-coord with no record) and the unlink path can remove its live
// lock. Mirrors the existing orphan-agents warning shape.
func TestRunGC_CoordLocks_MultiSocketWarning(t *testing.T) {
	t.Setenv("FLEET_TMUX_SOCKET", "/tmp/fleet-other.sock")
	t.Setenv("FLEET_HOME", t.TempDir()) // isolate from operator's real state
	var stdout, stderr bytes.Buffer
	err := runGC(&stdout, &stderr, &gcFlags{
		apply:    true,
		maxAge:   gcDefaultMaxAge,
		kindsCSV: "coord-locks",
	})
	if err != nil {
		t.Fatalf("runGC: %v", err)
	}
	se := stderr.String()
	if !strings.Contains(se, "FLEET_TMUX_SOCKET=/tmp/fleet-other.sock") {
		t.Errorf("stderr should name FLEET_TMUX_SOCKET; got:\n%s", se)
	}
	if !strings.Contains(se, "coord-locks") {
		t.Errorf("stderr should name coord-locks; got:\n%s", se)
	}
	if !strings.Contains(se, "different socket") {
		t.Errorf("stderr should explain cross-socket false-stale risk; got:\n%s", se)
	}
}

// TestRunGC_CoordLocks_NoWarning_WhenSocketUnset is the negative
// invariant: with FLEET_TMUX_SOCKET unset (default), runGC must NOT
// emit the coord-locks warning even on --apply --kinds=coord-locks.
func TestRunGC_CoordLocks_NoWarning_WhenSocketUnset(t *testing.T) {
	t.Setenv("FLEET_TMUX_SOCKET", "")
	t.Setenv("FLEET_HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	err := runGC(&stdout, &stderr, &gcFlags{
		apply:    true,
		maxAge:   gcDefaultMaxAge,
		kindsCSV: "coord-locks",
	})
	if err != nil {
		t.Fatalf("runGC: %v", err)
	}
	if strings.Contains(stderr.String(), "FLEET_TMUX_SOCKET") {
		t.Errorf("stderr should not mention FLEET_TMUX_SOCKET when unset; got:\n%s", stderr.String())
	}
}

// dropKind unit coverage: removes the target, preserves order, and never
// aliases the caller's backing array.
func TestDropKind(t *testing.T) {
	in := []gc.Kind{gc.KindSockets, gc.KindWorktrees, gc.KindCoordLocks}
	out := dropKind(in, gc.KindWorktrees)
	if len(out) != 2 || out[0] != gc.KindSockets || out[1] != gc.KindCoordLocks {
		t.Fatalf("dropKind removed-or-reordered wrong: %v", out)
	}
	// in must be untouched (no aliasing).
	if len(in) != 3 || in[1] != gc.KindWorktrees {
		t.Fatalf("dropKind mutated caller's slice: %v", in)
	}
	// Dropping an absent kind is a no-op copy.
	if got := dropKind([]gc.Kind{gc.KindSockets}, gc.KindWorktrees); len(got) != 1 {
		t.Fatalf("dropKind(absent) should keep all: %v", got)
	}
}

// Codex iter-1 [P2] regression: a worktree-GC that holds the lock longer
// than the bound must NOT let a second GC scan/re-check/remove worktrees
// unlocked. On an ErrLockTimeout the runGC call SKIPS the worktrees kind
// (warning to stderr) instead of proceeding unlocked. We hold the lock
// from a separate fd (OS-level contention) and shrink the timeout so the
// waiter times out deterministically (no multi-minute wait, no Sleep
// assertion).
func TestRunGC_Worktrees_SkipsOnLockTimeout(t *testing.T) {
	t.Setenv("FLEET_TMUX_SOCKET", "") // worktrees classifier alone never reaches tmux
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	const project = "rainier"
	restore := state.SetWorktreeGCLockTimeoutForTest(150 * time.Millisecond)
	t.Cleanup(restore)

	// Hold the worktree-GC lock from a separate fd so runGC's bounded
	// acquire times out.
	path, err := state.ProjectWorktreeGCLockPath(project)
	if err != nil {
		t.Fatalf("ProjectWorktreeGCLockPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	holder, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = holder.Close() }()
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("flock holder: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Flock(int(holder.Fd()), syscall.LOCK_UN) })

	var stdout, stderr bytes.Buffer
	start := time.Now()
	if err := runGC(&stdout, &stderr, &gcFlags{
		maxAge:   gcDefaultMaxAge,
		kindsCSV: "worktrees",
		project:  project,
	}); err != nil {
		t.Fatalf("runGC: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("runGC blocked %s; bounded acquire should time out near 150ms", elapsed)
	}
	se := stderr.String()
	if !strings.Contains(se, "held by another GC") || !strings.Contains(se, "skipping worktrees kind") {
		t.Fatalf("expected skip-worktrees warning on lock timeout; got stderr:\n%s", se)
	}
	// It must NOT have fallen back to the unlocked path.
	if strings.Contains(se, "proceeding unlocked") {
		t.Fatalf("runGC must NOT proceed unlocked on a lock timeout; got:\n%s", se)
	}
}
