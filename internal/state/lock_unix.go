//go:build unix

package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// ErrLockTimeout is wrapped into the error returned by a bounded acquire
// that gives up at its deadline (a holder still owns the lock). Callers
// use errors.Is to distinguish "another holder is provably running" from
// a genuine open/path fault — the two demand different handling (the
// worktree-GC caller skips its phase on a timeout rather than running
// unlocked, but still surfaces a real fault). Surface-don't-silo: a
// timeout is a first-class, inspectable outcome, not an opaque error.
var ErrLockTimeout = errors.New("lock acquire timed out")

// pollInterval is the busy-wait granularity for the bounded acquire. A
// flock(2) LOCK_NB poll is cheap (no I/O on success/EWOULDBLOCK), so a
// short interval keeps the worst-case overshoot past the deadline small
// while not spinning the CPU. Used only by the bounded variant; the
// non-blocking helpers never sleep.
const pollInterval = 10 * time.Millisecond

// LockProject takes an exclusive flock on
// ~/.fleet/projects/.locks/<project>.lock and returns a release
// function. Concurrent calls for the same project block; different
// projects proceed in parallel.
//
// Used by the handoff/spawn drain loop so two consumers (e.g., the
// CLI inline drain and a future TUI background drain) cannot race
// on the same project's queue. The .lock file is created if absent
// and never removed — a tiny zero-byte sentinel, the lock state
// lives in the kernel.
//
// Pattern:
//
//	release, err := state.LockProject("rainier")
//	if err != nil { return err }
//	defer release()
//	// ... drain queue, spawn replacement, etc ...
//
// Bootstrap creates ~/.fleet/projects/.locks/, so this never has to
// MkdirAll. If FLEET_HOME points at a fresh directory and Bootstrap
// has not run yet, opening the file will fail — surfacing the bug
// where it can be fixed.
//
// flock(2) is advisory and process-scoped. Two goroutines in the
// same process do NOT serialize via this — they share the same fd.
// That matches what we want for v1: per-process, per-project.
//
// Acquire blocks (bounded by a generous default deadline) until the
// holder releases. See acquireFlock for the LOCK_NB-poll mechanism that
// replaced the bare blocking LOCK_EX (DESIGN-handoff-drain-storm-leak:
// a bare LOCK_EX hangs forever behind a wedged holder).
func LockProject(project string) (func(), error) {
	return acquireFlock(ProjectLockPath, project, defaultLockTimeout)
}

// defaultLockTimeout bounds the legacy blocking-shaped acquires
// (LockProject / LockProjectState / LockProjectWorktreeGC). These guard
// short critical sections; a contended waiter should block long enough
// to win a normal hand-off but never hang forever behind a wedged holder
// (the incident behind DESIGN-handoff-drain-storm-leak). On timeout the
// error names the stale holder PID stamped in the lock body so the caller
// can surface a provably-stuck holder rather than failing opaquely.
const defaultLockTimeout = 30 * time.Second

// worktreeGCLockTimeout bounds LockProjectWorktreeGC. Unlike the
// per-record state lock (a fast critical section), the worktree-GC lock is
// held across a whole `fleet gc --kinds=worktrees` scan -> re-check ->
// remove window, which can legitimately run for minutes on a large repo.
// The bound is generous so a normally-contended waiter still serializes;
// on a genuine timeout the caller SKIPS its worktree phase (another GC is
// provably running it) rather than proceeding unlocked — see cmd/fleet/gc.go.
//
// A var (not const) only so a test can shrink it to assert the
// timeout-skip path without a 5-minute wait. Production never reassigns it.
var worktreeGCLockTimeout = 5 * time.Minute

// agentLockTimeout bounds LockAgent. Like worktree-GC (and UNLIKE the fast
// per-record state lock), the agent lock is held across a whole handoff
// `Resume` — which includes spawn.WaitForReadyToPrompt (~30s default) plus
// post-ready buffer and grace/kill/archive steps — so a 30s bound would
// make a legitimate concurrent same-agent op (a second `fleet handoff`/
// `fleet drain`/`fleet rm`) FAIL with ErrLockTimeout mid-handoff instead
// of serializing. The generous bound preserves serialization for a normal
// slow handoff while still ruling out the forever-hang incident (a wedged
// holder past this bound surfaces ErrLockTimeout naming the stale pid,
// rather than blocking silently forever). PR3 removes LockAgent-across-
// Resume entirely; until then this keeps existing behavior intact.
const agentLockTimeout = 5 * time.Minute

