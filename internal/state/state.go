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

// ProjectLockPath returns ~/.fleet/projects/.locks/<safe-name>.lock.
//
// Used as a flock target so handoff/spawn flows for the same project
// serialize while different projects proceed in parallel. The .locks
// subdirectory is created by Bootstrap.
//
// Project names are passed through SafeLockComponent so legacy records
// (written before ValidateProjectName existed at the dispatch CLI)
// continue to lock and hand off correctly. SafeLockComponent maps any
// unsafe character to "_"; same-project still serializes (same string
// → same sanitized form), different projects still don't collide
// (collisions only on names that were already aliases of each other).
//
// Validation at the dispatch CLI (ValidateProjectName) prevents NEW
// agents from getting weird names. This function is the safety net
// for OLD agents that exist on disk with weird names.
func ProjectLockPath(project string) (string, error) {
	root, err := Root()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "projects", ".locks", SafeLockComponent(project)+".lock"), nil
}

// SafeLockComponent returns a path-safe transformation of name for use
// as a single filesystem component. Any character outside
// [a-zA-Z0-9_.-] is replaced with "_". Empty input becomes "_default".
//
// Used by ProjectLockPath so legacy records with names like "owner/repo"
// or "gift finder" still serialize their handoffs. The mapping is
// non-injective (collisions possible), but two records that ALREADY
// shared the same project name remain in the same lock partition,
// which is the only invariant ProjectLockPath needs to preserve.
func SafeLockComponent(name string) string {
	if name == "" {
		return "_default"
	}
	out := make([]byte, 0, len(name))
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '-', c == '_', c == '.':
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}
	// Reject "." / ".." which would resolve to the parent dir.
	s := string(out)
	if s == "." || s == ".." {
		return "_" + s
	}
	return s
}

// ValidateProjectName rejects strings that would be unsafe to use as
// a single path component. Allowed: ASCII letters, digits, hyphen,
// underscore, period (but not just "." or ".."). Empty rejected.
//
// Centralized so the dispatch CLI (--project), the project manifest
// loader (future), and lock-file paths all enforce the same rule.
// Operator-supplied "owner/repo"-style names get a clear early error
// instead of a confusing flock-open failure or a silent path traversal.
func ValidateProjectName(name string) error {
	if name == "" {
		return fmt.Errorf("project name must not be empty")
	}
	if name == "." || name == ".." {
		return fmt.Errorf("project name %q reserved", name)
	}
	for _, c := range name {
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '-' || c == '_' || c == '.':
		default:
			return fmt.Errorf("project name %q contains invalid character %q (allowed: letters, digits, _, -, .)", name, c)
		}
	}
	return nil
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
