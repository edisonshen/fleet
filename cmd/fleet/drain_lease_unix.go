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
	"path/filepath"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/coord"
	"github.com/edisonshen/fleet/internal/coordlock"
	"github.com/edisonshen/fleet/internal/coordrepo"
	"github.com/edisonshen/fleet/internal/handoff"
	"github.com/edisonshen/fleet/internal/handoffop"
	"github.com/edisonshen/fleet/internal/queue"
	"github.com/edisonshen/fleet/internal/spawn"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tmux"
)

// leaseDrainEnabled reports whether the lease-aware drain path is selected.
// Delegates to coordlock's single source of truth (PR4 default ON;
// =0/false/off/no disables).
func leaseDrainEnabled() bool {
	return coordlock.FailoverEnabled()
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
	// Resume runs the cold-spawn fallback for a live-leader handoff that has no
	// barrier yet (the graceful producer hasn't run). Production: handoffop.Resume.
	// It is run under a SHORT per-agent LockAgent (Resume's contract requires
	// it) — but only when OLD is still the LIVE leader (no hung swap), so the
	// forever-hold class does not apply. The graceful/hung COORD path never
	// calls Resume; it kills/escalates lock-free.
	Resume func(req queue.SpawnFresh, queuePath string, graceMillis int, stdout, stderr io.Writer) error
	// LockAgent serializes the cold Resume against a concurrent drain so two
	// drains cannot both spawn a replacement (codex PR3 iter-4 [P1]).
	// Production: state.LockAgent. Returns a release func.
	LockAgent func(agentID string) (func(), error)
	// RecoverSpawn brings up a FRESH lease-wrapped successor AFTER a safety-net
	// takeover has fenced+killed a hung OLD (codex PR3 iter-7 [P1]). It must
	// spawn from the CACHED old record (captured BEFORE the takeover archived
	// it) as a dead-coord RECOVERY (spawn.Options{RecoverDeadCoord:true}) so
	// the successor is coord-run-wrapped and ACQUIRES + heartbeats the now-free
	// lease — NOT routed through handoffop.Resume (which loads OLD by id and
	// would race the takeover's archive + spawn an UNWRAPPED handoff
	// successor). preAllocatedID is the queue's pre-allocated successor id
	// (req.NewAgentID) so journal/doc/remote-control setup keyed to it still
	// correlates to the live replacement (codex PR3 iter-10 [P2]); empty means
	// generate a fresh id. nil OldRec means "could not cache OLD" -> the seam
	// surfaces a recovery instruction instead of spawning blind.
	RecoverSpawn func(oldRec *agent.Record, docPath, preAllocatedID string, stdout, stderr io.Writer) error
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
		Resume:       handoffop.Resume,
		LockAgent:    state.LockAgent,
		RecoverSpawn: productionRecoverSpawn,
		BarrierPoll:  defaultBarrierPoll,
		Self:         os.Getpid,
	}
}

