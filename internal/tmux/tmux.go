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

// Available returns nil if `tmux` is on PATH AND its version is at
// least MinTmuxMajor.MinTmuxMinor. Surfaces a clear error at startup
// before any spawn attempt; on older tmux, dispatch and handoff would
// fail at `new-session -e` with a confusing "unknown option" message.
func Available() error {
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
func HasSession(session string) bool {
	cmd := exec.Command("tmux", tmuxArgs("has-session", "-t", session)...)
	return cmd.Run() == nil
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
	if len(command) == 0 {
		return errors.New("tmux.Spawn: empty command")
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
	if stderr.Len() > 0 && !HasSession(session) {
		return fmt.Errorf("tmux new-session %s: exit 0 but session not created (%s)", session, stderr.String())
	}
	return nil
}

// Attach replaces the current process with `tmux attach -t <session>`.
// Only returns on error; on success the user is now inside tmux.
//
// Uses syscall-style exec so the user's terminal is connected directly
// to tmux without an intermediate `fleet` process holding the session.
func Attach(session string) error {
	if !HasSession(session) {
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
	if !HasSession(session) {
		return fmt.Errorf("%w: %s", ErrNoSession, session)
	}
	args := append([]string{"send-keys", "-t", session}, keys...)
	cmd := exec.Command("tmux", tmuxArgs(args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tmux send-keys %s: %w (%s)", session, err, string(out))
	}
	return nil
}

// Kill terminates a tmux session. Returns nil if the session is
// already gone (idempotent for cleanup paths).
func Kill(session string) error {
	if !HasSession(session) {
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
