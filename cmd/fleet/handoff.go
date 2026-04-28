package main

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/handoff"
	"github.com/edisonshen/fleet/internal/queue"
	"github.com/edisonshen/fleet/internal/spawn"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tmux"
)

// handoffOpts captures cobra-parsed flags + positional arg.
type handoffOpts struct {
	oldID       string
	cwd         string
	command     []string
	graceMillis int
}

func newHandoffCmd() *cobra.Command {
	opts := &handoffOpts{}
	cmd := &cobra.Command{
		Use:   "handoff <agent-id>",
		Short: "Hand off a running agent to a fresh replacement",
		Long: `handoff writes a handoff doc, spawns a fresh replacement
agent inheriting the same task/project, sends "/exit" to the outgoing
tmux session, waits a grace period, then kills the old session and
archives its record.

Week 4a (operator-triggered): the doc body is a stub with placeholders
in all five sections — the agent never received a HANDOFF REQUESTED
injection so we can't fill its view of the work. After ` + "`fleet attach`" + `
to the new agent, paste the handoff doc path and ask it to read+continue.

The new agent inherits TaskID, Project, Engine, Role, Mode from the
outgoing record and increments handoff_number by 1.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.oldID = args[0]
			return runHandoff(opts, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().StringVar(&opts.cwd, "cwd", "",
		"working directory for the replacement session (default: current dir)")
	cmd.Flags().StringSliceVar(&opts.command, "command", []string{"claude"},
		"command to run inside the replacement tmux session (default: claude)")
	cmd.Flags().IntVar(&opts.graceMillis, "grace-ms", 3000,
		"milliseconds to wait between /exit and kill on the old session")
	return cmd
}

// runHandoff orchestrates the full B2 flow:
//
//	load → flock(project) → reload-under-lock → write doc
//	→ write queue → spawn replacement → send /exit + grace
//	→ kill → archive → delete queue
//
// Concurrency: the per-project flock bounds the entire critical
// section. Two concurrent `fleet handoff X` invocations both reach
// step 1 (load), serialize on step 2 (LockProject), and the loser's
// step 3 (reload-under-lock) sees ErrNotFound (winner already
// archived) and bails — no double-spawn.
//
// Crash safety: the queue file (step 5) is the journal entry. If we
// crash between step 5 and step 11 (Delete), a future drainer (4b
// TUI background loop) sees a stale queue file pointing at an
// already-archived old_agent_id and skips it. In 4a there is no
// drainer; the file lingers as harmless residue until 4b ships.
func runHandoff(opts *handoffOpts, stdout, stderr io.Writer) error {
	if _, err := state.Bootstrap(); err != nil {
		return fmt.Errorf("bootstrap ~/.fleet: %w", err)
	}
	if err := tmux.Available(); err != nil {
		return err
	}
	if len(opts.command) == 0 {
		return errors.New("--command must not be empty")
	}

	// 1. Load the outgoing agent record (used only to pick the lock
	//    project — re-loaded under the flock below to detect a
	//    concurrent handoff that already archived the agent).
	oldRec, err := agent.Load(opts.oldID)
	if err != nil {
		return fmt.Errorf("load agent %s: %w", opts.oldID, err)
	}

	// 2. Per-project flock — acquired BEFORE any side effects so
	//    concurrent `fleet handoff X` invocations serialize from the
	//    earliest possible point. Without this, two callers race the
	//    queue write + spawn and both spawn replacements (only one
	//    gets to archive the old record; the other's archive fails
	//    after spawning). project may be "" for legacy records.
	lockProject := oldRec.Project
	if lockProject == "" {
		lockProject = "default"
	}
	release, err := state.LockProject(lockProject)
	if err != nil {
		return fmt.Errorf("lock project %s: %w", lockProject, err)
	}
	defer release()

	// 3. Re-load under the flock. If a concurrent handoff already
	//    archived this record, agent.Load returns ErrNotFound and we
	//    bail without double-spawning. The first-loaded oldRec is
	//    discarded — the under-flock copy is the source of truth.
	oldRec, err = agent.Load(opts.oldID)
	if err != nil {
		return fmt.Errorf("load agent %s under lock: %w (concurrent handoff?)", opts.oldID, err)
	}

	// 4. Write the handoff doc. The doc represents "agent oldID
	//    handed off"; its handoff_number is the OLD agent's number,
	//    its previous_handoff chains back to whatever the old agent
	//    itself inherited from. The next handoff increments by 1
	//    inside spawn.Spawn.
	now := time.Now().UTC()
	docPath, err := state.HandoffPath(oldRec.ID, now)
	if err != nil {
		return err
	}
	doc := handoff.NewManualStub(
		oldRec.ID, oldRec.TaskID, oldRec.Project,
		oldRec.HandoffNumber, oldRec.LastHandoffPath, now,
	)
	if err := handoff.Write(doc, docPath); err != nil {
		return fmt.Errorf("write handoff doc: %w", err)
	}

	// 5. Write the queue file. With flock acquired, this is no longer
	//    a race-prone commit point but a journal entry — if we crash
	//    before queue.Delete (step 10), a future drainer (4b TUI
	//    background loop) sees a stale queue file pointing at an
	//    archived agent and skips it. In 4a there's no drainer; the
	//    file lingers as harmless residue until 4b ships.
	queuePath, err := queue.WriteSpawnFresh(queue.SpawnFresh{
		OldAgentID: oldRec.ID,
		HandoffDoc: docPath,
		Project:    oldRec.Project,
		TaskID:     oldRec.TaskID,
	})
	if err != nil {
		return fmt.Errorf("enqueue spawn-fresh: %w", err)
	}

	// 6. Drain: spawn the replacement.
	newRec, err := spawn.Spawn(spawn.Options{
		OldRecord:  oldRec,
		NewDocPath: docPath,
		Cwd:        opts.cwd,
		Command:    opts.command,
	})
	if err != nil {
		// Spawn failed — leave the queue file in place so a later
		// drain can retry. The old agent is still alive and untouched.
		return fmt.Errorf("spawn replacement: %w", err)
	}

	// 7. Send "/exit" to the old session. ErrNoSession means it
	//    already died (operator killed manually, crash, etc.) — fine,
	//    fall through to Kill which is also idempotent.
	if err := tmux.SendKeys(oldRec.TmuxSession, "/exit", "Enter"); err != nil &&
		!errors.Is(err, tmux.ErrNoSession) {
		// Treat anything else as a warning, not a fatal — we still
		// want to archive and clean up.
		_, _ = fmt.Fprintf(stderr, "warning: send-keys to %s: %v\n", oldRec.TmuxSession, err)
	}

	// 8. Grace window so Claude can flush its own state on /exit.
	if opts.graceMillis > 0 {
		time.Sleep(time.Duration(opts.graceMillis) * time.Millisecond)
	}

	// 9. Kill is idempotent. Either /exit already wound the session
	//    down (no-op) or we forcibly close it now.
	if err := tmux.Kill(oldRec.TmuxSession); err != nil {
		_, _ = fmt.Fprintf(stderr, "warning: kill %s: %v\n", oldRec.TmuxSession, err)
	}

	// 10. Archive the old record. After this, `fleet status` no longer
	//     shows the outgoing agent.
	if err := oldRec.Archive(); err != nil {
		// Best-effort — the operator can clean up agents/<id>.json by
		// hand if this fails. The replacement is up and registered.
		_, _ = fmt.Fprintf(stderr, "warning: archive %s: %v\n", oldRec.ID, err)
	}

	// 11. Delete the queue file. Work is durable on disk; the journal
	//     entry is no longer needed.
	if err := queue.Delete(queuePath); err != nil {
		_, _ = fmt.Fprintf(stderr, "warning: delete queue file: %v\n", err)
	}

	_, _ = fmt.Fprintf(stdout, "agent %s handed off → %s\n", oldRec.ID, newRec.ID)
	_, _ = fmt.Fprintf(stdout, "  task:    %s\n", newRec.TaskID)
	_, _ = fmt.Fprintf(stdout, "  project: %s\n", newRec.Project)
	_, _ = fmt.Fprintf(stdout, "  tmux:    %s\n", newRec.TmuxSession)
	_, _ = fmt.Fprintf(stdout, "  handoff: %s\n", docPath)
	_, _ = fmt.Fprintf(stdout, "  number:  %d (was %d)\n", newRec.HandoffNumber, oldRec.HandoffNumber)
	_, _ = fmt.Fprintf(stdout, "\nattach with: fleet attach %s\n", newRec.ID)
	_, _ = fmt.Fprintf(stdout, "then say: read the handoff doc at %s and continue\n", docPath)
	return nil
}
