package main

// fleet maintenance — operator-facing one-shot diagnostics that don't
// belong on any other subcommand. The first member is
// `bootstrap-remote-control`, a survey that flags live agents whose
// persisted Command lacks `--remote-control`. v1 is REPORT-ONLY: an
// actual live retrofit would require restarting the agent (operator's
// call), so the command just lists candidates with a one-line
// remediation suggestion.
//
// Lineage: handoff-remote-control-shell-wrapper-fix (the dispatch +
// handoff replacement paths now inject the flag correctly via the
// relaxed wrapper-pattern matcher in internal/spawn.InjectRemoteControlFlag,
// but agents spawned BEFORE that fix landed are still running without
// the flag — this survey gives operators a checklist of which to
// hand off when convenient).

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/tmux"
)

func newMaintenanceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "maintenance",
		Short: "One-shot diagnostics and repair helpers",
		Long: `maintenance groups operator-facing diagnostics that don't fit
on any other subcommand.

Subcommands:
  bootstrap-remote-control  — list live agents missing --remote-control
                              in their persisted Command (report only).`,
	}
	cmd.AddCommand(newMaintenanceBootstrapRemoteControlCmd())
	return cmd
}

func newMaintenanceBootstrapRemoteControlCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bootstrap-remote-control",
		Short: "Survey live agents missing the --remote-control flag",
		Long: `bootstrap-remote-control walks ~/.fleet/agents/*.json and
identifies live agents whose persisted Command lacks --remote-control.
Live = the record's tmux session is currently alive (HasSession). For
each match, the command prints a one-line remediation suggestion
("fleet handoff <id>" is the typical fix; the replacement spawn goes
through the relaxed wrapper-pattern matcher and gets the flag injected
automatically).

This is REPORT-ONLY in v1. An actual live retrofit would require
restarting the agent's tmux session, which is the operator's call —
some agents may be mid-task and worth letting run to natural completion.

Exit codes:
  0  — survey ran (regardless of how many agents need attention)
  1  — read error (agents/ dir, etc.)
`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runMaintenanceBootstrapRemoteControl(
				cmd.OutOrStdout(),
				agent.List,
				tmux.HasSession,
			)
		},
	}
	return cmd
}

// runMaintenanceBootstrapRemoteControl is the testable core. The
// listFn / hasSessionFn injection lets tests substitute fake
// agent-store and tmux-presence views (no real tmux required).
func runMaintenanceBootstrapRemoteControl(
	stdout io.Writer,
	listFn func() ([]*agent.Record, error),
	hasSessionFn func(string) bool,
) error {
	records, err := listFn()
	if err != nil {
		return fmt.Errorf("list agents: %w", err)
	}
	type missing struct {
		id        string
		project   string
		taskID    string
		spawnedAt string
		session   string
	}
	var report []missing
	for _, r := range records {
		if r == nil {
			continue
		}
		if commandHasRemoteControl(r.Command) {
			continue
		}
		if !hasSessionFn(r.TmuxSession) {
			// Dead session — handoff is moot, the record will be
			// reaped by the normal reconciliation path. Don't pollute
			// the report.
			continue
		}
		report = append(report, missing{
			id:        r.ID,
			project:   r.Project,
			taskID:    r.TaskID,
			spawnedAt: r.SpawnedAt.UTC().Format("2006-01-02T15:04:05Z"),
			session:   r.TmuxSession,
		})
	}
	if len(report) == 0 {
		_, _ = fmt.Fprintln(stdout, "no live agents are missing --remote-control")
		return nil
	}
	// Stable order: by spawnedAt ascending, then ID. So the operator
	// sees the oldest "stuck without remote-control" agents first.
	sort.SliceStable(report, func(i, j int) bool {
		if report[i].spawnedAt != report[j].spawnedAt {
			return report[i].spawnedAt < report[j].spawnedAt
		}
		return report[i].id < report[j].id
	})
	_, _ = fmt.Fprintf(stdout,
		"%d live agent(s) missing --remote-control:\n\n", len(report))
	for _, m := range report {
		_, _ = fmt.Fprintf(stdout,
			"  %s  project=%s  task=%s  spawned=%s\n"+
				"     remediation: fleet handoff %s   (replacement spawn auto-injects the flag)\n\n",
			m.id, m.project, m.taskID, m.spawnedAt, m.id)
	}
	return nil
}

// commandHasRemoteControl returns true iff any element of the
// persisted Command argv contains the literal `--remote-control`
// substring. Conservative match: if the operator's custom command
// happens to embed the flag literally (e.g. inside a comment) we'd
// report it as already-flagged, but that's fine — the survey errs
// on the side of NOT pestering the operator about a false positive.
func commandHasRemoteControl(command []string) bool {
	for _, el := range command {
		if strings.Contains(el, "--remote-control") {
			return true
		}
	}
	return false
}
