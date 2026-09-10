//go:build linux || darwin

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/coordlock"
	"github.com/edisonshen/fleet/internal/handoff"
	"github.com/edisonshen/fleet/internal/tasks"
)

// A live coord flock held by a process this one does NOT descend from (the
// test process itself holds it; handoff-write proves ownership for its
// parent) is the successor-took-over case: the doc may land, the queue
// file must not.
func TestHandoffWrite_CoordFencedBeforePublish(t *testing.T) {
	noGH(t)
	home := setupFleetHome(t)
	rec := seedLiveRecord(t, "c0ffee02", "coord-myproj", "myproj")
	seedCoordProject(t, home, "myproj", rec.ID)

	lease, acquired, err := coordlock.AcquireLease("myproj", "successor")
	if err != nil || !acquired {
		t.Fatalf("acquire lease: acquired=%v err=%v", acquired, err)
	}
	defer lease.Release()

	tasksPath := filepath.Join(home, "projects", "myproj", "tasks.md")
	inflight := &tasks.Task{Slug: "e2e-login-1234", Status: tasks.StatusInProgress, Priority: "P1",
		Created: time.Now(), Updated: time.Now(), SpawnedBy: "user", Spec: "login e2e"}
	if err := tasks.Write(tasksPath, &tasks.File{Schema: tasks.SchemaVersion, Tasks: []*tasks.Task{inflight}}); err != nil {
		t.Fatalf("write tasks.md: %v", err)
	}
	before, _ := os.ReadFile(tasksPath)

	_, stderr, err := runWrite(t, &handoffWriteOpts{agentID: rec.ID, typ: handoff.TypeAutoRed}, "waiting on e2e\n")
	if err == nil || !strings.Contains(err.Error(), "FENCED") {
		t.Fatalf("fenced coord must refuse to publish, got err=%v stderr=%s", err, stderr)
	}
	queued, _ := filepath.Glob(filepath.Join(home, "queue", "spawn-fresh-*.json"))
	if len(queued) != 0 {
		t.Fatalf("fenced coord published a queue file: %v", queued)
	}
	// The unreferenced doc must leave no trace in task history either.
	after, _ := os.ReadFile(tasksPath)
	if string(after) != string(before) {
		t.Fatalf("fenced coord mutated tasks.md:\n%s", after)
	}
}

// Workers do not hold the coord lease, so a held flock never fences a
// worker handoff in the same project.
func TestHandoffWrite_WorkerNotFencedByCoordLease(t *testing.T) {
	noGH(t)
	home := setupFleetHome(t)
	rec := seedLiveRecord(t, "0000bef0", "e2e-login-1234", "myproj")

	lease, acquired, err := coordlock.AcquireLease("myproj", "successor")
	if err != nil || !acquired {
		t.Fatalf("acquire lease: acquired=%v err=%v", acquired, err)
	}
	defer lease.Release()

	res, stderr, err := runWrite(t, &handoffWriteOpts{agentID: rec.ID, typ: handoff.TypeAutoRed}, "")
	if err != nil {
		t.Fatalf("worker handoff-write: %v\nstderr: %s", err, stderr)
	}
	if _, err := os.Stat(res.QueuePath); err != nil {
		t.Fatalf("worker queue file missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "queue", "spawn-fresh-"+rec.ID+".json")); err != nil {
		t.Fatalf("queue file not at canonical path: %v", err)
	}
}
