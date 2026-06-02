package gc

// KindInvalidProjects classifier (invalid-project-dir-guar-d636).
//
// Sweeps malformed ~/.fleet/projects/<name>/ dirs whose name fails
// state.ValidateProjectName — the canonical case being a literal
// "--project" directory born from a `fleet ... --project` flag-misparse
// (the flag token captured as the project NAME). Because "--project"
// sorts before letters it hijacks the dashboard title and inflates the
// project count, so it's user-visible cruft fleet created and must reap.
//
// Reap flow:
//
//	ListProjectDirs ─→ for each ─→ name valid?  ─yes→ skip (real project)
//	                                  │no
//	                                  ▼
//	                            HasTasks?  ─yes→ surface-only (don't reap;
//	                                  │no         operator may have state)
//	                                  ▼
//	                         would-remove / remove (rm -rf)
//
// SURFACE by default (Verb=would-remove in dry-run). --apply rm -rf's
// the dir. The HasTasks guard is the surface-don't-silo safety: a
// malformed name that somehow accreted a tasks.md is reported but never
// auto-deleted — auto-removing a dir with a real task list would be
// data loss, so we hand it to the operator instead.

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/edisonshen/fleet/internal/state"
)

// KindInvalidProjects is the seventh classifier — malformed project dir
// sweep. See the file header for the full contract.
const KindInvalidProjects Kind = "invalid-projects"

// reconcileInvalidProjects enumerates every raw project dir and flags
// those whose name fails validation. Names that validate are real
// projects and are left alone. A malformed name WITH a tasks.md is
// surfaced (Verb=surface) but never removed — surface-don't-silo.
//
// opts.Project scoping does NOT apply here: a malformed name can't be
// the scoping target (it would fail validation upstream), and the whole
// point is to find the cruft regardless of which project the operator
// is focused on. The classifier is a no-op when no malformed dirs exist.
func reconcileInvalidProjects(r *Report, opts Options, deps Deps) error {
	dirs, err := deps.ListProjectDirs()
	if err != nil {
		return err
	}
	valid := deps.ValidProjectName
	if valid == nil {
		valid = func(name string) bool { return state.ValidateProjectName(name) == nil }
	}
	for _, d := range dirs {
		if valid(d.Name) {
			continue // real project — leave alone
		}
		if d.HasTasks {
			// Conservative: a malformed name with a task list might hold
			// real state the operator wants to migrate. Surface, don't
			// auto-delete (feedback_surface_dont_silo).
			r.Actions = append(r.Actions, Action{
				Kind: KindInvalidProjects, Target: d.Path, Verb: VerbSurface,
				Reason: fmt.Sprintf("invalid project name %q but tasks.md present — refusing to auto-remove; migrate or `rm -rf` manually after review", d.Name),
			})
			continue
		}
		act := Action{
			Kind: KindInvalidProjects, Target: d.Path, Verb: VerbWouldRemove,
			Reason: fmt.Sprintf("invalid project name %q (CLI flag-misparse / hand-edit) + no tasks.md", d.Name),
		}
		if opts.Apply {
			// TOCTOU guard (codex iter-1 [P1]): HasTasks was sampled during
			// ListProjectDirs; a concurrent coord/migration can write
			// tasks.md before we rm -rf. Re-stat immediately before the
			// destructive op and FAIL CLOSED — surface instead of deleting
			// on a freshly-appeared tasks.md OR any non-ENOENT stat error.
			// Data safety beats reaping one cruft dir this run.
			if hasTasks, terr := projectHasTasksRecheck(deps, d.Path); terr != nil {
				act.Verb = VerbSurface
				act.Reason = fmt.Sprintf("invalid project name %q but tasks.md stat failed (%v) — refusing to auto-remove; re-run `fleet gc --kinds invalid-projects` after resolving", d.Name, terr)
				r.Actions = append(r.Actions, act)
				continue
			} else if hasTasks {
				act.Verb = VerbSurface
				act.Reason = fmt.Sprintf("invalid project name %q but tasks.md appeared since scan — refusing to auto-remove (concurrent write?); migrate or `rm -rf` manually after review", d.Name)
				r.Actions = append(r.Actions, act)
				continue
			}
			if rerr := deps.RemoveProjectDir(d.Path); rerr != nil {
				act.Reason = fmt.Sprintf("remove failed: %v", rerr)
			} else {
				act.Verb = VerbRemoved
			}
		}
		r.Actions = append(r.Actions, act)
	}
	return nil
}

// listProjectDirsRaw enumerates EVERY ~/.fleet/projects/<name>/ entry
// (including malformed names ListProjects hides) plus whether each holds
// a tasks.md. The reserved ".locks" control dir and dot-prefixed entries
// are skipped — they're not projects and not the classifier's concern.
func listProjectDirsRaw() ([]ProjectDirInfo, error) {
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
	var out []ProjectDirInfo
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		// Reserved control dir + dot-prefixed entries are never projects.
		if name == ".locks" || (len(name) > 0 && name[0] == '.') {
			continue
		}
		path := filepath.Join(pdir, name)
		hasTasks := false
		if _, serr := os.Stat(filepath.Join(path, "tasks.md")); serr == nil {
			hasTasks = true
		}
		out = append(out, ProjectDirInfo{Name: name, Path: path, HasTasks: hasTasks})
	}
	return out, nil
}

// projectHasTasksRecheck re-stats <path>/tasks.md right before the
// destructive remove (codex iter-1 [P1] TOCTOU guard). Uses the injected
// dep when present so tests can simulate a tasks.md racing in; falls back
// to the production stat otherwise.
func projectHasTasksRecheck(deps Deps, path string) (bool, error) {
	if deps.ProjectHasTasks != nil {
		return deps.ProjectHasTasks(path)
	}
	return projectHasTasksNow(path)
}

// projectHasTasksNow reports whether <path>/tasks.md exists. A missing
// file (ENOENT) is the safe-to-remove case (false, nil); any OTHER stat
// error is returned so the caller fails closed rather than guessing.
func projectHasTasksNow(path string) (bool, error) {
	_, err := os.Stat(filepath.Join(path, "tasks.md"))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// removeProjectDirTree rm -rf's a malformed project dir under --apply.
// ENOENT-tolerant so a concurrent reap doesn't surface a spurious error.
func removeProjectDirTree(path string) error {
	if err := os.RemoveAll(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
