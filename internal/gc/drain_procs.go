package gc

// drain_procs.go — eighth classifier for `fleet gc`: reaps leaked /
// wedged `fleet drain` processes (the 81-process leak in
// DESIGN-handoff-drain-storm-leak.md §3.D, impl item 8). Two paths:
//
//	1. Steady-state (KindDrainProcs, the default-kinds member).
//	   Keys off the drain RUN-RECORD ~/.fleet/drain-runs/<pid>.json
//	   ({pid, pid_start, started_at, heartbeat_at}). A drain is WEDGED
//	   iff its heartbeat is stale past the TTL AND a live process at
//	   <pid> still has the recorded pid_start (PID-reuse-safe). The
//	   run-record + heartbeat is the PROVABLE wedged signal — design
//	   item 8 is explicit that inferring wedged-ness from raw `ps`
//	   "sleeping" state alone can kill a legitimate long recovery.
//
//	2. One-time legacy sweep (--legacy-drains). The existing 81 leaked
//	   drains PRE-DATE the run-record, so they have none. This sweep
//	   falls back to the coarse `ps` heuristic: a process whose argv is
//	   `fleet drain`, state sleeping, AND older than a floor (~5min).
//	   Conservative blast radius — only provably-fleet-owned `fleet
//	   drain` procs are touched.
//
// SURFACE by default for BOTH paths (a wedged drain is reported, not
// killed). `--apply` upgrades to a guarded kill: identity (pid_start +
// exe/argv) is RE-VALIDATED immediately before the signal, then the
// stale run-record is deleted (steady-state path; legacy procs have
// none). All kills are idempotent on already-dead PIDs.
//
// Per feedback_fleet_owns_its_resources.md (fleet owns the lifecycle of
// everything it creates) + the never-manual-kill rule: the operator
// reaps the 81 with `fleet gc --legacy-drains --apply`, never by hand.
//
//	┌──────────────────── steady-state (KindDrainProcs) ───────────────┐
//	│ run-record stale heartbeat? ──no──▶ healthy / fresh → no action  │
//	│         │ yes                                                     │
//	│         ▼                                                         │
//	│ pid+pid_start still a LIVE proc? ──no──▶ already dead →           │
//	│         │ yes                            delete stale record only │
//	│         ▼                                                         │
//	│   WEDGED → surface (default) │ --apply → guarded kill → del record│
//	└──────────────────────────────────────────────────────────────────┘

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/edisonshen/fleet/internal/spawn"
)

// KindDrainProcs is the eighth classifier — leaked/wedged `fleet drain`
// process reaper.
const KindDrainProcs Kind = "drain-procs"

// drainHeartbeatFloor is the minimum staleness window for the
// steady-state classifier — never reap a drain whose heartbeat is
// younger than this, regardless of configured spawn budgets.
const drainHeartbeatFloor = 5 * time.Minute

// drainHeartbeatTTL is the staleness threshold for the steady-state
// classifier. A run-record whose heartbeat_at is older than this (and
// whose pid+pid_start is still live) is provably wedged.
//
// The drain heartbeat is PROGRESS-driven (cmd/fleet/drain_runrecord.go
// Beat is called at each queue-file checkpoint, NOT from a timer), so a
// drain wedged forever inside a blocking LockAgent/Resume stops beating
// and crosses this TTL — which is the whole point (codex iter-5 [P2]).
//
// The TTL MUST exceed the worst-case time a single LEGITIMATE handoff can
// spend between Beats — dominated by spawn.Spawn's PID-resolution budget,
// which the operator can raise via FLEET_PID_RESOLVE_S (codex iter-7
// [P2]). A fixed 5min could be shorter than a configured budget and
// falsely reap a slow-but-progressing drain. So we derive it from the
// SAME budget the spawn path uses, mirroring the orphan-tmux freshness
// gate: max(floor, 2 x PidResolveTimeout). The injectable seam keeps it
// testable. The lease/bounded-drain rewrite (PR1-4) shrinks Resume's
// worst case; until then this is the conservative reaping signal.
var drainHeartbeatTTLFn = func() time.Duration {
	budget := 2 * spawn.PidResolveTimeout()
	if budget > drainHeartbeatFloor {
		return budget
	}
	return drainHeartbeatFloor
}

func drainHeartbeatTTL() time.Duration { return drainHeartbeatTTLFn() }

