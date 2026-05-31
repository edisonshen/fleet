package install

import (
	"bytes"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
)

// fakeFS builds an in-memory embedded-style FS for the copy path.
func fakeFS(files map[string]string) fstest.MapFS {
	m := fstest.MapFS{}
	for name, body := range files {
		m[name] = &fstest.MapFile{Data: []byte(body)}
	}
	return m
}

// oneSkillFS wraps a single skill's FS into the map[string]fs.FS shape
// Status expects. The FS content is irrelevant to Status (it only uses the
// map keys for iteration + the on-disk classify), so most callers pass an
// empty FS.
func oneSkillFS(name string, fsys fs.FS) map[string]fs.FS {
	return map[string]fs.FS{name: fsys}
}

func TestLink_CreatesSymlink(t *testing.T) {
	claudeHome := t.TempDir()
	repo := t.TempDir()
	repoSkill := filepath.Join(repo, "skills", "coordinator")
	if err := os.MkdirAll(repoSkill, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoSkill, "SKILL.md"), []byte("# coord"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Link(io.Discard, claudeHome, "coordinator", repoSkill, false); err != nil {
		t.Fatalf("Link: %v", err)
	}
	dst := SkillDir(claudeHome, "coordinator")
	got, err := os.Readlink(dst)
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	wantAbs, _ := filepath.Abs(repoSkill)
	if got != wantAbs {
		t.Errorf("symlink target=%q want %q", got, wantAbs)
	}
	// Reading through the symlink yields the repo's live file — this is
	// the load-bearing property: a repo edit goes live.
	body, err := os.ReadFile(filepath.Join(dst, "SKILL.md"))
	if err != nil || string(body) != "# coord" {
		t.Errorf("through-symlink read=%q err=%v", body, err)
	}
}

func TestLink_LiveEditGoesThrough(t *testing.T) {
	claudeHome := t.TempDir()
	repo := t.TempDir()
	repoSkill := filepath.Join(repo, "skills", "coordinator")
	if err := os.MkdirAll(repoSkill, 0o755); err != nil {
		t.Fatal(err)
	}
	skillMD := filepath.Join(repoSkill, "SKILL.md")
	if err := os.WriteFile(skillMD, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Link(io.Discard, claudeHome, "coordinator", repoSkill, false); err != nil {
		t.Fatal(err)
	}
	// Simulate a merged fix landing in the checkout.
	if err := os.WriteFile(skillMD, []byte("v2-fixed"), 0o644); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(SkillDir(claudeHome, "coordinator"), "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "v2-fixed" {
		t.Errorf("symlink did not reflect repo edit: got %q want v2-fixed", body)
	}
}

// TestLink_ReplacesExistingCopyWithoutForce is the headline UX path: a
// developer who ran `fleet init` first has a COPY on disk; `fleet skills
// link` (no flags) must convert it to a live symlink, not error out. A copy
// is regenerable embedded bytes, so replacing it needs no --force.
func TestLink_ReplacesExistingCopyWithoutForce(t *testing.T) {
	claudeHome := t.TempDir()
	repo := t.TempDir()
	repoSkill := filepath.Join(repo, "skills", "coordinator")
	if err := os.MkdirAll(repoSkill, 0o755); err != nil {
		t.Fatal(err)
	}
	// pre-existing copy with a stale file in it (proves the dir is removed).
	dst := SkillDir(claudeHome, "coordinator")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "SKILL.md"), []byte("stale copy"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Link(io.Discard, claudeHome, "coordinator", repoSkill, false); err != nil {
		t.Fatalf("Link over a copy without --force should succeed, got %v", err)
	}
	shape, _, _ := classify(dst)
	if shape != ShapeSymlink {
		t.Errorf("after link shape=%v want symlink (copy not converted)", shape)
	}
}

// TestLink_RefusesExistingSymlinkWithoutForce: an existing symlink to a
// DIFFERENT checkout is deliberate operator state — never re-point it
// silently. --force opts into the re-point.
func TestLink_RefusesExistingSymlinkWithoutForce(t *testing.T) {
	claudeHome := t.TempDir()
	repoA := t.TempDir()
	repoB := t.TempDir()
	skillA := filepath.Join(repoA, "skills", "coordinator")
	skillB := filepath.Join(repoB, "skills", "coordinator")
	for _, d := range []string{skillA, skillB} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Link to checkout A first.
	if err := Link(io.Discard, claudeHome, "coordinator", skillA, false); err != nil {
		t.Fatal(err)
	}
	// Re-pointing to B without --force must error (don't clobber the link).
	if err := Link(io.Discard, claudeHome, "coordinator", skillB, false); err == nil {
		t.Fatal("expected error re-pointing an existing symlink without --force")
	}
	// The original link to A must be intact (not removed by the failed call).
	gotTarget, _ := os.Readlink(SkillDir(claudeHome, "coordinator"))
	wantA, _ := filepath.Abs(skillA)
	if gotTarget != wantA {
		t.Errorf("symlink re-pointed despite error: target=%q want %q", gotTarget, wantA)
	}
	// With --force it re-points to B.
	if err := Link(io.Discard, claudeHome, "coordinator", skillB, true); err != nil {
		t.Fatalf("Link --force re-point: %v", err)
	}
	gotTarget, _ = os.Readlink(SkillDir(claudeHome, "coordinator"))
	wantB, _ := filepath.Abs(skillB)
	if gotTarget != wantB {
		t.Errorf("after force re-point target=%q want %q", gotTarget, wantB)
	}
}

func TestLink_IdempotentAlreadyLinked(t *testing.T) {
	claudeHome := t.TempDir()
	repo := t.TempDir()
	repoSkill := filepath.Join(repo, "skills", "coordinator")
	if err := os.MkdirAll(repoSkill, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Link(io.Discard, claudeHome, "coordinator", repoSkill, false); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Link(&buf, claudeHome, "coordinator", repoSkill, false); err != nil {
		t.Fatalf("second Link (no force) should be a no-op, got %v", err)
	}
	if !bytes.Contains(buf.Bytes(), []byte("already linked")) {
		t.Errorf("expected 'already linked' skip, got %q", buf.String())
	}
}

func TestCopyFromFS_WritesFiles(t *testing.T) {
	claudeHome := t.TempDir()
	fsys := fakeFS(map[string]string{
		"SKILL.md": "# coord",
		"loop.py":  "print('x')",
	})
	if err := CopyFromFS(io.Discard, claudeHome, "coordinator", fsys, false); err != nil {
		t.Fatalf("CopyFromFS: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(SkillDir(claudeHome, "coordinator"), "loop.py"))
	if err != nil || string(body) != "print('x')" {
		t.Errorf("loop.py=%q err=%v", body, err)
	}
}

func TestCopyFromFS_SkipsSymlinkWithoutForce(t *testing.T) {
	claudeHome := t.TempDir()
	repo := t.TempDir()
	repoSkill := filepath.Join(repo, "skills", "coordinator")
	if err := os.MkdirAll(repoSkill, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoSkill, "SKILL.md"), []byte("live"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Link(io.Discard, claudeHome, "coordinator", repoSkill, false); err != nil {
		t.Fatal(err)
	}
	// A plain copy (e.g. from `fleet init`) must NOT clobber the live link.
	fsys := fakeFS(map[string]string{"SKILL.md": "embedded"})
	if err := CopyFromFS(io.Discard, claudeHome, "coordinator", fsys, false); err != nil {
		t.Fatalf("CopyFromFS: %v", err)
	}
	if !IsSymlink(claudeHome, "coordinator") {
		t.Fatal("plain CopyFromFS clobbered the symlink")
	}
	body, _ := os.ReadFile(filepath.Join(SkillDir(claudeHome, "coordinator"), "SKILL.md"))
	if string(body) != "live" {
		t.Errorf("symlink content changed: %q", body)
	}
}

func TestCopyFromFS_ForceConvertsSymlinkToCopy(t *testing.T) {
	claudeHome := t.TempDir()
	repo := t.TempDir()
	repoSkill := filepath.Join(repo, "skills", "coordinator")
	if err := os.MkdirAll(repoSkill, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoSkill, "SKILL.md"), []byte("live"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Link(io.Discard, claudeHome, "coordinator", repoSkill, false); err != nil {
		t.Fatal(err)
	}
	fsys := fakeFS(map[string]string{"SKILL.md": "embedded"})
	if err := CopyFromFS(io.Discard, claudeHome, "coordinator", fsys, true); err != nil {
		t.Fatalf("CopyFromFS force: %v", err)
	}
	if IsSymlink(claudeHome, "coordinator") {
		t.Fatal("force copy left a symlink")
	}
	body, _ := os.ReadFile(filepath.Join(SkillDir(claudeHome, "coordinator"), "SKILL.md"))
	if string(body) != "embedded" {
		t.Errorf("force copy content=%q want embedded", body)
	}
}

func TestIsSymlink(t *testing.T) {
	claudeHome := t.TempDir()
	if IsSymlink(claudeHome, "coordinator") {
		t.Error("missing dir reported as symlink")
	}
	dst := SkillDir(claudeHome, "coordinator")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	if IsSymlink(claudeHome, "coordinator") {
		t.Error("copy dir reported as symlink")
	}
}

func TestStatus_FreshCopyNotStale(t *testing.T) {
	claudeHome := t.TempDir()
	repo := t.TempDir()
	repoSkill := filepath.Join(repo, "skills", "coordinator")
	if err := os.MkdirAll(repoSkill, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoSkill, "SKILL.md"), []byte("same"), 0o644); err != nil {
		t.Fatal(err)
	}
	// install a byte-identical copy
	fsys := fakeFS(map[string]string{"SKILL.md": "same"})
	if err := CopyFromFS(io.Discard, claudeHome, "coordinator", fsys, false); err != nil {
		t.Fatal(err)
	}
	locate := func(name string) (string, bool) { return repoSkill, true }
	statuses, err := Status(claudeHome, oneSkillFS("coordinator", fsys), locate)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 {
		t.Fatalf("want 1 status got %d", len(statuses))
	}
	if statuses[0].Shape != ShapeCopy {
		t.Errorf("shape=%v want copy", statuses[0].Shape)
	}
	if statuses[0].Stale {
		t.Errorf("fresh copy flagged stale: diverged=%v", statuses[0].DivergedFiles)
	}
}

func TestStatus_StaleCopyDetected(t *testing.T) {
	claudeHome := t.TempDir()
	repo := t.TempDir()
	repoSkill := filepath.Join(repo, "skills", "coordinator")
	if err := os.MkdirAll(repoSkill, 0o755); err != nil {
		t.Fatal(err)
	}
	// repo has the FIXED version
	if err := os.WriteFile(filepath.Join(repoSkill, "SKILL.md"), []byte("fixed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoSkill, "loop.py"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	// install holds the STALE snapshot (different SKILL.md, missing loop.py)
	dst := SkillDir(claudeHome, "coordinator")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "SKILL.md"), []byte("old-buggy"), 0o644); err != nil {
		t.Fatal(err)
	}
	// embedded set declares both files the coord runs.
	embed := fakeFS(map[string]string{"SKILL.md": "fixed", "loop.py": "new"})
	locate := func(name string) (string, bool) { return repoSkill, true }
	statuses, err := Status(claudeHome, oneSkillFS("coordinator", embed), locate)
	if err != nil {
		t.Fatal(err)
	}
	st := statuses[0]
	if !st.Stale {
		t.Fatal("stale copy not detected")
	}
	wantDiverged := map[string]bool{"SKILL.md": true, "loop.py": true}
	for _, f := range st.DivergedFiles {
		if !wantDiverged[f] {
			t.Errorf("unexpected diverged file %q", f)
		}
		delete(wantDiverged, f)
	}
	if len(wantDiverged) != 0 {
		t.Errorf("missing diverged files: %v", wantDiverged)
	}
}

// TestStatus_StrayRepoFileNotStale guards the false-positive: a file present
// in the repo checkout but NOT in the embedded set (a test file, __pycache__,
// or a new .py not yet wired into embed.go) must not flag an otherwise
// in-sync copy as stale.
func TestStatus_StrayRepoFileNotStale(t *testing.T) {
	claudeHome := t.TempDir()
	repo := t.TempDir()
	repoSkill := filepath.Join(repo, "skills", "coordinator")
	if err := os.MkdirAll(filepath.Join(repoSkill, "tests"), 0o755); err != nil {
		t.Fatal(err)
	}
	// embedded file: in sync between repo and install.
	if err := os.WriteFile(filepath.Join(repoSkill, "SKILL.md"), []byte("same"), 0o644); err != nil {
		t.Fatal(err)
	}
	// stray repo files NOT in the embedded set.
	if err := os.WriteFile(filepath.Join(repoSkill, "tests", "test_x.py"), []byte("t"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoSkill, "brand_new.py"), []byte("future"), 0o644); err != nil {
		t.Fatal(err)
	}
	embed := fakeFS(map[string]string{"SKILL.md": "same"})
	if err := CopyFromFS(io.Discard, claudeHome, "coordinator", embed, false); err != nil {
		t.Fatal(err)
	}
	locate := func(name string) (string, bool) { return repoSkill, true }
	statuses, err := Status(claudeHome, oneSkillFS("coordinator", embed), locate)
	if err != nil {
		t.Fatal(err)
	}
	if statuses[0].Stale {
		t.Errorf("stray repo files falsely flagged stale: %v", statuses[0].DivergedFiles)
	}
}

// TestLocateRepoSkillDirIn_SymlinkSiblingFallback covers the real-machine
// case: the fleet checkout has no meta.json registry entry, but ONE skill is
// already symlinked. A sibling skill must still resolve its repo source from
// that existing symlink (this is the bug live-testing surfaced: fleet-guard
// reporting "no repo to compare" while coordinator was a live symlink to the
// same checkout).
func TestLocateRepoSkillDirIn_SymlinkSiblingFallback(t *testing.T) {
	tmp := t.TempDir()
	claudeHome := filepath.Join(tmp, ".claude")
	// Empty registry: no meta.json anywhere.
	t.Setenv("FLEET_HOME", filepath.Join(tmp, ".fleet"))

	repo := t.TempDir()
	coordSrc := filepath.Join(repo, "skills", "coordinator")
	guardSrc := filepath.Join(repo, "skills", "fleet-guard")
	for _, d := range []string{coordSrc, guardSrc} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// coordinator is already symlinked; fleet-guard is not.
	if err := Link(io.Discard, claudeHome, "coordinator", coordSrc, false); err != nil {
		t.Fatal(err)
	}

	got, ok := LocateRepoSkillDirIn(claudeHome, "fleet-guard")
	if !ok {
		t.Fatal("fleet-guard not located via symlink sibling")
	}
	wantAbs, _ := filepath.Abs(guardSrc)
	if got != wantAbs {
		t.Errorf("located %q want %q", got, wantAbs)
	}
}

func TestStatus_SymlinkNeverStale(t *testing.T) {
	claudeHome := t.TempDir()
	repo := t.TempDir()
	repoSkill := filepath.Join(repo, "skills", "coordinator")
	if err := os.MkdirAll(repoSkill, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoSkill, "SKILL.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Link(io.Discard, claudeHome, "coordinator", repoSkill, false); err != nil {
		t.Fatal(err)
	}
	locate := func(name string) (string, bool) { return repoSkill, true }
	statuses, err := Status(claudeHome, oneSkillFS("coordinator", fakeFS(nil)), locate)
	if err != nil {
		t.Fatal(err)
	}
	if statuses[0].Shape != ShapeSymlink || statuses[0].Stale {
		t.Errorf("symlink: shape=%v stale=%v", statuses[0].Shape, statuses[0].Stale)
	}
}

func TestStatus_NoRepoLocatedCopyNotFlagged(t *testing.T) {
	claudeHome := t.TempDir()
	dst := SkillDir(claudeHome, "coordinator")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "SKILL.md"), []byte("whatever"), 0o644); err != nil {
		t.Fatal(err)
	}
	// no checkout locatable
	locate := func(name string) (string, bool) { return "", false }
	statuses, err := Status(claudeHome, oneSkillFS("coordinator", fakeFS(nil)), locate)
	if err != nil {
		t.Fatal(err)
	}
	if statuses[0].Stale {
		t.Error("copy with no locatable checkout should not be flagged stale")
	}
}
