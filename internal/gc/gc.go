// Package gc owns the one-shot orphan-resource reaper invoked from
// `fleet gc`. Scope = fleet's own resources only (per
// feedback_fleet_owns_its_resources.md): /tmp/fleet-test-*.sock,
// ~/.fleet/agents/*.json records whose tmux session is gone,
// fleet-* tmux sessions with no live agent record (surfaced by default,
// killed only with --aggressive per feedback_surface_dont_silo.md +
// feedback_user_owns_tmux_config.md), and worktrees under
// ~/.fleet/projects/*/worktrees/ whose task has reached a terminal
// status (done | abandoned).
//
// The package is also reused by PR-D's automatic reconciliation in
// `fleet status` / `fleet dispatch` — that path calls Reconcile with
// Apply=false to surface orphans to the operator without auto-mutating.
//
// Reconciliation flow:
//
//	Options + Deps  ─→  Reconcile  ─→  Report (Action list)
//	                       │
//	    ┌──────────────────┼──────────────────┐
//	    ▼                  ▼                  ▼
//	sockets        orphan-agents        orphan-tmux       worktrees
//	(/tmp glob,    (~/.fleet/agents     (tmux ls,         (per-project
//	max-age gate)  cross-checked vs     freshness floor,  worktrees,
//	               tmux.SessionAlive)   agent-record      task terminal
//	                                    match)            gate)
//
// Side effects only fire when Options.Apply is true. Dry-run produces
// the same Report with would-* verbs and never invokes any mutator.
package gc

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/spawn"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tmux"
)

// Kind enumerates the four resource families fleet gc reaps. Stored
// verbatim in --kinds CSV flag values.
type Kind string

const (
	KindSockets      Kind = "sockets"
	KindOrphanAgents Kind = "orphan-agents"
	KindOrphanTmux   Kind = "orphan-tmux"
	KindWorktrees    Kind = "worktrees"
	// KindCoordLocks scans ~/.fleet/projects/<p>/.locks/coordinator.lock
	// across every project and flags three failure modes: mismatch
	// (lock body's agent record reports a different project than the
	// lock's parent dir), dead-coord (record file missing), stale-tmux
	// (record exists but the named tmux session is gone). Surface-only
	// by default; --apply unlinks. Tracks fleet#172 — sweeps historical
	// state from before PRs #173 (fleet#170) and #174 (fleet#171) closed
	// the new-hijack window at spawn + tick time. See coord_locks.go.

	// KindWorkerRecords (fleet#177) is the sixth classifier — sweeps
	// stale `~/.fleet/projects/<p>/workers/<slug>/state.json` files.
	// Four failure modes: mismatch (state.project disagrees with parent
	// dir), dead-pid (pid set + process gone + old), stale-heartbeat
	// (pid=0 + old), task-terminal (tasks.md says done|abandoned but
	// worker dir survives). Surface-only by default; --apply removes
	// the worker dir (NOT the worktree — KindWorktrees owns that).
	// Live-PID guard + task-in-progress guard prevent live-worker harm.
	// See worker_records.go.
)

// AllKinds is the default --kinds set when the flag is omitted.
var AllKinds = []Kind{KindSockets, KindOrphanAgents, KindOrphanTmux, KindWorktrees, KindCoordLocks, KindWorkerRecords}

// Verb enumerates the operation attached to each report entry. Two
// flavors per mutator (would-X vs X) so dry-run vs apply paths share
// the same enum. Surface has no "would-" variant — surfacing is always
// surface-only (no mutation to "would" against).
type Verb string

const (
	VerbWouldRemove  Verb = "would-remove"
	VerbRemoved      Verb = "removed"
	VerbWouldArchive Verb = "would-archive"
	VerbArchived     Verb = "archived"
	VerbSurface      Verb = "surface"
	VerbWouldKill    Verb = "would-kill"
	VerbKilled       Verb = "killed"
)

// SocketInfo is the minimal shape Reconcile needs for sockets: path on
// disk + last-modified time. The default lister (scanSocketsDir +
// DefaultListSockets) populates both from os.Stat; tests fabricate
// arbitrary mtimes to exercise the max-age boundary.
type SocketInfo struct {
	Path    string
	ModTime time.Time
}

// WorktreeEntry pairs a project name with one worktree slug + on-disk
// path under ~/.fleet/projects/<project>/worktrees/<slug>/. Reconcile
// asks IsTaskTerminal for the gate.
type WorktreeEntry struct {
	Project string
	Slug    string
	Path    string
}

// Options drives Reconcile's behavior. All fields are explicit so the
// CLI flag wiring (cmd/fleet/gc.go) reads as a 1:1 translation.
type Options struct {
	// Apply=false is dry-run (default). Apply=true actually mutates.
	Apply bool
	// Aggressive opts in to auto-killing orphan tmux sessions. Default
	// false surfaces them only — see feedback_surface_dont_silo.md and
	// feedback_user_owns_tmux_config.md.
	Aggressive bool
	// MaxAge gates socket removal (a socket younger than MaxAge is
	// kept). Default 24h applied at the CLI layer.
	MaxAge time.Duration
	// Kinds names which families to consider. Empty falls through to
	// AllKinds at the CLI layer; Reconcile itself treats nil/empty as
	// "do nothing" so callers get the explicit-opt-in default.
	Kinds []Kind
	// Project scopes worktree + agent enumeration. Empty = all projects.
	Project string
}

