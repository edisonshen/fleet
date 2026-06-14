package gc

// live_test_sockets.go — orphan LIVE test-socket tmux reaper, a
// sub-classifier of the `orphan-tmux` kind (DESIGN-lifecycle-leak-
// recurrence.md PR-D).
//
// PROBLEM. The `sockets` kind reaps bare /tmp/fleet-test-*.sock FILES,
// but DELIBERATELY spares any socket whose tmux server is still alive
// (reconcileSockets's SocketLive gate) so a long-running test fixture
// isn't stranded mid-run. That left a gap: a `fleet-<id>` tmux server
// bound to a /tmp/fleet-test-*.sock left behind after the test fleet went
// quiescent is NEVER reaped — the operator had to `tmux -S <sock>
// kill-server` BY HAND during the 2026-05-29 OOM. That violates
// feedback_fleet_owns_its_resources.md ("operator never manually
// kills"). This classifier closes the gap.
//
// FLOW:
//
//	/tmp/fleet-test-*.sock  ──→  tmux -S <sock> ls (fleet-<id> session?)
//	                                     │
//	                          host-wide gate: is ANY `go test` /
//	                          *.test process alive on the host?
//	                          (pgrep — NOT lsof on the socket, because
//	                           tmux daemonizes and the socket FD is held
//	                           by the server, not the parent go test)
//	                                     │
//	              ┌──────────────────────┴───────────────────────┐
//	      go test alive (or probe failed)              host quiescent
//	         OwnerPID != 0 (spare)                      OwnerPID == 0
//	              │                                          │
//	         skip (in-flight / unknown,          surface (default) /
//	          fail-safe)                         would-kill / killed
//	                                             under --aggressive
//
// Why it lives under orphan-tmux (not sockets): it KILLS a live tmux
// server — that is operator-owned-tmux territory gated by --aggressive,
// exactly the surface-don't-silo escape hatch orphan-tmux already owns
// (feedback_surface_dont_silo.md). The `sockets` kind only unlinks dead
// files. Actions carry Kind=KindOrphanTmux so `fleet gc --kinds
// orphan-tmux` covers them and they group in the orphan-tmux output.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

// LiveTestSocket describes one live tmux server bound to a
// /tmp/fleet-test-*.sock. OwnerPID encodes whether the server may still
// belong to an in-flight `go test`:
//
//	OwnerPID == 0   — DEFINITIVELY orphan: no live `go test` exists on the
//	                  host, so no test could own this server. Reapable
//	                  under --apply --aggressive.
//	OwnerPID  > 0   — a live `go test` owns it (unit-test stubs set a real
//	                  PID here). Spared.
//	OwnerPID  < 0   — owner UNKNOWN (ownerProbeFailedPID): a `go test`
//	                  process IS alive on the host but cannot be attributed
//	                  to this specific socket, OR the probe failed. Spared
//	                  (fail-safe).
//
// Why not lsof the socket? A tmux server daemonizes — the `go test`
// process launches a transient `tmux` client that exits, leaving only
// the long-lived server holding the socket FD. `lsof -t <sock>` therefore
// sees `tmux`, not the parent `go test`, so per-socket FD attribution
// reports OwnerPID==0 for IN-FLIGHT tests and the kill path would reap a
// live test server (codex iter-1 [P1]). The sound primitive is a
// host-wide "is ANY go test running?" gate: a fleet-test socket is only
// reaped when the entire test fleet is quiescent.
type LiveTestSocket struct {
	// SocketPath is the /tmp/fleet-test-*.sock the server is bound to.
	SocketPath string
	// SessionName is the first fleet-<id> session on the server (used as
	// the Action.Target so the output reads like the rest of orphan-tmux).
	SessionName string
	// OwnerPID classifies ownership; see the type doc for the tri-state.
	OwnerPID int
	// ServerPID, when > 0, is the PID of a FILE-LESS orphan tmux daemon:
	// a live `tmux -S /tmp/fleet-test-*.sock` server whose socket FILE has
	// already been unlinked (the leak left by an interrupted run whose
	// t.Cleanup removed the .sock but the kill-server never landed, or a
	// partial sweep). Such a daemon is UNREACHABLE by socket path
	// (`tmux -S <gone-path> kill-server` errors "No such file or
	// directory"), so it must be killed by PID signal. ServerPID == 0
	// means the server is reachable on SocketPath (the normal disk-scan
	// case, killed via KillTmuxServer). The 2026-06-13 host had 399 of
	// these file-less daemons that NO gc path could reap.
	ServerPID int
}

