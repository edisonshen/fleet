package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/queue"
	"github.com/edisonshen/fleet/internal/spawn"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tmux"
)

func setupFleetHome(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	return tmp
}

func requireTmux(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed; skipping integration test")
	}
	// In-process random suffix for FLEET_TMUX_SOCKET — no external
	// `openssl` dep so tests pass anywhere tmux + Go work.
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	t.Setenv("FLEET_TMUX_SOCKET", "/tmp/fleet-test-"+hex.EncodeToString(b[:])+".sock")
	// runHandoff calls spawn.SendInitialPrompt between step 8a and
	// step 9; the helper polls pane stability before typing. Pin
	// small windows so tests don't pay the production 30 s cap on
	// shells that may not stabilize predictably.
	t.Setenv("FLEET_INITIAL_PROMPT_STABLE_MS", "100")
	t.Setenv("FLEET_INITIAL_PROMPT_MAX_MS", "1000")
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
	// captured pane content.
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
