package gc

// drain_procs_disk.go — production wiring for the KindDrainProcs
// classifier (the gc.Deps drain-* hooks). Kept out of drain_procs.go so
// the classifier file reads as pure logic + the injectable seam, and the
// OS-touching helpers (ps / kill / fs) live together here.
//
// Process identity uses `ps -p <pid> -o lstart=` as the start-time
// fingerprint (stable across darwin + linux; defeats PID reuse) and the
// STAT first char for the legacy sweep's sleeping gate. We deliberately
// avoid /proc (linux-only) and platform syscalls — `ps` is the portable
// surface already used elsewhere in fleet (internal/rc, internal/spawn).

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/edisonshen/fleet/internal/state"
)

// listDrainRunsOnDisk is the production ListDrainRuns. Reads every
// ~/.fleet/drain-runs/<pid>.json into a DrainRun with its Path set.
// A run-record that fails to parse is skipped (not fatal) — a torn /
// partial write must not strand the rest of the sweep; the next drain
// heartbeat or clean exit overwrites/deletes it.
func listDrainRunsOnDisk() ([]DrainRun, error) {
	dir, err := state.DrainRunsDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var out []DrainRun
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			continue // torn read; skip
		}
		var run DrainRun
		if jerr := json.Unmarshal(data, &run); jerr != nil {
			continue // partial / malformed write; skip
		}
		run.Path = path
		out = append(out, run)
	}
	return out, nil
}

// drainProcLiveOnDisk is the production DrainProcLive. True iff the pid
// is alive AND its current start-time fingerprint matches pidStart.
//
//   - pid dead (ESRCH) → false.
//   - pid alive but lstart != recorded pidStart → false (PID reuse: an
//     unrelated process now owns the number; T18c). NEVER kill it.
//   - pid alive + lstart match → true (the recorded drain is still it).
//   - recorded pidStart == "" (legacy record from before fingerprinting)
//     → fall back to a bare liveness probe so an old record still reaps.
func drainProcLiveOnDisk(pid int, pidStart string) bool {
	if pid <= 0 {
		return false
	}
	if !pidAliveOnDisk(pid) {
		return false
	}
	if pidStart == "" {
		return true // no fingerprint to corroborate — bare liveness
	}
	cur, err := procStartTime(pid)
	if err != nil {
		// Can't read the start time of a process we just saw alive →
		// conservative: treat as NOT a confirmed identity match so we do
		// not kill on ambiguity (surface-don't-silo).
		return false
	}
	return cur == pidStart
}

// killDrainGuarded is the production KillDrain. RE-VALIDATES identity
// immediately before signaling (the gap between classification and kill
// is a PID-reuse window) and signals SIGTERM only when identity holds.
// Returns:
//
//	(false, nil) — pid already dead OR identity no longer matches → no-op
//	               (idempotent; never shoots an unrelated reused PID).
//	(true,  nil) — identity confirmed → SIGTERM delivered.
//	(false, err) — the signal call itself failed for a non-ESRCH reason.
//
// Prints a one-line stderr-style audit via the standard log surface is
// intentionally omitted here (the CLI renders the Action verb/reason);
// the guard is the load-bearing safety, not the logging.
func killDrainGuarded(target DrainKillTarget) (bool, error) {
	if target.Pid <= 0 {
		return false, nil
	}
	// Re-validate liveness + start-time fingerprint at signal time.
	if !pidAliveOnDisk(target.Pid) {
		return false, nil // already gone — idempotent no-op
	}
	if target.PidStart != "" {
		cur, err := procStartTime(target.Pid)
		if err != nil || cur != target.PidStart {
			// Identity changed (PID reuse) or unreadable → surface, don't
			// kill. The classifier records this as a no-op.
			return false, nil
		}
	}
	// Corroborate the process is still a `fleet drain` (defends both
	// paths; the legacy sweep also passes Exe). argv must contain
	// "fleet" AND "drain" — anything else is out of blast radius.
	if !procIsFleetDrain(target.Pid) {
		return false, nil
	}
	if err := syscall.Kill(target.Pid, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return false, nil // raced to exit between probe and signal
		}
		return false, fmt.Errorf("SIGTERM pid %d: %w", target.Pid, err)
	}
	return true, nil
}

