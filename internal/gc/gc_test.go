package gc

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tmux"
)

// TDD-red suite for fleet#165 PR-A. Each test stubs the Reconcile Deps
// struct so the unit under test never touches the operator's real
// ~/.fleet/ or /tmp/ state. Production wiring (DefaultDeps) is covered
// indirectly by the CLI smoke tests in cmd/fleet/gc_test.go.
//
// Layout per kind:
//
//	sockets       — file-based, t.TempDir() instead of /tmp, max-age gate
//	orphan-agents — agent.Record fixture + fake SessionAlive
//	orphan-tmux   — tmux.SessionInfo + fake agent record presence
//	worktrees     — fake worktree listing + terminal-task probe
//
// Every test asserts both the Report content (planned action) AND the
// actual side effects (or lack thereof in dry-run / surface paths).

// stubDeps gives every test the same baseline: deterministic Now,
// empty slices everywhere, no-op mutators. Tests override only the
// fields they exercise.
func stubDeps(now time.Time) Deps {
	return Deps{
		Now:         func() time.Time { return now },
		ListSockets: func() ([]SocketInfo, error) { return nil, nil },
		RemoveSocket: func(string) error {
			return errors.New("stubDeps: RemoveSocket should not run")
		},
		ListAgents: func() ([]*agent.Record, error) { return nil, nil },
		ArchiveAgent: func(*agent.Record) error {
			return errors.New("stubDeps: ArchiveAgent should not run")
		},
		SessionAlive: func(string) (bool, error) {
			return true, nil // pretend alive unless overridden
		},
		// Default to "sane" so the existing tests that don't care about
		// the agent-dir-sanity gate continue passing. Regression tests
		// for codex iter-1 [P1] override this to return an error.
		AgentDirSane: func() error { return nil },
		ListSessions: func() ([]tmux.SessionInfo, error) { return nil, nil },
		KillSession: func(string) error {
			return errors.New("stubDeps: KillSession should not run")
		},
		// Default freshness mirrors the constant floor; tests for the
		// dynamic ceiling (codex iter-1 [P1]) override this to a longer
		// window to exercise the FLEET_PID_RESOLVE_S scaling.
		OrphanTmuxFreshness: func() time.Duration { return OrphanTmuxMinFreshness },
		ListProjects:        func() ([]string, error) { return nil, nil },
		ListWorktrees: func(string) ([]WorktreeEntry, error) {
			return nil, nil
		},
		RemoveWorktree: func(string) error {
			return errors.New("stubDeps: RemoveWorktree should not run")
		},
		IsTaskTerminal: func(string, string) (bool, error) {
			return false, nil
		},
		// Coord-locks: empty list by default; tests that exercise the
		// classifier override these three.
		ListCoordLocks: func() ([]CoordLockInfo, error) { return nil, nil },
		LoadAgent: func(string) (*agent.Record, error) {
			// Default: behave as if the agent record exists with no
			// project + no tmux session. Tests that exercise the
			// classifier override this to return ErrNotFound (dead-coord),
			// a record with a different project (mismatch), or a record
			// with a tmux session (stale-tmux).
			return &agent.Record{}, nil
		},
		RemoveCoordLock: func(string) error {
			return errors.New("stubDeps: RemoveCoordLock should not run")
		},
		// Worker-records (KindWorkerRecords). Empty list by default;
		// tests that exercise the classifier override these five.
		ListWorkerRecords: func() ([]WorkerRecordInfo, error) { return nil, nil },
		LoadWorkerState: func(string) (WorkerState, error) {
			return WorkerState{}, nil
		},
		LoadTaskStatus: func(string, string) (string, error) {
			// Default "in-progress" so absent overrides don't trigger
			// the task-terminal branch on tests that don't care about it.
			return "in-progress", nil
		},
		PidAlive: func(int) bool {
			// Default: pretend any pid is alive unless overridden. The
			// dead-pid + stale-heartbeat tests override this.
			return true
		},
		RemoveWorkerRecord: func(string) error {
			return errors.New("stubDeps: RemoveWorkerRecord should not run")
		},
		// Invalid-projects (KindInvalidProjects). Empty list by default;
		// the classifier tests override ListProjectDirs / RemoveProjectDir.
		ListProjectDirs:  func() ([]ProjectDirInfo, error) { return nil, nil },
		ValidProjectName: func(name string) bool { return state.ValidateProjectName(name) == nil },
		RemoveProjectDir: func(string) error {
			return errors.New("stubDeps: RemoveProjectDir should not run")
		},
		// Default recheck: tasks.md absent (safe to remove). The TOCTOU
		// test overrides this to simulate a racing concurrent write.
		ProjectHasTasks: func(string) (bool, error) { return false, nil },
		// Identity quarantine for in-memory tests (no real FS): the reap
		// flow renames before delete; stub it as a pass-through so the
		// classifier's verb/remover assertions don't need a temp dir. The
		// EndToEnd test uses DefaultDeps (real rename) instead.
		QuarantineProjectDir: func(p string) (string, error) { return p, nil },
		RestoreProjectDir:    func(string, string) error { return nil },
	}
}

func defaultKinds() []Kind {
	return []Kind{KindSockets, KindOrphanAgents, KindOrphanTmux, KindWorktrees, KindCoordLocks, KindWorkerRecords}
}

func findAction(r Report, kind Kind, target string) (Action, bool) {
	for _, a := range r.Actions {
		if a.Kind == kind && a.Target == target {
			return a, true
		}
	}
	return Action{}, false
}

func TestReconcile_OldSocketRemoved(t *testing.T) {
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	old := now.Add(-25 * time.Hour) // older than 24h max-age
	deps := stubDeps(now)
	socketPath := "/tmp/fleet-test-aaaaaa.sock"
	deps.ListSockets = func() ([]SocketInfo, error) {
		return []SocketInfo{{Path: socketPath, ModTime: old}}, nil
	}
	var removed []string
	deps.RemoveSocket = func(p string) error {
		removed = append(removed, p)
		return nil
	}

	// Dry-run: report contains would-remove; nothing removed.
	dry, err := Reconcile(Options{Apply: false, MaxAge: 24 * time.Hour, Kinds: defaultKinds()}, deps)
	if err != nil {
		t.Fatalf("Reconcile dry-run: %v", err)
	}
	a, ok := findAction(dry, KindSockets, socketPath)
	if !ok {
		t.Fatalf("missing sockets action; got %+v", dry.Actions)
	}
	if a.Verb != VerbWouldRemove {
		t.Fatalf("dry-run socket action=%q, want %q", a.Verb, VerbWouldRemove)
	}
	if len(removed) != 0 {
		t.Fatalf("dry-run removed %v sockets; want 0", removed)
	}

	// Apply: actually removes.
	got, err := Reconcile(Options{Apply: true, MaxAge: 24 * time.Hour, Kinds: defaultKinds()}, deps)
	if err != nil {
		t.Fatalf("Reconcile apply: %v", err)
	}
	a, ok = findAction(got, KindSockets, socketPath)
	if !ok {
		t.Fatalf("missing sockets action under apply; got %+v", got.Actions)
	}
	if a.Verb != VerbRemoved {
		t.Fatalf("apply socket action=%q, want %q", a.Verb, VerbRemoved)
	}
	if len(removed) != 1 || removed[0] != socketPath {
		t.Fatalf("removed=%v, want [%s]", removed, socketPath)
	}
}

func TestReconcile_RecentSocketKept(t *testing.T) {
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-1 * time.Hour) // within 24h max-age
	deps := stubDeps(now)
	deps.ListSockets = func() ([]SocketInfo, error) {
		return []SocketInfo{{Path: "/tmp/fleet-test-bbbbbb.sock", ModTime: recent}}, nil
	}
	got, err := Reconcile(Options{Apply: true, MaxAge: 24 * time.Hour, Kinds: defaultKinds()}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, ok := findAction(got, KindSockets, "/tmp/fleet-test-bbbbbb.sock"); ok {
		t.Fatalf("recent socket appeared in report; got %+v", got.Actions)
	}
}

func TestReconcile_OrphanAgentRecord_ArchivedOnApply(t *testing.T) {
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	deps := stubDeps(now)
	rec := &agent.Record{ID: "deadbeef", TmuxSession: "fleet-deadbeef"}
	deps.ListAgents = func() ([]*agent.Record, error) { return []*agent.Record{rec}, nil }
	// tmux session is GONE.
	deps.SessionAlive = func(name string) (bool, error) {
		if name == "fleet-deadbeef" {
			return false, nil
		}
		return true, nil
	}
	var archived []string
	deps.ArchiveAgent = func(r *agent.Record) error {
		archived = append(archived, r.ID)
		return nil
	}

	// Dry-run.
	dry, err := Reconcile(Options{Apply: false, MaxAge: 24 * time.Hour, Kinds: defaultKinds()}, deps)
	if err != nil {
		t.Fatalf("Reconcile dry-run: %v", err)
	}
	a, ok := findAction(dry, KindOrphanAgents, "deadbeef")
	if !ok {
		t.Fatalf("missing orphan-agents action; got %+v", dry.Actions)
	}
	if a.Verb != VerbWouldArchive {
		t.Fatalf("dry-run orphan-agent action=%q, want %q", a.Verb, VerbWouldArchive)
	}
	if len(archived) != 0 {
		t.Fatalf("dry-run archived %v; want 0", archived)
	}

	// Apply.
	got, err := Reconcile(Options{Apply: true, MaxAge: 24 * time.Hour, Kinds: defaultKinds()}, deps)
	if err != nil {
		t.Fatalf("Reconcile apply: %v", err)
	}
	a, ok = findAction(got, KindOrphanAgents, "deadbeef")
	if !ok || a.Verb != VerbArchived {
		t.Fatalf("apply orphan-agent action; got %+v", got.Actions)
	}
	if len(archived) != 1 || archived[0] != "deadbeef" {
		t.Fatalf("archived=%v, want [deadbeef]", archived)
	}
}

