// Package agent owns the agent record (~/.fleet/agents/<id>.json).
//
// Schema mirrors docs/STATE.md "agents/<id>.json" canonical shape.
// Writes go through state.WriteAtomic so readers (TUI, CLI, fsnotify)
// never see torn JSON.
package agent

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/edisonshen/fleet/internal/state"
)

// ErrChainCycle is returned by ResolveChain when the handoff chain
// loops back on itself (A.successor=B, B.successor=A). Callers
// surface this so the operator knows the predecessor archives are
// corrupt — re-spawn manually rather than walk forever.
var ErrChainCycle = errors.New("handoff chain cycle detected")

// ErrNoLiveSuccessor is returned by ResolveChain when the chain
// terminates without a live tail — either the chain reached an
// archived record with non-handoff cause (kill/exit/gc) or the next
// hop's id is missing from both live + archive. Error message
// includes the cause (or "missing") so the operator sees why.
var ErrNoLiveSuccessor = errors.New("no live successor")

// SchemaVersion is bumped when Record's on-disk shape changes
// incompatibly. Readers compare and refuse newer versions.
//
// v2 adds the handoff-chain pointer fields (SuccessorID,
// PredecessorID, ArchivedCause) so `fleet attach <predecessor>` can
// walk to the live successor instead of dead-ending. Older v1 records
// load cleanly — the new fields default to empty strings and the
// ResolveChain walker treats absent successor as "no chain to walk."
const SchemaVersion = 2

// ArchivedCause* are the enumerated values for Record.ArchivedCause.
// Only "handoff" makes ResolveChain attempt a successor walk; other
// causes are terminal (the resolver surfaces a clear stderr message).
const (
	ArchivedCauseHandoff = "handoff"
	ArchivedCauseExit    = "exit"
	ArchivedCauseKill    = "kill"
	ArchivedCauseGC      = "gc"
)

// DefaultEngine is the agent runtime spawned for new dispatches.
// v1 only writes "claude-code"; v1.1 adds "codex" (or similar)
// without a schema migration. See docs/DECISIONS.md 2026-04-26
// "v1.1 engine adapter — minimal v1 hooks".
const DefaultEngine = "claude-code"

