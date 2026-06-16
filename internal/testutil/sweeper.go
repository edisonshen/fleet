// Package testutil hosts test-only helpers that don't belong inside
// production packages. Sub-packages (e.g. tmuxtest) carry the bigger
// shared fixtures; this top-level file provides the test-suite sweeper
// used by each test package's TestMain to reap stale
// /tmp/fleet-test-*.sock debris.
//
// Per feedback_fleet_owns_its_resources.md (operator postmortem
// 2026-05-21, 3,570 leaked sockets / 4 GB memory warning): tests must
// clean up their own resources. The canonical per-test cleanup lives
// in internal/testutil/tmuxtest.RequireTmux. This sweeper is the
// belt-and-suspenders layer: even if a test panics before its
// t.Cleanup runs, the next test package's TestMain reaps the orphan
// at startup and on exit.
//
// Why this package does NOT import internal/gc: gc imports tmux,
// spawn, agent, state. The test packages most likely to leak sockets
// (internal/tmux, internal/spawn) are gc's own dependencies — pulling
// gc back in via testutil would create import cycles in test builds.
// The sweep policy here is intentionally a small subset of gc's
// (sockets only, no orphan-agents / orphan-tmux / worktrees) so the
// duplication is one strscan loop, not a maintenance burden. The
// `fleet gc` CLI still uses the full gc.Reconcile path for operator-
// invoked cleanup; this sweeper is just the test-infrastructure half.
package testutil

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Sweep reaps stale /tmp/fleet-test-*.sock files older than maxAge.
// Intended to be called from each test package's TestMain at suite
// start AND end. Errors are returned wrapped so a TestMain that
// surfaces them can include the dir in the message.
//
// Production use:
//
//	func TestMain(m *testing.M) {
//	    _ = testutil.Sweep(time.Hour)
//	    code := m.Run()
//	    _ = testutil.Sweep(time.Hour)
//	    os.Exit(code)
//	}
//
// Errors are intentionally non-fatal at the call site (best-effort
// cleanup; failing the test suite on a sweep glitch would be worse
// than letting the CI gate catch leaks at the assertion step).
//
// Sweep reaps stale leaked sockets on the REAL /tmp — it deliberately does
// NOT read FLEET_GC_SCAN_DIR.
//
// ci-perf-pr1 socket-leak P0 (2026-06-13): an earlier rev routed Sweep
// through FLEET_GC_SCAN_DIR (the SAME env the in-test gc reconcile PROBE
// reads), so every TestMain's IsolateSweepDir pointed BOTH the probe and the
// cleanup sweep at an empty decoy — which silently DISABLED the start-of-run
// safety-net sweep of real /tmp. Interrupted runs (panic/SIGKILL skipping
// t.Cleanup) then leaked tmux servers forever because nothing re-swept /tmp.
//
// The two concerns are now DECOUPLED:
//   - the gc reconcile PROBE (the in-test grind that walks every
//     fleet-test-*.sock with `tmux -S <sock> ls`) stays isolated to an empty
//     decoy via FLEET_GC_SCAN_DIR — that is the dirty-/tmp hang vector;
//   - this cleanup SWEEP targets real /tmp again so a leaked server from a
//     prior crashed run is reaped at the next suite start.
//
// Real-/tmp sweeping is SAFE despite the parallel `go test ./...` namespace:
// SweepDir liveness-gates every kill (freshness window + socketLive probe), so
// a fresh socket owned by a sibling package's live tmux server is spared. On a
// clean CI runner /tmp holds no fleet-test-*.sock, so the start sweep is a
// fast no-op.
func Sweep(maxAge time.Duration) error {
	return SweepDir(realTmpDir, maxAge)
}

// realTmpDir is the host /tmp — the cleanup sweep target. A package-level
// const (not FLEET_GC_SCAN_DIR) so the sweep can never be redirected at the
// probe decoy again (the regression this PR fixes).
const realTmpDir = "/tmp"

