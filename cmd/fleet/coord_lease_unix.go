//go:build linux || darwin

// coord_lease_unix.go — the lease-wiring half of `fleet coord-run`
// (DESIGN-handoff-drain-storm-leak PR2). Build-tagged to linux||darwin
// because internal/coordlock's lease primitive + internal/coord's STONITH
// (kill.go) are themselves gated to those two GOOS values (they need
// platform pid-start / monotonic-clock reads). Other Unix targets (e.g.
// FreeBSD) compile coord_lease_other.go instead, whose defaultAcquireLease
// reports the lease as unsupported so `fleet coord-run` runs the legacy
// bare-child path (codex PR2 iter-2 [P2]: keep GOOS=freebsd building).
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/coord"
	"github.com/edisonshen/fleet/internal/coordlock"
	"github.com/edisonshen/fleet/internal/fleetlog"
	"github.com/edisonshen/fleet/internal/gc"
	"github.com/edisonshen/fleet/internal/spawn"
	"github.com/edisonshen/fleet/internal/state"
)

// defaultAcquireLease is the production lease-acquire seam runCoordRun
// uses when opts.acquireLease is nil (see productionAcquireLease).
func defaultAcquireLease(opts coordRunOpts, stderr io.Writer) func() (coordLease, bool, []liveHolderInfo, error) {
	return productionAcquireLease(opts, stderr)
}

// leaseDisabledOrUnsupported reports whether err means "no lease, run the
// legacy path": the failover flag is off on this (lease-capable)
// platform. On non-linux/darwin the other-file variant also returns true
// for its unsupported sentinel.
func leaseDisabledOrUnsupported(err error) bool {
	return errors.Is(err, coordlock.ErrFailoverDisabled)
}

// coordLeaderCheck reports whether a HEALTHY coordinator lease leader (or
// a fresh in-progress takeover) currently exists for project. Wired into
// spawn.Options.LeaderCheck so spawn can tell a clean lease STAND-DOWN
// apart from a real supervisor failure (codex PR2 iter-6 [P2]).
//
// It uses coordlock.LeaderPresent — the SAME healthy/in-progress
// predicate AcquireLease uses (TTL + pid_start liveness + fresh-fencing),
// NOT a bare state==active check (codex PR2 iter-11 [P2]): a stale active
// record (dead/hung owner past TTL) must NOT read as "leader present"
// (that would hide a recoverable coord as "already running"), and a fresh
// fencing takeover MUST read as present (a legitimate successor is
// acquiring). Off-flag it returns false (no lease in play).
func coordLeaderCheck(project string) bool {
	if !leaseFailoverEnabled() {
		return false
	}
	return coordlock.LeaderPresent(project)
}

