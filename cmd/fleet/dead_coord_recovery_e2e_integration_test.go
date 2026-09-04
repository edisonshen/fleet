//go:build integration && (linux || darwin)

// End-to-end coverage for dead-coordinator recovery through the REAL
// process boundary — no stubs anywhere on the path:
//
//	fleet dispatch --coord-spawn  (built binary, subprocess)
//	  └─ spawn.Spawn → tmux new-session (isolated FLEET_TMUX_SOCKET)
//	       └─ fleet coord-run --standby   (real supervisor, real flock)
//	            └─ sh -c "claude ..."      (fake claude shim on PATH)
//
// The scenarios mirror what an operator hits when [a] lands on a coord
// that died days ago: the record is still on disk, its tmux session and
// supervisor are gone, its coord-state.json stopped ticking. Each test
// makes a REAL coord, kills it the way a crash does (SIGKILL the
// supervisor so nothing archives the record), backdates the freshness
// signals, and then re-dispatches exactly as the TUI does.
//
// Fenced behind the integration tag like the other coord-run tests: each
// case boots real standby supervisors. FLEET_STANDBY_TIMEOUT=3s bounds
// any standby a failed assertion leaves behind, and every test kills its
// coords + tmux server on cleanup.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/coordlock"
	"github.com/edisonshen/fleet/internal/handoff"
	"github.com/edisonshen/fleet/internal/testutil/coorde2e"
	"github.com/edisonshen/fleet/internal/tmux"
)

// deadCoordE2E is the per-test harness: built binary, isolated home +
// socket (requireTmux), fake claude first on PATH, a registered project.
type deadCoordE2E struct {
	bin      string
	project  string
	repo     string
	modeFile string
}

