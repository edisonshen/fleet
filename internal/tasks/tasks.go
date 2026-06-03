// Package tasks owns the per-project tasks.md registry: parser, writer,
// archive, and slug generation. See docs/PLAN-v0.2-coordinator.md and
// docs/ENG-v0.2-coordinator.md §3.1 for the on-disk grammar.
//
// The file is markdown with a YAML-frontmatter schema header and one
// `## task: <slug>` block per task. The grammar is deliberately
// hand-rolled (no yaml dep) — the bounded `key: value` shape between
// `---` lines is small enough that pulling in a parser would be
// overkill.
//
// All writes go through state.WriteAtomic; callers serialize via
// state.ProjectStateLockPath when concurrent writers are possible.
package tasks

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/edisonshen/fleet/internal/state"
)

// SchemaVersion is the on-disk schema this package produces. Files
// claiming a higher version are refused with ErrSchemaTooNew.
const SchemaVersion = 1

// Status enum (ENG §3.1). Stored verbatim in the markdown.
type Status string

const (
	StatusTodo       Status = "todo"
	StatusReady      Status = "ready"
	StatusInProgress Status = "in-progress"
	StatusInReview   Status = "in-review"
	StatusDone       Status = "done"
	StatusBlocked    Status = "blocked"
	StatusAbandoned  Status = "abandoned"
)

func validStatus(s Status) bool {
	switch s {
	case StatusTodo, StatusReady, StatusInProgress, StatusInReview,
		StatusDone, StatusBlocked, StatusAbandoned:
		return true
	}
	return false
}

// Priority enum.
type Priority string

const (
	PriorityP0 Priority = "P0"
	PriorityP1 Priority = "P1"
	PriorityP2 Priority = "P2"
	PriorityP3 Priority = "P3"
)

func validPriority(p Priority) bool {
	switch p {
	case PriorityP0, PriorityP1, PriorityP2, PriorityP3:
		return true
	}
	return false
}

// requiredTaskBullets enumerates every `- key:` bullet a well-formed
// task block must carry. Keep in sync with setKV — these are the keys
// Write emits, so the parser must refuse to accept anything missing
// (otherwise the next Write zero-values them).
//
// started_at / finished_at are intentionally NOT required: they were
// added in v0.6.x as additive lifecycle bullets. Old rows (created
// before the change) lack the lines and must still parse cleanly. The
// writer ALWAYS emits both bullets — empty string for unset values —
// so a re-saved old row picks up the new shape without a migration
// script. See `requiredTaskBullets`'s comment in parse.py for the
// matching Python contract.
var requiredTaskBullets = []string{
	"status", "priority", "worker_pid", "worktree", "pr_url", "branch",
	"created", "updated", "depends_on", "spawned_by",
}

// Lifecycle bullets started_at + finished_at are recognized on read
// (see setKV) but tolerated as missing on existing rows. The writer
// always emits them (empty for unset values) so re-saved rows pick up
// the new shape.

// Task is one entry in tasks.md. Workers are NOT Fleet agents; they're
// `claude --print` subprocesses. WorkerPID is the OS PID of that
// subprocess (0 = no worker active). Spec / Acceptance / Notes are the
// raw markdown bodies of the H3 sections, preserved verbatim across
// round-trip (so worker-edited notes never get clobbered).
type Task struct {
	Slug       string
	Status     Status
	Priority   Priority
	WorkerPID  int
	Worktree   string
	PRURL      string
	Branch     string
	Created    time.Time
	Updated    time.Time
	StartedAt  time.Time // First time status flipped to in-progress; sticky.
	FinishedAt time.Time // Most recent flip into done/abandoned; cleared on reopen.
	// DispatchGeneration is the coord-owned per-slug fence token
	// (DESIGN-coord-worktree-lifecycle §1). Absent / legacy rows read as
	// 0; the coord increments by 1 on every genuine (re-)dispatch.
	// Additive in this PR: parsed + rendered only; no writer sets it yet
	// beyond `fleet tasks set`.
	DispatchGeneration int
	// Parked is a durable "leave this worktree/worker-dir alone" marker
	// (DESIGN §4.2): a free-form `<UTC ts + reason>` string set when a
	// dirty worktree is parked, cleared on resolve. Empty = not parked.
	// Additive in this PR: parsed + rendered only; no writer sets it yet
	// beyond `fleet tasks set`.
	Parked     string
	DependsOn  []string
	SpawnedBy  string
	Spec       string
	Acceptance string
	Notes      string
}

// File is the parsed shape of tasks.md. Schema is the version read off
// the frontmatter; missing-frontmatter files are read as 0 and lazy-
// upgraded to SchemaVersion on the next Write.
//
// Footer holds any trailing markdown after the last `## task:` block —
// the grammar (ENG §3.1) allows arbitrary footer text, preserved
// verbatim. Footer detection requires the trailing region to start
// with a `## ` (non-task H2) header; the grammar's `body := text up
// to next ## or ### or EOF` rule means a paragraph or list item
// directly after `### Notes` is treated as Notes body, NOT footer.
// Operators wanting cross-task notes should anchor them with an
// explicit `## Operator notes` header.
type File struct {
	Schema int
	Tasks  []*Task
	Footer string
}

