package handoffop

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/state"
)

// seedWorkerAgent writes a worker-shaped agent record under
// FLEET_HOME and returns the record.
func seedWorkerAgent(t *testing.T, id, project, taskSlug string) *agent.Record {
	t.Helper()
	rec := agent.New(id)
	rec.TmuxSession = "fleet-" + id
	rec.Project = project
	rec.TaskID = taskSlug
	if err := rec.Write(); err != nil {
		t.Fatalf("seed agent %s: %v", id, err)
	}
	return rec
}

// stubWorkerTmux replaces the package-level worker-cleanup tmux
// helpers for the duration of the test.
func stubWorkerTmux(t *testing.T,
	kill func(string) error,
	alive func(string) (bool, error),
) {
	t.Helper()
	origKill := tmuxKillForCleanup
	origAlive := tmuxSessionAliveForCleanup
	tmuxKillForCleanup = kill
	tmuxSessionAliveForCleanup = alive
	t.Cleanup(func() {
		tmuxKillForCleanup = origKill
		tmuxSessionAliveForCleanup = origAlive
	})
}

// fileExists is a small helper for the worker tests.
func workerFileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// TestKillAgentsForTask_HappyPath.
func TestKillAgentsForTask_HappyPath(t *testing.T) {
	setupFleetHome(t)
	worker := seedWorkerAgent(t, "wrk00001", "rainier", "auth-fix")
	other := seedWorkerAgent(t, "wrk00002", "rainier", "other-task")

	stubWorkerTmux(t,
		func(string) error { return nil },
		func(string) (bool, error) { return false, nil },
	)

	var stderr bytes.Buffer
	res, err := KillAgentsForTask(WorkerCleanupOpts{
		Project: "rainier", TaskSlug: "auth-fix", Stderr: &stderr,
	})
	if err != nil {
		t.Fatalf("KillAgentsForTask: %v", err)
	}
	if res.Matched != 1 || res.Killed != 1 || res.Archived != 1 {
		t.Errorf("counts: matched=%d killed=%d archived=%d; want 1/1/1",
			res.Matched, res.Killed, res.Archived)
	}
	if len(res.IDs) != 1 || res.IDs[0] != worker.ID {
		t.Errorf("IDs = %v; want [%s]", res.IDs, worker.ID)
	}
	if livePath, _ := state.AgentPath(worker.ID); workerFileExists(livePath) {
		t.Errorf("worker live record still present")
	}
	if archPath, _ := state.AgentArchivePath(worker.ID); !workerFileExists(archPath) {
		t.Errorf("worker archive missing")
	}
	if otherPath, _ := state.AgentPath(other.ID); !workerFileExists(otherPath) {
		t.Errorf("non-matching worker %s archived (selector too broad)", other.ID)
	}
}

// TestKillAgentsForTask_KillFailsStillAlive_RefusesArchive — the
// headline regression test.
func TestKillAgentsForTask_KillFailsStillAlive_RefusesArchive(t *testing.T) {
	setupFleetHome(t)
	worker := seedWorkerAgent(t, "wrk-leak", "rainier", "stuck-task")

	stubWorkerTmux(t,
		func(string) error { return errors.New("simulated kill failure") },
		func(string) (bool, error) { return true, nil },
	)

	var stderr bytes.Buffer
	_, err := KillAgentsForTask(WorkerCleanupOpts{
		Project: "rainier", TaskSlug: "stuck-task", Stderr: &stderr,
	})
	if err == nil {
		t.Fatalf("expected error")
	}
	if !errors.Is(err, ErrWorkerCleanupFailed) {
		t.Errorf("not wrapping ErrWorkerCleanupFailed: %v", err)
	}
	if !strings.Contains(err.Error(), "still alive") {
		t.Errorf("error should mention 'still alive'; got %v", err)
	}
	livePath, _ := state.AgentPath(worker.ID)
	if !workerFileExists(livePath) {
		t.Errorf("worker live record removed despite live tmux (invariant violation)")
	}
	archPath, _ := state.AgentArchivePath(worker.ID)
	if workerFileExists(archPath) {
		t.Errorf("worker archived despite live tmux (invariant violation)")
	}
}

