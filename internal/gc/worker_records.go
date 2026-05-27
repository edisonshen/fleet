package gc

// worker_records.go — sixth classifier for `fleet gc`: scans
// `~/.fleet/projects/<p>/workers/<slug>/state.json` files across every
// project and flags four failure modes that survive after the worker is
// gone:
//
//   1. Mismatch — state.json's `project` field disagrees with the
//      parent-dir project name. PRs #173/#174 (fleet#170/#171) closed
//      the NEW cross-project write window at spawn + tick time; this
//      classifier sweeps historical state from BEFORE that fix landed.
//
//   2. Dead-pid — state.json's `pid` is set, the process is gone, AND
//      `updated_at` is older than 24h. The worker crashed without
//      clearing its record OR was force-killed.
//
//   3. Stale-heartbeat — `pid=0` AND `updated_at` is older than 24h.
//      Worker either never claimed the row (spawn failed) or died
//      before flipping pid back to 0 properly.
//
//   4. Task-terminal — the corresponding task entry in tasks.md is
//      `done` or `abandoned` but the worker dir survives on disk.
//
// Default behavior: surface (Verb=surface). `--apply` upgrades to
// would-remove / removed (atomic os.RemoveAll of the worker dir —
// state.json + output.log + scratch files). The worktree is at a
// SEPARATE path (`~/.fleet/projects/<p>/worktrees/<slug>/`) so this
// classifier NEVER touches it — that's KindWorktrees' job.
//
// Two load-bearing guards:
//
//   - Live-PID guard: when PidAlive(state.pid) returns true, the
//     classifier is a no-op regardless of how stale updated_at is.
//     Reaping a live worker would orphan a running subagent process.
//
//   - Task-in-progress guard: when tasks.md still says `in-progress`,
//     the classifier surfaces but refuses to apply. Coord reconcile
//     owns the in-progress path (task → worker reattachment); this
//     classifier is the on-disk cleanup that runs AFTER coord has
//     declared the task terminal.
//
// Out of scope (per task brief):
//   - Worktree cleanup (KindWorktrees owns it).
//   - Retroactive task-state repair if state.json + tasks.md disagree.
//   - Automatic reaping on coord tick (operator runs `fleet gc --apply`).
//   - Auto-cleanup of legacy coord-locks orphans (#176 already covers).

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/edisonshen/fleet/internal/state"
)

// KindWorkerRecords is the sixth classifier — worker state.json sweep.
const KindWorkerRecords Kind = "worker-records"

// workerRecordsStaleHours is the age floor for the dead-pid +
// stale-heartbeat branches. A worker dir less than this old is treated
// as in-flight (spawn races, subagent dispatch overlap) and left alone.
const workerRecordsStaleHours = 24 * time.Hour

// WorkerRecordInfo is the minimal per-state.json shape the classifier
// needs from the lister. Project is the PARENT-DIR project name (the
// authoritative "what dir does this live under?" answer, used for the
// mismatch detection). Slug is the worker slug (dir basename). Path is
// the worker DIR (parent of state.json), passed to RemoveWorkerRecord.
type WorkerRecordInfo struct {
	Project string
	Slug    string
	Path    string
}

// WorkerState is the parsed subset of state.json the classifier needs.
// The coord skill writes additional fields (phase, phases_completed,
// review_*_status, ...) but those don't affect classification — they're
// silently dropped by the json decoder.
type WorkerState struct {
	Slug      string    `json:"slug"`
	Project   string    `json:"project"`
	Pid       int       `json:"pid"`
	UpdatedAt time.Time `json:"updated_at"`
}

