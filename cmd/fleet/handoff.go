package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/coordrepo"
	"github.com/edisonshen/fleet/internal/handoff"
	"github.com/edisonshen/fleet/internal/handoffop"
	"github.com/edisonshen/fleet/internal/queue"
	"github.com/edisonshen/fleet/internal/spawn"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tmux"
)

// handoffSessionPrefix is the literal session-name-prefix that
// internal/handoff.FirstAction's bash bootstrap passes to the
// `claude remote-control` daemon
// (`--remote-control-session-name-prefix "fleet-handoff"`). The
// handoff replacement spawn injects `--remote-control
// "fleet-handoff-<new-id>"` so the freshly-spawned claude latches
// onto that daemon at startup, skipping the round-trip through
// FirstAction's manual `/remote-control` slash command.
//
// Why we still keep FirstAction's bash + slash-command instructions
// in the handoff doc: belt-and-braces. The bash block bootstraps
// the daemon if it isn't already running (handoff replacement is
// often the FIRST process on the host to need that daemon) and the
// slash command is a recovery path if for any reason the spawn-time
// flag didn't latch. Both paths converge on the same daemon prefix.
//
// Keep byte-identical with the literal in
// internal/handoff/handoff.go FirstAction's bash block — drift would
// silently regress mobile pairing on handoff. A regression test in
// the same package pins the equality.
const handoffSessionPrefix = "fleet-handoff"

