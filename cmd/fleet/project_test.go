package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/projects"
)

// makeRepo creates a directory with a .git entry inside it.
// kind == "dir" makes a real .git/ directory; kind == "file" writes a
// .git file (worktrees use a `gitdir: ...` pointer file). Both must be
// recognized by `fleet project add` since the spec requires worktree
// support.
func makeRepo(t *testing.T, kind string) string {
	t.Helper()
	dir := t.TempDir()
	dotgit := filepath.Join(dir, ".git")
	switch kind {
	case "dir":
		if err := os.Mkdir(dotgit, 0o755); err != nil {
			t.Fatalf("mkdir .git: %v", err)
		}
	case "file":
		if err := os.WriteFile(dotgit, []byte("gitdir: /elsewhere\n"), 0o644); err != nil {
			t.Fatalf("write .git file: %v", err)
		}
	default:
		t.Fatalf("unknown kind %q", kind)
	}
	return dir
}

// makeRepoUnder creates a repo under parent named `name` so the parent
// basename of the resulting path is predictable for ProjectTag tests.
func makeRepoUnder(t *testing.T, parent, name string) string {
	t.Helper()
	dir := filepath.Join(parent, name)
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	return dir
}

func TestProjectAdd_HappyPath_WritesTasksAndMeta(t *testing.T) {
	root := setupFleetHome(t)
	repo := makeRepo(t, "dir")

	out := &bytes.Buffer{}
	if err := runProjectAdd(repo, out, out); err != nil {
		t.Fatalf("runProjectAdd: %v\n%s", err, out.String())
	}

	// Tag is parent-basename via internal/tui.ProjectTag — derive in-test.
	tag := projectTagForTest(t, repo)

	// tasks.md must exist with frontmatter.
	tasksPath := filepath.Join(root, "projects", tag, "tasks.md")
	tasksData, err := os.ReadFile(tasksPath)
	if err != nil {
		t.Fatalf("read tasks.md: %v", err)
	}
	if !strings.HasPrefix(string(tasksData), "---\nschema: v1\n---\n") {
		t.Errorf("tasks.md missing frontmatter, got:\n%s", tasksData)
	}

	// meta.json must exist with the required fields.
	metaPath := filepath.Join(root, "projects", tag, "meta.json")
	metaData, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("read meta.json: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(metaData, &raw); err != nil {
		t.Fatalf("unmarshal meta.json: %v", err)
	}
	if raw["schema"] != "v1" {
		t.Errorf("schema=%v, want v1", raw["schema"])
	}
	if raw["repo_path"] != repo {
		t.Errorf("repo_path=%v, want %s", raw["repo_path"], repo)
	}
	if _, ok := raw["added_at"]; !ok {
		t.Errorf("added_at missing; got: %v", raw)
	}

	// Stdout must mention the tag so the operator sees the registered name.
	if !strings.Contains(out.String(), tag) {
		t.Errorf("stdout missing tag %q:\n%s", tag, out.String())
	}
}

func TestProjectAdd_WorktreeGitFile_Accepted(t *testing.T) {
	setupFleetHome(t)
	repo := makeRepo(t, "file")

	out := &bytes.Buffer{}
	if err := runProjectAdd(repo, out, out); err != nil {
		t.Fatalf("runProjectAdd worktree: %v\n%s", err, out.String())
	}
}

func TestProjectAdd_MissingPath_Errors(t *testing.T) {
	setupFleetHome(t)
	out := &bytes.Buffer{}
	err := runProjectAdd("/no/such/path/here", out, out)
	if err == nil {
		t.Fatal("expected error on missing path, got nil")
	}
	if !strings.Contains(err.Error(), "no such directory") {
		t.Errorf("error should mention 'no such directory', got: %v", err)
	}
}

// TestProjectAdd_NonGitDirectory_AcceptedWithWarning replaces the
// previous "must be a git repo" guard. Operator clarification
// 2026-05-12: non-git directories register as IsGit=false with a
// stderr warning. The worker dispatch then skips push + PR.
func TestProjectAdd_NonGitDirectory_AcceptedWithWarning(t *testing.T) {
	root := setupFleetHome(t)
	dir := t.TempDir() // no .git inside

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	if err := runProjectAdd(dir, stdout, stderr); err != nil {
		t.Fatalf("non-git add must succeed, got: %v", err)
	}

	// stderr carries the warning. Body text is informational; check
	// for the noun-marker "non-git" so the operator sees what mode
	// they're in.
	if !strings.Contains(stderr.String(), "non-git") {
		t.Errorf("expected 'non-git' warning on stderr, got:\n%s", stderr.String())
	}

	// meta.json on disk records is_git=false. Use the raw map to
	// confirm the field is present (not just default-true).
	tag := projectTagForTest(t, dir)
	metaPath := filepath.Join(root, "projects", tag, "meta.json")
	metaData, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("read meta.json: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(metaData, &raw); err != nil {
		t.Fatalf("unmarshal meta.json: %v", err)
	}
	v, ok := raw["is_git"]
	if !ok {
		t.Fatalf("is_git missing on disk for non-git add; raw=%v", raw)
	}
	if b, _ := v.(bool); b {
		t.Errorf("is_git=%v on disk for non-git add, want false", v)
	}
	got, err := projects.Read(tag)
	if err != nil {
		t.Fatalf("projects.Read: %v", err)
	}
	if got.GitMode() {
		t.Errorf("GitMode() = true for non-git project; want false")
	}
}

