package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/coordlock"
	"github.com/edisonshen/fleet/internal/handoffdelivery"
	"github.com/edisonshen/fleet/internal/queue"
	"github.com/edisonshen/fleet/internal/spawn"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/testutil/tmuxfake"
	"github.com/edisonshen/fleet/internal/testutil/tmuxtest"
	"github.com/edisonshen/fleet/internal/tmux"
)

func setupFleetHome(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	// Make the FLEET_MAX_SESSIONS cap effectively unbounded by
	// default. Tests that exercise the cap explicitly set their own
	// value (e.g. FLEET_MAX_SESSIONS=3). Without this, the new
	// spawn-time precheck would see the operator's REAL tmux
	// session count when tests don't isolate via FLEET_TMUX_SOCKET,
	// and unrelated tests would fire cap-refusal errors instead of
	// reaching the validation paths they're testing.
	t.Setenv("FLEET_MAX_SESSIONS", "100000")
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	return tmp
}

// requireTmux delegates socket isolation to tmuxtest.RequireTmux (the
// canonical helper at internal/testutil/tmuxtest) and adds the
// handoff-specific env pins. Postmortem 2026-05-14 (orphan tmux leak)
// + 2026-05-15 follow-up: tmux.Spawn under `go test` refuses to use
// the default socket; tmuxtest.RequireTmux is the lint-recognized
// isolation marker.
func requireTmux(t *testing.T) {
	t.Helper()
	tmuxtest.RequireTmux(t)
	// runHandoff calls spawn.SendInitialPrompt between step 8a and
	// step 9; the helper polls pane stability before typing. Pin
	// small windows so tests don't pay the production 30 s cap on
	// shells that may not stabilize predictably.
	t.Setenv("FLEET_INITIAL_PROMPT_STABLE_MS", "100")
	t.Setenv("FLEET_INITIAL_PROMPT_MAX_MS", "1000")
	// Issue #65 added a post-stability buffer (default 1.5 s) plus
	// bumped the prompt-enter delay default to 1 s, and added
	// post-send verify/retry delays (defaults 0.5 s + 1.5 s). Pin
	// all to fast values so handoff tests don't balloon — production
	// reliability is verified separately in spawn_test.go's buffer +
	// verifier tests.
	t.Setenv("FLEET_POST_READY_BUFFER_MS", "0")
	t.Setenv("FLEET_POST_SEND_VERIFY_MS", "0")
	t.Setenv("FLEET_POST_SEND_RETRY_MS", "0")
	t.Setenv("FLEET_PROMPT_ENTER_DELAY_MS", "50")
	// Speed up the pid-resolver fallback budget — production polls
	// up to 10s waiting for claude to exec inside the wrapper shell.
	// Integration tests use synthetic commands ("sleep 30", shells)
	// where no claude descendant ever appears, so we'd otherwise
	// pay the full 10s timeout per test. 1s keeps the polling loop
	// exercised while bounding wall time.
	t.Setenv("FLEET_PID_RESOLVE_S", "1")
}

// requireFakeTmux installs the in-process fake tmux backend
// (internal/testutil/tmuxfake) so the test exercises the real Fleet
// spawn/handoff/drain code paths WITHOUT execing a tmux(1) subprocess —
// the CI-perf lever (PR-2 of DESIGN-ci-3min-test-suite). It is the fast,
// orphan-proof substitute for requireTmux: no real tmux server, no
// `sleep 60` child, no pid-resolve floor, no leaked /tmp/fleet-test-*.sock.
//
// It is also a recognized isolation marker for lint-test-isolation.sh:
// installing the fake means tmux.Spawn never reaches the operator's
// default socket (the fake records in-process), so a test that calls
// requireFakeTmux satisfies the same safety boundary requireTmux does.
//
// Use requireFakeTmux for tests that only need a session abstraction
// (Spawn / Kill / HasSession / SendKeys / CapturePane) and assert on
// Fleet's own state machine. Use requireTmux only for tests that need
// REAL process semantics (real engine pid liveness, real pane content,
// real `coord-run` supervisor). The env pins below mirror requireTmux so
// any production timing path the test still touches stays fast.
func requireFakeTmux(t *testing.T) *tmuxfake.Fake {
	t.Helper()
	f := tmuxfake.InstallFake(t)
	t.Setenv("FLEET_INITIAL_PROMPT_STABLE_MS", "0")
	t.Setenv("FLEET_INITIAL_PROMPT_MAX_MS", "100")
	t.Setenv("FLEET_POST_READY_BUFFER_MS", "0")
	t.Setenv("FLEET_POST_SEND_VERIFY_MS", "0")
	t.Setenv("FLEET_POST_SEND_RETRY_MS", "0")
	t.Setenv("FLEET_PROMPT_ENTER_DELAY_MS", "0")
	t.Setenv("FLEET_PID_RESOLVE_S", "1")
	return f
}

// seedAgent dispatches a long-lived agent for handoff to operate on.
// Returns the spawned record. Caller cleans up via t.Cleanup if needed.
func seedAgent(t *testing.T) *agent.Record {
	t.Helper()
	out := &bytes.Buffer{}
	if err := runDispatch(&dispatchOpts{
		taskID:  "auth-fix",
		project: "rainier",
		command: []string{"sleep", "60"},
	}, out); err != nil {
		t.Fatalf("dispatch: %v\n%s", err, out.String())
	}
	live, err := agent.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(live) != 1 {
		t.Fatalf("expected 1 live agent after dispatch, got %d", len(live))
	}
	return live[0]
}

func TestHandoff_FailsClearlyOnUnknownAgent(t *testing.T) {
	requireFakeTmux(t)
	setupFleetHome(t)

	out := &bytes.Buffer{}
	err := runHandoff(&handoffOpts{
		oldID:       "ghostbas",
		command:     []string{"sleep", "60"},
		graceMillis: 1,
	}, out, out)
	if err == nil {
		t.Error("expected error for unknown agent")
	}
}

func TestHandoff_HappyPath(t *testing.T) {
	requireFakeTmux(t)
	tmp := setupFleetHome(t)

	old := seedAgent(t)
	t.Cleanup(func() { _ = tmux.Kill(old.TmuxSession) })

	out := &bytes.Buffer{}
	if err := runHandoff(&handoffOpts{
		oldID:       old.ID,
		command:     []string{"sleep", "60"},
		graceMillis: 1, // 1ms minimal grace: the coord-swap path defaults a
		// zero/negative GraceWindow to DefaultSwapGraceWindow (10s), so a
		// literal 0 makes every coord-handoff test pay a real 10s sleep.
	}, out, out); err != nil {
		t.Fatalf("handoff: %v\n%s", err, out.String())
	}

	// Old agent record archived.
	if _, err := os.Stat(filepath.Join(tmp, "agents", old.ID+".json")); !os.IsNotExist(err) {
		t.Errorf("old live record should be gone, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "agents", "archive", old.ID+".json")); err != nil {
		t.Errorf("old archive record missing: %v", err)
	}

	// Old tmux session killed.
	if tmux.HasSession(old.TmuxSession) {
		t.Errorf("old tmux session %s should be dead", old.TmuxSession)
	}

	// Exactly one live agent now (the replacement).
	live, err := agent.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(live) != 1 {
		t.Fatalf("expected 1 live agent after handoff, got %d", len(live))
	}
	newRec := live[0]
	t.Cleanup(func() { _ = tmux.Kill(newRec.TmuxSession) })

	if newRec.ID == old.ID {
		t.Error("new agent must have different ID")
	}
	if newRec.TaskID != "auth-fix" || newRec.Project != "rainier" {
		t.Errorf("task identity not inherited: %+v", newRec)
	}
	if newRec.HandoffNumber != old.HandoffNumber+1 {
		t.Errorf("HandoffNumber: got %d want %d", newRec.HandoffNumber, old.HandoffNumber+1)
	}
	if newRec.LastHandoffPath == nil {
		t.Error("LastHandoffPath should point at the doc just written")
	}
	if newRec.HandoffType == nil || *newRec.HandoffType != "manual" {
		t.Errorf("HandoffType: want manual, got %v", newRec.HandoffType)
	}

	// Handoff doc exists at the path the new agent points at.
	if _, err := os.Stat(*newRec.LastHandoffPath); err != nil {
		t.Errorf("handoff doc missing: %v", err)
	}

	// Queue file is gone (drained).
	queueDir := filepath.Join(tmp, "queue")
	entries, _ := os.ReadDir(queueDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "spawn-fresh-") {
			t.Errorf("queue file %s should have been deleted", e.Name())
		}
	}

	// Output mentions both IDs and the handoff doc path.
	s := out.String()
	for _, want := range []string{old.ID, newRec.ID, "handed off", *newRec.LastHandoffPath} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q:\n%s", want, s)
		}
	}
}

// TestHandoff_WritesChainPointer pins the v2 schema chain-pointer
// write on the happy-path handoff (handoff-identity-cont-3f1d).
// Predecessor archive must carry successor_id + archived_cause=handoff;
// live successor record must carry predecessor_id. Together these
// are what `fleet attach <predecessor>` walks via agent.ResolveChain.
func TestHandoff_WritesChainPointer(t *testing.T) {
	requireFakeTmux(t)
	tmp := setupFleetHome(t)

	old := seedAgent(t)
	t.Cleanup(func() { _ = tmux.Kill(old.TmuxSession) })

	out := &bytes.Buffer{}
	if err := runHandoff(&handoffOpts{
		oldID:       old.ID,
		command:     []string{"sleep", "60"},
		graceMillis: 1,
	}, out, out); err != nil {
		t.Fatalf("handoff: %v\n%s", err, out.String())
	}

	live, err := agent.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(live) != 1 {
		t.Fatalf("expected exactly 1 live after handoff; got %d", len(live))
	}
	newRec := live[0]
	t.Cleanup(func() { _ = tmux.Kill(newRec.TmuxSession) })

	// Predecessor archive carries successor_id + cause=handoff.
	arc, err := agent.LoadArchive(old.ID)
	if err != nil {
		t.Fatalf("LoadArchive predecessor: %v", err)
	}
	if arc.SuccessorID != newRec.ID {
		t.Errorf("archive[%s].SuccessorID: got %q want %q", old.ID, arc.SuccessorID, newRec.ID)
	}
	if arc.ArchivedCause != agent.ArchivedCauseHandoff {
		t.Errorf("archive[%s].ArchivedCause: got %q want %q",
			old.ID, arc.ArchivedCause, agent.ArchivedCauseHandoff)
	}

	// Live successor carries predecessor_id pointing back.
	if newRec.PredecessorID != old.ID {
		t.Errorf("live successor PredecessorID: got %q want %q", newRec.PredecessorID, old.ID)
	}

	// Resolver round-trip: ResolveChain(old.ID) lands on newRec with hops=1.
	tail, hops, rerr := agent.ResolveChain(old.ID)
	if rerr != nil {
		t.Fatalf("ResolveChain(%s): %v", old.ID, rerr)
	}
	if tail.ID != newRec.ID {
		t.Errorf("ResolveChain tail: got %q want %q", tail.ID, newRec.ID)
	}
	if hops != 1 {
		t.Errorf("ResolveChain hops: got %d want 1", hops)
	}

	// Silence the unused-tmp linter — kept in scope so future
	// assertions can reach the on-disk paths directly.
	_ = tmp
}

