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
	// codex iter-1 [P2]: use ListStrict so a malformed agent record
	// fails the cleanup gate instead of silently slipping through.
	// agent.List drops unparseable records (the documented triage
	// behavior at internal/agent/agent.go:List), which would let a
	// task transition to done|abandoned while a still-live worker
	// with a corrupt record stays attached and tmux-running.
	all, badIDs, err := agent.ListStrict()
	if err != nil {
		return res, fmt.Errorf("%w: list agents: %w", ErrWorkerCleanupFailed, err)
	}
	if len(badIDs) > 0 {
		return res, fmt.Errorf(
			"%w: %d agent record(s) unreadable (%v); refusing to mark task %s terminal — fix or remove the records and retry",
			ErrWorkerCleanupFailed, len(badIDs), badIDs, opts.TaskSlug)
	}
	for _, rec := range all {
		if rec == nil {
			continue
		}
		if rec.TaskID != opts.TaskSlug || rec.Project != opts.Project {
			continue
		}
		if rec.TmuxSession == "" {
			continue
		}
		res.Matched++
		res.IDs = append(res.IDs, rec.ID)

		if !opts.KeepSession {
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

		// Archive the record — only when we killed the session. With
		// --keep-session the tmux session stays alive, so we LEAVE the
		// record live too: that's the only path operators have to
		// `fleet attach` the preserved session for debugging (codex
		// iter-1 [P1]). Archive in --keep-session would hide the
		// session from the very lookup the operator runs to inspect
		// it.
		//
		// In the strict cleanup path (KeepSession=false), the tmux
		// session is dead by here, so archiving cannot hide a live
		// agent. Fall back to direct os.Remove on archive failure to
		// match the pattern in runHandoff step 12 / retireOldAgent.
		if !opts.KeepSession {
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
	return res, nil
}

// tmuxKillForCleanup / tmuxSessionAliveForCleanup are package vars so
// tests inject fakes. Production binds to the same tmux helpers
// DropReplacementRecord uses — separate vars so test stubs of one
// path don't bleed into the other.
var (
	tmuxKillForCleanup         = tmuxKillFn
	tmuxSessionAliveForCleanup = tmuxSessionAliveFn
)
