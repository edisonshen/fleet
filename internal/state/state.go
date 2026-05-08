// Package state owns ~/.fleet/ directory layout and atomic file writes.
//
// The directory layout is the source of truth for Fleet's runtime state
// (see docs/STATE.md). All writes go through WriteAtomic so readers
// (TUI, CLI, fsnotify) never see torn files.
package state

import (
	"crypto/rand"
	"encoding/hex"
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
	"agents/.locks",
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

// HandoffPath returns ~/.fleet/handoffs/<id>-<YYYYMMDD-HHMMSS>-<rnd>.md.
//
// ts is normalized to UTC so the filename is stable regardless of
// the operator's machine timezone. The trailing 4-hex-char random
// suffix prevents same-second collisions in retry / auto-handoff
// flows that could otherwise overwrite a previous doc and break the
// previous_handoff chain. Format mirrors the checkpoint's intent:
// `<agent-id>-<utc-iso>-<short-uuid>.md`.
func HandoffPath(agentID string, ts time.Time) (string, error) {
	root, err := Root()
	if err != nil {
		return "", err
	}
	stamp := ts.UTC().Format("20060102-150405")
	var rnd [2]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		// Exhaustively rare; fall back to a low-entropy nanosecond
		// suffix so we still produce a unique filename.
		return filepath.Join(root, "handoffs",
			fmt.Sprintf("%s-%s-%04x.md", agentID, stamp, ts.UTC().Nanosecond()&0xffff)), nil
	}
	return filepath.Join(root, "handoffs",
		agentID+"-"+stamp+"-"+hex.EncodeToString(rnd[:])+".md"), nil
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

// AgentLockPath returns ~/.fleet/agents/.locks/<id>.lock.
//
// Per-agent flock target. Used by `fleet handoff` to serialize
// concurrent handoffs of the same agent without blocking handoffs
// of OTHER agents in the same project (which would have happened
// with the broader per-project lock — different agents in the same
// project have no shared state in 4a).
//
// Agent IDs come from agent.NewID (8 hex chars), so SafeLockComponent
// would be a no-op; we still apply it as belt-and-suspenders for any
// future ID format change.
func AgentLockPath(id string) (string, error) {
	root, err := Root()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "agents", ".locks", SafeLockComponent(id)+".lock"), nil
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
// a single path component. Allowed: ASCII lowercase letters, digits,
// hyphen, underscore, period (but not just "." or ".."). Empty rejected.
//
// Lowercase-only is intentional: macOS/APFS is case-insensitive by
// default, so "Foo" and "foo" would alias the same projects/<name>/
// tree and silently corrupt each other's state. Forcing lowercase
// gives one canonical name per inode across all filesystems.
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
	if name == ".locks" {
		return fmt.Errorf("project name %q reserved (collides with projects/.locks/)", name)
	}
	for _, c := range name {
		switch {
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9':
		case c == '-' || c == '_' || c == '.':
		case c >= 'A' && c <= 'Z':
			return fmt.Errorf("project name %q contains uppercase %q (lowercase-only — case-insensitive filesystems alias case variants)", name, c)
		default:
			return fmt.Errorf("project name %q contains invalid character %q (allowed: lowercase letters, digits, _, -, .)", name, c)
		}
	}
	return nil
}