// IsolateSweepDir points FLEET_GC_SCAN_DIR at a fresh empty decoy dir for the
// whole test binary and returns a cleanup func that restores the prior env and
// removes the decoy. Call it FIRST in a TestMain so the start/end Sweep calls
// scan the decoy instead of the host /tmp. There is no *testing.T in TestMain,
// so this hand-rolls the env save/restore instead of t.Setenv.
//
// ci-perf-pr1 socket-leak P0 (2026-06-13): this isolates ONLY the in-test gc
// reconcile PROBE (the grind that walks every fleet-test-*.sock with
// `tmux -S <sock> ls`). It NO LONGER redirects testutil.Sweep — Sweep targets
// real /tmp unconditionally now (see Sweep's doc for why conflating the two
// disabled the safety-net sweep). The canonical TestMain shape is:
//
//	func TestMain(m *testing.M) {
//	    cleanup := testutil.IsolateSweepDir() // isolate the gc PROBE to a decoy
//	    _ = testutil.Sweep(time.Hour)         // safety-net sweep of REAL /tmp
//	    code := m.Run()
//	    _ = testutil.ForceReapTestServers()   // force-reap THIS suite's leftovers (gated)
//	    cleanup()
//	    os.Exit(code) // cleanup() BEFORE os.Exit — os.Exit skips defers
//	}
func IsolateSweepDir() func() {
	prev, had := os.LookupEnv("FLEET_GC_SCAN_DIR")
	// Prefix `fleet-gcdecoy-` (NOT `fleet-test-`) on purpose: this decoy is an
	// EMPTY scaffolding dir — it never holds a socket — so it is never a real
	// leak. CI's leak gate matches `/tmp/fleet-test-*` to catch socket-bearing
	// debris; an empty decoy under that glob is a guaranteed false positive that
	// reds the "Assert no /tmp/fleet-test-* leak" step (PR #233). Keeping the
	// prefix off `fleet-test-` makes the decoy invisible to the gate; the
	// cleanup func below + the per-package TestMain calling it before os.Exit are
	// what reap it. (An earlier rev used `fleet-test-` "for visibility on a
	// SIGKILL-skipped cleanup" — but an empty dir is not the socket leak the gate
	// guards, and on CI those decoys tripped the gate on the happy path.)
	decoy, err := os.MkdirTemp("", "fleet-gcdecoy-sweep-")
	if err != nil {
		// MkdirTemp failed: we can't isolate. Rather than SILENTLY falling back
		// to the prior (possibly /tmp) behavior — which would re-arm the very
		// hang this seam prevents with no trace — emit a loud stderr diagnostic
		// (feedback_surface_dont_silo) so a CI/dev run that suddenly grinds /tmp
		// has a breadcrumb. We still return rather than panic: a TestMain must
		// not abort the whole package over a transient tmp failure (claude
		// adversarial F4).
		fmt.Fprintf(os.Stderr,
			"testutil.IsolateSweepDir: WARNING could not create decoy scan dir (%v); "+
				"FLEET_GC_SCAN_DIR left as-is — the test sweep may scan real /tmp and grind on leaked sockets\n",
			err)
		return func() {}
	}
	_ = os.Setenv("FLEET_GC_SCAN_DIR", decoy)
	return func() {
		if had {
			_ = os.Setenv("FLEET_GC_SCAN_DIR", prev)
		} else {
			_ = os.Unsetenv("FLEET_GC_SCAN_DIR")
		}
		_ = os.RemoveAll(decoy)
	}
}

