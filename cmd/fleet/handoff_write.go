package main

// handoff_write.go — the single producer of handoff docs + spawn-fresh
// queue files, shared by the operator path (`fleet handoff <id>`, TUI
// `[h]`) and the fleet-guard auto path (`fleet handoff-write`).
//
//	fleet handoff <id>          fleet-guard (auto-yellow / auto-red / precompact)
//	   │                            │  tmux capture-pane → stdin
//	   │                            ▼
//	   │                        fleet handoff-write --agent <id> --type <t> [--context-pct]
//	   │                            │
//	   └──────► writeHandoffDoc(rec, type, pct, recent, repoDir, now) ◄──────┘
//	                 NewStub → EnrichManualDoc (coord only) → AppendRecentActivity → Write
//	                            │
//	            (manual: spawn inline)   (auto: queue.WriteSpawnFresh → drain later)
//
// fleet-guard used to render its own doc in Python and collect Active
// Subagents / Open PRs itself; the two renderers drifted (auto docs
// shipped placeholders for every narrative section) and that is exactly
// the "successor coord has nothing to continue" failure. Now there is one
// renderer and one set of collectors — Go's.

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/coordrepo"
	"github.com/edisonshen/fleet/internal/handoff"
	"github.com/edisonshen/fleet/internal/queue"
	"github.com/edisonshen/fleet/internal/spawn"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tmux"
	"github.com/spf13/cobra"
)

// autoHandoffTypes are the handoff_type values fleet-guard may write.
// TypeManual is reserved for `fleet handoff`.
var autoHandoffTypes = map[string]bool{
	handoff.TypeAutoYellow: true,
	handoff.TypeAutoRed:    true,
	handoff.TypePreCompact: true,
}

// writeHandoffDoc builds, enriches and atomically writes the handoff doc
// for rec, returning its path. recent is the outgoing agent's pane
// capture ("" on the operator path). repoDir binds `gh pr list` to the
// handed-off coord's checkout.
//
// COORD ONLY enrichment: Active Subagents / Open PRs / Key Decisions /
// Docs / Open Questions / Next Steps are coord-owned project state. A
// worker handoff has none, and pulling the live coord's coord-state.json
// into a worker's doc would resume the worker with unrelated
// project-wide state. Enrichment is best-effort and never fails the
// handoff — the worst case is a section left at its placeholder.
func writeHandoffDoc(rec *agent.Record, typ string, contextPct *float64, recent, repoDir string,
	now time.Time, stderr io.Writer) (string, error) {
	docPath, err := state.HandoffPath(rec.ID, now)
	if err != nil {
		return "", err
	}
	doc := handoff.NewStub(typ, rec.ID, rec.TaskID, rec.Project,
		rec.HandoffNumber, rec.LastHandoffPath, contextPct, now)
	if spawn.IsCoordSpawn(rec.TaskID, rec.Project) {
		handoff.EnrichManualDoc(doc, rec.Project, rec.ID, repoDir, rec.LastHandoffPath,
			func(msg string) { _, _ = fmt.Fprintln(stderr, msg) })
	}
	handoff.AppendRecentActivity(doc, recent)
	if err := handoff.Write(doc, docPath); err != nil {
		return "", fmt.Errorf("write handoff doc: %w", err)
	}
	return docPath, nil
}

type handoffWriteOpts struct {
	agentID    string
	typ        string
	contextPct string
}

// handoffWriteResult is the JSON `fleet handoff-write` prints on stdout.
type handoffWriteResult struct {
	DocPath    string `json:"doc_path"`
	QueuePath  string `json:"queue_path"`
	NewAgentID string `json:"new_agent_id"`
}

