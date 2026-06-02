// Package main — fleet claims hidden CLI subtree.
//
// This file implements the operator-invisible `fleet claims` namespace
// that backs the Delivery controller in internal/dispatch. The Python
// coord skill (skills/coordinator/loop.py) shells out to these
// subcommands instead of writing inbox files directly — the
// dispatch-lifecycle primitive's load-bearing contract is that EVERY
// fleet-created resource is owned by a journal, and only the Go side
// can mutate the journal atomically.
//
// PR1 scope (DESIGN-dispatch-lifecycle.md §"Vertical-slice sequencing"):
//   - `acquire-prompt`  — coord_prompt_inbox AcquireAndDeliver
//   - `release`         — Delivery Release (kind=coord_prompt_inbox)
//   - `inspect`         — Delivery Inspect
//
// All subcommands:
//   - Hidden from `fleet --help` (cobra.Command.Hidden = true).
//   - Read prompt content from stdin (no --content-file flag — avoids
//     path leaks, matches DESIGN §"Acquire is Go-side; Python shells
//     out via fleet claims").
//   - Emit a JSON envelope on stdout: `{"outcome": "...", ...}`.
//   - Exit with a STABLE code per outcome (plan-eng A1):
//     acquired         → 0
//     already_acquired → 0
//     released         → 0
//     already_released → 0
//     not_owned        → 10
//     absent           → 11
//     contested        → 12 (reserved for PR2 per-task_slug lock)
//     error            → 1
//
// Golden-file contract tests in cmd/fleet/testdata/claims/*.json pin
// the JSON schema + outcome enums; the CLI is the public boundary the
// Python skill depends on.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/edisonshen/fleet/internal/dispatch"
	"github.com/edisonshen/fleet/internal/state"
)