// Deps groups every external read/write Reconcile needs. Tests stub
// each field; production wiring is via DefaultDeps. Keeping the
// dependencies explicit (not package-level vars) makes the reuse
// boundary obvious for PR-D's automatic reconciliation in
// `fleet status` / `fleet dispatch`.
type Deps struct {
	// Now is the wall-clock source for age comparisons. Tests pin it
	// for determinism.
	Now func() time.Time

	// Sockets.
	ListSockets  func() ([]SocketInfo, error)
	RemoveSocket func(path string) error
	// SocketLive probes whether a tmux server is bound to the given
	// /tmp/fleet-test-*.sock path. Returns true if a live server
	// responds, false on "no server" / "connection refused" / file-
	// gone errors. Used to skip unlinking sockets that a long-running
	// test fixture is still bound to (codex iter-4 [P1]). Nil =
	// liveness probe disabled (legacy unit tests that pre-date this
	// gate); production wiring is socketLiveOnDisk via DefaultDeps.
	SocketLive func(path string) bool

	// Agents.
	ListAgents   func() ([]*agent.Record, error)
	ArchiveAgent func(*agent.Record) error
	SessionAlive func(name string) (bool, error)
	// AgentDirSane verifies the live-agent state root is readable
	// BEFORE the orphan-tmux classifier consumes the agent listing.
	// Fails closed for the orphan-tmux pass (codex iter-1 [P1]):
	// without this gate, a missing/corrupted ~/.fleet/agents directory
	// makes ListAgents() return an empty slice, every live fleet-*
	// tmux session looks unmatched, and --aggressive --apply mass-
	// terminates live agents. Mirrors cmd/fleet/maintenance.go's
	// agentDirSane precondition for fleet maintenance prune-orphan-tmux.
	AgentDirSane func() error
	// ListAgentsStrict returns the good records AND the IDs of records
	// that exist on disk but failed to parse. Used by the orphan-tmux
	// classifier as the strict gate (codex iter-2 [P1]): if any record
	// is malformed, its matching fleet-<id> tmux session would look
	// unmatched (because the silent-skip in ListAgents drops it from
	// liveIDs) and could be killed under --aggressive --apply. The
	// classifier fails closed when len(badIDs) > 0 — same shape as the
	// dispatch --coord-spawn split-brain veto (codex iter-17).
	ListAgentsStrict func() (good []*agent.Record, badIDs []string, err error)

	// Tmux.
	ListSessions func() ([]tmux.SessionInfo, error)
	KillSession  func(name string) error
	// OrphanTmuxFreshness returns the freshness window below which a
	// record-absent fleet-<id> session is treated as an in-flight spawn
	// (kept, never killed). MUST agree with the spawn path's record-write
	// budget: spawn.Spawn creates tmux first, resolves the engine pid
	// (up to spawn.PidResolveTimeout()), then writes the record — a
	// fixed 90s window can be too short on slow-spawn engines where the
	// operator raised FLEET_PID_RESOLVE_S (codex iter-1 [P1]). Default
	// wiring mirrors cmd/fleet/maintenance.go:pruneOrphanTmuxFreshness =
	// max(OrphanTmuxMinFreshness, 2 * spawn.PidResolveTimeout()).
	OrphanTmuxFreshness func() time.Duration

	// Worktrees.
	ListProjects   func() ([]string, error)
	ListWorktrees  func(project string) ([]WorktreeEntry, error)
	RemoveWorktree func(path string) error
	IsTaskTerminal func(project, slug string) (bool, error)

	// Coord-locks (KindCoordLocks).
	// ListCoordLocks enumerates every coordinator.lock file under
	// ~/.fleet/projects/<p>/.locks/. Production wiring is
	// listCoordLocksOnDisk in coord_locks.go.
	ListCoordLocks func() ([]CoordLockInfo, error)
	// LoadAgent loads a single agent record by ID. Used to look up
	// the lock holder so the classifier can detect mismatch + stale.
	// Returns state.ErrNotFound when the record file is missing — that's
	// the dead-coord signal. Production wiring is agent.Load.
	LoadAgent func(id string) (*agent.Record, error)
	// RemoveCoordLock unlinks the lock file under --apply. Production
	// wiring is removeCoordLockFile (best-effort, ENOENT-tolerant).
	RemoveCoordLock func(path string) error

	// Worker-records (KindWorkerRecords).
	// ListWorkerRecords enumerates every worker dir under
	// ~/.fleet/projects/<p>/workers/<slug>/ containing a state.json.
	// archive/ subtree is skipped. Production wiring is
	// listWorkerRecordsOnDisk in worker_records.go.
	ListWorkerRecords func() ([]WorkerRecordInfo, error)
	// LoadWorkerState parses one state.json into the classifier's
	// minimal projection (slug, project, pid, updated_at). Production
	// wiring is loadWorkerStateOnDisk.
	LoadWorkerState func(path string) (WorkerState, error)
	// LoadTaskStatus returns the raw status string for a (project, slug)
	// task from tasks.md (or tasks-archive.md). Returns "" with nil err
	// when the slug is absent from both files. Production wiring is
	// loadTaskStatusOnDisk.
	LoadTaskStatus func(project, slug string) (string, error)
	// PidAlive returns true iff the OS reports the pid as still
	// running. Used by the live-PID guard to skip-reaping live workers.
	// Production wiring is pidAliveOnDisk (signal-0 probe).
	PidAlive func(pid int) bool
	// RemoveWorkerRecord removes the worker DIR (state.json +
	// output.log + scratch) under --apply. NEVER touches the worktree
	// (separate path; KindWorktrees owns it). Production wiring is
	// removeWorkerRecordDir.
	RemoveWorkerRecord func(path string) error
}