// TestKillAgentsForTask_KillFailsProbeAmbiguous_RefusesArchive.
func TestKillAgentsForTask_KillFailsProbeAmbiguous_RefusesArchive(t *testing.T) {
	setupFleetHome(t)
	worker := seedWorkerAgent(t, "wrk-amb", "rainier", "ambig-task")

	stubWorkerTmux(t,
		func(string) error { return errors.New("kill error") },
		func(string) (bool, error) { return false, errors.New("probe error") },
	)

	var stderr bytes.Buffer
	_, err := KillAgentsForTask(WorkerCleanupOpts{
		Project: "rainier", TaskSlug: "ambig-task", Stderr: &stderr,
	})
	if err == nil {
		t.Fatalf("expected error on ambiguous post-kill probe")
	}
	if !errors.Is(err, ErrWorkerCleanupFailed) {
		t.Errorf("not wrapping ErrWorkerCleanupFailed: %v", err)
	}
	livePath, _ := state.AgentPath(worker.ID)
	if !workerFileExists(livePath) {
		t.Errorf("worker live record removed despite ambiguous probe")
	}
}

// TestKillAgentsForTask_KeepSessionSkipsKillAndArchive.
// codex iter-1 [P1]: --keep-session preserves BOTH the tmux session
// AND the live agent record — operators use `fleet attach <id>` to
// reach the preserved session, so archiving the record would hide it.
func TestKillAgentsForTask_KeepSessionSkipsKillAndArchive(t *testing.T) {
	setupFleetHome(t)
	worker := seedWorkerAgent(t, "wrk-keep", "rainier", "keep-task")

	stubWorkerTmux(t,
		func(string) error {
			t.Errorf("Kill must not be called with KeepSession=true")
			return nil
		},
		func(string) (bool, error) { return true, nil },
	)

	res, err := KillAgentsForTask(WorkerCleanupOpts{
		Project: "rainier", TaskSlug: "keep-task", KeepSession: true,
		Stderr: io.Discard,
	})
	if err != nil {
		t.Fatalf("KillAgentsForTask: %v", err)
	}
	if res.Matched != 1 {
		t.Errorf("Matched = %d; want 1", res.Matched)
	}
	if res.Killed != 0 {
		t.Errorf("Killed = %d; want 0", res.Killed)
	}
	if res.Archived != 0 {
		t.Errorf("Archived = %d; want 0 with KeepSession (preserve record for fleet attach)", res.Archived)
	}
	// Record stays live so `fleet attach <id>` can find the preserved
	// tmux session.
	livePath, _ := state.AgentPath(worker.ID)
	if !workerFileExists(livePath) {
		t.Errorf("worker record archived with KeepSession=true (codex iter-1 P1 regression)")
	}
	archPath, _ := state.AgentArchivePath(worker.ID)
	if workerFileExists(archPath) {
		t.Errorf("worker archive present with KeepSession=true")
	}
}

// TestKillAgentsForTask_NoMatchingWorkers_NoOp.
func TestKillAgentsForTask_NoMatchingWorkers_NoOp(t *testing.T) {
	setupFleetHome(t)
	seedWorkerAgent(t, "wrk-other", "rainier", "different-task")

	stubWorkerTmux(t,
		func(string) error {
			t.Errorf("Kill must not be called when no workers match")
			return nil
		},
		func(string) (bool, error) { return false, nil },
	)

	res, err := KillAgentsForTask(WorkerCleanupOpts{
		Project: "rainier", TaskSlug: "no-such-task", Stderr: io.Discard,
	})
	if err != nil {
		t.Fatalf("KillAgentsForTask: %v", err)
	}
	if res.Matched != 0 {
		t.Errorf("Matched = %d; want 0", res.Matched)
	}
}

