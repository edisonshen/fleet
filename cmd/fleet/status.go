package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/gc"
	"github.com/edisonshen/fleet/internal/rc"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/version"
)

// rcSweepFn is the package-level seam for rc.SweepAllProjects. status
// invokes it to reap stale-version + dead-owner RC daemons (the
// 2026-05-29 OOM root cause). Tests swap it for a no-op + counter.
// leak-rc-daemon-lifecycle PR-B: SweepAllProjects had zero production
// callers before this seam.
var rcSweepFn = rc.SweepAllProjects

// statusReconcileFn is the package-level seam for the auto-reconcile
// pass that runs at the end of runStatus. Production wraps
// gc.Reconcile(opts, gc.DefaultDeps()); tests override the var to
// inject canned reports without touching the real /tmp/ + ~/.fleet/.
//
// See fleet#165 PR-D in docs/DESIGN-cleanup-fleet-owns-resources.md.
// Status is informational — never mutates state (Apply=false hard-
// wired by the runStatus caller), never blocks on a reconcile error
// (logs to stderr + continues), and surfaces orphans under an
// explicit operator-action section so the operator can copy-paste
// the cleanup command.
var statusReconcileFn = func(opts gc.Options) (gc.Report, error) {
	return gc.Reconcile(opts, gc.DefaultDeps())
}

// statusReconcileMaxAge gates the socket-age classifier inside the
// reconcile pass. Mirrors gcDefaultMaxAge (cmd/fleet/gc.go) so the
// status surface and the operator-invoked `fleet gc` see consistent
// "what is orphan" definitions.
const statusReconcileMaxAge = 24 * time.Hour

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
			return runStatus(opts, cmd.OutOrStdout(), cmd.ErrOrStderr(), Version)
		},
	}
	cmd.Flags().BoolVar(&opts.jsonOut, "json", false,
		"emit raw agent records as a JSON array")
	return cmd
}

// statusEnsureFreshFn is overridable so tests don't pay for HTTP
// calls. Production runs version.EnsureFresh which performs a
// timeout-bounded synchronous check on the operator's machine.
var statusEnsureFreshFn = version.EnsureFresh

// statusEnsureFreshTimeout caps the synchronous cache refresh from
// the CLI path. 2s is a reasonable budget — `fleet status` is
// already waiting for stdio + agent.List, an extra 2s on the
// online-but-slow path is invisible. Offline users hit dial-refused
// in ~milliseconds.
const statusEnsureFreshTimeout = 2 * time.Second

// sessionCapWarnThresholdPct is the fraction of FLEET_MAX_SESSIONS
// above which `fleet status` surfaces a stderr banner pointing at
// the prune command. 0.80 (80%) gives the operator a chance to clean
// up before the cap actually blocks the next spawn.
const sessionCapWarnThresholdPct = 80

