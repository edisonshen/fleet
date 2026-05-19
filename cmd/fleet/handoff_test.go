package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/queue"
	"github.com/edisonshen/fleet/internal/rc"
	"github.com/edisonshen/fleet/internal/spawn"
	"github.com/edisonshen/fleet/internal/state"
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
	requireTmux(t)
	setupFleetHome(t)

	out := &bytes.Buffer{}
	err := runHandoff(&handoffOpts{
		oldID:       "ghostbas",
		command:     []string{"sleep", "60"},
		graceMillis: 0,
	}, out, out)
	if err == nil {
		t.Error("expected error for unknown agent")
	}
}

func TestHandoff_HappyPath(t *testing.T) {
	requireTmux(t)
	tmp := setupFleetHome(t)

	old := seedAgent(t)
	t.Cleanup(func() { _ = tmux.Kill(old.TmuxSession) })

	out := &bytes.Buffer{}
	if err := runHandoff(&handoffOpts{
		oldID:       old.ID,
		command:     []string{"sleep", "60"},
		graceMillis: 0, // no sleep in tests
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

func TestHandoff_ChainGrowsAcrossSequentialHandoffs(t *testing.T) {
	requireTmux(t)
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
			graceMillis: 0,
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
	requireTmux(t)
	setupFleetHome(t)

	old := seedAgent(t)
	t.Cleanup(func() { _ = tmux.Kill(old.TmuxSession) })

	// First handoff: succeeds, archives the agent.
	out1 := &bytes.Buffer{}
	if err := runHandoff(&handoffOpts{
		oldID:       old.ID,
		command:     []string{"sleep", "60"},
		graceMillis: 0,
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
		graceMillis: 0,
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
	requireTmux(t)
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
		graceMillis: 0,
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
		graceMillis: 0,
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
	requireTmux(t)
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
		graceMillis: 0,
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
		TaskID:  taskID,
		Project: project,
		Cwd:     cwd,
		Command: command,
	})
}

func TestHandoff_ResumeHandoff_CoordMarker_WritesBeforeReadinessWait(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)

	old := seedAgent(t)
	t.Cleanup(func() { _ = tmux.Kill(old.TmuxSession) })

	// Seed marker at old.ID so isCoordSwap fires on the recovery path.
	if _, err := state.EnsureProjectInitialized(old.Project); err != nil {
		t.Fatalf("EnsureProjectInitialized: %v", err)
	}
	if err := state.WriteCoordSpawnMarker(old.Project, old.ID); err != nil {
		t.Fatalf("seed marker: %v", err)
	}

	// Pre-spawn a live replacement (simulating the crash window
	// between original spawn and queue.Delete).
	replacement, err := agentSpawnForTest(t, t.TempDir(),
		[]string{"sleep", "120"}, old.Project, old.TaskID)
	if err != nil {
		t.Fatalf("seed replacement: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(replacement.TmuxSession) })

	docPath := "/some/handoffs/" + old.ID + "-stub.md"
	if _, err := queue.WriteSpawnFresh(queue.SpawnFresh{
		SchemaVersion: queue.SchemaVersion,
		OldAgentID:    old.ID,
		HandoffDoc:    docPath,
		Project:       old.Project,
		TaskID:        old.TaskID,
		NewAgentID:    replacement.ID,
		NewSession:    replacement.TmuxSession,
	}); err != nil {
		t.Fatalf("seed queue: %v", err)
	}

	out := &bytes.Buffer{}
	if err := runHandoff(&handoffOpts{
		oldID:       old.ID,
		graceMillis: 0,
	}, out, out); err != nil {
		t.Fatalf("runHandoff: %v\n%s", err, out.String())
	}

	// Marker must land at replacement.ID — AtomicCoordSwap's commit
	// point. Pre-fix iter-13: same end state, but the marker spent
	// the wait window stuck at old.ID. Test guards the end state;
	// the structural defense is the code order in resumeHandoff.
	got := state.ReadCoordSpawnMarker(old.Project)
	if got != replacement.ID {
		t.Errorf("coord marker not at replacement after resumeHandoff: got %q want %q",
			got, replacement.ID)
	}
}

// TestHandoff_RecoveryProbe_StaleMarker_RolledBackBeforeRespawn pins
// codex iter-12 [P1] on the manual-handoff recovery path
// (cmd/fleet/handoff.go:378-387). When the recovery probe finds a stale
// replacement record with a dead tmux session, it drops the record and
// falls through to the normal spawn flow. A previous coord-swap attempt
// may have committed marker → staleNew.ID before failing; without
// rolling back the marker, the fall-through's isCoordSwap detection at
// line 693-694 (`marker == oldRec.ID`) returns false, the inline retire
// path runs (no AtomicCoordSwap), and the marker is left pointing at
// the deleted ID — letting `[a]` spawn a duplicate coord.
//
// Post-fix: the cleanup branch calls
// handoffop.RollbackCoordMarkerIfPointingAt before the fall-through.
// The marker steps back to oldRec.ID, isCoordSwap fires on the retry,
// AtomicCoordSwap commits marker → fresh replacement's ID.
func TestHandoff_RecoveryProbe_StaleMarker_RolledBackBeforeRespawn(t *testing.T) {
	requireTmux(t)
	tmp := setupFleetHome(t)

	old := seedAgent(t)
	t.Cleanup(func() { _ = tmux.Kill(old.TmuxSession) })

	// Seed coord marker → old.ID (a real coord setup) THEN stomp it to
	// a fake "previous attempt" newAgentID, simulating the stale
	// marker a prior crashed swap would leave behind.
	if _, err := state.EnsureProjectInitialized(old.Project); err != nil {
		t.Fatalf("EnsureProjectInitialized: %v", err)
	}
	const staleNewID = "stalenew"
	if err := state.WriteCoordSpawnMarker(old.Project, staleNewID); err != nil {
		t.Fatalf("seed stale marker: %v", err)
	}

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

	// Run handoff with --force-replacement so the iter-14 P1 refusal
	// gate (which protects against duplicate-coord respawns when OLD
	// is still alive) is bypassed. This test specifically exercises
	// the iter-12 P1 rollback path; the iter-14 refusal gate is
	// covered by TestHandoff_RecoveryProbe_OldCoordAlive_RefusesRespawn
	// below.
	out := &bytes.Buffer{}
	if err := runHandoff(&handoffOpts{
		oldID:            old.ID,
		cwd:              old.Cwd,
		command:          []string{"sleep", "60"},
		graceMillis:      0,
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

	// Marker must point at the FRESH replacement, NOT the deleted
	// staleNewID. Pre-fix this assertion failed because isCoordSwap
	// went false on the retry (marker == staleNewID, not == old.ID)
	// and the inline retire path never moved the marker.
	got := state.ReadCoordSpawnMarker(old.Project)
	if got != fresh.ID {
		t.Errorf("coord marker not at fresh replacement: got %q want %q (staleNewID=%q, old.ID=%q)",
			got, fresh.ID, staleNewID, old.ID)
	}
}

// TestHandoff_RecoveryProbe_OldCoordAlive_RefusesRespawn pins codex
// iter-14 [P1] on the manual-handoff recovery path
// (cmd/fleet/handoff.go:378-410). When AtomicCoordSwap preserves the
// queue on ErrOrphanSurvived / ErrOldKillProbeAmbiguous, the next
// `fleet handoff` invocation lands in the recovery probe with newRec
// session dead but OLD still alive. Auto-respawning would create a
// duplicate coord.
//
// Post-fix: refuse-and-preserve when marker resolves to oldRec.ID or
// newRec.ID AND OLD's session is alive. Operator can pass
// --force-replacement once OLD is confirmed dead.
func TestHandoff_RecoveryProbe_OldCoordAlive_RefusesRespawn(t *testing.T) {
	requireTmux(t)
	tmp := setupFleetHome(t)

	old := seedAgent(t)
	t.Cleanup(func() { _ = tmux.Kill(old.TmuxSession) })

	if _, err := state.EnsureProjectInitialized(old.Project); err != nil {
		t.Fatalf("EnsureProjectInitialized: %v", err)
	}
	const staleNewID = "stalenew"
	if err := state.WriteCoordSpawnMarker(old.Project, staleNewID); err != nil {
		t.Fatalf("seed marker: %v", err)
	}

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

	// Run WITHOUT --force-replacement. Must refuse — OLD's session is
	// alive (seedAgent left it running), marker == newRec.ID (coord
	// swap journal), so the iter-14 gate fires.
	out := &bytes.Buffer{}
	err := runHandoff(&handoffOpts{
		oldID:       old.ID,
		cwd:         old.Cwd,
		command:     []string{"sleep", "60"},
		graceMillis: 0,
	}, out, out)
	if err == nil {
		t.Fatalf("expected handoff to refuse when OLD coord still alive; got nil\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "still alive") || !strings.Contains(err.Error(), "duplicate coord") {
		t.Errorf("expected refusal mentioning 'still alive' + 'duplicate coord'; got: %v", err)
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
	if got := state.ReadCoordSpawnMarker(old.Project); got != staleNewID {
		t.Errorf("marker mutated on refusal: got %q want %q", got, staleNewID)
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
		graceMillis: 0,
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
	requireTmux(t)
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
		graceMillis: 0,
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
	requireTmux(t)
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
		graceMillis: 0,
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
	requireTmux(t)
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
		graceMillis: 0,
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
		graceMillis: 0,
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
	requireTmux(t)
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
		graceMillis: 0,
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
		graceMillis: 0,
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
	setupFleetHome(t)
	// v0.12: the handoff path now gates --remote-control injection
	// on the per-project rc-enabled marker (DESIGN-rc-listener-
	// lifecycle.md §"Attach-surface gates" I2). Without the marker
	// the replacement spawn correctly does NOT carry the flag. The
	// contract under test here is the WITH-marker path, so opt in
	// for the duration of the test.
	if err := rc.WriteMarker("rainier"); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}
	t.Cleanup(func() { _ = rc.RemoveMarker("rainier") })

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

	first, err := agentSpawnForTest(t, originalCwd, wrapperCmd, "rainier", "auth-fix")
	if err != nil {
		t.Fatalf("seed spawn: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(first.TmuxSession) })

	// v0.12.1 (codex review iter-5 [P1]): the handoff inject is now
	// gated on isCoordHandoffForProject so worker handoffs in
	// RC-enabled projects don't silently inherit --remote-control.
	// This test exercises the WITH-coord path (the contract it was
	// written to pin); seed the coord-spawn marker pointing at the
	// outgoing agent so it counts as the project's coord.
	if _, err := state.EnsureProjectInitialized("rainier"); err != nil {
		t.Fatalf("EnsureProjectInitialized: %v", err)
	}
	if err := state.WriteCoordSpawnMarker("rainier", first.ID); err != nil {
		t.Fatalf("seed coord-spawn marker: %v", err)
	}

	// Run handoff. Command/cwd inherited from the outgoing record.
	out := &bytes.Buffer{}
	if err := runHandoff(&handoffOpts{
		oldID:       first.ID,
		graceMillis: 0,
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
	requireTmux(t)
	setupFleetHome(t)

	old := seedAgent(t)
	t.Cleanup(func() { _ = tmux.Kill(old.TmuxSession) })

	out := &bytes.Buffer{}
	if err := runHandoff(&handoffOpts{
		oldID:       old.ID,
		command:     []string{"sleep", "60"},
		graceMillis: 0,
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
		"## Files Modified",
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
	requireTmux(t)
	setupFleetHome(t)

	old := seedAgent(t)
	t.Cleanup(func() { _ = tmux.Kill(old.TmuxSession) })

	out := &bytes.Buffer{}
	if err := runHandoff(&handoffOpts{
		oldID:       old.ID,
		command:     []string{"sleep", "60"},
		graceMillis: 0,
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
	// Both slash commands must appear. /remote-control bootstraps the
	// daemon; /coordinator restarts the supervisor loop so the new
	// agent's ID lands in the project's coordinator.lock body.
	if !strings.Contains(doc, "`/remote-control`") {
		t.Errorf("handoff doc missing /remote-control instruction:\n%s", doc)
	}
	if !strings.Contains(doc, "`/coordinator`") {
		t.Errorf("handoff doc missing /coordinator instruction:\n%s", doc)
	}
	// Order: remote-control must appear first so the freshly-spawned
	// chat session attaches to the operator's mobile pairing BEFORE
	// the supervisor's startup output begins streaming.
	rcIdx := strings.Index(doc, "`/remote-control`")
	coordIdx := strings.Index(doc, "`/coordinator`")
	if rcIdx >= coordIdx {
		t.Errorf("expected /remote-control before /coordinator; rc=%d coord=%d in:\n%s",
			rcIdx, coordIdx, doc)
	}
}

// -- coord-spawn marker transfer (bug coord-marker-transfer-on-46a3) --------
//
// Same intent as the handoffop tests: when the OLD agent IS the
// project's coord (marker file points at oldRec.ID), the interactive
// `fleet handoff` path must re-point the marker at the replacement.
// Non-coord agents leave the marker untouched.

// TestHandoff_TransfersCoordMarkerWhenOldWasCoord — happy path: marker
// = old.ID before handoff, marker = newRec.ID after.
func TestHandoff_TransfersCoordMarkerWhenOldWasCoord(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)

	old := seedAgent(t)
	t.Cleanup(func() { _ = tmux.Kill(old.TmuxSession) })

	if _, err := state.EnsureProjectInitialized(old.Project); err != nil {
		t.Fatalf("EnsureProjectInitialized: %v", err)
	}
	if err := state.WriteCoordSpawnMarker(old.Project, old.ID); err != nil {
		t.Fatalf("seed marker: %v", err)
	}

	out := &bytes.Buffer{}
	if err := runHandoff(&handoffOpts{
		oldID:       old.ID,
		command:     []string{"sleep", "60"},
		graceMillis: 0,
	}, out, out); err != nil {
		t.Fatalf("handoff: %v\n%s", err, out.String())
	}

	live, err := agent.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(live) != 1 {
		t.Fatalf("expected 1 live agent after handoff, got %d", len(live))
	}
	newRec := live[0]
	t.Cleanup(func() { _ = tmux.Kill(newRec.TmuxSession) })

	got := state.ReadCoordSpawnMarker(old.Project)
	if got != newRec.ID {
		t.Errorf("coord marker not transferred: got %q want %q (old.ID=%q)",
			got, newRec.ID, old.ID)
	}
}

// TestHandoff_NoMarkerUpdate_WhenOldWasNotCoord — marker points at an
// unrelated agent ID; handoff of old must NOT mutate it.
func TestHandoff_NoMarkerUpdate_WhenOldWasNotCoord(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)

	old := seedAgent(t)
	t.Cleanup(func() { _ = tmux.Kill(old.TmuxSession) })

	const unrelatedID = "abcdef12"
	if _, err := state.EnsureProjectInitialized(old.Project); err != nil {
		t.Fatalf("EnsureProjectInitialized: %v", err)
	}
	if err := state.WriteCoordSpawnMarker(old.Project, unrelatedID); err != nil {
		t.Fatalf("seed marker: %v", err)
	}

	out := &bytes.Buffer{}
	if err := runHandoff(&handoffOpts{
		oldID:       old.ID,
		command:     []string{"sleep", "60"},
		graceMillis: 0,
	}, out, out); err != nil {
		t.Fatalf("handoff: %v\n%s", err, out.String())
	}

	live, _ := agent.List()
	if len(live) != 1 {
		t.Fatalf("expected 1 live agent, got %d", len(live))
	}
	t.Cleanup(func() { _ = tmux.Kill(live[0].TmuxSession) })

	if got := state.ReadCoordSpawnMarker(old.Project); got != unrelatedID {
		t.Errorf("unrelated coord marker mutated: got %q want %q", got, unrelatedID)
	}
}

// TestHandoff_NoMarkerUpdate_WhenNoMarkerExists — no marker file
// before handoff; no marker file after, no error.
func TestHandoff_NoMarkerUpdate_WhenNoMarkerExists(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)

	old := seedAgent(t)
	t.Cleanup(func() { _ = tmux.Kill(old.TmuxSession) })

	out := &bytes.Buffer{}
	if err := runHandoff(&handoffOpts{
		oldID:       old.ID,
		command:     []string{"sleep", "60"},
		graceMillis: 0,
	}, out, out); err != nil {
		t.Fatalf("handoff: %v\n%s", err, out.String())
	}

	live, _ := agent.List()
	if len(live) != 1 {
		t.Fatalf("expected 1 live agent, got %d", len(live))
	}
	t.Cleanup(func() { _ = tmux.Kill(live[0].TmuxSession) })

	if got := state.ReadCoordSpawnMarker(old.Project); got != "" {
		t.Errorf("marker created from nothing: got %q want empty", got)
	}
}
