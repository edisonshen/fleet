// Package tmuxtest provides the canonical test-isolation helper for any
// Fleet test that reaches `tmux.Spawn` directly or transitively (through
// `spawn.Spawn`, `runDispatch`, `runHandoff`, `Resume`, etc.).
//
// Postmortem 2026-05-14 (orphan tmux leak) + follow-up 2026-05-15
// (AtomicCoordSwap subagent leaked 10+ fleet-* sessions onto the
// operator's default server because tests transitively reached
// spawn.Spawn through runDispatch/runHandoff bypassed the per-package
// requireTmux/isolateTmuxSocket helpers — those helpers were opt-in
// convention, not a structural guarantee).
//
// `RequireTmux(t)` is the single answer: every prior copy of
// `requireTmux`/`isolateTmuxSocket` across packages was a duplicate
// implementation of the same five steps. Centralizing them means:
//
//   - One place to fix when the cleanup contract evolves.
//   - One identifying marker (`tmuxtest.RequireTmux`) for the lint at
//     scripts/lint-test-isolation.sh to recognize.
//   - The runtime sink guard in internal/tmux/tmux.go's Spawn function
//     turns "forgot to call this" from a silent leak into a deterministic
//     test failure.
package tmuxtest

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"testing"
)

// RequireTmux installs per-test tmux server isolation:
//
//   - Skip if `tmux` is not on PATH (CI without tmux still passes).
//   - Allocate a short /tmp/fleet-test-<hex>.sock path so the macOS
//     ~104-byte UNIX-socket length limit isn't hit. (TMUX_TMPDIR with
//     t.TempDir()-based paths overflows.)
//   - t.Setenv("FLEET_TMUX_SOCKET", <sock>) so every tmuxArgs call in
//     this test routes to the per-test server.
//   - Register t.Cleanup that runs `tmux -S <sock> kill-server` AND
//     removes the socket file, regardless of pass/fail/panic. Without
//     the explicit os.Remove some tmux builds leave the .sock file
//     behind even after kill-server.
//
// Idempotent within a single test: a test that calls RequireTmux twice
// (e.g., once in a setup helper and once at the top of the test) gets
// the LAST socket path. Cleanups are registered in FIFO order so both
// fire on exit.
//
// Why this is the canonical helper (not per-package copies):
// the AtomicCoordSwap subagent run on 2026-05-14 leaked 10+ fleet-*
// sessions onto the operator's default tmux server because tests
// reached spawn.Spawn through runDispatch without calling any
// isolation helper. The runtime sink guard in tmux.Spawn refuses to
// run under `go test` when FLEET_TMUX_SOCKET is empty, so calling
// RequireTmux is now the only way to make spawn-bearing tests pass.
func RequireTmux(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed; skipping integration test")
	}
	sock := isolatedSocketPath(t)
	t.Setenv("FLEET_TMUX_SOCKET", sock)
	t.Cleanup(func() {
		// kill-server is idempotent: tmux exits non-zero when no server
		// is running on the socket, which is fine — we just want it gone.
		_ = exec.Command("tmux", "-S", sock, "kill-server").Run()
		// Some tmux builds leave the .sock file behind even after
		// kill-server; remove it explicitly so /tmp doesn't accumulate
		// thousands of stale sockets across a long-running operator's
		// test runs.
		if err := os.Remove(sock); err != nil && !os.IsNotExist(err) {
			t.Logf("tmuxtest: remove %s: %v", sock, err)
		}
	})
	return sock
}

// isolatedSocketPath returns a unique /tmp/fleet-test-<hex>.sock path.
// Short prefix + 4-hex suffix keeps the path well under the macOS UNIX
// socket length limit while avoiding collisions across test functions
// within the same package (tests within a package run serially by
// default, but t.Parallel() or t.Run subtests can still share a process).
func isolatedSocketPath(t *testing.T) string {
	t.Helper()
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("tmuxtest: rand.Read: %v", err)
	}
	return "/tmp/fleet-test-" + hex.EncodeToString(b[:]) + ".sock"
}
