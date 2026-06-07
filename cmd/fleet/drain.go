package main

import (
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/edisonshen/fleet/internal/handoffop"
	"github.com/edisonshen/fleet/internal/queue"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tmux"
)

func newDrainCmd() *cobra.Command {
	var graceMillis int
	cmd := &cobra.Command{
		Use:   "drain",
		Short: "Process pending fleet-guard auto-handoff queue files",
		Long: `drain reads every ~/.fleet/queue/spawn-fresh-*.json file written
by the fleet-guard skill (or left behind by a crashed handoff) and
completes the handoff: spawn the replacement (if not already), retire
the old agent, delete the queue file.

Runs once per invocation — the TUI's queue fsnotify watcher invokes
the same code path on every queue event, but operators without the
TUI open can run ` + "`fleet drain`" + ` from cron or after a crash to
catch up.

Per-agent flocking keeps two concurrent drains (e.g. cron + TUI) from
double-spawning the same replacement.`,
		RunE: func(c *cobra.Command, _ []string) error {
			return runDrain(c.OutOrStdout(), c.ErrOrStderr(), graceMillis)
		},
	}
	cmd.Flags().IntVar(&graceMillis, "grace-ms", handoffop.DefaultGraceMillis,
		"milliseconds between /exit and Kill on each retired session")
	return cmd
}

// runDrain processes every pending spawn-fresh queue file. Each file is
// processed under its own per-agent flock so two concurrent drains
// serialize per agent_id; drains for DIFFERENT agents run in parallel.
//
// Failure isolation: a Resume error on one queue file logs to stderr and
// continues with the next file. The failing queue file is left in place
// so the next drain (or the operator) can pick it up. Returns nil if at
// least one file processed successfully OR there were zero files; only
// returns an error for setup failures (Bootstrap, ListPending) where no
// progress was possible.
func runDrain(stdout, stderr io.Writer, graceMillis int) error {
	if _, err := state.Bootstrap(); err != nil {
		return fmt.Errorf("bootstrap ~/.fleet: %w", err)
	}
	if err := tmux.Available(); err != nil {
		return err
	}

	// Write the run-record, deferring the delete so it runs on the happy
	// AND failure/panic path (fleet-owns-its-resources: cleanup is the
	// LAST step, via defer). A drain SIGKILLed mid-flight can't run the
	// defer — that leaked record is exactly what `fleet gc --kinds
	// drain-procs` reaps once the heartbeat goes stale. A write failure is
	// non-fatal (rh stays nil; Beat/Stop no-op): the drain still completes
	// its work; it just isn't gc-reapable via the run-record path.
	//
	// The heartbeat is PROGRESS-driven (rh.Beat at the loop checkpoints
	// below), not a background timer — so a drain wedged forever inside a
	// blocking drainOne/LockAgent stops beating and goes stale → reapable.
	var rh *drainRunHandle
	if h, rerr := startDrainRunRecord(); rerr != nil {
		_, _ = fmt.Fprintf(stderr, "fleet drain: run-record: %v (continuing)\n", rerr)
	} else {
		rh = h
		defer rh.Stop()
	}

	paths, err := queue.ListPending()
	if err != nil {
		return fmt.Errorf("list queue: %w", err)
	}
	if len(paths) == 0 {
		_, _ = fmt.Fprintln(stdout, "fleet drain: no pending handoffs")
		return nil
	}

	processed := 0
	failed := 0
	for _, path := range paths {
		// Progress checkpoint: we are about to start work on the next
		// queue file, which is forward progress. Advancing the heartbeat
		// HERE (not from a timer) means a drain that then wedges inside
		// drainOne's blocking LockAgent/Resume stops beating and goes
		// stale → the gc reaper catches it (codex iter-5 [P2]).
		rh.Beat()
		req, perr := queue.ReadSpawnFresh(path)
		if perr != nil {
			// Skip rather than fail-fast — a malformed or future-schema
			// file is one problem among many. Log and continue so other
			// pending agents still drain.
			_, _ = fmt.Fprintf(stderr, "fleet drain: read %s: %v\n", path, perr)
			failed++
			continue
		}
		if err := drainOne(req, path, graceMillis, stdout, stderr); err != nil {
			_, _ = fmt.Fprintf(stderr, "fleet drain: %s: %v\n", req.OldAgentID, err)
			failed++
			continue
		}
		processed++
	}

	_, _ = fmt.Fprintf(stdout, "fleet drain: %d processed, %d failed\n",
		processed, failed)
	if processed == 0 && failed > 0 {
		return errors.New("fleet drain: every pending handoff failed")
	}
	return nil
}

// drainOne handles a single queue file under its per-agent flock.
func drainOne(req queue.SpawnFresh, path string, graceMillis int,
	stdout, stderr io.Writer) error {

	release, err := state.LockAgent(req.OldAgentID)
	if err != nil {
		return fmt.Errorf("lock agent %s: %w", req.OldAgentID, err)
	}
	defer release()
	return handoffop.Resume(req, path, graceMillis, stdout, stderr)
}
