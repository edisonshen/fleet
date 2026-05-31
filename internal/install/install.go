// Package install owns the policy for how Fleet's bundled skills land in
// ~/.claude/skills/<name>/. There are two install shapes:
//
//	COPY    — the embedded skill bytes are written to disk (the historical
//	          `fleet init` behavior). Self-contained: works on a brew /
//	          binary-only machine that has no repo checkout. Cost: a merged
//	          skill fix only reaches the running coord after the operator
//	          re-runs `fleet init --upgrade` with a binary that embeds the
//	          fix. This is the gap that silently neutered a merged P0
//	          (#182): the install was a hand-copied snapshot, so fixes never
//	          went live.
//
//	SYMLINK — ~/.claude/skills/<name> is a symlink to the repo checkout's
//	          skills/<name>/ directory. Edits / merges in the checkout go
//	          live immediately for the next coord spawn. This is the right
//	          shape for anyone developing ON fleet (the common case for this
//	          operator). Cost: the skill follows whatever the checkout's
//	          working tree is — if the checkout sits on a feature branch the
//	          coord runs that branch's skill. Documented tradeoff.
//
// Policy (documented in README / CLAUDE.md): symlink when a repo checkout
// is locatable, copy otherwise. `fleet skills link` installs the symlink;
// `fleet skills sync` / `fleet init` install the copy.
//
// Stale-copy detection: a COPY install can silently diverge from the repo
// the operator is actively editing. `Status` compares the installed copy
// against the repo checkout (when one is locatable) and flags divergence so
// `fleet skills status` and coord startup can warn (surface-dont-silo).
package install

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/edisonshen/fleet/internal/state"
)

// Shape classifies how a skill currently sits on disk.
type Shape int

const (
	// ShapeMissing — nothing at ~/.claude/skills/<name>.
	ShapeMissing Shape = iota
	// ShapeSymlink — a symlink (assumed to point at a repo checkout; live).
	ShapeSymlink
	// ShapeCopy — a real directory holding copied skill files.
	ShapeCopy
)

func (s Shape) String() string {
	switch s {
	case ShapeMissing:
		return "missing"
	case ShapeSymlink:
		return "symlink"
	case ShapeCopy:
		return "copy"
	default:
		return "unknown"
	}
}

// SkillStatus is the diagnostic Status returns per skill.
type SkillStatus struct {
	// Name is the skill directory name ("coordinator", "fleet-guard").
	Name string
	// Shape is how it sits on disk.
	Shape Shape
	// Target is the symlink destination (only set when Shape==ShapeSymlink).
	Target string
	// Stale is true when Shape==ShapeCopy AND a repo checkout was located
	// AND the installed copy diverges from the checkout's skill files.
	// A symlink is never stale (it IS the checkout). A copy with no
	// locatable checkout is not flagged stale — there's nothing to
	// compare against, so we can't claim divergence.
	Stale bool
	// DivergedFiles lists the relative paths that differ (or are missing)
	// between the installed copy and the repo checkout. Empty unless Stale.
	DivergedFiles []string
	// RepoSkillDir is the checkout's skills/<name>/ dir used for the
	// comparison, when one was located. Empty otherwise.
	RepoSkillDir string
}

// SkillDir returns ~/.claude/skills/<name> under claudeHome. claudeHome
// must be the resolved ~/.claude path (callers resolve via os.UserHomeDir
// or a test override) so this package never reaches for the real HOME on
// its own — keeps it test-injectable end to end.
func SkillDir(claudeHome, name string) string {
	return filepath.Join(claudeHome, "skills", name)
}

// classify reports the on-disk Shape of dir. A dangling symlink still
// classifies as ShapeSymlink — the operator pointed it somewhere; we
// surface the (possibly broken) target rather than silently treating it
// as missing and clobbering it with a copy.
func classify(dir string) (Shape, string, error) {
	fi, err := os.Lstat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return ShapeMissing, "", nil
		}
		return ShapeMissing, "", fmt.Errorf("lstat %s: %w", dir, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		target, rerr := os.Readlink(dir)
		if rerr != nil {
			return ShapeSymlink, "", fmt.Errorf("readlink %s: %w", dir, rerr)
		}
		return ShapeSymlink, target, nil
	}
	return ShapeCopy, "", nil
}

