package rc

import (
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
	OutcomeConnected       = "connected"
	OutcomeNotEnabled      = "not_enabled"
	OutcomeNotOwned        = "not_owned"
	OutcomeAbsent          = "absent"
	OutcomeContested       = "contested"
	OutcomeError           = "error"

	// OutcomeRespawnedStaleVersion / OutcomeRespawnedDeadOwner — emitted
	// by Up's self-healing idempotent branch when the recorded daemon
	// must be killed and replaced. leak-rc-daemon-lifecycle PR-B:
	// before this, a stale-version daemon would return already_acquired
	// forever and the old `claude remote-control` binary would live on
	// across upgrades (5 daemons across 4 versions found during the
	// 2026-05-29 OOM). Both are success codes; the CLI maps them the
	// same as acquired (exit 0).
	OutcomeRespawnedStaleVersion = "respawned-stale-version"
	OutcomeRespawnedDeadOwner    = "respawned-dead-owner"
)

// UpOpts carries operator overrides for Up.
type UpOpts struct {
	// Cwd is the explicit working_dir override from `fleet rc up <p>
	// --cwd <path>`. Empty falls through to ResolveWorkingDir's
	// meta.json / live-coord chain.
	Cwd string

	// AdoptIfFleetOwned (default: true) — when rc-state.json exists,
	// its PID is alive, and argv matches the recorded session_prefix,
	// adopt the running listener instead of spawning a duplicate.
	// Idempotent re-Up semantics.
	AdoptIfFleetOwned bool

	// AdoptIfUnknown (default: false) — codex round 1: never adopt
	// arbitrary PIDs. v0.12 does not expose this; reserved for power-
	// users in a future release. When false, a PID that doesn't match
	// rc-state.json (or whose state.json is missing) is REFUSED — the
	// operator must `fleet rc reset` + `up` to reclaim ownership.
	AdoptIfUnknown bool

	// SkipSpawn (test seam) — when true, Up performs marker + state
	// bookkeeping but does NOT exec `claude remote-control`. Unit
	// tests set this; production callers leave it false. The injected
	// PID is taken from InjectedPID below (0 means "use os.Getpid()
	// as a placeholder so state.json carries a non-zero pid that
	// IsAlive will return true for during the same test run").
	SkipSpawn bool

	// InjectedPID (test seam) overrides the recorded PID when
	// SkipSpawn is true. 0 falls through to os.Getpid().
	InjectedPID int

	// RespawnOnly (codex P1): when true, Up MUST refuse to create a
	// marker. It only operates on already-enabled projects — i.e.,
	// re-spawn the listener if the recorded PID is dead, or no-op if
	// alive. Marker absent => return OutcomeNotEnabled with no
	// filesystem mutation. The Python coord tick uses this flag so the
	// idempotent fleet-rc-up shell-out NEVER auto-enables RC on a
	// project the operator hasn't opted in to.
	RespawnOnly bool

	// CoordID is the agent ID of the coord invoking Up. Persisted in
	// RecordedState.OwningCoordID so self-healing can detect a dead
	// owner across coord crash/restart. Empty is allowed (legacy
	// callers, manual `fleet rc up` invocations) — the state record
	// keeps the field blank and dead-owner detection is skipped for
	// that entry. leak-rc-daemon-lifecycle PR-B.
	CoordID string
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

// Enabled returns true iff the per-project rc-enabled marker is
// present on disk. THIS IS THE SINGLE SOURCE OF TRUTH for "should
// fleet inject --remote-control / spawn listener / drive
// /remote-control for this project". Every attach/spawn surface
// (S1, S2, S3, I1, I2, I3) calls this helper.
//
// Cheap: one stat. Best-effort: any error collapses to false.
func Enabled(project string) bool {
	return MarkerPresent(project)
}

// GateAttachFlag is the project-aware wrapper internal/handoffop uses
// (and any future caller that injects --remote-control onto a claude
// argv). When the per-project marker is absent OR the
// FLEET_RC_BOOTSTRAP_DISABLED env-gate is set (defense-in-depth from
// PR #157), GateAttachFlag returns argv unchanged. Otherwise it
// delegates to spawn.InjectRemoteControlFlag.
//
// The env-gate is kept through v0.12 per A2/A3 (CI invariant test
// must prove the marker-gate is sufficient before v0.13 retires it).
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

// spawner is the seam Up uses to exec `claude remote-control`.
// Production wires the real exec; tests inject a fake that records
// argv and returns a synthetic PID. Returns (pid, err).
type spawner func(workingDir string) (int, error)

// defaultSpawner shells out to `claude remote-control
// --remote-control-session-name-prefix fleet-coord` in workingDir
// with a detached process. The Claude daemon's directory-keyed
// registry uses workingDir for per-project isolation (codex round 2:
// daemon prefix stays the broad `fleet-coord`).
//
// Detach via Setsid so SIGHUP from the parent's tmux pane doesn't
// cascade; redirect stdio to /dev/null so the child has no
// controlling terminal.
var defaultSpawner spawner = func(workingDir string) (int, error) {
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return 0, fmt.Errorf("rc.spawn: open devnull: %w", err)
	}
	defer func() { _ = devnull.Close() }()

	attr := &os.ProcAttr{
		Dir:   workingDir,
		Files: []*os.File{devnull, devnull, devnull},
		Sys: &syscall.SysProcAttr{
			Setsid: true,
		},
	}
	// argv shape pins what tests grep for + the pgrep sentinel.
	// Resolve `claude` against PATH before exec — os.StartProcess
	// does NOT search PATH on a bare name (codex review iter-1 [P1]:
	// without LookPath, fleet rc up fails on every install where
	// `claude` is only on PATH, not in cwd).
	bin, err := exec.LookPath("claude")
	if err != nil {
		return 0, fmt.Errorf("rc.spawn: claude binary not found on PATH: %w", err)
	}
	argv := []string{bin, "remote-control",
		"--remote-control-session-name-prefix", SessionPrefix}
	proc, err := os.StartProcess(bin, argv, attr)
	if err != nil {
		return 0, fmt.Errorf("rc.spawn: %w", err)
	}
	return proc.Pid, nil
}

