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
	"fmt"
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

// SetLoadArchiveStub replaces the loadArchiveFn seam used by
// OrphanTmuxForProject's cross-project guard. Tests pass a closure that
// fakes the archive contents (codex review iter-4 P1). Returns a
// restore closure; production never calls this.
func SetLoadArchiveStub(fn func(id string) (*agent.Record, error)) (restore func()) {
	prev := loadArchiveFn
	if fn != nil {
		loadArchiveFn = fn
	}
	return func() { loadArchiveFn = prev }
}

// CoordTaskID returns the canonical task_id for a project's coord
// agent. Centralized here so callers don't reinvent the prefix.
//
// Project names are validated upstream via state.ValidateProjectName,
// so this never embeds a path-unsafe component.
func CoordTaskID(projectName string) string {
	return "coord-" + projectName
}

// CoordSpawnPrompt returns the bootstrap prompt the coord agent receives
// on first paint. Hardens the agent's role (discuss + delegate; never
// edit code directly) and instructs it to invoke /coordinator.
//
// Mirrors internal/tui/keys.go:coordSpawnPrompt verbatim so the two
// spawn entry points (TUI [a] / CLI attach Tier 3) produce identical
// coords. Codex review iter-6 P1: when Tier 3 attach has to spawn (Path
// C/C'/D) without this prompt, the operator lands in a bare Claude
// session that never types /coordinator — the project stays unowned.
//
// The TUI keeps its private wrapper for now (different package, no
// import cycle worth solving for one constant) but its body delegates
// to this once the TUI side is refactored. Until then this is a
// duplicated string; both must move in lockstep.
func CoordSpawnPrompt(projectName string) string {
	return fmt.Sprintf(`You are a Fleet COORDINATOR agent for project %s.

ROLE — discuss design with the operator, save approved plan docs, file tasks, dispatch workers. NEVER:
- Edit code files (no Edit, Write, NotebookEdit on source code).
- Run tests (no `+"`go test`"+`, `+"`pytest`"+`, etc. — workers handle this).
- Implement features inline.
- Run any tool that mutates the project source tree, except the narrow PLAN-DOC and TASK-PLAN-DOC steps below.

DELEGATE — for any implementation, testing, or code-touching work:
1. Discuss design with the operator until aligned.
2. PLAN-DOC: save the approved implementation plan as `+"`docs/DESIGN-<kebab-topic>.md`"+` and render `+"`docs/DESIGN-<kebab-topic>.html`"+` when the project has a renderer.
3. File tasks via `+"`fleet tasks add --project %s --spec <body>`"+` while keeping them unpromoted.
4. TASK-PLAN-DOC: save `+"`docs/TASK-PLAN-<slug>.md`"+` and render `+"`docs/TASK-PLAN-<slug>.html`"+` when supported.
5. Add the task plan path to worker-visible task text, e.g. `+"`fleet tasks note --project %s <slug> --section spec \"Task plan: docs/TASK-PLAN-<slug>.md\"`"+`.
6. Promote the task with `+"`fleet tasks promote <slug>`"+` only after its task plan doc exists and is linked or embedded.
7. The /coordinator skill auto-dispatches a worker on next tick.
8. Track progress via the supervisor loop.

ALLOWED — your toolbox is intentionally narrow:
- Read code files for design discussion (Read, Grep, Bash with non-mutating commands).
- Write/render approved implementation plan docs and per-task plan docs under the project's approved docs folder only (`+"`docs/`"+` when present).
- Run fleet CLI: `+"`fleet tasks {add,list,show,set,note,promote}`"+`, `+"`fleet workers list`"+`, `+"`fleet peek`"+`, `+"`fleet learnings`"+`, `+"`fleet standards show`"+`.
- Run gh CLI for status: `+"`gh pr view`"+`, `+"`gh pr checks`"+`, `+"`gh issue view`"+`.
- Talk to the operator about design, scope, priority.

Run /coordinator now to begin the supervisor loop.`, projectName, projectName, projectName)
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
//
// Returned record always has TmuxSession populated (codex review iter-3
// P2): legacy/pre-spawn records may have TmuxSession=="" on disk; we
// probe via the synthesized name and, when the synthesized name is the
// one that was alive, hand back a defensive COPY of the record with
// TmuxSession set. Callers that assign `pendingAttach = rec.TmuxSession`
// (notably the TUI F18 path) would otherwise quit with nothing to attach.
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
		if r.TmuxSession == "" {
			cp := *r
			cp.TmuxSession = session
			return &cp, true
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
//
// Same TmuxSession normalization as FindLiveCoord (codex review iter-3
// P2): when the record on disk has TmuxSession=="" we return a copy with
// the synthesized session populated so TUI callers don't quit with an
// empty pendingAttach.
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
		if r.TmuxSession == "" {
			cp := *r
			cp.TmuxSession = session
			return &cp, true
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
		// BUG #1 (attach-failover INVARIANT 2): an empty-TmuxSession
		// record is UNPROBEABLE, not "stale". Synthesizing
		// tmux.SessionName(r.ID) and probing it is a FALSE signal — it
		// checks a fleet-<id> session that was never the real one, so
		// the probe trivially returns dead and attach would print a
		// bogus "reaped stale <id>" line. gc's reconcileOrphanAgents
		// already SKIPS empty-TmuxSession records (gc.go), so the
		// classifier and the reaper must agree: skip it here too. Tier 3
		// falls through to Path D (fresh spawn); dispatch's own recovery
		// still matches the empty-session record (it guards its alive
		// probe with r.TmuxSession != "", so empty ⇒ "not alive" ⇒
		// recovery candidate) — recovery context is preserved WITHOUT
		// attach printing a false reap line.
		if r.TmuxSession == "" {
			continue
		}
		alive, err := sessionProbeFn(r.TmuxSession)
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

// StaleLockBodyCoord returns a record whose ID matches the project's
// coordinator.lock body AND whose tmux session is DEFINITIVELY dead.
// Covers the legacy / manually-spawned coord case (codex review iter-11
// P2): such records are tagged with task_id ≠ coord-<project>, so
// StaleCoordRecord skips them and Tier 3 would fall through to Path D
// (fresh spawn) — losing the dead coord's recovery context (cwd /
// engine / workers state). The lock body is authoritative ("this is
// the coord for this project"); if its tmux session is dead, the
// record is the right recovery candidate even without the task_id tag.
//
// Returns (nil, false) when the lock body is missing/empty/malformed,
// when no record has the matching ID, or when the matching record's
// session is alive or its liveness is ambiguous (transport-error
// conservatism — same discipline as StaleCoordRecord).
func StaleLockBodyCoord(records []*agent.Record, projectName string) (*agent.Record, bool) {
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
		alive, perr := sessionProbeFn(session)
		if perr != nil {
			continue // transport error → conservative
		}
		if alive {
			continue
		}
		return r, true
	}
	return nil, false
}

// OrphanTmuxForProject scans live tmux sessions for fleet-<id>
// patterns and returns the first <id> that BELONGS TO projectName but
// has no live agent record. "Belongs to projectName" requires evidence
// in the archived records — same project tag on the archive matching
// the orphan ID. Drives the Tier 3 "no record but lingering tmux
// session" branch — pass the returned ID into the gc reap path before
// respawning.
//
// Codex review iter-4 P1: an earlier version returned the FIRST orphan
// regardless of project, then the caller ran `fleet gc --aggressive
// --project A` to reap it. Because tmux session names don't encode the
// project, a `fleet attach --project A` could nuke a `fleet-<id>` left
// over from project B. The archive-record gate stops the cross-project
// reap; if the orphan can't be bound to projectName, OrphanTmuxForProject
// returns ("", false) so the caller falls through to spawn-fresh (Path D)
// instead of running gc against an unrelated session.
//
// Returns ("", false) on any list error, when every session has a live
// matching record, or when no orphan session's archive ties it to
// projectName.
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
		// Cross-project guard: only return this orphan when the
		// archived record agrees it belonged to projectName. No
		// archive (unknown provenance) → leave it alone; reaping it
		// might kill another project's lingering session. The gc
		// reap that follows is project-scoped anyway, so the
		// no-match case wouldn't actually clean this orphan — surface
		// "no" so Tier 3 falls through to spawn-fresh and leaves the
		// orphan for the cross-project gc sweep to handle.
		arec, aerr := loadArchiveFn(id)
		if aerr != nil || arec == nil || arec.Project != projectName {
			continue
		}
		return id, true
	}
	return "", false
}

// loadArchiveFn is a seam for OrphanTmuxForProject's archive lookup —
// tests stub it to avoid writing an on-disk archive for every orphan
// scenario. Production delegates to agent.LoadArchive.
var loadArchiveFn = func(id string) (*agent.Record, error) {
	return agent.LoadArchive(id)
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
