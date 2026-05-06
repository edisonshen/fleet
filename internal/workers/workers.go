// Package workers owns per-worker state.json files, alive-checking,
// archival, and pruning.
//
// Workers are NOT Fleet agents — they run as `claude --print`
// subprocesses launched by the per-project coordinator. Each worker
// owns a directory:
//
//	~/.fleet/projects/<project>/workers/<slug>/
//	  state.json    — phase + timestamps; atomic-rewritten on every change
//	  output.log    — captured stdout/stderr (OS appends; this package
//	                  doesn't read or write it)
//
// Coord watches state.json via fsnotify; operator inspects via
// `fleet peek`. On phase=done the directory is archived to
// workers/archive/<slug>-<UTC-stamp>/ and the active dir is removed
// atomically (rename). 7d default prune cleans archives.
//
// Validation (per ENG §6.2):
//   - phase=done    requires non-empty PRURL
//   - phase=blocked requires non-empty BlockedReason
//   - validation fires at WriteState time; ReadState is permissive
//     (recovers as much as possible from a malformed file).
package workers

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/edisonshen/fleet/internal/state"
)

// Phase is the worker's current step in its TDD → review → push pipe.
// Values are exact strings written into state.json; coord skill
// matches on these.
type Phase string

const (
	PhaseStarting     Phase = "starting"
	PhaseBranch       Phase = "branch"
	PhaseTDDRed       Phase = "tdd-red"
	PhaseTDDGreen     Phase = "tdd-green"
	PhaseTDDRefactor  Phase = "tdd-refactor"
	PhaseReviewClaude Phase = "review-claude"
	PhaseReviewCodex  Phase = "review-codex"
	PhasePush         Phase = "push"
	PhaseDone         Phase = "done"
	PhaseBlocked      Phase = "blocked"
	PhaseFailed       Phase = "failed"
)

func validPhase(p Phase) bool {
	switch p {
	case PhaseStarting, PhaseBranch, PhaseTDDRed, PhaseTDDGreen,
		PhaseTDDRefactor, PhaseReviewClaude, PhaseReviewCodex,
		PhasePush, PhaseDone, PhaseBlocked, PhaseFailed:
		return true
	}
	return false
}

