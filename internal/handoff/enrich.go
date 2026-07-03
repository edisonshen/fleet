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
	"strings"
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
		// gh pr list defaults to the first 30 results; a busy project can
		// have more open worker PRs, and handoff_resume.py respawns
		// shepherds strictly from this section — a truncated page would
		// silently stop monitoring the overflow. Cap high enough to cover
		// any realistic fan-out. (codex Slice-1 P2.)
		"--limit", "500",
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

	agentIDsBySlug, keyPresent, err := readWorkerAgentIDsWithPresence(csPath)
	if err != nil {
		// Unreadable (malformed JSON) — let the caller fall back to the
		// checkpoint.
		return nil, false
	}
	if !keyPresent {
		// The `worker_agent_ids` key is ABSENT (legacy / partially
		// upgraded coord-state.json). We cannot prove "zero workers" — the
		// writer simply didn't emit the field — so this is NOT
		// authoritative. Fall back to the checkpoint snapshot. (codex
		// Slice-1 P2.)
		return nil, false
	}
	if len(agentIDsBySlug) == 0 {
		// Key PRESENT + zero workers → AUTHORITATIVE EMPTY (ok=true). The
		// coord has no in-flight work; clear the section rather than
		// reviving stale checkpoint rows. (codex Slice-1 P1.)
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
		// pr_url: a NON-EMPTY value from EITHER source means "a PR exists";
		// trust it. tasks.md wins when both are set; otherwise state.json
		// fills the unstamped window (PR opened before the coord stamped
		// tasks.md). This is the common lifecycle case the feature targets.
		//
		// Accepted edge (codex P1, deliberately not "fixed"): on a CI-red
		// REQUEUE, tasks.md's pr_url is cleared but state.json can briefly
		// carry the dead PR's URL, so we'd surface a closed PR. That's the
		// strictly less-harmful failure: a shepherd on a closed PR exits on
		// its first poll (self-correcting), whereas making EMPTY tasks.md
		// authoritatively clear state.json would DROP every just-opened PR
		// in the much more common "stamped-after-state.json" window —
		// re-dispatching duplicate work against a live PR. We optimize for
		// the common window; the requeue race self-heals within one tick.
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

// readWorkerAgentIDsWithPresence parses coord-state.json's
// worker_agent_ids map AND reports whether the key was present at all.
// The presence flag lets the live walk distinguish "key present, zero
// workers" (authoritative empty) from "key absent" (legacy / partially
// upgraded writer — NOT authoritative). The plain readWorkerAgentIDs
// (shared with synth) collapses both to an empty map, so we re-parse
// here rather than change synth's contract. Malformed JSON returns an
// error; a missing file returns (empty, false, nil).
func readWorkerAgentIDsWithPresence(path string) (map[string]string, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, false, nil
		}
		return nil, false, err
	}
	var raw struct {
		WorkerAgentIDs *map[string]string `json:"worker_agent_ids"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, false, fmt.Errorf("parse coord-state.json: %w", err)
	}
	if raw.WorkerAgentIDs == nil {
		// Key absent (or explicit null) → not present.
		return map[string]string{}, false, nil
	}
	return *raw.WorkerAgentIDs, true, nil
}

// taskMeta is the per-slug (status, pr_url) pair the live walk overlays
// from tasks.md — mirrors handoff.py:_read_tasks_meta's
// {slug: (status, pr_url)} sourcing so the manual path matches the auto
// path's tasks.md-as-source-of-truth for both fields.
type taskMeta struct {
	status string
	prURL  string
}

// readTaskMetaBySlug reads tasks.md and returns {slug: {status, pr_url}}
// using the SAME tolerant per-block scan as the Open PRs fallback
// (scanTasksMetaTolerant). Using the tolerant scan here — rather than the
// strict tasks.Read — keeps the Active Subagents overlay and the Open PRs
// fallback CONSISTENT: a schema drift / one malformed block must not make
// the live walk lose a slug's status+pr_url (→ re-dispatch) while the
// Open PRs section still recovers the PR (→ shepherd), which would
// duplicate resume work. (codex Slice-1 P2.)
//
// Best-effort: a missing/unreadable file returns an empty map and the
// caller falls back to state.json + the safe "re-dispatch" default.
func readTaskMetaBySlug(projectDir string) map[string]taskMeta {
	data, err := os.ReadFile(filepath.Join(projectDir, "tasks.md"))
	if err != nil {
		return map[string]taskMeta{}
	}
	return scanTasksMetaTolerant(data)
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
// SLICE 2 maps the verbatim-lifted MACHINE sections (ActiveSubagents,
// OpenPRs) plus the NARRATIVE `Completed (recent)` buffer -> doc.Completed.
// SLICE 3 adds the `recent_decisions` buffer -> doc.KeyDecisions (the
// buffer's correct home; it finally has a producer wired in loop.py).
// Each empty buffer leaves the doc's existing section text (Placeholder
// for synth / the manual stub). NextSteps + Open Questions are NOT touched
// here -- they come live from tasks.md via CollectNextSteps /
// CollectOpenQuestions, so a checkpoint lift must not overwrite them.
func applyCheckpointToDoc(doc *Doc, cp *checkpointDoc) {
	doc.ActiveSubagents = cp.activeSubagents
	doc.OpenPRs = cp.openPRs
	if body := renderCompletionBullets(cp.recentCompletions); body != "" {
		doc.Completed = body
	}
	if body := renderCompletionBullets(cp.recentDecisions); body != "" {
		doc.KeyDecisions = body
	}
}

// renderCompletionBullets joins a checkpoint buffer (recent_completions
// -> Completed, or recent_decisions -> Key Decisions) into a `- <line>`
// bullet list. Returns "" for an empty buffer so the caller keeps the
// existing placeholder (no empty-section artifact).
func renderCompletionBullets(completions []string) string {
	if len(completions) == 0 {
		return ""
	}
	var b strings.Builder
	for i, c := range completions {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString("- ")
		b.WriteString(c)
	}
	return b.String()
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
// Narrative sections are filled from durable state: Completed from the
// checkpoint lift, Key Decisions live-preferred from
// coord-state.json:recent_decisions, Docs (this session) from
// coord-state.json:session_docs. Any panic mid-build is recovered and
// swallowed — the doc keeps whatever sections were already filled
// (placeholders at worst).
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

	// KEY DECISIONS — LIVE-PREFERRED. `fleet checkpoint decision` writes
	// coord-state.json:recent_decisions out-of-band (no tick), so the
	// tick-published checkpoint the lift above used can lag a just-logged
	// agent rationale. Read the buffer LIVE and let it WIN when present —
	// mirrors the CollectActiveSubagentsLive live-overwrite pattern and
	// closes the "manual handoff before the next tick → blank Key Decisions"
	// gap. Empty live buffer → keep the checkpoint value (or placeholder).
	if body := renderCompletionBullets(CollectRecentDecisionsLive(filepath.Join(pdir, "coord-state.json"))); body != "" {
		doc.KeyDecisions = body
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
	liveSubs, liveOK := CollectActiveSubagentsLive(project)
	if liveOK {
		doc.ActiveSubagents = liveSubs // non-empty live rows OR authoritative empty (nil)
	}

	// OPEN PRS preference, most-authoritative (freshest) first:
	//   1. `gh pr list` (live, authoritative) — overwrite on success.
	//   2. gh fails → derive from the LIVE Active Subagents' pr_urls. Those
	//      rows already merge state.json + tasks.md pr_url (and promote),
	//      so this captures a PR written to state.json before tasks.md was
	//      stamped — the window where a plain tasks.md read would drop it.
	//      (codex Slice-1 P2.) Terminal rows carry no pr_url worth
	//      shepherding here because the live walk only emits in-flight
	//      slugs. When the live walk is authoritative (liveOK) this result
	//      is authoritative too — including empty (clears stale checkpoint
	//      PRs). handoff_resume.py keys the respawn off the row's trailing
	//      URL; a Number=0 / title=slug row is sufficient.
	//   3. gh fails AND live walk NOT authoritative → readable tasks.md, if
	//      any; else the checkpoint snapshot (already set by
	//      applyCheckpointToDoc) stands as the last-resort stale snapshot.
	//   4. empty → _(no open PRs)_ placeholder.
	if prs, ghOK := CollectOpenPRs(repoDir, logw); ghOK {
		doc.OpenPRs = prs
	} else {
		// gh unavailable. tasks.md is the ONLY local source that covers
		// open PRs for tasks already moved to in-review and DROPPED from
		// coord-state.json's worker map (the `## Open PRs` section exists
		// precisely for these). So:
		//
		//   tasksOK (tasks.md readable) → tasks.md + live subagent pr_urls
		//     is the COMPLETE authoritative snapshot. Replace doc.OpenPRs
		//     with the merge (clears stale checkpoint PRs). Subagents add
		//     the state.json-before-tasks.md window. (codex Slice-1 P2.)
		//
		//   !tasksOK (tasks.md missing/corrupt) → we CANNOT see the
		//     in-review-dropped PRs from local state; the checkpoint
		//     snapshot may still hold them. So KEEP the checkpoint baseline
		//     (already set by applyCheckpointToDoc) and UNION in the live
		//     subagent pr_urls — never wipe the checkpoint. (codex Slice-1
		//     P2.)
		subagentPRs := openPRsFromSubagents(liveSubs)
		tasksPRs, tasksOK := collectOpenPRsFromTasks(pdir)
		switch {
		case tasksOK && liveOK:
			// BOTH local sources determinable → tasks.md + live subagents is
			// the COMPLETE authoritative snapshot. Replace doc.OpenPRs
			// (clears stale checkpoint PRs). (codex Slice-1 P2.)
			merged := mergeOpenPRsByURL(tasksPRs, subagentPRs)
			if logw != nil {
				logw(fmt.Sprintf("enrich: gh unavailable; using %d Open PR(s) from tasks.md+live state (authoritative)", len(merged)))
			}
			doc.OpenPRs = merged
		case tasksOK && !liveOK:
			// tasks.md readable BUT the live walk failed (coord-state
			// unreadable) → tasks.md alone is NOT complete; it can lag
			// state.json/checkpoint on a just-opened PR. UNION tasks.md +
			// subagents WITH the checkpoint baseline instead of replacing,
			// so a checkpoint-only PR isn't dropped. (codex Slice-1 P2.)
			merged := mergeOpenPRsByURL(doc.OpenPRs, mergeOpenPRsByURL(tasksPRs, subagentPRs))
			if logw != nil {
				logw(fmt.Sprintf("enrich: gh unavailable + live state incomplete; checkpoint ∪ tasks.md ∪ live = %d Open PR(s)", len(merged)))
			}
			doc.OpenPRs = merged
		case len(subagentPRs) > 0:
			// tasks.md undeterminable → supplement (don't replace) the
			// checkpoint baseline with live subagent PRs.
			merged := mergeOpenPRsByURL(doc.OpenPRs, subagentPRs)
			if logw != nil {
				logw(fmt.Sprintf("enrich: gh + tasks.md unavailable; checkpoint baseline + %d live PR(s)", len(subagentPRs)))
			}
			doc.OpenPRs = merged
		}
		// else: gh + tasks.md undeterminable AND no live PRs → keep the
		// checkpoint baseline already set by applyCheckpointToDoc (or the
		// placeholder if no checkpoint).
	}

	// NARRATIVE: Next Steps + Open Questions come LIVE from tasks.md (always
	// fresh, fill even when the coord died before its first checkpoint). Both
	// never-error: parse drift / missing file → empty → keep the placeholder.
	// Completed is set above by the checkpoint lift (applyCheckpointToDoc);
	// Key Decisions is set live above.
	tasksPath := filepath.Join(pdir, "tasks.md")
	if ns := CollectNextSteps(tasksPath); ns != "" {
		doc.NextSteps = ns
	}
	if oq := CollectOpenQuestions(tasksPath); oq != "" {
		doc.OpenQuestions = oq
	}
	// Docs (this session): the plan docs THIS coord recorded via
	// `fleet checkpoint doc`, read from coord-state.json:session_docs (NOT a
	// git shell-out — the retired CollectFilesModified dumped every past
	// coord's untracked docs). Same live coord-state source as the decisions
	// buffer above; best-effort → empty keeps the placeholder. synth.go reads
	// the same source, so recovery now fills this section too.
	if sd := CollectSessionDocs(filepath.Join(pdir, "coord-state.json")); sd != "" {
		doc.SessionDocs = sd
	}
}

