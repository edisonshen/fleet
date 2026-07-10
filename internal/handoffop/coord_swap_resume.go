package handoffop

import (
	"github.com/edisonshen/fleet/internal/coordlock"
	"github.com/edisonshen/fleet/internal/tmux"
)

// coord_swap_resume.go re-derives the DELETED spawn marker's synchronous
// "did the swap commit?" oracle onto the coordinator LEASE, for the
// crash-recovery retry path (TASK-PLAN-coord-lease-sole-identity D3, resume
// matrix operator-approved 2026-07-07).
//
// The marker used to be a synchronous binary commit sentinel: on a `fleet
// handoff` / drain retry where the queued replacement NEW's tmux session is
// already dead, `marker == NEW` meant "the swap already committed to NEW, so
// respawning would DUPLICATE the coord — refuse." The lease commit is ASYNC:
// the winning standby bumps the epoch to NEW only AFTER OLD is killed/releases,
// so at the recovery probe the lease is in one of three states, not two:
//
//	owner == OLD        retire not started/finished        NOT committed
//	handoff{NEW} + no active owner   swap in flight          committing (standby recovers)
//	owner == NEW        epoch committed                      committed
//	free + no handoff   pre-commit crash / clean            NOT committed
//
// Critically OLD and NEW can NEVER both be the active lease owner, so the old
// "NEW-committed AND OLD-alive" duplicate-prevention clause changes CHARACTER:
// it is re-derived as a LEASE decision plus a DEFENSIVE OLD-liveness surface
// (operator-approved "lease decision + defensive OLD-liveness surface").
//
//	CurrentOwner == NEW  → REFUSE unless --force-replacement.
//	                       DEFENSE-IN-DEPTH: if a probe finds OLD's session
//	                       STILL ALIVE here (cross-socket blind spot) → LOUD
//	                       stderr diagnostic + refuse. NEVER auto-kills;
//	                       operator-gate (DESIGN-coord-no-auto-kill).
//	handoff{NEW}, no owner → WAIT: the standby is the recovery vehicle; do NOT
//	                       respawn a 2nd replacement.
//	CurrentOwner == OLD, or free lease + no handoff → RESPAWN (safe: not committed).

// ResumeAction is the crash-recovery verdict for a dead queued replacement.
type ResumeAction int

const (
	// ResumeRespawn: the swap is NOT committed (OLD still owns, or a free lease
	// with no handoff reservation) — drop the dead NEW and respawn safely.
	ResumeRespawn ResumeAction = iota
	// ResumeRefuse: the epoch already committed to NEW — respawning would
	// duplicate the coord. Refuse unless --force-replacement.
	ResumeRefuse
	// ResumeWait: a handoff reservation names NEW and no active owner exists —
	// the swap is in flight and the warm standby is the recovery vehicle. Do
	// NOT respawn a second replacement; wait / redeliver.
	ResumeWait
)

func (a ResumeAction) String() string {
	switch a {
	case ResumeRespawn:
		return "respawn"
	case ResumeRefuse:
		return "refuse"
	case ResumeWait:
		return "wait"
	default:
		return "resume-action(?)"
	}
}

// CoordSwapResumeDeps are the injected lease/probe seams. Production wires
// coordlock + tmux via DefaultCoordSwapResumeDeps; deterministic tests build
// the committed/in-flight/free × NEW-dead/live matrix in memory (no disk, no
// clock).
type CoordSwapResumeDeps struct {
	// CurrentOwner reports the live flock holder WITH a readable identity
	// (busy⇒owner + agent_id, PR-2 flock-only).
	CurrentOwner func(project string) (coordlock.Owner, bool)
	// ClassifyHandoff inspects the write-once handoff journal for a FLOCK-FREE
	// project (PR-2 D3/D4, Decision 7 Abandon): none / in-flight (successor
	// process alive) / abandoned (successor dead). Replaces the deleted epoch
	// CurrentHandoff reservation. Production: coordlock.ClassifyFreeFlockHandoff.
	ClassifyHandoff func(project string) (coordlock.HandoffDisposition, coordlock.HandoffJournal, error)
	// FlockLive reports whether the coordinator flock is currently HELD by a
	// live process, regardless of whether its body identity is readable.
	// coordlock.ClassifyFreeFlockHandoff's own contract requires the caller to
	// have ALREADY established the flock is free before calling it — but
	// CurrentOwner's ok=false conflates two distinct states (genuinely free,
	// vs busy with an identity-pending/torn body), so case (2) below must not
	// infer "free" from !ownerOK alone (review adversarial-subagent finding).
	// nil defaults to "not free" (the safe direction: falls through to case
	// (3)'s respawn, which flock-exclusivity stands down on any real
	// collision — never the reverse).
	FlockLive func(project string) bool
	// OldSessionAlive is the DEFENSIVE cross-socket OLD-liveness probe, used
	// only on the committed (refuse) arm. It NEVER drives a kill — only shapes
	// the refusal diagnostic.
	OldSessionAlive func(session string) (bool, error)
}