// reconcileLiveTestSockets enumerates live test-socket tmux servers and
// classifies each. A server with a live go-test owner is healthy and
// produces no action (matches the orphan-rc-daemons healthy-continue
// shape). A server with no live owner is an orphan: surfaced by default,
// would-kill/killed under --aggressive (and only actually killed under
// --apply --aggressive). Without --aggressive the orphan stays surfaced
// even under --apply — the established surface-don't-silo default for
// live tmux state.
//
// Unwired ListLiveTestSockets (older Deps / narrow unit tests) is a
// no-op so the existing orphan-tmux tests are unaffected.
func reconcileLiveTestSockets(r *Report, opts Options, deps Deps) error {
	if deps.ListLiveTestSockets == nil {
		return nil
	}
	socks, err := deps.ListLiveTestSockets()
	if err != nil {
		return fmt.Errorf("list live test sockets: %w", err)
	}
	for _, s := range socks {
		if s.OwnerPID != 0 {
			// Non-zero OwnerPID = an in-flight test owns it (positive PID),
			// OR ownership is unknown / a live go test is running but
			// unattributable (ownerProbeFailedPID sentinel, negative).
			// Either way: never touch it, even under --aggressive --apply
			// (T4 + probe-failure spare). Only a definitive OwnerPID==0
			// ("no live go test exists on the host") is reaped.
			continue
		}
		target := s.SessionName
		if target == "" {
			target = s.SocketPath
		}
		// File-less orphan daemon (ServerPID > 0): unreachable by socket
		// path. Both the surface Reason and the apply path point operators at
		// the PID-reuse-safe `fleet gc` reap, NOT a raw kill (codex iter-5/6
		// [P2]): a pasted PID can race reuse, and only the gc apply path
		// re-verifies the PID is STILL the expected `tmux -S <sock>` daemon
		// (KillTmuxProc's expectSock recheck) before signaling.
		fileLess := s.ServerPID > 0
		hint := fmt.Sprintf("tmux -S %s kill-server", s.SocketPath)
		if fileLess {
			hint = "fleet gc --apply --aggressive --kinds orphan-tmux"
		}
		act := Action{
			Kind:   KindOrphanTmux,
			Target: target,
			Verb:   VerbSurface,
			Reason: fmt.Sprintf("live test-socket tmux server on %s with no live go-test owner; rerun with --apply --aggressive to kill (or `%s`)",
				s.SocketPath, hint),
		}
		if fileLess {
			// Target is a socket PATH (no session name), so the consumer's
			// default `tmux kill-session -t <Target>` synthesis is bogus.
			// CleanupHint is consumed VERBATIM as a shell command by
			// status.go/dispatch.go, so it must be command-ONLY — no
			// parenthesized prose (codex iter-6 [P2]: it would be a paste
			// syntax error). The explanation lives in Reason above.
			act.CleanupHint = "fleet gc --apply --aggressive --kinds orphan-tmux"
		}
		if opts.Aggressive {
			act.Verb = VerbWouldKill
			act.Reason = fmt.Sprintf("live test-socket tmux server on %s with no live go-test owner", s.SocketPath)
			if opts.Apply {
				switch {
				case fileLess:
					if deps.KillTmuxProc == nil {
						act.Reason = "kill seam unwired (set Deps.KillTmuxProc to apply)"
					} else if kerr := deps.KillTmuxProc(s.ServerPID, s.SocketPath); kerr != nil {
						act.Reason = fmt.Sprintf("kill failed: %v", kerr)
					} else {
						act.Verb = VerbKilled
					}
				case deps.KillTmuxServer == nil:
					act.Reason = "kill seam unwired (set Deps.KillTmuxServer to apply)"
				default:
					if kerr := deps.KillTmuxServer(s.SocketPath); kerr != nil {
						// Verb stays surface; a kill failure must NOT report killed.
						act.Reason = fmt.Sprintf("kill failed: %v", kerr)
					} else {
						act.Verb = VerbKilled
					}
				}
			}
		}
		r.Actions = append(r.Actions, act)
	}
	return nil
}

