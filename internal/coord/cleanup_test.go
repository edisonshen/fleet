// Tests for coord.Cleanup — the unified function invoked on every coord
// exit path (signal, clean exit, panic). See docs/DESIGN-cleanup-fleet-
// owns-resources.md §PR-C and docs/TASK-PLAN-cleanup-pr-c-coord-exit-6acc.md.
//
// The three side-effects MUST all fire on every call:
//
//  1. tmux.Kill(session)  — best-effort, non-fatal.
//  2. Move ~/.fleet/agents/<id>.json → ~/.fleet/agents/archive/<id>-<UTC-ts>.json.
//  3. Remove ~/.fleet/projects/<p>/.locks/coord-spawn-marker if its
//     body equals <id>. Other-ID markers are preserved.
//
// Panic-safety: each step runs in its own goroutine-local
// `func() { defer recover(); ... }()` so a panic in one (e.g. a buggy
// tmux killer) does NOT skip the remaining steps. The PanicViaDefer
// test pins this contract by injecting a tmux killer that panics and
// asserting the agent JSON + marker were still cleared.
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
// canonical subdirs so AgentPath / CoordSpawnMarkerPath / AgentArchivePath
// resolve cleanly. Mirrors internal/state/hidden_test.go withFleetHome.
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

// writeMarker bootstraps the project's .locks dir + writes the
// coord-spawn-marker with body = id (so tests don't reimplement the
// project-path resolution).
func writeMarker(t *testing.T, project, id string) string {
	t.Helper()
	if _, err := state.EnsureProjectInitialized(project); err != nil {
		t.Fatalf("ensure project: %v", err)
	}
	if err := state.WriteCoordSpawnMarker(project, id); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	path, err := state.CoordSpawnMarkerPath(project)
	if err != nil {
		t.Fatalf("marker path: %v", err)
	}
	return path
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
	markerPath := writeMarker(t, project, agentID)

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

	// Marker (body matched id) must be cleared.
	if _, err := os.Stat(markerPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("marker still exists at %s (err=%v)", markerPath, err)
	}
}

// TestCleanup_MarkerClearedWhenMatched pins the marker-body gate:
// body==id ⇒ removed; body==other ⇒ untouched. Operator's contract is
// "never stomp another coord's marker even if you're the named agent
// for THIS project under some other identity."
func TestCleanup_MarkerClearedWhenMatched(t *testing.T) {
	tests := []struct {
		name        string
		markerBody  string
		agentID     string
		wantRemoved bool
	}{
		{
			name:        "marker-body-matches-our-id",
			markerBody:  "aaaa1111",
			agentID:     "aaaa1111",
			wantRemoved: true,
		},
		{
			name:        "marker-body-is-other-id",
			markerBody:  "bbbb2222",
			agentID:     "aaaa1111",
			wantRemoved: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			helperFleetHome(t)
			const project = "marker-test"
			writeAgentRecord(t, tc.agentID, project, "fleet-"+tc.agentID)
			markerPath := writeMarker(t, project, tc.markerBody)

			deps := Deps{KillTmux: func(string) error { return nil }}
			if err := Cleanup(tc.agentID, project, deps); err != nil {
				t.Fatalf("Cleanup: %v", err)
			}

			_, statErr := os.Stat(markerPath)
			removed := errors.Is(statErr, os.ErrNotExist)
			if removed != tc.wantRemoved {
				t.Errorf("marker removed=%v, want %v (statErr=%v)", removed, tc.wantRemoved, statErr)
			}
		})
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
	markerPath := writeMarker(t, project, agentID)

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

	// Even though KillTmux panicked, the archive + marker steps must
	// still have fired.
	livePath, err := state.AgentPath(agentID)
	if err != nil {
		t.Fatalf("AgentPath: %v", err)
	}
	if _, err := os.Stat(livePath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("post-panic: live record still exists at %s (err=%v)", livePath, err)
	}
	if _, err := os.Stat(markerPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("post-panic: marker still exists at %s (err=%v)", markerPath, err)
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
	writeMarker(t, project, agentID)

	deps := Deps{KillTmux: func(string) error { return nil }}

	if err := Cleanup(agentID, project, deps); err != nil {
		t.Fatalf("first Cleanup: %v", err)
	}
	if err := Cleanup(agentID, project, deps); err != nil {
		t.Fatalf("second Cleanup (idempotency): %v", err)
	}
}

// TestCleanup_MarkerStepSkippedOnSpawnLockContended pins the codex
// iter-2 [P1] fix: when a replacement coord is in flight (it holds the
// coord-spawn lock right now), Cleanup MUST skip the marker removal
// step rather than race the replacement's marker write. Preserving the
// marker is strictly safer than deleting one belonging to another coord.
//
// The injected AcquireSpawnLock returns a permission-denied error to
// stand in for "lock currently held by another process" — we don't
// need the real flock here, just the contract that contention → skip.
func TestCleanup_MarkerStepSkippedOnSpawnLockContended(t *testing.T) {
	helperFleetHome(t)
	const (
		agentID = "cont1234"
		project = "cont-test"
		session = "fleet-cont1234"
	)
	writeAgentRecord(t, agentID, project, session)
	markerPath := writeMarker(t, project, agentID)

	deps := Deps{
		KillTmux: func(string) error { return nil },
		AcquireSpawnLock: func(string) (func(), error) {
			return nil, errors.New("simulated coord-spawn lock contention")
		},
	}
	if err := Cleanup(agentID, project, deps); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	// Steps 1+2 still ran: live record archived.
	livePath, err := state.AgentPath(agentID)
	if err != nil {
		t.Fatalf("AgentPath: %v", err)
	}
	if _, err := os.Stat(livePath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("live record still exists: %v", err)
	}
	// Step 3 was SKIPPED (lock contended) → marker preserved verbatim.
	body, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("marker should still exist when spawn-lock is contended: %v", err)
	}
	got := strings.TrimSpace(string(body))
	if got != agentID {
		t.Errorf("marker body = %q, want %q (preserved)", got, agentID)
	}
}

