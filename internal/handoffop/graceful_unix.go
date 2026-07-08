//go:build linux || darwin

// graceful_unix.go — the graceful handoff barrier + its completion phase
// (DESIGN-handoff-drain-storm-leak §3(A) PR3 prepared the barrier;
// DESIGN-coord-spawn-unified-standby §5b/§6 PR3 built the completion).
// It replaces the external "army of fleet drain processes racing a
// forever-held lock" with one fixed sequence:
//
//	┌───────────────────────────────────────────────────────────────────┐
//	│ ORCHESTRATOR (fleet handoff / drain)       STANDBY coord (warm)    │
//	│   GracefulHandoff():                                               │
//	│     1. spawn ONE standby (coord-run --standby) ─────▶ poll loop    │
//	│     2. write handoff doc        (atomic, fsync)      (busy: BUSY)  │
//	│     3. write checkpoint         (atomic, fsync)                    │
//	│     4. drain in-flight work safely (no crash)                      │
//	│     5. write handoff-complete-<epoch>.json BARRIER                 │
//	│        (atomic, ONLY after 2+3 fsynced)                            │
//	│   ── completion phase (only when the seams are wired) ──           │
//	│     6. RetireOld: OLD releases lease + exits ──▶ poll ACQUIRES     │
//	│                                                  (winner reaps     │
//	│                                                  losing siblings)  │
//	│     7. DeliverDocToWinner: poll lock owner ─────▶ inject doc       │
//	│        (mark-after-verified-send; §5c)                             │
//	└───────────────────────────────────────────────────────────────────┘
//
// The barrier ordering is load-bearing: the barrier is the drain
// verifier's "safe to graceful-kill OLD" signal, so it must NEVER appear on
// disk before the doc + checkpoint are durable. If any earlier step fails,
// GracefulHandoff returns WITHOUT writing the barrier (the drain path then
// escalates to the safety net rather than seeing a false "all clean").
//
// Completion-phase failure semantics are the inverse: once the barrier is
// durable the standby is NEVER reaped — it (or the lazy [a]-attach recovery
// from the still-pending doc) is the vehicle that finishes a half-done
// handoff. Retire/delivery errors propagate so the caller keeps its durable
// queue PENDING; the next drain/attach pass re-delivers (a benign duplicate
// is preferred over a silent strand, §5c).
//
// Build-tagged linux||darwin because the lease primitive it reads
// (coordlock.CurrentEpoch / BarrierPath) is itself gated to those GOOS
// values. Other Unix targets compile graceful_other.go's stub, which
// refuses (the whole lease path is unsupported there).

package handoffop

import (
	"fmt"
	"io"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/coordlock"
	"github.com/edisonshen/fleet/internal/spawn"
	"github.com/edisonshen/fleet/internal/state"
)

// GracefulHandoffInputs is the data the OLD coord hands to GracefulHandoff.
type GracefulHandoffInputs struct {
	// OldRec is the outgoing coord's agent record (its project is the
	// lease/barrier scope). Required.
	OldRec *agent.Record
	// HandoffDocPath + HandoffDoc are the doc to persist for the successor
	// to read. DocPath is required; empty DocContent means "the doc already
	// exists on disk" (the producer wrote it) and the write step is skipped.
	HandoffDocPath string
	HandoffDoc     []byte
	// CheckpointPath + Checkpoint are the durable state snapshot the standby
	// rebuilds from. CheckpointPath empty skips the checkpoint write (the
	// rolling coord-checkpoint.md the skill maintains is the fallback).
	CheckpointPath string
	Checkpoint     []byte
	// StandbyTimeout is passed to the SpawnStandby seam so production
	// closures wire spawn.Options.StandbyTimeout for the one standby. 0 uses
	// spawn.DefaultStandbyTimeout.
	StandbyTimeout time.Duration
}