// productionRecoverSpawn brings up a FRESH lease-wrapped successor after a
// takeover (codex PR3 iter-7 [P1]). It spawns from the CACHED old record as a
// dead-coord RECOVERY so the successor is coord-run-wrapped and acquires the
// now-free lease (RecoverDeadCoord=true). Coord cwd resolves through the shared
// project-repo resolver (same as the dispatch recovery path), never the old
// coord's stale stored Cwd. Surface-don't-silo on any gap: a missing record /
// unresolvable repo prints a concrete recovery command rather than spawning
// blind.
func productionRecoverSpawn(oldRec *agent.Record, docPath, preAllocatedID string, stdout, stderr io.Writer) error {
	if oldRec == nil {
		_, _ = fmt.Fprintf(stderr,
			"fleet drain: takeover reaped the hung coord but its record could not be cached; "+
				"run `fleet dispatch --coord-spawn` (or press [a] in the TUI) to bring up a replacement\n")
		return fmt.Errorf("fleet drain: recover-spawn: no cached old record")
	}
	if len(oldRec.Command) == 0 {
		return fmt.Errorf("fleet drain: recover-spawn: old coord %s has no stored command", oldRec.ID)
	}

	// ADOPT an already-spawned successor instead of re-spawning (codex PR4
	// [P1]/[P2]). A prior recovery pass may have spawned the replacement with
	// THIS preAllocatedID and then failed only at prompt delivery, preserving
	// the queue for retry. Re-running spawn with the same id would collide
	// with the live session. So: if the pre-allocated successor record exists
	// AND its tmux session is alive, skip the spawn and go straight to (idempotent)
	// prompt delivery. This makes a preserved-queue retry safe.
	if preAllocatedID != "" {
		if existing, lerr := agent.Load(preAllocatedID); lerr == nil && existing != nil {
			if alive, _ := tmux.SessionAlive(existing.TmuxSession); alive {
				_, _ = fmt.Fprintf(stdout,
					"fleet drain: adopting already-spawned successor %s for %s (no re-spawn)\n",
					existing.ID, oldRec.Project)
				return deliverRecoverResumePrompt(existing, docPath, oldRec.DisableAutoResume, stdout, stderr)
			}
		}
	}

	spawnCwd := oldRec.Cwd
	if spawn.IsCoordSpawn(oldRec.TaskID, oldRec.Project) {
		resolved, rerr := coordrepo.ResolveProjectRepo(oldRec.Project, true)
		if rerr != nil {
			return fmt.Errorf("fleet drain: recover-spawn: resolve repo for project %q: %w", oldRec.Project, rerr)
		}
		spawnCwd = resolved
	}
	rec, err := spawn.Spawn(spawn.Options{
		OldRecord:         oldRec,
		NewDocPath:        docPath,
		Cwd:               spawnCwd,
		Command:           oldRec.Command,
		DisableAutoResume: oldRec.DisableAutoResume,
		// Reuse the queue's pre-allocated successor id so any handoff
		// journal/doc/remote-control setup keyed to it correlates to the live
		// replacement (codex PR3 iter-10 [P2]). Empty -> spawn.Spawn allocates.
		PreAllocatedID: preAllocatedID,
		// RecoverDeadCoord: OLD is gone (the takeover reaped it) -> the
		// successor is a FRESH leader and MUST be lease-wrapped so it acquires
		// + heartbeats the now-free lease.
		RecoverDeadCoord: true,
		LeaderCheck:      coordLeaderCheck,
	})
	if err != nil {
		return fmt.Errorf("fleet drain: recover-spawn replacement for %s: %w", oldRec.ID, err)
	}
	_, _ = fmt.Fprintf(stdout,
		"fleet drain: cold-spawned lease-wrapped successor %s for %s after takeover\n", rec.ID, oldRec.Project)
	return deliverRecoverResumePrompt(rec, docPath, oldRec.DisableAutoResume, stdout, stderr)
}

