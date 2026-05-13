package main

// Tests for the dead-coord recovery surface added in
// resume-dead-coord-ab65: detection (findRecoveryCandidate) + the
// dispatch-side wiring that synthesizes a handoff doc from on-disk
// state and points the successor at it.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/handoff"
	"github.com/edisonshen/fleet/internal/state"
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

// TestFindRecoveryCandidate_AlivePidIsNotCandidate pins the safety
// gate: a running coord (pid alive) is NEVER a recovery candidate,
// even if tmux is unhappy — the operator might just be on a different
// tmux server. Don't kidnap a live process.
func TestFindRecoveryCandidate_AlivePidIsNotCandidate(t *testing.T) {
	records := []*agent.Record{
		fakeAgentRecord("aaaaaaaa", "coord-myproj", "myproj", 99999, "fleet-aaaaaaaa"),
	}
	pidAlive := func(int) bool { return true }         // still ticking
	sessionAlive := func(string) bool { return false } // tmux gone

	coordFresh := func(string) bool { return false } // coord-state.json stale → dead
	got := findRecoveryCandidate("coord-myproj", "myproj", records, pidAlive, sessionAlive, coordFresh)
	if got != nil {
		t.Errorf("alive-pid candidate must NOT be returned; got %+v", got)
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

// TestFindRecoveryCandidate_FreshCoordStateVetoes pins the load-bearing
// safety gate (codex review iter-3 P1 fix, iter-5 follow-up): when pid
// + tmux both LOOK dead (operator on a different tmux server, dispatch
// CLI pid long-since reaped) but coord-state.json's mtime is fresh,
// findRecovery MUST refuse to recover. The live coord ticks
// coord-state.json on every supervisor pass; fresh mtime means
// SOMETHING is actively supervising. We must NOT synth-recover over
// a live coord and race two coord supervisors on the same project.
//
// Why mtime instead of coordinator.lock: the Python /coordinator skill
// holds coordinator.lock only for the duration of each tick (LOCK_NB|
// LOCK_EX with finally-release). Between ticks it's acquirable, so
// a lock-based probe would falsely classify the coord as dead during
// every inter-tick gap (codex review iter-3 P1).
func TestFindRecoveryCandidate_FreshCoordStateVetoes(t *testing.T) {
	records := []*agent.Record{
		fakeAgentRecord("aaaaaaaa", "coord-myproj", "myproj", 99999, "fleet-aaaaaaaa"),
	}
	pidAlive := func(int) bool { return false }        // dispatch CLI pid reaped
	sessionAlive := func(string) bool { return false } // operator on diff tmux server
	coordFresh := func(string) bool { return true }    // BUT coord recently ticked

	got := findRecoveryCandidate("coord-myproj", "myproj", records, pidAlive, sessionAlive, coordFresh)
	if got != nil {
		t.Errorf("fresh-mtime coord must NOT be a recovery candidate; got %+v", got)
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
	// The dead agent's last_handoff_path must now point at the synth
	// doc so a future read sees the recovery doc as its predecessor.
	reread, lerr := agent.Load(deadRec.ID)
	if lerr != nil {
		t.Fatalf("reload dead record: %v", lerr)
	}
	if reread.LastHandoffPath == nil {
		t.Fatalf("dead agent's last_handoff_path must be set to the synth doc; got nil")
	}
	if *reread.LastHandoffPath != docPath {
		t.Errorf("last_handoff_path = %q; want %q", *reread.LastHandoffPath, docPath)
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

	// The dead record must be archived (no longer in agent.List).
	live, lerr := agent.List()
	if lerr != nil {
		t.Fatalf("agent.List: %v", lerr)
	}
	for _, r := range live {
		if r.ID == "deadc0de" {
			t.Errorf("dead record %s must be archived; still in live list", r.ID)
		}
	}

	// The successor must exist and carry a LastHandoffPath that points
	// at a recovery-synth doc.
	var successor *agent.Record
	for _, r := range live {
		if r.TaskID == "coord-myproj" && r.Project == "myproj" {
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
	live, _ := agent.List()
	for _, r := range live {
		if r.TaskID == "coord-myproj" && r.Project == "myproj" {
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

// TestRunDispatch_FreshCoordStateRefusesSpawn pins codex review
// iter-4/iter-5/iter-8 P1+P2: when --coord-spawn lands on a project
// whose coord-state.json was mtime-updated within coordFreshnessWindow
// AND a live agent record exists, runDispatch must refuse to spawn a
// duplicate. The TUI's [a]-on-dead-tmux flow lands here when the
// operator dashboard sits on a different tmux server than the live
// coord — tmux.HasSession reports false, but the coord is actively
// ticking and updating coord-state.json. Without this veto, we'd
// race two supervisors on the same project.
//
// iter-8 P2: this MUST also require an alive agent record. After a
// clean coord shutdown, coord-state.json persists with fresh mtime
// past the archive — the veto would falsely block legitimate respawns
// for the full freshness window. See the
// TestRunDispatch_FreshCoordState_NoLiveRecord_AllowsSpawn test
// below for the negative path.
func TestRunDispatch_FreshCoordStateRefusesSpawn(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)

	// Seed a live coord record alongside the fresh coord-state.json —
	// the BOTH-signal case from iter-8 P2. PID=os.Getpid() so the
	// liveCoordRecordExists probe (codex iter-9 P1) treats this
	// record as alive rather than as a recovery candidate.
	liveRec := agent.New("liverec0")
	liveRec.TaskID = "coord-myproj"
	liveRec.Project = "myproj"
	liveRec.TmuxSession = "fleet-liverec0"
	liveRec.PID = os.Getpid()
	if err := liveRec.Write(); err != nil {
		t.Fatalf("seed live record: %v", err)
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

	opts := &dispatchOpts{
		taskID:          "coord-myproj",
		project:         "myproj",
		projectExplicit: true,
		coordSpawn:      true,
		command:         []string{"sleep", "60"},
		commandExplicit: true,
	}
	var out bytes.Buffer
	err = runDispatch(opts, &out)
	if err == nil {
		t.Fatalf("expected runDispatch to refuse spawn for a fresh coord; got nil error\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "mtime is recent") {
		t.Errorf("error must mention recent mtime; got: %v", err)
	}
	// No successor record should be on disk.
	live, _ := agent.List()
	for _, r := range live {
		if r.TaskID == "coord-myproj" && r.Project == "myproj" && r.ID != "liverec0" {
			t.Cleanup(func() { _ = tmux.Kill(r.TmuxSession) })
			t.Errorf("no successor must be spawned under fresh coord-state.json; got %s on tmux %s", r.ID, r.TmuxSession)
		}
	}
}

// TestRunDispatch_FreshCoordState_NoLiveRecord_AllowsSpawn pins codex
// review iter-8 P2: when coord-state.json is fresh but NO live agent
// record exists for the project+task (the legitimate post-clean-
// shutdown / post-archive scenario), runDispatch must NOT veto. The
// stale file alone is misleading after an archive; without this
// allowance, [a] / `fleet dispatch --coord-spawn` would fail for
// the full freshness window after a clean coord shutdown.
func TestRunDispatch_FreshCoordState_NoLiveRecord_AllowsSpawn(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)

	// Fresh coord-state.json (post-shutdown leftover) but NO live
	// agent record.
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
		t.Fatalf("runDispatch must NOT veto when coord-state.json is fresh but no live record exists: %v\n%s",
			err, out.String())
	}
	live, _ := agent.List()
	for _, r := range live {
		if r.TaskID == "coord-myproj" && r.Project == "myproj" {
			t.Cleanup(func() { _ = tmux.Kill(r.TmuxSession) })
			break
		}
	}
}

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
		if r.TaskID == "coord-myproj" && r.Project == "myproj" {
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
		if r.TaskID == "coord-myproj" && r.Project == "myproj" {
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

// TestRunDispatch_CoordSpawnRejectsCodexEngine pins codex review
// iter-9 P2 front-door: the CLI must reject
// `fleet --engine codex dispatch coord-X --coord-spawn` outright
// (not just for recovery). The Python /coordinator skill emits
// Claude-Agent-tool DISPATCH blocks that only claude-code can run.
func TestRunDispatch_CoordSpawnRejectsCodexEngine(t *testing.T) {
	setupFleetHome(t)

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
		if r.TaskID == "coord-myproj" && r.Project == "myproj" {
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
		if r.TaskID == "coord-myproj" && r.Project == "myproj" {
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
