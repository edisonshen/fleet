package main

import (
	"strings"
	"testing"
)

// TestDispatch_DefaultCommandWrapsAndSkipsPermissions locks in two
// invariants: (1) claude runs with --dangerously-skip-permissions so
// permission prompts don't block one of N parallel agents, and (2)
// the command is wrapped in a shell so the tmux session survives
// claude exiting (Ctrl-D / /exit). Without (2), an operator who
// detaches via the wrong key destroys the session and `fleet attach`
// fails with "no sessions" on the next try.
func TestDispatch_DefaultCommandWrapsAndSkipsPermissions(t *testing.T) {
	cmd := newDispatchCmd()
	flag := cmd.Flag("command")
	if flag == nil {
		t.Fatal("dispatch must expose --command")
	}
	got := flag.DefValue
	if !strings.Contains(got, "sh") || !strings.Contains(got, "-c") {
		t.Errorf("default --command should wrap in a shell, got %q", got)
	}
	if !strings.Contains(got, "--dangerously-skip-permissions") {
		t.Errorf("default --command should pass --dangerously-skip-permissions, got %q", got)
	}
	if !strings.Contains(got, "exec") || !strings.Contains(got, "SHELL") {
		t.Errorf("default --command should drop into an interactive shell on claude exit, got %q", got)
	}
}
