package main

// coord_spawn_idempot_test.go — tests for the cold-start double-spawn
// fix (leak-coord-spawn-idempot): durable pending-spawn claim, OR-based
// veto, and idempotent attach-if-live. Maps to T1–T7 in
// docs/TASK-PLAN-leak-coord-spawn-idempot.md.

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/state"
)

// newPendingClaimHome sets FLEET_HOME to a temp dir and returns the
// per-project dir so a test can inspect / pre-seed the pending-claim
// file directly.
func newPendingClaimHome(t *testing.T, project string) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("FLEET_HOME", root)
	pdir, err := state.ProjectDir(project)
	if err != nil {
		t.Fatalf("ProjectDir(%q): %v", project, err)
	}
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatalf("mkdir %q: %v", pdir, err)
	}
	return pdir
}

// T5 (Go half): writeCoordPendingClaim writes a durable, atomically
// renamed claim file containing the agent ID + spawn timestamp; a
// freshly written claim reads back as fresh.
func TestCoordPendingClaim_WriteThenFresh(t *testing.T) {
	pdir := newPendingClaimHome(t, "myproj")

	if err := writeCoordPendingClaim("myproj", "agent-abc"); err != nil {
		t.Fatalf("writeCoordPendingClaim: %v", err)
	}

	// The on-disk file lives at the documented path.
	claimPath := filepath.Join(pdir, "coord-spawn-pending")
	if _, err := os.Stat(claimPath); err != nil {
		t.Fatalf("claim file not written at %q: %v", claimPath, err)
	}
	// No leftover .tmp from the atomic write.
	entries, _ := os.ReadDir(pdir)
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" || len(e.Name()) > len("coord-spawn-pending.tmp") && e.Name()[:len("coord-spawn-pending.tmp")] == "coord-spawn-pending.tmp" {
			t.Errorf("leftover tmp file after atomic write: %s", e.Name())
		}
	}

	fresh, claim := coordPendingClaimFresh("myproj", 5*time.Minute)
	if !fresh {
		t.Fatalf("freshly written claim must read as fresh")
	}
	if claim.AgentID != "agent-abc" {
		t.Errorf("claim AgentID = %q, want agent-abc", claim.AgentID)
	}
	if claim.SpawnedAt.IsZero() {
		t.Errorf("claim SpawnedAt must be set")
	}
}

// T1: a claim written ~18s ago (cold-start window) reads as fresh under
// the 5-minute budget → veto refuses the second spawn.
func TestCoordPendingClaim_RecentIsFresh(t *testing.T) {
	pdir := newPendingClaimHome(t, "projects-fleet")
	writeClaimAt(t, pdir, "agent-1", time.Now().UTC().Add(-18*time.Second))

	fresh, claim := coordPendingClaimFresh("projects-fleet", 5*time.Minute)
	if !fresh {
		t.Fatalf("an 18s-old claim must be fresh under a 5m budget")
	}
	if claim.AgentID != "agent-1" {
		t.Errorf("AgentID = %q, want agent-1", claim.AgentID)
	}
}

// T3: a stale claim (older than budget) reads as NOT fresh → veto does
// not block; the dispatch proceeds and overwrites the old claim.
func TestCoordPendingClaim_StaleIsNotFresh(t *testing.T) {
	pdir := newPendingClaimHome(t, "projects-fleet")
	writeClaimAt(t, pdir, "agent-old", time.Now().UTC().Add(-10*time.Minute))

	fresh, _ := coordPendingClaimFresh("projects-fleet", 5*time.Minute)
	if fresh {
		t.Fatalf("a 10m-old claim must NOT be fresh under a 5m budget")
	}
}

// Missing claim file → not fresh (no claim ever written).
func TestCoordPendingClaim_MissingIsNotFresh(t *testing.T) {
	newPendingClaimHome(t, "projects-fleet")
	fresh, _ := coordPendingClaimFresh("projects-fleet", 5*time.Minute)
	if fresh {
		t.Fatalf("missing claim must read as not fresh")
	}
}

