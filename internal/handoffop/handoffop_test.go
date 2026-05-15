package handoffop

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/queue"
	"github.com/edisonshen/fleet/internal/spawn"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/testutil/tmuxtest"
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

// requireTmux delegates socket isolation to tmuxtest.RequireTmux (the
// canonical helper at internal/testutil/tmuxtest) and adds the
// handoff-specific env pins for fast tests. Postmortem 2026-05-14
// (orphan tmux leak) + 2026-05-15 follow-up: tmux.Spawn under
// `go test` refuses to use the default socket; tmuxtest.RequireTmux
// is the lint-recognized isolation marker.
func requireTmux(t *testing.T) {
	t.Helper()
	tmuxtest.RequireTmux(t)
	// retireOldAgent calls spawn.SendInitialPrompt, which polls the
	// pane for stability. Production windows (500 ms stable / 30 s
	// max) would balloon the suite; tests pin small values that
	// converge fast on the synthetic shell commands seedAgent uses.
	t.Setenv("FLEET_INITIAL_PROMPT_STABLE_MS", "100")
	t.Setenv("FLEET_INITIAL_PROMPT_MAX_MS", "1000")
	// Issue #65: post-stability buffer (default 1.5 s), bumped
	// prompt-enter delay (default 1 s), and post-send verify/retry
	// delays (0.5/1.5 s) all need to be pinned for fast tests.
	// Production behavior is verified in spawn_test.go.
	t.Setenv("FLEET_POST_READY_BUFFER_MS", "0")
	t.Setenv("FLEET_POST_SEND_VERIFY_MS", "0")
	t.Setenv("FLEET_POST_SEND_RETRY_MS", "0")
	t.Setenv("FLEET_PROMPT_ENTER_DELAY_MS", "50")
	// pid-resolver fallback budget — production polls up to 10s for
	// claude to exec inside the wrapper shell; tests use synthetic
	// commands where no claude descendant exists, so we'd pay the
	// full timeout per test without this pin.
	t.Setenv("FLEET_PID_RESOLVE_S", "1")
}