// SweepDir is Sweep with the scan directory injectable for tests.
// Behavior is identical to the production call (which uses /tmp) — the
// dir parameter exists only so the unit test in sweeper_test.go can
// run against t.TempDir() instead of mutating the operator's real /tmp.
//
// Algorithm matches internal/gc.reconcileSockets for the KindSockets
// family:
//
//  1. List entries matching fleet-test-*.sock under dir.
//  2. Skip entries younger than maxAge (within freshness window).
//  3. Probe whether a tmux server is still bound to the socket. If
//     yes, keep it (removing a live socket would strand the bound
//     server's clients — see codex iter-4 [P1] history in
//     internal/gc/gc.go:295).
//  4. Otherwise unlink. ENOENT collapses to success (concurrent
//     removal is fine).
func SweepDir(dir string, maxAge time.Duration) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("testutil.SweepDir read %s: %w", dir, err)
	}
	now := time.Now()
	var firstErr error
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "fleet-test-") {
			continue
		}
		if !strings.HasSuffix(name, ".sock") {
			// Out of scope — `fleet-test-*` without `.sock` includes
			// tmuxtest's temp-dir contents which Go's t.TempDir
			// already reaps. Mirrors scanSocketsDir in internal/gc.
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) < maxAge {
			continue // within freshness window — keep
		}
		path := filepath.Join(dir, name)
		if socketLive(path) {
			continue // live tmux server still bound — keep
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			if firstErr == nil {
				firstErr = fmt.Errorf("testutil.SweepDir remove %s: %w", path, err)
			}
		}
	}
	return firstErr
}

// socketLive probes whether a tmux server is bound to path. Returns
// true ONLY if `tmux -S <path> list-sessions` succeeds. File-gone /
// no-server / probe-error all map to false. Mirrors
// internal/gc.socketLiveOnDisk — duplicated here rather than imported
// to keep this package out of the gc dependency tree (gc → tmux →
// would cycle back if tmux's TestMain imported testutil → gc).
func socketLive(path string) bool {
	if _, err := os.Stat(path); err != nil {
		return false
	}
	if err := exec.Command("tmux", "-S", path, "list-sessions").Run(); err != nil {
		return false
	}
	return true
}

// ForceReapTestServers is the GATED suite-teardown force-reap. It kills
// every `fleet-<id>` tmux server this suite left on REAL /tmp —
// file-BOUND (socket still on disk) AND file-LESS (socket already
// unlinked, daemon alive) — but ONLY when no OTHER `go test` / `*.test`
// process is alive anywhere on the host.
//
// ci-perf-pr1 socket-leak P0 (2026-06-13): even when a per-test
// t.Cleanup is skipped on a crash (panic / SIGKILL / ^C), this end-of-run
// reap kills the leftover servers so they never accumulate. The HOST
// QUIESCENCE GATE is load-bearing AND it EXCLUDES THIS PROCESS: this
// function runs from inside a TestMain, so the CURRENT `*.test` binary
// (and its parent `go test` driver) are still alive — counting them would
// pin the gate closed forever and the teardown reap would never fire for
// the suite that created the leak. We exclude self + parent so the gate
// opens for OUR OWN leftovers while a SIBLING package's still-running
// `*.test` (under `go test ./...`, parallel) keeps the gate closed and
// its live servers spared (the SweepAll sibling-kill regression the doc
// below warns about).
//
// Best-effort: a probe failure or a `ps` we cannot read leaves the gate
// CLOSED (spare) — an ambiguous host is treated as "a test might be
// running", never reaped.
//
// DURABLE BACKSTOP (codex iter-4 [P2]): if the package that finishes LAST in
// a parallel `go test ./...` is one without a ForceReapTestServers TestMain,
// a file-less daemon a sibling left can survive this gated reap — and CI's
// leak gate only checks socket FILES, not file-less daemons. That residual is
// NOT silently tolerated: the AUTHORITATIVE reaper is `fleet gc --apply
// --aggressive`, which (post-2026-06-13) classifies + PID-reaps file-less
// fleet-test daemons when the host is go-test-quiescent. The coord runs
// reconcile on every fleet command, so any escapee is reaped on the next
// fleet invocation — this teardown hook is the FAST path, `fleet gc` is the
// guaranteed one. (A bounded wait/retry here would risk hanging a TestMain on
// a busy CI host; deferring to the durable tool-side reaper is the
// fleet-owns-its-resources-correct boundary.)
func ForceReapTestServers() error {
	if anyOtherGoTestParentAlive(os.Getpid(), os.Getppid()) {
		return nil // another test process is alive — spare (fail-safe)
	}
	// File-bound: socket file present + bound server → force-kill+unlink.
	if err := SweepAllDir(realTmpDir); err != nil {
		return err
	}
	// File-less: live `tmux -S /tmp/fleet-test-*.sock` daemons whose socket
	// file is gone — unreachable by SweepAllDir's path scan; kill by PID.
	reapFileLessTestTmux()
	return nil
}

