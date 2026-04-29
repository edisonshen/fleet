package main

import (
	"strings"
	"testing"

	"github.com/spf13/pflag"
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
	// pflag.SliceValue exposes the unescaped slice; flag.DefValue is
	// the cobra-escaped help-text rendering and would mangle the
	// inner double quotes around shell vars.
	slice, ok := flag.Value.(pflag.SliceValue)
	if !ok {
		t.Fatalf("--command flag is not a SliceValue: %T", flag.Value)
	}
	parts := slice.GetSlice()
	if len(parts) < 3 || parts[0] != "sh" || parts[1] != "-c" {
		t.Fatalf("default --command should be [sh -c <script>], got %v", parts)
	}
	script := parts[2]
	if !strings.Contains(script, "--dangerously-skip-permissions") {
		t.Errorf("default script should pass --dangerously-skip-permissions, got %q", script)
	}
	if !strings.Contains(script, "exec ${SHELL:-bash}") {
		t.Errorf("default script should drop into an interactive shell on clean claude exit, got %q", script)
	}
	// codex iter-3 P1 regression: non-zero claude exits must propagate
	// out of the wrapper so the tmux session terminates and fleet's
	// failure-detection sees no live session — preventing zombie
	// agent records pointing at idle shells.
	if !strings.Contains(script, "RC=$?") || !strings.Contains(script, `exit "$RC"`) {
		t.Errorf("default script should propagate non-zero claude exits, got %q", script)
	}
}
