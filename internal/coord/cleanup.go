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
//  3. Remove ~/.fleet/projects/<project>/.locks/coord-spawn-marker
//     ONLY if its body equals the agent ID. A marker with a different
//     body belongs to a different coord and is preserved verbatim.
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
	"strings"
	"time"

	"github.com/edisonshen/fleet/internal/coordlock"
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
	// AcquireSpawnLock takes the per-project coord-spawn NB-flock for
	// the marker compare-and-remove step. Production uses
	// coordlock.Acquire (serializes against dispatch + handoffop spawn
	// paths so a replacement coord writing the marker can't race the
	// outgoing coord's unlink — codex iter-2 [P1] fix). Tests inject a
	// stub that returns a noop release to skip the real flock without
	// shelling out, or that returns os.ErrPermission to simulate the
	// "replacement is currently spawning" contended case.
	//
	// Returns (release, err). On err != nil the marker step is SKIPPED
	// (preserving the marker is safer than racing the replacement's
	// write — the replacement owns the marker now). release is invoked
	// in a defer after the marker step succeeds; nil release is treated
	// as a no-op.
	AcquireSpawnLock func(project string) (release func(), err error)
	// Stderr is where best-effort errors / panics from the three steps
	// are logged for operator visibility (per feedback_surface_dont_
	// silo.md — silent silos are the bug). nil = discard.
	Stderr io.Writer
}

// Default returns the production Deps: tmux.Kill for the killer +
// coordlock.Acquire for the marker compare-and-remove + nil Stderr
// (callers wanting log output pass os.Stderr explicitly).
func Default() Deps {
	return Deps{
		KillTmux:         tmux.Kill,
		AcquireSpawnLock: coordlock.Acquire,
	}
}

