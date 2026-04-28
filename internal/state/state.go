// Package state owns ~/.fleet/ directory layout and atomic file writes.
//
// The directory layout is the source of truth for Fleet's runtime state
// (see docs/STATE.md). All writes go through WriteAtomic so readers
// (TUI, CLI, fsnotify) never see torn files.
package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// Subdirectories under ~/.fleet/ that Bootstrap creates.
// Mirrors the layout in docs/STATE.md.
var subdirs = []string{
	"agents",
	"agents/archive",
	"projects",
	"projects/.locks",
	"handoffs",
	"inbox",
	"inbox/archive",
	"progress",
	"queue",
	"logs",
}

// Root returns the absolute path to ~/.fleet/.
//
// Honors FLEET_HOME if set (useful for tests).
func Root() (string, error) {
	if r := os.Getenv("FLEET_HOME"); r != "" {
		return r, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home dir: %w", err)
	}
	return filepath.Join(home, ".fleet"), nil
}

// Bootstrap creates ~/.fleet/ and its subdirectories if missing.
// Idempotent — safe to call on every command invocation.
func Bootstrap() (string, error) {
	root, err := Root()
	if err != nil {
		return "", err
	}
	for _, sub := range subdirs {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o755); err != nil {
			return "", fmt.Errorf("mkdir %s: %w", sub, err)
		}
	}
	return root, nil
}

// AgentPath returns ~/.fleet/agents/<id>.json.
func AgentPath(id string) (string, error) {
	root, err := Root()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "agents", id+".json"), nil
}

// AgentDir returns ~/.fleet/agents/.
func AgentDir() (string, error) {
	root, err := Root()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "agents"), nil
}

// WriteAtomic publishes data to path via .tmp + fsync + rename.
// Implements docs/STATE.md A1 (Atomic file publish).
//
// Pattern:
//
//	write to <path>.tmp.<pid> → fsync → close → rename to <path>
//
// fsnotify on the reader side fires CREATE the instant the file
// exists; rename(2) is atomic on POSIX same-fs, so readers never
// observe a partial write.
func WriteAtomic(path string, data []byte) (err error) {
	tmp := path + ".tmp." + strconv.Itoa(os.Getpid())
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create tmp: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	if _, werr := f.Write(data); werr != nil {
		_ = f.Close()
		return fmt.Errorf("write tmp: %w", werr)
	}
	if serr := f.Sync(); serr != nil {
		_ = f.Close()
		return fmt.Errorf("fsync tmp: %w", serr)
	}
	if cerr := f.Close(); cerr != nil {
		return fmt.Errorf("close tmp: %w", cerr)
	}
	if rerr := os.Rename(tmp, path); rerr != nil {
		return fmt.Errorf("rename tmp: %w", rerr)
	}
	return nil
}

// ErrNotFound is returned by readers when an expected path is absent.
var ErrNotFound = errors.New("not found")
