package main

// Tests for the dead-coord recovery surface added in
// resume-dead-coord-ab65: detection (findRecoveryCandidate) + the
// dispatch-side wiring that synthesizes a handoff doc from on-disk
// state and points the successor at it.

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/handoff"
	"github.com/edisonshen/fleet/internal/handoffdelivery"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tasks"
	"github.com/edisonshen/fleet/internal/tmux"
)

// fakeAgentRecord builds a minimal agent.Record for the recovery-detection
// helper's tests. Inline-built so we don't drag in cross-package factories.
func fakeAgentRecord(id, taskID, project string, pid int, session string) *agent.Record {
	r := agent.New(id)
	r.TaskID = taskID
	r.Project = project
	r.PID = pid
	r.TmuxSession = session
	return r
}

// seedRecoveryRepo writes a meta.json under projects/<project> pinning
// repo_path to a real temp directory, so coord recovery's repo binding
// (DESIGN-coord-repo-binding-from-project.md PR3) resolves via tier 1
// instead of refusing. Returns the bound repo path. The recovery-
// inheritance tests below verify command/engine/prompt behavior, not the
// binding itself, so they just need the resolver to succeed. The cwd-
// binding behavior is covered separately by E4/E5.
func seedRecoveryRepo(t *testing.T, root, project string) string {
	t.Helper()
	repo := t.TempDir()
	pdir := filepath.Join(root, "projects", project)
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}
	meta := `{"schema":"v1","is_git":false,"repo_path":"` + repo + `","added_at":"2026-01-01T00:00:00Z"}`
	if err := os.WriteFile(filepath.Join(pdir, "meta.json"), []byte(meta), 0o644); err != nil {
		t.Fatalf("write meta.json: %v", err)
	}
	return repo
}

// TestFindRecoveryCandidate_DeadPidAndDeadTmuxIsCandidate pins the core
// detection rule: when a record's pid is dead AND its tmux session is
// gone, the helper returns that record as a recovery candidate.
func TestFindRecoveryCandidate_DeadPidAndDeadTmuxIsCandidate(t *testing.T) {
	records := []*agent.Record{
		fakeAgentRecord("aaaaaaaa", "coord-myproj", "myproj", 99999, "fleet-aaaaaaaa"),
	}
	pidAlive := func(int) bool { return false }
	sessionAlive := func(string) bool { return false }

	coordFresh := func(string) bool { return false } // coord-state.json stale → dead
	got := findRecoveryCandidate("coord-myproj", "myproj", records, pidAlive, sessionAlive, coordFresh)
	if got == nil {
		t.Fatalf("expected recovery candidate; got nil")
	}
	if got.ID != "aaaaaaaa" {
		t.Errorf("candidate ID = %q; want aaaaaaaa", got.ID)
	}
}

// TestFindRecoveryCandidate_PidNoLongerVetoes pins codex review iter-11
// P2: agent.Record.PID is the short-lived dispatch CLI's pid (set in
// spawn.Spawn via os.Getpid), so after that pid is reused by an
// unrelated host process pidAlive(r.PID) would return true and
// suppress recovery — losing the dead coord's resume context. The
// pidAlive gate is therefore dropped from findRecoveryCandidate's
// body; tmux-aliveness on the local socket is the only signal.
// pidAlive remains in the signature for backwards-compat with tests
// that pre-date this iteration.
func TestFindRecoveryCandidate_PidNoLongerVetoes(t *testing.T) {
	records := []*agent.Record{
		fakeAgentRecord("aaaaaaaa", "coord-myproj", "myproj", 99999, "fleet-aaaaaaaa"),
	}
	pidAlive := func(int) bool { return true }         // pretend pid alive (e.g. recycled)
	sessionAlive := func(string) bool { return false } // tmux gone

	coordFresh := func(string) bool { return false }
	got := findRecoveryCandidate("coord-myproj", "myproj", records, pidAlive, sessionAlive, coordFresh)
	if got == nil {
		t.Fatalf("PID alive must no longer veto recovery (recycled pid case); got nil candidate")
	}
	if got.ID != "aaaaaaaa" {
		t.Errorf("candidate ID = %q; want aaaaaaaa", got.ID)
	}
}

// TestFindRecoveryCandidate_AliveTmuxIsNotCandidate pins the other
// half of the safety gate: alive tmux with dead pid is the
// pre-existing zombie-session case — `fleet attach` would land you
// in an orphan shell. Not a recovery candidate; the dispatch path
// must let the existing fail-loud handling (issue #65) cover it.
func TestFindRecoveryCandidate_AliveTmuxIsNotCandidate(t *testing.T) {
	records := []*agent.Record{
		fakeAgentRecord("aaaaaaaa", "coord-myproj", "myproj", 99999, "fleet-aaaaaaaa"),
	}
	pidAlive := func(int) bool { return false }
	sessionAlive := func(string) bool { return true } // tmux still up

	coordFresh := func(string) bool { return false } // coord-state.json stale → dead
	got := findRecoveryCandidate("coord-myproj", "myproj", records, pidAlive, sessionAlive, coordFresh)
	if got != nil {
		t.Errorf("alive-tmux candidate must NOT be returned; got %+v", got)
	}
}

// TestFindRecoveryCandidate_NameMustMatch pins task-id and project
// scoping. A dead coord for project A is NOT a recovery candidate for
// project B, even though both records are dead. Cross-project bleed
// would dispatch a successor that picks up another project's workers.
func TestFindRecoveryCandidate_NameMustMatch(t *testing.T) {
	records := []*agent.Record{
		fakeAgentRecord("aaaaaaaa", "coord-other", "other", 99999, "fleet-aaaaaaaa"),
	}
	pidAlive := func(int) bool { return false }
	sessionAlive := func(string) bool { return false }

	coordFresh := func(string) bool { return false } // coord-state.json stale → dead
	got := findRecoveryCandidate("coord-myproj", "myproj", records, pidAlive, sessionAlive, coordFresh)
	if got != nil {
		t.Errorf("cross-project candidate must NOT be returned; got %+v", got)
	}
}

// TestFindRecoveryCandidate_MultipleDeadPicksNewest pins codex review
// iter-7 P2: when multiple stale records share the same task_id (e.g.,
// a prior recovery itself crashed before archiving its predecessor),
// findRecoveryCandidate picks the most-recently-spawned record. An
// arbitrary first-match would inherit cwd / engine / handoff-chain
// from an older lineage and restart in the wrong checkout. The caller
// archives the picked record post-spawn so the next dispatch finds
// at most one — but until that archive lands, we pick the newest.
func TestFindRecoveryCandidate_MultipleDeadPicksNewest(t *testing.T) {
	oldest := fakeAgentRecord("oldest01", "coord-myproj", "myproj", 11111, "fleet-oldest01")
	oldest.SpawnedAt = time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	newest := fakeAgentRecord("newest02", "coord-myproj", "myproj", 22222, "fleet-newest02")
	newest.SpawnedAt = time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	// Deliberately place "oldest" first in slice order so the test
	// pins SpawnedAt-based picking, not slice-position.
	records := []*agent.Record{oldest, newest}
	pidAlive := func(int) bool { return false }
	sessionAlive := func(string) bool { return false }

	coordFresh := func(string) bool { return false } // coord-state.json stale → dead
	got := findRecoveryCandidate("coord-myproj", "myproj", records, pidAlive, sessionAlive, coordFresh)
	if got == nil {
		t.Fatalf("expected one candidate; got nil")
	}
	if got.ID != "newest02" {
		t.Errorf("candidate ID = %q; want newest02 (highest SpawnedAt wins)", got.ID)
	}
}

// TestFindRecoveryCandidate_FreshCoordStateStillCandidate pins codex
// review iter-9 P1: a dead record with fresh coord-state.json mtime
// MUST still be a recovery candidate. The fresh-mtime case is
// ambiguous (live-on-different-socket OR recently-crashed); the
// dispatch-side veto (runDispatch + liveCoordRecordExists) handles
// the live case via the dual-signal check. findRecoveryCandidate's
// job is narrower — identify dead records to recover, regardless of
// mtime. Without this, recently-crashed coords would be denied
// synth-handoff recovery (the exact feature we're shipping).
func TestFindRecoveryCandidate_FreshCoordStateStillCandidate(t *testing.T) {
	records := []*agent.Record{
		fakeAgentRecord("aaaaaaaa", "coord-myproj", "myproj", 99999, "fleet-aaaaaaaa"),
	}
	pidAlive := func(int) bool { return false }        // dispatch CLI pid reaped
	sessionAlive := func(string) bool { return false } // tmux session gone
	coordFresh := func(string) bool { return true }    // mtime is fresh — but irrelevant here

	got := findRecoveryCandidate("coord-myproj", "myproj", records, pidAlive, sessionAlive, coordFresh)
	if got == nil {
		t.Fatalf("dead record with fresh mtime must still be a recovery candidate (recent crash); got nil")
	}
	if got.ID != "aaaaaaaa" {
		t.Errorf("candidate ID = %q; want aaaaaaaa", got.ID)
	}
}

// TestCoordStateFresh_RecentMtimeReturnsTrue pins the production probe:
// when coord-state.json exists AND its mtime is within
// coordFreshnessWindow, coordStateFresh returns true so the live coord
// is protected from recovery.
func TestCoordStateFresh_RecentMtimeReturnsTrue(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEET_HOME", root)
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("state.Bootstrap: %v", err)
	}
	pdir, err := state.ProjectDir("myproj")
	if err != nil {
		t.Fatalf("ProjectDir: %v", err)
	}
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}
	csPath := filepath.Join(pdir, "coord-state.json")
	if err := os.WriteFile(csPath, []byte("{}"), 0o644); err != nil {
		t.Fatalf("write coord-state: %v", err)
	}
	// Just-now mtime is within the window by definition.
	if !coordStateFresh("myproj") {
		t.Errorf("coordStateFresh must return true for a just-written coord-state.json")
	}
}

// TestCoordStateFresh_StaleMtimeReturnsFalse pins the inverse: when
// coord-state.json's mtime is older than coordFreshnessWindow, the
// coord is treated as dead and recovery proceeds.
func TestCoordStateFresh_StaleMtimeReturnsFalse(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEET_HOME", root)
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("state.Bootstrap: %v", err)
	}
	pdir, err := state.ProjectDir("myproj")
	if err != nil {
		t.Fatalf("ProjectDir: %v", err)
	}
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}
	csPath := filepath.Join(pdir, "coord-state.json")
	if err := os.WriteFile(csPath, []byte("{}"), 0o644); err != nil {
		t.Fatalf("write coord-state: %v", err)
	}
	// Backdate mtime well past coordFreshnessWindow (5m).
	stale := time.Now().Add(-2 * coordFreshnessWindow)
	if err := os.Chtimes(csPath, stale, stale); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	if coordStateFresh("myproj") {
		t.Errorf("coordStateFresh must return false for a stale-mtime coord-state.json")
	}
}

