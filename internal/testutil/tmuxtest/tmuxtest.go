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
	"fmt"
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

// socketRemoveRetries / socketRemoveDelay bound BOTH bounded loops in
// killServerAndRemove (phase 1: wait for the server to die; phase 2:
// verified remove). Vars (not consts) so the unit test can shrink the delay
// and so the count is tunable if a slower CI kernel needs more settle
// iterations. Each phase is capped at 100 * 50ms = 5s, so the worst case is
// ~10s combined — and only when tmux or the filesystem keeps recreating the
// socket; the common case kills + removes on the first attempt. The
// per-test `go test -timeout` is the hard backstop above this.
var (
	socketRemoveRetries = 100
	socketRemoveDelay   = 50 * time.Millisecond
)

// killServerAndRemove tears the per-test tmux server down and DELETES the
// socket file, verifying the file is actually gone before returning.
//
// Why "confirm the server is DEAD before the final remove" (the leak this
// closes — Linux CI, 2026-06-07 + the 230 `srw-------` sockets on 2026-06-14):
// on Linux the tmux server unlinks AND can re-create its listen socket
// asynchronously during shutdown. The old code removed the file then
// returned the instant a single stat saw ENOENT — but if that stat caught
// the brief window before the dying server's final socket touch, tmux
// re-created the inode AFTER we returned, leaving a dead 0-byte `srw-------`
// socket with no live server. Removing harder doesn't help: as long as the
// server PROCESS is still alive it can keep re-touching the inode, so
// remove-until-gone races a live writer.
//
// The fix is ordering: first drive the SERVER to actual death
// (`kill-server`, then poll `list-sessions` until it fails — the server no
// longer answers, i.e. the process is gone and cannot re-create the
// socket), THEN unlink the now-orphaned file and verify it stays gone. Once
// no server is bound, nothing can re-create the inode, so a single
// successful remove is final.
//
//	kill-server ─▶ poll list-sessions until FAIL (server dead)
//	                          │
//	                          ▼
//	              remove file ─▶ stat (confirm gone) ─▶ retry (bounded)
//
// Both phases share the same bounded retry budget so a genuinely stuck
// server / filesystem still returns (logged by the caller) instead of
// hanging the cleanup. Returns the last non-ENOENT error if the socket
// could not be removed within the budget (the caller logs it; the
// suite-level leak guard then surfaces it loudly rather than passing
// silently).
func killServerAndRemove(sock string) error {
	// kill-server is idempotent: tmux exits non-zero when no server is
	// running on the socket, which is fine — we just want it gone.
	cmd := exec.Command("tmux", "-S", sock, "kill-server")
	cmd.Env = append(os.Environ(), "TMUX=")
	_ = cmd.Run()

	// Phase 1: wait until the server is CONFIRMED dead. While
	// `list-sessions` still succeeds a server process is bound to the
	// socket and can re-create the inode after we unlink it (the Linux
	// re-touch race). Probe FIRST (the kill-server above may already have
	// done the job), then on each iteration re-kill, back off, and re-probe
	// — so the LAST iteration's kill still gets a confirming probe. A failed
	// probe (no server / file already gone / error) all mean "no server can
	// recreate the inode", which is what we need.
	serverDead := !serverAlive(sock)
	for attempt := 0; attempt < socketRemoveRetries && !serverDead; attempt++ {
		// Server still answering: re-issue kill-server (idempotent) in case
		// the first signal raced a mid-startup server, back off, then re-probe.
		killCmd := exec.Command("tmux", "-S", sock, "kill-server")
		killCmd.Env = append(os.Environ(), "TMUX=")
		_ = killCmd.Run()
		time.Sleep(socketRemoveDelay)
		serverDead = !serverAlive(sock)
	}

	// codex PR2 iter-2 [P2]: if the server NEVER died within the budget (a
	// hung/wedged tmux), do NOT silently unlink-and-return-nil — that would
	// reopen the live-writer race this fix closes (the server can re-create
	// the inode after our remove) AND mask the leak. Best-effort unlink so
	// we don't strand the file either, then return a loud error so the
	// caller logs it and the suite-level leak gate surfaces it.
	if !serverDead {
		_ = os.Remove(sock) // best-effort; a live server may re-touch it
		_ = os.Remove(sock + ".lock")
		return fmt.Errorf("tmuxtest: tmux server on %s still alive after kill-server budget (%d * %v); socket may leak",
			sock, socketRemoveRetries, socketRemoveDelay)
	}

	// Phase 2: the server is gone — unlink the orphaned socket file (and its
	// tmux companion lock `<sock>.lock`, which tmux 3.x writes next to the
	// socket and a killed server leaves behind — the leak the CI gate caught
	// in run 27515608918). Verify the socket stays gone. With no bound server
	// nothing re-creates the inode, so this normally succeeds on the first
	// attempt; the retry only covers a slow async unlink finishing under us.
	var lastErr error
	for attempt := 0; attempt < socketRemoveRetries; attempt++ {
		removeErr := os.Remove(sock)
		if removeErr != nil && !os.IsNotExist(removeErr) {
			lastErr = removeErr
		}
		_ = os.Remove(sock + ".lock") // companion lock; ENOENT is fine
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

// serverAlive reports whether a tmux server is currently bound to sock and
// still answering. A successful `tmux -S <sock> list-sessions` means a live
// server owns the socket and could re-create the inode after an unlink;
// any failure (no server, file gone, probe error) means no server can. We
// suppress TMUX inheritance so a parent tmux session never makes the probe
// answer about the wrong server.
func serverAlive(sock string) bool {
	cmd := exec.Command("tmux", "-S", sock, "list-sessions")
	cmd.Env = append(os.Environ(), "TMUX=")
	return cmd.Run() == nil
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
