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
}

// claimsExitCode maps outcome strings to the stable exit code table.
// Any outcome not in this table exits 1 (caller error).
func claimsExitCode(outcome string) int {
	switch outcome {
	case dispatch.OutcomeAcquired,
		dispatch.OutcomeAlreadyAcquired,
		dispatch.OutcomeReleased,
		dispatch.OutcomeAlreadyReleased:
		return 0
	case dispatch.OutcomeNotOwned:
		return 10
	case dispatch.OutcomeAbsent:
		return 11
	case dispatch.OutcomeContested:
		return 12
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