// LockProjectState takes an exclusive flock on
// ~/.fleet/projects/<project>/.locks/state.lock — the v0.2 single
// state-lock per project state-dir (Q1 decision). Held briefly during
// writes to tasks.md / tasks-archive.md / learnings.md /
// learnings-archive.md.
//
// Distinct from LockProject (which serializes dispatch + handoff under
// projects/.locks/<name>.lock). The two lock trees do not interact;
// `fleet dispatch` doesn't touch tasks.md and `fleet tasks add`
// doesn't dispatch.
//
// The .locks/ dir is NOT created by Bootstrap — it's lazily created
// here on first acquire (the per-project state-dir is itself lazy:
// see internal/state.ProjectDir, which doesn't MkdirAll either).
func LockProjectState(project string) (func(), error) {
	path, err := ProjectStateLockPath(project)
	if err != nil {
		return nil, err
	}
	return acquireBoundedAt(path, defaultLockTimeout)
}

// LockProjectStateTimeout is LockProjectState with a caller-chosen
// deadline. A non-positive timeout means "single LOCK_NB attempt"
// (fail-fast). On timeout the error includes the stale holder PID stamped
// in the lock body (surface-don't-silo: a timed-out waiter must report
// the provably-stuck holder, not hang or fail opaquely).
func LockProjectStateTimeout(project string, timeout time.Duration) (func(), error) {
	path, err := ProjectStateLockPath(project)
	if err != nil {
		return nil, err
	}
	return acquireBoundedAt(path, timeout)
}

// SetWorktreeGCLockTimeoutForTest overrides the worktree-GC lock timeout
// and returns a restore func. TEST-ONLY: it exists so cmd/fleet's gc test
// (a different package) can assert the timeout-skip path without a
// 5-minute wait. Production code never calls it.
func SetWorktreeGCLockTimeoutForTest(d time.Duration) (restore func()) {
	prev := worktreeGCLockTimeout
	worktreeGCLockTimeout = d
	return func() { worktreeGCLockTimeout = prev }
}

// LockProjectWorktreeGC takes an exclusive flock on
// ~/.fleet/projects/<project>/.locks/worktree-gc.lock — held across the
// `fleet gc --kinds=worktrees` scan → re-check-porcelain → `git worktree
// remove` window (DESIGN-coord-worktree-lifecycle § Concurrency). A
// coord-invoked run (every-N-ticks) and an operator-invoked run serialize
// on this lock so they can't both decide to remove the same dir. Distinct
// from LockProjectState (tasks.md writes) so a slow GC scan doesn't block
// task mutations.
//
// The .locks/ dir is lazily created here (mirrors LockProjectState).
// On a contention timeout the error wraps ErrLockTimeout: the caller MUST
// skip the worktree-GC phase (another GC is provably mid-scan) rather than
// proceed unlocked — proceeding unlocked would reintroduce the very
// scan/re-check/remove race this lock prevents.
func LockProjectWorktreeGC(project string) (func(), error) {
	path, err := ProjectWorktreeGCLockPath(project)
	if err != nil {
		return nil, err
	}
	return acquireBoundedAt(path, worktreeGCLockTimeout)
}

// LockAgent takes an exclusive flock on
// ~/.fleet/agents/.locks/<id>.lock. Concurrent handoffs of the same
// agent serialize; concurrent handoffs of DIFFERENT agents (even in
// the same project) proceed in parallel.
//
// This is the right granularity for `fleet handoff` because the only
// shared state under the lock is the single agent record (and its
// handoff doc). Different agents have disjoint files, so there's no
// reason to serialize their handoffs together.
//
// Compare to LockProject (broader scope, used for project-manifest
// mutations in 4b+ when many agents share one manifest).
//
// Uses the generous agentLockTimeout (NOT the short state-lock default):
// the handoff Resume holds this across a slow spawn-and-wait, so a tighter
// bound would turn legitimate same-agent serialization into spurious
// ErrLockTimeout failures (see agentLockTimeout).
func LockAgent(id string) (func(), error) {
	return acquireFlock(AgentLockPath, id, agentLockTimeout)
}

