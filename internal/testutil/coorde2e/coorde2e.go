// Package coorde2e holds the shared fixtures for the dead-coordinator
// recovery end-to-end tests (cmd/fleet + internal/tui integration lanes):
// a fake `claude` shim, a real `fleet dispatch --coord-spawn` runner, and
// the "make this coord a corpse" steps that reproduce an operator's
// days-dead coordinator record.
//
// Everything here drives REAL processes: the built fleet binary, real
// `fleet coord-run` supervisors inside real tmux panes on the per-test
// FLEET_TMUX_SOCKET, real coordinator.flock leases. Callers must isolate
// FLEET_HOME + FLEET_TMUX_SOCKET (tmuxtest.RequireTmux) first and set
// FLEET_STANDBY_TIMEOUT so any standby they leave behind self-reaps.
package coorde2e

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/coordlock"
	"github.com/edisonshen/fleet/internal/projects"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tmux"
)

// Modes for the fake claude shim written by FakeClaude.
const (
	// ModeOK: print a banner, then read stdin forever with terminal echo
	// OFF and acknowledge each line as `fake claude: got prompt (<n> chars)`.
	// Echo off keeps the typed prompt out of the pane's bottom band, so
	// Fleet's post-send verifier sees the prompt as submitted for the same
	// reason it does with the real Claude Code input box (the raw text is
	// gone from the input area) rather than by relying on line wrapping.
	ModeOK = "ok"
	// ModeExit: exit 1 immediately — "claude exited at startup". The
	// dispatch wrapper's non-zero-exit branch ends the pane, so the tmux
	// session and its coord-run supervisor go away with it.
	ModeExit = "exit"
)

// PromptAck is the line the ModeOK shim prints after consuming a prompt.
const PromptAck = "fake claude: got prompt"

// FakeClaude writes an executable `claude` shim into dir and returns the
// path of the mode file that controls it (initially ModeOK). Callers put
// dir first on PATH so `fleet dispatch --engine claude-code` (the exact
// argv the TUI's [a] uses) execs the shim instead of a real claude. The
// shim ignores its arguments (`--dangerously-skip-permissions` etc).
func FakeClaude(t *testing.T, dir string) string {
	t.Helper()
	modeFile := filepath.Join(dir, "claude.mode")
	script := `#!/bin/sh
mode=$(cat "` + modeFile + `" 2>/dev/null || echo ok)
if [ "$mode" = "exit" ]; then
  echo "fake claude: startup failure pid $$"
  exit 1
fi
echo "fake claude: ready pid $$"
echo "> "
stty -echo 2>/dev/null
while IFS= read -r line; do
  echo "` + PromptAck + ` (${#line} chars)"
done
exit 1
`
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatalf("coorde2e: write fake claude: %v", err)
	}
	SetMode(t, modeFile, ModeOK)
	return modeFile
}

// SetMode switches the fake claude shim's behaviour for its NEXT start.
func SetMode(t *testing.T, modeFile, mode string) {
	t.Helper()
	if err := os.WriteFile(modeFile, []byte(mode+"\n"), 0o644); err != nil {
		t.Fatalf("coorde2e: write claude mode: %v", err)
	}
}

// SeedProject registers a non-git project whose repo is a fresh temp dir
// (what `fleet project add` would pin) and initializes its state tree.
// Returns the repo path (the coord's --cwd).
func SeedProject(t *testing.T, project string) string {
	t.Helper()
	repo := t.TempDir()
	if _, err := state.EnsureProjectInitialized(project); err != nil {
		t.Fatalf("coorde2e: init project %s: %v", project, err)
	}
	if err := projects.Write(project, projects.Meta{
		Schema:   projects.SchemaVersion,
		RepoPath: repo,
		AddedAt:  time.Now().UTC(),
		IsGit:    projects.BoolPtr(false),
	}); err != nil {
		t.Fatalf("coorde2e: write meta.json: %v", err)
	}
	return repo
}

