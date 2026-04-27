package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tmux"
)

func newAttachCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "attach <agent-id>",
		Short: "Attach to a running agent's tmux session",
		Long: `attach replaces the fleet process with ` + "`tmux attach`" + ` against
the agent's session. On exit (Ctrl-b d to detach, or exit inside the
session) you return to your shell, not to fleet.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			id := args[0]
			rec, err := agent.Load(id)
			if err != nil {
				if errors.Is(err, state.ErrNotFound) {
					return fmt.Errorf("no agent record for %q (try `fleet status` to list)", id)
				}
				return err
			}
			session := rec.TmuxSession
			if session == "" {
				session = tmux.SessionName(id)
			}
			// Replaces the current process. Returns only on error.
			return tmux.Attach(session)
		},
	}
}