// ----------------- production wiring (DefaultDeps) ------------------

// listLiveTestSocketsOnDisk scans dir (production: gcScanDir(), default
// `/tmp`; tests inject a decoy) for fleet-test-*.sock files and probes
// each for a live fleet-<id> tmux server.
//
// Ownership is resolved ONCE per sweep via a host-wide gate, not per
// socket: see goTestOwnerVerdict. A fleet-test socket server is reapable
// (OwnerPID==0) ONLY when no `go test` process is alive anywhere on the
// host. If a `go test` IS running — even one we can't tie to a specific
// socket — every test-socket server is spared (OwnerPID set to the
// unknown sentinel). If the probe itself fails, fail-safe: spare.
//
// Best-effort: a missing tmux binary or a socket with no server simply
// drops that entry — the classifier degrades to "no orphans found",
// never to a spurious kill.
func listLiveTestSocketsOnDisk(dir string) ([]LiveTestSocket, error) {
	infos, err := scanSocketsDir(dir)
	if err != nil {
		return nil, err
	}
	// One host-wide ownership verdict for the whole sweep (see doc).
	ownerVerdict := goTestOwnerVerdict()
	var out []LiveTestSocket
	for _, info := range infos {
		sock := info.Path
		session, ok := firstFleetSession(sock)
		if !ok {
			continue // no live fleet-<id> server on this socket
		}
		out = append(out, LiveTestSocket{
			SocketPath:  sock,
			SessionName: session,
			OwnerPID:    ownerVerdict,
		})
	}
	// Plus the file-less orphan daemons (socket file already unlinked,
	// daemon still alive) — unreachable by the disk scan above. Same
	// quiescence verdict; killed by PID. Constrained to `dir` so the PID
	// kill never escapes the configured scan namespace (codex iter-2 [P2]).
	// This is what reaps the 399 file-less daemons the 2026-06-13 host
	// accumulated.
	out = append(out, listFileLessTestTmux(dir, ownerVerdict)...)
	return out, nil
}

// firstFleetSession returns the first fleet-<id> session name on the
// server bound to sock, and ok=false when no server / no fleet session
// responds. Uses `tmux -S <sock> ls` directly (NOT tmux.ListSessions,
// which targets FLEET_TMUX_SOCKET / the default server).
//
// SYMLINK GUARD (codex iter-2 [P2]): /tmp is world-writable, so a stale
// or hostile symlink named fleet-test-*.sock could point at the
// operator's DEFAULT tmux socket. Following it with `tmux -S` then
// kill-server would terminate the operator's real server if it happens
// to host a fleet-<id> session. Require the path to be an actual Unix
// domain socket (os.ModeSocket) via Lstat (no symlink deref) before
// probing — a regular file, dir, or symlink is rejected outright.
func firstFleetSession(sock string) (string, bool) {
	fi, err := os.Lstat(sock)
	if err != nil {
		return "", false
	}
	if fi.Mode()&os.ModeSocket == 0 {
		// Not a real socket (regular file / dir / symlink). A genuine tmux
		// server socket is a Unix domain socket; anything else in the
		// fleet-test-*.sock namespace is debris or an attack and must never
		// be probed-then-killed.
		return "", false
	}
	out, err := exec.Command("tmux", "-S", sock, "ls", "-F", "#{session_name}").Output()
	if err != nil {
		// no server / no sessions / connection refused — all "not live".
		return "", false
	}
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		if !strings.HasPrefix(name, "fleet-") {
			continue
		}
		id := strings.TrimPrefix(name, "fleet-")
		if id == "" || !fleetAgentIDPattern.MatchString(id) {
			// fleet-coord-* / operator-named fleet-debug are out of this
			// detector's blast radius (user-owns-tmux-config). Skip but keep
			// looking for an agent session on the same server.
			continue
		}
		return name, true
	}
	return "", false
}

