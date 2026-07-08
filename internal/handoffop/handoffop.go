// Package handoffop holds the queue-driven handoff completion path used
// by both `fleet drain` (consumer of fleet-guard's auto-handoff queue
// files) and the TUI's queue fsnotify watcher.
//
// The operator-triggered `fleet handoff` path stays in cmd/fleet/handoff.go
// for now — that flow's 13 numbered steps are well-tested and refactoring
// them in the same PR as the new auto-handoff producer would conflate
// changes. A future PR can fold its body into Run() here without touching
// behavior.
//
// Resume is the single entry point. Given a queue file (already written
// by a producer — the skill on auto-handoff, the crashed handoff retry
// path on operator-triggered), it runs the recovery probe, spawns the
// replacement if needed, and retires the old agent. Caller holds the
// per-agent flock.
package handoffop

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/coordlock"
	"github.com/edisonshen/fleet/internal/coordrepo"
	"github.com/edisonshen/fleet/internal/handoff"
	"github.com/edisonshen/fleet/internal/handoffdelivery"
	"github.com/edisonshen/fleet/internal/queue"
	"github.com/edisonshen/fleet/internal/rc"
	"github.com/edisonshen/fleet/internal/spawn"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tmux"
)

// DefaultGraceMillis is the default delay between /exit and Kill. Matches
// cmd/fleet/handoff.go's default. Drain uses this directly; the TUI [h]
// handoff path may override.
const DefaultGraceMillis = 3000

// sessionListProbe is the tmux-session enumerator the auto-drain path
// uses for the FLEET_MAX_SESSIONS precheck. Indirected via a package-
// level var so tests inject a fake without touching tmux.
var sessionListProbe = tmux.ListSessions

// sessionAliveProbe is the tmux session-liveness probe the cap path
// uses to decide swap-vs-+1 accounting. Tristate: (alive, err) per
// tmux.SessionAlive's contract. Indirected via a package-level var
// so tests inject deterministic fakes.
var sessionAliveProbe = tmux.SessionAlive

var (
	handoffDeliveryDepsFn  = handoffdelivery.DefaultDeps
	handoffDeliveryTimeout = 30 * time.Second
	handoffDeliveryPoll    = 100 * time.Millisecond
	sendPromptKeysVerified = spawn.SendPromptKeysVerified
)

// gracefulSwapEligibleFn / gracefulCoordSwapFn are the PR3 graceful-route
// seams (DESIGN-coord-spawn-unified-standby §6): the live-coord swap routes
// through the GracefulHandoff barrier instead of the bespoke
// AtomicCoordSwap-then-deliver sequence. Package vars so tests inject fakes
// without a real lease.
var (
	gracefulSwapEligibleFn = GracefulSwapEligible
	gracefulCoordSwapFn    = GracefulCoordSwap
)

// liveCoordGracefulSpawn reports whether a coord successor on the drain/auto
// path should spawn LEASE-WRAPPED (coord-run --standby) because the handoff
// will complete through the GracefulHandoff barrier: the winner heartbeats
// the lease, so it never lapses (a BARE successor's lease did — the fd4ee2e6
// duplicate-coord incident). Dead-coord recovery (OLD not the live lease
// owner) keeps the bare cold-resume shape: a standby polling a dead owner's
// lease has no live releaser to wait on.
// Gates on the SAME lease predicate retireOldAgent's isCoordSwap uses (codex
// PR3-completion iter-6 [P2]): gracefulSwapEligibleFn already requires OLD to be
// the current lease owner (coordlock.CurrentOwner == oldRec.ID), so the wrap
// decision stays aligned with the graceful-route decision — an OLD that no
// longer owns the lease takes the worker/inline retire path, where a
// lease-wrapped standby would mis-route its prompt into the supervisor pane.
// The coord-spawn marker (D3, deleted) is no longer consulted: isCoordResume
// (spawn.IsCoordSpawn) + gracefulSwapEligibleFn (lease owner) cover it.
func liveCoordGracefulSpawn(oldRec *agent.Record, isCoordResume, autoResume bool) bool {
	if !isCoordResume || oldRec == nil || oldRec.Project == "" {
		return false
	}
	return gracefulSwapEligibleFn(oldRec, autoResume)
}

// requireOwner suppresses the legacy ErrNoOwnerObserved direct-send fallback
// (codex PR3-completion iter-7 [P1]): on the GRACEFUL route the successor is
// lease-wrapped by construction (a coord-run --standby supervisor), so "no
// owner ever observed" means the standby has not acquired YET — never a
// legacy bare coord. Direct-sending there types into the supervisor pane and
// can report a false success, letting the caller consume the durable queue
// with the doc undelivered. Owner-only callers keep the queue pending so the
// next drain/attach pass re-delivers to the eventual winner.
func deliverResumePrompt(project string, isCoordSwap, leaseWrappedSuccessor, requireOwner bool, rec *agent.Record,
	docPath string, stdout, stderr io.Writer) (*agent.Record, error) {
	prompt := handoff.ResumePrompt(docPath)
	if coordlock.FailoverEnabled() && isCoordSwap && leaseWrappedSuccessor && project != "" {
		delivered, err := handoffdelivery.DeliverToCurrentOwner(handoffdelivery.Options{
			Project: project,
			Prompt:  prompt,
			Timeout: handoffDeliveryTimeout,
			Poll:    handoffDeliveryPoll,
			Stdout:  stdout,
			Stderr:  stderr,
		}, handoffDeliveryDepsFn())
		if err != nil {
			// Legacy/bare coord with no lease record: the owner-poll can never
			// converge, so fall back to a direct send into the live replacement
			// (matches deliverHandoffResumePrompt in cmd/fleet/handoff.go). A
			// genuine owner-seen-but-undeliverable error is NOT this sentinel and
			// still propagates so the queue stays pending for a real-owner retry.
			if errors.Is(err, handoffdelivery.ErrNoOwnerObserved) && !requireOwner {
				_, _ = fmt.Fprintf(stderr,
					"  resume: no lease owner for project %s (legacy coord?); delivering directly to %s\n",
					project, rec.TmuxSession)
				submitted, serr := sendPromptKeysVerified(rec.TmuxSession, prompt)
				if serr != nil {
					return nil, serr
				}
				if !submitted {
					return nil, fmt.Errorf("resume prompt to %s was typed but not submitted", rec.TmuxSession)
				}
				return rec, nil
			}
			return nil, err
		}
		if delivered != nil {
			_, _ = fmt.Fprintf(stdout,
				"  resume: delivered to lock owner %s (%s)\n",
				delivered.ID, delivered.TmuxSession)
		}
		return delivered, nil
	}
	submitted, err := sendPromptKeysVerified(rec.TmuxSession, prompt)
	if err != nil {
		return nil, err
	}
	if !submitted {
		return nil, fmt.Errorf("resume prompt to %s was typed but not submitted", rec.TmuxSession)
	}
	return rec, nil
}