// Record matches docs/STATE.md "agents/<id>.json" canonical schema.
//
// One writer per file (fleet-guard in the agent's own process post-
// Week-4; the fleet binary today). Readers tolerate missing optional
// fields by treating them as null — never crash on absence.
type Record struct {
	SchemaVersion  int        `json:"schema_version"`
	ID             string     `json:"id"`
	PID            int        `json:"pid"`
	TmuxSession    string     `json:"tmux_session"`
	Engine         string     `json:"engine"`
	Role           string     `json:"role"`           // "executor" | "planner"
	Mode           string     `json:"mode,omitempty"` // "execute" | "plan" | "fix" | "review"
	TaskID         string     `json:"task_id,omitempty"`
	Project        string     `json:"project,omitempty"`
	ReviewRound    *int       `json:"review_round"`
	ContextPct     *float64   `json:"context_pct"`
	ContextSource  string     `json:"context_source,omitempty"` // "hook" | "proxy" (per DESIGN.md)
	LastActivityTS time.Time  `json:"last_activity_ts"`
	Blocked        bool       `json:"blocked"`
	BlockedReason  *string    `json:"blocked_reason"`
	BlockedSince   *time.Time `json:"blocked_since"`
	NeedsInput     bool       `json:"needs_input"`
	// HasPendingQuestion distinguishes "agent stopped on a question for
	// the operator" (asking) from "agent stopped, work done, nothing
	// pending" (idle). Both have NeedsInput=true; the heuristic that
	// flips this lives in skills/fleet-guard/health.py:detect_question
	// (tail-of-pane: trailing "?", "Do you" / "Should I" openers,
	// "[y/n]"). False on legacy records and whenever NeedsInput is
	// false. The TUI uses this to render two different statuses with
	// distinct glyph + color so the operator can ignore idle agents
	// at a glance and only attend to "asking" ones.
	HasPendingQuestion bool    `json:"has_pending_question,omitempty"`
	InboxPending       bool    `json:"inbox_pending"`
	HandoffType        *string `json:"handoff_type"`
	// HandoffTypeAt timestamps when HandoffType was last set (RFC 3339,
	// written by skills/fleet-guard/health.py:now_rfc3339). The
	// stuck-pending watchdog (handoff.py:_yellow_stuck_too_long) reads
	// this to decide whether to re-inject HANDOFF REQUESTED when Yellow
	// has lingered without a MILESTONE — the prior injection may have
	// been lost (pre-v0.1.1 stdout-only Stop-hook output, crashed pane,
	// etc.). nil for legacy records and whenever HandoffType is nil;
	// the watchdog treats nil as "re-inject on the next Stop" so legacy
	// stuck agents migrate in one fire.
	//
	// Stored as *string (not *time.Time) so a malformed value on disk —
	// operator hand-edit, partial write, future schema drift — degrades
	// to "watchdog re-injects" rather than failing json.Unmarshal of the
	// whole Record and bricking every Go reader (fleet attach / handoff
	// / rm / drain). The Python watchdog already designs for graceful
	// degradation here; matching that on the Go side avoids the wedge
	// where one side keeps moving and the other can't read the record
	// (codex review for stuck-yellow-watchdog: P2).
	HandoffTypeAt *string `json:"handoff_type_at,omitempty"`
	// LastHandoffPath points at the handoff doc this agent inherited
	// from. nil for the first agent on a task. Read by the next handoff
	// to populate the new doc's previous_handoff frontmatter, building
	// the chain Fleet shows in TUI task-detail views (DESIGN.md §"Handoff
	// Chain"). One writer per file, so this is updated only at spawn.
	LastHandoffPath *string `json:"last_handoff_path"`
	// HandoffNumber starts at 1 for the first agent on a task and
	// increments by 1 per handoff. Used as previous_handoff doc's
	// handoff_number when this agent eventually hands off.
	HandoffNumber int `json:"handoff_number"`
	// Cwd is the absolute working directory the agent was spawned in.
	// Captured at dispatch (defaulting to os.Getwd() when --cwd is
	// empty) so `fleet handoff` from a different shell can place the
	// replacement in the same project checkout. Empty for legacy
	// records — handoff falls back to its --cwd flag in that case.
	Cwd string `json:"cwd,omitempty"`
	// Command is the argv used to spawn the agent process inside
	// tmux. Captured at dispatch so `fleet handoff` preserves any
	// custom engine/wrapper the operator chose (e.g., a wrapped
	// claude binary). Empty for legacy records — handoff falls back
	// to its --command flag.
	Command []string `json:"command,omitempty"`
	// DisableAutoResume opts the agent out of fleet's handoff
	// auto-resume — the resume prompt typed into a freshly spawned
	// replacement after handoff. Default false (auto-resume ON) for
	// claude code's natural-language UI; operators set true via
	// `fleet dispatch --no-auto-resume` for custom wrappers (shells,
	// REPLs, vim, alternate engines) where typing
	// "Read your handoff doc..." would execute as garbage input.
	// Inherited unchanged across handoffs (codex review iter-7 P2).
	DisableAutoResume bool      `json:"disable_auto_resume,omitempty"`
	SpawnedAt         time.Time `json:"spawned_at"`

	// PredecessorID, SuccessorID, ArchivedCause are v2 schema fields
	// that turn `fleet attach <token>` into a chain-following resolver:
	// `ResolveChain` walks predecessor → successor pointers across
	// archived records so an operator holding a stale id lands in the
	// live tail's tmux session instead of dead-ending with "no agent."
	//
	// Write semantics (set by cmd/fleet/handoff.go via ArchiveWithHandoff
	// at archive time, and by internal/spawn at spawn time):
	//   - PredecessorID: set on the LIVE successor record when it is
	//     spawned from a handoff. Empty for fresh dispatches.
	//   - SuccessorID:   set on the ARCHIVED predecessor record after
	//     the swap commits. Empty when archived for non-handoff causes
	//     (kill / exit / gc) and on legacy v1 archives.
	//   - ArchivedCause: set only at archive time. Live records carry
	//     "" here. Empty on a v1 archive (legacy / pre-v2) means the
	//     resolver treats the chain as terminal at that hop and falls
	//     into the dead-chain branch.
	//
	// Backfill is NOT performed for pre-v2 archives — only new handoffs
	// from this binary forward carry the pointer. Old chains dead-end
	// at the first pre-v2 archive (acceptable: the operator gets a
	// clean diagnostic from the resolver instead of a silent loop).
	PredecessorID string `json:"predecessor_id,omitempty"`
	SuccessorID   string `json:"successor_id,omitempty"`
	ArchivedCause string `json:"archived_cause,omitempty"`

	// SupervisorPID, SupervisorPidStart, SupervisorExePath identify the
	// `fleet coord-run` SUPERVISOR process that holds + heartbeats the
	// coordinator lease for this coord's lifetime (DESIGN-handoff-drain-
	// storm-leak §3(B), §B.5). They are DISTINCT from PID, which is the
	// engine (claude) process resolved via the tmux pane child tree:
	//
	//	PID            -> engine child (claude)   — liveness probes, TUI
	//	SupervisorPID  -> fleet coord-run parent  — the lease HOLDER
	//
	// The authenticated STONITH primitive (internal/coord.KillCoordIf-
	// IdentityMatches) targets SupervisorPID — killing the supervisor is
	// what releases coordinator.flock via kernel-on-death. It re-validates
	// SupervisorPidStart (defeats PID reuse) and SupervisorExePath (must
	// be the fleet coord-run binary, never the engine child) immediately
	// before signaling.
	//
	// Stamped by `fleet coord-run` at supervisor startup (the supervisor
	// is the only process that knows its own identity). Empty on:
	//   - records spawned with FLEET_LEASE_FAILOVER off (coord-run not
	//     wired) — the kill primitive refuses (no supervisor to target),
	//   - legacy/worker records — never coords, never killed by STONITH.
	// All three behind FLEET_LEASE_FAILOVER; omitempty keeps off-flag
	// records byte-identical to today.
	SupervisorPID      int    `json:"supervisor_pid,omitempty"`
	SupervisorPidStart int64  `json:"supervisor_pid_start,omitempty"`
	SupervisorExePath  string `json:"supervisor_exe_path,omitempty"`
}

