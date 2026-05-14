// Package handoffop — worker_cleanup.go owns KillAgentsForTask, the
// worker-cleanup gate that runs when a task transitions to a terminal
// status (done | abandoned). The operator's brief
// (fix/atomic-coord-swap-and-worker-cleanup) makes this a BLOCKING
// step: a task isn't "done" until every fleet agent attached to that
// task is killed and archived. Without this, fleet workers accumulate
// the same way coordinators did before the orphan-tmux leak plug —
// each completed task leaves a live tmux session that nothing tracks.
//
// Scope: this helper only touches tmux-backed fleet AGENTS (records
// under ~/.fleet/agents/<id>.json with TaskID set). It does NOT touch:
//
//   - Workers that are `claude --print` subprocesses tracked via
//     ~/.fleet/projects/<p>/workers/<slug>/state.json — those are
//     handled by internal/lifecycle.OnTerminal + workers.Delete.
//   - Coordinator-skill gstack-style subagents (Agent tool with
//     run_in_background) — those are not tmux-backed; their parent
//     coord owns their lifecycle.
//
// The selector is conservative: TaskID match + Project match + non-
// empty TmuxSession. Empty TmuxSession means a legacy or no-tmux
// record — leave it untouched, the operator can `fleet rm` it
// manually.

package handoffop

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/state"
)

// WorkerCleanupOpts bundles inputs to KillAgentsForTask.
type WorkerCleanupOpts struct {
	// Project is required.
	Project string
	// TaskSlug is required. Matches against agent.Record.TaskID.
	TaskSlug string
	// KeepSession opts out of the tmux.Kill step. Default false:
	// kill + archive. True: skip kill, still archive the record.
	// Used by `fleet tasks set ... --keep-session`.
	KeepSession bool
	// Stderr receives non-fatal notes. nil-tolerant.
	Stderr io.Writer
}

// WorkerCleanupResult records what KillAgentsForTask did.
type WorkerCleanupResult struct {
	Matched  int
	Killed   int
	Archived int
	IDs      []string
}

// ErrWorkerCleanupFailed wraps the per-agent failure. Caller in
// cmd/fleet/tasks.go must treat it as a hard refusal to write the
// terminal status.
var ErrWorkerCleanupFailed = errors.New("worker cleanup failed; refusing to mark task terminal")

