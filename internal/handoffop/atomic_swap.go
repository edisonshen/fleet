// Package handoffop — atomic_swap.go holds AtomicSwap, the cross-path
// orchestrator that enforces the "exactly one live coord per project,
// always" invariant on every replacement code path.
//
// Why this exists:
//
//   Six different sites in cmd/fleet/handoff.go + internal/handoffop
//   re-implemented variations of the same swap dance (spawn NEW, verify
//   NEW alive, kill OLD, verify OLD dead, archive OLD, move marker).
//   The preceding leak-plug PR (DropReplacementRecord) closed the worst
//   leak surface — probe-failure → silent orphan — but the broader
//   invariant that the OLD agent is never archived while its tmux
//   session is still alive, and the NEW agent is never promoted while
//   its tmux session is dead, still depended on every call site getting
//   the ordering right by hand. AtomicSwap centralizes the orchestration
//   so the invariant holds even when a new replacement path lands.
//
// What AtomicSwap is NOT:
//
//   - It is NOT a spawner. Caller drives spawn.Spawn with the options
//     it needs. AtomicSwap takes the freshly-spawned NewRec and runs
//     the verify+retire dance.
//
//   - It is NOT a substitute for the post-spawn readiness wait. The
//     wait is policy-specific (readiness poll, prompt send) and stays
//     at the call site. AtomicSwap focuses on the OLD↔NEW liveness
//     invariant.
//
// Invariant:
//
//   At every observable moment between AtomicSwap's entry and successful
//   return, EITHER the OLD agent OR the NEW agent is the live coord for
//   the task — never both, never neither. Failure paths must preserve
//   that invariant before returning the error.

package handoffop

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tmux"
)

// SwapResult records what AtomicSwap did so callers can compose
// post-swap actions (prompt delivery, queue cleanup) without re-probing.
type SwapResult struct {
	// OldArchived is true when oldRec.Archive() succeeded. False on
	// the rare archive-failed-then-removed fallback path; callers
	// that care can re-Stat the archive dir.
	OldArchived bool
	// MarkerSwapped is true when AtomicSwap moved a coord-spawn marker
	// from OldRec to NewRec. False when no project on old, no marker,
	// or the marker pointed at someone else.
	MarkerSwapped bool
}

// AtomicSwapOpts bundles AtomicSwap's inputs. Required fields are
// validated at entry; missing fields return ErrInvalidSwap.
type AtomicSwapOpts struct {
	// OldRec is the agent being retired. Required.
	OldRec *agent.Record
	// NewRec is the freshly-spawned replacement. Required.
	NewRec *agent.Record
	// Stderr receives non-fatal notes (kill-but-gone races, marker
	// warnings). nil-tolerant.
	Stderr io.Writer
	// SwapMarker controls whether AtomicSwap re-points a coord-spawn
	// marker from OldRec.Project at NewRec. Most replacement paths
	// set this true; the dead-coord-recovery path in dispatch.go
	// sets it false because the new spawn has already taken the
	// marker via state.WriteCoordSpawnMarker.
	SwapMarker bool
}

// ErrInvalidSwap is returned when AtomicSwap's inputs are malformed.
// Kept separate from probe/kill errors so call sites can branch on
// "bug in fleet code" vs "transient tmux flake."
var ErrInvalidSwap = errors.New("invalid atomic swap inputs")

// tmuxKillForSwap / tmuxSessionAliveForSwap are package-level vars so
// tests inject fakes. Production binds tmux.Kill / tmux.SessionAlive.
// Separate from the names in replacement_cleanup.go so a test can stub
// AtomicSwap behavior independently of DropReplacementRecord.
var (
	tmuxKillForSwap         = tmux.Kill
	tmuxSessionAliveForSwap = tmux.SessionAlive
)

