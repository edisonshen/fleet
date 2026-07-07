// Package coord owns the coord-lifecycle cleanup function invoked on
// every coord shutdown path. See docs/DESIGN-cleanup-fleet-owns-
// resources.md §PR-C for the design.
//
// The "coord" in fleet is a Claude Code session running inside tmux —
// not a long-running Go process. The cleanup function here is invoked
// by the `fleet coord-run` wrapper subcommand (cmd/fleet/coord.go),
// which is the Go-level supervisor that:
//
//  1. Spawns the claude child (or, for tests, sleep/true).
//  2. Installs signal.NotifyContext for SIGTERM + SIGINT so an
//     operator-issued tmux kill-window / Ctrl-C cancels the child
//     context cleanly.
//  3. Has a top-level `defer Cleanup(...)` so cleanup runs on EVERY
//     exit path: clean child exit, signal-killed child, internal
//     panic, child exec failure, etc.
//
// The cleanup itself is three best-effort side-effects, each running
// inside its own `func() { defer recover(); ... }()` block. A panic in
// one (e.g. a buggy tmux killer) does NOT skip the remaining steps —
// this is the load-bearing contract pinned by TestCleanup_PanicViaDefer.
//
// Side-effects (in order):
//
//  1. tmux.Kill(session)  — best-effort; non-fatal if session is
//     already gone or tmux returns an error.
//  2. Archive ~/.fleet/agents/<id>.json → ~/.fleet/agents/archive/
//     <id>-<UTC-YYYYMMDD-HHMMSS>.json. Unconditional UTC suffix per
//     the design spec — distinct from agent.Record.Archive() which
//     suffixes only on collision.
//
// (The coord's identity lives in the coordinator LEASE now — D3, the
// coord-spawn marker is deleted — and the coord-run supervisor releases
// the lease on exit, so Cleanup no longer touches any marker file.)
//
// Per feedback_fleet_owns_its_resources.md (operator postmortem
// 2026-05-21): cleanup is the LAST step of every coord lifecycle, runs
// on happy AND failure paths, and never depends on operator-side
// manual rm / kill.
package coord

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tmux"
)

// Deps holds the injectable side-effect functions Cleanup performs.
// Production callers use Default() to get the real tmux + filesystem
// implementations; tests inject stubs to avoid shelling out to tmux
// and to deliberately panic in one step to exercise the per-step
// defer-recover contract.
type Deps struct {
	// KillTmux is the tmux-session terminator. Best-effort: returned
	// errors are logged (when Stderr is set) but never fatal. Panics
	// are recovered per-step so the remaining cleanup still runs.
	KillTmux func(session string) error
	// Stderr is where best-effort errors / panics from the cleanup steps
	// are logged for operator visibility (per feedback_surface_dont_
	// silo.md — silent silos are the bug). nil = discard.
	Stderr io.Writer
}

// Default returns the production Deps: tmux.Kill for the killer + nil Stderr
// (callers wanting log output pass os.Stderr explicitly).
func Default() Deps {
	return Deps{
		KillTmux: tmux.Kill,
	}
}

