// Package projectlookup tests — TDD red for attach-failover-59db.
//
// Covers the helpers Tier 3 PROJECT RECOVERY depends on:
//
//   - KnownProjects: enumerate ~/.fleet/projects/<name>/ on disk,
//     skipping malformed/reserved names — same rules the dashboard
//     applies, but factored out so cmd/fleet/attach.go and
//     internal/tui both share one source of truth (avoids the picker
//     and the resolver disagreeing about which projects exist).
//   - FindLiveCoord: scan records for task_id == coord-<project>
//     AND project == <project> AND tmux session alive. Returns the
//     match. NO marker requirement (unlike the TUI's actionAttach-
//     Project [a]-dedup helper) because Tier 3 is project recovery, not
//     duplicate-spawn protection — any live coord for the project is
//     acceptable.
//   - FindCoordByLockBody: extract the ID from
//     ~/.fleet/projects/<name>/.locks/coordinator.lock and match
//     against records with a live tmux session.
//   - OrphanTmuxForProject: a tmux session matching fleet-<id>
//     where <id> has no matching record. Drives the "no record but
//     lingering tmux" branch.
package projectlookup

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/state"
)

// withFleetHome points FLEET_HOME at a fresh tempdir for the test and
// returns the dir. t.Cleanup undoes the env mutation.
func withFleetHome(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	return tmp
}

// mkProjectDir creates ~/.fleet/projects/<name>/ inside FLEET_HOME.
// The dashboard's KnownProjects-equivalent enumerates this directory.
func mkProjectDir(t *testing.T, fleetHome, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(fleetHome, "projects", name), 0o755); err != nil {
		t.Fatal(err)
	}
}

// writeCoordLockBody writes <agentID> to the project's coordinator.lock
// so FindCoordByLockBody can find it via readCoordHolder-style logic.
func writeCoordLockBody(t *testing.T, fleetHome, project, agentID string) {
	t.Helper()
	dir := filepath.Join(fleetHome, "projects", project, ".locks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "coordinator.lock"), []byte(agentID+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeAgentRec writes ~/.fleet/agents/<id>.json with the given fields
// so agent.List picks it up. TmuxSession defaults to fleet-<id> when
// empty so the live-coord paths default to a sensible probe target.
func writeAgentRec(t *testing.T, id, project, taskID string) *agent.Record {
	t.Helper()
	// Ensure ~/.fleet/agents/ exists; agent.Record.Write writes the
	// tmp file there before renaming and refuses to MkdirAll on its
	// own path (state.AgentDir does NOT create the dir, only resolves
	// it). Tests using the production write path therefore have to
	// bootstrap the dir up front — same pattern cmd/fleet/attach_test
	// uses.
	if dir, err := state.AgentDir(); err == nil {
		_ = os.MkdirAll(dir, 0o755)
	}
	r := agent.New(id)
	r.Project = project
	r.TaskID = taskID
	r.TmuxSession = "fleet-" + id
	if err := r.Write(); err != nil {
		t.Fatal(err)
	}
	return r
}

// recordsFromList shells through agent.List so tests exercise the
// production code path; ordering by ID keeps assertions deterministic.
func recordsFromList(t *testing.T) []*agent.Record {
	t.Helper()
	rs, err := agent.List()
	if err != nil {
		t.Fatalf("agent.List: %v", err)
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].ID < rs[j].ID })
	return rs
}

// --- KnownProjects ---

