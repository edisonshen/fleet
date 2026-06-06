package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edisonshen/fleet/internal/projects"
	"github.com/edisonshen/fleet/internal/state"
)

// TASK-PLAN-resolve-repo-cli-b2d7 tests C1–C10. These exercise the new
// `fleet project resolve-repo` subcommand and the `--project` flag +
// fingerprint stamping on `fleet project add` — the PR2 surface over the
// PR1 resolver. The resolver internals (tier ladder, corroboration) are
// covered by internal/coordrepo; here we test the CLI contract:
// stdout/stderr/exit, tag pinning, and fingerprint stamping.

// gitEnv neutralizes ambient git identity/config so test repos are
// reproducible regardless of the developer's ~/.gitconfig.
func gitEnv() []string {
	return append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	)
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = gitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return string(out)
}

// evalSym resolves symlinks so test paths match the resolver's
// symlink-resolved output (macOS TempDir is /var → /private/var).
func evalSym(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		return p
	}
	return r
}

// newGitRepo creates a non-bare git repo with one commit and an optional
// origin remote. Returns the symlink-resolved path.
func newGitRepo(t *testing.T, parent, name, origin string) string {
	t.Helper()
	repo := filepath.Join(parent, name)
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	runGit(t, repo, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-q", "-m", "init")
	if origin != "" {
		runGit(t, repo, "remote", "add", "origin", origin)
	}
	return evalSym(t, repo)
}

// newShallow makes a depth-1 shallow clone of a freshly-built repo.
func newShallow(t *testing.T, parent, name string) string {
	t.Helper()
	src := newGitRepo(t, parent, name+"-src", "")
	if err := os.WriteFile(filepath.Join(src, "g.txt"), []byte("two"), 0o644); err != nil {
		t.Fatalf("write file2: %v", err)
	}
	runGit(t, src, "add", ".")
	runGit(t, src, "commit", "-q", "-m", "second")
	dst := filepath.Join(parent, name)
	cmd := exec.Command("git", "clone", "-q", "--depth", "1", "file://"+src, dst)
	cmd.Env = gitEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("shallow clone: %v\n%s", err, out)
	}
	return evalSym(t, dst)
}

// addWorktreeForProject registers a worktree of repo under
// ~/.fleet/projects/<project>/worktrees/<name>. FLEET_HOME must be set.
func addWorktreeForProject(t *testing.T, repo, project, name string) string {
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
	runGit(t, repo, "worktree", "add", "-q", "-b", "worker/"+name, wt)
	return wt
}

// resolveRepo invokes the resolve-repo command core, capturing stdout,
// stderr, and whether it returned an error (non-zero exit).
func resolveRepo(t *testing.T, project string, persist bool) (stdout, stderr string, failed bool) {
	t.Helper()
	var so, se bytes.Buffer
	err := runProjectResolveRepo(project, persist, &so, &se)
	return so.String(), se.String(), err != nil
}

// ── C1 — resolve-repo ok ────────────────────────────────────────────────
// meta.json present + identity matches → stdout == path, exit 0, stderr empty.
func TestResolveRepo_C1_OK(t *testing.T) {
	setupFleetHome(t)
	parent := t.TempDir()
	repo := newGitRepo(t, parent, "myrepo", "https://github.com/edisonshen/fleet.git")

	out := &bytes.Buffer{}
	if err := runProjectAdd(repo, "asgard", out, out); err != nil {
		t.Fatalf("add: %v\n%s", err, out)
	}

	stdout, stderr, failed := resolveRepo(t, "asgard", false)
	if failed {
		t.Fatalf("resolve-repo failed; stderr=%s", stderr)
	}
	if got := strings.TrimSpace(stdout); got != repo {
		t.Errorf("stdout=%q want %q", got, repo)
	}
	if stderr != "" {
		t.Errorf("stderr not empty: %q", stderr)
	}
}