func runStatus(opts *statusOpts, stdout, stderr io.Writer, current string) error {
	// Refresh the version cache before printing — short-lived CLI
	// can't rely on a goroutine the way the TUI does (the binary
	// exits before the goroutine finishes). Skipped when the cache
	// is already fresh, so back-to-back `fleet status` invocations
	// don't fan out HTTP calls.
	statusEnsureFreshFn(current, statusEnsureFreshTimeout)

	records, err := agent.List()
	if err != nil {
		return err
	}
	// Sort by spawned_at descending — newest first reads better when
	// triaging "what's running right now."
	sort.Slice(records, func(i, j int) bool {
		return records[i].SpawnedAt.After(records[j].SpawnedAt)
	})

	// RC daemon sweep (leak-rc-daemon-lifecycle PR-B). Reaps stale-
	// version + dead-owner daemons across all projects. Mutates only
	// fleet's own resources (kills the orphan PID, removes the
	// per-project rc-state.json) — matches the fleet-owns-its-resources
	// rule. Runs on BOTH the table and --json paths (codex P2: the JSON
	// branch returns early, so placing the sweep here is the only way it
	// fires for machine-readable callers like dashboards / coord ticks).
	// Surface-don't-silo: errors log to stderr — stdout stays valid JSON.
	if err := rcSweepFn(); err != nil {
		_, _ = fmt.Fprintf(stderr, "warning: rc sweep failed: %v (continuing)\n", err)
	}

	if opts.jsonOut {
		if records == nil {
			records = []*agent.Record{}
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		// Session-cap banner runs on the JSON path too (codex iter-3
		// P2). It writes to stderr — `fleet status --json | jq`
		// reads only stdout, so the banner doesn't corrupt the
		// JSON stream while still surfacing the cap warning to
		// monitoring jobs and scripted callers watching stderr.
		emitSessionCapBanner(stderr)
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

	// Session-cap warning banner. Surfaces when the count of fleet-*
	// tmux sessions on the active server is at or above 80% of
	// FLEET_MAX_SESSIONS. Written to stderr (not stdout) so scripted
	// `fleet status | jq` and `fleet status | grep` consumers
	// reading stdout aren't broken by the banner.
	emitSessionCapBanner(stderr)

	// Auto-reconcile pass (fleet#165 PR-D). Dry-run only — status is
	// informational and must NEVER mutate state. Errors log to stderr
	// + continue; a probe failure shouldn't stop the operator from
	// seeing the agent table (surface-don't-silo, but the surface for
	// `fleet status` is a non-fatal stderr line, not a non-zero exit).
	emitOrphanReconcileSection(stdout, stderr)

	// Upgrade nudge footer. Pure read against ~/.fleet/version_check.json;
	// silent when no cache, no upgrade, or dev build. Same source of
	// truth as the TUI banner — single chip, identical format.
	if nudge := version.Nudge(current); nudge != "" {
		_, _ = fmt.Fprintln(stdout, nudge)
	}
	return nil
}

// statusAgentListFn is the production seam used by
// emitOrphanReconcileSection to enrich orphan-agents rows with the
// record's TaskID/Project so coord records (TaskID prefix coord-)
// get the recovery-safe hint instead of the destructive global
// archive command. Production wraps agent.List; tests override to
// inject canned records without touching ~/.fleet/agents.
var statusAgentListFn = func() ([]*agent.Record, error) { return agent.List() }

// emitOrphanReconcileSection runs the dry-run reconcile pass at the
// end of `fleet status` and renders any orphans under an explicit
// "Orphans (operator action):" section with a per-action cleanup
// one-liner. Empty report → section omitted (no noise on healthy
// state). Reconcile errors → logged to stderr, section still emitted
// for whatever actions DID come back (gc.Reconcile returns partial
// results on per-classifier errors).
//
// Layout — one line per orphan, columns are <kind> <target> <cmd>:
//
//	Orphans (operator action):
//	  orphan-agents   abcd1234       fleet gc --apply --kinds=orphan-agents
//	  orphan-agents   coorddead      fleet rm coorddead  (preserve for recovery!)
//	  orphan-tmux     fleet-deadbef  tmux kill-session -t fleet-deadbef
//	  sockets         /tmp/...sock   fleet gc --apply --kinds=sockets
//	  worktrees       /path/to/wt    fleet gc --apply --kinds=worktrees
//
// Coord records get a recovery-safe `fleet rm <id>` hint instead of
// the global archive command (codex review PR-D iter-4 [P2] —
// mirrors the dispatch-side surfaceOrphanAgentsFromReport guard;
// without this, an operator following the status hint would archive
// the coord record and break dead-coord recovery for that project).
//
// gc.Reconcile already returns r.Actions in a stable order (kind →
// target), so no extra sorting needed here.
func emitOrphanReconcileSection(stdout, stderr io.Writer) {
	opts := gc.Options{
		Apply:  false,
		MaxAge: statusReconcileMaxAge,
		Kinds:  dropKind(gc.AllKinds, gc.KindOrphanKicked),
	}
	report, err := statusReconcileFn(opts)
	if err != nil {
		// Surface (don't silo) but keep going — the operator still
		// gets whatever actions the reconcile DID classify.
		_, _ = fmt.Fprintf(stderr, "warning: reconcile probe failed: %v (status output still informational)\n", err)
	}
	if len(report.Actions) == 0 {
		return
	}
	// Best-effort record enrichment for orphan-agents rows; failure
	// is non-fatal (bare ID + generic hint fallback).
	byID := map[string]*agent.Record{}
	for _, a := range report.Actions {
		if a.Kind == gc.KindOrphanAgents {
			records, lerr := statusAgentListFn()
			if lerr == nil {
				for _, r := range records {
					if r == nil {
						continue
					}
					byID[r.ID] = r
				}
			}
			break // lookup only once, regardless of how many rows
		}
	}
	_, _ = fmt.Fprintln(stdout, "")
	_, _ = fmt.Fprintln(stdout, "Orphans (operator action):")
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	for _, a := range report.Actions {
		_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\n", a.Kind, a.Target, orphanCleanupHint(a, byID[a.Target]))
	}
	_ = tw.Flush()
}

// isCoordRecord is the package-local mirror of dispatch.go's
// shouldSkipArchiveForRecovery — same invariant ("TaskID has prefix
// coord-"), local for status.go so the rendering path can decide the
// hint shape without a cross-file dependency. Kept in sync by the
// shared CoordTaskIDPrefix constant.
func isCoordRecord(r *agent.Record) bool {
	if r == nil {
		return false
	}
	return strings.HasPrefix(r.TaskID, CoordTaskIDPrefix)
}

// orphanCleanupHint returns the copy-paste cleanup command for one
// orphan action. The hints are stable strings — pinned by tests so a
// careless edit can't silently break operator muscle memory.
//
//	sockets                       → fleet gc --apply --kinds=sockets
//	orphan-agents (worker record) → fleet rm <id>  (per-record; safer
//	                                than global gc when coord records
//	                                share the orphan list)
//	orphan-agents (coord record)  → fleet rm <id>  (recovery-safe; do NOT
//	                                run fleet gc --apply --kinds=orphan-agents)
//	orphan-tmux                   → tmux kill-session -t <name>  (surface, NOT auto-kill)
//	worktrees                     → fleet gc --apply --kinds=worktrees
//
// orphan-tmux is the surface-don't-silo case: the cleanup hint names
// the manual tmux command rather than `fleet gc --aggressive` so the
// operator has to read + confirm before killing a session that might
// belong to a different operator's workflow (feedback_user_owns_tmux_config).
//
// orphan-agents uses per-record `fleet rm <id>` for BOTH worker and
// coord rows (codex review PR-D iter-6 [P2]). Earlier iterations
// suggested the global `fleet gc --apply --kinds=orphan-agents` for
// worker rows + a per-record `fleet rm` + "do NOT global gc" for
// coord rows, but a mixed orphan list (one worker + one coord) made
// the worker hint a footgun: an operator following the bulk command
// at the worker row would also reap the coord record the adjacent
// row was preserving. Per-record on both rows is consistent and the
// only safe shape until `fleet gc --apply --kinds=orphan-agents` is
// made coord-record-safe (internal/gc change, out of PR-D scope).
//
// Multi-socket caveat preserved (codex review PR-D iter-5 [P2]):
// agent.Record does not persist its spawn-time tmux socket. Running
// the hint from a shell whose FLEET_TMUX_SOCKET differs from the
// agent's spawn socket would mis-archive a live agent. `fleet rm`
// is operator-scoped (specific ID + reviewed identity), so the
// blast radius is bounded — but the per-record hint still mentions
// the check so the operator confirms identity before running.
func orphanCleanupHint(a gc.Action, rec *agent.Record) string {
	// A classifier-supplied hint wins over the per-kind default synthesis —
	// e.g. a file-less orphan tmux daemon whose Target is a socket PATH needs
	// `kill <pid>`, not a session kill (codex iter-2 [P2]).
	if a.CleanupHint != "" {
		return a.CleanupHint
	}
	switch a.Kind {
	case gc.KindOrphanTmux:
		// Codex review PR-D iter-7 [P2]: include -S $FLEET_TMUX_SOCKET
		// in the kill-session hint when the env var is set. The
		// reconcile probe found the orphan session via fleet's tmux
		// helpers which always pass -S; the copied `tmux kill-session
		// -t <name>` would talk to the default server instead and
		// either no-op or kill a same-named session on the wrong
		// server.
		if sock := strings.TrimSpace(os.Getenv("FLEET_TMUX_SOCKET")); sock != "" {
			return fmt.Sprintf("tmux -S %s kill-session -t %s", sock, a.Target)
		}
		return fmt.Sprintf("tmux kill-session -t %s", a.Target)
	case gc.KindOrphanAgents:
		if isCoordRecord(rec) {
			return fmt.Sprintf("fleet rm %s  (preserve for dead-coord recovery; do NOT run `fleet gc --apply --kinds=orphan-agents`)", a.Target)
		}
		return fmt.Sprintf("fleet rm %s  (per-record; verify FLEET_TMUX_SOCKET matches agent's spawn socket before running)", a.Target)
	case gc.KindSockets, gc.KindWorktrees, gc.KindCoordLocks, gc.KindWorkerRecords, gc.KindInvalidProjects, gc.KindOrphanRCDaemons, gc.KindDrainProcs, gc.KindOrphanKicked:
		// coord-locks + worker-records + invalid-projects + orphan-rc-daemons
		// + drain-procs + orphan-kicked share the global-gc hint shape
		// (sockets + worktrees). orphan-kicked is normally excluded from
		// status reconciliation, but keep the renderer exhaustive:
		// the action is reaped by `fleet gc --apply --kinds=<kind>` with no
		// per-record FLEET_TMUX_SOCKET caveat. See cmd/fleet/gc.go's --kinds wiring
		// (fleet#172 coord-locks, fleet#177 worker-records,
		// invalid-project-dir-guar-d636 invalid-projects, leak-rc-daemon-
		// lifecycle PR-B orphan-rc-daemons, handoff-drain-storm-leak
		// drain-procs — add --legacy-drains for the one-time reclaim of the
		// pre-run-record 81).
		return fmt.Sprintf("fleet gc --apply --kinds=%s", a.Kind)
	default:
		return "(unknown — run `fleet gc` for details)"
	}
}

// statusSessionListFn / statusSessionExistsFn are overridable for
// tests. Production wires to tmux.ListSessions + state.LiveAgentRecordExists.
// nil values fall through to production wiring at first use.
var (
	statusSessionListFn   func() ([]string, error)
	statusSessionExistsFn func(id string) bool
)

// emitSessionCapBanner prints a one-line stderr warning when the
// active tmux session count is at or above the configured warning
// threshold (80% of FLEET_MAX_SESSIONS). Silent below the threshold,
// silent on probe failure (don't spam operators on every `fleet
// status` when tmux is briefly unreachable).
//
// Two visual tiers:
//
//   - 80% <= count < 100% — yellow/amber tone (warning).
//   - count >= 100% — red tone (cap has been or will be hit on the
//     next spawn). Uses ANSI red so it stands out from the warning
//     tier; falls back to plain text when stderr isn't a terminal.
//
// stderr may be nil — callers like the integration smoke harness can
// pass nil to suppress the banner entirely.
func emitSessionCapBanner(stderr io.Writer) {
	if stderr == nil {
		return
	}
	listFn := statusSessionListFn
	if listFn == nil {
		listFn = productionSessionListFn()
	}
	existsFn := statusSessionExistsFn
	if existsFn == nil {
		existsFn = productionSessionExistsFn()
	}
	counts, err := state.CountFleetSessions(listFn, existsFn)
	if err != nil {
		// Silent on probe failure — the operator already paid the
		// cost of an enumeration failure once at startup if it
		// matters; spamming `fleet status` runs is worse.
		return
	}
	max := state.MaxSessions(stderr)
	total := counts.Total()
	// Compare `total*100 >= max*pct` instead of `total >= (max*pct)/100`
	// to keep the threshold faithful for small caps (codex review
	// iter-1 P2). Integer division floors: with max=1 and pct=80,
	// `(max*pct)/100` rounds to 0 and the banner would fire at zero
	// sessions; with max=4 it'd fire at 50% (2/4) instead of the
	// advertised 80%. The cross-multiplied form preserves the
	// percentage exactly without floats.
	//
	// Also skip emit when total is zero — nothing useful to warn
	// about, even when the rounded threshold lands at 0.
	if total == 0 || total*100 < max*sessionCapWarnThresholdPct {
		return
	}
	bannerStyle := bannerStyleWarning
	if total >= max {
		bannerStyle = bannerStyleCritical
	}
	// prune-orphan-tmux is dry-run by default; banner explicitly
	// names --kill so following the printed remediation actually
	// reaps the orphan sessions (codex iter-8 P3).
	line := fmt.Sprintf(
		"WARNING: %d/%d fleet tmux sessions in use (%d live, %d orphan).\nApproaching FLEET_MAX_SESSIONS cap. Run `fleet maintenance prune-orphan-tmux --kill` to reap orphans (omit --kill for a dry-run inspection).\n",
		total, max, counts.Live, counts.Orphan)
	// ANSI coloring only when stderr is a real terminal (codex
	// iter-6 P2). When stderr is redirected to a file / piped to a
	// log aggregator / running under CI, the escape codes turn
	// plain warning text into control-coded junk that downstream
	// alerting has to strip. Plain text suffices in those cases.
	if stderrIsTerminal(stderr) {
		_, _ = fmt.Fprint(stderr, paintBanner(line, bannerStyle))
	} else {
		_, _ = fmt.Fprint(stderr, line)
	}
}

// stderrIsTerminal reports whether the given writer is an actual
// terminal (vs a redirected file / pipe). Only *os.File backed by a
// tty fd qualifies; anything else (bytes.Buffer in tests, log
// pipelines, redirected runs) returns false.
func stderrIsTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd())
}

// bannerStyle is the visual tier for the session-cap banner. Two
// values: warning (yellow/amber) when count >= 80% of max, critical
// (red) when count >= max.
type bannerStyle int

const (
	bannerStyleWarning bannerStyle = iota
	bannerStyleCritical
)

// paintBanner wraps the line in ANSI escape codes for the chosen
// visual tier. We don't pull in lipgloss for one line of stderr —
// the rest of cmd/fleet is plain-text, and bringing in a TUI styling
// library here would add weight without operator-visible value.
//
// When the terminal can't render ANSI (CI logs, redirected stderr),
// the codes are still safe — most modern terminals tolerate them and
// strict-strip filters (`grep -v $'\x1b'`) work normally. The plain
// text inside the escape pair conveys the same information.
func paintBanner(line string, style bannerStyle) string {
	const reset = "\x1b[0m"
	var prefix string
	switch style {
	case bannerStyleCritical:
		prefix = "\x1b[1;31m" // bold red
	default:
		prefix = "\x1b[1;33m" // bold yellow / amber
	}
	return prefix + line + reset
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
