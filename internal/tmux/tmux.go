// Package tmux wraps the bits of `tmux(1)` Fleet uses: spawn detached
// session, attach to existing session, check session existence.
//
// Fleet's only runtime dep beyond the binary itself is tmux. Keeping
// every tmux invocation in one package makes that dep boundary obvious
// and easy to mock for tests.
//
// FLEET_TMUX_SOCKET env var, if set, is passed to every tmux invocation
// as `-S <path>`. Tests use this to isolate per-test tmux servers and
// avoid races with parallel test packages on the host's default socket.
// Production leaves it unset and uses the default tmux server.
package tmux

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ErrNoSession is returned when an operation references a session that
// doesn't exist (e.g., Attach on a dead agent).
var ErrNoSession = errors.New("tmux session not found")

// tmuxArgs prepends `-S <FLEET_TMUX_SOCKET>` if the env var is set.
// Centralizes the socket selection so every tmux subprocess in this
// package uses the same server.
func tmuxArgs(rest ...string) []string {
	if sock := os.Getenv("FLEET_TMUX_SOCKET"); sock != "" {
		return append([]string{"-S", sock}, rest...)
	}
	return rest
}

// MinTmuxMajor and MinTmuxMinor are the lowest tmux release Fleet
// supports. 3.2 (2021) added `tmux new-session -e KEY=VALUE`, which
// Spawn relies on to inject FLEET_AGENT_ID into the spawned command
// regardless of whether the tmux server was already running.
const (
	MinTmuxMajor = 3
	MinTmuxMinor = 2
)

// Function-table seam (ci-perf-pr2). Each exported shelling op below is a
// thin forwarder to a package-level var that defaults to the real `tmux`
// backend (realXxx). Tests swap the table for an in-process memory fake via
// tmuxtest.InstallFake so routed unit tests never spawn a real subprocess —
// the lever that takes CI under 3 minutes. SessionName is pure and exempt.
//
//	caller ──▶ tmux.Spawn(...) ──▶ spawnFn(...) ──┬─▶ realSpawn (prod)
//	                                              └─▶ fake.Spawn (test, swapped)
//
// The seam is the SOLE supported swap mechanism. The runtime sink guard in
// realSpawn (below) remains the load-bearing safety boundary for any test
// that DOES reach the real backend (parity/smoke tests under RequireTmux).
var (
	availableFn               = realAvailable
	hasSessionFn              = realHasSession
	sessionAliveFn            = realSessionAlive
	spawnFn                   = realSpawn
	attachFn                  = realAttach
	sendKeysFn                = realSendKeys
	capturePaneFn             = realCapturePane
	setStatusHintFn           = realSetStatusHint
	killFn                    = realKill
	listSessionsFn            = realListSessions
	listSessionsWithCreatedFn = realListSessionsWithCreated
)

// Available returns nil if `tmux` is on PATH AND its version is at
// least MinTmuxMajor.MinTmuxMinor. Surfaces a clear error at startup
// before any spawn attempt; on older tmux, dispatch and handoff would
// fail at `new-session -e` with a confusing "unknown option" message.
func Available() error { return availableFn() }

func realAvailable() error {
	cmd := exec.Command("tmux", tmuxArgs("-V")...)
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("tmux not available (install with `brew install tmux`): %w", err)
	}
	major, minor, parseErr := parseTmuxVersion(string(out))
	if parseErr != nil {
		// Couldn't parse — let the spawn fail later if it must, but
		// don't block startup on a regex miss.
		return nil
	}
	if major < MinTmuxMajor || (major == MinTmuxMajor && minor < MinTmuxMinor) {
		return fmt.Errorf("tmux %d.%d found but Fleet requires %d.%d+ (for `new-session -e`); upgrade with `brew upgrade tmux` or your distro's package manager",
			major, minor, MinTmuxMajor, MinTmuxMinor)
	}
	return nil
}

