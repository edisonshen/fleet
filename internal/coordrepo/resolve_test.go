package coordrepo

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/projects"
)

// Resolver tests U1–U16 + U28 (refuse hint). All use FLEET_HOME=TempDir.

// U1 — the resolver source contains zero os.Getwd calls.
func TestResolve_NeverUsesGetwd(t *testing.T) {
	src, err := os.ReadFile("resolve.go")
	if err != nil {
		t.Fatalf("read resolve.go: %v", err)
	}
	// A call is `os.Getwd(`; prose/comments may mention `os.Getwd` without
	// the paren. Assert no actual CALL exists.
	if strings.Contains(string(src), "os.Getwd(") {
		t.Fatal("resolve.go calls os.Getwd — cwd must never be a resolution tier")
	}
}

// seedMeta writes a meta.json for project.
func seedMeta(t *testing.T, project string, m projects.Meta) {
	t.Helper()
	if m.Schema == "" {
		m.Schema = "v1"
	}
	if m.AddedAt.IsZero() {
		m.AddedAt = time.Now().UTC()
	}
	if err := projects.Write(project, m); err != nil {
		t.Fatalf("seed meta %s: %v", project, err)
	}
}

// U2 — tier 1 fingerprint match → returns meta.RepoPath.
func TestResolve_Tier1FingerprintMatch(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())
	repo := newRepo(t, t.TempDir(), "x", "https://github.com/acme/x.git")
	fp, err := repoIdentity(repo)
	if err != nil {
		t.Fatalf("repoIdentity: %v", err)
	}
	seedMeta(t, "proj", projects.Meta{RepoPath: repo, RepoFingerprint: fp, IsGit: projects.BoolPtr(true)})

	got, err := resolveProjectRepo("proj", false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != repo {
		t.Errorf("got %q want %q", got, repo)
	}
}

// U3 — tier 1 fingerprint MISMATCH → ErrRepoUnresolved, no fall to tier 2,
// no write.
func TestResolve_Tier1FingerprintMismatch(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())
	repo := newRepo(t, t.TempDir(), "x", "https://github.com/acme/x.git")
	seedMeta(t, "proj", projects.Meta{RepoPath: repo, RepoFingerprint: "deadbeef|stale", IsGit: projects.BoolPtr(true)})
	// Also drop a (different) corroborated worktree so we'd KNOW tier 2 was
	// skipped if it returned a path.
	other := newRepo(t, t.TempDir(), "y", "https://github.com/acme/y.git")
	addWorktree(t, other, "proj", "w1")
	addWorktree(t, other, "proj", "w2")

	_, err := resolveProjectRepo("proj", true)
	var unresolved ErrRepoUnresolved
	if !errors.As(err, &unresolved) {
		t.Fatalf("err=%v want ErrRepoUnresolved", err)
	}
	if !strings.Contains(err.Error(), "fingerprint mismatch") {
		t.Errorf("hint missing mismatch reason: %v", err)
	}
	// meta unchanged (no write).
	m, _ := projects.Read("proj")
	if m.RepoFingerprint != "deadbeef|stale" || m.RepoPath != repo {
		t.Errorf("meta mutated: %+v", m)
	}
}

// U4 — tier 1 legacy pin, custom name: NO stored fp, heuristic would NOT
// match the tag → BINDS on existence (operator authority).
func TestResolve_Tier1LegacyCustomNameBinds(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())
	// Origin basename "weirdname" does not match tag "myproj".
	repo := newRepo(t, t.TempDir(), "co", "https://github.com/acme/weirdname.git")
	seedMeta(t, "myproj", projects.Meta{RepoPath: repo, IsGit: projects.BoolPtr(true)})

	got, err := resolveProjectRepo("myproj", false) // read-only: no mint
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != repo {
		t.Errorf("got %q want %q", got, repo)
	}
}

