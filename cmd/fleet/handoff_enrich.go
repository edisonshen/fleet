package main

// handoff_enrich.go — `fleet handoff-enrich <doc-path>`. Hidden helper the
// fleet-guard auto-handoff shells out to right after it renders a coord
// handoff doc and before it enqueues the spawn-fresh file: fills the
// narrative sections (Completed / Key Decisions / Docs / Open Questions /
// Next Steps) from coord-state.json, tasks.md and the rolling checkpoint —
// the same durable sources `fleet handoff <id>` reads — so the successor
// gets the same brief on an automatic handoff as on a manual one.
//
// Best-effort by contract: exit 0 whether or not anything was added.
// Non-zero only for a usage / read / write fault; the Python caller treats
// any failure as "leave the doc as rendered".

import (
	"fmt"
	"io"
	"os"

	"github.com/edisonshen/fleet/internal/handoff"
	"github.com/edisonshen/fleet/internal/spawn"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/spf13/cobra"
)

func newHandoffEnrichCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "handoff-enrich <doc-path>",
		Short:  "Fill a rendered coord handoff doc's narrative sections from durable state",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runHandoffEnrich(args[0], cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	return cmd
}

func runHandoffEnrich(docPath string, stdout, stderr io.Writer) error {
	raw, err := os.ReadFile(docPath)
	if err != nil {
		return fmt.Errorf("handoff-enrich: read %s: %w", docPath, err)
	}
	fm, err := handoff.ParseFrontmatter(raw)
	if err != nil {
		return fmt.Errorf("handoff-enrich: %w", err)
	}
	// Coord docs only — the narrative collectors read project-wide coord
	// state, which has no business in a worker's handoff doc.
	if !spawn.IsCoordSpawn(fm.TaskID, fm.Project) {
		_, _ = fmt.Fprintf(stdout, "handoff-enrich: %s is not a coord handoff; nothing to do\n", docPath)
		return nil
	}
	out, changed := handoff.EnrichRenderedDoc(raw, fm.Project, fm.AgentID, fm.PreviousHandoff,
		func(msg string) { _, _ = fmt.Fprintln(stderr, msg) })
	if !changed {
		_, _ = fmt.Fprintf(stdout, "handoff-enrich: no durable narrative state for %s; doc unchanged\n", fm.AgentID)
		return nil
	}
	if err := state.WriteAtomic(docPath, out); err != nil {
		return fmt.Errorf("handoff-enrich: write %s: %w", docPath, err)
	}
	_, _ = fmt.Fprintf(stdout, "handoff-enrich: filled narrative sections of %s\n", docPath)
	return nil
}
