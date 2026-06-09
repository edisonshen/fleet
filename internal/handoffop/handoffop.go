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
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/coordlock"
	"github.com/edisonshen/fleet/internal/coordrepo"
	"github.com/edisonshen/fleet/internal/handoff"
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

// writeMarkerFn is the seam for the v0.12.2 P0 drain-path RC marker
// backfill (DESIGN-coord-spawn-atomic-gate.md). Production wires
// rc.WriteMarker; tests stub to count invocations and exercise the
// non-fatal-on-failure contract pinned by
// TestDrain_CoordHandoff_MarkerWriteFailure_NonFatal.
//
// Mirrors cmd/fleet/dispatch.go's writeMarkerFn seam: the handoffop
// package can't import cmd/fleet (would cycle), so each package
// carries its own seam wired to the same rc.WriteMarker production
// implementation.
//
// Closes PR #163's deferred [P2] (2): cmd/fleet/handoff.go (PR #163)
// writes the marker before the inject for operator-triggered handoffs;
// internal/handoffop/handoffop.go (this seam) does the same for
// drain-path (auto-handoff + crash recovery) handoffs so pre-v0.12.1
// coords get RC backfilled on their next handoff.
var writeMarkerFn = rc.WriteMarker

// isCoordHandoffForAgent reports whether (project, agentID) identifies
// the project's current coord — i.e. the coord-spawn marker resolves
// to agentID. spawnAndRetire calls this to gate the v0.12.1 RC inject
// (codex review iter-7 [P1]): worker handoffs that happen during
// fleet drain / crash recovery must NOT silently inherit
// --remote-control just because the project is RC-enabled — that
// would reopen the v0.12 push-storm class for the auto-drained
// successor agent.
//
// Mirrors cmd/fleet/handoff.go's isCoordHandoffForProject (same
// predicate, package-local copy to keep the handoffop package self-
// contained without importing cmd/fleet/main).
func isCoordHandoffForAgent(project, agentID string) bool {
	if project == "" {
		return false
	}
	return state.ReadCoordSpawnMarker(project) == agentID
}