func newDeadCoordE2E(t *testing.T, project string) *deadCoordE2E {
	t.Helper()
	bin := buildFleetBinary(t) // before PATH is narrowed: needs `go`
	requireTmux(t)
	setupFleetHome(t)
	t.Setenv("FLEET_STANDBY_TIMEOUT", "3s")
	fakeDir := t.TempDir()
	modeFile := coorde2e.FakeClaude(t, fakeDir)
	t.Setenv("PATH", fakeDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	repo := coorde2e.SeedProject(t, project)
	t.Cleanup(func() { coorde2e.KillAllCoords(t) })
	return &deadCoordE2E{bin: bin, project: project, repo: repo, modeFile: modeFile}
}

func (h *deadCoordE2E) dispatch(t *testing.T) coorde2e.DispatchResult {
	t.Helper()
	return coorde2e.Dispatch(t, h.bin, h.project, h.repo)
}

// spawnLiveCoord dispatches a fresh coord and waits until it owns the
// lease with a live session.
func (h *deadCoordE2E) spawnLiveCoord(t *testing.T) *agent.Record {
	t.Helper()
	res := h.dispatch(t)
	if res.ExitCode != 0 {
		t.Fatalf("seed dispatch exit=%d, want 0\n%s", res.ExitCode, res.Out)
	}
	id := coorde2e.SpawnedID(res.Out)
	if id == "" {
		t.Fatalf("seed dispatch printed no `agent <id> spawned` line\n%s", res.Out)
	}
	return coorde2e.WaitLiveCoord(t, h.project, id)
}

// seedDaysDeadCoord makes the operator's corpse: a coord that ran, died
// hard, and has been dead long enough that every freshness signal is stale.
func (h *deadCoordE2E) seedDaysDeadCoord(t *testing.T) *agent.Record {
	t.Helper()
	rec := h.spawnLiveCoord(t)
	coorde2e.SeedInFlightWorker(t, h.project, "fix-login", "wkr00001", "implementing")
	coorde2e.KillCoordCorpse(t, rec)
	coorde2e.AgeDeadCoord(t, h.project, rec.ID)
	return rec
}

func liveCoordIDs(t *testing.T) []string {
	t.Helper()
	recs, err := agent.List()
	if err != nil {
		t.Fatalf("agent.List: %v", err)
	}
	var ids []string
	for _, r := range recs {
		ids = append(ids, r.ID)
	}
	return ids
}

// TestE2E_DeadCoordRecovery_ReplacementInheritsAndReapsPredecessor is the
// operator's scenario end to end: coord X died days ago with a worker in
// flight; one `--coord-spawn` dispatch (what [a] runs) must
//
//   - detect X as dead and synthesize a recovery handoff carrying X's
//     in-flight worker,
//   - boot replacement Y under a real coord-run supervisor that acquires
//     the project lease,
//   - physically deliver the resume prompt into Y's pane,
//   - link Y to X (predecessor_id, handoff_number, last_handoff_path),
//   - and archive X only once Y provably owns the lease — leaving exactly
//     one coord record and one fleet-* session.
func TestE2E_DeadCoordRecovery_ReplacementInheritsAndReapsPredecessor(t *testing.T) {
	requireTmux(t)
	h := newDeadCoordE2E(t, "e2e-recover")
	dead := h.seedDaysDeadCoord(t)

	res := h.dispatch(t)
	if res.ExitCode != 0 {
		t.Fatalf("recovery dispatch exit=%d, want 0\n%s", res.ExitCode, res.Out)
	}
	if !strings.Contains(res.Out, "recovering dead coord "+dead.ID+" for project "+h.project) {
		t.Fatalf("dispatch did not announce recovery of %s\n%s", dead.ID, res.Out)
	}
	newID := coorde2e.SpawnedID(res.Out)
	if newID == "" || newID == dead.ID {
		t.Fatalf("recovery dispatch advertised %q, want a fresh replacement id (dead=%s)\n%s", newID, dead.ID, res.Out)
	}
	if !strings.Contains(res.Out, "prompt:  delivered") {
		t.Errorf("resume prompt not reported delivered\n%s", res.Out)
	}
	if !strings.Contains(res.Out, "reaped stale coord "+dead.ID) {
		t.Errorf("dispatch did not reap predecessor %s after the successor took the lease\n%s", dead.ID, res.Out)
	}

	repl := coorde2e.WaitLiveCoord(t, h.project, newID)
	if repl.PredecessorID != dead.ID {
		t.Errorf("replacement predecessor_id = %q, want %q", repl.PredecessorID, dead.ID)
	}
	if repl.HandoffNumber != dead.HandoffNumber+1 {
		t.Errorf("replacement handoff_number = %d, want %d", repl.HandoffNumber, dead.HandoffNumber+1)
	}
	if repl.Project != dead.Project || repl.TaskID != dead.TaskID || repl.Engine != dead.Engine {
		t.Errorf("replacement lost identity: project=%q task=%q engine=%q (dead: %q %q %q)",
			repl.Project, repl.TaskID, repl.Engine, dead.Project, dead.TaskID, dead.Engine)
	}
	if !repl.LeaseWrapped || repl.SupervisorPID <= 0 {
		t.Errorf("replacement not lease-wrapped under a real supervisor: lease_wrapped=%v supervisor_pid=%d",
			repl.LeaseWrapped, repl.SupervisorPID)
	}
	owner, ok := coordlock.CurrentOwner(h.project)
	if !ok || owner.AgentID != newID {
		t.Errorf("lease owner = %+v ok=%v, want agent %s", owner, ok, newID)
	}

	// The synth handoff doc must exist, be a recovery-synth, and carry the
	// dead coord's in-flight worker so the successor picks it up.
	if repl.LastHandoffPath == nil || *repl.LastHandoffPath == "" {
		t.Fatalf("replacement has no last_handoff_path")
	}
	docPath := *repl.LastHandoffPath
	raw, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("read recovery doc %s: %v", docPath, err)
	}
	doc := string(raw)
	for _, want := range []string{handoff.TypeRecoverySynth, "fix-login", "wkr00001", "implementing"} {
		if !strings.Contains(doc, want) {
			t.Errorf("recovery doc %s missing %q:\n%s", docPath, want, doc)
		}
	}

	// The resume prompt physically reached the replacement's pane: the
	// fake claude acknowledges each line it consumes.
	coorde2e.WaitFor(t, 10*time.Second, "resume prompt ack in replacement pane", func() bool {
		pane, err := tmux.CapturePane(repl.TmuxSession)
		return err == nil && strings.Contains(string(pane), coorde2e.PromptAck)
	})

	// Predecessor archived (record moved), successor linked back to it.
	if _, err := agent.Load(dead.ID); err == nil {
		t.Errorf("dead coord %s still unarchived after successor owns the lease", dead.ID)
	}
	if !coorde2e.Archived(t, dead.ID) {
		t.Errorf("dead coord %s not in the archive", dead.ID)
	}
	if ids := liveCoordIDs(t); len(ids) != 1 || ids[0] != newID {
		t.Errorf("live records = %v, want exactly [%s]", ids, newID)
	}
	if sess := coorde2e.FleetSessions(t); len(sess) != 1 || sess[0] != repl.TmuxSession {
		t.Errorf("fleet tmux sessions = %v, want exactly [%s]", sess, repl.TmuxSession)
	}
}

