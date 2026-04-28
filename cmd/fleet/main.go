// Command fleet is the operator-facing CLI for the Fleet parallel-agent
// console. See docs/DESIGN.md for the v1 product spec.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
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

Pre-v0.1: ` + "`fleet`" + ` (no args) is the placeholder for the future TUI.
The CLI subcommands below are the Week 1 scaffold.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       Version,
		// No-op default action — when the TUI lands it'll go here.
		Run: func(_ *cobra.Command, _ []string) {
			fmt.Printf("fleet v%s\n", Version)
			fmt.Println("(TUI not yet implemented — try `fleet --help` for available commands)")
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