// readTasksTolerant wraps the strict tasks.Read so a parse error or a
// missing file degrades to an empty task list instead of failing the
// handoff. The collectors accept "lose the section on parse drift" rather
// than build a second tolerant scanner (DESIGN Slice 2 (b)) — the handoff
// must never fail on enrichment, and a malformed tasks.md is rare (atomic
// tmp+rename means readers see a whole old-or-new file).
func readTasksTolerant(tasksFile string) []*tasks.Task {
	f, err := tasks.Read(tasksFile)
	if err != nil || f == nil {
		return nil
	}
	return f.Tasks
}

// priorityRank maps a tasks.Priority to a sort rank where SMALLER == more
// urgent (P0=0 … P3=3). Unknown / empty priorities sort last (rank 4) so a
// malformed row never jumps the queue.
func priorityRank(p tasks.Priority) int {
	switch p {
	case tasks.PriorityP0:
		return 0
	case tasks.PriorityP1:
		return 1
	case tasks.PriorityP2:
		return 2
	case tasks.PriorityP3:
		return 3
	default:
		return 4
	}
}

// nextStepsLimit caps the Next Steps list so a huge backlog doesn't bloat
// the handoff doc. The successor sees the top-N most-urgent queued tasks.
const nextStepsLimit = 10

// CollectNextSteps renders the `## Next Steps (prioritized)` body live from
// tasks.md: the top-N ready/todo tasks (NOT in-progress — those appear
// under Active Subagents / Open PRs, listing them here double-lists),
// ordered priority-descending then by age (oldest first), each rendered
// `- [P{n}] <slug>: <goal>` where goal = first non-blank line of Spec,
// trimmed, ≤80 chars; an empty Spec renders the slug alone (no trailing
// colon). Never errors: parse drift / missing file → "" (caller keeps the
// placeholder). There is no Goal field on tasks.Task — Spec is the source.
func CollectNextSteps(tasksFile string) string {
	all := readTasksTolerant(tasksFile)
	var queued []*tasks.Task
	for _, t := range all {
		if t.Status == tasks.StatusReady || t.Status == tasks.StatusTodo {
			queued = append(queued, t)
		}
	}
	if len(queued) == 0 {
		return ""
	}
	// priority-desc (P0 first) then age (oldest Created first). Stable so
	// equal keys keep tasks.md order.
	sort.SliceStable(queued, func(i, j int) bool {
		ri, rj := priorityRank(queued[i].Priority), priorityRank(queued[j].Priority)
		if ri != rj {
			return ri < rj
		}
		return queued[i].Created.Before(queued[j].Created)
	})
	if len(queued) > nextStepsLimit {
		queued = queued[:nextStepsLimit]
	}
	var b strings.Builder
	for i, t := range queued {
		if i > 0 {
			b.WriteByte('\n')
		}
		prio := string(t.Priority)
		if prio == "" {
			prio = "P?"
		}
		goal := specFirstLine(t.Spec)
		if goal == "" {
			fmt.Fprintf(&b, "- [%s] %s", prio, t.Slug)
		} else {
			fmt.Fprintf(&b, "- [%s] %s: %s", prio, t.Slug, goal)
		}
	}
	return b.String()
}

