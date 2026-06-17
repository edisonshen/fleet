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

// ghRunner runs `gh` in working directory dir with the given args and a
// context deadline, returning stdout. Production uses runGH
// (exec.CommandContext); tests inject a fake so no real `gh` / network
// is touched and the timeout path is exercised deterministically. The
// seam lives on the package var so EnrichManualDoc's call site stays
// simple.
//
// dir binds `gh` to the handed-off coord's repo checkout: `gh pr list`
// resolves the repo from CWD, so without this an operator running
// `fleet handoff` from another directory would capture unrelated PRs
// (or fail with "not a git repository"). Empty dir → inherit the
// process CWD (legacy behavior; only happens when the caller couldn't
// resolve a repo).
var ghRunner = runGH

// runGH is the production ghRunner: resolve `gh` on PATH, run it in dir
// under a ghTimeout deadline, return stdout. Errors (missing binary,
// non-zero exit, timeout) propagate to the caller, which falls back to
// the checkpoint / tasks.md Open PRs. We deliberately do NOT inspect
// stderr here — the caller logs a single fallback line; surfacing raw
// `gh` stderr would be noise on the common "non-git repo" path.
func runGH(dir string, args ...string) ([]byte, error) {
	bin, err := exec.LookPath("gh")
	if err != nil {
		return nil, fmt.Errorf("gh not on PATH: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), ghTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir // empty → inherit process CWD
	out, err := cmd.Output()
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
//
// repoDir binds `gh` to the handed-off coord's checkout (empty →
// process CWD); see ghRunner.
func CollectOpenPRs(repoDir string, logw func(string)) ([]OpenPR, bool) {
	out, err := ghRunner(
		repoDir,
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
// "re-dispatch" fallback. A row that carries a pr_url but a pre-PR
// status is promoted to in-review (statusIsPrePR) so the successor
// takes the shepherd-only branch instead of double-dispatching.
//
// Return contract — the bool is "live coord-state was READABLE
// (authoritative)":
//   - ok=true, subs=non-empty: live workers, use them.
//   - ok=true, subs=nil: coord-state.json read fine and has NO workers
//     (all finished, or fresh project) → AUTHORITATIVE EMPTY. The caller
//     must CLEAR ## Active Subagents, not fall back to a stale checkpoint
//     that still lists finished workers. (codex Slice-1 P1.)
//   - ok=false, subs=nil: coord-state.json unreadable (malformed JSON) or
//     a malformed project NAME → the caller may fall back to the
//     checkpoint snapshot. Enrichment never fails the handoff either way.
func CollectActiveSubagentsLive(project string) ([]ActiveSubagent, bool) {
	pdir, err := state.ProjectDir(project)
	if err != nil {
		return nil, false
	}
	pdir = filepath.Clean(pdir)

	csPath := filepath.Join(pdir, "coord-state.json")
	// MISSING coord-state.json is NOT authoritative: a live coord always
	// writes it, so its absence means we can't observe live state (fresh
	// project before first dispatch, or a partial setup). Treat as
	// ok=false so the caller may fall back to the checkpoint, rather than
	// clearing a section the checkpoint legitimately populated. A PRESENT
	// file with an empty worker map IS authoritative (all workers
	// finished) — see below.
	if _, statErr := os.Stat(csPath); statErr != nil {
		return nil, false
	}

	agentIDsBySlug, err := readWorkerAgentIDs(csPath)
	if err != nil {
		// Unreadable (malformed JSON) — let the caller fall back to the
		// checkpoint.
		return nil, false
	}
	if len(agentIDsBySlug) == 0 {
		// Present + read OK + zero workers → AUTHORITATIVE EMPTY (ok=true).
		// The coord has no in-flight work; the section must be cleared
		// rather than reviving stale checkpoint rows. (codex Slice-1 P1.)
		return nil, true
	}

	metaBySlug := readTaskMetaBySlug(pdir)

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
		meta := metaBySlug[slug] // zero value when slug absent from tasks.md
		if meta.status != "" {
			sub.Status = meta.status
		}
		// pr_url comes from EITHER source: tasks.md (Python's source) OR
		// state.json (set earlier in the worker lifecycle, before the
		// coord stamps tasks.md). Whichever is non-empty wins; tasks.md
		// takes precedence when both are set. This handles both transient
		// windows — PR written to state.json but not yet tasks.md, AND the
		// reverse (tasks.md stamped before state.json) — that codex
		// flagged. (codex Slice-1 P1, both directions.)
		if meta.prURL != "" {
			sub.PRURL = meta.prURL
		}
		// PR-open promotion: a worker that has already opened a PR (pr_url
		// from either source) but whose status is still pre-PR (empty /
		// todo / ready / in-progress) is in the transient window before
		// the coord flips it to in-review. Leaving it makes
		// handoff_resume.py BOTH re-dispatch the Agent AND respawn a
		// shepherd from ## Open PRs — duplicating work against the open
		// PR. Promote to in-review so the resume takes the shepherd-only
		// branch. Terminal statuses (done/abandoned/blocked) are left
		// as-is; the resume path already skips them.
		if sub.PRURL != "" && statusIsPrePR(sub.Status) {
			sub.Status = string(tasks.StatusInReview)
		}
		out = append(out, sub)
	}
	return out, true
}

// taskMeta is the per-slug (status, pr_url) pair the live walk overlays
// from tasks.md — mirrors handoff.py:_read_tasks_meta's
// {slug: (status, pr_url)} sourcing so the manual path matches the auto
// path's tasks.md-as-source-of-truth for both fields.
type taskMeta struct {
	status string
	prURL  string
}

// readTaskMetaBySlug reads tasks.md and returns {slug: {status, pr_url}}.
// Best-effort: any missing-file / parse error returns an empty map and
// the caller falls back to state.json + the safe "re-dispatch" default.
func readTaskMetaBySlug(projectDir string) map[string]taskMeta {
	out := map[string]taskMeta{}
	path := filepath.Join(projectDir, "tasks.md")
	if _, err := os.Stat(path); err != nil {
		return out
	}
	f, err := tasks.Read(path)
	if err != nil {
		return out
	}
	for _, t := range f.Tasks {
		out[t.Slug] = taskMeta{status: string(t.Status), prURL: t.PRURL}
	}
	return out
}

// statusIsPrePR reports whether a tasks.md status is a pre-PR / still-
// writing state for which an already-open PR should promote the row to
// in-review (so the successor takes the shepherd-only resume branch).
// Empty status (legacy row / unread tasks.md) counts: a PR exists, so the
// safe interpretation is "in review", not "re-dispatch".
func statusIsPrePR(status string) bool {
	switch tasks.Status(status) {
	case "", tasks.StatusTodo, tasks.StatusReady, tasks.StatusInProgress:
		return true
	default:
		return false
	}
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
// repoDir is the handed-off coord's repo checkout — `gh pr list` runs
// there so it resolves the right repo regardless of the operator's CWD
// (empty → process CWD).
//
// CALLER GATE: this is for COORD handoffs only. Active Subagents +
// Open PRs are coord-owned project state; a worker handoff has none, and
// pulling a live coord's coord-state.json into a worker's doc would
// resume the worker with unrelated project-wide state. The cmd/fleet
// caller gates this call on spawn.IsCoordSpawn(taskID, project).
//
// Preference, matching the other producers:
//   - checkpoint fresher than lastHandoffPath + same coord generation
//     → lift it wholesale via applyCheckpointToDoc, then OVERWRITE Open
//     PRs with a fresh `gh` query (gh is authoritative + fresher than
//     the snapshot; on gh failure the checkpoint's Open PRs stand).
//   - otherwise → live emit-on-missing walk for Active Subagents + fresh
//     `gh` for Open PRs (gh failure → tasks.md → leave the placeholder).
//
// Narrative sections (Completed / Key Decisions) stay placeholder in
// Slice 1. Any panic mid-build is recovered and swallowed — the doc
// keeps whatever sections were already filled (placeholders at worst).
func EnrichManualDoc(doc *Doc, project, agentID, repoDir string, lastHandoffPath *string, logw func(string)) {
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

	// NARRATIVE + baseline machine state from the checkpoint via the
	// SHARED lift (the seam Slice 2 extends to add doc.Completed). This
	// gives the manual doc the checkpoint's recent-decisions→NextSteps
	// (and, post-Slice-2, Completed) in lockstep with the recovery path.
	// The machine-state fields it sets (ActiveSubagents / OpenPRs) are a
	// stale baseline that the LIVE walk + gh OVERWRITE immediately below —
	// live data wins for machine state on a manual (live-coord) handoff.
	if cpOK {
		applyCheckpointToDoc(doc, cp)
	}

	// ACTIVE SUBAGENTS — LIVE-FIRST (overwrites the checkpoint baseline).
	// `fleet handoff` runs against a (usually) live coord, so
	// coord-state.json + tasks.md are CURRENT — fresher than a periodic
	// checkpoint snapshot (~2.5min cadence). A worker dispatched,
	// finished, or PR-opened since the last checkpoint tick is reflected
	// in live state but stale in the checkpoint. Walk live state first;
	// keep the checkpoint baseline only when the live walk yields nothing
	// (e.g. coord-state.json unreadable). This matches the Python
	// auto-handoff path, which always walks live state. (codex Slice-1 P1.)
	//
	// NOTE: live-FIRST for machine state is fresher than the design's
	// "prefer checkpoint when fresher" wording (which targets the
	// dead-coord recovery path); a manual handoff is the live case where
	// coord-state.json is authoritative.
	//
	// liveOK distinguishes "coord-state read fine, zero workers"
	// (authoritative empty → CLEAR the section) from "coord-state
	// unreadable" (keep the checkpoint baseline). Without this, a handoff
	// taken after all workers finished would keep stale checkpoint rows
	// and tell the successor to resume finished work. (codex Slice-1 P1.)
	if subs, liveOK := CollectActiveSubagentsLive(project); liveOK {
		doc.ActiveSubagents = subs // non-empty live rows OR authoritative empty (nil)
	}

	// OPEN PRS preference, most-authoritative (freshest) first:
	//   1. `gh pr list` (live, authoritative) — overwrite on success.
	//   2. tasks.md — when gh fails AND tasks.md is READABLE. tasks.md is
	//      updated live by the coord, so it is fresher than any checkpoint
	//      snapshot; a PR opened since the last checkpoint is captured.
	//      Terminal (done/abandoned) rows are excluded. CRUCIALLY, a
	//      readable tasks.md with ZERO open PRs is AUTHORITATIVE — we set
	//      OpenPRs to that empty result rather than reviving the
	//      checkpoint, so a handoff after the last PR merged doesn't
	//      resurrect stale shepherd URLs. (codex Slice-1 P1/P2.)
	//      handoff_resume.py keys the respawn off the row's trailing URL
	//      (regex `\s—\s(https?://\S+)$`); a Number=0 / title=slug row is
	//      sufficient — the URL is the only load-bearing field.
	//   3. checkpoint snapshot — when gh fails AND tasks.md is UNREADABLE
	//      (the only ambiguous case). Last-resort stale snapshot; already
	//      set into doc.OpenPRs by applyCheckpointToDoc above, so this is
	//      the implicit no-op fall-through.
	//   4. empty → _(no open PRs)_ placeholder.
	switch prs, ghOK := CollectOpenPRs(repoDir, logw); {
	case ghOK:
		doc.OpenPRs = prs
	default:
		if fallback, tasksOK := collectOpenPRsFromTasks(pdir); tasksOK {
			// Readable tasks.md is authoritative — use its result even when
			// empty (clears any stale checkpoint baseline).
			if logw != nil {
				logw(fmt.Sprintf("enrich: gh unavailable; using %d Open PR(s) from tasks.md (authoritative)", len(fallback)))
			}
			doc.OpenPRs = fallback
		}
		// else: tasks.md unreadable → keep the checkpoint baseline already
		// set by applyCheckpointToDoc (or the placeholder if no checkpoint).
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
// Tasks in a TERMINAL state (done / abandoned) are excluded even when
// they still carry a pr_url: those PRs are merged/closed, and
// handoff_resume.py would otherwise reintroduce them as live shepherd
// watches (codex Slice-1 P2). Non-terminal tasks with a pr_url
// (in-review / in-progress / blocked / etc.) are kept — any of them may
// have a still-open PR worth watching.
//
// Return contract — the bool is "tasks.md was DETERMINABLE
// (authoritative)":
//   - ok=true: tasks.md was read successfully (or is legitimately
//     absent → fresh project, no tasks). The returned slice is the
//     authoritative open-PR set, which may be empty (all PRs merged).
//     The caller uses it verbatim and does NOT revive a stale checkpoint.
//   - ok=false: tasks.md exists but is unreadable (parse error). The
//     caller may fall back to the checkpoint snapshot.
//
// A MISSING or unreadable tasks.md is ok=false (ambiguous — let the
// checkpoint snapshot stand if one exists). Only a tasks.md that was
// READ SUCCESSFULLY is authoritative, including when it yields zero open
// PRs (the case codex flagged: all PRs merged → don't revive stale
// checkpoint URLs). (codex Slice-1 P2.)
func collectOpenPRsFromTasks(pdir string) ([]OpenPR, bool) {
	path := filepath.Join(pdir, "tasks.md")
	if _, err := os.Stat(path); err != nil {
		return nil, false // missing / stat error → ambiguous
	}
	f, err := tasks.Read(path)
	if err != nil {
		return nil, false // parse error — ambiguous, let checkpoint stand
	}
	// Sort by slug for deterministic doc shape (matches the live walk +
	// synth ordering; operators benefit from stable ordering too).
	sort.Slice(f.Tasks, func(i, j int) bool { return f.Tasks[i].Slug < f.Tasks[j].Slug })
	var out []OpenPR
	for _, t := range f.Tasks {
		if t.PRURL == "" {
			continue
		}
		if t.Status == tasks.StatusDone || t.Status == tasks.StatusAbandoned {
			continue // terminal: PR merged/closed, no shepherd needed.
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
	return out, true
}
