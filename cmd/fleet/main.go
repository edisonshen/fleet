// Command fleet is the operator-facing CLI for the Fleet parallel-agent
// console. See docs/DESIGN.md for the v1 product spec.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/edisonshen/fleet/internal/enginecfg"
	"github.com/edisonshen/fleet/internal/tui"
)

// Version is overwritten at release time via -ldflags. Default
// "dev" so non-released builds (`go run`, `go build` without
// goreleaser) DON'T get told to "brew upgrade fleet" — brew didn't
// install that binary, and a dev binary may even contain unreleased
// code newer than the latest tag. Released builds get the actual
// tag injected via -X main.Version=<tag>.
var Version = "dev"

// FleetEngineEnv is the env var Fleet uses to propagate the operator-
// chosen engine into subprocesses (notably the TUI's `fleet dispatch
// --coord-spawn` shell-out and any future sibling that wants to spawn
// an agent inheriting the parent session's engine). Read-only — the
// TUI never mutates it, just reads + forwards as `--engine <name>` on
// downstream argv.
//
// The env-var hop exists because cobra root flags don't propagate to
// subprocesses; we want `fleet -codex` to mean "every coord auto-
// spawn from this TUI session uses codex" without the operator
// repeating `--engine codex` on every shell-out. Tests set this
// directly to exercise the propagation.
const FleetEngineEnv = "FLEET_ENGINE"

func newRootCmd() *cobra.Command {
	// rootEngine captures the persistent --engine value plus the
	// -codex / -claude shorthand flags. cobra exposes the shorthand as
	// bool flags; the PersistentPreRunE below resolves precedence and
	// stamps FLEET_ENGINE so child subcommands (dispatch) see it.
	var (
		rootEngine string
		flagCodex  bool
		flagClaude bool
	)
	root := &cobra.Command{
		Use:   "fleet",
		Short: "Parallel Claude Code agent console",
		Long: `Fleet runs many Claude Code agents in parallel — one operator,
many concurrent agents across many repos, one TUI to keep them all
productive.

` + "`fleet`" + ` (no args) launches the interactive dashboard.
Subcommands below cover dispatch / attach / status from the shell.

Engine selection:
  fleet                      # default engine (claude-code)
  fleet -codex               # codex engine for this whole session
                             # (coord + workers + finisher all run codex;
                             #  reviewer subagent runs claude for second opinion)
  fleet -claude              # explicit claude-code (same as default)
  fleet --engine <name>      # spelled-out form`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       Version,
		// PersistentPreRunE resolves the engine flag precedence ONCE at
		// root and stamps FLEET_ENGINE so every subcommand (dispatch,
		// handoff, the TUI's startCoordSpawn) reads a single source of
		// truth. Precedence: -codex > -claude > --engine > existing env.
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			resolved, err := resolveEngineFlags(
				rootEngine, flagCodex, flagClaude,
				os.Getenv(FleetEngineEnv),
			)
			if err != nil {
				return err
			}
			return os.Setenv(FleetEngineEnv, resolved)
		},
		// `fleet` (no args) → launch the TUI. Auto-init runs BEFORE
		// tui.Run so any install output prints to the user's regular
		// terminal (bubbletea's altscreen would swallow it).
		RunE: func(cmd *cobra.Command, _ []string) error {
			maybeAutoInit(cmd.OutOrStdout(), "")
			return tui.Run(Version)
		},
	}

	// Persistent flags reach every subcommand. The shorthand bool flags
	// (-codex / -claude) are also exposed as full names so a user
	// hitting --help on a subcommand sees them documented.
	root.PersistentFlags().StringVar(&rootEngine, "engine", "",
		"engine for new agents spawned this session (claude-code | codex; default: claude-code)")
	root.PersistentFlags().BoolVar(&flagCodex, "codex", false,
		"shorthand for --engine codex (coord + workers + finisher run codex; reviewer runs claude)")
	root.PersistentFlags().BoolVar(&flagClaude, "claude", false,
		"shorthand for --engine claude-code (the default; explicit form)")

	root.AddCommand(newDispatchCmd())
	root.AddCommand(newAttachCmd())
	root.AddCommand(newStatusCmd())
	root.AddCommand(newHandoffCmd())
	root.AddCommand(newInitCmd())
	root.AddCommand(newDrainCmd())
	root.AddCommand(newRmCmd())
	root.AddCommand(newTasksCmd())
	root.AddCommand(newLearningsCmd())
	root.AddCommand(newStandardsCmd())
	root.AddCommand(newWorkersCmd())
	root.AddCommand(newPeekCmd())
	root.AddCommand(newProjectCmd())
	root.AddCommand(newMaintenanceCmd())
	root.AddCommand(newClaimsCmd())
	root.AddCommand(newRCCmd())
	root.AddCommand(newGCCmd())
	root.AddCommand(newCoordRunCmd())
	return root
}