// specFirstLine returns the first non-blank line of spec, trimmed and
// truncated to 80 chars. Empty spec → "".
func specFirstLine(spec string) string {
	for _, line := range strings.Split(spec, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if len(trimmed) > 80 {
			trimmed = trimmed[:80]
		}
		return trimmed
	}
	return ""
}

// CollectOpenQuestions renders the `## Open Questions` body live from
// tasks.md: tasks where Status == "blocked" OR Parked != "" (there is no
// `parked` status — Parked is a separate free-form field). A parked task
// renders its Parked text; a blocked task has no structured reason in
// tasks.md (it lives in coord sentinels) so it renders reason-less:
// `- <slug>: blocked`. A task both blocked AND parked renders the Parked
// text. Ordered by slug for a deterministic doc. Never errors: parse
// drift / missing file → "" (caller keeps the placeholder).
func CollectOpenQuestions(tasksFile string) string {
	all := readTasksTolerant(tasksFile)
	var picked []*tasks.Task
	for _, t := range all {
		if t.Status == tasks.StatusBlocked || t.Parked != "" {
			picked = append(picked, t)
		}
	}
	if len(picked) == 0 {
		return ""
	}
	sort.SliceStable(picked, func(i, j int) bool {
		return picked[i].Slug < picked[j].Slug
	})
	var b strings.Builder
	for i, t := range picked {
		if i > 0 {
			b.WriteByte('\n')
		}
		reason := "blocked"
		if t.Parked != "" {
			reason = strings.TrimSpace(strings.ReplaceAll(t.Parked, "\n", " "))
		}
		fmt.Fprintf(&b, "- %s: %s", t.Slug, reason)
	}
	return b.String()
}