// IsSymlink reports whether the skill at claudeHome/skills/<name> is a
// symlink. Used by the autoinit path to skip the byte-compare refresh that
// would otherwise clobber a live symlink with an embedded copy (the repo
// checkout legitimately differs from the binary's embedded bytes when the
// checkout is ahead of the last build). Errors collapse to false so a
// classify failure makes autoinit do its normal (safe) copy work.
func IsSymlink(claudeHome, name string) bool {
	shape, _, err := classify(SkillDir(claudeHome, name))
	return err == nil && shape == ShapeSymlink
}

// Link points claudeHome/skills/<name> at repoSkillDir via a symlink.
//
// repoSkillDir must be an existing directory holding the skill's source
// (e.g. <repo>/skills/coordinator).
//
// Replacement policy (this is the headline upgrade path — `fleet skills
// link` with no flags must Just Work on the common stale-copy machine):
//   - COPY → replaced automatically. A copy is regenerable embedded bytes,
//     not operator-authored state, so converting it to a live symlink is
//     exactly what the documented no-flag command promises. Requiring
//     --force here would make `fleet skills link` fail on every machine
//     that ran `fleet init` first (the normal case) — defeating the point.
//   - SYMLINK pointing ELSEWHERE → requires force. An existing symlink is a
//     deliberate operator choice (maybe a second checkout); silently
//     re-pointing it would blow away that intent (surface-dont-silo). force
//     opts into the re-point.
//   - SYMLINK already pointing at abs → no-op skip (idempotent).
//
// The replacement is NOT atomic across the unlink+symlink window — a crash
// between the two leaves the skill missing, which autoinit / `fleet init`
// re-creates on the next run. We accept that over an O_TMPFILE rename dance
// because a skill dir is not hot-path state and the recovery is automatic.
func Link(stdout io.Writer, claudeHome, name, repoSkillDir string, force bool) error {
	fi, err := os.Stat(repoSkillDir)
	if err != nil {
		return fmt.Errorf("link %s: source %s: %w", name, repoSkillDir, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("link %s: source %s is not a directory", name, repoSkillDir)
	}
	abs, err := filepath.Abs(repoSkillDir)
	if err != nil {
		return fmt.Errorf("link %s: resolve source abs: %w", name, err)
	}

	dst := SkillDir(claudeHome, name)
	shape, target, err := classify(dst)
	if err != nil {
		return err
	}
	switch {
	case shape == ShapeSymlink && target == abs:
		_, _ = fmt.Fprintf(stdout, "skip (already linked): %s -> %s\n", dst, abs)
		return nil
	case shape == ShapeSymlink && !force:
		// An existing symlink to a DIFFERENT checkout is deliberate operator
		// state; never silently re-point it (surface-dont-silo).
		return fmt.Errorf(
			"link %s: %s already symlinked to %q; pass --force to re-point",
			name, dst, target)
	case shape == ShapeSymlink:
		// force-replacing a symlink: remove just the link (RemoveAll on a
		// symlink unlinks it, never recurses into the target's repo dir).
		if rmErr := os.Remove(dst); rmErr != nil {
			return fmt.Errorf("link %s: remove existing symlink: %w", name, rmErr)
		}
	case shape == ShapeCopy:
		// A copy is regenerable embedded bytes — replace it with the live
		// link without requiring --force (this is the documented upgrade).
		if rmErr := os.RemoveAll(dst); rmErr != nil {
			return fmt.Errorf("link %s: remove existing copy: %w", name, rmErr)
		}
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("link %s: mkdir parent: %w", name, err)
	}
	if err := os.Symlink(abs, dst); err != nil {
		return fmt.Errorf("link %s: symlink: %w", name, err)
	}
	_, _ = fmt.Fprintf(stdout, "linked: %s -> %s\n", dst, abs)
	return nil
}

// CopyFromFS writes every file in fsys to claudeHome/skills/<name>/,
// replacing a symlink with a real copy when force is set. This is the
// embedded-bytes install path shared by `fleet init` and `fleet skills
// sync`. force replaces a symlink (operator explicitly asked to go back to
// copy mode); without force a symlink is left intact so a stray `fleet init`
// doesn't silently break a live link.
//
// Files are written via state.WriteAtomic (tmp + fsync + rename) so a
// crashed copy never publishes a half-written loop.py to a running coord.
func CopyFromFS(stdout io.Writer, claudeHome, name string, fsys fs.FS, force bool) error {
	dst := SkillDir(claudeHome, name)
	shape, target, err := classify(dst)
	if err != nil {
		return err
	}
	if shape == ShapeSymlink {
		if !force {
			_, _ = fmt.Fprintf(stdout,
				"skip (symlinked, live): %s -> %s (run `fleet skills sync --force` to convert to copy)\n",
				dst, target)
			return nil
		}
		if rmErr := os.Remove(dst); rmErr != nil {
			return fmt.Errorf("copy %s: remove symlink: %w", name, rmErr)
		}
	}
	return walkCopy(stdout, fsys, dst, force)
}

// walkCopy is the embedded-FS walk shared with the historical init path.
// Kept here so install policy (copy vs symlink) lives in one package; the
// byte-compare idempotency mirrors cmd/fleet/init.go's installSkillFilesFS.
func walkCopy(stdout io.Writer, fsys fs.FS, dst string, force bool) error {
	return fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		target := filepath.Join(dst, path)
		data, err := fs.ReadFile(fsys, path)
		if err != nil {
			return fmt.Errorf("read embedded %s: %w", path, err)
		}
		if !force {
			existing, statErr := os.ReadFile(target)
			switch {
			case statErr == nil && bytes.Equal(existing, data):
				_, _ = fmt.Fprintf(stdout, "skip (up to date): %s\n", target)
				return nil
			case statErr == nil:
				_, _ = fmt.Fprintf(stdout, "refresh (stale): %s\n", target)
			case !os.IsNotExist(statErr):
				return fmt.Errorf("stat %s: %w", target, statErr)
			}
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", filepath.Dir(target), err)
		}
		if err := state.WriteAtomic(target, data); err != nil {
			return fmt.Errorf("write %s: %w", target, err)
		}
		_, _ = fmt.Fprintf(stdout, "wrote: %s\n", target)
		return nil
	})
}

