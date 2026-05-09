// Coord-spawn indicator — surfaces the 3-5min cold-start window for
// `[a]` auto-spawn so the operator sees feedback while the coord skill
// is booting (issue #86).
//
// Inputs are pure (marker mtime + coord-state.json mtime + now), so
// the state derivation is testable without touching disk; the
// production callsite reuses coordSpawnMarkerMtimeFn (already
// stub-overridable from rows.go) and the same coord-state.json mtime
// the dashboard scanner reads into ProjectRow.LastTick / .Active.
//
// State machine (issue #86 spec):
//
//	┌────────────────────────────────────────────────────────────┐
//	│ no marker        → coordSpawnIdle    (existing render)     │
//	│ marker, fresh                                               │
//	│   coord-state stale → coordSpawnSpawning  ("⠋ spawning…")  │
//	│   coord-state fresh → coordSpawnActive    (existing ● live) │
//	│ marker, elapsed > timeout → coordSpawnStuck  ("⚠ stuck…")  │
//	└────────────────────────────────────────────────────────────┘
//
// "Fresh coord-state.json" reuses coordActiveWindow from dashboard.go
// — same freshness gate the existing scanner uses to flip a row to
// "● active". Keeping the spec single-sourced means a future change
// to that window automatically flows through both renderers.
package tui

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// coordSpawnCtx bundles the per-render inputs the project block needs
// to decide which spawn-indicator state applies. Threaded through
// projectBlockLines from buildBodyLines once per dashboard frame so a
// single now / spinner-frame is consistent across every project row
// in that frame.
type coordSpawnCtx struct {
	now          time.Time
	tickFrame    int
	spawnTimeout time.Duration
}

// coordSpawnState enumerates the four indicator states the project row
// can be in with respect to the coord-spawn marker. Naming mirrors the
// spec language in issue #86 to keep the test names readable.
type coordSpawnState int

const (
	// coordSpawnIdle: no marker on disk. Render exactly what the
	// dashboard rendered before this PR — no spawning line.
	coordSpawnIdle coordSpawnState = iota

	// coordSpawnSpawning: marker exists AND coord-state.json is missing
	// or stale (mtime > coordActiveWindow). The skill is in cold start.
	coordSpawnSpawning

	// coordSpawnActive: marker exists AND coord-state.json is fresh.
	// The existing PR #57 coord-on-left renderer wins; the spawning
	// line is suppressed.
	coordSpawnActive

	// coordSpawnStuck: marker mtime is older than spawnTimeout. We
	// lost confidence the coord skill will ever publish; surface a red
	// warning prompting the operator to attach via tmux.
	coordSpawnStuck
)

// coordSpawnTimeoutDefault is the default age past which a spawn is
// declared "stuck". 10 minutes is generous: cold starts on a fresh
// laptop top out around 3-5 minutes (issue #86), so 10× that is well
// past any healthy boot.
const coordSpawnTimeoutDefault = 10 * time.Minute

// coordSpawnTimeoutEnv lets the operator override the stuck threshold
// without recompiling. Value is parsed once at New() into Model.
const coordSpawnTimeoutEnv = "FLEET_COORD_SPAWN_TIMEOUT_S"

// coordSpawnGlyphs is the braille-dots spinner cycle. Spec calls out
// this exact sequence in issue #86. Indexed by m.tickCount per render
// so the glyph rotates once per pollInterval (1s). 10 frames is enough
// for the eye to see steady motion at 1Hz without feeling frantic.
var coordSpawnGlyphs = []rune{
	'⠋', '⠙', '⠹', '⠸', '⠼', '⠴', '⠦', '⠧', '⠇', '⠏',
}

// deriveCoordSpawnState classifies the indicator state from the
// (marker present, marker mtime, coord-state mtime, now) inputs.
//
// markerOK indicates the marker exists at all; markerMtime is its
// mtime when ok. coordStateOK + coordStateMtime mirror the same shape
// for the coord-state.json mtime read by scanProject. activeWindow is
// passed in (rather than reading the package-level constant) so tests
// can pin both windows deterministically — production threads
// coordActiveWindow + the configured spawnTimeout.
func deriveCoordSpawnState(
	markerOK bool,
	markerMtime time.Time,
	coordStateOK bool,
	coordStateMtime time.Time,
	now time.Time,
	activeWindow, spawnTimeout time.Duration,
) coordSpawnState {
	if !markerOK {
		return coordSpawnIdle
	}
	// Stuck check fires regardless of coord-state freshness — once the
	// marker is older than the timeout, even a "fresh" coord-state
	// could be a different agent (the spec's "stuck" framing assumes
	// the operator's [a] launch never succeeded, and resuming it at
	// minute 11 isn't useful — they should attach via tmux). Spec
	// section: "Marker AND elapsed > FLEET_COORD_SPAWN_TIMEOUT_S".
	if now.Sub(markerMtime) > spawnTimeout {
		return coordSpawnStuck
	}
	// Coord-state fresh → existing PR #57 active rendering wins. The
	// caller suppresses our extra line in this case.
	if coordStateOK && now.Sub(coordStateMtime) <= activeWindow {
		return coordSpawnActive
	}
	// Post-active idle-stop guard: if a coord-state.json exists AND its
	// mtime is newer than the marker, the coord successfully booted at
	// some point under this marker (and then stopped ticking — its
	// state mtime is now stale). That's the existing scanProject
	// IdleStop branch's territory ("○ idle · auto-stopped" on line 3),
	// not a fresh cold start. Returning Idle here suppresses our extra
	// line so the operator doesn't see a contradictory "spawning..."
	// alongside "auto-stopped" on the same row. The Stuck branch above
	// still wins past spawnTimeout; this gate only catches the narrow
	// "marker fresh, state file already published, state file went
	// stale" window.
	if coordStateOK && coordStateMtime.After(markerMtime) {
		return coordSpawnIdle
	}
	return coordSpawnSpawning
}

