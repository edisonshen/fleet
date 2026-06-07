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
	if err := killCoord(KillTarget{Pid: stalePid, PidStart: 222, AgentID: "stale001", Project: project, FencerEpoch: 6}, d); err != nil {
		t.Fatalf("stale same-project kill: want nil (reaped), got %v", err)
	}
	if !rec.sentTo(stalePid) {
		t.Errorf("expected SIGTERM/SIGKILL to stale pid %d", stalePid)
	}

	// Different project -> the record lookup is scoped to target.Project,
	// so no record matches -> benign no-op, no signal.
	if err := killCoord(KillTarget{Pid: otherPid, PidStart: 333, AgentID: "other001", Project: project, FencerEpoch: 6}, d); err != nil {
		t.Fatalf("cross-project: want nil no-op, got %v", err)
	}
	if rec.sentTo(otherPid) {
		t.Errorf("must NOT signal different-project coord pid %d", otherPid)
	}

	// The current active leader -> refused (epoch gate).
	err := killCoord(KillTarget{Pid: leaderPid, PidStart: 444, AgentID: "leader01", Project: project, FencerEpoch: 6}, d)
	if !errors.Is(err, ErrKillRefused) {
		t.Errorf("leader kill: want ErrKillRefused, got %v", err)
	}
	if rec.sentTo(leaderPid) {
		t.Errorf("must NOT signal the current active leader pid %d", leaderPid)
	}

	// Self -> refused.
	err = killCoord(KillTarget{Pid: selfPid, PidStart: 111, AgentID: "self0001", Project: project, FencerEpoch: 6}, d)
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

	err := killCoord(KillTarget{Pid: pid, PidStart: 555, AgentID: "reuse001", Project: project, FencerEpoch: 7}, d)
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
		err := killCoord(KillTarget{Pid: pid, PidStart: 666, AgentID: "exe00001", Project: project, FencerEpoch: 8}, d)
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
	if err := killCoord(KillTarget{Pid: pid, PidStart: 909, AgentID: "reap0001", Project: project, FencerEpoch: 5}, d); err != nil {
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