// Status classifies how each skill in fsMap sits on disk and, for COPY
// installs, compares against the repo checkout (when locatable via
// repoSkillDirFor) to flag divergence. names is iterated in sorted order so
// output is deterministic.
//
// repoSkillDirFor maps a skill name to the repo checkout's skills/<name>/
// directory, returning ("", false) when no checkout could be located. The
// caller injects it (LocateRepoSkillDir in production) so this package
// stays free of project-registry plumbing and is trivially testable.
func Status(claudeHome string, fsMap map[string]fs.FS, repoSkillDirFor func(name string) (string, bool)) ([]SkillStatus, error) {
	names := make([]string, 0, len(fsMap))
	for name := range fsMap {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]SkillStatus, 0, len(names))
	for _, name := range names {
		dst := SkillDir(claudeHome, name)
		shape, target, err := classify(dst)
		if err != nil {
			return nil, err
		}
		st := SkillStatus{Name: name, Shape: shape, Target: target}
		if shape == ShapeCopy {
			if repoDir, ok := repoSkillDirFor(name); ok {
				diverged, derr := divergedFiles(fsMap[name], repoDir, dst)
				if derr != nil {
					return nil, derr
				}
				st.RepoSkillDir = repoDir
				if len(diverged) > 0 {
					st.Stale = true
					st.DivergedFiles = diverged
				}
			}
		}
		out = append(out, st)
	}
	return out, nil
}

