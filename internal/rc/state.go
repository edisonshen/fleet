package rc

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/edisonshen/fleet/internal/state"
)

// StateFilename is the basename of the per-project rc-state.json.
const StateFilename = "rc-state.json"

// SchemaVersion is the value RecordedState.Schema receives on Write.
// Bump when adding non-backwards-compatible fields.
//
// v2 — leak-rc-daemon-lifecycle PR-B: adds ClaudeVersion + OwningCoordID
// so superseded daemons (across `claude` version upgrades) and orphaned
// daemons (owning coord died) can be recognized as orphans and self-
// healed by Up's idempotent tick. Older v1 state files load with empty
// values; the resolver treats an empty version as "always stale" to
// force one heal cycle which backfills the new fields.
const SchemaVersion = "v2"

// SessionPrefix is the legacy daemon prefix the Claude CLI's
// --remote-control-session-name-prefix accepts. Per the design doc
// (codex round 2), project isolation is via Claude daemon's
// directory-keyed registry (working_dir field below), NOT via prefix
// rewriting; the prefix stays the broad literal "fleet-coord".
const SessionPrefix = "fleet-coord"

// RecordedState is the rc-state.json schema. Written atomically by
// rc.Up; read by rc.Status/Inspect, the sweeper, and the duplicate-
// spawn refusal path.
//
// Fields:
//   - Schema:        version sentinel; bump on incompatible changes.
//   - Project:       safe project name (validated by state.ProjectDir).
//   - PID:           OS pid of the local `claude remote-control` process.
//   - HostID:        os.Hostname() snapshot at Up time. Cross-host
//     adoption is refused (single-host invariant).
//   - WorkingDir:    operator's project cwd at Up time. Used by Claude
//     daemon's directory-keyed registry; future Down /
//     sweep operations key on this value, not a re-derivation.
//   - SessionPrefix: SessionPrefix constant snapshot for forward-compat.
//   - LastSpawnAt:   UTC timestamp of the most recent successful Up.
//   - LastError:     empty on success; populated when Up fails (so
//     subsequent Status invocations can surface the
//     diagnostic without a re-spawn attempt).
type RecordedState struct {
	Schema        string    `json:"schema"`
	Project       string    `json:"project"`
	PID           int       `json:"pid"`
	HostID        string    `json:"host_id"`
	WorkingDir    string    `json:"working_dir"`
	SessionPrefix string    `json:"session_prefix"`
	LastSpawnAt   time.Time `json:"last_spawn_at"`
	LastError     string    `json:"last_error,omitempty"`

	// ClaudeVersion is the `claude --version` output captured at spawn
	// time (e.g. "2.1.156"). Used by Up's self-healing idempotent path
	// to detect a daemon left over from an older claude install — if
	// the recorded version disagrees with the current binary, the old
	// PID is killed and a fresh daemon is spawned. Empty on legacy v1
	// records → treated as "always stale" (one heal cycle backfills it).
	// leak-rc-daemon-lifecycle PR-B (v2 schema bump).
	ClaudeVersion string `json:"claude_version"`

	// OwningCoordID is the agent ID of the coord that called Up. When
	// that agent record disappears (coord crashed, replaced via handoff
	// without proper Down), the daemon becomes an orphan — no live coord
	// drives it. Self-healing Up detects the dead-owner case via a
	// liveness probe and respawns under the new caller. Empty on legacy
	// v1 records or fresh acquires without a CoordID hint.
	// leak-rc-daemon-lifecycle PR-B.
	OwningCoordID string `json:"owning_coord_id"`
}

// ErrStateMissing is returned by ReadState when no rc-state.json
// exists on disk. Callers check errors.Is(err, ErrStateMissing) to
// distinguish absent-state from malformed-state.
var ErrStateMissing = errors.New("rc: rc-state.json absent")

// StatePath returns ~/.fleet/projects/<safe-name>/rc-state.json.
func StatePath(project string) (string, error) {
	dir, err := state.ProjectDir(project)
	if err != nil {
		return "", fmt.Errorf("rc.StatePath: %w", err)
	}
	return filepath.Join(filepath.Clean(dir), StateFilename), nil
}

// WriteState publishes s atomically. The project tree must exist
// (caller's job — state.EnsureProjectInitialized).
func WriteState(s RecordedState) error {
	path, err := StatePath(s.Project)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("rc.WriteState: mkdir parent: %w", err)
	}
	if s.Schema == "" {
		s.Schema = SchemaVersion
	}
	if s.SessionPrefix == "" {
		s.SessionPrefix = SessionPrefix
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("rc.WriteState: marshal: %w", err)
	}
	data = append(data, '\n')
	return state.WriteAtomic(path, data)
}

// ReadState parses rc-state.json for project. Returns ErrStateMissing
// when the file does not exist. Parse errors are returned verbatim so
// hand-edits surface clearly.
func ReadState(project string) (RecordedState, error) {
	path, err := StatePath(project)
	if err != nil {
		return RecordedState{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return RecordedState{}, ErrStateMissing
		}
		return RecordedState{}, fmt.Errorf("rc.ReadState: %w", err)
	}
	var s RecordedState
	if err := json.Unmarshal(data, &s); err != nil {
		return RecordedState{}, fmt.Errorf("rc.ReadState: parse: %w", err)
	}
	return s, nil
}

// RemoveState removes rc-state.json. Idempotent.
func RemoveState(project string) error {
	path, err := StatePath(project)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("rc.RemoveState: %w", err)
	}
	return nil
}
