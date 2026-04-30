// Command fleet is the operator-facing CLI for the Fleet parallel-agent
// console. See docs/DESIGN.md for the v1 product spec.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/edisonshen/fleet/internal/tui"
)

// Version is overwritten at release time via -ldflags. Default
// tracks the upcoming release tag so dev builds and the dashboard
// title row read consistently before the first tagged build.
var Version = "0.1.0"

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "fleet",
		Short: "Parallel Claude Code agent console",
		Long: `Fleet runs many Claude Code agents in parallel — one operator,
many concurrent agents across many repos, one TUI to keep them all
productive.

` + "`fleet`" + ` (no args) launches the interactive dashboard.
Subcommands below cover dispatch / attach / status from the shell.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       Version,
		// `fleet` (no args) → launch the TUI. Auto-init runs BEFORE
		// tui.Run so any install output prints to the user's regular
		// terminal (bubbletea's altscreen would swallow it).
		RunE: func(cmd *cobra.Command, _ []string) error {
			maybeAutoInit(cmd.OutOrStdout(), "")
			return tui.Run(Version)
		},
	}

	root.AddCommand(newDispatchCmd())
	root.AddCommand(newAttachCmd())
	root.AddCommand(newStatusCmd())
	root.AddCommand(newHandoffCmd())
	root.AddCommand(newInitCmd())
	root.AddCommand(newDrainCmd())
	root.AddCommand(newRmCmd())
	return root
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
