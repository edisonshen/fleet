package tui

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
)

// TestRefresh_CoalescesWhileInFlight: a tick or fs event arriving while a
// refresh is running must not start another one, only mark it dirty.
func TestRefresh_CoalescesWhileInFlight(t *testing.T) {
	m := New("test")
	if !m.refreshInFlight {
		t.Fatal("New must mark the Init refresh in flight")
	}
	m.refreshInFlight = false

	updated, cmd := m.Update(fsEventMsg{})
	m = updated.(Model)
	if cmd == nil || !m.refreshInFlight {
		t.Fatalf("first fsEvent must start a refresh; cmd=%v inFlight=%v", cmd != nil, m.refreshInFlight)
	}

	for i := 0; i < 5; i++ {
		updated, cmd = m.Update(fsEventMsg{})
		m = updated.(Model)
		if cmd != nil {
			t.Fatalf("fsEvent %d during in-flight refresh must not start another", i)
		}
	}
	if !m.refreshDirty {
		t.Fatal("events during an in-flight refresh must set refreshDirty")
	}

	// The tick still reschedules itself but does not add a refresh.
	updated, cmd = m.Update(tickMsg(time.Now()))
	m = updated.(Model)
	if cmd == nil {
		t.Fatal("tick must always reschedule the next tick")
	}
	if !m.refreshInFlight || !m.refreshDirty {
		t.Fatal("tick during in-flight refresh must leave inFlight+dirty set")
	}

	// Landing the refresh runs exactly one follow-up because it was dirty.
	updated, cmd = m.Update(refreshMsg{dash: dashboardMsg{snap: &Snapshot{}}})
	m = updated.(Model)
	if cmd == nil || !m.refreshInFlight || m.refreshDirty {
		t.Fatalf("dirty refresh landing must start one follow-up; cmd=%v inFlight=%v dirty=%v",
			cmd != nil, m.refreshInFlight, m.refreshDirty)
	}

	// A clean landing goes idle.
	updated, cmd = m.Update(refreshMsg{dash: dashboardMsg{snap: &Snapshot{}}})
	m = updated.(Model)
	if cmd != nil || m.refreshInFlight {
		t.Fatalf("clean refresh landing must go idle; cmd=%v inFlight=%v", cmd != nil, m.refreshInFlight)
	}
}

// TestRefresh_StartupTickCoalesces: a tick or fs event arriving before
// the Init refresh lands must not start a second scan.
func TestRefresh_StartupTickCoalesces(t *testing.T) {
	m := New("test")
	updated, _ := m.Update(tickMsg(time.Now()))
	m = updated.(Model)
	updated, cmd := m.Update(fsEventMsg{})
	m = updated.(Model)
	if cmd != nil || !m.refreshDirty {
		t.Fatalf("startup events must coalesce; cmd=%v dirty=%v", cmd != nil, m.refreshDirty)
	}
}

// TestScanDashboard_MissingProjectsDirHasCoordCache: a completed scan
// always yields a non-nil Coord map so renders never fall back to live
// project-tree checks.
func TestScanDashboard_MissingProjectsDirHasCoordCache(t *testing.T) {
	home := withFleetHome(t)
	if err := os.RemoveAll(home); err != nil {
		t.Fatal(err)
	}
	snap := scanDashboard(time.Now())
	if snap.Err != nil || snap.Coord == nil {
		t.Fatalf("want nil err and non-nil Coord; got err=%v coord=%v", snap.Err, snap.Coord)
	}
}

// TestRefreshMsg_AppliesBothPayloads: the coalesced message must update
// records and the dashboard snapshot exactly like the two standalone
// messages do.
func TestRefreshMsg_AppliesBothPayloads(t *testing.T) {
	m := New("test")
	m.refreshInFlight = true
	rec := sampleAgent("agent01")
	snap := &Snapshot{Projects: []*ProjectRow{{Name: "demo", RepoSlug: "demo"}}}
	updated, _ := m.Update(refreshMsg{
		agents: agentsMsg{records: []*agent.Record{rec}, alive: map[string]bool{"agent01": true}, groupKeys: map[string]string{}},
		dash:   dashboardMsg{snap: snap},
	})
	m = updated.(Model)
	if len(m.records) != 1 || m.records[0].ID != "agent01" {
		t.Fatalf("records not applied: %+v", m.records)
	}
	if m.dashboard != snap {
		t.Fatal("dashboard snapshot not applied")
	}
	if !m.aliveByID["agent01"] {
		t.Fatal("alive cache not applied")
	}
}