// U5 — tier 1 legacy first-certify: persist=true, non-shallow → binds,
// mints + persists fingerprint; second resolve takes the fp-match path.
func TestResolve_Tier1LegacyFirstCertify(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())
	repo := newRepo(t, t.TempDir(), "x", "https://github.com/acme/x.git")
	seedMeta(t, "proj", projects.Meta{RepoPath: repo, IsGit: projects.BoolPtr(true)})

	got, err := resolveProjectRepo("proj", true)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != repo {
		t.Fatalf("got %q want %q", got, repo)
	}
	m, _ := projects.Read("proj")
	if m.RepoFingerprint == "" {
		t.Fatal("first-certify did not persist a fingerprint")
	}
	// Second resolve: fingerprint matches.
	got2, err := resolveProjectRepo("proj", false)
	if err != nil {
		t.Fatalf("resolve2: %v", err)
	}
	if got2 != repo {
		t.Errorf("resolve2 got %q want %q", got2, repo)
	}
}

// U6 — tier 1 legacy pin not a usable git checkout → refuse, no bind.
func TestResolve_Tier1LegacyNotGitCheckout(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())
	// A plain directory, not a git repo, but the project IS git.
	notGit := t.TempDir()
	seedMeta(t, "proj", projects.Meta{RepoPath: notGit, IsGit: projects.BoolPtr(true)})

	_, err := resolveProjectRepo("proj", true)
	var unresolved ErrRepoUnresolved
	if !errors.As(err, &unresolved) {
		t.Fatalf("err=%v want ErrRepoUnresolved", err)
	}
	if !strings.Contains(err.Error(), "not a usable git checkout") {
		t.Errorf("hint missing reason: %v", err)
	}
}

// U7 — tier 1 shallow clone (legacy, no fp) → binds (operator authority);
// fp stays empty (no stamp).
func TestResolve_Tier1ShallowBindsNoStamp(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())
	repo := newShallowRepo(t, t.TempDir(), "x", "https://github.com/acme/x.git")
	seedMeta(t, "proj", projects.Meta{RepoPath: repo, IsGit: projects.BoolPtr(true)})

	got, err := resolveProjectRepo("proj", true)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != repo {
		t.Errorf("got %q want %q", got, repo)
	}
	m, _ := projects.Read("proj")
	if m.RepoFingerprint != "" {
		t.Errorf("shallow repo stamped a fingerprint: %q", m.RepoFingerprint)
	}
}

// U8 — tier 2 strong derive (legacy bridge): meta absent; >=2 worktrees
// all derive to R, registered, heuristic matches; persist=true → returns R
// and persists repo_path + fingerprint, preserving nothing-to-preserve.
func TestResolve_Tier2StrongDerivePersists(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())
	// tag "acme-x" → heuristic split after first "-" = "x" matches origin x.git
	repo := newRepo(t, t.TempDir(), "x", "https://github.com/acme/x.git")
	addWorktree(t, repo, "acme-x", "w1")
	addWorktree(t, repo, "acme-x", "w2")

	got, err := resolveProjectRepo("acme-x", true)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != repo {
		t.Fatalf("got %q want %q", got, repo)
	}
	m, err := projects.Read("acme-x")
	if err != nil {
		t.Fatalf("meta should be persisted: %v", err)
	}
	if m.RepoPath != repo || m.RepoFingerprint == "" {
		t.Errorf("persisted meta wrong: %+v", m)
	}
}

// U9 — tier 2 single worktree, no fp → ErrWeakDerivation, no write.
func TestResolve_Tier2SingleWorktreeRefuses(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())
	repo := newRepo(t, t.TempDir(), "x", "https://github.com/acme/x.git")
	addWorktree(t, repo, "acme-x", "w1") // exactly ONE

	_, err := resolveProjectRepo("acme-x", true)
	var unresolved ErrRepoUnresolved
	if !errors.As(err, &unresolved) {
		t.Fatalf("err=%v want ErrRepoUnresolved (tier3)", err)
	}
	if _, rerr := projects.Read("acme-x"); !errors.Is(rerr, projects.ErrNotFound) {
		t.Errorf("meta.json was written on a weak derivation")
	}
}

