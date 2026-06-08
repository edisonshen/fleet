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
	"time"
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
//     removes the socket file (verified gone via a bounded stat+retry,
//     see killServerAndRemove), regardless of pass/fail/panic. Some
//     tmux builds — and the Linux CI runner specifically — leave the
//     .sock file behind even after kill-server; a single os.Remove can
//     race the server's async unlink, so the verified loop is what makes
//     /tmp leak-free deterministically.
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
		if err := killServerAndRemove(sock); err != nil {
			t.Errorf("tmuxtest: cleanup %s: %v", sock, err)
		}
	})
	return sock
}

// socketRemoveRetries / socketRemoveDelay bound the verified-remove loop
// in killServerAndRemove. Vars (not consts) so the unit test can shrink
// the delay and so the count is tunable if a slower CI kernel needs more
// settle iterations. 100 * 50ms = 5s worst case only when tmux or the
// filesystem keeps recreating the socket; the common case removes on the
// first attempt.
var (
	socketRemoveRetries = 100
	socketRemoveDelay   = 50 * time.Millisecond
)

// killServerAndRemove tears the per-test tmux server down and DELETES the
// socket file, verifying the file is actually gone before returning.
//
// Why the verify-with-retry (the leak this closes — Linux CI, 2026-06-07,
// 79 dead `srw-------` sockets / run): on Linux the tmux server unlinks
// its own socket asynchronously as it exits. A single os.Remove right
// after `kill-server` returns can race that teardown: the server has not
// finished unlinking, our Remove sees the file, removes it — fine — OR
// the server re-touches/leaves the inode such that one Remove is not
// enough and a dead 0-byte socket lingers. macOS tmux 3.6a never showed
// this; the Linux runner did, deterministically. The fix is to keep
// removing until a stat confirms the file is gone (ENOENT), bounded by a
// short retry budget so a genuinely stuck remove still returns instead of
// hanging the cleanup.
//
//	kill-server (idempotent) ─▶ remove ─▶ stat
//	                               ▲          │ still present?
//	                               └──────────┘ retry (bounded)
//
// Returns the last non-ENOENT error if the socket could not be removed
// within the budget (the caller logs it; the suite-level leak guard then
// surfaces it loudly rather than letting it pass silently).
func killServerAndRemove(sock string) error {
	// kill-server is idempotent: tmux exits non-zero when no server is
	// running on the socket, which is fine — we just want it gone.
	cmd := exec.Command("tmux", "-S", sock, "kill-server")
	cmd.Env = append(os.Environ(), "TMUX=")
	_ = cmd.Run()

	var lastErr error
	for attempt := 0; attempt < socketRemoveRetries; attempt++ {
		removeErr := os.Remove(sock)
		if removeErr != nil && !os.IsNotExist(removeErr) {
			lastErr = removeErr
		}
		// Verify it is actually gone — os.Remove returning nil is not
		// enough on the Linux race where the server re-creates/leaves the
		// inode after our unlink. stat is the source of truth.
		if _, statErr := os.Stat(sock); os.IsNotExist(statErr) {
			return nil
		}
		time.Sleep(socketRemoveDelay)
	}
	if lastErr != nil {
		return lastErr
	}
	// File still present after the budget but every Remove reported
	// success/ENOENT — surface a generic "still present" so the caller logs it.
	if _, statErr := os.Stat(sock); statErr == nil {
		return os.ErrExist
	}
	return nil
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
