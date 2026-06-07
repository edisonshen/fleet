package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/coordlock"
	"github.com/edisonshen/fleet/internal/coordrepo"
	"github.com/edisonshen/fleet/internal/enginecfg"
	"github.com/edisonshen/fleet/internal/gc"
	"github.com/edisonshen/fleet/internal/handoff"
	"github.com/edisonshen/fleet/internal/rc"
	"github.com/edisonshen/fleet/internal/spawn"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tmux"
)

// dispatchReconcileFn is the package-level seam for the auto-reconcile
// passes that run at the top of runDispatch (fleet#165 PR-D). Both
// passes are dry-run at the gc layer AND surface-only at the dispatch
// layer:
//
//  1. Apply=false, Kinds=[orphan-agents] — classify which records
//     have no live tmux session on the CURRENT FLEET_TMUX_SOCKET.
//     surfaceOrphanAgentsFromReport emits a stderr warning per
//     candidate (with the `fleet gc --apply --kinds=orphan-agents`
//     one-liner) so the operator can review + archive explicitly.
//  2. Apply=false, Kinds=[orphan-tmux]   — surface a stderr warning
//     for fleet-* tmux sessions with no agent record. NEVER auto-
//     killed (feedback_surface_dont_silo + feedback_user_owns_tmux_config —
//     could belong to a different operator's session).
//
// Why surface-only (not auto-archive) for orphan-agents — codex
// review PR-D iter-2 [P1]: agent.Record does NOT persist its
// spawn-time tmux socket. The gc classifier probes the CURRENT
// FLEET_TMUX_SOCKET (default if unset), so a record whose agent is
// live on a CUSTOM socket looks orphan on the default socket — and
// vice versa. There is no environment-side check this dispatch path
// can use to prove ownership; both "FLEET_TMUX_SOCKET set" AND
// "FLEET_TMUX_SOCKET unset" cases can mis-identify a live agent as
// orphan. Per feedback_surface_dont_silo.md the right move is to
// surface (don't silently destroy) and let the operator decide via
// `fleet gc --apply --kinds=orphan-agents` from a shell where they
// know the socket state.
//
// The companion coord-recovery skip (TaskID prefix coord- → never
// archive) becomes moot in the surface-only model, but is preserved
// in the helper as defense-in-depth in case a future change re-
// enables auto-archive.
//
// Production wraps gc.Reconcile(opts, gc.DefaultDeps()); tests stub
// the var to inject canned reports without touching real state. Both
// passes run BEFORE the coord-spawn lock acquire / live-coord veto so
// orphan records don't pollute those decisions.
//
// Reconcile errors NEVER block dispatch — logged to stderr + continue.
// Risk mitigation in TASK-PLAN §Risks: a leaked dispatch from a
// blocked-by-reconcile path compounds worse than a missed reconcile.
var dispatchReconcileFn = func(opts gc.Options) (gc.Report, error) {
	return gc.Reconcile(opts, gc.DefaultDeps())
}

// dispatchReconcileMaxAge mirrors statusReconcileMaxAge — sockets-age
// classifier floor matches the operator-invoked `fleet gc` default.
// Not used by the dispatch path today (only orphan-agents +
// orphan-tmux kinds run here), but declared for future expansion.
const dispatchReconcileMaxAge = 24 * time.Hour

// tmuxHasSession is the production sessionAliveProbe for
// findRecoveryCandidate. Wraps tmux.HasSession; the indirection keeps
// the recovery helper testable without a live tmux server (tests
// pass closures instead).
var tmuxHasSession = tmux.HasSession

// writeMarkerFn is the seam for the v0.12.1 P0 coord-spawn auto-marker
// write (DESIGN-rc-coord-auto-marker.md). Production wires
// rc.WriteMarker; tests stub to exercise the non-fatal-on-failure
// contract pinned by TestCoordSpawn_MarkerWriteFailure_Degrades.
//
// Both dispatch.go (coord-spawn branch) and handoff.go (coord
// replacement branch) call through this var, so a single test stub
// covers both auto-marker call sites.
var writeMarkerFn = rc.WriteMarker

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
	commandExplicit bool
	noAutoResume    bool
	// noAutoResumeExplicit tracks whether --no-auto-resume was set on
	// the CLI. False means the flag was left at its cobra default
	// (false), and the dead-coord recovery path should inherit from
	// oldRecord.DisableAutoResume rather than reset to false (codex
	// review iter-19 P2). Inheritance ensures a coord launched with a
	// custom shell/REPL wrapper + --no-auto-resume doesn't have
	// natural-language ResumePrompt typed into it on recovery.
	noAutoResumeExplicit bool
	// engine is the engine name persisted on agent.Record.Engine and
	// used to derive the default --command argv (via enginecfg) when
	// --command is not explicitly set. Empty falls through to the root-
	// cmd FLEET_ENGINE env var or the codebase default
	// (enginecfg.DefaultEngine). See resolveEngineFlags at the root cmd
	// for the precedence model.
	engine string
	// engineExplicit reports whether the operator explicitly chose the
	// engine on this dispatch (via --engine / -codex / -claude on root,
	// OR programmatically via opts.engine in tests). False means the
	// engine name was inherited from FLEET_ENGINE env or the codebase
	// default. The dead-coord recovery path uses this gate to decide
	// whether to clamp OldRecord.Engine; without the gate, default-
	// resolved values would silently rewrite a recovered non-default
	// engine back to claude-code (codex review iter-7 P1).
	engineExplicit bool
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
			opts.commandExplicit = cmd.Flags().Changed("command")
			opts.noAutoResumeExplicit = cmd.Flags().Changed("no-auto-resume")
			// engineExplicit tracks whether the operator (or a TUI
			// shell-out) chose the engine via a root flag. Codex review
			// iter-7 P1: distinguishing operator-chosen from
			// default-resolved is necessary so dead-coord recovery
			// only clamps the inherited Engine when the new dispatch
			// actually requested a different engine. Without this gate,
			// every plain `fleet dispatch --coord-spawn` would silently
			// rewrite a recovered codex coord back to claude-code.
			root := cmd.Root()
			opts.engineExplicit = root.PersistentFlags().Changed("engine") ||
				root.PersistentFlags().Changed("codex") ||
				root.PersistentFlags().Changed("claude")
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
		defaultClaudeCommand,
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
	// NOTE: --engine is NOT registered on dispatch as a local flag.
	// Registering it locally would shadow the root persistent flag
	// (cobra binds same-name local flags first), so root's
	// PersistentPreRunE would never see the operator's choice and
	// conflict-detection between --engine and -codex/-claude would
	// silently no-op. The dispatch path reads the resolved engine via
	// FLEET_ENGINE (which root's PersistentPreRunE always populates).
	// The opts.engine field survives only for programmatic test access
	// (engine_flag_test.go's TestDispatch_UnknownEngineRejected sets it
	// directly to exercise the validation gate).
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

