package gc

// rc_daemons.go — eighth classifier for `fleet gc`: enumerates live
// `claude remote-control --remote-control-session-name-prefix
// fleet-coord` PIDs (via pgrep) and cross-references each against
// every project's rc-state.json. A daemon is "orphan" when:
//
//   - No rc-state.json entry references its PID + working_dir. The
//     coord that spawned it crashed / was force-archived without
//     calling rc.Down, OR a manual `claude remote-control` was started
//     outside fleet.
//
//   - Its recorded ClaudeVersion (or the binary it's running) disagrees
//     with the current `claude --version`. Old daemons across upgrades
//     — the 2026-05-29 OOM root cause. Five daemons across 4 versions
//     ate ~471 MB.
//
// Default behavior: surface. `--apply` upgrades to would-kill / killed
// (SIGTERM with a fallback to SIGKILL handled by KillRCDaemon).
//
// leak-rc-daemon-lifecycle PR-B (fleet leak doc §2). The PR-B Up
// self-healing path catches orphans on the next coord tick; this gc
// kind is the operator-invoked sweep for existing leaked daemons.

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/edisonshen/fleet/internal/rc"
	"github.com/edisonshen/fleet/internal/state"
)

// KindOrphanRCDaemons is the eighth classifier — RC daemon process sweep.
const KindOrphanRCDaemons Kind = "orphan-rc-daemons"

// RCDaemonInfo is the minimal per-process shape the classifier needs.
// PID is the OS pid. Version is the `claude --version` reading of the
// binary the process is running (empty when the probe can't determine
// it — fall back to per-state cross-check on PID alone). WorkingDir
// is the process cwd (via lsof) used to disambiguate across projects.
type RCDaemonInfo struct {
	PID        int
	Version    string
	WorkingDir string
}

// RCStateInfo is one project's rc-state.json projection used to
// recognize a daemon as fleet-owned. The classifier asks the lister
// to enumerate every project so the cross-check is a single in-memory
// scan per Reconcile call (no per-PID re-read).
type RCStateInfo struct {
	Project       string
	PID           int
	ClaudeVersion string
	WorkingDir    string
	OwningCoordID string
}

// reconcileOrphanRCDaemons enumerates live RC daemons, cross-references
// each against the rc-state.json snapshot, and produces actions for
// daemons that match no live state OR whose recorded version differs
// from the current binary. See package comment for the full failure
// taxonomy.
//
// Two cross-check axes:
//
//   - PID + WorkingDir match: the rc-state.json for some project has
//     this exact PID and the working_dir agrees. Healthy.
//   - PID match but WorkingDir disagrees: kernel PID reuse, or a
//     stale state.json. Treat as orphan (the live process isn't who
//     state claims).
//
// Version mismatch fires even when the daemon matches a live state
// (stale-version branch) — Up's self-healing would catch this on the
// next tick, but the gc kind is the operator's "force the sweep now"
// hook.
func reconcileOrphanRCDaemons(r *Report, opts Options, deps Deps) error {
	if deps.ListRCDaemons == nil {
		return nil // unwired — production callers always set it.
	}
	daemons, err := deps.ListRCDaemons()
	if err != nil {
		return fmt.Errorf("list rc daemons: %w", err)
	}
	if len(daemons) == 0 {
		return nil
	}
	var states []RCStateInfo
	if deps.ListRCStates != nil {
		states, err = deps.ListRCStates()
		if err != nil {
			return fmt.Errorf("list rc states: %w", err)
		}
	}
	curVer := ""
	if deps.CurrentClaudeVersion != nil {
		v, verr := deps.CurrentClaudeVersion()
		if verr == nil {
			curVer = v
		}
	}

	// Index states by PID. A PID can appear in multiple rc-state.json
	// files (kernel PID reuse across projects) — keep ALL of them so the
	// working_dir cross-check below can find the CORRECT match rather
	// than latching onto whichever was enumerated first (codex P2: a
	// stale entry seen before the healthy one would otherwise mask the
	// live daemon's real state and get it killed under --apply).
	byPID := make(map[int][]RCStateInfo, len(states))
	for _, s := range states {
		byPID[s.PID] = append(byPID[s.PID], s)
	}

	for _, d := range daemons {
		if d.PID <= 0 {
			continue
		}
		// Find the best match among all states sharing this PID: prefer a
		// working_dir match; fall back to any entry when either side has
		// no working_dir recorded (best-effort probe).
		candidates := byPID[d.PID]
		var recorded RCStateInfo
		matches := false
		for _, s := range candidates {
			if d.WorkingDir != "" && s.WorkingDir != "" && d.WorkingDir == s.WorkingDir {
				recorded, matches = s, true
				break // exact working_dir match wins.
			}
			if !matches && (d.WorkingDir == "" || s.WorkingDir == "") {
				recorded, matches = s, true // tentative; keep scanning for an exact match.
			}
		}
		var reason string
		switch {
		case !matches:
			reason = "no matching rc-state.json (orphan daemon)"
		case curVer != "" && d.Version != "" && d.Version != curVer:
			reason = fmt.Sprintf("stale claude version (running %s, current %s)", d.Version, curVer)
		case curVer != "" && recorded.ClaudeVersion != "" && recorded.ClaudeVersion != curVer:
			reason = fmt.Sprintf("recorded version %s differs from current %s",
				recorded.ClaudeVersion, curVer)
		case curVer != "" && recorded.ClaudeVersion == "" && d.Version == "":
			// Legacy v1 rc-state.json: no recorded version, and the live
			// probe couldn't read one either. The rc self-heal path
			// (rc.computeHealReason) treats an empty recorded version as
			// stale to force a one-time backfill under the current claude.
			// gc must be consistent — surface it so a pre-upgrade daemon
			// isn't classified healthy and left to live forever.
			reason = fmt.Sprintf("legacy daemon with no recorded claude version (current %s)", curVer)
		default:
			continue // healthy
		}
		act := Action{
			Kind:   KindOrphanRCDaemons,
			Target: strconv.Itoa(d.PID),
			Verb:   VerbSurface,
			Reason: reason,
		}
		if opts.Apply {
			if deps.KillRCDaemon == nil {
				act.Reason = "kill seam unwired (set Deps.KillRCDaemon to apply)"
				r.Actions = append(r.Actions, act)
				continue
			}
			if kerr := deps.KillRCDaemon(d.PID); kerr != nil {
				act.Verb = VerbSurface
				act.Reason = fmt.Sprintf("kill failed: %v", kerr)
			} else {
				act.Verb = VerbKilled
			}
		}
		r.Actions = append(r.Actions, act)
	}
	return nil
}

