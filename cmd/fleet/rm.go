package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/queue"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tmux"
)

// rmOpts captures cobra-parsed args for `fleet rm`.
type rmOpts struct {
	id string
}

// newRmCmd wires the `fleet rm <agent-id>` shell command. The TUI's
// [x] keybind shells out here after a confirmation prompt — keeping
// the destructive logic in one place (tested via this CLI) means the
// TUI is just dispatch + confirmation, and a bug in `rm` cannot crash
// the TUI process.
func newRmCmd() *cobra.Command {
	opts := &rmOpts{}
	cmd := &cobra.Command{
		Use:   "rm <agent-id>",
		Short: "Archive an agent (kill its tmux session, no replacement)",
		Long: `rm kills the agent's tmux session if it is alive, then archives
the agent record. Unlike ` + "`fleet handoff`" + `, no replacement is
spawned and no handoff doc is written — this is the operator's "I'm
done with this agent, clear it from my view" action.

Used by the TUI's [x] keybind after a y/N confirmation prompt. Refuses
to run if a handoff for this agent is mid-flight (queue file pointing
at the same id) — drain or abort it first so the recovery probe in
` + "`fleet handoff`" + ` doesn't see a dangling journal entry.`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			opts.id = args[0]
			return runRm(opts, c.OutOrStdout(), c.ErrOrStderr())
		},
	}
	return cmd
}

// runRm: lock → load → guard against in-flight handoff → kill session
// (idempotent) → archive record. No spawn, no doc, no queue writes.
//
// Concurrency: the per-agent flock serializes rm against handoff and
// other rm invocations on the same id. A concurrent handoff that
// already archived the agent surfaces as ErrNotFound on Load and rm
// exits cleanly with a clear message.
func runRm(opts *rmOpts, stdout, stderr io.Writer) error {
	if _, err := state.Bootstrap(); err != nil {
		return fmt.Errorf("bootstrap ~/.fleet: %w", err)
	}
	if err := tmux.Available(); err != nil {
		return err
	}

	release, err := state.LockAgent(opts.id)
	if err != nil {
		return fmt.Errorf("lock agent %s: %w", opts.id, err)
	}
	defer release()

	rec, err := agent.Load(opts.id)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			return fmt.Errorf("no agent record for %q (try `fleet status` to list)", opts.id)
		}
		return fmt.Errorf("load agent %s under lock: %w", opts.id, err)
	}

	// Refuse when a handoff for this agent is journaled — removing the
	// old record while a replacement is in flight would orphan
	// `fleet handoff`'s recovery probe (it would see the queue file
	// but no old record on retry, then hit the "task has no live
	// agent" error path). Operator should drain or let the handoff
	// finish first.
	pendingPath, _ := state.QueuePath(queue.SpawnFreshName(opts.id))
	if _, perr := os.Stat(pendingPath); perr == nil {
		return fmt.Errorf(
			"agent %s has a pending handoff (%s) — `fleet drain` (or finish the handoff) before rm",
			opts.id, pendingPath)
	}

	// Kill the session. tmux.Kill is idempotent — returns nil if the
	// session is already gone. If Kill fails AND the session is still
	// alive, refuse to archive: that would hide a live agent from
	// `fleet status` while leaving a phantom tmux session running.
	if rec.TmuxSession != "" {
		if err := tmux.Kill(rec.TmuxSession); err != nil {
			if tmux.HasSession(rec.TmuxSession) {
				return fmt.Errorf(
					"kill session %s failed AND session still alive: %w (refusing to archive a live agent)",
					rec.TmuxSession, err)
			}
			// Session vanished concurrently with our Kill (operator
			// killed it manually, OS shutdown, etc.). Safe to proceed.
			_, _ = fmt.Fprintf(stderr,
				"note: kill %s reported error but session is gone: %v\n",
				rec.TmuxSession, err)
		}
	}

	// Archive the record. Same fallback shape as runHandoff step 12:
	// if Archive fails, try removing the live record so a retry of
	// `fleet rm <id>` doesn't loop on a stale entry.
	if err := rec.Archive(); err != nil {
		path, perr := state.AgentPath(rec.ID)
		if perr == nil {
			if rmErr := os.Remove(path); rmErr == nil {
				_, _ = fmt.Fprintf(stderr,
					"warning: archive %s: %v (live record removed instead, archive copy lost)\n",
					rec.ID, err)
			} else {
				return fmt.Errorf(
					"archive %s failed (%w) AND fallback remove failed (%w); clean up agents/%s.json manually",
					rec.ID, err, rmErr, rec.ID)
			}
		} else {
			return fmt.Errorf(
				"archive %s failed (%w) AND could not resolve live path (%w)",
				rec.ID, err, perr)
		}
	}

	_, _ = fmt.Fprintf(stdout, "agent %s archived (no replacement spawned)\n", rec.ID)
	return nil
}
