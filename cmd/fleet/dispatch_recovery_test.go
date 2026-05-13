package main

// Tests for the dead-coord recovery surface added in
// resume-dead-coord-ab65: detection (findRecoveryCandidate) + the
// dispatch-side wiring that synthesizes a handoff doc from on-disk
// state and points the successor at it.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/handoff"
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

	got := findRecoveryCandidate("coord-myproj", "myproj", records, pidAlive, sessionAlive)
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
	pidAlive := func(int) bool { return true }      // still ticking
	sessionAlive := func(string) bool { return false } // tmux gone

	got := findRecoveryCandidate("coord-myproj", "myproj", records, pidAlive, sessionAlive)
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

	got := findRecoveryCandidate("coord-myproj", "myproj", records, pidAlive, sessionAlive)
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

	got := findRecoveryCandidate("coord-myproj", "myproj", records, pidAlive, sessionAlive)
	if got != nil {
		t.Errorf("cross-project candidate must NOT be returned; got %+v", got)
	}
}

// TestFindRecoveryCandidate_MultipleDeadPicksFirst pins behavior on
// the unlikely-but-possible case where multiple stale records share
// the same task_id (e.g., a recovery itself crashed before archiving
// its predecessor). We pick the first match — the caller is expected
// to archive after spawning so the next dispatch finds at most one.
func TestFindRecoveryCandidate_MultipleDeadPicksFirst(t *testing.T) {
	records := []*agent.Record{
		fakeAgentRecord("oldest01", "coord-myproj", "myproj", 11111, "fleet-oldest01"),
		fakeAgentRecord("newest02", "coord-myproj", "myproj", 22222, "fleet-newest02"),
	}
	pidAlive := func(int) bool { return false }
	sessionAlive := func(string) bool { return false }

	got := findRecoveryCandidate("coord-myproj", "myproj", records, pidAlive, sessionAlive)
	if got == nil {
		t.Fatalf("expected one candidate; got nil")
	}
	// Either is acceptable behavior; we just lock determinism by
	// requiring the first match (slice order is the agent.List order).
	if got.ID != "oldest01" {
		t.Errorf("candidate ID = %q; want oldest01 (first match wins)", got.ID)
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
	if err := os.WriteFile(filepath.Join(pdir, "coord-state.json"), csData, 0o644); err != nil {
		t.Fatalf("write coord-state: %v", err)
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
