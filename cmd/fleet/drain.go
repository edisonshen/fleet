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
	"github.com/edisonshen/fleet/internal/tasks"
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
// lease-aware drain path. It is the wall-clock
// budget after which the drain STOPS waiting on a slow handoff and escalates
// to the safety-net takeover instead of blocking forever — the structural
// fix for the 81-drain leak. Non-lease platforms ignore it.
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

// errDropped / errHeldPending are the two NON-FAILURE outcomes the
// worker/legacy classifier returns (DESIGN-drain-nonforcing). Neither is a
// failure and neither trips exit 1:
//
//   - errDropped: the backing task is provably terminal (done/abandoned), so
//     the handoff is MOOT — drainOneLegacy already deleted the queue file and
//     surfaced the drop. runDrain counts it in the `dropped` bucket.
//   - errHeldPending: the backing task is UNRESOLVABLE (missing row / unreadable
//     tasks.md) — not proof of done, so the handoff is preserved and HELD.
//     runDrain counts it in the `pending` bucket (alongside handoffop's
//     ErrHeldOptOut, the live-but-opted-out case).
//
// Bucketing both out of `failed` is what removes the recurring "every pending
// handoff failed" red banner for entries that were never going to force-complete.
var (
	errDropped     = errors.New("fleet drain: handoff dropped (backing task terminal)")
	errHeldPending = errors.New("fleet drain: handoff held pending (unresolvable backing task)")
)

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

On lease-capable platforms the drain is BOUNDED + lease-aware: it stands
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
		"lease-aware drain only: wall-clock budget for one handoff before escalating to the safety-net takeover")
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
	dropped := 0
	pending := 0
	for _, path := range paths {
		// Progress checkpoint: we are about to start work on the next
		// queue file, which is forward progress. Advancing the heartbeat
		// HERE (not from a timer) means a drain that then wedges inside
		// drainOne's blocking LockAgent/Resume stops beating and goes
		// stale → the gc reaper catches it (codex iter-5 [P2]).
		rh.Beat()
		req, perr := queue.ReadSpawnFresh(path)
		if perr != nil {
			if errors.Is(perr, state.ErrNotFound) {
				// The file vanished between ListPending and this read — a
				// concurrent consumer (cron + TUI, or a sibling drain that
				// dropped it) already handled it. Benign: NOT a failure.
				continue
			}
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
		case errors.Is(err, errDropped):
			// Moot handoff dropped (backing task done/abandoned). The drop
			// diagnostic already fired inside drainOneLegacy; count it in the
			// `dropped` bucket — neither processed nor failed.
			dropped++
		case errors.Is(err, errHeldPending) || errors.Is(err, handoffop.ErrHeldOptOut):
			// Live-but-must-wait handoff: an opted-out worker (ErrHeldOptOut)
			// or an unresolvable backing task (errHeldPending). NOT a failure —
			// the queue file is preserved and the operator is surfaced a
			// manual-handoff hint. Reclassifying failed→pending is what stops
			// the recurring "every pending handoff failed" red banner.
			msg := fmt.Sprintf(
				"fleet drain: %s pending — worker handoff, run 'fleet handoff %s'",
				req.OldAgentID, req.OldAgentID)
			_, _ = fmt.Fprintln(stdout, msg)
			fleetlog.Log(fleetlog.CompCLI, "drain.pending", "info", fleetlog.Fields{
				Agent: req.OldAgentID,
				Proj:  req.Project,
				Slug:  req.TaskID,
				Msg:   msg,
			})
			pending++
		case err != nil:
			_, _ = fmt.Fprintf(stderr, "fleet drain: %s: %v\n", req.OldAgentID, err)
			failed++
		default:
			processed++
		}
	}

	// Truthful summary. Each bucket is neither processed nor failed:
	// backgrounded (still completing), dropped (moot handoff removed), pending
	// (held for a manual handoff). The bare "N processed, M failed" and the
	// backgrounded parenthetical stay VERBATIM when their buckets are zero so
	// existing cron/log scrapers + drain_test string asserts keep matching;
	// dropped / pending append via a fixed-order builder only when nonzero
	// (DESIGN-drain-nonforcing).
	summary := fmt.Sprintf("fleet drain: %d processed", processed)
	if backgrounded > 0 {
		summary += fmt.Sprintf(", %d backgrounded (still completing; a later drain retries)", backgrounded)
	}
	summary += fmt.Sprintf(", %d failed", failed)
	if dropped > 0 {
		summary += fmt.Sprintf(", %d dropped", dropped)
	}
	if pending > 0 {
		summary += fmt.Sprintf(", %d pending", pending)
	}
	_, _ = fmt.Fprintln(stdout, summary)

	// Exit code: unchanged rule — non-zero only when nothing succeeded
	// (processed==0 && backgrounded==0 && failed>0). Dropped + pending are NOT
	// failures, so they leave the `failed` bucket and never trip exit 1 on
	// their own (the false alarm this feature removes). But when there ARE
	// genuine failures alongside drops/holds, "every pending handoff failed"
	// would overstate — a held/dropped entry did not fail — so gate that exact
	// wording on dropped==0 && pending==0.
	if processed == 0 && backgrounded == 0 && failed > 0 {
		if dropped == 0 && pending == 0 {
			return errors.New("fleet drain: every pending handoff failed")
		}
		return fmt.Errorf("fleet drain: %d handoff(s) failed (no handoff completed)", failed)
	}
	return nil
}

