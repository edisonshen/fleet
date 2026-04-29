package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tmux"
)

// TestSmoke_AutoHandoffEndToEnd is the step-11 integration test from
// docs/PLAN-week-4bc.md. It drives the full auto-handoff pipeline
// without an interactive Claude session:
//
//  1. `fleet init` installs the skill to a sandboxed ~/.claude/.
//  2. Plant an agent record + live tmux session (stand-in for what
//     `fleet dispatch` would have done).
//  3. Construct a synthetic Stop hook payload pointing at a fake
//     transcript that puts context_pct over 70% (Red threshold).
//  4. Pipe the payload to the installed main.py via stdin.
//  5. Verify the skill wrote a queue file and a handoff doc.
//  6. Run `fleet drain` (via runDrain).
//  7. Verify the replacement agent exists and the old agent is
//     archived; verify the queue file is gone.
//
// What this catches that unit tests don't:
//   - byte-for-byte mismatch between Python's _render_doc and Go's
//     handoff.Render (both have golden tests, but they could drift
//     individually without paired regression unless the skill output
//     is fed through a real drain).
//   - sys.path / import resolution in main.py when invoked from a
//     real install path (not the in-tree dev location).
//   - Python version compatibility on the operator's machine.
//   - settings.json hook command shape (not exec'd here, but the
//     install path is verified).
//
// Skips if python3 or tmux aren't available.
func TestSmoke_AutoHandoffEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available; skipping smoke test")
	}
	requireTmux(t)

	// Sandboxed homes so the test can't pollute the operator's real
	// ~/.fleet or ~/.claude.
	fleetHome := t.TempDir()
	claudeHome := t.TempDir()
	t.Setenv("FLEET_HOME", fleetHome)
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	// Step 1: install the skill to a sandboxed ~/.claude/.
	var initOut bytes.Buffer
	if err := runInit(&initOut, false, claudeHome); err != nil {
		t.Fatalf("runInit: %v\n%s", err, initOut.String())
	}
	mainPath := filepath.Join(claudeHome, "skills", "fleet-guard", "main.py")
	if _, err := os.Stat(mainPath); err != nil {
		t.Fatalf("main.py not installed: %v", err)
	}

	// Step 2: plant an agent record + live tmux session.
	old := agent.New(agent.NewID())
	old.TaskID = "smoke-task"
	old.Project = "smoke-proj"
	old.Cwd = t.TempDir()
	old.Command = []string{"sleep", "60"}
	old.TmuxSession = tmux.SessionName(old.ID)
	old.SpawnedAt = time.Now().UTC()
	old.LastActivityTS = old.SpawnedAt
	if err := tmux.Spawn(old.TmuxSession, old.Cwd, old.Command,
		[]string{"FLEET_AGENT_ID=" + old.ID}); err != nil {
		t.Fatalf("plant tmux: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(old.TmuxSession) })
	if err := old.Write(); err != nil {
		t.Fatalf("write old record: %v", err)
	}

	// Step 3: synthetic Stop payload with a transcript that puts
	// context_pct at 75% on claude-sonnet-4-6 (200k limit) → Red.
	transcript := writeFakeTranscript(t, "claude-sonnet-4-6", 150_000)
	payload := map[string]any{
		"hook_event_name": "Stop",
		"transcript_path": transcript,
		"session_id":      "smoke-session",
	}
	payloadBytes, _ := json.Marshal(payload)

	// Step 4: invoke the installed main.py.
	cmd := exec.Command("python3", mainPath)
	cmd.Stdin = bytes.NewReader(payloadBytes)
	cmd.Env = append(os.Environ(),
		"FLEET_HOME="+fleetHome,
		"FLEET_AGENT_ID="+old.ID,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("main.py exec: %v\nstdout=%s\nstderr=%s",
			err, stdout.String(), stderr.String())
	}
	if stderr.Len() > 0 {
		t.Logf("main.py stderr (informational): %s", stderr.String())
	}

	// Step 5: verify the skill produced a queue + doc.
	queueFiles, err := filepath.Glob(filepath.Join(fleetHome, "queue",
		"spawn-fresh-"+old.ID+".json"))
	if err != nil || len(queueFiles) != 1 {
		t.Fatalf("expected 1 queue file, got %d (err=%v)", len(queueFiles), err)
	}
	docFiles, err := filepath.Glob(filepath.Join(fleetHome, "handoffs",
		old.ID+"-*.md"))
	if err != nil || len(docFiles) != 1 {
		t.Fatalf("expected 1 handoff doc, got %d (err=%v)", len(docFiles), err)
	}
	doc, err := os.ReadFile(docFiles[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(doc), `handoff_type: "auto-red"`) {
		t.Errorf("expected auto-red handoff_type in doc:\n%s", string(doc))
	}
	if !strings.Contains(string(doc), `agent_id: "`+old.ID+`"`) {
		t.Errorf("doc missing agent_id:\n%s", string(doc))
	}

	// Step 6: drain the queue.
	var drainOut, drainErr bytes.Buffer
	if err := runDrain(&drainOut, &drainErr, 0); err != nil {
		t.Fatalf("runDrain: %v\nstdout=%s\nstderr=%s",
			err, drainOut.String(), drainErr.String())
	}

	// Step 7: verify replacement spawned, old archived, queue empty.
	if _, err := agent.Load(old.ID); err == nil {
		t.Errorf("old agent %s not archived", old.ID)
	}
	queueFiles, _ = filepath.Glob(filepath.Join(fleetHome, "queue",
		"spawn-fresh-*.json"))
	if len(queueFiles) != 0 {
		t.Errorf("expected empty queue, found: %v", queueFiles)
	}
	live, err := agent.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 {
		t.Fatalf("expected 1 live agent (the replacement), got %d", len(live))
	}
	newRec := live[0]
	if newRec.ID == old.ID {
		t.Error("replacement has same ID as old agent")
	}
	if newRec.TaskID != old.TaskID || newRec.Project != old.Project {
		t.Errorf("replacement task/project mismatch: got %s/%s want %s/%s",
			newRec.TaskID, newRec.Project, old.TaskID, old.Project)
	}
	if newRec.HandoffNumber != old.HandoffNumber+1 {
		t.Errorf("handoff_number not incremented: got %d want %d",
			newRec.HandoffNumber, old.HandoffNumber+1)
	}
	if newRec.LastHandoffPath == nil ||
		!strings.Contains(*newRec.LastHandoffPath, old.ID+"-") {
		t.Errorf("replacement's last_handoff_path doesn't point at the auto-handoff doc: %v",
			newRec.LastHandoffPath)
	}
	t.Cleanup(func() { _ = tmux.Kill(newRec.TmuxSession) })
}

// writeFakeTranscript creates a JSONL file with one assistant turn
// whose usage block puts context_pct at the desired level when run
// through health.read_context_pct.
func writeFakeTranscript(t *testing.T, model string, inputTokens int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	line := map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"model": model,
			"usage": map[string]any{
				"input_tokens": inputTokens,
			},
		},
	}
	data, _ := json.Marshal(line)
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