// ── C2 — resolve-repo refuse ────────────────────────────────────────────
// no meta, no worktrees → exit non-zero; stderr == §7.1 hint incl. the
// `Run: fleet project add --project <p> <candidate>` line; stdout empty.
func TestResolveRepo_C2_Refuse(t *testing.T) {
	setupFleetHome(t)
	// Project state dir exists (so the tag is "known") but no meta.json and
	// no worktrees.
	if _, err := state.EnsureProjectInitialized("ghost"); err != nil {
		t.Fatalf("init project: %v", err)
	}

	stdout, stderr, failed := resolveRepo(t, "ghost", false)
	if !failed {
		t.Fatalf("expected non-zero exit; stdout=%q stderr=%q", stdout, stderr)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("stdout must be empty on refuse, got %q", stdout)
	}
	if !strings.Contains(stderr, "project ghost: no usable checkout") {
		t.Errorf("stderr missing refusal headline:\n%s", stderr)
	}
	if !strings.Contains(stderr, "Run: fleet project add --project ghost ") {
		t.Errorf("stderr missing the `Run: fleet project add --project ghost <candidate>` line:\n%s", stderr)
	}
	if !strings.Contains(stderr, "launch cwd was intentionally ignored") {
		t.Errorf("stderr missing cwd-ignored note:\n%s", stderr)
	}
}

// ── C3 — resolve-repo --persist writes ──────────────────────────────────
// meta absent + >=2 corroborated worktrees + heuristic match → binds the
// derived path; meta.json now has repo_path + repo_fingerprint.
func TestResolveRepo_C3_PersistWrites(t *testing.T) {
	setupFleetHome(t)
	parent := t.TempDir()
	// Tag "x-asgard" so the fuzzy heuristic (split on first '-' → "asgard")
	// matches the origin basename "asgard".
	project := "x-asgard"
	if _, err := state.EnsureProjectInitialized(project); err != nil {
		t.Fatalf("init project: %v", err)
	}
	repo := newGitRepo(t, parent, "asgard", "https://github.com/edisonshen/asgard.git")
	addWorktreeForProject(t, repo, project, "wt1")
	addWorktreeForProject(t, repo, project, "wt2")

	stdout, stderr, failed := resolveRepo(t, project, true)
	if failed {
		t.Fatalf("resolve-repo --persist failed; stderr=%s", stderr)
	}
	if got := strings.TrimSpace(stdout); got != repo {
		t.Errorf("stdout=%q want derived %q", got, repo)
	}

	m, err := projects.Read(project)
	if err != nil {
		t.Fatalf("read meta after persist: %v", err)
	}
	if m.RepoPath != repo {
		t.Errorf("meta repo_path=%q want %q", m.RepoPath, repo)
	}
	if m.RepoFingerprint == "" {
		t.Errorf("meta repo_fingerprint not stamped after --persist")
	}
}

// ── C4 — resolve-repo without --persist is read-only ────────────────────
// same as C3 but no --persist → prints derived path; meta.json NOT written.
func TestResolveRepo_C4_NoPersistReadOnly(t *testing.T) {
	setupFleetHome(t)
	parent := t.TempDir()
	project := "x-asgard"
	if _, err := state.EnsureProjectInitialized(project); err != nil {
		t.Fatalf("init project: %v", err)
	}
	repo := newGitRepo(t, parent, "asgard", "https://github.com/edisonshen/asgard.git")
	addWorktreeForProject(t, repo, project, "wt1")
	addWorktreeForProject(t, repo, project, "wt2")

	stdout, stderr, failed := resolveRepo(t, project, false)
	if failed {
		t.Fatalf("resolve-repo (no persist) failed; stderr=%s", stderr)
	}
	if got := strings.TrimSpace(stdout); got != repo {
		t.Errorf("stdout=%q want derived %q", got, repo)
	}

	// meta.json must NOT exist (no operator add, no persist).
	if _, err := projects.Read(project); err == nil {
		t.Errorf("meta.json was written on a read-only resolve (no --persist)")
	} else if !strings.Contains(err.Error(), "not found") &&
		!os.IsNotExist(err) {
		// Read returns ErrNotFound for an absent meta; any other error is a
		// genuine failure.
		if _, statErr := os.Stat(metaPathFor(t, project)); statErr == nil {
			t.Errorf("meta.json present on disk after read-only resolve")
		}
	}
}