// TestLoadAgentsCmd_UsesSessionListing: one `tmux ls` classifies every
// record; no per-record probe runs when the listing succeeds.
func TestLoadAgentsCmd_UsesSessionListing(t *testing.T) {
	home := withFleetHome(t)
	if err := os.MkdirAll(filepath.Join(filepath.Dir(home), "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"agent01", "agent02"} {
		rec := agent.New(id)
		rec.TmuxSession = "fleet-" + id
		rec.SpawnedAt = time.Now().UTC()
		if err := rec.Write(); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	prevList, prevProbe := listSessionsFn, sessionProbeFn
	listSessionsFn = func() ([]string, error) { return []string{"fleet-agent02", "unrelated"}, nil }
	sessionProbeFn = func(string) (bool, error) {
		t.Fatal("per-record probe must not run when the listing succeeds")
		return false, nil
	}
	t.Cleanup(func() { listSessionsFn, sessionProbeFn = prevList, prevProbe })

	msg := loadAgentsCmd()().(agentsMsg)
	if msg.err != nil {
		t.Fatal(msg.err)
	}
	if v, ok := msg.alive["agent01"]; !ok || v {
		t.Errorf("agent01 absent from listing must be cached dead; got %v/%v", v, ok)
	}
	if !msg.alive["agent02"] {
		t.Error("agent02 present in listing must be cached alive")
	}
}

// TestLoadAgentsCmd_ListingErrorFallsBackToProbe: a failed listing must
// not mark anything dead; the tristate per-record probe decides.
func TestLoadAgentsCmd_ListingErrorFallsBackToProbe(t *testing.T) {
	home := withFleetHome(t)
	if err := os.MkdirAll(filepath.Join(filepath.Dir(home), "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	rec := agent.New("agent01")
	rec.TmuxSession = "fleet-agent01"
	rec.SpawnedAt = time.Now().UTC()
	if err := rec.Write(); err != nil {
		t.Fatal(err)
	}
	prevList, prevProbe := listSessionsFn, sessionProbeFn
	listSessionsFn = func() ([]string, error) { return nil, errors.New("tmux wedged") }
	sessionProbeFn = func(string) (bool, error) { return false, errors.New("tmux wedged") }
	t.Cleanup(func() { listSessionsFn, sessionProbeFn = prevList, prevProbe })

	msg := loadAgentsCmd()().(agentsMsg)
	if _, present := msg.alive["agent01"]; present {
		t.Errorf("ambiguous probe must leave the alive entry absent; got %v", msg.alive["agent01"])
	}
}

// TestRender_NoLeaseOrTmuxIOWithCoordCache: once a Snapshot carries the
// scan-time Coord cache, View(), row building and key alignment must
// not read the lease or fork tmux.
func TestRender_NoLeaseOrTmuxIOWithCoordCache(t *testing.T) {
	withFleetHome(t)
	now := time.Now()
	coord := &agent.Record{ID: "cccc0001", Project: "demo", TaskID: "coord-demo", IsCoord: true, TmuxSession: "fleet-cccc0001", SpawnedAt: now}

	prevID, prevAlive, prevProbe, prevOrAlive, prevTree :=
		coordSpawnIdentityFn, sessionAliveFn, sessionProbeFn, sessionProbeOrAliveFn, projectTreeExistsFn
	fail := func(what string) {
		t.Helper()
		t.Errorf("%s called on the render path with a populated Coord cache", what)
	}
	coordSpawnIdentityFn = func(string) string { fail("coordSpawnIdentityFn"); return "" }
	sessionAliveFn = func(string) bool { fail("sessionAliveFn"); return true }
	sessionProbeFn = func(string) (bool, error) { fail("sessionProbeFn"); return true, nil }
	sessionProbeOrAliveFn = func(string) bool { fail("sessionProbeOrAliveFn"); return true }
	projectTreeExistsFn = func(string) bool { fail("projectTreeExistsFn"); return true }
	t.Cleanup(func() {
		coordSpawnIdentityFn, sessionAliveFn, sessionProbeFn, sessionProbeOrAliveFn, projectTreeExistsFn =
			prevID, prevAlive, prevProbe, prevOrAlive, prevTree
	})

	m := New("test")
	m.width = 140
	m.height = 30
	m.records = []*agent.Record{coord}
	m.aliveByID = map[string]bool{"cccc0001": true}
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{{Name: "demo", RepoSlug: "demo"}, {Name: "other", RepoSlug: "other"}},
		Coord: map[string]coordProbe{
			"demo":  {identity: "cccc0001", session: "fleet-cccc0001", alive: true},
			"other": {},
		},
		LoadedAt: now,
	}

	projects := m.unifiedProjects()
	var bound string
	for _, p := range projects {
		if p.Name == "demo" {
			bound = p.CoordID
		}
	}
	if bound != "cccc0001" {
		t.Errorf("coord must bind from the cached probe; CoordID=%q", bound)
	}

	_ = m.View()
	updated, _ := m.Update(keyMsg("down"))
	_ = updated.(Model).View()
}

// TestFindCoordByTaskID_CachedDeadSessionDoesNotBind: the cached liveness
// verdict is honoured — a dead coord session never promotes.
func TestFindCoordByTaskID_CachedDeadSessionDoesNotBind(t *testing.T) {
	coord := &agent.Record{ID: "cccc0001", Project: "demo", TaskID: "coord-demo", TmuxSession: "fleet-cccc0001"}
	probe := func(string) (coordProbe, bool) {
		return coordProbe{identity: "cccc0001", session: "fleet-cccc0001", alive: false}, true
	}
	if got := findCoordByTaskID([]*agent.Record{coord}, "demo", probe); got != nil {
		t.Errorf("dead cached session must not bind; got %s", got.ID)
	}
	missingTree := func(string) (coordProbe, bool) { return coordProbe{}, false }
	if got := findCoordByTaskID([]*agent.Record{coord}, "demo", missingTree); got != nil {
		t.Errorf("missing project tree must not bind; got %s", got.ID)
	}
}
