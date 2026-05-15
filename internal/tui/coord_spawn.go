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
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/workers"
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

// coordSpawnState enumerates the indicator states the project row can
// be in with respect to the coord-spawn marker: Idle, Spawning, Active,
// Stuck, Waiting (PART 2b downgrade), and NeverTicked (Path C
// post-suppression hint). Naming mirrors the spec language in issue #86
// to keep the test names readable.
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

	// coordSpawnWaiting: marker mtime is older than spawnTimeout BUT the
	// agent record has needs_input=true — the coord is alive and waiting
	// on operator input. fleet-guard sets needs_input from INSIDE claude
	// on each Stop hook fire, which is impossible if claude were dead.
	// Render informational ("◌ coord waiting on input — fleet attach
	// <agent_id>"), not the attention-chip stuck warning.
	//
	// Reserved for the narrow case where stuck WOULD have fired but
	// needs_input rescues. The derivation function does NOT return this
	// state directly — the render-side wrapper downgrades stuck → waiting
	// after checking the agent record. Keeping the gate at the render
	// layer means the derivation stays pure (marker mtime + state mtime
	// only) and the needs_input lookup is local to the project block
	// that already reads ctx.records.
	coordSpawnWaiting

	// coordSpawnNeverTicked: the spawn succeeded (tmux session alive AND
	// the agent record was written to ~/.fleet/agents/<id>.json) but
	// coord-state.json was never published under this agent's spawn —
	// i.e. last tick predates the agent's spawned_at, or no coord-state
	// ever existed, AND we're past the cold-start grace window. This is
	// a softer informational hint pointing at "Stop hook or /coordinator
	// skill registration may be broken" — distinct from the "▲ coord
	// spawn stuck" attention chip, which falsely accused live coords of
	// being hung (issue: 2026-05-14 false positive on fleet-49bc8a03 in
	// HANDOFF state at 6% context).
	//
	// The render-side gate constructs this state AFTER Path C self-heal
	// has cleared the marker. Pure derivation (deriveCoordSpawnState)
	// stays scoped to marker/state mtimes; this state surfaces only via
	// the project-block path that already loads ctx.records.
	coordSpawnNeverTicked
)

// coordSpawnTimeoutDefault is the default age past which a spawn is
// declared "stuck". 10 minutes is generous: cold starts on a fresh
// laptop top out around 3-5 minutes (issue #86), so 10× that is well
// past any healthy boot.
const coordSpawnTimeoutDefault = 10 * time.Minute

// minNeverTickedGrace is the floor on the cold-start grace window
// used by the soft "spawned but never ticked" hint. Operators can
// lower FLEET_COORD_SPAWN_TIMEOUT_S to a tight value (60-120s) to
// make the red stuck chip fire sooner without also forcing the
// never-ticked diagnostic ("Stop hook or skill registration may be
// broken") to fire during a perfectly healthy 3-5 minute cold start.
// 5 minutes covers the upper end of healthy boot per issue #86 spec.
// Codex iter-17 P2 drove this floor.
const minNeverTickedGrace = 5 * time.Minute

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
	// Liveness wins over spawn-timeout (PART 2.5 fix). A coord whose
	// coord-state.json mtime is within activeWindow is actively ticking
	// — by definition, NOT stuck. The marker is set ONCE at spawn and
	// never refreshed, so without this ordering EVERY long-lived alive
	// coord would eventually trip the timeout and render as stuck (real
	// bug surfaced 2026-05-13: coord de3e12a9 spawned 20:26 PDT, claude
	// alive 20+ min, badge fired at 20:47 PDT despite a 6-min-stale
	// coord-state mtime within the 5m activeWindow window when checked).
	//
	// Reserve "stuck" for the narrow case the spec actually targets:
	// marker exists but no fresh state ever arrived (cold-start wedge),
	// OR marker is past timeout AND state went stale alongside it
	// (genuinely dead). Both of those are caught by the timeout branch
	// below, which only runs when the active gate did NOT match.
	if coordStateOK && now.Sub(coordStateMtime) <= activeWindow {
		return coordSpawnActive
	}
	// Stuck: marker is older than the timeout AND we have no liveness
	// signal from coord-state.json. The operator's [a] launch never
	// converged (or converged and then went silent past the window);
	// resuming it at minute 11+ isn't useful — they should attach via
	// tmux. Spec section: "Marker AND elapsed > FLEET_COORD_SPAWN_TIMEOUT_S".
	if now.Sub(markerMtime) > spawnTimeout {
		return coordSpawnStuck
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
	switch st {
	case coordSpawnStuck:
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
	case coordSpawnWaiting:
		// Informational: the coord is alive, just waiting on operator
		// input. Use dim styling (matches the spawning line treatment)
		// rather than the attention-chip red so the operator's eye
		// isn't pulled to it — they already know they need to answer.
		var msg string
		if markerAgentID != "" {
			msg = "◌ coord waiting on input — fleet attach " + markerAgentID
		} else {
			msg = "◌ coord waiting on input for project " + projectName + " — check `tmux ls`"
		}
		body := dimStyle.Render(msg)
		return prefix + body, true
	case coordSpawnNeverTicked:
		// Informational hint, not an attention chip. Path C self-heal
		// proved the spawn succeeded; this line tells the operator the
		// /coordinator skill hasn't published a tick under THIS agent
		// (could indicate Stop hook misconfig, /coordinator skill not
		// registered, or HOME-pollution leaking into the tmux server —
		// see project_fleet_home_leak memory). Dim styling so the
		// operator's eye isn't dragged here; the agent appears in the
		// regular agent list and they can attach via `fleet attach`.
		var msg string
		if markerAgentID != "" {
			msg = "○ coord " + markerAgentID + " spawned but never ticked — Stop hook or skill registration may be broken"
		} else {
			msg = "○ coord for project " + projectName + " spawned but never ticked — Stop hook or skill registration may be broken"
		}
		body := dimStyle.Render(msg)
		return prefix + body, true
	}
	return renderCoordSpawnLine(st, prefix, now, markerMtime, tickFrame)
}

