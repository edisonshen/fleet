package standards

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edisonshen/fleet/internal/state"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

const globalSrc = `---
schema: v1
---

# Standards

## Testing
- TDD required.

## Code review
- Run /review.
`

const projectOverrideSrc = `---
schema: v1
---

# Standards

## Testing
- TDD required.
- This project also requires fuzz tests.

## Project-only
- Custom rule.
`

func TestLoadGlobal_Missing(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	g, err := LoadGlobal()
	if err != nil {
		t.Fatalf("LoadGlobal missing: %v", err)
	}
	if g == nil || len(g.Sections) != 0 {
		t.Errorf("got %+v; want empty Standards", g)
	}
}

func TestLoadGlobal_ParsesSections(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	writeFile(t, filepath.Join(tmp, "standards.md"), globalSrc)
	g, err := LoadGlobal()
	if err != nil {
		t.Fatalf("LoadGlobal: %v", err)
	}
	if len(g.Sections) != 2 {
		t.Fatalf("got %d sections; want 2", len(g.Sections))
	}
	if g.Sections[0].Name != "Testing" || g.Sections[1].Name != "Code review" {
		t.Errorf("got names %v", names(g.Sections))
	}
	if g.Sections[0].From != FromGlobal {
		t.Errorf("from=%q; want global", g.Sections[0].From)
	}
}

func TestMergeGlobalOnly(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	writeFile(t, filepath.Join(tmp, "standards.md"), globalSrc)
	g, err := LoadGlobal()
	if err != nil {
		t.Fatalf("LoadGlobal: %v", err)
	}
	merged := Merge(g, nil)
	if len(merged.Sections) != 2 {
		t.Errorf("len=%d; want 2 (global only)", len(merged.Sections))
	}
	for _, s := range merged.Sections {
		if s.From != FromGlobal {
			t.Errorf("section %q From=%q; want global", s.Name, s.From)
		}
	}
}

func TestMergeProjectOverride(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	writeFile(t, filepath.Join(tmp, "standards.md"), globalSrc)
	dir, _ := state.ProjectDir("fleet")
	writeFile(t, filepath.Join(dir, "standards.md"), projectOverrideSrc)

	g, err := LoadGlobal()
	if err != nil {
		t.Fatalf("LoadGlobal: %v", err)
	}
	p, err := LoadProject("fleet")
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	merged := Merge(g, p)

	// Expected order: Testing (project-overridden), Code review
	// (global), Project-only (project, appended).
	want := []struct {
		name string
		from From
	}{
		{"Testing", FromProject},
		{"Code review", FromGlobal},
		{"Project-only", FromProject},
	}
	if len(merged.Sections) != len(want) {
		t.Fatalf("len=%d; want %d", len(merged.Sections), len(want))
	}
	for i, w := range want {
		if merged.Sections[i].Name != w.name || merged.Sections[i].From != w.from {
			t.Errorf("section %d: got %+v; want %+v", i, merged.Sections[i], w)
		}
	}
	// Project body should win for Testing.
	if !strings.Contains(merged.Sections[0].Body, "fuzz tests") {
		t.Errorf("Testing body did not pick up project override; got %q", merged.Sections[0].Body)
	}
}

func TestMergeProjectAddSection(t *testing.T) {
	g := &Standards{Sections: []Section{{Name: "A", Body: "global A", From: FromGlobal}}}
	p := &Standards{Sections: []Section{
		{Name: "B", Body: "project B", From: FromProject},
		{Name: "C", Body: "project C", From: FromProject},
	}}
	m := Merge(g, p)
	if len(m.Sections) != 3 {
		t.Fatalf("len=%d; want 3", len(m.Sections))
	}
	if m.Sections[0].Name != "A" || m.Sections[1].Name != "B" || m.Sections[2].Name != "C" {
		t.Errorf("got %v; want A B C", names(m.Sections))
	}
}

func TestRenderRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	writeFile(t, filepath.Join(tmp, "standards.md"), globalSrc)
	g, err := LoadGlobal()
	if err != nil {
		t.Fatalf("LoadGlobal: %v", err)
	}
	rendered := g.Render()
	// Re-parse rendered output → same section names + bodies.
	g2, err := parse([]byte(rendered), FromGlobal)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if len(g.Sections) != len(g2.Sections) {
		t.Fatalf("len mismatch: %d vs %d", len(g.Sections), len(g2.Sections))
	}
	for i := range g.Sections {
		if g.Sections[i].Name != g2.Sections[i].Name || g.Sections[i].Body != g2.Sections[i].Body {
			t.Errorf("round-trip mismatch at %d: %+v vs %+v", i, g.Sections[i], g2.Sections[i])
		}
	}
}

func TestSchemaTooNew(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	writeFile(t, filepath.Join(tmp, "standards.md"), "---\nschema: v99\n---\n\n# Standards\n")
	_, err := LoadGlobal()
	if err == nil {
		t.Fatal("LoadGlobal returned nil; want ErrSchemaTooNew")
	}
	if !errors.Is(err, ErrSchemaTooNew) {
		t.Errorf("got %v; want ErrSchemaTooNew", err)
	}
}

func TestRender_EmitsSchemaFrontmatter(t *testing.T) {
	s := &Standards{Sections: []Section{{Name: "X", Body: "x"}}}
	got := s.Render()
	if !strings.HasPrefix(got, "---\nschema: v1\n---\n") {
		t.Errorf("missing frontmatter; got prefix %q", got[:32])
	}
}

func TestParse_FencedH2StaysInBody(t *testing.T) {
	// Section bodies are opaque markdown. A fenced ```...``` example
	// containing a column-0 `## Example` must NOT split the section.
	// Pre-fix the parser saw the inner `## ` and started a new section,
	// shredding both the body of the outer section and the file's
	// section count.
	src := "---\nschema: v1\n---\n\n# Standards\n\n" +
		"## Testing\n\n" +
		"Use fenced examples liberally:\n\n" +
		"```\n## Example header (inside fence)\nstill body\n```\n\n" +
		"trailing line.\n\n" +
		"## Code review\n\n" +
		"Reviewers must be ruthless.\n"
	t.Setenv("FLEET_HOME", t.TempDir())
	writeFile(t, filepath.Join(os.Getenv("FLEET_HOME"), "standards.md"), src)
	g, err := LoadGlobal()
	if err != nil {
		t.Fatalf("LoadGlobal: %v", err)
	}
	if len(g.Sections) != 2 {
		t.Fatalf("got %d sections; want 2 — fenced `## ` was treated as a header", len(g.Sections))
	}
	if g.Sections[0].Name != "Testing" {
		t.Errorf("first section name=%q; want Testing", g.Sections[0].Name)
	}
	if !strings.Contains(g.Sections[0].Body, "## Example header (inside fence)") {
		t.Errorf("Testing body lost the fenced `## Example` line:\n%s", g.Sections[0].Body)
	}
	if !strings.Contains(g.Sections[0].Body, "trailing line.") {
		t.Errorf("Testing body truncated before fence closed:\n%s", g.Sections[0].Body)
	}
}

func names(secs []Section) []string {
	out := make([]string, 0, len(secs))
	for _, s := range secs {
		out = append(out, s.Name)
	}
	return out
}