// claimsResponse is the JSON envelope every claims subcommand emits.
// Keep the field set stable — every key here is observed by external
// callers (skills/coordinator/loop.py + future PR2 subagent code).
//
// Required: outcome.
// Conditional: dispatch_id, kind, path, error.
type claimsResponse struct {
	Outcome    string `json:"outcome"`
	DispatchID string `json:"dispatch_id,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Path       string `json:"path,omitempty"`
	Error      string `json:"error,omitempty"`
	// Generation is the journal's launch token. Emitted by the
	// dispatch-durability (#184) subcommands so loop.py can stamp the
	// re-emitted DISPATCH block with the gen the journal currently holds
	// (reserve-replay / reset-for-relaunch return the NEW gen). A pointer
	// so omitempty drops it from the legacy acquire/release/inspect
	// envelopes that don't carry a generation.
	Generation *int `json:"generation,omitempty"`
}

// dispatch-durability (#184) subcommand outcome strings. The Python
// coord (loop.py / register_subagent / handoff_resume) branches on the
// `outcome` JSON field; exit codes are a secondary signal (the tri-state
// for mark-launch-attempted gets distinct non-zero codes so a shell
// caller can branch without parsing JSON).
const (
	outcomeLaunchOK         = "ok"             // pending → launch_attempted flipped
	outcomePredicateFail    = "predicate_fail" // not pending / gen mismatch → skip
	outcomeContention       = "contention"     // flock deadline → TRANSIENT, retry
	outcomeAcked            = "acked"          // launch_attempted → acked
	outcomeReplayReserved   = "reserved"       // replay emission reserved
	outcomeReplayCapped     = "capped"         // cap hit → journal ExecBlocked
	outcomeReplayNotPending = "not_pending"    // not ExecPending → no replay
	outcomeReset            = "reset"          // entry reset to fresh ExecPending
)

// claimsExitCode maps outcome strings to the stable exit code table.
// Any outcome not in this table exits 1 (caller error).
func claimsExitCode(outcome string) int {
	switch outcome {
	case dispatch.OutcomeAcquired,
		dispatch.OutcomeAlreadyAcquired,
		dispatch.OutcomeReleased,
		dispatch.OutcomeAlreadyReleased,
		// #184 success-shaped outcomes (no stderr noise; loop.py reads
		// stdout JSON). "capped" and "reset" are durable successes —
		// the journal mutation landed — so they exit 0.
		outcomeLaunchOK,
		outcomeAcked,
		outcomeReplayReserved,
		outcomeReplayCapped,
		outcomeReset:
		return 0
	case dispatch.OutcomeNotOwned:
		return 10
	case dispatch.OutcomeAbsent:
		return 11
	case dispatch.OutcomeContested:
		return 12
	// #184 non-error, non-success outcomes: distinct codes so a shell
	// caller can branch on $? without JSON parsing. The coord protocol
	// (SKILL.md) keys on the JSON `outcome` field.
	case outcomePredicateFail:
		return 20
	case outcomeContention:
		return 21
	case outcomeReplayNotPending:
		return 22
	default:
		return 1
	}
}

func newClaimsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "claims",
		Short:  "Internal dispatch-lifecycle claim operations (hidden)",
		Long:   "Internal helpers for the dispatch-lifecycle primitive; called by skills/coordinator/loop.py. Hidden from --help by design.",
		Hidden: true,
	}
	cmd.AddCommand(newClaimsAcquirePromptCmd())
	cmd.AddCommand(newClaimsReleaseCmd())
	cmd.AddCommand(newClaimsInspectCmd())
	// dispatch-durability (#184) launch-state subcommands.
	cmd.AddCommand(newClaimsMarkLaunchAttemptedCmd())
	cmd.AddCommand(newClaimsMarkAckedCmd())
	cmd.AddCommand(newClaimsReserveReplayCmd())
	cmd.AddCommand(newClaimsResetForRelaunchCmd())
	return cmd
}

func newClaimsAcquirePromptCmd() *cobra.Command {
	var (
		owner       string
		hostID      string
		tmuxSocket  string
		dispatchKnd string
		preserve    bool
	)
	cmd := &cobra.Command{
		Use:    "acquire-prompt <dispatch-id>",
		Short:  "Acquire a coord_prompt_inbox claim and write the prompt file (stdin → ~/.fleet/inbox/<id>.md)",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return runClaimsAcquirePrompt(
				c.OutOrStdout(), c.ErrOrStderr(),
				c.InOrStdin(),
				args[0], owner, hostID, tmuxSocket, dispatchKnd, preserve,
			)
		},
	}
	cmd.Flags().StringVar(&owner, "owner", "",
		"task slug / owner string recorded in the journal (e.g., 'project/fleet/slug/feat-foo')")
	cmd.Flags().StringVar(&hostID, "host-id", "",
		"hostname recorded on the claim (defaults to OS hostname)")
	cmd.Flags().StringVar(&tmuxSocket, "tmux-socket", "",
		"tmux socket path for the dispatch (carried for forward-compat; PR1 ignores)")
	cmd.Flags().StringVar(&dispatchKnd, "dispatch-kind", "worker",
		"dispatch role (worker | reviewer | finisher)")
	cmd.Flags().BoolVar(&preserve, "preserve", false,
		"on release, archive the inbox file instead of unlinking (default false)")
	return cmd
}

// runClaimsAcquirePrompt is the testable entry point. Reads content
// from stdin, calls dispatch.AcquireCoordPromptInbox, emits the JSON
// envelope, and returns an error whose presence triggers exit code 1.
// For non-error outcomes the caller (main.go) consults claimsExitCode
// to set the right exit status.
func runClaimsAcquirePrompt(stdout, stderr io.Writer, stdin io.Reader,
	dispatchID, owner, hostID, tmuxSocket, dispatchKnd string, preserve bool,
) error {
	if _, err := state.Bootstrap(); err != nil {
		return emitErrorAndFail(stdout, dispatchID, dispatch.KindCoordPromptInbox,
			fmt.Errorf("bootstrap ~/.fleet: %w", err))
	}
	id, err := dispatch.NewDispatchID(dispatchID)
	if err != nil {
		return emitErrorAndFail(stdout, dispatchID, dispatch.KindCoordPromptInbox, err)
	}
	if hostID == "" {
		hostID = dispatch.HostID()
	}
	res, err := dispatch.AcquireCoordPromptInbox(dispatch.AcquireCoordPromptInboxOptions{
		DispatchID: id,
		Owner:      owner,
		Kind:       dispatchKnd,
		HostID:     hostID,
		TmuxSocket: tmuxSocket,
		Preserve:   preserve,
		Content:    stdin,
	})
	if err != nil {
		return emitErrorAndFail(stdout, dispatchID, dispatch.KindCoordPromptInbox, err)
	}
	return emitOutcome(stdout, claimsResponse{
		Outcome:    res.Outcome,
		DispatchID: id.String(),
		Kind:       dispatch.KindCoordPromptInbox,
		Path:       res.Path,
	})
}

func newClaimsReleaseCmd() *cobra.Command {
	var (
		kind     string
		hostID   string
		preserve bool
	)
	cmd := &cobra.Command{
		Use:    "release <dispatch-id>",
		Short:  "Release a claim for the given dispatch_id and kind",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return runClaimsRelease(
				c.OutOrStdout(), c.ErrOrStderr(),
				args[0], kind, hostID, preserve,
			)
		},
	}
	cmd.Flags().StringVar(&kind, "kind", dispatch.KindCoordPromptInbox,
		"claim kind to release (PR1: coord_prompt_inbox; PR2 adds the rest)")
	cmd.Flags().StringVar(&hostID, "host-id", "",
		"hostname performing the release (defaults to OS hostname; used for cross-host refusal)")
	cmd.Flags().BoolVar(&preserve, "preserve", false,
		"archive instead of unlink (overrides the value recorded at acquire)")
	return cmd
}

// runClaimsRelease executes a Release for the given kind. PR1 only
// implements coord_prompt_inbox; any other kind exits with `error`.
func runClaimsRelease(stdout, stderr io.Writer,
	dispatchID, kind, hostID string, preserve bool,
) error {
	if _, err := state.Bootstrap(); err != nil {
		return emitErrorAndFail(stdout, dispatchID, kind,
			fmt.Errorf("bootstrap ~/.fleet: %w", err))
	}
	id, err := dispatch.NewDispatchID(dispatchID)
	if err != nil {
		return emitErrorAndFail(stdout, dispatchID, kind, err)
	}
	if kind != dispatch.KindCoordPromptInbox {
		return emitErrorAndFail(stdout, dispatchID, kind,
			fmt.Errorf("kind %q not implemented in PR1 (only coord_prompt_inbox)", kind))
	}
	if hostID == "" {
		hostID = dispatch.HostID()
	}
	res, err := dispatch.ReleaseCoordPromptInbox(dispatch.ReleaseCoordPromptInboxOptions{
		DispatchID: id,
		HostID:     hostID,
		Preserve:   preserve,
	})
	if err != nil {
		return emitErrorAndFail(stdout, dispatchID, kind, err)
	}
	return emitOutcome(stdout, claimsResponse{
		Outcome:    res.Outcome,
		DispatchID: id.String(),
		Kind:       kind,
		Path:       res.Path,
	})
}

func newClaimsInspectCmd() *cobra.Command {
	var kind string
	cmd := &cobra.Command{
		Use:    "inspect <dispatch-id>",
		Short:  "Inspect a claim (returns outcome=acquired or absent)",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return runClaimsInspect(c.OutOrStdout(), c.ErrOrStderr(), args[0], kind)
		},
	}
	cmd.Flags().StringVar(&kind, "kind", dispatch.KindCoordPromptInbox,
		"claim kind to inspect (PR1: coord_prompt_inbox)")
	return cmd
}

// runClaimsInspect emits the inspection outcome as JSON.
func runClaimsInspect(stdout, stderr io.Writer, dispatchID, kind string) error {
	if _, err := state.Bootstrap(); err != nil {
		return emitErrorAndFail(stdout, dispatchID, kind,
			fmt.Errorf("bootstrap ~/.fleet: %w", err))
	}
	id, err := dispatch.NewDispatchID(dispatchID)
	if err != nil {
		return emitErrorAndFail(stdout, dispatchID, kind, err)
	}
	if kind != dispatch.KindCoordPromptInbox {
		return emitErrorAndFail(stdout, dispatchID, kind,
			fmt.Errorf("kind %q not implemented in PR1", kind))
	}
	res, err := dispatch.InspectCoordPromptInbox(dispatch.InspectCoordPromptInboxOptions{
		DispatchID: id,
	})
	if err != nil {
		return emitErrorAndFail(stdout, dispatchID, kind, err)
	}
	return emitOutcome(stdout, claimsResponse{
		Outcome:    res.Outcome,
		DispatchID: id.String(),
		Kind:       kind,
		Path:       res.Path,
	})
}

// emitOutcome writes the JSON envelope to stdout. Returns the
// resp.Outcome-equivalent error sentinel for outcomes that should
// surface as non-zero exit codes (not_owned, absent, contested,
// error). The wrapper main loop sees the sentinel via errors.Is and
// sets exit accordingly.
func emitOutcome(stdout io.Writer, resp claimsResponse) error {
	data, err := json.Marshal(resp)
	if err != nil {
		// Best effort — return a fresh `error` envelope.
		fallback, _ := json.Marshal(claimsResponse{Outcome: dispatch.OutcomeError, Error: err.Error()})
		_, _ = stdout.Write(append(fallback, '\n'))
		return errClaimsError{outcome: dispatch.OutcomeError}
	}
	if _, werr := stdout.Write(append(data, '\n')); werr != nil {
		return werr
	}
	// Translate non-zero-exit outcomes into errors.
	switch resp.Outcome {
	case dispatch.OutcomeAcquired,
		dispatch.OutcomeAlreadyAcquired,
		dispatch.OutcomeReleased,
		dispatch.OutcomeAlreadyReleased:
		return nil
	default:
		return errClaimsError{outcome: resp.Outcome}
	}
}

// emitErrorAndFail writes an `error` envelope to stdout (so callers
// parsing JSON still see the outcome) and returns a sentinel.
func emitErrorAndFail(stdout io.Writer, dispatchID, kind string, err error) error {
	resp := claimsResponse{
		Outcome:    dispatch.OutcomeError,
		DispatchID: dispatchID,
		Kind:       kind,
		Error:      err.Error(),
	}
	data, _ := json.Marshal(resp)
	_, _ = stdout.Write(append(data, '\n'))
	return errClaimsError{outcome: dispatch.OutcomeError}
}

// errClaimsError is a sentinel carrying the outcome so main.go can
// derive the exit code via errors.As.
type errClaimsError struct {
	outcome string
}

func (e errClaimsError) Error() string { return "claims: outcome=" + e.outcome }

// claimsOutcomeFromErr extracts the outcome from a sentinel error.
// Returns "" when err is not an errClaimsError.
func claimsOutcomeFromErr(err error) string {
	var e errClaimsError
	if errors.As(err, &e) {
		return e.outcome
	}
	return ""
}

// ============================================================
// dispatch-durability (#184) launch-state subcommands
// ============================================================

// newClaimsMarkLaunchAttemptedCmd implements `fleet claims
// mark-launch-attempted <id> <gen>` — the tri-state CAS the coord runs
// IMMEDIATELY before invoking the host Agent (SKILL.md "Worker dispatch
// protocol" step 2). Three distinct outcomes the coord MUST branch on:
//
//	ok             (exit 0)  → flip landed; LAUNCH the Agent.
//	predicate_fail (exit 20) → not pending / stale gen; another path owns
//	                           this launch; SKIP, do NOT launch.
//	contention     (exit 21) → flock deadline; TRANSIENT; retry the SAME
//	                           block next tick; NEVER treat as skip.
func newClaimsMarkLaunchAttemptedCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "mark-launch-attempted <dispatch-id> <generation>",
		Short:  "Flip pending → launch_attempted iff gen matches (tri-state CAS)",
		Hidden: true,
		Args:   cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			return runClaimsMarkLaunchAttempted(c.OutOrStdout(), args[0], args[1])
		},
	}
	return cmd
}

