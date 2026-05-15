package handoff

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRender_HasFrontmatterFences(t *testing.T) {
	d := NewManualStub("a1b2c3d4", "auth-fix", "rainier", 1, nil,
		time.Date(2026, 4, 27, 18, 48, 7, 0, time.UTC))
	got := string(Render(d))

	if !strings.HasPrefix(got, "---\n") {
		t.Errorf("missing opening fence:\n%s", got)
	}
	if !strings.Contains(got, "\n---\n\n") {
		t.Errorf("missing closing fence:\n%s", got)
	}
}

func TestRender_HasAllSections(t *testing.T) {
	d := NewManualStub("a1b2c3d4", "auth-fix", "rainier", 1, nil, time.Now().UTC())
	got := string(Render(d))

	for _, h := range []string{
		"## First Action (auto)",
		"## Completed",
		"## Key Decisions",
		"## Files Modified",
		"## Open Questions",
		"## Next Steps (prioritized)",
		"## Active Subagents",
		"## Open PRs",
	} {
		if !strings.Contains(got, h) {
			t.Errorf("missing section %q in:\n%s", h, got)
		}
	}
}

func TestRender_FirstActionAppearsBeforeCompleted(t *testing.T) {
	d := NewManualStub("a1b2c3d4", "auth-fix", "rainier", 1, nil, time.Now().UTC())
	got := string(Render(d))
	first := strings.Index(got, "## First Action (auto)")
	completed := strings.Index(got, "## Completed")
	if first < 0 || completed < 0 {
		t.Fatalf("missing one of the sections — first=%d completed=%d", first, completed)
	}
	if first >= completed {
		t.Errorf("First Action must appear before Completed; first=%d completed=%d", first, completed)
	}
}

func TestRender_FirstActionCarriesRemoteControlInvocation(t *testing.T) {
	d := NewManualStub("a1b2c3d4", "auth-fix", "rainier", 1, nil, time.Now().UTC())
	got := string(Render(d))
	// Project-scoped (rc-session-name-include): the daemon prefix
	// + pgrep guard now carry the project name so per-project
	// daemons coexist and the operator can distinguish per-project
	// sessions on phone / claude.ai.
	for _, want := range []string{
		"pgrep -f '^claude remote-control --remote-control-session-name-prefix fleet-handoff-rainier",
		"nohup claude remote-control",
		`--remote-control-session-name-prefix "fleet-handoff-rainier"`,
		"run_in_background: true",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("First Action body missing %q", want)
		}
	}
}

// TestRender_FirstActionCarriesRemoteControlSlashCommand pins the
// issue #56 paragraph that tells the resuming agent to run the
// `/remote-control` slash command after the daemon is up. Without it,
// the daemon listens but the chat session never attaches and the
// operator's mobile pairing is lost across handoff. The byte-golden
// (TestRender_SkillByteGolden) covers exact-byte verification; this
// test gives a focused regression signal that's easier to read when
// the paragraph drifts.
func TestRender_FirstActionCarriesRemoteControlSlashCommand(t *testing.T) {
	d := NewManualStub("a1b2c3d4", "auth-fix", "rainier", 1, nil, time.Now().UTC())
	got := string(Render(d))
	want := "Then run the slash command `/remote-control` (in the chat, not bash) to connect this fresh session to your remote-control session."
	if !strings.Contains(got, want) {
		t.Errorf("First Action body missing /remote-control slash command instruction:\n%s", got)
	}
}

