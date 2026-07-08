package handoffop

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// requireFakeTmux installs the in-process fake tmux backend
// (internal/testutil/tmuxfake) + the fast pid resolver so the test
// exercises the real Resume/Drain code paths WITHOUT execing tmux(1) —
// the CI-perf lever (PR-2 of DESIGN-ci-3min-test-suite). It is the fast,
// orphan-proof substitute for requireTmux and a recognized isolation
// marker for lint-test-isolation.sh (the fake never touches the
// operator's default socket).
//
// Use requireFakeTmux for tests that only need a session abstraction
// (Spawn / Kill / HasSession / SendKeys) and assert on Fleet's own
// state. Use requireTmux only for tests that need REAL pane content or
// real process semantics. The env pins mirror requireTmux so any
// production timing path the test still touches stays fast.
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

// spawnSeedAgentWithCommand is spawnSeedAgent with a caller-supplied
// command (e.g. the wrapper-shaped `sh -c "claude ..."` body the RC
// inject matcher recognizes).
func spawnSeedAgentWithCommand(t *testing.T, command []string) *agent.Record {
	t.Helper()
	now := time.Now().UTC()
	rec := agent.New(agent.NewID())
	rec.TaskID = "auth-fix"
	rec.Project = "rainier"
	rec.SpawnedAt = now
	rec.LastActivityTS = now
	rec.Cwd = t.TempDir()
	rec.Command = append([]string(nil), command...)
	rec.TmuxSession = tmux.SessionName(rec.ID)

	if err := tmux.Spawn(rec.TmuxSession, rec.Cwd, rec.Command,
		[]string{"FLEET_AGENT_ID=" + rec.ID, "PATH=" + os.Getenv("PATH")}); err != nil {
		t.Fatalf("tmux.Spawn: %v", err)
	}
	rec.PID = 0
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
	requireFakeTmux(t)
	setupFleetHome(t)
	const project = "rainier"
	correctRepo := seedCoordRepoMeta(t, project)
	wrongCwd := t.TempDir()
	oldRec := spawnSeedCoord(t, project, wrongCwd)
	req, qp := writeCoordSkillQueue(t, oldRec)

	out := &bytes.Buffer{}
	if err := Resume(req, qp, 1, out, out); err != nil {
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
	requireFakeTmux(t)
	setupFleetHome(t)
	const project = "ghostproj" // no meta, no worktrees → refuse
	wrongCwd := t.TempDir()
	oldRec := spawnSeedCoord(t, project, wrongCwd)
	req, qp := writeCoordSkillQueue(t, oldRec)

	out := &bytes.Buffer{}
	err := Resume(req, qp, 1, out, out)
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
	requireFakeTmux(t)
	setupFleetHome(t)
	// Seed meta for the project to prove the worker path ignores it.
	otherRepo := seedCoordRepoMeta(t, "rainier")
	oldRec := spawnSeedAgent(t) // task_id "auth-fix" (worker), project "rainier"
	workerCwd := oldRec.Cwd
	req, qp, _ := writeSkillQueue(t, oldRec)

	out := &bytes.Buffer{}
	if err := Resume(req, qp, 1, out, out); err != nil {
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
	requireFakeTmux(t)
	setupFleetHome(t)
	oldRec := spawnSeedAgent(t)
	req, qp, _ := writeSkillQueue(t, oldRec)

	out := &bytes.Buffer{}
	if err := Resume(req, qp, 1, out, out); err != nil {
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
	requireFakeTmux(t)
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
	if err := Resume(req, qp, 1, out, out); err != nil {
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
	if err := Resume(req, qp, 1, out, out); err != nil {
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
	if err := Resume(req, qp, 1, out, out); err != nil {
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
	requireFakeTmux(t)
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
	if err := Resume(req, qp, 1, out, out); err != nil {
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
	requireFakeTmux(t)
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
	err := Resume(req, qp, 1, out, out)
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
	fake := requireFakeTmux(t)
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
		Command:        []string{"true"}, // crashed-at-startup stand-in
		PreAllocatedID: req.NewAgentID,
	})
	if err != nil {
		t.Fatalf("plant stale: %v", err)
	}
	// Make the planted session DEFINITIVELY dead. With real tmux the
	// `true` process would exit and tmux would reap the session; the fake
	// records an in-memory session that never exits on its own, so we kill
	// it explicitly to model the crashed-at-startup replacement. This is
	// deterministic (no exit-race poll / environment-quirk Skip — codex
	// iter-3 [P2], which otherwise silently disabled this regression).
	if err := fake.Kill(staleNew.TmuxSession); err != nil {
		t.Fatalf("kill planted stale session: %v", err)
	}
	if tmux.HasSession(staleNew.TmuxSession) {
		t.Fatal("planted stale session must be dead before Resume")
	}

	out := &bytes.Buffer{}
	if err := Resume(req, qp, 1, out, out); err != nil {
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
	requireFakeTmux(t)
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
	err := Resume(req, qp, 1, out, out)
	if err == nil || !strings.Contains(err.Error(), "legacy record") {
		t.Errorf("expected legacy-record refusal, got: %v", err)
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
	requireFakeTmux(t)
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
	resumeErr := Resume(req, qp, 1, out, out)
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
	requireFakeTmux(t)
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
	if err := Resume(req, qp, 1, out, out); err != nil {
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

// TestIsCoordHandoffForAgent_GatesOnCoordTask pins the drain coord predicate.
// spawnAndRetire uses it for the coord-spawn lock, and spawn.Spawn uses the
// same task/project convention for centralized RC injection. Worker handoffs
// resumed via fleet drain / crash recovery must not contend on the coord lock
// and must not inherit RC attach.
//
// The coord-spawn marker is gone (D3): isCoordHandoffForAgent now takes an
// *agent.Record and returns spawn.IsCoordSpawn(rec.TaskID, rec.Project) —
// true iff the task_id is coord-<project>.
func TestIsCoordHandoffForAgent_GatesOnCoordTask(t *testing.T) {
	const project = "test-drain-gate"

	cases := []struct {
		name    string
		taskID  string
		project string
		want    bool
	}{
		{"empty project rejects", "coord-" + project, "", false},
		{"worker task rejects", "auth-fix", project, false},
		{"non-coord feature task rejects", "some-feature", project, false},
		{"coord task accepts", "coord-" + project, project, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &agent.Record{TaskID: tc.taskID, Project: tc.project}
			got := isCoordHandoffForAgent(rec)
			if got != tc.want {
				t.Errorf("isCoordHandoffForAgent(task=%q project=%q): got %v, want %v",
					tc.taskID, tc.project, got, tc.want)
			}
		})
	}
}

func TestDrain_LeaseFailoverCoordHandoff_ResumeFallbackCompletes(t *testing.T) {
	requireFakeTmux(t)
	setupFleetHome(t)

	const project = "rainier"
	cwd := seedCoordRepoMeta(t, project)
	oldRec := spawnSeedCoord(t, project, cwd)
	oldRec.SupervisorPID = 4242
	if err := oldRec.Write(); err != nil {
		t.Fatalf("rewrite old coord supervisor pid: %v", err)
	}
	// D3: coord identity is the task_id (coord-<project>) + lease, no marker.

	req, qp := writeCoordSkillQueue(t, oldRec)
	origDeliveryDeps := handoffDeliveryDepsFn
	origDeliveryTimeout := handoffDeliveryTimeout
	origDeliveryPoll := handoffDeliveryPoll
	origSendPromptKeysVerified := sendPromptKeysVerified
	t.Cleanup(func() {
		handoffDeliveryDepsFn = origDeliveryDeps
		handoffDeliveryTimeout = origDeliveryTimeout
		handoffDeliveryPoll = origDeliveryPoll
		sendPromptKeysVerified = origSendPromptKeysVerified
	})
	handoffDeliveryTimeout = time.Second
	handoffDeliveryPoll = time.Millisecond
	handoffDeliveryDepsFn = func() handoffdelivery.Deps {
		deps := handoffdelivery.DefaultDeps()
		deps.CurrentOwner = func(project string) (coordlock.Owner, bool) {
			t.Fatalf("legacy bare coord resume must not poll lock owner for project %q", project)
			return coordlock.Owner{}, false
		}
		deps.WaitReady = func(string) error { return nil }
		deps.SessionAlive = func(string) (bool, error) { return true, nil }
		deps.SendVerified = func(string, string) (bool, error) { return true, nil }
		deps.Sleep = func(time.Duration) {}
		return deps
	}
	var sentSession, sentPrompt string
	sendPromptKeysVerified = func(session, prompt string) (bool, error) {
		sentSession = session
		sentPrompt = prompt
		return true, nil
	}
	out := &bytes.Buffer{}
	if err := Resume(req, qp, 1, out, out); err != nil {
		t.Fatalf("Resume must complete the drain cold-resume fallback, got: %v\n%s", err, out.String())
	}
	if _, lerr := agent.Load(oldRec.ID); !errors.Is(lerr, state.ErrNotFound) {
		t.Fatalf("old coord record should be archived after fallback resume, load err=%v", lerr)
	}
	if _, statErr := os.Stat(qp); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("queue file should be consumed, stat err=%v", statErr)
	}
	newRec, lerr := agent.Load(req.NewAgentID)
	if lerr != nil {
		t.Fatalf("replacement record should remain live after fallback resume: %v", lerr)
	}
	t.Cleanup(func() { _ = tmux.Kill(newRec.TmuxSession) })
	if sentSession != newRec.TmuxSession {
		t.Fatalf("resume prompt sent to %q, want direct successor session %q", sentSession, newRec.TmuxSession)
	}
	if !strings.Contains(sentPrompt, req.HandoffDoc) {
		t.Fatalf("resume prompt %q does not reference handoff doc %q", sentPrompt, req.HandoffDoc)
	}
}

func TestDrain_LeaseFailoverCoordHandoff_FinalizesDeliveredLockOwner(t *testing.T) {
	requireFakeTmux(t)
	setupFleetHome(t)

	// D3: coord-swap detection is now the task_id (coord-<project>), so the
	// OLD agent must be a real coord for the cold-spawn delivery routing under
	// test (a worker never lease-wraps and would trivially direct-send).
	const project = "rainier"
	cwd := seedCoordRepoMeta(t, project)
	oldRec := spawnSeedCoord(t, project, cwd)

	winner := agent.New(agent.NewID())
	winner.TaskID = oldRec.TaskID
	winner.Project = oldRec.Project
	winner.Cwd = oldRec.Cwd
	winner.Command = oldRec.Command
	winner.TmuxSession = tmux.SessionName(winner.ID)
	winner.SpawnedAt = time.Now().UTC()
	winner.LastActivityTS = winner.SpawnedAt
	winner.SupervisorPID = 9001
	winner.SupervisorPidStart = 99
	if err := winner.Write(); err != nil {
		t.Fatalf("write lock-winning standby: %v", err)
	}

	req, qp, _ := writeSkillQueue(t, oldRec)
	// Fake-backed spawnAndRetire records LeaseWrapped=false because the fake tmux
	// backend cannot execute coord-run. Such a successor must go through the
	// legacy direct-send branch; CurrentOwner must not be consulted.
	_ = winner
	origDeliveryDeps := handoffDeliveryDepsFn
	origSend := sendPromptKeysVerified
	t.Cleanup(func() {
		handoffDeliveryDepsFn = origDeliveryDeps
		sendPromptKeysVerified = origSend
	})
	handoffDeliveryDepsFn = func() handoffdelivery.Deps {
		deps := handoffdelivery.DefaultDeps()
		deps.CurrentOwner = func(string) (coordlock.Owner, bool) {
			t.Fatalf("fake-backed bare successor must NOT poll the lock owner")
			return coordlock.Owner{}, false
		}
		return deps
	}
	var sentSession, sentPrompt string
	sendPromptKeysVerified = func(session, prompt string) (bool, error) {
		sentSession, sentPrompt = session, prompt
		return true, nil
	}

	out := &bytes.Buffer{}
	if err := Resume(req, qp, 1, out, out); err != nil {
		t.Fatalf("Resume should deliver directly to the cold-spawned successor: %v\n%s", err, out.String())
	}
	newRec, lerr := agent.Load(req.NewAgentID)
	if lerr != nil {
		t.Fatalf("cold-spawned successor record should exist: %v", lerr)
	}
	t.Cleanup(func() { _ = tmux.Kill(newRec.TmuxSession) })
	if sentSession != newRec.TmuxSession {
		t.Fatalf("resume prompt sent to %q, want cold-spawned successor %q", sentSession, newRec.TmuxSession)
	}
	if !strings.Contains(sentPrompt, "Read your handoff doc at ") {
		t.Fatalf("resume prompt has wrong shape: %q", sentPrompt)
	}
	arc, err := agent.LoadArchive(oldRec.ID)
	if err != nil {
		t.Fatalf("LoadArchive(%s): %v", oldRec.ID, err)
	}
	if arc.SuccessorID != req.NewAgentID {
		t.Fatalf("archive successor = %q, want cold-spawned successor %q", arc.SuccessorID, req.NewAgentID)
	}
	if _, statErr := os.Stat(qp); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("queue file should be consumed after verified delivery, stat err=%v", statErr)
	}
}

func TestDrain_LeaseFailoverCoordStaleQueue_UsesLockOwnerForCapApprovedCoord(t *testing.T) {
	requireFakeTmux(t)
	setupFleetHome(t)

	const project = "rainier"
	cwd := seedCoordRepoMeta(t, project)
	oldRec := spawnSeedCoord(t, project, cwd)
	newRec := agent.New(agent.NewID())
	newRec.TaskID = oldRec.TaskID
	newRec.Project = oldRec.Project
	newRec.Cwd = oldRec.Cwd
	newRec.Command = oldRec.Command
	newRec.TmuxSession = tmux.SessionName(newRec.ID)
	newRec.SpawnedAt = time.Now().UTC()
	newRec.LastActivityTS = newRec.SpawnedAt
	newRec.LeaseWrapped = true // a lease-wrapped replacement -> lock-owner delivery
	if err := tmux.Spawn(newRec.TmuxSession, newRec.Cwd, newRec.Command,
		[]string{"FLEET_AGENT_ID=" + newRec.ID}); err != nil {
		t.Fatalf("spawn journaled replacement: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(newRec.TmuxSession) })
	if err := newRec.Write(); err != nil {
		t.Fatalf("write journaled replacement: %v", err)
	}

	winner := agent.New(agent.NewID())
	winner.TaskID = oldRec.TaskID
	winner.Project = oldRec.Project
	winner.Cwd = oldRec.Cwd
	winner.Command = oldRec.Command
	winner.TmuxSession = tmux.SessionName(winner.ID)
	winner.SpawnedAt = time.Now().UTC()
	winner.LastActivityTS = winner.SpawnedAt
	winner.SupervisorPID = 9001
	winner.SupervisorPidStart = 99
	if err := winner.Write(); err != nil {
		t.Fatalf("write lock-winning standby: %v", err)
	}
	if err := oldRec.ArchiveWithHandoff(newRec.ID); err != nil {
		t.Fatalf("archive old coord: %v", err)
	}

	req, qp := writeCoordSkillQueue(t, oldRec)
	req.NewAgentID = newRec.ID
	req.NewSession = newRec.TmuxSession
	req.CapApproved = true
	if _, err := queue.WriteSpawnFresh(req); err != nil {
		t.Fatalf("rewrite stale queue: %v", err)
	}

	origDeliveryDeps := handoffDeliveryDepsFn
	origDeliveryTimeout := handoffDeliveryTimeout
	origDeliveryPoll := handoffDeliveryPoll
	t.Cleanup(func() {
		handoffDeliveryDepsFn = origDeliveryDeps
		handoffDeliveryTimeout = origDeliveryTimeout
		handoffDeliveryPoll = origDeliveryPoll
	})
	handoffDeliveryTimeout = time.Second
	handoffDeliveryPoll = time.Millisecond
	var sentSession string
	handoffDeliveryDepsFn = func() handoffdelivery.Deps {
		deps := handoffdelivery.DefaultDeps()
		deps.CurrentOwner = func(project string) (coordlock.Owner, bool) {
			if project != oldRec.Project {
				t.Fatalf("current owner checked for project %q, want %q", project, oldRec.Project)
			}
			return coordlock.Owner{AgentID: winner.ID, PID: winner.SupervisorPID, PidStart: winner.SupervisorPidStart}, true
		}
		deps.WaitReady = func(session string) error {
			if session != winner.TmuxSession {
				t.Fatalf("wait-ready session = %q, want delivered owner session %q", session, winner.TmuxSession)
			}
			return nil
		}
		deps.SessionAlive = func(session string) (bool, error) {
			if session != winner.TmuxSession {
				t.Fatalf("session-alive probe = %q, want delivered owner session %q", session, winner.TmuxSession)
			}
			return true, nil
		}
		deps.SendVerified = func(session, prompt string) (bool, error) {
			sentSession = session
			if session != winner.TmuxSession {
				t.Fatalf("resume prompt session = %q, want delivered owner session %q", session, winner.TmuxSession)
			}
			if !strings.Contains(prompt, "Read your handoff doc at ") {
				t.Fatalf("resume prompt has wrong shape: %q", prompt)
			}
			return true, nil
		}
		deps.Sleep = func(time.Duration) {}
		return deps
	}

	out := &bytes.Buffer{}
	if err := cleanUpStaleQueue(req, qp, out); err != nil {
		t.Fatalf("cleanUpStaleQueue should deliver to lock owner: %v\n%s", err, out.String())
	}
	if sentSession != winner.TmuxSession {
		t.Fatalf("resume prompt sent to %q, want %q", sentSession, winner.TmuxSession)
	}
	arc, err := agent.LoadArchive(oldRec.ID)
	if err != nil {
		t.Fatalf("LoadArchive(%s): %v", oldRec.ID, err)
	}
	if arc.SuccessorID != winner.ID {
		t.Fatalf("archive successor = %q, want delivered lock owner %q", arc.SuccessorID, winner.ID)
	}
	if _, lerr := agent.Load(newRec.ID); !errors.Is(lerr, state.ErrNotFound) {
		t.Fatalf("superseded replacement %s should have been removed, load err=%v", newRec.ID, lerr)
	}
	if _, statErr := os.Stat(qp); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("queue file should be consumed after verified delivery, stat err=%v", statErr)
	}
}

// TestDrain_LeaseFailoverCoord_DeadLoserSession_StillDeliversToLockOwner pins
// codex iter-31 P1: when the stale queue names a lease-wrapped replacement that
// was SUPERSEDED by a racing standby and has since EXITED, cleanUpStaleQueue
// must NOT abort on the dead replacement session. For a coord lock-owner
// delivery the live coordinator is the lock WINNER (discovered via
// CurrentOwner), not the dead queued loser — so delivery proceeds to the winner
// and the queue is consumed, instead of being stranded pending forever.
func TestDrain_LeaseFailoverCoord_DeadLoserSession_StillDeliversToLockOwner(t *testing.T) {
	requireFakeTmux(t)
	setupFleetHome(t)

	const project = "rainier"
	cwd := seedCoordRepoMeta(t, project)
	oldRec := spawnSeedCoord(t, project, cwd)

	// The queued replacement (the LOSER): lease-wrapped, but its session is
	// NEVER spawned — it exited after losing the lock race. tmux.HasSession is
	// false for it; pre-iter-31 this aborted cleanUpStaleQueue.
	newRec := agent.New(agent.NewID())
	newRec.TaskID = oldRec.TaskID
	newRec.Project = oldRec.Project
	newRec.Cwd = oldRec.Cwd
	newRec.Command = oldRec.Command
	newRec.TmuxSession = tmux.SessionName(newRec.ID) // session deliberately NOT spawned
	newRec.SpawnedAt = time.Now().UTC()
	newRec.LastActivityTS = newRec.SpawnedAt
	newRec.LeaseWrapped = true // lease-wrapped -> coord lock-owner delivery
	if err := newRec.Write(); err != nil {
		t.Fatalf("write journaled (dead) replacement: %v", err)
	}

	// The live winner (a racing standby that won the lock).
	winner := agent.New(agent.NewID())
	winner.TaskID = oldRec.TaskID
	winner.Project = oldRec.Project
	winner.Cwd = oldRec.Cwd
	winner.Command = oldRec.Command
	winner.TmuxSession = tmux.SessionName(winner.ID)
	winner.SpawnedAt = time.Now().UTC()
	winner.LastActivityTS = winner.SpawnedAt
	winner.SupervisorPID = 9002
	winner.SupervisorPidStart = 77
	if err := winner.Write(); err != nil {
		t.Fatalf("write lock-winning standby: %v", err)
	}
	if err := oldRec.ArchiveWithHandoff(newRec.ID); err != nil {
		t.Fatalf("archive old coord: %v", err)
	}

	req, qp := writeCoordSkillQueue(t, oldRec)
	req.NewAgentID = newRec.ID
	req.NewSession = newRec.TmuxSession
	req.CapApproved = true
	if _, err := queue.WriteSpawnFresh(req); err != nil {
		t.Fatalf("rewrite stale queue: %v", err)
	}

	origDeliveryDeps := handoffDeliveryDepsFn
	origDeliveryTimeout := handoffDeliveryTimeout
	origDeliveryPoll := handoffDeliveryPoll
	t.Cleanup(func() {
		handoffDeliveryDepsFn = origDeliveryDeps
		handoffDeliveryTimeout = origDeliveryTimeout
		handoffDeliveryPoll = origDeliveryPoll
	})
	handoffDeliveryTimeout = time.Second
	handoffDeliveryPoll = time.Millisecond
	var sentSession string
	handoffDeliveryDepsFn = func() handoffdelivery.Deps {
		deps := handoffdelivery.DefaultDeps()
		deps.CurrentOwner = func(string) (coordlock.Owner, bool) {
			return coordlock.Owner{AgentID: winner.ID, PID: winner.SupervisorPID, PidStart: winner.SupervisorPidStart}, true
		}
		deps.WaitReady = func(string) error { return nil }
		deps.SessionAlive = func(string) (bool, error) { return true, nil }
		deps.SendVerified = func(session, _ string) (bool, error) {
			sentSession = session
			return true, nil
		}
		deps.Sleep = func(time.Duration) {}
		return deps
	}

	out := &bytes.Buffer{}
	if err := cleanUpStaleQueue(req, qp, out); err != nil {
		t.Fatalf("cleanUpStaleQueue must deliver to the lock owner despite the dead loser session: %v\n%s", err, out.String())
	}
	if sentSession != winner.TmuxSession {
		t.Fatalf("resume prompt sent to %q, want lock winner %q (must not require dead loser session)", sentSession, winner.TmuxSession)
	}
	if _, statErr := os.Stat(qp); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("queue file must be consumed after delivery to the lock owner, stat err=%v", statErr)
	}
}

// TestDrain_WorkerHandoff_NoMarkerWrite pins
// T-drain-worker-no-marker-write. A worker handoff (marker absent or
// pointing elsewhere) MUST NOT touch the rc-enabled marker via the
// drain path. Preserves the v0.12 strict opt-in for workers /
// subagents (push-storm protection).
// TestDrain_WorkerHandoff_NeverCarriesRemoteControl pins the worker
// carve-out under the native default-on model: RC is enabled
// project-wide by default, but a WORKER handoff replacement must never
// inherit --remote-control — the push-storm protection lives at the
// isCoordHandoffForAgent call-site gate.
//
// /review specialist S1: asserting on the persisted Command alone is
// tautological (spawn ALWAYS persists the clean command; the flag
// rides on the per-spawn exec argv). So this test observes the argv
// the replacement pane ACTUALLY execs, via the PATH-shim technique
// from cmd/fleet/handoff_test.go:TestHandoff_ReplacementSpawnedWith
// RemoteControlFlag: a fake `claude` script echoes its argv into the
// pane; the wrapper-shaped Command makes the inject eligible, so if
// the coord gate ever stopped carving out workers, the flag WOULD
// appear in the pane and this test fails.
func TestDrain_WorkerHandoff_NeverCarriesRemoteControl(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)
	t.Setenv("FLEET_RC_BOOTSTRAP_DISABLED", "")

	// Fake `claude` shim on PATH: echoes argv lines into the pane,
	// then blocks on cat so the session stays alive for capture.
	shimDir := t.TempDir()
	shimPath := filepath.Join(shimDir, "claude")
	if err := os.WriteFile(shimPath,
		[]byte("#!/bin/sh\nprintf 'SHIM-ARGV %s\\n' \"$@\"\ncat\n"), 0o755); err != nil {
		t.Fatalf("write shim: %v", err)
	}
	t.Setenv("PATH", shimDir+":"+os.Getenv("PATH"))

	// Wrapper-shaped seed command — the inject matcher WOULD rewrite
	// this shape, so a missing worker carve-out is observable.
	oldRec := spawnSeedAgentWithCommand(t,
		[]string{"sh", "-c", `claude --dangerously-skip-permissions; cat`})

	// Worker task ("auth-fix") → isCoordHandoffForAgent=false (task-based,
	// D3: no coord-spawn marker anymore).

	req, qp, _ := writeSkillQueue(t, oldRec)
	out := &bytes.Buffer{}
	if err := Resume(req, qp, 1, out, out); err != nil {
		t.Fatalf("Resume: %v\n%s", err, out.String())
	}

	newRec, lerr := agent.Load(req.NewAgentID)
	if lerr != nil {
		t.Fatalf("load replacement record: %v", lerr)
	}
	t.Cleanup(func() { _ = tmux.Kill(newRec.TmuxSession) })

	// Wait for the shim's positive signal in the pane, then assert the
	// flag is absent from the echoed argv.
	deadline := time.Now().Add(3 * time.Second)
	var pane string
	for time.Now().Before(deadline) {
		raw, capErr := exec.Command(
			"tmux", "-S", os.Getenv("FLEET_TMUX_SOCKET"),
			"capture-pane", "-pt", newRec.TmuxSession,
		).Output()
		if capErr == nil {
			pane = string(raw)
			if strings.Contains(pane, "SHIM-ARGV") {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(pane, "SHIM-ARGV") {
		t.Fatalf("shim never echoed argv (positive signal missing); pane:\n%s", pane)
	}
	if strings.Contains(pane, "--remote-control") {
		t.Fatalf("worker drain replacement must NEVER exec --remote-control (push-storm protection); pane:\n%s", pane)
	}
	// Persisted record stays clean too (secondary, non-tautological
	// here only as lineage hygiene).
	for _, el := range newRec.Command {
		if strings.Contains(el, "--remote-control") {
			t.Fatalf("persisted Command must stay clean; argv=%v", newRec.Command)
		}
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
	requireFakeTmux(t)
	setupFleetHome(t)
	// D3: a coord (task_id coord-<project>) makes isCoordHandoffForAgent fire
	// — the coord-spawn lock branch under test. Fails fast at coordlock.Acquire
	// before any repo resolution, so no meta seeding is needed.
	oldRec := spawnSeedCoord(t, "rainier", t.TempDir())

	// Hold the lock outside Resume — simulates a concurrent
	// `fleet dispatch --coord-spawn` that's mid-flight when the
	// drain triggers.
	release, err := coordlock.Acquire(oldRec.Project)
	if err != nil {
		t.Fatalf("outer coordlock.Acquire: %v", err)
	}
	defer release()

	req, qp, _ := writeSkillQueue(t, oldRec)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	rerr := Resume(req, qp, 1, stdout, stderr)

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
	requireFakeTmux(t)
	setupFleetHome(t)
	// D3: coord (task_id coord-<project>) runs the coord-spawn lock branch.
	// This Resume runs to completion, so the coord repo must resolve.
	cwd := seedCoordRepoMeta(t, "rainier")
	oldRec := spawnSeedCoord(t, "rainier", cwd)

	req, qp := writeCoordSkillQueue(t, oldRec)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	if err := Resume(req, qp, 1, stdout, stderr); err != nil {
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
	requireFakeTmux(t)
	setupFleetHome(t)
	// D3: a coord (task_id coord-<project>) drives the coord-spawn lock branch;
	// it fails fast at coordlock.Acquire before repo resolution.
	oldRec := spawnSeedCoord(t, "rainier", t.TempDir())

	// Hold the lock from outside for the entire Resume.
	release, err := coordlock.Acquire(oldRec.Project)
	if err != nil {
		t.Fatalf("outer coordlock.Acquire: %v", err)
	}
	defer release()

	req, qp, _ := writeSkillQueue(t, oldRec)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	rerr := Resume(req, qp, 1, stdout, stderr)

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

// T7: a successor stamped LeaseWrapped=true is owner-only by construction. If
// no owner is observed yet, delivery stays pending; direct-sending would type
// into the coord-run supervisor pane and could consume the queue without
// delivering the handoff doc to the eventual owner.
func TestDeliverResumePrompt_LeaseWrapped_NoOwner_StaysPending(t *testing.T) {
	setupFleetHome(t)

	rec := agent.New("legacynew1")
	rec.Project = "rainier"
	rec.TmuxSession = "fleet-legacynew1"

	origDeps := handoffDeliveryDepsFn
	origTimeout := handoffDeliveryTimeout
	origPoll := handoffDeliveryPoll
	origSend := sendPromptKeysVerified
	t.Cleanup(func() {
		handoffDeliveryDepsFn = origDeps
		handoffDeliveryTimeout = origTimeout
		handoffDeliveryPoll = origPoll
		sendPromptKeysVerified = origSend
	})
	handoffDeliveryTimeout = 100 * time.Millisecond
	handoffDeliveryPoll = time.Millisecond
	// Lock-owner branch entered (leaseWrappedSuccessor=true) but no owner ever
	// resolves -> ErrNoOwnerObserved must propagate.
	handoffDeliveryDepsFn = func() handoffdelivery.Deps {
		deps := handoffdelivery.DefaultDeps()
		deps.CurrentOwner = func(string) (coordlock.Owner, bool) {
			return coordlock.Owner{}, false
		}
		deps.Sleep = func(time.Duration) {}
		return deps
	}
	var sentSession string
	sendPromptKeysVerified = func(session, prompt string) (bool, error) {
		sentSession = session
		return true, nil
	}

	out := &bytes.Buffer{}
	delivered, err := deliverResumePrompt(rec.Project, true, true, rec, "/tmp/handoff.md", out, out)
	if !errors.Is(err, handoffdelivery.ErrNoOwnerObserved) {
		t.Fatalf("want ErrNoOwnerObserved so queue stays pending, got delivered=%+v err=%v\n%s",
			delivered, err, out.String())
	}
	if delivered != nil {
		t.Fatalf("delivered = %+v, want nil while owner is unobserved", delivered)
	}
	if sentSession != "" {
		t.Fatalf("lease-wrapped no-owner path direct-sent to %q; must stay pending", sentSession)
	}
}

// codex iter-18 [P1] REGRESSION: the case-3 Resume path (old record still live,
// replacement already spawned) routes through retireOldAgent. For a CapApproved
// coord handoff the resume prompt MUST go to the lock WINNER
// (DeliverToCurrentOwner), not the queued session which may have
// lost the lease. Before the leaseWrappedSuccessor threading, retireOldAgent
// derived lease-wrapping from TaskID alone and direct-sent to the loser.
func TestResume_Case3_CapApprovedCoord_DeliversToLockOwner(t *testing.T) {
	requireFakeTmux(t)
	setupFleetHome(t)

	const project = "rainier"
	cwd := seedCoordRepoMeta(t, project)
	oldRec := spawnSeedCoord(t, project, cwd) // NOT archived → case-3

	// Journaled replacement: spawned + alive, named by the queue.
	newRec := agent.New(agent.NewID())
	newRec.TaskID = oldRec.TaskID
	newRec.Project = oldRec.Project
	newRec.Cwd = oldRec.Cwd
	newRec.Command = oldRec.Command
	newRec.TmuxSession = tmux.SessionName(newRec.ID)
	newRec.SpawnedAt = time.Now().UTC()
	newRec.LastActivityTS = newRec.SpawnedAt
	newRec.LeaseWrapped = true // lease-wrapped replacement -> lock-owner delivery
	if err := tmux.Spawn(newRec.TmuxSession, newRec.Cwd, newRec.Command,
		[]string{"FLEET_AGENT_ID=" + newRec.ID}); err != nil {
		t.Fatalf("spawn journaled replacement: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(newRec.TmuxSession) })
	if err := newRec.Write(); err != nil {
		t.Fatalf("write journaled replacement: %v", err)
	}

	// Lock winner: a different standby that won the lease.
	winner := agent.New(agent.NewID())
	winner.TaskID = oldRec.TaskID
	winner.Project = oldRec.Project
	winner.Cwd = oldRec.Cwd
	winner.Command = oldRec.Command
	winner.TmuxSession = tmux.SessionName(winner.ID)
	winner.SpawnedAt = time.Now().UTC()
	winner.LastActivityTS = winner.SpawnedAt
	winner.SupervisorPID = 9001
	winner.SupervisorPidStart = 99
	if err := winner.Write(); err != nil {
		t.Fatalf("write lock-winning standby: %v", err)
	}
	// D3: isCoordSwap fires in case-3 off the coord task_id (coord-<project>).

	req, qp := writeCoordSkillQueue(t, oldRec)
	req.NewAgentID = newRec.ID
	req.NewSession = newRec.TmuxSession
	req.CapApproved = true // the authoritative lease-wrapped signal
	if _, err := queue.WriteSpawnFresh(req); err != nil {
		t.Fatalf("rewrite queue: %v", err)
	}

	origDeliveryDeps := handoffDeliveryDepsFn
	origDeliveryTimeout := handoffDeliveryTimeout
	origDeliveryPoll := handoffDeliveryPoll
	t.Cleanup(func() {
		handoffDeliveryDepsFn = origDeliveryDeps
		handoffDeliveryTimeout = origDeliveryTimeout
		handoffDeliveryPoll = origDeliveryPoll
	})
	handoffDeliveryTimeout = time.Second
	handoffDeliveryPoll = time.Millisecond
	var sentSession string
	handoffDeliveryDepsFn = func() handoffdelivery.Deps {
		deps := handoffdelivery.DefaultDeps()
		deps.CurrentOwner = func(string) (coordlock.Owner, bool) {
			return coordlock.Owner{AgentID: winner.ID, PID: winner.SupervisorPID, PidStart: winner.SupervisorPidStart}, true
		}
		deps.WaitReady = func(string) error { return nil }
		deps.SessionAlive = func(string) (bool, error) { return true, nil }
		deps.SendVerified = func(session, prompt string) (bool, error) {
			sentSession = session
			return true, nil
		}
		deps.Sleep = func(time.Duration) {}
		return deps
	}

	out := &bytes.Buffer{}
	if err := Resume(req, qp, 1, out, out); err != nil {
		t.Fatalf("Resume case-3 should deliver to lock owner: %v\n%s", err, out.String())
	}
	if sentSession != winner.TmuxSession {
		t.Fatalf("resume prompt sent to %q, want lock WINNER %q (not the queued loser %q)",
			sentSession, winner.TmuxSession, newRec.TmuxSession)
	}
	if _, statErr := os.Stat(qp); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("queue file should be consumed after verified delivery, stat err=%v", statErr)
	}
}

// T3/T7: spawnAndRetire COLD-spawns a coord successor without opting out of the
// lease wrapper. Because the successor record is stamped LeaseWrapped=true, the
// resume prompt must route through DeliverToCurrentOwner rather than direct-send
// to the queued session.
func TestDrain_CoordColdSpawn_CapApproved_LeaseWrapsAndDeliversToOwner(t *testing.T) {
	requireFakeTmux(t)
	setupFleetHome(t)

	const project = "rainier"
	resolvedRepo := seedCoordRepoMeta(t, project)
	// Old coord (taskID coord-<project> → legacyCoordResume=true), NOT archived,
	// NO pre-spawned replacement → Resume routes to spawnAndRetire (cold spawn).
	oldRec := spawnSeedCoord(t, project, t.TempDir())

	req, qp := writeCoordSkillQueue(t, oldRec)
	req.CapApproved = true
	if _, err := queue.WriteSpawnFresh(req); err != nil {
		t.Fatalf("rewrite queue: %v", err)
	}

	origSpawn := spawnAgentFn
	origDeliveryDeps := handoffDeliveryDepsFn
	origSend := sendPromptKeysVerified
	t.Cleanup(func() {
		spawnAgentFn = origSpawn
		handoffDeliveryDepsFn = origDeliveryDeps
		sendPromptKeysVerified = origSend
	})
	var gotSpawnOpts spawn.Options
	var spawnedRec *agent.Record
	var sentSession, sentPrompt string
	spawnAgentFn = func(opts spawn.Options) (*agent.Record, error) {
		gotSpawnOpts = opts
		rec := agent.New(opts.PreAllocatedID)
		rec.TaskID = opts.OldRecord.TaskID
		rec.Project = opts.OldRecord.Project
		rec.Cwd = opts.Cwd
		rec.Command = append([]string(nil), opts.Command...)
		rec.TmuxSession = tmux.SessionName(rec.ID)
		rec.SpawnedAt = time.Now().UTC()
		rec.LastActivityTS = rec.SpawnedAt
		rec.HandoffNumber = opts.OldRecord.HandoffNumber + 1
		rec.PredecessorID = opts.OldRecord.ID
		rec.LeaseWrapped = true
		if err := tmux.Spawn(rec.TmuxSession, rec.Cwd, rec.Command, []string{"FLEET_AGENT_ID=" + rec.ID}); err != nil {
			return nil, err
		}
		if err := rec.Write(); err != nil {
			return nil, err
		}
		spawnedRec = rec
		return rec, nil
	}
	handoffDeliveryDepsFn = func() handoffdelivery.Deps {
		deps := handoffdelivery.DefaultDeps()
		deps.CurrentOwner = func(string) (coordlock.Owner, bool) {
			return coordlock.Owner{AgentID: spawnedRec.ID, PID: 9001, PidStart: 99}, true
		}
		deps.WaitReady = func(string) error { return nil }
		deps.SessionAlive = func(string) (bool, error) { return true, nil }
		deps.SendVerified = func(session, prompt string) (bool, error) {
			sentSession, sentPrompt = session, prompt
			return true, nil
		}
		deps.Sleep = func(time.Duration) {}
		return deps
	}
	sendPromptKeysVerified = func(session, prompt string) (bool, error) {
		t.Fatalf("cold-spawned lease-wrapped coord must not direct-send to %q", session)
		return true, nil
	}

	out := &bytes.Buffer{}
	if err := Resume(req, qp, 1, out, out); err != nil {
		t.Fatalf("Resume cold-spawn coord should deliver to lock owner: %v\n%s", err, out.String())
	}
	if gotSpawnOpts.DisableLeaseWrap {
		t.Fatal("cold coord resume passed DisableLeaseWrap=true; want lease-wrapped successor")
	}
	if gotSpawnOpts.Cwd != resolvedRepo {
		t.Fatalf("spawn cwd = %q, want resolved repo %q", gotSpawnOpts.Cwd, resolvedRepo)
	}
	newRec, lerr := agent.Load(req.NewAgentID)
	if lerr != nil {
		t.Fatalf("cold-spawned successor record should exist: %v", lerr)
	}
	t.Cleanup(func() { _ = tmux.Kill(newRec.TmuxSession) })
	if !newRec.LeaseWrapped {
		t.Fatal("cold-spawned successor LeaseWrapped=false, want true")
	}
	if sentSession != newRec.TmuxSession {
		t.Fatalf("resume prompt sent to %q, want owner session %q", sentSession, newRec.TmuxSession)
	}
	if !strings.Contains(sentPrompt, "Read your handoff doc at ") {
		t.Fatalf("resume prompt has wrong shape: %q", sentPrompt)
	}
	if _, statErr := os.Stat(qp); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("queue file should be consumed after verified delivery, stat err=%v", statErr)
	}
}

// ---------------------------------------------------------------------------
// PR3 (DESIGN-coord-spawn-unified-standby §6) — the live-coord auto/drain
// handoff routes through the GracefulHandoff barrier (gracefulCoordSwapFn)
// instead of the bespoke AtomicCoordSwap + separate deliverResumePrompt
// sequence, and the successor for that route spawns LEASE-WRAPPED so the
// winner heartbeats the lease (a bare successor's lease lapses — the
// fd4ee2e6 duplicate-coord incident).
// ---------------------------------------------------------------------------

// seedGracefulRouteCase3 builds the Resume case-3 shape (old coord live +
// journaled replacement already spawned + alive + lease-wrapped) that the
// graceful-route tests drive.
func seedGracefulRouteCase3(t *testing.T) (oldRec, newRec *agent.Record, req queue.SpawnFresh, qp string) {
	t.Helper()
	const project = "rainier"
	cwd := seedCoordRepoMeta(t, project)
	oldRec = spawnSeedCoord(t, project, cwd)

	newRec = agent.New(agent.NewID())
	newRec.TaskID = oldRec.TaskID
	newRec.Project = oldRec.Project
	newRec.Cwd = oldRec.Cwd
	newRec.Command = oldRec.Command
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
	// D3: coord identity is the task_id (coord-<project>) + lease, no marker.
	req, qp = writeCoordSkillQueue(t, oldRec)
	req.NewAgentID = newRec.ID
	req.NewSession = newRec.TmuxSession
	if _, err := queue.WriteSpawnFresh(req); err != nil {
		t.Fatalf("rewrite queue: %v", err)
	}
	return oldRec, newRec, req, qp
}

// forbidSeparateDelivery makes any use of the bespoke delivery machinery a
// test failure: on the graceful route the doc is injected INSIDE the barrier
// completion (the swap closure), never by retireOldAgent's own send.
func forbidSeparateDelivery(t *testing.T) {
	t.Helper()
	origDeps := handoffDeliveryDepsFn
	origSend := sendPromptKeysVerified
	t.Cleanup(func() {
		handoffDeliveryDepsFn = origDeps
		sendPromptKeysVerified = origSend
	})
	handoffDeliveryDepsFn = func() handoffdelivery.Deps {
		deps := handoffdelivery.DefaultDeps()
		deps.CurrentOwner = func(string) (coordlock.Owner, bool) {
			t.Error("graceful route must not poll the lock owner outside the barrier")
			return coordlock.Owner{}, false
		}
		return deps
	}
	sendPromptKeysVerified = func(session, prompt string) (bool, error) {
		t.Errorf("graceful route must not direct-send (got send to %q)", session)
		return true, nil
	}
}

func TestResume_Case3_GracefulRoute_SwapsViaBarrier(t *testing.T) {
	requireFakeTmux(t)
	setupFleetHome(t)

	oldRec, newRec, req, qp := seedGracefulRouteCase3(t)
	forbidSeparateDelivery(t)

	winner := agent.New(agent.NewID())
	winner.TaskID = oldRec.TaskID
	winner.Project = oldRec.Project
	winner.Cwd = oldRec.Cwd
	winner.Command = oldRec.Command
	winner.TmuxSession = tmux.SessionName(winner.ID)
	winner.SpawnedAt = time.Now().UTC()
	winner.LastActivityTS = winner.SpawnedAt
	if err := winner.Write(); err != nil {
		t.Fatalf("write winner: %v", err)
	}

	origElig := gracefulSwapEligibleFn
	origSwap := gracefulCoordSwapFn
	t.Cleanup(func() {
		gracefulSwapEligibleFn = origElig
		gracefulCoordSwapFn = origSwap
	})
	gracefulSwapEligibleFn = func(rec *agent.Record, autoResume bool) bool {
		if rec == nil || rec.ID != oldRec.ID {
			t.Errorf("eligibility consulted for %+v, want old coord %s", rec, oldRec.ID)
		}
		if !autoResume {
			t.Error("eligibility consulted with autoResume=false on an auto-resume handoff")
		}
		return true
	}
	swapCalls := 0
	gracefulCoordSwapFn = func(o, n *agent.Record, docPath string, grace time.Duration,
		deliver func() (*agent.Record, error), stderr io.Writer) (*agent.Record, error) {
		swapCalls++
		if o.ID != oldRec.ID || n.ID != newRec.ID {
			t.Errorf("graceful swap got old=%s new=%s, want %s/%s", o.ID, n.ID, oldRec.ID, newRec.ID)
		}
		if docPath != req.HandoffDoc {
			t.Errorf("graceful swap doc = %q, want %q", docPath, req.HandoffDoc)
		}
		if deliver == nil {
			t.Error("graceful swap got a nil deliver closure")
		}
		// Simulate the barrier completion: AtomicCoordSwap retired OLD and
		// the doc landed on the lock WINNER.
		if err := o.ArchiveWithHandoff(n.ID); err != nil {
			t.Fatalf("simulated retire: %v", err)
		}
		return winner, nil
	}

	out := &bytes.Buffer{}
	if err := Resume(req, qp, 1, out, out); err != nil {
		t.Fatalf("Resume should complete via the graceful barrier route: %v\n%s", err, out.String())
	}
	if swapCalls != 1 {
		t.Fatalf("graceful swap ran %d times, want 1", swapCalls)
	}
	// The archive follows the WINNER (retargeted after barrier delivery).
	arc, err := agent.LoadArchive(oldRec.ID)
	if err != nil {
		t.Fatalf("LoadArchive(%s): %v", oldRec.ID, err)
	}
	if arc.SuccessorID != winner.ID {
		t.Fatalf("archive successor = %q, want lock winner %q", arc.SuccessorID, winner.ID)
	}
	// The superseded queued replacement is dropped; the queue is consumed.
	if _, lerr := agent.Load(newRec.ID); !errors.Is(lerr, state.ErrNotFound) {
		t.Fatalf("superseded replacement %s should be dropped, load err=%v", newRec.ID, lerr)
	}
	if _, statErr := os.Stat(qp); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("queue file should be consumed, stat err=%v", statErr)
	}
	if !strings.Contains(out.String(), winner.ID) {
		t.Fatalf("output does not name the winner %s:\n%s", winner.ID, out.String())
	}
}

func TestResume_Case3_GracefulRouteError_PreservesQueue(t *testing.T) {
	requireFakeTmux(t)
	setupFleetHome(t)

	oldRec, newRec, req, qp := seedGracefulRouteCase3(t)
	forbidSeparateDelivery(t)

	origElig := gracefulSwapEligibleFn
	origSwap := gracefulCoordSwapFn
	t.Cleanup(func() {
		gracefulSwapEligibleFn = origElig
		gracefulCoordSwapFn = origSwap
	})
	gracefulSwapEligibleFn = func(*agent.Record, bool) bool { return true }
	wantErr := errors.New("winner never converged")
	gracefulCoordSwapFn = func(_, _ *agent.Record, _ string, _ time.Duration,
		_ func() (*agent.Record, error), _ io.Writer) (*agent.Record, error) {
		return nil, wantErr
	}

	out := &bytes.Buffer{}
	err := Resume(req, qp, 1, out, out)
	if !errors.Is(err, wantErr) {
		t.Fatalf("graceful route error not surfaced: %v", err)
	}
	// Queue stays PENDING for the retry; the journaled replacement record
	// survives so the recovery pass can reload it.
	if _, statErr := os.Stat(qp); statErr != nil {
		t.Fatalf("queue file must be preserved on a graceful-route failure: %v", statErr)
	}
	if _, lerr := agent.Load(newRec.ID); lerr != nil {
		t.Fatalf("journaled replacement record must survive the failure: %v", lerr)
	}
	_ = oldRec
}

func TestResume_Case3_GracefulNotEligible_BespokePathRuns(t *testing.T) {
	requireFakeTmux(t)
	setupFleetHome(t)

	oldRec, newRec, req, qp := seedGracefulRouteCase3(t)

	// Not eligible (e.g. dead-coord recovery: OLD no longer owns the lease)
	// -> the proven bespoke path: AtomicCoordSwap + owner-poll delivery.
	origElig := gracefulSwapEligibleFn
	origSwap := gracefulCoordSwapFn
	origDeps := handoffDeliveryDepsFn
	origTimeout := handoffDeliveryTimeout
	origPoll := handoffDeliveryPoll
	t.Cleanup(func() {
		gracefulSwapEligibleFn = origElig
		gracefulCoordSwapFn = origSwap
		handoffDeliveryDepsFn = origDeps
		handoffDeliveryTimeout = origTimeout
		handoffDeliveryPoll = origPoll
	})
	gracefulSwapEligibleFn = func(*agent.Record, bool) bool { return false }
	gracefulCoordSwapFn = func(_, _ *agent.Record, _ string, _ time.Duration,
		_ func() (*agent.Record, error), _ io.Writer) (*agent.Record, error) {
		t.Error("graceful swap must not run when not eligible")
		return nil, nil
	}
	handoffDeliveryTimeout = time.Second
	handoffDeliveryPoll = time.Millisecond
	var sentSession string
	handoffDeliveryDepsFn = func() handoffdelivery.Deps {
		deps := handoffdelivery.DefaultDeps()
		deps.CurrentOwner = func(string) (coordlock.Owner, bool) {
			return coordlock.Owner{AgentID: newRec.ID, PID: 9001, PidStart: 99}, true
		}
		deps.WaitReady = func(string) error { return nil }
		deps.SessionAlive = func(string) (bool, error) { return true, nil }
		deps.SendVerified = func(session, _ string) (bool, error) {
			sentSession = session
			return true, nil
		}
		deps.Sleep = func(time.Duration) {}
		return deps
	}

	out := &bytes.Buffer{}
	if err := Resume(req, qp, 1, out, out); err != nil {
		t.Fatalf("bespoke fallback should complete: %v\n%s", err, out.String())
	}
	if sentSession != newRec.TmuxSession {
		t.Fatalf("bespoke delivery went to %q, want %q", sentSession, newRec.TmuxSession)
	}
	if _, lerr := agent.Load(oldRec.ID); !errors.Is(lerr, state.ErrNotFound) {
		t.Fatalf("old coord should be archived by the bespoke path, load err=%v", lerr)
	}
}

// The drain/auto path's live-coord successor completes through the graceful
// route exactly when OLD is still the eligible lease owner and auto-resume is
// enabled. Dead-coord cold resume takes the bespoke tail, but still lease-wraps
// at spawn.Spawn.
func TestLiveCoordGracefulSpawn_DecidesGracefulRoute(t *testing.T) {
	setupFleetHome(t)
	oldRec := agent.New("oldcoord1")
	oldRec.Project = "rainier"

	origElig := gracefulSwapEligibleFn
	t.Cleanup(func() { gracefulSwapEligibleFn = origElig })

	consulted := 0
	gracefulSwapEligibleFn = func(rec *agent.Record, autoResume bool) bool {
		consulted++
		if rec.ID != oldRec.ID {
			t.Errorf("eligibility consulted for %s, want %s", rec.ID, oldRec.ID)
		}
		return autoResume // mirror autoResume so the table below exercises it
	}

	// D3: the wrap decision gates on the isCoordResume PARAMETER (the caller's
	// spawn.IsCoordSpawn result) + gracefulSwapEligibleFn — the coord-spawn
	// marker is gone, so a non-coord resume (isCoordResume=false) short-circuits
	// before consulting eligibility.
	if liveCoordGracefulSpawn(oldRec, false, true) {
		t.Error("worker resume must never take the graceful coord route")
	}
	if consulted != 0 {
		t.Error("eligibility consulted for a non-coord resume")
	}
	if !liveCoordGracefulSpawn(oldRec, true, true) {
		t.Error("live-coord auto-resume should take the graceful route")
	}
	if liveCoordGracefulSpawn(oldRec, true, false) {
		t.Error("auto-resume off must not take the graceful route")
	}
}

// codex PR3-completion iter-7 [P1]: on the GRACEFUL route the successor is
// lease-wrapped by construction, so ErrNoOwnerObserved means "standby has not
// acquired YET" — the legacy direct-send fallback would type into the
// coord-run supervisor pane and could falsely report success, consuming the
// durable queue with the doc undelivered. The lease-wrapped delivery path must
// propagate the error instead.
func TestDeliverResumePrompt_LeaseWrappedGraceful_NoOwner_NoFallback(t *testing.T) {
	setupFleetHome(t)

	rec := agent.New("standby02")
	rec.Project = "rainier"
	rec.TmuxSession = "fleet-standby02"

	origDeps := handoffDeliveryDepsFn
	origTimeout := handoffDeliveryTimeout
	origPoll := handoffDeliveryPoll
	origSend := sendPromptKeysVerified
	t.Cleanup(func() {
		handoffDeliveryDepsFn = origDeps
		handoffDeliveryTimeout = origTimeout
		handoffDeliveryPoll = origPoll
		sendPromptKeysVerified = origSend
	})
	handoffDeliveryTimeout = 100 * time.Millisecond
	handoffDeliveryPoll = time.Millisecond
	handoffDeliveryDepsFn = func() handoffdelivery.Deps {
		deps := handoffdelivery.DefaultDeps()
		deps.CurrentOwner = func(string) (coordlock.Owner, bool) {
			return coordlock.Owner{}, false
		}
		deps.Sleep = func(time.Duration) {}
		return deps
	}
	sendPromptKeysVerified = func(session, _ string) (bool, error) {
		t.Errorf("lease-wrapped delivery direct-sent to %q; must stay pending", session)
		return true, nil
	}

	out := &bytes.Buffer{}
	delivered, err := deliverResumePrompt(rec.Project, true, true, rec, "/tmp/handoff.md", out, out)
	if !errors.Is(err, handoffdelivery.ErrNoOwnerObserved) {
		t.Fatalf("want ErrNoOwnerObserved propagated (queue stays pending), got %v", err)
	}
	if delivered != nil {
		t.Fatalf("delivered = %+v on the no-owner path, want nil", delivered)
	}
}
