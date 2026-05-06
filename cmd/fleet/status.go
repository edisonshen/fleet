package main

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/version"
)

type statusOpts struct {
	jsonOut bool
}

func newStatusCmd() *cobra.Command {
	opts := &statusOpts{}
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Print a one-shot health summary of every live agent",
		Long: `status reads ~/.fleet/agents/*.json and prints a one-line
summary per agent. Default output is human-readable; --json emits the
raw records for shell scripting.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runStatus(opts, cmd.OutOrStdout(), Version)
		},
	}
	cmd.Flags().BoolVar(&opts.jsonOut, "json", false,
		"emit raw agent records as a JSON array")
	return cmd
}

func runStatus(opts *statusOpts, stdout io.Writer, current string) error {
	records, err := agent.List()
	if err != nil {
		return err
	}
	// Sort by spawned_at descending — newest first reads better when
	// triaging "what's running right now."
	sort.Slice(records, func(i, j int) bool {
		return records[i].SpawnedAt.After(records[j].SpawnedAt)
	})

	if opts.jsonOut {
		if records == nil {
			records = []*agent.Record{}
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		// JSON path stays a single record array — appending the
		// nudge would break jq pipelines and `fleet status --json |
		// fleet ...` chains. Operators piping through jq see only
		// the records.
		return enc.Encode(records)
	}

	if len(records) == 0 {
		_, _ = fmt.Fprintln(stdout, "no agents (run `fleet dispatch <task-id>` to start one)")
	} else {
		tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "AGENT\tPROJECT\tTASK\tMODE\tAGE\tBLOCKED")
		for _, r := range records {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
				r.ID,
				defaultStr(r.Project, "-"),
				defaultStr(r.TaskID, "-"),
				defaultStr(r.Mode, "-"),
				humanAge(time.Since(r.SpawnedAt)),
				boolStr(r.Blocked),
			)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}

	// Upgrade nudge footer. Pure read against ~/.fleet/version_check.json;
	// silent when no cache, no upgrade, or dev build. Same source of
	// truth as the TUI banner — single chip, identical format.
	if nudge := version.Nudge(current); nudge != "" {
		_, _ = fmt.Fprintln(stdout, nudge)
	}
	return nil
}

func defaultStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func boolStr(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// humanAge formats a duration as "5m", "2h", "3d" — short enough for
// table columns, precise enough to triage.
func humanAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
