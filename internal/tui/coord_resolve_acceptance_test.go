package tui

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/coordlock"
	"github.com/edisonshen/fleet/internal/coordreconcile"
	"github.com/edisonshen/fleet/internal/coordreconcile/reconciletest"
	"github.com/edisonshen/fleet/internal/projectlookup"
	"github.com/edisonshen/fleet/internal/workers"
)

func installTUIResolveFromState(t *testing.T, st *reconciletest.State) *int {
	t.Helper()
	calls := 0
	prevResolve := tuiCoordResolveFn
	prevNewID := tuiCoordNewAgentIDFn
	tuiCoordResolveFn = func(project, agentID string) (coordreconcile.Verdict, error) {
		calls++
		return coordreconcile.Resolve(st.Deps(), project, agentID)
	}
	tuiCoordNewAgentIDFn = func() string { return "claimtui" }
	t.Cleanup(func() {
		tuiCoordResolveFn = prevResolve
		tuiCoordNewAgentIDFn = prevNewID
	})
	return &calls
}

func forceTUICurrentOwner(t *testing.T, ok bool, ownerID string) {
	t.Helper()
	prev := tuiCoordCurrentOwnerFn
	tuiCoordCurrentOwnerFn = func(string) (coordlock.Owner, bool) {
		if !ok {
			return coordlock.Owner{}, false
		}
		return coordlock.Owner{AgentID: ownerID, PID: 1}, true
	}
	t.Cleanup(func() { tuiCoordCurrentOwnerFn = prev })
}

func tuiCoordRecord(id, project string) *agent.Record {
	r := agent.New(id)
	r.Project = project
	r.TaskID = coordTaskID(project)
	r.TmuxSession = "fleet-" + id
	r.SpawnedAt = time.Now().UTC()
	return r
}

func installTUIOrphanTmux(t *testing.T, project, id string, killErr error) *[]string {
	t.Helper()
	root := os.Getenv("FLEET_HOME")
	if root == "" {
		t.Fatal("FLEET_HOME not set")
	}
	if err := os.MkdirAll(filepath.Join(root, "agents", "archive"), 0o755); err != nil {
		t.Fatalf("mkdir agents/archive: %v", err)
	}
	restorePL := projectlookup.SetTestStubs(nil, nil, func() ([]string, error) {
		return []string{"fleet-" + id}, nil
	})
	t.Cleanup(restorePL)
	restoreArchive := projectlookup.SetLoadArchiveStub(func(got string) (*agent.Record, error) {
		if got != id {
			return nil, errors.New("not found")
		}
		return &agent.Record{ID: id, Project: project, TaskID: coordTaskID(project)}, nil
	})
	t.Cleanup(restoreArchive)

	killed := []string{}
	prevKill := tuiKillTmuxSessionFn
	tuiKillTmuxSessionFn = func(session string) error {
		killed = append(killed, session)
		return killErr
	}
	t.Cleanup(func() { tuiKillTmuxSessionFn = prevKill })
	return &killed
}

func TestTA3_TUIConcurrentAttachPressesSingleSpawn(t *testing.T) {
	withFleetHome(t)
	seedProjectMeta(t, "demo", t.TempDir())
	stub := &stubFleetCmd{}
	stub.install(t)
	st := reconciletest.State{ClaimOK: true}
	resolveCalls := installTUIResolveFromState(t, &st)

	m := New("test")
	first, cmd := m.beginResolvedCoordAttach("demo", "project")
	if cmd == nil {
		t.Fatal("first [a] should start one coord")
	}
	if len(stub.calls) != 1 {
		t.Fatalf("dispatch calls after first [a] = %v, want one", stub.calls)
	}
	if st.ClaimCalls != 1 || st.ClaimedID != "claimtui" {
		t.Fatalf("first [a] should claim exactly one starting token; calls=%d id=%q", st.ClaimCalls, st.ClaimedID)
	}

	second, cmd2 := first.beginResolvedCoordAttach("demo", "project")
	if cmd2 != nil {
		t.Fatal("second concurrent [a] must be gated before a second spawn")
	}
	if *resolveCalls != 1 {
		t.Fatalf("second in-flight [a] must not re-resolve; calls=%d", *resolveCalls)
	}
	if len(stub.calls) != 1 {
		t.Fatalf("dispatch calls after second [a] = %v, want still one", stub.calls)
	}
	if second.flash == nil || !second.flash.isErr || !strings.Contains(second.flash.text, "in flight") {
		t.Fatalf("second [a] should surface in-flight recovery; got %+v", second.flash)
	}
}

