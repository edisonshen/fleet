package main

// End-to-end integration tests for the v0.2 coordinator skill. These
// tests build the real `fleet` binary, drop it on PATH, install the
// embedded coordinator skill into a sandboxed ~/.claude/, and drive
// `loop.tick()` against a sandboxed ~/.fleet/. Stubbed `gh` covers the
// CI-check path so no network access is needed.
//
// Coverage matrix (PR 13 from docs/ENG-v0.2-coordinator.md §11):
//
//   - happy path: tasks add → tick (dispatch) → inbox sentinel →
//     tick (drain) → status flips to in-review with pr_url set.
//   - C1: TestCoordHandoffPreservesInflight — coord-A holds an
//     in-progress task; coord-B's first tick must NOT clobber it.
//   - C2: TestParallelWorkerStatusIsolation — two worker reports
//     for two different slugs land in inbox/archive/, one drain
//     pass mutates each task with the correct PR URL.
//   - standards seed: fresh `fleet init` produces a parseable
//     ~/.fleet/standards.md and `fleet standards show` succeeds.
//   - skill install: ~/.claude/skills/coordinator/SKILL.md exists.

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tasks"
	"github.com/edisonshen/fleet/internal/tmux"
)

// fleetBinaryPath is built once per `go test` invocation by
// buildFleetBinary; tests share the same binary because builds are
// expensive (~5s) and the binary is read-only.
var fleetBinaryPath string

// buildFleetBinary compiles cmd/fleet into a tempdir and returns the
// absolute path. Called lazily on first integration test so the test
// suite still runs fast for unrelated `go test ./cmd/fleet/...`
// invocations (Go's test runner caches across invocations but a
// per-binary `go build` is unavoidable). On subsequent calls the
// already-built path is returned.
func buildFleetBinary(t *testing.T) string {
	t.Helper()
	if fleetBinaryPath != "" {
		return fleetBinaryPath
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("`go` not on PATH; skipping integration test")
	}
	dir, err := os.MkdirTemp("", "fleet-int-bin-")
	if err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	bin := filepath.Join(dir, "fleet")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/fleet")
	// The integration test lives in cmd/fleet/, so the module root is
	// two levels up. Resolve it via runtime.Caller so we don't rely on
	// where the test runner was invoked from.
	cmd.Dir = repoRoot(t)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		_ = os.RemoveAll(dir)
		t.Fatalf("go build ./cmd/fleet: %v", err)
	}
	fleetBinaryPath = bin
	return bin
}

// repoRoot walks upward from this test file's location until it finds
// a go.mod. Avoids hardcoding "../.." which breaks if the test moves
// into a subpackage.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("go.mod not found walking up from %s", file)
		}
		dir = parent
	}
}

// integrationEnv holds the per-test sandboxes wired together by setupCoordIntegration.
type integrationEnv struct {
	fleetHome  string // ~/.fleet equivalent
	claudeHome string // ~/.claude equivalent
	repoCwd    string // working directory for dispatched workers
	binDir     string // PATH directory holding fleet + gh stubs
	skillDir   string // ~/.claude/skills/coordinator (after init)
	project    string // resolved project name
	coordID    string // coord agent's 8-hex ID (set by plantCoord)
}

