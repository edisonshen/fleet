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

func TestValidateSlug_RejectsReserved(t *testing.T) {
	// Slug "archive" (any case on case-insensitive APFS) would alias
	// workers/archive/ — the reserved holding pen for archived
	// worker dirs. ValidateSlug must reject it so callers (tasks.Add,
	// tasks.Read, WorkerDir, WorkerArchiveDir, WorktreePath) all see
	// the rejection.
	for _, in := range []string{"archive", "Archive", "ARCHIVE", "ArChIvE", ".", "..", ".."} {
		t.Run(in, func(t *testing.T) {
			if err := ValidateSlug(in); err == nil {
				t.Errorf("ValidateSlug(%q) returned nil err; want rejection", in)
			}
		})
	}
}

func TestValidateProjectName_RejectsReserved(t *testing.T) {
	// ".locks" aliases the reserved projects/.locks/ control dir.
	// (Case variants are caught by the lowercase-only rule below
	// before they ever reach the reserved-name comparison, but we
	// keep the explicit reserved-name reject so the error message
	// is precise on the exact-match case.)
	for _, in := range []string{".locks", ".", ".."} {
		t.Run(in, func(t *testing.T) {
			if err := ValidateProjectName(in); err == nil {
				t.Errorf("ValidateProjectName(%q) returned nil err; want rejection", in)
			}
		})
	}
}

func TestValidateProjectName_RejectsFlagMisparse(t *testing.T) {
	// Regression for invalid-project-dir-guar-d636: a `--project` flag
	// misparse (e.g. `fleet tasks list --project=--project` or
	// `--project=-x`) captured a flag-shaped token as the project NAME.
	// Hyphen IS an allowed interior char, so the old loop accepted these
	// and a bogus ~/.fleet/projects/--project/ dir polluted the dashboard
	// (title "FLEET /--project", inflated count). Leading "-" and
	// punctuation-only names are now rejected at this single chokepoint.
	for _, in := range []string{"--project", "-x", "-", "--", "-foo", "_._", "._", "...", "-_-"} {
		t.Run(in, func(t *testing.T) {
			if err := ValidateProjectName(in); err == nil {
				t.Errorf("ValidateProjectName(%q) returned nil err; want rejection (flag-misparse / punctuation-only)", in)
			}
		})
	}
}

func TestValidateProjectName_RejectsUppercase(t *testing.T) {
	// macOS/APFS case-insensitive default would alias "Foo" and
	// "foo" onto the same projects/<name>/ tree → silent state
	// corruption. Lowercase-only is the canonical-name rule.
	for _, in := range []string{"Fleet", "FLEET", "myProject", "MyProject", "iOS", "macOS"} {
		t.Run(in, func(t *testing.T) {
			if err := ValidateProjectName(in); err == nil {
				t.Errorf("ValidateProjectName(%q) returned nil err; want rejection (uppercase)", in)
			}
		})
	}
}

func TestValidateSlug_RejectsUppercase(t *testing.T) {
	// Same case-insensitive aliasing risk as project names: two
	// slugs differing only in case alias the same workers/<slug>/
	// while tasks.Read treats them as distinct.
	for _, in := range []string{"Alpha-1234", "ALPHA-1234", "alphaBeta-1234", "Task-9999"} {
		t.Run(in, func(t *testing.T) {
			if err := ValidateSlug(in); err == nil {
				t.Errorf("ValidateSlug(%q) returned nil err; want rejection (uppercase)", in)
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
