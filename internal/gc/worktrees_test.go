package gc

// T7 — `fleet gc --kinds=worktrees` SAFE orphan-dir resolver
// (DESIGN-coord-worktree-lifecycle §4.4). Exercises the exact/prefix
// matrix, the generation-aware belt, live-over-archive precedence,
// repo-registration identity, the dirty-guard, and the report verbs.
//
// The resolver runs whenever ListTaskCandidates is wired, so these tests
// wire the full §4.4 dep set with in-memory fakes (no git on PATH).

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// resolverDeps builds a Deps with the §4.4 hardened-resolver hooks wired
// to in-memory fakes. branches/dirty/registered are keyed by worktree
// path; candidates are returned for project "p".
type fakeWT struct {
	candidates  []TaskCandidate
	entries     []WorktreeEntry
	repo        string
	branchByPth map[string]string
	dirtyByPth  map[string]bool
	regByPth    map[string]bool
	removed     []string
	removeErr   map[string]error
}

func (f *fakeWT) deps() Deps {
	d := stubDeps(time.Now())
	d.ListProjects = func() ([]string, error) { return []string{"p"}, nil }
	d.ListWorktrees = func(string) ([]WorktreeEntry, error) { return f.entries, nil }
	d.ListTaskCandidates = func(string) ([]TaskCandidate, error) { return f.candidates, nil }
	d.ProjectRepo = func(string) (string, error) {
		if f.repo == "" {
			return "/repo", nil
		}
		return f.repo, nil
	}
	d.WorktreeBranch = func(p string) (string, error) {
		if b, ok := f.branchByPth[p]; ok {
			return b, nil
		}
		return "", nil
	}
	d.WorktreeDirty = func(p string) (bool, error) { return f.dirtyByPth[p], nil }
	d.WorktreeRegistered = func(repo, p string) (bool, error) {
		// Default: registered, unless explicitly set false.
		if v, ok := f.regByPth[p]; ok {
			return v, nil
		}
		return true, nil
	}
	d.RemoveWorktreeNoForce = func(repo, p string) error {
		if f.removeErr != nil {
			if e := f.removeErr[p]; e != nil {
				return e
			}
		}
		f.removed = append(f.removed, p)
		return nil
	}
	return d
}

func runResolver(t *testing.T, f *fakeWT, apply bool) Report {
	t.Helper()
	r, err := Reconcile(Options{Apply: apply, Kinds: []Kind{KindWorktrees}, Project: "p"}, f.deps())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return r
}

func wtEntry(slug string) WorktreeEntry {
	return WorktreeEntry{Project: "p", Slug: slug, Path: "/wt/" + slug}
}

// ---------- exact-slug + branch identity ----------

func TestResolver_ExactTerminalCleanBranchCorroborated_Reaped(t *testing.T) {
	f := &fakeWT{
		entries:    []WorktreeEntry{wtEntry("feat-aaaa")},
		candidates: []TaskCandidate{{Slug: "feat-aaaa", Status: "done", Branch: "worker/feat-aaaa"}},
		branchByPth: map[string]string{
			"/wt/feat-aaaa": "worker/feat-aaaa",
		},
	}
	r := runResolver(t, f, true)
	a, ok := findAction(r, KindWorktrees, "/wt/feat-aaaa")
	if !ok || a.Verb != VerbRemoved {
		t.Fatalf("want removed; got %+v (ok=%v)", a, ok)
	}
	if len(f.removed) != 1 {
		t.Fatalf("expected 1 removal; got %v", f.removed)
	}
}

func TestResolver_WrongBranchCleanTree_Skipped(t *testing.T) {
	f := &fakeWT{
		entries:     []WorktreeEntry{wtEntry("feat-bbbb")},
		candidates:  []TaskCandidate{{Slug: "feat-bbbb", Status: "done", Branch: "worker/feat-bbbb"}},
		branchByPth: map[string]string{"/wt/feat-bbbb": "main"}, // WRONG branch
	}
	r := runResolver(t, f, true)
	a, ok := findAction(r, KindWorktrees, "/wt/feat-bbbb")
	if !ok || a.Verb != VerbSurface {
		t.Fatalf("want surface (skip); got %+v", a)
	}
	if len(f.removed) != 0 {
		t.Fatalf("wrong-branch clean tree must NOT be removed; got %v", f.removed)
	}
}