// setupCoordIntegration wires every dependency the coord skill touches:
// real fleet binary on PATH, embedded coordinator skill installed to a
// sandboxed ~/.claude/, ~/.fleet/ initialized via state.Bootstrap, and
// a stub `gh` binary returning canned JSON for the CI-check path.
//
// Sets t.Setenv FLEET_HOME and PATH (PATH = binDir + original) and
// returns the env for follow-up assertions. Skips when tmux isn't
// available — the coord's `fleet dispatch` shells through tmux.Spawn.
func setupCoordIntegration(t *testing.T, project string) *integrationEnv {
	t.Helper()
	requireTmux(t)
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH; skipping integration test")
	}

	bin := buildFleetBinary(t)

	tmp := t.TempDir()
	fleetHome := filepath.Join(tmp, ".fleet")
	claudeHome := filepath.Join(tmp, ".claude")
	repoCwd := filepath.Join(tmp, "repo")
	binDir := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(repoCwd, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Symlink fleet into binDir so PATH lookup works inside the skill.
	// Symlink (not copy) keeps the same inode — `fleet dispatch` uses
	// the binary's actual path for log messages, not its PATH entry.
	dst := filepath.Join(binDir, "fleet")
	if err := os.Symlink(bin, dst); err != nil {
		// Fallback to a copy on platforms that disallow symlinks
		// (some CI Windows agents). Copy fidelity preserves the exec bit.
		data, rerr := os.ReadFile(bin)
		if rerr != nil {
			t.Fatalf("read fleet binary: %v", rerr)
		}
		if werr := os.WriteFile(dst, data, 0o755); werr != nil {
			t.Fatalf("copy fleet binary: %v", werr)
		}
	}

	// Stub `gh` — the coord's reconcile path calls `gh pr checks` and
	// `gh pr view`. Default is "no PR exists" so reconcile leaves
	// tasks as-is; tests that exercise CI-state reconcile override
	// this stub by writing a custom one before driving the tick.
	writeStubGH(t, binDir, ghStubReturnsEmpty)

	// Set FLEET_HOME + PATH first so subsequent commands hit the sandbox.
	t.Setenv("FLEET_HOME", fleetHome)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	// Install fleet-guard + coordinator into the sandboxed ~/.claude/.
	// Using runInit gives us the same code path the operator hits, so
	// any breakage in the install loop fails the integration test.
	var initOut bytes.Buffer
	if err := runInit(&initOut, false, claudeHome); err != nil {
		t.Fatalf("runInit: %v\n%s", err, initOut.String())
	}
	skillDir := filepath.Join(claudeHome, "skills", "coordinator")
	if _, err := os.Stat(filepath.Join(skillDir, "SKILL.md")); err != nil {
		t.Fatalf("coordinator skill not installed: %v", err)
	}

	return &integrationEnv{
		fleetHome:  fleetHome,
		claudeHome: claudeHome,
		repoCwd:    repoCwd,
		binDir:     binDir,
		skillDir:   skillDir,
		project:    project,
	}
}