// mergeOpenPRsByURL concatenates two OpenPR slices, dropping duplicates
// by URL (the load-bearing identity). tasks.md rows come first so their
// status-filtered, terminal-excluded set wins on collision; subagent
// rows add only URLs tasks.md didn't already carry. Order within each
// source is preserved (both are slug-sorted upstream). Returns nil for
// two empty inputs so the caller renders the _(no open PRs)_ placeholder.
func mergeOpenPRsByURL(primary, secondary []OpenPR) []OpenPR {
	seen := make(map[string]bool, len(primary)+len(secondary))
	var out []OpenPR
	for _, group := range [][]OpenPR{primary, secondary} {
		for _, pr := range group {
			if pr.URL == "" || seen[pr.URL] {
				continue
			}
			seen[pr.URL] = true
			out = append(out, pr)
		}
	}
	return out
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
// openPRsFromSubagents derives Open PR rows from the live Active
// Subagent rows that carry a pr_url. Used as the gh-failure fallback
// when the live walk is authoritative: those rows are the freshest local
// signal (state.json + tasks.md pr_url merged + promoted), so they
// capture a PR opened before tasks.md was stamped. Partial rows (Number
// 0, title=slug) — the URL is the only field handoff_resume.py needs.
// Terminal-status rows (done/abandoned) are excluded — their PR is
// merged/closed, so a shepherd respawn would watch a dead PR (matches
// collectOpenPRsFromTasks's terminal filter). Returns nil when no
// non-terminal subagent carries a pr_url.
func openPRsFromSubagents(subs []ActiveSubagent) []OpenPR {
	var out []OpenPR
	for _, s := range subs {
		if s.PRURL == "" {
			continue
		}
		if s.Status == string(tasks.StatusDone) || s.Status == string(tasks.StatusAbandoned) {
			continue
		}
		head := s.Branch
		if head == "" {
			head = "worker/" + s.TaskID
		}
		out = append(out, OpenPR{
			Number:      0,
			Title:       s.TaskID,
			HeadRefName: head,
			URL:         s.PRURL,
		})
	}
	return out
}

// Return contract — the bool is "tasks.md was DETERMINABLE
// (authoritative)":
//   - ok=true: tasks.md was read successfully (or is legitimately
//     absent → fresh project, no tasks). The returned slice is the
//     authoritative open-PR set, which may be empty (all PRs merged).
//     The caller uses it verbatim and does NOT revive a stale checkpoint.
//   - ok=false: tasks.md exists but is unreadable (parse error). The
//     caller may fall back to the checkpoint snapshot.
//
// A MISSING tasks.md is ok=false (ambiguous — let the checkpoint stand).
// A PRESENT tasks.md is parsed with a TOLERANT scan (NOT tasks.Read):
// this fallback is the last local source for in-review PRs already
// dropped from worker_agent_ids, so a schema-too-new binary, a partial
// write, or one malformed block must NOT make the whole file unreadable
// — we still recover every well-formed `## task:` block's status/pr_url.
// This mirrors handoff.py:_read_tasks_meta's deliberate per-block
// tolerance.
//
// AUTHORITATIVE only when the scan parsed AT LEAST ONE `## task:` block.
// A readable file that yields ZERO blocks is ambiguous — it could be a
// truncated / stray-`##`-header / partial write rather than a genuinely
// empty task list — so we return ok=false and let the checkpoint
// snapshot stand rather than wiping it to an empty PR set. (A fresh empty
// project has no checkpoint PRs to preserve anyway.) When ≥1 block
// parsed, the result IS authoritative, including zero open PRs (all
// merged → don't revive stale checkpoint URLs). (codex Slice-1 P2.)
func collectOpenPRsFromTasks(pdir string) ([]OpenPR, bool) {
	path := filepath.Join(pdir, "tasks.md")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false // missing / read error → ambiguous
	}
	meta := scanTasksMetaTolerant(data)
	if len(meta) == 0 {
		return nil, false // no task block parsed → ambiguous, keep checkpoint
	}
	slugs := make([]string, 0, len(meta))
	for slug := range meta {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs) // deterministic doc shape
	var out []OpenPR
	for _, slug := range slugs {
		m := meta[slug]
		if m.prURL == "" {
			continue
		}
		if m.status == string(tasks.StatusDone) || m.status == string(tasks.StatusAbandoned) {
			continue // terminal: PR merged/closed, no shepherd needed.
		}
		out = append(out, OpenPR{
			Number:      0, // unknown from tasks.md; URL is the load-bearing field.
			Title:       slug,
			HeadRefName: "worker/" + slug,
			URL:         m.prURL,
		})
	}
	return out, true
}

