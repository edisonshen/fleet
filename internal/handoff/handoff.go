// Package handoff renders and writes handoff documents to
// ~/.fleet/handoffs/<id>-<utc-stamp>.md.
//
// The doc structure mirrors docs/DESIGN.md "Handoff Doc Structure":
// frontmatter with chain fields (agent_id, task_id, project,
// previous_handoff, handoff_number, timestamp, handoff_type,
// context_pct_at_handoff) followed by five sections (Completed, Key
// Decisions, Files Modified, Open Questions, Next Steps).
//
// Week 4a writes operator-triggered stubs via NewManualStub — the
// agent never got a HANDOFF REQUESTED injection, so the body sections
// are placeholders the operator (or fresh agent) fills in. Week 4b/c
// will build Docs with real bodies from skill-side context.
package handoff

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/edisonshen/fleet/internal/state"
)

// ResumePrompt is the first-turn prompt typed into a freshly spawned
// replacement so it picks up its predecessor's work without operator
// intervention. Both the operator-triggered handoff path
// (cmd/fleet/handoff.go) and the auto-handoff drain
// (internal/handoffop) build this from the doc path so the format
// stays identical across entry points.
//
// Path reference (not inlined doc body) keeps the prompt to one line —
// no escape headaches for tmux send-keys, no size cap on the doc, and
// the doc on disk remains the single source of truth if the operator
// later edits it before the agent's first read tool call.
//
// Empty docPath returns "" so callers can pass the result straight
// into spawn.SendInitialPrompt; the helper treats an empty prompt as
// a silent no-op.
func ResumePrompt(docPath string) string {
	if docPath == "" {
		return ""
	}
	return "Read your handoff doc at " + docPath +
		" and continue the task. Do not wait for further operator input."
}

// Placeholder is the body text for stub sections in operator-triggered
// handoffs. Exported so tests and the dispatch --from-handoff loader
// can recognize "this section was never filled in."
const Placeholder = "_(operator-triggered handoff — fill in before resuming)_"

// Type values for the frontmatter handoff_type field.
const (
	TypeManual     = "manual"      // 4a: operator hit `fleet handoff`
	TypeAutoYellow = "auto-yellow" // 4b/c: ≥50% context, doing modes
	TypeAutoRed    = "auto-red"    // 4b/c: ≥70% context, doing modes
	TypePreCompact = "precompact"  // 4b/c: PreCompact hook fired
)

// Doc is a fully-populated handoff document ready to render.
//
// PreviousPath is nil for the first handoff on a task.
// ContextPctAtHandoff is nil when the handoff was operator-triggered
// without a context measurement (4a's path).
//
// ActiveSubagents (issue #93 Phase B2) is the list of in-flight worker
// subagents the outgoing coord had spawned via the Agent tool. Empty
// when the agent isn't a coord (worker handoffs / single-agent
// handoffs) or when the coord has no in-flight workers. The successor
// coord parses this section on its first turn and re-dispatches each
// worker whose WIP file still exists.
type Doc struct {
	AgentID             string
	TaskID              string
	Project             string
	Type                string
	Number              int
	PreviousPath        *string
	ContextPctAtHandoff *float64
	Timestamp           time.Time

	Completed       string
	KeyDecisions    string
	FilesModified   string
	OpenQuestions   string
	NextSteps       string
	ActiveSubagents []ActiveSubagent
	// OpenPRs is the snapshot of open worker PRs at handoff time
	// (v0.8.3+). Pulled from `gh pr list --state open --search
	// head:worker/` by the caller. Empty list renders the
	// _(no open PRs)_ placeholder. The successor coord re-spawns
	// shepherd until-loops from this list — one per URL — so CI/merge
	// state stays observed across handoff without depending on the
	// per-slug pr_url field on ActiveSubagents (covers PRs whose
	// associated task is already in-review and was dropped from
	// coord-state.json:worker_agent_ids).
	OpenPRs []OpenPR
}

// OpenPR is one open worker PR at handoff time. Captured by the caller
// invoking `gh pr list --state open --search "head:worker/" --json
// number,title,headRefName,url`; rendered as a markdown bullet in the
// `## Open PRs` section. All fields are operator-supplied (PR title)
// or gh-returned (number, head, url) — render path quotes them as raw
// markdown text, not Go-strconv format, since this section is for
// human reading rather than machine round-trip.
type OpenPR struct {
	Number      int    // PR number (e.g. 137)
	Title       string // PR title text
	HeadRefName string // head branch (worker/<slug>)
	URL         string // full PR URL
}