// State is the on-disk shape of state.json.
type State struct {
	Slug            string    `json:"slug"`
	Project         string    `json:"project"`
	Phase           Phase     `json:"phase"`
	PhasesCompleted []Phase   `json:"phases_completed"`
	StartedAt       time.Time `json:"started_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	PID             int       `json:"pid"`
	PRURL           string    `json:"pr_url,omitempty"`
	BlockedReason   string    `json:"blocked_reason,omitempty"`
	Exit            *int      `json:"exit,omitempty"`
}

// Errors.
var (
	ErrNotFound         = errors.New("worker state.json not found")
	ErrInvalidState     = errors.New("invalid worker state")
	ErrPhaseRequiresPR  = errors.New("phase=done requires pr_url")
	ErrPhaseRequiresWhy = errors.New("phase=blocked requires blocked_reason")
	ErrInvalidPhase     = errors.New("invalid phase")
	ErrInvalidSlug      = errors.New("invalid worker slug")
	ErrPreconditionLive = errors.New("cannot archive live worker")
)

// updateMu serializes UpdateState calls within one process per
// (project, slug) pair. Cross-process serialization is provided by
// the per-state-file flock acquired inside UpdateState.
var updateMu sync.Map // key="<project>/<slug>" → *sync.Mutex

// ReadState loads the state.json file. ENOENT returns ErrNotFound.
// Read is permissive — does NOT validate phase requirements.
func ReadState(project, slug string) (*State, error) {
	if slug == "" {
		return nil, fmt.Errorf("%w: empty slug", ErrInvalidSlug)
	}
	path, err := stateJSONPath(project, slug)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("read state.json: %w", err)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("unmarshal state.json: %w", err)
	}
	return &s, nil
}

// WriteState atomically publishes s to state.json. Validates phase
// requirements (done→pr_url, blocked→blocked_reason) and bumps
// UpdatedAt.
//
// Caller is responsible for holding any cross-process lock if
// multiple writers may race; UpdateState wraps this with an flock.
func WriteState(project, slug string, s *State) error {
	if s == nil {
		return fmt.Errorf("%w: nil state", ErrInvalidState)
	}
	if !validPhase(s.Phase) {
		return fmt.Errorf("%w: %q", ErrInvalidPhase, s.Phase)
	}
	if s.Phase == PhaseDone && strings.TrimSpace(s.PRURL) == "" {
		return ErrPhaseRequiresPR
	}
	if s.Phase == PhaseBlocked && strings.TrimSpace(s.BlockedReason) == "" {
		return ErrPhaseRequiresWhy
	}
	// Enforce consistency between on-disk slug/project and the
	// state object. A buggy caller passing slug "a" with s.Slug "b"
	// would otherwise persist a mismatched record that breaks
	// reconciliation; reject the write rather than backfill it.
	if s.Slug != "" && s.Slug != slug {
		return fmt.Errorf("%w: slug mismatch: state=%q path=%q", ErrInvalidState, s.Slug, slug)
	}
	if s.Project != "" && s.Project != project {
		return fmt.Errorf("%w: project mismatch: state=%q path=%q", ErrInvalidState, s.Project, project)
	}
	if s.Slug == "" {
		s.Slug = slug
	}
	if s.Project == "" {
		s.Project = project
	}
	s.UpdatedAt = time.Now().UTC()

	path, err := stateJSONPath(project, slug)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir worker dir: %w", err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	return state.WriteAtomic(path, data)
}

// UpdateState reads the current state, applies mutate, validates, and
// atomically publishes. Both an in-process mutex and a per-worker
// flock serialize concurrent updaters.
//
// The flock target is workers/<slug>/.update.lock — separate from
// state.json so the lock file isn't itself the data and is safe to
// leave persistent.
func UpdateState(project, slug string, mutate func(*State)) error {
	if mutate == nil {
		return fmt.Errorf("%w: nil mutate fn", ErrInvalidState)
	}
	// In-process mutex first (cheap, no fs op).
	key := project + "/" + slug
	muIface, _ := updateMu.LoadOrStore(key, &sync.Mutex{})
	mu := muIface.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()

	dir, err := state.WorkerDir(project, slug)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir worker dir: %w", err)
	}
	lockPath := filepath.Join(dir, ".update.lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("open lock: %w", err)
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("flock: %w", err)
	}
	// Implicit unlock on Close.

	cur, err := ReadState(project, slug)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if cur == nil {
		// Bootstrap a minimal state for first-time writers.
		cur = &State{
			Slug:      slug,
			Project:   project,
			Phase:     PhaseStarting,
			StartedAt: time.Now().UTC(),
		}
	}
	mutate(cur)
	return WriteState(project, slug, cur)
}

// IsAlive returns true if the OS process with pid is alive. Uses
// kill(pid, 0) which sends no signal but checks process existence
// + permissions. Treat permission-denied as "alive" — we lack the
// permission to signal it but it does exist.
func IsAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	// EPERM means the process exists but we can't signal it; the
	// caller's question is "is it alive", and the answer is yes.
	if errors.Is(err, syscall.EPERM) {
		return true
	}
	return false
}

// Archive moves workers/<slug>/ → workers/archive/<slug>-<UTC-stamp>/.
// The rename is atomic on POSIX same-fs. Stamp uses YYYYMMDD-HHMMSS.
// If the source dir doesn't exist, returns nil (idempotent).
//
// PRECONDITION: the worker process must be exited AND its state must
// have reached a terminal phase (done | blocked | failed). Even a
// worker that wrote phase=done may emit one more UpdateState (e.g.
// setting `exit`); if we rename between those two writes, the
// updater's subsequent WriteState recreates workers/<slug>/state.json
// at the original path, leaving stray active + archive dirs. To
// close that race we (1) acquire the same in-process mutex +
// per-worker .update.lock that UpdateState uses, and (2) re-verify
// the precondition under that lock before renaming.
//
// If state.json is missing entirely, Archive proceeds — we have no
// liveness signal and assume the caller knows what they're doing.
// If state.json is present and either condition fails, returns
// ErrPreconditionLive (typed so coord can retry next tick).
func Archive(project, slug string) error {
	src, err := state.WorkerDir(project, slug)
	if err != nil {
		return err
	}
	if _, statErr := os.Stat(src); os.IsNotExist(statErr) {
		return nil
	} else if statErr != nil {
		return fmt.Errorf("stat worker dir: %w", statErr)
	}

	// Coordinate with UpdateState: take the same in-process mutex
	// AND the per-worker .update.lock so an in-flight UpdateState
	// can't race the rename. The flock+mutex pair mirrors
	// UpdateState's discipline; closing the fd at scope exit
	// releases the kernel lock.
	key := project + "/" + slug
	muIface, _ := updateMu.LoadOrStore(key, &sync.Mutex{})
	mu := muIface.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()

	lockPath := filepath.Join(src, ".update.lock")
	lockF, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("open update lock for archive: %w", err)
	}
	defer func() { _ = lockF.Close() }()
	if err := syscall.Flock(int(lockF.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("flock update lock: %w", err)
	}

	// Re-verify preconditions under the lock — the predecessor's
	// UpdateState may have just landed and bumped phase/pid.
	cur, rerr := ReadState(project, slug)
	switch {
	case rerr == nil:
		if !isTerminalPhase(cur.Phase) {
			return fmt.Errorf("%w: phase=%q (must be done|blocked|failed)", ErrPreconditionLive, cur.Phase)
		}
		// Even at terminal phase, the process may still be alive
		// for the brief window between writing phase=done and
		// exit. Re-check.
		if cur.PID > 0 && IsAlive(cur.PID) {
			return fmt.Errorf("%w: phase=%q but pid=%d still alive", ErrPreconditionLive, cur.Phase, cur.PID)
		}
	case errors.Is(rerr, ErrNotFound):
		// No state file — proceed (caller's responsibility).
	default:
		return fmt.Errorf("read state for archive precondition: %w", rerr)
	}

	// Close the lock fd before renaming. macOS allows rename on a
	// dir whose contents are open, but the fd would dangle inside
	// the archived path; we don't need it past this point because
	// the in-process mutex still serializes us against any future
	// UpdateState call (which would itself reopen the lock at the
	// new path and find no work to do).
	_ = lockF.Close()

	stamp := time.Now().UTC().Format("20060102-150405")
	dst, err := state.WorkerArchiveDir(project, slug, stamp)
	if err != nil {
		return err
	}
	// Both helpers return paths with a trailing separator; rename
	// works on either form. Strip trailing separator for clarity.
	src = strings.TrimSuffix(src, string(filepath.Separator))
	dst = strings.TrimSuffix(dst, string(filepath.Separator))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("mkdir archive parent: %w", err)
	}
	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("rename %s → %s: %w", src, dst, err)
	}
	return nil
}

// isTerminalPhase reports whether p represents a worker that has
// stopped writing state.json. Coord uses this to decide when it's
// safe to archive a worker dir.
func isTerminalPhase(p Phase) bool {
	switch p {
	case PhaseDone, PhaseBlocked, PhaseFailed:
		return true
	}
	return false
}

// PruneArchive removes archive directories older than olderThan.
// Decision is based on the UTC timestamp embedded in the directory
// name (`<slug>-YYYYMMDD-HHMMSS`). We do NOT use mtime because
// rename(2) preserves the source dir's mtime — a long-lived worker
// archived today could otherwise be pruned immediately because its
// mtime reflects when the worker first ran, not when it was archived.
//
// Directories whose names don't parse as the canonical archive
// pattern are skipped (returned in the int as un-pruned). Returns
// the count of removed directories.
func PruneArchive(project string, olderThan time.Time) (int, error) {
	dir, err := state.ProjectDir(project)
	if err != nil {
		return 0, err
	}
	archDir := filepath.Join(dir, "workers", "archive")
	entries, err := os.ReadDir(archDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("readdir archive: %w", err)
	}
	removed := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		archivedAt, ok := parseArchiveStamp(e.Name())
		if !ok {
			// Unrecognized naming — leave alone. Operator can
			// remove manually.
			continue
		}
		if !archivedAt.Before(olderThan) {
			continue
		}
		full := filepath.Join(archDir, e.Name())
		if err := os.RemoveAll(full); err != nil {
			return removed, fmt.Errorf("remove %s: %w", full, err)
		}
		removed++
	}
	return removed, nil
}

// parseArchiveStamp extracts the trailing `-YYYYMMDD-HHMMSS` from an
// archive directory name (`<slug>-YYYYMMDD-HHMMSS`). Returns the
// parsed UTC time and true on success; zero time + false on any
// parse failure.
//
// The slug itself can contain hyphens, so we look for the LAST 16
// chars matching the canonical stamp format. We use a fixed format
// rather than walking back to the last hyphen pair because a slug
// like `add-readme-7a3c` would otherwise be ambiguous.
func parseArchiveStamp(name string) (time.Time, bool) {
	const stampLen = len("YYYYMMDD-HHMMSS") // 15
	if len(name) < stampLen+1 || name[len(name)-stampLen-1] != '-' {
		return time.Time{}, false
	}
	stamp := name[len(name)-stampLen:]
	t, err := time.ParseInLocation("20060102-150405", stamp, time.UTC)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// ListActive scans workers/*/state.json and returns the parsed states
// in slug-sorted order. Skips the archive/ subdir and any directory
// without a state.json.
func ListActive(project string) ([]*State, error) {
	dir, err := state.ProjectDir(project)
	if err != nil {
		return nil, err
	}
	wDir := filepath.Join(dir, "workers")
	entries, err := os.ReadDir(wDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("readdir workers: %w", err)
	}
	var out []*State
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if e.Name() == "archive" {
			continue
		}
		s, err := ReadState(project, e.Name())
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			// Skip malformed; coord may still be writing it.
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out, nil
}

// ListAll returns active + archived workers. Archived states are
// re-read from each archive subdir's state.json. Slug order within
// each list. Archive entries carry the original slug (no stamp
// suffix) — callers can still locate the archive dir via the slug
// + archive directory listing if they need to.
func ListAll(project string) (active []*State, archived []*State, err error) {
	active, err = ListActive(project)
	if err != nil {
		return nil, nil, err
	}
	dir, derr := state.ProjectDir(project)
	if derr != nil {
		return nil, nil, derr
	}
	archDir := filepath.Join(dir, "workers", "archive")
	entries, rerr := os.ReadDir(archDir)
	if rerr != nil {
		if os.IsNotExist(rerr) {
			return active, nil, nil
		}
		return nil, nil, fmt.Errorf("readdir archive: %w", rerr)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(archDir, e.Name(), "state.json")
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			continue
		}
		var s State
		if jerr := json.Unmarshal(data, &s); jerr != nil {
			continue
		}
		archived = append(archived, &s)
	}
	sort.Slice(archived, func(i, j int) bool { return archived[i].Slug < archived[j].Slug })
	return active, archived, nil
}

// stateJSONPath builds workers/<slug>/state.json.
func stateJSONPath(project, slug string) (string, error) {
	if slug == "" {
		return "", fmt.Errorf("%w: empty slug", ErrInvalidSlug)
	}
	dir, err := state.WorkerDir(project, slug)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "state.json"), nil
}