// handoffOpts captures cobra-parsed flags + positional arg.
type handoffOpts struct {
	oldID                string
	cwd                  string
	command              []string
	graceMillis          int
	noAutoResume         bool
	autoResume           bool
	autoResumeFlagWasSet bool // true when operator explicitly passed --no-auto-resume OR --auto-resume
	// forceReplacement bypasses the FLEET_MAX_SESSIONS spawn-time cap
	// (see cmd/fleet/session_cap.go). Operator escape hatch: when
	// they're already doing the cleanup and need the successor up
	// first, the cap would otherwise block the very command meant to
	// make room. The bypass is logged to stderr so it shows in the
	// session transcript.
	forceReplacement bool
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
injection so we can't fill its view of the work. Once the old session
is killed, fleet auto-types a "Read your handoff doc at <path> and
continue" prompt into the new session so the replacement starts
working without operator intervention; ` + "`fleet attach`" + ` shows
the result.

The new agent inherits TaskID, Project, Engine, Role, Mode from the
outgoing record and increments handoff_number by 1.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.oldID = args[0]
			// Treat `--flag=false` (explicit "I don't want this
			// override") the same as "flag not passed" — both mean
			// inherit. Otherwise wrapper scripts that always render
			// boolean flags accidentally trigger overrides (codex
			// review iter-12 P3).
			noChanged := cmd.Flags().Changed("no-auto-resume") && opts.noAutoResume
			yesChanged := cmd.Flags().Changed("auto-resume") && opts.autoResume
			if noChanged && yesChanged {
				return fmt.Errorf("--no-auto-resume and --auto-resume are mutually exclusive")
			}
			opts.autoResumeFlagWasSet = noChanged || yesChanged
			return runHandoff(opts, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().StringVar(&opts.cwd, "cwd", "",
		"working directory for the replacement (default: outgoing agent's cwd)")
	cmd.Flags().StringSliceVar(&opts.command, "command", nil,
		"command to run inside the replacement tmux session (default: outgoing agent's command, or `claude`)")
	cmd.Flags().IntVar(&opts.graceMillis, "grace-ms", 3000,
		"milliseconds to wait between /exit and kill on the old session")
	// Override the auto-resume policy inherited from the outgoing
	// record (codex review iter-8 / 11 / 13 P2). Three states:
	//   - neither flag → inherit oldRec.DisableAutoResume; the new
	//     record's baseline matches.
	//   - --no-auto-resume → force OFF for this handoff AND persist
	//     OFF as the new record's baseline. Future handoffs of the
	//     replacement inherit OFF until --auto-resume is passed.
	//   - --auto-resume → force ON and persist ON, similarly.
	// Persisting (sticky) is the right default when the operator
	// switches command class via `--command` (e.g. claude → bash),
	// because the new agent IS the new command class and its
	// future handoffs should follow the new policy. Operators who
	// truly want a one-shot must re-pass the inverse flag next time.
	// Mutually exclusive — enforced in RunE above.
	cmd.Flags().BoolVar(&opts.noAutoResume, "no-auto-resume", false,
		"skip auto-typing the resume prompt; persists on the replacement record so future handoffs inherit OFF")
	cmd.Flags().BoolVar(&opts.autoResume, "auto-resume", false,
		"force auto-typing the resume prompt; persists on the replacement record so future handoffs inherit ON")
	// --force-replacement bypasses the FLEET_MAX_SESSIONS spawn-time
	// cap (see cmd/fleet/session_cap.go). The cap exists to refuse
	// runaway accumulation, but when the operator is ALREADY doing
	// the cleanup (e.g., handing off a stuck coord to free up room),
	// blocking the very command meant to make room would be the
	// wrong call. The bypass is logged to stderr so it appears in
	// transcripts.
	cmd.Flags().BoolVar(&opts.forceReplacement, "force-replacement", false,
		"bypass the FLEET_MAX_SESSIONS spawn-time cap (use when handing off as part of an operator cleanup)")
	return cmd
}

// runHandoff orchestrates the full B2 flow:
//
//	load → flock(agent) → reload-under-lock → write doc
//	→ write queue → spawn replacement → send /exit + grace
//	→ kill → archive → delete queue
//
// Concurrency: the per-agent flock bounds the entire critical
// section. Two concurrent `fleet handoff X` invocations serialize on
// step 2 (LockAgent), and the loser's step 3 (reload-under-lock)
// sees ErrNotFound (winner already archived) and bails — no
// double-spawn. Per-agent (not per-project) so concurrent handoffs
// on DIFFERENT agents in the same project run in parallel.
//
// isCoordHandoffForProject reports whether the agent identified by
// (project, agentID) IS the project's current coord — i.e. the
// coord-spawn marker for project resolves to agentID. Used by
// runHandoff / resumeHandoff to gate the v0.12.1 rc-enabled auto-
// write (DESIGN-rc-coord-auto-marker.md, codex review iter-1 [P1]):
// only coord handoffs auto-opt the project into RC; worker handoffs
// preserve the strict opt-in carve-out that protects against the
// push-storm class of failures.
//
// Empty project is "not a coord handoff" by definition — mirrors
// the defensive shape used by rc.Enabled and the existing
// isCoordSwap detection in runHandoff at the post-spawn step.
func isCoordHandoffForProject(project, agentID string) bool {
	if project == "" {
		return false
	}
	return state.ReadCoordSpawnMarker(project) == agentID
}

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

	// 1. Per-agent flock — acquired BEFORE any side effects so
	//    concurrent `fleet handoff X` invocations serialize from the
	//    earliest possible point. Per-agent (not per-project) so two
	//    handoffs on DIFFERENT agents in the same project run in
	//    parallel. Without this, two same-id callers would race the
	//    queue write + spawn and both spawn replacements.
	//
	//    LockAgent uses only opts.oldID (no project lookup needed),
	//    so we don't pre-Load — the agent might not exist yet, in
	//    which case the next step surfaces a clear error.
	release, err := state.LockAgent(opts.oldID)
	if err != nil {
		return fmt.Errorf("lock agent %s: %w", opts.oldID, err)
	}
	defer release()

	// 2. Crash-recovery probe: if a previous handoff for this agent
	//    crashed AFTER spawning the replacement but BEFORE deleting
	//    the queue file, the queue file's NewAgentID will be set.
	//    Resume by completing kill+archive+delete with the existing
	//    replacement instead of spawning a second one.
	//
	//    Distinguish state.ErrNotFound (the actual recovery case)
	//    from other Load errors (corrupted JSON, permission issue) —
	//    silently cleaning up the journal on a corrupted-record read
	//    would either mask broken state or trigger a double-spawn.
	pendingPath, _ := state.QueuePath(queue.SpawnFreshName(opts.oldID))
	pending, perr := queue.ReadSpawnFresh(pendingPath)
	switch {
	case errors.Is(perr, state.ErrNotFound):
		// No queue file — normal flow.
	case perr != nil:
		// Corrupted journal / newer schema / permission error. Don't
		// silently fall through to the spawn path — that's a
		// double-spawn risk. Abort so the operator can investigate.
		return fmt.Errorf("recovery probe: read queue file %s failed: %w", pendingPath, perr)
	case pending.NewAgentID != "":
		// Codex iter-18 [P1] (deferred — known recovery gap): if OLD
		// is gone because the operator manually ran `fleet rm <OLD>`
		// (e.g., per the instruction in *ErrOrphanSurvived /
		// *ErrOldKillProbeAmbiguous) AND NEW has since exited, this
		// branch surfaces "task has no live agent" instead of
		// respawning. Auto-respawn would need a fresh agent ID +
		// reconstructed spawn.Options (the queue file's NewAgentID is
		// already consumed and NewRecSpec is not carried verbatim).
		// Operator workaround: `fleet dispatch --resume <handoff-doc>`
		// after the queue file is cleaned up. Deferred to a follow-up
		// PR alongside the "swap-committed sentinel" structural fix.
		oldRec, lerr := agent.Load(opts.oldID)
		switch {
		case errors.Is(lerr, state.ErrNotFound):
			// Old record gone. Verify the replacement is actually
			// alive before declaring "already handed off"; if the
			// replacement also vanished, the task currently has NO
			// live agent and we must not silently exit success.
			newRec, nerr := agent.Load(pending.NewAgentID)
			switch {
			case errors.Is(nerr, state.ErrNotFound):
				return fmt.Errorf(
					"agent %s already archived BUT replacement %s record is gone — task has no live agent; clean up queue file %s manually after starting a new agent",
					opts.oldID, pending.NewAgentID, pendingPath)
			case nerr != nil:
				return fmt.Errorf("recovery probe: load replacement %s failed: %w", pending.NewAgentID, nerr)
			}
			if !tmux.HasSession(newRec.TmuxSession) {
				return fmt.Errorf(
					"agent %s already archived BUT replacement %s tmux session %s is gone — task has no live agent; investigate before deleting queue file %s",
					opts.oldID, pending.NewAgentID, newRec.TmuxSession, pendingPath)
			}
			// Old archived, replacement live. The previous run
			// reached at least Archive but may have crashed before
			// SendPromptKeys (codex review iter-10 P1). If the queue
			// file is still here, queue.Delete didn't run either,
			// which means SendPromptKeys also didn't (it lives
			// AFTER queue.Delete in runHandoff). Send the prompt
			// now to cover that gap, then delete the queue.
			//
			// Resolve auto-resume from queue override + newRec
			// baseline (codex review iter-12 P2): the original
			// handoff may have specified --no-auto-resume /
			// --auto-resume.
			//
			// Gate on schema v2+ (codex review iter-15 P2): older
			// queue files predate auto-resume entirely, so an
			// unconditional send here would inject a prompt into
			// a replacement the operator already kicked off
			// manually back when those v1 files were written.
			disableAutoResume := newRec.DisableAutoResume
			if pending.DisableAutoResume != nil {
				disableAutoResume = *pending.DisableAutoResume
			}
			autoResume := !disableAutoResume && pending.SchemaVersion >= 2
			// Wait + liveness check ALWAYS run; the readiness wait
			// doubles as a post-spawn liveness probe that catches
			// wrappers crashing shortly after step 8a's check
			// (codex review iter-16 P1). Only the SEND is gated on
			// autoResume below.
			if err := spawn.WaitForReadyToPrompt(newRec.TmuxSession); err != nil {
				_, _ = fmt.Fprintf(stderr,
					"warning: readiness poll for %s did not converge: %v (proceeding anyway)\n",
					newRec.TmuxSession, err)
			}
			if alive, perr := tmux.SessionAlive(newRec.TmuxSession); perr != nil {
				_, _ = fmt.Fprintf(stderr,
					"warning: post-readiness probe for %s failed: %v (proceeding anyway)\n",
					newRec.TmuxSession, perr)
			} else if !alive {
				return fmt.Errorf(
					"agent %s already archived BUT replacement %s tmux session %s exited during readiness wait — task has no live agent",
					opts.oldID, pending.NewAgentID, newRec.TmuxSession)
			}
			queueDeleted := true
			if err := queue.Delete(pendingPath); err != nil {
				_, _ = fmt.Fprintf(stderr, "warning: delete queue file: %v\n", err)
				queueDeleted = false
			}
			if queueDeleted && autoResume {
				if err := spawn.SendPromptKeys(newRec.TmuxSession,
					handoff.ResumePrompt(pending.HandoffDoc)); err != nil {
					_, _ = fmt.Fprintf(stderr,
						"warning: send resume prompt to %s after archive-recovery: %v (re-enqueuing for retry)\n",
						newRec.TmuxSession, err)
					// Re-enqueue so the next retry can attempt
					// delivery (codex review iter-14 P1).
					if _, werr := queue.WriteSpawnFresh(pending); werr != nil {
						_, _ = fmt.Fprintf(stderr,
							"warning: re-enqueue after archive-recovery send failure: %v\n",
							werr)
					}
				}
			}
			if !queueDeleted {
				// queue.Delete failed → SendPromptKeys was skipped,
				// so the replacement is still un-prompted and the
				// journal entry persists. Surface as error so the
				// operator retries (codex review iter-19 P2).
				return fmt.Errorf(
					"agent %s already archived BUT queue cleanup failed; rerun `fleet handoff %s` to deliver the resume prompt",
					opts.oldID, opts.oldID)
			}
			_, _ = fmt.Fprintf(stdout,
				"agent %s already handed off → %s (cleaned stale queue file)\n",
				opts.oldID, pending.NewAgentID)
			if !autoResume {
				// The original handoff opted out of auto-resume, so
				// the replacement is alive but still idle. Print the
				// manual prompt instruction so the operator knows
				// what to type on attach (codex review iter-11 P2).
				_, _ = fmt.Fprintf(stdout,
					"then say: read the handoff doc at %s and continue\n",
					pending.HandoffDoc)
			}
			return nil
		case lerr != nil:
			// Read failure that isn't "missing" — abort so the
			// operator can investigate without us touching state.
			return fmt.Errorf("recovery probe: load agent %s failed: %w", opts.oldID, lerr)
		}
		newRec, nerr := agent.Load(pending.NewAgentID)
		switch {
		case errors.Is(nerr, state.ErrNotFound):
			// Replacement record vanished. Before re-spawning, check
			// whether the previously-recorded session is still alive
			// — if it is, the record was hand-deleted (or some other
			// out-of-band cleanup) and a fresh spawn would create a
			// duplicate live session. Refuse and let the operator
			// reconcile.
			if pending.NewSession != "" && tmux.HasSession(pending.NewSession) {
				return fmt.Errorf(
					"recovery probe: replacement %s record missing but tmux session %s still alive — refusing to spawn duplicate; clean up the orphan session or its queue file before retrying",
					pending.NewAgentID, pending.NewSession)
			}
			// Both record and session gone — true orphan journal.
			// Delete and proceed normally so a fresh spawn can happen.
			// Preserve the persisted auto-resume override into opts
			// so the fall-through respects the original handoff's
			// policy (codex review iter-17 P1) — otherwise an opt-out
			// for a non-claude wrapper would silently re-enable on
			// the retry.
			if pending.DisableAutoResume != nil {
				opts.noAutoResume = *pending.DisableAutoResume
				opts.autoResume = !*pending.DisableAutoResume
				opts.autoResumeFlagWasSet = true
			}
			// Cap re-checked on retry (codex iter-8 P1): we do NOT
			// auto-bypass the cap here even when pending.CapApproved
			// is true. The cap math is state-dependent (post-crash
			// the session list and the old-alive bit may differ
			// from the original handoff's view), so the swap-aware
			// gate at step 4a must re-run. If the cap legitimately
			// refuses, the operator can re-issue with
			// `--force-replacement` knowing the current state.
			//
			// Cap check runs INSIDE this recovery branch BEFORE
			// queue.Delete (codex iter-10 P2) so a cap refusal
			// preserves the per-handoff overrides on disk for the
			// next retry. Without this, a refused recovery would
			// have already destroyed pending.DisableAutoResume.
			capBypass := SessionCapBypassReason("")
			if opts.forceReplacement {
				capBypass = SessionCapBypassForceReplacement
			}
			if capErr := enforceSessionCap(stderr, capBypass, 1); capErr != nil {
				return capErr
			}
			_ = queue.Delete(pendingPath)
		case nerr != nil:
			return fmt.Errorf("recovery probe: load replacement %s failed: %w", pending.NewAgentID, nerr)
		default:
			// Record exists. If the session is ALSO alive, the
			// previous handoff just needs its tail completed —
			// dispatch to resumeHandoff. If the session is dead
			// (replacement command crashed at startup before the
			// original process reached step 8a's rollback), clean
			// up the stale replacement and fall through to normal
			// spawn — the old agent is still alive, so a fresh
			// replacement is the correct recovery action.
			//
			// Codex iter-1 [P1]: use SessionAlive tristate so a
			// transient probe-failure doesn't masquerade as "session
			// dead" and trigger cleanup on a healthy replacement. Only
			// definitive (alive=false, err=nil) enters the cleanup +
			// fall-through-to-spawn branch.
			alive, perr := tmux.SessionAlive(newRec.TmuxSession)
			switch {
			case perr != nil:
				return fmt.Errorf(
					"recovery probe: probe replacement %s session %s failed: %w (queue file preserved for operator inspection)",
					newRec.ID, newRec.TmuxSession, perr)
			case !alive:
				// Codex iter-14 [P1] + iter-15 [P1] narrowing:
				// AtomicCoordSwap preserves the queue on ErrOrphanSurvived
				// / ErrOldKillProbeAmbiguous — NEW may have lost the
				// coordinator.lock race + died while OLD is still the
				// live coord. Refuse-and-preserve when the marker proves
				// the swap COMMITTED (marker == newRec.ID) AND OLD is
				// alive. Pre-commit crashes (marker == oldRec.ID, or
				// marker unset) are SAFE to auto-recover — OLD is the
				// only legitimate live coord, no duplicate-coord risk
				// from respawning. Operator can pass --force-replacement
				// to bypass post-commit refusal once OLD is confirmed
				// dead.
				//
				// Codex iter-18 [P1] (deferred — known limitation,
				// conservative stance): The eager marker write later
				// in runHandoff (see "8a-bis. Detect coord swap vs
				// worker handoff + eager marker") moves marker →
				// newRec.ID BEFORE the swap commits. A crash in that
				// pre-commit window leaves marker == newRec.ID with no
				// real commit, and this branch treats it as post-commit
				// — refusing auto-respawn instead of dropping the dead
				// NEW + retrying. Distinguishing pre-commit-eager-write
				// from post-commit-real-commit on disk requires a
				// separate "swap-committed" sentinel written only by
				// AtomicCoordSwap step 4; deferred to a follow-up PR.
				// Current behavior errs conservative; operator
				// workaround is `--force-replacement` once OLD is
				// confirmed dead.
				coordSwapPostCommit := false
				if oldRec.Project != "" &&
					state.ReadCoordSpawnMarker(oldRec.Project) == newRec.ID {
					coordSwapPostCommit = true
				}
				if coordSwapPostCommit && !opts.forceReplacement {
					// Codex iter-17 [P1] (deferred — known scope limitation):
					// tmux.SessionAlive only probes the CURRENT
					// FLEET_TMUX_SOCKET. (alive=false, err=nil) here
					// means "not on our socket," not "globally dead."
					// If OLD's coord was always on a different tmux
					// socket, we fall through and respawn — creating a
					// duplicate coord. Distinguishing requires
					// persisting OLD's originating socket on
					// agent.Record (schema change cross-cutting spawn +
					// state + every probe site); deferred to a follow-up
					// PR per the PR #149 cross-socket cap precedent.
					// `--force-replacement` still works for operator
					// cleanup once OLD is manually confirmed dead.
					// Single-socket fleet (the default) is correct here.
					oldAlive, probeErr := tmux.SessionAlive(oldRec.TmuxSession)
					switch {
					case probeErr != nil:
						return fmt.Errorf(
							"recovery probe: replacement %s session %s appears dead but probing old coord %s session %s failed: %w (queue preserved; retry once probe is reliable, or pass --force-replacement)",
							newRec.ID, newRec.TmuxSession, oldRec.ID, oldRec.TmuxSession, probeErr)
					case oldAlive:
						return fmt.Errorf(
							"recovery probe: replacement %s session %s exited but old coord %s session %s is still alive — refusing auto-respawn (would create duplicate coord); kill the orphan replacement or old coord manually then re-run, or pass --force-replacement to spawn anyway (queue preserved at %s)",
							newRec.ID, newRec.TmuxSession, oldRec.ID, oldRec.TmuxSession, pendingPath)
					}
				}
				// fix/orphan-tmux-sweeper-and-leak-plug: pair the record
				// removal with tmux.Kill (idempotent + SessionAlive-backed)
				// so a transient probe failure inside Kill can't delete the
				// record while leaving the tmux session alive.
				if dropErr := handoffop.DropReplacementRecord(newRec.TmuxSession, newRec.ID, stderr); dropErr != nil {
					return fmt.Errorf(
						"recovery probe: stale replacement %s session %s appeared dead but cleanup failed: %w (queue file preserved; investigate manually)",
						newRec.ID, newRec.TmuxSession, dropErr)
				}
				// Codex iter-12 [P1]: a prior swap may have committed
				// marker → newRec.ID before returning ErrOrphanSurvived
				// / ErrOldKillProbeAmbiguous / similar. With newRec
				// just dropped, the marker still points at the deleted
				// ID. The fall-through to normal spawn re-detects
				// isCoordSwap with `marker == oldRec.ID` (line 693-694);
				// without restoring the marker the check returns false,
				// the inline retire path runs (no AtomicCoordSwap), and
				// `[a]` can spawn a duplicate coord pointing at the
				// dead ID. Idempotent if marker already at oldRec.ID.
				handoffop.RollbackCoordMarkerIfPointingAt(oldRec.Project, oldRec.ID, newRec.ID, stderr)
				// Preserve auto-resume override into opts before
				// fall-through (codex iter-17 P1).
				if pending.DisableAutoResume != nil {
					opts.noAutoResume = *pending.DisableAutoResume
					opts.autoResume = !*pending.DisableAutoResume
					opts.autoResumeFlagWasSet = true
				}
				// Cap re-checked on retry (codex iter-8 P1 / iter-10
				// P2): the swap-aware gate must run with current
				// state, and the queue file must persist if the cap
				// refuses so a future retry can still find the
				// per-handoff overrides.
				capBypass := SessionCapBypassReason("")
				if opts.forceReplacement {
					capBypass = SessionCapBypassForceReplacement
				}
				if capErr := enforceSessionCap(stderr, capBypass, 1); capErr != nil {
					return capErr
				}
				_ = queue.Delete(pendingPath)
				_, _ = fmt.Fprintf(stderr,
					"note: stale replacement %s (session %s already exited) cleaned up; spawning fresh replacement\n",
					newRec.ID, newRec.TmuxSession)
				// Fall through to normal spawn flow.
			default:
				return resumeHandoff(opts, stdout, stderr, oldRec, newRec,
					pending.HandoffDoc, pendingPath, pending.DisableAutoResume,
					pending.SchemaVersion)
			}
		}
	}

	// 3. Load the agent record under the flock. If a concurrent
	//    handoff already archived it, agent.Load returns ErrNotFound
	//    and we bail without double-spawning. This is the only Load
	//    — the per-agent lock makes the pre-lock Load redundant.
	oldRec, err := agent.Load(opts.oldID)
	if err != nil {
		return fmt.Errorf("load agent %s under lock: %w (already handed off?)", opts.oldID, err)
	}

	// 3a. Dead-session short-circuit: if the outgoing tmux session is
	//     already gone (claude exited inside it via Ctrl-D / /exit /
	//     crash), there is no in-flight work to hand off and no agent
	//     to /exit. Spawning a replacement would emit a stub doc full
	//     of placeholders the new agent has no way to fill, then drop
	//     into a fresh shell — the operator wanted cleanup, not a
	//     fresh agent. Archive the record and return.
	//
	//     Runs AFTER the recovery probe so an in-flight handoff (queue
	//     file with NewAgentID set) still resumes via resumeHandoff,
	//     which retires the outgoing record using the already-spawned
	//     replacement instead of orphaning it.
	if !tmux.HasSession(oldRec.TmuxSession) {
		if err := oldRec.Archive(); err != nil {
			// Same fallback shape as step 12: try removing the live
			// record so a retry doesn't load a stale entry and loop.
			path, perr := state.AgentPath(oldRec.ID)
			if perr == nil {
				if rmErr := os.Remove(path); rmErr == nil {
					_, _ = fmt.Fprintf(stderr,
						"warning: archive %s: %v (live record removed instead, archive copy lost)\n",
						oldRec.ID, err)
				} else {
					return fmt.Errorf(
						"dead-session archive %s failed (%w) AND fallback remove failed (%w); clean up agents/%s.json manually",
						oldRec.ID, err, rmErr, oldRec.ID)
				}
			} else {
				return fmt.Errorf(
					"dead-session archive %s failed (%w) AND could not resolve live path (%w)",
					oldRec.ID, err, perr)
			}
		}
		_, _ = fmt.Fprintf(stdout,
			"agent %s session was dead — record archived, no replacement spawned\n", oldRec.ID)
		return nil
	}

	// 4. Resolve cwd + command for the replacement BEFORE writing
	//    any side effects. CLI flags win; otherwise inherit from
	//    oldRec so multi-repo operators invoking `fleet handoff`
	//    from a different shell still get the right project checkout
	//    and engine wrapper.
	//
	//    For legacy records (dispatched before this PR added Cwd /
	//    Command to agent.Record) BOTH the inherited value AND the
	//    flag may be empty. Refuse early — silently falling back to
	//    os.Getwd() / "claude" would land the replacement in the
	//    wrong tree and report success. Validating before doc/queue
	//    writes means a refusal leaves zero on-disk artifacts.
	cwd := opts.cwd
	if cwd == "" {
		// COORD handoffs resolve the repo binding via the shared
		// resolver, NOT the old coord's stored Cwd (DESIGN-coord-repo-
		// binding-from-project.md PR3). A wrong-repo coord handing off
		// to itself would otherwise perpetuate the cwd bug — the
		// replacement inherits the same wrong tree and keeps stamping
		// coord-config.json wrong. Resolve fresh and REFUSE on an
		// unresolvable project. persist=true (handoff is operator-
		// initiated). Worker handoffs keep inheriting oldRec.Cwd —
		// workers legitimately follow their dispatch tree.
		if spawn.IsCoordSpawn(oldRec.TaskID, oldRec.Project) {
			resolved, rerr := coordrepo.ResolveProjectRepo(oldRec.Project, true)
			if rerr != nil {
				return fmt.Errorf("coord handoff for project %q: %w", oldRec.Project, rerr)
			}
			cwd = resolved
		} else {
			cwd = oldRec.Cwd
		}
	}
	if cwd == "" {
		return fmt.Errorf("agent %s is a legacy record with no stored cwd; pass --cwd <path> explicitly", opts.oldID)
	}
	command := opts.command
	if len(command) == 0 {
		command = oldRec.Command
	}
	if len(command) == 0 {
		return fmt.Errorf("agent %s is a legacy record with no stored command; pass --command <argv> explicitly", opts.oldID)
	}

	// 4a. FLEET_MAX_SESSIONS backstop. Runs HERE (after recovery
	// branches in steps 2-3a, before the doc/queue/spawn side
	// effects in steps 5-8) so:
	//   - recovery completion paths above (no spawn) aren't blocked;
	//   - a refusal exits BEFORE writing the queue file, which would
	//     otherwise leave a `spawn-fresh-<id>.json` that blocks
	//     `fleet rm <id>` and traps the operator at-cap with no
	//     escape valve (codex iter-4 P1).
	// --force-replacement bypasses the cap for operator cleanups
	// that need the successor up first. swapsInFlight=1: the
	// about-to-be-killed old session is in the current count, so
	// the post-swap total is unchanged.
	bypass := SessionCapBypassReason("")
	if opts.forceReplacement {
		bypass = SessionCapBypassForceReplacement
	}
	if err := enforceSessionCap(stderr, bypass, 1); err != nil {
		return err
	}

	// 5. Write the handoff doc. The doc represents "agent oldID
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

	// 6. Pre-allocate the replacement's agent ID so we can journal
	//    it BEFORE spawn. This closes the crash window where the old
	//    code wrote the journal twice (once empty, once with newID
	//    after spawn) — a crash between those two writes left the
	//    journal with NewAgentID="" and a retry would double-spawn.
	//    Now: journal once with the pre-allocated ID, then spawn
	//    with that exact ID. Crash anywhere after step 7 leaves a
	//    journal entry the recovery probe can match against the
	//    actual record on disk (or detect as orphan if no record).
	newID := agent.NewID()
	newSession := tmux.SessionName(newID)

	// Auto-resume policy for the replacement (codex review iter-8 P2
	// + iter-10 / iter-11 / iter-12 P2). Tri-state override applies
	// ONLY to THIS handoff's wait+send, never to the new record's
	// baseline policy:
	//   - flag NOT passed: override = nil; this handoff inherits
	//     oldRec.DisableAutoResume.
	//   - --no-auto-resume: override = &true; force OFF here only.
	//   - --auto-resume:    override = &false; force ON here only.
	//
	// The new record's DisableAutoResume always inherits from oldRec
	// (no override) so a one-shot --no-auto-resume on a claude agent
	// doesn't permanently flip future handoffs to skip the prompt.
	var override *bool
	if opts.autoResumeFlagWasSet {
		v := opts.noAutoResume // exactly one of the two flags was set
		override = &v
	}
	thisHandoffDisableAutoResume := oldRec.DisableAutoResume
	if override != nil {
		thisHandoffDisableAutoResume = *override
	}

	// 7. Write the queue file with the pre-allocated successor ID.
	//    This is the durable commit point — anything after a crash
	//    here is recoverable via the resume path in step 2. The
	//    override pointer (NOT the resolved bool) is persisted so a
	//    later recovery can distinguish "operator overrode" from
	//    "inherited from old."
	queuePath, err := queue.WriteSpawnFresh(queue.SpawnFresh{
		OldAgentID:        oldRec.ID,
		HandoffDoc:        docPath,
		Project:           oldRec.Project,
		TaskID:            oldRec.TaskID,
		NewAgentID:        newID,
		NewSession:        newSession,
		DisableAutoResume: override,
		// CapApproved (codex iter-7 P1): step 4a already passed
		// the FLEET_MAX_SESSIONS gate. Persist so recovery via
		// handoffop.Resume / fleet drain doesn't re-block this
		// authorized handoff if the cap tightened between crash
		// and retry.
		CapApproved: true,
	})
	if err != nil {
		return fmt.Errorf("enqueue spawn-fresh: %w", err)
	}

	// 8. Drain: spawn the replacement using the pre-allocated ID.
	//    NewRec gets the resolved policy (override OR baseline) so
	//    a `--no-auto-resume` into a shell/vim/non-claude wrapper
	//    sticks for future handoffs of this lineage — otherwise the
	//    next hop would happily auto-type into the wrapper again
	//    (codex review iter-13 P2). Operators who want a true
	//    one-shot must re-pass the flag on the next handoff.
	//
	// Remote-control auto-attach for handoff replacements
	// (fix/remote-control-coord-injection P0): the replacement spawn
	// gets `--remote-control "fleet-handoff-<new-id>"` injected into
	// its claude argv, parallel to the coord-spawn path in
	// dispatch.go. The session prefix is `fleet-handoff` (not
	// `fleet-coord`) so the replacement attaches to the handoff
	// daemon that internal/handoff.FirstAction's bash block boots.
	//
	// The persisted rec.Command stays the clean `command` (so a
	// subsequent handoff doesn't inherit a stale --remote-control
	// "fleet-handoff-<old-id>"); ExecCommand carries the per-spawn
	// rewrite. injectRemoteControlFlag returns the slice unchanged
	// for non-default --command shapes so an operator-supplied
	// scripted pipeline / alt engine isn't silently mutated.
	// Project-first prefix (rc-session-name-include): the operator-
	// visible handoff session name on claude.ai mobile / web is
	// `fleet-handoff-<project>-<new-id>` so successor agents from
	// different projects can be distinguished. The project comes
	// BEFORE the id so the registered session name STARTS WITH the
	// per-project daemon prefix `fleet-handoff-<project>` that
	// internal/handoff.FirstAction renders into the bash bootstrap
	// (the Claude remote-control daemon only attaches sessions whose
	// name starts with its --remote-control-session-name-prefix).
	//
	// codex round-6 P1: post-v0.12 there is no longer a per-handoff
	// bash bootstrap (S2/S3 gates replaced it with operator-instruction
	// markdown). The only live listener now is the one `fleet rc up`
	// starts with prefix "fleet-coord". Injecting "fleet-handoff-..."
	// session names into the replacement coord would point at a prefix
	// the listener can't see → silent pairing failure post-handoff.
	// Use the coord session-name shape so the single live listener
	// matches both fresh-spawn and handoff replacements.
	// v0.12.1 P0 (DESIGN-rc-coord-auto-marker.md, operator G2
	// 2026-05-18): coord handoff replacements ARE coords too —
	// auto-opt-in to RC so the rc.Enabled gate inside
	// injectRemoteControlFlagProject below passes. Common case is
	// idempotent (marker already present from the predecessor coord's
	// auto-write); guards against the degenerate case where the
	// marker was removed between the predecessor's spawn and this
	// handoff (e.g. operator ran `fleet rc down <project>` then
	// realized they want pairing back). Failure is non-fatal —
	// operator can recover via `fleet rc up`.
	//
	// codex review iter-1 [P1] + iter-5 [P1]: gate BOTH the marker
	// write AND the --remote-control inject on isCoordHandoffForProject.
	// `fleet handoff` runs for BOTH coord and worker handoffs (same
	// code path; agent type comes from oldRec). Two separate hazards
	// motivate the dual gate:
	//
	//   (1) Writing the rc-enabled marker on a worker handoff would
	//       globally opt the project into RC, undoing an operator's
	//       explicit opt-out via `fleet rc down`. Fixed in iter-1.
	//
	//   (2) Even WITHOUT writing the marker, the helper below
	//       (injectRemoteControlFlagProject) consults
	//       rc.Enabled(project) — which is project-wide. If ANOTHER
	//       agent (e.g., the project's coord) already triggered the
	//       auto-write, a worker handoff would silently inherit RC
	//       attach, violating the v0.12 push-storm protection (workers
	//       and subagents stay strict opt-in even when the project is
	//       RC-enabled). Iter-5 [P1] catches this; gate the inject on
	//       isCoordHandoff so the handoff carve-out matches the
	//       dispatch-side carve-out (opts.coordSpawn).
	//
	// Detect coord via the coord-spawn marker (mirrors line ~817
	// isCoordSwap check); skip on workers.
	var rewrittenExecArgv []string
	if isCoordHandoffForProject(oldRec.Project, oldRec.ID) {
		if err := writeMarkerFn(oldRec.Project); err != nil {
			// Use injected stderr for parity with the rest of
			// runHandoff's warnings (codex review iter-3 [P3]).
			_, _ = fmt.Fprintf(stderr,
				"warning: rc.WriteMarker(%q) failed during handoff: %v (continuing with plain claude argv; run `fleet rc up %s` to recover)\n",
				oldRec.Project, err, oldRec.Project)
		}

		// v0.12 (DESIGN §"Attach-surface gates" I2): gate on the
		// per-project rc-enabled marker. Without it (operator hasn't
		// run `fleet rc up <project>`), the handoff replacement does
		// NOT carry --remote-control. FirstAction's operator-instruction
		// text tells the operator to run `fleet rc connect <project>`
		// in their terminal to re-attach pairing.
		rcSessionName := buildCoordRemoteControlSessionName(newID, oldRec.Project)
		rewritten := injectRemoteControlFlagProject(command, rcSessionName, oldRec.Project)
		// Only set ExecCommand when the rewrite actually changed
		// something — passing through an unchanged custom --command
		// avoids a no-op Command/ExecCommand divergence in spawn.Options.
		if !sameCommand(rewritten, command) {
			rewrittenExecArgv = rewritten
		}
	}

	newRec, err := spawn.Spawn(spawn.Options{
		OldRecord:         oldRec,
		NewDocPath:        docPath,
		Cwd:               cwd,
		Command:           command,
		ExecCommand:       rewrittenExecArgv,
		PreAllocatedID:    newID,
		DisableAutoResume: thisHandoffDisableAutoResume,
	})
	if err != nil {
		// Spawn failed — leave the queue file in place so the
		// recovery path can detect the orphan (no record + no
		// session at the journaled NewAgentID/NewSession) and clean
		// it up on retry. The old agent is still alive and untouched.
		return fmt.Errorf("spawn replacement: %w", err)
	}

	// 8a. Verify the replacement session is actually alive before
	//     we retire the old agent. spawn.Spawn intentionally tolerates
	//     short-lived commands that exit cleanly (no stderr signal),
	//     but for handoff the replacement IS supposed to be a
	//     long-lived agent. If `--command` was a wrapper that crashed
	//     at startup, killing the old session would leave the task
	//     with no live successor. Roll back instead.
	//
	//     Codex iter-1 [P1]: SessionAlive tristate at the outer gate so a
	//     transient probe-failure doesn't roll back a healthy spawn. Only
	//     definitive (alive=false, err=nil) enters the rollback branch.
	switch alive, perr := tmux.SessionAlive(newRec.TmuxSession); {
	case perr != nil:
		return fmt.Errorf(
			"probe replacement %s session %s failed: %w (record preserved; old agent untouched, queue file preserved for retry)",
			newRec.ID, newRec.TmuxSession, perr)
	case !alive:
		// fix/orphan-tmux-sweeper-and-leak-plug: pair record removal with
		// tmux.Kill so a probe-failure window inside Kill can't leak the
		// session.
		if dropErr := handoffop.DropReplacementRecord(newRec.TmuxSession, newRec.ID, stderr); dropErr != nil {
			return fmt.Errorf(
				"replacement %s tmux session %s appeared dead but cleanup failed: %w (old agent untouched, queue file preserved; investigate manually)",
				newRec.ID, newRec.TmuxSession, dropErr)
		}
		_ = queue.Delete(queuePath)
		return fmt.Errorf(
			"replacement %s spawned but tmux session %s already exited (command crashed at startup?); old agent untouched",
			newRec.ID, newRec.TmuxSession)
	}

	// 8a-bis. Detect coord swap vs worker handoff + eager marker
	//     write. The coord-spawn marker is the dashboard's gate for
	//     "this agent is this project's coord". When the OLD agent
	//     IS the project's coord (marker matches OLD.ID), the retire
	//     path runs through the transactional AtomicCoordSwap helper
	//     instead of the inline marker-write + Kill + Archive
	//     sequence below.
	//
	//     We ALSO eagerly write the marker → newRec.ID here, BEFORE
	//     the readiness wait at step 8c (codex review iter-9 [P1]).
	//     Without this eager write, the marker stays at oldRec.ID
	//     during NEW's up-to-30s boot — and if OLD exits during that
	//     window, the TUI's [a] path can't find NEW (record.ID !=
	//     marker) and spawns a duplicate coord. The eager write
	//     closes that duplicate-coord window. AtomicCoordSwap's
	//     step 1.b accepts the marker at either oldRec.ID (no eager
	//     write yet) or newRec.ID (eager write landed) and skips a
	//     redundant rename when markerAtNew is already true.
	//
	//     Worker handoffs (marker is unset OR marker != OLD.ID) keep
	//     the inline path unchanged — worker swap has different
	//     transactional semantics (task-row CompareAndSwap, deferred
	//     to a separate PR).
	isCoordSwap := false
	if oldRec.Project != "" && state.ReadCoordSpawnMarker(oldRec.Project) == oldRec.ID {
		isCoordSwap = true
		// Eager marker write — best-effort. AtomicCoordSwap will
		// re-commit (idempotent) inside its step 4 if this failed,
		// or skip step 4 if it succeeded.
		if werr := state.WriteCoordSpawnMarker(oldRec.Project, newRec.ID); werr != nil {
			_, _ = fmt.Fprintf(stderr,
				"warning: eager coord-spawn marker update for project %s failed: %v (AtomicCoordSwap will retry)\n",
				oldRec.Project, werr)
		}
	}

	// 8b. Auto-resume policy for the rest of this handoff. Uses the
	//     per-handoff resolved value (not newRec's baseline) so a
	//     one-shot --no-auto-resume / --auto-resume override only
	//     affects this handoff (codex review iter-12 P2). Gates the
	//     SEND only — the wait runs unconditionally since it's also
	//     our post-spawn liveness check (codex review iter-9 P1).
	autoResume := !thisHandoffDisableAutoResume

	// 8c. Wait for the new agent's pane to stabilize BEFORE we touch
	//     the old session. The wait is passive (new is rendering UI,
	//     not doing work — only SendPromptKeys later starts the new
	//     agent's work), so this respects the iter-2 P2 invariant
	//     that "new doesn't do work during the OLD↔NEW overlap"
	//     while also keeping recovery clean: if the new agent
	//     crashes during the up-to-30 s wait, old is still alive, so
	//     we roll back the spawn (kill new, delete record + queue)
	//     and return — the operator retries with old still standing.
	//
	//     Always runs, even when auto-resume is disabled — this is
	//     the only post-spawn liveness window, and a wrapper that
	//     survives step 8a but crashes shortly after would otherwise
	//     slip through to retire-old, stranding the task (codex
	//     review iter-9 P1).
	if err := spawn.WaitForReadyToPrompt(newRec.TmuxSession); err != nil {
		_, _ = fmt.Fprintf(stderr,
			"warning: readiness poll for %s did not converge: %v (proceeding anyway)\n",
			newRec.TmuxSession, err)
	}
	// Use SessionAlive (not HasSession) so a transport probe failure
	// — bad socket, lost server, transient error — doesn't masquerade
	// as "session dead" and roll back a live replacement (codex
	// review iter-15 P1).
	if alive, perr := tmux.SessionAlive(newRec.TmuxSession); perr != nil {
		_, _ = fmt.Fprintf(stderr,
			"warning: post-readiness probe for %s failed: %v (proceeding anyway)\n",
			newRec.TmuxSession, perr)
	} else if !alive {
		if path, perr := state.AgentPath(newRec.ID); perr == nil {
			_ = os.Remove(path)
		}
		_ = queue.Delete(queuePath)
		// Codex iter-10 [P1]: if we eagerly moved the coord marker
		// to newRec.ID (8a-bis) and NEW then died during the
		// readiness wait, the marker is pointing at a dead agent
		// while OLD is still the live coord. Restore the marker to
		// oldRec.ID so the dashboard's [a] discovery + a retry's
		// isCoordSwap detection both keep working. Best-effort —
		// the operator can also re-write via the TUI flow if this
		// fails.
		if isCoordSwap {
			if werr := state.WriteCoordSpawnMarker(oldRec.Project, oldRec.ID); werr != nil {
				_, _ = fmt.Fprintf(stderr,
					"warning: rollback coord-spawn marker for project %s failed: %v (operator may need to re-write manually)\n",
					oldRec.Project, werr)
			}
		}
		return fmt.Errorf(
			"replacement %s tmux session %s exited during readiness wait; old agent %s untouched, retry handoff",
			newRec.ID, newRec.TmuxSession, oldRec.ID)
	}

	// Coord swap fast path. AtomicCoordSwap collapses marker write +
	// /exit + grace + Kill + Archive into one transactional sequence
	// committed at the marker rename. Returns the cases the plan
	// enumerates (success, ErrOrphanSurvived = FAILURE MODE 5,
	// marker-write-failed = rollback) without leaving partial state.
	// Worker swap (the else branch below) keeps the existing inline
	// sequence; its transactional refactor is deferred (different
	// semantics: task-row CAS, not marker rename).
	if isCoordSwap {
		_, swapErr := handoffop.AtomicCoordSwap(handoffop.AtomicCoordSwapInputs{
			Project:              oldRec.Project,
			OldRec:               oldRec,
			AlreadySpawnedNewRec: newRec,
			GraceWindow:          time.Duration(opts.graceMillis) * time.Millisecond,
		}, stderr)
		if swapErr != nil {
			// Three error classes:
			//
			//   - ErrOrphanSurvived (FAILURE MODE 5): marker committed,
			//     OLD's tmux orphan but archived from records side,
			//     ready for prune-orphan-tmux. Safe to proceed with
			//     queue delete + prompt send.
			//
			//   - ErrOldKillProbeAmbiguous: marker committed BUT OLD
			//     may still be alive on a different tmux socket
			//     holding coordinator.lock. NEW may lose the race and
			//     exit. Preserve the queue journal so a retry after
			//     operator cleans up OLD can resume (codex review
			//     iter-6 [P1]). Return the error so the queue file
			//     stays.
			//
			//   - Any other error: true rollback (marker still at OLD,
			//     NEW cleaned up). Queue file stays for retry.
			// Codex iter-7 [P1]: ErrOrphanSurvived no longer auto-archives
			// OLD; the orphan is live and operator must triage. Preserve
			// the queue journal so retry after operator cleans up OLD
			// can resume. Treat both orphan classes identically — return
			// the error so the surrounding handoff path does NOT delete
			// the queue.
			return swapErr
		}
	} else {
		// 9. Send "/exit" to the old session. ErrNoSession means it
		//    already died (operator killed manually, crash, etc.) — fine,
		//    fall through to Kill which is also idempotent.
		if err := tmux.SendKeys(oldRec.TmuxSession, "/exit", "Enter"); err != nil &&
			!errors.Is(err, tmux.ErrNoSession) {
			// Treat anything else as a warning, not a fatal — we still
			// want to archive and clean up.
			_, _ = fmt.Fprintf(stderr, "warning: send-keys to %s: %v\n", oldRec.TmuxSession, err)
		}

		// 10. Grace window so Claude can flush its own state on /exit.
		if opts.graceMillis > 0 {
			time.Sleep(time.Duration(opts.graceMillis) * time.Millisecond)
		}

		// 11. Kill the old session. Idempotent — returns nil if already
		//    gone. If Kill fails AND HasSession still reports the session
		//    alive, we MUST NOT archive the old record (that would hide a
		//    live agent from `fleet status`) AND we MUST roll back the
		//    new agent (otherwise a retry of `fleet handoff <id>` finds
		//    the still-live old record and spawns ANOTHER replacement).
		//
		//    Rollback: kill the new tmux session, delete the new record,
		//    delete the queue file. Old agent + old session untouched.
		//    Handoff doc stays as a (stale) artifact — operator can
		//    inspect it. Operator retries cleanly after investigating
		//    why kill failed.
		if err := tmux.Kill(oldRec.TmuxSession); err != nil {
			if tmux.HasSession(oldRec.TmuxSession) {
				// Try to roll back the new agent. ONLY delete its record
				// after confirming the new tmux session is also gone —
				// otherwise we'd leave a live tmux session with no
				// fleet record (untracked successor that a later retry
				// would not see, leading to multiple replacements).
				// DropReplacementRecord pairs the kill + remove so a
				// probe-failure window can't leak the new session
				// (fix/orphan-tmux-sweeper-and-leak-plug). On cleanup
				// error we surface the both-alive variant so the
				// operator triages both stuck sessions manually.
				if dropErr := handoffop.DropReplacementRecord(newRec.TmuxSession, newRec.ID, stderr); dropErr != nil {
					return fmt.Errorf(
						"old session %s AND new session %s both alive after kill failure: %w (replacement %s record preserved; cleanup attempt also failed: %v; clean up both manually)",
						oldRec.TmuxSession, newRec.TmuxSession, err, newRec.ID, dropErr)
				}
				_ = queue.Delete(queuePath)
				return fmt.Errorf(
					"old session %s still alive after kill failure: %w (replacement %s rolled back; investigate before retrying)",
					oldRec.TmuxSession, err, newRec.ID)
			}
			// Session vanished concurrently with our Kill attempt
			// (race with operator's manual kill, OS shutdown, etc.).
			// Safe to proceed.
			_, _ = fmt.Fprintf(stderr, "note: kill %s reported error but session is gone: %v\n",
				oldRec.TmuxSession, err)
		}

		// 12. Archive the old record. After this, `fleet status` no
		//     longer shows the outgoing agent. We've confirmed the old
		//     session is dead in step 10, so this can't hide a live
		//     agent. If Archive fails (rare — agents/archive/ went away
		//     or is unwritable), fall back to deleting the live record
		//     directly so a retry of `fleet handoff <id>` doesn't load
		//     the stale record and double-spawn. If even the delete
		//     fails, hard-error: the live record is stuck and must be
		//     removed manually before retry.
		//
		// v2 schema: ArchiveWithHandoff stamps successor_id + cause=handoff
		// on the archive so the chain-following resolver in
		// cmd/fleet/attach.go can walk pred → succ. spawn.Spawn already
		// stamped predecessor_id on the live successor record.
		if err := oldRec.ArchiveWithHandoff(newRec.ID); err != nil {
			path, perr := state.AgentPath(oldRec.ID)
			if perr == nil {
				if rmErr := os.Remove(path); rmErr == nil {
					_, _ = fmt.Fprintf(stderr,
						"warning: archive %s: %v (live record removed instead, archive copy lost)\n",
						oldRec.ID, err)
				} else {
					return fmt.Errorf(
						"archive %s failed (%w) AND fallback remove failed (%w); replacement %s spawned but old record stuck — clean up agents/%s.json manually before retrying",
						oldRec.ID, err, rmErr, newRec.ID, oldRec.ID)
				}
			} else {
				return fmt.Errorf(
					"archive %s failed (%w) AND could not resolve live path (%w); replacement %s spawned",
					oldRec.ID, err, perr, newRec.ID)
			}
		}
	}

	// 13. Delete the queue file. Work is durable on disk; the journal
	//     entry is no longer needed. We GATE step 14's send on this
	//     succeeding — if queue.Delete fails the journal lingers, a
	//     retry will run cleanUpStaleQueue (or the equivalent
	//     archive-recovery branch above) which has its own send +
	//     delete pair; sending here too would double-deliver (codex
	//     review iter-11 P3).
	queueDeleted := true
	if err := queue.Delete(queuePath); err != nil {
		_, _ = fmt.Fprintf(stderr, "warning: delete queue file: %v\n", err)
		queueDeleted = false
	}

	// 14. Send the resume prompt — only when queue.Delete succeeded
	//     so the at-most-once guarantee holds. If the send itself
	//     fails after the queue was deleted, re-enqueue so a future
	//     `fleet handoff` / `fleet drain` can retry the delivery
	//     (codex review iter-13 P2). Without this, send failure
	//     would silently strand the replacement: queue is gone, no
	//     journal to recover from. Skipped entirely when
	//     DisableAutoResume is set.
	if queueDeleted && autoResume {
		if err := spawn.SendPromptKeys(newRec.TmuxSession,
			handoff.ResumePrompt(docPath)); err != nil {
			_, _ = fmt.Fprintf(stderr,
				"warning: send resume prompt to %s: %v (re-enqueuing for retry)\n",
				newRec.TmuxSession, err)
			if _, werr := queue.WriteSpawnFresh(queue.SpawnFresh{
				OldAgentID:        oldRec.ID,
				HandoffDoc:        docPath,
				Project:           oldRec.Project,
				TaskID:            oldRec.TaskID,
				NewAgentID:        newID,
				NewSession:        newSession,
				DisableAutoResume: override,
				// Already passed cap; preserve so retries don't
				// re-block on a tightened cap (codex iter-7 P1).
				CapApproved: true,
			}); werr != nil {
				_, _ = fmt.Fprintf(stderr,
					"warning: re-enqueue after send failure: %v (replacement may need manual prompt on attach)\n",
					werr)
			}
		}
	}

	_, _ = fmt.Fprintf(stdout, "agent %s handed off → %s\n", oldRec.ID, newRec.ID)
	_, _ = fmt.Fprintf(stdout, "  task:    %s\n", newRec.TaskID)
	_, _ = fmt.Fprintf(stdout, "  project: %s\n", newRec.Project)
	_, _ = fmt.Fprintf(stdout, "  tmux:    %s\n", newRec.TmuxSession)
	_, _ = fmt.Fprintf(stdout, "  handoff: %s\n", docPath)
	_, _ = fmt.Fprintf(stdout, "  number:  %d (was %d)\n", newRec.HandoffNumber, oldRec.HandoffNumber)
	_, _ = fmt.Fprintf(stdout, "\nattach with: fleet attach %s\n", newRec.ID)
	switch {
	case !autoResume:
		// Auto-resume disabled — replacement is alive but idle.
		// Tell the operator what to type once they attach (codex
		// review iter-7 P2).
		_, _ = fmt.Fprintf(stdout,
			"then say: read the handoff doc at %s and continue\n", docPath)
	case !queueDeleted:
		// queue.Delete failed → SendPromptKeys was skipped for the
		// at-most-once guarantee. The journal is still on disk;
		// running fleet handoff again triggers cleanUpStaleQueue
		// which delivers the prompt + cleans up. Telling the
		// operator to type the prompt manually here would race
		// with that recovery and produce a double-send (codex
		// review iter-17 P2).
		_, _ = fmt.Fprintf(stdout,
			"queue cleanup failed; rerun `fleet handoff %s` to deliver the resume prompt\n",
			oldRec.ID)
	}
	return nil
}