// Action describes one planned-or-applied operation against a single
// target. Reason is human-readable (printed by the CLI as the trailing
// rationale on each line).
type Action struct {
	Kind   Kind
	Target string
	Verb   Verb
	Reason string
}

// Report carries the per-kind planned/applied actions. Stable iteration
// order = Sockets → OrphanAgents → OrphanTmux → Worktrees, then
// alphabetical by Target within each kind.
type Report struct {
	Actions []Action
}

// OrphanTmuxMinFreshness is the floor for "this fleet-* tmux session
// might still be a brand-new spawn whose agent record is mid-write".
// Mirrors cmd/fleet/maintenance.go:pruneOrphanTmuxMinFreshness — the
// classification gate is the same shape, so the freshness window must
// agree. Sessions younger than this are NEVER touched, even under
// --aggressive (codex review iter-2/3 [P1] history on prune-orphan-tmux).
const OrphanTmuxMinFreshness = 90 * time.Second

// fleetAgentIDPattern matches the agent ID format produced by
// agent.NewID(): 8 lowercase hex characters (happy path), OR the
// crypto/rand fallback shape "t%07x" with the value masked to 32 bits
// (which yields t+7hex or t+8hex). Sessions whose suffix doesn't match
// are out of fleet's blast radius (operator-owned tmux config —
// feedback_user_owns_tmux_config.md).
var fleetAgentIDPattern = regexp.MustCompile(`^([0-9a-f]{8}|t[0-9a-f]{7,8})$`)

// hasKind is a tiny membership check on the Kinds opt-in list.
func hasKind(kinds []Kind, k Kind) bool {
	for _, x := range kinds {
		if x == k {
			return true
		}
	}
	return false
}

// Reconcile runs the four classifiers, executes any side effects gated
// by Apply, and returns the Report. A nil/empty Options.Kinds is a
// no-op (callers must explicitly opt in to each family).
//
// Errors from individual classifier reads are returned wrapped — the
// CLI surfaces them on stderr but still prints whatever actions were
// produced before the failure. Mutator errors are recorded as
// per-action Reason text and a separate stderr line in the CLI; they
// do NOT abort the whole sweep (one bad worktree must not strand the
// other three families).
func Reconcile(opts Options, deps Deps) (Report, error) {
	var r Report
	var firstErr error
	recordErr := func(e error) {
		if firstErr == nil {
			firstErr = e
		}
	}

	if hasKind(opts.Kinds, KindSockets) {
		if err := reconcileSockets(&r, opts, deps); err != nil {
			recordErr(fmt.Errorf("sockets: %w", err))
		}
	}
	if hasKind(opts.Kinds, KindOrphanAgents) {
		if err := reconcileOrphanAgents(&r, opts, deps); err != nil {
			recordErr(fmt.Errorf("orphan-agents: %w", err))
		}
	}
	if hasKind(opts.Kinds, KindOrphanTmux) {
		if err := reconcileOrphanTmux(&r, opts, deps); err != nil {
			recordErr(fmt.Errorf("orphan-tmux: %w", err))
		}
	}
	if hasKind(opts.Kinds, KindWorktrees) {
		if err := reconcileWorktrees(&r, opts, deps); err != nil {
			recordErr(fmt.Errorf("worktrees: %w", err))
		}
	}
	if hasKind(opts.Kinds, KindCoordLocks) {
		if err := reconcileCoordLocks(&r, opts, deps); err != nil {
			recordErr(fmt.Errorf("coord-locks: %w", err))
		}
	}
	if hasKind(opts.Kinds, KindWorkerRecords) {
		if err := reconcileWorkerRecords(&r, opts, deps); err != nil {
			recordErr(fmt.Errorf("worker-records: %w", err))
		}
	}

	// Stable order: Kind primary, Target secondary. Tests rely on
	// findAction() (linear scan) so the order is for human-readable
	// output, not for assertion order.
	sort.SliceStable(r.Actions, func(i, j int) bool {
		if r.Actions[i].Kind != r.Actions[j].Kind {
			return kindRank(r.Actions[i].Kind) < kindRank(r.Actions[j].Kind)
		}
		return r.Actions[i].Target < r.Actions[j].Target
	})
	return r, firstErr
}

func kindRank(k Kind) int {
	switch k {
	case KindSockets:
		return 0
	case KindOrphanAgents:
		return 1
	case KindOrphanTmux:
		return 2
	case KindWorktrees:
		return 3
	case KindCoordLocks:
		return 4
	case KindWorkerRecords:
		return 5
	}
	return 99
}

