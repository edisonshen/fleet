//go:build linux || darwin

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edisonshen/fleet/internal/coordlock"
	"github.com/edisonshen/fleet/internal/handoff"
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

	_, stderr, err := runWrite(t, &handoffWriteOpts{agentID: rec.ID, typ: handoff.TypeAutoRed}, "waiting on e2e\n")
	if err == nil || !strings.Contains(err.Error(), "FENCED") {
		t.Fatalf("fenced coord must refuse to publish, got err=%v stderr=%s", err, stderr)
	}
	queued, _ := filepath.Glob(filepath.Join(home, "queue", "spawn-fresh-*.json"))
	if len(queued) != 0 {
		t.Fatalf("fenced coord published a queue file: %v", queued)
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