func retargetArchivedHandoffSuccessor(agentID, successorID string) error {
	if successorID == "" {
		return fmt.Errorf("successorID required")
	}
	archived, err := agent.LoadArchive(agentID)
	if err != nil {
		return err
	}
	archived.SuccessorID = successorID
	archived.ArchivedCause = agent.ArchivedCauseHandoff
	data, err := json.MarshalIndent(archived, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal archive payload: %w", err)
	}
	data = append(data, '\n')
	path, err := state.AgentArchivePath(agentID)
	if err != nil {
		return err
	}
	if err := state.WriteAtomic(path, data); err != nil {
		return fmt.Errorf("write archive %s: %w", agentID, err)
	}
	return nil
}

// coordDrainExecArgv computes the per-spawn exec argv for a COORD
// drain replacement: routes the clean persisted Command through
// rc.GateAttachFlag (native default-on; suppressed by the rc-disabled
// opt-out marker or the FLEET_RC_BOOTSTRAP_DISABLED env-gate) and
// returns nil when the rewrite is a no-op so spawn.Spawn doesn't see a
// pointless Command/ExecCommand divergence. Workers never reach this —
// the isCoordHandoffForAgent call-site gate is the push-storm
// protection.
//
// Extracted for unit-testable coverage without driving a full
// spawnAndRetire (which would exec the rewritten argv for real).
func coordDrainExecArgv(project string, command []string, rcSessionName string) []string {
	rewritten := rc.GateAttachFlag(project, command, rcSessionName)
	if spawn.SameCommand(rewritten, command) {
		return nil
	}
	return rewritten
}

// isCoordHandoffForAgent reports whether rec is a COORD (not a worker /
// Agent-tool subagent) being handed off. spawnAndRetire calls this to gate the
// v0.12.1 RC inject (codex review iter-7 [P1]) AND the coord-spawn lock
// acquisition (duplicate-coord prevention): worker handoffs during fleet drain /
// crash recovery must NOT silently inherit --remote-control (which would reopen
// the v0.12 push-storm class) and must NOT contend on the coord-spawn lock.
//
// The coord-spawn marker (D3, deleted) is replaced by spawn.IsCoordSpawn — the
// primary task-based coord detector (task_id == coord-<project>). It is
// TTL-INDEPENDENT (unlike the lease owner), so a busy coord whose heartbeat
// briefly stalled is never mis-classified as a worker at the lock-acquire gate,
// keeping the duplicate-prevention guard robust.
func isCoordHandoffForAgent(rec *agent.Record) bool {
	return rec != nil && spawn.IsCoordSpawn(rec.TaskID, rec.Project)
}

// leaseNamesCoord reports whether the coordinator lease for project names
// agentID as its coord identity — either the current active owner
// (coordlock.CurrentOwner) or an in-flight handoff successor reservation
// (coordlock.CurrentHandoff). It replaces the deleted spawn marker's
// `marker == agentID` fallback in the coord-delivery detectors (D3): the lease
// owner/successor IS the identity now. spawn.IsCoordSpawn stays the PRIMARY
// coord detector at the call sites; this is the secondary "the lease points at
// this specific agent" signal that the marker used to carry.
func leaseNamesCoord(project, agentID string) bool {
	if project == "" || agentID == "" {
		return false
	}
	if o, ok := coordlock.CurrentOwner(project); ok && o.AgentID == agentID {
		return true
	}
	if h, ok := coordlock.CurrentHandoff(project); ok && h.SuccessorID == agentID {
		return true
	}
	return false
}