// reconcileWorkerRecords runs the four-mode classifier over every
// state.json the lister yields.
//
// Per-record flow:
//
//	WorkerRecordInfo + state.json →
//	    parse error                → SURFACE "parse failure" (don't silo)
//	    state.project != info.Project → MISMATCH
//	    state.pid set + alive      → SKIP (live-PID guard)
//	    state.pid set + dead + old → DEAD-PID
//	    state.pid == 0 + old       → STALE-HEARTBEAT
//	    task in done|abandoned     → TASK-TERMINAL
//	    otherwise                  → no action (healthy)
//
// Mismatch is checked BEFORE the live-PID guard because a
// cross-project record claiming a still-live PID is a bug worth
// surfacing regardless of liveness — the operator needs to know the
// worker is running against the wrong project's tasks.md.
//
// Apply path: any classified-and-surfaced action upgrades to
// would-remove → removed (via RemoveWorkerRecord) under --apply,
// EXCEPT the task-in-progress branch which always surfaces (coord
// reconcile owns that path) and the parse-error branch which surfaces
// only (can't safely apply against unparseable state).
//
// Project scope (opts.Project): when non-empty, only that project's
// workers/ subtree is examined.
func reconcileWorkerRecords(r *Report, opts Options, deps Deps) error {
	if deps.ListWorkerRecords == nil {
		return errors.New("worker-records: ListWorkerRecords dep not wired")
	}
	if deps.LoadWorkerState == nil {
		return errors.New("worker-records: LoadWorkerState dep not wired")
	}
	if deps.LoadTaskStatus == nil {
		return errors.New("worker-records: LoadTaskStatus dep not wired")
	}
	if deps.PidAlive == nil {
		return errors.New("worker-records: PidAlive dep not wired")
	}
	records, err := deps.ListWorkerRecords()
	if err != nil {
		return err
	}
	// Stable per-classifier order; the outer sort re-sorts by Kind+Target.
	sort.SliceStable(records, func(i, j int) bool { return records[i].Path < records[j].Path })

	now := deps.Now()
	stale := workerRecordsStaleHours

	for _, info := range records {
		if opts.Project != "" && info.Project != opts.Project {
			continue
		}
		ws, perr := deps.LoadWorkerState(info.Path)
		if perr != nil {
			// surface-don't-silo: corrupt / partial state.json files
			// shouldn't tank the sweep, but operator needs to see them.
			r.Actions = append(r.Actions, Action{
				Kind: KindWorkerRecords, Target: info.Path, Verb: VerbSurface,
				Reason: fmt.Sprintf("parse failure: %v; surface only — investigate manually before removing", perr),
			})
			continue
		}

		// 1. Mismatch — state.project disagrees with parent dir.
		if ws.Project != "" && ws.Project != info.Project {
			reason := fmt.Sprintf("mismatch (worker dir=%s vs state.project=%s)", info.Project, ws.Project)
			appendWorkerRecordAction(r, opts, deps, info, reason)
			continue
		}

		// 2. Live-PID guard — never touch a live worker.
		if ws.Pid > 0 && deps.PidAlive(ws.Pid) {
			continue
		}

		age := now.Sub(ws.UpdatedAt)

		// 3. Dead-pid — pid set, process gone, old.
		if ws.Pid > 0 && age >= stale {
			reason := fmt.Sprintf("dead-pid (pid=%d gone, last update %s ago)", ws.Pid, humanDuration(age))
			appendWorkerRecordWithTaskGuard(r, opts, deps, info, reason)
			continue
		}

		// 4. Stale-heartbeat — pid=0, old.
		if ws.Pid == 0 && age >= stale {
			reason := fmt.Sprintf("stale-heartbeat (pid=0, last update %s ago)", humanDuration(age))
			appendWorkerRecordWithTaskGuard(r, opts, deps, info, reason)
			continue
		}

		// 5. Task-terminal — task done/abandoned, but worker dir survives.
		status, terr := deps.LoadTaskStatus(info.Project, info.Slug)
		if terr != nil {
			// Tasks.md probe failed; refuse to act (ambiguous state).
			continue
		}
		if status == "done" || status == "abandoned" {
			reason := fmt.Sprintf("task-terminal (task status=%s, worker dir orphan post-archive)", status)
			// task-terminal IS a confirmed terminal state — apply is
			// safe without the task-in-progress guard.
			appendWorkerRecordAction(r, opts, deps, info, reason)
			continue
		}
		// Otherwise healthy — no action.
	}
	return nil
}

