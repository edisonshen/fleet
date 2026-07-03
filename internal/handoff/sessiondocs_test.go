package handoff

// sessiondocs_test.go — CollectSessionDocs + CollectRecentDecisionsLive
// (curated-handoff-context). The former replaces the retired git-dump
// CollectFilesModified; the latter closes the no-tick-before-handoff gap
// so an agent rationale logged via `fleet checkpoint decision` reaches
// Key Decisions even when no tick published a fresh coord-checkpoint.md.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeCoordStateJSON writes a coord-state.json with the given raw body
// under <dir> and returns its path.
func writeCoordStateJSON(t *testing.T, dir, body string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	path := filepath.Join(dir, "coord-state.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write coord-state.json: %v", err)
	}
	return path
}

// ---------- CollectSessionDocs ----------

// Test 1 — role-tagged rows, never a git dump.
func TestCollectSessionDocs_RendersRoleTaggedRows(t *testing.T) {
	cs := writeCoordStateJSON(t, t.TempDir(), `{"session_docs":[
		{"path":"docs/DESIGN-a.md","role":"authored","ts":"2026-07-02T00:00:00Z"},
		{"path":"docs/DESIGN-b.md","role":"authored","ts":"2026-07-02T00:01:00Z"},
		{"path":"docs/TASK-PLAN-c.md","role":"implementing","ts":"2026-07-02T00:02:00Z"}
	]}`)
	got := CollectSessionDocs(cs, "")
	for _, want := range []string{
		"- authored: docs/DESIGN-a.md",
		"- authored: docs/DESIGN-b.md",
		"- implementing: docs/TASK-PLAN-c.md",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Count(got, "\n") != 2 { // 3 rows → 2 newlines
		t.Errorf("expected exactly 3 rows, got:\n%s", got)
	}
}

// Test 3 + Test 5 — empty / missing / malformed / empty-path all → "".
func TestCollectSessionDocs_EmptyMissingMalformedYieldPlaceholder(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"empty list":  `{"session_docs":[]}`,
		"no key":      `{"tick_count":3}`,
		"null key":    `{"session_docs":null}`,
		"malformed":   `{not valid json`,
		"wrong shape": `{"session_docs":"oops"}`,
	}
	for name, body := range cases {
		cs := writeCoordStateJSON(t, dir, body)
		if got := CollectSessionDocs(cs, ""); got != "" {
			t.Errorf("%s: got %q want empty", name, got)
		}
	}
	// Missing file.
	if got := CollectSessionDocs(filepath.Join(dir, "nope.json"), ""); got != "" {
		t.Errorf("missing file: got %q want empty", got)
	}
	// Empty path (no shell-out / read).
	if got := CollectSessionDocs("", ""); got != "" {
		t.Errorf("empty path: got %q want empty", got)
	}
}

// Test 4 — cap at sessionDocsMax: newest N shown + "… and M more" tail.
func TestCollectSessionDocs_CapsAtSessionDocsMax(t *testing.T) {
	const extra = 5
	n := sessionDocsMax + extra
	var b strings.Builder
	b.WriteString(`{"session_docs":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"path":"docs/f%03d.md","role":"authored","ts":"2026-07-02T00:00:00Z"}`, i)
	}
	b.WriteString(`]}`)
	cs := writeCoordStateJSON(t, t.TempDir(), b.String())

	got := CollectSessionDocs(cs, "")
	lines := strings.Split(got, "\n")
	// sessionDocsMax rows + 1 overflow tail line.
	if len(lines) != sessionDocsMax+1 {
		t.Fatalf("line count: got %d want %d\n%s", len(lines), sessionDocsMax+1, got)
	}
	wantTail := fmt.Sprintf("- … and %d more", extra)
	if lines[len(lines)-1] != wantTail {
		t.Errorf("overflow tail: got %q want %q", lines[len(lines)-1], wantTail)
	}
	// Newest shown, oldest dropped.
	if !strings.Contains(got, fmt.Sprintf("docs/f%03d.md", n-1)) {
		t.Errorf("newest doc missing from capped output:\n%s", got)
	}
	if strings.Contains(got, "docs/f000.md") {
		t.Errorf("oldest doc should be dropped by the cap:\n%s", got)
	}
}

// ---------- CollectRecentDecisionsLive ----------

func TestCollectRecentDecisionsLive_ReadsBuffer(t *testing.T) {
	cs := writeCoordStateJSON(t, t.TempDir(),
		`{"recent_decisions":["auto: merged PR → task x done","agent: stopped rebase of PR #224 — superseded"]}`)
	got := CollectRecentDecisionsLive(cs, "")
	if len(got) != 2 {
		t.Fatalf("got %d entries want 2: %v", len(got), got)
	}
	if got[0] != "auto: merged PR → task x done" ||
		got[1] != "agent: stopped rebase of PR #224 — superseded" {
		t.Errorf("entries mismatch: %v", got)
	}
}