// deliverRecoverResumePrompt delivers the resume prompt to a recovered
// successor so it READS the handoff doc and continues the in-flight work
// (codex PR4 [P1]). The safety-net takeover path bypasses handoffop.Resume/
// retireOldAgent, so without this the new coord would start blank.
//
// At-most-once + no-strand semantics (codex PR4 [P2]):
//
//   - AT-MOST-ONCE: a per-successor sentinel file
//     (.locks/resume-prompt-sent-<newID>) is written BEFORE the send. On an
//     adopt-retry (queue.Delete failed after a prior successful send) the
//     sentinel short-circuits, so the same doc prompt is never re-submitted.
//     This is the at-most-once marker the other handoff paths get from
//     queue.Delete ordering; the recovery path can't reorder around
//     queue.Delete (it runs in the caller), so it carries its own marker.
//   - NO-STRAND on readiness timeout: WaitForReadyToPrompt failing does NOT
//     hard-fail. If the tmux session is still alive, we WARN and send anyway
//     (keys buffer in the pty until Claude is ready) — matching
//     retireOldAgent. Only a DEAD session returns an error (queue preserved).
//
// Skips silently when the doc is empty or auto-resume is disabled (a
// non-claude wrapper must not receive "Read your handoff doc..." as garbage).
func deliverRecoverResumePrompt(rec *agent.Record, docPath string, disableAutoResume bool,
	stdout, stderr io.Writer) error {
	if docPath == "" || disableAutoResume {
		return nil
	}
	sentinel, serr := resumePromptSentinelPath(rec.Project, rec.ID)
	if serr == nil {
		if _, statErr := os.Stat(sentinel); statErr == nil {
			// Already delivered on a prior pass; do not re-send (at-most-once).
			return nil
		}
	}
	if werr := spawn.WaitForReadyToPrompt(rec.TmuxSession); werr != nil {
		// Readiness poll didn't converge. If the session is DEAD, fail so the
		// queue is preserved. If it is ALIVE (slow startup / onboarding
		// screen / custom wrapper), warn and send anyway — the keys buffer.
		alive, probeErr := tmux.SessionAlive(rec.TmuxSession)
		if !alive && probeErr == nil {
			return fmt.Errorf(
				"fleet drain: recovered successor %s session %s is dead; cannot deliver resume prompt "+
					"(queue preserved for retry; doc: %s)", rec.ID, rec.TmuxSession, docPath)
		}
		_, _ = fmt.Fprintf(stderr,
			"fleet drain: recovered successor %s not visibly ready (%v) but session alive; sending resume prompt anyway\n",
			rec.ID, werr)
	}
	if serr := spawn.SendPromptKeys(rec.TmuxSession, handoff.ResumePrompt(docPath)); serr != nil {
		return fmt.Errorf(
			"fleet drain: resume prompt to recovered successor %s failed: %w "+
				"(queue preserved for retry; doc: %s)", rec.ID, serr, docPath)
	}
	// Mark AFTER a SUCCESSFUL send (codex PR4 [P2]). The sentinel must mean
	// "definitely delivered" — writing it before the send would, on a crash
	// between the marker and the send, make the retry skip delivery and
	// strand the successor (a hard failure). The opposite risk — a crash
	// between send and queue.Delete re-sending on retry — is a benign,
	// effectively-idempotent duplicate ("read your doc and continue"). Prefer
	// at-least-once delivery over a silent strand.
	if sentinel != "" {
		_ = os.WriteFile(sentinel, []byte(docPath+"\n"), 0o644)
	}
	_, _ = fmt.Fprintf(stdout, "fleet drain: delivered resume prompt to recovered successor %s\n", rec.ID)
	return nil
}