// reconcileSockets walks SocketInfo list, filters by MaxAge gate, and
// either records would-remove or actually removes the file via
// RemoveSocket. /tmp/fleet-test-*.sock files older than MaxAge are
// orphan test debris (per feedback_fleet_owns_its_resources.md the
// existing leak peaked at 3,570 of them).
func reconcileSockets(r *Report, opts Options, deps Deps) error {
	infos, err := deps.ListSockets()
	if err != nil {
		return err
	}
	now := deps.Now()
	for _, s := range infos {
		age := now.Sub(s.ModTime)
		if age < opts.MaxAge {
			continue // within freshness window — keep
		}
		// Liveness gate (codex iter-4 [P1]): a long-running tmux test
		// fixture can still be bound to a fleet-test-*.sock whose
		// mtime drifted past --max-age. Removing it would strand the
		// bound server (subsequent `tmux -S <sock>` from new clients
		// would fail to connect). Surface (don't remove) when a live
		// server still responds.
		//
		// Apply this gate in BOTH dry-run and apply mode (codex iter-5
		// [P2]): the dry-run report is the planned-action list, so it
		// must agree with what --apply will actually do. Without this,
		// dry-run prints `verb=would-remove` for a socket --apply would
		// surface, which silently misleads operators.
		if deps.SocketLive != nil && deps.SocketLive(s.Path) {
			r.Actions = append(r.Actions, Action{
				Kind: KindSockets, Target: s.Path, Verb: VerbSurface,
				Reason: fmt.Sprintf("age=%s exceeds max-age=%s but tmux server still bound (keeping); kill the server first if you want to reap",
					humanDuration(age), humanDuration(opts.MaxAge)),
			})
			continue
		}
		act := Action{Kind: KindSockets, Target: s.Path, Verb: VerbWouldRemove,
			Reason: fmt.Sprintf("age=%s exceeds max-age=%s", humanDuration(age), humanDuration(opts.MaxAge))}
		if opts.Apply {
			if rerr := deps.RemoveSocket(s.Path); rerr != nil {
				act.Reason = fmt.Sprintf("remove failed: %v", rerr)
			} else {
				act.Verb = VerbRemoved
			}
		}
		r.Actions = append(r.Actions, act)
	}
	return nil
}

// reconcileOrphanAgents enumerates ~/.fleet/agents/*.json and, for
// each, probes the declared tmux session. Records whose session is
// definitively gone (SessionAlive returns alive=false, err=nil) are
// candidates to archive. Probe errors (transport failure, ambiguous
// state) leave the record alone — surface-don't-silo means we don't
// archive a live agent on a transient probe miss
// (feedback_surface_dont_silo.md + SessionAlive's own contract).
//
// Project scope: when opts.Project is set, only records matching that
// project are considered. Records with empty Project field are
// included only when opts.Project is empty (the global sweep).
//
// MULTI-SOCKET CONSTRAINT (codex iter-4 [P1], inherited fleet-wide):
// tmux.SessionAlive probes the CURRENT FLEET_TMUX_SOCKET (see
// internal/tmux/tmux.go:tmuxArgs). agent.Record does not persist the
// socket path it was spawned on, so a live agent on a different
// socket can appear "missing" to this classifier and be archived.
// The same constraint exists in cmd/fleet/handoff.go (SessionAlive at
// handoff.go:281/397/402/817/820/etc — see the explicit acknowledgement
// at handoff.go:444). Fixing this properly requires persisting the
// socket path in agent.Record, which is a schema change tracked
// separately. Operators running `fleet gc --apply` from a shell with
// FLEET_TMUX_SOCKET set differently from spawn time can mis-archive;
// the runGC entrypoint prints a stderr warning when FLEET_TMUX_SOCKET
// is set + Apply=true + KindOrphanAgents is enabled, so the operator
// can abort. Defaults (dry-run, FLEET_TMUX_SOCKET unset) are safe.
func reconcileOrphanAgents(r *Report, opts Options, deps Deps) error {
	records, err := deps.ListAgents()
	if err != nil {
		return err
	}
	for _, rec := range records {
		if rec == nil {
			continue
		}
		if opts.Project != "" && rec.Project != opts.Project {
			continue
		}
		// Empty TmuxSession is unprobeable (codex iter-3 [P1]) — legacy
		// or partially-populated records would have `tmux has-session
		// -t ""` rejected with an ambiguous error that COULD match the
		// "no such session" branch and falsely classify the record as
		// dead. Mirrors internal/tui/model.go's conservative-treat-as-
		// live behavior for empty TmuxSession.
		if strings.TrimSpace(rec.TmuxSession) == "" {
			continue
		}
		alive, perr := deps.SessionAlive(rec.TmuxSession)
		if perr != nil {
			// Probe failed — refuse to act on ambiguous state.
			continue
		}
		if alive {
			continue
		}
		act := Action{Kind: KindOrphanAgents, Target: rec.ID, Verb: VerbWouldArchive,
			Reason: fmt.Sprintf("tmux session %s gone", rec.TmuxSession)}
		if opts.Apply {
			if aerr := deps.ArchiveAgent(rec); aerr != nil {
				act.Reason = fmt.Sprintf("archive failed: %v", aerr)
			} else {
				act.Verb = VerbArchived
			}
		}
		r.Actions = append(r.Actions, act)
	}
	return nil
}

