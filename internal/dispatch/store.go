package dispatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/edisonshen/fleet/internal/state"
)

// ErrJournalNotFound is returned when a journal does not exist on
// disk. Callers can distinguish "absent" from "unreadable" without
// stringly-typed error matching.
var ErrJournalNotFound = errors.New("journal not found")

// DispatchesDir returns ~/.fleet/dispatches/.
//
// The directory is created lazily on first write (parent dirs are
// MkdirAll'd in writeJournal). Bootstrap in internal/state does NOT
// include this yet — adding it there would require schema discipline
// every release, and the lazy mkdir is friction-free for v0.11.
func DispatchesDir() (string, error) {
	root, err := state.Root()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "dispatches"), nil
}

// JournalPath returns ~/.fleet/dispatches/<id>.json.
func JournalPath(id DispatchID) (string, error) {
	dir, err := DispatchesDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, id.String()+".json"), nil
}

// readJournal loads a journal from disk. Returns ErrJournalNotFound
// (wrapped) when the file is absent so callers can distinguish that
// from a parse / permission error.
func readJournal(id DispatchID) (*Journal, error) {
	path, err := JournalPath(id)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("journal %s: %w", id, ErrJournalNotFound)
		}
		return nil, fmt.Errorf("read journal %s: %w", id, err)
	}
	var j Journal
	if err := json.Unmarshal(data, &j); err != nil {
		return nil, fmt.Errorf("parse journal %s: %w", id, err)
	}
	return &j, nil
}

// writeJournal publishes the journal to disk via state.WriteAtomic
// (tmp+rename, fsync, parent-fsync). The dispatches/ subdir is created
// on first write so callers don't have to.
func writeJournal(j *Journal) error {
	path, err := JournalPath(j.ID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir dispatches: %w", err)
	}
	data, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal journal %s: %w", j.ID, err)
	}
	data = append(data, '\n')
	return state.WriteAtomic(path, data)
}

// LoadJournal is the public reader. Returns ErrJournalNotFound when
// absent.
func LoadJournal(id DispatchID) (*Journal, error) {
	return readJournal(id)
}

// SaveJournal is the public writer. Stamps UpdatedAt to "now-ish" via
// nowFunc (overridable for tests).
func SaveJournal(j *Journal) error {
	j.UpdatedAt = nowFunc().UTC()
	return writeJournal(j)
}

// nowFunc is the package-level clock seam. Production points at
// time.Now; tests override to inject a stable instant.
//
// Replaced via the test helper setNow() — never assigned directly from
// production code.
var nowFunc = defaultNow