// procExeBaseFromArgs returns the basename of argv[0] from a full argv
// string. We classify on argv[0] (the executable), NOT `ps comm` — macOS
// TRUNCATES comm for long paths (a `go test ./...` binary at
// `/var/.../go-build.../bNNN/handoffop.test` shows comm as only
// `/var/folders/69/`, dropping the `.test` suffix), which once made the
// quiescence gate blind to a live sibling test and force-killed its tmux
// session mid-run. macOS may also paren-wrap argv[0] for the live process
// (`(spawn.test)`), so strip surrounding parens.
func procExeBaseFromArgs(args string) string {
	fields := strings.Fields(args)
	if len(fields) == 0 {
		return ""
	}
	exe := fields[0]
	if i := strings.LastIndexAny(exe, "/\\"); i >= 0 {
		exe = exe[i+1:]
	}
	exe = strings.TrimPrefix(exe, "(")
	exe = strings.TrimSuffix(exe, ")")
	return exe
}

// argvIsGoTest reports whether a full argv is a real go-test PARENT: argv[0]
// basename ends `.test`, or argv[0] is `go` with a bare `test` subcommand
// token. Ignores any `.test` mention OUTSIDE argv[0] (a leaked tmux server's
// `-e FLEET_BIN=...spawn.test` must NOT count).
func argvIsGoTest(args string) bool {
	exe := procExeBaseFromArgs(args)
	if strings.HasSuffix(exe, ".test") {
		return true
	}
	if exe == "go" {
		fields := strings.Fields(args)
		for i := 1; i < len(fields); i++ {
			if fields[i] == "test" {
				return true
			}
		}
	}
	return false
}

// anyOtherGoTestParentAlive reports whether a real `go test` driver or
// compiled `*.test` binary OTHER THAN excludePIDs is alive on the host. It
// classifies on argv[0] (see procExeBaseFromArgs) — NOT the full argv — so
// a leaked tmux server whose argv merely carries `-e FLEET_BIN=...spawn.test`
// does NOT count. (testutil twin of internal/gc.anyGoTestRunning; duplicated
// rather than imported to keep testutil out of gc's dependency tree —
// gc → tmux → would cycle if tmux's TestMain imported testutil → gc.)
// excludePIDs is the caller's own PID + PPID (the current `*.test` + its
// `go test` driver) so the gate can open for the current suite's OWN
// teardown. Fail-safe: any inability to read the process table returns true
// (spare).
func anyOtherGoTestParentAlive(excludePIDs ...int) bool {
	excluded := make(map[int]bool, len(excludePIDs))
	for _, p := range excludePIDs {
		excluded[p] = true
	}
	out, err := exec.Command("ps", "-axo", "pid=,args=").Output()
	if err != nil {
		return true // cannot prove quiescence → spare
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		pid, perr := strconv.Atoi(parts[0])
		if perr != nil || len(parts) < 2 {
			continue
		}
		if excluded[pid] {
			continue // this is our own *.test / go test driver
		}
		if argvIsGoTest(strings.TrimSpace(parts[1])) {
			return true
		}
	}
	return false
}

// fleetTestSocketRe validates that an extracted socket path is in the
// fleet-test namespace — the only sockets this reaper may touch.
var fleetTestSocketRe = regexp.MustCompile(`^/[^\s]*/fleet-test-[^\s]*\.sock$`)

// tmuxServerTitleRe matches the LINUX tmux SERVER proctitle
// `tmux: server (<socket>)` (codex iter-9 [P1]: the ubuntu-latest CI runner
// reports this shape, not the macOS `tmux -S … new-session` argv).
var tmuxServerTitleRe = regexp.MustCompile(`^tmux: server \((/[^\s)]+)\)`)