// runFleet runs `fleet <args...>` against the integration env. Returns
// stdout on success, fails the test with stdout+stderr on non-zero exit.
// Used for both task setup (`tasks add`, `tasks promote`) and assertions
// (`tasks show`, `standards show`).
//
// Caller is on the hook for passing `--project <name>` when invoking
// task / standards / learnings subcommands — the cwd-default resolver
// (tui.ProjectTag) sanitizes the test's tempdir parent-basename to
// something unrelated to env.project, so omitting --project makes
// these helpers silently work against a different project's tasks.md.
func (env *integrationEnv) runFleet(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command(filepath.Join(env.binDir, "fleet"), args...)
	cmd.Env = append(os.Environ(), "FLEET_HOME="+env.fleetHome,
		"PATH="+env.binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
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

// runTick invokes `loop.tick(project, coord_id=..., fleet_home=...,
// fleet_bin=...)` via the installed coordinator skill. Stdout is the
// JSON-encoded TickResult from loop.main.
//
// We don't mock the skill — the integration test exercises the real
// skill's lock + parse + reconcile + drain + dispatch path. Driver
// script is short Python; it imports the installed skill modules and
// calls tick directly so we get structured TickResult assertions.
func (env *integrationEnv) runTick(t *testing.T) string {
	t.Helper()
	driver := fmt.Sprintf(`import json, os, sys
sys.path.insert(0, %q)
import loop
res = loop.tick(%q,
                coord_id=%q,
                cwd=%q,
                fleet_home=%q,
                fleet_bin=%q,
                cap=2)
print(json.dumps({
    "skipped": res.skipped,
    "reason": res.reason,
    "parsed": res.parsed_tasks,
    "reconciled": res.reconciled,
    "drained": res.drained,
    "dispatched": res.dispatched,
    "raised": res.raised,
    "errors": res.errors,
}))
`,
		env.skillDir, env.project, env.coordID,
		env.repoCwd, env.fleetHome, filepath.Join(env.binDir, "fleet"),
	)
	cmd := exec.Command("python3", "-c", driver)
	cmd.Env = append(os.Environ(),
		"FLEET_HOME="+env.fleetHome,
		"FLEET_AGENT_ID="+env.coordID,
		"PATH="+env.binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	cmd.Dir = env.repoCwd
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("loop.tick: %v\nstdout=%s\nstderr=%s",
			err, out.String(), errb.String())
	}
	if errb.Len() > 0 {
		t.Logf("loop.tick stderr (informational): %s", errb.String())
	}
	return out.String()
}

// plantCoord seeds an agent record + live tmux session standing in for
// the coord agent. The skill reads FLEET_AGENT_ID for inbox-archive
// scoping; tmux must be alive so reconcile's `agent.List()` filter
// (used elsewhere) doesn't archive it. Returns the coord ID.
func (env *integrationEnv) plantCoord(t *testing.T) string {
	t.Helper()
	id := agent.NewID()
	rec := agent.New(id)
	rec.TaskID = "coordinator"
	rec.Project = env.project
	rec.Cwd = env.repoCwd
	rec.Command = []string{"sleep", "60"}
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

// addReadyTask shells out to `fleet tasks add` with status=ready so the
// coord dispatches it on next tick. Returns the resolved slug parsed
// off `fleet tasks add` stdout (the CLI prints `added <slug>...`).
//
// Always passes --project explicitly because the test's repoCwd is a
// generic tempdir whose parent-basename sanitizes to something unrelated
// to env.project. Without --project the CLI would silently bind the
// task to the wrong project.
func (env *integrationEnv) addReadyTask(t *testing.T, slug, spec string) string {
	t.Helper()
	out := env.runFleet(t, "tasks", "add",
		"--project", env.project,
		"--slug", slug,
		"--status", "ready",
		"--priority", "P1",
		"--spec", spec,
	)
	// "added <slug> (status=ready priority=P1) to /path"
	first := strings.SplitN(out, "\n", 2)[0]
	parts := strings.Fields(first)
	if len(parts) < 2 || parts[0] != "added" {
		t.Fatalf("unexpected tasks add output: %q", out)
	}
	return parts[1]
}

// readTaskStatus + readTaskPRURL + readTaskWorkerPID — small helpers
// to inspect tasks.md without rebuilding the parser. They use the
// public internal/tasks package (the test is in cmd/fleet/main package
// so it has access).
func (env *integrationEnv) readTask(t *testing.T, slug string) *tasks.Task {
	t.Helper()
	dir, err := state.ProjectDir(env.project)
	if err != nil {
		t.Fatalf("ProjectDir: %v", err)
	}
	f, err := tasks.Read(filepath.Join(dir, "tasks.md"))
	if err != nil {
		t.Fatalf("tasks.Read: %v", err)
	}
	task, err := f.Get(slug)
	if err != nil {
		t.Fatalf("tasks.Get %s: %v", slug, err)
	}
	return task
}

// writeSentinelArchive writes one inbox/archive/<coord_id>-<stamp>.md
// file containing the supplied sentinel line. Mirrors the path
// fleet-guard's inbox.archive_after_deliver produces, so the coord
// drains it on next tick. Returns the absolute file path.
func (env *integrationEnv) writeSentinelArchive(t *testing.T, sentinel string) string {
	t.Helper()
	if env.coordID == "" {
		t.Fatal("plantCoord must run before writeSentinelArchive")
	}
	archiveDir := filepath.Join(env.fleetHome, "inbox", "archive")
	if err := os.MkdirAll(archiveDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Filenames sort lex; the loop drains files newer than
	// last_archive_scan_ts. Stamp at micro-second precision plus a
	// small random suffix to keep call ordering stable when two
	// archives are written in the same RFC3339 second.
	var rnd [2]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("%s-%s-%s.md",
		env.coordID,
		time.Now().UTC().Format("20060102-150405.000000"),
		hex.EncodeToString(rnd[:]),
	)
	target := filepath.Join(archiveDir, name)
	if err := os.WriteFile(target, []byte(sentinel+"\n"), 0o644); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}
	return target
}

// writeStubGH installs a tiny shell-script `gh` into binDir whose body
// is supplied by the caller. The body is interpreted verbatim — caller
// is on the hook for handling argv. Default ghStubReturnsEmpty replies
// with an empty array / null so the coord's CI parsing returns no
// state.
func writeStubGH(t *testing.T, binDir string, body string) {
	t.Helper()
	target := filepath.Join(binDir, "gh")
	if err := os.WriteFile(target, []byte(body), 0o755); err != nil {
		t.Fatalf("write gh stub: %v", err)
	}
}

// ghStubReturnsEmpty is the default stub: prints "[]" for any --json
// query, exits 0. Coord interprets empty-checks list as "all green",
// but without merge state the reconcile path still raises CI-green
// to operator. For tests that don't go down the reconcile path this
// is moot — the tests that DO exercise reconcile install a richer
// stub.
const ghStubReturnsEmpty = `#!/bin/sh
# Default stub gh used by the v0.2 coordinator integration tests.
# Echoes empty JSON for every query so the coord's CI-check path is
# a no-op. Specific tests override this via writeStubGH.
case "$*" in
  *"--json"*)
    echo "[]"
    ;;
  *)
    ;;
esac
exit 0
`

// ---------- happy path ----------

// TestCoordIntegration_HappyPath drives:
//
//  1. `fleet tasks add --status=ready` — adds a task ready to dispatch.
//  2. `loop.tick()` — coord dispatches the task; status flips to
//     in-progress.
//  3. Manually plant `worker_pid=<test_pid>` so the next tick's
//     reconcile finds the worker "alive" and skips that path. (Workers
//     don't currently auto-write worker_pid back to tasks.md — that's
//     v0.2.x; for v0.2.0 the test simulates the live worker.)
//  4. Write a TASK_DONE_PR=<slug> <url> sentinel into inbox/archive/.
//  5. `loop.tick()` — coord drains the sentinel; status flips to
//     in-review and pr_url populates.
func TestCoordIntegration_HappyPath(t *testing.T) {
	env := setupCoordIntegration(t, "happy-proj")
	env.plantCoord(t)

	slug := env.addReadyTask(t, "happy-task", "Write a tiny README change for testing.")

	// Tick #1 — dispatches the ready task.
	out1 := env.runTick(t)
	if !strings.Contains(out1, `"dispatched": 1`) {
		t.Fatalf("first tick did not dispatch: %s", out1)
	}
	task := env.readTask(t, slug)
	if task.Status != tasks.StatusInProgress {
		t.Fatalf("after dispatch: status=%q want %q", task.Status, tasks.StatusInProgress)
	}

	// Simulate the worker writing its PID back. Without this, the
	// next tick would reconcile worker_pid=0 → status=todo before the
	// drain step runs.
	env.runFleet(t, "tasks", "set", "--project", env.project, slug, fmt.Sprintf("worker_pid=%d", os.Getpid()))

	// Worker reports DONE_PR via inbox archive.
	prURL := "https://github.com/fake/repo/pull/42"
	env.writeSentinelArchive(t, fmt.Sprintf("TASK_DONE_PR=%s %s", slug, prURL))

	// Tick #2 — drains the sentinel.
	out2 := env.runTick(t)
	if !strings.Contains(out2, `"drained": 1`) {
		t.Fatalf("second tick did not drain sentinel: %s", out2)
	}
	task = env.readTask(t, slug)
	if task.Status != tasks.StatusInReview {
		t.Errorf("after drain: status=%q want %q", task.Status, tasks.StatusInReview)
	}
	if task.PRURL != prURL {
		t.Errorf("after drain: pr_url=%q want %q", task.PRURL, prURL)
	}

	// Cleanup: kill any worker tmux sessions left by the dispatch tick.
	for _, rec := range listAllAgents(t) {
		_ = tmux.Kill(rec.TmuxSession)
	}
}

// listAllAgents returns every agent record on disk (live + archived),
// used by test cleanups to reap tmux sessions across the full set.
func listAllAgents(t *testing.T) []*agent.Record {
	t.Helper()
	live, err := agent.List()
	if err != nil {
		t.Fatalf("agent.List: %v", err)
	}
	return live
}

// ---------- standards seed + skill install ----------

// TestCoordIntegration_StandardsSeedAndSkillInstall is the simplest
// PR 12 contract test in integration form: post-init, the canonical
// standards.md is on disk, parseable by the standards CLI, and the
// coordinator skill landed at the right path.
func TestCoordIntegration_StandardsSeedAndSkillInstall(t *testing.T) {
	env := setupCoordIntegration(t, "seed-proj")

	// `fleet standards show` must succeed against the seeded file.
	out := env.runFleet(t, "standards", "show", "--global")
	if !strings.Contains(out, "# Standards") {
		t.Errorf("standards show --global missing header:\n%s", out)
	}
	if !strings.Contains(out, "## Testing") {
		t.Errorf("standards show --global missing Testing section:\n%s", out)
	}

	// Skill install path:
	skillSKILL := filepath.Join(env.skillDir, "SKILL.md")
	if _, err := os.Stat(skillSKILL); err != nil {
		t.Errorf("coordinator SKILL.md missing: %v", err)
	}
	// At least one .py module so the skill can run.
	for _, want := range []string{"loop.py", "parse.py", "dispatch.py"} {
		if _, err := os.Stat(filepath.Join(env.skillDir, want)); err != nil {
			t.Errorf("coordinator/%s missing: %v", want, err)
		}
	}
}

// ---------- C1: handoff preserves in-flight ----------

// TestCoordHandoffPreservesInflight covers ENG §5.6 invariant C1: a
// new coord taking over (replacing a predecessor that exited) must
// NOT clobber an in-flight task. Simulation:
//
//  1. Plant coord-A with one task at status=in-progress and
//     worker_pid=<test_pid> (alive).
//  2. Switch the env's coord_id to a fresh ID (= coord-B replacement
//     post-handoff). The skill's flock is per-process, so coord-A's
//     lock is already gone when its tick exited.
//  3. Run coord-B's first tick.
//  4. Verify the in-progress task is unchanged: still status=in-progress,
//     same worker_pid, same notes.
func TestCoordHandoffPreservesInflight(t *testing.T) {
	env := setupCoordIntegration(t, "c1-proj")
	env.plantCoord(t)

	// Seed a task in the in-flight shape that survives handoff: status
	// in-progress, worker_pid=test_pid (alive), branch + worktree set.
	slug := env.addReadyTask(t, "inflight-c1", "C1 invariant test seed.")
	env.runFleet(t, "tasks", "set", "--project", env.project, slug, "status=in-progress")
	env.runFleet(t, "tasks", "set", "--project", env.project, slug, fmt.Sprintf("worker_pid=%d", os.Getpid()))
	env.runFleet(t, "tasks", "set", "--project", env.project, slug, "branch=worker/inflight-c1")

	before := env.readTask(t, slug)

	// Spawn coord-B as a separate plant — same project, fresh ID. The
	// previous coord_id stays in env.coordID until we overwrite it;
	// we deliberately leak the predecessor's tmux for the test
	// duration (cleaned up by the original plantCoord's t.Cleanup).
	prevID := env.coordID
	id := agent.NewID()
	rec := agent.New(id)
	rec.TaskID = "coordinator"
	rec.Project = env.project
	rec.Cwd = env.repoCwd
	rec.Command = []string{"sleep", "60"}
	rec.TmuxSession = tmux.SessionName(id)
	rec.SpawnedAt = time.Now().UTC()
	rec.LastActivityTS = rec.SpawnedAt
	if err := tmux.Spawn(rec.TmuxSession, rec.Cwd, rec.Command,
		[]string{"FLEET_AGENT_ID=" + id}); err != nil {
		t.Fatalf("plant tmux for coord-B: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(rec.TmuxSession) })
	if err := rec.Write(); err != nil {
		t.Fatalf("write coord-B record: %v", err)
	}
	env.coordID = id
	t.Logf("handoff: coord %s → %s", prevID, id)

	// First tick of the successor — must see the in-progress task,
	// reconcile finds worker alive, skip dispatch (cap not exceeded
	// but task is in-progress not ready), no drain (no archives).
	out := env.runTick(t)
	if !strings.Contains(out, `"reconciled": 0`) {
		t.Errorf("successor reconciled an alive worker: %s", out)
	}
	if !strings.Contains(out, `"drained": 0`) {
		t.Errorf("successor drained unexpected sentinel: %s", out)
	}

	after := env.readTask(t, slug)
	if after.Status != before.Status {
		t.Errorf("status changed across handoff: was=%q now=%q", before.Status, after.Status)
	}
	if after.WorkerPID != before.WorkerPID {
		t.Errorf("worker_pid changed across handoff: was=%d now=%d",
			before.WorkerPID, after.WorkerPID)
	}
	if after.Branch != before.Branch {
		t.Errorf("branch changed across handoff: was=%q now=%q", before.Branch, after.Branch)
	}
}

// ---------- C2: parallel worker isolation ----------

// TestParallelWorkerStatusIsolation covers ENG §6 invariant C2: two
// workers reporting status for two different slugs land in two
// distinct inbox/archive files; one tick drains both; each task is
// mutated with its own data, no crosstalk.
func TestParallelWorkerStatusIsolation(t *testing.T) {
	env := setupCoordIntegration(t, "c2-proj")
	env.plantCoord(t)

	// Two ready tasks → two dispatches in tick #1 (cap=2 in runTick).
	slugA := env.addReadyTask(t, "alpha-c2", "alpha task spec.")
	slugB := env.addReadyTask(t, "bravo-c2", "bravo task spec.")

	out1 := env.runTick(t)
	if !strings.Contains(out1, `"dispatched": 2`) {
		t.Fatalf("first tick did not dispatch both: %s", out1)
	}

	// Set both tasks' worker_pid alive so reconcile skips.
	env.runFleet(t, "tasks", "set", "--project", env.project, slugA, fmt.Sprintf("worker_pid=%d", os.Getpid()))
	env.runFleet(t, "tasks", "set", "--project", env.project, slugB, fmt.Sprintf("worker_pid=%d", os.Getpid()))

	// Two distinct sentinel archives, one per slug. Crucially, they
	// arrive in the SAME tick — the C2 invariant requires the drain
	// step to mutate each task independently with its own URL.
	urlA := "https://github.com/fake/repo/pull/100"
	urlB := "https://github.com/fake/repo/pull/200"
	env.writeSentinelArchive(t, fmt.Sprintf("TASK_DONE_PR=%s %s", slugA, urlA))
	// Sleep a microsecond so filenames sort deterministically; the
	// archive scan iterates in lex order and a same-µs timestamp
	// would have flaky ordering across runs.
	time.Sleep(2 * time.Millisecond)
	env.writeSentinelArchive(t, fmt.Sprintf("TASK_DONE_PR=%s %s", slugB, urlB))

	out2 := env.runTick(t)
	if !strings.Contains(out2, `"drained": 2`) {
		t.Fatalf("second tick did not drain both sentinels: %s", out2)
	}

	taskA := env.readTask(t, slugA)
	taskB := env.readTask(t, slugB)
	if taskA.PRURL != urlA {
		t.Errorf("taskA pr_url=%q want %q (C2 isolation broken)", taskA.PRURL, urlA)
	}
	if taskB.PRURL != urlB {
		t.Errorf("taskB pr_url=%q want %q (C2 isolation broken)", taskB.PRURL, urlB)
	}
	if taskA.PRURL == urlB || taskB.PRURL == urlA {
		t.Errorf("C2 crosstalk: taskA=%q taskB=%q (URLs swapped)",
			taskA.PRURL, taskB.PRURL)
	}
	// Both must end up in-review.
	if taskA.Status != tasks.StatusInReview || taskB.Status != tasks.StatusInReview {
		t.Errorf("statuses after drain: A=%q B=%q want both in-review",
			taskA.Status, taskB.Status)
	}

	// Cleanup tmux for any dispatched workers.
	for _, rec := range listAllAgents(t) {
		_ = tmux.Kill(rec.TmuxSession)
	}
}
