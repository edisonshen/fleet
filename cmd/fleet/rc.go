// Package main — `fleet rc` operator-facing CLI subtree.
//
// Implements docs/DESIGN-rc-listener-lifecycle.md v8. The operator
// controls per-project remote-control listeners via:
//
//	fleet rc up      <project> [--cwd <path>] [--idempotent]
//	fleet rc down    <project>
//	fleet rc connect <project> [--coord <id>]
//	fleet rc status  [<project>] [--healthy]
//	fleet rc list
//	fleet rc reset   [<project>]
//
// Stable JSON envelopes + exit codes; the Python skill
// (skills/coordinator/remote_control.py:spawn_daemon_if_needed)
// consumes `fleet rc up --idempotent` and routes ALL spawn through
// the Go controller (codex round 2: single owner).
//
// Exit-code table (mirrors fleet claims):
//
//	acquired / already_acquired / released / already_released /
//	connected           → 0
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
		rc.OutcomeConnected,
		// leak-rc-daemon-lifecycle PR-B: self-heal outcomes are
		// success codes (the daemon is now alive at the requested
		// version with the new owner). Treat the same as acquired.
		rc.OutcomeRespawnedStaleVersion,
		rc.OutcomeRespawnedDeadOwner:
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
		Short: "Manage per-project remote-control listeners (claude remote-control)",
		Long: `fleet rc — operator-managed per-project listener lifecycle.

Replaces implicit-spawn (PR #157 env-gate) with an explicit marker +
state.json controlled by 'fleet rc up/down/connect'. The 6 attach-
surface gates (dispatch, handoff, auto-handoff drain, skills) consult
rc.Enabled(<project>) before injecting --remote-control or spawning a
listener.

See docs/DESIGN-rc-listener-lifecycle.md for the design spec.`,
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
		Short: "Enable RC for project (create marker + spawn listener; idempotent)",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return runRCUp(c.OutOrStdout(), args[0], cwd, idempotent, respawnOnly, coordID)
		},
	}
	cmd.Flags().StringVar(&cwd, "cwd", "", "explicit working_dir override (highest priority resolution source)")
	cmd.Flags().BoolVar(&idempotent, "idempotent", false, "skill-friendly invocation: never error when already up (alias for stable-zero exit on already_acquired)")
	cmd.Flags().BoolVar(&respawnOnly, "respawn-only", false, "respawn dead listener only; refuse to create marker (Python coord-tick safety: never auto-enable RC on a project the operator hasn't opted in to)")
	cmd.Flags().StringVar(&coordID, "coord-id", "",
		"owning coord agent ID, persisted in rc-state.json so self-healing Up can detect dead-owner across crashes (leak-rc-daemon-lifecycle PR-B)")
	_ = idempotent // currently absorbed by Up's idempotent semantics
	return cmd
}

func runRCUp(stdout io.Writer, project, cwd string, _ bool, respawnOnly bool, coordID string) error {
	if _, err := state.Bootstrap(); err != nil {
		return emitRC(stdout, rcResponse{Outcome: rc.OutcomeError, Cmd: "up", Project: project, Error: err.Error()})
	}
	out, err := rc.Up(project, rc.UpOpts{Cwd: cwd, RespawnOnly: respawnOnly, CoordID: coordID})
	resp := rcResponse{Outcome: out, Cmd: "up", Project: project}
	if err != nil {
		resp.Error = err.Error()
	}
	if s, sErr := rc.Inspect(project); sErr == nil {
		resp.ListenerPID = s.ListenerPID
		resp.WorkingDir = s.WorkingDir
	}
	return emitRC(stdout, resp)
}

func newRCDownCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "down <project>",
		Short: "Disable RC for project (kill listener + remove marker)",
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
		Short: "Drive in-session /remote-control on the project's coord pane (submit-verified send-keys)",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return runRCConnect(c.OutOrStdout(), args[0], coordID)
		},
	}
	cmd.Flags().StringVar(&coordID, "coord", "", "target coord agent ID (default: coord-spawn-marker holder)")
	return cmd
}

func runRCConnect(stdout io.Writer, project, coordID string) error {
	if _, err := state.Bootstrap(); err != nil {
		return emitRC(stdout, rcResponse{Outcome: rc.OutcomeError, Cmd: "connect", Project: project, Error: err.Error()})
	}
	res, err := rc.Connect(project, rc.ConnectOpts{CoordID: coordID})
	resp := rcResponse{
		Outcome:     res.Outcome,
		Cmd:         "connect",
		Project:     project,
		CoordID:     res.CoordID,
		TmuxSession: res.TmuxSession,
		Retried:     res.Retried,
		Warn:        res.Warn,
	}
	if err != nil {
		resp.Error = err.Error()
	}
	return emitRC(stdout, resp)
}

func newRCStatusCmd() *cobra.Command {
	var healthy bool
	cmd := &cobra.Command{
		Use:   "status [<project>]",
		Short: "Report observed RC state (marker present? listener alive?). --healthy probes claude daemon registry.",
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
		// No-arg status: enumerate all marked projects.
		projs, err := rc.List()
		if err != nil {
			return emitRC(stdout, rcResponse{Outcome: rc.OutcomeError, Cmd: "status", Error: err.Error()})
		}
		// Use absent when no projects are enabled so JSON consumers
		// have a unambiguous "nothing to inspect" signal.
		if len(projs) == 0 {
			return emitRC(stdout, rcResponse{Outcome: rc.OutcomeAbsent, Cmd: "status", Projects: nil})
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
		Short: "Enumerate projects with RC enabled (marker present)",
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
	projs, err := rc.List()
	if err != nil {
		return emitRC(stdout, rcResponse{Outcome: rc.OutcomeError, Cmd: "list", Error: err.Error()})
	}
	return emitRC(stdout, rcResponse{Outcome: rc.OutcomeAcquired, Cmd: "list", Projects: projs})
}

func newRCResetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reset [<project>]",
		Short: "Operator emergency: remove marker + kill listener (or all projects)",
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
		rc.OutcomeConnected:
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