// formatSpawnElapsed returns a human-readable elapsed-time string for
// the spawning indicator. Distinct from humanAge() because the spec
// requests sub-hour precision in `Nm Ns` form (e.g. "1m 23s") instead
// of `Nm` rounded.
//
//	d <  1s   → "0s"            (avoid "-3s" or empty when d is tiny)
//	d <  60s  → "Ns"            ("23s")
//	d < 3600s → "Mm Ss"         ("1m 23s") — even when seconds == 0
//	d ≥ 1h    → "Hh Mm"         ("1h 4m")  — drop seconds at hour scale
func formatSpawnElapsed(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		m := int(d / time.Minute)
		s := int((d - time.Duration(m)*time.Minute) / time.Second)
		return fmt.Sprintf("%dm %ds", m, s)
	default:
		h := int(d / time.Hour)
		m := int((d - time.Duration(h)*time.Hour) / time.Minute)
		return fmt.Sprintf("%dh %dm", h, m)
	}
}

// coordSpawnSpinnerGlyph picks the spinner glyph for tick frame n.
// Wraps modulo len(coordSpawnGlyphs). Negative tick counts are
// clamped to 0 (defensive — tickCount is uint-shaped in practice but
// reading from an int field can underflow if anyone ever sets it).
func coordSpawnSpinnerGlyph(tickFrame int) rune {
	n := len(coordSpawnGlyphs)
	if n == 0 {
		return ' '
	}
	if tickFrame < 0 {
		tickFrame = 0
	}
	return coordSpawnGlyphs[tickFrame%n]
}

// renderCoordSpawnLine builds the project row's spawning / stuck line
// for the given state. Returns ("", false) when no extra line should
// render (Idle or Active). Caller appends the returned string as a
// new line in the project block when ok=true.
//
// prefix is the row's existing 2-cell indent (or attention border) so
// alignment with the surrounding lines is preserved. tickFrame is the
// current spinner frame (caller passes m.tickCount).
//
// Styling: spawning uses dim/faint text matching "idle" treatment so
// the row reads as "informational, not blocking"; stuck uses
// attentionChipStyle (bold red) matching the broader attention
// palette so the operator's eye is pulled.
func renderCoordSpawnLine(
	st coordSpawnState,
	prefix string,
	now time.Time,
	markerMtime time.Time,
	tickFrame int,
) (string, bool) {
	switch st {
	case coordSpawnSpawning:
		glyph := string(coordSpawnSpinnerGlyph(tickFrame))
		elapsed := formatSpawnElapsed(now.Sub(markerMtime))
		body := dimStyle.Render(glyph+" spawning coord... ") +
			dimStyle.Render(elapsed)
		return prefix + body, true
	case coordSpawnStuck:
		// Stuck warning needs the project name's tmux session hint.
		// Caller holds the project name; we accept it via a separate
		// wrapper (renderCoordSpawnLineForProject). This branch is
		// only reached through the wrapper so we can keep the inputs
		// minimal here.
		return "", false
	}
	return "", false
}

// renderCoordSpawnLineForProject is the project-aware variant: it
// folds the project name into the stuck-line tmux hint and otherwise
// delegates to renderCoordSpawnLine for the spawning case. Split so
// the stateless renderer can be tested in isolation, then composed
// with the projectName-bearing branch up here.
func renderCoordSpawnLineForProject(
	st coordSpawnState,
	prefix string,
	projectName string,
	now time.Time,
	markerMtime time.Time,
	tickFrame int,
) (string, bool) {
	if st == coordSpawnStuck {
		// "fleet-<id>" is the tmux session naming convention from
		// internal/tmux. We don't have the agent ID here (the marker
		// could have been overwritten); pointing at the project-shaped
		// session name gives the operator enough to grep their tmux
		// list. Spec calls out this exact text.
		body := attentionChipStyle.Render("⚠ coord spawn stuck — check tmux session fleet-" + projectName)
		return prefix + body, true
	}
	return renderCoordSpawnLine(st, prefix, now, markerMtime, tickFrame)
}

// resolveCoordSpawnTimeout returns the configured stuck threshold,
// reading FLEET_COORD_SPAWN_TIMEOUT_S when set + non-empty + parses
// to a positive integer. Falls back to coordSpawnTimeoutDefault for
// any failure (empty, unset, non-numeric, ≤ 0). Called once at New().
func resolveCoordSpawnTimeout() time.Duration {
	raw := strings.TrimSpace(os.Getenv(coordSpawnTimeoutEnv))
	if raw == "" {
		return coordSpawnTimeoutDefault
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return coordSpawnTimeoutDefault
	}
	return time.Duration(n) * time.Second
}