func TestResolver_BranchEmpty_UsesDeterministicWorkerSlug(t *testing.T) {
	f := &fakeWT{
		entries:     []WorktreeEntry{wtEntry("feat-cccc")},
		candidates:  []TaskCandidate{{Slug: "feat-cccc", Status: "abandoned", Branch: ""}},
		branchByPth: map[string]string{"/wt/feat-cccc": "worker/feat-cccc"},
	}
	r := runResolver(t, f, true)
	if a, ok := findAction(r, KindWorktrees, "/wt/feat-cccc"); !ok || a.Verb != VerbRemoved {
		t.Fatalf("want removed via deterministic worker/<slug>; got %+v", a)
	}
}

// ---------- unique-prefix ----------

func TestResolver_UniquePrefixBranchCorroborated_Reaped(t *testing.T) {
	// dir "feat" is a unique prefix of "feat-longslug-aaaa"; branch must
	// corroborate the longer slug's worker branch.
	f := &fakeWT{
		entries:     []WorktreeEntry{wtEntry("feat")},
		candidates:  []TaskCandidate{{Slug: "feat-longslug-aaaa", Status: "done", Branch: "worker/feat-longslug-aaaa"}},
		branchByPth: map[string]string{"/wt/feat": "worker/feat-longslug-aaaa"},
	}
	r := runResolver(t, f, true)
	if a, ok := findAction(r, KindWorktrees, "/wt/feat"); !ok || a.Verb != VerbRemoved {
		t.Fatalf("want removed via unique-prefix; got %+v", a)
	}
}

func TestResolver_UniquePrefixWithoutBranchIdentity_Skipped(t *testing.T) {
	f := &fakeWT{
		entries:     []WorktreeEntry{wtEntry("feat")},
		candidates:  []TaskCandidate{{Slug: "feat-longslug-bbbb", Status: "done", Branch: "worker/feat-longslug-bbbb"}},
		branchByPth: map[string]string{"/wt/feat": "main"}, // branch doesn't corroborate
	}
	r := runResolver(t, f, true)
	if a, ok := findAction(r, KindWorktrees, "/wt/feat"); !ok || a.Verb != VerbSurface {
		t.Fatalf("want surface (skip); got %+v", a)
	}
	if len(f.removed) != 0 {
		t.Fatalf("must not remove; got %v", f.removed)
	}
}

func TestResolver_AmbiguousPrefix_Skipped(t *testing.T) {
	f := &fakeWT{
		entries: []WorktreeEntry{wtEntry("feat")},
		candidates: []TaskCandidate{
			{Slug: "feat-one-aaaa", Status: "done", Branch: "worker/feat-one-aaaa"},
			{Slug: "feat-two-bbbb", Status: "done", Branch: "worker/feat-two-bbbb"},
		},
	}
	r := runResolver(t, f, true)
	a, _ := findAction(r, KindWorktrees, "/wt/feat")
	if a.Verb != VerbSurface {
		t.Fatalf("ambiguous prefix must surface (skip); got %+v", a)
	}
}

func TestResolver_NoMatchingTask_Skipped(t *testing.T) {
	f := &fakeWT{
		entries:    []WorktreeEntry{wtEntry("orphan-zzzz")},
		candidates: []TaskCandidate{{Slug: "unrelated-aaaa", Status: "done"}},
	}
	r := runResolver(t, f, true)
	a, _ := findAction(r, KindWorktrees, "/wt/orphan-zzzz")
	if a.Verb != VerbSurface {
		t.Fatalf("no-matching-task must surface (skip); got %+v", a)
	}
}

// ---------- non-terminal task ----------

func TestResolver_LiveInProgressPrefix_Skipped(t *testing.T) {
	f := &fakeWT{
		entries:     []WorktreeEntry{wtEntry("feat-dddd")},
		candidates:  []TaskCandidate{{Slug: "feat-dddd", Status: "in-progress", Branch: "worker/feat-dddd"}},
		branchByPth: map[string]string{"/wt/feat-dddd": "worker/feat-dddd"},
	}
	r := runResolver(t, f, true)
	a, _ := findAction(r, KindWorktrees, "/wt/feat-dddd")
	if a.Verb != VerbSurface {
		t.Fatalf("non-terminal task must surface (skip); got %+v", a)
	}
	if len(f.removed) != 0 {
		t.Fatalf("must not remove a live task's tree; got %v", f.removed)
	}
}

// ---------- generation-aware belt (R7) ----------