func TestTA5_TUIUsesSharedResolveStateToAttachHolder(t *testing.T) {
	withFleetHome(t)
	(&stubSessionProbe{}).install(t)
	owner := tuiCoordRecord("holder05", "demo")
	st := reconciletest.State{LiveOwner: &coordlock.Owner{AgentID: owner.ID, PID: 505}}
	resolveCalls := installTUIResolveFromState(t, &st)
	forceTUICurrentOwner(t, true, owner.ID)

	m := New("test")
	m.records = []*agent.Record{owner}
	updated, cmd := m.beginResolvedCoordAttach("demo", "project")
	mm := updated
	if cmd == nil {
		t.Fatal("Attach verdict should quit into tmux")
	}
	if mm.pendingAttach != owner.TmuxSession {
		t.Fatalf("pendingAttach = %q, want %q", mm.pendingAttach, owner.TmuxSession)
	}
	if *resolveCalls != 1 {
		t.Fatalf("Resolve calls = %d, want 1", *resolveCalls)
	}
}

func TestTA1_TUIAttachGuardMatrixNoRespawn(t *testing.T) {
	cases := []struct {
		name        string
		ownerID     string
		seedRecord  bool
		deadSession bool
		wantFlash   string
	}{
		{
			name:      "empty owner id",
			ownerID:   "",
			wantFlash: "empty owner",
		},
		{
			name:      "owner record load failure",
			ownerID:   "missing1",
			wantFlash: "agent record is not readable",
		},
		{
			name:        "definitively dead session",
			ownerID:     "deadlv01",
			seedRecord:  true,
			deadSession: true,
			wantFlash:   "process-live in the lease but session fleet-deadlv01 is unreachable",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withFleetHome(t)
			stub := &stubFleetCmd{}
			stub.install(t)
			probe := &stubSessionProbe{}
			var records []*agent.Record
			if tc.seedRecord {
				owner := tuiCoordRecord(tc.ownerID, "demo")
				records = []*agent.Record{owner}
				if tc.deadSession {
					probe.dead = map[string]bool{owner.TmuxSession: true}
				}
			}
			probe.install(t)
			st := reconciletest.State{LiveOwner: &coordlock.Owner{AgentID: tc.ownerID, PID: 501}}
			resolveCalls := installTUIResolveFromState(t, &st)

			m := New("test")
			m.records = records
			updated, cmd := m.beginResolvedCoordAttach("demo", "project")
			mm := updated

			if mm.pendingAttach != "" {
				t.Fatalf("pendingAttach = %q; want empty so [a] does not attach beside a guarded owner", mm.pendingAttach)
			}
			if len(stub.calls) != 0 {
				t.Fatalf("guard branch must not spawn; dispatch calls=%v", stub.calls)
			}
			if *resolveCalls != 1 {
				t.Fatalf("Resolve calls = %d, want 1", *resolveCalls)
			}
			if mm.flash == nil || !mm.flash.isErr || !strings.Contains(mm.flash.text, tc.wantFlash) {
				t.Fatalf("flash = %+v; want error containing %q", mm.flash, tc.wantFlash)
			}
			if tc.deadSession && !strings.Contains(mm.flash.text, "Do not respawn beside it") {
				t.Fatalf("dead-session guard flash must forbid respawn; got %q", mm.flash.text)
			}
			_ = cmd
		})
	}
}

func TestTA6_TUISpawnKillsOrphanBeforeDispatch(t *testing.T) {
	withFleetHome(t)
	seedProjectMeta(t, "demo", t.TempDir())
	killed := installTUIOrphanTmux(t, "demo", "deadbeef", nil)
	stub := &stubFleetCmd{}
	stub.install(t)
	st := reconciletest.State{ClaimOK: true}
	installTUIResolveFromState(t, &st)

	m := New("test")
	updated, cmd := m.beginResolvedCoordAttach("demo", "project")
	if cmd == nil {
		t.Fatal("Spawn verdict should return a dispatch cmd")
	}
	if strings.Join(*killed, ",") != "fleet-deadbeef" {
		t.Fatalf("killed sessions = %v, want fleet-deadbeef", *killed)
	}
	if len(stub.calls) != 1 {
		t.Fatalf("dispatch calls = %v, want one after orphan kill", stub.calls)
	}
	if updated.flash == nil || !strings.Contains(updated.flash.text, "reaped stale deadbeef") {
		t.Fatalf("spawn should surface orphan reap before dispatch; flash=%+v", updated.flash)
	}
}