// Cleanup runs the coord-exit side-effects: kill tmux session + archive agent
// record. Returns nil for both because each step is best-effort (operator-visible
// errors are logged to deps.Stderr per the surface-don't-silo rule); a non-nil
// error from any step would tempt callers to skip remaining steps, which is
// exactly the bug we're avoiding.
//
// Panic-safety: each step is wrapped in its own
// `func() { defer recover() }()` so a panic in one doesn't unwind
// past the remaining steps. The recovered value is logged to
// deps.Stderr for forensics.
//
// agentID and project must be non-empty (project is retained in the signature +
// guard for caller/API stability even though the coord-spawn marker step that
// used it is gone — D3; the lease Release on exit covers identity teardown now).
func Cleanup(agentID, project string, deps Deps) error {
	if agentID == "" {
		return fmt.Errorf("coord.Cleanup: empty agentID")
	}
	if project == "" {
		return fmt.Errorf("coord.Cleanup: empty project")
	}
	if deps.KillTmux == nil {
		deps.KillTmux = tmux.Kill
	}
	stderr := deps.Stderr
	if stderr == nil {
		stderr = io.Discard
	}

	session := tmux.SessionName(agentID)

	// Step 1: tmux kill. Per-step recover so a panic here doesn't skip
	// the archive + marker steps. tmux.Kill is idempotent (returns nil
	// when the session is already gone), but a transport-level tmux
	// failure between probe and kill IS returned — we swallow it here
	// because the operator can always `tmux kill-session -t <name>`
	// manually if the session truly persists.
	func() {
		defer func() {
			if r := recover(); r != nil {
				_, _ = fmt.Fprintf(stderr, "coord.Cleanup: tmux kill panicked: %v\n", r)
			}
		}()
		if err := deps.KillTmux(session); err != nil {
			_, _ = fmt.Fprintf(stderr, "coord.Cleanup: tmux kill %s: %v\n", session, err)
		}
	}()

	// Step 2: archive agent record with unconditional UTC suffix.
	// Per the design's exact spec: ~/.fleet/agents/archive/<id>-<UTC-ts>.json.
	// We don't call agent.Record.Archive() because its API only adds the
	// timestamp on collision; here we want the timestamp every time so
	// repeated coord restarts on the same agent ID (rare but possible
	// in handoff recovery) accumulate as distinct snapshots.
	func() {
		defer func() {
			if r := recover(); r != nil {
				_, _ = fmt.Fprintf(stderr, "coord.Cleanup: archive panicked: %v\n", r)
			}
		}()
		if err := archiveAgentRecord(agentID); err != nil {
			_, _ = fmt.Fprintf(stderr, "coord.Cleanup: archive %s: %v\n", agentID, err)
		}
	}()

	// (The coord-spawn marker step is gone — D3. The coord-run supervisor
	// releases the coordinator lease on exit, which is the identity teardown
	// the marker removal used to perform.)

	return nil
}

// archiveAgentRecord moves ~/.fleet/agents/<id>.json to
// ~/.fleet/agents/archive/<id>-<UTCYYYYMMDD-HHMMSS>.json. Idempotent
// on a missing live record (Cleanup may be invoked twice — once from
// the signal handler, once from defer in main).
//
// CONCURRENCY (codex PR2 iter-10 [P2]): under FLEET_LEASE_FAILOVER a
// lease-wrapped coord's record is also written by spawn.Spawn's final
// locked merge (engine PID) and by agent.StampSupervisorIdentity
// (supervisor identity). If this archive raced spawn's merge — archiving
// AFTER spawn's locked Load but BEFORE its rec.Write — the final write
// would RESURRECT a live record for a dead/stood-down coord. So the
// stat→rename runs under the SAME per-agent lock (state.LockAgent) those
// writers take: either we archive first (spawn's locked Load then sees
// ErrNotFound → its do-not-resurrect branch) or spawn finalizes first and
// we archive the complete record. Lock failure is non-fatal (best-effort
// cleanup) — fall through and archive unlocked rather than leak the
// record.
func archiveAgentRecord(id string) error {
	if unlock, err := state.LockAgent(id); err == nil {
		defer unlock()
	} else {
		// Surface-don't-silo: the archive proceeds unlocked (better a
		// rare resurrect-race than a leaked live record), but note it.
		fmt.Fprintf(os.Stderr,
			"coord.Cleanup: archive %s proceeding WITHOUT per-agent lock: %v\n", id, err)
	}
	livePath, err := state.AgentPath(id)
	if err != nil {
		return fmt.Errorf("AgentPath: %w", err)
	}
	if _, err := os.Stat(livePath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Already archived (or never lived). Idempotent: return nil so
			// a second Cleanup call doesn't log this as a failure.
			return nil
		}
		return fmt.Errorf("stat live record: %w", err)
	}
	// Unconditional UTC suffix per PR-C spec.
	ts := time.Now().UTC().Format("20060102-150405")
	archivePath, err := state.AgentArchivePath(id + "-" + ts)
	if err != nil {
		return fmt.Errorf("AgentArchivePath: %w", err)
	}
	// Bootstrap the archive dir's parent (~/.fleet/agents/archive/) —
	// state.Bootstrap creates ~/.fleet/agents/ but the archive subdir
	// is laid down lazily on first archive. Avoids ENOENT from rename.
	if err := os.MkdirAll(filepath.Dir(archivePath), 0o755); err != nil {
		return fmt.Errorf("mkdir archive dir: %w", err)
	}
	if err := os.Rename(livePath, archivePath); err != nil {
		return fmt.Errorf("rename live→archive: %w", err)
	}
	return nil
}
