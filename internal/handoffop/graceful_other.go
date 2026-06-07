//go:build !linux && !darwin

// graceful_other.go — non-linux/darwin stub for the graceful handoff. The
// lease primitive (coordlock.CurrentEpoch / BarrierPath) is build-tagged to
// linux||darwin, so on other Unix targets (e.g. FreeBSD) the whole lease
// failover path is unsupported. GracefulHandoff here refuses with a typed
// error rather than silently no-opping (surface-don't-silo). Production
// builds run on linux/darwin where graceful_unix.go provides the real
// implementation; this file only keeps the package compiling everywhere.

package handoffop

import (
	"errors"
	"io"

	"github.com/edisonshen/fleet/internal/agent"
)

// ErrGracefulHandoffUnsupported is returned by GracefulHandoff on a
// platform without the lease primitive.
var ErrGracefulHandoffUnsupported = errors.New(
	"handoffop.GracefulHandoff: warm-standby graceful handoff is unsupported on this platform " +
		"(requires the linux/darwin lease primitive)")

// GracefulHandoffInputs mirrors the linux/darwin definition so the package
// API is identical across platforms.
type GracefulHandoffInputs struct {
	OldRec         *agent.Record
	HandoffDocPath string
	HandoffDoc     []byte
	CheckpointPath string
	Checkpoint     []byte
}

// GracefulHandoffDeps mirrors the linux/darwin definition.
type GracefulHandoffDeps struct {
	SpawnStandby  func() error
	ReapStandby   func() error
	WriteAtomic   func(path string, data []byte) error
	DrainInFlight func() error
	CurrentEpoch  func(project string) (int64, bool)
	BarrierPath   func(project string, epoch int64) (string, error)
	Stderr        io.Writer
}

// DefaultGracefulHandoffDeps returns empty deps on unsupported platforms.
func DefaultGracefulHandoffDeps() GracefulHandoffDeps { return GracefulHandoffDeps{} }

// GracefulHandoff refuses on unsupported platforms.
func GracefulHandoff(GracefulHandoffInputs, GracefulHandoffDeps) error {
	return ErrGracefulHandoffUnsupported
}
