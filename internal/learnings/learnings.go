// Package learnings owns the per-project learnings.md log: append-only
// shared experience that workers and operators write to and the
// coordinator reads when assembling worker prompts.
//
// On-disk grammar (ENG §3.2):
//
//	---
//	schema: v1
//	---
//
//	## <RFC3339> · <author> · <task or "operator"> · tag:<topic>
//	<body>
//
//	## ...
//
// All writes go through state.WriteAtomic and serialize via
// state.ProjectStateLockPath (Q1 single state-lock per project state-
// dir). Even Append takes the lock — N concurrent goroutine appenders
// must each see a serialized read-modify-write so no entry is lost.
package learnings

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/edisonshen/fleet/internal/state"
)

// SchemaVersion is the on-disk schema this package emits. Files
// claiming a higher version are refused (callers handle ErrSchemaTooNew
// the same way tasks.md does).
const SchemaVersion = 1

// Entry is one ## H2 block in learnings.md. Body is everything between
// the H2 line and the next H2 (or EOF), with leading/trailing blanks
// trimmed.
type Entry struct {
	Timestamp time.Time
	Author    string
	TaskSlug  string
	Tag       string
	Body      string
}

// Errors.
var (
	ErrSchemaTooNew = errors.New("learnings.md schema newer than supported")
	ErrInvalidEntry = errors.New("invalid learnings entry")
)

// Append takes the project state-lock, reads, appends e, writes back.
// Atomically publishes the new file via state.WriteAtomic.
//
// The lock makes N concurrent appenders safe: each goroutine blocks on
// the flock, sees the previous appender's write, and adds its own
// entry. Without the lock a read-modify-write race would lose entries.
func Append(project string, e *Entry) error {
	if e == nil {
		return fmt.Errorf("%w: nil entry", ErrInvalidEntry)
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now().UTC()
	}
	if e.Author == "" {
		return fmt.Errorf("%w: empty author", ErrInvalidEntry)
	}
	if e.Tag == "" {
		return fmt.Errorf("%w: empty tag", ErrInvalidEntry)
	}

	return withLock(project, func() error {
		path, err := learningsPath(project)
		if err != nil {
			return err
		}
		entries, err := readEntries(path)
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		entries = append(entries, *e)
		return writeEntries(path, entries)
	})
}

// Read returns all entries in original (file) order. Used by the coord
// when assembling worker prompts (filtered/limited via Filter, but Read
// is still useful for debugging + the `fleet learnings list` CLI).
//
// Read does NOT take the lock. STATE.md A1 guarantees the file is
// never torn (rename-publish), so a reader either sees the old file or
// the new one.
func Read(project string) ([]Entry, error) {
	path, err := learningsPath(project)
	if err != nil {
		return nil, err
	}
	return readEntries(path)
}

// Filter returns up to limit entries whose Tag contains tagSubstr (if
// non-empty) AND whose TaskSlug equals taskSlug (if non-empty). Most
// recent first (reverse file order). limit <= 0 means no cap.
//
// The substring match on Tag is intentional — the worker-prompt
// assembly heuristic ("relevant prior learnings") wants to grab any
// entry whose tag _contains_ a topic keyword without forcing exact
// equality.
func Filter(project string, tagSubstr, taskSlug string, limit int) ([]Entry, error) {
	all, err := Read(project)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(all))
	// Reverse iteration so newest-first.
	for i := len(all) - 1; i >= 0; i-- {
		e := all[i]
		if tagSubstr != "" && !strings.Contains(e.Tag, tagSubstr) {
			continue
		}
		if taskSlug != "" && e.TaskSlug != taskSlug {
			continue
		}
		out = append(out, e)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// Prune moves entries older than olderThan to learnings-archive.md and
// rewrites learnings.md with the survivors. Atomic publish for both
// files; lock taken once across the whole operation.
func Prune(project string, olderThan time.Time) error {
	return withLock(project, func() error {
		curPath, err := learningsPath(project)
		if err != nil {
			return err
		}
		dir, err := state.ProjectDir(project)
		if err != nil {
			return err
		}
		arcPath := filepath.Join(dir, "learnings-archive.md")

		current, err := readEntries(curPath)
		if err != nil {
			return fmt.Errorf("read current: %w", err)
		}
		archive, err := readEntries(arcPath)
		if err != nil {
			return fmt.Errorf("read archive: %w", err)
		}

		kept := current[:0]
		for _, e := range current {
			if e.Timestamp.Before(olderThan) {
				archive = append(archive, e)
				continue
			}
			kept = append(kept, e)
		}
		current = kept

		if err := writeEntries(arcPath, archive); err != nil {
			return fmt.Errorf("write archive: %w", err)
		}
		if err := writeEntries(curPath, current); err != nil {
			return fmt.Errorf("write current: %w", err)
		}
		return nil
	})
}

// ---------- internals ----------

func learningsPath(project string) (string, error) {
	dir, err := state.ProjectDir(project)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "learnings.md"), nil
}

