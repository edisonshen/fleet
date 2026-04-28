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

func TestRender_HasAllFiveSections(t *testing.T) {
	d := NewManualStub("a1b2c3d4", "auth-fix", "rainier", 1, nil, time.Now().UTC())
	got := string(Render(d))

	for _, h := range []string{
		"## Completed",
		"## Key Decisions",
		"## Files Modified",
		"## Open Questions",
		"## Next Steps (prioritized)",
	} {
		if !strings.Contains(got, h) {
			t.Errorf("missing section %q in:\n%s", h, got)
		}
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
