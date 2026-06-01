package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	fleet "github.com/edisonshen/fleet"
	"github.com/edisonshen/fleet/internal/install"
)

// `fleet skills` owns how the bundled skills land in ~/.claude/skills/.
//
// The deploy gap this subcommand closes: `fleet init` copies the embedded
// skill bytes to disk. For an operator developing ON fleet, that copy is a
// frozen snapshot — a merged skill fix (e.g. #182's worktree prompts) never
// reaches the running coord until the binary is rebuilt with the fix AND
// `fleet init --upgrade` is re-run. The fix silently neutered a merged P0.
//
// `fleet skills link` instead symlinks ~/.claude/skills/<name> at the repo
// checkout's skills/<name>/, so the next coord spawn picks up whatever the
// checkout has — merges go live immediately. `fleet skills status` reports
// the install shape and warns when a COPY install has diverged from the
// repo (surface-dont-silo). `fleet skills sync` re-copies the embedded
// bytes for copy-mode installs that need a manual refresh.
//
// Policy (also documented in README / CLAUDE.md):
//   - Developing on fleet → `fleet skills link` (symlink, always live).
//   - Binary-only / brew install → copy (the embedded bytes are the only
//     source); `fleet skills sync` refreshes after a `brew upgrade`.

func newSkillsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skills",
		Short: "Manage how bundled skills (coordinator, fleet-guard) are installed",
		Long: `Inspect and control the install shape of Fleet's bundled skills.

A skill can sit on disk two ways:
  symlink — ~/.claude/skills/<name> points at a repo checkout; edits/merges
            in the checkout go live for the next coord spawn (best for
            developing ON fleet).
  copy    — the embedded skill bytes are written to disk; self-contained but
            a merged fix only lands after rebuilding + re-running sync (best
            for brew / binary-only installs).

  fleet skills status              show each skill's install shape + drift
  fleet skills link [--from REPO]  symlink skills at a repo checkout (live)
  fleet skills sync                re-copy embedded skill bytes (copy mode)`,
	}
	cmd.AddCommand(newSkillsStatusCmd())
	cmd.AddCommand(newSkillsLinkCmd())
	cmd.AddCommand(newSkillsSyncCmd())
	return cmd
}

// resolveClaudeHome returns ~/.claude (or the override, for tests).
func resolveClaudeHome(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home: %w", err)
	}
	return filepath.Join(home, ".claude"), nil
}

func newSkillsStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show install shape (symlink/copy) and flag stale copies",
		Long: `Reports, for each bundled skill, whether it is installed as a
symlink (live), a copy in sync with the repo, or a copy that has DIVERGED
from the repo checkout (a merged fix the running coord can't see). A stale
copy prints a remediation hint and makes the command exit non-zero so
scripts / CI catch the drift.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runSkillsStatus(c.OutOrStdout(), "")
		},
	}
}

// runSkillsStatus prints per-skill install status and returns an error
// (exit non-zero) when any copy has diverged from the repo — so the operator
// and any CI wrapper see the drift instead of it sitting silent.
func runSkillsStatus(stdout io.Writer, claudeHomeOverride string) error {
	claudeHome, err := resolveClaudeHome(claudeHomeOverride)
	if err != nil {
		return err
	}
	locate := func(name string) (string, bool) {
		return install.LocateRepoSkillDirIn(claudeHome, name)
	}
	statuses, err := install.Status(claudeHome, fleet.SkillFS(), locate)
	if err != nil {
		return err
	}
	stale := false
	for _, st := range statuses {
		switch {
		case st.Shape == install.ShapeSymlink:
			_, _ = fmt.Fprintf(stdout, "%-12s symlink -> %s (live)\n", st.Name, st.Target)
		case st.Shape == install.ShapeMissing:
			_, _ = fmt.Fprintf(stdout, "%-12s missing (run `fleet init` or `fleet skills link`)\n", st.Name)
		case st.Stale:
			stale = true
			_, _ = fmt.Fprintf(stdout,
				"%-12s copy — STALE: diverged from %s\n", st.Name, st.RepoSkillDir)
			for _, f := range st.DivergedFiles {
				_, _ = fmt.Fprintf(stdout, "               diverged: %s\n", f)
			}
			_, _ = fmt.Fprintf(stdout,
				"               fix: `fleet skills link` (go live) or `fleet skills sync` (re-copy)\n")
		case st.RepoSkillDir != "":
			_, _ = fmt.Fprintf(stdout, "%-12s copy (in sync with %s)\n", st.Name, st.RepoSkillDir)
		default:
			_, _ = fmt.Fprintf(stdout, "%-12s copy (no repo checkout located to compare)\n", st.Name)
		}
	}
	if stale {
		return fmt.Errorf("one or more skills are a stale copy diverged from the repo; see above")
	}
	return nil
}

func newSkillsLinkCmd() *cobra.Command {
	var from string
	var force bool
	cmd := &cobra.Command{
		Use:   "link",
		Short: "Symlink ~/.claude/skills/<name> at the repo checkout (fixes go live)",
		Long: `Replaces the copied skill install with a symlink to the repo
