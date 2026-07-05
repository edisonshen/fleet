package gc

// stale_coords.go — KindStaleCoords (DESIGN-coord-no-auto-kill, task
// coord-no-auto-kill-ac54). The new-leader lease sweep used to KILL two
// families of same-project coord leftovers on a staleness heuristic; the
// sweep is now detect+report only, and the reap moved HERE, operator-
// gated behind `fleet gc --apply`, with a HARD live/dead split:
//
//	class (a) STAMPED COMPETITOR — LeaseWrapped && SupervisorPID>0 &&
//	    SupervisorPID != the project's current ACTIVE lease owner pid
//	    (its own claim on the lease is stale by construction: someone
//	    else owns the generation).
//	      supervisor DEAD (pid+pid_start pre-probe) -> reapable under
//	          --apply via the KillCoord seam (authenticated kill of a
//	          corpse is a benign no-op + the cleanup its defer never ran);
//	      supervisor ALIVE -> listed live-stale with the explicit
//	          per-target commands (`fleet rm <id>` / `fleet handoff <id>`);
//	          --apply SKIPS it with a printed reason. gc NEVER signals a
//	          live pid — the gc-side pre-probe is the enforcement, NOT
//	          KillCoordIfIdentityMatches (which refuses only the current
//	          active owner and WOULD signal a live stale competitor).
//	class (b) UNSTAMPED LEASE-WRAPPED STANDBY — LeaseWrapped &&
//	    SupervisorPID==0 (a `coord-run --standby` that never stamped),
//	    tmux session still present past the standby timeout (a healthy
//	    standby self-exits at that timeout, so a lingering session is
//	    wedged/crashed). --apply reaps the SESSION (a session kill of an
//	    unstamped wedged standby, not a pid signal).
//
//	Bare legacy coords (SupervisorPID==0 && !LeaseWrapped) are NEVER
//	candidates in either class — a bare coord can be the only working
//	coordinator and holds no lease to be stale against.
//
// Platform seams (ActiveLeaseOwnerPID / CoordSupervisorAlive / KillCoord)
// are wired by cmd/fleet's build-tagged wireGCCoordDeps; on platforms
// without the lease primitives they stay nil and class (a) is skipped
// entirely (fail-safe: nothing is ever signaled). CoordStandbyTimeout is
// owned by cmd/fleet (the coord-run --standby default); zero skips
// class (b).

import (
	"fmt"
	"time"

	"github.com/edisonshen/fleet/internal/spawn"
)

// KindStaleCoords is the tenth classifier: stale same-project coord
// leftovers (the lease sweep's former kill targets).
const KindStaleCoords Kind = "stale-coords"

// CoordKillTarget is the gc-local kill-target shape handed to the
// KillCoord seam. Deliberately NOT internal/coord.KillTarget:
// internal/coord/kill.go is `//go:build linux || darwin` and internal/gc
// must keep building on freebsd, so gc never imports the coord package.
type CoordKillTarget struct {
	Pid      int
	PidStart int64
	AgentID  string
	Project  string
}

// reconcileStaleCoords runs both candidate classes. Surface-don't-silo:
// every skip that hides a possible candidate is either structural (nil
// platform seam on an unsupported GOOS) or conservative-by-design
// (cannot prove competitor/dead).
func reconcileStaleCoords(r *Report, opts Options, deps Deps) error {
	records, err := deps.ListAgents()
	if err != nil {
		return err
	}
	now := deps.Now()
	for _, rec := range records {
		if rec == nil || !rec.LeaseWrapped {
			continue // bare/legacy coords: never candidates (either class)
		}
		if opts.Project != "" && rec.Project != opts.Project {
			continue
		}
		switch {
		case rec.SupervisorPID > 0:
			staleCoordCompetitor(r, opts, deps, rec.ID, rec.Project,
				rec.SupervisorPID, rec.SupervisorPidStart)
		default:
			staleCoordUnstampedStandby(r, opts, deps, rec.ID,
				rec.TmuxSession, rec.TaskID, rec.Project, now.Sub(rec.SpawnedAt))
		}
	}
	return nil
}

