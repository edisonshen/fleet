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
// last_activity_ts), the warning is real — a genuinely-hung spawn the
// operator should attach to via tmux. Heal must NOT fire.
//
// This is the regression case for the Path B follow-up: we must not
// expand healing to "any live session" — only to "live agent record."
// The original Path A behavior is preserved.
func TestApplyStuckSelfHeal_LiveSessionStaleRecordPreservesStuck(t *testing.T) {
	rmCalled := 0
	got, _ := applyStuckSelfHeal(coordSpawnStuck, "abcd1234", true, false, func() error {
		rmCalled++
		return nil
	})
	if got != coordSpawnStuck {
		t.Errorf("got %v; want coordSpawnStuck (live session + stale record must keep warning)", got)
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
// Active are pass-throughs. The helper must never mutate non-Stuck
// states, even when both heal paths would otherwise match — those rows
// aren't claiming the row is stuck.
func TestApplyStuckSelfHeal_NonStuckStatesUnaffected(t *testing.T) {
	for _, st := range []coordSpawnState{coordSpawnIdle, coordSpawnSpawning, coordSpawnActive} {
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

// TestProjectRow_FreshAgentRecordSelfHealsLiveSession pins the
// follow-up bug: marker stuck, agent record fresh (last_activity_ts
// within freshness window), tmux ALIVE → state heals to Idle, removeMarker
// called once. This is the exact local scenario the operator hit:
// `~/.fleet/projects/projects-fleet/.locks/coord-spawn-marker` past
// timeout, but `~/.fleet/agents/<id>.json` shows a fresh tick AND the
// tmux session is up.
func TestProjectRow_FreshAgentRecordSelfHealsLiveSession(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	markerMtime := now.Add(-15 * time.Minute) // past 10m timeout
	stubMarkerMtime(t, "demo", markerMtime, true)
	stubMarkerAgentID(t, "demo", "abcd1234")
	// fleet-abcd1234 IS alive — the bug case. Without Path B this would
	// have rendered the red "stuck" warning forever.
	stubAliveSessions(t, map[string]bool{"fleet-abcd1234": true})
	rmCalls := stubRemoveMarker(t)

	records := []*agent.Record{
		{ID: "abcd1234", LastActivityTS: now.Add(-30 * time.Second)}, // fresh
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
		t.Errorf("Path B self-heal must suppress stuck warning when agent record is fresh; got:\n%s", joined)
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

// TestProjectRow_StaleAgentRecordLiveSessionPreservesStuck pins the
// truly-hung case: marker past timeout, agent record exists but
// last_activity_ts is OLD (beyond the freshness window), tmux ALIVE.
// Neither heal path matches — the operator IS looking at a real hang
// and the warning must remain so they triage via tmux.
func TestProjectRow_StaleAgentRecordLiveSessionPreservesStuck(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	markerMtime := now.Add(-15 * time.Minute)
	stubMarkerMtime(t, "demo", markerMtime, true)
	stubMarkerAgentID(t, "demo", "abcd1234")
	stubAliveSessions(t, map[string]bool{"fleet-abcd1234": true})
	rmCalls := stubRemoveMarker(t)

	records := []*agent.Record{
		{ID: "abcd1234", LastActivityTS: now.Add(-1 * time.Hour)}, // stale
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

	if !strings.Contains(joined, "coord spawn stuck") {
		t.Errorf("expected stuck warning when record is stale + session alive; got:\n%s", joined)
	}
	if len(*rmCalls) != 0 {
		t.Errorf("self-heal must NOT fire on truly-hung row; got %d remove calls", len(*rmCalls))
	}
}
