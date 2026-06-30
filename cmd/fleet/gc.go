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
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/edisonshen/fleet/internal/fleetlog"
	"github.com/edisonshen/fleet/internal/gc"
	"github.com/edisonshen/fleet/internal/state"
)

// gcDefaultMaxAge is the floor for socket / generic age gates when the
// operator omits --max-age. 24h matches the design doc default and the
// operator's hand-coded janitor sweeps observed during fleet#165 triage.
const gcDefaultMaxAge = 24 * time.Hour

type gcFlags struct {
	apply        bool
	aggressive   bool
	maxAge       time.Duration
	kindsCSV     string
	project      string
	legacyDrains bool
}

func newGCCmd() *cobra.Command {
	f := &gcFlags{maxAge: gcDefaultMaxAge}
	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Reap orphan fleet-owned resources (sockets, agent records, tmux, worktrees)",
		Long: `gc one-shot reaper for fleet-created leftovers (fleet#165, #172, #177, leak-rc-daemon-lifecycle). Eight kinds:

  sockets        — /tmp/fleet-test-*.sock files older than --max-age
  orphan-agents  — ~/.fleet/agents/*.json records whose tmux session is gone
  orphan-tmux    — fleet-<id> tmux sessions with no live agent record
                   (SURFACE only by default; --aggressive opts into kill).
                   ALSO reaps live fleet-<id> tmux SERVERS bound to a
                   /tmp/fleet-test-*.sock left behind when the test fleet
                   went quiescent (the 2026-05-29 hand-kill leak): surfaced
                   by default, killed under --apply --aggressive
                   (tmux -S <sock> kill-server). Reaped ONLY when no
                   go-test process is running anywhere on the host — if any
                   test is in flight, every test-socket server is spared.
  worktrees      — ~/.fleet/projects/*/worktrees/<slug> trees whose task
                   is done or abandoned
  coord-locks    — ~/.fleet/projects/<p>/.locks/coordinator.lock files
                   whose holder agent (a) loads with a DIFFERENT project
                   than the lock dir (mismatch), (b) has no record file
                   on disk (dead coord), or (c) names a tmux session
                   that's gone (stale). SURFACE only by default; --apply
                   unlinks the offending lock file (fleet#172)
  worker-records — ~/.fleet/projects/<p>/workers/<slug>/state.json files
                   whose worker is stale: (a) state.project disagrees
                   with parent-dir project (mismatch), (b) pid set but
                   process gone + 24h+ stale (dead-pid), (c) pid=0 +
                   24h+ stale (stale-heartbeat), (d) tasks.md says
                   done|abandoned but dir survives (task-terminal).
                   SURFACE only by default; --apply removes the worker
                   dir (state.json + output.log + scratch). Worktree
                   is NEVER touched — KindWorktrees owns that (fleet#177)
  invalid-projects — ~/.fleet/projects/<name>/ dirs whose name fails
                   validation (e.g. a "--project" dir from a CLI flag-
                   misparse) AND that hold no tasks.md. SURFACE only by
                   default; --apply rm -rf's the dir. A malformed name
                   WITH a tasks.md is surfaced but never auto-removed
                   (invalid-project-dir-guar-d636)
  orphan-rc-daemons — live claude remote-control daemons (matched by
                   --remote-control-session-name-prefix fleet-coord) whose
                   PID is in no project's rc-state.json, OR whose claude
                   version is superseded (the 2026-05-29 OOM root cause —
                   5 stale daemons across 4 versions ate ~471 MB).
                   SURFACE only by default; --apply sends SIGTERM with
                   SIGKILL escalation. PR-B of leak-rc-daemon-lifecycle
                   (DESIGN-lifecycle-leak-recurrence.md)
  drain-procs    — leaked/wedged fleet-drain processes (the 81-proc
                   leak). Keys off ~/.fleet/drain-runs/<pid>.json: a
                   drain whose heartbeat is stale past the TTL AND whose
                   pid+pid_start is still live is WEDGED. SURFACE only by
                   default; --apply reaps via a guarded kill (pid_start +
                   exe re-validated before signal) then deletes the stale
                   record. Add --legacy-drains for the one-time coarse
                   ps sweep of the existing 81 that predate the
                   run-record (sleeping + age>=5m) (handoff-drain-storm-leak)

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
  summary: N sockets, M agents, K tmux (surface only), L worktrees, P coord-locks, Q worker-records, R invalid-projects, S orphan-rc-daemons, T drain-procs

Exit codes:
  0  — sweep ran (always; per-action failures surface in stderr lines)
  1  — sweep failed before any classifier ran (CLI parse / dep wiring)`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			finish := fleetlog.CLIStart(fleetlog.Fields{}, "gc")
			err := runGC(cmd.OutOrStdout(), cmd.ErrOrStderr(), f)
			finish(err)
			return err
		},
	}
	cmd.Flags().BoolVar(&f.apply, "apply", false, "actually mutate (default: dry-run)")
	cmd.Flags().BoolVar(&f.aggressive, "aggressive", false,
		"opt into auto-killing orphan tmux sessions (per surface-don't-silo, default surfaces only)")
	cmd.Flags().DurationVar(&f.maxAge, "max-age", gcDefaultMaxAge,
		"age floor for socket sweep (Go duration; default 24h)")
	cmd.Flags().StringVar(&f.kindsCSV, "kinds", "",
		"comma-separated kinds to consider (sockets,orphan-agents,orphan-tmux,worktrees,coord-locks,worker-records,invalid-projects,orphan-rc-daemons,drain-procs); empty = all")
	cmd.Flags().StringVar(&f.project, "project", "",
		"scope worktree + agent enumeration to one project (default: all projects)")
	cmd.Flags().BoolVar(&f.legacyDrains, "legacy-drains", false,
		"one-time coarse `ps` sweep for leaked `fleet drain` procs predating the run-record (sleeping + age>=5m); implies the drain-procs kind")
	return cmd
}