// A corrupt claim file must not be treated as fresh (fail toward
// allowing the spawn — a corrupt claim cannot block forever).
func TestCoordPendingClaim_CorruptIsNotFresh(t *testing.T) {
	pdir := newPendingClaimHome(t, "projects-fleet")
	if err := os.WriteFile(filepath.Join(pdir, "coord-spawn-pending"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write corrupt claim: %v", err)
	}
	fresh, _ := coordPendingClaimFresh("projects-fleet", 5*time.Minute)
	if fresh {
		t.Fatalf("corrupt claim must NOT read as fresh")
	}
}

// clearCoordPendingClaim removes the claim file (the coord's first tick
// calls the Python equivalent). Removing an absent claim is a no-op.
func TestCoordPendingClaim_Clear(t *testing.T) {
	pdir := newPendingClaimHome(t, "myproj")
	if err := writeCoordPendingClaim("myproj", "agent-abc"); err != nil {
		t.Fatalf("writeCoordPendingClaim: %v", err)
	}
	if err := clearCoordPendingClaim("myproj"); err != nil {
		t.Fatalf("clearCoordPendingClaim: %v", err)
	}
	if _, err := os.Stat(filepath.Join(pdir, "coord-spawn-pending")); !os.IsNotExist(err) {
		t.Fatalf("claim file must be gone after clear, stat err = %v", err)
	}
	// Idempotent: clearing an absent claim is not an error.
	if err := clearCoordPendingClaim("myproj"); err != nil {
		t.Errorf("clearing an absent claim must be a no-op, got %v", err)
	}
}

// writeClaimAt writes a claim file with an explicit spawned_at so a test
// can simulate an aged claim without sleeping.
func writeClaimAt(t *testing.T, pdir, agentID string, ts time.Time) {
	t.Helper()
	c := coordPendingClaim{AgentID: agentID, SpawnedAt: ts, PID: 4242}
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal claim: %v", err)
	}
	if err := state.WriteAtomic(filepath.Join(pdir, "coord-spawn-pending"), data); err != nil {
		t.Fatalf("write claim: %v", err)
	}
}

// ---- T7: OR-based veto, exhaustive over the three signals ----

// coordSpawnVeto returns a non-empty reason when ANY of the three
// signals indicates a coord is (or is about to be) supervising the
// project:
//   - coordStateFresh && recordExists  (the original AND-gate)
//   - a live coord record (tmux session alive) for this task_id
//   - a fresh pending-spawn claim
//
// It returns "" only when ALL three are absent.
func TestCoordSpawnVeto_ORLogic(t *testing.T) {
	cases := []struct {
		name        string
		stateFresh  bool
		recordExist bool
		liveCoord   bool
		claimFresh  bool
		wantVeto    bool
	}{
		{"all absent", false, false, false, false, false},
		{"coord-state only (no record)", true, false, false, false, false},
		{"record only (state stale)", false, true, false, false, false},
		{"coord-state AND record", true, true, false, false, true},
		{"live coord record only", false, false, true, false, true},
		{"pending claim only", false, false, false, true, true},
		{"claim + state+record", true, true, false, true, true},
		{"live coord + claim", false, false, true, true, true},
		{"everything", true, true, true, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason := coordSpawnVeto(tc.stateFresh, tc.recordExist, tc.liveCoord, tc.claimFresh)
			gotVeto := reason != ""
			if gotVeto != tc.wantVeto {
				t.Errorf("coordSpawnVeto(stateFresh=%v, record=%v, live=%v, claim=%v) veto=%v, want %v (reason=%q)",
					tc.stateFresh, tc.recordExist, tc.liveCoord, tc.claimFresh, gotVeto, tc.wantVeto, reason)
			}
		})
	}
}

// ---- T4: idempotent attach-if-live ----

// liveCoordForAttach returns the live coord record (tmux session alive)
// matching the task_id+project, or nil. The dispatch path prints an
// attach hint and skips the spawn when this returns non-nil.
func TestLiveCoordForAttach_FindsLive(t *testing.T) {
	recs := []*agent.Record{
		{ID: "dead1", TaskID: "coord-projects-fleet", Project: "projects-fleet", TmuxSession: "fleet-dead1"},
		{ID: "live1", TaskID: "coord-projects-fleet", Project: "projects-fleet", TmuxSession: "fleet-live1"},
	}
	sessionAlive := func(s string) bool { return s == "fleet-live1" }

	got := liveCoordForAttach("coord-projects-fleet", "projects-fleet", recs, sessionAlive)
	if got == nil {
		t.Fatalf("expected the live coord record, got nil")
	}
	if got.ID != "live1" {
		t.Errorf("liveCoordForAttach = %q, want live1", got.ID)
	}
}

