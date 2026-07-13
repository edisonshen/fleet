package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/gc"
)

// TestReap_RealReconcile_WorkerArchivedCoordSurfaced (B5) drives the REAL
// gc reconcile over a dead non-coord worker record + a WELL-FORMED dead
// coord record (Project matches TaskID, IsCoord=true), then renders. The
// worker record is archived (gone from disk + the agents list); the dead
// coord is surfaced-only (kept on disk + still rendered in the agents
// block) so --coord-spawn recovery holds.
func TestReap_RealReconcile_WorkerArchivedCoordSurfaced(t *testing.T) {
	// Scope the reconcile's OrphanTmux scan to an empty temp dir so it does
	// not walk real /tmp and exec 'tmux -S <sock> ls' on every stray socket
	// (the PR #232 CI hang). This test's intent is the orphan-AGENTS reap.
	t.Setenv("FLEET_GC_SCAN_DIR", t.TempDir())
	pdir := withFleetHome(t)
	// Archive is where r.Archive() moves reaped records; seed both dirs.
	if err := os.MkdirAll(filepath.Join(filepath.Dir(pdir), "agents", "archive"), 0o755); err != nil {
		t.Fatalf("mkdir agents/archive: %v", err)
	}
	now := time.Now()

	worker := agent.New("wkr00001")
	worker.Project = "spark"
	worker.TaskID = "fix-bug"
	worker.TmuxSession = "fleet-wkr00001"
	if err := worker.Write(); err != nil {
		t.Fatalf("write worker: %v", err)
	}
	coord := agent.New("crd00001")
	coord.Project = "spark" // well-formed: matches "coord-spark"
	coord.TaskID = "coord-spark"
	coord.IsCoord = true
	coord.TmuxSession = "fleet-crd00001"
	if err := coord.Write(); err != nil {
		t.Fatalf("write coord: %v", err)
	}

	// REAL reconcile: agent-backed List/Archive, every session gone.
	deps := gc.Deps{
		Now:          time.Now,
		ListAgents:   agent.List,
		ArchiveAgent: func(r *agent.Record) error { return r.Archive() },
		SessionAlive: func(string) (bool, error) { return false, nil },
	}
	report, err := gc.Reconcile(gc.Options{Apply: true, Kinds: []gc.Kind{gc.KindOrphanAgents}}, deps)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	assertVerb := func(id string, want gc.Verb) {
		t.Helper()
		for _, a := range report.Actions {
			if a.Kind == gc.KindOrphanAgents && a.Target == id {
				if a.Verb != want {
					t.Errorf("%s verb = %q; want %q", id, a.Verb, want)
				}
				return
			}
		}
		t.Errorf("no orphan-agents action for %s; got %+v", id, report.Actions)
	}
	assertVerb("wkr00001", gc.VerbArchived)     // dead worker archived
	assertVerb("crd00001", gc.VerbWouldArchive) // dead coord surfaced, not archived

	// Surviving on-disk set: coord kept, worker gone.
	live, err := agent.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	ids := map[string]bool{}
	for _, r := range live {
		ids[r.ID] = true
	}
	if ids["wkr00001"] {
		t.Errorf("worker record must be archived (gone from live set); got %v", ids)
	}
	if !ids["crd00001"] {
		t.Fatalf("dead coord record must survive (surfaced, not archived); got %v", ids)
	}

	// Render the surviving records: coord in the agents block, worker gone.
	m := New("test")
	m.width = 140
	m.height = 30
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{{Name: "spark", RepoSlug: "spark", Active: true}},
		LoadedAt: now,
	}
	m.records = live
	m.aliveByID = map[string]bool{"crd00001": true}
	out := m.View()
	if strings.Contains(out, "wkr00001") {
		t.Errorf("archived worker must not render; got:\n%s", out)
	}
	if !strings.Contains(out, "crd00001") {
		t.Errorf("surviving dead coord must still render; got:\n%s", out)
	}
}

// TestTUIReap_SeamSurfacesReapedIDs (B7) exercises the tick→cmd→msg seam
// synchronously via the tuiReapFn override (no sleeps): the reap result
// surfaces in the status flash, and the single-flight + cadence gates
// hold.
func TestTUIReap_SeamSurfacesReapedIDs(t *testing.T) {
	prev := tuiReapFn
	tuiReapFn = func() []string { return []string{"deadaaaa", "deadbbbb"} }
	t.Cleanup(func() { tuiReapFn = prev })

	// reapCmd wraps tuiReapFn's result into a tuiReapMsg.
	msg, ok := reapCmd()().(tuiReapMsg)
	if !ok {
		t.Fatalf("reapCmd did not produce a tuiReapMsg")
	}
	if got := strings.Join(msg.reapedIDs, ","); got != "deadaaaa,deadbbbb" {
		t.Fatalf("reapCmd IDs = %q; want deadaaaa,deadbbbb", got)
	}

	// The reducer surfaces reaped IDs in the flash + clears in-flight.
	m := New("test")
	m.reapInFlight = true
	updated, _ := m.Update(msg)
	um := updated.(Model)
	if um.reapInFlight {
		t.Error("tuiReapMsg must clear reapInFlight (single-flight release)")
	}
	if um.flash == nil || !strings.Contains(um.flash.text, "deadaaaa") || !strings.Contains(um.flash.text, "reaped 2") {
		t.Fatalf("reaped IDs must surface in the status flash; got %+v", um.flash)
	}

	// Cadence + single-flight gate.
	gate := func(tickCount int, inFlight bool) bool {
		mm := New("test")
		mm.tickCount = tickCount
		mm.reapInFlight = inFlight
		return mm.shouldReapNow()
	}
	if !gate(reapEveryTicks, false) {
		t.Error("shouldReapNow must fire on a cadence tick")
	}
	if gate(reapEveryTicks-1, false) {
		t.Error("shouldReapNow must not fire off-cadence")
	}
	if gate(reapEveryTicks, true) {
		t.Error("shouldReapNow must not fire while a reap is in flight (single-flight)")
	}
	if gate(0, false) {
		t.Error("shouldReapNow must not fire at tickCount 0")
	}

	// The tick reducer arms single-flight when the cadence hits.
	mt := New("test")
	mt.tickCount = reapEveryTicks - 1
	uptd, cmd := mt.Update(tickMsg(time.Now()))
	if cmd == nil {
		t.Fatal("tick must still return a batch cmd")
	}
	if ut := uptd.(Model); ut.tickCount != reapEveryTicks || !ut.reapInFlight {
		t.Fatalf("cadence tick must bump tickCount to %d and arm reapInFlight; got tickCount=%d reapInFlight=%v",
			reapEveryTicks, ut.tickCount, ut.reapInFlight)
	}
}
