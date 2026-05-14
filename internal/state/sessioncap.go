package state

// Session-count cap config + tmux-session counters.
//
// Backstop for the orphan tmux session leak that OOM-crashed the
// operator's Mac twice (docs/postmortems/2026-05-14-orphan-tmux-leak.md
// action items #5 + #6). The leak plug (Subagent #1) and the atomic
// coord-swap invariant (Subagent #2) prevent NEW leaks. This file's
// job is to refuse unbounded accumulation when (despite both plugs)
// a future bug introduces a similar leak — fleet should fail loud,
// not silently consume RAM.
//
// FLEET_MAX_SESSIONS env var configures the cap (default
// DefaultMaxSessions). Parse failures fall back to the default with a
// stderr warning rather than abort — better to keep fleet usable than
// refuse to start.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// FleetMaxSessionsEnv is the env var that overrides the default cap on
// the total number of tmux sessions whose name starts with "fleet-".
// Read at the start of any operation that would spawn a new session
// (`fleet dispatch`, `fleet handoff`, the auto-drain entry point). When
// the live + orphan count is already at or above the cap, the
// spawn-side caller refuses with an actionable error pointing at the
// `fleet maintenance prune-orphan-tmux` and `fleet rm` escape valves.
const FleetMaxSessionsEnv = "FLEET_MAX_SESSIONS"

// DefaultMaxSessions is the fallback used when FLEET_MAX_SESSIONS is
// unset, unparseable, or non-positive. 30 was chosen as the postmortem-
// derived ceiling: 9 active project coordinators + worker spawn churn
// during multi-task days easily reaches the high teens, so a cap below
// 20 would false-positive on legitimate fleet expansion. 30 leaves
// headroom while still catching the runaway-accumulation case
// (incident peak was 68 sessions before OOM).
const DefaultMaxSessions = 30

// FleetSessionPrefix is the prefix every fleet-managed tmux session
// name uses. Centralized here so the counter, the spawn paths, and
// the maintenance prune helper agree on a single source of truth.
// tmux.SessionName returns "fleet-" + agentID; this is the same
// literal.
const FleetSessionPrefix = "fleet-"

// MaxSessions resolves the effective session cap by reading
// FLEET_MAX_SESSIONS. Falls back to DefaultMaxSessions on:
//
//   - var unset / empty,
//   - non-integer value (e.g. "thirty"),
//   - zero or negative value.
//
// Writes a one-line warning to `warn` (nil tolerated) when the env
// var was set but ignored, so the operator sees the fallback. Never
// returns an error — making fleet refuse to start on a typo'd env var
// would be a worse failure mode than silently defaulting to 30.
//
// Pure function of (env value, warn). No filesystem touch — callers
// can invoke at request time without re-reading os.Environ.
func MaxSessions(warn io.Writer) int {
	raw := os.Getenv(FleetMaxSessionsEnv)
	return resolveMaxSessions(raw, warn)
}

// resolveMaxSessions is the testable core of MaxSessions. Lets tests
// exercise the parse-fallback paths without touching the process env.
func resolveMaxSessions(raw string, warn io.Writer) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DefaultMaxSessions
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		if warn != nil {
			_, _ = fmt.Fprintf(warn,
				"warning: %s=%q is not an integer; falling back to default %d\n",
				FleetMaxSessionsEnv, raw, DefaultMaxSessions)
		}
		return DefaultMaxSessions
	}
	if n <= 0 {
		if warn != nil {
			_, _ = fmt.Fprintf(warn,
				"warning: %s=%d is not positive; falling back to default %d\n",
				FleetMaxSessionsEnv, n, DefaultMaxSessions)
		}
		return DefaultMaxSessions
	}
	return n
}

// SessionCounts holds the breakdown returned by CountFleetSessions.
// `Live` = sessions whose name is "fleet-<id>" and whose state.json
// is present in ~/.fleet/agents/. `Orphan` = sessions whose name is
// "fleet-<id>" and whose state.json is absent (the leak symptom).
// `Total = Live + Orphan`. Sessions whose name doesn't match the
// "fleet-<id>" pattern aren't counted in any bucket — operators may
// keep unrelated tmux sessions on the same server.
type SessionCounts struct {
	Live   int
	Orphan int
}

// Total is the sum of Live + Orphan. Both the cap and the status
// banner threshold compare against Total — orphans count too, because
// they consume RAM and tmux server resources just like live sessions.
func (s SessionCounts) Total() int { return s.Live + s.Orphan }

// LiveAgentRecordExists reports whether ~/.fleet/agents/<id>.json
// exists as a LIVE record (archive copies don't count). Returns true
// on stat errors other than "does not exist" — conservative for any
// caller that uses it as the orphan-detection gate, since a permission
// failure that returned false would mis-classify a live agent as an
// orphan and (in the prune path) kill its session.
//
// Mirrors cmd/fleet/maintenance.agentRecordExists by design — both
// callers MUST agree on the "is this session owned by a live record?"
// answer or the cap counts disagree between the spawn-time check and
// the operator-facing prune command. Centralizing here means a single
// edit at this site propagates to both.
func LiveAgentRecordExists(id string) bool {
	path, err := AgentPath(id)
	if err != nil {
		// Path resolution failure → can't probe; conservative.
		return true
	}
	_, statErr := os.Stat(path)
	switch {
	case statErr == nil:
		return true
	case errors.Is(statErr, os.ErrNotExist):
		return false
	default:
		// Other stat error (permission denied, I/O failure).
		// Conservative: treat as live so we don't classify as orphan.
		return true
	}
}

// CountFleetSessions enumerates tmux sessions via listFn, filters to
// names starting with FleetSessionPrefix, and bins each into the
// Live/Orphan buckets using existsFn. Both fns are injected so callers
// (tests; the CLI prefilter; the status banner; the maintenance
// pruner) can share one counting routine while substituting fakes.
//
// listFn signature mirrors internal/tmux.ListSessions:
//
//	()([]string, error)
//
// existsFn reports whether a live state.json exists for the given
// agent ID. The CLI uses cmd/fleet's `agentRecordExists` (which
// conservatively returns true on stat errors — see its docstring) so
// "unreadable record" doesn't falsely inflate the orphan count.
//
// On listFn failure: returns SessionCounts{} + the error. Callers
// generally treat list errors as "can't enforce cap right now" — log
// and proceed, since refusing to spawn on a transient tmux probe
// failure would be worse than letting one spawn through.
func CountFleetSessions(
	listFn func() ([]string, error),
	existsFn func(id string) bool,
) (SessionCounts, error) {
	sessions, err := listFn()
	if err != nil {
		return SessionCounts{}, err
	}
	var out SessionCounts
	for _, s := range sessions {
		if !strings.HasPrefix(s, FleetSessionPrefix) {
			continue
		}
		id := strings.TrimPrefix(s, FleetSessionPrefix)
		if id == "" {
			// Literal "fleet-" — odd; skip rather than misclassify.
			continue
		}
		if existsFn(id) {
			out.Live++
		} else {
			out.Orphan++
		}
	}
	return out, nil
}
