// Package dispatch is the single dispatch-lifecycle primitive for Fleet.
//
// See docs/DESIGN-dispatch-lifecycle.md (v9, approved 2026-05-15) for the
// full design. PR1 ships the minimal vertical slice that proves the
// primitive end-to-end on `coord_prompt_inbox` Delivery claims — the
// single resource kind responsible for today's 30-file inbox leak (see
// docs/postmortems/2026-05-14-orphan-tmux-leak.md for the prior parallel
// postmortem; the inbox-leak postmortem itself is captured in
// DESIGN-dispatch-lifecycle.md §"Motivation").
//
// PR2/PR3/PR4 expand the package to cover the other 8 resource kinds
// across the 5 semantic classes (Exclusive, Adoptable, Derived,
// Delivery, Audit) and the `Replace` operation that supersedes
// `internal/handoffop/atomic_coord_swap.go`.
//
// Load-bearing invariants (from DESIGN-dispatch-lifecycle.md §TL;DR):
//
//   - DispatchID == agent_id (8-hex; same shape as
//     `skills/coordinator/dispatch.py::mint_agent_id`). Promoted to a
//     named type in PR1 with a constructor test.
//   - state == terminal ⇒ all claims released (per-class semantics).
//   - Single store: claims live INLINE in ~/.fleet/dispatches/<id>.json.
//     No separate claims directory. Atomic tmp+rename writes via
//     state.WriteAtomic; updates rewrite the whole journal.
//   - host_id + tmux_socket on every claim that needs same-host
//     discrimination (PR2 adds these; PR1 carries the fields for
//     forward-compatibility).
package dispatch

import (
	"encoding/json"
	"fmt"
	"regexp"
	"time"
)

// SchemaVersion is bumped when the on-disk journal shape changes
// incompatibly. PR1 ships v1.
const SchemaVersion = "v1"

// ExecState is the dispatch's execution lifecycle stage.
//
// Dispatch-durability (fleet#184) reshaped the live launch path:
//
//	pending → launch_attempted → acked → { done | blocked | failed }
//
// in_flight is RETIRED from the live path (acquire no longer sets it)
// but is still PARSED as a legacy alias for on-disk entries written by
// the pre-#184 binary — see ExecInFlight's doc.
type ExecState string

const (
	ExecPending ExecState = "pending"
	// ExecInFlight is RETIRED from the live path as of fleet#184.
	// Pre-#184 acquire flipped a fresh journal to in_flight at
	// inbox-write time, which was premature (the coord had not yet
	// launched the Agent). Acquire now leaves the entry at ExecPending
	// and it is the COORD that durably flips pending → launch_attempted
	// immediately before invoking the Agent tool. The constant stays so
	// old on-disk journals parse without error; nothing in the live
	// path WRITES it anymore. Treated as non-terminal (same as before).
	ExecInFlight ExecState = "in_flight"
	// ExecLaunchAttempted is set by the coord (via `fleet claims
	// mark-launch-attempted`) under the per-journal flock, IMMEDIATELY
	// before it invokes the host Agent tool. This is the durable
	// "launch was attempted" record that makes replay safe: replay
	// re-emits ONLY ExecPending entries (coord never flipped them ⇒
	// never launched), and NEVER ExecLaunchAttempted (the double-launch
	// trap — a launched-but-unacked worker frequently has no ack because
	// register_subagent is best-effort).
	ExecLaunchAttempted ExecState = "launch_attempted"
	// ExecAcked is set when register_subagent succeeds (best-effort; may
	// be skipped on lock contention). A live, acked worker is left
	// alone by replay.
	ExecAcked   ExecState = "acked"
	ExecDone    ExecState = "done"
	ExecBlocked ExecState = "blocked"
	ExecFailed  ExecState = "failed"
)

// IsTerminal reports whether this ExecState is one of the terminal
// outcomes (done | blocked | failed). The reclamation policy fires on
// the terminal-transition boundary: each owned claim's controller
// `Release` is called.
func (s ExecState) IsTerminal() bool {
	switch s {
	case ExecDone, ExecBlocked, ExecFailed:
		return true
	}
	return false
}

// ReclState is the dispatch's reclamation lifecycle stage. Driven by
// per-claim Release results aggregated across all owned claims.
//
//	pending → { complete | partial | blocked }
type ReclState string

