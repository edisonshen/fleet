package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBootstrap_CreatesAllSubdirs(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)

	root, err := Bootstrap()
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if root != tmp {
		t.Fatalf("Root mismatch: got %q want %q", root, tmp)
	}

	for _, sub := range subdirs {
		path := filepath.Join(tmp, sub)
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("missing subdir %s: %v", sub, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("%s is not a directory", sub)
		}
	}
}

func TestBootstrap_Idempotent(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)

	if _, err := Bootstrap(); err != nil {
		t.Fatalf("first Bootstrap: %v", err)
	}
	if _, err := Bootstrap(); err != nil {
		t.Fatalf("second Bootstrap: %v", err)
	}
}

func TestWriteAtomic_PublishesFile(t *testing.T) {
	tmp := t.TempDir()
	dest := filepath.Join(tmp, "out.json")
	want := []byte(`{"hello":"world"}`)

	if err := WriteAtomic(dest, want); err != nil {
		t.Fatalf("WriteAtomic: %v", err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("content mismatch: got %q want %q", got, want)
	}
}

func TestWriteAtomic_NoTmpLeftover(t *testing.T) {
	tmp := t.TempDir()
	dest := filepath.Join(tmp, "out.json")

	if err := WriteAtomic(dest, []byte("ok")); err != nil {
		t.Fatalf("WriteAtomic: %v", err)
	}

	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "out.json" {
			t.Errorf("unexpected leftover: %s", e.Name())
		}
	}
}

