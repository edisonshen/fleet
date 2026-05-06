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