// spawnSeedAgent stands in for `fleet dispatch`: seeds an agent record
// and a long-lived tmux session that subsequent Resume calls operate on.
// Returns the seeded record. The handoffop package can't import
// cmd/fleet, so this duplicates the dispatch helper minimally.
func spawnSeedAgent(t *testing.T) *agent.Record {
	t.Helper()
	now := time.Now().UTC()
	rec := agent.New(agent.NewID())
	rec.TaskID = "auth-fix"
	rec.Project = "rainier"
	rec.SpawnedAt = now
	rec.LastActivityTS = now
	rec.Cwd = t.TempDir()
	rec.Command = []string{"sleep", "60"}
	rec.TmuxSession = tmux.SessionName(rec.ID)

	if err := tmux.Spawn(rec.TmuxSession, rec.Cwd, rec.Command,
		[]string{"FLEET_AGENT_ID=" + rec.ID}); err != nil {
		t.Fatalf("tmux.Spawn: %v", err)
	}
	rec.PID = 0 // not asserted; tmux owns the process
	if err := rec.Write(); err != nil {
		_ = tmux.Kill(rec.TmuxSession)
		t.Fatalf("agent.Write: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(rec.TmuxSession) })
	return rec
}

// writeSkillQueue mimics what fleet-guard's handoff.py writes: a queue file
// with NewAgentID + NewSession pre-allocated and a doc path that already
// exists on disk.
func writeSkillQueue(t *testing.T, oldRec *agent.Record) (req queue.SpawnFresh, queuePath, docPath string) {
	t.Helper()
	now := time.Now().UTC()

	dp, err := state.HandoffPath(oldRec.ID, now)
	if err != nil {
		t.Fatalf("HandoffPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(dp), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dp, []byte(`---
agent_id: "`+oldRec.ID+`"
task_id: "auth-fix"
project: "rainier"
context_pct_at_handoff: 55
previous_handoff: null
handoff_number: 1
timestamp: "`+now.Format(time.RFC3339)+`"
handoff_type: "auto-yellow"
---

## Completed
captured tmux pane

## Key Decisions
_(operator-triggered handoff — fill in before resuming)_
`), 0o644); err != nil {
		t.Fatal(err)
	}

	newID := agent.NewID()
	req = queue.SpawnFresh{
		// Set explicitly so the returned value matches what
		// WriteSpawnFresh persists on disk — the test wants the
		// in-memory req and file to agree (auto-resume gates the
		// drain on req.SchemaVersion >= 2).
		SchemaVersion: queue.SchemaVersion,
		OldAgentID:    oldRec.ID,
		HandoffDoc:    dp,
		Project:       oldRec.Project,
		TaskID:        oldRec.TaskID,
		NewAgentID:    newID,
		NewSession:    tmux.SessionName(newID),
	}
	qp, err := queue.WriteSpawnFresh(req)
	if err != nil {
		t.Fatalf("WriteSpawnFresh: %v", err)
	}
	return req, qp, dp
}

// -- happy path: skill wrote queue, no replacement yet ----------------------

func TestResume_SkillDrivenSpawnsAndRetires(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)
	oldRec := spawnSeedAgent(t)
	req, qp, _ := writeSkillQueue(t, oldRec)

	out := &bytes.Buffer{}
	if err := Resume(req, qp, 0, out, out); err != nil {
		t.Fatalf("Resume: %v\n%s", err, out.String())
	}

	// Old session is gone, old record is archived.
	if tmux.HasSession(oldRec.TmuxSession) {
		t.Errorf("old session %s still alive after drain", oldRec.TmuxSession)
	}
	if _, err := agent.Load(oldRec.ID); err == nil {
		t.Errorf("old record %s not archived", oldRec.ID)
	}

	// Replacement was spawned with the pre-allocated ID and is alive.
	newRec, err := agent.Load(req.NewAgentID)
	if err != nil {
		t.Fatalf("load new record: %v", err)
	}
	if !tmux.HasSession(newRec.TmuxSession) {
		t.Errorf("new session %s not alive after drain", newRec.TmuxSession)
	}
	t.Cleanup(func() { _ = tmux.Kill(newRec.TmuxSession) })

	// Queue file deleted on success.
	if _, err := os.Stat(qp); !os.IsNotExist(err) {
		t.Errorf("queue file %s not deleted (err=%v)", qp, err)
	}

	// Chain advanced: new agent's number = old + 1, prev path = doc.
	if newRec.HandoffNumber != oldRec.HandoffNumber+1 {
		t.Errorf("handoff_number not incremented: got %d want %d",
			newRec.HandoffNumber, oldRec.HandoffNumber+1)
	}
}

// -- crash recovery: replacement already spawned ----------------------------

func TestResume_AlreadySpawnedSkipsSpawnRunsTail(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)
	oldRec := spawnSeedAgent(t)
	req, qp, docPath := writeSkillQueue(t, oldRec)

	// Simulate "previous Resume crashed AFTER spawn but BEFORE archive":
	// spawn the replacement here, leave queue + doc + old record intact.
	newRec, err := spawn.Spawn(spawn.Options{
		OldRecord:      oldRec,
		NewDocPath:     docPath,
		Cwd:            oldRec.Cwd,
		Command:        oldRec.Command,
		PreAllocatedID: req.NewAgentID,
	})
	if err != nil {
		t.Fatalf("pre-spawn: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(newRec.TmuxSession) })

	out := &bytes.Buffer{}
	if err := Resume(req, qp, 0, out, out); err != nil {
		t.Fatalf("Resume: %v\n%s", err, out.String())
	}

	// Old gone, new still alive.
	if tmux.HasSession(oldRec.TmuxSession) {
		t.Error("old session still alive")
	}
	if !tmux.HasSession(newRec.TmuxSession) {
		t.Error("new session killed during resume — should have been left alone")
	}
}

// TestResume_CrashRecoveryDeliversPromptToReplacement verifies the
// codex iter-1 P1 fix: when a previous Resume crashed AFTER spawn but
// BEFORE retire, the recovery path's retireOldAgent call delivers the
// resume prompt to the surviving replacement. Pre-fix, send-keys lived
// inside spawn.Spawn — the recovery path skipped spawn → never sent
// the prompt → replacement sat idle forever.
func TestResume_CrashRecoveryDeliversPromptToReplacement(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)
	oldRec := spawnSeedAgent(t)
	req, qp, docPath := writeSkillQueue(t, oldRec)

	// Pre-spawn the replacement with a shell that echoes whatever
	// Resume types into it. Mirrors "crashed AFTER spawn but BEFORE
	// archive": record + session exist, queue + doc + old still
	// intact, but no prompt was delivered. We expect Resume's
	// retireOldAgent call to type ResumePrompt(docPath) into this
	// session, which the shell echoes back as `GOT:<prompt>`.
	//
	// DisableAutoResume defaults to false (zero value) → auto-resume
	// fires, prompt is typed.
	newRec := agent.New(req.NewAgentID)
	newRec.TaskID = oldRec.TaskID
	newRec.Project = oldRec.Project
	newRec.Cwd = oldRec.Cwd
	newRec.Command = []string{"sh", "-c", "read line; echo GOT:$line; sleep 30"}
	newRec.TmuxSession = req.NewSession
	if err := tmux.Spawn(newRec.TmuxSession, newRec.Cwd, newRec.Command,
		[]string{"FLEET_AGENT_ID=" + newRec.ID}); err != nil {
		t.Fatalf("pre-spawn replacement: %v", err)
	}
	if err := newRec.Write(); err != nil {
		_ = tmux.Kill(newRec.TmuxSession)
		t.Fatalf("write replacement record: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(newRec.TmuxSession) })

	out := &bytes.Buffer{}
	if err := Resume(req, qp, 0, out, out); err != nil {
		t.Fatalf("Resume: %v\n%s", err, out.String())
	}

	// Replacement still alive; prompt was delivered (shell echoed it).
	// tmux capture-pane wraps long lines at terminal width — strip
	// newlines before substring matching so the path-bearing prompt
	// matches whether or not it crossed a column boundary.
	want := "GOT:Read your handoff doc at " + docPath
	deadline := time.Now().Add(2 * time.Second)
	var lastOut []byte
	for time.Now().Before(deadline) {
		captured, err := tmux.CapturePane(newRec.TmuxSession)
		if err == nil {
			lastOut = captured
			joined := strings.ReplaceAll(string(captured), "\n", "")
			if strings.Contains(joined, want) {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("crash-recovery did not deliver resume prompt; want substring %q in:\n%s",
		want, string(lastOut))
}

// TestResume_DisableAutoResumeSkipsPrompt verifies the codex iter-7
// P2 fix (updated for iter-12): per-handoff override on the queue
// file ANDed with the record's baseline. Either pathway disables
// auto-resume → no prompt typed.
func TestResume_DisableAutoResumeSkipsPrompt(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)

	// Seed the outgoing agent with DisableAutoResume=true (baseline
	// policy from a hypothetical original `fleet dispatch
	// --no-auto-resume`).
	oldRec := spawnSeedAgent(t)
	oldRec.DisableAutoResume = true
	if err := oldRec.Write(); err != nil {
		t.Fatalf("re-write old with DisableAutoResume=true: %v", err)
	}
	req, qp, _ := writeSkillQueue(t, oldRec)

	// Pre-spawn replacement that would echo any typed prompt.
	newRec := agent.New(req.NewAgentID)
	newRec.TaskID = oldRec.TaskID
	newRec.Project = oldRec.Project
	newRec.Cwd = oldRec.Cwd
	newRec.Command = []string{"sh", "-c", "read line; echo GOT:$line; sleep 30"}
	newRec.TmuxSession = req.NewSession
	newRec.DisableAutoResume = oldRec.DisableAutoResume // baseline inherits
	if err := tmux.Spawn(newRec.TmuxSession, newRec.Cwd, newRec.Command,
		[]string{"FLEET_AGENT_ID=" + newRec.ID}); err != nil {
		t.Fatalf("pre-spawn replacement: %v", err)
	}
	if err := newRec.Write(); err != nil {
		_ = tmux.Kill(newRec.TmuxSession)
		t.Fatalf("write replacement record: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(newRec.TmuxSession) })

	out := &bytes.Buffer{}
	if err := Resume(req, qp, 0, out, out); err != nil {
		t.Fatalf("Resume: %v\n%s", err, out.String())
	}

	// Replacement still alive (no prompt sent → shell still blocked
	// on read). Pane must NOT contain GOT: marker.
	time.Sleep(300 * time.Millisecond)
	captured, err := tmux.CapturePane(newRec.TmuxSession)
	if err != nil {
		t.Fatalf("capture-pane: %v", err)
	}
	joined := strings.ReplaceAll(string(captured), "\n", "")
	if strings.Contains(joined, "GOT:") {
		t.Errorf("auto-resume fired despite DisableAutoResume=true; pane:\n%s",
			string(captured))
	}
}

// -- stale queue cleanup ----------------------------------------------------

func TestResume_StaleQueueClearedWhenHandoffAlreadyComplete(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)
	oldRec := spawnSeedAgent(t)
	req, qp, docPath := writeSkillQueue(t, oldRec)

	// Spawn replacement.
	newRec, err := spawn.Spawn(spawn.Options{
		OldRecord:      oldRec,
		NewDocPath:     docPath,
		Cwd:            oldRec.Cwd,
		Command:        oldRec.Command,
		PreAllocatedID: req.NewAgentID,
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(newRec.TmuxSession) })

	// Archive old + kill its session — simulating a successful prior
	// handoff that crashed BEFORE deleting the queue file.
	if err := oldRec.Archive(); err != nil {
		t.Fatal(err)
	}
	_ = tmux.Kill(oldRec.TmuxSession)

	out := &bytes.Buffer{}
	if err := Resume(req, qp, 0, out, out); err != nil {
		t.Fatalf("Resume: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "already handed off") {
		t.Errorf("expected stale-queue success message, got:\n%s", out.String())
	}
	if _, err := os.Stat(qp); !os.IsNotExist(err) {
		t.Errorf("stale queue file %s not deleted", qp)
	}
}

// -- orphan refusal ---------------------------------------------------------

func TestResume_RefusesWhenNewSessionAliveButRecordMissing(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)
	oldRec := spawnSeedAgent(t)
	req, qp, _ := writeSkillQueue(t, oldRec)

	// Plant an orphan tmux session at req.NewSession but no record. This
	// simulates the case where a prior Resume crashed AFTER spawn but
	// somehow the record file was hand-deleted.
	if err := tmux.Spawn(req.NewSession, oldRec.Cwd,
		[]string{"sleep", "60"}, nil); err != nil {
		t.Fatalf("plant orphan session: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(req.NewSession) })

	out := &bytes.Buffer{}
	err := Resume(req, qp, 0, out, out)
	if err == nil {
		t.Fatal("expected refusal but got nil")
	}
	if !strings.Contains(err.Error(), "refusing duplicate spawn") {
		t.Errorf("error did not mention duplicate-spawn refusal: %v", err)
	}
	// Queue file preserved for retry.
	if _, err := os.Stat(qp); err != nil {
		t.Errorf("queue file deleted on orphan-refusal path: %v", err)
	}
}

// -- stale-replacement-with-dead-session → cleanup + fresh spawn ------------

func TestResume_StaleReplacementWithDeadSessionRespawnsFresh(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)
	oldRec := spawnSeedAgent(t)
	req, qp, docPath := writeSkillQueue(t, oldRec)

	// Plant a stale replacement record with a dead session: a prior
	// spawn finished writing the record but the agent process crashed
	// at startup. Resume should clean it up and spawn fresh.
	staleNew, err := spawn.Spawn(spawn.Options{
		OldRecord:      oldRec,
		NewDocPath:     docPath,
		Cwd:            oldRec.Cwd,
		Command:        []string{"true"}, // exits instantly
		PreAllocatedID: req.NewAgentID,
	})
	if err != nil {
		t.Fatalf("plant stale: %v", err)
	}
	// Wait for the `true` process to exit and tmux to reap the session.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !tmux.HasSession(staleNew.TmuxSession) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if tmux.HasSession(staleNew.TmuxSession) {
		t.Skip("stale session refused to die; environment quirk")
	}

	out := &bytes.Buffer{}
	if err := Resume(req, qp, 0, out, out); err != nil {
		t.Fatalf("Resume: %v\n%s", err, out.String())
	}
	// Resume must have spawned a fresh replacement at the SAME pre-allocated
	// ID. The record now points at a fresh `sleep 60` (oldRec's command),
	// not the dead `true` we planted.
	newRec, err := agent.Load(req.NewAgentID)
	if err != nil {
		t.Fatalf("load new: %v", err)
	}
	if !tmux.HasSession(newRec.TmuxSession) {
		t.Error("fresh replacement session not alive after Resume")
	}
	t.Cleanup(func() { _ = tmux.Kill(newRec.TmuxSession) })
}

// -- legacy record refusal --------------------------------------------------

func TestResume_RefusesLegacyRecordMissingCwd(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)

	// Seed an agent missing Cwd / Command — mimicking a record dispatched
	// before those fields existed. fleet-guard's auto-handoff path can
	// only spawn from the OUTGOING record's stored values; legacy records
	// must surface a clear error rather than silently default to the
	// drainer's own cwd.
	rec := agent.New(agent.NewID())
	rec.TaskID = "legacy"
	rec.Project = "old"
	rec.SpawnedAt = time.Now().UTC()
	rec.TmuxSession = tmux.SessionName(rec.ID)
	// Simulate a live tmux session for the legacy agent.
	if err := tmux.Spawn(rec.TmuxSession, t.TempDir(),
		[]string{"sleep", "60"}, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tmux.Kill(rec.TmuxSession) })
	if err := rec.Write(); err != nil {
		t.Fatal(err)
	}

	req, qp, _ := writeSkillQueue(t, rec)
	out := &bytes.Buffer{}
	err := Resume(req, qp, 0, out, out)
	if err == nil || !strings.Contains(err.Error(), "legacy record") {
		t.Errorf("expected legacy-record refusal, got: %v", err)
	}
}

// -- coord-spawn marker transfer (bug coord-marker-transfer-on-46a3) --------
//
// When an agent that IS the project's coord (marker == oldRec.ID)
// handoffs through the auto-drain path, the marker must re-point
// at the replacement so the TUI's [a] keystroke finds the live
// coord instead of spawning a duplicate. Workers and other non-coord
// agents leave the marker untouched.

// TestAutoHandoff_TransfersCoordMarkerWhenOldWasCoord asserts the
// happy path: pre-seed marker = oldRec.ID, run Resume, marker should
// now equal newRec.ID.
func TestAutoHandoff_TransfersCoordMarkerWhenOldWasCoord(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)
	oldRec := spawnSeedAgent(t)

	// Initialize the project dir so WriteCoordSpawnMarker can land
	// (matches the TUI's pre-dispatch flow).
	if _, err := state.EnsureProjectInitialized(oldRec.Project); err != nil {
		t.Fatalf("EnsureProjectInitialized: %v", err)
	}
	if err := state.WriteCoordSpawnMarker(oldRec.Project, oldRec.ID); err != nil {
		t.Fatalf("seed marker: %v", err)
	}

	req, qp, _ := writeSkillQueue(t, oldRec)
	out := &bytes.Buffer{}
	if err := Resume(req, qp, 0, out, out); err != nil {
		t.Fatalf("Resume: %v\n%s", err, out.String())
	}

	newRec, err := agent.Load(req.NewAgentID)
	if err != nil {
		t.Fatalf("load new record: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(newRec.TmuxSession) })

	got := state.ReadCoordSpawnMarker(oldRec.Project)
	if got != newRec.ID {
		t.Errorf("coord marker not transferred: got %q want %q (oldRec.ID=%q)",
			got, newRec.ID, oldRec.ID)
	}
}

// TestAutoHandoff_NoMarkerUpdate_WhenOldWasNotCoord asserts the
// guard: marker points at some OTHER agent ID (not oldRec.ID); after
// a handoff of oldRec, the marker is unchanged.
func TestAutoHandoff_NoMarkerUpdate_WhenOldWasNotCoord(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)
	oldRec := spawnSeedAgent(t)

	const unrelatedID = "abcdef12"
	if _, err := state.EnsureProjectInitialized(oldRec.Project); err != nil {
		t.Fatalf("EnsureProjectInitialized: %v", err)
	}
	if err := state.WriteCoordSpawnMarker(oldRec.Project, unrelatedID); err != nil {
		t.Fatalf("seed marker: %v", err)
	}

	req, qp, _ := writeSkillQueue(t, oldRec)
	out := &bytes.Buffer{}
	if err := Resume(req, qp, 0, out, out); err != nil {
		t.Fatalf("Resume: %v\n%s", err, out.String())
	}

	newRec, err := agent.Load(req.NewAgentID)
	if err != nil {
		t.Fatalf("load new record: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(newRec.TmuxSession) })

	got := state.ReadCoordSpawnMarker(oldRec.Project)
	if got != unrelatedID {
		t.Errorf("unrelated coord marker mutated: got %q want %q", got, unrelatedID)
	}
}

// TestAutoHandoff_NoMarkerUpdate_WhenNoMarkerExists asserts the
// absent-marker branch: no marker file before Resume → no marker file
// after Resume (no error, no spurious creation). Most non-coord agents
// run with no marker at all.
func TestAutoHandoff_NoMarkerUpdate_WhenNoMarkerExists(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)
	oldRec := spawnSeedAgent(t)

	// Intentionally do NOT seed a marker. Project dir may or may not
	// exist; ReadCoordSpawnMarker returns "" either way.
	req, qp, _ := writeSkillQueue(t, oldRec)
	out := &bytes.Buffer{}
	if err := Resume(req, qp, 0, out, out); err != nil {
		t.Fatalf("Resume: %v\n%s", err, out.String())
	}

	newRec, err := agent.Load(req.NewAgentID)
	if err != nil {
		t.Fatalf("load new record: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(newRec.TmuxSession) })

	if got := state.ReadCoordSpawnMarker(oldRec.Project); got != "" {
		t.Errorf("marker created from nothing: got %q want empty", got)
	}
}

// -- codex iter-1 P1: SessionAlive tristate gate at outer cleanup sites -----
//
// HasSession used to gate the cleanup branches in Resume's case-3
// (record-exists path) and spawnAndRetire's post-spawn check. HasSession
// returns false for BOTH "session genuinely dead" AND "probe failed"
// (transport error / lost server). On a flaky probe window:
//
//   1. Resume's outer gate saw HasSession==false and entered the
//      cleanup branch on a live replacement.
//   2. DropReplacementRecord re-probed via SessionAlive (tristate),
//      which on the retry sometimes succeeded — disagreement with the
//      first probe. If Kill then ran, a healthy replacement got killed
//      and the handoff respawned needlessly (or failed mid-way).
//
// The fix at each outer gate switches to SessionAlive tristate
// (via the package-level tmuxSessionAliveFn seam). Cleanup runs only
// on definitive (alive=false, err=nil). Probe errors surface as
// explicit errors with the record preserved for operator inspection.

// TestResume_Case3_ProbeErrorPreservesRecordAndDropsNoSession is the
// regression test for the case-3 outer gate (replacement record exists,
// session probe returns an error). Pre-fix HasSession-based gate would
// enter cleanup, possibly killing a healthy replacement. Post-fix
// SessionAlive tristate must surface the probe error without invoking
// DropReplacementRecord (no kill, record file intact, queue preserved).
func TestResume_Case3_ProbeErrorPreservesRecordAndDropsNoSession(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)
	oldRec := spawnSeedAgent(t)
	req, qp, docPath := writeSkillQueue(t, oldRec)

	// Plant a replacement record (so Resume reaches case-3) but DO NOT
	// spawn a tmux session for it — only the record is on disk. The fake
	// SessionAlive below will say "probe failed", and the outer gate
	// must NOT call DropReplacementRecord (which would invoke Kill).
	newRec := agent.New(req.NewAgentID)
	newRec.TaskID = oldRec.TaskID
	newRec.Project = oldRec.Project
	newRec.Cwd = oldRec.Cwd
	newRec.Command = oldRec.Command
	newRec.TmuxSession = req.NewSession
	newRec.SpawnedAt = time.Now().UTC()
	if err := newRec.Write(); err != nil {
		t.Fatalf("plant new rec: %v", err)
	}
	newRecPath, err := state.AgentPath(newRec.ID)
	if err != nil {
		t.Fatalf("state.AgentPath: %v", err)
	}

	probeErr := errors.New("simulated tmux probe failure (transport error)")
	origAlive := tmuxSessionAliveFn
	origKill := tmuxKillFn
	var killCalls int
	tmuxSessionAliveFn = func(s string) (bool, error) {
		if s == newRec.TmuxSession {
			return false, probeErr
		}
		return origAlive(s)
	}
	tmuxKillFn = func(s string) error {
		killCalls++
		return origKill(s)
	}
	t.Cleanup(func() { tmuxSessionAliveFn = origAlive; tmuxKillFn = origKill })

	out := &bytes.Buffer{}
	resumeErr := Resume(req, qp, 0, out, out)
	_ = docPath

	if resumeErr == nil {
		t.Fatalf("Resume: expected probe-failure error; got nil")
	}
	if !strings.Contains(resumeErr.Error(), "probe replacement") &&
		!strings.Contains(resumeErr.Error(), "probe") {
		t.Errorf("Resume error should mention probe failure; got: %v", resumeErr)
	}
	if !strings.Contains(resumeErr.Error(), probeErr.Error()) {
		t.Errorf("Resume error should wrap the underlying probe error %q; got: %v", probeErr, resumeErr)
	}
	// Critical regression bar: record preserved AND no Kill invoked.
	if _, statErr := os.Stat(newRecPath); statErr != nil {
		t.Errorf("replacement record removed on probe error (this is the leak shape): %v", statErr)
	}
	if killCalls != 0 {
		t.Errorf("tmuxKillFn called %d times on probe error; expected 0 (outer gate must not enter cleanup branch on ambiguous probe)",
			killCalls)
	}
	// Queue file must also stay so the operator can retry once the
	// probe is reliable again.
	if _, err := os.Stat(qp); err != nil {
		t.Errorf("queue file %s removed on probe error; want preserved: %v", qp, err)
	}
}

// TestResume_Case3_DefinitiveDeadStillEntersCleanup pins the happy-path
// cleanup branch: when the probe is unambiguous (alive=false, err=nil),
// the outer gate must call DropReplacementRecord and fall through to a
// fresh spawn. Without this test, a regression that erroneously routed
// definitive-dead through the new probe-error branch would silently
// strand the operator (no fresh spawn).
//
// Uses the existing fakeable seam so we don't need to race a real tmux
// session into the "definitely-dead but record-exists" shape.
func TestResume_Case3_DefinitiveDeadStillEntersCleanup(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)
	oldRec := spawnSeedAgent(t)
	req, qp, _ := writeSkillQueue(t, oldRec)

	// Plant a replacement record but no tmux session (so cleanup +
	// fresh-spawn is the correct branch).
	newRec := agent.New(req.NewAgentID)
	newRec.TaskID = oldRec.TaskID
	newRec.Project = oldRec.Project
	newRec.Cwd = oldRec.Cwd
	newRec.Command = oldRec.Command
	newRec.TmuxSession = req.NewSession
	newRec.SpawnedAt = time.Now().UTC()
	if err := newRec.Write(); err != nil {
		t.Fatalf("plant new rec: %v", err)
	}

	origAlive := tmuxSessionAliveFn
	// First probe (outer gate): definitively dead. Subsequent probes
	// fall through to the real tmux.SessionAlive (used by the spawned-
	// fresh path's own post-spawn liveness check).
	var probedSessions []string
	tmuxSessionAliveFn = func(s string) (bool, error) {
		probedSessions = append(probedSessions, s)
		if s == req.NewSession && len(probedSessions) == 1 {
			return false, nil // definitively dead
		}
		return origAlive(s)
	}
	t.Cleanup(func() { tmuxSessionAliveFn = origAlive })

	out := &bytes.Buffer{}
	if err := Resume(req, qp, 0, out, out); err != nil {
		t.Fatalf("Resume: %v\n%s", err, out.String())
	}

	// Fresh replacement must be spawned + alive at the same PreAllocatedID.
	freshRec, err := agent.Load(req.NewAgentID)
	if err != nil {
		t.Fatalf("load fresh record: %v", err)
	}
	if !tmux.HasSession(freshRec.TmuxSession) {
		t.Errorf("fresh replacement session %s not alive after cleanup+respawn", freshRec.TmuxSession)
	}
	t.Cleanup(func() { _ = tmux.Kill(freshRec.TmuxSession) })
}
func TestResume_StaleCoordMarker_RolledBackBeforeRespawn(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)
	oldRec := spawnSeedAgent(t)
	req, qp, _ := writeSkillQueue(t, oldRec)

	// Initialize project + seed marker pointing at the would-be
	// replacement (req.NewAgentID), simulating a prior swap that
	// committed the marker and then failed.
	if _, err := state.EnsureProjectInitialized(oldRec.Project); err != nil {
		t.Fatalf("EnsureProjectInitialized: %v", err)
	}
	if err := state.WriteCoordSpawnMarker(oldRec.Project, req.NewAgentID); err != nil {
		t.Fatalf("seed stale marker: %v", err)
	}

	// Plant a stale replacement record but no tmux session (so the
	// outer gate's SessionAlive returns alive=false, triggering
	// cleanup + fall-through to spawnAndRetire).
	staleNew := agent.New(req.NewAgentID)
	staleNew.TaskID = oldRec.TaskID
	staleNew.Project = oldRec.Project
	staleNew.Cwd = oldRec.Cwd
	staleNew.Command = oldRec.Command
	staleNew.TmuxSession = req.NewSession
	staleNew.SpawnedAt = time.Now().UTC()
	if err := staleNew.Write(); err != nil {
		t.Fatalf("plant stale new rec: %v", err)
	}

	// Kill OLD's session so iter-14's "old still alive" refusal gate
	// passes (this test exercises the rollback + respawn path; the
	// refusal path is covered separately by
	// TestResume_StaleCoordMarker_OldStillAlive_RefusesRespawn).
	_ = tmux.Kill(oldRec.TmuxSession)

	// Force the outer-gate probe to report "definitively dead" on the
	// first call so cleanup fires deterministically.
	origAlive := tmuxSessionAliveFn
	var probedSessions []string
	tmuxSessionAliveFn = func(s string) (bool, error) {
		probedSessions = append(probedSessions, s)
		if s == req.NewSession && len(probedSessions) == 1 {
			return false, nil
		}
		return origAlive(s)
	}
	t.Cleanup(func() { tmuxSessionAliveFn = origAlive })

	out := &bytes.Buffer{}
	if err := Resume(req, qp, 0, out, out); err != nil {
		t.Fatalf("Resume: %v\n%s", err, out.String())
	}

	// Fresh replacement spawned at the same pre-allocated ID and alive.
	freshRec, err := agent.Load(req.NewAgentID)
	if err != nil {
		t.Fatalf("load fresh record: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(freshRec.TmuxSession) })
	if !tmux.HasSession(freshRec.TmuxSession) {
		t.Errorf("fresh replacement session %s not alive after respawn", freshRec.TmuxSession)
	}

	// The load-bearing assertion: after Resume completes, the coord
	// marker must point at the FRESH replacement (freshRec.ID, same as
	// the pre-allocated req.NewAgentID), proving:
	//
	//   1. The rollback fired (marker stepped back from staleNew →
	//      oldRec.ID after the drop), AND
	//   2. spawnAndRetire's isCoordSwap detection saw marker ==
	//      oldRec.ID and routed through AtomicCoordSwap, which then
	//      committed marker → freshRec.ID.
	//
	// Pre-fix: marker would still equal req.NewAgentID (the
	// pre-allocated ID), but only because the stale marker was
	// pointing at the deleted record's same ID. That coincidence
	// makes the pre-fix bug invisible in this test. So we also assert
	// the freshRec is alive — which proves a NEW spawn happened with
	// the marker-rooted swap path completing successfully.
	got := state.ReadCoordSpawnMarker(oldRec.Project)
	if got != freshRec.ID {
		t.Errorf("coord marker not at fresh replacement: got %q want %q",
			got, freshRec.ID)
	}

	// Stronger pin: assert oldRec was archived (proves AtomicCoordSwap
	// ran end-to-end, not the inline retire path). The inline retire
	// path also archives, but the marker rebase to freshRec.ID is the
	// AtomicCoordSwap commit-point signature — without isCoordSwap
	// firing on the retry, the marker would NOT be moved to freshRec.ID
	// at all (the inline path doesn't touch the marker).
	if _, lerr := agent.Load(oldRec.ID); !errors.Is(lerr, state.ErrNotFound) {
		t.Errorf("oldRec %s should be archived (not findable in live agents): err=%v",
			oldRec.ID, lerr)
	}
}

// TestResume_StaleCoordMarker_NonCoord_NoRollback pins the guard
// branch: when the marker exists but points at some UNRELATED agent
// (not oldRec.ID and not newRec.ID), the cleanup must NOT rewrite it.
// Touching an unrelated marker would corrupt another project's coord
// pointer. Idempotency of RollbackCoordMarkerIfPointingAt covers this.
func TestResume_StaleCoordMarker_NonCoord_NoRollback(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)
	oldRec := spawnSeedAgent(t)
	req, qp, _ := writeSkillQueue(t, oldRec)

	const unrelatedID = "deadbeef"
	if _, err := state.EnsureProjectInitialized(oldRec.Project); err != nil {
		t.Fatalf("EnsureProjectInitialized: %v", err)
	}
	if err := state.WriteCoordSpawnMarker(oldRec.Project, unrelatedID); err != nil {
		t.Fatalf("seed unrelated marker: %v", err)
	}

	// Plant a stale replacement record with no tmux session.
	staleNew := agent.New(req.NewAgentID)
	staleNew.TaskID = oldRec.TaskID
	staleNew.Project = oldRec.Project
	staleNew.Cwd = oldRec.Cwd
	staleNew.Command = oldRec.Command
	staleNew.TmuxSession = req.NewSession
	staleNew.SpawnedAt = time.Now().UTC()
	if err := staleNew.Write(); err != nil {
		t.Fatalf("plant stale new rec: %v", err)
	}

	origAlive := tmuxSessionAliveFn
	var probedSessions []string
	tmuxSessionAliveFn = func(s string) (bool, error) {
		probedSessions = append(probedSessions, s)
		if s == req.NewSession && len(probedSessions) == 1 {
			return false, nil
		}
		return origAlive(s)
	}
	t.Cleanup(func() { tmuxSessionAliveFn = origAlive })

	out := &bytes.Buffer{}
	if err := Resume(req, qp, 0, out, out); err != nil {
		t.Fatalf("Resume: %v\n%s", err, out.String())
	}

	// Marker must STILL be at the unrelated ID — the rollback only
	// fires on marker == staleNew.ID (the deleted replacement's ID).
	if got := state.ReadCoordSpawnMarker(oldRec.Project); got != unrelatedID {
		t.Errorf("unrelated marker corrupted: got %q want %q",
			got, unrelatedID)
	}

	// Fresh replacement must still be alive (the cleanup + respawn
	// still works regardless of marker rollback path).
	freshRec, err := agent.Load(req.NewAgentID)
	if err != nil {
		t.Fatalf("load fresh record: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(freshRec.TmuxSession) })
	if !tmux.HasSession(freshRec.TmuxSession) {
		t.Errorf("fresh replacement session %s not alive", freshRec.TmuxSession)
	}
}

// TestResume_StaleCoordMarker_OldStillAlive_RefusesRespawn pins codex
// iter-14 [P1] + iter-15 [P1] narrowing: AtomicCoordSwap preserves the
// queue on ErrOrphanSurvived / ErrOldKillProbeAmbiguous — NEW exited
// (lost the coordinator.lock race or crashed) but OLD is still the
// live coord. Post-commit means marker == newRec.ID. On the next drain
// pass we land in Resume's case-3 outer gate with newRec session dead.
// Auto-respawning here would stack a second replacement on top of the
// live OLD coord, recreating the duplicate-coord loop the preserved
// queue is supposed to avoid.
//
// iter-15 narrowing: the refusal only fires on marker == newRec.ID
// (post-commit). Marker == oldRec.ID (pre-commit) means OLD is the
// only legitimate coord and respawning is safe — see
// TestResume_PreCommitStaleNewRec_OldAlive_StillRespawns below.
func TestResume_StaleCoordMarker_OldStillAlive_RefusesRespawn(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)
	oldRec := spawnSeedAgent(t) // OLD session alive for the duration of the test.
	req, qp, _ := writeSkillQueue(t, oldRec)

	// Marker at newRec.ID (committed by a prior swap that returned
	// ErrOrphanSurvived / ErrOldKillProbeAmbiguous before retire).
	if _, err := state.EnsureProjectInitialized(oldRec.Project); err != nil {
		t.Fatalf("EnsureProjectInitialized: %v", err)
	}
	if err := state.WriteCoordSpawnMarker(oldRec.Project, req.NewAgentID); err != nil {
		t.Fatalf("seed marker: %v", err)
	}

	// Plant a stale replacement record with a dead session (the helper
	// session string we never spawned).
	staleNew := agent.New(req.NewAgentID)
	staleNew.TaskID = oldRec.TaskID
	staleNew.Project = oldRec.Project
	staleNew.Cwd = oldRec.Cwd
	staleNew.Command = oldRec.Command
	staleNew.TmuxSession = req.NewSession
	staleNew.SpawnedAt = time.Now().UTC()
	if err := staleNew.Write(); err != nil {
		t.Fatalf("plant stale new rec: %v", err)
	}

	// Outer-gate probe says NEW is definitively dead. OLD probes fall
	// through to real tmux.SessionAlive — OLD is alive (spawnSeedAgent).
	origAlive := tmuxSessionAliveFn
	tmuxSessionAliveFn = func(s string) (bool, error) {
		if s == req.NewSession {
			return false, nil
		}
		return origAlive(s)
	}
	t.Cleanup(func() { tmuxSessionAliveFn = origAlive })

	out := &bytes.Buffer{}
	err := Resume(req, qp, 0, out, out)
	if err == nil {
		t.Fatalf("Resume must refuse when OLD coord still alive; got nil error\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "still alive") || !strings.Contains(err.Error(), "duplicate coord") {
		t.Errorf("expected refusal mentioning 'still alive' + 'duplicate coord'; got: %v", err)
	}

	// Stale newRec record MUST NOT have been deleted (operator triage).
	if _, lerr := agent.Load(staleNew.ID); lerr != nil {
		t.Errorf("stale newRec record should be preserved for operator inspection; got load err=%v", lerr)
	}

	// Marker untouched.
	if got := state.ReadCoordSpawnMarker(oldRec.Project); got != req.NewAgentID {
		t.Errorf("marker mutated on refusal: got %q want %q", got, req.NewAgentID)
	}

	// Queue file must persist so the next pass (or operator's
	// triage-then-retry) can resume.
	if _, qerr := os.Stat(qp); qerr != nil {
		t.Errorf("queue file should be preserved on refusal: stat err=%v", qerr)
	}

	// OLD record + session untouched.
	if _, lerr := agent.Load(oldRec.ID); lerr != nil {
		t.Errorf("oldRec should still be live: %v", lerr)
	}
	if !tmux.HasSession(oldRec.TmuxSession) {
		t.Errorf("oldRec session %s should still be alive", oldRec.TmuxSession)
	}
}

// TestResume_PreCommitStaleNewRec_OldAlive_StillRespawns pins codex
// iter-15 [P1] narrowing: the iter-14 refusal gate must fire ONLY on
// post-commit state (marker == newRec.ID). A pre-commit crash —
// previous handoff spawned newRec but failed BEFORE the AtomicCoordSwap
// step 4 marker write — leaves marker at oldRec.ID. OLD is still the
// only legitimate coord. Auto-respawning is safe and required (no
// duplicate-coord risk: no NEW2 because OLD never lost coordinator.lock).
//
// Pre-iter-15: my iter-14 predicate `marker == oldRec.ID || marker ==
// newRec.ID` would refuse this case, blocking automatic recovery and
// requiring operator intervention. iter-15 narrows the predicate to
// `marker == newRec.ID` only.
func TestResume_PreCommitStaleNewRec_OldAlive_StillRespawns(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)
	oldRec := spawnSeedAgent(t) // OLD session stays alive.
	req, qp, _ := writeSkillQueue(t, oldRec)

	// Pre-commit state: marker at oldRec.ID (eager write never landed
	// or AtomicCoordSwap step 4 never reached).
	if _, err := state.EnsureProjectInitialized(oldRec.Project); err != nil {
		t.Fatalf("EnsureProjectInitialized: %v", err)
	}
	if err := state.WriteCoordSpawnMarker(oldRec.Project, oldRec.ID); err != nil {
		t.Fatalf("seed marker: %v", err)
	}

	// Plant a stale newRec with a dead session (previous handoff
	// crashed after spawn but before marker commit).
	staleNew := agent.New(req.NewAgentID)
	staleNew.TaskID = oldRec.TaskID
	staleNew.Project = oldRec.Project
	staleNew.Cwd = oldRec.Cwd
	staleNew.Command = oldRec.Command
	staleNew.TmuxSession = req.NewSession
	staleNew.SpawnedAt = time.Now().UTC()
	if err := staleNew.Write(); err != nil {
		t.Fatalf("plant stale new rec: %v", err)
	}

	// Outer-gate probe: NEW definitively dead.
	origAlive := tmuxSessionAliveFn
	var probedSessions []string
	tmuxSessionAliveFn = func(s string) (bool, error) {
		probedSessions = append(probedSessions, s)
		if s == req.NewSession && len(probedSessions) == 1 {
			return false, nil
		}
		return origAlive(s)
	}
	t.Cleanup(func() { tmuxSessionAliveFn = origAlive })

	// Resume must NOT refuse — pre-commit state is safe to auto-recover.
	out := &bytes.Buffer{}
	if err := Resume(req, qp, 0, out, out); err != nil {
		t.Fatalf("Resume on pre-commit stale newRec must succeed; got: %v\n%s", err, out.String())
	}

	// Fresh replacement spawned + alive.
	freshRec, err := agent.Load(req.NewAgentID)
	if err != nil {
		t.Fatalf("load fresh: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(freshRec.TmuxSession) })
	if !tmux.HasSession(freshRec.TmuxSession) {
		t.Errorf("fresh replacement session %s not alive", freshRec.TmuxSession)
	}

	// Marker committed to fresh replacement (AtomicCoordSwap completed
	// the swap end-to-end on the retry).
	if got := state.ReadCoordSpawnMarker(oldRec.Project); got != freshRec.ID {
		t.Errorf("marker not at fresh replacement: got %q want %q", got, freshRec.ID)
	}

	// OLD archived (proves the swap committed normally, not refused).
	if _, lerr := agent.Load(oldRec.ID); !errors.Is(lerr, state.ErrNotFound) {
		t.Errorf("oldRec should be archived after successful swap: err=%v", lerr)
	}
}
