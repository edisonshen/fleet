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
	"strings"
	"time"

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
		// A leftover quarantine dir is fleet's own cruft and is always
		// reapable, even though its dotted name happens to pass
		// ValidateProjectName (dots + alnum are allowed). Treat it like a
		// malformed dir so the same tasks.md-guarded reap applies (codex
		// iter-5 [P2]).
		isQuarantine := strings.Contains(d.Name, quarantineMarker)
		if !isQuarantine && valid(d.Name) {
			continue // real project — leave alone
		}
		if d.HasTasks {
			// Conservative: a malformed name (or stranded quarantine) with
			// a task list might hold real state the operator wants to
			// migrate. Surface, don't auto-delete (feedback_surface_dont_silo).
			reason := fmt.Sprintf("invalid project name %q but tasks.md present — refusing to auto-remove; migrate or `rm -rf` manually after review", d.Name)
			if isQuarantine {
				reason = fmt.Sprintf("stranded gc quarantine %q still holds tasks.md (failed restore?) — refusing to auto-remove; recover state then `rm -rf` manually", d.Name)
			}
			r.Actions = append(r.Actions, Action{
				Kind: KindInvalidProjects, Target: d.Path, Verb: VerbSurface,
				Reason: reason,
			})
			continue
		}
		reason := fmt.Sprintf("invalid project name %q (CLI flag-misparse / hand-edit) + no tasks.md", d.Name)
		if isQuarantine {
			reason = fmt.Sprintf("stranded gc quarantine %q + no tasks.md — reaping fleet's own leftover", d.Name)
		}
		act := Action{
			Kind: KindInvalidProjects, Target: d.Path, Verb: VerbWouldRemove,
			Reason: reason,
		}
		if opts.Apply {
			applyInvalidProjectReap(&act, deps, d)
		}
		r.Actions = append(r.Actions, act)
	}
	return nil
}

// applyInvalidProjectReap performs the destructive removal under --apply,
// closing the TOCTOU data-loss window codex flagged (iter-1/iter-2 [P1]).
//
// A plain "re-stat tasks.md, then rm -rf" still races: a coord/migration
// can write tasks.md AFTER the stat but BEFORE the RemoveAll, and the new
// task list is destroyed. The fix is to make the gate atomic against the
// LIVE path by quarantining first:
//
//	rename(dir → dir.gc-quarantine)   ← atomic; live writers now miss
//	  │                                 the tree (they hit the old path,
//	  │                                 which no longer exists / is recreated
//	  │                                 fresh — never our quarantined tree)
//	  ▼
//	stat(quarantine/tasks.md)
//	  ├─ present / stat-err → rename back (restore) + surface, never delete
//	  └─ absent            → RemoveAll(quarantine)
//
// Once renamed, nothing using the canonical projects/<name> path can add a
// tasks.md into the tree we're about to delete, so the post-quarantine
// stat is authoritative. Fail closed on any quarantine/stat/restore error.
func applyInvalidProjectReap(act *Action, deps Deps, d ProjectDirInfo) {
	// Pre-quarantine fast path: if tasks.md is already visible, surface
	// without even touching the dir (cheap, common no-op).
	if hasTasks, terr := projectHasTasksRecheck(deps, d.Path); terr != nil {
		act.Verb = VerbSurface
		act.Reason = fmt.Sprintf("invalid project name %q but tasks.md stat failed (%v) — refusing to auto-remove; re-run `fleet gc --kinds invalid-projects` after resolving", d.Name, terr)
		return
	} else if hasTasks {
		act.Verb = VerbSurface
		act.Reason = fmt.Sprintf("invalid project name %q but tasks.md present — refusing to auto-remove; migrate or `rm -rf` manually after review", d.Name)
		return
	}

	quarantine := deps.QuarantineProjectDir
	if quarantine == nil {
		quarantine = quarantineProjectDir
	}
	restore := deps.RestoreProjectDir
	if restore == nil {
		restore = restoreProjectDir
	}

	qpath, qerr := quarantine(d.Path)
	if qerr != nil {
		act.Verb = VerbSurface
		act.Reason = fmt.Sprintf("invalid project name %q — quarantine rename failed (%v); refusing to auto-remove", d.Name, qerr)
		return
	}

	// Authoritative re-stat on the quarantined tree: no live writer can
	// reach it anymore, so this result is stable through the delete.
	if hasTasks, terr := projectHasTasksRecheck(deps, qpath); terr != nil || hasTasks {
		// tasks.md raced in (or stat ambiguous) — un-quarantine and surface.
		reason := "tasks.md appeared during reap (concurrent write?)"
		if terr != nil {
			reason = fmt.Sprintf("tasks.md stat failed (%v)", terr)
		}
		if rerr := restore(qpath, d.Path); rerr != nil {
			// Restore failed: the dir is stranded at the quarantine path.
			// Surface loudly with the exact path so the operator recovers
			// it (surface-don't-silo) rather than silently losing state.
			act.Verb = VerbSurface
			act.Reason = fmt.Sprintf("invalid project name %q — %s; auto-remove aborted but RESTORE FAILED (%v); recover state from %q manually", d.Name, reason, rerr, qpath)
			return
		}
		act.Verb = VerbSurface
		act.Reason = fmt.Sprintf("invalid project name %q but %s — refusing to auto-remove; migrate or `rm -rf` manually after review", d.Name, reason)
		return
	}

	if rerr := deps.RemoveProjectDir(qpath); rerr != nil {
		act.Reason = fmt.Sprintf("remove failed: %v", rerr)
		return
	}
	act.Verb = VerbRemoved
}

