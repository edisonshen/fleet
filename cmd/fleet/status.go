package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/state"
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

	// Upgrade nudge footer. Pure read against ~/.fleet/version_check.json;
	// silent when no cache, no upgrade, or dev build. Same source of
	// truth as the TUI banner — single chip, identical format.
	if nudge := version.Nudge(current); nudge != "" {
		_, _ = fmt.Fprintln(stdout, nudge)
	}
	return nil
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