// parseTmuxVersion extracts MAJOR.MINOR from `tmux -V` output, which
// looks like "tmux 3.5a\n" (suffix letter for patch releases) or
// "tmux next-3.5\n" (pre-release builds). Returns ("", "", err) if
// no match found.
func parseTmuxVersion(s string) (major, minor int, err error) {
	// Find the first "<digit>.<digit>" pattern.
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			continue
		}
		j := i
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
		}
		if j == len(s) || s[j] != '.' {
			continue
		}
		k := j + 1
		for k < len(s) && s[k] >= '0' && s[k] <= '9' {
			k++
		}
		if k == j+1 {
			continue
		}
		major, err = parseInt(s[i:j])
		if err != nil {
			continue
		}
		minor, err = parseInt(s[j+1 : k])
		if err != nil {
			continue
		}
		return major, minor, nil
	}
	return 0, 0, fmt.Errorf("no MAJOR.MINOR pattern in %q", s)
}

func parseInt(s string) (int, error) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("non-digit in %q", s)
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

// HasSession returns true if a tmux session named `session` exists.
// It conflates "session does not exist" with "probe failed" (binary
// missing, socket broken). Cleanup or status paths that must NOT
// confuse those two cases should use SessionAlive instead.
func HasSession(session string) bool { return hasSessionFn(session) }

func realHasSession(session string) bool {
	cmd := exec.Command("tmux", tmuxArgs("has-session", "-t", session)...)
	return cmd.Run() == nil
}

// SessionAlive distinguishes the three outcomes of probing a tmux
// session, which HasSession collapses into a single bool:
//
//   - alive=true,  err=nil  — session exists
//   - alive=false, err=nil  — tmux confirmed the session does not
//     exist (or no server is running, which means no sessions
//     anywhere) — definitive dead
//   - alive=false, err!=nil — probe failed (tmux missing, socket
//     unreadable, lost-server transport error) — caller must NOT
//     treat this as "dead"
//
// Used by destructive paths (`fleet rm`) and the TUI status cache
// where mistaking a transport failure for a dead session would
// either archive a live agent or paint a healthy row "dead".
//
// tmux exit-code reality: `has-session` returns 1 for BOTH "no such
// session" AND many connection failures (bad socket path, server
// disappeared). Distinguishing them requires inspecting stderr —
// the only reliable signal that the session is genuinely gone is
// tmux's "can't find session" / "no server running" message.
// Anything else with exit 1 is reported as a probe error so the
// caller refuses to act on ambiguous state.
func SessionAlive(session string) (bool, error) { return sessionAliveFn(session) }

func realSessionAlive(session string) (bool, error) {
	cmd := exec.Command("tmux", tmuxArgs("has-session", "-t", session)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	if runErr == nil {
		return true, nil
	}
	if exitErr, ok := runErr.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
		msg := strings.ToLower(stderr.String())
		// Patterns where tmux exit 1 is authoritative for "session is
		// not running anywhere" — safe to treat as definitive dead.
		// All variations land here because if the server isn't up, no
		// session can be alive, and the operator's intent (rm, status
		// cache) is the same as if tmux explicitly said no-such-session.
		//
		// Codex iter-7 P1: FLEET_TMUX_SOCKET test isolation tears the
		// server down after the last session is killed, leaving the
		// socket file removed. Subsequent has-session calls land in
		// the "no such file or directory" / "connection refused"
		// branches — those used to be misclassified as probe errors,
		// which made the new dead-session cleanup path refuse to
		// archive orphaned agents.
		switch {
		case strings.Contains(msg, "can't find session"),
			strings.Contains(msg, "session not found"),
			strings.Contains(msg, "no server running"),
			strings.Contains(msg, "no such file or directory"),
			strings.Contains(msg, "connection refused"),
			strings.Contains(msg, "lost server"):
			return false, nil
		}
		return false, fmt.Errorf("probe session %s: tmux exit 1 with stderr %q (treating as ambiguous, not dead)",
			session, strings.TrimSpace(stderr.String()))
	}
	return false, fmt.Errorf("probe session %s: %w", session, runErr)
}

