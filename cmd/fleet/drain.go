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

// defaultResumeTimeoutMillis bounds a single per-queue-file Resume on the
// lease-failover drain path (FLEET_LEASE_FAILOVER on). It is the wall-clock
// budget after which the drain STOPS waiting on a slow handoff and escalates
// to the safety-net takeover instead of blocking forever — the structural
// fix for the 81-drain leak. Legacy (flag-off) drains ignore it.
const defaultResumeTimeoutMillis = 30000

func newDrainCmd() *cobra.Command {
	var (
		graceMillis        int
		resumeTimeoutMilli int
	)
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
double-spawning the same replacement.

Under FLEET_LEASE_FAILOVER the drain is BOUNDED + lease-aware: it stands
down when a healthy leader holds the lease, verifies the handoff-complete
barrier before a graceful kill, and escalates a slow/hung handoff to the
safety-net takeover after --resume-timeout-ms instead of blocking.`,
		RunE: func(c *cobra.Command, _ []string) error {
			return runDrain(c.OutOrStdout(), c.ErrOrStderr(), graceMillis, resumeTimeoutMilli)
		},
	}
	cmd.Flags().IntVar(&graceMillis, "grace-ms", handoffop.DefaultGraceMillis,
		"milliseconds between /exit and Kill on each retired session")
	cmd.Flags().IntVar(&resumeTimeoutMilli, "resume-timeout-ms", defaultResumeTimeoutMillis,
		"FLEET_LEASE_FAILOVER only: wall-clock budget for one handoff before escalating to the safety-net takeover")
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
func runDrain(stdout, stderr io.Writer, graceMillis, resumeTimeoutMillis int) error {
	if _, err := state.Bootstrap(); err != nil {
		return fmt.Errorf("bootstrap ~/.fleet: %w", err)
	}
	if err := tmux.Available(); err != nil {
		return err
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
		req, perr := queue.ReadSpawnFresh(path)
		if perr != nil {
			// Skip rather than fail-fast — a malformed or future-schema
			// file is one problem among many. Log and continue so other
			// pending agents still drain.
			_, _ = fmt.Fprintf(stderr, "fleet drain: read %s: %v\n", path, perr)
			failed++
			continue
		}
		if err := drainOne(req, path, graceMillis, resumeTimeoutMillis, stdout, stderr); err != nil {
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

// drainOne handles a single queue file.
//
// FLEET_LEASE_FAILOVER routes the behavior (DESIGN-handoff-drain-storm-leak
// §3(D), PR3):
//
//   - OFF (default): the LEGACY path — take the per-agent flock and run
//     handoffop.Resume under it. Byte-identical to pre-PR3 behavior. This is
//     the only path that runs in production today; the lease-aware path is
//     dev-only until the stack lands.
//   - ON: the BOUNDED, lease-aware path (drainOneLeaseAware) — NEVER holds a
//     lock across Resume, stands down under a healthy leader, verifies the
//     handoff-complete barrier before a graceful kill, and escalates a
//     slow/hung handoff to the safety-net takeover after the timeout. This
//     is the structural fix that removes the forever-held lock + the
//     81-drain leak.
func drainOne(req queue.SpawnFresh, path string, graceMillis, resumeTimeoutMillis int,
	stdout, stderr io.Writer) error {

	if leaseDrainEnabled() {
		return drainOneLeaseAware(req, path, graceMillis, resumeTimeoutMillis, stdout, stderr)
	}
	return drainOneLegacy(req, path, graceMillis, stdout, stderr)
}

// drainOneLegacy is the pre-PR3 drain: take the per-agent flock and run
// Resume under it. Retained verbatim for the FLEET_LEASE_FAILOVER-off path
// so production behavior is unchanged this PR.
//
// KNOWN ROOT CAUSE (the reason the lease-aware path exists): this holds the
// per-agent flock across the ENTIRE Resume — tmux probes, AtomicCoordSwap's
// nested flock, spawn — any of which can hang, and acquireFlock has no
// timeout, so a single stuck holder wedges every later drain forever
// (drain.go:101-106, the 81-process leak). The lease-aware path drops this.
func drainOneLegacy(req queue.SpawnFresh, path string, graceMillis int,
	stdout, stderr io.Writer) error {

	release, err := state.LockAgent(req.OldAgentID)
	if err != nil {
		return fmt.Errorf("lock agent %s: %w", req.OldAgentID, err)
	}
	defer release()
	return handoffop.Resume(req, path, graceMillis, stdout, stderr)
}
