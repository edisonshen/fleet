package coordrepo

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/edisonshen/fleet/internal/state"
)

// gitTestEnv neutralizes any ambient git identity/config so the test repos
// are reproducible regardless of the developer's ~/.gitconfig.
func gitTestEnv() []string {
	return append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	)
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = gitTestEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return string(out)
}

// newRepo creates a non-bare git repo at <parent>/<name> with one commit.
// origin (if non-empty) is set as the `origin` remote URL.
func newRepo(t *testing.T, parent, name, origin string) string {
	t.Helper()
	repo := filepath.Join(parent, name)
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	git(t, repo, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-q", "-m", "init")
	if origin != "" {
		git(t, repo, "remote", "add", "origin", origin)
	}
	return eval(t, repo)
}

// eval resolves symlinks so test paths match git's `--git-common-dir`
// output (macOS TempDir is /var → /private/var). Resolver-returned paths
// are symlink-resolved; test repo paths must be too for equality checks.
func eval(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		return p
	}
	return r
}

// newRepoEnv is newRepo but using the AMBIENT process env (it does NOT
// override GIT_CONFIG_GLOBAL), so a t.Setenv("GIT_CONFIG_GLOBAL", ...) in
// the test is honored. Author identity is still pinned via env so commits
// succeed regardless of the developer's global config.
func newRepoEnv(t *testing.T, parent, name, origin string) string {
	t.Helper()
	repo := filepath.Join(parent, name)
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	ambient := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	ambient("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	ambient("add", ".")
	ambient("commit", "-q", "-m", "init")
	if origin != "" {
		ambient("remote", "add", "origin", origin)
	}
	return eval(t, repo)
}

// writeFile is a tiny os.WriteFile wrapper for test fixtures.
func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}

// makeShallow rewrites repo into a shallow clone (depth 1) of itself by
// cloning from a second commit. Simplest reliable way: add a commit, then
// shallow-clone into a fresh dir.
func newShallowRepo(t *testing.T, parent, name, origin string) string {
	t.Helper()
	src := newRepo(t, parent, name+"-src", origin)
	// second commit so depth=1 actually truncates history
	if err := os.WriteFile(filepath.Join(src, "g.txt"), []byte("two"), 0o644); err != nil {
		t.Fatalf("write file2: %v", err)
	}
	git(t, src, "add", ".")
	git(t, src, "commit", "-q", "-m", "second")

	dst := filepath.Join(parent, name)
	cmd := exec.Command("git", "clone", "-q", "--depth", "1", "file://"+src, dst)
	cmd.Env = gitTestEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("shallow clone: %v\n%s", err, out)
	}
	if origin != "" {
		// clone sets origin to the file:// src; override to the requested URL
		git(t, dst, "remote", "set-url", "origin", origin)
	} else {
		git(t, dst, "remote", "remove", "origin")
	}
	return eval(t, dst)
}

// addWorktree registers a worktree of repo under
// ~/.fleet/projects/<project>/worktrees/<name> (branch worker/<name>).
// Returns the worktree path. FLEET_HOME must already be set.
func addWorktree(t *testing.T, repo, project, name string) string {
	t.Helper()
	dir, err := state.ProjectDir(project)
	if err != nil {
		t.Fatalf("ProjectDir: %v", err)
	}
	wtRoot := filepath.Join(filepath.Clean(dir), "worktrees")
	if err := os.MkdirAll(wtRoot, 0o755); err != nil {
		t.Fatalf("mkdir worktrees: %v", err)
	}
	wt := filepath.Join(wtRoot, name)
	git(t, repo, "worktree", "add", "-q", "-b", "worker/"+name, wt)
	return wt
}

// readUUID returns the fleet.repoId config value ("" if unset).
func readUUID(t *testing.T, repo string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", repo, "config", "--get", "fleet.repoId")
	cmd.Env = gitTestEnv()
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return trim(string(out))
}

// projectStateDir returns the cleaned ~/.fleet/projects/<project>/ dir.
func projectStateDir(t *testing.T, project string) string {
	t.Helper()
	dir, err := state.ProjectDir(project)
	if err != nil {
		t.Fatalf("ProjectDir: %v", err)
	}
	return filepath.Clean(dir)
}

func trim(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r' || s[len(s)-1] == ' ') {
		s = s[:len(s)-1]
	}
	return s
}