// TestCoordStateFresh_MissingFileReturnsFalse covers the fresh-project
// case: coord-state.json doesn't exist (no coord ever ran), so
// coordStateFresh reports "not live" and the caller proceeds.
func TestCoordStateFresh_MissingFileReturnsFalse(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEET_HOME", root)
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("state.Bootstrap: %v", err)
	}

	if coordStateFresh("freshproj") {
		t.Errorf("coordStateFresh on a missing coord-state.json must return false")
	}
}

// TestWriteRecoveryHandoffDoc_PopulatesOpenPRsFromGH pins codex review
// iter-18 P1: the synth handoff doc must carry an Open PRs section
// pulled from `gh pr list` so the successor coord re-spawns shepherd
// until-loops for in-review workers. Without this, handoff_resume.py
// skips re-dispatching in-review workers (their status says skip)
// AND there's no shepherd hint either — the PR is dropped on the floor.
// Mirrors skills/fleet-guard/handoff.py:_collect_open_prs.
func TestWriteRecoveryHandoffDoc_PopulatesOpenPRsFromGH(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEET_HOME", root)
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("state.Bootstrap: %v", err)
	}

	// Stub gh's PR list output so the test doesn't depend on a real
	// gh CLI or GitHub auth.
	prev := collectOpenPRs
	collectOpenPRs = func(string) []handoff.OpenPR {
		return []handoff.OpenPR{
			{Number: 42, Title: "fix: foo", HeadRefName: "worker/fix-foo-1234", URL: "https://github.com/owner/repo/pull/42"},
			{Number: 43, Title: "feat: bar", HeadRefName: "worker/feat-bar-5678", URL: "https://github.com/owner/repo/pull/43"},
		}
	}
	t.Cleanup(func() { collectOpenPRs = prev })

	deadRec := fakeAgentRecord("ghprtest", "coord-myproj", "myproj", 11111, "fleet-ghprtest")
	if err := deadRec.Write(); err != nil {
		t.Fatalf("write dead record: %v", err)
	}
	pdir := filepath.Join(root, "projects", "myproj")
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cs := map[string]any{"worker_agent_ids": map[string]string{}}
	csData, _ := json.Marshal(cs)
	if err := os.WriteFile(filepath.Join(pdir, "coord-state.json"), csData, 0o644); err != nil {
		t.Fatalf("write coord-state: %v", err)
	}

	docPath, err := writeRecoveryHandoffDoc(deadRec, time.Now().UTC())
	if err != nil {
		t.Fatalf("writeRecoveryHandoffDoc: %v", err)
	}
	body, rerr := os.ReadFile(docPath)
	if rerr != nil {
		t.Fatalf("read synth doc: %v", rerr)
	}
	if !strings.Contains(string(body), "#42 fix: foo") {
		t.Errorf("synth doc must include PR #42 from gh output; got body:\n%s", body)
	}
	if !strings.Contains(string(body), "#43 feat: bar") {
		t.Errorf("synth doc must include PR #43 from gh output; got body:\n%s", body)
	}
	if !strings.Contains(string(body), "https://github.com/owner/repo/pull/42") {
		t.Errorf("synth doc must include PR URL for shepherd respawn; got body:\n%s", body)
	}
}

// TestWriteRecoveryHandoffDoc_WritesSynthDocToDisk pins the wiring
// between SynthesizeRecovery and the on-disk handoff doc file. The
// dispatch path produces a doc at ~/.fleet/handoffs/<id>-<stamp>.md
// with handoff_type "recovery-synth", and updates the dead agent's
// last_handoff_path to point at it. Tests use t.TempDir() via
// FLEET_HOME so we don't touch the real ~/.fleet tree.
func TestWriteRecoveryHandoffDoc_WritesSynthDocToDisk(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEET_HOME", root)
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("state.Bootstrap: %v", err)
	}

	// Seed the dead coord's agent record so writeRecoveryHandoffDoc has
	// something to update.
	deadRec := fakeAgentRecord("deadbeef", "coord-myproj", "myproj", 11111, "fleet-deadbeef")
	if err := deadRec.Write(); err != nil {
		t.Fatalf("write dead record: %v", err)
	}
	// Seed minimal project state so the synth pass has something to
	// describe.
	pdir := filepath.Join(root, "projects", "myproj")
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cs := map[string]any{
		"worker_agent_ids": map[string]string{
			"fix-foo-1234": "cafef00d",
		},
	}
	csData, _ := json.Marshal(cs)
	csPath := filepath.Join(pdir, "coord-state.json")
	if err := os.WriteFile(csPath, csData, 0o644); err != nil {
		t.Fatalf("write coord-state: %v", err)
	}
	// Backdate mtime past coordFreshnessWindow so the dead-coord
	// recovery probe sees this as a stopped coord. A just-written
	// coord-state.json looks like a live coord that JUST ticked — the
	// veto path would refuse to spawn.
	stale := time.Now().Add(-2 * coordFreshnessWindow)
	if err := os.Chtimes(csPath, stale, stale); err != nil {
		t.Fatalf("chtimes coord-state: %v", err)
	}
	wDir := filepath.Join(pdir, "workers", "fix-foo-1234")
	if err := os.MkdirAll(wDir, 0o755); err != nil {
		t.Fatalf("mkdir worker dir: %v", err)
	}
	wState := map[string]any{
		"slug":    "fix-foo-1234",
		"project": "myproj",
		"phase":   "tdd-green",
		"pid":     0,
	}
	wData, _ := json.Marshal(wState)
	if err := os.WriteFile(filepath.Join(wDir, "state.json"), wData, 0o644); err != nil {
		t.Fatalf("write worker state: %v", err)
	}

	docPath, err := writeRecoveryHandoffDoc(deadRec, time.Now().UTC())
	if err != nil {
		t.Fatalf("writeRecoveryHandoffDoc: %v", err)
	}
	if docPath == "" {
		t.Fatalf("docPath is empty")
	}
	if _, statErr := os.Stat(docPath); statErr != nil {
		t.Fatalf("synth handoff doc not on disk at %s: %v", docPath, statErr)
	}
	body, rerr := os.ReadFile(docPath)
	if rerr != nil {
		t.Fatalf("read synth doc: %v", rerr)
	}
	if !strings.Contains(string(body), `handoff_type: "`+handoff.TypeRecoverySynth+`"`) {
		t.Errorf("synth doc must carry handoff_type=%q; got body:\n%s",
			handoff.TypeRecoverySynth, body)
	}
	if !strings.Contains(string(body), "fix-foo-1234") {
		t.Errorf("synth doc must list the in-flight worker slug; got body:\n%s", body)
	}
	// Codex iter-12 P1: the dead record's last_handoff_path must NOT
	// be mutated by the recovery write — if findRecoveryCandidate
	// misclassified a live coord (cross-tmux-socket case) and the
	// duplicate recovery loses the lock race, mutating the live
	// coord's record would corrupt its chain. The chain link lives
	// on the SUCCESSOR's record (spawn.Spawn's OldRecord branch sets
	// it), not the predecessor's.
	reread, lerr := agent.Load(deadRec.ID)
	if lerr != nil {
		t.Fatalf("reload dead record: %v", lerr)
	}
	if reread.LastHandoffPath != nil {
		t.Errorf("dead record's last_handoff_path must remain nil after recovery write; got %q",
			*reread.LastHandoffPath)
	}
}