// drainLegacyAgeFloor is the floor for the --legacy-drains coarse
// sweep. The existing 81 leaked drains have no run-record (they predate
// it), so this sweep classifies on `ps` state + age: a `fleet drain`
// proc that is sleeping AND older than this floor is the 81-leak shape.
// 5min keeps a freshly-spawned, legitimately-running drain (which can
// briefly sleep on I/O) out of the blast radius.
const drainLegacyAgeFloor = 5 * time.Minute

// DrainRun is the parsed run-record the steady-state classifier reads
// from ~/.fleet/drain-runs/<pid>.json. PidStart is the recorded process
// start identity (defeats PID reuse); HeartbeatAt is refreshed while the
// drain runs and goes stale when it wedges/dies.
type DrainRun struct {
	Pid         int       `json:"pid"`
	PidStart    string    `json:"pid_start"`
	StartedAt   time.Time `json:"started_at"`
	HeartbeatAt time.Time `json:"heartbeat_at"`
	// Path is the run-record file on disk (set by the lister, not
	// serialized). RemoveDrainRun targets it after a kill.
	Path string `json:"-"`
}

// DrainProcInfo is one `fleet drain` process the legacy `ps` sweep
// yielded. PidStart + Exe corroborate identity for the guarded kill.
// Sleeping + Age drive the legacy classification gate.
type DrainProcInfo struct {
	Pid      int
	PidStart string
	Exe      string // argv[0] / exe path — must be fleet-shaped to be in scope
	Sleeping bool
	Age      time.Duration
}

// DrainKillTarget is the identity passed to KillDrain. The killer
// RE-VALIDATES pid_start + exe immediately before signaling and is a
// no-op on a pid that is already dead OR whose identity no longer
// matches — so a reaped record never shoots an unrelated process that
// reused the PID.
type DrainKillTarget struct {
	Pid      int
	PidStart string
	Exe      string
	// RequireFingerprint fails the guard CLOSED when PidStart is empty
	// (codex iter-6 [P2]). The legacy `ps` sweep sets this: it has no
	// run-record proving ownership, so it must NOT signal a PID whose
	// start fingerprint could not be captured (a reused PID could be a
	// different process). The steady-state path leaves it false — an
	// empty fingerprint there comes from an old run-record and falls back
	// to a bare liveness check.
	RequireFingerprint bool
}

// DrainKillResult is the three-way outcome of a guarded kill (codex
// [P2]). The caller MUST distinguish these to decide whether the stale
// run-record is safe to delete:
//
//	Killed → SIGTERM delivered to the confirmed-identity drain. Delete
//	         the record (LAST reap step).
//	Gone   → the pid is confirmed dead OR its start identity changed (PID
//	         reuse). No signal sent. Safe to delete the now-meaningless
//	         record.
//	neither (zero value) → the guard could NOT confirm liveness/identity
//	         (e.g. the argv re-probe failed transiently). The pid MAY
//	         still be the wedged drain, so the record is KEPT and the next
//	         sweep retries. Never strand a wedged drain on an ambiguous
//	         guard result.
type DrainKillResult struct {
	Killed bool
	Gone   bool
}