// ActiveSubagent is one in-flight worker the outgoing coord had spawned
// via Claude's Agent tool. Rendered (and parsed back) as a single
// machine-readable line in the `## Active Subagents` section so the
// successor coord can re-dispatch deterministically — no free-form
// prose for the resume path.
//
// AgentID is the 8-hex Fleet agent ID minted by the skill's
// mint_agent_id() — it doubles as the inbox filename
// (~/.fleet/inbox/<agent_id>.md) where the worker prompt lives.
//
// SubagentID is the Claude internal subagent identifier returned by
// the Agent tool. Empty when the coord skill hasn't captured it (Phase
// A's DISPATCH-block flow doesn't feed the captured ID back to the
// skill); in that case the successor re-dispatches via the AgentID's
// inbox file regardless. Carried for forward-compat.
//
// LastPhase mirrors workers/<slug>/state.json's phase field at the
// time of handoff. Useful as triage context for the successor; the
// re-dispatch decision is gated on WIP-file existence + status.
//
// PRURL is the open PR URL pulled from tasks.md's pr_url field for the
// worker's slug. Empty when no PR has been opened yet (worker still
// writing code). The successor coord uses this to (a) re-spawn a
// shepherd `until`-loop watching CI/merge state without re-dispatching
// the Agent call, and (b) skip the Agent re-dispatch entirely when a
// PR exists (work is already on disk + remote).
//
// Status mirrors tasks.md's status field per slug (todo / ready /
// in-progress / in-review / blocked / done / abandoned — the same enum
// as internal/tasks.Status). The successor branches on this:
//   - empty pr_url + status=in-progress → re-dispatch Agent (worker
//     was still writing).
//   - pr_url set + status=in-review → skip Agent, just re-spawn shepherd.
//   - status=done/blocked → drop the entry; no resume action needed.
//
// Empty strings for either field are legitimate ("no PR yet" / "older
// handoff doc, status not captured"). Parser tolerates the legacy
// 5-field row shape that doesn't carry these fields — they default to
// "" and the successor falls back to the pre-enrichment behavior
// (always re-dispatch).
type ActiveSubagent struct {
	TaskID     string // task slug (the worker's task)
	Branch     string // worker/<slug>
	LastPhase  string // tdd-green / push / blocked / etc.
	Status     string // tasks.md status enum; "" when not captured (legacy doc).
	PRURL      string // open PR URL from tasks.md; "" when no PR yet.
	AgentID    string // 8-hex Fleet agent ID (inbox file key)
	SubagentID string // Claude Agent tool subagent ID; empty when not captured
}

// NewManualStub builds a Doc with all five body sections set to
// Placeholder. This is the 4a operator-triggered shape — honest about
// not having the agent's view of the work.
//
// number and prev come from the outgoing agent's HandoffNumber and
// LastHandoffPath; the caller (cmd/fleet/handoff.go) reads them from
// the agent record before calling here.
func NewManualStub(agentID, taskID, project string, number int, prev *string, ts time.Time) *Doc {
	return &Doc{
		AgentID:       agentID,
		TaskID:        taskID,
		Project:       project,
		Type:          TypeManual,
		Number:        number,
		PreviousPath:  prev,
		Timestamp:     ts.UTC(),
		Completed:     Placeholder,
		KeyDecisions:  Placeholder,
		FilesModified: Placeholder,
		OpenQuestions: Placeholder,
		NextSteps:     Placeholder,
	}
}