// TestRunDispatch_DeadCoord_Recovers exercises runDispatch end-to-end:
// a stale coord record on disk with a dead pid AND no live tmux
// session must (a) trigger the synth-handoff write, (b) archive the
// dead record, (c) spawn a fresh successor whose last_handoff_path
// points at the synth doc. Pre-fix, this case would have ignored the
// dead record (and the dashboard's existing dispatch path would have
// over-counted live coords).
//
// Requires real tmux (we actually spawn the successor); skips on CI
// hosts that lack it. The sleep/60 command keeps the successor alive
// long enough to assert its record fields without racing teardown.
func TestRunDispatch_DeadCoord_Recovers(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)

	// Seed the dead coord record. pid 99999 is overwhelmingly likely
	// not running on the test host; the kill(0) probe returns ESRCH.
	// TmuxSession points at a name no one will create — we never
	// spawn it, so tmux.HasSession returns false.
	deadRec := agent.New("deadc0de")
	deadRec.TaskID = "coord-myproj"
	deadRec.Project = "myproj"
	deadRec.PID = 99999
	deadRec.TmuxSession = "fleet-deadc0de"
	if err := deadRec.Write(); err != nil {
		t.Fatalf("seed dead record: %v", err)
	}
	// Seed minimal project state so synth has something to describe.
	root := os.Getenv("FLEET_HOME")
	seedRecoveryRepo(t, root, "myproj") // coord recovery binds via resolver (PR3)
	pdir := filepath.Join(root, "projects", "myproj")
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}
	cs := map[string]any{
		"worker_agent_ids": map[string]string{
			"fix-foo-1234": "cafef00d",
		},
	}
	csData, _ := json.Marshal(cs)
	csPath := filepath.Join(pdir, "coord-state.json")
	if err := os.WriteFile(csPath, csData, 0o644); err != nil {
		t.Fatalf("write coord-state: %v", err)
	}
	// Backdate mtime past coordFreshnessWindow so the dead-coord
	// recovery probe sees this as a stopped coord. A just-written
	// coord-state.json looks like a live coord that JUST ticked — the
	// veto path would refuse to spawn.
	stale := time.Now().Add(-2 * coordFreshnessWindow)
	if err := os.Chtimes(csPath, stale, stale); err != nil {
		t.Fatalf("chtimes coord-state: %v", err)
	}
	wDir := filepath.Join(pdir, "workers", "fix-foo-1234")
	if err := os.MkdirAll(wDir, 0o755); err != nil {
		t.Fatalf("mkdir worker dir: %v", err)
	}
	wState := map[string]any{
		"slug":    "fix-foo-1234",
		"project": "myproj",
		"phase":   "tdd-green",
		"pid":     0,
	}
	wData, _ := json.Marshal(wState)
	if err := os.WriteFile(filepath.Join(wDir, "state.json"), wData, 0o644); err != nil {
		t.Fatalf("write worker state: %v", err)
	}

	// Run dispatch with --coord-spawn pointing at the same task_id.
	opts := &dispatchOpts{
		taskID:          "coord-myproj",
		project:         "myproj",
		projectExplicit: true,
		coordSpawn:      true,
		command:         []string{"sleep", "60"},
		commandExplicit: true,
	}
	var out bytes.Buffer
	if err := runDispatch(opts, &out); err != nil {
		t.Fatalf("runDispatch: %v\n%s", err, out.String())
	}
	stdout := out.String()
	if !strings.Contains(stdout, "recovering dead coord deadc0de") {
		t.Errorf("dispatch stdout must announce recovery; got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "synth handoff written to") {
		t.Errorf("dispatch stdout must mention synth handoff path; got:\n%s", stdout)
	}

	// The dead record is intentionally LEFT on disk (codex review
	// iter-11 P1): archiving it pre-emptively would disappear a
	// still-live coord on a different tmux socket if
	// findRecoveryCandidate misclassified the cross-socket case.
	// The dashboard's [x] flow cleans up the dead record manually.
	live, lerr := agent.List()
	if lerr != nil {
		t.Fatalf("agent.List: %v", lerr)
	}
	var deadFound bool
	for _, r := range live {
		if r.ID == "deadc0de" {
			deadFound = true
			break
		}
	}
	if !deadFound {
		t.Errorf("dead record must stay on disk after recovery (iter-11 P1 archive deferral); got it missing from agent.List")
	}

	// The successor must exist and carry a LastHandoffPath that points
	// at a recovery-synth doc. Skip the dead record itself (codex
	// iter-11 P1 leaves it on disk).
	var successor *agent.Record
	for _, r := range live {
		if r.TaskID == "coord-myproj" && r.Project == "myproj" && r.ID != "deadc0de" {
			successor = r
			break
		}
	}
	if successor == nil {
		t.Fatalf("expected a successor coord on disk; got %d live records (%+v)", len(live), live)
	}
	t.Cleanup(func() {
		// Best-effort cleanup of the successor's tmux session — the
		// test launched a real sleep/60 inside it via spawn.Spawn.
		_ = tmux.Kill(successor.TmuxSession)
	})
	if successor.LastHandoffPath == nil {
		t.Fatalf("successor LastHandoffPath must point at synth doc; got nil")
	}
	body, rerr := os.ReadFile(*successor.LastHandoffPath)
	if rerr != nil {
		t.Fatalf("read successor handoff doc: %v", rerr)
	}
	if !strings.Contains(string(body), `handoff_type: "`+handoff.TypeRecoverySynth+`"`) {
		t.Errorf("successor handoff doc must be recovery-synth; got:\n%s", body)
	}
	if !strings.Contains(string(body), "fix-foo-1234") {
		t.Errorf("successor handoff doc must list the in-flight worker slug; got:\n%s", body)
	}
	// handoff_number on the successor should be > 1 (incremented from
	// the dead's HandoffNumber). New() defaults to 1; spawn.Spawn's
	// OldRecord branch bumps to old+1.
	if successor.HandoffNumber <= 1 {
		t.Errorf("successor HandoffNumber = %d; want >= 2 (incremented from dead's 1)",
			successor.HandoffNumber)
	}
}

// TestRunDispatch_DeadCoord_SendsResumePromptToSuccessor pins codex
// iter-2 P1: when the recovery path fires AND opts.prompt is non-empty,
// the successor receives `handoff.ResumePrompt(newDocPath)` — NOT the
// original opts.prompt. Without this, the synth doc sits on disk and
// the new /coordinator session boots fresh, throwing in-flight worker
// state away (defeating the entire purpose of recovery).
func TestRunDispatch_DeadCoord_SendsResumePromptToSuccessor(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)

	// Capture the prompt that gets sent so we can assert ResumePrompt.
	var capturedPrompt string
	prev := sendInitialPrompt
	sendInitialPrompt = func(session, prompt string) (bool, error) {
		capturedPrompt = prompt
		return true, nil
	}
	t.Cleanup(func() { sendInitialPrompt = prev })

	// Seed dead coord record + minimal project state.
	deadRec := agent.New("c0ded00d")
	deadRec.TaskID = "coord-myproj"
	deadRec.Project = "myproj"
	deadRec.PID = 99999
	deadRec.TmuxSession = "fleet-c0ded00d"
	if err := deadRec.Write(); err != nil {
		t.Fatalf("seed dead record: %v", err)
	}
	root := os.Getenv("FLEET_HOME")
	seedRecoveryRepo(t, root, "myproj") // coord recovery binds via resolver (PR3)
	pdir := filepath.Join(root, "projects", "myproj")
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}
	cs := map[string]any{
		"worker_agent_ids": map[string]string{
			"fix-foo-1234": "cafef00d",
		},
	}
	csData, _ := json.Marshal(cs)
	csPath := filepath.Join(pdir, "coord-state.json")
	if err := os.WriteFile(csPath, csData, 0o644); err != nil {
		t.Fatalf("write coord-state: %v", err)
	}
	// Backdate mtime past coordFreshnessWindow so the dead-coord
	// recovery probe sees this as a stopped coord. A just-written
	// coord-state.json looks like a live coord that JUST ticked — the
	// veto path would refuse to spawn.
	stale := time.Now().Add(-2 * coordFreshnessWindow)
	if err := os.Chtimes(csPath, stale, stale); err != nil {
		t.Fatalf("chtimes coord-state: %v", err)
	}
	wDir := filepath.Join(pdir, "workers", "fix-foo-1234")
	if err := os.MkdirAll(wDir, 0o755); err != nil {
		t.Fatalf("mkdir worker dir: %v", err)
	}
	wState := map[string]any{
		"slug":    "fix-foo-1234",
		"project": "myproj",
		"phase":   "tdd-green",
		"pid":     0,
	}
	wData, _ := json.Marshal(wState)
	if err := os.WriteFile(filepath.Join(wDir, "state.json"), wData, 0o644); err != nil {
		t.Fatalf("write worker state: %v", err)
	}

	// The TUI passes a freshCoord-style prompt; we send a sentinel here
	// so we can verify the recovery branch SWAPPED it for ResumePrompt
	// (rather than appending or leaving the sentinel intact).
	origPrompt := "FRESH-COORD-PROMPT-SENTINEL — should be replaced on recovery"
	opts := &dispatchOpts{
		taskID:          "coord-myproj",
		project:         "myproj",
		projectExplicit: true,
		coordSpawn:      true,
		command:         []string{"sleep", "60"},
		commandExplicit: true,
		prompt:          origPrompt,
	}
	var out bytes.Buffer
	if err := runDispatch(opts, &out); err != nil {
		t.Fatalf("runDispatch: %v\n%s", err, out.String())
	}
	// Cleanup the successor's tmux session — runDispatch spawned a real one.
	// Skip the dead record (iter-11 P1: stays on disk).
	live, _ := agent.List()
	for _, r := range live {
		if r.TaskID == "coord-myproj" && r.Project == "myproj" && r.ID != "c0ded00d" {
			t.Cleanup(func() { _ = tmux.Kill(r.TmuxSession) })
			break
		}
	}

	if capturedPrompt == origPrompt {
		t.Fatalf("recovery path must REPLACE opts.prompt with ResumePrompt; got original sentinel: %q", capturedPrompt)
	}
	if !strings.Contains(capturedPrompt, "Read your handoff doc at ") {
		t.Errorf("captured prompt must be handoff.ResumePrompt shape; got: %q", capturedPrompt)
	}
	if !strings.Contains(capturedPrompt, ".md") {
		t.Errorf("captured prompt must reference a .md handoff doc; got: %q", capturedPrompt)
	}
}

// Note (codex review iter-11): the dispatch-side fresh-mtime veto was
// removed because no local probe combination reliably detects a live
// coord on a different tmux socket without also blocking legitimate
// recoveries. The coord skill's NB-flock on coordinator.lock is the
// authoritative single-supervisor guarantee. Tests that previously
// pinned the veto (TestRunDispatch_FreshCoordStateRefusesSpawn,
// TestRunDispatch_FreshCoordState_NoLiveRecord_AllowsSpawn) were
// deleted alongside the dispatch-side check.

// TestRunDispatch_DeadCoord_NoAutoResumeSkipsPromptSwap pins codex
// review iter-4 P2: --no-auto-resume is the documented escape hatch
// for shells/REPLs/alternate engines that can't consume natural-
// language prompts. The recovery path must honor it — opts.prompt
// stays whatever the caller passed (empty in TUI case, or operator-
// supplied) rather than being replaced by ResumePrompt.
func TestRunDispatch_DeadCoord_NoAutoResumeSkipsPromptSwap(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)

	var capturedPrompt string
	prev := sendInitialPrompt
	sendInitialPrompt = func(session, prompt string) (bool, error) {
		capturedPrompt = prompt
		return true, nil
	}
	t.Cleanup(func() { sendInitialPrompt = prev })

	deadRec := agent.New("deadbabe")
	deadRec.TaskID = "coord-myproj"
	deadRec.Project = "myproj"
	deadRec.PID = 99999
	deadRec.TmuxSession = "fleet-deadbabe"
	if err := deadRec.Write(); err != nil {
		t.Fatalf("seed dead record: %v", err)
	}
	root := os.Getenv("FLEET_HOME")
	seedRecoveryRepo(t, root, "myproj") // coord recovery binds via resolver (PR3)
	pdir := filepath.Join(root, "projects", "myproj")
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}
	cs := map[string]any{"worker_agent_ids": map[string]string{}}
	csData, _ := json.Marshal(cs)
	csPath := filepath.Join(pdir, "coord-state.json")
	if err := os.WriteFile(csPath, csData, 0o644); err != nil {
		t.Fatalf("write coord-state: %v", err)
	}
	// Backdate mtime past coordFreshnessWindow so the dead-coord
	// recovery probe sees this as a stopped coord. A just-written
	// coord-state.json looks like a live coord that JUST ticked — the
	// veto path would refuse to spawn.
	stale := time.Now().Add(-2 * coordFreshnessWindow)
	if err := os.Chtimes(csPath, stale, stale); err != nil {
		t.Fatalf("chtimes coord-state: %v", err)
	}

	origPrompt := "shell-friendly-no-auto-resume-prompt"
	opts := &dispatchOpts{
		taskID:          "coord-myproj",
		project:         "myproj",
		projectExplicit: true,
		coordSpawn:      true,
		command:         []string{"sleep", "60"},
		commandExplicit: true,
		prompt:          origPrompt,
		noAutoResume:    true, // operator explicitly opted out
	}
	var out bytes.Buffer
	if err := runDispatch(opts, &out); err != nil {
		t.Fatalf("runDispatch: %v\n%s", err, out.String())
	}
	live, _ := agent.List()
	for _, r := range live {
		if r.TaskID == "coord-myproj" && r.Project == "myproj" && r.ID != "deadbabe" {
			t.Cleanup(func() { _ = tmux.Kill(r.TmuxSession) })
			break
		}
	}

	if capturedPrompt != origPrompt {
		t.Errorf("noAutoResume must skip the ResumePrompt swap; got: %q want: %q",
			capturedPrompt, origPrompt)
	}
	if strings.Contains(capturedPrompt, "Read your handoff doc at ") {
		t.Errorf("noAutoResume must NOT inject ResumePrompt natural-language; got: %q", capturedPrompt)
	}
}