// SeedInFlightWorker records a worker the (soon to be dead) coord had in
// flight: coord-state.json's worker_agent_ids entry plus the worker's
// state.json. This is the state a recovery synth handoff must carry to
// the replacement so the worker is not orphaned.
func SeedInFlightWorker(t *testing.T, project, slug, workerID, phase string) {
	t.Helper()
	pdir, err := state.ProjectDir(project)
	if err != nil {
		t.Fatalf("coorde2e: project dir: %v", err)
	}
	cs := map[string]any{"worker_agent_ids": map[string]string{slug: workerID}}
	data, err := json.Marshal(cs)
	if err != nil {
		t.Fatalf("coorde2e: marshal coord-state: %v", err)
	}
	if err := state.WriteAtomic(filepath.Join(pdir, "coord-state.json"), data); err != nil {
		t.Fatalf("coorde2e: write coord-state.json: %v", err)
	}
	wdir := filepath.Join(pdir, "workers", slug)
	if err := os.MkdirAll(wdir, 0o755); err != nil {
		t.Fatalf("coorde2e: mkdir worker dir: %v", err)
	}
	ws := fmt.Sprintf(`{"phase":%q,"pr_url":"https://example.invalid/pr/7"}`, phase)
	if err := os.WriteFile(filepath.Join(wdir, "state.json"), []byte(ws), 0o644); err != nil {
		t.Fatalf("coorde2e: write worker state.json: %v", err)
	}
}

// DispatchResult is one real `fleet dispatch --coord-spawn` run.
type DispatchResult struct {
	Out      string // combined stdout+stderr
	ExitCode int    // 0 on success; 75 is the "a coord is already running" veto
}

// DispatchArgs is the argv the TUI's [a] passes to `fleet dispatch` for a
// project (internal/tui keys.go startCoordSpawn), minus the binary.
func DispatchArgs(project, cwd string) []string {
	return []string{
		"dispatch", "coord-" + project, "--project", project, "--coord-spawn",
		"--prompt", "Run /coordinator now.", "--cwd", cwd, "--engine", "claude-code",
	}
}

// Dispatch runs the real fleet binary's coord-spawn dispatch for project
// and returns its combined output + exit code. The subprocess inherits the
// test's environment (isolated FLEET_HOME / FLEET_TMUX_SOCKET, fake-claude
// PATH, timing pins). Fails the test on anything other than a clean run
// or a non-zero exit.
func Dispatch(t *testing.T, bin, project, cwd string) DispatchResult {
	t.Helper()
	cmd := exec.Command(bin, DispatchArgs(project, cwd)...)
	out, err := cmd.CombinedOutput()
	res := DispatchResult{Out: string(out)}
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("coorde2e: run %s dispatch: %v\n%s", bin, err, out)
		}
		res.ExitCode = ee.ExitCode()
	}
	t.Logf("coorde2e: dispatch %s exit=%d\n%s", project, res.ExitCode, res.Out)
	return res
}

var spawnedLine = regexp.MustCompile(`(?m)^agent ([0-9a-f]{8}) spawned$`)

// SpawnedID returns the agent id named by the LAST `agent <id> spawned`
// line in dispatch output — the same parse the TUI uses to decide which
// session to attach to. Empty when absent.
func SpawnedID(out string) string {
	var id string
	for _, m := range spawnedLine.FindAllStringSubmatch(out, -1) {
		id = m[1]
	}
	return id
}

// WaitFor polls cond every 100ms until it holds or timeout elapses, then
// fails the test with desc. Never a fixed sleep as an assertion.
func WaitFor(t *testing.T, timeout time.Duration, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("coorde2e: timed out after %s waiting for %s", timeout, desc)
}

// PIDAlive reports whether pid exists (signal 0). Zombies count as alive;
// tmux reaps its pane processes promptly so a killed supervisor reads dead.
func PIDAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// WaitLiveCoord waits until the coord record id is fully booted: its
// coord-run supervisor stamped its pid into the record, that supervisor
// owns the project's coordinator.flock, and the tmux session is alive.
// Returns the final record.
func WaitLiveCoord(t *testing.T, project, id string) *agent.Record {
	t.Helper()
	var rec *agent.Record
	WaitFor(t, 20*time.Second, "coord "+id+" to own the lease with a live session", func() bool {
		r, err := agent.Load(id)
		if err != nil || r.SupervisorPID <= 0 {
			return false
		}
		ownerPID, ok := coordlock.CurrentActiveOwnerPID(project)
		if !ok || ownerPID != r.SupervisorPID {
			return false
		}
		if !tmux.HasSession(r.TmuxSession) {
			return false
		}
		rec = r
		return true
	})
	return rec
}

