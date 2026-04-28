// Package spawn is the shared "create a new agent in a tmux session
// and write its record" code path used by both `fleet dispatch` (no
// origin) and `fleet handoff` (origin = OldRecord).
//
// Centralizing here means the chain-field logic (HandoffNumber,
// LastHandoffPath, HandoffType) lives in one place. Dispatch and
// handoff differ only in whether they pass an OldRecord.
//
// Failure mode: if the tmux session comes up but the agent record
// write fails, kill the orphan session before returning. Operators
// must never see a "ghost" session with no record (or a record
// pointing at a dead session).
package spawn

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/handoff"
	"github.com/edisonshen/fleet/internal/tmux"
)

// Options control a single Spawn call.
//
// OldRecord nil → fresh dispatch (uses TaskID, Project, no chain).
// OldRecord non-nil → handoff replacement (inherits TaskID/Project/
// Engine/Role/Mode from old, increments HandoffNumber, sets
// LastHandoffPath = NewDocPath if non-empty).
type Options struct {
	OldRecord *agent.Record

	// NewDocPath is the path to the handoff doc this new agent
	// inherits. Only meaningful when OldRecord is non-nil. Stored on
	// the new record's LastHandoffPath so the *next* handoff can
	// build the chain forward.
	NewDocPath string

	// TaskID and Project are used only for fresh dispatch. Ignored
	// when OldRecord is non-nil — the handoff inherits those fields.
	TaskID  string
	Project string

	// Cwd is the working directory for the spawned tmux session.
	// Empty inherits the caller's cwd.
	Cwd string

	// Command is the argv of the agent process inside tmux. Defaults
	// to {"claude"} via the dispatch CLI; spawn.Spawn does not
	// default — callers must pass it explicitly so the contract is
	// obvious.
	Command []string
}

// Spawn creates a fresh agent (or a handoff replacement, if
// opts.OldRecord is set), brings up its tmux session, and writes
// the agent record. Returns the populated record.
//
// The caller is responsible for killing the OLD agent's tmux session
// (graceful /exit + grace + Kill) and archiving the OLD record.
// Spawn handles only the *new* agent.
//
// On any failure after tmux.Spawn succeeds (e.g., record write
// rejected by disk), the orphan tmux session is killed before
// returning so dispatch is exactly-once from the operator's view.
func Spawn(opts Options) (*agent.Record, error) {
	if len(opts.Command) == 0 {
		return nil, fmt.Errorf("spawn.Spawn: Command required")
	}

	id := agent.NewID()
	session := tmux.SessionName(id)
	rec := agent.New(id)
	rec.TmuxSession = session
	rec.PID = os.Getpid()

	// Capture the resolved cwd so `fleet handoff` can place the
	// replacement in the same project checkout even when invoked
	// from a different shell. Empty opts.Cwd means "inherit caller"
	// — resolve via os.Getwd(). Relative paths from --cwd get
	// canonicalized via filepath.Abs so the record always stores
	// an absolute path, immune to "next handoff invoked from a
	// different shell" wrong-tree spawns.
	cwd := opts.Cwd
	if cwd == "" {
		if wd, err := os.Getwd(); err == nil {
			cwd = wd
		}
	} else if !filepath.IsAbs(cwd) {
		if abs, err := filepath.Abs(cwd); err == nil {
			cwd = abs
		}
	}
	rec.Cwd = cwd

	// Capture the launch command so `fleet handoff` preserves any
	// custom engine/wrapper the operator dispatched with.
	rec.Command = append([]string(nil), opts.Command...)

	// FLEET_AGENT_ID is propagated into the agent's process env so
	// fleet-guard (4b/c) can identify which agent record to update
	// without round-tripping via tmux session name parsing.
	extraEnv := []string{"FLEET_AGENT_ID=" + id}

	if opts.OldRecord != nil {
		// Inherit task identity from outgoing agent.
		rec.TaskID = opts.OldRecord.TaskID
		rec.Project = opts.OldRecord.Project
		// Inherit engine + role + mode so the replacement runs in the
		// same configuration. v1.1 engine adapter relies on this for
		// per-agent engine continuity.
		if opts.OldRecord.Engine != "" {
			rec.Engine = opts.OldRecord.Engine
		}
		if opts.OldRecord.Role != "" {
			rec.Role = opts.OldRecord.Role
		}
		if opts.OldRecord.Mode != "" {
			rec.Mode = opts.OldRecord.Mode
		}
		// Chain: handoff_number = old + 1, prev_path = doc just written.
		rec.HandoffNumber = opts.OldRecord.HandoffNumber + 1
		if opts.NewDocPath != "" {
			rec.LastHandoffPath = &opts.NewDocPath
		}
		// Mark the spawn origin so the TUI can render the transition.
		manualType := handoff.TypeManual
		rec.HandoffType = &manualType
	} else {
		rec.TaskID = opts.TaskID
		rec.Project = opts.Project
	}

	// Pass the canonicalized cwd (not opts.Cwd) so the tmux session
	// actually starts in the directory we recorded on rec.Cwd.
	// Otherwise a relative --cwd resolved to one path here could
	// resolve to a different one inside tmux (especially with an
	// existing tmux server), and a future handoff would land the
	// replacement in the wrong checkout.
	if err := tmux.Spawn(session, cwd, opts.Command, extraEnv); err != nil {
		return nil, err
	}
	if err := rec.Write(); err != nil {
		// Orphan rollback: tmux session up, record missing → operator
		// would see a ghost session in `tmux ls` with no `fleet status`
		// entry. Kill the session so spawn is all-or-nothing.
		_ = tmux.Kill(session)
		return nil, fmt.Errorf("write agent record (orphan tmux session killed): %w", err)
	}
	return rec, nil
}