// NewID generates a short hex agent identifier (8 chars from 4 random
// bytes). Collision probability is negligible at v1's 1-20 concurrent
// agent ceiling. Examples: "a1b2c3d4", "7f3a92e1".
//
// We do NOT use a sequential counter (a1, a2, ...) because that would
// require a per-machine state file and a flock to serialize allocation.
// Random hex sidesteps that entirely. The TUI can still display short
// labels by truncating to the first 4 chars if desired.
func NewID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is exceptional; fall back to time-based
		// to keep dispatch from crashing on a transient kernel hiccup.
		return fmt.Sprintf("t%07x", time.Now().UnixNano()&0xffffffff)
	}
	return hex.EncodeToString(b[:])
}

// New builds a Record with required defaults filled in. Caller sets
// task-specific fields before Write.
//
// HandoffNumber defaults to 1 — this agent is the first on its task.
// Spawn-from-handoff (internal/spawn) overrides this with old+1.
func New(id string) *Record {
	now := time.Now().UTC()
	return &Record{
		SchemaVersion:  SchemaVersion,
		ID:             id,
		Engine:         DefaultEngine,
		Role:           "executor",
		Mode:           "execute",
		HandoffNumber:  1,
		LastActivityTS: now,
		SpawnedAt:      now,
	}
}