// KillCoordCorpse turns a live lease-wrapped coord into the corpse an
// operator finds after a crash: SIGKILL the coord-run supervisor FIRST (so
// its exit path never archives the record or releases the lease
// gracefully — the kernel drops the flock), then kill the tmux session.
// Polls until the session is gone, the supervisor pid is dead, and the
// lease no longer names a live owner. The agent record stays on disk.
func KillCoordCorpse(t *testing.T, rec *agent.Record) {
	t.Helper()
	if rec.SupervisorPID > 0 {
		_ = syscall.Kill(rec.SupervisorPID, syscall.SIGKILL)
	}
	if rec.TmuxSession != "" {
		_ = tmux.Kill(rec.TmuxSession)
	}
	WaitFor(t, 10*time.Second, "coord "+rec.ID+" to be provably dead", func() bool {
		if rec.TmuxSession != "" && tmux.HasSession(rec.TmuxSession) {
			return false
		}
		if PIDAlive(rec.SupervisorPID) {
			return false
		}
		if pid, ok := coordlock.CurrentActiveOwnerPID(rec.Project); ok && PIDAlive(pid) {
			return false
		}
		return true
	})
	if _, err := agent.Load(rec.ID); err != nil {
		t.Fatalf("coorde2e: corpse record %s must remain on disk, got %v", rec.ID, err)
	}
}

// AgeDeadCoord backdates every freshness signal dispatch consults so the
// corpse reads as "dead for days" rather than "booting right now":
// coord-state.json's mtime (the load-bearing liveness signal), the
// coord-spawn-pending cold-start claim (otherwise the next --coord-spawn is
// vetoed with exit 75 for 5 minutes), and the record's spawned_at.
func AgeDeadCoord(t *testing.T, project, id string) {
	t.Helper()
	old := time.Now().Add(-72 * time.Hour).UTC()
	pdir, err := state.ProjectDir(project)
	if err != nil {
		t.Fatalf("coorde2e: project dir: %v", err)
	}
	cs := filepath.Join(pdir, "coord-state.json")
	if _, err := os.Stat(cs); err == nil {
		if err := os.Chtimes(cs, old, old); err != nil {
			t.Fatalf("coorde2e: backdate coord-state.json: %v", err)
		}
	}
	claim := filepath.Join(pdir, "coord-spawn-pending")
	if data, err := os.ReadFile(claim); err == nil {
		var c map[string]any
		if err := json.Unmarshal(data, &c); err != nil {
			t.Fatalf("coorde2e: parse pending claim: %v", err)
		}
		c["spawned_at"] = old
		out, err := json.Marshal(c)
		if err != nil {
			t.Fatalf("coorde2e: marshal pending claim: %v", err)
		}
		if err := state.WriteAtomic(claim, out); err != nil {
			t.Fatalf("coorde2e: backdate pending claim: %v", err)
		}
	}
	rec, err := agent.Load(id)
	if err != nil {
		t.Fatalf("coorde2e: load record %s: %v", id, err)
	}
	rec.SpawnedAt = old
	if err := rec.Write(); err != nil {
		t.Fatalf("coorde2e: backdate record %s: %v", id, err)
	}
}

// Archived reports whether an archived copy of record id exists. Both
// archive writers are accepted: agent.Record.Archive (agents/archive/<id>.json)
// and coord.Cleanup (agents/archive/<id>-<UTC ts>.json).
func Archived(t *testing.T, id string) bool {
	t.Helper()
	dir, err := state.AgentDir()
	if err != nil {
		t.Fatalf("coorde2e: agent dir: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "archive", id+"*.json"))
	if err != nil {
		t.Fatalf("coorde2e: glob archive: %v", err)
	}
	return len(matches) > 0
}

// FleetSessions lists the fleet-* tmux sessions on the isolated socket.
func FleetSessions(t *testing.T) []string {
	t.Helper()
	all, err := tmux.ListSessions()
	if err != nil {
		// No server on the socket → no sessions.
		return nil
	}
	var out []string
	for _, s := range all {
		if strings.HasPrefix(s, "fleet-") {
			out = append(out, s)
		}
	}
	return out
}

// KillAllCoords is a cleanup: SIGKILL every live supervisor named by an
// unarchived record and kill every fleet-* session, so no coord-run or
// fake claude outlives the test.
func KillAllCoords(t *testing.T) {
	t.Helper()
	recs, err := agent.List()
	if err == nil {
		for _, r := range recs {
			if r.SupervisorPID > 0 {
				_ = syscall.Kill(r.SupervisorPID, syscall.SIGKILL)
			}
		}
	}
	for _, s := range FleetSessions(t) {
		_ = tmux.Kill(s)
	}
}
