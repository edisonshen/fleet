package rc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/spawn"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tmux"
	"github.com/edisonshen/fleet/internal/workers"
)

// Outcome strings — stable wire contract for cmd/fleet/rc.go JSON
// envelopes + skill consumers. Mirrors fleet claims outcome enum.
//
// Exit codes (cmd/fleet maps these):
//
//	acquired/already_acquired/released/already_released/connected → 0
//	not_enabled / not_owned → 10
//	absent → 11
//	contested → 12
//	error → 1
const (
	OutcomeAcquired        = "acquired"
	OutcomeAlreadyAcquired = "already_acquired"
	OutcomeReleased        = "released"
	OutcomeAlreadyReleased = "already_released"
	OutcomeNotEnabled      = "not_enabled"
	// OutcomeNotOwned has no producer left (its emitter was the
	// retired Connect path); kept as a reserved wire-compat value so
	// external scripts matching the documented exit-code table don't
	// break.
	OutcomeNotOwned  = "not_owned"
	OutcomeAbsent    = "absent"
	OutcomeContested = "contested"
	OutcomeError     = "error"

	// OutcomeNativeDefault — emitted by the deprecated `fleet rc
	// connect` surface. The native model attaches RC at coord spawn
	// via the baked-in `--remote-control` flag, so there is nothing
	// for connect to drive; the CLI reports this outcome (exit 0)
	// with a diagnostic pointing at the native flow.
	OutcomeNativeDefault = "native_default"
)

// UpOpts carries flags for Up.
type UpOpts struct {
	// RespawnOnly is the legacy coord-tick compatibility flag. Pre-
	// native coordinator skill copies shell out `fleet rc up <p>
	// --respawn-only --idempotent` every 30s expecting the v0.12/v0.13
	// listener-respawn behavior. The native model NEVER spawns a
	// listener; Up with RespawnOnly is a pure no-op that reports a
	// stable outcome (not_enabled when the project is opted out,
	// already_acquired otherwise) and touches nothing on disk —
	// keeping half-upgraded installs (old skill copy + new binary)
	// storm-free.
	RespawnOnly bool
}

// State is the snapshot Status / Inspect return. Mirrors the
// design doc §"Resource shape" State struct.
type State struct {
	Project       string    `json:"project"`
	Enabled       bool      `json:"enabled"`
	ListenerPID   int       `json:"listener_pid"`
	HostID        string    `json:"host_id,omitempty"`
	WorkingDir    string    `json:"working_dir,omitempty"`
	SessionPrefix string    `json:"session_prefix,omitempty"`
	LastSpawnAt   time.Time `json:"last_spawn_at,omitempty"`
	LastError     string    `json:"last_error,omitempty"`
	Alive         bool      `json:"alive"`
	// ClaudeVersion + OwningCoordID surface the schema v2 fields from
	// RecordedState so operators can see "what version is this daemon"
	// + "what coord owns it" via `fleet rc status --json`. Empty on
	// legacy v1 records that haven't been re-acquired since the schema
	// bump. leak-rc-daemon-lifecycle PR-B.
	ClaudeVersion string `json:"claude_version,omitempty"`
	OwningCoordID string `json:"owning_coord_id,omitempty"`
}

// Enabled reports whether remote control is on for project. THIS IS
// THE SINGLE SOURCE OF TRUTH for "should fleet bake --remote-control
// into this project's coord spawn argv". Every attach surface calls
// this helper.
//
// Native model semantics — opt-OUT, default-on:
//
//	project == ""            → false (legacy / untargeted dispatch —
//	                           no project to key an opt-out on, so
//	                           stay conservative and skip the flag)
//	invalid project name     → false (cannot have a marker; mirrors
//	                           the empty-project posture)
//	rc-disabled marker found → false (operator ran `fleet rc down`)
//	otherwise                → true (the default)
//
// Cheap: one stat. Best-effort: marker stat errors collapse to
// "no opt-out" (fail-open to the default).
func Enabled(project string) bool {
	if project == "" {
		return false
	}
	if _, err := state.ProjectDir(project); err != nil {
		return false
	}
	return !DisabledMarkerPresent(project)
}

// GateAttachFlag is the project-aware wrapper internal/handoffop uses
// (and any future caller that bakes --remote-control onto a claude
// argv). When Enabled(project) is false (empty project, invalid name,
// or rc-disabled opt-out marker present) OR the
// FLEET_RC_BOOTSTRAP_DISABLED env-gate is set (defense-in-depth from
// PR #157; keeps tests flag-free), GateAttachFlag returns argv
// unchanged. Otherwise it delegates to spawn.InjectRemoteControlFlag.
func GateAttachFlag(project string, argv []string, sessionName string) []string {
	if os.Getenv("FLEET_RC_BOOTSTRAP_DISABLED") != "" {
		return argv
	}
	if !Enabled(project) {
		return argv
	}
	return spawn.InjectRemoteControlFlag(argv, sessionName)
}

// LockPath returns the per-project NB-flock target. Lives at
// ~/.fleet/claims-locks/rc-<safe>.lock so it's siblings to the
// dispatch-claims lock tree but isolated by the `rc-` prefix
// (claims-locks/ is created lazily here; bootstrap doesn't know
// about it yet — that lands with the dispatch-lifecycle PR2).
func LockPath(project string) (string, error) {
	root, err := state.Root()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, "claims-locks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("rc.LockPath: mkdir: %w", err)
	}
	return filepath.Join(dir, "rc-"+state.SafeLockComponent(project)+".lock"), nil
}

