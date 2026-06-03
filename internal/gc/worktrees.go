package gc

// worktrees.go — the SAFE branch-identity-corroborated orphan-dir
// resolver for `fleet gc --kinds=worktrees`
// (DESIGN-coord-worktree-lifecycle §4.4). The legacy classifier in
// gc.go did exact-slug + task-terminal only; a CLEAN checkout of the
// WRONG branch sitting at a terminal-slug worktree path would be reaped
// (force=False does NOT protect a clean tree). This resolver only reaps
// a tree it can POSITIVELY identify as the current-terminal worktree of
// a task:
//
//	orphan dir name ──► resolve to a task candidate
//	    1. exact-slug match            (+ branch identity required)
//	    2. else unique-prefix match    (+ branch identity required)
//	    3. else ambiguous / no-match   → SKIP + SURFACE
//
// Then, before any reap, ALL of these must hold:
//
//	- branch identity:  dir's HEAD == candidate's worker/<slug> branch
//	- repo registration: dir is registered in the project base repo's
//	                     `git worktree list`, and removal runs THROUGH
//	                     that repo with --no-force
//	- generation-aware belt: a CURRENT-generation non-terminal worker
//	                     state vetoes; a STALE-generation one does not
//	                     (else the leak never clears — R7)
//	- live-over-archive: an exact slug prefers a LIVE row over an archive
//	                     row; conflicting live exacts → surface + skip
//	- dirty-guard:       `git status --porcelain` non-empty → dirty-parked
//	                     (kept, surfaced), never removed
//
// Report counts (N reaped / M dirty-parked / K skipped) are derivable
// from the per-action Verb + Reason the resolver appends, so a no-op is
// visible. Fail-soft: a per-project read error is a synthetic surface
// action, never a wedged tick.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tasks"
	"github.com/edisonshen/fleet/internal/workers"
)

// TaskCandidate is one live-or-archive task row, projected with exactly
// the fields the resolver needs (DESIGN §4.4 "full task-candidate
// index"). The resolver matches orphan dir names against the candidate
// set (exact → unique-prefix) and applies live-over-archive precedence.
type TaskCandidate struct {
	Slug        string
	Status      string // raw status string (e.g. "done", "in-progress")
	Branch      string // task.branch; "" → worker/<slug> deterministic
	Worktree    string // persisted worktree= (SUPPORTING evidence only)
	FromArchive bool   // true when sourced from tasks-archive.md

	// Worker-state projection for the generation-aware belt (R7).
	HasWorkerState bool   // a state.json exists for this slug
	WorkerPhase    string // state.json phase ("" when absent)
	TaskGen        int    // task row dispatch_generation (the authority)
	WorkerStateGen int    // state.json dispatch_generation
}

// expectedBranch returns the branch a worktree of this candidate MUST
// have checked out: the task's branch=, or the deterministic worker/<slug>
// fallback when branch= is empty.
func (c TaskCandidate) expectedBranch() string {
	if c.Branch != "" {
		return c.Branch
	}
	return "worker/" + c.Slug
}

// taskTerminal reports whether a status string is terminal (done |
// abandoned). An archive row is terminal by definition (the task left the
// live list) regardless of its recorded status — mirrors gc.go's
// isTaskTerminalOnDisk archive rule.
func (c TaskCandidate) taskTerminal() bool {
	if c.FromArchive {
		return true
	}
	return c.Status == "done" || c.Status == "abandoned"
}

// workerPhaseTerminal reports whether the worker-state phase is a
// terminal phase (push/done/blocked/failed) — i.e. NOT mid-work. A
// non-terminal phase is the signal a worker might still be editing.
func workerPhaseTerminal(p string) bool {
	switch workers.Phase(p) {
	case workers.PhasePush, workers.PhaseDone, workers.PhaseBlocked, workers.PhaseFailed:
		return true
	}
	return false
}

