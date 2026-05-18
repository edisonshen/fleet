package rc

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/projects"
	"github.com/edisonshen/fleet/internal/tmux"
)

// ErrCwdUnresolvable is returned by ResolveWorkingDir when none of
// the resolution sources yielded a working_dir. The wrapped error
// message guides the operator toward the recovery commands (per
// codex round 5: `fleet project add <path>` is positional).
var ErrCwdUnresolvable = errors.New("rc: cannot determine working dir")

// ResolveWorkingDir returns the canonical working directory for
// project's RC listener. Resolution order (DESIGN §"Working-dir
// provenance"):
//
//  1. override (operator's --cwd flag) — highest priority.
//  2. ~/.fleet/projects/<p>/meta.json:repo_path.
//  3. First alive agent for project's .Cwd (any role).
//  4. Fail with ErrCwdUnresolvable.
//
// All resolved values are canonicalized to absolute paths via
// filepath.Abs before being returned (codex round-6 P2). Relative
// inputs like `--cwd .` would otherwise be persisted to
// rc-state.json verbatim and break lsof-based cwd comparisons in
// Down / Reset / Connect (lsof always reports absolute cwd).
//
// Callers persist the resolved value into rc-state.json:working_dir
// so subsequent operations (Down, Sweep) use the recorded source-of-
// truth rather than re-deriving (which can drift if meta.json moves).
func ResolveWorkingDir(project, override string) (string, error) {
	if override != "" {
		return canonicalCwd(override)
	}

	// (2) meta.json:repo_path — the operator's `fleet project add <path>`
	// commit. Set on every git-mode project; absent on legacy or hand-
	// edited trees.
	if m, err := projects.Read(project); err == nil && m.RepoPath != "" {
		return canonicalCwd(m.RepoPath)
	}

	// (3) Live coord's recorded Cwd. agent.List enumerates all agents
	// in ~/.fleet/agents/; we walk for the FIRST alive COORD agent
	// matching project. Workers are filtered out — their Cwd is the
	// worktree path, not the project root, and registering the
	// listener there would mismatch the coord pane's directory-keyed
	// Claude registry (codex round-9 P2). Tests can stub via
	// ResolveWorkingDirAgentList (var seam below) but production
	// uses agent.List directly.
	//
	// codex round-8 P2: skip records whose tmux session is no longer
	// alive. Live JSON files routinely outlive crashed agents — without
	// the liveness check a stale crashed-coord record could win over
	// the active coord and register the listener under the wrong
	// working_dir, breaking every later directory-keyed verify.
	coordTaskID := "coord-" + project
	if records, err := agentList(); err == nil {
		for _, rec := range records {
			if rec == nil || rec.Project != project {
				continue
			}
			if rec.TaskID != coordTaskID {
				continue
			}
			if rec.Cwd == "" {
				continue
			}
			if !sessionAliveForCwd(rec.TmuxSession) {
				continue
			}
			return canonicalCwd(rec.Cwd)
		}
	}

	return "", fmt.Errorf("%w for project %q: pass --cwd <path>, OR re-register the project from the repo root with 'cd <path> && fleet project add <path>' so meta.json carries repo_path", ErrCwdUnresolvable, project)
}

// canonicalCwd converts a possibly-relative path to absolute via
// filepath.Abs. On failure (extremely rare — would imply the working
// directory is itself unreadable), returns the input verbatim so the
// caller still gets a non-empty string and we degrade rather than
// hard-fail Up.
func canonicalCwd(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p, nil
	}
	return abs, nil
}

// agentList is a test seam so cwd_test.go can inject synthetic agent
// records without touching ~/.fleet/agents/ globally. Production uses
// agent.List which reads from disk.
var agentList = func() ([]*agent.Record, error) { return agent.List() }

// sessionAliveForCwd probes whether an agent record's tmux session is
// still alive. Test seam — defaults to tmux.HasSession; cwd_test.go
// stubs to control liveness without driving a real tmux server.
// Empty session means the record predates tmux integration; accept
// it (legacy compatibility).
var sessionAliveForCwd = func(session string) bool {
	if session == "" {
		return true
	}
	return tmuxHasSessionFn(session)
}

// tmuxHasSessionFn is the inner test seam. Production hits the real
// tmux binary via internal/tmux.HasSession; tests can override
// without touching sessionAliveForCwd's "empty session = legacy"
// branch.
var tmuxHasSessionFn = tmux.HasSession
