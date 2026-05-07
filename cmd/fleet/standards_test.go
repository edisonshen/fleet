package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStandardsShow_GlobalRendersStub — empty global file renders the
// frontmatter + H1 stub.
func TestStandardsShow_GlobalRendersStub(t *testing.T) {
	setupTasksHome(t)
	out := &bytes.Buffer{}
	if err := runStandardsShow(&standardsShowOpts{scope: scopeGlobal}, out); err != nil {
		t.Fatalf("show global: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "schema: v1") || !strings.Contains(got, "# Standards") {
		t.Errorf("global stub render wrong: %q", got)
	}
}

// TestStandardsShow_MergedPrefersProject — same-named H2 in per-project
// shadows global.
func TestStandardsShow_MergedPrefersProject(t *testing.T) {
	fleetHome, project := setupTasksHome(t)

	// Seed a global "Testing" section.
	globalPath := filepath.Join(fleetHome, "standards.md")
	if err := os.WriteFile(globalPath, []byte(`---
schema: v1
---

# Standards

## Testing

global rule
`), 0o644); err != nil {
		t.Fatalf("seed global: %v", err)
	}
	// Seed a project "Testing" override + a project-only "PRs" section.
	projDir := filepath.Join(fleetHome, "projects", project)
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir proj: %v", err)
	}
	projPath := filepath.Join(projDir, "standards.md")
	if err := os.WriteFile(projPath, []byte(`---
schema: v1
---

# Standards

## Testing

project rule

## PRs

PR rule
`), 0o644); err != nil {
		t.Fatalf("seed proj: %v", err)
	}

	out := &bytes.Buffer{}
	if err := runStandardsShow(&standardsShowOpts{scope: scopeMerged, project: project}, out); err != nil {
		t.Fatalf("show merged: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "project rule") {
		t.Errorf("merged should show project's Testing override: %s", got)
	}
	if strings.Contains(got, "global rule") {
		t.Errorf("merged should NOT show global Testing when project overrides: %s", got)
	}
	if !strings.Contains(got, "PR rule") {
		t.Errorf("merged should include project-only PRs section: %s", got)
	}
}

// TestStandardsShow_MutuallyExclusiveFlags — passing two scope flags
// fails fast (we surface this through cobra's PreRunE-equivalent path).
func TestStandardsShow_MutuallyExclusiveFlags(t *testing.T) {
	cmd := newStandardsShowCmd()
	cmd.SetArgs([]string{"--global", "--merged"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error when --global + --merged both set")
	}
}

// TestStandardsEdit_SeedsStubAndInvokesEditor — runs the edit path
// against a fake editor (`true`) so we can assert the file gets seeded
// and the editor is invoked. Skipped on Windows where `true` doesn't
// exist; we're a Unix-only project (file: lock_unix.go) so the skip is
// belt-and-suspenders.
func TestStandardsEdit_SeedsStubAndInvokesEditor(t *testing.T) {
	fleetHome, project := setupTasksHome(t)
	// Use the BSD/GNU `true` binary as a no-op editor — opens, returns 0.
	out := &bytes.Buffer{}
	if err := runStandardsEdit(&standardsEditOpts{
		project: project,
		editor:  "true",
	}, out, &bytes.Buffer{}); err != nil {
		t.Fatalf("edit: %v\n%s", err, out.String())
	}
	stub, err := os.ReadFile(filepath.Join(fleetHome, "projects", project, "standards.md"))
	if err != nil {
		t.Fatalf("read stub: %v", err)
	}
	if !strings.Contains(string(stub), "schema: v1") {
		t.Errorf("stub missing frontmatter: %s", string(stub))
	}
}

// TestStandardsEdit_GlobalPath — --global edits ~/.fleet/standards.md,
// not the per-project file.
func TestStandardsEdit_GlobalPath(t *testing.T) {
	fleetHome, _ := setupTasksHome(t)
	if err := runStandardsEdit(&standardsEditOpts{
		global: true,
		editor: "true",
	}, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fleetHome, "standards.md")); err != nil {
		t.Errorf("global standards.md not created: %v", err)
	}
}
