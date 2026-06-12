// Package main — `fleet rc` operator-facing CLI subtree.
//
// Native model (v0.14): remote control is DEFAULT-ON. Every coord
// spawn bakes `--remote-control "fleet-coord-<id>-<project>"` into
// the coord's own claude argv; there is no standalone listener daemon
// and no send-keys injection. The CLI manages the per-project opt-OUT
// marker and cleans up legacy (pre-native) listener daemons:
//
//	fleet rc up      <project>            — re-enable (remove opt-out marker)
//	fleet rc down    <project>            — disable (write opt-out marker; reap legacy listener)
//	fleet rc connect <project>            — DEPRECATED no-op (native startup replaced it)
//	fleet rc status  [<project>] [--healthy]
//	fleet rc list                         — projects with RC DISABLED (the exceptions)
//	fleet rc reset   [<project>]          — back to pristine default-on
//
// Stable JSON envelopes + exit codes.
//
// Exit-code table (mirrors fleet claims):
//
//	acquired / already_acquired / released / already_released /
//	native_default      → 0
//	not_enabled         → 10
//	not_owned           → 10
//	absent              → 11
//	contested           → 12
//	error               → 1
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/edisonshen/fleet/internal/rc"
	"github.com/edisonshen/fleet/internal/state"
)

// rcResponse is the JSON envelope shared by every `fleet rc`
// subcommand. Field set kept stable across commands; subcommands
// populate the subset they care about. Optional fields are omitempty
// so the JSON parses identically whether or not they're present.
type rcResponse struct {
	Outcome     string    `json:"outcome"`
	Project     string    `json:"project,omitempty"`
	Cmd         string    `json:"cmd,omitempty"`
	ListenerPID int       `json:"listener_pid,omitempty"`
	WorkingDir  string    `json:"working_dir,omitempty"`
	CoordID     string    `json:"coord_id,omitempty"`
	TmuxSession string    `json:"tmux_session,omitempty"`
	Retried     bool      `json:"retried,omitempty"`
	Warn        string    `json:"warn,omitempty"`
	Health      string    `json:"health,omitempty"`
	Diagnostic  string    `json:"diagnostic,omitempty"`
	Projects    []string  `json:"projects,omitempty"`
	State       *rc.State `json:"state,omitempty"`
	Error       string    `json:"error,omitempty"`
}

// errRC is a sentinel carrying the outcome so main.go derives the
// exit code via errors.As. Mirrors errClaimsError shape.
type errRC struct {
	outcome string
}

func (e errRC) Error() string { return "rc: outcome=" + e.outcome }

// rcOutcomeFromErr extracts the outcome from a sentinel error.
func rcOutcomeFromErr(err error) string {
	var e errRC
	if errors.As(err, &e) {
		return e.outcome
	}
	return ""
}

// rcExitCode maps outcome strings to the stable exit code table.
func rcExitCode(outcome string) int {
	switch outcome {
	case rc.OutcomeAcquired,
		rc.OutcomeAlreadyAcquired,
		rc.OutcomeReleased,
		rc.OutcomeAlreadyReleased,
		rc.OutcomeNativeDefault:
		return 0
	case rc.OutcomeNotEnabled, rc.OutcomeNotOwned:
		return 10
	case rc.OutcomeAbsent:
		return 11
	case rc.OutcomeContested:
		return 12
	default:
		return 1
	}
}

func newRCCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rc",
		Short: "Manage per-project remote control (native, default-on; opt-out via rc-disabled)",
		Long: `fleet rc — per-project remote-control gate (native model).

Remote control is DEFAULT-ON: every coord spawn bakes
--remote-control "fleet-coord-<id>-<project>" into the coord's own
claude argv, so pairing from mobile / claude.ai works the moment the
coord starts. No standalone listener daemon, no slash-command
injection.

'fleet rc down <p>' opts a project OUT (writes
~/.fleet/projects/<p>/rc-disabled; takes effect on the next coord
spawn). 'fleet rc up <p>' removes the opt-out. The attach-surface
gates (dispatch, handoff, auto-handoff drain) consult
rc.Enabled(<project>) before baking the flag.

Legacy (pre-native) standalone listeners are reaped by 'fleet rc
down/reset' and the background sweep; nothing spawns new ones.`,
	}
	cmd.AddCommand(newRCUpCmd())
	cmd.AddCommand(newRCDownCmd())
	cmd.AddCommand(newRCConnectCmd())
	cmd.AddCommand(newRCStatusCmd())
	cmd.AddCommand(newRCListCmd())
	cmd.AddCommand(newRCResetCmd())
	return cmd
}

