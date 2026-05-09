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
//	│ marker, elapsed > timeout → coordSpawnStuck  ("▲ stuck…")  │
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

	"github.com/edisonshen/fleet/internal/agent"
)

// coordSpawnCtx bundles the per-render inputs the project block needs
// to decide which spawn-indicator state applies. Threaded through
// projectBlockLines from buildBodyLines once per dashboard frame so a
// single now / spinner-frame is consistent across every project row
// in that frame.
//
// records is the model's cached agent.List() slice (m.records). The
// stuck-marker self-heal path B reads it through isAgentRecordFresh
// to decide whether the agent named in the marker is still ticking —
// in which case the marker is stale and should be removed regardless
// of tmux liveness. Empty / nil is fine: Path B simply won't fire and
// Path A's dead-tmux gate continues to drive the heal.
type coordSpawnCtx struct {
	now          time.Time
	tickFrame    int
	spawnTimeout time.Duration
	records      []*agent.Record
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
//
// Stuck hint uses the marker's agent_id (`fleet-<agentID>`) to point at
// the actual tmux session, not the project name. Issue #96 gap 2: the
// previous text rendered `fleet-<projectName>` which never matched a
// real session (tmux convention from internal/tmux is `fleet-<8hex>`),
// so the hint led the operator nowhere. We accept the agent ID through
// the call (caller already reads the marker for the self-heal probe),
// falling back to the project-name framing only when the read returned
// empty — that "no agent ID found" branch is rare (concurrent marker
// rewrite during render) and softens the wording so the operator
// understands why the hint isn't pointing at a specific session.
func renderCoordSpawnLineForProject(
	st coordSpawnState,
	prefix string,
	projectName string,
	markerAgentID string,
	now time.Time,
	markerMtime time.Time,
	tickFrame int,
) (string, bool) {
	if st == coordSpawnStuck {
		var msg string
		if markerAgentID != "" {
			msg = "▲ coord spawn stuck — check tmux session fleet-" + markerAgentID
		} else {
			// Marker is on disk (we wouldn't be in Stuck otherwise) but
			// its body read returned empty — likely a concurrent rewrite
			// or a hand-edited zero-byte file. Fall back to the project
			// shape with softer wording so the operator knows the agent
			// ID couldn't be resolved.
			msg = "▲ coord spawn stuck for project " + projectName + " — no agent ID in marker; check `tmux ls`"
		}
		body := attentionChipStyle.Render(msg)
		return prefix + body, true
	}
	return renderCoordSpawnLine(st, prefix, now, markerMtime, tickFrame)
}

// applyStuckSelfHeal is the issue #96 gap 1 + follow-up self-heal gate.
// When derivation flagged the row as Stuck but evidence on disk says
// the spawn either died silently OR already succeeded, the warning is a
// lie — keeping it forever just trains operators to ignore the chip.
// Transition Stuck → Idle and remove the stale marker via removeMarker
// so the next render snaps back to the existing renderer.
//
// Two heal paths cover the two ways "marker says stuck but the row
// isn't really stuck" can happen:
//
//	Path A (issue #96): tmux session for the marker's agent ID is
//	  definitively gone → spawn died silently → heal. (sessionAlive=false)
//
//	Path B (follow-up): the agent record at ~/.fleet/agents/<id>.json
//	  has a fresh last_activity_ts → spawn succeeded, the agent is
//	  ticking, the marker just never got cleared → heal regardless of
//	  current tmux liveness. (agentRecordFresh=true)
//
// Both paths are independently sufficient. Either one being true causes
// the marker to be removed and the state to flip to Idle. This deliberate
// OR keeps Path B from regressing on tmux probe failures and keeps Path A
// working when the agent record was never written (silent spawn death).
//
// Inputs are pure: caller supplies the derived state, the agent ID
// from `state.ReadCoordSpawnMarker` (empty when no body / unreadable),
// the tmux liveness probe (`tmux.HasSession`), the agent-record-fresh
// probe (computed from the loaded record's last_activity_ts), and a
// remover stub. The remover is invoked exactly once per healed row;
// errors are returned so the caller can surface them in a flash but
// the healed state is still applied (the warning silently lingering
// is the worse failure mode).
//
// Self-heal does NOT fire when:
//   - state ≠ Stuck (Idle/Spawning/Active are unaffected),
//   - markerAgentID is empty (we can't probe a session OR a record we
//     don't know the name of — fall back to the existing Stuck warning
//     so the operator still sees something is wrong),
//   - neither heal path matches: sessionAlive=true AND agentRecordFresh=
//     false. That's the genuinely-hung case (live tmux, no fresh agent
//     ticks) the original Stuck warning is designed for.
//
// Returns the (possibly healed) state and any removal error. The error
// is informational; callers may log via flash but should still render
// using the returned state.
func applyStuckSelfHeal(
	st coordSpawnState,
	markerAgentID string,
	sessionAlive bool,
	agentRecordFresh bool,
	removeMarker func() error,
) (coordSpawnState, error) {
	if st != coordSpawnStuck {
		return st, nil
	}
	if markerAgentID == "" {
		return st, nil
	}
	// Heal if EITHER path matches. Path A: dead session. Path B: fresh
	// agent record (spawn succeeded, marker stale). When neither
	// condition holds, the operator IS looking at a real hang.
	if sessionAlive && !agentRecordFresh {
		return st, nil
	}
	var rmErr error
	if removeMarker != nil {
		rmErr = removeMarker()
	}
	return coordSpawnIdle, rmErr
}

// agentRecordFreshWindow is the freshness window used by Path B of
// applyStuckSelfHeal: an agent record's last_activity_ts must be within
// this window for the marker to be considered stale-but-the-spawn-
// succeeded. Set to 2× coordActiveWindow so it tolerates one missed
// fleet-guard heartbeat (the standard "Stop hook fired but the agent's
// idle between turns" case) without papering over a truly-dead coord
// whose record file lingers because nothing cleaned it up.
//
// Tuning rationale: coordActiveWindow=5m is the existing "● active"
// gate. Doubling to 10m gives one round-trip of slack — at 11+ minutes
// of silence we'd rather show the warning than silently swallow it,
// because the spawn really might be wedged. The constant is exported
// only via callers; tests pass an explicit window so the value can be
// pinned without touching production.
const agentRecordFreshWindow = 2 * coordActiveWindow

// isAgentRecordFresh reports whether the agent-record matching id has a
// last_activity_ts within window of now. Returns false when records is
// nil/empty, when no record matches the id, or when the matching record
// has a zero last_activity_ts (legacy / partial write — degrade safely
// to "not fresh" so we don't heal off a record we can't trust).
//
// records is the same slice the model already loads via loadAgentsCmd
// (m.records); the caller threads it through coordSpawnCtx so this stays
// off the os.ReadFile path on every render frame.
func isAgentRecordFresh(records []*agent.Record, id string, now time.Time, window time.Duration) bool {
	if id == "" || len(records) == 0 {
		return false
	}
	for _, r := range records {
		if r == nil || r.ID != id {
			continue
		}
		if r.LastActivityTS.IsZero() {
			return false
		}
		return now.Sub(r.LastActivityTS) <= window
	}
	return false
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