// vetoExitCode is the process exit code dispatch uses when the
// --coord-spawn live-coord veto fires. 75 == sysexits.h EX_TEMPFAIL
// ("temporary failure; retry shortly"), deliberately distinct from the
// 70 (EX_SOFTWARE) other dispatch failures map to. The veto is NOT a
// hard error — it means a coord is alive on another tmux socket OR one
// crashed within the freshness window and may be restarting. `fleet
// attach`'s shared coord-spawn wrapper inspects exec.ExitError.ExitCode()
// for this value and treats it as a wait+retry / re-resolve signal
// instead of a never-exit-violating hard exit 70 (BUG #2,
// docs/TASK-PLAN-attach-failover.md INVARIANT 3).
const vetoExitCode = 75

// vetoError is the typed sentinel runDispatch returns when the
// --coord-spawn live-coord veto fires. main() detects it via errors.As
// and maps it to vetoExitCode (75) — mirroring the errClaimsError /
// errRC exit-code wiring. A typed/sentinel error can't cross the
// exec.Command subprocess boundary attach uses, so the EXIT CODE (not a
// stderr substring) is the contract attach classifies on.
type vetoError struct {
	msg string
}

func (e *vetoError) Error() string { return e.msg }

// vetoErrorFromErr reports whether err (or anything it wraps) is a
// *vetoError. Used by main()'s exit-code dispatch.
func vetoErrorFromErr(err error) bool {
	var e *vetoError
	return errors.As(err, &e)
}

