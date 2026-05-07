package learnings

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/state"
)

func TestAppend_CreatesFileWithFrontmatter(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	e := &Entry{
		Timestamp: time.Date(2026, 5, 6, 14, 22, 0, 0, time.UTC),
		Author:    "agent:91f0a2c4",
		TaskSlug:  "fix-flaky-test-7a3c",
		Tag:       "testing",
		Body:      "go test -count=N races on tmpdir cleanup. Use t.TempDir().",
	}
	if err := Append("fleet", e); err != nil {
		t.Fatalf("Append: %v", err)
	}
	dir, _ := state.ProjectDir("fleet")
	data, err := os.ReadFile(filepath.Join(dir, "learnings.md"))
	if err != nil {
		t.Fatalf("read learnings.md: %v", err)
	}
	got := string(data)
	if !strings.HasPrefix(got, "---\nschema: v1\n---\n") {
		t.Errorf("missing frontmatter; got prefix %q", got[:32])
	}
	if !strings.Contains(got, "## 2026-05-06T14:22:00Z · agent:91f0a2c4 · task:fix-flaky-test-7a3c · tag:testing") {
		t.Errorf("missing canonical header; got\n%s", got)
	}
}

func TestAppendConcurrent(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	const N = 20
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			e := &Entry{
				Timestamp: time.Date(2026, 5, 6, 0, 0, i, 0, time.UTC),
				Author:    fmt.Sprintf("agent:%08x", i),
				Tag:       "concurrent",
				Body:      fmt.Sprintf("entry %d", i),
			}
			if err := Append("fleet", e); err != nil {
				t.Errorf("Append %d: %v", i, err)
			}
		}()
	}
	wg.Wait()
	got, err := Read("fleet")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != N {
		t.Errorf("len=%d; want %d (concurrent appends lost entries)", len(got), N)
	}
	// All N distinct authors must be present.
	seen := make(map[string]bool)
	for _, e := range got {
		seen[e.Author] = true
	}
	if len(seen) != N {
		t.Errorf("distinct authors=%d; want %d", len(seen), N)
	}
}

func TestPrune_MovesOldEntries(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	new := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	if err := Append("fleet", &Entry{Timestamp: old, Author: "operator", Tag: "stale", Body: "old"}); err != nil {
		t.Fatalf("Append old: %v", err)
	}
	if err := Append("fleet", &Entry{Timestamp: new, Author: "operator", Tag: "fresh", Body: "new"}); err != nil {
		t.Fatalf("Append new: %v", err)
	}
	cutoff := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	if err := Prune("fleet", cutoff); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	cur, err := Read("fleet")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(cur) != 1 || cur[0].Tag != "fresh" {
		t.Errorf("after prune learnings.md has %v; want [fresh]", tagList(cur))
	}
	dir, _ := state.ProjectDir("fleet")
	arc, err := readEntries(filepath.Join(dir, "learnings-archive.md"))
	if err != nil {
		t.Fatalf("read archive: %v", err)
	}
	if len(arc) != 1 || arc[0].Tag != "stale" {
		t.Errorf("archive has %v; want [stale]", tagList(arc))
	}
}