// reconcileOrphanTmux finds fleet-* sessions whose ID suffix has no
// matching live agent record AND that are past the freshness floor.
// Default behavior surfaces (Verb=surface) without killing —
// feedback_surface_dont_silo.md + feedback_user_owns_tmux_config.md
// say we don't auto-mutate operator-owned tmux state on a maybe.
//
// Aggressive=true upgrades the surface action to would-kill / killed,
// matching the documented escape hatch (--aggressive on the CLI).
//
// Fails closed when AgentDirSane reports the live-agent state root is
// unavailable: an empty ListAgents() in that case is a read failure,
// not "every session is orphan", so classifying based on it would
// mass-terminate live agents under --aggressive --apply. The whole
// orphan-tmux pass is skipped and the error is recorded so the
// operator sees the gap (surface-don't-silo via Reconcile's first-err
// return) — same shape as cmd/fleet/maintenance.go's agentDirSane gate
// for fleet maintenance prune-orphan-tmux (codex iter-1 [P1]).
func reconcileOrphanTmux(r *Report, opts Options, deps Deps) error {
	sessions, err := deps.ListSessions()
	if err != nil {
		return err
	}
	if len(sessions) == 0 {
		return nil
	}
	if deps.AgentDirSane != nil {
		if err := deps.AgentDirSane(); err != nil {
			return fmt.Errorf("agent dir sanity check: %w", err)
		}
	}
	// Prefer the strict lister: if any record on disk failed to parse,
	// its matching fleet-<id> session would look unmatched and could be
	// killed under --aggressive --apply (codex iter-2 [P1]). Fail closed
	// before consuming the listing — same shape as dispatch --coord-spawn
	// split-brain veto.
	var records []*agent.Record
	if deps.ListAgentsStrict != nil {
		good, badIDs, lerr := deps.ListAgentsStrict()
		if lerr != nil {
			return lerr
		}
		if len(badIDs) > 0 {
			sort.Strings(badIDs)
			return fmt.Errorf("agent listing has %d unparseable record(s) %v — refusing to classify orphan-tmux (matching live session could misclassify and be killed)",
				len(badIDs), badIDs)
		}
		records = good
	} else {
		var err2 error
		records, err2 = deps.ListAgents()
		if err2 != nil {
			return err2
		}
	}
	// Build a set of live agent IDs (and their session names) to
	// short-circuit the per-session probe.
	liveIDs := make(map[string]struct{}, len(records))
	liveSessions := make(map[string]struct{}, len(records))
	for _, rec := range records {
		if rec == nil {
			continue
		}
		liveIDs[rec.ID] = struct{}{}
		liveSessions[rec.TmuxSession] = struct{}{}
	}
	freshness := OrphanTmuxMinFreshness
	if deps.OrphanTmuxFreshness != nil {
		if dyn := deps.OrphanTmuxFreshness(); dyn > freshness {
			freshness = dyn
		}
	}
	now := deps.Now()
	for _, s := range sessions {
		if !strings.HasPrefix(s.Name, "fleet-") {
			continue // not fleet's session
		}
		id := strings.TrimPrefix(s.Name, "fleet-")
		if id == "" || !fleetAgentIDPattern.MatchString(id) {
			// Non-agent fleet-* session (e.g. fleet-coord-839b11ff or
			// operator's manually named fleet-debug). Out of scope —
			// reaping those would violate the user-owns-tmux-config
			// boundary on a heuristic match.
			continue
		}
		if _, ok := liveIDs[id]; ok {
			continue // session belongs to a live agent record
		}
		if _, ok := liveSessions[s.Name]; ok {
			continue
		}
		// Freshness gate: a brand-new tmux session whose agent record
		// is still being written must not be classified as orphan.
		// Zero Created (parse failure) → treat as fresh-unknown
		// (conservative, refuse to act). Window scales with
		// FLEET_PID_RESOLVE_S via OrphanTmuxFreshness (codex iter-1 [P1])
		// so a slow-spawn engine's in-flight replacement isn't killed.
		if s.Created.IsZero() || now.Sub(s.Created) < freshness {
			continue
		}
		act := Action{Kind: KindOrphanTmux, Target: s.Name, Verb: VerbSurface,
			Reason: fmt.Sprintf("no agent record; kill manually with `tmux kill-session -t %s` or rerun with --aggressive --apply", s.Name)}
		if opts.Aggressive {
			act.Verb = VerbWouldKill
			act.Reason = fmt.Sprintf("no agent record (age=%s)", humanDuration(now.Sub(s.Created)))
			if opts.Apply {
				if kerr := deps.KillSession(s.Name); kerr != nil {
					act.Reason = fmt.Sprintf("kill failed: %v", kerr)
				} else {
					act.Verb = VerbKilled
				}
			}
		}
		r.Actions = append(r.Actions, act)
	}
	return nil
}

