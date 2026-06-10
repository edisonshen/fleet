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
	"github.com/edisonshen/fleet/internal/spawn"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tmux"
)

// defaultAcquireLease is the production lease-acquire seam runCoordRun
// uses when opts.acquireLease is nil (see productionAcquireLease).
func defaultAcquireLease(opts coordRunOpts, stderr io.Writer) func() (coordLease, bool, error) {
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
//  3. On acquire, run the new-leader competitor SWEEP: enumerate
//     same-project coord supervisors and reap any stale straggler (a
//     pre-lease or cross-socket coord the flock alone wouldn't catch)
//     through the SAME authenticated primitive. The flock is the primary
//     singleton; the sweep is belt-and-braces.
//
// All of this is behind FLEET_LEASE_FAILOVER inside coordlock (Acquire-
// LeaseWithKill returns ErrFailoverDisabled when off), so off-flag the
// closure is a cheap no-op that signals "legacy path" to runCoordRun.
func productionAcquireLease(opts coordRunOpts, stderr io.Writer) func() (coordLease, bool, error) {
	return func() (coordLease, bool, error) {
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

		// Step 2: acquire with the authenticated kill injected.
		lease, acquired, err := coordlock.AcquireLeaseWithKill(
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
			return nil, acquired, err
		}

		// Step 3: new-leader competitor sweep. Best-effort: a sweep error
		// is surfaced but does NOT fail the acquire — we already hold the
		// flock, so we are the leader regardless.
		sweepStaleCompetitors(opts.agentID, opts.project, stderr)
		return lease, true, nil
	}
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

func leaseLeaderPresent(project string) bool {
	return coordlock.LeaderPresent(project)
}

var (
	sweepKillCoordFn   = coord.KillCoordIfIdentityMatches
	sweepKillSessionFn = tmux.Kill
)

// sweepStaleCompetitors reaps any OTHER same-project coord supervisor
// through the authenticated kill primitive after we win the lease. The
// flock is the primary singleton; this catches a pre-lease or
// cross-tmux-socket straggler the flock alone can't see. The primitive
// re-validates every target (pid-reuse, exe-path, epoch, self) and
// refuses unless it is provably a stale same-project coord-run — so this
// can never shoot the new leader (us) or a different project's coord.
func sweepStaleCompetitors(selfAgentID, project string, stderr io.Writer) {
	recs, err := agent.List()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "coord-run: sweep: list records: %v (skipping)\n", err)
		return
	}
	self := os.Getpid()
	for _, r := range recs {
		if r == nil || r.Project != project {
			continue
		}
		if r.ID == selfAgentID {
			continue // never target our own record
		}
		if r.SupervisorPID <= 0 {
			// Unstamped record: SupervisorPID==0 means either (a) a
			// lease-wrapped standby that lost the race / hasn't stamped its
			// supervisor identity yet — safe to reap, OR (b) a live LEGACY/
			// BARE coord that never runs a supervisor at all. We must NOT
			// blind-kill (b): a bare coord can be the only working
			// coordinator, and reaping its session bypasses the handoff
			// readiness/rollback path and can strand the project coord-less
			// mid-handoff (codex iter-28 P1).
			//
			// LeaseWrapped is the authoritative discriminator (agent.go):
			// true ONLY for `coord-run --standby` spawns, false/absent for
			// legacy + bare/direct successors. Reap only the lease-wrapped,
			// not-yet-stamped losing standby.
			if r.LeaseWrapped && spawn.IsCoordSpawn(r.TaskID, r.Project) && r.TmuxSession != "" {
				if err := sweepKillSessionFn(r.TmuxSession); err != nil &&
					!errors.Is(err, tmux.ErrNoSession) {
					_, _ = fmt.Fprintf(stderr, "coord-run: sweep reap standby session=%s agent=%s: %v\n",
						r.TmuxSession, r.ID, err)
				}
			}
			continue
		}
		if r.SupervisorPID == self {
			continue // it's us
		}
		if err := sweepKillCoordFn(coord.KillTarget{
			Pid:      r.SupervisorPID,
			PidStart: r.SupervisorPidStart,
			AgentID:  r.ID,
			Project:  project,
			// The sweep runs AFTER we became the active owner, so any
			// other coord is fenced by construction; the primitive's
			// "never shoot the current active owner" gate protects us.
			FencerEpoch: 0,
		}); err != nil && !errors.Is(err, coord.ErrKillRefused) {
			// A refusal is expected + benign (most records won't match);
			// only surface real signal/infra faults.
			_, _ = fmt.Fprintf(stderr, "coord-run: sweep reap pid=%d agent=%s: %v\n",
				r.SupervisorPID, r.ID, err)
		}
	}
}