// Cleanup runs the three coord-exit side-effects: kill tmux session,
// archive agent record, clear marker. Returns nil for all three
// because each step is best-effort (operator-visible errors are
// logged to deps.Stderr per the surface-don't-silo rule); a non-nil
// error from any step would tempt callers to skip remaining steps,
// which is exactly the bug we're avoiding.
//
// Panic-safety: each step is wrapped in its own
// `func() { defer recover() }()` so a panic in one doesn't unwind
// past the remaining steps. The recovered value is logged to
// deps.Stderr for forensics.
//
// agentID and project must be non-empty. The function fast-fails on
// empty inputs because (a) an empty agentID would search for the
// wrong record path, and (b) an empty project would skip the marker
// step silently, leaving stale markers behind.
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
	if deps.AcquireSpawnLock == nil {
		deps.AcquireSpawnLock = coordlock.Acquire
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

	// Step 3: remove marker IFF its body matches our agent ID — under
	// the coord-spawn lock so a replacement coord's marker write can't
	// race our read-check-remove. (Codex iter-2 [P1].)
	//
	// Race we're closing:
	//   T0 cleanup reads marker, sees body == oldAgentID  ← old coord
	//   T1 replacement coord writes new agent ID to marker
	//   T2 cleanup unconditionally unlinks marker         ← bug:
	//         deletes the replacement's marker, dashboard/rc lookup
	//         loses the live coord pointer.
	//
	// Fix: hold the same NB-flock that dispatch.go + handoffop.go take
	// around their spawn → marker-write tuple. If we can't acquire
	// (replacement is in flight RIGHT NOW), SKIP the marker step
	// entirely — the replacement will overwrite the marker with its
	// own ID and own its own future cleanup. Preserving the marker on
	// contention is strictly safer than racing the writer.
	func() {
		defer func() {
			if r := recover(); r != nil {
				_, _ = fmt.Fprintf(stderr, "coord.Cleanup: marker panicked: %v\n", r)
			}
		}()
		release, err := deps.AcquireSpawnLock(project)
		if err != nil {
			// Contended → replacement coord is spawning right now and
			// holds the marker's authoritative writer. Skip our
			// removal; the replacement owns the marker lifecycle.
			_, _ = fmt.Fprintf(stderr,
				"coord.Cleanup: marker step skipped — coord-spawn lock contended: %v\n", err)
			return
		}
		defer release()
		if err := clearMarkerIfMatched(project, agentID); err != nil {
			_, _ = fmt.Fprintf(stderr, "coord.Cleanup: marker %s: %v\n", project, err)
		}
	}()

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

// clearMarkerIfMatched removes the project's coord-spawn-marker iff
// its body equals agentID. Other-body markers (a different live coord
// claimed the project after we wrote ours) are preserved. Missing
// marker is a no-op.
//
// Concurrency contract (codex iter-4 [P1]): outside the coord-spawn
// lock there are several marker WRITERS that this function races
// against:
//
//   - cmd/fleet/handoff.go (3 sites): eager swap marker + rollback.
//   - cmd/fleet/dispatch.go: spawn-time marker (inside coordlock).
//   - internal/handoffop/{handoffop,atomic_coord_swap,replacement_
//     cleanup}.go: drain-path marker writes.
//   - internal/tui/keys.go: post-spawn marker (now lock-wrapped per
//     iter-3, but historically not).
//
// Even though the caller (coord.Cleanup) holds the coord-spawn lock,
// not every writer above honors it. A naive read-check-unlink can
// still see the OLD body, get preempted, then unlink the NEW marker
// a non-cooperative writer just dropped in.
//
// Fix: ATOMIC capture via rename. We rename the marker to a
// process-unique side path (a single atomic syscall — no concurrent
// writer can see the marker between our rename and their write because
// the path no longer points at our renamed inode). Then we inspect the
// captured body. If body == agentID we delete the side file (the
// removal is now safe — we own that inode). If body != agentID we
// rename it back so the marker is restored verbatim. Concurrent
// writers using state.WriteCoordSpawnMarker (WriteAtomic → temp +
// rename) create a fresh marker; we never touch theirs.
//
// Race walkthrough:
//
//	T0 cleanup rename(marker, side)         ← atomic capture
//	T1 [writer in flight, e.g. handoff.go]
//	   rename(temp.NNN, marker)              ← writes NEW marker
//	   (cleanup's `side` has OLD content;
//	    the path now has NEW content from writer.)
//	T2 cleanup reads side, body == oldID    ← matches; safe to delete
//	T3 cleanup unlink(side)                  ← deletes OLD content only;
//	   NEW marker at the canonical path is intact.
//
// If T1 doesn't happen (no concurrent writer), cleanup's unlink-side
// is observationally identical to unlink-marker.
//
// Idempotency: if the side file already exists from a crashed prior
// Cleanup, we fail-open (treat as already-cleared) rather than
// stomping on someone else's recovery state.
func clearMarkerIfMatched(project, agentID string) error {
	markerPath, err := state.CoordSpawnMarkerPath(project)
	if err != nil {
		return fmt.Errorf("CoordSpawnMarkerPath: %w", err)
	}
	// Side path is process+PID+nanos-unique so two concurrent
	// Cleanup calls (different agentIDs same project) don't collide.
	sidePath := fmt.Sprintf("%s.clearing.%d.%d",
		markerPath, os.Getpid(), time.Now().UnixNano())
	// Atomic capture. ENOENT = marker already gone (idempotent).
	if err := os.Rename(markerPath, sidePath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("rename marker→side: %w", err)
	}
	// We own sidePath now. Read body via the captured file.
	data, rerr := os.ReadFile(sidePath)
	body := ""
	if rerr == nil && len(data) > 0 {
		// Match the same first-line whitespace-trimmed parse
		// state.ReadCoordSpawnMarker performs.
		body = trimFirstLine(string(data))
	}
	if body == "" || body == agentID {
		// Matched (or unreadable, which collapses to the "best-effort
		// clear" branch — leaving an unreadable file behind is worse
		// than removing it). Unlink the captured side file. Live
		// marker path is intact (concurrent writer may have written
		// a fresh one).
		if err := os.Remove(sidePath); err != nil {
			return fmt.Errorf("remove side: %w", err)
		}
		return nil
	}
	// Body belongs to a different coord — restore the captured marker
	// IF AND ONLY IF the canonical marker path is still empty. Codex
	// iter-5 [P1]: a naive os.Rename(side, marker) would CLOBBER a
	// NEW marker that a concurrent writer dropped in between our
	// capture and our restore. os.Link is atomic + fails with EEXIST
	// when the destination is already populated — exactly the
	// non-overwrite semantic we want. (POSIX has no portable
	// RENAME_NOREPLACE; link+unlink is the standard workaround.)
	if err := os.Link(sidePath, markerPath); err != nil {
		if errors.Is(err, os.ErrExist) {
			// Writer beat us to it — a NEW marker is at the canonical
			// path. Leave it. Drop our captured stale copy.
			if rerr := os.Remove(sidePath); rerr != nil {
				return fmt.Errorf("remove side after writer-win: %w", rerr)
			}
			return nil
		}
		// Genuine error — best-effort scrub of the side file so it
		// doesn't accumulate in .locks/. Marker is lost in this
		// pathological case; surface rather than silo.
		_ = os.Remove(sidePath)
		return fmt.Errorf("restore non-matching marker: %w (side file removed; live marker lost)", err)
	}
	// Link succeeded — now drop the side path (the inode lives on at
	// markerPath via the link).
	if err := os.Remove(sidePath); err != nil {
		return fmt.Errorf("remove side after restore: %w", err)
	}
	return nil
}

// trimFirstLine returns the first line of s with surrounding
// whitespace stripped. Mirrors state.ReadCoordSpawnMarker's parse so
// our match check sees the same body the production reader sees.
func trimFirstLine(s string) string {
	i := strings.IndexByte(s, '\n')
	if i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
