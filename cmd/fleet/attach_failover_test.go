// Tier 3 PROJECT RECOVERY tests for fleet attach (attach-failover-59db).
//
// Covers F1–F18 from docs/TASK-PLAN-attach-failover.md. All Tier 3
// branches share the same invariant: attach NEVER returns non-zero
// in any recoverable case. Exit 64 (CLI usage error) is reserved for
// the non-tty / no-derivation case (operator MUST pass --project).
// Exit non-zero otherwise is a true system failure (tmux missing,
// dispatch failed, FS broken) — also tested.
//
// Test seam discipline: runAttachWith takes a struct (AttachOpts)
// holding every external dep — tmux, dispatch CLI shell-out, gc CLI
// shell-out, cwd derivation, stdin/stderr. Real tmux is never
// invoked; the production wiring lives in newAttachCmd's RunE.

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/coordlock"
	"github.com/edisonshen/fleet/internal/coordreconcile"
	"github.com/edisonshen/fleet/internal/projectlookup"
)

// --- test fixtures + helpers ---

// failoverSetup is the shared state for an attach-failover test:
// FLEET_HOME tempdir, attach call recorder, dispatch/gc shell-out
// recorders, session-alive map for tmux probes.
type failoverSetup struct {
	tmp            string
	attachedTo     string
	attachCalls    int
	dispatched     []string // project names passed to coord-spawn
	gcCalls        []string // project names passed to gc --kinds=orphan-agents
	killedSessions []string // tmux sessions passed to tmux.Kill (Path C')
	aliveSessions  map[string]bool
	newSpawnID     string // ID minted by the stub coord-spawn
}

