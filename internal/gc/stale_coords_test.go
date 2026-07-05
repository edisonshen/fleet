package gc

// stale_coords_test.go — KindStaleCoords (DESIGN-coord-no-auto-kill, task
// coord-no-auto-kill-ac54). The lease sweep's former kill targets become
// gc candidates in two classes with a HARD live/dead split:
//
//	(a) stamped competitors — LeaseWrapped && SupervisorPID>0 &&
//	    SupervisorPID != active lease owner. Supervisor DEAD (pid+pid_start
//	    probe) -> reapable under --apply via the KillCoord seam; ALIVE ->
//	    listed live-stale with the fleet rm / fleet handoff hint, --apply
//	    SKIPS with a printed reason. gc NEVER signals a live pid.
//	(b) unstamped lease-wrapped standbys — LeaseWrapped && SupervisorPID==0
//	    with a live tmux session older than the standby timeout -> session
//	    reaped under --apply.
//
//	Bare legacy coords (SupervisorPID==0 && !LeaseWrapped) are NEVER
//	candidates in either class.

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
)

// staleCoordDeps extends stubDeps with the stale-coords seams wired to
// deterministic fakes. Tests override per-case behavior.
func staleCoordDeps(now time.Time) Deps {
	d := stubDeps(now)
	d.ActiveLeaseOwnerPID = func(string) (int, bool) { return 0, false }
	d.CoordSupervisorAlive = func(int, int64) bool { return false }
	d.KillCoord = func(CoordKillTarget) error {
		return errors.New("staleCoordDeps: KillCoord should not run")
	}
	d.CoordStandbyTimeout = 10 * time.Minute
	return d
}

func staleCoordRecord(id, project string, supPid int, supStart int64, leaseWrapped bool, spawnedAt time.Time) *agent.Record {
	r := agent.New(id)
	r.Project = project
	r.TaskID = "coord-" + project
	r.TmuxSession = "fleet-" + id
	r.SupervisorPID = supPid
	r.SupervisorPidStart = supStart
	r.LeaseWrapped = leaseWrapped
	r.SpawnedAt = spawnedAt
	return r
}

func findStaleCoordAction(r Report, target string) *Action {
	for i := range r.Actions {
		if r.Actions[i].Kind == KindStaleCoords && r.Actions[i].Target == target {
			return &r.Actions[i]
		}
	}
	return nil
}

// Plan test 8: gc dead competitor. A stamped competitor whose
// CoordSupervisorAlive(pid,pidStart) is false is listed; --apply invokes
// KillCoord with the correct gc-local target; dry-run never invokes it.
func TestStaleCoords_DeadCompetitor_ReapedUnderApplyOnly(t *testing.T) {
	now := time.Now()
	const project = "rainier"
	dead := staleCoordRecord("deadcomp", project, 4242, 999, true, now.Add(-time.Hour))

	var killCalls []CoordKillTarget
	deps := staleCoordDeps(now)
	deps.ListAgents = func() ([]*agent.Record, error) { return []*agent.Record{dead}, nil }
	deps.ActiveLeaseOwnerPID = func(p string) (int, bool) {
		if p != project {
			t.Errorf("ActiveLeaseOwnerPID probed project %q, want %q", p, project)
		}
		return 777777, true // a different pid owns the lease -> dead is a competitor
	}
	deps.CoordSupervisorAlive = func(pid int, pidStart int64) bool {
		if pid != 4242 || pidStart != 999 {
			t.Errorf("CoordSupervisorAlive probed %d/%d, want 4242/999", pid, pidStart)
		}
		return false // provably dead
	}
	deps.KillCoord = func(target CoordKillTarget) error {
		killCalls = append(killCalls, target)
		return nil
	}

	// Dry-run: listed, KillCoord NEVER invoked.
	rep, err := Reconcile(Options{Kinds: []Kind{KindStaleCoords}}, deps)
	if err != nil {
		t.Fatalf("Reconcile dry-run: %v", err)
	}
	act := findStaleCoordAction(rep, "deadcomp")
	if act == nil {
		t.Fatalf("dead competitor not listed in dry-run; actions=%v", rep.Actions)
	}
	if act.Verb != VerbWouldKill {
		t.Errorf("dry-run verb = %s, want %s", act.Verb, VerbWouldKill)
	}
	if len(killCalls) != 0 {
		t.Fatalf("dry-run invoked KillCoord %d times, want 0", len(killCalls))
	}

	// Apply: reaped via the seam with the full identity.
	rep, err = Reconcile(Options{Apply: true, Kinds: []Kind{KindStaleCoords}}, deps)
	if err != nil {
		t.Fatalf("Reconcile apply: %v", err)
	}
	act = findStaleCoordAction(rep, "deadcomp")
	if act == nil || act.Verb != VerbKilled {
		t.Fatalf("apply action = %+v, want verb=%s", act, VerbKilled)
	}
	if len(killCalls) != 1 {
		t.Fatalf("apply invoked KillCoord %d times, want 1", len(killCalls))
	}
	got := killCalls[0]
	if got.Pid != 4242 || got.PidStart != 999 || got.AgentID != "deadcomp" || got.Project != project {
		t.Fatalf("KillCoord target = %+v, want pid=4242 start=999 agent=deadcomp project=%s", got, project)
	}
}

