// Package coordreconcile is the ONE lease-owner resolver that both
// `fleet attach` (Tier 3 PROJECT RECOVERY) and the TUI `[a]` handler call
// to answer "who is project P's coord, and what should I do to reach exactly
// one?" — WITHOUT a coord-spawn marker. Under the FLOCK-ONLY lease
// (DESIGN-coord-lease-flock-only) the flock is the sole source of truth:
// a live process holding coordinator.flock IS the coord.
//
// The resolver introduces NO kill path (no-auto-kill invariant): every
// decision is an ATTACH to a live flock holder, a WAIT (poll + re-resolve),
// or a fresh SPAWN into a provably-free lease. It never fences or kills.
//
// Decision matrix (flock-only, PR-2 D4 — evaluated top-to-bottom):
//
//	flock HELD + body agent_id     -> ATTACH  (a live coord leads)
//	flock HELD + identity-less     -> WAIT    (poll for the identity; attach's
//	                                           PROJECT-RECOVERY falls back to
//	                                           FindLiveCoord, D9e — never spawns beside)
//	flock FREE + journal, succ ALIVE -> WAIT   (a handoff is in flight; don't
//	                                            spawn beside the booting successor)
//	flock FREE + journal, succ DEAD  -> clear the stale journal+barrier THEN SPAWN
//	flock FREE + no journal         -> SPAWN
//
// PR-2 deleted the epoch `starting`/`Supersede`/`SpawnStandby` sub-matrix (no
// starting state exists any more) and replaced the epoch's CurrentHandoff
// reservation with the write-once handoff journal (Decision 7). The
// clear-before-spawn on a dead successor is MANDATORY here (not only in
// coord_swap_resume): an attach-spawned coord has a fresh identity, so a spawn
// WITHOUT the preceding clear would leave a journal naming a now-dead successor
// that no live holder matches — wedging every future O_EXCL handoff.
package coordreconcile

import (
	"fmt"

	"github.com/edisonshen/fleet/internal/coordlock"
)

// Decision is the resolver's verdict — what the CALLER (attach exec-tmux /
// TUI spawn Cmd) must do next.
type Decision int

const (
	// Attach: a live flock holder with a readable identity exists — attach to
	// it. Verdict.Owner names the agent record whose tmux session to enter.
	Attach Decision = iota
	// Wait: a live flock holder whose identity is not yet readable
	// (identity-pending), or a handoff in flight (a booting successor), or a
	// benign race — the caller polls (bounded) and re-resolves. NEVER spawns.
	Wait
	// Spawn: the flock was FREE with no in-flight handoff (or an abandoned one
	// the resolver just cleared) — the caller should spawn a fresh coord that
	// acquires the free flock.
	Spawn
)

func (d Decision) String() string {
	switch d {
	case Attach:
		return "attach"
	case Wait:
		return "wait"
	case Spawn:
		return "spawn"
	default:
		return fmt.Sprintf("decision(%d)", int(d))
	}
}

// Verdict is the resolver's answer.
type Verdict struct {
	Decision Decision
	// Owner is set only for Decision == Attach: the live flock holder to attach to.
	Owner coordlock.Owner
	// Reason is a short human-readable explanation for stderr/fleetlog
	// surfacing (feedback_surface_dont_silo: never silently reconcile).
	Reason string
}

// Deps are the injected lease seams. Production callers use DefaultDeps();
// tests build a deterministic in-memory matrix.
type Deps struct {
	// LiveOwner reports a live flock holder (busy⇒owner, D1). ok=false when the
	// flock is FREE.
	LiveOwner func(project string) (coordlock.Owner, bool)
	// ClassifyHandoff inspects the handoff journal for a FLOCK-FREE project
	// (Decision 7 Abandon): none / in-flight (successor alive) / abandoned
	// (successor dead). Production: coordlock.ClassifyFreeFlockHandoff.
	ClassifyHandoff func(project string) (coordlock.HandoffDisposition, coordlock.HandoffJournal, error)
	// ClearHandoff clears a stale journal + its barrier before an abandoned-
	// successor respawn (clear-before-spawn). Production: coordlock.DeleteHandoffJournal.
	ClearHandoff func(project string) error
}

