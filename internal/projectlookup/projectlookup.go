// Package projectlookup is the shared coord-discovery helper used by
// cmd/fleet/attach.go (Tier 3 PROJECT RECOVERY) and internal/tui
// ([a] agent-row failover). Extracts logic that previously lived
// inside the TUI as findExistingCoordForProject / findCoordByLockBody
// (PR #181 / #189) so CLI and TUI agree on which coord is "the live
// coord for project X" — without duplicating the rules.
//
// The TUI's per-project attach uses a SUPERSET of this package: it
// also gates [a] dedup on the coord-spawn marker so a failed-prompt
// dispatch re-spawns instead of dropping the operator into a bare
// Claude. Tier 3 has no such gate — by the time PROJECT RECOVERY
// fires, any live coord is acceptable. Keeping that nuance in the
// TUI wrapper rather than this package preserves the failover
// invariant ("never exit").
package projectlookup

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tmux"
)

// sessionAliveFn / sessionProbeFn / listSessionsFn are seams for
// tests; production wires the real tmux helpers. The TUI uses the
// same pattern so unit tests can avoid spawning a tmux server.
var (
	sessionAliveFn = tmux.HasSession
	sessionProbeFn = tmux.SessionAlive
	listSessionsFn = tmux.ListSessions
)

// SetTestStubs lets external test packages (cmd/fleet/attach_failover
// _test.go) replace the tmux seams without exposing the unexported
// vars. Returns a restore func the caller can `defer` to undo. Pass
// nil for any field to leave the existing seam unchanged. Test-only
// — production never calls this.
//
// Without this hook, cmd/fleet's failover tests would have to spin up
// a real tmux server (slow + flaky) or duplicate the projectlookup
// helpers with their own seam plumbing (drift risk). One narrow
// exported hook keeps both packages honest.
func SetTestStubs(alive func(string) bool, probe func(string) (bool, error), list func() ([]string, error)) (restore func()) {
	prevAlive := sessionAliveFn
	prevProbe := sessionProbeFn
	prevList := listSessionsFn
	if alive != nil {
		sessionAliveFn = alive
	}
	if probe != nil {
		sessionProbeFn = probe
	}
	if list != nil {
		listSessionsFn = list
	}
	return func() {
		sessionAliveFn = prevAlive
		sessionProbeFn = prevProbe
		listSessionsFn = prevList
	}
}

// CoordTaskID returns the canonical task_id for a project's coord
// agent. Centralized here so callers don't reinvent the prefix.
//
// Project names are validated upstream via state.ValidateProjectName,
// so this never embeds a path-unsafe component.
func CoordTaskID(projectName string) string {
	return "coord-" + projectName
}

