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
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tasks"
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
//
// Legacy entry point — preserves the pre-checkpoint signature for
// callers that don't track a last-handoff path. Equivalent to
// SynthesizeRecoveryWithLastHandoff(agentID, project, "", ts).
func SynthesizeRecovery(agentID, project string, ts time.Time) (*Doc, error) {
	return SynthesizeRecoveryWithLastHandoff(agentID, project, "", ts)
}

// SynthesizeRecoveryWithLastHandoff is SynthesizeRecovery plus a
// preference for the rolling coord-checkpoint.md doc when it exists and
// is newer than lastHandoffPath. The rolling checkpoint is written by
// the coord skill every N ticks (dispatch.write_coord_checkpoint /
// FLEET_COORD_CHECKPOINT_EVERY) and bounds the recovery window between
// fleet-guard's 50%/70% handoffs.
//
// Preference order:
//
//  1. If coord-checkpoint.md exists AND its updated_at is newer than
//     lastHandoffPath's filename-encoded timestamp (or lastHandoffPath
//     == "" — no clean handoff has ever been recorded), copy the
//     checkpoint's Active Subagents + Open PRs + Recent decisions into
//     the synth doc verbatim.
//  2. Otherwise fall through to the legacy coord-state.json + worker
//     state.json walk.
//
// Without the checkpoint preference, a coord that crashed seconds after
// a worker dispatch would recover from the last handoff doc (hours
// stale) instead of the checkpoint (~2.5min stale on defaults).
func SynthesizeRecoveryWithLastHandoff(
	agentID, project, lastHandoffPath string, ts time.Time,
) (*Doc, error) {
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

	// Checkpoint preference: when present + fresher than the last
	// handoff doc, lift its body sections wholesale. The recovery window
	// between clean handoffs is thus bounded by the checkpoint interval
	// rather than the (potentially hours-long) gap between fleet-guard's
	// context-pct triggers.
	if cp, ok := loadCheckpointIfFresher(pdir, lastHandoffPath); ok {
		doc.ActiveSubagents = cp.activeSubagents
		doc.OpenPRs = cp.openPRs
		// Recent decisions surface in the NextSteps body as a free-form
		// bullet list. handoff_resume.py doesn't parse them structurally
		// — they're operator-readable context for "what was the dead
		// coord up to". Dropping them into NextSteps keeps the existing
		// doc shape (and parser) unchanged.
		if len(cp.recentDecisions) > 0 {
			var b strings.Builder
			b.WriteString("Recent coord decisions (from checkpoint):\n")
			for _, d := range cp.recentDecisions {
				b.WriteString("- ")
				b.WriteString(d)
				b.WriteString("\n")
			}
			doc.NextSteps = strings.TrimRight(b.String(), "\n")
		}
		return doc, nil
	}

	agentIDsBySlug, err := readWorkerAgentIDs(filepath.Join(pdir, "coord-state.json"))
	if err != nil {
		return nil, fmt.Errorf("read coord-state.json: %w", err)
	}
	if len(agentIDsBySlug) == 0 {
		// Empty project — no in-flight work. Doc still renders cleanly
		// via the _(none)_ placeholder for Active Subagents.
		return doc, nil
	}

	// Pull per-slug status from tasks.md (codex review iter-3 P2). The
	// worker state.json doesn't carry tasks.md's status enum — that's
	// authoritative only in tasks.md. Without this lookup, every synth
	// row carries Status="", which handoff_resume.py treats as the
	// legacy "always re-dispatch" case. Recovering a coord with tasks
	// already in-review / done / blocked would then reissue workers
	// that should have been skipped.
	statusBySlug := readStatusBySlug(pdir, project)

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
		// Overlay tasks.md status when present. Empty when (a) tasks.md
		// doesn't exist (fresh project before any /tasks add), (b) the
		// slug isn't in tasks.md (e.g., the coord registered the slug
		// in coord-state.json but crashed before adding to tasks.md),
		// or (c) tasks.md couldn't be parsed. In all three cases, the
		// successor coord's handoff_resume.py falls back to the legacy
		// "re-dispatch" path, which is the safe default — we'd rather
		// re-issue a worker than silently skip one.
		if st, ok := statusBySlug[slug]; ok {
			sub.Status = st
		}
		doc.ActiveSubagents = append(doc.ActiveSubagents, sub)
	}

	return doc, nil
}