// TestRunDispatch_UnsubmittedPromptEmitsFailureMarker pins codex
// review iter-6 P2: when sendInitialPrompt returns submitted=false,
// runDispatch's stdout MUST contain the "initial prompt not delivered"
// substring that the TUI's dispatchPromptFailedMarker matches on.
// Without this, the TUI parses the lack of marker as "prompt delivered
// successfully" and writes the coord-spawn marker — even though the
// prompt is still in Claude's input box and no /coordinator skill is
// running, leaving a phantom coord on the dashboard.
func TestRunDispatch_UnsubmittedPromptEmitsFailureMarker(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)

	prev := sendInitialPrompt
	sendInitialPrompt = func(session, prompt string) (bool, error) {
		return false, nil // typed but Enter did not submit
	}
	t.Cleanup(func() { sendInitialPrompt = prev })

	opts := &dispatchOpts{
		taskID:          "some-task",
		project:         "myproj",
		projectExplicit: true,
		command:         []string{"sleep", "60"},
		commandExplicit: true,
		prompt:          "trigger the unsubmitted-warning branch",
	}
	var out bytes.Buffer
	if err := runDispatch(opts, &out); err != nil {
		t.Fatalf("runDispatch: %v\n%s", err, out.String())
	}
	// Cleanup the successor's tmux session.
	live, _ := agent.List()
	for _, r := range live {
		if r.TaskID == "some-task" && r.Project == "myproj" {
			t.Cleanup(func() { _ = tmux.Kill(r.TmuxSession) })
			break
		}
	}

	if !strings.Contains(out.String(), "initial prompt not delivered") {
		t.Errorf("unsubmitted-prompt warning must include dispatchPromptFailedMarker sigil; got:\n%s",
			out.String())
	}
}

// TestRunDispatch_DeadCoord_EngineClampOverridesInheritedCodex pins
// codex review iter-4 P2: when the caller passes an explicit --engine
// (e.g., TUI auto-spawn pinning claude-code), the recovery path must
// NOT silently inherit OldRecord.Engine="codex". Without the clamp,
// spawn.Spawn's OldRecord branch would set rec.Engine = "codex"
// regardless of opts.Engine, defeating the TUI's safety guard against
// auto-spawning codex coords (which the coord skill doesn't yet support).
func TestRunDispatch_DeadCoord_EngineClampOverridesInheritedCodex(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)

	deadRec := agent.New("c0dexc0d")
	deadRec.TaskID = "coord-myproj"
	deadRec.Project = "myproj"
	deadRec.PID = 99999
	deadRec.TmuxSession = "fleet-c0dexc0d"
	deadRec.Engine = "codex" // dead coord ran codex
	if err := deadRec.Write(); err != nil {
		t.Fatalf("seed dead record: %v", err)
	}
	root := os.Getenv("FLEET_HOME")
	seedRecoveryRepo(t, root, "myproj") // coord recovery binds via resolver (PR3)
	pdir := filepath.Join(root, "projects", "myproj")
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}
	cs := map[string]any{"worker_agent_ids": map[string]string{}}
	csData, _ := json.Marshal(cs)
	csPath := filepath.Join(pdir, "coord-state.json")
	if err := os.WriteFile(csPath, csData, 0o644); err != nil {
		t.Fatalf("write coord-state: %v", err)
	}
	// Backdate mtime past coordFreshnessWindow so the dead-coord
	// recovery probe sees this as a stopped coord. A just-written
	// coord-state.json looks like a live coord that JUST ticked — the
	// veto path would refuse to spawn.
	stale := time.Now().Add(-2 * coordFreshnessWindow)
	if err := os.Chtimes(csPath, stale, stale); err != nil {
		t.Fatalf("chtimes coord-state: %v", err)
	}

	opts := &dispatchOpts{
		taskID:          "coord-myproj",
		project:         "myproj",
		projectExplicit: true,
		coordSpawn:      true,
		command:         []string{"sleep", "60"},
		commandExplicit: true,
		engine:          "claude-code", // explicit TUI-style override
	}
	var out bytes.Buffer
	if err := runDispatch(opts, &out); err != nil {
		t.Fatalf("runDispatch: %v\n%s", err, out.String())
	}
	live, _ := agent.List()
	var successor *agent.Record
	for _, r := range live {
		if r.TaskID == "coord-myproj" && r.Project == "myproj" && r.ID != "c0dexc0d" {
			successor = r
			t.Cleanup(func() { _ = tmux.Kill(r.TmuxSession) })
			break
		}
	}
	if successor == nil {
		t.Fatalf("expected successor record; got none")
	}
	if successor.Engine != "claude-code" {
		t.Errorf("engine clamp: successor.Engine = %q; want claude-code (explicit --engine must beat inherited codex)",
			successor.Engine)
	}
}

// TestRunDispatch_DeadCoord_CodexRecoveryRejected pins codex review
// iter-9 P2 back-door: recovery of a dead codex coord without
// engine-clamp would inherit engine=codex into the successor, but the
// Python /coordinator skill needs Claude's Agent tool — a codex
// successor is non-functional. The dispatch path must reject this
// instead of silently spawning a broken coord.
//
// Operator escape: pass --engine claude-code to force-migrate the
// recovery (engineExplicit=true → clamp fires → successor is
// claude-code, command is reset to claude wrapper).
func TestRunDispatch_DeadCoord_CodexRecoveryRejected(t *testing.T) {
	setupFleetHome(t)
	// Defensive isolation (postmortem 2026-05-14 follow-up): the codex
	// rejection fires before any tmux.Spawn, but the runtime sink guard
	// would block a regression instead of letting it leak. Isolate up
	// front so the lint passes without relying on the gate's correctness.
	isolateTmuxSocket(t)

	deadRec := agent.New("c0dexc0de")
	deadRec.TaskID = "coord-myproj"
	deadRec.Project = "myproj"
	deadRec.PID = 99999
	deadRec.TmuxSession = "fleet-c0dexc0de"
	deadRec.Engine = "codex" // dead coord ran codex
	if err := deadRec.Write(); err != nil {
		t.Fatalf("seed dead record: %v", err)
	}
	root := os.Getenv("FLEET_HOME")
	seedRecoveryRepo(t, root, "myproj") // coord recovery binds via resolver (PR3)
	pdir := filepath.Join(root, "projects", "myproj")
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}
	cs := map[string]any{"worker_agent_ids": map[string]string{}}
	csData, _ := json.Marshal(cs)
	csPath := filepath.Join(pdir, "coord-state.json")
	if err := os.WriteFile(csPath, csData, 0o644); err != nil {
		t.Fatalf("write coord-state: %v", err)
	}
	stale := time.Now().Add(-2 * coordFreshnessWindow)
	if err := os.Chtimes(csPath, stale, stale); err != nil {
		t.Fatalf("chtimes coord-state: %v", err)
	}

	opts := &dispatchOpts{
		taskID:          "coord-myproj",
		project:         "myproj",
		projectExplicit: true,
		coordSpawn:      true,
		command:         []string{"sleep", "60"},
		commandExplicit: true,
		// No engine flag — would inherit codex without the guard.
	}
	var out bytes.Buffer
	err := runDispatch(opts, &out)
	if err == nil {
		t.Fatalf("expected codex recovery to be rejected; got nil error\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "coordinator skill only works under claude-code") {
		t.Errorf("error must explain the codex-coord limitation; got: %v", err)
	}
	// No successor should be on disk.
	live, _ := agent.List()
	for _, r := range live {
		if r.TaskID == "coord-myproj" && r.Project == "myproj" && r.ID != "c0dexc0de" {
			t.Cleanup(func() { _ = tmux.Kill(r.TmuxSession) })
			t.Errorf("no successor must be spawned when codex recovery is rejected; got %s", r.ID)
		}
	}
}

