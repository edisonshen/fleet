//go:build linux || darwin

// drain_lease_unix.go — the BOUNDED, lease-aware drain path
// (DESIGN-handoff-drain-storm-leak §3(D), PR3). It replaces the legacy
// "hold the per-agent flock across the entire Resume" drain (the
// forever-held lock + 81-process leak) with a drain that:
//
//   - NEVER holds a lock across Resume / tmux / spawn / kill (the root-cause
//     line drain.go:101-106 is gone on this path);
//   - STANDS DOWN (exit 0) when a healthy leader holds the lease and no
//     handoff is pending (nothing to do);
//   - waits (BOUNDED) for the handoff-complete-<epoch>.json barrier before a
//     GRACEFUL kill, then reaps OLD through the ONE authenticated
//     coord.KillCoordIfIdentityMatches primitive (never an unguarded kill);
//   - on a slow/hung handoff (timeout) ESCALATES to the safety-net takeover
//     (coordlock.AcquireLeaseWithKill, which internally fences -> kills ->
//     acquires) instead of blocking — so a stuck handoff can never wedge
//     later drains.
//
//	┌──────────────────────────────────────────────────────────────────┐
//	│ drainOneLeaseAware(req)                                            │
//	│   leader healthy + NO barrier  -> STAND DOWN (exit 0)              │
//	│   barrier present (graceful)   -> KillCoordIfIdentityMatches(OLD)  │
//	│   no barrier, bounded wait     -> wait barrier OR timeout          │
//	│   timeout / hung leader        -> AcquireLeaseWithKill (TAKEOVER)  │
//	│   stealable lease (cold)       -> bounded Resume (NO lock held)    │
//	└──────────────────────────────────────────────────────────────────┘
//
// Build-tagged linux||darwin (the lease + STONITH primitives are). Other
// Unix targets compile drain_lease_other.go's stub (lease always "off").

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
	"github.com/edisonshen/fleet/internal/handoffop"
	"github.com/edisonshen/fleet/internal/queue"
	"github.com/edisonshen/fleet/internal/state"
)

// leaseDrainEnabled reports whether the lease-aware drain path is selected
// (FLEET_LEASE_FAILOVER on). Mirrors coordlock's internal gate.
func leaseDrainEnabled() bool {
	v := os.Getenv(coordlock.FailoverEnvVar)
	return v != "" && v != "0" && v != "false"
}

// drainLeaseDeps are the injectable seams that keep drainOneLeaseAware
// deterministic under test (no real lease reads, no real kill, no real
// Resume, no real clock). Production callers use defaultDrainLeaseDeps().
type drainLeaseDeps struct {
	// LeaderPresent reports whether a HEALTHY leader (or fresh in-progress
	// takeover) holds the lease for project. Production: coordlock.LeaderPresent.
	LeaderPresent func(project string) bool
	// CurrentEpoch returns the project's current lease epoch (the barrier's
	// generation). Production: coordlock.CurrentEpoch.
	CurrentEpoch func(project string) (int64, bool)
	// ActiveOwnerPID returns the pid of the current ACTIVE lease owner (and
	// ok=false if none). Used to tell "OLD is still the active leader,
	// finishing its graceful exit" (must NOT be force-killed — the epoch gate
	// refuses; wait for its self-release / escalate to takeover, which fences
	// first) apart from "OLD already releasing/stale" (safe to reap directly
	// through the authenticated primitive). Production:
	// coordlock.CurrentActiveOwnerPID.
	ActiveOwnerPID func(project string) (int, bool)
	// BarrierExists reports whether the handoff-complete-<epoch>.json barrier
	// for (project, epoch) is on disk. Production: a stat through
	// coordlock.BarrierPath.
	BarrierExists func(project string, epoch int64) bool
	// LoadAgent loads an agent record (for OLD's supervisor identity).
	// Production: agent.Load.
	LoadAgent func(id string) (*agent.Record, error)
	// KillCoord reaps OLD's coord-run supervisor through the authenticated
	// gate. Production: coord.KillCoordIfIdentityMatches.
	KillCoord func(coord.KillTarget) error
	// TakeOver escalates to the safety-net takeover (fence -> kill ->
	// acquire). Production: a closure over coordlock.AcquireLeaseWithKill +
	// the authenticated kill. Returns acquired + err. The drain does not
	// keep the lease (it is not the coord); it only needs the side effect of
	// the OLD holder being fenced/killed so a standby/successor can lead.
	TakeOver func(project, agentID string) (acquired bool, err error)
	// Resume runs the cold-spawn fallback (no live leader; spawn a successor
	// from the queue). Production: handoffop.Resume. Run BOUNDED + with NO
	// lock held across it.
	Resume func(req queue.SpawnFresh, queuePath string, graceMillis int, stdout, stderr io.Writer) error
	// BarrierPoll is the interval between barrier-existence checks while
	// waiting (bounded) for a graceful handoff to complete. 0 = default.
	BarrierPoll time.Duration
	// Self is the caller's pid (never target self in a kill). Production:
	// os.Getpid.
	Self func() int
}

