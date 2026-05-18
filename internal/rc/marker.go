// Package rc owns the per-project `claude remote-control` listener
// lifecycle. It replaces the implicit-spawn architecture (PR #157)
// with an operator-managed marker + state file that every fleet
// attach/spawn surface consults via Enabled(project).
//
// The flat marker file `~/.fleet/projects/<p>/rc-enabled` matches the
// coord-spawn-marker convention at internal/state/state.go:528-566:
// a zero-byte sentinel that means "operator opted in for this project".
// The rich state lives in rc-state.json (see state.go).
//
// See docs/DESIGN-rc-listener-lifecycle.md for the full spec.
package rc

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/edisonshen/fleet/internal/state"
)

// MarkerFilename is the basename of the per-project rc-enabled
// marker file. Path-only callers (sweeper, tests) reference this
// constant rather than hard-coding the literal string.
const MarkerFilename = "rc-enabled"

// MarkerPath returns
// ~/.fleet/projects/<safe-name>/rc-enabled.
//
// Mirrors state.CoordSpawnMarkerPath shape. Validation matches
// state.ValidateProjectName via state.ProjectDir.
func MarkerPath(project string) (string, error) {
	dir, err := state.ProjectDir(project)
	if err != nil {
		return "", fmt.Errorf("rc.MarkerPath: %w", err)
	}
	return filepath.Join(filepath.Clean(dir), MarkerFilename), nil
}

// WriteMarker atomically creates the zero-byte rc-enabled marker for
// project. Idempotent — re-publishing an empty file is a no-op for
// readers (presence is the signal).
//
// Caller is responsible for any preceding state.EnsureProjectInitialized
// MkdirAll on the project tree. Writes via state.WriteAtomic for crash
// safety.
func WriteMarker(project string) error {
	path, err := MarkerPath(project)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("rc.WriteMarker: mkdir parent: %w", err)
	}
	return state.WriteAtomic(path, nil)
}

// RemoveMarker removes the rc-enabled marker for project. Idempotent:
// returns nil if the marker is already absent.
func RemoveMarker(project string) error {
	path, err := MarkerPath(project)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("rc.RemoveMarker: %w", err)
	}
	return nil
}

// MarkerPresent returns true iff MarkerPath(project) exists on disk.
// Best-effort: any error (including ENOENT) collapses to false; the
// marker is a gate, not a security boundary.
func MarkerPresent(project string) bool {
	path, err := MarkerPath(project)
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}