// acquireFlock is the shared body for the per-scope lock helpers.
// Takes a path-builder function so each helper can resolve its own
// canonical lock path while sharing the open + Flock + close-on-release
// machinery, plus a timeout that bounds the acquire (see acquireBoundedAt).
func acquireFlock(pathFn func(string) (string, error), key string, timeout time.Duration) (func(), error) {
	path, err := pathFn(key)
	if err != nil {
		return nil, err
	}
	return acquireBoundedAt(path, timeout)
}

// acquireBoundedAt opens (creating if absent) the lock file at path and
// takes an exclusive flock, polling LOCK_NB until acquired or the timeout
// elapses. It NEVER uses a bare blocking LOCK_EX — the incident behind
// DESIGN-handoff-drain-storm-leak was a bare LOCK_EX hanging forever
// behind a wedged holder. A non-positive timeout means "one LOCK_NB
// attempt" (fail-fast).
//
// On success the lock body is stamped with the holder PID + acquire
// timestamp so a contending waiter that times out can name the provably-
// stale holder (surface-don't-silo). On timeout the error embeds that
// holder hint.
//
//	t=0   LOCK_NB → EWOULDBLOCK ─┐
//	      sleep(pollInterval)    │ loop until acquired
//	t=Δ   LOCK_NB → EWOULDBLOCK ─┘ or deadline exceeded
//	      deadline exceeded → read holder body → timeout error
func acquireBoundedAt(path string, timeout time.Duration) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir lock dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open lock %s: %w", path, err)
	}

	// deadline == zero value means "single attempt" (timeout <= 0).
	var deadline time.Time
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}

	for {
		lerr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if lerr == nil {
			stampLockBody(f)
			return func() {
				// Unlock BEFORE close so a contending waiter can acquire
				// immediately. Close also releases on Linux/macOS, but the
				// explicit unlock is the documented contract.
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if lerr != syscall.EWOULDBLOCK { //nolint:errorlint // syscall.Flock returns the bare errno, not a wrapped error.
			_ = f.Close()
			return nil, fmt.Errorf("flock %s: %w", path, lerr)
		}
		// Held by someone else. Give up immediately on a non-positive
		// timeout, otherwise poll until the deadline. The timeout error
		// wraps ErrLockTimeout so callers can distinguish "another holder
		// is running" from a real fault, and names the stale holder pid.
		if timeout <= 0 || !time.Now().Before(deadline) {
			holder := readLockHolder(path)
			_ = f.Close()
			return nil, fmt.Errorf("acquire lock %s: timed out after %s waiting for holder%s: %w",
				path, timeout, holder, ErrLockTimeout)
		}
		time.Sleep(pollInterval)
	}
}

// stampLockBody writes "pid=<n> ts=<unix>" into the lock body so a
// contending waiter that times out can report the holder. Best-effort:
// flock alone enforces exclusion, so a stamp failure is not fatal.
func stampLockBody(f *os.File) {
	if err := f.Truncate(0); err != nil {
		return
	}
	if _, err := f.Seek(0, 0); err != nil {
		return
	}
	_, _ = fmt.Fprintf(f, "pid=%d ts=%d\n", os.Getpid(), time.Now().Unix())
}

// readLockHolder reads the stamped holder body for a timeout error
// message. Best-effort — a torn read between a holder's Truncate and
// Write just yields an empty/partial hint, which is harmless (it's a
// debug aid, not a load-bearing signal). Returns a leading-space clause
// (" (holder: ...)") or "" so callers can concatenate directly.
func readLockHolder(path string) string {
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 {
		return ""
	}
	body := string(b)
	for len(body) > 0 && (body[len(body)-1] == '\n' || body[len(body)-1] == ' ') {
		body = body[:len(body)-1]
	}
	if body == "" {
		return ""
	}
	return " (holder: " + body + ")"
}