func TestWriteAtomic_OverwritesExisting(t *testing.T) {
	tmp := t.TempDir()
	dest := filepath.Join(tmp, "out.json")

	if err := WriteAtomic(dest, []byte("v1")); err != nil {
		t.Fatalf("first WriteAtomic: %v", err)
	}
	if err := WriteAtomic(dest, []byte("v2")); err != nil {
		t.Fatalf("second WriteAtomic: %v", err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "v2" {
		t.Errorf("got %q want v2", got)
	}
}

func TestRoot_HonorsFLEET_HOME(t *testing.T) {
	t.Setenv("FLEET_HOME", "/tmp/fleet-test-root")
	root, err := Root()
	if err != nil {
		t.Fatalf("Root: %v", err)
	}
	if root != "/tmp/fleet-test-root" {
		t.Errorf("got %q", root)
	}
}

func TestAgentPath(t *testing.T) {
	t.Setenv("FLEET_HOME", "/tmp/fleet-test")
	got, err := AgentPath("a1b2")
	if err != nil {
		t.Fatalf("AgentPath: %v", err)
	}
	want := "/tmp/fleet-test/agents/a1b2.json"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestAgentArchivePath(t *testing.T) {
	t.Setenv("FLEET_HOME", "/tmp/fleet-test")
	got, err := AgentArchivePath("a1b2")
	if err != nil {
		t.Fatalf("AgentArchivePath: %v", err)
	}
	want := "/tmp/fleet-test/agents/archive/a1b2.json"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestHandoffDir(t *testing.T) {
	t.Setenv("FLEET_HOME", "/tmp/fleet-test")
	got, err := HandoffDir()
	if err != nil {
		t.Fatalf("HandoffDir: %v", err)
	}
	want := "/tmp/fleet-test/handoffs"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestHandoffPath_FormatsUTCTimestampWithRandomSuffix(t *testing.T) {
	t.Setenv("FLEET_HOME", "/tmp/fleet-test")

	// Pacific time input — must be rendered as the equivalent UTC
	// in the filename so different operators produce identical
	// timestamps for identical handoffs (random suffix differs).
	pacific, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skip("zoneinfo not available on this platform")
	}
	ts := time.Date(2026, 4, 15, 7, 32, 0, 0, pacific) // 14:32:00 UTC

	got, err := HandoffPath("a1b2", ts)
	if err != nil {
		t.Fatalf("HandoffPath: %v", err)
	}
	wantPrefix := "/tmp/fleet-test/handoffs/a1b2-20260415-143200-"
	if !strings.HasPrefix(got, wantPrefix) || !strings.HasSuffix(got, ".md") {
		t.Errorf("got %q want prefix %q + 4-hex-char suffix + .md", got, wantPrefix)
	}
}

func TestHandoffPath_RandomSuffixDiffersAcrossCalls(t *testing.T) {
	// Same agent + same UTC second must produce different filenames
	// (P3 from codex iter-9: retries within one second would
	// otherwise overwrite the previous doc and break the chain).
	t.Setenv("FLEET_HOME", "/tmp/fleet-test")
	ts := time.Date(2026, 4, 27, 18, 48, 7, 0, time.UTC)
	a, _ := HandoffPath("7f3a92e1", ts)
	b, _ := HandoffPath("7f3a92e1", ts)
	if a == b {
		t.Errorf("HandoffPath returned identical paths %q for same input — random suffix not applied", a)
	}
	// Both must share the timestamped prefix.
	prefix := "/tmp/fleet-test/handoffs/7f3a92e1-20260427-184807-"
	if !strings.HasPrefix(a, prefix) || !strings.HasPrefix(b, prefix) {
		t.Errorf("paths missing expected prefix %q: a=%q b=%q", prefix, a, b)
	}
}

func TestQueueDir(t *testing.T) {
	t.Setenv("FLEET_HOME", "/tmp/fleet-test")
	got, err := QueueDir()
	if err != nil {
		t.Fatalf("QueueDir: %v", err)
	}
	want := "/tmp/fleet-test/queue"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestQueuePath_AppendsJSONExtension(t *testing.T) {
	t.Setenv("FLEET_HOME", "/tmp/fleet-test")
	got, err := QueuePath("spawn-fresh-a1b2")
	if err != nil {
		t.Fatalf("QueuePath: %v", err)
	}
	want := "/tmp/fleet-test/queue/spawn-fresh-a1b2.json"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestProjectLockPath(t *testing.T) {
	t.Setenv("FLEET_HOME", "/tmp/fleet-test")
	got, err := ProjectLockPath("rainier")
	if err != nil {
		t.Fatalf("ProjectLockPath: %v", err)
	}
	want := "/tmp/fleet-test/projects/.locks/rainier.lock"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestProjectLockPath_SanitizesLegacyNames(t *testing.T) {
	// ProjectLockPath must NOT reject legacy project names — that
	// would break handoff for any agent dispatched before
	// ValidateProjectName landed at the CLI. SafeLockComponent maps
	// unsafe chars to "_" so the lock file path is always valid.
	t.Setenv("FLEET_HOME", "/tmp/fleet-test")
	cases := map[string]string{
		"owner/repo":  "/tmp/fleet-test/projects/.locks/owner_repo.lock",
		"gift finder": "/tmp/fleet-test/projects/.locks/gift_finder.lock",
		"foo:bar":     "/tmp/fleet-test/projects/.locks/foo_bar.lock",
		"":            "/tmp/fleet-test/projects/.locks/_default.lock",
		".":           "/tmp/fleet-test/projects/.locks/_..lock",
		"..":          "/tmp/fleet-test/projects/.locks/_...lock",
		"../escape":   "/tmp/fleet-test/projects/.locks/.._escape.lock",
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			got, err := ProjectLockPath(in)
			if err != nil {
				t.Fatalf("ProjectLockPath(%q): %v", in, err)
			}
			if got != want {
				t.Errorf("got %q want %q", got, want)
			}
		})
	}
}

func TestSafeLockComponent_StableForLegalNames(t *testing.T) {
	// Legal names should pass through unchanged so existing locks
	// (and existing test expectations) keep working.
	for _, in := range []string{"rainier", "gift-finder", "v2.1", "abc_123"} {
		if got := SafeLockComponent(in); got != in {
			t.Errorf("SafeLockComponent(%q) = %q, want pass-through", in, got)
		}
	}
}

func TestValidateProjectName_Accepts(t *testing.T) {
	for _, name := range []string{
		"rainier",
		"gift-finder",
		"caching",
		"v2.1",
		"my_project",
		"abc123",
		"a",
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateProjectName(name); err != nil {
				t.Errorf("ValidateProjectName(%q) rejected: %v", name, err)
			}
		})
	}
}

func TestBootstrap_CreatesQueueAndHandoffAndLockDirs(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)

	if _, err := Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	for _, sub := range []string{"queue", "handoffs", "projects/.locks", "agents/archive"} {
		info, err := os.Stat(filepath.Join(tmp, sub))
		if err != nil {
			t.Errorf("missing %s: %v", sub, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("%s is not a directory", sub)
		}
	}
}

// TestEnsureProjectInitialized_CreatesDirOnMissing pins the v0.2 per-
// project bootstrap path: when the operator hits [a] on a project row
// for the first time, EnsureProjectInitialized must create
// ~/.fleet/projects/<name>/.locks/ before dispatch so the spawned coord
// skill's first tick can publish coord-state.json + acquire the flock
// without racing on a missing parent (issue #63).
func TestEnsureProjectInitialized_CreatesDirOnMissing(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)

	dir, err := EnsureProjectInitialized("demo")
	if err != nil {
		t.Fatalf("EnsureProjectInitialized: %v", err)
	}
	wantDir := filepath.Join(tmp, "projects", "demo")
	if dir != wantDir {
		t.Errorf("returned dir = %q; want %q", dir, wantDir)
	}
	for _, sub := range []string{"", ".locks"} {
		info, serr := os.Stat(filepath.Join(wantDir, sub))
		if serr != nil {
			t.Errorf("missing %s: %v", filepath.Join(wantDir, sub), serr)
			continue
		}
		if !info.IsDir() {
			t.Errorf("%s is not a directory", filepath.Join(wantDir, sub))
		}
	}
}

// TestEnsureProjectInitialized_NoOpWhenPresent confirms idempotency —
// a second call after the dirs already exist must not error and must
// not disturb existing files inside (a coord that already wrote
// coord-state.json must not see it truncated).
func TestEnsureProjectInitialized_NoOpWhenPresent(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)

	if _, err := EnsureProjectInitialized("demo"); err != nil {
		t.Fatalf("first EnsureProjectInitialized: %v", err)
	}
	// Drop a sentinel inside .locks/ to confirm it survives a second call.
	sentinel := filepath.Join(tmp, "projects", "demo", ".locks", "sentinel")
	if err := os.WriteFile(sentinel, []byte("keep me"), 0o644); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}
	if _, err := EnsureProjectInitialized("demo"); err != nil {
		t.Fatalf("second EnsureProjectInitialized: %v", err)
	}
	got, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatalf("read sentinel after second call: %v", err)
	}
	if string(got) != "keep me" {
		t.Errorf("sentinel content disturbed: got %q", got)
	}
}