// ----------------- production wiring (DefaultDeps) ------------------

// listRCDaemonsOnDisk enumerates live `claude remote-control` PIDs via
// `pgrep -f` and probes each with `ps`/`lsof` for the working_dir.
// Best-effort: missing pgrep collapses to (nil, nil) so the classifier
// becomes a no-op on hosts that don't have it.
func listRCDaemonsOnDisk() ([]RCDaemonInfo, error) {
	// Match the same argv signature rc.defaultSpawner produces:
	// `claude remote-control --remote-control-session-name-prefix fleet-coord`.
	out, err := exec.Command("pgrep", "-f",
		"claude remote-control --remote-control-session-name-prefix "+rc.SessionPrefix).Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return nil, nil // no matches
		}
		return nil, fmt.Errorf("pgrep: %w", err)
	}
	var infos []RCDaemonInfo
	for _, pidStr := range strings.Fields(string(out)) {
		pid, perr := strconv.Atoi(pidStr)
		if perr != nil || pid <= 0 {
			continue
		}
		info := RCDaemonInfo{PID: pid}
		// Best-effort working_dir via lsof — skip on error.
		if cwd, err := lsofProcessCwd(pid); err == nil {
			info.WorkingDir = cwd
		}
		// Best-effort version: the running daemon's argv doesn't carry
		// the version, so we can't derive it cheaply. Leave empty —
		// the classifier falls back to comparing recorded state's
		// ClaudeVersion against current.
		infos = append(infos, info)
	}
	return infos, nil
}

// lsofProcessCwd returns the cwd of pid via `lsof -a -p <pid> -d cwd -Fn`.
// Empty + nil on probe failure (lsof not installed, permission denied).
func lsofProcessCwd(pid int) (string, error) {
	out, err := exec.Command("lsof", "-a", "-p", strconv.Itoa(pid), "-d", "cwd", "-Fn").Output()
	if err != nil {
		return "", err
	}
	for _, l := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(l, "n") {
			return l[1:], nil
		}
	}
	return "", errors.New("no cwd line")
}

// listRCStatesOnDisk enumerates every ~/.fleet/projects/*/rc-state.json
// and projects it into RCStateInfo. Parse failures are silently
// skipped (the orphan-rc-daemons classifier treats missing matches as
// "no state", which is the conservative answer).
func listRCStatesOnDisk() ([]RCStateInfo, error) {
	root, err := state.Root()
	if err != nil {
		return nil, err
	}
	matches, err := filepath.Glob(filepath.Join(root, "projects", "*", rc.StateFilename))
	if err != nil {
		return nil, fmt.Errorf("glob rc-state.json: %w", err)
	}
	out := make([]RCStateInfo, 0, len(matches))
	for _, m := range matches {
		p := filepath.Base(filepath.Dir(m))
		if p == "" || strings.HasPrefix(p, ".") {
			continue
		}
		cur, err := rc.ReadState(p)
		if err != nil {
			continue
		}
		out = append(out, RCStateInfo{
			Project:       cur.Project,
			PID:           cur.PID,
			ClaudeVersion: cur.ClaudeVersion,
			WorkingDir:    cur.WorkingDir,
			OwningCoordID: cur.OwningCoordID,
		})
	}
	return out, nil
}

// currentClaudeVersionOnDisk reads `claude --version` and returns the
// leading semver token. Mirrors internal/rc/rc.go:defaultClaudeVersion;
// duplicated here to avoid exposing the unexported helper from
// internal/rc, and because the version-format is stable across the
// codebase (cmd/fleet → internal/rc dependency direction).
func currentClaudeVersionOnDisk() (string, error) {
	bin, err := exec.LookPath("claude")
	if err != nil {
		return "", err
	}
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return "", nil
	}
	if i := strings.IndexAny(line, " \t"); i > 0 {
		return line[:i], nil
	}
	return line, nil
}

// killRCDaemonOnDisk sends SIGTERM, waits up to 10s for the process
// to exit, then escalates to SIGKILL. Mirrors internal/rc.defaultKill.
// Used by `fleet gc --apply --kinds=orphan-rc-daemons`.
func killRCDaemonOnDisk(pid int) error {
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("SIGTERM %d: %w", pid, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	if processAlive(pid) {
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("SIGKILL %d: %w", pid, err)
		}
	}
	return nil
}

// processAlive probes whether pid is still running via signal-0
// (the canonical liveness probe). Returns false on any error.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return false
	}
	return true
}
