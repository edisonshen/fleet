// Package coordreconcile is the ONE lease-owner resolver that both
// `fleet attach` (Tier 3 PROJECT RECOVERY) and the TUI `[a]` handler call
// to answer "who is project P's coord, and what should I do to reach
// exactly one?" — WITHOUT a coord-spawn marker. The lease
// (coordinator.epoch) is the sole source of truth
// (DESIGN-coord-lifecycle-crash-recovery §2A; TASK-PLAN-coord-lease-sole-identity
// D4). Sharing one resolver across the CLI and the TUI closes the exact
// CLI/TUI asymmetry that caused the 2026-06-18 incident, where `[a]` trusted
// a stale marker over the live lease and spawned a duplicate beside a healthy
// coord.
//
// The resolver introduces NO new kill path (no-auto-kill invariant,
// DESIGN-coord-no-auto-kill): every decision is either an ATTACH to a
// process-live owner, a WAIT (poll + re-resolve), a fresh SPAWN into a
// provably-free lease, or a SPAWN-STANDBY after a PURE RECORD CAS that
// signals no process. It never fences or kills a live coordinator.
//
// Decision matrix (evaluated top-to-bottom; first match wins):
//
//	LiveOwner (active, pid alive, TTL-independent) -> ATTACH  (T3, T7b)
//	starting within TTL                            -> WAIT    (T2: boot in flight)
//	starting past TTL, owner LIVE                   -> SUPERSEDE + SPAWN-STANDBY (T6s)
//	starting past TTL, owner DEAD                   -> claim   (falls to free branch)
//	valid handoff reservation                       -> WAIT   (#247 gap-window rule a)
//	free / dead-active lease                         -> CLAIM + SPAWN (T7a, T8)
//
// The ATTACH gate keys on PROCESS-LIVENESS (LiveOwner), not TTL-health, so a
// live-but-stale leader (a machine sleep briefly stalled its renewal) is
// attached to and never spawned beside — the "process-live overrides stale
// heartbeat" rule (§7.0). By the time control reaches the free/dead branch,
// any active record has a provably-DEAD owner, so the ClaimStartingRecord
// there can never clobber a live coord's lease.
package coordreconcile

import (
	"fmt"

	"github.com/edisonshen/fleet/internal/coordlock"
)

// Decision is the resolver's verdict — what the CALLER (attach exec-tmux /
// TUI spawn Cmd) must do next. The resolver performs the lease-record CAS
// side effects (Supersede / ClaimStarting) itself; the caller owns the
// process-level side effects (attach, spawn, poll) because those differ
// between the CLI and the TUI.
type Decision int

const (
	// Attach: a process-live active lease owner exists — attach to it. The
	// Verdict.Owner names the agent record whose tmux session to enter.
	Attach Decision = iota
	// Wait: a healthy boot is in flight (starting within TTL) or a valid
	// handoff reservation names a successor, or a benign CAS race lost — the
	// caller polls (bounded) and re-resolves. NEVER spawns a duplicate.
	Wait
	// Spawn: the lease was free / dead-owned — the resolver claimed a
	// `starting` record naming the caller's agentID (spawn-serialization) and
	// the caller should spawn a fresh coord that acquires the free flock.
	Spawn
	// SpawnStandby: a `starting` owner was wedged past its TTL but its process
	// is still LIVE (holding the flock). The resolver superseded the stale
	// record (a PURE record CAS — NO process signalled, no-auto-kill) so the
	// zombie's Activate flip fails closed; the caller should spawn a
	// `coord-run --standby` that polls behind the still-held flock and wins
	// when the zombie dies. The hung session is left as a visible zombie for
	// `fleet gc`'s dead/live split.
	SpawnStandby
)

func (d Decision) String() string {
	switch d {
	case Attach:
		return "attach"
	case Wait:
		return "wait"
	case Spawn:
		return "spawn"
	case SpawnStandby:
		return "spawn-standby"
	default:
		return fmt.Sprintf("decision(%d)", int(d))
	}
}

// Verdict is the resolver's answer.
type Verdict struct {
	Decision Decision
	// Owner is set only for Decision == Attach: the process-live active lease
	// owner to attach to.
	Owner coordlock.Owner
	// Reason is a short human-readable explanation for stderr/fleetlog
	// surfacing (feedback_surface_dont_silo: never silently reconcile).
	Reason string
}

// Deps are the injected lease seams. Production callers use DefaultDeps();
// tests build a deterministic in-memory matrix. All reads are TTL/clock
// pure via the underlying coordlock cfg, so a table-driven test drives every
// branch without sleeps.
type Deps struct {
	// LiveOwner reports a process-live active owner (TTL-independent).
	LiveOwner func(project string) (coordlock.Owner, bool)
	// CurrentStarting reports a `starting` record + liveness + within-TTL.
	CurrentStarting func(project string) (coordlock.StartingStatus, bool)
	// CurrentHandoff reports a non-expired successor reservation.
	CurrentHandoff func(project string) (coordlock.Handoff, bool)
	// Supersede bumps the epoch of a wedged `starting` record (pure CAS).
	Supersede func(project string, observedEpoch int64) (bool, error)
	// ClaimStarting writes a `starting` record naming agentID iff the lease
	// is claimable (free / dead / no live boot / no valid handoff).
	ClaimStarting func(project, agentID string) (bool, error)
}