func TestCollectRecentDecisionsLive_MissingMalformedEmptyYieldNil(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"empty list": `{"recent_decisions":[]}`,
		"no key":     `{"tick_count":1}`,
		"null":       `{"recent_decisions":null}`,
		"malformed":  `{bad`,
	}
	for name, body := range cases {
		cs := writeCoordStateJSON(t, dir, body)
		if got := CollectRecentDecisionsLive(cs, ""); got != nil {
			t.Errorf("%s: got %v want nil", name, got)
		}
	}
	if got := CollectRecentDecisionsLive(filepath.Join(dir, "nope.json"), ""); got != nil {
		t.Errorf("missing file: got %v want nil", got)
	}
	if got := CollectRecentDecisionsLive("", ""); got != nil {
		t.Errorf("empty path: got %v want nil", got)
	}
}

// Test 7b — the motivating no-tick case: `fleet checkpoint decision`
// writes coord-state.json:recent_decisions out-of-band, and a manual
// handoff BEFORE the next tick must read it LIVE (not the stale
// coord-checkpoint.md) so Key Decisions carries the agent's rationale.
func TestEnrichManualDoc_LiveRecentDecisions_NoTick(t *testing.T) {
	pdir := withFleetHomeSynth(t)
	now := time.Now().UTC()
	// coord-state.json: no workers, but a freshly-logged agent decision
	// and NO coord-checkpoint.md at all (the no-tick window).
	writeCoordStateJSON(t, filepath.Join(pdir, "myproj"),
		`{"worker_agent_ids":{},"recent_decisions":["stopped rebase of PR #224 — superseded PR for a paused task"]}`)
	fakeGH(t, []byte("[]"), nil)

	doc := NewManualStub("deadbeef", "coord-myproj", "myproj", 1, nil, now)
	EnrichManualDoc(doc, "myproj", "deadbeef", "", nil, nil)

	if !strings.Contains(doc.KeyDecisions, "stopped rebase of PR #224 — superseded PR for a paused task") {
		t.Errorf("live recent_decisions not lifted into Key Decisions: %q", doc.KeyDecisions)
	}
}

// Live coord-state recent_decisions must WIN over a stale checkpoint's
// Recent decisions (the checkpoint predates the just-logged agent line).
func TestEnrichManualDoc_LiveRecentDecisions_OverridesStaleCheckpoint(t *testing.T) {
	pdir := withFleetHomeSynth(t)
	now := time.Now().UTC()
	// Checkpoint (coord_id deadbeef) carries an OLD decision; coord-state
	// carries a NEWER agent decision logged after the checkpoint tick.
	seedCheckpointFull(t, pdir, "myproj", now, nil,
		[]string{"- old checkpoint decision"}, nil)
	writeCoordStateJSON(t, filepath.Join(pdir, "myproj"),
		`{"worker_agent_ids":{},"recent_decisions":["fresh agent decision — logged after the tick"]}`)
	fakeGH(t, []byte("[]"), nil)

	doc := NewManualStub("deadbeef", "coord-myproj", "myproj", 1, nil, now)
	EnrichManualDoc(doc, "myproj", "deadbeef", "", nil, nil)

	if !strings.Contains(doc.KeyDecisions, "fresh agent decision — logged after the tick") {
		t.Errorf("live decision must win over checkpoint: %q", doc.KeyDecisions)
	}
	if strings.Contains(doc.KeyDecisions, "old checkpoint decision") {
		t.Errorf("stale checkpoint decision must be overridden by live: %q", doc.KeyDecisions)
	}
}

// ---------- generation scoping (coord-state survives succession) ----------