// TestRunDispatch_DeadCoord_FreshMtimeBlocksDispatch pins codex review
// iter-12 P1: when coord-state.json is fresh AND a record exists for
// the project, dispatch must refuse. The split-brain risk in the
// cross-tmux-socket case (live coord on another socket → fresh mtime
// + record present) outweighs the UX cost of refusing a recently-
// crashed coord's same-window restart. Operator workaround:
// `fleet rm <coord-id>` to clear the stale record, then retry.
//
// (Earlier iter-9/iter-10 versions of this test pinned the inverse:
// "recovery fires on fresh-mtime crashes." Codex iter-12 P1 reverted
// that decision because the cross-socket scenario can't be
// distinguished locally, and split-brain coordination is worse than
// blocked recovery.)
func TestRunDispatch_DeadCoord_FreshMtimeBlocksDispatch(t *testing.T) {
	setupFleetHome(t)
	// Defensive isolation (postmortem 2026-05-14 follow-up): the fresh-
	// mtime gate fires before tmux.Spawn, but isolating matches the
	// "rather block CI than re-leak production" rule.
	isolateTmuxSocket(t)

	deadRec := agent.New("recentdc")
	deadRec.TaskID = "coord-myproj"
	deadRec.Project = "myproj"
	deadRec.PID = 99999
	deadRec.TmuxSession = "fleet-recentdc"
	if err := deadRec.Write(); err != nil {
		t.Fatalf("seed dead record: %v", err)
	}
	root := os.Getenv("FLEET_HOME")
	seedRecoveryRepo(t, root, "myproj") // coord recovery binds via resolver (PR3)
	pdir := filepath.Join(root, "projects", "myproj")
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}
	cs := map[string]any{"worker_agent_ids": map[string]string{}}
	csData, _ := json.Marshal(cs)
	csPath := filepath.Join(pdir, "coord-state.json")
	if err := os.WriteFile(csPath, csData, 0o644); err != nil {
		t.Fatalf("write coord-state: %v", err)
	}
	// Fresh mtime + record present → veto must fire.

	opts := &dispatchOpts{
		taskID:          "coord-myproj",
		project:         "myproj",
		projectExplicit: true,
		coordSpawn:      true,
		command:         []string{"sleep", "60"},
		commandExplicit: true,
	}
	var out bytes.Buffer
	err := runDispatch(opts, &out)
	if err == nil {
		t.Fatalf("expected runDispatch to refuse spawn under fresh mtime + record present; got nil\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "mtime is recent") {
		t.Errorf("error must mention recent mtime; got: %v", err)
	}
	// F23b (attach-failover BUG #2): the veto must be a TYPED *vetoError
	// so main() maps it to exit 75 (EX_TEMPFAIL), distinct from the 70/1
	// other dispatch failures take. This is the cross-process exit-code
	// contract `fleet attach`'s shared coord-spawn wrapper classifies on
	// via exec.ExitError.ExitCode()==75. A plain fmt.Errorf here would
	// regress attach to exit 70 for a recoverable veto, violating the
	// never-exit rule.
	var ve *vetoError
	if !errors.As(err, &ve) {
		t.Fatalf("veto must return a typed *vetoError (main maps it to exit 75); got %T: %v", err, err)
	}
	if !vetoErrorFromErr(err) {
		t.Errorf("vetoErrorFromErr must detect the veto sentinel main() exits 75 on")
	}
	if vetoExitCode != 75 {
		t.Errorf("vetoExitCode = %d, want 75 (EX_TEMPFAIL) — the contract attach's wrapper classifies on", vetoExitCode)
	}
	// No successor should be on disk.
	live, _ := agent.List()
	for _, r := range live {
		if r.TaskID == "coord-myproj" && r.Project == "myproj" && r.ID != "recentdc" {
			t.Cleanup(func() { _ = tmux.Kill(r.TmuxSession) })
			t.Errorf("no successor should spawn under fresh-mtime veto; got %s", r.ID)
		}
	}
}

// TestRunDispatch_DeadCoord_InheritsCwdFromOldRecord pins codex review
// E4 — DESIGN-coord-repo-binding-from-project.md PR3: coord recovery
// resolves the repo binding via the shared resolver (meta.json pin), NOT
// the dead coord's recorded Cwd. The dead coord's Cwd may itself be a
// cwd-bug victim pointing at the WRONG tree; inheriting it would
// perpetuate the corruption across the resume. This test sets a WRONG
// dead.Cwd and a CORRECT meta.json repo_path and asserts the successor
// binds the resolver's repo, never the stale dead.Cwd.
func TestRunDispatch_DeadCoord_ResolvesRepoNotOldRecordCwd(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)

	// The dead coord's recorded cwd is a WRONG/stale tree.
	wrongCwd := t.TempDir()
	deadRec := agent.New("cwd1cwd1")
	deadRec.TaskID = "coord-myproj"
	deadRec.Project = "myproj"
	deadRec.PID = 99999
	deadRec.TmuxSession = "fleet-cwd1cwd1"
	deadRec.Cwd = wrongCwd
	if err := deadRec.Write(); err != nil {
		t.Fatalf("seed dead record: %v", err)
	}
	root := os.Getenv("FLEET_HOME")
	// The CORRECT binding comes from meta.json (resolver tier 1).
	correctRepo := seedRecoveryRepo(t, root, "myproj")
	pdir := filepath.Join(root, "projects", "myproj")
	cs := map[string]any{"worker_agent_ids": map[string]string{}}
	csData, _ := json.Marshal(cs)
	csPath := filepath.Join(pdir, "coord-state.json")
	if err := os.WriteFile(csPath, csData, 0o644); err != nil {
		t.Fatalf("write coord-state: %v", err)
	}
	stale := time.Now().Add(-2 * coordFreshnessWindow)
	if err := os.Chtimes(csPath, stale, stale); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	// No --cwd on this dispatch — the recovery path must resolve via the
	// resolver (meta), NOT fall back to the dead coord's wrong cwd.
	opts := &dispatchOpts{
		taskID:          "coord-myproj",
		project:         "myproj",
		projectExplicit: true,
		coordSpawn:      true,
		command:         []string{"sleep", "60"},
		commandExplicit: true,
		// cwd intentionally empty
	}
	var out bytes.Buffer
	if err := runDispatch(opts, &out); err != nil {
		t.Fatalf("runDispatch: %v\n%s", err, out.String())
	}
	live, _ := agent.List()
	var successor *agent.Record
	for _, r := range live {
		if r.TaskID == "coord-myproj" && r.Project == "myproj" && r.ID != "cwd1cwd1" {
			successor = r
			t.Cleanup(func() { _ = tmux.Kill(r.TmuxSession) })
			break
		}
	}
	if successor == nil {
		t.Fatalf("expected successor record; got none")
	}
	if successor.Cwd != correctRepo {
		t.Errorf("coord recovery bound the wrong tree: successor.Cwd = %q; want %q (resolver meta repo)",
			successor.Cwd, correctRepo)
	}
	if successor.Cwd == wrongCwd {
		t.Errorf("coord recovery reused the dead coord's stale Cwd %q — must resolve via meta", wrongCwd)
	}
}

// E5 — coord recovery REFUSES (no spawn) when the resolver cannot bind:
// no meta.json, no worktrees. It must NOT fall back to oldRecord.Cwd
// even though the dead coord has a live recorded cwd.
func TestRunDispatch_DeadCoord_RefusesWhenUnresolvable(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)

	liveButWrongCwd := t.TempDir()
	deadRec := agent.New("refuse01")
	deadRec.TaskID = "coord-myproj"
	deadRec.Project = "myproj"
	deadRec.PID = 99999
	deadRec.TmuxSession = "fleet-refuse01"
	deadRec.Cwd = liveButWrongCwd
	if err := deadRec.Write(); err != nil {
		t.Fatalf("seed dead record: %v", err)
	}
	root := os.Getenv("FLEET_HOME")
	// NO seedRecoveryRepo — no meta.json, no worktrees → resolver refuses.
	pdir := filepath.Join(root, "projects", "myproj")
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}
	cs := map[string]any{"worker_agent_ids": map[string]string{}}
	csData, _ := json.Marshal(cs)
	csPath := filepath.Join(pdir, "coord-state.json")
	if err := os.WriteFile(csPath, csData, 0o644); err != nil {
		t.Fatalf("write coord-state: %v", err)
	}
	stale := time.Now().Add(-2 * coordFreshnessWindow)
	if err := os.Chtimes(csPath, stale, stale); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	opts := &dispatchOpts{
		taskID:          "coord-myproj",
		project:         "myproj",
		projectExplicit: true,
		coordSpawn:      true,
		command:         []string{"sleep", "60"},
		commandExplicit: true,
	}
	var out bytes.Buffer
	err := runDispatch(opts, &out)
	if err == nil {
		t.Fatal("coord recovery must REFUSE when the resolver cannot bind, got nil error")
	}
	if !strings.Contains(err.Error(), "no usable checkout") {
		t.Errorf("refusal should surface the resolver hint, got: %v", err)
	}
	// No successor must have been spawned.
	live, _ := agent.List()
	for _, r := range live {
		if r.TaskID == "coord-myproj" && r.Project == "myproj" && r.ID != "refuse01" {
			t.Cleanup(func() { _ = tmux.Kill(r.TmuxSession) })
			t.Errorf("coord recovery spawned a successor %q despite refusal — must not bind a wrong tree", r.ID)
		}
	}
}

// TestRunDispatch_DeadCoord_InheritsCommandWhenEngineMatchesExplicit
// pins codex review iter-12 P2: when the operator (or TUI auto-spawn)
// passes --engine but the requested engine MATCHES the dead coord's
// engine, custom command inheritance must still fire. The TUI always
// shells recovery with --engine claude-code; a dead claude-code coord
// running a custom wrapper should restart under that wrapper, not
// under the default. The earlier iter-7 gate of !engineExplicit
// blocked this case unnecessarily.
func TestRunDispatch_DeadCoord_InheritsCommandWhenEngineMatchesExplicit(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)

	// Dead claude-code coord with a custom wrapper command.
	deadRec := agent.New("matchcmd")
	deadRec.TaskID = "coord-myproj"
	deadRec.Project = "myproj"
	deadRec.PID = 99999
	deadRec.TmuxSession = "fleet-matchcmd"
	deadRec.Engine = "claude-code"
	deadRec.Command = []string{"sleep", "240"} // custom wrapper sentinel
	if err := deadRec.Write(); err != nil {
		t.Fatalf("seed dead record: %v", err)
	}
	root := os.Getenv("FLEET_HOME")
	seedRecoveryRepo(t, root, "myproj") // coord recovery binds via resolver (PR3)
	pdir := filepath.Join(root, "projects", "myproj")
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}
	cs := map[string]any{"worker_agent_ids": map[string]string{}}
	csData, _ := json.Marshal(cs)
	csPath := filepath.Join(pdir, "coord-state.json")
	if err := os.WriteFile(csPath, csData, 0o644); err != nil {
		t.Fatalf("write coord-state: %v", err)
	}
	stale := time.Now().Add(-2 * coordFreshnessWindow)
	if err := os.Chtimes(csPath, stale, stale); err != nil {
		t.Fatalf("chtimes coord-state: %v", err)
	}

	// engineExplicit=true (programmatic) AND engine matches dead's.
	// lint-test-isolation:command-exempt — this test intentionally exercises
	// the wrapper-inheritance branch which fires only when commandExplicit
	// is false AND opts.command is empty. The dead-coord recovery probe
	// runs BEFORE the wrapper-swap, and inheritance replaces opts.command
	// with the dead record's Command (the sentinel ["sleep","240"] seeded
	// above) — so no real claude/codex argv ever reaches spawn.
	opts := &dispatchOpts{
		taskID:          "coord-myproj",
		project:         "myproj",
		projectExplicit: true,
		coordSpawn:      true,
		engine:          "claude-code", // explicit + matches dead
		// commandExplicit=false; opts.command empty
	}
	var out bytes.Buffer
	if err := runDispatch(opts, &out); err != nil {
		t.Fatalf("runDispatch: %v\n%s", err, out.String())
	}
	live, _ := agent.List()
	var successor *agent.Record
	for _, r := range live {
		if r.TaskID == "coord-myproj" && r.Project == "myproj" && r.ID != "matchcmd" {
			successor = r
			t.Cleanup(func() { _ = tmux.Kill(r.TmuxSession) })
			break
		}
	}
	if successor == nil {
		t.Fatalf("expected successor record; got none")
	}
	if len(successor.Command) != 2 || successor.Command[0] != "sleep" || successor.Command[1] != "240" {
		t.Errorf("custom command must be inherited when engine matches dead's: got %v; want [sleep 240]",
			successor.Command)
	}
}