// reconcileWorktreesResolver is the §4.4 hardened path. It scans each
// project's worktree dirs, resolves each to a task candidate, and reaps
// only positively-identified current-terminal trees.
func reconcileWorktreesResolver(r *Report, opts Options, deps Deps, dirtyParked map[string]bool) error {
	var projects []string
	if opts.Project != "" {
		projects = []string{opts.Project}
	} else {
		ps, err := deps.ListProjects()
		if err != nil {
			return err
		}
		projects = ps
	}
	for _, p := range projects {
		entries, err := deps.ListWorktrees(p)
		if err != nil {
			r.Actions = append(r.Actions, Action{
				Kind: KindWorktrees, Target: p, Verb: VerbSurface,
				Reason: fmt.Sprintf("list worktrees failed: %v", err),
			})
			continue
		}
		if len(entries) == 0 {
			continue
		}
		candidates, cerr := deps.ListTaskCandidates(p)
		if cerr != nil {
			r.Actions = append(r.Actions, Action{
				Kind: KindWorktrees, Target: p, Verb: VerbSurface,
				Reason: fmt.Sprintf("list task candidates failed: %v", cerr),
			})
			continue
		}
		repo, rerr := deps.ProjectRepo(p)
		if rerr != nil || repo == "" {
			// Without the base repo we cannot corroborate registration or
			// run the through-repo removal — surface every dir, reap none.
			for _, e := range entries {
				r.Actions = append(r.Actions, Action{
					Kind: KindWorktrees, Target: e.Path, Verb: VerbSurface,
					Reason: fmt.Sprintf("base repo for project %q unresolved (%v); cannot corroborate registration — skipping", p, rerr),
				})
			}
			continue
		}
		for _, e := range entries {
			act, parkedSlug := resolveOneWorktree(opts, deps, e, candidates, repo)
			r.Actions = append(r.Actions, act)
			// Record dirty-parked slugs (under --apply) so the worker-
			// records pass in the same Reconcile run keeps their recovery
			// dir even before `parked` is persisted to tasks.md.
			if dirtyParked != nil && parkedSlug != "" && opts.Apply {
				dirtyParked[p+"/"+parkedSlug] = true
			}
		}
	}
	return nil
}

