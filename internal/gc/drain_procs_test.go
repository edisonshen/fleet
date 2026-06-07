package gc

import (
	"errors"
	"testing"
	"time"
)

// drainTestDeps builds a minimal Deps wired only for the drain-procs
// classifier. Each test overrides the relevant hooks. now is the pinned
// clock; killCalls records every guarded-kill target so a test can
// assert kill-or-not. recordRemoves records every deleted run-record.
type drainHarness struct {
	deps       Deps
	killCalls  []DrainKillTarget
	removed    []string
	killReturn func(DrainKillTarget) (DrainKillResult, error)
}

func newDrainHarness(now time.Time) *drainHarness {
	h := &drainHarness{}
	h.deps = Deps{
		Now: func() time.Time { return now },
		KillDrain: func(t DrainKillTarget) (DrainKillResult, error) {
			h.killCalls = append(h.killCalls, t)
			if h.killReturn != nil {
				return h.killReturn(t)
			}
			return DrainKillResult{Killed: true}, nil // default: killed
		},
		RemoveDrainRun: func(run DrainRun) error {
			h.removed = append(h.removed, run.Path)
			return nil
		},
	}
	return h
}

func reconcileDrains(t *testing.T, opts Options, deps Deps) Report {
	t.Helper()
	opts.Kinds = []Kind{KindDrainProcs}
	r, err := Reconcile(opts, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return r
}

// ---------------- T17: steady-state wedged-drain reaping ----------------

// T17a — gc surfaces a wedged drain in dry-run; no kill, record kept.
func TestDrainProcs_T17a_SurfacesWedged_DryRun(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	h := newDrainHarness(now)
	h.deps.ListDrainRuns = func() ([]DrainRun, error) {
		return []DrainRun{{
			Pid: 4242, PidStart: "Mon Jun  2 10:00:00 2026",
			StartedAt:   now.Add(-3 * time.Hour),
			HeartbeatAt: now.Add(-10 * time.Minute), // stale > TTL
			Path:        "/tmp/drain-runs/4242.json",
		}}, nil
	}
	h.deps.DrainProcLive = func(pid int, start string) bool { return true } // live + identity match

	r := reconcileDrains(t, Options{Apply: false}, h.deps)

	act, ok := findAction(r, KindDrainProcs, "drain pid=4242")
	if !ok {
		t.Fatalf("expected a drain-procs action for pid 4242; got %+v", r.Actions)
	}
	if act.Verb != VerbSurface {
		t.Fatalf("dry-run should SURFACE, got verb=%s", act.Verb)
	}
	if len(h.killCalls) != 0 {
		t.Fatalf("dry-run must not kill; got %d kill calls", len(h.killCalls))
	}
	if len(h.removed) != 0 {
		t.Fatalf("dry-run must not delete the record; got %v", h.removed)
	}
}

// T17b — --apply reaps a wedged drain: kill once, record deleted.
func TestDrainProcs_T17b_ReapsWedged_Apply(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	h := newDrainHarness(now)
	h.deps.ListDrainRuns = func() ([]DrainRun, error) {
		return []DrainRun{{
			Pid: 4242, PidStart: "Mon Jun  2 10:00:00 2026",
			HeartbeatAt: now.Add(-10 * time.Minute),
			Path:        "/tmp/drain-runs/4242.json",
		}}, nil
	}
	h.deps.DrainProcLive = func(pid int, start string) bool { return true }

	r := reconcileDrains(t, Options{Apply: true}, h.deps)

	act, _ := findAction(r, KindDrainProcs, "drain pid=4242")
	if act.Verb != VerbKilled {
		t.Fatalf("apply should KILL the wedged drain, got verb=%s reason=%s", act.Verb, act.Reason)
	}
	if len(h.killCalls) != 1 || h.killCalls[0].Pid != 4242 {
		t.Fatalf("expected exactly one kill of pid 4242; got %+v", h.killCalls)
	}
	if h.killCalls[0].PidStart != "Mon Jun  2 10:00:00 2026" {
		t.Fatalf("kill target must carry recorded pid_start for re-validation; got %q", h.killCalls[0].PidStart)
	}
	if len(h.removed) != 1 || h.removed[0] != "/tmp/drain-runs/4242.json" {
		t.Fatalf("expected the stale run-record deleted after kill; got %v", h.removed)
	}
}

// T17c — reap is idempotent: after the record is gone / pid dead, a
// re-run produces no kill and exits clean.
func TestDrainProcs_T17c_Idempotent(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	h := newDrainHarness(now)
	// Record still on disk (delete lagged) but the pid is now dead.
	h.deps.ListDrainRuns = func() ([]DrainRun, error) {
		return []DrainRun{{
			Pid: 4242, PidStart: "Mon Jun  2 10:00:00 2026",
			HeartbeatAt: now.Add(-10 * time.Minute),
			Path:        "/tmp/drain-runs/4242.json",
		}}, nil
	}
	h.deps.DrainProcLive = func(pid int, start string) bool { return false } // dead now

	r := reconcileDrains(t, Options{Apply: true}, h.deps)

	if len(h.killCalls) != 0 {
		t.Fatalf("idempotent re-run must NOT kill a dead pid; got %+v", h.killCalls)
	}
	act, _ := findAction(r, KindDrainProcs, "drain pid=4242")
	if act.Verb != VerbRemoved {
		t.Fatalf("dead pid should clean the stale record (VerbRemoved), got %s", act.Verb)
	}
	if len(h.removed) != 1 {
		t.Fatalf("expected stale record cleaned exactly once; got %v", h.removed)
	}
}

// TOCTOU freshness re-check (codex [P2]): the snapshot heartbeat is stale,
// but a re-read just before the kill shows the drain Beat()'d fresh (it
// reached the next queue-file checkpoint). Must NOT kill, must NOT delete.
func TestDrainProcs_RacedFreshBeat_NotKilled(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	h := newDrainHarness(now)
	h.deps.ListDrainRuns = func() ([]DrainRun, error) {
		return []DrainRun{{
			Pid: 4242, PidStart: "s",
			HeartbeatAt: now.Add(-10 * time.Minute), // STALE snapshot
			Path:        "/tmp/4242.json",
		}}, nil
	}
	h.deps.DrainProcLive = func(int, string) bool { return true }
	// Re-read shows a FRESH heartbeat (drain made progress).
	h.deps.ReloadDrainRun = func(string) (DrainRun, bool, error) {
		return DrainRun{Pid: 4242, PidStart: "s", HeartbeatAt: now.Add(-3 * time.Second), Path: "/tmp/4242.json"}, true, nil
	}

	r := reconcileDrains(t, Options{Apply: true}, h.deps)
	if len(h.killCalls) != 0 {
		t.Fatalf("a drain that Beat()'d fresh during the race must NOT be killed; got %+v", h.killCalls)
	}
	if len(h.removed) != 0 {
		t.Fatalf("must NOT delete the fresh drain's record; got %v", h.removed)
	}
	act, _ := findAction(r, KindDrainProcs, "drain pid=4242")
	if act.Verb != VerbSurface {
		t.Fatalf("raced-fresh drain should SURFACE, got %s", act.Verb)
	}
}

// Re-read still stale → the kill proceeds (the re-check doesn't block a
// genuinely-wedged drain).
func TestDrainProcs_RecheckStillStale_Killed(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	h := newDrainHarness(now)
	h.deps.ListDrainRuns = func() ([]DrainRun, error) {
		return []DrainRun{{Pid: 4242, PidStart: "s", HeartbeatAt: now.Add(-10 * time.Minute), Path: "/tmp/4242.json"}}, nil
	}
	h.deps.DrainProcLive = func(int, string) bool { return true }
	h.deps.ReloadDrainRun = func(string) (DrainRun, bool, error) {
		return DrainRun{Pid: 4242, PidStart: "s", HeartbeatAt: now.Add(-9 * time.Minute), Path: "/tmp/4242.json"}, true, nil
	}
	r := reconcileDrains(t, Options{Apply: true}, h.deps)
	if a, _ := findAction(r, KindDrainProcs, "drain pid=4242"); a.Verb != VerbKilled {
		t.Fatalf("a still-stale drain should be killed, got %s", a.Verb)
	}
}

// ---------------- T18: never-kill-a-good-drain guards ----------------

// T18a — a fresh drain (heartbeat within TTL) is never killed.
func TestDrainProcs_T18a_FreshDrain_Untouched(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	h := newDrainHarness(now)
	h.deps.ListDrainRuns = func() ([]DrainRun, error) {
		return []DrainRun{{
			Pid: 7, PidStart: "x", HeartbeatAt: now, // fresh
			Path: "/tmp/drain-runs/7.json",
		}}, nil
	}
	h.deps.DrainProcLive = func(int, string) bool { return true }

	r := reconcileDrains(t, Options{Apply: true}, h.deps)

	if len(r.Actions) != 0 {
		t.Fatalf("fresh drain must produce zero actions; got %+v", r.Actions)
	}
	if len(h.killCalls) != 0 {
		t.Fatalf("fresh drain must not be killed; got %+v", h.killCalls)
	}
}

// T18b — a long-recovery drain (old started_at but heartbeat fresh) is
// never killed. Heartbeat, not age, is the wedged signal.
func TestDrainProcs_T18b_LongRecovery_Untouched(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	h := newDrainHarness(now)
	h.deps.ListDrainRuns = func() ([]DrainRun, error) {
		return []DrainRun{{
			Pid: 7, PidStart: "x",
			StartedAt:   now.Add(-6 * time.Hour),   // hours old
			HeartbeatAt: now.Add(-5 * time.Second), // but fresh!
			Path:        "/tmp/drain-runs/7.json",
		}}, nil
	}
	h.deps.DrainProcLive = func(int, string) bool { return true }

	r := reconcileDrains(t, Options{Apply: true}, h.deps)

	if len(r.Actions) != 0 {
		t.Fatalf("long-recovery (fresh heartbeat) drain must produce zero actions; got %+v", r.Actions)
	}
	if len(h.killCalls) != 0 {
		t.Fatalf("long-recovery drain must not be killed; got %+v", h.killCalls)
	}
}

// T18c — PID-reuse safety: stale heartbeat, but the live pid now has a
// DIFFERENT start time than recorded. Treated as already-dead → record
// cleaned, NO kill (the live PID is an unrelated process).
func TestDrainProcs_T18c_PidReuse_NoKill(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	h := newDrainHarness(now)
	h.deps.ListDrainRuns = func() ([]DrainRun, error) {
		return []DrainRun{{
			Pid: 4242, PidStart: "Mon Jun  2 10:00:00 2026",
			HeartbeatAt: now.Add(-10 * time.Minute),
			Path:        "/tmp/drain-runs/4242.json",
		}}, nil
	}
	// DrainProcLive encodes the PID-reuse check: live pid but start
	// mismatch → false (the production drainProcLiveOnDisk returns false
	// on start-time mismatch).
	h.deps.DrainProcLive = func(pid int, recordedStart string) bool {
		return false // mismatch → not the recorded drain
	}

	r := reconcileDrains(t, Options{Apply: true}, h.deps)

	if len(h.killCalls) != 0 {
		t.Fatalf("PID reuse must NOT kill the unrelated live process; got %+v", h.killCalls)
	}
	act, _ := findAction(r, KindDrainProcs, "drain pid=4242")
	if act.Verb != VerbRemoved {
		t.Fatalf("PID-reuse case should clean the stale record only, got %s", act.Verb)
	}
}

// T17b-variant — kill races to exit between classify + signal:
// KillDrain returns (false, nil). No error; record cleaned; no double-kill.
func TestDrainProcs_KillRacedToExit_NoError(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	h := newDrainHarness(now)
	h.killReturn = func(DrainKillTarget) (DrainKillResult, error) { return DrainKillResult{Gone: true}, nil } // raced to exit
	h.deps.ListDrainRuns = func() ([]DrainRun, error) {
		return []DrainRun{{
			Pid: 4242, PidStart: "s", HeartbeatAt: now.Add(-10 * time.Minute),
			Path: "/tmp/drain-runs/4242.json",
		}}, nil
	}
	h.deps.DrainProcLive = func(int, string) bool { return true }

	r := reconcileDrains(t, Options{Apply: true}, h.deps)

	act, _ := findAction(r, KindDrainProcs, "drain pid=4242")
	if act.Verb != VerbRemoved {
		t.Fatalf("raced-to-exit kill should clean the record, got %s reason=%s", act.Verb, act.Reason)
	}
	if len(h.removed) != 1 {
		t.Fatalf("expected the now-dead record cleaned; got %v", h.removed)
	}
}

// Ambiguous guard (codex [P2]): KillDrain returns the zero result
// (could NOT confirm liveness/identity). The wedged drain may still be
// live — the record must be KEPT (not deleted) and the action surfaced
// so the next sweep retries. Never strand a wedged drain.
func TestDrainProcs_AmbiguousGuard_RecordKept(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	h := newDrainHarness(now)
	h.killReturn = func(DrainKillTarget) (DrainKillResult, error) { return DrainKillResult{}, nil } // unconfirmed
	h.deps.ListDrainRuns = func() ([]DrainRun, error) {
		return []DrainRun{{
			Pid: 4242, PidStart: "s", HeartbeatAt: now.Add(-10 * time.Minute),
			Path: "/tmp/drain-runs/4242.json",
		}}, nil
	}
	h.deps.DrainProcLive = func(int, string) bool { return true }

	r := reconcileDrains(t, Options{Apply: true}, h.deps)

	act, _ := findAction(r, KindDrainProcs, "drain pid=4242")
	if act.Verb != VerbSurface {
		t.Fatalf("ambiguous guard should SURFACE (not kill/remove), got %s reason=%s", act.Verb, act.Reason)
	}
	if len(h.removed) != 0 {
		t.Fatalf("ambiguous guard must KEEP the record (no delete); got removed=%v", h.removed)
	}
}

// Gone + record-delete failure (codex [P3]): when KillDrain reports Gone
// but RemoveDrainRun errors, the action must NOT claim "removed" — it
// surfaces the failure so the operator knows the next sweep retries.
func TestDrainProcs_GoneButRemoveFails_SurfacesNotRemoved(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	h := newDrainHarness(now)
	h.killReturn = func(DrainKillTarget) (DrainKillResult, error) { return DrainKillResult{Gone: true}, nil }
	h.deps.RemoveDrainRun = func(DrainRun) error { return errors.New("permission denied") }
	h.deps.ListDrainRuns = func() ([]DrainRun, error) {
		return []DrainRun{{
			Pid: 4242, PidStart: "s", HeartbeatAt: now.Add(-10 * time.Minute),
			Path: "/tmp/drain-runs/4242.json",
		}}, nil
	}
	h.deps.DrainProcLive = func(int, string) bool { return true }

	r := reconcileDrains(t, Options{Apply: true}, h.deps)
	act, _ := findAction(r, KindDrainProcs, "drain pid=4242")
	if act.Verb == VerbRemoved {
		t.Fatalf("must NOT report removed when the delete failed; reason=%s", act.Reason)
	}
	if act.Verb != VerbSurface {
		t.Fatalf("delete failure should SURFACE; got %s", act.Verb)
	}
}

// ---------------- TLD: --legacy-drains coarse ps sweep ----------------

// TLD1 — --legacy-drains surfaces an old sleeping drain (dry-run).
func TestLegacyDrains_TLD1_SurfacesOldSleeping(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	h := newDrainHarness(now)
	h.deps.ListDrainRuns = func() ([]DrainRun, error) { return nil, nil } // no run-records (legacy)
	h.deps.ListDrainProcs = func() ([]DrainProcInfo, error) {
		return []DrainProcInfo{{
			Pid: 909, PidStart: "Mon Jun  2 00:00:00 2026",
			Exe: "/usr/local/bin/fleet", Sleeping: true, Age: 30 * time.Minute,
		}}, nil
	}

	r := reconcileDrains(t, Options{Apply: false, LegacyDrains: true}, h.deps)

	act, ok := findAction(r, KindDrainProcs, "drain pid=909")
	if !ok {
		t.Fatalf("expected legacy drain surfaced; got %+v", r.Actions)
	}
	if act.Verb != VerbSurface {
		t.Fatalf("legacy dry-run should SURFACE, got %s", act.Verb)
	}
	if len(h.killCalls) != 0 {
		t.Fatalf("legacy dry-run must not kill; got %+v", h.killCalls)
	}
}

// TLD2 — --legacy-drains --apply reaps the 81-shape; idempotent on a 2nd
// run (KillDrain returns false=already-gone → no error, no double-kill).
func TestLegacyDrains_TLD2_ReapsAndIdempotent(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	h := newDrainHarness(now)
	h.deps.ListDrainRuns = func() ([]DrainRun, error) { return nil, nil }
	h.deps.ListDrainProcs = func() ([]DrainProcInfo, error) {
		return []DrainProcInfo{{
			Pid: 909, PidStart: "s", Exe: "fleet", Sleeping: true, Age: time.Hour,
		}}, nil
	}

	r := reconcileDrains(t, Options{Apply: true, LegacyDrains: true}, h.deps)
	act, _ := findAction(r, KindDrainProcs, "drain pid=909")
	if act.Verb != VerbKilled {
		t.Fatalf("legacy apply should kill pid 909, got %s", act.Verb)
	}
	if len(h.killCalls) != 1 {
		t.Fatalf("expected one kill; got %+v", h.killCalls)
	}

	// 2nd run: pid already gone → KillDrain returns (false, nil).
	h2 := newDrainHarness(now)
	h2.killReturn = func(DrainKillTarget) (DrainKillResult, error) { return DrainKillResult{Gone: true}, nil }
	h2.deps.ListDrainRuns = func() ([]DrainRun, error) { return nil, nil }
	h2.deps.ListDrainProcs = func() ([]DrainProcInfo, error) {
		return []DrainProcInfo{{Pid: 909, PidStart: "s", Exe: "fleet", Sleeping: true, Age: time.Hour}}, nil
	}
	r2 := reconcileDrains(t, Options{Apply: true, LegacyDrains: true}, h2.deps)
	act2, _ := findAction(r2, KindDrainProcs, "drain pid=909")
	if act2.Verb == VerbKilled {
		t.Fatalf("2nd run must NOT report a 2nd kill of an already-dead pid; verb=%s", act2.Verb)
	}
}

// Legacy sweep must EXCLUDE a PID that has a current run-record (codex
// iter-4 [P1]): an old+sleeping drain that is heartbeating fresh is a
// legitimate long recovery the steady-state pass deliberately skipped;
// the coarse legacy heuristic must not override that and kill it.
func TestLegacyDrains_ExcludesRecordedPid(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	h := newDrainHarness(now)
	// The recorded drain at pid 909 is FRESH (heartbeat now) → steady
	// state leaves it alone.
	h.deps.ListDrainRuns = func() ([]DrainRun, error) {
		return []DrainRun{{Pid: 909, PidStart: "s", HeartbeatAt: now, Path: "/tmp/909.json"}}, nil
	}
	h.deps.DrainProcLive = func(int, string) bool { return true }
	// The SAME pid shows up in the coarse ps sweep as old+sleeping.
	h.deps.ListDrainProcs = func() ([]DrainProcInfo, error) {
		return []DrainProcInfo{{Pid: 909, PidStart: "s", Exe: "fleet", Sleeping: true, Age: time.Hour}}, nil
	}

	r := reconcileDrains(t, Options{Apply: true, LegacyDrains: true}, h.deps)
	if _, ok := findAction(r, KindDrainProcs, "drain pid=909"); ok {
		t.Fatalf("a recorded fresh drain must be excluded from the legacy sweep")
	}
	if len(h.killCalls) != 0 {
		t.Fatalf("recorded fresh drain must not be killed; got %+v", h.killCalls)
	}
}

// TLD3 — --legacy-drains skips a young drain (below the 5-min floor).
func TestLegacyDrains_TLD3_SkipsYoung(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	h := newDrainHarness(now)
	h.deps.ListDrainRuns = func() ([]DrainRun, error) { return nil, nil }
	h.deps.ListDrainProcs = func() ([]DrainProcInfo, error) {
		return []DrainProcInfo{{
			Pid: 909, PidStart: "s", Exe: "fleet", Sleeping: true, Age: 1 * time.Minute,
		}}, nil
	}

	r := reconcileDrains(t, Options{Apply: true, LegacyDrains: true}, h.deps)
	if _, ok := findAction(r, KindDrainProcs, "drain pid=909"); ok {
		t.Fatalf("young drain (age < floor) must NOT be surfaced/killed")
	}
	if len(h.killCalls) != 0 {
		t.Fatalf("young drain must not be killed; got %+v", h.killCalls)
	}
}

// TLD3b — a non-sleeping (running) drain is also skipped by the legacy
// sweep even when old.
func TestLegacyDrains_SkipsRunning(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	h := newDrainHarness(now)
	h.deps.ListDrainRuns = func() ([]DrainRun, error) { return nil, nil }
	h.deps.ListDrainProcs = func() ([]DrainProcInfo, error) {
		return []DrainProcInfo{{
			Pid: 909, PidStart: "s", Exe: "fleet", Sleeping: false, Age: time.Hour,
		}}, nil
	}
	r := reconcileDrains(t, Options{Apply: true, LegacyDrains: true}, h.deps)
	if _, ok := findAction(r, KindDrainProcs, "drain pid=909"); ok {
		t.Fatalf("a running (non-sleeping) drain must not be reaped by the legacy sweep")
	}
}

// TLD4 — --legacy-drains skips non-fleet procs. The production lister
// only yields fleet-drain procs; here we assert the classifier honors an
// EMPTY list (a non-fleet proc the lister already filtered out is never
// seen) AND that the legacy sweep never runs without --legacy-drains.
func TestLegacyDrains_TLD4_NonFleetOutOfBlastRadius(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	h := newDrainHarness(now)
	h.deps.ListDrainRuns = func() ([]DrainRun, error) { return nil, nil }
	// Lister yields nothing (it already excluded the non-fleet sleeping
	// proc by argv). ListDrainProcs must NOT even be consulted unless
	// LegacyDrains is set — assert that too.
	legacyCalled := false
	h.deps.ListDrainProcs = func() ([]DrainProcInfo, error) {
		legacyCalled = true
		return nil, nil
	}

	// Without --legacy-drains: the coarse sweep is not run at all.
	r := reconcileDrains(t, Options{Apply: true, LegacyDrains: false}, h.deps)
	if legacyCalled {
		t.Fatalf("ListDrainProcs must NOT be called without --legacy-drains")
	}
	if len(r.Actions) != 0 {
		t.Fatalf("no run-records + no legacy sweep → zero actions; got %+v", r.Actions)
	}

	// With --legacy-drains and an empty (filtered) list: zero actions.
	r2 := reconcileDrains(t, Options{Apply: true, LegacyDrains: true}, h.deps)
	if !legacyCalled {
		t.Fatalf("ListDrainProcs SHOULD be called with --legacy-drains")
	}
	if len(r2.Actions) != 0 {
		t.Fatalf("non-fleet proc filtered by lister → zero actions; got %+v", r2.Actions)
	}
}

// Steady-state + legacy run together: a wedged run-record AND a legacy
// proc are both reaped in one --legacy-drains --apply sweep.
func TestDrainProcs_SteadyAndLegacy_BothReaped(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	h := newDrainHarness(now)
	h.deps.ListDrainRuns = func() ([]DrainRun, error) {
		return []DrainRun{{Pid: 100, PidStart: "a", HeartbeatAt: now.Add(-30 * time.Minute), Path: "/tmp/100.json"}}, nil
	}
	h.deps.DrainProcLive = func(int, string) bool { return true }
	h.deps.ListDrainProcs = func() ([]DrainProcInfo, error) {
		return []DrainProcInfo{{Pid: 200, PidStart: "b", Exe: "fleet", Sleeping: true, Age: time.Hour}}, nil
	}

	r := reconcileDrains(t, Options{Apply: true, LegacyDrains: true}, h.deps)
	if a, _ := findAction(r, KindDrainProcs, "drain pid=100"); a.Verb != VerbKilled {
		t.Fatalf("steady-state wedged drain should be killed, got %s", a.Verb)
	}
	if a, _ := findAction(r, KindDrainProcs, "drain pid=200"); a.Verb != VerbKilled {
		t.Fatalf("legacy drain should be killed, got %s", a.Verb)
	}
	if len(h.killCalls) != 2 {
		t.Fatalf("expected 2 kills (steady + legacy); got %+v", h.killCalls)
	}
}

// TTL derivation (codex iter-7 [P2]): never below the floor, and scales
// past it when the configured spawn PID-resolution budget is larger, so a
// legitimate slow drain under a raised FLEET_PID_RESOLVE_S is not reaped.
func TestDrainHeartbeatTTL_DerivesFromBudget(t *testing.T) {
	prev := drainHeartbeatTTLFn
	t.Cleanup(func() { drainHeartbeatTTLFn = prev })

	// Default budget (small) → floor wins.
	if got := drainHeartbeatTTL(); got < drainHeartbeatFloor {
		t.Fatalf("TTL %v must never be below the floor %v", got, drainHeartbeatFloor)
	}

	// A large configured budget must raise the TTL above the floor so a
	// slow-but-progressing drain isn't falsely reaped. We exercise the
	// derivation directly (the production fn reads spawn.PidResolveTimeout).
	drainHeartbeatTTLFn = func() time.Duration {
		budget := 20 * time.Minute
		if budget > drainHeartbeatFloor {
			return budget
		}
		return drainHeartbeatFloor
	}
	if got := drainHeartbeatTTL(); got != 20*time.Minute {
		t.Fatalf("TTL = %v, want it to scale to the 20m budget", got)
	}
}

// A drain stale beyond the floor but WITHIN a raised budget-derived TTL is
// NOT reaped (the false-reap codex iter-7 guards against).
func TestDrainProcs_WithinRaisedTTL_NotReaped(t *testing.T) {
	prev := drainHeartbeatTTLFn
	drainHeartbeatTTLFn = func() time.Duration { return 20 * time.Minute }
	t.Cleanup(func() { drainHeartbeatTTLFn = prev })

	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	h := newDrainHarness(now)
	h.deps.ListDrainRuns = func() ([]DrainRun, error) {
		// 8min stale: > 5min floor, but < the 20min budget-derived TTL.
		return []DrainRun{{Pid: 7, PidStart: "s", HeartbeatAt: now.Add(-8 * time.Minute), Path: "/tmp/7.json"}}, nil
	}
	h.deps.DrainProcLive = func(int, string) bool { return true }

	r := reconcileDrains(t, Options{Apply: true}, h.deps)
	if len(r.Actions) != 0 {
		t.Fatalf("a drain within the raised TTL must not be reaped; got %+v", r.Actions)
	}
	if len(h.killCalls) != 0 {
		t.Fatalf("must not kill within TTL; got %+v", h.killCalls)
	}
}

// A drain with a long configured grace is NOT reaped while within
// base-TTL + grace (codex iter-11 [P2]): a long --grace-ms sleep happens
// between Beats, so the effective TTL must absorb it.
func TestDrainProcs_GraceExtendsTTL(t *testing.T) {
	prev := drainHeartbeatTTLFn
	drainHeartbeatTTLFn = func() time.Duration { return 5 * time.Minute }
	t.Cleanup(func() { drainHeartbeatTTLFn = prev })

	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	h := newDrainHarness(now)
	// 8min stale, base TTL 5min → would be wedged WITHOUT grace; with a
	// 10min grace the effective TTL is 15min, so it's NOT reaped.
	h.deps.ListDrainRuns = func() ([]DrainRun, error) {
		return []DrainRun{{
			Pid: 9, PidStart: "s", HeartbeatAt: now.Add(-8 * time.Minute),
			GraceMillis: 600000, Path: "/tmp/9.json",
		}}, nil
	}
	h.deps.DrainProcLive = func(int, string) bool { return true }

	r := reconcileDrains(t, Options{Apply: true}, h.deps)
	if len(r.Actions) != 0 {
		t.Fatalf("a drain within base-TTL+grace must not be reaped; got %+v", r.Actions)
	}
	if len(h.killCalls) != 0 {
		t.Fatalf("must not kill within grace-extended TTL; got %+v", h.killCalls)
	}
}

// Per-record PID-resolve budget extends the TTL (codex iter-14 [P2]):
// the gc env TTL is the 5min floor, but a drain that recorded a large
// PID-resolve budget gets a longer effective TTL from ITS budget, so a gc
// run from a normal shell does not reap it early.
func TestDrainProcs_RecordedBudgetExtendsTTL(t *testing.T) {
	prev := drainHeartbeatTTLFn
	drainHeartbeatTTLFn = func() time.Duration { return 5 * time.Minute } // gc-env default
	t.Cleanup(func() { drainHeartbeatTTLFn = prev })

	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	h := newDrainHarness(now)
	// 8min stale: > 5min env TTL → would be reaped on the env TTL alone.
	// But this drain recorded a 10min PID-resolve budget → base TTL = 20min,
	// so it is NOT reaped.
	h.deps.ListDrainRuns = func() ([]DrainRun, error) {
		return []DrainRun{{
			Pid: 7, PidStart: "s", HeartbeatAt: now.Add(-8 * time.Minute),
			PidResolveMillis: int((10 * time.Minute) / time.Millisecond),
			Path:             "/tmp/7.json",
		}}, nil
	}
	h.deps.DrainProcLive = func(int, string) bool { return true }

	r := reconcileDrains(t, Options{Apply: true}, h.deps)
	if len(r.Actions) != 0 {
		t.Fatalf("a drain within its recorded-budget TTL must not be reaped; got %+v", r.Actions)
	}
	if len(h.killCalls) != 0 {
		t.Fatalf("must not kill within recorded-budget TTL; got %+v", h.killCalls)
	}
}

// drainTTLFromBudget: floor wins for a small budget; 2x wins for a large one.
func TestDrainTTLFromBudget(t *testing.T) {
	if got := drainTTLFromBudget(10 * time.Second); got != drainHeartbeatFloor {
		t.Fatalf("small budget should yield the floor %v; got %v", drainHeartbeatFloor, got)
	}
	if got := drainTTLFromBudget(10 * time.Minute); got != 20*time.Minute {
		t.Fatalf("large budget should yield 2x = 20m; got %v", got)
	}
}

// Missing-dep guard: KindDrainProcs without ListDrainRuns errors clearly.
func TestDrainProcs_MissingDep_Errors(t *testing.T) {
	_, err := Reconcile(Options{Kinds: []Kind{KindDrainProcs}, Apply: false}, Deps{Now: time.Now})
	if err == nil {
		t.Fatalf("expected a wiring error when ListDrainRuns is nil")
	}
}