// DefaultCoordSwapResumeDeps wires the classifier to the real flock/journal +
// tmux probe. The OLD-liveness probe routes through the package tmux seam so
// tests of the call sites can fake it.
func DefaultCoordSwapResumeDeps() CoordSwapResumeDeps {
	return CoordSwapResumeDeps{
		CurrentOwner:    coordlock.CurrentOwner,
		ClassifyHandoff: coordlock.ClassifyFreeFlockHandoff,
		OldSessionAlive: tmux.SessionAlive,
		FlockLive: func(project string) bool {
			_, ok := coordlock.LiveOwner(project) // ok=true iff busy⇒owner; false ONLY when free.
			return ok
		},
	}
}

// CoordSwapResume is the classifier verdict plus the defensive-probe evidence
// the caller needs to shape its stderr diagnostic.
type CoordSwapResume struct {
	Action ResumeAction
	// OldStillAlive is set only on the ResumeRefuse (committed) arm: the
	// defensive OLD-liveness probe found OLD's session STILL ALIVE — a
	// cross-socket blind-spot signal. It NEVER triggers a kill; it only makes
	// the refusal diagnostic LOUD (surface + operator-gate).
	OldStillAlive bool
	// OldProbeErr is a non-nil defensive-probe error on the committed arm
	// (ambiguous OLD liveness → still refuse, conservative).
	OldProbeErr error
	// Reason is a short human-readable explanation for the diagnostic.
	Reason string
}

// ClassifyCoordSwapResume decides what to do when the queued replacement NEW's
// session is confirmed dead on a handoff/drain retry. Pure: all state comes
// through d; no side effects.
func ClassifyCoordSwapResume(project, oldID, oldSession, newID string, force bool, d CoordSwapResumeDeps) CoordSwapResume {
	owner, ownerOK := d.CurrentOwner(project)

	// (1) Committed: the epoch names NEW. Respawning would duplicate the coord.
	if ownerOK && owner.AgentID == newID {
		if force {
			return CoordSwapResume{
				Action: ResumeRespawn,
				Reason: "epoch committed to replacement " + newID + " but --force-replacement given",
			}
		}
		res := CoordSwapResume{
			Action: ResumeRefuse,
			Reason: "epoch committed to replacement " + newID,
		}
		// Defensive OLD-liveness probe (belt-and-suspenders). The lease
		// commit-ordering invariant means owner==NEW should imply OLD already
		// released/died; a cross-socket OLD is the blind spot — surface, never
		// kill.
		if d.OldSessionAlive != nil && oldSession != "" {
			alive, perr := d.OldSessionAlive(oldSession)
			res.OldStillAlive = alive
			res.OldProbeErr = perr
		}
		return res
	}

	// (2) Swap in flight: the flock is free and the write-once journal names NEW
	// as a successor whose process is still ALIVE (booting, not yet holding the
	// flock). The standby IS the recovery vehicle — do NOT respawn a second
	// replacement. A journal read error is treated conservatively as NOT
	// in-flight (fall through to respawn — flock-exclusivity bounds a duplicate
	// to one transient stand-down; the alternative, stranding on an unreadable
	// journal, is worse).
	//
	// !ownerOK alone is NOT sufficient to establish "flock free" — CurrentOwner
	// also reports ok=false for a BUSY flock with an identity-pending/torn body
	// (old-binary straggler, or the narrow stampFlockBody stamp window). Gate
	// on FlockLive too, so a busy-but-unreadable flock never routes through the
	// free-flock handoff classifier.
	if !ownerOK && d.FlockLive != nil && !d.FlockLive(project) {
		if disp, j, herr := d.ClassifyHandoff(project); herr == nil &&
			disp == coordlock.HandoffInFlight && j.SuccessorID == newID {
			return CoordSwapResume{
				Action: ResumeWait,
				Reason: "handoff journal names successor " + newID + " (process alive); standby is the recovery vehicle",
			}
		}
	}

	// (3) Not committed: the flock is held by OLD (or anyone that is not NEW),
	// or free with no in-flight journal (a pre-commit crash, an abandoned
	// successor, or a clean free flock). Safe to drop the dead NEW and respawn a
	// fresh replacement — flock-exclusivity stands it down if a live holder
	// races in.
	return CoordSwapResume{
		Action: ResumeRespawn,
		Reason: "swap not committed (flock held by non-NEW, or free flock with no in-flight handoff journal)",
	}
}