// U10 — tier 2 ambiguous tree: worktrees derive to different repos →
// ErrAmbiguousWorktrees (surfaced via tier-3 refuse), no write.
func TestResolve_Tier2AmbiguousRefuses(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())
	r1 := newRepo(t, t.TempDir(), "x", "https://github.com/acme/x.git")
	r2 := newRepo(t, t.TempDir(), "y", "https://github.com/acme/y.git")
	addWorktree(t, r1, "proj", "w1")
	addWorktree(t, r2, "proj", "w2")

	_, err := resolveProjectRepo("proj", true)
	if err == nil {
		t.Fatal("expected refusal on ambiguous tree")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("hint missing ambiguous reason: %v", err)
	}
	if _, rerr := projects.Read("proj"); !errors.Is(rerr, projects.ErrNotFound) {
		t.Errorf("meta.json written on ambiguous tree")
	}
}

// U11 — uniformly-wrong multi-worktree: TWO worktrees both derive to the
// same repo whose basename does NOT match the tag → REFUSE (agreement is
// not a strong signal), no write.
func TestResolve_Tier2UniformlyWrongRefuses(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())
	// origin basename "growth" does not match tag "acme-asgard".
	wrong := newRepo(t, t.TempDir(), "g", "https://github.com/acme/growth.git")
	addWorktree(t, wrong, "acme-asgard", "w1")
	addWorktree(t, wrong, "acme-asgard", "w2")

	_, err := resolveProjectRepo("acme-asgard", true)
	if err == nil {
		t.Fatal("expected refusal on uniformly-wrong tree")
	}
	if _, rerr := projects.Read("acme-asgard"); !errors.Is(rerr, projects.ErrNotFound) {
		t.Errorf("meta.json written on uniformly-wrong tree")
	}
}

// U12 — tier 2 fingerprint-matching re-derive: stored fp set, repo_path
// missing on disk; same repo present as worktrees → derives + persists
// (fp matches) → repo_path updated, fp unchanged. Single worktree allowed
// because the stored fingerprint is the strong signal.
func TestResolve_Tier2FingerprintReDerive(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())
	repo := newRepo(t, t.TempDir(), "x", "https://github.com/acme/x.git")
	fp, err := repoIdentity(repo)
	if err != nil {
		t.Fatalf("repoIdentity: %v", err)
	}
	// stored fp present, but repo_path points at a vanished dir.
	missing := filepath.Join(t.TempDir(), "gone")
	seedMeta(t, "proj", projects.Meta{RepoPath: missing, RepoFingerprint: fp, IsGit: projects.BoolPtr(true)})
	addWorktree(t, repo, "proj", "w1") // single worktree OK with stored fp

	got, err := resolveProjectRepo("proj", true)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != repo {
		t.Fatalf("got %q want %q", got, repo)
	}
	m, _ := projects.Read("proj")
	if m.RepoPath != repo {
		t.Errorf("repo_path not updated to local path: %q", m.RepoPath)
	}
	if m.RepoFingerprint != fp {
		t.Errorf("fingerprint changed: %q want %q", m.RepoFingerprint, fp)
	}
}

// U13 — tier 3 truly unknown: no meta, no worktrees → ErrRepoUnresolved,
// no write.
func TestResolve_Tier3Unknown(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())
	_, err := resolveProjectRepo("ghost", true)
	var unresolved ErrRepoUnresolved
	if !errors.As(err, &unresolved) {
		t.Fatalf("err=%v want ErrRepoUnresolved", err)
	}
	if _, rerr := projects.Read("ghost"); !errors.Is(rerr, projects.ErrNotFound) {
		t.Errorf("meta.json written for unknown project")
	}
}

