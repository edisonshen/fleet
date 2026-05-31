package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/edisonshen/fleet/internal/install"
)

// writeFakeRepo lays down a minimal repo checkout with a coordinator and
// fleet-guard skill dir (each with a SKILL.md so LocateRepoSkillDir's stat
// gate passes), and registers it as a project under FLEET_HOME so
// install.LocateRepoSkillDir finds it.
func writeFakeRepo(t *testing.T, fleetHome string) string {
	t.Helper()
	repo := t.TempDir()
	for _, name := range []string{"coordinator", "fleet-guard"} {
		dir := filepath.Join(repo, "skills", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("# "+name+" repo"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	projDir := filepath.Join(fleetHome, "projects", "projects-fleet")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := map[string]any{"schema": "v1", "repo_path": repo}
	data, _ := json.Marshal(meta)
	if err := os.WriteFile(filepath.Join(projDir, "meta.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return repo
}

func TestRunSkillsLink_AutoDetectAndStatus(t *testing.T) {
	tmp := t.TempDir()
	claudeHome := filepath.Join(tmp, ".claude")
	fleetHome := filepath.Join(tmp, ".fleet")
	t.Setenv("FLEET_HOME", fleetHome)
	repo := writeFakeRepo(t, fleetHome)

	var buf bytes.Buffer
	if err := runSkillsLink(&buf, "", false, claudeHome); err != nil {
		t.Fatalf("runSkillsLink: %v", err)
	}
	// coordinator must now be a live symlink at the repo checkout.
	if !install.IsSymlink(claudeHome, "coordinator") {
		t.Fatal("coordinator not symlinked after link")
	}
	target, _ := os.Readlink(install.SkillDir(claudeHome, "coordinator"))
	wantAbs, _ := filepath.Abs(filepath.Join(repo, "skills", "coordinator"))
	if target != wantAbs {
		t.Errorf("symlink target=%q want %q", target, wantAbs)
	}

	// status reports symlink/live and exits clean (no stale).
	var sbuf bytes.Buffer
	if err := runSkillsStatus(&sbuf, claudeHome); err != nil {
		t.Fatalf("runSkillsStatus after link returned error: %v\n%s", err, sbuf.String())
	}
	if !bytes.Contains(sbuf.Bytes(), []byte("symlink")) {
		t.Errorf("status missing 'symlink': %s", sbuf.String())
	}
}

func TestRunSkillsLink_FromFlag(t *testing.T) {
	tmp := t.TempDir()
	claudeHome := filepath.Join(tmp, ".claude")
	t.Setenv("FLEET_HOME", filepath.Join(tmp, ".fleet"))
	// No registered project; supply --from explicitly.
	repo := t.TempDir()
	for _, name := range []string{"coordinator", "fleet-guard"} {
		dir := filepath.Join(repo, "skills", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := runSkillsLink(&bytes.Buffer{}, repo, false, claudeHome); err != nil {
		t.Fatalf("runSkillsLink --from: %v", err)
	}
	if !install.IsSymlink(claudeHome, "coordinator") {
		t.Fatal("coordinator not symlinked with --from")
	}
}

func TestRunSkillsLink_NoRepoFails(t *testing.T) {
	tmp := t.TempDir()
	claudeHome := filepath.Join(tmp, ".claude")
	t.Setenv("FLEET_HOME", filepath.Join(tmp, ".fleet"))
	// No projects registered, no --from → nothing to link → error.
	err := runSkillsLink(&bytes.Buffer{}, "", false, claudeHome)
	if err == nil {
		t.Fatal("expected error when no repo can be located")
	}
}

func TestRunSkillsStatus_StaleCopyExitsNonZero(t *testing.T) {
	tmp := t.TempDir()
	claudeHome := filepath.Join(tmp, ".claude")
	fleetHome := filepath.Join(tmp, ".fleet")
	t.Setenv("FLEET_HOME", fleetHome)
	repo := writeFakeRepo(t, fleetHome)

	// Install a STALE copy of coordinator: a SKILL.md whose bytes differ
	// from the repo's (repo says "# coordinator repo").
	coordDst := install.SkillDir(claudeHome, "coordinator")
	if err := os.MkdirAll(coordDst, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(coordDst, "SKILL.md"), []byte("# OLD BUGGY"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = repo

	var buf bytes.Buffer
	err := runSkillsStatus(&buf, claudeHome)
	if err == nil {
		t.Fatalf("stale copy should make status return error; output:\n%s", buf.String())
	}
	if !bytes.Contains(buf.Bytes(), []byte("STALE")) {
		t.Errorf("status output missing STALE marker:\n%s", buf.String())
	}
}

func TestRunSkillsSync_LeavesSymlinkIntact(t *testing.T) {
	tmp := t.TempDir()
	claudeHome := filepath.Join(tmp, ".claude")
	fleetHome := filepath.Join(tmp, ".fleet")
	t.Setenv("FLEET_HOME", fleetHome)
	writeFakeRepo(t, fleetHome)

	if err := runSkillsLink(&bytes.Buffer{}, "", false, claudeHome); err != nil {
		t.Fatal(err)
	}
	// sync (no force) must NOT clobber a live symlink.
	if err := runSkillsSync(&bytes.Buffer{}, false, claudeHome); err != nil {
		t.Fatalf("runSkillsSync: %v", err)
	}
	if !install.IsSymlink(claudeHome, "coordinator") {
		t.Fatal("sync without force clobbered the symlink")
	}
}

func TestRunSkillsSync_CopiesEmbedded(t *testing.T) {
	tmp := t.TempDir()
	claudeHome := filepath.Join(tmp, ".claude")
	t.Setenv("FLEET_HOME", filepath.Join(tmp, ".fleet"))
	if err := runSkillsSync(&bytes.Buffer{}, false, claudeHome); err != nil {
		t.Fatalf("runSkillsSync: %v", err)
	}
	// Embedded coordinator SKILL.md must now be on disk as a copy.
	if install.IsSymlink(claudeHome, "coordinator") {
		t.Fatal("sync produced a symlink, expected a copy")
	}
	if _, err := os.Stat(filepath.Join(install.SkillDir(claudeHome, "coordinator"), "SKILL.md")); err != nil {
		t.Fatalf("coordinator SKILL.md not copied: %v", err)
	}
}