// reconcileDrainProcs runs the steady-state run-record classifier. For
// each run-record:
//
//	heartbeat fresh (within TTL)          → no action (healthy/long-recovery)
//	heartbeat stale + pid+pid_start live  → WEDGED → surface | --apply kill+del
//	heartbeat stale + pid dead/mismatch   → already gone → delete stale record
//
// Heartbeat — not age — is the wedged signal: a drain whose started_at
// is hours old but heartbeat is fresh is a legitimate long recovery and
// is left untouched (T18b).
func reconcileDrainProcs(r *Report, opts Options, deps Deps) error {
	if deps.ListDrainRuns == nil {
		return errors.New("drain-procs: ListDrainRuns dep not wired")
	}
	runs, err := deps.ListDrainRuns()
	if err != nil {
		return err
	}
	// DrainProcLive is only consulted when there are run-records to
	// classify — the legacy-only path (no run-records) doesn't need it,
	// so we don't force callers to wire it for a --legacy-drains sweep.
	if len(runs) > 0 && deps.DrainProcLive == nil {
		return errors.New("drain-procs: DrainProcLive dep not wired")
	}
	sort.SliceStable(runs, func(i, j int) bool { return runs[i].Pid < runs[j].Pid })

	now := deps.Now()
	ttl := drainHeartbeatTTL()
	for _, run := range runs {
		age := now.Sub(run.HeartbeatAt)
		if age < ttl {
			// Fresh heartbeat → healthy or actively-progressing
			// long-recovery drain. Never touched (T18a / T18b).
			continue
		}
		// Heartbeat is stale. Is the recorded pid+pid_start STILL a live
		// process? DrainProcLive returns true only when pid is alive AND
		// its start identity matches run.PidStart (PID-reuse-safe).
		live := deps.DrainProcLive(run.Pid, run.PidStart)
		if !live {
			// The drain is gone but left its run-record behind (crash /
			// kill -9), OR the PID was reused by an unrelated process
			// (start-time mismatch). Either way: NO kill — just delete
			// the stale record (T17c idempotency, T18c PID-reuse). Done
			// in apply mode only; dry-run surfaces the would-clean.
			act := Action{
				Kind: KindDrainProcs, Target: drainTarget(run.Pid),
				Verb:   VerbWouldRemove,
				Reason: fmt.Sprintf("stale run-record (heartbeat %s ago); pid %d no longer live (start-time mismatch or gone) — cleaning record, no kill", humanDuration(age), run.Pid),
			}
			if opts.Apply {
				if rerr := removeDrainRunIfWired(deps, run); rerr != nil {
					act.Reason = fmt.Sprintf("%s; remove record failed: %v", act.Reason, rerr)
				} else {
					act.Verb = VerbRemoved
				}
			}
			r.Actions = append(r.Actions, act)
			continue
		}
		// WEDGED: stale heartbeat + live pid+pid_start.
		act := Action{
			Kind: KindDrainProcs, Target: drainTarget(run.Pid),
			Verb:   VerbSurface,
			Reason: fmt.Sprintf("WEDGED fleet drain (pid=%d, heartbeat %s ago > TTL %s); surface only — rerun with --apply to reap", run.Pid, humanDuration(age), humanDuration(ttl)),
		}
		if opts.Apply {
			act.Verb = VerbWouldKill
			act.Reason = fmt.Sprintf("WEDGED fleet drain (pid=%d, heartbeat %s ago)", run.Pid, humanDuration(age))
			if deps.KillDrain == nil {
				act.Verb = VerbSurface
				act.Reason = fmt.Sprintf("%s; --apply requested but KillDrain dep not wired", act.Reason)
			} else {
				res, kerr := deps.KillDrain(DrainKillTarget{Pid: run.Pid, PidStart: run.PidStart})
				switch {
				case kerr != nil:
					act.Verb = VerbSurface
					act.Reason = fmt.Sprintf("%s; kill failed: %v", act.Reason, kerr)
				case res.Killed:
					act.Verb = VerbKilled
					// LAST step of the sweep: delete the stale run-record.
					if rerr := removeDrainRunIfWired(deps, run); rerr != nil {
						act.Reason = fmt.Sprintf("reaped pid=%d but remove record failed: %v", run.Pid, rerr)
					}
				case res.Gone:
					// pid confirmed dead / identity changed between
					// classification and signal (raced to exit, or PID
					// reuse) — no kill, record is now meaningless: clean it.
					if rerr := removeDrainRunIfWired(deps, run); rerr != nil {
						// codex [P3]: don't claim "removed" when the delete
						// failed — the stale record survives and the next
						// sweep retries. Surface the failure honestly.
						act.Verb = VerbSurface
						act.Reason = fmt.Sprintf("pid=%d gone at signal time but record cleanup failed: %v — will retry next sweep", run.Pid, rerr)
					} else {
						act.Verb = VerbRemoved
						act.Reason = fmt.Sprintf("pid=%d gone / identity changed at signal time (raced to exit or PID reuse) — record cleaned, no kill", run.Pid)
					}
				default:
					// Guard could NOT confirm liveness/identity (e.g. a
					// transient argv-probe failure). The pid MAY still be
					// the wedged drain — KEEP the record (codex [P2]: don't
					// strand a wedged drain on an ambiguous guard result)
					// and surface so the next sweep retries.
					act.Verb = VerbSurface
					act.Reason = fmt.Sprintf("pid=%d kill guard could not confirm identity (transient probe failure) — record KEPT, will retry next sweep", run.Pid)
				}
			}
		}
		r.Actions = append(r.Actions, act)
	}
	return nil
}

