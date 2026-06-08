//go:build !linux && !darwin

// coord_lease_other.go — non-linux/darwin stub for the `fleet coord-run`
// lease wiring (DESIGN-handoff-drain-storm-leak PR2). The lease primitive
// (internal/coordlock/lease.go) and the STONITH primitive
// (internal/coord/kill.go) are build-tagged to linux||darwin (they need
// platform pid-start / monotonic-clock reads), so on other Unix targets
// (e.g. FreeBSD) those symbols don't exist. This stub keeps cmd/fleet
// building there by reporting the lease as UNSUPPORTED — runCoordRun then
// runs the legacy bare-child path, exactly as if FLEET_LEASE_FAILOVER were
// off (codex PR2 iter-2 [P2]).
package main

import (
	"errors"
	"io"
)

// errLeaseUnsupported is the sentinel defaultAcquireLease returns on a
// platform without the lease primitive. leaseDisabledOrUnsupported maps it
// to the legacy-path branch in runCoordRun.
var errLeaseUnsupported = errors.New(
	"coord-run: coordinator lease failover is unsupported on this platform " +
		"(linux/darwin only); running coord without a lease supervisor")

// defaultAcquireLease always reports "unsupported" on non-linux/darwin.
func defaultAcquireLease(_ coordRunOpts, _ io.Writer) func() (coordLease, bool, error) {
	return func() (coordLease, bool, error) {
		return nil, false, errLeaseUnsupported
	}
}

// leaseDisabledOrUnsupported is true for the unsupported sentinel so
// runCoordRun falls through to the legacy bare-child path.
func leaseDisabledOrUnsupported(err error) bool {
	return errors.Is(err, errLeaseUnsupported)
}

// coordLeaderCheck always reports "no leader" on platforms without the
// lease primitive — the lease never runs here, so spawn never wraps a
// coord-run and never consults this.
func coordLeaderCheck(string) bool { return false }

// leaseFailoverEnabled is the non-linux/darwin stub. The lease primitive is
// unsupported here (coordlock is build-tagged linux||darwin), so failover is
// always OFF regardless of FLEET_LEASE_FAILOVER — `fleet lease-check` is a
// no-op success and the legacy bare-child path always runs. The
// linux||darwin definition lives in coord_lease_unix.go (delegates to
// coordlock.FailoverEnabled).
func leaseFailoverEnabled() bool { return false }
