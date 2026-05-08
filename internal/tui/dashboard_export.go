package tui

// Test-only helpers exported so cross-package integration tests
// (cmd/fleet/fleet_e2e_test.go) can drive the same render path the
// interactive bubbletea loop uses, without leaking lipgloss style
// types or the Model itself.
//
// These are NOT part of the stable API. Production code must not
// import them; their names carry the "ForTest" suffix to make abuse
// obvious. The tui package's own dashboard_test.go calls scanDashboard
// + Model.View() directly — these wrappers exist because cmd/fleet
// (package main) can't reach unexported symbols.

import "time"

// RenderDashboardForTest assembles the dashboard snapshot from
// FLEET_HOME on disk and renders it once at the given (width, height)
// terminal size. Returns the rendered string and the underlying
// snapshot so callers can assert on both the user-visible output and
// the structured data.
//
// Caller is responsible for $FLEET_HOME / $HOME being pointed at a
// sandboxed temp dir before invocation.
func RenderDashboardForTest(now time.Time, width, height int, version string) (string, *Snapshot) {
	m := New(version)
	m.width = width
	m.height = height
	snap := scanDashboard(now)
	m.dashboard = snap
	return m.View(), snap
}