// ── C5 — add --project pins tag ─────────────────────────────────────────
// `fleet project add --project p <path>` where TagForPath(path) != p →
// registers under tag p (meta.json under <p>/), not the derived tag.
func TestResolveRepo_C5_AddProjectPinsTag(t *testing.T) {
	root := setupFleetHome(t)
	repo := makeRepo(t, "dir")
	derived := projects.TagForPath(repo)
	pinned := "custom-pin"
	if derived == pinned {
		t.Fatalf("test setup: derived tag equals pinned tag %q", pinned)
	}

	out := &bytes.Buffer{}
	if err := runProjectAdd(repo, pinned, out, out); err != nil {
		t.Fatalf("add --project: %v\n%s", err, out)
	}

	// meta.json under the PINNED tag.
	if _, err := os.Stat(filepath.Join(root, "projects", pinned, "meta.json")); err != nil {
		t.Errorf("meta.json not under pinned tag %q: %v", pinned, err)
	}
	// NOT under the derived tag.
	if _, err := os.Stat(filepath.Join(root, "projects", derived, "meta.json")); err == nil {
		t.Errorf("meta.json wrongly registered under derived tag %q", derived)
	}
	m, err := projects.Read(pinned)
	if err != nil {
		t.Fatalf("read pinned meta: %v", err)
	}
	if m.RepoPath != repo {
		t.Errorf("pinned repo_path=%q want %q", m.RepoPath, repo)
	}
}

