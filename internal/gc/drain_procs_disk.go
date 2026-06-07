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
		// Alive but start-time unreadable (transient ps failure /
		// permissions). Treat as LIVE/ambiguous (codex [P2]): returning
		// false here would route the classifier into the !live branch
		// which DELETES the run-record under --apply, stranding a
		// potentially-wedged drain. Returning true sends it through the
		// WEDGED branch instead, where the guarded kill itself re-probes
		// and, on the same ambiguity, KEEPS the record for the next sweep.
		return true
	}
	return cur == pidStart
}

// killDrainGuarded is the production KillDrain. RE-VALIDATES identity
// immediately before signaling (the gap between classification and kill
// is a PID-reuse window) and signals SIGTERM only when identity holds.
// Returns a DrainKillResult so the classifier can distinguish a real
// kill from a confirmed-gone no-op from an UNconfirmed guard failure
// (codex [P2] — the last case must NOT delete the run-record):
//
//	{Killed:true}            — identity confirmed → SIGTERM delivered.
//	{Gone:true}              — pid confirmed dead OR start identity changed
//	                           (PID reuse) → no signal, safe to clean.
//	{} (zero)                — guard could NOT confirm (transient argv-probe
//	                           failure) → KEEP the record, retry next sweep.
//	{}, err                  — the signal call failed for a non-ESRCH reason.
func killDrainGuarded(target DrainKillTarget) (DrainKillResult, error) {
	if target.Pid <= 0 {
		return DrainKillResult{Gone: true}, nil
	}
	// Re-validate liveness at signal time.
	if !pidAliveOnDisk(target.Pid) {
		return DrainKillResult{Gone: true}, nil // already gone — idempotent
	}
	// Re-validate the start-time fingerprint (PID-reuse defense). A
	// mismatch means the live PID is an UNRELATED process → confirmed the
	// recorded drain is gone. An unreadable fingerprint is AMBIGUOUS (we
	// saw it alive but can't prove identity) → do NOT kill, do NOT claim
	// gone — return the zero result so the record is kept.
	recorded := target.PidStart != "" // steady-state: pid_start IS the strong identity
	if recorded {
		cur, err := procStartTime(target.Pid)
		switch {
		case err != nil:
			return DrainKillResult{}, nil // ambiguous — keep record, retry
		case cur != target.PidStart:
			return DrainKillResult{Gone: true}, nil // PID reuse — unrelated proc
		}
		// pid_start matched → this IS the recorded drain. Do NOT run the
		// exe-identity gate here (codex iter-8 [P2]): a binary-path change
		// (post-upgrade, dev build) would otherwise mis-report Gone and
		// delete the record for a still-live drain, stranding it. The argv
		// re-probe below is the only remaining corroboration.
	} else if target.RequireFingerprint {
		// Legacy path with no captured fingerprint (codex iter-6 [P2]):
		// fail CLOSED — without a start-time to corroborate, the live PID
		// could be a different process that reused the number. Surface,
		// never signal.
		return DrainKillResult{}, nil
	} else {
		// Legacy path WITH a fingerprint: argv-basename alone is spoofable
		// (an unrelated binary/symlink named `fleet` with a `drain` arg),
		// so require the target's executable to be the SAME fleet binary we
		// are running before signaling (codex iter-6 [P2]). Only the legacy
		// path needs this — the steady-state path already proved identity
		// via pid_start above.
		switch sameFleetExe(target.Pid) {
		case exeProbeMismatch:
			return DrainKillResult{Gone: true}, nil // a different binary owns the PID
		case exeProbeFailed:
			return DrainKillResult{}, nil // can't prove ownership — surface, don't kill
		}
	}
	// Corroborate the process is still a `fleet drain` (defends both
	// paths). A probe failure is AMBIGUOUS (keep record); a clean
	// non-match means the PID is now a different command → gone.
	switch procDrainState(target.Pid) {
	case drainProbeFailed:
		return DrainKillResult{}, nil // ambiguous — keep record, retry
	case drainProbeNotDrain:
		return DrainKillResult{Gone: true}, nil // reused by a non-drain command
	}
	if err := syscall.Kill(target.Pid, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return DrainKillResult{Gone: true}, nil // raced to exit
		}
		return DrainKillResult{}, fmt.Errorf("SIGTERM pid %d: %w", target.Pid, err)
	}
	return DrainKillResult{Killed: true}, nil
}