// KillAgentsForTask is the cleanup gate. See file-level doc.
func KillAgentsForTask(opts WorkerCleanupOpts) (WorkerCleanupResult, error) {
	res := WorkerCleanupResult{}
	if opts.Project == "" {
		return res, fmt.Errorf("%w: empty project", ErrWorkerCleanupFailed)
	}
	if opts.TaskSlug == "" {
		return res, fmt.Errorf("%w: empty task slug", ErrWorkerCleanupFailed)
	}
	// codex iter-1 [P2] + iter-2 [P1]: use ListStrict so unparseable
	// agent records can't silently slip past the cleanup gate.
	// agent.List drops unparseable records (the documented triage
	// behavior at internal/agent/agent.go:List), which would let a
	// task transition to done|abandoned while a still-live worker
	// with a corrupt record stays attached and tmux-running.
	//
	// codex iter-2 [P1]: a global fail-closed on any bad record turns
	// localized record damage into a fleet-wide block on every task
	// transition. The safe-but-narrow policy: collect bad IDs, run
	// the loop against parsed records, and fail closed at the END
	// only if we matched real workers (in which case a bad record
	// COULD be another match we missed). On a no-op transition
	// (zero matched workers) we surface bad records as a warning
	// via stderr and proceed — the operator's `tasks set` for an
	// unrelated project shouldn't be wedged by a hand-edited record
	// elsewhere.
	all, badIDs, err := agent.ListStrict()
	if err != nil {
		return res, fmt.Errorf("%w: list agents: %w", ErrWorkerCleanupFailed, err)
	}
	// Pass 1: filter to matching records WITHOUT mutating any. Coord
	// records are excluded (iter-2 [P1]); empty-session records are
	// skipped; only TaskID + Project matches contribute.
	var matches []*agent.Record
	for _, rec := range all {
		if rec == nil {
			continue
		}
		if isProjectCoordRecord(rec.ID, rec.TaskID, rec.Project) {
			continue
		}
		if rec.TaskID != opts.TaskSlug || rec.Project != opts.Project {
			continue
		}
		matches = append(matches, rec)
	}
	// codex iter-2 [P1] + iter-4 [P2] + iter-7 [P1]: bad-records gate.
	// Apply the raw-text relevance filter UNIFORMLY: a bad record only
	// counts against this task if its raw bytes mention this task's
	// slug OR project name. Two outcomes:
	//
	//   - any plausibly-related bad record: refuse the transition.
	//     The "ALL agents for this task are corrupt" failure mode
	//     and the "matches exist + some unrelated bad record" failure
	//     mode have the same fix: hide the bad record's contents
	//     might still be a worker we missed, but only if those
	//     contents reference this task at all.
	//
	//   - no plausibly-related bad records: warn (if any bad records
	//     exist) and proceed. A corrupt record in an unrelated
	//     project shouldn't wedge every task transition.
	//
	// The raw-text check is a safety net, not a parser: it grep-
	// matches `"task_id": "<slug>"` OR `"project": "<project>"`
	// literals against the file bytes. Either substring is enough —
	// requiring BOTH (iter-4) missed truncated records where one
	// field is lost (iter-7 P1). False positives are tolerable
	// (refuse; operator triages). False negatives only happen when
	// the record is corrupted past every reference to slug/project,
	// at which point the record cannot meaningfully belong to this
	// task anyway.
	if len(badIDs) > 0 {
		if relevant := badRecordsPlausiblyRelated(badIDs, opts.Project, opts.TaskSlug); len(relevant) > 0 {
			return res, fmt.Errorf(
				"%w: %d agent record(s) unreadable AND raw-text match task %s or project %s (%v); refusing to mark terminal — fix or remove the unreadable records and retry",
				ErrWorkerCleanupFailed, len(relevant), opts.TaskSlug, opts.Project, relevant)
		}
		if opts.Stderr != nil {
			_, _ = fmt.Fprintf(opts.Stderr,
				"warning: %d agent record(s) unreadable (%v); none match task %s or project %s, cleanup gate proceeds — operator should triage the corrupt records\n",
				len(badIDs), badIDs, opts.TaskSlug, opts.Project)
		}
	}
	// codex iter-9 [P1] + iter-10 [P1]: after the kill+archive loop
	// we'll re-list and refuse the transition if ANY live record
	// matches this task. Two cases this catches:
	//   (a) Concurrent fleet handoff / auto-handoff wrote a fresh
	//       replacement after our snapshot (iter-9).
	//   (b) A late fleet-guard health tick or other writer
	//       resurrected an agents/<id>.json we just archived
	//       (iter-10) — Archive moves the file, a subsequent
	//       UpdateState recreates it.
	// In both cases the post-cleanup check sees the live record and
	// refuses. The operator's recovery is to re-run `fleet tasks set`,
	// which will pick up the live record on its own snapshot.

	// Pass 2: kill + archive each matched record.
	for _, rec := range matches {
		res.Matched++
		res.IDs = append(res.IDs, rec.ID)

		// Only kill when there's a real tmux session to kill. Records
		// with empty TmuxSession (legacy / no-tmux) still get
		// archived below — they were dropped on the floor previously
		// (codex iter-8 P2 regression).
		if !opts.KeepSession && rec.TmuxSession != "" {
			if err := tmuxKillForCleanup(rec.TmuxSession); err != nil {
				alive, probeErr := tmuxSessionAliveForCleanup(rec.TmuxSession)
				switch {
				case probeErr != nil:
					return res, fmt.Errorf(
						"%w: kill worker tmux %s failed (%w) AND post-kill probe ambiguous (%w); task %s NOT marked terminal",
						ErrWorkerCleanupFailed, rec.TmuxSession, err, probeErr, opts.TaskSlug)
				case alive:
					return res, fmt.Errorf(
						"%w: kill worker tmux %s failed AND session still alive: %w; task %s NOT marked terminal",
						ErrWorkerCleanupFailed, rec.TmuxSession, err, opts.TaskSlug)
				default:
					if opts.Stderr != nil {
						_, _ = fmt.Fprintf(opts.Stderr,
							"note: kill worker %s (session %s) reported error but session is gone: %v\n",
							rec.ID, rec.TmuxSession, err)
					}
				}
			}
			res.Killed++
		}

		// Archive the record — only when there's something worth
		// preserving:
		//   - KeepSession=false: always archive (session is dead).
		//   - KeepSession=true AND non-empty TmuxSession: skip
		//     archive (the operator wants `fleet attach <id>` to
		//     reach the preserved tmux session — codex iter-1 P1).
		//   - KeepSession=true AND empty TmuxSession: archive
		//     anyway. There's no tmux to preserve, so leaving the
		//     record live just stale-displays in fleet status
		//     (codex iter-11 P2).
		archiveThis := !opts.KeepSession || rec.TmuxSession == ""
		if archiveThis {
			if err := rec.Archive(); err != nil {
				path, perr := state.AgentPath(rec.ID)
				if perr != nil {
					return res, fmt.Errorf(
						"%w: archive worker %s failed (%w) AND could not resolve path (%w); task %s NOT marked terminal",
						ErrWorkerCleanupFailed, rec.ID, err, perr, opts.TaskSlug)
				}
				if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
					return res, fmt.Errorf(
						"%w: archive worker %s failed (%w) AND fallback remove failed (%w); task %s NOT marked terminal",
						ErrWorkerCleanupFailed, rec.ID, err, rmErr, opts.TaskSlug)
				}
				if opts.Stderr != nil {
					_, _ = fmt.Fprintf(opts.Stderr,
						"warning: archive worker %s: %v (live record removed instead, archive copy lost)\n",
						rec.ID, err)
				}
			}
			res.Archived++
		}
	}

	// codex iter-9 [P1]: race re-check. Between our snapshot at the
	// top of this function and now, a concurrent `fleet handoff` /
	// auto-handoff may have written a fresh replacement record for
	// this task. If so, our cleanup is incomplete — the new record
	// is live and untouched. Re-list and refuse the transition so
	// the operator (or the next `tasks set`) catches the new record.
	//
	// Skip the re-check in KeepSession mode: the operator INTENTIONALLY
	// preserved the live records, so they'll be there. The cleanup
	// gate's invariant ("no live record for this task after cleanup")
	// doesn't apply when the operator opted out of the cleanup.
	if opts.KeepSession {
		return res, nil
	}
	postAll, postBadIDs, perr := agent.ListStrict()
	if perr != nil {
		return res, fmt.Errorf(
			"%w: post-cleanup re-list failed: %w (workers killed but cannot confirm none appeared concurrently; task %s NOT marked terminal — retry)",
			ErrWorkerCleanupFailed, perr, opts.TaskSlug)
	}
	var stillLive []string
	for _, rec := range postAll {
		if rec == nil {
			continue
		}
		if isProjectCoordRecord(rec.ID, rec.TaskID, rec.Project) {
			continue
		}
		if rec.TaskID != opts.TaskSlug || rec.Project != opts.Project {
			continue
		}
		// iter-10 P1: ANY live record matching this task means
		// invariant violation. Don't filter by snapshot — a record
		// with an ID we already archived could have been resurrected
		// by a late writer.
		stillLive = append(stillLive, rec.ID)
	}
	if len(stillLive) > 0 {
		return res, fmt.Errorf(
			"%w: %d agent record(s) live for task %s after cleanup (%v) — concurrent handoff or late writer resurrected a record; task NOT marked terminal — re-run `fleet tasks set` to clean them up",
			ErrWorkerCleanupFailed, len(stillLive), opts.TaskSlug, stillLive)
	}
	if len(postBadIDs) > 0 {
		if relevant := badRecordsPlausiblyRelated(postBadIDs, opts.Project, opts.TaskSlug); len(relevant) > 0 {
			return res, fmt.Errorf(
				"%w: %d unreadable record(s) appeared during cleanup AND raw-text match task %s (%v); task NOT marked terminal — fix the records and retry",
				ErrWorkerCleanupFailed, len(relevant), opts.TaskSlug, relevant)
		}
	}
	return res, nil
}

