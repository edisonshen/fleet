package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makeRepo creates dir/<name>/.git/. Returns the repo path.
func makeRepo(t *testing.T, parent, name string) string {
	t.Helper()
	repo := filepath.Join(parent, name)
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", repo, err)
	}
	return repo
}

// withCwd cds into dir for the duration of the test, restoring on
// cleanup. Required because discoverRepos calls os.Getwd().
func withCwd(t *testing.T, dir string) {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir %s: %v", dir, err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
}

func TestDiscoverRepos_CwdIsFirst(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_PROJECT_DIRS", filepath.Join(tmp, "nope"))

	withCwd(t, tmp)

	got := discoverRepos()
	if len(got) != 1 {
		t.Fatalf("expected 1 candidate (cwd), got %d: %+v", len(got), got)
	}
	if !strings.HasPrefix(got[0].Display, "(cwd)") {
		t.Errorf("first candidate should be marked (cwd), got %q", got[0].Display)
	}
}

func TestDiscoverRepos_ProjectDirsScansGitRepos(t *testing.T) {
	tmp := t.TempDir()
	projects := filepath.Join(tmp, "projects")
	if err := os.MkdirAll(projects, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	makeRepo(t, projects, "alpha")
	makeRepo(t, projects, "beta")
	// Non-repo directory: should be skipped.
	if err := os.MkdirAll(filepath.Join(projects, "gamma"), 0o755); err != nil {
		t.Fatalf("mkdir gamma: %v", err)
	}
	// File at top level: should be ignored.
	if err := os.WriteFile(filepath.Join(projects, "stray.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write stray: %v", err)
	}

	t.Setenv("FLEET_PROJECT_DIRS", projects)
	withCwd(t, t.TempDir()) // cwd is unrelated, doesn't pollute results

	got := discoverRepos()
	displays := make([]string, 0, len(got))
	for _, c := range got {
		displays = append(displays, c.Display)
	}
	mustContain(t, displays, "alpha")
	mustContain(t, displays, "beta")
	mustNotContain(t, displays, "gamma")
}

func TestDiscoverRepos_DedupsCwdAgainstProjectScan(t *testing.T) {
	tmp := t.TempDir()
	projects := filepath.Join(tmp, "projects")
	if err := os.MkdirAll(projects, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	repo := makeRepo(t, projects, "fleet")

	t.Setenv("FLEET_PROJECT_DIRS", projects)
	withCwd(t, repo)

	got := discoverRepos()
	count := 0
	for _, c := range got {
		if strings.Contains(c.Display, "fleet") {
			count++
		}
	}
	if count != 1 {
		t.Errorf("fleet repo should appear exactly once, got %d candidates: %+v", count, got)
	}
	// And the cwd row must win: it carries the (cwd) marker.
	if !strings.HasPrefix(got[0].Display, "(cwd)") {
		t.Errorf("cwd should be first with (cwd) marker, got %q", got[0].Display)
	}
}

func TestDiscoverRepos_MultipleProjectDirsHonored(t *testing.T) {
	tmp := t.TempDir()
	a := filepath.Join(tmp, "a")
	b := filepath.Join(tmp, "b")
	if err := os.MkdirAll(a, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.MkdirAll(b, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	makeRepo(t, a, "one")
	makeRepo(t, b, "two")

	t.Setenv("FLEET_PROJECT_DIRS", a+string(os.PathListSeparator)+b)
	withCwd(t, tmp) // tmp is not a git repo, but cwd is always added

	got := discoverRepos()
	displays := make([]string, 0, len(got))
	for _, c := range got {
		displays = append(displays, c.Display)
	}
	mustContain(t, displays, "one")
	mustContain(t, displays, "two")
}

func TestFilterCandidates_EmptyFilterReturnsAll(t *testing.T) {
	c := []repoCandidate{{Display: "alpha"}, {Display: "beta"}}
	idx := filterCandidates(c, "")
	if len(idx) != 2 || idx[0] != 0 || idx[1] != 1 {
		t.Errorf("empty filter should return [0,1], got %v", idx)
	}
}

func TestFilterCandidates_CaseInsensitiveSubstring(t *testing.T) {
	c := []repoCandidate{
		{Display: "Alpha"},
		{Display: "BETA"},
		{Display: "charlie"},
	}
	idx := filterCandidates(c, "AL")
	if len(idx) != 1 || idx[0] != 0 {
		t.Errorf("case-insensitive 'AL' should match only Alpha (index 0), got %v", idx)
	}
	idx = filterCandidates(c, "e")
	if len(idx) != 2 || idx[0] != 1 || idx[1] != 2 {
		t.Errorf("'e' should match BETA + charlie, got %v", idx)
	}
}

func TestProjectDirs_DefaultsToHomeProjects(t *testing.T) {
	t.Setenv("FLEET_PROJECT_DIRS", "")
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	dirs := projectDirs()
	want := filepath.Join(tmp, "projects")
	if len(dirs) != 1 || dirs[0] != want {
		t.Errorf("default projectDirs should be [%q], got %v", want, dirs)
	}
}

// TestDiscoverRepos_DisambiguatesDuplicateBasenames is the codex
// P2 regression: two repos with the same basename in different
// project roots must render as distinct picker rows. Without
// disambiguation the operator sees two identical "fleet" labels and
// can't tell which checkout will be selected.
func TestDiscoverRepos_DisambiguatesDuplicateBasenames(t *testing.T) {
	tmp := t.TempDir()
	work := filepath.Join(tmp, "work")
	personal := filepath.Join(tmp, "personal")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.MkdirAll(personal, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	makeRepo(t, work, "fleet")
	makeRepo(t, personal, "fleet")

	t.Setenv("FLEET_PROJECT_DIRS", work+string(os.PathListSeparator)+personal)
	withCwd(t, t.TempDir())

	got := discoverRepos()
	displays := []string{}
	for _, c := range got {
		displays = append(displays, c.Display)
	}
	mustContain(t, displays, "work/fleet")
	mustContain(t, displays, "personal/fleet")
	mustNotContain(t, displays, "fleet") // no bare label survives
}

func TestProjectTag_ParentBasename(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"/Users/x/work/fleet", "work-fleet"},
		{"/Users/x/personal/fleet", "personal-fleet"},
		{"/tmp/projects/foo", "projects-foo"},
		{"/foo", "foo"}, // single segment falls back to basename
		{"/", "fleet"},  // sanitization → empty → fallback
	}
	for _, tc := range cases {
		got := ProjectTag(tc.path)
		if got != tc.want {
			t.Errorf("ProjectTag(%q)=%q, want %q", tc.path, got, tc.want)
		}
	}
}

// TestProjectTag_SanitizesUnsafeChars regresses codex iter-2 P1: paths
// with spaces or other punctuation used to produce tags that
// state.ValidateProjectName rejects, and dispatch failed before any
// agent spawned. Sanitization maps unsafe chars to "-" and ensures
// every output is a valid project name.
func TestProjectTag_SanitizesUnsafeChars(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"/Users/x/Client Work/api", "Client-Work-api"},
		{"/foo bar baz", "foo-bar-baz"},
		{"/proj/v1.0", "proj-v1.0"},             // dots preserved
		{"/proj/with;semis", "proj-with-semis"}, // semis → dash
		{"/  /  ", "fleet"},                     // all whitespace → fallback
		{"/foo/bar baz qux", "foo-bar-baz-qux"}, // parent "foo" + sanitized basename
	}
	for _, tc := range cases {
		got := ProjectTag(tc.path)
		if got != tc.want {
			t.Errorf("ProjectTag(%q)=%q, want %q", tc.path, got, tc.want)
		}
	}
}

// TestDisambiguateDisplays_FallsBackToFullPath regresses codex iter-2
// P3: two repos at .../src/fleet from different mounts both produce
// "src/fleet" after the 1-parent prefix. The second pass falls back
// to the full Path so the operator can still tell them apart.
func TestDisambiguateDisplays_FallsBackToFullPath(t *testing.T) {
	c := []repoCandidate{
		{Path: "/Users/x/src/fleet", Display: "fleet"},
		{Path: "/Volumes/ssd/src/fleet", Display: "fleet"},
	}
	disambiguateDisplays(c)
	// After 1-parent: both become "src/fleet" — still colliding.
	// After 2nd pass: replaced with full Path, distinct.
	if c[0].Display == c[1].Display {
		t.Fatalf("expected distinct displays after disambiguation, got %q twice", c[0].Display)
	}
	if c[0].Display != "/Users/x/src/fleet" || c[1].Display != "/Volumes/ssd/src/fleet" {
		t.Errorf("expected full-path fallback, got %q and %q", c[0].Display, c[1].Display)
	}
}

func TestProjectDirs_DropsEmptyEntries(t *testing.T) {
	t.Setenv("FLEET_PROJECT_DIRS",
		"/a"+string(os.PathListSeparator)+
			""+string(os.PathListSeparator)+
			"/b")
	dirs := projectDirs()
	if len(dirs) != 2 || dirs[0] != "/a" || dirs[1] != "/b" {
		t.Errorf("expected [/a /b], got %v", dirs)
	}
}

// -- helpers ----------------------------------------------------------

func mustContain(t *testing.T, haystack []string, needle string) {
	t.Helper()
	for _, s := range haystack {
		if s == needle {
			return
		}
	}
	t.Errorf("expected %q in %v", needle, haystack)
}

func mustNotContain(t *testing.T, haystack []string, needle string) {
	t.Helper()
	for _, s := range haystack {
		if s == needle {
			t.Errorf("did not expect %q in %v", needle, haystack)
			return
		}
	}
}
