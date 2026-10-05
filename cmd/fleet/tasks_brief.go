package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/edisonshen/fleet/internal/planreview"
	"github.com/edisonshen/fleet/internal/projects"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tasks"
)

type tasksBriefOpts struct {
	project string
	plan    bool
}

func newTasksBriefCmd() *cobra.Command {
	opts := &tasksBriefOpts{}
	cmd := &cobra.Command{
		Use:   "brief <slug>",
		Short: "Scaffold a task's DISCOVER brief (and, with --plan, its TASK-PLAN doc)",
		Long: `brief starts the DISCOVER phase: it writes docs/BRIEF-<slug>.md in the
project repo with Relevant files, Current behavior, Options, Open questions
and Decision sections. A read-only research subagent fills the first three;
the operator answers the questions and records the Decision.

With --plan it also writes docs/TASK-PLAN-<slug>.md (Goal, Files, Steps,
Verification, Open questions) linked to the brief, and links the plan from
the task spec. ` + "`fleet tasks approve`" + ` refuses a plan with a missing
section, open questions, or an undecided brief. Existing files are kept.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTasksBrief(opts, args[0], cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&opts.project, "project", "", "project name (default: cwd basename)")
	cmd.Flags().BoolVar(&opts.plan, "plan", false, "also scaffold docs/TASK-PLAN-<slug>.md and link it from the task spec")
	return cmd
}

func runTasksBrief(opts *tasksBriefOpts, slug string, stdout io.Writer) error {
	if _, err := state.Bootstrap(); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	project, err := resolveProject(opts.project)
	if err != nil {
		return err
	}
	f, path, err := readTasks(project)
	if err != nil {
		return err
	}
	t, err := f.Get(slug)
	if err != nil {
		return err
	}
	repo := ""
	if m, err := projects.Read(project); err == nil {
		repo = m.RepoPath
	}
	if repo == "" {
		if repo, err = os.Getwd(); err != nil {
			return err
		}
	}
	briefRel := filepath.ToSlash(filepath.Join("docs", "BRIEF-"+slug+".md"))
	brief := filepath.Join(repo, briefRel)
	if err := writeIfAbsent(brief, planreview.BriefTemplate(slug), stdout); err != nil {
		return err
	}
	if !opts.plan {
		_, _ = fmt.Fprintf(stdout, "next: dispatch a read-only research subagent to fill %s, then record the operator's Decision\n", brief)
		return nil
	}
	planRel := filepath.ToSlash(filepath.Join("docs", "TASK-PLAN-"+slug+".md"))
	if err := writeIfAbsent(filepath.Join(repo, planRel), planreview.PlanTemplate(slug, briefRel), stdout); err != nil {
		return err
	}
	if planreview.LinkedDoc(t) == "" {
		t.Spec += "\n\nTask plan: `" + planRel + "`"
		if err := tasks.Write(path, f); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "linked %s from %s spec\n", planRel, slug)
	}
	return nil
}

func writeIfAbsent(path, body string, stdout io.Writer) error {
	if _, err := os.Stat(path); err == nil {
		_, _ = fmt.Fprintf(stdout, "kept %s (exists)\n", path)
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "wrote %s\n", path)
	return nil
}