// runGC parses the flag struct into gc.Options, calls Reconcile with
// the production Deps, and renders the Report. Stdout = the per-action
// list + summary; stderr = any classifier-level errors that didn't
// abort the whole sweep.
//
// Multi-socket safety warnings (codex iter-4 [P1] / codex review
// iter-2 [P2]): when FLEET_TMUX_SOCKET is set AND --apply AND the
// affected kind is in the active list, print a stderr warning. Both
// orphan-agents and coord-locks probe SessionAlive on the CURRENT
// socket; agents spawned against a different socket may look orphan
// (orphan-agents path) or stale (coord-locks stale-tmux path). The
// coord-locks split-brain defense (live-holder refusal) only triggers
// when the probe returns alive=true — a cross-socket probe returns
// alive=false and the lock gets unlinked. Same load-bearing constraint
// the agent.Record schema would need to fix; out of scope here.
// See reconcileOrphanAgents docstring + reconcileCoordLocks stale-tmux
// branch for the full constraint discussion.
func runGC(stdout, stderr io.Writer, f *gcFlags) error {
	kinds, err := parseKindsCSV(f.kindsCSV)
	if err != nil {
		return err
	}
	// --legacy-drains implies the drain-procs kind: if the operator
	// passed an explicit --kinds list that omitted it, fold it in so the
	// one-time sweep actually runs (a no-op kind that silently does
	// nothing would violate surface-don't-silo).
	if f.legacyDrains && !hasKind(kinds, gc.KindDrainProcs) {
		kinds = append(kinds, gc.KindDrainProcs)
	}
	if f.apply && hasKind(kinds, gc.KindOrphanAgents) {
		if sock := strings.TrimSpace(os.Getenv("FLEET_TMUX_SOCKET")); sock != "" {
			_, _ = fmt.Fprintf(stderr,
				"warning: FLEET_TMUX_SOCKET=%s is set; orphan-agents probes use THIS socket.\n"+
					"  Agent records do not persist their spawn-time socket. If any live agent\n"+
					"  was spawned against a different FLEET_TMUX_SOCKET, --apply may archive it\n"+
					"  as orphan. Re-run with FLEET_TMUX_SOCKET unset (default) OR drop\n"+
					"  --kinds=orphan-agents from this sweep to skip the agent-archive pass.\n",
				sock)
		}
	}
	if f.apply && hasKind(kinds, gc.KindCoordLocks) {
		if sock := strings.TrimSpace(os.Getenv("FLEET_TMUX_SOCKET")); sock != "" {
			_, _ = fmt.Fprintf(stderr,
				"warning: FLEET_TMUX_SOCKET=%s is set; coord-locks SessionAlive probes use THIS socket.\n"+
					"  agent.Record does not persist its spawn-time socket. A healthy coord on a\n"+
					"  different socket can look stale-tmux (or dead-coord with no record) and the\n"+
					"  unlink path may remove its live lock. Re-run with FLEET_TMUX_SOCKET unset\n"+
					"  (default) OR drop --kinds=coord-locks from this sweep.\n",
				sock)
		}
	}
	opts := gc.Options{
		Apply:        f.apply,
		Aggressive:   f.aggressive,
		MaxAge:       f.maxAge,
		Kinds:        kinds,
		Project:      f.project,
		LegacyDrains: f.legacyDrains,
	}
	// DESIGN-coord-worktree-lifecycle § Concurrency: hold the project-
	// scoped worktree-GC flock across the whole scan → re-check → remove
	// window so a coord-invoked run (every-N-ticks) and an operator-
	// invoked run can't race the same removal. Only relevant when the
	// worktrees kind is enabled AND a project is scoped — a host-wide
	// sweep (no --project) is the operator's per-project reclaim and the
	// coord always passes --project (coord-scope strict).
	//
	// Two failure modes, two handlings (the lock is now bounded, not
	// blocking-forever):
	//   - ErrLockTimeout → another GC is provably mid-scan. SKIP the
	//     worktrees kind AND its dependent worker-records kind (drop both
	//     from opts.Kinds) so this run does NOT scan/re-check/remove
	//     worktrees unlocked — that would reintroduce the race the lock
	//     prevents. worker-records is dropped too because reconcileWorker-
	//     Records relies on the worktrees pass populating dirtyParkedInRun
	//     (in-run suppression of recovery-context worker dirs for
	//     dirty-parked trees); without that pass it could remove a worker
	//     record the OTHER, running GC is dirty-parking. Other classifiers
	//     (sockets, orphan-agents/tmux, coord-locks, invalid-projects) are
	//     independent and still run.
	//   - any other (open/path) fault → genuine error; surface a warning
	//     and proceed (the worktrees pass is best-effort, the rest must run).
	if hasKind(kinds, gc.KindWorktrees) && strings.TrimSpace(f.project) != "" {
		release, lerr := state.LockProjectWorktreeGC(f.project)
		switch {
		case lerr == nil:
			defer release()
		case errors.Is(lerr, state.ErrLockTimeout):
			_, _ = fmt.Fprintf(stderr,
				"warning: worktree-gc lock for %q held by another GC: %v "+
					"(skipping worktrees + worker-records kinds this run)\n",
				f.project, lerr)
			kinds = dropKind(kinds, gc.KindWorktrees)
			kinds = dropKind(kinds, gc.KindWorkerRecords)
			opts.Kinds = kinds
		default:
			_, _ = fmt.Fprintf(stderr,
				"warning: could not take worktree-gc lock for %q: %v (proceeding unlocked)\n",
				f.project, lerr)
		}
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

// hasKind is a local helper to check Kind membership in the parsed
// list. Mirrors the same one in internal/gc but kept local to avoid
// exporting it from the gc package just for this one call site.
func hasKind(ks []gc.Kind, target gc.Kind) bool {
	for _, k := range ks {
		if k == target {
			return true
		}
	}
	return false
}

// dropKind returns ks without target, preserving order. Used to skip the
// worktrees classifier when its serialization lock is held by another GC
// (running it unlocked would reintroduce the removal race).
func dropKind(ks []gc.Kind, target gc.Kind) []gc.Kind {
	out := ks[:0:0] // new backing array; never alias the caller's slice
	for _, k := range ks {
		if k != target {
			out = append(out, k)
		}
	}
	return out
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
		case gc.KindSockets, gc.KindOrphanAgents, gc.KindOrphanTmux, gc.KindWorktrees, gc.KindCoordLocks, gc.KindWorkerRecords, gc.KindInvalidProjects, gc.KindOrphanRCDaemons, gc.KindDrainProcs:
			out = append(out, k)
		default:
			return nil, fmt.Errorf("unknown --kinds value %q (allowed: sockets, orphan-agents, orphan-tmux, worktrees, coord-locks, worker-records, invalid-projects, orphan-rc-daemons, drain-procs)", p)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("--kinds parsed empty list (allowed: sockets, orphan-agents, orphan-tmux, worktrees, coord-locks, worker-records, invalid-projects, orphan-rc-daemons, drain-procs)")
	}
	return out, nil
}

// renderReport prints each action one-per-line followed by a single
// summary line. Format pinned by the design doc:
//
//	<kind>  <target>  verb=<v>  reason=<r>
//	summary: N sockets, M agents, K tmux (surface only by default), L worktrees, P coord-locks, Q worker-records
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

	var nSockets, nAgents, nTmux, nWorktrees, nCoordLocks, nWorkerRecords, nInvalidProjects, nRCDaemons, nDrainProcs int
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
		case gc.KindCoordLocks:
			nCoordLocks++
		case gc.KindWorkerRecords:
			nWorkerRecords++
		case gc.KindInvalidProjects:
			nInvalidProjects++
		case gc.KindOrphanRCDaemons:
			nRCDaemons++
		case gc.KindDrainProcs:
			nDrainProcs++
		}
	}
	_, _ = fmt.Fprintf(stdout,
		"summary: %d sockets, %d agents, %d tmux (surface only by default), %d worktrees, %d coord-locks, %d worker-records, %d invalid-projects, %d orphan-rc-daemons, %d drain-procs\n",
		nSockets, nAgents, nTmux, nWorktrees, nCoordLocks, nWorkerRecords, nInvalidProjects, nRCDaemons, nDrainProcs)
}
