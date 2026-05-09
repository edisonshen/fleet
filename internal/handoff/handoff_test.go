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
	for _, want := range []string{
		`pgrep -f "claude remote-control"`,
		"nohup claude remote-control",
		`--remote-control-session-name-prefix "fleet-handoff"`,
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
		"## First Action (auto)\n" + FirstAction + "\n\n" +
		"## Completed\nWrote tests for foo\n\n" +
		"## Key Decisions\n" + Placeholder + "\n\n" +
		"## Files Modified\n" + Placeholder + "\n\n" +
		"## Open Questions\n" + Placeholder + "\n\n" +
		"## Next Steps (prioritized)\n" + Placeholder + "\n\n" +
		"## Active Subagents\n" + ActiveSubagentsNonePlaceholder + "\n"
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
	if !strings.HasSuffix(got, want) {
		t.Errorf("expected doc to end with empty Active Subagents section:\n%s", got)
	}
}

// TestRender_ActiveSubagents_SingleEntry pins the per-entry line
// shape. Each line is `- task=<q> branch=<q> phase=<q> agent_id=<q>
// subagent_id=<q>` — load-bearing for the parser.
func TestRender_ActiveSubagents_SingleEntry(t *testing.T) {
	d := NewManualStub("a1b2c3d4", "t", "p", 1, nil,
		time.Date(2026, 4, 27, 18, 48, 7, 0, time.UTC))
	d.ActiveSubagents = []ActiveSubagent{
		{TaskID: "fix-foo", Branch: "worker/fix-foo",
			LastPhase: "tdd-green", AgentID: "abcd1234",
			SubagentID: "claude-sub-1"},
	}
	got := string(Render(d))
	want := `- task="fix-foo" branch="worker/fix-foo" phase="tdd-green" agent_id="abcd1234" subagent_id="claude-sub-1"`
	if !strings.Contains(got, want) {
		t.Errorf("expected entry line %q in:\n%s", want, got)
	}
}

// TestRender_ActiveSubagents_MultipleEntries pins multi-worker
// rendering. Each entry on its own line, deterministic ordering
// (input order preserved).
func TestRender_ActiveSubagents_MultipleEntries(t *testing.T) {
	d := NewManualStub("a1b2c3d4", "t", "p", 1, nil, time.Now().UTC())
	d.ActiveSubagents = []ActiveSubagent{
		{TaskID: "a", Branch: "worker/a", LastPhase: "push", AgentID: "11111111", SubagentID: ""},
		{TaskID: "b", Branch: "worker/b", LastPhase: "tdd-red", AgentID: "22222222", SubagentID: "claude-sub-2"},
	}
	got := string(Render(d))
	if !strings.Contains(got, `- task="a" branch="worker/a" phase="push" agent_id="11111111" subagent_id=""`) {
		t.Errorf("missing entry a in:\n%s", got)
	}
	if !strings.Contains(got, `- task="b" branch="worker/b" phase="tdd-red" agent_id="22222222" subagent_id="claude-sub-2"`) {
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
			LastPhase: "tdd-green", AgentID: "abcd1234",
			SubagentID: "claude-sub-1"},
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
		{TaskID: "a", Branch: "worker/a", LastPhase: "push", AgentID: "11111111", SubagentID: ""},
		{TaskID: "b", Branch: "worker/b", LastPhase: "tdd-red", AgentID: "22222222", SubagentID: "claude-sub-2"},
		{TaskID: "c", Branch: "worker/c", LastPhase: "review-codex", AgentID: "33333333", SubagentID: "claude-sub-3"},
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

// TestParseActiveSubagents_MalformedLineSkipped pins the resilience
// contract: a malformed entry mid-section logs a warning and skips
// the line, but the well-formed entries still parse.
func TestParseActiveSubagents_MalformedLineSkipped(t *testing.T) {
	doc := []byte(
		"---\nagent_id: \"x\"\n---\n\n" +
			"## Active Subagents\n" +
			`- task="ok" branch="worker/ok" phase="push" agent_id="11111111" subagent_id=""` + "\n" +
			"this line is not a valid entry\n" +
			`- task="also-ok" branch="worker/also-ok" phase="" agent_id="22222222" subagent_id=""` + "\n",
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
			LastPhase: "phase: with colon", AgentID: "abcd1234",
			SubagentID: `with"quote`},
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
			`- task="ok" branch="" phase="" agent_id="11111111" subagent_id="abc\` + "\n" +
			// Well-formed entry on the next line still parses.
			`- task="recovers" branch="" phase="" agent_id="22222222" subagent_id=""` + "\n",
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
