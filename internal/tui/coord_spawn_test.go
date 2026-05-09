// Tests for the coord-spawn indicator (issue #86). State-derivation
// tests live alongside formatter / spinner / project-row integration
// tests so the feature is verified end-to-end in one file.
package tui

import (
	"os"
	"strings"
	"testing"
	"time"
)

// TestCoordSpawnState_NoMarkerNoCoordState_ReturnsIdle pins the
// no-op branch: when no marker is on disk and no coord-state.json
// has ever been written, the indicator stays idle so the existing
// "no coord" rendering wins.
func TestCoordSpawnState_NoMarkerNoCoordState_ReturnsIdle(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	got := deriveCoordSpawnState(
		false, time.Time{},
		false, time.Time{},
		now,
		5*time.Minute, 10*time.Minute,
	)
	if got != coordSpawnIdle {
		t.Errorf("got %v, want coordSpawnIdle", got)
	}
}

// TestCoordSpawnState_MarkerExistsNoCoordState_ReturnsSpawning pins
// the cold-start path: marker is fresh (just written by [a]
// dispatch), coord-state.json hasn't been created yet → render the
// spawning spinner.
func TestCoordSpawnState_MarkerExistsNoCoordState_ReturnsSpawning(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	markerMtime := now.Add(-30 * time.Second)
	got := deriveCoordSpawnState(
		true, markerMtime,
		false, time.Time{},
		now,
		5*time.Minute, 10*time.Minute,
	)
	if got != coordSpawnSpawning {
		t.Errorf("got %v, want coordSpawnSpawning", got)
	}
}

// TestCoordSpawnState_MarkerStaleCoordStateMissing_ReturnsSpawning:
// even if coord-state.json exists but its mtime is older than
// activeWindow (and the marker still claims a spawn is in flight),
// the indicator should still read as Spawning. Equivalent to "the
// previous coord left a stale state file, this fresh [a] press is
// the one we're tracking."
func TestCoordSpawnState_MarkerStaleCoordStateMissing_ReturnsSpawning(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	markerMtime := now.Add(-1 * time.Minute) // fresh
	staleStateMtime := now.Add(-30 * time.Minute)
	got := deriveCoordSpawnState(
		true, markerMtime,
		true, staleStateMtime,
		now,
		5*time.Minute, 10*time.Minute,
	)
	if got != coordSpawnSpawning {
		t.Errorf("got %v, want coordSpawnSpawning (stale coord-state shouldn't read as Active)", got)
	}
}

// TestCoordSpawnState_MarkerFreshCoordStateFresh_ReturnsActive: the
// happy steady-state. Marker exists from a recent [a] press AND the
// coord skill has booted + published a fresh coord-state.json. The
// existing PR #57 coord-on-left rendering wins; our extra line is
// suppressed.
func TestCoordSpawnState_MarkerFreshCoordStateFresh_ReturnsActive(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	markerMtime := now.Add(-2 * time.Minute)
	stateMtime := now.Add(-15 * time.Second)
	got := deriveCoordSpawnState(
		true, markerMtime,
		true, stateMtime,
		now,
		5*time.Minute, 10*time.Minute,
	)
	if got != coordSpawnActive {
		t.Errorf("got %v, want coordSpawnActive", got)
	}
}

// TestCoordSpawnState_MarkerOlderThanSpawnTimeout_ReturnsStuck: when
// the marker has aged past spawnTimeout we declare the spawn stuck
// regardless of coord-state freshness — by minute 11+ the operator
// needs to triage via tmux, not stare at a spinner.
func TestCoordSpawnState_MarkerOlderThanSpawnTimeout_ReturnsStuck(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	markerMtime := now.Add(-15 * time.Minute) // past 10m timeout
	got := deriveCoordSpawnState(
		true, markerMtime,
		false, time.Time{},
		now,
		5*time.Minute, 10*time.Minute,
	)
	if got != coordSpawnStuck {
		t.Errorf("got %v, want coordSpawnStuck", got)
	}
}