// Plan test 9: gc live competitor. Same record shape but the supervisor
// pid is ALIVE — listed live-stale with the fleet rm / fleet handoff
// hint; --apply SKIPS with a printed reason; KillCoord never invoked
// under ANY flag (the gc-side pre-probe is the enforcement).
func TestStaleCoords_LiveCompetitor_NeverSignaled(t *testing.T) {
	now := time.Now()
	const project = "rainier"
	liveRec := staleCoordRecord("livecomp", project, 4242, 999, true, now.Add(-time.Hour))

	var killCalls int
	deps := staleCoordDeps(now)
	deps.ListAgents = func() ([]*agent.Record, error) { return []*agent.Record{liveRec}, nil }
	deps.ActiveLeaseOwnerPID = func(string) (int, bool) { return 777777, true }
	deps.CoordSupervisorAlive = func(int, int64) bool { return true } // ALIVE
	deps.KillCoord = func(CoordKillTarget) error { killCalls++; return nil }

	for _, apply := range []bool{false, true} {
		rep, err := Reconcile(Options{Apply: apply, Aggressive: true, Kinds: []Kind{KindStaleCoords}}, deps)
		if err != nil {
			t.Fatalf("Reconcile apply=%v: %v", apply, err)
		}
		act := findStaleCoordAction(rep, "livecomp")
		if act == nil {
			t.Fatalf("live competitor not listed (apply=%v)", apply)
		}
		if act.Verb != VerbSurface {
			t.Errorf("apply=%v verb = %s, want %s (surface-only for a live pid)", apply, act.Verb, VerbSurface)
		}
		if !strings.Contains(act.Reason, "fleet rm livecomp") ||
			!strings.Contains(act.Reason, "fleet handoff livecomp") {
			t.Errorf("apply=%v reason %q missing the explicit per-target commands", apply, act.Reason)
		}
		if apply && !strings.Contains(act.Reason, "skip") {
			t.Errorf("--apply must print a skip reason for a live competitor, got %q", act.Reason)
		}
	}
	if killCalls != 0 {
		t.Fatalf("KillCoord invoked %d times against a LIVE pid; gc --apply must never signal a live pid", killCalls)
	}
}