// TestKillAgentsForTask_EmptyTmuxSession_Skipped.
func TestKillAgentsForTask_EmptyTmuxSession_Skipped(t *testing.T) {
	setupFleetHome(t)
	legacy := agent.New("legacy01")
	legacy.TaskID = "legacy-task"
	legacy.Project = "rainier"
	legacy.TmuxSession = ""
	if err := legacy.Write(); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}
	stubWorkerTmux(t,
		func(string) error {
			t.Errorf("Kill must not be called for empty-session record")
			return nil
		},
		func(string) (bool, error) { return false, nil },
	)
	res, err := KillAgentsForTask(WorkerCleanupOpts{
		Project: "rainier", TaskSlug: "legacy-task", Stderr: io.Discard,
	})
	if err != nil {
		t.Fatalf("KillAgentsForTask: %v", err)
	}
	if res.Matched != 0 {
		t.Errorf("legacy empty-session record should not match (Matched=%d)", res.Matched)
	}
	livePath, _ := state.AgentPath(legacy.ID)
	if !workerFileExists(livePath) {
		t.Errorf("legacy record removed unexpectedly")
	}
}

// TestKillAgentsForTask_MalformedRecordWithMatch_RefusesArchive —
// codex iter-1 [P2] + iter-2 [P1]: an unreadable record alongside a
// matching live worker fails the gate. The bad record might be
// another match we couldn't parse.
func TestKillAgentsForTask_MalformedRecordWithMatch_RefusesArchive(t *testing.T) {
	fleetHome := setupFleetHome(t)
	// Seed a matching live worker AND a malformed record. Both
	// scenarios where ListStrict reports badIDs while we have a real
	// match must fail closed.
	seedWorkerAgent(t, "wrk-real", "rainier", "any-task")
	badPath := fleetHome + "/agents/badcoord.json"
	if err := os.WriteFile(badPath, []byte("not json"), 0o644); err != nil {
		t.Fatalf("seed bad record: %v", err)
	}

	stubWorkerTmux(t,
		func(string) error {
			t.Errorf("Kill must not be called when records are malformed alongside matches")
			return nil
		},
		func(string) (bool, error) { return false, nil },
	)
	_, err := KillAgentsForTask(WorkerCleanupOpts{
		Project: "rainier", TaskSlug: "any-task", Stderr: io.Discard,
	})
	if err == nil {
		t.Fatalf("expected error on malformed agent record + match")
	}
	if !errors.Is(err, ErrWorkerCleanupFailed) {
		t.Errorf("not wrapping ErrWorkerCleanupFailed: %v", err)
	}
	if !strings.Contains(err.Error(), "unreadable") {
		t.Errorf("error should mention 'unreadable'; got %v", err)
	}
}

// TestKillAgentsForTask_MalformedRecordNoMatch_WarnsAndProceeds —
// codex iter-2 [P1]: when no workers match this task, a global bad
// record must NOT block the transition. Surface as a stderr warning.
func TestKillAgentsForTask_MalformedRecordNoMatch_WarnsAndProceeds(t *testing.T) {
	fleetHome := setupFleetHome(t)
	// No matching workers; only a corrupt record.
	badPath := fleetHome + "/agents/unrelated.json"
	if err := os.WriteFile(badPath, []byte("not json"), 0o644); err != nil {
		t.Fatalf("seed bad record: %v", err)
	}
	stubWorkerTmux(t,
		func(string) error { t.Errorf("Kill must not be called on no-match path"); return nil },
		func(string) (bool, error) { return false, nil },
	)
	var stderr bytes.Buffer
	res, err := KillAgentsForTask(WorkerCleanupOpts{
		Project: "rainier", TaskSlug: "no-match-task", Stderr: &stderr,
	})
	if err != nil {
		t.Fatalf("expected success on no-match-bad-record path; got %v", err)
	}
	if res.Matched != 0 {
		t.Errorf("Matched = %d; want 0", res.Matched)
	}
	if !strings.Contains(stderr.String(), "unreadable") {
		t.Errorf("stderr should warn about unreadable records; got: %s", stderr.String())
	}
}

