package main

// fleet learnings {add,list,prune} — operator-facing CLI for the
// per-project learnings.md log. Append-only experience tracked by the
// internal/learnings primitive (ENG §3.2). Workers shell out to
// `fleet learnings add` so their writes go through the same state-lock
// mutations as operator-driven appends.

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/edisonshen/fleet/internal/learnings"
	"github.com/edisonshen/fleet/internal/state"
)

func newLearningsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "learnings",
		Short: "Manage the per-project learnings.md log",
		Long: `learnings owns the per-project shared experience log at
~/.fleet/projects/<project>/learnings.md. Workers append entries
auto-magically (via this CLI from inside their session); operators
prune via --before=<duration> when the file gets noisy.

Default --project is the current directory's basename. All mutations
serialize through the per-project state-lock.`,
	}
	cmd.AddCommand(
		newLearningsAddCmd(),
		newLearningsListCmd(),
		newLearningsPruneCmd(),
	)
	return cmd
}

// ---------- fleet learnings add ----------

type learningsAddOpts struct {
	project  string
	author   string
	taskSlug string
	tags     []string
}

func newLearningsAddCmd() *cobra.Command {
	opts := &learningsAddOpts{}
	cmd := &cobra.Command{
		Use:   "add <text>",
		Short: "Append a learning entry",
		Long: `add appends a new entry to learnings.md. Required fields are
the body text (positional) and a tag (--tag, repeatable; first tag is
the canonical one stored in the H2 header). Author defaults to
"operator"; workers should pass --author=agent:<8-hex> + --task=<slug>.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLearningsAdd(opts, args[0], cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&opts.project, "project", "", "project name (default: cwd basename)")
	cmd.Flags().StringVar(&opts.author, "author", "operator", "entry author: operator | agent:<8hex>")
	cmd.Flags().StringVar(&opts.taskSlug, "task", "", "associated task slug (omit for operator-authored notes)")
	cmd.Flags().StringSliceVar(&opts.tags, "tag", nil, "topic tag (first tag stored in header; pass multiple times to attach more)")
	return cmd
}

func runLearningsAdd(opts *learningsAddOpts, body string, stdout io.Writer) error {
	if _, err := state.Bootstrap(); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	project, err := resolveProject(opts.project)
	if err != nil {
		return err
	}
	tags := nonEmptyStrings(opts.tags)
	if len(tags) == 0 {
		return fmt.Errorf("learnings add: at least one --tag required")
	}
	if strings.TrimSpace(body) == "" {
		return fmt.Errorf("learnings add: body is empty")
	}
	// Storage shape carries one tag in the header; extras are merged
	// into the body as a markdown line so they're searchable but don't
	// require a schema bump. Keeps the CLI ergonomics ("pass as many
	// --tag as you want") while leaving internal/learnings unchanged.
	primary := tags[0]
	extras := tags[1:]
	if len(extras) > 0 {
		body = body + "\n\nadditional tags: " + strings.Join(extras, ", ")
	}
	entry := &learnings.Entry{
		Author:   opts.author,
		TaskSlug: opts.taskSlug,
		Tag:      primary,
		Body:     body,
		// Timestamp left zero — Append stamps it with time.Now().UTC()
		// so two near-simultaneous workers can't pick the same second
		// in a way the operator passed in.
	}
	if err := learnings.Append(project, entry); err != nil {
		return fmt.Errorf("append: %w", err)
	}
	_, _ = fmt.Fprintf(stdout, "appended learning (author=%s tag=%s task=%s)\n",
		opts.author, primary, defaultStr(opts.taskSlug, "-"))
	return nil
}

// ---------- fleet learnings list ----------

type learningsListOpts struct {
	project string
	limit   int
	tag     string
	task    string
}

func newLearningsListCmd() *cobra.Command {
	opts := &learningsListOpts{}
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List recent learnings entries",
		Long: `list prints recent learnings, newest first. Default cap is 20;
pass --limit=N to widen. --tag/--task narrow by substring (tag) or
exact match (task slug).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runLearningsList(opts, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&opts.project, "project", "", "project name (default: cwd basename)")
	cmd.Flags().IntVar(&opts.limit, "limit", 20, "max entries to show (0 or negative = no cap)")
	cmd.Flags().StringVar(&opts.tag, "tag", "", "filter by tag substring")
	cmd.Flags().StringVar(&opts.task, "task", "", "filter by exact task slug")
	return cmd
}

func runLearningsList(opts *learningsListOpts, stdout io.Writer) error {
	project, err := resolveProject(opts.project)
	if err != nil {
		return err
	}
	entries, err := learnings.Filter(project, opts.tag, opts.task, opts.limit)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		_, _ = fmt.Fprintln(stdout, "no learnings (run `fleet learnings add` to record one)")
		return nil
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "WHEN\tAUTHOR\tTAG\tTASK\tBODY")
	for _, e := range entries {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			e.Timestamp.UTC().Format(time.RFC3339),
			e.Author,
			e.Tag,
			defaultStr(e.TaskSlug, "-"),
			summarizeBody(e.Body),
		)
	}
	return tw.Flush()
}

