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
	// PhaseReviewPending and PhaseReviewDone are the three-stage flow
	// (worker → reviewer → finisher) handoff points (issue
	// reviewer-subagent-arch). The worker writes phase=review-pending
	// when its code commits land; the coord dispatches a reviewer
	// subagent against it. The reviewer writes phase=review-done +
	// terminal review_*_status when its /review + codex loop returns
	// clean; the coord dispatches a finisher subagent that pushes +
	// opens the PR. PhasePush remains the on-the-wire phase the
	// finisher writes immediately before phase=done.
	PhaseReviewPending Phase = "review-pending"
	PhaseReviewDone    Phase = "review-done"
	PhasePush          Phase = "push"
	PhaseDone          Phase = "done"
	PhaseBlocked       Phase = "blocked"
	PhaseFailed        Phase = "failed"
)

func validPhase(p Phase) bool {
	switch p {
	case PhaseStarting, PhaseBranch, PhaseTDDRed, PhaseTDDGreen,
		PhaseTDDRefactor, PhaseReviewClaude, PhaseReviewCodex,
		PhaseReviewPending, PhaseReviewDone,
		PhasePush, PhaseDone, PhaseBlocked, PhaseFailed:
		return true
	}
	return false
}

// ReviewStatus is the per-reviewer state on a worker's state.json.
// The three-stage flow's reviewer subagent writes terminal values
// (passed | skipped | blocked) before flipping phase to review-done;
// the workers.go writeStateLocked validator gates phase=push on those
// terminal values to prevent a worker (or a buggy reviewer) from
// reaching push without recording review outcome.
//
// "skipped" is allowed for codex review when the codex CLI is
// unreachable (rate-limit, network) — see the CLI-side allowlist on
// `--review-codex-skip-reason`. "skipped" is NEVER allowed for /review
// (Claude-side gstack skill); the validator below enforces that.
type ReviewStatus string

const (
	ReviewStatusPending   ReviewStatus = "pending"   // not started
	ReviewStatusIterating ReviewStatus = "iterating" // in /review or codex loop
	ReviewStatusPassed    ReviewStatus = "passed"    // terminal — clean
	ReviewStatusSkipped   ReviewStatus = "skipped"   // terminal — codex only
	ReviewStatusBlocked   ReviewStatus = "blocked"   // terminal — needs operator
)