// KnownProjects enumerates ~/.fleet/projects/<name>/ alphabetically.
// Filters out reserved directories (.locks, anything starting with
// `.`) and names that fail state.ValidateProjectName (e.g. a
// "--project" dir from a CLI flag-misparse — see invalid-project-dir-
// guar-d636). Returns ([], nil) when no project dir exists yet (fresh
// install). Matches scanDashboard's filtering so picker output and
// dashboard rows stay in lockstep.
func KnownProjects() ([]string, error) {
	root, err := state.Root()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(root, "projects")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if name == ".locks" || strings.HasPrefix(name, ".") {
			continue
		}
		if err := state.ValidateProjectName(name); err != nil {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// FindLiveCoord returns the first record tagged as the coord for
// projectName whose tmux session is alive. "Tagged" means
// task_id == coord-<project> AND project == <project>. "Alive" uses
// the tristate probe so a tmux transport hiccup doesn't drop a live
// claim — same conservative discipline the TUI uses (codex iter-6 P2).
//
// Differs intentionally from the TUI's findExistingCoordForProject:
// no coord-spawn-marker requirement here. Tier 3 PROJECT RECOVERY is
// failover, not dedup — any live coord for the project is acceptable.
// The TUI's [a]-dedup helper layers the marker gate on top of this.
func FindLiveCoord(records []*agent.Record, projectName string) (*agent.Record, bool) {
	want := CoordTaskID(projectName)
	for _, r := range records {
		if r == nil || r.TaskID != want || r.Project != projectName {
			continue
		}
		session := r.TmuxSession
		if session == "" {
			session = tmux.SessionName(r.ID)
		}
		if !sessionAliveOrProbe(session) {
			continue
		}
		return r, true
	}
	return nil, false
}

// FindCoordByLockBody returns the alive agent whose ID is the body
// of ~/.fleet/projects/<projectName>/.locks/coordinator.lock. The
// body is set via LOCK_EX in the coord skill's _try_lock, so presence
// of an ID there means a coord successfully acquired the lock at
// least once. flock(2) does not truncate on release, so the body can
// outlive its writer — gate on the matching agent's tmux session
// being alive to catch "coord crashed but lock body remains".
//
// Returns (nil, false) when the lock body is missing/empty/malformed,
// when no record has the matching ID, or when the matching record's
// tmux session is definitively dead.
func FindCoordByLockBody(records []*agent.Record, projectName string) (*agent.Record, bool) {
	root, err := state.Root()
	if err != nil {
		return nil, false
	}
	holderID := readCoordHolder(filepath.Join(root, "projects"), projectName)
	if holderID == "" {
		return nil, false
	}
	for _, r := range records {
		if r == nil || r.ID != holderID {
			continue
		}
		session := r.TmuxSession
		if session == "" {
			session = tmux.SessionName(r.ID)
		}
		if !sessionAliveOrProbe(session) {
			continue
		}
		return r, true
	}
	return nil, false
}

// StaleCoordRecord returns the first record tagged as the coord for
// projectName whose tmux session is DEFINITIVELY dead (probe returns
// alive=false with no error). Drives the Tier 3 "record alive but
// tmux missing" branch — pass the returned ID to
// `fleet gc --apply --aggressive --project <p>` before respawning.
//
// Definitive-dead semantics (not "any false from HasSession"): a
// transport error from tmux means the probe is ambiguous; we must
// NOT mark a live coord stale just because the socket hiccupped.
// Matches the discipline in feedback_fleet_owns_its_resources.md.
func StaleCoordRecord(records []*agent.Record, projectName string) (*agent.Record, bool) {
	want := CoordTaskID(projectName)
	for _, r := range records {
		if r == nil || r.TaskID != want || r.Project != projectName {
			continue
		}
		session := r.TmuxSession
		if session == "" {
			session = tmux.SessionName(r.ID)
		}
		alive, err := sessionProbeFn(session)
		if err != nil {
			continue // transport error — don't classify as stale
		}
		if alive {
			continue
		}
		return r, true
	}
	return nil, false
}

// OrphanTmuxForProject scans live tmux sessions for fleet-<id>
// patterns and returns the first <id> with NO matching agent record.
// Drives the Tier 3 "no record but lingering tmux session" branch —
// pass the returned ID into the gc reap path before respawning.
//
// projectName is hint-only today: tmux session names don't carry the
// project, so the helper returns the first orphan it sees. (Future
// work: bind orphan sessions to projects via a session-name suffix
// or a record-of-record file. Not in this PR's scope.)
//
// Returns ("", false) on any list error or when every session has a
// matching record.
func OrphanTmuxForProject(records []*agent.Record, projectName string) (string, bool) {
	sessions, err := listSessionsFn()
	if err != nil {
		return "", false
	}
	have := map[string]bool{}
	for _, r := range records {
		if r == nil {
			continue
		}
		have[r.ID] = true
	}
	for _, s := range sessions {
		const prefix = "fleet-"
		if !strings.HasPrefix(s, prefix) {
			continue
		}
		id := strings.TrimPrefix(s, prefix)
		if !isAgentIDShape(id) {
			continue
		}
		if have[id] {
			continue
		}
		return id, true
	}
	return "", false
}

// sessionAliveOrProbe returns true when the session is alive OR when
// the probe failed (transport error — conservative; don't drop a
// claim on a hiccup). False only on a definitive "no such session".
// Same strategy as internal/tui's sessionProbeOrAliveFn.
func sessionAliveOrProbe(session string) bool {
	if sessionAliveFn(session) {
		return true
	}
	alive, err := sessionProbeFn(session)
	if err != nil {
		return true // transport error → conservative
	}
	return alive
}

// readCoordHolder reads the first line of
// projectsRoot/<projectName>/.locks/coordinator.lock and returns it
// if it parses as an agent ID. Returns "" on any failure. This is a
// duplicate of internal/tui/dashboard.readCoordHolder — the TUI
// version stays put because the dashboard reads more aggressively
// during the snapshot scan; extracting both into one helper here
// would mean importing the TUI package from cmd/fleet/attach.go (a
// dependency direction Go test discipline prohibits).
func readCoordHolder(projectsRoot, projectName string) string {
	path := filepath.Join(projectsRoot, projectName, ".locks", "coordinator.lock")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	line := data
	if i := bytes.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	if i := bytes.IndexByte(line, '\r'); i >= 0 {
		line = line[:i]
	}
	s := strings.TrimSpace(string(line))
	if !isAgentIDShape(s) {
		return ""
	}
	return s
}

// isAgentIDShape mirrors internal/tui/dashboard.isAgentIDShape: 8-char
// lower-hex agent IDs (the shape agent.NewID generates).
func isAgentIDShape(s string) bool {
	if len(s) != 8 {
		return false
	}
	for _, c := range s {
		if !isHexLower(c) {
			return false
		}
	}
	return true
}

func isHexLower(c rune) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
}
