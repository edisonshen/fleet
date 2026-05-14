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

// withInjectedSessionAliveProbe swaps the package-level
// sessionAliveProbe for the test's duration so the cap path's
// swap-vs-+1 decision is deterministic regardless of real tmux
// state. Matches tmux.SessionAlive's tristate contract:
//
//	(alive=true,  err=nil) — session is alive
//	(alive=false, err=nil) — definitively dead
//	(alive=false, err!=nil) — probe ambiguous (treated as alive)
func withInjectedSessionAliveProbe(t *testing.T, fn func(string) (bool, error)) {
	t.Helper()
	prev := sessionAliveProbe
	sessionAliveProbe = fn
	t.Cleanup(func() { sessionAliveProbe = prev })
}

// TestSpawnAndRetire_SessionCapRefusal pins the auto-drain path's
// FLEET_MAX_SESSIONS gate. spawnAndRetire is net-zero by intent
// (spawn new + retire old), so the cap compares the CURRENT total
// against max (not total+1). When the current total already EXCEEDS
// the cap (e.g. orphan leak built up past the ceiling), the cap
// refuses, preserves the queue file, and surfaces a message
// pointing at the prune + rm escape valves.
//
// Builds the smallest record/queue state needed to drive spawnAndRetire
// into the cap-check path BEFORE it tries to call spawn.Spawn. The
// cap check runs first in spawnAndRetire, so we never need a real
// tmux/claude. Test FLEET_HOME is isolated via setupFleetHome.
func TestSpawnAndRetire_SessionCapRefusal(t *testing.T) {
	tmp := setupFleetHome(t)
	_ = tmp
	t.Setenv("FLEET_MAX_SESSIONS", "2")

	// Inject a fake list that returns 3 fleet-* sessions — over-cap
	// even with the net-zero offset (count=3, max=2). Old session
	// alive → projected = 3 > 2 → refuse.
	withInjectedSessionListProbe(t, func() ([]string, error) {
		return []string{"fleet-aaa", "fleet-bbb", "fleet-ccc"}, nil
	})
	// Old session alive (in the count) — net-zero swap accounting.
	withInjectedSessionAliveProbe(t, func(string) (bool, error) {
		return true, nil
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
		"FLEET_MAX_SESSIONS=2",
		"prune-orphan-tmux",
		"fleet rm",
		queuePath,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err missing %q\nfull: %s", want, err.Error())
		}
	}
}

// TestSpawnAndRetire_NetZeroAtCapAllowed regresses codex iter-2 P1
// + iter-4 P2: when the old session is alive (in the tmux count),
// the auto-drain handoff is net-zero — at exactly N=max it must
// proceed. We confirm spawnAndRetire moves past the cap check by
// observing the legacy-record-rejection error from the NEXT step
// (oldRec.Cwd == "" is rejected explicitly).
func TestSpawnAndRetire_NetZeroAtCapAllowed(t *testing.T) {
	setupFleetHome(t)
	t.Setenv("FLEET_MAX_SESSIONS", "3")
	// Exactly at cap — 3 live fleet-* sessions in the list.
	withInjectedSessionListProbe(t, func() ([]string, error) {
		return []string{"fleet-aaa", "fleet-bbb", "fleet-ccc"}, nil
	})
	for _, id := range []string{"aaa", "bbb", "ccc"} {
		seedAgentRecord(t, id)
	}
	// Old session alive → net-zero accounting: projected = 3 ≤ 3,
	// allowed.
	withInjectedSessionAliveProbe(t, func(string) (bool, error) {
		return true, nil
	})

	// Cwd intentionally empty so spawnAndRetire reaches the next
	// rejection AFTER passing the cap check.
	oldRec := &agent.Record{ID: "olda", TmuxSession: "fleet-olda", Command: []string{"x"}}
	req := queue.SpawnFresh{OldAgentID: "olda", NewAgentID: "newa"}
	var stdout, stderr bytes.Buffer
	err := spawnAndRetire(req, "/tmp/q.json", oldRec, 0, &stdout, &stderr)
	if err == nil {
		t.Fatalf("expected legacy-record rejection, got nil")
	}
	if strings.Contains(err.Error(), "refusing to spawn") {
		t.Fatalf("cap should NOT have blocked at-cap with old alive; err=%v", err)
	}
	if !strings.Contains(err.Error(), "no stored cwd") {
		t.Fatalf("expected legacy-record rejection downstream; got: %v", err)
	}
}