// TestRender_FirstActionInstructsCoordinatorRun pins the regression
// for the handoff-coord-spawn-prompt-fix bug: a freshly-spawned
// replacement coord agent must be told to run the `/coordinator` skill
// so the supervisor loop resumes (NB-flock acquired by the new agent's
// ID, dashboard "left side" lights up with the new 8-hex). Without
// this paragraph, the lock body keeps the predecessor's ID and the
// task queue silently stops draining.
//
// The instruction is universal in the FirstAction body (not gated on
// "is this a coord lineage?") because /coordinator is idempotent — a
// worker handoff that runs it briefly observes the NB-flock skip on a
// non-coord cwd and exits cleanly. The cost of an extra slash command
// in worker handoffs is tiny; the cost of forgetting it on coord
// handoffs is silent task-queue stalls.
func TestRender_FirstActionInstructsCoordinatorRun(t *testing.T) {
	d := NewManualStub("a1b2c3d4", "auth-fix", "rainier", 1, nil, time.Now().UTC())
	got := string(Render(d))
	want := "Then run the slash command `/coordinator`"
	if !strings.Contains(got, want) {
		t.Errorf("First Action body missing /coordinator instruction:\n%s", got)
	}
	// The paragraph should also document idempotency so an operator
	// (or successor agent) inspecting the doc understands why running
	// it on a non-coord lineage is a safe no-op.
	if !strings.Contains(got, "idempotent") {
		t.Errorf("First Action body missing idempotency note for /coordinator:\n%s", got)
	}
}

// TestRender_FirstActionCoordinatorAfterRemoteControl pins the
// ordering invariant: the `/remote-control` slash command must appear
// BEFORE the `/coordinator` slash command in the rendered doc. Order
// matters because remote-control attaches the freshly-spawned chat
// session to the operator's mobile pairing — running /coordinator
// first means the operator misses the supervisor loop's startup
// output (which surfaces the "lock acquired" line operators rely on
// to confirm the handoff replaced the predecessor cleanly).
func TestRender_FirstActionCoordinatorAfterRemoteControl(t *testing.T) {
	d := NewManualStub("a1b2c3d4", "auth-fix", "rainier", 1, nil, time.Now().UTC())
	got := string(Render(d))
	rcIdx := strings.Index(got, "`/remote-control`")
	coordIdx := strings.Index(got, "`/coordinator`")
	if rcIdx < 0 || coordIdx < 0 {
		t.Fatalf("missing one of the slash commands: rc=%d coord=%d", rcIdx, coordIdx)
	}
	if rcIdx >= coordIdx {
		t.Errorf("/remote-control must appear before /coordinator; rc=%d coord=%d", rcIdx, coordIdx)
	}
}

func TestRender_StubBodyIsPlaceholder(t *testing.T) {
	d := NewManualStub("a1b2c3d4", "auth-fix", "rainier", 1, nil, time.Now().UTC())
	got := string(Render(d))
	count := strings.Count(got, Placeholder)
	if count != 5 {
		t.Errorf("expected 5 placeholders (one per section), got %d in:\n%s", count, got)
	}
}

func TestRender_NullableFieldsRenderAsNull(t *testing.T) {
	d := NewManualStub("a1b2c3d4", "auth-fix", "rainier", 1, nil,
		time.Date(2026, 4, 27, 18, 48, 7, 0, time.UTC))
	got := string(Render(d))
	if !strings.Contains(got, "context_pct_at_handoff: null\n") {
		t.Errorf("expected null context_pct_at_handoff:\n%s", got)
	}
	if !strings.Contains(got, "previous_handoff: null\n") {
		t.Errorf("expected null previous_handoff:\n%s", got)
	}
}

func TestRender_PreviousPathQuoted(t *testing.T) {
	prev := "/Users/op/.fleet/handoffs/aaaa1111-20260427-184807.md"
	d := NewManualStub("a1b2c3d4", "auth-fix", "rainier", 4, &prev,
		time.Date(2026, 4, 27, 19, 0, 0, 0, time.UTC))
	got := string(Render(d))

	want := `previous_handoff: "/Users/op/.fleet/handoffs/aaaa1111-20260427-184807.md"`
	if !strings.Contains(got, want) {
		t.Errorf("expected quoted previous_handoff line %q, got:\n%s", want, got)
	}
	if !strings.Contains(got, "handoff_number: 4\n") {
		t.Errorf("expected handoff_number: 4 in:\n%s", got)
	}
}

