//go:build unix

package tmux

import "syscall"

// execve replaces the current process with `bin argv...` in the given env.
// Only returns on error.
func execve(bin string, argv, env []string) error {
	return syscall.Exec(bin, argv, env)
}
