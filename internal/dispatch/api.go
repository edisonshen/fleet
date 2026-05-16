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
			j.ExecState = ExecInFlight
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
	// (loop.py reaper / supervisor). PR1 keeps the flip conservative:
	// only mark done if currently in_flight/pending. PR3 layers richer
	// terminal-cause tracking on top.
	if !j.ExecState.IsTerminal() {
		j.ExecState = ExecDone
	}
	controller := NewCoordPromptInboxController()
	status, existing, err := controller.Inspect(j, KindCoordPromptInbox)
	if err != nil {
		return ReleaseResult{}, err
	}
	if status == DeliveryAbsent || existing == nil {
		// Recompute recl_state: if no live delivery claims and journal
		// is terminal, mark complete.
		if existing != nil {
			// claim recorded but released or absent — flip recl_state
			// when all claims released.
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