// withLock acquires the per-project NB-flock and runs fn while
// holding it. Returns OutcomeContested + nil error if another
// invocation holds the lock — this is the "loser sees
// already_enabled" path (per design §"Failure modes / acceptance").
//
// NB: we use LOCK_EX|LOCK_NB. fn must not call withLock recursively
// (would self-deadlock if Flock were blocking; non-blocking returns
// EWOULDBLOCK, which we report as contested).
func withLock(project string, fn func() (string, error)) (string, error) {
	path, err := LockPath(project)
	if err != nil {
		return OutcomeError, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return OutcomeError, fmt.Errorf("rc.withLock: open: %w", err)
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return OutcomeContested, nil
		}
		return OutcomeError, fmt.Errorf("rc.withLock: flock: %w", err)
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	return fn()
}

// Up enables remote control for project by removing the rc-disabled
// opt-out marker. Idempotent. Native model: Up NEVER spawns a
// standalone listener — the --remote-control flag is baked into the
// coord's own claude argv at spawn time, so enabling takes effect on
// the NEXT coord spawn (fresh dispatch or handoff replacement).
//
// Outcomes:
//
//	acquired         — rc-disabled marker removed (was opted out)
//	already_acquired — project was already enabled (the default)
//	not_enabled      — RespawnOnly call on an opted-out project
//	contested        — another rc operation holds the project lock
func Up(project string, opts UpOpts) (string, error) {
	if project == "" {
		return OutcomeError, errors.New("rc.Up: empty project")
	}
	return withLock(project, func() (string, error) {
		if opts.RespawnOnly {
			// Legacy coord-tick compat (see UpOpts.RespawnOnly): pure
			// no-op, stable outcome, zero filesystem mutation. The old
			// skill's tick loop latches quiet on not_enabled (exit 10)
			// and treats already_acquired (exit 0) as "nothing to do".
			if DisabledMarkerPresent(project) {
				return OutcomeNotEnabled, nil
			}
			return OutcomeAlreadyAcquired, nil
		}
		if _, err := state.EnsureProjectInitialized(project); err != nil {
			return OutcomeError, err
		}
		if !DisabledMarkerPresent(project) {
			return OutcomeAlreadyAcquired, nil
		}
		if err := RemoveDisabledMarker(project); err != nil {
			return OutcomeError, err
		}
		return OutcomeAcquired, nil
	})
}

// healReason* are the labels for the legacy-daemon sweep rubric
// (SweepAllProjects / reapStaleLegacyDaemon). computeHealReason
// returns one of these (or empty for "leave the daemon alone").
const (
	healReasonStaleVersion = "stale claude version"
	healReasonDeadOwner    = "owning coord is gone"
)

// computeHealReason inspects a recorded LEGACY daemon's state against
// the current claude binary version + owner liveness. Returns one of
// the healReason* strings (the daemon should be reaped), or empty
// (leave it alone — a live pre-native coord may still be paired
// through it; it decays naturally once its owner goes away).
//
// Empty recorded ClaudeVersion is treated as "always stale" so legacy
// v1 records get reaped on the first sweep.
//
// Empty OwningCoordID skips the dead-owner check (legacy or
// manually-invoked records without a coord hint).
//
// Probe failures collapse to "healthy" (no reap) — better to leak
// briefly than to kill a healthy daemon on a transient probe error.
//
// curVer is the current `claude --version` PROBED ONCE BY THE CALLER (codex
// P2): SweepAllProjects iterates many projects and reapStaleLegacyDaemon
// re-checks under the lock — probing per call shelled out to claude N+ times
// per `fleet status`, and a hung claude could stall status/JSON callers.
// The caller probes once and threads the result here. curVer == "" means
// the probe was unavailable/empty → skip the version check (collapse to
// healthy on that axis), exactly as the old err/empty path did.
func computeHealReason(cur RecordedState, curVer string) string {
	// Version check.
	if curVer != "" {
		// Legacy v1 record (empty recorded version) → force heal so the
		// backfill happens once, on the next Up tick. Mismatch → heal.
		if cur.ClaudeVersion == "" || cur.ClaudeVersion != curVer {
			return healReasonStaleVersion
		}
	}
	// Dead-owner check (skip on empty recorded owner).
	if cur.OwningCoordID != "" && !ownerAliveFn(cur.OwningCoordID) {
		return healReasonDeadOwner
	}
	return ""
}

// probeClaudeVersion runs the version probe once and collapses any error
// to "" (unavailable). Callers pass the result into computeHealReason so a
// single sweep/up does at most ONE claude --version shell-out.
func probeClaudeVersion() string {
	v, err := claudeVersionFn()
	if err != nil {
		return ""
	}
	return v
}

