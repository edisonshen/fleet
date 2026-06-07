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
	"os"
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
		// Find the best match among all states sharing this PID. Selection
		// priority (codex P2: a stale duplicate must never mask a healthy
		// owner — e.g. after a project rename leaves a stale project dir
		// beside the current one, both with the same PID + working_dir):
		//   1. exact working_dir match whose ClaudeVersion == curVer (healthy)
		//   2. any exact working_dir match (stale, but the daemon is owned)
		//   3. loose match when either side has no working_dir (best-effort)
		// We scan ALL candidates and only fall to a lower tier if no higher
		// one exists, so a current-version state always wins over a stale one.
		candidates := byPID[d.PID]
		var recorded RCStateInfo
		matches := false
		matchTier := 0 // 0=none, 1=loose, 2=exact-cwd, 3=exact-cwd+current
		for _, s := range candidates {
			exactCwd := d.WorkingDir != "" && s.WorkingDir != "" && d.WorkingDir == s.WorkingDir
			loose := d.WorkingDir == "" || s.WorkingDir == ""
			switch {
			case exactCwd && curVer != "" && s.ClaudeVersion == curVer:
				recorded, matches, matchTier = s, true, 3 // best possible — stop.
			case exactCwd && matchTier < 3:
				recorded, matches, matchTier = s, true, 2
			case loose && matchTier < 2:
				recorded, matches, matchTier = s, true, 1
			}
			if matchTier == 3 {
				break
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
		// codex P2 (no apply-kill on unconfirmed cwd for state-matched
		// daemons): the stale-version / legacy / recorded-version reasons all
		// derive from a MATCHED rc-state.json. Every project shares the
		// fleet-coord prefix and the kill seam only re-checks argv, so if that
		// match was LOOSE (d.WorkingDir unknown because lsof couldn't populate
		// it), a REUSED stale PID could actually be another project's HEALTHY
		// listener — killing it would be wrong. For state-matched kills,
		// require an EXACT working_dir match (matchTier >= 2). The pure-orphan
		// case (!matches: no state claims this PID) is exempt: there is no
		// competing state to mislead us, and the kill seam's argv re-verify is
		// the defense — a genuine orphan with a probed cwd stays killable.
		stateMatched := matches // !matches → orphan, no rc-state owns the PID.
		cwdConfirmed := !stateMatched || (d.WorkingDir != "" && matchTier >= 2)
		if opts.Apply {
			if !cwdConfirmed {
				act.Reason = reason + " — NOT killed: working_dir unconfirmed (lsof unavailable or PID-only match); surfacing only to avoid killing a reused PID"
				r.Actions = append(r.Actions, act)
				continue
			}
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
		// codex P3: a missing `pgrep` binary surfaces as *exec.Error
		// (ErrNotFound), NOT an *exec.ExitError. The classifier is
		// documented best-effort — on hosts without pgrep it must be a
		// quiet no-op, not a spurious error that pollutes every
		// `fleet status` / `fleet gc` now that this kind is in AllKinds.
		var execErr *exec.Error
		if errors.As(err, &execErr) {
			return nil, nil
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
//
// codex P2 (PID-reuse defense before the kill): the process can exit
// between ListRCDaemons/pgrep enumeration and this signal, and the kernel
// can recycle the PID for an unrelated process. Re-verify the PID's argv
// still looks like a `claude remote-control` listener IMMEDIATELY before
// signaling — same defense rc.Down's verifyPIDIsListener applies. If the
// argv no longer matches (recycled PID) or already gone, refuse the kill.
func killRCDaemonOnDisk(pid int) error {
	if !pidIsRCListenerFn(pid) {
		// Either the process already exited (nothing to kill) or the PID
		// was recycled by an unrelated process (must not signal it).
		fmt.Fprintf(os.Stderr,
			"gc: rc daemon PID %d no longer matches a claude remote-control listener (exited or PID reuse); skipping kill\n",
			pid)
		return nil
	}
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

// pidIsRCListenerFn is the test seam for the pre-kill PID-reuse defense.
// Production uses pidIsRCListener (shells out to ps). Tests override it to
// exercise the "refuse to signal a recycled PID" branch deterministically.
var pidIsRCListenerFn = pidIsRCListener

// pidIsRCListener re-confirms a PID's argv still looks like a fleet
// `claude remote-control` listener via `ps -p <pid> -o args=`. Mirrors
// the argv half of rc.psArgsVerify (claude + remote-control + the
// fleet-coord session prefix). Conservative: any probe failure, empty
// argv, or a non-matching command returns false so we refuse to signal a
// recycled / unrelated PID. The cwd half (lsof) is intentionally skipped
// here — the version/owner cross-check already tied this PID to fleet
// state during classification; this final gate only needs to prove the
// LIVE process is still the listener and not a recycled stranger.
func pidIsRCListener(pid int) bool {
	if pid <= 0 {
		return false
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "args=").Output()
	if err != nil {
		return false
	}
	args := strings.TrimSpace(string(out))
	if args == "" {
		return false
	}
	return strings.Contains(args, "claude") &&
		strings.Contains(args, "remote-control") &&
		strings.Contains(args, rc.SessionPrefix)
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
