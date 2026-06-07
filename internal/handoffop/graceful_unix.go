//go:build linux || darwin

// graceful_unix.go — the IN-PROCESS graceful handoff the OLD coord drives
// itself (DESIGN-handoff-drain-storm-leak §3(A), PR3). It replaces the
// external "army of fleet drain processes racing a forever-held lock" with
// a self-driven sequence:
//
//	┌─────────────────────────────────────────────────────────────────┐
//	│ OLD coord (leader, holds lease)            STANDBY coord (warm)   │
//	│   GracefulHandoff():                                              │
//	│     1. spawn ONE standby (coord-run --standby) ─────▶ poll loop   │
//	│     2. write handoff doc        (atomic, fsync)      (busy: BUSY) │
//	│     3. write checkpoint         (atomic, fsync)                   │
//	│     4. drain in-flight work safely (no crash)                     │
//	│     5. write handoff-complete-<epoch>.json BARRIER                │
//	│        (atomic, ONLY after 2+3 fsynced)                           │
//	│   OLD exits ──▶ kernel releases flock ──▶ standby poll ACQUIRES   │
//	└─────────────────────────────────────────────────────────────────┘
//
// The barrier ordering is load-bearing: the barrier is the drain
// verifier's "safe to graceful-kill OLD" signal, so it must NEVER appear on
// disk before the doc + checkpoint are durable. If any earlier step fails,
// GracefulHandoff returns WITHOUT writing the barrier (the drain path then
// escalates to the safety net rather than seeing a false "all clean").
//
// Build-tagged linux||darwin because the lease primitive it reads
// (coordlock.CurrentEpoch / BarrierPath) is itself gated to those GOOS
// values. Other Unix targets compile graceful_other.go's stub, which
// refuses (the whole lease path is unsupported there). All gated behind
// FLEET_LEASE_FAILOVER (default OFF) — the caller passes the flag check in.

package handoffop

import (
	"fmt"
	"io"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/coordlock"
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
}

// GracefulHandoffDeps are the injectable seams that keep GracefulHandoff
// deterministic under test (no real spawn, no real disk, no real clock).
// Production callers use DefaultGracefulHandoffDeps().
type GracefulHandoffDeps struct {
	// SpawnStandby spawns exactly ONE warm-standby coord
	// (coord-run --standby) in the background. Production wires a closure
	// that calls spawn.Spawn with Options{Standby: true}. It MUST be called
	// exactly once per GracefulHandoff.
	SpawnStandby func() error
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
//
// On success the OLD coord may exit; the kernel releases the flock and the
// standby's next poll acquires it. On any error the barrier is absent and
// the OLD coord stays leader until the drain safety net (or a retry)
// recovers — never a torn/false barrier.
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
	if err := d.SpawnStandby(); err != nil {
		return fmt.Errorf("handoffop.GracefulHandoff: spawn standby for project %q: %w", project, err)
	}
	_, _ = fmt.Fprintf(stderr, "graceful-handoff: spawned standby coord for %s (epoch %d)\n", project, epoch)

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
