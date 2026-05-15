package tmux

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// These tests touch the real `tmux` binary. Skip cleanly if it's not
// installed so CI without tmux still passes.
//
// Each test gets its own tmux server via FLEET_TMUX_SOCKET — a short
// path under /tmp so the macOS Unix-socket length limit isn't hit
// (TMUX_TMPDIR with t.TempDir()-based paths overflows). Cleans up
// the server AND removes the socket file on test exit (postmortem
// 2026-05-14: tests were leaving 2,800+ stale .sock files in /tmp/).
func requireTmux(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed; skipping integration test")
	}
	sock := isolatedSocket(t)
	t.Setenv("FLEET_TMUX_SOCKET", sock)
	cleanupTmuxSocket(t, sock)
}

// isolatedSocket returns a short /tmp path unique to this test run.
// 4-hex-char suffix is enough since tests within a process share a
// single test-binary PID and only one test runs at a time per package.
func isolatedSocket(t *testing.T) string {
	t.Helper()
	return "/tmp/fleet-test-" + randHex(t) + ".sock"
}

// cleanupTmuxSocket installs a t.Cleanup that kills the per-test tmux
// server and removes the socket file on test exit. Runs regardless of
// pass/fail/panic. Errors are swallowed — server may already be dead,
// socket may already be gone. Postmortem 2026-05-14 (orphan tmux leak):
// without this, every integration test left a tmux server + a .sock
// file behind, and a long-running operator's /tmp accumulated thousands
// of stale sockets plus a session-per-test on the default tmux server.
func cleanupTmuxSocket(t *testing.T, sock string) {
	t.Helper()
	t.Cleanup(func() {
		// kill-server is idempotent: tmux returns non-zero if the
		// server isn't running, which is fine — we just want it gone.
		_ = exec.Command("tmux", "-S", sock, "kill-server").Run()
		// Some tmux builds leave the socket file behind even after
		// kill-server; remove it explicitly.
		if err := os.Remove(sock); err != nil && !os.IsNotExist(err) {
			// Don't fail the test on cleanup error — surface it as a
			// log so the operator can investigate, but the test result
			// is already recorded.
			t.Logf("cleanupTmuxSocket: remove %s: %v", sock, err)
		}
	})
}

// capturePaneArgs builds the args for `tmux capture-pane` against the
// current FLEET_TMUX_SOCKET (if set). Tests use this so capture-pane
// hits the same isolated server as the test's other tmux ops.
func capturePaneArgs(session string) []string {
	args := []string{"capture-pane", "-t", session, "-p"}
	if sock := os.Getenv("FLEET_TMUX_SOCKET"); sock != "" {
		args = append([]string{"-S", sock}, args...)
	}
	return args
}

func TestSessionName(t *testing.T) {
	if got := SessionName("a1b2c3d4"); got != "fleet-a1b2c3d4" {
		t.Errorf("got %q, want fleet-a1b2c3d4", got)
	}
}

func TestAvailable(t *testing.T) {
	requireTmux(t)
	if err := Available(); err != nil {
		t.Errorf("Available: %v", err)
	}
}

// TestRequireTmux_SetsFleetTmuxSocket regresses postmortem 2026-05-14
// (orphan tmux leak): requireTmux MUST set FLEET_TMUX_SOCKET to a
// non-empty path before any test code runs, AND must register a
// cleanup that removes the socket file on exit. Without those two
// invariants integration tests leak tmux sessions onto the operator's
// default server and stale .sock files into /tmp.
//
// We exercise requireTmux from within a t.Run subtest so we can read
// FLEET_TMUX_SOCKET both during the subtest (must be set) and after
// the subtest completes (cleanup must have fired, the socket file
// must be gone).
func TestRequireTmux_SetsFleetTmuxSocket(t *testing.T) {
	// Capture the parent's FLEET_TMUX_SOCKET so we can detect that
	// requireTmux changed it. The outer test process may have inherited
	// a socket from a wrapping shell; we just need to see a different
	// value inside the subtest.
	parentSock := os.Getenv("FLEET_TMUX_SOCKET")

	var inSock string
	t.Run("inner", func(t *testing.T) {
		requireTmux(t) // sets FLEET_TMUX_SOCKET + registers cleanup
		inSock = os.Getenv("FLEET_TMUX_SOCKET")
		if inSock == "" {
			t.Fatal("requireTmux did not set FLEET_TMUX_SOCKET")
		}
		if inSock == parentSock {
			t.Fatalf("requireTmux did not override FLEET_TMUX_SOCKET; still %q", inSock)
		}
		if !strings.HasPrefix(inSock, "/tmp/fleet-test-") {
			t.Errorf("FLEET_TMUX_SOCKET = %q; want /tmp/fleet-test-* prefix", inSock)
		}
		// Touch the socket path to verify cleanup actually removes it.
		// requireTmux only creates the file when tmux later spawns a
		// session against it, so we forge one here.
		if err := os.WriteFile(inSock, []byte("probe"), 0o600); err != nil {
			t.Fatalf("write probe socket file: %v", err)
		}
	})
	// After the subtest's t.Cleanup fires, the socket file must be
	// gone. (We don't assert on the env var because t.Setenv inside
	// the subtest restored it on subtest exit.)
	if _, err := os.Stat(inSock); !os.IsNotExist(err) {
		t.Errorf("requireTmux cleanup did not remove %s (stat err=%v)", inSock, err)
	}
}