// TestCoordSpawnState_StuckOverridesActive: a coord-state.json that
// happens to be fresh when the marker is past timeout doesn't rescue
// the row from "stuck". The marker is the in-flight signal for the
// operator's most recent [a] press; if it never converged on time
// the warning fires.
func TestCoordSpawnState_StuckOverridesActive(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	markerMtime := now.Add(-12 * time.Minute) // past timeout
	stateMtime := now.Add(-15 * time.Second)  // fresh
	got := deriveCoordSpawnState(
		true, markerMtime,
		true, stateMtime,
		now,
		5*time.Minute, 10*time.Minute,
	)
	if got != coordSpawnStuck {
		t.Errorf("stuck must override active; got %v", got)
	}
}

// TestFormatSpawnElapsed_HumanReadable pins the elapsed-time strings
// for the spawning line. Spec calls out "1m 23s" / "15s" / "3m"
// shapes — so we test sub-minute, minute-with-seconds, and round-
// hour. Negative durations clamp to "0s" rather than rendering "-3s".
func TestFormatSpawnElapsed_HumanReadable(t *testing.T) {
	cases := []struct {
		name string
		d    time.Duration
		want string
	}{
		{"zero", 0, "0s"},
		{"sub-second clamps to 0s", 500 * time.Millisecond, "0s"},
		{"15s", 15 * time.Second, "15s"},
		{"59s", 59 * time.Second, "59s"},
		{"exactly 1m → 1m 0s", time.Minute, "1m 0s"},
		{"1m 23s", time.Minute + 23*time.Second, "1m 23s"},
		{"3m 0s", 3 * time.Minute, "3m 0s"},
		{"59m 59s", 59*time.Minute + 59*time.Second, "59m 59s"},
		{"exactly 1h → 1h 0m", time.Hour, "1h 0m"},
		{"1h 4m", time.Hour + 4*time.Minute, "1h 4m"},
		{"negative clamps to 0s", -5 * time.Second, "0s"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := formatSpawnElapsed(c.d); got != c.want {
				t.Errorf("formatSpawnElapsed(%v) = %q, want %q", c.d, got, c.want)
			}
		})
	}
}

// TestSpinnerGlyphRotatesAcrossTicks pins the rotation contract: the
// spinner advances exactly one frame per tick, and wraps cleanly at
// the end of the cycle so a long-running spawn doesn't crash. We
// also assert all 10 glyphs appear in the cycle (no empty / repeated
// frames in the table).
func TestSpinnerGlyphRotatesAcrossTicks(t *testing.T) {
	if len(coordSpawnGlyphs) != 10 {
		t.Fatalf("coordSpawnGlyphs cycle length = %d, want 10 (braille spec)",
			len(coordSpawnGlyphs))
	}
	// Frames 0..9 must all be distinct.
	seen := map[rune]int{}
	for i := 0; i < 10; i++ {
		seen[coordSpawnSpinnerGlyph(i)]++
	}
	if len(seen) != 10 {
		t.Errorf("frames 0..9 must produce 10 distinct glyphs; got %d distinct (%v)",
			len(seen), seen)
	}
	// Frame 10 wraps to frame 0 without panicking.
	if coordSpawnSpinnerGlyph(10) != coordSpawnSpinnerGlyph(0) {
		t.Errorf("frame 10 must wrap to frame 0; got %q vs %q",
			string(coordSpawnSpinnerGlyph(10)), string(coordSpawnSpinnerGlyph(0)))
	}
	// Negative frames clamp to frame 0 (defensive).
	if coordSpawnSpinnerGlyph(-1) != coordSpawnSpinnerGlyph(0) {
		t.Errorf("negative frame must clamp to 0; got %q",
			string(coordSpawnSpinnerGlyph(-1)))
	}
}

