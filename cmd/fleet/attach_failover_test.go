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
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/projectlookup"
)

// --- test fixtures + helpers ---

// failoverSetup is the shared state for an attach-failover test:
// FLEET_HOME tempdir, attach call recorder, dispatch/gc shell-out
// recorders, session-alive map for tmux probes.
type failoverSetup struct {
	tmp           string
	attachedTo    string
	attachCalls   int
	dispatched    []string // project names passed to coord-spawn
	gcCalls       []string // project names passed to gc --aggressive
	aliveSessions map[string]bool
	newSpawnID    string // ID minted by the stub coord-spawn
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
	prevGC := gcAggressiveFnVar
	gcAggressiveFnVar = func(project string) error {
		s.gcCalls = append(s.gcCalls, project)
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
	t.Cleanup(func() {
		attachFnVar = prevAttach
		coordSpawnFnVar = prevDispatch
		gcAggressiveFnVar = prevGC
		sessionAliveFnVar = prevSessionAlive
		sessionProbeFnVar = prevSessionProbe
		listSessionsFnVar = prevListSessions
		tmuxAvailableFnVar = prevTmuxAvailable
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
	if !strings.Contains(got, "foo: reaped stale staleeee; spawned newcoord for projects-fleet; attaching") {
		t.Errorf("F6 stderr: %q", got)
	}
	if len(s.gcCalls) != 1 || s.gcCalls[0] != "projects-fleet" {
		t.Errorf("F6: expected one gc call for projects-fleet, got %v", s.gcCalls)
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
	s.addOrphanTmux(t, "deadbeef") // tmux only, no record
	stderr, err := s.run(t, "foo", AttachOpts{Project: "projects-fleet"})
	if err != nil {
		t.Fatalf("F7: expected no error, got %v", err)
	}
	got := stderr.String()
	if !strings.Contains(got, "foo: reaped stale deadbeef; spawned newcoord for projects-fleet; attaching") {
		t.Errorf("F7 stderr: %q", got)
	}
	if len(s.gcCalls) != 1 || s.gcCalls[0] != "projects-fleet" {
		t.Errorf("F7: expected one gc call, got %v", s.gcCalls)
	}
	if len(s.dispatched) != 1 || s.dispatched[0] != "projects-fleet" {
		t.Errorf("F7: expected one coord-spawn, got %v", s.dispatched)
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
	if !strings.Contains(got, "foo: no coord for projects-fleet; spawned newcoord; attaching") {
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
	if !strings.Contains(got, "projects-fleet: token matched project name; attached to current coord xxxxxxxx") {
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
	if !strings.Contains(got, "mystery0: deriving project from cwd basename → fleet") {
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
}

// --- helper: silence unused-import linter for io ---
var _ = io.Discard

// helper to keep imports tidy when adding more tests
var _ = fmt.Sprintf
