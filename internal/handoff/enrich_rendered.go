package handoff

// enrich_rendered.go — fill the narrative sections of an ALREADY-RENDERED
// handoff doc from durable coord state.
//
// The fleet-guard auto-handoff (skills/fleet-guard/handoff.py, fired at the
// 40/50% context thresholds and on PreCompact) renders the doc in Python.
// That side only has the tmux pane capture (→ Completed) plus the live
// machine state (Active Subagents / Open PRs); it writes Placeholder into
// Key Decisions, Docs (this session), Open Questions and Next Steps. The
// manual `fleet handoff <id>` path fills those from coord-state.json /
// tasks.md / the rolling checkpoint via EnrichManualDoc, so an auto-handed-
// off coord — the common case — used to hand its successor a doc with
// nothing to act on while the manual path handed over a full brief.
//
//	fleet-guard Stop hook (Python)
//	   write_doc(pane, active_subagents, open_prs)      # placeholders elsewhere
//	   fleet handoff-enrich <doc>  ──► EnrichRenderedDoc(raw, project, id, prev)
//	                                     Completed      = checkpoint bullets + pane
//	                                     Key Decisions  = recent_decisions (live ▸ checkpoint)
//	                                     Docs           = session_docs
//	                                     Open Questions = blocked/parked session slugs
//	                                     Next Steps     = session_next_steps + session_tasks
//	   write_queue(...)
//
// Only sections still holding Placeholder are filled (Completed is the
// exception: checkpoint completions are PREPENDED above the pane capture so
// neither view is lost). Active Subagents / Open PRs are never touched — the
// producer already walked live state for those. Best-effort: any failure
// returns the input unchanged.

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/edisonshen/fleet/internal/state"
)

// Section headings in Render order. The narrative fill walks these so a
// body is delimited by the NEXT KNOWN heading rather than any `## ` line —
// a pane capture in Completed may legitimately contain markdown headings.
const (
	headingFirstAction     = "## First Action (auto)"
	headingCompleted       = "## Completed"
	headingKeyDecisions    = "## Key Decisions"
	headingSessionDocs     = "## Docs (this session)"
	headingOpenQuestions   = "## Open Questions"
	headingNextSteps       = "## Next Steps (prioritized)"
	headingActiveSubagents = "## Active Subagents"
	headingOpenPRs         = "## Open PRs"
)

var renderedHeadings = []string{
	headingFirstAction,
	headingCompleted,
	headingKeyDecisions,
	headingSessionDocs,
	headingOpenQuestions,
	headingNextSteps,
	headingActiveSubagents,
	headingOpenPRs,
}

// Frontmatter is the chain metadata parsed back out of a rendered doc.
// Only the fields the enrichment needs are surfaced; PreviousHandoff is
// "" for a `null` previous_handoff.
type Frontmatter struct {
	AgentID         string
	TaskID          string
	Project         string
	PreviousHandoff string
}

// ParseFrontmatter reads the `---`-fenced frontmatter Render emits. Values
// are Go-%q quoted (or the bare `null`); unquoting failures keep the raw
// text so a slightly drifted doc still yields usable identity fields.
func ParseFrontmatter(raw []byte) (Frontmatter, error) {
	var fm Frontmatter
	body := string(raw)
	if !strings.HasPrefix(body, "---\n") {
		return fm, fmt.Errorf("handoff doc: missing frontmatter fence")
	}
	end := strings.Index(body[4:], "\n---\n")
	if end < 0 {
		return fm, fmt.Errorf("handoff doc: unterminated frontmatter")
	}
	for _, line := range strings.Split(body[4:4+end], "\n") {
		k, v, ok := splitCheckpointFrontmatterKV(line)
		if !ok {
			continue
		}
		if v == "null" {
			v = ""
		}
		switch k {
		case "agent_id":
			fm.AgentID = v
		case "task_id":
			fm.TaskID = v
		case "project":
			fm.Project = v
		case "previous_handoff":
			fm.PreviousHandoff = v
		}
	}
	if fm.AgentID == "" {
		return fm, fmt.Errorf("handoff doc: frontmatter has no agent_id")
	}
	return fm, nil
}