// exeProbe is the three-way outcome of comparing a target's executable
// to the running fleet binary.
type exeProbe int

const (
	exeProbeMatch    exeProbe = iota // same fleet binary
	exeProbeMismatch                 // a DIFFERENT executable owns the PID
	exeProbeFailed                   // could not read one/both exes (ambiguous)
)

// sameFleetExe compares the target pid's executable to THIS fleet
// process's executable (os.Executable). A match proves the target is the
// same fleet binary (legacy-path ownership proof, codex iter-6 [P2]).
//
// Resolution (codex iter-8 [P1] — darwin must actually reap): Linux reads
// /proc/<pid>/exe; darwin tries `ps -o comm=` then `lsof` txt. When BOTH
// sides yield absolute paths we compare them (EvalSymlinks-resolved) — a
// strong match. When the target's path is only a basename (a PATH-launched
// `fleet` whose comm is just "fleet"), we fall back to a basename match
// against our own exe basename so darwin can still reap (a weaker but
// real signal; combined with the argv `fleet drain` re-probe + pid age
// gate, the legacy blast radius stays narrow). Unreadable target →
// exeProbeFailed (don't kill on ambiguity).
func sameFleetExe(pid int) exeProbe {
	self, err := os.Executable()
	if err != nil {
		return exeProbeFailed
	}
	other, abs, ok := procExePath(pid)
	if !ok {
		return exeProbeFailed
	}
	if abs {
		if resolvePath(other) == resolvePath(self) {
			return exeProbeMatch
		}
		return exeProbeMismatch
	}
	// Target gave only a basename — compare basenames.
	if filepath.Base(other) == filepath.Base(self) {
		return exeProbeMatch
	}
	return exeProbeMismatch
}

// procExePath returns the executable path of pid and whether it is
// ABSOLUTE. Linux: readlink /proc/<pid>/exe (absolute). Darwin/other:
// `ps -p <pid> -o comm=` (often absolute, sometimes argv0/basename), then
// `lsof -p <pid> -Ftn` txt entry (absolute) as a stronger fallback.
// (path, abs, false) on total failure.
func procExePath(pid int) (path string, abs bool, ok bool) {
	if link, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/exe"); err == nil && link != "" {
		return link, filepath.IsAbs(link), true
	}
	if out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=").Output(); err == nil {
		if p := strings.TrimSpace(string(out)); p != "" {
			if filepath.IsAbs(p) {
				return p, true, true
			}
			// Bare name from comm — try lsof for an absolute path before
			// settling for the basename.
			if lp, lok := lsofTxtPath(pid); lok {
				return lp, true, true
			}
			return p, false, true
		}
	}
	if lp, lok := lsofTxtPath(pid); lok {
		return lp, true, true
	}
	return "", false, false
}

// lsofTxtPath returns the absolute executable (txt) path of pid via
// `lsof -p <pid> -Ftn`. Scans for the `t`(ype)==REG following a `txt`
// fd marker; returns the first program-text file. (path, false) on any
// failure / lsof-absent host.
func lsofTxtPath(pid int) (string, bool) {
	out, err := exec.Command("lsof", "-p", strconv.Itoa(pid), "-Fn").Output()
	if err != nil {
		return "", false
	}
	// -Fn output is line-oriented; an `n`-prefixed line carries a path.
	// The first absolute path that exists is a good exe proxy on darwin
	// (the binary is among the earliest mapped files). We pick the first
	// `n/...` whose basename matches our own exe basename if possible,
	// else the first absolute path.
	self, _ := os.Executable()
	wantBase := ""
	if self != "" {
		wantBase = filepath.Base(self)
	}
	var firstAbs string
	for _, ln := range strings.Split(string(out), "\n") {
		if !strings.HasPrefix(ln, "n/") {
			continue
		}
		p := ln[1:]
		if firstAbs == "" {
			firstAbs = p
		}
		if wantBase != "" && filepath.Base(p) == wantBase {
			return p, true
		}
	}
	if firstAbs != "" {
		return firstAbs, true
	}
	return "", false
}