// goTestOwnerVerdict returns the host-wide ownership classification used
// for EVERY fleet-test socket server in a sweep:
//
//	0                    — no `go test` process is alive on the host, so a
//	                       fleet-test socket server is definitively orphan.
//	ownerProbeFailedPID  — a `go test` IS running (or the probe failed):
//	                       ownership is ambiguous, spare every server.
//
// Why host-wide, not per-socket: a tmux server daemonizes, so `lsof` on
// the socket sees `tmux`, not the parent `go test` (codex iter-1 [P1]).
// We cannot attribute a specific socket to a specific test run, so the
// only sound, fail-safe gate is "are we sure NO test is running?" — and
// only then reap. A running test on ANY socket spares ALL of them; that
// is acceptable because the leak being closed is dead servers left after
// the test fleet went quiescent (the 2026-05-29 OOM), not a server
// stranded mid-run.
//
// Fail-safe: any probe error (pgrep missing, unexpected exit) returns the
// unknown sentinel so a flaky probe never drives a kill.
func goTestOwnerVerdict() int {
	if anyGoTestRunning() {
		return ownerProbeFailedPID
	}
	return 0
}

// ownerProbeFailedPID is a sentinel non-zero OwnerPID meaning "could not
// prove the host is test-quiescent" — the classifier treats any
// OwnerPID != 0 as "spare it", so an ambiguous probe never drives a kill
// (fail-safe).
const ownerProbeFailedPID = -1

// anyGoTestRunning reports whether at least one real `go test` driver or
// compiled `*.test` binary is alive on the host. It classifies on the
// process EXECUTABLE (argv[0] / `ps comm`), not the full argv.
//
// WHY EXECUTABLE-ONLY (the 2026-06-13 deadlock fix). The old form ran
// `pgrep -f '\.test[ /]'`, and `-f` matches the WHOLE argv. A leaked
// fleet-test tmux server's own argv is, verbatim:
//
//	tmux -S /tmp/fleet-test-XX.sock new-session -d -s fleet-<id> ... \
//	     -e FLEET_BIN=/var/.../go-build.../b168/spawn.test ... sh -c ...
//
// The `...spawn.test ` substring (a path ending `.test` followed by a
// space) matched the `\.test[ /]` needle — so every orphan tmux server
// pinned anyGoTestRunning()==true and SPARED ITSELF FOREVER. On the host
// that left 199 orphan servers that `fleet gc --apply --aggressive`
// reaped ZERO of, even with no real `go test` anywhere (verified). The
// gate was deadlocked by the very debris it was meant to reap.
//
// The real test PARENT is distinguishable: its EXECUTABLE is either the
// compiled binary `pkg.test` (argv[0] basename ends `.test`) or the `go`
// driver running the `test` subcommand (argv[0] basename == `go` AND a
// bare `test` token in argv). A leaked tmux server's executable is `tmux`
// — it only MENTIONS a `.test` path in a later `-e FLEET_BIN=` arg, which
// no longer counts.
//
// WHY argv[0], NOT `ps comm` (the 2026-06-13 sibling-kill fix). macOS
// `ps -o comm=` TRUNCATES a long path: a `go test ./...` binary lives at
// `/var/folders/.../go-build.../bNNN/handoffop.test`, and `comm` reports
// only the leading `/var/folders/69/` — the `.test` suffix is GONE. The
// gate then failed to see a live sibling test, OPENED, and a peer
// package's teardown force-reap killed handoffop's live tmux session
// mid-test (full-suite flake, never reproduced package-alone). The full
// argv (`ps args=`) carries the COMPLETE argv[0] path, so we derive the
// executable from argv[0]'s basename and never trust the truncated comm.
//
// Fail-safe defaults (unchanged intent): if the process table cannot be
// read at all, we cannot prove quiescence, so we return true (spare).

