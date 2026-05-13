package handoff

// synth.go — synthesize a recovery handoff doc from on-disk state when
// `fleet dispatch <existing-coord>` lands on a coord whose tmux session
// is gone and whose pid is dead.
//
// The synth doc has the same shape as a normal handoff (frontmatter +
// Active Subagents + Open PRs body) so the successor coord's existing
// handoff_resume.py path consumes it without a special branch. The
// frontmatter handoff_type is set to "recovery-synth" so any future
// branch that wants to distinguish "clean handoff" from "recovery"
// (e.g. logging, telemetry) can do so by reading one field.
//
// Walk order:
//
//	~/.fleet/projects/<p>/coord-state.json
//	    └─ worker_agent_ids (slug → agent_id)
//
//	per slug:
//	  ~/.fleet/projects/<p>/workers/<slug>/state.json
//	    ├─ phase    → ActiveSubagent.LastPhase
//	    └─ pr_url   → ActiveSubagent.PRURL
//
// Slugs without an on-disk worker state.json are skipped (the legitimate
// case is "coord crashed mid-archive": the slug entry persists but the
// worker dir was already moved to workers/archive/). The successor coord
// doesn't need to re-dispatch finished work, so skipping is the right
// move — the alternative (rendering a malformed row) would block resume.
//
// Open PRs enrichment is OUT of synth's scope: shell-out to `gh pr list`
// is the dispatch caller's job (or the successor coord can re-derive
// from `gh` on its first tick). The OpenPRs slice is left empty here so
// synth stays a pure function of on-disk state — no network, no
// subprocesses, no test-side flakes.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/edisonshen/fleet/internal/state"
)

// TypeRecoverySynth marks a handoff doc that was synthesized from
// on-disk state rather than written by a live agent. The successor
// coord can distinguish from clean handoffs by reading the
// frontmatter's handoff_type field.
const TypeRecoverySynth = "recovery-synth"

// SynthesizeRecovery builds a recovery-synth Doc from the on-disk state
// under ~/.fleet/projects/<project>/. agentID is the dead coord's id —
// it stamps the synth doc's agent_id so the chain link is preserved
// when the successor reads its previous_handoff frontmatter (the dead
// agent IS the predecessor, even though it can't write its own handoff
// anymore). ts is the synth doc's timestamp — typically time.Now().UTC()
// from the dispatch path.
//
// Never errors on a missing project tree / missing coord-state.json /
// missing worker state.json: those are the legitimate "fresh project"
// or "mid-archive crash" cases the synth doc must tolerate. A returned
// error means a hard FS failure (Root() unresolvable, malformed JSON
// we couldn't recover from) — caller decides whether to fail the
// dispatch or proceed with an empty doc.
func SynthesizeRecovery(agentID, project string, ts time.Time) (*Doc, error) {
	doc := &Doc{
		AgentID:       agentID,
		TaskID:        "coord-" + project,
		Project:       project,
		Type:          TypeRecoverySynth,
		Number:        1, // No chain history available; mint a fresh number.
		Timestamp:     ts.UTC(),
		Completed:     Placeholder,
		KeyDecisions:  Placeholder,
		FilesModified: Placeholder,
		OpenQuestions: Placeholder,
		NextSteps:     Placeholder,
	}

	pdir, err := state.ProjectDir(project)
	if err != nil {
		// ProjectDir only errors on validation (bad name). Surface so
		// the caller knows the project name itself is malformed —
		// proceeding with an empty doc would mask a config bug.
		return nil, fmt.Errorf("SynthesizeRecovery: %w", err)
	}
	pdir = filepath.Clean(pdir)

	agentIDsBySlug, err := readWorkerAgentIDs(filepath.Join(pdir, "coord-state.json"))
	if err != nil {
		return nil, fmt.Errorf("read coord-state.json: %w", err)
	}
	if len(agentIDsBySlug) == 0 {
		// Empty project — no in-flight work. Doc still renders cleanly
		// via the _(none)_ placeholder for Active Subagents.
		return doc, nil
	}

	// Sort slugs for deterministic ordering — without this, the synth
	// doc body would shuffle on each run and a hand-diff between two
	// recovery attempts on the same state would lie. Tests rely on
	// the order being a function of input shape only.
	slugs := make([]string, 0, len(agentIDsBySlug))
	for slug := range agentIDsBySlug {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)

	wDir := filepath.Join(pdir, "workers")
	for _, slug := range slugs {
		sub, ok := readWorkerStateForSynth(wDir, slug)
		if !ok {
			// Worker dir gone (archived mid-crash, hand-deleted, never
			// existed). Skip — successor coord doesn't need to know.
			continue
		}
		sub.TaskID = slug
		sub.Branch = "worker/" + slug
		sub.AgentID = agentIDsBySlug[slug]
		doc.ActiveSubagents = append(doc.ActiveSubagents, sub)
	}

	return doc, nil
}

// readWorkerAgentIDs parses coord-state.json's worker_agent_ids map.
// Missing file is treated as empty map (legitimate fresh-project case).
// Malformed JSON returns an error so the caller can refuse to publish
// a partial recovery doc.
func readWorkerAgentIDs(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	var raw struct {
		WorkerAgentIDs map[string]string `json:"worker_agent_ids"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse coord-state.json: %w", err)
	}
	if raw.WorkerAgentIDs == nil {
		return map[string]string{}, nil
	}
	return raw.WorkerAgentIDs, nil
}

// readWorkerStateForSynth reads workers/<slug>/state.json and returns
// the subset of fields the synth doc needs. We avoid importing
// internal/workers here to keep this package free of the workers ↔
// handoff dep cycle (workers already imports state; handoff already
// imports state; bringing workers in would couple two unrelated
// schemas at a place where the synth doc only needs phase + pr_url).
//
// Returns ok=false when the file is missing or malformed — the slug is
// skipped at the call site. Permissive parsing (no schema_version
// check) is deliberate: synth runs against state files written by
// possibly-older or partially-written workers, and the alternative
// (refusing to build the doc) is worse than skipping one row.
func readWorkerStateForSynth(workersDir, slug string) (ActiveSubagent, bool) {
	path := filepath.Join(workersDir, slug, "state.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return ActiveSubagent{}, false
	}
	var raw struct {
		Phase  string `json:"phase"`
		PRURL  string `json:"pr_url"`
		Status string `json:"status"` // optional; not always present
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return ActiveSubagent{}, false
	}
	return ActiveSubagent{
		LastPhase: raw.Phase,
		PRURL:     raw.PRURL,
		Status:    raw.Status,
	}, true
}
