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
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tasks"
	"github.com/edisonshen/fleet/internal/tmux"
)

// fleetBinaryPath / fleetBinaryErr are populated once per `go test`
// invocation by buildFleetBinary; tests share the same binary because
// the build is expensive (~5s) and the binary is read-only. sync.Once
// protects against the race where two parallel tests both find an
// empty path and start two builds.
var (
	fleetBinaryOnce sync.Once
	fleetBinaryPath string
	fleetBinaryErr  error
)

// buildFleetBinary compiles cmd/fleet into a tempdir and returns the
// absolute path. Called lazily on first integration test so the test
// suite still runs fast for unrelated `go test ./cmd/fleet/...`
// invocations (Go's test runner caches across invocations but a
// per-binary `go build` is unavoidable). On subsequent calls the
// already-built path is returned.
//
// sync.Once ensures only one build runs even if multiple integration
// tests start concurrently. Build errors propagate through
// fleetBinaryErr so subsequent callers fail with the same message
// rather than racing into a second build attempt.
func buildFleetBinary(t *testing.T) string {
	t.Helper()
	fleetBinaryOnce.Do(func() {
		if _, err := exec.LookPath("go"); err != nil {
			fleetBinaryErr = fmt.Errorf("`go` not on PATH: %w", err)
			return
		}
		dir, err := os.MkdirTemp("", "fleet-int-bin-")
		if err != nil {
			fleetBinaryErr = fmt.Errorf("mkdir bin: %w", err)
			return
		}
		bin := filepath.Join(dir, "fleet")
		if runtime.GOOS == "windows" {
			bin += ".exe"
		}
		cmd := exec.Command("go", "build", "-o", bin, "./cmd/fleet")
		cmd.Dir = repoRoot(t)
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			_ = os.RemoveAll(dir)
			fleetBinaryErr = fmt.Errorf("go build ./cmd/fleet: %w", err)
			return
		}
		fleetBinaryPath = bin
	})
	if fleetBinaryErr != nil {
		// Skip rather than fatal so a missing `go` toolchain on a
		// CI runner without dev tooling doesn't tank the whole
		// integration test suite — just declines to run them.
		t.Skip(fleetBinaryErr.Error())
	}
	return fleetBinaryPath
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
	homeDir    string // sandboxed $HOME so subprocess `fleet dispatch` can't write to the operator's real ~/.claude
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
	homeDir := filepath.Join(tmp, "home")
	fleetHome := filepath.Join(tmp, ".fleet")
	claudeHome := filepath.Join(homeDir, ".claude")
	repoCwd := filepath.Join(tmp, "repo")
	binDir := filepath.Join(tmp, "bin")
	for _, d := range []string{homeDir, repoCwd, binDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
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

	// Set FLEET_HOME + HOME + PATH first so subsequent commands hit the
	// sandbox. HOME override is critical: maybeAutoInit (called by
	// `fleet dispatch`) walks UserHomeDir() to compute its skill
	// install path. Without this override, the integration test's
	// dispatched workers would auto-install fleet-guard into the
	// operator's REAL ~/.claude/skills/, polluting the dev machine.
	t.Setenv("FLEET_HOME", fleetHome)
	t.Setenv("HOME", homeDir)
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
		homeDir:    homeDir,
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
	cmd.Env = append(os.Environ(),
		"FLEET_HOME="+env.fleetHome,
		"HOME="+env.homeDir,
		"PATH="+env.binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	// Postmortem 2026-05-14 (orphan tmux leak): propagate
	// FLEET_TMUX_SOCKET so the child `fleet` binary's tmux.Spawn
	// targets the per-test server, not the operator's default.
	// os.Environ() above DOES include this var (requireTmux set it
	// via t.Setenv), but the explicit append in cmd.Env after
	// os.Environ() would still inherit it — so a no-op append here
	// is for explicitness + future-proofing if the env layering shifts.
	if sock := os.Getenv("FLEET_TMUX_SOCKET"); sock != "" {
		cmd.Env = append(cmd.Env, "FLEET_TMUX_SOCKET="+sock)
	}
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
	return env.runTickCap(t, 1)
}

// runTickCap drives one tick at the requested cap. cap=1 keeps
// single-worker mode (byte-identical to v0.2.0); cap>1 enables
// worktree-mode dispatch (requires a real git repo at env.repoCwd).
func (env *integrationEnv) runTickCap(t *testing.T, cap int) string {
	t.Helper()
	driver := fmt.Sprintf(`import json, os, sys
sys.path.insert(0, %q)
import loop
res = loop.tick(%q,
                coord_id=%q,
                cwd=%q,
                fleet_home=%q,
                fleet_bin=%q,
                cap=%d)
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
		cap,
	)
	cmd := exec.Command("python3", "-c", driver)
	cmd.Env = append(os.Environ(),
		"FLEET_HOME="+env.fleetHome,
		"HOME="+env.homeDir,
		"FLEET_AGENT_ID="+env.coordID,
		"PATH="+env.binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		// Disable the supervisor loop (issue #79). Integration tests
		// drive runTick synchronously — they assert on the FIRST tick
		// only. Without this, the supervisor enters its 30 s sleep
		// loop and the test wedges until FLEET_COORD_POLL_MAX_S
		// (default 4 h) elapses.
		"FLEET_COORD_POLL_INTERVAL_S=0",
	)
	// Postmortem 2026-05-14 (orphan tmux leak): the python3 driver's
	// `loop.tick` shells out to `fleet dispatch`, which calls
	// tmux.Spawn. Without explicit FLEET_TMUX_SOCKET propagation any
	// drift in the os.Environ() inheritance (e.g., a future PR that
	// passes cmd.Env = []string{...} directly) silently leaks tmux
	// sessions onto the operator's default server.
	if sock := os.Getenv("FLEET_TMUX_SOCKET"); sock != "" {
		cmd.Env = append(cmd.Env, "FLEET_TMUX_SOCKET="+sock)
	}
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

// markLaunched simulates the coord's "Worker dispatch protocol" step 2
// (dispatch-durability #184): for every dispatch journal currently at
// ExecPending, flip it to launch_attempted via `fleet claims
// mark-launch-attempted <id> <gen>`. The real coord does this between
// ticks immediately before invoking the Agent; integration tests that
// drive ticks directly must replicate it, otherwise the journals stay
// ExecPending and the tick-entry replay reconcile re-emits them (a
// faithful behavior, but it inflates the `dispatched` count the
// pre-#184 single-launch tests assert on).
func (env *integrationEnv) markLaunched(t *testing.T) {
	t.Helper()
	dispDir := filepath.Join(env.fleetHome, "dispatches")
	entries, err := os.ReadDir(dispDir)
	if err != nil {
		return // no journals yet
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".json.lock") {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		raw, rerr := os.ReadFile(filepath.Join(dispDir, name))
		if rerr != nil {
			continue
		}
		var j struct {
			ExecState  string `json:"exec_state"`
			Generation int    `json:"generation"`
		}
		if json.Unmarshal(raw, &j) != nil || j.ExecState != "pending" {
			continue
		}
		env.runFleet(t, "claims", "mark-launch-attempted", id,
			strconv.Itoa(j.Generation))
	}
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

// initGitRepo bootstraps `dir` as a real git repository with one
// initial commit on `main`. The coord's worktree-mode dispatch needs
// HEAD to exist so `git worktree add -b <branch>` resolves a base
// commit; without an initial commit git rejects the add as "fatal:
// not a valid object name: 'HEAD'".
func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH; skipping worktree-mode test")
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "test@example.invalid"},
		{"config", "user.name", "Test"},
		{"commit", "--allow-empty", "-q", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		var errb bytes.Buffer
		cmd.Stderr = &errb
		if err := cmd.Run(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, errb.String())
		}
	}
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
//     in-progress and worker_pid is set to the coord's pid (codex
//     full-stack [P1] regress: previously left worker_pid=0, which
//     made tick #2's reconcile requeue the task before the drain
//     step could run).
//  3. Replant `worker_pid=<test_pid>` to make tick #2's
//     reconcile see a live worker (the dispatched coord pid is the
//     test process, alive in this thread).
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
	assertNoTickErrors(t, out1)
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
	assertNoTickErrors(t, out2)
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

// listAllAgents returns every live agent record, used by test cleanups
// to reap tmux sessions for any workers spawned during the test.
func listAllAgents(t *testing.T) []*agent.Record {
	t.Helper()
	live, err := agent.List()
	if err != nil {
		t.Fatalf("agent.List: %v", err)
	}
	return live
}

// assertNoTickErrors fails the test if loop.tick() reported any errors
// in its TickResult. Catches silent CLI failures (e.g., missing flag,
// permission error inside the Python skill) that wouldn't otherwise
// surface as a non-zero exit because tick() always returns 0 to match
// fleet-guard discipline.
//
// fleet#175 carve-out: tick() now emits an informational fallback
// warning into result.errors when coord-config.json::repo is missing.
// That breadcrumb is informational (the operator should re-spawn the
// coord or set the field manually), NOT a hard error. The integration
// tests don't seed coord-config.json so they'd see this warning on
// every tick — filter it out at the assertion site rather than
// mutating every test fixture.
//
// iter-10 (codex P2 resolution): parse the JSON output and check the
// errors[] array directly instead of substring-matching against a
// hard-coded badPrefix denylist. The denylist approach masked any
// future error category not yet listed (e.g., a new `claims ...`
// error class would slip through). Parsing is precise.
func assertNoTickErrors(t *testing.T, tickOutput string) {
	t.Helper()
	// Parse the JSON. tickOutput may contain leading garbage from the
	// Python driver (e.g., stderr captured before the JSON line), so
	// find the last `{...}` blob and parse that.
	jsonStart := strings.LastIndex(tickOutput, "{")
	jsonEnd := strings.LastIndex(tickOutput, "}")
	if jsonStart < 0 || jsonEnd < jsonStart {
		t.Errorf("assertNoTickErrors: no JSON blob in tick output:\n%s", tickOutput)
		return
	}
	var parsed struct {
		Errors []string `json:"errors"`
	}
	if err := json.Unmarshal([]byte(tickOutput[jsonStart:jsonEnd+1]), &parsed); err != nil {
		t.Errorf("assertNoTickErrors: parse tick JSON: %v\n%s", err, tickOutput)
		return
	}
	const fleet175Marker = "coord-config.json::repo not set for"
	var realErrors []string
	for _, e := range parsed.Errors {
		// Tolerate ONLY the canonical fleet#175 fallback warning.
		// Any other error entry — including future fleet#175 warnings
		// in other shapes — fails the test, surfacing the new
		// category for explicit triage.
		if strings.Contains(e, fleet175Marker) {
			continue
		}
		realErrors = append(realErrors, e)
	}
	if len(realErrors) > 0 {
		t.Errorf("loop.tick() reported %d real error(s) beyond the fleet#175 fallback:\n%s\nfull output:\n%s",
			len(realErrors), strings.Join(realErrors, "\n"), tickOutput)
	}
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
	assertNoTickErrors(t, out)

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
	// Worktree-mode dispatch (cap>1) needs a real git repo at repoCwd.
	initGitRepo(t, env.repoCwd)

	// Two ready tasks → two dispatches in tick #1. Each task declares
	// its own disjoint Files: scope so the v0.2.x conflict-aware
	// dispatch loop lets both run concurrently. Without an explicit
	// Files: line the loop would conservatively skip the second task
	// (a no-files task is treated as "could touch anything").
	slugA := env.addReadyTask(t, "alpha-c2", "alpha task spec.\nFiles: alpha-c2.go")
	slugB := env.addReadyTask(t, "bravo-c2", "bravo task spec.\nFiles: bravo-c2.go")

	out1 := env.runTickCap(t, 2)
	if !strings.Contains(out1, `"dispatched": 2`) {
		t.Fatalf("first tick did not dispatch both: %s", out1)
	}
	assertNoTickErrors(t, out1)

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

	out2 := env.runTickCap(t, 2)
	if !strings.Contains(out2, `"drained": 2`) {
		t.Fatalf("second tick did not drain both sentinels: %s", out2)
	}
	assertNoTickErrors(t, out2)

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

// ---------- v0.2.x: worktree mode ----------

// TestCoordParallelDispatch_CreatesWorktrees covers the core v0.2.x
// behavior: with cap > 1 and a real git repo, two parallel dispatches
// each get their own ~/.fleet/projects/<p>/worktrees/<slug>/ working
// tree branched off `worker/<slug>`. Cleanup half: simulating both
// workers reporting DONE_PR triggers `git worktree remove` so the
// directories disappear after the next tick.
func TestCoordParallelDispatch_CreatesWorktrees(t *testing.T) {
	env := setupCoordIntegration(t, "wt-proj")
	env.plantCoord(t)
	initGitRepo(t, env.repoCwd)

	// Disjoint Files: scopes so the conflict-aware dispatch loop
	// dispatches both in the same tick (a no-files task would be
	// conservatively skipped while another worker is in flight).
	slugA := env.addReadyTask(t, "wt-alpha", "alpha worktree task spec.\nFiles: wt-alpha.go")
	slugB := env.addReadyTask(t, "wt-bravo", "bravo worktree task spec.\nFiles: wt-bravo.go")

	// First tick: cap=2 dispatches both tasks, each into its own worktree.
	out1 := env.runTickCap(t, 2)
	if !strings.Contains(out1, `"dispatched": 2`) {
		t.Fatalf("first tick did not dispatch both: %s", out1)
	}
	assertNoTickErrors(t, out1)

	// Both worktree dirs exist on disk.
	wtA := filepath.Join(env.fleetHome, "projects", env.project, "worktrees", slugA)
	wtB := filepath.Join(env.fleetHome, "projects", env.project, "worktrees", slugB)
	for _, wt := range []string{wtA, wtB} {
		if info, err := os.Stat(wt); err != nil {
			t.Errorf("worktree dir missing: %s (%v)", wt, err)
		} else if !info.IsDir() {
			t.Errorf("worktree path is not a dir: %s", wt)
		}
	}

	// Both tasks carry the worktree path on tasks.md.
	taskA := env.readTask(t, slugA)
	taskB := env.readTask(t, slugB)
	if !strings.HasPrefix(taskA.Worktree, wtA) {
		t.Errorf("taskA worktree=%q want prefix %q", taskA.Worktree, wtA)
	}
	if !strings.HasPrefix(taskB.Worktree, wtB) {
		t.Errorf("taskB worktree=%q want prefix %q", taskB.Worktree, wtB)
	}
	if taskA.Branch != "worker/"+slugA {
		t.Errorf("taskA branch=%q want worker/%s", taskA.Branch, slugA)
	}
	if taskB.Branch != "worker/"+slugB {
		t.Errorf("taskB branch=%q want worker/%s", taskB.Branch, slugB)
	}

	// Cleanup half: simulate both workers reporting DONE_PR via the
	// inbox archive. Reconcile alone won't fire (workers are alive in
	// kill -0 sense via test pid), so the cleanup must come through
	// the sentinel-drain path.
	env.runFleet(t, "tasks", "set", "--project", env.project, slugA, fmt.Sprintf("worker_pid=%d", os.Getpid()))
	env.runFleet(t, "tasks", "set", "--project", env.project, slugB, fmt.Sprintf("worker_pid=%d", os.Getpid()))
	urlA := "https://github.com/fake/repo/pull/100"
	urlB := "https://github.com/fake/repo/pull/200"
	env.writeSentinelArchive(t, fmt.Sprintf("TASK_DONE_PR=%s %s", slugA, urlA))
	time.Sleep(2 * time.Millisecond)
	env.writeSentinelArchive(t, fmt.Sprintf("TASK_DONE_PR=%s %s", slugB, urlB))

	out2 := env.runTickCap(t, 2)
	if !strings.Contains(out2, `"drained": 2`) {
		t.Fatalf("second tick did not drain both sentinels: %s", out2)
	}
	assertNoTickErrors(t, out2)

	// Both worktree dirs gone post-cleanup.
	for _, wt := range []string{wtA, wtB} {
		if _, err := os.Stat(wt); !os.IsNotExist(err) {
			t.Errorf("worktree dir still present after cleanup: %s (err=%v)", wt, err)
		}
	}

	// tasks.md worktree field cleared so re-dispatch starts fresh.
	taskA = env.readTask(t, slugA)
	taskB = env.readTask(t, slugB)
	if taskA.Worktree != "" {
		t.Errorf("taskA worktree not cleared post-cleanup: %q", taskA.Worktree)
	}
	if taskB.Worktree != "" {
		t.Errorf("taskB worktree not cleared post-cleanup: %q", taskB.Worktree)
	}
	// PR URLs propagated; status flipped to in-review.
	if taskA.PRURL != urlA || taskB.PRURL != urlB {
		t.Errorf("PR URLs missing: A=%q B=%q", taskA.PRURL, taskB.PRURL)
	}
	if taskA.Status != tasks.StatusInReview || taskB.Status != tasks.StatusInReview {
		t.Errorf("statuses: A=%q B=%q want both in-review", taskA.Status, taskB.Status)
	}

	// Cleanup tmux.
	for _, rec := range listAllAgents(t) {
		_ = tmux.Kill(rec.TmuxSession)
	}
}

// TestCoordParallelDispatch_Cap1Mode_NoWorktreeCreated regression-
// guards single-worker mode: with cap=1, the dispatch path must not
// create any worktree dir even if the project has multiple ready
// tasks. Worker uses repoCwd as its cwd; tasks.md.worktree stays "".
func TestCoordParallelDispatch_Cap1Mode_NoWorktreeCreated(t *testing.T) {
	env := setupCoordIntegration(t, "wt-cap1")
	env.plantCoord(t)
	// initGitRepo NOT called — cap=1 mode never invokes git worktree.

	slug := env.addReadyTask(t, "cap1-task", "single-worker task.")

	out := env.runTickCap(t, 1)
	if !strings.Contains(out, `"dispatched": 1`) {
		t.Fatalf("cap=1 tick did not dispatch: %s", out)
	}
	assertNoTickErrors(t, out)

	task := env.readTask(t, slug)
	if task.Worktree != "" {
		t.Errorf("cap=1 dispatch set worktree=%q, want empty", task.Worktree)
	}

	// No worktrees/ tree was created under projects/<name>/.
	wtRoot := filepath.Join(env.fleetHome, "projects", env.project, "worktrees")
	if _, err := os.Stat(wtRoot); !os.IsNotExist(err) {
		t.Errorf("cap=1 mode created worktrees/ tree: err=%v", err)
	}

	for _, rec := range listAllAgents(t) {
		_ = tmux.Kill(rec.TmuxSession)
	}
}

// TestCoordParallelDispatch_PrunesOrphanWorktrees covers the v0.2.x
// orphan-cleanup contract: at the top of every cap > 1 tick the coord
// runs `git worktree prune` so a registry entry pointing at a missing
// directory (left behind by a coord that crashed mid-tick) doesn't
// poison the next dispatch.
//
// Setup mirrors a real failure: we register a worktree via
// `git worktree add`, then `rm -rf` the directory. Git keeps the
// registry entry — `git worktree list` still mentions the path — so
// the next `git worktree add` for the SAME path would fatal "already
// exists". After tick(), the prune call must have dropped the entry.
func TestCoordParallelDispatch_PrunesOrphanWorktrees(t *testing.T) {
	env := setupCoordIntegration(t, "orphan-proj")
	env.plantCoord(t)
	initGitRepo(t, env.repoCwd)

	// Register an orphan worktree: real `git worktree add`, then nuke
	// the directory. Git's registry entry survives.
	orphanDir := filepath.Join(env.fleetHome, "projects", env.project, "worktrees", "orphan-leftover")
	if err := os.MkdirAll(filepath.Dir(orphanDir), 0o755); err != nil {
		t.Fatalf("mkdir worktrees parent: %v", err)
	}
	addCmd := exec.Command("git", "-C", env.repoCwd, "worktree", "add",
		"-b", "worker/orphan-leftover", orphanDir)
	var addErr bytes.Buffer
	addCmd.Stderr = &addErr
	if err := addCmd.Run(); err != nil {
		t.Fatalf("seed git worktree add: %v\n%s", err, addErr.String())
	}
	// rm -rf the dir; git's registry entry survives.
	if err := os.RemoveAll(orphanDir); err != nil {
		t.Fatalf("rm orphan dir: %v", err)
	}

	// Confirm the orphan is registered before the tick runs.
	listBefore, err := exec.Command("git", "-C", env.repoCwd, "worktree", "list", "--porcelain").Output()
	if err != nil {
		t.Fatalf("git worktree list pre-tick: %v", err)
	}
	if !strings.Contains(string(listBefore), orphanDir) {
		t.Fatalf("orphan setup didn't register worktree: %s", listBefore)
	}

	// Add an unrelated ready task so the tick has something to do
	// (besides prune). cap=2 dispatch path runs end-to-end.
	slug := env.addReadyTask(t, "fresh-task", "Fresh task.\nFiles: fresh.go")

	// Drive one cap=2 tick. Prune fires at the top.
	out := env.runTickCap(t, 2)
	assertNoTickErrors(t, out)

	// After tick: orphan registry entry pruned. Use --porcelain so the
	// match is on the absolute path git emits, not a relative shorthand.
	listAfter, err := exec.Command("git", "-C", env.repoCwd, "worktree", "list", "--porcelain").Output()
	if err != nil {
		t.Fatalf("git worktree list post-tick: %v", err)
	}
	if strings.Contains(string(listAfter), orphanDir) {
		t.Errorf("orphan worktree still registered post-tick:\n%s", listAfter)
	}

	// Sanity: the fresh task still dispatched (its worktree exists).
	freshWT := filepath.Join(env.fleetHome, "projects", env.project, "worktrees", slug)
	if _, err := os.Stat(freshWT); err != nil {
		t.Errorf("fresh-task worktree missing: %s (%v)", freshWT, err)
	}

	// Cleanup tmux for any dispatched workers.
	for _, rec := range listAllAgents(t) {
		_ = tmux.Kill(rec.TmuxSession)
	}
}

// TestCoordParallelDispatch_SkipsOverlappingTasks covers the v0.2.x
// conflict-aware dispatch contract end-to-end:
//
//   - 3 ready tasks: A (Files: a.go), B (Files: a.go b.go), C (Files: c.go).
//   - Tick #1 with cap=2: A and C dispatch (disjoint scopes); B is
//     skipped because it overlaps A on a.go.
//   - Mark A's worker DONE_PR via the inbox archive; tick #2 drains it
//     so A transitions to in-review. Now only C is in-flight (with
//     Files: c.go) — B's a.go and b.go are no longer claimed by an
//     in-progress worker, and B's scope is disjoint from C's c.go.
//   - Tick #2 also dispatches B as the second cap slot (cap=2 still has
//     headroom: A is now in-review, only C is in-progress).
//
// The test exercises the full skill path (parse → dispatch → drain)
// against a real `fleet` binary + real git. CI is stubbed via the
// default ghStubReturnsEmpty so reconcile leaves tasks alone.
func TestCoordParallelDispatch_SkipsOverlappingTasks(t *testing.T) {
	env := setupCoordIntegration(t, "conflict-proj")
	env.plantCoord(t)
	initGitRepo(t, env.repoCwd)

	// Three ready tasks. Files: declarations encode the overlap matrix:
	//   A ↔ B overlap on a.go; A ↔ C disjoint; B ↔ C disjoint.
	slugA := env.addReadyTask(t, "alpha-conf", "Alpha task.\nFiles: a.go")
	slugB := env.addReadyTask(t, "bravo-conf", "Bravo task.\nFiles: a.go b.go")
	slugC := env.addReadyTask(t, "charlie-conf", "Charlie task.\nFiles: c.go")

	// Tick #1: cap=2 → expect 2 dispatches, B skipped.
	out1 := env.runTickCap(t, 2)
	if !strings.Contains(out1, `"dispatched": 2`) {
		t.Fatalf("first tick did not dispatch exactly 2 (expected A+C, B skipped): %s", out1)
	}
	assertNoTickErrors(t, out1)

	// dispatch-durability (#184): simulate the coord flipping the
	// dispatched workers' journals to launch_attempted (protocol step 2)
	// so the tick-#2 replay reconcile doesn't re-emit A/C (which would
	// inflate the dispatch count this conflict test asserts on).
	env.markLaunched(t)

	taskA := env.readTask(t, slugA)
	taskB := env.readTask(t, slugB)
	taskC := env.readTask(t, slugC)
	if taskA.Status != tasks.StatusInProgress {
		t.Errorf("alpha status=%q want in-progress (sorted first by slug, dispatched)", taskA.Status)
	}
	if taskC.Status != tasks.StatusInProgress {
		t.Errorf("charlie status=%q want in-progress (disjoint from A)", taskC.Status)
	}
	if taskB.Status != tasks.StatusReady {
		t.Errorf("bravo status=%q want ready (overlaps A on a.go, must be skipped)", taskB.Status)
	}

	// Mark A and C alive so the next tick's reconcile doesn't requeue
	// them. Worker pid points at the live test process.
	env.runFleet(t, "tasks", "set", "--project", env.project, slugA,
		fmt.Sprintf("worker_pid=%d", os.Getpid()))
	env.runFleet(t, "tasks", "set", "--project", env.project, slugC,
		fmt.Sprintf("worker_pid=%d", os.Getpid()))

	// Worker A reports DONE_PR — moves to in-review and frees a.go.
	urlA := "https://github.com/fake/repo/pull/501"
	env.writeSentinelArchive(t, fmt.Sprintf("TASK_DONE_PR=%s %s", slugA, urlA))

	// Tick #2: drain A's sentinel; B becomes dispatchable because the
	// only remaining in-flight worker (C) has disjoint scope (c.go).
	out2 := env.runTickCap(t, 2)
	if !strings.Contains(out2, `"drained": 1`) {
		t.Fatalf("second tick did not drain A's sentinel: %s", out2)
	}
	if !strings.Contains(out2, `"dispatched": 1`) {
		t.Fatalf("second tick did not dispatch B (conflict cleared): %s", out2)
	}
	assertNoTickErrors(t, out2)

	// Final state:
	//   A: in-review (drained, pr_url set)
	//   B: in-progress (newly dispatched — a.go no longer in-flight)
	//   C: in-progress (still running)
	taskA = env.readTask(t, slugA)
	taskB = env.readTask(t, slugB)
	taskC = env.readTask(t, slugC)
	if taskA.Status != tasks.StatusInReview {
		t.Errorf("alpha post-drain status=%q want in-review", taskA.Status)
	}
	if taskA.PRURL != urlA {
		t.Errorf("alpha pr_url=%q want %q", taskA.PRURL, urlA)
	}
	if taskB.Status != tasks.StatusInProgress {
		t.Errorf("bravo post-drain status=%q want in-progress (conflict cleared)", taskB.Status)
	}
	if taskC.Status != tasks.StatusInProgress {
		t.Errorf("charlie post-drain status=%q want in-progress (still running)", taskC.Status)
	}

	// Cleanup tmux for any dispatched workers.
	for _, rec := range listAllAgents(t) {
		_ = tmux.Kill(rec.TmuxSession)
	}
}