func TestRender_ContextPctRendersAsNumber(t *testing.T) {
	pct := 72.5
	d := &Doc{
		AgentID:             "a1b2c3d4",
		TaskID:              "t",
		Project:             "p",
		Type:                TypeAutoYellow,
		Number:              2,
		ContextPctAtHandoff: &pct,
		Timestamp:           time.Now().UTC(),
		Completed:           "x",
		KeyDecisions:        "x",
		FilesModified:       "x",
		OpenQuestions:       "x",
		NextSteps:           "x",
	}
	got := string(Render(d))
	if !strings.Contains(got, "context_pct_at_handoff: 72.5\n") {
		t.Errorf("expected context_pct_at_handoff: 72.5 in:\n%s", got)
	}
	if !strings.Contains(got, `handoff_type: "auto-yellow"`+"\n") {
		t.Errorf("expected quoted handoff_type in:\n%s", got)
	}
}

func TestRender_QuotesYAMLMetacharsInOperatorFields(t *testing.T) {
	// Operator-supplied --project / --task may contain colons,
	// newlines, or other YAML metacharacters. %q on every string
	// field neutralizes these — without quoting, a project name like
	// "foo: bar" would corrupt the frontmatter (parsed as
	// `project: foo, then a stray "bar" key).
	d := NewManualStub("a1b2c3d4", "task: with colon", "proj\nbroken", 1, nil,
		time.Date(2026, 4, 27, 18, 48, 7, 0, time.UTC))
	got := string(Render(d))
	if !strings.Contains(got, `task_id: "task: with colon"`+"\n") {
		t.Errorf("task_id not quoted to preserve colon literal:\n%s", got)
	}
	// %q escapes \n as the literal sequence backslash-n inside the
	// quotes — the raw newline never reaches the YAML.
	if !strings.Contains(got, `project: "proj\nbroken"`+"\n") {
		t.Errorf("project not quoted to escape newline:\n%s", got)
	}
}

func TestRender_TimestampQuoted(t *testing.T) {
	d := NewManualStub("a1b2c3d4", "t", "p", 1, nil,
		time.Date(2026, 4, 27, 18, 48, 7, 0, time.UTC))
	got := string(Render(d))
	if !strings.Contains(got, `timestamp: "2026-04-27T18:48:07Z"`+"\n") {
		t.Errorf("expected quoted timestamp in:\n%s", got)
	}
}

func TestRender_TimestampNormalizedToUTC(t *testing.T) {
	pacific, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skip("zoneinfo not available")
	}
	// 07:32 PT == 14:32 UTC on this date (PDT, UTC-7).
	ts := time.Date(2026, 4, 27, 7, 32, 0, 0, pacific)
	d := NewManualStub("a1b2c3d4", "auth-fix", "rainier", 1, nil, ts)

	got := string(Render(d))
	if !strings.Contains(got, `timestamp: "2026-04-27T14:32:00Z"`+"\n") {
		t.Errorf("expected UTC RFC3339 timestamp in:\n%s", got)
	}
}

func TestRender_DeterministicForSameInput(t *testing.T) {
	ts := time.Date(2026, 4, 27, 18, 48, 7, 0, time.UTC)
	d1 := NewManualStub("a1b2c3d4", "t", "p", 1, nil, ts)
	d2 := NewManualStub("a1b2c3d4", "t", "p", 1, nil, ts)
	if string(Render(d1)) != string(Render(d2)) {
		t.Error("Render is non-deterministic for identical input")
	}
}