// Down disables remote control for project: writes the rc-disabled
// opt-out marker (suppresses the --remote-control flag on future
// coord spawns) and reaps any LEGACY standalone listener Fleet still
// owns (kills the recorded PID after strict verification, removes the
// legacy rc-enabled marker + rc-state.json). Idempotent. Returns
// already_released when the project was already opted out and no
// legacy artifacts remained.
//
// Note: a coord that is ALREADY running with --remote-control keeps
// its RC session until it exits/hands off — Down can't (and doesn't
// try to) strip a flag from a live process. The opt-out takes effect
// on the next spawn.
//
// Per the v0.12 design (codex round 2): we do NOT invoke `claude
// daemon remote-control remove` — that API is for the dir-registry,
// not for live-listener teardown. The local PID kill IS the teardown.
func Down(project string) (string, error) {
	if project == "" {
		return OutcomeError, errors.New("rc.Down: empty project")
	}
	return withLock(project, func() (string, error) {
		hadDisabled := DisabledMarkerPresent(project)
		markerHad := MarkerPresent(project)
		cur, stateErr := ReadState(project)
		stateHad := stateErr == nil
		// codex round-4 P2: distinguish "state file missing" from
		// "state file present but malformed". The latter is exactly
		// the case `fleet rc reset` exists to clean up — if we
		// returned already_released and skipped RemoveState, the
		// operator would be stuck with a corrupt rc-state.json that
		// `fleet rc status` keeps failing on. Surface a stderr
		// diagnostic so they know we cleaned a corrupt file (not
		// just a missing one) before falling through to cleanup.
		stateCorrupt := stateErr != nil && !errors.Is(stateErr, ErrStateMissing)
		if stateCorrupt {
			fmt.Fprintf(os.Stderr,
				"rc.Down: project %q has malformed rc-state.json (%v); removing it as part of teardown\n",
				project, stateErr)
		}

		if !markerHad && !stateHad && !stateCorrupt {
			// No legacy artifacts. The opt-out marker is the only
			// mutation needed.
			if hadDisabled {
				return OutcomeAlreadyReleased, nil
			}
			if _, err := state.EnsureProjectInitialized(project); err != nil {
				return OutcomeError, err
			}
			if err := WriteDisabledMarker(project); err != nil {
				return OutcomeError, err
			}
			return OutcomeReleased, nil
		}

		// Kill local PID if state says Fleet owns one. Cross-host
		// refusal: if host_id doesn't match, log + skip the kill (we
		// can't safely signal a remote-PID). Marker still gets
		// removed locally so the gate fires off.
		//
		// codex P1 (PID-reuse defense): between the listener exiting
		// and Down running, the kernel can recycle the PID for an
		// unrelated process (a Make build, a different daemon). We
		// MUST NOT SIGTERM/SIGKILL such a process. Before signaling,
		// verify the PID's argv still matches our recorded
		// session_prefix. If it doesn't (or the probe fails), skip
		// the kill and fall through to marker/state cleanup — better
		// to leak a now-dead listener for the sweeper than to murder
		// the user's terminal multiplexer.
		if stateHad && cur.PID > 0 {
			host, _ := os.Hostname()
			if cur.HostID == "" || cur.HostID == host {
				if workers.IsAlive(cur.PID) {
					prefix := cur.SessionPrefix
					if prefix == "" {
						prefix = SessionPrefix
					}
					if verifyPIDIsListener(cur.PID, prefix, cur.WorkingDir) {
						killFn(cur.PID)
					} else {
						// argv/cwd mismatch — most likely the PID was
						// recycled, possibly by another project's
						// fleet-coord listener (every project shares
						// the prefix). Refusing to kill avoids
						// terminating an unrelated listener.
						fmt.Fprintf(os.Stderr,
							"rc.Down: project %q recorded PID %d alive but argv/cwd does not match recorded session_prefix %q + working_dir %q; skipping kill (likely PID reuse — possibly by another project's listener)\n",
							project, cur.PID, prefix, cur.WorkingDir)
					}
				}
			}
			// Cross-host: silently skip kill, fall through to marker
			// removal. Sweeper will catch the orphan on the other
			// host.
		}

		if err := RemoveMarker(project); err != nil {
			return OutcomeError, err
		}
		if err := RemoveState(project); err != nil {
			return OutcomeError, err
		}
		if _, err := state.EnsureProjectInitialized(project); err != nil {
			return OutcomeError, err
		}
		if err := WriteDisabledMarker(project); err != nil {
			return OutcomeError, err
		}
		return OutcomeReleased, nil
	})
}

// Inspect returns the observed State for project. Read-only — no
// lock acquired. Suitable for the dashboard / `fleet rc status`.
// Enabled reflects the native opt-out gate; the listener fields are
// LEGACY observability (populated only while a pre-native standalone
// daemon's rc-state.json is still on disk awaiting sweep).
func Inspect(project string) (State, error) {
	s := State{Project: project, Enabled: Enabled(project)}
	cur, err := ReadState(project)
	if err == nil {
		s.ListenerPID = cur.PID
		s.HostID = cur.HostID
		s.WorkingDir = cur.WorkingDir
		s.SessionPrefix = cur.SessionPrefix
		s.LastSpawnAt = cur.LastSpawnAt
		s.LastError = cur.LastError
		s.Alive = cur.PID > 0 && workers.IsAlive(cur.PID)
		s.ClaudeVersion = cur.ClaudeVersion
		s.OwningCoordID = cur.OwningCoordID
	} else if !errors.Is(err, ErrStateMissing) {
		return s, err
	}
	return s, nil
}

// ListDisabled enumerates projects with the rc-disabled opt-out
// marker present — i.e. the exceptions to the default-on native
// model. Stable order (directory order) so JSON output is
// reproducible.
func ListDisabled() ([]string, error) {
	return listProjectsWith(DisabledMarkerPresent)
}

// List enumerates projects with the LEGACY rc-enabled marker present.
// Cleanup/observability surface only (Reset enumeration, tests) — the
// native gate no longer reads this marker.
func List() ([]string, error) {
	return listProjectsWith(MarkerPresent)
}

