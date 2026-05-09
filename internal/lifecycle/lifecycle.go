// Package lifecycle is the cross-entity classification + cleanup
// orchestrator for Fleet (issue #101).
//
// Tasks, workers, and coord agent records each carry their own granular
// status enums (tasks.StatusInProgress, workers.PhaseTDDRed, agent
// records' alive/asking/blocked variants). The operator-visible
// surfaces continue to use those granular values verbatim — nothing
// here renames or collapses them. Lifecycle adds a small abstract layer
// that the cleanup machinery + TUI history grouping read against:
//
//	PRERUN → ACTIVE ⇄ WAITING → TERMINAL_SUCCESS
//	                              ↘ TERMINAL_FAILURE
//
// Classify(x) returns one of the five abstract states for any
// supported entity type. OnTerminal(x) runs the cleanup the design
// document spells out per-entity (issue #101 "Cleanup rules"):
//
//	Task:   no-op (history group is a TUI-side render decision; the
//	        markdown row stays put).
//	Worker: rm -rf workers/<slug>/ via workers.Delete. Idempotent on
//	        ENOENT.
//	Agent:  no-op. Coord-skill 4h auto-idle-stop owns the agent-record
//	        lifecycle; auto-mutating operator data here would violate
//	        the user_owns_tmux_config principle.
//
// The orchestrator is intentionally thin — entity packages keep their
// own enum + write paths. Future entities (a v0.6 long-running task
// type, e.g.) plug in by adding one Classify branch + one OnTerminal
// branch here.
package lifecycle

import (
	"fmt"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/tasks"
	"github.com/edisonshen/fleet/internal/workers"
)

// State is the abstract lifecycle state. Granular per-entity enums
// (tasks.Status, workers.Phase, agent record fields) map onto these
// five values via Classify.
type State int

const (
	// StateUnknown is the zero value — Classify falls back to this for
	// unrecognized inputs (nil pointers, future enum values this build
	// doesn't know). Callers MUST treat StateUnknown as "do nothing"
	// rather than guessing — the cleanup orchestrator is gated on
	// known terminal states only.
	StateUnknown State = iota
	// StatePrerun: the entity exists but hasn't started running yet.
	// Tasks in todo/ready, workers bootstrapped at phase=starting/branch
	// before TDD begins. No cleanup applies.
	StatePrerun
	// StateActive: the entity is doing work right now. Tasks
	// in-progress / in-review, workers in the TDD/review/push pipe,
	// fresh agents with no flags set.
	StateActive
	// StateWaiting: the entity is parked on input it can't proceed
	// without. Tasks blocked, workers blocked, agent records flagged
	// asking/blocked. Distinct from terminal: a Waiting entity may
	// resume Active once unblocked, so cleanup MUST NOT fire here.
	StateWaiting
	// StateTerminalSuccess: the entity finished its work normally.
	// Tasks done, workers phase=done, (agent records have no success
	// state — the coord skill manages its own lifecycle). OnTerminal
	// applies entity-specific cleanup.
	StateTerminalSuccess
	// StateTerminalFailure: the entity failed unrecoverably. Tasks
	// abandoned, workers phase=failed, agent records past their
	// active window with no responsive process. OnTerminal applies
	// entity-specific cleanup.
	StateTerminalFailure
)

// String returns a stable name for s. Used for error messages + tests
// — never rendered to the operator (the granular per-entity status is
// the operator-visible label).
func (s State) String() string {
	switch s {
	case StatePrerun:
		return "prerun"
	case StateActive:
		return "active"
	case StateWaiting:
		return "waiting"
	case StateTerminalSuccess:
		return "terminal-success"
	case StateTerminalFailure:
		return "terminal-failure"
	}
	return "unknown"
}

// IsTerminal reports whether s is a terminal state (success or failure).
// Cleanup is gated on this — Prerun/Active/Waiting/Unknown all return
// false.
func (s State) IsTerminal() bool {
	return s == StateTerminalSuccess || s == StateTerminalFailure
}

// ClassifyTask maps a task's granular Status onto the abstract
// lifecycle state. nil → Unknown.
//
// Mapping (issue #101):
//
//	todo        → Prerun
//	ready       → Prerun
//	in-progress → Active
//	in-review   → Active
//	blocked     → Waiting
//	done        → TerminalSuccess
//	abandoned   → TerminalFailure
//
// "ready" is Prerun rather than Active because the worker hasn't been
// dispatched yet — the task is queued, not running. Coord's dispatch
// loop is what flips it to in-progress (Active).
func ClassifyTask(t *tasks.Task) State {
	if t == nil {
		return StateUnknown
	}
	switch t.Status {
	case tasks.StatusTodo, tasks.StatusReady:
		return StatePrerun
	case tasks.StatusInProgress, tasks.StatusInReview:
		return StateActive
	case tasks.StatusBlocked:
		return StateWaiting
	case tasks.StatusDone:
		return StateTerminalSuccess
	case tasks.StatusAbandoned:
		return StateTerminalFailure
	}
	return StateUnknown
}

