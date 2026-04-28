package tmux

import (
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
// the server on test exit.
func requireTmux(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed; skipping integration test")
	}
	sock := isolatedSocket(t)
	t.Setenv("FLEET_TMUX_SOCKET", sock)
}

// isolatedSocket returns a short /tmp path unique to this test run.
// 4-hex-char suffix is enough since tests within a process share a
// single test-binary PID and only one test runs at a time per package.
func isolatedSocket(t *testing.T) string {
	t.Helper()
	return "/tmp/fleet-test-" + randHex(t) + ".sock"
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
// names from colliding when run in parallel.
func randHex(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("openssl", "rand", "-hex", "2").Output()
	if err != nil {
		// Fallback if openssl is missing; not great but tests still
		// pass on CI without it.
		return "0000"
	}
	return strings.TrimSpace(string(out))
}