func TestResolver_CurrentGenNonTerminalState_Vetoes(t *testing.T) {
	// Task is terminal (done) but a CURRENT-gen worker state is
	// non-terminal (a live worker may still be editing) → veto.
	f := &fakeWT{
		entries: []WorktreeEntry{wtEntry("feat-eeee")},
		candidates: []TaskCandidate{{
			Slug: "feat-eeee", Status: "done", Branch: "worker/feat-eeee",
			HasWorkerState: true, WorkerPhase: "tdd-green", TaskGen: 3, WorkerStateGen: 3,
		}},
		branchByPth: map[string]string{"/wt/feat-eeee": "worker/feat-eeee"},
	}
	r := runResolver(t, f, true)
	a, _ := findAction(r, KindWorktrees, "/wt/feat-eeee")
	if a.Verb != VerbSurface {
		t.Fatalf("current-gen non-terminal state must veto (surface); got %+v", a)
	}
	if len(f.removed) != 0 {
		t.Fatalf("must not remove; got %v", f.removed)
	}
}

func TestResolver_StaleGenNonTerminalState_DoesNotVeto(t *testing.T) {
	// Task terminal; a STALE-gen non-terminal state (a prior attempt's
	// leftover) must NOT veto — else the leak never clears.
	f := &fakeWT{
		entries: []WorktreeEntry{wtEntry("feat-ffff")},
		candidates: []TaskCandidate{{
			Slug: "feat-ffff", Status: "done", Branch: "worker/feat-ffff",
			HasWorkerState: true, WorkerPhase: "tdd-green", TaskGen: 5, WorkerStateGen: 4,
		}},
		branchByPth: map[string]string{"/wt/feat-ffff": "worker/feat-ffff"},
	}
	r := runResolver(t, f, true)
	a, ok := findAction(r, KindWorktrees, "/wt/feat-ffff")
	if !ok || a.Verb != VerbRemoved {
		t.Fatalf("stale-gen non-terminal state must NOT veto (reap); got %+v", a)
	}
}

// ---------- live-over-archive precedence ----------

func TestResolver_LiveOverArchive_PrefersLiveNonTerminal(t *testing.T) {
	// Same slug in live (in-progress) + archive (terminal). Live wins →
	// non-terminal → skip (must not reap the live task's tree).
	f := &fakeWT{
		entries: []WorktreeEntry{wtEntry("dup-aaaa")},
		candidates: []TaskCandidate{
			{Slug: "dup-aaaa", Status: "in-progress", Branch: "worker/dup-aaaa", FromArchive: false},
			{Slug: "dup-aaaa", Status: "done", Branch: "worker/dup-aaaa", FromArchive: true},
		},
		branchByPth: map[string]string{"/wt/dup-aaaa": "worker/dup-aaaa"},
	}
	r := runResolver(t, f, true)
	a, _ := findAction(r, KindWorktrees, "/wt/dup-aaaa")
	if a.Verb != VerbSurface {
		t.Fatalf("live-over-archive must prefer LIVE (skip non-terminal); got %+v", a)
	}
	if len(f.removed) != 0 {
		t.Fatalf("must not remove the live task's tree; got %v", f.removed)
	}
}

func TestResolver_ArchiveOnlyExact_Reaped(t *testing.T) {
	// Archive-only exact (no live row) → terminal by definition → reap.
	f := &fakeWT{
		entries: []WorktreeEntry{wtEntry("arch-bbbb")},
		candidates: []TaskCandidate{
			{Slug: "arch-bbbb", Status: "in-progress", Branch: "worker/arch-bbbb", FromArchive: true},
		},
		branchByPth: map[string]string{"/wt/arch-bbbb": "worker/arch-bbbb"},
	}
	r := runResolver(t, f, true)
	if a, ok := findAction(r, KindWorktrees, "/wt/arch-bbbb"); !ok || a.Verb != VerbRemoved {
		t.Fatalf("archive-only exact is terminal → reap; got %+v", a)
	}
}

func TestResolver_ConflictingLiveExacts_Skipped(t *testing.T) {
	f := &fakeWT{
		entries: []WorktreeEntry{wtEntry("conf-cccc")},
		candidates: []TaskCandidate{
			{Slug: "conf-cccc", Status: "done", Branch: "worker/conf-cccc"},
			{Slug: "conf-cccc", Status: "done", Branch: "worker/conf-cccc"},
		},
	}
	r := runResolver(t, f, true)
	a, _ := findAction(r, KindWorktrees, "/wt/conf-cccc")
	if a.Verb != VerbSurface {
		t.Fatalf("conflicting live exacts must surface (skip); got %+v", a)
	}
}