// resumePromptSentinelPath returns the at-most-once marker path for a
// recovered successor's resume prompt, under the project's .locks/ dir.
func resumePromptSentinelPath(project, newID string) (string, error) {
	pdir, err := state.ProjectDir(project)
	if err != nil {
		return "", err
	}
	return filepath.Join(pdir, ".locks", "resume-prompt-sent-"+newID), nil
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
		//   - OLD still live -> a handoff was requested (the queue file exists).
		//     The in-process GRACEFUL producer (GracefulHandoff) may have
		//     spawned a standby and be about to write the barrier, so we POLL
		//     (bounded) for the barrier FIRST (codex PR3 iter-12 [P2]) — racing
		//     straight to legacy Resume here would spawn a SECOND successor
		//     while the standby is already polling. Only if no barrier appears
		//     within the budget (no graceful producer ran) do we fall back to
		//     the proven legacy Resume to complete the handoff.
		return drainLiveLeaderFallback(req, path, deadline, graceMillis, resumeTimeoutMillis, stdout, stderr, d)

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
		return drainWaitBarrierOrEscalate(req, path, deadline, graceMillis, resumeTimeoutMillis, stdout, stderr, d)
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
		// treat as hung; escalate to the safety-net takeover + lease-wrapped
		// recovery (checks the takeover actually fenced+killed OLD before
		// spawning a successor — codex PR3 iter-9 [P1]). Never block.
		_, _ = fmt.Fprintf(stderr,
			"fleet drain: %s did not release the lease after writing the barrier within the budget; escalating to takeover\n",
			oldRec.ID)
		return takeoverAndRecover(req, path, oldRec, stdout, stderr, d)
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
// appearing -> graceful finish. On the deadline -> escalate to the safety-net
// takeover (fence -> kill the hung OLD), THEN cold-spawn a successor so the
// project is not left coordless (codex PR3 iter-6 [P1]). NEVER blocks past the
// deadline; NEVER holds a lock across the kill. T25 (no graceful kill before
// barrier) + T41 (escalate on a hung OLD).
func drainWaitBarrierOrEscalate(req queue.SpawnFresh, path string, deadline time.Time,
	graceMillis, resumeTimeoutMillis int, stdout, stderr io.Writer, d drainLeaseDeps) error {

	project := req.Project
	// Cache OLD's record BEFORE the takeover (codex PR3 iter-7 [P1]): the
	// takeover kills OLD's coord-run, whose cleanup archives the record, so a
	// post-takeover load would race the archive. The cached record is what the
	// lease-wrapped recovery spawns from. A load failure here is non-fatal: we
	// still escalate (reap the hung OLD); RecoverSpawn surfaces a recovery
	// instruction when the record is missing.
	var cachedOld *agent.Record
	if rec, lerr := d.LoadAgent(req.OldAgentID); lerr == nil {
		cachedOld = rec
	}
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
	// barrier, and no graceful standby was ever spawned (no producer ran). The
	// takeover (fence -> kill -> [drain releases]) reaps the hung OLD but does
	// NOT itself bring up a replacement, so we must cold-spawn the successor
	// after it — otherwise the project is left coordless after the kill
	// (codex PR3 iter-6 [P1]).
	_, _ = fmt.Fprintf(stderr,
		"fleet drain: handoff for %s did not reach the barrier within the budget; escalating to safety-net takeover\n",
		project)
	return takeoverAndRecover(req, path, cachedOld, stdout, stderr, d)
}

// takeoverAndRecover runs the safety-net takeover (fence -> kill the hung OLD)
// and then brings up a FRESH lease-wrapped successor from the CACHED old record
// (dead-coord recovery), deleting the queue on success. It NEVER blocks across
// the kill, but it DOES hold a SHORT per-agent lock across the
// takeover->recover sequence (codex PR3 iter-11 [P1]): the production TakeOver
// releases the lease as soon as OLD is proven gone, opening a window where a
// SECOND concurrent drain could acquire the freed lease and ALSO RecoverSpawn —
// a duplicate successor. The per-agent LockAgent serializes the whole sequence
// (it is BOUNDED — state.acquireBoundedAt — so a contending drain times out
// instead of hanging, then re-reads state and stands down). Shared by the
// hung-leader, graceful-past-budget, and Resume-timeout escalations. Returns
// ErrEscalatedToTakeOver on success (a non-fatal processed outcome).
func takeoverAndRecover(req queue.SpawnFresh, path string, cachedOld *agent.Record,
	stdout, stderr io.Writer, d drainLeaseDeps) error {

	project := req.Project
	// Serialize the takeover->recover sequence against a concurrent drain so
	// only ONE successor is recovered (codex PR3 iter-11 [P1]).
	release, lerr := d.LockAgent(req.OldAgentID)
	if lerr != nil {
		return fmt.Errorf("fleet drain: lock agent %s for takeover recovery: %w", req.OldAgentID, lerr)
	}
	defer release()

	// Re-check under the lock: a drain that won the race may have already
	// recovered (queue gone / a successor now leads). If the queue file is gone
	// the work is done — stand down (another drain handled it).
	if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) {
		_, _ = fmt.Fprintf(stdout, "fleet drain: %s already recovered by a concurrent drain; nothing to do\n", project)
		return nil
	}

	acquired, err := d.TakeOver(project, req.OldAgentID)
	if err != nil {
		return fmt.Errorf("fleet drain: safety-net takeover for %s: %w", project, err)
	}
	// acquired==false means the takeover did NOT confirm OLD is gone (codex PR3
	// iter-9 [P1]): the production seam (AcquireLeaseWithKill) only fences+kills
	// a HUNG holder — a HEALTHY holder makes it stand down (acquired=false,
	// err=nil), and a hung holder it could not authenticate/kill also yields
	// acquired=false. In neither case may we RecoverSpawn: a healthy OLD is
	// still leading (a successor would duplicate) and an un-killable OLD must
	// not be shot over. Surface the incomplete escalation; leave the queue so a
	// later drain retries once OLD is provably gone (or `fleet doctor` recovers
	// the un-killable case — PR6).
	if !acquired {
		_, _ = fmt.Fprintf(stderr,
			"fleet drain: takeover for %s did not acquire the lease (old coord still leading or un-killable); "+
				"NOT spawning a successor (would duplicate); leaving queue for retry\n", project)
		return fmt.Errorf("fleet drain: takeover for %s did not confirm the old coord is gone", project)
	}
	// The hung OLD is fenced + killed (drain acquired the freed flock, proving
	// OLD released, then released it for the successor). Bring up a
	// FRESH lease-wrapped successor from the CACHED old record (dead-coord
	// recovery — RecoverDeadCoord) so it acquires + heartbeats the now-free
	// lease (codex PR3 iter-7 [P1]). We do NOT route through handoffop.Resume:
	// that loads OLD by id (racing the archive) and would spawn an UNWRAPPED
	// handoff successor that never acquires the lease.
	_, _ = fmt.Fprintf(stderr,
		"fleet drain: takeover fenced/killed the hung coord for %s; recovering a lease-wrapped successor\n", project)
	if err := d.RecoverSpawn(cachedOld, req.HandoffDoc, req.NewAgentID, stdout, stderr); err != nil {
		// Surface the recovery-spawn failure but keep the escalation semantics:
		// the takeover succeeded (OLD reaped). Leave the queue in place so the
		// dead-coord recovery path (next dispatch / TUI [a]) retries against
		// the now-stealable lease.
		return fmt.Errorf("fleet drain: takeover for %s succeeded but recover-spawn failed: %w", project, err)
	}
	// Successor recovered after a clean takeover. The queue file's handoff
	// request is fulfilled by the recovery spawn; delete it so a later drain
	// doesn't re-escalate. A delete FAILURE is surfaced as an error (codex PR3
	// iter-11 [P2]): if we returned ErrEscalatedToTakeOver, runDrain would
	// count this processed while the queue lingers, and a later drain would
	// re-escalate / re-spawn for an already-completed handoff.
	if derr := queue.Delete(path); derr != nil {
		return fmt.Errorf(
			"fleet drain: recovered %s after takeover but queue delete failed (%w); rerun fleet drain to clean it",
			project, derr)
	}
	return ErrEscalatedToTakeOver
}