// TestCoordSpawnMarker_RoundTrip pins issue #63 codex iter-3 P2: the
// TUI writes the marker after dispatch with the new coord agent's ID;
// the dashboard reads it to validate the candidate record matches.
// Atomic write + first-line read with whitespace trim.
func TestCoordSpawnMarker_RoundTrip(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)

	if _, err := EnsureProjectInitialized("demo"); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := WriteCoordSpawnMarker("demo", "abcd1234"); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := ReadCoordSpawnMarker("demo")
	if got != "abcd1234" {
		t.Errorf("read = %q; want abcd1234", got)
	}
}

// TestCoordSpawnMarker_OverwritesExisting confirms marker writes are
// last-writer-wins (idempotent for retries / re-spawns).
func TestCoordSpawnMarker_OverwritesExisting(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)

	if _, err := EnsureProjectInitialized("demo"); err != nil {
		t.Fatalf("init: %v", err)
	}
	for _, id := range []string{"oldold01", "newnew02", "latest03"} {
		if err := WriteCoordSpawnMarker("demo", id); err != nil {
			t.Fatalf("write %q: %v", id, err)
		}
	}
	got := ReadCoordSpawnMarker("demo")
	if got != "latest03" {
		t.Errorf("read = %q; want latest03 (last write)", got)
	}
}

