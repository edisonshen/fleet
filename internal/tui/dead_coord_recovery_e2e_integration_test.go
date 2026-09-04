//go:build integration && (linux || darwin)

// End-to-end coverage for the [a] dead-coordinator recovery loop with the
// REAL subprocess chain under the Bubble Tea model: the model's cmds shell
// out to the built fleet binary (`fleet dispatch --coord-spawn`), which
// spawns real `fleet coord-run --standby` supervisors in real tmux panes
// (isolated FLEET_TMUX_SOCKET) running a fake claude; the model's session
// probes are the real tmux probes. Only tea's event loop is replaced by
// the test stepping cmds by hand.
//
// Fenced behind the integration tag like cmd/fleet's coord-run tests; see
// internal/testutil/coorde2e for the shared corpse-seeding fixtures.
package tui

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/coordlock"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/testutil/coorde2e"
	"github.com/edisonshen/fleet/internal/testutil/tmuxtest"
)

func buildFleetBinaryForTUI(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}
	bin := filepath.Join(t.TempDir(), "fleet")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/edisonshen/fleet/cmd/fleet")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build cmd/fleet: %v\n%s", err, out)
	}
	return bin
}

// tuiE2E is one operator sitting at the dashboard of an isolated Fleet
// home. It drives [a] through the real model and steps the resulting
// cmds (real dispatch subprocesses) by hand.
type tuiE2E struct {
	m        Model
	cmd      tea.Cmd
	bin      string
	project  string
	repo     string
	modeFile string
	calls    [][]string
}