// StampSupervisorIdentity load-modify-writes ONLY the agent record's
// three supervisor identity fields (the coord-run lease holder). Called
// by `fleet coord-run` at startup — the supervisor is the only process
// that knows its own pid/start/exe.
//
// CONCURRENCY (codex PR2 iter-8 [P2]): this races spawn.Spawn's final
// record write (which sets the engine PID). Both sides take the per-agent
// lock (state.LockAgent) and do a LOCKED load-modify-write that touches
// only THEIR fields, so neither writer can clobber the other's:
//   - this function preserves the engine PID + every non-supervisor field
//     by re-reading under the lock and overlaying only the 3 supervisor
//     fields,
//   - spawn re-reads + preserves the supervisor fields under the same
//     lock before its final write.
//
// A missing record returns state.ErrNotFound for the caller to retry/
// surface (the supervisor may start before spawn's record write lands;
// coord-run retries — see stampSupervisorWithRetry). Off-flag coords
// never call this, so off-flag records keep the fields empty (omitempty).
func StampSupervisorIdentity(id string, pid int, pidStart int64, exePath string) error {
	unlock, err := state.LockAgent(id)
	if err != nil {
		return fmt.Errorf("lock agent %s for supervisor stamp: %w", id, err)
	}
	defer unlock()

	rec, err := Load(id)
	if err != nil {
		return err
	}
	rec.SupervisorPID = pid
	rec.SupervisorPidStart = pidStart
	rec.SupervisorExePath = exePath
	return rec.Write()
}

// Write atomically publishes the record to ~/.fleet/agents/<id>.json.
func (r *Record) Write() error {
	path, err := state.AgentPath(r.ID)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal agent record: %w", err)
	}
	// Pretty-printed JSON ends without a trailing newline; add one
	// so cat/grep behave nicely.
	data = append(data, '\n')
	return state.WriteAtomic(path, data)
}

// Load reads an agent record by ID from disk.
//
// Backfills HandoffNumber=0 to 1 on read. Records written before the
// chain-fields PR (Week 4a) lack the handoff_number field;
// json.Unmarshal leaves it as the int zero value. Treating that zero
// as "first agent on task" preserves the chain semantics for the
// first post-upgrade handoff (otherwise the new doc gets
// handoff_number=0 and the next agent starts at 1, repeating the
// number — broken chain).
func Load(id string) (*Record, error) {
	path, err := state.AgentPath(id)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("agent %s: %w", id, state.ErrNotFound)
		}
		return nil, fmt.Errorf("read agent %s: %w", id, err)
	}
	var r Record
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("parse agent %s: %w", id, err)
	}
	if r.HandoffNumber == 0 {
		r.HandoffNumber = 1
	}
	return &r, nil
}

// Archive moves the agent's live record file to ~/.fleet/agents/archive/.
//
// Used after a handoff (record's owner agent has been replaced) or a
// crash (record outlives the tmux session). After Archive, the live
// agents/ scan in List() no longer returns this record. Atomic via
// rename(2) on same-filesystem (always true for ~/.fleet/).
//
// If agents/archive/<id>.json already exists (from an old archived
// agent that happened to share the 8-hex-char ID via the birthday
// paradox over a long-lived install), append a UTC stamp to keep
// both archives. Loses the old archive only if BOTH the bare path
// and the stamped path are taken — vanishingly unlikely.
//
// Returns ErrNotFound if the live file is missing.
func (r *Record) Archive() error {
	src, err := state.AgentPath(r.ID)
	if err != nil {
		return err
	}
	if _, err := os.Stat(src); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("archive %s: live record %w", r.ID, state.ErrNotFound)
		}
		return fmt.Errorf("stat live record %s: %w", r.ID, err)
	}
	dst, err := state.AgentArchivePath(r.ID)
	if err != nil {
		return err
	}
	if _, err := os.Stat(dst); err == nil {
		// Bare archive path collides — append a UTC suffix to keep
		// both copies. Format: <id>-<UTCYYYYMMDD-HHMMSS>.json.
		suffixed, derr := state.AgentArchivePath(r.ID + "-" + time.Now().UTC().Format("20060102-150405"))
		if derr != nil {
			return derr
		}
		dst = suffixed
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat archive path: %w", err)
	}
	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("rename %s -> archive: %w", r.ID, err)
	}
	return nil
}