// GracefulHandoffDeps are the injectable seams that keep GracefulHandoff
// deterministic under test (no real spawn, no real disk, no real clock).
// Production callers use DefaultGracefulHandoffDeps().
type GracefulHandoffDeps struct {
	// SpawnStandby spawns exactly ONE warm-standby coord
	// (coord-run --standby) in the background. Production wires a closure
	// that calls spawn.Spawn with Options{StandbyTimeout: timeout}. It MUST
	// be called exactly once per GracefulHandoff. It should return enough
	// context (via a closure capture) for ReapStandby to tear the standby
	// down on a post-spawn abort.
	SpawnStandby func(timeout time.Duration) error
	// ReapStandby tears down the standby spawned by SpawnStandby. It is
	// called ONLY when a post-spawn step fails (doc/checkpoint/drain/barrier)
	// so a half-done graceful handoff does not leave a warm standby polling
	// to acquire the lease WITHOUT the completed checkpoint + barrier this
	// flow requires (codex PR3 iter-2 [P2]). nil = no reap (the caller
	// accepts a leaked standby — discouraged). Best-effort: a reap error is
	// surfaced to stderr but the original failure is what GracefulHandoff
	// returns.
	ReapStandby func() error
	// WriteAtomic persists data to path with .tmp->fsync->rename semantics
	// (a torn write is never renamed into place). Production: state.WriteAtomic.
	WriteAtomic func(path string, data []byte) error
	// DrainInFlight drains the coord's in-flight work safely (flush pending
	// dispatches/PR actions to the durable checkpoint) WITHOUT crashing
	// mid-flight. nil = nothing to drain (a no-op is valid). An error here
	// aborts BEFORE the barrier so the safety net handles recovery.
	DrainInFlight func() error
	// CurrentEpoch returns the project's current lease epoch (the barrier's
	// generation tag). Production: coordlock.CurrentEpoch.
	CurrentEpoch func(project string) (int64, bool)
	// BarrierPath resolves the handoff-complete-<epoch>.json path.
	// Production: coordlock.BarrierPath.
	BarrierPath func(project string, epoch int64) (string, error)
	// RetireOld retires the OLD coord AFTER the barrier is durable — the
	// marker-commit + /exit + grace + kill + archive sequence (production:
	// an AtomicCoordSwap closure; GracefulCoordSwap wires it). Retiring OLD
	// makes its coord-run supervisor exit, which releases the lease so a
	// polling standby can win it. nil (with DeliverDocToWinner nil) =
	// prepare-only mode: GracefulHandoff returns right after the barrier
	// and the OLD coord self-retires (the original stub contract).
	RetireOld func() error
	// DeliverDocToWinner polls the project lock for the WINNING standby and
	// injects the handoff-doc resume prompt into it (production: a caller
	// closure over handoffdelivery.DeliverToCurrentOwner — verified send,
	// marker promotion). The winner reaps losing sibling standbys itself on
	// acquire (coord-run's sweepStaleCompetitors, PR2); this seam only
	// addresses whoever won. Returns the record that received the doc.
	// Must be set iff RetireOld is set.
	DeliverDocToWinner func() (*agent.Record, error)
	// Stderr receives progress + diagnostic output (surface-don't-silo).
	// nil = io.Discard (the caller usually passes the drain/coord stderr).
	Stderr io.Writer
}

// DefaultGracefulHandoffDeps returns the production seams EXCEPT
// SpawnStandby + DrainInFlight, which the caller must supply (they need
// project-specific spawn options / coord state the handoffop package does
// not own). A nil SpawnStandby is rejected by GracefulHandoff.
func DefaultGracefulHandoffDeps() GracefulHandoffDeps {
	return GracefulHandoffDeps{
		WriteAtomic:  state.WriteAtomic,
		CurrentEpoch: coordlock.CurrentEpoch,
		BarrierPath:  coordlock.BarrierPath,
	}
}

