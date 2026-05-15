// Tests for the coord-spawn indicator (issue #86). State-derivation
// tests live alongside formatter / spinner / project-row integration
// tests so the feature is verified end-to-end in one file.
package tui

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
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

// TestCoordSpawnState_PostActiveIdleStop_ReturnsIdle pins the
// /review iter-1 fix: when a coord previously ticked (coord-state.json
// mtime is newer than the marker) but then stopped (state mtime is
// now stale), we're in the existing IdleStop branch — NOT a cold
// start. Returning Idle prevents a contradictory "spawning..." line
// from rendering alongside the existing "○ idle · auto-stopped"
// status on the same project row.
func TestCoordSpawnState_PostActiveIdleStop_ReturnsIdle(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	// Marker was written 7 minutes ago (past activeWindow but inside
	// spawnTimeout) — the coord tied to this marker booted, ticked at
	// minute 6 (newer than marker), then died at minute 6 + change.
	markerMtime := now.Add(-7 * time.Minute)
	stateMtime := now.Add(-6 * time.Minute) // newer than marker, but stale (>5m)
	got := deriveCoordSpawnState(
		true, markerMtime,
		true, stateMtime,
		now,
		5*time.Minute, 10*time.Minute,
	)
	if got != coordSpawnIdle {
		t.Errorf("post-active idle-stop must return Idle (existing renderer wins); got %v", got)
	}
}

// TestCoordSpawnState_StuckRequiresStaleCoordState: stuck only fires
// when the marker is past timeout AND there is no liveness signal from
// coord-state.json (mtime stale or absent). A coord that is still
// actively ticking — proven by a coord-state mtime within activeWindow
// — by definition is not stuck during spawn; it successfully booted
// and continues to publish state. The "stuck" framing is reserved for
// the narrow case where the marker exists but no fresh state ever
// arrived, OR where state arrived once and then went stale alongside
// the marker aging past the timeout.
//
// The pre-fix ordering treated every long-lived alive coord as stuck
// (PART 2.5 — false-positive on coord de3e12a9 at 20:47 PDT 2026-05-13:
// claude alive 20 min, coord-state mtime 6 min ago, badge fired). The
// fix inverts the order in deriveCoordSpawnState: the active check
// runs before the spawn-timeout check.
func TestCoordSpawnState_StuckRequiresStaleCoordState(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	markerMtime := now.Add(-12 * time.Minute) // past timeout
	// Stale coord-state — no liveness signal. With the marker past
	// timeout AND no fresh state, the coord legitimately looks stuck.
	staleStateMtime := now.Add(-30 * time.Minute)
	got := deriveCoordSpawnState(
		true, markerMtime,
		true, staleStateMtime,
		now,
		5*time.Minute, 10*time.Minute,
	)
	if got != coordSpawnStuck {
		t.Errorf("marker past timeout + stale state must be stuck; got %v", got)
	}
}