// TestCoordSpawnMarker_AbsentReturnsEmpty pins read robustness — a
// missing marker returns "" silently rather than err. The dashboard
// fallback uses "" as "no in-flight coord; don't promote".
func TestCoordSpawnMarker_AbsentReturnsEmpty(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	if got := ReadCoordSpawnMarker("nonexistent"); got != "" {
		t.Errorf("absent marker = %q; want empty string", got)
	}
}

// TestCoordSpawnMarker_TrimsWhitespaceAndCRLF pins read tolerance:
// hand-edited markers with CRLF / trailing whitespace must still
// produce the bare agent ID.
func TestCoordSpawnMarker_TrimsWhitespaceAndCRLF(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	if _, err := EnsureProjectInitialized("demo"); err != nil {
		t.Fatalf("init: %v", err)
	}
	path, err := CoordSpawnMarkerPath("demo")
	if err != nil {
		t.Fatalf("path: %v", err)
	}
	cases := map[string]string{
		"abcd1234":            "abcd1234",
		"abcd1234\n":          "abcd1234",
		"abcd1234\r\n":        "abcd1234",
		"  abcd1234  \n":      "abcd1234",
		"abcd1234\nextrajunk": "abcd1234",
	}
	for content, want := range cases {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %q: %v", content, err)
		}
		if got := ReadCoordSpawnMarker("demo"); got != want {
			t.Errorf("ReadCoordSpawnMarker for %q = %q; want %q", content, got, want)
		}
	}
}

// TestRemoveCoordSpawnMarker_RemovesAndIsIdempotent pins issue #96
// gap 1's clear-on-self-heal contract: the remove path must delete an
// existing marker AND collapse to nil when the marker is already gone
// (race: concurrent dashboard render or in-flight dispatch goroutine
// already cleared it). Non-ENOENT errors propagate so the caller's
// flash banner can surface real I/O problems.
func TestRemoveCoordSpawnMarker_RemovesAndIsIdempotent(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	if _, err := EnsureProjectInitialized("demo"); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := WriteCoordSpawnMarker("demo", "abcd1234"); err != nil {
		t.Fatalf("write: %v", err)
	}
	path, err := CoordSpawnMarkerPath("demo")
	if err != nil {
		t.Fatalf("path: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("marker should exist before remove: %v", err)
	}
	if err := RemoveCoordSpawnMarker("demo"); err != nil {
		t.Errorf("first remove: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("marker should be gone after remove; stat err = %v", err)
	}
	// Idempotent: a second remove on an absent marker must not error.
	if err := RemoveCoordSpawnMarker("demo"); err != nil {
		t.Errorf("second (no-op) remove must be nil; got %v", err)
	}
}

// TestRemoveCoordSpawnMarker_RejectsInvalidName mirrors WriteCoordSpawnMarker
// validation so a hand-mangled project name doesn't accidentally
// resolve to a path-traversal target. Same rule set as ProjectDir.
func TestRemoveCoordSpawnMarker_RejectsInvalidName(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	for _, bad := range []string{"owner/repo", "..", ".", "Foo"} {
		if err := RemoveCoordSpawnMarker(bad); err == nil {
			t.Errorf("RemoveCoordSpawnMarker(%q) should reject; got nil err", bad)
		}
	}
}

// TestEnsureProjectInitialized_RejectsInvalidName ensures the validator
// fires (no path-traversal via "../" or absolute-path injection). Same
// rule as ProjectDir; no silent mapping.
func TestEnsureProjectInitialized_RejectsInvalidName(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	for _, bad := range []string{"owner/repo", "..", ".", "Foo"} {
		if _, err := EnsureProjectInitialized(bad); err == nil {
			t.Errorf("EnsureProjectInitialized(%q) should reject; got nil err", bad)
		}
	}
}
