// Hidden-projects + activity-based grouping for the LEFT-column
// PROJECTS list (issue #98).
//
// Two orthogonal mechanisms combined:
//
//  1. Activity grouping (auto, no persistence) — projects with recent
//     coord ticks / agent heartbeats / worker activity render in the
//     ACTIVE group; everything else collapses into a "─── N idle ───"
//     separator. Threshold = FLEET_ACTIVE_WINDOW_DAYS env (default 7d)
//     for the agent/worker signal; coord uses the existing
//     coordActiveWindow.
//
//  2. Explicit hide list (operator-driven, persistent) — projects on
//     ~/.fleet/hidden-projects.json drop from view entirely OR render
//     dim under "─── N hidden ───" when show-hidden mode is on. Hide
//     is HARD: fresh activity does NOT auto-unhide.
//
// The two mechanisms compose: hidden wins over active/idle for filter
// purposes (a hidden project never appears in active or idle groups
// even when it has fresh activity), but the activity signal is still
// computed so the footer chip can surface "M with activity" as a nudge
// without overriding intent.
package tui

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/projects"
	"github.com/edisonshen/fleet/internal/state"
)

// projectActivity classifies a project for the LEFT-column grouping
// decision. ACTIVE means render in the top group; IDLE means collapse
// under the "─── N idle ───" separator. HIDDEN status is a separate
// axis driven by the hidden-list filter — handled in dashboardRows.
type projectActivity int

const (
	projectActive projectActivity = iota
	projectIdle
)

// activeWindowEnv is the env var override for the agent/worker
// activity threshold. Coord uses coordActiveWindow (5m) regardless;
// this controls the broader "is anyone touching this project?" signal
// and defaults to 7 days per spec.
const activeWindowEnv = "FLEET_ACTIVE_WINDOW_DAYS"

// activeWindowDefault is the spec-mandated default — 7 days, the same
// floor where the operator stops thinking of a project as ongoing.
const activeWindowDefault = 7 * 24 * time.Hour

// resolveActiveWindow returns the configured activity threshold,
// reading FLEET_ACTIVE_WINDOW_DAYS when set + non-empty + parses to a
// positive integer (interpreted as days). Falls back to
// activeWindowDefault for any failure (empty, unset, non-numeric, ≤ 0).
//
// Mirrors resolveCoordSpawnTimeout's pattern so operators have one
// mental model for env-driven duration overrides.
func resolveActiveWindow() time.Duration {
	raw := strings.TrimSpace(os.Getenv(activeWindowEnv))
	if raw == "" {
		return activeWindowDefault
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return activeWindowDefault
	}
	return time.Duration(n) * 24 * time.Hour
}

// classifyProjectActivity returns ACTIVE when ANY of:
//   - coord-state.json is fresh (within coordActiveWindow) — already
//     surfaced via ProjectRow.Active from scanProject.
//   - coordinator.lock mtime within coordActiveWindow.
//   - any agent record tagged with the project has LastActivityTS
//     within the active window.
//   - any worker state.json under workers/<slug>/ has UpdatedAt within
//     the active window — surfaced via dashboard.Workers.
//   - the project's meta.json AddedAt is within the active window —
//     covers a freshly-registered project (via `fleet project add` or
//     the TUI's [+] hotkey) where no operator activity has accumulated
//     yet. Without this, a brand-new project falls under the
//     "─── N idle ───" separator on its very first render. Legacy
//     projects pre-dating meta.json carry a zero addedAt (caller's
//     projects.Read returned ErrNotFound); the zero value is skipped so
//     the absence is NOT misread as "infinitely recent" (which would
//     flip every pre-meta project to ACTIVE).
//
// Otherwise IDLE.
//
// Pure derivation: no I/O. Caller passes the snapshot fields and
// records; the activity threshold is supplied so tests can pin it
// deterministically. addedAt is also supplied by the caller — the
// dashboard render loop reads meta.json once per project per render
// (rows.go:dashboardRows), same place coord-state / records are read
// today.
func classifyProjectActivity(
	p *ProjectRow,
	workers []*WorkerRow,
	records []*agent.Record,
	addedAt time.Time,
	now time.Time,
	window time.Duration,
) projectActivity {
	if p == nil {
		return projectIdle
	}
	// Coord active is the strongest signal — already gated on
	// coord-state.json mtime within coordActiveWindow upstream
	// (scanDashboard).
	if p.Active {
		return projectActive
	}
	// LastTick within window — covers the case where coord-state.json
	// mtime is between coordActiveWindow (5m) and our window (7d) and
	// we still want to surface the project as active because someone
	// touched the coord recently.
	if !p.LastTick.IsZero() && now.Sub(p.LastTick) <= window {
		return projectActive
	}
	// Agent heartbeats: any tagged record with a fresh LastActivityTS.
	for _, r := range records {
		if r == nil || r.Project != p.Name {
			continue
		}
		if !r.LastActivityTS.IsZero() && now.Sub(r.LastActivityTS) <= window {
			return projectActive
		}
	}
	// Worker activity: any worker tagged to this project. The
	// dashboard's Workers list already filters to active (non-archived)
	// workers — scanWorkers reads from workers/<slug>/, and the coord
	// archives finished workers, so a row in the snapshot means there's
	// an in-flight worker. WorkerRow doesn't carry UpdatedAt (only the
	// human-aged string), so we trust the snapshot's existence as the
	// freshness signal here.
	for _, w := range workers {
		if w == nil || w.Project != p.Name {
			continue
		}
		return projectActive
	}
	// AddedAt freshness — fifth signal (the [+] hotkey fix). Zero value
	// means caller couldn't read meta.json (legacy / pre-meta projects)
	// and MUST NOT be treated as recent; only an explicit non-zero ts
	// within window flips the classification.
	if !addedAt.IsZero() && now.Sub(addedAt) <= window {
		return projectActive
	}
	return projectIdle
}

