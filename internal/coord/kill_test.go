//go:build linux || darwin

// Tests for KillCoordIfIdentityMatches — the ONE authenticated coord-kill
// primitive (DESIGN-handoff-drain-storm-leak §B.5). The gate must signal
// ONLY a stale, same-project, exe-path-verified, non-self, non-leader
// coord supervisor whose live pid_start matches the record (PID-reuse
// safe). Every other case SURFACES (returns) without signaling.
//
// All seams are injected so tests are deterministic: no real signals
// (Signal records calls), no real clock (Sleep is a no-op), no sysctl
// (PidStart is a fake map), no real epoch file (CurrentLeaderPID is a
// stub). Covers T10 (epoch-gated sweep), T36 (PID-reuse refusal), W9
// (exe-path refusal), plus self / different-project / already-gone.
package coord

import (
	"errors"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
)

// killRecorder captures the (pid, signal) pairs the primitive emits so a
// test can assert exactly which signals fired (or that none did).
type killRecorder struct {
	mu    sync.Mutex
	calls []struct {
		pid int
		sig syscall.Signal
	}
	// reaps records (agentID, project) pairs passed to the ReapOrphan seam so
	// a test can assert the SIGKILL-escalation orphan reap fired (or did not).
	reaps []struct{ agentID, project string }
}

func (k *killRecorder) reap(agentID, project string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.reaps = append(k.reaps, struct{ agentID, project string }{agentID, project})
	return nil
}

func (k *killRecorder) reapCount() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.reaps)
}

func (k *killRecorder) signal(pid int, sig syscall.Signal) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.calls = append(k.calls, struct {
		pid int
		sig syscall.Signal
	}{pid, sig})
	return nil
}

func (k *killRecorder) sentTo(pid int) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, c := range k.calls {
		if c.pid == pid {
			return true
		}
	}
	return false
}

func (k *killRecorder) count() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.calls)
}

// testKillDeps builds a deterministic KillDeps. `liveStart` maps pid ->
// pid_start for live processes (absent = dead). `leader` is the current
// active-owner pid (0 = none). `coordRunPids` is the set of pids whose
// recorded exe-path is treated as the fleet coord-run binary; any other
// non-empty exe is treated as an engine child. `dead` lets a test make a
// pid "die after the first SIGTERM" — here we keep it simple: a pid is
// alive iff in liveStart, and stays alive (so SIGKILL escalation fires
// deterministically without wall-clock waiting — Sleep is a no-op and
// KillTimeout is tiny).
func testKillDeps(rec *killRecorder, recs []*agent.Record, liveStart map[int]int64, leader int, self int) KillDeps {
	return KillDeps{
		Signal: rec.signal,
		Alive: func(pid int) bool {
			_, ok := liveStart[pid]
			return ok
		},
		PidStart: func(pid int) (int64, bool) {
			st, ok := liveStart[pid]
			return st, ok
		},
		ListRecords: func() ([]*agent.Record, error) { return recs, nil },
		CurrentLeaderPID: func(string) (int, bool) {
			if leader <= 0 {
				return 0, false
			}
			return leader, true
		},
		// In these unit tests we drive exe-path acceptance via the record's
		// SupervisorExePath value directly using the real basename rule —
		// so we use the production isFleetCoordRunBinary, exercising W9's
		// real classifier.
		IsCoordRunBinary: isFleetCoordRunBinary,
		Self:             func() int { return self },
		Grace:            0, // no grace delay in unit tests
		KillTimeout:      10 * time.Millisecond,
		Sleep:            func(time.Duration) {},
		// Stub the orphan reap so unit tests neither shell out to tmux nor
		// touch the real ~/.fleet archive; the dedicated reap test asserts it
		// fires on the SIGKILL branch.
		ReapOrphan: rec.reap,
	}
}

func coordRec(id, project string, supPid int, supStart int64, exe string) *agent.Record {
	r := agent.New(id)
	r.Project = project
	r.SupervisorPID = supPid
	r.SupervisorPidStart = supStart
	r.SupervisorExePath = exe
	return r
}