const (
	ReclPending  ReclState = "pending"
	ReclComplete ReclState = "complete"
	ReclPartial  ReclState = "partial"
	ReclBlocked  ReclState = "blocked"
)

// ClaimState is the per-claim transition state.
//
//	allocating → live → releasing → released
//	             ↓
//	        (failed_alloc)
type ClaimState string

const (
	ClaimAllocating  ClaimState = "allocating"
	ClaimLive        ClaimState = "live"
	ClaimReleasing   ClaimState = "releasing"
	ClaimReleased    ClaimState = "released"
	ClaimFailedAlloc ClaimState = "failed_alloc"
)

// Claim semantic classes (5). PR1 only exercises ClassDelivery via the
// coord_prompt_inbox kind; the other class constants are declared here
// so PR2+ can extend the journal schema without bumping SchemaVersion.
const (
	ClassExclusive = "exclusive"
	ClassAdoptable = "adoptable"
	ClassDerived   = "derived"
	ClassDelivery  = "delivery"
	ClassAudit     = "audit"
)

// Resource kinds. PR1 only ships `coord_prompt_inbox`. The remaining
// kinds are declared here so journal readers in PR1 don't choke on
// future inline claim entries written by a forward-compatible PR2
// binary running against an upgrade-in-progress filesystem.
const (
	KindCoordPromptInbox   = "coord_prompt_inbox"
	KindHandoffResumeInbox = "handoff_resume_inbox"   // PR2
	KindRemoteControlInbox = "remote_control_inbox"   // PR2
	KindTmuxSession        = "tmux_session"           // PR2
	KindAgentRecord        = "agent_record"           // PR2
	KindCoordSpawnMarker   = "coord_spawn_marker"     // PR3
	KindWorkerDir          = "worker_dir"             // PR2
	KindWorktree           = "worktree"               // PR2
	KindSupervisorEntry    = "supervisor_entry"       // PR4 (derived)
	KindWorkerAgentIDs     = "worker_agent_ids_entry" // PR4 (derived)
)

// DispatchID wraps an 8-hex agent identifier as a named type.
//
// plan-eng-review A6 (2026-05-15): the design's load-bearing invariant
// is `DispatchID == agent_id`. Promoting to a named type here lets the
// Go side type-check away "any-string-shaped" callers, while a
// regression test pins the wire shape byte-equal with the Python
// minter (skills/coordinator/dispatch.py:mint_agent_id, which calls
// `secrets.token_hex(4)`). The shape is "exactly 8 lowercase hex chars"
// — same as `agent.NewID()` in internal/agent/agent.go.
type DispatchID string

// dispatchIDRe pins the wire shape: exactly 8 lowercase hex chars.
// Mirrors `_AGENT_ID_FULL_RE` in skills/coordinator/dispatch.py.
var dispatchIDRe = regexp.MustCompile(`^[0-9a-f]{8}$`)

// NewDispatchID validates and wraps an 8-hex string as a DispatchID.
//
// Rejects anything that isn't exactly 8 lowercase hex chars — this is
// the same constraint the Python skill enforces on `agent_id` before
// it goes anywhere near `format_dispatch_instruction` or the inbox
// writer. Callers that produce IDs in Go should generate via
// `internal/agent.NewID()` and pass the result here.
func NewDispatchID(s string) (DispatchID, error) {
	if !dispatchIDRe.MatchString(s) {
		return "", fmt.Errorf("invalid dispatch_id %q: expected 8 lowercase hex chars", s)
	}
	return DispatchID(s), nil
}

// String returns the raw 8-hex form. Provided so callers don't have to
// type-cast back to string for fmt/path joins.
func (d DispatchID) String() string { return string(d) }