func TestHandoff_ChainGrowsAcrossSequentialHandoffs(t *testing.T) {
	requireFakeTmux(t)
	setupFleetHome(t)

	// Initial dispatch.
	first := seedAgent(t)
	t.Cleanup(func() { _ = tmux.Kill(first.TmuxSession) })

	// Three handoffs in a row.
	currentID := first.ID
	var lastDocPath string
	for i := 0; i < 3; i++ {
		out := &bytes.Buffer{}
		if err := runHandoff(&handoffOpts{
			oldID:       currentID,
			command:     []string{"sleep", "60"},
			graceMillis: 1,
		}, out, out); err != nil {
			t.Fatalf("handoff #%d: %v\n%s", i+1, err, out.String())
		}
		live, err := agent.List()
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(live) != 1 {
			t.Fatalf("expected 1 live, got %d (after handoff #%d)", len(live), i+1)
		}
		current := live[0]
		t.Cleanup(func() { _ = tmux.Kill(current.TmuxSession) })

		wantNumber := first.HandoffNumber + i + 1
		if current.HandoffNumber != wantNumber {
			t.Errorf("handoff #%d: got HandoffNumber=%d want %d",
				i+1, current.HandoffNumber, wantNumber)
		}
		if current.LastHandoffPath == nil {
			t.Errorf("handoff #%d: LastHandoffPath nil", i+1)
		}
		// The previous_handoff field of doc N points to doc N-1.
		if i > 0 && lastDocPath == "" {
			t.Errorf("handoff #%d: chain broken, no prev doc tracked", i+1)
		}
		lastDocPath = *current.LastHandoffPath
		currentID = current.ID
	}
}

func TestHandoff_ConcurrentHandoffDetectedAfterArchive(t *testing.T) {
	// Simulates the race the flock-first ordering prevents: handoff
	// runs once, archives the agent. A second handoff invocation for
	// the same ID then loads the agent (succeeds — caller just read
	// from disk before lock), acquires the flock, re-loads under the
	// lock, sees ErrNotFound, bails. This test exercises the second
	// invocation against an already-archived record.
	requireFakeTmux(t)
	setupFleetHome(t)

	old := seedAgent(t)
	t.Cleanup(func() { _ = tmux.Kill(old.TmuxSession) })

	// First handoff: succeeds, archives the agent.
	out1 := &bytes.Buffer{}
	if err := runHandoff(&handoffOpts{
		oldID:       old.ID,
		command:     []string{"sleep", "60"},
		graceMillis: 1,
	}, out1, out1); err != nil {
		t.Fatalf("first handoff: %v\n%s", err, out1.String())
	}
	for _, l := range listLive(t) {
		t.Cleanup(func() { _ = tmux.Kill(l.TmuxSession) })
	}

	// Second handoff for the same OLD ID: should fail at the
	// re-load-under-lock step. The agent is gone from agents/.
	out2 := &bytes.Buffer{}
	err := runHandoff(&handoffOpts{
		oldID:       old.ID,
		command:     []string{"sleep", "60"},
		graceMillis: 1,
	}, out2, out2)
	if err == nil {
		t.Fatalf("expected second handoff to fail (record archived), got success:\n%s", out2.String())
	}
	if !strings.Contains(err.Error(), "concurrent handoff") &&
		!strings.Contains(err.Error(), "not found") {
		t.Errorf("expected error to mention concurrent handoff or not found, got: %v", err)
	}

	// Live count should still be 1 (only the first handoff's
	// replacement, no second double-spawn).
	if n := len(listLive(t)); n != 1 {
		t.Errorf("expected 1 live agent after blocked second handoff, got %d", n)
	}
}

func listLive(t *testing.T) []*agent.Record {
	t.Helper()
	live, err := agent.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return live
}

