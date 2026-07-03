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
// reader.
type sessionDoc struct {
	Path string `json:"path"`
	Role string `json:"role"`
	TS   string `json:"ts"`
}

// coordStateForCollect is the subset of coord-state.json the collectors read.
// A pointer-typed session_docs would over-fit; a plain slice + a []string
// suffice, and unknown keys are ignored by encoding/json.
type coordStateForCollect struct {
	SessionDocs     []sessionDoc `json:"session_docs"`
	RecentDecisions []string     `json:"recent_decisions"`
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
// Never errors: missing / malformed coord-state.json, an empty
// coordStatePath, or an absent/empty session_docs key returns "" so the
// caller keeps the section's placeholder.
func CollectSessionDocs(coordStatePath string) string {
	docs := readCoordStateForCollect(coordStatePath).SessionDocs
	// Drop malformed entries (a hand-edited row missing path/role).
	valid := docs[:0]
	for _, d := range docs {
		if strings.TrimSpace(d.Path) == "" || strings.TrimSpace(d.Role) == "" {
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
// Never errors: missing / malformed coord-state.json, an empty
// coordStatePath, or an absent/empty recent_decisions key returns nil so the
// caller keeps whatever the checkpoint lift already set.
func CollectRecentDecisionsLive(coordStatePath string) []string {
	raw := readCoordStateForCollect(coordStatePath).RecentDecisions
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