func TestLiveCoordForAttach_NoneLive(t *testing.T) {
	recs := []*agent.Record{
		{ID: "dead1", TaskID: "coord-projects-fleet", Project: "projects-fleet", TmuxSession: "fleet-dead1"},
	}
	sessionAlive := func(string) bool { return false }
	if got := liveCoordForAttach("coord-projects-fleet", "projects-fleet", recs, sessionAlive); got != nil {
		t.Errorf("expected nil when no session alive, got %q", got.ID)
	}
}

func TestLiveCoordForAttach_WrongProjectIgnored(t *testing.T) {
	recs := []*agent.Record{
		{ID: "other", TaskID: "coord-projects-other", Project: "projects-other", TmuxSession: "fleet-other"},
	}
	sessionAlive := func(string) bool { return true }
	if got := liveCoordForAttach("coord-projects-fleet", "projects-fleet", recs, sessionAlive); got != nil {
		t.Errorf("expected nil for a different project, got %q", got.ID)
	}
}

// uniqueLiveCoordForAttach returns the live record ONLY when exactly one
// is live; nil for zero or for the ambiguous multi-live case (codex
// iter-8 P2) so the fast path never promotes the wrong session in a
// duplicate-coord state.
func TestUniqueLiveCoordForAttach(t *testing.T) {
	sessionAlive := func(string) bool { return true }
	const taskID = "coord-projects-fleet"
	const project = "projects-fleet"

	// Exactly one live → returns it.
	one := []*agent.Record{
		{ID: "live1", TaskID: taskID, Project: project, TmuxSession: "fleet-live1"},
	}
	if got := uniqueLiveCoordForAttach(taskID, project, one, sessionAlive); got == nil || got.ID != "live1" {
		t.Errorf("single live candidate: got %v, want live1", got)
	}

	// Two live → nil (ambiguous, refuse to promote).
	two := []*agent.Record{
		{ID: "live1", TaskID: taskID, Project: project, TmuxSession: "fleet-live1"},
		{ID: "live2", TaskID: taskID, Project: project, TmuxSession: "fleet-live2"},
	}
	if got := uniqueLiveCoordForAttach(taskID, project, two, sessionAlive); got != nil {
		t.Errorf("two live candidates must return nil (ambiguous), got %q", got.ID)
	}

	// Zero live → nil.
	none := []*agent.Record{
		{ID: "dead1", TaskID: taskID, Project: project, TmuxSession: "fleet-dead1"},
	}
	if got := uniqueLiveCoordForAttach(taskID, project, none, func(string) bool { return false }); got != nil {
		t.Errorf("no live candidate must return nil, got %q", got.ID)
	}
}

// ---- P2 (codex iter-2): attach fast path preserves the spawn-output contract ----