// downgradeStuckOnNeedsInput converts coordSpawnStuck → coordSpawnWaiting
// when the agent record matching markerAgentID has needs_input=true AND
// the record is fresh (last_activity_ts within agentRecordFreshWindow).
//
// PART 2b of the bundled liveness-correctness fix (2026-05-13): an
// agent with needs_input=true is alive — fleet-guard sets that flag
// from INSIDE claude on every Stop hook fire. The stuck-attention-chip
// path is reserved for the "spawn never converged" failure mode where
// no needs_input signal exists.
//
// Codex iter-4 hardening (2026-05-14): the needs_input flag is only
// as fresh as the last Stop hook write. If claude asked a question
// then died while the tmux wrapper survived, the stale flag would
// mask a dead coord. Gate the downgrade on last_activity_ts being
// within agentRecordFreshWindow (same window applyStuckSelfHeal uses
// for Path B). A stale-flag-from-dead-claude case falls through to
// the original stuck warning, surfacing recovery to the operator.
//
// Returns the (possibly downgraded) state. No effect on non-stuck
// states, when no matching record is found, or when the record's
// last_activity_ts is older than the freshness window.
func downgradeStuckOnNeedsInput(
	st coordSpawnState,
	records []*agent.Record,
	markerAgentID string,
	now time.Time,
	freshWindow time.Duration,
) coordSpawnState {
	if st != coordSpawnStuck || markerAgentID == "" {
		return st
	}
	for _, r := range records {
		if r == nil || r.ID != markerAgentID {
			continue
		}
		if !r.NeedsInput {
			return st
		}
		// needs_input=true AND last_activity_ts fresh → trust the flag.
		// last_activity_ts is stamped on every Stop hook fire; a stale
		// timestamp means fleet-guard hasn't run recently — the agent
		// may have died with the flag set.
		if r.LastActivityTS.IsZero() {
			return st
		}
		if now.Sub(r.LastActivityTS) > freshWindow {
			return st
		}
		return coordSpawnWaiting
	}
	return st
}

