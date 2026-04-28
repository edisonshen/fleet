package main

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tmux"
)

// dispatchOpts captures cobra-parsed flags so the run() func is testable
// without poking at globals.
type dispatchOpts struct {
	taskID  string
	project string
	cwd     string
	command []string
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
	cmd.Flags().StringSliceVar(&opts.command, "command", []string{"claude"},
		"command to run inside the tmux session (default: claude)")
	return cmd
}

func runDispatch(opts *dispatchOpts, stdout io.Writer) error {
	if _, err := state.Bootstrap(); err != nil {
		return fmt.Errorf("bootstrap ~/.fleet: %w", err)
	}
	if err := tmux.Available(); err != nil {
		return err
	}

	id := agent.NewID()
	session := tmux.SessionName(id)

	if err := tmux.Spawn(session, opts.cwd, opts.command); err != nil {
		return err
	}

	rec := agent.New(id)
	rec.TmuxSession = session
	rec.TaskID = opts.taskID
	rec.Project = opts.project
	rec.PID = os.Getpid() // best-effort — tmux pane PID is harder to capture pre-spawn

	if err := rec.Write(); err != nil {
		// Spawn succeeded but record write failed — kill the orphan
		// session so we don't leak. Operator can re-run dispatch.
		_ = tmux.Kill(session)
		return fmt.Errorf("write agent record: %w (orphan tmux session killed)", err)
	}

	_, _ = fmt.Fprintf(stdout, "agent %s spawned\n", id)
	_, _ = fmt.Fprintf(stdout, "  task:    %s\n", opts.taskID)
	_, _ = fmt.Fprintf(stdout, "  project: %s\n", opts.project)
	_, _ = fmt.Fprintf(stdout, "  tmux:    %s\n", session)
	_, _ = fmt.Fprintf(stdout, "\nattach with: fleet attach %s\n", id)
	return nil
}
