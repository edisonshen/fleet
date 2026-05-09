package main

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/spawn"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tmux"
)

// dispatchOpts captures cobra-parsed flags so the run() func is testable
// without poking at globals.
type dispatchOpts struct {
	taskID  string
	project string
	// projectExplicit tracks whether --project was set on the command
	// line (vs left at its cobra default "default"). The defensive check
	// for --coord-spawn requires this so a TUI bug that drops --project
	// from the auto-spawn args fails loud at dispatch instead of writing
	// an agent record with project="default" that the dashboard then
	// can't bind to any project row (issue #70).
	projectExplicit bool
	cwd             string
	command         []string
	noAutoResume    bool
	// prompt is the optional first-turn prompt to type into the
	// freshly-spawned tmux session AFTER the pane stabilizes. Empty
	// (default) → no prompt; the operator types one manually after
	// `fleet attach`. Non-empty → spawn calls
	// spawn.SendInitialPrompt(session, prompt) so the agent boots with
	// the prompt already executed. Used by the v0.2 dashboard's
	// project-row [a] auto-spawn path (issue #60) where the coord agent
	// is bootstrapped non-interactively with the role-boundary prompt
	// from internal/tui/keys.go:coordSpawnPrompt (issue #80).
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

// remoteControlSessionPrefix is the literal prefix that
// `skills/coordinator/remote_control.py:spawn_daemon_if_needed`
// passes to `claude remote-control --remote-control-session-name-prefix`.
// Coord-spawn dispatches inject `--remote-control "<prefix>-<agent_id>"`
// at session start (issue #73) so the agent registers under a name
// the daemon will actually accept; any other prefix wouldn't match
// the daemon's filter (codex review #73 iter-2 P1).
//
// Keep byte-identical with the Python side. There's no shared schema
// file yet (the Python skill doesn't import Go); a string-equality
// test pins the contract.
const remoteControlSessionPrefix = "fleet-coord"

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
			opts.projectExplicit = cmd.Flags().Changed("project")
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
		[]string{"sh", "-c", defaultClaudeWrapperScript},
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
	// spawn path bootstraps a coord with the role-boundary prompt
	// (internal/tui/keys.go:coordSpawnPrompt, issue #80) so the agent
	// boots with explicit role / delegate / allowed constraints in
	// place. Empty → no prompt typed (the classic interactive dispatch
	// flow).
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
	// Issue #70: when the TUI's auto-spawn shells out with --coord-spawn,
	// it MUST also set --project explicitly to the target project name.
	// The dashboard binds coords to project rows by matching record.Project
	// against ProjectRow.Name; if --project is missing the agent record
	// gets project="default" (the cobra flag default) and the coord shows
	// up under no project row at all (or under a synthetic "default" row
	// if other agents share that tag). Failing loud here turns a silent
	// render bug into an immediate dispatch error so any future TUI
	// regression that drops --project from the args slice is caught at
	// the wire instead of the operator's screen.
	if opts.coordSpawn && !opts.projectExplicit {
		return fmt.Errorf(
			"--coord-spawn requires --project to be set explicitly (the TUI's auto-spawn always passes the target project name; missing --project means the args slice was malformed)")
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

	// Issue #73 + dispatch-flag-injection P0 (mobile pairing broken):
	// coord-spawn dispatches inject Claude Code's documented
	// `--remote-control "<session>"` flag so the operator's `[a]`
	// re-attach can reach the coord's tools without manually running
	// `/remote-control` inside the session.
	//
	// Session-name prefix MUST be "fleet-coord" — that's the prefix
	// `bootstrap_remote_control` (skills/coordinator/remote_control.py)
	// passes via `--remote-control-session-name-prefix` when it spawns
	// the daemon on the coord's first tick. Names that don't share
	// this prefix would never attach to the daemon (codex review #73
	// iter-2 P1). We append the agent_id so each coord registers as
	// a unique session within the same daemon scope.
	//
	// Pre-allocate the agent ID HERE (instead of letting spawn.Spawn
	// call agent.NewID()) so we can interpolate it into the shell
	// wrapper's claude argv. Spawn already supports this surface via
	// Options.PreAllocatedID — used by the handoff path for the same
	// "ID needed before spawn" reason.
	//
	// We pass the rewritten argv via Options.ExecCommand (used for
	// THIS tmux exec only); the persisted agent.Record.Command stays
	// the clean opts.command so a future handoff that reads
	// oldRec.Command doesn't inherit a stale `--remote-control
	// "fleet-coord-<old-id>"` flag (codex review #73 iter-1 P1).
	//
	// HISTORY — daemon-presence gate REMOVED (this commit). The
	// previous code path was:
	//
	//     if opts.coordSpawn && remoteControlDaemonRunning() { ... }
	//
	// The daemon-presence gate (codex review #73 iter-5 P1) was a
	// belt-and-braces guard against "claude exits non-zero when the
	// daemon isn't listening yet". It turned out to be exactly the
	// wrong half of a chicken-and-egg loop with Bug 1 in
	// skills/coordinator/remote_control.py:
	//
	//   - The Python skill's spawn_daemon_if_needed used a broad
	//     `pgrep -f "claude remote-control"` guard. When ANY
	//     remote-control daemon was running (e.g. the operator's
	//     fleet-handoff daemon from a prior handoff), the coord skill
	//     silently SKIPPED its own daemon launch.
	//   - This dispatch path's gate `remoteControlDaemonRunning()`
	//     correctly narrowed on the fleet-coord prefix. With no
	//     fleet-coord daemon ever launching, this gate ALWAYS
	//     returned false on coord spawn → the flag was NEVER
	//     injected → mobile pairing was broken on every coord across
	//     the operator's whole fleet.
	//
	// Survey at the time of this fix found ALL 7 live agents on the
	// operator's machine (4 coords + 3 workers, where 2 coords were
	// the in-flight ones we needed to pair) missing the
	// --remote-control flag in their persisted command. The Python
	// pgrep fix (Bug 1) closes one half of the chicken-and-egg, but
	// keeping the gate here re-creates the same failure on every
	// fresh coord boot until /tmp gets cleared — because the gate
	// asks "is the daemon up *right now*", and on a clean dispatch
	// the answer is always no (the bash bootstrap inside the coord
	// runs many seconds AFTER tmux.Spawn returns).
	//
	// Why dropping the gate is safe:
	//   - claude --remote-control "<name>" retries on its own
	//     internal connection loop. The CLI does not exit non-zero
	//     when the daemon is briefly absent; it waits and connects
	//     when the daemon comes up. The wrapper's RC=$? branch only
	//     fires on a real claude process exit (Ctrl-D, /exit, or a
	//     hard error), not on transient connection retries.
	//   - The fleet-coord daemon launches on the coord's first tick
	//     (skills/coordinator/remote_control.py:spawn_daemon_if_needed)
	//     within seconds of the agent booting. Worst case is a few
	//     seconds of "daemon not yet up" while claude retries.
	//   - The inbox-relay path
	//     (skills/coordinator/remote_control.py:seed_inbox) still
	//     runs as a belt-and-braces fallback: even if the daemon
	//     comes up but the spawned claude session doesn't latch
	//     onto it for some reason, the seeded /remote-control inbox
	//     message will land on the next Stop hook.
	//
	// remoteControlDaemonRunning() and its tests have been removed
	// (search the git log for the deletion commit). Any new code
	// that wants to check daemon presence should NOT gate flag
	// injection on it — the gate is the bug.
	//
	// Non-coord dispatches (workers, operator-shelled `fleet dispatch`)
	// keep the original command unchanged: they don't get auto-attach,
	// matching v0.1's manual-attach contract.
	//
	// If the operator overrode --command (scripted pipeline, alt
	// engine), injectRemoteControlFlag returns the slice unchanged —
	// we only rewrite the documented default shell-wrapped claude
	// shape. Custom argvs are out of scope.
	preAllocatedID := ""
	var rewrittenExecArgv []string
	if opts.coordSpawn {
		preAllocatedID = agent.NewID()
		// Match the daemon prefix from skills/coordinator/remote_control.py
		// (`--remote-control-session-name-prefix "fleet-coord"`). Names
		// without this prefix wouldn't attach to the daemon.
		rcSessionName := remoteControlSessionPrefix + "-" + preAllocatedID
		rewritten := injectRemoteControlFlag(opts.command, rcSessionName)
		// Only set ExecCommand when the rewrite actually changed
		// something — passing through an unchanged custom --command
		// avoids a no-op divergence between Command and ExecCommand
		// in spawn.Options.
		if !sameCommand(rewritten, opts.command) {
			rewrittenExecArgv = rewritten
		}
	}

	rec, err := spawn.Spawn(spawn.Options{
		TaskID:            opts.taskID,
		Project:           opts.project,
		Cwd:               opts.cwd,
		Command:           opts.command,
		ExecCommand:       rewrittenExecArgv,
		PreAllocatedID:    preAllocatedID,
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
		//
		// The two failure modes (issue #65 Fix D):
		//   - perr != nil: send-keys itself errored. tmux session might
		//     be dead before we reached it, or the pane is borked.
		//   - perr == nil, submitted == false: send-keys succeeded but
		//     the prompt remained sitting in Claude's input box after
		//     two Enter attempts. Surfacing this is critical — the
		//     dispatch caller (TUI auto-spawn) currently treats exit-0
		//     as full success, but the coord won't actually run
		//     /coordinator until the operator attaches and presses
		//     Enter manually. Logging here lets operator log analysis
		//     correlate "coord-spawn-marker exists but coord is idle"
		//     with the unsubmitted-warning that fired during dispatch.
		submitted, perr := sendInitialPrompt(rec.TmuxSession, opts.prompt)
		switch {
		case perr != nil:
			_, _ = fmt.Fprintf(stdout,
				"warning: initial prompt not delivered (%v) — attach to type it manually\n",
				perr)
		case !submitted:
			_, _ = fmt.Fprintf(stdout,
				"warning: initial prompt typed but Enter did not submit (still in Claude's input box after retry) — attach and press Enter manually\n")
		default:
			_, _ = fmt.Fprintf(stdout, "  prompt:  delivered\n")
		}
	}
	_, _ = fmt.Fprintf(stdout, "\nattach with: fleet attach %s\n", rec.ID)
	return nil
}

// defaultClaudeWrapperScript re-exposes spawn.DefaultClaudeWrapperScript
// at the top-level constant name the existing dispatch surface uses.
// Keeping the alias means the test that pins the byte-equality between
// `--command`'s default element and the wrapper-script literal stays
// readable without cross-package qualification.
const defaultClaudeWrapperScript = spawn.DefaultClaudeWrapperScript

// injectRemoteControlFlag is a thin local wrapper around
// spawn.InjectRemoteControlFlag so the dispatch + handoff call sites
// in this package read with identical signatures. Lifted to
// internal/spawn so internal/handoffop (the auto-handoff drain) can
// use the same rewrite without a cross-package import-cycle through
// cmd/fleet.
func injectRemoteControlFlag(command []string, sessionName string) []string {
	return spawn.InjectRemoteControlFlag(command, sessionName)
}

// sameCommand mirrors the spawn.SameCommand helper. See doc there.
func sameCommand(a, b []string) bool {
	return spawn.SameCommand(a, b)
}

// sendInitialPrompt is a var so tests can stub the tmux interaction.
// Production wraps spawn.WaitForReadyToPrompt + SendPromptKeysVerified
// so the caller sees both transport-level errors AND the post-send
// verification result (submitted=false means the prompt typed but
// Enter didn't submit — issue #65 Fix D).
var sendInitialPrompt = func(session, prompt string) (submitted bool, err error) {
	if prompt == "" {
		return true, nil
	}
	if werr := spawn.WaitForReadyToPrompt(session); werr != nil {
		_, _ = fmt.Fprintf(os.Stderr,
			"warning: initial-prompt readiness poll for %s did not converge: %v (sending anyway)\n",
			session, werr)
	}
	return spawn.SendPromptKeysVerified(session, prompt)
}