func runClaimsMarkLaunchAttempted(stdout io.Writer, dispatchID, genStr string) error {
	if _, err := state.Bootstrap(); err != nil {
		return emitErrorAndFail(stdout, dispatchID, dispatch.KindCoordPromptInbox,
			fmt.Errorf("bootstrap ~/.fleet: %w", err))
	}
	id, err := dispatch.NewDispatchID(dispatchID)
	if err != nil {
		return emitErrorAndFail(stdout, dispatchID, dispatch.KindCoordPromptInbox, err)
	}
	gen, err := strconv.Atoi(genStr)
	if err != nil {
		return emitErrorAndFail(stdout, dispatchID, dispatch.KindCoordPromptInbox,
			fmt.Errorf("invalid generation %q: %w", genStr, err))
	}
	outcome, err := dispatch.MarkLaunchAttempted(id, gen)
	if err != nil {
		return emitErrorAndFail(stdout, dispatchID, dispatch.KindCoordPromptInbox, err)
	}
	var outStr string
	switch outcome {
	case dispatch.LaunchAttemptOK:
		outStr = outcomeLaunchOK
	case dispatch.LaunchAttemptPredicateFail:
		outStr = outcomePredicateFail
	case dispatch.LaunchAttemptContention:
		outStr = outcomeContention
	default:
		return emitErrorAndFail(stdout, dispatchID, dispatch.KindCoordPromptInbox,
			fmt.Errorf("unexpected launch-attempt outcome %q", outcome))
	}
	return emitOutcome(stdout, claimsResponse{
		Outcome:    outStr,
		DispatchID: id.String(),
		Kind:       dispatch.KindCoordPromptInbox,
	})
}