// divergedFiles returns the relative paths whose bytes differ between the
// repo checkout and the install copy, considering ONLY the files in the
// embedded set (embedFS) — i.e. the exact files the coord actually runs.
//
// Walking the embedded set (not the raw repo dir) is deliberate: the repo's
// skills/<name>/ holds files that are NOT shipped to the install (the
// tests/ subtree, __pycache__, editor backups, or a new .py added to the
// repo but not yet wired into embed.go). Comparing the whole repo dir would
// false-positive on those as "missing in install" and falsely flag a fresh
// copy as stale. The embedded set is the canonical "what the coord runs"
// manifest, so a divergence on one of those files is a real "running stale
// code" signal.
//
// For each embedded file we compare repo-vs-install:
//   - repo missing the file → skip (can't claim drift against a file the
//     checkout doesn't have; the binary is simply newer than the checkout).
//   - install differs from repo (or install missing it) → diverged: the
//     coord is running bytes that don't match the checkout.
func divergedFiles(embedFS fs.FS, repoDir, installedDir string) ([]string, error) {
	var diverged []string
	err := fs.WalkDir(embedFS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		repoData, repoErr := os.ReadFile(filepath.Join(repoDir, p))
		if repoErr != nil {
			// Checkout doesn't have this file — nothing to drift against.
			return nil //nolint:nilerr // intentional: missing-in-repo is not drift
		}
		installedData, statErr := os.ReadFile(filepath.Join(installedDir, p))
		if statErr != nil || !bytes.Equal(repoData, installedData) {
			diverged = append(diverged, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(diverged)
	return diverged, nil
}

// LocateRepoSkillDir finds the fleet repo checkout's skills/<name>/
// directory by scanning registered projects for one whose repo_path holds a
// skills/<name>/SKILL.md. Returns ("", false) when none is found (the
// binary-only / brew install case — copy is the only option).
//
// Why the project registry and not the binary's own path: a brew-installed
// binary lives in /opt/homebrew/bin with no nearby checkout, and
// os.Executable() under `go test` points at a temp build dir. The registry
// is the one place fleet reliably knows where the operator's checkouts live.
// The first match wins after sorting project names for determinism; an
// operator with two fleet checkouts can disambiguate with `fleet skills link
// --from <repo>`.
// Two-stage resolution:
//  1. Project registry — a project whose repo_path holds skills/<name>/SKILL.md.
//  2. Symlink-sibling fallback — if ANY already-installed skill is a symlink
//     into a checkout that ALSO has skills/<name>/, reuse that checkout. This
//     covers the real-world case where the fleet repo predates the meta.json
//     feature (no registry entry) but the operator already linked one skill:
//     the existing symlink IS proof of where the fleet checkout lives, so a
//     sibling skill's stale-copy can still be compared / linked. Without it,
//     fleet-guard on this operator's machine reported "no repo to compare"
//     while coordinator was a live symlink to the very same checkout.
func LocateRepoSkillDir(name string) (string, bool) {
	claudeHome, err := defaultClaudeHome()
	if err != nil {
		return locateViaRegistry(name) // best-effort; sibling fallback needs claudeHome
	}
	return LocateRepoSkillDirIn(claudeHome, name)
}

// LocateRepoSkillDirIn is the claudeHome-injectable form (tests pass a temp
// dir). Production goes through LocateRepoSkillDir which resolves ~/.claude.
func LocateRepoSkillDirIn(claudeHome, name string) (string, bool) {
	if dir, ok := locateViaRegistry(name); ok {
		return dir, true
	}
	return locateViaSymlinkSibling(claudeHome, name)
}

// locateViaRegistry scans ~/.fleet/projects/*/meta.json for a repo_path that
// holds skills/<name>/SKILL.md. First match wins after sorting for
// determinism; an operator with two fleet checkouts disambiguates with
// `fleet skills link --from <repo>`.
//
// The registry is preferred over the binary's own path: a brew-installed
// binary lives in /opt/homebrew/bin with no nearby checkout, and
// os.Executable() under `go test` points at a temp build dir.
func locateViaRegistry(name string) (string, bool) {
	root, err := state.Root()
	if err != nil {
		return "", false
	}
	projectsDir := filepath.Join(root, "projects")
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return "", false
	}
	candidates := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			candidates = append(candidates, e.Name())
		}
	}
	sort.Strings(candidates)
	for _, proj := range candidates {
		metaPath := filepath.Join(projectsDir, proj, "meta.json")
		repoPath, ok := readRepoPath(metaPath)
		if !ok {
			continue
		}
		skillDir := filepath.Join(repoPath, "skills", name)
		if _, err := os.Stat(filepath.Join(skillDir, "SKILL.md")); err == nil {
			return skillDir, true
		}
	}
	return "", false
}

