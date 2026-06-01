package dispatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

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

// ============================================================
// dispatch-durability (fleet#184) — per-journal flock RMW primitive
// ============================================================
//
// PROBLEM the lock closes (DESIGN-dispatch-durability.md rev5 P1-A):
// writeJournal is a bare state.WriteAtomic (tmp+rename+fsync) with NO
// lock and NO version check. Atomic rename prevents TORN writes but NOT
// lost updates across a read-modify-write: writer A reads genN, writer
// B reads genN, A renames N+1, B renames N+1 clobbering A. With #184's
// launch-attempt flip + replay-cap increment as concurrent RMW writers,
// that lost update silently re-opens the double-launch P0.
//
// FIX: a per-id flock on <id>.json.lock held across the ENTIRE
// read → predicate → mutate → WriteAtomic critical section, taken by
// EVERY journal writer. Under the lock the RMW is genuinely atomic —
// B blocks until A's rename completes, then reads A's fresh state.
//
//	withJournalLock(id):
//	  flock(LOCK_EX|LOCK_NB) with bounded retry/backoff + deadline
//	    |
//	    +-- deadline exceeded -> ErrJournalContention (TRANSIENT; retry)
//	    |
//	    +-- acquired -> fn(load -> predicate -> mutate -> WriteAtomic) -> unlock
//
// Per-id (NOT dir-wide) matches the LockAgent precedent: different
// dispatches proceed in parallel, only same-id writers serialize. flock
// auto-releases on process death (fd close), so a dead coord cannot hold
// it. Requires ~/.fleet on a local POSIX fs (flock over NFS is
// unreliable) — fail closed with a clear diagnostic otherwise.

// ErrJournalContention is returned when the per-journal flock could not
// be acquired within journalLockDeadline. It is TRANSIENT — the caller
// (coord protocol step 2) must retry the same block on the next tick,
// NEVER treat it as a predicate-fail/skip (that would silently drop a
// launch). Distinct sentinel so callers branch on it explicitly.
var ErrJournalContention = errors.New("journal lock contention")

// journalLockDeadline bounds how long withJournalLock spins on
// LOCK_NB before giving up with ErrJournalContention. Mirrors the
// fail-fast spirit of coordlock/rc: never an unbounded LOCK_EX (which
// would stall the tick behind a wedged writer). The critical section is
// a few-KB tmp+rename, so a holder normally releases in microseconds;
// 2s of retries tolerates a momentarily-busy disk without blocking the
// tick.
var journalLockDeadline = 2 * time.Second

// journalLockBackoff is the sleep between LOCK_NB retry attempts.
const journalLockBackoff = 20 * time.Millisecond

// journalLockSleep is the package-level sleep seam so concurrency tests
// can drive the backoff loop deterministically. Production sleeps for
// real.
var journalLockSleep = time.Sleep

// journalLockPath returns ~/.fleet/dispatches/<id>.json.lock — a
// zero-byte sentinel whose lock state lives in the kernel (the file
// itself is never removed; same discipline as state.LockAgent).
func journalLockPath(id DispatchID) (string, error) {
	p, err := JournalPath(id)
	if err != nil {
		return "", err
	}
	return p + ".lock", nil
}

// withJournalLock runs fn while holding the per-id flock. It is the
// single locked RMW primitive — ALL journal writers route through it.
//
// Lock idiom: non-blocking LOCK_EX|LOCK_NB + bounded retry/backoff with
// a deadline (mirrors coordlock.Acquire's fail-fast + the rc withLock).
// On deadline → ErrJournalContention (the caller retries; we NEVER block
// unbounded, and NEVER map EWOULDBLOCK → silent skip).
//
// fn receives nothing and returns an error; it should load the journal,
// run its predicate, mutate, and persist via writeJournalLocked (the
// raw writer — SaveJournal is the lock-free public path kept only for
// non-RMW callers / back-compat). The lock is held for the whole fn.
func withJournalLock(id DispatchID, fn func() error) error {
	lockPath, err := journalLockPath(id)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return fmt.Errorf("mkdir dispatches: %w", err)
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("open journal lock %s: %w", lockPath, err)
	}
	defer func() { _ = f.Close() }() // close releases the flock

	// Deadline uses real wall-clock time (time.Now), NOT the pinnable
	// nowFunc: tests freeze nowFunc for deterministic journal timestamps,
	// and a frozen clock would make the retry loop spin forever. The lock
	// backoff is a real-time concern (how long to wait on a busy disk),
	// orthogonal to the journal's logical timestamps.
	deadline := time.Now().Add(journalLockDeadline)
	for {
		lerr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if lerr == nil {
			break
		}
		if !errors.Is(lerr, syscall.EWOULDBLOCK) {
			// EINVAL/ENOLCK etc — e.g. flock unsupported on this fs
			// (NFS). Fail closed with a clear diagnostic rather than
			// proceeding lock-free (which would re-open the lost-update
			// race). surface-dont-silo.
			return fmt.Errorf(
				"flock journal %s: %w (requires ~/.fleet on a local POSIX fs; "+
					"flock over NFS is unreliable)", lockPath, lerr)
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%w: %s held past %s deadline",
				ErrJournalContention, lockPath, journalLockDeadline)
		}
		journalLockSleep(journalLockBackoff)
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()

	return fn()
}

// writeJournalLocked persists the journal from inside withJournalLock.
// It stamps UpdatedAt (like SaveJournal) but must ONLY be called with
// the per-id flock held — it is the commit half of the RMW critical
// section.
func writeJournalLocked(j *Journal) error {
	j.UpdatedAt = nowFunc().UTC()
	return writeJournal(j)
}

// loadJournalLocked is the read half of an RMW, callable from inside
// withJournalLock. It is just readJournal — named for symmetry so the
// lock contract is obvious at call sites.
func loadJournalLocked(id DispatchID) (*Journal, error) {
	return readJournal(id)
}
