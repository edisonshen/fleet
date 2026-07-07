// Tests for coord.Cleanup — the unified function invoked on every coord
// exit path (signal, clean exit, panic). See docs/DESIGN-cleanup-fleet-
// owns-resources.md §PR-C and docs/TASK-PLAN-cleanup-pr-c-coord-exit-6acc.md.
//
// The two side-effects MUST all fire on every call:
//
//  1. tmux.Kill(session)  — best-effort, non-fatal.
//  2. Move ~/.fleet/agents/<id>.json → ~/.fleet/agents/archive/<id>-<UTC-ts>.json.
//
// (The coord-spawn marker step is gone — D3. The coord's identity lives in
// the coordinator lease now, released by the coord-run supervisor on exit.)
//
// Panic-safety: each step runs in its own goroutine-local
// `func() { defer recover(); ... }()` so a panic in one (e.g. a buggy
// tmux killer) does NOT skip the remaining steps. The PanicViaDefer
// test pins this contract by injecting a tmux killer that panics and
// asserting the agent JSON was still archived.
package coord

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/state"
)

// helperFleetHome points FLEET_HOME at a fresh tmpdir + bootstraps the
// canonical subdirs so AgentPath / AgentArchivePath resolve cleanly.
// Mirrors internal/state/hidden_test.go withFleetHome.
func helperFleetHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("FLEET_HOME", dir)
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return dir
}

// writeAgentRecord lays down a minimal agent record at the expected
// live-path so Cleanup has something to archive.
func writeAgentRecord(t *testing.T, id, project, session string) {
	t.Helper()
	rec := agent.New(id)
	rec.TmuxSession = session
	rec.Project = project
	if err := rec.Write(); err != nil {
		t.Fatalf("write agent record: %v", err)
	}
}

// TestCleanup_CleanExitPath exercises the happy-path direct call:
// every side-effect fires, no errors.
func TestCleanup_CleanExitPath(t *testing.T) {
	helperFleetHome(t)
	const (
		agentID = "abcd1234"
		project = "test-project"
		// tmux.SessionName is the canonical "fleet-<id>"; coord-spawn
		// records this as the agent's TmuxSession, and Cleanup uses the
		// same helper to derive what to kill.
		session = "fleet-abcd1234"
	)
	writeAgentRecord(t, agentID, project, session)

	// Stub tmux killer to record the session name it was asked to kill,
	// without actually shelling out to tmux (tests must be deterministic
	// + isolated from any /tmp/ side-effects per CLAUDE.md §8).
	var killedSession string
	deps := Deps{
		KillTmux: func(s string) error {
			killedSession = s
			return nil
		},
	}

	if err := Cleanup(agentID, project, deps); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	if killedSession != session {
		t.Errorf("KillTmux called with %q, want %q", killedSession, session)
	}

	// Live record must be gone.
	livePath, err := state.AgentPath(agentID)
	if err != nil {
		t.Fatalf("AgentPath: %v", err)
	}
	if _, err := os.Stat(livePath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("live record still exists at %s (err=%v)", livePath, err)
	}

	// Archive must exist with the UTC-suffixed name.
	archiveDir, err := state.AgentArchivePath(agentID)
	if err != nil {
		t.Fatalf("AgentArchivePath: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(archiveDir), agentID+"-*.json"))
	if err != nil {
		t.Fatalf("glob archive: %v", err)
	}
	if len(matches) != 1 {
		t.Errorf("archive glob = %v, want exactly one UTC-suffixed file", matches)
	}
	// Sanity: the archive name must carry an UTC timestamp suffix shape
	// "<id>-YYYYMMDD-HHMMSS.json" (20060102-150405 layout).
	if len(matches) == 1 {
		base := filepath.Base(matches[0])
		want := agentID + "-"
		if !strings.HasPrefix(base, want) || !strings.HasSuffix(base, ".json") {
			t.Errorf("archive basename %q lacks expected <id>-<ts>.json shape", base)
		}
		// 8 digits + "-" + 6 digits = 15 chars between the dash after id and ".json".
		stem := strings.TrimSuffix(strings.TrimPrefix(base, want), ".json")
		if len(stem) != 15 {
			t.Errorf("archive timestamp stem %q has wrong width (want 15)", stem)
		}
	}
}

// TestCleanup_PanicViaDefer is the critical failure-path test: a buggy
// dep (tmux killer panics) must NOT skip the remaining steps. Per
// CLAUDE.md memory feedback_fleet_owns_its_resources.md: "cleanup must
// run on the failure path too, not just the happy path." Each step in
// Cleanup runs under its own `defer recover()` so one panic doesn't
// brick the whole reap.
func TestCleanup_PanicViaDefer(t *testing.T) {
	helperFleetHome(t)
	const (
		agentID = "panic1234"
		project = "panic-test"
		session = "fleet-panic1234"
	)
	writeAgentRecord(t, agentID, project, session)

	deps := Deps{
		KillTmux: func(string) error {
			panic("simulated tmux dep panic")
		},
	}

	// Cleanup must NOT propagate the panic — the whole point of the
	// per-step recover is that the caller (signal handler / defer in
	// main) gets normal control flow back so it can exit cleanly.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Cleanup propagated panic: %v", r)
		}
	}()

	_ = Cleanup(agentID, project, deps)

	// Even though KillTmux panicked, the archive step must still have fired.
	livePath, err := state.AgentPath(agentID)
	if err != nil {
		t.Fatalf("AgentPath: %v", err)
	}
	if _, err := os.Stat(livePath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("post-panic: live record still exists at %s (err=%v)", livePath, err)
	}
}

// TestCleanup_IdempotentOnMissingRecord is a regression hedge: calling
// Cleanup twice in a row (e.g. signal handler races with defer in main)
// must not error on the second call. ENOENT on the live record path is
// the expected "already archived" state.
func TestCleanup_IdempotentOnMissingRecord(t *testing.T) {
	helperFleetHome(t)
	const (
		agentID = "idem1234"
		project = "idem-test"
		session = "fleet-idem1234"
	)
	writeAgentRecord(t, agentID, project, session)

	deps := Deps{KillTmux: func(string) error { return nil }}

	if err := Cleanup(agentID, project, deps); err != nil {
		t.Fatalf("first Cleanup: %v", err)
	}
	if err := Cleanup(agentID, project, deps); err != nil {
		t.Fatalf("second Cleanup (idempotency): %v", err)
	}
}

// TestCleanup_TmuxKillErrorIsNonFatal pins the design's "best-effort,
// non-fatal" tmux-kill contract: a returned error from the killer must
// not abort the rest of the reap. Distinct from the panic test —
// returned errors are the common case (session already gone, etc.).
func TestCleanup_TmuxKillErrorIsNonFatal(t *testing.T) {
	helperFleetHome(t)
	const (
		agentID = "errk1234"
		project = "errk-test"
		session = "fleet-errk1234"
	)
	writeAgentRecord(t, agentID, project, session)

	deps := Deps{
		KillTmux: func(string) error {
			return errors.New("simulated tmux kill failure")
		},
	}
	if err := Cleanup(agentID, project, deps); err != nil {
		t.Fatalf("Cleanup must swallow tmux errors: %v", err)
	}

	livePath, err := state.AgentPath(agentID)
	if err != nil {
		t.Fatalf("AgentPath: %v", err)
	}
	if _, err := os.Stat(livePath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("live record still exists after tmux-kill error: %v", err)
	}
}