// TestSpawnAndRetire_DeadOldSessionAtCapRefused regresses codex
// iter-4 P2: when oldRec.TmuxSession has already exited (crash-
// after-queue, Resume reaches spawnAndRetire on the fresh-spawn
// branch), the new spawn is net +1 (not net 0). spawnAndRetire must
// use projected = total+1 in this case so the auto-drain can't
// push the host past the configured cap.
func TestSpawnAndRetire_DeadOldSessionAtCapRefused(t *testing.T) {
	setupFleetHome(t)
	t.Setenv("FLEET_MAX_SESSIONS", "3")
	// 3 unrelated fleet-* sessions in the count. old's session is
	// NOT in the list (already exited).
	withInjectedSessionListProbe(t, func() ([]string, error) {
		return []string{"fleet-aaa", "fleet-bbb", "fleet-ccc"}, nil
	})
	for _, id := range []string{"aaa", "bbb", "ccc"} {
		seedAgentRecord(t, id)
	}
	// Old session DEFINITIVELY dead (probe returns alive=false, nil).
	// Projected = total + 1 = 4 > 3 → refuse.
	withInjectedSessionAliveProbe(t, func(string) (bool, error) {
		return false, nil
	})

	oldRec := &agent.Record{
		ID:          "olda",
		TmuxSession: "fleet-olda-dead",
		Cwd:         "/tmp",
		Command:     []string{"x"},
	}
	req := queue.SpawnFresh{
		OldAgentID: "olda",
		NewAgentID: "newa",
		HandoffDoc: "/tmp/stub.md",
	}
	var stdout, stderr bytes.Buffer
	err := spawnAndRetire(req, "/tmp/q.json", oldRec, 0, &stdout, &stderr)
	if err == nil {
		t.Fatalf("expected cap refusal when old session is dead, got nil")
	}
	if !strings.Contains(err.Error(), "refusing to spawn") {
		t.Fatalf("expected cap refusal message; got: %v", err)
	}
}

// TestSpawnAndRetire_CapApprovedSkipsCheck regresses codex iter-7
// P1: when the queue file's CapApproved flag is true (set by
// `fleet handoff` at step 4a, or by a prior drain pass after the
// initial check passed), spawnAndRetire must NOT re-check the cap.
// Otherwise an authorized in-flight handoff gets stranded if the
// cap state tightened between crash and retry. Auto-handoff queues
// (CapApproved=false) still gate normally.
func TestSpawnAndRetire_CapApprovedSkipsCheck(t *testing.T) {
	setupFleetHome(t)
	t.Setenv("FLEET_MAX_SESSIONS", "1")
	// Way over cap — 5 sessions, max=1. Without CapApproved this
	// would refuse.
	withInjectedSessionListProbe(t, func() ([]string, error) {
		return []string{"fleet-a", "fleet-b", "fleet-c", "fleet-d", "fleet-e"}, nil
	})
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		seedAgentRecord(t, id)
	}

	oldRec := &agent.Record{ID: "olda", TmuxSession: "fleet-olda", Command: []string{"x"}}
	// CapApproved=true → skip cap, fall through to next rejection
	// (oldRec.Cwd == "" — legacy record).
	req := queue.SpawnFresh{OldAgentID: "olda", NewAgentID: "newa", CapApproved: true}
	var stdout, stderr bytes.Buffer
	err := spawnAndRetire(req, "/tmp/q.json", oldRec, 0, &stdout, &stderr)
	if err == nil {
		t.Fatalf("expected legacy-record rejection, got nil")
	}
	if strings.Contains(err.Error(), "refusing to spawn") {
		t.Fatalf("CapApproved=true should bypass cap; err=%v", err)
	}
	if !strings.Contains(err.Error(), "no stored cwd") {
		t.Fatalf("expected legacy-record rejection downstream; got: %v", err)
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
