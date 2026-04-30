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
	"github.com/edisonshen/fleet/internal/handoff"
	"github.com/edisonshen/fleet/internal/queue"
	"github.com/edisonshen/fleet/internal/spawn"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tmux"
)

// DefaultGraceMillis is the default delay between /exit and Kill. Matches
// cmd/fleet/handoff.go's default. Drain uses this directly; the TUI [h]
// handoff path may override.
const DefaultGraceMillis = 3000

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
	if !tmux.HasSession(newRec.TmuxSession) {
		if path, perr := state.AgentPath(newRec.ID); perr == nil {
			_ = os.Remove(path)
		}
		_, _ = fmt.Fprintf(stderr,
			"note: stale replacement %s (session %s already exited) cleaned up; spawning fresh replacement\n",
			newRec.ID, newRec.TmuxSession)
		return spawnAndRetire(req, queuePath, oldRec, graceMillis, stdout, stderr)
	}
	// Resolve THIS handoff's auto-resume policy from queue override
	// + oldRec baseline (codex review iter-12 P2).
	thisHandoffDisable := oldRec.DisableAutoResume
	if req.DisableAutoResume != nil {
		thisHandoffDisable = *req.DisableAutoResume
	}
	return retireOldAgent(oldRec, newRec, req.HandoffDoc, queuePath,
		thisHandoffDisable, graceMillis, stdout, stderr)
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
	if autoResume {
		if err := spawn.WaitForReadyToPrompt(newRec.TmuxSession); err != nil {
			_, _ = fmt.Fprintf(stdout,
				"warning: readiness poll for %s did not converge: %v (sending anyway)\n",
				newRec.TmuxSession, err)
		}
		// SessionAlive (not HasSession) so transport probe failures
		// don't fail the recovery (codex iter-15 P1).
		if alive, perr := tmux.SessionAlive(newRec.TmuxSession); perr != nil {
			_, _ = fmt.Fprintf(stdout,
				"warning: post-readiness probe for %s failed: %v (proceeding anyway)\n",
				newRec.TmuxSession, perr)
		} else if !alive {
			return fmt.Errorf(
				"resume: agent %s already archived BUT replacement %s tmux session %s exited during readiness wait — task has no live agent",
				req.OldAgentID, req.NewAgentID, newRec.TmuxSession)
		}
	}
	if err := queue.Delete(queuePath); err != nil {
		_, _ = fmt.Fprintf(stdout,
			"warning: agent %s already handed off → %s but queue cleanup failed: %v\n",
			req.OldAgentID, req.NewAgentID, err)
		return nil
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
	// The new record's baseline always inherits from oldRec — the
	// override is one-shot.
	thisHandoffDisableAutoResume := oldRec.DisableAutoResume
	if req.DisableAutoResume != nil {
		thisHandoffDisableAutoResume = *req.DisableAutoResume
	}

	// Reject fresh-spawn auto-handoff for opt-out agents (codex
	// review iter-9 P1, scoped to spawnAndRetire per iter-10 P2).
	// We're about to bring up a NEW agent that won't get a resume
	// prompt — and there's no operator on this drain path to type
	// one manually. Spawning would leave the replacement idle
	// forever. Surface a clear error pointing at `fleet handoff`,
	// the interactive path. Queue file preserved for that retry.
	//
	// Already-spawned recovery (case 3 in Resume) takes the
	// retireOldAgent branch directly, NOT this one — there the
	// replacement was spawned by an operator who already accepted
	// the manual-prompt requirement, so we just complete the retire
	// without sending.
	if thisHandoffDisableAutoResume {
		return fmt.Errorf(
			"resume: agent %s opted out of auto-resume; auto-handoff would leave the replacement idle. Trigger handoff manually with `fleet handoff %s` (queue file %s preserved)",
			req.OldAgentID, req.OldAgentID, queuePath)
	}
	newRec, err := spawn.Spawn(spawn.Options{
		OldRecord:      oldRec,
		NewDocPath:     req.HandoffDoc,
		Cwd:            oldRec.Cwd,
		Command:        oldRec.Command,
		PreAllocatedID: req.NewAgentID,
		// Persist resolved override so the new record's baseline
		// reflects the operator's choice (codex review iter-13 P2).
		DisableAutoResume: thisHandoffDisableAutoResume,
	})
	if err != nil {
		return fmt.Errorf("resume: spawn replacement: %w", err)
	}
	if !tmux.HasSession(newRec.TmuxSession) {
		if path, perr := state.AgentPath(newRec.ID); perr == nil {
			_ = os.Remove(path)
		}
		return fmt.Errorf(
			"resume: replacement %s spawned but tmux session %s already exited (command crashed at startup?); old agent untouched, queue file preserved for retry",
			newRec.ID, newRec.TmuxSession)
	}
	return retireOldAgent(oldRec, newRec, req.HandoffDoc, queuePath,
		thisHandoffDisableAutoResume, graceMillis, stdout, stderr)
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
	graceMillis int, stdout, stderr io.Writer) error {

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
		return fmt.Errorf(
			"resume: replacement %s tmux session %s exited during readiness wait; old agent %s untouched, retry handoff",
			newRec.ID, newRec.TmuxSession, oldRec.ID)
	}

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
			// Old still alive after Kill — roll back the new agent ONLY
			// if the new session is also gone (don't strand a live tmux
			// session with no fleet record).
			_ = tmux.Kill(newRec.TmuxSession)
			if !tmux.HasSession(newRec.TmuxSession) {
				if path, perr := state.AgentPath(newRec.ID); perr == nil {
					_ = os.Remove(path)
				}
				_ = queue.Delete(queuePath)
				return fmt.Errorf(
					"resume: old session %s still alive after kill failure: %w (replacement %s rolled back; investigate)",
					oldRec.TmuxSession, err, newRec.ID)
			}
			return fmt.Errorf(
				"resume: old session %s AND new session %s both alive after kill failure: %w (replacement %s record preserved; clean up both manually)",
				oldRec.TmuxSession, newRec.TmuxSession, err, newRec.ID)
		}
		_, _ = fmt.Fprintf(stderr,
			"note: kill %s reported error but session is gone: %v\n",
			oldRec.TmuxSession, err)
	}

	if err := oldRec.Archive(); err != nil {
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
	queueDeleted := true
	if err := queue.Delete(queuePath); err != nil {
		_, _ = fmt.Fprintf(stderr, "warning: delete queue file: %v\n", err)
		queueDeleted = false
	}

	// Send the resume prompt only when queue.Delete succeeded so
	// the retry doesn't re-send via cleanUpStaleQueue (codex review
	// iter-11 P3). On queue.Delete failure, the next retry will
	// recover via cleanUpStaleQueue (or the cmd/fleet equivalent),
	// which has its own send + delete pair. On SEND failure with
	// queue already deleted, re-enqueue so cleanUpStaleQueue can
	// retry — preserves recovery for non-interactive drains where
	// no operator can type the prompt manually (codex iter-13 P2).
	if queueDeleted && autoResume {
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
			}); werr != nil {
				_, _ = fmt.Fprintf(stderr,
					"warning: re-enqueue after send failure: %v (replacement may need manual prompt on attach)\n",
					werr)
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
