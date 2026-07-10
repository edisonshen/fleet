//go:build linux || darwin

package main

// coord_lease_unix_test.go — KP3 (DESIGN-coord-no-auto-kill, task
// coord-no-auto-kill-ac54): sweepStaleCompetitors is DETECT + REPORT
// only. The new-leader sweep fired on a staleness heuristic (records of
// coords presumed stale after a lease win), so it must never kill;
// reaping is operator-gated (`fleet gc --apply` for corpses, `fleet rm`
// / `fleet handoff` for live ones). The no-kill guarantee is asserted
// STRUCTURALLY (source-level absence of kill invocations), not via
// injected seams — the seams themselves are deleted.

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/coordlock"
	"github.com/edisonshen/fleet/internal/gc"
	"github.com/edisonshen/fleet/internal/state"
)

// readQuarantineEvents (shared helper) lives in
// coord_run_quarantine_test.go — untagged so the platform-neutral
// standby-poll quarantine tests can use it too.

// Test 1 (plan): sweep report-only. One stale lease-wrapped stamped
// competitor + one bare legacy coord + self: exactly one stderr report
// and one fleetlog coord.quarantine{stale-competitor} event — for the
// competitor only. The bare coord and self are never reported, and
// nothing is killed (structural test below).
func TestSweepStaleCompetitors_ReportOnly(t *testing.T) {
	home := setupFleetHome(t)
	t.Setenv("XDG_STATE_HOME", "") // fleetlog writes under FLEET_HOME/logs
	const project = "rainier"

	self := agent.New("winner1")
	self.Project = project
	self.TaskID = "coord-" + project
	self.SupervisorPID = os.Getpid()
	self.LeaseWrapped = true
	self.TmuxSession = "fleet-winner1"
	if err := self.Write(); err != nil {
		t.Fatalf("write self: %v", err)
	}
	// A stamped stale competitor (another coord-run supervisor).
	competitor := agent.New("compet1")
	competitor.Project = project
	competitor.TaskID = "coord-" + project
	competitor.SupervisorPID = 999999
	competitor.SupervisorPidStart = 42
	competitor.LeaseWrapped = true
	competitor.TmuxSession = "fleet-compet1"
	if err := competitor.Write(); err != nil {
		t.Fatalf("write competitor: %v", err)
	}
	// A live LEGACY/BARE coord (no supervisor, not lease-wrapped): out of
	// scope, never reported, never touched.
	bare := agent.New("barecord")
	bare.Project = project
	bare.TaskID = "coord-" + project
	bare.TmuxSession = "fleet-barecord"
	bare.LeaseWrapped = false
	if err := bare.Write(); err != nil {
		t.Fatalf("write bare: %v", err)
	}

	var stderr bytes.Buffer
	sweepStaleCompetitors(self.ID, project, &stderr)

	got := stderr.String()
	if !strings.Contains(got, "compet1") || !strings.Contains(got, "report-only") {
		t.Fatalf("stderr missing the competitor report line; got:\n%s", got)
	}
	if strings.Count(got, "report-only") != 1 {
		t.Fatalf("want exactly one report line, got:\n%s", got)
	}
	if strings.Contains(got, "barecord") || strings.Contains(got, "winner1") {
		t.Fatalf("bare coord / self must not be reported; got:\n%s", got)
	}

	events := readQuarantineEvents(t, home)
	if len(events) != 1 {
		t.Fatalf("want exactly 1 coord.quarantine event, got %d: %v", len(events), events)
	}
	ev := events[0]
	if ev["agent"] != "compet1" || ev["proj"] != project {
		t.Fatalf("event agent/proj = %v/%v, want compet1/%s", ev["agent"], ev["proj"], project)
	}
	data, _ := ev["data"].(map[string]any)
	if data == nil || data["reason"] != "stale-competitor" {
		t.Fatalf("event data.reason = %v, want stale-competitor", data)
	}
	msg, _ := ev["msg"].(string)
	if msg == "" || !strings.Contains(got, msg) {
		t.Fatalf("event msg %q must match the stderr report string exactly; stderr:\n%s", msg, got)
	}
}