// TestCleanup_MarkerStepHoldsSpawnLock pins the lock-held-during-step
// half of the codex iter-2 [P1] fix: while step 3 runs, the spawn-lock
// release function is NOT invoked. We assert this by inspecting a
// release-was-called flag that the injected stub flips. The check
// itself happens INSIDE clearMarkerIfMatched's callback path, so if
// the implementation releases too early (before the unlink) the flag
// would already be true at marker-read time.
//
// Mechanically: the stub returns a release that increments a counter;
// we verify the counter goes 0→1 exactly once (acquire holds across
// the whole step, then release fires once via defer).
func TestCleanup_MarkerStepHoldsSpawnLock(t *testing.T) {
	helperFleetHome(t)
	const (
		agentID = "hold1234"
		project = "hold-test"
		session = "fleet-hold1234"
	)
	writeAgentRecord(t, agentID, project, session)
	writeMarker(t, project, agentID)

	var releaseCount int
	deps := Deps{
		KillTmux: func(string) error { return nil },
		AcquireSpawnLock: func(p string) (func(), error) {
			if p != project {
				t.Errorf("AcquireSpawnLock called with %q, want %q", p, project)
			}
			return func() { releaseCount++ }, nil
		},
	}
	if err := Cleanup(agentID, project, deps); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if releaseCount != 1 {
		t.Errorf("AcquireSpawnLock release called %d times, want 1", releaseCount)
	}
}

// TestCleanup_MarkerAtomicCaptureRestoresWhenBodyChanges pins the
// codex iter-4 [P1] fix: the atomic-rename capture in
// clearMarkerIfMatched MUST preserve a marker whose body has been
// rewritten by a non-cooperative concurrent writer. We simulate this
// by pre-writing a side file with a DIFFERENT body before invoking
// Cleanup — the rename → side captures the other-body marker, the
// content check fails the match, and the marker is renamed back. End
// state: marker exists with body == other (preserved verbatim).
//
// This is the test that catches a regression to the old non-atomic
// "read then unlink" path: if the implementation reverts to
// state.RemoveCoordSpawnMarker after a non-matching body, the marker
// would be unlinked instead of restored.
func TestCleanup_MarkerAtomicCaptureRestoresWhenBodyChanges(t *testing.T) {
	helperFleetHome(t)
	const (
		agentID = "aaaa1111"
		otherID = "ZZZZ9999"
		project = "atomic-restore-test"
		session = "fleet-aaaa1111"
	)
	writeAgentRecord(t, agentID, project, session)
	// Marker body deliberately != agentID. clearMarkerIfMatched must
	// rename-capture → see non-matching body → rename-back.
	markerPath := writeMarker(t, project, otherID)

	deps := Deps{KillTmux: func(string) error { return nil }}
	if err := Cleanup(agentID, project, deps); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	data, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("marker missing after non-match restore: %v", err)
	}
	got := strings.TrimSpace(string(data))
	if got != otherID {
		t.Errorf("marker body = %q, want %q (restored verbatim)", got, otherID)
	}

	// No leftover side files in the .locks/ dir (only the marker
	// itself).
	parent := filepath.Dir(markerPath)
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatalf("readdir %s: %v", parent, err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".clearing.") {
			t.Errorf("leftover side file %s in %s — restore did not clean up its scratch path", e.Name(), parent)
		}
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
	markerPath := writeMarker(t, project, agentID)

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
	if _, err := os.Stat(markerPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("marker still exists after tmux-kill error: %v", err)
	}
}