// FirstAction is the body of the "First Action (auto)" section that
// every handoff doc carries.
//
// Native-RC rewrite (rc-default-native-startup, operator directive
// 2026-05-29): replacement coords are spawned with `--remote-control
// "fleet-coord-<id>-<project>"` baked into their claude argv, so
// mobile/web pairing carries through the handoff automatically —
// there is no re-attach command for the operator to run. The section
// is now a status note plus the opt-out escape hatch.
//
// Body order is load-bearing:
//
//  1. Status note: pairing is native at spawn; `fleet rc status
//     <project>` is the diagnostic if it looks broken. If the project
//     was opted out (`fleet rc down <project>`), re-enable with
//     `fleet rc up <project>` and respawn via `fleet handoff`.
//  2. `/coordinator` slash command: resume the per-project supervisor
//     loop (idempotent — NB-flock skips when held). Same load-bearing
//     paragraph as before; universal (worker handoffs also run it as
//     a no-op).
//
// Slash commands run in chat (not bash). NO bash block — the "exec
// arbitrary bash from a markdown file" semantics stays gone (v0.12).
//
// Issue #31, #56 lineage. Must stay byte-identical with
// skills/fleet-guard/handoff.py:first_action(project) — the Python
// skill writes the same handoff doc shape on auto-handoff and
// renderers are tested for byte-equality (TestRender_SkillByteGolden).
//
// Empty project falls back to `<project>` placeholder text — older
// records / legacy paths that don't carry a project still produce a
// well-formed instruction (operator substitutes the project name
// manually).
func FirstAction(project string) string {
	display := project
	if display == "" {
		display = "<project>"
	}
	return "Mobile/web pairing (Remote Control) is native: this replacement coord\n" +
		"was spawned with `--remote-control` baked into its claude argv, so\n" +
		"pairing carries through automatically — nothing to re-attach. If\n" +
		"pairing looks broken, check:\n" +
		"\n" +
		"    fleet rc status " + display + "\n" +
		"\n" +
		"If RC was disabled for this project (`fleet rc down " + display + "`),\n" +
		"re-enable it with `fleet rc up " + display + "` — it takes effect on the\n" +
		"next coord spawn (`fleet handoff <coord-id>` respawns the live coord).\n" +
		"\n" +
		"Then run the slash command `/coordinator` (in the chat, not bash) to resume the per-project supervisor tick loop. The /coordinator skill is idempotent — running it on a coord session that already holds the NB-flock is a no-op (the flock skips when held), and on a non-coord lineage it exits cleanly with no project to supervise.\n" +
		"\n" +
		"Then continue with the sections below."
}

// Render produces the markdown+frontmatter bytes for d.
//
// Frontmatter is hand-rolled YAML so we avoid pulling in a YAML
// dependency just to write 8 key:value pairs. All string fields are
// quoted via Go's %q (double-quoted form, YAML-compatible flow scalar)
// so operator-supplied values containing colons, newlines, or other
// YAML metacharacters cannot corrupt or inject into the frontmatter.
// agent_id is hex from agent.NewID and would never need quoting in
// practice, but consistency beats minimizing bytes.
//
// Body sections, in order:
//  1. ## First Action (auto)        — fixed FirstAction string
//  2. ## Completed                  — Doc.Completed
//  3. ## Key Decisions              — Doc.KeyDecisions
//  4. ## Files Modified             — Doc.FilesModified
//  5. ## Open Questions             — Doc.OpenQuestions
//  6. ## Next Steps (prioritized)   — Doc.NextSteps
//  7. ## Active Subagents           — Doc.ActiveSubagents (issue #93 Phase B2)
//  8. ## Open PRs                   — Doc.OpenPRs (v0.8.3 — successor
//     re-spawns shepherd until-loops from this snapshot)
//
// Pure function — no I/O, no globals. Use Write to persist.
func Render(d *Doc) []byte {
	var b bytes.Buffer
	b.WriteString("---\n")
	fmt.Fprintf(&b, "agent_id: %q\n", d.AgentID)
	fmt.Fprintf(&b, "task_id: %q\n", d.TaskID)
	fmt.Fprintf(&b, "project: %q\n", d.Project)
	if d.ContextPctAtHandoff != nil {
		fmt.Fprintf(&b, "context_pct_at_handoff: %s\n",
			strconv.FormatFloat(*d.ContextPctAtHandoff, 'f', -1, 64))
	} else {
		b.WriteString("context_pct_at_handoff: null\n")
	}
	if d.PreviousPath != nil {
		fmt.Fprintf(&b, "previous_handoff: %q\n", *d.PreviousPath)
	} else {
		b.WriteString("previous_handoff: null\n")
	}
	fmt.Fprintf(&b, "handoff_number: %d\n", d.Number)
	fmt.Fprintf(&b, "timestamp: %q\n", d.Timestamp.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "handoff_type: %q\n", d.Type)
	b.WriteString("---\n\n")

	fmt.Fprintf(&b, "## First Action (auto)\n%s\n\n", FirstAction(d.Project))
	fmt.Fprintf(&b, "## Completed\n%s\n\n", d.Completed)
	fmt.Fprintf(&b, "## Key Decisions\n%s\n\n", d.KeyDecisions)
	fmt.Fprintf(&b, "## Files Modified\n%s\n\n", d.FilesModified)
	fmt.Fprintf(&b, "## Open Questions\n%s\n\n", d.OpenQuestions)
	fmt.Fprintf(&b, "## Next Steps (prioritized)\n%s\n\n", d.NextSteps)
	fmt.Fprintf(&b, "## Active Subagents\n%s\n\n", renderActiveSubagents(d.ActiveSubagents))
	fmt.Fprintf(&b, "## Open PRs\n%s\n", renderOpenPRs(d.OpenPRs))
	return b.Bytes()
}

