// Package projects owns ~/.fleet/projects/<name>/meta.json — the
// per-project metadata file written by `fleet project add` and
// (eventually) consumed by the dashboard's per-project default-cwd
// + summary features.
//
// The schema is intentionally tiny in v1: schema, repo_path, added_at.
// Future readers tolerate a missing file (existing pre-meta projects
// continue to work); only callers that need the data must opt in.
//
// Atomic publish via state.WriteAtomic (tmp + fsync + rename) so
// concurrent readers (TUI dashboard scan, future per-project consumers)
// never observe a torn file.
package projects

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/edisonshen/fleet/internal/state"
)

// Meta is the parsed shape of meta.json. Only the three fields the
// spec requires; future fields land here as omitempty so older readers
// continue to round-trip them through Write.
//
// IsGit is a pointer so the JSON layer can distinguish "field absent on
// disk" (legacy meta.json predating non-git support) from "field present,
// value false" (operator added a non-git directory). Callers should
// route the git-vs-non-git decision through GitMode(); it treats nil
// (absent field) as true so legacy projects keep behaving as git
// projects unchanged. Read() returns the field verbatim — it does NOT
// normalize.
type Meta struct {
	Schema   string    `json:"schema"`
	RepoPath string    `json:"repo_path"`
	AddedAt  time.Time `json:"added_at"`
	IsGit    *bool     `json:"is_git,omitempty"`

	// RepoFingerprint is the strong, machine-independent identity of the
	// repo at RepoPath: `<root-commit-sha>|<origin-hash-OR-fleet-repoId>`
	// (minted by internal/coordrepo). Stamped at `fleet project add` / on
	// first successful bind. Empty for legacy meta.json files written
	// before the field existed, and for shallow clones that cannot be
	// stamped. omitempty so legacy readers round-trip absent files.
	RepoFingerprint string `json:"repo_fingerprint,omitempty"`
}

// GitMode returns true when the project is git-backed. Defaults to true
// when IsGit is nil — legacy meta.json files pre-date the field and the
// invariant "v0.8.x and earlier only registered git projects" makes the
// default safe.
func (m Meta) GitMode() bool {
	if m.IsGit == nil {
		return true
	}
	return *m.IsGit
}

// boolPtr is a tiny constructor for the *bool fields above. Local to
// the package because every caller is in projects/ or its tests.
func boolPtr(b bool) *bool { return &b }

// BoolPtr re-exports boolPtr for callers outside the package (cmd/fleet,
// tests) that need to set IsGit explicitly. Kept tiny on purpose.
func BoolPtr(b bool) *bool { return boolPtr(b) }

// SchemaVersion is the value Write emits in Schema for newly minted
// meta.json files. Existing files keep whatever schema they had on
// disk (round-trip preservation) until the operator re-adds the
// project.
const SchemaVersion = "v1"

// ErrNotFound signals "no meta.json on disk for this project". Callers
// that tolerate the absence (the dashboard's future per-project cwd
// consumer) check errors.Is(err, ErrNotFound) and fall through; only
// `fleet project add`-style writers care about presence.
var ErrNotFound = errors.New("projects: meta.json not found")

// metaPath returns ~/.fleet/projects/<project>/meta.json.
//
// Validation matches state.ProjectDir: the project name must pass
// state.ValidateProjectName so a hand-edit can't silently alias to a
// neighboring tree (case-only collisions on macOS/APFS, path-traversal
// via ".."/".").
func metaPath(project string) (string, error) {
	dir, err := state.ProjectDir(project)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Clean(dir), "meta.json"), nil
}

// Read returns the meta.json for project. ENOENT collapses to
// ErrNotFound so callers can errors.Is the absent-file case without
// string-matching the underlying syscall error.
//
// Malformed JSON returns the parse error verbatim — that's a hand-edit
// gone wrong, and the operator should see exactly what's broken
// instead of being told "not found".
func Read(project string) (Meta, error) {
	path, err := metaPath(project)
	if err != nil {
		return Meta{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Meta{}, ErrNotFound
		}
		return Meta{}, fmt.Errorf("read meta.json: %w", err)
	}
	var m Meta
	if err := json.Unmarshal(data, &m); err != nil {
		return Meta{}, fmt.Errorf("parse meta.json: %w", err)
	}
	return m, nil
}

// Write atomically publishes m to ~/.fleet/projects/<project>/meta.json.
//
// Creates the project directory if missing — `fleet project add` is
// the sole writer in v1 and it's responsible for bootstrapping the
// tree. Future writers (e.g. project rename) can rely on the same
// MkdirAll-then-WriteAtomic pattern.
//
// Uses a stable JSON shape (indent + trailing newline) so repeated
// writes don't churn the byte content. The state.WriteAtomic dance
// (tmp + fsync + rename) means a crash mid-write never publishes a
// partial file.
func Write(project string, m Meta) error {
	path, err := metaPath(project)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("write meta.json: mkdir parent: %w", err)
	}
	// Default the schema to the current version so callers can pass a
	// zero Schema and still produce a valid file. Existing-on-disk
	// schema is preserved by going through Read first when the caller
	// wants round-trip semantics.
	if m.Schema == "" {
		m.Schema = SchemaVersion
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("write meta.json: marshal: %w", err)
	}
	data = append(data, '\n')
	if err := state.WriteAtomic(path, data); err != nil {
		return fmt.Errorf("write meta.json: %w", err)
	}
	return nil
}