// DefaultDeps wires the resolver to the real coordlock lease primitives.
func DefaultDeps() Deps {
	return Deps{
		LiveOwner:       coordlock.LiveOwner,
		CurrentStarting: coordlock.CurrentStarting,
		CurrentHandoff:  coordlock.CurrentHandoff,
		Supersede:       coordlock.SupersedeStartingLease,
		ClaimStarting:   coordlock.ClaimStartingRecord,
	}
}

// Resolve answers "what should a caller do to reach project P's sole coord?"
//
// agentID is the caller's pre-allocated coord id, stamped into the `starting`
// record on a fresh claim (Spawn / SpawnStandby) as the spawn-serialization
// token. It MAY be "" for a pure read (a poll that only wants Attach/Wait);
// a decision that would spawn with an empty agentID degrades to Wait, since
// the caller cannot spawn without an id.
//
// Resolve performs the lease-record CAS side effects (Supersede / ClaimStarting)
// but NOT the process side effects (attach / spawn) — the caller owns those.
// A benign CAS race (another spawn won between our reads and our CAS) always
// resolves to Wait, so the caller re-resolves rather than spawning a
// duplicate — the "exactly one coord" guarantee (T8) under concurrent [a].
func Resolve(d Deps, project, agentID string) (Verdict, error) {
	// (1) A PROCESS-LIVE active owner is the coord — attach, regardless of
	// heartbeat TTL (process-live overrides stale heartbeat; the incident
	// regression T3 needs no marker, and T7b's live-wedged leader is never
	// spawned beside).
	if owner, ok := d.LiveOwner(project); ok {
		return Verdict{
			Decision: Attach,
			Owner:    owner,
			Reason:   "process-live active lease owner " + owner.AgentID,
		}, nil
	}

	// (2) A `starting` owner: a coord acquired the flock and is booting its
	// /coordinator loop but has not flipped to `active` yet.
	if st, ok := d.CurrentStarting(project); ok {
		if st.WithinTTL {
			// Healthy boot in flight -> WAIT regardless of OwnerLive: the
			// owner pid is not stamped during the pre-spawn
			// ClaimStartingRecord window, so a not-yet-live owner within TTL
			// is still a legitimate boot, not a duplicate trigger (T2).
			return Verdict{
				Decision: Wait,
				Reason:   "coord boot in flight (starting within TTL)",
			}, nil
		}
		if st.Owner.PID > 0 && st.OwnerLive {
			// Wedged past startingTTL but the process is LIVE and holding the
			// flock (T6s). Supersede the stale record — a PURE record CAS that
			// signals NO process (no-auto-kill) — so the zombie's Activate
			// fails closed, then spawn a `--standby` that polls behind the
			// held flock. A fresh (non-standby) coord-run would stand down on
			// the live-starting holder and leave no coord.
			//
			// Note: a repeated [a] press while the zombie lingers re-bumps the
			// epoch each pass (the resolver is stateless and cannot see the
			// already-polling standby). That is bounded epoch inflation on
			// rare manual presses, NOT a poll loop — acceptable.
			bumped, err := d.Supersede(project, st.Epoch)
			if err != nil {
				return Verdict{}, fmt.Errorf("coordreconcile: supersede wedged starting lease (project=%q epoch=%d): %w", project, st.Epoch, err)
			}
			if !bumped {
				// Benign race: the record flipped to active/released or moved
				// to a newer epoch. Re-resolve.
				return Verdict{
					Decision: Wait,
					Reason:   "wedged starting lease raced during supersede; re-resolve",
				}, nil
			}
			if agentID == "" {
				return Verdict{
					Decision: Wait,
					Reason:   "wedged starting superseded; caller has no id to spawn a standby",
				}, nil
			}
			return Verdict{
				Decision: SpawnStandby,
				Reason:   "wedged starting lease superseded (record CAS, no process signalled); spawn standby behind held flock",
			}, nil
		}
		// A `starting` record whose owner is DEAD (or unset): the flock is
		// (or will be) free. Fall through to the free/dead claim branch —
		// ClaimStartingRecord overwrites a past-TTL starting record, which
		// fences the dead starter (its Activate no longer names it).
	}

	// (3) A valid handoff reservation names a successor still within its TTL
	// (#247 gap-window rule a): WAIT for that successor rather than spawning a
	// duplicate. If the successor died mid-boot, CurrentHandoff already
	// reports the reservation expired -> we fall through to spawn (#247 fix).
	if h, ok := d.CurrentHandoff(project); ok {
		return Verdict{
			Decision: Wait,
			Reason:   "handoff reservation naming successor " + h.SuccessorID,
		}, nil
	}

	// (4) Free / dead-active lease. Step (1) already excluded every
	// PROCESS-LIVE active owner, so any active record here has a provably-dead
	// owner — the ClaimStartingRecord CAS can never clobber a live coord.
	if agentID == "" {
		return Verdict{
			Decision: Wait,
			Reason:   "lease free but caller has no id to claim",
		}, nil
	}
	claimed, err := d.ClaimStarting(project, agentID)
	if err != nil {
		return Verdict{}, fmt.Errorf("coordreconcile: claim starting record (project=%q agent=%q): %w", project, agentID, err)
	}
	if !claimed {
		// A concurrent spawn/owner claimed the lease between our reads and the
		// CAS -> stand down and re-resolve (T8: exactly one coord wins).
		return Verdict{
			Decision: Wait,
			Reason:   "lease claimed by a concurrent spawn; re-resolve",
		}, nil
	}
	return Verdict{
		Decision: Spawn,
		Reason:   "free lease claimed (starting record written)",
	}, nil
}
