package coordrepo

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// U17 — origin URL with credentials is NOT leaked into the fingerprint;
// the origin component is a SHA-256 hex, not the raw token/userinfo.
func TestFingerprint_OriginNotLeaked(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())
	parent := t.TempDir()
	repo := newRepo(t, parent, "x", "https://user:s3cr3t-token@github.com/edisonshen/x.git")

	fp, err := repoIdentity(repo)
	if err != nil {
		t.Fatalf("repoIdentity: %v", err)
	}
	if strings.Contains(fp, "s3cr3t-token") || strings.Contains(fp, "user:") {
		t.Fatalf("fingerprint leaked credentials: %q", fp)
	}
	parts := strings.SplitN(fp, "|", 2)
	if len(parts) != 2 {
		t.Fatalf("fingerprint not <root>|<origin>: %q", fp)
	}
	// origin component is sha256 hex (64 chars).
	if len(parts[1]) != 64 {
		t.Errorf("origin component len=%d want 64 (sha256 hex): %q", len(parts[1]), parts[1])
	}
}

// U18 — origin path case is PRESERVED (not lowercased): two origins
// differing only by path case produce DIFFERENT fingerprints.
func TestFingerprint_OriginPathCasePreserved(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())
	parent := t.TempDir()
	rFoo := newRepo(t, parent, "foo", "https://github.com/Acme/Foo.git")
	rfoo := newRepo(t, parent, "lc", "https://github.com/Acme/foo.git")

	fFoo, err := repoIdentity(rFoo)
	if err != nil {
		t.Fatalf("repoIdentity Foo: %v", err)
	}
	ffoo, err := repoIdentity(rfoo)
	if err != nil {
		t.Fatalf("repoIdentity foo: %v", err)
	}
	// Both repos have different histories (different root commits) too, so
	// also assert the ORIGIN components specifically differ.
	oFoo := strings.SplitN(fFoo, "|", 2)[1]
	ofoo := strings.SplitN(ffoo, "|", 2)[1]
	if oFoo == ofoo {
		t.Errorf("origin hash collapsed case: %q == %q", oFoo, ofoo)
	}
}

// U18b — host case is NORMALIZED (lowercased): same path, host case differs
// → same origin component.
func TestFingerprint_HostCaseNormalized(t *testing.T) {
	a := normalizeOrigin("https://GitHub.com/acme/foo.git")
	b := normalizeOrigin("https://github.com/acme/foo.git")
	if a != b {
		t.Errorf("host case not normalized: %q != %q", a, b)
	}
	// SCP form too.
	c := normalizeOrigin("git@GitHub.com:acme/foo.git")
	d := normalizeOrigin("git@github.com:acme/foo.git")
	if c != d {
		t.Errorf("scp host case not normalized: %q != %q", c, d)
	}
}

// U18c — userinfo + query/fragment stripped; scp normalized to host/path.
func TestNormalizeOrigin_StripsCredentialsAndQuery(t *testing.T) {
	cases := map[string]string{
		"https://user:tok@github.com/Acme/Foo.git?x=1#frag": "https://github.com/Acme/Foo",
		"git@github.com:Acme/Foo.git":                       "github.com/Acme/Foo",
		"ssh://git@github.com/Acme/Foo.git":                 "ssh://github.com/Acme/Foo",
	}
	for in, want := range cases {
		if got := normalizeOrigin(in); got != want {
			t.Errorf("normalizeOrigin(%q)=%q want %q", in, got, want)
		}
	}
}

// U19 — two local-only repos forked from the SAME history (same root
// commit, no origin) get DISTINCT fleet.repoId → distinct fingerprints.
func TestFingerprint_NoOriginForksDistinct(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())
	parent := t.TempDir()
	src := newRepo(t, parent, "src", "") // no origin

	// Clone twice (same root commit, no origin on either clone).
	cloneA := filepath.Join(parent, "a")
	cloneB := filepath.Join(parent, "b")
	for _, dst := range []string{cloneA, cloneB} {
		mustClone(t, src, dst)
		git(t, dst, "remote", "remove", "origin")
	}

	fpA, err := mintFingerprint(cloneA)
	if err != nil {
		t.Fatalf("mint A: %v", err)
	}
	fpB, err := mintFingerprint(cloneB)
	if err != nil {
		t.Fatalf("mint B: %v", err)
	}
	if fpA == fpB {
		t.Errorf("same-history forks collided: %q", fpA)
	}
	// Same root commit, so the difference is purely the fleet.repoId.
	rootA := strings.SplitN(fpA, "|", 2)[0]
	rootB := strings.SplitN(fpB, "|", 2)[0]
	if rootA != rootB {
		t.Fatalf("clones unexpectedly differ in root commit (%s vs %s)", rootA, rootB)
	}
}