// List returns every live agent record under ~/.fleet/agents/.
// Archived records (under agents/archive/) are not included.
// Records that fail to parse are silently skipped — partial results
// beat zero results when the operator is trying to triage. Callers
// that need a strict "everything parses or we abort" guarantee
// (split-brain veto in dispatch) should use ListStrict.
func List() ([]*Record, error) {
	good, _, err := listInternal()
	return good, err
}

// ListStrict is List that also returns the IDs of records that
// failed to parse. Used by the dispatch --coord-spawn split-brain
// veto (codex review iter-17 P1): the silent-skip behavior is unsafe
// when we're deciding whether a live coord exists, because the
// unparseable record might be the live coord's. The caller must fail
// closed when len(badIDs) > 0.
//
// Other callers (fleet status, TUI dashboard) keep using List() to
// preserve the triage UX.
func ListStrict() (good []*Record, badIDs []string, err error) {
	return listInternal()
}

// listInternal does the actual directory walk. Shared between List
// and ListStrict so the silent-skip behavior is in one place.
func listInternal() (good []*Record, badIDs []string, err error) {
	dir, err := state.AgentDir()
	if err != nil {
		return nil, nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("readdir agents: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		id := e.Name()[:len(e.Name())-len(".json")]
		r, lerr := Load(id)
		if lerr != nil {
			badIDs = append(badIDs, id)
			continue
		}
		good = append(good, r)
	}
	return good, badIDs, nil
}

// LoadArchive reads an archived agent record from
// ~/.fleet/agents/archive/<id>.json. Used by ResolveChain to walk
// handoff pointers backwards through history.
//
// Only the bare <id>.json file is read; collision-stamped archives
// (<id>-<utc>.json from Record.Archive's fallback path) are NOT
// consulted because the chain pointer always references the bare
// id and the stamp is reserved for the genuine birthday-paradox
// case where a re-used id collides — too rare in practice to
// support both lookup paths.
//
// Returns state.ErrNotFound (wrapped) when the file is absent so
// callers can distinguish "no archive" from "read error."
func LoadArchive(id string) (*Record, error) {
	path, err := state.AgentArchivePath(id)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("agent archive %s: %w", id, state.ErrNotFound)
		}
		return nil, fmt.Errorf("read agent archive %s: %w", id, err)
	}
	var r Record
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("parse agent archive %s: %w", id, err)
	}
	// Mirror Load's HandoffNumber backfill — same legacy v1 records
	// could surface through this read path post-archive.
	if r.HandoffNumber == 0 {
		r.HandoffNumber = 1
	}
	return &r, nil
}

// ResolveChain finds the live tail of a handoff chain starting at
// token. Walks the predecessor → successor pointer chain across
// archived records:
//
//	live(token)              → return (rec, 0, nil) — direct hit
//	archive(token) succ=B    → recurse with depth+1, cycle guard
//	archive(token) no chain  → ErrNoLiveSuccessor with cause embedded
//	missing everywhere       → state.ErrNotFound wrapper
//
// hops is 0 for a direct live hit and N for "had to traverse N
// archived predecessors before landing on the live tail." Callers
// emit "rotated through N hop(s)" to stderr so the operator sees
// the resolution path.
//
// Cycle guard: a visited-set blocks the resolver from following
// A.succ=B, B.succ=A back to A. Returns ErrChainCycle on detection.
// (The dispatch task brief calls this out explicitly; corrupt
// archives or operator-edited fields could produce one.)
//
// Dead-chain handling: when an archived record has either no
// successor_id, or a non-handoff archived_cause, or its named
// successor exists nowhere, the resolver returns ErrNoLiveSuccessor
// wrapped with the visible cause string ("cause=kill",
// "cause=handoff successor missing", etc.) so the cmd/fleet/attach.go
// error message tells the operator what happened.
func ResolveChain(token string) (*Record, int, error) {
	visited := map[string]bool{}
	return resolveChainStep(token, 0, visited)
}

