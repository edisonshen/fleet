//go:build linux || darwin

package coord

// kill.go — KillCoordIfIdentityMatches: the ONE authenticated coord-kill
// primitive (DESIGN-handoff-drain-storm-leak §B.5). Every path that
// terminates a coord — the takeover STONITH (injected into the lease via
// coordlock.AcquireLeaseWithKill), the new-leader competitor sweep, and
// (in later PRs) the graceful drain killer / fleet doctor — funnels
// through this function so the PID-reuse + exe-path + epoch + self gate is
// enforced in exactly one place.
//
// It targets the `fleet coord-run` SUPERVISOR pid (agent.Record.
// SupervisorPID), NOT the engine child (agent.Record.PID). Killing the
// supervisor is what releases coordinator.flock via kernel-on-death.
//
//	      ┌─────────────────────────────────────────────────────────┐
//	      │ KillCoordIfIdentityMatches(target)                        │
//	      └─────────────────────────────────────────────────────────┘
//	                              │
//	   re-read the live agent record(s) for target.Project; find the
//	   one whose SupervisorPID == target.Pid
//	                              │
//	   SIGNAL only if ALL hold (else SURFACE, never kill):
//	     1. record.Project == target.Project
//	     2. live pid_start(SupervisorPID) == record.SupervisorPidStart
//	        AND == target.PidStart            (defeats PID reuse, T36)
//	     3. record.SupervisorExePath is the fleet coord-run binary
//	        (NOT the engine child)            (W9)
//	     4. target.Pid is NOT the current active lease owner
//	        (epoch gate — never shoot the live leader, T10)
//	     5. target.Pid != self
//	                              │
//	   grace delay  →  SIGTERM  →  poll  →  SIGKILL
//	                              │
//	   stderr: "reaped coord pid=<n> agent=<id> epoch=<e>"

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/coordlock"
)

// KillTarget mirrors coordlock.KillTarget so the coord package's public
// primitive does not force callers to import coordlock just to name a
// target. The supervisor wires the two together (coordlock.KillTarget ->
// coord.KillTarget) in cmd/fleet/coord.go.
type KillTarget struct {
	Pid         int    // OLD coord-run supervisor pid to reap
	PidStart    int64  // its start time at fence (PID-reuse guard)
	AgentID     string // OLD coord's agent id (diagnostics + record match)
	Project     string // scopes the kill to one project's coord
	FencerEpoch int64  // the epoch the caller fenced TO (diagnostics + log)
}

// ErrKillRefused wraps every "do not signal" outcome so callers can
// distinguish a deliberate, authenticated refusal (identity mismatch,
// PID reuse, wrong exe-path, would-shoot-the-leader, self) from an
// infrastructure error. Surface-don't-silo: a refusal is always logged
// with the concrete reason, never a silent no-op.
var ErrKillRefused = errors.New("coord.KillCoordIfIdentityMatches: refused (identity gate failed)")

// KillDeps are the injectable seams that keep the primitive
// deterministic under test (no real signals, no real clock, no sysctl).
// Production callers use DefaultKillDeps().
type KillDeps struct {
	// Signal sends sig to pid. Production: syscall.Kill.
	Signal func(pid int, sig syscall.Signal) error
	// Alive reports whether pid is still a running process (signal-0
	// probe). Production: a syscall.Kill(pid,0)==nil check.
	Alive func(pid int) bool
	// PidStart returns pid's start time + ok (PID-reuse-safe). Production:
	// coordlock.PidStartNanos.
	PidStart func(pid int) (int64, bool)
	// ListRecords returns the live agent records. Production: agent.List.
	ListRecords func() ([]*agent.Record, error)
	// CurrentLeaderPID returns the current ACTIVE lease owner pid for the
	// project (epoch gate). Production: coordlock.CurrentActiveOwnerPID.
	CurrentLeaderPID func(project string) (int, bool)
	// IsCoordRunBinary reports whether exePath is the fleet coord-run
	// supervisor binary (NOT the engine child). Production:
	// isFleetCoordRunBinary.
	IsCoordRunBinary func(exePath string) bool
	// Self is the caller's own pid (never kill self). Production:
	// os.Getpid.
	Self func() int
	// Grace is the lock-delay before SIGTERM so a briefly-paused coord
	// self-demotes first (Consul lock-delay). Production: killGraceDelay.
	Grace time.Duration
	// KillTimeout bounds the SIGTERM->SIGKILL escalation poll.
	KillTimeout time.Duration
	// Sleep is the poll sleep between liveness checks (injectable so tests
	// don't wall-clock wait). Production: time.Sleep.
	Sleep func(time.Duration)
	// Stderr receives the reaped-coord log + refusal diagnostics
	// (surface-don't-silo). nil = os.Stderr.
	Stderr io.Writer
}

// killGraceDelay is the default lock-delay before the first SIGTERM.
const killGraceDelay = 2 * time.Second