// GracefulHandoff runs the in-process graceful handoff for the OLD coord.
// It is the producer side of the warm-standby flow; the standby (spawned in
// step 1) and the bounded drain verifier (cmd/fleet/drain.go) are the
// consumers. Steps run in a FIXED order; any failure short-circuits BEFORE
// the barrier so the drain path never reads a premature "all clean."
//
//  1. spawn ONE standby                       (background poller)
//  2. write handoff doc      (atomic, fsync)  } durable BEFORE the
//  3. write checkpoint       (atomic, fsync)  } barrier — load-bearing
//  4. drain in-flight work safely (no crash)
//  5. write handoff-complete-<epoch>.json     (atomic; barrier)
//  6. RetireOld    — OLD releases the lease + exits        } completion
//  7. DeliverDocToWinner — poll lock winner, inject doc    } (PR3; only
//     } when wired)
//
// Prepare-only mode (nil completion seams): on success the OLD coord may
// exit; the kernel releases the flock and the standby's next poll acquires
// it. On any pre-barrier error the barrier is absent and the OLD coord
// stays leader until the drain safety net (or a retry) recovers — never a
// torn/false barrier. Post-barrier (completion) errors leave the barrier in
// place and the standby alive: the drain verifier / lazy attach recovery
// finishes from the durable doc.
func GracefulHandoff(in GracefulHandoffInputs, d GracefulHandoffDeps) error {
	d = fillGracefulDeps(d)
	stderr := d.Stderr

	if in.OldRec == nil {
		return fmt.Errorf("handoffop.GracefulHandoff: OldRec required")
	}
	project := in.OldRec.Project
	if project == "" {
		return fmt.Errorf("handoffop.GracefulHandoff: OldRec has no project")
	}
	if d.SpawnStandby == nil {
		return fmt.Errorf("handoffop.GracefulHandoff: SpawnStandby seam required")
	}
	// The completion seams come as a PAIR. Retiring OLD without a delivery
	// seam would strand the doc behind an exited coord; delivering without
	// retiring would poll a lock OLD never releases. Refuse before ANY side
	// effect so a mis-wired caller can't half-run the handoff.
	if (d.RetireOld == nil) != (d.DeliverDocToWinner == nil) {
		return fmt.Errorf(
			"handoffop.GracefulHandoff: completion seams must be set together (RetireOld set=%v, DeliverDocToWinner set=%v)",
			d.RetireOld != nil, d.DeliverDocToWinner != nil)
	}

	// Capture the epoch FIRST — the barrier must name the lease generation
	// that is handing off (the one OLD currently owns), not whatever epoch a
	// concurrent takeover might bump it to mid-handoff. If we cannot read it,
	// abort before doing anything destructive: a barrier without a valid
	// epoch is worse than no barrier (the drain verifier could match the
	// wrong generation). Surface-don't-silo.
	epoch, ok := d.CurrentEpoch(project)
	if !ok {
		return fmt.Errorf(
			"handoffop.GracefulHandoff: no lease epoch for project %q (lease not held?); refusing graceful handoff",
			project)
	}

	// Step 1: spawn exactly ONE standby. Do this FIRST so the warm poller is
	// already racing for the lease by the time OLD exits — minimizing the
	// no-leader gap. A spawn failure aborts: without a standby the graceful
	// path has no receiver, so we must not proceed to write the barrier (the
	// drain safety net / a retry handles it).
	standbyTimeout := in.StandbyTimeout
	if standbyTimeout <= 0 {
		standbyTimeout = spawn.DefaultStandbyTimeout
	}
	if err := d.SpawnStandby(standbyTimeout); err != nil {
		return fmt.Errorf("handoffop.GracefulHandoff: spawn standby for project %q: %w", project, err)
	}
	// "ready", not "spawned": on the converged GracefulCoordSwap route the
	// standby was already spawned by the caller and the seam is a no-op.
	_, _ = fmt.Fprintf(stderr, "graceful-handoff: standby coord ready for %s (epoch %d)\n", project, epoch)

	// Steps 2-5 run AFTER the standby is alive. If ANY of them fails we must
	// REAP the standby (codex PR3 iter-2 [P2]): a standby left polling after a
	// half-done handoff could acquire the lease WITHOUT the completed
	// checkpoint + barrier this flow guarantees. OLD stays leader and a
	// retry / the safety net recovers cleanly with no orphan standby.
	if err := gracefulDurableSteps(in, d, project, epoch, stderr); err != nil {
		if d.ReapStandby != nil {
			if rerr := d.ReapStandby(); rerr != nil {
				_, _ = fmt.Fprintf(stderr,
					"graceful-handoff: WARNING — post-spawn abort AND standby reap failed for %s: %v "+
						"(orphan standby may be polling; `fleet doctor` / gc will reap it)\n",
					project, rerr)
			} else {
				_, _ = fmt.Fprintf(stderr,
					"graceful-handoff: aborted after spawning standby for %s; reaped the standby (OLD stays leader)\n",
					project)
			}
		}
		return err
	}

	// Completion phase (PR3). Prepare-only callers stop at the barrier.
	if d.RetireOld == nil {
		return nil
	}

	// Step 6: retire OLD. The barrier is durable, so OLD may now release the
	// lease and exit; the polling standby (or a duplicate-attach racer — the
	// winner reaps the losers on acquire, PR2) wins the freed lock. On error
	// the standby is NOT reaped: the barrier is up, so the drain verifier /
	// a retry finishes the handoff — the standby is the recovery vehicle.
	if err := d.RetireOld(); err != nil {
		return fmt.Errorf(
			"handoffop.GracefulHandoff: retire old coord %s for project %q: %w (barrier %d stays; standby left polling for recovery)",
			in.OldRec.ID, project, err, epoch)
	}

	// Step 7: deliver the doc to whoever WON the lock. On error the doc /
	// caller queue stays PENDING so a later drain/attach re-delivers (§5c) —
	// again no standby reap (the winner may be the live coord by now).
	winner, err := d.DeliverDocToWinner()
	if err != nil {
		return fmt.Errorf(
			"handoffop.GracefulHandoff: deliver handoff doc to the lock winner for project %q: %w (doc stays pending; next drain/attach re-delivers)",
			project, err)
	}
	if winner != nil {
		_, _ = fmt.Fprintf(stderr,
			"graceful-handoff: delivered handoff doc to lock winner %s (%s) for %s\n",
			winner.ID, winner.TmuxSession, project)
	}
	return nil
}

