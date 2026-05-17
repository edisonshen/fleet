package rc

import (
	"errors"
	"fmt"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/projects"
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
// The override is taken verbatim — empty string falls through.
// Callers persist the resolved value into rc-state.json:working_dir
// so subsequent operations (Down, Sweep) use the recorded source-of-
// truth rather than re-deriving (which can drift if meta.json moves).
func ResolveWorkingDir(project, override string) (string, error) {
	if override != "" {
		return override, nil
	}

	// (2) meta.json:repo_path — the operator's `fleet project add <path>`
	// commit. Set on every git-mode project; absent on legacy or hand-
	// edited trees.
	if m, err := projects.Read(project); err == nil && m.RepoPath != "" {
		return m.RepoPath, nil
	}

	// (3) Live coord's recorded Cwd. agent.List enumerates all agents
	// in ~/.fleet/agents/; we walk for the FIRST alive agent matching
	// project. Tests can stub via ResolveWorkingDirAgentList (var seam
	// below) but production uses agent.List directly.
	if records, err := agentList(); err == nil {
		for _, rec := range records {
			if rec == nil || rec.Project != project {
				continue
			}
			if rec.Cwd == "" {
				continue
			}
			return rec.Cwd, nil
		}
	}

	return "", fmt.Errorf("%w for project %q: pass --cwd <path>, OR re-register the project from the repo root with 'cd <path> && fleet project add <path>' so meta.json carries repo_path", ErrCwdUnresolvable, project)
}

// agentList is a test seam so cwd_test.go can inject synthetic agent
// records without touching ~/.fleet/agents/ globally. Production uses
// agent.List which reads from disk.
var agentList = func() ([]*agent.Record, error) { return agent.List() }
