package handoff

// collect.go — session-scoped collectors for the manual + recovery handoff
// docs. Both read LIVE from ~/.fleet/projects/<p>/coord-state.json (written
// by `fleet checkpoint` while the coord is alive), NOT from a synchronous
// prompt to a possibly-dead agent:
//
//	section              source                              reader
//	-------              ------                              ------
//	Docs (this session)  coord-state.json:session_docs       CollectSessionDocs
//	Key Decisions        coord-state.json:recent_decisions   CollectRecentDecisionsLive
//	                     (live-preferred over the checkpoint)
//
// (Next Steps + Open Questions come from tasks.md via CollectNextSteps /
// CollectOpenQuestions in enrich.go; Completed comes from the rolling
// checkpoint buffer via applyCheckpointToDoc.)
//
// Both collectors are best-effort: a missing / malformed coord-state.json, an
// empty path, or an absent/empty key returns the zero value so the caller
// keeps the section's existing placeholder. Enrichment NEVER fails a handoff.
//
// The retired predecessor `CollectFilesModified` shelled out to `git status`
// and dumped EVERY untracked doc under docs/ (every past coord's litter);
// `CollectSessionDocs` shows only the plan docs THIS coord recorded.

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// sessionDocsMax caps how many "Docs (this session)" rows render in a
// handoff doc so a long-lived coord that authored a large number of plan
// docs can't bloat the section. Its own constant, INDEPENDENT of the
// recent_decisions env cap (resolve_checkpoint_decisions, default 10) —
// the two buffers cap separately (design: coord-state schema). The writer
// (`fleet checkpoint doc`) caps at the same value; this reader-side cap is
// belt-and-suspenders against a hand-edited coord-state.json.
const sessionDocsMax = 20

// sessionDoc is one entry of coord-state.json:session_docs — a plan doc the
// coordinator authored or is actively implementing this session. The Go CLI
// (`fleet checkpoint doc`) is the only writer; the collector below is the
// reader. CoordID stamps the writing coord's generation (FLEET_AGENT_ID at
// write time; empty for an operator-shell write) so the reader can apply
// the SAME generation guard loadCheckpointIfFresher applies to the
// checkpoint — coord-state.json survives coord succession, and without the
// stamp a successor's "Docs (this session)" would attribute a predecessor's
// docs to itself.
type sessionDoc struct {
	Path    string `json:"path"`
	Role    string `json:"role"`
	TS      string `json:"ts"`
	CoordID string `json:"coord_id,omitempty"`
}

// coordStateForCollect is the subset of coord-state.json the collectors read.
// A pointer-typed session_docs would over-fit; a plain slice + a []string
// suffice, and unknown keys are ignored by encoding/json.
// RecentDecisionsOwner is the generation stamp `fleet checkpoint decision`
// maintains for the shared recent_decisions buffer (per-entry stamping would
// break the plain-strings checkpoint round-trip the tick producer shares).
type coordStateForCollect struct {
	SessionDocs          []sessionDoc      `json:"session_docs"`
	RecentDecisions      []string          `json:"recent_decisions"`
	RecentDecisionsOwner string            `json:"recent_decisions_owner"`
	SessionNextSteps     []sessionNextStep `json:"session_next_steps"`
	SessionTasks         []sessionTask     `json:"session_tasks"`
}

// sessionNextStep is one entry of coord-state.json:session_next_steps — an
// explicit free-text Next Step the coordinator recorded via `fleet checkpoint
// next-step`. Slug is optional (used only for exact dedup against the auto
// block). CoordID stamps the writing coord's generation so CollectNextSteps
// drops a predecessor's entries after succession (coord-state.json survives).
type sessionNextStep struct {
	Text    string `json:"text"`
	Slug    string `json:"slug,omitempty"`
	CoordID string `json:"coord_id,omitempty"`
	TS      string `json:"ts"`
}

// sessionTask is one entry of coord-state.json:session_tasks — a slug this
// coordinator promoted or dispatched this session (the auto Next Steps
// source). Dual-written by the Go `fleet tasks promote` and the Python tick
// (_record_session_task); the JSON keys are byte-identical across both.
// CoordID drives the same foreignGeneration filter as sessionDoc.
type sessionTask struct {
	Slug    string `json:"slug"`
	CoordID string `json:"coord_id,omitempty"`
	TS      string `json:"ts"`
}

// foreignGeneration reports whether a stamped owner belongs to a DIFFERENT
// coord generation than the agent whose handoff doc is being built. Empty
// stamp (legacy entry / operator-shell write) or empty agentID (caller
// without generation context) can't be attributed, so it is NOT foreign —
// the exact semantics of loadCheckpointIfFresher's coord_id guard.
func foreignGeneration(stamp, agentID string) bool {
	return stamp != "" && agentID != "" && stamp != agentID
}