// OpenPRsNonePlaceholder is the body of the Open PRs section when the
// outgoing agent had zero open worker PRs (or the caller didn't
// populate OpenPRs at all). Successor parsers treat this body as "no
// shepherds to re-spawn".
const OpenPRsNonePlaceholder = "_(no open PRs)_"

// renderOpenPRs emits the body of the `## Open PRs` section. Each entry
// renders as a markdown bullet:
//
//   - #<number> <title> — <head> — <url>
//
// The URL is the load-bearing field — the successor coord re-spawns a
// shepherd until-loop watching CI/merge state for each URL. Number +
// title + head are operator readability only.
//
// Empty list / nil renders OpenPRsNonePlaceholder. Like the Active
// Subagents section, every entry is one physical line so a
// future parser can split on '\n' without re-joining.
func renderOpenPRs(prs []OpenPR) string {
	if len(prs) == 0 {
		return OpenPRsNonePlaceholder
	}
	var b bytes.Buffer
	for i, p := range prs {
		if i > 0 {
			b.WriteByte('\n')
		}
		// Sanitize embedded newlines in the title — PR titles are
		// user-supplied and a literal newline would break the line-
		// per-entry contract. Single line, no escaping needed beyond
		// that.
		title := strings.ReplaceAll(p.Title, "\n", " ")
		title = strings.ReplaceAll(title, "\r", " ")
		fmt.Fprintf(&b, "- #%d %s — %s — %s",
			p.Number, title, p.HeadRefName, p.URL)
	}
	return b.String()
}

// ActiveSubagentsNonePlaceholder is the body of the Active Subagents
// section when the outgoing agent had zero in-flight workers (or
// wasn't a coord). The placeholder makes the section presence
// invariant — successor coord parsers always find the section header
// and produce zero entries on this body.
const ActiveSubagentsNonePlaceholder = "_(none)_"

// renderActiveSubagents emits the body of the Active Subagents
// section. Empty list → ActiveSubagentsNonePlaceholder. Non-empty →
// one structured line per worker, key=value form, deterministic order.
//
// Format per entry (v0.8.3+, 7 fields):
//
//   - task=<slug> branch=<branch> phase=<phase> status=<status> pr_url=<url> agent_id=<hex> subagent_id=<id>
//
// Status + pr_url were added to drive the successor coord's
// re-dispatch decision (was: always re-dispatch; now: skip Agent
// when a PR is already open + work is in-review, just re-spawn
// shepherd). Legacy 5-field rows in older handoff docs parse with
// status="" and pr_url="" — see parseActiveSubagentLine.
//
// Empty fields render as `key=""`. The line is a single physical line
// (no embedded newlines) so the parser can split on '\n' without
// re-joining. Every field appears even when empty so the parser
// can rely on the field-set being uniform across rows.
func renderActiveSubagents(subs []ActiveSubagent) string {
	if len(subs) == 0 {
		return ActiveSubagentsNonePlaceholder
	}
	var b bytes.Buffer
	for i, s := range subs {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b,
			"- task=%q branch=%q phase=%q status=%q pr_url=%q agent_id=%q subagent_id=%q",
			s.TaskID, s.Branch, s.LastPhase, s.Status, s.PRURL, s.AgentID, s.SubagentID,
		)
	}
	return b.String()
}

// Write atomically publishes Render(d) at path. Path comes from
// state.HandoffPath.
func Write(d *Doc, path string) error {
	return state.WriteAtomic(path, Render(d))
}