// activeSpawner is the package-level pointer tests swap. Production
// uses defaultSpawner; rc_test.go sets a fake.
var activeSpawner = defaultSpawner

// SetSpawnerForTest replaces activeSpawner; returns a restore func
// the test should defer. Exported (vs unexported var) so external
// test packages (cmd/fleet/rc_test.go) can stub without unsafe.
func SetSpawnerForTest(s spawner) func() {
	prev := activeSpawner
	activeSpawner = s
	return func() { activeSpawner = prev }
}

// Up creates the rc-enabled marker + spawns (or adopts) the listener
// for project. Idempotent.
//
// Flow:
//  1. Acquire per-project NB-flock (loser → contested).
//  2. Resolve working_dir (--cwd > meta.json > live coord > fail).
//  3. If rc-state.json exists AND PID alive AND host_id matches:
//     adopt. Outcome=already_acquired. (Fleet-owned, idempotent.)
//  4. Else if marker present BUT state.json absent BUT a
//     fleet-coord-prefix listener appears alive: codex round 2
//     duplicate-spawn refusal. Outcome=contested (operator must
//     `fleet rc reset`).
//  5. Else: ensure project tree, write marker, spawn listener,
//     write state.json. Outcome=acquired.
//
// On (5) spawner errors: marker is left in place (operator opted in,
// the failure is transient), state.json is written with LastError
// populated so Status surfaces the diagnostic.
func Up(project string, opts UpOpts) (string, error) {
	if project == "" {
		return OutcomeError, errors.New("rc.Up: empty project")
	}
	// Default the safe knobs.
	if !opts.AdoptIfFleetOwned {
		opts.AdoptIfFleetOwned = true
	}

	return withLock(project, func() (string, error) {
		// (0) RespawnOnly gate (codex P1, Python coord tick safety):
		// when the caller is the implicit-respawn path (Python skill
		// shelling out every tick), Up must NEVER auto-create a marker
		// for a project the operator hasn't explicitly enabled. If the
		// marker is absent here, return OutcomeNotEnabled and touch
		// nothing else.
		if opts.RespawnOnly && !MarkerPresent(project) {
			return OutcomeNotEnabled, nil
		}

		// (3) Idempotent re-Up: existing state.json + alive PID +
		// matching host + argv matches session_prefix → return
		// already_acquired. The argv check (codex round-3 P2)
		// closes the PID-reuse hole: between listener exit and
		// next Up, the kernel can recycle the PID for an unrelated
		// process. Without the argv verify we'd claim
		// already_acquired and the listener would silently be
		// dead. Falling through on verify-fail respawns the
		// listener AND emits a stderr diagnostic so the operator
		// learns about the underlying state drift (surface, don't
		// silo).
		//
		// Self-healing (leak-rc-daemon-lifecycle PR-B): even when
		// the recorded daemon passes the PID + argv check, we now
		// also check whether ClaudeVersion matches the current
		// `claude --version` AND whether OwningCoordID is still
		// alive. A version mismatch or dead owner triggers
		// kill+respawn so superseded daemons (across upgrades) and
		// orphaned daemons (after coord crash) self-heal on the
		// next tick instead of living forever.
		var healReason string // non-empty triggers respawn after spawn-side prep
		// preResolvedCwd carries the working_dir resolved during the
		// self-heal precheck (codex P2). When non-empty, the (5) fresh-
		// acquire path reuses it instead of resolving AGAIN — re-resolving
		// after killFn could fail if the source (project meta / live coord
		// record) changed concurrently, killing the old listener with no
		// replacement. Resolve once, before the kill, and reuse.
		var preResolvedCwd string
		if cur, err := ReadState(project); err == nil {
			host, _ := os.Hostname()
			if opts.AdoptIfFleetOwned && cur.PID > 0 && workers.IsAlive(cur.PID) && cur.HostID == host {
				prefix := cur.SessionPrefix
				if prefix == "" {
					prefix = SessionPrefix
				}
				if verifyPIDIsListener(cur.PID, prefix, cur.WorkingDir) {
					// PID + argv verified — now apply the self-heal
					// checks (version + owner). Both can short-circuit
					// to a respawn below.
					healReason = computeHealReason(cur)
					if healReason == "" {
						// Healthy daemon — adopt.
						// Ensure marker is present even if operator rm'd it
						// while listener kept running (codex round 2: this
						// is "re-up the marker, not the listener" semantics).
						if !MarkerPresent(project) {
							if _, err := state.EnsureProjectInitialized(project); err != nil {
								return OutcomeError, err
							}
							if err := WriteMarker(project); err != nil {
								return OutcomeError, err
							}
						}
						// codex P2 (owner backfill on adopt): a daemon enabled
						// via operator `fleet rc up <project>` (no --coord-id)
						// records an EMPTY owning_coord_id. The coord tick then
						// adopts it here via `--respawn-only --coord-id <id>`,
						// but without this backfill the owner stays empty
						// forever and computeHealReason keeps SKIPPING dead-
						// owner detection — so a crashed coord's daemon never
						// self-heals. When the caller supplies a CoordID and the
						// record has none, stamp it in (idempotent: only when
						// the recorded owner is empty, so we never clobber an
						// existing owner with a different coord's tick).
						if opts.CoordID != "" && cur.OwningCoordID == "" {
							cur.OwningCoordID = opts.CoordID
							if err := WriteState(cur); err != nil {
								return OutcomeError, err
							}
						}
						return OutcomeAlreadyAcquired, nil
					}
					// codex P2 (resolve-before-kill): we must be able to
					// resolve the replacement working_dir BEFORE signaling
					// the old listener. If we kill first and resolution
					// then fails, we've taken down a working daemon and
					// left rc-state.json pointing at a dead PID with no
					// respawn. Resolve now; on failure, leave the daemon
					// alive and surface the error so the next tick retries.
					// CAPTURE the result and reuse it for (5) — re-resolving
					// after the kill risks a concurrent source change (meta
					// /coord record) failing the second resolve (codex P2).
					rcwd, rerr := ResolveWorkingDir(project, opts.Cwd)
					if rerr != nil {
						return OutcomeError, fmt.Errorf(
							"rc.Up: project %q self-heal (%s) aborted — cannot resolve replacement working_dir, leaving existing PID %d alive: %w",
							project, healReason, cur.PID, rerr)
					}
					preResolvedCwd = rcwd
					// Self-heal: surface what changed so the operator can
					// see why we respawned (version drift / dead owner).
					fmt.Fprintf(os.Stderr,
						"rc.Up: project %q self-heal — %s; killing PID %d and respawning\n",
						project, healReason, cur.PID)
					killFn(cur.PID)
					// Fall through to (5) fresh acquire path. Marker is
					// already present; (5) re-publishes it idempotently and
					// writes a fresh state.json with current version + new
					// CoordID, reusing preResolvedCwd (no re-resolve).
				} else {
					// argv/cwd mismatch — kernel PID reuse (possibly by
					// another project's listener), external kill, or moved
					// working_dir. Tell the operator what we observed
					// before falling through to fresh spawn.
					fmt.Fprintf(os.Stderr,
						"rc.Up: project %q has recorded PID %d alive but does not match recorded session_prefix %q + working_dir %q; treating as dead and respawning (likely kernel PID reuse, cross-project reuse, or external kill)\n",
						project, cur.PID, prefix, cur.WorkingDir)
				}
			}
		}

		// Resolve working_dir BEFORE we touch any state. The self-heal
		// path already resolved it (and reuses that value here) so we
		// never resolve a second time after killing the old listener
		// (codex P2: a concurrent source change must not strand us with a
		// dead daemon and no replacement).
		cwd := preResolvedCwd
		if cwd == "" {
			var err error
			cwd, err = ResolveWorkingDir(project, opts.Cwd)
			if err != nil {
				return OutcomeError, err
			}
		}

		// (4) Duplicate-spawn refusal: marker present, state absent,
		// but a fleet-coord listener appears alive. Conservative:
		// operator must reset.
		if MarkerPresent(project) {
			if _, err := ReadState(project); errors.Is(err, ErrStateMissing) {
				if alive, _ := detectFleetCoordListener(cwd); alive {
					if !opts.AdoptIfUnknown {
						return OutcomeContested, errors.New("rc.Up: marker present + rc-state.json missing + fleet-coord listener alive in working_dir; operator must `fleet rc reset` to reclaim ownership")
					}
				}
			}
		}

		// (5) Fresh acquire path.
		if _, err := state.EnsureProjectInitialized(project); err != nil {
			return OutcomeError, err
		}
		if err := WriteMarker(project); err != nil {
			return OutcomeError, err
		}

		host, _ := os.Hostname()
		now := time.Now().UTC()

		var pid int
		var spawnErr error
		if opts.SkipSpawn {
			pid = opts.InjectedPID
			if pid == 0 {
				pid = os.Getpid()
			}
		} else {
			pid, spawnErr = activeSpawner(cwd)
		}

		// Capture current claude --version + owner ID for the new
		// state record. Empty version is acceptable (degraded probe);
		// the next tick will detect the empty value as stale and
		// re-attempt the probe.
		curVer, _ := claudeVersionFn()
		rec := RecordedState{
			Schema:        SchemaVersion,
			Project:       project,
			PID:           pid,
			HostID:        host,
			WorkingDir:    cwd,
			SessionPrefix: SessionPrefix,
			LastSpawnAt:   now,
			ClaudeVersion: curVer,
			OwningCoordID: opts.CoordID,
		}
		if spawnErr != nil {
			rec.LastError = spawnErr.Error()
		}
		if werr := WriteState(rec); werr != nil {
			return OutcomeError, fmt.Errorf("rc.Up: write state: %w", werr)
		}
		if spawnErr != nil {
			return OutcomeError, spawnErr
		}
		// leak-rc-daemon-lifecycle PR-B: the self-healing path
		// (healReason != "") falls through here after killing the
		// stale daemon. Map healReason → outcome so the caller can
		// observe what happened.
		switch healReason {
		case healReasonStaleVersion:
			return OutcomeRespawnedStaleVersion, nil
		case healReasonDeadOwner:
			return OutcomeRespawnedDeadOwner, nil
		default:
			return OutcomeAcquired, nil
		}
	})
}