// T10: epoch-gated sweep kills only the stale same-project coord; self,
// different-project, and the current active leader are untouched.
func TestKillCoord_T10_EpochGatedSweep(t *testing.T) {
	const (
		project   = "proj-a"
		selfPid   = 1000
		stalePid  = 2000 // stale same-project coord -> SHOULD be killed
		otherPid  = 3000 // different-project coord  -> untouched
		leaderPid = 4000 // current active leader     -> untouched
	)
	const fleetExe = "/usr/local/bin/fleet"
	recs := []*agent.Record{
		coordRec("stale001", project, stalePid, 222, fleetExe),
		coordRec("other001", "proj-b", otherPid, 333, fleetExe),
		coordRec("leader01", project, leaderPid, 444, fleetExe),
		coordRec("self0001", project, selfPid, 111, fleetExe),
	}
	live := map[int]int64{selfPid: 111, stalePid: 222, otherPid: 333, leaderPid: 444}
	rec := &killRecorder{}
	d := testKillDeps(rec, recs, live, leaderPid, selfPid)

	// Stale same-project coord -> reaped.
	if err := killCoord(KillTarget{Pid: stalePid, PidStart: 222, AgentID: "stale001", Project: project}, d); err != nil {
		t.Fatalf("stale same-project kill: want nil (reaped), got %v", err)
	}
	if !rec.sentTo(stalePid) {
		t.Errorf("expected SIGTERM/SIGKILL to stale pid %d", stalePid)
	}

	// Different project -> the record lookup is scoped to target.Project,
	// so no record matches -> benign no-op, no signal.
	if err := killCoord(KillTarget{Pid: otherPid, PidStart: 333, AgentID: "other001", Project: project}, d); err != nil {
		t.Fatalf("cross-project: want nil no-op, got %v", err)
	}
	if rec.sentTo(otherPid) {
		t.Errorf("must NOT signal different-project coord pid %d", otherPid)
	}

	// The current active leader -> refused (epoch gate).
	err := killCoord(KillTarget{Pid: leaderPid, PidStart: 444, AgentID: "leader01", Project: project}, d)
	if !errors.Is(err, ErrKillRefused) {
		t.Errorf("leader kill: want ErrKillRefused, got %v", err)
	}
	if rec.sentTo(leaderPid) {
		t.Errorf("must NOT signal the current active leader pid %d", leaderPid)
	}

	// Self -> refused.
	err = killCoord(KillTarget{Pid: selfPid, PidStart: 111, AgentID: "self0001", Project: project}, d)
	if !errors.Is(err, ErrKillRefused) {
		t.Errorf("self kill: want ErrKillRefused, got %v", err)
	}
	if rec.sentTo(selfPid) {
		t.Errorf("must NEVER signal self pid %d", selfPid)
	}
}

// T36: a target whose recorded pid is now a DIFFERENT process (live
// pid_start != recorded supervisor_pid_start) is refused — PID reuse.
func TestKillCoord_T36_PidReuseRefusal(t *testing.T) {
	const (
		project = "proj-reuse"
		selfPid = 1000
		pid     = 5000
	)
	recs := []*agent.Record{
		coordRec("reuse001", project, pid, 555 /*recorded start*/, "/usr/local/bin/fleet"),
	}
	// The live process at pid now has a DIFFERENT start time (it's a
	// recycled pid belonging to some unrelated process).
	live := map[int]int64{selfPid: 111, pid: 999 /*different!*/}
	rec := &killRecorder{}
	d := testKillDeps(rec, recs, live, 0, selfPid)

	err := killCoord(KillTarget{Pid: pid, PidStart: 555, AgentID: "reuse001", Project: project}, d)
	if !errors.Is(err, ErrKillRefused) {
		t.Fatalf("PID-reuse: want ErrKillRefused, got %v", err)
	}
	if rec.count() != 0 {
		t.Errorf("PID-reuse: must NOT signal anything, got %d signals", rec.count())
	}
}

// W9: a target whose recorded exe-path is the ENGINE child (not the fleet
// coord-run binary) is refused — killing the engine pid would not release
// the kernel flock and could shoot an unrelated process.
func TestKillCoord_W9_ExePathMustBeCoordRun(t *testing.T) {
	const (
		project = "proj-exe"
		selfPid = 1000
		pid     = 6000
	)
	for _, exe := range []string{"/usr/local/bin/claude", "/opt/homebrew/bin/node", "", "/bin/sh"} {
		recs := []*agent.Record{coordRec("exe00001", project, pid, 666, exe)}
		live := map[int]int64{selfPid: 111, pid: 666}
		rec := &killRecorder{}
		d := testKillDeps(rec, recs, live, 0, selfPid)
		err := killCoord(KillTarget{Pid: pid, PidStart: 666, AgentID: "exe00001", Project: project}, d)
		if !errors.Is(err, ErrKillRefused) {
			t.Errorf("exe=%q: want ErrKillRefused, got %v", exe, err)
		}
		if rec.count() != 0 {
			t.Errorf("exe=%q: must NOT signal, got %d", exe, rec.count())
		}
	}
}