// TestRender_SkillByteGolden pins the exact bytes Render produces for a
// known input. The same byte-for-byte assertion lives in the Python skill at
// skills/fleet-guard/tests/test_handoff.py:EXPECTED_GOLDEN — both sides MUST
// produce the same bytes for the same input. If either drifts, both fail
// and we re-converge intentionally rather than discovering at handoff time
// that 4a's chain reader can't parse 4b's auto-handoff doc.
func TestRender_SkillByteGolden(t *testing.T) {
	prev := "/home/op/.fleet/handoffs/prev.md"
	pct := 50.0
	d := &Doc{
		AgentID:             "abcd1234",
		TaskID:              "demo-task",
		Project:             "myproj",
		Type:                TypeAutoYellow,
		Number:              2,
		PreviousPath:        &prev,
		ContextPctAtHandoff: &pct,
		Timestamp:           time.Date(2026, 4, 28, 12, 34, 56, 0, time.UTC),
		Completed:           "Wrote tests for foo",
		KeyDecisions:        Placeholder,
		FilesModified:       Placeholder,
		OpenQuestions:       Placeholder,
		NextSteps:           Placeholder,
	}
	want := "---\n" +
		"agent_id: \"abcd1234\"\n" +
		"task_id: \"demo-task\"\n" +
		"project: \"myproj\"\n" +
		"context_pct_at_handoff: 50\n" +
		"previous_handoff: \"/home/op/.fleet/handoffs/prev.md\"\n" +
		"handoff_number: 2\n" +
		"timestamp: \"2026-04-28T12:34:56Z\"\n" +
		"handoff_type: \"auto-yellow\"\n" +
		"---\n" +
		"\n" +
		"## First Action (auto)\n" + FirstAction(d.Project) + "\n\n" +
		"## Completed\nWrote tests for foo\n\n" +
		"## Key Decisions\n" + Placeholder + "\n\n" +
		"## Files Modified\n" + Placeholder + "\n\n" +
		"## Open Questions\n" + Placeholder + "\n\n" +
		"## Next Steps (prioritized)\n" + Placeholder + "\n\n" +
		"## Active Subagents\n" + ActiveSubagentsNonePlaceholder + "\n\n" +
		"## Open PRs\n" + OpenPRsNonePlaceholder + "\n"
	got := string(Render(d))
	if got != want {
		t.Errorf("Render byte-shape drifted from skill golden.\nwant:\n%s\n\ngot:\n%s", want, got)
	}
}

// TestRender_ActiveSubagents_NoneRendersPlaceholder pins the body of
// the Active Subagents section when the outgoing agent had zero
// in-flight workers. The placeholder shape lets the parser
// short-circuit without scanning entries.
func TestRender_ActiveSubagents_NoneRendersPlaceholder(t *testing.T) {
	d := NewManualStub("a1b2c3d4", "t", "p", 1, nil,
		time.Date(2026, 4, 27, 18, 48, 7, 0, time.UTC))
	got := string(Render(d))
	want := "## Active Subagents\n" + ActiveSubagentsNonePlaceholder + "\n"
	if !strings.Contains(got, want) {
		t.Errorf("expected empty Active Subagents section in:\n%s", got)
	}
}

// TestRender_OpenPRs_NoneRendersPlaceholder pins the placeholder body
// for the Open PRs section when the outgoing agent had zero open
// worker PRs (or the caller didn't populate OpenPRs).
func TestRender_OpenPRs_NoneRendersPlaceholder(t *testing.T) {
	d := NewManualStub("a1b2c3d4", "t", "p", 1, nil,
		time.Date(2026, 4, 27, 18, 48, 7, 0, time.UTC))
	got := string(Render(d))
	want := "## Open PRs\n" + OpenPRsNonePlaceholder + "\n"
	if !strings.HasSuffix(got, want) {
		t.Errorf("expected doc to end with empty Open PRs section:\n%s", got)
	}
}

// TestRender_OpenPRs_SingleEntry pins the per-PR bullet shape. The
// successor coord re-spawns one shepherd until-loop per URL listed,
// so the URL is the load-bearing field; number + title + head are
// readability only.
func TestRender_OpenPRs_SingleEntry(t *testing.T) {
	d := NewManualStub("a1b2c3d4", "t", "p", 1, nil,
		time.Date(2026, 4, 27, 18, 48, 7, 0, time.UTC))
	d.OpenPRs = []OpenPR{
		{Number: 137, Title: "feat(handoff): rich state",
			HeadRefName: "worker/handoff-rich-state",
			URL:         "https://github.com/edisonshen/fleet/pull/137"},
	}
	got := string(Render(d))
	want := "- #137 feat(handoff): rich state — worker/handoff-rich-state — https://github.com/edisonshen/fleet/pull/137"
	if !strings.Contains(got, want) {
		t.Errorf("expected PR bullet %q in:\n%s", want, got)
	}
}

