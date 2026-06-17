package handoff

// enrich.go — fill the machine-state sections (Active Subagents + Open
// PRs) of a MANUAL handoff doc (`fleet handoff <id>` / TUI `[h]`) from
// on-disk state + `gh`, matching the auto-handoff Python path
// (skills/fleet-guard/handoff.py) rather than synth's recovery path.
//
// Why a separate walk from synth.go (and NOT a unification):
//
//	                          missing workers/<slug>/state.json
//	synth.go (recovery)       → SKIP the slug   (dead coord; archived
//	                            mid-crash; successor needn't re-dispatch
//	                            finished work)
//	enrich.go (live handoff)  → EMIT the slug with phase=""  (live coord
//	                            handing off; coord-state.json alone is
//	                            enough to re-dispatch; matches
//	                            handoff.py:_collect_active_subagents)
//
// The two walks share (a) loadCheckpointIfFresher, (b) the shared
// applyCheckpointToDoc lift, and (c) the tasks.md status overlay
// (readStatusBySlug) — they differ ONLY in the missing-worker branch.
// Per the design, we deliberately do not merge them.
//
// EnrichManualDoc is best-effort: a panic, a malformed file, a dead
// `gh`, or a non-git repo leaves the section's existing placeholder and
// the handoff still succeeds. Enrichment NEVER fails a handoff.
//
//	fleet handoff <id>
//	   └─> EnrichManualDoc(doc, project, oldRec.ID, oldRec.LastHandoffPath):
//	         [recover() → swallow on panic]
//	         active  = checkpoint-preferred walk, EMIT-on-missing
//	         openPRs = gh pr list head:worker/ (10s) [fallback: cp → empty]
//	       Write(doc)

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"time"

	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tasks"
)

// ghTimeout caps the `gh pr list` shell-out. Matches
// handoff.py:_collect_open_prs (timeout=10) so the auto and manual
// paths behave identically — a hung/slow `gh` degrades to the
// checkpoint fallback rather than wedging the handoff.
const ghTimeout = 10 * time.Second

// ghRunner runs `gh` with the given args and a context deadline,
// returning stdout. Production uses runGH (exec.CommandContext);
// tests inject a fake so no real `gh` / network is touched and the
// timeout path is exercised deterministically. The seam lives on the
// package var so EnrichManualDoc's call site stays simple.
var ghRunner = runGH

