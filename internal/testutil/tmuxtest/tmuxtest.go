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
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"regexp"
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

// defaultProbeTimeout caps a single `pgrep` liveness probe so a stalled
// /proc scan can't outrun the killServerAndRemove budget (codex PR-2 iter-7
// [P2]). The deadline-bounded loop additionally clamps each probe to the
// time remaining, so the TOTAL Phase-1 wait stays at ~socketRemoveRetries *
// socketRemoveDelay regardless of per-probe latency.
const defaultProbeTimeout = time.Second

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
	//
	// codex PR-2 iter-7 [P2]: the wait is bounded by a GLOBAL DEADLINE, not
	// just the iteration count. serverAlive shells out to `pgrep` (under its
	// own timeout); a fresh per-call timeout × socketRemoveRetries iterations
	// could balloon the advertised ~5s budget to minutes on a runner with a
	// stalled /proc. The deadline (retries × delay) caps the TOTAL Phase-1
	// wall time regardless of per-probe latency: each serverAlive probe is
	// given only the time remaining to the deadline, and the loop exits the
	// instant the deadline passes.
	deadline := time.Now().Add(time.Duration(socketRemoveRetries) * socketRemoveDelay)
	serverDead := !serverAliveBefore(sock, deadline)
	for attempt := 0; attempt < socketRemoveRetries && !serverDead; attempt++ {
		if !time.Now().Before(deadline) {
			break // global budget exhausted — fall through to the leak error
		}
		// Server still answering: re-issue kill-server (idempotent) in case
		// the first signal raced a mid-startup server, back off, then re-probe.
		killCmd := exec.Command("tmux", "-S", sock, "kill-server")
		killCmd.Env = append(os.Environ(), "TMUX=")
		_ = killCmd.Run()
		time.Sleep(socketRemoveDelay)
		serverDead = !serverAliveBefore(sock, deadline)
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

	// Phase 2: the server is gone — unlink the orphaned socket file AND its
	// tmux companion lock `<sock>.lock`, which tmux 3.x writes next to the
	// socket and a killed server leaves behind (the leak the CI gate caught
	// in run 27515608918). Verify BOTH stay gone before reporting success.
	//
	// codex PR-2 iter-6 [P2]: an earlier rev returned nil as soon as the
	// SOCKET was gone, ignoring whether the `.lock` removal actually
	// succeeded. The broad CI leak gate matches `fleet-test-*` (lock files
	// included), so a leaked `.lock` would red the gate even though this
	// helper claimed a clean reap. Now both files gate the success return,
	// and a `.lock` removal error is surfaced to the caller.
	//
	// With no bound server nothing re-creates the inodes, so this normally
	// succeeds on the first attempt; the retry only covers a slow async
	// unlink finishing under us.
	lock := sock + ".lock"
	var lastErr error
	for attempt := 0; attempt < socketRemoveRetries; attempt++ {
		if removeErr := os.Remove(sock); removeErr != nil && !os.IsNotExist(removeErr) {
			lastErr = removeErr
		}
		if lockErr := os.Remove(lock); lockErr != nil && !os.IsNotExist(lockErr) {
			lastErr = lockErr
		}
		_, sockStat := os.Stat(sock)
		_, lockStat := os.Stat(lock)
		if os.IsNotExist(sockStat) && os.IsNotExist(lockStat) {
			return nil
		}
		time.Sleep(socketRemoveDelay)
	}
	if lastErr != nil {
		return lastErr
	}
	// A file still present after the budget but every Remove reported
	// success/ENOENT — surface a generic "still present" so the caller logs
	// it. Name which artifact lingered for a faster diagnosis.
	if _, statErr := os.Stat(sock); statErr == nil {
		return fmt.Errorf("tmuxtest: socket %s still present after remove budget", sock)
	}
	if _, statErr := os.Stat(lock); statErr == nil {
		return fmt.Errorf("tmuxtest: companion lock %s still present after remove budget", lock)
	}
	return nil
}

