//go:build !linux && !darwin

// drain_lease_other.go — non-linux/darwin stub for the lease-aware drain
// (DESIGN-handoff-drain-storm-leak PR3). The lease + STONITH primitives are
// build-tagged to linux||darwin, so on other Unix targets the lease drain
// path is unsupported: leaseDrainEnabled always reports false and drainOne
// runs the legacy path. drainOneLeaseAware is a refusing stub kept only so
// the package compiles (it is never reached because leaseDrainEnabled is
// false here).

package main

import (
	"errors"
	"io"

	"github.com/edisonshen/fleet/internal/queue"
)

// leaseDrainEnabled is always false on platforms without the lease primitive.
func leaseDrainEnabled() bool { return false }

// drainOneLeaseAware is unreachable here (leaseDrainEnabled() is false) but
// must exist for drainOne to compile.
func drainOneLeaseAware(_ queue.SpawnFresh, _ string, _, _ int, _, _ io.Writer) error {
	return errors.New("fleet drain: lease-aware drain is unsupported on this platform (linux/darwin only)")
}
