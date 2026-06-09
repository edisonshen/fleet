package main

// doctor.go — `fleet doctor [--project <p>] [--fix] [--verbose]`, the
// user-facing coordinator-recovery command (PR6 of DESIGN-handoff-drain-
// storm-leak).
//
// WHAT IT IS FOR (plain English): when Fleet's coordinator for a project
// stops responding — a handoff to a fresh coordinator that didn't complete,
// a coordinator process that's alive but stuck, leftover recovery files
// piling up — the operator runs `fleet doctor` to SEE what's wrong and
// `fleet doctor --fix` to recover it. No manual `kill`/`rm` against fleet's
// own processes and files (that is fleet's job, not the operator's).
//
// TWO HALVES, split so the user surface is testable in isolation:
//
//	doctor.go (this file, all platforms)
//	  - cobra command + flag wiring
//	  - PLAIN-ENGLISH rendering of a doctorReport (default + --fix)
//	  - the jargon (coordinator-lease / epoch / STONITH / flock / tmux /
//	    fence) appears ONLY under --verbose. The plain path is the one the
//	    operator reads.
//
//	doctor_unix.go (linux||darwin) / doctor_other.go (stub)
//	  - the real inspection (lease health via coordlock.Diagnose, pending
//	    queue files, duplicate handoff docs, wedged drain run-records, coord
//	    session liveness) and the --fix recovery (fence -> STONITH -> acquire
//	    + respawn-from-checkpoint), which depend on the linux||darwin-only
//	    lease + kill primitives. Other platforms get a stub that reports the
//	    feature is unavailable, mirroring lease_check_{unix,other}.go.

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