// goTestProcLister enumerates live processes as (argv) records. Seam:
// production uses psProcLister (a `ps` shell-out); the unit test injects a
// fake table so detection is deterministic without depending on the real
// process table.
var goTestProcLister = psProcLister

// procInfo is one live process: Args is the FULL argv (argv[0] is the
// executable PATH — the reliable classification source, since macOS
// `ps comm` truncates long paths), PID is the process id (used to kill
// file-less orphan daemons by signal).
type procInfo struct {
	PID  int
	Args string
}

// procExeBase returns the basename of argv[0] from a full argv string.
// macOS `ps` may wrap argv[0] in parens for the live process (e.g.
// `(spawn.test)`); strip them so the suffix check sees `spawn.test`.
func procExeBase(args string) string {
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

// isGoTestProc reports whether p is a real go-test PARENT: a compiled
// `*.test` binary, or the `go` driver invoking the `test` subcommand.
// It deliberately ignores any `.test` mention OUTSIDE argv[0]
// (e.g. a tmux server's `-e FLEET_BIN=...spawn.test`).
func isGoTestProc(p procInfo) bool {
	exe := procExeBase(p.Args)
	if strings.HasSuffix(exe, ".test") {
		return true // compiled pkg.test binary
	}
	if exe == "go" {
		// `go test ...` — the driver while compiling+running a package.
		// `go build` / `go vet` etc. must NOT count. The `test` subcommand
		// is the only place a BARE `test` token appears in a `go` argv:
		// build/vet/run targets are paths or import paths (`./test`,
		// `testdata`, `internal/test`), never the standalone word `test`.
		// Matching the bare token tolerates the rare `go -C <dir> test`
		// form (where `test` is not argv[1]) without mis-parsing flags.
		fields := strings.Fields(p.Args)
		for i := 1; i < len(fields); i++ {
			if fields[i] == "test" {
				return true
			}
		}
	}
	return false
}

func anyGoTestRunning() bool {
	procs, err := goTestProcLister()
	if err != nil {
		// Cannot enumerate processes → cannot prove quiescence → spare.
		return true
	}
	for _, p := range procs {
		if isGoTestProc(p) {
			return true
		}
	}
	return false
}

// psProcLister enumerates live processes via `ps -axo pid=,args=`. We do
// NOT request `comm` — macOS truncates it for long paths (the 2026-06-13
// sibling-kill bug). `args` carries the COMPLETE argv (argv[0] = the
// executable path), which is the reliable classification source. The line
// is `<pid> <argv...>`; pid is the leading numeric token, the rest is argv.
func psProcLister() ([]procInfo, error) {
	out, err := exec.Command("ps", "-axo", "pid=,args=").Output()
	if err != nil {
		// ps missing or failed: signal the caller to fail-safe (spare).
		return nil, err
	}
	var procs []procInfo
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Split into [pid, rest-of-argv]. SplitN keeps argv intact even
		// though it carries spaces.
		parts := strings.SplitN(line, " ", 2)
		pid, perr := strconv.Atoi(parts[0])
		if perr != nil {
			continue // malformed line (no leading numeric pid)
		}
		args := ""
		if len(parts) > 1 {
			args = strings.TrimSpace(parts[1])
		}
		procs = append(procs, procInfo{PID: pid, Args: args})
	}
	return procs, nil
}

// fleetTestSocketRe validates that an extracted socket path is in the
// fleet-test namespace — the only sockets this reaper may touch
// (user-owns-tmux-config: never the operator's default/custom servers).
var fleetTestSocketRe = regexp.MustCompile(`^/[^\s]*/fleet-test-[^\s]*\.sock$`)

