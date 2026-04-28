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

// Available returns nil if `tmux` is on PATH and exits cleanly when
// asked for its version. Run this at startup to surface a clear error
// before any spawn attempt.
func Available() error {
	cmd := exec.Command("tmux", tmuxArgs("-V")...)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tmux not available (install with `brew install tmux`): %w", err)
	}
	return nil
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
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("tmux new-session %s: %w (%s)", session, err, string(out))
	}
	// `tmux new-session` can exit 0 even when it failed to create the
	// session (unusable socket path, sandbox restriction, oversized
	// UNIX-socket path). Verify with has-session so spawn.Spawn
	// doesn't write an agent record for a session that never existed.
	if !HasSession(session) {
		return fmt.Errorf("tmux new-session %s: exit 0 but session not created (%s)", session, string(out))
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