// runGH is the production ghRunner: resolve `gh` on PATH, run it under a
// ghTimeout deadline, return stdout. Errors (missing binary, non-zero
// exit, timeout) propagate to the caller, which falls back to the
// checkpoint Open PRs. We deliberately do NOT inspect stderr here — the
// caller logs a single fallback line; surfacing raw `gh` stderr would
// be noise on the common "non-git repo" path.
func runGH(args ...string) ([]byte, error) {
	bin, err := exec.LookPath("gh")
	if err != nil {
		return nil, fmt.Errorf("gh not on PATH: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), ghTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, args...).Output()
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ghOpenPR mirrors one element of `gh pr list --json
// number,title,headRefName,url`. Field names match gh's JSON keys
// exactly so encoding/json maps them without struct tags drift.
type ghOpenPR struct {
	Number      int    `json:"number"`
	Title       string `json:"title"`
	HeadRefName string `json:"headRefName"`
	URL         string `json:"url"`
}

// CollectOpenPRs runs `gh pr list --state open --search "head:worker/"
// --json number,title,headRefName,url` (10s timeout) and returns the
// parsed open worker PRs.
//
// Never errors: on missing `gh`, non-zero exit, empty output, timeout,
// or unparseable JSON it logs ONE diagnostic line to logw (when
// non-nil) and returns (nil, false). ok=false tells the caller to fall
// back to the checkpoint's Open PRs, then to empty.
//
// Field mapping mirrors handoff.py:_collect_open_prs exactly so the
// auto (Python) and manual (Go) paths produce identical Open PRs rows.
func CollectOpenPRs(logw func(string)) ([]OpenPR, bool) {
	out, err := ghRunner(
		"pr", "list",
		"--state", "open",
		"--search", "head:worker/",
		"--json", "number,title,headRefName,url",
	)
	if err != nil {
		if logw != nil {
			logw(fmt.Sprintf("enrich: gh pr list failed (%v); falling back to checkpoint Open PRs", err))
		}
		return nil, false
	}
	if len(out) == 0 {
		if logw != nil {
			logw("enrich: gh pr list returned empty output; falling back to checkpoint Open PRs")
		}
		return nil, false
	}
	var raw []ghOpenPR
	if err := json.Unmarshal(out, &raw); err != nil {
		if logw != nil {
			logw(fmt.Sprintf("enrich: gh pr list JSON parse failed (%v); falling back to checkpoint Open PRs", err))
		}
		return nil, false
	}
	prs := make([]OpenPR, 0, len(raw))
	for _, r := range raw {
		// ghOpenPR and OpenPR share field names/types (only json tags
		// differ), so a direct conversion is exact and tag-agnostic.
		prs = append(prs, OpenPR(r))
	}
	return prs, true
}

// CollectActiveSubagentsLive walks coord-state.json:worker_agent_ids
// for project and returns one ActiveSubagent per in-flight worker, using
// LIVE-handoff (emit-on-missing) semantics: a slug whose
// workers/<slug>/state.json is missing or unparseable is STILL emitted
// with LastPhase="" (the agent_id + slug alone drive a re-dispatch).
// This diverges from synth.go's recovery walk, which SKIPS such slugs —
// see the package comment. Matches handoff.py:_collect_active_subagents.
//
// tasks.md status is overlaid per slug (readStatusBySlug); pr_url is
// read from the worker state.json when present. Both default to "" on
// any read failure, which the successor coord treats as the safe
// "re-dispatch" fallback.
//
// Best-effort: a missing project tree / missing coord-state.json / empty
// worker map returns nil (the caller leaves the _(none)_ placeholder). A
// malformed project NAME (validation error from ProjectDir) also returns
// nil — enrichment never fails the handoff.
func CollectActiveSubagentsLive(project string) []ActiveSubagent {
	pdir, err := state.ProjectDir(project)
	if err != nil {
		return nil
	}
	pdir = filepath.Clean(pdir)

	agentIDsBySlug, err := readWorkerAgentIDs(filepath.Join(pdir, "coord-state.json"))
	if err != nil || len(agentIDsBySlug) == 0 {
		return nil
	}

	statusBySlug := readStatusBySlug(pdir, project)

	slugs := make([]string, 0, len(agentIDsBySlug))
	for slug := range agentIDsBySlug {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)

	wDir := filepath.Join(pdir, "workers")
	out := make([]ActiveSubagent, 0, len(slugs))
	for _, slug := range slugs {
		// EMIT-on-missing: unlike synth, a missing/unparseable worker
		// state.json yields an entry with phase="" rather than a skip.
		sub, _ := readWorkerStateForSynth(wDir, slug)
		sub.TaskID = slug
		sub.Branch = "worker/" + slug
		sub.AgentID = agentIDsBySlug[slug]
		if st, ok := statusBySlug[slug]; ok {
			sub.Status = st
		}
		out = append(out, sub)
	}
	return out
}

// applyCheckpointToDoc copies the lifted-verbatim sections of a parsed
// rolling checkpoint into doc. Extracted from
// SynthesizeRecoveryWithLastHandoff's former inline block so BOTH the
// recovery-synth path and the manual EnrichManualDoc path apply the
// checkpoint identically.
//
// SLICE 1 IS BEHAVIOR-PRESERVING: this reproduces synth's prior inline
// mapping verbatim (ActiveSubagents, OpenPRs, decisions→NextSteps) — the
// refactor-parity test (synth_test.go) pins byte-identical recovery
// output. Slice 2 changes THIS one helper to add the narrative
// (Completed (recent) → doc.Completed), fixing manual + recovery in
// lockstep.
func applyCheckpointToDoc(doc *Doc, cp *checkpointDoc) {
	doc.ActiveSubagents = cp.activeSubagents
	doc.OpenPRs = cp.openPRs
	// Recent decisions surface in NextSteps as a free-form bullet list.
	// handoff_resume.py doesn't parse them structurally — they're
	// operator-readable "what was the coord up to" context. Dropping
	// them into NextSteps keeps the existing doc shape (and parser)
	// unchanged. Empty buffer leaves doc.NextSteps as-is (Placeholder
	// for synth).
	doc.NextSteps = applyRecentDecisions(doc.NextSteps, cp.recentDecisions)
}

// applyRecentDecisions returns the NextSteps body for a doc given the
// checkpoint's recent-decisions buffer: the rendered bullet list when
// non-empty, else the passed-through fallback (the doc's existing
// NextSteps). Split out so the formatting lives in one place and reads
// the same in both producers.
func applyRecentDecisions(fallback string, decisions []string) string {
	if len(decisions) == 0 {
		return fallback
	}
	var b []byte
	b = append(b, "Recent coord decisions (from checkpoint):\n"...)
	for _, d := range decisions {
		b = append(b, "- "...)
		b = append(b, d...)
		b = append(b, '\n')
	}
	// Trim the trailing newline (TrimRight on '\n').
	for len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	return string(b)
}

// EnrichManualDoc fills the machine-state sections of a manual handoff
// doc in place, best-effort. agentID is the handing-off coord's id (used
// for the checkpoint generation guard). lastHandoffPath is the doc this
// coord inherited (a *string on the agent record; nil normalizes to "").
//
// Preference, matching the other producers:
//   - checkpoint fresher than lastHandoffPath + same coord generation
//     → lift it wholesale via applyCheckpointToDoc, then OVERWRITE Open
//     PRs with a fresh `gh` query (gh is authoritative + fresher than
//     the snapshot; on gh failure the checkpoint's Open PRs stand).
//   - otherwise → live emit-on-missing walk for Active Subagents + fresh
//     `gh` for Open PRs (gh failure → leave the existing placeholder).
//
// Narrative sections (Completed / Key Decisions) stay placeholder in
// Slice 1. Any panic mid-build is recovered and swallowed — the doc
// keeps whatever sections were already filled (placeholders at worst).
func EnrichManualDoc(doc *Doc, project, agentID string, lastHandoffPath *string, logw func(string)) {
	// Best-effort guard: a panic anywhere in enrichment must not fail
	// the handoff. Leave the doc with whatever it had (placeholders).
	defer func() {
		if r := recover(); r != nil && logw != nil {
			logw(fmt.Sprintf("enrich: recovered from panic during manual-doc enrichment (%v); leaving placeholders", r))
		}
	}()

	if doc == nil {
		return
	}

	lhp := ""
	if lastHandoffPath != nil {
		lhp = *lastHandoffPath
	}

	pdir, err := state.ProjectDir(project)
	if err != nil {
		// Malformed project name — leave placeholders. ProjectDir only
		// errors on validation, so this is a config bug, not a runtime
		// miss; surface it but don't fail the handoff.
		if logw != nil {
			logw(fmt.Sprintf("enrich: ProjectDir(%q) failed (%v); leaving placeholders", project, err))
		}
		return
	}
	pdir = filepath.Clean(pdir)

	cp, cpOK := loadCheckpointIfFresher(pdir, agentID, lhp)

	if cpOK {
		// Checkpoint wins for Active Subagents (same preference synth
		// uses). applyCheckpointToDoc also sets OpenPRs from the
		// snapshot; we then prefer a fresh gh query below.
		applyCheckpointToDoc(doc, cp)
	} else {
		// No fresher checkpoint → live emit-on-missing walk. Leave the
		// section's placeholder when there are no in-flight workers.
		if subs := CollectActiveSubagentsLive(project); len(subs) > 0 {
			doc.ActiveSubagents = subs
		}
	}

	// Open PRs preference, most-authoritative first:
	//   1. `gh pr list` (fresh, authoritative) — overwrite on success.
	//   2. checkpoint snapshot (already set by applyCheckpointToDoc when
	//      cpOK) — stands when gh fails.
	//   3. tasks.md pr_url per slug — when gh fails AND there's no
	//      checkpoint snapshot. tasks.md carries the authoritative pr_url
	//      the coord recorded, so a non-git shell / gh-auth / timeout
	//      failure no longer drops shepherd supervision for every
	//      in-review PR. handoff_resume.py keys the shepherd respawn off
	//      the row's trailing URL (regex `\s—\s(https?://\S+)$`), so a
	//      Number=0 / title=slug row is sufficient — the URL is the only
	//      load-bearing field. (codex Slice-1 P1.)
	//   4. empty → _(no open PRs)_ placeholder.
	if prs, ghOK := CollectOpenPRs(logw); ghOK {
		doc.OpenPRs = prs
	} else if len(doc.OpenPRs) == 0 {
		if fallback := collectOpenPRsFromTasks(pdir, project); len(fallback) > 0 {
			if logw != nil {
				logw(fmt.Sprintf("enrich: gh unavailable; recovered %d Open PR(s) from tasks.md", len(fallback)))
			}
			doc.OpenPRs = fallback
		}
	}
}

// collectOpenPRsFromTasks reads tasks.md and returns an OpenPR per task
// that has a non-empty pr_url, used as the degraded-path fallback when
// `gh pr list` is unavailable and no fresher checkpoint snapshot exists.
//
// tasks.md does NOT carry a PR number or title, so the row is partial:
// URL = pr_url (the load-bearing field handoff_resume.py parses), head =
// the task's branch (or worker/<slug>), title = slug, number = 0. The
// successor still re-spawns one shepherd per URL — supervision continuity
// is preserved even though the operator-readability fields are coarse.
//
// Best-effort: any read/parse failure or empty tasks.md returns nil and
// the caller falls through to the _(no open PRs)_ placeholder.
func collectOpenPRsFromTasks(pdir, project string) []OpenPR {
	path := filepath.Join(pdir, "tasks.md")
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	f, err := tasks.Read(path)
	if err != nil {
		return nil
	}
	// Sort by slug for deterministic doc shape (matches the live walk +
	// synth ordering; operators benefit from stable ordering too).
	sort.Slice(f.Tasks, func(i, j int) bool { return f.Tasks[i].Slug < f.Tasks[j].Slug })
	var out []OpenPR
	for _, t := range f.Tasks {
		if t.PRURL == "" {
			continue
		}
		head := t.Branch
		if head == "" {
			head = "worker/" + t.Slug
		}
		out = append(out, OpenPR{
			Number:      0, // unknown from tasks.md; URL is the load-bearing field.
			Title:       t.Slug,
			HeadRefName: head,
			URL:         t.PRURL,
		})
	}
	return out
}
