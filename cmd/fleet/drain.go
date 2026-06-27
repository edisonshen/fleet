package main

import (
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/edisonshen/fleet/internal/fleetlog"
	"github.com/edisonshen/fleet/internal/handoffop"
	"github.com/edisonshen/fleet/internal/queue"
	"github.com/edisonshen/fleet/internal/spawn"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tmux"
)

// ErrEscalatedToTakeOver is the typed signal the lease-aware drain returns
// when it could not complete a graceful drain within the budget and escalated
// to the safety-net takeover (fence -> kill -> acquire). It is NOT a failure:
// the drain did not block and held no lock; the takeover handles recovery.
// runDrain counts it as processed (codex PR3 iter-2 [P2]). Declared here (the
// all-platform file) so the loop can reference it on every GOOS; the
// lease-aware path that returns it is linux/darwin only.
var ErrEscalatedToTakeOver = errors.New("fleet drain: escalated to safety-net takeover")

// ErrResumeBackgrounded is the typed signal coldResume returns when a
// handoff Resume exceeds the --resume-timeout-ms budget. It is NOT a
// failure: the resume goroutine keeps running (holding the bounded
// per-agent lock until Resume unwinds) and the drain merely stops
// waiting — so it must never produce "every pending handoff failed" +
// exit 1 while the handoff in fact completes (DESIGN-handoff-lifecycle-
// hardening bug A, observed live 2026-06-10). But it is NOT completion
// either (codex iter-1 [P1]): standalone `fleet drain` exits when
// runDrain returns, killing the goroutine, so the handoff may still be
// pending — only a completed Resume deletes the queue file, which is
// what lets a later drain retry. runDrain therefore counts it in a
// separate `backgrounded` bucket: never failed, never claimed processed.
// Declared here (the all-platform file) so the loop can reference it on
// every GOOS; the path that returns it is linux/darwin only.
var ErrResumeBackgrounded = errors.New("fleet drain: resume exceeded the budget; handoff completing in the background")

// defaultResumeTimeoutMillis bounds a single per-queue-file Resume on the
// lease-failover drain path (FLEET_LEASE_FAILOVER on). It is the wall-clock
// budget after which the drain STOPS waiting on a slow handoff and escalates
// to the safety-net takeover instead of blocking forever — the structural
// fix for the 81-drain leak. Legacy (flag-off) drains ignore it.
//
// 120s, not 30s (DESIGN-handoff-lifecycle-hardening bug A, operator: "wait
// at least 2 mins"): a real coord resume — tmux spawn + readiness waits —
// routinely outlives 30s, and a budget that trips on the happy path turns
// every slow-but-succeeding handoff into a false alarm.
const defaultResumeTimeoutMillis = 120000