// applyStuckSelfHeal is the issue #96 gap 1 + follow-up self-heal gate.
// When derivation flagged the row as Stuck but evidence on disk says
// the spawn either died silently OR already succeeded AND state has
// been published, the warning is a lie — keeping it forever just trains
// operators to ignore the chip. Transition Stuck → Idle and remove the
// stale marker via removeMarker so the next render snaps back to the
// existing renderer.
//
// Two heal paths preserve the contract that the marker is removed only
// when re-attach is either impossible (Path A) OR rescued by another
// authoritative signal (Path B — coord-state.json + lock body published
// by the ticking agent):
//
//	Path A (issue #96): tmux session for the marker's agent ID is
//	  definitively gone → spawn died silently → heal + remove marker.
//	  (sessionAlive=false; the operator cannot re-attach to a dead
//	  session, so destroying the marker loses no useful state.)
//
//	Path B (follow-up): the agent record at ~/.fleet/agents/<id>.json
//	  has a fresh last_activity_ts → spawn succeeded, the agent is
//	  ticking, AND fleet-guard's per-tick writes imply coord-state.json
//	  publication AND coordinator.lock body publication → heal +
//	  remove marker. findCoordByLockBody rescues the subsequent [a]
//	  re-attach. (agentRecordFresh=true)
//
// Path C (alive but never ticked — e.g. HANDOFF state, freshly-booted,
// Stop-hook misconfig) is NOT handled here. The marker is the ONLY
// proof of which agent is coord for the project in that state (no lock
// body exists yet), so removing it breaks [a] re-attach in
// findExistingCoordForProject AND findCoordByLockBody both. See
// renderCoordSpawnLineForProject's caller in dashboard_view.go for
// the Path C suppression: it downgrades Stuck → NeverTicked at render
// time without touching the marker file. The marker stays so the
// operator can press [a] and re-enter the live session.
//
// Both Path A and Path B are independently sufficient. Either being
// true causes the marker to be removed and the state to flip to Idle.
// The deliberate OR keeps Path B from regressing on tmux probe failures
// and keeps Path A working when the agent record was never written
// (silent spawn death).
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
//   - neither A nor B matches: sessionAlive=true AND agentRecordFresh=
//     false. That's the genuinely-hung case (live tmux, no fresh agent
//     ticks) the original Stuck warning is designed for. Path C handles
//     the subset where an agent record exists but is stale (i.e. the
//     spawn pipeline registered the agent but no Stop-hook tick has
//     fired yet) at the render layer.
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
	// Heal if EITHER A or B matches. Path A: dead session. Path B: fresh
	// agent record (spawn succeeded, lock body presumed published).
	// Path C is intentionally NOT collapsed here — see docstring; it
	// must NOT remove the marker, so its handling lives in the caller.
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

// agentRecordExists reports whether records contains a non-nil entry
// matching id. Drives Path C of applyStuckSelfHeal: a marker that names
// an agent ID for which fleet wrote a record at ~/.fleet/agents/<id>.json
// is proof the spawn pipeline completed, regardless of the record's
// freshness. The Path A / B probes already gate on more specific signals
// (dead tmux, fresh tick); Path C is the catch-all for "spawn succeeded
// but the agent isn't currently ticking" (HANDOFF state, just-spawned,
// Stop hook misconfigured) where the stuck-attention chip is the wrong
// signal to give the operator.
//
// Empty id short-circuits to false (we can't match a record we don't
// know the name of). Nil entries in the slice are tolerated.
func agentRecordExists(records []*agent.Record, id string) bool {
	if id == "" {
		return false
	}
	for _, r := range records {
		if r == nil || r.ID != id {
			continue
		}
		return true
	}
	return false
}

// agentProcessAliveFn returns true if the OS process with the given
// PID is alive. Production points at workers.IsAlive (kill(pid, 0)
// check); tests swap it to a deterministic map. Path C uses this to
// distinguish "Claude alive, idle between Stop-hook fires" (suppress
// red chip) from "tmux wrapper survived after Claude exited" (keep
// red chip — operator-visible failure).
//
// var for stub-ability — same pattern as sessionAliveFn /
// coordSpawnMarkerFn elsewhere in this file. PID ≤ 0 is treated as
// "no PID recorded" → false (we can't confirm liveness without a PID,
// default to "treat as dead" so the stuck warning surfaces).
var agentProcessAliveFn = func(pid int) bool {
	if pid <= 0 {
		return false
	}
	return workersIsAlive(pid)
}

