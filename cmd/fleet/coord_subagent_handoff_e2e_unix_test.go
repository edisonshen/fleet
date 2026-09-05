//go:build linux || darwin

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/coordlock"
	"github.com/edisonshen/fleet/internal/handoff"
	"github.com/edisonshen/fleet/internal/queue"
	"github.com/edisonshen/fleet/internal/tasks"
	"github.com/edisonshen/fleet/internal/tmux"
)

// TestE2E_CoordAutoHandoffWhileSubagentWorking replays the operator-reported
// lifecycle end to end with the real coord skill, the real fleet-guard hook
// and the built `fleet` binary — nothing about the coord or worker state is
// hand-seeded:
//
//  1. a fresh coord is planted (record + live tmux pane + coord lease),
//  2. the coord's own tick dispatches a subagent for a task with a
//     predefined spec (inbox prompt, tasks.md flip, coord-state.json),
//  3. the subagent reports progress via `fleet workers update`, the coord
//     records what it is waiting on via `fleet checkpoint next-step`, and
//     its pane shows it blocked on the worker's browser run,
//  4. while the subagent is still mid-task, fleet-guard's Stop hook fires
//     at Red and auto-hands the coord off,
//  5. the successor doc must carry the coord's status (decision + next
//     step + pane) and the subagent's identity, work and phase/status, and
//     the queue entry must point at that doc.
func TestE2E_CoordAutoHandoffWhileSubagentWorking(t *testing.T) {
	const (
		project  = "e2e-proj"
		slug     = "login-e2e-0001"
		workSpec = "Browser e2e: sign in as the demo user and assert the dashboard renders."
		nextStep = "merge login-e2e-0001 once its browser run is green"
		paneLine = "coord: waiting for login-e2e-0001 browser e2e run"
	)

	env := setupCoordIntegration(t, project)
	env.bindRepo(t)
	coordID := plantLiveCoord(t, env, paneLine)

	// A live coord holds the project lease via `fleet coord-run`; the test
	// process stands in for that supervisor so both the tick's lease-check
	// and handoff-write's pre-publish fence see a lease held by an ancestor.
	lease, acquired, err := coordlock.AcquireLease(project, coordID)
	if err != nil || !acquired {
		t.Fatalf("acquire coord lease: acquired=%v err=%v", acquired, err)
	}
	defer lease.Release()

	// Step 2: the coord dispatches the subagent.
	env.addReadyTask(t, slug, workSpec)
	tickOut := env.runTick(t)
	assertNoTickErrors(t, tickOut)
	if !strings.Contains(tickOut, `"dispatched": 1`) {
		t.Fatalf("coord tick did not dispatch %s: %s", slug, tickOut)
	}
	workerID := readWorkerAgentID(t, env.fleetHome, project, slug)
	inboxPath := filepath.Join(env.fleetHome, "inbox", workerID+".md")
	inbox, err := os.ReadFile(inboxPath)
	if err != nil {
		t.Fatalf("worker inbox prompt: %v", err)
	}
	if !strings.Contains(string(inbox), workSpec) {
		t.Fatalf("worker prompt %s lacks the predefined work text:\n%s", inboxPath, inbox)
	}
	task := env.readTask(t, slug)
	if task.Status != tasks.StatusInProgress {
		t.Fatalf("task status after dispatch = %q, want in-progress", task.Status)
	}
	gen := strconv.Itoa(task.DispatchGeneration)

	// Step 3: the subagent works; the coord waits on it.
	env.runFleet(t, "workers", "update", slug, "--project", project,
		"--phase", "tdd-red", "--dispatch-generation", gen)
	env.runFleet(t, "workers", "update", slug, "--project", project,
		"--phase", "tdd-green", "--dispatch-generation", gen)
	env.runFleetAs(t, coordID, "checkpoint", "next-step", "--project", project,
		"--slug", slug, nextStep)

	// Step 4: Red on the coord while the subagent is still in-progress.
	docPath := fireStopHookRed(t, env, coordID)

	// Step 5: the successor doc.
	raw, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	for _, want := range []string{
		`agent_id: "` + coordID + `"`,
		`task_id: "coord-` + project + `"`,
		`handoff_type: "auto-red"`,
		`context_pct_at_handoff: 75`,
		"- dispatched worker " + slug,
		"- [explicit] " + nextStep,
		paneLine,
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("handoff doc missing %q:\n%s", want, doc)
		}
	}
	for _, section := range []string{"## Key Decisions", "## Next Steps (prioritized)"} {
		if strings.Contains(sectionBody(doc, section), "fill in before resuming") {
			t.Errorf("%s left as placeholder:\n%s", section, doc)
		}
	}

	subs, warnings, err := handoff.ParseActiveSubagents(raw)
	if err != nil {
		t.Fatalf("ParseActiveSubagents: %v", err)
	}
	if len(warnings) > 0 {
		t.Errorf("ParseActiveSubagents warnings: %v", warnings)
	}
	if len(subs) != 1 {
		t.Fatalf("Active Subagents = %+v, want exactly the dispatched worker", subs)
	}
	got := subs[0]
	want := handoff.ActiveSubagent{
		TaskID:    slug,
		Branch:    "worker/" + slug,
		LastPhase: "tdd-green",
		Status:    string(tasks.StatusInProgress),
		AgentID:   workerID,
	}
	if got != want {
		t.Errorf("Active Subagents row:\n got %+v\nwant %+v", got, want)
	}

	// The worker's prompt the row points at (via agent_id) still carries the
	// predefined work, and the task is still the worker's — the handoff took
	// nothing away from the in-flight subagent.
	if after, err := os.ReadFile(filepath.Join(env.fleetHome, "inbox", got.AgentID+".md")); err != nil ||
		!strings.Contains(string(after), workSpec) {
		t.Errorf("worker prompt for agent_id %s no longer resolvable to the work text (err=%v)", got.AgentID, err)
	}
	if st := env.readTask(t, slug).Status; st != tasks.StatusInProgress {
		t.Errorf("task status after handoff = %q, want in-progress", st)
	}

	queuePath := filepath.Join(env.fleetHome, "queue", "spawn-fresh-"+coordID+".json")
	sf, err := queue.ReadSpawnFresh(queuePath)
	if err != nil {
		t.Fatalf("queue entry: %v", err)
	}
	if sf.HandoffDoc != docPath || sf.OldAgentID != coordID || sf.TaskID != "coord-"+project {
		t.Errorf("queue entry %+v does not reference doc %s for coord %s", sf, docPath, coordID)
	}
}