// Errors. All carry context via fmt.Errorf("...: %w", base).
var (
	ErrSchemaTooNew  = errors.New("tasks.md schema version newer than this binary supports")
	ErrDuplicateSlug = errors.New("duplicate task slug")
	ErrTaskNotFound  = errors.New("task not found")
	ErrInvalidTask   = errors.New("invalid task")
)

// ParseError carries line + column + raw line for actionable errors.
// Coord skill / CLI surface these to the operator.
type ParseError struct {
	Line int
	Col  int
	Raw  string
	Msg  string
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("tasks.md:%d:%d: %s (line: %q)", e.Line, e.Col, e.Msg, e.Raw)
}

// Read parses tasks.md from path. Returns ErrSchemaTooNew if the file
// claims a schema version > SchemaVersion. ENOENT returns an empty
// *File at the current schema (callers can Write to create the file).
func Read(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &File{Schema: SchemaVersion}, nil
		}
		return nil, fmt.Errorf("read tasks.md: %w", err)
	}
	return parse(data)
}

// Write atomically publishes f to path. Always emits frontmatter at
// the current SchemaVersion regardless of what was read (this is the
// lazy-upgrade path for v0 files).
//
// Caller is responsible for serializing concurrent writers via
// state.ProjectStateLockPath. The tmp+rename inside WriteAtomic is
// crash-safe but not race-safe across processes.
func Write(path string, f *File) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir parent: %w", err)
	}
	out, err := render(f)
	if err != nil {
		return err
	}
	return state.WriteAtomic(path, out)
}

// Get looks up a task by slug.
func (f *File) Get(slug string) (*Task, error) {
	for _, t := range f.Tasks {
		if t.Slug == slug {
			return t, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrTaskNotFound, slug)
}

// Add appends a task; returns ErrDuplicateSlug if Slug is already in
// use. Validates Status/Priority enums, non-zero Created/Updated, and
// rejects column-0 H2 lines in Spec/Acceptance/Notes (would self-corrupt
// the file: Read uses `## ` at column 0 as the task-block boundary).
func (f *File) Add(t *Task) error {
	if t == nil {
		return fmt.Errorf("%w: nil task", ErrInvalidTask)
	}
	if err := state.ValidateSlug(t.Slug); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidTask, err)
	}
	if !validStatus(t.Status) {
		return fmt.Errorf("%w: status=%q", ErrInvalidTask, t.Status)
	}
	if !validPriority(t.Priority) {
		return fmt.Errorf("%w: priority=%q", ErrInvalidTask, t.Priority)
	}
	// Non-zero Created/Updated. Archive uses Created.Equal() to
	// distinguish retry-recovery from slug-reuse; two zero-valued
	// Created tasks would compare equal and let the second silently
	// drop on Archive (codex iter-12 finding).
	if t.Created.IsZero() {
		return fmt.Errorf("%w: created must be set", ErrInvalidTask)
	}
	if t.Updated.IsZero() {
		return fmt.Errorf("%w: updated must be set", ErrInvalidTask)
	}
	// Spec/Acceptance/Notes are free-form markdown but two markers
	// would terminate the task block on next parse:
	//   - column-0 `## ` (next task header)
	//   - column-0 reserved `### Spec|Acceptance|Notes` (next H3 section)
	// Reject both when they appear OUTSIDE a fenced code block so the
	// writer never produces a file it can't read back. Fenced examples
	// are allowed; readSection honors the same fence rule on the read
	// side. Non-reserved `### ` is fine (it's body content for us).
	for _, sec := range []struct{ name, body string }{
		{"spec", t.Spec},
		{"acceptance", t.Acceptance},
		{"notes", t.Notes},
	} {
		inFence := false
		for _, ln := range strings.Split(sec.body, "\n") {
			if isFenceMarker(ln) {
				inFence = !inFence
				continue
			}
			if inFence {
				continue
			}
			if strings.HasPrefix(ln, "## ") {
				return fmt.Errorf("%w: %s body has unfenced column-0 '## ' (use ### for in-section headings, or wrap in ``` for examples)", ErrInvalidTask, sec.name)
			}
			if strings.HasPrefix(ln, "### ") {
				name := strings.TrimSpace(strings.TrimPrefix(ln, "### "))
				if isReservedH3(name) {
					return fmt.Errorf("%w: %s body contains unfenced reserved heading '### %s' (wrap in ``` for examples)", ErrInvalidTask, sec.name, name)
				}
			}
		}
	}
	for _, existing := range f.Tasks {
		if existing.Slug == t.Slug {
			return fmt.Errorf("%w: %s", ErrDuplicateSlug, t.Slug)
		}
	}
	f.Tasks = append(f.Tasks, t)
	return nil
}