// removeDrainRunFile is the production RemoveDrainRun. ENOENT-tolerant so
// two operators racing `fleet gc --apply` don't see spurious errors.
func removeDrainRunFile(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

// listDrainProcsOnDisk is the production ListDrainProcs for the
// --legacy-drains coarse sweep. Enumerates EVERY process via
// `ps -o pid,state,lstart,args -ax`, keeps ONLY those whose argv is a
// `fleet drain` invocation (argv contains the fleet binary basename AND
// the `drain` subcommand token), and projects each into a DrainProcInfo
// with its sleeping flag (STAT first char) + age (now - lstart).
//
// Blast radius: a non-`fleet drain` process (a different fleet
// subcommand, an unrelated binary that merely mentions "drain") is never
// yielded, so the legacy sweep can only ever touch provably-fleet-owned
// drains (TLD4).
func listDrainProcsOnDisk() ([]DrainProcInfo, error) {
	out, err := exec.Command("ps", "-o", "pid,state,lstart,args", "-ax").Output()
	if err != nil {
		return nil, fmt.Errorf("ps list: %w", err)
	}
	now := time.Now()
	var procs []DrainProcInfo
	self := os.Getpid()
	for _, ln := range strings.Split(string(out), "\n") {
		s := strings.TrimLeft(ln, " ")
		if s == "" {
			continue
		}
		pidTok, rest := splitFirstField(s)
		pid, perr := strconv.Atoi(pidTok)
		if perr != nil {
			continue // header / malformed
		}
		if pid == self {
			continue // never classify the running gc process
		}
		stateTok, rest := splitFirstField(rest)
		if stateTok == "" {
			continue
		}
		// lstart is a fixed 5-field ctime string: "Wed May 13 17:20:39 2026".
		lstart, args := splitLstart(rest)
		if lstart == "" || args == "" {
			continue
		}
		if !argvIsFleetDrain(args) {
			continue
		}
		started, terr := parseLstart(lstart)
		age := time.Duration(0)
		if terr == nil {
			age = now.Sub(started)
		}
		procs = append(procs, DrainProcInfo{
			Pid:      pid,
			PidStart: lstart, // the lstart string IS the fingerprint
			Exe:      firstField(args),
			Sleeping: isSleepingState(stateTok),
			Age:      age,
		})
	}
	return procs, nil
}

// procStartTime returns the `ps -p <pid> -o lstart=` start-time string —
// the stable per-process fingerprint used to defeat PID reuse. Empty
// output / ps error → an error (caller treats as unverifiable).
func procStartTime(pid int) (string, error) {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "lstart=").Output()
	if err != nil {
		return "", fmt.Errorf("ps lstart pid %d: %w", pid, err)
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return "", fmt.Errorf("ps lstart pid %d: empty", pid)
	}
	return s, nil
}

// procIsFleetDrain reports whether the live process's argv is a
// `fleet drain` invocation. Used as the final guard before SIGTERM.
// Any probe failure → false (don't kill what we can't confirm).
func procIsFleetDrain(pid int) bool {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "args=").Output()
	if err != nil {
		return false
	}
	return argvIsFleetDrain(strings.TrimSpace(string(out)))
}

// argvIsFleetDrain matches a `fleet drain` command line. Requires BOTH
// the fleet binary token (basename "fleet") AND the `drain` subcommand
// to appear, so a different fleet subcommand or an unrelated binary
// mentioning "drain" is excluded.
func argvIsFleetDrain(argv string) bool {
	fields := strings.Fields(argv)
	if len(fields) < 2 {
		return false
	}
	exe := filepath.Base(fields[0])
	// Accept "fleet" or a test/dev binary basename that ends in "fleet".
	if exe != "fleet" && !strings.HasSuffix(exe, "/fleet") {
		// Also accept an absolute path whose basename is exactly fleet.
		if filepath.Base(exe) != "fleet" {
			return false
		}
	}
	for _, f := range fields[1:] {
		if f == "drain" {
			return true
		}
		// First non-flag token after the binary is the subcommand; if it
		// is not "drain", this is a different fleet command.
		if !strings.HasPrefix(f, "-") {
			return f == "drain"
		}
	}
	return false
}

// isSleepingState reports whether a `ps` STAT token's primary state char
// is a sleeping/idle state. Darwin + Linux both use: S (interruptible
// sleep), D (uninterruptible sleep), I (idle). R = running, Z = zombie,
// T = stopped. The leaked drains block forever on a lock → sleeping.
func isSleepingState(stat string) bool {
	if stat == "" {
		return false
	}
	switch stat[0] {
	case 'S', 'D', 'I':
		return true
	default:
		return false
	}
}

// splitFirstField splits the leading whitespace-delimited token off s,
// returning (token, remainder-with-leading-space-trimmed).
func splitFirstField(s string) (string, string) {
	s = strings.TrimLeft(s, " ")
	i := strings.IndexByte(s, ' ')
	if i < 0 {
		return s, ""
	}
	return s[:i], strings.TrimLeft(s[i+1:], " ")
}

// firstField returns just the first whitespace-delimited token of s.
func firstField(s string) string {
	tok, _ := splitFirstField(s)
	return tok
}

// splitLstart peels the fixed 5-token ctime lstart string
// ("Wed May 13 17:20:39 2026") off the front of s and returns it joined
// by single spaces plus the remaining args. Returns ("","") if s has
// fewer than 5 leading tokens.
func splitLstart(s string) (lstart, args string) {
	rest := strings.TrimLeft(s, " ")
	var toks []string
	for i := 0; i < 5; i++ {
		var t string
		t, rest = splitFirstField(rest)
		if t == "" {
			return "", ""
		}
		toks = append(toks, t)
	}
	return strings.Join(toks, " "), rest
}

// parseLstart parses the `ps lstart` ctime format. Tries the canonical
// Unix ctime layout; on failure returns an error and the caller treats
// age as 0 (which fails the age-floor gate → conservatively not reaped).
func parseLstart(s string) (time.Time, error) {
	// ctime layout: "Mon Jan _2 15:04:05 2006".
	if t, err := time.ParseInLocation("Mon Jan _2 15:04:05 2006", s, time.Local); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("unparseable lstart %q", s)
}