// ---------- repo-registration identity ----------

func TestResolver_NotRegisteredInBaseRepo_Skipped(t *testing.T) {
	f := &fakeWT{
		entries:     []WorktreeEntry{wtEntry("reg-dddd")},
		candidates:  []TaskCandidate{{Slug: "reg-dddd", Status: "done", Branch: "worker/reg-dddd"}},
		branchByPth: map[string]string{"/wt/reg-dddd": "worker/reg-dddd"},
		regByPth:    map[string]bool{"/wt/reg-dddd": false}, // NOT registered
	}
	r := runResolver(t, f, true)
	a, _ := findAction(r, KindWorktrees, "/wt/reg-dddd")
	if a.Verb != VerbSurface {
		t.Fatalf("unregistered tree must surface (skip); got %+v", a)
	}
	if len(f.removed) != 0 {
		t.Fatalf("must not remove an unregistered tree; got %v", f.removed)
	}
}

func TestResolver_UnresolvedBaseRepo_SurfacesAll(t *testing.T) {
	f := &fakeWT{
		entries:     []WorktreeEntry{wtEntry("repo-eeee")},
		candidates:  []TaskCandidate{{Slug: "repo-eeee", Status: "done", Branch: "worker/repo-eeee"}},
		branchByPth: map[string]string{"/wt/repo-eeee": "worker/repo-eeee"},
	}
	d := f.deps()
	d.ProjectRepo = func(string) (string, error) { return "", nil } // unresolved
	r, err := Reconcile(Options{Apply: true, Kinds: []Kind{KindWorktrees}, Project: "p"}, d)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	a, ok := findAction(r, KindWorktrees, "/wt/repo-eeee")
	if !ok || a.Verb != VerbSurface {
		t.Fatalf("unresolved base repo must surface every dir; got %+v", a)
	}
}

// ---------- dirty-guard ----------

func TestResolver_DirtyTerminalTree_Parked(t *testing.T) {
	f := &fakeWT{
		entries:     []WorktreeEntry{wtEntry("dirty-ffff")},
		candidates:  []TaskCandidate{{Slug: "dirty-ffff", Status: "done", Branch: "worker/dirty-ffff"}},
		branchByPth: map[string]string{"/wt/dirty-ffff": "worker/dirty-ffff"},
		dirtyByPth:  map[string]bool{"/wt/dirty-ffff": true},
	}
	r := runResolver(t, f, true)
	a, _ := findAction(r, KindWorktrees, "/wt/dirty-ffff")
	if a.Verb != VerbSurface {
		t.Fatalf("dirty terminal tree must be parked (surface); got %+v", a)
	}
	if len(f.removed) != 0 {
		t.Fatalf("dirty tree must NOT be removed (fails-on-parent); got %v", f.removed)
	}
}

func TestResolver_TOCTOUDirtyRemoveRefusal_Parked(t *testing.T) {
	// Clean at the dirty-check, but --no-force removal refuses (turned
	// dirty in the TOCTOU window) → reclassified as parked (surface).
	f := &fakeWT{
		entries:     []WorktreeEntry{wtEntry("toctou-gggg")},
		candidates:  []TaskCandidate{{Slug: "toctou-gggg", Status: "done", Branch: "worker/toctou-gggg"}},
		branchByPth: map[string]string{"/wt/toctou-gggg": "worker/toctou-gggg"},
		removeErr: map[string]error{
			"/wt/toctou-gggg": errors.New("fatal: contains modified or untracked files, use --force to delete it"),
		},
	}
	r := runResolver(t, f, true)
	a, _ := findAction(r, KindWorktrees, "/wt/toctou-gggg")
	if a.Verb != VerbSurface {
		t.Fatalf("TOCTOU dirty removal refusal must be parked (surface); got %+v", a)
	}
}

// ---------- dry-run + report counts ----------

func TestResolver_DryRun_WouldRemove_NoMutation(t *testing.T) {
	f := &fakeWT{
		entries:     []WorktreeEntry{wtEntry("dry-hhhh")},
		candidates:  []TaskCandidate{{Slug: "dry-hhhh", Status: "done", Branch: "worker/dry-hhhh"}},
		branchByPth: map[string]string{"/wt/dry-hhhh": "worker/dry-hhhh"},
	}
	r := runResolver(t, f, false) // dry-run
	a, ok := findAction(r, KindWorktrees, "/wt/dry-hhhh")
	if !ok || a.Verb != VerbWouldRemove {
		t.Fatalf("dry-run must report would-remove; got %+v", a)
	}
	if len(f.removed) != 0 {
		t.Fatalf("dry-run must not remove; got %v", f.removed)
	}
}