// TestProjectAdd_GitDirectoryHasIsGitTrue: happy path stays unchanged —
// a directory with .git registers as IsGit=true (explicit pointer).
func TestProjectAdd_GitDirectoryHasIsGitTrue(t *testing.T) {
	root := setupFleetHome(t)
	repo := makeRepo(t, "dir")

	out := &bytes.Buffer{}
	if err := runProjectAdd(repo, out, out); err != nil {
		t.Fatalf("git add: %v", err)
	}

	tag := projectTagForTest(t, repo)
	metaPath := filepath.Join(root, "projects", tag, "meta.json")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("read meta.json: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal meta.json: %v", err)
	}
	v, ok := raw["is_git"]
	if !ok {
		t.Fatalf("is_git missing for git add; raw=%v", raw)
	}
	if b, _ := v.(bool); !b {
		t.Errorf("is_git=%v for git add, want true", v)
	}
	got, err := projects.Read(tag)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !got.GitMode() {
		t.Errorf("GitMode()=false for git project; want true")
	}
}

// TestProjectAdd_Idempotent_RefreshesAddedAt: re-adding the same path
// must not error and must update added_at on the meta.json.
func TestProjectAdd_Idempotent_RefreshesAddedAt(t *testing.T) {
	root := setupFleetHome(t)
	repo := makeRepo(t, "dir")

	out := &bytes.Buffer{}
	if err := runProjectAdd(repo, out, out); err != nil {
		t.Fatalf("first add: %v", err)
	}
	tag := projectTagForTest(t, repo)
	first, err := projects.Read(tag)
	if err != nil {
		t.Fatalf("first Read: %v", err)
	}

	// Second add — same path. Sleep is unnecessary (we compare on
	// strict After), but the test relies on time advancing between
	// the two writes. Use a no-op time gap by mutating the meta.json
	// stamp directly to the past, then re-add.
	pastMeta := first
	pastMeta.AddedAt = pastMeta.AddedAt.Add(-time.Hour)
	if err := projects.Write(tag, pastMeta); err != nil {
		t.Fatalf("rewrite past meta: %v", err)
	}

	out.Reset()
	if err := runProjectAdd(repo, out, out); err != nil {
		t.Fatalf("re-add: %v", err)
	}

	second, err := projects.Read(tag)
	if err != nil {
		t.Fatalf("second Read: %v", err)
	}
	if !second.AddedAt.After(pastMeta.AddedAt) {
		t.Errorf("re-add must refresh added_at; first=%v second=%v", pastMeta.AddedAt, second.AddedAt)
	}
	if second.RepoPath != repo {
		t.Errorf("repo_path mutated: %v want %v", second.RepoPath, repo)
	}
	// Sanity: verify root dir still contains both files.
	for _, name := range []string{"tasks.md", "meta.json"} {
		if _, err := os.Stat(filepath.Join(root, "projects", tag, name)); err != nil {
			t.Errorf("%s missing after re-add: %v", name, err)
		}
	}
}

// TestProjectAdd_TagCollisionDifferentPath_RefreshesRepoPath:
// when two paths sanitize to the same tag, re-add must update
// repo_path and warn on stderr — never error.
func TestProjectAdd_TagCollisionDifferentPath_RefreshesRepoPath(t *testing.T) {
	setupFleetHome(t)

	// Two repos under the same parent dir name produce identical tags
	// because ProjectTag uses parent-basename.
	parent := t.TempDir()
	repoA := makeRepoUnder(t, parent, "alpha")

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	if err := runProjectAdd(repoA, stdout, stderr); err != nil {
		t.Fatalf("first add: %v", err)
	}
	tag := projectTagForTest(t, repoA)

	// Build a second repo with the same parent name + basename combo
	// in a different ancestor — guarantees same tag, different path.
	parent2 := t.TempDir()
	parentNamed := filepath.Join(parent2, filepath.Base(filepath.Dir(repoA)))
	if err := os.Mkdir(parentNamed, 0o755); err != nil {
		t.Fatalf("mkdir parent2 named: %v", err)
	}
	repoB := makeRepoUnder(t, parentNamed, filepath.Base(repoA))
	if projectTagForTest(t, repoB) != tag {
		t.Fatalf("test setup error: tags differ %q vs %q", projectTagForTest(t, repoB), tag)
	}

	stdout.Reset()
	stderr.Reset()
	if err := runProjectAdd(repoB, stdout, stderr); err != nil {
		t.Fatalf("collision add: %v", err)
	}

	got, err := projects.Read(tag)
	if err != nil {
		t.Fatalf("Read after collision: %v", err)
	}
	if got.RepoPath != repoB {
		t.Errorf("repo_path=%q want %q (refreshed to second path)", got.RepoPath, repoB)
	}
	combined := stdout.String() + stderr.String()
	if !strings.Contains(strings.ToLower(combined), "warn") &&
		!strings.Contains(strings.ToLower(combined), "refresh") {
		t.Errorf("expected warning about repo_path change in output, got:\n%s", combined)
	}
}

