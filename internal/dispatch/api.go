package dispatch

import (
	"errors"
	"io"
	"os"
)

// AcquireCoordPromptInboxOptions captures the inputs the CLI / loop.py
// migration needs to drop a coord_prompt_inbox claim.
//
// Owner is the slug/owner string stored in the journal so observability
// later can map dispatch IDs back to tasks; HostID + TmuxSocket
// discriminate same-host cases in PR2 (the fields are recorded now so
// PR1 journals are forward-compatible).
type AcquireCoordPromptInboxOptions struct {
	DispatchID DispatchID
	Owner      string
	Kind       string // dispatch kind (worker | reviewer | finisher)
	HostID     string
	TmuxSocket string
	Preserve   bool
	Content    io.Reader
}

// AcquireResult is the outcome of an AcquireCoordPromptInbox call.
//
// Outcome maps directly to the CLI's stable outcome enum (acquired |
// already_acquired). Path is the on-disk file the caller should hand
// to format_dispatch_instruction / SessionStart.
type AcquireResult struct {
	Outcome string
	Path    string
}

// AcquireCoordPromptInbox is the high-level API the CLI uses. It:
//
//  1. Loads (or creates) the journal for the dispatch.
//  2. If a live coord_prompt_inbox claim already exists, returns
//     `already_acquired` with the existing path (idempotent retry).
//  3. Otherwise runs the controller AcquireAndDeliver and returns
//     `acquired`.
//
// The journal kind is "worker" if Opts.Kind is empty; this is the
// PR1-default because today's only call sites are loop.py worker /
// reviewer / finisher dispatches and the CLI carries the kind through.
func AcquireCoordPromptInbox(opts AcquireCoordPromptInboxOptions) (AcquireResult, error) {
	if err := validateAcquireOptions(opts); err != nil {
		return AcquireResult{}, err
	}
	// dispatch-durability (fleet#184): acquire is a create/load → mutate
	// → SaveJournal RMW (it sets ExecPending on a fresh journal and the
	// controller flips the claim allocating → live), so it MUST hold the
	// per-id flock like every other journal writer. Otherwise an acquire
	// (or an idempotent re-acquire) racing a mark-launch-attempted /
	// reset-for-relaunch on the same id in a separate `fleet` process
	// could clobber the flock-guarded write via WriteAtomic's
	// unconditional os.Rename. The stdin Content reader is consumed
	// inside the controller; reading it under the lock is bounded (a
	// few-KB prompt), matching the reset-for-relaunch "stdin is already a
	// short prompt" assumption.
	var res AcquireResult
	lockErr := withJournalLock(opts.DispatchID, func() error {
		r, e := acquireCoordPromptInboxLocked(opts)
		res = r
		return e
	})
	if lockErr != nil {
		return AcquireResult{}, lockErr
	}
	return res, nil
}

// acquireCoordPromptInboxLocked is the body of AcquireCoordPromptInbox,
// run while the per-id flock is held by the withJournalLock wrapper.
func acquireCoordPromptInboxLocked(opts AcquireCoordPromptInboxOptions) (AcquireResult, error) {
	path, err := CoordPromptInboxPath(opts.DispatchID)
	if err != nil {
		return AcquireResult{}, err
	}

	j, err := LoadJournal(opts.DispatchID)
	if err != nil {
		if errors.Is(err, ErrJournalNotFound) {
			j = newJournal(
				opts.DispatchID,
				dispatchKindOrDefault(opts.Kind),
				opts.Owner,
				opts.HostID,
				opts.TmuxSocket,
				nowFunc(),
			)
			// dispatch-durability (fleet#184): acquire leaves the entry
			// at ExecPending — it is the COORD that durably flips
			// pending → launch_attempted (via `fleet claims
			// mark-launch-attempted`) IMMEDIATELY before invoking the
			// host Agent. Setting ExecInFlight here (pre-#184) was
			// premature: it implied a launch that had not happened, so
			// replay could not distinguish "recorded but never launched"
			// (safe to re-emit) from "launched". newJournal already
			// defaults ExecState=ExecPending; the explicit set documents
			// the contract.
			j.ExecState = ExecPending
			if err := SaveJournal(j); err != nil {
				return AcquireResult{}, err
			}
		} else {
			return AcquireResult{}, err
		}
	}

	controller := NewCoordPromptInboxController()

	// Idempotency probe: a live claim short-circuits to already_acquired.
	status, existing, err := controller.Inspect(j, KindCoordPromptInbox)
	if err != nil {
		return AcquireResult{}, err
	}
	if status == DeliveryPresent && existing != nil {
		return AcquireResult{Outcome: OutcomeAlreadyAcquired, Path: existing.Path}, nil
	}

	claim := DeliveryClaim{
		Kind:       KindCoordPromptInbox,
		ID:         opts.DispatchID.String(),
		Path:       path,
		OwnerID:    opts.DispatchID,
		HostID:     opts.HostID,
		TmuxSocket: opts.TmuxSocket,
		Preserve:   opts.Preserve,
	}
	if err := controller.AcquireAndDeliver(j, claim, opts.Content); err != nil {
		if errors.Is(err, ErrAlreadyAcquired) {
			return AcquireResult{Outcome: OutcomeAlreadyAcquired, Path: path}, nil
		}
		return AcquireResult{}, err
	}
	return AcquireResult{Outcome: OutcomeAcquired, Path: path}, nil
}