// appendWorkerRecordAction builds the per-record Action (default
// Verb=surface) and, under --apply, calls RemoveWorkerRecord to remove
// the worker directory. Failure on remove flips Verb back to surface
// and the Reason carries the remove error so the operator can re-run
// with elevated perms.
func appendWorkerRecordAction(r *Report, opts Options, deps Deps, info WorkerRecordInfo, reason string) {
	act := Action{
		Kind:   KindWorkerRecords,
		Target: info.Path,
		Verb:   VerbSurface,
		Reason: reason + "; surface only — rerun with --apply to remove",
	}
	if opts.Apply {
		act.Verb = VerbWouldRemove
		act.Reason = reason
		if deps.RemoveWorkerRecord == nil {
			act.Verb = VerbSurface
			act.Reason = reason + "; --apply requested but RemoveWorkerRecord dep not wired"
		} else if rerr := deps.RemoveWorkerRecord(info.Path); rerr != nil {
			act.Verb = VerbSurface
			act.Reason = fmt.Sprintf("%s; remove failed: %v", reason, rerr)
		} else {
			act.Verb = VerbRemoved
		}
	}
	r.Actions = append(r.Actions, act)
}

// appendWorkerRecordWithTaskGuard wraps appendWorkerRecordAction with
// the task-in-progress guard: if tasks.md still says the task is
// in-progress, the action surfaces but never applies (coord reconcile
// owns that path).
//
// Probe error → conservative skip-of-apply (treat as in-progress).
// surface-don't-silo: the operator sees the action either way; the
// apply path just won't fire on ambiguous state.
func appendWorkerRecordWithTaskGuard(r *Report, opts Options, deps Deps, info WorkerRecordInfo, reason string) {
	status, terr := deps.LoadTaskStatus(info.Project, info.Slug)
	if terr != nil || status == "in-progress" {
		// Surface only — coord reconcile owns the in-progress path.
		extra := "; task in-progress per tasks.md — coord reconcile owns this path, not gc"
		if terr != nil {
			extra = fmt.Sprintf("; tasks.md probe failed (%v) — refusing to apply on ambiguous state", terr)
		}
		r.Actions = append(r.Actions, Action{
			Kind: KindWorkerRecords, Target: info.Path, Verb: VerbSurface,
			Reason: reason + extra,
		})
		return
	}
	appendWorkerRecordAction(r, opts, deps, info, reason)
}