// newClaimsMarkAckedCmd implements `fleet claims mark-acked <id>` —
// register_subagent flips launch_attempted → acked (best-effort).
func newClaimsMarkAckedCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "mark-acked <dispatch-id>",
		Short:  "Flip launch_attempted → acked (best-effort, idempotent)",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return runClaimsMarkAcked(c.OutOrStdout(), args[0])
		},
	}
	return cmd
}

func runClaimsMarkAcked(stdout io.Writer, dispatchID string) error {
	if _, err := state.Bootstrap(); err != nil {
		return emitErrorAndFail(stdout, dispatchID, dispatch.KindCoordPromptInbox,
			fmt.Errorf("bootstrap ~/.fleet: %w", err))
	}
	id, err := dispatch.NewDispatchID(dispatchID)
	if err != nil {
		return emitErrorAndFail(stdout, dispatchID, dispatch.KindCoordPromptInbox, err)
	}
	if err := dispatch.MarkAcked(id); err != nil {
		if errors.Is(err, dispatch.ErrJournalContention) {
			// Best-effort: contention is non-fatal for ack (the chip just
			// stays empty); surface it as a distinct outcome so the coord
			// can log + move on without treating it as an error.
			return emitOutcome(stdout, claimsResponse{
				Outcome:    outcomeContention,
				DispatchID: id.String(),
				Kind:       dispatch.KindCoordPromptInbox,
			})
		}
		return emitErrorAndFail(stdout, dispatchID, dispatch.KindCoordPromptInbox, err)
	}
	return emitOutcome(stdout, claimsResponse{
		Outcome:    outcomeAcked,
		DispatchID: id.String(),
		Kind:       dispatch.KindCoordPromptInbox,
	})
}