// drainOne handles a single queue file.
//
// The lease-aware COORD-vs-WORKER classification routes the behavior
// (DESIGN-handoff-drain-storm-leak §3(D), PR3):
//
//   - COORD handoff on a lease-capable platform: the BOUNDED, lease-aware path
//     (drainOneLeaseAware) — the lease is the single-flight guarantee, so the
//     graceful/hung path holds NO lock across kill/escalate (the structural
//     fix for the forever-held lock + 81-drain leak).
//   - WORKER (non-coord) handoff, or any handoff on a non-lease platform:
//     the LEGACY path. A worker
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
// Resume under it. Retained for worker handoffs and platforms without the
// coordinator lease primitive.
//
// drain-nonforcing (DESIGN-drain-nonforcing): before the opt-out refusal it
// classifies the WORKER handoff by resolved backing-task status and stops
// FORCING. Coord entries are never classified.
//
//	                  drainOneLegacy(req)
//	                         │
//	       IsCoordSpawn? ────┤ yes → lock+Resume (coord recovery, UNCHANGED)
//	                         │ no
//	classifyBackingTask(project, taskID)
//	     ├─ terminal (done/abandoned)          → queue.Delete + errDropped [LOCK-FREE]
//	     ├─ unresolvable (tasks.md READ ERROR)  → errHeldPending            [no Resume]
//	     └─ live (incl. no tracked row at all)  → lock+Resume ──► handoffop opt-out → ErrHeldOptOut  [pending]
//
// The drop/unresolvable paths run LOCK-FREE — LockAgent is taken only on the
// live/coord Resume path. Moving it inside that branch is load-bearing: a moot
// drop must not wait on the 5-minute agentLockTimeout behind a held lock.
//
// KNOWN ROOT CAUSE (why the lease-aware path exists): the lock+Resume tail
// holds the per-agent flock across the ENTIRE Resume — tmux probes,
// AtomicCoordSwap's nested flock, spawn — any of which can hang; the
// lease-aware path drops this for coords on lease builds.
func drainOneLegacy(req queue.SpawnFresh, path string, graceMillis int,
	stdout, stderr io.Writer) error {

	// A coord entry is NEVER status-classified or dropped. On lease builds
	// drainOne already routed it to drainOneLeaseAware; this guard covers the
	// NON-lease build where everything falls here, so dead-coord recovery stays
	// intact on every platform. Route it straight to the lock+Resume tail.
	if !spawn.IsCoordSpawn(req.TaskID, req.Project) {
		switch class, status := classifyBackingTask(req.Project, req.TaskID); class {
		case taskTerminal:
			// DROP a moot handoff. No LockAgent: the drop only removes a file
			// and spawns nothing; a concurrent consumer that already deleted it
			// hits queue.Delete's idempotent ENOENT->nil. No rename to
			// `.cancelled` (nothing reaps it → a new leak).
			if err := queue.Delete(path); err != nil {
				return fmt.Errorf("drain: drop moot handoff %s: %w", req.OldAgentID, err)
			}
			emitDropDiagnostic(req, status, stdout)
			return errDropped
		case taskUnresolvable:
			// HOLD: never drop an unresolved status (not proof of done).
			// Preserve the queue file, report pending (not a failure).
			return errHeldPending
		}
		// taskLive falls through to the lock+Resume tail below.
	}

	// Live worker OR coord (non-lease build): take the per-agent flock and run
	// Resume under it — the serialization contract Resume requires.
	release, err := state.LockAgent(req.OldAgentID)
	if err != nil {
		return fmt.Errorf("lock agent %s: %w", req.OldAgentID, err)
	}
	defer release()
	return handoffop.Resume(req, path, graceMillis, stdout, stderr)
}

// drainTaskClass buckets a worker handoff by its RESOLVED backing-task status.
type drainTaskClass int

const (
	taskLive         drainTaskClass = iota // non-terminal row (or no tracked row) → resume via the existing route
	taskTerminal                           // done/abandoned → DROP the moot handoff
	taskUnresolvable                       // tasks.md/archive genuinely UNREADABLE → HOLD pending (never drop)
)

