package main

// `fleet project add <path>` — register a cloned repo as a fleet
// project without dispatching a task or spawning a coord. The
// dashboard surfaces it on the next refresh in alpha order.
//
// v1 verb set: only `add`. Future verbs (rm, list, rename) are
// deferred — file separately if/when a use case appears.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/edisonshen/fleet/internal/projects"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tasks"
)

// newProjectCmd wires `fleet project ...`. The TUI's [+] hotkey shells
// out to `fleet project add <path>` after the operator picks a row in
// the repo picker — keeping the disk-mutation logic in one place
// (tested via this CLI) means the TUI is just dispatch + flash on
// success/failure.
func newProjectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "project",
		Short: "Manage Fleet's per-project state",
		Long: `project owns ~/.fleet/projects/<tag>/ — the per-project state tree
that holds tasks.md, learnings.md, workers/, worktrees/, and the
.locks/ subdir. v1 ships ` + "`add`" + ` only.`,
	}
	cmd.AddCommand(newProjectAddCmd())
	return cmd
}

// newProjectAddCmd is `fleet project add <path>`. Idempotent: re-adding
// the same path refreshes added_at; re-adding a different path that
// sanitizes to the same tag refreshes repo_path and warns on stderr.
func newProjectAddCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add <path>",
		Short: "Register a cloned repo as a fleet project (no dispatch, no coord)",
		Long: `add registers <path> as a fleet project without dispatching a task
or auto-spawning a coordinator. The path must exist and contain a .git
entry (file or directory — covers worktrees).

Creates ~/.fleet/projects/<tag>/{tasks.md, meta.json}. The tag is
derived via the standard parent-basename + sanitization rule (same as
` + "`fleet dispatch`" + `'s --project default).

Idempotent on the same path (refreshes added_at). Re-adding a
different path that sanitizes to the same tag refreshes repo_path and
prints a one-line warning on stderr — the operator-supplied path wins,
but you'll know it happened.`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return runProjectAdd(args[0], c.OutOrStdout(), c.ErrOrStderr())
		},
	}
	return cmd
}

