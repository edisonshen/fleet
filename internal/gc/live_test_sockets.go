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
	"strings"
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
		act := Action{
			Kind:   KindOrphanTmux,
			Target: target,
			Verb:   VerbSurface,
			Reason: fmt.Sprintf("live test-socket tmux server on %s with no live go-test owner; rerun with --apply --aggressive to kill (or `tmux -S %s kill-server`)",
				s.SocketPath, s.SocketPath),
		}
		if opts.Aggressive {
			act.Verb = VerbWouldKill
			act.Reason = fmt.Sprintf("live test-socket tmux server on %s with no live go-test owner", s.SocketPath)
			if opts.Apply {
				if deps.KillTmuxServer == nil {
					act.Reason = "kill seam unwired (set Deps.KillTmuxServer to apply)"
				} else if kerr := deps.KillTmuxServer(s.SocketPath); kerr != nil {
					// Verb stays surface; a kill failure must NOT report killed.
					act.Reason = fmt.Sprintf("kill failed: %v", kerr)
				} else {
					act.Verb = VerbKilled
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

// anyGoTestRunning reports whether at least one `go test` / test-binary
// process is alive on the host. It probes pgrep for the canonical test
// process shapes.
//
// pgrep -f treats the needle as a regex. macOS pgrep is BSD and uses
// BRE (basic regex): alternation `\|` and groups `(...)` are NOT
// portable, but `\.` (escaped dot) and `[ /]` (character class) are. The
// needles below are therefore written in the BRE/ERE common subset
// (codex iter-2 [P2] flagged the unescaped dot; the first fix used an
// ERE-only `(...|$)` group that silently matched nothing under BSD
// pgrep — codex iter-3 caught the test-flakiness, this is the BRE-safe
// form):
//
//   - `go test`   — the driver while compiling + running a package's
//     tests. The space makes it specific; no metachars to escape.
//   - `\.test[ /]` — a compiled `pkg.test` binary. go builds these to a
//     temp dir and execs them as `/tmp/go-build.../pkg.test -test.*`, so
//     the argv always carries a path component ending in `.test` followed
//     by a space (the -test.* flags) or a slash. The escaped dot + the
//     trailing boundary class prevent the old bare `.test` regex from
//     matching unrelated argv like `pytest`, `latest`, or `contest`
//     (which would pin anyGoTestRunning()==true forever and silently
//     defeat the reaper on any host running pytest).
//   - `compile`/`link`/`vet` toolchain processes are NOT matched: they run
//     for plain `go build` too, so matching them would spare sockets
//     during unrelated builds. The two shapes above bracket the entire
//     window in which a fleet-test socket can legitimately be owned.
//
// Fail-safe defaults:
//   - pgrep exit 1 ("no match") for a needle → that needle found nothing.
//   - pgrep missing (*exec.Error) → return true (unknown → spare). A host
//     without pgrep cannot prove quiescence, so we never reap there.
//   - any other pgrep error → return true (spare).
//
// goTestPgrepNeedles are the BRE/ERE-common-subset patterns fed to
// `pgrep -f`. Exposed (package-level) so a unit test can validate the
// patterns' INTENT against representative argv strings without depending
// on pgrep's cross-process visibility (which some sandboxes restrict).
var goTestPgrepNeedles = []string{"go test", `\.test[ /]`}

func anyGoTestRunning() bool {
	// pgrep -f matches the regex against the full argv. "-x" is NOT used
	// because the argv carries package paths / flags after the needle.
	for _, needle := range goTestPgrepNeedles {
		out, err := exec.Command("pgrep", "-f", needle).Output()
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
				continue // this needle matched nothing; try the next
			}
			// pgrep missing / unexpected failure: cannot prove quiescence.
			var execErr *exec.Error
			if errors.As(err, &execErr) {
				return true
			}
			return true
		}
		if strings.TrimSpace(string(out)) != "" {
			return true
		}
	}
	return false
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
