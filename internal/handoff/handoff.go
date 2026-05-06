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
type Doc struct {
	AgentID             string
	TaskID              string
	Project             string
	Type                string
	Number              int
	PreviousPath        *string
	ContextPctAtHandoff *float64
	Timestamp           time.Time

	Completed     string
	KeyDecisions  string
	FilesModified string
	OpenQuestions string
	NextSteps     string
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
// every handoff doc carries. It instructs the resuming agent to spawn
// `claude remote-control` in the background so the operator's mobile /
// claude.ai pairing carries through the fleet-guard handoff. Idempotent
// (pgrep guards re-launch when the daemon is already up).
//
// Issue #31. Must stay byte-identical with skills/fleet-guard/handoff.py
// FIRST_ACTION constant — the Python skill writes the same handoff doc
// shape on auto-handoff and renderers are tested for byte-equality.
const FirstAction = "**Run this BEFORE anything else** to reconnect the new instance to Remote Control:\n" +
	"\n" +
	"```bash\n" +
	"( pgrep -f \"claude remote-control\" >/dev/null 2>&1 || \\\n" +
	"  nohup claude remote-control \\\n" +
	"    --remote-control-session-name-prefix \"fleet-handoff\" \\\n" +
	"    > /tmp/claude-rc-handoff.log 2>&1 & )\n" +
	"```\n" +
	"\n" +
	"Use the Bash tool with run_in_background: true. Then continue with the sections below."

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

	fmt.Fprintf(&b, "## First Action (auto)\n%s\n\n", FirstAction)
	fmt.Fprintf(&b, "## Completed\n%s\n\n", d.Completed)
	fmt.Fprintf(&b, "## Key Decisions\n%s\n\n", d.KeyDecisions)
	fmt.Fprintf(&b, "## Files Modified\n%s\n\n", d.FilesModified)
	fmt.Fprintf(&b, "## Open Questions\n%s\n\n", d.OpenQuestions)
	fmt.Fprintf(&b, "## Next Steps (prioritized)\n%s\n", d.NextSteps)
	return b.Bytes()
}

// Write atomically publishes Render(d) at path. Path comes from
// state.HandoffPath.
func Write(d *Doc, path string) error {
	return state.WriteAtomic(path, Render(d))
}