// serverAlive reports whether a tmux server that could still re-create
// sock's inode is bound to it. It must return true for the WHOLE window in
// which a re-touch is possible, not just while the control socket answers.
//
// codex PR-2 iter-2 [P1]: a `tmux -S <sock> list-sessions` probe is NOT a
// sufficient liveness signal on its own. During shutdown the server stops
// answering control commands BEFORE the process exits, so the probe fails
// while the dying process can still unlink-and-re-create the socket. If we
// treated that probe-failure as "server dead" and unlinked, the dying
// server would re-touch the inode after us — the exact Linux leak this code
// exists to close. So we OR two signals:
//
//	answering := list-sessions succeeds   (server up, normal operation)
//	bound     := a tmux process references this socket path   (pgrep -f)
//
// The server is "alive" (can still re-create the inode) while EITHER holds.
// Only once the probe fails AND no tmux process is bound to the socket is a
// re-touch impossible — and only then does killServerAndRemove unlink. We
// suppress TMUX inheritance so a parent tmux session never makes the probe
// answer about the wrong server.
//
// pgrep is best-effort: if it is missing or errors we fall back to the
// probe alone (no worse than the prior behavior) rather than spin forever.
//
// serverAlive uses a fixed default probe timeout; the deadline-bounded
// killServerAndRemove loop calls serverAliveBefore instead.
func serverAlive(sock string) bool {
	return serverAliveBefore(sock, time.Now().Add(defaultProbeTimeout))
}

// serverAliveBefore is serverAlive with a hard deadline: BOTH probes (the
// `list-sessions` control probe and the `pgrep` process-bound probe) are
// given only the time remaining to `deadline`, capped at defaultProbeTimeout.
// So the killServerAndRemove loop's total wait stays bounded even when tmux's
// control socket OR pgrep is slow (codex PR-2 iter-7 [P2]; /review iter [P2]:
// the list-sessions probe was previously unbounded). A deadline already in the
// past skips both probes (the loop is about to exit on its budget anyway).
func serverAliveBefore(sock string, deadline time.Time) bool {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return false // budget spent — treat as "no longer holding the inode"
	}
	if remaining > defaultProbeTimeout {
		remaining = defaultProbeTimeout
	}
	// Control probe under the same bound: a tmux server wedged on its control
	// socket must not block past the budget. A timed-out/failed probe means
	// "not answering"; we then check the process-bound signal below.
	ctx, cancel := context.WithTimeout(context.Background(), remaining)
	defer cancel()
	cmd := exec.CommandContext(ctx, "tmux", "-S", sock, "list-sessions")
	cmd.Env = append(os.Environ(), "TMUX=")
	if cmd.Run() == nil {
		return true
	}
	return tmuxProcessBound(sock, remaining)
}

// tmuxProcessBound reports whether any tmux process still references sock in
// its argv (a server `tmux: server (<sock>)` or a client `tmux -S <sock>`).
// This catches the shutdown window where the control probe already fails but
// the process has not exited and can still re-touch the socket inode.
//
// The pattern requires BOTH `tmux` AND the literal socket path in the argv
// (/review P2): matching the socket path alone would treat ANY process whose
// argv merely mentions the path (a shell, a logger, a sibling test's helper)
// as a live tmux server, spinning killServerAndRemove's bounded loop to its
// budget and falsely reporting a leak. Requiring `tmux` narrows the match to
// actual tmux server/client processes, which is the only thing that can
// re-create the socket inode.
//
// Best-effort with a caller-supplied timeout (/review P3, codex iter-7 [P2]):
// a missing pgrep, any pgrep error, or a pgrep that outruns `timeout` returns
// false so the caller's bounded loop never hangs on a stalled /proc scan.
// pgrep exits 1 when there is no match (the common, healthy case) — we treat
// only a clean exit with non-empty output as "still bound".
func tmuxProcessBound(sock string, timeout time.Duration) bool {
	if _, err := exec.LookPath("pgrep"); err != nil {
		return false
	}
	if timeout <= 0 {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	// regexp.QuoteMeta the path (a regex to pgrep -f); `.*` between `tmux`
	// and the path matches both the server proctitle `tmux: server (<sock>)`
	// and a client `tmux -S <sock>`.
	pat := `tmux.*` + regexp.QuoteMeta(sock)
	out, err := exec.CommandContext(ctx, "pgrep", "-f", pat).Output()
	if err != nil {
		// Exit 1 (no match), timeout, or any pgrep failure → not bound
		// (or unknown). Either way the caller falls through safely.
		return false
	}
	return len(bytes.TrimSpace(out)) > 0
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
