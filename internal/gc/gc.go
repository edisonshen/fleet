// Package gc owns the one-shot orphan-resource reaper invoked from
// `fleet gc`. Scope = fleet's own resources only (per
// feedback_fleet_owns_its_resources.md): /tmp/fleet-test-*.sock,
// ~/.fleet/agents/*.json records whose tmux session is gone,
// fleet-* tmux sessions with no live agent record, and worktrees
// under ~/.fleet/projects/*/worktrees/ whose task has reached a
// terminal status.
//
// Tdd-red stub: Reconcile + scanSocketsDir + the Deps/Options/Report
// types compile but every function returns zero values. The accompanying
// _test.go suite drives the contract that the tdd-green pass must
// satisfy.
package gc

import (
	"time"

	"github.com/edisonshen/fleet/internal/agent"
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
)

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

// SocketInfo is the minimal shape Reconcile needs for sockets:
// path on disk + last-modified time. The default lister
// (scanSocketsDir) populates both from os.Stat; tests fabricate
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
	Apply      bool
	Aggressive bool
	MaxAge     time.Duration
	Kinds      []Kind
	Project    string // empty = all projects (for worktrees + agent scope)
}

// Deps groups every external read/write Reconcile needs. Tests stub
// each field; production wiring is via DefaultDeps. Keeping the
// dependencies explicit (not package-level vars) makes the reuse
// boundary obvious for PR-D's automatic reconciliation in
// `fleet status` / `fleet dispatch`.
type Deps struct {
	Now            func() time.Time
	ListSockets    func() ([]SocketInfo, error)
	RemoveSocket   func(path string) error
	ListAgents     func() ([]*agent.Record, error)
	ArchiveAgent   func(*agent.Record) error
	SessionAlive   func(name string) (bool, error)
	ListSessions   func() ([]tmux.SessionInfo, error)
	KillSession    func(name string) error
	ListProjects   func() ([]string, error)
	ListWorktrees  func(project string) ([]WorktreeEntry, error)
	RemoveWorktree func(path string) error
	IsTaskTerminal func(project, slug string) (bool, error)
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

// Reconcile is the package's only entrypoint. TDD-red: empty body.
func Reconcile(_ Options, _ Deps) (Report, error) { //nolint:unused
	return Report{}, nil
}

// scanSocketsDir lists /tmp (or the provided dir) for the
// fleet-test-*.sock pattern. TDD-red: empty.
func scanSocketsDir(_ string) ([]SocketInfo, error) { //nolint:unused
	return nil, nil
}
