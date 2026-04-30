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

	// 1a. Reject auto-handoff for opt-out agents (codex review iter-9
	//     P1). The auto-handoff drain is non-interactive: there's no
	//     operator to type the manual prompt that --no-auto-resume
	//     agents need. Silently completing the handoff would leave
	//     the replacement idle forever. Surface a clear error so the
	//     operator must trigger handoff manually via `fleet handoff`,
	//     where they can attach + type the first prompt.
	//
	//     In practice this is rare — fleet-guard, the producer of
	//     auto-handoff queue files, only runs inside claude code's
	//     hook system, so a non-claude wrapper wouldn't generate
	//     these queue files in the first place. But the check is
	//     cheap belt-and-suspenders.
	if oldRec.DisableAutoResume {
		return fmt.Errorf(
			"resume: agent %s has --no-auto-resume; auto-handoff would leave the replacement idle. Trigger handoff manually with `fleet handoff %s` (queue file %s preserved)",
			req.OldAgentID, req.OldAgentID, queuePath)
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
	return retireOldAgent(oldRec, newRec, req.HandoffDoc, queuePath,
		graceMillis, stdout, stderr)
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
	if err := queue.Delete(queuePath); err != nil {
		_, _ = fmt.Fprintf(stdout,
			"warning: agent %s already handed off → %s but queue cleanup failed: %v\n",
			req.OldAgentID, req.NewAgentID, err)
		return nil
	}
	_, _ = fmt.Fprintf(stdout,
		"agent %s already handed off → %s (cleaned stale queue file)\n",
		req.OldAgentID, req.NewAgentID)
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
	newRec, err := spawn.Spawn(spawn.Options{
		OldRecord:      oldRec,
		NewDocPath:     req.HandoffDoc,
		Cwd:            oldRec.Cwd,
		Command:        oldRec.Command,
		PreAllocatedID: req.NewAgentID,
		// Auto-handoff inherits the policy verbatim — there's no
		// operator override on this path (drain is non-interactive).
		DisableAutoResume: oldRec.DisableAutoResume,
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
		graceMillis, stdout, stderr)
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
	graceMillis int, stdout, stderr io.Writer) error {

	autoResume := !newRec.DisableAutoResume

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
	if !tmux.HasSession(newRec.TmuxSession) {
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
	if err := queue.Delete(queuePath); err != nil {
		_, _ = fmt.Fprintf(stderr, "warning: delete queue file: %v\n", err)
	}

	// Send the resume prompt AFTER queue.Delete. Pure send-keys —
	// no waits, microsecond-scale. The readiness wait already ran
	// above (recoverable via the queue file). Now the queue is gone
	// so this send happens at most once per logical handoff.
	if autoResume {
		if err := spawn.SendPromptKeys(newRec.TmuxSession,
			handoff.ResumePrompt(docPath)); err != nil {
			_, _ = fmt.Fprintf(stderr,
				"warning: send resume prompt to %s: %v (replacement may need manual prompt on attach)\n",
				newRec.TmuxSession, err)
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