// ErrEscalatedToTakeOver is declared in drain.go (the all-platform file) so
// runDrain can reference it on every GOOS. The lease-aware path here returns
// it when a slow/hung handoff is handed to the safety-net takeover.

const defaultBarrierPoll = 200 * time.Millisecond

func defaultDrainLeaseDeps() drainLeaseDeps {
	return drainLeaseDeps{
		LeaderPresent:  coordlock.LeaderPresent,
		CurrentEpoch:   coordlock.CurrentEpoch,
		ActiveOwnerPID: coordlock.CurrentActiveOwnerPID,
		BarrierExists: func(project string, epoch int64) bool {
			p, err := coordlock.BarrierPath(project, epoch)
			if err != nil {
				return false
			}
			_, statErr := os.Stat(p)
			return statErr == nil
		},
		LoadAgent: agent.Load,
		KillCoord: coord.KillCoordIfIdentityMatches,
		TakeOver: func(project, agentID string) (bool, error) {
			lease, acquired, err := coordlock.AcquireLeaseWithKill(project, agentID,
				func(t coordlock.KillTarget) error {
					return coord.KillCoordIfIdentityMatches(coord.KillTarget{
						Pid:         t.Pid,
						PidStart:    t.PidStart,
						AgentID:     t.AgentID,
						Project:     t.Project,
						FencerEpoch: t.FencerEpoch,
					})
				})
			// The drain is NOT the coordinator — if it accidentally acquired
			// the lease (the OLD holder was already gone), release it
			// immediately so a real standby/successor can lead. The point of
			// the escalation is to FENCE+KILL the hung OLD, not to make the
			// drain process the leader.
			if acquired && lease != nil {
				lease.Release()
			}
			return acquired, err
		},
		Resume:      handoffop.Resume,
		BarrierPoll: defaultBarrierPoll,
		Self:        os.Getpid,
	}
}

// drainOneLeaseAware is the production entry point for the bounded drain.
func drainOneLeaseAware(req queue.SpawnFresh, path string, graceMillis, resumeTimeoutMillis int,
	stdout, stderr io.Writer) error {

	return drainOneLeaseAwareWith(req, path, graceMillis, resumeTimeoutMillis,
		stdout, stderr, defaultDrainLeaseDeps())
}