// Plan test 10: gc unstamped-standby class + bare-coord protection. A
// LeaseWrapped, SupervisorPID==0 record whose tmux session is still
// present past the standby timeout is listed and its SESSION reaped under
// --apply. A bare coord (same shape, !LeaseWrapped) is never listed. A
// young standby (within the timeout) is never listed.
func TestStaleCoords_UnstampedStandbyReaped_BareCoordNever(t *testing.T) {
	now := time.Now()
	const project = "rainier"
	wedged := staleCoordRecord("wedgedsb", project, 0, 0, true, now.Add(-20*time.Minute))
	young := staleCoordRecord("youngsb", project, 0, 0, true, now.Add(-time.Minute))
	bare := staleCoordRecord("barecord", project, 0, 0, false, now.Add(-48*time.Hour))

	var killedSessions []string
	deps := staleCoordDeps(now)
	deps.ListAgents = func() ([]*agent.Record, error) {
		return []*agent.Record{wedged, young, bare}, nil
	}
	deps.SessionAlive = func(string) (bool, error) { return true, nil }
	deps.KillSession = func(name string) error { killedSessions = append(killedSessions, name); return nil }

	// Dry-run lists the wedged standby only.
	rep, err := Reconcile(Options{Kinds: []Kind{KindStaleCoords}}, deps)
	if err != nil {
		t.Fatalf("Reconcile dry-run: %v", err)
	}
	if act := findStaleCoordAction(rep, "wedgedsb"); act == nil || act.Verb != VerbWouldKill {
		t.Fatalf("wedged standby action = %+v, want listed with %s", act, VerbWouldKill)
	}
	if act := findStaleCoordAction(rep, "youngsb"); act != nil {
		t.Fatalf("young standby (within standby timeout) must not be listed, got %+v", act)
	}
	if act := findStaleCoordAction(rep, "barecord"); act != nil {
		t.Fatalf("bare legacy coord must NEVER be a stale-coords candidate, got %+v", act)
	}
	if len(killedSessions) != 0 {
		t.Fatalf("dry-run killed sessions %v, want none", killedSessions)
	}

	// Apply reaps the wedged standby's session (and only it).
	rep, err = Reconcile(Options{Apply: true, Kinds: []Kind{KindStaleCoords}}, deps)
	if err != nil {
		t.Fatalf("Reconcile apply: %v", err)
	}
	if act := findStaleCoordAction(rep, "wedgedsb"); act == nil || act.Verb != VerbKilled {
		t.Fatalf("apply action = %+v, want verb=%s", act, VerbKilled)
	}
	if len(killedSessions) != 1 || killedSessions[0] != "fleet-wedgedsb" {
		t.Fatalf("killed sessions = %v, want [fleet-wedgedsb]", killedSessions)
	}
}

// The record whose SupervisorPID IS the active lease owner is the live
// leader — never a candidate (that protects the expired-heartbeat leader:
// CurrentActiveOwnerPID still names it while its record is stale).
func TestStaleCoords_ActiveOwnerNeverCandidate(t *testing.T) {
	now := time.Now()
	const project = "rainier"
	leader := staleCoordRecord("leader1", project, 4242, 999, true, now.Add(-time.Hour))

	deps := staleCoordDeps(now)
	deps.ListAgents = func() ([]*agent.Record, error) { return []*agent.Record{leader}, nil }
	deps.ActiveLeaseOwnerPID = func(string) (int, bool) { return 4242, true } // leader itself
	deps.CoordSupervisorAlive = func(int, int64) bool { return false }        // even if probe says dead

	rep, err := Reconcile(Options{Apply: true, Kinds: []Kind{KindStaleCoords}}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if act := findStaleCoordAction(rep, "leader1"); act != nil {
		t.Fatalf("the active lease owner must never be a candidate, got %+v", act)
	}
}

// No readable active lease owner (ok=false) -> class (a) cannot prove the
// record is a competitor -> not a candidate (conservative).
func TestStaleCoords_NoActiveOwner_NotACompetitor(t *testing.T) {
	now := time.Now()
	rec := staleCoordRecord("maybe1", "rainier", 4242, 999, true, now.Add(-time.Hour))

	deps := staleCoordDeps(now)
	deps.ListAgents = func() ([]*agent.Record, error) { return []*agent.Record{rec}, nil }
	deps.ActiveLeaseOwnerPID = func(string) (int, bool) { return 0, false }

	rep, err := Reconcile(Options{Apply: true, Kinds: []Kind{KindStaleCoords}}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if act := findStaleCoordAction(rep, "maybe1"); act != nil {
		t.Fatalf("no active owner -> not provably a competitor -> no candidate, got %+v", act)
	}
}

// Unwired platform seams (freebsd: no lease/kill primitives) fail safe:
// class (a) is skipped entirely; nothing is ever signaled.
func TestStaleCoords_NilSeams_FailSafe(t *testing.T) {
	now := time.Now()
	rec := staleCoordRecord("unwired1", "rainier", 4242, 999, true, now.Add(-time.Hour))

	deps := staleCoordDeps(now)
	deps.ListAgents = func() ([]*agent.Record, error) { return []*agent.Record{rec}, nil }
	deps.ActiveLeaseOwnerPID = nil
	deps.CoordSupervisorAlive = nil
	deps.KillCoord = nil

	rep, err := Reconcile(Options{Apply: true, Kinds: []Kind{KindStaleCoords}}, deps)
	if err != nil {
		t.Fatalf("Reconcile with nil seams: %v", err)
	}
	if act := findStaleCoordAction(rep, "unwired1"); act != nil {
		t.Fatalf("nil platform seams must skip class (a), got %+v", act)
	}
}