// ReleaseCoordPromptInboxOptions is Release's input.
type ReleaseCoordPromptInboxOptions struct {
	DispatchID DispatchID
	HostID     string
	Preserve   bool
}

// ReleaseResult mirrors AcquireResult for symmetry.
type ReleaseResult struct {
	Outcome string
	Path    string
}

// ReleaseCoordPromptInbox is the high-level release. Outcomes:
//
//	released         — claim was live; file unlinked (or archived) and
//	                   journal flipped to released.
//	already_released — no claim (or already released); idempotent
//	                   success.
//	not_owned        — claim's OwnerID != caller dispatch_id.
//
// On released, the journal's recl_state is recomputed: when all claims
// are released AND exec_state is terminal, recl_state flips to complete.
// PR1's CLI sets ExecState=done before calling release; pure release
// without terminal still leaves recl_state in pending so the sweeper
// (PR4) sees the state.
func ReleaseCoordPromptInbox(opts ReleaseCoordPromptInboxOptions) (ReleaseResult, error) {
	if opts.DispatchID == "" {
		return ReleaseResult{}, errors.New("dispatch_id required")
	}
	// dispatch-durability (fleet#184): release is a read → mutate
	// exec_state → SaveJournal RMW, so it MUST hold the per-id flock —
	// exactly like mark-launch-attempted / reserve-replay / reset-for-
	// relaunch. Without it, a release running in one `fleet` process
	// (loop.py reaper / residual-crash repair, during a tick) can
	// interleave with a mark-launch-attempted running in another `fleet`
	// process (the coord agent, between tick turns, across the agent-turn
	// boundary that coordinator.lock does NOT serialize) and clobber the
	// flock-guarded launch flip via the unconditional os.Rename in
	// WriteAtomic — the rev4 lost-update the flock exists to close. Load
	// the journal INSIDE the lock so we mutate fresh state.
	var res ReleaseResult
	lockErr := withJournalLock(opts.DispatchID, func() error {
		r, e := releaseCoordPromptInboxLocked(opts)
		res = r
		return e
	})
	if lockErr != nil {
		if errors.Is(lockErr, ErrJournalContention) {
			// Surface contention to the caller as an error; the Python
			// release helper logs it and the next tick / sweeper retries.
			return ReleaseResult{}, lockErr
		}
		return ReleaseResult{}, lockErr
	}
	return res, nil
}