// resolveEngineFlags merges the three root-flag inputs plus the
// inherited env var into a single engine name. Precedence (highest
// wins, falls through to lower on absence):
//
//  1. -codex   → "codex"
//  2. -claude  → "claude-code"
//  3. --engine <name> → that name (validated against enginecfg.Known)
//  4. existing FLEET_ENGINE env (e.g. set by an outer `fleet` parent
//     that this process was shelled out from)
//  5. enginecfg.DefaultEngine
//
// Conflicts: passing -codex AND -claude together returns an error
// (the operator's intent is ambiguous). Passing --engine alongside a
// matching shorthand is fine (idempotent); passing --engine with a
// conflicting shorthand returns an error.
//
// Unknown engine names are rejected here so a typo at the CLI fails
// fast rather than landing in agent records the dashboard can't
// render.
func resolveEngineFlags(
	engineFlag string, codexFlag, claudeFlag bool, envFleet string,
) (string, error) {
	if codexFlag && claudeFlag {
		return "", errors.New(
			"conflicting flags: -codex and -claude both set; pass exactly one")
	}
	var picked string
	switch {
	case codexFlag:
		picked = enginecfg.EngineCodex
	case claudeFlag:
		picked = enginecfg.EngineClaudeCode
	case engineFlag != "":
		picked = engineFlag
	case envFleet != "":
		picked = envFleet
	default:
		picked = enginecfg.DefaultEngine
	}
	// Disallow --engine codex with -claude (and vice versa).
	if engineFlag != "" && engineFlag != picked {
		return "", fmt.Errorf(
			"conflicting flags: --engine %q vs shorthand picked %q",
			engineFlag, picked)
	}
	if !enginecfg.Known(picked) {
		return "", fmt.Errorf(
			"unknown engine %q (known: claude-code, codex)", picked)
	}
	return picked, nil
}

// rewriteEngineShorthand turns `fleet -codex ...` into
// `fleet --codex ...` (and same for `-claude`) BEFORE cobra/pflag
// parses argv. pflag treats `-codex` as a cluster of single-character
// shorthand flags (`-c -o -d -e -x`), which the operator definitely
// didn't mean. The pre-processing happens at main() so every code
// path (`fleet -codex`, `fleet -codex dispatch ...`, `fleet -codex
// tasks list`) parses the long-form flag identically.
//
// Scope of the rewrite (codex review iter-1 P2 — don't mangle child
// argv):
//   - Only the exact tokens `-codex` and `-claude` are transformed.
//   - Stops rewriting after the POSIX end-of-options marker `--`. Anything
//     past `--` is positional data for the subcommand, not Fleet flags.
//   - Stops rewriting the IMMEDIATE next token after `--command`. The
//     `--command` flag's documented use is "pass arbitrary argv to the
//     spawned engine wrapper" (e.g. `--command sh --command -c
//     --command -codex` where `-codex` is a child-script argument, not
//     a Fleet engine selector). Without this gate, an operator's
//     custom wrapper that happens to use a `-codex` argument gets
//     silently mangled into `--codex`. The gate is per-token, not
//     "everything after the first --command", because StringSliceVar
//     accepts multiple `--command <value>` pairs and each value is a
//     single child-argv element.
//   - Other tokens (including `--codex` / `--claude` already in long
//     form, unrelated short flags like `-c`, `--codex-other`) pass
//     through unchanged.
func rewriteEngineShorthand(argv []string) []string {
	out := make([]string, len(argv))
	endOfOpts := false
	skipNext := false
	for i, a := range argv {
		if endOfOpts || skipNext {
			out[i] = a
			skipNext = false
			continue
		}
		if a == "--" {
			out[i] = a
			endOfOpts = true
			continue
		}
		if a == "--command" {
			// The next token is user data — don't rewrite it.
			out[i] = a
			skipNext = true
			continue
		}
		switch a {
		case "-codex":
			out[i] = "--codex"
		case "-claude":
			out[i] = "--claude"
		default:
			out[i] = a
		}
	}
	return out
}

func main() {
	// Translate `fleet -codex ...` / `fleet -claude ...` into the
	// pflag-friendly long form before cobra parses argv. See
	// rewriteEngineShorthand for rationale.
	if len(os.Args) > 1 {
		os.Args = append([]string{os.Args[0]}, rewriteEngineShorthand(os.Args[1:])...)
	}
	if err := newRootCmd().Execute(); err != nil {
		// `fleet claims` carries stable exit codes via an errClaimsError
		// sentinel that wraps the outcome string. Non-claims errors
		// keep the historical exit-1 + stderr-message behavior.
		if outcome := claimsOutcomeFromErr(err); outcome != "" {
			// The JSON envelope has already been written to stdout by
			// the claims subcommand; suppress the stderr "error: ..."
			// noise to keep the response shape clean for the Python
			// caller (json.loads on stdout).
			os.Exit(claimsExitCode(outcome))
		}
		if outcome := rcOutcomeFromErr(err); outcome != "" {
			// Same shape as claims: rc subcommands write the JSON
			// envelope to stdout and signal exit via the errRC
			// sentinel. Skill consumers parse stdout, not stderr.
			os.Exit(rcExitCode(outcome))
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