// resolveOneWorktree resolves a single worktree dir to its planned/applied
// Action. Pure decision logic (no I/O beyond the injected deps) so it is
// table-test-friendly. The second return value is the resolved task slug
// when the outcome is DIRTY-PARKED (a tree left on disk that the worker-
// records pass must not strip the recovery dir for); "" otherwise.
func resolveOneWorktree(opts Options, deps Deps, e WorktreeEntry, candidates []TaskCandidate, repo string) (Action, string) {
	skip := func(reason string) Action {
		return Action{Kind: KindWorktrees, Target: e.Path, Verb: VerbSurface, Reason: reason}
	}

	cand, why := matchCandidate(e.Slug, candidates)
	if why != "" {
		return skip(why), ""
	}

	// Generation-aware belt (R7): a CURRENT-generation non-terminal worker
	// state vetoes the reap (a live worker may still be editing). A
	// STALE-generation non-terminal state is surfaced but does NOT veto
	// (else a prior attempt's leftover state pins the leak forever). The
	// belt runs against the SAME candidate we resolved (its worker-state
	// projection).
	if cand.HasWorkerState && !workerPhaseTerminal(cand.WorkerPhase) {
		if cand.WorkerStateGen == cand.TaskGen {
			return skip(fmt.Sprintf(
				"worktree %s: current-gen worker state phase=%q (gen=%d) is non-terminal — a live worker may be editing; skipping",
				e.Path, cand.WorkerPhase, cand.WorkerStateGen)), ""
		}
		// Stale-gen non-terminal: surface, do NOT veto — fall through to
		// the terminal-task gate below (the leak must be reapable).
	}

	// Task-terminal gate: only reap a worktree whose task is terminal.
	if !cand.taskTerminal() {
		return skip(fmt.Sprintf(
			"worktree %s → task %s/%s is non-terminal (status=%q); skipping",
			e.Path, e.Project, cand.Slug, cand.Status)), ""
	}

	// Branch identity: the dir must have the candidate's expected branch
	// checked out. A clean WRONG-branch checkout is NOT this task's tree.
	want := cand.expectedBranch()
	got, berr := deps.WorktreeBranch(e.Path)
	if berr != nil {
		return skip(fmt.Sprintf(
			"worktree %s: branch probe failed (%v); skipping", e.Path, berr)), ""
	}
	if got != want {
		return skip(fmt.Sprintf(
			"worktree %s: HEAD branch %q != expected %q for task %s — not this task's tree; skipping",
			e.Path, got, want, cand.Slug)), ""
	}

	// Repo-registration identity: the dir must be registered in the base
	// repo's worktree list. A clean wrong checkout that merely SITS at the
	// deterministic path (never `git worktree add`-ed) would pass the
	// branch gate; this closes it.
	reg, regErr := deps.WorktreeRegistered(repo, e.Path)
	if regErr != nil {
		return skip(fmt.Sprintf(
			"worktree %s: registration probe in repo %s failed (%v); skipping",
			e.Path, repo, regErr)), ""
	}
	if !reg {
		return skip(fmt.Sprintf(
			"worktree %s: not registered in base repo %s — refusing to reap; skipping",
			e.Path, repo)), ""
	}

	// Dirty-guard: an uncommitted tree is parked, never removed. An
	// undeterminable status is treated as dirty (fail-safe).
	dirty, derr := deps.WorktreeDirty(e.Path)
	if derr != nil {
		// Cleanliness undeterminable → parked (kept) AND recorded as a
		// dirty-park slug so the worker-records pass keeps the recovery dir.
		return Action{
			Kind: KindWorktrees, Target: e.Path, Verb: VerbSurface,
			Reason: fmt.Sprintf("worktree %s: cleanliness undeterminable (%v) — parked (kept); resolve manually", e.Path, derr),
		}, cand.Slug
	}
	if dirty {
		return Action{
			Kind: KindWorktrees, Target: e.Path, Verb: VerbSurface,
			Reason: fmt.Sprintf("worktree %s: uncommitted changes — DIRTY-PARKED (kept); commit/discard then re-run gc", e.Path),
		}, cand.Slug
	}

	// Clean + identified + terminal → reap (through the base repo,
	// --no-force, which itself refuses a tree that turned dirty in the
	// TOCTOU window).
	act := Action{
		Kind: KindWorktrees, Target: e.Path, Verb: VerbWouldRemove,
		Reason: fmt.Sprintf("task %s/%s terminal + clean + branch %q corroborated; registered in %s",
			e.Project, cand.Slug, want, repo),
	}
	parkedSlug := ""
	if opts.Apply {
		if rerr := deps.RemoveWorktreeNoForce(repo, e.Path); rerr != nil {
			// A --no-force removal that fails on a dirty tree is the TOCTOU
			// belt: classify as dirty-parked (surface), not removed — and
			// record the slug so the worker dir survives the same sweep.
			if isDirtyRemoveRefusal(rerr.Error()) {
				act.Verb = VerbSurface
				act.Reason = fmt.Sprintf("worktree %s: turned dirty before removal (TOCTOU) — DIRTY-PARKED (kept): %v", e.Path, rerr)
				parkedSlug = cand.Slug
			} else {
				act.Reason = fmt.Sprintf("remove failed: %v", rerr)
			}
		} else {
			act.Verb = VerbRemoved
		}
	}
	return act, parkedSlug
}

// matchCandidate resolves an orphan dir slug to a single TaskCandidate.
// Returns (cand, "") on a clean resolution, or (zero, reason) when the
// dir must be SKIPPED (ambiguous, no match, or conflicting exacts).
//
// Resolution order (DESIGN §4.4):
//  1. exact-slug match, with live-over-archive precedence (a LIVE row is
//     preferred over an archive row; an archive candidate is eligible
//     only when no live row exists; conflicting LIVE exacts → skip).
//  2. else unique-prefix match (the dir name is a prefix of exactly one
//     candidate slug — heterogeneous leaked dir names: -rand-stripped,
//     mid-word-truncated). Ambiguous (>1) → skip. Branch identity is
//     enforced by the caller for BOTH exact and prefix matches.
func matchCandidate(dirSlug string, candidates []TaskCandidate) (TaskCandidate, string) {
	var liveExact, archiveExact []TaskCandidate
	for _, c := range candidates {
		if c.Slug == dirSlug {
			if c.FromArchive {
				archiveExact = append(archiveExact, c)
			} else {
				liveExact = append(liveExact, c)
			}
		}
	}
	// Exact match: live-over-archive precedence.
	if len(liveExact) == 1 {
		return liveExact[0], ""
	}
	if len(liveExact) > 1 {
		return TaskCandidate{}, fmt.Sprintf(
			"worktree dir %q matches %d LIVE task rows exactly (conflicting) — skipping", dirSlug, len(liveExact))
	}
	// No live exact. An archive exact is eligible only when no live row
	// claims the slug (which we've established).
	if len(archiveExact) == 1 {
		return archiveExact[0], ""
	}
	if len(archiveExact) > 1 {
		return TaskCandidate{}, fmt.Sprintf(
			"worktree dir %q matches %d archive task rows exactly (conflicting) — skipping", dirSlug, len(archiveExact))
	}

	// Unique-prefix match: the dir name is a prefix of exactly one
	// candidate slug. Live-over-archive applies here too: prefer a unique
	// LIVE prefix; an archive prefix is eligible only when no live prefix
	// matches. Self-exact already handled above, so prefix here means
	// dirSlug is a strict prefix of a longer slug (the -rand / truncation
	// case).
	var livePrefix, archivePrefix []TaskCandidate
	for _, c := range candidates {
		if c.Slug != dirSlug && strings.HasPrefix(c.Slug, dirSlug) {
			if c.FromArchive {
				archivePrefix = append(archivePrefix, c)
			} else {
				livePrefix = append(livePrefix, c)
			}
		}
	}
	if len(livePrefix) == 1 {
		return livePrefix[0], ""
	}
	if len(livePrefix) > 1 {
		return TaskCandidate{}, fmt.Sprintf(
			"worktree dir %q is an ambiguous prefix of %d live task slugs — skipping", dirSlug, len(livePrefix))
	}
	if len(archivePrefix) == 1 {
		return archivePrefix[0], ""
	}
	if len(archivePrefix) > 1 {
		return TaskCandidate{}, fmt.Sprintf(
			"worktree dir %q is an ambiguous prefix of %d archive task slugs — skipping", dirSlug, len(archivePrefix))
	}
	return TaskCandidate{}, fmt.Sprintf(
		"worktree dir %q matches no task row (live or archive) — skipping", dirSlug)
}

