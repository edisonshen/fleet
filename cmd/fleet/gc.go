package main

// `fleet gc` — operator-facing one-shot orphan-resource reaper. Thin
// CLI shell over internal/gc. Wired into the root cobra command in
// main.go's newRootCmd().
//
// Default is DRY-RUN: prints what would be removed per kind and exits
// 0. --apply actually mutates. --aggressive enables the orphan-tmux
// auto-kill escape hatch (per feedback_surface_dont_silo.md +
// feedback_user_owns_tmux_config.md, default surfaces only).
//
// See docs/DESIGN-cleanup-fleet-owns-resources.md §PR-A for the
// product spec.

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/edisonshen/fleet/internal/gc"
)

// gcDefaultMaxAge is the floor for socket / generic age gates when the
// operator omits --max-age. 24h matches the design doc default and the
// operator's hand-coded janitor sweeps observed during fleet#165 triage.
const gcDefaultMaxAge = 24 * time.Hour

type gcFlags struct {
	apply      bool
	aggressive bool
	maxAge     time.Duration
	kindsCSV   string
	project    string
}

func newGCCmd() *cobra.Command {
	f := &gcFlags{maxAge: gcDefaultMaxAge}
	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Reap orphan fleet-owned resources (sockets, agent records, tmux, worktrees)",
		Long: `gc one-shot reaper for fleet-created leftovers (fleet#165). Four kinds:

  sockets        — /tmp/fleet-test-*.sock files older than --max-age
  orphan-agents  — ~/.fleet/agents/*.json records whose tmux session is gone
  orphan-tmux    — fleet-<id> tmux sessions with no live agent record
                   (SURFACE only by default; --aggressive opts into kill)
  worktrees      — ~/.fleet/projects/*/worktrees/<slug> trees whose task
                   is done or abandoned

Default behavior is DRY-RUN — prints a planned action list and exits
0 WITHOUT mutating. Pass --apply to actually remove / archive / kill.

Flags:
  --apply              actually mutate (default: dry-run)
  --max-age <dur>      sockets-only age floor (Go duration; default 24h)
  --kinds <csv>        restrict to comma-separated kinds (default: all)
  --aggressive         allow orphan-tmux auto-kill (off-by-default
                       opt-in per feedback_surface_dont_silo +
                       feedback_user_owns_tmux_config)
  --project <name>     scope worktree + agent enumeration to one project

Per-action output format:
  <kind>  <target>  verb=<v>  reason=<r>

Trailing summary line:
  summary: N sockets, M agents, K tmux (surface only), L worktrees

Exit codes:
  0  — sweep ran (always; per-action failures surface in stderr lines)
  1  — sweep failed before any classifier ran (CLI parse / dep wiring)`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runGC(cmd.OutOrStdout(), cmd.ErrOrStderr(), f)
		},
	}
	cmd.Flags().BoolVar(&f.apply, "apply", false, "actually mutate (default: dry-run)")
	cmd.Flags().BoolVar(&f.aggressive, "aggressive", false,
		"opt into auto-killing orphan tmux sessions (per surface-don't-silo, default surfaces only)")
	cmd.Flags().DurationVar(&f.maxAge, "max-age", gcDefaultMaxAge,
		"age floor for socket sweep (Go duration; default 24h)")
	cmd.Flags().StringVar(&f.kindsCSV, "kinds", "",
		"comma-separated kinds to consider (sockets,orphan-agents,orphan-tmux,worktrees); empty = all")
	cmd.Flags().StringVar(&f.project, "project", "",
		"scope worktree + agent enumeration to one project (default: all projects)")
	return cmd
}

// runGC parses the flag struct into gc.Options, calls Reconcile with
// the production Deps, and renders the Report. Stdout = the per-action
// list + summary; stderr = any classifier-level errors that didn't
// abort the whole sweep.
func runGC(stdout, stderr io.Writer, f *gcFlags) error {
	kinds, err := parseKindsCSV(f.kindsCSV)
	if err != nil {
		return err
	}
	opts := gc.Options{
		Apply:      f.apply,
		Aggressive: f.aggressive,
		MaxAge:     f.maxAge,
		Kinds:      kinds,
		Project:    f.project,
	}
	deps := gc.DefaultDeps()
	report, rerr := gc.Reconcile(opts, deps)
	// Render the report regardless of error — per-classifier failures
	// shouldn't suppress the actions that DID succeed.
	renderReport(stdout, opts, report)
	if rerr != nil {
		_, _ = fmt.Fprintf(stderr, "warning: %v (continuing)\n", rerr)
	}
	return nil
}

// parseKindsCSV converts the --kinds flag value into a gc.Kind slice.
// Empty input → gc.AllKinds. Unknown kinds → error (typos shouldn't
// silently degrade to "skip that one").
func parseKindsCSV(csv string) ([]gc.Kind, error) {
	csv = strings.TrimSpace(csv)
	if csv == "" {
		return gc.AllKinds, nil
	}
	parts := strings.Split(csv, ",")
	out := make([]gc.Kind, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		k := gc.Kind(p)
		switch k {
		case gc.KindSockets, gc.KindOrphanAgents, gc.KindOrphanTmux, gc.KindWorktrees:
			out = append(out, k)
		default:
			return nil, fmt.Errorf("unknown --kinds value %q (allowed: sockets, orphan-agents, orphan-tmux, worktrees)", p)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("--kinds parsed empty list (allowed: sockets, orphan-agents, orphan-tmux, worktrees)")
	}
	return out, nil
}

// renderReport prints each action one-per-line followed by a single
// summary line. Format pinned by the design doc:
//
//	<kind>  <target>  verb=<v>  reason=<r>
//	summary: N sockets, M agents, K tmux (surface only), L worktrees
//
// dry-run / apply distinction is conveyed by the verb (would-* vs *),
// not by a separate prefix.
func renderReport(stdout io.Writer, opts gc.Options, r gc.Report) {
	mode := "dry-run"
	if opts.Apply {
		mode = "apply"
	}
	_, _ = fmt.Fprintf(stdout, "fleet gc — mode=%s aggressive=%t max-age=%s\n",
		mode, opts.Aggressive, opts.MaxAge)

	var nSockets, nAgents, nTmux, nWorktrees int
	for _, a := range r.Actions {
		_, _ = fmt.Fprintf(stdout, "%s  %s  verb=%s  reason=%s\n",
			a.Kind, a.Target, a.Verb, a.Reason)
		switch a.Kind {
		case gc.KindSockets:
			nSockets++
		case gc.KindOrphanAgents:
			nAgents++
		case gc.KindOrphanTmux:
			nTmux++
		case gc.KindWorktrees:
			nWorktrees++
		}
	}
	_, _ = fmt.Fprintf(stdout,
		"summary: %d sockets, %d agents, %d tmux (surface only by default), %d worktrees\n",
		nSockets, nAgents, nTmux, nWorktrees)
}
