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
	"github.com/edisonshen/fleet/internal/tasks"
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
	requireFakeTmux(t)
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
	requireFakeTmux(t)
	setupFleetHome(t)
	oldRec := seedAgent(t)
	// Row 7: a live (non-terminal) backing task routes through the existing
	// spawn+resume path. Without a resolvable row the drain-nonforcing
	// classifier would HOLD it pending, so seed the task live.
	seedDrainTask(t, oldRec.Project, oldRec.TaskID, tasks.StatusInProgress)
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

// codex PR3 iter-4 [P1]: a WORKER (non-coord) handoff must NOT be routed
// through the coord lease stand-down — it carries a
// Project but is not the project coord, so the coord lease says nothing about
// it. It must drain via the LEGACY path (spawn + retire), not get stranded.
func TestDrain_FailoverOn_WorkerHandoffUsesLegacyPath(t *testing.T) {
	requireFakeTmux(t)
	setupFleetHome(t)
	oldRec := seedAgent(t)
	seedDrainTask(t, oldRec.Project, oldRec.TaskID, tasks.StatusInProgress)
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
	requireFakeTmux(t)
	setupFleetHome(t)

	// Two independent agents, two queue files. Drain should retire both
	// and report 2 processed.
	oldA := seedAgentForDrain(t)
	oldB := seedAgentForDrain(t)
	// Both seedAgentForDrain records share TaskID "drain-test"; one live row
	// resolves both (classifier routes live → resume).
	seedDrainTask(t, oldA.Project, oldA.TaskID, tasks.StatusInProgress)
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
	requireFakeTmux(t)
	setupFleetHome(t)

	// Agent A is healthy → drain should retire it.
	// Agent B has a queue file pointing at a missing record AND a
	// missing replacement → drain should fail on B but still process A.
	oldA := seedAgent(t)
	seedDrainTask(t, oldA.Project, oldA.TaskID, tasks.StatusInProgress)
	_, reqA := writeSkillQueueFile(t, oldA)

	// Plant a queue file for a non-existent agent. Resume's first step
	// (Load oldRec) returns ErrNotFound, then cleanUpStaleQueue requires
	// the new replacement record — also missing → returns an error. Seed a
	// LIVE backing task so the classifier routes it to Resume (where it
	// genuinely fails) rather than holding it pending.
	seedDrainTask(t, "ghost", "ghost", tasks.StatusInProgress)
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
	requireFakeTmux(t)
	setupFleetHome(t)

	// One queue file that will fail. With no successful processes, drain
	// must surface an error so callers (cron, CI smoke) notice. Seed a LIVE
	// backing task so the classifier routes it to Resume (genuine failure),
	// not the pending-hold path.
	seedDrainTask(t, "ghost", "ghost", tasks.StatusInProgress)
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
	return bogusQueueFileFor(t, "slowcord")
}