// DefaultDeps wires the resolver to the real coordlock flock/journal primitives.
func DefaultDeps() Deps {
	return Deps{
		LiveOwner:       coordlock.LiveOwner,
		ClassifyHandoff: coordlock.ClassifyFreeFlockHandoff,
		ClearHandoff:    coordlock.DeleteHandoffJournal,
	}
}

// Resolve answers "what should a caller do to reach project P's sole coord?"
//
// agentID is the caller's pre-allocated coord id (used only to gate Spawn — a
// decision that would spawn with an empty agentID degrades to Wait since the
// caller cannot spawn without an id). It MAY be "" for a pure read.
func Resolve(d Deps, project, agentID string) (Verdict, error) {
	// (1) The flock is HELD by a live process (busy⇒owner). A live holder —
	// busy, booting, or version-skewed — is the coord and is NEVER spawned
	// beside (the incident fix; no-auto-kill).
	if owner, ok := d.LiveOwner(project); ok {
		if owner.AgentID == "" {
			// Identity-less body (old-binary straggler / torn). The attach
			// callers hard-error on an empty AgentID, so Attach-with-empty-id
			// would dead-end. Wait (poll for the identity); attach's
			// PROJECT-RECOVERY chain falls back to projectlookup.FindLiveCoord
			// (D9e) to Attach via the agent record — never a spawn-beside.
			return Verdict{
				Decision: Wait,
				Reason:   "flock held by a live coord whose identity is not yet readable; polling",
			}, nil
		}
		return Verdict{
			Decision: Attach,
			Owner:    owner,
			Reason:   "live flock holder " + owner.AgentID,
		}, nil
	}

	// (2) The flock is FREE — consult the handoff journal (Decision 7 Abandon).
	disp, _, err := d.ClassifyHandoff(project)
	if err != nil {
		// A torn/unreadable journal is ambiguous: a handoff may be in flight.
		// Do NOT spawn beside a possibly-booting successor — Wait and re-resolve.
		return Verdict{
			Decision: Wait,
			Reason:   fmt.Sprintf("handoff journal for %q unreadable (%v); polling before spawn", project, err),
		}, nil
	}
	switch disp {
	case coordlock.HandoffInFlight:
		// A handoff is in flight and the recorded successor's process is ALIVE
		// (booting, not yet holding the flock). Hold off — don't spawn beside it.
		return Verdict{
			Decision: Wait,
			Reason:   "handoff in flight; successor process alive and acquiring the flock",
		}, nil
	case coordlock.HandoffAbandoned:
		// The recorded successor died mid-boot. Clear the stale journal + barrier
		// FIRST (clear-before-spawn — else the fixed-path O_EXCL wedges every
		// future handoff), then spawn a fresh coord.
		if d.ClearHandoff != nil {
			if cerr := d.ClearHandoff(project); cerr != nil {
				return Verdict{}, fmt.Errorf("coordreconcile: clear abandoned handoff journal (project=%q): %w", project, cerr)
			}
		}
		// fall through to the spawn gate below
	}

	// (3) Free flock, no in-flight handoff (or an abandoned one just cleared) ->
	// spawn a fresh coord that acquires the free flock.
	if agentID == "" {
		return Verdict{
			Decision: Wait,
			Reason:   "lease free but caller has no id to spawn",
		}, nil
	}
	reason := "lease free; spawning a fresh coord"
	if disp == coordlock.HandoffAbandoned {
		reason = "handoff abandoned (successor process dead); cleared stale journal, spawning fresh"
	}
	return Verdict{
		Decision: Spawn,
		Reason:   reason,
	}, nil
}