// TestE2E_DeadCoordRecovery_ReplacementDiesAtStartup_NextAttemptRecovers
// covers the "claude is broken right now" branch: the recovery's
// replacement exits at startup. Dispatch must not loop or leave a runaway
// standby; the corpse stays recoverable (never archived without a live
// successor owning the lease); and once claude starts again the next
// dispatch — the TUI's bounded retry / the operator's next [a] — recovers
// into a single live coord and reaps every dead predecessor.
func TestE2E_DeadCoordRecovery_ReplacementDiesAtStartup_NextAttemptRecovers(t *testing.T) {
	requireTmux(t)
	h := newDeadCoordE2E(t, "e2e-flaky")
	dead := h.seedDaysDeadCoord(t)

	coorde2e.SetMode(t, h.modeFile, coorde2e.ModeExit)
	failed := h.dispatch(t)
	if failed.ExitCode == vetoExitCode {
		t.Fatalf("dispatch with a dead corpse must not be vetoed as 'coord already running' (exit 75)\n%s", failed.Out)
	}
	if !strings.Contains(failed.Out, "recovering dead coord "+dead.ID) {
		t.Fatalf("dispatch did not attempt recovery of %s\n%s", dead.ID, failed.Out)
	}
	if strings.Contains(failed.Out, "reaped stale coord "+dead.ID) {
		t.Errorf("predecessor %s reaped although no successor ever owned the lease\n%s", dead.ID, failed.Out)
	}
	// Whether spawn reported the supervisor's early exit (exit 1) or the
	// session outlived the readiness poll long enough to be advertised
	// (exit 0 + undelivered prompt), the end state is the same: nothing
	// alive, corpse kept, lease free.
	coorde2e.WaitFor(t, 15*time.Second, "crashed replacement and its standby to be gone", func() bool {
		if len(coorde2e.FleetSessions(t)) != 0 {
			return false
		}
		pid, ok := coordlock.CurrentActiveOwnerPID(h.project)
		return !ok || !coorde2e.PIDAlive(pid)
	})
	if _, err := agent.Load(dead.ID); err != nil {
		t.Fatalf("corpse %s must survive a failed recovery, got %v", dead.ID, err)
	}

	// claude works again → the next attempt recovers for real.
	coorde2e.SetMode(t, h.modeFile, coorde2e.ModeOK)
	res := h.dispatch(t)
	if res.ExitCode != 0 {
		t.Fatalf("retry dispatch exit=%d, want 0\n%s", res.ExitCode, res.Out)
	}
	newID := coorde2e.SpawnedID(res.Out)
	if newID == "" || newID == dead.ID {
		t.Fatalf("retry advertised %q, want a fresh replacement (dead=%s)\n%s", newID, dead.ID, res.Out)
	}
	repl := coorde2e.WaitLiveCoord(t, h.project, newID)
	if repl.PredecessorID == "" {
		t.Errorf("replacement %s has no predecessor link", newID)
	}
	if !coorde2e.Archived(t, dead.ID) {
		t.Errorf("original corpse %s not archived after successful recovery", dead.ID)
	}
	if ids := liveCoordIDs(t); len(ids) != 1 || ids[0] != newID {
		t.Errorf("live records = %v, want exactly [%s]", ids, newID)
	}
	if sess := coorde2e.FleetSessions(t); len(sess) != 1 {
		t.Errorf("fleet tmux sessions = %v, want exactly one", sess)
	}
}

