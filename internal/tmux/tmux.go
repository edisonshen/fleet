// Package tmux wraps the bits of `tmux(1)` Fleet uses: spawn detached
// session, attach to existing session, check session existence.
//
// Fleet's only runtime dep beyond the binary itself is tmux. Keeping
// every tmux invocation in one package makes that dep boundary obvious
// and easy to mock for tests.
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

// Available returns nil if `tmux` is on PATH and exits cleanly when
// asked for its version. Run this at startup to surface a clear error
// before any spawn attempt.
func Available() error {
	cmd := exec.Command("tmux", "-V")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tmux not available (install with `brew install tmux`): %w", err)
	}
	return nil
}

// HasSession returns true if a tmux session named `session` exists.
func HasSession(session string) bool {
	cmd := exec.Command("tmux", "has-session", "-t", session)
	return cmd.Run() == nil
}

// Spawn starts a detached tmux session running `command`. Returns
// without waiting for the command to exit.
//
// Equivalent shell:
//
//	tmux new-session -d -s <session> -c <cwd> <command...>
//
// `cwd` may be empty to inherit the caller's working directory.
func Spawn(session, cwd string, command []string) error {
	if len(command) == 0 {
		return errors.New("tmux.Spawn: empty command")
	}
	args := []string{"new-session", "-d", "-s", session}
	if cwd != "" {
		args = append(args, "-c", cwd)
	}
	args = append(args, command...)
	cmd := exec.Command("tmux", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("tmux new-session %s: %w (%s)", session, err, string(out))
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
	return execve(bin, []string{"tmux", "attach", "-t", session}, os.Environ())
}

// Kill terminates a tmux session. Returns nil if the session is
// already gone (idempotent for cleanup paths).
func Kill(session string) error {
	if !HasSession(session) {
		return nil
	}
	cmd := exec.Command("tmux", "kill-session", "-t", session)
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