// isDirtyRemoveRefusal recognizes git's "refused dirty worktree" error so
// a --no-force removal that loses the TOCTOU race is classified
// dirty-parked rather than a generic remove failure. Mirrors
// worktree.py:_is_dirty_remove_refusal.
func isDirtyRemoveRefusal(msg string) bool {
	low := strings.ToLower(msg)
	return strings.Contains(low, "use --force to delete it") ||
		strings.Contains(low, "modified or untracked files")
}

// listTaskCandidatesOnDisk is the production ListTaskCandidates wiring:
// reads <project>/tasks.md + tasks-archive.md and, per slug, the
// state.json projection (phase + dispatch_generation). Defined here (not
// inline in DefaultDeps) so the §4.4 projection seam is one readable unit.
//
// Read errors on tasks.md surface as the returned error (fail-soft at the
// per-project synthetic-action layer). A missing tasks-archive.md is not
// an error (a project may have never archived). A missing/unparseable
// state.json for a given slug just leaves HasWorkerState=false (the
// generation belt then can't veto — correct: no live worker signal).
func listTaskCandidatesOnDisk(project string) ([]TaskCandidate, error) {
	dir, err := state.ProjectDir(project)
	if err != nil {
		return nil, err
	}
	out := []TaskCandidate{}
	add := func(t tasks.Task, fromArchive bool) {
		c := TaskCandidate{
			Slug:        t.Slug,
			Status:      string(t.Status),
			Branch:      t.Branch,
			Worktree:    t.Worktree,
			FromArchive: fromArchive,
			TaskGen:     t.DispatchGeneration,
		}
		if st, serr := workers.ReadState(project, t.Slug); serr == nil && st != nil {
			c.HasWorkerState = true
			c.WorkerPhase = string(st.Phase)
			c.WorkerStateGen = st.DispatchGeneration
		}
		out = append(out, c)
	}
	tasksPath := filepath.Join(filepath.Clean(dir), "tasks.md")
	lf, lerr := tasks.Read(tasksPath)
	if lerr != nil {
		// A parse/strict error makes the WHOLE project skip — which is
		// fail-safe (skip = keep the tree). Surface it so the operator
		// sees the gap rather than silently reaping on partial data.
		return nil, fmt.Errorf("read tasks.md: %w", lerr)
	}
	for _, t := range lf.Tasks {
		add(*t, false)
	}
	archivePath := filepath.Join(filepath.Clean(dir), "tasks-archive.md")
	af, aerr := tasks.Read(archivePath)
	if aerr != nil {
		return nil, fmt.Errorf("read tasks-archive.md: %w", aerr)
	}
	for _, t := range af.Tasks {
		add(*t, true)
	}
	return out, nil
}