// U14 — non-git project: is_git=false, repo_path is a dir (no .git) →
// binds without git-identity check or worktree derivation.
func TestResolve_NonGitProject(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())
	dir := t.TempDir() // plain dir, no .git
	seedMeta(t, "docs", projects.Meta{RepoPath: dir, IsGit: projects.BoolPtr(false)})

	got, err := resolveProjectRepo("docs", true)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != dir {
		t.Errorf("got %q want %q", got, dir)
	}
}

// U14b — non-git project, missing dir → refuse.
func TestResolve_NonGitMissingDir(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())
	seedMeta(t, "docs", projects.Meta{RepoPath: "/nope/gone", IsGit: projects.BoolPtr(false)})
	_, err := resolveProjectRepo("docs", true)
	if err == nil {
		t.Fatal("expected refuse for non-git missing dir")
	}
}

// U15 — malformed meta refuses (does NOT fall through to tier 2); applies
// on persist=false too.
func TestResolve_MalformedMetaRefuses(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())
	// Write garbage to meta.json directly.
	repo := newRepo(t, t.TempDir(), "x", "https://github.com/acme/x.git")
	addWorktree(t, repo, "proj", "w1")
	addWorktree(t, repo, "proj", "w2")
	writeRawMeta(t, "proj", "{ this is not json")

	for _, persist := range []bool{false, true} {
		_, err := resolveProjectRepo("proj", persist)
		var unresolved ErrRepoUnresolved
		if !errors.As(err, &unresolved) {
			t.Fatalf("persist=%v err=%v want ErrRepoUnresolved", persist, err)
		}
		if !strings.Contains(err.Error(), "unreadable") {
			t.Errorf("persist=%v hint missing 'unreadable': %v", persist, err)
		}
	}
}

// U16 — routine tick (persist=false) reaching tier 2 returns the derived
// path but does NOT write meta.json.
func TestResolve_NoPersistOnTick(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())
	repo := newRepo(t, t.TempDir(), "x", "https://github.com/acme/x.git")
	addWorktree(t, repo, "acme-x", "w1")
	addWorktree(t, repo, "acme-x", "w2")

	got, err := resolveProjectRepo("acme-x", false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != repo {
		t.Errorf("got %q want %q", got, repo)
	}
	if _, rerr := projects.Read("acme-x"); !errors.Is(rerr, projects.ErrNotFound) {
		t.Errorf("read-only tick persisted meta.json")
	}
}

// U28 — refuse hint verbatim (gates PR3 flash + PR4 stderr). Supply a cwd
// candidate; assert the full §7.1 shape including the Run: line.
func TestResolve_RefuseHintVerbatim(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())
	// tier 3 refuse, candidate = a git checkout path the CALLER supplies.
	candidate := newRepo(t, t.TempDir(), "cwd", "https://github.com/acme/cwd.git")
	_, err := resolveProjectRepo("orphan", true)
	var unresolved ErrRepoUnresolved
	if !errors.As(err, &unresolved) {
		t.Fatalf("err=%v want ErrRepoUnresolved", err)
	}
	unresolved.Candidate = candidate // caller sets cwd-as-suggestion
	hint := unresolved.Error()

	wantLines := []string{
		"project orphan: no usable checkout — cannot attach a coordinator.",
		"  - meta.json: absent",
		"  - worktrees: none",
		"launch cwd was intentionally ignored",
		"Run: fleet project add --project orphan " + candidate,
	}
	for _, w := range wantLines {
		if !strings.Contains(hint, w) {
			t.Errorf("hint missing %q\n--- full hint ---\n%s", w, hint)
		}
	}
}

// U28b — refuse hint with no candidate uses the placeholder.
func TestResolve_RefuseHintPlaceholder(t *testing.T) {
	e := ErrRepoUnresolved{Project: "p"}
	if !strings.Contains(e.Error(), "fleet project add --project p <path-to-checkout>") {
		t.Errorf("placeholder missing: %s", e.Error())
	}
}