// newFailoverSetup wires the env + a fresh setup and prepares the
// agents dir so agent.Record.Write succeeds.
func newFailoverSetup(t *testing.T) *failoverSetup {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	if err := os.MkdirAll(filepath.Join(tmp, "agents", "archive"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &failoverSetup{
		tmp:           tmp,
		aliveSessions: map[string]bool{},
		newSpawnID:    "newcoord", // 8-char default for the picker / spawn path
	}
}

// installStubs replaces the package-level seams used by Tier 3
// branches and registers t.Cleanup to restore them.
func (s *failoverSetup) installStubs(t *testing.T) {
	t.Helper()
	prevAttach := attachFnVar
	attachFnVar = func(session string) error {
		s.attachedTo = session
		s.attachCalls++
		return nil
	}
	prevDispatch := coordSpawnFnVar
	coordSpawnFnVar = func(project string) (string, error) {
		s.dispatched = append(s.dispatched, project)
		// Mint a record + alive session so the post-spawn attach
		// resolves cleanly.
		r := agent.New(s.newSpawnID)
		r.Project = project
		r.TaskID = projectlookup.CoordTaskID(project)
		r.TmuxSession = "fleet-" + s.newSpawnID
		if err := r.Write(); err != nil {
			return "", err
		}
		s.aliveSessions["fleet-"+s.newSpawnID] = true
		return s.newSpawnID, nil
	}
	// codex iter-14 P2: parallel stub for the struct-returning seam.
	// Tests that don't override get the happy-path "prompt delivered"
	// result; tests that need to simulate the dispatch warning override
	// coordSpawnFullFnVar directly.
	prevDispatchFull := coordSpawnFullFnVar
	coordSpawnFullFnVar = func(project string) (coordSpawnResult, error) {
		id, err := coordSpawnFnVar(project)
		if err != nil {
			return coordSpawnResult{}, err
		}
		return coordSpawnResult{ID: id, PromptDelivered: true}, nil
	}
	// Path C' (orphan tmux) uses a direct tmux kill — record those too
	// so tests can distinguish single-session kill from gc invocation.
	// Path C (stale record) no longer calls gc at all (codex iter-8 P1
	// — dispatch's findRecoveryCandidate needs the live record to
	// inherit cwd/engine from; pre-archiving severs that). gcCalls left
	// in the struct so a regression that re-introduces a gc shell-out is
	// caught by F6/F7 assertions, but no production seam writes to it.
	prevKill := killTmuxSessionFnVar
	killTmuxSessionFnVar = func(session string) error {
		s.killedSessions = append(s.killedSessions, session)
		// Make the killed session disappear from the alive map so
		// subsequent probes see it gone.
		delete(s.aliveSessions, session)
		return nil
	}
	prevSessionAlive := sessionAliveFnVar
	sessionAliveFnVar = func(session string) bool { return s.aliveSessions[session] }
	prevSessionProbe := sessionProbeFnVar
	sessionProbeFnVar = func(session string) (bool, error) {
		return s.aliveSessions[session], nil
	}
	prevListSessions := listSessionsFnVar
	listSessionsFnVar = func() ([]string, error) {
		var out []string
		for s := range s.aliveSessions {
			out = append(out, s)
		}
		return out, nil
	}
	prevTmuxAvailable := tmuxAvailableFnVar
	tmuxAvailableFnVar = func() error { return nil }
	prevResolve := attachResolveFnVar
	attachResolveFnVar = func(project, agentID string) (coordreconcile.Verdict, error) {
		records, _, _ := agent.ListStrict()
		for _, r := range records {
			if r == nil || r.Project != project || r.TaskID != projectlookup.CoordTaskID(project) {
				continue
			}
			session := r.TmuxSession
			if session == "" {
				session = "fleet-" + r.ID
			}
			if s.aliveSessions[session] {
				return coordreconcile.Verdict{
					Decision: coordreconcile.Attach,
					Owner:    coordlock.Owner{AgentID: r.ID, PID: 1},
					Reason:   "test live owner",
				}, nil
			}
		}
		if agentID == "" {
			return coordreconcile.Verdict{Decision: coordreconcile.Wait, Reason: "test read-only wait"}, nil
		}
		return coordreconcile.Verdict{Decision: coordreconcile.Spawn, Reason: "test spawn"}, nil
	}
	prevCurrentOwner := attachCurrentOwnerFnVar
	attachCurrentOwnerFnVar = func(project string) (coordlock.Owner, bool) {
		verdict, _ := attachResolveFnVar(project, "")
		if verdict.Decision != coordreconcile.Attach {
			return coordlock.Owner{}, false
		}
		return verdict.Owner, true
	}
	prevNewID := attachNewAgentIDFnVar
	attachNewAgentIDFnVar = func() string { return "claimtok" }
	// projectlookup has its own session-probe seams (different
	// package); the failover paths route through them, so stub both
	// the cmd/fleet seam (for direct tmux.Attach calls) and the
	// projectlookup seams (for FindLiveCoord / FindCoordByLockBody /
	// StaleCoordRecord / OrphanTmuxForProject lookups).
	restorePL := projectlookup.SetTestStubs(
		func(session string) bool { return s.aliveSessions[session] },
		func(session string) (bool, error) { return s.aliveSessions[session], nil },
		func() ([]string, error) {
			var out []string
			for s := range s.aliveSessions {
				out = append(out, s)
			}
			return out, nil
		},
	)
	t.Cleanup(func() {
		attachFnVar = prevAttach
		coordSpawnFnVar = prevDispatch
		coordSpawnFullFnVar = prevDispatchFull
		killTmuxSessionFnVar = prevKill
		sessionAliveFnVar = prevSessionAlive
		sessionProbeFnVar = prevSessionProbe
		listSessionsFnVar = prevListSessions
		tmuxAvailableFnVar = prevTmuxAvailable
		attachResolveFnVar = prevResolve
		attachCurrentOwnerFnVar = prevCurrentOwner
		attachNewAgentIDFnVar = prevNewID
		restorePL()
	})
}

func (s *failoverSetup) addLiveCoord(t *testing.T, project, id string) {
	t.Helper()
	r := agent.New(id)
	r.Project = project
	r.TaskID = projectlookup.CoordTaskID(project)
	r.TmuxSession = "fleet-" + id
	if err := r.Write(); err != nil {
		t.Fatal(err)
	}
	s.aliveSessions["fleet-"+id] = true
}

func (s *failoverSetup) addRecordOnly(t *testing.T, project, id string) {
	t.Helper()
	r := agent.New(id)
	r.Project = project
	r.TaskID = projectlookup.CoordTaskID(project)
	r.TmuxSession = "fleet-" + id
	if err := r.Write(); err != nil {
		t.Fatal(err)
	}
	// Intentionally NOT in aliveSessions — record alive + tmux dead.
}

func (s *failoverSetup) addOrphanTmux(_ *testing.T, id string) {
	s.aliveSessions["fleet-"+id] = true
}

func (s *failoverSetup) addArchivedRecord(t *testing.T, id, project, cause, successor string) {
	t.Helper()
	r := agent.New(id)
	r.Project = project
	// Tag as the project's coord lineage. OrphanTmuxForProject's coord-only
	// reap guard (codex P2) requires task_id == coord-<project> before it
	// will return an orphan for Path C' to kill. Handoff-chain tests
	// (F1-F3) read SuccessorID/ArchivedCause, not TaskID, so this is inert
	// for them.
	r.TaskID = projectlookup.CoordTaskID(project)
	r.ArchivedCause = cause
	if successor != "" {
		r.SuccessorID = successor
	}
	data, _ := json.MarshalIndent(r, "", "  ")
	if err := os.WriteFile(filepath.Join(s.tmp, "agents", "archive", id+".json"),
		data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func (s *failoverSetup) addProjectDir(t *testing.T, project string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(s.tmp, "projects", project), 0o755); err != nil {
		t.Fatal(err)
	}
}

// addEmptySessionCoord writes a coord-<project>-tagged record with an
// EMPTY TmuxSession (the BUG #1 case): unprobeable, so StaleCoordRecord
// must return (nil,false) and Tier 3 falls through to Path D.
func (s *failoverSetup) addEmptySessionCoord(t *testing.T, project, id string) {
	t.Helper()
	r := agent.New(id)
	r.Project = project
	r.TaskID = projectlookup.CoordTaskID(project)
	r.TmuxSession = "" // empty — the BUG #1 signal
	if err := r.Write(); err != nil {
		t.Fatal(err)
	}
}

// addStaleLockBodyCoord writes an UNTAGGED record (task_id ≠
// coord-<project>) whose ID is the project's coordinator.lock body, with
// a DEFINITIVELY-dead tmux session (not in aliveSessions). This is the
// Path B case: StaleCoordRecord skips it (untagged), StaleLockBodyCoord
// catches it, and attach must NOT kill the already-dead session.
func (s *failoverSetup) addStaleLockBodyCoord(t *testing.T, project, id string) {
	t.Helper()
	r := agent.New(id)
	r.Project = project
	r.TaskID = "manual-spawn" // NOT coord-<project>
	r.TmuxSession = "fleet-" + id
	if err := r.Write(); err != nil {
		t.Fatal(err)
	}
	// Intentionally NOT in aliveSessions — session proven dead.
	lockDir := filepath.Join(s.tmp, "projects", project, ".locks")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lockDir, "coordinator.lock"), []byte(id+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// installVetoStub makes coordSpawnFullFnVar return errCoordSpawnVetoed
// (simulating dispatch exit 75) and counts the spawn attempts, so tests
// can assert exactly ONE spawn per invocation. Restores on cleanup.
func (s *failoverSetup) installVetoStub(t *testing.T) *int {
	t.Helper()
	calls := 0
	prev := coordSpawnFullFnVar
	coordSpawnFullFnVar = func(project string) (coordSpawnResult, error) {
		calls++
		s.dispatched = append(s.dispatched, project)
		return coordSpawnResult{}, errCoordSpawnVetoed
	}
	t.Cleanup(func() { coordSpawnFullFnVar = prev })
	return &calls
}

func (s *failoverSetup) run(t *testing.T, token string, opts AttachOpts) (bytes.Buffer, error) {
	t.Helper()
	var stderr bytes.Buffer
	opts.Stderr = &stderr
	if opts.Stdin == nil {
		opts.Stdin = strings.NewReader("")
	}
	err := runAttachFailover(token, opts)
	return stderr, err
}

// --- F1: cycle in chain → Tier 3 ---

func TestF1_CycleInChain_FailsOverToProjectRecovery(t *testing.T) {
	s := newFailoverSetup(t)
	s.installStubs(t)
	s.addArchivedRecord(t, "aaaaaaaa", "projects-fleet", agent.ArchivedCauseHandoff, "bbbbbbbb")
	s.addArchivedRecord(t, "bbbbbbbb", "projects-fleet", agent.ArchivedCauseHandoff, "aaaaaaaa")
	s.addLiveCoord(t, "projects-fleet", "xxxxxxxx")
	stderr, err := s.run(t, "aaaaaaaa", AttachOpts{})
	if err != nil {
		t.Fatalf("F1: expected no error, got %v", err)
	}
	got := stderr.String()
	if !strings.Contains(got, "cycle") {
		t.Errorf("F1 stderr missing cycle line: %q", got)
	}
	if !strings.Contains(got, "aaaaaaaa: attached to current coord xxxxxxxx for projects-fleet") {
		t.Errorf("F1 stderr missing failover-attached line: %q", got)
	}
	if s.attachedTo != "fleet-xxxxxxxx" {
		t.Errorf("F1: attached to %q want fleet-xxxxxxxx", s.attachedTo)
	}
}

// --- F2: chain breaks mid-walk → Tier 3 ---

func TestF2_ChainBreaksMidWalk_FailsOverToProjectRecovery(t *testing.T) {
	s := newFailoverSetup(t)
	s.installStubs(t)
	s.addArchivedRecord(t, "aaaaaaaa", "projects-fleet", agent.ArchivedCauseHandoff, "bbbbbbbb")
	// bbbbbbbb does NOT exist anywhere
	s.addLiveCoord(t, "projects-fleet", "xxxxxxxx")
	stderr, err := s.run(t, "aaaaaaaa", AttachOpts{})
	if err != nil {
		t.Fatalf("F2: expected no error, got %v", err)
	}
	got := stderr.String()
	if !strings.Contains(got, "aaaaaaaa handoff chain broken at bbbbbbbb") {
		t.Errorf("F2 stderr missing chain-broken line: %q", got)
	}
	if !strings.Contains(got, "aaaaaaaa: attached to current coord xxxxxxxx for projects-fleet") {
		t.Errorf("F2 stderr missing attached line: %q", got)
	}
}

// --- F3: non-handoff archive (cause=kill) → Tier 3 ---

func TestF3_NonHandoffArchive_FailsOverToProjectRecovery(t *testing.T) {
	s := newFailoverSetup(t)
	s.installStubs(t)
	s.addArchivedRecord(t, "killedaa", "projects-fleet", agent.ArchivedCauseKill, "")
	s.addLiveCoord(t, "projects-fleet", "xxxxxxxx")
	stderr, err := s.run(t, "killedaa", AttachOpts{})
	if err != nil {
		t.Fatalf("F3: expected no error, got %v", err)
	}
	got := stderr.String()
	if !strings.Contains(got, "killedaa archived (cause=kill); failing over to project recovery") {
		t.Errorf("F3 stderr missing failover line: %q", got)
	}
	if !strings.Contains(got, "killedaa: attached to current coord xxxxxxxx for projects-fleet") {
		t.Errorf("F3 stderr missing attached line: %q", got)
	}
}

// --- F4: unknown token, cwd derivable → Tier 3 ---

func TestF4_UnknownToken_CwdDerivable_FailsOver(t *testing.T) {
	s := newFailoverSetup(t)
	s.installStubs(t)
	s.addProjectDir(t, "fleet")
	s.addLiveCoord(t, "fleet", "e3836016")
	stderr, err := s.run(t, "missingg", AttachOpts{CwdBasename: "fleet"})
	if err != nil {
		t.Fatalf("F4: expected no error, got %v", err)
	}
	got := stderr.String()
	if !strings.Contains(got, "missingg: unknown identifier; deriving project from cwd basename → fleet") {
		t.Errorf("F4 stderr missing derivation line: %q", got)
	}
	if !strings.Contains(got, "missingg: attached to current coord e3836016 for fleet") {
		t.Errorf("F4 stderr missing attached line: %q", got)
	}
}

// --- F5: --project flag, live coord ---

func TestF5_ProjectFlag_LiveCoord_Attaches(t *testing.T) {
	s := newFailoverSetup(t)
	s.installStubs(t)
	s.addProjectDir(t, "projects-fleet")
	s.addLiveCoord(t, "projects-fleet", "xxxxxxxx")
	stderr, err := s.run(t, "foo", AttachOpts{Project: "projects-fleet"})
	if err != nil {
		t.Fatalf("F5: expected no error, got %v", err)
	}
	if !strings.Contains(stderr.String(), "foo: attached to current coord xxxxxxxx for projects-fleet") {
		t.Errorf("F5 stderr: %q", stderr.String())
	}
	if s.attachedTo != "fleet-xxxxxxxx" {
		t.Errorf("F5: attached to %q want fleet-xxxxxxxx", s.attachedTo)
	}
	if len(s.dispatched) != 0 {
		t.Errorf("F5: must NOT dispatch on live-coord branch; got %v", s.dispatched)
	}
	if len(s.gcCalls) != 0 {
		t.Errorf("F5: must NOT gc on live-coord branch; got %v", s.gcCalls)
	}
}

// --- F6: --project, stale coord (record alive + tmux missing) ---

func TestF6_ProjectFlag_StaleCoord_ReapsAndRespawns(t *testing.T) {
	s := newFailoverSetup(t)
	s.installStubs(t)
	s.addProjectDir(t, "projects-fleet")
	s.addRecordOnly(t, "projects-fleet", "staleeee") // record alive + no tmux
	stderr, err := s.run(t, "foo", AttachOpts{Project: "projects-fleet"})
	if err != nil {
		t.Fatalf("F6: expected no error, got %v", err)
	}
	got := stderr.String()
	// Codex iter-8 P1: Path C delegates the archive to dispatch's
	// recovery flow (findRecoveryCandidate). The verb changes from
	// "reaped stale" to "recovered" to reflect that the dead record
	// stays on disk for dispatch to inherit cwd/engine from. NO gc
	// invocation here — dispatch archives it after writing the synth
	// handoff doc.
	if !strings.Contains(got, "foo: lease resolved spawn for projects-fleet") ||
		!strings.Contains(got, "spawned newcoord; attaching") {
		t.Errorf("F6 stderr: %q", got)
	}
	if len(s.gcCalls) != 0 {
		t.Errorf("F6: Path C must NOT pre-archive (dispatch needs the live record for recovery); got gc calls %v", s.gcCalls)
	}
	if len(s.dispatched) != 1 || s.dispatched[0] != "projects-fleet" {
		t.Errorf("F6: expected one coord-spawn for projects-fleet, got %v", s.dispatched)
	}
	if s.attachedTo != "fleet-newcoord" {
		t.Errorf("F6: attached to %q want fleet-newcoord", s.attachedTo)
	}
}

// --- F7: --project, orphan tmux (no record, lingering session) ---

func TestF7_ProjectFlag_OrphanTmux_ReapsAndRespawns(t *testing.T) {
	s := newFailoverSetup(t)
	s.installStubs(t)
	s.addProjectDir(t, "projects-fleet")
	s.addOrphanTmux(t, "deadbeef") // tmux only, no live record
	// Codex iter-4 P1: OrphanTmuxForProject now requires the archive to
	// tie the orphan to projectName. Without this, the cross-project
	// guard rejects the orphan and we'd fall through to spawn-fresh
	// (Path D) instead of the reap-and-respawn (Path C) F7 is meant to
	// exercise.
	s.addArchivedRecord(t, "deadbeef", "projects-fleet", agent.ArchivedCauseKill, "")
	stderr, err := s.run(t, "foo", AttachOpts{Project: "projects-fleet"})
	if err != nil {
		t.Fatalf("F7: expected no error, got %v", err)
	}
	got := stderr.String()
	if !strings.Contains(got, "foo: reaped stale deadbeef (live orphan session); spawned newcoord for projects-fleet; attaching") {
		t.Errorf("F7 stderr: %q", got)
	}
	// Codex iter-5 P1: Path C' uses targeted tmux.Kill, not the
	// host-wide gc --aggressive. Assert the kill landed on the specific
	// fleet-<id> session and NO gc was invoked (the old path's blast
	// radius would have killed cross-project orphans).
	if len(s.killedSessions) != 1 || s.killedSessions[0] != "fleet-deadbeef" {
		t.Errorf("F7: expected one tmux.Kill on fleet-deadbeef, got %v", s.killedSessions)
	}
	if len(s.gcCalls) != 0 {
		t.Errorf("F7: orphan-tmux path must NOT invoke gc (host-wide blast radius); got %v", s.gcCalls)
	}
	if len(s.dispatched) != 1 || s.dispatched[0] != "projects-fleet" {
		t.Errorf("F7: expected one coord-spawn, got %v", s.dispatched)
	}
}

// TestBuildCoordSpawnArgs_ShapeAndTaskID — codex review iter-4 P1
// regression. The dispatch CLI is `dispatch <task-id> [flags]` with
// cobra.ExactArgs(1); a missing positional fails with a usage error
// and Tier 3 recovery breaks. Pin the argv shape so any future
// refactor that drops the task-id is caught immediately.
func TestBuildCoordSpawnArgs_ShapeAndTaskID(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	// Codex iter-6 P1: the spawned coord MUST receive the bootstrap
	// prompt (so /coordinator runs) AND be forced to engine claude-code
	// (so DISPATCH blocks work). The full argv shape is asserted below.
	wantPrompt := projectlookup.CoordSpawnPrompt("projects-fleet")
	// DESIGN-coord-repo-binding-from-project.md PR3: buildCoordSpawnArgs
	// now resolves via the shared resolver. No meta.json + no worktrees =
	// unresolvable → REFUSE (no args, surfaced hint). cwd is no longer a
	// fallback, so the old "args without --cwd" shape is gone.
	_, err := buildCoordSpawnArgs("projects-fleet")
	if err == nil {
		t.Fatal("buildCoordSpawnArgs(no meta) must refuse an unresolvable project, got nil")
	}
	if !strings.Contains(err.Error(), "no usable checkout") {
		t.Errorf("buildCoordSpawnArgs(no meta) should surface the resolver hint; got %v", err)
	}
	// With a usable meta.json (real dir, non-git → bind on existence):
	// --cwd <resolved repo> appended after the prompt/engine.
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "projects", "projects-fleet"), 0o755); err != nil {
		t.Fatal(err)
	}
	metaPath := filepath.Join(tmp, "projects", "projects-fleet", "meta.json")
	if err := os.WriteFile(metaPath, []byte(`{"schema":"v1","is_git":false,"repo_path":"`+repo+`","added_at":"2026-01-01T00:00:00Z"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := buildCoordSpawnArgs("projects-fleet")
	if err != nil {
		t.Fatalf("buildCoordSpawnArgs(with meta) err: %v", err)
	}
	want := []string{
		"dispatch", "coord-projects-fleet",
		"--coord-spawn", "--project", "projects-fleet",
		"--prompt", wantPrompt,
		"--engine", "claude-code",
		"--cwd", repo,
	}
	if !stringSlicesEqual(got, want) {
		t.Errorf("buildCoordSpawnArgs(with meta): got %v want %v", got, want)
	}
	// Sanity-check the prompt content — it MUST contain "/coordinator"
	// (the operator-visible cue that the agent will run the supervisor
	// loop) and the project name (so the agent knows which tasks.md to
	// own). Codex iter-6 P1.
	if !strings.Contains(wantPrompt, "/coordinator") {
		t.Errorf("CoordSpawnPrompt missing /coordinator invocation: %q", wantPrompt)
	}
	if !strings.Contains(wantPrompt, "projects-fleet") {
		t.Errorf("CoordSpawnPrompt missing project name: %q", wantPrompt)
	}
}

// TestParseSpawnedAgentID_RecoveryFlowAvoidsPredecessorID — codex
// review iter-9 P1. dispatch's dead-coord recovery prints
// "recovering dead coord <oldID> for project <p>..." BEFORE the
// canonical "agent <newID> spawned" line. The first-hex-token scan
// would have returned the dead predecessor ID and attach would have
// probed the dead session, defeating the iter-2 P2 spawn probe gate.
//
// parseSpawnedAgentID must specifically match the `agent <id> spawned`
// shape, skipping any earlier hex tokens.
func TestParseSpawnedAgentID_RecoveryFlowAvoidsPredecessorID(t *testing.T) {
	out := `recovering dead coord deadbeef for project projects-fleet: synth handoff written to /tmp/handoff.md
agent c0ffee01 spawned
`
	got := parseSpawnedAgentID(out)
	if got != "c0ffee01" {
		t.Errorf("parseSpawnedAgentID: got %q want c0ffee01 (must skip the predecessor ID `deadbeef` on the recovery line)", got)
	}
}

// TestParseSpawnedAgentID_HappyPath — fresh-spawn output (no recovery
// line). The parser must still find the canonical spawn line.
func TestParseSpawnedAgentID_HappyPath(t *testing.T) {
	out := "agent abcd1234 spawned\n"
	got := parseSpawnedAgentID(out)
	if got != "abcd1234" {
		t.Errorf("parseSpawnedAgentID: got %q want abcd1234", got)
	}
}

// TestParseSpawnedAgentID_NoMatch — when dispatch output doesn't carry
// the canonical line at all, the parser returns "" so shellCoordSpawn
// surfaces a clear "could not parse" error instead of a silent bogus id.
func TestParseSpawnedAgentID_NoMatch(t *testing.T) {
	out := "some other unrelated text 12345678 here\n"
	got := parseSpawnedAgentID(out)
	if got != "" {
		t.Errorf("parseSpawnedAgentID: got %q want \"\" (no canonical line)", got)
	}
}

// TestParseSpawnedAgentID_RacingStandbyTakesLast pins codex iter-30 P1: when
// dead-coord recovery re-emits an authoritative `agent <winner> spawned` line
// after a racing standby won the coord lease, CLI attach must target the LAST
// such line (the lock winner), NOT the first (the losing spawned standby).
// Otherwise `fleet attach` probes/attaches the wrong/dead session — the same
// bug the TUI's dispatchAgentID last-match fix closes.
func TestParseSpawnedAgentID_RacingStandbyTakesLast(t *testing.T) {
	out := `agent 11111111 spawned
  task:    coord-demo
note: another standby (22222222) won the coord lease for project demo; resume prompt + coord marker delivered there
agent 22222222 spawned

attach with: fleet attach 22222222
`
	got := parseSpawnedAgentID(out)
	if got != "22222222" {
		t.Errorf("parseSpawnedAgentID: got %q want 22222222 (must take the LAST spawn line — the racing lock winner)", got)
	}
}

// TestBuildCoordSpawnArgs_MalformedMetaFailsClosed — codex review iter-7
// P2. A corrupt meta.json (invalid JSON) must surface a clear error
// rather than silently respawn the coord in the operator's shell cwd
// (which is almost certainly NOT the project's repo).
func TestBuildCoordSpawnArgs_MalformedMetaFailsClosed(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	if err := os.MkdirAll(filepath.Join(tmp, "projects", "projects-fleet"), 0o755); err != nil {
		t.Fatal(err)
	}
	metaPath := filepath.Join(tmp, "projects", "projects-fleet", "meta.json")
	if err := os.WriteFile(metaPath, []byte("{not-json"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := buildCoordSpawnArgs("projects-fleet")
	if err == nil {
		t.Fatal("expected error on malformed meta.json, got nil (would silently spawn in wrong cwd)")
	}
	if !strings.Contains(err.Error(), "meta.json") || !strings.Contains(err.Error(), "projects-fleet") {
		t.Errorf("err must name the bad meta path: %q", err.Error())
	}
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// An orphan tmux session whose archive ties it to project B must NOT
// be reaped by `fleet attach --project A`. Tier 3 falls through to
// Path D (spawn-fresh) instead. Without this guard, attach-failover
// could nuke unrelated sessions.
func TestF7b_CrossProjectOrphan_DoesNotReap(t *testing.T) {
	s := newFailoverSetup(t)
	s.installStubs(t)
	s.addProjectDir(t, "projects-fleet")
	// Orphan session belongs to a DIFFERENT project (per its archive).
	s.addOrphanTmux(t, "otherbbb")
	s.addArchivedRecord(t, "otherbbb", "other-project", agent.ArchivedCauseKill, "")
	stderr, err := s.run(t, "foo", AttachOpts{Project: "projects-fleet"})
	if err != nil {
		t.Fatalf("F7b: expected no error, got %v", err)
	}
	got := stderr.String()
	// Must take Path D, NOT Path C — diagnostic differs.
	if strings.Contains(got, "reaped stale otherbbb") {
		t.Errorf("F7b: must NOT reap cross-project orphan; stderr: %q", got)
	}
	if !strings.Contains(got, "lease resolved spawn for projects-fleet") {
		t.Errorf("F7b: must fall through to resolver spawn; stderr: %q", got)
	}
	// GC must NOT have fired against projects-fleet — there's nothing
	// for that project to reap (the orphan isn't ours).
	if len(s.gcCalls) != 0 {
		t.Errorf("F7b: must NOT run gc on cross-project orphan; got %v", s.gcCalls)
	}
}

// --- F8: --project, no coord at all ---

func TestF8_ProjectFlag_NoCoord_SpawnsFresh(t *testing.T) {
	s := newFailoverSetup(t)
	s.installStubs(t)
	s.addProjectDir(t, "projects-fleet")
	stderr, err := s.run(t, "foo", AttachOpts{Project: "projects-fleet"})
	if err != nil {
		t.Fatalf("F8: expected no error, got %v", err)
	}
	got := stderr.String()
	if !strings.Contains(got, "foo: lease resolved spawn for projects-fleet") ||
		!strings.Contains(got, "spawned newcoord; attaching") {
		t.Errorf("F8 stderr: %q", got)
	}
	if len(s.gcCalls) != 0 {
		t.Errorf("F8: must NOT gc when no coord; got %v", s.gcCalls)
	}
	if len(s.dispatched) != 1 {
		t.Errorf("F8: expected exactly one dispatch, got %v", s.dispatched)
	}
}

// --- F9: token IS a known project name ---

func TestF9_TokenMatchesProjectName_AttachesToLiveCoord(t *testing.T) {
	s := newFailoverSetup(t)
	s.installStubs(t)
	s.addProjectDir(t, "projects-fleet")
	s.addLiveCoord(t, "projects-fleet", "xxxxxxxx")
	stderr, err := s.run(t, "projects-fleet", AttachOpts{})
	if err != nil {
		t.Fatalf("F9: expected no error, got %v", err)
	}
	got := stderr.String()
	if !strings.Contains(got, "projects-fleet: token matched project name; resolving coord for projects-fleet via lease") ||
		!strings.Contains(got, "projects-fleet: attached to current coord xxxxxxxx for projects-fleet") {
		t.Errorf("F9 stderr: %q", got)
	}
}

// --- F10: cwd-basename fallback ---

func TestF10_CwdBasenameFallback_Attaches(t *testing.T) {
	s := newFailoverSetup(t)
	s.installStubs(t)
	s.addProjectDir(t, "fleet")
	s.addLiveCoord(t, "fleet", "e3836016")
	stderr, err := s.run(t, "mystery0", AttachOpts{CwdBasename: "fleet"})
	if err != nil {
		t.Fatalf("F10: expected no error, got %v", err)
	}
	got := stderr.String()
	// F10 + F4 share the cwd-derivation surface. Per task plan F10
	// the operator-visible cue is "deriving project from cwd basename
	// → <p>" — accept the F4-shape prefix ("unknown identifier; ") as
	// equivalent (it strictly adds context for unknown tokens).
	if !strings.Contains(got, "deriving project from cwd basename → fleet") {
		t.Errorf("F10 stderr missing derivation line: %q", got)
	}
	if !strings.Contains(got, "mystery0: attached to current coord e3836016 for fleet") {
		t.Errorf("F10 stderr missing attached line: %q", got)
	}
}

// --- F11: interactive picker, tty, choose by number ---

func TestF11_InteractivePicker_Tty_Choose(t *testing.T) {
	s := newFailoverSetup(t)
	s.installStubs(t)
	s.addProjectDir(t, "fleet")
	s.addProjectDir(t, "projects-fleet")
	s.addLiveCoord(t, "fleet", "ffffffff")
	stdin := strings.NewReader("1\n") // pick first project (alpha sort: "fleet")
	var stdout bytes.Buffer
	stderr, err := s.run(t, "unknownt", AttachOpts{IsTty: true, Stdin: stdin, Stdout: &stdout})
	if err != nil {
		t.Fatalf("F11: expected no error, got %v", err)
	}
	if !strings.Contains(stdout.String(), "[1] fleet") {
		t.Errorf("F11 stdout missing picker option: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "[2] projects-fleet") {
		t.Errorf("F11 stdout missing picker option 2: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "unknownt: attached to current coord ffffffff for fleet") {
		t.Errorf("F11 stderr: %q", stderr.String())
	}
}

// --- F12: non-tty → CLI usage error (exit 64) ---

func TestF12_NonTty_NoDerivation_ReturnsUsageError(t *testing.T) {
	s := newFailoverSetup(t)
	s.installStubs(t)
	s.addProjectDir(t, "fleet")
	s.addProjectDir(t, "projects-fleet")
	stderr, err := s.run(t, "foo", AttachOpts{IsTty: false})
	if err == nil {
		t.Fatal("F12: expected usage error, got nil")
	}
	var ue *UsageError
	if !errors.As(err, &ue) {
		t.Errorf("F12: expected *UsageError, got %T %v", err, err)
	}
	if !strings.Contains(err.Error(), "foo: cannot derive project (non-interactive)") {
		t.Errorf("F12 err: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "fleet, projects-fleet") {
		t.Errorf("F12 err: must list known projects: %q", err.Error())
	}
	// Cross-check ExitCode() conventions: usage error exit code 64.
	if ec := ExitCodeFor(err); ec != 64 {
		t.Errorf("F12: expected exit 64 (usage error), got %d", ec)
	}
	_ = stderr
}

// --- F13: archived record wins over --project flag ---

func TestF13_ArchivedRecordWinsOverFlag(t *testing.T) {
	s := newFailoverSetup(t)
	s.installStubs(t)
	s.addProjectDir(t, "projects-fleet")
	s.addProjectDir(t, "other")
	s.addArchivedRecord(t, "aaaaaaaa", "projects-fleet", agent.ArchivedCauseKill, "")
	s.addLiveCoord(t, "projects-fleet", "xxxxxxxx")
	stderr, err := s.run(t, "aaaaaaaa", AttachOpts{Project: "other"})
	if err != nil {
		t.Fatalf("F13: %v", err)
	}
	got := stderr.String()
	if !strings.Contains(got, "for projects-fleet") {
		t.Errorf("F13: archived rec should win over flag; stderr: %q", got)
	}
	if strings.Contains(got, "for other") {
		t.Errorf("F13: must NOT use flag when archived rec has project; stderr: %q", got)
	}
}

// --- F14: flag wins when no archive ---

func TestF14_FlagWinsWhenNoArchive(t *testing.T) {
	s := newFailoverSetup(t)
	s.installStubs(t)
	s.addProjectDir(t, "projects-fleet")
	s.addLiveCoord(t, "projects-fleet", "xxxxxxxx")
	stderr, err := s.run(t, "missingg", AttachOpts{Project: "projects-fleet"})
	if err != nil {
		t.Fatalf("F14: %v", err)
	}
	if !strings.Contains(stderr.String(), "for projects-fleet") {
		t.Errorf("F14 stderr: %q", stderr.String())
	}
}

// --- F15: token-as-project wins over cwd ---

func TestF15_TokenAsProjectWinsOverCwd(t *testing.T) {
	s := newFailoverSetup(t)
	s.installStubs(t)
	s.addProjectDir(t, "fleet")
	s.addProjectDir(t, "projects-fleet")
	s.addLiveCoord(t, "projects-fleet", "xxxxxxxx")
	stderr, err := s.run(t, "projects-fleet", AttachOpts{CwdBasename: "fleet"})
	if err != nil {
		t.Fatalf("F15: %v", err)
	}
	got := stderr.String()
	if !strings.Contains(got, "for projects-fleet") {
		t.Errorf("F15: token-as-project must win over cwd; stderr: %q", got)
	}
}

// --- F16: cwd wins when none of the above ---

func TestF16_CwdWinsWhenNothingElse(t *testing.T) {
	s := newFailoverSetup(t)
	s.installStubs(t)
	s.addProjectDir(t, "fleet")
	s.addLiveCoord(t, "fleet", "ffffffff")
	stderr, err := s.run(t, "mystery0", AttachOpts{CwdBasename: "fleet"})
	if err != nil {
		t.Fatalf("F16: %v", err)
	}
	if !strings.Contains(stderr.String(), "for fleet") {
		t.Errorf("F16 stderr: %q", stderr.String())
	}
}

// --- F19 (codex review iter-1 P1): stale live record → Tier 3 ---
//
// Token resolves to a live agent.Record on disk (Tier 1 hits), but its
// tmux session is DEFINITIVELY dead (probe returns alive=false). Before
// the fix, runAttachFailover called tmux.Attach immediately, which
// returns ErrNoSession and the operator dead-ended. The never-exit
// guarantee requires Tier 3 PROJECT RECOVERY to take over.

func TestF19_StaleLiveRecord_FailsOverToProjectRecovery(t *testing.T) {
	s := newFailoverSetup(t)
	s.installStubs(t)
	s.addProjectDir(t, "projects-fleet")
	// Stale live record: agent.Record on disk, but no entry in
	// aliveSessions → session probe returns (false, nil) → definitively
	// dead. The record is tagged with project so Tier 3 can derive.
	r := agent.New("staleabc")
	r.Project = "projects-fleet"
	r.TaskID = projectlookup.CoordTaskID("projects-fleet")
	r.TmuxSession = "fleet-staleabc"
	if err := r.Write(); err != nil {
		t.Fatal(err)
	}
	// Live successor coord for the same project — Tier 3 attaches here.
	s.addLiveCoord(t, "projects-fleet", "xxxxxxxx")
	stderr, err := s.run(t, "staleabc", AttachOpts{})
	if err != nil {
		t.Fatalf("F19: expected no error (must fail over, not dead-end), got %v", err)
	}
	got := stderr.String()
	if !strings.Contains(got, "staleabc: attached to current coord xxxxxxxx for projects-fleet") {
		t.Errorf("F19 stderr missing Tier-3 attached line: %q", got)
	}
	if s.attachedTo != "fleet-xxxxxxxx" {
		t.Errorf("F19: attached to %q want fleet-xxxxxxxx (live successor)", s.attachedTo)
	}
}

// TestSystemFailure_CorruptArchiveRecord_DoesNotRecover — codex review
// iter-3 P2 #1. If the archive record for the token is malformed (parse
// error / FS read error) we must NOT fall through to Tier 3 — that path
// could spawn/reap a coord on top of the corruption and bury the real
// fault. The chain resolver returns a wrapped JSON-parse error in that
// case (NOT one of the recoverable sentinels), and runAttachFailover
// must surface SystemError with the exact next-step path so the operator
// fixes the record.
func TestSystemFailure_CorruptArchiveRecord_DoesNotRecover(t *testing.T) {
	s := newFailoverSetup(t)
	s.installStubs(t)
	// Write a malformed archive record: invalid JSON body.
	corruptID := "corruptx"
	path := filepath.Join(s.tmp, "agents", "archive", corruptID+".json")
	if err := os.WriteFile(path, []byte("{not-json"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Even with a derivable project + live coord, we must NOT recover —
	// the right call is to surface the corruption.
	s.addProjectDir(t, "projects-fleet")
	s.addLiveCoord(t, "projects-fleet", "xxxxxxxx")
	_, err := s.run(t, corruptID, AttachOpts{Project: "projects-fleet"})
	if err == nil {
		t.Fatal("expected SystemError on corrupt archive, got nil (Tier 3 must not mask record read errors)")
	}
	// Surface must name the file path so the operator can fix it.
	if !strings.Contains(err.Error(), "corruptx") || !strings.Contains(err.Error(), "agents") {
		t.Errorf("err must name the corrupt record path: %q", err.Error())
	}
	if ec := ExitCodeFor(err); ec != 70 {
		t.Errorf("ExitCodeFor(corrupt record): got %d want 70", ec)
	}
	// Spawn/gc must NOT have fired — the bug we're guarding against is
	// "Tier 3 hides the corruption by silently respawning."
	if len(s.dispatched) != 0 {
		t.Errorf("dispatch must not fire on corrupt record; got %v", s.dispatched)
	}
	if len(s.gcCalls) != 0 {
		t.Errorf("gc must not fire on corrupt record; got %v", s.gcCalls)
	}
}

// TestSystemFailure_CorruptLiveRecord_VetoesTier3 — codex review iter-5
// P2. Tier 3 PROJECT RECOVERY scans ~/.fleet/agents/ via agent.List, which
// silently skips unparseable records. If the skipped record is the
// project's live coord, Path A misses → Path D spawns a duplicate →
// split-brain (the operator now has two coords for the project, one
// invisible to the dashboard).
//
// Fix: ListStrict + bad-ID veto. When any agent JSON fails to parse,
// surface a SystemError naming the bad IDs and refuse to spawn.
func TestSystemFailure_CorruptLiveRecord_VetoesTier3(t *testing.T) {
	s := newFailoverSetup(t)
	s.installStubs(t)
	s.addProjectDir(t, "projects-fleet")
	// Write a malformed LIVE record (NOT in archive — this is the case
	// that would silently slip through and let Path D spawn over it).
	corruptPath := filepath.Join(s.tmp, "agents", "corruptv.json")
	if err := os.WriteFile(corruptPath, []byte("{not-json"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := s.run(t, "foo", AttachOpts{Project: "projects-fleet"})
	if err == nil {
		t.Fatal("expected SystemError when a live agent record is unparseable, got nil")
	}
	if !strings.Contains(err.Error(), "unparseable") || !strings.Contains(err.Error(), "corruptv") {
		t.Errorf("err must name the bad record IDs: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "orphan cleanup failed") {
		t.Errorf("err must explain the pre-spawn cleanup refusal: %q", err.Error())
	}
	if ec := ExitCodeFor(err); ec != 70 {
		t.Errorf("ExitCodeFor(corrupt live record): got %d want 70", ec)
	}
	if len(s.dispatched) != 0 {
		t.Errorf("must NOT spawn when records are corrupt; got %v", s.dispatched)
	}
}

// --- system failure tests: tmux missing, dispatch failed ---

func TestSystemFailure_TmuxMissing_ReturnsSystemErr(t *testing.T) {
	s := newFailoverSetup(t)
	s.installStubs(t)
	tmuxAvailableFnVar = func() error { return errors.New("tmux: command not found") }
	s.addProjectDir(t, "projects-fleet")
	s.addLiveCoord(t, "projects-fleet", "xxxxxxxx")
	_, err := s.run(t, "foo", AttachOpts{Project: "projects-fleet"})
	if err == nil {
		t.Fatal("expected system error when tmux missing")
	}
	if !strings.Contains(err.Error(), "tmux") {
		t.Errorf("err must mention tmux: %q", err.Error())
	}
	// Should suggest next-step install command (surface-don't-silo).
	if !strings.Contains(err.Error(), "install") && !strings.Contains(err.Error(), "PATH") {
		t.Errorf("err must surface next-step (install/PATH); got %q", err.Error())
	}
	// Codex review iter-1 P2: SystemError must map to its embedded exit
	// code (127 for tmux-missing) so main() exits with the documented
	// 127. Without the ExitCodeFor wiring this regressed to exit 1.
	if ec := ExitCodeFor(err); ec != 127 {
		t.Errorf("ExitCodeFor(tmux missing): got %d want 127", ec)
	}
}

// TestSystemFailure_SpawnSessionDead — codex review iter-2 P2. dispatch
// exits 0 but the newly-spawned tmux session is DEFINITIVELY dead (the
// coord process crashed before the first tmux paint, or initial-prompt
// delivery failed and the wrapper exited). Tier 3 must surface a
// SystemError with the retry next-step command rather than exec'ing into
// nothing and stranding the operator on tmux's "no sessions" error.
func TestSystemFailure_SpawnSessionDead_ReturnsSystemErr(t *testing.T) {
	s := newFailoverSetup(t)
	s.installStubs(t)
	// Replace the dispatch stub: returns an ID but does NOT register
	// fleet-<id> in aliveSessions → probe returns (false, nil).
	coordSpawnFnVar = func(project string) (string, error) {
		s.dispatched = append(s.dispatched, project)
		r := agent.New("ghostxxx")
		r.Project = project
		r.TaskID = projectlookup.CoordTaskID(project)
		r.TmuxSession = "fleet-ghostxxx"
		if err := r.Write(); err != nil {
			return "", err
		}
		// Intentionally do NOT add fleet-ghostxxx to aliveSessions.
		return "ghostxxx", nil
	}
	s.addProjectDir(t, "projects-fleet")
	_, err := s.run(t, "foo", AttachOpts{Project: "projects-fleet"})
	if err == nil {
		t.Fatal("expected SystemError when spawn session is dead")
	}
	if !strings.Contains(err.Error(), "session") || !strings.Contains(err.Error(), "ghostxxx") {
		t.Errorf("err must name the dead session: %q", err.Error())
	}
	// Must surface a concrete retry command (surface-don't-silo).
	if !strings.Contains(err.Error(), "re-run") {
		t.Errorf("err must surface next-step retry command: %q", err.Error())
	}
	// Codex iter-8 P2: retry command must embed the actual token, not
	// the literal placeholder `<token>` (would fail arg parsing on
	// copy-paste).
	if strings.Contains(err.Error(), "<token>") {
		t.Errorf("err must NOT contain literal `<token>` placeholder: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "attach foo") {
		t.Errorf("err must embed the actual token in the retry command (`attach foo`): %q", err.Error())
	}
	if ec := ExitCodeFor(err); ec != 70 {
		t.Errorf("ExitCodeFor(spawn session dead): got %d want 70", ec)
	}
	// Attach must NOT have been called — we'd otherwise have exec'd
	// into a dead session, defeating the gate.
	if s.attachCalls != 0 {
		t.Errorf("attach must not be called when spawn session is dead; got %d calls", s.attachCalls)
	}
}

// TestTier12_AttachRaceCoord_FailsOverToTier3 — codex review iter-13 P2.
// Tier 1 (live record) probe passed, but the session died before
// tmux.Attach. For coord-tagged records, Tier 3 can recover — must fall
// over instead of returning the raw tmux error.
//
// The trick: the SAME session must look alive to the probe (so Tier 1
// fires) AND dead by the time attach runs (the race). Once Tier 3 kicks
// in, the probe for the dead session is now consistent with the attach
// failure → Path A FindLiveCoord skips it, Path B lock-body misses,
// Path C StaleCoordRecord catches it (probe now reports dead) → Path C
// runs dispatch which spawns the successor.
func TestTier12_AttachRaceCoord_FailsOverToTier3(t *testing.T) {
	s := newFailoverSetup(t)
	s.installStubs(t)
	s.addProjectDir(t, "projects-fleet")
	// Coord-tagged live record. aliveSessions is empty initially so
	// the probe sees it as dead — but we override sessionProbeFnVar to
	// say alive on the FIRST probe call (Tier 1) and dead thereafter
	// (so Tier 3's recovery sees the truth).
	r := agent.New("deadrace")
	r.Project = "projects-fleet"
	r.TaskID = projectlookup.CoordTaskID("projects-fleet")
	r.TmuxSession = "fleet-deadrace"
	if err := r.Write(); err != nil {
		t.Fatal(err)
	}

	probeCalls := 0
	sessionProbeFnVar = func(session string) (bool, error) {
		if session == "fleet-deadrace" {
			probeCalls++
			if probeCalls == 1 {
				return true, nil // Tier 1 sees alive
			}
			return false, nil // Tier 3 sees dead (the truth)
		}
		return s.aliveSessions[session], nil
	}

	// Attach stub: errors only for fleet-deadrace (the race target).
	// dispatch stub registers fleet-newcoord as alive → spawned-session
	// attach succeeds.
	originalAttach := attachFnVar
	attachFnVar = func(session string) error {
		if session == "fleet-deadrace" {
			return errors.New("tmux: no such session (race)")
		}
		return originalAttach(session)
	}

	stderr, err := s.run(t, "deadrace", AttachOpts{})
	if err != nil {
		t.Fatalf("expected Tier 3 to recover the race, got %v", err)
	}
	got := stderr.String()
	if !strings.Contains(got, "lease resolved spawn for projects-fleet") {
		t.Errorf("stderr must surface resolver spawn after attach race: %q", got)
	}
	// Tier 3 Path C should fire: StaleCoordRecord (probe says dead) →
	// dispatch (no pre-archive after iter-8) → spawn fresh → attach.
	if len(s.dispatched) != 1 || s.dispatched[0] != "projects-fleet" {
		t.Errorf("expected Tier 3 to dispatch coord-spawn; got dispatched=%v", s.dispatched)
	}
	if s.attachedTo != "fleet-newcoord" {
		t.Errorf("attached to %q want fleet-newcoord (Tier 3 spawn)", s.attachedTo)
	}
}

// TestTier3_PromptDeliveryWarning_Surfaced — codex review iter-14 P2.
// dispatch can exit 0 with a "warning: initial prompt not delivered"
// — the agent is alive but /coordinator never ran. Tier 3's attach
// diagnostic must surface this so the operator knows the next step
// (type /coordinator manually) instead of attaching to a bare Claude
// session and assuming the project is owned.
func TestTier3_PromptDeliveryWarning_Surfaced(t *testing.T) {
	s := newFailoverSetup(t)
	s.installStubs(t)
	s.addProjectDir(t, "projects-fleet")
	// Override the full stub to report promptDelivered=false.
	coordSpawnFullFnVar = func(project string) (coordSpawnResult, error) {
		id, err := coordSpawnFnVar(project)
		if err != nil {
			return coordSpawnResult{}, err
		}
		return coordSpawnResult{ID: id, PromptDelivered: false}, nil
	}
	stderr, err := s.run(t, "foo", AttachOpts{Project: "projects-fleet"})
	if err != nil {
		t.Fatalf("expected attach to succeed (prompt warning is non-fatal), got %v", err)
	}
	got := stderr.String()
	if !strings.Contains(got, "initial prompt was NOT delivered") {
		t.Errorf("stderr must surface the prompt-delivery warning: %q", got)
	}
	if !strings.Contains(got, "type /coordinator manually") {
		t.Errorf("stderr must explain the next step: %q", got)
	}
}

// TestTier3_PromptDelivered_NoWarning — happy path: promptDelivered=true
// MUST NOT emit the warning (silent in the common case).
func TestTier3_PromptDelivered_NoWarning(t *testing.T) {
	s := newFailoverSetup(t)
	s.installStubs(t)
	s.addProjectDir(t, "projects-fleet")
	// Default stub (installStubs) sets PromptDelivered=true.
	stderr, err := s.run(t, "foo", AttachOpts{Project: "projects-fleet"})
	if err != nil {
		t.Fatalf("expected attach to succeed, got %v", err)
	}
	if strings.Contains(stderr.String(), "prompt was NOT delivered") {
		t.Errorf("happy-path stderr must NOT emit the warning: %q", stderr.String())
	}
}

// TestSystemFailure_ExistingCoordAttachRace — codex review iter-12 P2.
// Path A (FindLiveCoord hit) probes the session alive, but it dies
// between the probe and tmux.Attach. The operator should see the same
// retry advice (re-run attach) instead of a generic exit-1.
func TestSystemFailure_ExistingCoordAttachRace_ReturnsSystemErr(t *testing.T) {
	s := newFailoverSetup(t)
	s.installStubs(t)
	s.addProjectDir(t, "projects-fleet")
	s.addLiveCoord(t, "projects-fleet", "xxxxxxxx") // probe sees alive
	// Override attach stub: it errors as if the session died
	// post-FindLiveCoord-probe.
	attachFnVar = func(session string) error {
		s.attachedTo = session
		s.attachCalls++
		return errors.New("tmux: no such session (race)")
	}
	_, err := s.run(t, "foo", AttachOpts{Project: "projects-fleet"})
	if err == nil {
		t.Fatal("expected SystemError when existing-coord attach races")
	}
	if !strings.Contains(err.Error(), "existing coord") || !strings.Contains(err.Error(), "xxxxxxxx") {
		t.Errorf("err must name the dead coord ID: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "re-run") || !strings.Contains(err.Error(), "attach foo") {
		t.Errorf("err must surface re-run retry with actual token: %q", err.Error())
	}
	if ec := ExitCodeFor(err); ec != 70 {
		t.Errorf("ExitCodeFor(existing-coord race): got %d want 70", ec)
	}
}

// TestSystemFailure_AttachRaceAfterSpawn — codex review iter-10 P2.
// The probe-then-attach window is non-zero: the session can die after
// sessionProbeFnVar says alive but BEFORE tmux.Attach runs. The
// operator should see the same retry diagnostic, not a raw tmux error.
func TestSystemFailure_AttachRaceAfterSpawn_ReturnsSystemErr(t *testing.T) {
	s := newFailoverSetup(t)
	s.installStubs(t)
	// Spawn succeeds + registers the session as alive (so the probe
	// passes), but the attach stub returns an error simulating "session
	// died between probe and attach".
	attachFnVar = func(session string) error {
		s.attachedTo = session
		s.attachCalls++
		return errors.New("tmux: no such session")
	}
	s.addProjectDir(t, "projects-fleet")
	_, err := s.run(t, "foo", AttachOpts{Project: "projects-fleet"})
	if err == nil {
		t.Fatal("expected SystemError when attach race kills the session post-probe")
	}
	// Must surface the retry command (same shape as the dead-probe
	// branch — codex iter-10 P2 wants both windows to land on the same
	// operator surface).
	if !strings.Contains(err.Error(), "re-run") {
		t.Errorf("err must surface next-step retry: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "attach foo") {
		t.Errorf("err must embed actual token in retry: %q", err.Error())
	}
	if ec := ExitCodeFor(err); ec != 70 {
		t.Errorf("ExitCodeFor(attach race): got %d want 70", ec)
	}
}

func TestSystemFailure_DispatchFailed_ReturnsSystemErr(t *testing.T) {
	s := newFailoverSetup(t)
	s.installStubs(t)
	coordSpawnFnVar = func(project string) (string, error) {
		return "", errors.New("disk full")
	}
	s.addProjectDir(t, "projects-fleet")
	_, err := s.run(t, "foo", AttachOpts{Project: "projects-fleet"})
	if err == nil {
		t.Fatal("expected error when dispatch fails")
	}
	if !strings.Contains(err.Error(), "dispatch") || !strings.Contains(err.Error(), "disk full") {
		t.Errorf("err must mention dispatch + cause: %q", err.Error())
	}
	// Codex review iter-1 P2: dispatch failure maps to 70 (sysexits
	// EX_SOFTWARE) per ExitCodeFor's default for SystemError.
	if ec := ExitCodeFor(err); ec != 70 {
		t.Errorf("ExitCodeFor(dispatch failed): got %d want 70", ec)
	}
}

// --- F20: BUG #1 end-to-end — empty-session coord routes to Path D ---

// TestF20_EmptySessionCoord_RoutesToPathD pins the attach-failover BUG #1
// invariant end-to-end: a coord-<project> record with an EMPTY
// TmuxSession is unprobeable, so StaleCoordRecord returns (nil,false),
// Tier 3 falls through to Path D (fresh spawn), and the operator sees
// the Path-D "no coord" wording — NOT a bogus "reaped/recovered stale X"
// line. dispatch's own recovery still matches the empty-session record
// (it guards its alive probe with TmuxSession != ""), so recovery
// context is preserved without attach printing a false reap.
func TestF20_EmptySessionCoord_RoutesToPathD(t *testing.T) {
	s := newFailoverSetup(t)
	s.installStubs(t)
	s.addProjectDir(t, "projects-fleet")
	s.addEmptySessionCoord(t, "projects-fleet", "emptyse5") // empty TmuxSession
	stderr, err := s.run(t, "foo", AttachOpts{Project: "projects-fleet"})
	if err != nil {
		t.Fatalf("F20: expected no error, got %v", err)
	}
	got := stderr.String()
	if !strings.Contains(got, "foo: lease resolved spawn for projects-fleet") {
		t.Errorf("F20: want resolver spawn wording, got %q", got)
	}
	if strings.Contains(got, "reaped stale") || strings.Contains(got, "recovered emptyse5") {
		t.Errorf("F20: must NOT print a bogus reap/recover line for empty-session record; got %q", got)
	}
	if len(s.dispatched) != 1 {
		t.Errorf("F20: expected exactly one dispatch, got %v", s.dispatched)
	}
	if len(s.killedSessions) != 0 {
		t.Errorf("F20: must NOT kill any session for an empty-session record; got %v", s.killedSessions)
	}
}

// --- F21: BUG #2 — veto = wait-retry naming both causes, exit 75 ---

// TestF21_Veto_NoLiveCoord_WaitRetryExit75 pins the BUG #2 attach side:
// when dispatch exits 75 (the live-coord veto) and the scan-only re-
// resolve finds NO live coord, attach must exit 75 (NOT 70), name BOTH
// veto causes (cross-socket live + recent crash) plus the next-step
// command, and spawn EXACTLY ONCE (no in-process respawn loop).
func TestF21_Veto_NoLiveCoord_WaitRetryExit75(t *testing.T) {
	s := newFailoverSetup(t)
	s.installStubs(t)
	s.addProjectDir(t, "projects-fleet")
	s.addRecordOnly(t, "projects-fleet", "staleeee") // Path C: tagged record, tmux dead
	calls := s.installVetoStub(t)                    // dispatch exits 75
	_, err := s.run(t, "foo", AttachOpts{Project: "projects-fleet"})
	if err == nil {
		t.Fatal("F21: expected exit-75 error on veto with no live coord")
	}
	if ec := ExitCodeFor(err); ec != 75 {
		t.Errorf("F21: ExitCodeFor(veto) = %d, want 75 (never 70 for a veto)", ec)
	}
	got := err.Error()
	if !strings.Contains(got, "different tmux socket") || !strings.Contains(got, "freshness window") {
		t.Errorf("F21: error must name BOTH veto causes; got %q", got)
	}
	if !strings.Contains(got, "FLEET_TMUX_SOCKET") || !strings.Contains(got, "fleet attach foo --project projects-fleet") {
		t.Errorf("F21: error must include next-step command; got %q", got)
	}
	if *calls != 1 {
		t.Errorf("F21: expected exactly ONE spawn attempt (one-spawn-per-invocation), got %d", *calls)
	}
}

// --- F22: BUG #2 — veto then coord now live → attach exit 0 ---

// TestF22_Veto_CoordNowLive_Attaches pins the recover branch: dispatch
// vetoes (exit 75), but the scan-only re-resolve finds a now-live coord
// (the cross-socket/restarting one finished booting) → attach to it,
// exit 0, still exactly ONE spawn attempt.
func TestF22_Veto_CoordNowLive_Attaches(t *testing.T) {
	s := newFailoverSetup(t)
	s.installStubs(t)
	s.addProjectDir(t, "projects-fleet")
	s.addRecordOnly(t, "projects-fleet", "staleeee")
	// Veto stub that, on its single call, makes a live coord appear (as
	// if the restarting/cross-socket coord finished booting).
	calls := 0
	coordSpawnFullFnVar = func(project string) (coordSpawnResult, error) {
		calls++
		s.dispatched = append(s.dispatched, project)
		s.addLiveCoord(t, project, "liveyyyy")
		return coordSpawnResult{}, errCoordSpawnVetoed
	}
	stderr, err := s.run(t, "foo", AttachOpts{Project: "projects-fleet"})
	if err != nil {
		t.Fatalf("F22: expected no error (attach to now-live coord), got %v", err)
	}
	got := stderr.String()
	if !strings.Contains(got, "foo: attached to current coord liveyyyy for projects-fleet") {
		t.Errorf("F22: want attach-to-live line, got %q", got)
	}
	if s.attachedTo != "fleet-liveyyyy" {
		t.Errorf("F22: attached to %q want fleet-liveyyyy", s.attachedTo)
	}
	if calls != 1 {
		t.Errorf("F22: expected exactly ONE spawn attempt, got %d", calls)
	}
}

// --- F23: BUG #3 — one spawn per invocation under persistent veto ---

// TestF23_PersistentVeto_OneSpawnNoLoop: dispatch ALWAYS exits 75 and
// re-resolve never finds a live coord. The veto handler must do a scan-
// only re-resolve (NO 2nd spawn) and exit 75 — never enter an in-process
// respawn loop.
func TestF23_PersistentVeto_OneSpawnNoLoop(t *testing.T) {
	s := newFailoverSetup(t)
	s.installStubs(t)
	s.addProjectDir(t, "projects-fleet")
	s.addRecordOnly(t, "projects-fleet", "staleeee")
	calls := s.installVetoStub(t)
	_, err := s.run(t, "foo", AttachOpts{Project: "projects-fleet"})
	if ec := ExitCodeFor(err); ec != 75 {
		t.Errorf("F23: ExitCodeFor = %d, want 75", ec)
	}
	if *calls != 1 {
		t.Errorf("F23: dispatch must be invoked exactly ONCE (no respawn loop), got %d", *calls)
	}
}

// --- F23c: BUG #2 scope — Path D honors the veto identically ---

// TestF23c_PathD_Veto_Exit75 proves the veto classification lives in the
// SHARED spawn wrapper, not special-cased inside Path C: a Path D
// invocation (no coord at all) that hits the veto must behave exactly
// like F21 — exit 75, one dispatch, both causes named.
func TestF23c_PathD_Veto_Exit75(t *testing.T) {
	s := newFailoverSetup(t)
	s.installStubs(t)
	s.addProjectDir(t, "projects-fleet") // no coord → Path D
	calls := s.installVetoStub(t)
	_, err := s.run(t, "foo", AttachOpts{Project: "projects-fleet"})
	if ec := ExitCodeFor(err); ec != 75 {
		t.Errorf("F23c: ExitCodeFor(Path D veto) = %d, want 75", ec)
	}
	if !strings.Contains(err.Error(), "different tmux socket") || !strings.Contains(err.Error(), "freshness window") {
		t.Errorf("F23c: Path D veto must name both causes (shared wrapper); got %q", err.Error())
	}
	if *calls != 1 {
		t.Errorf("F23c: expected exactly ONE spawn attempt on Path D veto, got %d", *calls)
	}
}

// --- F26: MAJOR — Path B (proven-dead lock-body) does NOT kill ---

// TestF26_PathB_StaleLockBody_DoesNotKill pins the safety split: a
// proven-dead lock-body coord (session already dead) must spawn fresh
// WITHOUT calling tmux kill-session — killing a dead session can error
// and turn recovery into a false exit 70. The kill stub fails loudly if
// invoked.
func TestF26_PathB_StaleLockBody_DoesNotKill(t *testing.T) {
	s := newFailoverSetup(t)
	s.installStubs(t)
	// Override the kill stub to FAIL the test if Path B ever kills.
	killTmuxSessionFnVar = func(session string) error {
		t.Errorf("F26: Path B must NOT kill the already-dead session; got kill(%q)", session)
		return errors.New("kill should not be called")
	}
	s.addProjectDir(t, "projects-fleet")
	s.addStaleLockBodyCoord(t, "projects-fleet", "1ace0b0d") // untagged record + lock body, session dead
	stderr, err := s.run(t, "foo", AttachOpts{Project: "projects-fleet"})
	if err != nil {
		t.Fatalf("F26: expected no error (spawn-fresh proceeds), got %v", err)
	}
	got := stderr.String()
	if !strings.Contains(got, "foo: lease resolved spawn for projects-fleet") {
		t.Errorf("F26: want resolver spawn wording, got %q", got)
	}
	if len(s.killedSessions) != 0 {
		t.Errorf("F26: must NOT kill; got %v", s.killedSessions)
	}
	if len(s.dispatched) != 1 {
		t.Errorf("F26: expected exactly one dispatch, got %v", s.dispatched)
	}
}

// TestClassifyCoordSpawnRunErr exercises the load-bearing BUG #2
// cross-process exit-code contract against a REAL exec.ExitError (not a
// stub): a subprocess that exits 75 must classify as errCoordSpawnVetoed;
// exit 70 / other codes / non-exec errors must wrap as a hard failure.
// This is the only test that proves attach correctly reads dispatch's
// exit-75 veto over the actual subprocess boundary (F21/F23 stub the
// sentinel directly and skip the exec layer).
func TestClassifyCoordSpawnRunErr(t *testing.T) {
	run := func(code int) error {
		// `sh -c "exit N"` yields an *exec.ExitError with ExitCode()==N.
		return exec.Command("sh", "-c", fmt.Sprintf("exit %d", code)).Run()
	}

	// exit 75 → veto sentinel.
	if got := classifyCoordSpawnRunErr(run(75), "stderr text"); !errors.Is(got, errCoordSpawnVetoed) {
		t.Errorf("exit 75 must classify as errCoordSpawnVetoed; got %v", got)
	}
	// exit 70 → hard failure (NOT veto), stderr preserved.
	got70 := classifyCoordSpawnRunErr(run(70), "boom")
	if errors.Is(got70, errCoordSpawnVetoed) {
		t.Errorf("exit 70 must NOT classify as veto; got %v", got70)
	}
	if !strings.Contains(got70.Error(), "boom") {
		t.Errorf("exit 70 error must preserve stderr; got %v", got70)
	}
	// exit 1 → hard failure.
	if got := classifyCoordSpawnRunErr(run(1), ""); errors.Is(got, errCoordSpawnVetoed) {
		t.Errorf("exit 1 must NOT classify as veto; got %v", got)
	}
	// Non-exec error (e.g. binary missing) → hard failure, not veto.
	nonExec := classifyCoordSpawnRunErr(errors.New("locate self binary: nope"), "")
	if errors.Is(nonExec, errCoordSpawnVetoed) {
		t.Errorf("non-exec error must NOT classify as veto; got %v", nonExec)
	}
}

// --- F27: MAJOR — Path C' (live orphan) DOES kill that one session ---

// TestF27_PathCprime_LiveOrphan_Kills pins the other half of the split:
// a LIVE orphan session (no record) must be killed exactly once before
// the fresh spawn so its name doesn't collide. This is the Path C' case
// F7 also exercises; F27 asserts the kill-once + live-orphan wording.
func TestF27_PathCprime_LiveOrphan_Kills(t *testing.T) {
	s := newFailoverSetup(t)
	s.installStubs(t)
	s.addProjectDir(t, "projects-fleet")
	s.addOrphanTmux(t, "0a0bbeef") // LIVE session, no live record
	s.addArchivedRecord(t, "0a0bbeef", "projects-fleet", agent.ArchivedCauseKill, "")
	stderr, err := s.run(t, "foo", AttachOpts{Project: "projects-fleet"})
	if err != nil {
		t.Fatalf("F27: expected no error, got %v", err)
	}
	got := stderr.String()
	if !strings.Contains(got, "foo: reaped stale 0a0bbeef (live orphan session); spawned newcoord for projects-fleet; attaching") {
		t.Errorf("F27: want Path-C' live-orphan wording, got %q", got)
	}
	if len(s.killedSessions) != 1 || s.killedSessions[0] != "fleet-0a0bbeef" {
		t.Errorf("F27: expected exactly one tmux.Kill on fleet-0a0bbeef, got %v", s.killedSessions)
	}
	if len(s.dispatched) != 1 {
		t.Errorf("F27: expected exactly one dispatch, got %v", s.dispatched)
	}
}