// TestProjectRow_RendersSpawningLineWithSpinner: end-to-end render
// path. With a fresh marker on disk and no coord-state.json, the
// project block must include a line containing both the spinner
// glyph (frame 0 = ⠋) and the literal "spawning coord..." text.
func TestProjectRow_RendersSpawningLineWithSpinner(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	markerMtime := now.Add(-30 * time.Second)
	stubMarkerMtime(t, "demo", markerMtime, true)

	p := &ProjectRow{Name: "demo", RepoSlug: "demo"}
	ctx := coordSpawnCtx{now: now, tickFrame: 0, spawnTimeout: 10 * time.Minute}
	lines := projectBlockLines(p, 60, false, ctx)

	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, string(coordSpawnGlyphs[0])) {
		t.Errorf("rendered block missing frame-0 glyph %q\nlines:\n%s",
			string(coordSpawnGlyphs[0]), joined)
	}
	if !strings.Contains(joined, "spawning coord...") {
		t.Errorf("rendered block missing 'spawning coord...' text\nlines:\n%s", joined)
	}
}

// TestProjectRow_RendersElapsedTimeHumanReadable: same render path,
// asserts the elapsed-time string format for a 1m 23s window.
func TestProjectRow_RendersElapsedTimeHumanReadable(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	markerMtime := now.Add(-(1*time.Minute + 23*time.Second))
	stubMarkerMtime(t, "demo", markerMtime, true)

	p := &ProjectRow{Name: "demo", RepoSlug: "demo"}
	ctx := coordSpawnCtx{now: now, tickFrame: 0, spawnTimeout: 10 * time.Minute}
	lines := projectBlockLines(p, 60, false, ctx)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "1m 23s") {
		t.Errorf("expected '1m 23s' elapsed in render; got:\n%s", joined)
	}
}

// TestProjectRow_HidesSpawningOnceCoordStateActive: when LastTick is
// fresh (within coordActiveWindow), p.Active is true and the existing
// coord-on-left renderer wins — no spawning line should appear, even
// if the marker is also on disk.
func TestProjectRow_HidesSpawningOnceCoordStateActive(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	markerMtime := now.Add(-2 * time.Minute)
	stubMarkerMtime(t, "demo", markerMtime, true)

	// Fresh coord-state.json → p.Active=true, p.LastTick=stateMtime,
	// p.CoordID=non-empty. The existing renderer takes line 4 with
	// the coord ID; our spawning line must not render.
	stateMtime := now.Add(-15 * time.Second)
	p := &ProjectRow{
		Name:     "demo",
		RepoSlug: "demo",
		Active:   true,
		LastTick: stateMtime,
		CoordID:  "c00bf001",
	}
	ctx := coordSpawnCtx{now: now, tickFrame: 0, spawnTimeout: 10 * time.Minute}
	lines := projectBlockLines(p, 60, false, ctx)
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, "spawning coord...") {
		t.Errorf("Active project must not render spawning line; got:\n%s", joined)
	}
	if !strings.Contains(joined, "coord ") || !strings.Contains(joined, "c00bf001") {
		t.Errorf("Active project must still render coord-id line; got:\n%s", joined)
	}
}

// TestProjectRow_RendersStuckWarningAfterTimeout: marker mtime older
// than spawnTimeout → the spawning line flips to a red warning
// pointing at the tmux session naming convention.
func TestProjectRow_RendersStuckWarningAfterTimeout(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	markerMtime := now.Add(-15 * time.Minute) // past 10m timeout
	stubMarkerMtime(t, "demo", markerMtime, true)

	p := &ProjectRow{Name: "demo", RepoSlug: "demo"}
	ctx := coordSpawnCtx{now: now, tickFrame: 0, spawnTimeout: 10 * time.Minute}
	lines := projectBlockLines(p, 80, false, ctx)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "coord spawn stuck") {
		t.Errorf("expected stuck warning; got:\n%s", joined)
	}
	if !strings.Contains(joined, "fleet-demo") {
		t.Errorf("stuck warning must name the tmux session 'fleet-demo'; got:\n%s", joined)
	}
	if strings.Contains(joined, "spawning coord...") {
		t.Errorf("stuck row must not also render spawning line; got:\n%s", joined)
	}
}