// Archive moves the named slugs from <project>/tasks.md to
// <project>/tasks-archive.md. Both writes go through WriteAtomic.
//
// Internally takes state.LockProjectState so callers don't need to
// (mirrors learnings.Prune). Order: write archive first (additive),
// then write tasks (removal). On crash between the two writes, slugs
// appear in BOTH files — coord reconciles by treating tasks.md as the
// source of truth (presence wins). This bias matches v0.2's Ralph
// rule: tasks.md is the durable plan; archive is operational debris.
//
// Slugs not present in tasks.md are silently skipped (callers can
// detect this by diffing before/after task counts if needed).
func Archive(project string, slugs []string) error {
	release, err := state.LockProjectState(project)
	if err != nil {
		return fmt.Errorf("lock: %w", err)
	}
	defer release()

	dir, err := state.ProjectDir(project)
	if err != nil {
		return err
	}
	tasksPath := filepath.Join(dir, "tasks.md")
	archivePath := filepath.Join(dir, "tasks-archive.md")

	current, err := Read(tasksPath)
	if err != nil {
		return fmt.Errorf("read tasks.md: %w", err)
	}
	archive, err := Read(archivePath)
	if err != nil {
		return fmt.Errorf("read tasks-archive.md: %w", err)
	}

	want := make(map[string]struct{}, len(slugs))
	for _, s := range slugs {
		want[s] = struct{}{}
	}

	// Build a fresh kept slice instead of aliasing current.Tasks[:0].
	// The aliased form was correct (range copies the header before
	// the loop body mutates), but the explicit allocation is harder
	// to misread on a future edit.
	//
	// Dedupe against the archive: a partial previous Archive (crashed
	// between writing archive and writing tasks) leaves the same
	// slug in BOTH files; the retry would otherwise append a
	// duplicate to archive, breaking subsequent Read with
	// ErrDuplicateSlug.
	//
	// Use Task.Created (immutable from Add) as the identity check:
	//   - same Created  → retry-recovery case, keep archive entry as-is
	//   - diff Created  → slug was REUSED after archive (Add doesn't
	//                     check archive today; future Phase B should).
	//                     Surface ErrDuplicateSlug rather than silently
	//                     dropping the newer record.
	archived := make(map[string]*Task, len(archive.Tasks))
	for _, t := range archive.Tasks {
		archived[t.Slug] = t
	}
	kept := make([]*Task, 0, len(current.Tasks))
	for _, t := range current.Tasks {
		if _, ok := want[t.Slug]; ok {
			if existing, dup := archived[t.Slug]; dup {
				if !existing.Created.Equal(t.Created) {
					return fmt.Errorf("%w: %s reused after archive (archive Created=%s, current Created=%s)",
						ErrDuplicateSlug, t.Slug,
						existing.Created.UTC().Format(time.RFC3339),
						t.Created.UTC().Format(time.RFC3339))
				}
				// Retry-recovery — refresh the archive entry's mutable
				// fields from the live row before dropping it. If an
				// operator edited the still-visible task between the
				// failed Archive and this retry (issue #37), the stale
				// archived snapshot would otherwise lose those edits.
				// Slug + Created are immutable identity; everything
				// else is operator-mutable state we want to preserve.
				existing.Status = t.Status
				existing.Priority = t.Priority
				existing.WorkerPID = t.WorkerPID
				existing.Worktree = t.Worktree
				existing.PRURL = t.PRURL
				existing.Branch = t.Branch
				existing.Updated = t.Updated
				existing.DispatchGeneration = t.DispatchGeneration
				existing.Parked = t.Parked
				existing.DependsOn = t.DependsOn
				existing.SpawnedBy = t.SpawnedBy
				existing.Spec = t.Spec
				existing.Acceptance = t.Acceptance
				existing.Notes = t.Notes
				continue
			}
			archive.Tasks = append(archive.Tasks, t)
			continue
		}
		kept = append(kept, t)
	}
	current.Tasks = kept

	if err := Write(archivePath, archive); err != nil {
		return fmt.Errorf("write archive: %w", err)
	}
	if err := Write(tasksPath, current); err != nil {
		return fmt.Errorf("write tasks: %w", err)
	}
	return nil
}

// GenerateSlug implements the v0.2 slug rule (PLAN §"Slug input rule"):
//   - short empty + spec given     → derive short from spec first line
//   - short looks like full slug   → return as-is (caller checks dup)
//   - else                         → "<short>-<4hex>"
//
// The 4hex random suffix ensures uniqueness across operators / agents
// generating slugs concurrently. existing is used only for the rare
// case of a 4hex collision (~1 in 65536); we re-roll up to 8 times.
func GenerateSlug(short, spec string, existing []string) string {
	if isFullSlug(short) {
		return short
	}
	if short == "" {
		short = deriveShort(spec)
	}
	short = sanitizeShort(short)
	if short == "" {
		short = "task"
	}

	used := make(map[string]struct{}, len(existing))
	for _, s := range existing {
		used[s] = struct{}{}
	}
	for attempt := 0; attempt < 8; attempt++ {
		candidate := short + "-" + randHex4()
		if _, taken := used[candidate]; !taken {
			return candidate
		}
	}
	// Pathologically unlikely; return the last attempt rather than
	// loop forever. Caller's Add will surface ErrDuplicateSlug and
	// the operator can retry.
	return short + "-" + randHex4()
}

