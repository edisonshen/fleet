package main

// lease_check.go — `fleet lease-check --project <p>` (DESIGN-handoff-drain-
// storm-leak PR4). The executable form of the skill-side ownership proof:
// the coordinator/fleet-guard Python tick shells out to this BEFORE any
// disk mutation, so a fenced/stale coord's skill-side writes are REFUSED in
// Go (the same boundary the *WithLease APIs enforce), not merely discouraged
// by advisory Python back-off.
//
// Exit codes (the skill branches on these):
//
//	0  — the calling process descends from the live ACTIVE lease owner.
//	     The mutation may proceed. (Also exit 0 when FLEET_LEASE_FAILOVER=0:
//	     no lease in play, behave exactly as pre-lease — reversibility.)
//	3  — NOT the lease owner (fenced/stale/no-leader). The skill aborts the
//	     mutation and the coord self-demotes.
//	1  — usage / internal error (the skill treats this conservatively as
//	     "cannot prove ownership" and also refuses).
//
// The coordlock calls live in lease_check_unix.go / lease_check_other.go
// (build-tagged) because the lease primitive is linux||darwin only.

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
)

// leaseCheckNotOwnerExit is the dedicated exit code the skill maps to
// "refuse this mutation". Distinct from 1 (usage/internal) so the skill can
// tell a definitive fence from an inconclusive error.
const leaseCheckNotOwnerExit = 3

func newLeaseCheckCmd() *cobra.Command {
	var project string
	var pid int
	cmd := &cobra.Command{
		Use:   "lease-check --project <project>",
		Short: "Prove the caller descends from the active coordinator lease owner",
		Long: "Exit 0 if the calling process tree descends from the live active " +
			"coordinator lease owner for --project; exit 3 if it does not " +
			"(fenced/stale coord); exit 1 on usage/internal error. With " +
			"FLEET_LEASE_FAILOVER=0 the check is a no-op success.",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runLeaseCheck(project, pid, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project whose lease to check (required)")
	cmd.Flags().IntVar(&pid, "pid", 0, "process to prove ownership for (default: this process's parent)")
	return cmd
}

// runLeaseCheck performs the ownership proof. It returns a normal error for
// usage faults (cobra maps to exit 1); for the definitive "not owner"
// outcome it calls os.Exit(leaseCheckNotOwnerExit) directly so the skill
// gets a distinct, scriptable signal.
func runLeaseCheck(project string, pid int, stdout, stderr io.Writer) error {
	if project == "" {
		return fmt.Errorf("lease-check: --project is required")
	}
	// Reversibility: flag explicitly off -> no lease -> no-op success.
	if !leaseFailoverEnabled() {
		_, _ = fmt.Fprintln(stdout, "lease-check: failover disabled; no lease to check (ok)")
		return nil
	}
	// Default to the CALLER'S PARENT — the skill runs `fleet lease-check` as
	// a child, so the supervisor it must prove ownership for is the skill's
	// own parent (this fleet process's parent). An explicit --pid overrides
	// for tests / non-standard invocations.
	target := pid
	if target == 0 {
		target = os.Getppid()
	}
	outcome, err := leaseCheckOwnership(project, target)
	switch outcome {
	case leaseCheckOK:
		_, _ = fmt.Fprintf(stdout, "lease-check: ok (pid=%d under active lease owner for %q)\n", target, project)
		return nil
	case leaseCheckNotOwner:
		_, _ = fmt.Fprintf(stderr, "lease-check: REFUSE: %v\n", err)
		os.Exit(leaseCheckNotOwnerExit)
		return nil // unreachable
	default: // leaseCheckError
		return fmt.Errorf("lease-check: %w", err)
	}
}

// leaseCheckOutcome is the platform-agnostic verdict the build-tagged
// leaseCheckOwnership returns.
type leaseCheckOutcome int

const (
	leaseCheckOK leaseCheckOutcome = iota
	leaseCheckNotOwner
	leaseCheckError
)
