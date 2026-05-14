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
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/state"
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
                              in their persisted Command (report only).
  prune-orphan-tmux         — find tmux sessions named fleet-<id> that
                              have no matching live agent record. Dry-run
                              by default; pass --kill to actually kill.`,
	}
	cmd.AddCommand(newMaintenanceBootstrapRemoteControlCmd())
	cmd.AddCommand(newMaintenancePruneOrphanTmuxCmd())
	return cmd
}

// newMaintenancePruneOrphanTmuxCmd wires `fleet maintenance
// prune-orphan-tmux`. Defensive sweeper for the orphan tmux session
// leak that crashed the operator's Mac twice (PR
// fix/orphan-tmux-sweeper-and-leak-plug). The same PR plugs the root
// cause in handoff.go / handoffop.go — this command is the safety net
// for orphans left over from buggy builds AND a periodic operator
// hygiene tool going forward.
func newMaintenancePruneOrphanTmuxCmd() *cobra.Command {
	var kill bool
	cmd := &cobra.Command{
		Use:   "prune-orphan-tmux",
		Short: "Find tmux sessions named fleet-<id> with no live agent record",
		Long: `prune-orphan-tmux enumerates tmux sessions whose name starts with
"fleet-" via ` + "`tmux ls -F '#{session_name}'`" + `, derives the agent ID
from the suffix, and checks whether a live agent record exists at
~/.fleet/agents/<id>.json. Sessions whose ID has NO live record are
orphans — most likely the residue of a previous build's leak.

Default behavior is DRY-RUN: prints the orphan list and exits 0 WITHOUT
killing. Pass --kill to actually run ` + "`tmux kill-session`" + ` on
each orphan.

Output format (one line per session):
  <session>  orphan=<true|false>  state=<missing|live>  killed=<true|false>

Idempotent. Safe to run while live fleet agents are working — only
sessions whose state.json is absent are candidates, so live agents are
never touched. Sessions whose name doesn't match the fleet-<id> pattern
are also left alone.

Exit code is always 0 (even if some kills failed; warnings go to
stderr) so the operator can chain this into cron / a startup script
without worrying about ordering edge cases.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runMaintenancePruneOrphanTmux(
				cmd.OutOrStdout(),
				cmd.ErrOrStderr(),
				kill,
				tmux.ListSessions,
				agentRecordExists,
				tmux.Kill,
			)
		},
	}
	cmd.Flags().BoolVar(&kill, "kill", false,
		"actually kill the orphan tmux sessions (default: dry-run, list only)")
	return cmd
}

// agentRecordExists reports whether ~/.fleet/agents/<id>.json exists
// AS A LIVE record (archive copies don't count — the operator's brief
// is explicit that "live state.json" is the gate).
//
// Returns false on both "file does not exist" AND "Stat error" (e.g.,
// FLEET_HOME unresolvable). The fallback is conservative for safety:
// if we can't be sure the record is live, we still treat the session
// as "has record" so we don't accidentally kill a session for which we
// just lost read access to its record. The orphan-detection bar is
// "missing record", not "ambiguous record" — operator can retry after
// fixing perms.
func agentRecordExists(id string) bool {
	path, err := state.AgentPath(id)
	if err != nil {
		// Path resolution failure → can't even probe. Conservative:
		// treat as "live" so we don't kill on ambiguity.
		return true
	}
	_, statErr := os.Stat(path)
	switch {
	case statErr == nil:
		return true
	case errors.Is(statErr, os.ErrNotExist):
		return false
	default:
		// Other stat error (permission denied, I/O failure). Be
		// conservative — don't classify as orphan.
		return true
	}
}

// runMaintenancePruneOrphanTmux is the testable core. listFn yields the
// tmux session names on the active server; existsFn reports whether an
// agent record is live; killFn kills a session (only called when
// performKill is true and the session is an orphan).
//
// Output contract (pinned by tests):
//   - One line per session whose name starts with "fleet-".
//   - Lines start with the session name, followed by three key=value
//     pairs separated by two spaces: orphan=, state=, killed=.
//   - Sessions whose name doesn't start with "fleet-" are not listed
//     (out of scope for this command).
//   - Stable ordering: alphabetical by session name.
func runMaintenancePruneOrphanTmux(
	stdout, stderr io.Writer,
	performKill bool,
	listFn func() ([]string, error),
	existsFn func(id string) bool,
	killFn func(session string) error,
) error {
	sessions, err := listFn()
	if err != nil {
		return fmt.Errorf("list tmux sessions: %w", err)
	}
	type row struct {
		session string
		id      string
		orphan  bool
		killed  bool
	}
	var rows []row
	for _, s := range sessions {
		if !strings.HasPrefix(s, "fleet-") {
			// Not a fleet-managed session; out of scope.
			continue
		}
		id := strings.TrimPrefix(s, "fleet-")
		if id == "" {
			// Session name was literally "fleet-" — odd but treat as
			// non-fleet so we don't accidentally kill something we
			// can't identify.
			continue
		}
		live := existsFn(id)
		r := row{session: s, id: id, orphan: !live}
		if performKill && r.orphan {
			if kerr := killFn(s); kerr != nil {
				_, _ = fmt.Fprintf(stderr,
					"warning: kill %s: %v (continuing)\n", s, kerr)
			} else {
				r.killed = true
			}
		}
		rows = append(rows, r)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return rows[i].session < rows[j].session
	})
	for _, r := range rows {
		stateLabel := "live"
		if r.orphan {
			stateLabel = "missing"
		}
		_, _ = fmt.Fprintf(stdout,
			"%s  orphan=%t  state=%s  killed=%t\n",
			r.session, r.orphan, stateLabel, r.killed)
	}
	return nil
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