// TestRunDispatch_DeadCoord_InheritsDisableAutoResume pins codex
// review iter-19 P2: when the dead coord had DisableAutoResume=true
// (custom shell/REPL wrapper) and the operator did NOT explicitly
// pass --no-auto-resume on the recovery dispatch, the successor must
// inherit the setting. Without inheritance, ResumePrompt's natural-
// language text would get typed into a session that explicitly
// opted out — defeating the whole point of --no-auto-resume.
func TestRunDispatch_DeadCoord_InheritsDisableAutoResume(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)

	var capturedPrompt string
	prev := sendInitialPrompt
	sendInitialPrompt = func(session, prompt string) (bool, error) {
		capturedPrompt = prompt
		return true, nil
	}
	t.Cleanup(func() { sendInitialPrompt = prev })

	deadRec := agent.New("dis4uto1")
	deadRec.TaskID = "coord-myproj"
	deadRec.Project = "myproj"
	deadRec.PID = 99999
	deadRec.TmuxSession = "fleet-dis4uto1"
	deadRec.Engine = "claude-code"
	deadRec.DisableAutoResume = true // dead coord explicitly opted out
	if err := deadRec.Write(); err != nil {
		t.Fatalf("seed dead record: %v", err)
	}
	root := os.Getenv("FLEET_HOME")
	seedRecoveryRepo(t, root, "myproj") // coord recovery binds via resolver (PR3)
	pdir := filepath.Join(root, "projects", "myproj")
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}
	cs := map[string]any{"worker_agent_ids": map[string]string{}}
	csData, _ := json.Marshal(cs)
	csPath := filepath.Join(pdir, "coord-state.json")
	if err := os.WriteFile(csPath, csData, 0o644); err != nil {
		t.Fatalf("write coord-state: %v", err)
	}
	stale := time.Now().Add(-2 * coordFreshnessWindow)
	if err := os.Chtimes(csPath, stale, stale); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	// Operator did NOT pass --no-auto-resume → inherit from dead.
	origPrompt := "operator-supplied-prompt-that-must-survive"
	opts := &dispatchOpts{
		taskID:               "coord-myproj",
		project:              "myproj",
		projectExplicit:      true,
		coordSpawn:           true,
		command:              []string{"sleep", "60"},
		commandExplicit:      true,
		prompt:               origPrompt,
		noAutoResumeExplicit: false,
	}
	var out bytes.Buffer
	if err := runDispatch(opts, &out); err != nil {
		t.Fatalf("runDispatch: %v\n%s", err, out.String())
	}
	live, _ := agent.List()
	var successor *agent.Record
	for _, r := range live {
		if r.TaskID == "coord-myproj" && r.Project == "myproj" && r.ID != "dis4uto1" {
			successor = r
			t.Cleanup(func() { _ = tmux.Kill(r.TmuxSession) })
			break
		}
	}
	if successor == nil {
		t.Fatalf("expected successor record; got none")
	}
	if !successor.DisableAutoResume {
		t.Errorf("successor must inherit DisableAutoResume=true from dead coord; got false")
	}
	if capturedPrompt != origPrompt {
		t.Errorf("with inherited DisableAutoResume, ResumePrompt swap must be skipped; got %q want %q",
			capturedPrompt, origPrompt)
	}
}

// TestCollectOpenPRs_EmptyCwdReturnsNil pins codex review iter-19 P2:
// when the dead coord's Cwd is empty (legacy record without cwd
// stored), collectOpenPRs must NOT shell out to gh — that would
// resolve PR list against the process cwd (an unrelated repo) and
// synth wrong-repo PR URLs into the recovery doc. The tasks.md
// fallback in writeRecoveryHandoffDoc handles this case
// authoritatively.
func TestCollectOpenPRs_EmptyCwdReturnsNil(t *testing.T) {
	prs := collectOpenPRs("")
	if prs != nil {
		t.Errorf("collectOpenPRs(\"\") must return nil to avoid wrong-repo PR enrichment; got %v", prs)
	}
}

// TestRunDispatch_CoordSpawnRejectsCodexEngine pins codex review
// iter-9 P2 front-door: the CLI must reject
// `fleet --engine codex dispatch coord-X --coord-spawn` outright
// (not just for recovery). The Python /coordinator skill emits
// Claude-Agent-tool DISPATCH blocks that only claude-code can run.
func TestRunDispatch_CoordSpawnRejectsCodexEngine(t *testing.T) {
	setupFleetHome(t)
	// Defensive isolation (postmortem 2026-05-14 follow-up): the
	// engine-rejection gate fires before tmux.Spawn, but isolating
	// matches the "rather block CI than re-leak production" rule.
	isolateTmuxSocket(t)

	opts := &dispatchOpts{
		taskID:          "coord-myproj",
		project:         "myproj",
		projectExplicit: true,
		coordSpawn:      true,
		command:         []string{"sleep", "60"},
		commandExplicit: true,
		engine:          "codex", // explicit codex on coord-spawn — must fail
	}
	var out bytes.Buffer
	err := runDispatch(opts, &out)
	if err == nil {
		t.Fatalf("expected --coord-spawn + --engine codex to be rejected; got nil\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "--coord-spawn requires --engine claude-code") {
		t.Errorf("error must mention coord-spawn engine constraint; got: %v", err)
	}
}

// TestRunDispatch_DeadCoord_InheritsCommandFromOldRecord pins codex
// review iter-7 P2: when the operator did NOT pass --command on the
// recovery dispatch, the successor inherits the dead coord's recorded
// Command so custom wrappers / non-default argvs survive recovery.
// Without this, a coord launched with `--command custom-wrapper` would
// restart under the default wrapper on recovery — wrong binary even
// though task identity was preserved.
func TestRunDispatch_DeadCoord_InheritsCommandFromOldRecord(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)

	deadRec := agent.New("c0dec0de")
	deadRec.TaskID = "coord-myproj"
	deadRec.Project = "myproj"
	deadRec.PID = 99999
	deadRec.TmuxSession = "fleet-c0dec0de"
	deadRec.Engine = "claude-code"
	// Custom command — the operator dispatched the dead coord with
	// `--command /usr/local/bin/custom-wrapper`. Recovery must inherit
	// this rather than fall back to the engine-default wrapper.
	deadRec.Command = []string{"sleep", "120"} // sentinel non-default
	if err := deadRec.Write(); err != nil {
		t.Fatalf("seed dead record: %v", err)
	}
	root := os.Getenv("FLEET_HOME")
	seedRecoveryRepo(t, root, "myproj") // coord recovery binds via resolver (PR3)
	pdir := filepath.Join(root, "projects", "myproj")
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}
	cs := map[string]any{"worker_agent_ids": map[string]string{}}
	csData, _ := json.Marshal(cs)
	csPath := filepath.Join(pdir, "coord-state.json")
	if err := os.WriteFile(csPath, csData, 0o644); err != nil {
		t.Fatalf("write coord-state: %v", err)
	}
	stale := time.Now().Add(-2 * coordFreshnessWindow)
	if err := os.Chtimes(csPath, stale, stale); err != nil {
		t.Fatalf("chtimes coord-state: %v", err)
	}

	// No --command on the recovery dispatch — simulates plain
	// `fleet dispatch <task> --coord-spawn`. commandExplicit=false
	// triggers the inheritance branch.
	// lint-test-isolation:command-exempt — same rationale as the sibling
	// InheritsCommandWhenEngineMatchesExplicit test: inheritance replaces
	// opts.command with the dead record's sentinel before spawn, so no real
	// engine argv is ever launched.
	opts := &dispatchOpts{
		taskID:          "coord-myproj",
		project:         "myproj",
		projectExplicit: true,
		coordSpawn:      true,
		// command intentionally empty; commandExplicit=false
	}
	var out bytes.Buffer
	if err := runDispatch(opts, &out); err != nil {
		t.Fatalf("runDispatch: %v\n%s", err, out.String())
	}
	live, _ := agent.List()
	var successor *agent.Record
	for _, r := range live {
		if r.TaskID == "coord-myproj" && r.Project == "myproj" && r.ID != "c0dec0de" {
			successor = r
			t.Cleanup(func() { _ = tmux.Kill(r.TmuxSession) })
			break
		}
	}
	if successor == nil {
		t.Fatalf("expected successor record; got none")
	}
	// The successor's persisted Command must be the dead coord's
	// sentinel value, NOT the engine-default wrapper.
	if len(successor.Command) != 2 || successor.Command[0] != "sleep" || successor.Command[1] != "120" {
		t.Errorf("command inheritance: successor.Command = %v; want [sleep 120] (inherited from dead coord)",
			successor.Command)
	}
}

