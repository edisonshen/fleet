package dispatch

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/edisonshen/fleet/internal/state"
)

// durability.go — the launch-attempt state machine on top of the
// dispatch journal (DESIGN-dispatch-durability.md rev6, fleet#184).
//
// Every operation here routes through withJournalLock (store.go) so the
// read → predicate → mutate → WriteAtomic sequence is a genuine critical
// section. The generation field is the launch token: a DISPATCH block
// carries the generation it was emitted for, and MarkLaunchAttempted
// flips pending → launch_attempted ONLY if the on-disk generation still
// equals the token (stale-block guard).
//
// Exactly-once boundary (the crux): the coord launches the host Agent
// BEFORE register_subagent, and registration is best-effort. So an acked
// worker frequently has no ack. Replaying on "no ack" would re-emit the
// block → a SECOND live Agent on the same id/inbox/worktree. The fix is
// to durably record the launch ATTEMPT before the Agent is invoked, and
// replay ONLY ExecPending entries (coord never flipped them ⇒ never
// launched).

// LaunchAttemptOutcome is the tri-state result of MarkLaunchAttempted.
// The coord MUST branch on all three — collapsing contention into "skip"
// is the lost-launch bug.
type LaunchAttemptOutcome string

const (
	// LaunchAttemptOK — was ExecPending AND on-disk gen == token; the
	// flip to ExecLaunchAttempted landed. Coord proceeds to launch.
	LaunchAttemptOK LaunchAttemptOutcome = "ok"
	// LaunchAttemptPredicateFail — state != ExecPending (already
	// attempted) OR generation mismatch (stale block). Another tick/path
	// owns this launch. Coord SKIPS, does NOT launch.
	LaunchAttemptPredicateFail LaunchAttemptOutcome = "predicate_fail"
	// LaunchAttemptContention — could not take the flock within the
	// deadline. TRANSIENT. Coord retries the SAME block next tick; NEVER
	// treats as skip.
	LaunchAttemptContention LaunchAttemptOutcome = "contention"
)

// MarkLaunchAttempted flips ExecPending → ExecLaunchAttempted under the
// per-journal flock, iff the on-disk generation still equals gen. The
// coord runs this IMMEDIATELY before invoking the host Agent tool.
//
// Tri-state (see LaunchAttemptOutcome). ErrJournalNotFound (no journal)
// resolves to predicate-fail: there is nothing to launch, and re-emitting
// would be wrong.
func MarkLaunchAttempted(id DispatchID, gen int) (LaunchAttemptOutcome, error) {
	outcome := LaunchAttemptPredicateFail
	err := withJournalLock(id, func() error {
		j, lerr := loadJournalLocked(id)
		if lerr != nil {
			if errors.Is(lerr, ErrJournalNotFound) {
				// No journal — nothing to launch. predicate-fail.
				return nil
			}
			return lerr
		}
		// Predicate: must be pending AND generation must match the
		// launch token. A stale re-emitted block (older lifecycle)
		// carries an old gen → reject; a block for an entry already
		// flipped → reject.
		if j.ExecState != ExecPending || j.Generation != gen {
			return nil // predicate-fail (already set above)
		}
		j.ExecState = ExecLaunchAttempted
		j.LaunchAttemptedAt = nowFunc().UTC()
		if werr := writeJournalLocked(j); werr != nil {
			return werr
		}
		outcome = LaunchAttemptOK
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrJournalContention) {
			return LaunchAttemptContention, nil
		}
		return "", err
	}
	return outcome, nil
}

// MarkAcked flips a launched dispatch to ExecAcked under the flock.
// Called by register_subagent (best-effort) when the host Agent's
// subagent_id is captured. Idempotent and tolerant: a journal that is
// already terminal/acked is left as-is (returns nil) so a late ack can't
// resurrect a released dispatch.
//
// Only ExecLaunchAttempted → ExecAcked is a real transition. ExecPending
// → acked is rejected (would mean acking a launch the coord never
// flipped — a protocol violation; we leave pending so replay can still
// recover it). ErrJournalNotFound is a no-op success (best-effort).
func MarkAcked(id DispatchID) error {
	return withJournalLock(id, func() error {
		j, lerr := loadJournalLocked(id)
		if lerr != nil {
			if errors.Is(lerr, ErrJournalNotFound) {
				return nil // best-effort: nothing to ack
			}
			return lerr
		}
		switch j.ExecState {
		case ExecLaunchAttempted:
			j.ExecState = ExecAcked
			return writeJournalLocked(j)
		default:
			// pending (never launched — don't mask the phantom),
			// already acked, or terminal: leave alone.
			return nil
		}
	})
}

// ReplayReservation is the result of ReserveReplay.
type ReplayReservation struct {
	// Outcome is one of: "reserved" (block may be emitted; Generation is
	// the launch token to stamp on it), "capped" (ReplayEmitAttempts >=
	// cap; journal flipped to ExecBlocked), "not_pending" (state !=
	// ExecPending; nothing to replay), "absent" (no journal),
	// "contention" (flock deadline — caller retries next tick).
	Outcome string
	// Generation is the current launch token (valid when
	// Outcome=="reserved"); the emitted DISPATCH block MUST carry it so
	// MarkLaunchAttempted validates against the same lifecycle.
	Generation int
}

// Replay reservation outcomes.
const (
	ReplayReserved   = "reserved"
	ReplayCapped     = "capped"
	ReplayNotPending = "not_pending"
	ReplayAbsent     = "absent"
	ReplayContention = "contention"
)

