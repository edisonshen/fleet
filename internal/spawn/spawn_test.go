package spawn

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/handoff"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/testutil/tmuxtest"
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

// requireTmux delegates socket isolation to tmuxtest.RequireTmux (the
// canonical helper at internal/testutil/tmuxtest) and adds the
// spawn-specific env pin. Postmortem 2026-05-14 (orphan tmux leak) +
// follow-up 2026-05-15: tmux.Spawn under `go test` refuses to use the
// default socket, and tmuxtest.RequireTmux is the lint-recognized
// isolation marker.
func requireTmux(t *testing.T) {
	t.Helper()
	tmuxtest.RequireTmux(t)
	// Speed up the pid-resolver poll budget in tests. Production uses
	// 10s; tests with synthetic commands ("sleep 30", "sh -c sleep 60")
	// will never find a "claude" descendant, so the resolver will run
	// to timeout. 1s keeps the test wall time bounded while still
	// exercising the polling loop.
	t.Setenv("FLEET_PID_RESOLVE_S", "1")
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
	// Defensive isolation (codex review iter-10 [P3]): the empty-command
	// rejection fires before tmux.Spawn, but isolate the socket so a
	// regression cannot leak onto the operator's default tmux server.
	tmuxtest.IsolateSocket(t)
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

// TestSpawn_RecordsRealEnginePidNotFleetBinaryPid is the regression
// test for the P0 bug surfaced 2026-05-13: spawn used to write
// os.Getpid() (the fleet binary's own pid) into agent.Record.PID. That
// pid dies as soon as `fleet dispatch` exits, so every downstream
// liveness probe (TUI dead-coord sweep, coord reconcile) classified
// every coord as DEAD by construction.
//
// After the fix, rec.PID must be the pid of the engine running inside
// the tmux pane — not the test binary's pid. We verify by spawning a
// long-lived `sleep` and checking the recorded pid:
//   - is not the test process's pid (the bug's signature)
//   - corresponds to a live process (kill -0 succeeds)
//   - matches a child of the tmux pane pid
func TestSpawn_RecordsRealEnginePidNotFleetBinaryPid(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)

	rec, err := Spawn(Options{
		TaskID:  "pid-resolution",
		Project: "rainier",
		// Long-lived child so the pid is still alive when we probe.
		Command: []string{"sh", "-c", "sleep 60"},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(rec.TmuxSession) })

	// Bug signature: recorded pid == os.Getpid().
	if rec.PID == os.Getpid() {
		t.Fatalf("recorded pid = test-process pid (%d) — spawn-pid bug regressed", rec.PID)
	}
	if rec.PID <= 0 {
		t.Fatalf("recorded pid invalid: %d", rec.PID)
	}

	// Live process check via kill(0). EPERM also means alive; we don't
	// expect EPERM here since the spawned tmux session runs as the
	// same user as the test.
	if err := syscall.Kill(rec.PID, 0); err != nil {
		t.Errorf("recorded pid %d is not alive: %v", rec.PID, err)
	}

	// Round-trips to disk with the real pid.
	loaded, err := agent.Load(rec.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.PID != rec.PID {
		t.Errorf("loaded.PID = %d, want %d", loaded.PID, rec.PID)
	}
}

// TestSpawn_EngineOverride pins the v0.9 multi-engine MVP: when
// Options.Engine is set, Spawn stamps that name on the new agent
// record (instead of agent.DefaultEngine). Empty Engine preserves the
// pre-v0.9 byte shape (DefaultEngine wins). Round-trips to disk.
func TestSpawn_EngineOverride(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)

	rec, err := Spawn(Options{
		TaskID:  "t",
		Project: "p",
		Command: []string{"sleep", "30"},
		Engine:  "codex",
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(rec.TmuxSession) })

	if rec.Engine != "codex" {
		t.Errorf("rec.Engine = %q, want \"codex\"", rec.Engine)
	}
	loaded, err := agent.Load(rec.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Engine != "codex" {
		t.Errorf("loaded.Engine = %q, want \"codex\"", loaded.Engine)
	}
}

func TestSpawn_EngineEmptyPreservesDefault(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)

	rec, err := Spawn(Options{
		TaskID:  "t",
		Project: "p",
		Command: []string{"sleep", "30"},
		// Engine intentionally empty: legacy callers shouldn't see a
		// byte-shape change.
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(rec.TmuxSession) })

	if rec.Engine != agent.DefaultEngine {
		t.Errorf("rec.Engine = %q, want %q (default preserved)",
			rec.Engine, agent.DefaultEngine)
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
	// 500 ms stable / 30 s max waits. Issue #65: zero out the
	// post-ready buffer (default 1.5s), the post-send verify/retry
	// delays (defaults 0.5s/1.5s), and shrink the prompt-enter
	// delay so this test continues to converge inside its 2 s
	// deadline. The post-send verifier WILL falsely report
	// "unsubmitted" here because the synthetic shell echoes
	// `GOT:<prompt>` to the pane (the verifier can't distinguish
	// "prompt in input box" from "prompt printed by a downstream
	// shell command"); the retry's second Enter sends a literal
	// newline to the now-blocked shell, which is harmless. Buffer
	// + verifier behavior is verified separately in dedicated
	// tests so we trade test fidelity here for runtime.
	t.Setenv("FLEET_INITIAL_PROMPT_STABLE_MS", "150")
	t.Setenv("FLEET_INITIAL_PROMPT_MAX_MS", "3000")
	t.Setenv("FLEET_POST_READY_BUFFER_MS", "0")
	t.Setenv("FLEET_POST_SEND_VERIFY_MS", "0")
	t.Setenv("FLEET_POST_SEND_RETRY_MS", "0")
	t.Setenv("FLEET_PROMPT_ENTER_DELAY_MS", "50")

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
	// Empty prompt should return BEFORE WaitForReadyToPrompt runs, so
	// the post-ready buffer is irrelevant — but we pin it to 0
	// defensively in case the helper call order changes.
	t.Setenv("FLEET_POST_READY_BUFFER_MS", "0")
	t.Setenv("FLEET_POST_SEND_VERIFY_MS", "0")
	t.Setenv("FLEET_POST_SEND_RETRY_MS", "0")

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
//
// Issue #65 update: defaultPromptEnterDelay was bumped from 200ms
// to 1000ms because the original 200ms gap was too short on the
// operator's box (Enter still landed inside Claude's paste-detection
// window and was swallowed). The pinned env override was bumped to
// 1100ms to keep the test deterministic while asserting the new
// minimum.
func TestSendPromptKeys_SeparatesPromptAndEnter(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)

	// Pin the inter-key delay to a value above the new 1000ms
	// default so the test asserts the floor without paying production-
	// scale latency variability. 1100ms gives ~10% headroom over the
	// new default.
	t.Setenv("FLEET_PROMPT_ENTER_DELAY_MS", "1100")
	// Zero verifier delays — this test exercises the prompt+Enter
	// split, not the post-send verifier. The shell-echoes-prompt
	// pattern would falsely trip verification anyway (see notes on
	// TestSendInitialPrompt_TypedAfterPaneStable).
	t.Setenv("FLEET_POST_SEND_VERIFY_MS", "0")
	t.Setenv("FLEET_POST_SEND_RETRY_MS", "0")

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
	// Two send-keys subprocess invocations + the 1100 ms sleep land
	// well above 1000 ms; a regression that drops below the new
	// default (e.g., reverting to 200 ms) returns much faster.
	if elapsed < 1000*time.Millisecond {
		t.Errorf("SendPromptKeys returned in %s; expected ≥1000 ms because the prompt and Enter must be sent as two send-keys calls separated by FLEET_PROMPT_ENTER_DELAY_MS (default 1000ms post-#65) to defeat Claude Code's bracketed-paste detection",
			elapsed)
	}

	// And the prompt still has to actually reach the session — the
	// split must not have broken delivery.
	want := "GOT:echo SPLIT_SEND_DELIVERED"
	deadline := time.Now().Add(3 * time.Second)
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
	// Issue #65: zero out the post-ready buffer (default 1.5s),
	// the post-send verify/retry delays, and shrink the
	// prompt-enter delay so this test stays inside its 2 s
	// deadline. WaitForReadyToPrompt SKIPS the buffer when
	// stability errors out anyway, but pinning is defensive and
	// the verify/retry zeros are mandatory regardless.
	t.Setenv("FLEET_POST_READY_BUFFER_MS", "0")
	t.Setenv("FLEET_POST_SEND_VERIFY_MS", "0")
	t.Setenv("FLEET_POST_SEND_RETRY_MS", "0")
	t.Setenv("FLEET_PROMPT_ENTER_DELAY_MS", "50")

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

// TestSpawn_RuntimeEnvPropagated verifies every var in
// propagatedRuntimeEnv flows into the agent's session when set.
// Codex iter-2 P2 (FLEET_TMUX_SOCKET) and iter-3 P2 (prompt-timing
// + FLEET_HOME by extension) on fix-fleet-guard-self-drains: tmux
// strips non-`-e` vars when the server is already running, so a
// drain kicked from inside the agent pane wouldn't see the
// operator's overrides. Without propagation: custom FLEET_HOME
// splits reads/writes between operator and agent (TUI doesn't see
// the agent), slow-wrapper prompt-timing overrides regress to
// defaults, and custom tmux sockets break auto-drain entirely.
func TestSpawn_RuntimeEnvPropagated(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)

	// Set every propagated var with a recognizable value. requireTmux
	// already set FLEET_TMUX_SOCKET; setupFleetHome set FLEET_HOME.
	// The prompt-timing knobs we set explicitly here.
	t.Setenv("FLEET_INITIAL_PROMPT_STABLE_MS", "777")
	t.Setenv("FLEET_INITIAL_PROMPT_MAX_MS", "8888")
	t.Setenv("FLEET_PROMPT_ENTER_DELAY_MS", "99")
	t.Setenv("FLEET_POST_READY_BUFFER_MS", "1234")
	t.Setenv("FLEET_POST_SEND_VERIFY_MS", "456")
	t.Setenv("FLEET_POST_SEND_RETRY_MS", "789")

	cmd := []string{"sh", "-c",
		"echo FH=$FLEET_HOME TMS=$FLEET_TMUX_SOCKET STABLE=$FLEET_INITIAL_PROMPT_STABLE_MS MAX=$FLEET_INITIAL_PROMPT_MAX_MS DELAY=$FLEET_PROMPT_ENTER_DELAY_MS BUF=$FLEET_POST_READY_BUFFER_MS VER=$FLEET_POST_SEND_VERIFY_MS RTY=$FLEET_POST_SEND_RETRY_MS; cat"}
	rec, err := Spawn(Options{
		TaskID:  "x",
		Project: "y",
		Command: cmd,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(rec.TmuxSession) })

	// Each marker is short enough to fit on a single tmux pane line.
	wants := []string{
		"STABLE=777", "MAX=8888", "DELAY=99",
		"BUF=1234", "VER=456", "RTY=789",
		"TMS=" + os.Getenv("FLEET_TMUX_SOCKET"),
	}
	if got := os.Getenv("FLEET_HOME"); got != "" {
		wants = append(wants, "FH="+got)
	}

	// FLEET_HOME (under TempDir) and FLEET_TMUX_SOCKET paths are long
	// enough that tmux capture-pane wraps them. Normalize whitespace
	// before matching so a path split across lines still validates.
	stripWS := func(s string) string {
		var b strings.Builder
		for _, r := range s {
			if r != ' ' && r != '\n' && r != '\r' && r != '\t' {
				b.WriteRune(r)
			}
		}
		return b.String()
	}
	deadline := time.Now().Add(2 * time.Second)
	var lastOut []byte
	for time.Now().Before(deadline) {
		out, err := exec.Command("tmux", capturePaneArgs(rec.TmuxSession)...).Output()
		if err != nil {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		lastOut = out
		normalized := stripWS(string(out))
		all := true
		for _, w := range wants {
			if !strings.Contains(normalized, stripWS(w)) {
				all = false
				break
			}
		}
		if all {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("expected all of %v in pane within deadline:\n%s", wants, string(lastOut))
}

// TestSpawn_HandoffOverridesFleetEngineEnv regresses codex review
// iter-2 [P1]: on the handoff branch the replacement agent inherits
// OldRecord.Engine (e.g. claude-code), so the FLEET_ENGINE env var
// stamped into the spawned tmux session MUST match the record, not
// the caller's session env. Without the guard a caller running
// `fleet --engine codex handoff <claude-agent>` propagates
// FLEET_ENGINE=codex into a replacement that's actually running
// claude-code; any downstream code keying off FLEET_ENGINE (the
// reviewer-prompt builder + `fleet dispatch` subprocesses) then
// picks the wrong engine.
func TestSpawn_HandoffOverridesFleetEngineEnv(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)

	// Caller's env claims codex. The OldRecord is claude-code. The
	// replacement's env must reflect the record (claude-code), not the
	// caller (codex).
	t.Setenv("FLEET_ENGINE", "codex")

	old := agent.New("aaaa2222")
	old.TaskID = "engine-leak"
	old.Project = "rainier"
	old.Engine = "claude-code"
	old.Role = "executor"
	old.Mode = "execute"

	// Echo FLEET_ENGINE then idle so the pane stays alive while we
	// capture it.
	cmd := []string{"sh", "-c", "echo ENGINE_SEEN=$FLEET_ENGINE; cat"}
	rec, err := Spawn(Options{
		OldRecord: old,
		Command:   cmd,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(rec.TmuxSession) })

	if rec.Engine != "claude-code" {
		t.Fatalf("rec.Engine = %q, want claude-code (OldRecord inheritance)",
			rec.Engine)
	}

	// Poll the pane for ENGINE_SEEN=claude-code. The negative assertion
	// (must NOT contain codex) is the regression bite.
	deadline := time.Now().Add(2 * time.Second)
	var lastOut []byte
	for time.Now().Before(deadline) {
		out, err := exec.Command("tmux", capturePaneArgs(rec.TmuxSession)...).Output()
		if err == nil {
			lastOut = out
			s := string(out)
			if strings.Contains(s, "ENGINE_SEEN=claude-code") {
				// Belt + suspenders: confirm we didn't ALSO leak codex.
				if strings.Contains(s, "ENGINE_SEEN=codex") {
					t.Fatalf("env leaked caller's FLEET_ENGINE=codex into "+
						"claude-code handoff replacement:\n%s", s)
				}
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("expected ENGINE_SEEN=claude-code in pane within deadline:\n%s",
		string(lastOut))
}

// TestSpawn_HandoffLegacyRecordNormalizesFleetEngine regresses codex
// review iter-3 [P2]: when handing off a pre-v0.9 agent record that
// predates the engine field, OldRecord.Engine is empty but agent.New
// stamps DefaultEngine (claude-code) on the new record. Without
// normalizing the env to DefaultEngine, the caller's FLEET_ENGINE
// (e.g. codex from `fleet --engine codex handoff ...`) would leak
// into a replacement whose record actually says claude-code, re-
// introducing the env/record mismatch the iter-2 fix removed.
func TestSpawn_HandoffLegacyRecordNormalizesFleetEngine(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)

	// Caller's session is codex; OldRecord predates the engine field.
	t.Setenv("FLEET_ENGINE", "codex")

	old := agent.New("aaaa3333")
	old.TaskID = "legacy-handoff"
	old.Project = "rainier"
	old.Engine = "" // pre-v0.9 legacy: engine field absent
	old.Role = "executor"
	old.Mode = "execute"

	cmd := []string{"sh", "-c", "echo ENGINE_SEEN=$FLEET_ENGINE; cat"}
	rec, err := Spawn(Options{
		OldRecord: old,
		Command:   cmd,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(rec.TmuxSession) })

	// agent.New defaults Engine to claude-code on the new record;
	// inheritance branch leaves it (OldRecord.Engine == "" so the
	// "if old.Engine != '' { rec.Engine = old.Engine }" guard
	// preserves the default).
	if rec.Engine != agent.DefaultEngine {
		t.Fatalf("rec.Engine = %q, want %q (legacy handoff default)",
			rec.Engine, agent.DefaultEngine)
	}

	deadline := time.Now().Add(2 * time.Second)
	var lastOut []byte
	for time.Now().Before(deadline) {
		out, err := exec.Command("tmux", capturePaneArgs(rec.TmuxSession)...).Output()
		if err == nil {
			lastOut = out
			s := string(out)
			if strings.Contains(s, "ENGINE_SEEN=claude-code") {
				if strings.Contains(s, "ENGINE_SEEN=codex") {
					t.Fatalf("env leaked caller's FLEET_ENGINE=codex into "+
						"legacy-record handoff replacement:\n%s", s)
				}
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("expected ENGINE_SEEN=claude-code in pane (legacy normalization):\n%s",
		string(lastOut))
}

// TestWaitForReadyToPrompt_AppliesPostReadyBuffer pins issue #65
// Fix B: after waitForPaneStable converges, WaitForReadyToPrompt
// MUST sleep an additional FLEET_POST_READY_BUFFER_MS (default 1500
// ms) before returning. The buffer is the only deterministic line
// of defense against Symptom B — pane is stable but Claude isn't
// actually input-ready yet (splash, onboarding, version-update,
// model-selection screens).
//
// We measure the elapsed time of two back-to-back WaitForReadyToPrompt
// calls: one with buffer=0, one with buffer=N. The N-vs-0 difference
// must be ≥ N (the buffer actually fires) and ≤ N+jitter.
func TestWaitForReadyToPrompt_AppliesPostReadyBuffer(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)

	// Tight stability window — synthetic shell goes idle fast.
	t.Setenv("FLEET_INITIAL_PROMPT_STABLE_MS", "100")
	t.Setenv("FLEET_INITIAL_PROMPT_MAX_MS", "3000")

	rec, err := Spawn(Options{
		TaskID:  "post-ready-buf",
		Project: "p",
		// Plain `read line` so the pane settles to idle quickly.
		Command: []string{"sh", "-c",
			"echo READY; read line; sleep 30"},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(rec.TmuxSession) })

	// Wait for the shell to hit `read` so the pane is genuinely
	// stable before either measurement.
	time.Sleep(300 * time.Millisecond)

	// Baseline: buffer=0.
	t.Setenv("FLEET_POST_READY_BUFFER_MS", "0")
	startBase := time.Now()
	if err := WaitForReadyToPrompt(rec.TmuxSession); err != nil {
		t.Fatalf("WaitForReadyToPrompt baseline: %v", err)
	}
	baselineElapsed := time.Since(startBase)

	// With buffer. Use a generous value so jitter in the stability
	// poll's run-to-run variation doesn't swallow the assertion.
	const bufferMS = 1500
	t.Setenv("FLEET_POST_READY_BUFFER_MS",
		strconv.Itoa(bufferMS))
	startWithBuf := time.Now()
	if err := WaitForReadyToPrompt(rec.TmuxSession); err != nil {
		t.Fatalf("WaitForReadyToPrompt with buffer: %v", err)
	}
	withBufElapsed := time.Since(startWithBuf)

	delta := withBufElapsed - baselineElapsed
	// Allow 200ms slack to absorb stability-poll run-to-run jitter
	// (capture-pane subprocess fork timing varies on busy CI runners).
	if delta < (bufferMS-200)*time.Millisecond {
		t.Errorf("post-ready buffer did not fire: baseline=%s with-buffer=%s delta=%s want ≥ %dms",
			baselineElapsed, withBufElapsed, delta, bufferMS-200)
	}
	// Sanity: buffer shouldn't add WAY more than its configured
	// value (allow 1s headroom for capture-pane jitter).
	if delta > (bufferMS+1000)*time.Millisecond {
		t.Errorf("post-ready buffer overshot: delta=%s configured=%dms",
			delta, bufferMS)
	}
}

// TestWaitForReadyToPrompt_SkipsBufferOnUnstable pins the design
// choice: when waitForPaneStable returns an error (pane never
// converged), WaitForReadyToPrompt SKIPS the buffer. The pane is
// already long-late; adding more delay just makes the failure
// path slower without helping. The error propagates so the caller
// can still log + send-keys-anyway.
func TestWaitForReadyToPrompt_SkipsBufferOnUnstable(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)

	// Pin tiny windows so the unstable pane never converges and
	// WaitForReadyToPrompt errors out fast.
	t.Setenv("FLEET_INITIAL_PROMPT_STABLE_MS", "200")
	t.Setenv("FLEET_INITIAL_PROMPT_MAX_MS", "300")
	// Big buffer — if the skip-on-error path is broken, the test
	// will time out paying this delay.
	t.Setenv("FLEET_POST_READY_BUFFER_MS", "5000")

	rec, err := Spawn(Options{
		TaskID:  "never-stable-buf",
		Project: "p",
		// Constantly-printing shell so stability never converges.
		Command: []string{"sh", "-c",
			"while true; do echo TICK; sleep 0.05; done"},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(rec.TmuxSession) })

	start := time.Now()
	err = WaitForReadyToPrompt(rec.TmuxSession)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected WaitForReadyToPrompt to error on unstable pane")
	}
	// The 5s buffer must NOT have fired. Allow 2s of slack for the
	// stability poll itself (pinned at 300ms max) + scheduling jitter.
	if elapsed > 2*time.Second {
		t.Errorf("WaitForReadyToPrompt slept the buffer despite stability error: elapsed=%s; expected fast-fail without buffer",
			elapsed)
	}
}

// TestWaitForPaneStable_DeadlineBoundedOnUnstable pins the deadline
// contract for the unstable-pane error path deterministically. The
// pre-fix code checked the deadline AFTER capture but BEFORE a full
// poll-interval sleep, so the last iteration could pay one extra
// poll-interval of wall-time past maxWait — which, under CI
// scheduler jitter, periodically tripped the integration test's
// slack. The fix: check at the loop HEADER and cap the trailing
// sleep at remaining-to-deadline.
//
// We exercise waitForPaneStableWithDeps directly with a fake clock
// and an always-changing capture (so stability never converges and
// the function MUST exit via the deadline path). Total simulated
// elapsed time must equal exactly maxWait (no overshoot) because
// the fake "sleep" advances the clock by exactly the requested
// duration with no jitter.
func TestWaitForPaneStable_DeadlineBoundedOnUnstable(t *testing.T) {
	const (
		stableWindow = 200 * time.Millisecond
		maxWait      = 300 * time.Millisecond
	)
	// Fake clock advanced by sleepFn. Captures count up so the
	// always-changing-capture path is exercised (stableSince resets
	// every iteration).
	clock := time.Unix(0, 0)
	nowFn := func() time.Time { return clock }
	sleepFn := func(d time.Duration) { clock = clock.Add(d) }

	captureCount := 0
	captureFn := func() ([]byte, error) {
		captureCount++
		return []byte(fmt.Sprintf("frame-%d", captureCount)), nil
	}

	start := clock
	err := waitForPaneStableWithDeps(stableWindow, maxWait,
		captureFn, sleepFn, nowFn)
	elapsed := clock.Sub(start)

	if err == nil {
		t.Fatal("expected deadline error on always-changing pane; got nil")
	}
	if elapsed > maxWait {
		t.Errorf("waitForPaneStable overshot deadline: elapsed=%s maxWait=%s — deadline check must run at loop header AND cap the trailing sleep",
			elapsed, maxWait)
	}
	// Sanity: we must have polled enough that the deadline path is
	// genuinely the exit (not a no-op).
	if captureCount < 2 {
		t.Errorf("captureCount=%d; want ≥ 2 (function exited before polling meaningfully)",
			captureCount)
	}
}

// TestWaitForPaneStable_ConvergesOnStable pins the happy path of the
// extracted core: when capture returns the same bytes across two
// polls separated by stableWindow, the function returns nil before
// maxWait. Uses the same fake-clock seam as the deadline test so
// the assertion is deterministic.
func TestWaitForPaneStable_ConvergesOnStable(t *testing.T) {
	const (
		stableWindow = 200 * time.Millisecond
		maxWait      = 2 * time.Second
	)
	clock := time.Unix(0, 0)
	nowFn := func() time.Time { return clock }
	sleepFn := func(d time.Duration) { clock = clock.Add(d) }

	// Capture returns the SAME bytes every call → stable from poll 2
	// onward; stableSince first set on poll 2, function returns on
	// the iteration where now()-stableSince >= stableWindow.
	captureFn := func() ([]byte, error) {
		return []byte("steady"), nil
	}

	start := clock
	if err := waitForPaneStableWithDeps(stableWindow, maxWait,
		captureFn, sleepFn, nowFn); err != nil {
		t.Fatalf("waitForPaneStable returned error on steady pane: %v", err)
	}
	elapsed := clock.Sub(start)
	// Must converge within the stability window plus a couple of
	// poll-intervals (first-pass primes prev; second pass sets
	// stableSince; subsequent passes accumulate stable time).
	if elapsed >= maxWait {
		t.Errorf("waitForPaneStable did not converge before maxWait: elapsed=%s",
			elapsed)
	}
}

// TestSendPromptKeys_VerifiesSubmittedHappyPath pins issue #65 Fix
// C: when the prompt is no longer in the pane's bottom band after
// Enter, the verifier reports "submitted" and does NOT send a retry
// Enter.
//
// We exercise the testable core (verifyAndRetryWithDeps) directly
// with stub send-keys + capture-pane functions so the assertion is
// deterministic and doesn't depend on tmux behavior.
func TestSendPromptKeys_VerifiesSubmittedHappyPath(t *testing.T) {
	t.Setenv("FLEET_POST_SEND_VERIFY_MS", "0")
	t.Setenv("FLEET_POST_SEND_RETRY_MS", "0")

	const prompt = "Run the /coordinator skill loop for project demo."

	// Capture stub: returns a pane with the prompt visible only in
	// the SCROLLBACK (top), simulating "Claude submitted: prompt is
	// in the transcript but the input box is now empty".
	scrollbackOnly := []byte(
		"> " + prompt + "\n" + // submitted echo, top of pane
			strings.Repeat("padding line\n", 30) + // 30 lines of fluff
			"╭───────────╮\n" +
			"│ > _       │\n" + // empty input box
			"╰───────────╯\n")

	var sendKeysCalls int
	stubSendKeys := func(session string, keys ...string) error {
		sendKeysCalls++
		return nil
	}
	stubCapture := func(session string) ([]byte, error) {
		return scrollbackOnly, nil
	}

	var warn bytes.Buffer
	submitted := verifyAndRetryWithDeps("fleet-x", prompt,
		stubSendKeys, stubCapture, &warn)
	if !submitted {
		t.Errorf("submitted = false; want true (prompt is only in scrollback, not input box)")
	}
	if sendKeysCalls != 0 {
		t.Errorf("sendKeysCalls = %d; want 0 (no retry Enter on happy path)",
			sendKeysCalls)
	}
	if warn.Len() > 0 {
		t.Errorf("unexpected warning on happy path: %q", warn.String())
	}
}

// TestSendPromptKeys_RetriesOnUnsubmitted pins issue #65 Fix C: when
// the prompt is still visible in the pane's bottom band after the
// initial Enter, the verifier sends ONE additional Enter. If the
// prompt clears on the second capture, the function reports
// "submitted" and does NOT warn.
func TestSendPromptKeys_RetriesOnUnsubmitted(t *testing.T) {
	t.Setenv("FLEET_POST_SEND_VERIFY_MS", "0")
	t.Setenv("FLEET_POST_SEND_RETRY_MS", "0")

	const prompt = "Run the /coordinator skill loop for project demo."

	// First capture: prompt is sitting in the input box (bottom).
	stillInBox := []byte(
		"some scrollback\n" +
			"╭───────────╮\n" +
			"│ > " + prompt + " │\n" + // bottom: input box has prompt
			"╰───────────╯\n")
	// Second capture (after retry Enter): input box cleared. The
	// prompt appears as a submitted echo at the TOP of the pane,
	// followed by enough filler that the bottom unsubmittedTailLines
	// window contains only the empty input box. Mirrors how Claude
	// Code actually renders post-submit: user message in scrollback
	// + cleared input field at the bottom.
	cleared := []byte(
		"> " + prompt + "\n" + // submitted echo (top of pane)
			strings.Repeat("filler\n", 30) + // push prompt out of tail band
			"╭───────────╮\n" +
			"│ > _       │\n" +
			"╰───────────╯\n")

	var captureCount int
	stubCapture := func(session string) ([]byte, error) {
		captureCount++
		if captureCount == 1 {
			return stillInBox, nil
		}
		return cleared, nil
	}

	var sendKeysCalls int
	var lastKeys []string
	stubSendKeys := func(session string, keys ...string) error {
		sendKeysCalls++
		lastKeys = keys
		return nil
	}

	var warn bytes.Buffer
	submitted := verifyAndRetryWithDeps("fleet-x", prompt,
		stubSendKeys, stubCapture, &warn)
	if !submitted {
		t.Errorf("submitted = false; want true (retry Enter cleared the input box)")
	}
	if sendKeysCalls != 1 {
		t.Errorf("sendKeysCalls = %d; want 1 (one retry Enter)", sendKeysCalls)
	}
	if len(lastKeys) != 1 || lastKeys[0] != "Enter" {
		t.Errorf("retry sent %v; want [Enter]", lastKeys)
	}
	if captureCount != 2 {
		t.Errorf("captureCount = %d; want 2 (initial + post-retry)", captureCount)
	}
	if warn.Len() > 0 {
		t.Errorf("unexpected warning on retry-success path: %q", warn.String())
	}
}

// TestSendPromptKeys_StillUnsubmittedAfterRetry_Warns pins issue
// #65 Fix C tail: if the retry Enter ALSO fails to clear the prompt
// from the input box, the verifier reports "unsubmitted" and writes
// a warning. The dispatch CLI uses this signal to surface a stronger
// "attach and press Enter manually" message (Fix D).
func TestSendPromptKeys_StillUnsubmittedAfterRetry_Warns(t *testing.T) {
	t.Setenv("FLEET_POST_SEND_VERIFY_MS", "0")
	t.Setenv("FLEET_POST_SEND_RETRY_MS", "0")

	const prompt = "Run the /coordinator skill loop for project demo."
	stillInBox := []byte(
		"some scrollback\n" +
			"╭───────────╮\n" +
			"│ > " + prompt + " │\n" +
			"╰───────────╯\n")

	stubCapture := func(session string) ([]byte, error) {
		return stillInBox, nil
	}
	stubSendKeys := func(session string, keys ...string) error {
		return nil
	}

	var warn bytes.Buffer
	submitted := verifyAndRetryWithDeps("fleet-x", prompt,
		stubSendKeys, stubCapture, &warn)
	if submitted {
		t.Errorf("submitted = true; want false (prompt still in input box after retry)")
	}
	if !strings.Contains(warn.String(), "unsubmitted after retry") {
		t.Errorf("warning did not mention unsubmitted after retry; got %q",
			warn.String())
	}
	if !strings.Contains(warn.String(), "fleet-x") {
		t.Errorf("warning did not include session name; got %q", warn.String())
	}
	if !strings.Contains(warn.String(), "manually") {
		t.Errorf("warning did not include the operator-facing recovery hint; got %q",
			warn.String())
	}
}

// TestSendPromptKeys_RetrySendKeysFailureWarns pins the edge case
// where the retry Enter itself fails to reach tmux (session
// disappeared, transient error). The verifier must still report
// "unsubmitted" and write a distinguishable warning so operator
// log analysis can tell "Enter swallowed by paste detection" from
// "tmux died mid-retry".
func TestSendPromptKeys_RetrySendKeysFailureWarns(t *testing.T) {
	t.Setenv("FLEET_POST_SEND_VERIFY_MS", "0")
	t.Setenv("FLEET_POST_SEND_RETRY_MS", "0")

	const prompt = "ping"
	// Bottom of pane: prompt still in input box → triggers retry.
	stillInBox := []byte("padding\n>" + prompt + "\n")

	stubCapture := func(session string) ([]byte, error) {
		return stillInBox, nil
	}
	stubSendKeys := func(session string, keys ...string) error {
		return fmt.Errorf("session vanished")
	}

	var warn bytes.Buffer
	submitted := verifyAndRetryWithDeps("fleet-x", prompt,
		stubSendKeys, stubCapture, &warn)
	if submitted {
		t.Error("submitted = true; want false (retry send-keys errored)")
	}
	if !strings.Contains(warn.String(), "retry Enter failed") {
		t.Errorf("warning did not mention retry Enter failed; got %q",
			warn.String())
	}
}

// TestPromptSubmitted_BottomBandHeuristic pins the design rationale
// for the unsubmitted-tail-lines window: the verifier must NOT trip
// when the prompt appears only in the scrollback / submitted-
// transcript area higher up. Otherwise every Claude Code submission
// (which echoes the prompt as a "user turn" line) would falsely
// trigger a retry and the operator would see two duplicate /run-skill
// invocations.
func TestPromptSubmitted_BottomBandHeuristic(t *testing.T) {
	const prompt = "Run the /coordinator skill loop"

	// Scenario A: prompt appears at the top of pane (scrollback /
	// transcript echo). Bottom band has only the empty input box.
	// Expected: promptSubmittedWithDeps=true (NOT in tail band).
	scrollbackEcho := bytes.Repeat([]byte("filler\n"), 20)
	scrollbackEcho = append([]byte("> "+prompt+"\n"), scrollbackEcho...)
	scrollbackEcho = append(scrollbackEcho, []byte(
		"╭───╮\n│ > │\n╰───╯\n")...)

	if !promptSubmittedWithDeps("x", prompt,
		func(string) ([]byte, error) { return scrollbackEcho, nil }) {
		t.Errorf("scrollback-only prompt should report submitted=true (input box is clear)")
	}

	// Scenario B: prompt sits in the input box at the bottom of the
	// pane. Expected: promptSubmittedWithDeps=false (IS in tail band).
	inBox := append(bytes.Repeat([]byte("filler\n"), 5),
		[]byte("╭───╮\n│ > "+prompt+" │\n╰───╯\n")...)
	if promptSubmittedWithDeps("x", prompt,
		func(string) ([]byte, error) { return inBox, nil }) {
		t.Errorf("prompt in input box should report submitted=false (still in tail band)")
	}

	// Scenario C: capture-pane errors. Verifier conservatively reports
	// "submitted" so a transient tmux glitch doesn't drive a spurious
	// retry.
	capErr := func(string) ([]byte, error) {
		return nil, fmt.Errorf("tmux gone")
	}
	if !promptSubmittedWithDeps("x", prompt, capErr) {
		t.Errorf("capture-pane error should report submitted=true (avoid spurious retry)")
	}
}

// TestTailLines pins tailLines's behavior: returns the last N lines
// (or the whole buffer if fewer than N), and treats n<=0 as "the
// whole buffer". The verifier depends on this to scope its prompt-in-
// input-box check to the bottom of the pane.
func TestTailLines(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		n    int
		want []byte
	}{
		{"empty", []byte(""), 5, []byte("")},
		{"n_zero_returns_all", []byte("a\nb\nc\n"), 0, []byte("a\nb\nc\n")},
		{"n_negative_returns_all", []byte("a\nb\nc\n"), -1, []byte("a\nb\nc\n")},
		{"fewer_lines_than_n", []byte("only\n"), 5, []byte("only\n")},
		{"exact_n_lines", []byte("a\nb\nc\n"), 3, []byte("a\nb\nc\n")},
		{"more_lines_than_n", []byte("a\nb\nc\nd\n"), 2, []byte("c\nd\n")},
		// No trailing newline: "a\nb\nc" has only 2 newlines, so
		// tailLines can't find the (n+1)-th-from-end and returns
		// the whole buffer. Acceptable for the verifier — Claude's
		// real pane captures end with a newline, so this edge
		// case doesn't fire in production.
		{"no_trailing_newline_returns_all", []byte("a\nb\nc"), 2, []byte("a\nb\nc")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tailLines(tc.in, tc.n)
			if !bytes.Equal(got, tc.want) {
				t.Errorf("tailLines(%q, %d) = %q; want %q",
					tc.in, tc.n, got, tc.want)
			}
		})
	}
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

// TestSpawn_ExecCommandRunsButPersistsCleanCommand pins issue #73's
// codex-iter-1 P1 fix: when ExecCommand is non-empty, the tmux
// session runs ExecCommand, but the persisted agent.Record.Command
// keeps the (clean) Command. The dispatch coord-spawn path uses
// this so a future handoff that respawns from oldRec.Command does
// NOT inherit a stale `--remote-control "fleet-<old-id>"` flag —
// the successor gets the original operator-supplied command and
// can opt back into auto-attach explicitly if it's also a coord.
//
// The test exercises BOTH halves: (a) tmux ran ExecCommand (we
// look for a sentinel only that argv prints), and (b) the on-disk
// record's Command field equals the clean Command argv.
func TestSpawn_ExecCommandRunsButPersistsCleanCommand(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)

	cleanCmd := []string{"sh", "-c", "echo CLEAN; cat"}
	execCmd := []string{"sh", "-c", "echo EXEC_VARIANT; cat"}

	rec, err := Spawn(Options{
		TaskID:      "t",
		Project:     "p",
		Command:     cleanCmd,
		ExecCommand: execCmd,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(rec.TmuxSession) })

	// (a) Persisted record carries the CLEAN command.
	if len(rec.Command) != len(cleanCmd) {
		t.Fatalf("rec.Command length: got %d want %d", len(rec.Command), len(cleanCmd))
	}
	for i := range cleanCmd {
		if rec.Command[i] != cleanCmd[i] {
			t.Errorf("rec.Command[%d]: got %q want %q", i, rec.Command[i], cleanCmd[i])
		}
	}
	loaded, err := agent.Load(rec.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for i := range cleanCmd {
		if loaded.Command[i] != cleanCmd[i] {
			t.Errorf("loaded.Command[%d]: got %q want %q (the on-disk record must store the clean form so handoff successors don't inherit per-spawn substitutions)",
				i, loaded.Command[i], cleanCmd[i])
		}
	}

	// (b) tmux actually ran the EXEC variant — poll for its sentinel.
	want := "EXEC_VARIANT"
	deadline := time.Now().Add(2 * time.Second)
	var lastOut []byte
	for time.Now().Before(deadline) {
		out, capErr := exec.Command("tmux", capturePaneArgs(rec.TmuxSession)...).Output()
		if capErr == nil {
			lastOut = out
			if strings.Contains(string(out), want) {
				// And the clean variant's sentinel must NOT have run.
				if strings.Contains(string(out), "CLEAN") {
					t.Errorf("pane shows both CLEAN and EXEC_VARIANT — tmux ran the wrong command:\n%s", string(out))
				}
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("expected %q in pane within deadline:\n%s", want, string(lastOut))
}