func TestHandoff_RefusesLegacyRecordMissingCwdAndCommand(t *testing.T) {
	// Codex iter-7 P1: legacy records (pre-PR, no Cwd/Command in
	// JSON) MUST NOT silently fall back to os.Getwd / "claude" when
	// no flags supplied — that would land the replacement in the
	// wrong tree / wrong binary while reporting success.
	requireFakeTmux(t)
	tmp := setupFleetHome(t)

	// Hand-craft a legacy record JSON: no cwd, no command fields.
	legacy := `{
  "schema_version": 1,
  "id": "legacyid",
  "engine": "claude-code",
  "role": "executor",
  "mode": "execute",
  "tmux_session": "fleet-legacyid",
  "task_id": "t",
  "project": "p"
}`
	if err := os.WriteFile(filepath.Join(tmp, "agents", "legacyid.json"),
		[]byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	// Spawn a real tmux session so tmux.Available passes the load.
	t.Cleanup(func() { _ = tmux.Kill("fleet-legacyid") })
	if err := tmux.Spawn("fleet-legacyid", "", []string{"sleep", "60"}, nil); err != nil {
		t.Fatalf("seed tmux: %v", err)
	}

	out := &bytes.Buffer{}
	err := runHandoff(&handoffOpts{
		oldID:       "legacyid",
		graceMillis: 1,
	}, out, out)
	if err == nil {
		t.Fatal("expected legacy handoff with no flags to refuse")
	}
	if !strings.Contains(err.Error(), "legacy record") {
		t.Errorf("expected error about legacy record, got: %v", err)
	}

	// Codex iter-8 P2: refusal MUST NOT leave on-disk side effects
	// (no handoff doc, no queue file, no archived record).
	if entries, _ := os.ReadDir(filepath.Join(tmp, "handoffs")); len(entries) > 0 {
		t.Errorf("legacy refusal left handoff doc on disk: %v", entries)
	}
	if entries, _ := os.ReadDir(filepath.Join(tmp, "queue")); len(entries) > 0 {
		t.Errorf("legacy refusal left queue file on disk: %v", entries)
	}
	if entries, _ := os.ReadDir(filepath.Join(tmp, "agents", "archive")); len(entries) > 0 {
		t.Errorf("legacy refusal archived old record: %v", entries)
	}

	// Supplying both flags should succeed.
	out2 := &bytes.Buffer{}
	if err := runHandoff(&handoffOpts{
		oldID:       "legacyid",
		cwd:         t.TempDir(),
		command:     []string{"sleep", "120"},
		graceMillis: 1,
	}, out2, out2); err != nil {
		t.Fatalf("legacy handoff with explicit flags should succeed, got: %v\n%s", err, out2.String())
	}
	for _, l := range listLive(t) {
		t.Cleanup(func() { _ = tmux.Kill(l.TmuxSession) })
	}
}

func TestHandoff_PreservesCwdAndCommandFromOldRecord(t *testing.T) {
	// Codex iter-5 P1: handoff invoked from a different shell must
	// place the replacement in the OUTGOING agent's cwd and run its
	// original command, not the invoker's defaults.
	requireFakeTmux(t)
	setupFleetHome(t)

	// Seed an agent with explicit cwd + a non-default command. We
	// can't use seedAgent because that hard-codes "sleep 60" without
	// passing cwd through dispatch; build the spawn directly.
	originalCwd := t.TempDir()
	// Long-running, valid command (extra arg would crash `sleep` and
	// trigger the new replacement-session-alive check).
	originalCmd := []string{"sh", "-c", "exec sleep 120"}
	first, err := agentSpawnForTest(t, originalCwd, originalCmd, "rainier", "auth-fix")
	if err != nil {
		t.Fatalf("seed spawn: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(first.TmuxSession) })

	if first.Cwd != originalCwd {
		t.Fatalf("seed: Cwd not captured: got %q want %q", first.Cwd, originalCwd)
	}

	// Run handoff WITHOUT --cwd or --command. Replacement should
	// inherit both from the outgoing record.
	out := &bytes.Buffer{}
	if err := runHandoff(&handoffOpts{
		oldID:       first.ID,
		graceMillis: 1,
	}, out, out); err != nil {
		t.Fatalf("handoff: %v\n%s", err, out.String())
	}

	live := listLive(t)
	if len(live) != 1 {
		t.Fatalf("expected 1 live agent, got %d", len(live))
	}
	rep := live[0]
	t.Cleanup(func() { _ = tmux.Kill(rep.TmuxSession) })

	if rep.Cwd != originalCwd {
		t.Errorf("replacement Cwd: got %q want %q (inherited)", rep.Cwd, originalCwd)
	}
	if len(rep.Command) != len(originalCmd) {
		t.Fatalf("replacement Command length: got %d want %d", len(rep.Command), len(originalCmd))
	}
	for i := range originalCmd {
		if rep.Command[i] != originalCmd[i] {
			t.Errorf("replacement Command[%d]: got %q want %q", i, rep.Command[i], originalCmd[i])
		}
	}
}

func agentSpawnForTest(t *testing.T, cwd string, command []string, project, taskID string) (*agent.Record, error) {
	t.Helper()
	return spawn.Spawn(spawn.Options{
		TaskID:           taskID,
		Project:          project,
		Cwd:              cwd,
		Command:          command,
		DisableLeaseWrap: true,
	})
}

// E7 (manual handoff) — DESIGN-coord-repo-binding-from-project.md PR3: a
// COORD handoff resolves the replacement's Cwd via the shared resolver
// (meta repo_path), NOT the outgoing coord's possibly-wrong Cwd. The
// outgoing coord here is seeded in a WRONG tree; meta pins the correct
// repo; the replacement must land in the meta repo.
func TestHandoff_CoordResolvesRepoNotOldCwd(t *testing.T) {
	requireFakeTmux(t)
	root := setupFleetHome(t)

	const project = "rainier"
	correctRepo := seedRecoveryRepo(t, root, project) // resolver tier 1

	// Outgoing COORD (task_id = coord-<project>) seeded in a WRONG tree.
	wrongCwd := t.TempDir()
	first, err := agentSpawnForTest(t, wrongCwd,
		[]string{"sh", "-c", "exec sleep 120"}, project, "coord-"+project)
	if err != nil {
		t.Fatalf("seed coord spawn: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(first.TmuxSession) })

	// Handoff WITHOUT --cwd → coord path must resolve via the resolver.
	out := &bytes.Buffer{}
	if err := runHandoff(&handoffOpts{oldID: first.ID, graceMillis: 1}, out, out); err != nil {
		t.Fatalf("coord handoff: %v\n%s", err, out.String())
	}
	live := listLive(t)
	if len(live) != 1 {
		t.Fatalf("expected 1 live agent, got %d", len(live))
	}
	rep := live[0]
	t.Cleanup(func() { _ = tmux.Kill(rep.TmuxSession) })
	if rep.Cwd != correctRepo {
		t.Errorf("coord handoff bound wrong tree: rep.Cwd = %q; want %q (resolver meta repo, not old coord Cwd %q)",
			rep.Cwd, correctRepo, wrongCwd)
	}
}

// E7 (manual handoff, refuse) — a COORD handoff REFUSES when the resolver
// cannot bind, rather than falling back to the outgoing coord's Cwd.
func TestHandoff_CoordRefusesWhenUnresolvable(t *testing.T) {
	requireFakeTmux(t)
	setupFleetHome(t)

	const project = "ghostproj" // NO meta.json, no worktrees → refuse

	wrongCwd := t.TempDir()
	first, err := agentSpawnForTest(t, wrongCwd,
		[]string{"sh", "-c", "exec sleep 120"}, project, "coord-"+project)
	if err != nil {
		t.Fatalf("seed coord spawn: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(first.TmuxSession) })

	out := &bytes.Buffer{}
	err = runHandoff(&handoffOpts{oldID: first.ID, graceMillis: 1}, out, out)
	if err == nil {
		t.Fatal("coord handoff must REFUSE when resolver cannot bind, got nil")
	}
	if !strings.Contains(err.Error(), "no usable checkout") {
		t.Errorf("refusal should surface the resolver hint; got %v", err)
	}
	// No replacement must have spawned (refusal is before any side effect).
	for _, l := range listLive(t) {
		if l.ID != first.ID {
			t.Cleanup(func() { _ = tmux.Kill(l.TmuxSession) })
			t.Errorf("coord handoff spawned a replacement %q despite refusal", l.ID)
		}
	}
}

func TestHandoff_ShouldRefuseLeaseWrappedCoordHandoffRetire_RequiresLiveOld(t *testing.T) {
	oldRec := &agent.Record{ID: "oldcoord", Project: "rainier", SupervisorPID: 4242}

	origOwner := handoffLeaseActiveOwnerPIDFn
	origLeader := handoffLeaseLeaderPresentFn
	handoffLeaseActiveOwnerPIDFn = func(project string) (int, bool) {
		if project != oldRec.Project {
			t.Fatalf("active owner checked for project %q, want %q", project, oldRec.Project)
		}
		return 0, false
	}
	handoffLeaseLeaderPresentFn = func(string) bool { return true }
	t.Cleanup(func() {
		handoffLeaseActiveOwnerPIDFn = origOwner
		handoffLeaseLeaderPresentFn = origLeader
	})

	refuse, err := shouldRefuseLeaseWrappedCoordHandoffRetire(oldRec)
	if err != nil {
		t.Fatalf("shouldRefuseLeaseWrappedCoordHandoffRetire: %v", err)
	}
	if refuse {
		t.Fatal("old coord is not the active lease owner; handoff must not drop a valid standby")
	}

	ownerReads := 0
	handoffLeaseActiveOwnerPIDFn = func(string) (int, bool) {
		ownerReads++
		if ownerReads == 1 {
			return oldRec.SupervisorPID, true
		}
		return oldRec.SupervisorPID + 1, true
	}
	refuse, err = shouldRefuseLeaseWrappedCoordHandoffRetire(oldRec)
	if err != nil {
		t.Fatalf("shouldRefuseLeaseWrappedCoordHandoffRetire owner-race: %v", err)
	}
	if refuse {
		t.Fatal("active owner moved to successor after health check; must not drop standby")
	}

	handoffLeaseActiveOwnerPIDFn = func(string) (int, bool) { return oldRec.SupervisorPID, true }
	refuse, err = shouldRefuseLeaseWrappedCoordHandoffRetire(oldRec)
	if err != nil {
		t.Fatalf("shouldRefuseLeaseWrappedCoordHandoffRetire active-old: %v", err)
	}
	if !refuse {
		t.Fatal("old coord owns the active lease; PR1 must refuse before retiring it behind a standby supervisor")
	}
}

// E8 (manual handoff) — a WORKER handoff (task_id is a real slug, NOT
// coord-<project>) keeps inheriting the outgoing record's Cwd. The
// resolver is COORD-only; workers legitimately follow their dispatch
// tree. Guards against the coord-binding change leaking into workers.
func TestHandoff_WorkerStillInheritsOldCwd(t *testing.T) {
	requireFakeTmux(t)
	root := setupFleetHome(t)

	const project = "rainier"
	// Seed meta pointing at a DIFFERENT repo to prove the worker path
	// does NOT consult the resolver (it would bind this otherwise).
	otherRepo := seedRecoveryRepo(t, root, project)

	workerCwd := t.TempDir()
	first, err := agentSpawnForTest(t, workerCwd,
		[]string{"sh", "-c", "exec sleep 120"}, project, "auth-fix") // worker slug
	if err != nil {
		t.Fatalf("seed worker spawn: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(first.TmuxSession) })

	out := &bytes.Buffer{}
	if err := runHandoff(&handoffOpts{oldID: first.ID, graceMillis: 1}, out, out); err != nil {
		t.Fatalf("worker handoff: %v\n%s", err, out.String())
	}
	live := listLive(t)
	if len(live) != 1 {
		t.Fatalf("expected 1 live agent, got %d", len(live))
	}
	rep := live[0]
	t.Cleanup(func() { _ = tmux.Kill(rep.TmuxSession) })
	if rep.Cwd != workerCwd {
		t.Errorf("worker handoff Cwd: got %q want %q (inherited, NOT resolver)", rep.Cwd, workerCwd)
	}
	if rep.Cwd == otherRepo {
		t.Errorf("worker handoff incorrectly used the resolver meta repo %q", otherRepo)
	}
}

// TestHandoff_RecoveryProbe_DeadReplacement_ForceRespawns pins the
// manual-handoff recovery path (cmd/fleet/handoff.go, the `case !alive`
// arm). When the recovery probe finds a journaled replacement record
// whose tmux session is dead, it consults the coordinator LEASE via
// handoffop.ClassifyCoordSwapResume (the coord-spawn marker is gone,
// D3). With a free lease (no committed epoch, no handoff reservation)
// AND --force-replacement, the classifier returns ResumeRespawn: drop
// the dead replacement, then fall through to a fresh spawn while the old
// agent is retired. No duplicate coord, no orphaned stale record.
func TestHandoff_RecoveryProbe_DeadReplacement_ForceRespawns(t *testing.T) {
	requireFakeTmux(t)
	tmp := setupFleetHome(t)

	old := seedAgent(t)
	t.Cleanup(func() { _ = tmux.Kill(old.TmuxSession) })

	if _, err := state.EnsureProjectInitialized(old.Project); err != nil {
		t.Fatalf("EnsureProjectInitialized: %v", err)
	}
	const staleNewID = "stalenew"

	// Plant a stale replacement record whose tmux session is dead
	// (the session string points at a name we never spawned, so
	// tmux.SessionAlive returns (false, nil) definitively).
	staleNew := agent.New(staleNewID)
	staleNew.TaskID = old.TaskID
	staleNew.Project = old.Project
	staleNew.Cwd = old.Cwd
	staleNew.Command = old.Command
	staleNew.TmuxSession = "fleet-stalenew-dead"
	staleNew.SpawnedAt = time.Now().UTC()
	if err := staleNew.Write(); err != nil {
		t.Fatalf("plant stale new rec: %v", err)
	}

	// Seed queue file so runHandoff enters the recovery probe.
	docPath := filepath.Join(tmp, "handoffs", old.ID+"-stub.md")
	if err := os.MkdirAll(filepath.Dir(docPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(docPath, []byte("stub doc"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := queue.WriteSpawnFresh(queue.SpawnFresh{
		SchemaVersion: queue.SchemaVersion,
		OldAgentID:    old.ID,
		HandoffDoc:    docPath,
		Project:       old.Project,
		TaskID:        old.TaskID,
		NewAgentID:    staleNewID,
		NewSession:    staleNew.TmuxSession,
	}); err != nil {
		t.Fatalf("seed queue: %v", err)
	}

	// Run handoff with --force-replacement so the recovery refusal gate
	// (which protects against duplicate-coord respawns when the epoch
	// committed to the dead replacement) is bypassed. Here the lease is
	// free, so the classifier would respawn regardless; force keeps the
	// path deterministic. The committed-refuse gate is covered by
	// TestHandoff_RecoveryProbe_OldCoordAlive_RefusesRespawn below.
	out := &bytes.Buffer{}
	if err := runHandoff(&handoffOpts{
		oldID:            old.ID,
		cwd:              old.Cwd,
		command:          []string{"sleep", "60"},
		graceMillis:      1,
		forceReplacement: true,
	}, out, out); err != nil {
		t.Fatalf("runHandoff: %v\n%s", err, out.String())
	}

	// Exactly one live agent remains — the fresh replacement. The
	// stale newRec record is gone, the old one is archived.
	live := listLive(t)
	if len(live) != 1 {
		t.Fatalf("expected 1 live agent post-handoff, got %d: %+v", len(live), live)
	}
	fresh := live[0]
	t.Cleanup(func() { _ = tmux.Kill(fresh.TmuxSession) })

	// Stale record must be gone (DropReplacementRecord ran).
	if _, err := os.Stat(filepath.Join(tmp, "agents", staleNewID+".json")); !os.IsNotExist(err) {
		t.Errorf("stale replacement record should be deleted: stat err=%v", err)
	}

	// Old archived.
	if _, err := os.Stat(filepath.Join(tmp, "agents", old.ID+".json")); !os.IsNotExist(err) {
		t.Errorf("old live record should be archived (not in live dir): stat err=%v", err)
	}
	// The fresh replacement is distinct from the dropped stale one.
	if fresh.ID == staleNewID {
		t.Errorf("fresh replacement reused the dropped stale id %q", staleNewID)
	}
}

// TestHandoff_RecoveryProbe_OldCoordAlive_RefusesRespawn pins the
// duplicate-coord guard on the manual-handoff recovery path
// (cmd/fleet/handoff.go, the `case !alive` arm). When a prior graceful
// swap preserved the queue but the coordinator epoch already COMMITTED
// to the replacement NEW, the next `fleet handoff` lands in the recovery
// probe with NEW's session dead. The coord-spawn marker is gone (D3);
// handoffop.ClassifyCoordSwapResume reads the LEASE: owner == NEW means
// the swap committed, so auto-respawning would create a duplicate coord.
//
// Re-keyed onto the lease: the committed epoch is established by
// acquiring the project lease FOR the replacement id (owner == NEW).
// The classifier returns ResumeRefuse; because OLD's session is still
// alive here (a cross-socket blind spot), the defensive probe also fires
// the loud [P1] diagnostic. Nothing is auto-killed; the operator passes
// --force-replacement once OLD is confirmed dead.
func TestHandoff_RecoveryProbe_OldCoordAlive_RefusesRespawn(t *testing.T) {
	requireFakeTmux(t)
	tmp := setupFleetHome(t)

	old := seedAgent(t)
	t.Cleanup(func() { _ = tmux.Kill(old.TmuxSession) })

	if _, err := state.EnsureProjectInitialized(old.Project); err != nil {
		t.Fatalf("EnsureProjectInitialized: %v", err)
	}
	const staleNewID = "stalenew"

	// Establish the committed lease: acquiring the flock IS becoming the
	// coordinator (PR-2 D1 — no Activate flip). The flock body names the
	// replacement (agent_id == NEW), so CurrentOwner reports staleNewID and the
	// classifier's committed arm (ResumeRefuse) fires. The lease owner pid is
	// this live test process, so the busy flock stays live for the duration of
	// runHandoff.
	lease, acquired, lerr := coordlock.AcquireLease(old.Project, staleNewID)
	if lerr != nil || !acquired || lease == nil {
		t.Fatalf("acquire committed lease for NEW: acquired=%v err=%v", acquired, lerr)
	}
	t.Cleanup(lease.Release)

	staleNew := agent.New(staleNewID)
	staleNew.TaskID = old.TaskID
	staleNew.Project = old.Project
	staleNew.Cwd = old.Cwd
	staleNew.Command = old.Command
	staleNew.TmuxSession = "fleet-stalenew-dead"
	staleNew.SpawnedAt = time.Now().UTC()
	if err := staleNew.Write(); err != nil {
		t.Fatalf("plant stale new rec: %v", err)
	}

	docPath := filepath.Join(tmp, "handoffs", old.ID+"-stub.md")
	if err := os.MkdirAll(filepath.Dir(docPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(docPath, []byte("stub doc"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := queue.WriteSpawnFresh(queue.SpawnFresh{
		SchemaVersion: queue.SchemaVersion,
		OldAgentID:    old.ID,
		HandoffDoc:    docPath,
		Project:       old.Project,
		TaskID:        old.TaskID,
		NewAgentID:    staleNewID,
		NewSession:    staleNew.TmuxSession,
	}); err != nil {
		t.Fatalf("seed queue: %v", err)
	}

	// Run WITHOUT --force-replacement. Must refuse — the epoch committed
	// to NEW (lease owner == staleNewID) and OLD's session is still alive,
	// so the classifier's ResumeRefuse arm fires.
	out := &bytes.Buffer{}
	err := runHandoff(&handoffOpts{
		oldID:       old.ID,
		cwd:         old.Cwd,
		command:     []string{"sleep", "60"},
		graceMillis: 1,
	}, out, out)
	if err == nil {
		t.Fatalf("expected handoff to refuse when the epoch committed to NEW; got nil\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "refusing auto-respawn") || !strings.Contains(err.Error(), "duplicate coord") {
		t.Errorf("expected refusal mentioning 'refusing auto-respawn' + 'duplicate coord'; got: %v", err)
	}
	// OLD is still alive: the classifier's defensive cross-socket probe
	// surfaces the loud [P1] OLD-still-alive diagnostic on stderr.
	if !strings.Contains(out.String(), "may still be alive") {
		t.Errorf("expected loud [P1] OLD-still-alive diagnostic; got:\n%s", out.String())
	}

	// State must be preserved:
	if _, err := os.Stat(filepath.Join(tmp, "agents", staleNewID+".json")); err != nil {
		t.Errorf("stale newRec should be preserved on refusal: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "agents", old.ID+".json")); err != nil {
		t.Errorf("old record should remain live (not archived): %v", err)
	}
	if !tmux.HasSession(old.TmuxSession) {
		t.Errorf("old session should still be alive")
	}
	queuePath := filepath.Join(tmp, "queue", "spawn-fresh-"+old.ID+".json")
	if _, err := os.Stat(queuePath); err != nil {
		t.Errorf("queue file should persist on refusal: %v", err)
	}
}

func TestHandoff_AbortsWhenReplacementDiesAtStartup(t *testing.T) {
	// Codex iter-9 P1: if the replacement command exits immediately
	// (e.g., a wrapper that crashes during startup), spawn.Spawn
	// returns success by design but the new tmux session is gone.
	// Handoff MUST detect this and refuse to retire the old agent —
	// otherwise the task is left with no live successor.
	requireTmux(t)
	tmp := setupFleetHome(t)

	old := seedAgent(t)
	t.Cleanup(func() { _ = tmux.Kill(old.TmuxSession) })

	out := &bytes.Buffer{}
	err := runHandoff(&handoffOpts{
		oldID:       old.ID,
		cwd:         t.TempDir(),
		command:     []string{"sh", "-c", "true"}, // exits immediately
		graceMillis: 1,
	}, out, out)
	if err == nil {
		t.Fatal("expected handoff to fail when replacement dies at startup")
	}
	if !strings.Contains(err.Error(), "already exited") {
		t.Errorf("expected 'already exited' in error, got: %v", err)
	}

	// Old agent untouched: live record still there, tmux session
	// still alive, no archive entry.
	if _, err := os.Stat(filepath.Join(tmp, "agents", old.ID+".json")); err != nil {
		t.Errorf("old live record should still exist, got: %v", err)
	}
	if !tmux.HasSession(old.TmuxSession) {
		t.Errorf("old session %s should still be alive", old.TmuxSession)
	}
	if entries, _ := os.ReadDir(filepath.Join(tmp, "agents", "archive")); len(entries) > 0 {
		t.Errorf("old should not be archived, archive contains: %v", entries)
	}
	// Queue file removed (rollback cleaned it up).
	if entries, _ := os.ReadDir(filepath.Join(tmp, "queue")); len(entries) > 0 {
		t.Errorf("queue file should be removed by rollback, contains: %v", entries)
	}
}

func TestHandoff_RecoveryRefusesDuplicateWhenSessionAlive(t *testing.T) {
	// Codex iter-11 P1: if the replacement RECORD was hand-deleted
	// but pending.NewSession is still alive, fresh-spawning would
	// create a duplicate. Refuse instead.
	requireFakeTmux(t)
	tmp := setupFleetHome(t)

	old := seedAgent(t)
	t.Cleanup(func() { _ = tmux.Kill(old.TmuxSession) })

	// Spawn a real tmux session to act as the "still-alive
	// replacement" — but DON'T register an agent record for it,
	// simulating an out-of-band record deletion.
	orphanSession := "fleet-orphanrep"
	if err := tmux.Spawn(orphanSession, "", []string{"sleep", "60"}, nil); err != nil {
		t.Fatalf("seed orphan session: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(orphanSession) })

	// Seed a queue file that points at an agent ID that doesn't
	// exist on disk but whose session DOES exist.
	if _, err := queue.WriteSpawnFresh(queue.SpawnFresh{
		OldAgentID: old.ID,
		HandoffDoc: "/some/doc.md",
		Project:    old.Project,
		TaskID:     old.TaskID,
		NewAgentID: "orphanrep",
		NewSession: orphanSession,
	}); err != nil {
		t.Fatalf("seed queue: %v", err)
	}

	out := &bytes.Buffer{}
	err := runHandoff(&handoffOpts{
		oldID:       old.ID,
		graceMillis: 1,
	}, out, out)
	if err == nil {
		t.Fatal("expected handoff to refuse when orphan session alive")
	}
	if !strings.Contains(err.Error(), "still alive") {
		t.Errorf("expected error about still-alive session, got: %v", err)
	}
	// Queue file MUST still exist — operator needs to investigate.
	if _, err := os.Stat(filepath.Join(tmp, "queue", "spawn-fresh-"+old.ID+".json")); err != nil {
		t.Errorf("queue file should still exist, got: %v", err)
	}
}

func TestHandoff_RecoveryProbeAbortsOnCorruptedRecord(t *testing.T) {
	// Codex iter-8 P1: the recovery branch must distinguish
	// state.ErrNotFound (which triggers cleanup) from other Load
	// errors (corrupted JSON, perm error). A corrupted-JSON read
	// must NOT be treated as "old already archived" — that would
	// silently delete the journal and exit success while the agent
	// is still live.
	requireFakeTmux(t)
	tmp := setupFleetHome(t)

	// Seed a queue file with NewAgentID set (so the recovery probe
	// engages), then write a corrupted JSON for the old agent.
	if _, err := queue.WriteSpawnFresh(queue.SpawnFresh{
		OldAgentID: "corrupt1",
		HandoffDoc: "/some/doc.md",
		Project:    "p",
		TaskID:     "t",
		NewAgentID: "newrepl1",
		NewSession: "fleet-newrepl1",
	}); err != nil {
		t.Fatalf("seed queue: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "agents", "corrupt1.json"),
		[]byte("{not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := &bytes.Buffer{}
	err := runHandoff(&handoffOpts{
		oldID:       "corrupt1",
		graceMillis: 1,
	}, out, out)
	if err == nil {
		t.Fatal("expected handoff to abort on corrupted record, got success")
	}
	if !strings.Contains(err.Error(), "recovery probe") {
		t.Errorf("expected error to mention recovery probe, got: %v", err)
	}
	// Queue file MUST NOT have been deleted — operator needs to
	// investigate the corrupted record.
	if _, err := os.Stat(filepath.Join(tmp, "queue", "spawn-fresh-corrupt1.json")); err != nil {
		t.Errorf("queue file should still exist after probe failure, got: %v", err)
	}
}

func TestHandoff_ResumesCrashedHandoffWithoutDoubleSpawn(t *testing.T) {
	// Codex iter-6 P1: simulate the crash window between spawn (step
	// 7b) and queue.Delete (step 12). A retry must NOT spawn a
	// second replacement; it must complete kill+archive+delete via
	// resumeHandoff.
	requireFakeTmux(t)
	setupFleetHome(t)

	old := seedAgent(t)
	t.Cleanup(func() { _ = tmux.Kill(old.TmuxSession) })

	// Manually construct the post-crash state:
	//   - old still in agents/, old session still alive
	//   - replacement record + tmux session exist
	//   - queue file exists with NewAgentID populated
	repCwd := t.TempDir()
	replacement, err := agentSpawnForTest(t, repCwd, []string{"sleep", "120"}, old.Project, old.TaskID)
	if err != nil {
		t.Fatalf("seed replacement: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(replacement.TmuxSession) })

	docPath := "/some/handoffs/" + old.ID + "-stub.md"
	if _, err := queue.WriteSpawnFresh(queue.SpawnFresh{
		OldAgentID: old.ID,
		HandoffDoc: docPath,
		Project:    old.Project,
		TaskID:     old.TaskID,
		NewAgentID: replacement.ID,
		NewSession: replacement.TmuxSession,
	}); err != nil {
		t.Fatalf("seed queue: %v", err)
	}

	// Run handoff for the same oldID. Should detect the journal
	// entry, resume (no spawn), kill old, archive old, delete queue.
	out := &bytes.Buffer{}
	if err := runHandoff(&handoffOpts{
		oldID:       old.ID,
		graceMillis: 1,
	}, out, out); err != nil {
		t.Fatalf("resumed handoff: %v\n%s", err, out.String())
	}

	// Exactly one live agent remains: the replacement, NOT a third
	// double-spawn.
	live := listLive(t)
	if len(live) != 1 {
		t.Fatalf("expected 1 live agent post-resume, got %d", len(live))
	}
	if live[0].ID != replacement.ID {
		t.Errorf("expected resumed-into replacement %s, got %s", replacement.ID, live[0].ID)
	}
	// Output should signal the resume path so operator knows what
	// happened (vs a fresh handoff).
	if !strings.Contains(out.String(), "resumed") {
		t.Errorf("expected output to mention 'resumed':\n%s", out.String())
	}
}

func TestHandoff_ResumeDeliversPromptToReplacement(t *testing.T) {
	// Codex review iter-3 P1: resumeHandoff (operator-triggered crash
	// recovery, dispatched when a queue entry is found at handoff
	// start) was missing the SendInitialPrompt call, so a recovered
	// replacement got the kill/archive of the old but never received
	// "read your handoff doc" — sat idle until manual operator input.
	requireTmux(t)
	setupFleetHome(t)

	old := seedAgent(t)
	t.Cleanup(func() { _ = tmux.Kill(old.TmuxSession) })

	// Pre-spawn a replacement with a shell that echoes whatever
	// resumeHandoff types into it, so the test can assert on
	// captured pane content. DisableAutoResume defaults to false →
	// auto-resume fires.
	repCwd := t.TempDir()
	replacement, err := agentSpawnForTest(t, repCwd,
		[]string{"sh", "-c", "read line; echo GOT:$line; sleep 30"},
		old.Project, old.TaskID)
	if err != nil {
		t.Fatalf("seed replacement: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(replacement.TmuxSession) })

	// Use a real on-disk handoff doc path so ResumePrompt embeds it
	// verbatim — matches what fleet would write in production.
	now := time.Now().UTC()
	docPath, err := state.HandoffPath(old.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(docPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(docPath, []byte("stub doc"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := queue.WriteSpawnFresh(queue.SpawnFresh{
		OldAgentID: old.ID,
		HandoffDoc: docPath,
		Project:    old.Project,
		TaskID:     old.TaskID,
		NewAgentID: replacement.ID,
		NewSession: replacement.TmuxSession,
	}); err != nil {
		t.Fatalf("seed queue: %v", err)
	}

	out := &bytes.Buffer{}
	if err := runHandoff(&handoffOpts{
		oldID:       old.ID,
		graceMillis: 1,
	}, out, out); err != nil {
		t.Fatalf("resumed handoff: %v\n%s", err, out.String())
	}

	// Replacement still alive; pane contains the resume prompt
	// (echoed back as GOT:<prompt>). Strip newlines because tmux
	// capture-pane wraps long lines at terminal width.
	want := "GOT:Read your handoff doc at " + docPath
	deadline := time.Now().Add(2 * time.Second)
	var lastOut []byte
	for time.Now().Before(deadline) {
		captured, err := tmux.CapturePane(replacement.TmuxSession)
		if err == nil {
			lastOut = captured
			joined := strings.ReplaceAll(string(captured), "\n", "")
			if strings.Contains(joined, want) {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("resumeHandoff did not deliver resume prompt; want substring %q in:\n%s",
		want, string(lastOut))
}

func TestHandoff_DeadSession_ArchivesWithoutSpawn(t *testing.T) {
	// Smart [h] for orphan records: when the outgoing tmux session is
	// already dead (claude exited inside it), handoff archives the
	// record without spawning a replacement, writing a doc, or queueing.
	// The operator's intent is cleanup, not "continue this work" — there
	// is no work in flight to continue.
	requireFakeTmux(t)
	tmp := setupFleetHome(t)

	old := seedAgent(t)
	// Kill the tmux session so the outgoing agent looks orphaned.
	if err := tmux.Kill(old.TmuxSession); err != nil {
		t.Fatalf("seed kill: %v", err)
	}
	if tmux.HasSession(old.TmuxSession) {
		t.Fatalf("seed: session %s should be dead after kill", old.TmuxSession)
	}

	out := &bytes.Buffer{}
	if err := runHandoff(&handoffOpts{
		oldID:       old.ID,
		graceMillis: 1,
	}, out, out); err != nil {
		t.Fatalf("handoff on dead session: %v\n%s", err, out.String())
	}

	// Old live record gone.
	if _, err := os.Stat(filepath.Join(tmp, "agents", old.ID+".json")); !os.IsNotExist(err) {
		t.Errorf("old live record should be gone, stat err=%v", err)
	}
	// Old record archived.
	if _, err := os.Stat(filepath.Join(tmp, "agents", "archive", old.ID+".json")); err != nil {
		t.Errorf("old archive missing: %v", err)
	}
	// No replacement spawned.
	if n := len(listLive(t)); n != 0 {
		t.Errorf("expected 0 live agents after dead-session cleanup, got %d", n)
	}
	// No handoff doc written.
	if entries, _ := os.ReadDir(filepath.Join(tmp, "handoffs")); len(entries) > 0 {
		t.Errorf("dead-session cleanup should not write a handoff doc, got: %v", entries)
	}
	// No queue file left behind.
	if entries, _ := os.ReadDir(filepath.Join(tmp, "queue")); len(entries) > 0 {
		t.Errorf("dead-session cleanup should not leave queue files, got: %v", entries)
	}
	// Output explains what happened so the operator isn't confused by
	// the missing "handed off → <new id>" line.
	s := out.String()
	for _, want := range []string{old.ID, "session was dead", "no replacement spawned"} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q:\n%s", want, s)
		}
	}
}

func TestHandoff_DeadSession_RecoveryStillWinsWhenPendingExists(t *testing.T) {
	// If a previous handoff crashed after spawning the replacement
	// (queue file with NewAgentID set, replacement record + session
	// alive), a retry on a now-dead OUTGOING session must still take
	// the resume path — the dead-session short-circuit must not run
	// before the recovery probe and orphan the live replacement.
	requireTmux(t)
	setupFleetHome(t)

	old := seedAgent(t)
	t.Cleanup(func() { _ = tmux.Kill(old.TmuxSession) })

	// Spawn a real replacement record + session (simulating the
	// post-spawn pre-archive crash state).
	repCwd := t.TempDir()
	replacement, err := agentSpawnForTest(t, repCwd, []string{"sleep", "120"}, old.Project, old.TaskID)
	if err != nil {
		t.Fatalf("seed replacement: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(replacement.TmuxSession) })

	if _, err := queue.WriteSpawnFresh(queue.SpawnFresh{
		OldAgentID: old.ID,
		HandoffDoc: "/some/doc.md",
		Project:    old.Project,
		TaskID:     old.TaskID,
		NewAgentID: replacement.ID,
		NewSession: replacement.TmuxSession,
	}); err != nil {
		t.Fatalf("seed queue: %v", err)
	}

	// Now kill the outgoing session so it LOOKS like a dead-session
	// candidate. The recovery probe must still win.
	if err := tmux.Kill(old.TmuxSession); err != nil {
		t.Fatalf("kill old: %v", err)
	}

	out := &bytes.Buffer{}
	if err := runHandoff(&handoffOpts{
		oldID:       old.ID,
		graceMillis: 1,
	}, out, out); err != nil {
		t.Fatalf("resume handoff: %v\n%s", err, out.String())
	}

	// Replacement preserved as the single live agent (not orphaned).
	live := listLive(t)
	if len(live) != 1 {
		t.Fatalf("expected 1 live agent, got %d", len(live))
	}
	if live[0].ID != replacement.ID {
		t.Errorf("expected replacement %s alive, got %s", replacement.ID, live[0].ID)
	}
	// Output should mention "resumed", not the dead-session message.
	s := out.String()
	if !strings.Contains(s, "resumed") {
		t.Errorf("expected resume path output, got:\n%s", s)
	}
	if strings.Contains(s, "no replacement spawned") {
		t.Errorf("dead-session message must not fire when recovery applies:\n%s", s)
	}
}

// TestHandoff_ReplacementSpawnedWithRemoteControlFlag pins the
// handoff-remote-control-shell-wrapper-fix integration contract: when
// the outgoing record's persisted Command is the standard
// `["sh", "-c", "claude ..."]` shell wrapper, the replacement spawn's
// tmux session runs with `--remote-control "fleet-handoff-<new-id>"`
// injected into the body. This is the bug the fix addresses — the
// previous strict byte-equality matcher silently skipped injection
// when the persisted body drifted from the literal wrapper script.
//
// We use a wrapper whose body starts with `claude ` (the relaxed
// matcher's trigger) but substitutes `printf` for the real binary so
// the test runs even where claude is uninstalled AND the rewritten
// body is observable. printf prints its own argv to the pane, so
// capture-pane sees the injected flag verbatim. The trailing `cat`
// keeps the session alive past the spawn liveness check.
//
// The body string is a heredoc-style template: `claude` is the
// matched token (so the relaxed matcher triggers), but the line is
// preceded with `set -- ` to capture argv into the shell's positional
// params, then `printf '%s\n' "$@"` echoes them so the pane shows the
// effective argv. This sidesteps claude's TUI overwriting the pane.
func TestHandoff_ReplacementSpawnedWithRemoteControlFlag(t *testing.T) {
	enableRCBootstrapForTest(t)
	requireTmux(t)
	root := setupFleetHome(t)
	// Native model: injection is DEFAULT-ON for coord handoffs — no
	// marker setup needed (the legacy rc-enabled marker is ignored by
	// the gate; only the rc-disabled opt-out or the env-gate suppress).
	// The outgoing agent is the project's COORD (task_id ==
	// coord-<project>); after the D3 marker deletion that task shape is
	// the sole coord discriminator isCoordHandoffForProject gates on. A
	// coord handoff re-resolves cwd via the shared resolver, so seed a
	// bindable repo for "rainier".
	seedRecoveryRepo(t, root, "rainier")

	// Wrapper body that (a) starts with `claude ` (matcher trigger),
	// (b) is observable in the pane regardless of whether claude is
	// installed. Trick: `claude` is consumed as a no-op via `:` shell
	// builtin alias — `claude(){ printf '%s\n' "claude" "$@"; }`. The
	// matcher only inspects the BODY shape (`begins with "claude "`);
	// what's actually in the function table at runtime is up to us.
	wrapperCmd := []string{
		"sh", "-c",
		// Define a shell function named `claude` that echoes its
		// rewritten argv (so capture-pane can observe the flag), then
		// invoke `claude ...`. The relaxed matcher rewrites the
		// `claude ` token at the START of the body, after function
		// definition runs — the matcher doesn't introspect shell
		// semantics, just the literal first non-whitespace token.
		// To make the matcher fire, the body must START with
		// `claude `; we put the function definition BEFORE the
		// invocation, but the matcher only checks the leading token.
		// Workaround: the entire body must begin with `claude `, so we
		// invoke `claude` first and have `claude` itself a built-in
		// alias resolved by the shell. POSIX sh aliases don't carry
		// across `sh -c`, so use a wrapper script in PATH.
		//
		// Simplest robust approach: ship a tiny shim via the env. The
		// wrapper body is `claude --dangerously-skip-permissions; cat`
		// and PATH is set so `claude` resolves to a script we drop in
		// a temp dir.
		`claude --dangerously-skip-permissions; cat`,
	}
	originalCwd := t.TempDir()

	// Drop a fake `claude` shim in a temp dir, prepend to PATH for
	// this test. The shim echoes its argv (so capture-pane can see
	// the rewritten flag).
	shimDir := t.TempDir()
	shimPath := filepath.Join(shimDir, "claude")
	if err := os.WriteFile(shimPath, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0o755); err != nil {
		t.Fatalf("write shim: %v", err)
	}
	// FLEET_TMUX_SOCKET is per-test, but env vars set via t.Setenv
	// propagate into spawn.Spawn's tmux command (which inherits the
	// test process env). The new tmux session inherits PATH; the
	// `sh -c` body finds our shim before any system claude.
	t.Setenv("PATH", shimDir+":"+os.Getenv("PATH"))
	prevSpawn := handoffSpawnFn
	handoffSpawnFn = func(opts spawn.Options) (*agent.Record, error) {
		opts.DisableLeaseWrap = true
		return spawn.Spawn(opts)
	}
	t.Cleanup(func() { handoffSpawnFn = prevSpawn })

	// v0.12.1 (codex review iter-5 [P1]): the handoff inject is gated on
	// isCoordHandoffForProject so worker handoffs in RC-enabled projects
	// don't silently inherit --remote-control. This test exercises the
	// WITH-coord path (the contract it was written to pin) — the outgoing
	// agent's task_id is coord-<project>, which spawn.IsCoordSpawn (the
	// post-D3 coord discriminator) recognizes as the project's coord.
	first, err := agentSpawnForTest(t, originalCwd, wrapperCmd, "rainier", "coord-rainier")
	if err != nil {
		t.Fatalf("seed spawn: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(first.TmuxSession) })

	// Run handoff. Command/cwd inherited from the outgoing record.
	out := &bytes.Buffer{}
	if err := runHandoff(&handoffOpts{
		oldID:       first.ID,
		graceMillis: 1,
	}, out, out); err != nil {
		t.Fatalf("handoff: %v\n%s", err, out.String())
	}

	live := listLive(t)
	if len(live) != 1 {
		t.Fatalf("expected 1 live agent post-handoff, got %d", len(live))
	}
	rep := live[0]
	t.Cleanup(func() { _ = tmux.Kill(rep.TmuxSession) })

	// (a) Persisted record carries the CLEAN wrapperCmd (no
	// --remote-control polluting the next handoff's lineage).
	if len(rep.Command) != len(wrapperCmd) {
		t.Fatalf("rep.Command length: got %d want %d", len(rep.Command), len(wrapperCmd))
	}
	for i := range wrapperCmd {
		if rep.Command[i] != wrapperCmd[i] {
			t.Errorf("rep.Command[%d]: got %q want %q (persisted record must NOT carry --remote-control)",
				i, rep.Command[i], wrapperCmd[i])
		}
	}

	// (b) The pane shows the shim's argv echo, which includes the
	// `--remote-control "fleet-coord-<id>-<project>"` flag. The
	// shim prints each arg on its own line, so we grep for the
	// session-name literal directly.
	//
	// codex round-6 P1: post-v0.12 the only listener prefix is
	// `fleet-coord` (started by `fleet rc up`). The per-handoff
	// bash bootstrap that previously launched a daemon under
	// `fleet-handoff-<project>` is gone — replaced by operator-
	// instruction markdown in FirstAction. Injecting any other
	// prefix into the replacement coord would point at a daemon
	// the live listener can't see → silent pairing failure post-
	// handoff. So the rendered flag must use the coord shape:
	// `fleet-coord-<rep.ID>-rainier`.
	wantFlag := "fleet-coord-" + rep.ID + "-rainier"
	deadline := time.Now().Add(3 * time.Second)
	var lastOut []byte
	for time.Now().Before(deadline) {
		raw, capErr := exec.Command(
			"tmux", "-S", os.Getenv("FLEET_TMUX_SOCKET"),
			"capture-pane", "-pt", rep.TmuxSession,
		).Output()
		if capErr == nil {
			lastOut = raw
			if strings.Contains(string(raw), wantFlag) {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("expected pane to contain %q within deadline; got:\n%s",
		wantFlag, string(lastOut))
}

func TestHandoff_DocBodyContainsPlaceholders(t *testing.T) {
	requireFakeTmux(t)
	setupFleetHome(t)

	old := seedAgent(t)
	t.Cleanup(func() { _ = tmux.Kill(old.TmuxSession) })

	out := &bytes.Buffer{}
	if err := runHandoff(&handoffOpts{
		oldID:       old.ID,
		command:     []string{"sleep", "60"},
		graceMillis: 1,
	}, out, out); err != nil {
		t.Fatalf("handoff: %v", err)
	}

	live, _ := agent.List()
	if len(live) == 0 {
		t.Fatal("no live agent after handoff")
	}
	t.Cleanup(func() { _ = tmux.Kill(live[0].TmuxSession) })

	body, err := os.ReadFile(*live[0].LastHandoffPath)
	if err != nil {
		t.Fatalf("read doc: %v", err)
	}
	for _, want := range []string{
		"## Completed",
		"## Key Decisions",
		"## Docs (this session)",
		"## Open Questions",
		"## Next Steps (prioritized)",
		"_(operator-triggered handoff",
		`handoff_type: "manual"`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("doc missing %q:\n%s", want, string(body))
		}
	}
}

// TestHandoff_CoordReplacementLineageGetsCoordinatorPrompt regresses
// the handoff-coord-spawn-prompt-fix bug: the predecessor's
// ~/.fleet/projects/<p>/.locks/coordinator.lock NB-flock body keeps
// the OLD coord's 8-hex ID after a handoff because the replacement
// coord session never runs `/coordinator`. The TUI dashboard's
// project row reads that lock body for the "left-side" coord display,
// so it shows the predecessor's name (or empty) instead of the new
// coord's ID. The fix: make handoff replacement docs include a
// `/coordinator` instruction in the First Action (auto) section,
// parallel to the `/remote-control` instruction the doc already
// carries. /coordinator is idempotent — running it on a coord agent
// that already holds the flock is a no-op (NB-flock skips the
// already-held case), so universal injection in FirstAction is safe
// for non-coord handoffs too.
//
// End-to-end via runHandoff so we exercise the path the operator
// hits with `fleet handoff <coord-id>`. seedAgent dispatches with
// task_id="auth-fix" by default — but the FirstAction body is
// invariant of task_id (approach (a)), so any seeded agent surfaces
// the regression. A separate task_id="coord-<project>" lineage test
// would be necessary only if approach (b) were taken.
func TestHandoff_CoordReplacementLineageGetsCoordinatorPrompt(t *testing.T) {
	requireFakeTmux(t)
	setupFleetHome(t)

	old := seedAgent(t)
	t.Cleanup(func() { _ = tmux.Kill(old.TmuxSession) })

	out := &bytes.Buffer{}
	if err := runHandoff(&handoffOpts{
		oldID:       old.ID,
		command:     []string{"sleep", "60"},
		graceMillis: 1,
	}, out, out); err != nil {
		t.Fatalf("handoff: %v", err)
	}

	live, _ := agent.List()
	if len(live) == 0 {
		t.Fatal("no live agent after handoff")
	}
	t.Cleanup(func() { _ = tmux.Kill(live[0].TmuxSession) })

	body, err := os.ReadFile(*live[0].LastHandoffPath)
	if err != nil {
		t.Fatalf("read doc: %v", err)
	}
	doc := string(body)
	// Native model: RC is baked into the replacement's spawn argv, so
	// the doc carries a status note (--remote-control mention) instead
	// of a /remote-control instruction. /coordinator must still appear
	// — it restarts the supervisor loop so the new agent's ID lands in
	// the project's coordinator.lock body.
	if !strings.Contains(doc, "--remote-control") {
		t.Errorf("handoff doc missing native RC status note:\n%s", doc)
	}
	if !strings.Contains(doc, "`/coordinator`") {
		t.Errorf("handoff doc missing /coordinator instruction:\n%s", doc)
	}
}

// NOTE: the coord-spawn marker transfer tests (TransfersCoordMarker*,
// NoMarkerUpdate_*) were deleted with the D3 marker removal — the coord
// identity handoff now rides the coordinator LEASE (AtomicCoordSwap
// reserves the handoff, the winning standby bumps the epoch), covered by
// the lease/standby integration tests (handoff_lease_standby_integration_test.go).
// There is no marker file to transfer or leave untouched anymore.

// Codex P2 regression: retargetArchivedHandoffSuccessor rewrites the archived
// predecessor's SuccessorID so chain-follow (`fleet attach <old>`) resolves to
// the record that actually received the resume prompt. The stale-queue recovery
// path relies on this before dropping a superseded queued replacement; without
// it, attach chained to a just-deleted record.
func TestRetargetArchivedHandoffSuccessor_RewritesChain(t *testing.T) {
	setupFleetHome(t)

	old := agent.New(agent.NewID())
	old.Project = "rainier"
	old.TaskID = "coord-rainier"
	old.TmuxSession = tmux.SessionName(old.ID)
	old.SuccessorID = "droppedreplacement"
	if err := old.Write(); err != nil {
		t.Fatalf("write old: %v", err)
	}
	if err := old.Archive(); err != nil {
		t.Fatalf("archive old: %v", err)
	}

	const lockOwner = "actuallockowner"
	if err := retargetArchivedHandoffSuccessor(old.ID, lockOwner); err != nil {
		t.Fatalf("retarget: %v", err)
	}

	arc, err := agent.LoadArchive(old.ID)
	if err != nil {
		t.Fatalf("LoadArchive: %v", err)
	}
	if arc.SuccessorID != lockOwner {
		t.Fatalf("archive successor = %q, want retargeted %q", arc.SuccessorID, lockOwner)
	}
	if arc.ArchivedCause != agent.ArchivedCauseHandoff {
		t.Fatalf("archive cause = %q, want %q", arc.ArchivedCause, agent.ArchivedCauseHandoff)
	}

	// Empty successor is a programming error, not a silent no-op.
	if err := retargetArchivedHandoffSuccessor(old.ID, ""); err == nil {
		t.Fatal("expected error on empty successorID")
	}
}

// ---------------------------------------------------------------------------
// PR3 (DESIGN-coord-spawn-unified-standby §6) — the MANUAL `fleet handoff`
// live-coord path routes through the GracefulHandoff barrier
// (handoffGracefulSwapFn) instead of the bespoke AtomicCoordSwap +
// step-13 deliverHandoffResumePrompt sequence. The doc is injected INSIDE
// the barrier completion; runHandoff must not deliver a second time.
// ---------------------------------------------------------------------------

// seedGracefulHandoffOld seeds a live coord OLD agent. Its task_id is
// coord-<project>, which spawn.IsCoordSpawn recognizes as the project's
// coord (the coord-spawn marker is gone — D3).
func seedGracefulHandoffOld(t *testing.T, project, cwd string) *agent.Record {
	t.Helper()
	old := agent.New(agent.NewID())
	old.TaskID = "coord-" + project
	old.Project = project
	old.Cwd = cwd
	old.Command = []string{"sleep", "60"}
	old.TmuxSession = tmux.SessionName(old.ID)
	old.SpawnedAt = time.Now().UTC()
	old.LastActivityTS = old.SpawnedAt
	old.SupervisorPID = 4242
	if err := tmux.Spawn(old.TmuxSession, old.Cwd, old.Command,
		[]string{"FLEET_AGENT_ID=" + old.ID}); err != nil {
		t.Fatalf("spawn old coord: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(old.TmuxSession) })
	if err := old.Write(); err != nil {
		t.Fatalf("write old coord: %v", err)
	}
	if _, err := state.EnsureProjectInitialized(project); err != nil {
		t.Fatalf("EnsureProjectInitialized: %v", err)
	}
	return old
}

// forbidHandoffDeliveryOutsideBarrier fails the test if runHandoff polls the
// lock owner itself — on the graceful route the swap closure owns delivery.
func forbidHandoffDeliveryOutsideBarrier(t *testing.T) {
	t.Helper()
	origDeps := handoffDeliveryDepsFn
	t.Cleanup(func() { handoffDeliveryDepsFn = origDeps })
	handoffDeliveryDepsFn = func() handoffdelivery.Deps {
		deps := handoffdelivery.DefaultDeps()
		deps.CurrentOwner = func(string) (coordlock.Owner, bool) {
			t.Error("graceful route must not poll the lock owner outside the barrier")
			return coordlock.Owner{}, false
		}
		return deps
	}
}

func TestResumeHandoff_GracefulRoute_SwapsViaBarrier(t *testing.T) {
	requireFakeTmux(t)
	root := setupFleetHome(t)

	const project = "rainier"
	cwd := seedRecoveryRepo(t, root, project)
	old := seedGracefulHandoffOld(t, project, cwd)
	forbidHandoffDeliveryOutsideBarrier(t)

	// Journaled replacement: already spawned + alive + lease-wrapped, so the
	// recovery probe dispatches to resumeHandoff (crashed-mid-handoff case).
	newRec := agent.New(agent.NewID())
	newRec.TaskID = old.TaskID
	newRec.Project = project
	newRec.Cwd = cwd
	newRec.Command = old.Command
	newRec.TmuxSession = tmux.SessionName(newRec.ID)
	newRec.SpawnedAt = time.Now().UTC()
	newRec.LastActivityTS = newRec.SpawnedAt
	newRec.LeaseWrapped = true
	if err := tmux.Spawn(newRec.TmuxSession, newRec.Cwd, newRec.Command,
		[]string{"FLEET_AGENT_ID=" + newRec.ID}); err != nil {
		t.Fatalf("spawn journaled replacement: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(newRec.TmuxSession) })
	if err := newRec.Write(); err != nil {
		t.Fatalf("write journaled replacement: %v", err)
	}
	docPath, derr := state.HandoffPath(old.ID, time.Now().UTC())
	if derr != nil {
		t.Fatalf("HandoffPath: %v", derr)
	}
	if err := os.MkdirAll(filepath.Dir(docPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(docPath, []byte("# doc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := queue.WriteSpawnFresh(queue.SpawnFresh{
		OldAgentID: old.ID,
		HandoffDoc: docPath,
		Project:    project,
		TaskID:     old.TaskID,
		NewAgentID: newRec.ID,
		NewSession: newRec.TmuxSession,
	}); err != nil {
		t.Fatalf("WriteSpawnFresh: %v", err)
	}

	winner := agent.New(agent.NewID())
	winner.TaskID = old.TaskID
	winner.Project = project
	winner.Cwd = cwd
	winner.Command = old.Command
	winner.TmuxSession = tmux.SessionName(winner.ID)
	winner.SpawnedAt = time.Now().UTC()
	winner.LastActivityTS = winner.SpawnedAt
	if err := winner.Write(); err != nil {
		t.Fatalf("write winner: %v", err)
	}

	origElig := handoffGracefulEligibleFn
	origSwap := handoffGracefulSwapFn
	t.Cleanup(func() {
		handoffGracefulEligibleFn = origElig
		handoffGracefulSwapFn = origSwap
	})
	handoffGracefulEligibleFn = func(*agent.Record, bool) bool { return true }
	swapCalls := 0
	handoffGracefulSwapFn = func(o, n *agent.Record, gotDoc string, _ time.Duration,
		deliver func() (*agent.Record, error), _ io.Writer) (*agent.Record, error) {
		swapCalls++
		if o.ID != old.ID || n.ID != newRec.ID {
			t.Errorf("graceful swap got old=%s new=%s, want %s/%s", o.ID, n.ID, old.ID, newRec.ID)
		}
		if gotDoc != docPath {
			t.Errorf("graceful swap doc = %q, want %q", gotDoc, docPath)
		}
		if err := o.ArchiveWithHandoff(n.ID); err != nil {
			t.Fatalf("simulated retire: %v", err)
		}
		_ = tmux.Kill(o.TmuxSession)
		return winner, nil
	}

	out := &bytes.Buffer{}
	err := runHandoff(&handoffOpts{
		oldID:       old.ID,
		graceMillis: 1,
	}, out, out)
	if err != nil {
		t.Fatalf("resumeHandoff graceful route failed: %v\n%s", err, out.String())
	}
	if swapCalls != 1 {
		t.Fatalf("graceful swap ran %d times, want 1", swapCalls)
	}
	arc, aerr := agent.LoadArchive(old.ID)
	if aerr != nil {
		t.Fatalf("LoadArchive(%s): %v", old.ID, aerr)
	}
	if arc.SuccessorID != winner.ID {
		t.Fatalf("archive successor = %q, want lock winner %q", arc.SuccessorID, winner.ID)
	}
	if _, lerr := agent.Load(newRec.ID); lerr == nil {
		t.Fatalf("superseded replacement %s should be dropped", newRec.ID)
	}
	if !strings.Contains(out.String(), winner.ID) {
		t.Fatalf("output does not name the winner %s:\n%s", winner.ID, out.String())
	}
}

// codex PR3-completion iter-7 [P1]: the graceful route's lease-wrapped
// successor must propagate ErrNoOwnerObserved (queue stays pending for the next
// drain/attach), never the legacy direct-send fallback into the successor's
// supervisor pane.
func TestDeliverHandoffResumePrompt_LeaseWrapped_NoOwner_NoFallback(t *testing.T) {
	setupFleetHome(t)

	rep := agent.New(agent.NewID())
	rep.Project = "rainier"
	rep.TmuxSession = tmux.SessionName(rep.ID)
	rep.LeaseWrapped = true

	origDeps := handoffDeliveryDepsFn
	origTimeout := handoffDeliveryTimeout
	origPoll := handoffDeliveryPoll
	t.Cleanup(func() {
		handoffDeliveryDepsFn = origDeps
		handoffDeliveryTimeout = origTimeout
		handoffDeliveryPoll = origPoll
	})
	handoffDeliveryTimeout = 100 * time.Millisecond
	handoffDeliveryPoll = time.Millisecond
	handoffDeliveryDepsFn = func() handoffdelivery.Deps {
		deps := handoffdelivery.DefaultDeps()
		deps.CurrentOwner = func(string) (coordlock.Owner, bool) {
			return coordlock.Owner{}, false
		}
		deps.SendVerified = func(session, _ string) (bool, error) {
			t.Errorf("lease-wrapped delivery sent to %q; must stay pending", session)
			return true, nil
		}
		deps.Sleep = func(time.Duration) {}
		return deps
	}

	out := &bytes.Buffer{}
	delivered, err := deliverHandoffResumePrompt(rep.Project, true, "oldcoord", rep, "/tmp/handoff.md", out, out)
	if !errors.Is(err, handoffdelivery.ErrNoOwnerObserved) {
		t.Fatalf("want ErrNoOwnerObserved propagated (queue stays pending), got %v", err)
	}
	if delivered != nil {
		t.Fatalf("delivered = %+v on the no-owner path, want nil", delivered)
	}
}

func TestDeliverHandoffResumePrompt_LeaseWrapped_TargetsSuccessorAndInlinesSections(t *testing.T) {
	setupFleetHome(t)

	rep := agent.New("newcoord")
	rep.Project = "rainier"
	rep.TmuxSession = tmux.SessionName(rep.ID)
	rep.LeaseWrapped = true
	docPath := filepath.Join(t.TempDir(), "handoff.md")
	doc := "## Completed\nold work\n\n" +
		"## Key Decisions\n- target the spawned session\n\n" +
		"## Next Steps (prioritized)\n1. keep the queue until successor owns the lock\n\n" +
		"## Active Subagents\n_(none)_\n"
	if err := os.WriteFile(docPath, []byte(doc), 0o644); err != nil {
		t.Fatalf("write handoff doc: %v", err)
	}

	origDeps := handoffDeliveryDepsFn
	t.Cleanup(func() { handoffDeliveryDepsFn = origDeps })
	var sentSession, sentPrompt string
	handoffDeliveryDepsFn = func() handoffdelivery.Deps {
		deps := handoffdelivery.DefaultDeps()
		deps.CurrentOwner = func(string) (coordlock.Owner, bool) {
			return coordlock.Owner{AgentID: rep.ID, PID: 2002, PidStart: 22}, true
		}
		deps.LoadAgent = func(id string) (*agent.Record, error) {
			if id != rep.ID {
				t.Fatalf("LoadAgent id = %q, want %q", id, rep.ID)
			}
			return rep, nil
		}
		deps.WaitReady = func(string) error { return nil }
		deps.SessionAlive = func(string) (bool, error) { return true, nil }
		deps.SendVerified = func(session, prompt string) (bool, error) {
			sentSession, sentPrompt = session, prompt
			return true, nil
		}
		return deps
	}

	out := &bytes.Buffer{}
	delivered, err := deliverHandoffResumePrompt(rep.Project, true, "oldcoord", rep, docPath, out, out)
	if err != nil {
		t.Fatalf("deliverHandoffResumePrompt: %v\n%s", err, out.String())
	}
	if delivered == nil || delivered.ID != rep.ID {
		t.Fatalf("delivered = %+v, want successor %s", delivered, rep.ID)
	}
	if sentSession != rep.TmuxSession {
		t.Fatalf("sent session = %q, want owning successor session %q", sentSession, rep.TmuxSession)
	}
	for _, want := range []string{
		"Read your handoff doc at " + docPath,
		"## Key Decisions\\n- target the spawned session",
		"## Next Steps (prioritized)\\n1. keep the queue until successor owns the lock",
	} {
		if !strings.Contains(sentPrompt, want) {
			t.Fatalf("sent prompt missing %q:\n%s", want, sentPrompt)
		}
	}
}

// codex PR3-completion iter-8 [P1]: a graceful swap that retired OLD but
// timed out on delivery retries through runHandoff's archived-OLD recovery
// branch. With a LEASE-WRAPPED journaled replacement and no lock owner
// observed yet (standby mid-acquire), the recovery must keep the queue
// PENDING — the legacy direct-send fallback would type into the coord-run
// supervisor pane, report success, and consume the only durable inbox.
func TestHandoff_ArchivedOldRecovery_LeaseWrapped_NoOwner_PreservesQueue(t *testing.T) {
	requireFakeTmux(t)
	root := setupFleetHome(t)

	const project = "rainier"
	cwd := seedRecoveryRepo(t, root, project)

	// OLD already retired+archived by the crashed graceful run.
	old := agent.New(agent.NewID())
	old.TaskID = "coord-" + project
	old.Project = project
	old.Cwd = cwd
	old.Command = []string{"sleep", "60"}
	old.TmuxSession = tmux.SessionName(old.ID)
	if err := old.Write(); err != nil {
		t.Fatalf("write old: %v", err)
	}

	// Journaled LEASE-WRAPPED replacement, alive (a polling standby).
	newRec := agent.New(agent.NewID())
	newRec.TaskID = old.TaskID
	newRec.Project = project
	newRec.Cwd = cwd
	newRec.Command = []string{"sleep", "60"}
	newRec.TmuxSession = tmux.SessionName(newRec.ID)
	newRec.LeaseWrapped = true
	if err := tmux.Spawn(newRec.TmuxSession, newRec.Cwd, newRec.Command,
		[]string{"FLEET_AGENT_ID=" + newRec.ID}); err != nil {
		t.Fatalf("spawn replacement: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(newRec.TmuxSession) })
	if err := newRec.Write(); err != nil {
		t.Fatalf("write replacement: %v", err)
	}

	docPath := filepath.Join(t.TempDir(), "handoff.md")
	if err := os.WriteFile(docPath, []byte("# doc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	qp, err := queue.WriteSpawnFresh(queue.SpawnFresh{
		OldAgentID: old.ID,
		HandoffDoc: docPath,
		Project:    project,
		TaskID:     old.TaskID,
		NewAgentID: newRec.ID,
		NewSession: newRec.TmuxSession,
	})
	if err != nil {
		t.Fatalf("WriteSpawnFresh: %v", err)
	}
	if err := old.ArchiveWithHandoff(newRec.ID); err != nil {
		t.Fatalf("archive old: %v", err)
	}

	origDeps := handoffDeliveryDepsFn
	origTimeout := handoffDeliveryTimeout
	origPoll := handoffDeliveryPoll
	t.Cleanup(func() {
		handoffDeliveryDepsFn = origDeps
		handoffDeliveryTimeout = origTimeout
		handoffDeliveryPoll = origPoll
	})
	handoffDeliveryTimeout = 100 * time.Millisecond
	handoffDeliveryPoll = time.Millisecond
	var directSends int32
	handoffDeliveryDepsFn = func() handoffdelivery.Deps {
		deps := handoffdelivery.DefaultDeps()
		deps.CurrentOwner = func(string) (coordlock.Owner, bool) {
			return coordlock.Owner{}, false // standby mid-acquire, no owner yet
		}
		deps.SendVerified = func(session, _ string) (bool, error) {
			atomic.AddInt32(&directSends, 1)
			return true, nil
		}
		deps.Sleep = func(time.Duration) {}
		return deps
	}

	out := &bytes.Buffer{}
	err = runHandoff(&handoffOpts{oldID: old.ID, graceMillis: 1}, out, out)
	if err == nil || !strings.Contains(err.Error(), "delivery is still pending") {
		t.Fatalf("archived-OLD recovery with no owner must stay pending, got %v\n%s", err, out.String())
	}
	if _, statErr := os.Stat(qp); statErr != nil {
		t.Fatalf("queue must be preserved for the next retry: %v", statErr)
	}
	if n := atomic.LoadInt32(&directSends); n != 0 {
		t.Fatalf("owner-poll send ran %d times with no owner, want 0", n)
	}
}

// archivedRecoveryFixture holds the seeded state for the archived-OLD
// recovery consumption-gate tests (PR-2 Path A). OLD is already archived; its
// lease-wrapped replacement (newRec) is queued and alive to receive the
// transport push; winner is a rival record a subtest can hand the lock to.
type archivedRecoveryFixture struct {
	old     *agent.Record
	newRec  *agent.Record
	winner  *agent.Record
	qp      string
	docPath string
}

// seedArchivedRecoveryHandoff builds the shared archived-OLD recovery state so
// the consumption-gate variants below differ only in who owns the lock. The
// caller must install fake-tmux isolation (requireFakeTmux) itself — the
// test-isolation lint only sees the marker in the Test function body, not here.
func seedArchivedRecoveryHandoff(t *testing.T, project string) archivedRecoveryFixture {
	t.Helper()
	root := setupFleetHome(t)
	cwd := seedRecoveryRepo(t, root, project)
	if _, err := state.EnsureProjectInitialized(project); err != nil {
		t.Fatalf("EnsureProjectInitialized: %v", err)
	}

	old := agent.New(agent.NewID())
	old.TaskID = "coord-" + project
	old.Project = project
	old.Cwd = cwd
	old.Command = []string{"sleep", "60"}
	old.TmuxSession = tmux.SessionName(old.ID)
	if err := old.Write(); err != nil {
		t.Fatalf("write old: %v", err)
	}

	// Queued LEASE-WRAPPED standby: record present, session alive so it can
	// receive the transport push regardless of who wins the lock.
	newRec := agent.New(agent.NewID())
	newRec.TaskID = old.TaskID
	newRec.Project = project
	newRec.Cwd = cwd
	newRec.Command = []string{"sleep", "60"}
	newRec.TmuxSession = tmux.SessionName(newRec.ID)
	newRec.LeaseWrapped = true
	if err := tmux.Spawn(newRec.TmuxSession, newRec.Cwd, newRec.Command,
		[]string{"FLEET_AGENT_ID=" + newRec.ID}); err != nil {
		t.Fatalf("spawn replacement: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(newRec.TmuxSession) })
	if err := newRec.Write(); err != nil {
		t.Fatalf("write replacement: %v", err)
	}

	// A rival coord record a subtest can name as the lock owner.
	winner := agent.New(agent.NewID())
	winner.TaskID = old.TaskID
	winner.Project = project
	winner.Cwd = cwd
	winner.Command = old.Command
	winner.TmuxSession = tmux.SessionName(winner.ID)
	if err := winner.Write(); err != nil {
		t.Fatalf("write winner: %v", err)
	}

	docPath := filepath.Join(t.TempDir(), "handoff.md")
	if err := os.WriteFile(docPath, []byte("# doc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	qp, err := queue.WriteSpawnFresh(queue.SpawnFresh{
		OldAgentID: old.ID,
		HandoffDoc: docPath,
		Project:    project,
		TaskID:     old.TaskID,
		NewAgentID: newRec.ID,
		NewSession: newRec.TmuxSession,
	})
	if err != nil {
		t.Fatalf("WriteSpawnFresh: %v", err)
	}
	if err := old.ArchiveWithHandoff(newRec.ID); err != nil {
		t.Fatalf("archive old: %v", err)
	}
	return archivedRecoveryFixture{old: old, newRec: newRec, winner: winner, qp: qp, docPath: docPath}
}

// installArchivedRecoveryDeps points delivery at owner and records the session
// keys were pushed to. Returns the sent-session pointer for assertions.
func installArchivedRecoveryDeps(t *testing.T, owner coordlock.Owner) *string {
	t.Helper()
	origDeps := handoffDeliveryDepsFn
	origTimeout := handoffDeliveryTimeout
	origPoll := handoffDeliveryPoll
	t.Cleanup(func() {
		handoffDeliveryDepsFn = origDeps
		handoffDeliveryTimeout = origTimeout
		handoffDeliveryPoll = origPoll
	})
	handoffDeliveryTimeout = time.Second
	handoffDeliveryPoll = time.Millisecond
	sentSession := new(string)
	handoffDeliveryDepsFn = func() handoffdelivery.Deps {
		deps := handoffdelivery.DefaultDeps()
		deps.CurrentOwner = func(string) (coordlock.Owner, bool) { return owner, true }
		deps.WaitReady = func(string) error { return nil }
		deps.SessionAlive = func(string) (bool, error) { return true, nil }
		deps.SendVerified = func(session, _ string) (bool, error) {
			*sentSession = session
			return true, nil
		}
		deps.Sleep = func(time.Duration) {}
		return deps
	}
	return sentSession
}

// PR-2 Path A (Option A): a lease-failover winner (a live coord that took the
// lease, distinct from both the retired OLD and the queued replacement) is the
// coord that owns the role — delivery follows it, finalizes on it (archive
// retargeted, queue consumed), and supersedes the losing replacement. This is
// the pre-PR-2 "deliver to the live winner" behavior Option A restores.
func TestHandoff_ArchivedOldRecovery_DifferentWinner_DeliversToWinner(t *testing.T) {
	requireFakeTmux(t)
	fx := seedArchivedRecoveryHandoff(t, "rainier")
	sentSession := installArchivedRecoveryDeps(t,
		coordlock.Owner{AgentID: fx.winner.ID, PID: 9001, PidStart: 99})

	out := &bytes.Buffer{}
	if err := runHandoff(&handoffOpts{oldID: fx.old.ID, graceMillis: 1}, out, out); err != nil {
		t.Fatalf("live lock winner should be delivered to + finalized: %v\n%s", err, out.String())
	}
	if *sentSession != fx.winner.TmuxSession {
		t.Fatalf("delivery went to %q, want live lock winner %q", *sentSession, fx.winner.TmuxSession)
	}
	if _, statErr := os.Stat(fx.qp); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("queue should be consumed after winner delivery, stat err=%v", statErr)
	}
	arc, err := agent.LoadArchive(fx.old.ID)
	if err != nil {
		t.Fatalf("LoadArchive: %v", err)
	}
	if arc.SuccessorID != fx.winner.ID {
		t.Fatalf("archive successor = %q, want retargeted winner %q", arc.SuccessorID, fx.winner.ID)
	}
}

// PR-2 Path A (T14 integration): when the intended successor OWNS the lock, the
// consumption gate passes — the queue file is consumed and runHandoff succeeds.
// Sibling of the pending case above; both feed the same recovery branch so the
// gate's two outcomes are asserted end-to-end, not just at the delivery unit.
func TestHandoff_ArchivedOldRecovery_SuccessorOwnsLock_ConsumesQueue(t *testing.T) {
	requireFakeTmux(t)
	fx := seedArchivedRecoveryHandoff(t, "rainier")
	sentSession := installArchivedRecoveryDeps(t,
		coordlock.Owner{AgentID: fx.newRec.ID, PID: 2002, PidStart: 22})

	out := &bytes.Buffer{}
	if err := runHandoff(&handoffOpts{oldID: fx.old.ID, graceMillis: 1}, out, out); err != nil {
		t.Fatalf("successor-owns-lock delivery should consume the queue: %v\n%s", err, out.String())
	}
	if *sentSession != fx.newRec.TmuxSession {
		t.Fatalf("delivery went to %q, want named successor %q", *sentSession, fx.newRec.TmuxSession)
	}
	if _, statErr := os.Stat(fx.qp); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("queue should be consumed after successor-owns-lock delivery, stat err=%v", statErr)
	}
}