// A target with no matching live record (already archived / exited) is a
// benign no-op, NOT a refusal — the flock is already free.
func TestKillCoord_AlreadyGone_NoOp(t *testing.T) {
	const (
		project = "proj-gone"
		selfPid = 1000
		pid     = 7000
	)
	rec := &killRecorder{}
	d := testKillDeps(rec, nil /*no records*/, map[int]int64{selfPid: 111}, 0, selfPid)
	if err := killCoord(KillTarget{Pid: pid, PidStart: 777, AgentID: "gone0001", Project: project}, d); err != nil {
		t.Fatalf("already-gone: want nil no-op, got %v", err)
	}
	if rec.count() != 0 {
		t.Errorf("already-gone: must NOT signal, got %d", rec.count())
	}
}

// A dead supervisor pid (record exists, but the process is gone) is a
// benign no-op too — nothing to reap.
func TestKillCoord_DeadPid_NoOp(t *testing.T) {
	const (
		project = "proj-dead"
		selfPid = 1000
		pid     = 8000
	)
	recs := []*agent.Record{coordRec("dead0001", project, pid, 888, "/usr/local/bin/fleet")}
	rec := &killRecorder{}
	// pid absent from liveStart -> dead.
	d := testKillDeps(rec, recs, map[int]int64{selfPid: 111}, 0, selfPid)
	if err := killCoord(KillTarget{Pid: pid, PidStart: 888, AgentID: "dead0001", Project: project}, d); err != nil {
		t.Fatalf("dead pid: want nil no-op, got %v", err)
	}
	if rec.count() != 0 {
		t.Errorf("dead pid: must NOT signal, got %d", rec.count())
	}
}

// A successful reap escalates SIGTERM -> SIGKILL when the pid stays alive
// past the (tiny) KillTimeout, and emits the reaped-coord log line.
func TestKillCoord_SuccessfulReap_TermThenKill(t *testing.T) {
	const (
		project = "proj-reap"
		selfPid = 1000
		pid     = 9000
	)
	recs := []*agent.Record{coordRec("reap0001", project, pid, 909, "/usr/local/bin/fleet")}
	live := map[int]int64{selfPid: 111, pid: 909}
	rec := &killRecorder{}
	d := testKillDeps(rec, recs, live, 0, selfPid)
	if err := killCoord(KillTarget{Pid: pid, PidStart: 909, AgentID: "reap0001", Project: project}, d); err != nil {
		t.Fatalf("reap: want nil, got %v", err)
	}
	// Stays "alive" in our fake (liveStart never mutated), so both SIGTERM
	// and SIGKILL must have fired.
	var sawTerm, sawKill bool
	rec.mu.Lock()
	for _, c := range rec.calls {
		if c.pid == pid && c.sig == syscall.SIGTERM {
			sawTerm = true
		}
		if c.pid == pid && c.sig == syscall.SIGKILL {
			sawKill = true
		}
	}
	rec.mu.Unlock()
	if !sawTerm || !sawKill {
		t.Errorf("expected SIGTERM then SIGKILL escalation; sawTerm=%v sawKill=%v", sawTerm, sawKill)
	}
}

// codex PR3 iter-14 [P1] REGRESSION: when escalation reaches SIGKILL (the
// supervisor ignored SIGTERM), the dead supervisor's `defer Cleanup(...)` never
// runs — so the killer MUST itself reap the orphan (archive record + kill
// tmux/engine). Otherwise the old engine session stays alive while takeover
// spawns a replacement -> duplicate/orphaned coord + stale live record. Asserts
// ReapOrphan fires once with the matched record's (agentID, project).
func TestKillCoord_SigkillEscalation_ReapsOrphan(t *testing.T) {
	const (
		project = "proj-orphan"
		selfPid = 1000
		pid     = 9100
	)
	recs := []*agent.Record{coordRec("orph0001", project, pid, 707, "/usr/local/bin/fleet")}
	live := map[int]int64{selfPid: 111, pid: 707} // pid stays alive -> SIGKILL fires
	rec := &killRecorder{}
	d := testKillDeps(rec, recs, live, 0, selfPid)
	if err := killCoord(KillTarget{Pid: pid, PidStart: 707, AgentID: "orph0001", Project: project}, d); err != nil {
		t.Fatalf("reap: want nil, got %v", err)
	}
	if rec.reapCount() != 1 {
		t.Fatalf("ReapOrphan fired %d times after SIGKILL, want 1 (orphan tmux/engine + record must be reaped)", rec.reapCount())
	}
	rec.mu.Lock()
	got := rec.reaps[0]
	rec.mu.Unlock()
	if got.agentID != "orph0001" || got.project != project {
		t.Errorf("ReapOrphan called with (%q,%q), want (orph0001,%q)", got.agentID, got.project, project)
	}
}