// resolvePath best-effort canonicalizes p via EvalSymlinks; on failure
// it returns p unchanged (the comparison then falls back to the raw
// path, which is still correct for a non-symlinked install).
func resolvePath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// procDrainState is a three-way argv probe for the guarded kill: probe
// failed (ambiguous), confirmed NOT a fleet-drain, or confirmed drain.
type drainProbe int

const (
	drainProbeIsDrain drainProbe = iota
	drainProbeNotDrain
	drainProbeFailed
)

func procDrainState(pid int) drainProbe {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "args=").Output()
	if err != nil {
		return drainProbeFailed
	}
	argv := strings.TrimSpace(string(out))
	if argv == "" {
		return drainProbeFailed
	}
	if argvIsFleetDrain(argv) {
		return drainProbeIsDrain
	}
	return drainProbeNotDrain
}

// removeDrainRunFile is the production RemoveDrainRun. RE-READS the
// run-record and unlinks ONLY if it still carries the pid_start that was
// classified (codex [P2] TOCTOU close): between ListDrainRuns and this
// delete, the old PID could exit and a NEW `fleet drain` could reuse the
// number and overwrite <pid>.json — a bare unlink would drop the fresh
// drain's record, leaving it un-reapable if it later wedges. A pid_start
// mismatch → skip the delete (the file now belongs to a different run).
// ENOENT-tolerant so two operators racing `fleet gc --apply` don't see
// spurious errors. An empty classified pid_start (legacy/unfingerprinted
// record) falls back to a bare delete — there's nothing to compare.
func removeDrainRunFile(run DrainRun) error {
	if run.Path == "" {
		return nil
	}
	if run.PidStart != "" {
		data, rerr := os.ReadFile(run.Path)
		if rerr != nil {
			if os.IsNotExist(rerr) {
				return nil // already gone — idempotent
			}
			return fmt.Errorf("re-read %s: %w", run.Path, rerr)
		}
		var cur DrainRun
		if jerr := json.Unmarshal(data, &cur); jerr == nil && cur.PidStart != "" && cur.PidStart != run.PidStart {
			// The file was overwritten by a new drain reusing the PID —
			// do NOT delete the fresh record.
			return nil
		}
	}
	if err := os.Remove(run.Path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", run.Path, err)
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
		// PidStart MUST be the SAME fingerprint format killDrainGuarded
		// re-derives via procStartTime (codex [P1]) — a raw lstart here
		// would never equal the guard's prefixed "linux-stat:"/"lstart:"
		// value, so the guard would treat it as PID reuse and skip the
		// SIGTERM, leaving the legacy drain alive. Empty fingerprint (read
		// failed) → the guard falls back to a bare liveness check.
		fp, _ := procStartFingerprint(pid)
		procs = append(procs, DrainProcInfo{
			Pid:      pid,
			PidStart: fp,
			Exe:      firstField(args),
			Sleeping: isSleepingState(stateTok),
			Age:      age,
		})
	}
	return procs, nil
}

