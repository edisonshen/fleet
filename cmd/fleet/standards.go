package main

// fleet standards {show,edit} — operator-facing CLI for the global +
// per-project standards.md files. show renders one or both files (or
// the merged result) to stdout; edit opens $EDITOR on the right path,
// seeding a v1-frontmatter stub when the file is missing.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/edisonshen/fleet/internal/standards"
	"github.com/edisonshen/fleet/internal/state"
)

func newStandardsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "standards",
		Short: "Manage global + per-project standards.md (the bar)",
		Long: `standards owns the global ` + "`~/.fleet/standards.md`" + ` and the
optional per-project ` + "`~/.fleet/projects/<project>/standards.md`" + `.
Workers receive the merged result inlined in their prompts; the merge
is section-level per H2 (project wins on conflict).

show prints the rendered standards (defaults to merged). edit opens
$EDITOR on the chosen file, creating a v1-frontmatter stub if missing.`,
	}
	cmd.AddCommand(
		newStandardsShowCmd(),
		newStandardsEditCmd(),
	)
	return cmd
}

// ---------- fleet standards show ----------

type standardsShowOpts struct {
	project string
	scope   standardsScope
	global  bool
	proj    bool
	merged  bool
}

type standardsScope int

const (
	scopeMerged standardsScope = iota
	scopeGlobal
	scopeProject
)

func newStandardsShowCmd() *cobra.Command {
	opts := &standardsShowOpts{}
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Render standards (default: merged global + per-project)",
		Long: `show renders the standards file as it would appear to a worker.

Default is --merged: per-H2 merge of global + per-project, project
winning on same-named sections, project-only sections appended.

--global shows only the global file; --project shows only the
per-project file.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Decide scope from flag combinations. Mutually exclusive:
			// --global, --project, --merged. Default is merged.
			set := 0
			if opts.global {
				set++
				opts.scope = scopeGlobal
			}
			if opts.proj {
				set++
				opts.scope = scopeProject
			}
			if opts.merged {
				set++
				opts.scope = scopeMerged
			}
			if set > 1 {
				return fmt.Errorf("standards show: --global / --project / --merged are mutually exclusive")
			}
			return runStandardsShow(opts, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&opts.project, "project", "", "project name (default: cwd basename)")
	cmd.Flags().BoolVar(&opts.global, "global", false, "show only global standards")
	cmd.Flags().BoolVar(&opts.proj, "project-only", false, "show only per-project standards")
	cmd.Flags().BoolVar(&opts.merged, "merged", false, "show merged result (default behavior)")
	return cmd
}

func runStandardsShow(opts *standardsShowOpts, stdout io.Writer) error {
	switch opts.scope {
	case scopeGlobal:
		s, err := standards.LoadGlobal()
		if err != nil {
			return fmt.Errorf("load global: %w", err)
		}
		_, err = io.WriteString(stdout, s.Render())
		return err
	case scopeProject:
		project, err := resolveProject(opts.project)
		if err != nil {
			return err
		}
		s, err := standards.LoadProject(project)
		if err != nil {
			return fmt.Errorf("load project: %w", err)
		}
		_, err = io.WriteString(stdout, s.Render())
		return err
	case scopeMerged:
		project, err := resolveProject(opts.project)
		if err != nil {
			return err
		}
		g, err := standards.LoadGlobal()
		if err != nil {
			return fmt.Errorf("load global: %w", err)
		}
		p, err := standards.LoadProject(project)
		if err != nil {
			return fmt.Errorf("load project: %w", err)
		}
		_, err = io.WriteString(stdout, standards.Merge(g, p).Render())
		return err
	default:
		return fmt.Errorf("standards show: unknown scope %d", opts.scope)
	}
}

// ---------- fleet standards edit ----------

type standardsEditOpts struct {
	project string
	global  bool
	proj    bool
	editor  string // override $EDITOR for tests
}

func newStandardsEditCmd() *cobra.Command {
	opts := &standardsEditOpts{}
	cmd := &cobra.Command{
		Use:   "edit",
		Short: "Open standards.md in $EDITOR (creates a stub if missing)",
		Long: `edit opens the chosen standards.md in $EDITOR. Default is the
per-project file (` + "`~/.fleet/projects/<project>/standards.md`" + `);
pass --global to edit the global file at ` + "`~/.fleet/standards.md`" + `.

If the target file is missing, edit seeds a v1-frontmatter stub before
launching the editor so the operator gets a parseable starting point.

Honors $EDITOR; falls back to ` + "`vi`" + ` if unset. Refuses if both
$EDITOR is unset AND vi isn't on PATH (rare, but better than silently
opening nothing).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if opts.global && opts.proj {
				return fmt.Errorf("standards edit: --global and --project are mutually exclusive")
			}
			return runStandardsEdit(opts, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().StringVar(&opts.project, "project", "", "project name (default: cwd basename)")
	cmd.Flags().BoolVar(&opts.global, "global", false, "edit ~/.fleet/standards.md")
	cmd.Flags().BoolVar(&opts.proj, "project-only", false, "edit ~/.fleet/projects/<project>/standards.md (default)")
	return cmd
}

// runStandardsEdit resolves the target path, seeds a stub if missing,
// then launches $EDITOR. Stdin/stdout/stderr are wired through so the
// editor can run interactively even when the CLI's stdout was captured.
func runStandardsEdit(opts *standardsEditOpts, stdout, stderr io.Writer) error {
	if _, err := state.Bootstrap(); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	path, err := standardsPathFor(opts)
	if err != nil {
		return err
	}
	if err := ensureStandardsStub(path); err != nil {
		return fmt.Errorf("seed stub: %w", err)
	}
	editor := opts.editor
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = "vi"
	}
	if _, err := exec.LookPath(editor); err != nil {
		return fmt.Errorf("editor %q not on PATH: %w", editor, err)
	}
	c := exec.Command(editor, path)
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("editor exited non-zero: %w", err)
	}
	_, _ = fmt.Fprintf(stdout, "edited %s\n", path)
	return nil
}

// standardsPathFor resolves the target file for `standards edit`
// flags. Default (neither --global nor --project-only) is the per-
// project file — that's the file an operator scoping a single repo
// is most likely to want.
func standardsPathFor(opts *standardsEditOpts) (string, error) {
	if opts.global {
		root, err := state.Root()
		if err != nil {
			return "", err
		}
		return filepath.Join(root, "standards.md"), nil
	}
	project, err := resolveProject(opts.project)
	if err != nil {
		return "", err
	}
	dir, err := state.ProjectDir(project)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "standards.md"), nil
}

// ensureStandardsStub creates path with a v1-frontmatter stub if it
// doesn't exist. Goes through state.WriteAtomic so a partial seed
// can't leave a torn file the parser then rejects.
//
// The stub matches what `standards.Render` produces for an empty
// Standards{} — frontmatter + the canonical "# Standards" H1 — so
// `fleet standards show` immediately after `edit` produces stable
// output even before the operator adds any sections.
func ensureStandardsStub(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir parent: %w", err)
	}
	stub := (&standards.Standards{}).Render()
	return state.WriteAtomic(path, []byte(stub))
}