// tmuxServerSocketFromArgv extracts tmux's OWN `-S <path>` server-socket
// option from a tmux argv, returning ("", false) unless:
//
//   - argv[0]'s basename is `tmux`, AND
//   - `-S <path>` appears among tmux's GLOBAL options (before the tmux
//     command word like `new-session`), AND
//   - <path> is in the fleet-test namespace.
//
// codex iter-1 [P2]: a prior regex matched any `-S /tmp/fleet-test-*.sock`
// substring ANYWHERE in the argv — including a pane command or env value on
// an OPERATOR-owned tmux server — and would then PID-kill that server. tmux
// global options precede the command, so we parse positionally and STOP at
// the first non-option token (the command), never scanning the command's
// own args. `-Spath` (glued, no space) is also a valid tmux form.
func tmuxServerSocketFromArgv(args string) (string, bool) {
	fields := strings.Fields(args)
	if len(fields) == 0 {
		return "", false
	}
	exe := fields[0]
	if i := strings.LastIndexAny(exe, "/\\"); i >= 0 {
		exe = exe[i+1:]
	}
	exe = strings.TrimPrefix(exe, "(")
	exe = strings.TrimSuffix(exe, ")")
	if exe != "tmux" {
		return "", false
	}
	for i := 1; i < len(fields); i++ {
		tok := fields[i]
		if !strings.HasPrefix(tok, "-") {
			// First non-option token = the tmux command (new-session, etc.).
			// tmux's own -S must appear before it; stop scanning so a -S in
			// the command's args is never picked up.
			return "", false
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
		if strings.HasPrefix(tok, "-S") {
			// Glued form `-S/path`.
			sock := tok[2:]
			if !fleetTestSocketRe.MatchString(sock) {
				return "", false
			}
			return sock, true
		}
	}
	return "", false
}

// listFileLessTestTmux finds live `tmux -S /tmp/fleet-test-*.sock` daemons
// whose socket FILE no longer exists on disk — the file-less orphans that
// the disk scan (listLiveTestSocketsOnDisk) cannot see and that
// `tmux -S <path> kill-server` cannot reach. Each becomes a LiveTestSocket
// with ServerPID set so the reaper kills it by PID signal. ownerVerdict
// is the shared host-wide quiescence verdict (a running test spares them
// all, identical to the file-bound path).
//
// Only daemons whose socket file is GONE are returned here; a daemon whose
// .sock still exists is already covered by the disk scan, so returning it
// here too would double-count.
//
// SCOPE (codex iter-2 [P2]): only daemons whose socket lived in `dir` (the
// configured scan namespace — production `/tmp`, tests a decoy) are
// returned. Without this, a `-S /home/me/fleet-test-debug.sock` server with
// an unlinked socket would be SIGTERM'd even when the scan dir is `/tmp` or
// a decoy — escaping the configured namespace. Best-effort: any lister
// error yields an empty slice (the file-bound path still runs).
func listFileLessTestTmux(dir string, ownerVerdict int) []LiveTestSocket {
	procs, err := goTestProcLister()
	if err != nil {
		return nil
	}
	var out []LiveTestSocket
	for _, p := range procs {
		sock, ok := tmuxServerSocketFromArgv(p.Args)
		if !ok {
			continue
		}
		if filepath.Dir(sock) != filepath.Clean(dir) {
			continue // outside the configured scan namespace — out of scope
		}
		if _, statErr := os.Stat(sock); statErr == nil {
			continue // socket file still present — the disk scan owns it
		}
		out = append(out, LiveTestSocket{
			SocketPath:  sock,
			SessionName: "", // file-less: cannot `tmux ls` to read it
			OwnerPID:    ownerVerdict,
			ServerPID:   p.PID,
		})
	}
	return out
}

// killTmuxProcByPID terminates a file-less orphan tmux daemon by PID. It
// re-verifies the process is STILL the SAME `tmux -S <expectSock>` daemon
// BEFORE signaling, so a recycled PID (the daemon exited and the OS
// reassigned its PID between enumeration and apply) is never killed.
//
// codex iter-4 [P2]: matching only the fleet-test-*.sock NAME pattern was
// not enough — a reused PID could be a `tmux -S /other/dir/fleet-test-X.sock`
// OUTSIDE the configured scan dir, escaping the namespace the enumeration
// pass enforced. Requiring the re-verified socket to EQUAL expectSock (the
// path enumeration matched under scanDir) closes that gap exactly.
//
// SIGTERM first; the daemon has no clients to drain so it exits promptly. A
// no-longer-existing process collapses to success (already reaped).
func killTmuxProcByPID(pid int, expectSock string) error {
	if pid <= 0 {
		return fmt.Errorf("killTmuxProcByPID: invalid pid %d", pid)
	}
	// Re-verify identity: the PID must still be a tmux server on the SAME
	// fleet-test socket. Distinguish "no such pid" (ps exits 1 with empty
	// stdout → genuinely gone → success, nothing to reap) from a PROBE
	// FAILURE (ps missing/denied/other exit → we CANNOT confirm the PID is
	// the expected daemon, so returning nil here would let the caller report
	// VerbKilled for a daemon that is still running — codex iter-7 [P2]).
	out, err := exec.Command("ps", "-o", "args=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 && len(strings.TrimSpace(string(out))) == 0 {
			return nil // no such pid — already gone, nothing to kill
		}
		// ps unavailable / denied / unexpected: cannot prove identity. Surface
		// the failure so the action does NOT falsely report killed.
		return fmt.Errorf("killTmuxProcByPID: identity re-verify for pid %d failed (cannot confirm it is the %s daemon; refusing to kill): %w", pid, expectSock, err)
	}
	args := strings.TrimSpace(string(out))
	sock, ok := tmuxServerSocketFromArgv(args)
	if !ok || sock != expectSock {
		return fmt.Errorf("killTmuxProcByPID: pid %d is no longer the expected fleet-test tmux daemon on %s (recycled); refusing to kill", pid, expectSock)
	}
	proc, ferr := os.FindProcess(pid)
	if ferr != nil {
		return nil // not found
	}
	if serr := proc.Signal(syscall.SIGTERM); serr != nil {
		// ESRCH (process gone) is success; anything else is a real error.
		if errors.Is(serr, os.ErrProcessDone) || errors.Is(serr, syscall.ESRCH) {
			return nil
		}
		return fmt.Errorf("kill %d: %w", pid, serr)
	}
	return nil
}