// quarantineMarker tags the dot-prefixed sibling dirs quarantineProjectDir
// creates. listProjectDirsRaw deliberately re-surfaces these (despite the
// dot prefix) so a stranded quarantine from a failed restore is reapable by
// fleet itself — fleet owns the lifecycle of everything it creates
// (feedback_fleet_owns_its_resources). codex iter-5 [P2].
const quarantineMarker = ".gc-quarantine."

// listProjectDirsRaw enumerates EVERY ~/.fleet/projects/<name>/ entry
// (including malformed names ListProjects hides) plus whether each holds
// a tasks.md. The reserved ".locks" control dir and OTHER dot-prefixed
// entries are skipped — they're not projects and not the classifier's
// concern. The ONE dot-prefixed exception is fleet's own leftover
// `.<base>.gc-quarantine.<pid>.<nano>` dirs: those are cruft fleet created
// and must reap (a name starting with "." fails ValidateProjectName, so the
// classifier treats them as malformed — reaped if empty, surfaced if they
// still hold a tasks.md from a failed restore).
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
		// Reserved control dir + dot-prefixed entries are never projects,
		// EXCEPT fleet's own leftover quarantine dirs (reap fleet's cruft).
		if name == ".locks" {
			continue
		}
		if len(name) > 0 && name[0] == '.' && !strings.Contains(name, quarantineMarker) {
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

// quarantineProjectDir atomically renames a malformed project dir to a
// UNIQUE dot-prefixed sibling before deletion (codex iter-2/iter-3 [P1]
// TOCTOU close). Same-parent rename is atomic on a single filesystem, so
// after this returns no live writer using the canonical projects/<name>
// path can mutate the tree we're about to inspect+delete. The "." prefix
// keeps the quarantine out of listProjectDirsRaw / the dashboard scan (both
// skip dot-prefixed entries) so a half-finished reap can't resurface as a
// phantom project.
//
// The path is made unique with pid+nanotime so two concurrent
// `fleet gc --apply` runs can't collide on a deterministic name (iter-3
// [P1.1]). We NEVER blindly RemoveAll a pre-existing quarantine: a leftover
// from a failed restore may still hold a real tasks.md, and deleting it as
// "stale" would be exactly the data loss the quarantine exists to prevent
// (iter-3 [P1.2]). Instead we fail closed if the unique target already
// exists (effectively never, but checked) and leave any orphaned quarantine
// for a future surfacing pass / the operator.
func quarantineProjectDir(path string) (string, error) {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	qpath := filepath.Join(dir,
		fmt.Sprintf(".%s.gc-quarantine.%d.%d", base, os.Getpid(), time.Now().UnixNano()))
	if _, err := os.Lstat(qpath); err == nil {
		return "", fmt.Errorf("quarantine path %s already exists — refusing to overwrite", qpath)
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("stat quarantine path %s: %w", qpath, err)
	}
	if err := os.Rename(path, qpath); err != nil {
		return "", err
	}
	return qpath, nil
}

// restoreProjectDir renames a quarantined dir back to its original path
// when the post-quarantine recheck found tasks.md (un-quarantine). If the
// original path was recreated in the meantime (concurrent writer), the
// rename would clobber it, so we refuse and leave the dir quarantined for
// the operator to merge by hand (the caller surfaces the quarantine path).
func restoreProjectDir(quarantined, original string) error {
	if _, err := os.Lstat(original); err == nil {
		return fmt.Errorf("original path %s reappeared — leaving quarantined to avoid clobber", original)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat %s: %w", original, err)
	}
	return os.Rename(quarantined, original)
}

// removeProjectDirTree rm -rf's a malformed project dir under --apply.
// ENOENT-tolerant so a concurrent reap doesn't surface a spurious error.
func removeProjectDirTree(path string) error {
	if err := os.RemoveAll(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