// listProjectsWith walks ~/.fleet/projects/* and returns the project
// names for which pred is true.
func listProjectsWith(pred func(project string) bool) ([]string, error) {
	root, err := state.Root()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(root, "projects"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("rc.List: %w", err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if name == ".locks" || strings.HasPrefix(name, ".") {
			continue
		}
		if pred(name) {
			out = append(out, name)
		}
	}
	return out, nil
}

// Reset cleans project's LEGACY listener state (or all projects when
// project==""): kills any legacy daemon Fleet owns (strict PID
// verification), removes the legacy rc-enabled marker + rc-state.json
// (including corrupt ones — the operator-emergency case Reset exists
// for).
//
// Reset NEVER touches the rc-disabled opt-out marker (/review
// adversarial F4): the opt-out is operator INTENT, not corruptible
// listener state, and under the default-on model `fleet rc down` is
// the push kill-switch — an emergency command must not silently
// re-arm RC on projects the operator disarmed. Use `fleet rc up` to
// clear an opt-out explicitly.
//
// codex round-5 P2 (kept from v0.12): reset-all must enumerate
// legacy-markered projects AND markerless state-only projects (Glob).
func Reset(project string) (string, error) {
	if project == "" {
		seen := map[string]struct{}{}

		// Legacy rc-enabled markers.
		projs, err := List()
		if err != nil {
			return OutcomeError, err
		}
		for _, p := range projs {
			seen[p] = struct{}{}
		}

		// Add markerless state-only projects so emergency cleanup
		// actually catches them.
		if root, err := state.Root(); err == nil {
			matches, _ := filepath.Glob(filepath.Join(root, "projects", "*", "rc-state.json"))
			for _, m := range matches {
				p := filepath.Base(filepath.Dir(m))
				if p == "" || strings.HasPrefix(p, ".") {
					continue
				}
				seen[p] = struct{}{}
			}
		}

		for p := range seen {
			if _, err := resetOne(p); err != nil {
				// Continue; best-effort across all projects.
				_ = err
			}
		}
		return OutcomeReleased, nil
	}
	return resetOne(project)
}

// resetOne is the per-project Reset body: legacy reap + legacy file
// cleanup, with the rc-disabled opt-out marker preserved bit-for-bit
// (present stays present, absent stays absent).
func resetOne(project string) (string, error) {
	if project == "" {
		return OutcomeError, errors.New("rc.resetOne: empty project")
	}
	return withLock(project, func() (string, error) {
		cur, stateErr := ReadState(project)
		stateHad := stateErr == nil
		stateCorrupt := stateErr != nil && !errors.Is(stateErr, ErrStateMissing)
		if stateCorrupt {
			fmt.Fprintf(os.Stderr,
				"rc.Reset: project %q has malformed rc-state.json (%v); removing it as part of cleanup\n",
				project, stateErr)
		}
		// Kill the legacy daemon with the same PID-reuse + cross-host
		// defenses Down uses.
		if stateHad && cur.PID > 0 {
			host, _ := os.Hostname()
			if cur.HostID == "" || cur.HostID == host {
				if workers.IsAlive(cur.PID) {
					prefix := cur.SessionPrefix
					if prefix == "" {
						prefix = SessionPrefix
					}
					if verifyPIDIsListener(cur.PID, prefix, cur.WorkingDir) {
						killFn(cur.PID)
					} else {
						fmt.Fprintf(os.Stderr,
							"rc.Reset: project %q recorded PID %d alive but argv/cwd does not match; skipping kill (likely PID reuse)\n",
							project, cur.PID)
					}
				}
			}
		}
		if err := RemoveMarker(project); err != nil {
			return OutcomeError, err
		}
		if err := RemoveState(project); err != nil {
			return OutcomeError, err
		}
		return OutcomeReleased, nil
	})
}

// SweepAllProjects is the cross-project reconcile hook for LEGACY
// standalone listener daemons (pre-native installs). Native model:
// nothing spawns listeners anymore, so every rc-state.json on disk is
// a decaying legacy artifact. The sweep enumerates them and reaps:
//
//   - Dead-PID records: the daemon already exited — remove the stale
//     rc-state.json + legacy rc-enabled marker (pure file cleanup).
//   - Legacy-marker-absent + live PID: operator opted out under the
//     old model but the daemon kept running. Reap it.
//   - Stale-version daemon: recorded ClaudeVersion differs from current
//     `claude --version` (or recorded version is empty / v1 legacy).
//     Old daemons across upgrades — the 2026-05-29 OOM root cause.
//   - Dead-owner daemon: recorded OwningCoordID has no live agent
//     record / dead tmux session. Coord crashed without releasing.
//
// A live daemon with a matching version AND a live owner is left
// alone: a pre-native coord may still be paired through it. It decays
// via the dead-owner class once that coord exits/hands off — no new
// daemon ever replaces it (the v0.12 respawn tick is retired).
//
// Skips cross-host entries (host_id mismatch — unsafe to kill).
//
// Never kills on prefix-only evidence; state.json must claim Fleet
// ownership (codex round 3 free-form).
//
// codex P2 (sweep-source correctness): iterate rc-state.json directly
// via filepath.Glob — marker-filtered listings miss exactly the
// orphans we need to reach.
func SweepAllProjects() error {
	root, err := state.Root()
	if err != nil {
		return err
	}
	matches, err := filepath.Glob(filepath.Join(root, "projects", "*", "rc-state.json"))
	if err != nil {
		return fmt.Errorf("rc.SweepAllProjects: glob: %w", err)
	}
	if len(matches) == 0 {
		return nil // nothing to sweep — never touch claude (codex P2).
	}
	host, _ := os.Hostname()
	// codex P2: probe claude --version LAZILY and at most ONCE. The probe
	// is only needed for the Class 2/3 (marker-present) self-heal version
	// check — NOT for markerless / dead / cross-host entries. On installs
	// with no version-based sweep work, we never run `claude --version`, so
	// a slow/broken claude can't stall a read-only `fleet status`. The
	// closure memoizes the first probe and reuses it for the rest of the
	// sweep + each under-lock re-check.
	versionProbed := false
	curVer := ""
	getVer := func() string {
		if !versionProbed {
			curVer = probeClaudeVersion()
			versionProbed = true
		}
		return curVer
	}
	for _, m := range matches {
		// projects/<name>/rc-state.json — pull the <name> segment.
		p := filepath.Base(filepath.Dir(m))
		if p == "" || strings.HasPrefix(p, ".") {
			continue
		}
		cur, err := ReadState(p)
		if err != nil {
			continue
		}
		if cur.HostID != "" && cur.HostID != host {
			continue
		}
		if cur.PID <= 0 || !workers.IsAlive(cur.PID) {
			// Recorded PID gone — the legacy daemon already exited.
			// Native model: nothing will ever rewrite this record, so
			// remove the stale rc-state.json + legacy marker (pure
			// file cleanup; no signal sent).
			reapDeadLegacyRecord(p, cur)
			continue
		}
		// Class 1: marker absent. Operator opted out manually; daemon
		// kept running. codex P1: this is the DESTRUCTIVE sweep path that
		// now runs on every (read-only) `fleet status`. Down() degrades to
		// argv-only PID verification when lsof is missing, and every project
		// shares the fleet-coord prefix — so a reused stale PID could be
		// another project's HEALTHY listener and status would kill it. Use
		// the strict-cwd-confirmed teardown instead (removes marker + state
		// like Down, but never signals an unverifiable PID).
		if !MarkerPresent(p) {
			reapMarkerlessStrict(p, cur)
			continue
		}
		// Legacy marker present, daemon alive — apply the heal rubric.
		// A stale version or dead owner means the legacy daemon is an
		// orphan: reap it (kill + remove state + remove the legacy
		// marker — nothing respawns under the native model, so there
		// is no respawn expectation to preserve the marker for).
		ver := getVer() // probed lazily on the first heal-rubric entry.
		if reason := computeHealReason(cur, ver); reason != "" {
			fmt.Fprintf(os.Stderr,
				"rc.SweepAllProjects: project %q legacy daemon — %s; reaping PID %d\n",
				p, reason, cur.PID)
			reapStaleLegacyDaemon(p, cur, ver)
		}
	}
	return nil
}

// reapStaleLegacyDaemon kills the recorded local PID (with the same
// PID-reuse defense Down uses) and removes rc-state.json + the legacy
// rc-enabled marker. Native model: nothing respawns listeners, so a
// stale/orphaned legacy daemon is pure leak — full teardown.
//
// codex P1 (re-read under lock, kept from v0.12): SweepAllProjects
// reads the state snapshot WITHOUT the lock; a concurrent rc
// operation (Down/Reset on an old binary, another sweep) can rewrite
// rc-state.json between the snapshot and this call. Re-read under the
// lock and confirm (a) the PID still matches the snapshot and (b)
// computeHealReason still flags it stale; abort otherwise.
//
// Best-effort: errors are swallowed (sweep is fire-and-forget across all
// projects; one bad entry must not abort the rest).
func reapStaleLegacyDaemon(project string, snapshot RecordedState, curVer string) {
	_, _ = withLock(project, func() (string, error) {
		// Re-read under the lock. If state vanished, nothing to reap.
		cur, err := ReadState(project)
		if err != nil {
			return OutcomeAlreadyReleased, nil
		}
		// Race guard: a fresh respawn between snapshot and lock rewrites
		// PID/version/owner. If the current record no longer matches the
		// snapshot PID, or no longer warrants a heal, leave it alone.
		if cur.PID != snapshot.PID {
			fmt.Fprintf(os.Stderr,
				"rc.reapStaleLegacyDaemon: project %q state changed under lock (snapshot PID %d -> current PID %d); aborting reap (likely concurrent respawn)\n",
				project, snapshot.PID, cur.PID)
			return OutcomeAlreadyAcquired, nil
		}
		if computeHealReason(cur, curVer) == "" {
			fmt.Fprintf(os.Stderr,
				"rc.reapStaleLegacyDaemon: project %q no longer stale under lock (PID %d); aborting reap\n",
				project, cur.PID)
			return OutcomeAlreadyAcquired, nil
		}
		host, _ := os.Hostname()
		// codex P2 (cross-host): the caller filters cross-host entries off
		// an UNLOCKED snapshot; the state could be rewritten for another
		// host between snapshot and this locked re-read. We must abort the
		// ENTIRE reap (not just the kill) — falling through to RemoveState
		// would delete another host's LIVE rc-state.json in a shared /
		// migrated FLEET_HOME, leaving its daemon untracked. Only the local
		// host (or an unattributed empty HostID) may be reaped here.
		if cur.HostID != "" && cur.HostID != host {
			fmt.Fprintf(os.Stderr,
				"rc.reapStaleLegacyDaemon: project %q rc-state.json is now owned by host %q (local %q); aborting reap (cross-host — that host's sweep will handle it)\n",
				project, cur.HostID, host)
			return OutcomeAlreadyAcquired, nil
		}
		if cur.PID > 0 && workers.IsAlive(cur.PID) {
			prefix := cur.SessionPrefix
			if prefix == "" {
				prefix = SessionPrefix
			}
			// codex P2 (strict cwd before destructive sweep kill): the
			// auto-sweep runs on every `fleet status`. verifyPIDIsListener
			// degrades to argv-only when lsof is missing, and every project
			// shares the fleet-coord prefix — so a REUSED stale PID could
			// pass argv-only and we'd kill another project's healthy
			// listener. Require an EXACT cwd confirmation here. If we can't
			// strictly verify (lsof missing, cwd unknown, or mismatch), skip
			// BOTH the kill AND the state removal: we can't prove this PID is
			// ours, and removing the state could untrack a live foreign
			// daemon. Leave it for a host/tick that can verify.
			if !verifyPIDIsListener(cur.PID, prefix, cur.WorkingDir) ||
				!verifyPIDCwdStrictFn(cur.PID, cur.WorkingDir) {
				fmt.Fprintf(os.Stderr,
					"rc.reapStaleLegacyDaemon: project %q PID %d could not be strictly verified as our listener (argv/cwd unconfirmed — likely PID reuse or lsof unavailable); skipping reap to avoid killing an unrelated process\n",
					project, cur.PID)
				return OutcomeAlreadyAcquired, nil
			}
			killFn(cur.PID)
		}
		_ = RemoveState(project)
		// Legacy rc-enabled marker is dead weight under the native
		// model — remove it with the daemon (no respawn expectation).
		_ = RemoveMarker(project)
		return OutcomeReleased, nil
	})
}

// reapDeadLegacyRecord removes a stale rc-state.json (and legacy
// rc-enabled marker) whose recorded PID is already dead. Pure file
// cleanup — no signal is ever sent. Native model: nothing rewrites
// these records, so leaving them would leak one stale JSON per
// pre-native project forever.
//
// Same locked re-read discipline as the other reap helpers: abort if
// the record changed (PID differs / now alive) or moved cross-host
// between the unlocked snapshot and the locked re-read.
func reapDeadLegacyRecord(project string, snapshot RecordedState) {
	_, _ = withLock(project, func() (string, error) {
		cur, err := ReadState(project)
		if err != nil {
			return OutcomeAlreadyReleased, nil
		}
		if cur.PID != snapshot.PID {
			return OutcomeAlreadyAcquired, nil
		}
		host, _ := os.Hostname()
		if cur.HostID != "" && cur.HostID != host {
			return OutcomeAlreadyAcquired, nil
		}
		if cur.PID > 0 && workers.IsAlive(cur.PID) {
			// Came back alive under the lock (PID reuse window) —
			// leave it for the live-daemon classes to verify.
			return OutcomeAlreadyAcquired, nil
		}
		_ = RemoveState(project)
		_ = RemoveMarker(project)
		return OutcomeReleased, nil
	})
}

// reapMarkerlessStrict is the legacy-marker-absent sweep teardown. The
// operator opted out (removed the marker) but the daemon kept running, so
// we remove BOTH the marker and the state (full opt-out, like Down) — BUT
// with the same strict cwd confirmation reapStaleLegacyDaemon uses, because
// this path runs on every read-only `fleet status` (codex P1). If we can't
// strictly verify the PID is our listener (lsof missing / cwd mismatch /
// PID reuse), we skip the kill AND leave state alone — a reused PID could
// be another project's healthy listener, and an informational command must
// never terminate it.
func reapMarkerlessStrict(project string, snapshot RecordedState) {
	_, _ = withLock(project, func() (string, error) {
		cur, err := ReadState(project)
		if err != nil {
			return OutcomeAlreadyReleased, nil
		}
		// Race guard: re-read under the lock; if the marker reappeared or
		// the PID changed since the snapshot, this is no longer the same
		// markerless-orphan case — leave it for the appropriate path.
		if MarkerPresent(project) || cur.PID != snapshot.PID {
			return OutcomeAlreadyAcquired, nil
		}
		host, _ := os.Hostname()
		if cur.HostID != "" && cur.HostID != host {
			fmt.Fprintf(os.Stderr,
				"rc.reapMarkerlessStrict: project %q rc-state.json now owned by host %q (local %q); aborting reap (cross-host)\n",
				project, cur.HostID, host)
			return OutcomeAlreadyAcquired, nil
		}
		if cur.PID > 0 && workers.IsAlive(cur.PID) {
			prefix := cur.SessionPrefix
			if prefix == "" {
				prefix = SessionPrefix
			}
			if !verifyPIDIsListener(cur.PID, prefix, cur.WorkingDir) ||
				!verifyPIDCwdStrictFn(cur.PID, cur.WorkingDir) {
				fmt.Fprintf(os.Stderr,
					"rc.reapMarkerlessStrict: project %q PID %d could not be strictly verified as our listener (argv/cwd unconfirmed — likely PID reuse or lsof unavailable); skipping reap to avoid killing an unrelated process\n",
					project, cur.PID)
				return OutcomeAlreadyAcquired, nil
			}
			killFn(cur.PID)
		}
		// Marker already absent (Class-1 precondition); remove state. Call
		// RemoveMarker too for idempotent full teardown (no-op if absent).
		_ = RemoveMarker(project)
		_ = RemoveState(project)
		return OutcomeReleased, nil
	})
}

// IsAlive re-exports workers.IsAlive for rc-package callers (Health,
// external test packages) so they don't need a workers import.
func IsAlive(pid int) bool { return workers.IsAlive(pid) }

// verifyPIDIsListenerFn is the test seam for PID-reuse defense.
// Production uses psArgsVerify (shells out to `ps -p <pid> -o
// args=` + lsof for cwd). Tests stub to return true so unit tests
// don't need a live process whose argv + cwd match.
var verifyPIDIsListenerFn = psArgsVerify

// verifyPIDIsListener returns true iff the OS reports a process at
// pid whose argv contains "claude" + "remote-control" + the
// recorded session_prefix AND whose cwd matches expectedCwd. Both
// checks are required: every project shares the "fleet-coord"
// session_prefix, so argv-only matching can't distinguish project
// A's listener from project B's after PID reuse (codex round-5 P1).
//
// expectedCwd == "" disables the cwd check — only callers without a
// recorded working_dir should pass empty (none in production; some
// test fixtures).
//
// codex P1 mitigation: never adopt or signal a PID without checking
// who owns it now AND that the working_dir matches.
func verifyPIDIsListener(pid int, sessionPrefix, expectedCwd string) bool {
	if pid <= 0 || sessionPrefix == "" {
		return false
	}
	return verifyPIDIsListenerFn(pid, sessionPrefix, expectedCwd)
}

// SetVerifyPIDIsListenerForTest swaps verifyPIDIsListenerFn;
// returns a restore func tests defer.
func SetVerifyPIDIsListenerForTest(fn func(pid int, sessionPrefix, expectedCwd string) bool) func() {
	prev := verifyPIDIsListenerFn
	verifyPIDIsListenerFn = fn
	return func() { verifyPIDIsListenerFn = prev }
}

// psArgsVerify is the production verifier. Two probes:
//  1. `ps -p <pid> -o args=` for argv (portable to macOS/Linux/BSD).
//  2. `lsof -a -p <pid> -d cwd -Fn` for working_dir.
//
// On any probe failure, returns false — the conservative answer:
// "don't trust the PID because we can't prove it's ours."
func psArgsVerify(pid int, sessionPrefix, expectedCwd string) bool {
	out, err := exec.Command("ps", "-p", fmt.Sprintf("%d", pid), "-o", "args=").Output()
	if err != nil {
		return false
	}
	args := strings.TrimSpace(string(out))
	if args == "" {
		return false
	}
	if !strings.Contains(args, "claude") {
		return false
	}
	if !strings.Contains(args, "remote-control") {
		return false
	}
	if !strings.Contains(args, sessionPrefix) {
		return false
	}
	// Empty expectedCwd: legacy / test fixture — accept argv match.
	// Production callers always pass a non-empty value.
	if expectedCwd == "" {
		return true
	}
	cwdOut, err := exec.Command("lsof", "-a", "-p", strconv.Itoa(pid), "-d", "cwd", "-Fn").Output()
	if err != nil {
		// codex round-8 P1 regression-fix: hosts without lsof
		// (minimal Linux containers, locked-down systems) shouldn't
		// hard-fail every adopt/connect/down — that would respawn
		// duplicate listeners on every coord tick. lsof is not a
		// documented runtime dependency. Degrade to argv-only match
		// (still better than the pre-R5 baseline which had no
		// argv check at all). Surface the degraded mode so the
		// operator can install lsof to restore cross-project
		// PID-reuse defense.
		fmt.Fprintf(os.Stderr,
			"rc: lsof unavailable for pid %d (%v); falling back to argv-only PID verify (install lsof to restore cross-project PID-reuse defense)\n",
			pid, err)
		return true
	}
	for _, l := range strings.Split(string(cwdOut), "\n") {
		if strings.HasPrefix(l, "n") && l[1:] == expectedCwd {
			return true
		}
	}
	return false
}

// verifyPIDCwdStrictFn is the test seam for the STRICT cwd verifier used
// by the auto-sweep reap (codex P2). Production uses psCwdStrict.
var verifyPIDCwdStrictFn = psCwdStrict

// psCwdStrict confirms — via lsof ONLY — that pid's working_dir equals
// expectedCwd. Unlike psArgsVerify it does NOT degrade to argv-only when
// lsof is missing: it returns false. The destructive auto-sweep path
// (`fleet status` reaping stale daemons) needs this stricter gate because
// every project shares the fleet-coord prefix, so an argv-only match on a
// REUSED PID could otherwise kill another project's healthy listener.
// Empty expectedCwd → false (can't strictly verify an unknown cwd).
func psCwdStrict(pid int, expectedCwd string) bool {
	if pid <= 0 || expectedCwd == "" {
		return false
	}
	out, err := exec.Command("lsof", "-a", "-p", strconv.Itoa(pid), "-d", "cwd", "-Fn").Output()
	if err != nil {
		return false // lsof unavailable / failed → cannot strictly verify.
	}
	for _, l := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(l, "n") && l[1:] == expectedCwd {
			return true
		}
	}
	return false
}