// summarizeBody collapses the entry body into a one-line preview so
// the table stays readable. Operators who want the full text drop the
// limit and grep, or read learnings.md directly.
func summarizeBody(body string) string {
	body = strings.TrimSpace(body)
	if i := strings.Index(body, "\n"); i >= 0 {
		body = body[:i]
	}
	const max = 80
	if len(body) > max {
		body = body[:max-1] + "…"
	}
	return body
}

// ---------- fleet learnings prune ----------

type learningsPruneOpts struct {
	project string
	before  string
}

func newLearningsPruneCmd() *cobra.Command {
	opts := &learningsPruneOpts{}
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Move old entries to learnings-archive.md",
		Long: `prune moves entries older than --before=<duration> from
learnings.md to learnings-archive.md. Duration accepts Go's standard
syntax plus the operator-friendly "Nd" form (Ndays). Examples:
  fleet learnings prune --before 7d
  fleet learnings prune --before 720h

Lock + atomic publish are handled by internal/learnings.Prune.
Pruning a partially-archived state is safe — entries are deduped
before append (codex review's anti-duplicate guarantee).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runLearningsPrune(opts, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&opts.project, "project", "", "project name (default: cwd basename)")
	cmd.Flags().StringVar(&opts.before, "before", "", "prune entries older than this duration (required; e.g. 7d, 30d, 720h)")
	return cmd
}

func runLearningsPrune(opts *learningsPruneOpts, stdout io.Writer) error {
	if _, err := state.Bootstrap(); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	if strings.TrimSpace(opts.before) == "" {
		return fmt.Errorf("learnings prune: --before is required (e.g. --before 7d)")
	}
	project, err := resolveProject(opts.project)
	if err != nil {
		return err
	}
	dur, err := parseDurationLoose(opts.before)
	if err != nil {
		return fmt.Errorf("--before %q: %w", opts.before, err)
	}
	if dur <= 0 {
		return fmt.Errorf("--before must be positive, got %s", dur)
	}
	cutoff := time.Now().UTC().Add(-dur)
	if err := learnings.Prune(project, cutoff); err != nil {
		return fmt.Errorf("prune: %w", err)
	}
	_, _ = fmt.Fprintf(stdout, "pruned entries older than %s (cutoff=%s)\n",
		dur, cutoff.Format(time.RFC3339))
	return nil
}

// parseDurationLoose extends time.ParseDuration with "Nd" / "Nw" so
// `--before 7d` works without forcing operators to compute hours.
// Falls back to time.ParseDuration on shapes the loose parser doesn't
// recognize.
func parseDurationLoose(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	// Single-token shorthand: "<int><unit>" where unit ∈ {d, w}. Any
	// other shape (h, m, s, mixed, etc.) falls through to stdlib.
	if len(s) >= 2 {
		last := s[len(s)-1]
		body := s[:len(s)-1]
		var per time.Duration
		switch last {
		case 'd':
			per = 24 * time.Hour
		case 'w':
			per = 7 * 24 * time.Hour
		default:
			per = 0
		}
		if per > 0 {
			n := 0
			if _, err := fmt.Sscanf(body, "%d", &n); err == nil && fmt.Sprintf("%d", n) == body {
				return time.Duration(n) * per, nil
			}
		}
	}
	return time.ParseDuration(s)
}