func newTUIE2E(t *testing.T, project string) *tuiE2E {
	t.Helper()
	bin := buildFleetBinaryForTUI(t) // before PATH is narrowed: needs `go`
	tmuxtest.RequireTmux(t)
	t.Setenv("FLEET_HOME", t.TempDir())
	t.Setenv("FLEET_GC_SCAN_DIR", t.TempDir()) // dispatch's OrphanTmux pass must not walk /tmp
	t.Setenv("FLEET_MAX_SESSIONS", "100000")
	t.Setenv("FLEET_STANDBY_TIMEOUT", "3s")
	t.Setenv("FLEET_INITIAL_PROMPT_STABLE_MS", "100")
	t.Setenv("FLEET_INITIAL_PROMPT_MAX_MS", "1000")
	t.Setenv("FLEET_POST_READY_BUFFER_MS", "0")
	t.Setenv("FLEET_POST_SEND_VERIFY_MS", "0")
	t.Setenv("FLEET_POST_SEND_RETRY_MS", "0")
	t.Setenv("FLEET_PROMPT_ENTER_DELAY_MS", "50")
	t.Setenv("FLEET_PID_RESOLVE_S", "1")
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	fakeDir := t.TempDir()
	modeFile := coorde2e.FakeClaude(t, fakeDir)
	t.Setenv("PATH", fakeDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	repo := coorde2e.SeedProject(t, project)
	t.Cleanup(func() { coorde2e.KillAllCoords(t) })

	e := &tuiE2E{m: New("test"), bin: bin, project: project, repo: repo, modeFile: modeFile}
	prevBin, prevRun := fleetBinary, runFleetCmd
	fleetBinary = bin
	runFleetCmd = func(args []string, msgFn func(string, error) tea.Msg) tea.Cmd {
		e.calls = append(e.calls, append([]string(nil), args...))
		return prevRun(args, msgFn)
	}
	t.Cleanup(func() { fleetBinary, runFleetCmd = prevBin, prevRun })
	return e
}

func (e *tuiE2E) pressA(t *testing.T) {
	t.Helper()
	e.m.flash = nil
	e.m.dashboard = &Snapshot{Projects: []*ProjectRow{{Name: e.project}}}
	for i, row := range e.m.dashboardRows() {
		if row.kind == rowProject {
			e.m.dashCursor = i
			break
		}
	}
	updated, cmd := e.m.Update(keyMsg("a"))
	e.m, e.cmd = updated.(Model), cmd
	if e.m.flash != nil && e.m.flash.isErr {
		t.Fatalf("[a] refused before dispatch: %s", e.m.flash.text)
	}
}

// runDispatch executes the pending cmd — a real `fleet dispatch` — and
// returns its result WITHOUT feeding it to the model, so a test can act
// on the world (kill the coord it names) before the model sees it.
func (e *tuiE2E) runDispatch(t *testing.T) coordSpawnDoneMsg {
	t.Helper()
	if e.cmd == nil {
		t.Fatal("expected a pending dispatch cmd")
	}
	msg, ok := e.cmd().(coordSpawnDoneMsg)
	if !ok {
		t.Fatalf("pending cmd did not yield coordSpawnDoneMsg")
	}
	t.Logf("dispatch → agent=%s session=%s err=%v", msg.agentID, msg.session, msg.err)
	return msg
}

func (e *tuiE2E) feed(msg tea.Msg) {
	updated, cmd := e.m.Update(msg)
	e.m, e.cmd = updated.(Model), cmd
}

// killNamedCoord makes the coord a dispatch just named a corpse, the way
// the operator finds it days later: supervisor SIGKILLed, session gone,
// every freshness signal stale.
func (e *tuiE2E) killNamedCoord(t *testing.T, id string) *agent.Record {
	t.Helper()
	rec := coorde2e.WaitLiveCoord(t, e.project, id)
	coorde2e.KillCoordCorpse(t, rec)
	coorde2e.AgeDeadCoord(t, e.project, id)
	return rec
}

func (e *tuiE2E) wantAttachedTo(t *testing.T, id string) *agent.Record {
	t.Helper()
	rec := coorde2e.WaitLiveCoord(t, e.project, id)
	if e.m.pendingAttach != rec.TmuxSession {
		t.Errorf("pendingAttach = %q, want %q", e.m.pendingAttach, rec.TmuxSession)
	}
	if e.cmd == nil {
		t.Fatal("expected tea.Quit to attach")
	} else if _, isQuit := e.cmd().(tea.QuitMsg); !isQuit {
		t.Error("expected tea.Quit to attach")
	}
	if e.m.flash == nil || e.m.flash.isErr || !strings.Contains(e.m.flash.text, id) {
		t.Errorf("attach flash = %+v, want a non-error flash naming %s", e.m.flash, id)
	}
	if owner, ok := coordlock.CurrentOwner(e.project); !ok || owner.AgentID != id {
		t.Errorf("lease owner = %+v ok=%v, want %s", owner, ok, id)
	}
	return rec
}

func (e *tuiE2E) wantCalls(t *testing.T, n int) {
	t.Helper()
	if len(e.calls) != n {
		t.Fatalf("fleet invocations = %d, want %d: %v", len(e.calls), n, e.calls)
	}
}

func wantLinked(t *testing.T, repl *agent.Record, deadID string) {
	t.Helper()
	if repl.PredecessorID != deadID {
		t.Errorf("replacement %s predecessor_id = %q, want %q", repl.ID, repl.PredecessorID, deadID)
	}
	if repl.LastHandoffPath == nil || !strings.HasPrefix(filepath.Base(*repl.LastHandoffPath), deadID) {
		t.Errorf("replacement %s last_handoff_path = %v, want a synth doc minted for %s", repl.ID, repl.LastHandoffPath, deadID)
	}
	if !coorde2e.Archived(t, deadID) {
		t.Errorf("dead coord %s not archived after %s took the lease", deadID, repl.ID)
	}
	if _, err := agent.Load(deadID); err == nil {
		t.Errorf("dead coord %s still listed as live", deadID)
	}
}

// TestE2E_KeyA_DaysDeadCoord_RecoversAndAttachesReplacement is the
// operator's report end to end: [a] on a project whose coord died days
// ago. One press must start a replacement that inherits the dead coord's
// in-flight work, attach to it, and leave the corpse archived — no
// "archive it and re-press [a]" detour.
func TestE2E_KeyA_DaysDeadCoord_RecoversAndAttachesReplacement(t *testing.T) {
	e := newTUIE2E(t, "e2e-tui-recover")
	seed := coorde2e.Dispatch(t, e.bin, e.project, e.repo)
	deadID := coorde2e.SpawnedID(seed.Out)
	if seed.ExitCode != 0 || deadID == "" {
		t.Fatalf("seed dispatch exit=%d id=%q\n%s", seed.ExitCode, deadID, seed.Out)
	}
	coorde2e.SeedInFlightWorker(t, e.project, "fix-login", "wkr00001", "implementing")
	e.killNamedCoord(t, deadID)

	e.pressA(t)
	msg := e.runDispatch(t)
	if msg.err != nil || msg.agentID == "" || msg.agentID == deadID {
		t.Fatalf("recovery dispatch: err=%v agent=%q (dead=%s)", msg.err, msg.agentID, deadID)
	}
	e.feed(msg)
	e.wantCalls(t, 1)
	repl := e.wantAttachedTo(t, msg.agentID)
	wantLinked(t, repl, deadID)
	if sess := coorde2e.FleetSessions(t); len(sess) != 1 {
		t.Errorf("fleet sessions = %v, want exactly the replacement", sess)
	}
}

// TestE2E_KeyA_ReplacementDiesAfterDispatch_AutoRespawnsOnce covers the
// exact message the operator saw: dispatch exits 0 naming a coord, but by
// the time the model probes it the session is gone. The model must
// recover (one more real dispatch, which hands the dead coord's state to
// a replacement and reaps it) and attach to the replacement — not flash
// "re-press [a] after archiving".
func TestE2E_KeyA_ReplacementDiesAfterDispatch_AutoRespawnsOnce(t *testing.T) {
	e := newTUIE2E(t, "e2e-tui-respawn")
	e.pressA(t)
	first := e.runDispatch(t)
	if first.err != nil || first.agentID == "" {
		t.Fatalf("first dispatch: err=%v agent=%q", first.err, first.agentID)
	}
	coorde2e.SeedInFlightWorker(t, e.project, "fix-login", "wkr00001", "implementing")
	e.killNamedCoord(t, first.agentID)

	e.feed(first) // model probes fleet-<first>: gone → recovery
	if e.m.flash == nil || e.m.flash.isErr || !strings.Contains(e.m.flash.text, "recovering") {
		t.Fatalf("expected the recovery flash, got %+v", e.m.flash)
	}
	if e.m.pendingAttach != "" {
		t.Fatalf("must not attach to the dead session; pendingAttach=%q", e.m.pendingAttach)
	}
	e.wantCalls(t, 2)
	second := e.runDispatch(t)
	if second.err != nil || second.agentID == "" || second.agentID == first.agentID {
		t.Fatalf("recovery dispatch: err=%v agent=%q (dead=%s)", second.err, second.agentID, first.agentID)
	}
	e.feed(second)
	e.wantCalls(t, 2)
	repl := e.wantAttachedTo(t, second.agentID)
	wantLinked(t, repl, first.agentID)
	if _, marked := e.m.coordDeadRespawn[e.project]; marked {
		t.Error("recovery bound marker must be consumed once the replacement is attached")
	}
}

// TestE2E_KeyA_ReplacementDiesTwice_BoundedThenFreshPressRecovers pins
// the loop bound against real processes: when the replacement is ALSO
// dead by the time the model probes it, the press ends after exactly two
// dispatches with a startup-failure diagnosis and nothing left running.
// A fresh operator [a] gets a full recovery attempt and attaches.
func TestE2E_KeyA_ReplacementDiesTwice_BoundedThenFreshPressRecovers(t *testing.T) {
	e := newTUIE2E(t, "e2e-tui-bounded")
	e.pressA(t)
	first := e.runDispatch(t)
	if first.err != nil || first.agentID == "" {
		t.Fatalf("first dispatch: err=%v agent=%q", first.err, first.agentID)
	}
	e.killNamedCoord(t, first.agentID)
	e.feed(first)
	e.wantCalls(t, 2)
	second := e.runDispatch(t)
	if second.err != nil || second.agentID == "" {
		t.Fatalf("recovery dispatch: err=%v agent=%q", second.err, second.agentID)
	}
	e.killNamedCoord(t, second.agentID)
	e.feed(second)

	e.wantCalls(t, 2)
	if e.m.pendingAttach != "" {
		t.Errorf("must not attach to a dead replacement; pendingAttach=%q", e.m.pendingAttach)
	}
	if e.cmd != nil {
		if _, isQuit := e.cmd().(tea.QuitMsg); isQuit {
			t.Error("must stay in the TUI, not tea.Quit")
		}
	}
	if e.m.flash == nil || !e.m.flash.isErr {
		t.Fatalf("expected the bounded-failure flash, got %+v", e.m.flash)
	}
	for _, want := range []string{second.agentID, first.agentID, "also exited at startup", "press [a] again"} {
		if !strings.Contains(e.m.flash.text, want) {
			t.Errorf("flash missing %q: %q", want, e.m.flash.text)
		}
	}
	if _, inFlight := e.m.inFlightOp(e.project); inFlight {
		t.Error("op gate must be released after the surfaced failure")
	}
	if sess := coorde2e.FleetSessions(t); len(sess) != 0 {
		t.Errorf("fleet sessions = %v, want none after a bounded failure", sess)
	}
	if pid, ok := coordlock.CurrentActiveOwnerPID(e.project); ok && coorde2e.PIDAlive(pid) {
		t.Errorf("lease still held by live pid %d after a bounded failure", pid)
	}

	// Fresh press: full recovery again, now succeeding.
	e.pressA(t)
	third := e.runDispatch(t)
	if third.err != nil || third.agentID == "" {
		t.Fatalf("third dispatch: err=%v agent=%q", third.err, third.agentID)
	}
	e.feed(third)
	e.wantCalls(t, 3)
	repl := e.wantAttachedTo(t, third.agentID)
	wantLinked(t, repl, second.agentID)
	if _, err := agent.Load(first.agentID); err == nil {
		t.Errorf("first corpse %s still listed as live", first.agentID)
	}
}