// TestRunDispatch_DeadCoord_EngineClampSkipsCommandInherit pins codex
// review iter-8 P1: when the engine clamp fires (operator explicitly
// chose --engine claude-code over a dead codex coord), the recovery
// path must NOT inherit the dead coord's Command — that argv was
// built for the OLD engine's wrapper and would spawn codex even
// though the record advertises claude-code, defeating the clamp.
// The fresh-engine wrapper (already in opts.command after the wrapper-
// swap block) is the correct argv.
func TestRunDispatch_DeadCoord_EngineClampSkipsCommandInherit(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)

	// Dead coord ran codex with a sentinel codex-wrapper command.
	deadRec := agent.New("c0dexsen")
	deadRec.TaskID = "coord-myproj"
	deadRec.Project = "myproj"
	deadRec.PID = 99999
	deadRec.TmuxSession = "fleet-c0dexsen"
	deadRec.Engine = "codex"
	deadRec.Command = []string{"sleep", "300"} // codex-era sentinel
	if err := deadRec.Write(); err != nil {
		t.Fatalf("seed dead record: %v", err)
	}
	root := os.Getenv("FLEET_HOME")
	seedRecoveryRepo(t, root, "myproj") // coord recovery binds via resolver (PR3)
	pdir := filepath.Join(root, "projects", "myproj")
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}
	cs := map[string]any{"worker_agent_ids": map[string]string{}}
	csData, _ := json.Marshal(cs)
	csPath := filepath.Join(pdir, "coord-state.json")
	if err := os.WriteFile(csPath, csData, 0o644); err != nil {
		t.Fatalf("write coord-state: %v", err)
	}
	stale := time.Now().Add(-2 * coordFreshnessWindow)
	if err := os.Chtimes(csPath, stale, stale); err != nil {
		t.Fatalf("chtimes coord-state: %v", err)
	}

	// Operator forces claude-code (TUI auto-spawn pattern). No
	// --command on the dispatch — without the iter-8 gate, the
	// inheritance branch would silently install the codex Command.
	// lint-test-isolation:command-exempt — this test asserts the engine-clamp
	// branch SKIPS inheritance and falls back to the claude-code default
	// wrapper from the wrapper-swap. Setting commandExplicit:true would
	// defeat the contract under test.
	opts := &dispatchOpts{
		taskID:          "coord-myproj",
		project:         "myproj",
		projectExplicit: true,
		coordSpawn:      true,
		engine:          "claude-code", // explicit + non-matching → clamp fires
		// command intentionally empty; commandExplicit=false
	}
	var out bytes.Buffer
	if err := runDispatch(opts, &out); err != nil {
		t.Fatalf("runDispatch: %v\n%s", err, out.String())
	}
	live, _ := agent.List()
	var successor *agent.Record
	for _, r := range live {
		if r.TaskID == "coord-myproj" && r.Project == "myproj" && r.ID != "c0dexsen" {
			successor = r
			t.Cleanup(func() { _ = tmux.Kill(r.TmuxSession) })
			break
		}
	}
	if successor == nil {
		t.Fatalf("expected successor record; got none")
	}
	// Successor.Engine must be claude-code (clamp).
	if successor.Engine != "claude-code" {
		t.Errorf("engine: got %q; want claude-code", successor.Engine)
	}
	// Successor.Command must NOT be the codex sentinel — it should
	// be the claude-code default wrapper (engine-derived argv from
	// the wrapper-swap block in runDispatch).
	if len(successor.Command) == 2 && successor.Command[0] == "sleep" && successor.Command[1] == "300" {
		t.Errorf("engine clamp + command inherit interaction: successor still carries codex sentinel command %v; expected claude-code default wrapper", successor.Command)
	}
}

// TestRunDispatch_DeadCoord_LegacyRecordSkipsCommandInherit pins codex
// review iter-14 P1: pre-v0.9 records have engine="" because the field
// didn't exist. iter-13 added a "" → claude-code normalization in the
// command-inheritance branch, which bypassed the claude-only guard
// (that guard short-circuits on engine=="") and silently installed
// the legacy record's custom Command on the successor — even though
// the argv could be launching codex or anything else. The fix: refuse
// inheritance for legacy records; let the engine-default wrapper from
// the wrapper-swap block win. Operators can re-add a custom wrapper
// post-recovery via `fleet handoff <id> --command <wrapper>`.
func TestRunDispatch_DeadCoord_LegacyRecordSkipsCommandInherit(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)

	// Pre-v0.9 dead record: Engine field empty, custom Command that
	// could be running anything. The codex-sentinel is the smoking
	// gun — if iter-13's normalization were still live, this argv
	// would get inherited despite the claude-only contract.
	deadRec := agent.New("legacyc0")
	deadRec.TaskID = "coord-myproj"
	deadRec.Project = "myproj"
	deadRec.PID = 99999
	deadRec.TmuxSession = "fleet-legacyc0"
	deadRec.Engine = "" // pre-v0.9 record: field didn't exist
	deadRec.Command = []string{"sleep", "300"}
	if err := deadRec.Write(); err != nil {
		t.Fatalf("seed dead record: %v", err)
	}
	root := os.Getenv("FLEET_HOME")
	seedRecoveryRepo(t, root, "myproj") // coord recovery binds via resolver (PR3)
	pdir := filepath.Join(root, "projects", "myproj")
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}
	cs := map[string]any{"worker_agent_ids": map[string]string{}}
	csData, _ := json.Marshal(cs)
	csPath := filepath.Join(pdir, "coord-state.json")
	if err := os.WriteFile(csPath, csData, 0o644); err != nil {
		t.Fatalf("write coord-state: %v", err)
	}
	stale := time.Now().Add(-2 * coordFreshnessWindow)
	if err := os.Chtimes(csPath, stale, stale); err != nil {
		t.Fatalf("chtimes coord-state: %v", err)
	}

	// Plain `fleet dispatch <task> --coord-spawn`: no explicit engine,
	// no --command. Default engine is claude-code so the wrapper-swap
	// block populates opts.command with the claude-code default. The
	// inheritance branch must NOT overwrite that with the legacy
	// custom command.
	// lint-test-isolation:command-exempt — this test asserts the legacy-record
	// bypass: inheritance is REFUSED and the claude-code default wrapper
	// wins. Setting commandExplicit:true would defeat the contract under
	// test. requireTmux + isolateTmuxSocket (via setupFleetHome →
	// requireTmux earlier) reap the per-test tmux server.
	opts := &dispatchOpts{
		taskID:          "coord-myproj",
		project:         "myproj",
		projectExplicit: true,
		coordSpawn:      true,
		// engine intentionally empty → resolves to enginecfg.DefaultEngine (claude-code)
		// commandExplicit=false, command empty
	}
	var out bytes.Buffer
	if err := runDispatch(opts, &out); err != nil {
		t.Fatalf("runDispatch: %v\n%s", err, out.String())
	}
	live, _ := agent.List()
	var successor *agent.Record
	for _, r := range live {
		if r.TaskID == "coord-myproj" && r.Project == "myproj" && r.ID != "legacyc0" {
			successor = r
			t.Cleanup(func() { _ = tmux.Kill(r.TmuxSession) })
			break
		}
	}
	if successor == nil {
		t.Fatalf("expected successor record; got none")
	}
	// The successor's record must NOT carry the legacy custom command.
	if len(successor.Command) == 2 && successor.Command[0] == "sleep" && successor.Command[1] == "300" {
		t.Errorf("legacy-record bypass: successor inherited untrusted legacy command %v; expected claude-code default wrapper", successor.Command)
	}
}

// TestRunDispatch_CoordSpawn_FailsClosedOnUnparseableRecord pins
// codex review iter-17 P1: agent.List silently skips records that
// fail to parse, so an unparseable record (corrupt JSON, partial
// write) could mask a live coord and let --coord-spawn fall through
// to a fresh spawn — split-brain. The dispatch path now uses
// agent.ListStrict and aborts when any record won't parse.
func TestRunDispatch_CoordSpawn_FailsClosedOnUnparseableRecord(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEET_HOME", root)
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("state.Bootstrap: %v", err)
	}
	// Defensive isolation (postmortem 2026-05-14 follow-up): ListStrict
	// fails before tmux.Spawn, but isolating matches the "rather block
	// CI than re-leak production" rule.
	isolateTmuxSocket(t)
	// Drop a malformed .json into the agents dir. agent.List would
	// silently skip it; ListStrict reports it via badIDs.
	badPath := filepath.Join(root, "agents", "corruptr.json")
	if err := os.WriteFile(badPath, []byte("{ not valid json"), 0o644); err != nil {
		t.Fatalf("write corrupt record: %v", err)
	}

	// command + commandExplicit: leak-test-spawn-stub (DESIGN-lifecycle-leak-
	// recurrence PR-A). Rejection fires before wrapper-swap, but pin the
	// stub for gate-reorder safety.
	opts := &dispatchOpts{
		taskID:          "coord-myproj",
		project:         "myproj",
		projectExplicit: true,
		coordSpawn:      true,
		command:         []string{"sleep", "30"},
		commandExplicit: true,
	}
	var out bytes.Buffer
	err := runDispatch(opts, &out)
	if err == nil {
		t.Fatalf("expected runDispatch to fail closed on unparseable record; got nil")
	}
	if !strings.Contains(err.Error(), "unparseable record") {
		t.Errorf("error message should mention unparseable record for split-brain safety; got: %v", err)
	}
	if !strings.Contains(err.Error(), "corruptr") {
		t.Errorf("error message should surface the corrupt record's ID; got: %v", err)
	}
}

// TestWriteRecoveryHandoffDoc_FallsBackToTasksMdWhenGHEmpty pins
// codex review round-6 P1: when collectOpenPRs returns empty (gh
// missing, auth issue, network blip, or no head:worker/ matches),
// the recovery synth doc must still surface in-review PRs from
// tasks.md so the successor coord respawns shepherds. Without this,
// a gh outage during recovery silently drops in-review PR
// supervision — the worker has already exited, the task is in-review,
// and no shepherd until-loop watches the PR.
func TestWriteRecoveryHandoffDoc_FallsBackToTasksMdWhenGHEmpty(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEET_HOME", root)
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("state.Bootstrap: %v", err)
	}

	// gh returns nothing — simulates the degraded case.
	prev := collectOpenPRs
	collectOpenPRs = func(string) []handoff.OpenPR { return nil }
	t.Cleanup(func() { collectOpenPRs = prev })

	deadRec := fakeAgentRecord("ghoutage", "coord-myproj", "myproj", 11111, "fleet-ghoutage")
	if err := deadRec.Write(); err != nil {
		t.Fatalf("write dead record: %v", err)
	}
	pdir := filepath.Join(root, "projects", "myproj")
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}
	// coord-state.json with empty worker_agent_ids — simulates the
	// case codex flagged: workers exited, tasks in-review, no
	// active subagents to walk.
	cs := map[string]any{"worker_agent_ids": map[string]string{}}
	csData, _ := json.Marshal(cs)
	if err := os.WriteFile(filepath.Join(pdir, "coord-state.json"), csData, 0o644); err != nil {
		t.Fatalf("write coord-state: %v", err)
	}
	// tasks.md with two in-review tasks carrying PR URLs, plus one
	// done task that must NOT appear in OpenPRs. Built via the
	// tasks package's writer to keep the test resilient to format
	// changes in the on-disk markdown grammar.
	tfile := &tasks.File{
		Schema: 1,
		Tasks: []*tasks.Task{
			{Slug: "fix-foo-1234", Status: tasks.StatusInReview, Priority: tasks.PriorityP1, PRURL: "https://github.com/owner/repo/pull/42"},
			{Slug: "feat-bar-5678", Status: tasks.StatusInReview, Priority: tasks.PriorityP1, PRURL: "https://github.com/owner/repo/pull/43"},
			{Slug: "done-baz-9999", Status: tasks.StatusDone, Priority: tasks.PriorityP2, PRURL: "https://github.com/owner/repo/pull/40"},
		},
	}
	if err := tasks.Write(filepath.Join(pdir, "tasks.md"), tfile); err != nil {
		t.Fatalf("write tasks.md: %v", err)
	}

	docPath, err := writeRecoveryHandoffDoc(deadRec, time.Now().UTC())
	if err != nil {
		t.Fatalf("writeRecoveryHandoffDoc: %v", err)
	}
	body, rerr := os.ReadFile(docPath)
	if rerr != nil {
		t.Fatalf("read synth doc: %v", rerr)
	}
	bodyStr := string(body)
	if !strings.Contains(bodyStr, "https://github.com/owner/repo/pull/42") {
		t.Errorf("synth doc must include in-review PR URL #42 from tasks.md fallback; got body:\n%s", bodyStr)
	}
	if !strings.Contains(bodyStr, "https://github.com/owner/repo/pull/43") {
		t.Errorf("synth doc must include in-review PR URL #43 from tasks.md fallback; got body:\n%s", bodyStr)
	}
	// Done task's PR must NOT appear — only in-review needs respawn.
	if strings.Contains(bodyStr, "https://github.com/owner/repo/pull/40") {
		t.Errorf("synth doc must NOT include done-task PR #40; got body:\n%s", bodyStr)
	}
}

