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
	"time"

	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tui"
)

// Meta is the parsed shape of meta.json. Only the three fields the
// spec requires; future fields land here as omitempty so older readers
// continue to round-trip them through Write.
type Meta struct {
	Schema   string    `json:"schema"`
	RepoPath string    `json:"repo_path"`
	AddedAt  time.Time `json:"added_at"`
}

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

// TagForPath is the canonical project-tag derivation used by the
// `fleet project add` CLI and the TUI's [+] picker. Wraps
// internal/tui.ProjectTag so the two callers stay in sync without
// duplicating sanitization rules.
//
// Re-exporting via this package (rather than asking each consumer to
// import internal/tui) avoids dragging the bubbletea dep tree into
// places that only care about the path → tag mapping. The TUI keeps
// the implementation; this is a thin wrapper.
func TagForPath(p string) string {
	return tui.ProjectTag(p)
}
