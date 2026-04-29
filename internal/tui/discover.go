package tui

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// repoCandidate is one row in the [d] picker list.
type repoCandidate struct {
	// Path is the absolute directory passed to `fleet dispatch --cwd`.
	Path string
	// Display is what the picker shows. Usually the basename, but the
	// "current directory" entry annotates with "(cwd) ".
	Display string
}

// discoverRepos returns candidate directories for the [d] picker.
//
// Order:
//  1. Current working directory (always first if it's accessible).
//  2. Each directory listed in $FLEET_PROJECT_DIRS (path-list separated),
//     or $HOME/projects/ if that env var is unset.
//
// A child of a project directory is included only if it contains a .git
// entry (file or directory — covers worktrees too). The cwd is always
// included even if it isn't a git repo, because the operator may
// reasonably want to dispatch in a subdirectory.
//
// Output is deduped: if cwd lives under a scanned project root, it is
// listed only once (as "(cwd)" at position 0).
func discoverRepos() []repoCandidate {
	out := []repoCandidate{}
	// seen maps the canonical (symlinks-resolved) path to true so the
	// cwd row and a project-dir row pointing at the same physical
	// directory don't both appear. macOS's /private/var symlink is the
	// load-bearing case: Getwd() returns /private/var/... but a tmp
	// project root is created under /var/...; without canonicalization,
	// the two strings differ and the repo is duplicated.
	seen := map[string]bool{}

	if cwd, err := os.Getwd(); err == nil {
		abs, err := filepath.Abs(cwd)
		if err == nil {
			out = append(out, repoCandidate{
				Path:    abs,
				Display: "(cwd) " + filepath.Base(abs),
			})
			seen[canonical(abs)] = true
		}
	}

	dirs := projectDirs()
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		batch := []repoCandidate{}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			path := filepath.Join(dir, e.Name())
			canon := canonical(path)
			if seen[canon] {
				continue
			}
			if _, err := os.Stat(filepath.Join(path, ".git")); err != nil {
				continue
			}
			batch = append(batch, repoCandidate{
				Path:    path,
				Display: e.Name(),
			})
			seen[canon] = true
		}
		// Sort each project root's batch alphabetically. Multiple roots
		// stay in the order the operator listed them — preserving
		// FLEET_PROJECT_DIRS priority.
		sort.Slice(batch, func(i, j int) bool {
			return batch[i].Display < batch[j].Display
		})
		out = append(out, batch...)
	}
	disambiguateDisplays(out)
	return out
}

// disambiguateDisplays adds a parent-directory prefix to any rows
// whose Display would otherwise collide. Operators with both
// ~/work/fleet and ~/personal/fleet should see "work/fleet" and
// "personal/fleet" in the picker, not two indistinguishable "fleet"
// rows. The cwd row keeps its "(cwd) <basename>" label and never
// collides with a project-scan row of the same basename — the (cwd)
// prefix makes the strings distinct.
func disambiguateDisplays(c []repoCandidate) {
	counts := map[string]int{}
	for _, r := range c {
		counts[r.Display]++
	}
	for i := range c {
		if counts[c[i].Display] <= 1 {
			continue
		}
		parent := filepath.Base(filepath.Dir(c[i].Path))
		if parent == "" || parent == "." || parent == string(filepath.Separator) {
			continue
		}
		c[i].Display = parent + "/" + c[i].Display
	}
}

// ProjectTag returns the project name to pass to `fleet dispatch
// --project` for a picked path. Last-two-segments form
// (parent-basename) keeps two checkouts with the same basename
// distinct: ~/work/fleet → "work-fleet", ~/personal/fleet →
// "personal-fleet". Without this, both would tag as "fleet" and
// fleet-guard's per-project locking would serialize unrelated work.
//
// Falls back to plain basename when the path is one segment deep
// (e.g. /tmp). Sanitization strips path separators that would break
// state.ValidateProjectName.
func ProjectTag(p string) string {
	p = filepath.Clean(p)
	base := filepath.Base(p)
	parent := filepath.Base(filepath.Dir(p))
	if parent == "" || parent == "." || parent == string(filepath.Separator) {
		return base
	}
	return parent + "-" + base
}

// canonical returns p with symlinks resolved, falling back to p itself
// if the resolution fails (e.g., the path doesn't exist or symlink
// access is denied). Used only for dedup keying — never for display.
func canonical(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return p
}

// projectDirs resolves the search roots scanned for git repos.
// Empty entries (e.g. trailing colon) are dropped.
func projectDirs() []string {
	if env := os.Getenv("FLEET_PROJECT_DIRS"); env != "" {
		raw := strings.Split(env, string(os.PathListSeparator))
		out := raw[:0]
		for _, d := range raw {
			if d != "" {
				out = append(out, d)
			}
		}
		return out
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{filepath.Join(home, "projects")}
}

// filterCandidates returns a slice of indexes into repos matching the
// case-insensitive substring filter against Display. Returning indexes
// (not the candidates themselves) keeps cursor math stable when callers
// compare against the underlying slice.
//
// Empty filter returns every index in order.
func filterCandidates(repos []repoCandidate, filter string) []int {
	if filter == "" {
		out := make([]int, len(repos))
		for i := range repos {
			out[i] = i
		}
		return out
	}
	f := strings.ToLower(filter)
	out := []int{}
	for i, r := range repos {
		if strings.Contains(strings.ToLower(r.Display), f) {
			out = append(out, i)
		}
	}
	return out
}