// TestKillAgentsForTask_SkipsCoordinatorRecord — codex iter-2 [P1].
// A task named "coord-<project>" must NOT match the project's
// coordinator agent. Kill must not be called on the coord.
func TestKillAgentsForTask_SkipsCoordinatorRecord(t *testing.T) {
	setupFleetHome(t)
	// Worker task happens to share the coord sentinel slug.
	const project = "rainier"
	const coordSlug = "coord-" + project
	// Coordinator agent — TaskID matches the sentinel pattern.
	coord := agent.New("coordagt")
	coord.TmuxSession = "fleet-coordagt"
	coord.Project = project
	coord.TaskID = coordSlug
	if err := coord.Write(); err != nil {
		t.Fatalf("seed coord: %v", err)
	}
	// Worker agent — TaskID happens to also equal the coord slug
	// (operator's questionable choice, but the gate must handle it).
	worker := agent.New("wrk-coll")
	worker.TmuxSession = "fleet-wrk-coll"
	worker.Project = project
	worker.TaskID = coordSlug
	if err := worker.Write(); err != nil {
		t.Fatalf("seed worker: %v", err)
	}
	killed := map[string]bool{}
	stubWorkerTmux(t,
		func(s string) error { killed[s] = true; return nil },
		func(string) (bool, error) { return false, nil },
	)
	_, err := KillAgentsForTask(WorkerCleanupOpts{
		Project: project, TaskSlug: coordSlug, Stderr: io.Discard,
	})
	// Both records share the slug — but the coord one must be
	// excluded. Result: no matches AND no work done. The "worker"
	// in this test was also classified as a coord because its
	// TaskID equals the sentinel. That's actually the safer policy:
	// the gate refuses to mutate ANY record whose TaskID looks like
	// the coord sentinel. Operators who run into this can `fleet
	// rm` directly.
	if err != nil {
		t.Fatalf("KillAgentsForTask: %v", err)
	}
	if killed[coord.TmuxSession] {
		t.Errorf("coord tmux session killed (P1 regression)")
	}
}

// TestKillAgentsForTask_InvalidInputs.
func TestKillAgentsForTask_InvalidInputs(t *testing.T) {
	setupFleetHome(t)
	cases := []struct {
		name string
		opts WorkerCleanupOpts
	}{
		{"empty project", WorkerCleanupOpts{TaskSlug: "x"}},
		{"empty slug", WorkerCleanupOpts{Project: "y"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := KillAgentsForTask(tc.opts)
			if err == nil {
				t.Errorf("expected error for %s", tc.name)
			}
			if !errors.Is(err, ErrWorkerCleanupFailed) {
				t.Errorf("not wrapping ErrWorkerCleanupFailed: %v", err)
			}
		})
	}
}

// TestKillAgentsForTask_KillRaceSessionGone_LogsAndArchives.
func TestKillAgentsForTask_KillRaceSessionGone_LogsAndArchives(t *testing.T) {
	setupFleetHome(t)
	worker := seedWorkerAgent(t, "wrk-race", "rainier", "race-task")

	killErr := errors.New("simulated kill race error")
	stubWorkerTmux(t,
		func(string) error { return killErr },
		func(string) (bool, error) { return false, nil },
	)

	var stderr bytes.Buffer
	res, err := KillAgentsForTask(WorkerCleanupOpts{
		Project: "rainier", TaskSlug: "race-task", Stderr: &stderr,
	})
	if err != nil {
		t.Fatalf("expected success on kill-race-but-gone; got %v", err)
	}
	if res.Killed != 1 {
		t.Errorf("Killed = %d; want 1", res.Killed)
	}
	if res.Archived != 1 {
		t.Errorf("Archived = %d; want 1", res.Archived)
	}
	if !strings.Contains(stderr.String(), "session is gone") {
		t.Errorf("stderr should note race; got: %s", stderr.String())
	}
	livePath, _ := state.AgentPath(worker.ID)
	if workerFileExists(livePath) {
		t.Errorf("worker record still live")
	}
}
