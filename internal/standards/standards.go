// Package standards owns the global + per-project standards.md files
// and their section-level merge (Q2 decision: per-H2 merge, project
// wins on same name; project-only sections append).
//
// On-disk grammar (ENG §3.3):
//
//	---
//	schema: v1
//	---
//
//	# Standards
//
//	## Section A
//	body
//
//	## Section B
//	body
//
// The parser cares only about frontmatter and `## ` boundaries; section
// bodies are opaque markdown preserved verbatim.
package standards

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/edisonshen/fleet/internal/state"
)

// SchemaVersion is the on-disk schema this package emits / accepts.
const SchemaVersion = 1

// Errors.
var (
	ErrSchemaTooNew = errors.New("standards.md schema newer than supported")
)

// From identifies which file a section came from. Useful for the
// coord skill's debug log when assembling worker prompts.
type From string

const (
	FromGlobal  From = "global"
	FromProject From = "project"
)

// Section is one `## H2` block.
type Section struct {
	Name string
	Body string
	From From
}

// Standards is the parsed shape. Sections preserve file order;
// merging preserves global order then appends project-only sections.
type Standards struct {
	Schema   int
	Sections []Section
}

// LoadGlobal reads ~/.fleet/standards.md. Missing file ⇒ empty
// Standards (callers can still call Render to get a v1 frontmatter
// stub).
func LoadGlobal() (*Standards, error) {
	root, err := state.Root()
	if err != nil {
		return nil, err
	}
	return load(filepath.Join(root, "standards.md"), FromGlobal)
}

// LoadProject reads ~/.fleet/projects/<name>/standards.md. Missing
// file ⇒ empty Standards.
func LoadProject(project string) (*Standards, error) {
	dir, err := state.ProjectDir(project)
	if err != nil {
		return nil, err
	}
	return load(filepath.Join(dir, "standards.md"), FromProject)
}

// Merge combines global + project per Q2: section-level per-H2.
//
// Algorithm:
//  1. Iterate global sections in order. For each: if project has a
//     same-named section, emit the project's (with From=project);
//     else emit global's.
//  2. After global, append every project-only section (with
//     From=project).
//
// nil inputs are treated as empty Standards.
func Merge(global, project *Standards) *Standards {
	if global == nil {
		global = &Standards{Schema: SchemaVersion}
	}
	if project == nil {
		project = &Standards{Schema: SchemaVersion}
	}
	projByName := make(map[string]Section, len(project.Sections))
	for _, s := range project.Sections {
		projByName[s.Name] = s
	}
	out := &Standards{Schema: SchemaVersion}
	emitted := make(map[string]bool, len(global.Sections))
	for _, g := range global.Sections {
		if p, ok := projByName[g.Name]; ok {
			out.Sections = append(out.Sections, Section{
				Name: p.Name,
				Body: p.Body,
				From: FromProject,
			})
		} else {
			out.Sections = append(out.Sections, Section{
				Name: g.Name,
				Body: g.Body,
				From: FromGlobal,
			})
		}
		emitted[g.Name] = true
	}
	// Project-only sections, in project file order.
	for _, p := range project.Sections {
		if emitted[p.Name] {
			continue
		}
		out.Sections = append(out.Sections, Section{
			Name: p.Name,
			Body: p.Body,
			From: FromProject,
		})
	}
	return out
}

// Render emits canonical bytes:
//
//	---
//	schema: v1
//	---
//
//	# Standards
//
//	## <name>
//	<body>
//
// The "# Standards" H1 is fixed (mirrors the operator-edited template).
// Section bodies are emitted verbatim with one trailing blank line.
func (s *Standards) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "---\nschema: v%d\n---\n\n# Standards\n", SchemaVersion)
	for _, sec := range s.Sections {
		fmt.Fprintf(&b, "\n## %s\n", sec.Name)
		if sec.Body != "" {
			b.WriteString("\n")
			b.WriteString(sec.Body)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// ---------- internals ----------

func load(path string, from From) (*Standards, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Standards{Schema: SchemaVersion}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return parse(data, from)
}

func parse(data []byte, from From) (*Standards, error) {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(text, "\n")

	s := &Standards{Schema: SchemaVersion}
	idx := 0
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == "---" {
		end := -1
		for i := 1; i < len(lines) && i < 32; i++ {
			line := lines[i]
			if strings.TrimSpace(line) == "---" {
				end = i
				break
			}
			k, v, ok := splitKV(line)
			if !ok {
				if strings.TrimSpace(line) == "" {
					continue
				}
				return nil, fmt.Errorf("frontmatter line %d malformed: %q", i+1, line)
			}
			if k == "schema" {
				v = strings.TrimSpace(v)
				if !strings.HasPrefix(v, "v") {
					return nil, fmt.Errorf("schema must be vN: %q", v)
				}
				var n int
				if _, ferr := fmt.Sscanf(v[1:], "%d", &n); ferr != nil {
					return nil, fmt.Errorf("schema vN: %w", ferr)
				}
				if n > SchemaVersion {
					return nil, fmt.Errorf("%w: file=v%d max=v%d", ErrSchemaTooNew, n, SchemaVersion)
				}
				s.Schema = n
			}
		}
		if end < 0 {
			return nil, fmt.Errorf("unterminated frontmatter")
		}
		idx = end + 1
	}

	// Skip the optional `# Standards` H1 — it's part of the canonical
	// shape but not an H2 we care about.
	for idx < len(lines) {
		line := lines[idx]
		if strings.HasPrefix(line, "## ") {
			break
		}
		idx++
	}

	for idx < len(lines) {
		line := lines[idx]
		if !strings.HasPrefix(line, "## ") {
			idx++
			continue
		}
		name := strings.TrimSpace(strings.TrimPrefix(line, "## "))
		idx++
		bodyStart := idx
		for idx < len(lines) {
			if strings.HasPrefix(lines[idx], "## ") {
				break
			}
			idx++
		}
		body := lines[bodyStart:idx]
		// Trim leading + trailing blank lines so bodies normalize
		// across writes.
		for len(body) > 0 && strings.TrimSpace(body[0]) == "" {
			body = body[1:]
		}
		for len(body) > 0 && strings.TrimSpace(body[len(body)-1]) == "" {
			body = body[:len(body)-1]
		}
		s.Sections = append(s.Sections, Section{
			Name: name,
			Body: strings.Join(body, "\n"),
			From: from,
		})
	}
	return s, nil
}

func splitKV(line string) (string, string, bool) {
	i := strings.Index(line, ":")
	if i < 0 {
		return "", "", false
	}
	k := strings.TrimSpace(line[:i])
	v := strings.TrimSpace(line[i+1:])
	if k == "" {
		return "", "", false
	}
	return k, v, true
}
