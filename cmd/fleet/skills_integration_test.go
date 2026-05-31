package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	fleet "github.com/edisonshen/fleet"
	"github.com/edisonshen/fleet/internal/install"
)

// TestSkillsDeployGap_EndToEnd exercises the full deploy-gap scenario that
// motivated this work (#182): a merged skill fix that never reached the
// running coord because the install was a frozen copy.
//
// The integrated path under test:
//  1. A repo checkout holds the FIXED skill.
//  2. `fleet skills link` symlinks ~/.claude/skills/coordinator at it.
//  3. A merged fix lands in the checkout AFTER linking.
//  4. The running coord (via WarnIfStale at coord-run startup) reads the
//     fix LIVE — no stale warning, because a symlink is always current.
//
// Then the negative control: a frozen COPY that diverges from the fixed
// repo is detected as STALE and warned about (surface-dont-silo).
//
// This is the architect-level test feedback_e2e_tests_for_all_cases asks
// for: a unit test on Status alone would pass while the real install path
// (init/autoinit clobbering, coord-run not warning) silently regressed.
func TestSkillsDeployGap_EndToEnd(t *testing.T) {
	tmp := t.TempDir()
	claudeHome := filepath.Join(tmp, ".claude")
	fleetHome := filepath.Join(tmp, ".fleet")
	t.Setenv("FLEET_HOME", fleetHome)

	// (1) repo checkout with the skill at "v1".
	repo := t.TempDir()
	coordSrc := filepath.Join(repo, "skills", "coordinator")
	guardSrc := filepath.Join(repo, "skills", "fleet-guard")
	for _, d := range []string{coordSrc, guardSrc} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	skillMD := filepath.Join(coordSrc, "SKILL.md")
	if err := os.WriteFile(skillMD, []byte("coord v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(guardSrc, "SKILL.md"), []byte("guard"), 0o644); err != nil {
		t.Fatal(err)
	}
	// register the repo as a project so auto-detect finds it.
	projDir := filepath.Join(fleetHome, "projects", "projects-fleet")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta, _ := json.Marshal(map[string]any{"schema": "v1", "repo_path": repo})
	if err := os.WriteFile(filepath.Join(projDir, "meta.json"), meta, 0o644); err != nil {
		t.Fatal(err)
	}

	// (2) link.
	if err := runSkillsLink(&bytes.Buffer{}, "", false, claudeHome); err != nil {
		t.Fatalf("runSkillsLink: %v", err)
	}

	// (3) merged fix lands in the checkout AFTER linking.
	if err := os.WriteFile(skillMD, []byte("coord v2 FIXED"), 0o644); err != nil {
		t.Fatal(err)
	}

	// (4) the coord reads the fix live through the symlink.
	live, err := os.ReadFile(filepath.Join(install.SkillDir(claudeHome, "coordinator"), "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(live) != "coord v2 FIXED" {
		t.Errorf("symlinked skill did not reflect merged fix: got %q", live)
	}

	// coord-run startup warning must stay SILENT for a symlink (it is live).
	var warnBuf bytes.Buffer
	if n := install.WarnIfStale(&warnBuf, claudeHome, fleet.SkillFS()); n != 0 {
		t.Errorf("symlinked install warned stale (%d): %s", n, warnBuf.String())
	}

	// autoinit must NOT clobber the live symlink even though the repo's
	// bytes differ from the binary's embedded bytes.
	maybeAutoInit(&bytes.Buffer{}, claudeHome)
	if !install.IsSymlink(claudeHome, "coordinator") {
		t.Fatal("maybeAutoInit clobbered the live coordinator symlink")
	}
	// and the link still resolves to the FIXED content.
	live2, _ := os.ReadFile(filepath.Join(install.SkillDir(claudeHome, "coordinator"), "SKILL.md"))
	if string(live2) != "coord v2 FIXED" {
		t.Errorf("after autoinit symlink content changed: %q", live2)
	}
}

// TestSkillsDeployGap_StaleCopyWarns is the negative control: a frozen COPY
// that diverges from the repo IS detected and warned about at coord-run
// startup, with a remediation hint (surface-dont-silo).
func TestSkillsDeployGap_StaleCopyWarns(t *testing.T) {
	tmp := t.TempDir()
	claudeHome := filepath.Join(tmp, ".claude")
	fleetHome := filepath.Join(tmp, ".fleet")
	t.Setenv("FLEET_HOME", fleetHome)

	repo := t.TempDir()
	coordSrc := filepath.Join(repo, "skills", "coordinator")
	if err := os.MkdirAll(coordSrc, 0o755); err != nil {
		t.Fatal(err)
	}
	// repo has the FIXED skill.
	if err := os.WriteFile(filepath.Join(coordSrc, "SKILL.md"), []byte("FIXED"), 0o644); err != nil {
		t.Fatal(err)
	}
	projDir := filepath.Join(fleetHome, "projects", "projects-fleet")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta, _ := json.Marshal(map[string]any{"schema": "v1", "repo_path": repo})
	if err := os.WriteFile(filepath.Join(projDir, "meta.json"), meta, 0o644); err != nil {
		t.Fatal(err)
	}

	// install a frozen STALE copy (the bug: hand-copied snapshot).
	coordDst := install.SkillDir(claudeHome, "coordinator")
	if err := os.MkdirAll(coordDst, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(coordDst, "SKILL.md"), []byte("OLD BUGGY"), 0o644); err != nil {
		t.Fatal(err)
	}

	var warnBuf bytes.Buffer
	n := install.WarnIfStale(&warnBuf, claudeHome, fleet.SkillFS())
	if n == 0 {
		t.Fatalf("stale copy not warned about; output:\n%s", warnBuf.String())
	}
	out := warnBuf.String()
	if !bytes.Contains(warnBuf.Bytes(), []byte("STALE COPY")) {
		t.Errorf("warning missing STALE COPY marker:\n%s", out)
	}
	if !bytes.Contains(warnBuf.Bytes(), []byte("fleet skills link")) {
		t.Errorf("warning missing remediation hint:\n%s", out)
	}
}