func runDispatch(opts *dispatchOpts, stdout io.Writer) error {
	maybeAutoInit(stdout, "")
	if _, err := state.Bootstrap(); err != nil {
		return fmt.Errorf("bootstrap ~/.fleet: %w", err)
	}
	// fleet#165 PR-D auto-reconciliation. Runs BEFORE tmux.Available
	// and the coord-spawn lock so orphan resources surface as a
	// one-time-per-dispatch nudge during the operator's normal
	// workflow (no `fleet gc` invocation required to discover state).
	//
	// Both passes are SURFACE-ONLY (codex review PR-D iter-2 [P1]):
	// agent.Record does not persist its spawn-time tmux socket, so
	// the gc classifier cannot prove a record is truly orphan from
	// the dispatch path. Auto-archiving on ambiguous state would
	// silently destroy live agents whose tmux session is on a custom
	// socket. Per feedback_surface_dont_silo.md the dispatch path
	// surfaces the candidates and lets the operator run `fleet gc
	// --apply --kinds=orphan-agents` (or `fleet rm <id>`) from a
	// shell where they know the socket state. Same shape as the
	// orphan-tmux surface (no kill, just warning).
	//
	//   - --coord-spawn dispatches SKIP the orphan-agents surface
	//     entirely. The coord-spawn path has its own live-coord-veto
	//     + dead-coord-recovery branch (further down in this
	//     function) and additional orphan-agent noise would confuse
	//     the recovery flow. The orphan-tmux surface still runs.
	//   - Worker dispatches surface BOTH orphan-agents and orphan-
	//     tmux candidates to stderr.
	//
	// Failure path is non-blocking: reconcile errors log to stderr +
	// continue (TASK-PLAN §Risks). Healthy state emits zero output.
	runDispatchReconcile(os.Stderr, opts.coordSpawn)
	if err := tmux.Available(); err != nil {
		return err
	}
	// v0.12.2 P0 atomic coord-spawn gate. NB-flock per project
	// (~/.fleet/projects/<p>/.locks/coord-spawn.lock) serializes the
	// (live-coord-veto + agent-record-write + spawn-invocation) tuple
	// for concurrent `fleet dispatch --coord-spawn` against the same
	// project. Closes the TOCTOU race between TUI [a] double-press +
	// in-flight handoff replacement that produced duplicate coords
	// for projects-spark on 2026-05-19 (agents 72ea51b4 + 04f00601).
	//
	// Acquired BEFORE the veto/records-list read at line ~526; held
	// through the dead-coord recovery probe, marker write, and
	// spawn.Spawn; released on runDispatch return via defer.
	//
	// The existing live-coord veto at line ~526 stays as defense in
	// depth: the flock catches in-process / same-host concurrent
	// dispatchers (the dominant case); the veto still catches
	// cross-tmux-socket scenarios where the flock file might be on a
	// different mount.
	//
	// Workers / Agent-tool subagents (opts.coordSpawn=false) DO NOT
	// enter this branch — only coord-spawn dispatches need the gate,
	// matching the original design's scope.
	if opts.coordSpawn && opts.project != "" {
		release, err := coordlock.Acquire(opts.project)
		if err != nil {
			return err
		}
		defer release()
	}
	// Resolve engine BEFORE the rest of the validation so the agent
	// record's engine field is settled when spawn runs. Precedence:
	//   1. opts.engine (programmatic only — tests + Options struct;
	//      NOT a CLI flag, see newDispatchCmd for why)
	//   2. FLEET_ENGINE env (root's PersistentPreRunE always sets this
	//      after running resolveEngineFlags, which conflict-checks the
	//      --engine / -codex / -claude root flags)
	//   3. enginecfg.DefaultEngine
	//
	// Once resolved we also stamp FLEET_ENGINE so the spawned tmux
	// session (and any subprocess spawn calls out to, e.g. the workers
	// CLI shell-out) sees a consistent env value matching the agent
	// record's Engine field. Without this re-stamp, a future code path
	// that sets opts.engine directly (the TUI's startCoordSpawn does
	// this through --engine on the inherited root flag, but tests and
	// programmatic callers can bypass it) would leave the env stale.
	engineName := opts.engine
	// Programmatic opts.engine counts as an explicit choice (tests +
	// Options struct callers). engineExplicit may also already be true
	// if the CLI RunE detected a root --engine / -codex / -claude flag.
	if opts.engine != "" {
		opts.engineExplicit = true
	}
	if engineName == "" {
		engineName = os.Getenv(FleetEngineEnv)
	}
	if engineName == "" {
		engineName = enginecfg.DefaultEngine
	}
	if !enginecfg.Known(engineName) {
		return fmt.Errorf(
			"--engine %q is unknown (known: claude-code, codex)",
			engineName)
	}
	// Coord-spawn engine guard (codex review iter-9 P2): the Python
	// /coordinator skill emits Claude-Agent-tool DISPATCH blocks that
	// ONLY claude-code sessions can consume. The TUI's startCoordSpawn
	// hardcodes --engine claude-code as a self-protective measure, but
	// the direct CLI path (`fleet --engine codex dispatch coord-X
	// --coord-spawn --project X`) would otherwise spawn a codex
	// session that can't fan out workers / reviewers / finishers —
	// the project row would stall the first time it needed a subagent.
	// Fail loud at the CLI to match the TUI's safety guarantee.
	if opts.coordSpawn && engineName != enginecfg.EngineClaudeCode {
		return fmt.Errorf(
			"--coord-spawn requires --engine %s (got %q); the coordinator skill needs Claude's Agent tool to fan out workers/reviewers/finishers",
			enginecfg.EngineClaudeCode, engineName)
	}
	if err := os.Setenv(FleetEngineEnv, engineName); err != nil {
		return fmt.Errorf("set %s=%s: %w", FleetEngineEnv, engineName, err)
	}
	// When the operator did NOT pass --command, swap the cobra default
	// (built for claude-code) for the engine-appropriate wrapper. This
	// is what makes `fleet -codex dispatch <task>` actually launch
	// codex rather than claude.
	//
	// We compare against the claude wrapper bytes-equality the test
	// `TestDefaultClaudeWrapperScript_MatchesFlagDefault` pins; if the
	// operator did pass --command (commandExplicit), we leave it alone
	// regardless of engine. The engine field on the record is still
	// what it was resolved to — the operator's choice of command and
	// engine name aren't required to agree (e.g., a custom wrapper
	// that internally launches claude can still be tagged as engine=
	// codex if the operator wants to track it that way; we don't
	// second-guess).
	// Two signals tell us the caller did NOT pick a command and we
	// should swap in the engine wrapper:
	//   1. CLI path: cobra's RunE sets `commandExplicit` from
	//      Flags().Changed("command"). False ⇒ operator didn't pass
	//      --command, so cobra's claude-shaped default is leaking
	//      through (`[]string{"sh","-c",defaultClaudeWrapperScript}`)
	//      and we should overwrite it for non-claude engines.
	//   2. Programmatic path: in-process callers (tests, future
	//      cross-package wiring) bypass cobra entirely; `commandExplicit`
	//      stays false but `opts.command` is whatever the caller
	//      passed — empty if they didn't care, populated if they did.
	//
	// Distinguish the two: when commandExplicit=false AND opts.command
	// is empty (or equals the cobra default), the caller had no
	// preference → swap. Otherwise the caller picked a command → leave
	// it alone. Pre-fix the guard only checked commandExplicit, which
	// stomped programmatically-injected commands on CI (see
	// TestDispatch_ProgrammaticCommandNotOverriddenByEngine).
	if !opts.commandExplicit &&
		(len(opts.command) == 0 || sameCommand(opts.command, defaultClaudeCommand)) {
		argv, err := enginecfg.BuildWrapperCommand(engineName)
		if err != nil {
			// Should not happen — Known() above already filtered.
			return fmt.Errorf("resolve engine %q: %w", engineName, err)
		}
		opts.command = argv
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
		// v0.12.1 P0: marker write + fresh-spawn inject were originally
		// here, before the early-refusal gates (ListStrict, live-coord
		// veto, FLEET_MAX_SESSIONS, dead-coord recovery). Codex review
		// iter-4 [P1] caught the lifecycle hole: writing the rc-enabled
		// marker before knowing whether the spawn will actually proceed
		// can persist RC opt-in for a project even when the spawn is
		// refused. The pathological case: operator runs `fleet rc down
		// <project>`, then `fleet dispatch --coord-spawn` writes the
		// marker back, then the live-coord veto refuses (existing coord
		// alive) — net effect is the opt-out is silently undone with
		// no new coord. Both moved DOWN to after the gates, immediately
		// before spawn.Spawn (see "v0.12.1 deferred marker+inject" block
		// near line ~724).
	}

	// Pre-fetch the agent record list ONCE so the live-coord veto AND
	// the dead-coord recovery probe below share the same view (codex
	// review iter-15 P1). Use ListStrict so unparseable records also
	// fail the dispatch (codex iter-17 P1): agent.List silently skips
	// corrupt JSON, but an unparseable record might be the live coord's.
	// Skipping it would let the veto fall open and a duplicate coord
	// spawn against the same project on a different tmux socket. Two
	// fail-closed gates here:
	//
	//   - lerr != nil: directory-level read failure (permission, mount).
	//     We can't enumerate records at all → abort.
	//   - len(badIDs) > 0: at least one record file exists but won't
	//     parse → abort, surface the IDs so the operator can fix or
	//     archive them.
	//
	// Other --coord-spawn safety checks (coordStateFresh, recovery
	// probe) fail closed too: stat errors on coord-state.json are
	// treated as "fresh" (live), and the recovery probe shares the
	// same records slice.
	var coordRecords []*agent.Record
	if opts.coordSpawn {
		recs, badIDs, lerr := agent.ListStrict()
		if lerr != nil {
			return fmt.Errorf(
				"--coord-spawn refusing to spawn for project %q: cannot list agent records (%v). "+
					"split-brain risk: a live coord on a different tmux socket might exist whose record we can't read. "+
					"fix the agents directory (likely permission/mount issue under ~/.fleet/agents) and retry",
				opts.project, lerr)
		}
		if len(badIDs) > 0 {
			return fmt.Errorf(
				"--coord-spawn refusing to spawn for project %q: %d unparseable record(s) under ~/.fleet/agents (%v). "+
					"split-brain risk: an unparseable record could belong to a live coord on this or another tmux socket. "+
					"inspect the file(s), then fix the JSON or move them to ~/.fleet/agents/archive/ and retry",
				opts.project, len(badIDs), badIDs)
		}
		coordRecords = recs
	}

	// Idempotent coord-spawn (leak-coord-spawn-idempot): if a coord is
	// ALREADY alive for this project (a record + a live tmux session on
	// THIS socket), don't mint a duplicate — print an attach hint and
	// return cleanly. This is the operator-friendly fast path for a
	// double-press / re-dispatch of an already-running coord. It runs
	// BEFORE the veto because "attach me to the live one" is a success,
	// not a retry-later refusal. Cross-socket live coords aren't caught
	// here (the local probe can't see them) — those fall to the
	// coordSpawnVeto below, which returns exit 75 retry.
	if opts.coordSpawn {
		if live := liveCoordForAttach(opts.taskID, opts.project, coordRecords, tmuxHasSession); live != nil {
			_, _ = fmt.Fprintf(stdout,
				"coord %s already alive for project %s; attach with `fleet attach %s`\n",
				live.ID, opts.project, live.ID)
			return nil
		}
	}

	// Live-coord veto (codex review iter-12 P1 — reinstated after
	// iter-11 briefly removed it). The Python /coordinator skill only
	// holds coordinator.lock for a single tick before releasing it in
	// `finally`, so the lock CANNOT serve as a racing-coords safety
	// net at the dispatch boundary (two coords would alternate ticks,
	// both mutating tasks/worker state — split-brain coordination).
	//
	// OR-based veto (leak-coord-spawn-idempot) — refuse when ANY of:
	//   (a) coord-state.json mtime fresh AND a record exists (the
	//       original AND-gate; steady-state signal once the coord has
	//       ticked at least once).
	//   (b) a live coord record (tmux session alive) exists — covers a
	//       live coord whose coord-state.json hasn't refreshed within
	//       the window yet (e.g. mid-boot, post-first-tick lag).
	//   (c) a FRESH pending-spawn claim exists — the cold-start window:
	//       dispatch #1 wrote the claim + spawned, but coord-state.json
	//       isn't written until the first tick (3-5 min). Without (c),
	//       a dispatch #2 in that gap sees neither (a) nor (b) and
	//       double-spawns. This is root cause #3 from
	//       DESIGN-lifecycle-leak-recurrence.md (projects-spark, two
	//       coords 18.8s apart on 2026-05-29).
	//
	// The (a) AND-gate stays paired (not just coordStateFresh alone) so
	// the dispatch-after-clean-archive case is unblocked (codex iter-8
	// P2): after an operator hits [x] to archive the dead record, the
	// record disjunct fails and — if the claim is stale and coord-state
	// is stale — the dispatch proceeds.
	//
	// Only fires on --coord-spawn (the auto-coord-bootstrap path).
	if opts.coordSpawn {
		stateFresh := coordStateFresh(opts.project)
		recordExists := coordRecordExistsInList(opts.taskID, opts.project, coordRecords)
		liveCoord := liveCoordForAttach(opts.taskID, opts.project, coordRecords, tmuxHasSession) != nil
		claimFresh, claim := coordPendingClaimFresh(opts.project, coordFreshnessWindow)
		if reason := coordSpawnVeto(stateFresh, recordExists, liveCoord, claimFresh); reason != "" {
			// Typed vetoError → main() maps to exit 75 (EX_TEMPFAIL).
			// This is a RETRY signal, not a hard failure: a live coord
			// exists on another tmux socket, one crashed within the
			// freshness window and may be restarting, or a cold-start
			// spawn is still booting. `fleet attach`'s shared
			// coord-spawn wrapper classifies exit 75 (via
			// exec.ExitError.ExitCode()) as wait+retry, NOT a
			// never-exit-violating hard exit 70 (BUG #2, attach-failover
			// plan INVARIANT 3). Spawning over an in-flight/live coord
			// here would split-brain the project.
			claimClause := ""
			if claimFresh {
				claimClause = fmt.Sprintf(" (pending-spawn claim age %s, agent %s)",
					time.Since(claim.SpawnedAt).Round(time.Second), claim.AgentID)
			}
			return &vetoError{msg: fmt.Sprintf(
				"refusing to spawn coord for project %q: %s%s. "+
					"likely causes: (1) a live coord is on a different tmux socket, (2) a coord just crashed within the freshness window, or (3) a coord-spawn is still booting (cold start). "+
					"wait %s for the signal to age out, then retry — recovery will pick up in-flight worker state. "+
					"do NOT `fleet rm` the dead record first; that disables the recovery path",
				opts.project, reason, claimClause, coordFreshnessWindow)}
		}
	}

	// FLEET_MAX_SESSIONS backstop. Runs HERE — after engine /
	// project / coord-spawn validation, BEFORE the dead-coord
	// recovery branch's synth handoff doc write — so a cap refusal
	// doesn't leave a synthetic doc on disk for a replacement that
	// never spawned (codex iter-9 P2). swapsInFlight=0: dispatch
	// is net +1.
	if err := enforceSessionCap(os.Stderr, "", 0); err != nil {
		return err
	}

	// Dead-coord recovery (resume-dead-coord-ab65): when --coord-spawn
	// hits a project whose previous coord left a stale record on disk
	// (pid dead AND tmux session gone), build a synth handoff doc from
	// on-disk state and route the spawn through the
	// OldRecord+NewDocPath branch of spawn.Spawn. The successor's first
	// turn runs handoff_resume.py and picks up the dead coord's
	// in-flight work without throwing state away.
	//
	// Guarded on opts.coordSpawn: an operator-supplied `fleet dispatch
	// <task>` with no --coord-spawn flag must NEVER substitute a
	// recovery synth — that would steal another lineage's identity if
	// the operator's task_id happened to collide.
	//
	// Failure mode: a synth-doc write failure DOES NOT block the
	// dispatch. We log a warning and fall through to a fresh spawn so
	// the project isn't stuck unable to recover. The dead record's
	// state.json + workers/<slug>/ trees stay on disk; a later
	// dispatch attempt can retry the synth write. Choosing
	// "available coord without resume context" over "no coord at all"
	// preserves the operator's ability to manually inspect the workers
	// dir if they need to.
	var oldRecord *agent.Record
	var newDocPath string
	if opts.coordSpawn {
		if dead := findRecoveryCandidate(opts.taskID, opts.project, coordRecords, pidAlive, tmuxHasSession, coordStateFresh); dead != nil {
			docPath, derr := writeRecoveryHandoffDoc(dead, time.Now().UTC())
			if derr != nil {
				_, _ = fmt.Fprintf(stdout,
					"warning: synth handoff doc write failed (%v) — proceeding with fresh spawn (workers state preserved on disk)\n",
					derr)
			} else {
				oldRecord = dead
				newDocPath = docPath
				// Snapshot the dead record's original engine BEFORE
				// the clamp mutates it (codex review iter-12 P2). The
				// command-inheritance check below needs to know
				// whether the engine ACTUALLY changed (pre-clamp
				// vs post-clamp), not just whether it ended up
				// matching engineName.
				preClampEngine := oldRecord.Engine
				// Engine override clamp (codex review iter-4 P2,
				// refined iter-7 P1): only clamp when the caller
				// EXPLICITLY chose the engine on this dispatch (via
				// root --engine / -codex / -claude / programmatic
				// opts.engine). Without this gate, a plain `fleet
				// dispatch <task> --coord-spawn` against a dead codex
				// coord would silently inherit claude-code from the
				// FLEET_ENGINE default and rewrite the recovered
				// lineage's engine — breaking dead-coord recovery for
				// non-default engines. The TUI's auto-spawn path
				// always passes --engine claude-code on the shell-out,
				// so engineExplicit is true there and the clamp still
				// fires as intended.
				if opts.engineExplicit && oldRecord.Engine != engineName {
					oldRecord.Engine = engineName
				}
				// Coord-spawn back-door guard (codex review iter-9 P2):
				// the front-door check above rejects an operator
				// running `fleet --engine codex dispatch coord-X
				// --coord-spawn`, but the recovery path can still
				// inherit a dead codex coord's Engine when the caller
				// didn't set engineExplicit. Block that here so the
				// CLI's coord-spawn contract holds end-to-end: every
				// successful --coord-spawn produces a claude-code
				// successor that can fan out workers.
				if oldRecord.Engine != "" && oldRecord.Engine != enginecfg.EngineClaudeCode {
					return fmt.Errorf(
						"--coord-spawn recovery refused: dead coord %s ran engine %q but the coordinator skill only works under claude-code. "+
							"either pass --engine claude-code to force-migrate the recovery, or archive the dead record (`fleet rm %s`) and start fresh",
						oldRecord.ID, oldRecord.Engine, oldRecord.ID)
				}
				// Command inheritance (codex review iter-7 P2,
				// refined iter-8 P1 + iter-12 P2): when the operator
				// did NOT pass --command, inherit the dead coord's
				// recorded Command so a custom wrapper / non-default
				// argv survives the recovery. Without this the
				// resumed coord restarts under the current default
				// wrapper — wrong binary even though task identity
				// was preserved.
				//
				// The engine-clamp interaction: inheritance MUST be
				// skipped when the engine was clamped to a different
				// value (e.g., explicit --engine claude-code over a
				// dead codex coord). Inheriting the OLD engine's
				// argv under the NEW engine record defeats the clamp.
				// The gate `oldRecord.Engine == engineName` after the
				// clamp block above captures both cases: if no clamp
				// fired (engineExplicit=false), oldRecord.Engine is
				// unchanged and equals itself; if clamp fired,
				// oldRecord.Engine was set to engineName.
				//
				// Codex iter-12 P2: when engineExplicit=true AND the
				// requested engine matches the dead's (e.g., TUI
				// auto-spawn forcing claude-code over a dead claude-
				// code coord that had a custom wrapper), inheritance
				// SHOULD still fire — same engine, same wrapper
				// expectation. The old gate of `!engineExplicit`
				// blocked this case unnecessarily.
				//
				// Operators who DO want a non-default command under a
				// non-default engine can pass --command + --engine
				// together; commandExplicit then takes the explicit
				// path and skips this inheritance.
				// Legacy-record safety (codex review iter-14 P1):
				// pre-v0.9 records have engine="" because the field
				// didn't exist. iter-13 tried to make these inherit
				// by treating "" as claude-code, but that bypasses
				// the claude-only guard above (which short-circuits
				// on engine=="") for legacy records whose custom
				// Command might launch a non-claude engine. We can't
				// introspect an arbitrary argv to know what it
				// spawns, so the safe default is: skip inheritance
				// for legacy records. The wrapper-swap block earlier
				// already populated opts.command with the engine-
				// default wrapper, so the successor runs claude-code
				// as the --coord-spawn contract requires. Operators
				// who want their legacy custom wrapper back can re-
				// add it post-recovery via `fleet handoff <id>
				// --command <wrapper>`.
				legacyRecord := preClampEngine == ""
				if !opts.commandExplicit && !legacyRecord && preClampEngine == engineName && len(oldRecord.Command) > 0 {
					opts.command = append([]string(nil), oldRecord.Command...)
					if opts.coordSpawn && preAllocatedID != "" {
						// Project-suffixed name for the recovered coord
						// — same shape as the fresh-spawn branch above.
						rcSessionName := buildCoordRemoteControlSessionName(preAllocatedID, opts.project)
						rewritten := injectRemoteControlFlagProject(opts.command, rcSessionName, opts.project)
						if !sameCommand(rewritten, opts.command) {
							rewrittenExecArgv = rewritten
						} else {
							// Custom command (non-default shape) — pass
							// nil so the tmux exec uses opts.command
							// untouched and the persisted Command on
							// the record stays clean.
							rewrittenExecArgv = nil
						}
					}
				}
				_, _ = fmt.Fprintf(stdout,
					"recovering dead coord %s for project %s: synth handoff written to %s\n",
					dead.ID, dead.Project, docPath)
			}
		}
	}

	// Cwd resolution on recovery.
	//
	// DESIGN-coord-repo-binding-from-project.md (PR3): for COORD spawns,
	// the repo binding is a property of the PROJECT — resolved through
	// the shared resolver, NEVER inherited from the dead coord's stored
	// Cwd (which may itself be a cwd-bug victim pointing at the wrong
	// tree). When the operator passed an explicit --cwd it is honored
	// (operator authority, same as a `fleet project add` pin); otherwise
	// the resolver runs and REFUSES on an unresolvable project rather
	// than falling back to oldRecord.Cwd. persist=true because recovery
	// is an operator-initiated repair path.
	//
	// Worker recovery (opts.coordSpawn=false) keeps the legacy behavior:
	// inherit oldRecord.Cwd, then spawn.Spawn resolves empty to os.Getwd
	// — workers legitimately follow their dispatch tree (codex iter-9 P2).
	//
	// Durability note (codex PR3 review [P2], owned): an explicit --cwd
	// on a coord spawn for a project with NO usable meta.json is honored
	// for THIS spawn only — it is NOT persisted as the project's pin.
	// Subsequent handoff/drain/recovery (which now ignore record.Cwd and
	// resolve via the resolver) will refuse until the operator registers
	// the checkout durably with `fleet project add --project <p> <cwd>`.
	// This is the §10 owned UX consequence, not a regression: persisting
	// --cwd here would re-implement the resolver's fingerprint+SetRepoPath
	// persist outside the single binder, the exact split-authority the
	// design forbids. The refusal surfaces the `fleet project add` hint.
	spawnCwd := opts.cwd
	if opts.coordSpawn {
		if spawnCwd == "" {
			resolved, rerr := coordrepo.ResolveProjectRepo(opts.project, true)
			if rerr != nil {
				return fmt.Errorf("coord recovery for project %q: %w", opts.project, rerr)
			}
			spawnCwd = resolved
		}
	} else if spawnCwd == "" && oldRecord != nil && oldRecord.Cwd != "" {
		spawnCwd = oldRecord.Cwd
	}
	// DisableAutoResume inheritance on recovery (codex review iter-19
	// P2): when the operator did NOT explicitly pass --no-auto-resume
	// AND the dead coord had it set, inherit. Otherwise a coord
	// launched with a custom shell/REPL wrapper + --no-auto-resume
	// would get natural-language ResumePrompt typed into it on
	// recovery, breaking the very case --no-auto-resume exists for.
	// The explicit-flag gate lets operators override on the recovery
	// dispatch if they want to change the policy.
	disableAutoResume := opts.noAutoResume
	if !opts.noAutoResumeExplicit && oldRecord != nil && oldRecord.DisableAutoResume {
		disableAutoResume = true
	}

	// v0.12.1 P0 deferred marker+inject (DESIGN-rc-coord-auto-marker.md,
	// operator G2 2026-05-18; codex review iter-4 [P1]): coord-spawn
	// auto-opts-in to RC so the rc.Enabled(project) gate inside
	// injectRemoteControlFlagProject passes on the same dispatch.
	// Without this, freshly-dispatched coords spawn with plain claude
	// argv and `/remote-control` + `fleet rc connect <project>` return
	// `not_enabled` until the operator manually runs `fleet rc up`.
	//
	// Runs HERE (after ListStrict / live-coord veto / FLEET_MAX_SESSIONS
	// / dead-coord recovery — all the gates that can refuse the spawn),
	// not back at the original site near line ~445. Earlier placement
	// could persist the rc-enabled marker even when the spawn was
	// rejected, silently undoing an operator's `fleet rc down`.
	//
	// Scope carve-out: workers / Agent-tool subagents
	// (opts.coordSpawn=false) do NOT enter this branch, so the v0.12
	// push-storm protection that targeted runaway reviewer subagents
	// is preserved — only coords are auto-opted-in.
	//
	// Recovery-spawn argv inheritance (the inject inside the
	// findRecoveryCandidate block above at line ~673) ALSO consumes
	// this marker: that path runs AFTER the gates so its inject
	// either reads the marker we just wrote here, or — if recovery
	// already wrote rewrittenExecArgv — we leave it untouched
	// (sameCommand short-circuit below).
	//
	// Failure is non-fatal: log a warning and continue with plain
	// claude argv (operator can still recover with `fleet rc up`).
	if opts.coordSpawn && opts.project != "" {
		if err := writeMarkerFn(opts.project); err != nil {
			_, _ = fmt.Fprintf(os.Stderr,
				"warning: rc.WriteMarker(%q) failed: %v (continuing with plain claude argv; run `fleet rc up %s` to recover)\n",
				opts.project, err, opts.project)
		}
		// Only inject when the recovery path didn't already set
		// rewrittenExecArgv (skips wasted work + avoids double-rewriting
		// a custom command). The recovery-spawn inject at line ~673
		// runs inside the dead-coord-recovery branch; if it fired,
		// rewrittenExecArgv is set and we honor it as-is. Otherwise,
		// this is a fresh-spawn coord (or a recovery that inherited
		// the engine-default wrapper without inheriting opts.command)
		// and we do the rewrite here.
		if rewrittenExecArgv == nil {
			rcSessionName := buildCoordRemoteControlSessionName(preAllocatedID, opts.project)
			rewritten := injectRemoteControlFlagProject(opts.command, rcSessionName, opts.project)
			if !sameCommand(rewritten, opts.command) {
				rewrittenExecArgv = rewritten
			}
		}
	}

	// Durable pending-spawn claim (leak-coord-spawn-idempot). Written
	// HERE — under the coord-spawn flock (held via `defer release()`
	// for the whole runDispatch when coordSpawn), after every gate that
	// could refuse the spawn, immediately before spawn.Spawn. The claim
	// bridges the cold-start window: it makes a second dispatch's
	// coordSpawnVeto (disjunct c) refuse until the new coord's first
	// tick clears it (skills/coordinator/loop.py). Atomic .tmp+rename
	// so a contending veto never reads a torn claim.
	//
	// Failure is non-fatal: if the claim write fails we log and proceed
	// to spawn anyway — the NB-flock + coord-state veto remain as
	// defense in depth, and refusing the spawn over a claim-write error
	// would leave the project coord-less (worse than the narrow
	// double-spawn race the claim closes).
	if opts.coordSpawn && opts.project != "" && preAllocatedID != "" {
		if cerr := writeCoordPendingClaim(opts.project, preAllocatedID); cerr != nil {
			_, _ = fmt.Fprintf(os.Stderr,
				"warning: coord-spawn pending-claim write failed (%v) — proceeding (flock + coord-state veto still guard against double-spawn)\n",
				cerr)
		}
	}

	rec, err := spawn.Spawn(spawn.Options{
		OldRecord:         oldRecord,
		NewDocPath:        newDocPath,
		TaskID:            opts.taskID,
		Project:           opts.project,
		Cwd:               spawnCwd,
		Command:           opts.command,
		ExecCommand:       rewrittenExecArgv,
		PreAllocatedID:    preAllocatedID,
		DisableAutoResume: disableAutoResume,
		Engine:            engineName,
	})
	if err != nil {
		return err
	}

	// Recovery-path bookkeeping (codex review iter-11 P1): we
	// deliberately DO NOT archive the dead record here. Local tmux
	// probes cannot rule out a live coord on a different tmux
	// socket — if findRecoveryCandidate misclassified a still-live
	// coord as dead, archiving its record pre-emptively would
	// disappear the live coord from `fleet status` and the TUI even
	// though the duplicate-recovery's coord skill will lose the
	// coordinator.lock race and exit. Leaving the dead record on
	// disk is the safe default:
	//
	//   - if the recovery was correct (dead coord truly gone): the
	//     successor is now live; the dead record sits unarchived on
	//     the dashboard until the operator hits [x] (matches the v0.1
	//     cleanup UX for crashed agents).
	//   - if the recovery misclassified (live coord on different
	//     socket): the duplicate successor's coord skill loses the
	//     NB-flock race and exits cleanly; both records remain;
	//     dashboard truthfully shows both; operator decides what's
	//     stale.
	//
	// Idempotency: a subsequent dispatch sees the same dead record
	// and re-runs recovery. spawn.Spawn's OldRecord-branch fields
	// (handoff_number++, last_handoff_path=NewDocPath) are derived
	// per-call from oldRecord state, so the chain stays coherent.
	// findRecoveryCandidate's newest-wins tiebreaker (codex iter-7
	// P2) picks the most-recent dead record if multiple recoveries
	// accumulate, so successive recoveries don't fork lineages.
	_ = oldRecord // archive intentionally skipped; see comment block.

	_, _ = fmt.Fprintf(stdout, "agent %s spawned\n", rec.ID)
	_, _ = fmt.Fprintf(stdout, "  task:    %s\n", rec.TaskID)
	_, _ = fmt.Fprintf(stdout, "  project: %s\n", rec.Project)
	_, _ = fmt.Fprintf(stdout, "  tmux:    %s\n", rec.TmuxSession)
	// Recovery-path prompt swap (codex review iter-2 P1): when the
	// dead-coord recovery branch wrote a synth handoff doc, the
	// successor MUST receive `handoff.ResumePrompt(newDocPath)` so its
	// first action runs handoff_resume.py and picks up in-flight
	// workers. Without this, the synth doc sits on disk and the new
	// /coordinator session boots fresh, orphaning worker state.
	//
	// Replaces (rather than appends to) opts.prompt because the coord-
	// spawn role briefing ends with "Run /coordinator now" — same
	// terminal action the resume doc's ## First Action body already
	// instructs (see handoff.FirstAction). Two directives competing in
	// one prompt would race over which one the agent acts on first.
	// The resume doc IS the source of truth post-recovery; the role
	// briefing's content is for fresh coord boots only.
	//
	// Gated on !opts.noAutoResume (codex review iter-4 P2):
	// --no-auto-resume is the documented escape hatch for shells/REPLs/
	// alternate engines that can't consume natural-language prompts.
	// Honoring it here too keeps the recovery path consistent with the
	// regular handoff path's contract.
	if newDocPath != "" && !disableAutoResume {
		opts.prompt = handoff.ResumePrompt(newDocPath)
	}
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
			// Codex review iter-6 P2: include the same sigil
			// ("initial prompt not delivered") the TUI's
			// dispatchPromptFailedMarker matches on. Without this,
			// the TUI's startCoordSpawn parses stdout for the marker
			// and finds it missing — even though the prompt sits
			// unsubmitted in Claude's input box — so it writes the
			// coord-spawn marker and treats the session as a live
			// coord. The literal "initial prompt not delivered"
			// substring is the wire contract.
			_, _ = fmt.Fprintf(stdout,
				"warning: initial prompt not delivered (typed but Enter did not submit; still in Claude's input box after retry) — attach and press Enter manually\n")
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

// defaultClaudeCommand is the canonical argv cobra leaves in
// opts.command when the operator did NOT pass --command. Mirrors the
// StringSliceVar default at flag registration (newDispatchCmd).
// runDispatch compares against this to detect "cobra-default-pass-
// through" so the engine→wrapper swap can fire for non-claude engines
// without stomping a caller-supplied command.
var defaultClaudeCommand = []string{"sh", "-c", defaultClaudeWrapperScript}

// injectRemoteControlFlag is a thin local wrapper around
// spawn.InjectRemoteControlFlag so the dispatch + handoff call sites
// in this package read with identical signatures. Lifted to
// internal/spawn so internal/handoffop (the auto-handoff drain) can
// use the same rewrite without a cross-package import-cycle through
// cmd/fleet.
//
// FLEET_RC_BOOTSTRAP_DISABLED gate (rc-listener-bootstrap-sk-3e98):
// when the env var is set to any non-empty value, this wrapper
// returns the input slice unchanged — no `--remote-control "<name>"`
// injection. Kept through v0.12 as a belt-and-suspenders gate for
// test paths that might somehow set a marker; v0.13 retires it once
// the marker-gate (rc-enabled) is field-proven via the v0.12
// CI-invariant test (rc_invariant_test.go).
//
// NB: this signature stays 2-arg for backwards-compat with the
// existing dispatch_test.go / rc_bootstrap_env_test.go pinning the
// env-gate contract. Project-aware callers use
// injectRemoteControlFlagProject below.
func injectRemoteControlFlag(command []string, sessionName string) []string {
	if os.Getenv("FLEET_RC_BOOTSTRAP_DISABLED") != "" {
		return command
	}
	return spawn.InjectRemoteControlFlag(command, sessionName)
}

// injectRemoteControlFlagProject is the v0.12 project-aware variant.
// Adds an rc.Enabled(project) check on top of the existing env-gate.
// Used by coord-spawn + dispatch-recovery (handoff replacement uses
// the same gate at cmd/fleet/handoff.go).
//
// Two-layer gate (v0.12 DESIGN-rc-listener-lifecycle.md §"Attach-
// surface gates"):
//
//  1. PRIMARY: rc.Enabled(project) — per-project marker. Absent means
//     operator hasn't opted in; no flag injection.
//  2. SECONDARY (defense-in-depth from PR #157, retired in v0.13):
//     FLEET_RC_BOOTSTRAP_DISABLED env var.
//
// Empty project (legacy / untargeted dispatch) returns false from
// rc.Enabled, so the gate fires the same way as for projects-without-
// markers.
func injectRemoteControlFlagProject(command []string, sessionName, project string) []string {
	if os.Getenv("FLEET_RC_BOOTSTRAP_DISABLED") != "" {
		return command
	}
	if !rc.Enabled(project) {
		return command
	}
	return spawn.InjectRemoteControlFlag(command, sessionName)
}

// sameCommand mirrors the spawn.SameCommand helper. See doc there.
func sameCommand(a, b []string) bool {
	return spawn.SameCommand(a, b)
}

// buildCoordRemoteControlSessionName constructs the per-coord
// --remote-control session name. The shape
// `fleet-coord-<id>-<project>` is what the operator sees on
// claude.ai mobile / web; the project suffix lets them distinguish
// coords across projects (spark vs fleet vs rainier) instead of
// seeing identical `fleet-coord-<8hex>` entries.
//
// The daemon's --remote-control-session-name-prefix stays the broad
// `fleet-coord` literal (skills/coordinator/remote_control.py), so any
// session name starting with that prefix attaches to the same daemon.
// We don't need a per-project daemon for the coord side; the per-
// project naming is purely operator-visible disambiguation.
func buildCoordRemoteControlSessionName(agentID, project string) string {
	return spawn.CoordRemoteControlSessionName(agentID, project)
}

// buildHandoffRemoteControlSessionName mirrors
// buildCoordRemoteControlSessionName for handoff replacements.
// Format: `fleet-handoff-<id>-<project>`. See the spawn-side helper
// for the suffix-extension rationale.
func buildHandoffRemoteControlSessionName(agentID, project string) string {
	return spawn.HandoffRemoteControlSessionName(agentID, project)
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

// dispatchAgentListFn is the production seam used by
// surfaceOrphanAgentsFromReport to enrich each gc Action with the
// record's TaskID/Project for a more useful operator warning line.
// Production wraps agent.List; tests override the var to inject
// canned record slices without touching ~/.fleet/agents.
var dispatchAgentListFn = func() ([]*agent.Record, error) { return agent.List() }

// shouldSkipArchiveForRecovery reports whether an orphan-agents
// archive on the dispatch path MUST skip the given record. Currently
// the dispatch path is fully surface-only (codex review PR-D iter-2
// [P1]) so this helper is defense-in-depth — preserved so that any
// future change re-enabling auto-archive can't silently destroy
// dead-coord recovery state.
//
// Load-bearing case: any record whose TaskID starts with
// CoordTaskIDPrefix ("coord-") is a coordinator record. Archiving it
// on a worker dispatch would block a later `fleet dispatch
// --coord-spawn` against that project from running
// findRecoveryCandidate, breaking dead-coord recovery.
//
// The CoordTaskIDPrefix check covers BOTH the project this dispatch
// targets AND every other project's coord records that happen to be
// on disk. A worker dispatch against project A must not collateral-
// archive project B's dead coord record — recovery is per-project.
func shouldSkipArchiveForRecovery(r *agent.Record) bool {
	if r == nil {
		return true
	}
	return strings.HasPrefix(r.TaskID, CoordTaskIDPrefix)
}

// runDispatchReconcile fires the two reconcile passes documented on
// dispatchReconcileFn. Errors are non-fatal — logged to stderr and
// swallowed so the dispatch path can proceed to its normal failure
// modes (tmux unavailable, lock contention, spawn error) without a
// reconcile probe failure compounding into "dispatch refuses to run".
//
// Pass 1 (skipped when isCoordSpawn=true): Apply=false,
// Kinds=[orphan-agents] — every would-archive action prints one
// stderr line of the shape:
//
//	warning: orphan agent record <id> (task=<task_id>, project=<project>)
//	  on this tmux socket; archive manually with
//	  `fleet gc --apply --kinds=orphan-agents` from a shell where you
//	  know the socket state (record does not persist spawn-time socket).
//
// Surface (don't auto-archive) per feedback_surface_dont_silo +
// codex review PR-D iter-2 [P1]: the gc classifier cannot prove a
// record is truly orphan because agent.Record does not persist its
// spawn-time socket. A live agent on a custom socket looks orphan to
// a dispatch on the default socket, and vice versa. Surface lets the
// operator decide.
//
// Pass 1 is also skipped wholesale when isCoordSpawn=true: the
// coord-spawn path has its own live-coord-veto + dead-coord-recovery
// branch (further down in runDispatch); additional orphan-agent
// noise during a recovery flow is confusing.
//
// Pass 2 (always fires): Apply=false, Kinds=[orphan-tmux] — every
// surface action prints one stderr line of the shape:
//
//	warning: orphan tmux session <name> has no agent record;
//	  kill manually with `tmux kill-session -t <name>`
//
// Surface (don't auto-kill) per feedback_surface_dont_silo +
// feedback_user_owns_tmux_config — fleet-named sessions outside the
// current operator's record set may belong to a different workflow.
//
// stderr is io.Writer (not *os.File) so callers can redirect for
// tests via the standard os.Stderr swap pattern.
func runDispatchReconcile(stderr io.Writer, isCoordSpawn bool) {
	// Pass 1: orphan agent records → surface only. Coord-spawn
	// dispatches skip the whole pass to avoid noise during the
	// recovery flow.
	if !isCoordSpawn {
		report, err := dispatchReconcileFn(gc.Options{
			Apply:  false,
			MaxAge: dispatchReconcileMaxAge,
			Kinds:  []gc.Kind{gc.KindOrphanAgents},
		})
		if err != nil {
			_, _ = fmt.Fprintf(stderr,
				"warning: reconcile (orphan-agents) probe failed: %v (continuing — dispatch is not gated on cleanup)\n",
				err)
		}
		surfaceOrphanAgentsFromReport(stderr, report)
	}
	// Pass 2: orphan tmux sessions → surface only, never kill. Runs
	// in BOTH worker and coord-spawn paths because it doesn't mutate
	// state — just informs the operator about accumulating leaks.
	report, err := dispatchReconcileFn(gc.Options{
		Apply:  false,
		MaxAge: dispatchReconcileMaxAge,
		Kinds:  []gc.Kind{gc.KindOrphanTmux},
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr,
			"warning: reconcile (orphan-tmux) probe failed: %v (continuing — dispatch is not gated on cleanup)\n",
			err)
	}
	for _, a := range report.Actions {
		if a.Kind != gc.KindOrphanTmux {
			continue
		}
		// Codex review PR-D iter-7 [P2]: include -S $FLEET_TMUX_SOCKET
		// in the kill-session hint when set. The reconcile probe
		// found the orphan session via fleet's tmux helpers which
		// always pass -S; the bare `tmux kill-session -t <name>`
		// hint would talk to the default server instead. Mirrors
		// status.go's orphanCleanupHint.
		killCmd := fmt.Sprintf("tmux kill-session -t %s", a.Target)
		if sock := strings.TrimSpace(os.Getenv("FLEET_TMUX_SOCKET")); sock != "" {
			killCmd = fmt.Sprintf("tmux -S %s kill-session -t %s", sock, a.Target)
		}
		_, _ = fmt.Fprintf(stderr,
			"warning: orphan tmux session %s has no agent record; kill manually with `%s`\n",
			a.Target, killCmd)
	}
}

// surfaceOrphanAgentsFromReport emits one stderr line per orphan-agents
// would-archive action so the operator can review and decide whether
// to archive. The dispatch path NEVER mutates agent records itself
// (codex review PR-D iter-2 [P1]) because the gc classifier cannot
// prove a record is truly orphan without persisting the spawn-time
// tmux socket on agent.Record (out-of-scope schema change). Surface-
// only here mirrors the orphan-tmux pass and the feedback_surface_
// dont_silo.md guidance.
//
// Hint shapes (kept in sync with status.go's orphanCleanupHint per
// codex review PR-D iter-6 [P2]):
//
//   - Coord records (TaskID prefix "coord-"): name `fleet rm <id>`
//     plus an explicit "do NOT run `fleet gc --apply
//     --kinds=orphan-agents`" warning. The global archive would
//     also reap this coord record, disabling dead-coord recovery.
//   - Worker records: name per-record `fleet rm <id>` (NOT the
//     global gc command). A mixed orphans list with a worker row
//     AND a coord row would otherwise let an operator follow the
//     worker hint and reap the coord record too — codex iter-6
//     [P2] caught that footgun.
//
// The helper enriches the warning with TaskID + Project (a useful
// triage signal) by re-looking-up records via dispatchAgentListFn;
// when the list fails or a record vanished between probe + lookup,
// the line falls back to the bare ID + worker-shaped command (the
// recovery-safe hint requires knowing the task_id, which we don't
// have without the record). Empty report short-circuits before the
// listing call.
func surfaceOrphanAgentsFromReport(stderr io.Writer, report gc.Report) {
	if len(report.Actions) == 0 {
		return
	}
	// Best-effort record enrichment; failure is non-fatal.
	byID := map[string]*agent.Record{}
	if records, lerr := dispatchAgentListFn(); lerr == nil {
		for _, r := range records {
			if r == nil {
				continue
			}
			byID[r.ID] = r
		}
	}
	for _, a := range report.Actions {
		if a.Kind != gc.KindOrphanAgents {
			continue
		}
		rec, ok := byID[a.Target]
		if ok && shouldSkipArchiveForRecovery(rec) {
			_, _ = fmt.Fprintf(stderr,
				"warning: orphan coord record %s (task=%s, project=%s) — %s; preserve for dead-coord recovery, or archive with `fleet rm %s` ONLY if you're sure no --coord-spawn against project %s will need it (do NOT run `fleet gc --apply --kinds=orphan-agents` — that reaps this record too)\n",
				rec.ID, rec.TaskID, rec.Project, a.Reason, rec.ID, rec.Project)
			continue
		}
		taskID := "?"
		project := "?"
		if ok {
			if rec.TaskID != "" {
				taskID = rec.TaskID
			}
			if rec.Project != "" {
				project = rec.Project
			}
		}
		_, _ = fmt.Fprintf(stderr,
			"warning: orphan agent record %s (task=%s, project=%s) — %s; archive manually with `fleet rm %s` (per-record; verify FLEET_TMUX_SOCKET matches agent's spawn socket before running — record does not persist spawn-time socket)\n",
			a.Target, taskID, project, a.Reason, a.Target)
	}
}
