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

// RequireTmux installs per-test tmux server isolation AND skips the
// test cleanly if `tmux` is not on PATH. Use for tests that genuinely
// exercise tmux (spawn/attach/send-keys/etc.) — CI without tmux still
// passes because the test is skipped before the tmux-dependent body
// runs.
//
// Steps:
//
//   - Skip if `tmux` is not on PATH (CI without tmux still passes).
//   - Allocate a /tmp/fleet-test-<hex>.sock path (short to stay well
//     under the macOS ~104-byte UNIX socket length limit;
//     t.TempDir()-based paths overflow on TMUX_TMPDIR).
//   - t.Setenv("FLEET_TMUX_SOCKET", <sock>) so every tmuxArgs call in
//     this test routes to the per-test server.
//   - Register t.Cleanup that runs `tmux -S <sock> kill-server` AND
//     removes the socket file, regardless of pass/fail/panic. Without
//     the explicit os.Remove some tmux builds leave the .sock file
//     behind even after kill-server.
//
// For tests that should ALSO run on tmux-less CI (e.g., rejection /
// validation tests where runDispatch is expected to fail before
// reaching tmux.Spawn), use IsolateSocket(t) instead — it sets the
// env var and registers cleanup but does NOT skip on missing tmux.
//
// Why this is the canonical helper (not per-package copies):
// the AtomicCoordSwap subagent run on 2026-05-14 leaked 10+ fleet-*
// sessions onto the operator's default tmux server because tests
// reached spawn.Spawn through runDispatch without calling any
// isolation helper. The runtime sink guard in tmux.Spawn refuses to
// run under `go test` when FLEET_TMUX_SOCKET is empty, so isolation
// is the only way to make spawn-bearing tests pass.
func RequireTmux(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed; skipping integration test")
	}
	return IsolateSocket(t)
}

// IsolateSocket sets FLEET_TMUX_SOCKET to a unique per-test path and
// registers a t.Cleanup that kills the per-test tmux server + removes
// the socket file. Unlike RequireTmux it does NOT skip on missing
// tmux — used by tests whose code-under-test rejects BEFORE reaching
// tmux.Spawn so they keep covering the validation path on tmux-less CI.
// If a rejection ever regresses and tmux.Spawn is reached, the runtime
// sink guard sees FLEET_TMUX_SOCKET set and proceeds; on a tmux-less
// machine the spawn fails with a tmux-binary-missing error, which is
// the loud-failure mode we want (not a silent leak onto the host server).
//
// Codex review iter-1 [P2] (2026-05-15): the pre-split RequireTmux
// skipped the entire test on no-tmux CI, regressing coverage for
// rejection-only tests (unknown-engine / missing-project / coord-prefix
// gates) that previously ran via the original per-package
// isolateTmuxSocket helper.
func IsolateSocket(t *testing.T) string {
	t.Helper()
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
// 6-hex-char (24-bit, 16.7M-space) suffix lowers the birthday-collision
// probability to ~negligible across the dozens of tmux-backed tests
// `go test ./...` runs concurrently across packages. (Codex review
// iter-1 [P2] (2026-05-15): the earlier 2-byte / 16-bit suffix lived
// in a 65K-space, realistic to collide across cross-package parallel
// test runs.) Still short enough to stay well under the macOS ~104-byte
// UNIX socket length limit.
func isolatedSocketPath(t *testing.T) string {
	t.Helper()
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("tmuxtest: rand.Read: %v", err)
	}
	return "/tmp/fleet-test-" + hex.EncodeToString(b[:]) + ".sock"
}
