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
injection so we can't fill its view of the work. After ` + "`fleet attach`" + `
to the new agent, paste the handoff doc path and ask it to read+continue.

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

	// 2. Load the agent record under the flock. If a concurrent
	//    handoff already archived it, agent.Load returns ErrNotFound
	//    and we bail without double-spawning. This is the only Load
	//    — the per-agent lock makes the pre-lock Load redundant.
	oldRec, err := agent.Load(opts.oldID)
	if err != nil {
		return fmt.Errorf("load agent %s under lock: %w (already handed off?)", opts.oldID, err)
	}

	// 4. Write the handoff doc. The doc represents "agent oldID
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

	// 5. Write the queue file. With flock acquired, this is no longer
	//    a race-prone commit point but a journal entry — if we crash
	//    before queue.Delete (step 10), a future drainer (4b TUI
	//    background loop) sees a stale queue file pointing at an
	//    archived agent and skips it. In 4a there's no drainer; the
	//    file lingers as harmless residue until 4b ships.
	queuePath, err := queue.WriteSpawnFresh(queue.SpawnFresh{
		OldAgentID: oldRec.ID,
		HandoffDoc: docPath,
		Project:    oldRec.Project,
		TaskID:     oldRec.TaskID,
	})
	if err != nil {
		return fmt.Errorf("enqueue spawn-fresh: %w", err)
	}

	// 6. Resolve cwd + command for the replacement, defaulting to
	//    the outgoing agent's stored values so multi-repo operators
	//    invoking `fleet handoff` from a different shell still get
	//    the right project checkout and engine wrapper. CLI flags
	//    (when set) override the inherited values.
	cwd := opts.cwd
	if cwd == "" {
		cwd = oldRec.Cwd
	}
	command := opts.command
	if len(command) == 0 {
		command = oldRec.Command
	}
	if len(command) == 0 {
		// Both the flag AND the legacy record were empty (pre-PR
		// records lack Command). Fall back to "claude" rather than
		// erroring — that's what dispatch defaults to.
		command = []string{"claude"}
	}

	// 7. Drain: spawn the replacement.
	newRec, err := spawn.Spawn(spawn.Options{
		OldRecord:  oldRec,
		NewDocPath: docPath,
		Cwd:        cwd,
		Command:    command,
	})
	if err != nil {
		// Spawn failed — leave the queue file in place so a later
		// drain can retry. The old agent is still alive and untouched.
		return fmt.Errorf("spawn replacement: %w", err)
	}

	// 8. Send "/exit" to the old session. ErrNoSession means it
	//    already died (operator killed manually, crash, etc.) — fine,
	//    fall through to Kill which is also idempotent.
	if err := tmux.SendKeys(oldRec.TmuxSession, "/exit", "Enter"); err != nil &&
		!errors.Is(err, tmux.ErrNoSession) {
		// Treat anything else as a warning, not a fatal — we still
		// want to archive and clean up.
		_, _ = fmt.Fprintf(stderr, "warning: send-keys to %s: %v\n", oldRec.TmuxSession, err)
	}

	// 9. Grace window so Claude can flush its own state on /exit.
	if opts.graceMillis > 0 {
		time.Sleep(time.Duration(opts.graceMillis) * time.Millisecond)
	}

	// 10. Kill the old session. Idempotent — returns nil if already
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
			_ = tmux.Kill(newRec.TmuxSession)
			if path, perr := state.AgentPath(newRec.ID); perr == nil {
				_ = os.Remove(path)
			}
			_ = queue.Delete(queuePath)
			return fmt.Errorf(
				"old session %s still alive after kill failure: %w (replacement %s rolled back; investigate before retrying)",
				oldRec.TmuxSession, err, newRec.ID)
		}
		// Session vanished concurrently with our Kill attempt
		// (race with operator's manual kill, OS shutdown, etc.).
		// Safe to proceed.
		_, _ = fmt.Fprintf(stderr, "note: kill %s reported error but session is gone: %v\n",
			oldRec.TmuxSession, err)
	}

	// 11. Archive the old record. After this, `fleet status` no
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

	// 12. Delete the queue file. Work is durable on disk; the journal
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