// runProjectAdd is the core of `fleet project add`. Validates the
// path, derives the tag, bootstraps tasks.md (frontmatter only) and
// writes meta.json atomically. Returns errors with a clear prefix so
// the TUI's flash can surface them verbatim.
//
// Flow:
//
//  1. Resolve path to absolute.
//  2. Stat: must exist and be a directory.
//  3. Stat <abs>/.git: must exist (file or directory; worktree pointer
//     files count).
//  4. tag = projects.TagForPath(abs).
//  5. state.Bootstrap (idempotent — creates ~/.fleet/ subdirs if missing).
//  6. EnsureProjectInitialized creates the project dir + .locks/.
//  7. Lazy-create tasks.md with frontmatter if missing (existing files
//     are left alone — preserves operator state on re-add).
//  8. Read existing meta.json (if any) for collision detection.
//  9. Write meta.json atomically (refreshes added_at + repo_path).
//  10. Stdout: "added project <tag>" or "refreshed project <tag>".
//     Stderr: warning if tag collided with a different repo.
func runProjectAdd(rawPath string, stdout, stderr io.Writer) error {
	if rawPath == "" {
		return errors.New("project add: <path> must not be empty")
	}
	// (1) absolute path. filepath.Abs uses the process cwd as the
	// anchor for relative inputs — matches the operator's mental
	// model when they `cd ~/work && fleet project add my-repo`.
	abs, err := filepath.Abs(rawPath)
	if err != nil {
		return fmt.Errorf("project add: resolve %q: %w", rawPath, err)
	}
	abs = filepath.Clean(abs)

	// (2) directory must exist.
	info, err := os.Stat(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("project add: no such directory: %s", abs)
		}
		return fmt.Errorf("project add: stat %s: %w", abs, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("project add: not a directory: %s", abs)
	}

	// (3) .git entry — file OR directory. A real clone has a .git
	// directory; a `git worktree add`'d checkout has a .git file with
	// a "gitdir: ..." pointer. Both are valid project roots.
	if _, gerr := os.Stat(filepath.Join(abs, ".git")); gerr != nil {
		if errors.Is(gerr, os.ErrNotExist) {
			return fmt.Errorf("project add: not a git repo: %s (no .git entry)", abs)
		}
		return fmt.Errorf("project add: stat %s/.git: %w", abs, gerr)
	}

	// (4) tag.
	tag := projects.TagForPath(abs)
	if err := state.ValidateProjectName(tag); err != nil {
		// projects.TagForPath is supposed to produce a name that
		// passes ValidateProjectName ("fleet" fallback included). If
		// validation still fails, the input was so degenerate that we
		// can't safely register it — surface the underlying error.
		return fmt.Errorf("project add: derived tag %q invalid: %w", tag, err)
	}

	// (5) ~/.fleet bootstrap.
	if _, err := state.Bootstrap(); err != nil {
		return fmt.Errorf("project add: bootstrap ~/.fleet: %w", err)
	}

	// (6) per-project tree + .locks/.
	if _, err := state.EnsureProjectInitialized(tag); err != nil {
		return fmt.Errorf("project add: init project tree: %w", err)
	}

	// (7) lazy-create tasks.md with frontmatter. Preserves any existing
	// content on re-add — operator's task list must NOT be wiped by a
	// repeat `fleet project add`.
	dir, err := state.ProjectDir(tag)
	if err != nil {
		return fmt.Errorf("project add: resolve project dir: %w", err)
	}
	tasksPath := filepath.Join(filepath.Clean(dir), "tasks.md")
	if _, terr := os.Stat(tasksPath); errors.Is(terr, os.ErrNotExist) {
		// Empty File — internal/tasks.Write emits the schema header
		// regardless of input, so a zero-Tasks file lands on disk
		// as "---\nschema: v1\n---\n".
		if err := tasks.Write(tasksPath, &tasks.File{Schema: tasks.SchemaVersion}); err != nil {
			return fmt.Errorf("project add: bootstrap tasks.md: %w", err)
		}
	} else if terr != nil {
		return fmt.Errorf("project add: stat tasks.md: %w", terr)
	}

	// (8) collision detection. Read existing meta.json if any so the
	// re-add path can warn on a different repo_path landing on the
	// same tag (parent-basename sanitization is lossy — see
	// internal/tui/discover.go's Known limitation note).
	prev, prevErr := projects.Read(tag)
	hadPrev := prevErr == nil
	collision := hadPrev && prev.RepoPath != "" && prev.RepoPath != abs
	if !hadPrev && prevErr != nil && !errors.Is(prevErr, projects.ErrNotFound) {
		// Real read error — surface it. Fresh-add is fine on
		// ErrNotFound but a malformed meta.json is a hand-edit
		// problem the operator should see.
		return fmt.Errorf("project add: read existing meta.json: %w", prevErr)
	}

	// (9) write meta.json. Use UTC so timestamps are stable across
	// machines / timezones (matches state.HandoffPath's UTC choice).
	now := nowFn().UTC()
	m := projects.Meta{
		Schema:   projects.SchemaVersion,
		RepoPath: abs,
		AddedAt:  now,
	}
	if err := projects.Write(tag, m); err != nil {
		return fmt.Errorf("project add: write meta.json: %w", err)
	}

	// (10) operator-facing output. The "refreshed" verb on re-add
	// is operator-friendlier than a generic "added" — they pressed
	// the same path twice on purpose, and the message confirms it.
	switch {
	case collision:
		_, _ = fmt.Fprintf(stderr,
			"warning: project tag %q already pointed at %s — refreshed repo_path to %s\n",
			tag, prev.RepoPath, abs)
		_, _ = fmt.Fprintf(stdout, "refreshed project %s (%s)\n", tag, abs)
	case hadPrev:
		_, _ = fmt.Fprintf(stdout, "refreshed project %s (%s)\n", tag, abs)
	default:
		_, _ = fmt.Fprintf(stdout, "added project %s (%s)\n", tag, abs)
	}
	return nil
}

// nowFn lets tests stub the timestamp without time.Sleep. Production
// resolves to time.Now.
var nowFn = time.Now