// TestRender_OpenPRs_MultipleEntries pins multi-PR rendering. Each
// entry on its own line, input order preserved (gh pr list returns
// in newest-first order; we don't re-sort).
func TestRender_OpenPRs_MultipleEntries(t *testing.T) {
	d := NewManualStub("a1b2c3d4", "t", "p", 1, nil, time.Now().UTC())
	d.OpenPRs = []OpenPR{
		{Number: 137, Title: "rich state", HeadRefName: "worker/a",
			URL: "https://example/pr/137"},
		{Number: 138, Title: "next slice", HeadRefName: "worker/b",
			URL: "https://example/pr/138"},
	}
	got := string(Render(d))
	if !strings.Contains(got, "- #137 rich state — worker/a — https://example/pr/137") {
		t.Errorf("missing PR #137 bullet in:\n%s", got)
	}
	if !strings.Contains(got, "- #138 next slice — worker/b — https://example/pr/138") {
		t.Errorf("missing PR #138 bullet in:\n%s", got)
	}
	idx137 := strings.Index(got, "#137 rich state")
	idx138 := strings.Index(got, "#138 next slice")
	if idx137 < 0 || idx138 < 0 || idx137 >= idx138 {
		t.Errorf("PR entries lost input order: 137=%d 138=%d", idx137, idx138)
	}
}

// TestRender_OpenPRs_NewlineInTitleNeutralized pins the line-per-entry
// contract: a newline in a PR title must not break the section into
// multiple bullets. The renderer converts embedded \n and \r to
// spaces so the bullet stays one physical line.
func TestRender_OpenPRs_NewlineInTitleNeutralized(t *testing.T) {
	d := NewManualStub("a1b2c3d4", "t", "p", 1, nil, time.Now().UTC())
	d.OpenPRs = []OpenPR{
		{Number: 1, Title: "line one\nline two\rline three",
			HeadRefName: "worker/x", URL: "https://example/pr/1"},
	}
	got := string(Render(d))
	want := "- #1 line one line two line three — worker/x — https://example/pr/1"
	if !strings.Contains(got, want) {
		t.Errorf("expected sanitized bullet %q in:\n%s", want, got)
	}
}

// TestRender_ActiveSubagents_SingleEntry pins the per-entry line
// shape. Each line is `- task=<q> branch=<q> phase=<q> status=<q>
// pr_url=<q> agent_id=<q> subagent_id=<q>` — load-bearing for the
// parser. The status + pr_url additions (v0.8.3) drive the successor
// coord's selective re-dispatch decision.
func TestRender_ActiveSubagents_SingleEntry(t *testing.T) {
	d := NewManualStub("a1b2c3d4", "t", "p", 1, nil,
		time.Date(2026, 4, 27, 18, 48, 7, 0, time.UTC))
	d.ActiveSubagents = []ActiveSubagent{
		{TaskID: "fix-foo", Branch: "worker/fix-foo",
			LastPhase: "tdd-green", Status: "in-progress", PRURL: "",
			AgentID: "abcd1234", SubagentID: "claude-sub-1"},
	}
	got := string(Render(d))
	want := `- task="fix-foo" branch="worker/fix-foo" phase="tdd-green" status="in-progress" pr_url="" agent_id="abcd1234" subagent_id="claude-sub-1"`
	if !strings.Contains(got, want) {
		t.Errorf("expected entry line %q in:\n%s", want, got)
	}
}

// TestRender_ActiveSubagents_NewSchemaShape pins the v0.8.3 7-field
// row shape for a fully populated entry (status=in-review + pr_url
// set). Distinct from TestRender_ActiveSubagents_SingleEntry which
// covers the empty-pr_url path: this one exercises the
// "skip-Agent-redispatch, re-spawn shepherd only" branch the successor
// coord uses when the worker already has a PR open.
func TestRender_ActiveSubagents_NewSchemaShape(t *testing.T) {
	d := NewManualStub("a1b2c3d4", "t", "p", 1, nil,
		time.Date(2026, 4, 27, 18, 48, 7, 0, time.UTC))
	d.ActiveSubagents = []ActiveSubagent{
		{
			TaskID: "rich-state", Branch: "worker/rich-state",
			LastPhase: "review-codex", Status: "in-review",
			PRURL:   "https://github.com/edisonshen/fleet/pull/137",
			AgentID: "abcd1234", SubagentID: "claude-sub-1",
		},
	}
	got := string(Render(d))
	want := `- task="rich-state" branch="worker/rich-state" phase="review-codex" status="in-review" pr_url="https://github.com/edisonshen/fleet/pull/137" agent_id="abcd1234" subagent_id="claude-sub-1"`
	if !strings.Contains(got, want) {
		t.Errorf("expected 7-field row %q in:\n%s", want, got)
	}
}