// newClaimsReserveReplayCmd implements `fleet claims reserve-replay <id>
// [--cap N]` — the tick-entry replay primitive. Increments the durable
// replay counter under the flock BEFORE the block reaches tick output;
// returns the launch token (generation) to stamp on the re-emitted block.
func newClaimsReserveReplayCmd() *cobra.Command {
	var cap int
	cmd := &cobra.Command{
		Use:    "reserve-replay <dispatch-id>",
		Short:  "Reserve a replay emission (increment cap, return launch gen)",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return runClaimsReserveReplay(c.OutOrStdout(), args[0], cap)
		},
	}
	cmd.Flags().IntVar(&cap, "cap", 5,
		"total-per-dispatch replay cap; at/over cap the journal flips to ExecBlocked")
	return cmd
}

func runClaimsReserveReplay(stdout io.Writer, dispatchID string, cap int) error {
	if _, err := state.Bootstrap(); err != nil {
		return emitErrorAndFail(stdout, dispatchID, dispatch.KindCoordPromptInbox,
			fmt.Errorf("bootstrap ~/.fleet: %w", err))
	}
	id, err := dispatch.NewDispatchID(dispatchID)
	if err != nil {
		return emitErrorAndFail(stdout, dispatchID, dispatch.KindCoordPromptInbox, err)
	}
	if cap < 1 {
		return emitErrorAndFail(stdout, dispatchID, dispatch.KindCoordPromptInbox,
			fmt.Errorf("invalid --cap %d: must be >= 1", cap))
	}
	res, err := dispatch.ReserveReplay(id, cap)
	if err != nil {
		return emitErrorAndFail(stdout, dispatchID, dispatch.KindCoordPromptInbox, err)
	}
	resp := claimsResponse{DispatchID: id.String(), Kind: dispatch.KindCoordPromptInbox}
	switch res.Outcome {
	case dispatch.ReplayReserved:
		resp.Outcome = outcomeReplayReserved
		g := res.Generation
		resp.Generation = &g
	case dispatch.ReplayCapped:
		resp.Outcome = outcomeReplayCapped
	case dispatch.ReplayNotPending:
		resp.Outcome = outcomeReplayNotPending
	case dispatch.ReplayAbsent:
		resp.Outcome = dispatch.OutcomeAbsent
	case dispatch.ReplayContention:
		resp.Outcome = outcomeContention
	default:
		return emitErrorAndFail(stdout, dispatchID, dispatch.KindCoordPromptInbox,
			fmt.Errorf("unexpected reserve-replay outcome %q", res.Outcome))
	}
	return emitOutcome(stdout, resp)
}