func TestParseTmuxVersion(t *testing.T) {
	// Codex iter-11 P1: tmux 3.2+ required for `new-session -e`.
	// Cover the version strings tmux actually emits.
	cases := []struct {
		in    string
		major int
		minor int
		ok    bool
	}{
		{"tmux 3.5a\n", 3, 5, true},
		{"tmux 3.2\n", 3, 2, true},
		{"tmux 2.6\n", 2, 6, true},
		{"tmux next-3.5\n", 3, 5, true},
		{"tmux master-pre-3.6\n", 3, 6, true},
		{"garbage", 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(strings.TrimSpace(tc.in), func(t *testing.T) {
			maj, min, err := parseTmuxVersion(tc.in)
			if tc.ok && err != nil {
				t.Fatalf("parseTmuxVersion(%q): unexpected err %v", tc.in, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("parseTmuxVersion(%q): expected error", tc.in)
			}
			if tc.ok && (maj != tc.major || min != tc.minor) {
				t.Errorf("parseTmuxVersion(%q): got %d.%d want %d.%d", tc.in, maj, min, tc.major, tc.minor)
			}
		})
	}
}

func TestSpawnAndKill_RoundTrip(t *testing.T) {
	requireTmux(t)

	session := "fleet-test-" + randHex(t)
	t.Cleanup(func() { _ = Kill(session) })

	if HasSession(session) {
		t.Fatalf("session %s already exists", session)
	}

	if err := Spawn(session, "", []string{"sleep", "30"}, nil); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if !HasSession(session) {
		t.Errorf("HasSession returned false after Spawn")
	}
	if err := Kill(session); err != nil {
		t.Errorf("Kill: %v", err)
	}
	if HasSession(session) {
		t.Errorf("HasSession returned true after Kill")
	}
}

func TestKill_IdempotentOnMissing(t *testing.T) {
	requireTmux(t)
	if err := Kill("fleet-test-nonexistent-" + randHex(t)); err != nil {
		t.Errorf("Kill on missing session should be nil, got %v", err)
	}
}

// TestListSessions_NoServerReturnsEmpty covers the
// no-server-running case: ListSessions must collapse it into
// "no sessions, no error" instead of surfacing it as a probe failure.
// `fleet maintenance prune-orphan-tmux` should print a clean empty
// report when there's no tmux server, not fail.
func TestListSessions_NoServerReturnsEmpty(t *testing.T) {
	requireTmux(t)
	// requireTmux gave us a fresh, isolated socket; no sessions yet.
	got, err := ListSessions()
	if err != nil {
		t.Errorf("ListSessions on fresh socket: got err %v; want nil", err)
	}
	if len(got) != 0 {
		t.Errorf("ListSessions on fresh socket: got %v; want empty slice", got)
	}
}

// TestListSessions_RoundTripWithSpawnedSession is the happy path:
// spawn a session, ListSessions sees it. Cleanup kills it.
func TestListSessions_RoundTripWithSpawnedSession(t *testing.T) {
	requireTmux(t)
	session := "fleet-test-" + randHex(t)
	t.Cleanup(func() { _ = Kill(session) })
	if err := Spawn(session, "", []string{"sleep", "30"}, nil); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	got, err := ListSessions()
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	found := false
	for _, s := range got {
		if s == session {
			found = true
		}
	}
	if !found {
		t.Errorf("ListSessions did not include spawned %q; got %v", session, got)
	}
}

// TestListSessionsWithCreated_NoServerReturnsEmpty mirrors
// TestListSessions_NoServerReturnsEmpty for the timestamped variant.
// `fleet maintenance prune-orphan-tmux` relies on this collapsing
// no-server into "no orphans" rather than surfacing a probe error.
func TestListSessionsWithCreated_NoServerReturnsEmpty(t *testing.T) {
	requireTmux(t)
	got, err := ListSessionsWithCreated()
	if err != nil {
		t.Errorf("ListSessionsWithCreated on fresh socket: got err %v; want nil", err)
	}
	if len(got) != 0 {
		t.Errorf("ListSessionsWithCreated on fresh socket: got %v; want empty slice", got)
	}
}

// TestListSessionsWithCreated_RoundTripRecordsCreation spawns a session
// and verifies ListSessionsWithCreated returns it WITH a non-zero
// Created timestamp roughly matching wall-clock — the prune-orphan-tmux
// freshness gate (codex iter-2 [P1]) depends on this metadata.
func TestListSessionsWithCreated_RoundTripRecordsCreation(t *testing.T) {
	requireTmux(t)
	session := "fleet-test-" + randHex(t)
	t.Cleanup(func() { _ = Kill(session) })

	before := time.Now()
	if err := Spawn(session, "", []string{"sleep", "30"}, nil); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	after := time.Now()

	got, err := ListSessionsWithCreated()
	if err != nil {
		t.Fatalf("ListSessionsWithCreated: %v", err)
	}
	var found *SessionInfo
	for i := range got {
		if got[i].Name == session {
			found = &got[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("ListSessionsWithCreated did not include spawned %q; got %v", session, got)
	}
	if found.Created.IsZero() {
		t.Errorf("expected non-zero Created for %q; got zero (parse failure?)", session)
	}
	// Allow a 5s slack on either side — tmux session_created is a
	// Unix second so it rounds; wall-clock skew on a busy test runner
	// is small but real.
	lo := before.Add(-5 * time.Second)
	hi := after.Add(5 * time.Second)
	if found.Created.Before(lo) || found.Created.After(hi) {
		t.Errorf("Created %v outside expected window [%v, %v]", found.Created, lo, hi)
	}
}

func TestSpawn_EmptyCommand(t *testing.T) {
	if err := Spawn("any", "", nil, nil); err == nil {
		t.Error("expected error for empty command")
	}
}

func TestSpawn_ExtraEnvPropagatesToCommand(t *testing.T) {
	requireTmux(t)

	session := "fleet-test-" + randHex(t)
	t.Cleanup(func() { _ = Kill(session) })

	// Spawn `sh -c 'echo $FLEET_TEST_PROBE; cat'` so the env var is
	// echoed at startup and the shell stays alive (cat) for capture.
	cmd := []string{"sh", "-c", "echo $FLEET_TEST_PROBE; cat"}
	if err := Spawn(session, "", cmd, []string{"FLEET_TEST_PROBE=handoff-week-4a"}); err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	// Poll capture-pane until the echo shows up — single-shot capture
	// races against shell startup.
	deadline := time.Now().Add(2 * time.Second)
	var lastOut []byte
	for time.Now().Before(deadline) {
		out, err := exec.Command("tmux", capturePaneArgs(session)...).Output()
		if err == nil {
			lastOut = out
			if strings.Contains(string(out), "handoff-week-4a") {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("extra env did not reach the command within deadline:\n%s", string(lastOut))
}

func TestSpawn_AllowsShortLivedCommands(t *testing.T) {
	// Codex P3 (2026-04-27): tmux removes a session as soon as its
	// command exits. The post-spawn HasSession check must NOT flag
	// short-lived commands (e.g., `sh -c true`) as silent failures —
	// they ran successfully, the session is just already gone.
	//
	// Heuristic: only require HasSession when stderr signals trouble.
	// Successful short-lived runs leave stderr empty; silent socket
	// failures print to stderr.
	requireTmux(t)
	session := "fleet-test-shortlived-" + randHex(t)
	t.Cleanup(func() { _ = Kill(session) })
	if err := Spawn(session, "", []string{"sh", "-c", "true"}, nil); err != nil {
		t.Errorf("expected Spawn(short-lived) to succeed, got: %v", err)
	}
}

func TestSpawn_FailsWhenSocketUnusable(t *testing.T) {
	// If FLEET_TMUX_SOCKET points at an unwritable path, tmux can
	// print errors but exit 0 — the post-spawn HasSession check
	// catches that case so spawn.Spawn doesn't write a record for a
	// session that never existed.
	requireTmux(t)
	// Override to a deeply nested path that exceeds macOS's 104-byte
	// UNIX socket limit. tmux either fails to bind (socket too long)
	// or the surrounding dir doesn't exist.
	t.Setenv("FLEET_TMUX_SOCKET", "/tmp/"+strings.Repeat("a", 200)+".sock")

	session := "fleet-test-" + randHex(t)
	err := Spawn(session, "", []string{"sleep", "30"}, nil)
	if err == nil {
		_ = Kill(session)
		t.Fatal("expected Spawn to fail with unusable socket path")
	}
	// Error message should mention either creation failure or the
	// post-spawn HasSession safety net.
	if !strings.Contains(err.Error(), "tmux") {
		t.Errorf("expected tmux-related error, got: %v", err)
	}
}

func TestSpawn_ExtraEnvWorksAgainstExistingServer(t *testing.T) {
	// Codex P1 (2026-04-27): cmd.Env doesn't propagate when the tmux
	// client connects to an existing server. The fix is to pass env
	// via `tmux new-session -e KEY=VALUE`. This test exercises that
	// path by pre-starting the tmux server (so the Spawn under test
	// connects to an existing one), then verifying the env var still
	// reaches the command.
	requireTmux(t)

	// Pre-start the tmux server by creating + killing a throwaway
	// session. After this, the server is up but no fleet-* sessions
	// exist; the next Spawn call connects to the running server.
	prep := "fleet-test-prep-" + randHex(t)
	if err := Spawn(prep, "", []string{"sleep", "10"}, nil); err != nil {
		t.Fatalf("prep Spawn: %v", err)
	}
	if err := Kill(prep); err != nil {
		t.Fatalf("prep Kill: %v", err)
	}

	session := "fleet-test-" + randHex(t)
	t.Cleanup(func() { _ = Kill(session) })

	cmd := []string{"sh", "-c", "echo PROBE=$EXISTING_SERVER_PROBE; cat"}
	if err := Spawn(session, "", cmd, []string{"EXISTING_SERVER_PROBE=via-dash-e-flag"}); err != nil {
		t.Fatalf("Spawn against existing server: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	var lastOut []byte
	for time.Now().Before(deadline) {
		out, err := exec.Command("tmux", capturePaneArgs(session)...).Output()
		if err == nil {
			lastOut = out
			if strings.Contains(string(out), "PROBE=via-dash-e-flag") {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("env did not reach session spawned against existing server:\n%s",
		string(lastOut))
}

func TestSendKeys_NoSession(t *testing.T) {
	requireTmux(t)
	err := SendKeys("fleet-test-nonexistent-"+randHex(t), "hello", "Enter")
	if err == nil {
		t.Fatal("expected ErrNoSession")
	}
	if !strings.Contains(err.Error(), "tmux session not found") {
		t.Errorf("expected ErrNoSession-shaped error, got %v", err)
	}
}

func TestSendKeys_DeliversToSession(t *testing.T) {
	requireTmux(t)

	session := "fleet-test-" + randHex(t)
	t.Cleanup(func() { _ = Kill(session) })

	// `cat` echoes whatever we type — perfect probe for send-keys.
	if err := Spawn(session, "", []string{"cat"}, nil); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if err := SendKeys(session, "fleet-handoff-probe", "Enter"); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}

	// Poll capture-pane until the text round-trips through cat.
	deadline := time.Now().Add(2 * time.Second)
	var lastOut []byte
	for time.Now().Before(deadline) {
		out, err := exec.Command("tmux", capturePaneArgs(session)...).Output()
		if err == nil {
			lastOut = out
			if strings.Contains(string(out), "fleet-handoff-probe") {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("send-keys output missing within deadline:\n%s", string(lastOut))
}

// randHex returns 4 random lowercase hex chars to keep test session
// names from colliding when run in parallel. In-process so tests
// don't depend on external tools beyond `tmux` itself.
func randHex(t *testing.T) string {
	t.Helper()
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return hex.EncodeToString(b[:])
}