// readStatusBySlug reads tasks.md and returns slug→status as a map.
// Returns an empty map on any error (missing file, parse failure) —
// the caller treats absent entries as "unknown status" and falls back
// to the safe default of re-dispatch. We deliberately swallow errors
// here rather than fail the whole recovery: tasks.md may legitimately
// be missing (fresh project), partially written (coord crashed
// mid-write), or schema-incompatible (binary downgrade).
func readStatusBySlug(projectDir, project string) map[string]string {
	out := map[string]string{}
	path := filepath.Join(projectDir, "tasks.md")
	if _, err := os.Stat(path); err != nil {
		// Missing file → empty map. Not an error: legitimate when no
		// tasks were ever added.
		if errors.Is(err, os.ErrNotExist) {
			return out
		}
		return out
	}
	f, err := tasks.Read(path)
	if err != nil {
		// Parse error → empty map. Successor coord re-dispatches all
		// workers (legacy fallback), which is safer than refusing to
		// build the recovery doc.
		return out
	}
	for _, t := range f.Tasks {
		out[t.Slug] = string(t.Status)
	}
	return out
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

// ----------------------------------------------------------------------
// Rolling checkpoint preference
// ----------------------------------------------------------------------

// checkpointDoc is the in-memory shape of a parsed coord-checkpoint.md —
// only the fields the synth path lifts wholesale: Active Subagents
// (parsed via the same key="value" scanner as the handoff doc), Open
// PRs (`- #N title — head — url` bullets), and Recent decisions
// (free-form bullet text).
type checkpointDoc struct {
	updatedAt       time.Time
	activeSubagents []ActiveSubagent
	openPRs         []OpenPR
	recentDecisions []string
}

// loadCheckpointIfFresher reads `<pdir>/coord-checkpoint.md` and returns
// the parsed checkpoint when its updated_at is strictly newer than
// lastHandoffPath's filename-encoded timestamp (or when lastHandoffPath
// is empty — no clean handoff has ever been written, so any checkpoint
// wins).
//
// Returns ok=false when:
//   - the checkpoint file doesn't exist (fresh-project case before the
//     coord's first N-tick boundary),
//   - the checkpoint's frontmatter is malformed (refuse to lift from a
//     half-written / schema-drifted doc),
//   - the checkpoint pre-dates the last handoff (the handoff doc is
//     fresher, and the successor's handoff_resume.py re-reads state from
//     it anyway; shadowing with older data would regress).
//
// Best-effort: any I/O error returns ok=false; the caller falls through
// to the state.json walk, which is the safe default.
func loadCheckpointIfFresher(pdir, lastHandoffPath string) (*checkpointDoc, bool) {
	path := filepath.Join(pdir, "coord-checkpoint.md")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	cp, ok := parseCheckpoint(data)
	if !ok {
		return nil, false
	}
	// Empty path → checkpoint always wins (no handoff baseline to
	// undercut).
	if lastHandoffPath == "" {
		return cp, true
	}
	// Compare against the timestamp encoded in lastHandoffPath's
	// filename (`<id>-YYYYMMDD-HHMMSS-rnd.md`). The filename stamp is the
	// load-bearing freshness signal for the previous_handoff chain; mtime
	// is unreliable because a doc claiming to reflect state from `now-1h`
	// is os.WriteFile'd at `now` in setup/recovery. Filename-parse
	// failure → fall back to the file's mtime; stat ENOENT → treat as
	// missing and let the checkpoint win.
	handoffTime, ok := parseHandoffFilenameTimestamp(lastHandoffPath)
	if !ok {
		info, err := os.Stat(lastHandoffPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return cp, true
			}
			// Other stat errors (permission, EIO) — bias toward the
			// state.json walk; we can't trust either timestamp.
			return nil, false
		}
		handoffTime = info.ModTime().UTC()
	}
	// Strictly newer wins. Equal timestamps lose: a tied checkpoint is no
	// fresher than the handoff that already records the same state, and
	// we'd rather keep the well-tested state.json walk than risk subtle
	// drift in the section-body translation.
	if cp.updatedAt.After(handoffTime) {
		return cp, true
	}
	return nil, false
}

// parseHandoffFilenameTimestamp pulls the `YYYYMMDD-HHMMSS` stamp out of
// a handoff doc filename produced by state.HandoffPath: the layout is
// `<agent-id>-<YYYYMMDD>-<HHMMSS>-<4hex>.md`. Returns the parsed UTC
// time on success; ok=false on anything that doesn't match (lets the
// caller fall back to mtime).
func parseHandoffFilenameTimestamp(path string) (time.Time, bool) {
	name := filepath.Base(path)
	name = strings.TrimSuffix(name, ".md")
	parts := strings.Split(name, "-")
	// Need at least: <id> <YYYYMMDD> <HHMMSS> <rnd> → 4 segments.
	if len(parts) < 4 {
		return time.Time{}, false
	}
	// The random suffix is at the tail; the two segments before it are
	// date + time. Pulling from the end is robust to agent IDs that
	// might contain hyphens.
	dateStr := parts[len(parts)-3]
	timeStr := parts[len(parts)-2]
	stamp := dateStr + "-" + timeStr
	ts, err := time.ParseInLocation("20060102-150405", stamp, time.UTC)
	if err != nil {
		return time.Time{}, false
	}
	return ts, true
}

