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
	"os"
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
		newWorkersWorktreePathCmd(),
		newWorkersDeleteCmd(),
	)
	return cmd
}

// ---------- fleet workers delete ----------

// newWorkersDeleteCmd is the lifecycle-hygiene cleanup hook (issue
// #101). The Python coord skill calls it after the worker subagent
// returns terminal — the PR URL is already persisted on the task entry
// at that point, so the worker dir contributes no extra information
// and is rm-rf'd outright (no archive, per issue #101 "Cleanup rules").
//
// Idempotent on missing dir — repeated calls return 0 with the
// "already gone" line so the coord skill + TUI defense-in-depth path
// can both fire without tripping over each other (first mover wins;
// second sees ENOENT).
//
// Slug + path validation lives in workers.Delete; this CLI wrapper is
// a thin shell. Refusing the literal slug "archive" prevents an
// operator typo from blowing away the archive root.
func newWorkersDeleteCmd() *cobra.Command {
	var project string
	cmd := &cobra.Command{
		Use:   "delete <slug>",
		Short: "Remove ~/.fleet/projects/<project>/workers/<slug>/ (rm -rf, no archive)",
		Long: `delete removes the worker dir for one (project, slug) pair entirely.
The worker's state.json, output log, and any nested files are gone.
This is the issue #101 lifecycle cleanup path: a worker at terminal
phase (done|failed) has nothing the coordinator or operator needs to
read again — the PR URL has already been persisted on the task entry.

Idempotent: a missing dir returns 0 with the "already gone" message.

NOTE: this is NOT the same as ` + "`fleet workers prune`" + ` (which targets archived
dirs by age). Delete is the active-dir cleanup; Prune is the
archive-dir retention path (which v0.5+ doesn't populate, so prune
is mostly a v0.1 / pre-#101 hand-archive janitor).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			slug := strings.TrimSpace(args[0])
			if slug == "" {
				return errors.New("slug must be non-empty")
			}
			if _, err := state.Bootstrap(); err != nil {
				return fmt.Errorf("bootstrap: %w", err)
			}
			proj, err := resolveProject(project)
			if err != nil {
				return err
			}
			// Stat first so the success line distinguishes "removed"
			// from "already gone". workers.Delete is idempotent either
			// way; the operator-visible message is informational only.
			workerDir, derr := state.WorkerDir(proj, slug)
			pre := ""
			if derr == nil {
				if _, sErr := os.Stat(workerDir); sErr == nil {
					pre = "removed"
				}
			}
			if err := workers.Delete(proj, slug); err != nil {
				return fmt.Errorf("delete: %w", err)
			}
			if pre == "" {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "worker %s already gone\n", slug)
			} else {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "worker %s removed\n", slug)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name (default: cwd basename)")
	return cmd
}

// ---------- fleet workers worktree-path ----------

// newWorkersWorktreePathCmd prints the canonical worktree path for one
// (project, slug) pair and exits 0. It is a thin wrapper over
// state.WorktreePath; the Python skill (skills/coordinator/worktree.py)
// shells out to it so Go remains the single source of truth for project
// tree layout. Plumbing only — no mkdir, no `git worktree add`.
//
// Path-only by design: the coord skill calls this to learn WHERE the
// worktree should live, then issues `git worktree add` itself. v0.2
// keeps the create / remove primitives in Python because they have to
// be subprocess-cheap inside the tick loop; Go's job is just to vouch
// for the path.
func newWorkersWorktreePathCmd() *cobra.Command {
	var project string
	cmd := &cobra.Command{
		Use:   "worktree-path <slug>",
		Short: "Print ~/.fleet/projects/<project>/worktrees/<slug>/ for cap>1 dispatch",
		Long: `worktree-path resolves the canonical worktree directory for one
(project, slug) pair and prints it to stdout. Used by the coordinator
skill in cap > 1 mode to bootstrap parallel workers via
"git -C <repo> worktree add <path> -b worker/<slug>".

This is a path-only resolver — the directory is NOT created. The Python
caller decides whether to create or remove the worktree on disk.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			slug := strings.TrimSpace(args[0])
			if slug == "" {
				return errors.New("slug must be non-empty")
			}
			proj, err := resolveProject(project)
			if err != nil {
				return err
			}
			path, err := state.WorktreePath(proj, slug)
			if err != nil {
				return fmt.Errorf("worktree-path: %w", err)
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), path)
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name (default: cwd basename)")
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

	// Three-stage flow review fields (reviewer-subagent-arch).
	// Reviewer subagents call `fleet workers update <slug> --phase
	// review-done --review-alpha-status passed --review-alpha-engine
	// claude --review-alpha-model <model> --review-beta-status passed
	// --review-beta-engine claude --review-beta-model <model>` after
	// the two-slot review loop returns clean. Without these flags the
	// worker's review_* fields stay empty and the terminal review gate
	// rejects the finisher's phase update.
	reviewAlphaStatus        string
	reviewAlphaRounds        int
	reviewAlphaSkipReason    string
	reviewAlphaEngine        string
	reviewAlphaModel         string
	reviewBetaStatus         string
	reviewBetaRounds         int
	reviewBetaSkipReason     string
	reviewBetaEngine         string
	reviewBetaModel          string
	reviewAlphaStatusSet     bool
	reviewAlphaRoundsSet     bool
	reviewAlphaSkipReasonSet bool
	reviewAlphaEngineSet     bool
	reviewAlphaModelSet      bool
	reviewBetaStatusSet      bool
	reviewBetaRoundsSet      bool
	reviewBetaSkipReasonSet  bool
	reviewBetaEngineSet      bool
	reviewBetaModelSet       bool

	// dispatchGeneration is the coord-owned per-slug fence token
	// (DESIGN §1/§2.2) stamped into this dispatch's worker prompt. When
	// set, the update routes through workers.UpdateStateGen, a CAS that
	// rejects the write when it doesn't match the task row's
	// authoritative dispatch_generation (a stale prior attempt). When
	// the flag is absent (legacy / non-worker callers), the update keeps
	// the ungated path so pre-migration workers don't fail open.
	dispatchGeneration    int
	dispatchGenerationSet bool
}

