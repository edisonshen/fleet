package gc

// live_test_sockets.go — orphan LIVE test-socket tmux reaper, a
// sub-classifier of the `orphan-tmux` kind (DESIGN-lifecycle-leak-
// recurrence.md PR-D).
//
// PROBLEM. The `sockets` kind reaps bare /tmp/fleet-test-*.sock FILES,
// but DELIBERATELY spares any socket whose tmux server is still alive
// (reconcileSockets's SocketLive gate) so a long-running test fixture
// isn't stranded mid-run. That left a gap: a `fleet-<id>` tmux server
// bound to a /tmp/fleet-test-*.sock whose owning `go test` process is
// long dead is NEVER reaped — the operator had to `tmux -S <sock>
// kill-server` BY HAND during the 2026-05-29 OOM. That violates
// feedback_fleet_owns_its_resources.md ("operator never manually
// kills"). This classifier closes the gap.
//
// FLOW:
//
//	/tmp/fleet-test-*.sock  ──→  tmux -S <sock> ls (fleet-<id> session?)
//	                                     │
//	                          lsof <sock>: any live `go test` (*.test)
//	                          process still holding it open?
//	                                     │
//	              ┌──────────────────────┴───────────────────────┐
//	         OwnerPID > 0                                    OwnerPID == 0
//	      (live test owns it)                              (no live owner)
//	              │                                              │
//	         skip (healthy,                          surface (default) /
//	          in-flight test)                        would-kill / killed
//	                                                 under --aggressive
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
	"strconv"
	"strings"
)

// LiveTestSocket describes one live tmux server bound to a
// /tmp/fleet-test-*.sock. OwnerPID is the PID of a live `go test`
// (*.test) process still holding the socket open, or 0 when none does
// — the 0 case is the orphan this classifier reaps under --aggressive.
type LiveTestSocket struct {
	// SocketPath is the /tmp/fleet-test-*.sock the server is bound to.
	SocketPath string
	// SessionName is the first fleet-<id> session on the server (used as
	// the Action.Target so the output reads like the rest of orphan-tmux).
	SessionName string
	// OwnerPID is a live go-test process holding the socket open, or 0.
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
			// OR the owner probe could not prove there's no owner
			// (ownerProbeFailedPID sentinel). Either way: never touch it,
			// even under --aggressive --apply (T4). Only a definitive
			// OwnerPID==0 ("no holder is a go-test process") is reaped.
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

// listLiveTestSocketsOnDisk scans /tmp for fleet-test-*.sock files,
// probes each for a live fleet-<id> tmux server, and resolves whether a
// live `go test` (*.test) process still holds the socket open.
//
// Best-effort: a missing tmux binary, an lsof failure, or a socket with
// no server simply drops that entry — the classifier degrades to "no
// orphans found", never to a spurious kill.
func listLiveTestSocketsOnDisk() ([]LiveTestSocket, error) {
	infos, err := scanSocketsDir("/tmp")
	if err != nil {
		return nil, err
	}
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
			OwnerPID:    socketGoTestOwner(sock),
		})
	}
	return out, nil
}

// firstFleetSession returns the first fleet-<id> session name on the
// server bound to sock, and ok=false when no server / no fleet session
// responds. Uses `tmux -S <sock> ls` directly (NOT tmux.ListSessions,
// which targets FLEET_TMUX_SOCKET / the default server).
func firstFleetSession(sock string) (string, bool) {
	if _, err := os.Stat(sock); err != nil {
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

// socketGoTestOwner returns the PID of a live `go test` (*.test) process
// that holds sock open, or 0 when none does. The tmux server process
// itself always holds the socket; the OWNER we look for is the test
// harness that spawned it (a `*.test` binary, `go`, or the compile/link
// driver). When only the tmux server holds the socket open, the test
// that created it is gone → orphan.
//
// `lsof -t <sock>` lists every PID with the file open (one per line).
// For each, `ps -o comm=` gives the command basename used to recognize
// a go-test process. Fail-safe: any probe failure returns 0 (orphan)
// ONLY for the owner question — but a probe failure on the WHOLE lsof
// call returns 0 too, which would mis-reap a live-owned server. To stay
// conservative we treat an lsof ERROR (not "no holders") as "owner
// unknown" and report a sentinel positive PID so the classifier spares
// it (see ownerProbeFailedPID).
func socketGoTestOwner(sock string) int {
	out, err := exec.Command("lsof", "-t", sock).Output()
	if err != nil {
		// lsof exits 1 when no process holds the file — that's a genuine
		// "no holders" answer (the .sock can linger after kill-server).
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return 0
		}
		// lsof missing / permission denied / other: owner unknown. Fail
		// closed — spare the server rather than risk killing a live test.
		var execErr *exec.Error
		if errors.As(err, &execErr) {
			return ownerProbeFailedPID
		}
		return ownerProbeFailedPID
	}
	for _, line := range strings.Fields(string(out)) {
		pid, perr := strconv.Atoi(line)
		if perr != nil || pid <= 0 {
			continue
		}
		comm, cerr := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=").Output()
		if cerr != nil {
			continue
		}
		if isGoTestComm(strings.TrimSpace(string(comm))) {
			return pid
		}
	}
	return 0
}

// ownerProbeFailedPID is a sentinel non-zero OwnerPID meaning "could not
// determine the owner" — the classifier treats any OwnerPID > 0 as
// "spare it", so an ambiguous probe never drives a kill (fail-safe).
const ownerProbeFailedPID = -1

// isGoTestComm recognizes a process command basename as a Go test
// harness: the compiled `*.test` binary, the `go` driver, or the
// compile/link toolchain processes that run during `go test`.
func isGoTestComm(comm string) bool {
	if comm == "" {
		return false
	}
	base := comm
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	switch base {
	case "go", "compile", "link", "vet":
		return true
	}
	return strings.HasSuffix(base, ".test")
}

// killTmuxServerOnDisk runs `tmux -S <sock> kill-server` and best-effort
// removes the (now stale) socket file so the next gc run doesn't see a
// dangling .sock. Idempotent: a kill against an already-dead server
// exits non-zero, which we tolerate as "already gone".
func killTmuxServerOnDisk(sock string) error {
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
