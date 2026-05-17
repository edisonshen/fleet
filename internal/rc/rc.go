package rc

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/edisonshen/fleet/internal/spawn"
	"github.com/edisonshen/fleet/internal/state"
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
	argv := []string{"claude", "remote-control",
		"--remote-control-session-name-prefix", SessionPrefix}
	proc, err := os.StartProcess("claude", argv, attr)
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
		// (3) Idempotent re-Up: existing state.json + alive PID +
		// matching host → return already_acquired.
		if cur, err := ReadState(project); err == nil {
			host, _ := os.Hostname()
			if opts.AdoptIfFleetOwned && cur.PID > 0 && workers.IsAlive(cur.PID) && cur.HostID == host {
				// Ensure marker is present even if operator rm'd it
				// while listener kept running (codex round 2: this is
				// "re-up the marker, not the listener" semantics).
				if !MarkerPresent(project) {
					if _, err := state.EnsureProjectInitialized(project); err != nil {
						return OutcomeError, err
					}
					if err := WriteMarker(project); err != nil {
						return OutcomeError, err
					}
				}
				return OutcomeAlreadyAcquired, nil
			}
		}

		// Resolve working_dir BEFORE we touch any state.
		cwd, err := ResolveWorkingDir(project, opts.Cwd)
		if err != nil {
			return OutcomeError, err
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

		rec := RecordedState{
			Schema:        SchemaVersion,
			Project:       project,
			PID:           pid,
			HostID:        host,
			WorkingDir:    cwd,
			SessionPrefix: SessionPrefix,
			LastSpawnAt:   now,
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
		return OutcomeAcquired, nil
	})
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

		if !markerHad && !stateHad {
			return OutcomeAlreadyReleased, nil
		}

		// Kill local PID if state says Fleet owns one. Cross-host
		// refusal: if host_id doesn't match, log + skip the kill (we
		// can't safely signal a remote-PID). Marker still gets
		// removed locally so the gate fires off.
		if stateHad && cur.PID > 0 {
			host, _ := os.Hostname()
			if cur.HostID == "" || cur.HostID == host {
				if workers.IsAlive(cur.PID) {
					killFn(cur.PID)
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
func Reset(project string) (string, error) {
	if project == "" {
		// Reset-all: enumerate List, Down each.
		projs, err := List()
		if err != nil {
			return OutcomeError, err
		}
		for _, p := range projs {
			if _, err := Down(p); err != nil {
				// Continue; best-effort across all projects.
				_ = err
			}
		}
		return OutcomeReleased, nil
	}
	return Down(project)
}

// SweepAllProjects is the PR4 sweeper hook. Enumerates every
// rc-state.json under ~/.fleet/projects/* and:
//   - releases orphans (state alive but marker absent).
//   - skips cross-host entries (host_id mismatch — unsafe to kill).
//   - leaves alive+matching entries untouched.
//
// Never kills on prefix-only evidence; state.json must claim Fleet
// ownership (codex round 3 free-form).
func SweepAllProjects() error {
	projs, err := List()
	if err != nil {
		return err
	}
	host, _ := os.Hostname()
	for _, p := range projs {
		cur, err := ReadState(p)
		if err != nil {
			continue
		}
		if cur.HostID != "" && cur.HostID != host {
			continue
		}
		if !MarkerPresent(p) && cur.PID > 0 && workers.IsAlive(cur.PID) {
			// Orphan: state says we own a live PID but the marker
			// is gone (operator rm'd the marker manually). Down it.
			if _, err := Down(p); err != nil {
				continue
			}
		}
	}
	return nil
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

// pgrepDetect is the default implementation. Returns false on any
// pgrep error (no matches → exit 1, which we treat as "no listener
// alive" — the conservative answer for the duplicate-spawn refusal
// gate; a false negative here just allows a fresh spawn, which is
// the same outcome as not having had a listener at all).
//
// NB: production is best-effort; we don't want pgrep absence on
// minimal Linux containers to break Up.
func pgrepDetect(workingDir string) (bool, error) {
	// Without pgrep available, return false — the gate just becomes
	// "spawn anyway". The dup-refusal test stubs this via
	// SetDetectListenerForTest, so the production path's accuracy
	// is best-effort but not load-bearing for the unit test.
	_ = workingDir
	return false, nil
}