// Resume completes a handoff for which the queue file already exists.
// Two producers create such queue files:
//
//  1. The fleet-guard skill (steps 4b/c), on auto-handoff. The queue's
//     NewAgentID + NewSession are pre-allocated but no spawn has happened.
//     Resume must spawn before the tail.
//
//  2. The operator-triggered cmd/fleet/handoff.go, when a previous run
//     crashed AFTER spawn but BEFORE the queue was deleted. The
//     replacement record + session already exist; Resume skips spawn and
//     just runs the tail.
//
// The recovery probe distinguishes the two by checking whether the
// replacement record at NewAgentID exists and its tmux session is alive.
//
// Caller MUST hold state.LockAgent(req.OldAgentID). On success the queue
// file is deleted. On failure the queue file is left in place so a retry
// can pick up where this attempt left off.
func Resume(req queue.SpawnFresh, queuePath string,
	graceMillis int, stdout, stderr io.Writer) error {

	// 1. Load the outgoing agent. If it's already archived, the previous
	//    handoff completed successfully and only the queue file is stale.
	//    Reuse cmd/fleet/handoff.go's existing semantics: verify the
	//    declared replacement is alive before declaring success-noop.
	//
	// Codex iter-18 [P1] (deferred — known recovery gap): if OLD is gone
	// because the operator manually ran `fleet rm <OLD>` (e.g., per the
	// instruction in *ErrOrphanSurvived / *ErrOldKillProbeAmbiguous) AND
	// NEW has since exited, cleanUpStaleQueue surfaces "task has no live
	// agent" instead of respawning. Auto-respawn from this state requires
	// pre-allocating a fresh agent ID (the queue file's NewAgentID is
	// already consumed) and threading the original handoff doc + project
	// context into a fresh spawn — non-trivial because the recovery path
	// would need spawn.Options reconstructed from the queue journal, which
	// does not currently carry NewRecSpec verbatim. Operator workaround:
	// `fleet dispatch --resume <handoff-doc>` after the queue file is
	// cleaned up manually. Deferred to a follow-up PR alongside the
	// "swap-committed sentinel" structural fix.
	oldRec, lerr := agent.Load(req.OldAgentID)
	switch {
	case errors.Is(lerr, state.ErrNotFound):
		return cleanUpStaleQueue(req, queuePath, stdout)
	case lerr != nil:
		return fmt.Errorf("resume: load agent %s failed: %w", req.OldAgentID, lerr)
	}

	// 2. Recovery probe on the replacement.
	newRec, nerr := agent.Load(req.NewAgentID)
	switch {
	case errors.Is(nerr, state.ErrNotFound):
		// Replacement record missing.
		if req.NewSession != "" && tmux.HasSession(req.NewSession) {
			// Orphan tmux session at the journaled NewSession. Refuse —
			// spawning a fresh replacement would create a duplicate
			// live session under the same logical task.
			return fmt.Errorf(
				"resume: replacement %s record missing but tmux session %s still alive — refusing duplicate spawn; clean up the orphan session or queue file %s before retrying",
				req.NewAgentID, req.NewSession, queuePath)
		}
		// Both record and session absent → fresh spawn path.
		return spawnAndRetire(req, queuePath, oldRec, graceMillis, stdout, stderr)
	case nerr != nil:
		return fmt.Errorf("resume: load replacement %s failed: %w", req.NewAgentID, nerr)
	}

	// Replacement record exists. If its session is alive too, this is the
	// crashed-mid-handoff case — skip spawn and just retire the old agent.
	// If the session is dead, the spawn started but the command crashed
	// at startup (a wrapped engine binary, perhaps); wipe the stale
	// record and spawn fresh.
	//
	// HasSession-then-Remove was the leak (fix/orphan-tmux-sweeper-and-leak-plug):
	// HasSession returns false for both "session gone" AND "probe failed",
	// so a transient probe failure could delete the record while the tmux
	// session was still alive — orphaning it.
	//
	// Codex iter-1 [P1]: HasSession is also ambiguous at the OUTER gate.
	// A transient probe-fail used to enter the cleanup branch where the
	// helper's own SessionAlive probe could disagree on a retry and kill
	// a healthy replacement. Use SessionAlive tristate directly (via the
	// package-level seam tests can swap) so the branch fires only on
	// definitive (alive=false, err=nil); on probe errors, surface the
	// ambiguity and preserve the record for operator inspection — the
	// surrounding handoff stays untouched.
	switch alive, perr := tmuxSessionAliveFn(newRec.TmuxSession); {
	case perr != nil:
		return fmt.Errorf(
			"resume: probe replacement %s session %s failed: %w (record preserved for operator inspection)",
			newRec.ID, newRec.TmuxSession, perr)
	case !alive:
		// NEW's session is definitively dead. Decide respawn / refuse / wait
		// PURELY from the coordinator LEASE (the coord-spawn marker is gone, D3)
		// via the Class-4 classifier — the P0 duplicate-coord guard. The lease
		// commit is ASYNC (the winning standby bumps the epoch), so at this
		// recovery probe the lease is in one of three states:
		//
		//   owner == NEW (epoch committed)   → REFUSE: respawning duplicates the
		//     coord. DEFENSE-IN-DEPTH: a cross-socket OLD still alive here is a
		//     LOUD diagnostic (surface, operator-gate; never auto-kill).
		//   handoff{NEW}, no active owner    → WAIT: swap in flight, the warm
		//     standby is the recovery vehicle; do NOT respawn a 2nd replacement.
		//   owner == OLD, or free + no handoff → RESPAWN: not committed, safe to
		//     drop the dead NEW and spawn fresh.
		//
		// force=false: the auto-drain path has no operator to pass
		// --force-replacement.
		deps := DefaultCoordSwapResumeDeps()
		deps.OldSessionAlive = tmuxSessionAliveFn // reuse the test-injectable probe seam
		resume := ClassifyCoordSwapResume(oldRec.Project, oldRec.ID, oldRec.TmuxSession, newRec.ID, false, deps)
		switch resume.Action {
		case ResumeRefuse:
			// Committed to NEW — refuse. Surface a LOUD diagnostic when the
			// defensive probe found OLD still alive (cross-socket blind spot)
			// or was ambiguous; the verdict is unchanged (refuse), the probe
			// only shapes the message. NEVER auto-kills.
			if resume.OldStillAlive || resume.OldProbeErr != nil {
				_, _ = fmt.Fprintf(stderr,
					"[P1] resume: replacement %s session %s exited AFTER the epoch committed to it, but a defensive probe found OLD coord %s session %s may still be alive (alive=%v err=%v) — refusing auto-respawn (would create a duplicate coord); operator: confirm which coord is live, then re-run fleet drain (queue preserved at %s)\n",
					newRec.ID, newRec.TmuxSession, oldRec.ID, oldRec.TmuxSession, resume.OldStillAlive, resume.OldProbeErr, queuePath)
			}
			return fmt.Errorf(
				"resume: replacement %s session %s exited but the coordinator epoch already committed to it (%s) — refusing auto-respawn (would create a duplicate coord); kill the orphan or use `fleet handoff --force-replacement` (queue preserved at %s)",
				newRec.ID, newRec.TmuxSession, resume.Reason, queuePath)
		case ResumeWait:
			// Swap in flight (a handoff reservation names NEW, no active owner):
			// the warm standby is the recovery vehicle. Do NOT respawn a 2nd
			// replacement; keep the queue for the standby's eventual win.
			return fmt.Errorf(
				"resume: replacement %s session %s exited while a handoff to it is in flight (%s) — the standby is the recovery vehicle; not respawning a duplicate (queue preserved at %s)",
				newRec.ID, newRec.TmuxSession, resume.Reason, queuePath)
		}
		// ResumeRespawn: not committed (OLD owns the lease, or a free lease with
		// no reservation) — safe to drop the dead NEW and spawn fresh.
		if dropErr := DropReplacementRecord(newRec.TmuxSession, newRec.ID, stderr); dropErr != nil {
			return fmt.Errorf(
				"resume: stale replacement %s session %s appeared dead but cleanup failed (%w); spawn fresh aborted",
				newRec.ID, newRec.TmuxSession, dropErr)
		}
		_, _ = fmt.Fprintf(stderr,
			"note: stale replacement %s (session %s already exited) cleaned up; spawning fresh replacement (%s)\n",
			newRec.ID, newRec.TmuxSession, resume.Reason)
		return spawnAndRetire(req, queuePath, oldRec, graceMillis, stdout, stderr)
	}
	// Resolve THIS handoff's auto-resume policy from queue override
	// + oldRec baseline (codex review iter-12 P2). Combine v1 schema
	// gate (codex iter-17 P2) — but ONLY for the SEND. Don't conflate
	// "v1 legacy drain" with "operator opted out" (codex iter-18 P2):
	// case-3 always proceeds to retire (the replacement is already
	// alive, caller of `fleet handoff` already accepted manual
	// prompt requirements), so the only knob the v1 gate touches
	// here is whether retireOldAgent SENDS the prompt.
	thisHandoffDisable := oldRec.DisableAutoResume
	if req.DisableAutoResume != nil {
		thisHandoffDisable = *req.DisableAutoResume
	}
	if req.SchemaVersion < 2 {
		thisHandoffDisable = true
	}
	// Coord-swap detection for the case-3 resume path (replacement already
	// spawned + alive). isCoordSwap is now the task-based coord detector
	// (spawn.IsCoordSpawn) — the coord-spawn marker is gone (D3), and the
	// identity commit is the winning standby's async epoch bump. No eager write
	// is needed: there is no marker to move, and retireOldAgent's graceful /
	// AtomicCoordSwap branches reserve the handoff on OLD's lease themselves.
	isCoordSwap := spawn.IsCoordSpawn(oldRec.TaskID, oldRec.Project)
	// Read the ACTUAL wrap state from the already-spawned replacement record
	// (codex iter-24 [P2]) — a bare drain cold-resume coord records false even
	// on a CapApproved queue, so do NOT infer wrapping from req.CapApproved.
	leaseWrappedSuccessor := newRec.LeaseWrapped
	return retireOldAgent(oldRec, newRec, req.HandoffDoc, queuePath,
		thisHandoffDisable, graceMillis, isCoordSwap, leaseWrappedSuccessor, stdout, stderr)
}

