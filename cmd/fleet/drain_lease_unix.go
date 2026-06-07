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
	// Resume runs the cold-spawn fallback (no lease epoch; spawn a successor
	// from the queue). Production: handoffop.Resume. It is run under a SHORT
	// per-agent LockAgent (Resume's contract requires it) — but only on the
	// COLD path, which spawns fresh and does NOT swap a hung live leader, so
	// the forever-hold class does not apply. The graceful/hung COORD path
	// never calls Resume; it kills/escalates lock-free.
	Resume func(req queue.SpawnFresh, queuePath string, graceMillis int, stdout, stderr io.Writer) error
	// LockAgent serializes the cold Resume against a concurrent drain so two
	// drains cannot both spawn a replacement (codex PR3 iter-4 [P1]).
	// Production: state.LockAgent. Returns a release func.
	LockAgent func(agentID string) (func(), error)
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
		LockAgent:   state.LockAgent,
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
		// Defensive: drainOne only routes COORD handoffs here (they always
		// carry a project), but if a projectless queue reaches this seam we
		// cannot stand-down/verify (the lease machinery is per-project). Fall
		// back to a cold Resume under a SHORT per-agent lock (coldResume holds
		// LockAgent — Resume's serialization contract).
		return coldResume(req, path, graceMillis, resumeTimeoutMillis, stdout, stderr, d)
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
		// A healthy heartbeating leader holds the lease and no completion
		// barrier exists yet. Two sub-cases (codex PR3 iter-5 [P1]):
		//   - OLD already archived -> the handoff already completed; the queue
		//     file is a stale leftover. STAND DOWN (clean the queue, spawn
		//     nothing). This is the true "nothing to drain" case (T13).
		//   - OLD still live -> a handoff was requested (the queue file exists)
		//     but the in-process GRACEFUL producer (GracefulHandoff) has not
		//     run to write the barrier. We must still COMPLETE the handoff
		//     rather than strand the queue forever: fall back to the LEGACY
		//     Resume path (the proven spawn+retire flow), under a short
		//     per-agent lock. (When the graceful producer is the trigger — the
		//     flag-flip is PR4 — the barrier case above handles it lock-free;
		//     until then this fallback keeps failover-on handoffs working.)
		return drainLiveLeaderFallback(req, path, graceMillis, resumeTimeoutMillis, stdout, stderr, d)

	case !hasEpoch:
		// COLD recovery: failover is on but there is NO lease epoch for this
		// project (no coord ever held the lease here, or the record is gone)
		// — the lease is stealable and nothing to fence/kill. Spawn the
		// successor via coldResume (codex PR3 iter-1 [P2]). coldResume holds a
		// SHORT per-agent lock for Resume's serialization contract (codex PR3
		// iter-4 [P1]) — safe here: a cold spawn has no hung live leader to
		// wedge on. This is the "stealable lease -> cold-spawn" path.
		_, _ = fmt.Fprintf(stderr, "fleet drain: no lease leader for %s; cold-spawning successor\n", project)
		return coldResume(req, path, graceMillis, resumeTimeoutMillis, stdout, stderr, d)

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

// drainLiveLeaderFallback handles a coord handoff queue file while a healthy
// leader still holds the lease and no completion barrier exists. If OLD is
// already archived the handoff completed and the queue is a stale leftover
// (stand down, clean it). Otherwise the graceful producer has not written a
// barrier yet, so complete the handoff via the LEGACY Resume path under a
// short per-agent lock (codex PR3 iter-5 [P1]) rather than stranding the
// queue. coldResume provides the locked Resume.
func drainLiveLeaderFallback(req queue.SpawnFresh, path string, graceMillis, resumeTimeoutMillis int,
	stdout, stderr io.Writer, d drainLeaseDeps) error {

	_, err := d.LoadAgent(req.OldAgentID)
	if errors.Is(err, state.ErrNotFound) {
		// OLD already archived -> handoff already completed; stale queue.
		_, _ = fmt.Fprintf(stdout,
			"fleet drain: coord live for %s and %s already retired; cleaning stale queue\n",
			req.Project, req.OldAgentID)
		return queue.Delete(path)
	}
	// OLD still present (or load error) -> complete the handoff the proven
	// way. Delegate to the LEGACY drain (LockAgent + handoffop.Resume), which
	// is byte-identical to the flag-off path: it spawns + retires the
	// successor exactly as production does today. The successor is therefore
	// NOT lease-wrapped here — that is the same PR2-documented gap that exists
	// flag-off, NOT a regression this PR introduces. The lease-correct
	// successor transfer is the warm-standby flow (GracefulHandoff spawns a
	// `coord-run --standby` successor that polls + acquires after OLD exits);
	// its TRIGGER wiring + the flag flip are PR4. So under failover a live
	// coord handoff completes via the proven legacy flow until PR4 routes it
	// through the standby producer (codex PR3 iter-5 [P1]).
	// coldResume runs d.Resume (production: handoffop.Resume) under d.LockAgent
	// (production: state.LockAgent) — byte-equivalent to drainOneLegacy.
	_, _ = fmt.Fprintf(stderr,
		"fleet drain: coord handoff pending for %s with no barrier yet; completing via legacy resume\n",
		req.Project)
	return coldResume(req, path, graceMillis, resumeTimeoutMillis, stdout, stderr, d)
}

// coldResume runs handoffop.Resume to spawn a successor for a STEALABLE
// (no-epoch) coord lease. It holds a SHORT per-agent LockAgent for the
// duration (codex PR3 iter-4 [P1]) — Resume's contract requires that lock,
// and two concurrent drains would otherwise both spawn a replacement. This
// does NOT reintroduce the 81-leak: that leak was holding the lock across a
// HUNG live leader's coord swap (tmux probes / nested flock / spawn that can
// wedge forever). A COLD spawn has no live leader to swap and no hung holder
// — it is the fresh first-spawn path, where the per-agent lock is brief and
// always released. The graceful/hung COORD path never calls Resume at all; it
// kills/escalates lock-free (that is where the T40 no-lock invariant lives).
//
// Run SYNCHRONOUSLY: `fleet drain` is a short-lived CLI, so an abandoned
// Resume goroutine would be killed on process exit mid-spawn (codex PR3
// iter-3 [P2]).
func coldResume(req queue.SpawnFresh, path string, graceMillis, resumeTimeoutMillis int,
	stdout, stderr io.Writer, d drainLeaseDeps) error {

	_ = resumeTimeoutMillis // the cold spawn is not abandoned on a timer
	release, err := d.LockAgent(req.OldAgentID)
	if err != nil {
		return fmt.Errorf("fleet drain: lock agent %s for cold resume: %w", req.OldAgentID, err)
	}
	defer release()
	return d.Resume(req, path, graceMillis, stdout, stderr)
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
	if d.LockAgent == nil {
		d.LockAgent = def.LockAgent
	}
	if d.BarrierPoll == 0 {
		d.BarrierPoll = def.BarrierPoll
	}
	if d.Self == nil {
		d.Self = def.Self
	}
	return d
}