// releaseCoordPromptInboxLocked is the body of ReleaseCoordPromptInbox,
// run while the per-id flock is held by the withJournalLock wrapper.
// The internal controller SaveJournal calls execute under the held
// flock (SaveJournal does not re-acquire it), so the whole read →
// mutate → write sequence is one critical section.
func releaseCoordPromptInboxLocked(opts ReleaseCoordPromptInboxOptions) (ReleaseResult, error) {
	j, err := LoadJournal(opts.DispatchID)
	if err != nil {
		if errors.Is(err, ErrJournalNotFound) {
			// No journal = nothing to release.
			return ReleaseResult{Outcome: OutcomeAlreadyReleased}, nil
		}
		return ReleaseResult{}, err
	}
	// Mark exec_state terminal on release if not already — the PR1
	// migration calls release at the dispatch's terminal transition
	// (loop.py reaper / supervisor).
	//
	// dispatch-durability (fleet#184): the pre-#184 code force-flipped
	// ANY non-terminal state to ExecDone. The launch states need care:
	//   - ExecBlocked / ExecFailed are already terminal (IsTerminal
	//     covers them) — never downgrade.
	//   - ExecAcked (live worker finished) and ExecPending / ExecInFlight
	//     (legacy) → ExecDone, the normal completion.
	//   - ExecLaunchAttempted → ExecDone.
	//
	// Codex iter-5 [P2]: an un-acked ExecLaunchAttempted at RELEASE time
	// does NOT mean a failed launch. register_subagent is best-effort
	// (skipped on lock contention) while the worker runs to completion,
	// and release is driven by the worker's OWN terminal transition
	// (loop.py reaper / supervisor on phase=done). So a missing ack is a
	// missing BREADCRUMB, not a failure — recording ExecFailed would
	// corrupt the lifecycle of a worker that completed fine. The phantom
	// "launched-but-never-ran" case is handled separately by the
	// residual-crash repair (loop.py), which ESCALATES off-channel and
	// deliberately does NOT call release (a live-but-unregistered worker
	// is indistinguishable), so a launch_attempted journal only reaches
	// THIS release via a real worker's terminal signal → ExecDone.
	if !j.ExecState.IsTerminal() {
		j.ExecState = ExecDone
	}
	controller := NewCoordPromptInboxController()
	status, existing, err := controller.Inspect(j, KindCoordPromptInbox)
	if err != nil {
		return ReleaseResult{}, err
	}
	// Codex iter-3 [P2] + iter-4 [P1]: Inspect returns DeliveryAbsent
	// for any of (a) no journal claim, (b) claim already released,
	// (c) claim live + file missing, (d) claim in transient state
	// (ClaimReleasing — crashed mid-teardown). Cases (c) and (d) are
	// recoverable through controller.Release:
	//
	//   - ClaimLive    + file gone     → controller.Release tolerates
	//                                     ENOENT on the file unlink
	//                                     (step 4) and still flips
	//                                     the journal to released.
	//   - ClaimReleasing               → controller.Release re-runs
	//                                     steps 4 + 5; the journal
	//                                     advances from releasing →
	//                                     released. Without this, a
	//                                     prior teardown failure
	//                                     (transient unlink/rename
	//                                     error) becomes permanent
	//                                     leakage on the journal.
	//   - ClaimAllocating               → similarly transient; not in
	//                                     the canonical release path
	//                                     (acquire failed mid-write),
	//                                     but routing through Release
	//                                     is correct: it'll find the
	//                                     allocating claim and the
	//                                     teardown completes the
	//                                     reclamation. controller.
	//                                     Release returns
	//                                     ErrAlreadyReleased only on
	//                                     ClaimReleased, so the
	//                                     allocating + releasing
	//                                     states fall through.
	//
	// Only (a) "no claim at all" and (b) "claim explicitly released"
	// are truly nothing-to-do and short-circuit to already_released.
	needsRelease := false
	if existing != nil && status == DeliveryAbsent {
		if idx := j.findClaim(KindCoordPromptInbox); idx >= 0 {
			st := j.Claims[idx].State
			if st != ClaimReleased {
				// Any non-released state (live + missing file,
				// releasing, allocating) is recoverable through
				// controller.Release. Falling through advances the
				// journal even when the resource is already gone.
				needsRelease = true
			}
		}
	}
	if (status == DeliveryAbsent || existing == nil) && !needsRelease {
		// Recompute recl_state: if no live delivery claims and journal
		// is terminal, mark complete.
		if existing != nil {
			// claim recorded but released — flip recl_state when all
			// claims released.
			j.ReclState = recomputeReclState(j)
			if serr := SaveJournal(j); serr != nil {
				return ReleaseResult{}, serr
			}
		}
		return ReleaseResult{Outcome: OutcomeAlreadyReleased}, nil
	}
	caller := DeliveryClaim{
		Kind:    KindCoordPromptInbox,
		OwnerID: opts.DispatchID,
		HostID:  opts.HostID,
	}
	// preserve override: CLI passes from --preserve; otherwise use the
	// claim's recorded Preserve flag (set at acquire time).
	caller.Preserve = opts.Preserve || existing.Preserve
	if err := controller.Release(j, caller); err != nil {
		if errors.Is(err, ErrAlreadyReleased) {
			return ReleaseResult{Outcome: OutcomeAlreadyReleased, Path: existing.Path}, nil
		}
		if errors.Is(err, ErrNotOwned) {
			return ReleaseResult{Outcome: OutcomeNotOwned, Path: existing.Path}, nil
		}
		return ReleaseResult{}, err
	}
	return ReleaseResult{Outcome: OutcomeReleased, Path: existing.Path}, nil
}