// reconcileWorktrees walks ~/.fleet/projects/<project>/worktrees/<slug>/
// directories and removes those whose corresponding task has reached a
// terminal status (done | abandoned). Active-task worktrees stay put —
// the worker may be mid-rebase / mid-push.
//
// Project scope: empty opts.Project enumerates every project via
// ListProjects; an explicit name skips enumeration and calls
// ListWorktrees(project) directly.
func reconcileWorktrees(r *Report, opts Options, deps Deps) error {
	var projects []string
	if opts.Project != "" {
		projects = []string{opts.Project}
	} else {
		ps, err := deps.ListProjects()
		if err != nil {
			return err
		}
		projects = ps
	}
	for _, p := range projects {
		entries, err := deps.ListWorktrees(p)
		if err != nil {
			// Per-project failure is recorded as a synthetic action so
			// the operator sees it without it tanking the whole sweep.
			r.Actions = append(r.Actions, Action{
				Kind: KindWorktrees, Target: p, Verb: VerbSurface,
				Reason: fmt.Sprintf("list worktrees failed: %v", err),
			})
			continue
		}
		for _, e := range entries {
			terminal, terr := deps.IsTaskTerminal(e.Project, e.Slug)
			if terr != nil {
				// Probe failed — leave alone (surface-don't-silo).
				continue
			}
			if !terminal {
				continue
			}
			act := Action{Kind: KindWorktrees, Target: e.Path, Verb: VerbWouldRemove,
				Reason: fmt.Sprintf("task %s/%s reached terminal status", e.Project, e.Slug)}
			if opts.Apply {
				if rerr := deps.RemoveWorktree(e.Path); rerr != nil {
					act.Reason = fmt.Sprintf("remove failed: %v", rerr)
				} else {
					act.Verb = VerbRemoved
				}
			}
			r.Actions = append(r.Actions, act)
		}
	}
	return nil
}

// humanDuration formats a duration in a compact form. Used in Reason
// fields so the CLI output reads well without a heavyweight formatter.
func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Round(time.Second).Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Round(time.Minute).Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Round(time.Hour).Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Round(time.Hour)/24/time.Hour))
	}
}

// ----------------- production wiring (DefaultDeps) ------------------

// DefaultDeps returns the production Deps wired to ~/.fleet/, /tmp/,
// the tmux binary, the agent package, and git for worktree removal.
//
// All the heavy lifting lives in helpers under this comment so the
// returned struct is one screenful and trivially reads as "here's the
// real call for each hook."
func DefaultDeps() Deps {
	return Deps{
		Now:                 time.Now,
		ListSockets:         func() ([]SocketInfo, error) { return scanSocketsDir("/tmp") },
		RemoveSocket:        removeSocketFile,
		SocketLive:          socketLiveOnDisk,
		ListAgents:          agent.List,
		ListAgentsStrict:    agent.ListStrict,
		ArchiveAgent:        func(r *agent.Record) error { return r.Archive() },
		SessionAlive:        tmux.SessionAlive,
		AgentDirSane:        agentDirSane,
		ListSessions:        tmux.ListSessionsWithCreated,
		KillSession:         tmux.Kill,
		OrphanTmuxFreshness: orphanTmuxFreshness,
		ListProjects:        listProjectsOnDisk,
		ListWorktrees:       listProjectWorktrees,
		RemoveWorktree:      removeGitWorktree,
		IsTaskTerminal:      isTaskTerminalOnDisk,
		ListCoordLocks:      listCoordLocksOnDisk,
		LoadAgent:           agent.Load,
		RemoveCoordLock:     removeCoordLockFile,
		ListWorkerRecords:   listWorkerRecordsOnDisk,
		LoadWorkerState:     loadWorkerStateOnDisk,
		LoadTaskStatus:      loadTaskStatusOnDisk,
		PidAlive:            pidAliveOnDisk,
		RemoveWorkerRecord:  removeWorkerRecordDir,
	}
}

// agentDirSane is the production AgentDirSane wired into DefaultDeps.
// Mirrors cmd/fleet/maintenance.go:agentDirSane — duplicated rather than
// imported because cmd/fleet → internal/gc is the dependency direction
// (gc cannot depend back on cmd/fleet). Returns an error when the
// live-agent state root is missing or not a directory; the orphan-tmux
// classifier fails closed on that error.
func agentDirSane() error {
	dir, err := state.AgentDir()
	if err != nil {
		return fmt.Errorf("resolve agent dir: %w", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("stat agent dir %s: %w (live-agent state root unavailable — refusing to sweep, every session would misclassify as orphan)",
			dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("agent dir %s exists but is not a directory (live-agent state root corrupted — refusing to sweep)",
			dir)
	}
	return nil
}

// orphanTmuxFreshness mirrors cmd/fleet/maintenance.go:pruneOrphanTmuxFreshness.
// Both call sites must agree: spawn.Spawn creates the tmux session, then
// resolves the engine pid (up to spawn.PidResolveTimeout()), then writes
// the record. The freshness floor has to outlast that whole window or
// fleet gc --aggressive --apply will kill an in-flight replacement.
//
// Returns max(OrphanTmuxMinFreshness, 2 * spawn.PidResolveTimeout()).
func orphanTmuxFreshness() time.Duration {
	dynamicCeiling := 2 * spawn.PidResolveTimeout()
	if dynamicCeiling > OrphanTmuxMinFreshness {
		return dynamicCeiling
	}
	return OrphanTmuxMinFreshness
}

// scanSocketsDir returns every entry matching fleet-test-*.sock under
// dir. Used by the production socket lister with dir=/tmp. Both files
// AND directories are honored — fleet's own temp-dir convention spawns
// some test fixtures into /tmp/fleet-test-<hex>/ trees, and those leak
// the same way.
func scanSocketsDir(dir string) ([]SocketInfo, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var infos []SocketInfo
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "fleet-test-") {
			continue
		}
		if !strings.HasSuffix(name, ".sock") {
			// Out of scope — `fleet-test-*` without `.sock` includes
			// tmuxtest's temp-dir contents which Go's t.TempDir
			// already reaps. Future expansion can broaden this.
			continue
		}
		fullPath := filepath.Join(dir, name)
		info, err := e.Info()
		if err != nil {
			continue
		}
		infos = append(infos, SocketInfo{Path: fullPath, ModTime: info.ModTime()})
	}
	return infos, nil
}