// ClassifyWorker maps a worker's granular Phase onto the abstract
// lifecycle state. nil → Unknown.
//
// Mapping (issue #101):
//
//	starting   → Prerun  (state.json bootstrap; worker hasn't begun)
//	branch     → Prerun  (cutting the worker branch; pre-TDD)
//	tdd-*      → Active
//	review-*   → Active
//	push       → Active
//	done       → TerminalSuccess
//	blocked    → Waiting
//	failed     → TerminalFailure
//
// Critical: blocked is Waiting (not Terminal). Operator may unblock the
// worker, at which point the same worker dir resumes Active. Deleting
// it here would orphan an in-flight conversation.
func ClassifyWorker(w *workers.State) State {
	if w == nil {
		return StateUnknown
	}
	switch w.Phase {
	case workers.PhaseStarting, workers.PhaseBranch:
		return StatePrerun
	case workers.PhaseTDDRed, workers.PhaseTDDGreen, workers.PhaseTDDRefactor,
		workers.PhaseReviewClaude, workers.PhaseReviewCodex, workers.PhasePush:
		return StateActive
	case workers.PhaseDone:
		return StateTerminalSuccess
	case workers.PhaseBlocked:
		return StateWaiting
	case workers.PhaseFailed:
		return StateTerminalFailure
	}
	return StateUnknown
}

// ClassifyAgent maps an agent record's flags onto the abstract
// lifecycle state. nil → Unknown.
//
// Agent records don't carry an explicit Status enum; we infer:
//
//	NeedsInput=true OR Blocked=true → Waiting (asking / blocked)
//	otherwise                        → Active
//
// Stale-past-window agents (live record but no recent activity) are
// detected upstream by the dashboard's freshness gates — this function
// does NOT consult time.Now() or LastActivityTS so it's pure +
// deterministic for tests. Callers wanting a "stale → TerminalFailure"
// signal should layer that decision on top.
//
// Coord agents have no v0.5 success state in the design — the coord
// skill's 4h auto-idle-stop transition writes a fresh "stopped" record;
// it doesn't go through OnTerminal here.
func ClassifyAgent(r *agent.Record) State {
	if r == nil {
		return StateUnknown
	}
	if r.NeedsInput || r.Blocked {
		return StateWaiting
	}
	return StateActive
}

// OnTerminal runs the per-entity cleanup for a terminal-state entity.
// Returns nil for non-terminal inputs (caller's predicate is buggy but
// we don't punish them with an error — let the operator-visible state
// of tasks.md / workers/<slug>/ be the source of truth).
//
// Currently dispatches on the concrete entity type:
//
//	*tasks.Task        → no-op (issue #101: tasks stay in tasks.md;
//	                      TUI history grouping is a render-side concern)
//	*workers.State     → workers.Delete(project, slug)
//	*agent.Record      → no-op (coord skill 4h auto-idle-stop owns this)
//
// project is required for *workers.State. Empty project + worker is
// rejected — the cleanup target lives at
// ~/.fleet/projects/<project>/workers/<slug>/, and an empty project
// would mean "delete the wrong thing"; refuse the call so the bug
// surfaces at the call site rather than as a silent miss.
//
// Idempotency: workers.Delete is safe on ENOENT, so a second OnTerminal
// for the same slug returns nil. This matches the design's "first mover
// wins; second sees ENOENT" contract for the dual-trigger TUI/coord
// path.
func OnTerminal(entity any, project string) error {
	switch e := entity.(type) {
	case *tasks.Task:
		// Tasks: history is a TUI render decision — NO disk change.
		// Returning nil keeps OnTerminal callable uniformly without
		// special-casing "is this a task" at every call site.
		return nil
	case *workers.State:
		if e == nil {
			return nil
		}
		if !ClassifyWorker(e).IsTerminal() {
			return nil
		}
		if project == "" {
			return fmt.Errorf("lifecycle.OnTerminal worker: empty project (slug=%q)", e.Slug)
		}
		if e.Slug == "" {
			return fmt.Errorf("lifecycle.OnTerminal worker: empty slug")
		}
		return workers.Delete(project, e.Slug)
	case *agent.Record:
		// Agent records: untouched. The coord skill's 4h auto-idle-stop
		// is the authoritative writer; cross-package mutation here
		// would race with that path.
		return nil
	}
	return fmt.Errorf("lifecycle.OnTerminal: unsupported entity type %T", entity)
}