// SetVerifyPIDCwdStrictForTest swaps verifyPIDCwdStrictFn; returns a
// restore func tests defer.
func SetVerifyPIDCwdStrictForTest(fn func(pid int, expectedCwd string) bool) func() {
	prev := verifyPIDCwdStrictFn
	verifyPIDCwdStrictFn = fn
	return func() { verifyPIDCwdStrictFn = prev }
}

// killFn is the test seam for Down's listener teardown. Production
// implements SIGTERM → 10s poll → SIGKILL. Tests stub to a no-op
// so they don't shoot themselves in the foot when state.json
// records os.Getpid() as the "listener" PID.
var killFn = defaultKill

func defaultKill(pid int) {
	_ = syscall.Kill(pid, syscall.SIGTERM)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !workers.IsAlive(pid) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	if workers.IsAlive(pid) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

// SetKillFnForTest swaps killFn; returns a restore func tests defer.
// Tests use this to keep Down deterministic (no real signals fired).
func SetKillFnForTest(fn func(int)) func() {
	prev := killFn
	killFn = fn
	return func() { killFn = prev }
}

// claudeVersionFn is the test seam for the self-healing version probe.
// Production shells out to `claude --version` (cached for one Up call
// via the package-level cache below). Tests stub to return a fixed
// value so unit tests don't need a `claude` binary on PATH.
//
// leak-rc-daemon-lifecycle PR-B: the recorded daemon's ClaudeVersion
// is compared to the current binary's version on every Up tick; a
// mismatch triggers kill+respawn so the old daemon doesn't outlive
// its claude install.
var claudeVersionFn = defaultClaudeVersion

// defaultClaudeVersion shells out to `claude --version` and returns
// the trimmed first line. Errors collapse to ("", err) — the caller's
// self-healing path falls back to "treat as stale" semantics so a
// broken claude binary doesn't pin a stale daemon in place.
//
// The output shape from current claude CLI is a single line like:
//
//	2.1.156 (Claude Code)
//
// We split on whitespace and take the leading token; anything else is
// treated as "unknown" (empty string).
// claudeVersionProbeTimeout bounds the `claude --version` shell-out so a
// broken/blocking claude wrapper can't hang fleet status / gc (codex P2).
const claudeVersionProbeTimeout = 5 * time.Second

func defaultClaudeVersion() (string, error) {
	bin, err := exec.LookPath("claude")
	if err != nil {
		return "", fmt.Errorf("claude binary not found on PATH: %w", err)
	}
	// codex P2: bound the probe. `fleet status` reaches this via
	// rc.SweepAllProjects on every run; a broken `claude` wrapper that
	// blocks on --version would otherwise hang status/JSON/coord ticks
	// indefinitely. A timeout collapses to "unknown version" (caller treats
	// empty as "skip version check"), which is the safe degraded behavior.
	ctx, cancel := context.WithTimeout(context.Background(), claudeVersionProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("claude --version: %w", err)
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return "", nil
	}
	// First whitespace-delimited token is the semver — anything after
	// (e.g. "(Claude Code)") is metadata.
	if i := strings.IndexAny(line, " \t"); i > 0 {
		return line[:i], nil
	}
	return line, nil
}

// ownerAliveFn is the test seam for the owning-coord liveness probe.
// Production: agent.Load(coordID) succeeds AND its TmuxSession is
// alive on the current FLEET_TMUX_SOCKET. Tests stub to return true
// for known-live IDs / false for known-dead IDs.
//
// leak-rc-daemon-lifecycle PR-B: when an RC daemon's owning coord
// goes away (record archived, tmux session killed), the daemon
// becomes an orphan — Up's self-healing path kills it and respawns
// under the new caller.
var ownerAliveFn = defaultOwnerAlive

// defaultOwnerAlive checks whether the agent record for coordID is
// present AND its TmuxSession is alive on the active tmux server.
//
// Conservative on errors: any probe failure collapses to "alive"
// (return true) so a transient agent-dir read error doesn't trigger a
// kill+respawn of a healthy daemon. The version probe is the load-
// bearing self-heal signal; dead-owner is the secondary case.
//
// Empty coordID is treated as "alive" (skip dead-owner check) so
// legacy v1 records and manual `fleet rc up` invocations without a
// CoordID hint don't get respawned on every tick.
func defaultOwnerAlive(coordID string) bool {
	if coordID == "" {
		return true
	}
	rec, err := agent.Load(coordID)
	if err != nil {
		// state.ErrNotFound is the only definitive dead-owner signal.
		if errors.Is(err, state.ErrNotFound) {
			return false
		}
		// Any other error (corrupt JSON, permission denied) — treat
		// as alive to avoid flapping kill+respawn.
		return true
	}
	if rec == nil || rec.TmuxSession == "" {
		// Legacy record with no tmux session field — can't probe,
		// treat as alive.
		return true
	}
	// codex P2 (wrong-socket false-positive): the agent record EXISTS, so
	// the coord was NOT archived. We could probe tmux.SessionAlive, but it
	// targets the CURRENT FLEET_TMUX_SOCKET and the coord may have been
	// spawned on a DIFFERENT tmux server (env unset/changed between spawn
	// and this probe). Its "no server / no such session" cases return
	// (false, nil), which — taken at face value — would reap a LIVE daemon
	// as dead-owner. Agent records don't persist the spawn socket, so a
	// not-alive result is AMBIGUOUS, not definitive. The ONLY definitive
	// dead-owner signal is a MISSING record (handled above via ErrNotFound).
	// With the record present we treat the owner as alive and lean on the
	// version-mismatch check — the load-bearing self-heal signal — rather
	// than risk killing a healthy listener owned by a coord on another
	// socket. Surface the ambiguity so the operator can investigate; the
	// orphan-rc-daemons gc kind still catches genuinely stale daemons.
	if alive, perr := tmux.SessionAlive(rec.TmuxSession); perr == nil && !alive {
		fmt.Fprintf(os.Stderr,
			"rc: coord %q record present but tmux session %q not found on current socket (FLEET_TMUX_SOCKET=%q); treating owner as alive (ambiguous — may be a different tmux server, not a dead owner)\n",
			coordID, rec.TmuxSession, os.Getenv("FLEET_TMUX_SOCKET"))
	}
	return true
}

// SetClaudeVersionFnForTest / SetOwnerAliveFnForTest are exported
// stubs for cross-package tests (cmd/fleet/rc_test.go can swap them
// to inject fixed values without shelling out).
func SetClaudeVersionFnForTest(fn func() (string, error)) func() {
	prev := claudeVersionFn
	claudeVersionFn = fn
	return func() { claudeVersionFn = prev }
}

func SetOwnerAliveFnForTest(fn func(coordID string) bool) func() {
	prev := ownerAliveFn
	ownerAliveFn = fn
	return func() { ownerAliveFn = prev }
}
