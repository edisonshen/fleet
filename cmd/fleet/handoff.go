package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/handoff"
	"github.com/edisonshen/fleet/internal/queue"
	"github.com/edisonshen/fleet/internal/spawn"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tmux"
)

// handoffOpts captures cobra-parsed flags + positional arg.
type handoffOpts struct {
	oldID       string
	cwd         string
	command     []string
	graceMillis int
}

func newHandoffCmd() *cobra.Command {
	opts := &handoffOpts{}
	cmd := &cobra.Command{
		Use:   "handoff <agent-id>",
		Short: "Hand off a running agent to a fresh replacement",
		Long: `handoff writes a handoff doc, spawns a fresh replacement
agent inheriting the same task/project, sends "/exit" to the outgoing
tmux session, waits a grace period, then kills the old session and
archives its record.

Week 4a (operator-triggered): the doc body is a stub with placeholders
in all five sections — the agent never received a HANDOFF REQUESTED
injection so we can't fill its view of the work. After spawn, fleet
auto-types a "Read your handoff doc at <path> and continue" prompt
into the new session so the replacement starts working without
operator intervention; ` + "`fleet attach`" + ` shows the result.

The new agent inherits TaskID, Project, Engine, Role, Mode from the
outgoing record and increments handoff_number by 1.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.oldID = args[0]
			return runHandoff(opts, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().StringVar(&opts.cwd, "cwd", "",
		"working directory for the replacement (default: outgoing agent's cwd)")
	cmd.Flags().StringSliceVar(&opts.command, "command", nil,
		"command to run inside the replacement tmux session (default: outgoing agent's command, or `claude`)")
	cmd.Flags().IntVar(&opts.graceMillis, "grace-ms", 3000,
		"milliseconds to wait between /exit and kill on the old session")
	return cmd
}

// runHandoff orchestrates the full B2 flow:
//
//	load → flock(agent) → reload-under-lock → write doc
//	→ write queue → spawn replacement → send /exit + grace
//	→ kill → archive → delete queue
//
// Concurrency: the per-agent flock bounds the entire critical
// section. Two concurrent `fleet handoff X` invocations serialize on
// step 2 (LockAgent), and the loser's step 3 (reload-under-lock)
// sees ErrNotFound (winner already archived) and bails — no
// double-spawn. Per-agent (not per-project) so concurrent handoffs
// on DIFFERENT agents in the same project run in parallel.
//
// Crash safety: the queue file (step 5) is the journal entry. If we
// crash between step 5 and step 11 (Delete), a future drainer (4b
// TUI background loop) sees a stale queue file pointing at an
// already-archived old_agent_id and skips it. In 4a there is no
// drainer; the file lingers as harmless residue until 4b ships.
func runHandoff(opts *handoffOpts, stdout, stderr io.Writer) error {
	if _, err := state.Bootstrap(); err != nil {
		return fmt.Errorf("bootstrap ~/.fleet: %w", err)
	}
	if err := tmux.Available(); err != nil {
		return err
	}

	// 1. Per-agent flock — acquired BEFORE any side effects so
	//    concurrent `fleet handoff X` invocations serialize from the
	//    earliest possible point. Per-agent (not per-project) so two
	//    handoffs on DIFFERENT agents in the same project run in
	//    parallel. Without this, two same-id callers would race the
	//    queue write + spawn and both spawn replacements.
	//
	//    LockAgent uses only opts.oldID (no project lookup needed),
	//    so we don't pre-Load — the agent might not exist yet, in
	//    which case the next step surfaces a clear error.
	release, err := state.LockAgent(opts.oldID)
	if err != nil {
		return fmt.Errorf("lock agent %s: %w", opts.oldID, err)
	}
	defer release()

	// 2. Crash-recovery probe: if a previous handoff for this agent
	//    crashed AFTER spawning the replacement but BEFORE deleting
	//    the queue file, the queue file's NewAgentID will be set.
	//    Resume by completing kill+archive+delete with the existing
	//    replacement instead of spawning a second one.
	//
	//    Distinguish state.ErrNotFound (the actual recovery case)
	//    from other Load errors (corrupted JSON, permission issue) —
	//    silently cleaning up the journal on a corrupted-record read
	//    would either mask broken state or trigger a double-spawn.
	pendingPath, _ := state.QueuePath(queue.SpawnFreshName(opts.oldID))
	pending, perr := queue.ReadSpawnFresh(pendingPath)
	switch {
	case errors.Is(perr, state.ErrNotFound):
		// No queue file — normal flow.
	case perr != nil:
		// Corrupted journal / newer schema / permission error. Don't
		// silently fall through to the spawn path — that's a
		// double-spawn risk. Abort so the operator can investigate.
		return fmt.Errorf("recovery probe: read queue file %s failed: %w", pendingPath, perr)
	case pending.NewAgentID != "":
		oldRec, lerr := agent.Load(opts.oldID)
		switch {
		case errors.Is(lerr, state.ErrNotFound):
			// Old record gone. Verify the replacement is actually
			// alive before declaring "already handed off"; if the
			// replacement also vanished, the task currently has NO
			// live agent and we must not silently exit success.
			newRec, nerr := agent.Load(pending.NewAgentID)
			switch {
			case errors.Is(nerr, state.ErrNotFound):
				return fmt.Errorf(
					"agent %s already archived BUT replacement %s record is gone — task has no live agent; clean up queue file %s manually after starting a new agent",
					opts.oldID, pending.NewAgentID, pendingPath)
			case nerr != nil:
				return fmt.Errorf("recovery probe: load replacement %s failed: %w", pending.NewAgentID, nerr)
			}
			if !tmux.HasSession(newRec.TmuxSession) {
				return fmt.Errorf(
					"agent %s already archived BUT replacement %s tmux session %s is gone — task has no live agent; investigate before deleting queue file %s",
					opts.oldID, pending.NewAgentID, newRec.TmuxSession, pendingPath)
			}
			// Old archived, replacement live — handoff truly
			// completed. Clean up the journal and report success.
			_ = queue.Delete(pendingPath)
			_, _ = fmt.Fprintf(stdout,
				"agent %s already handed off → %s (cleaned stale queue file)\n",
				opts.oldID, pending.NewAgentID)
			return nil
		case lerr != nil:
			// Read failure that isn't "missing" — abort so the
			// operator can investigate without us touching state.
			return fmt.Errorf("recovery probe: load agent %s failed: %w", opts.oldID, lerr)
		}
		newRec, nerr := agent.Load(pending.NewAgentID)
		switch {
		case errors.Is(nerr, state.ErrNotFound):
			// Replacement record vanished. Before re-spawning, check
			// whether the previously-recorded session is still alive
			// — if it is, the record was hand-deleted (or some other
			// out-of-band cleanup) and a fresh spawn would create a
			// duplicate live session. Refuse and let the operator
			// reconcile.
			if pending.NewSession != "" && tmux.HasSession(pending.NewSession) {
				return fmt.Errorf(
					"recovery probe: replacement %s record missing but tmux session %s still alive — refusing to spawn duplicate; clean up the orphan session or its queue file before retrying",
					pending.NewAgentID, pending.NewSession)
			}
			// Both record and session gone — true orphan journal.
			// Delete and proceed normally so a fresh spawn can happen.
			_ = queue.Delete(pendingPath)
		case nerr != nil:
			return fmt.Errorf("recovery probe: load replacement %s failed: %w", pending.NewAgentID, nerr)
		default:
			// Record exists. If the session is ALSO alive, the
			// previous handoff just needs its tail completed —
			// dispatch to resumeHandoff. If the session is dead
			// (replacement command crashed at startup before the
			// original process reached step 8a's rollback), clean
			// up the stale replacement and fall through to normal
			// spawn — the old agent is still alive, so a fresh
			// replacement is the correct recovery action.
			if !tmux.HasSession(newRec.TmuxSession) {
				if path, perr := state.AgentPath(newRec.ID); perr == nil {
					_ = os.Remove(path)
				}
				_ = queue.Delete(pendingPath)
				_, _ = fmt.Fprintf(stderr,
					"note: stale replacement %s (session %s already exited) cleaned up; spawning fresh replacement\n",
					newRec.ID, newRec.TmuxSession)
				// Fall through to normal spawn flow.
				break
			}
			return resumeHandoff(opts, stdout, stderr, oldRec, newRec, pending.HandoffDoc, pendingPath)
		}
	}

	// 3. Load the agent record under the flock. If a concurrent
	//    handoff already archived it, agent.Load returns ErrNotFound
	//    and we bail without double-spawning. This is the only Load
	//    — the per-agent lock makes the pre-lock Load redundant.
	oldRec, err := agent.Load(opts.oldID)
	if err != nil {
		return fmt.Errorf("load agent %s under lock: %w (already handed off?)", opts.oldID, err)
	}

	// 3a. Dead-session short-circuit: if the outgoing tmux session is
	//     already gone (claude exited inside it via Ctrl-D / /exit /
	//     crash), there is no in-flight work to hand off and no agent
	//     to /exit. Spawning a replacement would emit a stub doc full
	//     of placeholders the new agent has no way to fill, then drop
	//     into a fresh shell — the operator wanted cleanup, not a
	//     fresh agent. Archive the record and return.
	//
	//     Runs AFTER the recovery probe so an in-flight handoff (queue
	//     file with NewAgentID set) still resumes via resumeHandoff,
	//     which retires the outgoing record using the already-spawned
	//     replacement instead of orphaning it.
	if !tmux.HasSession(oldRec.TmuxSession) {
		if err := oldRec.Archive(); err != nil {
			// Same fallback shape as step 12: try removing the live
			// record so a retry doesn't load a stale entry and loop.
			path, perr := state.AgentPath(oldRec.ID)
			if perr == nil {
				if rmErr := os.Remove(path); rmErr == nil {
					_, _ = fmt.Fprintf(stderr,
						"warning: archive %s: %v (live record removed instead, archive copy lost)\n",
						oldRec.ID, err)
				} else {
					return fmt.Errorf(
						"dead-session archive %s failed (%w) AND fallback remove failed (%w); clean up agents/%s.json manually",
						oldRec.ID, err, rmErr, oldRec.ID)
				}
			} else {
				return fmt.Errorf(
					"dead-session archive %s failed (%w) AND could not resolve live path (%w)",
					oldRec.ID, err, perr)
			}
		}
		_, _ = fmt.Fprintf(stdout,
			"agent %s session was dead — record archived, no replacement spawned\n", oldRec.ID)
		return nil
	}

	// 4. Resolve cwd + command for the replacement BEFORE writing
	//    any side effects. CLI flags win; otherwise inherit from
	//    oldRec so multi-repo operators invoking `fleet handoff`
	//    from a different shell still get the right project checkout
	//    and engine wrapper.
	//
	//    For legacy records (dispatched before this PR added Cwd /
	//    Command to agent.Record) BOTH the inherited value AND the
	//    flag may be empty. Refuse early — silently falling back to
	//    os.Getwd() / "claude" would land the replacement in the
	//    wrong tree and report success. Validating before doc/queue
	//    writes means a refusal leaves zero on-disk artifacts.
	cwd := opts.cwd
	if cwd == "" {
		cwd = oldRec.Cwd
	}
	if cwd == "" {
		return fmt.Errorf("agent %s is a legacy record with no stored cwd; pass --cwd <path> explicitly", opts.oldID)
	}
	command := opts.command
	if len(command) == 0 {
		command = oldRec.Command
	}
	if len(command) == 0 {
		return fmt.Errorf("agent %s is a legacy record with no stored command; pass --command <argv> explicitly", opts.oldID)
	}

	// 5. Write the handoff doc. The doc represents "agent oldID
	//    handed off"; its handoff_number is the OLD agent's number,
	//    its previous_handoff chains back to whatever the old agent
	//    itself inherited from. The next handoff increments by 1
	//    inside spawn.Spawn.
	now := time.Now().UTC()
	docPath, err := state.HandoffPath(oldRec.ID, now)
	if err != nil {
		return err
	}
	doc := handoff.NewManualStub(
		oldRec.ID, oldRec.TaskID, oldRec.Project,
		oldRec.HandoffNumber, oldRec.LastHandoffPath, now,
	)
	if err := handoff.Write(doc, docPath); err != nil {
		return fmt.Errorf("write handoff doc: %w", err)
	}

	// 6. Pre-allocate the replacement's agent ID so we can journal
	//    it BEFORE spawn. This closes the crash window where the old
	//    code wrote the journal twice (once empty, once with newID
	//    after spawn) — a crash between those two writes left the
	//    journal with NewAgentID="" and a retry would double-spawn.
	//    Now: journal once with the pre-allocated ID, then spawn
	//    with that exact ID. Crash anywhere after step 7 leaves a
	//    journal entry the recovery probe can match against the
	//    actual record on disk (or detect as orphan if no record).
	newID := agent.NewID()
	newSession := tmux.SessionName(newID)

	// 7. Write the queue file with the pre-allocated successor ID.
	//    This is the durable commit point — anything after a crash
	//    here is recoverable via the resume path in step 2.
	queuePath, err := queue.WriteSpawnFresh(queue.SpawnFresh{
		OldAgentID: oldRec.ID,
		HandoffDoc: docPath,
		Project:    oldRec.Project,
		TaskID:     oldRec.TaskID,
		NewAgentID: newID,
		NewSession: newSession,
	})
	if err != nil {
		return fmt.Errorf("enqueue spawn-fresh: %w", err)
	}

	// 8. Drain: spawn the replacement using the pre-allocated ID.
	newRec, err := spawn.Spawn(spawn.Options{
		OldRecord:      oldRec,
		NewDocPath:     docPath,
		Cwd:            cwd,
		Command:        command,
		PreAllocatedID: newID,
		InitialPrompt: "Read your handoff doc at " + docPath +
			" and continue the task. Do not wait for further operator input.",
	})
	if err != nil {
		// Spawn failed — leave the queue file in place so the
		// recovery path can detect the orphan (no record + no
		// session at the journaled NewAgentID/NewSession) and clean
		// it up on retry. The old agent is still alive and untouched.
		return fmt.Errorf("spawn replacement: %w", err)
	}

	// 8a. Verify the replacement session is actually alive before
	//     we retire the old agent. spawn.Spawn intentionally tolerates
	//     short-lived commands that exit cleanly (no stderr signal),
	//     but for handoff the replacement IS supposed to be a
	//     long-lived agent. If `--command` was a wrapper that crashed
	//     at startup, killing the old session would leave the task
	//     with no live successor. Roll back instead.
	if !tmux.HasSession(newRec.TmuxSession) {
		if path, perr := state.AgentPath(newRec.ID); perr == nil {
			_ = os.Remove(path)
		}
		_ = queue.Delete(queuePath)
		return fmt.Errorf(
			"replacement %s spawned but tmux session %s already exited (command crashed at startup?); old agent untouched",
			newRec.ID, newRec.TmuxSession)
	}

	// 9. Send "/exit" to the old session. ErrNoSession means it
	//    already died (operator killed manually, crash, etc.) — fine,
	//    fall through to Kill which is also idempotent.
	if err := tmux.SendKeys(oldRec.TmuxSession, "/exit", "Enter"); err != nil &&
		!errors.Is(err, tmux.ErrNoSession) {
		// Treat anything else as a warning, not a fatal — we still
		// want to archive and clean up.
		_, _ = fmt.Fprintf(stderr, "warning: send-keys to %s: %v\n", oldRec.TmuxSession, err)
	}

	// 10. Grace window so Claude can flush its own state on /exit.
	if opts.graceMillis > 0 {
		time.Sleep(time.Duration(opts.graceMillis) * time.Millisecond)
	}

	// 11. Kill the old session. Idempotent — returns nil if already
	//    gone. If Kill fails AND HasSession still reports the session
	//    alive, we MUST NOT archive the old record (that would hide a
	//    live agent from `fleet status`) AND we MUST roll back the
	//    new agent (otherwise a retry of `fleet handoff <id>` finds
	//    the still-live old record and spawns ANOTHER replacement).
	//
	//    Rollback: kill the new tmux session, delete the new record,
	//    delete the queue file. Old agent + old session untouched.
	//    Handoff doc stays as a (stale) artifact — operator can
	//    inspect it. Operator retries cleanly after investigating
	//    why kill failed.
	if err := tmux.Kill(oldRec.TmuxSession); err != nil {
		if tmux.HasSession(oldRec.TmuxSession) {
			// Try to roll back the new agent. ONLY delete its record
			// after confirming the new tmux session is also gone —
			// otherwise we'd leave a live tmux session with no
			// fleet record (untracked successor that a later retry
			// would not see, leading to multiple replacements).
			_ = tmux.Kill(newRec.TmuxSession)
			if !tmux.HasSession(newRec.TmuxSession) {
				if path, perr := state.AgentPath(newRec.ID); perr == nil {
					_ = os.Remove(path)
				}
				_ = queue.Delete(queuePath)
				return fmt.Errorf(
					"old session %s still alive after kill failure: %w (replacement %s rolled back; investigate before retrying)",
					oldRec.TmuxSession, err, newRec.ID)
			}
			// Both kills failed. Don't touch the new record — it
			// still points at a live tmux session. Operator must
			// investigate both stuck sessions.
			return fmt.Errorf(
				"old session %s AND new session %s both alive after kill failure: %w (replacement %s record preserved to track the live session; clean up both manually)",
				oldRec.TmuxSession, newRec.TmuxSession, err, newRec.ID)
		}
		// Session vanished concurrently with our Kill attempt
		// (race with operator's manual kill, OS shutdown, etc.).
		// Safe to proceed.
		_, _ = fmt.Fprintf(stderr, "note: kill %s reported error but session is gone: %v\n",
			oldRec.TmuxSession, err)
	}

	// 12. Archive the old record. After this, `fleet status` no
	//     longer shows the outgoing agent. We've confirmed the old
	//     session is dead in step 10, so this can't hide a live
	//     agent. If Archive fails (rare — agents/archive/ went away
	//     or is unwritable), fall back to deleting the live record
	//     directly so a retry of `fleet handoff <id>` doesn't load
	//     the stale record and double-spawn. If even the delete
	//     fails, hard-error: the live record is stuck and must be
	//     removed manually before retry.
	if err := oldRec.Archive(); err != nil {
		path, perr := state.AgentPath(oldRec.ID)
		if perr == nil {
			if rmErr := os.Remove(path); rmErr == nil {
				_, _ = fmt.Fprintf(stderr,
					"warning: archive %s: %v (live record removed instead, archive copy lost)\n",
					oldRec.ID, err)
			} else {
				return fmt.Errorf(
					"archive %s failed (%w) AND fallback remove failed (%w); replacement %s spawned but old record stuck — clean up agents/%s.json manually before retrying",
					oldRec.ID, err, rmErr, newRec.ID, oldRec.ID)
			}
		} else {
			return fmt.Errorf(
				"archive %s failed (%w) AND could not resolve live path (%w); replacement %s spawned",
				oldRec.ID, err, perr, newRec.ID)
		}
	}

	// 13. Delete the queue file. Work is durable on disk; the journal
	//     entry is no longer needed.
	if err := queue.Delete(queuePath); err != nil {
		_, _ = fmt.Fprintf(stderr, "warning: delete queue file: %v\n", err)
	}

	_, _ = fmt.Fprintf(stdout, "agent %s handed off → %s\n", oldRec.ID, newRec.ID)
	_, _ = fmt.Fprintf(stdout, "  task:    %s\n", newRec.TaskID)
	_, _ = fmt.Fprintf(stdout, "  project: %s\n", newRec.Project)
	_, _ = fmt.Fprintf(stdout, "  tmux:    %s\n", newRec.TmuxSession)
	_, _ = fmt.Fprintf(stdout, "  handoff: %s\n", docPath)
	_, _ = fmt.Fprintf(stdout, "  number:  %d (was %d)\n", newRec.HandoffNumber, oldRec.HandoffNumber)
	_, _ = fmt.Fprintf(stdout, "\nattach with: fleet attach %s\n", newRec.ID)
	_, _ = fmt.Fprintf(stdout, "then say: read the handoff doc at %s and continue\n", docPath)
	return nil
}

// resumeHandoff finishes a handoff that crashed AFTER spawn but
// BEFORE archive (the recovery branch in step 2 of runHandoff).
// Same kill+archive+delete sequence as the tail of runHandoff, no
// spawn — the new agent already exists.
//
// Caller holds the per-agent flock.
func resumeHandoff(opts *handoffOpts, stdout, stderr io.Writer,
	oldRec, newRec *agent.Record, docPath, queuePath string) error {

	// Verify the previously-spawned replacement is still alive. If it
	// died after the original spawn (operator manually killed it,
	// crashed, etc.), retiring the old agent now would leave the task
	// with nothing running. Bail without touching the old agent.
	if !tmux.HasSession(newRec.TmuxSession) {
		return fmt.Errorf(
			"resume handoff: replacement %s tmux session %s is gone; old agent %s untouched (clean up agents/%s.json + queue file or restart handoff)",
			newRec.ID, newRec.TmuxSession, oldRec.ID, newRec.ID)
	}

	if err := tmux.SendKeys(oldRec.TmuxSession, "/exit", "Enter"); err != nil &&
		!errors.Is(err, tmux.ErrNoSession) {
		_, _ = fmt.Fprintf(stderr, "warning: send-keys to %s: %v\n", oldRec.TmuxSession, err)
	}
	if opts.graceMillis > 0 {
		time.Sleep(time.Duration(opts.graceMillis) * time.Millisecond)
	}
	if err := tmux.Kill(oldRec.TmuxSession); err != nil {
		if tmux.HasSession(oldRec.TmuxSession) {
			return fmt.Errorf(
				"resume handoff: old session %s still alive after kill: %w (replacement %s exists; investigate)",
				oldRec.TmuxSession, err, newRec.ID)
		}
		_, _ = fmt.Fprintf(stderr, "note: kill %s reported error but session is gone: %v\n",
			oldRec.TmuxSession, err)
	}
	if err := oldRec.Archive(); err != nil {
		path, perr := state.AgentPath(oldRec.ID)
		if perr == nil {
			if rmErr := os.Remove(path); rmErr == nil {
				_, _ = fmt.Fprintf(stderr, "warning: archive %s: %v (live record removed instead)\n",
					oldRec.ID, err)
			} else {
				return fmt.Errorf("resume handoff: archive %s failed (%w) AND remove failed (%w)",
					oldRec.ID, err, rmErr)
			}
		}
	}
	if err := queue.Delete(queuePath); err != nil {
		_, _ = fmt.Fprintf(stderr, "warning: delete queue file: %v\n", err)
	}

	_, _ = fmt.Fprintf(stdout, "resumed crashed handoff: %s → %s (replacement was already spawned)\n",
		oldRec.ID, newRec.ID)
	_, _ = fmt.Fprintf(stdout, "  task:    %s\n", newRec.TaskID)
	_, _ = fmt.Fprintf(stdout, "  project: %s\n", newRec.Project)
	_, _ = fmt.Fprintf(stdout, "  tmux:    %s\n", newRec.TmuxSession)
	_, _ = fmt.Fprintf(stdout, "  handoff: %s\n", docPath)
	_, _ = fmt.Fprintf(stdout, "\nattach with: fleet attach %s\n", newRec.ID)
	return nil
}
