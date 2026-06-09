//go:build linux || darwin

package coordlock

// diagnose.go — the read-only lease accessor `fleet doctor` (PR6 of
// DESIGN-handoff-drain-storm-leak) uses to inspect a project's coordinator
// lease WITHOUT mutating anything. It reuses the SAME staleness/health math
// the acquire path uses (holderHealthy / readEpoch) so the doctor's verdict
// can never drift from what AcquireLease would actually do — the design's
// "don't reinvent the staleness math" rule.
//
// The doctor command lives in package main (cmd/fleet) and cannot reach the
// unexported epochRecord / holderHealthy. Rather than export those internals
// (and the whole two-phase state machine), this exposes ONE typed snapshot:
// the facts the doctor renders + the SINGLE classification (LeaseHealth)
// that drives its plain-English message and its --fix decision.

import (
	"errors"
	"os"
)

// LeaseHealth is the doctor's single-verdict classification of a project's
// coordinator lease. It is derived from the SAME predicate the acquire path
// applies (holderHealthy), so a doctor that says "healthy" agrees with an
// AcquireLease that would stand down, and a doctor that says "hung" agrees
// with one that would take over.
type LeaseHealth int

const (
	// LeaseHealthNone — no lease record at all (fresh project, or the
	// failover flag is off so no lease is in play). Nothing to recover.
	LeaseHealthNone LeaseHealth = iota
	// LeaseHealthOK — a live leader holds the lease and is heartbeating
	// within TTL. --fix MUST refuse to clear it (invariant: never steal a
	// live, heartbeating holder).
	LeaseHealthOK
	// LeaseHealthHung — the recorded owner's pid is alive but its
	// heartbeat (renewed_at) is frozen past the TTL (the incident this
	// whole stack exists to detect). Stealable via fence->kill->acquire.
	LeaseHealthHung
	// LeaseHealthDead — the recorded owner's pid is gone (or its pid_start
	// no longer matches — PID reuse). The lease is stale and stealable.
	LeaseHealthDead
	// LeaseHealthFencedNotAcquired — the typed escalation state: a takeover
	// fenced the old epoch (bumped it) but could not kill/acquire the
	// flock, so no live leader holds it. Surfaced as its own diagnosis;
	// --fix offers operator-confirmed recovery (never a silent stall, never
	// a second leader).
	LeaseHealthFencedNotAcquired
	// LeaseHealthReleased — the holder cleanly released; no live leader.
	LeaseHealthReleased
)

// LeaseDiagnosis is the read-only snapshot of a project's coordinator lease.
// All fields are best-effort reads — a torn/absent record degrades to
// Health=LeaseHealthNone, never a panic. It takes NO lock (read-only).
type LeaseDiagnosis struct {
	// Health is the single classification that drives the doctor's message
	// and its --fix decision.
	Health LeaseHealth
	// HasRecord reports whether an epoch record exists at all (vs. absent).
	HasRecord bool
	// Epoch is the current on-disk fencing epoch (0 when no record).
	Epoch int64
	// State is the raw on-disk state string (active/fencing/
	// fenced_not_acquired/released) for --verbose / engineer logs.
	State string
	// OwnerPID is the recorded lease owner's supervisor pid (0 when none).
	OwnerPID int
	// OwnerPidStart is the owner's start-time fingerprint (PID-reuse guard),
	// needed by the doctor's --fix to authenticate a STONITH target.
	OwnerPidStart int64
	// OwnerAgentID is the recorded owner's agent id (for the kill target +
	// the respawn-from-record path).
	OwnerAgentID string
	// OwnerAlive reports whether the recorded owner pid+pid_start is a live
	// process (PID-reuse-safe). Distinguishes Hung (alive but frozen) from
	// Dead (gone).
	OwnerAlive bool
}

// Diagnose returns a read-only snapshot of the coordinator lease for
// project, classifying its health with the SAME math AcquireLease uses. It
// mutates NOTHING and takes no lock; a missing/torn epoch record degrades to
// Health=LeaseHealthNone. It is the doctor command's single inspection
// entry point (PR6). When the failover flag is explicitly off there is no
// lease in play, so it reports LeaseHealthNone.
func Diagnose(project string) LeaseDiagnosis {
	return diagnoseWithCfg(project, defaultLeaseConfig())
}