// TestRender_ActiveSubagents_MultipleEntries pins multi-worker
// rendering. Each entry on its own line, deterministic ordering
// (input order preserved).
func TestRender_ActiveSubagents_MultipleEntries(t *testing.T) {
	d := NewManualStub("a1b2c3d4", "t", "p", 1, nil, time.Now().UTC())
	d.ActiveSubagents = []ActiveSubagent{
		{TaskID: "a", Branch: "worker/a", LastPhase: "push", Status: "in-progress", PRURL: "", AgentID: "11111111", SubagentID: ""},
		{TaskID: "b", Branch: "worker/b", LastPhase: "tdd-red", Status: "in-review", PRURL: "https://example/pr/1", AgentID: "22222222", SubagentID: "claude-sub-2"},
	}
	got := string(Render(d))
	if !strings.Contains(got, `- task="a" branch="worker/a" phase="push" status="in-progress" pr_url="" agent_id="11111111" subagent_id=""`) {
		t.Errorf("missing entry a in:\n%s", got)
	}
	if !strings.Contains(got, `- task="b" branch="worker/b" phase="tdd-red" status="in-review" pr_url="https://example/pr/1" agent_id="22222222" subagent_id="claude-sub-2"`) {
		t.Errorf("missing entry b in:\n%s", got)
	}
	idxA := strings.Index(got, `task="a"`)
	idxB := strings.Index(got, `task="b"`)
	if idxA < 0 || idxB < 0 || idxA >= idxB {
		t.Errorf("entries lost input order: idxA=%d idxB=%d", idxA, idxB)
	}
}

// TestParseActiveSubagents_NonePlaceholder regresses the empty
// section path: an empty subagent list MUST round-trip from
// Render(nil) → ParseActiveSubagents → nil.
func TestParseActiveSubagents_NonePlaceholder(t *testing.T) {
	d := NewManualStub("a1b2c3d4", "t", "p", 1, nil, time.Now().UTC())
	doc := Render(d) // ActiveSubagents nil → "(none)" body
	subs, warnings, err := ParseActiveSubagents(doc)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(subs) != 0 {
		t.Errorf("expected zero subs on empty section, got %d: %+v", len(subs), subs)
	}
	if len(warnings) != 0 {
		t.Errorf("expected zero warnings on empty section, got %d: %v", len(warnings), warnings)
	}
}

// TestParseActiveSubagents_SingleWorker pins the round-trip for one
// worker. Render → Parse must recover the same fields.
func TestParseActiveSubagents_SingleWorker(t *testing.T) {
	d := NewManualStub("a1b2c3d4", "t", "p", 1, nil, time.Now().UTC())
	d.ActiveSubagents = []ActiveSubagent{
		{TaskID: "fix-foo", Branch: "worker/fix-foo",
			LastPhase: "tdd-green", Status: "in-progress", PRURL: "",
			AgentID: "abcd1234", SubagentID: "claude-sub-1"},
	}
	doc := Render(d)
	subs, warnings, err := ParseActiveSubagents(doc)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}
	if len(subs) != 1 {
		t.Fatalf("expected 1 sub, got %d: %+v", len(subs), subs)
	}
	got := subs[0]
	want := d.ActiveSubagents[0]
	if got != want {
		t.Errorf("round-trip mismatch:\ngot:  %+v\nwant: %+v", got, want)
	}
}