// procStartTime returns the per-process start fingerprint used to defeat
// PID reuse. MUST format-match cmd/fleet's drain-record producer
// (procStartTimeForSelf) so the reaper's comparison holds.
//
// Resolution (codex [P2]): `ps lstart` has only 1-second granularity, so
// a stale drain PID reused within the SAME second by another `fleet
// drain` could spuriously match. To raise resolution we prefer the
// Linux per-boot starttime from /proc/<pid>/stat (field 22, in clock
// ticks — sub-second and monotonic), prefixed "linux-stat:". On darwin
// (no /proc) we fall back to lstart, prefixed "lstart:". A same-second
// reuse is then only a residual on darwin, further narrowed by the
// argv re-probe in the guard (a reuse by a NON-drain command is
// excluded). The prefix keeps the two encodings unambiguous so a
// cross-platform record never false-matches.
func procStartTime(pid int) (string, error) {
	return procStartFingerprint(pid)
}

// procStartFingerprint is shared between gc's reaper and (by identical
// reimplementation) cmd/fleet's drain producer. Keep them byte-identical.
func procStartFingerprint(pid int) (string, error) {
	if hi, ok := linuxProcStarttime(pid); ok {
		return "linux-stat:" + hi, nil
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "lstart=").Output()
	if err != nil {
		return "", fmt.Errorf("ps lstart pid %d: %w", pid, err)
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return "", fmt.Errorf("ps lstart pid %d: empty", pid)
	}
	return "lstart:" + s, nil
}

// linuxProcStarttime reads field 22 (starttime, clock ticks since boot)
// from /proc/<pid>/stat — a sub-second, monotonic, per-boot start
// fingerprint that defeats same-second PID reuse on Linux. Returns
// (value, false) on any non-Linux / read / parse failure so the caller
// falls back to lstart. The comm field (field 2) can contain spaces and
// parens, so we split on the LAST ')' before counting fields.
func linuxProcStarttime(pid int) (string, bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", false
	}
	s := string(data)
	close := strings.LastIndexByte(s, ')')
	if close < 0 || close+2 >= len(s) {
		return "", false
	}
	fields := strings.Fields(s[close+2:]) // after "comm) "
	// After comm, field indices shift: original field 3 (state) is now
	// fields[0]; field 22 (starttime) is fields[19].
	if len(fields) < 20 {
		return "", false
	}
	st := fields[19]
	if _, perr := strconv.ParseUint(st, 10, 64); perr != nil {
		return "", false
	}
	return st, true
}

// rootValueFlags are the fleet ROOT persistent flags that consume the
// NEXT argv token as their value (so the token after them is NOT the
// subcommand). `--engine codex drain` → skip `codex`, match `drain`.
// Bool root flags (--codex/--claude) take no value and are handled by
// the generic flag-skip. Mirrors cmd/fleet/main.go's root.PersistentFlags.
var rootValueFlags = map[string]bool{
	"--engine": true,
	"-engine":  true,
}

// argvIsFleetDrain matches a `fleet [root-flags] drain ...` command line.
// Requires the fleet binary (basename "fleet") AND that the resolved
// SUBCOMMAND (the first bare token after the binary, skipping root flags
// and their values) is `drain`. Handles `--engine codex drain`,
// `--engine=codex drain`, and `-codex drain` (codex [P2]); a different
// fleet subcommand (e.g. `fleet tasks add drain`) is correctly excluded
// because `tasks` is the resolved subcommand, not `drain`.
func argvIsFleetDrain(argv string) bool {
	fields := strings.Fields(argv)
	if len(fields) < 2 {
		return false
	}
	exe := filepath.Base(fields[0])
	if exe != "fleet" && filepath.Base(exe) != "fleet" {
		return false
	}
	for i := 1; i < len(fields); i++ {
		f := fields[i]
		if strings.HasPrefix(f, "-") {
			// `--engine=codex` carries its value inline → just skip it.
			if strings.Contains(f, "=") {
				continue
			}
			// `--engine codex` consumes the next token as its value.
			if rootValueFlags[f] && i+1 < len(fields) {
				i++ // skip the value token
			}
			continue
		}
		// First bare token = the resolved subcommand.
		return f == "drain"
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