// agentProcessArgvFn returns the argv of the process with the given
// PID, or empty string when the lookup fails or the process is dead.
// Production runs `ps -p <pid> -o args=` (~10ms per call on macOS);
// tests swap it to a deterministic map.
//
// Path C uses this to detect the wrapper-pane-shell case: when
// spawn.Spawn's resolveEnginePid fell back to recording the tmux
// pane's wrapper shell PID (typically `sh -c "...claude..."`), the
// shell stays alive after Claude exits — kill(0) cannot tell us
// Claude died. argv inspection identifies the shell-wrapper case
// directly and keeps the stuck warning visible (codex iter-16 P1).
//
// Mirror of skills/fleet-guard/health.py:_recorded_pid_looks_like_wrapper
// but lives in Go for the dashboard's single-process render path.
var agentProcessArgvFn = func(pid int) string {
	if pid <= 0 {
		return ""
	}
	// ps is POSIX-portable; -o args= prints only the argv without a
	// header. Five-second timeout matches the Python helper. Errors
	// (transient ps blip, EPERM) return "" → treat as "can't tell"
	// at the caller; the caller defaults to "alive" so a probe blip
	// does not flip a healthy row to stuck.
	cmd := exec.Command("ps", "-o", "args=", "-p", strconv.Itoa(pid))
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// shellBasenames lists the POSIX shell binary basenames that indicate
// a wrapper shell rather than an engine process. We match by basename
// (the trailing path component of argv[0]) rather than a hardcoded
// list of full paths because the fleet wrapper exits into
// `exec ${SHELL:-bash} -i`, which on common installs (Homebrew, Nix,
// asdf, custom prefix, etc.) lives at paths like
// /opt/homebrew/bin/bash or /nix/store/.../bin/zsh that no fixed
// allowlist could cover (codex iter-17 P1).
//
// Mirror of skills/fleet-guard/health.py:_is_shell_argv intent.
var shellBasenames = map[string]bool{
	"sh":    true,
	"bash":  true,
	"dash":  true,
	"zsh":   true,
	"ksh":   true,
	"fish":  true,
	"tcsh":  true,
	"csh":   true,
	"mksh":  true,
	"yash":  true,
	"posh":  true,
	"ash":   true,
	"oksh":  true,
	"loksh": true,
}

// argvLooksLikeShellWrapper reports whether argv's first token's
// basename is a known POSIX shell binary. Empty argv returns false —
// the caller's "can't tell" default is "treat as alive" which means we
// do NOT flip a real coord to stuck on a transient ps probe failure.
//
// Basename-based matching (codex iter-17 P1): handles non-system
// shell paths (e.g. /opt/homebrew/bin/bash, /nix/store/.../bin/zsh)
// that a hardcoded path allowlist could not cover.
func argvLooksLikeShellWrapper(argv string) bool {
	if argv == "" {
		return false
	}
	first := argv
	if idx := strings.IndexAny(argv, " \t"); idx > 0 {
		first = argv[:idx]
	}
	// Strip any leading path component to get the basename.
	if idx := strings.LastIndex(first, "/"); idx >= 0 {
		first = first[idx+1:]
	}
	return shellBasenames[first]
}

// workersIsAlive is a thin indirection over workers.IsAlive so the
// tui package can test agentProcessAliveFn's behavior without taking
// a direct dependency on the workers package's exported helper. The
// dependency is one-way (tui → workers) and the indirection here is
// purely for the test stub seam.
var workersIsAlive = workers.IsAlive

// agentNeedsInput reports whether the agent record matching id has
// needs_input=true. Thin lookup helper distinct from
// downgradeStuckOnNeedsInput (which couples the flag check with the
// freshness window). The render-layer second-chance downgrade in
// projectFooterLines pairs this with agentClaudeAlive to trust the
// flag even when last_activity_ts is stale — fleet-guard only clears
// needs_input on UserPromptSubmit, so a stale-but-live coord that's
// been waiting > 10min remains the right operator action ("fleet
// attach <id>").
func agentNeedsInput(records []*agent.Record, id string) bool {
	if id == "" || len(records) == 0 {
		return false
	}
	for _, r := range records {
		if r == nil || r.ID != id {
			continue
		}
		return r.NeedsInput
	}
	return false
}

// agentClaudeAlive reports whether the Claude process recorded in the
// agent record matching id is still alive AND not just a wrapper
// shell. Returns false when:
//   - id is empty, records is nil/empty, or no record matches,
//   - the record's PID is ≤ 0 (legacy / partial write — degrade safely),
//   - the PID's process is dead (kill(0) returns ESRCH),
//   - the PID's argv first-token is a POSIX shell binary, indicating
//     spawn.Spawn's resolveEnginePid fell back to the tmux pane's
//     wrapper shell instead of the real Claude process. Codex iter-16
//     P1: kill(0) alone can't distinguish "Claude alive idle" from
//     "wrapper survived Claude exit" because both keep the same shell
//     PID alive. argv inspection identifies the wrapper case so the
//     red stuck chip stays visible for real spawn failures.
//
// Path C uses this to distinguish:
//   - Claude alive, idle between Stop-hook fires (HANDOFF state) →
//     argv contains "claude", kill(0) true → suppress red chip
//   - Wrapper survived after Claude exited at startup → argv is
//     "sh -c ..." or similar, kill(0) true on shell → keep red chip
//
// Cost: one kill(0) (~1μs) + one `ps -o args= -p <pid>` (~10ms on
// macOS) per render per project. Probe failures (transient ps blip,
// EPERM, kernel hiccup) return "" from agentProcessArgvFn, which
// argvLooksLikeShellWrapper treats as "not a wrapper" — so we
// default to "alive" on probe failure rather than flipping a healthy
// row to stuck. Stub-overridable via agentProcessAliveFn AND
// agentProcessArgvFn for tests.
func agentClaudeAlive(records []*agent.Record, id string) bool {
	if id == "" || len(records) == 0 {
		return false
	}
	for _, r := range records {
		if r == nil || r.ID != id {
			continue
		}
		if !agentProcessAliveFn(r.PID) {
			return false
		}
		// PID is alive — verify it's not the wrapper shell. argv
		// probe failures are conservative ("" → not-wrapper → trust
		// kill(0)) so a flaky ps doesn't flip working coords to stuck.
		if argvLooksLikeShellWrapper(agentProcessArgvFn(r.PID)) {
			return false
		}
		return true
	}
	return false
}

// agentEverActivated reports whether the agent record for id has
// fired at least one fleet-guard Stop hook since spawn. agent.New()
// initializes last_activity_ts == spawned_at (same time.Now() call);
// the Stop hook is the only thing that advances last_activity_ts, and
// the hook runs from INSIDE Claude on every assistant turn. So
// last_activity_ts strictly NOT-EQUAL to spawned_at proves a Stop
// hook fired at least once, which proves Claude was alive after spawn.
//
// Used by Path C to distinguish:
//   - "Claude alive, idle between Stop-hook fires" (HANDOFF state,
//     waiting on operator) → last_activity_ts != spawned_at → activated
//     → Path C may suppress the red stuck chip
//   - "tmux wrapper survived but Claude died at startup before any
//     Stop-hook fired" → last_activity_ts == spawned_at (same nanosec
//     value from agent.New()) → NOT activated → keep the red stuck
//     chip so the operator notices
//
// Inequality (not "strictly after") is the right comparison because
// skills/fleet-guard/health.py writes last_activity_ts with whole-
// second RFC 3339 precision while agent.New() preserves nanoseconds.
// A first Stop hook that fires in the same wall-clock second as spawn
// will write last_activity_ts = "12:00:00Z" while spawned_at remains
// "12:00:00.123456789Z" — the hook timestamp compares LESS than spawn
// by up to ~999ms (codex iter-7 P1 fix). Strict after-comparison
// would misclassify that fast-Stop-hook case as "not activated."
// Inequality catches both the fast-same-second case and any later
// second; the only thing it excludes is the exact-equal agent.New()
// shape, which is what we want.
//
// Returns false on the obvious non-applicable cases (empty id, no
// matching record, zero spawned_at, zero last_activity_ts).
func agentEverActivated(records []*agent.Record, id string) bool {
	if id == "" || len(records) == 0 {
		return false
	}
	for _, r := range records {
		if r == nil || r.ID != id {
			continue
		}
		if r.SpawnedAt.IsZero() || r.LastActivityTS.IsZero() {
			return false
		}
		return !r.LastActivityTS.Equal(r.SpawnedAt)
	}
	return false
}

// coordNeverTickedThisSpawn reports whether the coord-state.json
// publication has NOT yet caught up to the agent's spawned_at. Returns
// true when:
//   - the agent record exists for id (Path C only makes sense with a
//     record on disk), AND
//   - lastTick is zero (no coord-state.json ever) OR predates
//     spawned_at by more than coordTickMtimeTolerance (a leftover state
//     file from a previous coord under this project, with slack for
//     filesystem mtime resolution). Legacy records with zero
//     spawned_at also count as never-ticked — we can't prove a lock
//     body / state file exists under this spawn without spawned_at,
//     and defaulting to "never ticked" preserves the marker so [a]
//     re-attach still works (codex iter-9 P2: don't allow Path B
//     marker removal on a record we can't reason about).
//
// Used by the render-layer Path C suppression AND by Path A/B's marker-
// removal gate to distinguish "alive coord that has never ticked under
// this spawn" (false-positive stuck; preserve marker for re-attach)
// from "alive coord that ticked successfully then wedged" (genuine
// stuck — keep the warning so the operator triages via tmux). Without
// the narrower check Path C would demote every live tmux session to
// Idle even when LastTick proves the coord successfully booted (codex
// iter-2 P1), and Path B would delete the marker before the operator
// could re-attach a never-ticked-but-otherwise-alive coord (codex
// iter-5 P1).
//
// Distinct from agentNeverTickedSinceSpawn, which adds a cold-start
// grace window before promoting Idle → NeverTicked for the
// informational hint. The Path C / Path B gate has no grace window
// (whether the coord has ticked is a binary fact regardless of age),
// only the mtime-resolution slack.
func coordNeverTickedThisSpawn(
	records []*agent.Record,
	id string,
	lastTick time.Time,
) bool {
	if id == "" || len(records) == 0 {
		return false
	}
	for _, r := range records {
		if r == nil || r.ID != id {
			continue
		}
		if r.SpawnedAt.IsZero() {
			// Legacy / partial-write record without spawned_at. We
			// can't prove the coord ticked under this spawn (no
			// reference point). Codex iter-15 P2 split:
			//   - lastTick non-zero → some coord-state.json exists on
			//     disk. Could be from this spawn OR a previous one;
			//     without SpawnedAt we cannot tell. Conservative:
			//     assume "ticked" so Path C doesn't falsely render
			//     "never ticked" / hide a real wedge.
			//   - lastTick zero → no state file at all, no reference
			//     point. Default to "never ticked" so the marker is
			//     preserved (codex iter-9 P2) — losing the marker
			//     breaks [a] re-attach for legacy installs.
			return lastTick.IsZero()
		}
		if lastTick.IsZero() {
			return true
		}
		// Strict comparison: lastTick BEFORE spawned_at means the tick
		// is from a PREVIOUS coord (the coord-state.json file is
		// project-scoped, so a leftover tick from the prior coord
		// outlives that coord's lifecycle). We do NOT add a negative
		// tolerance here — codex iter-14 P2: a 1-second gap between
		// the old coord's last tick and the new coord's spawn was
		// being misclassified as "the new coord ticked," letting Path
		// B remove the marker without proof of publication under this
		// spawn. Same-second first-tick from THIS spawn is essentially
		// impossible in production (cold-start takes seconds; first
		// Stop hook needs ≥1 assistant turn), so dropping the
		// tolerance has no realistic miss.
		return lastTick.Before(r.SpawnedAt)
	}
	return false
}

// agentNeverTickedSinceSpawn reports whether a softer "spawned but never
// ticked" hint should render after Path C self-heal. Returns true when
// the agent record for id exists AND either (a) spawned_at is zero
// (legacy record — we can't measure grace, but the marker has aged
// past spawnTimeout so the operator has waited long enough; codex
// iter-10 P2) or (b) spawned_at is non-zero, older than graceWindow,
// AND the last coord-state.json tick (lastTick) either is zero (no
// state file ever) or predates spawned_at (a stale state from a
// previous coord under this project).
//
// graceWindow is the cold-start budget: cold starts on a fresh laptop
// top out around 3-5min, so the caller passes coordSpawnTimeoutDefault
// (10m) in production — beyond that window, "the coord never ticked
// under this spawn" is informational rather than alarming, and points
// the operator at /coordinator skill registration or Stop hook config.
//
// Returns false on the non-applicable cases:
//   - empty id, nil/empty records, no matching record,
//   - non-zero spawned_at within the grace window (cold start may still complete),
//   - non-zero spawned_at AND lastTick within mtime tolerance of (or
//     after) spawned_at (a tick already arrived under this agent —
//     no longer "never ticked").
func agentNeverTickedSinceSpawn(
	records []*agent.Record,
	id string,
	lastTick time.Time,
	now time.Time,
	graceWindow time.Duration,
) bool {
	if id == "" || len(records) == 0 {
		return false
	}
	for _, r := range records {
		if r == nil || r.ID != id {
			continue
		}
		if r.SpawnedAt.IsZero() {
			// Legacy record: marker mtime past spawnTimeout AND no
			// state file = operator has waited long enough; surface
			// the diagnostic. Codex iter-10 P2 — without this fallback
			// legacy records render silent after Path C suppression.
			return true
		}
		if now.Sub(r.SpawnedAt) <= graceWindow {
			return false
		}
		// Tick exists AND is on-or-after spawned_at → already ticked.
		// Strict comparison aligned with coordNeverTickedThisSpawn
		// (codex iter-14 P2): no tolerance window because a leftover
		// tick from the previous coord (project-scoped state file)
		// would otherwise be attributed to this spawn.
		if !lastTick.IsZero() && !lastTick.Before(r.SpawnedAt) {
			return false
		}
		return true
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
