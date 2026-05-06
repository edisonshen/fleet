package state

import (
	"strings"
	"testing"
)

// v0.2 introduces per-project state directories under
// ~/.fleet/projects/<safe-name>/. These tests pin the new helpers'
// path layout so callers (internal/tasks, internal/learnings,
// internal/standards, internal/workers) can rely on the shapes.

func TestProjectDir(t *testing.T) {
	t.Setenv("FLEET_HOME", "/tmp/fleet-test")
	cases := map[string]string{
		"fleet":       "/tmp/fleet-test/projects/fleet/",
		"gift-finder": "/tmp/fleet-test/projects/gift-finder/",
		// Safe-name sanitization mirrors SafeLockComponent: legacy
		// names with slashes / spaces / colons must still resolve to
		// a single-component directory, never escape projects/.
		"owner/repo":  "/tmp/fleet-test/projects/owner_repo/",
		"gift finder": "/tmp/fleet-test/projects/gift_finder/",
		"foo:bar":     "/tmp/fleet-test/projects/foo_bar/",
		"":            "/tmp/fleet-test/projects/_default/",
		"..":          "/tmp/fleet-test/projects/_../",
		"../escape":   "/tmp/fleet-test/projects/.._escape/",
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			got, err := ProjectDir(in)
			if err != nil {
				t.Fatalf("ProjectDir(%q): %v", in, err)
			}
			if got != want {
				t.Errorf("got %q want %q", got, want)
			}
		})
	}
}

func TestProjectStateLockPath(t *testing.T) {
	t.Setenv("FLEET_HOME", "/tmp/fleet-test")
	got, err := ProjectStateLockPath("fleet")
	if err != nil {
		t.Fatalf("ProjectStateLockPath: %v", err)
	}
	want := "/tmp/fleet-test/projects/fleet/.locks/state.lock"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestProjectStateLockPath_SanitizesLegacyNames(t *testing.T) {
	t.Setenv("FLEET_HOME", "/tmp/fleet-test")
	got, err := ProjectStateLockPath("owner/repo")
	if err != nil {
		t.Fatalf("ProjectStateLockPath: %v", err)
	}
	want := "/tmp/fleet-test/projects/owner_repo/.locks/state.lock"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestCoordinatorLockPath(t *testing.T) {
	t.Setenv("FLEET_HOME", "/tmp/fleet-test")
	got, err := CoordinatorLockPath("fleet")
	if err != nil {
		t.Fatalf("CoordinatorLockPath: %v", err)
	}
	want := "/tmp/fleet-test/projects/fleet/.locks/coordinator.lock"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestCoordinatorLockPath_SeparateFromState(t *testing.T) {
	// state.lock and coordinator.lock are sibling files under the
	// same .locks/ dir but must NOT collide — they serialize
	// different things (state writes vs the one-coord-per-project
	// invariant).
	t.Setenv("FLEET_HOME", "/tmp/fleet-test")
	state, err := ProjectStateLockPath("fleet")
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	coord, err := CoordinatorLockPath("fleet")
	if err != nil {
		t.Fatalf("coord: %v", err)
	}
	if state == coord {
		t.Errorf("state-lock and coord-lock must be distinct: %q", state)
	}
}

func TestWorkerDir(t *testing.T) {
	t.Setenv("FLEET_HOME", "/tmp/fleet-test")
	got, err := WorkerDir("fleet", "add-readme-7a3c")
	if err != nil {
		t.Fatalf("WorkerDir: %v", err)
	}
	want := "/tmp/fleet-test/projects/fleet/workers/add-readme-7a3c/"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestWorkerDir_SafeSlug(t *testing.T) {
	// Slug is supposed to be [a-z0-9-]+-<4hex>, but defensively pass
	// it through SafeLockComponent so a crafted slug never escapes
	// the workers/ dir. Dot-only / dot-dot must be rejected too.
	t.Setenv("FLEET_HOME", "/tmp/fleet-test")
	for _, in := range []string{"../escape", "foo bar"} {
		got, err := WorkerDir("fleet", in)
		if err != nil {
			t.Fatalf("WorkerDir(%q): %v", in, err)
		}
		if strings.Contains(got, "..") && !strings.HasSuffix(got, "_../") && !strings.Contains(got, ".._escape") {
			t.Errorf("WorkerDir(%q)=%q escaped sanitizer", in, got)
		}
		if strings.Contains(got, " ") {
			t.Errorf("WorkerDir(%q)=%q kept space", in, got)
		}
	}
}

func TestWorkerArchiveDir(t *testing.T) {
	t.Setenv("FLEET_HOME", "/tmp/fleet-test")
	got, err := WorkerArchiveDir("fleet", "add-readme-7a3c", "20260506-140000")
	if err != nil {
		t.Fatalf("WorkerArchiveDir: %v", err)
	}
	want := "/tmp/fleet-test/projects/fleet/workers/archive/add-readme-7a3c-20260506-140000/"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestWorktreePath(t *testing.T) {
	t.Setenv("FLEET_HOME", "/tmp/fleet-test")
	got, err := WorktreePath("fleet", "add-readme-7a3c")
	if err != nil {
		t.Fatalf("WorktreePath: %v", err)
	}
	want := "/tmp/fleet-test/projects/fleet/worktrees/add-readme-7a3c/"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}