func newRCUpCmd() *cobra.Command {
	var cwd string
	var idempotent bool
	var respawnOnly bool
	var coordID string
	cmd := &cobra.Command{
		Use:   "up <project>",
		Short: "Re-enable RC for project (remove the rc-disabled opt-out marker; takes effect on next coord spawn)",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return runRCUp(c.OutOrStdout(), args[0], respawnOnly)
		},
	}
	// Legacy flags — accepted and IGNORED so pre-native skill copies
	// (which shell out `fleet rc up <p> --respawn-only --idempotent
	// [--coord-id <id>] [--cwd <path>]` every coord tick) keep getting
	// a stable zero-exit no-op instead of a cobra unknown-flag error.
	// --respawn-only retains its load-bearing back-compat semantics
	// (never enable, never spawn — see rc.UpOpts.RespawnOnly).
	cmd.Flags().StringVar(&cwd, "cwd", "", "DEPRECATED no-op (legacy listener working_dir override)")
	cmd.Flags().BoolVar(&idempotent, "idempotent", false, "DEPRECATED no-op (up is always idempotent)")
	cmd.Flags().BoolVar(&respawnOnly, "respawn-only", false, "legacy coord-tick compat: pure no-op, never enables RC (exit 10 when opted out, 0 otherwise)")
	cmd.Flags().StringVar(&coordID, "coord-id", "", "DEPRECATED no-op (legacy listener owner tracking)")
	_ = cwd
	_ = idempotent
	_ = coordID
	return cmd
}

func runRCUp(stdout io.Writer, project string, respawnOnly bool) error {
	if _, err := state.Bootstrap(); err != nil {
		return emitRC(stdout, rcResponse{Outcome: rc.OutcomeError, Cmd: "up", Project: project, Error: err.Error()})
	}
	out, err := rc.Up(project, rc.UpOpts{RespawnOnly: respawnOnly})
	resp := rcResponse{Outcome: out, Cmd: "up", Project: project}
	if err != nil {
		resp.Error = err.Error()
	}
	return emitRC(stdout, resp)
}

func newRCDownCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "down <project>",
		Short: "Disable RC for project (write rc-disabled opt-out marker; reap any legacy listener)",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return runRCDown(c.OutOrStdout(), args[0])
		},
	}
	return cmd
}

func runRCDown(stdout io.Writer, project string) error {
	if _, err := state.Bootstrap(); err != nil {
		return emitRC(stdout, rcResponse{Outcome: rc.OutcomeError, Cmd: "down", Project: project, Error: err.Error()})
	}
	out, err := rc.Down(project)
	resp := rcResponse{Outcome: out, Cmd: "down", Project: project}
	if err != nil {
		resp.Error = err.Error()
	}
	return emitRC(stdout, resp)
}

func newRCConnectCmd() *cobra.Command {
	var coordID string
	cmd := &cobra.Command{
		Use:   "connect <project>",
		Short: "DEPRECATED no-op: RC starts natively with the coord (claude --remote-control at spawn)",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return runRCConnect(c.OutOrStdout(), args[0])
		},
	}
	// Accepted and ignored for script back-compat.
	cmd.Flags().StringVar(&coordID, "coord", "", "DEPRECATED no-op (legacy send-keys target selection)")
	_ = coordID
	return cmd
}

// runRCConnect is the deprecation shim for the retired send-keys
// attach path. The v0.12 implementation drove the /remote-control
// slash command into the coord's tmux pane; the native model bakes
// --remote-control into the coord spawn argv, so there is nothing to
// connect after the fact. Exit 0 with a diagnostic — failing here
// would break legacy handoff docs that still instruct the operator to
// run `fleet rc connect <project>`.
func runRCConnect(stdout io.Writer, project string) error {
	if _, err := state.Bootstrap(); err != nil {
		return emitRC(stdout, rcResponse{Outcome: rc.OutcomeError, Cmd: "connect", Project: project, Error: err.Error()})
	}
	diag := "deprecated: remote control starts natively with the coord " +
		"(claude --remote-control at spawn) — nothing to connect. "
	if !rc.Enabled(project) {
		diag += fmt.Sprintf("NOTE: RC is currently DISABLED for project %q; "+
			"run `fleet rc up %s` to re-enable on the next coord spawn. ", project, project)
	}
	diag += "For a coord spawned before the native model, `fleet handoff <coord-id>` " +
		"respawns it with RC, or type /remote-control in its pane."
	return emitRC(stdout, rcResponse{
		Outcome:    rc.OutcomeNativeDefault,
		Cmd:        "connect",
		Project:    project,
		Diagnostic: diag,
	})
}

func newRCStatusCmd() *cobra.Command {
	var healthy bool
	cmd := &cobra.Command{
		Use:   "status [<project>]",
		Short: "Report RC state (enabled = no rc-disabled marker; legacy listener fields if any). --healthy probes claude daemon registry.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			var project string
			if len(args) == 1 {
				project = args[0]
			}
			return runRCStatus(c.OutOrStdout(), project, healthy)
		},
	}
	cmd.Flags().BoolVar(&healthy, "healthy", false, "probe claude daemon remote-control list to verify the listener is registered")
	return cmd
}