// When --coord-spawn finds a live coord already supervising the project,
// runDispatch short-circuits to the attach fast path and exits 0. The
// wrapper callers (attach.go:doCoordSpawn → parseSpawnedAgentID, TUI
// startCoordSpawn) parse the canonical `agent <id> spawned` line from a
// 0-exit dispatch to learn which session to probe+attach. The fast path
// MUST emit that line with the LIVE coord's ID — otherwise an "already
// alive" race exits 0 with no parseable line and the wrapper reports a
// parse failure instead of attaching (codex iter-2 P2).
func TestRunDispatch_AttachFastPathEmitsSpawnLine(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEET_HOME", root)
	isolateTmuxSocket(t)

	pdir, err := state.ProjectDir("aliveproj")
	if err != nil {
		t.Fatalf("ProjectDir: %v", err)
	}
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Seed a live coord record for the project's stable coord task_id.
	if err := os.MkdirAll(filepath.Join(root, "agents"), 0o755); err != nil {
		t.Fatalf("mkdir agents: %v", err)
	}
	const liveID = "abcd1234"
	spawnedAt := time.Now().UTC().Add(-10 * time.Second)
	rec := &agent.Record{
		ID:          liveID,
		TaskID:      CoordTaskIDPrefix + "aliveproj",
		Project:     "aliveproj",
		TmuxSession: "fleet-" + liveID,
		SpawnedAt:   spawnedAt,
	}
	if werr := rec.Write(); werr != nil {
		t.Fatalf("seed live coord record: %v", werr)
	}
	// Fresh coord-state.json whose mtime POST-DATES the record's spawn —
	// proves THIS coord ticked (codex iter-3 + iter-7 P2 gate). Chtimes to
	// a deterministic instant after spawnedAt so sub-second clock skew
	// can't flip mtime.Before(spawnedAt).
	statePath := filepath.Join(pdir, "coord-state.json")
	if err := os.WriteFile(statePath, []byte("{}"), 0o644); err != nil {
		t.Fatalf("seed coord-state.json: %v", err)
	}
	ticked := spawnedAt.Add(3 * time.Second)
	if err := os.Chtimes(statePath, ticked, ticked); err != nil {
		t.Fatalf("chtimes coord-state.json: %v", err)
	}

	// Report ONLY the live coord's session as alive.
	prev := tmuxHasSession
	tmuxHasSession = func(s string) bool { return s == "fleet-"+liveID }
	t.Cleanup(func() { tmuxHasSession = prev })

	opts := &dispatchOpts{
		taskID:          CoordTaskIDPrefix + "aliveproj",
		project:         "aliveproj",
		projectExplicit: true,
		coordSpawn:      true,
		command:         []string{"sleep", "30"},
		commandExplicit: true,
	}
	var out bytes.Buffer
	if derr := runDispatch(opts, &out); derr != nil {
		t.Fatalf("attach fast path must exit cleanly, got: %v", derr)
	}
	// The stdout must carry a parseable canonical spawn line so the
	// wrapper attaches to the live coord instead of failing the parse.
	got := parseSpawnedAgentID(out.String())
	if got != liveID {
		t.Fatalf("parseSpawnedAgentID(%q) = %q, want %q (attach fast path broke the spawn-output contract)",
			out.String(), got, liveID)
	}
}

// TestRunDispatch_AttachFastPathSkippedWhenNotTicking pins the
// prompt-failure recovery contract for a live coord record + alive tmux
// session + NO fresh coord-state.json + NO fresh claim (the session never
// ticked /coordinator — e.g. its initial prompt failed to deliver and
// clearClaimOnPromptFailure dropped the claim):
//
//	(iter-3 P2) The attach fast path must NOT fire — it would exit 0 and
//	   the TUI would PROMOTE a non-coordinator session as the coord.
//	(iter-6 P2) The veto must NOT fire either — with no fresh claim, the
//	   dispatch must FALL THROUGH to a fresh respawn so the operator's
//	   prompt-failure recovery ([a] / re-dispatch) isn't stranded. The
//	   cold-start duplicate window is closed by the CLAIM (tested
//	   separately in TestRunDispatch_ColdStartRefusedByPendingClaim), not
//	   by raw tmux liveness, precisely so this recovery path stays open.
//
// Net: a live-but-not-ticking coord with no claim is neither promoted nor
// veto-stranded; the dispatch respawns.
func TestRunDispatch_AttachFastPathSkippedWhenNotTicking(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEET_HOME", root)
	// Empty socket → the fall-through respawn fails deterministically under
	// go test (no real session leaks), so we can assert "fell through to
	// respawn" by observing a NON-veto error and no live-ID promotion.
	t.Setenv("FLEET_TMUX_SOCKET", "")

	pdir, err := state.ProjectDir("nottick")
	if err != nil {
		t.Fatalf("ProjectDir: %v", err)
	}
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "agents"), 0o755); err != nil {
		t.Fatalf("mkdir agents: %v", err)
	}
	// Live coord record + alive session, but coord-state.json ABSENT
	// (never ticked → prompt-failed scenario).
	const liveID = "beef5678"
	rec := &agent.Record{
		ID:          liveID,
		TaskID:      CoordTaskIDPrefix + "nottick",
		Project:     "nottick",
		TmuxSession: "fleet-" + liveID,
		SpawnedAt:   time.Now().UTC(),
	}
	if werr := rec.Write(); werr != nil {
		t.Fatalf("seed live coord record: %v", werr)
	}
	if _, statErr := os.Stat(filepath.Join(pdir, "coord-state.json")); !os.IsNotExist(statErr) {
		t.Fatalf("precondition: coord-state.json must be absent, stat err = %v", statErr)
	}

	prev := tmuxHasSession
	tmuxHasSession = func(s string) bool { return s == "fleet-"+liveID }
	t.Cleanup(func() { tmuxHasSession = prev })

	opts := &dispatchOpts{
		taskID:          CoordTaskIDPrefix + "nottick",
		project:         "nottick",
		projectExplicit: true,
		coordSpawn:      true,
		command:         []string{"sleep", "30"},
		commandExplicit: true,
	}
	var out bytes.Buffer
	derr := runDispatch(opts, &out)
	// iter-6: the dispatch must FALL THROUGH to respawn (which fails on the
	// empty socket), NOT veto — otherwise prompt-failure recovery strands.
	if derr == nil {
		t.Fatalf("expected fall-through to respawn (fails on empty socket), got nil")
	}
	// It must NOT be a vetoError: a veto here would strand the recovery
	// (iter-6 P2). The error must come from the spawn path instead.
	var ve *vetoError
	if errors.As(derr, &ve) {
		t.Fatalf("dispatch vetoed a no-claim live-but-not-ticking session (%v) — strands prompt-failure recovery", derr)
	}
	// iter-3: the fast path must NOT have promoted the session — its ID
	// must never appear as a successful `agent <id> spawned` line.
	if got := parseSpawnedAgentID(out.String()); got == liveID {
		t.Fatalf("fast path promoted a non-ticking session (%q) — prompt-failure gate not enforced; out=%q", liveID, out.String())
	}
}