// TestCoordStateFresh_FailsClosedOnTransientStatError pins codex
// review iter-17 P1: when stat on coord-state.json fails for a
// transient reason (permission, I/O), coordStateFresh used to return
// false ("not fresh"), which bypassed the live-coord veto. A live
// coord on another socket whose state.json was briefly unreadable
// would be misclassified as dead. The fix: IsNotExist still returns
// false, but other errors return true (fail closed → veto fires).
func TestCoordStateFresh_FailsClosedOnTransientStatError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root — chmod 600 doesn't deny root execute")
	}
	root := t.TempDir()
	t.Setenv("FLEET_HOME", root)
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("state.Bootstrap: %v", err)
	}
	pdir := filepath.Join(root, "projects", "myproj")
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}
	// Seed coord-state.json so the path resolves to something stat
	// could read under normal perms.
	if err := os.WriteFile(filepath.Join(pdir, "coord-state.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatalf("write coord-state: %v", err)
	}
	// chmod 600 on the project dir removes execute → stat of any
	// child fails with EACCES (POSIX requires execute on each path
	// component to stat through it).
	if err := os.Chmod(pdir, 0o600); err != nil {
		t.Skipf("chmod project dir: %v (likely sandbox restriction)", err)
	}
	t.Cleanup(func() { _ = os.Chmod(pdir, 0o755) })

	if !coordStateFresh("myproj") {
		t.Errorf("coordStateFresh must fail closed (return true) on transient stat error; got false → live-coord veto would be bypassed")
	}
}

// TestRunDispatch_CoordSpawn_FailsClosedOnAgentListError pins codex
// review round-3 P1: when agent.List() errors during --coord-spawn
// (corrupt/unreadable agents dir), the live-coord veto and dead-coord
// recovery probe used to both fall open — letting dispatch spawn a
// fresh coord even though a live coord could exist on a different
// tmux socket whose record we couldn't read. The fix lifts agent.List
// above both checks and aborts the dispatch with a clear error.
func TestRunDispatch_CoordSpawn_FailsClosedOnAgentListError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root — chmod 000 doesn't deny root reads")
	}
	root := t.TempDir()
	t.Setenv("FLEET_HOME", root)
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("state.Bootstrap: %v", err)
	}
	// Defensive isolation (postmortem 2026-05-14 follow-up): agent.List
	// fails before tmux.Spawn, but isolating matches the "rather block
	// CI than re-leak production" rule.
	isolateTmuxSocket(t)
	// Make the agents directory unreadable so agent.List's ReadDir
	// errors. chmod 100 (execute-only) lets runDispatch's internal
	// state.Bootstrap re-run safely (MkdirAll on the already-existing
	// agents/.locks subdir only needs execute on the parent) while
	// still blocking the directory-listing read that agent.List does.
	agentsDir := filepath.Join(root, "agents")
	if err := os.Chmod(agentsDir, 0o100); err != nil {
		t.Skipf("chmod agents dir: %v (likely sandbox restriction)", err)
	}
	t.Cleanup(func() { _ = os.Chmod(agentsDir, 0o755) })

	// command + commandExplicit: leak-test-spawn-stub (DESIGN-lifecycle-leak-
	// recurrence PR-A). Rejection fires before wrapper-swap, but pin the
	// stub for gate-reorder safety.
	opts := &dispatchOpts{
		taskID:          "coord-myproj",
		project:         "myproj",
		projectExplicit: true,
		coordSpawn:      true,
		command:         []string{"sleep", "30"},
		commandExplicit: true,
	}
	var out bytes.Buffer
	err := runDispatch(opts, &out)
	if err == nil {
		t.Fatalf("expected runDispatch to fail closed on agent.List error; got nil")
	}
	if !strings.Contains(err.Error(), "cannot list agent records") {
		t.Errorf("error message should mention agent.List failure for split-brain safety; got: %v", err)
	}
}

// TestRunDispatch_DeadCoordRecovery_AdvertisesLockWinner pins codex iter-26 P1:
// when the dead-coord recovery path delivers the resume prompt via
// DeliverToCurrentOwner and a RACING standby (different agent-id than the one
// this dispatch just spawned) won the lease first, dispatch must advertise the
// WINNER's id in the "attach with" line — not the losing spawned standby's id.
//
//	spawn standby S  ──┐
//	                   ├─ both poll the lock; racer W wins, gets doc + marker
//	racing standby W ──┘
//	dispatch must print "attach with: fleet attach W"  (NOT S)
//
// Otherwise the TUI parses S from stdout, re-stamps the coord-spawn marker onto
// the losing standby S, overwrites the marker DeliverToCurrentOwner promoted to
// W, and attaches the operator to the wrong/dead session.
func TestRunDispatch_DeadCoordRecovery_AdvertisesLockWinner(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)
	t.Setenv("FLEET_LEASE_FAILOVER", "1") // delivery branch only fires with failover on

	// Stub the lock-owner delivery so we deterministically model "a racing
	// standby (winnerID) won the lease" without seeding a live foreign lease +
	// PID + tmux session. The returned record is the WINNER, distinct from the
	// standby runDispatch spawns below.
	const winnerID = "racewin1"
	winnerRec := agent.New(winnerID)
	winnerRec.TaskID = "coord-myproj"
	winnerRec.Project = "myproj"
	winnerRec.TmuxSession = "fleet-" + winnerID
	prevDeliver := deliverToCurrentOwner
	var deliverCalled bool
	deliverToCurrentOwner = func(opts handoffdelivery.Options) (*agent.Record, error) {
		deliverCalled = true
		if opts.Project != "myproj" {
			t.Errorf("delivery project = %q, want myproj", opts.Project)
		}
		if !opts.PromoteMarker {
			t.Errorf("recovery delivery must promote the coord-spawn marker")
		}
		return winnerRec, nil
	}
	t.Cleanup(func() { deliverToCurrentOwner = prevDeliver })

	deadRec := agent.New("deadc0d3")
	deadRec.TaskID = "coord-myproj"
	deadRec.Project = "myproj"
	deadRec.PID = 99999 // not alive → triggers recovery synth doc
	deadRec.TmuxSession = "fleet-deadc0d3"
	deadRec.Engine = "claude-code"
	if err := deadRec.Write(); err != nil {
		t.Fatalf("seed dead record: %v", err)
	}
	root := os.Getenv("FLEET_HOME")
	seedRecoveryRepo(t, root, "myproj")
	pdir := filepath.Join(root, "projects", "myproj")
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}
	cs := map[string]any{"worker_agent_ids": map[string]string{}}
	csData, _ := json.Marshal(cs)
	csPath := filepath.Join(pdir, "coord-state.json")
	if err := os.WriteFile(csPath, csData, 0o644); err != nil {
		t.Fatalf("write coord-state: %v", err)
	}
	stale := time.Now().Add(-2 * coordFreshnessWindow)
	if err := os.Chtimes(csPath, stale, stale); err != nil {
		t.Fatalf("chtimes coord-state: %v", err)
	}

	opts := &dispatchOpts{
		taskID:          "coord-myproj",
		project:         "myproj",
		projectExplicit: true,
		coordSpawn:      true,
		command:         []string{"sleep", "60"},
		commandExplicit: true,
	}
	var out bytes.Buffer
	if err := runDispatch(opts, &out); err != nil {
		t.Fatalf("runDispatch: %v\n%s", err, out.String())
	}
	// Reap whatever standby this dispatch spawned (the LOSER of the race).
	live, _ := agent.List()
	for _, r := range live {
		if r.TaskID == "coord-myproj" && r.Project == "myproj" &&
			r.ID != "deadc0d3" && r.ID != winnerID {
			t.Cleanup(func() { _ = tmux.Kill(r.TmuxSession) })
		}
	}

	if !deliverCalled {
		t.Fatalf("expected dead-coord recovery to route delivery through DeliverToCurrentOwner; it did not\n%s", out.String())
	}
	got := out.String()
	wantAttach := "attach with: fleet attach " + winnerID
	if !strings.Contains(got, wantAttach) {
		t.Errorf("dispatch must advertise the lock WINNER; want %q in output, got:\n%s", wantAttach, got)
	}
	// The spawned standby (loser) id must NOT be the advertised attach target.
	if strings.Contains(got, "attach with: fleet attach deadc0d3") {
		t.Errorf("must not advertise the dead coord id as attach target:\n%s", got)
	}
	// A differing-owner note tells the operator the real coord landed elsewhere.
	if !strings.Contains(got, "won the coord lease") {
		t.Errorf("expected a note that another standby won the lease; got:\n%s", got)
	}
	// The authoritative machine-readable line the TUI parses (LAST
	// "agent <id> spawned") must name the WINNER, so the TUI promotes the
	// coord marker + attaches to the live coordinator, not the losing standby
	// (codex iter-27 P1). Assert the winner's line appears AFTER the note.
	winnerSpawn := "agent " + winnerID + " spawned"
	if !strings.Contains(got, winnerSpawn) {
		t.Errorf("expected authoritative %q line for the TUI to parse; got:\n%s", winnerSpawn, got)
	}
	if idx := strings.LastIndex(got, "agent "); !strings.HasPrefix(got[idx:], winnerSpawn) {
		t.Errorf("the LAST 'agent ... spawned' line must name the winner %s (TUI takes last match); got tail:\n%s",
			winnerID, got[idx:])
	}
}