func newHandoffWriteCmd() *cobra.Command {
	opts := &handoffWriteOpts{}
	cmd := &cobra.Command{
		Use:    "handoff-write",
		Short:  "Write an auto-handoff doc + spawn-fresh queue file for an agent (fleet-guard producer)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runHandoffWrite(opts, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().StringVar(&opts.agentID, "agent", "", "outgoing agent ID (required)")
	cmd.Flags().StringVar(&opts.typ, "type", "", "handoff_type: auto-yellow | auto-red | precompact (required)")
	cmd.Flags().StringVar(&opts.contextPct, "context-pct", "", "context window percentage at handoff (omit when unknown)")
	_ = cmd.MarkFlagRequired("agent")
	_ = cmd.MarkFlagRequired("type")
	return cmd
}

// runHandoffWrite is the fleet-guard producer: read the pane capture from
// stdin, write the doc, then the queue file. The queue file is the
// durable commit point — it lands only after the doc is complete on disk,
// so `fleet drain` never spawns a successor against a half-written brief.
// Fencing (lease-check) and storm back-off stay in the caller: they are
// hook-lifecycle decisions, not document ones.
func runHandoffWrite(opts *handoffWriteOpts, stdin io.Reader, stdout, stderr io.Writer) error {
	if !autoHandoffTypes[opts.typ] {
		return fmt.Errorf("handoff-write: --type must be one of auto-yellow, auto-red, precompact (got %q)", opts.typ)
	}
	var contextPct *float64
	if opts.contextPct != "" {
		v, err := strconv.ParseFloat(opts.contextPct, 64)
		if err != nil {
			return fmt.Errorf("handoff-write: --context-pct %q: %w", opts.contextPct, err)
		}
		contextPct = &v
	}
	if _, err := state.Bootstrap(); err != nil {
		return fmt.Errorf("handoff-write: %w", err)
	}
	rec, err := agent.Load(opts.agentID)
	if err != nil {
		return fmt.Errorf("handoff-write: load agent %s: %w", opts.agentID, err)
	}
	recentRaw, err := io.ReadAll(stdin)
	if err != nil {
		return fmt.Errorf("handoff-write: read pane capture from stdin: %w", err)
	}

	// Repo binding for `gh pr list`: the shared resolver for coords (same
	// as `fleet handoff`), the stored cwd for workers. Unlike the operator
	// path this is best-effort — the auto path only needs the repo for the
	// Open PRs query; the successor's cwd is resolved by drain at spawn.
	repoDir := rec.Cwd
	if spawn.IsCoordSpawn(rec.TaskID, rec.Project) {
		if resolved, rerr := coordrepo.ResolveProjectRepo(rec.Project, false); rerr == nil {
			repoDir = resolved
		} else {
			_, _ = fmt.Fprintf(stderr, "handoff-write: resolve repo for project %q: %v; using stored cwd %q\n",
				rec.Project, rerr, rec.Cwd)
		}
	}

	now := time.Now().UTC()
	docPath, err := writeHandoffDoc(rec, opts.typ, contextPct, string(recentRaw), repoDir, now, stderr)
	if err != nil {
		return fmt.Errorf("handoff-write: %w", err)
	}

	// Pre-allocated successor ID + session, as `fleet handoff` does, so the
	// drain-side recovery probe can detect a crashed-mid-spawn replacement
	// on retry. No DisableAutoResume override (inherit from rec) and no
	// CapApproved: the auto producer never checked FLEET_MAX_SESSIONS.
	newID := agent.NewID()
	queuePath, err := queue.WriteSpawnFresh(queue.SpawnFresh{
		OldAgentID: rec.ID,
		HandoffDoc: docPath,
		Project:    rec.Project,
		TaskID:     rec.TaskID,
		NewAgentID: newID,
		NewSession: tmux.SessionName(newID),
		EnqueuedAt: now,
	})
	if err != nil {
		// Doc is on disk but unreferenced; the caller's retry writes a
		// fresh pair. Leave it for forensics rather than racing a delete.
		return fmt.Errorf("handoff-write: enqueue spawn-fresh: %w", err)
	}

	out, err := json.Marshal(handoffWriteResult{DocPath: docPath, QueuePath: queuePath, NewAgentID: newID})
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(stdout, string(out))
	return nil
}