// drainOneFn is a test seam over drainOne (same pattern as drainProcStartFn /
// drainRunNow): runDrain's accounting is exercised without a real tmux/lease
// Resume behind it. Production never reassigns it.
var drainOneFn = drainOne

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
			finish := fleetlog.CLIStart(fleetlog.Fields{}, "drain")
			err := runDrain(c.OutOrStdout(), c.ErrOrStderr(), graceMillis, resumeTimeoutMilli)
			finish(err)
			return err
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
// least one file processed OR was backgrounded (still completing; its
// queue file survives for a later drain to retry) OR there were zero
// files; returns an error only when EVERY file genuinely failed, or for
// setup failures (Bootstrap, ListPending) where no progress was possible.
func runDrain(stdout, stderr io.Writer, graceMillis, resumeTimeoutMillis int) error {
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
	if h, rerr := startDrainRunRecord(graceMillis); rerr != nil {
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
	backgrounded := 0
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
		err := drainOneFn(req, path, graceMillis, resumeTimeoutMillis, stdout, stderr)
		switch {
		case errors.Is(err, ErrResumeBackgrounded):
			// Not a failure (DESIGN-handoff-lifecycle-hardening bug A): the
			// resume was merely SLOW. The goroutine keeps the bounded per-agent
			// lock and finishes the handoff in the background; a contending
			// drain stands down, so no duplicate spawn. A slow handoff must
			// never produce exit 1 + "every pending handoff failed".
			//
			// But NOT processed either (codex iter-1 [P1]): standalone
			// `fleet drain` exits when runDrain returns, killing the
			// goroutine mid-Resume, so the handoff may still be pending —
			// "processed" would report success for work that did not happen.
			// That interruption is safe, not silent: ONLY a completed Resume
			// deletes the queue file (queue.Delete in retireOldAgent/
			// cleanUpStaleQueue), so the file survives and the NEXT drain
			// re-runs Resume, which finishes the handoff or reconciles an
			// already-completed one (pinned by
			// TestDrain_BackgroundedResumeKeepsQueueFile). Count it in the
			// separate `backgrounded` bucket so the summary tells cron/manual
			// callers the truth: still completing, retried by a later drain.
			_, _ = fmt.Fprintf(stdout,
				"fleet drain: %s resume still running; handoff completing in the background (queue file kept; a later drain verifies or retries)\n",
				req.OldAgentID)
			backgrounded++
		case errors.Is(err, ErrEscalatedToTakeOver):
			// Not a failure (codex PR3 iter-2 [P2]): the bounded drain did its
			// job — it stopped waiting on a slow/hung handoff and handed
			// recovery to the safety-net takeover without blocking or holding a
			// lock. Count it as processed so a queue of successful escalations
			// does not make `fleet drain` report "every pending handoff failed".
			_, _ = fmt.Fprintf(stdout,
				"fleet drain: %s escalated to safety-net takeover (handoff was slow/hung)\n", req.OldAgentID)
			processed++
		case err != nil:
			_, _ = fmt.Fprintf(stderr, "fleet drain: %s: %v\n", req.OldAgentID, err)
			failed++
		default:
			processed++
		}
	}

	// Truthful summary (codex iter-1 [P1]): backgrounded resumes get their
	// own bucket — they are neither completed nor failed, and the queue
	// file left in place is what makes "a later drain retries" true. The
	// two-bucket line is kept verbatim when nothing was backgrounded so
	// existing cron/log scrapers keep matching.
	if backgrounded > 0 {
		_, _ = fmt.Fprintf(stdout,
			"fleet drain: %d processed, %d backgrounded (still completing; a later drain retries), %d failed\n",
			processed, backgrounded, failed)
	} else {
		_, _ = fmt.Fprintf(stdout, "fleet drain: %d processed, %d failed\n",
			processed, failed)
	}
	// Exit code: only "every pending handoff failed" is an error. A
	// backgrounded resume is not a genuine failure — it must not trip
	// exit 1 (the false alarm this task exists to fix), and its presence
	// also falsifies "every ... failed" when mixed with real failures.
	if processed == 0 && backgrounded == 0 && failed > 0 {
		return errors.New("fleet drain: every pending handoff failed")
	}
	return nil
}

// drainOne handles a single queue file.
//
// FLEET_LEASE_FAILOVER + the COORD-vs-WORKER classification route the
// behavior (DESIGN-handoff-drain-storm-leak §3(D), PR3):
//
//   - flag OFF (default): the LEGACY path — take the per-agent flock and run
//     handoffop.Resume under it. Byte-identical to pre-PR3 behavior.
//   - flag ON + a COORD handoff: the BOUNDED, lease-aware path
//     (drainOneLeaseAware) — the lease is the single-flight guarantee, so the
//     graceful/hung path holds NO lock across kill/escalate (the structural
//     fix for the forever-held lock + 81-drain leak).
//   - flag ON + a WORKER (non-coord) handoff: the LEGACY path. A worker
//     handoff carries a Project but is NOT the project coord, so the coord
//     lease says nothing about it; routing it through the coord lease
//     stand-down would strand it forever (codex PR3 iter-4 [P1]). Workers
//     keep the per-agent-flock serialization their Resume contract requires.
//
// The discriminator is spawn.IsCoordSpawn(req.TaskID, req.Project) — the SAME
// coord-vs-worker convention the spawn path uses, so the two never drift.
func drainOne(req queue.SpawnFresh, path string, graceMillis, resumeTimeoutMillis int,
	stdout, stderr io.Writer) error {

	if leaseDrainEnabled() && spawn.IsCoordSpawn(req.TaskID, req.Project) {
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
