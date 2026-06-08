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
	"strings"
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
	"drain-runs",
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

// DrainRunsDir returns ~/.fleet/drain-runs/.
//
// Each live `fleet drain` writes a tiny run-record <pid>.json here on
// start, heartbeats it while running, and deletes it on clean exit. A
// drain that stops heartbeating past a TTL is provably wedged — the
// signal the gc KindDrainProcs classifier keys off (vs. inferring
// wedged-ness from raw `ps` "sleeping" state, which can kill a
// legitimate long recovery). One writer per record (the drain itself),
// one reader (fleet gc).
func DrainRunsDir() (string, error) {
	root, err := Root()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "drain-runs"), nil
}

// HandoffDir returns ~/.fleet/handoffs/.
func HandoffDir() (string, error) {
	root, err := Root()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "handoffs"), nil
}

// IncidentsDir returns ~/.fleet/incidents/.
//
// The macOS memorystatus / jetsam observer in skills/fleet-guard
// writes structured incident JSON files here when the kernel kills a
// claude process under memory pressure. The TUI dashboard scans this
// directory to render a per-project "killed by memorystatus" badge.
// Same shape as HandoffDir / AgentDir — one writer (the skill), many
// readers (Go-side TUI scan + Python-side debug helpers).
func IncidentsDir() (string, error) {
	root, err := Root()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "incidents"), nil
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
	// Reject names that start with "-": a CLI flag-misparse (e.g.
	// `fleet tasks list --project=--project` or `--project=-x`) can
	// capture a flag-shaped token as the project NAME. ValidateProjectName
	// is the single chokepoint every --project surface (dispatch, tasks,
	// project, tui actions) funnels through, so rejecting the leading "-"
	// here closes the hole everywhere. A literal `--project` directory
	// otherwise sorts before letters and hijacks the dashboard title /
	// project count (the bug this guard fixes). No legitimate project
	// name begins with a hyphen.
	if strings.HasPrefix(name, "-") {
		return fmt.Errorf("project name %q must not start with %q (looks like a CLI flag — likely a `--project` flag-misparse)", name, "-")
	}
	hasAlnum := false
	for _, c := range name {
		switch {
		case c >= 'a' && c <= 'z':
			hasAlnum = true
		case c >= '0' && c <= '9':
			hasAlnum = true
		case c == '-' || c == '_' || c == '.':
		case c >= 'A' && c <= 'Z':
			return fmt.Errorf("project name %q contains uppercase %q (lowercase-only — case-insensitive filesystems alias case variants)", name, c)
		default:
			return fmt.Errorf("project name %q contains invalid character %q (allowed: lowercase letters, digits, _, -, .)", name, c)
		}
	}
	// Punctuation-only names ("--", "-_-", "..." would already be caught
	// by the leading-"-" / reserved guards above, but "_._" / "._" slip
	// through the per-char loop) carry no identity and alias confusingly
	// on disk. Require at least one alphanumeric rune.
	if !hasAlnum {
		return fmt.Errorf("project name %q has no alphanumeric character (allowed: lowercase letters, digits, _, -, .)", name)
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

// WriteAtomic publishes data to path via .tmp + fsync + rename +
// parent-dir-fsync. Implements docs/STATE.md A1 (Atomic file publish).
//
// Pattern:
//
//	write to <path>.tmp.<pid> → fsync(tmp) → close → rename →
//	  open(parent) → fsync(parent) → close
//
// fsnotify on the reader side fires CREATE the instant the file
// exists; rename(2) is atomic on POSIX same-fs, so readers never
// observe a partial write.
//
// The trailing parent-dir fsync ensures the rename is durable on
// POSIX. Without it, a crash between rename(2) returning and the
// containing directory's dentry being written to disk can leave the
// renamed file invisible after reboot — the data is on disk in an
// orphaned inode but the directory entry pointing at it isn't. For
// coord-spawn markers and other AtomicCoordSwap commit points,
// "looks committed but reverts on crash" silently breaks the swap
// invariant.
//
// fsyncParent is a package-level seam so tests can detect the call
// without relying on syscall tracing. Production points at the
// stdlib's fsync; the test wrapper records every (path, error) pair
// it sees.
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
	// Parent-dir fsync makes the rename durable across a crash. On
	// AtomicCoordSwap's marker-rename commit point this is the
	// difference between "marker flipped, NEW owns" surviving a
	// kernel panic versus a reboot reverting to OLD.
	//
	// Critical: this fsync runs AFTER rename. A failure here means
	// the rename ALREADY succeeded on disk — `path` resolves to the
	// new content from this process's perspective and from any other
	// reader's perspective. We CANNOT propagate the error as a
	// regular failure: existing callers (spawn.Spawn, AtomicCoordSwap,
	// agent.Write, etc.) treat a WriteAtomic error as "nothing
	// changed; roll back" — but in this case the rename DID change
	// observable state, and rolling back (e.g., killing the just-
	// spawned NEW agent because we couldn't fsync the parent dir)
	// would leave on-disk state inconsistent with the rolled-back
	// in-memory state. Logging via fsyncFailureLog (test-overridable)
	// surfaces the durability gap without breaking caller semantics.
	// The cost of a parent-fsync miss is "rename invisible after a
	// kernel panic that reboots before the dentry flushes" — rare,
	// and the operator's recovery in that window is the same as for
	// any other pre-fsync crash (manual triage via `fleet status` +
	// `fleet rm`).
	//
	// Codex review iter-1 [P1]: returning the parent-fsync error from
	// WriteAtomic let callers treat it as "rename never happened" and
	// trigger rollback paths against committed state. Fixed by
	// switching to best-effort log + no error propagation.
	if perr := fsyncParent(filepath.Dir(path)); perr != nil {
		fsyncFailureLog(path, perr)
	}
	return nil
}