// staleCoordCompetitor classifies one stamped record (class a).
func staleCoordCompetitor(r *Report, opts Options, deps Deps, id, project string, pid int, pidStart int64) {
	// Structural: without the platform seams we can neither prove the
	// record is a competitor nor that its pid is dead — skip (freebsd /
	// narrow Deps). Never guess toward a kill.
	if deps.ActiveLeaseOwnerPID == nil || deps.CoordSupervisorAlive == nil {
		return
	}
	ownerPid, ok := deps.ActiveLeaseOwnerPID(project)
	if !ok {
		// No readable ACTIVE lease generation: the record might be the
		// (expired/heartbeat-stale) leader itself. Not provably a
		// competitor -> not a candidate.
		return
	}
	if ownerPid == pid {
		return // the current active owner is never a candidate
	}
	if deps.CoordSupervisorAlive(pid, pidStart) {
		// LIVE stale competitor: surface with the explicit per-target
		// commands. --apply must never signal a live pid, so under Apply
		// the action stays a surface with a printed skip reason.
		reason := fmt.Sprintf(
			"live stale coord competitor (supervisor pid=%d alive, not the lease owner pid=%d); "+
				"reap manually with `fleet rm %s` or `fleet handoff %s`",
			pid, ownerPid, id, id)
		if opts.Apply {
			reason = "skipped under --apply: " + reason
		}
		r.Actions = append(r.Actions, Action{
			Kind: KindStaleCoords, Target: id, Verb: VerbSurface, Reason: reason,
		})
		return
	}
	// DEAD competitor: reapable under --apply through the KillCoord seam.
	act := Action{Kind: KindStaleCoords, Target: id, Verb: VerbWouldKill,
		Reason: fmt.Sprintf("dead stale coord competitor (supervisor pid=%d gone; lease owner pid=%d)",
			pid, ownerPid)}
	if opts.Apply {
		if deps.KillCoord == nil {
			act.Verb = VerbSurface
			act.Reason = "kill seam unwired on this platform; " + act.Reason
		} else if kerr := deps.KillCoord(CoordKillTarget{
			Pid: pid, PidStart: pidStart, AgentID: id, Project: project,
		}); kerr != nil {
			act.Reason = fmt.Sprintf("reap failed: %v", kerr)
		} else {
			act.Verb = VerbKilled
		}
	}
	r.Actions = append(r.Actions, act)
}

// staleCoordUnstampedStandby classifies one unstamped lease-wrapped
// record (class b): a standby that never stamped its supervisor identity
// and whose session outlived the standby timeout.
func staleCoordUnstampedStandby(r *Report, opts Options, deps Deps, id, session, taskID, project string, age time.Duration) {
	if deps.CoordStandbyTimeout <= 0 {
		return // timeout not wired (narrow Deps / unsupported platform)
	}
	if session == "" || !spawn.IsCoordSpawn(taskID, project) {
		return
	}
	if age <= deps.CoordStandbyTimeout {
		return // a live standby may still be polling; never touch it
	}
	alive, perr := deps.SessionAlive(session)
	if perr != nil || !alive {
		return // gone or unprovable — nothing to reap here
	}
	act := Action{Kind: KindStaleCoords, Target: id, Verb: VerbWouldKill,
		Reason: fmt.Sprintf("wedged unstamped standby: session %s alive %s after spawn (standby timeout %s)",
			session, humanDuration(age), humanDuration(deps.CoordStandbyTimeout))}
	if opts.Apply {
		if kerr := deps.KillSession(session); kerr != nil {
			act.Reason = fmt.Sprintf("session kill failed: %v", kerr)
		} else {
			act.Verb = VerbKilled
		}
	}
	r.Actions = append(r.Actions, act)
}