// parseCheckpoint walks the checkpoint markdown doc and pulls out the
// frontmatter's updated_at + the three body sections we lift verbatim.
// Must match dispatch.write_coord_checkpoint's output.
//
// Returns ok=false on missing frontmatter or unparseable updated_at —
// the synth path falls back to the state.json walk in that case.
func parseCheckpoint(data []byte) (*checkpointDoc, bool) {
	body := string(data)
	if !strings.HasPrefix(body, "---\n") {
		return nil, false
	}
	end := strings.Index(body[4:], "\n---\n")
	if end < 0 {
		return nil, false
	}
	fm := body[4 : 4+end]
	rest := body[4+end+len("\n---\n"):]

	parsed := map[string]string{}
	for _, line := range strings.Split(fm, "\n") {
		k, v, ok := splitCheckpointFrontmatterKV(line)
		if !ok {
			continue
		}
		parsed[k] = v
	}
	updatedRaw, ok := parsed["updated_at"]
	if !ok {
		return nil, false
	}
	updated, err := time.Parse(time.RFC3339, updatedRaw)
	if err != nil {
		return nil, false
	}

	sections := splitCheckpointH3Sections(rest)
	cp := &checkpointDoc{updatedAt: updated.UTC()}
	if raw, ok := sections["Active Subagents"]; ok {
		cp.activeSubagents = parseCheckpointActiveSubagents(raw)
	}
	if raw, ok := sections["Open PRs"]; ok {
		cp.openPRs = parseCheckpointOpenPRs(raw)
	}
	if raw, ok := sections["Recent decisions"]; ok {
		cp.recentDecisions = parseCheckpointDecisions(raw)
	}
	return cp, true
}

// splitCheckpointFrontmatterKV parses one `key: value` line of the
// checkpoint's YAML-ish frontmatter. Values may be bare or `"..."`-
// quoted; the surrounding quotes are stripped when present. Empty /
// separator lines return ok=false.
func splitCheckpointFrontmatterKV(line string) (string, string, bool) {
	idx := strings.Index(line, ":")
	if idx < 0 {
		return "", "", false
	}
	k := strings.TrimSpace(line[:idx])
	v := strings.TrimSpace(line[idx+1:])
	if k == "" {
		return "", "", false
	}
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		if uq, err := strconv.Unquote(v); err == nil {
			v = uq
		} else {
			v = v[1 : len(v)-1]
		}
	}
	return k, v, true
}

// splitCheckpointH3Sections returns a map from section heading text
// (with the `### ` prefix stripped) to the body between that heading and
// the next `### `. Bodies preserve internal whitespace but strip
// surrounding blank lines.
func splitCheckpointH3Sections(body string) map[string]string {
	out := map[string]string{}
	scanner := bufio.NewScanner(strings.NewReader(body))
	// Allow long lines — section bodies can carry many subagent rows.
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	var current string
	var cur []string
	flush := func() {
		if current == "" {
			return
		}
		out[current] = strings.TrimSpace(strings.Join(cur, "\n"))
		cur = nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "### ") {
			flush()
			current = strings.TrimSpace(strings.TrimPrefix(line, "### "))
			continue
		}
		if current == "" {
			continue
		}
		cur = append(cur, line)
	}
	flush()
	return out
}

// parseCheckpointActiveSubagents reuses parseActiveSubagentLine — the
// checkpoint emits the same 7-field key="value" shape as the handoff
// doc. The `_(none)_` placeholder returns nil so the synth path renders
// the same placeholder in its doc body.
func parseCheckpointActiveSubagents(body string) []ActiveSubagent {
	if strings.TrimSpace(body) == ActiveSubagentsNonePlaceholder {
		return nil
	}
	var out []ActiveSubagent
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		entry, ok := parseActiveSubagentLine(trimmed)
		if !ok {
			continue
		}
		out = append(out, entry)
	}
	return out
}

// parseCheckpointOpenPRs walks the `### Open PRs` body and pulls out the
// `- #<number> <title> — <head> — <url>` bullets — the inverse of
// handoff.go's renderOpenPRs. Malformed lines are skipped silently:
// dropping one PR beats refusing the whole recovery.
func parseCheckpointOpenPRs(body string) []OpenPR {
	if strings.TrimSpace(body) == OpenPRsNonePlaceholder {
		return nil
	}
	var out []OpenPR
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "- #") {
			continue
		}
		rest := strings.TrimPrefix(trimmed, "- #")
		// rest: "<number> <title> — <head> — <url>"
		spaceIdx := strings.IndexByte(rest, ' ')
		if spaceIdx < 0 {
			continue
		}
		number, err := strconv.Atoi(rest[:spaceIdx])
		if err != nil {
			continue
		}
		tail := rest[spaceIdx+1:]
		// Split on " — " (em dash with surrounding spaces): title, head,
		// url. Three parts expected — anything else is malformed.
		parts := strings.SplitN(tail, " — ", 3)
		if len(parts) != 3 {
			continue
		}
		out = append(out, OpenPR{
			Number:      number,
			Title:       parts[0],
			HeadRefName: parts[1],
			URL:         parts[2],
		})
	}
	return out
}

// parseCheckpointDecisions extracts the `- <text>` bullets from the
// Recent decisions section. Empty / placeholder bodies return nil.
func parseCheckpointDecisions(body string) []string {
	if strings.TrimSpace(body) == "_(no recent decisions)_" {
		return nil
	}
	var out []string
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "- ") {
			continue
		}
		out = append(out, strings.TrimPrefix(trimmed, "- "))
	}
	return out
}