// Spawn starts a detached tmux session running `command`. Returns
// without waiting for the command to exit.
//
// Equivalent shell:
//
//	tmux new-session -d -s <session> -c <cwd> -e K1=V1 -e K2=V2 <command...>
//
// `cwd` may be empty to inherit the caller's working directory.
//
// `extraEnv` is forwarded as repeated `-e KEY=VALUE` flags to
// `tmux new-session`. This is the documented way to inject per-session
// env into tmux. Setting cmd.Env on the client subprocess does NOT
// propagate when tmux talks to an already-running server — the server
// inherited its own env at startup and won't pick up new vars from a
// later client connection. Using `-e` works in both cases (fresh
// server or existing).
func Spawn(session, cwd string, command, extraEnv []string) error {
	return spawnFn(session, cwd, command, extraEnv)
}

func realSpawn(session, cwd string, command, extraEnv []string) error {
	if len(command) == 0 {
		return errors.New("tmux.Spawn: empty command")
	}
	// Sink guard (postmortem 2026-05-14, follow-up to PR #150): inside
	// `go test`, refuse to create a session unless FLEET_TMUX_SOCKET
	// points at a per-test socket path. Tests MUST call the shared
	// `tmuxtest.RequireTmux(t)` (or `IsolateSocket(t)`) helper so the
	// per-test tmux server is torn down in t.Cleanup. Without this
	// guard a transitive caller (runDispatch → spawn.Spawn → tmux.Spawn)
	// can leak fleet-* sessions onto the host server.
	//
	// testing.Testing() (Go 1.21+) is true iff the binary was built by
	// `go test`, so production paths are untouched.
	//
	// Pattern check rationale:
	//   - Empty FLEET_TMUX_SOCKET → default operator server (REJECT)
	//   - FLEET_TMUX_SOCKET that doesn't follow the canonical
	//     `/tmp/fleet-test-*.sock` shape → likely inherited from a
	//     wrapping shell (e.g., operator exported a shared server's
	//     socket) → REJECT
	//   - `/tmp/fleet-test-*.sock` → produced by tmuxtest.IsolateSocket
	//     (the canonical helper) OR by a test that deliberately uses
	//     the prefix for negative-path coverage (e.g.,
	//     TestSpawn_FailsWhenSocketUnusable allocates
	//     `/tmp/fleet-test-aaaa…aaaa.sock` to exercise the
	//     unwritable-socket failure mode). Accept.
	//
	// Codex review history on this guard:
	//   - iter-12 [P2]: introduced inherited-socket rejection
	//   - iter-15 [P2]: relaxed prefix to any /tmp/*.sock — too loose
	//   - iter-17 [P2]: re-tightened to /tmp/fleet-test-*.sock (this
	//     version). TestSpawn_FailsWhenSocketUnusable was updated to
	//     use the canonical prefix.
	//
	// The lint at scripts/lint-test-isolation.sh is a static backup;
	// this runtime guard is the load-bearing safety boundary.
	if testing.Testing() {
		sock := os.Getenv("FLEET_TMUX_SOCKET")
		if sock == "" {
			return fmt.Errorf("tmux.Spawn: refusing to use default tmux socket under go test (session %q); call tmuxtest.RequireTmux(t) to isolate FLEET_TMUX_SOCKET", session)
		}
		if !strings.HasPrefix(sock, "/tmp/fleet-test-") || !strings.HasSuffix(sock, ".sock") {
			return fmt.Errorf("tmux.Spawn: refusing inherited FLEET_TMUX_SOCKET=%q under go test (session %q); only /tmp/fleet-test-*.sock paths (from tmuxtest.RequireTmux / IsolateSocket) are accepted", sock, session)
		}
	}
	args := []string{"new-session", "-d", "-s", session}
	if cwd != "" {
		args = append(args, "-c", cwd)
	}
	for _, kv := range extraEnv {
		args = append(args, "-e", kv)
	}
	args = append(args, command...)
	cmd := exec.Command("tmux", tmuxArgs(args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tmux new-session %s: %w (%s)", session, err, stderr.String())
	}
	// `tmux new-session` can exit 0 even when it failed to create the
	// session (unwritable socket path, sandbox restriction, oversized
	// UNIX-socket path). When that happens it prints to stderr but
	// doesn't fail the exit code. Verify with HasSession only when
	// stderr signals trouble — short-lived commands like `sh -c true`
	// also leave HasSession=false, but cleanly (no stderr), and we
	// must not flag those as failures.
	// realHasSession (NOT the exported HasSession): a real op must probe
	// real tmux even when a fake backend is installed, so RealBackend()
	// genuinely bypasses the fake (codex iter-2 [P2]). The exported
	// wrapper forwards to hasSessionFn, which the fake swaps.
	if stderr.Len() > 0 && !realHasSession(session) {
		return fmt.Errorf("tmux new-session %s: exit 0 but session not created (%s)", session, stderr.String())
	}
	return nil
}

// Attach replaces the current process with `tmux attach -t <session>`.
// Only returns on error; on success the user is now inside tmux.
//
// Uses syscall-style exec so the user's terminal is connected directly
// to tmux without an intermediate `fleet` process holding the session.
func Attach(session string) error { return attachFn(session) }

func realAttach(session string) error {
	if !realHasSession(session) { // real probe — bypass any installed fake
		return fmt.Errorf("%w: %s", ErrNoSession, session)
	}
	bin, err := exec.LookPath("tmux")
	if err != nil {
		return fmt.Errorf("locate tmux: %w", err)
	}
	// Replace current process. Returns only on error.
	argv := append([]string{"tmux"}, tmuxArgs("attach", "-t", session)...)
	return execve(bin, argv, os.Environ())
}

// SendKeys sends one or more key sequences to a tmux session.
//
// `keys` is forwarded verbatim as positional args to tmux:
//
//	tmux send-keys -t <session> <keys...>
//
// Pass "Enter" as a separate arg to submit a command. Example:
//
//	tmux.SendKeys("fleet-a1b2", "/exit", "Enter")
//
// Returns ErrNoSession if the session has already exited — callers
// in cleanup paths typically ignore this and proceed to Kill.
func SendKeys(session string, keys ...string) error {
	return sendKeysFn(session, keys...)
}

func realSendKeys(session string, keys ...string) error {
	if !realHasSession(session) { // real probe — bypass any installed fake
		return fmt.Errorf("%w: %s", ErrNoSession, session)
	}
	args := append([]string{"send-keys", "-t", session}, keys...)
	cmd := exec.Command("tmux", tmuxArgs(args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tmux send-keys %s: %w (%s)", session, err, string(out))
	}
	return nil
}

// CapturePane returns the visible content of the given session's
// active pane plus the alternate screen if one is active. Used by
// spawn.SendInitialPrompt's readiness poll: when both buffers stop
// changing, the agent is idle and ready to receive input.
//
// We concatenate "current" (`capture-pane -p`) AND "alternate"
// (`capture-pane -p -a`) so the poll sees activity regardless of
// whether the wrapped command runs in the main screen (shells) or
// the alternate screen (claude code / vim / TUI apps). On main-only
// panes the alt-capture errors with "no alternate screen" — we
// treat that as empty and return just the current capture.
//
// Pre-flight uses SessionAlive (not HasSession) so a transport
// failure (bad socket, lost server) bubbles up as a generic error
// rather than masquerading as ErrNoSession. Callers downstream
// roll back the new agent on ErrNoSession but only log on other
// errors, so misclassifying transport failures would strand live
// replacements (codex review iter-15 P1).
func CapturePane(session string) ([]byte, error) { return capturePaneFn(session) }

func realCapturePane(session string) ([]byte, error) {
	alive, probeErr := realSessionAlive(session) // real probe — bypass fake
	switch {
	case probeErr != nil:
		return nil, fmt.Errorf("tmux capture-pane %s: probe failed: %w",
			session, probeErr)
	case !alive:
		return nil, fmt.Errorf("%w: %s", ErrNoSession, session)
	}
	var combined []byte
	visibleOut, visibleErr := exec.Command("tmux",
		tmuxArgs("capture-pane", "-t", session, "-p")...).Output()
	if visibleErr == nil {
		combined = append(combined, visibleOut...)
	}
	altOut, altErr := exec.Command("tmux",
		tmuxArgs("capture-pane", "-t", session, "-p", "-a")...).Output()
	if altErr == nil {
		combined = append(combined, altOut...)
	}
	if visibleErr != nil && altErr != nil {
		return nil, fmt.Errorf("tmux capture-pane %s: visible=%w; alt=%v",
			session, visibleErr, altErr)
	}
	return combined, nil
}

// SetStatusHint prepends a fleet hint to the given session's
// status-right so operators see "Ctrl-b d to detach" persistently
// while attached, without losing whatever custom right-status they
// configured globally. The change is session-scoped — other sessions
// keep the user's normal config.
//
// Best-effort: any failure (old tmux, sandbox, format-string quirks)
// returns the error so the caller can ignore it. The TUI's pre-attach
// footer + the in-session shell banner are fallback hint paths if
// this no-ops.
//
// status-right-length is bumped so a long pre-existing format string
// plus the prepended hint fits without truncation.
func SetStatusHint(session, hint string) error { return setStatusHintFn(session, hint) }

func realSetStatusHint(session, hint string) error {
	// Read the user's current status-right (global). The format
	// string is preserved verbatim, so #[fg=...] and #() interpolations
	// stay intact.
	out, _ := exec.Command("tmux", tmuxArgs("show-options", "-gqv", "status-right")...).Output()
	existing := string(bytes.TrimSpace(out))

	var combined string
	if existing == "" {
		combined = hint
	} else {
		combined = hint + " " + existing
	}

	// Order matters: bump length before assigning the longer value.
	if err := exec.Command("tmux", tmuxArgs("set-option", "-t", session,
		"status-right-length", "200")...).Run(); err != nil {
		return fmt.Errorf("set status-right-length: %w", err)
	}
	if err := exec.Command("tmux", tmuxArgs("set-option", "-t", session,
		"status-right", combined)...).Run(); err != nil {
		return fmt.Errorf("set status-right: %w", err)
	}
	return nil
}

// Kill terminates a tmux session. Returns nil if the session is
// already gone (idempotent for cleanup paths). Probe errors are
// surfaced to the caller — without that, a transport-level tmux
// failure between the pre-probe and the kill would silently skip
// the actual kill-session command and the caller would archive a
// live session as if it had been terminated (codex review iter-7
// P2). Uses the tristate SessionAlive instead of HasSession so the
// "probe failed" case no longer collapses into "already gone".
func Kill(session string) error { return killFn(session) }

func realKill(session string) error {
	alive, err := realSessionAlive(session) // real probe — bypass any installed fake
	if err != nil {
		return fmt.Errorf("tmux kill-session %s: pre-probe failed: %w", session, err)
	}
	if !alive {
		return nil
	}
	cmd := exec.Command("tmux", tmuxArgs("kill-session", "-t", session)...)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tmux kill-session %s: %w", session, err)
	}
	return nil
}

// SessionName returns the canonical tmux session name for an agent ID.
// Centralized so the spawn / attach / kill paths agree on the format.
func SessionName(agentID string) string {
	return "fleet-" + agentID
}

// ListSessions returns the names of every tmux session on the active
// server (FLEET_TMUX_SOCKET when set, else the default server).
//
// Equivalent shell:
//
//	tmux ls -F '#{session_name}'
//
// Returns an empty slice with no error when the server has no live
// sessions (tmux prints "no server running" / "no current client"
// to stderr; both collapse here into "no sessions"). Genuine probe
// failures (tmux binary missing, broken socket permissions) surface
// as errors so the operator-facing `fleet maintenance prune-orphan-tmux`
// can distinguish "no orphans" from "couldn't even check".
//
// Trailing newline stripped; per-line names trimmed. Empty lines
// (defensive — tmux shouldn't emit them) skipped.
func ListSessions() ([]string, error) { return listSessionsFn() }

func realListSessions() ([]string, error) {
	cmd := exec.Command("tmux", tmuxArgs("ls", "-F", "#{session_name}")...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			msg := strings.ToLower(stderr.String())
			switch {
			case strings.Contains(msg, "no server running"),
				strings.Contains(msg, "no sessions"),
				strings.Contains(msg, "no current client"),
				strings.Contains(msg, "no such file or directory"),
				strings.Contains(msg, "connection refused"),
				strings.Contains(msg, "lost server"):
				return nil, nil
			}
		}
		return nil, fmt.Errorf("tmux ls: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	var names []string
	for _, line := range strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		names = append(names, line)
	}
	return names, nil
}

// SessionInfo pairs a tmux session name with the wall-clock instant
// tmux created it. Used by the prune-orphan-tmux sweeper to skip
// freshly-spawned sessions whose agent record is still being written.
type SessionInfo struct {
	Name    string
	Created time.Time
}

// ListSessionsWithCreated returns every tmux session on the active
// server paired with its creation time. Equivalent shell:
//
//	tmux ls -F '#{session_name} #{session_created}'
//
// `session_created` is a Unix epoch (seconds). Lines that fail to parse
// (defensive — tmux shouldn't emit malformed output) are skipped with a
// zero Created so the caller falls back to "treat as fresh-unknown".
//
// Returns an empty slice with no error when the server has no live
// sessions. Genuine probe failures surface as errors.
//
// Used by `fleet maintenance prune-orphan-tmux` to gate destructive
// cleanup on session age — a freshly-spawned tmux session can exist
// before its agent record finishes writing (see spawn.Spawn:
// tmux.Spawn → resolveEnginePid → rec.Write), and the sweeper must
// not kill that legitimate window.
func ListSessionsWithCreated() ([]SessionInfo, error) { return listSessionsWithCreatedFn() }

func realListSessionsWithCreated() ([]SessionInfo, error) {
	cmd := exec.Command("tmux", tmuxArgs("ls", "-F", "#{session_name} #{session_created}")...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			msg := strings.ToLower(stderr.String())
			switch {
			case strings.Contains(msg, "no server running"),
				strings.Contains(msg, "no sessions"),
				strings.Contains(msg, "no current client"),
				strings.Contains(msg, "no such file or directory"),
				strings.Contains(msg, "connection refused"),
				strings.Contains(msg, "lost server"):
				return nil, nil
			}
		}
		return nil, fmt.Errorf("tmux ls (with created): %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	var infos []SessionInfo
	for _, line := range strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Format: "<name> <unix-seconds>". Split on the LAST space so
		// session names with spaces (rare but legal in tmux) survive.
		idx := strings.LastIndex(line, " ")
		if idx <= 0 || idx == len(line)-1 {
			// Malformed — record name with zero Created so caller treats
			// it as freshness-unknown (safer than dropping silently).
			infos = append(infos, SessionInfo{Name: line})
			continue
		}
		name := line[:idx]
		secs, err := strconv.ParseInt(line[idx+1:], 10, 64)
		if err != nil {
			infos = append(infos, SessionInfo{Name: name})
			continue
		}
		infos = append(infos, SessionInfo{Name: name, Created: time.Unix(secs, 0)})
	}
	return infos, nil
}