// drainOneLeaseAwareWith is the seam-injected core (see drainLeaseDeps).
func drainOneLeaseAwareWith(req queue.SpawnFresh, path string, graceMillis, resumeTimeoutMillis int,
	stdout, stderr io.Writer, d drainLeaseDeps) error {

	d = fillDrainLeaseDeps(d)
	project := req.Project
	if project == "" {
		// No project scope (legacy worker queue without project) — fall back
		// to a BOUNDED cold Resume with NO lock held (the lease-aware path's
		// hard invariant). The lease machinery is per-project; without one we
		// cannot stand-down/verify, so just run the successor spawn bounded.
		return boundedResume(req, path, graceMillis, resumeTimeoutMillis, stdout, stderr, d)
	}

	timeout := time.Duration(resumeTimeoutMillis) * time.Millisecond
	deadline := time.Now().Add(timeout)

	epoch, hasEpoch := d.CurrentEpoch(project)
	barrierUp := hasEpoch && d.BarrierExists(project, epoch)
	leaderHealthy := d.LeaderPresent(project)

	switch {
	case barrierUp:
		// Graceful path: the OLD coord wrote the completion barrier — doc +
		// checkpoint are durable. How we finish depends on whether OLD is
		// STILL the active lease owner:
		//   - OLD still active owner -> it is responsive and exiting on its
		//     own; the authenticated kill's epoch gate would (correctly)
		//     refuse to shoot the active leader, so we WAIT (bounded) for OLD
		//     to release the flock — the standby then acquires it. On timeout
		//     -> escalate to takeover (which FENCES first, so its kill is no
		//     longer gated by "is the active owner"). codex PR3 iter-1 [P1].
		//   - OLD already releasing / not the active owner -> reap directly
		//     through the authenticated primitive (the gate passes).
		return drainGraceful(req, path, epoch, deadline, stdout, stderr, d)

	case leaderHealthy:
		// T13: a healthy heartbeating leader holds the lease and no graceful
		// handoff has completed (no barrier). Nothing to drain — STAND DOWN.
		// We do NOT kill a live leader, and we hold no lock. If the queue
		// file is a stale leftover from a completed handoff it is left in
		// place (a future drain after the leader exits handles it).
		_, _ = fmt.Fprintf(stdout, "fleet drain: coord live for %s; nothing to drain\n", project)
		return nil

	case !hasEpoch:
		// COLD recovery: failover is on but there is NO lease epoch for this
		// project (no coord ever held the lease here, or the record is gone)
		// — the lease is stealable and nothing to fence/kill. Spawn the
		// successor via a BOUNDED Resume with NO lock held (codex PR3 iter-1
		// [P2]). This is the "stealable lease -> cold-spawn" path.
		_, _ = fmt.Fprintf(stderr, "fleet drain: no lease leader for %s; cold-spawning successor (bounded)\n", project)
		return boundedResume(req, path, graceMillis, resumeTimeoutMillis, stdout, stderr, d)

	default:
		// An epoch exists but the leader is NOT healthy (hung past TTL /
		// stale) and NO barrier. Either OLD is mid-handoff (will write the
		// barrier soon) OR HUNG before the barrier (the incident). Wait
		// BOUNDED for the barrier; if it appears -> graceful finish; if the
		// deadline passes -> ESCALATE to the safety-net takeover (never block).
		return drainWaitBarrierOrEscalate(req, path, deadline, stdout, stderr, d)
	}
}