// healReason* are the internal labels for Up's self-healing branch.
// computeHealReason returns one of these (or empty for "healthy").
const (
	healReasonStaleVersion = "stale claude version"
	healReasonDeadOwner    = "owning coord is gone"
)

// computeHealReason inspects the recorded state against the current
// claude binary version + owner liveness. Returns one of the
// healReason* strings, or empty when the daemon is healthy (adopt).
//
// Empty recorded ClaudeVersion is treated as "always stale" so legacy
// v1 records force one heal cycle which backfills the schema.
//
// Empty OwningCoordID skips the dead-owner check (legacy or
// manually-invoked records without a coord hint).
//
// Probe failures collapse to "healthy" (no respawn) — better to leak
// briefly than to kill a healthy daemon on a transient probe error.
func computeHealReason(cur RecordedState) string {
	// Version check.
	curVer, err := claudeVersionFn()
	if err == nil && curVer != "" {
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

// Down kills the local PID Fleet owns + removes marker + removes
// rc-state.json. Idempotent. Returns already_released when nothing
// was on disk.
//
// Per design §"Service-side management" (codex round 2): we do NOT
// invoke `claude daemon remote-control remove` — that API is for
// the dir-registry, not for live-listener teardown. The local PID
// kill IS the teardown. Reset (operator emergency) may optionally
// call the registry-clean path; Down does not.
func Down(project string) (string, error) {
	if project == "" {
		return OutcomeError, errors.New("rc.Down: empty project")
	}
	return withLock(project, func() (string, error) {
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
			return OutcomeAlreadyReleased, nil
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
		return OutcomeReleased, nil
	})
}

// Inspect returns the observed State for project. Read-only — no
// lock acquired. Suitable for the dashboard / `fleet rc status`.
func Inspect(project string) (State, error) {
	s := State{Project: project, Enabled: MarkerPresent(project)}
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

// List enumerates projects with markers present. Stable order
// (sorted by name) so JSON output is reproducible.
func List() ([]string, error) {
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
		if MarkerPresent(name) {
			out = append(out, name)
		}
	}
	return out, nil
}

// Reset removes the marker + state.json + kills the listener for
// project (or all projects when project=="").
//
// Operator emergency: when state.json is corrupt or pointing at a
// dead PID, Reset gives a clean slate.
//
// codex round-5 P2: reset-all must enumerate BOTH markered projects
// (via List) AND markerless state files (via Glob). The latter is
// exactly the corruption case `fleet rc reset` exists to clean.
// Without the glob, a project that had its marker removed manually
// but kept its rc-state.json would silently survive reset-all.
func Reset(project string) (string, error) {
	if project == "" {
		seen := map[string]struct{}{}

		// Markered projects (the common case).
		if projs, err := List(); err == nil {
			for _, p := range projs {
				seen[p] = struct{}{}
			}
		} else {
			return OutcomeError, err
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
			if _, err := Down(p); err != nil {
				// Continue; best-effort across all projects.
				_ = err
			}
		}
		return OutcomeReleased, nil
	}
	return Down(project)
}

// SweepAllProjects is the cross-project reconcile hook. Enumerates
// every rc-state.json under ~/.fleet/projects/* and reaps three
// orphan classes:
//
//   - Marker-absent + live PID: operator removed the marker manually
//     but the daemon kept running. Down it.
//   - Stale-version daemon: recorded ClaudeVersion differs from current
//     `claude --version` (or recorded version is empty / v1 legacy).
//     Old daemons across upgrades — the 2026-05-29 OOM root cause.
//   - Dead-owner daemon: recorded OwningCoordID has no live agent
//     record / dead tmux session. Coord crashed without releasing.
//
// Skips cross-host entries (host_id mismatch — unsafe to kill).
// Leaves alive + version-matching + owner-alive entries untouched.
//
// Never kills on prefix-only evidence; state.json must claim Fleet
// ownership (codex round 3 free-form). leak-rc-daemon-lifecycle PR-B
// broadened from marker-absent only to the full three-class sweep
// and wired it into fleet status via cmd/fleet/status.go.
//
// codex P2 (sweep-source correctness): iterate rc-state.json directly
// via filepath.Glob — List() filters marker-absent entries, which are
// exactly the orphans we need to reach.
func SweepAllProjects() error {
	root, err := state.Root()
	if err != nil {
		return err
	}
	matches, err := filepath.Glob(filepath.Join(root, "projects", "*", "rc-state.json"))
	if err != nil {
		return fmt.Errorf("rc.SweepAllProjects: glob: %w", err)
	}
	host, _ := os.Hostname()
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
			continue // recorded PID gone — sweeper has nothing to kill.
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
		// Class 2/3: marker present, daemon alive — apply the same
		// self-heal rubric Up uses. A stale version or dead owner
		// means this daemon needs replacement.
		//
		// codex P1 (sweep must preserve the marker): Down() removes the
		// project's RC marker. The coord's implicit recovery tick uses
		// `fleet rc up --respawn-only`, which returns not_enabled when
		// the marker is gone — so a Down here would silently DISABLE RC
		// for an opted-in project. We reap the dead/stale daemon but
		// KEEP the marker, exactly like Up's self-heal (killFn + fresh
		// acquire, marker never removed). Next --respawn-only tick then
		// respawns under the current claude + fresh coord.
		if reason := computeHealReason(cur); reason != "" {
			fmt.Fprintf(os.Stderr,
				"rc.SweepAllProjects: project %q self-heal — %s; reaping PID %d (marker preserved for respawn)\n",
				p, reason, cur.PID)
			reapDaemonKeepMarker(p, cur)
		}
	}
	return nil
}

// reapDaemonKeepMarker kills the recorded local PID (with the same
// PID-reuse defense Down uses) and removes rc-state.json, but DELIBERATELY
// leaves the RC marker in place. This is the "reap for respawn" teardown:
// the daemon is stale/orphaned and must die, but the project is still
// opted in to RC, so the marker must survive for the coord's next
// `fleet rc up --respawn-only` tick to bring a fresh listener back.
//
//	Down()                 → kill + RemoveState + RemoveMarker (opt-out)
//	reapDaemonKeepMarker() → kill + RemoveState, marker preserved (heal)
//
// codex P1 (re-read under lock): SweepAllProjects reads the state snapshot
// WITHOUT the lock, so a coord tick's `fleet rc up --respawn-only` can
// respawn + rewrite rc-state.json between the snapshot and this call. We
// MUST re-read under the lock and confirm (a) the PID still matches the
// snapshot and (b) computeHealReason still flags it stale. If the state
// changed (fresh respawn) we abort — removing the fresh state here would
// leave marker + no-state + live listener, forcing the next respawn-only
// tick down the contested path instead of clean adoption.
//
// Best-effort: errors are swallowed (sweep is fire-and-forget across all
// projects; one bad entry must not abort the rest).
func reapDaemonKeepMarker(project string, snapshot RecordedState) {
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
				"rc.reapDaemonKeepMarker: project %q state changed under lock (snapshot PID %d -> current PID %d); aborting reap (likely concurrent respawn)\n",
				project, snapshot.PID, cur.PID)
			return OutcomeAlreadyAcquired, nil
		}
		if computeHealReason(cur) == "" {
			fmt.Fprintf(os.Stderr,
				"rc.reapDaemonKeepMarker: project %q no longer stale under lock (PID %d); aborting reap\n",
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
				"rc.reapDaemonKeepMarker: project %q rc-state.json is now owned by host %q (local %q); aborting reap (cross-host — that host's sweep will handle it)\n",
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
					"rc.reapDaemonKeepMarker: project %q PID %d could not be strictly verified as our listener (argv/cwd unconfirmed — likely PID reuse or lsof unavailable); skipping reap to avoid killing an unrelated process\n",
					project, cur.PID)
				return OutcomeAlreadyAcquired, nil
			}
			killFn(cur.PID)
		}
		_ = RemoveState(project)
		return OutcomeReleased, nil
	})
}