// session_docs entries stamped with a DIFFERENT coord_id are a
// predecessor's — the reader filters them so a successor's "Docs (this
// session)" never attributes another generation's docs to itself.
// Unstamped (legacy / operator-shell) entries pass, and an empty agentID
// disables filtering — the exact semantics of loadCheckpointIfFresher's
// coord_id guard.
func TestCollectSessionDocs_FiltersForeignGeneration(t *testing.T) {
	cs := writeCoordStateJSON(t, t.TempDir(), `{"session_docs":[
		{"path":"docs/DESIGN-old.md","role":"authored","ts":"2026-07-01T00:00:00Z","coord_id":"aaaa1111"},
		{"path":"docs/DESIGN-mine.md","role":"authored","ts":"2026-07-02T00:00:00Z","coord_id":"bbbb2222"},
		{"path":"docs/DESIGN-legacy.md","role":"implementing","ts":"2026-07-02T00:01:00Z"}
	]}`)
	// Reader is coord bbbb2222: the aaaa1111 entry is foreign, the
	// unstamped legacy entry passes.
	got := CollectSessionDocs(cs, "bbbb2222")
	if strings.Contains(got, "docs/DESIGN-old.md") {
		t.Errorf("foreign-generation doc leaked into session docs: %q", got)
	}
	if !strings.Contains(got, "- authored: docs/DESIGN-mine.md") {
		t.Errorf("own-generation doc missing: %q", got)
	}
	if !strings.Contains(got, "- implementing: docs/DESIGN-legacy.md") {
		t.Errorf("unstamped legacy doc must pass the guard: %q", got)
	}
	// Empty agentID (no generation context) → no filtering.
	all := CollectSessionDocs(cs, "")
	for _, want := range []string{"docs/DESIGN-old.md", "docs/DESIGN-mine.md", "docs/DESIGN-legacy.md"} {
		if !strings.Contains(all, want) {
			t.Errorf("empty agentID must disable the filter; missing %q in %q", want, all)
		}
	}
	// A third coord: both stamped rows are foreign; only the unstamped
	// legacy row survives.
	third := CollectSessionDocs(cs, "cccc3333")
	if third != "- implementing: docs/DESIGN-legacy.md" {
		t.Errorf("third generation should see only the legacy row, got %q", third)
	}
}

// A recent_decisions_owner stamp from a DIFFERENT coord suppresses the
// live override entirely (return nil) — the caller then falls back to the
// checkpoint value, which loadCheckpointIfFresher generation-guards. Own /
// unstamped / empty-agentID all pass.
func TestCollectRecentDecisionsLive_ForeignOwnerSuppressed(t *testing.T) {
	dir := t.TempDir()
	cs := writeCoordStateJSON(t, dir,
		`{"recent_decisions":["their decision — not mine"],"recent_decisions_owner":"aaaa1111"}`)
	if got := CollectRecentDecisionsLive(cs, "bbbb2222"); got != nil {
		t.Errorf("foreign-owner live buffer must be suppressed, got %v", got)
	}
	// Own stamp passes.
	if got := CollectRecentDecisionsLive(cs, "aaaa1111"); len(got) != 1 {
		t.Errorf("own-generation live buffer must pass: %v", got)
	}
	// Empty agentID (no generation context) passes.
	if got := CollectRecentDecisionsLive(cs, ""); len(got) != 1 {
		t.Errorf("empty agentID must disable the owner guard: %v", got)
	}
	// Unstamped buffer (tick-only writes) passes for any reader.
	cs2 := writeCoordStateJSON(t, dir, `{"recent_decisions":["tick decision"]}`)
	if got := CollectRecentDecisionsLive(cs2, "bbbb2222"); len(got) != 1 {
		t.Errorf("unstamped buffer must pass the owner guard: %v", got)
	}
}

// End-to-end: a successor coord's manual handoff must NOT lift a
// predecessor-owned live decisions buffer into ITS Key Decisions.
func TestEnrichManualDoc_ForeignOwnerDecisions_NotLifted(t *testing.T) {
	pdir := withFleetHomeSynth(t)
	now := time.Now().UTC()
	writeCoordStateJSON(t, filepath.Join(pdir, "myproj"),
		`{"worker_agent_ids":{},"recent_decisions":["predecessor rationale"],"recent_decisions_owner":"aaaa1111"}`)
	fakeGH(t, []byte("[]"), nil)

	doc := NewManualStub("deadbeef", "coord-myproj", "myproj", 1, nil, now)
	EnrichManualDoc(doc, "myproj", "deadbeef", "", nil, nil)

	if strings.Contains(doc.KeyDecisions, "predecessor rationale") {
		t.Errorf("foreign-owned live decisions leaked into successor's Key Decisions: %q", doc.KeyDecisions)
	}
	if doc.KeyDecisions != Placeholder {
		t.Errorf("expected placeholder (no own decisions), got %q", doc.KeyDecisions)
	}
}

// Recovery synth must ALSO prefer the live coord-state recent_decisions.
func TestSynthesizeRecovery_LiveRecentDecisions(t *testing.T) {
	pdir := withFleetHomeSynth(t)
	now := time.Now().UTC()
	writeCoordStateJSON(t, filepath.Join(pdir, "myproj"),
		`{"worker_agent_ids":{},"recent_decisions":["recovery live decision — from coord-state"]}`)

	doc, err := SynthesizeRecoveryWithLastHandoff("deadbeef", "myproj", "", now)
	if err != nil {
		t.Fatalf("SynthesizeRecoveryWithLastHandoff: %v", err)
	}
	if !strings.Contains(doc.KeyDecisions, "recovery live decision — from coord-state") {
		t.Errorf("synth did not lift live recent_decisions: %q", doc.KeyDecisions)
	}
}