// ReserveReplay is the tick-entry replay primitive. Under the flock, for
// an ExecPending entry it increments ReplayEmitAttempts (the RESERVED
// emission count) BEFORE the block reaches tick output — so a broken-pipe
// print still advances the cap, no infinite re-emit across coord
// restarts. The cap is total-per-dispatch.
//
//   - state != ExecPending          → ReplayNotPending (no replay).
//   - ReplayEmitAttempts >= cap      → flip ExecBlocked(reason) → ReplayCapped.
//   - otherwise                      → increment, stamp last_replay_at,
//     return ReplayReserved + current gen.
//
// reserve happens EXCLUSIVELY from ExecPending; the MarkLaunchAttempted
// CAS in protocol step 2 prevents two ticks racing the same entry, and
// the generation token makes a reset-for-relaunch'd lifecycle reject a
// stale reserved block.
func ReserveReplay(id DispatchID, cap int) (ReplayReservation, error) {
	res := ReplayReservation{Outcome: ReplayNotPending}
	err := withJournalLock(id, func() error {
		j, lerr := loadJournalLocked(id)
		if lerr != nil {
			if errors.Is(lerr, ErrJournalNotFound) {
				res.Outcome = ReplayAbsent
				return nil
			}
			return lerr
		}
		if j.ExecState != ExecPending {
			res.Outcome = ReplayNotPending
			res.Generation = j.Generation
			return nil
		}
		if j.ReplayEmitAttempts >= cap {
			// Durable BLOCKED — replay can't be delivered. Off-channel
			// escalation (TUI blocked_reason + coord-state note) is the
			// caller's job; here we make the journal terminal so the
			// next tick stops re-emitting.
			j.ExecState = ExecBlocked
			j.BlockedReason = "dispatch_undelivered"
			if werr := writeJournalLocked(j); werr != nil {
				return werr
			}
			res.Outcome = ReplayCapped
			res.Generation = j.Generation
			return nil
		}
		j.ReplayEmitAttempts++
		j.LastReplayAt = nowFunc().UTC()
		if werr := writeJournalLocked(j); werr != nil {
			return werr
		}
		res.Outcome = ReplayReserved
		res.Generation = j.Generation
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrJournalContention) {
			return ReplayReservation{Outcome: ReplayContention}, nil
		}
		return ReplayReservation{}, err
	}
	return res, nil
}

// ResetForRelaunchResult is the result of ResetForRelaunch.
type ResetForRelaunchResult struct {
	// Outcome: "reset" (inbox rewritten + entry reset to fresh
	// ExecPending; Generation is the NEW launch token), "absent" (no
	// journal — nothing to reset; the caller should acquire fresh),
	// "contention" (flock deadline — retry).
	Outcome    string
	Generation int
	Path       string
}

// Reset-for-relaunch outcomes.
const (
	ResetDone       = "reset"
	ResetAbsent     = "absent"
	ResetContention = "contention"
)

// ResetForRelaunch atomically re-arms an EXISTING dispatch for relaunch
// (handoff_resume.py's case: re-emit a block for an agent_id whose
// journal entry is ExecAcked/terminal from the ORIGINAL dispatch). A
// blind MarkLaunchAttempted would predicate-fail (not pending) and the
// resume would never launch.
//
// CRITICAL ordering (rev6): the new prompt is read FULLY from `content`
// into memory FIRST, THEN the flock is taken — never hold the journal
// lock across a slow/stalled stdin pipe. Under one critical section it
// does only two bounded writes:
//  1. rewrite the inbox prompt file (the coord_prompt_inbox resource),
//  2. reset the entry to a fresh ExecPending with a BUMPED generation
//     and ReplayEmitAttempts=0,
//
// so no tick can observe the reset ExecPending before the inbox is in
// place (the reset commit is the LAST write under the lock).
//
// ErrJournalNotFound → ResetAbsent (caller acquires fresh instead).
func ResetForRelaunch(id DispatchID, content io.Reader) (ResetForRelaunchResult, error) {
	// Read the full prompt BEFORE taking the lock (rev6 stdin-before-lock).
	body, rerr := io.ReadAll(content)
	if rerr != nil {
		return ResetForRelaunchResult{}, fmt.Errorf("read relaunch prompt: %w", rerr)
	}
	inboxPath, perr := CoordPromptInboxPath(id)
	if perr != nil {
		return ResetForRelaunchResult{}, perr
	}

	res := ResetForRelaunchResult{Outcome: ResetAbsent, Path: inboxPath}
	err := withJournalLock(id, func() error {
		j, lerr := loadJournalLocked(id)
		if lerr != nil {
			if errors.Is(lerr, ErrJournalNotFound) {
				res.Outcome = ResetAbsent
				return nil
			}
			return lerr
		}
		// Write 1: rewrite the inbox prompt (atomic tmp+rename+fsync).
		if werr := state.WriteAtomic(inboxPath, body); werr != nil {
			return fmt.Errorf("rewrite relaunch inbox %s: %w", inboxPath, werr)
		}
		// Write 2 (LAST under the lock): reset to a fresh ExecPending
		// lifecycle. Bump the generation so any stale block carrying the
		// old gen predicate-fails, and zero the replay cap so the fresh
		// lifecycle gets its full budget.
		j.ExecState = ExecPending
		j.Generation++
		j.ReplayEmitAttempts = 0
		j.LaunchAttemptedAt = time.Time{}
		j.BlockedReason = ""
		j.LastReplayError = ""
		if werr := writeJournalLocked(j); werr != nil {
			return werr
		}
		res.Outcome = ResetDone
		res.Generation = j.Generation
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrJournalContention) {
			return ResetForRelaunchResult{Outcome: ResetContention, Path: inboxPath}, nil
		}
		return ResetForRelaunchResult{}, err
	}
	return res, nil
}