// reapMarkerlessStrict is the Class-1 (marker-absent) sweep teardown. The
// operator opted out (removed the marker) but the daemon kept running, so
// we remove BOTH the marker and the state (full opt-out, like Down) — BUT
// with the same strict cwd confirmation reapDaemonKeepMarker uses, because
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

// detectFleetCoordListener returns true iff a process whose argv
// matches the fleet-coord listener shape is alive AND its cwd
// matches workingDir.
//
// Best-effort: errors collapse to (false, err). Used only by Up's
// duplicate-spawn refusal path; never load-bearing for adoption
// (codex round 2: "never kill on prefix-only evidence").
//
// Implementation note: we shell out to pgrep via the test seam so
// tests can stub. Production uses the OS pgrep binary.
func detectFleetCoordListener(workingDir string) (bool, error) {
	return detectListenerFn(workingDir)
}

// detectListenerFn is the test seam. Production uses pgrepDetect.
// Tests override via SetDetectListenerForTest.
var detectListenerFn = pgrepDetect

// SetDetectListenerForTest swaps detectListenerFn; returns a restore
// func the test should defer.
func SetDetectListenerForTest(fn func(workingDir string) (bool, error)) func() {
	prev := detectListenerFn
	detectListenerFn = fn
	return func() { detectListenerFn = prev }
}

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
func defaultClaudeVersion() (string, error) {
	bin, err := exec.LookPath("claude")
	if err != nil {
		return "", fmt.Errorf("claude binary not found on PATH: %w", err)
	}
	out, err := exec.Command(bin, "--version").Output()
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

// pgrepDetect finds candidate listener PIDs via `pgrep -f
// "claude.*remote-control"` (portable: macOS/Linux/BSD all support
// -f matching against argv). For each candidate, it then inspects
// argv via `ps -p <pid> -o args=` to filter on SessionPrefix, and
// verifies cwd matches workingDir via `lsof -a -p <pid> -d cwd`.
//
// codex round-3 P2: the previous stub always returned false, making
// the duplicate-spawn refusal gate dead code. codex round-4 P2:
// the first restoration used `pgrep -a` which is Linux-only — on
// macOS/BSD it fails and the gate degraded to false again. This
// version uses only portable flags and per-PID `ps` for argv.
//
// Best-effort: missing pgrep/ps/lsof emits a one-line stderr
// diagnostic so the operator knows the gate is degraded (surface-
// don't-silo), then returns false. A false negative just allows a
// fresh spawn — same outcome as no listener alive — so recovery
// stays available via `fleet rc reset`.
//
// pgrep exit semantics:
//
//	0 → matches found
//	1 → no matches (NOT an error — treat as "no listener")
//	2/3 → syntax / system error (surface diagnostic)
func pgrepDetect(workingDir string) (bool, error) {
	if workingDir == "" {
		return false, nil
	}
	out, err := exec.Command("pgrep", "-f", "claude.*remote-control").Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return false, nil // no matches
		}
		fmt.Fprintf(os.Stderr,
			"rc: pgrep unavailable; duplicate-spawn refusal gate degraded (%v)\n",
			err)
		return false, nil
	}
	for _, pidStr := range strings.Fields(string(out)) {
		pid, perr := strconv.Atoi(pidStr)
		if perr != nil || pid <= 0 {
			continue
		}
		argsOut, aerr := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "args=").Output()
		if aerr != nil {
			// ps refusal usually means the process exited between
			// pgrep and ps — treat as no match for this PID.
			continue
		}
		if !strings.Contains(string(argsOut), SessionPrefix) {
			continue
		}
		cwdOut, lerr := exec.Command("lsof", "-a", "-p", strconv.Itoa(pid), "-d", "cwd", "-Fn").Output()
		if lerr != nil {
			fmt.Fprintf(os.Stderr,
				"rc: lsof unavailable for pid %d; duplicate-spawn refusal gate degraded (%v)\n",
				pid, lerr)
			continue
		}
		for _, l := range strings.Split(string(cwdOut), "\n") {
			if strings.HasPrefix(l, "n") && l[1:] == workingDir {
				return true, nil
			}
		}
	}
	return false, nil
}