// resumeHandoff finishes a handoff that crashed AFTER spawn but
// BEFORE archive (the recovery branch in step 2 of runHandoff).
// Same kill+archive+delete sequence as the tail of runHandoff, no
// spawn — the new agent already exists.
//
// Caller holds the per-agent flock.
func resumeHandoff(opts *handoffOpts, stdout, stderr io.Writer,
	oldRec, newRec *agent.Record, docPath, queuePath string,
	pendingDisableAutoResume *bool, pendingSchemaVersion int) error {

	// Verify the previously-spawned replacement is still alive. If it
	// died after the original spawn (operator manually killed it,
	// crashed, etc.), retiring the old agent now would leave the task
	// with nothing running. Bail without touching the old agent.
	//
	// Codex iter-1 [P1]: SessionAlive tristate so a transient probe-fail
	// doesn't fabricate "session gone" and force a rollback when the
	// replacement is actually alive. Probe errors surface as explicit
	// errors with the record preserved; only definitive (alive=false,
	// err=nil) trips the gone-session bail-out.
	switch alive, perr := tmux.SessionAlive(newRec.TmuxSession); {
	case perr != nil:
		return fmt.Errorf(
			"resume handoff: probe replacement %s session %s failed: %w; old agent %s untouched (retry after the probe is reliable)",
			newRec.ID, newRec.TmuxSession, perr, oldRec.ID)
	case !alive:
		return fmt.Errorf(
			"resume handoff: replacement %s tmux session %s is gone; old agent %s untouched (clean up agents/%s.json + queue file or restart handoff)",
			newRec.ID, newRec.TmuxSession, oldRec.ID, newRec.ID)
	}

	// Resolve THIS handoff's auto-resume policy: queue override
	// (set by the original handoff CLI) wins over newRec baseline
	// (codex review iter-12 P2). Gate on schema v2+ (codex review
	// iter-16 P2): v1 queue files predate auto-resume; resuming
	// one with the new binary would unexpectedly inject a prompt
	// into a replacement the operator already started manually.
	disableAutoResume := newRec.DisableAutoResume
	if pendingDisableAutoResume != nil {
		disableAutoResume = *pendingDisableAutoResume
	}
	autoResume := !disableAutoResume && pendingSchemaVersion >= 2

	// Coord swap fast path on the crash-recovery flow too. The
	// previous run may have:
	//   - crashed AFTER spawn but BEFORE marker write: marker still
	//     at oldRec.ID, swap still pending → AtomicCoordSwap commits.
	//   - crashed AFTER marker commit during retire: marker at
	//     newRec.ID, retire bookkeeping still pending → helper's
	//     step 1.b accepts markerAtNew, step 4 skips the redundant
	//     rename, retire (kill+archive OLD) proceeds.
	// (Codex iter-5 [P1], iter-9 [P1].)
	//
	// Codex iter-13 [P1]: this block MUST run BEFORE the readiness
	// wait below. Without the eager marker write happening first,
	// the marker stays at oldRec.ID through the full up-to-30s boot
	// wait. If OLD exits during that window (or operator hits `[a]`),
	// the dashboard can't discover NEW as the coord (record.ID !=
	// marker) and spawns a duplicate coordinator — the exact race
	// this whole atomic-swap PR is meant to close. runHandoff already
	// orders these correctly (step 8a-bis precedes step 8c); the
	// crash-recovery path must match.
	//
	// Eager marker write when marker is still at oldRec.ID, same as
	// runHandoff — closes the duplicate-coord window during the
	// resumeHandoff readiness wait.
	isCoordSwap := false
	if oldRec.Project != "" {
		switch state.ReadCoordSpawnMarker(oldRec.Project) {
		case oldRec.ID:
			isCoordSwap = true
			if werr := state.WriteCoordSpawnMarker(oldRec.Project, newRec.ID); werr != nil {
				_, _ = fmt.Fprintf(stderr,
					"warning: eager coord-spawn marker update for project %s failed: %v (AtomicCoordSwap will retry)\n",
					oldRec.Project, werr)
			}
		case newRec.ID:
			isCoordSwap = true
		}
	}

	// Wait BEFORE killing old (codex iter-8 P1). Always runs — the
	// wait doubles as a post-spawn liveness check, and a wrapper
	// that crashes shortly after the previous run's spawn must be
	// caught here so we don't retire old into nothing (codex iter-9
	// P1).
	if err := spawn.WaitForReadyToPrompt(newRec.TmuxSession); err != nil {
		_, _ = fmt.Fprintf(stderr,
			"warning: readiness poll for %s did not converge: %v (proceeding anyway)\n",
			newRec.TmuxSession, err)
	}
	// SessionAlive (not HasSession) so probe failures don't roll
	// back a live replacement (codex iter-15 P1).
	if alive, perr := tmux.SessionAlive(newRec.TmuxSession); perr != nil {
		_, _ = fmt.Fprintf(stderr,
			"warning: post-readiness probe for %s failed: %v (proceeding anyway)\n",
			newRec.TmuxSession, perr)
	} else if !alive {
		// Codex iter-11 [P1]: the eager marker write above may have
		// committed marker → newRec.ID before NEW died during the
		// readiness wait. Restore it to oldRec.ID so dashboard
		// discovery + a retry's isCoordSwap detection keep working.
		// Idempotent if marker is already at oldRec.ID (eager write
		// failed) or unrelated.
		if isCoordSwap {
			handoffop.RollbackCoordMarkerIfPointingAt(oldRec.Project, oldRec.ID, newRec.ID, stderr)
		}
		return fmt.Errorf(
			"resume handoff: replacement %s tmux session %s exited during readiness wait; old agent %s untouched, retry handoff",
			newRec.ID, newRec.TmuxSession, oldRec.ID)
	}
	if isCoordSwap {
		_, swapErr := handoffop.AtomicCoordSwap(handoffop.AtomicCoordSwapInputs{
			Project:              oldRec.Project,
			OldRec:               oldRec,
			AlreadySpawnedNewRec: newRec,
			GraceWindow:          time.Duration(opts.graceMillis) * time.Millisecond,
		}, stderr)
		if swapErr != nil {
			// See iter-6 [P1] commentary in runHandoff: ambiguous-probe
			// preserves the queue journal so retry can resume after
			// operator confirms OLD dead.
			// Codex iter-7 [P1]: ErrOrphanSurvived no longer auto-archives
			// OLD; the orphan is live and operator must triage. Preserve
			// the queue journal so retry after operator cleans up OLD
			// can resume. Treat both orphan classes identically — return
			// the error so the surrounding handoff path does NOT delete
			// the queue.
			return swapErr
		}
	} else {
		if err := tmux.SendKeys(oldRec.TmuxSession, "/exit", "Enter"); err != nil &&
			!errors.Is(err, tmux.ErrNoSession) {
			_, _ = fmt.Fprintf(stderr, "warning: send-keys to %s: %v\n", oldRec.TmuxSession, err)
		}
		if opts.graceMillis > 0 {
			time.Sleep(time.Duration(opts.graceMillis) * time.Millisecond)
		}
		if err := tmux.Kill(oldRec.TmuxSession); err != nil {
			if tmux.HasSession(oldRec.TmuxSession) {
				return fmt.Errorf(
					"resume handoff: old session %s still alive after kill: %w (replacement %s exists; investigate)",
					oldRec.TmuxSession, err, newRec.ID)
			}
			_, _ = fmt.Fprintf(stderr, "note: kill %s reported error but session is gone: %v\n",
				oldRec.TmuxSession, err)
		}

		// v2 schema: stamp successor_id + cause=handoff on the archive
		// so the chain resolver lands operators on the live tail.
		if err := oldRec.ArchiveWithHandoff(newRec.ID); err != nil {
			path, perr := state.AgentPath(oldRec.ID)
			if perr == nil {
				if rmErr := os.Remove(path); rmErr == nil {
					_, _ = fmt.Fprintf(stderr, "warning: archive %s: %v (live record removed instead)\n",
						oldRec.ID, err)
				} else {
					return fmt.Errorf("resume handoff: archive %s failed (%w) AND remove failed (%w)",
						oldRec.ID, err, rmErr)
				}
			}
		}
	}
	queueDeleted := true
	if err := queue.Delete(queuePath); err != nil {
		_, _ = fmt.Fprintf(stderr, "warning: delete queue file: %v\n", err)
		queueDeleted = false
	}

	// Send the resume prompt only if queue.Delete succeeded — see
	// runHandoff step 14 / codex iter-11 P3. Re-enqueue on send
	// failure so the next retry can deliver (codex iter-13 P2).
	if queueDeleted && autoResume {
		if err := spawn.SendPromptKeys(newRec.TmuxSession,
			handoff.ResumePrompt(docPath)); err != nil {
			_, _ = fmt.Fprintf(stderr,
				"warning: send resume prompt to %s: %v (re-enqueuing for retry)\n",
				newRec.TmuxSession, err)
			if _, werr := queue.WriteSpawnFresh(queue.SpawnFresh{
				OldAgentID:        oldRec.ID,
				HandoffDoc:        docPath,
				Project:           oldRec.Project,
				TaskID:            oldRec.TaskID,
				NewAgentID:        newRec.ID,
				NewSession:        newRec.TmuxSession,
				DisableAutoResume: pendingDisableAutoResume,
				// Resume path; the original handoff already passed
				// the cap. Preserve so further retries don't
				// re-block on a tightened cap (codex iter-7 P1).
				CapApproved: true,
			}); werr != nil {
				_, _ = fmt.Fprintf(stderr,
					"warning: re-enqueue after send failure: %v (replacement may need manual prompt on attach)\n",
					werr)
			}
		}
	}

	_, _ = fmt.Fprintf(stdout, "resumed crashed handoff: %s → %s (replacement was already spawned)\n",
		oldRec.ID, newRec.ID)
	_, _ = fmt.Fprintf(stdout, "  task:    %s\n", newRec.TaskID)
	_, _ = fmt.Fprintf(stdout, "  project: %s\n", newRec.Project)
	_, _ = fmt.Fprintf(stdout, "  tmux:    %s\n", newRec.TmuxSession)
	_, _ = fmt.Fprintf(stdout, "  handoff: %s\n", docPath)
	_, _ = fmt.Fprintf(stdout, "\nattach with: fleet attach %s\n", newRec.ID)
	if !autoResume {
		_, _ = fmt.Fprintf(stdout,
			"then say: read the handoff doc at %s and continue\n", docPath)
	}
	return nil
}
