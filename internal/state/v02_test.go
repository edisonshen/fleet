package state

import (
	"testing"
)

// v0.2 introduces per-project state directories under
// ~/.fleet/projects/<safe-name>/. These tests pin the new helpers'
// path layout so callers (internal/tasks, internal/learnings,
// internal/standards, internal/workers) can rely on the shapes.

func TestProjectDir_AcceptsValidNames(t *testing.T) {
	t.Setenv("FLEET_HOME", "/tmp/fleet-test")
	cases := map[string]string{
		"fleet":       "/tmp/fleet-test/projects/fleet/",
		"gift-finder": "/tmp/fleet-test/projects/gift-finder/",
		"v2.1":        "/tmp/fleet-test/projects/v2.1/",
		"my_project":  "/tmp/fleet-test/projects/my_project/",
		"":            "/tmp/fleet-test/projects/_default/",
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

func TestProjectDir_RejectsAliasingNames(t *testing.T) {
	// ProjectDir refuses inputs that SafeLockComponent would alias —
	// silent collision on tasks.md / workers/ would corrupt state
	// across two distinct projects.
	t.Setenv("FLEET_HOME", "/tmp/fleet-test")
	for _, in := range []string{
		"owner/repo",
		"gift finder",
		"foo:bar",
		"..",
		"../escape",
		".",
	} {
		t.Run(in, func(t *testing.T) {
			_, err := ProjectDir(in)
			if err == nil {
				t.Errorf("ProjectDir(%q) returned nil err; want validation failure", in)
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

func TestProjectStateLockPath_RejectsAliasingNames(t *testing.T) {
	// ProjectStateLockPath layers on ProjectDir, which refuses
	// names that would alias on disk. Verify the rejection
	// propagates.
	t.Setenv("FLEET_HOME", "/tmp/fleet-test")
	_, err := ProjectStateLockPath("owner/repo")
	if err == nil {
		t.Error("ProjectStateLockPath(owner/repo) returned nil err; want validation failure")
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

func TestWorkerDir_RejectsUnsafeSlug(t *testing.T) {
	// Slug is supposed to be [a-z0-9-]+-<4hex>. WorkerDir refuses
	// slugs containing path separators or other unsafe runes —
	// silent SafeLockComponent mapping would alias `feature/a` and
	// `feature_a` onto the same workers/<x>/ directory.
	t.Setenv("FLEET_HOME", "/tmp/fleet-test")
	for _, in := range []string{"../escape", "foo bar", "feature/a", "with:colon"} {
		t.Run(in, func(t *testing.T) {
			_, err := WorkerDir("fleet", in)
			if err == nil {
				t.Errorf("WorkerDir(fleet, %q) returned nil err; want rejection", in)
			}
		})
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
