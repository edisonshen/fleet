package projects

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/state"
)

// TestMeta_RoundTrip pins the basic Read/Write contract: a Meta
// written to disk parses back to an equal struct.
func TestMeta_RoundTrip(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())

	m := Meta{
		Schema:   "v1",
		RepoPath: "/abs/path/to/repo",
		AddedAt:  time.Date(2026, 5, 9, 20, 30, 0, 0, time.UTC),
	}
	if err := Write("demo", m); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := Read("demo")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Schema != m.Schema {
		t.Errorf("Schema=%q want %q", got.Schema, m.Schema)
	}
	if got.RepoPath != m.RepoPath {
		t.Errorf("RepoPath=%q want %q", got.RepoPath, m.RepoPath)
	}
	if !got.AddedAt.Equal(m.AddedAt) {
		t.Errorf("AddedAt=%v want %v", got.AddedAt, m.AddedAt)
	}
}

// TestMeta_WriteAtomicNoLeftoverTmp regresses the "atomic publish"
// guarantee: after a successful Write, no .tmp files linger in the
// project's directory. Mirrors hidden_test.go's atomic check.
func TestMeta_WriteAtomicNoLeftoverTmp(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEET_HOME", root)

	m := Meta{Schema: "v1", RepoPath: "/abs/path", AddedAt: time.Now().UTC()}
	if err := Write("demo", m); err != nil {
		t.Fatalf("Write: %v", err)
	}

	dir := filepath.Join(root, "projects", "demo")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp.") {
			t.Errorf("leftover tmp file: %s", e.Name())
		}
	}
}

// TestMeta_ReadMissingReturnsErrNotFound: callers must distinguish
// "no meta on disk" from "read failed". Existing projects without
// meta.json must continue to work — Read signals absence so callers
// can choose to ignore or fall back.
func TestMeta_ReadMissingReturnsErrNotFound(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())
	_, err := Read("ghost")
	if err == nil {
		t.Fatal("expected error reading missing meta, got nil")
	}
	if err != ErrNotFound {
		t.Errorf("err=%v, want ErrNotFound", err)
	}
}

// TestMeta_OnDiskShape pins the JSON keys the spec requires
// (schema, repo_path, added_at). Future readers (per-project
// default-cwd, summary feature) depend on these names — bumping
// them is a schema change.
func TestMeta_OnDiskShape(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEET_HOME", root)

	m := Meta{
		Schema:   "v1",
		RepoPath: "/abs/path",
		AddedAt:  time.Date(2026, 5, 9, 20, 30, 0, 0, time.UTC),
	}
	if err := Write("demo", m); err != nil {
		t.Fatalf("Write: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(root, "projects", "demo", "meta.json"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if raw["schema"] != "v1" {
		t.Errorf("schema field=%v, want v1", raw["schema"])
	}
	if raw["repo_path"] != "/abs/path" {
		t.Errorf("repo_path field=%v, want /abs/path", raw["repo_path"])
	}
	if _, ok := raw["added_at"]; !ok {
		t.Errorf("added_at field missing; got keys=%v", raw)
	}
}

// TestMeta_RejectsInvalidProjectName regresses the safe-name rule.
// The state package validates project names; Write must fail loudly
// on inputs that would silently alias on disk.
func TestMeta_RejectsInvalidProjectName(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())
	err := Write("Foo/Bar", Meta{Schema: "v1", RepoPath: "/x", AddedAt: time.Now().UTC()})
	if err == nil {
		t.Fatal("expected validation error on bad project name, got nil")
	}
}