checkout's skills/<name>/ directory, so merged skill fixes reach the next
coord spawn without a re-copy.

Source resolution: --from <repo> if given; otherwise Fleet scans registered
projects for a checkout containing skills/coordinator/SKILL.md. --force
replaces an existing copy or a symlink pointing elsewhere.

Tradeoff: the linked skill follows the checkout's working tree. If the
checkout sits on a feature branch, the coord runs that branch's skill. Keep
the linked checkout on main for stable behavior.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runSkillsLink(c.OutOrStdout(), from, force, "")
		},
	}
	cmd.Flags().StringVar(&from, "from", "",
		"repo checkout root to link from (default: auto-detect from registered projects)")
	cmd.Flags().BoolVar(&force, "force", false,
		"replace an existing copy or a symlink pointing elsewhere")
	return cmd
}

// runSkillsLink symlinks every bundled skill at the repo checkout. When
// --from is empty, each skill is located independently via the project
// registry; a skill whose source can't be found is skipped with a stderr
// note (surface-dont-silo) rather than failing the whole command — the
// other skills still get linked.
func runSkillsLink(stdout io.Writer, from string, force bool, claudeHomeOverride string) error {
	claudeHome, err := resolveClaudeHome(claudeHomeOverride)
	if err != nil {
		return err
	}
	linked := 0
	for _, name := range sortedSkillNames() {
		repoSkillDir, ok := resolveSkillSource(claudeHome, from, name)
		if !ok {
			_, _ = fmt.Fprintf(stdout,
				"skip %s: no repo checkout located (pass --from <repo>); leaving install as-is\n", name)
			continue
		}
		if err := install.Link(stdout, claudeHome, name, repoSkillDir, force); err != nil {
			return err
		}
		linked++
	}
	if linked == 0 {
		return fmt.Errorf("no skills linked: could not locate a repo checkout (pass --from <repo>)")
	}
	return nil
}

// resolveSkillSource picks the skills/<name> source dir. An explicit --from
// wins (joined with skills/<name>); otherwise auto-detect via the registry,
// then via an already-installed sibling symlink (covers a fleet checkout
// with no meta.json once one skill is already linked).
func resolveSkillSource(claudeHome, from, name string) (string, bool) {
	if from != "" {
		dir := filepath.Join(from, "skills", name)
		if _, err := os.Stat(filepath.Join(dir, "SKILL.md")); err == nil {
			return dir, true
		}
		return "", false
	}
	return install.LocateRepoSkillDirIn(claudeHome, name)
}

func newSkillsSyncCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Re-copy embedded skill bytes (copy-mode installs)",
		Long: `Writes the binary's embedded skill bytes to ~/.claude/skills/<name>/,
refreshing a copy install after a ` + "`brew upgrade`" + `. A symlinked skill is
left intact (it is already live) unless --force converts it back to a copy.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runSkillsSync(c.OutOrStdout(), force, "")
		},
	}
	cmd.Flags().BoolVar(&force, "force", false,
		"overwrite even byte-identical files; converts a symlink back to a copy")
	return cmd
}

// runSkillsSync re-copies the embedded skill bytes. force=false leaves a
// symlink intact (live) and skips byte-identical files; force=true converts
// a symlink back to a copy and rewrites everything.
func runSkillsSync(stdout io.Writer, force bool, claudeHomeOverride string) error {
	claudeHome, err := resolveClaudeHome(claudeHomeOverride)
	if err != nil {
		return err
	}
	skills := fleet.SkillFS()
	for _, name := range sortedSkillNames() {
		if err := install.CopyFromFS(stdout, claudeHome, name, skills[name], force); err != nil {
			return fmt.Errorf("sync %s: %w", name, err)
		}
	}
	_, _ = fmt.Fprintln(stdout, "fleet skills sync: done")
	return nil
}

// sortedSkillNames returns the bundled skill names in deterministic order so
// command output is stable across runs.
func sortedSkillNames() []string {
	return skillInstallOrder(fleet.SkillFS())
}