// killEscalateTimeout bounds SIGTERM -> SIGKILL.
const killEscalateTimeout = 10 * time.Second

// DefaultKillDeps returns the production seams.
func DefaultKillDeps() KillDeps {
	return KillDeps{
		Signal: func(pid int, sig syscall.Signal) error { return syscall.Kill(pid, sig) },
		Alive: func(pid int) bool {
			if pid <= 0 {
				return false
			}
			return syscall.Kill(pid, 0) == nil
		},
		PidStart:         coordlock.PidStartNanos,
		ListRecords:      agent.List,
		CurrentLeaderPID: coordlock.CurrentActiveOwnerPID,
		IsCoordRunBinary: isFleetCoordRunBinary,
		Self:             os.Getpid,
		Grace:            killGraceDelay,
		KillTimeout:      killEscalateTimeout,
		Sleep:            time.Sleep,
	}
}

// KillCoordIfIdentityMatches is the production entry point — it reaps a
// stale coord supervisor through the authenticated gate using the
// production seams. See killCoord for the gate logic.
func KillCoordIfIdentityMatches(target KillTarget) error {
	return killCoord(target, DefaultKillDeps())
}

// killCoord is the seam-injected core. It SIGNALS only when every gate
// clause holds; any failing clause (or unreadable fact) returns a
// wrapped ErrKillRefused / infra error WITHOUT signaling. Caller never
// has to guess whether a kill happened: nil == reaped (or already gone),
// non-nil == refused/failed and surfaced.
func killCoord(target KillTarget, d KillDeps) error {
	d = fillKillDeps(d)
	stderr := d.Stderr

	if target.Pid <= 0 {
		return fmt.Errorf("%w: non-positive target pid %d (project %q)",
			ErrKillRefused, target.Pid, target.Project)
	}
	if target.Project == "" {
		return fmt.Errorf("%w: empty target project (pid %d)", ErrKillRefused, target.Pid)
	}

	// Clause 5 (cheap, do first): never shoot self. A bug that records the
	// supervisor's own pid as a competitor must never become a self-kill.
	if target.Pid == d.Self() {
		return fmt.Errorf("%w: target pid %d == self", ErrKillRefused, target.Pid)
	}

	// Re-read the live agent record set and find the coord whose
	// SupervisorPID matches the target. This is the "re-validate from
	// facts recorded at spawn" step — we do NOT trust the target's own
	// fields except as the lookup key + the PID-reuse cross-check.
	recs, err := d.ListRecords()
	if err != nil {
		return fmt.Errorf("coord.KillCoordIfIdentityMatches: list records: %w", err)
	}
	var match *agent.Record
	for _, r := range recs {
		if r == nil {
			continue
		}
		if r.SupervisorPID == target.Pid && r.Project == target.Project {
			match = r
			break
		}
	}
	if match == nil {
		// No live record names this supervisor pid for this project. The
		// coord was already archived (it exited / was reaped) — the flock
		// is free and nothing to kill. NOT a refusal: a benign no-op.
		_, _ = fmt.Fprintf(stderr,
			"coord.KillCoordIfIdentityMatches: no live record for supervisor pid=%d project=%q (already gone); nothing to reap\n",
			target.Pid, target.Project)
		return nil
	}

	// Clause 1: project match (redundant with the lookup filter, but kept
	// explicit so a future refactor of the lookup can't silently drop it).
	if match.Project != target.Project {
		return fmt.Errorf("%w: record project %q != target project %q (pid %d)",
			ErrKillRefused, match.Project, target.Project, target.Pid)
	}

	// Clause 3: exe-path must be the fleet coord-run supervisor binary,
	// never the engine child. A target whose recorded exe-path is the
	// engine (or empty / unknown) is refused — killing the engine child
	// pid would NOT release the kernel flock and could shoot an unrelated
	// process (W9).
	if !d.IsCoordRunBinary(match.SupervisorExePath) {
		return fmt.Errorf("%w: supervisor exe_path %q is not the fleet coord-run binary (pid %d agent %s)",
			ErrKillRefused, match.SupervisorExePath, target.Pid, match.ID)
	}

	// Clause 2: PID-reuse guard. The live process at SupervisorPID must
	// still have the start time recorded at spawn AND the start time the
	// caller fenced. A recycled pid reads a different start time -> refuse
	// (T36). A dead pid -> already gone (benign no-op, like match==nil).
	liveStart, ok := d.PidStart(target.Pid)
	if !ok {
		_, _ = fmt.Fprintf(stderr,
			"coord.KillCoordIfIdentityMatches: supervisor pid=%d (agent %s) already dead; nothing to reap\n",
			target.Pid, match.ID)
		return nil
	}
	if match.SupervisorPidStart != 0 && liveStart != match.SupervisorPidStart {
		return fmt.Errorf(
			"%w: PID REUSE — live pid_start %d != recorded supervisor_pid_start %d (pid %d agent %s)",
			ErrKillRefused, liveStart, match.SupervisorPidStart, target.Pid, match.ID)
	}
	if target.PidStart != 0 && liveStart != target.PidStart {
		return fmt.Errorf(
			"%w: PID REUSE — live pid_start %d != fenced target pid_start %d (pid %d agent %s)",
			ErrKillRefused, liveStart, target.PidStart, target.Pid, match.ID)
	}

	// Clause 4: epoch gate. Never shoot the CURRENT active lease owner.
	// Once we hold the lease, any other coord supervisor for this project
	// is stale by construction; but if the target IS the recorded active
	// owner, killing it is killing the live leader -> refuse.
	if leaderPid, hasLeader := d.CurrentLeaderPID(target.Project); hasLeader && leaderPid == target.Pid {
		return fmt.Errorf(
			"%w: target pid %d IS the current active lease owner for %q (would shoot the live leader)",
			ErrKillRefused, target.Pid, target.Project)
	}

	// All gates passed. Grace delay (Consul lock-delay) so a briefly-
	// paused coord self-demotes before we signal, then SIGTERM ->
	// poll -> SIGKILL.
	if d.Grace > 0 {
		d.Sleep(d.Grace)
	}
	// Re-check liveness after the grace delay: the coord may have already
	// self-demoted + exited. If so, nothing to do (benign).
	if !d.Alive(target.Pid) {
		_, _ = fmt.Fprintf(stderr,
			"coord.KillCoordIfIdentityMatches: supervisor pid=%d (agent %s) exited during grace delay; nothing to reap\n",
			target.Pid, match.ID)
		return nil
	}

	if err := d.Signal(target.Pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("coord.KillCoordIfIdentityMatches: SIGTERM pid %d: %w", target.Pid, err)
	}
	deadline := time.Now().Add(d.KillTimeout)
	for time.Now().Before(deadline) {
		if !d.Alive(target.Pid) {
			break
		}
		d.Sleep(50 * time.Millisecond)
	}
	if d.Alive(target.Pid) {
		if err := d.Signal(target.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("coord.KillCoordIfIdentityMatches: SIGKILL pid %d: %w", target.Pid, err)
		}
	}

	_, _ = fmt.Fprintf(stderr, "reaped coord pid=%d agent=%s epoch=%d\n",
		target.Pid, match.ID, target.FencerEpoch)
	return nil
}