// renderedSection locates heading's body in a rendered doc. start/end are
// byte offsets of the body (exclusive of the heading line and of the
// "\n\n" that precedes the next heading). ok=false when the heading or the
// next known heading is missing.
func renderedSection(raw []byte, heading string) (start, end int, ok bool) {
	idx := -1
	for i, h := range renderedHeadings {
		if h == heading {
			idx = i
			break
		}
	}
	if idx < 0 || idx+1 >= len(renderedHeadings) {
		return 0, 0, false
	}
	marker := []byte("\n" + heading + "\n")
	pos := bytes.Index(raw, marker)
	if pos < 0 {
		return 0, 0, false
	}
	start = pos + len(marker)
	next := []byte("\n\n" + renderedHeadings[idx+1] + "\n")
	rel := bytes.Index(raw[start:], next)
	if rel < 0 {
		return 0, 0, false
	}
	return start, start + rel, true
}

// replaceRenderedSection swaps heading's body for newBody. Returns raw
// unchanged when the section can't be located.
func replaceRenderedSection(raw []byte, heading, newBody string) []byte {
	start, end, ok := renderedSection(raw, heading)
	if !ok {
		return raw
	}
	out := make([]byte, 0, len(raw)-(end-start)+len(newBody))
	out = append(out, raw[:start]...)
	out = append(out, newBody...)
	out = append(out, raw[end:]...)
	return out
}

// EnrichRenderedDoc fills the narrative sections of a rendered coord
// handoff doc from durable state and returns the new bytes. changed is
// false (and raw is returned as-is) when nothing could be added.
//
// CALLER GATE: coord docs only — the caller checks the coord identity
// (task_id == "coord-<project>") before calling, exactly as
// cmd/fleet/handoff.go gates EnrichManualDoc.
//
// Sources, matching EnrichManualDoc:
//   - Completed:      checkpoint `Completed (recent)` bullets, PREPENDED to
//     the existing body (the pane capture) unless that body is Placeholder.
//   - Key Decisions:  coord-state.json:recent_decisions live, else checkpoint.
//   - Docs:           CollectSessionDocs.
//   - Open Questions: CollectOpenQuestions.
//   - Next Steps:     CollectNextSteps.
//
// Every section other than Completed is filled ONLY while it still holds
// Placeholder, so re-running on an already-enriched doc is a no-op.
func EnrichRenderedDoc(raw []byte, project, agentID, lastHandoffPath string, logw func(string)) (out []byte, changed bool) {
	out = raw
	defer func() {
		if r := recover(); r != nil {
			if logw != nil {
				logw(fmt.Sprintf("enrich: recovered from panic during rendered-doc enrichment (%v); leaving doc unchanged", r))
			}
			out, changed = raw, false
		}
	}()

	pdir, err := state.ProjectDir(project)
	if err != nil {
		if logw != nil {
			logw(fmt.Sprintf("enrich: ProjectDir(%q) failed (%v); leaving doc unchanged", project, err))
		}
		return raw, false
	}
	pdir = filepath.Clean(pdir)
	coordStatePath := filepath.Join(pdir, "coord-state.json")

	cp, cpOK := loadCheckpointIfFresher(pdir, agentID, lastHandoffPath)

	fill := func(heading, body string) {
		if body == "" {
			return
		}
		start, end, ok := renderedSection(out, heading)
		if !ok || string(out[start:end]) != Placeholder {
			return
		}
		out = replaceRenderedSection(out, heading, body)
		changed = true
	}

	if cpOK {
		if bullets := renderCompletionBullets(cp.recentCompletions); bullets != "" {
			if start, end, ok := renderedSection(out, headingCompleted); ok {
				existing := string(out[start:end])
				body := bullets
				if existing != Placeholder && strings.TrimSpace(existing) != "" {
					body = bullets + "\n\n" + existing
				}
				out = replaceRenderedSection(out, headingCompleted, body)
				changed = true
			}
		}
	}

	decisions := renderCompletionBullets(CollectRecentDecisionsLive(coordStatePath, agentID))
	if decisions == "" && cpOK {
		decisions = renderCompletionBullets(cp.recentDecisions)
	}
	fill(headingKeyDecisions, decisions)
	fill(headingSessionDocs, CollectSessionDocs(coordStatePath, agentID))
	fill(headingOpenQuestions, CollectOpenQuestions(pdir, agentID))
	fill(headingNextSteps, CollectNextSteps(pdir, agentID))

	if !changed {
		return raw, false
	}
	return out, true
}