// readCoordStateForCollect loads the collector's view of coord-state.json.
// Empty path, missing file, or malformed JSON → zero-value struct (both
// collectors then render their placeholder). Never errors.
func readCoordStateForCollect(coordStatePath string) coordStateForCollect {
	var cs coordStateForCollect
	if strings.TrimSpace(coordStatePath) == "" {
		return cs
	}
	data, err := os.ReadFile(coordStatePath)
	if err != nil {
		return cs
	}
	// Ignore unmarshal errors: a malformed / partially-written file degrades
	// to the placeholder, never fails the handoff. (Atomic tmp+rename means a
	// reader normally sees a whole old-or-new file; genuine corruption still
	// degrades gracefully.)
	_ = json.Unmarshal(data, &cs)
	return cs
}

// CollectSessionDocs renders the `## Docs (this session)` body from
// coord-state.json:session_docs — the plan docs THIS coordinator authored or
// is implementing, recorded live via `fleet checkpoint doc`. Each entry
// renders `- <role>: <path>`; the NEWEST sessionDocsMax are shown with a
// `- … and N more` tail counting the older overflow.
//
// agentID is the coord whose handoff doc is being built. coord-state.json
// survives coord succession (the tick preserves session_docs across
// generations), so entries stamped with a DIFFERENT coord_id are a
// predecessor's (or, on a recovery misclassification, a live sibling's) —
// they are filtered out, mirroring loadCheckpointIfFresher's generation
// guard. Unstamped entries (legacy / operator-shell writes) and an empty
// agentID pass through, same as the checkpoint guard's empty-coord_id
// handling.
//
// Never errors: missing / malformed coord-state.json, an empty
// coordStatePath, or an absent/empty session_docs key returns "" so the
// caller keeps the section's placeholder.
func CollectSessionDocs(coordStatePath, agentID string) string {
	docs := readCoordStateForCollect(coordStatePath).SessionDocs
	// Drop malformed entries (a hand-edited row missing path/role) and
	// foreign-generation entries (another coord's session).
	valid := docs[:0]
	for _, d := range docs {
		if strings.TrimSpace(d.Path) == "" || strings.TrimSpace(d.Role) == "" {
			continue
		}
		if foreignGeneration(d.CoordID, agentID) {
			continue
		}
		valid = append(valid, d)
	}
	if len(valid) == 0 {
		return ""
	}
	total := len(valid)
	hidden := 0
	if total > sessionDocsMax {
		// Keep the NEWEST sessionDocsMax (append order is chronological, so
		// the tail is newest); count the older head as overflow.
		hidden = total - sessionDocsMax
		valid = valid[hidden:]
	}
	var b strings.Builder
	for i, d := range valid {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "- %s: %s", d.Role, d.Path)
	}
	if hidden > 0 {
		fmt.Fprintf(&b, "\n- … and %d more", hidden)
	}
	return b.String()
}

// CollectRecentDecisionsLive reads coord-state.json:recent_decisions LIVE
// (the same buffer the tick auto-producer AND `fleet checkpoint decision`
// both feed) and returns it as an ordered slice. The handoff paths prefer
// this over the tick-published coord-checkpoint.md value so an agent
// rationale logged out-of-band (no tick between the log and the handoff)
// still reaches Key Decisions — the motivating no-tick case.
//
// agentID gates the live override by generation: `fleet checkpoint
// decision` stamps coord-state.json:recent_decisions_owner with the writing
// coord's FLEET_AGENT_ID, and a stamp from a DIFFERENT coord means the live
// buffer's agent lines belong to another generation (predecessor leftovers
// after succession, or a live sibling during a recovery misclassification).
// In that case return nil — the caller falls back to the checkpoint value,
// which loadCheckpointIfFresher already generation-guards. An empty stamp
// (tick-only buffer, no CLI write yet) or empty agentID passes: that is the
// pre-existing tick-producer exposure, unchanged.
//
// Never errors: missing / malformed coord-state.json, an empty
// coordStatePath, or an absent/empty recent_decisions key returns nil so the
// caller keeps whatever the checkpoint lift already set.
func CollectRecentDecisionsLive(coordStatePath, agentID string) []string {
	cs := readCoordStateForCollect(coordStatePath)
	if foreignGeneration(cs.RecentDecisionsOwner, agentID) {
		return nil
	}
	raw := cs.RecentDecisions
	out := make([]string, 0, len(raw))
	for _, d := range raw {
		if strings.TrimSpace(d) == "" {
			continue
		}
		out = append(out, d)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