// fullSlugRe-ish check: lowercase + digits + hyphens, ending in -<4hex>.
func isFullSlug(s string) bool {
	if len(s) < 6 {
		return false
	}
	if s[len(s)-5] != '-' {
		return false
	}
	for _, c := range s[:len(s)-5] {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	for _, c := range s[len(s)-4:] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// deriveShort takes the first non-empty line of spec, kebab-cases it,
// and truncates to 24 chars. Deterministic given the spec.
func deriveShort(spec string) string {
	for _, line := range strings.Split(spec, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		return line
	}
	return ""
}

// sanitizeShort lowercases, replaces non-[a-z0-9] runs with single
// hyphens, trims leading/trailing hyphens, truncates to 24 chars.
func sanitizeShort(s string) string {
	s = strings.ToLower(s)
	out := make([]byte, 0, len(s))
	prevHy := true // start: skip leading hyphens
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			out = append(out, c)
			prevHy = false
		default:
			if !prevHy {
				out = append(out, '-')
				prevHy = true
			}
		}
	}
	// Trim trailing hyphen.
	for len(out) > 0 && out[len(out)-1] == '-' {
		out = out[:len(out)-1]
	}
	if len(out) > 24 {
		out = out[:24]
		// Re-trim if truncation left a trailing hyphen.
		for len(out) > 0 && out[len(out)-1] == '-' {
			out = out[:len(out)-1]
		}
	}
	return string(out)
}

func randHex4() string {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Fallback to nanosecond entropy; vanishingly rare.
		ns := time.Now().UnixNano()
		return fmt.Sprintf("%04x", ns&0xffff)
	}
	return hex.EncodeToString(b[:])
}

// ---------- parser ----------

// parse drives the full file: frontmatter then task blocks.
func parse(data []byte) (*File, error) {
	// Normalize CRLF → LF before splitting; CRLF tolerance is part
	// of TestFrontmatterRoundTrip's contract and avoids per-line
	// stripping noise inside the parser proper.
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(text, "\n")

	f := &File{}
	idx := 0
	schema, consumed, err := parseFrontmatter(lines)
	if err != nil {
		return nil, err
	}
	f.Schema = schema
	if schema > SchemaVersion {
		return nil, fmt.Errorf("%w: file schema=v%d, max=v%d", ErrSchemaTooNew, schema, SchemaVersion)
	}
	idx = consumed

	// Walk remaining lines: on `## task: <slug>` parse a task block,
	// on any other `## <name>` we've reached the footer (file-level
	// markdown after the last task). Footer is captured verbatim.
	footerStart := -1
	for idx < len(lines) {
		line := lines[idx]
		if strings.HasPrefix(line, "## task: ") {
			t, next, perr := parseTaskBlock(lines, idx)
			if perr != nil {
				return nil, perr
			}
			// Duplicate detection during read — corrupted file
			// shouldn't silently merge entries.
			for _, existing := range f.Tasks {
				if existing.Slug == t.Slug {
					return nil, fmt.Errorf("%w: %s", ErrDuplicateSlug, t.Slug)
				}
			}
			f.Tasks = append(f.Tasks, t)
			idx = next
			continue
		}
		if strings.HasPrefix(line, "## ") {
			// Non-task H2 → footer territory. Anything from this
			// line forward (including this header) is footer.
			// If a later `## task:` would otherwise appear after
			// this point, it'd silently disappear into the footer
			// blob — surface that as a parse error so the operator
			// fixes the layout.
			for j := idx + 1; j < len(lines); j++ {
				if strings.HasPrefix(lines[j], "## task: ") {
					return nil, &ParseError{Line: idx + 1, Col: 1, Raw: line, Msg: "footer header appears before later task block — move footer to end of file"}
				}
			}
			footerStart = idx
			break
		}
		idx++
	}
	if footerStart >= 0 {
		footer := lines[footerStart:]
		// Drop the trailing empty entry strings.Split injects when
		// the file ends in '\n' — otherwise we'd re-emit an extra
		// blank on round-trip.
		if len(footer) > 0 && footer[len(footer)-1] == "" {
			footer = footer[:len(footer)-1]
		}
		if len(footer) > 0 {
			f.Footer = strings.Join(footer, "\n")
		}
	}
	return f, nil
}