// drainLiveLeaderFallback handles a coord handoff queue file while a healthy
// leader still holds the lease and no completion barrier exists. If OLD is
// already archived the handoff completed and the queue is a stale leftover
// (stand down, clean it). Otherwise the graceful producer has not written a
// barrier yet, so complete the handoff via the LEGACY Resume path under a
// short per-agent lock (codex PR3 iter-5 [P1]) rather than stranding the
// queue. coldResume provides the locked Resume.
func drainLiveLeaderFallback(req queue.SpawnFresh, path string, deadline time.Time,
	graceMillis, resumeTimeoutMillis int, stdout, stderr io.Writer, d drainLeaseDeps) error {

	oldRec, err := d.LoadAgent(req.OldAgentID)
	if errors.Is(err, state.ErrNotFound) {
		// OLD already archived -> handoff already completed; stale queue.
		_, _ = fmt.Fprintf(stdout,
			"fleet drain: coord live for %s and %s already retired; cleaning stale queue\n",
			req.Project, req.OldAgentID)
		return queue.Delete(path)
	}

	// POLL (bounded) for the graceful barrier before falling back (codex PR3
	// iter-12 [P2]): a GracefulHandoff producer may have spawned a standby and
	// be about to write handoff-complete-<epoch>.json. If it appears, finish the
	// graceful path (which never spawns a second successor). Only on timeout do
	// we fall back to legacy Resume.
	for time.Now().Before(deadline) {
		if epoch, ok := d.CurrentEpoch(req.Project); ok && d.BarrierExists(req.Project, epoch) {
			return drainGraceful(req, path, epoch, deadline, stdout, stderr, d)
		}
		// Also bail early if a successor already took over (active owner != OLD).
		if oldRec != nil && oldRec.SupervisorPID > 0 {
			if op, ok := d.ActiveOwnerPID(req.Project); ok && op != oldRec.SupervisorPID {
				break
			}
		}
		wait := d.BarrierPoll
		if rem := time.Until(deadline); rem < wait {
			wait = rem
		}
		if wait > 0 {
			time.Sleep(wait)
		}
	}
	// A SUCCESSOR already leads (codex PR3 iter-6 [P2]): if the current active
	// lease owner is NOT OLD's supervisor, a graceful standby has already
	// acquired the lease (the barrier was written at OLD's now-superseded
	// epoch, so the current-epoch barrier check above missed it). OLD's record
	// just hasn't been archived yet. The handoff IS complete — clean the
	// stale queue, do NOT run legacy Resume (which would spawn a SECOND
	// replacement for an already-done handoff).
	if err == nil && oldRec != nil && oldRec.SupervisorPID > 0 {
		if ownerPid, ok := d.ActiveOwnerPID(req.Project); ok && ownerPid != oldRec.SupervisorPID {
			_, _ = fmt.Fprintf(stdout,
				"fleet drain: a successor already leads %s (active owner pid %d != old %d); handoff complete, cleaning queue\n",
				req.Project, ownerPid, oldRec.SupervisorPID)
			return queue.Delete(path)
		}
	}
	// OLD still present and still the leader -> complete the handoff the proven
	// way. Delegate to the LEGACY drain (LockAgent + handoffop.Resume), which
	// is byte-identical to the flag-off path: it spawns + retires the
	// successor exactly as production does today.
	//
	// SCOPE NOTE (codex PR4 [P1], accepted limitation): the successor spawned
	// here is NOT lease-wrapped — it is the legacy `claude` shape. Under the
	// PR4 flag flip (failover default ON) that means a live `fleet handoff`
	// completed via THIS fallback yields a successor that coordinates but does
	// not hold/heartbeat the coordinator lease. This is SAFE, not a
	// correctness break: the successor's `/coordinator` tick reads "no healthy
	// active owner" (OLD released the lease on retire) and PROCEEDS (the
	// lease-check's no-lease-in-play branch) — identical to flag-off behavior.
	// What is lost is lease HA (heartbeat + STONITH) on that one successor
	// until it itself hands off through the warm-standby path. The
	// lease-PRESERVING transfer is GracefulHandoff (the OLD coord spawns its
	// own `coord-run --standby` successor that polls + acquires + receives the
	// resume prompt post-acquire after OLD exits). Wiring GracefulHandoff's
	// TRIGGER into the producer — and the standby's post-acquire prompt
	// delivery — is the remaining PR3-completion follow-up, deliberately NOT
	// bolted onto spawnAndRetire here (a standby successor cannot receive the
	// resume prompt through the spawn-then-retire send path, so doing so would
	// strand the successor idle — a worse failure than the bare-but-functional
	// successor). Tracked as a follow-up; this fallback stays the proven
	// legacy completion so no live handoff regresses on activation.
	//
	// coldResume runs d.Resume (production: handoffop.Resume) under d.LockAgent
	// (production: state.LockAgent) — byte-equivalent to drainOneLegacy.
	_, _ = fmt.Fprintf(stderr,
		"fleet drain: coord handoff pending for %s with no barrier yet; completing via legacy resume\n",
		req.Project)
	return coldResume(req, path, graceMillis, resumeTimeoutMillis, stdout, stderr, d)
}

