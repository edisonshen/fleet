package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
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

// codex PR3 iter-4 [P1]: with FLEET_LEASE_FAILOVER on, a WORKER (non-coord)
// handoff must NOT be routed through the coord lease stand-down — it carries a
// Project but is not the project coord, so the coord lease says nothing about
// it. It must drain via the LEGACY path (spawn + retire), not get stranded.
func TestDrain_FailoverOn_WorkerHandoffUsesLegacyPath(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)
	t.Setenv("FLEET_LEASE_FAILOVER", "1")
	// seedAgent's TaskID is a worker task (not "coord-<project>").
	oldRec := seedAgent(t)
	qp, req := writeSkillQueueFile(t, oldRec)

	out := &bytes.Buffer{}
	if err := runDrain(out, out, 0, 0); err != nil {
		t.Fatalf("runDrain: %v\n%s", err, out.String())
	}
	// The worker handoff drained normally (legacy): old gone, new alive, queue
	// deleted — NOT a "coord live; nothing to drain" stand-down.
	if strings.Contains(out.String(), "coord live") {
		t.Errorf("worker handoff was wrongly routed through coord lease stand-down:\n%s", out.String())
	}
	if _, err := os.Stat(qp); !os.IsNotExist(err) {
		t.Errorf("queue file %s not deleted (worker handoff stranded): %v", qp, err)
	}
	newRec, err := agent.Load(req.NewAgentID)
	if err != nil {
		t.Fatalf("load new: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(newRec.TmuxSession) })
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

// stubDrainOne swaps the drainOneFn seam for the duration of the test
// (mirrors the drainProcStartFn/drainRunNow seam pattern).
func stubDrainOne(t *testing.T, fn func(queue.SpawnFresh, string, int, int, io.Writer, io.Writer) error) {
	t.Helper()
	prev := drainOneFn
	drainOneFn = fn
	t.Cleanup(func() { drainOneFn = prev })
}

// bogusQueueFile plants a syntactically valid spawn-fresh queue file; the
// stubbed drainOneFn never dereferences it beyond the parsed request.
func bogusQueueFile(t *testing.T) queue.SpawnFresh {
	t.Helper()
	req := queue.SpawnFresh{
		OldAgentID: "slowcord",
		HandoffDoc: "/nonexistent",
		Project:    "projects-fleet",
		TaskID:     "coord",
		NewAgentID: "newcoord",
		NewSession: "fleet-newcoord",
	}
	if _, err := queue.WriteSpawnFresh(req); err != nil {
		t.Fatal(err)
	}
	return req
}

// DESIGN-handoff-lifecycle-hardening bug A, test 3: the default resume budget
// is 120s ("wait at least 2 mins"), and the --resume-timeout-ms flag default
// follows the constant.
func TestDrain_DefaultResumeTimeoutIs120Seconds(t *testing.T) {
	if defaultResumeTimeoutMillis != 120000 {
		t.Fatalf("defaultResumeTimeoutMillis = %d, want 120000", defaultResumeTimeoutMillis)
	}
	f := newDrainCmd().Flags().Lookup("resume-timeout-ms")
	if f == nil {
		t.Fatal("--resume-timeout-ms flag missing")
	}
	if f.DefValue != "120000" {
		t.Fatalf("--resume-timeout-ms default = %s, want 120000", f.DefValue)
	}
}

// Bug A, test 4 (flag wiring): --resume-timeout-ms still overrides the
// default. The timeout-honoring behavior itself is covered at the coldResume
// seam (TestDrainLease_ColdResume_HonorsResumeTimeout).
func TestDrain_ResumeTimeoutFlagOverride(t *testing.T) {
	cmd := newDrainCmd()
	if err := cmd.ParseFlags([]string{"--resume-timeout-ms", "500"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	v, err := cmd.Flags().GetInt("resume-timeout-ms")
	if err != nil {
		t.Fatalf("GetInt: %v", err)
	}
	if v != 500 {
		t.Fatalf("--resume-timeout-ms parsed = %d, want 500", v)
	}
}

// Bug A, test 2 (the false-alarm regression): a resume that merely exceeds the
// budget is BACKGROUNDED, not failed — runDrain counts it processed, exits 0,
// and the top-line says the handoff is completing in the background. The
// string "every pending handoff failed" must not be reachable from a timeout
// alone. (Observed live 2026-06-10: exit 1 + "0 processed, 1 failed" while the
// handoff in fact completed.)
func TestDrain_BackgroundedResumeCountsProcessed(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)
	req := bogusQueueFile(t)
	stubDrainOne(t, func(r queue.SpawnFresh, _ string, _, timeoutMillis int, _, stderr io.Writer) error {
		// Mirror coldResume's timeout branch: stderr diagnostic + wrapped sentinel.
		_, _ = fmt.Fprintf(stderr,
			"fleet drain: resume for %s exceeded the %dms budget; returning (the handoff completes in the background or a later drain retries)\n",
			r.Project, timeoutMillis)
		return fmt.Errorf("fleet drain: resume for %s exceeded the %dms resume-timeout budget: %w",
			r.Project, timeoutMillis, ErrResumeBackgrounded)
	})

	out := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	if err := runDrain(out, stderr, 0, defaultResumeTimeoutMillis); err != nil {
		t.Fatalf("backgrounded resume must exit 0, got %v\nstdout=%s\nstderr=%s",
			err, out.String(), stderr.String())
	}
	if !strings.Contains(out.String(), "1 processed, 0 failed") {
		t.Errorf("expected '1 processed, 0 failed' summary, got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "background") {
		t.Errorf("expected a 'completing in the background' top-line mentioning %s, got:\n%s",
			req.OldAgentID, out.String())
	}
	combined := out.String() + stderr.String()
	if strings.Contains(combined, "every pending handoff failed") {
		t.Errorf("timeout alone must never report 'every pending handoff failed':\n%s", combined)
	}
}

// Bug A durability (codex iter-1 [P1]): a backgrounded resume must leave the
// queue file ON DISK — only a completed Resume deletes it (queue.Delete in
// retireOldAgent/cleanUpStaleQueue). The file is the durable retry signal:
// `fleet drain` is a short-lived CLI, so process exit can kill the
// backgrounded goroutine mid-Resume; the surviving queue file makes the NEXT
// drain re-run Resume, which finishes the handoff or reconciles an
// already-completed one. Without this retention, exit 0 + "completing in the
// background" could silently strand a half-done handoff.
func TestDrain_BackgroundedResumeKeepsQueueFile(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)
	bogusQueueFile(t)
	stubDrainOne(t, func(r queue.SpawnFresh, _ string, _, timeoutMillis int, _, _ io.Writer) error {
		return fmt.Errorf("fleet drain: resume for %s exceeded the %dms resume-timeout budget: %w",
			r.Project, timeoutMillis, ErrResumeBackgrounded)
	})

	out := &bytes.Buffer{}
	if err := runDrain(out, out, 0, defaultResumeTimeoutMillis); err != nil {
		t.Fatalf("backgrounded resume must exit 0, got %v\n%s", err, out.String())
	}
	paths, err := queue.ListPending()
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	if len(paths) != 1 {
		t.Fatalf("queue files after backgrounded drain = %d, want 1 — the pending file is the durable retry signal", len(paths))
	}
}

// Bug A, test 5 (regression guard): a GENUINE Resume error (not a timeout) is
// still counted failed — the new sentinel must not mask real failures.
func TestDrain_GenuineResumeErrorStillFails(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)
	bogusQueueFile(t)
	stubDrainOne(t, func(queue.SpawnFresh, string, int, int, io.Writer, io.Writer) error {
		return errors.New("spawn replacement: boom")
	})

	out := &bytes.Buffer{}
	err := runDrain(out, out, 0, defaultResumeTimeoutMillis)
	if err == nil {
		t.Fatalf("genuine resume error must surface as a drain error; got nil\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "every pending handoff failed") {
		t.Errorf("expected 'every pending handoff failed', got %v", err)
	}
	if !strings.Contains(out.String(), "0 processed, 1 failed") {
		t.Errorf("expected '0 processed, 1 failed' summary, got:\n%s", out.String())
	}
}