func resolveChainStep(token string, depth int, visited map[string]bool) (*Record, int, error) {
	if visited[token] {
		return nil, depth, fmt.Errorf("%w: %s revisited at depth %d", ErrChainCycle, token, depth)
	}
	visited[token] = true

	// Tier 1: live record wins immediately.
	rec, err := Load(token)
	if err == nil {
		return rec, depth, nil
	}
	if !errors.Is(err, state.ErrNotFound) {
		return nil, depth, err
	}

	// Tier 2: try archive. If neither, unknown token.
	arec, aerr := LoadArchive(token)
	if aerr != nil {
		if errors.Is(aerr, state.ErrNotFound) {
			// Token unknown — caller wraps with a friendly message.
			return nil, depth, fmt.Errorf("agent %s: %w (no live or archived record)",
				token, state.ErrNotFound)
		}
		return nil, depth, aerr
	}

	// Chain follow only when archived as handoff with a named successor.
	if arec.ArchivedCause != ArchivedCauseHandoff || arec.SuccessorID == "" {
		cause := arec.ArchivedCause
		if cause == "" {
			cause = "unknown"
		}
		return nil, depth, fmt.Errorf(
			"%w for %s: archived (cause=%s)", ErrNoLiveSuccessor, token, cause)
	}

	// Recurse on the named successor. Depth bumped one per hop.
	tail, total, terr := resolveChainStep(arec.SuccessorID, depth+1, visited)
	if terr != nil {
		// Re-wrap broken-mid-chain as ErrNoLiveSuccessor with a
		// pointer-trace so the operator can see where the chain
		// broke (e.g. cleaned-up successor archive). Leave cycle
		// + other sentinel errors alone — they carry their own
		// meaning.
		if errors.Is(terr, state.ErrNotFound) {
			return nil, total, fmt.Errorf(
				"%w for %s: chain broken at %s (cause=handoff successor missing)",
				ErrNoLiveSuccessor, token, arec.SuccessorID)
		}
		return nil, total, terr
	}
	return tail, total, nil
}

// ArchiveWithHandoff archives a record AND stamps the handoff chain
// pointer atomically. The atomicity is achieved by writing the
// updated archive payload directly to the destination path via
// state.WriteAtomic (.tmp + rename), then removing the live file in
// the same call. If the rename succeeds but the live remove fails,
// the resolver tolerates the resulting "in both live and archive"
// state as live-wins (Load returns first).
//
// successorID is required (caller is the handoff path); empty is a
// caller bug.
func (r *Record) ArchiveWithHandoff(successorID string) error {
	if successorID == "" {
		return fmt.Errorf("ArchiveWithHandoff: successorID required")
	}
	// Mutate the in-memory record so the on-disk archive carries the
	// chain fields. Live record (if it still exists) does NOT get
	// re-written — the live read path returns ErrNotFound after we
	// unlink it below, so updating it would be a wasted syscall.
	r.SuccessorID = successorID
	r.ArchivedCause = ArchivedCauseHandoff

	src, err := state.AgentPath(r.ID)
	if err != nil {
		return err
	}
	dst, err := state.AgentArchivePath(r.ID)
	if err != nil {
		return err
	}
	// Bare-archive collision keeps the same fallback shape as Archive
	// for symmetry — vanishingly rare with 32-bit ids but trivial to
	// support.
	if _, err := os.Stat(dst); err == nil {
		suffixed, derr := state.AgentArchivePath(r.ID + "-" + time.Now().UTC().Format("20060102-150405"))
		if derr != nil {
			return derr
		}
		dst = suffixed
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat archive path: %w", err)
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal archive payload: %w", err)
	}
	data = append(data, '\n')
	if err := state.WriteAtomic(dst, data); err != nil {
		return fmt.Errorf("write archive %s: %w", r.ID, err)
	}
	// Best-effort unlink of the live record. A failure here is
	// recoverable — the resolver's live-wins behavior means the agent
	// still surfaces as live until the live file goes away. Surface
	// as a non-fatal error so callers can log + continue.
	if err := os.Remove(src); err != nil {
		if os.IsNotExist(err) {
			// Already gone (another writer raced us); fine.
			return nil
		}
		return fmt.Errorf("unlink live record %s (archive committed): %w", r.ID, err)
	}
	return nil
}
