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
	"github.com/edisonshen/fleet/internal/coordlock"
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
	t.Setenv("FLEET_LEASE_FAILOVER", "0")
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

// seedCoordRepoMeta writes a non-git meta.json pinning repo_path for the
// project so a COORD drain handoff resolves via the resolver (PR3).
func seedCoordRepoMeta(t *testing.T, project string) string {
	t.Helper()
	home := os.Getenv("FLEET_HOME")
	repo := t.TempDir()
	pdir := filepath.Join(home, "projects", project)
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}
	meta := `{"schema":"v1","is_git":false,"repo_path":"` + repo + `","added_at":"2026-01-01T00:00:00Z"}`
	if err := os.WriteFile(filepath.Join(pdir, "meta.json"), []byte(meta), 0o644); err != nil {
		t.Fatalf("write meta.json: %v", err)
	}
	return repo
}

// spawnSeedCoord seeds an outgoing COORD (task_id = coord-<project>) in a
// WRONG tree, used to prove the drain handoff resolves via the resolver.
func spawnSeedCoord(t *testing.T, project, wrongCwd string) *agent.Record {
	t.Helper()
	now := time.Now().UTC()
	rec := agent.New(agent.NewID())
	rec.TaskID = "coord-" + project
	rec.Project = project
	rec.SpawnedAt = now
	rec.LastActivityTS = now
	rec.Cwd = wrongCwd
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

// writeCoordSkillQueue mirrors writeSkillQueue but for a coord oldRec
// (inherits oldRec.TaskID / Project instead of the hardcoded worker slug).
func writeCoordSkillQueue(t *testing.T, oldRec *agent.Record) (queue.SpawnFresh, string) {
	t.Helper()
	now := time.Now().UTC()
	dp, err := state.HandoffPath(oldRec.ID, now)
	if err != nil {
		t.Fatalf("HandoffPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(dp), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nagent_id: \"" + oldRec.ID + "\"\ntask_id: \"" + oldRec.TaskID +
		"\"\nproject: \"" + oldRec.Project + "\"\ncontext_pct_at_handoff: 55\n" +
		"previous_handoff: null\nhandoff_number: 1\ntimestamp: \"" +
		now.Format(time.RFC3339) + "\"\nhandoff_type: \"auto-yellow\"\n---\n\n## Completed\nx\n"
	if err := os.WriteFile(dp, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	newID := agent.NewID()
	req := queue.SpawnFresh{
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
	return req, qp
}

// E7 (drain handoff) — a COORD drain handoff resolves the replacement's
// Cwd via the shared resolver (meta repo), NOT the outgoing coord's Cwd.
func TestResume_CoordDrain_ResolvesRepoNotOldCwd(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)
	const project = "rainier"
	correctRepo := seedCoordRepoMeta(t, project)
	wrongCwd := t.TempDir()
	oldRec := spawnSeedCoord(t, project, wrongCwd)
	req, qp := writeCoordSkillQueue(t, oldRec)

	out := &bytes.Buffer{}
	if err := Resume(req, qp, 0, out, out); err != nil {
		t.Fatalf("Resume: %v\n%s", err, out.String())
	}
	newRec, err := agent.Load(req.NewAgentID)
	if err != nil {
		t.Fatalf("load new record: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(newRec.TmuxSession) })
	if newRec.Cwd != correctRepo {
		t.Errorf("coord drain bound wrong tree: newRec.Cwd = %q; want %q (resolver meta, not old Cwd %q)",
			newRec.Cwd, correctRepo, wrongCwd)
	}
}

// E7 (drain handoff, refuse) — a COORD drain handoff REFUSES when the
// resolver cannot bind; it does NOT fall back to oldRec.Cwd.
func TestResume_CoordDrain_RefusesWhenUnresolvable(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)
	const project = "ghostproj" // no meta, no worktrees → refuse
	wrongCwd := t.TempDir()
	oldRec := spawnSeedCoord(t, project, wrongCwd)
	req, qp := writeCoordSkillQueue(t, oldRec)

	out := &bytes.Buffer{}
	err := Resume(req, qp, 0, out, out)
	if err == nil {
		t.Fatal("coord drain must REFUSE when resolver cannot bind, got nil")
	}
	if !strings.Contains(err.Error(), "no usable checkout") {
		t.Errorf("refusal should surface the resolver hint; got %v", err)
	}
	// No replacement record should be loadable.
	if _, lerr := agent.Load(req.NewAgentID); lerr == nil {
		t.Cleanup(func() { _ = tmux.Kill(tmux.SessionName(req.NewAgentID)) })
		t.Error("coord drain spawned a replacement despite refusal")
	}
}

// E8 (drain handoff) — a WORKER drain handoff still inherits oldRec.Cwd.
func TestResume_WorkerDrain_StillInheritsOldCwd(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)
	// Seed meta for the project to prove the worker path ignores it.
	otherRepo := seedCoordRepoMeta(t, "rainier")
	oldRec := spawnSeedAgent(t) // task_id "auth-fix" (worker), project "rainier"
	workerCwd := oldRec.Cwd
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
	if newRec.Cwd != workerCwd {
		t.Errorf("worker drain Cwd: got %q want %q (inherited, NOT resolver)", newRec.Cwd, workerCwd)
	}
	if newRec.Cwd == otherRepo {
		t.Errorf("worker drain incorrectly used the resolver meta repo %q", otherRepo)
	}
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

// TestIsCoordHandoffForAgent_GatesOnCoordSpawnMarker is the codex
// review iter-7 [P1] regression: spawnAndRetire's --remote-control
// inject (rc.GateAttachFlag) MUST gate on whether the OLD agent is
// the project's coord, not just on the project-wide rc-enabled
// marker. Without this, a worker handoff resumed via fleet drain /
// crash recovery would silently inherit RC attach once the v0.12.1
// coord-spawn auto-write had marked the project — defeating the
// strict opt-in carve-out for workers / subagents.
//
// Mirrors cmd/fleet/handoff.go's predicate test (T6) for the auto-
// drain path. Tests the predicate directly because spawnAndRetire's
// inject site is a 1-line gated expression — the predicate's
// correctness IS the gate's correctness, and exercising it through
// spawn.Spawn would require either tmux or a deeper test double.
func TestIsCoordHandoffForAgent_GatesOnCoordSpawnMarker(t *testing.T) {
	const project = "test-drain-gate"
	const coordID = "ccccdddd"
	const workerID = "wwwweeee"

	cases := []struct {
		name      string
		project   string
		agentID   string
		setMarker string
		want      bool
	}{
		{"empty project rejects", "", coordID, "", false},
		{"unset marker rejects", project, coordID, "", false},
		{"marker points elsewhere rejects",
			project, workerID, coordID, false},
		{"marker matches agentID accepts",
			project, coordID, coordID, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setupFleetHome(t)
			if tc.setMarker != "" {
				if _, err := state.EnsureProjectInitialized(tc.project); err != nil {
					t.Fatalf("EnsureProjectInitialized: %v", err)
				}
				if err := state.WriteCoordSpawnMarker(tc.project, tc.setMarker); err != nil {
					t.Fatalf("WriteCoordSpawnMarker: %v", err)
				}
			}
			got := isCoordHandoffForAgent(tc.project, tc.agentID)
			if got != tc.want {
				t.Errorf("isCoordHandoffForAgent(%q, %q) with marker=%q: got %v, want %v",
					tc.project, tc.agentID, tc.setMarker, got, tc.want)
			}
		})
	}
}

// -- v0.12.2 P0: drain-path RC marker backfill ------------------------------
//
// The drain path (handoffop.spawnAndRetire) consumes queue files written
// by fleet-guard / crash recovery. PR #163 closed the FRESH-spawn marker
// hole at the dispatch layer; the deferred [P2] (2) is the drain path
// equivalent: when a pre-v0.12.1 coord that never wrote the rc-enabled
// marker triggers an auto-handoff, the drain path injects
// `--remote-control` only when rc.Enabled(project) holds. Without a
// marker, rc.GateAttachFlag returns the plain argv → coord-spawn
// replacement misses RC pairing.
//
// Fix: inside the existing `if isCoordHandoffForAgent(...)` block in
// spawnAndRetire (where the inject already runs), write the rc-enabled
// marker BEFORE the inject. Mirrors cmd/fleet/handoff.go's pattern.
//
// Tests below pin three behaviors:
//
//   - T-drain-coord-marker-write: a coord handoff (marker == oldRec.ID)
//     calls writeMarkerFn(oldRec.Project) exactly once before injecting.
//
//   - T-drain-worker-no-marker-write: a worker handoff
//     (isCoordHandoffForAgent=false) NEVER calls writeMarkerFn.
//     Preserves the v0.12 push-storm protection for non-coord agents.
//
//   - T-drain-marker-write-failure-non-fatal: when writeMarkerFn returns
//     an error inside the coord block, drain logs to stderr and
//     continues; the spawn proceeds (and the inject no-ops because the
//     marker is absent — graceful degrade).

// TestDrain_CoordHandoff_WritesMarkerBeforeInject pins
// T-drain-coord-marker-write. The drain path's coord branch must call
// the writeMarkerFn seam exactly once with the project, BEFORE the
// inject runs. We verify via the seam (call count + arg captured) so
// the test doesn't depend on rc.WriteMarker's on-disk side effect
// timing inside Resume.
func TestDrain_CoordHandoff_WritesMarkerBeforeInject(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)
	oldRec := spawnSeedAgent(t)

	// Marker = oldRec.ID makes isCoordHandoffForAgent fire.
	if _, err := state.EnsureProjectInitialized(oldRec.Project); err != nil {
		t.Fatalf("EnsureProjectInitialized: %v", err)
	}
	if err := state.WriteCoordSpawnMarker(oldRec.Project, oldRec.ID); err != nil {
		t.Fatalf("WriteCoordSpawnMarker: %v", err)
	}

	// Stub the seam to count + capture invocations.
	prev := writeMarkerFn
	var calls []string
	writeMarkerFn = func(project string) error {
		calls = append(calls, project)
		return nil
	}
	t.Cleanup(func() { writeMarkerFn = prev })

	req, qp, _ := writeSkillQueue(t, oldRec)
	out := &bytes.Buffer{}
	if err := Resume(req, qp, 0, out, out); err != nil {
		t.Fatalf("Resume: %v\n%s", err, out.String())
	}

	// Cleanup the replacement agent's tmux session.
	if newRec, lerr := agent.Load(req.NewAgentID); lerr == nil {
		t.Cleanup(func() { _ = tmux.Kill(newRec.TmuxSession) })
	}

	if len(calls) == 0 {
		t.Fatalf("expected drain coord handoff to call writeMarkerFn at least once; got 0 invocations")
	}
	// First call MUST be the coord project (this PR's fix). Later
	// calls inside the same drain (if any from other paths added in
	// future) are fine.
	if calls[0] != oldRec.Project {
		t.Errorf("first writeMarkerFn call: got project=%q want %q", calls[0], oldRec.Project)
	}
}

func TestDrain_LeaseFailoverCoordHandoffRefusesBeforeRetiringOld(t *testing.T) {
	requireTmux(t)
	t.Setenv("FLEET_LEASE_FAILOVER", "1")
	setupFleetHome(t)

	const project = "rainier"
	cwd := seedCoordRepoMeta(t, project)
	oldRec := spawnSeedCoord(t, project, cwd)
	oldRec.SupervisorPID = 4242
	if err := oldRec.Write(); err != nil {
		t.Fatalf("rewrite old coord supervisor pid: %v", err)
	}
	origOwner := handoffLeaseActiveOwnerPIDFn
	origLeader := handoffLeaseLeaderPresentFn
	handoffLeaseActiveOwnerPIDFn = func(p string) (int, bool) {
		if p != project {
			t.Fatalf("active owner checked for project %q, want %q", p, project)
		}
		return oldRec.SupervisorPID, true
	}
	handoffLeaseLeaderPresentFn = func(p string) bool {
		if p != project {
			t.Fatalf("leader checked for project %q, want %q", p, project)
		}
		return true
	}
	t.Cleanup(func() {
		handoffLeaseActiveOwnerPIDFn = origOwner
		handoffLeaseLeaderPresentFn = origLeader
	})
	if _, err := state.EnsureProjectInitialized(oldRec.Project); err != nil {
		t.Fatalf("EnsureProjectInitialized: %v", err)
	}
	if err := state.WriteCoordSpawnMarker(oldRec.Project, oldRec.ID); err != nil {
		t.Fatalf("WriteCoordSpawnMarker: %v", err)
	}

	req, qp := writeCoordSkillQueue(t, oldRec)
	out := &bytes.Buffer{}
	err := Resume(req, qp, 0, out, out)
	if err == nil {
		t.Fatal("expected failover coord drain handoff to refuse before retiring old")
	}
	if !strings.Contains(err.Error(), "cannot verify its child before retiring old coord") {
		t.Fatalf("refusal did not explain child verification gap: %v\n%s", err, out.String())
	}
	if _, lerr := agent.Load(oldRec.ID); lerr != nil {
		t.Fatalf("old coord record was not preserved: %v", lerr)
	}
	if got := state.ReadCoordSpawnMarker(oldRec.Project); got != oldRec.ID {
		t.Fatalf("coord marker = %q, want old id %q", got, oldRec.ID)
	}
	if _, statErr := os.Stat(qp); statErr != nil {
		t.Fatalf("queue file should be preserved for retry: %v", statErr)
	}
	if _, lerr := agent.Load(req.NewAgentID); !errors.Is(lerr, state.ErrNotFound) {
		t.Fatalf("replacement record should be dropped, load err=%v", lerr)
	}
}

func TestShouldRefuseLeaseWrappedCoordHandoffRetire_RequiresLiveOld(t *testing.T) {
	t.Setenv("FLEET_LEASE_FAILOVER", "1")
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

// TestDrain_WorkerHandoff_NoMarkerWrite pins
// T-drain-worker-no-marker-write. A worker handoff (marker absent or
// pointing elsewhere) MUST NOT touch the rc-enabled marker via the
// drain path. Preserves the v0.12 strict opt-in for workers /
// subagents (push-storm protection).
func TestDrain_WorkerHandoff_NoMarkerWrite(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)
	oldRec := spawnSeedAgent(t)

	// Marker absent on the project tree → isCoordHandoffForAgent=false.
	// (We deliberately do NOT WriteCoordSpawnMarker.)

	prev := writeMarkerFn
	var calls []string
	writeMarkerFn = func(project string) error {
		calls = append(calls, project)
		return nil
	}
	t.Cleanup(func() { writeMarkerFn = prev })

	req, qp, _ := writeSkillQueue(t, oldRec)
	out := &bytes.Buffer{}
	if err := Resume(req, qp, 0, out, out); err != nil {
		t.Fatalf("Resume: %v\n%s", err, out.String())
	}

	if newRec, lerr := agent.Load(req.NewAgentID); lerr == nil {
		t.Cleanup(func() { _ = tmux.Kill(newRec.TmuxSession) })
	}

	if len(calls) != 0 {
		t.Fatalf("expected drain worker handoff to skip writeMarkerFn; got %d calls (args=%v)",
			len(calls), calls)
	}
}

// TestDrain_CoordHandoff_MarkerWriteFailure_NonFatal pins
// T-drain-marker-write-failure-non-fatal. If writeMarkerFn returns an
// error inside the drain coord branch, the handoff continues (logs a
// warning to stderr) — graceful degrade matches dispatch.go's
// non-fatal contract pinned by TestCoordSpawn_MarkerWriteFailure_Degrades.
func TestDrain_CoordHandoff_MarkerWriteFailure_NonFatal(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)
	oldRec := spawnSeedAgent(t)

	if _, err := state.EnsureProjectInitialized(oldRec.Project); err != nil {
		t.Fatalf("EnsureProjectInitialized: %v", err)
	}
	if err := state.WriteCoordSpawnMarker(oldRec.Project, oldRec.ID); err != nil {
		t.Fatalf("WriteCoordSpawnMarker: %v", err)
	}

	prev := writeMarkerFn
	stubErr := errors.New("simulated drain marker write failure")
	writeMarkerFn = func(string) error { return stubErr }
	t.Cleanup(func() { writeMarkerFn = prev })

	req, qp, _ := writeSkillQueue(t, oldRec)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	err := Resume(req, qp, 0, stdout, stderr)
	if err != nil && strings.Contains(err.Error(), "simulated drain marker write failure") {
		t.Fatalf("marker-write failure MUST be non-fatal in drain coord branch; surfaced as: %v", err)
	}
	// Resume itself may still return nil (happy spawn after the
	// non-fatal warning); the contract is just "marker failure
	// isn't the cause of any error returned."
	if err != nil {
		// Non-fatal contract: any error returned must NOT mention
		// our stubbed marker failure. Other errors from spawn /
		// retire are unrelated and acceptable here.
		if strings.Contains(err.Error(), stubErr.Error()) {
			t.Fatalf("drain returned error attributable to marker failure: %v", err)
		}
	}

	if newRec, lerr := agent.Load(req.NewAgentID); lerr == nil {
		t.Cleanup(func() { _ = tmux.Kill(newRec.TmuxSession) })
	}

	// Warning MUST be written to stderr so the operator can recover
	// via `fleet rc up <project>`. Mirrors dispatch.go's warning shape.
	if !strings.Contains(stderr.String(), "rc.WriteMarker") {
		t.Errorf("expected stderr warning mentioning rc.WriteMarker; got: %q", stderr.String())
	}
}

// TestDrainAndDispatch_ShareSameLock is T7 (v3 Change 7): the drain
// path inside spawnAndRetire (the isCoordHandoffForAgent branch) MUST
// acquire the SAME project-level lock that cmd/fleet/dispatch.go uses,
// so a TUI `[a]` racing an in-flight drain replacement contends on the
// lock rather than slipping past it.
//
// Proxy for "concurrent dispatcher": hold coordlock.Acquire(project)
// directly from this test. Then call Resume to trigger the drain path
// for the same project. The drain branch MUST fail-fast (return error
// attributable to the lock) and MUST NOT spawn a replacement agent —
// the queue file is preserved untouched (queue.Delete only runs on the
// spawnAndRetire success path, which we bail out of before reaching).
// The drain loop / fsnotify watcher retries next cycle when the lock
// holder has released.
//
// Codex iter-1 [P1] in the v3 review surfaced that the original
// warn-and-continue contract from design v3 §Change 7 actually CONSUMED
// the queue file via queue.Delete on success while racing — defeating
// the gate's whole purpose. Fail-fast is the correct contract.
func TestDrainAndDispatch_ShareSameLock(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)
	oldRec := spawnSeedAgent(t)

	// Marker = oldRec.ID so isCoordHandoffForAgent fires.
	if _, err := state.EnsureProjectInitialized(oldRec.Project); err != nil {
		t.Fatalf("EnsureProjectInitialized: %v", err)
	}
	if err := state.WriteCoordSpawnMarker(oldRec.Project, oldRec.ID); err != nil {
		t.Fatalf("WriteCoordSpawnMarker: %v", err)
	}

	// Hold the lock outside Resume — simulates a concurrent
	// `fleet dispatch --coord-spawn` that's mid-flight when the
	// drain triggers.
	release, err := coordlock.Acquire(oldRec.Project)
	if err != nil {
		t.Fatalf("outer coordlock.Acquire: %v", err)
	}
	defer release()

	// Stub writeMarkerFn so a side effect of the drain block does
	// not depend on the rc package's real disk state.
	prev := writeMarkerFn
	writeMarkerFn = func(string) error { return nil }
	t.Cleanup(func() { writeMarkerFn = prev })

	req, qp, _ := writeSkillQueue(t, oldRec)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	rerr := Resume(req, qp, 0, stdout, stderr)

	// Resume MUST return error and the error MUST be attributable
	// to the lock contention — that's how drainOne knows to leave
	// the queue file for retry next cycle.
	if rerr == nil {
		t.Fatal("expected drain Resume to return error on lock contention; got nil")
	}
	if !strings.Contains(rerr.Error(), "coord-spawn lock contended") {
		t.Fatalf("expected error to mention 'coord-spawn lock contended'; got: %v", rerr)
	}

	// Warning still goes to stderr so the operator can see what
	// happened in real time.
	if !strings.Contains(stderr.String(), "coordlock.Acquire") {
		t.Errorf("expected drain stderr to mention coordlock.Acquire contention; got: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "contended during drain handoff") {
		t.Errorf("expected drain stderr to mention 'contended during drain handoff'; got: %q", stderr.String())
	}

	// CRITICAL invariant: no replacement agent record was written.
	// If a record exists, the drain raced past the lock — the bug
	// codex iter-1 surfaced.
	if _, lerr := agent.Load(req.NewAgentID); lerr == nil {
		t.Fatalf("replacement agent %s was spawned despite lock contention — fail-fast contract broken",
			req.NewAgentID)
	}
}

// TestDrain_LockReleasedOnReturn is T8 (v3 Change 7): the drain block's
// defer release() actually frees the lock after the drain returns. We
// run Resume to completion (no outer holder), then attempt a fresh
// coordlock.Acquire — it should succeed, proving the drain released.
func TestDrain_LockReleasedOnReturn(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)
	oldRec := spawnSeedAgent(t)

	if _, err := state.EnsureProjectInitialized(oldRec.Project); err != nil {
		t.Fatalf("EnsureProjectInitialized: %v", err)
	}
	if err := state.WriteCoordSpawnMarker(oldRec.Project, oldRec.ID); err != nil {
		t.Fatalf("WriteCoordSpawnMarker: %v", err)
	}

	prev := writeMarkerFn
	writeMarkerFn = func(string) error { return nil }
	t.Cleanup(func() { writeMarkerFn = prev })

	req, qp, _ := writeSkillQueue(t, oldRec)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	if err := Resume(req, qp, 0, stdout, stderr); err != nil {
		t.Fatalf("Resume: %v\n%s", err, stderr.String())
	}

	if newRec, lerr := agent.Load(req.NewAgentID); lerr == nil {
		t.Cleanup(func() { _ = tmux.Kill(newRec.TmuxSession) })
	}

	// After Resume returns, the lock must be free. A fresh acquire
	// proves the drain's defer release() fired.
	release, err := coordlock.Acquire(oldRec.Project)
	if err != nil {
		t.Fatalf("post-Resume coordlock.Acquire: lock still held; defer release() did not fire: %v", err)
	}
	release()
}

// TestDrain_ContentionPreservesQueueFile is T9 (v3 Change 7 corrected
// per v3 reviewer codex iter-1 [P1]): when the drain hits lock
// contention, it MUST NOT delete the queue file. The queue file must
// remain on disk so the next drain cycle / fsnotify-driven retry can
// re-process it after the lock holder releases.
//
// This is the inverse of the original design v3 §Change 7 rationale
// ("queue file would be orphaned if we bail") — the actual semantics
// are that queue.Delete only runs on the spawnAndRetire success path
// (lines ~387, ~796, ~868), so returning early from the
// isCoordHandoffForAgent block preserves the queue file untouched.
// Continuing past contention would consume the queue file AND race —
// defeating the entire atomic gate.
func TestDrain_ContentionPreservesQueueFile(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)
	oldRec := spawnSeedAgent(t)

	if _, err := state.EnsureProjectInitialized(oldRec.Project); err != nil {
		t.Fatalf("EnsureProjectInitialized: %v", err)
	}
	if err := state.WriteCoordSpawnMarker(oldRec.Project, oldRec.ID); err != nil {
		t.Fatalf("WriteCoordSpawnMarker: %v", err)
	}

	// Hold the lock from outside for the entire Resume.
	release, err := coordlock.Acquire(oldRec.Project)
	if err != nil {
		t.Fatalf("outer coordlock.Acquire: %v", err)
	}
	defer release()

	prev := writeMarkerFn
	writeMarkerFn = func(string) error { return nil }
	t.Cleanup(func() { writeMarkerFn = prev })

	req, qp, _ := writeSkillQueue(t, oldRec)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	rerr := Resume(req, qp, 0, stdout, stderr)

	// Drain MUST fail-fast with the lock-contention error.
	if rerr == nil {
		t.Fatal("expected drain Resume to return error on lock contention; got nil")
	}
	if !strings.Contains(rerr.Error(), "coord-spawn lock contended") {
		t.Fatalf("expected error to mention 'coord-spawn lock contended'; got: %v", rerr)
	}

	// CRITICAL invariant: queue file MUST still exist on disk.
	// This is what the next drain cycle picks up to retry.
	if _, statErr := os.Stat(qp); statErr != nil {
		t.Fatalf("queue file %s was deleted on lock contention — bug: fleet drain has no retry signal: %v",
			qp, statErr)
	}

	// CRITICAL invariant: no replacement agent record was written.
	if _, lerr := agent.Load(req.NewAgentID); lerr == nil {
		t.Fatalf("replacement agent %s was spawned despite lock contention — fail-fast contract broken",
			req.NewAgentID)
	}

	// Warning still emitted to stderr so the operator can see the
	// contention live.
	if !strings.Contains(stderr.String(), "coordlock.Acquire") {
		t.Errorf("expected stderr to mention coordlock.Acquire warning; got: %q", stderr.String())
	}
}