// diagnoseWithCfg is the seam-injected core so tests drive a deterministic
// clock / pid-liveness / boot id (no real sysctl, no time.Sleep). Mirrors
// leaderPresentWithCfg's throwaway-lease pattern: a zero-self Lease carries
// the seams so holderHealthy applies the identical predicate the acquire
// path does.
func diagnoseWithCfg(project string, cfg leaseConfig) LeaseDiagnosis {
	if !failoverEnabled() {
		// Reversibility: flag explicitly off -> no lease in play.
		return LeaseDiagnosis{Health: LeaseHealthNone}
	}
	paths, err := resolvePaths(project)
	if err != nil {
		return LeaseDiagnosis{Health: LeaseHealthNone}
	}
	l := &Lease{cfg: cfg, paths: paths, boot: cfg.boot()}

	rec, err := readEpoch(paths.epoch)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			// Torn/unreadable (not merely absent) -> conservatively "no record"
			// so the doctor surfaces nothing to recover rather than acting on
			// garbage.
			return LeaseDiagnosis{Health: LeaseHealthNone}
		}
		// NO epoch record. Mirror the acquire path's busy-flock booting check
		// (codex PR6 iter-4 [P2]): a holder can grab coordinator.flock and HANG
		// before writing coordinator.epoch (the acquire-to-epoch window). That
		// state is recoverable via the flock body, so Diagnose must NOT report
		// LeaseHealthNone and leave the stuck holder in place.
		f, gotFlock, ferr := tryFlock(paths.flock)
		if ferr != nil {
			return LeaseDiagnosis{Health: LeaseHealthNone}
		}
		if gotFlock {
			// We acquired it -> nobody holds it -> truly no leader.
			_ = releaseFlock(f)
			return LeaseDiagnosis{Health: LeaseHealthNone}
		}
		// Busy flock, no epoch: a fresh same-boot live holder is legitimately
		// booting (None); a stale/dead/cross-boot/past-TTL body is a holder
		// hung in the acquire window -> Hung (recoverable).
		if l.flockHolderRecoverable() {
			return LeaseDiagnosis{Health: LeaseHealthHung}
		}
		return LeaseDiagnosis{Health: LeaseHealthNone}
	}

	d := LeaseDiagnosis{
		HasRecord:     true,
		Epoch:         rec.Epoch,
		State:         rec.State,
		OwnerPID:      rec.Owner.Pid,
		OwnerPidStart: rec.Owner.PidStart,
		OwnerAgentID:  rec.Owner.AgentID,
		OwnerAlive:    l.pidAlive(rec.Owner),
	}

	switch rec.State {
	case stateActive:
		if l.holderHealthy(rec) {
			d.Health = LeaseHealthOK
			return d
		}
		// Active record but NOT healthy: either the owner pid is gone
		// (Dead) or it is alive with a frozen heartbeat / cross-boot stamp
		// (Hung). pidAlive is the discriminator the acquire path uses.
		if d.OwnerAlive {
			d.Health = LeaseHealthHung
		} else {
			d.Health = LeaseHealthDead
		}
	case stateFencedNotAcquired:
		d.Health = LeaseHealthFencedNotAcquired
	case stateReleased:
		d.Health = LeaseHealthReleased
	case stateFencing:
		// A fencing record is mid-takeover. If a fresh, live, in-budget
		// candidate is driving it, a successor is coming (treat as OK so the
		// doctor doesn't barge a healthy takeover); otherwise it is an
		// abandoned/stalled takeover the doctor should surface as recoverable
		// (same class as fenced_not_acquired for the operator's purposes).
		if l.transientResumable(rec) {
			d.Health = LeaseHealthFencedNotAcquired
		} else {
			d.Health = LeaseHealthOK
		}
	default:
		d.Health = LeaseHealthNone
	}
	return d
}