func refuseLeaseWrappedCoordHandoffRetire(oldRec, newRec *agent.Record, stderr io.Writer) error {
	RollbackCoordMarkerIfPointingAt(oldRec.Project, oldRec.ID, newRec.ID, stderr)
	if dropErr := DropReplacementRecord(newRec.TmuxSession, newRec.ID, stderr); dropErr != nil {
		return fmt.Errorf(
			"resume: coord handoff for project %q spawned standby successor %s but PR1 cannot verify its child before retiring old coord %s; cleanup failed: %w (old agent untouched, queue file preserved for retry after PR2 lock-coupled delivery)",
			oldRec.Project, newRec.ID, oldRec.ID, dropErr)
	}
	return fmt.Errorf(
		"resume: coord handoff for project %q spawned standby successor %s but PR1 cannot verify its child before retiring old coord %s; old agent untouched, replacement dropped, queue file preserved for retry after PR2 lock-coupled delivery",
		oldRec.Project, newRec.ID, oldRec.ID)
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
		// Codex iter-14 [P1]: AtomicCoordSwap preserves the queue
		// journal on ErrOrphanSurvived / ErrOldKillProbeAmbiguous —
		// the cases where NEW may have lost the coordinator.lock race
		// + exited, while OLD is still the live coord. On the next
		// drain / watcher pass we land here with newRec session
		// definitively dead. Auto-respawning would put a SECOND
		// replacement on top of a still-alive OLD, recreating the
		// duplicate-coord loop the preserved queue is meant to avoid.
		//
		// Codex iter-15 [P1] narrowing: refuse ONLY when marker is at
		// newRec.ID (post-commit), not at oldRec.ID (pre-commit).
		//
		//   - marker == newRec.ID → AtomicCoordSwap's step 4 succeeded
		//     and step 5 returned ErrOrphanSurvived / ErrOldKillProbeAmbiguous.
		//     The swap "committed" — NEW briefly owned by the marker —
		//     and OLD's record was preserved (helper iter-7 P1 stopped
		//     archiving OLD on these paths). If OLD's tmux is still
		//     alive (orphan or live coord on another socket), respawning
		//     here would put a NEW2 competing for coordinator.lock with
		//     the OLD orphan that's still holding it.
		//
		//   - marker == oldRec.ID → previous handoff crashed BEFORE the
		//     atomic commit (no eager write yet, or eager write failed,
		//     or the helper rolled the marker back). OLD is still the
		//     only legitimate coord. Respawning is the correct recovery:
		//     drop NEW, fresh-spawn a replacement, normal swap path
		//     completes the handoff.
		//
		// Codex iter-18 [P1] (deferred — known limitation, conservative
		// stance): The eager marker write in spawnAndRetire (search
		// "Eager marker write — closes the duplicate-coord window")
		// moves marker → newRec.ID BEFORE retireOldAgent invokes
		// AtomicCoordSwap. A crash in that pre-commit window leaves
		// marker == newRec.ID with no real commit having happened, and
		// this branch treats it as post-commit (refuses auto-respawn
		// instead of dropping the dead NEW + retrying the swap).
		// Distinguishing pre-commit-eager-write from post-commit-real-
		// commit on disk requires a separate "swap-committed" sentinel
		// written only by AtomicCoordSwap step 4 — a structural addition
		// deferred to a follow-up PR. Current behavior errs conservative
		// (preserves the queue, surfaces refusal to operator) rather
		// than risk auto-respawning into a real ErrOrphanSurvived state.
		// Operator workaround: `--force-replacement` on `fleet handoff`,
		// or manually rewriting the marker to oldRec.ID before retry.
		coordSwapPostCommit := false
		if oldRec.Project != "" &&
			state.ReadCoordSpawnMarker(oldRec.Project) == newRec.ID {
			coordSwapPostCommit = true
		}
		if coordSwapPostCommit {
			// Codex iter-17 [P1] (deferred — known scope limitation):
			// tmuxSessionAliveFn only probes the CURRENT
			// FLEET_TMUX_SOCKET. (alive=false, err=nil) here means
			// "not on our socket," not "globally dead." If OLD's coord
			// was always on a different tmux socket, we fall through
			// and respawn — creating a duplicate coord. Distinguishing
			// requires persisting OLD's originating socket on
			// agent.Record (schema change cross-cutting spawn + state
			// + every probe site); deferred to a follow-up PR per the
			// PR #149 cross-socket cap precedent. Single-socket fleet
			// (the default) is correct here.
			oldAlive, probeErr := tmuxSessionAliveFn(oldRec.TmuxSession)
			switch {
			case probeErr != nil:
				// Ambiguous OLD probe — refuse rather than risk
				// respawning into a live coord. Queue stays so the
				// next pass (or operator's explicit `fleet handoff`)
				// can retry once the probe is reliable.
				return fmt.Errorf(
					"resume: replacement %s session %s appears dead but probing old coord %s session %s failed: %w (queue preserved; investigate manually)",
					newRec.ID, newRec.TmuxSession, oldRec.ID, oldRec.TmuxSession, probeErr)
			case oldAlive:
				// OLD coord is still alive — typically because the
				// previous AtomicCoordSwap returned ErrOrphanSurvived
				// or ErrOldKillProbeAmbiguous and preserved OLD.
				// Auto-respawning here would compound the problem
				// with a second replacement competing for the same
				// coordinator.lock. Refuse, leave queue intact, surface
				// the orphan to the operator.
				return fmt.Errorf(
					"resume: replacement %s session %s exited but old coord %s session %s is still alive — refusing auto-respawn (would create duplicate coord); kill the orphan replacement or the old coord manually, then re-run fleet drain (queue preserved at %s)",
					newRec.ID, newRec.TmuxSession, oldRec.ID, oldRec.TmuxSession, queuePath)
			}
			// OLD is definitively dead — safe to drop NEW and spawn
			// fresh. Falls through to DropReplacementRecord + rollback
			// + spawnAndRetire below.
		}
		if dropErr := DropReplacementRecord(newRec.TmuxSession, newRec.ID, stderr); dropErr != nil {
			return fmt.Errorf(
				"resume: stale replacement %s session %s appeared dead but cleanup failed (%w); spawn fresh aborted",
				newRec.ID, newRec.TmuxSession, dropErr)
		}
		// Codex iter-12 [P1]: a prior coord-swap attempt may have
		// committed marker → newRec.ID (eager write before retire,
		// or the AtomicCoordSwap commit point) and then returned
		// ErrOrphanSurvived / ErrOldKillProbeAmbiguous / similar,
		// leaving the marker pointing at the replacement we just
		// dropped. The fall-through to spawnAndRetire's isCoordSwap
		// check only matches marker == oldRec.ID, so without this
		// rollback the retry's swap detection misses, the inline
		// retire path runs (no AtomicCoordSwap), and `[a]` can spawn
		// a duplicate coord because the marker still names the dead
		// agent. Idempotent if marker is already at oldRec.ID.
		RollbackCoordMarkerIfPointingAt(oldRec.Project, oldRec.ID, newRec.ID, stderr)
		_, _ = fmt.Fprintf(stderr,
			"note: stale replacement %s (session %s already exited) cleaned up; spawning fresh replacement\n",
			newRec.ID, newRec.TmuxSession)
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
	// Coord-swap detection for the case-3 resume path (replacement
	// already spawned + alive). isCoordSwap fires when the marker
	// resolves to either oldRec.ID (previous run crashed BEFORE
	// commit) or newRec.ID (previous run committed but crashed during
	// retire — still need atomic kill+archive bookkeeping). The
	// helper's step 1.b accepts both states (codex iter-9 [P1]).
	//
	// On marker==oldRec.ID, eager-write the marker → newRec.ID before
	// retireOldAgent so the readiness wait doesn't open a duplicate-
	// coord window.
	isCoordSwap := false
	if oldRec.Project != "" {
		switch state.ReadCoordSpawnMarker(oldRec.Project) {
		case oldRec.ID:
			isCoordSwap = true
			if werr := state.WriteCoordSpawnMarker(oldRec.Project, newRec.ID); werr != nil {
				_, _ = fmt.Fprintf(stderr,
					"warning: eager coord-spawn marker update for project %s failed: %v (retireOldAgent will retry inside AtomicCoordSwap)\n",
					oldRec.Project, werr)
			}
		case newRec.ID:
			isCoordSwap = true
		}
	}
	return retireOldAgent(oldRec, newRec, req.HandoffDoc, queuePath,
		thisHandoffDisable, graceMillis, isCoordSwap, stdout, stderr)
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
	if !tmux.HasSession(newRec.TmuxSession) {
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
	//
	// Resolve auto-resume from queue override + newRec baseline
	// (codex review iter-12 P2). Gate on schema v2+ (codex
	// iter-15 P2) — v1 queue files predate this feature.
	disableAutoResume := newRec.DisableAutoResume
	if req.DisableAutoResume != nil {
		disableAutoResume = *req.DisableAutoResume
	}
	autoResume := !disableAutoResume && req.SchemaVersion >= 2

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
	} else if !alive {
		return fmt.Errorf(
			"resume: agent %s already archived BUT replacement %s tmux session %s exited during readiness wait — task has no live agent",
			req.OldAgentID, req.NewAgentID, newRec.TmuxSession)
	}
	if err := queue.Delete(queuePath); err != nil {
		// Return error so fleet drain / TUI watcher retries; under
		// the new post-delete send order the prompt would never have
		// been sent if the delete failed, so silently reporting
		// success would leave the replacement idle (codex review
		// iter-18 P2).
		return fmt.Errorf(
			"resume: agent %s already handed off → %s but queue cleanup failed (%w); will retry",
			req.OldAgentID, req.NewAgentID, err)
	}
	if autoResume {
		if err := spawn.SendPromptKeys(newRec.TmuxSession,
			handoff.ResumePrompt(req.HandoffDoc)); err != nil {
			_, _ = fmt.Fprintf(stdout,
				"warning: send resume prompt to %s after archive-recovery: %v (re-enqueuing for retry)\n",
				newRec.TmuxSession, err)
			// Re-enqueue so a future drain / `fleet handoff` can
			// retry delivery — without this, send failure on the
			// non-interactive drain path silently strands the
			// replacement (codex review iter-14 P1).
			if _, werr := queue.WriteSpawnFresh(req); werr != nil {
				_, _ = fmt.Fprintf(stdout,
					"warning: re-enqueue after archive-recovery send failure: %v\n",
					werr)
			}
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
	// codex round-6 P1: post-v0.12 only one listener prefix exists
	// (`fleet-coord`, started by `fleet rc up`). The legacy per-
	// handoff `fleet-handoff-<project>` daemon went away when the
	// S2/S3 gates replaced the embedded bash bootstrap with operator-
	// instruction markdown. Injecting "fleet-handoff-..." into the
	// replacement coord would point at a prefix the live listener
	// can't see → silent pairing failure post-auto-handoff. Mirror
	// cmd/fleet/handoff.go: use the coord session-name shape.
	rcSessionName := spawn.CoordRemoteControlSessionName(req.NewAgentID, oldRec.Project)
	// v0.12 (DESIGN §"Attach-surface gates" I3): use the project-aware
	// rc.GateAttachFlag helper. Auto-handoff drain hits this code path
	// without going through cmd/fleet's wrapper, so the dedicated
	// helper carries the same FLEET_RC_BOOTSTRAP_DISABLED + rc.Enabled
	// two-layer gate (rather than re-implementing it here).
	//
	// v0.12.1 codex review iter-7 [P1]: ALSO gate on isCoordHandoff
	// — the project-wide rc-enabled marker is now auto-written by
	// coord-spawn (DESIGN-rc-coord-auto-marker.md), so worker handoffs
	// resumed via fleet drain / crash recovery would silently inherit
	// --remote-control after any coord in the same project triggered
	// the auto-write. The strict opt-in carve-out for workers /
	// subagents (v0.12 push-storm protection) requires gating EVERY
	// RC inject site that runs during a handoff, not just the marker
	// write site. Mirrors cmd/fleet/handoff.go and dispatch.go.
	//
	// The predicate matches the post-spawn isCoordSwap detector at
	// line ~602 below — same fact (coord-spawn marker resolves to
	// oldRec.ID), used at two points (pre-inject + post-spawn marker
	// transfer). Extracted as isCoordHandoffForAgent for unit-testable
	// gate coverage without driving full spawnAndRetire.
	var rewrittenExecArgv []string
	if isCoordHandoffForAgent(oldRec.Project, oldRec.ID) {
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

		// v0.12.2 P0 (DESIGN-coord-spawn-atomic-gate.md Change 6;
		// closes PR #163 deferred [P2] (2)): backfill the rc-enabled
		// marker for the project BEFORE the inject so a pre-v0.12.1
		// coord whose project never had the marker written still
		// gets RC on its drain-path replacement. Without this, the
		// rc.Enabled(project) gate inside rc.GateAttachFlag returns
		// false → inject no-ops → the replacement coord boots
		// without --remote-control, breaking mobile / claude.ai
		// pairing for the operator.
		//
		// Mirrors cmd/fleet/handoff.go's marker-write-before-inject
		// pattern (PR #163 line 768). Failure is non-fatal: log a
		// warning to stderr and continue — the inject will then
		// no-op gracefully (the gate's other half), and the
		// operator can recover via `fleet rc up <project>`.
		if err := writeMarkerFn(oldRec.Project); err != nil {
			_, _ = fmt.Fprintf(stderr,
				"warning: rc.WriteMarker(%q) failed during drain handoff: %v "+
					"(continuing with plain claude argv; run `fleet rc up %s` to recover)\n",
				oldRec.Project, err, oldRec.Project)
		}
		rewrittenExecArgv = rc.GateAttachFlag(oldRec.Project, oldRec.Command, rcSessionName)
		if spawn.SameCommand(rewrittenExecArgv, oldRec.Command) {
			rewrittenExecArgv = nil
		}
	}

	// COORD drain handoffs resolve the repo binding via the shared
	// resolver, NOT the old coord's stored Cwd (DESIGN-coord-repo-
	// binding-from-project.md PR3). Same rationale as the manual
	// cmd/fleet/handoff.go path: a wrong-repo coord must not hand off
	// into the same wrong tree. Resolve fresh and REFUSE on an
	// unresolvable project. persist=true (handoff is operator-initiated).
	// Worker handoffs keep inheriting oldRec.Cwd.
	spawnCwd := oldRec.Cwd
	if spawn.IsCoordSpawn(oldRec.TaskID, oldRec.Project) {
		resolved, rerr := coordrepo.ResolveProjectRepo(oldRec.Project, true)
		if rerr != nil {
			return fmt.Errorf("resume: coord handoff for project %q: %w", oldRec.Project, rerr)
		}
		spawnCwd = resolved
	}

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

	// Coord-spawn marker transfer in two passes (codex iter-1 [P1] +
	// iter-9 [P1]):
	//
	//   1. DETECT isCoordSwap by reading the marker BEFORE we change
	//      it. Pass through to retireOldAgent so its isCoordSwap branch
	//      fires regardless of subsequent marker reads.
	//
	//   2. EAGERLY write marker → newRec.ID BEFORE retireOldAgent's
	//      readiness wait. Without this the marker stays at oldRec.ID
	//      during NEW's boot. If OLD exits during that window, the
	//      TUI's [a] path can't find NEW (record.ID != marker) and
	//      spawns a duplicate coord.
	//
	// AtomicCoordSwap's step 1.b accepts EITHER marker==oldRec.ID OR
	// marker==newRec.ID (when AlreadySpawnedNewRec is set), so the
	// eager write is idempotent w.r.t. the helper's commit contract.
	// The helper's step 4 also skips a redundant rename when
	// markerAtNew is already true.
	//
	// Workers and other non-coord agents go through retireOldAgent's
	// inline path unchanged — the v0.2 worker-swap transactional
	// refactor is deferred per the v5 plan's Non-goals.
	isCoordSwap := false
	if oldRec.Project != "" && state.ReadCoordSpawnMarker(oldRec.Project) == oldRec.ID {
		isCoordSwap = true
		// Eager marker write — closes the duplicate-coord window
		// during NEW's readiness wait. Best-effort; marker errors
		// print a warning but don't fail the drain. retireOldAgent's
		// AtomicCoordSwap call will re-commit (idempotent) or
		// commit fresh, whichever applies.
		if werr := state.WriteCoordSpawnMarker(oldRec.Project, newRec.ID); werr != nil {
			_, _ = fmt.Fprintf(stderr,
				"warning: eager coord-spawn marker update for project %s failed: %v (retireOldAgent will retry inside AtomicCoordSwap)\n",
				oldRec.Project, werr)
		}
	}

	return retireOldAgent(oldRec, newRec, req.HandoffDoc, queuePath,
		thisHandoffDisableAutoResume, graceMillis, isCoordSwap, stdout, stderr)
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
func retireOldAgent(oldRec, newRec *agent.Record, docPath, queuePath string,
	disableAutoResume bool,
	graceMillis int, isCoordSwap bool, stdout, stderr io.Writer) error {

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
	//
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
		// Codex iter-11 [P1]: the caller (spawnAndRetire / Resume
		// case-3) may have eagerly moved the marker to newRec.ID
		// before invoking us. NEW is now dead — restore the marker
		// to oldRec.ID so dashboard discovery + a retry's
		// isCoordSwap detection keep working.
		if isCoordSwap && oldRec.Project != "" && state.ReadCoordSpawnMarker(oldRec.Project) == newRec.ID {
			if werr := state.WriteCoordSpawnMarker(oldRec.Project, oldRec.ID); werr != nil {
				_, _ = fmt.Fprintf(stderr,
					"warning: rollback coord-spawn marker for project %s failed: %v (operator may need to re-write manually)\n",
					oldRec.Project, werr)
			}
		}
		return fmt.Errorf(
			"resume: replacement %s tmux session %s exited during readiness wait; old agent %s untouched, retry handoff",
			newRec.ID, newRec.TmuxSession, oldRec.ID)
	}

	// Coord swap fast path. When OLD is the project's coordinator
	// (caller-supplied isCoordSwap from spawnAndRetire's pre-flight
	// marker check — codex iter-1 [P1] fix: doing the check HERE
	// would always see the marker already at newRec.ID because the
	// pre-helper marker write moved it before this function ran),
	// the retire sequence runs through the transactional
	// AtomicCoordSwap helper instead of the inline marker-flip +
	// /exit + grace + Kill + Archive sequence. This collapses the
	// four observable writes into one atomic commit point at the
	// marker rename (the v5 plan's load-bearing invariant). Worker
	// handoffs fall through to the inline path.
	//
	// IMPORTANT: the spawnAndRetire caller has already (a) written
	// the NEW agent record and (b) probed NEW alive. AtomicCoordSwap's
	// AlreadySpawnedNewRec entrypoint skips its own spawn + probe
	// steps and proceeds straight to the marker commit.
	if isCoordSwap {
		if leaseFailoverEnabled() {
			return refuseLeaseWrappedCoordHandoffRetire(oldRec, newRec, stderr)
		}
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
	queueDeleted := true
	if err := queue.Delete(queuePath); err != nil {
		_, _ = fmt.Fprintf(stderr, "warning: delete queue file: %v\n", err)
		queueDeleted = false
	}

	// If queue.Delete failed, surface as error so drain reports
	// the handoff as not-yet-complete (codex review iter-20 P1).
	// Old is already archived, so a retry will reach
	// cleanUpStaleQueue, which has its own send + delete pair.
	// Returning nil here would silently strand the replacement
	// (queue still on disk, prompt never sent, drain reports
	// success).
	if !queueDeleted {
		return fmt.Errorf(
			"resume: %s archived but queue file delete failed; rerun fleet drain (or fleet handoff) to deliver the resume prompt",
			oldRec.ID)
	}

	// Send the resume prompt now that queue.Delete succeeded (we
	// returned early on failure above). On SEND failure, re-enqueue
	// so cleanUpStaleQueue can retry — preserves recovery for non-
	// interactive drains where no operator can type the prompt
	// manually (codex iter-13 P2).
	if autoResume {
		if err := spawn.SendPromptKeys(newRec.TmuxSession,
			handoff.ResumePrompt(docPath)); err != nil {
			_, _ = fmt.Fprintf(stderr,
				"warning: send resume prompt to %s: %v (re-enqueuing for retry)\n",
				newRec.TmuxSession, err)
			// Re-enqueue: oldRec is now archived, so a retry
			// will land in cleanUpStaleQueue, which sends + deletes.
			var override *bool
			if disableAutoResume != oldRec.DisableAutoResume {
				v := disableAutoResume
				override = &v
			}
			if _, werr := queue.WriteSpawnFresh(queue.SpawnFresh{
				OldAgentID:        oldRec.ID,
				HandoffDoc:        docPath,
				Project:           oldRec.Project,
				TaskID:            oldRec.TaskID,
				NewAgentID:        newRec.ID,
				NewSession:        newRec.TmuxSession,
				DisableAutoResume: override,
				// Spawn already happened on this drain pass, so the
				// cap was effectively approved — mark so future
				// retries don't re-check (codex iter-7 P1).
				CapApproved: true,
			}); werr != nil {
				// Send failed AND re-enqueue failed → replacement
				// is alive but un-prompted, no journal entry to
				// recover from. Surface as error so the drainer
				// reports failure instead of silent success
				// (codex review iter-19 P2).
				return fmt.Errorf(
					"resume: send prompt to %s failed (%w) AND re-enqueue failed (%w); replacement %s alive but idle, retry handoff manually",
					newRec.TmuxSession, err, werr, newRec.ID)
			}
		}
	}

	_, _ = fmt.Fprintf(stdout, "drained %s → %s\n", oldRec.ID, newRec.ID)
	_, _ = fmt.Fprintf(stdout, "  task:    %s\n", newRec.TaskID)
	_, _ = fmt.Fprintf(stdout, "  project: %s\n", newRec.Project)
	_, _ = fmt.Fprintf(stdout, "  tmux:    %s\n", newRec.TmuxSession)
	_, _ = fmt.Fprintf(stdout, "  handoff: %s\n", docPath)
	_, _ = fmt.Fprintf(stdout, "  number:  %d (was %d)\n",
		newRec.HandoffNumber, oldRec.HandoffNumber)
	return nil
}
