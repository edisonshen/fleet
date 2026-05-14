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

// sessionListProbe is the tmux-session enumerator the auto-drain path
// uses for the FLEET_MAX_SESSIONS precheck. Indirected via a package-
// level var so tests inject a fake without touching tmux.
var sessionListProbe = tmux.ListSessions

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
		if dropErr := DropReplacementRecord(newRec.TmuxSession, newRec.ID, stderr); dropErr != nil {
			return fmt.Errorf(
				"resume: stale replacement %s session %s appeared dead but cleanup failed (%w); spawn fresh aborted",
				newRec.ID, newRec.TmuxSession, dropErr)
		}
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
	counts, cerr := state.CountFleetSessions(
		sessionListProbe, state.LiveAgentRecordExists)
	if cerr != nil {
		_, _ = fmt.Fprintf(stderr,
			"warning: FLEET_MAX_SESSIONS precheck could not enumerate tmux sessions (%v); proceeding without cap enforcement\n",
			cerr)
	} else if max := state.MaxSessions(stderr); counts.Total() >= max {
		// Queue file is preserved so the operator can drain manually
		// after pruning. The error message mirrors the CLI gate's
		// language for consistency.
		return fmt.Errorf(
			"resume: refusing to spawn — already at FLEET_MAX_SESSIONS=%d tmux sessions (%d live, %d orphan); run `fleet maintenance prune-orphan-tmux --kill` or `fleet rm <id>`, then rerun `fleet drain` (queue file %s preserved)",
			max, counts.Live, counts.Orphan, queuePath)
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
	rcSessionName := "fleet-handoff-" + req.NewAgentID
	rewrittenExecArgv := spawn.InjectRemoteControlFlag(oldRec.Command, rcSessionName)
	if spawn.SameCommand(rewrittenExecArgv, oldRec.Command) {
		rewrittenExecArgv = nil
	}

	newRec, err := spawn.Spawn(spawn.Options{
		OldRecord:      oldRec,
		NewDocPath:     req.HandoffDoc,
		Cwd:            oldRec.Cwd,
		Command:        oldRec.Command,
		ExecCommand:    rewrittenExecArgv,
		PreAllocatedID: req.NewAgentID,
		// Use disableAutoResume (explicit opt-out only), not
		// thisHandoffDisableAutoResume (which includes the v1
		// legacy case). A v1 drain shouldn't permanently flip the
		// new record's baseline to opt-out (codex iter-18 P2).
		DisableAutoResume: disableAutoResume,
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

	// Coord-spawn marker transfer. The marker is the dashboard's
	// gate for "this agent is this project's coord" (see
	// internal/state/state.go ReadCoordSpawnMarker). When a coord
	// agent handoffs, the OLD agent ID gets archived but the marker
	// still points at it — the TUI's [a] keystroke then can't find a
	// live coord and spawns a duplicate. Re-point the marker at the
	// replacement here, AFTER spawn confirmed alive and BEFORE
	// retireOldAgent archives oldRec. Workers and other non-coord
	// agents leave the marker untouched (the wantID match below
	// gates this strictly on "old was the project's coord").
	//
	// Best-effort: marker errors print a warning but don't fail the
	// drain — the marker is a UX gate, not a load-bearing security
	// boundary (per ReadCoordSpawnMarker's doc).
	if oldRec.Project != "" {
		if wantID := state.ReadCoordSpawnMarker(oldRec.Project); wantID == oldRec.ID {
			if werr := state.WriteCoordSpawnMarker(oldRec.Project, newRec.ID); werr != nil {
				_, _ = fmt.Fprintf(stderr,
					"warning: coord-spawn marker update for project %s failed: %v\n",
					oldRec.Project, werr)
			}
		}
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
			// session with no fleet record). DropReplacementRecord pairs
			// the kill + remove so a probe-failure window can't drop the
			// record while the new session is still alive
			// (fix/orphan-tmux-sweeper-and-leak-plug). On cleanup error
			// we surface the both-alive variant of the message so the
			// operator triages both stuck sessions manually.
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