// tmuxServerSocketFromArgv extracts tmux's OWN server socket from a tmux
// process's `ps args`, recognizing BOTH the Linux server title
// `tmux: server (<socket>)` and the macOS/client `tmux -S <path> …` argv,
// returning ("", false) unless the socket is in the fleet-test namespace.
// (testutil twin of internal/gc.tmuxServerSocketFromArgv — see that doc for
// why anchoring to tmux's own option, not any `-S` substring, prevents
// PID-killing an operator-owned tmux server whose pane command happens to
// mention such a path. codex iter-1 [P2], iter-9 [P1].)
func tmuxServerSocketFromArgv(args string) (string, bool) {
	if m := tmuxServerTitleRe.FindStringSubmatch(args); m != nil {
		if fleetTestSocketRe.MatchString(m[1]) {
			return m[1], true
		}
		return "", false
	}
	fields := strings.Fields(args)
	if len(fields) == 0 {
		return "", false
	}
	if procExeBaseFromArgs(args) != "tmux" {
		return "", false
	}
	for i := 1; i < len(fields); i++ {
		tok := fields[i]
		if !strings.HasPrefix(tok, "-") {
			return "", false // first non-option token = the tmux command
		}
		if tok == "-S" {
			if i+1 >= len(fields) {
				return "", false
			}
			sock := fields[i+1]
			if !fleetTestSocketRe.MatchString(sock) {
				return "", false
			}
			return sock, true
		}
		if strings.HasPrefix(tok, "-S") { // glued `-S/path`
			sock := tok[2:]
			if !fleetTestSocketRe.MatchString(sock) {
				return "", false
			}
			return sock, true
		}
	}
	return "", false
}

// reapFileLessTestTmux SIGTERMs live `tmux -S /tmp/fleet-test-*.sock`
// daemons whose socket FILE is gone. Caller (ForceReapTestServers) has
// already confirmed host quiescence, so every such daemon is an orphan.
// Best-effort: enumeration / kill failures are swallowed (the CI leak
// gate is the backstop).
func reapFileLessTestTmux() {
	out, err := exec.Command("ps", "-axo", "pid=,args=").Output()
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		pid, perr := strconv.Atoi(parts[0])
		if perr != nil || len(parts) < 2 {
			continue
		}
		rest := strings.TrimSpace(parts[1])
		sock, ok := tmuxServerSocketFromArgv(rest)
		if !ok {
			continue
		}
		// Constrain to realTmpDir (codex iter-3 [P2]): the file-bound
		// SweepAllDir only touches /tmp, and the gc twin has the same
		// filepath.Dir==scanDir guard. Without this, a developer/CI host's
		// own live `tmux -S /home/me/fleet-test-debug.sock` daemon (socket
		// unlinked) would be SIGTERM'd — outside Fleet's /tmp test namespace.
		if filepath.Dir(sock) != filepath.Clean(realTmpDir) {
			continue
		}
		if _, statErr := os.Stat(sock); statErr == nil {
			continue // socket file present — SweepAllDir already handled it
		}
		// Re-verify the PID is STILL the SAME tmux -S <sock> daemon
		// IMMEDIATELY before signaling (codex iter-8 [P2]): the snapshot
		// above can go stale — the daemon may exit and the OS reuse the PID
		// between enumeration and Signal, so a stale snapshot could SIGTERM an
		// unrelated process. Mirrors gc.killTmuxProcByPID's expectSock recheck.
		out2, perr := exec.Command("ps", "-o", "args=", "-p", strconv.Itoa(pid)).Output()
		if perr != nil {
			continue // pid gone / probe failed — do not signal (best-effort)
		}
		if got, ok := tmuxServerSocketFromArgv(strings.TrimSpace(string(out2))); !ok || got != sock {
			continue // recycled to a different process — never signal it
		}
		if proc, ferr := os.FindProcess(pid); ferr == nil {
			_ = proc.Signal(syscall.SIGTERM)
		}
	}
}