func newDoctorCmd() *cobra.Command {
	var project string
	var fix bool
	var verbose bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check coordinator health and recover a stuck coordinator",
		Long: "doctor inspects a project's coordinator and reports, in plain " +
			"English, whether it is healthy or stuck (an unresponsive " +
			"coordinator, a handoff that didn't finish, leftover recovery " +
			"files). With --fix it safely restarts a provably-stuck or dead " +
			"coordinator and cleans up after it; it NEVER disturbs a " +
			"coordinator that is alive and responding. --verbose adds the " +
			"engineer-level detail (lease epoch, process ids, file paths).",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDoctor(doctorOpts{project: project, fix: fix, verbose: verbose},
				cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().StringVar(&project, "project", "",
		"project whose coordinator to check (default: every project with a coordinator)")
	cmd.Flags().BoolVar(&fix, "fix", false,
		"recover a provably-stuck or dead coordinator (restart it and clean up). "+
			"A live, responding coordinator is never disturbed")
	cmd.Flags().BoolVar(&verbose, "verbose", false,
		"include engineer-level detail (process ids, lease epoch, file paths)")
	return cmd
}

type doctorOpts struct {
	project string
	fix     bool
	verbose bool
}

// doctorReport is the platform-agnostic result the build-tagged gather/fix
// functions return. doctor.go renders it; doctor_unix.go fills it. Keeping
// the rendering here (all platforms) is what lets the user-surface tests
// (T22 no-jargon) run without the unix-only lease machinery.
type doctorReport struct {
	// projects is one entry per project the doctor inspected.
	projects []doctorProjectReport
	// unsupported is set on non-linux/darwin builds: the lease + kill
	// primitives are unavailable, so there is nothing to diagnose.
	unsupported bool
}

// doctorProjectReport is the per-project diagnosis + (when --fix ran) the
// recovery outcome.
type doctorProjectReport struct {
	project string

	// --- diagnosis (always populated) ---
	// status is the single plain-English health line, e.g. "coordinator is
	// healthy" / "the coordinator is not responding".
	status doctorStatus
	// findings are the secondary plain-English observations (leftover
	// recovery files, a handoff that didn't finish, etc.), each with an
	// engineer-detail string shown only under --verbose.
	findings []doctorFinding

	// --- recovery (populated only when --fix ran) ---
	fixPlanned    bool     // --fix decided to act on this project
	fixActions    []string // the plain-English actions, surfaced as they ran
	fixRefused    string   // non-empty: --fix deliberately did NOT act, with the plain reason
	fixErr        error    // a recovery error (surfaced, never silently dropped)
	verboseDetail []string // engineer-detail lines for the recovery (--verbose only)
}

// doctorStatus is the headline health classification, decoupled from the
// lease internals so the plain renderer never names a lease state.
type doctorStatus int

const (
	doctorStatusHealthy      doctorStatus = iota // a live coordinator is responding
	doctorStatusNone                             // no coordinator for this project (nothing wrong)
	doctorStatusUnresponsive                     // alive but stuck (hung)
	doctorStatusDead                             // the coordinator process is gone
	doctorStatusHandoffStuck                     // a handoff to a fresh coordinator didn't complete
	doctorStatusNeedsConfirm                     // an ambiguous state needing operator-confirmed recovery
)

// doctorFinding is one secondary observation with a plain message + an
// engineer-detail string (shown only under --verbose).
type doctorFinding struct {
	plain   string // user-facing, NO jargon
	verbose string // engineer detail (paths, pids, epoch) — --verbose only
}

// plainStatusLine maps a status to its canonical plain-English line. These
// are the exact operator-approved phrasings (no jargon).
func (s doctorStatus) plainStatusLine() string {
	switch s {
	case doctorStatusHealthy:
		return "coordinator is healthy"
	case doctorStatusNone:
		return "no coordinator is running for this project"
	case doctorStatusUnresponsive:
		return "the coordinator is not responding"
	case doctorStatusDead:
		return "the coordinator has stopped"
	case doctorStatusHandoffStuck:
		return "Fleet isn't responding — the handoff to a fresh coordinator didn't complete"
	case doctorStatusNeedsConfirm:
		return "the coordinator is in a state that needs a confirmed recovery"
	default:
		return "coordinator status unknown"
	}
}

// needsRecovery reports whether a status describes a coordinator that --fix
// should act on (a stuck/dead/handoff-stuck/ambiguous one). A healthy or
// absent coordinator is left alone.
func (s doctorStatus) needsRecovery() bool {
	switch s {
	case doctorStatusUnresponsive, doctorStatusDead, doctorStatusHandoffStuck, doctorStatusNeedsConfirm:
		return true
	default:
		return false
	}
}

// runDoctor gathers the diagnosis (and, with --fix, runs recovery), then
// renders it in plain English (default) or with engineer detail (--verbose).
// The gather/fix work is delegated to the build-tagged doctorGatherFn /
// doctorFixFn seams so this renderer (and its no-jargon tests) builds on
// every platform.
func runDoctor(opts doctorOpts, stdout, stderr io.Writer) error {
	report, err := doctorGatherFn(opts)
	if err != nil {
		return err
	}
	if report.unsupported {
		_, _ = fmt.Fprintln(stdout,
			"fleet doctor: coordinator recovery is only available on macOS and Linux")
		return nil
	}
	if opts.fix {
		// Run recovery per project (surface-don't-silo: each action is
		// printed as it runs, inside doctorFixFn, via the writers).
		doctorFixFn(opts, &report, stdout, stderr)
	}
	renderDoctorReport(opts, report, stdout)
	// A recovery error on any project makes the command exit non-zero so a
	// scripted caller sees the failure — but the report is still rendered.
	for i := range report.projects {
		if report.projects[i].fixErr != nil {
			return report.projects[i].fixErr
		}
	}
	return nil
}

// renderDoctorReport prints the plain-English report. Engineer detail
// (paths/pids/epoch) is appended ONLY under --verbose. The jargon words
// (coordinator-lease / epoch / STONITH / flock / tmux / fence) live solely
// in the verbose strings the gatherer builds — the plain path never names
// them.
func renderDoctorReport(opts doctorOpts, report doctorReport, w io.Writer) {
	if len(report.projects) == 0 {
		_, _ = fmt.Fprintln(w, "fleet doctor: no projects with a coordinator to check")
		return
	}
	for i, p := range report.projects {
		if i > 0 {
			_, _ = fmt.Fprintln(w)
		}
		_, _ = fmt.Fprintf(w, "Project %s: %s\n", p.project, p.status.plainStatusLine())

		for _, f := range p.findings {
			_, _ = fmt.Fprintf(w, "  - %s\n", f.plain)
			if opts.verbose && f.verbose != "" {
				_, _ = fmt.Fprintf(w, "      (%s)\n", f.verbose)
			}
		}

		// Remedy hint on the read-only path: tell the operator the one
		// command that recovers it (don't silo — concrete next step).
		if !opts.fix && p.status.needsRecovery() {
			_, _ = fmt.Fprintf(w,
				"  Run `fleet doctor --project %s --fix` to recover it.\n", p.project)
		}

		// Recovery surface (--fix). Each action was already streamed by the
		// fixer as it ran; here we echo the planned/refused outcome so the
		// final report is self-contained.
		if opts.fix {
			switch {
			case p.fixRefused != "":
				_, _ = fmt.Fprintf(w, "  Did not act: %s\n", p.fixRefused)
			case p.fixErr != nil:
				_, _ = fmt.Fprintf(w, "  Recovery did not finish: %s\n", plainErr(p.fixErr))
			case p.fixPlanned:
				for _, a := range p.fixActions {
					_, _ = fmt.Fprintf(w, "  - %s\n", a)
				}
				_, _ = fmt.Fprintln(w, "  Recovery complete.")
			}
		}

		// Engineer detail (--verbose) renders on BOTH the read-only diagnosis
		// path AND the --fix path (codex PR6 iter-2 [P2]): verboseDetail holds
		// the lease state/epoch/pid the diagnosis collects PLUS any recovery
		// detail, so `fleet doctor --verbose` (no --fix) must show it too.
		if opts.verbose {
			for _, v := range p.verboseDetail {
				_, _ = fmt.Fprintf(w, "      (%s)\n", v)
			}
		}
	}
}

// plainErr renders an error in plain English for the non-verbose surface.
// It strips the wrapped jargon-laden internal message down to a generic
// recovery line; --verbose callers see the raw error via verboseDetail.
func plainErr(err error) string {
	if err == nil {
		return ""
	}
	return "the recovery hit a problem; rerun `fleet doctor --fix` or check `fleet status`"
}

// doctorGatherFn / doctorFixFn are the build-tagged seams. doctor_unix.go
// sets them to the real implementations; doctor_other.go to stubs. They are
// vars (not direct calls) so tests can inject a canned report and exercise
// the no-jargon renderer (T22) without the lease machinery.
var (
	doctorGatherFn = func(opts doctorOpts) (doctorReport, error) {
		return gatherDoctorReport(opts)
	}
	doctorFixFn = func(opts doctorOpts, report *doctorReport, stdout, stderr io.Writer) {
		doctorRunFix(opts, report, stdout, stderr)
	}
)