// AtomicSwap drives the post-spawn retire-and-promote sequence with
// the invariant outlined at the top of this file. Caller has already
// done:
//
//   1. agent.Load both records (OldRec live, NewRec just-spawned).
//   2. spawn.Spawn the replacement (NewRec carries the result).
//   3. Any pre-swap readiness wait the caller's policy requires.
//
// AtomicSwap then:
//
//   A. Re-verifies NewRec's tmux session is alive via SessionAlive
//      (NOT HasSession — tristate distinguishes "vanished" from
//      "probe failed"). Confirmed-dead rolls back NewRec via
//      DropReplacementRecord and returns. Probe ambiguous refuses
//      to proceed and returns — operator triages.
//
//   B. Kills OldRec.TmuxSession via tmux.Kill (idempotent +
//      SessionAlive-backed). On kill error, re-probes via
//      SessionAlive:
//        - still alive → roll back NewRec; OLD stays live; return.
//        - probe ambiguous → roll back NewRec; OLD untouched; return.
//        - confirmed dead → race; log note; proceed to archive.
//
//   C. Optionally re-points the coord-spawn marker from
//      OldRec.Project at NewRec. Best-effort.
//
//   D. Archives OldRec via oldRec.Archive() with fallback to direct
//      os.Remove of the live record.
//
// Returns SwapResult on success; (zero, error) on any failure.
// Callers MUST treat AtomicSwap's error as "do not delete the queue
// file; do not send the resume prompt".
func AtomicSwap(opts AtomicSwapOpts) (SwapResult, error) {
	if opts.OldRec == nil || opts.NewRec == nil {
		return SwapResult{}, fmt.Errorf("%w: OldRec and NewRec required", ErrInvalidSwap)
	}
	if opts.OldRec.ID == "" || opts.NewRec.ID == "" {
		return SwapResult{}, fmt.Errorf("%w: both records need non-empty IDs", ErrInvalidSwap)
	}
	if opts.OldRec.TmuxSession == "" {
		return SwapResult{}, fmt.Errorf("%w: OldRec %s has empty TmuxSession; use agent.Archive directly",
			ErrInvalidSwap, opts.OldRec.ID)
	}

	// Step A: re-verify NEW is alive under the "every observable
	// moment" invariant.
	alive, perr := tmuxSessionAliveForSwap(opts.NewRec.TmuxSession)
	switch {
	case perr != nil:
		return SwapResult{}, fmt.Errorf(
			"swap %s→%s: probe NEW session %s failed: %w (refusing to archive OLD; NEW record preserved)",
			opts.OldRec.ID, opts.NewRec.ID, opts.NewRec.TmuxSession, perr)
	case !alive:
		if dropErr := DropReplacementRecord(opts.NewRec.TmuxSession, opts.NewRec.ID, opts.Stderr); dropErr != nil {
			return SwapResult{}, fmt.Errorf(
				"swap %s→%s: NEW session %s confirmed dead AND drop-NEW-record failed (%w); OLD %s untouched, NEW record may be stuck",
				opts.OldRec.ID, opts.NewRec.ID, opts.NewRec.TmuxSession, dropErr, opts.OldRec.ID)
		}
		return SwapResult{}, fmt.Errorf(
			"swap %s→%s: NEW session %s exited before swap could complete; OLD %s untouched, retry",
			opts.OldRec.ID, opts.NewRec.ID, opts.NewRec.TmuxSession, opts.OldRec.ID)
	}

	// Step B: kill OLD.
	if err := tmuxKillForSwap(opts.OldRec.TmuxSession); err != nil {
		stillAlive, postProbeErr := tmuxSessionAliveForSwap(opts.OldRec.TmuxSession)
		switch {
		case postProbeErr != nil:
			if dropErr := DropReplacementRecord(opts.NewRec.TmuxSession, opts.NewRec.ID, opts.Stderr); dropErr != nil {
				return SwapResult{}, fmt.Errorf(
					"swap %s→%s: kill OLD %s failed (%w), post-kill probe ambiguous (%w), AND NEW rollback failed (%w); both records preserved",
					opts.OldRec.ID, opts.NewRec.ID, opts.OldRec.TmuxSession, err, postProbeErr, dropErr)
			}
			return SwapResult{}, fmt.Errorf(
				"swap %s→%s: kill OLD %s failed (%w) AND post-kill probe ambiguous (%w); NEW rolled back, OLD preserved",
				opts.OldRec.ID, opts.NewRec.ID, opts.OldRec.TmuxSession, err, postProbeErr)
		case stillAlive:
			if dropErr := DropReplacementRecord(opts.NewRec.TmuxSession, opts.NewRec.ID, opts.Stderr); dropErr != nil {
				return SwapResult{}, fmt.Errorf(
					"swap %s→%s: OLD session %s still alive after kill failure (%w) AND NEW rollback failed (%w); both records preserved",
					opts.OldRec.ID, opts.NewRec.ID, opts.OldRec.TmuxSession, err, dropErr)
			}
			return SwapResult{}, fmt.Errorf(
				"swap %s→%s: OLD session %s still alive after kill failure: %w (NEW rolled back)",
				opts.OldRec.ID, opts.NewRec.ID, opts.OldRec.TmuxSession, err)
		default:
			if opts.Stderr != nil {
				_, _ = fmt.Fprintf(opts.Stderr,
					"note: kill %s reported error but session is gone: %v\n",
					opts.OldRec.TmuxSession, err)
			}
		}
	}

	res := SwapResult{}

	// Step C: re-point coord-spawn marker if asked.
	if opts.SwapMarker && opts.OldRec.Project != "" {
		if wantID := state.ReadCoordSpawnMarker(opts.OldRec.Project); wantID == opts.OldRec.ID {
			if werr := state.WriteCoordSpawnMarker(opts.OldRec.Project, opts.NewRec.ID); werr != nil {
				if opts.Stderr != nil {
					_, _ = fmt.Fprintf(opts.Stderr,
						"warning: swap coord-spawn marker for project %s: %v\n",
						opts.OldRec.Project, werr)
				}
			} else {
				res.MarkerSwapped = true
			}
		}
	}

	// Step D: archive OLD.
	if err := opts.OldRec.Archive(); err != nil {
		path, perr := state.AgentPath(opts.OldRec.ID)
		if perr == nil {
			if rmErr := os.Remove(path); rmErr == nil || errors.Is(rmErr, os.ErrNotExist) {
				if opts.Stderr != nil {
					_, _ = fmt.Fprintf(opts.Stderr,
						"warning: archive %s: %v (live record removed instead, archive copy lost)\n",
						opts.OldRec.ID, err)
				}
			} else {
				return res, fmt.Errorf(
					"swap %s→%s: archive OLD failed (%w) AND fallback remove failed (%w); NEW live but OLD record stuck",
					opts.OldRec.ID, opts.NewRec.ID, err, rmErr)
			}
		} else {
			return res, fmt.Errorf(
				"swap %s→%s: archive OLD failed (%w) AND could not resolve OLD's path (%w); NEW live",
				opts.OldRec.ID, opts.NewRec.ID, err, perr)
		}
	} else {
		res.OldArchived = true
	}
	return res, nil
}