// productionAcquireLease builds the real lease-acquire closure for
// `fleet coord-run`. It is the seam runCoordRun uses when opts.acquire-
// Lease is nil. Steps, in order:
//
//  1. Stamp THIS supervisor's identity (pid + pid_start + exe_path) into
//     the agent record so the authenticated STONITH primitive can target
//     us by the lease-holder pid, NOT the engine child. Done BEFORE the
//     acquire so a candidate that fences us mid-acquire can still find a
//     valid kill target. Best-effort: a stamp failure is surfaced but
//     does not abort (the lease still protects exclusion; the kill
//     primitive simply refuses on an unstamped record).
//  2. coordlock.AcquireLeaseWithKill, injecting the authenticated
//     internal/coord.KillCoordIfIdentityMatches as the takeover STONITH.
//  3. On acquire, run the new-leader competitor SWEEP: DETECT + REPORT
//     any same-project coord straggler (a pre-lease or cross-socket coord
//     the flock alone wouldn't catch). Report-only (KP3,
//     DESIGN-coord-no-auto-kill): the flock is the singleton; reaping is
//     operator-gated (`fleet gc --apply` for corpses, `fleet rm` /
//     `fleet handoff` for live ones).
//
// All of this is behind FLEET_LEASE_FAILOVER inside coordlock (Acquire-
// LeaseWithKill returns ErrFailoverDisabled when off), so off-flag the
// closure is a cheap no-op that signals "legacy path" to runCoordRun.
func productionAcquireLease(opts coordRunOpts, stderr io.Writer) func() (coordLease, bool, []liveHolderInfo, error) {
	return func() (coordLease, bool, []liveHolderInfo, error) {
		// Step 1: stamp supervisor identity (best-effort, only when the
		// flag is on — off-flag we skip to keep records byte-identical).
		// spawn.Spawn writes the record BEFORE launching us, but we RETRY
		// briefly on ErrNotFound to be robust against any ordering skew
		// (codex PR2 iter-2 [P1]). The identity is what STONITH
		// authenticates a takeover against, so getting it onto the record
		// is load-bearing, not cosmetic.
		if leaseFailoverEnabled() {
			pid := os.Getpid()
			pidStart, ok := coordlock.PidStartNanos(pid)
			exe, exeErr := os.Executable()
			if !ok || exeErr != nil {
				_, _ = fmt.Fprintf(stderr,
					"coord-run: WARNING: could not read supervisor identity (pid_start ok=%v exe err=%v); "+
						"STONITH of this coord will refuse until re-stamped\n", ok, exeErr)
			} else if serr := stampSupervisorWithRetry(opts.agentID, pid, pidStart, exe); serr != nil {
				_, _ = fmt.Fprintf(stderr,
					"coord-run: WARNING: stamp supervisor identity for agent %s failed: %v "+
						"(continuing; STONITH may refuse on the unstamped record)\n", opts.agentID, serr)
			}
		}

		// Step 2: acquire with the authenticated kill injected. The KP6
		// live-holder detection rides along (converted to the
		// platform-neutral shape) so the standby poll loop can quarantine.
		lease, acquired, live, err := coordlock.AcquireLeaseWithKill(
			opts.project, opts.agentID,
			func(t coordlock.KillTarget) error {
				return coord.KillCoordIfIdentityMatches(coord.KillTarget{
					Pid:         t.Pid,
					PidStart:    t.PidStart,
					AgentID:     t.AgentID,
					Project:     t.Project,
					FencerEpoch: t.FencerEpoch,
				})
			})
		if err != nil || !acquired {
			return nil, acquired, liveHolderInfosFrom(live), err
		}

		// Step 3: new-leader competitor sweep — detect + report only (KP3).
		// Best-effort: a sweep error is surfaced but does NOT fail the
		// acquire — we already hold the flock, so we are the leader
		// regardless.
		sweepStaleCompetitors(opts.agentID, opts.project, stderr)
		return lease, true, nil, nil
	}
}

// liveHolderInfosFrom converts coordlock's exported detection tuples into
// the platform-neutral shape coord.go's seams carry.
func liveHolderInfosFrom(hs []coordlock.LiveHolder) []liveHolderInfo {
	if len(hs) == 0 {
		return nil
	}
	out := make([]liveHolderInfo, 0, len(hs))
	for _, h := range hs {
		out = append(out, liveHolderInfo{pid: h.Pid, pidStart: h.PidStart, agentID: h.AgentID})
	}
	return out
}

// stampSupervisorIdentityRetries / Delay bound the load-modify-write
// retry while the spawn's record write lands. Vars (not consts) so a test
// can shrink them.
var (
	stampSupervisorIdentityRetries = 25
	stampSupervisorIdentityDelay   = 100 * time.Millisecond
)

// stampSupervisorWithRetry stamps the supervisor identity, retrying on
// ErrNotFound until the spawn's record write lands (codex PR2 iter-2
// [P1]). spawn.Spawn now pre-writes the record before launching us, so
// the first attempt almost always wins; the bounded retry only covers a
// pathological ordering skew. Any non-ErrNotFound error is returned
// immediately (a real fault won't fix itself by waiting).
func stampSupervisorWithRetry(agentID string, pid int, pidStart int64, exe string) error {
	var lastErr error
	for attempt := 0; attempt < stampSupervisorIdentityRetries; attempt++ {
		err := agent.StampSupervisorIdentity(agentID, pid, pidStart, exe)
		if err == nil {
			return nil
		}
		if !errors.Is(err, state.ErrNotFound) {
			return err // real fault — surface now
		}
		lastErr = err
		time.Sleep(stampSupervisorIdentityDelay)
	}
	return lastErr
}

