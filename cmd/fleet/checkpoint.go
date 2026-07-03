package main

// fleet checkpoint {doc,decision} — the coordinator's provenance writer
// for the curated-handoff sections. Each subcommand takes coordinator.lock
// and read-modify-writes ~/.fleet/projects/<project>/coord-state.json
// atomically, so a doc/decision the agent records while alive survives into
// its handoff doc (Docs (this session) + Key Decisions) even when the coord
// later dies. TDD-RED STUB — implemented in tdd-green.

import (
	"errors"

	"github.com/spf13/cobra"
)

// errCheckpointNotImplemented is the tdd-red stub sentinel.
var errCheckpointNotImplemented = errors.New("fleet checkpoint: not implemented")

func newCheckpointCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "checkpoint",
		Short: "Record coordinator provenance (session docs / decisions) into coord-state.json",
	}
	cmd.AddCommand(newCheckpointDocCmd())
	cmd.AddCommand(newCheckpointDecisionCmd())
	return cmd
}

func newCheckpointDocCmd() *cobra.Command {
	return &cobra.Command{
		Use: "doc",
		RunE: func(cmd *cobra.Command, args []string) error {
			return errCheckpointNotImplemented
		},
	}
}

func newCheckpointDecisionCmd() *cobra.Command {
	return &cobra.Command{
		Use: "decision",
		RunE: func(cmd *cobra.Command, args []string) error {
			return errCheckpointNotImplemented
		},
	}
}