// The orphan reap is SCOPED to the SIGKILL branch: a supervisor that exits
// cleanly under SIGTERM ran its OWN deferred Cleanup, so the killer must NOT
// double-reap. Here the pid "dies" before SIGKILL (removed from the live map by
// an Alive stub that reports dead after the first SIGTERM).
func TestKillCoord_CleanSigtermExit_NoOrphanReap(t *testing.T) {
	const (
		project = "proj-clean"
		selfPid = 1000
		pid     = 9200
	)
	recs := []*agent.Record{coordRec("clean001", project, pid, 808, "/usr/local/bin/fleet")}
	live := map[int]int64{selfPid: 111, pid: 808}
	rec := &killRecorder{}
	d := testKillDeps(rec, recs, live, 0, selfPid)
	// After the first SIGTERM, the supervisor exits: mark pid dead so the
	// SIGKILL escalation branch is NOT entered.
	d.Signal = func(p int, sig syscall.Signal) error {
		_ = rec.signal(p, sig)
		if p == pid && sig == syscall.SIGTERM {
			delete(live, pid) // self-demoted + exited under SIGTERM
		}
		return nil
	}
	if err := killCoord(KillTarget{Pid: pid, PidStart: 808, AgentID: "clean001", Project: project}, d); err != nil {
		t.Fatalf("clean exit: want nil, got %v", err)
	}
	if rec.reapCount() != 0 {
		t.Errorf("ReapOrphan fired %d times on a clean SIGTERM exit, want 0 (supervisor's own defer ran the cleanup)", rec.reapCount())
	}
}

// P2 regression: the target becomes the current active lease owner during
// the grace delay (it won the lease). The pre-grace gates passed, but the
// re-validation before SIGTERM must REFUSE rather than shoot the new
// leader.
func TestKillCoord_P2_BecomesLeaderDuringGrace(t *testing.T) {
	const (
		project = "proj-p2leader"
		selfPid = 1000
		pid     = 11000
	)
	recs := []*agent.Record{coordRec("p2l00001", project, pid, 1212, "/usr/local/bin/fleet")}
	live := map[int]int64{selfPid: 111, pid: 1212}
	rec := &killRecorder{}
	leader := 0 // no leader at the static-gate check...
	d := testKillDeps(rec, recs, live, leader, selfPid)
	// ...but the grace Sleep flips the target into the active-owner role.
	d.Grace = time.Millisecond
	d.Sleep = func(time.Duration) { leader = pid }
	d.CurrentLeaderPID = func(string) (int, bool) {
		if leader <= 0 {
			return 0, false
		}
		return leader, true
	}

	err := killCoord(KillTarget{Pid: pid, PidStart: 1212, AgentID: "p2l00001", Project: project}, d)
	if !errors.Is(err, ErrKillRefused) {
		t.Fatalf("became-leader-during-grace: want ErrKillRefused, got %v", err)
	}
	if rec.count() != 0 {
		t.Errorf("must NOT signal a target that became the leader; got %d signals", rec.count())
	}
}