// scanTasksMetaTolerant extracts {slug: {status, pr_url}} from tasks.md
// bytes with a hand-rolled per-block scan that survives schema drift /
// partial writes — a port of handoff.py:_read_tasks_meta. It reads only
// the `- status:` / `- pr_url:` bullets inside each `## task: <slug>`
// block's leading bullet section (before the first blank line / `### `
// header), so prose bullets in Spec/Acceptance/Notes are never mistaken
// for task fields. Unparseable lines are skipped, never fatal.
func scanTasksMetaTolerant(data []byte) map[string]taskMeta {
	out := map[string]taskMeta{}
	var slug, status, prURL string
	// The task FIELDS are the `- key: val` bullets BEFORE the first `### `
	// H3 (Spec/Acceptance/Notes). A blank line is only a soft pause — a
	// hand edit or future writer may split the field bullets across one,
	// so we do NOT latch on blank lines (matches handoff.py:_read_tasks_
	// meta, which reopens). We DO latch permanently on `### ` so prose
	// bullets under a body header are never mis-read as fields. (codex
	// Slice-1 P2 — both the prose-bullet and the blank-line cases.)
	pastFields := false
	// Code-fence safety: a `## task:` / `## ` / `### ` / `- ` line INSIDE
	// a ```-fenced example (e.g. a Spec block pasting markdown) is body
	// content, never structural. internal/tasks allows these fenced
	// headings, so we must track fences identically or we'd mis-detect a
	// fake task / truncate the file. Toggle on a column-0 ``` marker
	// (matches internal/tasks.isFenceMarker: exactly 3 backticks + tag).
	inFence := false
	flush := func() {
		if slug != "" {
			out[slug] = taskMeta{status: status, prURL: prURL}
		}
	}
	for _, raw := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		// Fence handling is scoped to a task's `### `-introduced BODY
		// (pastFields), exactly like tasks.go:readSection: fences only
		// protect prose INSIDE a body section. While in a body fence, a
		// `## Example` / `### Foo` / `- pr_url:` line is body, never
		// structural (codex P2). Outside a body (the leading-field region
		// or between blocks), `## task:` / `## ` are ALWAYS structural —
		// and a `## task:` additionally RESETS fence state, so a
		// malformed/unclosed fence in one block's body cannot swallow a
		// later real task block (the Claude-adversarial P1). Net: a closed
		// fenced heading is body; an unclosed fence is bounded to its block.
		// `## task: ` is the task-block delimiter — ALWAYS structural and
		// ALWAYS resets fence state, even inside a body fence. This bounds
		// a malformed/unclosed fence to its own block so a later REAL task
		// is never swallowed (Claude-adversarial P1). A fenced
		// `## task: <x>` *example* is rare and yields only a benign phantom
		// row (no WIP → skipped on resume); silently dropping a real task's
		// open PR is the worse failure.
		if strings.HasPrefix(raw, "## task: ") {
			flush()
			slug = strings.TrimSpace(strings.TrimPrefix(raw, "## task: "))
			status, prURL, pastFields, inFence = "", "", false, false
			continue
		}
		// Within a `### `-introduced body, honor fences so a CLOSED fenced
		// generic heading (`## Example`, `### Foo`) or `- ` prose bullet is
		// treated as prose, not structure (codex P2). Fences are only
		// tracked in the body region — the leading-field region has none.
		if pastFields && strings.HasPrefix(raw, "```") && !strings.HasPrefix(raw[3:], "`") {
			inFence = !inFence
			continue
		}
		if pastFields && inFence {
			continue // inside a body fence — pure prose, skip
		}
		if strings.HasPrefix(raw, "## ") {
			// Non-task H2 (footer) — flush + stop.
			flush()
			slug = ""
			break
		}
		if slug == "" {
			continue
		}
		if strings.HasPrefix(raw, "### ") {
			pastFields = true // H3 body section — no more task fields
			continue
		}
		if pastFields || !strings.HasPrefix(raw, "- ") {
			continue
		}
		body := raw[2:]
		colon := strings.IndexByte(body, ':')
		if colon < 0 {
			continue
		}
		key := strings.TrimSpace(body[:colon])
		val := strings.TrimSpace(body[colon+1:])
		if val == "null" {
			val = ""
		}
		switch key {
		case "status":
			status = val
		case "pr_url":
			prURL = val
		}
	}
	flush() // EOF flush
	return out
}