// readEntries parses an entire learnings.md file. Missing file ⇒ empty
// slice (not error). Schema mismatch ⇒ ErrSchemaTooNew.
func readEntries(path string) ([]Entry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read: %w", err)
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(text, "\n")

	// Frontmatter (optional in v0 files; auto-upgraded on next Write).
	idx := 0
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == "---" {
		// scan for closing ---
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
			}
		}
		if end < 0 {
			return nil, fmt.Errorf("unterminated frontmatter")
		}
		idx = end + 1
	}

	// Walk H2 entries.
	var out []Entry
	for idx < len(lines) {
		line := lines[idx]
		if !strings.HasPrefix(line, "## ") {
			idx++
			continue
		}
		header := strings.TrimPrefix(line, "## ")
		entry, perr := parseHeader(header)
		idx++
		// Body until next H2 or EOF.
		bodyStart := idx
		for idx < len(lines) {
			if strings.HasPrefix(lines[idx], "## ") {
				break
			}
			idx++
		}
		body := lines[bodyStart:idx]
		// Trim a leading + trailing blank line.
		for len(body) > 0 && strings.TrimSpace(body[0]) == "" {
			body = body[1:]
		}
		for len(body) > 0 && strings.TrimSpace(body[len(body)-1]) == "" {
			body = body[:len(body)-1]
		}
		entry.Body = strings.Join(body, "\n")
		if perr != nil {
			// Log via stderr would be wrong here (library code).
			// Skip the malformed entry — TestParseMalformed asserts
			// we don't crash + don't emit the entry.
			continue
		}
		out = append(out, entry)
	}
	return out, nil
}

// parseHeader splits "RFC3339 · author · task-or-op · tag:tag" into an
// Entry skeleton. Returns the partially-populated entry on success.
func parseHeader(h string) (Entry, error) {
	parts := strings.Split(h, " · ")
	if len(parts) != 4 {
		return Entry{}, fmt.Errorf("header expects 4 parts split by ' · ', got %d: %q", len(parts), h)
	}
	ts, err := time.Parse(time.RFC3339, strings.TrimSpace(parts[0]))
	if err != nil {
		return Entry{}, fmt.Errorf("timestamp: %w", err)
	}
	e := Entry{
		Timestamp: ts.UTC(),
		Author:    strings.TrimSpace(parts[1]),
	}
	taskField := strings.TrimSpace(parts[2])
	if taskField == "operator" {
		e.TaskSlug = ""
	} else if strings.HasPrefix(taskField, "task:") {
		e.TaskSlug = strings.TrimPrefix(taskField, "task:")
	} else {
		return Entry{}, fmt.Errorf("third field must be 'operator' or 'task:<slug>', got %q", taskField)
	}
	tagField := strings.TrimSpace(parts[3])
	if !strings.HasPrefix(tagField, "tag:") {
		return Entry{}, fmt.Errorf("fourth field must be 'tag:<topic>', got %q", tagField)
	}
	e.Tag = strings.TrimPrefix(tagField, "tag:")
	if e.Author == "" || e.Tag == "" {
		return Entry{}, fmt.Errorf("empty author or tag in header: %q", h)
	}
	return e, nil
}

// writeEntries renders frontmatter + entries and atomic-publishes.
func writeEntries(path string, entries []Entry) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir parent: %w", err)
	}
	var b strings.Builder
	b.WriteString("---\nschema: v")
	fmt.Fprintf(&b, "%d", SchemaVersion)
	b.WriteString("\n---\n")
	for _, e := range entries {
		b.WriteString("\n")
		fmt.Fprintf(&b, "## %s · %s · %s · tag:%s\n",
			e.Timestamp.UTC().Format(time.RFC3339),
			e.Author,
			taskOrOperator(e.TaskSlug),
			e.Tag,
		)
		if e.Body != "" {
			b.WriteString("\n")
			b.WriteString(e.Body)
			b.WriteString("\n")
		}
	}
	return state.WriteAtomic(path, []byte(b.String()))
}

func taskOrOperator(slug string) string {
	if slug == "" {
		return "operator"
	}
	return "task:" + slug
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

// withLock acquires the project state-lock (blocking flock) for the
// duration of fn. Reuses state.LockProjectState which lazily creates
// the .locks/ dir and serializes per-project state-dir writes.
func withLock(project string, fn func() error) error {
	release, err := state.LockProjectState(project)
	if err != nil {
		return err
	}
	defer release()
	return fn()
}