// cleanUpStaleQueue handles the "old record already archived" branch.
// Verifies the replacement is alive before declaring success — otherwise
// the task has zero live agents and we must surface that, not silently
// delete the queue.
func cleanUpStaleQueue(req queue.SpawnFresh, queuePath string,
	stdout io.Writer) error {

	newRec, nerr := agent.Load(req.NewAgentID)
	switch {
	case errors.Is(nerr, state.ErrNotFound):
		return fmt.Errorf(
			"resume: agent %s already archived BUT replacement %s record is gone — task has no live agent; clean up queue file %s manually after starting a new agent",
			req.OldAgentID, req.NewAgentID, queuePath)
	case nerr != nil:
		return fmt.Errorf("resume: load replacement %s failed: %w", req.NewAgentID, nerr)
	}
	// Resolve auto-resume from queue override + newRec baseline
	// (codex review iter-12 P2). Gate on schema v2+ (codex
	// iter-15 P2) — v1 queue files predate this feature.
	disableAutoResume := newRec.DisableAutoResume
	if req.DisableAutoResume != nil {
		disableAutoResume = *req.DisableAutoResume
	}
	autoResume := !disableAutoResume && req.SchemaVersion >= 2

	// Coord lock-owner delivery (PR2 §5a): when this is a lease-wrapped coord
	// swap on a lease-capable platform, the resume prompt is delivered to the
	// project's CURRENT LOCK OWNER (discovered via coordlock.CurrentOwner), NOT
	// into newRec's session. newRec may be a racing standby that LOST the lock and
	// has since exited; the live coord is the winner. So the queued-replacement
	// session-liveness aborts below must NOT fire for this case — they would
	// strand the queue pending even though CurrentOwner can identify the live
	// winner and deliver to it (codex iter-31 P1).
	coordOwnerDelivery := autoResume && coordlock.FailoverEnabled() && newRec.LeaseWrapped &&
		(spawn.IsCoordSpawn(newRec.TaskID, newRec.Project) ||
			leaseNamesCoord(newRec.Project, newRec.ID))

	if !tmux.HasSession(newRec.TmuxSession) && !coordOwnerDelivery {
		return fmt.Errorf(
			"resume: agent %s already archived BUT replacement %s tmux session %s is gone — task has no live agent; investigate before deleting queue file %s",
			req.OldAgentID, req.NewAgentID, newRec.TmuxSession, queuePath)
	}

	// Cover the iter-10 P1 crash window: if the previous run
	// crashed AFTER oldRec.Archive() but BEFORE queue.Delete,
	// SendPromptKeys also never ran (it lives after queue.Delete).
	// The replacement is alive but un-prompted. Send here. If we
	// got past queue.Delete in the previous run, queue would be
	// gone and we wouldn't be on this path — so this delivery is
	// the FIRST send, not a duplicate.

	// Wait + liveness probe ALWAYS run, even when autoResume is off
	// — the wait doubles as a post-spawn liveness probe (codex
	// review iter-16 P1). Only the SEND is gated on autoResume below.
	if err := spawn.WaitForReadyToPrompt(newRec.TmuxSession); err != nil {
		_, _ = fmt.Fprintf(stdout,
			"warning: readiness poll for %s did not converge: %v (proceeding anyway)\n",
			newRec.TmuxSession, err)
	}
	if alive, perr := tmux.SessionAlive(newRec.TmuxSession); perr != nil {
		_, _ = fmt.Fprintf(stdout,
			"warning: post-readiness probe for %s failed: %v (proceeding anyway)\n",
			newRec.TmuxSession, perr)
	} else if !alive && !coordOwnerDelivery {
		// For a coord lock-owner delivery the queued replacement exiting is
		// EXPECTED when a racing standby won — delivery targets CurrentOwner,
		// not this dead session, so proceed (codex iter-31 P1).
		return fmt.Errorf(
			"resume: agent %s already archived BUT replacement %s tmux session %s exited during readiness wait — task has no live agent",
			req.OldAgentID, req.NewAgentID, newRec.TmuxSession)
	}
	supersededSession := ""
	supersededID := ""
	if autoResume {
		coordDelivery := spawn.IsCoordSpawn(newRec.TaskID, newRec.Project) ||
			leaseNamesCoord(newRec.Project, newRec.ID)
		// Read the ACTUAL wrap state from the already-spawned replacement
		// record (codex iter-24 [P2]), not the producer's cap-approval bit: a
		// bare drain cold-resume coord records false even on a CapApproved queue.
		//
		// requireOwner = leaseWrappedSuccessor (codex PR3-completion iter-8
		// [P1]): this archived-OLD retry is exactly where a graceful swap that
		// retired OLD but timed out on delivery resumes. A LEASE-WRAPPED
		// replacement with no observed owner is a standby that has not
		// acquired YET — the legacy direct-send fallback would type into its
		// coord-run supervisor pane, report success, and consume the queue
		// while the eventual winner never gets the doc. Bare legacy
		// replacements (leaseWrappedSuccessor=false) skip the owner-poll
		// entirely, so the fallback remains for genuinely lease-less coords.
		leaseWrappedSuccessor := newRec.LeaseWrapped
		deliveredRec, err := deliverResumePrompt(newRec.Project, coordDelivery, leaseWrappedSuccessor, leaseWrappedSuccessor,
			newRec, req.HandoffDoc, stdout, stdout)
		if err != nil {
			return fmt.Errorf(
				"resume: agent %s already archived BUT resume prompt delivery is still pending: %w (queue preserved at %s)",
				req.OldAgentID, err, queuePath)
		}
		if deliveredRec != nil {
			if deliveredRec.ID != newRec.ID {
				// Retarget the archive to the lock owner, but DEFER dropping the
				// superseded queued replacement until AFTER queue.Delete: if a
				// later step fails the queue still names newRec, so a recovery
				// pass must be able to reload it (codex P2).
				if err := retargetArchivedHandoffSuccessor(req.OldAgentID, deliveredRec.ID); err != nil {
					return fmt.Errorf(
						"resume: prompt delivered to lock owner %s but handoff archive retarget failed: %w (queue preserved at %s)",
						deliveredRec.ID, err, queuePath)
				}
				supersededSession = newRec.TmuxSession
				supersededID = newRec.ID
			}
			newRec = deliveredRec
		}
	}
	if err := queue.Delete(queuePath); err != nil {
		return fmt.Errorf(
			"resume: agent %s already handed off → %s but queue cleanup failed (%w); will retry",
			req.OldAgentID, req.NewAgentID, err)
	}
	if supersededID != "" {
		if err := DropReplacementRecord(supersededSession, supersededID, stdout); err != nil {
			return fmt.Errorf(
				"resume: agent %s already handed off → %s BUT superseded replacement %s cleanup failed: %w",
				req.OldAgentID, newRec.ID, supersededID, err)
		}
	}
	_, _ = fmt.Fprintf(stdout,
		"agent %s already handed off → %s (cleaned stale queue file)\n",
		req.OldAgentID, req.NewAgentID)
	if !autoResume {
		// Original handoff opted out — replacement is alive but
		// idle. Tell the operator what to type on attach (codex
		// review iter-11 P2). Note: this is the auto-handoff drain
		// path so "operator" output goes to whoever's reading
		// stdout (drain CLI / TUI background message stream).
		_, _ = fmt.Fprintf(stdout,
			"then say: read the handoff doc at %s and continue\n",
			req.HandoffDoc)
	}
	return nil
}