// parseFrontmatter inspects lines[0..]. Returns the schema int (0 if
// no frontmatter) and the number of lines to skip past.
func parseFrontmatter(lines []string) (schema int, consumed int, err error) {
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return 0, 0, nil // no-frontmatter case (auto-upgrade on Write)
	}
	// Scan for the closing ---. Bounded ~10 lines; refuse anything weirder.
	for i := 1; i < len(lines) && i < 32; i++ {
		line := lines[i]
		if strings.TrimSpace(line) == "---" {
			return schema, i + 1, nil
		}
		// `key: value` lines. We only care about `schema: v<int>`.
		k, v, ok := splitKV(line)
		if !ok {
			// blank line inside frontmatter is tolerated
			if strings.TrimSpace(line) == "" {
				continue
			}
			return 0, 0, &ParseError{Line: i + 1, Col: 1, Raw: line, Msg: "frontmatter expects key: value or ---"}
		}
		if k == "schema" {
			n, perr := parseSchemaValue(v)
			if perr != nil {
				return 0, 0, &ParseError{Line: i + 1, Col: 1, Raw: line, Msg: perr.Error()}
			}
			schema = n
		}
	}
	return 0, 0, &ParseError{Line: 1, Col: 1, Raw: lines[0], Msg: "unterminated frontmatter (missing closing ---)"}
}

func parseSchemaValue(v string) (int, error) {
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, "v") {
		return 0, fmt.Errorf("schema must be vN, got %q", v)
	}
	n, err := strconv.Atoi(v[1:])
	if err != nil {
		return 0, fmt.Errorf("schema vN: %w", err)
	}
	return n, nil
}

// parseTaskBlock parses one `## task: <slug>` block starting at lines[start].
// Returns the task and the index of the line AFTER the block (the next
// `## task: ` header or end-of-file).
func parseTaskBlock(lines []string, start int) (*Task, int, error) {
	t := &Task{}
	// 1. Slug from the H2 line.
	header := lines[start]
	slug := strings.TrimPrefix(header, "## task: ")
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return nil, 0, &ParseError{Line: start + 1, Col: 1, Raw: header, Msg: "task header missing slug"}
	}
	// Reject slugs that would alias on disk via state.SafeLockComponent
	// — `feature/a` and `feature_a` would otherwise collapse onto the
	// same workers/<x>/ directory at runtime.
	if err := state.ValidateSlug(slug); err != nil {
		return nil, 0, &ParseError{Line: start + 1, Col: 1, Raw: header, Msg: "invalid slug: " + err.Error()}
	}
	t.Slug = slug

	// 2. Walk forward through key bullets and H3 sections until next H2 / EOF.
	idx := start + 1
	// Skip leading blanks.
	for idx < len(lines) && strings.TrimSpace(lines[idx]) == "" {
		idx++
	}
	// Key bullets: `- key: value` until we hit a blank line or H3.
	// Track which keys appeared so a duplicate (e.g. two `status:`
	// bullets) raises an error instead of silently overwriting the
	// first value.
	seenKey := make(map[string]bool, 11)
	for idx < len(lines) {
		line := lines[idx]
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			idx++
			break
		}
		if !strings.HasPrefix(line, "- ") {
			break
		}
		k, v, ok := splitKV(strings.TrimPrefix(line, "- "))
		if !ok {
			return nil, 0, &ParseError{Line: idx + 1, Col: 1, Raw: line, Msg: "expected `- key: value`"}
		}
		if seenKey[k] {
			return nil, 0, &ParseError{Line: idx + 1, Col: 1, Raw: line, Msg: "duplicate task field: " + k}
		}
		seenKey[k] = true
		if err := setKV(t, k, v, idx+1, line); err != nil {
			return nil, 0, err
		}
		idx++
	}

	// H3 sections (Spec / Acceptance / Notes), in any order. Track
	// which sections have been parsed so a duplicate H3 raises an
	// error (instead of silently overwriting the earlier body).
	//
	// Stray non-blank text between bullets and the first H3 (or
	// between H3 sections) would otherwise be silently dropped on
	// the next Write — surface it as a ParseError so operator-edit
	// mistakes are visible. Blank lines are tolerated (they're the
	// canonical separators).
	seenH3 := make(map[string]bool, 3)
	for idx < len(lines) {
		line := lines[idx]
		if strings.HasPrefix(line, "## ") {
			break // next task block (or non-task H2 — footer)
		}
		if strings.HasPrefix(line, "### ") {
			name := strings.TrimSpace(strings.TrimPrefix(line, "### "))
			if !isReservedH3(name) {
				// Non-reserved H3 outside any section's body —
				// operator-edit error (e.g. `### Followup` sitting
				// alone before `### Spec`). Surface as ParseError.
				return nil, 0, &ParseError{Line: idx + 1, Col: 1, Raw: line, Msg: "unknown H3 section: " + name}
			}
			body, next := readSection(lines, idx+1)
			if seenH3[name] {
				return nil, 0, &ParseError{Line: idx + 1, Col: 1, Raw: line, Msg: "duplicate ### " + name + " section"}
			}
			seenH3[name] = true
			switch name {
			case "Spec":
				t.Spec = body
			case "Acceptance":
				t.Acceptance = body
			case "Notes":
				t.Notes = body
			}
			idx = next
			continue
		}
		if strings.TrimSpace(line) != "" {
			return nil, 0, &ParseError{Line: idx + 1, Col: 1, Raw: line, Msg: "unexpected content between sections (allowed: blank lines, ### Spec/Acceptance/Notes)"}
		}
		idx++
	}

	if !validStatus(t.Status) {
		return nil, 0, &ParseError{Line: start + 1, Col: 1, Raw: header, Msg: "invalid status: " + string(t.Status)}
	}
	if !validPriority(t.Priority) {
		return nil, 0, &ParseError{Line: start + 1, Col: 1, Raw: header, Msg: "invalid priority: " + string(t.Priority)}
	}
	// All bullet keys must be present. If an operator edit or merge
	// conflict drops e.g. `- created:`, the in-memory Task carries a
	// zero value and the next Write silently rewrites the file with
	// `0001-01-01T00:00:00Z` / empty deps — destroying task state. List
	// matches the keys recognized by setKV; keep in sync.
	for _, key := range requiredTaskBullets {
		if !seenKey[key] {
			return nil, 0, &ParseError{Line: start + 1, Col: 1, Raw: header, Msg: "task missing required field: - " + key}
		}
	}
	// All three reserved H3 sections must be present (per ENG §3.1
	// `task-block := ..., h3-section+`). Missing sections would be
	// silently synthesized as empty by Write — surface the operator
	// edit error instead.
	for _, name := range []string{"Spec", "Acceptance", "Notes"} {
		if !seenH3[name] {
			return nil, 0, &ParseError{Line: start + 1, Col: 1, Raw: header, Msg: "task missing required ### " + name + " section"}
		}
	}

	return t, idx, nil
}