// fsyncParent is the package-level seam for the parent-dir fsync.
// Production implementation opens the directory read-only and calls
// Sync on it. Tests override this to record invocations without
// needing to spy on the syscall layer.
var fsyncParent = func(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	syncErr := d.Sync()
	closeErr := d.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

// fsyncFailureLog records a post-rename parent-dir fsync failure
// without propagating it as a WriteAtomic error. Default writes a
// note to stderr; tests can override to capture the call without
// noise.
//
// The failure means the rename committed in-memory and observably,
// but the dentry write to disk might be deferred until the next
// fsync of any file in that directory. A crash before that flush
// can leave the renamed file invisible after reboot. Operator
// recovery for that narrow window is the same as for any pre-fsync
// crash — `fleet status` + `fleet rm` for stale records, marker
// re-write if it disappeared.
var fsyncFailureLog = func(path string, err error) {
	_, _ = fmt.Fprintf(os.Stderr,
		"warning: post-rename parent-dir fsync for %s failed: %v (rename is committed in-memory but may not be durable across a crash)\n",
		path, err)
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

// ProjectWorktreeGCLockPath resolves
// ~/.fleet/projects/<project>/.locks/worktree-gc.lock — the
// project-scoped flock taken around the `fleet gc --kinds=worktrees`
// scan → re-check → remove window (DESIGN-coord-worktree-lifecycle §
// Concurrency) so a coord-invoked run and an operator-invoked run can't
// race the same removal. Distinct from state.lock (tasks.md writes) so a
// long GC scan doesn't block task mutations.
func ProjectWorktreeGCLockPath(name string) (string, error) {
	dir, err := ProjectDir(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, ".locks", "worktree-gc.lock"), nil
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

// CoordSpawnMarkerPath returns
// ~/.fleet/projects/<safe-name>/.locks/coord-spawn-marker.
//
// The TUI writes this file IMMEDIATELY AFTER `fleet dispatch` returns
// the agent ID for a coord auto-spawn. The dashboard's task_id
// fallback signal (rows.go:findCoordByTaskID) gates promotion on the
// marker file's content matching the candidate agent's ID — so an
// operator who shells out `fleet dispatch coord-<project> --project
// <project> --coord-spawn` can write an agent record but cannot make
// the dashboard treat it as the project's coord (they'd also have to
// write this marker with the right ID).
//
// Marker is overwritten on each fresh coord-spawn (idempotent for
// retries). It is NOT cleaned up on coord exit; the freshness gate
// (alive session + project-tree exists) prevents stale markers from
// hijacking a future coord. The marker can outlive its referent
// agent, but the alive-gate keeps that from rendering as a coord.
func CoordSpawnMarkerPath(name string) (string, error) {
	dir, err := ProjectDir(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Clean(dir), ".locks", "coord-spawn-marker"), nil
}

// WriteCoordSpawnMarker atomically writes agentID into the coord-spawn
// marker for projectName. The TUI calls this BEFORE shelling out to
// `fleet dispatch` so the dashboard's task_id fallback can verify the
// record originated from the TUI auto-spawn path.
//
// Idempotent: a second call overwrites the previous content (atomic
// rename via WriteAtomic). Caller must have already run
// EnsureProjectInitialized so the .locks/ parent exists.
func WriteCoordSpawnMarker(projectName, agentID string) error {
	path, err := CoordSpawnMarkerPath(projectName)
	if err != nil {
		return fmt.Errorf("WriteCoordSpawnMarker: %w", err)
	}
	return WriteAtomic(path, []byte(agentID+"\n"))
}

// ReadCoordSpawnMarker returns the agent ID stored in the coord-spawn
// marker for projectName, or "" when the marker is absent / unreadable
// / malformed. Used by the dashboard's task_id fallback to validate
// that a candidate coord record was spawned via the TUI flow (rather
// than hand-edited / CLI-spoofed).
//
// Reads only the first line and trims whitespace, so CRLF or trailing
// blank lines from a hand-edit don't trip the comparison. Returns ""
// silently rather than err — this is a best-effort gate, not a
// load-bearing security boundary.
func ReadCoordSpawnMarker(projectName string) string {
	path, err := CoordSpawnMarkerPath(projectName)
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	// First line, whitespace-trimmed. SplitN+TrimSpace handles CRLF /
	// trailing blank lines / leading-or-trailing spaces from a hand-
	// edit without reimplementing the byte loop.
	first := strings.SplitN(string(data), "\n", 2)[0]
	return strings.TrimSpace(first)
}

// RemoveCoordSpawnMarker deletes the coord-spawn marker for projectName.
// Returns nil when the marker is already absent (idempotent — the
// self-heal path in the TUI dashboard renderer is best-effort and must
// not error out when concurrent dashboard ticks race to clear the same
// marker).
//
// Used by the TUI's self-heal path (issue #96 gap 1): when the spawn
// marker still exists but the tmux session for the agent_id stored in
// the marker is gone, the dashboard concludes the spawn died and
// removes the stale marker so the next render flips Stuck → Idle. Any
// non-ENOENT removal error is propagated so the caller can surface a
// flash; ENOENT collapses to nil so a parallel remover doesn't trip
// the warning.
func RemoveCoordSpawnMarker(projectName string) error {
	path, err := CoordSpawnMarkerPath(projectName)
	if err != nil {
		return fmt.Errorf("RemoveCoordSpawnMarker: %w", err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("RemoveCoordSpawnMarker: %w", err)
	}
	return nil
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