// locateViaSymlinkSibling derives the fleet checkout from an existing
// symlinked skill: if ~/.claude/skills/<other> -> <repo>/skills/<other>,
// then <repo>/skills/<name> is the sibling we want (when it has a SKILL.md).
// Returns the first match in sorted skill-name order for determinism.
func locateViaSymlinkSibling(claudeHome, name string) (string, bool) {
	skillsRoot := filepath.Join(claudeHome, "skills")
	entries, err := os.ReadDir(skillsRoot)
	if err != nil {
		return "", false
	}
	others := make([]string, 0, len(entries))
	for _, e := range entries {
		others = append(others, e.Name())
	}
	sort.Strings(others)
	for _, other := range others {
		shape, target, cerr := classify(filepath.Join(skillsRoot, other))
		if cerr != nil || shape != ShapeSymlink || target == "" {
			continue
		}
		// target is <repo>/skills/<other>; climb to <repo>/skills then sibling.
		siblingRoot := filepath.Dir(target)
		candidate := filepath.Join(siblingRoot, name)
		if _, err := os.Stat(filepath.Join(candidate, "SKILL.md")); err == nil {
			return candidate, true
		}
	}
	return "", false
}

// defaultClaudeHome resolves ~/.claude. Split out so LocateRepoSkillDir can
// fall back gracefully when the home dir is unresolvable.
func defaultClaudeHome() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude"), nil
}

// WarnIfStale writes a one-line-per-skill diagnostic to w for every COPY
// install that has diverged from its repo checkout. It is the
// surface-dont-silo hook the coord supervisor calls at startup: a running
// coord on a stale skill copy is exactly how a merged P0 fix gets silently
// neutered, so we shout about it to stderr with a concrete remediation
// command instead of letting the coord quietly run old code.
//
// Best-effort: any error locating skills or comparing files is swallowed
// (the coord must still start). claudeHome is the resolved ~/.claude path.
// Returns the number of stale skills warned about (0 = clean), so callers /
// tests can assert without re-parsing the text.
func WarnIfStale(w io.Writer, claudeHome string, fsMap map[string]fs.FS) int {
	locate := func(name string) (string, bool) { return LocateRepoSkillDirIn(claudeHome, name) }
	statuses, err := Status(claudeHome, fsMap, locate)
	if err != nil {
		return 0
	}
	warned := 0
	for _, st := range statuses {
		if !st.Stale {
			continue
		}
		warned++
		_, _ = fmt.Fprintf(w,
			"fleet: WARNING — skill %q is a STALE COPY diverged from %s\n",
			st.Name, st.RepoSkillDir)
		_, _ = fmt.Fprintf(w,
			"fleet:   the running coord is using OLD skill code; merged fixes are NOT live.\n")
		_, _ = fmt.Fprintf(w,
			"fleet:   diverged files: %v\n", st.DivergedFiles)
		_, _ = fmt.Fprintf(w,
			"fleet:   fix: run `fleet skills link` (go live) or `fleet skills sync` (re-copy)\n")
	}
	return warned
}

// readRepoPath pulls repo_path out of a project's meta.json. Returns
// ("", false) on any read/parse error or an empty field — a malformed
// meta.json just means "this project can't tell us where its checkout is,"
// not a hard failure for the skill-location scan.
func readRepoPath(metaPath string) (string, bool) {
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return "", false
	}
	var meta struct {
		RepoPath string `json:"repo_path"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return "", false
	}
	if meta.RepoPath == "" {
		return "", false
	}
	return meta.RepoPath, true
}