// TestProjectAdd_PreservesExistingTasksMd regresses the lock contract:
// when tasks.md already exists with content (e.g. a `fleet tasks add`
// landed first), `fleet project add` must NOT clobber it. Pre-lock
// implementations could TOCTOU-race tasks.add and overwrite the file
// with empty frontmatter.
func TestProjectAdd_PreservesExistingTasksMd(t *testing.T) {
	root := setupFleetHome(t)
	repo := makeRepo(t, "dir")

	out := &bytes.Buffer{}
	if err := runProjectAdd(repo, out, out); err != nil {
		t.Fatalf("first add: %v", err)
	}

	tag := projectTagForTest(t, repo)
	tasksPath := filepath.Join(root, "projects", tag, "tasks.md")

	// Seed tasks.md with a fake operator-added task. project add must
	// not overwrite this on re-add.
	seed := "---\nschema: v1\n---\n\n## task: real-task-abcd\n\n- status: todo\n- priority: P2\n- created: 2026-05-09T20:00:00Z\n- updated: 2026-05-09T20:00:00Z\n"
	if err := os.WriteFile(tasksPath, []byte(seed), 0o644); err != nil {
		t.Fatalf("seed tasks.md: %v", err)
	}

	out.Reset()
	if err := runProjectAdd(repo, out, out); err != nil {
		t.Fatalf("re-add: %v", err)
	}

	got, err := os.ReadFile(tasksPath)
	if err != nil {
		t.Fatalf("read tasks.md: %v", err)
	}
	if !strings.Contains(string(got), "real-task-abcd") {
		t.Errorf("project add wiped seeded task; got:\n%s", got)
	}
}

// TestProjectAdd_RelativePath_IsResolvedToAbsolute exercises the spec
// "absolute or relative path" — when the operator passes a relative
// path, meta.json must store the absolute form (so the dashboard's
// future cwd consumer finds the right tree).
func TestProjectAdd_RelativePath_IsResolvedToAbsolute(t *testing.T) {
	setupFleetHome(t)
	repo := makeRepo(t, "dir")

	// Run from the repo's parent, pass the basename only.
	parent := filepath.Dir(repo)
	name := filepath.Base(repo)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if err := os.Chdir(parent); err != nil {
		t.Fatalf("Chdir parent: %v", err)
	}

	out := &bytes.Buffer{}
	if err := runProjectAdd(name, out, out); err != nil {
		t.Fatalf("runProjectAdd relative: %v", err)
	}
	tag := projectTagForTest(t, repo)
	got, err := projects.Read(tag)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !filepath.IsAbs(got.RepoPath) {
		t.Errorf("repo_path is not absolute: %q", got.RepoPath)
	}
}

// projectTagForTest delegates to projects.TagForPath — the canonical
// path → tag rule the CLI uses. Wrapping it in a helper keeps the
// test calls one-line and self-documenting.
func projectTagForTest(t *testing.T, p string) string {
	t.Helper()
	return projects.TagForPath(p)
}

// Compile-time check that the cobra command exists.
func TestProjectCmd_Wired(t *testing.T) {
	cmd := newProjectCmd()
	if cmd == nil {
		t.Fatal("newProjectCmd returned nil")
	}
	subs := cmd.Commands()
	found := false
	for _, c := range subs {
		if c.Use == "add <path>" || strings.HasPrefix(c.Use, "add ") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'add' subcommand; got commands: %v", subs)
	}
}

// TestProjectCmd_RegisteredOnRoot pins that `fleet project ...` is
// reachable from main's root command — operators expect to invoke
// the verb at the top level, not via a hidden helper.
func TestProjectCmd_RegisteredOnRoot(t *testing.T) {
	root := newRootCmd()
	for _, c := range root.Commands() {
		if c.Use == "project" {
			return
		}
	}
	var names []string
	for _, c := range root.Commands() {
		names = append(names, c.Use)
	}
	t.Errorf("`fleet project` not registered on root; got: %v", names)
}