// allowedCodexSkipReasonsCLI is the CLI-side mirror of the
// workers.allowedCodexSkipReasons set. Duplicated here so the CLI can
// reject bad input upfront with a clear message ("not in the
// allowlist"), rather than letting workers.WriteState surface the
// state-layer error via the same path the operator hits for genuine
// state file corruption. The hard guard is workers.WriteState — this
// is the friendly guard.
var allowedCodexSkipReasonsCLI = map[string]struct{}{
	"rate-limited": {},
	"unavailable":  {},
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
			opts.reviewAlphaStatusSet = cmd.Flags().Changed("review-alpha-status")
			opts.reviewAlphaRoundsSet = cmd.Flags().Changed("review-alpha-rounds")
			opts.reviewAlphaSkipReasonSet = cmd.Flags().Changed("review-alpha-skip-reason")
			opts.reviewAlphaEngineSet = cmd.Flags().Changed("review-alpha-engine")
			opts.reviewAlphaModelSet = cmd.Flags().Changed("review-alpha-model")
			opts.reviewBetaStatusSet = cmd.Flags().Changed("review-beta-status")
			opts.reviewBetaRoundsSet = cmd.Flags().Changed("review-beta-rounds")
			opts.reviewBetaSkipReasonSet = cmd.Flags().Changed("review-beta-skip-reason")
			opts.reviewBetaEngineSet = cmd.Flags().Changed("review-beta-engine")
			opts.reviewBetaModelSet = cmd.Flags().Changed("review-beta-model")
			opts.dispatchGenerationSet = cmd.Flags().Changed("dispatch-generation")
			return runWorkersUpdate(args[0], opts, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&opts.project, "project", "", "project name (default: cwd basename)")
	cmd.Flags().StringVar(&opts.phase, "phase", "",
		"new worker phase (starting|branch|tdd-red|tdd-green|tdd-refactor|review-claude|review-codex|review-pending|review-done|push|done|blocked|failed)")
	cmd.Flags().StringVar(&opts.prURL, "pr-url", "", "PR URL (required for phase=done)")
	cmd.Flags().StringVar(&opts.reason, "reason", "", "blocked reason (required for phase=blocked)")
	cmd.Flags().IntVar(&opts.pid, "pid", 0, "worker OS PID (default: os.Getpid())")
	cmd.Flags().IntVar(&opts.exit, "exit", 0, "worker exit code (set on phase=done|failed)")
	cmd.Flags().StringVar(&opts.reviewAlphaStatus, "review-alpha-status", "",
		"reviewer subagent: alpha slot status (pending|iterating|passed|skipped|blocked|single-claude-degraded)")
	cmd.Flags().IntVar(&opts.reviewAlphaRounds, "review-alpha-rounds", 0,
		"reviewer subagent: alpha slot review rounds (0..N)")
	cmd.Flags().StringVar(&opts.reviewAlphaSkipReason, "review-alpha-skip-reason", "",
		"reviewer subagent: alpha skip reason — required when status=skipped and engine=codex (allowlist: rate-limited, unavailable)")
	cmd.Flags().StringVar(&opts.reviewAlphaEngine, "review-alpha-engine", "",
		"reviewer subagent: alpha slot engine (codex|claude)")
	cmd.Flags().StringVar(&opts.reviewAlphaModel, "review-alpha-model", "",
		"reviewer subagent: alpha slot model")
	cmd.Flags().StringVar(&opts.reviewBetaStatus, "review-beta-status", "",
		"reviewer subagent: beta slot status (pending|iterating|passed|skipped|blocked|single-claude-degraded)")
	cmd.Flags().IntVar(&opts.reviewBetaRounds, "review-beta-rounds", 0,
		"reviewer subagent: beta slot review rounds (0..N)")
	cmd.Flags().StringVar(&opts.reviewBetaSkipReason, "review-beta-skip-reason", "",
		"reviewer subagent: beta skip reason — required when status=skipped and engine=codex (allowlist: rate-limited, unavailable)")
	cmd.Flags().StringVar(&opts.reviewBetaEngine, "review-beta-engine", "",
		"reviewer subagent: beta slot engine (codex|claude)")
	cmd.Flags().StringVar(&opts.reviewBetaModel, "review-beta-model", "",
		"reviewer subagent: beta slot model")
	cmd.Flags().IntVar(&opts.dispatchGeneration, "dispatch-generation", 0,
		"coord-owned per-slug fence token (DESIGN §2.2). When set, the update is a CAS against the task row's dispatch_generation: a stale generation is rejected. Omit on legacy/non-worker callers.")
	_ = cmd.MarkFlagRequired("phase")
	return cmd
}

