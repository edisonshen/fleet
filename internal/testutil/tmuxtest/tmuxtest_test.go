package tmuxtest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// TestIsolateSocket_DoesNotSkipWhenTmuxMissing pins codex review iter-1
// [P2] (2026-05-15): IsolateSocket must NOT call t.Skip on missing tmux,
// so rejection-style tests in cmd/fleet keep their coverage on tmux-less
// CI. We exercise the invariant by calling IsolateSocket inside a
// subtest and verifying it returned (didn't skip) and set the env var.
// The subtest only knows it was skipped after its inner code ran; we
// just check the env-var side effect since LookPath cannot be mocked
// here.
func TestIsolateSocket_DoesNotSkipWhenTmuxMissing(t *testing.T) {
	var sock string
	t.Run("inner", func(t *testing.T) {
		sock = IsolateSocket(t)
	})
	if sock == "" {
		t.Fatal("IsolateSocket returned empty path — must succeed regardless of tmux availability")
	}
	if !strings.HasPrefix(sock, "/tmp/fleet-test-") {
		t.Errorf("IsolateSocket sock = %q; want /tmp/fleet-test-* prefix", sock)
	}
}

// TestKillServerAndRemove_DeletesDeadSocketFile is the regression for the
// Linux-CI socket leak (2026-06-07, 79 dead srw------- sockets / run). A
// dead socket file (no tmux server bound — the server already exited and
// left the inode behind) MUST be removed by the per-test cleanup. Before
// the verified-remove loop a single os.Remove could race the server's
// async unlink; this pins that the file is gone after killServerAndRemove
// returns. We forge a plain file at a fleet-test path to stand in for the
// dead socket (the kill-server call is a no-op against it) and assert it
// is unlinked deterministically.
func TestKillServerAndRemove_DeletesDeadSocketFile(t *testing.T) {
	// Use t.TempDir so we never touch the operator's real /tmp namespace.
	sock := filepath.Join(t.TempDir(), "fleet-test-dead.sock")
	if err := os.WriteFile(sock, []byte("dead"), 0o600); err != nil {
		t.Fatalf("forge dead socket: %v", err)
	}
	if err := killServerAndRemove(sock); err != nil {
		t.Fatalf("killServerAndRemove returned error for a dead socket: %v", err)
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Errorf("dead socket %s not removed (stat err=%v)", sock, err)
	}
}

// TestKillServerAndRemove_IdempotentWhenGone pins that calling cleanup on
// an already-absent socket is a clean no-op (ENOENT collapses to success)
// — the common case where tmux already unlinked the socket on server exit.
func TestKillServerAndRemove_IdempotentWhenGone(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "fleet-test-absent.sock")
	if err := killServerAndRemove(sock); err != nil {
		t.Errorf("killServerAndRemove on an absent socket should be nil, got %v", err)
	}
}

// TestKillServerAndRemove_RemovesCompanionLock pins the lock-file leak fix
// (CI run 27515608918): tmux 3.x writes a `<socket>.sock.lock` companion
// next to the socket, and a killed server leaves it behind. The CI leak
// gate matches `fleet-test-*` (broad), so a stranded `.sock.lock` reds the
// run even after the socket is gone. killServerAndRemove must unlink the
// companion lock too.
func TestKillServerAndRemove_RemovesCompanionLock(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "fleet-test-lockcheck.sock")
	lock := sock + ".lock"
	if err := os.WriteFile(sock, []byte("dead"), 0o600); err != nil {
		t.Fatalf("forge dead socket: %v", err)
	}
	if err := os.WriteFile(lock, []byte(""), 0o600); err != nil {
		t.Fatalf("forge companion lock: %v", err)
	}
	if err := killServerAndRemove(sock); err != nil {
		t.Fatalf("killServerAndRemove: %v", err)
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Errorf("socket %s not removed (stat err=%v)", sock, err)
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Errorf("companion lock %s not removed (stat err=%v)", lock, err)
	}
}

// TestKillServerAndRemove_StopsLiveServerAndDeletesSocket pins the CI
// failure mode: a real tmux server bound to a fleet-test socket must leave
// no /tmp/fleet-test-*.sock file behind after helper cleanup.
func TestKillServerAndRemove_StopsLiveServerAndDeletesSocket(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}

	sock := isolatedSocketPath(t)
	session := "fleet-test-cleanup"
	cmd := exec.Command("tmux", "-S", sock, "new-session", "-d", "-s", session, "sleep", "60")
	cmd.Env = append(os.Environ(), "TMUX=")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("start tmux server: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		cleanup := exec.Command("tmux", "-S", sock, "kill-server")
		cleanup.Env = append(os.Environ(), "TMUX=")
		_ = cleanup.Run()
		_ = os.Remove(sock)
	})
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("expected live tmux socket at %s: %v", sock, err)
	}

	if err := killServerAndRemove(sock); err != nil {
		t.Fatalf("killServerAndRemove live server: %v", err)
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Errorf("live tmux socket %s not removed (stat err=%v)", sock, err)
	}
}

