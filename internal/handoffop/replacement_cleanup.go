package handoffop

// This file holds DropReplacementRecord — the unified "this replacement
// looks dead, clean up its record" helper. It exists to plug an orphan
// tmux session leak triggered by fleet-guard auto-handoffs running every
// ~10 minutes per coordinator.
//
// The leak (before this helper landed):
//
//   if !tmux.HasSession(newRec.TmuxSession) {
//       if path, perr := state.AgentPath(newRec.ID); perr == nil {
//           _ = os.Remove(path)
//       }
//       return fmt.Errorf("replacement %s ... already exited", newRec.ID)
//   }
//
// tmux.HasSession returns false for BOTH "session does not exist" AND
// "probe failed" (transport error, lost server, socket unreadable —
// the documented ambiguous behavior at internal/tmux/tmux.go:121). On a
// probe-failure window the session is still alive, but the record gets
// deleted — fleet has no record of the session and nothing kills it. Each
// auto-handoff that hits this window leaks one tmux session; the operator's
// machine OOM-crashed twice after ~68 sessions accumulated in 12 hours.
//
// DropReplacementRecord plugs this by calling tmux.Kill BEFORE the
// os.Remove. tmux.Kill is idempotent (returns nil for already-dead sessions
// via SessionAlive's tristate probe) and surfaces probe failures as real
// errors. If kill or post-probe says the session is still alive, the
// helper refuses to delete the record and returns an error — an operator-
// visible orphan record is far better than a leaked tmux session.
//
// All six call sites that used the leaking pattern were swapped to
// DropReplacementRecord in the same commit as this file.

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tmux"
)

// tmuxKillFn / tmuxSessionAliveFn are package-level vars so tests can
// inject fakes that simulate transient probe failures without spinning up
// a real flaky tmux server. Production uses the real tmux functions.
var (
	tmuxKillFn         = tmux.Kill
	tmuxSessionAliveFn = tmux.SessionAlive
)

// DropReplacementRecord kills the replacement's tmux session (if any),
// then deletes its agent record. Returns an error WITHOUT removing the
// record if the kill fails AND the session is still alive — preserving
// operator-visible state instead of leaking the tmux session.
//
// `session` may be empty: skipped Kill, just removes the record. Used
// from paths where the record was written before tmux.Spawn ran (rare).
//
// `recID` MUST be non-empty.
//
// On success: returns nil with the record removed and the tmux session
// either confirmed gone or just-killed. Caller proceeds with its normal
// "replacement dropped" return.
//
// On failure: returns an error describing why the cleanup couldn't be
// completed safely. Caller wraps and propagates — the surrounding
// handoff path should bail out, leaving the orphan visible to the
// operator for manual triage.
//
// stderr is for the "kill reported error but session is gone" note (rare:
// kill returns nil for already-dead sessions, but a tmux race can still
// trigger this). nil-tolerant.
func DropReplacementRecord(session, recID string, stderr io.Writer) error {
	if recID == "" {
		return errors.New("DropReplacementRecord: empty recID")
	}
	if session != "" {
		if err := tmuxKillFn(session); err != nil {
			// tmux.Kill internally uses SessionAlive (tristate). An error
			// here means either the pre-probe failed (transport / binary
			// missing) or the kill-session call itself failed. Either way,
			// re-probe with SessionAlive to disambiguate before removing
			// the record. If the session is still alive (or the probe is
			// ambiguous), refuse — preserving the record means the
			// operator sees the orphan in `fleet status` and can clean
			// up manually, instead of fleet silently leaking a tmux
			// session that nothing tracks.
			alive, perr := tmuxSessionAliveFn(session)
			if perr != nil {
				return fmt.Errorf(
					"kill tmux session %s failed (%w) AND post-kill probe also failed (%w); refusing to remove record %s — investigate manually (orphan record preserved)",
					session, err, perr, recID)
			}
			if alive {
				return fmt.Errorf(
					"kill tmux session %s failed and session still alive: %w; refusing to remove record %s — investigate manually (orphan record preserved)",
					session, err, recID)
			}
			// Genuine race: kill failed but session is now gone (operator
			// killed it manually, OS reaped tmux, etc.). Safe to proceed.
			if stderr != nil {
				_, _ = fmt.Fprintf(stderr,
					"note: kill %s reported error but session is gone: %v\n",
					session, err)
			}
		}
	}
	path, perr := state.AgentPath(recID)
	if perr != nil {
		return fmt.Errorf("resolve agent record path for %s: %w", recID, perr)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove agent record %s: %w", recID, err)
	}
	return nil
}

// RollbackCoordMarkerIfPointingAt restores the coord-spawn marker to
// `oldID` when it currently points at `staleNewID`. Used after a
// replacement record + session have been dropped (DropReplacementRecord)
// to undo a prior swap's eager marker write — without this, the marker
// keeps pointing at a deleted ID and the retry path's isCoordSwap check
// (which only fires on marker == oldRec.ID) misses the swap, letting
// a duplicate coord spawn.
//
// Codex iter-12 [P1]: Resume (handoffop.go:125-134) and the manual
// handoff recovery path (cmd/fleet/handoff.go:378-387) both fall
// through to a fresh spawn after dropping a dead replacement. A
// previous swap may have committed marker == staleNewID before
// returning ErrOrphanSurvived / ErrOldKillProbeAmbiguous / similar;
// without rollback, the retry's isCoordSwap goes false, the inline
// retire path runs (no AtomicCoordSwap), and `[a]` ends up spawning
// a second coord because the marker still points at the deleted ID.
//
// Idempotent: a no-op when project is empty, marker is already at
// oldID, or marker is anything other than staleNewID. Best-effort —
// marker write failures print a warning but don't error out, matching
// the rollback semantics elsewhere in the codebase.
func RollbackCoordMarkerIfPointingAt(project, oldID, staleNewID string, stderr io.Writer) {
	if project == "" || staleNewID == "" {
		return
	}
	if state.ReadCoordSpawnMarker(project) != staleNewID {
		return
	}
	if werr := state.WriteCoordSpawnMarker(project, oldID); werr != nil {
		if stderr != nil {
			_, _ = fmt.Fprintf(stderr,
				"warning: rollback coord-spawn marker for project %s to %s failed: %v (operator may need to re-write manually)\n",
				project, oldID, werr)
		}
	}
}
