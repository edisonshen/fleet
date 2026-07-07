// Package rc owns fleet's remote-control surface.
//
// v0.14 native model (operator directive 2026-05-29): remote control
// is DEFAULT-ON and native. Every coord spawn bakes
// `--remote-control "fleet-coord-<id>-<project>"` into the coord's
// own claude argv — RC is live the moment the coord process starts.
// There is NO standalone `claude remote-control` listener daemon and
// NO send-keys injection of the /remote-control slash command.
//
// The gate is opt-OUT: the flat marker file
// `~/.fleet/projects/<p>/rc-disabled` (written by `fleet rc down`,
// removed by `fleet rc up`) suppresses the flag for a project.
// Enabled(project) is the single source of truth every attach surface
// consults.
//
// The legacy v0.12 opt-in marker (`rc-enabled`) and the rc-state.json
// listener-ownership record are retained ONLY as cleanup targets:
// Down/Reset/SweepAllProjects reap leftover standalone daemons from
// pre-v0.14 installs; nothing spawns new ones.
//
// History: docs/DESIGN-rc-listener-lifecycle.md (v0.12 opt-in
// listener architecture) and docs/DESIGN-rc-coord-auto-marker.md
// (v0.12.1 coord auto-opt-in) — both superseded by this native model.
package rc

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/edisonshen/fleet/internal/state"
)

// MarkerFilename is the basename of the LEGACY per-project rc-enabled
// opt-in marker (v0.12/v0.13). The native model no longer reads it as
// a gate; it survives only as a cleanup target (Down/Reset/sweep
// remove it). Path-only callers (sweeper, tests) reference this
// constant rather than hard-coding the literal string.
const MarkerFilename = "rc-enabled"

// DisabledMarkerFilename is the basename of the per-project opt-OUT
// marker. Presence means "operator disabled remote control for this
// project" — coord spawns skip the --remote-control flag. Written by
// `fleet rc down`, removed by `fleet rc up`.
const DisabledMarkerFilename = "rc-disabled"

// MarkerPath returns
// ~/.fleet/projects/<safe-name>/rc-enabled.
//
// Uses the same per-project state.ProjectDir layout; validation matches
// state.ValidateProjectName via state.ProjectDir.
func MarkerPath(project string) (string, error) {
	dir, err := state.ProjectDir(project)
	if err != nil {
		return "", fmt.Errorf("rc.MarkerPath: %w", err)
	}
	return filepath.Join(filepath.Clean(dir), MarkerFilename), nil
}

// DisabledMarkerPath returns
// ~/.fleet/projects/<safe-name>/rc-disabled.
func DisabledMarkerPath(project string) (string, error) {
	dir, err := state.ProjectDir(project)
	if err != nil {
		return "", fmt.Errorf("rc.DisabledMarkerPath: %w", err)
	}
	return filepath.Join(filepath.Clean(dir), DisabledMarkerFilename), nil
}

// WriteDisabledMarker atomically creates the zero-byte rc-disabled
// opt-out marker for project. Idempotent.
func WriteDisabledMarker(project string) error {
	path, err := DisabledMarkerPath(project)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("rc.WriteDisabledMarker: mkdir parent: %w", err)
	}
	return state.WriteAtomic(path, nil)
}

// RemoveDisabledMarker removes the rc-disabled marker for project.
// Idempotent: returns nil if the marker is already absent.
func RemoveDisabledMarker(project string) error {
	path, err := DisabledMarkerPath(project)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("rc.RemoveDisabledMarker: %w", err)
	}
	return nil
}

// DisabledMarkerPresent returns true iff DisabledMarkerPath(project)
// exists on disk. Best-effort: any error (including ENOENT) collapses
// to false — an unreadable marker fails OPEN (RC stays enabled, the
// default), matching the marker-is-a-gate-not-a-security-boundary
// posture of MarkerPresent.
func DisabledMarkerPresent(project string) bool {
	path, err := DisabledMarkerPath(project)
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

// WriteMarker atomically creates the zero-byte LEGACY rc-enabled
// marker for project. Idempotent — re-publishing an empty file is a
// no-op for readers (presence is the signal).
//
// Native model: nothing in production writes this anymore; it is kept
// for tests that seed legacy state and exercise the cleanup paths.
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

// RemoveMarker removes the LEGACY rc-enabled marker for project.
// Idempotent: returns nil if the marker is already absent. Cleanup
// surface only (Down/Reset/sweep).
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

// MarkerPresent returns true iff the LEGACY rc-enabled marker exists
// on disk. No longer a gate — cleanup/observability surface only.
// Best-effort: any error (including ENOENT) collapses to false.
func MarkerPresent(project string) bool {
	path, err := MarkerPath(project)
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}