// TestKillServerAndRemove_ConfirmsServerDeadBeforeRemove pins the
// ordering fix for the 230-socket Linux leak (ci-perf PR-2, 2026-06-14):
// killServerAndRemove must drive the tmux SERVER to actual death BEFORE
// unlinking the socket, so a dying server cannot re-create the inode after
// we return. After the helper returns, serverAlive(sock) must be false
// (no server can re-touch the file) AND the file must stay gone across a
// brief settle window.
func TestKillServerAndRemove_ConfirmsServerDeadBeforeRemove(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}

	sock := isolatedSocketPath(t)
	session := "fleet-test-deadcheck"
	cmd := exec.Command("tmux", "-S", sock, "new-session", "-d", "-s", session, "sleep", "60")
	cmd.Env = append(os.Environ(), "TMUX=")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("start tmux server: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		c := exec.Command("tmux", "-S", sock, "kill-server")
		c.Env = append(os.Environ(), "TMUX=")
		_ = c.Run()
		_ = os.Remove(sock)
	})

	if !serverAlive(sock) {
		t.Fatalf("expected a live tmux server bound to %s before cleanup", sock)
	}

	if err := killServerAndRemove(sock); err != nil {
		t.Fatalf("killServerAndRemove: %v", err)
	}

	// The server must be confirmed gone — nothing can re-create the inode.
	if serverAlive(sock) {
		t.Errorf("server still alive on %s after killServerAndRemove returned", sock)
	}
	// And the file must STAY gone (the old code could return on a momentary
	// ENOENT just before the dying server's final socket touch).
	time.Sleep(50 * time.Millisecond)
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Errorf("socket %s reappeared after cleanup (stat err=%v) — dead-server ordering not enforced", sock, err)
	}
}

// TestServerAlive_FalseWhenNoServer pins serverAlive's contract: an
// absent/unbound socket path reports not-alive, so killServerAndRemove's
// phase-1 wait exits immediately when there is no server to kill.
func TestServerAlive_FalseWhenNoServer(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	sock := isolatedSocketPath(t) // never bound
	if serverAlive(sock) {
		t.Errorf("serverAlive(%s) = true for an unbound socket; want false", sock)
	}
}

// TestKillServerAndRemove_RetriesUntilGone pins the verified-remove loop:
// a socket file that only disappears after a few attempts (simulating the
// Linux server-teardown race where the inode lingers briefly) is still
// reaped within the retry budget. We shrink the delay and spawn a
// background unlinker that removes the file after a short beat — the loop
// must succeed without erroring.
func TestKillServerAndRemove_RetriesUntilGone(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "fleet-test-laggy.sock")
	if err := os.WriteFile(sock, []byte("laggy"), 0o600); err != nil {
		t.Fatalf("forge laggy socket: %v", err)
	}
	// Make os.Remove fail by removing write/exec perms on the parent dir
	// for a beat, then restoring so a later attempt in the loop succeeds.
	// This exercises the retry path deterministically without depending on
	// real tmux timing.
	dir := filepath.Dir(sock)
	if err := os.Chmod(dir, 0o500); err != nil { // r-x: cannot unlink children
		t.Fatalf("chmod dir: %v", err)
	}
	restored := make(chan struct{})
	go func() {
		time.Sleep(15 * time.Millisecond)
		_ = os.Chmod(dir, 0o700) // restore so a retry can unlink
		close(restored)
	}()
	t.Cleanup(func() { <-restored; _ = os.Chmod(dir, 0o700) })

	// Give the loop enough iterations to outlast the 15ms perms window.
	oldDelay := socketRemoveDelay
	socketRemoveDelay = 5 * time.Millisecond
	t.Cleanup(func() { socketRemoveDelay = oldDelay })

	if err := killServerAndRemove(sock); err != nil {
		t.Fatalf("killServerAndRemove did not recover within the retry budget: %v", err)
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Errorf("laggy socket %s not removed after retries (stat err=%v)", sock, err)
	}
}

// TestIsolatedSocketPath_Width pins the 24-bit (6-hex-char) suffix.
// Birthday-collision math: with N parallel test sockets, P(collision)
// grows ~= N^2 / (2 * 2^24). For N ~= 200 dozen, P < 0.001 — safe.
// (Codex review iter-1 [P2] (2026-05-15) flagged the 16-bit version
// as collision-realistic.)
func TestIsolatedSocketPath_Width(t *testing.T) {
	sock := isolatedSocketPath(t)
	prefix := "/tmp/fleet-test-"
	suffix := ".sock"
	if len(sock) != len(prefix)+6+len(suffix) {
		t.Errorf("sock %q length %d; want prefix+6hex+suffix = %d",
			sock, len(sock), len(prefix)+6+len(suffix))
	}
}