// TestProjectRow_NoMarkerNoSpawningLine: the existing pre-#86
// rendering must be exactly preserved when no marker is on disk.
// Guards against accidental "always show spawning line" regressions.
func TestProjectRow_NoMarkerNoSpawningLine(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	stubMarkerMtime(t, "demo", time.Time{}, false) // no marker

	p := &ProjectRow{Name: "demo", RepoSlug: "demo"}
	ctx := coordSpawnCtx{now: now, tickFrame: 0, spawnTimeout: 10 * time.Minute}
	lines := projectBlockLines(p, 60, false, ctx)
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, "spawning coord...") || strings.Contains(joined, "coord spawn stuck") {
		t.Errorf("no marker → no spawning/stuck line; got:\n%s", joined)
	}
	// The block should still render the header / repo / counts trio
	// (3 content lines + a trailing blank = 4 entries).
	if len(lines) != 4 {
		t.Errorf("no-marker block must be 4 lines (3 content + blank); got %d:\n%s",
			len(lines), joined)
	}
}

// TestResolveCoordSpawnTimeout_DefaultAndOverride pins env-var
// parsing for FLEET_COORD_SPAWN_TIMEOUT_S. Default fires when unset
// or unparseable; valid integers convert to seconds.
func TestResolveCoordSpawnTimeout_DefaultAndOverride(t *testing.T) {
	cases := []struct {
		name string
		env  string
		set  bool
		want time.Duration
	}{
		{"unset → default", "", false, coordSpawnTimeoutDefault},
		{"empty → default", "", true, coordSpawnTimeoutDefault},
		{"whitespace → default", "   ", true, coordSpawnTimeoutDefault},
		{"non-numeric → default", "abc", true, coordSpawnTimeoutDefault},
		{"zero → default (positive only)", "0", true, coordSpawnTimeoutDefault},
		{"negative → default", "-30", true, coordSpawnTimeoutDefault},
		{"60 → 60s", "60", true, 60 * time.Second},
		{"600 → 10m (matches default coincidentally)", "600", true, 600 * time.Second},
		{"1800 → 30m", "1800", true, 30 * time.Minute},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.set {
				t.Setenv(coordSpawnTimeoutEnv, c.env)
			} else {
				// Defensive: explicitly unset so a leaked env from CI
				// doesn't pollute the default-path assertion.
				_ = os.Unsetenv(coordSpawnTimeoutEnv)
			}
			if got := resolveCoordSpawnTimeout(); got != c.want {
				t.Errorf("resolveCoordSpawnTimeout(%q) = %v, want %v", c.env, got, c.want)
			}
		})
	}
}

// stubMarkerMtime swaps coordSpawnMarkerMtimeFn for a deterministic
// stub during the test. Caller passes the project name + the mtime
// to return + whether the marker is "present" (ok=true). Restored
// at test end via t.Cleanup.
//
// We don't need to also stub coordSpawnMarkerFn (the agent-ID
// reader) because projectBlockLines's spawning-indicator branch
// only consults the mtime — the marker contents are irrelevant for
// the indicator (the [a] handler / unifiedProjects already consume
// them for the boot-window task_id fallback).
func stubMarkerMtime(t *testing.T, project string, mtime time.Time, ok bool) {
	t.Helper()
	prev := coordSpawnMarkerMtimeFn
	coordSpawnMarkerMtimeFn = func(name string) (time.Time, bool) {
		if name != project {
			return time.Time{}, false
		}
		return mtime, ok
	}
	t.Cleanup(func() { coordSpawnMarkerMtimeFn = prev })
}