func validReviewStatus(s ReviewStatus) bool {
	switch s {
	case "", // empty = unset; older workers that pre-date the field
		ReviewStatusPending, ReviewStatusIterating,
		ReviewStatusPassed, ReviewStatusSkipped, ReviewStatusBlocked:
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

	// Three-stage flow review fields (reviewer-subagent-arch).
	// Workers pre-dating the three-stage flow leave these zero;
	// reviewer subagents populate them via `fleet workers update`
	// before flipping phase=review-done. The phase=push validator
	// gates on these (see writeStateLocked).
	ReviewClaudeStatus    ReviewStatus `json:"review_claude_status,omitempty"`
	ReviewClaudeRounds    int          `json:"review_claude_rounds,omitempty"`
	ReviewCodexStatus     ReviewStatus `json:"review_codex_status,omitempty"`
	ReviewCodexRounds     int          `json:"review_codex_rounds,omitempty"`
	ReviewCodexSkipReason string       `json:"review_codex_skip_reason,omitempty"`
}

// Errors.
var (
	ErrNotFound          = errors.New("worker state.json not found")
	ErrInvalidState      = errors.New("invalid worker state")
	ErrPhaseRequiresPR   = errors.New("phase=done requires pr_url")
	ErrPhaseRequiresWhy  = errors.New("phase=blocked requires blocked_reason")
	ErrInvalidPhase      = errors.New("invalid phase")
	ErrInvalidReviewStat = errors.New("invalid review status")
	ErrInvalidSlug       = errors.New("invalid worker slug")
	ErrPreconditionLive  = errors.New("cannot archive live worker")
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
// Takes the per-worker in-process mutex + flock so concurrent writers
// (including a concurrent Archive) serialize through the same lock as
// UpdateState. Without this, a WriteState landing after Archive's
// rename would recreate workers/<slug>/state.json and leave both an
// archived dir and a fresh active one.
func WriteState(project, slug string, s *State) error {
	if slug == "" {
		return fmt.Errorf("%w: empty slug", ErrInvalidSlug)
	}
	key := project + "/" + slug
	muIface, _ := updateMu.LoadOrStore(key, &sync.Mutex{})
	mu := muIface.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	release, err := acquireWorkerLock(project, slug)
	if err != nil {
		return err
	}
	defer release()
	return writeStateLocked(project, slug, s)
}

// writeStateLocked is the body of WriteState without lock acquisition.
// UpdateState calls this directly because it already holds the lock;
// public callers must go through WriteState (which locks) so concurrent
// writes serialize with Archive.
func writeStateLocked(project, slug string, s *State) error {
	if s == nil {
		return fmt.Errorf("%w: nil state", ErrInvalidState)
	}
	if !validPhase(s.Phase) {
		return fmt.Errorf("%w: %q", ErrInvalidPhase, s.Phase)
	}
	if !validReviewStatus(s.ReviewClaudeStatus) {
		return fmt.Errorf("%w: review_claude_status=%q", ErrInvalidReviewStat, s.ReviewClaudeStatus)
	}
	if !validReviewStatus(s.ReviewCodexStatus) {
		return fmt.Errorf("%w: review_codex_status=%q", ErrInvalidReviewStat, s.ReviewCodexStatus)
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
// The flock target is ~/.fleet/projects/<project>/.locks/worker-<slug>.lock
// — under the per-project lock dir, NOT under workers/<slug>/. Putting
// the lock under workers/<slug>/ would let Archive's rename move it
// out from under in-flight updaters; Archive could then race a fresh
// UpdateState that recreates workers/<slug>/state.json after the
// rename, leaving stray active + archive dirs.
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

	release, err := acquireWorkerLock(project, slug)
	if err != nil {
		return err
	}
	defer release()

	dir, err := state.WorkerDir(project, slug)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir worker dir: %w", err)
	}

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
	return writeStateLocked(project, slug, cur)
}

// acquireWorkerLock returns an exclusive flock on the per-worker lock
// file under ~/.fleet/projects/<project>/.locks/worker-<slug>.lock.
// The path is stable across worker dir renames, so Archive and
// UpdateState contend through the same fs object even when the
// worker dir itself is being moved.
//
// Returns a release function the caller must defer.
func acquireWorkerLock(project, slug string) (func(), error) {
	pdir, err := state.ProjectDir(project)
	if err != nil {
		return nil, err
	}
	lockDir := filepath.Join(pdir, ".locks")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir lock dir: %w", err)
	}
	lockPath := filepath.Join(lockDir, "worker-"+state.SafeLockComponent(slug)+".lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open worker lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("flock worker lock: %w", err)
	}
	return func() { _ = f.Close() }, nil
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

	// Coordinate with UpdateState: take the same in-process mutex
	// + per-worker flock. Both live under .locks/ — a stable path
	// the rename below does NOT touch, so the lock survives the
	// directory move and any concurrent UpdateState observes the
	// lock through the SAME inode.
	key := project + "/" + slug
	muIface, _ := updateMu.LoadOrStore(key, &sync.Mutex{})
	mu := muIface.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()

	release, err := acquireWorkerLock(project, slug)
	if err != nil {
		return fmt.Errorf("acquire worker lock for archive: %w", err)
	}
	defer release()

	// Existence check AFTER the lock — a concurrent Archive could
	// have moved the dir between our entry and the lock acquire.
	// Treat post-lock missing-dir as a successful no-op.
	if _, statErr := os.Stat(src); os.IsNotExist(statErr) {
		return nil
	} else if statErr != nil {
		return fmt.Errorf("stat worker dir: %w", statErr)
	}

	// Re-verify preconditions under the lock — the predecessor's
	// UpdateState may have just landed and bumped phase/pid.
	cur, rerr := ReadState(project, slug)
	switch {
	case rerr == nil:
		if !isTerminalPhase(cur.Phase) {
			return fmt.Errorf("%w: phase=%q (must be done|blocked|failed)", ErrPreconditionLive, cur.Phase)
		}
		// Trust a recorded exit code: if the worker wrote one,
		// the process IS gone, and IsAlive(PID) is unreliable
		// (kernel may have recycled the PID for another process).
		// Only consult IsAlive when no exit code is recorded —
		// e.g. the worker reached terminal phase and crashed
		// before exit could be set.
		//
		// pid==0 + exit==nil is "unknown" — UpdateState bootstraps
		// states with phase=starting and no PID, so a worker that
		// flips straight to terminal before its PID/exit is persisted
		// would otherwise sail through this gate while still running.
		// Refuse: the caller must record an exit code (or a PID) before
		// archive is safe (codex iter-10 P2).
		switch {
		case cur.Exit != nil:
			// Process gone, archive is safe.
		case cur.PID > 0:
			if IsAlive(cur.PID) {
				return fmt.Errorf("%w: phase=%q but pid=%d still alive", ErrPreconditionLive, cur.Phase, cur.PID)
			}
		default:
			return fmt.Errorf("%w: phase=%q but pid=0 and no exit recorded — refuse to archive (record exit code first)", ErrPreconditionLive, cur.Phase)
		}
	case errors.Is(rerr, ErrNotFound):
		// No state file — proceed (caller's responsibility).
	default:
		return fmt.Errorf("read state for archive precondition: %w", rerr)
	}

	// Lock stays held through the rename (closure runs on return).
	// A second UpdateState entering after our return must take the
	// same lock first; by that time tasks.md should have been
	// updated by the coord to clear worker_pid, and a fresh
	// UpdateState on the now-archived slug is the caller's bug.

	// Archive same slug twice within one second is possible (worker
	// blocked → operator clarifies → coord re-dispatches → worker
	// fails fast → archive again). Bump second-precision stamp to
	// retry on EEXIST so we never leave the worker dir behind.
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
	for attempt := 0; attempt < 8; attempt++ {
		err := os.Rename(src, dst)
		if err == nil {
			return nil
		}
		// EEXIST or directory-not-empty — bump suffix and retry.
		// Salt is single digit (1..7); parseArchiveStamp strips a
		// trailing -<digit> before reading the canonical 15-char
		// stamp, so retention math is independent of the salt.
		dst2, derr := state.WorkerArchiveDir(project, slug, fmt.Sprintf("%s-%d", stamp, attempt+1))
		if derr != nil {
			return derr
		}
		dst = strings.TrimSuffix(dst2, string(filepath.Separator))
	}
	return fmt.Errorf("rename %s → %s after 8 attempts", src, dst)
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

// Delete removes workers/<slug>/ entirely (rm -rf, no archive). Used
// by the lifecycle hygiene path (issue #101) when a worker reaches a
// terminal phase (done | failed) — the operator-visible PR URL is
// already persisted on the task entry, so the worker dir contributes
// no extra information and just clutters the project tree.
//
// Validation:
//   - slug must pass state.ValidateSlug (rejects "..", "/", empty, etc).
//   - destination MUST resolve under
//     ~/.fleet/projects/<project>/workers/ — defense against a
//     bug-introduced path traversal that could otherwise let a
//     mis-routed Delete recurse into something operator-owned.
//   - the archive/ subdir is NOT a valid target. Archive entries are
//     pruned via PruneArchive's age-based path; a slug parameter that
//     literally equals "archive" would resolve to workers/archive/ and
//     blow away every archived worker. Refuse explicitly.
//
// Idempotency: returns nil when the dir is already gone (ENOENT). The
// design's "first mover wins; second sees ENOENT" contract for the
// dual-trigger TUI/coord cleanup flow depends on this.
//
// Atomicity: rename to a sibling `<slug>.deleting-<UTCstamp>/` first,
// then RemoveAll on the staged path. A crash mid-delete leaves at most
// a `.deleting-...` directory, never a partial slug dir that a future
// Read would mistake for live state. Stamp-suffix avoids collisions if
// Delete is retried after a partial crash. Best-effort: rename failure
// (cross-filesystem, permissions) falls back to direct RemoveAll —
// less crash-safe but still gets the dir gone in the common case.
//
// NB: Delete does NOT take the per-worker flock that Archive holds.
// The intended caller is the coord skill (post-Agent-tool-return) and
// the TUI defense-in-depth scan, both of which run AFTER the worker
// process has exited and the task entry has the PR URL persisted. A
// concurrent UpdateState landing during a Delete would recreate the
// dir; that's the operator's bug to surface (writing to a worker the
// caller already declared terminal), not a state we work around here.
// Future: layer the same lock + precondition pattern as Archive if
// we observe the race in practice.
func Delete(project, slug string) error {
	if err := state.ValidateSlug(slug); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidSlug, err)
	}
	if slug == "archive" {
		return fmt.Errorf("%w: refusing to delete archive directory", ErrInvalidSlug)
	}
	pdir, err := state.ProjectDir(project)
	if err != nil {
		return err
	}
	src, err := state.WorkerDir(project, slug)
	if err != nil {
		return err
	}
	src = strings.TrimSuffix(src, string(filepath.Separator))

	// Path-traversal safety net. state.WorkerDir already constructs the
	// path from validated components, but a future refactor could
	// silently change that — keep an independent boundary check here so
	// a regression surfaces before it lets us recurse into something
	// outside workers/.
	workersRoot := filepath.Join(pdir, "workers")
	rel, relErr := filepath.Rel(workersRoot, src)
	if relErr != nil || strings.HasPrefix(rel, "..") || rel == "." {
		return fmt.Errorf("%w: resolved path %q escapes workers root", ErrInvalidSlug, src)
	}

	if _, statErr := os.Stat(src); os.IsNotExist(statErr) {
		return nil
	} else if statErr != nil {
		return fmt.Errorf("stat worker dir: %w", statErr)
	}

	// Stage rename to a sibling `<slug>.deleting-<UTCstamp>` so a crash
	// can't leave the operator wondering whether `workers/<slug>/` is
	// half-deleted. Stamp avoids EEXIST on retry-after-crash.
	stamp := time.Now().UTC().Format("20060102-150405")
	staged := filepath.Join(workersRoot, slug+".deleting-"+stamp)
	for attempt := 0; attempt < 4; attempt++ {
		err := os.Rename(src, staged)
		if err == nil {
			return os.RemoveAll(staged)
		}
		if os.IsNotExist(err) {
			// Won the race against another Delete. Idempotent — fine.
			return nil
		}
		// EEXIST on the staged target — bump suffix and retry.
		if os.IsExist(err) {
			staged = filepath.Join(workersRoot, fmt.Sprintf("%s.deleting-%s-%d", slug, stamp, attempt+1))
			continue
		}
		// Cross-filesystem rename or permission — fall back to direct
		// RemoveAll. Less crash-safe but the common-case POSIX same-fs
		// path already returned by now.
		return os.RemoveAll(src)
	}
	return fmt.Errorf("rename %s → %s after 4 attempts", src, staged)
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

// parseArchiveStamp extracts the trailing UTC stamp from an archive
// directory name. The canonical name shape is:
//
//	<slug>-YYYYMMDD-HHMMSS               (most common)
//	<slug>-YYYYMMDD-HHMMSS-<N>           (collision-retry suffix; N=1..7)
//
// Returns the parsed UTC time and true on success; zero time + false
// on any parse failure. The slug itself can contain hyphens, so we
// anchor on the fixed 15-char stamp pattern preceded by `-`.
func parseArchiveStamp(name string) (time.Time, bool) {
	const stampLen = len("YYYYMMDD-HHMMSS") // 15
	// Strip a possible `-<digit>` collision suffix first so the
	// stamp inspection sees the canonical form. We accept up to a
	// single-digit salt (matches the producer in Archive).
	if len(name) >= 2 && name[len(name)-2] == '-' && name[len(name)-1] >= '0' && name[len(name)-1] <= '9' {
		name = name[:len(name)-2]
	}
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