// Journal is the per-dispatch execution + claim record.
//
// One file per dispatch at ~/.fleet/dispatches/<id>.json. All claim
// data lives INLINE in Claims — codex round 2 collapsed v2's two-store
// design into one. Updates are atomic tmp+rename via
// state.WriteAtomic. The whole file rewrites on every claim transition
// (cost: a few KB; benefit: no split-brain).
//
// ASCII diagram of the per-claim lifecycle inside this journal:
//
//	Append(allocating) -> tmp+rename
//	      |
//	      v
//	   closure (create resource)
//	      |
//	   success?
//	   /     \
//	 no       yes
//	  |        |
//	  v        v
//	 failed   flip(live) -> tmp+rename
//	 _alloc      |
//	             v
//	          terminal exec_state -> Release()
//	             |
//	             v
//	          flip(releasing) -> teardown -> flip(released)
type Journal struct {
	ID            DispatchID    `json:"id"`
	Kind          string        `json:"kind"`  // "worker" | "reviewer" | "finisher" | "coord" | "fix" | "rebase"
	Owner         string        `json:"owner"` // "project/<p>/slug/<s>" or "coord/<p>"
	HostID        string        `json:"host_id"`
	TmuxSocket    string        `json:"tmux_socket,omitempty"`
	SchemaVersion string        `json:"schema"`
	CreatedAt     time.Time     `json:"created_at"`
	UpdatedAt     time.Time     `json:"updated_at"`
	ExecState     ExecState     `json:"exec_state"`
	ReclState     ReclState     `json:"recl_state"`
	BlockedReason string        `json:"blocked_reason,omitempty"`
	Claims        []ClaimInline `json:"claims"`

	// --- dispatch-durability (fleet#184) ---

	// Generation is bumped under the per-journal flock on every
	// lifecycle reset (acquire of a fresh journal starts at 0;
	// reset-for-relaunch bumps it). It is an audit/version field AND
	// the launch token: a DISPATCH block carries the generation it was
	// emitted for, and `mark-launch-attempted <id> <gen>` flips
	// pending → launch_attempted ONLY if the on-disk generation still
	// equals the token. A stale re-emitted block (from a tick that read
	// an older lifecycle) carries an old gen → predicate-fail → cannot
	// consume a later lifecycle's launch. NOT the concurrency primitive
	// (the flock is) — see store.go withJournalLock.
	Generation int `json:"generation"`

	// LaunchAttemptedAt records when the coord durably flipped
	// pending → launch_attempted. Residual-crash repair fires on
	// (launch_attempted && !acked && no-live-subagent && now-this >
	// LAUNCH_ACK_GRACE) → ExecBlocked + off-channel escalation.
	LaunchAttemptedAt time.Time `json:"launch_attempted_at,omitempty"`

	// ReplayEmitAttempts counts RESERVED replay emissions (not
	// deliveries). reserve-replay increments it under the flock BEFORE
	// the block is added to tick output, so a broken-pipe print still
	// advances the cap — no infinite re-emit across coord restarts. The
	// cap is total-per-dispatch, not per-coord-process.
	ReplayEmitAttempts int `json:"replay_emit_attempts,omitempty"`

	// LastReplayAt / LastReplayError are debug breadcrumbs for the
	// replay path (last reserved emission + last recorded error).
	LastReplayAt    time.Time `json:"last_replay_at,omitempty"`
	LastReplayError string    `json:"last_replay_error,omitempty"`
}

// ClaimInline is the on-disk envelope for any claim, regardless of
// semantic class. The Data blob is class-specific JSON (DeliveryClaim
// in PR1; ExclusiveClaim / AdoptableClaim in PR2). Readers that don't
// recognize a Class+Kind tuple MUST tolerate it (forward-compat: PR1
// reader sees a PR2-written tmux_session claim inline and ignores it
// rather than failing the whole journal parse).
type ClaimInline struct {
	Class string          `json:"class"`
	Kind  string          `json:"kind"`
	State ClaimState      `json:"state"`
	Data  json.RawMessage `json:"data"`
}

// newJournal builds an empty journal for a fresh dispatch with the
// canonical default values.
func newJournal(id DispatchID, kind, owner, hostID, tmuxSocket string, now time.Time) *Journal {
	return &Journal{
		ID:            id,
		Kind:          kind,
		Owner:         owner,
		HostID:        hostID,
		TmuxSocket:    tmuxSocket,
		SchemaVersion: SchemaVersion,
		CreatedAt:     now.UTC(),
		UpdatedAt:     now.UTC(),
		ExecState:     ExecPending,
		ReclState:     ReclPending,
		Claims:        []ClaimInline{},
	}
}

// findClaim returns the first inline claim matching kind, or -1.
// Convenience helper; callers iterate when they need multi-match.
func (j *Journal) findClaim(kind string) int {
	for i, c := range j.Claims {
		if c.Kind == kind {
			return i
		}
	}
	return -1
}