// reconcileLegacyDrains runs the one-time coarse `ps` sweep for the
// existing 81 leaked drains that predate the run-record. A `fleet drain`
// proc is legacy-wedged iff it is sleeping AND older than the age floor.
// SURFACE by default; --apply → guarded kill (idempotent on dead PIDs).
//
// Blast radius is deliberately narrow: the lister (DefaultDeps wiring)
// only yields processes whose argv is `fleet drain` and whose exe is
// fleet-shaped, so a non-fleet sleeping process is never in scope
// (TLD4). It ALSO excludes any PID that has a current run-record (codex
// [P1]): the steady-state pass owns those — a recorded drain that is old
// + sleeping but heartbeating fresh is a legitimate long recovery the
// steady-state pass deliberately skipped, and the coarse age/state
// heuristic must not override that and kill it.
func reconcileLegacyDrains(r *Report, opts Options, deps Deps) error {
	if deps.ListDrainProcs == nil {
		return errors.New("drain-procs: ListDrainProcs dep not wired (required for --legacy-drains)")
	}
	// Build the exclusion set of PIDs that have a run-record — those are
	// the steady-state pass's domain, never the legacy coarse sweep's.
	recorded := map[int]bool{}
	if deps.ListDrainRuns != nil {
		runs, rerr := deps.ListDrainRuns()
		if rerr != nil {
			return rerr
		}
		for _, run := range runs {
			recorded[run.Pid] = true
		}
	}
	procs, err := deps.ListDrainProcs()
	if err != nil {
		return err
	}
	sort.SliceStable(procs, func(i, j int) bool { return procs[i].Pid < procs[j].Pid })

	for _, p := range procs {
		// A PID with a current run-record belongs to the steady-state pass
		// (codex [P1]) — skip it here regardless of age/state.
		if recorded[p.Pid] {
			continue
		}
		// Conservative legacy gate: sleeping AND old. A young or
		// non-sleeping `fleet drain` is a legitimate in-flight drain
		// (TLD3) and is left alone.
		if !p.Sleeping {
			continue
		}
		if p.Age < drainLegacyAgeFloor {
			continue
		}
		act := Action{
			Kind: KindDrainProcs, Target: drainTarget(p.Pid),
			Verb:   VerbSurface,
			Reason: fmt.Sprintf("legacy wedged fleet drain (pid=%d, sleeping, age=%s > floor %s, no run-record); surface only — rerun with --apply to reap", p.Pid, humanDuration(p.Age), humanDuration(drainLegacyAgeFloor)),
		}
		if opts.Apply {
			act.Verb = VerbWouldKill
			act.Reason = fmt.Sprintf("legacy wedged fleet drain (pid=%d, sleeping, age=%s)", p.Pid, humanDuration(p.Age))
			if deps.KillDrain == nil {
				act.Verb = VerbSurface
				act.Reason = fmt.Sprintf("%s; --apply requested but KillDrain dep not wired", act.Reason)
			} else {
				res, kerr := deps.KillDrain(DrainKillTarget{Pid: p.Pid, PidStart: p.PidStart, Exe: p.Exe, RequireFingerprint: true})
				switch {
				case kerr != nil:
					act.Verb = VerbSurface
					act.Reason = fmt.Sprintf("%s; kill failed: %v", act.Reason, kerr)
				case res.Killed:
					act.Verb = VerbKilled
				case res.Gone:
					// Idempotent: already dead / identity changed at signal
					// time → no-op, not an error (TLD2 2nd run). Legacy
					// procs have no run-record to clean.
					act.Verb = VerbSurface
					act.Reason = fmt.Sprintf("pid=%d already gone at signal time — no-op (idempotent)", p.Pid)
				default:
					// Guard could not confirm — surface, don't claim a kill.
					act.Verb = VerbSurface
					act.Reason = fmt.Sprintf("pid=%d kill guard could not confirm identity — not killed, will retry next sweep", p.Pid)
				}
			}
		}
		r.Actions = append(r.Actions, act)
	}
	return nil
}

// removeDrainRunIfWired deletes a stale run-record when the dep is
// wired; a nil dep is tolerated (narrow unit tests that only exercise
// classification). An empty Path is a no-op. The full DrainRun is passed
// so the production impl can re-validate pid_start before unlinking
// (TOCTOU close, codex [P2]).
func removeDrainRunIfWired(deps Deps, run DrainRun) error {
	if run.Path == "" || deps.RemoveDrainRun == nil {
		return nil
	}
	return deps.RemoveDrainRun(run)
}

// drainTarget renders the per-action Target for a drain pid. A stable
// string keeps the report's secondary sort (by Target) deterministic.
func drainTarget(pid int) string {
	return fmt.Sprintf("drain pid=%d", pid)
}
