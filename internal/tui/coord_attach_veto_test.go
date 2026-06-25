// Tests for the project-row [a] exit-75 (EX_TEMPFAIL) attach-the-live-
// leader path. When `fleet dispatch --coord-spawn` stands down because a
// healthy coord already holds the project's lease, dispatch exits 75 —
// the "attach the live one" signal, NOT a failure. The startCoordSpawn
// callback must re-resolve the live leader from disk (markerless
// FindLiveCoord + FindCoordByLockBody fallback, exactly like the CLI
// veto path) and attach, never render a fatal banner ("fleet attach
// never exits").
//
// These tests exercise the REAL production callback by stubbing
// runFleetCmd to invoke msgFn with a genuine *exec.ExitError (sh -c
// "exit 75"), then feed the resulting coordSpawnDoneMsg through Update.
// Records are seeded ON DISK (FLEET_HOME tmpdir) so the callback's
// agent.ListStrict() sees them — the cached m.records slice is
// intentionally NOT used (codex round-1 P1-B).
package tui

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/projectlookup"
	"github.com/edisonshen/fleet/internal/state"
)

// exitErr returns a real *exec.ExitError whose ExitCode() == code by
// running `sh -c "exit code"`. Deterministic, no timing. Same pattern
// cmd/fleet/attach_failover_test.go uses to pin the cross-process
// exit-code contract against a real ExitError rather than a fake.
func exitErr(t *testing.T, code int) error {
	t.Helper()
	err := exec.Command("sh", "-c", fmt.Sprintf("exit %d", code)).Run()
	if err == nil {
		t.Fatalf("sh -c 'exit %d' returned nil error; want *exec.ExitError", code)
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != code {
		t.Fatalf("sh -c 'exit %d' did not yield ExitCode()==%d; got %v", code, code, err)
	}
	return err
}

// stubFleetCmdInvokesCallback replaces runFleetCmd with a stub that
// invokes the REAL production msgFn (the startCoordSpawn closure) with
// the supplied combined output + error, then emits its message. Unlike
// stubFleetCmd (which short-circuits with a canned coordSpawnDoneMsg),
// this exercises the production exit-75 classification + disk re-resolve
// logic. Restored at test end.
func stubFleetCmdInvokesCallback(t *testing.T, out string, err error) {
	t.Helper()
	prev := runFleetCmd
	runFleetCmd = func(args []string, msgFn func(string, error) tea.Msg) tea.Cmd {
		return func() tea.Msg { return msgFn(out, err) }
	}
	t.Cleanup(func() { runFleetCmd = prev })
}

// seedDiskCoord writes ~/.fleet/agents/<id>.json (via the production
// Record.Write path) tagged as the coord for project, so the callback's
// agent.ListStrict() picks it up. taskID controls whether FindLiveCoord
// matches (coordTaskID(project)) or only the lock-body fallback can.
func seedDiskCoord(t *testing.T, id, project, taskID string) {
	t.Helper()
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
}

// seedLockBody writes <agentID> into the project's coordinator.lock so
// projectlookup.FindCoordByLockBody resolves it. Resolves the root via
// state.Root() (honors FLEET_HOME) rather than taking a path arg — the
// tui withFleetHome returns the projects/ subdir, not the root, so a
// caller-supplied path would double-nest.
func seedLockBody(t *testing.T, project, agentID string) {
	t.Helper()
	root, err := state.Root()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "projects", project, ".locks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "coordinator.lock"), []byte(agentID+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// projectRowModel builds a Model whose dashboard carries a single
// project row with the cursor on it, so [a] dispatches the project-row
// auto-spawn path.
func projectRowModelForVeto(project string) Model {
	m := New("test")
	m.dashboard = &Snapshot{Projects: []*ProjectRow{{Name: project}}}
	for i, r := range m.dashboardRows() {
		if r.kind == rowProject {
			m.dashCursor = i
			break
		}
	}
	return m
}

// stubMarkerWrite swaps writeCoordSpawnMarkerFn for a recording stub so
// tests can assert the marker was (or was NOT) written. Returns a
// pointer to the call count.
func stubMarkerWrite(t *testing.T) *int {
	t.Helper()
	prev := writeCoordSpawnMarkerFn
	calls := 0
	writeCoordSpawnMarkerFn = func(project, agentID string) error {
		calls++
		return nil
	}
	t.Cleanup(func() { writeCoordSpawnMarkerFn = prev })
	return &calls
}

// driveSpawn fires the [a] spawn cmd, drains the goroutine, and returns
// the resulting message + the model after Update consumes it.
func driveSpawn(t *testing.T, m Model) (coordSpawnDoneMsg, Model) {
	t.Helper()
	_, cmd := m.Update(keyMsg("a"))
	if cmd == nil {
		t.Fatal("[a] on project row should produce a spawn cmd")
	}
	msg, ok := cmd().(coordSpawnDoneMsg)
	if !ok {
		t.Fatalf("spawn cmd produced %T; want coordSpawnDoneMsg", cmd())
	}
	updated, _ := m.Update(msg)
	return msg, updated.(Model)
}

// TestVeto_MarkerBackedLiveLeader_Attaches — exit 75 + an on-disk live
// coord tagged coord-<project> (the marker-backed shape) → FindLiveCoord
// resolves it and the TUI attaches; no error flash.
func TestVeto_MarkerBackedLiveLeader_Attaches(t *testing.T) {
	withFleetHome(t)
	seedProjectMeta(t, "demo", t.TempDir())
	restorePL := projectlookup.SetTestStubs(
		func(s string) bool { return s == "fleet-livecord" },
		func(s string) (bool, error) { return s == "fleet-livecord", nil },
		nil,
	)
	t.Cleanup(restorePL)
	seedDiskCoord(t, "livecord", "demo", coordTaskID("demo"))
	// model.go now does a DEFINITIVE re-probe via sessionProbeFn before
	// attach; stub it so the resolved session reads reachable+alive
	// (true, nil) without real tmux.
	(&stubSessionProbe{}).install(t)
	markerWrites := stubMarkerWrite(t)
	// Seed the LITERAL 75 (not the tuiCoordVetoExitCode symbol) so this
	// test pins the cross-PROCESS contract: dispatch.go returns the
	// literal 75, and if the TUI's const ever drifts the classification
	// silently stops recognizing real vetoes. Self-consistent
	// symbol-vs-symbol tests would mask that drift.
	stubFleetCmdInvokesCallback(t, "spawn vetoed\n", exitErr(t, 75))

	msg, mm := driveSpawn(t, projectRowModelForVeto("demo"))
	if !msg.attachedExisting {
		t.Fatalf("exit 75 + live leader → attachedExisting must be true; got %+v", msg)
	}
	if mm.pendingAttach != "fleet-livecord" {
		t.Errorf("pendingAttach = %q; want fleet-livecord", mm.pendingAttach)
	}
	if mm.flash == nil || mm.flash.isErr {
		t.Fatalf("expected informational (non-err) flash; got %+v", mm.flash)
	}
	if *markerWrites != 0 {
		t.Errorf("attach-existing must not write the coord-spawn marker; got %d writes", *markerWrites)
	}
}

// TestVetoExitCode_MatchesDispatchContract pins the cross-process exit
// code: the TUI classifies on tuiCoordVetoExitCode but `fleet dispatch`
// returns the literal 75 (dispatch.go vetoExitCode, attach.go
// dispatchVetoExitCode). A drift here silently breaks veto recognition,
// so guard the literal directly (maintainability review P2).
func TestVetoExitCode_MatchesDispatchContract(t *testing.T) {
	if tuiCoordVetoExitCode != 75 {
		t.Fatalf("tuiCoordVetoExitCode = %d; want 75 (EX_TEMPFAIL — must match dispatch.go vetoExitCode / attach.go dispatchVetoExitCode)", tuiCoordVetoExitCode)
	}
}

// TestVeto_MarkerlessLiveLeader_Attaches — exit 75 + a live coord with
// task_id+project but NO coord-spawn marker (e.g. started from the
// shell). The markerless FindLiveCoord still matches (this is the case
// codex round-1 P1-A proves the marker-gated findExistingCoordForProject
// would have DROPPED). Also asserts the coord-spawn marker is NOT
// written (codex round-2 P1).
func TestVeto_MarkerlessLiveLeader_Attaches(t *testing.T) {
	withFleetHome(t)
	seedProjectMeta(t, "demo", t.TempDir())
	restorePL := projectlookup.SetTestStubs(
		func(s string) bool { return s == "fleet-nomarker" },
		func(s string) (bool, error) { return s == "fleet-nomarker", nil },
		nil,
	)
	t.Cleanup(restorePL)
	// No coord-spawn marker stub → coordSpawnMarkerFn returns "" for the
	// project (the marker file doesn't exist in this fresh FLEET_HOME),
	// so the marker-gated resolver would have bailed. The record is still
	// tagged coord-<project> so the markerless FindLiveCoord matches.
	seedDiskCoord(t, "nomarker", "demo", coordTaskID("demo"))
	(&stubSessionProbe{}).install(t)
	markerWrites := stubMarkerWrite(t)
	stubFleetCmdInvokesCallback(t, "spawn vetoed\n", exitErr(t, tuiCoordVetoExitCode))

	msg, mm := driveSpawn(t, projectRowModelForVeto("demo"))
	if !msg.attachedExisting {
		t.Fatalf("markerless live leader must still attach; got %+v", msg)
	}
	if mm.pendingAttach != "fleet-nomarker" {
		t.Errorf("pendingAttach = %q; want fleet-nomarker", mm.pendingAttach)
	}
	if *markerWrites != 0 {
		t.Errorf("writeCoordSpawnMarkerFn called %d times; want 0 (TUI did not spawn this coord — codex round-2 P1)", *markerWrites)
	}
}

// TestVeto_LockBodyOnlyLiveLeader_Attaches — exit 75 + a live coord
// whose tie to the project is ONLY the coordinator.lock body (its
// task_id is NOT coord-<project>, so FindLiveCoord misses). The
// FindCoordByLockBody fallback must resolve + attach.
func TestVeto_LockBodyOnlyLiveLeader_Attaches(t *testing.T) {
	withFleetHome(t)
	seedProjectMeta(t, "demo", t.TempDir())
	restorePL := projectlookup.SetTestStubs(
		func(s string) bool { return s == "fleet-1c00d001" },
		func(s string) (bool, error) { return s == "fleet-1c00d001", nil },
		nil,
	)
	t.Cleanup(restorePL)
	// task_id intentionally NOT coord-demo → FindLiveCoord misses; only
	// the lock body knows about this coord (manual/legacy spawn shape).
	seedDiskCoord(t, "1c00d001", "demo", "manual-spawn")
	seedLockBody(t, "demo", "1c00d001")
	(&stubSessionProbe{}).install(t)
	markerWrites := stubMarkerWrite(t)
	stubFleetCmdInvokesCallback(t, "spawn vetoed\n", exitErr(t, tuiCoordVetoExitCode))

	msg, mm := driveSpawn(t, projectRowModelForVeto("demo"))
	if !msg.attachedExisting {
		t.Fatalf("lock-body-only live leader must attach via FindCoordByLockBody; got %+v", msg)
	}
	if mm.pendingAttach != "fleet-1c00d001" {
		t.Errorf("pendingAttach = %q; want fleet-1c00d001", mm.pendingAttach)
	}
	if mm.flash == nil || mm.flash.isErr {
		t.Fatalf("expected informational flash; got %+v", mm.flash)
	}
	if *markerWrites != 0 {
		t.Errorf("attach-existing must not write the coord-spawn marker; got %d writes", *markerWrites)
	}
}

// TestVeto_MultipleLiveCoords_LockHolderWins — codex iter-8 [P1]: during
// a handoff/rotation window two live coord-<project> records exist. The
// veto fired against the lock HOLDER, so the attach must target the lock
// body's coord, not whichever live coord-<project> record happens to be
// first in directory order. Resolving FindCoordByLockBody before
// FindLiveCoord guarantees the authoritative leader wins.
func TestVeto_MultipleLiveCoords_LockHolderWins(t *testing.T) {
	withFleetHome(t)
	seedProjectMeta(t, "demo", t.TempDir())
	// Both coords alive. "0fd00002" sorts before "1eade001" in the
	// agents/ dir, so FindLiveCoord (directory order) would pick it — but
	// the lock body names "1eade001" as the leader.
	restorePL := projectlookup.SetTestStubs(
		func(s string) bool { return s == "fleet-0fd00002" || s == "fleet-1eade001" },
		func(s string) (bool, error) {
			return s == "fleet-0fd00002" || s == "fleet-1eade001", nil
		},
		nil,
	)
	t.Cleanup(restorePL)
	seedDiskCoord(t, "0fd00002", "demo", coordTaskID("demo")) // outgoing, sorts first
	seedDiskCoord(t, "1eade001", "demo", coordTaskID("demo")) // leader (lock holder)
	seedLockBody(t, "demo", "1eade001")
	(&stubSessionProbe{}).install(t)
	stubFleetCmdInvokesCallback(t, "spawn vetoed\n", exitErr(t, tuiCoordVetoExitCode))

	msg, mm := driveSpawn(t, projectRowModelForVeto("demo"))
	if !msg.attachedExisting {
		t.Fatalf("multi-coord veto must still attach; got %+v", msg)
	}
	if mm.pendingAttach != "fleet-1eade001" {
		t.Errorf("pendingAttach = %q; want fleet-1eade001 (lock holder must win over directory-first live coord fleet-0fd00002)", mm.pendingAttach)
	}
}

// TestVeto_NoLiveRecord_RecoverableFlash — exit 75 but NO live record on
// disk (winner mid-boot). Must emit a recoverable non-err flash, leave
// pendingAttach empty, and NOT respawn (no banner).
func TestVeto_NoLiveRecord_RecoverableFlash(t *testing.T) {
	withFleetHome(t)
	seedProjectMeta(t, "demo", t.TempDir())
	restorePL := projectlookup.SetTestStubs(
		func(s string) bool { return false },
		func(s string) (bool, error) { return false, nil },
		nil,
	)
	t.Cleanup(restorePL)
	// No record seeded on disk → neither resolver matches.
	stubFleetCmdInvokesCallback(t, "spawn vetoed\n", exitErr(t, tuiCoordVetoExitCode))

	msg, mm := driveSpawn(t, projectRowModelForVeto("demo"))
	if msg.attachedExisting {
		t.Fatalf("no live record → must NOT attach; got %+v", msg)
	}
	if msg.err != nil {
		t.Errorf("exit 75 must never surface as err; got %v", msg.err)
	}
	if msg.recoverable == "" {
		t.Error("expected a recoverable flash message on unresolved veto")
	}
	// Must name BOTH causes (cross-socket + winner-booting), mirroring
	// handleCoordSpawnVeto — naming only "press [a] again" loops a
	// cross-socket operator forever (codex round-5 P2).
	if !strings.Contains(msg.recoverable, "FLEET_TMUX_SOCKET") {
		t.Errorf("unresolved-veto flash must surface the cross-socket next step; got %q", msg.recoverable)
	}
	if !strings.Contains(msg.recoverable, "[a] again") {
		t.Errorf("unresolved-veto flash must keep the winner-booting retry hint; got %q", msg.recoverable)
	}
	if mm.pendingAttach != "" {
		t.Errorf("pendingAttach = %q; want empty (no respawn, no attach)", mm.pendingAttach)
	}
	if mm.flash == nil || mm.flash.isErr {
		t.Fatalf("expected recoverable non-err flash; got %+v", mm.flash)
	}
}

// TestVeto_NonVetoExit_PreservesBanner — a genuine non-75 failure (exit
// 1) must keep the existing fatal-banner behavior: coordSpawnDoneMsg.err
// set, error flash, no attach.
func TestVeto_NonVetoExit_PreservesBanner(t *testing.T) {
	withFleetHome(t)
	seedProjectMeta(t, "demo", t.TempDir())
	stubFleetCmdInvokesCallback(t, "boom\n", exitErr(t, 1))

	msg, mm := driveSpawn(t, projectRowModelForVeto("demo"))
	if msg.err == nil {
		t.Fatal("non-75 exit must set err (fatal banner preserved)")
	}
	if msg.attachedExisting {
		t.Error("non-75 exit must not take the attach-live path")
	}
	if mm.flash == nil || !mm.flash.isErr {
		t.Fatalf("expected error flash; got %+v", mm.flash)
	}
	if mm.pendingAttach != "" {
		t.Errorf("pendingAttach = %q; want empty on genuine failure", mm.pendingAttach)
	}
}

// TestVeto_ListStrictError_RecoverableNamesFault — codex iter-1 + iter-5
// [P2]: a disk read failure during the post-veto re-resolve (e.g.
// ~/.fleet/agents unreadable / not a dir) must be RECOVERABLE (exit 75
// already proved a live coord exists, so a fatal banner would dead-end
// the operator) yet NAME the real fault (not a generic "not visible
// yet"). We force ListStrict to fail by replacing FLEET_HOME/agents with
// a regular FILE so os.ReadDir returns ENOTDIR.
func TestVeto_ListStrictError_RecoverableNamesFault(t *testing.T) {
	withFleetHome(t)
	seedProjectMeta(t, "demo", t.TempDir())
	root, err := state.Root()
	if err != nil {
		t.Fatal(err)
	}
	// agents must be a FILE, not a dir → os.ReadDir errors (not
	// IsNotExist), so agent.ListStrict returns a real error.
	if werr := os.WriteFile(filepath.Join(root, "agents"), []byte("x"), 0o644); werr != nil {
		t.Fatal(werr)
	}
	stubFleetCmdInvokesCallback(t, "spawn vetoed\n", exitErr(t, tuiCoordVetoExitCode))

	msg, mm := driveSpawn(t, projectRowModelForVeto("demo"))
	if msg.err != nil {
		t.Fatalf("ListStrict failure must NOT be a fatal banner (dead-end); got err=%v", msg.err)
	}
	if msg.recoverable == "" {
		t.Fatal("ListStrict failure must emit a recoverable flash")
	}
	// Must LEAD with the actionable retry/socket guidance (codex iter-8
	// P2 — never let the disk diagnostic suppress it) and APPEND the read
	// fault as a note (don't silo).
	if !strings.Contains(msg.recoverable, "FLEET_TMUX_SOCKET") {
		t.Errorf("recoverable flash must lead with retry/socket guidance; got %q", msg.recoverable)
	}
	if !strings.Contains(msg.recoverable, "reading ~/.fleet/agents failed") {
		t.Errorf("recoverable flash must name the read fault as a note; got %q", msg.recoverable)
	}
	if msg.attachedExisting {
		t.Error("ListStrict failure must not attach")
	}
	if mm.flash == nil || mm.flash.isErr {
		t.Fatalf("expected recoverable (non-err) flash on disk read failure; got %+v", mm.flash)
	}
	if mm.pendingAttach != "" {
		t.Errorf("pendingAttach = %q; want empty", mm.pendingAttach)
	}
}

// TestSummarizeBadIDs_CapsLongLists — the corrupt-record flash must not
// blow out the TUI render when ~/.fleet/agents has many unparseable
// files (the project has a leaked-file history). Cap at 3 + "(and N
// more)" (review P3).
func TestSummarizeBadIDs_CapsLongLists(t *testing.T) {
	if got := summarizeBadIDs([]string{"a", "b"}); got != "a, b" {
		t.Errorf("short list = %q; want \"a, b\"", got)
	}
	if got := summarizeBadIDs([]string{"a", "b", "c"}); got != "a, b, c" {
		t.Errorf("exactly-cap list = %q; want \"a, b, c\"", got)
	}
	got := summarizeBadIDs([]string{"a", "b", "c", "d", "e"})
	if !strings.Contains(got, "a, b, c") || !strings.Contains(got, "(and 2 more)") {
		t.Errorf("over-cap list = %q; want first 3 + \"(and 2 more)\"", got)
	}
}

// TestVeto_CorruptRecord_RecoverableNamesBadID — codex iter-5 [P2]: when
// ListStrict returns badIDs (unparseable agent JSON) and neither
// resolver matched, the live leader may BE one of the corrupt records —
// every retry re-hits exit 75 forever. The recoverable flash must name
// the corrupt record so the operator knows to fix ~/.fleet/agents/<id>.json
// rather than mash [a].
func TestVeto_CorruptRecord_RecoverableNamesBadID(t *testing.T) {
	withFleetHome(t)
	seedProjectMeta(t, "demo", t.TempDir())
	// Write a corrupt agent record so ListStrict reports it in badIDs.
	dir, err := state.AgentDir()
	if err != nil {
		t.Fatal(err)
	}
	if mkerr := os.MkdirAll(dir, 0o755); mkerr != nil {
		t.Fatal(mkerr)
	}
	if werr := os.WriteFile(filepath.Join(dir, "badc0de1.json"), []byte("{not valid json"), 0o644); werr != nil {
		t.Fatal(werr)
	}
	stubFleetCmdInvokesCallback(t, "spawn vetoed\n", exitErr(t, tuiCoordVetoExitCode))

	msg, mm := driveSpawn(t, projectRowModelForVeto("demo"))
	if msg.err != nil {
		t.Fatalf("corrupt record must not be a fatal banner; got err=%v", msg.err)
	}
	if msg.attachedExisting {
		t.Error("corrupt record (no resolvable live coord) must not attach")
	}
	if !strings.Contains(msg.recoverable, "FLEET_TMUX_SOCKET") {
		t.Errorf("recoverable flash must lead with retry/socket guidance, not be hijacked by the corrupt-record note (codex iter-8 P2); got %q", msg.recoverable)
	}
	if !strings.Contains(msg.recoverable, "corrupt") || !strings.Contains(msg.recoverable, "badc0de1") {
		t.Errorf("recoverable flash must name the corrupt record id as a note; got %q", msg.recoverable)
	}
	if mm.flash == nil || mm.flash.isErr {
		t.Fatalf("expected recoverable (non-err) flash; got %+v", mm.flash)
	}
	if mm.pendingAttach != "" {
		t.Errorf("pendingAttach = %q; want empty", mm.pendingAttach)
	}
}

// TestVeto_LeaderDiesBeforeAttach_RecoverableNoQuit — codex round-4
// [P2]: the callback resolves a live leader, but it dies in the window
// before Update handles the message. The model.go re-probe must catch
// the now-dead session and emit a recoverable retry flash WITHOUT
// quitting into a dead tmux attach (the raw "no sessions" UX this
// handler exists to avoid). pendingAttach stays empty; no tea.Quit.
func TestVeto_LeaderDiesBeforeAttach_RecoverableNoQuit(t *testing.T) {
	withFleetHome(t)
	seedProjectMeta(t, "demo", t.TempDir())
	// projectlookup probe (callback-side) sees the leader alive so the
	// callback resolves it...
	restorePL := projectlookup.SetTestStubs(
		func(s string) bool { return s == "fleet-dyingcrd" },
		func(s string) (bool, error) { return s == "fleet-dyingcrd", nil },
		nil,
	)
	t.Cleanup(restorePL)
	seedDiskCoord(t, "dyingcrd", "demo", coordTaskID("demo"))
	// ...but the tui-level DEFINITIVE re-probe in model.go sees it DEAD
	// (alive=false, no err — the race: it died between resolution and
	// Update).
	(&stubSessionProbe{dead: map[string]bool{"fleet-dyingcrd": true}}).install(t)
	stubFleetCmdInvokesCallback(t, "spawn vetoed\n", exitErr(t, tuiCoordVetoExitCode))

	m := projectRowModelForVeto("demo")
	_, cmd := m.Update(keyMsg("a"))
	if cmd == nil {
		t.Fatal("[a] should produce a spawn cmd")
	}
	msg := cmd().(coordSpawnDoneMsg)
	if !msg.attachedExisting {
		t.Fatalf("callback should still resolve attachedExisting=true; got %+v", msg)
	}
	updated, attachCmd := m.Update(msg)
	mm := updated.(Model)
	if mm.pendingAttach != "" {
		t.Errorf("dead-session race: pendingAttach must stay empty; got %q", mm.pendingAttach)
	}
	if attachCmd == nil {
		t.Error("expected a loadAgentsCmd refresh, got nil (no quit)")
	}
	if mm.flash == nil {
		t.Fatal("expected a recoverable retry flash")
	}
	if !strings.Contains(mm.flash.text, "again") {
		t.Errorf("flash should tell operator to retry; got %q", mm.flash.text)
	}
}

// TestVeto_CrossSocketLeader_RecoverableNotDeadAttach — codex iter-3
// [P1]: the live coord is on a DIFFERENT tmux socket (wrong
// FLEET_TMUX_SOCKET). FindLiveCoord still matches because the
// projectlookup probe treats a tmux transport ERROR as "alive" (don't
// drop a live claim on a hiccup), so the callback returns
// attachedExisting=true. But the model.go DEFINITIVE re-probe
// (sessionProbeFn) returns a transport error for the unreachable
// session — so the TUI must NOT quit into a doomed tmux attach. Instead
// it surfaces the cross-socket guidance (FLEET_TMUX_SOCKET) and stays in
// the TUI. This is the exact dead-end the never-exit rule forbids.
func TestVeto_CrossSocketLeader_RecoverableNotDeadAttach(t *testing.T) {
	withFleetHome(t)
	seedProjectMeta(t, "demo", t.TempDir())
	// Callback-side projectlookup probe: alive=false but probe ERRORS →
	// projectlookup's sessionAliveOrProbe treats the error as alive, so
	// FindLiveCoord resolves the cross-socket coord.
	restorePL := projectlookup.SetTestStubs(
		func(s string) bool { return false },
		func(s string) (bool, error) {
			if s == "fleet-xsocket01" {
				return false, errors.New("no server on this socket")
			}
			return false, nil
		},
		nil,
	)
	t.Cleanup(restorePL)
	seedDiskCoord(t, "xsocket01", "demo", coordTaskID("demo"))
	// tui-level DEFINITIVE re-probe ERRORS for the unreachable session
	// (wrong socket) → model.go must route to the cross-socket flash.
	(&stubSessionProbe{errSessions: map[string]bool{"fleet-xsocket01": true}}).install(t)
	stubFleetCmdInvokesCallback(t, "spawn vetoed\n", exitErr(t, tuiCoordVetoExitCode))

	m := projectRowModelForVeto("demo")
	_, cmd := m.Update(keyMsg("a"))
	if cmd == nil {
		t.Fatal("[a] should produce a spawn cmd")
	}
	msg := cmd().(coordSpawnDoneMsg)
	if !msg.attachedExisting {
		t.Fatalf("callback should resolve attachedExisting=true via probe-error-as-alive; got %+v", msg)
	}
	updated, attachCmd := m.Update(msg)
	mm := updated.(Model)
	if mm.pendingAttach != "" {
		t.Errorf("cross-socket: must NOT quit into a doomed attach; pendingAttach=%q want empty", mm.pendingAttach)
	}
	if attachCmd == nil {
		t.Error("expected loadAgentsCmd refresh (stay in TUI), got nil")
	}
	if mm.flash == nil || mm.flash.isErr {
		t.Fatalf("expected recoverable non-err flash; got %+v", mm.flash)
	}
	if !strings.Contains(mm.flash.text, "FLEET_TMUX_SOCKET") {
		t.Errorf("cross-socket flash must surface the FLEET_TMUX_SOCKET next step; got %q", mm.flash.text)
	}
}
