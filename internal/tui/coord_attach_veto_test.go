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
	stubFleetCmdInvokesCallback(t, "spawn vetoed\n", exitErr(t, tuiCoordVetoExitCode))

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