// fillKillDeps backfills any nil seam with its production default so a
// partially-specified KillDeps (common in tests that only override one
// hook) still runs.
func fillKillDeps(d KillDeps) KillDeps {
	def := DefaultKillDeps()
	if d.Signal == nil {
		d.Signal = def.Signal
	}
	if d.Alive == nil {
		d.Alive = def.Alive
	}
	if d.PidStart == nil {
		d.PidStart = def.PidStart
	}
	if d.ListRecords == nil {
		d.ListRecords = def.ListRecords
	}
	if d.CurrentLeaderPID == nil {
		d.CurrentLeaderPID = def.CurrentLeaderPID
	}
	if d.IsCoordRunBinary == nil {
		d.IsCoordRunBinary = def.IsCoordRunBinary
	}
	if d.Self == nil {
		d.Self = def.Self
	}
	if d.Sleep == nil {
		d.Sleep = def.Sleep
	}
	if d.Grace == 0 {
		d.Grace = def.Grace
	}
	if d.KillTimeout == 0 {
		d.KillTimeout = def.KillTimeout
	}
	if d.Stderr == nil {
		d.Stderr = os.Stderr
	}
	return d
}

// isFleetCoordRunBinary reports whether exePath looks like the fleet
// binary that runs the coord-run supervisor. We stamp the supervisor's
// own os.Executable() into SupervisorExePath at startup, so the basename
// is "fleet" (or a test/go-build temp binary). The check is deliberately
// loose on the path (operators install fleet under many prefixes:
// /usr/local/bin, ~/go/bin, brew Cellar, a side-loaded build) and tight
// on "this is NOT the engine child": an empty exe_path, or one whose
// basename is an engine ("claude", "codex", "node", "sh", "bash"), is
// refused. The supervisor is always the fleet binary; the engine child
// never is.
func isFleetCoordRunBinary(exePath string) bool {
	if exePath == "" {
		return false
	}
	base := strings.ToLower(filepath.Base(exePath))
	// Reject known engine / shell basenames outright — these are the
	// child the supervisor spawns, never the supervisor itself.
	switch base {
	case "claude", "codex", "node", "sh", "bash", "zsh", "python", "python3":
		return false
	}
	// Accept the fleet binary by basename. Covers production ("fleet"),
	// go-test binaries ("fleet.test", "coord.test"), and `go run` temp
	// artifacts (which contain "fleet" or end in the package name). We
	// require the basename to contain "fleet" so a stray unrelated binary
	// recorded by a bug is still refused.
	return strings.Contains(base, "fleet")
}
