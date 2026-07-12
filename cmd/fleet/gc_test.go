package main

import (
	"bytes"
	"encoding/json"
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

func TestParseKindsCSV_AcceptsDrainProcs(t *testing.T) {
	// handoff-drain-storm-leak PR5: --kinds=drain-procs must parse cleanly.
	got, err := parseKindsCSV("drain-procs")
	if err != nil {
		t.Fatalf("parseKindsCSV: %v", err)
	}
	if len(got) != 1 || got[0] != gc.KindDrainProcs {
		t.Fatalf("got %v, want [drain-procs]", got)
	}
}

func TestGC_OrphanKickedCLI(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEET_HOME", root)
	t.Setenv("FLEET_TMUX_SOCKET", "")
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	queueDir, err := state.QueueDir()
	if err != nil {
		t.Fatalf("QueueDir: %v", err)
	}
	orphan := filepath.Join(queueDir, "spawn-fresh-cli-orphan.json.kicked")
	matchedBase := filepath.Join(queueDir, "spawn-fresh-cli-live.json")
	matched := matchedBase + ".kicked"
	gcTestWriteFile(t, orphan)
	gcTestWriteFile(t, matchedBase)
	gcTestWriteFile(t, matched)

	kinds, err := parseKindsCSV("orphan-kicked")
	if err != nil {
		t.Fatalf("parseKindsCSV: %v", err)
	}
	if len(kinds) != 1 || kinds[0] != gc.KindOrphanKicked {
		t.Fatalf("got kinds=%v, want [orphan-kicked]", kinds)
	}

	var stdout, stderr bytes.Buffer
	if err := runGC(&stdout, &stderr, &gcFlags{
		maxAge:   gcDefaultMaxAge,
		kindsCSV: "orphan-kicked",
	}); err != nil {
		t.Fatalf("runGC dry-run: %v\nstderr=%s", err, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "orphan-kicked  "+orphan+"  verb=would-remove") {
		t.Fatalf("dry-run should report the orphan marker; got:\n%s", out)
	}
	if !strings.Contains(out, "1 kicked") {
		t.Fatalf("summary should include kicked count; got:\n%s", out)
	}
	if !gcTestPathExists(orphan) {
		t.Fatalf("dry-run removed orphan marker %s", orphan)
	}
	if !gcTestPathExists(matched) {
		t.Fatalf("dry-run removed matched marker %s", matched)
	}

	stdout.Reset()
	stderr.Reset()
	if err := runGC(&stdout, &stderr, &gcFlags{
		apply:    true,
		maxAge:   gcDefaultMaxAge,
		kindsCSV: "orphan-kicked",
	}); err != nil {
		t.Fatalf("runGC apply: %v\nstderr=%s", err, stderr.String())
	}
	out = stdout.String()
	if !strings.Contains(out, "orphan-kicked  "+orphan+"  verb=removed") {
		t.Fatalf("apply should remove the orphan marker; got:\n%s", out)
	}
	if !strings.Contains(out, "1 kicked") {
		t.Fatalf("apply summary should include kicked count; got:\n%s", out)
	}
	if gcTestPathExists(orphan) {
		t.Fatalf("--apply left orphan marker %s", orphan)
	}
	if !gcTestPathExists(matched) {
		t.Fatalf("--apply removed matched marker %s", matched)
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
	// Include worker-records: it depends on the worktrees pass populating
	// dirtyParkedInRun, so a timeout must drop BOTH (codex iter-7 [P2]).
	if err := runGC(&stdout, &stderr, &gcFlags{
		maxAge:   gcDefaultMaxAge,
		kindsCSV: "worktrees,worker-records",
		project:  project,
	}); err != nil {
		t.Fatalf("runGC: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("runGC blocked %s; bounded acquire should time out near 150ms", elapsed)
	}
	se := stderr.String()
	if !strings.Contains(se, "held by another GC") ||
		!strings.Contains(se, "skipping worktrees + worker-records kinds") {
		t.Fatalf("expected skip-worktrees+worker-records warning on lock timeout; got stderr:\n%s", se)
	}
	// It must NOT have fallen back to the unlocked path.
	if strings.Contains(se, "proceeding unlocked") {
		t.Fatalf("runGC must NOT proceed unlocked on a lock timeout; got:\n%s", se)
	}
}

// TestRenderReport_IncludesDrainProcs pins the drain-procs column in the
// per-action + summary output (handoff-drain-storm-leak PR5).
func TestRenderReport_IncludesDrainProcs(t *testing.T) {
	var buf bytes.Buffer
	renderReport(&buf, gc.Options{Apply: false, Kinds: gc.AllKinds},
		gc.Report{Actions: []gc.Action{
			{Kind: gc.KindDrainProcs, Target: "drain pid=4242", Verb: gc.VerbSurface,
				Reason: "WEDGED fleet drain (pid=4242, heartbeat 10m ago > TTL 2m)"},
		}})
	out := buf.String()
	if !strings.Contains(out, "drain-procs  drain pid=4242  verb=surface") {
		t.Errorf("drain-procs action line missing:\n%s", out)
	}
	if !strings.Contains(out, "1 drain-procs") {
		t.Errorf("summary should count drain-procs; got:\n%s", out)
	}
}

// writeDrainRun seeds a ~/.fleet/drain-runs/<pid>.json run-record under
// the test's FLEET_HOME for the end-to-end runGC surface tests. The pid
// is set to a definitely-dead value so the production DrainProcLive
// probe returns false (no real process is touched).
func writeDrainRun(t *testing.T, root string, pid int, heartbeatAt time.Time) {
	t.Helper()
	dir := filepath.Join(root, "drain-runs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir drain-runs: %v", err)
	}
	rec := map[string]any{
		"pid":          pid,
		"pid_start":    "Mon Jun  2 00:00:00 2026",
		"started_at":   heartbeatAt.Add(-time.Hour),
		"heartbeat_at": heartbeatAt,
	}
	data, _ := json.Marshal(rec)
	if err := os.WriteFile(filepath.Join(dir, "999999999.json"), data, 0o644); err != nil {
		t.Fatalf("write run-record: %v", err)
	}
}

func gcTestWriteFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func gcTestPathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// TestRunGC_DrainProcs_DryRun_StaleRecordDeadPid surfaces a stale
// run-record whose pid is dead (no live process). Dry-run → the
// classifier reports a "would-clean record" action and never kills.
// Uses an impossible pid (999999999) so the production DrainProcLive
// returns false deterministically without any real process.
func TestRunGC_DrainProcs_DryRun_StaleRecordDeadPid(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEET_HOME", root)
	t.Setenv("FLEET_TMUX_SOCKET", "")
	// Heartbeat far in the past → stale beyond TTL.
	writeDrainRun(t, root, 999999999, time.Now().Add(-time.Hour))

	var stdout, stderr bytes.Buffer
	if err := runGC(&stdout, &stderr, &gcFlags{
		apply:    false,
		maxAge:   gcDefaultMaxAge,
		kindsCSV: "drain-procs",
	}); err != nil {
		t.Fatalf("runGC: %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "drain pid=999999999") {
		t.Fatalf("expected the stale dead-pid run-record surfaced; got:\n%s", out)
	}
	// Dead pid in dry-run → would-remove the stale record (no kill verb).
	if !strings.Contains(out, "verb=would-remove") {
		t.Fatalf("dead-pid stale record should be a would-remove in dry-run; got:\n%s", out)
	}
	if strings.Contains(out, "verb=killed") {
		t.Fatalf("dry-run must never kill; got:\n%s", out)
	}
}

// TestRunGC_LegacyDrains_ImpliesKind asserts --legacy-drains folds the
// drain-procs kind in even when --kinds names a different family, and
// runs end-to-end without error against an isolated FLEET_HOME (no real
// fleet drain processes → the legacy ps sweep finds none, zero actions).
func TestRunGC_LegacyDrains_ImpliesKind(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEET_HOME", root)
	t.Setenv("FLEET_TMUX_SOCKET", "")

	var stdout, stderr bytes.Buffer
	if err := runGC(&stdout, &stderr, &gcFlags{
		apply:        false,
		maxAge:       gcDefaultMaxAge,
		kindsCSV:     "sockets", // deliberately omit drain-procs
		legacyDrains: true,
	}); err != nil {
		t.Fatalf("runGC: %v", err)
	}
	// The summary always prints the drain-procs column; with the kind
	// folded in, the sweep ran (0 found on a clean host).
	if !strings.Contains(stdout.String(), "drain-procs") {
		t.Fatalf("--legacy-drains should imply the drain-procs kind; summary:\n%s", stdout.String())
	}
}