// drainGraceful finishes a graceful handoff once the completion barrier is
// up. If OLD is still the ACTIVE lease owner it is finishing its own exit;
// we wait (bounded) for it to release (the standby then acquires) and
// escalate to takeover on timeout (the takeover fences first, so its kill is
// not blocked by the active-owner gate). Otherwise OLD is already
// releasing/stale and we reap it directly through the authenticated
// primitive. Holds NO lock.
func drainGraceful(req queue.SpawnFresh, path string, epoch int64, deadline time.Time,
	stdout, stderr io.Writer, d drainLeaseDeps) error {

	oldRec, err := d.LoadAgent(req.OldAgentID)
	switch {
	case errors.Is(err, state.ErrNotFound):
		// OLD already archived — handoff fully complete. Clean the queue.
		_, _ = fmt.Fprintf(stdout, "fleet drain: %s already retired (barrier present); cleaning queue\n", req.OldAgentID)
		return queue.Delete(path)
	case err != nil:
		return fmt.Errorf("fleet drain: load old agent %s: %w", req.OldAgentID, err)
	}

	// If OLD is still the recorded ACTIVE lease owner, do NOT force-kill it
	// (the epoch gate refuses, correctly). Wait bounded for its self-release;
	// escalate to takeover on timeout.
	if ownerPid, ok := d.ActiveOwnerPID(req.Project); ok && ownerPid == oldRec.SupervisorPID {
		_, _ = fmt.Fprintf(stdout,
			"fleet drain: %s wrote the barrier and is still the active leader; waiting for its self-release\n",
			oldRec.ID)
		for {
			if op, ok := d.ActiveOwnerPID(req.Project); !ok || op != oldRec.SupervisorPID {
				// OLD released (or a takeover advanced past it). The standby
				// acquires the freed flock; the handoff is complete. Clean up.
				_, _ = fmt.Fprintf(stdout, "fleet drain: %s released the lease; handoff complete\n", oldRec.ID)
				return queue.Delete(path)
			}
			if !time.Now().Before(deadline) {
				break
			}
			wait := d.BarrierPoll
			if rem := time.Until(deadline); rem < wait {
				wait = rem
			}
			if wait > 0 {
				time.Sleep(wait)
			}
		}
		// OLD held the lease past the budget despite writing the barrier —
		// treat as hung; escalate to the safety-net takeover (fence -> kill ->
		// acquire). Never block.
		_, _ = fmt.Fprintf(stderr,
			"fleet drain: %s did not release the lease after writing the barrier within the budget; escalating to takeover\n",
			oldRec.ID)
		if _, terr := d.TakeOver(req.Project, req.OldAgentID); terr != nil {
			return fmt.Errorf("fleet drain: safety-net takeover for %s: %w", req.Project, terr)
		}
		return ErrEscalatedToTakeOver
	}

	// OLD is not the active owner (already releasing / stale record) — reap it
	// directly through the authenticated primitive.
	return drainReapOld(oldRec, req, path, epoch, stdout, stderr, d)
}

// drainReapOld reaps OLD's coord-run supervisor through the ONE
// authenticated kill primitive (NO unguarded kill). T42: a recycled PID is
// refused by the primitive (start-time mismatch).
func drainReapOld(oldRec *agent.Record, req queue.SpawnFresh, path string, epoch int64,
	stdout, stderr io.Writer, d drainLeaseDeps) error {

	if oldRec.SupervisorPID <= 0 {
		// No supervisor identity recorded — cannot authenticate a kill.
		// Surface-don't-silo: refuse rather than an unguarded kill.
		return fmt.Errorf(
			"fleet drain: old coord %s has no recorded supervisor identity; cannot reap safely (queue %s preserved)",
			req.OldAgentID, path)
	}
	if oldRec.SupervisorPID == d.Self() {
		return fmt.Errorf("fleet drain: refusing to reap self (pid %d)", oldRec.SupervisorPID)
	}

	if err := d.KillCoord(coord.KillTarget{
		Pid:         oldRec.SupervisorPID,
		PidStart:    oldRec.SupervisorPidStart,
		AgentID:     oldRec.ID,
		Project:     oldRec.Project,
		FencerEpoch: epoch,
	}); err != nil {
		// A refusal (T42 PID-reuse, exe-path, would-shoot-leader) is surfaced
		// — never silently dropped. The queue file is preserved so a later
		// pass can retry once the ambiguity clears.
		return fmt.Errorf("fleet drain: graceful reap of old coord %s refused/failed: %w (queue %s preserved)",
			oldRec.ID, err, path)
	}
	_, _ = fmt.Fprintf(stdout, "fleet drain: reaped old coord %s (graceful, barrier epoch %d)\n", oldRec.ID, epoch)
	// OLD is dead -> kernel released the flock -> the standby's next poll
	// acquires it. Clean the queue file.
	if err := queue.Delete(path); err != nil {
		return fmt.Errorf("fleet drain: reaped %s but queue delete failed: %w", oldRec.ID, err)
	}
	return nil
}