// ---- P2 (codex iter-4): prompt-delivery failure must clear the claim ----

// When spawn.Spawn succeeds but the initial prompt fails to deliver, the
// session is alive but /coordinator never starts, so the coord's first
// tick (the normal claim-clearer) never runs. clearClaimOnPromptFailure
// drops the claim so an immediate retry isn't vetoed for the full
// freshness window. Guarded to the coord-spawn path: a non-coord-spawn
// dispatch (worker) must never touch a coord claim.
func TestClearClaimOnPromptFailure(t *testing.T) {
	newPendingClaimHome(t, "promptfail")

	seed := func(t *testing.T) {
		t.Helper()
		if err := writeCoordPendingClaim("promptfail", "agent-pf"); err != nil {
			t.Fatalf("writeCoordPendingClaim: %v", err)
		}
	}
	claimExists := func(t *testing.T) bool {
		t.Helper()
		path, err := coordPendingClaimPath("promptfail")
		if err != nil {
			t.Fatalf("coordPendingClaimPath: %v", err)
		}
		_, statErr := os.Stat(path)
		return statErr == nil
	}

	// coord-spawn path → claim cleared.
	seed(t)
	clearClaimOnPromptFailure(&dispatchOpts{
		project: "promptfail", coordSpawn: true,
	}, "agent-pf")
	if claimExists(t) {
		t.Errorf("coord-spawn prompt-failure must clear the claim")
	}

	// non-coord-spawn path → claim untouched (worker dispatch).
	seed(t)
	clearClaimOnPromptFailure(&dispatchOpts{
		project: "promptfail", coordSpawn: false,
	}, "agent-pf")
	if !claimExists(t) {
		t.Errorf("non-coord-spawn dispatch must NOT clear a coord claim")
	}

	// empty preAllocatedID (defensive guard) → claim untouched.
	clearClaimOnPromptFailure(&dispatchOpts{
		project: "promptfail", coordSpawn: true,
	}, "")
	if !claimExists(t) {
		t.Errorf("empty preAllocatedID must NOT clear the claim (guard mismatch)")
	}
}

// ---- P2 (codex iter-7): candidate-specific freshness for the fast path ----

