package tmuxtest

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestRequireTmux_SetsAndCleansSocket verifies the two invariants the
// canonical helper enforces: FLEET_TMUX_SOCKET is non-empty + uniquely
// prefixed inside a subtest, and the t.Cleanup removes the socket file
// after the subtest finishes. Postmortem 2026-05-14 (orphan tmux leak)
// + follow-up 2026-05-15: without these guarantees we silently leak
// sessions onto the operator's default tmux server.
func TestRequireTmux_SetsAndCleansSocket(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed; helper would Skip too")
	}
	parentSock := os.Getenv("FLEET_TMUX_SOCKET")

	var inSock string
	t.Run("inner", func(t *testing.T) {
		inSock = RequireTmux(t)
		got := os.Getenv("FLEET_TMUX_SOCKET")
		if got == "" {
			t.Fatal("RequireTmux did not set FLEET_TMUX_SOCKET")
		}
		if got != inSock {
			t.Fatalf("returned %q but env is %q", inSock, got)
		}
		if got == parentSock {
			t.Fatalf("RequireTmux did not override FLEET_TMUX_SOCKET; still %q", got)
		}
		if !strings.HasPrefix(got, "/tmp/fleet-test-") {
			t.Errorf("FLEET_TMUX_SOCKET = %q; want /tmp/fleet-test-* prefix", got)
		}
		// Forge the socket file so we can verify cleanup removes it.
		if err := os.WriteFile(inSock, []byte("probe"), 0o600); err != nil {
			t.Fatalf("write probe socket file: %v", err)
		}
	})
	if _, err := os.Stat(inSock); !os.IsNotExist(err) {
		t.Errorf("RequireTmux cleanup did not remove %s (stat err=%v)", inSock, err)
	}
}

// TestRequireTmux_UniquePerCall pins that two calls produce two distinct
// socket paths — a precondition for tests that use t.Run subtests where
// each subtest expects its own isolated server.
func TestRequireTmux_UniquePerCall(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	var a, b string
	t.Run("first", func(t *testing.T) { a = RequireTmux(t) })
	t.Run("second", func(t *testing.T) { b = RequireTmux(t) })
	if a == b {
		t.Errorf("two RequireTmux calls returned identical sock paths %q", a)
	}
}