// U20 — repoIdentity (read-only) on a no-origin repo lacking fleet.repoId
// does NOT modify .git/config; the remote component is the empty string.
func TestFingerprint_ReadOnlyNeverMints(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())
	parent := t.TempDir()
	repo := newRepo(t, parent, "x", "") // no origin

	if readUUID(t, repo) != "" {
		t.Fatal("precondition: fleet.repoId should be unset")
	}
	fp, err := repoIdentity(repo)
	if err != nil {
		t.Fatalf("repoIdentity: %v", err)
	}
	if readUUID(t, repo) != "" {
		t.Fatal("repoIdentity minted fleet.repoId (should be read-only)")
	}
	// remote component empty → fingerprint ends with "|"
	if !strings.HasSuffix(fp, "|") {
		t.Errorf("expected empty remote component, got %q", fp)
	}
}

// U21 — mintFingerprint on a no-origin repo lacking fleet.repoId CREATES
// it and returns a stable fingerprint thereafter.
func TestFingerprint_MintCreatesOnPersist(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())
	parent := t.TempDir()
	repo := newRepo(t, parent, "x", "") // no origin

	fp1, err := mintFingerprint(repo)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	id := readUUID(t, repo)
	if id == "" {
		t.Fatal("mintFingerprint did not create fleet.repoId")
	}
	// Stable on the next call (read-only sees the minted id).
	fp2, err := repoIdentity(repo)
	if err != nil {
		t.Fatalf("repoIdentity: %v", err)
	}
	if fp1 != fp2 {
		t.Errorf("fingerprint unstable after mint: %q != %q", fp1, fp2)
	}
	if !strings.HasSuffix(fp1, "|"+id) {
		t.Errorf("fingerprint %q does not end with minted id %q", fp1, id)
	}
}

// U20b (codex iter-2 [P2]) — a fleet.repoId in GLOBAL git config must NOT
// be read as a per-clone identity: distinct no-origin clones must still get
// distinct identities. repoIdentity reads --local only.
func TestFingerprint_GlobalRepoIdIgnored(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())
	parent := t.TempDir()

	// A global gitconfig that pins a fleet.repoId for ALL repos.
	globalCfg := filepath.Join(parent, "globalconfig")
	if err := writeFile(globalCfg, "[fleet]\n\trepoId = GLOBAL-SHARED-ID\n"); err != nil {
		t.Fatalf("write global config: %v", err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", globalCfg)
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	repo := newRepoEnv(t, parent, "x", "") // no origin; uses ambient env (incl GIT_CONFIG_GLOBAL)

	// read-only: local has no fleet.repoId, global does → must be ignored → "".
	fp, err := repoIdentity(repo)
	if err != nil {
		t.Fatalf("repoIdentity: %v", err)
	}
	if strings.Contains(fp, "GLOBAL-SHARED-ID") {
		t.Fatalf("read fleet.repoId from global config: %q", fp)
	}
	if !strings.HasSuffix(fp, "|") {
		t.Errorf("expected empty remote component (global ignored), got %q", fp)
	}

	// mint: creates a LOCAL id distinct from the global one.
	mfp, err := mintFingerprint(repo)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if strings.Contains(mfp, "GLOBAL-SHARED-ID") {
		t.Errorf("minted fingerprint used global id: %q", mfp)
	}
}

// U22 — shallow clone → both functions return errShallow.
func TestFingerprint_ShallowSentinel(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())
	parent := t.TempDir()
	repo := newShallowRepo(t, parent, "shallow", "https://github.com/acme/shallow.git")

	if _, err := repoIdentity(repo); err != errShallow {
		t.Errorf("repoIdentity err=%v want errShallow", err)
	}
	if _, err := mintFingerprint(repo); err != errShallow {
		t.Errorf("mintFingerprint err=%v want errShallow", err)
	}
}

func mustClone(t *testing.T, src, dst string) {
	t.Helper()
	cmd := exec.Command("git", "clone", "-q", src, dst)
	cmd.Env = gitTestEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone %s->%s: %v\n%s", src, dst, err, out)
	}
}
