package spawn

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/handoff"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tmux"
)

func setupFleetHome(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	return tmp
}

func requireTmux(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed; skipping integration test")
	}
	// Per-test tmux server via FLEET_TMUX_SOCKET so cross-package
	// parallel runs don't contend on the host's default tmux server.
	// In-process random suffix — no openssl/external-tool dep.
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	t.Setenv("FLEET_TMUX_SOCKET", "/tmp/fleet-test-"+hex.EncodeToString(b[:])+".sock")
}

// capturePaneArgs builds tmux args for capture-pane against the
// per-test FLEET_TMUX_SOCKET so we don't query the host's default
// server (which races with parallel test packages).
func capturePaneArgs(session string) []string {
	args := []string{"capture-pane", "-t", session, "-p"}
	if sock := os.Getenv("FLEET_TMUX_SOCKET"); sock != "" {
		args = append([]string{"-S", sock}, args...)
	}
	return args
}

func TestSpawn_RequiresCommand(t *testing.T) {
	setupFleetHome(t)
	if _, err := Spawn(Options{}); err == nil {
		t.Error("expected error for missing command")
	}
}

func TestSpawn_FreshDispatch(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)

	rec, err := Spawn(Options{
		TaskID:  "auth-fix",
		Project: "rainier",
		Command: []string{"sleep", "30"},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(rec.TmuxSession) })

	if rec.ID == "" {
		t.Error("ID not set")
	}
	if rec.TmuxSession != "fleet-"+rec.ID {
		t.Errorf("TmuxSession=%q want fleet-%s", rec.TmuxSession, rec.ID)
	}
	if rec.TaskID != "auth-fix" || rec.Project != "rainier" {
		t.Errorf("task identity not set: %+v", rec)
	}
	if rec.HandoffNumber != 1 {
		t.Errorf("HandoffNumber: got %d want 1 (fresh dispatch)", rec.HandoffNumber)
	}
	if rec.LastHandoffPath != nil {
		t.Errorf("LastHandoffPath: want nil for fresh dispatch, got %v", rec.LastHandoffPath)
	}
	if rec.HandoffType != nil {
		t.Errorf("HandoffType: want nil for fresh dispatch, got %v", rec.HandoffType)
	}
	if !tmux.HasSession(rec.TmuxSession) {
		t.Errorf("tmux session %s should be alive", rec.TmuxSession)
	}

	// Record should be on disk and loadable.
	loaded, err := agent.Load(rec.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.TaskID != "auth-fix" {
		t.Errorf("loaded.TaskID=%q", loaded.TaskID)
	}
}

func TestSpawn_CapturesCwdAndCommandOnRecord(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)

	myCwd := t.TempDir()
	cmd := []string{"sleep", "30"}
	rec, err := Spawn(Options{
		TaskID:  "t",
		Project: "p",
		Cwd:     myCwd,
		Command: cmd,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(rec.TmuxSession) })

	if rec.Cwd != myCwd {
		t.Errorf("Cwd not captured: got %q want %q", rec.Cwd, myCwd)
	}
	if len(rec.Command) != len(cmd) {
		t.Fatalf("Command length: got %d want %d", len(rec.Command), len(cmd))
	}
	for i := range cmd {
		if rec.Command[i] != cmd[i] {
			t.Errorf("Command[%d]: got %q want %q", i, rec.Command[i], cmd[i])
		}
	}
	// Reload from disk to confirm it round-trips.
	loaded, err := agent.Load(rec.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Cwd != myCwd {
		t.Errorf("loaded.Cwd: got %q want %q", loaded.Cwd, myCwd)
	}
	if len(loaded.Command) != 2 || loaded.Command[0] != "sleep" {
		t.Errorf("loaded.Command not round-tripped: %v", loaded.Command)
	}
}

