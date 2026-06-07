package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/queue"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tmux"
)

// seedAgentForDrain plants a live tmux session + agent record without
// going through `fleet dispatch`'s "exactly one agent after seed"
// invariant — drain tests need MULTIPLE concurrent agents, so we drop
// down to spawn primitives directly.
func seedAgentForDrain(t *testing.T) *agent.Record {
	t.Helper()
	rec := agent.New(agent.NewID())
	rec.TaskID = "drain-test"
	rec.Project = "rainier"
	rec.SpawnedAt = time.Now().UTC()
	rec.LastActivityTS = rec.SpawnedAt
	rec.Cwd = t.TempDir()
	rec.Command = []string{"sleep", "60"}
	rec.TmuxSession = tmux.SessionName(rec.ID)
	if err := tmux.Spawn(rec.TmuxSession, rec.Cwd, rec.Command,
		[]string{"FLEET_AGENT_ID=" + rec.ID}); err != nil {
		t.Fatalf("tmux.Spawn: %v", err)
	}
	if err := rec.Write(); err != nil {
		_ = tmux.Kill(rec.TmuxSession)
		t.Fatalf("agent.Write: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(rec.TmuxSession) })
	return rec
}

// writeSkillQueueFile mimics what fleet-guard's handoff.py writes:
// queue file with NewAgentID + NewSession pre-allocated and a doc on
// disk. Returns the queue path and the request payload.
func writeSkillQueueFile(t *testing.T, oldRec *agent.Record) (queuePath string, req queue.SpawnFresh) {
	t.Helper()
	now := time.Now().UTC()

	dp, err := state.HandoffPath(oldRec.ID, now)
	if err != nil {
		t.Fatalf("HandoffPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(dp), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dp, []byte("---\nagent_id: \""+oldRec.ID+"\"\n---\n"), 0o644); err != nil {
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
	queuePath, err = queue.WriteSpawnFresh(req)
	if err != nil {
		t.Fatalf("WriteSpawnFresh: %v", err)
	}
	return queuePath, req
}

func TestDrain_NoQueueFilesIsNotAnError(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)

	out := &bytes.Buffer{}
	if err := runDrain(out, out, 0, 0); err != nil {
		t.Fatalf("runDrain: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "no pending handoffs") {
		t.Errorf("expected 'no pending handoffs' message, got:\n%s", out.String())
	}
}

func TestDrain_ProcessesSkillWrittenQueue(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)
	oldRec := seedAgent(t)
	qp, req := writeSkillQueueFile(t, oldRec)

	out := &bytes.Buffer{}
	if err := runDrain(out, out, 0, 0); err != nil {
		t.Fatalf("runDrain: %v\n%s", err, out.String())
	}

	// Old gone, new alive, queue deleted.
	if tmux.HasSession(oldRec.TmuxSession) {
		t.Error("old session still alive after drain")
	}
	if _, err := os.Stat(qp); !os.IsNotExist(err) {
		t.Errorf("queue file %s not deleted: %v", qp, err)
	}
	newRec, err := agent.Load(req.NewAgentID)
	if err != nil {
		t.Fatalf("load new: %v", err)
	}
	if !tmux.HasSession(newRec.TmuxSession) {
		t.Error("replacement session not alive")
	}
	t.Cleanup(func() { _ = tmux.Kill(newRec.TmuxSession) })

	if !strings.Contains(out.String(), "1 processed, 0 failed") {
		t.Errorf("expected '1 processed' summary, got:\n%s", out.String())
	}
}

func TestDrain_ProcessesMultipleQueueFilesIndependently(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)

	// Two independent agents, two queue files. Drain should retire both
	// and report 2 processed.
	oldA := seedAgentForDrain(t)
	oldB := seedAgentForDrain(t)
	_, reqA := writeSkillQueueFile(t, oldA)
	_, reqB := writeSkillQueueFile(t, oldB)

	out := &bytes.Buffer{}
	if err := runDrain(out, out, 0, 0); err != nil {
		t.Fatalf("runDrain: %v\n%s", err, out.String())
	}
	for _, id := range []string{reqA.NewAgentID, reqB.NewAgentID} {
		newRec, err := agent.Load(id)
		if err != nil {
			t.Errorf("expected new record %s: %v", id, err)
			continue
		}
		t.Cleanup(func() { _ = tmux.Kill(newRec.TmuxSession) })
	}
	if !strings.Contains(out.String(), "2 processed, 0 failed") {
		t.Errorf("expected '2 processed' summary, got:\n%s", out.String())
	}
}

func TestDrain_FailureIsolatedToOneFile(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)

	// Agent A is healthy → drain should retire it.
	// Agent B has a queue file pointing at a missing record AND a
	// missing replacement → drain should fail on B but still process A.
	oldA := seedAgent(t)
	_, reqA := writeSkillQueueFile(t, oldA)

	// Plant a queue file for a non-existent agent. Resume's first step
	// (Load oldRec) returns ErrNotFound, then cleanUpStaleQueue requires
	// the new replacement record — also missing → returns an error.
	bogus := queue.SpawnFresh{
		OldAgentID: "ghostbas",
		HandoffDoc: "/nonexistent",
		Project:    "ghost",
		TaskID:     "ghost",
		NewAgentID: "doesnoex",
		NewSession: "fleet-doesnoex",
	}
	bogusPath, err := queue.WriteSpawnFresh(bogus)
	if err != nil {
		t.Fatal(err)
	}

	out := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	if err := runDrain(out, stderr, 0, 0); err != nil {
		t.Fatalf("runDrain: %v\nstdout=%s\nstderr=%s", err, out.String(), stderr.String())
	}

	// Agent A drained successfully.
	if newRec, err := agent.Load(reqA.NewAgentID); err == nil {
		t.Cleanup(func() { _ = tmux.Kill(newRec.TmuxSession) })
	} else {
		t.Errorf("expected agent A to drain: %v", err)
	}

	// Bogus queue file left in place for retry.
	if _, err := os.Stat(bogusPath); err != nil {
		t.Errorf("expected bogus queue file to be preserved, got: %v", err)
	}
	if !strings.Contains(out.String(), "1 processed, 1 failed") {
		t.Errorf("expected '1 processed, 1 failed' summary, got:\n%s", out.String())
	}
	if !strings.Contains(stderr.String(), "ghostbas") {
		t.Errorf("expected stderr to mention failing agent ghostbas, got:\n%s", stderr.String())
	}
}

func TestDrain_AllFailuresReturnsError(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)

	// One queue file that will fail. With no successful processes, drain
	// must surface an error so callers (cron, CI smoke) notice.
	bogus := queue.SpawnFresh{
		OldAgentID: "ghostbas",
		HandoffDoc: "/nonexistent",
		Project:    "ghost",
		TaskID:     "ghost",
		NewAgentID: "doesnoex",
		NewSession: "fleet-doesnoex",
	}
	if _, err := queue.WriteSpawnFresh(bogus); err != nil {
		t.Fatal(err)
	}

	out := &bytes.Buffer{}
	if err := runDrain(out, out, 0, 0); err == nil {
		t.Errorf("expected error when every file failed; got nil")
	}
}