// leaseFailoverEnabled delegates to coordlock's single source of truth for
// the failover flag (PR4 flipped the default to ON; =0/false/off/no still
// disables). Kept as a thin wrapper so call sites read naturally.
func leaseFailoverEnabled() bool {
	return coordlock.FailoverEnabled()
}

func leaseActiveOwnerPID(project string) (int, bool) {
	return coordlock.CurrentActiveOwnerPID(project)
}

// supervisorAliveByStart is the pid-reuse-safe supervisor liveness probe
// backing BOTH the drain KP7 escalation gate (drainLeaseDeps.
// SupervisorAlive) and gc's stale-coords live/dead split
// (Deps.CoordSupervisorAlive). Contract (DESIGN-coord-no-auto-kill):
//
//	pid gone                        -> false (provably dead)
//	live pid, start-time mismatch   -> false (reused pid; recorded proc dead)
//	live pid, recorded pidStart==0  -> true  (identity UNPROVABLE — never
//	                                   treat an unprovable process as a
//	                                   corpse; no escalation, no reap)
//	live pid, start-time match      -> true
func supervisorAliveByStart(pid int, pidStart int64) bool {
	if pid <= 0 {
		return false
	}
	st, ok := coordlock.PidStartNanos(pid)
	if !ok {
		return false // no such process — provably dead
	}
	if pidStart == 0 {
		return true // identity unprovable — never escalate/reap on it
	}
	return st == pidStart
}

// wireGCCoordDeps installs the platform-gated stale-coords seams into a
// gc.Deps (DESIGN-coord-no-auto-kill gc spec). internal/gc is untagged so
// it keeps building on freebsd; the lease + kill primitives these seams
// need are linux||darwin-only, so the production wiring lives in THIS
// build-tagged file (the other-platform stub leaves them nil and the
// classifier fails safe: class (a) skipped, nothing ever signaled).
//
//   - CoordSupervisorAlive is the pid-reuse-safe (pid + pid_start) probe.
//     The live/dead split MUST be enforced by this gc-side pre-probe:
//     KillCoordIfIdentityMatches refuses only the CURRENT active lease
//     owner and WOULD signal a live stale competitor. A recorded
//     pid_start of 0 makes the identity unprovable -> treat as ALIVE
//     (never signal on an unprovable identity).
//   - KillCoord runs the authenticated kill (a provably-dead pid is a
//     benign no-op there) and then the coord cleanup the corpse's own
//     `defer Cleanup` never ran: archive the record + reap the session.
//   - CoordStandbyTimeout feeds class (b): the standby-timeout value is
//     owned by cmd/fleet, so gc receives it via Deps.
func wireGCCoordDeps(deps *gc.Deps) {
	deps.ActiveLeaseOwnerPID = coordlock.CurrentActiveOwnerPID
	deps.CoordSupervisorAlive = supervisorAliveByStart
	deps.KillCoord = func(t gc.CoordKillTarget) error {
		// The classifier only sends provably-dead targets, but re-verify
		// at the destructive boundary (TOCTOU belt): a pid+pid_start that
		// reads alive here is NEVER touched.
		if supervisorAliveByStart(t.Pid, t.PidStart) {
			return fmt.Errorf("refusing reap: supervisor pid=%d pid_start=%d is ALIVE (probe changed since classification)",
				t.Pid, t.PidStart)
		}
		// Belt-and-braces authenticated kill — a benign no-op on a corpse.
		// A typed REFUSAL is tolerated here and ONLY here: the refusal
		// gates (exe-path, active-owner, pid-reuse) exist to stop a signal
		// reaching the wrong LIVE process, and the corpse re-check above
		// proves there is nothing live to signal. Without this tolerance a
		// dead competitor whose record was never exe-stamped (the
		// stamp-failure warning path) would be unreapable forever. Real
		// infra faults still surface.
		if err := coord.KillCoordIfIdentityMatches(coord.KillTarget{
			Pid:      t.Pid,
			PidStart: t.PidStart,
			AgentID:  t.AgentID,
			Project:  t.Project,
		}); err != nil && !errors.Is(err, coord.ErrKillRefused) {
			return err
		}
		// The dead supervisor's own deferred cleanup never ran; reap its
		// tmux/engine session + archive the record (idempotent).
		return coord.Cleanup(t.AgentID, t.Project, coord.Default())
	}
	deps.CoordStandbyTimeout = defaultStandbyTimeout
}