// coordStateTickedAfter must prove the SELECTED record ticked: file
// present + fresh + mtime post-dates the record's spawn. It fails CLOSED
// (false) on absent / stale / mtime-older-than-spawn.
func TestCoordStateTickedAfter(t *testing.T) {
	pdir := newPendingClaimHome(t, "tickedafter")
	statePath := filepath.Join(pdir, "coord-state.json")
	spawnedAt := time.Now().UTC()

	// No coord-state.json yet → false.
	if coordStateTickedAfter("tickedafter", spawnedAt) {
		t.Errorf("absent coord-state must be NOT-ticked")
	}

	// Write coord-state then set its mtime to AFTER the record spawned →
	// true (the record ticked).
	if err := os.WriteFile(statePath, []byte("{}"), 0o644); err != nil {
		t.Fatalf("write coord-state: %v", err)
	}
	after := spawnedAt.Add(2 * time.Second)
	if err := os.Chtimes(statePath, after, after); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if !coordStateTickedAfter("tickedafter", spawnedAt) {
		t.Errorf("fresh coord-state mtime after spawn must be ticked")
	}

	// mtime BEFORE the record spawned (stale-from-predecessor) → false.
	before := spawnedAt.Add(-2 * time.Second)
	if err := os.Chtimes(statePath, before, before); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if coordStateTickedAfter("tickedafter", spawnedAt) {
		t.Errorf("coord-state mtime predating spawn must be NOT-ticked (predecessor staleness)")
	}

	// Fresh mtime but OLDER than the freshness window → false.
	old := time.Now().UTC().Add(-10 * time.Minute)
	if err := os.Chtimes(statePath, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if coordStateTickedAfter("tickedafter", old.Add(-time.Minute)) {
		t.Errorf("stale coord-state (past freshness window) must be NOT-ticked")
	}
}

// TestRunDispatch_FastPathSkipsStalePredecessorState pins iter-7 at the
// dispatch level: coord-state.json is fresh from a now-archived
// PREDECESSOR (mtime predates the live record's spawn), so the fast path
// must NOT promote the new-but-not-yet-ticked live record. It falls
// through to the veto/recovery path instead.
func TestRunDispatch_FastPathSkipsStalePredecessorState(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEET_HOME", root)
	t.Setenv("FLEET_TMUX_SOCKET", "")

	pdir, err := state.ProjectDir("staleproj")
	if err != nil {
		t.Fatalf("ProjectDir: %v", err)
	}
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "agents"), 0o755); err != nil {
		t.Fatalf("mkdir agents: %v", err)
	}

	// The live record spawned NOW.
	const liveID = "cafe9999"
	now := time.Now().UTC()
	rec := &agent.Record{
		ID:          liveID,
		TaskID:      CoordTaskIDPrefix + "staleproj",
		Project:     "staleproj",
		TmuxSession: "fleet-" + liveID,
		SpawnedAt:   now,
	}
	if werr := rec.Write(); werr != nil {
		t.Fatalf("seed live coord record: %v", werr)
	}
	// coord-state.json is FRESH (within window) but its mtime PREDATES the
	// new record's spawn — it belongs to a now-gone predecessor.
	statePath := filepath.Join(pdir, "coord-state.json")
	if err := os.WriteFile(statePath, []byte("{}"), 0o644); err != nil {
		t.Fatalf("write coord-state: %v", err)
	}
	predecessor := now.Add(-30 * time.Second)
	if err := os.Chtimes(statePath, predecessor, predecessor); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	prev := tmuxHasSession
	tmuxHasSession = func(s string) bool { return s == "fleet-"+liveID }
	t.Cleanup(func() { tmuxHasSession = prev })

	opts := &dispatchOpts{
		taskID:          CoordTaskIDPrefix + "staleproj",
		project:         "staleproj",
		projectExplicit: true,
		coordSpawn:      true,
		command:         []string{"sleep", "30"},
		commandExplicit: true,
	}
	var out bytes.Buffer
	derr := runDispatch(opts, &out)
	// The fast path must NOT promote the not-yet-ticked record. (Whatever
	// the downstream outcome — veto or spawn-fail — the live ID must never
	// appear as a successful spawn line from the fast path.)
	if derr == nil {
		t.Fatalf("expected fast path skipped → downstream refusal/spawn-fail, got nil")
	}
	if got := parseSpawnedAgentID(out.String()); got == liveID {
		t.Fatalf("fast path promoted a record whose coord-state is stale-from-predecessor (%q); out=%q", liveID, out.String())
	}
}

// ---- P2 (codex iter-1): failed spawn must clear the pending claim ----