// TagForPath returns the project tag to register a repo at p as.
// Last-two-segments form (parent-basename) keeps two checkouts with
// the same basename distinct: ~/work/fleet → "work-fleet",
// ~/personal/fleet → "personal-fleet". Without this both would tag
// as "fleet" and fleet-guard's per-project locking would serialize
// unrelated work.
//
// Output is always safe for state.ValidateProjectName: characters
// outside [a-z0-9._-] are replaced with "-", runs of "-" are
// collapsed, and leading/trailing punctuation is stripped. Falls back
// to "fleet" when every segment sanitizes to empty (e.g. path was "/").
//
// Known limitation: sanitization is lossy, so two distinct paths
// whose parent-basename collapses identically after sanitizing share
// a project tag. Example: "~/foo bar/api" and "~/foo-bar/api" both
// → "foo-bar-api". Operators who hit this case can override per-
// dispatch via `fleet dispatch --project <unique>`. A path-derived
// hash suffix would solve it but breaks tag stability across builds
// for in-flight agents, which is a worse practical regression for a
// pre-1.0 project. Revisit at v1.0 with a documented migration path.
//
// Lives here (not in internal/tui) so callers that only care about
// the path → tag mapping don't drag the bubbletea dep tree. The TUI
// re-exports this via internal/tui.ProjectTag.
func TagForPath(p string) string {
	p = filepath.Clean(p)
	base := filepath.Base(p)
	parent := filepath.Base(filepath.Dir(p))
	var raw string
	if parent == "" || parent == "." || parent == string(filepath.Separator) {
		raw = base
	} else {
		raw = parent + "-" + base
	}
	return sanitizeTag(raw)
}

// sanitizeTag forces s into the state.ValidateProjectName allowlist
// ([a-z0-9._-]). Anything else becomes "-"; runs collapse;
// leading/trailing "-" and "." are trimmed because
// state.ValidateProjectName rejects "." and "..". Uppercase is
// lowercased because case-insensitive filesystems would otherwise
// alias case-only variants onto the same project tree.
// Returns "fleet" when the result would otherwise be empty or reserved.
func sanitizeTag(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= '0' && r <= '9',
			r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		default:
			b.WriteRune('-')
		}
	}
	out := b.String()
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	// Trim only "-" and "." at the edges (NOT "_"): underscores are valid
	// interior AND edge chars in ValidateProjectName, so trimming a
	// trailing "_" would alias e.g. "repos-foo_" onto "repos-foo" — a
	// different project (codex iter-4 [P2]). Leading "-" must go because
	// the validator rejects it; "." because "." / ".." are reserved.
	out = strings.Trim(out, "-.")
	// ValidateProjectName rejects leading-hyphen + punctuation-only names
	// (invalid-project-dir-guar-d636). Keep the producer in lockstep: a
	// path that sanitizes to punctuation-only (e.g. "_", "_._") or empty
	// must fall back to "fleet", not emit a tag the validator would reject.
	// The !hasAlnum check (not edge-trimming "_") does the punctuation-only
	// fallback so valid underscore-bearing tags are preserved verbatim.
	if !hasAlnum(out) {
		return "fleet"
	}
	return out
}

// ErrPinLostRace signals that a concurrent writer (`fleet project add`,
// another resolver) changed the project's repo pin between the time the
// caller observed it and the time SetRepoPath re-read it under the lock.
// The caller MUST NOT clobber the winner — it re-resolves from the
// freshly-written meta.json instead of persisting its now-stale value.
type ErrPinLostRace struct {
	// Stored is the repo_path the winning writer left in meta.json.
	Stored string
}

func (e ErrPinLostRace) Error() string {
	return fmt.Sprintf("projects: repo pin changed under us (stored repo_path=%q)", e.Stored)
}

// SetRepoPath persists path (+ fingerprint) into the project's meta.json
// as a race-safe Read-modify-Write under state.LockProjectState — the
// SAME lock `fleet project add` holds across its meta.json write, so the
// two serialize. A naive Write(project, Meta{RepoPath: path}) would
// CLOBBER is_git/added_at/schema (Write marshals the whole struct); this
// re-reads the on-disk Meta inside the lock and updates ONLY RepoPath +
// RepoFingerprint, preserving every other field.
//
// Compare-and-set guard (operator-pin protection):
//
//	prevPath = the meta.RepoPath the caller observed BEFORE deriving
//	           ("" for a from-scratch derive).
//
//	┌─ re-read meta under lock
//	│  stored fp != "" AND stored fp != fingerprint → ErrPinLostRace
//	│      (a different identity was pinned mid-flight)
//	│  stored repo_path != "" AND != prevPath AND != path → ErrPinLostRace
//	│      (path appeared/changed under us — covers the empty-fingerprint
//	│       non-git / shallow case where the fp check can't fire)
//	└─ else: set RepoPath + RepoFingerprint, Write merged struct
//
// On a matching stored fingerprint (cross-machine missing-path re-derive)
// the write proceeds and updates repo_path to the local path.
func SetRepoPath(project, prevPath, path, fingerprint string) error {
	release, err := state.LockProjectState(project)
	if err != nil {
		return err
	}
	defer release()

	m, err := Read(project)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	// Different identity already pinned → lost race, don't clobber.
	if m.RepoFingerprint != "" && m.RepoFingerprint != fingerprint {
		return ErrPinLostRace{Stored: m.RepoPath}
	}
	// repo_path appeared/changed to something other than our prev or our
	// intended path → lost race (catches the empty-fingerprint cases the
	// fingerprint check above can't see).
	if m.RepoPath != "" && m.RepoPath != prevPath && m.RepoPath != path {
		return ErrPinLostRace{Stored: m.RepoPath}
	}
	m.RepoPath = path
	m.RepoFingerprint = fingerprint
	return Write(project, m)
}

// hasAlnum reports whether s contains at least one lowercase letter or
// digit — the minimum ValidateProjectName requires.
func hasAlnum(s string) bool {
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			return true
		}
	}
	return false
}