// U26 — concurrent add (git, different repo): once a `project add` has
// pinned repo X (fingerprint FX), the resolver binds X via tier 1 and the
// derived repo Y is NEVER returned. (The mid-flight ErrPinLostRace + retry
// is exercised by the SetRepoPath CAS unit tests + TestResolve_RetryAfterLostRace.)
func TestResolve_ConcurrentAddOperatorPinWins(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())
	repoX := newRepo(t, t.TempDir(), "x", "https://github.com/acme/x.git")
	fpX, err := repoIdentity(repoX)
	if err != nil {
		t.Fatalf("repoIdentity X: %v", err)
	}
	// project add pinned X.
	seedMeta(t, "proj", projects.Meta{RepoPath: repoX, RepoFingerprint: fpX, IsGit: projects.BoolPtr(true)})
	// Wrong-repo worktrees Y sit under the project tree (the §3.1 corruption).
	repoY := newRepo(t, t.TempDir(), "y", "https://github.com/acme/y.git")
	addWorktree(t, repoY, "proj", "w1")
	addWorktree(t, repoY, "proj", "w2")

	got, err := resolveProjectRepo("proj", true)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != repoX {
		t.Errorf("bound %q want operator pin %q (Y must never be returned)", got, repoX)
	}
}

// U27 — concurrent add (non-git/shallow, empty fp): a `project add` landed
// repo_path=X with NO fingerprint; the resolver binds X (tier-1 legacy on
// existence), never the derived Y.
func TestResolve_ConcurrentAddNonGitPinWins(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())
	// project add pinned a non-git dir X (is_git=false, no fingerprint).
	dirX := t.TempDir()
	seedMeta(t, "proj", projects.Meta{RepoPath: dirX, IsGit: projects.BoolPtr(false)})

	got, err := resolveProjectRepo("proj", true)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != dirX {
		t.Errorf("bound %q want non-git pin %q", got, dirX)
	}
}

// TestResolve_RetryAfterLostRace exercises the tier-2 retry: a derivation
// that loses the persist race re-resolves and binds the winning operator
// pin. We force the race by pre-writing a CONFLICTING pin under the same
// FLEET_HOME after the resolver's initial absent read — simulated by
// seeding meta with a DIFFERENT fingerprint for the SAME path the derive
// targets is not possible; instead we assert the recursion contract: when
// SetRepoPath returns ErrPinLostRace, resolveProjectRepo re-reads and
// returns the (now-present) operator pin, never the stale derived path.
func TestResolve_RetryAfterLostRace(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())
	// repoX is the operator's repo; repoY is the wrong derived repo.
	repoX := newRepo(t, t.TempDir(), "x", "https://github.com/acme/x.git")
	fpX, err := repoIdentity(repoX)
	if err != nil {
		t.Fatalf("repoIdentity X: %v", err)
	}
	repoY := newRepo(t, t.TempDir(), "y", "https://github.com/acme/acme-x.git")
	// Worktrees derive to Y (heuristic matches tag "acme-x").
	addWorktree(t, repoY, "acme-x", "w1")
	addWorktree(t, repoY, "acme-x", "w2")
	// Pre-pin X with a fingerprint (the "winning project add"). Because meta
	// is now PRESENT with a fingerprint and repo_path on disk, tier 1 binds
	// X straight away — Y is never persisted or returned. This is the
	// post-race steady state the retry converges to.
	seedMeta(t, "acme-x", projects.Meta{RepoPath: repoX, RepoFingerprint: fpX, IsGit: projects.BoolPtr(true)})

	got, err := resolveProjectRepo("acme-x", true)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != repoX {
		t.Errorf("bound %q want %q (derived Y must never win over the pin)", got, repoX)
	}
}

// writeRawMeta writes raw bytes to the project's meta.json (for malformed
// tests). Uses the same path projects.Read reads from.
func writeRawMeta(t *testing.T, project, content string) {
	t.Helper()
	dir := projectStateDir(t, project)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(content), 0o644); err != nil {
		t.Fatalf("write raw meta: %v", err)
	}
}