// A coord-spawn dispatch writes the pending claim immediately BEFORE
// spawn.Spawn. If spawn.Spawn then fails, NO coord is booting — so the
// claim is a lie. Leaving it would make every retry hit
// coordPendingClaimFresh and get vetoed for the full freshness window,
// blocking immediate recovery from a transient spawn failure. The error
// path must clear the claim it wrote.
//
// We force a deterministic spawn failure by leaving FLEET_TMUX_SOCKET
// UNSET: tmux.Spawn refuses the default socket under `go test`, so
// spawn.Spawn returns an error AFTER runDispatch has written the claim.
func TestRunDispatch_FailedSpawnClearsPendingClaim(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEET_HOME", root)
	// Force tmux.Spawn to fail under go test: empty FLEET_TMUX_SOCKET is
	// the rejection trigger (the orphan-leak guard). Explicit unset so a
	// leaked env from another test can't accidentally let the spawn
	// succeed and leave a real tmux session behind.
	t.Setenv("FLEET_TMUX_SOCKET", "")

	pdir, err := state.ProjectDir("spawnfail")
	if err != nil {
		t.Fatalf("ProjectDir: %v", err)
	}
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Clean veto state: no claim, no coord-state.json, no live coord —
	// so the dispatch passes every gate and reaches spawn.Spawn.
	claimPath := filepath.Join(pdir, "coord-spawn-pending")
	if _, statErr := os.Stat(claimPath); !os.IsNotExist(statErr) {
		t.Fatalf("precondition: claim must be absent, stat err = %v", statErr)
	}

	opts := &dispatchOpts{
		taskID:          CoordTaskIDPrefix + "spawnfail",
		project:         "spawnfail",
		projectExplicit: true,
		coordSpawn:      true,
		command:         []string{"sleep", "30"},
		commandExplicit: true,
	}
	var out bytes.Buffer
	err = runDispatch(opts, &out)
	if err == nil {
		t.Fatalf("expected spawn failure (no tmux socket under go test), got nil")
	}
	// A vetoError would mean a gate refused us before spawn — that's the
	// wrong failure; we need to have REACHED spawn.Spawn to exercise the
	// claim-cleanup path.
	var ve *vetoError
	if errors.As(err, &ve) {
		t.Fatalf("dispatch was vetoed before spawn (%v); test cannot exercise the failed-spawn cleanup path", err)
	}
	// The claim written just before the failed spawn must be cleared.
	if _, statErr := os.Stat(claimPath); !os.IsNotExist(statErr) {
		t.Fatalf("pending claim must be cleared after a failed spawn, stat err = %v", statErr)
	}
}

// ---- T1 (integration): cold-start double-spawn refused via pending claim ----

// A fresh pending-spawn claim on disk (no coord-state.json yet — the
// cold-start window) must make a second --coord-spawn dispatch refuse
// with a typed vetoError, BEFORE reaching spawn.Spawn. On the parent
// commit the pending-claim signal does not exist, so the dispatch falls
// through the veto and proceeds to spawn — this test would fail there.
func TestRunDispatch_ColdStartRefusedByPendingClaim(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEET_HOME", root)
	// Isolate tmux so a host with tmux installed never leaks a session
	// if the veto regressed and the spawn path were reached.
	isolateTmuxSocket(t)

	pdir, err := state.ProjectDir("coldstart")
	if err != nil {
		t.Fatalf("ProjectDir: %v", err)
	}
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Simulate dispatch #1 having written the claim ~18s ago and spawned;
	// coord-state.json is deliberately ABSENT (cold start).
	writeClaimAt(t, pdir, "agent-first", time.Now().UTC().Add(-18*time.Second))

	opts := &dispatchOpts{
		taskID:          CoordTaskIDPrefix + "coldstart",
		project:         "coldstart",
		projectExplicit: true,
		coordSpawn:      true,
		command:         []string{"sleep", "30"},
		commandExplicit: true,
	}
	var out bytes.Buffer
	err = runDispatch(opts, &out)
	if err == nil {
		t.Fatalf("expected refusal, got nil error (a duplicate coord would have spawned)")
	}
	var ve *vetoError
	if !errors.As(err, &ve) {
		t.Fatalf("expected *vetoError (exit 75 retry signal), got %T: %v", err, err)
	}
}