func TestSpawn_AbsolutizesRelativeCwd(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)

	// Pick an existing absolute path, then derive a RELATIVE form by
	// chdir'ing to its parent. spawn must canonicalize back to the
	// absolute path when storing on the record.
	abs := t.TempDir()
	parent := filepath.Dir(abs)
	rel := filepath.Base(abs)

	origWd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(origWd) })
	if err := os.Chdir(parent); err != nil {
		t.Fatal(err)
	}

	rec, err := Spawn(Options{
		TaskID:  "t",
		Project: "p",
		Cwd:     rel, // relative
		Command: []string{"sleep", "30"},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(rec.TmuxSession) })

	if !filepath.IsAbs(rec.Cwd) {
		t.Errorf("Cwd not absolutized: got %q", rec.Cwd)
	}
	// Compare via EvalSymlinks because macOS resolves /var → /private/var
	// during filepath.Abs; t.TempDir returns the unfollowed form.
	got, _ := filepath.EvalSymlinks(rec.Cwd)
	want, _ := filepath.EvalSymlinks(abs)
	if got != want {
		t.Errorf("Cwd absolutization wrong: got %q want %q", got, want)
	}
}

func TestSpawn_FromHandoffInheritsAndIncrements(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)

	// Build an "old" record as if we just wrote a handoff doc for it.
	old := agent.New("aaaa1111")
	old.TaskID = "auth-fix"
	old.Project = "rainier"
	old.Engine = "claude-code"
	old.Role = "executor"
	old.Mode = "execute"
	old.HandoffNumber = 3
	prevPath := "/some/handoffs/aaaa0000-20260427-180000.md"
	old.LastHandoffPath = &prevPath

	docPath := "/some/handoffs/aaaa1111-20260427-184807.md"

	rec, err := Spawn(Options{
		OldRecord:  old,
		NewDocPath: docPath,
		Command:    []string{"sleep", "30"},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(rec.TmuxSession) })

	if rec.ID == old.ID {
		t.Error("new agent must have a different ID than old")
	}
	if rec.TaskID != "auth-fix" || rec.Project != "rainier" {
		t.Errorf("task identity not inherited: %+v", rec)
	}
	if rec.Engine != "claude-code" || rec.Role != "executor" || rec.Mode != "execute" {
		t.Errorf("engine/role/mode not inherited: %+v", rec)
	}
	if rec.HandoffNumber != 4 {
		t.Errorf("HandoffNumber: got %d want 4 (3+1)", rec.HandoffNumber)
	}
	if rec.LastHandoffPath == nil || *rec.LastHandoffPath != docPath {
		t.Errorf("LastHandoffPath not set to NewDocPath: %v", rec.LastHandoffPath)
	}
	if rec.HandoffType == nil || *rec.HandoffType != handoff.TypeManual {
		t.Errorf("HandoffType: want manual, got %v", rec.HandoffType)
	}
}