// projectRepoOnDisk resolves the project's base repo (the main checkout
// worktrees were `git worktree add`-ed from). Fleet does not persist a
// per-project repo path, so we derive it from the worktrees themselves:
// the first worktree dir's `--git-common-dir` points at the shared
// `<repo>/.git`; its parent is the base repo. This is self-corroborating
// — a stray dir that points at a DIFFERENT repo won't be registered in
// the resolved base repo, so WorktreeRegistered fails it. Returns "" with
// nil err when the project has no usable worktree (the resolver then
// surfaces every dir, reaps none).
func projectRepoOnDisk(project string) (string, error) {
	dir, err := state.ProjectDir(project)
	if err != nil {
		return "", err
	}
	wtRoot := filepath.Join(filepath.Clean(dir), "worktrees")
	entries, rerr := os.ReadDir(wtRoot)
	if rerr != nil {
		if os.IsNotExist(rerr) {
			return "", nil
		}
		return "", fmt.Errorf("read %s: %w", wtRoot, rerr)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		wt := filepath.Join(wtRoot, e.Name())
		common, cerr := gitCommonDir(wt)
		if cerr != nil || common == "" {
			continue
		}
		// <repo>/.git → <repo>. A bare/linked layout where common-dir is
		// not a `.git` basename is skipped (can't safely infer the repo).
		if filepath.Base(common) == ".git" {
			return filepath.Dir(common), nil
		}
	}
	return "", nil
}

// gitCommonDir returns the absolute shared git dir for a worktree
// (`git -C <wt> rev-parse --path-format=absolute --git-common-dir`).
func gitCommonDir(wt string) (string, error) {
	out, err := exec.Command("git", "-C", wt, "rev-parse",
		"--path-format=absolute", "--git-common-dir").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// worktreeBranchOnDisk is the production WorktreeBranch:
// `git -C <path> rev-parse --abbrev-ref HEAD`. Detached HEAD prints
// "HEAD" → normalized to "" (no branch identity).
func worktreeBranchOnDisk(path string) (string, error) {
	out, err := exec.Command("git", "-C", path, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("rev-parse HEAD in %s: %w", path, err)
	}
	b := strings.TrimSpace(string(out))
	if b == "HEAD" {
		return "", nil
	}
	return b, nil
}

// worktreeDirtyOnDisk is the production WorktreeDirty:
// `git -C <path> status --porcelain` non-empty == dirty.
func worktreeDirtyOnDisk(path string) (bool, error) {
	out, err := exec.Command("git", "-C", path, "status", "--porcelain").Output()
	if err != nil {
		return false, fmt.Errorf("status --porcelain in %s: %w", path, err)
	}
	return strings.TrimSpace(string(out)) != "", nil
}

// worktreeRegisteredOnDisk is the production WorktreeRegistered: scans
// `git -C <repo> worktree list --porcelain` for the target path
// (realpath-compared so a symlinked/trailing-slash variant still matches).
func worktreeRegisteredOnDisk(repo, path string) (bool, error) {
	out, err := exec.Command("git", "-C", repo, "worktree", "list", "--porcelain").Output()
	if err != nil {
		return false, fmt.Errorf("worktree list in %s: %w", repo, err)
	}
	target, terr := filepath.EvalSymlinks(path)
	if terr != nil {
		target = filepath.Clean(path)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.HasPrefix(line, "worktree ") {
			continue
		}
		listed := strings.TrimSpace(strings.TrimPrefix(line, "worktree "))
		if listed == "" {
			continue
		}
		resolved, rerr := filepath.EvalSymlinks(listed)
		if rerr != nil {
			resolved = filepath.Clean(listed)
		}
		if resolved == target {
			return true, nil
		}
	}
	return false, nil
}

// removeWorktreeNoForce is the production RemoveWorktreeNoForce:
// `git -C <repo> worktree remove --no-force <path>`. --no-force makes
// git REFUSE a tree that turned dirty after the dirty-check (the TOCTOU
// belt); the resolver classifies that refusal as dirty-parked. Already-
// gone path collapses to nil (idempotent).
func removeWorktreeNoForce(repo, path string) error {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat %s: %w", path, err)
	}
	out, err := exec.Command("git", "-C", repo, "worktree", "remove", "--no-force", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("git worktree remove --no-force %s: %w (%s)", path, err, strings.TrimSpace(string(out)))
	}
	return nil
}