// TestParseActiveSubagents_MultipleWorkers pins the round-trip for
// the multi-worker case (issue #93 Phase B2 use case).
func TestParseActiveSubagents_MultipleWorkers(t *testing.T) {
	d := NewManualStub("a1b2c3d4", "t", "p", 1, nil, time.Now().UTC())
	d.ActiveSubagents = []ActiveSubagent{
		{TaskID: "a", Branch: "worker/a", LastPhase: "push", Status: "in-progress", PRURL: "", AgentID: "11111111", SubagentID: ""},
		{TaskID: "b", Branch: "worker/b", LastPhase: "tdd-red", Status: "in-progress", PRURL: "", AgentID: "22222222", SubagentID: "claude-sub-2"},
		{TaskID: "c", Branch: "worker/c", LastPhase: "review-codex", Status: "in-review", PRURL: "https://example/pr/3", AgentID: "33333333", SubagentID: "claude-sub-3"},
	}
	doc := Render(d)
	subs, warnings, err := ParseActiveSubagents(doc)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}
	if len(subs) != 3 {
		t.Fatalf("expected 3 subs, got %d: %+v", len(subs), subs)
	}
	for i, want := range d.ActiveSubagents {
		if subs[i] != want {
			t.Errorf("entry %d mismatch:\ngot:  %+v\nwant: %+v", i, subs[i], want)
		}
	}
}

// TestParseActiveSubagents_BackwardsCompat pins the legacy-row-shape
// tolerance: a 5-field row (no status, no pr_url) written by an older
// fleet binary MUST still parse, with the new fields defaulting to
// "". The successor coord falls back to the pre-enrichment "always
// re-dispatch Agent" behavior on these entries — which is what older
// docs actually wanted anyway.
func TestParseActiveSubagents_BackwardsCompat(t *testing.T) {
	doc := []byte(
		"---\nagent_id: \"x\"\n---\n\n" +
			"## Active Subagents\n" +
			`- task="legacy" branch="worker/legacy" phase="tdd-green" agent_id="abcd1234" subagent_id=""` + "\n",
	)
	subs, warnings, err := ParseActiveSubagents(doc)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}
	if len(subs) != 1 {
		t.Fatalf("expected 1 sub from legacy row, got %d: %+v", len(subs), subs)
	}
	got := subs[0]
	want := ActiveSubagent{
		TaskID: "legacy", Branch: "worker/legacy", LastPhase: "tdd-green",
		Status: "", PRURL: "", AgentID: "abcd1234", SubagentID: "",
	}
	if got != want {
		t.Errorf("legacy row parse mismatch:\ngot:  %+v\nwant: %+v", got, want)
	}
}

// TestParseActiveSubagents_MalformedLineSkipped pins the resilience
// contract: a malformed entry mid-section logs a warning and skips
// the line, but the well-formed entries still parse.
func TestParseActiveSubagents_MalformedLineSkipped(t *testing.T) {
	doc := []byte(
		"---\nagent_id: \"x\"\n---\n\n" +
			"## Active Subagents\n" +
			`- task="ok" branch="worker/ok" phase="push" status="in-progress" pr_url="" agent_id="11111111" subagent_id=""` + "\n" +
			"this line is not a valid entry\n" +
			`- task="also-ok" branch="worker/also-ok" phase="" status="in-progress" pr_url="" agent_id="22222222" subagent_id=""` + "\n",
	)
	subs, warnings, err := ParseActiveSubagents(doc)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(subs) != 2 {
		t.Errorf("expected 2 well-formed entries, got %d: %+v", len(subs), subs)
	}
	if len(warnings) == 0 {
		t.Errorf("expected at least one warning for malformed line, got none")
	}
}

// TestParseActiveSubagents_MissingSection regresses the
// pre-Phase-B2 doc shape: a doc with no `## Active Subagents` header
// (legacy) MUST return zero entries + no error.
func TestParseActiveSubagents_MissingSection(t *testing.T) {
	doc := []byte(
		"---\nagent_id: \"x\"\n---\n\n" +
			"## First Action (auto)\nfoo\n\n" +
			"## Next Steps (prioritized)\nfoo\n",
	)
	subs, warnings, err := ParseActiveSubagents(doc)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(subs) != 0 {
		t.Errorf("expected zero subs on missing section, got %d: %+v", len(subs), subs)
	}
	if len(warnings) != 0 {
		t.Errorf("expected no warnings on missing section, got %v", warnings)
	}
}