// plantLiveCoord seeds the coord record + a live tmux pane whose content
// fleet-guard captures into the handoff doc. Unlike plantCoord the task_id
// is the exact coord identity (coord-<project>) so handoff-write enriches.
func plantLiveCoord(t *testing.T, env *integrationEnv, paneLine string) string {
	t.Helper()
	id := agent.NewID()
	rec := agent.New(id)
	rec.TaskID = "coord-" + env.project
	rec.Project = env.project
	rec.Cwd = env.repoCwd
	rec.Command = []string{"sh", "-c", "echo '" + paneLine + "'; sleep 300"}
	rec.TmuxSession = tmux.SessionName(id)
	rec.SpawnedAt = time.Now().UTC()
	rec.LastActivityTS = rec.SpawnedAt
	if err := tmux.Spawn(rec.TmuxSession, rec.Cwd, rec.Command,
		[]string{"FLEET_AGENT_ID=" + id}); err != nil {
		t.Fatalf("plant tmux for coord: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(rec.TmuxSession) })
	if err := rec.Write(); err != nil {
		t.Fatalf("write coord record: %v", err)
	}
	env.coordID = id
	return id
}

// runFleetAs is runFleet with FLEET_AGENT_ID set, for commands that scope
// their writes to the calling coord (checkpoint next-step / decision).
func (env *integrationEnv) runFleetAs(t *testing.T, agentID string, args ...string) string {
	t.Helper()
	cmd := exec.Command(filepath.Join(env.binDir, "fleet"), args...)
	cmd.Env = append(os.Environ(),
		"FLEET_HOME="+env.fleetHome,
		"HOME="+env.homeDir,
		"FLEET_AGENT_ID="+agentID,
		"PATH="+env.binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	cmd.Dir = env.repoCwd
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("fleet %s: %v\nstdout=%s\nstderr=%s",
			strings.Join(args, " "), err, out.String(), errb.String())
	}
	return out.String()
}

// readWorkerAgentID returns coord-state.json's worker_agent_ids[slug] —
// the durable record the coord's dispatch left of which agent owns slug.
func readWorkerAgentID(t *testing.T, fleetHome, project, slug string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fleetHome, "projects", project, "coord-state.json"))
	if err != nil {
		t.Fatalf("coord-state.json: %v", err)
	}
	var st struct {
		WorkerAgentIDs map[string]string `json:"worker_agent_ids"`
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("coord-state.json: %v\n%s", err, raw)
	}
	id, ok := st.WorkerAgentIDs[slug]
	if !ok || !regexp.MustCompile(`^[0-9a-f]{8}$`).MatchString(id) {
		t.Fatalf("coord-state.json has no worker agent id for %s:\n%s", slug, raw)
	}
	return id
}