// readSection reads lines forming the body of an H3 section, stopping
// at the next H2 OR a known H3 section name (Spec/Acceptance/Notes)
// or EOF. Bodies are free-form markdown — H1 (`# Title`) and arbitrary
// `### Subheading` lines whose name is NOT one of our reserved
// section names are kept inside the body, so worker/operator notes
// with subheadings like `### Follow-up` survive round-trip.
//
// The canonical layout is:
//
//	### Section
//	<blank>
//	body line 1
//	body line 2
//	<blank>
//	### NextSection
//
// We skip exactly one leading blank (the separator between header and
// body) and trim the trailing blank(s) so the round-trip is stable.
// Multi-paragraph bodies (with internal blank lines) round-trip
// verbatim because trimming is anchored at start/end only.
//
// Code-fence safety: lines inside a ```-delimited fenced code block
// are body content, never structural headers. A fenced example like:
//
//	```
//	### Acceptance
//	some pasted markdown
//	```
//
// must stay inside the section body. We toggle inFence on every
// column-0 line whose trimmed prefix is ```, matching CommonMark's
// fenced-code-block opener/closer rules at indent 0.
func readSection(lines []string, start int) (string, int) {
	end := start
	inFence := false
	for end < len(lines) {
		line := lines[end]
		if isFenceMarker(line) {
			inFence = !inFence
			end++
			continue
		}
		if !inFence {
			if strings.HasPrefix(line, "## ") {
				break
			}
			if strings.HasPrefix(line, "### ") {
				name := strings.TrimSpace(strings.TrimPrefix(line, "### "))
				if isReservedH3(name) {
					break
				}
				// Non-reserved H3 — body content; keep going.
			}
		}
		end++
	}
	body := lines[start:end]
	// Skip leading blank separator (the canonical layout has
	// exactly one). Multiple leading blanks are unusual; we still
	// only consume one so any extras round-trip.
	if len(body) > 0 && strings.TrimSpace(body[0]) == "" {
		body = body[1:]
	}
	// Strip trailing blank lines (the canonical layout has one).
	for len(body) > 0 && strings.TrimSpace(body[len(body)-1]) == "" {
		body = body[:len(body)-1]
	}
	return strings.Join(body, "\n"), end
}

// isReservedH3 reports whether name is one of the recognized task
// H3 sections. Used by readSection to keep arbitrary `### Subheading`
// content inside section bodies as free-form markdown.
func isReservedH3(name string) bool {
	return name == "Spec" || name == "Acceptance" || name == "Notes"
}

// isFenceMarker reports whether line is a column-0 ```-delimited
// fenced-code-block opener/closer (with or without a language tag).
// Indented fences are intentionally not recognized — operator-authored
// task bodies use indent 0 fences; deeper indents are nested content
// inside the outer fence we already track.
func isFenceMarker(line string) bool {
	if !strings.HasPrefix(line, "```") {
		return false
	}
	// Reject ```` (4+ backticks) — CommonMark allows them but our
	// canonical layout uses 3. Anything starting with exactly ``` plus
	// optional language tag counts.
	rest := line[3:]
	return !strings.HasPrefix(rest, "`")
}