// gracefulEligibleOwnerFn is the seam GracefulSwapEligible reads the current
// healthy lease owner through. Production: coordlock.CurrentOwner; tests
// inject a deterministic fake.
var gracefulEligibleOwnerFn = coordlock.CurrentOwner

// GracefulSwapEligible reports whether a coord swap should route through the
// GracefulHandoff barrier (DESIGN-coord-spawn-unified-standby §6 PR3): this
// platform supports coordinator leases, OLD is the project's CURRENT healthy
// lease owner (a live-coord handoff — a dead-coord recovery has no live
// releaser and keeps the proven cold/bespoke path), and the resume prompt will
// be auto-delivered
// (completion IS delivery; an opt-out handoff has nothing to inject).
//
// Callers additionally gate on the successor being lease-wrapped
// (newRec.LeaseWrapped): only a coord-run --standby poller can win the freed
// lock and heartbeat the lease — a bare successor's lease lapses (the
// fd4ee2e6 duplicate-coord incident).
func GracefulSwapEligible(oldRec *agent.Record, autoResume bool) bool {
	if oldRec == nil || oldRec.Project == "" || !autoResume {
		return false
	}
	if !coordlock.FailoverEnabled() {
		return false
	}
	owner, ok := gracefulEligibleOwnerFn(oldRec.Project)
	return ok && owner.AgentID == oldRec.ID
}

// gracefulSwapDepsFn / atomicCoordSwapFn are GracefulCoordSwap's seams.
// Production: the package defaults; tests inject fakes so the composition is
// testable without a real lease/tmux.
var (
	gracefulSwapDepsFn = DefaultGracefulHandoffDeps
	atomicCoordSwapFn  = AtomicCoordSwap
)

// GracefulCoordSwap is the production composition the live-coord handoff
// paths (manual `fleet handoff`, drain-of-live, auto 50/70% — via
// cmd/fleet/handoff.go and retireOldAgent) route through, retiring the
// bespoke "AtomicCoordSwap then separately deliver" sequence PR2 added:
//
//	barrier write (doc is ALREADY durable on disk — the caller wrote it
//	before spawn)  →  retire OLD via AtomicCoordSwap (its coord-run exit
//	releases the lease)  →  deliver(): poll the lock winner, inject the doc.
//
// newRec is the lease-wrapped standby the caller already spawned and probed
// alive — so the SpawnStandby seam is satisfied with a no-op (the "exactly
// one standby" invariant holds: this run spawns zero additional standbys).
// deliver is the caller's lock-owner delivery closure (each caller carries
// its own fallback/IO shape); the record it returns is the WINNER that got
// the doc. AtomicCoordSwap's sentinel errors (ErrOrphanSurvived,
// ErrOldKillProbeAmbiguous, ...) stay errors.Is-reachable through the wrap.
func GracefulCoordSwap(oldRec, newRec *agent.Record, docPath string,
	graceWindow time.Duration, deliver func() (*agent.Record, error),
	stderr io.Writer) (*agent.Record, error) {

	if deliver == nil {
		return nil, fmt.Errorf("handoffop.GracefulCoordSwap: deliver closure required")
	}
	var winner *agent.Record
	deps := gracefulSwapDepsFn()
	deps.SpawnStandby = func(time.Duration) error { return nil } // standby == newRec, already spawned
	deps.RetireOld = func() error {
		_, err := atomicCoordSwapFn(AtomicCoordSwapInputs{
			Project:              oldRec.Project,
			OldRec:               oldRec,
			AlreadySpawnedNewRec: newRec,
			GraceWindow:          graceWindow,
		}, stderr)
		return err
	}
	deps.DeliverDocToWinner = func() (*agent.Record, error) {
		w, err := deliver()
		if err != nil {
			return nil, err
		}
		winner = w
		return w, nil
	}
	deps.Stderr = stderr
	if err := GracefulHandoff(GracefulHandoffInputs{
		OldRec:         oldRec,
		HandoffDocPath: docPath, // already durable — empty HandoffDoc skips the write
	}, deps); err != nil {
		// Pre-retire failure (epoch read / barrier write / seam validation):
		// AtomicCoordSwap never ran, so it never reserved a handoff on OLD's
		// lease. There is nothing to undo — the coord-spawn marker is gone
		// (D3) and the identity commit is the winner's async epoch bump, which
		// only happens once the standby actually acquires the freed flock. OLD
		// still owns the lease, so discovery keeps resolving to OLD.
		//
		// The standby (newRec) is deliberately NOT reaped on this path
		// (codex iter-4 [P1] reviewed + rejected): the callers preserve the
		// durable queue AND the journaled replacement so the retry
		// (resumeHandoff / Resume case 3) REUSES the live standby instead of
		// spawning a second one. If OLD crashes before the retry and the
		// standby wins the freed lease, the pending queue's lock-owner
		// delivery heals it (§5c) — it is never a blank strand. A standby
		// that never wins self-reaps at the standby timeout (10 min).
		return nil, err
	}
	return winner, nil
}