// newClaimsResetForRelaunchCmd implements `fleet claims
// reset-for-relaunch <id>` — handoff_resume.py re-arms an existing
// dispatch. Reads the new prompt on stdin, then under one flock rewrites
// the inbox + resets the entry to a fresh ExecPending with a bumped
// generation. Returns the NEW gen for the resume DISPATCH block.
func newClaimsResetForRelaunchCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "reset-for-relaunch <dispatch-id>",
		Short:  "Rewrite inbox (stdin) + reset entry to fresh ExecPending (gen-bumped)",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return runClaimsResetForRelaunch(c.OutOrStdout(), c.InOrStdin(), args[0])
		},
	}
	return cmd
}

func runClaimsResetForRelaunch(stdout io.Writer, stdin io.Reader, dispatchID string) error {
	if _, err := state.Bootstrap(); err != nil {
		return emitErrorAndFail(stdout, dispatchID, dispatch.KindCoordPromptInbox,
			fmt.Errorf("bootstrap ~/.fleet: %w", err))
	}
	id, err := dispatch.NewDispatchID(dispatchID)
	if err != nil {
		return emitErrorAndFail(stdout, dispatchID, dispatch.KindCoordPromptInbox, err)
	}
	res, err := dispatch.ResetForRelaunch(id, stdin)
	if err != nil {
		return emitErrorAndFail(stdout, dispatchID, dispatch.KindCoordPromptInbox, err)
	}
	resp := claimsResponse{
		DispatchID: id.String(),
		Kind:       dispatch.KindCoordPromptInbox,
		Path:       res.Path,
	}
	switch res.Outcome {
	case dispatch.ResetDone:
		resp.Outcome = outcomeReset
		g := res.Generation
		resp.Generation = &g
	case dispatch.ResetAbsent:
		resp.Outcome = dispatch.OutcomeAbsent
	case dispatch.ResetContention:
		resp.Outcome = outcomeContention
	default:
		return emitErrorAndFail(stdout, dispatchID, dispatch.KindCoordPromptInbox,
			fmt.Errorf("unexpected reset-for-relaunch outcome %q", res.Outcome))
	}
	return emitOutcome(stdout, resp)
}
