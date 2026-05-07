package main

// fleet workers {list,prune} — operator-facing CLI for per-project
// worker state. Workers are NOT Fleet agents (they're `claude --print`
// subprocesses launched by the coordinator), so they don't show up in
// `fleet status`. This subcommand is the operator's window into their
// state.

import (
	"errors"
	"fmt"
	"io"
	"strings"
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
		newWorkersUpdateCmd(),
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

// ---------- fleet workers update ----------

// workersUpdateOpts captures the worker-side mutation surface. Workers
// (coord-dispatched `claude --print` subprocesses) call this on every
// phase boundary to publish their progress; the coordinator's reconcile
// loop reads the resulting state.json and decides what to do next.
//
// `--phase` is required because every legitimate use of this command
// is "I just transitioned to phase X". `--pr-url` and `--reason` are
// phase-coupled: phase=done requires a PR URL (workers.writeStateLocked
// enforces ErrPhaseRequiresPR), phase=blocked requires a reason
// (ErrPhaseRequiresWhy). Setting --pid records the worker's own OS
// PID so coord's reconcile can liveness-check via kill(0).
type workersUpdateOpts struct {
	project string
	phase   string
	prURL   string
	reason  string
	pid     int
	exit    int
	exitSet bool // distinguishes "not passed" from "exit=0"
	pidSet  bool // distinguishes "not passed" from "pid=0"
}

func newWorkersUpdateCmd() *cobra.Command {
	opts := &workersUpdateOpts{}
	cmd := &cobra.Command{
		Use:   "update <slug>",
		Short: "Update one worker's state.json (called by the worker subprocess)",
		Long: `update writes a phase boundary into the worker's state.json under
~/.fleet/projects/<project>/workers/<slug>/state.json. Workers run this
on every phase change in their TDD → review → push pipeline so the
coordinator can reconcile in-flight tasks correctly.

Phases: starting, branch, tdd-red, tdd-green, tdd-refactor,
review-claude, review-codex, push, done, blocked, failed.

Phase=done requires --pr-url. Phase=blocked requires --reason. The
state file is created on first call (the coord pre-seeds it on
dispatch but a missing file is bootstrapped here so workers don't
fail open).

Bootstraps the worker dir + state.json if absent (mirrors the coord's
pre-seed) so a missing file does not fail the worker on its first
phase update. PID defaults to the caller's os.Getpid(); pass --pid
explicitly when the caller is a wrapper script.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Detect explicit --exit / --pid so default 0 doesn't
			// accidentally shadow a still-running worker. Codex
			// iter-3 [P1]: defaulting --pid to os.Getpid() of the
			// short-lived `fleet` helper made workers list/peek
			// report active workers as dead almost immediately
			// (the helper exits, the recorded pid dies, the
			// rendered state shows "dead" until the next phase
			// boundary writes a fresh transient pid).
			opts.exitSet = cmd.Flags().Changed("exit")
			opts.pidSet = cmd.Flags().Changed("pid")
			return runWorkersUpdate(args[0], opts, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&opts.project, "project", "", "project name (default: cwd basename)")
	cmd.Flags().StringVar(&opts.phase, "phase", "",
		"new worker phase (starting|branch|tdd-red|tdd-green|tdd-refactor|review-claude|review-codex|push|done|blocked|failed)")
	cmd.Flags().StringVar(&opts.prURL, "pr-url", "", "PR URL (required for phase=done)")
	cmd.Flags().StringVar(&opts.reason, "reason", "", "blocked reason (required for phase=blocked)")
	cmd.Flags().IntVar(&opts.pid, "pid", 0, "worker OS PID (default: os.Getpid())")
	cmd.Flags().IntVar(&opts.exit, "exit", 0, "worker exit code (set on phase=done|failed)")
	_ = cmd.MarkFlagRequired("phase")
	return cmd
}

func runWorkersUpdate(slug string, opts *workersUpdateOpts, stdout io.Writer) error {
	if _, err := state.Bootstrap(); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	project, err := resolveProject(opts.project)
	if err != nil {
		return err
	}
	phase := workers.Phase(strings.TrimSpace(opts.phase))
	if phase == "" {
		return errors.New("--phase is required")
	}

	updateErr := workers.UpdateState(project, slug, func(s *workers.State) {
		// Record phase transition: append the previous phase to the
		// completed list so workers.list / peek can show "5/9 phases
		// done" without losing history. Skip the append on the very
		// first transition out of "starting" (= bootstrap default)
		// so we don't double-count it.
		if s.Phase != "" && s.Phase != phase {
			s.PhasesCompleted = append(s.PhasesCompleted, s.Phase)
		}
		s.Phase = phase
		// Only set pid when the caller passed --pid explicitly.
		// Defaulting to os.Getpid() captured the short-lived
		// `fleet` helper PID and made workers look dead immediately
		// after each phase update (codex iter-3 [P1]). Without
		// --pid, preserve whatever pid is already in state.json
		// (typically 0 from the dispatch bootstrap).
		if opts.pidSet {
			s.PID = opts.pid
		}
		// Phase-specific terminal fields. Codex iter-4 [P2]: when
		// state.json is reused for a retry (worker restart on the
		// same slug after CI-red), --phase starting must not leave
		// the previous attempt's pr_url / blocked_reason / exit
		// hanging around. We clear them on every non-terminal
		// transition (starting, branch, tdd-*, review-*, push) and
		// only re-write them when the caller explicitly passes the
		// matching flag.
		switch phase {
		case workers.PhaseDone:
			if strings.TrimSpace(opts.prURL) != "" {
				s.PRURL = strings.TrimSpace(opts.prURL)
			}
			if opts.exitSet {
				ec := opts.exit
				s.Exit = &ec
			}
		case workers.PhaseBlocked:
			if strings.TrimSpace(opts.reason) != "" {
				s.BlockedReason = strings.TrimSpace(opts.reason)
			}
		case workers.PhaseFailed:
			if strings.TrimSpace(opts.reason) != "" {
				s.BlockedReason = strings.TrimSpace(opts.reason)
			}
			if opts.exitSet {
				ec := opts.exit
				s.Exit = &ec
			}
		default:
			// Non-terminal phases: clear terminal metadata so a
			// retry doesn't carry over the previous attempt's
			// completion markers. Operator can still pass the
			// flags explicitly to override (e.g., setting pr_url
			// on review-codex when the PR was opened earlier).
			s.PRURL = ""
			s.BlockedReason = ""
			s.Exit = nil
			if strings.TrimSpace(opts.prURL) != "" {
				s.PRURL = strings.TrimSpace(opts.prURL)
			}
			if strings.TrimSpace(opts.reason) != "" {
				s.BlockedReason = strings.TrimSpace(opts.reason)
			}
			if opts.exitSet {
				ec := opts.exit
				s.Exit = &ec
			}
		}
	})
	if updateErr != nil {
		return fmt.Errorf("workers update: %w", updateErr)
	}
	// Re-read the persisted state so the success line reports the
	// actual pid we wrote (the file's existing pid when --pid was
	// omitted, or opts.pid when explicit).
	persisted, _ := workers.ReadState(project, slug)
	persistedPID := 0
	if persisted != nil {
		persistedPID = persisted.PID
	}
	_, _ = fmt.Fprintf(stdout, "worker %s phase=%s pid=%d\n", slug, phase, persistedPID)
	return nil
}