// SweepAll is the suite-teardown variant of Sweep. Reaps EVERY
// /tmp/fleet-test-*.sock regardless of freshness AND regardless of
// whether a tmux server is still bound. Use ONLY from TestMain
// teardown (after m.Run()) — once `go test` is exiting, a live test
// socket is by definition an orphan whose owning test process either
// panicked, called os.Exit, or was killed mid-run.
//
// Closes the gap that lets bypassed-t.Cleanup orphans (7-day-old
// claude/tmux procs in the 2026-05-29 OOM, per
// docs/DESIGN-lifecycle-leak-recurrence.md PR-A root cause #1) survive.
// Sweep (with freshness + socketLive guards) is still correct for the
// SUITE-START sweep where another concurrent `go test` may legitimately
// own a fresh live socket; SweepAll is intentionally narrower in scope.
//
// SweepAll force-reaps real /tmp (never FLEET_GC_SCAN_DIR). It is the
// file-bound half of ForceReapTestServers' teardown reap — and is ONLY
// safe behind that function's host-quiescence gate. Do NOT call SweepAll
// directly from a TestMain in place of Sweep: ungated, it would (a) grind
// real /tmp at suite START where a concurrent `go test ./...` legitimately
// owns fresh live sockets, and (b) force-kill those sibling-package
// sockets mid-run. The quiescence gate in ForceReapTestServers is what
// makes the teardown force-kill safe; the surviving start-of-run safety
// net is the guarded Sweep.
func SweepAll() error {
	return SweepAllDir(realTmpDir)
}

// SweepAllDir is SweepAll with the scan directory injectable for tests.
// Like SweepDir but bypasses BOTH the freshness window AND the
// socketLive() guard. For each `fleet-test-*.sock` entry:
//
//  1. If a tmux server is still bound to the socket, kill it via
//     `tmux -S <path> kill-server` (idempotent — exits non-zero with no
//     server, which we ignore). This stops the leaked claude/tmux
//     process before the file is unlinked, so we don't strand orphans
//     after the .sock disappears.
//  2. Unlink the socket file. ENOENT is success.
func SweepAllDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("testutil.SweepAllDir read %s: %w", dir, err)
	}
	var firstErr error
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "fleet-test-") {
			continue
		}
		// tmux 3.x writes a companion lock `<socket>.sock.lock` next to each
		// socket; a killed server leaves it behind. Reap those too — the CI
		// leak gate matches `fleet-test-*` (broad), so a stranded `.sock.lock`
		// reds the run even after every socket is gone (run 27515608918).
		if strings.HasSuffix(name, ".sock.lock") {
			if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
				if firstErr == nil {
					firstErr = fmt.Errorf("testutil.SweepAllDir remove %s: %w", filepath.Join(dir, name), err)
				}
			}
			continue
		}
		if !strings.HasSuffix(name, ".sock") {
			continue
		}
		path := filepath.Join(dir, name)
		// Symlink/non-socket guard (codex iter-8 [P2], mirrors
		// gc.firstFleetSession): /tmp is world-writable, so a stale or
		// malicious fleet-test-*.sock SYMLINK could point at the operator's
		// default tmux socket; `tmux -S <symlink> kill-server` would then
		// terminate the operator's real server. Lstat (no deref) + ModeSocket:
		// only kill-server a REAL Unix domain socket. A symlink / regular file
		// / dir in the namespace is fleet-test debris — unlink it, never
		// kill-server through it.
		fi, lerr := os.Lstat(path)
		isRealSocket := lerr == nil && fi.Mode()&os.ModeSocket != 0
		if isRealSocket {
			// kill-server is idempotent: tmux exits non-zero when no server is
			// running, which is fine — we only want any bound server reaped
			// before the socket file is unlinked. Best-effort.
			_ = exec.Command("tmux", "-S", path, "kill-server").Run()
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			if firstErr == nil {
				firstErr = fmt.Errorf("testutil.SweepAllDir remove %s: %w", path, err)
			}
		}
		// Companion lock for THIS socket, in case the directory scan already
		// passed its alphabetical position.
		_ = os.Remove(path + ".lock")
	}
	return firstErr
}