func TestReconcile_HealthyAgent_Unchanged(t *testing.T) {
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	deps := stubDeps(now)
	rec := &agent.Record{ID: "livecafe", TmuxSession: "fleet-livecafe"}
	deps.ListAgents = func() ([]*agent.Record, error) { return []*agent.Record{rec}, nil }
	deps.SessionAlive = func(string) (bool, error) { return true, nil }
	got, err := Reconcile(Options{Apply: true, MaxAge: 24 * time.Hour, Kinds: defaultKinds()}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, ok := findAction(got, KindOrphanAgents, "livecafe"); ok {
		t.Fatalf("healthy agent flagged orphan; got %+v", got.Actions)
	}
}

func TestReconcile_OrphanTmuxSession_SurfacedNotKilled(t *testing.T) {
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	created := now.Add(-2 * time.Hour) // safely past freshness floor
	deps := stubDeps(now)
	// Session exists; NO agent record matches the suffix.
	deps.ListSessions = func() ([]tmux.SessionInfo, error) {
		return []tmux.SessionInfo{{Name: "fleet-0a0a0a0a", Created: created}}, nil
	}
	deps.ListAgents = func() ([]*agent.Record, error) { return nil, nil }
	// Default Aggressive=false. Even with --apply, surface only.
	got, err := Reconcile(Options{Apply: true, Aggressive: false, MaxAge: 24 * time.Hour, Kinds: defaultKinds()}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	a, ok := findAction(got, KindOrphanTmux, "fleet-0a0a0a0a")
	if !ok {
		t.Fatalf("missing orphan-tmux action; got %+v", got.Actions)
	}
	if a.Verb != VerbSurface {
		t.Fatalf("orphan-tmux action=%q, want %q (surface-only by default per feedback_surface_dont_silo + feedback_user_owns_tmux_config)",
			a.Verb, VerbSurface)
	}
}

func TestReconcile_OrphanTmuxSession_AggressiveKills(t *testing.T) {
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	created := now.Add(-2 * time.Hour)
	deps := stubDeps(now)
	deps.ListSessions = func() ([]tmux.SessionInfo, error) {
		return []tmux.SessionInfo{{Name: "fleet-0b0b0b0b", Created: created}}, nil
	}
	deps.ListAgents = func() ([]*agent.Record, error) { return nil, nil }
	var killed []string
	deps.KillSession = func(name string) error {
		killed = append(killed, name)
		return nil
	}

	// --aggressive + dry-run = would-kill, no side effect.
	dry, err := Reconcile(Options{Apply: false, Aggressive: true, MaxAge: 24 * time.Hour, Kinds: defaultKinds()}, deps)
	if err != nil {
		t.Fatalf("Reconcile dry-run: %v", err)
	}
	a, ok := findAction(dry, KindOrphanTmux, "fleet-0b0b0b0b")
	if !ok || a.Verb != VerbWouldKill {
		t.Fatalf("dry-run aggressive action; got %+v", dry.Actions)
	}
	if len(killed) != 0 {
		t.Fatalf("dry-run killed %v; want 0", killed)
	}

	// --aggressive + --apply = killed.
	got, err := Reconcile(Options{Apply: true, Aggressive: true, MaxAge: 24 * time.Hour, Kinds: defaultKinds()}, deps)
	if err != nil {
		t.Fatalf("Reconcile apply: %v", err)
	}
	a, ok = findAction(got, KindOrphanTmux, "fleet-0b0b0b0b")
	if !ok || a.Verb != VerbKilled {
		t.Fatalf("apply aggressive action; got %+v", got.Actions)
	}
	if len(killed) != 1 || killed[0] != "fleet-0b0b0b0b" {
		t.Fatalf("killed=%v, want [fleet-0b0b0b0b]", killed)
	}
}

func TestReconcile_OrphanTmuxFresh_SkippedEvenAggressive(t *testing.T) {
	// Same freshness floor as fleet maintenance prune-orphan-tmux:
	// a record-absent session younger than the floor is not yet an
	// orphan (likely an in-flight spawn). Even --aggressive must NOT
	// touch it. Reuses pruneOrphanTmuxMinFreshness's 90s rationale.
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	created := now.Add(-30 * time.Second) // fresher than 90s floor
	deps := stubDeps(now)
	deps.ListSessions = func() ([]tmux.SessionInfo, error) {
		return []tmux.SessionInfo{{Name: "fleet-fffeeedd", Created: created}}, nil
	}
	deps.ListAgents = func() ([]*agent.Record, error) { return nil, nil }
	got, err := Reconcile(Options{Apply: true, Aggressive: true, MaxAge: 24 * time.Hour, Kinds: defaultKinds()}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, ok := findAction(got, KindOrphanTmux, "fleet-fffeeedd"); ok {
		t.Fatalf("fresh tmux session flagged; got %+v", got.Actions)
	}
}

func TestReconcile_NonFleetTmuxSession_Ignored(t *testing.T) {
	// Sessions whose name doesn't match the fleet-<8-hex> shape are
	// outside fleet's blast radius (feedback_user_owns_tmux_config).
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	created := now.Add(-2 * time.Hour)
	deps := stubDeps(now)
	deps.ListSessions = func() ([]tmux.SessionInfo, error) {
		return []tmux.SessionInfo{
			{Name: "main", Created: created},
			{Name: "fleet-debug", Created: created},          // not 8-hex
			{Name: "fleet-coord-839b11ff", Created: created}, // not bare 8-hex
		}, nil
	}
	deps.ListAgents = func() ([]*agent.Record, error) { return nil, nil }
	got, err := Reconcile(Options{Apply: true, Aggressive: true, MaxAge: 24 * time.Hour, Kinds: defaultKinds()}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	for _, a := range got.Actions {
		if a.Kind == KindOrphanTmux {
			t.Fatalf("non-fleet session classified: %+v", a)
		}
	}
}

func TestReconcile_WorktreeOfDoneTask_Removed(t *testing.T) {
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	deps := stubDeps(now)
	deps.ListProjects = func() ([]string, error) { return []string{"projects-fleet"}, nil }
	deps.ListWorktrees = func(project string) ([]WorktreeEntry, error) {
		if project != "projects-fleet" {
			return nil, nil
		}
		return []WorktreeEntry{{Project: project, Slug: "old-feature-aaaa", Path: "/fake/wt/old-feature-aaaa"}}, nil
	}
	deps.IsTaskTerminal = func(project, slug string) (bool, error) {
		return project == "projects-fleet" && slug == "old-feature-aaaa", nil
	}
	var removed []string
	deps.RemoveWorktree = func(path string) error {
		removed = append(removed, path)
		return nil
	}

	dry, err := Reconcile(Options{Apply: false, MaxAge: 24 * time.Hour, Kinds: defaultKinds()}, deps)
	if err != nil {
		t.Fatalf("Reconcile dry-run: %v", err)
	}
	a, ok := findAction(dry, KindWorktrees, "/fake/wt/old-feature-aaaa")
	if !ok || a.Verb != VerbWouldRemove {
		t.Fatalf("dry-run worktree action; got %+v", dry.Actions)
	}
	if len(removed) != 0 {
		t.Fatalf("dry-run removed=%v", removed)
	}

	got, err := Reconcile(Options{Apply: true, MaxAge: 24 * time.Hour, Kinds: defaultKinds()}, deps)
	if err != nil {
		t.Fatalf("Reconcile apply: %v", err)
	}
	a, ok = findAction(got, KindWorktrees, "/fake/wt/old-feature-aaaa")
	if !ok || a.Verb != VerbRemoved {
		t.Fatalf("apply worktree action; got %+v", got.Actions)
	}
	if len(removed) != 1 || removed[0] != "/fake/wt/old-feature-aaaa" {
		t.Fatalf("removed=%v, want [/fake/wt/old-feature-aaaa]", removed)
	}
}

func TestReconcile_WorktreeOfActiveTask_Kept(t *testing.T) {
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	deps := stubDeps(now)
	deps.ListProjects = func() ([]string, error) { return []string{"projects-fleet"}, nil }
	deps.ListWorktrees = func(project string) ([]WorktreeEntry, error) {
		return []WorktreeEntry{{Project: project, Slug: "live-task-bbbb", Path: "/fake/wt/live-task-bbbb"}}, nil
	}
	deps.IsTaskTerminal = func(string, string) (bool, error) {
		return false, nil // task is in-progress
	}
	got, err := Reconcile(Options{Apply: true, MaxAge: 24 * time.Hour, Kinds: defaultKinds()}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, ok := findAction(got, KindWorktrees, "/fake/wt/live-task-bbbb"); ok {
		t.Fatalf("active-task worktree flagged; got %+v", got.Actions)
	}
}

func TestReconcile_DryRun_NeverMutates(t *testing.T) {
	// Comprehensive negative invariant: every applyable mutator path
	// (RemoveSocket, ArchiveAgent, KillSession, RemoveWorktree) must
	// be silent when Apply=false, even if the planner says it would
	// act on every kind.
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	old := now.Add(-25 * time.Hour)
	deps := stubDeps(now)
	deps.ListSockets = func() ([]SocketInfo, error) {
		return []SocketInfo{{Path: "/tmp/fleet-test-cccccc.sock", ModTime: old}}, nil
	}
	deps.ListAgents = func() ([]*agent.Record, error) {
		return []*agent.Record{{ID: "01010101", TmuxSession: "fleet-01010101"}}, nil
	}
	deps.SessionAlive = func(string) (bool, error) { return false, nil }
	deps.ListSessions = func() ([]tmux.SessionInfo, error) {
		// Session whose suffix doesn't match the listed agent record →
		// orphan tmux candidate.
		return []tmux.SessionInfo{{Name: "fleet-02020202", Created: old}}, nil
	}
	deps.ListProjects = func() ([]string, error) { return []string{"p"}, nil }
	deps.ListWorktrees = func(string) ([]WorktreeEntry, error) {
		return []WorktreeEntry{{Project: "p", Slug: "done-task", Path: "/fake/wt/done"}}, nil
	}
	deps.IsTaskTerminal = func(string, string) (bool, error) { return true, nil }

	// Mutators must not be called.
	deps.RemoveSocket = func(string) error { t.Fatal("RemoveSocket called under dry-run"); return nil }
	deps.ArchiveAgent = func(*agent.Record) error { t.Fatal("ArchiveAgent called under dry-run"); return nil }
	deps.KillSession = func(string) error { t.Fatal("KillSession called under dry-run"); return nil }
	deps.RemoveWorktree = func(string) error { t.Fatal("RemoveWorktree called under dry-run"); return nil }

	if _, err := Reconcile(Options{Apply: false, Aggressive: true, MaxAge: 24 * time.Hour, Kinds: defaultKinds()}, deps); err != nil {
		t.Fatalf("Reconcile dry-run: %v", err)
	}
}

func TestReconcile_KindsFilter(t *testing.T) {
	// --kinds=sockets only — agent, tmux, worktree paths must not run.
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	old := now.Add(-25 * time.Hour)
	deps := stubDeps(now)
	deps.ListSockets = func() ([]SocketInfo, error) {
		return []SocketInfo{{Path: "/tmp/fleet-test-dddddd.sock", ModTime: old}}, nil
	}
	deps.RemoveSocket = func(string) error { return nil }
	deps.ListAgents = func() ([]*agent.Record, error) {
		t.Fatal("ListAgents called under --kinds=sockets")
		return nil, nil
	}
	deps.ListSessions = func() ([]tmux.SessionInfo, error) {
		t.Fatal("ListSessions called under --kinds=sockets")
		return nil, nil
	}
	deps.ListProjects = func() ([]string, error) {
		t.Fatal("ListProjects called under --kinds=sockets")
		return nil, nil
	}
	got, err := Reconcile(Options{Apply: true, MaxAge: 24 * time.Hour, Kinds: []Kind{KindSockets}}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(got.Actions) != 1 {
		t.Fatalf("actions=%+v; want exactly 1 sockets action", got.Actions)
	}
	if got.Actions[0].Kind != KindSockets {
		t.Fatalf("actions[0].Kind=%q, want sockets", got.Actions[0].Kind)
	}
}

func TestReconcile_ProjectScope(t *testing.T) {
	// --project filters worktree + agent enumeration. Other projects
	// must not be touched. Sockets are global (no project scope) and
	// remain in the report regardless.
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	deps := stubDeps(now)
	listed := []string{}
	deps.ListProjects = func() ([]string, error) {
		// Reconcile MUST NOT call ListProjects when Project is set —
		// it should target the named project directly.
		t.Fatal("ListProjects called under --project scope")
		return nil, nil
	}
	deps.ListWorktrees = func(p string) ([]WorktreeEntry, error) {
		listed = append(listed, p)
		return nil, nil
	}
	deps.ListAgents = func() ([]*agent.Record, error) {
		return []*agent.Record{
			{ID: "11111111", TmuxSession: "fleet-11111111", Project: "alpha"},
			{ID: "22222222", TmuxSession: "fleet-22222222", Project: "beta"},
		}, nil
	}
	deps.SessionAlive = func(string) (bool, error) { return false, nil }
	deps.ArchiveAgent = func(r *agent.Record) error {
		if r.Project != "alpha" {
			t.Fatalf("ArchiveAgent on out-of-scope record: %+v", r)
		}
		return nil
	}
	got, err := Reconcile(Options{Apply: true, Project: "alpha", MaxAge: 24 * time.Hour, Kinds: defaultKinds()}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !contains(listed, "alpha") || contains(listed, "beta") {
		t.Fatalf("worktree list scope=%v; want only [alpha]", listed)
	}
	// agent action only for alpha record.
	if _, ok := findAction(got, KindOrphanAgents, "11111111"); !ok {
		t.Fatalf("missing alpha agent action; got %+v", got.Actions)
	}
	if _, ok := findAction(got, KindOrphanAgents, "22222222"); ok {
		t.Fatalf("beta agent flagged out-of-scope; got %+v", got.Actions)
	}
}

func TestReconcile_SessionAliveProbeError_PreservesAgent(t *testing.T) {
	// If the tmux probe fails (transport error, lost server), the
	// orphan-agent classifier must NOT archive the record — that
	// would mistake a transient tmux failure for a dead session and
	// strand the operator's live agent. Mirrors tmux.SessionAlive's
	// own "alive=false, err!=nil = ambiguous, do not act" contract.
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	deps := stubDeps(now)
	rec := &agent.Record{ID: "33333333", TmuxSession: "fleet-33333333"}
	deps.ListAgents = func() ([]*agent.Record, error) { return []*agent.Record{rec}, nil }
	deps.SessionAlive = func(string) (bool, error) {
		return false, errors.New("simulated transport error")
	}
	deps.ArchiveAgent = func(*agent.Record) error {
		t.Fatal("ArchiveAgent called on probe-error ambiguous record")
		return nil
	}
	got, err := Reconcile(Options{Apply: true, MaxAge: 24 * time.Hour, Kinds: defaultKinds()}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, ok := findAction(got, KindOrphanAgents, "33333333"); ok {
		t.Fatalf("probe-error record flagged orphan; got %+v", got.Actions)
	}
}

func TestReconcile_OrphanTmux_MatchedByLiveAgent_Skipped(t *testing.T) {
	// The orphan-tmux classifier must skip sessions whose suffix
	// matches a live agent record (the agent's own session). Without
	// this, every healthy fleet-* session would surface in the
	// report.
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	created := now.Add(-2 * time.Hour)
	deps := stubDeps(now)
	deps.ListSessions = func() ([]tmux.SessionInfo, error) {
		return []tmux.SessionInfo{{Name: "fleet-44444444", Created: created}}, nil
	}
	deps.ListAgents = func() ([]*agent.Record, error) {
		return []*agent.Record{{ID: "44444444", TmuxSession: "fleet-44444444"}}, nil
	}
	deps.SessionAlive = func(string) (bool, error) { return true, nil }
	got, err := Reconcile(Options{Apply: true, MaxAge: 24 * time.Hour, Kinds: defaultKinds()}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, ok := findAction(got, KindOrphanTmux, "fleet-44444444"); ok {
		t.Fatalf("session matched to live agent flagged orphan; got %+v", got.Actions)
	}
}

// ----- helpers for production-dep tests (SocketInfo path) -----

// TestIsTaskTerminalOnDisk covers the production tasks.md parser
// behavior smoked during PR-A: a worktree-dir slug that doesn't match
// any task entry must NOT be classified as terminal (operator-side
// rename / truncated slug / partial workflow — surface-don't-silo
// rather than silently delete the directory). Done/abandoned tasks in
// tasks.md or tasks-archive.md ARE terminal.
func TestIsTaskTerminalOnDisk(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	pdir := filepath.Join(tmp, "projects", "alpha")
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	tasksMD := `## task: live-slug-aaaa
- status: in-progress
- priority: P1

## task: done-slug-bbbb
- status: done
- priority: P2
`
	archiveMD := `## task: abandoned-slug-cccc
- status: abandoned
- priority: P3
`
	if err := os.WriteFile(filepath.Join(pdir, "tasks.md"), []byte(tasksMD), 0o644); err != nil {
		t.Fatalf("write tasks.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pdir, "tasks-archive.md"), []byte(archiveMD), 0o644); err != nil {
		t.Fatalf("write tasks-archive.md: %v", err)
	}
	cases := []struct {
		slug     string
		terminal bool
	}{
		{"live-slug-aaaa", false},
		{"done-slug-bbbb", true},
		{"abandoned-slug-cccc", true},
		{"unknown-slug-eeee", false}, // critical: missing-slug = keep
	}
	for _, c := range cases {
		got, err := isTaskTerminalOnDisk("alpha", c.slug)
		if err != nil {
			t.Errorf("isTaskTerminalOnDisk(%s): %v", c.slug, err)
			continue
		}
		if got != c.terminal {
			t.Errorf("isTaskTerminalOnDisk(%s) = %v, want %v", c.slug, got, c.terminal)
		}
	}
}

// TestDefaultSocketLister_ScansTmp covers the production glob to make
// sure /tmp/fleet-test-*.sock files are picked up via the default
// Deps wiring. Uses a temp dir to avoid touching the operator's real
// /tmp (cleanup is mandatory — feedback_fleet_owns_its_resources).
func TestDefaultSocketLister_ScansTmp(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"fleet-test-aaaaaa.sock",
		"fleet-test-bbbbbb.sock",
		"other-fleet.sock",
	} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatalf("seed %s: %v", p, err)
		}
	}
	infos, err := scanSocketsDir(dir)
	if err != nil {
		t.Fatalf("scanSocketsDir: %v", err)
	}
	var got []string
	for _, i := range infos {
		got = append(got, filepath.Base(i.Path))
	}
	sort.Strings(got)
	want := []string{"fleet-test-aaaaaa.sock", "fleet-test-bbbbbb.sock"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("scanSocketsDir got %v, want %v", got, want)
	}
}

func contains(ss []string, target string) bool {
	for _, s := range ss {
		if s == target {
			return true
		}
	}
	return false
}

// ----- codex iter-1 [P1] regression tests -----

// TestReconcile_OrphanTmux_AgentDirUnavailable_FailsClosed pins the
// fix for codex iter-1 [P1]: when the live-agent state root is missing
// or unreadable, ListAgents() returns an empty slice — every live
// fleet-<id> tmux session then looks unmatched, and --aggressive
// --apply would mass-terminate healthy agents. The orphan-tmux pass
// MUST fail closed (return the AgentDirSane error, produce no kill
// actions) instead of trusting the empty agent listing.
func TestReconcile_OrphanTmux_AgentDirUnavailable_FailsClosed(t *testing.T) {
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	created := now.Add(-2 * time.Hour) // past freshness floor
	deps := stubDeps(now)
	deps.ListSessions = func() ([]tmux.SessionInfo, error) {
		// A live agent's session — would look orphan without the
		// agent-dir-sanity gate because ListAgents returns nil.
		return []tmux.SessionInfo{{Name: "fleet-aabbccdd", Created: created}}, nil
	}
	deps.ListAgents = func() ([]*agent.Record, error) {
		// Simulate the "dir missing → empty listing" failure mode.
		return nil, nil
	}
	deps.AgentDirSane = func() error {
		return errors.New("stat agent dir: no such file or directory (simulated)")
	}
	var killed []string
	deps.KillSession = func(name string) error {
		killed = append(killed, name)
		return nil
	}

	got, err := Reconcile(Options{
		Apply: true, Aggressive: true, MaxAge: 24 * time.Hour,
		Kinds: []Kind{KindOrphanTmux},
	}, deps)
	if err == nil {
		t.Fatalf("Reconcile must surface AgentDirSane error; got nil")
	}
	if !strings.Contains(err.Error(), "agent dir") {
		t.Fatalf("error %q does not mention agent dir", err)
	}
	if len(killed) != 0 {
		t.Fatalf("orphan-tmux killed %v with agent dir unavailable; must fail closed", killed)
	}
	for _, a := range got.Actions {
		if a.Kind == KindOrphanTmux {
			t.Fatalf("orphan-tmux action produced despite AgentDirSane error: %+v", a)
		}
	}
}

// TestReconcile_OrphanTmux_FreshnessFromDeps_RespectsDynamic pins the
// fix for codex iter-1 [P1]: the freshness window MUST scale with the
// configured spawn budget (FLEET_PID_RESOLVE_S via spawn.PidResolveTimeout()
// in production). If the operator raised the pid-resolve timeout for a
// slow-spawn engine, a legitimate in-flight spawn can still be inside
// the record-write window past 90s — and --aggressive --apply with the
// old hard-coded floor would kill it. With the dep returning a longer
// freshness window, the same session must be skipped.
func TestReconcile_OrphanTmux_FreshnessFromDeps_RespectsDynamic(t *testing.T) {
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	// 3 minutes old — past the 90s constant floor, but inside the
	// 5-minute dynamic window the operator wired up.
	created := now.Add(-3 * time.Minute)
	deps := stubDeps(now)
	deps.ListSessions = func() ([]tmux.SessionInfo, error) {
		return []tmux.SessionInfo{{Name: "fleet-bbccddee", Created: created}}, nil
	}
	deps.ListAgents = func() ([]*agent.Record, error) { return nil, nil }
	deps.OrphanTmuxFreshness = func() time.Duration { return 5 * time.Minute }
	var killed []string
	deps.KillSession = func(name string) error {
		killed = append(killed, name)
		return nil
	}

	got, err := Reconcile(Options{
		Apply: true, Aggressive: true, MaxAge: 24 * time.Hour,
		Kinds: []Kind{KindOrphanTmux},
	}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(killed) != 0 {
		t.Fatalf("session inside dynamic freshness window killed: %v", killed)
	}
	if _, ok := findAction(got, KindOrphanTmux, "fleet-bbccddee"); ok {
		t.Fatalf("session inside dynamic freshness window flagged: %+v", got.Actions)
	}
}

// TestReconcile_OrphanTmux_UnparseableAgent_FailsClosed pins the
// fix for codex iter-2 [P1]: when a record on disk is malformed,
// agent.List() silently skips it, so the matching fleet-<id> tmux
// session is absent from the in-memory live set and would be killed
// as an orphan under --aggressive --apply. Wiring through
// ListAgentsStrict forces a fail-closed when badIDs is non-empty,
// same shape as the dispatch --coord-spawn split-brain veto.
func TestReconcile_OrphanTmux_UnparseableAgent_FailsClosed(t *testing.T) {
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	created := now.Add(-2 * time.Hour) // past freshness floor
	deps := stubDeps(now)
	deps.ListSessions = func() ([]tmux.SessionInfo, error) {
		// A live agent's session — would look orphan because its
		// record failed to parse and was silently dropped.
		return []tmux.SessionInfo{{Name: "fleet-deadbeef", Created: created}}, nil
	}
	deps.ListAgentsStrict = func() ([]*agent.Record, []string, error) {
		// Simulate an unparseable record on disk: it does NOT appear
		// in good[], but its ID is in badIDs[].
		return nil, []string{"deadbeef"}, nil
	}
	var killed []string
	deps.KillSession = func(name string) error {
		killed = append(killed, name)
		return nil
	}

	got, err := Reconcile(Options{
		Apply: true, Aggressive: true, MaxAge: 24 * time.Hour,
		Kinds: []Kind{KindOrphanTmux},
	}, deps)
	if err == nil {
		t.Fatalf("Reconcile must surface unparseable-record error; got nil")
	}
	if !strings.Contains(err.Error(), "unparseable") {
		t.Fatalf("error %q does not mention unparseable records", err)
	}
	if len(killed) != 0 {
		t.Fatalf("orphan-tmux killed %v with unparseable records; must fail closed", killed)
	}
	for _, a := range got.Actions {
		if a.Kind == KindOrphanTmux {
			t.Fatalf("orphan-tmux action produced despite bad records: %+v", a)
		}
	}
}

// TestReconcile_OrphanTmux_FreshnessFromDeps_FallsThroughPastWindow is
// the positive counterpart: once the session ages past the dynamic
// window, it IS classified — so the dep doesn't accidentally disable
// classification entirely.
func TestReconcile_OrphanTmux_FreshnessFromDeps_FallsThroughPastWindow(t *testing.T) {
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	created := now.Add(-10 * time.Minute) // past the 5-minute window
	deps := stubDeps(now)
	deps.ListSessions = func() ([]tmux.SessionInfo, error) {
		return []tmux.SessionInfo{{Name: "fleet-ccddeeff", Created: created}}, nil
	}
	deps.ListAgents = func() ([]*agent.Record, error) { return nil, nil }
	deps.OrphanTmuxFreshness = func() time.Duration { return 5 * time.Minute }

	got, err := Reconcile(Options{
		Apply: false, Aggressive: false, MaxAge: 24 * time.Hour,
		Kinds: []Kind{KindOrphanTmux},
	}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	a, ok := findAction(got, KindOrphanTmux, "fleet-ccddeeff")
	if !ok {
		t.Fatalf("session past dynamic freshness window NOT flagged: %+v", got.Actions)
	}
	if a.Verb != VerbSurface {
		t.Fatalf("verb=%q want %q", a.Verb, VerbSurface)
	}
}

// ----- codex iter-3 [P1] regression tests -----

// TestIsTaskTerminalOnDisk_FencedSpoofIgnored pins the fix for codex
// iter-3 [P1]: the naive line-scanning parser could be tricked by a
// `## task: <live-slug>` block embedded inside a fenced markdown
// example, classifying a live worktree as terminal and removing it
// under --apply. Switching to the canonical tasks.Read parser
// (internal/tasks/tasks.go parse() honors `inFence`) closes the hole.
//
// Test layout: an in-progress task with the SAME slug appears verbatim
// inside a fenced code block in tasksMD. The naive parser saw the
// fenced occurrence and reported done. The canonical parser sees
// only the real header (the first one) which is in-progress.
func TestIsTaskTerminalOnDisk_FencedSpoofIgnored(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	pdir := filepath.Join(tmp, "projects", "alpha")
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Build the file content carefully — the fenced block reproduces
	// the exact shape the naive parser used to misread. The canonical
	// parser MUST treat the fenced text as body content of the live
	// task and refuse to classify it as terminal.
	tasksMD := "## task: live-slug-aaaa\n" +
		"- status: in-progress\n" +
		"- priority: P1\n" +
		"- created: 2026-05-20T00:00:00Z\n" +
		"- updated: 2026-05-20T00:00:00Z\n" +
		"\n" +
		"### Notes\n" +
		"Example of a done block (do NOT reap me):\n" +
		"```markdown\n" +
		"## task: live-slug-aaaa\n" +
		"- status: done\n" +
		"```\n"
	if err := os.WriteFile(filepath.Join(pdir, "tasks.md"), []byte(tasksMD), 0o644); err != nil {
		t.Fatalf("write tasks.md: %v", err)
	}
	terminal, err := isTaskTerminalOnDisk("alpha", "live-slug-aaaa")
	if err != nil {
		t.Fatalf("isTaskTerminalOnDisk: %v", err)
	}
	if terminal {
		t.Fatalf("fenced-done block spoofed isTaskTerminalOnDisk; expected false (the real header is in-progress)")
	}
}

// TestReconcile_SocketLive_PreservesBoundSocket pins the fix for
// codex iter-4 [P1]: a long-running tmux test fixture can still be
// bound to a fleet-test-*.sock whose mtime drifted past --max-age.
// Removing it would strand the bound server. Under --apply with
// SocketLive=true, the classifier must surface (Verb=surface) instead
// of removing.
func TestReconcile_SocketLive_PreservesBoundSocket(t *testing.T) {
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	old := now.Add(-25 * time.Hour) // older than 24h max-age
	deps := stubDeps(now)
	livePath := "/tmp/fleet-test-livebind.sock"
	deps.ListSockets = func() ([]SocketInfo, error) {
		return []SocketInfo{{Path: livePath, ModTime: old}}, nil
	}
	deps.SocketLive = func(p string) bool {
		return p == livePath // bound server still responds
	}
	var removed []string
	deps.RemoveSocket = func(p string) error {
		removed = append(removed, p)
		return nil
	}

	got, err := Reconcile(Options{Apply: true, MaxAge: 24 * time.Hour,
		Kinds: []Kind{KindSockets}}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("live-bound socket removed: %v", removed)
	}
	a, ok := findAction(got, KindSockets, livePath)
	if !ok {
		t.Fatalf("live-bound socket not surfaced: %+v", got.Actions)
	}
	if a.Verb != VerbSurface {
		t.Fatalf("verb=%q want %q (live socket should be surfaced not removed)", a.Verb, VerbSurface)
	}
}

// TestReconcile_SocketLive_PreservesBoundSocketInDryRun pins the fix
// for codex iter-5 [P2]: dry-run must show the SAME planned action
// as --apply (the report IS the plan). Without applying SocketLive
// in dry-run, an operator running `fleet gc` would see
// `verb=would-remove` for a bound socket that `fleet gc --apply`
// would actually surface.
func TestReconcile_SocketLive_PreservesBoundSocketInDryRun(t *testing.T) {
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	old := now.Add(-25 * time.Hour)
	deps := stubDeps(now)
	livePath := "/tmp/fleet-test-dryrun-bind.sock"
	deps.ListSockets = func() ([]SocketInfo, error) {
		return []SocketInfo{{Path: livePath, ModTime: old}}, nil
	}
	deps.SocketLive = func(p string) bool { return p == livePath }

	// Apply=false (dry-run) — must STILL surface the live-bound socket.
	got, err := Reconcile(Options{Apply: false, MaxAge: 24 * time.Hour,
		Kinds: []Kind{KindSockets}}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	a, ok := findAction(got, KindSockets, livePath)
	if !ok {
		t.Fatalf("dry-run did not surface live-bound socket: %+v", got.Actions)
	}
	if a.Verb != VerbSurface {
		t.Fatalf("dry-run verb=%q want %q (must match --apply behavior)", a.Verb, VerbSurface)
	}
}

// TestReconcile_SocketLive_DeadSocketStillRemoved is the negative
// counterpart: when SocketLive returns false, the classifier removes
// the socket as before. Confirms the new gate doesn't accidentally
// disable removal for genuinely orphan sockets.
func TestReconcile_SocketLive_DeadSocketStillRemoved(t *testing.T) {
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	old := now.Add(-25 * time.Hour)
	deps := stubDeps(now)
	deadPath := "/tmp/fleet-test-deadbind.sock"
	deps.ListSockets = func() ([]SocketInfo, error) {
		return []SocketInfo{{Path: deadPath, ModTime: old}}, nil
	}
	deps.SocketLive = func(p string) bool { return false }
	var removed []string
	deps.RemoveSocket = func(p string) error {
		removed = append(removed, p)
		return nil
	}

	got, err := Reconcile(Options{Apply: true, MaxAge: 24 * time.Hour,
		Kinds: []Kind{KindSockets}}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(removed) != 1 || removed[0] != deadPath {
		t.Fatalf("dead socket not removed; removed=%v", removed)
	}
	a, ok := findAction(got, KindSockets, deadPath)
	if !ok || a.Verb != VerbRemoved {
		t.Fatalf("dead socket action wrong: %+v", got.Actions)
	}
}

// TestIsTaskTerminalOnDisk_ArchivedNonTerminalStatus_TreatedTerminal
// pins the fix for codex iter-3 [P2]: fleet tasks archive moves rows
// verbatim without coercing status to done, so an operator-forced
// archive of an in-progress task lands in tasks-archive.md with
// status=in-progress. Per the function's docstring ("archived tasks
// are terminal by definition"), the worktree for that slug must be
// classified as terminal, otherwise the archive class of leak is
// permanently unreapable.
func TestIsTaskTerminalOnDisk_ArchivedNonTerminalStatus_TreatedTerminal(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	pdir := filepath.Join(tmp, "projects", "alpha")
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Empty tasks.md (so the slug is found ONLY in tasks-archive.md).
	if err := os.WriteFile(filepath.Join(pdir, "tasks.md"), []byte(""), 0o644); err != nil {
		t.Fatalf("write tasks.md: %v", err)
	}
	// Archive contains the slug with status=in-progress (operator-
	// forced archive of a live task — Archive moves rows verbatim).
	archiveMD := `## task: forced-archive-slug
- status: in-progress
- priority: P2
`
	if err := os.WriteFile(filepath.Join(pdir, "tasks-archive.md"), []byte(archiveMD), 0o644); err != nil {
		t.Fatalf("write tasks-archive.md: %v", err)
	}
	terminal, err := isTaskTerminalOnDisk("alpha", "forced-archive-slug")
	if err != nil {
		t.Fatalf("isTaskTerminalOnDisk: %v", err)
	}
	if !terminal {
		t.Fatalf("archived in-progress slug should be terminal (presence in archive = terminal by definition)")
	}
}

// TestIsTaskTerminalOnDisk_IndentedSpoofIgnored pins the fix for codex
// iter-3 [P2]: an indented `    ## task: <live-slug>` followed by
// `    - status: done` is example text (sub-bullet under Spec/Notes,
// or unfenced markdown), not a structural task header. Trimming
// leading whitespace before matching would let indented examples
// spoof a terminal status. Mirror the canonical parser's column-0
// rule: only column-0 `## task:` headers count as structural
// boundaries.
func TestIsTaskTerminalOnDisk_IndentedSpoofIgnored(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	pdir := filepath.Join(tmp, "projects", "alpha")
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Indented header inside a Notes section. Real header is in-progress;
	// indented "example" header says done. The classifier must use the
	// real one.
	tasksMD := "## task: live-slug-aaaa\n" +
		"- status: in-progress\n" +
		"- priority: P1\n" +
		"\n" +
		"### Notes\n" +
		"Example (do NOT reap me; this is an indented markdown example):\n" +
		"    ## task: live-slug-aaaa\n" +
		"    - status: done\n"
	if err := os.WriteFile(filepath.Join(pdir, "tasks.md"), []byte(tasksMD), 0o644); err != nil {
		t.Fatalf("write tasks.md: %v", err)
	}
	terminal, err := isTaskTerminalOnDisk("alpha", "live-slug-aaaa")
	if err != nil {
		t.Fatalf("isTaskTerminalOnDisk: %v", err)
	}
	if terminal {
		t.Fatalf("indented `## task:` example spoofed terminal status; expected false")
	}
}

// ----- fleet#172 coord-locks classifier tests -----

// TestReconcile_CoordLocks_DeadCoord_Surfaced is the dead-coord failure
// mode: the coordinator.lock body references an agent ID whose record
// file under ~/.fleet/agents/<id>.json is missing. LoadAgent returns
// state.ErrNotFound; the classifier must surface (default Verb=surface,
// no unlink without --apply).
func TestReconcile_CoordLocks_DeadCoord_Surfaced(t *testing.T) {
	now := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	deps := stubDeps(now)
	lockPath := "/fake/projects/alpha/.locks/coordinator.lock"
	deps.ListCoordLocks = func() ([]CoordLockInfo, error) {
		return []CoordLockInfo{{
			Path:     lockPath,
			Project:  "alpha",
			HolderID: "deadbeef",
		}}, nil
	}
	deps.LoadAgent = func(id string) (*agent.Record, error) {
		if id != "deadbeef" {
			t.Fatalf("LoadAgent called with %q, want deadbeef", id)
		}
		return nil, state.ErrNotFound
	}
	// Holder coord is truly dead (its tmux session also gone) — the
	// classic dead-coord case. codex iter-2 [P2]: the live-holder
	// variant is covered separately by
	// TestReconcile_CoordLocks_DeadCoord_LiveHolder_SurfaceRefusesUnlink.
	deps.SessionAlive = func(name string) (bool, error) {
		if name == "fleet-deadbeef" {
			return false, nil
		}
		return true, nil
	}
	got, err := Reconcile(Options{
		Apply: false, MaxAge: 24 * time.Hour,
		Kinds: []Kind{KindCoordLocks},
	}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	a, ok := findAction(got, KindCoordLocks, lockPath)
	if !ok {
		t.Fatalf("missing coord-locks action; got %+v", got.Actions)
	}
	if a.Verb != VerbSurface {
		t.Fatalf("dead-coord verb=%q, want %q (default is surface-only)", a.Verb, VerbSurface)
	}
	if !strings.Contains(a.Reason, "dead coord") {
		t.Fatalf("reason=%q does not name dead-coord mode", a.Reason)
	}
	if strings.Contains(a.Reason, "still alive") {
		t.Fatalf("dead-holder reason should NOT mention live-refusal; got %q", a.Reason)
	}
}

// TestReconcile_CoordLocks_DeadCoord_ApplyUnlinks: --apply upgrades
// the action to would-remove → removed and actually unlinks the lock
// file via RemoveCoordLock.
func TestReconcile_CoordLocks_DeadCoord_ApplyUnlinks(t *testing.T) {
	now := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	deps := stubDeps(now)
	lockPath := "/fake/projects/alpha/.locks/coordinator.lock"
	deps.ListCoordLocks = func() ([]CoordLockInfo, error) {
		return []CoordLockInfo{{Path: lockPath, Project: "alpha", HolderID: "11112222"}}, nil
	}
	deps.LoadAgent = func(string) (*agent.Record, error) { return nil, state.ErrNotFound }
	// Holder coord is truly dead — tmux gone — so --apply may unlink
	// safely. (Live-holder variant covered by the LiveHolder test
	// below; codex iter-2 [P2] split-brain guard.)
	deps.SessionAlive = func(name string) (bool, error) {
		if name == "fleet-11112222" {
			return false, nil
		}
		return true, nil
	}
	var removed []string
	deps.RemoveCoordLock = func(p string) error {
		removed = append(removed, p)
		return nil
	}
	got, err := Reconcile(Options{
		Apply: true, MaxAge: 24 * time.Hour,
		Kinds: []Kind{KindCoordLocks},
	}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	a, ok := findAction(got, KindCoordLocks, lockPath)
	if !ok {
		t.Fatalf("missing coord-locks action; got %+v", got.Actions)
	}
	if a.Verb != VerbRemoved {
		t.Fatalf("apply verb=%q, want %q", a.Verb, VerbRemoved)
	}
	if len(removed) != 1 || removed[0] != lockPath {
		t.Fatalf("removed=%v, want [%s]", removed, lockPath)
	}
}

// TestReconcile_CoordLocks_DeadCoord_LiveHolder_SurfaceRefusesUnlink
// pins codex review iter-2 [P2]: when LoadAgent returns ErrNotFound
// (record file missing) but the fleet-<id> tmux session is STILL
// ALIVE (e.g. operator-side `rm ~/.fleet/agents/<id>.json` while the
// coord process is still running), the classifier must NOT unlink the
// lock under --apply. flock(2) is inode-based; os.Remove of a held
// coordinator.lock leaves the live coord pinned to the unlinked inode
// while a new coord can create + flock a fresh file → split-brain
// (same shape as the mismatch live-holder defense at line 131+).
func TestReconcile_CoordLocks_DeadCoord_LiveHolder_SurfaceRefusesUnlink(t *testing.T) {
	now := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	deps := stubDeps(now)
	lockPath := "/fake/projects/alpha/.locks/coordinator.lock"
	deps.ListCoordLocks = func() ([]CoordLockInfo, error) {
		return []CoordLockInfo{{Path: lockPath, Project: "alpha", HolderID: "deadbeef"}}, nil
	}
	deps.LoadAgent = func(string) (*agent.Record, error) { return nil, state.ErrNotFound }
	deps.SessionAlive = func(name string) (bool, error) {
		if name == "fleet-deadbeef" {
			return true, nil // holder process still alive
		}
		return false, nil
	}
	deps.RemoveCoordLock = func(string) error {
		t.Fatal("RemoveCoordLock called for live-holder dead-coord — would split-brain coord mutual-exclusion (codex iter-2 [P2])")
		return nil
	}
	got, err := Reconcile(Options{
		Apply: true, MaxAge: 24 * time.Hour,
		Kinds: []Kind{KindCoordLocks},
	}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	a, ok := findAction(got, KindCoordLocks, lockPath)
	if !ok {
		t.Fatalf("missing coord-locks action; got %+v", got.Actions)
	}
	if a.Verb != VerbSurface {
		t.Fatalf("live-holder dead-coord verb=%q, want %q (must NEVER be would-remove/removed)", a.Verb, VerbSurface)
	}
	if !strings.Contains(a.Reason, "dead coord") {
		t.Fatalf("reason=%q does not name dead-coord mode", a.Reason)
	}
	if !strings.Contains(a.Reason, "still alive") || !strings.Contains(a.Reason, "split-brain") {
		t.Fatalf("reason=%q does not explain the live-holder refusal", a.Reason)
	}
}

// TestReconcile_CoordLocks_Mismatch_Surfaced is the cross-project
// hijack failure mode (the historical state PRs #173/#174 closed at
// spawn + tick time but did NOT sweep): coordinator.lock under
// project alpha holds an agent record whose record.Project="beta".
// The classifier must flag mismatch (lock dir != record project).
func TestReconcile_CoordLocks_Mismatch_Surfaced(t *testing.T) {
	// Mismatch + holder DEAD: classic historical-sweep case. PRs #173/#174
	// closed the new-hijack window at spawn + tick time; this sweeps
	// state from before then. The hijacker coord whose record.Project
	// disagrees with the lock dir has since died (tmux session gone),
	// so the unlink is safe under --apply.
	now := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	deps := stubDeps(now)
	lockPath := "/fake/projects/alpha/.locks/coordinator.lock"
	deps.ListCoordLocks = func() ([]CoordLockInfo, error) {
		return []CoordLockInfo{{Path: lockPath, Project: "alpha", HolderID: "aabbccdd"}}, nil
	}
	deps.LoadAgent = func(id string) (*agent.Record, error) {
		return &agent.Record{
			ID:          id,
			Project:     "beta", // record claims a DIFFERENT project
			TmuxSession: "fleet-" + id,
		}, nil
	}
	// Holder is dead — SessionAlive returns alive=false. Mismatch
	// branch surfaces with the standard mismatch reason.
	deps.SessionAlive = func(string) (bool, error) {
		return false, nil
	}
	got, err := Reconcile(Options{
		Apply: false, MaxAge: 24 * time.Hour,
		Kinds: []Kind{KindCoordLocks},
	}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	a, ok := findAction(got, KindCoordLocks, lockPath)
	if !ok {
		t.Fatalf("missing coord-locks action; got %+v", got.Actions)
	}
	if a.Verb != VerbSurface {
		t.Fatalf("mismatch verb=%q, want %q", a.Verb, VerbSurface)
	}
	if !strings.Contains(a.Reason, "mismatch") {
		t.Fatalf("reason=%q does not name mismatch mode", a.Reason)
	}
	if !strings.Contains(a.Reason, "alpha") || !strings.Contains(a.Reason, "beta") {
		t.Fatalf("reason=%q does not name both alpha + beta", a.Reason)
	}
	if strings.Contains(a.Reason, "split-brain") {
		t.Fatalf("dead-holder reason should NOT mention split-brain refusal; got %q", a.Reason)
	}
}

// TestReconcile_CoordLocks_Mismatch_LiveHolder_SurfaceRefusesUnlink pins
// codex review iter-1 [P1]: when the mismatch holder's tmux session is
// STILL ALIVE, the classifier must NOT unlink the lock under --apply.
// flock(2) is inode-based; os.Remove of a held coordinator.lock leaves
// the live holder on the unlinked inode while a new coord can create +
// flock a fresh file → split-brain (two coords own the same project's
// mutual-exclusion gate). Surface only, regardless of --apply.
func TestReconcile_CoordLocks_Mismatch_LiveHolder_SurfaceRefusesUnlink(t *testing.T) {
	now := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	deps := stubDeps(now)
	lockPath := "/fake/projects/alpha/.locks/coordinator.lock"
	deps.ListCoordLocks = func() ([]CoordLockInfo, error) {
		return []CoordLockInfo{{Path: lockPath, Project: "alpha", HolderID: "aabbccdd"}}, nil
	}
	deps.LoadAgent = func(id string) (*agent.Record, error) {
		return &agent.Record{
			ID:          id,
			Project:     "beta", // mismatch
			TmuxSession: "fleet-" + id,
		}, nil
	}
	deps.SessionAlive = func(name string) (bool, error) {
		if name == "fleet-aabbccdd" {
			return true, nil // holder is ALIVE
		}
		return false, nil
	}
	// RemoveCoordLock must NEVER fire when the holder is live — verifies
	// the split-brain defense end-to-end (no unlink under --apply).
	deps.RemoveCoordLock = func(string) error {
		t.Fatal("RemoveCoordLock called for live-holder mismatch — would split-brain coord mutual-exclusion (codex iter-1 [P1])")
		return nil
	}
	got, err := Reconcile(Options{
		Apply: true, MaxAge: 24 * time.Hour, // <-- --apply on
		Kinds: []Kind{KindCoordLocks},
	}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	a, ok := findAction(got, KindCoordLocks, lockPath)
	if !ok {
		t.Fatalf("missing coord-locks action; got %+v", got.Actions)
	}
	if a.Verb != VerbSurface {
		t.Fatalf("live-holder mismatch verb=%q, want %q (must NEVER be would-remove/removed)", a.Verb, VerbSurface)
	}
	if !strings.Contains(a.Reason, "mismatch") {
		t.Fatalf("reason=%q does not name mismatch mode", a.Reason)
	}
	if !strings.Contains(a.Reason, "alive") || !strings.Contains(a.Reason, "split-brain") {
		t.Fatalf("reason=%q does not explain the live-holder refusal", a.Reason)
	}
}

// TestReconcile_CoordLocks_Mismatch_ProbeError_SurfaceRefusesUnlink:
// SessionAlive probe failure is ambiguous (transport / tmux server
// missing). Treat as conservative-live and refuse to unlink — same
// shape as the live-holder case. Mirrors reconcileOrphanAgents'
// probe-error guard.
func TestReconcile_CoordLocks_Mismatch_ProbeError_SurfaceRefusesUnlink(t *testing.T) {
	now := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	deps := stubDeps(now)
	lockPath := "/fake/projects/alpha/.locks/coordinator.lock"
	deps.ListCoordLocks = func() ([]CoordLockInfo, error) {
		return []CoordLockInfo{{Path: lockPath, Project: "alpha", HolderID: "aabbccdd"}}, nil
	}
	deps.LoadAgent = func(id string) (*agent.Record, error) {
		return &agent.Record{ID: id, Project: "beta", TmuxSession: "fleet-" + id}, nil
	}
	deps.SessionAlive = func(string) (bool, error) {
		return false, errors.New("simulated tmux transport error")
	}
	deps.RemoveCoordLock = func(string) error {
		t.Fatal("RemoveCoordLock called on ambiguous probe error — must refuse to unlink (codex iter-1 [P1])")
		return nil
	}
	got, err := Reconcile(Options{
		Apply: true, MaxAge: 24 * time.Hour,
		Kinds: []Kind{KindCoordLocks},
	}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	a, ok := findAction(got, KindCoordLocks, lockPath)
	if !ok {
		t.Fatalf("missing coord-locks action; got %+v", got.Actions)
	}
	if a.Verb != VerbSurface {
		t.Fatalf("ambiguous-probe mismatch verb=%q, want %q", a.Verb, VerbSurface)
	}
}

// TestReconcile_CoordLocks_Mismatch_EmptyTmux_SurfaceRefusesUnlink:
// empty TmuxSession is unprobeable → conservative-treat-as-live so
// --apply refuses to unlink. The mismatch is still SURFACED so the
// operator knows something needs attention.
func TestReconcile_CoordLocks_Mismatch_EmptyTmux_SurfaceRefusesUnlink(t *testing.T) {
	now := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	deps := stubDeps(now)
	lockPath := "/fake/projects/alpha/.locks/coordinator.lock"
	deps.ListCoordLocks = func() ([]CoordLockInfo, error) {
		return []CoordLockInfo{{Path: lockPath, Project: "alpha", HolderID: "aabbccdd"}}, nil
	}
	deps.LoadAgent = func(id string) (*agent.Record, error) {
		return &agent.Record{ID: id, Project: "beta", TmuxSession: ""}, nil
	}
	deps.SessionAlive = func(string) (bool, error) {
		t.Fatal("SessionAlive must not be called when TmuxSession is empty")
		return false, nil
	}
	deps.RemoveCoordLock = func(string) error {
		t.Fatal("RemoveCoordLock called on empty-TmuxSession mismatch — must refuse to unlink")
		return nil
	}
	got, err := Reconcile(Options{
		Apply: true, MaxAge: 24 * time.Hour,
		Kinds: []Kind{KindCoordLocks},
	}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	a, ok := findAction(got, KindCoordLocks, lockPath)
	if !ok {
		t.Fatalf("missing coord-locks action; got %+v", got.Actions)
	}
	if a.Verb != VerbSurface {
		t.Fatalf("empty-tmux mismatch verb=%q, want %q", a.Verb, VerbSurface)
	}
}

// TestReconcile_CoordLocks_StaleTmux_Surfaced is the stale-tmux failure
// mode: agent record exists and project matches, but the tmux session
// the record names is gone (SessionAlive returns alive=false, err=nil).
// The lock body is left behind without its session.
func TestReconcile_CoordLocks_StaleTmux_Surfaced(t *testing.T) {
	now := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	deps := stubDeps(now)
	lockPath := "/fake/projects/gamma/.locks/coordinator.lock"
	deps.ListCoordLocks = func() ([]CoordLockInfo, error) {
		return []CoordLockInfo{{Path: lockPath, Project: "gamma", HolderID: "55556666"}}, nil
	}
	deps.LoadAgent = func(id string) (*agent.Record, error) {
		return &agent.Record{
			ID:          id,
			Project:     "gamma",
			TmuxSession: "fleet-" + id,
		}, nil
	}
	deps.SessionAlive = func(name string) (bool, error) {
		if name == "fleet-55556666" {
			return false, nil // session is gone
		}
		return true, nil
	}
	got, err := Reconcile(Options{
		Apply: false, MaxAge: 24 * time.Hour,
		Kinds: []Kind{KindCoordLocks},
	}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	a, ok := findAction(got, KindCoordLocks, lockPath)
	if !ok {
		t.Fatalf("missing coord-locks action; got %+v", got.Actions)
	}
	if a.Verb != VerbSurface {
		t.Fatalf("stale-tmux verb=%q, want %q", a.Verb, VerbSurface)
	}
	if !strings.Contains(a.Reason, "stale tmux") {
		t.Fatalf("reason=%q does not name stale-tmux mode", a.Reason)
	}
}

// TestReconcile_CoordLocks_HealthyCoord_Unchanged is the negative
// invariant: a coord whose record loads cleanly, whose record.Project
// matches the lock's parent dir, and whose tmux session is alive must
// NOT be classified. The classifier is opt-in to misbehavior signals,
// not opt-out from healthy coords.
func TestReconcile_CoordLocks_HealthyCoord_Unchanged(t *testing.T) {
	now := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	deps := stubDeps(now)
	lockPath := "/fake/projects/delta/.locks/coordinator.lock"
	deps.ListCoordLocks = func() ([]CoordLockInfo, error) {
		return []CoordLockInfo{{Path: lockPath, Project: "delta", HolderID: "77778888"}}, nil
	}
	deps.LoadAgent = func(id string) (*agent.Record, error) {
		return &agent.Record{
			ID:          id,
			Project:     "delta",
			TmuxSession: "fleet-" + id,
		}, nil
	}
	deps.SessionAlive = func(string) (bool, error) { return true, nil }
	got, err := Reconcile(Options{
		Apply: true, MaxAge: 24 * time.Hour,
		Kinds: []Kind{KindCoordLocks},
	}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, ok := findAction(got, KindCoordLocks, lockPath); ok {
		t.Fatalf("healthy coord flagged; got %+v", got.Actions)
	}
}

// TestReconcile_CoordLocks_EmptyHolder_Skipped pins the legacy
// zero-byte lock guard: a coordinator.lock with no holder body (a
// pre-issue-#55 legacy lock, or torn body mid-write) must NOT be
// classified. The kernel-side flock is the load-bearing signal; an
// empty body is harmless and removing it could race a coord that's
// mid-acquire-and-write.
func TestReconcile_CoordLocks_EmptyHolder_Skipped(t *testing.T) {
	now := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	deps := stubDeps(now)
	deps.ListCoordLocks = func() ([]CoordLockInfo, error) {
		return []CoordLockInfo{
			{Path: "/fake/projects/eps/.locks/coordinator.lock", Project: "eps", HolderID: ""},
			{Path: "/fake/projects/zet/.locks/coordinator.lock", Project: "zet", HolderID: "   "},
		}, nil
	}
	deps.LoadAgent = func(string) (*agent.Record, error) {
		t.Fatal("LoadAgent called for empty-holder lock; must be skipped pre-load")
		return nil, nil
	}
	got, err := Reconcile(Options{
		Apply: true, MaxAge: 24 * time.Hour,
		Kinds: []Kind{KindCoordLocks},
	}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	for _, a := range got.Actions {
		if a.Kind == KindCoordLocks {
			t.Fatalf("empty-holder lock flagged: %+v", a)
		}
	}
}

// TestReconcile_CoordLocks_LoadAgentTransientError_Preserved pins the
// surface-don't-silo guard for transient errors. A non-ErrNotFound
// LoadAgent error (parse failure, IO error) leaves the lock alone —
// classifying a transient-fail record as dead-coord would unlink a
// legitimate live coord's lock under --apply.
func TestReconcile_CoordLocks_LoadAgentTransientError_Preserved(t *testing.T) {
	now := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	deps := stubDeps(now)
	lockPath := "/fake/projects/eta/.locks/coordinator.lock"
	deps.ListCoordLocks = func() ([]CoordLockInfo, error) {
		return []CoordLockInfo{{Path: lockPath, Project: "eta", HolderID: "99990000"}}, nil
	}
	deps.LoadAgent = func(string) (*agent.Record, error) {
		return nil, errors.New("simulated parse error")
	}
	deps.RemoveCoordLock = func(string) error {
		t.Fatal("RemoveCoordLock called on transient LoadAgent error; must be skipped")
		return nil
	}
	got, err := Reconcile(Options{
		Apply: true, MaxAge: 24 * time.Hour,
		Kinds: []Kind{KindCoordLocks},
	}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, ok := findAction(got, KindCoordLocks, lockPath); ok {
		t.Fatalf("transient-error lock flagged: %+v", got.Actions)
	}
}

// TestReconcile_CoordLocks_SessionAliveProbeError_Preserved is the
// stale-tmux counterpart: if the tmux probe errors (transport failure),
// the classifier must NOT flag stale. Mirrors reconcileOrphanAgents'
// SessionAlive ambiguous-error guard.
func TestReconcile_CoordLocks_SessionAliveProbeError_Preserved(t *testing.T) {
	now := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	deps := stubDeps(now)
	lockPath := "/fake/projects/theta/.locks/coordinator.lock"
	deps.ListCoordLocks = func() ([]CoordLockInfo, error) {
		return []CoordLockInfo{{Path: lockPath, Project: "theta", HolderID: "ababcdcd"}}, nil
	}
	deps.LoadAgent = func(id string) (*agent.Record, error) {
		return &agent.Record{ID: id, Project: "theta", TmuxSession: "fleet-" + id}, nil
	}
	deps.SessionAlive = func(string) (bool, error) {
		return false, errors.New("simulated tmux transport error")
	}
	deps.RemoveCoordLock = func(string) error {
		t.Fatal("RemoveCoordLock called on SessionAlive probe error; must be skipped")
		return nil
	}
	got, err := Reconcile(Options{
		Apply: true, MaxAge: 24 * time.Hour,
		Kinds: []Kind{KindCoordLocks},
	}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, ok := findAction(got, KindCoordLocks, lockPath); ok {
		t.Fatalf("probe-error lock flagged: %+v", got.Actions)
	}
}

// TestReconcile_CoordLocks_DryRun_NeverMutates: comprehensive negative
// invariant. Even with three failure-mode lock files queued, Apply=false
// must call RemoveCoordLock zero times.
func TestReconcile_CoordLocks_DryRun_NeverMutates(t *testing.T) {
	now := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	deps := stubDeps(now)
	deps.ListCoordLocks = func() ([]CoordLockInfo, error) {
		return []CoordLockInfo{
			{Path: "/fake/projects/p1/.locks/coordinator.lock", Project: "p1", HolderID: "deadbeef"}, // dead
			{Path: "/fake/projects/p2/.locks/coordinator.lock", Project: "p2", HolderID: "11112222"}, // mismatch
			{Path: "/fake/projects/p3/.locks/coordinator.lock", Project: "p3", HolderID: "33334444"}, // stale tmux
		}, nil
	}
	deps.LoadAgent = func(id string) (*agent.Record, error) {
		switch id {
		case "deadbeef":
			return nil, state.ErrNotFound
		case "11112222":
			return &agent.Record{ID: id, Project: "OTHER", TmuxSession: "fleet-" + id}, nil
		case "33334444":
			return &agent.Record{ID: id, Project: "p3", TmuxSession: "fleet-" + id}, nil
		}
		return nil, state.ErrNotFound
	}
	deps.SessionAlive = func(name string) (bool, error) {
		if name == "fleet-33334444" {
			return false, nil
		}
		return true, nil
	}
	deps.RemoveCoordLock = func(string) error {
		t.Fatal("RemoveCoordLock called under dry-run")
		return nil
	}
	got, err := Reconcile(Options{
		Apply: false, MaxAge: 24 * time.Hour,
		Kinds: []Kind{KindCoordLocks},
	}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// All three actions should be present, with verb=surface (dry-run).
	count := 0
	for _, a := range got.Actions {
		if a.Kind != KindCoordLocks {
			continue
		}
		count++
		if a.Verb != VerbSurface {
			t.Fatalf("dry-run coord-locks verb=%q, want %q (action=%+v)", a.Verb, VerbSurface, a)
		}
	}
	if count != 3 {
		t.Fatalf("dry-run coord-locks action count=%d, want 3; report=%+v", count, got.Actions)
	}
}

// TestReconcile_CoordLocks_ProjectScope: --project filters the
// classifier to the named project's lock only.
func TestReconcile_CoordLocks_ProjectScope(t *testing.T) {
	now := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	deps := stubDeps(now)
	deps.ListCoordLocks = func() ([]CoordLockInfo, error) {
		return []CoordLockInfo{
			{Path: "/fake/projects/keep/.locks/coordinator.lock", Project: "keep", HolderID: "deadbeef"},
			{Path: "/fake/projects/skip/.locks/coordinator.lock", Project: "skip", HolderID: "00001111"},
		}, nil
	}
	deps.LoadAgent = func(id string) (*agent.Record, error) {
		// Both would fire dead-coord absent the project scope.
		return nil, state.ErrNotFound
	}
	got, err := Reconcile(Options{
		Apply: false, Project: "keep", MaxAge: 24 * time.Hour,
		Kinds: []Kind{KindCoordLocks},
	}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, ok := findAction(got, KindCoordLocks, "/fake/projects/keep/.locks/coordinator.lock"); !ok {
		t.Fatalf("missing in-scope action; got %+v", got.Actions)
	}
	if _, ok := findAction(got, KindCoordLocks, "/fake/projects/skip/.locks/coordinator.lock"); ok {
		t.Fatalf("out-of-scope lock flagged; got %+v", got.Actions)
	}
}

// TestListCoordLocksOnDisk_ProductionLister covers the on-disk parser
// that DefaultDeps wires up: scan ~/.fleet/projects/<p>/.locks/coordinator.lock,
// skip the reserved .locks sibling dir under projects/, parse holder
// body's first line as 8-hex-char agent ID (anything else → empty).
func TestListCoordLocksOnDisk_ProductionLister(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	mustMkdir := func(p string) {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
	}
	mustWrite := func(p, content string) {
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	projects := filepath.Join(tmp, "projects")
	mustMkdir(projects)
	// alpha: well-formed lock body with 8-hex holder.
	mustMkdir(filepath.Join(projects, "alpha", ".locks"))
	mustWrite(filepath.Join(projects, "alpha", ".locks", "coordinator.lock"), "aabbccdd\n")
	// beta: zero-byte lock (legacy / pre-issue-#55).
	mustMkdir(filepath.Join(projects, "beta", ".locks"))
	mustWrite(filepath.Join(projects, "beta", ".locks", "coordinator.lock"), "")
	// gamma: garbage holder (not 8-hex).
	mustMkdir(filepath.Join(projects, "gamma", ".locks"))
	mustWrite(filepath.Join(projects, "gamma", ".locks", "coordinator.lock"), "not-an-id-here\n")
	// delta: no .locks dir at all → skipped from output.
	mustMkdir(filepath.Join(projects, "delta"))
	// .locks: reserved sibling under projects/ — MUST be excluded.
	mustMkdir(filepath.Join(projects, ".locks"))
	mustWrite(filepath.Join(projects, ".locks", "coordinator.lock"), "11223344\n")

	got, err := listCoordLocksOnDisk()
	if err != nil {
		t.Fatalf("listCoordLocksOnDisk: %v", err)
	}
	byProj := map[string]CoordLockInfo{}
	for _, l := range got {
		byProj[l.Project] = l
	}
	if _, ok := byProj[".locks"]; ok {
		t.Fatalf("reserved .locks sibling included in output")
	}
	if _, ok := byProj["delta"]; ok {
		t.Fatalf("project without .locks/coordinator.lock should be skipped")
	}
	if a, ok := byProj["alpha"]; !ok || a.HolderID != "aabbccdd" {
		t.Fatalf("alpha holder=%v, want aabbccdd; got=%+v", a.HolderID, byProj)
	}
	if b, ok := byProj["beta"]; !ok || b.HolderID != "" {
		t.Fatalf("beta should be present with empty holder; got=%+v", b)
	}
	if g, ok := byProj["gamma"]; !ok || g.HolderID != "" {
		t.Fatalf("gamma holder=%v, want empty (garbage body)", g.HolderID)
	}
}

// TestReconcile_OrphanAgents_EmptyTmuxSession_Skipped pins the fix
// for codex iter-3 [P1]: legacy / partially populated agent records
// with empty TmuxSession would get `tmux has-session -t ""` probed,
// which CAN match an ambiguous error string and falsely classify the
// record as dead → archive. Skip empty sessions entirely.
func TestReconcile_OrphanAgents_EmptyTmuxSession_Skipped(t *testing.T) {
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	deps := stubDeps(now)
	deps.ListAgents = func() ([]*agent.Record, error) {
		return []*agent.Record{
			{ID: "legacy-record-aabb", TmuxSession: ""}, // empty session
		}, nil
	}
	probed := false
	deps.SessionAlive = func(string) (bool, error) {
		probed = true
		// If we ever get here, the test FAILS — we should never probe
		// an empty session name.
		return false, nil
	}
	var archived []string
	deps.ArchiveAgent = func(r *agent.Record) error {
		archived = append(archived, r.ID)
		return nil
	}

	got, err := Reconcile(Options{Apply: true, MaxAge: 24 * time.Hour,
		Kinds: []Kind{KindOrphanAgents}}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if probed {
		t.Fatalf("SessionAlive was called for empty TmuxSession; must be skipped before probe")
	}
	if len(archived) != 0 {
		t.Fatalf("archived %v for empty TmuxSession; must be skipped", archived)
	}
	if _, ok := findAction(got, KindOrphanAgents, "legacy-record-aabb"); ok {
		t.Fatalf("legacy empty-session record produced action: %+v", got.Actions)
	}
}

// TestRemoveCoordLockFile_HeldByLiveCoord_RefusesUnlink pins codex
// review iter-4 [P1]: the production unlink path MUST acquire
// LOCK_EX|LOCK_NB on the coordinator.lock file BEFORE os.Remove. If
// another process holds the flock (the realistic concurrent-start
// race: a coord spawning between our liveness probe and this unlink),
// the function must return an error instead of unlinking — flock(2)
// is inode-based, so os.Remove on a held lock leaves the holder on
// the unlinked inode while a new coord can create + flock a fresh
// file → split-brain coord mutual-exclusion.
//
// The classifier's appendCoordLockAction surfaces this error as
// "remove failed: ..." with Verb=surface, so --apply gracefully
// degrades to surface on the realistic race window.
func TestRemoveCoordLockFile_HeldByLiveCoord_RefusesUnlink(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "coordinator.lock")
	if err := os.WriteFile(lockPath, []byte("aabbccdd\n"), 0o644); err != nil {
		t.Fatalf("seed lock: %v", err)
	}
	// Acquire LOCK_EX as a separate fd — simulates a live coord
	// holding the flock at the moment removeCoordLockFile attempts
	// the unlink.
	holder, err := os.OpenFile(lockPath, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open holder: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Flock(int(holder.Fd()), syscall.LOCK_UN)
		_ = holder.Close()
	})
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("holder Flock: %v", err)
	}

	rerr := removeCoordLockFile(lockPath)
	if rerr == nil {
		t.Fatal("removeCoordLockFile silently unlinked a held lock — split-brain risk")
	}
	if !strings.Contains(rerr.Error(), "currently held") && !strings.Contains(rerr.Error(), "split-brain") {
		t.Errorf("error %q does not explain the live-holder refusal", rerr.Error())
	}

	// File must still exist on disk — the unlink was refused.
	if _, statErr := os.Stat(lockPath); statErr != nil {
		t.Errorf("lock file disappeared even though removal was refused: %v", statErr)
	}
}

// TestRemoveCoordLockFile_NotHeld_Unlinks confirms the happy path:
// when no one holds the flock, removeCoordLockFile acquires it and
// unlinks atomically.
func TestRemoveCoordLockFile_NotHeld_Unlinks(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "coordinator.lock")
	if err := os.WriteFile(lockPath, []byte("deadbeef\n"), 0o644); err != nil {
		t.Fatalf("seed lock: %v", err)
	}
	if err := removeCoordLockFile(lockPath); err != nil {
		t.Fatalf("removeCoordLockFile: %v", err)
	}
	if _, statErr := os.Stat(lockPath); !os.IsNotExist(statErr) {
		t.Errorf("lock file still exists after unlink; stat=%v", statErr)
	}
}

// TestRemoveCoordLockFile_Missing_NoError confirms the ENOENT-tolerant
// contract: two operators racing `fleet gc --apply` must not see
// spurious errors when the second pass finds the file already gone.
func TestRemoveCoordLockFile_Missing_NoError(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing-coordinator.lock")
	if err := removeCoordLockFile(missing); err != nil {
		t.Fatalf("removeCoordLockFile on missing path returned error: %v", err)
	}
}

// TestRemoveIfSameInode_InodeSwap_RefusesUnlink pins codex review iter-5
// [P2]: after Flock succeeds, the unlink path MUST recheck that the
// inode at `path` still equals the inode of our fd. Without this, two
// overlapping `fleet gc --apply` passes + a concurrent coord-spawn can
// leave the first pass holding an unlinked inode while a new dirent at
// the same path points at a freshly created LIVE lock — os.Remove
// would delete the live replacement and split-brain returns.
//
// Synchronously reproduce the swap by:
//  1. Creating the original lock file at `path` → inode I1.
//  2. Opening + flocking it ourselves (simulating the gc helper post-
//     OpenFile + post-Flock state, holding I1).
//  3. Unlinking `path` while we hold the fd (dirent gone, I1 still
//     alive via our open fd).
//  4. Creating a fresh file at `path` → inode I2.
//  5. Calling removeIfSameInode(fd_I1, path) → must return the
//     inode-swap error, leaving the I2 dirent intact.
func TestRemoveIfSameInode_InodeSwap_RefusesUnlink(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "coordinator.lock")
	if err := os.WriteFile(lockPath, []byte("aabbccdd\n"), 0o644); err != nil {
		t.Fatalf("seed lock: %v", err)
	}
	fd1, err := os.OpenFile(lockPath, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open original: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Flock(int(fd1.Fd()), syscall.LOCK_UN)
		_ = fd1.Close()
	})
	if err := syscall.Flock(int(fd1.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("flock original: %v", err)
	}
	// Unlink the original dirent — fd1 still holds I1 alive.
	if err := os.Remove(lockPath); err != nil {
		t.Fatalf("unlink original: %v", err)
	}
	// Create a fresh dirent at the same path → new inode I2 (simulates
	// a concurrent coord-spawn between our flock and our recheck).
	if err := os.WriteFile(lockPath, []byte("11112222\n"), 0o644); err != nil {
		t.Fatalf("create replacement: %v", err)
	}
	// Capture I2's inode for the post-call assertion.
	var pstBefore syscall.Stat_t
	if err := syscall.Stat(lockPath, &pstBefore); err != nil {
		t.Fatalf("stat replacement: %v", err)
	}

	rerr := removeIfSameInode(fd1, lockPath)
	if rerr == nil {
		t.Fatal("removeIfSameInode silently unlinked the replacement live lock — split-brain re-introduced")
	}
	if !strings.Contains(rerr.Error(), "different inode") && !strings.Contains(rerr.Error(), "replaced") {
		t.Errorf("error %q does not explain the inode-swap refusal", rerr.Error())
	}

	// Replacement file must still exist on disk with its original inode.
	var pstAfter syscall.Stat_t
	if err := syscall.Stat(lockPath, &pstAfter); err != nil {
		t.Errorf("replacement file disappeared after inode-swap refusal: %v", err)
	} else if pstAfter.Ino != pstBefore.Ino {
		t.Errorf("replacement inode changed: before=%d after=%d", pstBefore.Ino, pstAfter.Ino)
	}
}

// TestRemoveIfSameInode_PathVanished_NoError confirms the safe-collapse
// branch: if `path` is gone between our flock and recheck (e.g. an
// earlier gc pass already won the race AND no spawn rebound the path),
// removeIfSameInode returns nil. Our orphan inode reaps on close;
// nothing to do.
func TestRemoveIfSameInode_PathVanished_NoError(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "coordinator.lock")
	if err := os.WriteFile(lockPath, []byte("ababcdcd\n"), 0o644); err != nil {
		t.Fatalf("seed lock: %v", err)
	}
	fd, err := os.OpenFile(lockPath, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = fd.Close() })
	if err := syscall.Flock(int(fd.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("flock: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Flock(int(fd.Fd()), syscall.LOCK_UN) })

	// Path vanishes after our flock — no replacement.
	if err := os.Remove(lockPath); err != nil {
		t.Fatalf("unlink: %v", err)
	}

	if err := removeIfSameInode(fd, lockPath); err != nil {
		t.Errorf("removeIfSameInode returned error on safe-collapse path-vanished branch: %v", err)
	}
}

// TestRemoveIfSameInode_SameInode_Unlinks confirms the happy path: the
// fd inode equals the path inode → os.Remove fires + returns nil.
func TestRemoveIfSameInode_SameInode_Unlinks(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "coordinator.lock")
	if err := os.WriteFile(lockPath, []byte("77778888\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	fd, err := os.OpenFile(lockPath, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = fd.Close() })
	if err := syscall.Flock(int(fd.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("flock: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Flock(int(fd.Fd()), syscall.LOCK_UN) })

	if err := removeIfSameInode(fd, lockPath); err != nil {
		t.Fatalf("removeIfSameInode: %v", err)
	}
	if _, statErr := os.Stat(lockPath); !os.IsNotExist(statErr) {
		t.Errorf("lock file still exists; stat=%v", statErr)
	}
}

// ----- fleet#177 worker-records classifier tests -----
//
// KindWorkerRecords sweeps stale `~/.fleet/projects/<p>/workers/<slug>/state.json`
// files across every project. Four failure modes:
//
//   - mismatch       — state.json's `project` field != parent-dir project name
//                      (cross-project taint from before fleet#170/#171 closed
//                      the new-write window)
//   - dead-pid       — `pid` set + process gone + updated_at older than 24h
//   - stale-heartbeat — `pid=0` + updated_at older than 24h
//   - task-terminal  — task in tasks.md is done|abandoned but worker dir still
//                      on disk
//
// Surface-only by default; --apply removes the worker directory (state.json +
// output.log + scratch) via RemoveWorkerRecord. The worktree (separate path)
// is NEVER touched — that's KindWorktrees' job.

// TestReconcile_WorkerRecords_Mismatch_Surfaced pins the cross-project
// taint case: state.json under projects-fleet/workers/<slug>/ but the
// state.project field says "projects-rainier".
func TestReconcile_WorkerRecords_Mismatch_Surfaced(t *testing.T) {
	now := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	deps := stubDeps(now)
	workerPath := "/fake/projects/projects-fleet/workers/etf-dashboard-publish-cr-31aa/"
	deps.ListWorkerRecords = func() ([]WorkerRecordInfo, error) {
		return []WorkerRecordInfo{{
			Project: "projects-fleet",
			Slug:    "etf-dashboard-publish-cr-31aa",
			Path:    workerPath,
		}}, nil
	}
	deps.LoadWorkerState = func(p string) (WorkerState, error) {
		if p != workerPath {
			t.Fatalf("LoadWorkerState path=%q want %q", p, workerPath)
		}
		return WorkerState{
			Slug:      "etf-dashboard-publish-cr-31aa",
			Project:   "projects-rainier", // mismatch vs parent dir
			Pid:       0,
			UpdatedAt: now.Add(-25 * time.Hour),
		}, nil
	}
	got, err := Reconcile(Options{
		Apply: false, MaxAge: 24 * time.Hour,
		Kinds: []Kind{KindWorkerRecords},
	}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	a, ok := findAction(got, KindWorkerRecords, workerPath)
	if !ok {
		t.Fatalf("missing worker-records action; got %+v", got.Actions)
	}
	if a.Verb != VerbSurface {
		t.Fatalf("mismatch verb=%q, want %q (default is surface-only)", a.Verb, VerbSurface)
	}
	if !strings.Contains(a.Reason, "mismatch") {
		t.Fatalf("reason=%q does not name mismatch mode", a.Reason)
	}
	if !strings.Contains(a.Reason, "projects-rainier") {
		t.Fatalf("reason=%q does not name the recorded project", a.Reason)
	}
}

// TestReconcile_WorkerRecords_Mismatch_ApplyRemoves: --apply on the
// mismatch branch upgrades the action to removed and calls
// RemoveWorkerRecord with the worker dir path.
func TestReconcile_WorkerRecords_Mismatch_ApplyRemoves(t *testing.T) {
	now := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	deps := stubDeps(now)
	workerPath := "/fake/projects/projects-fleet/workers/cross-taint-aaaa/"
	deps.ListWorkerRecords = func() ([]WorkerRecordInfo, error) {
		return []WorkerRecordInfo{{
			Project: "projects-fleet",
			Slug:    "cross-taint-aaaa",
			Path:    workerPath,
		}}, nil
	}
	deps.LoadWorkerState = func(string) (WorkerState, error) {
		return WorkerState{
			Slug:      "cross-taint-aaaa",
			Project:   "projects-rainier",
			Pid:       0,
			UpdatedAt: now.Add(-25 * time.Hour),
		}, nil
	}
	var removed []string
	deps.RemoveWorkerRecord = func(p string) error {
		removed = append(removed, p)
		return nil
	}
	got, err := Reconcile(Options{
		Apply: true, MaxAge: 24 * time.Hour,
		Kinds: []Kind{KindWorkerRecords},
	}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	a, ok := findAction(got, KindWorkerRecords, workerPath)
	if !ok {
		t.Fatalf("missing worker-records action; got %+v", got.Actions)
	}
	if a.Verb != VerbRemoved {
		t.Fatalf("apply verb=%q, want %q", a.Verb, VerbRemoved)
	}
	if len(removed) != 1 || removed[0] != workerPath {
		t.Fatalf("removed=%v, want [%s]", removed, workerPath)
	}
}

// TestReconcile_WorkerRecords_Mismatch_LivePid_RefusesUnlink pins the
// live-PID safety gate inside the mismatch apply path (review iter-1
// follow-up). A cross-project record claiming a still-live PID is
// surfaced regardless of liveness — the operator needs to see the bug
// — but `--apply` must NOT remove the worker dir while the recorded
// PID is alive. Yanking state.json out from under a running subagent
// orphans its in-flight phase writes. Mirrors the live-PID guard at
// step 2 of the classifier but applied INSIDE the mismatch branch
// because mismatch is checked first.
func TestReconcile_WorkerRecords_Mismatch_LivePid_RefusesUnlink(t *testing.T) {
	now := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	deps := stubDeps(now)
	workerPath := "/fake/projects/projects-fleet/workers/live-mismatch-aaaa/"
	deps.ListWorkerRecords = func() ([]WorkerRecordInfo, error) {
		return []WorkerRecordInfo{{
			Project: "projects-fleet",
			Slug:    "live-mismatch-aaaa",
			Path:    workerPath,
		}}, nil
	}
	deps.LoadWorkerState = func(string) (WorkerState, error) {
		return WorkerState{
			Slug:      "live-mismatch-aaaa",
			Project:   "projects-rainier", // mismatch
			Pid:       12345,              // alive
			UpdatedAt: now.Add(-25 * time.Hour),
		}, nil
	}
	deps.PidAlive = func(pid int) bool {
		if pid != 12345 {
			t.Fatalf("PidAlive pid=%d want 12345", pid)
		}
		return true
	}
	deps.RemoveWorkerRecord = func(string) error {
		t.Fatal("RemoveWorkerRecord called on live-pid mismatch — would orphan running worker")
		return nil
	}
	got, err := Reconcile(Options{
		Apply: true, MaxAge: 24 * time.Hour,
		Kinds: []Kind{KindWorkerRecords},
	}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	a, ok := findAction(got, KindWorkerRecords, workerPath)
	if !ok {
		t.Fatalf("missing worker-records mismatch surface; got %+v", got.Actions)
	}
	if a.Verb != VerbSurface {
		t.Fatalf("live-pid mismatch verb=%q, want %q (must surface, never apply)", a.Verb, VerbSurface)
	}
	if !strings.Contains(a.Reason, "mismatch") {
		t.Fatalf("reason=%q does not name mismatch mode", a.Reason)
	}
	if !strings.Contains(a.Reason, "still alive") && !strings.Contains(a.Reason, "alive") {
		t.Fatalf("reason=%q does not explain the live-pid refusal", a.Reason)
	}
}

// TestReconcile_WorkerRecords_DeadPid_Surfaced: pid set + process dead +
// updated_at older than 24h is the dead-pid failure mode.
func TestReconcile_WorkerRecords_DeadPid_Surfaced(t *testing.T) {
	now := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	deps := stubDeps(now)
	workerPath := "/fake/projects/projects-fleet/workers/dead-pid-bbbb/"
	deps.ListWorkerRecords = func() ([]WorkerRecordInfo, error) {
		return []WorkerRecordInfo{{
			Project: "projects-fleet",
			Slug:    "dead-pid-bbbb",
			Path:    workerPath,
		}}, nil
	}
	deps.LoadWorkerState = func(string) (WorkerState, error) {
		return WorkerState{
			Slug:      "dead-pid-bbbb",
			Project:   "projects-fleet",
			Pid:       99999,
			UpdatedAt: now.Add(-25 * time.Hour),
		}, nil
	}
	deps.PidAlive = func(pid int) bool {
		if pid != 99999 {
			t.Fatalf("PidAlive pid=%d want 99999", pid)
		}
		return false
	}
	got, err := Reconcile(Options{
		Apply: false, MaxAge: 24 * time.Hour,
		Kinds: []Kind{KindWorkerRecords},
	}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	a, ok := findAction(got, KindWorkerRecords, workerPath)
	if !ok {
		t.Fatalf("missing worker-records action; got %+v", got.Actions)
	}
	if a.Verb != VerbSurface {
		t.Fatalf("dead-pid verb=%q, want %q (surface only)", a.Verb, VerbSurface)
	}
	if !strings.Contains(a.Reason, "dead-pid") {
		t.Fatalf("reason=%q does not name dead-pid mode", a.Reason)
	}
}

// TestReconcile_WorkerRecords_LivePid_Unchanged pins the live-PID guard:
// when the worker's pid is still alive, the classifier is a no-op even
// if updated_at is ancient. Reaping a live worker would orphan the
// running process.
func TestReconcile_WorkerRecords_LivePid_Unchanged(t *testing.T) {
	now := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	deps := stubDeps(now)
	workerPath := "/fake/projects/projects-fleet/workers/live-cccc/"
	deps.ListWorkerRecords = func() ([]WorkerRecordInfo, error) {
		return []WorkerRecordInfo{{
			Project: "projects-fleet",
			Slug:    "live-cccc",
			Path:    workerPath,
		}}, nil
	}
	deps.LoadWorkerState = func(string) (WorkerState, error) {
		return WorkerState{
			Slug:      "live-cccc",
			Project:   "projects-fleet",
			Pid:       12345,
			UpdatedAt: now.Add(-30 * 24 * time.Hour), // 30 days stale
		}, nil
	}
	deps.PidAlive = func(int) bool { return true }
	deps.RemoveWorkerRecord = func(string) error {
		t.Fatal("RemoveWorkerRecord called on live-pid worker — would orphan running process")
		return nil
	}
	got, err := Reconcile(Options{
		Apply: true, MaxAge: 24 * time.Hour,
		Kinds: []Kind{KindWorkerRecords},
	}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, ok := findAction(got, KindWorkerRecords, workerPath); ok {
		t.Fatalf("live-pid worker produced an action; want no-op. got %+v", got.Actions)
	}
}

// TestReconcile_WorkerRecords_StaleHeartbeat_Surfaced: pid=0 +
// updated_at older than 24h (worker died before claiming the row OR was
// never spawned at all).
func TestReconcile_WorkerRecords_StaleHeartbeat_Surfaced(t *testing.T) {
	now := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	deps := stubDeps(now)
	workerPath := "/fake/projects/projects-fleet/workers/stale-dddd/"
	deps.ListWorkerRecords = func() ([]WorkerRecordInfo, error) {
		return []WorkerRecordInfo{{
			Project: "projects-fleet",
			Slug:    "stale-dddd",
			Path:    workerPath,
		}}, nil
	}
	deps.LoadWorkerState = func(string) (WorkerState, error) {
		return WorkerState{
			Slug:      "stale-dddd",
			Project:   "projects-fleet",
			Pid:       0,
			UpdatedAt: now.Add(-25 * time.Hour),
		}, nil
	}
	got, err := Reconcile(Options{
		Apply: false, MaxAge: 24 * time.Hour,
		Kinds: []Kind{KindWorkerRecords},
	}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	a, ok := findAction(got, KindWorkerRecords, workerPath)
	if !ok {
		t.Fatalf("missing worker-records action; got %+v", got.Actions)
	}
	if a.Verb != VerbSurface {
		t.Fatalf("stale-heartbeat verb=%q, want %q", a.Verb, VerbSurface)
	}
	if !strings.Contains(a.Reason, "stale-heartbeat") {
		t.Fatalf("reason=%q does not name stale-heartbeat mode", a.Reason)
	}
}

// TestReconcile_WorkerRecords_StaleHeartbeat_FreshSkipped: pid=0 but
// updated_at is within the 24h window — worker is mid-spawn or
// genuinely active without a pid (e.g., subagent dispatch overlap).
// Must be left alone.
func TestReconcile_WorkerRecords_StaleHeartbeat_FreshSkipped(t *testing.T) {
	now := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	deps := stubDeps(now)
	workerPath := "/fake/projects/projects-fleet/workers/fresh-eeee/"
	deps.ListWorkerRecords = func() ([]WorkerRecordInfo, error) {
		return []WorkerRecordInfo{{
			Project: "projects-fleet",
			Slug:    "fresh-eeee",
			Path:    workerPath,
		}}, nil
	}
	deps.LoadWorkerState = func(string) (WorkerState, error) {
		return WorkerState{
			Slug:      "fresh-eeee",
			Project:   "projects-fleet",
			Pid:       0,
			UpdatedAt: now.Add(-30 * time.Minute), // way inside 24h window
		}, nil
	}
	got, err := Reconcile(Options{
		Apply: false, MaxAge: 24 * time.Hour,
		Kinds: []Kind{KindWorkerRecords},
	}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, ok := findAction(got, KindWorkerRecords, workerPath); ok {
		t.Fatalf("fresh stale-heartbeat candidate produced an action; want no-op. got %+v", got.Actions)
	}
}

// TestReconcile_WorkerRecords_TaskTerminal_Surfaced: tasks.md says the
// task is done; the worker dir is orphan post-archive.
func TestReconcile_WorkerRecords_TaskTerminal_Surfaced(t *testing.T) {
	now := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	deps := stubDeps(now)
	workerPath := "/fake/projects/projects-fleet/workers/finished-ffff/"
	deps.ListWorkerRecords = func() ([]WorkerRecordInfo, error) {
		return []WorkerRecordInfo{{
			Project: "projects-fleet",
			Slug:    "finished-ffff",
			Path:    workerPath,
		}}, nil
	}
	deps.LoadWorkerState = func(string) (WorkerState, error) {
		return WorkerState{
			Slug:      "finished-ffff",
			Project:   "projects-fleet",
			Pid:       0,
			UpdatedAt: now.Add(-2 * time.Hour), // fresh — not stale-heartbeat
		}, nil
	}
	deps.LoadTaskStatus = func(project, slug string) (string, error) {
		if project != "projects-fleet" || slug != "finished-ffff" {
			t.Fatalf("LoadTaskStatus(%q,%q) wrong args", project, slug)
		}
		return "done", nil
	}
	got, err := Reconcile(Options{
		Apply: false, MaxAge: 24 * time.Hour,
		Kinds: []Kind{KindWorkerRecords},
	}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	a, ok := findAction(got, KindWorkerRecords, workerPath)
	if !ok {
		t.Fatalf("missing worker-records action; got %+v", got.Actions)
	}
	if a.Verb != VerbSurface {
		t.Fatalf("task-terminal verb=%q, want %q", a.Verb, VerbSurface)
	}
	if !strings.Contains(a.Reason, "task-terminal") {
		t.Fatalf("reason=%q does not name task-terminal mode", a.Reason)
	}
	if !strings.Contains(a.Reason, "done") {
		t.Fatalf("reason=%q does not include the task status", a.Reason)
	}
}

// TestReconcile_WorkerRecords_TaskTerminal_Abandoned_Surfaced ensures the
// classifier honors "abandoned" as a terminal status, not just "done".
func TestReconcile_WorkerRecords_TaskTerminal_Abandoned_Surfaced(t *testing.T) {
	now := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	deps := stubDeps(now)
	workerPath := "/fake/projects/projects-fleet/workers/abandoned-gggg/"
	deps.ListWorkerRecords = func() ([]WorkerRecordInfo, error) {
		return []WorkerRecordInfo{{
			Project: "projects-fleet",
			Slug:    "abandoned-gggg",
			Path:    workerPath,
		}}, nil
	}
	deps.LoadWorkerState = func(string) (WorkerState, error) {
		return WorkerState{
			Slug:      "abandoned-gggg",
			Project:   "projects-fleet",
			Pid:       0,
			UpdatedAt: now.Add(-2 * time.Hour),
		}, nil
	}
	deps.LoadTaskStatus = func(string, string) (string, error) {
		return "abandoned", nil
	}
	got, err := Reconcile(Options{
		Apply: false, MaxAge: 24 * time.Hour,
		Kinds: []Kind{KindWorkerRecords},
	}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	a, ok := findAction(got, KindWorkerRecords, workerPath)
	if !ok {
		t.Fatalf("missing worker-records action; got %+v", got.Actions)
	}
	if a.Verb != VerbSurface {
		t.Fatalf("task-terminal verb=%q, want %q", a.Verb, VerbSurface)
	}
	if !strings.Contains(a.Reason, "abandoned") {
		t.Fatalf("reason=%q does not include 'abandoned'", a.Reason)
	}
}

// TestReconcile_WorkerRecords_TaskInProgress_RefusesUnlink pins the
// task-in-progress guard: even if the state.json looks orphan, when
// tasks.md still says in-progress, the classifier MUST surface but
// MUST NOT apply (coord reconcile owns that path).
func TestReconcile_WorkerRecords_TaskInProgress_RefusesUnlink(t *testing.T) {
	now := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	deps := stubDeps(now)
	workerPath := "/fake/projects/projects-fleet/workers/inprog-hhhh/"
	deps.ListWorkerRecords = func() ([]WorkerRecordInfo, error) {
		return []WorkerRecordInfo{{
			Project: "projects-fleet",
			Slug:    "inprog-hhhh",
			Path:    workerPath,
		}}, nil
	}
	deps.LoadWorkerState = func(string) (WorkerState, error) {
		return WorkerState{
			Slug:      "inprog-hhhh",
			Project:   "projects-fleet",
			Pid:       0,
			UpdatedAt: now.Add(-25 * time.Hour), // looks stale-heartbeat
		}, nil
	}
	deps.LoadTaskStatus = func(string, string) (string, error) {
		return "in-progress", nil
	}
	deps.RemoveWorkerRecord = func(string) error {
		t.Fatal("RemoveWorkerRecord called for in-progress task — coord reconcile owns that path")
		return nil
	}
	got, err := Reconcile(Options{
		Apply: true, MaxAge: 24 * time.Hour,
		Kinds: []Kind{KindWorkerRecords},
	}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	a, ok := findAction(got, KindWorkerRecords, workerPath)
	if !ok {
		t.Fatalf("missing worker-records action; got %+v", got.Actions)
	}
	if a.Verb != VerbSurface {
		t.Fatalf("in-progress task verb=%q, want %q (must never auto-remove)", a.Verb, VerbSurface)
	}
	if !strings.Contains(a.Reason, "in-progress") && !strings.Contains(a.Reason, "coord reconcile") {
		t.Fatalf("reason=%q does not explain the in-progress refusal", a.Reason)
	}
}

// TestReconcile_WorkerRecords_DryRun_NeverMutates pins the dry-run
// invariant: dry-run produces would-* actions, never calls
// RemoveWorkerRecord, regardless of the orphan mode.
func TestReconcile_WorkerRecords_DryRun_NeverMutates(t *testing.T) {
	now := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	deps := stubDeps(now)
	workerPath := "/fake/projects/projects-fleet/workers/dryrun-iiii/"
	deps.ListWorkerRecords = func() ([]WorkerRecordInfo, error) {
		return []WorkerRecordInfo{{
			Project: "projects-fleet",
			Slug:    "dryrun-iiii",
			Path:    workerPath,
		}}, nil
	}
	deps.LoadWorkerState = func(string) (WorkerState, error) {
		return WorkerState{
			Slug:      "dryrun-iiii",
			Project:   "projects-rainier", // mismatch
			Pid:       0,
			UpdatedAt: now.Add(-25 * time.Hour),
		}, nil
	}
	deps.RemoveWorkerRecord = func(string) error {
		t.Fatal("RemoveWorkerRecord called in dry-run mode")
		return nil
	}
	got, err := Reconcile(Options{
		Apply: false, MaxAge: 24 * time.Hour,
		Kinds: []Kind{KindWorkerRecords},
	}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	a, ok := findAction(got, KindWorkerRecords, workerPath)
	if !ok {
		t.Fatalf("missing worker-records action; got %+v", got.Actions)
	}
	if a.Verb != VerbSurface {
		t.Fatalf("dry-run verb=%q, want %q", a.Verb, VerbSurface)
	}
}

// TestReconcile_WorkerRecords_ParseError_SurfacedAsWarning: when
// LoadWorkerState fails to parse the state.json (corrupt / partial
// write / hand-edit), the classifier surfaces a warning rather than
// silently skipping. surface-don't-silo.
func TestReconcile_WorkerRecords_ParseError_SurfacedAsWarning(t *testing.T) {
	now := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	deps := stubDeps(now)
	workerPath := "/fake/projects/projects-fleet/workers/corrupt-jjjj/"
	deps.ListWorkerRecords = func() ([]WorkerRecordInfo, error) {
		return []WorkerRecordInfo{{
			Project: "projects-fleet",
			Slug:    "corrupt-jjjj",
			Path:    workerPath,
		}}, nil
	}
	deps.LoadWorkerState = func(string) (WorkerState, error) {
		return WorkerState{}, errors.New("unexpected end of JSON input")
	}
	got, err := Reconcile(Options{
		Apply: false, MaxAge: 24 * time.Hour,
		Kinds: []Kind{KindWorkerRecords},
	}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	a, ok := findAction(got, KindWorkerRecords, workerPath)
	if !ok {
		t.Fatalf("missing worker-records parse-error action; got %+v", got.Actions)
	}
	if a.Verb != VerbSurface {
		t.Fatalf("parse-error verb=%q, want %q", a.Verb, VerbSurface)
	}
	if !strings.Contains(a.Reason, "parse") {
		t.Fatalf("reason=%q does not name parse failure", a.Reason)
	}
}

// TestReconcile_WorkerRecords_Healthy_NoAction pins the no-noise contract:
// a healthy worker (fresh updated_at, no mismatch, task in-progress)
// produces zero actions.
func TestReconcile_WorkerRecords_Healthy_NoAction(t *testing.T) {
	now := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	deps := stubDeps(now)
	deps.ListWorkerRecords = func() ([]WorkerRecordInfo, error) {
		return []WorkerRecordInfo{{
			Project: "projects-fleet",
			Slug:    "healthy-kkkk",
			Path:    "/fake/projects/projects-fleet/workers/healthy-kkkk/",
		}}, nil
	}
	deps.LoadWorkerState = func(string) (WorkerState, error) {
		return WorkerState{
			Slug:      "healthy-kkkk",
			Project:   "projects-fleet",
			Pid:       0,
			UpdatedAt: now.Add(-5 * time.Minute),
		}, nil
	}
	deps.LoadTaskStatus = func(string, string) (string, error) {
		return "in-progress", nil
	}
	got, err := Reconcile(Options{
		Apply: false, MaxAge: 24 * time.Hour,
		Kinds: []Kind{KindWorkerRecords},
	}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	for _, a := range got.Actions {
		if a.Kind == KindWorkerRecords {
			t.Errorf("healthy worker produced action %+v; want zero", a)
		}
	}
}

// TestReconcile_WorkerRecords_MissingDep_Errors pins fail-fast: every
// required Deps hook (ListWorkerRecords, LoadWorkerState, LoadTaskStatus,
// PidAlive) must be wired. Missing any → error rather than silent skip.
func TestReconcile_WorkerRecords_MissingDep_Errors(t *testing.T) {
	now := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	deps := stubDeps(now)
	deps.ListWorkerRecords = nil // nil out the required hook
	_, err := Reconcile(Options{
		Apply: false, MaxAge: 24 * time.Hour,
		Kinds: []Kind{KindWorkerRecords},
	}, deps)
	if err == nil {
		t.Fatal("Reconcile with missing ListWorkerRecords should error")
	}
	if !strings.Contains(err.Error(), "ListWorkerRecords") {
		t.Errorf("error=%q does not name missing dep", err.Error())
	}
}

// TestListWorkerRecordsOnDisk_ProductionLister exercises the production
// `~/.fleet/projects/<p>/workers/<slug>/state.json` walker against a
// fake home tree. Asserts:
//   - state.json files under workers/<slug>/ are yielded
//   - directories under workers/archive/ are skipped (archive subtree
//     is fleet's own cold-storage and not a live worker)
//   - non-state.json files (output.log, scratch/) are not yielded
//   - parent-dir project name is plumbed into WorkerRecordInfo.Project
func TestListWorkerRecordsOnDisk_ProductionLister(t *testing.T) {
	fleetHome := t.TempDir()
	t.Setenv("FLEET_HOME", fleetHome)

	// Build:
	//   projects/projects-fleet/workers/live-slug-aaaa/state.json
	//   projects/projects-fleet/workers/live-slug-aaaa/output.log     (skipped)
	//   projects/projects-fleet/workers/archive/old-bbbb-20260101/state.json (skipped)
	//   projects/projects-rainier/workers/cross-cccc/state.json
	type seed struct {
		path string
		want bool
	}
	seeds := []seed{
		{"projects/projects-fleet/workers/live-slug-aaaa/state.json", true},
		{"projects/projects-fleet/workers/live-slug-aaaa/output.log", false},
		{"projects/projects-fleet/workers/archive/old-bbbb-20260101/state.json", false},
		{"projects/projects-rainier/workers/cross-cccc/state.json", true},
	}
	for _, s := range seeds {
		full := filepath.Join(fleetHome, s.path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", full, err)
		}
		if err := os.WriteFile(full, []byte("{}"), 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}

	got, err := listWorkerRecordsOnDisk()
	if err != nil {
		t.Fatalf("listWorkerRecordsOnDisk: %v", err)
	}

	// Build wantPaths for comparison.
	wantPaths := map[string]string{}
	for _, s := range seeds {
		if !s.want {
			continue
		}
		// Path is the worker DIR (parent of state.json), not the json itself.
		full := filepath.Join(fleetHome, filepath.Dir(s.path))
		// Derive project name from path: projects/<name>/workers/<slug>
		parts := strings.Split(s.path, "/")
		project := parts[1]
		wantPaths[full] = project
	}
	if len(got) != len(wantPaths) {
		t.Fatalf("listWorkerRecordsOnDisk returned %d entries, want %d. got=%+v", len(got), len(wantPaths), got)
	}
	for _, info := range got {
		// Trim trailing separator for comparison since WorkerDir() returns
		// trailing separator and our seed paths don't.
		key := strings.TrimSuffix(info.Path, string(filepath.Separator))
		wantProject, ok := wantPaths[key]
		if !ok {
			t.Errorf("unexpected entry path=%s slug=%s project=%s", info.Path, info.Slug, info.Project)
			continue
		}
		if info.Project != wantProject {
			t.Errorf("entry path=%s project=%q, want %q", info.Path, info.Project, wantProject)
		}
		if info.Slug == "" {
			t.Errorf("entry path=%s has empty slug", info.Path)
		}
	}
}

// TestLoadWorkerStateOnDisk pins the production state.json parser:
// must round-trip the four fields we care about (slug, project, pid,
// updated_at) plus tolerate the extra phase/heartbeat fields the
// coord skill writes.
func TestLoadWorkerStateOnDisk(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	body := `{
		"slug": "round-trip-llll",
		"project": "projects-fleet",
		"phase": "review-pending",
		"phases_completed": ["starting", "branch"],
		"pid": 4242,
		"updated_at": "2026-05-27T00:00:00Z"
	}`
	if err := os.WriteFile(statePath, []byte(body), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, err := loadWorkerStateOnDisk(statePath)
	if err != nil {
		t.Fatalf("loadWorkerStateOnDisk: %v", err)
	}
	if got.Slug != "round-trip-llll" {
		t.Errorf("Slug=%q, want round-trip-llll", got.Slug)
	}
	if got.Project != "projects-fleet" {
		t.Errorf("Project=%q, want projects-fleet", got.Project)
	}
	if got.Pid != 4242 {
		t.Errorf("Pid=%d, want 4242", got.Pid)
	}
	wantTime := time.Date(2026, 5, 27, 0, 0, 0, 0, time.UTC)
	if !got.UpdatedAt.Equal(wantTime) {
		t.Errorf("UpdatedAt=%v, want %v", got.UpdatedAt, wantTime)
	}
}

// TestPidAliveOnDisk_TreatsEPERMAsAlive pins the conservative-alive
// behavior for permission-denied probe results (codex iter-4 [P2]).
// In shared / sudo-crossed environments, kill(pid, 0) can return EPERM
// for a process that DOES exist but belongs to a different uid.
// Returning false would let `fleet gc --apply --kinds=worker-records`
// remove a live worker directory. Mirrors internal/workers.IsAlive +
// dispatch recovery's liveness contract.
func TestPidAliveOnDisk_TreatsEPERMAsAlive(t *testing.T) {
	// pid 1 (launchd / init) exists on every macOS+Linux system but
	// cannot be signaled by a non-root caller → kill(1, 0) returns
	// EPERM under the test process's normal uid. (When the test
	// happens to run as root, kill(1, 0) returns nil — also alive.
	// Either way the assertion holds.)
	if !pidAliveOnDisk(1) {
		t.Errorf("pidAliveOnDisk(1) reported dead — EPERM must collapse to alive (would let --apply remove a live worker dir in sudo-crossed environments)")
	}
}

// TestLoadTaskStatusOnDisk_ArchivePresenceIsTerminal pins the
// archive-is-dispositive invariant (codex iter-2 [P2] follow-up). The
// `fleet tasks archive` flow (internal/tasks.go Archive) moves task
// rows verbatim — it does NOT force status=done. An operator-forced
// archive of an in-progress task leaves the row in tasks-archive.md
// with status=in-progress. Returning that raw status from
// loadTaskStatusOnDisk would trip the task-in-progress guard in
// appendWorkerRecordWithTaskGuard and leave the orphan worker dir
// unreapable forever. The contract: presence in tasks-archive.md is
// dispositive — return "done" regardless of recorded status (mirrors
// KindWorktrees / isTaskTerminalOnDisk).
func TestLoadTaskStatusOnDisk_ArchivePresenceIsTerminal(t *testing.T) {
	fleetHome := t.TempDir()
	t.Setenv("FLEET_HOME", fleetHome)
	// state.ProjectDir resolves to <CWD>/.fleet/projects/<name>/ when
	// FLEET_HOME points elsewhere on some platforms; use the standard
	// projects path under FLEET_HOME (mirrors listWorkerRecordsOnDisk).
	projectsDir := filepath.Join(fleetHome, "projects", "projects-fleet")
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Archive a row with the WORST-CASE recorded status — in-progress
	// (the exact case that previously trapped the orphan).
	archiveBody := `## task: archived-inprog-slug
- status: in-progress
- priority: P1
- worker_pid: 0

Spec: archived but never marked done.
`
	archivePath := filepath.Join(projectsDir, "tasks-archive.md")
	if err := os.WriteFile(archivePath, []byte(archiveBody), 0o644); err != nil {
		t.Fatalf("write archive: %v", err)
	}
	// Sibling tasks.md exists but does NOT contain the slug — forces
	// the lookup to fall through to the archive path.
	if err := os.WriteFile(filepath.Join(projectsDir, "tasks.md"), []byte("# tasks\n"), 0o644); err != nil {
		t.Fatalf("write tasks: %v", err)
	}
	got, err := loadTaskStatusOnDisk("projects-fleet", "archived-inprog-slug")
	if err != nil {
		t.Fatalf("loadTaskStatusOnDisk: %v", err)
	}
	if got != "done" {
		t.Fatalf("archived in-progress row: status=%q, want %q (archive presence MUST be terminal — codex iter-2 P2)", got, "done")
	}
}

// ----- invalid-project-dir-guar-d636 invalid-projects classifier tests -----

// TestReconcile_InvalidProjects_DryRunSurfaces: a malformed dir name
// ("--project") with NO tasks.md is reported with would-remove in
// dry-run and the remover is never called.
func TestReconcile_InvalidProjects_DryRunSurfaces(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	deps := stubDeps(now)
	bogus := "/fake/projects/--project"
	deps.ListProjectDirs = func() ([]ProjectDirInfo, error) {
		return []ProjectDirInfo{
			{Name: "--project", Path: bogus, HasTasks: false},
			{Name: "fleet", Path: "/fake/projects/fleet", HasTasks: true},
		}, nil
	}
	// RemoveProjectDir must NOT run in dry-run (stubDeps default errors).
	got, err := Reconcile(Options{
		Apply: false, MaxAge: 24 * time.Hour,
		Kinds: []Kind{KindInvalidProjects},
	}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	a, ok := findAction(got, KindInvalidProjects, bogus)
	if !ok {
		t.Fatalf("missing invalid-projects action for %q; got %+v", bogus, got.Actions)
	}
	if a.Verb != VerbWouldRemove {
		t.Fatalf("dry-run verb=%q, want %q", a.Verb, VerbWouldRemove)
	}
	// The valid project must NOT be flagged.
	if _, ok := findAction(got, KindInvalidProjects, "/fake/projects/fleet"); ok {
		t.Errorf("valid project 'fleet' must not be flagged")
	}
}

// TestReconcile_InvalidProjects_ApplyRemoves: --apply calls the remover
// and reports verb=removed.
func TestReconcile_InvalidProjects_ApplyRemoves(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	deps := stubDeps(now)
	bogus := "/fake/projects/--project"
	deps.ListProjectDirs = func() ([]ProjectDirInfo, error) {
		return []ProjectDirInfo{{Name: "--project", Path: bogus, HasTasks: false}}, nil
	}
	var removed []string
	deps.RemoveProjectDir = func(p string) error { removed = append(removed, p); return nil }
	got, err := Reconcile(Options{
		Apply: true, MaxAge: 24 * time.Hour,
		Kinds: []Kind{KindInvalidProjects},
	}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	a, ok := findAction(got, KindInvalidProjects, bogus)
	if !ok {
		t.Fatalf("missing invalid-projects action; got %+v", got.Actions)
	}
	if a.Verb != VerbRemoved {
		t.Fatalf("apply verb=%q, want %q", a.Verb, VerbRemoved)
	}
	if len(removed) != 1 || removed[0] != bogus {
		t.Fatalf("RemoveProjectDir called with %v, want [%q]", removed, bogus)
	}
}

// TestReconcile_InvalidProjects_WithTasksSurfacedNotRemoved: a malformed
// name that DOES hold a tasks.md is surfaced (Verb=surface) but never
// auto-removed — surface-don't-silo (the operator may have state).
func TestReconcile_InvalidProjects_WithTasksSurfacedNotRemoved(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	deps := stubDeps(now)
	bogus := "/fake/projects/-weird"
	deps.ListProjectDirs = func() ([]ProjectDirInfo, error) {
		return []ProjectDirInfo{{Name: "-weird", Path: bogus, HasTasks: true}}, nil
	}
	// RemoveProjectDir must NOT run even under --apply (stub errors).
	got, err := Reconcile(Options{
		Apply: true, MaxAge: 24 * time.Hour,
		Kinds: []Kind{KindInvalidProjects},
	}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	a, ok := findAction(got, KindInvalidProjects, bogus)
	if !ok {
		t.Fatalf("missing invalid-projects action; got %+v", got.Actions)
	}
	if a.Verb != VerbSurface {
		t.Fatalf("with-tasks verb=%q, want %q (surface-only)", a.Verb, VerbSurface)
	}
}

// TestReconcile_InvalidProjects_EndToEnd is the architect-level
// integration test (feedback_e2e_tests_for_all_cases): it builds a real
// FLEET_HOME with a "--project" dir (mirroring the operator's screenshot:
// .locks + coord-state.json, NO tasks.md) plus a real "fleet" project,
// wires DefaultDeps (the production on-disk hooks), and asserts dry-run
// surfaces the bogus dir while --apply removes it from disk and leaves
// the real project untouched. Catches a future regression where the
// on-disk lister or remover diverges from the classifier contract.
func TestReconcile_InvalidProjects_EndToEnd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FLEET_HOME", home)
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	pdir := filepath.Join(home, "projects")
	// Bogus "--project" dir: .locks + coord-state.json, NO tasks.md.
	bogus := filepath.Join(pdir, "--project")
	if err := os.MkdirAll(filepath.Join(bogus, ".locks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bogus, "coord-state.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Real project with a tasks.md.
	real := filepath.Join(pdir, "fleet")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "tasks.md"), []byte("# tasks\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	deps := DefaultDeps()
	deps.Now = func() time.Time { return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC) }
	kinds := []Kind{KindInvalidProjects}

	// Dry-run: surfaces would-remove, dir still on disk.
	dry, err := Reconcile(Options{Apply: false, MaxAge: 24 * time.Hour, Kinds: kinds}, deps)
	if err != nil {
		t.Fatalf("dry Reconcile: %v", err)
	}
	a, ok := findAction(dry, KindInvalidProjects, bogus)
	if !ok {
		t.Fatalf("dry-run missing action for %q; got %+v", bogus, dry.Actions)
	}
	if a.Verb != VerbWouldRemove {
		t.Fatalf("dry verb=%q want %q", a.Verb, VerbWouldRemove)
	}
	if _, serr := os.Stat(bogus); serr != nil {
		t.Fatalf("dry-run must NOT remove the dir; stat err=%v", serr)
	}
	// The real project must NOT be flagged.
	if _, ok := findAction(dry, KindInvalidProjects, real); ok {
		t.Errorf("real 'fleet' project must not be flagged")
	}

	// Apply: removes the bogus dir, real project survives.
	app, err := Reconcile(Options{Apply: true, MaxAge: 24 * time.Hour, Kinds: kinds}, deps)
	if err != nil {
		t.Fatalf("apply Reconcile: %v", err)
	}
	a2, ok := findAction(app, KindInvalidProjects, bogus)
	if !ok || a2.Verb != VerbRemoved {
		t.Fatalf("apply action for %q = %+v (ok=%v); want verb=%q", bogus, a2, ok, VerbRemoved)
	}
	if _, serr := os.Stat(bogus); !os.IsNotExist(serr) {
		t.Fatalf("apply must remove the bogus dir; stat err=%v", serr)
	}
	if _, serr := os.Stat(real); serr != nil {
		t.Fatalf("apply must NOT touch the real project; stat err=%v", serr)
	}
}

// TestReconcile_InvalidProjects_TOCTOU_TasksAppearedNotRemoved regresses
// codex iter-1 [P1]: HasTasks is sampled during ListProjectDirs, but a
// concurrent coord/migration can write tasks.md before the rm -rf. The
// classifier must re-stat right before deletion and FAIL CLOSED — surface,
// never remove — when tasks.md raced in. Here HasTasks=false (scan-time)
// but ProjectHasTasks=true (recheck-time) simulates the race.
func TestReconcile_InvalidProjects_TOCTOU_TasksAppearedNotRemoved(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	deps := stubDeps(now)
	bogus := "/fake/projects/--project"
	deps.ListProjectDirs = func() ([]ProjectDirInfo, error) {
		return []ProjectDirInfo{{Name: "--project", Path: bogus, HasTasks: false}}, nil
	}
	// tasks.md raced in between scan and delete.
	deps.ProjectHasTasks = func(string) (bool, error) { return true, nil }
	// RemoveProjectDir must NOT run (stubDeps default errors).
	got, err := Reconcile(Options{
		Apply: true, MaxAge: 24 * time.Hour,
		Kinds: []Kind{KindInvalidProjects},
	}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	a, ok := findAction(got, KindInvalidProjects, bogus)
	if !ok {
		t.Fatalf("missing invalid-projects action; got %+v", got.Actions)
	}
	if a.Verb != VerbSurface {
		t.Fatalf("TOCTOU verb=%q, want %q (must fail closed, not remove)", a.Verb, VerbSurface)
	}
}

// TestReconcile_InvalidProjects_TOCTOU_PostQuarantineRace regresses codex
// iter-2 [P1]: the residual window where tasks.md appears AFTER the first
// recheck but BEFORE rm -rf. The fix quarantines (renames) first, then
// re-stats the quarantined tree. Here the pre-quarantine stat sees no
// tasks.md (path=d.Path → false) but the post-quarantine stat does
// (path=quarantined → true), so the classifier must un-quarantine
// (restore called) and surface — never remove.
func TestReconcile_InvalidProjects_TOCTOU_PostQuarantineRace(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	deps := stubDeps(now)
	bogus := "/fake/projects/--project"
	qpath := "/fake/projects/.--project.gc-quarantine"
	deps.ListProjectDirs = func() ([]ProjectDirInfo, error) {
		return []ProjectDirInfo{{Name: "--project", Path: bogus, HasTasks: false}}, nil
	}
	deps.QuarantineProjectDir = func(p string) (string, error) {
		if p != bogus {
			t.Errorf("quarantine called with %q, want %q", p, bogus)
		}
		return qpath, nil
	}
	// tasks.md is absent on the live path but present once quarantined
	// (the race: a writer landed tasks.md right after the first stat).
	deps.ProjectHasTasks = func(p string) (bool, error) {
		return p == qpath, nil
	}
	var restored [][2]string
	deps.RestoreProjectDir = func(q, o string) error {
		restored = append(restored, [2]string{q, o})
		return nil
	}
	// RemoveProjectDir must NOT run (stubDeps default errors).
	got, err := Reconcile(Options{
		Apply: true, MaxAge: 24 * time.Hour,
		Kinds: []Kind{KindInvalidProjects},
	}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	a, ok := findAction(got, KindInvalidProjects, bogus)
	if !ok {
		t.Fatalf("missing invalid-projects action; got %+v", got.Actions)
	}
	if a.Verb != VerbSurface {
		t.Fatalf("post-quarantine race verb=%q, want %q (must restore + surface)", a.Verb, VerbSurface)
	}
	if len(restored) != 1 || restored[0] != [2]string{qpath, bogus} {
		t.Fatalf("RestoreProjectDir calls=%v, want one [%q %q]", restored, qpath, bogus)
	}
}

// TestReconcile_InvalidProjects_TOCTOU_StatErrorFailsClosed: a non-ENOENT
// stat error during the pre-delete recheck must also fail closed (surface,
// not remove) — codex iter-1 [P1]. We never rm -rf on an ambiguous stat.
func TestReconcile_InvalidProjects_TOCTOU_StatErrorFailsClosed(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	deps := stubDeps(now)
	bogus := "/fake/projects/--project"
	deps.ListProjectDirs = func() ([]ProjectDirInfo, error) {
		return []ProjectDirInfo{{Name: "--project", Path: bogus, HasTasks: false}}, nil
	}
	deps.ProjectHasTasks = func(string) (bool, error) {
		return false, errors.New("permission denied")
	}
	got, err := Reconcile(Options{
		Apply: true, MaxAge: 24 * time.Hour,
		Kinds: []Kind{KindInvalidProjects},
	}, deps)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	a, ok := findAction(got, KindInvalidProjects, bogus)
	if !ok {
		t.Fatalf("missing invalid-projects action; got %+v", got.Actions)
	}
	if a.Verb != VerbSurface {
		t.Fatalf("stat-error verb=%q, want %q (must fail closed)", a.Verb, VerbSurface)
	}
}