// TestProductionAcquireLease_CommitsHandoffJournalOnAcquire is a /review
// testing-specialist gap: every coord-run test stubs opts.acquireLease, so
// productionAcquireLease's real step-3a — the winner commits (deletes) any
// in-flight handoff journal + barrier on acquire (Decision 7) — had zero
// direct test coverage. Seed a leftover journal for the project, invoke the
// real production closure, and assert the journal is gone once the lease is
// acquired.
func TestProductionAcquireLease_CommitsHandoffJournalOnAcquire(t *testing.T) {
	setupFleetHome(t)
	const project = "rainier"
	const agentID = "pal-winner"

	rec := agent.New(agentID)
	rec.Project = project
	rec.TaskID = CoordTaskIDPrefix + project
	if err := rec.Write(); err != nil {
		t.Fatalf("write record: %v", err)
	}

	if err := coordlock.CreateHandoffJournal(coordlock.HandoffJournal{
		Project: project, SuccessorID: agentID, BarrierID: "b-pal",
		SuccessorPID: 999999, SuccessorPidStart: 424242,
	}); err != nil {
		t.Fatalf("seed leftover handoff journal: %v", err)
	}
	if _, ok, err := coordlock.ReadHandoffJournal(project); err != nil || !ok {
		t.Fatalf("precondition: journal must be seeded, ok=%v err=%v", ok, err)
	}

	var stderr bytes.Buffer
	acquire := productionAcquireLease(coordRunOpts{agentID: agentID, project: project}, &stderr)
	lease, acquired, holders, err := acquire()
	if err != nil || !acquired || lease == nil {
		t.Fatalf("productionAcquireLease: acquired=%v err=%v lease=%v (stderr=%s)", acquired, err, lease, stderr.String())
	}
	defer lease.Release()
	if holders != nil {
		t.Errorf("live-holder slice = %v, want nil (flock-only never populates it)", holders)
	}
	if _, ok, rerr := coordlock.ReadHandoffJournal(project); rerr != nil || ok {
		t.Fatalf("handoff journal must be committed (deleted) on acquire; ok=%v err=%v (stderr=%s)", ok, rerr, stderr.String())
	}
}

// The unstamped lease-wrapped standby family (the sweep's second former
// kill target) is also detect+report now: reported, never session-killed.
func TestSweepStaleCompetitors_UnstampedStandbyReportedNotKilled(t *testing.T) {
	home := setupFleetHome(t)
	t.Setenv("XDG_STATE_HOME", "")
	const project = "rainier"

	self := agent.New("winner1")
	self.Project = project
	self.TaskID = "coord-" + project
	self.SupervisorPID = os.Getpid()
	self.LeaseWrapped = true
	self.TmuxSession = "fleet-winner1"
	if err := self.Write(); err != nil {
		t.Fatalf("write self: %v", err)
	}
	loser := agent.New("loser1")
	loser.Project = project
	loser.TaskID = "coord-" + project
	loser.TmuxSession = "fleet-loser1"
	loser.LeaseWrapped = true // an unstamped lease-wrapped standby
	if err := loser.Write(); err != nil {
		t.Fatalf("write loser: %v", err)
	}

	var stderr bytes.Buffer
	sweepStaleCompetitors(self.ID, project, &stderr)

	got := stderr.String()
	if !strings.Contains(got, "loser1") || !strings.Contains(got, "report-only") {
		t.Fatalf("stderr missing the unstamped-standby report; got:\n%s", got)
	}
	events := readQuarantineEvents(t, home)
	if len(events) != 1 {
		t.Fatalf("want 1 coord.quarantine event for the standby, got %d", len(events))
	}
	if events[0]["agent"] != "loser1" {
		t.Fatalf("event agent = %v, want loser1", events[0]["agent"])
	}
}

