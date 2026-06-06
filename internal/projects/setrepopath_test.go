package projects

import (
	"errors"
	"testing"
	"time"
)

// SetRepoPath tests (U23–U27) — race-safe Read-modify-Write +
// compare-and-set guard. All use FLEET_HOME=t.TempDir() (no /tmp litter).

// U23 — clean merge: SetRepoPath sets ONLY repo_path + fingerprint and
// preserves added_at / schema / is_git.
func TestSetRepoPath_CleanMergePreservesFields(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())

	added := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	if err := Write("demo", Meta{
		Schema:  "v1",
		AddedAt: added,
		IsGit:   BoolPtr(true),
	}); err != nil {
		t.Fatalf("seed Write: %v", err)
	}

	if err := SetRepoPath("demo", "", "/repos/x", "rootsha|orighash"); err != nil {
		t.Fatalf("SetRepoPath: %v", err)
	}

	got, err := Read("demo")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.RepoPath != "/repos/x" {
		t.Errorf("RepoPath=%q want /repos/x", got.RepoPath)
	}
	if got.RepoFingerprint != "rootsha|orighash" {
		t.Errorf("RepoFingerprint=%q want rootsha|orighash", got.RepoFingerprint)
	}
	if !got.AddedAt.Equal(added) {
		t.Errorf("AddedAt=%v clobbered, want %v", got.AddedAt, added)
	}
	if got.Schema != "v1" {
		t.Errorf("Schema=%q clobbered, want v1", got.Schema)
	}
	if got.IsGit == nil || !*got.IsGit {
		t.Errorf("IsGit=%v clobbered, want true", got.IsGit)
	}
}

// U24 — compare-and-set: a DIFFERENT fingerprint appeared mid-flight →
// ErrPinLostRace, existing pin unchanged.
func TestSetRepoPath_DifferentFingerprintLosesRace(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())

	// A racing `project add` already pinned repo X with fingerprint FX.
	if err := Write("demo", Meta{
		Schema:          "v1",
		RepoPath:        "/repos/x",
		RepoFingerprint: "rootX|origX",
		IsGit:           BoolPtr(true),
	}); err != nil {
		t.Fatalf("seed Write: %v", err)
	}

	// We observed an empty pin earlier (prev="") and try to write Y/FY.
	err := SetRepoPath("demo", "", "/repos/y", "rootY|origY")
	if err == nil {
		t.Fatal("expected ErrPinLostRace, got nil")
	}
	var lost ErrPinLostRace
	if !errors.As(err, &lost) {
		t.Fatalf("err=%v want ErrPinLostRace", err)
	}
	if lost.Stored != "/repos/x" {
		t.Errorf("Stored=%q want /repos/x", lost.Stored)
	}

	got, _ := Read("demo")
	if got.RepoPath != "/repos/x" || got.RepoFingerprint != "rootX|origX" {
		t.Errorf("pin clobbered: %+v", got)
	}
}

// U25 — compare-and-set: repo_path changed under us (Z != prev != y) with
// an empty fingerprint → ErrPinLostRace.
func TestSetRepoPath_RepoPathChangedLosesRace(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())

	// A racing non-git `project add` landed repo_path=Z, no fingerprint.
	if err := Write("demo", Meta{
		Schema:   "v1",
		RepoPath: "/repos/z",
		IsGit:    BoolPtr(false),
	}); err != nil {
		t.Fatalf("seed Write: %v", err)
	}

	// We observed prev="" and try to write y with empty fp.
	err := SetRepoPath("demo", "", "/repos/y", "")
	if err == nil {
		t.Fatal("expected ErrPinLostRace, got nil")
	}
	var lost ErrPinLostRace
	if !errors.As(err, &lost) {
		t.Fatalf("err=%v want ErrPinLostRace", err)
	}
	got, _ := Read("demo")
	if got.RepoPath != "/repos/z" {
		t.Errorf("pin clobbered: RepoPath=%q want /repos/z", got.RepoPath)
	}
}

// U25-variant — matching stored fingerprint (cross-machine missing-path
// re-derive): write proceeds and updates repo_path to the local path.
func TestSetRepoPath_MatchingFingerprintUpdatesPath(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())

	if err := Write("demo", Meta{
		Schema:          "v1",
		RepoPath:        "/old/path",
		RepoFingerprint: "root|orig",
		IsGit:           BoolPtr(true),
	}); err != nil {
		t.Fatalf("seed Write: %v", err)
	}

	// prevPath observed = /old/path; same fingerprint, new local path.
	if err := SetRepoPath("demo", "/old/path", "/local/path", "root|orig"); err != nil {
		t.Fatalf("SetRepoPath: %v", err)
	}
	got, _ := Read("demo")
	if got.RepoPath != "/local/path" {
		t.Errorf("RepoPath=%q want /local/path", got.RepoPath)
	}
	if got.RepoFingerprint != "root|orig" {
		t.Errorf("RepoFingerprint=%q changed, want root|orig", got.RepoFingerprint)
	}
}

// U26 — concurrent add (git, different repo): SetRepoPath sees a different
// stored fingerprint → ErrPinLostRace (the resolver then retries). Here we
// assert the SetRepoPath half (the resolver retry is exercised in
// coordrepo's tests). Also covers the from-absent meta case.
func TestSetRepoPath_FromAbsentMeta(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())

	// No meta on disk; a from-scratch derive persists.
	if err := SetRepoPath("fresh", "", "/repos/fresh", "root|orig"); err != nil {
		t.Fatalf("SetRepoPath from absent: %v", err)
	}
	got, err := Read("fresh")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.RepoPath != "/repos/fresh" || got.RepoFingerprint != "root|orig" {
		t.Errorf("unexpected meta: %+v", got)
	}
	// Schema defaulted on Write.
	if got.Schema != SchemaVersion {
		t.Errorf("Schema=%q want %q", got.Schema, SchemaVersion)
	}
}

// U27 — compare-and-set: repo_path appeared identical to our intended path
// (idempotent re-persist) → succeeds (not a lost race).
func TestSetRepoPath_IdempotentSamePath(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())

	if err := Write("demo", Meta{
		Schema:   "v1",
		RepoPath: "/repos/y",
		IsGit:    BoolPtr(true),
	}); err != nil {
		t.Fatalf("seed Write: %v", err)
	}
	// stored repo_path == path we want to write, empty fp on both sides.
	if err := SetRepoPath("demo", "", "/repos/y", "root|orig"); err != nil {
		t.Fatalf("SetRepoPath idempotent: %v", err)
	}
	got, _ := Read("demo")
	if got.RepoFingerprint != "root|orig" {
		t.Errorf("fingerprint not stamped: %q", got.RepoFingerprint)
	}
}
