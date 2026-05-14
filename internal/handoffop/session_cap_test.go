package handoffop

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/queue"
)

// withInjectedSessionListProbe swaps the package-level
// sessionListProbe for the test's duration.
func withInjectedSessionListProbe(t *testing.T, fn func() ([]string, error)) {
	t.Helper()
	prev := sessionListProbe
	sessionListProbe = fn
	t.Cleanup(func() { sessionListProbe = prev })
}

// TestSpawnAndRetire_SessionCapRefusal pins the auto-drain path's
// FLEET_MAX_SESSIONS gate. When the count of fleet-* tmux sessions is
// already at or above the cap, spawnAndRetire must refuse, preserve
// the queue file, and surface a message pointing at the prune + rm
// escape valves.
//
// Builds the smallest record/queue state needed to drive spawnAndRetire
// into the cap-check path BEFORE it tries to call spawn.Spawn. The
// cap check runs first in spawnAndRetire, so we never need a real
// tmux/claude. Test FLEET_HOME is isolated via setupFleetHome.
func TestSpawnAndRetire_SessionCapRefusal(t *testing.T) {
	tmp := setupFleetHome(t)
	_ = tmp
	t.Setenv("FLEET_MAX_SESSIONS", "3")

	// Inject a fake list that returns 3 fleet-* sessions — at-cap
	// regardless of orphan/live split (we mark them all live so the
	// total is 3 but orphan=0).
	withInjectedSessionListProbe(t, func() ([]string, error) {
		return []string{"fleet-aaa", "fleet-bbb", "fleet-ccc"}, nil
	})
	// state.LiveAgentRecordExists is what the cap counter calls. It
	// reads from ~/.fleet/agents/. We seed three live records so
	// the counter classifies them as live.
	for _, id := range []string{"aaa", "bbb", "ccc"} {
		seedAgentRecord(t, id)
	}

	// Write a stub agent record for the old agent + queue file. The
	// queue file's path is what the refusal error must mention as
	// preserved-for-retry.
	oldID := "olda1234"
	seedAgentRecord(t, oldID)
	req := queue.SpawnFresh{
		OldAgentID: oldID,
		NewAgentID: "newb5678",
		NewSession: "fleet-newb5678",
		HandoffDoc: filepath.Join(tmp, "handoffs", "stub.md"),
	}
	queuePath, err := queue.WriteSpawnFresh(req)
	if err != nil {
		t.Fatalf("WriteSpawnFresh: %v", err)
	}

	oldRec := &agent.Record{
		ID:      oldID,
		Cwd:     tmp,
		Command: []string{"echo", "ok"},
	}

	var stdout, stderr bytes.Buffer
	err = spawnAndRetire(req, queuePath, oldRec, 0, &stdout, &stderr)
	if err == nil {
		t.Fatalf("expected cap refusal, got nil")
	}
	for _, want := range []string{
		"refusing to spawn",
		"FLEET_MAX_SESSIONS=3",
		"prune-orphan-tmux",
		"fleet rm",
		queuePath,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err missing %q\nfull: %s", want, err.Error())
		}
	}
}

// TestSpawnAndRetire_SessionCapProbeFailureProceeds confirms the
// best-effort semantic: when the tmux list probe itself fails, the
// cap check logs to stderr and falls through (rather than blocking
// the auto-drain on a transient tmux outage).
//
// We can't drive the full happy path without real tmux, but we can
// confirm spawnAndRetire moves past the cap check on probe failure
// by observing the legacy-record-rejection error from the NEXT step
// (oldRec.Cwd == "" is rejected explicitly).
func TestSpawnAndRetire_SessionCapProbeFailureProceeds(t *testing.T) {
	setupFleetHome(t)
	t.Setenv("FLEET_MAX_SESSIONS", "1")

	withInjectedSessionListProbe(t, func() ([]string, error) {
		return nil, errProbeFail
	})

	// Cwd intentionally empty so spawnAndRetire reaches the next
	// rejection. If the cap check had blocked, we'd see its message
	// instead.
	oldRec := &agent.Record{ID: "olda", Command: []string{"x"}}
	req := queue.SpawnFresh{OldAgentID: "olda", NewAgentID: "newa"}
	var stdout, stderr bytes.Buffer
	err := spawnAndRetire(req, "/tmp/q.json", oldRec, 0, &stdout, &stderr)
	if err == nil {
		t.Fatalf("expected error from downstream check, got nil")
	}
	if strings.Contains(err.Error(), "refusing to spawn") {
		t.Fatalf("cap should NOT have blocked on probe failure; err=%v", err)
	}
	if !strings.Contains(err.Error(), "no stored cwd") {
		t.Fatalf("expected legacy-record rejection, got: %v", err)
	}
	if !strings.Contains(stderr.String(), "could not enumerate") {
		t.Errorf("expected probe-fail warning, got %q", stderr.String())
	}
}

// errProbeFail is a sentinel for the probe-failure test.
var errProbeFail = errors.New("tmux: command not found")

// seedAgentRecord writes a minimal live state.json so
// state.LiveAgentRecordExists returns true for the id. Uses
// agent.NewID's underlying file shape (see agent/record.go).
func seedAgentRecord(t *testing.T, id string) {
	t.Helper()
	rec := &agent.Record{
		ID:          id,
		TaskID:      "stub",
		Project:     "stub",
		TmuxSession: "fleet-" + id,
		SpawnedAt:   time.Now().UTC(),
	}
	if err := rec.Write(); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}
