package gc

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/edisonshen/fleet/internal/state"
)

// U29 — ProjectWorktreeBases returns PER-worktree base-repo derivations,
// and projectRepoOnDisk (the existing gc consumer) still returns the first
// usable base (net behavior unchanged).

func gcGitEnv() []string {
	return append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	)
}

func gcGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = gcGitEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
}

func gcNewRepo(t *testing.T, name string) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	gcGit(t, repo, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "f"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	gcGit(t, repo, "add", ".")
	gcGit(t, repo, "commit", "-q", "-m", "init")
	r, _ := filepath.EvalSymlinks(repo)
	return r
}

func gcAddWorktree(t *testing.T, repo, project, name string) string {
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
	gcGit(t, repo, "worktree", "add", "-q", "-b", "worker/"+name, wt)
	r, _ := filepath.EvalSymlinks(wt)
	return r
}

func TestProjectWorktreeBases_PerWorktree(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())
	r1 := gcNewRepo(t, "r1")
	r2 := gcNewRepo(t, "r2")
	wtA := gcAddWorktree(t, r1, "proj", "a")
	wtB := gcAddWorktree(t, r2, "proj", "b") // different repo (mixed tree)

	bases, err := ProjectWorktreeBases("proj")
	if err != nil {
		t.Fatalf("ProjectWorktreeBases: %v", err)
	}
	if len(bases) != 2 {
		t.Fatalf("got %d bases want 2: %+v", len(bases), bases)
	}
	byWt := map[string]string{}
	for _, b := range bases {
		rb, _ := filepath.EvalSymlinks(b.Repo)
		rw, _ := filepath.EvalSymlinks(b.Worktree)
		byWt[rw] = rb
	}
	if byWt[wtA] != r1 {
		t.Errorf("wtA derived to %q want %q", byWt[wtA], r1)
	}
	if byWt[wtB] != r2 {
		t.Errorf("wtB derived to %q want %q", byWt[wtB], r2)
	}
}

func TestProjectRepoOnDisk_NetBehaviorUnchanged(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())
	r1 := gcNewRepo(t, "r1")
	gcAddWorktree(t, r1, "proj", "a")

	got, err := projectRepoOnDisk("proj")
	if err != nil {
		t.Fatalf("projectRepoOnDisk: %v", err)
	}
	g, _ := filepath.EvalSymlinks(got)
	if g != r1 {
		t.Errorf("projectRepoOnDisk got %q want %q", g, r1)
	}
}

func TestProjectWorktreeBases_NoWorktreesDir(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())
	bases, err := ProjectWorktreeBases("empty")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(bases) != 0 {
		t.Errorf("got %d bases want 0", len(bases))
	}
}