// spawnAndRetire is the "skill wrote the queue, no spawn yet" path.
// Spawns the replacement using the queue's pre-allocated ID, verifies
// the session, then retires the old agent.
func spawnAndRetire(req queue.SpawnFresh, queuePath string,
	oldRec *agent.Record, graceMillis int, stdout, stderr io.Writer) error {

	// FLEET_MAX_SESSIONS backstop on the auto-drain path. The
	// fleet-guard skill writes queue files on auto-handoff; this
	// helper is the consumer. Without the cap here, a runaway
	// auto-handoff loop (e.g. a future bug that retries forever)
	// could blow past the operator's limit. No --force-replacement
	// escape on this path because there's no operator to flag it —
	// the queue file is the only producer. Probe failures don't
	// block (best-effort, same as the CLI gate).
	//
	// Cap is re-checked on EVERY consumer pass (codex iter-8 P1):
	// the producer-side approval in cmd/fleet/handoff.go is not
	// load-bearing for crash recovery — between original handoff
	// and retry the cap state can shift (operator lowered
	// FLEET_MAX_SESSIONS; other paths spawned sessions; the old
	// session died, turning a net-zero swap into a +1 spawn). Only
	// the swap-aware accounting below is correct for arbitrary
	// post-crash state. The producer-side CapApproved field
	// remains in the queue schema as a forward-compatibility hook
	// and a tracking signal for diagnostics, but is intentionally
	// NOT used to bypass this check.
	counts, cerr := state.CountFleetSessions(
		sessionListProbe, state.LiveAgentRecordExists)
	if cerr != nil {
		_, _ = fmt.Fprintf(stderr,
			"warning: FLEET_MAX_SESSIONS precheck could not enumerate tmux sessions (%v); proceeding without cap enforcement\n",
			cerr)
	} else {
		max := state.MaxSessions(stderr)
		// If the old session is alive (its name appears in the list),
		// the swap is net-zero. Otherwise the spawn is net +1.
		// SessionAlive (tristate): probe-error treated as alive for
		// this accounting so a transient tmux flake doesn't push us
		// into the over-refusal branch. Best-effort semantics —
		// matches the upstream "don't block on probe failures" rule
		// applied to listFn above.
		alive, probeErr := sessionAliveProbe(oldRec.TmuxSession)
		oldInCount := alive || probeErr != nil
		projected := counts.Total()
		if !oldInCount {
			// Old session definitively gone — spawn is net +1.
			projected = counts.Total() + 1
		}
		if projected > max {
			// Queue file is preserved so the operator can drain
			// manually after pruning. We intentionally DON'T suggest
			// `fleet rm <id>` here (codex iter-10 P1): rm refuses
			// any agent that has a pending spawn-fresh queue file,
			// so on this drain path the operator must either prune
			// ORPHANS (the only sessions rm-able right now) or
			// raise FLEET_MAX_SESSIONS. The CLI handoff gate
			// retains `fleet rm <id>` since it fires BEFORE the
			// queue write.
			return fmt.Errorf(
				"resume: refusing to spawn — already at FLEET_MAX_SESSIONS=%d tmux sessions (%d live, %d orphan); run `fleet maintenance prune-orphan-tmux --kill` to reap orphans (or raise FLEET_MAX_SESSIONS), then rerun `fleet drain` (queue file %s preserved)",
				max, counts.Live, counts.Orphan, queuePath)
		}
	}

	if oldRec.Cwd == "" {
		return fmt.Errorf(
			"resume: agent %s is a legacy record with no stored cwd; manual `fleet handoff --cwd` required",
			oldRec.ID)
	}
	if len(oldRec.Command) == 0 {
		return fmt.Errorf(
			"resume: agent %s is a legacy record with no stored command; manual `fleet handoff --command` required",
			oldRec.ID)
	}
	// Resolve THIS handoff's auto-resume: queue's override (if set)
	// wins, else inherit from oldRec (codex review iter-10/11/12 P2).
	disableAutoResume := oldRec.DisableAutoResume
	if req.DisableAutoResume != nil {
		disableAutoResume = *req.DisableAutoResume
	}

	// Reject fresh-spawn auto-handoff for opt-out agents (codex
	// review iter-9 P1, scoped to spawnAndRetire per iter-10 P2).
	// We're about to bring up a NEW agent that won't get a resume
	// prompt — and there's no operator on this drain path to type
	// one manually. Spawning would leave the replacement idle
	// forever. Surface a clear error pointing at `fleet handoff`,
	// the interactive path. Queue file preserved for that retry.
	//
	// IMPORTANT: this reject is for EXPLICIT opt-out only (record
	// baseline or queue override). v1 schema queues (codex iter-18
	// P2) are NOT opt-outs — they just predate the auto-resume
	// feature. v1 queues drain normally; the only difference is
	// retireOldAgent skips the send for them.
	if disableAutoResume {
		return fmt.Errorf(
			"resume: agent %s opted out of auto-resume; auto-handoff would leave the replacement idle. Trigger handoff manually with `fleet handoff %s` (queue file %s preserved)",
			req.OldAgentID, req.OldAgentID, queuePath)
	}

	// thisHandoffDisableAutoResume is what gets passed to retire's
	// SEND gate. It collapses "explicit opt-out" with "v1 queue
	// legacy compatibility" — both mean "don't send" but we already
	// returned above on the explicit opt-out, so this is just the
	// v1 case. Spawn.DisableAutoResume gets the explicit-only value
	// (disableAutoResume) so v1 drains don't permanently flip the
	// new record's baseline.
	thisHandoffDisableAutoResume := disableAutoResume
	if req.SchemaVersion < 2 {
		thisHandoffDisableAutoResume = true
	}

	// Auto-handoff replacements get `--remote-control
	// "fleet-handoff-<new-id>"` injected into the spawned claude argv
	// so mobile / claude.ai pairing carries through automatically —
	// matching the operator-triggered cmd/fleet/handoff.go path
	// (fix/remote-control-coord-injection P0). Without this the
	// auto-drained replacement only pairs after the agent runs the
	// `/remote-control` slash command from FirstAction's manual
	// instructions, which may never happen on a busy session.
	//
	// Persisted Command stays the clean `oldRec.Command` so a
	// subsequent handoff doesn't inherit a stale session name; the
	// rewrite goes via ExecCommand (per-spawn argv only). For
	// operator-overridden custom --commands, InjectRemoteControlFlag
	// returns the slice unchanged — we then pass nil as ExecCommand
	// so spawn.Spawn doesn't see a no-op divergence.
	// Session names use the coord shape (`fleet-coord-<id>-<project>`)
	// so fresh-spawn and handoff replacements are uniform on
	// claude.ai / mobile (codex round-6 P1, historical).
	rcSessionName := spawn.CoordRemoteControlSessionName(req.NewAgentID, oldRec.Project)
	// Native model: use the project-aware rc.GateAttachFlag helper.
	// Auto-handoff drain hits this code path without going through
	// cmd/fleet's wrapper, so the dedicated helper carries the same
	// rc-disabled opt-out + FLEET_RC_BOOTSTRAP_DISABLED two-layer gate
	// (rather than re-implementing it here).
	//
	// v0.12.1 codex review iter-7 [P1] (kept): ALSO gate on
	// isCoordHandoff — RC is default-on project-wide, so worker
	// handoffs resumed via fleet drain / crash recovery would silently
	// inherit --remote-control without this call-site gate. Workers /
	// Agent-tool subagents must NEVER carry the flag (push-storm
	// protection). Mirrors cmd/fleet/handoff.go and dispatch.go.
	//
	// The predicate matches the post-spawn isCoordSwap detector below —
	// same fact (spawn.IsCoordSpawn: task_id == coord-<project>), used
	// at two points (pre-inject + post-spawn). Extracted as
	// isCoordHandoffForAgent for unit-testable gate coverage without
	// driving full spawnAndRetire.
	var rewrittenExecArgv []string
	if isCoordHandoffForAgent(oldRec) {
		// v0.12.2 P0 v3 (DESIGN-coord-spawn-atomic-gate.md §Change 7;
		// codex iter-1 [P1] from the v2 reviewer + v3 reviewer codex
		// iter-1 [P1] on warn-and-continue): acquire the SAME
		// project-level coord-spawn lock that cmd/fleet/dispatch.go
		// uses, so a TUI `[a]` racing an in-flight drain replacement
		// contends on the lock rather than slipping past it.
		//
		// Fail-fast on contention (corrected from design v3 §Change 7
		// "warn-and-continue" rationale): the design's claim that
		// "bailing out would orphan the queue file" is the inverse of
		// the actual semantics. queue.Delete only runs on the SUCCESS
		// path of spawnAndRetire (line ~387 / line ~796 / line ~868);
		// returning here BEFORE the spawn preserves the queue file
		// untouched, and the drain loop (fleet drain / fsnotify
		// watcher) retries the same queue file in the next cycle by
		// which time the contending lock holder will have released.
		// Continuing past contention would actually CONSUME the queue
		// file AND race — defeating the gate's whole purpose. Codex
		// surfaced this in the v3 review.
		release, lockErr := coordlock.Acquire(oldRec.Project)
		if lockErr != nil {
			_, _ = fmt.Fprintf(stderr,
				"warning: coordlock.Acquire(%q) contended during drain handoff: %v "+
					"(deferring; queue file preserved for retry next drain cycle)\n",
				oldRec.Project, lockErr)
			return fmt.Errorf(
				"drain: coord-spawn lock contended for project %q: %w "+
					"(queue file preserved for retry)",
				oldRec.Project, lockErr)
		}
		defer release()

		// Native model: no marker backfill needed — rc.Enabled is
		// default-on; coordDrainExecArgv bakes the flag unless the
		// operator opted the project out (rc-disabled) or the test
		// env-gate is set.
		rewrittenExecArgv = coordDrainExecArgv(oldRec.Project, oldRec.Command, rcSessionName)
	}

	// COORD drain handoffs resolve the repo binding via the shared
	// resolver, NOT the old coord's stored Cwd (DESIGN-coord-repo-
	// binding-from-project.md PR3). Same rationale as the manual
	// cmd/fleet/handoff.go path: a wrong-repo coord must not hand off
	// into the same wrong tree. Resolve fresh and REFUSE on an
	// unresolvable project. persist=true (handoff is operator-initiated).
	// Worker handoffs keep inheriting oldRec.Cwd.
	spawnCwd := oldRec.Cwd
	legacyCoordResume := spawn.IsCoordSpawn(oldRec.TaskID, oldRec.Project)
	if legacyCoordResume {
		resolved, rerr := coordrepo.ResolveProjectRepo(oldRec.Project, true)
		if rerr != nil {
			return fmt.Errorf("resume: coord handoff for project %q: %w", oldRec.Project, rerr)
		}
		spawnCwd = resolved
	}

	// PR3 (DESIGN-coord-spawn-unified-standby §5b/§6): a LIVE-coord handoff
	// (OLD currently owns the lease + the doc will be auto-delivered) spawns
	// a LEASE-WRAPPED standby successor and completes through the
	// GracefulHandoff barrier in retireOldAgent — the winner heartbeats the
	// lease so it never lapses. Only the dead-coord cold resume keeps the
	// bare/direct shape (see liveCoordGracefulSpawn).
	gracefulCoordRoute := liveCoordGracefulSpawn(oldRec, legacyCoordResume,
		!thisHandoffDisableAutoResume)

	newRec, err := spawn.Spawn(spawn.Options{
		OldRecord:      oldRec,
		NewDocPath:     req.HandoffDoc,
		Cwd:            spawnCwd,
		Command:        oldRec.Command,
		ExecCommand:    rewrittenExecArgv,
		PreAllocatedID: req.NewAgentID,
		// Use disableAutoResume (explicit opt-out only), not
		// thisHandoffDisableAutoResume (which includes the v1
		// legacy case). A v1 drain shouldn't permanently flip the
		// new record's baseline to opt-out (codex iter-18 P2).
		DisableAutoResume: disableAutoResume,
		StandbyTimeout:    spawn.DefaultStandbyTimeout,
		DisableLeaseWrap:  legacyCoordResume && !gracefulCoordRoute,
	})
	if err != nil {
		return fmt.Errorf("resume: spawn replacement: %w", err)
	}
	// Codex iter-1 [P1]: use SessionAlive tristate at the outer gate (via
	// the package-level seam tests can swap) so a transient probe-failure
	// doesn't masquerade as "session dead" and roll back a healthy spawn.
	// Probe errors surface as explicit errors with the new record
	// preserved; only definitive (alive=false, err=nil) enters the
	// rollback branch.
	switch alive, perr := tmuxSessionAliveFn(newRec.TmuxSession); {
	case perr != nil:
		return fmt.Errorf(
			"resume: probe replacement %s session %s failed: %w (record preserved; old agent untouched, queue file preserved for retry)",
			newRec.ID, newRec.TmuxSession, perr)
	case !alive:
		// fix/orphan-tmux-sweeper-and-leak-plug: use DropReplacementRecord
		// so a probe-failure window inside Kill doesn't delete the record
		// while the tmux session is still alive.
		if dropErr := DropReplacementRecord(newRec.TmuxSession, newRec.ID, stderr); dropErr != nil {
			return fmt.Errorf(
				"resume: replacement %s tmux session %s appeared dead but cleanup failed (%w); old agent untouched, queue file preserved for retry",
				newRec.ID, newRec.TmuxSession, dropErr)
		}
		return fmt.Errorf(
			"resume: replacement %s spawned but tmux session %s already exited (command crashed at startup?); old agent untouched, queue file preserved for retry",
			newRec.ID, newRec.TmuxSession)
	}

	// Coord-swap detection is now the task-based coord detector
	// (spawn.IsCoordSpawn) — the coord-spawn marker is gone (D3). There is no
	// eager marker write anymore: the identity commit is the winning standby's
	// async epoch bump, and retireOldAgent's graceful / AtomicCoordSwap branches
	// reserve the handoff on OLD's lease themselves (a soft TTL guard that closes
	// the duplicate-coord window without a marker). Workers and other non-coord
	// agents go through retireOldAgent's inline path unchanged — the v0.2
	// worker-swap transactional refactor is deferred per the v5 plan's Non-goals.
	isCoordSwap := spawn.IsCoordSpawn(oldRec.TaskID, oldRec.Project)

	// A coord successor here is BARE on the dead-coord cold resume
	// (DisableLeaseWrap above; codex iter-20 P2) but LEASE-WRAPPED on the
	// PR3 live-coord graceful route. spawn.Spawn stamped the ACTUAL wrap
	// state onto the record, so read that authoritative value (codex
	// iter-24 P2) rather than the producer's cap-approval bit. Delivering
	// through the owner-poll for a bare successor would mis-route to the
	// old/dead lease owner instead of the successor we just spawned.
	leaseWrappedSuccessor := newRec.LeaseWrapped
	return retireOldAgent(oldRec, newRec, req.HandoffDoc, queuePath,
		thisHandoffDisableAutoResume, graceMillis, isCoordSwap, leaseWrappedSuccessor, stdout, stderr)
}