// TestPrune_PreservesIdenticalDuplicatesInBatch confirms the dedup
// only fires against PRE-EXISTING archive entries, not entries
// archived during the same Prune pass. Two identical-content entries
// in current must both survive Prune (legal in append-only log; can
// happen when two appenders write the same message in the same
// second with the same headers).
func TestPrune_PreservesIdenticalDuplicatesInBatch(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Two identical entries — same RFC3339 timestamp, same headers,
	// same body. Both should survive prune (move to archive, not
	// drop one as a "duplicate").
	for i := 0; i < 2; i++ {
		if err := Append("fleet", &Entry{Timestamp: old, Author: "operator", Tag: "stale", Body: "same"}); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	cutoff := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	if err := Prune("fleet", cutoff); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	dir, _ := state.ProjectDir("fleet")
	arc, err := readEntries(filepath.Join(dir, "learnings-archive.md"))
	if err != nil {
		t.Fatalf("read archive: %v", err)
	}
	if len(arc) != 2 {
		t.Errorf("archive has %d entries; want 2 (both identical-content entries preserved)", len(arc))
	}
}

// TestPrune_RetrySafeAfterPartialFailure simulates the crash-recovery
// path: a previous Prune wrote learnings-archive.md but failed before
// rewriting learnings.md, so the same entry exists in BOTH files. The
// retry must NOT duplicate it in the archive.
func TestPrune_RetrySafeAfterPartialFailure(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	new := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

	// Seed both learnings.md and learnings-archive.md with the
	// "stale" entry — exact crash-state after a partial Prune.
	if err := Append("fleet", &Entry{Timestamp: old, Author: "operator", Tag: "stale", Body: "old"}); err != nil {
		t.Fatalf("Append stale to current: %v", err)
	}
	if err := Append("fleet", &Entry{Timestamp: new, Author: "operator", Tag: "fresh", Body: "new"}); err != nil {
		t.Fatalf("Append fresh to current: %v", err)
	}
	dir, _ := state.ProjectDir("fleet")
	arcPath := filepath.Join(dir, "learnings-archive.md")
	if err := writeFile(arcPath, []Entry{{Timestamp: old, Author: "operator", Tag: "stale", Body: "old"}}, nil); err != nil {
		t.Fatalf("seed archive: %v", err)
	}

	// Retry the prune — should be a no-op for archive (already
	// there) and remove "stale" from current.
	cutoff := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	if err := Prune("fleet", cutoff); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	arc, err := readEntries(arcPath)
	if err != nil {
		t.Fatalf("read archive: %v", err)
	}
	if len(arc) != 1 {
		t.Errorf("archive has %d entries; want 1 (deduped after retry); got tags=%v", len(arc), tagList(arc))
	}
	cur, err := Read("fleet")
	if err != nil {
		t.Fatalf("Read current: %v", err)
	}
	if len(cur) != 1 || cur[0].Tag != "fresh" {
		t.Errorf("after retry learnings.md has %v; want [fresh]", tagList(cur))
	}
}

// TestPrune_RetryPreservesLegitimateDuplicates is the count-vs-set
// dedup regression. A previous Prune crash archived ONE copy of an
// entry that legitimately appears TWICE in learnings.md (identical
// timestamp, headers, body — explicitly allowed in the append-only
// log). The retry must archive the second copy, not skip both as
// "already archived" duplicates.
//
// Pre-fix (set membership): both current copies see the key in the
// archived set, both get dropped, archive ends with 1 entry total —
// silently losing the legitimate second copy.
//
// Post-fix (multiset): the first match decrements the count, the
// second match falls through and is appended to archive — archive
// ends with 2 entries total, one from the prior crash + one from
// this retry.
func TestPrune_RetryPreservesLegitimateDuplicates(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	new := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

	// Two identical-content entries in current (same timestamp,
	// same header, same body — legal in append-only log).
	for i := 0; i < 2; i++ {
		if err := Append("fleet", &Entry{Timestamp: old, Author: "operator", Tag: "stale", Body: "same"}); err != nil {
			t.Fatalf("Append stale %d: %v", i, err)
		}
	}
	// Plus an unrelated fresh entry that must stay in current.
	if err := Append("fleet", &Entry{Timestamp: new, Author: "operator", Tag: "fresh", Body: "new"}); err != nil {
		t.Fatalf("Append fresh: %v", err)
	}
	// Seed archive with ONE copy of the stale entry — exact crash
	// state after a partial previous Prune (archive write
	// succeeded, current rewrite failed before publishing).
	dir, _ := state.ProjectDir("fleet")
	arcPath := filepath.Join(dir, "learnings-archive.md")
	if err := writeFile(arcPath, []Entry{{Timestamp: old, Author: "operator", Tag: "stale", Body: "same"}}, nil); err != nil {
		t.Fatalf("seed archive: %v", err)
	}

	cutoff := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	if err := Prune("fleet", cutoff); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	// Archive must hold 2 stale copies (the seeded one + the second
	// current copy that wasn't yet archived).
	arc, err := readEntries(arcPath)
	if err != nil {
		t.Fatalf("read archive: %v", err)
	}
	staleArc := 0
	for _, e := range arc {
		if e.Tag == "stale" {
			staleArc++
		}
	}
	if staleArc != 2 {
		t.Errorf("archive stale count=%d; want 2 (seeded + retried second copy); tags=%v", staleArc, tagList(arc))
	}
	// Current must hold ONLY the fresh entry — both stale copies
	// are now in the archive.
	cur, err := Read("fleet")
	if err != nil {
		t.Fatalf("Read current: %v", err)
	}
	if len(cur) != 1 || cur[0].Tag != "fresh" {
		t.Errorf("current has %v; want [fresh]", tagList(cur))
	}
}

func TestFilter_TagSubstring(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	for i, tag := range []string{"testing", "test-flake", "review", "test-skill"} {
		e := &Entry{
			Timestamp: time.Date(2026, 5, 6, 0, 0, i, 0, time.UTC),
			Author:    "operator",
			Tag:       tag,
			Body:      fmt.Sprintf("entry %d", i),
		}
		if err := Append("fleet", e); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	got, err := Filter("fleet", "test", "", 0)
	if err != nil {
		t.Fatalf("Filter: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("len=%d; want 3 (testing, test-flake, test-skill); got %v", len(got), tagList(got))
	}
}

func TestFilter_TaskSlug(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	now := time.Date(2026, 5, 6, 0, 0, 0, 0, time.UTC)
	mk := func(slug, tag string) *Entry {
		return &Entry{Timestamp: now, Author: "agent:abc", TaskSlug: slug, Tag: tag, Body: "x"}
	}
	for _, e := range []*Entry{mk("a-1234", "x"), mk("b-5678", "x"), mk("a-1234", "y")} {
		// give each a unique timestamp so they don't dedupe in
		// downstream readers. We bump now manually.
		e.Timestamp = now
		now = now.Add(time.Second)
		if err := Append("fleet", e); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	got, err := Filter("fleet", "", "a-1234", 0)
	if err != nil {
		t.Fatalf("Filter: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("len=%d; want 2 (both a-1234)", len(got))
	}
}

func TestFilter_Limit(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	for i := 0; i < 10; i++ {
		e := &Entry{
			Timestamp: time.Date(2026, 5, 6, 0, 0, i, 0, time.UTC),
			Author:    "operator",
			Tag:       "x",
			Body:      fmt.Sprintf("e%d", i),
		}
		if err := Append("fleet", e); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	got, err := Filter("fleet", "x", "", 3)
	if err != nil {
		t.Fatalf("Filter: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("len=%d; want 3 (limit)", len(got))
	}
	// Newest first → last 3 are seconds 9,8,7.
	if got[0].Body != "e9" || got[1].Body != "e8" || got[2].Body != "e7" {
		t.Errorf("got %v; want [e9 e8 e7]", []string{got[0].Body, got[1].Body, got[2].Body})
	}
}

func TestAppend_RejectsDelimiterInFields(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	bad := []*Entry{
		{Author: "agent · sneaky", Tag: "x", Body: "x"},                   // delimiter in author
		{Author: "operator", Tag: "x · poison", Body: "x"},                // delimiter in tag
		{Author: "operator", TaskSlug: "a · b-1234", Tag: "x", Body: "x"}, // delimiter in slug
		{Author: "agent\nnewline", Tag: "x", Body: "x"},                   // newline in author
		{Author: "operator", Tag: "x\rcarriage", Body: "x"},               // CR in tag
	}
	for i, e := range bad {
		err := Append("fleet", e)
		if err == nil {
			t.Errorf("Append %d returned nil; want ErrInvalidEntry", i)
		}
	}
}

func TestAppend_RejectsH2InBody(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	e := &Entry{
		Author: "operator", Tag: "x",
		Body: "First line\n\n## Sneaky H2\n\nSplits the entry on next read.",
	}
	err := Append("fleet", e)
	if err == nil {
		t.Errorf("Append returned nil; want ErrInvalidEntry for body H2")
	}
}

func TestAppend_RejectsWhitespaceOnlyFields(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	for _, e := range []*Entry{
		{Author: "   ", Tag: "x", Body: "x"},
		{Author: "operator", Tag: "  \t  ", Body: "x"},
	} {
		err := Append("fleet", e)
		if err == nil {
			t.Errorf("Append %+v returned nil; want ErrInvalidEntry", e)
		}
	}
}

func TestParseMalformed_PreservedAcrossRewrite(t *testing.T) {
	// Append/Prune rewrite the file. A malformed block must NOT be
	// silently deleted on rewrite — it's operator memory, fail open.
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	dir, err := state.ProjectDir("fleet")
	if err != nil {
		t.Fatalf("ProjectDir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	src := "---\nschema: v1\n---\n\n" +
		"## not-a-timestamp · operator · operator · tag:bad\n\nMalformed body line 1.\nMalformed body line 2.\n"
	if err := os.WriteFile(filepath.Join(dir, "learnings.md"), []byte(src), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Trigger a rewrite via Append.
	if err := Append("fleet", &Entry{
		Timestamp: time.Date(2026, 5, 6, 10, 0, 0, 0, time.UTC),
		Author:    "operator", Tag: "good", Body: "good entry",
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	// Both the new entry AND the malformed block must be present.
	data, err := os.ReadFile(filepath.Join(dir, "learnings.md"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got := string(data)
	if !strings.Contains(got, "Malformed body line 1") {
		t.Errorf("malformed block deleted on rewrite; got:\n%s", got)
	}
	if !strings.Contains(got, "tag:good") {
		t.Errorf("new entry missing; got:\n%s", got)
	}
}

func TestParseMalformed_HeaderSkipped(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	dir, err := state.ProjectDir("fleet")
	if err != nil {
		t.Fatalf("ProjectDir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// A file with one valid + one malformed entry. Read should
	// surface the valid one and skip the malformed one without
	// crashing.
	src := "---\nschema: v1\n---\n\n" +
		"## 2026-05-06T10:00:00Z · operator · operator · tag:good\n\nGood body.\n\n" +
		"## not-a-timestamp · operator · operator · tag:bad\n\nBad body.\n\n" +
		"## 2026-05-06T11:00:00Z · operator · operator · tag:also-good\n\nMore good.\n"
	if err := os.WriteFile(filepath.Join(dir, "learnings.md"), []byte(src), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := Read("fleet")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("len=%d; want 2 (skip malformed)", len(got))
	}
}

func TestRead_SchemaTooNew(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	dir, err := state.ProjectDir("fleet")
	if err != nil {
		t.Fatalf("ProjectDir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	src := "---\nschema: v99\n---\n\n## 2026-05-06T10:00:00Z · operator · operator · tag:x\n\nx\n"
	if err := os.WriteFile(filepath.Join(dir, "learnings.md"), []byte(src), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err = Read("fleet")
	if err == nil {
		t.Fatal("Read returned nil; want ErrSchemaTooNew")
	}
	if !errors.Is(err, ErrSchemaTooNew) {
		t.Errorf("got %v; want ErrSchemaTooNew", err)
	}
}

func TestAppend_MissingProjectCreatesDir(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	e := &Entry{
		Timestamp: time.Now().UTC(),
		Author:    "operator",
		Tag:       "first",
		Body:      "bootstrap",
	}
	if err := Append("brand-new-project", e); err != nil {
		t.Fatalf("Append: %v", err)
	}
	dir, _ := state.ProjectDir("brand-new-project")
	if _, err := os.Stat(filepath.Join(dir, "learnings.md")); err != nil {
		t.Errorf("learnings.md not created: %v", err)
	}
}

func tagList(es []Entry) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.Tag)
	}
	return out
}