func runRCStatus(stdout io.Writer, project string, healthy bool) error {
	if _, err := state.Bootstrap(); err != nil {
		return emitRC(stdout, rcResponse{Outcome: rc.OutcomeError, Cmd: "status", Project: project, Error: err.Error()})
	}
	if project == "" {
		// No-arg status: enumerate the exceptions — projects with the
		// rc-disabled opt-out marker (everything else is enabled by
		// default under the native model). An EMPTY list is the
		// healthy steady-state ("RC enabled everywhere"), so it maps
		// to a success outcome / exit 0 — health checks must not fail
		// on a clean install (codex review iter-2 [P2]). JSON
		// consumers distinguish via the empty/omitted projects array.
		projs, err := rc.ListDisabled()
		if err != nil {
			return emitRC(stdout, rcResponse{Outcome: rc.OutcomeError, Cmd: "status", Error: err.Error()})
		}
		return emitRC(stdout, rcResponse{Outcome: rc.OutcomeAcquired, Cmd: "status", Projects: projs})
	}
	s, err := rc.Inspect(project)
	if err != nil {
		return emitRC(stdout, rcResponse{Outcome: rc.OutcomeError, Cmd: "status", Project: project, Error: err.Error()})
	}
	resp := rcResponse{
		Outcome: rc.OutcomeAcquired,
		Cmd:     "status",
		Project: project,
		State:   &s,
	}
	// absent = opted out (rc-disabled marker present). Enabled is the
	// default; the listener fields in State are legacy observability.
	if !s.Enabled {
		resp.Outcome = rc.OutcomeAbsent
	}
	if healthy {
		h := rc.Health(project)
		resp.Health = h.Status
		resp.Diagnostic = h.Diagnostic
	}
	return emitRC(stdout, resp)
}

func newRCListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "Enumerate projects with RC DISABLED (rc-disabled opt-out marker present)",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, args []string) error {
			return runRCList(c.OutOrStdout())
		},
	}
	return cmd
}

func runRCList(stdout io.Writer) error {
	if _, err := state.Bootstrap(); err != nil {
		return emitRC(stdout, rcResponse{Outcome: rc.OutcomeError, Cmd: "list", Error: err.Error()})
	}
	projs, err := rc.ListDisabled()
	if err != nil {
		return emitRC(stdout, rcResponse{Outcome: rc.OutcomeError, Cmd: "list", Error: err.Error()})
	}
	return emitRC(stdout, rcResponse{Outcome: rc.OutcomeAcquired, Cmd: "list", Projects: projs})
}

func newRCResetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reset [<project>]",
		Short: "Operator emergency: back to pristine default-on (reap legacy listener, remove ALL rc markers)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			var project string
			if len(args) == 1 {
				project = args[0]
			}
			return runRCReset(c.OutOrStdout(), project)
		},
	}
	return cmd
}

func runRCReset(stdout io.Writer, project string) error {
	if _, err := state.Bootstrap(); err != nil {
		return emitRC(stdout, rcResponse{Outcome: rc.OutcomeError, Cmd: "reset", Project: project, Error: err.Error()})
	}
	out, err := rc.Reset(project)
	resp := rcResponse{Outcome: out, Cmd: "reset", Project: project}
	if err != nil {
		resp.Error = err.Error()
	}
	return emitRC(stdout, resp)
}

// emitRC writes the JSON envelope + maps non-zero-exit outcomes to
// an errRC sentinel. Mirrors emitOutcome from claims.go.
func emitRC(stdout io.Writer, resp rcResponse) error {
	data, err := json.Marshal(resp)
	if err != nil {
		fallback, _ := json.Marshal(rcResponse{Outcome: rc.OutcomeError, Error: err.Error()})
		_, _ = stdout.Write(append(fallback, '\n'))
		return errRC{outcome: rc.OutcomeError}
	}
	if _, werr := stdout.Write(append(data, '\n')); werr != nil {
		return werr
	}
	switch resp.Outcome {
	case rc.OutcomeAcquired,
		rc.OutcomeAlreadyAcquired,
		rc.OutcomeReleased,
		rc.OutcomeAlreadyReleased,
		rc.OutcomeNativeDefault:
		return nil
	default:
		return errRC{outcome: resp.Outcome}
	}
}

// formatRCError shapes a generic error into the JSON envelope shape
// for the `error` outcome. Used by RunE when err != nil but the rc
// package didn't surface an outcome string.
func formatRCError(_ string, err error) rcResponse {
	return rcResponse{Outcome: rc.OutcomeError, Error: fmt.Sprintf("%v", err)}
}

// _ kills the unused-import lint for formatRCError; kept exported
// for future callers that want a simpler error shape.
var _ = formatRCError
