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
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/edisonshen/fleet/internal/state"
)

// SchemaVersion is bumped when Record's on-disk shape changes
// incompatibly. Readers compare and refuse newer versions.
const SchemaVersion = 1

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
	InboxPending   bool       `json:"inbox_pending"`
	HandoffType    *string    `json:"handoff_type"`
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
func List() ([]*Record, error) {
	dir, err := state.AgentDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("readdir agents: %w", err)
	}
	var out []*Record
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		id := e.Name()[:len(e.Name())-len(".json")]
		r, err := Load(id)
		if err != nil {
			// Skip records we can't parse rather than failing the
			// whole list — partial results beat zero results when
			// the operator is trying to triage.
			continue
		}
		out = append(out, r)
	}
	return out, nil
}
