package main

// fleet workers {list,prune} — operator-facing CLI for per-project
// worker state. Workers are NOT Fleet agents (they're `claude --print`
// subprocesses launched by the coordinator), so they don't show up in
// `fleet status`. This subcommand is the operator's window into their
// state.

import (
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/workers"
)

func newWorkersCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "workers",
		Short: "Inspect per-project worker state.json + archives",
		Long: `workers reads ~/.fleet/projects/<project>/workers/<slug>/state.json
files written by the coordinator's dispatched workers. v0.2 ships
list (active workers) and prune (clean archived dirs older than a
duration).

For tailing one worker's logs, use ` + "`fleet peek <slug> --logs`" + `.`,
	}
	cmd.AddCommand(
		newWorkersListCmd(),
		newWorkersPruneCmd(),
	)
	return cmd
}

// ---------- fleet workers list ----------

type workersListOpts struct {
	project string
	all     bool
}

func newWorkersListCmd() *cobra.Command {
	opts := &workersListOpts{}
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List active workers (and archived ones with --all)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWorkersList(opts, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&opts.project, "project", "", "project name (default: cwd basename)")
	cmd.Flags().BoolVar(&opts.all, "all", false, "include archived workers under workers/archive/")
	return cmd
}

func runWorkersList(opts *workersListOpts, stdout io.Writer) error {
	project, err := resolveProject(opts.project)
	if err != nil {
		return err
	}

	// Default path stays cheap: only scan workers/<slug>/. The archive
	// walk fires only when --all is set so a project with a large
	// archive doesn't pay for its size on every `fleet workers list`
	// (codex iter-5 P2). An unreadable archive can no longer break
	// active-worker listing either.
	active, err := workers.ListActive(project)
	if err != nil {
		return err
	}
	var archived []*workers.State
	if opts.all {
		_, archived, err = workers.ListAll(project)
		if err != nil {
			return err
		}
	}
	if !opts.all && len(active) == 0 {
		_, _ = fmt.Fprintln(stdout, "no active workers (run `fleet tasks add` to seed work for the coordinator)")
		return nil
	}

	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "SLUG\tSTATUS\tPHASE\tPID\tAGE\tLAST_HEARTBEAT")
	now := time.Now().UTC()
	for _, s := range active {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			s.Slug,
			workerLiveness(s),
			s.Phase,
			pidString(s.PID),
			humanAge(now.Sub(s.StartedAt)),
			humanAge(now.Sub(s.UpdatedAt)),
		)
	}
	for _, s := range archived {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			s.Slug,
			"archived",
			s.Phase,
			pidString(s.PID),
			humanAge(now.Sub(s.StartedAt)),
			humanAge(now.Sub(s.UpdatedAt)),
		)
	}
	return tw.Flush()
}

// workerLiveness reduces a worker's State to one of "alive", "dead",
// "starting", "done", "blocked", "failed". Coord's reconcile path
// uses the same signals; the CLI just renders them.
//
// "starting" is a separate bucket because workers.UpdateState
// bootstraps fresh state files with phase=starting and pid=0 until
// the coord knows the subprocess PID — in that window the CLI must
// not report healthy launching workers as dead (codex iter-6 P2).
func workerLiveness(s *workers.State) string {
	switch s.Phase {
	case workers.PhaseDone:
		return "done"
	case workers.PhaseBlocked:
		return "blocked"
	case workers.PhaseFailed:
		return "failed"
	}
	if s.PID > 0 {
		if workers.IsAlive(s.PID) {
			return "alive"
		}
		return "dead"
	}
	// PID not yet published. Phase=starting is the canonical
	// pre-PID bootstrap; treat any other phase the same way for
	// future-proofing if coord adds a new pre-PID phase.
	return "starting"
}

func pidString(pid int) string {
	if pid <= 0 {
		return "-"
	}
	return fmt.Sprintf("%d", pid)
}

// ---------- fleet workers prune ----------

type workersPruneOpts struct {
	project string
	older   string
}

func newWorkersPruneCmd() *cobra.Command {
	opts := &workersPruneOpts{}
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Remove archived worker directories older than a duration",
		Long: `prune deletes ~/.fleet/projects/<project>/workers/archive/<slug>-<ts>
directories whose embedded UTC stamp is older than --older-than. Uses
the embedded stamp (not mtime) so rename(2)-preserved mtimes don't
make a freshly-archived worker eligible for immediate prune.

Default --older-than is 7d (matches the design's retention math). Pass
0d to prune everything; the operator must opt in explicitly because
that erases the post-mortem trail.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWorkersPrune(opts, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&opts.project, "project", "", "project name (default: cwd basename)")
	cmd.Flags().StringVar(&opts.older, "older-than", "7d", "prune archive dirs older than this duration (e.g. 7d, 30d)")
	return cmd
}

func runWorkersPrune(opts *workersPruneOpts, stdout io.Writer) error {
	if _, err := state.Bootstrap(); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	project, err := resolveProject(opts.project)
	if err != nil {
		return err
	}
	dur, err := parseDurationLoose(opts.older)
	if err != nil {
		return fmt.Errorf("--older-than %q: %w", opts.older, err)
	}
	if dur < 0 {
		return fmt.Errorf("--older-than must be non-negative, got %s", dur)
	}
	cutoff := time.Now().UTC().Add(-dur)
	removed, err := workers.PruneArchive(project, cutoff)
	if err != nil {
		return fmt.Errorf("prune: %w", err)
	}
	_, _ = fmt.Fprintf(stdout, "pruned %d archived worker dir(s) older than %s\n", removed, dur)
	return nil
}