// splitKV splits "key: value" into (key, value, ok). Anything before
// the first ":" is the key; rest is the value (no further trim — value
// trim is value-type-specific).
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

func setKV(t *Task, k, v string, lineNum int, raw string) error {
	switch k {
	case "status":
		t.Status = Status(v)
		if !validStatus(t.Status) {
			return &ParseError{Line: lineNum, Col: 1, Raw: raw, Msg: "invalid status: " + v}
		}
	case "priority":
		t.Priority = Priority(v)
		if !validPriority(t.Priority) {
			return &ParseError{Line: lineNum, Col: 1, Raw: raw, Msg: "invalid priority: " + v}
		}
	case "worker_pid":
		if v == "" || v == "null" {
			t.WorkerPID = 0
			return nil
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			return &ParseError{Line: lineNum, Col: 1, Raw: raw, Msg: "invalid worker_pid: " + v}
		}
		t.WorkerPID = n
	case "worktree":
		t.Worktree = nullToEmpty(v)
	case "pr_url":
		t.PRURL = nullToEmpty(v)
	case "branch":
		t.Branch = nullToEmpty(v)
	case "created":
		ts, err := parseTime(v)
		if err != nil {
			return &ParseError{Line: lineNum, Col: 1, Raw: raw, Msg: "invalid created: " + err.Error()}
		}
		t.Created = ts
	case "updated":
		ts, err := parseTime(v)
		if err != nil {
			return &ParseError{Line: lineNum, Col: 1, Raw: raw, Msg: "invalid updated: " + err.Error()}
		}
		t.Updated = ts
	case "started_at":
		// Lifecycle stamp. Empty / "null" → zero time (never started).
		// Non-required: old rows missing this bullet parse fine.
		ts, err := parseTime(v)
		if err != nil {
			return &ParseError{Line: lineNum, Col: 1, Raw: raw, Msg: "invalid started_at: " + err.Error()}
		}
		t.StartedAt = ts
	case "finished_at":
		// Lifecycle stamp. Empty / "null" → zero time (not finished).
		ts, err := parseTime(v)
		if err != nil {
			return &ParseError{Line: lineNum, Col: 1, Raw: raw, Msg: "invalid finished_at: " + err.Error()}
		}
		t.FinishedAt = ts
	case "dispatch_generation":
		// Coord-owned per-slug fence token (DESIGN §1). Empty / "null" /
		// absent → 0 (legacy). Non-required; old rows parse fine.
		if v == "" || v == "null" {
			t.DispatchGeneration = 0
			return nil
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			return &ParseError{Line: lineNum, Col: 1, Raw: raw, Msg: "invalid dispatch_generation: " + v}
		}
		if n < 0 {
			return &ParseError{Line: lineNum, Col: 1, Raw: raw, Msg: "dispatch_generation must be non-negative: " + v}
		}
		t.DispatchGeneration = n
	case "parked":
		// Durable dirty-worktree park marker (DESIGN §4.2). Empty / "null"
		// → not parked. Non-required; old rows parse fine.
		t.Parked = nullToEmpty(v)
	case "depends_on":
		deps, err := parseDeps(v)
		if err != nil {
			return &ParseError{Line: lineNum, Col: 1, Raw: raw, Msg: err.Error()}
		}
		t.DependsOn = deps
	case "spawned_by":
		t.SpawnedBy = v
	default:
		// Unknown key — refuse rather than silently drop. Forward-
		// compatibility lives in the schema version, not in lax keys.
		return &ParseError{Line: lineNum, Col: 1, Raw: raw, Msg: "unknown task key: " + k}
	}
	return nil
}

func nullToEmpty(v string) string {
	if v == "null" {
		return ""
	}
	return v
}

func parseTime(v string) (time.Time, error) {
	if v == "" || v == "null" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, v)
}

// parseDeps parses a JSON-array literal of slugs: `[]`, `[a]`, `[a, b]`.
// We don't pull in encoding/json — the shape is bounded enough that a
// hand-rolled split is clearer.
func parseDeps(v string) ([]string, error) {
	v = strings.TrimSpace(v)
	if v == "" || v == "[]" {
		return nil, nil
	}
	if !strings.HasPrefix(v, "[") || !strings.HasSuffix(v, "]") {
		return nil, fmt.Errorf("depends_on must be [slug, ...]: %q", v)
	}
	inner := strings.TrimSpace(v[1 : len(v)-1])
	if inner == "" {
		return nil, nil
	}
	parts := strings.Split(inner, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			return nil, fmt.Errorf("depends_on has empty entry")
		}
		// Reject quoted strings — slugs are bare.
		if strings.ContainsAny(p, "\"'") {
			return nil, fmt.Errorf("depends_on entry should be bare slug, got %q", p)
		}
		out = append(out, p)
	}
	return out, nil
}