// ValidateSlug rejects task / worker slugs that would alias on disk.
//
// Allowed: ASCII lowercase letters, digits, hyphen, underscore, period
// — no path separators, spaces, uppercase, or other runes that
// SafeLockComponent would map to "_". An invalid slug would otherwise
// let `feature/a` and `feature_a` collide on the workers/<x>/ tree.
//
// Lowercase-only matches ValidateProjectName's reasoning: macOS/APFS
// is case-insensitive by default, so two slugs differing only in case
// would alias the same workers/<slug>/, archive, and worktrees paths
// while tasks.Read treats them as distinct entries — silent state
// corruption.
//
// Empty rejected; "." and ".." rejected (parent-dir traversal).
// "archive" rejected because workers/archive/ is the reserved holding
// pen for archived worker dirs — a worker with slug=archive would
// alias that directory and become invisible to ListActive + un-
// archivable (rename into self).
func ValidateSlug(slug string) error {
	if slug == "" {
		return fmt.Errorf("slug must not be empty")
	}
	if slug == "." || slug == ".." {
		return fmt.Errorf("slug %q reserved", slug)
	}
	if slug == "archive" {
		return fmt.Errorf("slug %q reserved (collides with workers/archive/)", slug)
	}
	for _, c := range slug {
		switch {
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9':
		case c == '-' || c == '_' || c == '.':
		case c >= 'A' && c <= 'Z':
			return fmt.Errorf("slug %q contains uppercase %q (lowercase-only — case-insensitive filesystems alias case variants)", slug, c)
		default:
			return fmt.Errorf("slug %q contains invalid character %q (allowed: lowercase letters, digits, _, -, .)", slug, c)
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

// ProjectDir returns ~/.fleet/projects/<safe-name>/.
//
// v0.2 per-project coordinator state lives here: tasks.md, learnings.md,
// per-project standards.md, workers/<slug>/, worktrees/<slug>/, and the
// .locks/ subdirectory holding state.lock + coordinator.lock.
//
// The trailing slash is intentional — callers join with `filepath.Join`
// or treat the result as a directory prefix.
//
// SAFE-NAME RULE: name must be empty (resolves to "_default") or pass
// the same character set as ValidateProjectName ([a-zA-Z0-9_.-], no
// "." or ".."). The v0.1 ProjectLockPath path used SafeLockComponent
// to map unsafe inputs to "_" so legacy LOCK files kept working —
// that's tolerable for a zero-byte sentinel where collisions are
// harmless. For ProjectDir the same mapping would silently alias
// `owner/repo` and `owner_repo` onto the SAME tasks.md / learnings.md
// / workers/ tree — corrupting both projects' state. We reject
// invalid names here instead, and callers must use safe names.
//
// This is a path-only helper; it does NOT create the directory. First
// writer (e.g. tasks.Write) is responsible for MkdirAll.
func ProjectDir(name string) (string, error) {
	root, err := Root()
	if err != nil {
		return "", err
	}
	if name == "" {
		// Backwards-compat with empty input: use the same fallback
		// as SafeLockComponent so a zero-value caller doesn't break.
		return filepath.Join(root, "projects", "_default") + string(filepath.Separator), nil
	}
	if err := ValidateProjectName(name); err != nil {
		return "", fmt.Errorf("ProjectDir: %w", err)
	}
	return filepath.Join(root, "projects", name) + string(filepath.Separator), nil
}

// ProjectStateLockPath returns ~/.fleet/projects/<safe-name>/.locks/state.lock.
//
// The Q1-locked v0.2 single state-lock — serializes writes to tasks.md /
// tasks-archive.md / learnings.md / learnings-archive.md within one
// project. Distinct from the v0.1 ProjectLockPath (which serializes
// dispatch + handoff under projects/.locks/<name>.lock); the two lock
// trees do not interact.
func ProjectStateLockPath(name string) (string, error) {
	dir, err := ProjectDir(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, ".locks", "state.lock"), nil
}

// EnsureProjectInitialized creates ~/.fleet/projects/<safe-name>/.locks/
// (and the parent <safe-name>/ dir) when missing. Idempotent — calling
// twice is a no-op once the directories exist.
//
// The TUI's project-row [a] auto-spawn flow uses this as a pre-dispatch
// step so the spawned coord skill's first tick can write coord-state.json
// and acquire coordinator.lock without racing on a missing parent dir.
// Without this step, fresh projects landed agents on disk with no lock
// body ever published — the dashboard couldn't bind the agent to the
// project, and repeated [a] presses piled up zombies (issue #63).
//
// Validation matches ProjectDir's rule (ValidateProjectName); empty
// name resolves to "_default" via ProjectDir's backwards-compat branch.
//
// Returns the project root dir (no trailing separator) on success.
func EnsureProjectInitialized(name string) (string, error) {
	dir, err := ProjectDir(name)
	if err != nil {
		return "", err
	}
	// ProjectDir appends a trailing separator; trim before MkdirAll
	// so the .locks subdir resolves cleanly (filepath.Join already
	// tolerates trailing separators, but stripping keeps the returned
	// path symmetric with ProjectDir's other consumers).
	dir = filepath.Clean(dir)
	if err := os.MkdirAll(filepath.Join(dir, ".locks"), 0o755); err != nil {
		return "", fmt.Errorf("EnsureProjectInitialized: mkdir .locks: %w", err)
	}
	return dir, nil
}

// CoordinatorLockPath returns ~/.fleet/projects/<safe-name>/.locks/coordinator.lock.
//
// Held by the running coordinator agent for the lifetime of one tick;
// a second coord that finds it taken logs and exits cleanly. Sibling of
// state.lock under .locks/ but never combined with it (different
// invariants, different acquisition disciplines).
func CoordinatorLockPath(name string) (string, error) {
	dir, err := ProjectDir(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, ".locks", "coordinator.lock"), nil
}

// WorkerDir returns ~/.fleet/projects/<project>/workers/<slug>/.
//
// Each worker (a `claude --print` subprocess launched by the coord)
// owns one directory containing state.json + output.log. Coordinator
// watches state.json via fsnotify; operator inspects via `fleet peek`.
//
// Slug must pass ValidateSlug — only [a-zA-Z0-9_.-] allowed, no
// "." or "..". Unsafe input is rejected rather than mapped via
// SafeLockComponent because that mapping would alias `feature/a` and
// `feature_a` onto the same workers/<x>/ directory.
func WorkerDir(project, slug string) (string, error) {
	dir, err := ProjectDir(project)
	if err != nil {
		return "", err
	}
	if err := ValidateSlug(slug); err != nil {
		return "", fmt.Errorf("WorkerDir: %w", err)
	}
	return filepath.Join(dir, "workers", slug) + string(filepath.Separator), nil
}

// WorkerArchiveDir returns
// ~/.fleet/projects/<project>/workers/archive/<slug>-<ts>/.
//
// Workers archive on phase=done; auto-prune at 7d. ts is a UTC stamp
// produced by the caller (e.g. ts.UTC().Format("20060102-150405")) so
// archive paths are stable across timezones.
//
// Slug is validated (same rule as WorkerDir).
func WorkerArchiveDir(project, slug, ts string) (string, error) {
	dir, err := ProjectDir(project)
	if err != nil {
		return "", err
	}
	if err := ValidateSlug(slug); err != nil {
		return "", fmt.Errorf("WorkerArchiveDir: %w", err)
	}
	return filepath.Join(dir, "workers", "archive", slug+"-"+ts) + string(filepath.Separator), nil
}

// WorktreePath returns ~/.fleet/projects/<project>/worktrees/<slug>/.
//
// Used when the coord runs in cap > 1 mode (parallel workers). Created
// via `git worktree add` and removed via `git worktree remove --force`
// on task archive. Single-worker mode (default) reuses the repo root
// instead.
//
// Slug is validated (same rule as WorkerDir).
func WorktreePath(project, slug string) (string, error) {
	dir, err := ProjectDir(project)
	if err != nil {
		return "", err
	}
	if err := ValidateSlug(slug); err != nil {
		return "", fmt.Errorf("WorktreePath: %w", err)
	}
	return filepath.Join(dir, "worktrees", slug) + string(filepath.Separator), nil
}