// taskRowDispatchGeneration reads the AUTHORITATIVE dispatch_generation
// for slug from the project's tasks.md (DESIGN §2.2: the task row is the
// durable CAS authority, never the on-disk state alone). A slug absent
// from live tasks.md (never tracked, or archived terminal) resolves to 0
// — the legacy/untracked authority — so a gen-0 (pre-migration) writer is
// accepted and a current-gen writer against a vanished row is CAS-
// rejected + surfaced (fail-safe). A genuine read error (corrupt/too-new
// tasks.md) is propagated so the CAS fails closed rather than silently
// defaulting to 0 and accepting a stale write.
func taskRowDispatchGeneration(project, slug string) (int, error) {
	f, _, err := readTasks(project)
	if err != nil {
		return 0, fmt.Errorf("read task row for CAS authority: %w", err)
	}
	t, err := f.Get(slug)
	if err != nil {
		// Slug not present in live tasks.md → authority 0 (legacy /
		// untracked). f.Get returns a not-found error; treat it as 0,
		// NOT a hard failure (a worker can outlive its task row's
		// archival).
		return 0, nil
	}
	return t.DispatchGeneration, nil
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

	// CLI-side review status validation. Empty values pass through
	// when the flag was not set. Explicit status flags must be in the
	// shared non-empty enum set; skip legality is keyed by the slot's
	// engine value, not the alpha/beta flag name.
	alphaStatus := workers.ReviewStatus(strings.TrimSpace(opts.reviewAlphaStatus))
	betaStatus := workers.ReviewStatus(strings.TrimSpace(opts.reviewBetaStatus))
	alphaSkipReason := strings.TrimSpace(opts.reviewAlphaSkipReason)
	betaSkipReason := strings.TrimSpace(opts.reviewBetaSkipReason)
	for _, slot := range []struct {
		name          string
		status        workers.ReviewStatus
		statusRaw     string
		statusSet     bool
		engine        string
		skipReason    string
		skipReasonSet bool
	}{
		{name: "alpha", status: alphaStatus, statusRaw: opts.reviewAlphaStatus, statusSet: opts.reviewAlphaStatusSet, engine: strings.TrimSpace(opts.reviewAlphaEngine), skipReason: alphaSkipReason, skipReasonSet: opts.reviewAlphaSkipReasonSet},
		{name: "beta", status: betaStatus, statusRaw: opts.reviewBetaStatus, statusSet: opts.reviewBetaStatusSet, engine: strings.TrimSpace(opts.reviewBetaEngine), skipReason: betaSkipReason, skipReasonSet: opts.reviewBetaSkipReasonSet},
	} {
		if slot.statusSet {
			if !workers.ReviewStatusValidNonEmpty(slot.status) {
				return fmt.Errorf(
					"--review-%s-status %q: must be one of pending|iterating|passed|skipped|blocked|single-claude-degraded",
					slot.name,
					slot.statusRaw,
				)
			}
			if slot.status == workers.ReviewStatusSkipped {
				if slot.engine != workers.ReviewEngineCodex {
					return fmt.Errorf("--review-%s-status=skipped: only a codex-engine slot may be skipped", slot.name)
				}
				if _, ok := allowedCodexSkipReasonsCLI[slot.skipReason]; !ok {
					return fmt.Errorf(
						"--review-%s-status=skipped requires --review-%s-skip-reason in {rate-limited, unavailable}; got %q",
						slot.name,
						slot.name,
						slot.skipReason,
					)
				}
			}
		}
		// Setting skip reason without status=skipped is a confused call
		// — reject so the operator notices the mistake rather than
		// silently persisting a reason that has no terminal status.
		if slot.skipReasonSet && slot.status != workers.ReviewStatusSkipped {
			return fmt.Errorf("--review-%s-skip-reason set without --review-%s-status=skipped", slot.name, slot.name)
		}
	}

	mutate := func(s *workers.State) {
		// Record phase transition: append the previous phase to the
		// completed list so workers.list / peek can show "5/9 phases
		// done" without losing history. Skip the append on the very
		// first transition out of "starting" (= bootstrap default)
		// so we don't double-count it.
		if s.Phase != "" && s.Phase != phase {
			s.PhasesCompleted = append(s.PhasesCompleted, s.Phase)
		}
		s.Phase = phase
		// Re-dispatch reset: when state.json is reused for a retry
		// (CI-red worker re-dispatched on the same slug), the new
		// attempt must NOT carry the previous run's review status —
		// otherwise a re-dispatched worker that bypasses the reviewer
		// would inherit "review_alpha_status=passed" from the prior
		// attempt and the phase=push validator would let it through.
		// Phase=starting is the canonical re-dispatch entry point
		// (worker.go bootstraps fresh state with phase=starting and
		// every re-dispatch issues `fleet workers update --phase
		// starting` before anything else). Clear unconditionally on
		// this transition; the reviewer subagent re-populates the
		// fields on its run.
		if phase == workers.PhaseStarting {
			s.ReviewAlphaStatus = ""
			s.ReviewAlphaRounds = 0
			s.ReviewAlphaSkipReason = ""
			s.ReviewAlphaEngine = ""
			s.ReviewAlphaModel = ""
			s.ReviewBetaStatus = ""
			s.ReviewBetaRounds = 0
			s.ReviewBetaSkipReason = ""
			s.ReviewBetaEngine = ""
			s.ReviewBetaModel = ""
		}
		// Apply review-* flag updates. Set-flag semantics: only
		// overwrite when the operator (or reviewer subagent) passed
		// the flag explicitly. Otherwise preserve whatever's on disk
		// — review state is sticky across phase transitions (a
		// reviewer that wrote review_alpha_status=passed at
		// phase=review-done expects the finisher's phase=push
		// transition to see the same value).
		if opts.reviewAlphaStatusSet {
			s.ReviewAlphaStatus = alphaStatus
			if alphaStatus != workers.ReviewStatusSkipped {
				s.ReviewAlphaSkipReason = ""
			}
		}
		if opts.reviewAlphaRoundsSet {
			s.ReviewAlphaRounds = opts.reviewAlphaRounds
		}
		if opts.reviewAlphaSkipReasonSet {
			s.ReviewAlphaSkipReason = alphaSkipReason
		}
		if opts.reviewAlphaEngineSet {
			s.ReviewAlphaEngine = strings.TrimSpace(opts.reviewAlphaEngine)
		}
		if opts.reviewAlphaModelSet {
			s.ReviewAlphaModel = strings.TrimSpace(opts.reviewAlphaModel)
		}
		if opts.reviewBetaStatusSet {
			s.ReviewBetaStatus = betaStatus
			if betaStatus != workers.ReviewStatusSkipped {
				s.ReviewBetaSkipReason = ""
			}
		}
		if opts.reviewBetaRoundsSet {
			s.ReviewBetaRounds = opts.reviewBetaRounds
		}
		if opts.reviewBetaSkipReasonSet {
			s.ReviewBetaSkipReason = betaSkipReason
		}
		if opts.reviewBetaEngineSet {
			s.ReviewBetaEngine = strings.TrimSpace(opts.reviewBetaEngine)
		}
		if opts.reviewBetaModelSet {
			s.ReviewBetaModel = strings.TrimSpace(opts.reviewBetaModel)
		}
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
	}

	// Writer CAS (DESIGN §2.2). Read the task row's authoritative
	// dispatch_generation ONCE (under the same logical dispatch).
	//
	//   --dispatch-generation set  → route through UpdateStateGen, a CAS
	//     that rejects a stale write incl. when state.json is absent.
	//   --dispatch-generation OMITTED → allowed ONLY while the authority
	//     is 0 (a genuinely un-migrated / legacy slug). Codex iter-3 [P1]:
	//     once the slug has been (re-)dispatched under the epoch (authority
	//     > 0), an ungated UpdateState would let a stale/pre-upgrade worker
	//     load the CURRENT gen>0 state.json, mutate it, and write it back
	//     with the gen PRESERVED — so the chokepoint reader would classify
	//     the stale write `current`, defeating the fence. Reject it with a
	//     clear next step instead of failing open.
	taskGen, gerr := taskRowDispatchGeneration(project, slug)
	if gerr != nil {
		return gerr
	}
	var updateErr error
	if opts.dispatchGenerationSet {
		updateErr = workers.UpdateStateGen(project, slug, opts.dispatchGeneration, taskGen, mutate)
	} else if taskGen > 0 {
		return fmt.Errorf(
			"worker %q has dispatch_generation=%d on its task row; "+
				"`fleet workers update` must pass --dispatch-generation %d "+
				"(the ungated path is rejected once a slug is dispatched under "+
				"the epoch — a stale/legacy worker must not clobber current state)",
			slug, taskGen, taskGen,
		)
	} else {
		updateErr = workers.UpdateState(project, slug, mutate)
	}
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