// coldResume runs handoffop.Resume to complete a coord handoff (the live-leader
// fallback) or cold-spawn a successor (stealable no-epoch lease). It holds a
// SHORT per-agent LockAgent (Resume's contract; two concurrent drains must not
// both spawn).
//
// It runs SYNCHRONOUSLY (codex PR3 iter-3/9 [P1/P2]): `fleet drain` is a
// short-lived CLI, so an abandoned Resume goroutine would either be killed
// mid-spawn on process exit OR race a safety-net recovery to a duplicate /
// inconsistent handoff. Bounding is NOT needed here to prevent the drain-storm:
//   - the per-agent LockAgent is BOUNDED (state.acquireBoundedAt never uses a
//     bare LOCK_EX), so a contending drain times out instead of forever-blocking
//     — the 81-leak's forever-block class is already gone at the lock layer;
//   - the genuinely-HUNG-leader case never reaches coldResume: it is detected
//     up front (LeaderPresent==false / no healthy holder) and handled by
//     drainWaitBarrierOrEscalate's fence->kill->recover, which holds no lock.
//
// So coldResume is exactly the proven legacy completion (LockAgent + Resume),
// reached only when a HEALTHY leader is present (a real, responsive handoff) or
// a stealable cold lease — neither of which is the forever-hang incident.
func coldResume(req queue.SpawnFresh, path string, graceMillis, resumeTimeoutMillis int,
	stdout, stderr io.Writer, d drainLeaseDeps) error {

	_ = resumeTimeoutMillis // synchronous: see doc comment (no abandonable goroutine)
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
	if d.RecoverSpawn == nil {
		d.RecoverSpawn = def.RecoverSpawn
	}
	if d.BarrierPoll == 0 {
		d.BarrierPoll = def.BarrierPoll
	}
	if d.Self == nil {
		d.Self = def.Self
	}
	return d
}