// P2 regression: the PID is reused during the grace delay (the original
// coord exited and the kernel handed its number to an unrelated process
// with a different start time). The pre-signal re-validation must REFUSE.
func TestKillCoord_P2_PidReusedDuringGrace(t *testing.T) {
	const (
		project = "proj-p2reuse"
		selfPid = 1000
		pid     = 12000
	)
	recs := []*agent.Record{coordRec("p2r00001", project, pid, 3434, "/usr/local/bin/fleet")}
	live := map[int]int64{selfPid: 111, pid: 3434}
	rec := &killRecorder{}
	d := testKillDeps(rec, recs, live, 0, selfPid)
	d.Grace = time.Millisecond
	// During grace the pid is recycled: same pid, NEW start time.
	d.Sleep = func(time.Duration) { live[pid] = 7878 }
	d.PidStart = func(p int) (int64, bool) { st, ok := live[p]; return st, ok }
	d.Alive = func(p int) bool { _, ok := live[p]; return ok }

	err := killCoord(KillTarget{Pid: pid, PidStart: 3434, AgentID: "p2r00001", Project: project}, d)
	if !errors.Is(err, ErrKillRefused) {
		t.Fatalf("pid-reused-during-grace: want ErrKillRefused, got %v", err)
	}
	if rec.count() != 0 {
		t.Errorf("must NOT signal a recycled pid; got %d signals", rec.count())
	}
}

// P2: the target exits cleanly during the grace delay -> benign no-op
// (no signal, no error).
func TestKillCoord_P2_ExitsDuringGrace(t *testing.T) {
	const (
		project = "proj-p2exit"
		selfPid = 1000
		pid     = 13000
	)
	recs := []*agent.Record{coordRec("p2e00001", project, pid, 5656, "/usr/local/bin/fleet")}
	live := map[int]int64{selfPid: 111, pid: 5656}
	rec := &killRecorder{}
	d := testKillDeps(rec, recs, live, 0, selfPid)
	d.Grace = time.Millisecond
	d.Sleep = func(time.Duration) { delete(live, pid) } // exited during grace
	d.PidStart = func(p int) (int64, bool) { st, ok := live[p]; return st, ok }
	d.Alive = func(p int) bool { _, ok := live[p]; return ok }

	if err := killCoord(KillTarget{Pid: pid, PidStart: 5656, AgentID: "p2e00001", Project: project}, d); err != nil {
		t.Fatalf("exits-during-grace: want nil no-op, got %v", err)
	}
	if rec.count() != 0 {
		t.Errorf("must NOT signal an exited target; got %d signals", rec.count())
	}
}

// codex PR2 iter-12 [P2]: when target.AgentID is set, the primitive
// selects the record for THAT agent id (agreeing on the pid), NOT a STALE
// same-project record that happens to carry the reused SupervisorPID
// first in the list — otherwise the stale record's pid_start mismatch
// would refuse a legitimate kill.
func TestKillCoord_SelectsByAgentIDOverStalePidMatch(t *testing.T) {
	const (
		project = "proj-sel"
		selfPid = 1000
		pid     = 14000
	)
	// Stale record FIRST in the list: same project, same (reused)
	// SupervisorPID, but a DIFFERENT pid_start (it's an old archived-ish
	// dup) and a different agent id. The real target record is second.
	recs := []*agent.Record{
		coordRec("stale999", project, pid, 1111 /*stale start*/, "/usr/local/bin/fleet"),
		coordRec("realtgt0", project, pid, 2222 /*real start*/, "/usr/local/bin/fleet"),
	}
	live := map[int]int64{selfPid: 111, pid: 2222 /*live == real target's start*/}
	rec := &killRecorder{}
	d := testKillDeps(rec, recs, live, 0, selfPid)

	// Target names the REAL agent id + its start time.
	err := killCoord(KillTarget{Pid: pid, PidStart: 2222, AgentID: "realtgt0", Project: project}, d)
	if err != nil {
		t.Fatalf("kill should select realtgt0 and succeed, got %v", err)
	}
	if !rec.sentTo(pid) {
		t.Errorf("expected the real target pid %d to be signaled", pid)
	}
}

// isFleetCoordRunBinary classifier unit coverage (W9 helper).
func TestIsFleetCoordRunBinary(t *testing.T) {
	cases := map[string]bool{
		"/usr/local/bin/fleet":            true,
		"/home/u/go/bin/fleet":            true,
		"/tmp/go-build123/b001/exe/fleet": true,
		"/path/fleet.test":                true,
		"/usr/local/bin/claude":           false,
		"/opt/homebrew/bin/codex":         false,
		"/usr/bin/node":                   false,
		"/bin/sh":                         false,
		"/bin/bash":                       false,
		"":                                false,
		"/usr/bin/python3":                false,
	}
	for exe, want := range cases {
		if got := isFleetCoordRunBinary(exe); got != want {
			t.Errorf("isFleetCoordRunBinary(%q) = %v, want %v", exe, got, want)
		}
	}
}
