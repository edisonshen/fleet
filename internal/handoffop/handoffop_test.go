package handoffop

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
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	t.Setenv("FLEET_TMUX_SOCKET", "/tmp/fleet-test-"+hex.EncodeToString(b[:])+".sock")
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
		OldAgentID: oldRec.ID,
		HandoffDoc: dp,
		Project:    oldRec.Project,
		TaskID:     oldRec.TaskID,
		NewAgentID: newID,
		NewSession: tmux.SessionName(newID),
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