// TestSanitizeTag_ProducerValidatorContract regresses codex iter-3 [P2]:
// ValidateProjectName now rejects leading-hyphen + punctuation-only names,
// so the sanitizeTag producer must never emit one. A path that sanitizes to
// punctuation-only ("_", "_._") or leading-hyphen must fall back to "fleet",
// not a tag the validator would reject. EVERY sanitizeTag output must pass
// ValidateProjectName, or `fleet project add` would refuse a tag it itself
// generated for an otherwise-valid path.
func TestSanitizeTag_ProducerValidatorContract(t *testing.T) {
	for _, in := range []string{
		"_", "_._", "...", "---", "-x", "--project",
		"@@@", "  ", "/", "ok-name", "Foo_Bar", "123",
		"-leading", "trailing-", ".dotfile", "a.b.c",
	} {
		t.Run(in, func(t *testing.T) {
			got := sanitizeTag(in)
			if err := state.ValidateProjectName(got); err != nil {
				t.Errorf("sanitizeTag(%q)=%q but ValidateProjectName rejects it: %v", in, got, err)
			}
		})
	}

	// Underscores are valid edge + interior chars; the punctuation-only
	// fallback must NOT trim a trailing/leading "_" off an otherwise-valid
	// tag, or distinct repos alias onto one project (codex iter-4 [P2]).
	preserve := map[string]string{
		"repos-foo_": "repos-foo_",
		"_foo":       "_foo",
		"a_b":        "a_b",
		"foo.bar":    "foo.bar",
	}
	for in, want := range preserve {
		t.Run("preserve/"+in, func(t *testing.T) {
			if got := sanitizeTag(in); got != want {
				t.Errorf("sanitizeTag(%q)=%q, want %q (underscore-bearing tags must survive)", in, got, want)
			}
		})
	}
}

// TestMeta_LegacyFileWithoutIsGit_ParsesAsTrue: a meta.json written by
// v0.8.x and earlier (no is_git field) must continue to behave as a
// git-backed project. GitMode() returns true; the on-disk file is
// untouched on subsequent Reads.
func TestMeta_LegacyFileWithoutIsGit_ParsesAsTrue(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEET_HOME", root)
	// Hand-craft a legacy meta.json with no is_git field. Reproduces
	// the on-disk shape of every project registered before this PR.
	dir := filepath.Join(root, "projects", "legacy-demo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	legacy := `{"schema":"v1","repo_path":"/abs/old","added_at":"2026-04-01T00:00:00Z"}`
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(legacy), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := Read("legacy-demo")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.IsGit != nil {
		t.Errorf("IsGit should remain nil for legacy file, got %v", *got.IsGit)
	}
	if !got.GitMode() {
		t.Errorf("GitMode() should default to true for legacy file")
	}
}

// TestMeta_IsGitFalseRoundTrip: when the operator registers a non-git
// directory, IsGit=false must round-trip through disk verbatim. The
// JSON encoder MUST emit the field (so a re-read sees false rather
// than the legacy-default true).
func TestMeta_IsGitFalseRoundTrip(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEET_HOME", root)

	m := Meta{
		Schema:   "v1",
		RepoPath: "/abs/non-git/dir",
		AddedAt:  time.Date(2026, 5, 12, 0, 0, 0, 0, time.UTC),
		IsGit:    BoolPtr(false),
	}
	if err := Write("non-git-demo", m); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Inspect raw on-disk shape: is_git must be present and false.
	data, err := os.ReadFile(filepath.Join(root, "projects", "non-git-demo", "meta.json"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal raw: %v", err)
	}
	v, ok := raw["is_git"]
	if !ok {
		t.Fatalf("is_git missing on disk for explicit-false; raw=%v", raw)
	}
	if b, _ := v.(bool); b {
		t.Errorf("is_git on disk = %v, want false", v)
	}

	got, err := Read("non-git-demo")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.IsGit == nil {
		t.Fatal("IsGit nil after read; want explicit false")
	}
	if *got.IsGit {
		t.Errorf("IsGit=%v, want false", *got.IsGit)
	}
	if got.GitMode() {
		t.Errorf("GitMode()=true, want false")
	}
}

// TestMeta_IsGitTrueRoundTrip: when IsGit is set explicitly to true,
// Read also reflects that. (The legacy default-true path is tested
// separately above.) The on-disk file omits the field (matches the
// omitempty json tag) only when the pointer is nil — an explicit
// pointer-to-true keeps the field present.
func TestMeta_IsGitTrueRoundTrip(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEET_HOME", root)

	m := Meta{
		Schema:   "v1",
		RepoPath: "/abs/git/dir",
		AddedAt:  time.Date(2026, 5, 12, 0, 0, 0, 0, time.UTC),
		IsGit:    BoolPtr(true),
	}
	if err := Write("git-demo", m); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := Read("git-demo")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.IsGit == nil || !*got.IsGit {
		t.Errorf("IsGit=%v, want pointer to true", got.IsGit)
	}
	if !got.GitMode() {
		t.Errorf("GitMode()=false, want true")
	}
}
