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
	// coordSpawn is the internal flag that whitelists the reserved
	// "coord-<project>" task_id prefix. The dashboard's task_id
	// fallback signal (issue #63) treats agents tagged with this
	// prefix as the project's coord — without this gate, an operator
	// could shell out `fleet dispatch coord-foo --project foo` and
	// hijack the dashboard's coord-on-LEFT slot for a worker session.
	// Set only by the TUI's startCoordSpawn shell-out.
	coordSpawn bool
}

// CoordTaskIDPrefix is the reserved task_id prefix used to mark a
// dispatch as a project's coordinator. The TUI's project-row [a]
// auto-spawn flow writes "coord-<project>" via the --coord-spawn
// CLI flag; the dashboard reads the prefix as a fallback identity
// signal during the 10-30s skill-boot window before the lock body
// publishes (issue #63). Operator-supplied dispatches with this
// prefix are rejected by runDispatch unless --coord-spawn is set.
const CoordTaskIDPrefix = "coord-"

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
	// --coord-spawn is the internal escape hatch for the TUI's project-
	// row [a] auto-spawn flow. The "coord-<project>" task_id prefix is
	// a sentinel the dashboard reads to identify the project's coord
	// (issue #63 task_id-fallback signal). Without this gate, an
	// operator-supplied `fleet dispatch coord-foo --project foo` would
	// create a worker that the dashboard treats as the coord —
	// hijacking [a] / [x] / coord-on-LEFT for a worker session. Hidden
	// from --help so accidental use isn't encouraged; the TUI sets it
	// when dispatching its own coord agents.
	cmd.Flags().BoolVar(&opts.coordSpawn, "coord-spawn", false,
		"internal: allow reserved coord-<project> task_id prefix (used by the TUI)")
	_ = cmd.Flags().MarkHidden("coord-spawn")
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
	// Reserve the EXACT "coord-<project>" task_id sentinel for the
	// TUI's auto-spawn path (issue #63). The dashboard's task_id
	// fallback signal treats agents tagged with task_id ==
	// "coord-"+project AND project == <project> as the project's
	// coord; without the gate, an operator-supplied
	// `fleet dispatch coord-foo --project foo` would hijack the
	// LEFT-column coord slot for a worker. We narrow the rejection
	// to this exact form (codex iter-2 P2): a benign task name like
	// `fleet dispatch coord-cache-warm --project ops` ("coord-cache-
	// warm" != "coord-ops") is unaffected.
	if !opts.coordSpawn && opts.taskID == CoordTaskIDPrefix+opts.project {
		return fmt.Errorf(
			"task_id %q is the reserved coord sentinel for project %q (the TUI uses this exact task_id to mark coordinator dispatches; rename the task)",
			opts.taskID, opts.project)
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