// fireStopHookRed runs the installed fleet-guard Stop hook for the coord
// with a transcript at 75% context, exactly as Claude Code would: same
// stdin payload, spawn-stamped FLEET_BIN, and TMUX pointing at the pane's
// server so `tmux capture-pane` reaches the isolated per-test socket.
// Returns the handoff doc path fleet-guard wrote.
func fireStopHookRed(t *testing.T, env *integrationEnv, coordID string) string {
	t.Helper()
	transcript := writeFakeTranscript(t, "claude-sonnet-4-6", 150_000)
	payload, _ := json.Marshal(map[string]any{
		"hook_event_name": "Stop",
		"transcript_path": transcript,
		"session_id":      "e2e-session",
	})
	// Pre-throttle the detached `fleet drain` kick: this test asserts on
	// the doc + queue, not on the successor spawn.
	kicked := filepath.Join(env.fleetHome, "queue", "spawn-fresh-"+coordID+".json.kicked")
	if err := os.WriteFile(kicked, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", filepath.Join(env.claudeHome, "skills", "fleet-guard", "main.py"))
	cmd.Stdin = bytes.NewReader(payload)
	cmd.Env = append(os.Environ(),
		"FLEET_HOME="+env.fleetHome,
		"HOME="+env.homeDir,
		"FLEET_AGENT_ID="+coordID,
		"FLEET_BIN="+filepath.Join(env.binDir, "fleet"),
		"PATH="+env.binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"TMUX="+os.Getenv("FLEET_TMUX_SOCKET")+",0,0",
	)
	cmd.Dir = env.repoCwd
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("fleet-guard main.py: %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	if stderr.Len() > 0 {
		t.Logf("main.py stderr (informational): %s", stderr.String())
	}
	docs, err := filepath.Glob(filepath.Join(env.fleetHome, "handoffs", coordID+"-*.md"))
	if err != nil || len(docs) != 1 {
		t.Fatalf("expected 1 handoff doc for %s, got %v (err=%v)", coordID, docs, err)
	}
	return docs[0]
}

// sectionBody returns the text between `header` and the next `## ` heading.
func sectionBody(doc, header string) string {
	_, rest, ok := strings.Cut(doc, header+"\n")
	if !ok {
		return ""
	}
	if i := strings.Index(rest, "\n## "); i >= 0 {
		rest = rest[:i]
	}
	return rest
}