// bogusQueueFileFor is bogusQueueFile with a caller-chosen OldAgentID —
// queue files are keyed by OldAgentID, so tests that need multiple
// pending files must use distinct IDs.
func bogusQueueFileFor(t *testing.T, oldAgentID string) queue.SpawnFresh {
	t.Helper()
	req := queue.SpawnFresh{
		OldAgentID: oldAgentID,
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
// budget is BACKGROUNDED, not failed — runDrain exits 0 and the top-line says
// the handoff is completing in the background. The string "every pending
// handoff failed" must not be reachable from a timeout alone. (Observed live
// 2026-06-10: exit 1 + "0 processed, 1 failed" while the handoff in fact
// completed.) It is also NOT processed (codex iter-1 [P1]): the CLI exit can
// kill the resume goroutine, so the summary must report the timed-out resume
// in its own `backgrounded` bucket — still completing, retried by a later
// drain — not claim completion.
func TestDrain_BackgroundedResumeCountsBackgrounded(t *testing.T) {
	requireFakeTmux(t)
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
	if !strings.Contains(out.String(),
		"0 processed, 1 backgrounded (still completing; a later drain retries), 0 failed") {
		t.Errorf("expected '0 processed, 1 backgrounded (still completing; a later drain retries), 0 failed' summary, got:\n%s",
			out.String())
	}
	// Exact marker substring: the TUI's drainBackgroundedMarker
	// (internal/tui/model.go) matches this on the drain child's stdout to
	// schedule its re-drain — this assert pins the producer side of that
	// cross-package contract.
	if !strings.Contains(out.String(), "completing in the background") {
		t.Errorf("expected a 'completing in the background' top-line mentioning %s, got:\n%s",
			req.OldAgentID, out.String())
	}
	combined := out.String() + stderr.String()
	if strings.Contains(combined, "every pending handoff failed") {
		t.Errorf("timeout alone must never report 'every pending handoff failed':\n%s", combined)
	}
}

// Bug A, codex iter-1 [P1] (mixed outcomes): one backgrounded resume + one
// genuine failure. The backgrounded handoff falsifies "every pending handoff
// failed", so the drain still exits 0 (failure isolation: the failed file is
// left in place + logged), and the summary reports each bucket truthfully.
func TestDrain_BackgroundedPlusFailedStillExitsZero(t *testing.T) {
	requireFakeTmux(t)
	setupFleetHome(t)
	slow := bogusQueueFileFor(t, "slowcord")
	bogusQueueFileFor(t, "deadcord")
	stubDrainOne(t, func(r queue.SpawnFresh, _ string, _, timeoutMillis int, _, _ io.Writer) error {
		if r.OldAgentID == slow.OldAgentID {
			return fmt.Errorf("fleet drain: resume for %s exceeded the %dms resume-timeout budget: %w",
				r.Project, timeoutMillis, ErrResumeBackgrounded)
		}
		return errors.New("spawn replacement: boom")
	})

	out := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	if err := runDrain(out, stderr, 0, defaultResumeTimeoutMillis); err != nil {
		t.Fatalf("backgrounded+failed must exit 0 (not every handoff failed), got %v\nstdout=%s\nstderr=%s",
			err, out.String(), stderr.String())
	}
	if !strings.Contains(out.String(),
		"0 processed, 1 backgrounded (still completing; a later drain retries), 1 failed") {
		t.Errorf("expected '0 processed, 1 backgrounded (still completing; a later drain retries), 1 failed' summary, got:\n%s",
			out.String())
	}
	if !strings.Contains(stderr.String(), "boom") {
		t.Errorf("expected the genuine failure on stderr, got:\n%s", stderr.String())
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
	requireFakeTmux(t)
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
	requireFakeTmux(t)
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

// ---------------------------------------------------------------------------
// drain-nonforcing (DESIGN-drain-nonforcing): classify by resolved backing-
// task status BEFORE the opt-out refusal — drop moot handoffs, hold live ones
// as pending, never force a failure.
// ---------------------------------------------------------------------------

// seedDrainTask writes a task row into tasks.md so the classifier can resolve
// req.TaskID. Only Status matters to the classifier; timestamps are filler.
func seedDrainTask(t *testing.T, project, slug string, status tasks.Status) {
	t.Helper()
	now := time.Now().UTC()
	listSeedTask(t, project, slug, status, tasks.PriorityP1, now, now, now, time.Time{})
}

// seedDrainArchiveTask seeds the row into tasks-archive.md instead — used to
// prove archive PRESENCE alone never drops (STATUS decides).
func seedDrainArchiveTask(t *testing.T, project, slug string, status tasks.Status) {
	t.Helper()
	now := time.Now().UTC()
	listSeedArchiveTask(t, project, slug, status, tasks.PriorityP1, now, now, now)
}

// writeSkillQueueFileOptOut is writeSkillQueueFile with a per-handoff
// DisableAutoResume override on the queue file (models fleet-guard writing an
// opt-out handoff). A nil override leaves the field unset (inherit oldRec).
func writeSkillQueueFileOptOut(t *testing.T, oldRec *agent.Record, disable *bool) (string, queue.SpawnFresh) {
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
	req := queue.SpawnFresh{
		SchemaVersion:     queue.SchemaVersion,
		OldAgentID:        oldRec.ID,
		HandoffDoc:        dp,
		Project:           oldRec.Project,
		TaskID:            oldRec.TaskID,
		NewAgentID:        newID,
		NewSession:        tmux.SessionName(newID),
		DisableAutoResume: disable,
	}
	qp, err := queue.WriteSpawnFresh(req)
	if err != nil {
		t.Fatalf("WriteSpawnFresh: %v", err)
	}
	return qp, req
}

// Rows 1,2,3,4,6a,12: classify an OPTED-OUT worker by resolved backing-task
// status. Terminal (done/abandoned, in tasks.md OR archive) → DROP; every
// other resolvable status is live → HOLD pending via the opt-out refusal;
// an unresolvable (absent) status → HOLD pending via errHeldPending. All exit
// 0 (drop + pending are not failures). seedAgentForDrain's session is always
// live, so the drop rows also cover the "agent live" manual-handoff hint.
func TestDrain_ClassifyOptedOutWorker(t *testing.T) {
	cases := []struct {
		name     string
		status   tasks.Status // "" = don't seed (unresolvable / absent)
		archive  bool         // seed in tasks-archive.md instead of tasks.md
		wantDrop bool         // true = dropped; false = held pending
	}{
		{"row1 done drops", tasks.StatusDone, false, true},
		{"row3 abandoned drops", tasks.StatusAbandoned, false, true},
		{"row12 in-review holds", tasks.StatusInReview, false, false},
		{"in-progress holds", tasks.StatusInProgress, false, false},
		{"row6a absent holds", "", false, false},
		{"row4 archived done drops", tasks.StatusDone, true, true},
		{"row4 archived in-progress holds", tasks.StatusInProgress, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requireFakeTmux(t)
			setupFleetHome(t)
			oldRec := seedAgentForDrain(t)
			oldRec.DisableAutoResume = true // opted-out worker (the wedged shape)
			if err := oldRec.Write(); err != nil {
				t.Fatalf("rewrite opt-out: %v", err)
			}
			if tc.status != "" {
				if tc.archive {
					seedDrainArchiveTask(t, oldRec.Project, oldRec.TaskID, tc.status)
				} else {
					seedDrainTask(t, oldRec.Project, oldRec.TaskID, tc.status)
				}
			}
			qp, _ := writeSkillQueueFile(t, oldRec)

			out := &bytes.Buffer{}
			if err := runDrain(out, out, 0, 0); err != nil {
				t.Fatalf("drop/hold must exit 0, got %v\n%s", err, out.String())
			}
			s := out.String()
			_, statErr := os.Stat(qp)
			if tc.wantDrop {
				if !os.IsNotExist(statErr) {
					t.Errorf("terminal task: queue file not dropped: %v\n%s", statErr, s)
				}
				if !strings.Contains(s, "dropped — backing task") {
					t.Errorf("missing drop diagnostic:\n%s", s)
				}
				if !strings.Contains(s, "agent live; run 'fleet handoff") {
					t.Errorf("drop diagnostic missing live/manual-handoff hint:\n%s", s)
				}
				if !strings.Contains(s, ", 1 dropped") {
					t.Errorf("summary missing dropped bucket:\n%s", s)
				}
			} else {
				if statErr != nil {
					t.Errorf("held handoff: queue file must be preserved: %v", statErr)
				}
				if !strings.Contains(s, "pending — worker handoff") {
					t.Errorf("missing pending diagnostic:\n%s", s)
				}
				if !strings.Contains(s, ", 1 pending") {
					t.Errorf("summary missing pending bucket:\n%s", s)
				}
				if _, err := agent.Load(oldRec.ID); err != nil {
					t.Errorf("held handoff must leave the old agent untouched: %v", err)
				}
			}
		})
	}
}

// Testing-specialist gap: every drop test above uses a session that's
// genuinely alive (seedAgentForDrain spawns a real fake-tmux session), so the
// "agent live" hint always fires — the alive=false / SessionAlive-error
// branches of emitDropDiagnostic's tristate (feedback: a probe error OMITS
// the hint but never aborts the drop) were never exercised. Kill the old
// session BEFORE draining to pin the omission.
func TestDrain_DropDiagnosticOmitsHintWhenSessionDead(t *testing.T) {
	requireFakeTmux(t)
	setupFleetHome(t)
	oldRec := seedAgentForDrain(t)
	oldRec.DisableAutoResume = true // opted-out worker (the wedged shape)
	if err := oldRec.Write(); err != nil {
		t.Fatalf("rewrite opt-out: %v", err)
	}
	seedDrainTask(t, oldRec.Project, oldRec.TaskID, tasks.StatusDone) // terminal → drop
	qp, _ := writeSkillQueueFile(t, oldRec)

	// Session dies before drain runs — no in-flight handoff to warn about.
	if err := tmux.Kill(oldRec.TmuxSession); err != nil {
		t.Fatalf("kill old session: %v", err)
	}

	out := &bytes.Buffer{}
	if err := runDrain(out, out, 0, 0); err != nil {
		t.Fatalf("drop must exit 0, got %v\n%s", err, out.String())
	}
	s := out.String()
	if _, statErr := os.Stat(qp); !os.IsNotExist(statErr) {
		t.Errorf("terminal task: queue file not dropped: %v\n%s", statErr, s)
	}
	if !strings.Contains(s, "dropped — backing task") {
		t.Errorf("missing drop diagnostic:\n%s", s)
	}
	if strings.Contains(s, "agent live; run 'fleet handoff") {
		t.Errorf("dead session must NOT get the live/manual-handoff hint:\n%s", s)
	}
}

// Rows 5a/5b/5c: per-handoff opt-out precedence. The queue override wins over
// the record baseline; either "true" pathway HOLDS the live handoff as pending
// (never a failure), while an override of "false" lets it PROCESS (we don't
// over-hold). Task is live so classification routes to Resume.
func TestDrain_OptOutOverridePrecedence(t *testing.T) {
	tt, ff := true, false
	cases := []struct {
		name         string
		recDisable   bool
		queueDisable *bool // nil = no override
		wantPending  bool  // true = held; false = processed
	}{
		{"row5a baseline opt-out holds", true, nil, true},
		{"row5b queue override true holds", false, &tt, true},
		{"row5c queue override false does not over-hold", true, &ff, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requireFakeTmux(t)
			setupFleetHome(t)
			oldRec := seedAgentForDrain(t)
			oldRec.DisableAutoResume = tc.recDisable
			if err := oldRec.Write(); err != nil {
				t.Fatalf("rewrite: %v", err)
			}
			seedDrainTask(t, oldRec.Project, oldRec.TaskID, tasks.StatusInProgress)
			qp, req := writeSkillQueueFileOptOut(t, oldRec, tc.queueDisable)

			out := &bytes.Buffer{}
			if err := runDrain(out, out, 0, 0); err != nil {
				t.Fatalf("must exit 0, got %v\n%s", err, out.String())
			}
			s := out.String()
			if tc.wantPending {
				if _, statErr := os.Stat(qp); statErr != nil {
					t.Errorf("held: queue file must be preserved: %v", statErr)
				}
				if !strings.Contains(s, ", 1 pending") {
					t.Errorf("expected pending bucket:\n%s", s)
				}
				if _, err := agent.Load(oldRec.ID); err != nil {
					t.Errorf("held handoff archived the old agent: %v", err)
				}
			} else {
				if _, statErr := os.Stat(qp); !os.IsNotExist(statErr) {
					t.Errorf("processed: queue file not deleted: %v", statErr)
				}
				if !strings.Contains(s, "1 processed") {
					t.Errorf("expected processed summary:\n%s", s)
				}
				newRec, err := agent.Load(req.NewAgentID)
				if err != nil {
					t.Fatalf("load replacement: %v", err)
				}
				t.Cleanup(func() { _ = tmux.Kill(newRec.TmuxSession) })
			}
		})
	}
}

// Row 6b: a corrupt/unreadable tasks.md must be SWALLOWED → unresolvable →
// HOLD pending, never surfaced as a failure (returning it would count failed++
// and resurrect the exit-1 banner). The worker is NOT opted out, so a wrong
// route to Resume would PROCESS — pending discriminates the swallow.
func TestDrain_CorruptTasksFileHoldsPending(t *testing.T) {
	requireFakeTmux(t)
	setupFleetHome(t)
	oldRec := seedAgentForDrain(t) // auto-resume worker
	dir, err := state.ProjectDir(oldRec.Project)
	if err != nil {
		t.Fatalf("ProjectDir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Unterminated / non-key:value frontmatter → tasks.Read parse error.
	if err := os.WriteFile(filepath.Join(dir, "tasks.md"),
		[]byte("---\ngarbage line no colon\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	qp, _ := writeSkillQueueFile(t, oldRec)

	out := &bytes.Buffer{}
	if err := runDrain(out, out, 0, 0); err != nil {
		t.Fatalf("corrupt tasks.md must HOLD pending (exit 0), not fail: %v\n%s", err, out.String())
	}
	s := out.String()
	if _, statErr := os.Stat(qp); statErr != nil {
		t.Errorf("held handoff: queue file must be preserved: %v", statErr)
	}
	if !strings.Contains(s, ", 1 pending") {
		t.Errorf("expected pending bucket (read error swallowed → unresolvable):\n%s", s)
	}
	if strings.Contains(s, "1 processed") {
		t.Errorf("corrupt-file classifier wrongly processed instead of holding:\n%s", s)
	}
}

// codex/adversarial-review iter-1 [P1]: a task_id that is simply ABSENT from
// both tasks.md and tasks-archive.md (never registered — the ordinary shape
// of an ad hoc / pre-task-tracking `fleet dispatch <id>`, and the DEFAULT
// configuration every OTHER baseline test in this file has to explicitly
// override via seedDrainTask) must NOT be held pending for an auto-resume
// (non-opted-out) worker. classifyBackingTask's "not found in either file"
// branch buckets taskLive (falls through to the existing lock+Resume route),
// NOT taskUnresolvable — only a genuine tasks.md/archive READ ERROR holds.
// Before this fix, "no row" and "read error" were conflated into the same
// taskUnresolvable bucket, silently converting a previously-working
// auto-resume into a handoff held pending forever with no forced failure to
// alert anyone (the exact silent-breakage class this feature exists to
// remove). This test pins the regression closed: PROCESSED, not pending.
func TestDrain_UntrackedTaskAutoResumesNotHeld(t *testing.T) {
	requireFakeTmux(t)
	setupFleetHome(t)
	oldRec := seedAgentForDrain(t) // auto-resume worker; TaskID="drain-test", NOT seeded anywhere
	qp, req := writeSkillQueueFile(t, oldRec)

	out := &bytes.Buffer{}
	if err := runDrain(out, out, 0, 0); err != nil {
		t.Fatalf("untracked task must still auto-resume (exit 0), got %v\n%s", err, out.String())
	}
	s := out.String()
	if !strings.Contains(s, "1 processed") {
		t.Errorf("untracked-task worker was held pending instead of processed:\n%s", s)
	}
	if strings.Contains(s, "pending") {
		t.Errorf("untracked task must not be held pending at all:\n%s", s)
	}
	if _, statErr := os.Stat(qp); !os.IsNotExist(statErr) {
		t.Errorf("queue file not deleted after processing: %v", statErr)
	}
	newRec, err := agent.Load(req.NewAgentID)
	if err != nil {
		t.Fatalf("load replacement: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(newRec.TmuxSession) })
}

// Companion to TestDrain_UntrackedTaskAutoResumesNotHeld: an OPTED-OUT worker
// whose task_id is untracked still falls through to the live/opt-out route
// (classifyBackingTask no longer short-circuits it), and handoffop.Resume's
// existing opt-out refusal (wrapped ErrHeldOptOut) buckets it pending exactly
// as it did before this fix — the outcome for the opted-out case is
// unchanged, only the code path that reaches it changed (classifier
// short-circuit → Resume's own opt-out gate).
func TestDrain_UntrackedTaskOptedOutHoldsPending(t *testing.T) {
	requireFakeTmux(t)
	setupFleetHome(t)
	oldRec := seedAgentForDrain(t)
	oldRec.DisableAutoResume = true
	if err := oldRec.Write(); err != nil {
		t.Fatalf("rewrite opt-out: %v", err)
	}
	qp, _ := writeSkillQueueFile(t, oldRec)

	out := &bytes.Buffer{}
	if err := runDrain(out, out, 0, 0); err != nil {
		t.Fatalf("held handoff must exit 0, got %v\n%s", err, out.String())
	}
	s := out.String()
	if !strings.Contains(s, ", 1 pending") {
		t.Errorf("untracked task + opted-out must still hold pending:\n%s", s)
	}
	if _, statErr := os.Stat(qp); statErr != nil {
		t.Errorf("held handoff: queue file must be preserved: %v", statErr)
	}
}

// 2nd-round adversarial review: an empty project is NOT the same as an
// untracked task_id. state.ProjectDir("") silently resolves to an internal
// "_default" fallback directory — a DIFFERENT project than whatever the
// caller actually meant — so falling through to taskLive here would risk
// resolving status against the wrong project's tasks.md. A missing project
// signals a malformed/legacy queue entry; classifyBackingTask must hold it
// pending rather than guess, even though a bare empty task_id (project
// present) correctly falls through per TestDrain_UntrackedTaskAutoResumesNotHeld.
func TestClassifyBackingTask_EmptyProjectHoldsRegardlessOfTaskID(t *testing.T) {
	setupFleetHome(t)
	seedDrainTask(t, "rainier", "drain-test", tasks.StatusDone) // would wrongly resolve if project=="" leaked to another project
	cases := []struct {
		name   string
		taskID string
	}{
		{"empty task_id", ""},
		{"non-empty task_id", "drain-test"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			class, status := classifyBackingTask("", tc.taskID)
			if class != taskUnresolvable {
				t.Errorf("project=\"\" must hold (taskUnresolvable), got class=%v status=%q", class, status)
			}
		})
	}
}

// Testing-specialist gap: the archive-read-error branch (classifyBackingTask's
// SECOND readArchiveTasks error path, reached only when the slug is absent
// from tasks.md) was previously unexercised — TestDrain_CorruptTasksFileHoldsPending
// only corrupts tasks.md, which returns unresolvable from the FIRST readTasks
// error without ever reaching the archive read. Corrupt tasks-archive.md
// specifically (with a clean, non-matching tasks.md) to pin the second error
// path: still HELD pending, never processed, never failed.
func TestDrain_CorruptArchiveFileHoldsPending(t *testing.T) {
	requireFakeTmux(t)
	setupFleetHome(t)
	oldRec := seedAgentForDrain(t) // auto-resume worker; TaskID="drain-test"
	dir, err := state.ProjectDir(oldRec.Project)
	if err != nil {
		t.Fatalf("ProjectDir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// tasks.md is present but doesn't contain oldRec.TaskID, so
	// classifyBackingTask falls through to the archive read.
	seedDrainTask(t, oldRec.Project, "some-other-task", tasks.StatusInProgress)
	// Unterminated / non-key:value frontmatter → tasks.Read parse error.
	if err := os.WriteFile(filepath.Join(dir, "tasks-archive.md"),
		[]byte("---\ngarbage line no colon\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	qp, _ := writeSkillQueueFile(t, oldRec)

	out := &bytes.Buffer{}
	if err := runDrain(out, out, 0, 0); err != nil {
		t.Fatalf("corrupt tasks-archive.md must HOLD pending (exit 0), not fail: %v\n%s", err, out.String())
	}
	s := out.String()
	if _, statErr := os.Stat(qp); statErr != nil {
		t.Errorf("held handoff: queue file must be preserved: %v", statErr)
	}
	if !strings.Contains(s, ", 1 pending") {
		t.Errorf("expected pending bucket (archive read error swallowed → unresolvable):\n%s", s)
	}
	if strings.Contains(s, "1 processed") {
		t.Errorf("corrupt-archive classifier wrongly processed instead of holding:\n%s", s)
	}
}

// codex review iter-2 [P2]: a terminal task status does NOT mean this queue
// file is a fresh, never-spawned request. Simulate "a previous Resume
// crashed AFTER spawning the replacement but BEFORE retiring the old
// agent" — exactly TestResume_AlreadySpawnedSkipsSpawnRunsTail's shape
// (internal/handoffop) — then mark the backing task done. The classifier
// must NOT lock-free-delete the queue file out from under that in-flight
// reconciliation: it must fall through to lock+Resume, which detects the
// existing (alive) replacement and retires the old agent, exactly as it
// would have before this feature existed. A naive unconditional drop would
// leave the old agent's record un-retired with nothing left to clean it up.
func TestDrain_TerminalWithReplacementAlreadySpawnedFallsThroughToResume(t *testing.T) {
	requireFakeTmux(t)
	setupFleetHome(t)
	oldRec := seedAgentForDrain(t)
	qp, req := writeSkillQueueFile(t, oldRec)

	// Simulate the crash window: replacement already spawned + alive,
	// queue/doc/old record still intact (mirrors
	// TestResume_AlreadySpawnedSkipsSpawnRunsTail in internal/handoffop).
	newRec := agent.New(req.NewAgentID)
	newRec.TaskID = oldRec.TaskID
	newRec.Project = oldRec.Project
	newRec.Cwd = oldRec.Cwd
	newRec.Command = oldRec.Command
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

	// NOW the backing task goes terminal — the classifier would otherwise
	// see this as a moot, droppable request.
	seedDrainTask(t, oldRec.Project, oldRec.TaskID, tasks.StatusDone)

	out := &bytes.Buffer{}
	if err := runDrain(out, out, 0, 0); err != nil {
		t.Fatalf("must exit 0, got %v\n%s", err, out.String())
	}
	s := out.String()
	if strings.Contains(s, "dropped") || strings.Contains(s, ", 1 dropped") {
		t.Errorf("terminal-but-already-spawned entry must NOT take the lock-free drop path:\n%s", s)
	}
	if !strings.Contains(s, "1 processed") {
		t.Errorf("expected the existing crash-recovery reconciliation to run (processed), got:\n%s", s)
	}
	if _, statErr := os.Stat(qp); !os.IsNotExist(statErr) {
		t.Errorf("queue file should be cleared by Resume's own retire step: %v", statErr)
	}
	// The old agent must actually be retired (archived), not left behind —
	// the whole point of falling through instead of dropping.
	if _, err := agent.Load(oldRec.ID); err == nil {
		t.Error("old agent record must be retired/archived, not left live")
	}
	if !tmux.HasSession(newRec.TmuxSession) {
		t.Error("replacement session was killed — should have been left alone")
	}
}

// Row 8b (REQUIRED): a coord queue entry called through drainOneLegacy DIRECTLY
// must hit the IsCoordSpawn skip and NEVER be status-classified/dropped — even
// with a terminal backing task. On CI leaseDrainEnabled() is always true, so
// the public drainOne path can't reach the legacy branch for a coord; this
// direct call pins the guard on every platform (dead-coord recovery preserved).
func TestDrain_CoordEntryNeverClassifiedLegacy(t *testing.T) {
	requireFakeTmux(t)
	setupFleetHome(t)
	const project = "rainier"
	// Seed the coord's synthetic task DONE — IF the classifier wrongly ran on a
	// coord entry it would DROP it. The skip must prevent that.
	seedDrainTask(t, project, "coord-"+project, tasks.StatusDone)
	oldRec := seedAgentForDrain(t)
	oldRec.TaskID = "coord-" + project
	oldRec.Project = project
	if err := oldRec.Write(); err != nil {
		t.Fatalf("rewrite coord: %v", err)
	}
	qp, req := writeSkillQueueFile(t, oldRec)

	out := &bytes.Buffer{}
	err := drainOneLegacy(req, qp, 0, out, out)
	if errors.Is(err, errDropped) {
		t.Fatalf("coord entry was status-classified + dropped — IsCoordSpawn skip failed")
	}
	if strings.Contains(out.String(), "dropped — backing task") {
		t.Errorf("coord entry emitted a drop diagnostic:\n%s", out.String())
	}
}

// Row 8a: on a lease build the public drainOne routes a coord entry to
// drainOneLeaseAware FIRST — never the status classifier — so a coord is never
// dropped even with a terminal backing task. We only assert not-dropped; the
// lease-aware outcome (fail/standby) is orthogonal.
func TestDrain_CoordEntryRoutesToLeaseAware(t *testing.T) {
	if !leaseDrainEnabled() {
		t.Skip("lease-aware drain is linux/darwin only")
	}
	requireFakeTmux(t)
	setupFleetHome(t)
	const project = "rainier"
	seedDrainTask(t, project, "coord-"+project, tasks.StatusDone) // would drop IF classified
	oldRec := seedAgentForDrain(t)
	oldRec.TaskID = "coord-" + project
	oldRec.Project = project
	if err := oldRec.Write(); err != nil {
		t.Fatalf("rewrite coord: %v", err)
	}
	writeSkillQueueFile(t, oldRec)

	out := &bytes.Buffer{}
	_ = runDrain(out, out, 0, 0) // outcome may be a lease-path failure; only assert not-dropped
	s := out.String()
	if strings.Contains(s, "dropped — backing task") {
		t.Errorf("coord entry was status-classified + dropped on the public path:\n%s", s)
	}
	if strings.Contains(s, ", 1 dropped") {
		t.Errorf("coord entry counted as dropped:\n%s", s)
	}
}

// Row 10a: concurrent drop. When a drainOne handler deletes a SIBLING queue
// file, the loop's next ReadSpawnFresh hits ErrNotFound — a benign concurrent-
// drain skip that must NOT count failed++. Both drains exit 0.
func TestDrain_ConcurrentDropBenignSkip(t *testing.T) {
	requireFakeTmux(t)
	setupFleetHome(t)
	mk := func(id string) (string, queue.SpawnFresh) {
		req := queue.SpawnFresh{
			SchemaVersion: queue.SchemaVersion,
			OldAgentID:    id,
			HandoffDoc:    "/nonexistent",
			Project:       "projects-fleet",
			TaskID:        "sib",
			NewAgentID:    "new" + id[:5],
			NewSession:    "fleet-new" + id[:5],
		}
		p, err := queue.WriteSpawnFresh(req)
		if err != nil {
			t.Fatal(err)
		}
		return p, req
	}
	pathA, reqA := mk("aaaaaaaa")
	pathB, _ := mk("bbbbbbbb")

	stubDrainOne(t, func(r queue.SpawnFresh, _ string, _, _ int, _, _ io.Writer) error {
		// Whichever file the loop reaches first deletes the sibling, so the
		// next ReadSpawnFresh vanishes deterministically.
		if r.OldAgentID == reqA.OldAgentID {
			_ = queue.Delete(pathB)
		} else {
			_ = queue.Delete(pathA)
		}
		return nil
	})

	out := &bytes.Buffer{}
	if err := runDrain(out, out, 0, 0); err != nil {
		t.Fatalf("concurrent benign skip must exit 0, got %v\n%s", err, out.String())
	}
	// Exactly one processed; the vanished sibling was benign-skipped, not failed.
	if !strings.Contains(out.String(), "1 processed, 0 failed") {
		t.Errorf("expected '1 processed, 0 failed' (sibling benign-skipped):\n%s", out.String())
	}
}

// Row 10b: the DROP path must be LOCK-FREE. Holding the per-agent lock that the
// live/spawn path would take must NOT stall a moot-entry drop. A regression that
// re-added state.LockAgent as drainOneLegacy's first statement would block this
// drop on the 5-minute agentLockTimeout — we bound the wait far below that.
func TestDrain_DropIsLockFree(t *testing.T) {
	requireFakeTmux(t)
	setupFleetHome(t)
	oldRec := seedAgentForDrain(t)
	seedDrainTask(t, oldRec.Project, oldRec.TaskID, tasks.StatusDone) // terminal → drop
	qp, _ := writeSkillQueueFile(t, oldRec)

	// Hold the per-agent lock BEFORE draining. release runs at cleanup so a
	// regressed (blocked) goroutine can eventually unwind.
	release, err := state.LockAgent(oldRec.ID)
	if err != nil {
		t.Fatalf("LockAgent: %v", err)
	}
	t.Cleanup(release)

	out := &bytes.Buffer{}
	done := make(chan error, 1)
	go func() { done <- runDrain(out, out, 0, 0) }()
	// Liveness bound, not a timing assertion: lock-free completes in ms; a
	// regression blocks ~5 min on the held lock. 30 s discriminates with a
	// huge margin and never false-fails a healthy run (which never waits).
	select {
	case derr := <-done:
		if derr != nil {
			t.Fatalf("drop must exit 0, got %v\n%s", derr, out.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatal("drop STALLED behind the held agent lock — regression: LockAgent moved before the drop")
	}
	if _, statErr := os.Stat(qp); !os.IsNotExist(statErr) {
		t.Errorf("moot handoff not dropped: %v\n%s", statErr, out.String())
	}
}

// Rows 11a-d: summary bucketing + exit code. drainOneFn is stubbed so runDrain's
// accounting is exercised in isolation. Drop + pending are neither processed nor
// failed; the exit-1 rule (processed==0 && backgrounded==0 && failed>0) is
// unchanged; the backgrounded parenthetical stays verbatim; dropped/pending
// append only when nonzero.
func TestDrain_SummaryBucketsAndExit(t *testing.T) {
	cases := []struct {
		name     string
		outcomes map[string]error // 8-char OldAgentID -> drainOneFn return
		wantErr  bool
		wantSubs []string
	}{
		{
			name: "11a dropped+pending+processed no failure exits 0",
			outcomes: map[string]error{
				"dropaaaa": errDropped,
				"pendbbbb": errHeldPending,
				"proccccc": nil,
			},
			wantErr:  false,
			wantSubs: []string{"1 processed", ", 1 dropped", ", 1 pending"},
		},
		{
			name: "11b only dropped+pending + 1 failure exits nonzero",
			outcomes: map[string]error{
				"dropaaaa": errDropped,
				"pendbbbb": errHeldPending,
				"faildddd": errors.New("spawn replacement: boom"),
			},
			wantErr:  true,
			wantSubs: []string{"0 processed", ", 1 failed", ", 1 dropped", ", 1 pending"},
		},
		{
			name: "11c processed + 1 isolated failure exits 0",
			outcomes: map[string]error{
				"proccccc": nil,
				"faildddd": errors.New("spawn replacement: boom"),
			},
			wantErr:  false,
			wantSubs: []string{"1 processed, 1 failed"},
		},
		{
			name: "11d backgrounded+dropped+pending keeps parenthetical verbatim",
			outcomes: map[string]error{
				"bgeeeeee": fmt.Errorf("slow: %w", ErrResumeBackgrounded),
				"dropaaaa": errDropped,
				"pendbbbb": errHeldPending,
			},
			wantErr: false,
			wantSubs: []string{
				"0 processed, 1 backgrounded (still completing; a later drain retries), 0 failed, 1 dropped, 1 pending",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requireFakeTmux(t)
			setupFleetHome(t)
			for id := range tc.outcomes {
				bogusQueueFileFor(t, id)
			}
			stubDrainOne(t, func(r queue.SpawnFresh, _ string, _, _ int, _, _ io.Writer) error {
				return tc.outcomes[r.OldAgentID]
			})
			out := &bytes.Buffer{}
			stderr := &bytes.Buffer{}
			err := runDrain(out, stderr, 0, 0)
			if tc.wantErr != (err != nil) {
				t.Fatalf("wantErr=%v got err=%v\nstdout=%s\nstderr=%s", tc.wantErr, err, out.String(), stderr.String())
			}
			for _, sub := range tc.wantSubs {
				if !strings.Contains(out.String(), sub) {
					t.Errorf("summary missing %q:\n%s", sub, out.String())
				}
			}
		})
	}
}