// removeSocketFile is the production RemoveSocket. Tolerates a
// concurrent removal (ENOENT collapses to nil) so two operators
// running `fleet gc --apply` back-to-back don't see spurious errors.
func removeSocketFile(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// socketLiveOnDisk probes whether a tmux server is currently bound to
// the given socket path. Returns true ONLY if `tmux -S <path>
// list-sessions` succeeds (or exits with non-empty output indicating a
// server). Anything else (file gone, connection refused, no server
// running) returns false.
//
// Codex iter-4 [P1]: prevents `fleet gc --apply` from unlinking a
// live tmux socket whose mtime drifted past --max-age. Without this,
// a long-running test fixture's clients would lose ability to connect
// (`tmux -S <path>` would fail to find the socket file).
func socketLiveOnDisk(path string) bool {
	// Cheap file probe first — if the path isn't even there, no server.
	if _, err := os.Stat(path); err != nil {
		return false
	}
	cmd := exec.Command("tmux", "-S", path, "list-sessions")
	if err := cmd.Run(); err != nil {
		// Non-zero exit = no server / no sessions / connection refused.
		// All of those are "not live" for our purposes.
		return false
	}
	return true
}

// listProjectsOnDisk enumerates ~/.fleet/projects/<name>/ — every
// directory entry is a project, except the reserved `.locks` subdir.
// Unknown ~/.fleet root (FLEET_HOME unset, home dir resolution
// failure) returns the error so the operator sees the gap; an empty
// projects/ dir returns (nil, nil).
func listProjectsOnDisk() ([]string, error) {
	root, err := state.Root()
	if err != nil {
		return nil, err
	}
	pdir := filepath.Join(root, "projects")
	entries, err := os.ReadDir(pdir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", pdir, err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if name == ".locks" {
			continue
		}
		if err := state.ValidateProjectName(name); err != nil {
			// Hand-edited / pre-validation legacy entry — skip rather
			// than fail closed; operator-visible via the rest of the
			// fleet CLI.
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// listProjectWorktrees scans ~/.fleet/projects/<project>/worktrees/
// and yields one WorktreeEntry per directory. Slug = dir basename.
func listProjectWorktrees(project string) ([]WorktreeEntry, error) {
	dir, err := state.ProjectDir(project)
	if err != nil {
		return nil, err
	}
	wtRoot := filepath.Join(filepath.Clean(dir), "worktrees")
	entries, err := os.ReadDir(wtRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", wtRoot, err)
	}
	var out []WorktreeEntry
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		out = append(out, WorktreeEntry{
			Project: project,
			Slug:    e.Name(),
			Path:    filepath.Join(wtRoot, e.Name()),
		})
	}
	return out, nil
}

// removeGitWorktree runs `git -C <path> worktree remove --force <path>`.
// `-C <worktree-path>` makes git auto-resolve the linked main repo via
// the `gitdir:` pointer in the worktree's .git file, so the caller
// doesn't need to know where the canonical repo lives.
//
// Best-effort: if the path is already gone, return nil. If git itself
// fails, return the wrapped error — the caller surfaces it on the
// per-action Reason field.
func removeGitWorktree(path string) error {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat %s: %w", path, err)
	}
	cmd := exec.Command("git", "-C", path, "worktree", "remove", "--force", path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git worktree remove %s: %w (%s)", path, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// isTaskTerminalOnDisk reads <project>/tasks.md AND
// <project>/tasks-archive.md and returns true when slug appears with
// status=done OR status=abandoned in either file. Archived tasks are
// terminal by definition, so a slug only found in tasks-archive.md is
// also terminal.
//
// On read error, returns (false, err) so the caller's surface-don't-
// silo gate keeps the worktree.
//
// Slug not present in either file → returns (false, nil). The worktree
// is kept. Rationale: a worktree dir whose name doesn't match any task
// entry could be an operator-side rename, a partial workflow, or a
// truncated slug (the worktree dir basename sometimes drops the trailing
// `-<random>` suffix — `dispatch-lifecycle-pr2` directory vs
// `dispatch-lifecycle-pr2-e-396f` task slug, observed during PR-A smoke
// test). Auto-removing on absent task entry would delete work that
// `fleet gc` couldn't tie back to a clear "done|abandoned" signal,
// violating feedback_surface_dont_silo.md. The bare directory still
// shows up under `--project <name>` enumeration so the operator can
// triage manually.
//
// Parser: lightweight fence-aware reader (taskStatusInFile). Codex
// iter-3 [P1]: an earlier naive line-scanning parser could be spoofed
// by a `## task: <live-slug>` block embedded inside a fenced markdown
// example, classifying the live worktree as terminal and removing it
// under --apply. taskStatusInFile now tracks `inFence` state across
// the whole file (mirroring internal/tasks/tasks.go parse() at line
// 241+) so fenced examples cannot impersonate structural headers.
func isTaskTerminalOnDisk(project, slug string) (bool, error) {
	dir, err := state.ProjectDir(project)
	if err != nil {
		return false, err
	}
	// tasks.md is checked first: live status (done|abandoned) is
	// terminal, anything else (in-progress, todo, ready, blocked,
	// in-review) is non-terminal.
	tasksPath := filepath.Join(filepath.Clean(dir), "tasks.md")
	terminal, found, ferr := taskStatusInFile(tasksPath, slug)
	if ferr != nil {
		return false, ferr
	}
	if found {
		return terminal, nil
	}
	// tasks-archive.md: presence ALONE is terminal regardless of
	// the recorded status. `fleet tasks archive` (internal/tasks.go
	// Archive) moves rows verbatim without forcing status=done, so an
	// operator-forced archive of an `in-progress` task leaves the slug
	// in tasks-archive.md with status=in-progress. The worktree for an
	// archived row is unambiguously inactive (the task is no longer in
	// the live list), so the docstring's "archived tasks are terminal
	// by definition" rule is the right gate (codex iter-3 [P2]).
	archivePath := filepath.Join(filepath.Clean(dir), "tasks-archive.md")
	_, foundInArchive, aerr := taskStatusInFile(archivePath, slug)
	if aerr != nil {
		return false, aerr
	}
	if foundInArchive {
		return true, nil
	}
	// Slug not in either file → keep the worktree. See doc above.
	return false, nil
}

// taskStatusInFile parses one tasks.md / tasks-archive.md file looking
// for `## task: <slug>` followed by a `- status: <value>` bullet
// within the block. Returns:
//
//	terminal=true, found=true   — status is done or abandoned
//	terminal=false, found=true  — status is anything else
//	found=false                 — slug not present in this file
//
// Fence-aware: lines inside ```-delimited fenced code blocks are body
// content, NOT structural headers. Codex iter-3 [P1]: without this
// guard, a `## task: <live-slug>` block embedded inside a fenced
// markdown example could spoof a terminal status for the live worktree
// and `fleet gc --apply --kinds worktrees` would delete it. The
// canonical parser at internal/tasks/tasks.go enforces the same fence
// rule (`inFence` toggle); we replicate the toggle here rather than
// call tasks.Read because the canonical reader is strict about the 10
// required bullets (status, priority, worker_pid, ...) which legitimate
// hand-edited / pre-v0.6 task files may not carry. Surface-don't-silo
// + only-care-about-status → narrowest fence-aware reader is the right
// shape for a read-only sweep classifier.
//
// Grammar (subset of ENG §3.1):
//
//	task-header := "## task: " <slug>
//	body        := lines until next ## header or EOF, fences inert
//	status      := "- status: " <value>
//
// The parser is deliberately minimal (no validator for the other
// 9 bullets) because gc only needs the status token; richer parsing
// would re-create the brittleness the canonical reader's strict mode
// exposes.
func taskStatusInFile(path, slug string) (terminal, found bool, err error) {
	data, rerr := os.ReadFile(path)
	if rerr != nil {
		if os.IsNotExist(rerr) {
			return false, false, nil
		}
		return false, false, fmt.Errorf("read %s: %w", path, rerr)
	}
	wantHeader := "## task: " + slug
	lines := strings.Split(string(data), "\n")
	inFence := false
	for i, line := range lines {
		// Fence toggle MUST run before the header check so that the
		// opening ``` itself doesn't get mistaken for body content of
		// the previous block. Fences are recognized at column 0 only
		// (matching internal/tasks/tasks.go parse() line 241+) — an
		// indented "```" is example text, not a fence boundary.
		if strings.HasPrefix(line, "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		// Codex iter-3 [P2]: structural headers live at column 0. An
		// indented `    ## task: <slug>` is example/body text and must
		// not match. Mirrors the canonical parser's column-0 rule.
		if line != wantHeader {
			continue
		}
		// Scan forward until the next column-0 `## ` header (block
		// boundary) or EOF, looking for the column-0 `- status:`
		// bullet. Honor fence state in the inner scan too — a fenced
		// block inside Notes can legitimately contain `- status: done`
		// as example text. Indented `- status:` bullets are body text
		// (the spec/acceptance/notes sub-bullets) and don't count.
		innerFence := false
		for j := i + 1; j < len(lines); j++ {
			lj := lines[j]
			if strings.HasPrefix(lj, "```") {
				innerFence = !innerFence
				continue
			}
			if innerFence {
				continue
			}
			if strings.HasPrefix(lj, "## ") {
				break
			}
			if strings.HasPrefix(lj, "- status:") {
				val := strings.TrimSpace(strings.TrimPrefix(lj, "- status:"))
				return val == "done" || val == "abandoned", true, nil
			}
		}
		// Found the block but no status bullet — defensive: treat as
		// non-terminal (don't reap on a malformed entry).
		return false, true, nil
	}
	return false, false, nil
}