// badRecordsPlausiblyRelated reads each unparseable agent record's
// raw bytes and reports those whose contents could plausibly belong
// to the named task. Safety net for the codex iter-4 [P2] /
// iter-7 [P1] / iter-11 [P1] case: when agent.ListStrict surfaces
// unparseable records, we can't Load them, but their raw JSON
// usually still has identifiable key/value pairs.
//
// Relation rule (iter-11 P1 + iter-13 P2): require task_id + slug
// match AND project match. Task slugs are only unique within a
// project — project A and B could both have "fix-login-1234", so
// matching on slug alone would block cross-project transitions
// (iter-13). Project alone is too broad (iter-11). Both together
// give the same selector the parsed loop uses.
//
// We grep for BOTH `"task_id"` AND `"<slug>"` AND `"project"` AND
// `"<project>"`. Records whose corruption ate any of these fields
// fall through as "not related" — symmetric with iter-2's policy
// of not wedging fleet-wide on unrelated corrupt records.
//
// Read errors treat the record as "could match" — we already know
// it's unreadable; surfacing it as related lets the gate refuse.
// ENOENT (concurrently removed) is NOT related (iter-9 P2).
func badRecordsPlausiblyRelated(badIDs []string, project, slug string) []string {
	var related []string
	taskMarker := `"task_id"`
	slugMarker := `"` + slug + `"`
	projMarker := `"project"`
	projValueMarker := `"` + project + `"`
	for _, id := range badIDs {
		path, perr := state.AgentPath(id)
		if perr != nil {
			related = append(related, id)
			continue
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			if errors.Is(rerr, os.ErrNotExist) {
				continue
			}
			related = append(related, id)
			continue
		}
		s := string(data)
		hasTask := strings.Contains(s, taskMarker) && strings.Contains(s, slugMarker)
		hasProj := strings.Contains(s, projMarker) && strings.Contains(s, projValueMarker)
		// iter-13 P2: BOTH required — task_id+slug AND project+project.
		if hasTask && hasProj {
			related = append(related, id)
		}
	}
	return related
}

// isProjectCoordRecord reports whether a record IS its project's
// coordinator agent. Two-signal check (codex iter-2 P1 + iter-6 P2):
//
//  1. TaskID matches the "coord-<project>" sentinel, AND
//  2. The project's coord-spawn marker points at this exact agent ID.
//
// Both signals together rule out the false-positive case where an
// operator names a real task "coord-<project>" — that task's worker
// agent won't have the coord-spawn marker pointing at it. Only the
// actual coord-spawn flow writes the marker.
//
// Empty TaskID / Project / agent ID all return false (definitely
// not a coord — bail out cheap).
func isProjectCoordRecord(agentID, taskID, project string) bool {
	if agentID == "" || taskID == "" || project == "" {
		return false
	}
	if taskID != "coord-"+project {
		return false
	}
	return state.ReadCoordSpawnMarker(project) == agentID
}

// tmuxKillForCleanup / tmuxSessionAliveForCleanup are package vars so
// tests inject fakes. Production binds to the same tmux helpers
// DropReplacementRecord uses — separate vars so test stubs of one
// path don't bleed into the other.
var (
	tmuxKillForCleanup         = tmuxKillFn
	tmuxSessionAliveForCleanup = tmuxSessionAliveFn
)
