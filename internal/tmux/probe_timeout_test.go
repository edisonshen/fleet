package tmux

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// installSlowTmux puts a fake `tmux` that sleeps well past probeTimeout
// at the front of PATH and shortens probeTimeout for the test.
func installSlowTmux(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nsleep 5\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	prev := probeTimeout
	probeTimeout = 100 * time.Millisecond
	t.Cleanup(func() { probeTimeout = prev })
}

// TestSessionAlive_TimeoutIsAmbiguousNotDead: a wedged tmux must return
// a probe error within the deadline, never a definitive "dead".
func TestSessionAlive_TimeoutIsAmbiguousNotDead(t *testing.T) {
	installSlowTmux(t)
	start := time.Now()
	alive, err := realSessionAlive("fleet-anything")
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("probe did not honour the deadline: took %s", elapsed)
	}
	if err == nil {
		t.Fatalf("timed-out probe must surface an error; got alive=%v err=nil", alive)
	}
	if alive {
		t.Fatal("timed-out probe must not report alive")
	}
}

// TestHasSession_TimeoutReturnsFalseQuickly: the bool probe collapses a
// timeout to false but must still return within the deadline.
func TestHasSession_TimeoutReturnsFalseQuickly(t *testing.T) {
	installSlowTmux(t)
	start := time.Now()
	if realHasSession("fleet-anything") {
		t.Fatal("timed-out has-session must be false")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("probe did not honour the deadline: took %s", elapsed)
	}
}

// TestListSessions_TimeoutIsError: a wedged listing is an error (callers
// fall back to per-session probes), not an empty session set.
func TestListSessions_TimeoutIsError(t *testing.T) {
	installSlowTmux(t)
	start := time.Now()
	names, err := realListSessions()
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("listing did not honour the deadline: took %s", elapsed)
	}
	if err == nil {
		t.Fatalf("timed-out listing must be an error; got %v", names)
	}
}