// Structural no-kill proof (plan acceptance: "no kill invocation,
// asserted structurally"). The sweep function's source must contain no
// kill call, and the old sweepKillCoordFn/sweepKillSessionFn seams must
// be deleted from the file entirely.
func TestSweepStaleCompetitors_StructurallyKillFree(t *testing.T) {
	src, err := os.ReadFile("coord_lease_unix.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	file := string(src)
	for _, seam := range []string{"sweepKillCoordFn", "sweepKillSessionFn"} {
		if strings.Contains(file, seam) {
			t.Errorf("kill seam %s still present in coord_lease_unix.go; KP3 deletes it", seam)
		}
	}
	// Extract the sweepStaleCompetitors function body.
	start := strings.Index(file, "func sweepStaleCompetitors(")
	if start < 0 {
		t.Fatal("sweepStaleCompetitors not found in coord_lease_unix.go")
	}
	rest := file[start:]
	end := strings.Index(rest[1:], "\nfunc ")
	if end < 0 {
		end = len(rest) - 1
	}
	body := rest[:end+1]
	// Case-sensitive "Kill" catches KillCoordIfIdentityMatches, tmux.Kill,
	// KillSession, syscall.Kill — every kill surface in this codebase.
	if strings.Contains(body, "Kill") {
		t.Fatalf("sweepStaleCompetitors contains a kill invocation:\n%s", body)
	}
	if strings.Contains(body, "syscall.") {
		t.Fatalf("sweepStaleCompetitors must not signal processes directly:\n%s", body)
	}
}

// Regression (found via manual `fleet gc` smoke on this branch): a DEAD
// stale competitor whose record was never exe-stamped (the
// stampSupervisorWithRetry warning path) made KillCoordIfIdentityMatches
// return ErrKillRefused, so gc --apply could never archive the corpse.
// The production KillCoord wrapper re-verifies the pid is dead
// (pid+pid_start) and then tolerates the typed refusal — the refusal
// gates protect LIVE processes, and the corpse re-check proves there is
// nothing live to signal. The record must end up archived.
func TestWireGCCoordDeps_DeadUnstampedCompetitorStillReaped(t *testing.T) {
	setupFleetHome(t)
	const project = "rainier"
	dead := agent.New("deadcomp2")
	dead.Project = project
	dead.TaskID = "coord-" + project
	dead.TmuxSession = "fleet-deadcomp2" // no live session; Cleanup is best-effort
	dead.SupervisorPID = 999999          // beyond pid_max on darwin/linux defaults
	dead.SupervisorPidStart = 42
	dead.LeaseWrapped = true
	// SupervisorExePath deliberately EMPTY (partial stamp).
	if err := dead.Write(); err != nil {
		t.Fatalf("write record: %v", err)
	}

	var deps gc.Deps
	wireGCCoordDeps(&deps)
	if err := deps.KillCoord(gc.CoordKillTarget{
		Pid: 999999, PidStart: 42, AgentID: "deadcomp2", Project: project,
	}); err != nil {
		t.Fatalf("KillCoord on a provably-dead unstamped competitor must reap, got %v", err)
	}
	if _, err := agent.Load("deadcomp2"); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("record must be archived after the reap, Load err=%v", err)
	}
}

// The wrapper's own TOCTOU belt: a target whose pid+pid_start reads
// ALIVE at the destructive boundary is refused outright — gc never
// signals (or session-reaps) a live supervisor, even if the classifier
// mislabeled it.
func TestWireGCCoordDeps_LiveTargetRefused(t *testing.T) {
	setupFleetHome(t)
	const project = "rainier"
	self := os.Getpid()
	selfStart, ok := coordlock.PidStartNanos(self)
	if !ok {
		t.Fatal("cannot read own pid_start")
	}
	rec := agent.New("livecomp2")
	rec.Project = project
	rec.TaskID = "coord-" + project
	rec.TmuxSession = "fleet-livecomp2"
	rec.SupervisorPID = self
	rec.SupervisorPidStart = selfStart
	rec.LeaseWrapped = true
	if err := rec.Write(); err != nil {
		t.Fatalf("write record: %v", err)
	}

	var deps gc.Deps
	wireGCCoordDeps(&deps)
	if err := deps.KillCoord(gc.CoordKillTarget{
		Pid: self, PidStart: selfStart, AgentID: "livecomp2", Project: project,
	}); err == nil {
		t.Fatal("KillCoord must refuse a live pid+pid_start target")
	}
	if _, err := agent.Load("livecomp2"); err != nil {
		t.Fatalf("live record must be untouched, Load err=%v", err)
	}
}