// ── C6 — add without --project unchanged ────────────────────────────────
// `fleet project add <path>` → tag derived via TagForPath; behavior
// identical to today.
func TestResolveRepo_C6_AddNoProjectUnchanged(t *testing.T) {
	root := setupFleetHome(t)
	repo := makeRepo(t, "dir")
	derived := projects.TagForPath(repo)

	out := &bytes.Buffer{}
	if err := runProjectAdd(repo, "", out, out); err != nil {
		t.Fatalf("add: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(root, "projects", derived, "meta.json")); err != nil {
		t.Errorf("meta.json not under derived tag %q: %v", derived, err)
	}
	m, err := projects.Read(derived)
	if err != nil {
		t.Fatalf("read derived meta: %v", err)
	}
	if m.RepoPath != repo {
		t.Errorf("repo_path=%q want %q", m.RepoPath, repo)
	}
}

// ── C7 — add stamps fingerprint (git) ───────────────────────────────────
// `fleet project add <git-repo>` → meta.json has repo_fingerprint =
// <root-sha>|<sha256(origin)>.
func TestResolveRepo_C7_AddStampsFingerprint(t *testing.T) {
	setupFleetHome(t)
	parent := t.TempDir()
	repo := newGitRepo(t, parent, "fp-repo", "https://github.com/edisonshen/fp-repo.git")

	out := &bytes.Buffer{}
	if err := runProjectAdd(repo, "fpproj", out, out); err != nil {
		t.Fatalf("add: %v\n%s", err, out)
	}
	m, err := projects.Read("fpproj")
	if err != nil {
		t.Fatalf("read meta: %v", err)
	}
	if m.RepoFingerprint == "" {
		t.Fatalf("repo_fingerprint not stamped for git project")
	}
	// Shape: <root-commit-sha>|<origin-hash>. Both parts non-empty.
	parts := strings.SplitN(m.RepoFingerprint, "|", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		t.Errorf("fingerprint %q not in <root-sha>|<origin-hash> form", m.RepoFingerprint)
	}
	// The origin-hash component must be a SHA-256 hex (64 chars), NOT the
	// raw URL.
	if len(parts[1]) != 64 {
		t.Errorf("origin component %q is not a sha256 hex (len %d)", parts[1], len(parts[1]))
	}
	if strings.Contains(m.RepoFingerprint, "github.com") {
		t.Errorf("fingerprint leaks raw origin URL: %q", m.RepoFingerprint)
	}
}

// ── C8 — add shallow no stamp ───────────────────────────────────────────
// `fleet project add <shallow-repo>` → registers; repo_fingerprint empty;
// warns `git fetch --unshallow`.
func TestResolveRepo_C8_AddShallowNoStamp(t *testing.T) {
	setupFleetHome(t)
	parent := t.TempDir()
	repo := newShallow(t, parent, "shallow")

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	if err := runProjectAdd(repo, "shproj", stdout, stderr); err != nil {
		t.Fatalf("add shallow must succeed, got: %v\n%s", err, stderr)
	}
	m, err := projects.Read("shproj")
	if err != nil {
		t.Fatalf("read meta: %v", err)
	}
	if m.RepoFingerprint != "" {
		t.Errorf("shallow clone should NOT be stamped; got %q", m.RepoFingerprint)
	}
	if !strings.Contains(stderr.String(), "unshallow") {
		t.Errorf("expected `git fetch --unshallow` warning, got:\n%s", stderr.String())
	}
}

// ── C9 — add --project invalid name ─────────────────────────────────────
// `fleet project add --project "bad/name" <path>` → rejected via
// state.ValidateProjectName; non-zero exit (returns error).
func TestResolveRepo_C9_AddProjectInvalidName(t *testing.T) {
	setupFleetHome(t)
	repo := makeRepo(t, "dir")

	out := &bytes.Buffer{}
	err := runProjectAdd(repo, "bad/name", out, out)
	if err == nil {
		t.Fatalf("expected error for invalid --project name; out=%s", out)
	}
	if !strings.Contains(err.Error(), "invalid") {
		t.Errorf("error should mention invalid, got: %v", err)
	}
	// Sanity: validate the same name through the gate to confirm the test
	// premise (the name really is invalid).
	if state.ValidateProjectName("bad/name") == nil {
		t.Errorf("test premise broken: ValidateProjectName accepted 'bad/name'")
	}
}

// ── C10 — non-git add then resolve ──────────────────────────────────────
// `fleet project add <plain-dir>` (no .git) → registers is_git=false;
// `resolve-repo --project <p>` binds the dir without a git-identity check.
func TestResolveRepo_C10_NonGitAddThenResolve(t *testing.T) {
	setupFleetHome(t)
	dir := evalSym(t, t.TempDir()) // no .git

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	if err := runProjectAdd(dir, "plainproj", stdout, stderr); err != nil {
		t.Fatalf("non-git add: %v\n%s", err, stderr)
	}
	m, err := projects.Read("plainproj")
	if err != nil {
		t.Fatalf("read meta: %v", err)
	}
	if m.GitMode() {
		t.Errorf("non-git project registered as git mode")
	}
	if m.RepoFingerprint != "" {
		t.Errorf("non-git project should never be fingerprinted; got %q", m.RepoFingerprint)
	}

	rStdout, rStderr, failed := resolveRepo(t, "plainproj", false)
	if failed {
		t.Fatalf("resolve-repo non-git failed; stderr=%s", rStderr)
	}
	if got := strings.TrimSpace(rStdout); got != dir {
		t.Errorf("resolved path=%q want non-git dir %q", got, dir)
	}
}

// metaPathFor returns the on-disk meta.json path for a project tag.
func metaPathFor(t *testing.T, project string) string {
	t.Helper()
	dir, err := state.ProjectDir(project)
	if err != nil {
		t.Fatalf("ProjectDir: %v", err)
	}
	return filepath.Join(filepath.Clean(dir), "meta.json")
}