// ---------- writer ----------

// render emits the canonical bytes for f. Frontmatter is always at
// SchemaVersion. Tasks are emitted in slice order — caller controls
// ordering; we don't sort. Footer (if present) is appended verbatim
// after a blank-line separator so operator-authored trailing markdown
// survives round-trip.
func render(f *File) ([]byte, error) {
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("schema: v")
	b.WriteString(strconv.Itoa(SchemaVersion))
	b.WriteString("\n---\n")
	if len(f.Tasks) > 0 {
		b.WriteByte('\n')
		for i, t := range f.Tasks {
			if err := renderTask(&b, t); err != nil {
				return nil, err
			}
			// Blank line between tasks; trailing blank is intentional
			// to make `git diff` show the previous block unchanged when
			// only the new last task is appended.
			if i < len(f.Tasks)-1 {
				b.WriteByte('\n')
			}
		}
	}
	if f.Footer != "" {
		// Separator between last task (or frontmatter, if no tasks)
		// and the footer's own `## ` header.
		b.WriteByte('\n')
		b.WriteString(f.Footer)
		// Ensure the output ends with a newline (POSIX text-file
		// convention).
		if !strings.HasSuffix(f.Footer, "\n") {
			b.WriteByte('\n')
		}
	}
	return []byte(b.String()), nil
}

func renderTask(b *strings.Builder, t *Task) error {
	if t.Slug == "" {
		return fmt.Errorf("%w: empty slug", ErrInvalidTask)
	}
	if !validStatus(t.Status) {
		return fmt.Errorf("%w: status=%q", ErrInvalidTask, t.Status)
	}
	if !validPriority(t.Priority) {
		return fmt.Errorf("%w: priority=%q", ErrInvalidTask, t.Priority)
	}

	fmt.Fprintf(b, "## task: %s\n\n", t.Slug)
	fmt.Fprintf(b, "- status: %s\n", t.Status)
	fmt.Fprintf(b, "- priority: %s\n", t.Priority)
	if t.WorkerPID == 0 {
		b.WriteString("- worker_pid: 0\n")
	} else {
		fmt.Fprintf(b, "- worker_pid: %d\n", t.WorkerPID)
	}
	writeOptional(b, "worktree", t.Worktree)
	writeOptional(b, "pr_url", t.PRURL)
	writeOptional(b, "branch", t.Branch)
	writeOptional(b, "created", formatTime(t.Created))
	writeOptional(b, "updated", formatTime(t.Updated))
	// Lifecycle timestamps live next to created/updated so the file
	// stays chronologically grouped. Both are ALWAYS emitted (empty
	// string for unset values) so re-saved old rows pick up the new
	// shape — see optionalLifecycleBullets comment.
	writeOptional(b, "started_at", formatTime(t.StartedAt))
	writeOptional(b, "finished_at", formatTime(t.FinishedAt))
	// Coord-lifecycle fields (DESIGN-coord-worktree-lifecycle §1/§4.2).
	// dispatch_generation is ALWAYS emitted (like worker_pid) so re-saved
	// old rows pick up the new shape; parked is optional (like worktree).
	fmt.Fprintf(b, "- dispatch_generation: %d\n", t.DispatchGeneration)
	writeOptional(b, "parked", t.Parked)
	fmt.Fprintf(b, "- depends_on: %s\n", formatDeps(t.DependsOn))
	writeOptional(b, "spawned_by", t.SpawnedBy)
	b.WriteByte('\n')
	writeSection(b, "Spec", t.Spec)
	writeSection(b, "Acceptance", t.Acceptance)
	writeSection(b, "Notes", t.Notes)
	return nil
}

// writeSection emits an `### Name\n\nbody\n\n` block. Empty body
// renders as `### Name\n\n` (the blank after the header IS the body
// separator; nothing else is needed). The trailing `\n\n` is the
// canonical separator between sections; renderTask's caller is
// responsible for the inter-task blank line.
func writeSection(b *strings.Builder, name, body string) {
	fmt.Fprintf(b, "### %s\n\n", name)
	if body == "" {
		return
	}
	b.WriteString(body)
	b.WriteString("\n\n")
}

// writeOptional emits "- key: value" or "- key:" when value is empty.
// Avoids the trailing space after `key:` on empty fields, which the
// fixtures (and our round-trip contract) require.
func writeOptional(b *strings.Builder, key, value string) {
	if value == "" {
		fmt.Fprintf(b, "- %s:\n", key)
		return
	}
	fmt.Fprintf(b, "- %s: %s\n", key, value)
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func formatDeps(deps []string) string {
	if len(deps) == 0 {
		return "[]"
	}
	// Sort defensively for stable output. Slugs are unique.
	cp := append([]string(nil), deps...)
	sort.Strings(cp)
	return "[" + strings.Join(cp, ", ") + "]"
}