func TestResolver_ReportCounts_NReapedMParkedKSkipped(t *testing.T) {
	f := &fakeWT{
		entries: []WorktreeEntry{
			wtEntry("reaped-aaaa"), wtEntry("dirty-bbbb"), wtEntry("skip-cccc"),
		},
		candidates: []TaskCandidate{
			{Slug: "reaped-aaaa", Status: "done", Branch: "worker/reaped-aaaa"},
			{Slug: "dirty-bbbb", Status: "done", Branch: "worker/dirty-bbbb"},
			{Slug: "skip-cccc", Status: "in-progress", Branch: "worker/skip-cccc"},
		},
		branchByPth: map[string]string{
			"/wt/reaped-aaaa": "worker/reaped-aaaa",
			"/wt/dirty-bbbb":  "worker/dirty-bbbb",
			"/wt/skip-cccc":   "worker/skip-cccc",
		},
		dirtyByPth: map[string]bool{"/wt/dirty-bbbb": true},
	}
	r := runResolver(t, f, true)
	var nReaped, mParked, kSkipped int
	for _, a := range r.Actions {
		if a.Kind != KindWorktrees {
			continue
		}
		switch a.Verb {
		case VerbRemoved:
			nReaped++
		case VerbSurface:
			// dirty vs skip distinguished by Reason substring
			if strings.Contains(a.Reason, "DIRTY-PARKED") {
				mParked++
			} else {
				kSkipped++
			}
		}
	}
	if nReaped != 1 || mParked != 1 || kSkipped != 1 {
		t.Fatalf("want 1 reaped / 1 dirty-parked / 1 skipped; got %d/%d/%d", nReaped, mParked, kSkipped)
	}
}

// codex [P2] regression: a dirty terminal worktree reached in a default
// `fleet gc --apply` sweep must NOT have its worker-record dir removed by
// the worker-records pass in the SAME run (the recovery context the
// dirty-park is trying to keep). The in-run dirty-park set bridges the
// two passes before `parked` is persisted to tasks.md.
func TestResolver_DirtyPark_KeepsWorkerRecordInSameRun(t *testing.T) {
	f := &fakeWT{
		entries:     []WorktreeEntry{wtEntry("recov-iiii")},
		candidates:  []TaskCandidate{{Slug: "recov-iiii", Status: "done", Branch: "worker/recov-iiii"}},
		branchByPth: map[string]string{"/wt/recov-iiii": "worker/recov-iiii"},
		dirtyByPth:  map[string]bool{"/wt/recov-iiii": true}, // dirty → parked
	}
	d := f.deps()
	old := time.Now().Add(-72 * time.Hour)
	d.ListWorkerRecords = func() ([]WorkerRecordInfo, error) {
		return []WorkerRecordInfo{{Project: "p", Slug: "recov-iiii", Path: "/workers/recov-iiii"}}, nil
	}
	d.LoadWorkerState = func(string) (WorkerState, error) {
		return WorkerState{Slug: "recov-iiii", Project: "p", Pid: 0, UpdatedAt: old}, nil
	}
	d.LoadTaskStatus = func(string, string) (string, error) { return "done", nil }
	d.LoadTaskParked = func(string, string) (string, error) { return "", nil } // not yet persisted
	d.PidAlive = func(int) bool { return false }
	removedRecords := []string{}
	d.RemoveWorkerRecord = func(p string) error { removedRecords = append(removedRecords, p); return nil }

	r, err := Reconcile(Options{
		Apply: true, Kinds: []Kind{KindWorktrees, KindWorkerRecords}, Project: "p",
	}, d)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(f.removed) != 0 {
		t.Fatalf("dirty worktree must not be removed; got %v", f.removed)
	}
	if len(removedRecords) != 0 {
		t.Fatalf("worker dir must be KEPT as recovery context; got removed=%v", removedRecords)
	}
	a, ok := findAction(r, KindWorkerRecords, "/workers/recov-iiii")
	if !ok || a.Verb != VerbSurface {
		t.Fatalf("worker-records should surface (keep) the dirty-parked slug; got %+v", a)
	}
}