// TestCoordSpawnState_FreshCoordStateBeatsStaleSpawnMarker pins the
// PART 2.5 fix: a coord that is actively publishing coord-state.json
// (mtime within activeWindow) is NOT stuck regardless of how old the
// spawn marker is. The marker is set ONCE at spawn and never refreshed;
// a coord ticking 12+ minutes after spawn is healthy, not stuck.
//
// Pre-fix: this case returned coordSpawnStuck because the timeout check
// ran before the active check. After the fix, active wins.
//
// Evidence (real bug, 2026-05-13 20:47 PDT):
//   - coord de3e12a9 spawned at 20:26 PDT (~21 min before badge fired)
//   - claude pid 10445 alive and executing tool calls
//   - coord-state.json mtime 20:41 PDT (6 min stale, but within 5m
//     when the badge fired)
//   - badge said "▲ coord spawn stuck" — false positive
func TestCoordSpawnState_FreshCoordStateBeatsStaleSpawnMarker(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	// Marker spawned 15 minutes ago — past the 10-minute spawn timeout.
	markerMtime := now.Add(-15 * time.Minute)
	// Coord-state.json was updated 1 minute ago — clearly alive.
	coordStateMtime := now.Add(-1 * time.Minute)
	got := deriveCoordSpawnState(
		true, markerMtime,
		true, coordStateMtime,
		now,
		5*time.Minute, 10*time.Minute,
	)
	if got != coordSpawnActive {
		t.Errorf("fresh coord-state must beat stale spawn marker; got %v want coordSpawnActive", got)
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
// than spawnTimeout AND its named tmux session is alive → the
// spawning line flips to a red warning pointing at the tmux session
// naming convention. Issue #96 gap 2 fix: the hint now reads
// `fleet-<agentID>` (sourced from the marker body) instead of the
// previous `fleet-<projectName>` text — the project-shaped form never
// matched a real session, so the operator was led nowhere.
//
// Stuck self-heal (gap 1) is gated on the tmux session being DEAD;
// here we stub it alive so the warning fires (the genuinely-hung-spawn
// case the operator should triage via tmux).
func TestProjectRow_RendersStuckWarningAfterTimeout(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	markerMtime := now.Add(-15 * time.Minute) // past 10m timeout
	stubMarkerMtime(t, "demo", markerMtime, true)
	stubMarkerAgentID(t, "demo", "abcd1234")
	stubAliveSessions(t, map[string]bool{"fleet-abcd1234": true})

	p := &ProjectRow{Name: "demo", RepoSlug: "demo"}
	ctx := coordSpawnCtx{now: now, tickFrame: 0, spawnTimeout: 10 * time.Minute}
	lines := projectBlockLines(p, 80, false, ctx)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "coord spawn stuck") {
		t.Errorf("expected stuck warning; got:\n%s", joined)
	}
	if !strings.Contains(joined, "fleet-abcd1234") {
		t.Errorf("stuck warning must name the tmux session 'fleet-abcd1234' from marker; got:\n%s", joined)
	}
	if strings.Contains(joined, "fleet-demo") {
		t.Errorf("stuck warning must NOT use project-shaped session name 'fleet-demo'; got:\n%s", joined)
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

// TestProjectRow_StuckDowngradesToWaitingWhenNeedsInputFresh pins
// PART 2b of the bundle + codex iter-4 freshness gate: when the
// spawn marker is past timeout AND the agent record matching the
// marker has needs_input=true AND last_activity_ts is within
// agentRecordFreshWindow, render an informational "coord waiting on
// input" line instead of the attention-chip stuck warning. The
// freshness gate is load-bearing — a stale flag from a dead claude
// (wrapper-shell survived after engine exit) must NOT trigger the
// downgrade.
//
// Note: with last_activity_ts fresh enough to pass the freshness
// gate (within agentRecordFreshWindow = 10m by default), the Path B
// self-heal in applyStuckSelfHeal ALSO fires — it transitions stuck
// → idle and removes the marker. So the realistic render here is
// "no spawn line at all" (Idle). To exercise the WAITING path
// specifically, we pin a last_activity_ts that is fresh enough for
// needs-input trust (within the 10m freshWindow) but past the
// Path B "agent record is fresh" gate is the same window — so both
// heal and waiting are eligible. Heal wins (runs first). This is
// the correct production outcome: a healthy coord asking for input
// renders Idle (with the operator-input chip), not Waiting.
//
// Real bug surfaced 2026-05-13: coord bab86984 (projects-spark)
// showed the stuck badge while its claude process pid 8306 had been
// running 12 minutes idle awaiting input — needs_input=true,
// last_activity 33s after spawn. The badge trained the operator to
// spawn replacements over a live coord.
func TestProjectRow_StuckDowngradesToWaitingWhenNeedsInputFresh(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	markerMtime := now.Add(-15 * time.Minute) // past 10m timeout
	stubMarkerMtime(t, "demo", markerMtime, true)
	stubMarkerAgentID(t, "demo", "abcd1234")
	stubAliveSessions(t, map[string]bool{"fleet-abcd1234": false})

	// last_activity_ts FRESH (within freshWindow=10m) → needs_input
	// flag is trusted. Session-alive=false so Path A of heal doesn't
	// short-circuit; Path B (record-fresh) heals to Idle BEFORE the
	// downgrade runs. Realistic production behavior — see test
	// docstring above.
	rec := &agent.Record{
		ID:             "abcd1234",
		LastActivityTS: now.Add(-3 * time.Minute), // FRESH
		NeedsInput:     true,
	}
	p := &ProjectRow{Name: "demo", RepoSlug: "demo"}
	ctx := coordSpawnCtx{
		now:          now,
		tickFrame:    0,
		spawnTimeout: 10 * time.Minute,
		records:      []*agent.Record{rec},
	}
	lines := projectBlockLines(p, 80, false, ctx)
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, "coord spawn stuck") {
		t.Errorf("fresh needs_input must NOT render stuck; got:\n%s", joined)
	}
}

// TestDowngradeStuckOnNeedsInput_UnitContract pins the new
// downgrade contract via the pure helper (no project-block plumbing
// involved). Confirms the freshness gate flips the verdict on the
// exact case codex iter-4 raised: stale needs_input flag must fall
// through to stuck, not mask a dead coord.
func TestDowngradeStuckOnNeedsInput_UnitContract(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	freshWindow := 10 * time.Minute
	cases := []struct {
		name       string
		needsInput bool
		lastTSAge  time.Duration // age from now (positive = past)
		want       coordSpawnState
	}{
		{
			name:       "needs_input=true + fresh ts → waiting",
			needsInput: true,
			lastTSAge:  3 * time.Minute,
			want:       coordSpawnWaiting,
		},
		{
			name:       "needs_input=true + stale ts → STUCK (codex iter-4 gate)",
			needsInput: true,
			lastTSAge:  30 * time.Minute, // past 10m window
			want:       coordSpawnStuck,  // dead coord with stale flag
		},
		{
			name:       "needs_input=false + fresh ts → stuck (no input signal)",
			needsInput: false,
			lastTSAge:  3 * time.Minute,
			want:       coordSpawnStuck,
		},
		{
			name:       "needs_input=true + zero ts → stuck (no signal trust)",
			needsInput: true,
			lastTSAge:  0, // sentinel for IsZero()
			want:       coordSpawnStuck,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var ts time.Time
			if tc.lastTSAge > 0 {
				ts = now.Add(-tc.lastTSAge)
			}
			rec := &agent.Record{
				ID:             "abcd1234",
				LastActivityTS: ts,
				NeedsInput:     tc.needsInput,
			}
			got := downgradeStuckOnNeedsInput(
				coordSpawnStuck,
				[]*agent.Record{rec},
				"abcd1234",
				now,
				freshWindow,
			)
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestProjectRow_StuckPreservedWhenNoAgentRecord: the inverse case —
// genuine stuck (live tmux session BUT NO agent record on disk, which
// means fleet-guard never reported in) still renders the attention-chip
// warning. Ensures the Path C heal doesn't accidentally suppress real
// failure modes.
//
// (Pre-Path-C this case was "live + stale record → real stuck" because
// Path B's freshness gate left stale records as evidence of hangs. Path
// C reclassifies "record exists" as proof of spawn success — so to keep
// the real-stuck case observable we must remove the record entirely.)
func TestProjectRow_StuckPreservedWhenNoAgentRecord(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	markerMtime := now.Add(-15 * time.Minute)
	stubMarkerMtime(t, "demo", markerMtime, true)
	stubMarkerAgentID(t, "demo", "abcd1234")
	stubAliveSessions(t, map[string]bool{"fleet-abcd1234": true})

	// No matching agent record → fleet-guard never reported in.
	// Live tmux + no record is the real-stuck case (Path C cannot heal
	// because the spawn pipeline visibly didn't complete the record
	// write — the operator should attach via tmux to triage).
	p := &ProjectRow{Name: "demo", RepoSlug: "demo"}
	ctx := coordSpawnCtx{
		now:          now,
		tickFrame:    0,
		spawnTimeout: 10 * time.Minute,
		records:      nil,
	}
	lines := projectBlockLines(p, 80, false, ctx)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "coord spawn stuck") {
		t.Errorf("no record + live session + past timeout → stuck must fire; got:\n%s", joined)
	}
	if strings.Contains(joined, "waiting on input") {
		t.Errorf("real stuck must not render waiting line; got:\n%s", joined)
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
// Production projectBlockLines now consults BOTH the mtime AND the
// agent_id body of the marker (gap 2 hint text + gap 1 self-heal
// probe). Tests that don't care about those branches can stub mtime
// alone and the agent-id reader will return its production value (""
// for a missing marker file in the per-test FLEET_HOME tmpdir, which
// is the safe default).
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

// stubMarkerAgentID swaps coordSpawnMarkerFn for a deterministic stub
// returning agentID for the matching project, "" otherwise. Used by
// gap-1 self-heal tests + gap-2 hint-text tests so the body and the
// mtime can be controlled independently. Restored at test end.
func stubMarkerAgentID(t *testing.T, project, agentID string) {
	t.Helper()
	prev := coordSpawnMarkerFn
	coordSpawnMarkerFn = func(name string) string {
		if name != project {
			return ""
		}
		return agentID
	}
	t.Cleanup(func() { coordSpawnMarkerFn = prev })
}

// stubAliveSessions swaps sessionAliveFn for a deterministic in-memory
// map: present-and-true → alive, present-and-false or absent → dead.
// Restored at test end. Mirrors the existing tmux.HasSession contract
// (definitive bool — the conflated probe-error case is irrelevant for
// the self-heal path; we err toward "treat as alive" only when the
// session was deliberately stubbed dead).
//
// Distinct from the keys_test.go helper `stubSessionAlive` (struct,
// dead-set) — this is the inverse-shape variant for self-heal tests
// where the alive-set is the natural way to describe the world.
func stubAliveSessions(t *testing.T, alive map[string]bool) {
	t.Helper()
	prev := sessionAliveFn
	sessionAliveFn = func(session string) bool {
		return alive[session]
	}
	t.Cleanup(func() { sessionAliveFn = prev })
}

// stubProcessAlive swaps agentProcessAliveFn for a deterministic
// in-memory map keyed by PID. PIDs not in the map (or with value
// false) are treated as dead. Restored at test end. Used by Path C
// tests that need to distinguish "Claude alive" from "wrapper-only"
// without taking a real OS-process dependency.
func stubProcessAlive(t *testing.T, alive map[int]bool) {
	t.Helper()
	prev := agentProcessAliveFn
	agentProcessAliveFn = func(pid int) bool {
		return alive[pid]
	}
	t.Cleanup(func() { agentProcessAliveFn = prev })
}

// stubRemoveMarker swaps removeCoordSpawnMarkerFn for a recording stub
// so tests can assert the self-heal path triggered. Returns a *[]string
// of the project names the stub was called with so the test can pin
// expected behavior. Restored at test end.
func stubRemoveMarker(t *testing.T) *[]string {
	t.Helper()
	prev := removeCoordSpawnMarkerFn
	calls := []string{}
	removeCoordSpawnMarkerFn = func(name string) error {
		calls = append(calls, name)
		return nil
	}
	t.Cleanup(func() { removeCoordSpawnMarkerFn = prev })
	return &calls
}

// TestApplyStuckSelfHeal_DeadSessionTransitionsToIdle pins issue #96
// gap 1 (Path A): when derivation flagged the row as Stuck (marker past
// spawnTimeout) but the tmux session for the marker's agent_id is
// gone, the helper transitions the state to Idle and invokes the
// remove callback exactly once. Pure-input test — no disk, no tmux.
//
// agentRecordFresh=false because Path A's defining case is "no record
// on disk OR record is stale" — silent spawn death.
func TestApplyStuckSelfHeal_DeadSessionTransitionsToIdle(t *testing.T) {
	rmCalled := 0
	got, err := applyStuckSelfHeal(coordSpawnStuck, "abcd1234", false, false, func() error {
		rmCalled++
		return nil
	})
	if err != nil {
		t.Errorf("unexpected err from self-heal: %v", err)
	}
	if got != coordSpawnIdle {
		t.Errorf("got %v; want coordSpawnIdle (dead session must heal)", got)
	}
	if rmCalled != 1 {
		t.Errorf("remove invoked %d times; want 1", rmCalled)
	}
}

// TestApplyStuckSelfHeal_LiveSessionStaleRecordPreservesStuck: when the
// tmux session is alive AND the agent record is NOT fresh (no recent
// last_activity_ts), applyStuckSelfHeal must NOT remove the marker —
// re-attach in findExistingCoordForProject depends on the marker
// pointing at the agent. The render-layer Path C suppression (in
// projectFooterLines) downgrades Stuck → Idle for these rows WITHOUT
// touching the marker, but that's separate from this helper.
//
// This is the regression case for the Path B follow-up: we must not
// expand healing here to "any live session" — only to "live agent
// record." The original Path A behavior is preserved.
func TestApplyStuckSelfHeal_LiveSessionStaleRecordPreservesStuck(t *testing.T) {
	rmCalled := 0
	got, _ := applyStuckSelfHeal(coordSpawnStuck, "abcd1234", true, false, func() error {
		rmCalled++
		return nil
	})
	if got != coordSpawnStuck {
		t.Errorf("got %v; want coordSpawnStuck (live session + stale record must NOT remove marker here)", got)
	}
	if rmCalled != 0 {
		t.Errorf("remove invoked %d times for live session + stale record; want 0", rmCalled)
	}
}

// TestApplyStuckSelfHeal_FreshRecordLiveSessionHealsViaPathB pins the
// follow-up bug fix: when the marker is past spawnTimeout BUT the agent
// record at ~/.fleet/agents/<id>.json shows a fresh last_activity_ts
// AND tmux is alive, the spawn already succeeded and the marker is just
// stale. Path B heals.
func TestApplyStuckSelfHeal_FreshRecordLiveSessionHealsViaPathB(t *testing.T) {
	rmCalled := 0
	got, err := applyStuckSelfHeal(coordSpawnStuck, "abcd1234", true, true, func() error {
		rmCalled++
		return nil
	})
	if err != nil {
		t.Errorf("unexpected err from self-heal: %v", err)
	}
	if got != coordSpawnIdle {
		t.Errorf("got %v; want coordSpawnIdle (fresh record + live session must heal via Path B)", got)
	}
	if rmCalled != 1 {
		t.Errorf("remove invoked %d times; want 1", rmCalled)
	}
}

// TestApplyStuckSelfHeal_FreshRecordDeadSessionAlsoHeals: Path B doesn't
// require live tmux. A fresh record alone is sufficient evidence the
// spawn succeeded — the tmux session may have been killed independently
// (operator [x]'d the agent, machine slept, etc). Heal regardless.
func TestApplyStuckSelfHeal_FreshRecordDeadSessionAlsoHeals(t *testing.T) {
	rmCalled := 0
	got, _ := applyStuckSelfHeal(coordSpawnStuck, "abcd1234", false, true, func() error {
		rmCalled++
		return nil
	})
	if got != coordSpawnIdle {
		t.Errorf("got %v; want coordSpawnIdle (fresh record alone must heal)", got)
	}
	if rmCalled != 1 {
		t.Errorf("remove invoked %d times; want 1", rmCalled)
	}
}

// TestApplyStuckSelfHeal_EmptyAgentIDPreservesStuck: when the marker's
// body read returned empty (concurrent rewrite or hand-edited zero-
// byte file), we can't probe a session OR a record we don't know the
// name of — keep the Stuck state and let the operator triage via the
// soft-fallback hint. Remove must NOT fire (no marker we're confident
// is stale). This regression test pins the contract under Path B too:
// even with agentRecordFresh=true, an empty markerAgentID short-circuits.
func TestApplyStuckSelfHeal_EmptyAgentIDPreservesStuck(t *testing.T) {
	rmCalled := 0
	for _, alive := range []bool{false, true} {
		for _, fresh := range []bool{false, true} {
			got, _ := applyStuckSelfHeal(coordSpawnStuck, "", alive, fresh, func() error {
				rmCalled++
				return nil
			})
			if got != coordSpawnStuck {
				t.Errorf("alive=%v fresh=%v: got %v; want coordSpawnStuck (empty agent ID must keep warning)", alive, fresh, got)
			}
		}
	}
	if rmCalled != 0 {
		t.Errorf("remove invoked %d times with empty agent ID; want 0", rmCalled)
	}
}

// TestApplyStuckSelfHeal_NonStuckStatesUnaffected: Idle / Spawning /
// Active / Waiting / NeverTicked are pass-throughs. The helper must
// never mutate non-Stuck states, even when both heal paths would
// otherwise match — those rows aren't claiming the row is stuck.
func TestApplyStuckSelfHeal_NonStuckStatesUnaffected(t *testing.T) {
	nonStuck := []coordSpawnState{
		coordSpawnIdle,
		coordSpawnSpawning,
		coordSpawnActive,
		coordSpawnWaiting,
		coordSpawnNeverTicked,
	}
	for _, st := range nonStuck {
		rmCalled := 0
		got, _ := applyStuckSelfHeal(st, "abcd1234", false, true, func() error {
			rmCalled++
			return nil
		})
		if got != st {
			t.Errorf("state %v changed to %v; want unchanged", st, got)
		}
		if rmCalled != 0 {
			t.Errorf("remove invoked for non-Stuck state %v; want 0 calls", st)
		}
	}
}

// TestAgentEverActivated_EdgeCases pins the activation-detection
// helper. agentEverActivated uses INEQUALITY (not "strictly after")
// because fleet-guard writes last_activity_ts with whole-second
// precision while agent.New() preserves nanoseconds — a fast first
// Stop hook may write last_activity_ts < spawned_at by sub-second
// truncation while still proving Claude is alive (codex iter-7 P1).
// The only false case is exact-equal (agent.New() shape before any
// hook fires).
func TestAgentEverActivated_EdgeCases(t *testing.T) {
	spawnedAt := time.Date(2026, 5, 9, 12, 0, 0, 123456789, time.UTC)
	cases := []struct {
		name    string
		records []*agent.Record
		id      string
		want    bool
	}{
		{
			name:    "empty id → false",
			records: []*agent.Record{{ID: "x", SpawnedAt: spawnedAt, LastActivityTS: spawnedAt.Add(time.Hour)}},
			id:      "",
			want:    false,
		},
		{
			name:    "nil records → false",
			records: nil,
			id:      "x",
			want:    false,
		},
		{
			name:    "no matching id → false",
			records: []*agent.Record{{ID: "y", SpawnedAt: spawnedAt, LastActivityTS: spawnedAt.Add(time.Hour)}},
			id:      "x",
			want:    false,
		},
		{
			name:    "zero spawned_at → false",
			records: []*agent.Record{{ID: "x", LastActivityTS: spawnedAt}},
			id:      "x",
			want:    false,
		},
		{
			name:    "zero last_activity_ts → false",
			records: []*agent.Record{{ID: "x", SpawnedAt: spawnedAt}},
			id:      "x",
			want:    false,
		},
		{
			name: "exactly equal (agent.New() shape) → false (not activated)",
			records: []*agent.Record{
				{ID: "x", SpawnedAt: spawnedAt, LastActivityTS: spawnedAt},
			},
			id:   "x",
			want: false,
		},
		{
			name: "fast Stop hook in same second (sub-second-less by truncation) → true",
			// fleet-guard writes whole seconds: spawnedAt's nanoseconds
			// drop, leaving last_activity_ts ~123ms LESS than spawnedAt.
			// Activation must be detected (codex iter-7 P1).
			records: []*agent.Record{
				{
					ID:             "x",
					SpawnedAt:      spawnedAt,
					LastActivityTS: spawnedAt.Truncate(time.Second),
				},
			},
			id:   "x",
			want: true,
		},
		{
			name: "Stop hook in next second → true",
			records: []*agent.Record{
				{ID: "x", SpawnedAt: spawnedAt, LastActivityTS: spawnedAt.Truncate(time.Second).Add(1 * time.Second)},
			},
			id:   "x",
			want: true,
		},
		{
			name: "minutes past → true",
			records: []*agent.Record{
				{ID: "x", SpawnedAt: spawnedAt, LastActivityTS: spawnedAt.Add(5 * time.Minute)},
			},
			id:   "x",
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := agentEverActivated(tc.records, tc.id)
			if got != tc.want {
				t.Errorf("agentEverActivated(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestRender_WrapperSurvivedClaudeDied_PreservesStuck pins the codex
// iter-6 [P1] fix: tmux wrapper survived a Claude crash at startup
// (record was written by agent.New() but no Stop hook ever fired, so
// last_activity_ts == spawned_at). Path C must NOT suppress the red
// stuck chip — the operator needs the alert to notice the failure
// and respawn. agentEverActivated is the gate that distinguishes
// "Claude is or was running" (LastActivityTS > SpawnedAt by mtime
// tolerance) from "wrapper alone, Claude never reached a Stop hook."
func TestRender_WrapperSurvivedClaudeDied_PreservesStuck(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	markerMtime := now.Add(-15 * time.Minute) // past 10m timeout
	stubMarkerMtime(t, "demo", markerMtime, true)
	stubMarkerAgentID(t, "demo", "49bc8a03")
	stubAliveSessions(t, map[string]bool{"fleet-49bc8a03": true})
	stubRemoveMarker(t)

	// agent.New()-shape record: SpawnedAt and LastActivityTS set to
	// the same time. Claude crashed before any Stop hook fired, but
	// the tmux wrapper shell is still alive.
	spawnedAt := now.Add(-15 * time.Minute)
	records := []*agent.Record{
		{
			ID:             "49bc8a03",
			LastActivityTS: spawnedAt, // same as SpawnedAt → not activated
			SpawnedAt:      spawnedAt,
		},
	}
	p := &ProjectRow{Name: "demo", RepoSlug: "demo"}
	ctx := coordSpawnCtx{
		now:          now,
		tickFrame:    0,
		spawnTimeout: 10 * time.Minute,
		records:      records,
	}
	lines := projectBlockLines(p, 100, false, ctx)
	joined := strings.Join(lines, "\n")

	if !strings.Contains(joined, "coord spawn stuck") {
		t.Errorf("wrapper-only failure: red stuck chip MUST surface (agentEverActivated gate); got:\n%s", joined)
	}
	if !strings.Contains(joined, "▲") {
		t.Errorf("wrapper-only failure: red attention triangle must surface; got:\n%s", joined)
	}
}

// TestRender_WrapperSurvivedClaudeExited_PreservesStuck pins the codex
// iter-8 [P1] fix: tmux session alive (wrapper shell still running)
// but Claude process exited mid-session — agentEverActivated would
// have been true (one Stop hook fired before the exit), but kill(0)
// returns false. Path C MUST NOT suppress; the operator needs the red
// stuck chip to notice the launch failure and respawn.
//
// This is the case the inequality-based agentEverActivated check
// cannot catch: Claude WAS alive long enough to fire a hook, then
// died. Only the process-liveness probe (agentClaudeAlive) tracks
// the current state.
func TestRender_WrapperSurvivedClaudeExited_PreservesStuck(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	markerMtime := now.Add(-15 * time.Minute)
	stubMarkerMtime(t, "demo", markerMtime, true)
	stubMarkerAgentID(t, "demo", "49bc8a03")
	stubAliveSessions(t, map[string]bool{"fleet-49bc8a03": true})
	// PID 66666 NOT in the alive map → Claude is dead, even though
	// tmux session and agent record both still exist.
	stubProcessAlive(t, map[int]bool{})
	stubRemoveMarker(t)

	// Claude fired one Stop hook then exited. last_activity_ts
	// reflects that hook (after spawned_at) — inequality-based
	// agentEverActivated would return true. But kill(66666, 0) fails.
	spawnedAt := now.Add(-15 * time.Minute)
	records := []*agent.Record{
		{
			ID:             "49bc8a03",
			PID:            66666,
			LastActivityTS: now.Add(-13 * time.Minute), // > spawnedAt
			SpawnedAt:      spawnedAt,
		},
	}
	p := &ProjectRow{Name: "demo", RepoSlug: "demo"}
	ctx := coordSpawnCtx{
		now:          now,
		tickFrame:    0,
		spawnTimeout: 10 * time.Minute,
		records:      records,
	}
	lines := projectBlockLines(p, 100, false, ctx)
	joined := strings.Join(lines, "\n")

	if !strings.Contains(joined, "coord spawn stuck") {
		t.Errorf("wrapper-survived-Claude-exited: red stuck chip MUST surface (kill(0) fails); got:\n%s", joined)
	}
}

// TestRender_StaleNeedsInputLiveClaudeDowngradesToWaiting pins the
// codex iter-8 [P2] fix: when needs_input=true AND Claude alive AND
// last_activity_ts is stale (> agentRecordFreshWindow), the row
// should render the actionable "coord waiting on input — fleet
// attach <id>" hint, NOT the "spawned but never ticked" misdirection.
// fleet-guard only clears needs_input on UserPromptSubmit, so the
// flag stays authoritative; the kill(0) probe independently confirms
// Claude is still running and waiting.
func TestRender_StaleNeedsInputLiveClaudeDowngradesToWaiting(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	markerMtime := now.Add(-15 * time.Minute)
	stubMarkerMtime(t, "demo", markerMtime, true)
	stubMarkerAgentID(t, "demo", "49bc8a03")
	stubAliveSessions(t, map[string]bool{"fleet-49bc8a03": true})
	stubProcessAlive(t, map[int]bool{77777: true})
	stubRemoveMarker(t)

	// needs_input=true; last_activity_ts stale (15min ago → past 10m
	// freshness window so downgradeStuckOnNeedsInput's gate fails);
	// Claude alive (kill(0) succeeds).
	spawnedAt := now.Add(-30 * time.Minute)
	records := []*agent.Record{
		{
			ID:             "49bc8a03",
			PID:            77777,
			LastActivityTS: now.Add(-15 * time.Minute),
			SpawnedAt:      spawnedAt,
			NeedsInput:     true,
		},
	}
	p := &ProjectRow{Name: "demo", RepoSlug: "demo"}
	ctx := coordSpawnCtx{
		now:          now,
		tickFrame:    0,
		spawnTimeout: 10 * time.Minute,
		records:      records,
	}
	lines := projectBlockLines(p, 100, false, ctx)
	joined := strings.Join(lines, "\n")

	if !strings.Contains(joined, "waiting on input") {
		t.Errorf("stale needs_input + live Claude: must render Waiting cue; got:\n%s", joined)
	}
	if strings.Contains(joined, "spawned but never ticked") {
		t.Errorf("Waiting cue must override never-ticked hint; got:\n%s", joined)
	}
	if strings.Contains(joined, "coord spawn stuck") {
		t.Errorf("Waiting cue must override red stuck chip; got:\n%s", joined)
	}
}

// TestRender_LiveClaudeStopHookNeverFired_SuppressesStuck pins the
// codex iter-11 [P1] fix: when spawn.Spawn resolved a real Claude
// PID at dispatch and Claude is alive but no Stop hook has fired
// yet (last_activity_ts == spawned_at), the row must NOT show the
// red stuck warning. The kill(0) probe alone is sufficient (no
// agentEverActivated double-gate) — fleet-binary fallback PIDs are
// dead-by-construction (kill(0) false naturally), and the rare
// wrapper-pane-shell-fallback case is accepted as a v0.1 trade-off
// per the iter-11 reading.
//
// The case this test exercises: cold-start completed (Claude reached
// "ready"), but Stop hook misconfigured / /coordinator skill not
// registered so no Stop hook ever fired since spawn. The dim
// "spawned but never ticked" hint is the correct signal.
func TestRender_LiveClaudeStopHookNeverFired_SuppressesStuck(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	markerMtime := now.Add(-15 * time.Minute) // past 10m timeout
	stubMarkerMtime(t, "demo", markerMtime, true)
	stubMarkerAgentID(t, "demo", "49bc8a03")
	stubAliveSessions(t, map[string]bool{"fleet-49bc8a03": true})
	// PID 99999 IS alive (Claude resolved correctly by spawn.Spawn).
	stubProcessAlive(t, map[int]bool{99999: true})
	stubRemoveMarker(t)

	// Stop hook never fired: last_activity_ts == spawned_at.
	spawnedAt := now.Add(-15 * time.Minute)
	records := []*agent.Record{
		{
			ID:             "49bc8a03",
			PID:            99999,
			LastActivityTS: spawnedAt,
			SpawnedAt:      spawnedAt,
		},
	}
	p := &ProjectRow{Name: "demo", RepoSlug: "demo"}
	ctx := coordSpawnCtx{
		now:          now,
		tickFrame:    0,
		spawnTimeout: 10 * time.Minute,
		records:      records,
	}
	lines := projectBlockLines(p, 100, false, ctx)
	joined := strings.Join(lines, "\n")

	if strings.Contains(joined, "coord spawn stuck") {
		t.Errorf("live Claude + no Stop hook: red stuck chip must be suppressed; got:\n%s", joined)
	}
	if !strings.Contains(joined, "spawned but never ticked") {
		t.Errorf("live Claude + no Stop hook: soft never-ticked hint must surface; got:\n%s", joined)
	}
}

// TestAgentClaudeAlive_EdgeCases pins the process-liveness helper:
// empty id, no matching record, PID 0, nil-record entries all return
// false; matching record with alive PID returns true; matching record
// with dead PID returns false.
func TestAgentClaudeAlive_EdgeCases(t *testing.T) {
	stubProcessAlive(t, map[int]bool{1000: true, 2000: false})

	cases := []struct {
		name    string
		records []*agent.Record
		id      string
		want    bool
	}{
		{"empty id → false", []*agent.Record{{ID: "x", PID: 1000}}, "", false},
		{"nil records → false", nil, "x", false},
		{"no matching id → false", []*agent.Record{{ID: "y", PID: 1000}}, "x", false},
		{"PID 0 → false (legacy / partial write)", []*agent.Record{{ID: "x", PID: 0}}, "x", false},
		{"PID alive → true", []*agent.Record{{ID: "x", PID: 1000}}, "x", true},
		{"PID stubbed dead → false", []*agent.Record{{ID: "x", PID: 2000}}, "x", false},
		{
			name: "nil entry tolerated, later match wins",
			records: []*agent.Record{
				nil,
				{ID: "x", PID: 1000},
			},
			id:   "x",
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := agentClaudeAlive(tc.records, tc.id)
			if got != tc.want {
				t.Errorf("agentClaudeAlive(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestRender_NeedsInputDowngradeBeatsPathC pins the codex iter-6
// [P1] ordering: when a live coord has needs_input=true (Claude
// paused at a question) AND coord-state.json never published, the
// downgrade to Waiting must run BEFORE Path C suppression. Otherwise
// Path C demotes Stuck → Idle first and the downgrade never sees
// Stuck, losing the operator's "fleet attach <id>" cue.
func TestRender_NeedsInputDowngradeBeatsPathC(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	markerMtime := now.Add(-15 * time.Minute)
	stubMarkerMtime(t, "demo", markerMtime, true)
	stubMarkerAgentID(t, "demo", "49bc8a03")
	stubAliveSessions(t, map[string]bool{"fleet-49bc8a03": true})
	stubProcessAlive(t, map[int]bool{88888: true})
	stubRemoveMarker(t)

	// Fresh activity (within freshWindow) AND needs_input=true AND
	// LastTick zero (coord never published a tick).
	spawnedAt := now.Add(-15 * time.Minute)
	records := []*agent.Record{
		{
			ID:             "49bc8a03",
			PID:            88888,
			LastActivityTS: now.Add(-3 * time.Minute), // fresh
			SpawnedAt:      spawnedAt,
			NeedsInput:     true,
		},
	}
	p := &ProjectRow{Name: "demo", RepoSlug: "demo"}
	ctx := coordSpawnCtx{
		now:          now,
		tickFrame:    0,
		spawnTimeout: 10 * time.Minute,
		records:      records,
	}
	lines := projectBlockLines(p, 100, false, ctx)
	joined := strings.Join(lines, "\n")

	if !strings.Contains(joined, "waiting on input") {
		t.Errorf("needs_input + fresh + never-ticked: must downgrade to Waiting (not Path C Idle); got:\n%s", joined)
	}
	if strings.Contains(joined, "spawned but never ticked") {
		t.Errorf("Waiting must override the never-ticked hint; got:\n%s", joined)
	}
	if strings.Contains(joined, "coord spawn stuck") {
		t.Errorf("Waiting must override red stuck chip; got:\n%s", joined)
	}
}

// TestRender_FreshRecordNeverTickedPreservesMarker pins the codex
// iter-5 [P1] fix: when the agent record has a fresh last_activity_ts
// (Path B's freshness gate would normally fire) BUT coord-state.json
// has never been published under this spawn, the marker MUST be
// preserved. Otherwise the operator loses the only signal that points
// at this agent for [a] re-attach, AND the next render drops the
// never-ticked diagnostic entirely (markerAgentID becomes empty).
//
// Real scenario: fleet-guard updates the agent record via Stop hook
// (so last_activity_ts is fresh), but /coordinator skill is broken /
// misconfigured / not registered → no coord-state.json publication.
// Operator is interacting with the misconfigured coord — Path B's
// fresh-record signal lies if the coord didn't actually publish.
func TestRender_FreshRecordNeverTickedPreservesMarker(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	markerMtime := now.Add(-15 * time.Minute) // past 10m timeout
	stubMarkerMtime(t, "demo", markerMtime, true)
	stubMarkerAgentID(t, "demo", "49bc8a03")
	stubAliveSessions(t, map[string]bool{"fleet-49bc8a03": true})
	stubProcessAlive(t, map[int]bool{12345: true}) // claude alive
	rmCalls := stubRemoveMarker(t)

	// last_activity_ts FRESH (within 10m freshWindow) but coord-state
	// never published (LastTick is zero). Pre-iter-5 fix: Path B
	// removed the marker; this PR's gate prevents the removal.
	spawnedAt := now.Add(-15 * time.Minute)
	records := []*agent.Record{
		{
			ID:             "49bc8a03",
			PID:            12345,
			LastActivityTS: now.Add(-2 * time.Minute), // FRESH
			SpawnedAt:      spawnedAt,
		},
	}
	p := &ProjectRow{Name: "demo", RepoSlug: "demo"}
	ctx := coordSpawnCtx{
		now:          now,
		tickFrame:    0,
		spawnTimeout: 10 * time.Minute,
		records:      records,
	}
	lines := projectBlockLines(p, 100, false, ctx)
	joined := strings.Join(lines, "\n")

	if len(*rmCalls) != 0 {
		t.Errorf("never-ticked + fresh record: marker must NOT be removed (re-attach depends on it); got %d (%v)",
			len(*rmCalls), *rmCalls)
	}
	if strings.Contains(joined, "coord spawn stuck") {
		t.Errorf("never-ticked + fresh record: red stuck chip must be suppressed; got:\n%s", joined)
	}
	if !strings.Contains(joined, "spawned but never ticked") {
		t.Errorf("never-ticked + fresh record + past grace window: soft hint must surface; got:\n%s", joined)
	}
}

// TestCoordNeverTickedThisSpawn_MtimePrecision pins the codex iter-14
// [P2] strict-comparison contract: any lastTick BEFORE spawned_at is
// "never ticked under this spawn" (i.e. it's a leftover from the
// previous coord under this project, since coord-state.json is
// project-scoped). No tolerance: a 1-second gap between old coord's
// last tick and new coord's spawn would otherwise be misclassified as
// "new coord ticked," letting Path B remove the marker without proof.
func TestCoordNeverTickedThisSpawn_MtimePrecision(t *testing.T) {
	spawnedAt := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	rec := &agent.Record{ID: "x", SpawnedAt: spawnedAt}
	records := []*agent.Record{rec}

	cases := []struct {
		name     string
		lastTick time.Time
		want     bool // true = "never ticked"
	}{
		{
			name:     "tick exactly at spawn → ticked",
			lastTick: spawnedAt,
			want:     false,
		},
		{
			name:     "tick 1ns before spawn → never ticked (strict)",
			lastTick: spawnedAt.Add(-1 * time.Nanosecond),
			want:     true,
		},
		{
			name:     "tick 1s before spawn → never ticked (leftover from prior coord)",
			lastTick: spawnedAt.Add(-1 * time.Second),
			want:     true,
		},
		{
			name:     "tick 1h before spawn → never ticked (leftover)",
			lastTick: spawnedAt.Add(-1 * time.Hour),
			want:     true,
		},
		{
			name:     "tick 1m after spawn → ticked",
			lastTick: spawnedAt.Add(1 * time.Minute),
			want:     false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := coordNeverTickedThisSpawn(records, "x", tc.lastTick)
			if got != tc.want {
				t.Errorf("coordNeverTickedThisSpawn(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestRender_PreviouslyTickedAlreadyWedged_PreservesStuck pins the
// codex iter-2 [P1] gate on Path C: when an alive coord already
// published a coord-state.json tick under THIS spawn (LastTick ≥
// SpawnedAt) and then wedged (LastTick stale + marker past timeout),
// the stuck attention chip MUST surface — it's a real hang. Path C
// suppression is for "never ticked", not "ticked once and then died".
//
// Without this gate, the codex review iter-2 finding would land: every
// live tmux session with any matching agent record would demote to
// Idle, hiding real wedge cases the operator needs to triage.
func TestRender_PreviouslyTickedAlreadyWedged_PreservesStuck(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	markerMtime := now.Add(-15 * time.Minute) // past 10m timeout
	stubMarkerMtime(t, "demo", markerMtime, true)
	stubMarkerAgentID(t, "demo", "49bc8a03")
	stubAliveSessions(t, map[string]bool{"fleet-49bc8a03": true})
	stubRemoveMarker(t)

	// SpawnedAt 20m ago; LastActivityTS stale; coord ticked at minute
	// 12 (after SpawnedAt) then went silent. Classic "booted, ticked
	// once, then wedged" — real hang.
	records := []*agent.Record{
		{
			ID:             "49bc8a03",
			LastActivityTS: now.Add(-30 * time.Minute), // stale
			SpawnedAt:      now.Add(-20 * time.Minute),
		},
	}
	p := &ProjectRow{
		Name:     "demo",
		RepoSlug: "demo",
		LastTick: now.Add(-8 * time.Minute), // after SpawnedAt, but stale
	}
	ctx := coordSpawnCtx{
		now:          now,
		tickFrame:    0,
		spawnTimeout: 10 * time.Minute,
		records:      records,
	}
	lines := projectBlockLines(p, 100, false, ctx)
	joined := strings.Join(lines, "\n")

	if !strings.Contains(joined, "coord spawn stuck") {
		t.Errorf("previously-ticked + wedged → stuck must surface; Path C must NOT suppress; got:\n%s", joined)
	}
}

// TestCoordNeverTickedThisSpawn_EdgeCases pins the Path C narrow gate
// helper. The "previously ticked then wedged" case (LastTick after
// SpawnedAt) returns false — keep the stuck warning. "Never ticked"
// (zero LastTick OR LastTick before SpawnedAt) returns true — Path C
// can suppress.
func TestCoordNeverTickedThisSpawn_EdgeCases(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	spawnedAt := now.Add(-15 * time.Minute)

	cases := []struct {
		name     string
		records  []*agent.Record
		id       string
		lastTick time.Time
		want     bool
	}{
		{
			name:    "empty id → false",
			records: []*agent.Record{{ID: "x", SpawnedAt: spawnedAt}},
			id:      "",
			want:    false,
		},
		{
			name:    "nil records → false",
			records: nil,
			id:      "x",
			want:    false,
		},
		{
			name:    "no matching record → false",
			records: []*agent.Record{{ID: "y", SpawnedAt: spawnedAt}},
			id:      "x",
			want:    false,
		},
		{
			// codex iter-9 P2: default to "never ticked" when
			// SpawnedAt is missing AND no state file exists, so
			// Path B can't remove the marker.
			name:    "zero SpawnedAt + zero LastTick → true (legacy, preserve marker)",
			records: []*agent.Record{{ID: "x"}},
			id:      "x",
			want:    true,
		},
		{
			// codex iter-15 P2: if a coord-state.json exists on disk
			// (legacy record with non-zero LastTick), assume the coord
			// ticked — without SpawnedAt we can't tell if the tick is
			// from this spawn or a prior one, but defaulting to
			// "ticked" prevents Path C from hiding real hangs behind
			// the soft never-ticked hint for upgraded installs.
			name: "zero SpawnedAt + non-zero LastTick → false (assume ticked, surface hangs)",
			records: []*agent.Record{
				{ID: "x"},
			},
			id:       "x",
			lastTick: now.Add(-5 * time.Minute),
			want:     false,
		},
		{
			name:    "zero LastTick, valid SpawnedAt → true (never ticked)",
			records: []*agent.Record{{ID: "x", SpawnedAt: spawnedAt}},
			id:      "x",
			want:    true,
		},
		{
			name:     "LastTick before SpawnedAt → true (leftover tick from previous coord)",
			records:  []*agent.Record{{ID: "x", SpawnedAt: spawnedAt}},
			id:       "x",
			lastTick: now.Add(-1 * time.Hour),
			want:     true,
		},
		{
			name:     "LastTick equals SpawnedAt → false (boundary: tick at exact spawn moment counts as ticked)",
			records:  []*agent.Record{{ID: "x", SpawnedAt: spawnedAt}},
			id:       "x",
			lastTick: spawnedAt,
			want:     false,
		},
		{
			name:     "LastTick after SpawnedAt → false (already ticked, even if now stale)",
			records:  []*agent.Record{{ID: "x", SpawnedAt: spawnedAt}},
			id:       "x",
			lastTick: now.Add(-2 * time.Minute),
			want:     false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := coordNeverTickedThisSpawn(tc.records, tc.id, tc.lastTick)
			if got != tc.want {
				t.Errorf("coordNeverTickedThisSpawn(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestStuckSelfHeal_PathC_TmuxAliveAndRecordExists_ClearsStuck pins the
// brief-required Path C contract at the render layer (not in
// applyStuckSelfHeal — see that helper's docstring for why Path C is
// deliberately separated). With tmux alive AND an agent record on disk
// for markerAgentID (stale — outside Path B's 10m freshness window),
// the project block must NOT render the red "▲ coord spawn stuck"
// warning, even though derivation lands on Stuck because the marker
// is past spawnTimeout AND coord-state.json is stale. The marker
// MUST remain on disk so [a] re-attach via findExistingCoordForProject
// still resolves the live coord.
//
// Path C is only meaningful when Path B does not already heal — i.e.
// when last_activity_ts is older than the freshness window. In
// realistic fleet-on-disk shapes that means SpawnedAt is also older
// (last_activity_ts is initialized to SpawnedAt at agent.New()), so
// the never-ticked hint downstream is expected to render alongside.
// Asserting Path C's no-marker-removal property is the load-bearing
// claim of this test.
func TestStuckSelfHeal_PathC_TmuxAliveAndRecordExists_ClearsStuck(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	markerMtime := now.Add(-15 * time.Minute) // past 10m timeout
	stubMarkerMtime(t, "demo", markerMtime, true)
	stubMarkerAgentID(t, "demo", "49bc8a03")
	stubAliveSessions(t, map[string]bool{"fleet-49bc8a03": true})
	stubProcessAlive(t, map[int]bool{12345: true}) // claude alive
	rmCalls := stubRemoveMarker(t)

	// Record exists, stale (past 10m freshness window) but Claude is
	// alive (process probe). Path B does NOT fire because the
	// freshness gate fails; Path C suppresses the stuck chip without
	// touching the marker.
	records := []*agent.Record{
		{
			ID:             "49bc8a03",
			PID:            12345,
			LastActivityTS: now.Add(-15 * time.Minute),
			SpawnedAt:      now.Add(-30 * time.Minute),
		},
	}
	p := &ProjectRow{Name: "demo", RepoSlug: "demo"}
	ctx := coordSpawnCtx{
		now:          now,
		tickFrame:    0,
		spawnTimeout: 10 * time.Minute,
		records:      records,
	}
	lines := projectBlockLines(p, 100, false, ctx)
	joined := strings.Join(lines, "\n")

	if strings.Contains(joined, "coord spawn stuck") {
		t.Errorf("Path C suppression failed: red 'coord spawn stuck' fired; got:\n%s", joined)
	}
	if strings.Contains(joined, "▲") {
		t.Errorf("Path C suppression failed: red attention triangle still present; got:\n%s", joined)
	}
	if len(*rmCalls) != 0 {
		t.Errorf("Path C must NOT remove marker (re-attach depends on it); got %d (%v)", len(*rmCalls), *rmCalls)
	}
}

// TestProjectRow_StaleMarkerSelfHeals: end-to-end render-path
// integration for issue #96 gap 1. With a marker past spawnTimeout
// AND the named tmux session dead, the renderer must:
//  1. NOT emit the red "stuck" warning (the row is stale, not really
//     hung),
//  2. invoke removeCoordSpawnMarkerFn for this project so the next
//     render naturally flips back to the existing Idle path.
func TestProjectRow_StaleMarkerSelfHeals(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	markerMtime := now.Add(-15 * time.Minute) // past 10m timeout
	stubMarkerMtime(t, "demo", markerMtime, true)
	stubMarkerAgentID(t, "demo", "abcd1234")
	// Empty alive map → fleet-abcd1234 is dead.
	stubAliveSessions(t, map[string]bool{})
	rmCalls := stubRemoveMarker(t)

	p := &ProjectRow{Name: "demo", RepoSlug: "demo"}
	ctx := coordSpawnCtx{now: now, tickFrame: 0, spawnTimeout: 10 * time.Minute}
	lines := projectBlockLines(p, 80, false, ctx)
	joined := strings.Join(lines, "\n")

	if strings.Contains(joined, "coord spawn stuck") {
		t.Errorf("stale-marker self-heal must suppress stuck warning; got:\n%s", joined)
	}
	if len(*rmCalls) != 1 || (*rmCalls)[0] != "demo" {
		t.Errorf("expected exactly one remove(demo) call; got %v", *rmCalls)
	}
}

// TestProjectRow_StuckHintFallsBackWhenAgentIDMissing pins issue #96
// gap 2's fallback path: a marker present (mtime past timeout) but with
// no agent_id body must render a soft-fallback hint that names the
// project AND tells the operator the agent ID couldn't be resolved.
// We don't render `fleet-<projectName>` (which never matches a real
// session under tmux's `fleet-<8hex>` convention).
//
// Self-heal does NOT fire when agent_id is empty (we can't probe a
// session we don't know), so the warning still surfaces.
func TestProjectRow_StuckHintFallsBackWhenAgentIDMissing(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	markerMtime := now.Add(-15 * time.Minute)
	stubMarkerMtime(t, "demo", markerMtime, true)
	stubMarkerAgentID(t, "demo", "") // marker present but body empty
	stubAliveSessions(t, map[string]bool{})

	p := &ProjectRow{Name: "demo", RepoSlug: "demo"}
	ctx := coordSpawnCtx{now: now, tickFrame: 0, spawnTimeout: 10 * time.Minute}
	lines := projectBlockLines(p, 100, false, ctx)
	joined := strings.Join(lines, "\n")

	if !strings.Contains(joined, "coord spawn stuck") {
		t.Errorf("expected stuck warning to render with empty agent ID; got:\n%s", joined)
	}
	if !strings.Contains(joined, "demo") {
		t.Errorf("fallback hint must mention project 'demo'; got:\n%s", joined)
	}
	if !strings.Contains(joined, "no agent ID") {
		t.Errorf("fallback hint must surface 'no agent ID'; got:\n%s", joined)
	}
}

// TestIsAgentRecordFresh covers the helper that drives Path B of
// applyStuckSelfHeal. Pure-input — feeds a synthetic records slice +
// fixed now + window so the test is deterministic.
func TestIsAgentRecordFresh(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	window := 10 * time.Minute

	tests := []struct {
		name    string
		records []*agent.Record
		id      string
		want    bool
	}{
		{
			name:    "nil records → not fresh",
			records: nil,
			id:      "abcd1234",
			want:    false,
		},
		{
			name:    "empty records → not fresh",
			records: []*agent.Record{},
			id:      "abcd1234",
			want:    false,
		},
		{
			name:    "empty id short-circuits → not fresh",
			records: []*agent.Record{{ID: "abcd1234", LastActivityTS: now.Add(-1 * time.Minute)}},
			id:      "",
			want:    false,
		},
		{
			name:    "id not present → not fresh",
			records: []*agent.Record{{ID: "deadbeef", LastActivityTS: now.Add(-1 * time.Minute)}},
			id:      "abcd1234",
			want:    false,
		},
		{
			name:    "match, fresh tick within window → fresh",
			records: []*agent.Record{{ID: "abcd1234", LastActivityTS: now.Add(-2 * time.Minute)}},
			id:      "abcd1234",
			want:    true,
		},
		{
			name:    "match, exactly at window edge → fresh (≤ window)",
			records: []*agent.Record{{ID: "abcd1234", LastActivityTS: now.Add(-window)}},
			id:      "abcd1234",
			want:    true,
		},
		{
			name:    "match, just past window → not fresh",
			records: []*agent.Record{{ID: "abcd1234", LastActivityTS: now.Add(-window - time.Second)}},
			id:      "abcd1234",
			want:    false,
		},
		{
			name:    "match, zero LastActivityTS (legacy / partial write) → not fresh",
			records: []*agent.Record{{ID: "abcd1234"}},
			id:      "abcd1234",
			want:    false,
		},
		{
			name: "match in middle of slice → fresh",
			records: []*agent.Record{
				{ID: "11111111", LastActivityTS: now.Add(-1 * time.Hour)},
				{ID: "abcd1234", LastActivityTS: now.Add(-30 * time.Second)},
				{ID: "22222222", LastActivityTS: now.Add(-2 * time.Minute)},
			},
			id:   "abcd1234",
			want: true,
		},
		{
			name: "nil entry tolerated; later match wins",
			records: []*agent.Record{
				nil,
				{ID: "abcd1234", LastActivityTS: now.Add(-30 * time.Second)},
			},
			id:   "abcd1234",
			want: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isAgentRecordFresh(tc.records, tc.id, now, window)
			if got != tc.want {
				t.Errorf("isAgentRecordFresh(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestProjectRow_FreshAgentRecordSelfHealsLiveSession pins Path B's
// heal + marker removal: marker stuck, agent record fresh
// (last_activity_ts within freshness window), tmux ALIVE, coord has
// ticked under this spawn → state heals to Idle, removeMarker called
// once. The marker is safe to remove because findCoordByLockBody
// rescues subsequent [a] re-attach (coord-state.json + lock body
// published per the LastTick > SpawnedAt evidence).
//
// Post codex iter-5: Path B's marker removal is gated on the coord
// having ticked (LastTick set AND ≥ SpawnedAt). Without that proof,
// the marker is preserved for re-attach (different test below).
func TestProjectRow_FreshAgentRecordSelfHealsLiveSession(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	markerMtime := now.Add(-15 * time.Minute) // past 10m timeout
	stubMarkerMtime(t, "demo", markerMtime, true)
	stubMarkerAgentID(t, "demo", "abcd1234")
	stubAliveSessions(t, map[string]bool{"fleet-abcd1234": true})
	rmCalls := stubRemoveMarker(t)

	spawnedAt := now.Add(-20 * time.Minute)
	records := []*agent.Record{
		{
			ID:             "abcd1234",
			LastActivityTS: now.Add(-30 * time.Second), // fresh
			SpawnedAt:      spawnedAt,
		},
	}
	// LastTick AFTER SpawnedAt by 10min — proves the coord booted and
	// ticked at some point under THIS spawn (post codex iter-5 gate).
	p := &ProjectRow{
		Name:     "demo",
		RepoSlug: "demo",
		LastTick: now.Add(-10 * time.Minute),
	}
	ctx := coordSpawnCtx{
		now:          now,
		tickFrame:    0,
		spawnTimeout: 10 * time.Minute,
		records:      records,
	}
	lines := projectBlockLines(p, 80, false, ctx)
	joined := strings.Join(lines, "\n")

	if strings.Contains(joined, "coord spawn stuck") {
		t.Errorf("Path B self-heal must suppress stuck warning when agent record is fresh AND ticked; got:\n%s", joined)
	}
	if len(*rmCalls) != 1 || (*rmCalls)[0] != "demo" {
		t.Errorf("expected exactly one remove(demo) call; got %v", *rmCalls)
	}
}

// TestProjectRow_FreshAgentRecordSelfHealsDeadSession pins Path B
// independence from tmux state: even if the tmux session is dead, a
// fresh agent record proves the spawn succeeded at some point and the
// marker is stale. Heal regardless of session state.
//
// (Path A would also fire here — both paths are sufficient. The check is
// that the heal happens once, which is what removeMarker call count
// asserts.)
func TestProjectRow_FreshAgentRecordSelfHealsDeadSession(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	markerMtime := now.Add(-15 * time.Minute)
	stubMarkerMtime(t, "demo", markerMtime, true)
	stubMarkerAgentID(t, "demo", "abcd1234")
	stubAliveSessions(t, map[string]bool{}) // dead
	rmCalls := stubRemoveMarker(t)

	records := []*agent.Record{
		{ID: "abcd1234", LastActivityTS: now.Add(-1 * time.Minute)},
	}
	p := &ProjectRow{Name: "demo", RepoSlug: "demo"}
	ctx := coordSpawnCtx{
		now:          now,
		tickFrame:    0,
		spawnTimeout: 10 * time.Minute,
		records:      records,
	}
	lines := projectBlockLines(p, 80, false, ctx)
	joined := strings.Join(lines, "\n")

	if strings.Contains(joined, "coord spawn stuck") {
		t.Errorf("fresh agent record must heal even with dead tmux; got:\n%s", joined)
	}
	if len(*rmCalls) != 1 {
		t.Errorf("expected exactly one remove call; got %d (%v)", len(*rmCalls), *rmCalls)
	}
}

// TestProjectRow_NoAgentRecordLiveSessionPreservesStuck pins the
// regression: marker stuck, NO matching agent record loaded into the
// model, tmux alive → does NOT heal. The operator should still see the
// warning so they can investigate (the spawn is genuinely hung — no
// record was ever written, suggesting fleet-guard never reported in).
func TestProjectRow_NoAgentRecordLiveSessionPreservesStuck(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	markerMtime := now.Add(-15 * time.Minute)
	stubMarkerMtime(t, "demo", markerMtime, true)
	stubMarkerAgentID(t, "demo", "abcd1234")
	stubAliveSessions(t, map[string]bool{"fleet-abcd1234": true})
	rmCalls := stubRemoveMarker(t)

	// No matching agent record — m.records is nil / empty.
	p := &ProjectRow{Name: "demo", RepoSlug: "demo"}
	ctx := coordSpawnCtx{
		now:          now,
		tickFrame:    0,
		spawnTimeout: 10 * time.Minute,
		records:      nil,
	}
	lines := projectBlockLines(p, 80, false, ctx)
	joined := strings.Join(lines, "\n")

	if !strings.Contains(joined, "coord spawn stuck") {
		t.Errorf("expected stuck warning when no agent record + live session; got:\n%s", joined)
	}
	if len(*rmCalls) != 0 {
		t.Errorf("self-heal must NOT fire without an agent record; got %d remove calls (%v)", len(*rmCalls), *rmCalls)
	}
}

// TestProjectRow_StaleAgentRecordDeadSessionStillHealsViaPathA pins
// regression for Path A: even when a stale agent record exists (old
// last_activity_ts), the dead-tmux gate must still heal — silent spawn
// death is the original Path A case and must keep working.
func TestProjectRow_StaleAgentRecordDeadSessionStillHealsViaPathA(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	markerMtime := now.Add(-15 * time.Minute)
	stubMarkerMtime(t, "demo", markerMtime, true)
	stubMarkerAgentID(t, "demo", "abcd1234")
	stubAliveSessions(t, map[string]bool{}) // dead
	rmCalls := stubRemoveMarker(t)

	records := []*agent.Record{
		{ID: "abcd1234", LastActivityTS: now.Add(-2 * time.Hour)}, // stale
	}
	p := &ProjectRow{Name: "demo", RepoSlug: "demo"}
	ctx := coordSpawnCtx{
		now:          now,
		tickFrame:    0,
		spawnTimeout: 10 * time.Minute,
		records:      records,
	}
	lines := projectBlockLines(p, 80, false, ctx)
	joined := strings.Join(lines, "\n")

	if strings.Contains(joined, "coord spawn stuck") {
		t.Errorf("Path A must still heal on dead tmux even with stale record; got:\n%s", joined)
	}
	if len(*rmCalls) != 1 {
		t.Errorf("expected one remove call via Path A; got %d (%v)", len(*rmCalls), *rmCalls)
	}
}

// TestProjectRow_StaleAgentRecordLiveSessionSuppressesViaPathC pins
// the false-positive fix (2026-05-14): marker past timeout, agent record
// exists with OLD last_activity_ts (beyond freshness window), tmux ALIVE,
// Claude process alive.
//
// Pre-Path-C this rendered "▲ coord spawn stuck" — the false alarm that
// drove the operator-facing complaint. Real-world driver: fleet-49bc8a03
// in HANDOFF state at 6% context, idle between Stop hook fires, working
// fine. The record's last_activity_ts had not advanced recently, but
// Claude was alive (kill(0) succeeds) AND fleet's state model
// registered it (record present).
//
// Post-fix this suppresses the attention chip at the render layer
// WITHOUT removing the marker — [a] re-attach still depends on the
// marker pointing at the agent (findExistingCoordForProject reads
// coordSpawnMarkerFn). If the agent is past the cold-start grace window
// without a tick, the softer coordSpawnNeverTicked hint surfaces the
// underlying Stop-hook / skill-registration problem informationally.
func TestProjectRow_StaleAgentRecordLiveSessionSuppressesViaPathC(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	markerMtime := now.Add(-15 * time.Minute)
	stubMarkerMtime(t, "demo", markerMtime, true)
	stubMarkerAgentID(t, "demo", "abcd1234")
	stubAliveSessions(t, map[string]bool{"fleet-abcd1234": true})
	stubProcessAlive(t, map[int]bool{22222: true}) // claude alive
	rmCalls := stubRemoveMarker(t)

	// Stale last_activity_ts AND stale SpawnedAt (older than the
	// coordSpawnTimeoutDefault grace window). Path C suppresses (Claude
	// process alive via kill(0)); the never-ticked hint surfaces
	// because LastTick is zero (no coord-state.json has ever been
	// published).
	records := []*agent.Record{
		{
			ID:             "abcd1234",
			PID:            22222,
			LastActivityTS: now.Add(-30 * time.Minute),
			SpawnedAt:      now.Add(-1 * time.Hour),
		},
	}
	p := &ProjectRow{Name: "demo", RepoSlug: "demo"}
	ctx := coordSpawnCtx{
		now:          now,
		tickFrame:    0,
		spawnTimeout: 10 * time.Minute,
		records:      records,
	}
	lines := projectBlockLines(p, 100, false, ctx)
	joined := strings.Join(lines, "\n")

	if strings.Contains(joined, "coord spawn stuck") {
		t.Errorf("Path C must suppress red chip on live tmux + record exists; got:\n%s", joined)
	}
	if len(*rmCalls) != 0 {
		t.Errorf("Path C must NOT remove marker (would break [a] re-attach); got %d (%v)", len(*rmCalls), *rmCalls)
	}
	if !strings.Contains(joined, "spawned but never ticked") {
		t.Errorf("expected softer 'never ticked' hint after Path C suppression; got:\n%s", joined)
	}
}

// TestRender_NeverTicked_ShowsNotTickingHint pins the brief-required
// observable: a coord whose tmux session is alive AND whose record was
// written but which has never published a coord-state.json tick must
// render the informational "spawned but never ticked" hint (dim
// styling), not the red "▲ coord spawn stuck" attention chip.
//
// Setup mirrors the real false-positive driver (2026-05-14
// fleet-49bc8a03): marker past timeout, agent record on disk with
// SpawnedAt 15min ago AND LastActivityTS == SpawnedAt (no tick stamp
// since spawn), tmux session up. Path C suppresses Stuck → Idle (the
// marker stays on disk so [a] re-attach in findExistingCoordForProject
// still works), then the never-ticked detector promotes Idle →
// NeverTicked so the operator gets a soft pointer at Stop-hook or
// /coordinator skill-registration breakage.
func TestRender_NeverTicked_ShowsNotTickingHint(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	markerMtime := now.Add(-15 * time.Minute) // past 10m timeout
	stubMarkerMtime(t, "demo", markerMtime, true)
	stubMarkerAgentID(t, "demo", "49bc8a03")
	stubAliveSessions(t, map[string]bool{"fleet-49bc8a03": true})
	stubProcessAlive(t, map[int]bool{33333: true}) // claude alive
	rmCalls := stubRemoveMarker(t)

	// Record on disk: SpawnedAt 15min ago (past coldStart grace),
	// LastActivityTS 10min ago (stale, but Claude process is alive via
	// kill(0) probe). Path B does not fire (stale freshness gate);
	// Path C suppresses on alive-claude + record-exists + never-ticked
	// (without touching the marker).
	spawnedAt := now.Add(-15 * time.Minute)
	records := []*agent.Record{
		{
			ID:             "49bc8a03",
			PID:            33333,
			LastActivityTS: now.Add(-12 * time.Minute), // stale
			SpawnedAt:      spawnedAt,
		},
	}
	// LastTick is zero — coord-state.json was never written.
	p := &ProjectRow{Name: "demo", RepoSlug: "demo"}
	ctx := coordSpawnCtx{
		now:          now,
		tickFrame:    0,
		spawnTimeout: 10 * time.Minute,
		records:      records,
	}
	lines := projectBlockLines(p, 100, false, ctx)
	joined := strings.Join(lines, "\n")

	if strings.Contains(joined, "coord spawn stuck") {
		t.Errorf("must NOT render red stuck warning for alive+never-ticked; got:\n%s", joined)
	}
	if !strings.Contains(joined, "spawned but never ticked") {
		t.Errorf("expected 'spawned but never ticked' soft hint; got:\n%s", joined)
	}
	if !strings.Contains(joined, "49bc8a03") {
		t.Errorf("never-ticked hint must include agent ID; got:\n%s", joined)
	}
	if len(*rmCalls) != 0 {
		t.Errorf("Path C must NOT remove marker (would break [a] re-attach); got %d (%v)", len(*rmCalls), *rmCalls)
	}
}

// TestRender_NeverTicked_GraceWindowSuppressesHint: during the
// cold-start grace window (≤ coordSpawnTimeoutDefault since SpawnedAt),
// the never-ticked hint must NOT fire — the cold start may still
// complete. The Path C suppression handles the red chip; never-ticked
// only adds the soft informational hint when waiting has clearly gone
// beyond healthy.
//
// Setup: agent activated (Stop hook fired once, last_activity_ts >
// spawned_at) but recent enough to be inside the grace window;
// coord-state.json never published. Path C suppresses Stuck → Idle;
// never-ticked's grace check sees SpawnedAt within window → suppresses
// the soft hint.
func TestRender_NeverTicked_GraceWindowSuppressesHint(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	markerMtime := now.Add(-15 * time.Minute)
	stubMarkerMtime(t, "demo", markerMtime, true)
	stubMarkerAgentID(t, "demo", "49bc8a03")
	stubAliveSessions(t, map[string]bool{"fleet-49bc8a03": true})
	stubProcessAlive(t, map[int]bool{44444: true}) // claude alive
	stubRemoveMarker(t)

	spawnedAt := now.Add(-2 * time.Minute) // within 10m grace window
	records := []*agent.Record{
		{
			ID:             "49bc8a03",
			PID:            44444,
			LastActivityTS: now.Add(-30 * time.Second), // > spawnedAt → activated
			SpawnedAt:      spawnedAt,
		},
	}
	p := &ProjectRow{Name: "demo", RepoSlug: "demo"}
	ctx := coordSpawnCtx{
		now:          now,
		tickFrame:    0,
		spawnTimeout: 10 * time.Minute,
		records:      records,
	}
	lines := projectBlockLines(p, 100, false, ctx)
	joined := strings.Join(lines, "\n")

	if strings.Contains(joined, "coord spawn stuck") {
		t.Errorf("stuck must be suppressed (Path C: alive + activated + never-ticked); got:\n%s", joined)
	}
	if strings.Contains(joined, "spawned but never ticked") {
		t.Errorf("never-ticked hint must NOT fire inside grace window; got:\n%s", joined)
	}
}

// TestRender_NeverTicked_PriorTickSuppressesHint: when a coord-state.json
// tick exists from a previous spawn (LastTick non-zero AND >= the
// agent's SpawnedAt), the never-ticked hint must NOT fire — a tick
// arrived under THIS spawn, even if it's now stale. Distinct from the
// "no tick file at all" case the hint is meant to catch.
func TestRender_NeverTicked_PriorTickSuppressesHint(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	markerMtime := now.Add(-15 * time.Minute)
	stubMarkerMtime(t, "demo", markerMtime, true)
	stubMarkerAgentID(t, "demo", "49bc8a03")
	stubAliveSessions(t, map[string]bool{"fleet-49bc8a03": true})
	stubRemoveMarker(t)

	spawnedAt := now.Add(-15 * time.Minute)
	records := []*agent.Record{
		{
			ID:             "49bc8a03",
			LastActivityTS: spawnedAt,
			SpawnedAt:      spawnedAt,
		},
	}
	// LastTick is AFTER SpawnedAt → a tick already happened under this
	// agent. The agent stopped ticking later (now stale), but it's
	// "post-active idle-stop", not "spawned but never ticked".
	p := &ProjectRow{
		Name:     "demo",
		RepoSlug: "demo",
		LastTick: now.Add(-7 * time.Minute), // > spawnedAt, stale (>5m)
	}
	ctx := coordSpawnCtx{
		now:          now,
		tickFrame:    0,
		spawnTimeout: 10 * time.Minute,
		records:      records,
	}
	lines := projectBlockLines(p, 100, false, ctx)
	joined := strings.Join(lines, "\n")

	if strings.Contains(joined, "spawned but never ticked") {
		t.Errorf("never-ticked hint must NOT fire when LastTick >= SpawnedAt; got:\n%s", joined)
	}
}

// TestRender_IdleNotStuck_NoRedWarning pins the headline observable:
// the operator-facing complaint that "▲ coord spawn stuck" rendered on
// a working coord. This test exercises the exact configuration the
// operator described — alive tmux, agent record on disk, idle in
// HANDOFF (no recent tick) — and asserts the row renders WITHOUT the
// red attention chip. The softer never-ticked hint is acceptable (and
// asserted by TestRender_NeverTicked_ShowsNotTickingHint); this test
// only asserts the false-positive is gone.
func TestRender_IdleNotStuck_NoRedWarning(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	markerMtime := now.Add(-20 * time.Minute) // well past 10m timeout
	stubMarkerMtime(t, "demo", markerMtime, true)
	stubMarkerAgentID(t, "demo", "49bc8a03")
	stubAliveSessions(t, map[string]bool{"fleet-49bc8a03": true})
	stubProcessAlive(t, map[int]bool{55555: true}) // claude alive
	stubRemoveMarker(t)

	// Alive coord in HANDOFF state: spawned 20 minutes ago, Stop hook
	// has fired (last_activity_ts > spawned_at, but stale relative to
	// the 10m freshness window — operator's been idle waiting). Maps
	// to the operator-reported config 1:1; agentClaudeAlive returns
	// true (kill(record.PID, 0) succeeds), distinguishing this from
	// "tmux wrapper survived a Claude crash."
	spawnedAt := now.Add(-20 * time.Minute)
	records := []*agent.Record{
		{
			ID:             "49bc8a03",
			PID:            55555,
			LastActivityTS: now.Add(-15 * time.Minute), // > spawnedAt by 5min, stale
			SpawnedAt:      spawnedAt,
		},
	}
	p := &ProjectRow{Name: "demo", RepoSlug: "demo"}
	ctx := coordSpawnCtx{
		now:          now,
		tickFrame:    0,
		spawnTimeout: 10 * time.Minute,
		records:      records,
	}
	lines := projectBlockLines(p, 100, false, ctx)
	joined := strings.Join(lines, "\n")

	if strings.Contains(joined, "coord spawn stuck") {
		t.Errorf("false-positive: red 'coord spawn stuck' must not fire for alive+record-present row; got:\n%s", joined)
	}
	if strings.Contains(joined, "▲") {
		t.Errorf("red attention triangle must not appear on a working coord; got:\n%s", joined)
	}
}

// TestAgentRecordExists_EdgeCases pins the helper's contract: nil id,
// empty slice, nil entries, and not-present id all return false; a
// match anywhere in the slice returns true regardless of LastActivityTS.
func TestAgentRecordExists_EdgeCases(t *testing.T) {
	cases := []struct {
		name    string
		records []*agent.Record
		id      string
		want    bool
	}{
		{"empty id → false", []*agent.Record{{ID: "abcd1234"}}, "", false},
		{"nil records → false", nil, "abcd1234", false},
		{"empty records → false", []*agent.Record{}, "abcd1234", false},
		{"no match → false", []*agent.Record{{ID: "deadbeef"}}, "abcd1234", false},
		{"match (zero LastActivityTS) → true", []*agent.Record{{ID: "abcd1234"}}, "abcd1234", true},
		{
			name: "match in middle of slice → true",
			records: []*agent.Record{
				{ID: "11111111"},
				nil,
				{ID: "abcd1234"},
				{ID: "22222222"},
			},
			id:   "abcd1234",
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := agentRecordExists(tc.records, tc.id); got != tc.want {
				t.Errorf("agentRecordExists(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestAgentNeverTickedSinceSpawn_EdgeCases pins the never-ticked
// predicate. The fresh-spawn (within grace), record-absent, and
// already-ticked cases must all suppress.
func TestAgentNeverTickedSinceSpawn_EdgeCases(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	grace := 10 * time.Minute

	cases := []struct {
		name     string
		records  []*agent.Record
		id       string
		lastTick time.Time
		want     bool
	}{
		{
			name:    "empty id → false",
			records: []*agent.Record{{ID: "x", SpawnedAt: now.Add(-15 * time.Minute)}},
			id:      "",
			want:    false,
		},
		{
			name:    "nil records → false",
			records: nil,
			id:      "x",
			want:    false,
		},
		{
			name:    "no matching record → false",
			records: []*agent.Record{{ID: "y", SpawnedAt: now.Add(-15 * time.Minute)}},
			id:      "x",
			want:    false,
		},
		{
			// codex iter-10 P2 fix: legacy records without SpawnedAt
			// still surface the never-ticked hint so the row isn't
			// silent after Path C suppression.
			name:    "zero SpawnedAt (legacy) → true (surface diagnostic)",
			records: []*agent.Record{{ID: "x"}},
			id:      "x",
			want:    true,
		},
		{
			name:    "spawn within grace window → false",
			records: []*agent.Record{{ID: "x", SpawnedAt: now.Add(-5 * time.Minute)}},
			id:      "x",
			want:    false,
		},
		{
			name:    "spawn exactly at grace edge → false (≤ grace)",
			records: []*agent.Record{{ID: "x", SpawnedAt: now.Add(-grace)}},
			id:      "x",
			want:    false,
		},
		{
			name:    "spawn past grace, no tick file → true",
			records: []*agent.Record{{ID: "x", SpawnedAt: now.Add(-15 * time.Minute)}},
			id:      "x",
			want:    true,
		},
		{
			name:     "spawn past grace, tick BEFORE spawn → true",
			records:  []*agent.Record{{ID: "x", SpawnedAt: now.Add(-15 * time.Minute)}},
			id:       "x",
			lastTick: now.Add(-1 * time.Hour),
			want:     true,
		},
		{
			name:     "spawn past grace, tick AT spawn → false (already ticked)",
			records:  []*agent.Record{{ID: "x", SpawnedAt: now.Add(-15 * time.Minute)}},
			id:       "x",
			lastTick: now.Add(-15 * time.Minute),
			want:     false,
		},
		{
			name:     "spawn past grace, tick AFTER spawn → false (already ticked)",
			records:  []*agent.Record{{ID: "x", SpawnedAt: now.Add(-15 * time.Minute)}},
			id:       "x",
			lastTick: now.Add(-2 * time.Minute),
			want:     false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := agentNeverTickedSinceSpawn(tc.records, tc.id, tc.lastTick, now, grace)
			if got != tc.want {
				t.Errorf("agentNeverTickedSinceSpawn(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}