// classifyBackingTask resolves taskID's status for project (tasks.md, then
// tasks-archive.md) and buckets it. It reads the RESOLVED status — archive
// PRESENCE alone never drops, since a task can be archived while still
// in-progress.
//
// A READ ERROR (corrupt/unreadable tasks.md or tasks-archive.md) is SWALLOWED
// → taskUnresolvable: returning it up to runDrain would count failed++ and
// resurrect the exit-1 banner this feature removes. An unresolvable status is
// HELD, never dropped — it is not proof of done.
//
// taskID simply ABSENT from both files is NOT unresolvable — it is taskLive
// (codex/adversarial-review iter-1 [P1]). A task_id that was never registered
// in tasks.md is the ORDINARY case for a raw/ad-hoc `fleet dispatch <id>` that
// never touched the task-tracking system (internal/spawn/coord_config.go's own
// "raw spawn" comment documents an empty TaskID as first-class), and this
// codebase's own baseline drain tests confirm it: every pre-existing auto-
// resume test had no tracked task at all. Treating "no row" the same as a
// genuine read error would silently convert a previously-working auto-resume
// into a handoff HELD pending forever with no forced failure to alert anyone —
// exactly the kind of silent breakage this feature exists to remove. Falling
// through to the existing live/opt-out route reproduces pre-drain-nonforcing
// behavior exactly (auto-resume succeeds; an opted-out worker still refuses via
// handoffop's ErrHeldOptOut, bucketed pending same as before). Only a genuine
// I/O failure reading tasks.md/tasks-archive.md — a real signal something is
// wrong with the project's state — holds pending pre-emptively.
//
// Uses readTasks/readArchiveTasks (resolved Task.Status), NOT internal/gc's
// loadTaskStatusOnDisk, which coarsely maps any archived row to "done" and
// would wrongly drop an archived-in-progress task.
func classifyBackingTask(project, taskID string) (drainTaskClass, tasks.Status) {
	if project == "" {
		// A missing project is NOT the same as an untracked task_id: it
		// means the queue file (or the record it was derived from) is
		// malformed/legacy — state.ProjectDir("") silently resolves to an
		// internal "_default" fallback directory, a DIFFERENT project than
		// whatever the caller actually meant. Guessing here risks reading
		// the wrong project's tasks.md. Hold pending rather than fall
		// through (2nd-round adversarial review, low-likelihood but
		// unbounded-blast-radius if it ever happens).
		return taskUnresolvable, ""
	}
	if taskID == "" {
		// Raw spawn (no task_id at all, project present) — never a signal
		// to hold; fall through to the existing live/opt-out route
		// unchanged.
		return taskLive, ""
	}
	f, _, err := readTasks(project)
	if err != nil {
		return taskUnresolvable, "" // read error swallowed → hold, never fail
	}
	if t, gerr := f.Get(taskID); gerr == nil {
		return classForStatus(t.Status), t.Status
	}
	arch, err := readArchiveTasks(project)
	if err != nil {
		return taskUnresolvable, ""
	}
	for _, t := range arch {
		if t.Slug == taskID {
			return classForStatus(t.Status), t.Status
		}
	}
	// Not found in either file: the task was never tracked (ad hoc / pre-
	// task-tracking dispatch) — not a signal to hold. Fall through to the
	// existing live/opt-out route, same as taskID=="" above.
	return taskLive, ""
}

func classForStatus(s tasks.Status) drainTaskClass {
	if s == tasks.StatusDone || s == tasks.StatusAbandoned {
		return taskTerminal
	}
	return taskLive
}

// emitDropDiagnostic surfaces a dropped moot handoff to stdout + fleetlog
// (feedback_surface_dont_silo). When the OLD session is DEFINITIVELY alive it
// appends a manual-handoff hint for the rare done-but-still-active anomaly.
// SessionAlive is tristate: a probe error OMITS the hint but never aborts the
// drop. Uses tmux.SessionName (not a hand-written fleet-<id>) to avoid drift.
func emitDropDiagnostic(req queue.SpawnFresh, status tasks.Status, stdout io.Writer) {
	msg := fmt.Sprintf("fleet drain: %s dropped — backing task %s is %s",
		req.OldAgentID, req.TaskID, status)
	if alive, perr := tmux.SessionAlive(tmux.SessionName(req.OldAgentID)); perr == nil && alive {
		msg += fmt.Sprintf(" — agent live; run 'fleet handoff %s' if it still needs a successor", req.OldAgentID)
	}
	_, _ = fmt.Fprintln(stdout, msg)
	fleetlog.Log(fleetlog.CompCLI, "drain.dropped", "info", fleetlog.Fields{
		Agent: req.OldAgentID,
		Proj:  req.Project,
		Slug:  req.TaskID,
		Msg:   msg,
	})
}
