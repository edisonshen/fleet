package main

import (
	"testing"
)

// TestDispatch_DefaultCommandSkipsPermissions locks in Fleet's
// fire-and-forget default: every dispatched claude runs with
// --dangerously-skip-permissions so a permission prompt can't block
// one of N parallel agents. Operators who want regular prompting
// override with `--command claude`.
func TestDispatch_DefaultCommandSkipsPermissions(t *testing.T) {
	cmd := newDispatchCmd()
	flag := cmd.Flag("command")
	if flag == nil {
		t.Fatal("dispatch must expose --command")
	}
	got := flag.DefValue
	want := "[claude,--dangerously-skip-permissions]"
	if got != want {
		t.Errorf("default --command=%q, want %q", got, want)
	}
}