// gracefulDurableSteps runs steps 2-5 (doc, checkpoint, drain, barrier) in
// the fixed order. The barrier is the LAST side effect and is written ONLY
// after doc + checkpoint + drain are durable. Any failure short-circuits
// BEFORE the barrier (so the drain path never reads a premature "all clean").
func gracefulDurableSteps(in GracefulHandoffInputs, d GracefulHandoffDeps,
	project string, epoch int64, stderr io.Writer) error {

	// Step 2: write the handoff doc durably (atomic). Skipped when the
	// producer already wrote it (empty DocContent).
	if in.HandoffDocPath != "" && len(in.HandoffDoc) > 0 {
		if err := d.WriteAtomic(in.HandoffDocPath, in.HandoffDoc); err != nil {
			return fmt.Errorf("handoffop.GracefulHandoff: write handoff doc %s: %w", in.HandoffDocPath, err)
		}
	}

	// Step 3: write the checkpoint durably (atomic). The barrier is written
	// ONLY after this returns, so a torn/failed checkpoint can never be
	// followed by a barrier claiming completion.
	if in.CheckpointPath != "" && len(in.Checkpoint) > 0 {
		if err := d.WriteAtomic(in.CheckpointPath, in.Checkpoint); err != nil {
			return fmt.Errorf("handoffop.GracefulHandoff: write checkpoint %s: %w", in.CheckpointPath, err)
		}
	}

	// Step 4: drain in-flight work safely. Must NOT crash mid-flight — on
	// error we abort before the barrier so the safety net replays.
	if d.DrainInFlight != nil {
		if err := d.DrainInFlight(); err != nil {
			return fmt.Errorf("handoffop.GracefulHandoff: drain in-flight work for project %q: %w", project, err)
		}
	}

	// Step 5: write the completion barrier — the LAST step, only now that
	// doc + checkpoint + in-flight drain are all durable. Atomic so a torn
	// .tmp is never renamed into place (T43).
	barrierPath, err := d.BarrierPath(project, epoch)
	if err != nil {
		return fmt.Errorf("handoffop.GracefulHandoff: resolve barrier path for project %q: %w", project, err)
	}
	barrier := fmt.Sprintf(
		"{\"project\":%q,\"old_agent\":%q,\"epoch\":%d}\n",
		project, in.OldRec.ID, epoch)
	if err := d.WriteAtomic(barrierPath, []byte(barrier)); err != nil {
		return fmt.Errorf("handoffop.GracefulHandoff: write barrier %s: %w", barrierPath, err)
	}
	_, _ = fmt.Fprintf(stderr,
		"graceful-handoff: wrote completion barrier %s — OLD coord may exit; standby will acquire the lease\n",
		barrierPath)
	return nil
}

// fillGracefulDeps backfills nil seams with production defaults so a
// partially-specified deps (common in tests) still runs. SpawnStandby +
// DrainInFlight are intentionally NOT defaulted (caller-owned).
func fillGracefulDeps(d GracefulHandoffDeps) GracefulHandoffDeps {
	def := DefaultGracefulHandoffDeps()
	if d.WriteAtomic == nil {
		d.WriteAtomic = def.WriteAtomic
	}
	if d.CurrentEpoch == nil {
		d.CurrentEpoch = def.CurrentEpoch
	}
	if d.BarrierPath == nil {
		d.BarrierPath = def.BarrierPath
	}
	if d.Stderr == nil {
		d.Stderr = io.Discard
	}
	return d
}