func TestTA6_TUIOrphanCleanupListStrictFaultsSurface(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, root string)
		want  string
	}{
		{
			name: "ListStrict read error",
			setup: func(t *testing.T, root string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(root, "agents"), []byte("not a dir"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: "agent.ListStrict failed",
		},
		{
			name: "corrupt agent record",
			setup: func(t *testing.T, root string) {
				t.Helper()
				dir := filepath.Join(root, "agents")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "badc0de1.json"), []byte("{not valid json"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: "unparseable agent record(s) [badc0de1]",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			projectsRoot := withFleetHome(t)
			root := filepath.Dir(projectsRoot)
			tc.setup(t, root)
			stub := &stubFleetCmd{}
			stub.install(t)
			st := reconciletest.State{ClaimOK: true}
			installTUIResolveFromState(t, &st)

			updated, cmd := New("test").beginResolvedCoordAttach("demo", "project")
			if len(stub.calls) != 0 {
				t.Fatalf("ListStrict fault must abort before dispatch; calls=%v", stub.calls)
			}
			if st.ClaimCalls != 1 || st.ClaimedID != "claimtui" {
				t.Fatalf("Spawn verdict must pre-claim before fallible cleanup; calls=%d id=%q", st.ClaimCalls, st.ClaimedID)
			}
			if updated.flash == nil || !updated.flash.isErr {
				t.Fatalf("ListStrict fault should flash an error; got %+v", updated.flash)
			}
			for _, want := range []string{"orphan cleanup failed", tc.want, "self-heal after its TTL"} {
				if !strings.Contains(updated.flash.text, want) {
					t.Fatalf("flash missing %q; got %q", want, updated.flash.text)
				}
			}
			_ = cmd
		})
	}
}

func TestTA7b_TUIWaitLoopAsyncBounded(t *testing.T) {
	withFleetHome(t)
	st := reconciletest.State{Handoff: &coordlock.Handoff{SuccessorID: "succwait"}}
	resolveCalls := installTUIResolveFromState(t, &st)

	prevAttempts := tuiCoordResolveMaxAttempts
	prevInterval := tuiCoordResolveWaitInterval
	prevTick := tuiCoordResolveWaitTickFn
	ticks := 0
	tuiCoordResolveMaxAttempts = 2
	tuiCoordResolveWaitInterval = time.Hour
	tuiCoordResolveWaitTickFn = func(d time.Duration, f func(time.Time) tea.Msg) tea.Cmd {
		if d != time.Hour {
			t.Fatalf("tick interval = %s, want injected interval", d)
		}
		ticks++
		return func() tea.Msg { return f(time.Unix(0, 0)) }
	}
	t.Cleanup(func() {
		tuiCoordResolveMaxAttempts = prevAttempts
		tuiCoordResolveWaitInterval = prevInterval
		tuiCoordResolveWaitTickFn = prevTick
	})

	m := New("test")
	first, cmd := m.beginResolvedCoordAttach("demo", "project")
	if cmd == nil {
		t.Fatal("initial Wait verdict should return a tick cmd")
	}
	msg1, ok := cmd().(coordResolveRetryMsg)
	if !ok {
		t.Fatalf("first cmd produced %T, want coordResolveRetryMsg", cmd())
	}
	updated1, cmd2 := first.Update(msg1)
	if cmd2 == nil {
		t.Fatal("second Wait verdict should return another tick cmd")
	}
	msg2, ok := cmd2().(coordResolveRetryMsg)
	if !ok {
		t.Fatalf("second cmd produced %T, want coordResolveRetryMsg", cmd2())
	}
	updated2, _ := updated1.(Model).Update(msg2)
	mm := updated2.(Model)
	if *resolveCalls != 3 {
		t.Fatalf("Resolve calls = %d, want initial + 2 retries", *resolveCalls)
	}
	if ticks != 2 {
		t.Fatalf("ticks = %d, want 2", ticks)
	}
	if mm.pendingAttach != "" {
		t.Fatalf("wait exhaustion must not attach; pending=%q", mm.pendingAttach)
	}
	if mm.flash == nil || !mm.flash.isErr || !strings.Contains(mm.flash.text, "bounded lease retries") {
		t.Fatalf("wait exhaustion should surface recovery flash; got %+v", mm.flash)
	}
}

func TestTA10_TUIWorkerRowResolvesCoordHolder(t *testing.T) {
	pdir := withFleetHome(t)
	seedTasks(t, pdir, "fleet", TaskCounts{Todo: 1})
	seedWorker(t, pdir, "fleet", "do-x-1a2b", workers.State{Phase: workers.PhaseTDDGreen})
	(&stubSessionProbe{}).install(t)
	owner := tuiCoordRecord("holder10", "fleet")
	st := reconciletest.State{LiveOwner: &coordlock.Owner{AgentID: owner.ID, PID: 1010}}
	resolveCalls := installTUIResolveFromState(t, &st)
	forceTUICurrentOwner(t, true, owner.ID)

	m := New("test")
	m.width = 130
	m.height = 30
	m.records = []*agent.Record{owner}
	m.dashboard = scanDashboard(time.Now())
	for i, row := range m.dashboardRows() {
		if row.kind == rowWorker && row.worker != nil && row.worker.Slug == "do-x-1a2b" {
			m.dashCursor = i
			break
		}
	}

	updated, cmd := m.Update(keyMsg("a"))
	mm := updated.(Model)
	if cmd == nil {
		t.Fatal("[a] on worker should quit into the resolved coord")
	}
	if mm.pendingAttach != owner.TmuxSession {
		t.Fatalf("pendingAttach = %q, want %q", mm.pendingAttach, owner.TmuxSession)
	}
	if *resolveCalls != 1 {
		t.Fatalf("worker row must route through Resolve exactly once; calls=%d", *resolveCalls)
	}
}

func TestTA11_TUISpawnFailureAfterPreClaimSurfacesSelfHeal(t *testing.T) {
	withFleetHome(t)
	seedProjectMeta(t, "demo", t.TempDir())
	killed := installTUIOrphanTmux(t, "demo", "badc0ffe", errors.New("kill refused"))
	stub := &stubFleetCmd{}
	stub.install(t)
	st := reconciletest.State{ClaimOK: true}
	installTUIResolveFromState(t, &st)

	m := New("test")
	updated, _ := m.beginResolvedCoordAttach("demo", "project")
	if st.ClaimCalls != 1 || st.ClaimedID != "claimtui" {
		t.Fatalf("Spawn verdict must pre-claim before fallible cleanup; calls=%d id=%q", st.ClaimCalls, st.ClaimedID)
	}
	if strings.Join(*killed, ",") != "fleet-badc0ffe" {
		t.Fatalf("killed sessions = %v, want attempted fleet-badc0ffe", *killed)
	}
	if len(stub.calls) != 0 {
		t.Fatalf("cleanup failure must not dispatch; calls=%v", stub.calls)
	}
	if updated.flash == nil || !updated.flash.isErr {
		t.Fatalf("cleanup failure should flash error; got %+v", updated.flash)
	}
	for _, want := range []string{"orphan cleanup failed", "self-heal after its TTL"} {
		if !strings.Contains(updated.flash.text, want) {
			t.Fatalf("flash missing %q; got %q", want, updated.flash.text)
		}
	}
}

// TestTA11b_TUISpawnInitFailureAfterPreClaimSurfacesSelfHeal mirrors TA11
// but for the state.EnsureProjectInitialized failure branch (codex 265b
// iter-1 [P1]): a Spawn verdict has ALREADY claimed the starting record
// (no release primitive) before this branch runs, so a silent "init
// failed" flash with no self-heal note left the operator unable to tell
// a bounded (TTL-limited) phantom-boot outage from a wedged one.
func TestTA11b_TUISpawnInitFailureAfterPreClaimSurfacesSelfHeal(t *testing.T) {
	tmp := t.TempDir()
	fleetRoot := filepath.Join(tmp, "fleet")
	t.Setenv("FLEET_HOME", fleetRoot)
	// agents/ must exist and be readable so tuiKillCollidingOrphanTmux's
	// agent.ListStrict() falls through cleanly into the init-failure
	// branch under test (same setup as
	// TestKeyA_ProjectRow_InitErrShowsBanner in actions_test.go).
	if err := os.MkdirAll(filepath.Join(fleetRoot, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Force state.EnsureProjectInitialized to fail: "projects" exists as a
	// regular file, not a directory.
	if err := os.WriteFile(filepath.Join(fleetRoot, "projects"), []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	stub := &stubFleetCmd{}
	stub.install(t)
	st := reconciletest.State{ClaimOK: true}
	installTUIResolveFromState(t, &st)

	updated, _ := New("test").beginResolvedCoordAttach("demo", "project")
	if st.ClaimCalls != 1 || st.ClaimedID != "claimtui" {
		t.Fatalf("Spawn verdict must pre-claim before the fallible init check; calls=%d id=%q", st.ClaimCalls, st.ClaimedID)
	}
	if len(stub.calls) != 0 {
		t.Fatalf("init failure must not dispatch; calls=%v", stub.calls)
	}
	if updated.flash == nil || !updated.flash.isErr {
		t.Fatalf("init failure should flash error; got %+v", updated.flash)
	}
	for _, want := range []string{"init failed", "self-heal after its TTL"} {
		if !strings.Contains(updated.flash.text, want) {
			t.Fatalf("flash missing %q; got %q", want, updated.flash.text)
		}
	}
}

// TestTA11c_TUISpawnRepoUnresolvedAfterPreClaimSurfacesSelfHeal mirrors
// TA11/TA11b for the coordRepoForProject failure branch (codex 265b
// iter-1 [P1]): an unresolvable repo binding (no project meta) is
// discovered AFTER the Spawn verdict already claimed the starting record.
func TestTA11c_TUISpawnRepoUnresolvedAfterPreClaimSurfacesSelfHeal(t *testing.T) {
	withFleetHome(t)
	// No seedProjectMeta call: "ghost-project" has no meta.json, so
	// coordRepoForProject refuses (TestCoordRepoForProject_RefusesWhenUnresolvable
	// pins the same refusal). state.EnsureProjectInitialized only needs a
	// writable FLEET_HOME, so it succeeds and control reaches this branch.
	stub := &stubFleetCmd{}
	stub.install(t)
	st := reconciletest.State{ClaimOK: true}
	installTUIResolveFromState(t, &st)

	updated, _ := New("test").beginResolvedCoordAttach("ghost-project", "project")
	if st.ClaimCalls != 1 || st.ClaimedID != "claimtui" {
		t.Fatalf("Spawn verdict must pre-claim before the fallible repo resolve; calls=%d id=%q", st.ClaimCalls, st.ClaimedID)
	}
	if len(stub.calls) != 0 {
		t.Fatalf("repo-unresolved failure must not dispatch; calls=%v", stub.calls)
	}
	if updated.flash == nil || !updated.flash.isErr {
		t.Fatalf("repo-unresolved failure should flash error; got %+v", updated.flash)
	}
	for _, want := range []string{"no usable checkout", "self-heal after its TTL"} {
		if !strings.Contains(updated.flash.text, want) {
			t.Fatalf("flash missing %q; got %q", want, updated.flash.text)
		}
	}
}

func TestTA12_TUISpawnVetoCallbackReResolvesLease(t *testing.T) {
	t.Run("Attach holder", func(t *testing.T) {
		withFleetHome(t)
		owner := "vetoa12"
		seedDiskCoord(t, owner, "demo", coordTaskID("demo"))
		prevResolve := tuiCoordResolveFn
		readOnly := 0
		tuiCoordResolveFn = func(project, agentID string) (coordreconcile.Verdict, error) {
			if agentID != "" {
				t.Fatalf("veto callback must re-Resolve read-only; agentID=%q", agentID)
			}
			readOnly++
			return coordreconcile.Verdict{
				Decision: coordreconcile.Attach,
				Owner:    coordlock.Owner{AgentID: owner, PID: 12},
				Reason:   "veto winner",
			}, nil
		}
		t.Cleanup(func() { tuiCoordResolveFn = prevResolve })

		msg := resolveCoordSpawnVeto("demo")
		if !msg.attachedExisting {
			t.Fatalf("veto attach should return attachedExisting; msg=%+v", msg)
		}
		if msg.session != "fleet-"+owner {
			t.Fatalf("session = %q, want fleet-%s", msg.session, owner)
		}
		if readOnly != 1 {
			t.Fatalf("read-only Resolve calls = %d, want 1", readOnly)
		}
	})

	t.Run("Attach holder record load failure", func(t *testing.T) {
		withFleetHome(t)
		owner := "missveto"
		prevResolve := tuiCoordResolveFn
		tuiCoordResolveFn = func(project, agentID string) (coordreconcile.Verdict, error) {
			if agentID != "" {
				t.Fatalf("veto callback must re-Resolve read-only; agentID=%q", agentID)
			}
			return coordreconcile.Verdict{
				Decision: coordreconcile.Attach,
				Owner:    coordlock.Owner{AgentID: owner, PID: 12},
				Reason:   "veto winner record missing",
			}, nil
		}
		t.Cleanup(func() { tuiCoordResolveFn = prevResolve })

		msg := resolveCoordSpawnVeto("demo")
		if msg.attachedExisting {
			t.Fatalf("record-load failure must not attach; msg=%+v", msg)
		}
		if !strings.Contains(msg.recoverable, "lease owner "+owner+" record is not readable") ||
			!strings.Contains(msg.recoverable, "coord lease for demo is held by a live leader not reachable from this TUI") {
			t.Fatalf("recoverable veto message did not include load failure; got %q", msg.recoverable)
		}
	})

	t.Run("Wait recovery", func(t *testing.T) {
		withFleetHome(t)
		prevResolve := tuiCoordResolveFn
		tuiCoordResolveFn = func(string, string) (coordreconcile.Verdict, error) {
			return coordreconcile.Verdict{Decision: coordreconcile.Wait, Reason: "winner still publishing"}, nil
		}
		t.Cleanup(func() { tuiCoordResolveFn = prevResolve })

		msg := resolveCoordSpawnVeto("demo")
		if msg.attachedExisting {
			t.Fatalf("Wait verdict must not attach; msg=%+v", msg)
		}
		if !strings.Contains(msg.recoverable, "lease re-resolve returned wait") ||
			!strings.Contains(msg.recoverable, "winner still publishing") {
			t.Fatalf("recoverable veto message did not include Wait verdict; got %q", msg.recoverable)
		}
	})
}

func TestTA13_TUIRowAgentChainFollowCoordUsesResolve(t *testing.T) {
	t.Run("coord tail validates through Resolve", func(t *testing.T) {
		withFleetHome(t)
		(&stubSessionProbe{}).install(t)
		cur := tuiCoordRecord("oldc0013", "demo")
		tail := tuiCoordRecord("tail0013", "demo")
		holder := tuiCoordRecord("hold0013", "demo")
		st := reconciletest.State{LiveOwner: &coordlock.Owner{AgentID: holder.ID, PID: 13}}
		resolveCalls := installTUIResolveFromState(t, &st)
		forceTUICurrentOwner(t, true, holder.ID)
		prevChain := chainResolveFn
		chainResolveFn = func(id string) (*agent.Record, int, error) {
			if id != cur.ID {
				t.Fatalf("chainResolveFn id = %q, want %q", id, cur.ID)
			}
			return tail, 1, nil
		}
		t.Cleanup(func() { chainResolveFn = prevChain })

		m := makeModelWithAgents(cur, holder)
		updated, cmd := m.Update(keyMsg("a"))
		mm := updated.(Model)
		if cmd == nil {
			t.Fatal("coord chain-follow should quit into resolved holder")
		}
		if mm.pendingAttach != holder.TmuxSession {
			t.Fatalf("pendingAttach = %q, want resolved holder %q (not tail %q)", mm.pendingAttach, holder.TmuxSession, tail.TmuxSession)
		}
		if *resolveCalls != 1 {
			t.Fatalf("coord chain-follow must call Resolve once; calls=%d", *resolveCalls)
		}
	})

	t.Run("non-coord tail stays direct", func(t *testing.T) {
		withFleetHome(t)
		(&stubSessionAlive{}).install(t)
		cur := sampleAgent("oldn0013")
		cur.Project = "demo"
		cur.TaskID = "worker-demo"
		tail := sampleAgent("tailn013")
		tail.Project = "demo"
		tail.TaskID = "worker-demo"

		prevResolve := tuiCoordResolveFn
		resolveCalls := 0
		tuiCoordResolveFn = func(string, string) (coordreconcile.Verdict, error) {
			resolveCalls++
			return coordreconcile.Verdict{Decision: coordreconcile.Wait, Reason: "should not be called"}, nil
		}
		t.Cleanup(func() { tuiCoordResolveFn = prevResolve })
		prevChain := chainResolveFn
		chainResolveFn = func(string) (*agent.Record, int, error) { return tail, 1, nil }
		t.Cleanup(func() { chainResolveFn = prevChain })

		m := makeModelWithAgents(cur)
		updated, cmd := m.Update(keyMsg("a"))
		mm := updated.(Model)
		if cmd == nil {
			t.Fatal("non-coord chain-follow should still attach directly to live tail")
		}
		if mm.pendingAttach != tail.TmuxSession {
			t.Fatalf("pendingAttach = %q, want non-coord tail %q", mm.pendingAttach, tail.TmuxSession)
		}
		if resolveCalls != 0 {
			t.Fatalf("non-coord chain-follow must not call coord Resolve; calls=%d", resolveCalls)
		}
	})
}
