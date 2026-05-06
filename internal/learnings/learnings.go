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
	if strings.TrimSpace(e.Author) == "" {
		return fmt.Errorf("%w: empty or whitespace-only author", ErrInvalidEntry)
	}
	if strings.TrimSpace(e.Tag) == "" {
		return fmt.Errorf("%w: empty or whitespace-only tag", ErrInvalidEntry)
	}
	// Reject fields that contain the H2 delimiter (" · ") or a
	// newline — both would produce a header that this package's
	// own parser cannot read back, silently demoting the entry to
	// a raw block on the next read. Self-corrupting writes are
	// rejected at the source.
	for _, f := range []struct{ name, val string }{
		{"author", e.Author},
		{"tag", e.Tag},
		{"task_slug", e.TaskSlug},
	} {
		if strings.Contains(f.val, " · ") {
			return fmt.Errorf("%w: %s contains delimiter ' · '", ErrInvalidEntry, f.name)
		}
		if strings.ContainsAny(f.val, "\r\n") {
			return fmt.Errorf("%w: %s contains newline", ErrInvalidEntry, f.name)
		}
	}
	// Body cannot contain a column-0 H2 line (`## ...`) — the
	// parser uses that as the entry-start anchor, so a body H2
	// would split the entry on the next read. Workers wanting
	// nested headings should use H3 (`### ...`) which the parser
	// keeps inside the body.
	for _, ln := range strings.Split(e.Body, "\n") {
		if strings.HasPrefix(ln, "## ") {
			return fmt.Errorf("%w: body line starts with '## ' (use ### for in-body headings)", ErrInvalidEntry)
		}
	}

	return withLock(project, func() error {
		path, err := learningsPath(project)
		if err != nil {
			return err
		}
		entries, raws, err := readEntriesAndRaw(path)
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		entries = append(entries, *e)
		return writeFile(path, entries, raws)
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
//
// Malformed (unparseable) blocks in the source file are preserved
// in-place — they stay in learnings.md, not in the archive — because
// we can't decide their age without a parseable timestamp. Operator
// can fix or remove them by hand.
//
// Retry-safe: a partial previous Prune (archive write succeeded but
// current write failed/crashed) leaves expired entries in BOTH files.
// On retry we dedupe against entries already in the archive so a
// re-run never appends duplicates — same invariant tasks.Archive
// upholds via slug equality.
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

		current, curRaws, err := readEntriesAndRaw(curPath)
		if err != nil {
			return fmt.Errorf("read current: %w", err)
		}
		archive, arcRaws, err := readEntriesAndRaw(arcPath)
		if err != nil {
			return fmt.Errorf("read archive: %w", err)
		}

		// Build a dedup set keyed on the wire-format identity (the
		// fields the parser reads back) so a previously-archived
		// entry isn't appended twice across a Prune retry. The set
		// is seeded ONLY from the pre-existing archive — duplicates
		// within the current pass are preserved because two
		// legitimately-identical entries (same RFC3339 second, same
		// header, same body) are valid in an append-only log and
		// must survive prune.
		archived := make(map[string]struct{}, len(archive))
		for _, a := range archive {
			archived[entryKey(a)] = struct{}{}
		}

		kept := make([]Entry, 0, len(current))
		for _, e := range current {
			if e.Timestamp.Before(olderThan) {
				if _, dup := archived[entryKey(e)]; !dup {
					archive = append(archive, e)
				}
				continue
			}
			kept = append(kept, e)
		}

		if err := writeFile(arcPath, archive, arcRaws); err != nil {
			return fmt.Errorf("write archive: %w", err)
		}
		if err := writeFile(curPath, kept, curRaws); err != nil {
			return fmt.Errorf("write current: %w", err)
		}
		return nil
	})
}

// entryKey produces a dedup key matching the on-disk identity of an
// Entry. RFC3339 normalizes Timestamp precision (writeFile writes
// second-resolution; freshly-Append'd entries may carry sub-second)
// so a current-side Entry round-trips to the same key as its
// archive-side copy. Body is included so two entries with identical
// headers but distinct bodies (rare but legal) don't collapse.
func entryKey(e Entry) string {
	return e.Timestamp.UTC().Format(time.RFC3339) + "|" +
		e.Author + "|" + e.TaskSlug + "|" + e.Tag + "|" + e.Body
}

// ---------- internals ----------

func learningsPath(project string) (string, error) {
	dir, err := state.ProjectDir(project)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "learnings.md"), nil
}

// rawBlock holds the verbatim text of an H2 block whose header we
// could not parse. We carry these through Append/Prune so a single
// malformed entry doesn't get silently deleted on the next rewrite —
// the file is operator memory, and "fail open / preserve unknowns"
// is the correct stance for a markdown log.
type rawBlock struct {
	header string   // the `<text>` after `## `
	body   []string // body lines verbatim, including leading + trailing blanks
}

// readEntries parses an entire learnings.md file. Missing file ⇒ empty
// slice (not error). Schema mismatch ⇒ ErrSchemaTooNew. Malformed
// blocks (bad header) are returned as a parallel rawBlock slice so the
// rewrite path can splice them back in unchanged.
func readEntries(path string) ([]Entry, error) {
	entries, _, err := readEntriesAndRaw(path)
	return entries, err
}

func readEntriesAndRaw(path string) ([]Entry, []rawBlock, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("read: %w", err)
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
				return nil, nil, fmt.Errorf("frontmatter line %d malformed: %q", i+1, line)
			}
			if k == "schema" {
				v = strings.TrimSpace(v)
				if !strings.HasPrefix(v, "v") {
					return nil, nil, fmt.Errorf("schema must be vN: %q", v)
				}
				var n int
				if _, ferr := fmt.Sscanf(v[1:], "%d", &n); ferr != nil {
					return nil, nil, fmt.Errorf("schema vN: %w", ferr)
				}
				if n > SchemaVersion {
					return nil, nil, fmt.Errorf("%w: file=v%d max=v%d", ErrSchemaTooNew, n, SchemaVersion)
				}
			}
		}
		if end < 0 {
			return nil, nil, fmt.Errorf("unterminated frontmatter")
		}
		idx = end + 1
	}

	// Walk H2 entries.
	var out []Entry
	var raws []rawBlock
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
		if perr != nil {
			// Preserve verbatim — caller will re-emit on rewrite
			// so a single bad entry doesn't get silently dropped.
			rawCopy := make([]string, len(body))
			copy(rawCopy, body)
			raws = append(raws, rawBlock{header: header, body: rawCopy})
			continue
		}
		// Trim a leading + trailing blank line on parsed body.
		for len(body) > 0 && strings.TrimSpace(body[0]) == "" {
			body = body[1:]
		}
		for len(body) > 0 && strings.TrimSpace(body[len(body)-1]) == "" {
			body = body[:len(body)-1]
		}
		entry.Body = strings.Join(body, "\n")
		out = append(out, entry)
	}
	return out, raws, nil
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

// writeFile renders frontmatter + parsed entries + preserved raw
// blocks (re-emitted verbatim) and atomic-publishes.
//
// Raws are appended after entries — they hold their original text
// including any internal whitespace, so the operator sees them in a
// stable position after each rewrite. We don't try to interleave by
// timestamp because raws have no parseable timestamp.
func writeFile(path string, entries []Entry, raws []rawBlock) error {
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
	for _, r := range raws {
		b.WriteString("\n## ")
		b.WriteString(r.header)
		b.WriteString("\n")
		for _, ln := range r.body {
			b.WriteString(ln)
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