// retireOldAgent runs the post-spawn tail in this order: wait for
// new's pane to stabilize → /exit + grace + kill the old → archive
// → delete queue → send the resume prompt. Caller has verified
// newRec.TmuxSession is alive at entry.
//
// Sequencing rationale across the codex review series (iter-1, 2,
// 4, 5, 6, 7, 8):
//
//  1. Prompt delivery lives HERE, not in spawn.Spawn, so crash
//     recovery (Resume → retireOldAgent for case-3) uses the same
//     delivery path as happy path. Single source of truth.
//
//  2. The readiness wait runs BEFORE Kill(old). The wait is passive
//     — new is rendering UI, not doing work — so it doesn't violate
//     the iter-2 P2 invariant that "new doesn't do work during the
//     OLD↔NEW overlap." Putting the wait first means a dead-during-
//     wait crashes cleanly: roll back the new, leave the old alive,
//     surface the error so the operator/recovery can retry. Pre-fix
//     (iter-7), the wait happened AFTER Kill(old) and a dead-during-
//     wait left the task stranded with no live agent.
//
//  3. The actual send-keys runs AFTER queue.Delete. Once the queue
//     file is gone no recovery path can run, so the prompt is
//     delivered at most once per logical handoff. Sending earlier
//     would mean a crash between send and queue.Delete leads to a
//     retry that re-sends, making claude redo work. Lost-prompt
//     window is the microseconds between queue.Delete returning
//     and the send-keys call.
//
//  4. Auto-resume can be disabled per-record via DisableAutoResume
//     (set by --no-auto-resume on dispatch or handoff). When off,
//     both the wait and the send are skipped — the operator types
//     their own first prompt on attach. This protects non-claude
//     wrappers from receiving "Read your handoff doc..." as garbage
//     input.
//
// Rollback semantics on Kill failure: kill the new session, delete
// the new record + queue, surface the live old session for operator
// triage.
// leaseWrappedSuccessor reports whether newRec is a lease-wrapped successor
// (spawned via coord-run --standby), in which case the resume prompt must be
// delivered to whoever wins the coordinator lease (DeliverToCurrentOwner)
// rather than the queued session, which may have lost the race. The caller
// threads it in because the queue's CapApproved flag — the authoritative
// signal — is not reconstructable from the agent record alone (codex iter-18
// [P1]: deriving it from TaskID here mis-routed recovered coord prompts to the
// losing session).
func retireOldAgent(oldRec, newRec *agent.Record, docPath, queuePath string,
	disableAutoResume bool,
	graceMillis int, isCoordSwap, leaseWrappedSuccessor bool, stdout, stderr io.Writer) error {

	// disableAutoResume comes from the caller so per-handoff
	// overrides (queue's *bool) win over newRec's baseline policy
	// (codex review iter-12 P2).
	autoResume := !disableAutoResume

	// Wait BEFORE killing old (codex review iter-8 P1). The wait is
	// passive — new is rendering UI, not doing work; only the post-
	// queue.Delete SendPromptKeys starts the new agent's work — so
	// this respects the iter-2 P2 invariant. If the new agent dies
	// during the wait, OLD is still alive; roll back the new and
	// return so operator/recovery can retry cleanly.
	// Always runs, even when auto-resume is disabled (codex iter-9
	// P1): the wait doubles as a post-spawn liveness check, catching
	// wrappers that survive the immediate HasSession check but crash
	// shortly after.
	if err := spawn.WaitForReadyToPrompt(newRec.TmuxSession); err != nil {
		_, _ = fmt.Fprintf(stderr,
			"warning: readiness poll for %s did not converge: %v (proceeding anyway)\n",
			newRec.TmuxSession, err)
	}
	// SessionAlive (not HasSession) so transport probe failures
	// don't roll back a live replacement (codex iter-15 P1).
	if alive, perr := tmux.SessionAlive(newRec.TmuxSession); perr != nil {
		_, _ = fmt.Fprintf(stderr,
			"warning: post-readiness probe for %s failed: %v (proceeding anyway)\n",
			newRec.TmuxSession, perr)
	} else if !alive {
		if path, perr := state.AgentPath(newRec.ID); perr == nil {
			_ = os.Remove(path)
		}
		_ = queue.Delete(queuePath)
		// NEW died during the readiness wait. There is no marker to roll back
		// (D3, deleted): OLD still owns the lease, so discovery keeps resolving
		// to OLD and no eager write ever moved the identity off it.
		return fmt.Errorf(
			"resume: replacement %s tmux session %s exited during readiness wait; old agent %s untouched, retry handoff",
			newRec.ID, newRec.TmuxSession, oldRec.ID)
	}

	// Coord swap fast path. When OLD is the project's coordinator
	// (caller-supplied isCoordSwap = spawn.IsCoordSpawn), the retire sequence
	// runs through the transactional AtomicCoordSwap helper (reserve handoff on
	// OLD's lease → /exit + grace + Kill + Archive) instead of the inline
	// worker retire. The identity commit is the winning standby's async epoch
	// bump — there is no synchronous marker write anymore. Worker handoffs fall
	// through to the inline path.
	//
	// IMPORTANT: the spawnAndRetire caller has already (a) written
	// the NEW agent record and (b) probed NEW alive. AtomicCoordSwap's
	// AlreadySpawnedNewRec entrypoint skips its own spawn + probe
	// steps and proceeds straight to reserving the handoff + retiring OLD.
	//
	// PR3 (DESIGN-coord-spawn-unified-standby §6): a LIVE-coord swap —
	// OLD currently owns the lease, the successor is a lease-wrapped
	// standby, and the doc will be auto-delivered — routes through the
	// GracefulHandoff BARRIER instead: barrier write (doc already
	// durable) → AtomicCoordSwap (OLD's coord-run exit releases the
	// lease) → poll the lock winner → inject the doc. The barrier lets a
	// concurrent `fleet drain` see graceful-in-flight (drainGraceful)
	// instead of racing a second successor in. Delivery happens INSIDE
	// the barrier completion, so the autoResume send below is skipped.
	var gracefulWinner *agent.Record
	gracefulRouted := false
	if isCoordSwap && leaseWrappedSuccessor && autoResume &&
		gracefulSwapEligibleFn(oldRec, autoResume) {
		winner, gerr := gracefulCoordSwapFn(oldRec, newRec, docPath,
			time.Duration(graceMillis)*time.Millisecond,
			func() (*agent.Record, error) {
				return deliverResumePrompt(oldRec.Project, isCoordSwap,
					leaseWrappedSuccessor, true, newRec, docPath, stdout, stderr)
			}, stderr)
		if gerr != nil {
			// Same contract as the bespoke branches: queue preserved so a
			// retry (Resume / next drain pass / lazy attach) finishes from
			// the durable journal. AtomicCoordSwap sentinels stay
			// errors.Is-reachable through the wrap.
			return fmt.Errorf("resume: graceful coord swap for %s: %w (queue preserved at %s)",
				oldRec.ID, gerr, queuePath)
		}
		gracefulRouted = true
		gracefulWinner = winner
	} else if isCoordSwap {
		_, swapErr := AtomicCoordSwap(AtomicCoordSwapInputs{
			Project:              oldRec.Project,
			OldRec:               oldRec,
			AlreadySpawnedNewRec: newRec,
			GraceWindow:          time.Duration(graceMillis) * time.Millisecond,
		}, stderr)
		if swapErr != nil {
			// Codex iter-6/7: both ErrOldKillProbeAmbiguous AND
			// ErrOrphanSurvived (iter-7 stopped auto-archiving OLD
			// on FAILURE MODE 5) mean the swap is not in a clean
			// final state — OLD may still be alive on another socket
			// holding the coordinator.lock; NEW may lose the lock
			// race + exit. Preserve the queue journal so retry can
			// resume after operator confirms OLD dead. Pass the
			// error up unchanged — surrounding drain leaves the
			// queue alone, surfaces the error to log analysis.
			return swapErr
		}
	} else {
		// Worker (or non-coord) handoff: inline retire — same flow
		// the codex iter-1..20 series hardened. Deferred for a later
		// PR that adds task-row CAS semantics.
		if err := tmux.SendKeys(oldRec.TmuxSession, "/exit", "Enter"); err != nil &&
			!errors.Is(err, tmux.ErrNoSession) {
			_, _ = fmt.Fprintf(stderr, "warning: send-keys to %s: %v\n",
				oldRec.TmuxSession, err)
		}
		if graceMillis > 0 {
			time.Sleep(time.Duration(graceMillis) * time.Millisecond)
		}
		if err := tmux.Kill(oldRec.TmuxSession); err != nil {
			if tmux.HasSession(oldRec.TmuxSession) {
				if dropErr := DropReplacementRecord(newRec.TmuxSession, newRec.ID, stderr); dropErr != nil {
					return fmt.Errorf(
						"resume: old session %s AND new session %s both alive after kill failure: %w (replacement %s record preserved; cleanup attempt also failed: %v; clean up both manually)",
						oldRec.TmuxSession, newRec.TmuxSession, err, newRec.ID, dropErr)
				}
				_ = queue.Delete(queuePath)
				return fmt.Errorf(
					"resume: old session %s still alive after kill failure: %w (replacement %s rolled back; investigate)",
					oldRec.TmuxSession, err, newRec.ID)
			}
			_, _ = fmt.Fprintf(stderr,
				"note: kill %s reported error but session is gone: %v\n",
				oldRec.TmuxSession, err)
		}

		// v2 schema: stamp successor_id + cause=handoff so the chain
		// resolver lands operators on the live tail.
		if err := oldRec.ArchiveWithHandoff(newRec.ID); err != nil {
			path, perr := state.AgentPath(oldRec.ID)
			if perr == nil {
				if rmErr := os.Remove(path); rmErr == nil {
					_, _ = fmt.Fprintf(stderr,
						"warning: archive %s: %v (live record removed instead)\n",
						oldRec.ID, err)
				} else {
					return fmt.Errorf(
						"resume: archive %s failed (%w) AND remove failed (%w); replacement %s spawned but old record stuck",
						oldRec.ID, err, rmErr, newRec.ID)
				}
			} else {
				return fmt.Errorf(
					"resume: archive %s failed (%w) AND could not resolve live path (%w); replacement %s spawned",
					oldRec.ID, err, perr, newRec.ID)
			}
		}
	}
	successorRec := newRec
	supersededSession := ""
	supersededID := ""
	if autoResume {
		var deliveredRec *agent.Record
		if gracefulRouted {
			// The doc already landed on the lock winner inside the barrier
			// completion — a second send here would be a duplicate prompt.
			deliveredRec = gracefulWinner
		} else {
			var err error
			deliveredRec, err = deliverResumePrompt(oldRec.Project, isCoordSwap, leaseWrappedSuccessor, false,
				newRec, docPath, stdout, stderr)
			if err != nil {
				return fmt.Errorf("resume: prompt delivery pending for %s: %w (queue preserved at %s)",
					newRec.ID, err, queuePath)
			}
		}
		if deliveredRec != nil {
			successorRec = deliveredRec
		}
		if isCoordSwap && successorRec.ID != newRec.ID {
			if err := retargetArchivedHandoffSuccessor(oldRec.ID, successorRec.ID); err != nil {
				return fmt.Errorf("resume: prompt delivered to lock owner %s but handoff archive retarget failed: %w (queue preserved at %s)",
					successorRec.ID, err, queuePath)
			}
			// Defer the superseded-record drop until AFTER queue.Delete: if the
			// delete fails the queue still names newRec, so a recovery pass must
			// be able to reload it (codex P2). Mirrors cmd/fleet/handoff.go.
			supersededSession = newRec.TmuxSession
			supersededID = newRec.ID
		}
	}
	if err := queue.Delete(queuePath); err != nil {
		return fmt.Errorf(
			"resume: %s archived but queue file delete failed; rerun fleet drain (or fleet handoff) to deliver the resume prompt: %w",
			oldRec.ID, err)
	}
	if supersededID != "" {
		if err := DropReplacementRecord(supersededSession, supersededID, stderr); err != nil {
			return fmt.Errorf("resume: prompt delivered to lock owner %s but superseded replacement %s cleanup failed: %w",
				successorRec.ID, supersededID, err)
		}
	}

	_, _ = fmt.Fprintf(stdout, "drained %s → %s\n", oldRec.ID, successorRec.ID)
	_, _ = fmt.Fprintf(stdout, "  task:    %s\n", successorRec.TaskID)
	_, _ = fmt.Fprintf(stdout, "  project: %s\n", successorRec.Project)
	_, _ = fmt.Fprintf(stdout, "  tmux:    %s\n", successorRec.TmuxSession)
	_, _ = fmt.Fprintf(stdout, "  handoff: %s\n", docPath)
	_, _ = fmt.Fprintf(stdout, "  number:  %d (was %d)\n",
		successorRec.HandoffNumber, oldRec.HandoffNumber)
	return nil
}
