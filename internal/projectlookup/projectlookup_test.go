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
//     Project [a]-dedup helper) because Tier 3 is failover, not
//     duplicate-spawn protection — when failover lands, ANY live
//     coord for the project is acceptable.
//   - FindCoordByLockBody: extract the ID from
//     ~/.fleet/projects/<name>/.locks/coordinator.lock and match
//     against records with a live tmux session.
//   - StaleCoordRecord: a record tagged coord-<project> whose tmux
//     session is definitively dead. Drives the "record alive but
//     tmux missing" stale-detection branch in attach failover.
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

// --- StaleCoordRecord ---

// TestStaleCoordRecord_RecordAliveTmuxDeadIsStale — the "record
// alive but tmux missing" branch of Tier 3.
func TestStaleCoordRecord_RecordAliveTmuxDeadIsStale(t *testing.T) {
	withFleetHome(t)
	writeAgentRec(t, "cccccccc", "fleet", "coord-fleet")
	records := recordsFromList(t)
	restore := stubSessionAlive(t, map[string]bool{}) // tmux dead
	defer restore()
	stale, ok := StaleCoordRecord(records, "fleet")
	if !ok {
		t.Fatal("StaleCoordRecord: ok=false want true (record alive, tmux dead)")
	}
	if stale.ID != "cccccccc" {
		t.Errorf("StaleCoordRecord: got %s want cccccccc", stale.ID)
	}
}

// TestStaleCoordRecord_RecordPlusLiveTmuxIsNotStale — guard against
// reaping a live coord. record-alive + tmux-alive must report ok=false
// so the failover funnel never reaps a live coord under the "stale"
// branch.
func TestStaleCoordRecord_RecordPlusLiveTmuxIsNotStale(t *testing.T) {
	withFleetHome(t)
	writeAgentRec(t, "cccccccc", "fleet", "coord-fleet")
	records := recordsFromList(t)
	restore := stubSessionAlive(t, map[string]bool{"fleet-cccccccc": true})
	defer restore()
	if _, ok := StaleCoordRecord(records, "fleet"); ok {
		t.Errorf("StaleCoordRecord: must NOT mark live coord stale")
	}
}

// --- OrphanTmuxForProject ---

// TestOrphanTmuxForProject_OrphanSessionFound — tmux has a fleet-<id>
// session with no matching record. Drives the "no record but lingering
// tmux" stale branch.
func TestOrphanTmuxForProject_OrphanSessionFound(t *testing.T) {
	withFleetHome(t)
	// No agent record — but a tmux session "fleet-deadbeef" exists.
	// The orphan helper only needs a stub for ListSessions; the
	// project name is hint-only (Tier 3 doesn't currently bind
	// tmux sessions to projects, so the helper returns the orphan
	// list and the caller treats any one as the stale).
	records := recordsFromList(t) // empty
	restore := stubListSessions(t, []string{"fleet-deadbeef", "other-thing"})
	defer restore()
	got, ok := OrphanTmuxForProject(records, "fleet")
	if !ok {
		t.Fatal("OrphanTmuxForProject: ok=false want true")
	}
	if got != "deadbeef" {
		t.Errorf("OrphanTmuxForProject: got %q want deadbeef", got)
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