// TestKnownProjects_EmptyHomeReturnsEmpty — no projects on disk →
// empty slice + nil error. Critical for the cmd/fleet/attach picker
// path: an operator on a fresh box must not get a panic; the
// "non-interactive" branch must surface "no projects" cleanly.
func TestKnownProjects_EmptyHomeReturnsEmpty(t *testing.T) {
	withFleetHome(t)
	names, err := KnownProjects()
	if err != nil {
		t.Fatalf("KnownProjects: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("KnownProjects: got %v want []", names)
	}
}

// TestKnownProjects_ListsValidDirsAlphabetically — picker output must
// be deterministic; sort by name so "[1] alpha [2] bravo ..." matches
// the test's exact-string expectations.
func TestKnownProjects_ListsValidDirsAlphabetically(t *testing.T) {
	home := withFleetHome(t)
	mkProjectDir(t, home, "fleet")
	mkProjectDir(t, home, "alpha")
	mkProjectDir(t, home, "projects-fleet")
	names, err := KnownProjects()
	if err != nil {
		t.Fatalf("KnownProjects: %v", err)
	}
	want := []string{"alpha", "fleet", "projects-fleet"}
	if len(names) != len(want) {
		t.Fatalf("KnownProjects: got %v want %v", names, want)
	}
	for i, n := range want {
		if names[i] != n {
			t.Errorf("KnownProjects[%d]: got %q want %q", i, names[i], n)
		}
	}
}

// TestKnownProjects_SkipsReservedAndMalformed — .locks (reserved),
// dotfiles, and names that fail state.ValidateProjectName (e.g.
// "--project" from a CLI flag-misparse) must not be returned. Matches
// the dashboard's surfaceMalformedProjects discipline.
func TestKnownProjects_SkipsReservedAndMalformed(t *testing.T) {
	home := withFleetHome(t)
	mkProjectDir(t, home, "fleet")
	mkProjectDir(t, home, ".locks")
	mkProjectDir(t, home, ".hidden")
	mkProjectDir(t, home, "--project") // CLI flag misparse
	names, err := KnownProjects()
	if err != nil {
		t.Fatalf("KnownProjects: %v", err)
	}
	if len(names) != 1 || names[0] != "fleet" {
		t.Errorf("KnownProjects: got %v want [fleet]", names)
	}
}

// --- FindLiveCoord ---

// TestFindLiveCoord_HitOnTaskIDAndProject — a record tagged
// coord-<project> + project=<project> + live tmux session is the
// answer. Critical for Tier 3 path "live coord exists → attach".
func TestFindLiveCoord_HitOnTaskIDAndProject(t *testing.T) {
	withFleetHome(t)
	writeAgentRec(t, "aaaaaaaa", "fleet", "coord-fleet")
	records := recordsFromList(t)
	// Stub session probe → alive for fleet-aaaaaaaa.
	restore := stubSessionAlive(t, map[string]bool{"fleet-aaaaaaaa": true})
	defer restore()
	got, ok := FindLiveCoord(records, "fleet")
	if !ok {
		t.Fatal("FindLiveCoord: ok=false want true")
	}
	if got.ID != "aaaaaaaa" {
		t.Errorf("FindLiveCoord: got %s want aaaaaaaa", got.ID)
	}
}

// TestFindLiveCoord_DeadSessionMisses — record is tagged correctly
// but tmux has-session reports no such session → ok=false. Drives
// the failover into the stale-coord reap+respawn branch.
func TestFindLiveCoord_DeadSessionMisses(t *testing.T) {
	withFleetHome(t)
	writeAgentRec(t, "aaaaaaaa", "fleet", "coord-fleet")
	records := recordsFromList(t)
	restore := stubSessionAlive(t, map[string]bool{}) // empty map = all dead
	defer restore()
	if _, ok := FindLiveCoord(records, "fleet"); ok {
		t.Errorf("FindLiveCoord: ok=true want false (session dead)")
	}
}

// TestFindLiveCoord_NoMarkerRequirement — the TUI's actionAttachProject
// gates [a]-dedup on the coord-spawn marker so a failed-prompt dispatch
// re-spawns instead of dropping the operator in a plain Claude shell.
// Tier 3 PROJECT RECOVERY does NOT have that requirement: by the time
// we're in failover, any live coord for the project is acceptable.
func TestFindLiveCoord_NoMarkerRequirement(t *testing.T) {
	withFleetHome(t)
	writeAgentRec(t, "aaaaaaaa", "fleet", "coord-fleet")
	// Intentionally do NOT write a coord-spawn marker.
	records := recordsFromList(t)
	restore := stubSessionAlive(t, map[string]bool{"fleet-aaaaaaaa": true})
	defer restore()
	if _, ok := FindLiveCoord(records, "fleet"); !ok {
		t.Errorf("FindLiveCoord without marker: ok=false want true (Tier 3 does not gate on marker)")
	}
}

// TestFindLiveCoord_WrongProjectMisses — record tagged
// coord-other-project must not match "fleet" lookup.
func TestFindLiveCoord_WrongProjectMisses(t *testing.T) {
	withFleetHome(t)
	writeAgentRec(t, "aaaaaaaa", "other", "coord-other")
	records := recordsFromList(t)
	restore := stubSessionAlive(t, map[string]bool{"fleet-aaaaaaaa": true})
	defer restore()
	if _, ok := FindLiveCoord(records, "fleet"); ok {
		t.Errorf("FindLiveCoord: matched wrong project")
	}
}

// TestFindLiveCoordPreferPID_PicksMatchingSupervisor — review adversarial-
// subagent finding: a botched non-graceful swap can leave TWO live-session
// records for the same project+coord-task (an orphaned OLD alongside the
// real NEW; atomic_coord_swap.go's ErrOrphanSurvived/ErrOldKillProbeAmbiguous
// deliberately preserve OLD's record + session rather than force-killing on
// an ambiguous probe). Plain first-match (sorted by ID) would land on
// whichever sorts first, which may be the orphan, not the live flock holder.
// FindLiveCoordPreferPID must pick the record whose SupervisorPID+
// SupervisorPidStart both match the ACTUAL flock holder when that identity
// is known (attach's D9e path reads it via coordlock.LiveOwner even when the
// body's AgentID itself is unreadable).
func TestFindLiveCoordPreferPID_PicksMatchingSupervisor(t *testing.T) {
	withFleetHome(t)
	old := writeAgentRec(t, "aaaaaaaa", "fleet", "coord-fleet") // sorts first by ID
	old.SupervisorPID = 111
	old.SupervisorPidStart = 1110
	if err := old.Write(); err != nil {
		t.Fatal(err)
	}
	newRec := writeAgentRec(t, "bbbbbbbb", "fleet", "coord-fleet") // sorts second by ID
	newRec.SupervisorPID = 222
	newRec.SupervisorPidStart = 2220
	if err := newRec.Write(); err != nil {
		t.Fatal(err)
	}
	records := recordsFromList(t)
	restore := stubSessionAlive(t, map[string]bool{"fleet-aaaaaaaa": true, "fleet-bbbbbbbb": true})
	defer restore()

	// Sanity: plain FindLiveCoord (no PID hint) picks the first-sorted record
	// (the orphan) — this is the exact hazard the PID-aware variant closes.
	if got, ok := FindLiveCoord(records, "fleet"); !ok || got.ID != "aaaaaaaa" {
		t.Fatalf("sanity: FindLiveCoord = %v ok=%v, want aaaaaaaa (first-match baseline)", got, ok)
	}

	// The real flock holder's identity is pid=222/pid_start=2220 (newRec) —
	// must be preferred over the naive first-match.
	got, ok := FindLiveCoordPreferPID(records, "fleet", 222, 2220)
	if !ok {
		t.Fatal("FindLiveCoordPreferPID: ok=false, want true")
	}
	if got.ID != "bbbbbbbb" {
		t.Fatalf("FindLiveCoordPreferPID: got %s, want bbbbbbbb (the actual flock holder, not the orphan)", got.ID)
	}
}

// TestFindLiveCoordPreferPID_PidReuseMismatchRejected is codex confirm round
// [P2] #4: PIDs get reused by the OS, so a stale/orphan record can
// coincidentally carry the SAME (recycled) pid as the real live holder while
// being a completely different process. Matching on SupervisorPID alone
// would wrongly accept it; pid_start (the process's actual start time) must
// also match, mirroring this codebase's identity convention everywhere else
// (HandoffSuccessorAlive, KillCoordIfIdentityMatches, LeaseCheckByAncestor).
func TestFindLiveCoordPreferPID_PidReuseMismatchRejected(t *testing.T) {
	withFleetHome(t)
	stale := writeAgentRec(t, "aaaaaaaa", "fleet", "coord-fleet")
	stale.SupervisorPID = 555      // SAME pid as the real holder below...
	stale.SupervisorPidStart = 111 // ...but a DIFFERENT (older) start time — a recycled pid.
	if err := stale.Write(); err != nil {
		t.Fatal(err)
	}
	records := recordsFromList(t)
	restore := stubSessionAlive(t, map[string]bool{"fleet-aaaaaaaa": true})
	defer restore()

	// The real holder is pid=555, but pid_start=222 (freshly started) — does
	// NOT match the stale record's pid_start=111.
	if got, ok := FindLiveCoordPreferPID(records, "fleet", 555, 222); ok {
		t.Fatalf("FindLiveCoordPreferPID matched a recycled pid with a different pid_start: got %v ok=%v, want ok=false", got, ok)
	}
}

// TestFindLiveCoordPreferPID_NoMatchIsStrict — a preferredPID>0 that matches
// no candidate's SupervisorPID+SupervisorPidStart must return ok=false
// (codex confirm round [P2]: a confirmed identity is real evidence and must
// never degrade into a guess at an unrelated record — that reproduces the
// exact orphan-attach hazard this function exists to close). preferredPID<=0
// (no cross-check available) is the one case that still falls back to
// first-match — identical to plain FindLiveCoord.
func TestFindLiveCoordPreferPID_NoMatchIsStrict(t *testing.T) {
	withFleetHome(t)
	writeAgentRec(t, "aaaaaaaa", "fleet", "coord-fleet")
	records := recordsFromList(t)
	restore := stubSessionAlive(t, map[string]bool{"fleet-aaaaaaaa": true})
	defer restore()

	if got, ok := FindLiveCoordPreferPID(records, "fleet", 999999, 424242); ok {
		t.Fatalf("FindLiveCoordPreferPID with no matching identity = %v ok=%v, want ok=false (no guessing)", got, ok)
	}
	if got, ok := FindLiveCoordPreferPID(records, "fleet", 0, 0); !ok || got.ID != "aaaaaaaa" {
		t.Fatalf("FindLiveCoordPreferPID with preferredPID<=0 = %v ok=%v, want aaaaaaaa (identical to FindLiveCoord)", got, ok)
	}
}

// TestFindLiveCoord_EmptyTmuxSession_ReturnsSynthesized — codex review
// iter-3 P2 #2. Legacy records may have TmuxSession=="" on disk; the
// helper probes the synthesized "fleet-<id>" and treats a live session
// there as a match. The returned record MUST have TmuxSession populated
// — TUI callers assign `pendingAttach = rec.TmuxSession` and would quit
// with nothing to attach if the field were left empty. Original record
// stays untouched (defensive copy).
func TestFindLiveCoord_EmptyTmuxSession_ReturnsSynthesized(t *testing.T) {
	withFleetHome(t)
	// Bootstrap agents dir + write a record with TmuxSession="".
	if dir, err := state.AgentDir(); err == nil {
		_ = os.MkdirAll(dir, 0o755)
	}
	r := agent.New("12345678")
	r.Project = "fleet"
	r.TaskID = "coord-fleet"
	r.TmuxSession = "" // legacy: empty session field on disk
	if err := r.Write(); err != nil {
		t.Fatal(err)
	}
	records := recordsFromList(t)
	restore := stubSessionAlive(t, map[string]bool{"fleet-12345678": true})
	defer restore()
	got, ok := FindLiveCoord(records, "fleet")
	if !ok {
		t.Fatal("FindLiveCoord: expected hit on synthesized session")
	}
	if got.TmuxSession != "fleet-12345678" {
		t.Errorf("FindLiveCoord: returned TmuxSession=%q want fleet-12345678 (must be normalized)", got.TmuxSession)
	}
	// Defensive copy: original record must still have empty session so
	// the caller can detect "was-synthesized" if needed.
	for _, rr := range records {
		if rr.ID == "12345678" && rr.TmuxSession != "" {
			t.Errorf("FindLiveCoord mutated input record TmuxSession=%q", rr.TmuxSession)
		}
	}
}

// TestFindCoordByLockBody_EmptyTmuxSession_ReturnsSynthesized — same
// invariant as FindLiveCoord above, applied to the lock-body path.
func TestFindCoordByLockBody_EmptyTmuxSession_ReturnsSynthesized(t *testing.T) {
	home := withFleetHome(t)
	if dir, err := state.AgentDir(); err == nil {
		_ = os.MkdirAll(dir, 0o755)
	}
	r := agent.New("12345678")
	r.Project = "fleet"
	r.TmuxSession = ""
	if err := r.Write(); err != nil {
		t.Fatal(err)
	}
	writeCoordLockBody(t, home, "fleet", "12345678")
	records := recordsFromList(t)
	restore := stubSessionAlive(t, map[string]bool{"fleet-12345678": true})
	defer restore()
	got, ok := FindCoordByLockBody(records, "fleet")
	if !ok {
		t.Fatal("FindCoordByLockBody: expected hit")
	}
	if got.TmuxSession != "fleet-12345678" {
		t.Errorf("FindCoordByLockBody: returned TmuxSession=%q want fleet-12345678", got.TmuxSession)
	}
}

// --- FindCoordByLockBody ---

// TestFindCoordByLockBody_ReadsLockBodyAndMatchesRecord — the lock
// body holds the ID of the coord that acquired the project's flock;
// match it against records with a live tmux session.
func TestFindCoordByLockBody_ReadsLockBodyAndMatchesRecord(t *testing.T) {
	home := withFleetHome(t)
	writeAgentRec(t, "bbbbbbbb", "fleet", "")
	writeCoordLockBody(t, home, "fleet", "bbbbbbbb")
	records := recordsFromList(t)
	restore := stubSessionAlive(t, map[string]bool{"fleet-bbbbbbbb": true})
	defer restore()
	got, ok := FindCoordByLockBody(records, "fleet")
	if !ok {
		t.Fatal("FindCoordByLockBody: ok=false want true")
	}
	if got.ID != "bbbbbbbb" {
		t.Errorf("FindCoordByLockBody: got %s want bbbbbbbb", got.ID)
	}
}

// TestFindCoordByLockBody_EmptyBodyMisses — coordinator.lock is
// missing / empty → no result, no panic.
func TestFindCoordByLockBody_EmptyBodyMisses(t *testing.T) {
	withFleetHome(t)
	writeAgentRec(t, "bbbbbbbb", "fleet", "")
	records := recordsFromList(t)
	restore := stubSessionAlive(t, map[string]bool{"fleet-bbbbbbbb": true})
	defer restore()
	if _, ok := FindCoordByLockBody(records, "fleet"); ok {
		t.Errorf("FindCoordByLockBody: ok=true without lock body")
	}
}

// TestFindCoordByLockBody_DeadSessionMisses — body names an agent
// whose tmux session is gone (coord crashed, body not yet stale-
// cleaned). Misses so failover continues into reap+respawn.
func TestFindCoordByLockBody_DeadSessionMisses(t *testing.T) {
	home := withFleetHome(t)
	writeAgentRec(t, "bbbbbbbb", "fleet", "")
	writeCoordLockBody(t, home, "fleet", "bbbbbbbb")
	records := recordsFromList(t)
	restore := stubSessionAlive(t, map[string]bool{}) // all dead
	defer restore()
	if _, ok := FindCoordByLockBody(records, "fleet"); ok {
		t.Errorf("FindCoordByLockBody: ok=true with dead session")
	}
}

// TestFindCoordByLockBody_CrossProjectRecordMisses — codex P2 regression.
// Project A's coordinator.lock holds an ID whose record is tagged for a
// DIFFERENT project B (stale/copied/reused ID). FindCoordByLockBody must
// NOT return B's coord for an attach --project A — that would attach the
// operator to the wrong project. The project-match guard makes it miss so
// Tier 3 falls through to a fresh spawn for A.
func TestFindCoordByLockBody_CrossProjectRecordMisses(t *testing.T) {
	home := withFleetHome(t)
	// Record is tagged project "otherproj"; lock body for "fleet" names it.
	writeAgentRec(t, "bbbbbbbb", "otherproj", "")
	writeCoordLockBody(t, home, "fleet", "bbbbbbbb")
	records := recordsFromList(t)
	restore := stubSessionAlive(t, map[string]bool{"fleet-bbbbbbbb": true}) // live
	defer restore()
	if got, ok := FindCoordByLockBody(records, "fleet"); ok {
		t.Errorf("FindCoordByLockBody: must NOT return cross-project record; got %s for project=%s", got.ID, got.Project)
	}
}

// --- OrphanTmuxForProject ---

// TestOrphanTmuxForProject_OrphanSessionFound — tmux has a fleet-<id>
// session with no live record. After codex iter-4 P1 the helper also
// requires an archived record tying the orphan ID to the requested
// project; otherwise cross-project reap is too easy. Stub the archive
// lookup to claim the orphan belonged to "fleet".
func TestOrphanTmuxForProject_OrphanSessionFound(t *testing.T) {
	withFleetHome(t)
	records := recordsFromList(t) // empty — no live record for deadbeef
	restoreList := stubListSessions(t, []string{"fleet-deadbeef", "other-thing"})
	defer restoreList()
	// Bind the orphan to the requested project's COORD lineage via the
	// archive seam. The coord-only reap guard (codex P2) requires
	// task_id == coord-<project>.
	restoreArch := SetLoadArchiveStub(func(id string) (*agent.Record, error) {
		if id == "deadbeef" {
			r := &agent.Record{ID: "deadbeef", Project: "fleet", TaskID: CoordTaskID("fleet")}
			return r, nil
		}
		return nil, errors.New("not found")
	})
	defer restoreArch()
	got, ok := OrphanTmuxForProject(records, "fleet")
	if !ok {
		t.Fatal("OrphanTmuxForProject: ok=false want true")
	}
	if got != "deadbeef" {
		t.Errorf("OrphanTmuxForProject: got %q want deadbeef", got)
	}
}

// TestOrphanTmuxForProject_RejectsCrossProjectOrphan — codex review
// iter-4 P1 guard. The orphan tmux session belongs to a DIFFERENT
// project (per its archive); attach failover for project A must NOT
// claim it and must NOT trigger gc that could reap it.
func TestOrphanTmuxForProject_RejectsCrossProjectOrphan(t *testing.T) {
	withFleetHome(t)
	records := recordsFromList(t)
	restoreList := stubListSessions(t, []string{"fleet-deadbeef"})
	defer restoreList()
	// Archive ties the orphan to project B; caller asks about A.
	restoreArch := SetLoadArchiveStub(func(id string) (*agent.Record, error) {
		if id == "deadbeef" {
			r := &agent.Record{ID: "deadbeef", Project: "other-project"}
			return r, nil
		}
		return nil, errors.New("not found")
	})
	defer restoreArch()
	if _, ok := OrphanTmuxForProject(records, "fleet"); ok {
		t.Errorf("OrphanTmuxForProject: must reject cross-project orphan; reaping it would terminate another project's session")
	}
}

// TestOrphanTmuxForProject_RejectsUnknownProvenanceOrphan — same guard
// as above for orphans with NO archive record. Without provenance we
// can't safely tie the session to projectName; surface "no" so the
// caller falls through to spawn-fresh instead of running gc that wouldn't
// help anyway.
func TestOrphanTmuxForProject_RejectsUnknownProvenanceOrphan(t *testing.T) {
	withFleetHome(t)
	records := recordsFromList(t)
	restoreList := stubListSessions(t, []string{"fleet-deadbeef"})
	defer restoreList()
	// Archive lookup fails — no provenance.
	restoreArch := SetLoadArchiveStub(func(id string) (*agent.Record, error) {
		return nil, errors.New("not found")
	})
	defer restoreArch()
	if _, ok := OrphanTmuxForProject(records, "fleet"); ok {
		t.Errorf("OrphanTmuxForProject: must reject orphan of unknown provenance")
	}
}

// TestOrphanTmuxForProject_RejectsSameProjectWorkerOrphan — codex P2
// regression. The orphan's archive is tagged for the SAME project but is
// a WORKER (task_id != coord-<project>), not the coord. Tier 3 Path C'
// KILLS the returned session, so returning a worker orphan would let an
// unrelated coord attach terminate a worker's session. The coord-only
// guard must reject it → caller falls through to spawn-fresh (Path D).
func TestOrphanTmuxForProject_RejectsSameProjectWorkerOrphan(t *testing.T) {
	withFleetHome(t)
	records := recordsFromList(t)
	restoreList := stubListSessions(t, []string{"fleet-deadbeef"})
	defer restoreList()
	// Same project, but a worker task_id — NOT the coord lineage.
	restoreArch := SetLoadArchiveStub(func(id string) (*agent.Record, error) {
		if id == "deadbeef" {
			return &agent.Record{ID: "deadbeef", Project: "fleet", TaskID: "fix-bug-1234"}, nil
		}
		return nil, errors.New("not found")
	})
	defer restoreArch()
	if _, ok := OrphanTmuxForProject(records, "fleet"); ok {
		t.Errorf("OrphanTmuxForProject: must reject same-project WORKER orphan (only coord lineage is a reap target); killing it would terminate a worker's session")
	}
}

// TestOrphanTmuxForProject_NoOrphans — sessions all have matching
// records → ok=false (nothing for the reap branch to clean).
func TestOrphanTmuxForProject_NoOrphans(t *testing.T) {
	withFleetHome(t)
	writeAgentRec(t, "aaaaaaaa", "fleet", "coord-fleet")
	records := recordsFromList(t)
	restore := stubListSessions(t, []string{"fleet-aaaaaaaa"})
	defer restore()
	if _, ok := OrphanTmuxForProject(records, "fleet"); ok {
		t.Errorf("OrphanTmuxForProject: must skip sessions with matching records")
	}
}

// --- shared helpers ---

// stubSessionAlive replaces sessionAliveFn for the test. The package
// exposes that var (TUI does the same) so unit tests don't need a
// real tmux server.
func stubSessionAlive(t *testing.T, alive map[string]bool) func() {
	t.Helper()
	prev := sessionAliveFn
	sessionAliveFn = func(s string) bool { return alive[s] }
	prevProbe := sessionProbeFn
	sessionProbeFn = func(s string) (bool, error) { return alive[s], nil }
	return func() {
		sessionAliveFn = prev
		sessionProbeFn = prevProbe
	}
}

// stubListSessions replaces listSessionsFn for the test.
func stubListSessions(t *testing.T, sessions []string) func() {
	t.Helper()
	prev := listSessionsFn
	listSessionsFn = func() ([]string, error) { return sessions, nil }
	return func() { listSessionsFn = prev }
}

// helper to keep the imports honest — referenced by indirect helpers.
var _ = errors.New
var _ = json.Marshal