// TestParseActiveSubagents_QuotedFieldsRoundTrip exercises tricky
// values: a path with spaces + a phase with a colon. strconv.Quote /
// Unquote cycles must round-trip these intact.
func TestParseActiveSubagents_QuotedFieldsRoundTrip(t *testing.T) {
	d := NewManualStub("a1b2c3d4", "t", "p", 1, nil, time.Now().UTC())
	d.ActiveSubagents = []ActiveSubagent{
		{TaskID: "weird slug", Branch: "worker/weird slug",
			LastPhase: "phase: with colon", Status: "in-review",
			PRURL:   `https://example/pr/1?token="x"`,
			AgentID: "abcd1234", SubagentID: `with"quote`},
	}
	doc := Render(d)
	subs, warnings, err := ParseActiveSubagents(doc)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}
	if len(subs) != 1 || subs[0] != d.ActiveSubagents[0] {
		t.Errorf("round-trip lost data:\ngot:  %+v\nwant: %+v", subs, d.ActiveSubagents)
	}
}

// TestParseActiveSubagents_NilDoc returns an explicit error so callers
// can distinguish "I/O failed" from "doc has no section".
func TestParseActiveSubagents_NilDoc(t *testing.T) {
	if _, _, err := ParseActiveSubagents(nil); err == nil {
		t.Error("expected error on nil doc")
	}
}

// TestParseActiveSubagents_TrailingBackslashSkipped pins iter-1
// review hardening: a malformed entry with a trailing backslash that
// would jump past len(s) inside splitKeyValuePairs's escape skip MUST
// be reported as a malformed-line warning, not panic on out-of-bounds
// access. strconv.Quote never produces this shape — the test guards
// against hand-edited / corrupted handoff docs reaching the parser.
func TestParseActiveSubagents_TrailingBackslashSkipped(t *testing.T) {
	doc := []byte(
		"---\nagent_id: \"x\"\n---\n\n" +
			"## Active Subagents\n" +
			// Note: trailing backslash inside the quoted value would
			// have caused +2 to overrun pre-fix.
			`- task="ok" branch="" phase="" status="" pr_url="" agent_id="11111111" subagent_id="abc\` + "\n" +
			// Well-formed entry on the next line still parses.
			`- task="recovers" branch="" phase="" status="" pr_url="" agent_id="22222222" subagent_id=""` + "\n",
	)
	subs, warnings, err := ParseActiveSubagents(doc)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(subs) != 1 || subs[0].TaskID != "recovers" {
		t.Errorf("expected 1 well-formed entry after malformed line, got %+v", subs)
	}
	if len(warnings) == 0 {
		t.Errorf("expected warning for malformed trailing-backslash line, got none")
	}
}

func TestResumePrompt(t *testing.T) {
	t.Run("non-empty path embeds it", func(t *testing.T) {
		got := ResumePrompt("/Users/x/.fleet/handoffs/a1b2c3d4-20260430-100000.md")
		want := "Read your handoff doc at /Users/x/.fleet/handoffs/a1b2c3d4-20260430-100000.md" +
			" and continue the task. Do not wait for further operator input."
		if got != want {
			t.Errorf("ResumePrompt mismatch:\ngot:  %q\nwant: %q", got, want)
		}
	})
	t.Run("empty path returns empty string so spawn skips send-keys", func(t *testing.T) {
		if got := ResumePrompt(""); got != "" {
			t.Errorf("ResumePrompt(\"\") = %q, want \"\"", got)
		}
	})
}

func TestWrite_PublishesAtomically(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	if err := os.MkdirAll(filepath.Join(tmp, "handoffs"), 0o755); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(tmp, "handoffs", "a1b2c3d4-20260427-184807.md")

	d := NewManualStub("a1b2c3d4", "auth-fix", "rainier", 1, nil,
		time.Date(2026, 4, 27, 18, 48, 7, 0, time.UTC))
	if err := Write(d, dest); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(Render(d)) {
		t.Errorf("file content does not match Render output")
	}

	// No .tmp leftover from the atomic write.
	entries, err := os.ReadDir(filepath.Join(tmp, "handoffs"))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp.") {
			t.Errorf("leftover tmp file: %s", e.Name())
		}
	}
}
