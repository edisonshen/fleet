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
	"time"
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

// AgentArchivePath returns ~/.fleet/agents/archive/<id>.json.
//
// Records move here when the agent crashes or hands off; readers
// looking for *live* agents iterate AgentDir() and skip subdirs.
func AgentArchivePath(id string) (string, error) {
	root, err := Root()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "agents", "archive", id+".json"), nil
}

// HandoffDir returns ~/.fleet/handoffs/.
func HandoffDir() (string, error) {
	root, err := Root()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "handoffs"), nil
}

// HandoffPath returns ~/.fleet/handoffs/<id>-<YYYYMMDD-HHMMSS>.md.
//
// ts is normalized to UTC so the filename is stable regardless of
// the operator's machine timezone. Format mirrors the example in
// docs/DESIGN.md "State directory" (a1-20260415-143200.md).
func HandoffPath(agentID string, ts time.Time) (string, error) {
	root, err := Root()
	if err != nil {
		return "", err
	}
	stamp := ts.UTC().Format("20060102-150405")
	return filepath.Join(root, "handoffs", agentID+"-"+stamp+".md"), nil
}

// QueueDir returns ~/.fleet/queue/.
func QueueDir() (string, error) {
	root, err := Root()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "queue"), nil
}

// QueuePath returns ~/.fleet/queue/<name>.json.
//
// name is the queue file's logical identifier; the queue package
// owns the naming convention (e.g., "spawn-fresh-a1b2c3d4",
// "handoff-a1b2c3d4"). Centralizing the .json extension here keeps
// readers (fsnotify filters, list helpers) consistent.
func QueuePath(name string) (string, error) {
	root, err := Root()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "queue", name+".json"), nil
}

// ProjectLockPath returns ~/.fleet/projects/.locks/<project>.lock.
//
// Used as a flock target so handoff/spawn flows for the same project
// serialize while different projects proceed in parallel. The .locks
// subdirectory is created by Bootstrap.
func ProjectLockPath(project string) (string, error) {
	root, err := Root()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "projects", ".locks", project+".lock"), nil
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