// drainWaitBarrierOrEscalate polls (bounded) for the barrier. On the barrier
// appearing -> graceful kill. On the deadline -> escalate to the safety-net
// takeover and return ErrEscalatedToTakeOver. NEVER blocks past the deadline;
// NEVER holds a lock. T25 (no graceful kill before barrier) + T41 (escalate
// on a hung OLD).
func drainWaitBarrierOrEscalate(req queue.SpawnFresh, path string, deadline time.Time,
	stdout, stderr io.Writer, d drainLeaseDeps) error {

	project := req.Project
	for {
		if epoch, ok := d.CurrentEpoch(project); ok && d.BarrierExists(project, epoch) {
			// Barrier appeared while we waited — finish the graceful path.
			return drainGraceful(req, path, epoch, deadline, stdout, stderr, d)
		}
		if !time.Now().Before(deadline) {
			break
		}
		// Sleep the smaller of the poll interval and the time left, so we
		// never overshoot the deadline.
		wait := d.BarrierPoll
		if rem := time.Until(deadline); rem < wait {
			wait = rem
		}
		if wait > 0 {
			time.Sleep(wait)
		}
	}

	// Deadline passed with no barrier — OLD is slow or HUNG before the
	// barrier. ESCALATE to the safety-net takeover (fence -> kill -> acquire),
	// which never blocks and holds no lock across slow work. The queue file is
	// left in place: after the takeover fences/kills OLD, a standby/successor
	// leads and a later drain (now seeing a healthy leader / completed state)
	// resolves the queue.
	_, _ = fmt.Fprintf(stderr,
		"fleet drain: handoff for %s did not reach the barrier within the budget; escalating to safety-net takeover\n",
		project)
	if _, err := d.TakeOver(project, req.OldAgentID); err != nil {
		return fmt.Errorf("fleet drain: safety-net takeover for %s: %w", project, err)
	}
	// Signal (not a hard error): we escalated cleanly. The drain did not
	// block and held no lock; the takeover handles recovery.
	return ErrEscalatedToTakeOver
}

// boundedResume runs handoffop.Resume with a wall-clock budget and NO lock
// held across it (the lease-aware path's hard invariant). On timeout it
// returns without waiting for Resume to finish — the goroutine running Resume
// is allowed to complete in the background (it holds no shared lock, so a
// slow one cannot wedge later drains). T12 + T40.
func boundedResume(req queue.SpawnFresh, path string, graceMillis, resumeTimeoutMillis int,
	stdout, stderr io.Writer, d drainLeaseDeps) error {

	timeout := time.Duration(resumeTimeoutMillis) * time.Millisecond
	done := make(chan error, 1)
	go func() {
		done <- d.Resume(req, path, graceMillis, stdout, stderr)
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		_, _ = fmt.Fprintf(stderr,
			"fleet drain: Resume for %s exceeded %dms budget; not blocking (no lock held)\n",
			req.OldAgentID, resumeTimeoutMillis)
		// For a project-scoped queue this would escalate to takeover; the
		// no-project fallback has no lease, so we simply stop waiting. The
		// background Resume holds no shared lock, so later drains are not
		// wedged — the structural fix for the 81-process leak.
		return ErrEscalatedToTakeOver
	}
}

func fillDrainLeaseDeps(d drainLeaseDeps) drainLeaseDeps {
	def := defaultDrainLeaseDeps()
	if d.LeaderPresent == nil {
		d.LeaderPresent = def.LeaderPresent
	}
	if d.CurrentEpoch == nil {
		d.CurrentEpoch = def.CurrentEpoch
	}
	if d.ActiveOwnerPID == nil {
		d.ActiveOwnerPID = def.ActiveOwnerPID
	}
	if d.BarrierExists == nil {
		d.BarrierExists = def.BarrierExists
	}
	if d.LoadAgent == nil {
		d.LoadAgent = def.LoadAgent
	}
	if d.KillCoord == nil {
		d.KillCoord = def.KillCoord
	}
	if d.TakeOver == nil {
		d.TakeOver = def.TakeOver
	}
	if d.Resume == nil {
		d.Resume = def.Resume
	}
	if d.BarrierPoll == 0 {
		d.BarrierPoll = def.BarrierPoll
	}
	if d.Self == nil {
		d.Self = def.Self
	}
	return d
}