func leaseLeaderPresent(project string) bool {
	return coordlock.LeaderPresent(project)
}

// sweepStaleCompetitors DETECTS and REPORTS any OTHER same-project coord
// record after we win the lease — it never signals a process or reaps a
// session (KP3, DESIGN-coord-no-auto-kill). The sweep fires on a
// staleness heuristic (records presumed stale after a lease win), and a
// staleness heuristic may report and quarantine but never terminate: the
// 2026-07-04 incident was exactly this machinery shooting live coords
// after a machine stall. The flock remains the singleton; reaping is
// operator-gated — `fleet gc` lists both families (dead => --apply reaps,
// live => surfaced with the `fleet rm <id>` / `fleet handoff <id>` hint).
//
// Two detected families, one report line + one fleetlog
// coord.quarantine{stale-competitor} event each:
//   - stamped competitors (another coord-run supervisor's record);
//   - unstamped lease-wrapped standbys (a `coord-run --standby` spawn
//     that never stamped its supervisor identity). LeaseWrapped is the
//     discriminator (agent.go): bare/legacy coords (SupervisorPID==0 &&
//     !LeaseWrapped) are OUT of scope entirely — never even reported.
//
// The no-invocation guarantee is asserted structurally by
// TestSweepStaleCompetitors_StructurallyKillFree.
func sweepStaleCompetitors(selfAgentID, project string, stderr io.Writer) {
	recs, err := agent.List()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "coord-run: sweep: list records: %v (skipping)\n", err)
		return
	}
	self := os.Getpid()
	report := func(r *agent.Record, msg string) {
		_, _ = fmt.Fprintln(stderr, msg)
		fleetlog.Log(fleetlog.CompCoord, "coord.quarantine", "warn", fleetlog.Fields{
			Proj:  project,
			Agent: r.ID,
			Msg:   msg,
			Data: map[string]any{
				"reason":    "stale-competitor",
				"pid":       r.SupervisorPID,
				"pid_start": r.SupervisorPidStart,
				"session":   r.TmuxSession,
			},
		})
	}
	for _, r := range recs {
		if r == nil || r.Project != project {
			continue
		}
		if r.ID == selfAgentID {
			continue // never report our own record
		}
		switch {
		case r.SupervisorPID > 0 && r.SupervisorPID != self:
			report(r, fmt.Sprintf(
				"coord-run: stale coord competitor detected (report-only): agent=%s supervisor_pid=%d project=%s — run `fleet gc` to reap a dead one, `fleet rm %s` or `fleet handoff %s` for a live one",
				r.ID, r.SupervisorPID, project, r.ID, r.ID))
		case r.SupervisorPID == 0 && r.LeaseWrapped &&
			spawn.IsCoordSpawn(r.TaskID, r.Project) && r.TmuxSession != "":
			report(r, fmt.Sprintf(
				"coord-run: stale unstamped standby detected (report-only): agent=%s session=%s project=%s — `fleet gc --apply` reaps it once it is past the standby timeout",
				r.ID, r.TmuxSession, project))
		}
	}
}