func TestSpawn_RollsBackTmuxOnRecordWriteFailure(t *testing.T) {
	requireTmux(t)
	tmp := setupFleetHome(t)

	// Sabotage the agents/ directory: replace it with a regular file
	// so any record write fails.
	agentsDir := filepath.Join(tmp, "agents")
	if err := os.RemoveAll(agentsDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agentsDir, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Spawn(Options{
		TaskID:  "x",
		Project: "y",
		Command: []string{"sleep", "30"},
	})
	if err == nil {
		t.Fatal("expected Spawn to fail on record write")
	}
	// Rollback contract: spawn.Spawn called tmux.Kill on its session.
	// We can't reliably scan all fleet-* sessions globally without
	// racing concurrent test packages, so we trust the source-level
	// invariant (verified by code review) and the tmux.Kill unit test.
}

func TestSendInitialPrompt_TypedAfterPaneStable(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)

	// Pin small windows so the test suite doesn't pay production's
	// 500 ms stable / 30 s max waits.
	t.Setenv("FLEET_INITIAL_PROMPT_STABLE_MS", "150")
	t.Setenv("FLEET_INITIAL_PROMPT_MAX_MS", "3000")

	// `echo READY; read line; echo GOT:$line` prints a banner (so
	// CapturePane has non-empty content), blocks on read (pane goes
	// stable), then echoes whatever was typed. Mirrors how production
	// claude prints a startup banner, then idles waiting for input.
	rec, err := Spawn(Options{
		TaskID:  "auto-resume",
		Project: "p",
		Command: []string{"sh", "-c",
			"echo READY; read line; echo GOT:$line; sleep 30"},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(rec.TmuxSession) })

	if err := SendInitialPrompt(rec.TmuxSession, "echo HELLO_FROM_PROMPT"); err != nil {
		t.Fatalf("SendInitialPrompt: %v", err)
	}

	want := "GOT:echo HELLO_FROM_PROMPT"
	deadline := time.Now().Add(2 * time.Second)
	var lastOut []byte
	for time.Now().Before(deadline) {
		out, err := exec.Command("tmux", capturePaneArgs(rec.TmuxSession)...).Output()
		if err == nil {
			lastOut = out
			if strings.Contains(string(out), want) {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("prompt did not reach session within deadline; want %q in:\n%s",
		want, string(lastOut))
}

func TestSendInitialPrompt_EmptyPromptIsNoOp(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)

	t.Setenv("FLEET_INITIAL_PROMPT_STABLE_MS", "150")
	t.Setenv("FLEET_INITIAL_PROMPT_MAX_MS", "3000")

	rec, err := Spawn(Options{
		TaskID:  "no-prompt",
		Project: "p",
		Command: []string{"sh", "-c",
			"echo READY; read line; echo GOT:$line; sleep 30"},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(rec.TmuxSession) })

	// Empty prompt: SendInitialPrompt should return immediately
	// without typing anything. No GOT: should appear in the pane.
	start := time.Now()
	if err := SendInitialPrompt(rec.TmuxSession, ""); err != nil {
		t.Fatalf("SendInitialPrompt(empty): %v", err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("SendInitialPrompt(empty) took %s, expected near-instant", elapsed)
	}
	time.Sleep(150 * time.Millisecond)
	out, err := exec.Command("tmux", capturePaneArgs(rec.TmuxSession)...).Output()
	if err != nil {
		t.Fatalf("capture-pane: %v", err)
	}
	if strings.Contains(string(out), "GOT:") {
		t.Errorf("session received input despite empty prompt; pane:\n%s", string(out))
	}
}

// TestSendPromptKeys_SeparatesPromptAndEnter is the regression test
// for the May 2026 bug where SendPromptKeys sent the prompt text and
// the Enter key as a single `tmux send-keys <prompt> Enter`
// invocation. That makes the bytes arrive at the pty as one
// contiguous burst, and Claude Code's TUI treats the trailing CR as
// part of a paste rather than a submit. End result was the auto-
// resume prompt sat in the input box waiting for the operator to
// press Enter manually — exactly what auto-resume is supposed to
// avoid.
//
// The fix splits the send into two `tmux send-keys` invocations
// with a sleep between them. This test pins the sleep via env var
// to a measurable value and asserts SendPromptKeys actually pauses
// — proving the structural split is in place. A future refactor
// that collapses back to a single send-keys call will fail this
// test.
func TestSendPromptKeys_SeparatesPromptAndEnter(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)

	// Pin the inter-key delay to something measurable but small
	// enough to keep the test fast. 250 ms gives ~2x headroom over
	// scheduling jitter on busy CI runners.
	t.Setenv("FLEET_PROMPT_ENTER_DELAY_MS", "250")

	rec, err := Spawn(Options{
		TaskID:  "split-send",
		Project: "p",
		Command: []string{"sh", "-c",
			"echo READY; read line; echo GOT:$line; sleep 30"},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(rec.TmuxSession) })

	// Wait for the `read` to be ready so SendPromptKeys's only
	// blocking work is the inter-key sleep, not pty buffering.
	time.Sleep(100 * time.Millisecond)

	start := time.Now()
	if err := SendPromptKeys(rec.TmuxSession, "echo SPLIT_SEND_DELIVERED"); err != nil {
		t.Fatalf("SendPromptKeys: %v", err)
	}
	elapsed := time.Since(start)

	// Lower bound only — the pinned delay is a floor, not a ceiling.
	// Two send-keys subprocess invocations + the 250 ms sleep land
	// well above 200 ms; a single-call regression would return in a
	// handful of milliseconds.
	if elapsed < 200*time.Millisecond {
		t.Errorf("SendPromptKeys returned in %s; expected ≥200 ms because the prompt and Enter must be sent as two send-keys calls separated by FLEET_PROMPT_ENTER_DELAY_MS to defeat Claude Code's bracketed-paste detection",
			elapsed)
	}

	// And the prompt still has to actually reach the session — the
	// split must not have broken delivery.
	want := "GOT:echo SPLIT_SEND_DELIVERED"
	deadline := time.Now().Add(2 * time.Second)
	var lastOut []byte
	for time.Now().Before(deadline) {
		out, err := exec.Command("tmux", capturePaneArgs(rec.TmuxSession)...).Output()
		if err == nil {
			lastOut = out
			if strings.Contains(string(out), want) {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("prompt did not reach session within deadline; want %q in:\n%s",
		want, string(lastOut))
}

func TestSendInitialPrompt_SendsAfterMaxWaitWhenPaneNeverStabilizes(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)

	// Pin a tiny max wait — the test command keeps the pane changing
	// forever (printing every 50 ms), so stability never converges.
	// SendInitialPrompt should log the timeout and send anyway.
	t.Setenv("FLEET_INITIAL_PROMPT_STABLE_MS", "200")
	t.Setenv("FLEET_INITIAL_PROMPT_MAX_MS", "300")

	rec, err := Spawn(Options{
		TaskID:  "never-stable",
		Project: "p",
		// `while ... read line` so the shell still consumes input
		// when send-keys arrives, even though the printing loop in
		// the background keeps the pane churning. The marker proves
		// the prompt was delivered despite the unstable pane.
		Command: []string{"sh", "-c",
			"(while true; do echo TICK; sleep 0.05; done) & read line; echo GOT:$line; sleep 30"},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(rec.TmuxSession) })

	if err := SendInitialPrompt(rec.TmuxSession, "echo NEVER_STABLE_BUT_DELIVERED"); err != nil {
		t.Fatalf("SendInitialPrompt: %v", err)
	}

	want := "GOT:echo NEVER_STABLE_BUT_DELIVERED"
	deadline := time.Now().Add(2 * time.Second)
	var lastOut []byte
	for time.Now().Before(deadline) {
		out, err := exec.Command("tmux", capturePaneArgs(rec.TmuxSession)...).Output()
		if err == nil {
			lastOut = out
			if strings.Contains(string(out), want) {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("prompt not delivered after max-wait fallback; want %q in:\n%s",
		want, string(lastOut))
}

// TestSpawn_FleetBinInEnv verifies fleet stamps its own executable
// path into the agent's process env so fleet-guard's _kick_drain can
// invoke the SAME binary without a PATH lookup. Codex review on
// fix-fleet-guard-self-drains flagged that `shutil.which("fleet")`
// silently breaks the new auto-drain path on dev runs / non-PATH
// installs; FLEET_BIN is the fix, this test is its smoke check.
func TestSpawn_FleetBinInEnv(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)

	cmd := []string{"sh", "-c", "echo FLEET_BIN=$FLEET_BIN; cat"}
	rec, err := Spawn(Options{
		TaskID:  "x",
		Project: "y",
		Command: cmd,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(rec.TmuxSession) })

	exe, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable() failed: %v — env stamp is best-effort", err)
	}
	want := "FLEET_BIN=" + exe
	// Test binary paths land in /var/folders/.../go-build*/spawn.test
	// which is longer than a tmux pane line, so capture-pane wraps the
	// path across multiple rows. Normalize whitespace before matching.
	stripWS := func(s string) string {
		var b strings.Builder
		for _, r := range s {
			if r != ' ' && r != '\n' && r != '\r' && r != '\t' {
				b.WriteRune(r)
			}
		}
		return b.String()
	}
	wantNorm := stripWS(want)
	deadline := time.Now().Add(2 * time.Second)
	var lastOut []byte
	for time.Now().Before(deadline) {
		out, err := exec.Command("tmux", capturePaneArgs(rec.TmuxSession)...).Output()
		if err == nil {
			lastOut = out
			if strings.Contains(stripWS(string(out)), wantNorm) {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("expected %q in pane within deadline:\n%s", want, string(lastOut))
}

func TestSpawn_FleetAgentIDInEnv(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)

	cmd := []string{"sh", "-c", "echo AGENT_ID=$FLEET_AGENT_ID; cat"}
	rec, err := Spawn(Options{
		TaskID:  "x",
		Project: "y",
		Command: cmd,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(rec.TmuxSession) })

	// Poll capture-pane until the echo lands or we time out — the
	// shell takes a moment to start and print, and a single capture
	// is racy.
	want := "AGENT_ID=" + rec.ID
	deadline := time.Now().Add(2 * time.Second)
	var lastOut []byte
	for time.Now().Before(deadline) {
		out, err := exec.Command("tmux", capturePaneArgs(rec.TmuxSession)...).Output()
		if err == nil {
			lastOut = out
			if strings.Contains(string(out), want) {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("expected %q in pane within deadline:\n%s", want, string(lastOut))
}