// TestE2E_DeadCoordRecovery_LiveCoordIsNeverDuplicated pins the other side
// of the contract: when the coord is ALIVE, [a]'s dispatch must never
// spawn beside it. Before its first tick the cold-start signals veto with
// exit 75 (the TUI's "attach the live leader" path); after a tick the
// idempotent fast path advertises the SAME id with exit 0. In both cases
// the supervisor, lease owner, record set and session set are unchanged.
func TestE2E_DeadCoordRecovery_LiveCoordIsNeverDuplicated(t *testing.T) {
	requireTmux(t)
	h := newDeadCoordE2E(t, "e2e-live")
	live := h.spawnLiveCoord(t)

	veto := h.dispatch(t)
	if veto.ExitCode != vetoExitCode {
		t.Fatalf("dispatch beside a live pre-tick coord exit=%d, want %d (veto)\n%s", veto.ExitCode, vetoExitCode, veto.Out)
	}
	if coorde2e.SpawnedID(veto.Out) != "" {
		t.Errorf("veto path must not advertise a spawned agent\n%s", veto.Out)
	}

	// Simulate the coord's first /coordinator tick: coord-state.json is
	// written and the cold-start claim cleared.
	coorde2e.SeedInFlightWorker(t, h.project, "fix-login", "wkr00001", "implementing")
	if err := clearCoordPendingClaim(h.project); err != nil {
		t.Fatalf("clear pending claim: %v", err)
	}
	attach := h.dispatch(t)
	if attach.ExitCode != 0 {
		t.Fatalf("dispatch beside a live ticked coord exit=%d, want 0\n%s", attach.ExitCode, attach.Out)
	}
	if got := coorde2e.SpawnedID(attach.Out); got != live.ID {
		t.Errorf("fast path advertised %q, want the live coord %s\n%s", got, live.ID, attach.Out)
	}

	after, err := agent.Load(live.ID)
	if err != nil {
		t.Fatalf("live coord record vanished: %v", err)
	}
	if after.SupervisorPID != live.SupervisorPID || !coorde2e.PIDAlive(after.SupervisorPID) {
		t.Errorf("live coord supervisor changed/died: before=%d after=%d", live.SupervisorPID, after.SupervisorPID)
	}
	if pid, ok := coordlock.CurrentActiveOwnerPID(h.project); !ok || pid != live.SupervisorPID {
		t.Errorf("lease owner pid = %d ok=%v, want %d", pid, ok, live.SupervisorPID)
	}
	if ids := liveCoordIDs(t); len(ids) != 1 || ids[0] != live.ID {
		t.Errorf("live records = %v, want exactly [%s]", ids, live.ID)
	}
	if sess := coorde2e.FleetSessions(t); len(sess) != 1 || sess[0] != live.TmuxSession {
		t.Errorf("fleet tmux sessions = %v, want exactly [%s]", sess, live.TmuxSession)
	}
}

// TestE2E_DeadCoordRecovery_BareLegacyRecordIsKept pins the conservative
// archival rule: a record with NO supervisor stamp (pre-lease legacy, or
// a hand-written record) cannot be proven dead by pid+pid_start, so
// recovery still hands its work to a replacement but leaves the record on
// the dashboard for the operator's [x] instead of guessing.
func TestE2E_DeadCoordRecovery_BareLegacyRecordIsKept(t *testing.T) {
	requireTmux(t)
	h := newDeadCoordE2E(t, "e2e-legacy")

	legacy := agent.New("1e9acy01")
	legacy.TaskID = CoordTaskIDPrefix + h.project
	legacy.Project = h.project
	legacy.IsCoord = true
	legacy.Engine = "claude-code"
	legacy.TmuxSession = tmux.SessionName(legacy.ID)
	legacy.SpawnedAt = time.Now().Add(-72 * time.Hour).UTC()
	if err := legacy.Write(); err != nil {
		t.Fatalf("write legacy record: %v", err)
	}
	coorde2e.SeedInFlightWorker(t, h.project, "fix-login", "wkr00001", "implementing")
	coorde2e.AgeDeadCoord(t, h.project, legacy.ID)

	res := h.dispatch(t)
	if res.ExitCode != 0 {
		t.Fatalf("recovery dispatch exit=%d, want 0\n%s", res.ExitCode, res.Out)
	}
	if !strings.Contains(res.Out, "recovering dead coord "+legacy.ID) {
		t.Fatalf("dispatch did not recover from legacy record %s\n%s", legacy.ID, res.Out)
	}
	newID := coorde2e.SpawnedID(res.Out)
	if newID == "" || newID == legacy.ID {
		t.Fatalf("advertised %q, want a fresh replacement\n%s", newID, res.Out)
	}
	if strings.Contains(res.Out, "reaped stale coord "+legacy.ID) {
		t.Errorf("bare legacy record %s was reaped without pid proof\n%s", legacy.ID, res.Out)
	}
	repl := coorde2e.WaitLiveCoord(t, h.project, newID)
	if repl.PredecessorID != legacy.ID {
		t.Errorf("replacement predecessor_id = %q, want %q", repl.PredecessorID, legacy.ID)
	}
	if _, err := agent.Load(legacy.ID); err != nil {
		t.Errorf("legacy record %s must stay on disk for [x], got %v", legacy.ID, err)
	}
	if coorde2e.Archived(t, legacy.ID) {
		t.Errorf("legacy record %s must not be archived", legacy.ID)
	}
	ids := liveCoordIDs(t)
	if len(ids) != 2 {
		t.Errorf("live records = %v, want the kept legacy record + the replacement", ids)
	}
	if doc := repl.LastHandoffPath; doc == nil || !strings.HasPrefix(filepath.Base(*doc), legacy.ID) {
		t.Errorf("replacement handoff doc = %v, want a synth doc minted for %s", doc, legacy.ID)
	}
}