// killTmuxServerOnDisk runs `tmux -S <sock> kill-server` and best-effort
// removes the (now stale) socket file so the next gc run doesn't see a
// dangling .sock. Idempotent: a kill against an already-dead server
// exits non-zero, which we tolerate as "already gone".
func killTmuxServerOnDisk(sock string) error {
	// Defense-in-depth symlink guard (codex iter-2 [P2]): firstFleetSession
	// already rejected non-sockets at enumeration, but this kill seam is a
	// standalone DefaultDeps hook that both signals kill-server AND removes
	// the path. Re-verify it is a real Unix socket (no symlink deref) so a
	// path that turned into a symlink-to-default-server between enumeration
	// and apply can never get kill-server'd. A non-socket → no-op success.
	if fi, err := os.Lstat(sock); err != nil || fi.Mode()&os.ModeSocket == 0 {
		return nil
	}
	// Confirm a server is actually bound before signaling — mirrors
	// tmux.Kill's pre-probe so a dead-already socket isn't reported as a
	// kill failure.
	if err := exec.Command("tmux", "-S", sock, "ls").Run(); err != nil {
		// No server (or no sessions): nothing to kill. Clean up the file.
		_ = os.Remove(sock)
		return nil
	}
	if out, err := exec.Command("tmux", "-S", sock, "kill-server").CombinedOutput(); err != nil {
		// kill-server can race a server that just exited — re-probe and
		// treat a now-dead server as success.
		if perr := exec.Command("tmux", "-S", sock, "ls").Run(); perr != nil {
			_ = os.Remove(sock)
			return nil
		}
		return fmt.Errorf("tmux -S %s kill-server: %w (%s)", sock, err, strings.TrimSpace(string(out)))
	}
	_ = os.Remove(sock)
	return nil
}