// listWorkerRecordsOnDisk is the production ListWorkerRecords. Walks
// every project dir under ~/.fleet/projects/ and yields one
// WorkerRecordInfo per workers/<slug>/state.json file.
//
// Skipped:
//   - .locks (reserved sibling)
//   - workers/archive/* (fleet's cold-storage subtree; not live)
//   - any entries that fail state.ValidateProjectName (legacy / hand-edit)
func listWorkerRecordsOnDisk() ([]WorkerRecordInfo, error) {
	root, err := state.Root()
	if err != nil {
		return nil, err
	}
	pdir := filepath.Join(root, "projects")
	entries, err := os.ReadDir(pdir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", pdir, err)
	}
	var out []WorkerRecordInfo
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if name == ".locks" {
			continue
		}
		if err := state.ValidateProjectName(name); err != nil {
			continue
		}
		workersRoot := filepath.Join(pdir, name, "workers")
		wEntries, werr := os.ReadDir(workersRoot)
		if werr != nil {
			if os.IsNotExist(werr) {
				continue
			}
			return nil, fmt.Errorf("read %s: %w", workersRoot, werr)
		}
		for _, w := range wEntries {
			if !w.IsDir() {
				continue
			}
			slug := w.Name()
			if slug == "archive" {
				// archive/ is fleet's own cold-storage subtree (not a
				// live worker). out of scope.
				continue
			}
			workerDir := filepath.Join(workersRoot, slug) + string(filepath.Separator)
			statePath := filepath.Join(workersRoot, slug, "state.json")
			st, serr := os.Stat(statePath)
			if serr != nil || st.IsDir() {
				continue
			}
			out = append(out, WorkerRecordInfo{
				Project: name,
				Slug:    slug,
				Path:    workerDir,
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// loadWorkerStateOnDisk is the production LoadWorkerState. Reads the
// state.json at the given worker DIR + "state.json" path and decodes
// the subset of fields the classifier needs.
//
// path can be either the worker dir (with trailing separator) OR the
// state.json file directly — both round-trip cleanly.
func loadWorkerStateOnDisk(path string) (WorkerState, error) {
	// Tolerant of either "/foo/bar/" or "/foo/bar/state.json" callers.
	if !strings.HasSuffix(path, "state.json") {
		path = filepath.Join(path, "state.json")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return WorkerState{}, fmt.Errorf("read %s: %w", path, err)
	}
	var ws WorkerState
	if err := json.Unmarshal(data, &ws); err != nil {
		return WorkerState{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return ws, nil
}

// loadTaskStatusOnDisk is the production LoadTaskStatus. Reads tasks.md
// (and tasks-archive.md as fallback) for the given project + slug and
// returns the raw status string ("in-progress" / "done" / "abandoned" /
// etc.). Returns "" + error when the file cannot be read or the slug
// is absent from both files.
//
// Reuses taskStatusInFile (gc.go) for the parser — same fence-aware,
// column-0-anchored grammar as KindWorktrees uses for IsTaskTerminal.
// We need the FULL status string, not just the terminal/non-terminal
// boolean IsTaskTerminal returns, so we add a thin wrapper that
// returns the raw value.
func loadTaskStatusOnDisk(project, slug string) (string, error) {
	dir, err := state.ProjectDir(project)
	if err != nil {
		return "", err
	}
	tasksPath := filepath.Join(filepath.Clean(dir), "tasks.md")
	if status, found, ferr := taskStatusRawInFile(tasksPath, slug); ferr != nil {
		return "", ferr
	} else if found {
		return status, nil
	}
	archivePath := filepath.Join(filepath.Clean(dir), "tasks-archive.md")
	if status, found, ferr := taskStatusRawInFile(archivePath, slug); ferr != nil {
		return "", ferr
	} else if found {
		// Archive = terminal by definition. If the archived row's
		// status bullet is missing / non-terminal, the classifier treats
		// the dir as task-terminal anyway because presence in the
		// archive is dispositive (mirrors KindWorktrees logic).
		if status == "" {
			return "done", nil
		}
		return status, nil
	}
	// Slug not present in either file — treat as "no task entry"; the
	// classifier interprets this as not-terminal so the worker dir is
	// kept (surface-don't-silo; could be a partial workflow).
	return "", nil
}

// taskStatusRawInFile is a thin variant of taskStatusInFile that returns
// the RAW status string rather than just the terminal boolean. Shares
// the same fence-aware parser invariants — see taskStatusInFile's
// docstring (gc.go) for the grammar discussion.
func taskStatusRawInFile(path, slug string) (status string, found bool, err error) {
	data, rerr := os.ReadFile(path)
	if rerr != nil {
		if os.IsNotExist(rerr) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read %s: %w", path, rerr)
	}
	wantHeader := "## task: " + slug
	lines := strings.Split(string(data), "\n")
	inFence := false
	for i, line := range lines {
		if strings.HasPrefix(line, "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if line != wantHeader {
			continue
		}
		innerFence := false
		for j := i + 1; j < len(lines); j++ {
			lj := lines[j]
			if strings.HasPrefix(lj, "```") {
				innerFence = !innerFence
				continue
			}
			if innerFence {
				continue
			}
			if strings.HasPrefix(lj, "## ") {
				break
			}
			if strings.HasPrefix(lj, "- status:") {
				val := strings.TrimSpace(strings.TrimPrefix(lj, "- status:"))
				return val, true, nil
			}
		}
		return "", true, nil
	}
	return "", false, nil
}

// pidAliveOnDisk is the production PidAlive. Uses signal 0 — POSIX
// "is the process there?" probe that doesn't actually deliver a signal.
// Returns false on any error (process gone, permission denied on a
// different user's pid, etc).
func pidAliveOnDisk(pid int) bool {
	if pid <= 0 {
		return false
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return false
	}
	return true
}

// removeWorkerRecordDir is the production RemoveWorkerRecord. Removes
// the entire worker directory (state.json + output.log + scratch).
// Atomic enough — by the time the classifier has decided to act, the
// worker is orphan (no live PID, no live coord owns it) so no other
// process should be writing to the dir.
//
// ENOENT-tolerant: two operators racing `fleet gc --apply` mustn't see
// spurious errors.
func removeWorkerRecordDir(path string) error {
	if err := os.RemoveAll(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}