// hiddenWithActivity returns the count of hidden projects that DO have
// fresh activity. Used by the footer chip to render the
// " · M with activity" nudge so operators know the hidden list isn't
// dormant without overriding the hide.
//
// allProjects is the un-filtered project list (before applying the
// hidden filter); hiddenSet is the set of names on the hidden list.
// addedAtFn resolves a project's meta.AddedAt; nil = "no caller-side
// meta knowledge" and the call short-circuits to time.Time{}. Real
// callers pass projectAddedAtFn (production reads disk) or a stub
// (tests). Optional so test sites that don't care about AddedAt
// (the existing TestHiddenWithActivity coverage) can pass nil and
// rely on the other signals.
func hiddenWithActivity(
	allProjects []*ProjectRow,
	hiddenSet map[string]bool,
	addedAtFn func(string) time.Time,
	workers []*WorkerRow,
	records []*agent.Record,
	now time.Time,
	window time.Duration,
) int {
	n := 0
	for _, p := range allProjects {
		if p == nil || !hiddenSet[p.Name] {
			continue
		}
		var addedAt time.Time
		if addedAtFn != nil {
			addedAt = addedAtFn(p.Name)
		}
		if classifyProjectActivity(p, workers, records, addedAt, now, window) == projectActive {
			n++
		}
	}
	return n
}

// projectAddedAtFn resolves a project's meta.AddedAt. Returns the zero
// time on any failure (ErrNotFound for legacy pre-meta projects,
// parse errors, or invalid project names) so the AddedAt signal
// short-circuits to "unknown" — classifyProjectActivity treats that
// as no-signal and falls through to the other rules. var so tests can
// stub the disk read; production calls projects.Read.
//
// Read is best-effort: a project whose meta.json is missing or
// malformed degrades to "no AddedAt" rather than blocking the
// dashboard render. This matches the meta.go contract — ErrNotFound is
// not an error for tolerant readers.
var projectAddedAtFn = func(name string) time.Time {
	m, err := projects.Read(name)
	if err != nil {
		return time.Time{}
	}
	return m.AddedAt
}

// readHiddenProjectsFn returns the operator's hidden-list as a slice.
// var so tests can stub the disk read without seeding a tmp file.
// Production calls state.ReadHiddenProjects.
var readHiddenProjectsFn = func() []string {
	names, err := state.ReadHiddenProjects()
	if err != nil {
		return nil
	}
	return names
}

// writeHiddenProjectsFn publishes a new hidden list. var so tests can
// stub the disk write. Production calls state.WriteHiddenProjects.
var writeHiddenProjectsFn = state.WriteHiddenProjects

// toggleHiddenProjectFn flips a project's hidden state and returns
// (nowHidden, err). var so tests can stub the disk round-trip.
// Production calls state.ToggleHiddenProject.
var toggleHiddenProjectFn = state.ToggleHiddenProject

// hiddenProjectsSet returns the operator's hidden list as a map for
// O(1) membership checks. Empty/nil on read failure (best-effort —
// never block the dashboard).
func hiddenProjectsSet() map[string]bool {
	names := readHiddenProjectsFn()
	if len(names) == 0 {
		return nil
	}
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out
}