// InspectCoordPromptInboxOptions targets a dispatch_id.
type InspectCoordPromptInboxOptions struct {
	DispatchID DispatchID
}

// InspectResult is the inspect outcome.
type InspectResult struct {
	Outcome string
	Path    string
	Status  DeliveryStatus
}

// InspectCoordPromptInbox returns the claim's status for tooling /
// debugging. Outcomes:
//
//	acquired   — claim present + live + file on disk.
//	absent     — no claim, no journal, or claim released.
//	error      — surfaced as a Go error to the caller.
func InspectCoordPromptInbox(opts InspectCoordPromptInboxOptions) (InspectResult, error) {
	if opts.DispatchID == "" {
		return InspectResult{}, errors.New("dispatch_id required")
	}
	j, err := LoadJournal(opts.DispatchID)
	if err != nil {
		if errors.Is(err, ErrJournalNotFound) {
			return InspectResult{Outcome: OutcomeAbsent, Status: DeliveryAbsent}, nil
		}
		return InspectResult{}, err
	}
	controller := NewCoordPromptInboxController()
	status, claim, err := controller.Inspect(j, KindCoordPromptInbox)
	if err != nil {
		return InspectResult{}, err
	}
	res := InspectResult{Status: status}
	if claim != nil {
		res.Path = claim.Path
	}
	if status == DeliveryPresent {
		res.Outcome = OutcomeAcquired
	} else {
		res.Outcome = OutcomeAbsent
	}
	return res, nil
}

// recomputeReclState is the post-release helper: when all inline claims
// are released and exec_state is terminal, the journal is recl_complete.
// Partial release leaves recl_state in pending until the sweeper (PR4)
// re-runs.
func recomputeReclState(j *Journal) ReclState {
	allReleased := true
	for _, c := range j.Claims {
		if c.State != ClaimReleased {
			allReleased = false
			break
		}
	}
	if allReleased && j.ExecState.IsTerminal() {
		return ReclComplete
	}
	return j.ReclState
}

// Stable outcome enum values. The CLI (cmd/fleet/claims.go) embeds
// these into JSON responses; tests assert against them. Mirrors the
// values listed in DESIGN-dispatch-lifecycle.md §"Atomicity contract"
// outcome table.
const (
	OutcomeAcquired        = "acquired"
	OutcomeAlreadyAcquired = "already_acquired"
	OutcomeReleased        = "released"
	OutcomeAlreadyReleased = "already_released"
	OutcomeNotOwned        = "not_owned"
	OutcomeAbsent          = "absent"
	OutcomeContested       = "contested" // reserved for PR2 (per-task_slug lock)
	OutcomeError           = "error"
)

// validateAcquireOptions returns a concrete error for malformed input.
// The CLI maps these into the `error` outcome (exit 1) so operators see
// a clear message rather than a stringly-typed JSON blob.
func validateAcquireOptions(opts AcquireCoordPromptInboxOptions) error {
	if opts.DispatchID == "" {
		return errors.New("dispatch_id required")
	}
	if opts.Content == nil {
		return errors.New("content reader required")
	}
	return nil
}

// dispatchKindOrDefault normalizes opts.Kind. "worker" is the default
// because PR1's three call sites are worker / reviewer / finisher
// dispatches; if the caller passed nothing, the worker default is the
// most-common label.
func dispatchKindOrDefault(k string) string {
	if k == "" {
		return "worker"
	}
	return k
}

// HostID returns the OS hostname, used as the default HostID for
// claims when callers don't override.
func HostID() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "unknown"
	}
	return h
}

// _ enforces that the controller satisfies the DeliveryController
// interface at compile time — guards against accidental signature drift
// when PR2 adds Rewrite().
var _ DeliveryController = (*coordPromptInboxController)(nil)
