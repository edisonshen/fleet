package main

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/edisonshen/fleet/internal/spawn"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tmux"
)

// dispatchOpts captures cobra-parsed flags so the run() func is testable
// without poking at globals.
type dispatchOpts struct {
	taskID       string
	project      string
	cwd          string
	command      []string
	noAutoResume bool
	// prompt is the optional first-turn prompt to type into the
	// freshly-spawned tmux session AFTER the pane stabilizes. Empty
	// (default) → no prompt; the operator types one manually after
	// `fleet attach`. Non-empty → spawn calls
	// spawn.SendInitialPrompt(session, prompt) so the agent boots with
	// the prompt already executed. Used by the v0.2 dashboard's
	// project-row [a] auto-spawn path (issue #60) where the coord agent
	// is bootstrapped non-interactively with `Run the /coordinator skill
	// loop for project <name>.`
	prompt string
}

func newDispatchCmd() *cobra.Command {
	opts := &dispatchOpts{}
	cmd := &cobra.Command{
		Use:   "dispatch <task-id>",
		Short: "Spawn a Claude Code agent in a detached tmux session",
		Long: `dispatch creates a new agent identified by a short hex ID,
spawns a Claude Code session in a detached tmux session named
"fleet-<agent-id>", and writes a stub agent record to
~/.fleet/agents/<agent-id>.json.

Pre-v1: the project and task model is minimal — pass --project to tag
the record. A full project manifest model lands later (see docs/DESIGN.md
"projects/<name>.yaml").`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.taskID = args[0]
			return runDispatch(opts, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&opts.project, "project", "default",
		"project name to tag this agent with")
	cmd.Flags().StringVar(&opts.cwd, "cwd", "",
		"working directory for the spawned session (default: current dir)")
	// Default wraps claude in a shell so the tmux session SURVIVES
	// claude exiting CLEANLY (Ctrl-D / /exit). Without the wrapper,
	// any claude exit kills the session and `fleet attach` later
	// fails with "no sessions". The wrapper drops into an interactive
	// shell only when claude returned 0 — non-zero exits (binary
	// missing, unsupported flag, crash) propagate so the session
	// terminates, mirroring the no-wrapper failure-detection
	// behavior: tmux.Spawn / fleet status see no live session and
	// the operator gets a hard signal that something went wrong
	// instead of a zombie agent record on top of an idle shell.
	//
	// `--dangerously-skip-permissions` is on by default because
	// Fleet's premise is fire-and-forget parallel agents: every
	// permission prompt blocks one of N agents and forces the
	// operator to babysit it. Override with `--command` for scripted
	// pipelines or alternate engines.
	cmd.Flags().StringSliceVar(&opts.command, "command",
		[]string{"sh", "-c", `claude --dangerously-skip-permissions; RC=$?; if [ "$RC" -ne 0 ]; then echo; echo "[fleet] claude exited code $RC — session terminating"; exit "$RC"; fi; echo; echo "[fleet] claude exited cleanly — rerun claude --dangerously-skip-permissions or Ctrl-b then & to kill this session"; exec ${SHELL:-bash} -i`},
		"command to run inside the tmux session (default: shell-wrapped claude --dangerously-skip-permissions)")
	// Auto-resume types "Read your handoff doc at <path> and continue"
	// into the replacement on handoff. Disable for custom --command
	// argvs running shells / REPLs / non-claude engines where the
	// natural-language prompt would execute as garbage input
	// (codex review iter-7 P2). Inherited across handoffs.
	cmd.Flags().BoolVar(&opts.noAutoResume, "no-auto-resume", false,
		"skip auto-typing the resume prompt on handoff (use for non-claude --command argvs)")
	// --prompt types `<text>` into the freshly-spawned session after the
	// pane stabilizes. v0.2 use case: the TUI's project-row [a] auto-
	// spawn path bootstraps a coord with `Run the /coordinator skill
	// loop for project <name>.` so the agent is productive without the
	// operator having to attach + type. Empty → no prompt typed (the
	// classic interactive dispatch flow).
	cmd.Flags().StringVar(&opts.prompt, "prompt", "",
		"first-turn prompt to type into the spawned session (default: none)")
	return cmd
}

func runDispatch(opts *dispatchOpts, stdout io.Writer) error {
	maybeAutoInit(stdout, "")
	if _, err := state.Bootstrap(); err != nil {
		return fmt.Errorf("bootstrap ~/.fleet: %w", err)
	}
	if err := tmux.Available(); err != nil {
		return err
	}
	// Reject project names with path separators / "..": they'd
	// silently misbehave at handoff time when they're used as a lock
	// file name. Better to fail at dispatch.
	if err := state.ValidateProjectName(opts.project); err != nil {
		return fmt.Errorf("--project: %w", err)
	}

	rec, err := spawn.Spawn(spawn.Options{
		TaskID:            opts.taskID,
		Project:           opts.project,
		Cwd:               opts.cwd,
		Command:           opts.command,
		DisableAutoResume: opts.noAutoResume,
	})
	if err != nil {
		return err
	}

	_, _ = fmt.Fprintf(stdout, "agent %s spawned\n", rec.ID)
	_, _ = fmt.Fprintf(stdout, "  task:    %s\n", rec.TaskID)
	_, _ = fmt.Fprintf(stdout, "  project: %s\n", rec.Project)
	_, _ = fmt.Fprintf(stdout, "  tmux:    %s\n", rec.TmuxSession)
	if opts.prompt != "" {
		// Best-effort: a SendInitialPrompt failure here logs a warning
		// to stderr but does NOT fail the dispatch — the session is up,
		// the operator can attach + type the prompt manually. The
		// alternative (returning err) would orphan the agent record +
		// tmux session, requiring a manual cleanup. The TUI's coord
		// auto-spawn path (issue #60) reads dispatch's exit code to
		// decide whether the spawn succeeded; treating prompt-delivery
		// failure as fatal would mistakenly mark a running coord as
		// failed.
		if perr := sendInitialPrompt(rec.TmuxSession, opts.prompt); perr != nil {
			_, _ = fmt.Fprintf(stdout,
				"warning: initial prompt not delivered (%v) — attach to type it manually\n",
				perr)
		} else {
			_, _ = fmt.Fprintf(stdout, "  prompt:  delivered\n")
		}
	}
	_, _ = fmt.Fprintf(stdout, "\nattach with: fleet attach %s\n", rec.ID)
	return nil
}

// sendInitialPrompt is a var so tests can stub the tmux interaction.
// Production calls spawn.SendInitialPrompt which polls the pane until
// the wrapped claude is idle, then sends the prompt + Enter.
var sendInitialPrompt = func(session, prompt string) error {
	return spawn.SendInitialPrompt(session, prompt)
}
