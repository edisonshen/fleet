// Command fleet is the operator-facing CLI for the Fleet parallel-agent
// console. See docs/DESIGN.md for the v1 product spec.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/edisonshen/fleet/internal/tui"
)

// Version is overwritten at release time via -ldflags.
var Version = "0.0.0"

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
		// `fleet` (no args) → launch the TUI.
		RunE: func(_ *cobra.Command, _ []string) error {
			return tui.Run(Version)
		},
	}

	root.AddCommand(newDispatchCmd())
	root.AddCommand(newAttachCmd())
	root.AddCommand(newStatusCmd())
	return root
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
